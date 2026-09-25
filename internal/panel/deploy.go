package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

// Proxy is how the panel tells the proxy where each domain goes.
type Proxy interface {
	Config(ctx context.Context) (proxy.Config, error)
	Apply(ctx context.Context, cfg proxy.Config) error
}

// A deployment's log is cut off here, so a runaway build cannot fill the
// disk.
const maxDeployLog = 8 << 20

// A new container must stay up for startupGrace to count as started. Tests
// shorten these.
var (
	startupGrace = 3 * time.Second
	startupLimit = 60 * time.Second
	startupPoll  = 500 * time.Millisecond
)

// deploys runs deployments in the background, one at a time per app.
type deploys struct {
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	cancels map[string]context.CancelFunc // of the deployment running now, by app
	wg      sync.WaitGroup
}

// running records the cancel function of an app's current deployment.
func (d *deploys) running(app string, cancel context.CancelFunc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.cancels == nil {
		d.cancels = map[string]context.CancelFunc{}
	}
	if cancel == nil {
		delete(d.cancels, app)
	} else {
		d.cancels[app] = cancel
	}
}

// cancel stops the app's current deployment, if there is one.
func (d *deploys) cancel(app string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c := d.cancels[app]; c != nil {
		c()
	}
}

func (d *deploys) lock(app string) func() {
	d.mu.Lock()
	if d.locks == nil {
		d.locks = map[string]*sync.Mutex{}
	}
	l, ok := d.locks[app]
	if !ok {
		l = &sync.Mutex{}
		d.locks[app] = l
	}
	d.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// deploy queues a new deployment of the app and returns its id.
func (s *Server) deploy(ctx context.Context, app store.App) (int64, error) {
	version := app.Image
	if app.Source == store.SourceGitHub {
		version = app.Branch
	}
	id, err := s.Store.CreateDeployment(ctx, app.ID, version, s.now())
	if err != nil {
		return 0, err
	}
	s.deploys.wg.Add(1)
	go func() {
		defer s.deploys.wg.Done()
		unlock := s.deploys.lock(app.ID)
		defer unlock()
		ctx, cancel := context.WithCancel(s.baseContext())
		defer cancel()
		s.deploys.running(app.ID, cancel)
		defer s.deploys.running(app.ID, nil)
		s.runDeployment(ctx, app.ID, id)
	}()
	return id, nil
}

func (s *Server) deployLogPath(id int64) string {
	return filepath.Join(s.DataDir, "deploys", strconv.FormatInt(id, 10)+".log")
}

// runDeployment builds or pulls the new version, starts it next to the old
// one, moves the domain over and only then removes the old container. If
// anything fails the old version keeps running.
func (s *Server) runDeployment(ctx context.Context, appID string, id int64) {
	d, err := s.Store.Deployment(ctx, appID, id)
	if err != nil {
		s.Log.Error("load deployment", "app", appID, "id", id, "err", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.deployLogPath(id)), 0o700); err != nil {
		s.Log.Error("deployment log", "err", err)
		return
	}
	f, err := os.OpenFile(s.deployLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		s.Log.Error("deployment log", "err", err)
		return
	}
	defer f.Close()
	out := &cappedWriter{w: f, left: maxDeployLog}

	set := func(state string) {
		d.State = state
		if err := s.Store.SetDeployment(ctx, d, s.now()); err != nil {
			s.Log.Error("save deployment", "app", appID, "id", id, "err", err)
		}
	}
	fail := func(err error) {
		fmt.Fprintf(out, "\nDeployment failed: %v\n", err)
		d.Error = err.Error()
		set(store.DeployFailed)
		s.Log.Warn("deployment failed", "app", appID, "id", id, "err", err)
	}

	// Settings may have changed while the deployment waited its turn.
	app, err := s.Store.App(ctx, appID)
	if err != nil {
		fail(err)
		return
	}

	if app.Source == store.SourceGitHub {
		set(store.DeployBuilding)
		image, commit, err := s.build(ctx, app, out)
		d.Version = commit
		if err != nil {
			fail(err)
			return
		}
		d.Image = image
	} else {
		d.Image = app.Image
	}

	set(store.DeployStarting)
	container := fmt.Sprintf("%s-%d", app.ID, id)
	if err := s.start(ctx, app, d.Image, container, out); err != nil {
		s.Core.Remove(context.WithoutCancel(ctx), container)
		fail(err)
		return
	}
	if err := s.syncRoutes(ctx, map[string]string{app.ID: container}); err != nil {
		s.Core.Remove(context.WithoutCancel(ctx), container)
		fail(fmt.Errorf("point %s at the new version: %w", app.Domain, err))
		return
	}
	if err := s.Store.GoLive(ctx, d, s.now()); err != nil {
		fail(err)
		return
	}
	fmt.Fprintln(out, "The new version is live.")
	s.Log.Info("deployment live", "app", app.ID, "id", id, "image", d.Image)
	s.removeOldContainers(ctx, app.ID, container, out)
}

// build fetches the app's code and has the core build it.
func (s *Server) build(ctx context.Context, app store.App, out io.Writer) (image, commit string, err error) {
	fmt.Fprintf(out, "Fetching %s, branch %s.\n", app.Repo, app.Branch)
	commit, err = s.Source.Resolve(ctx, app.Repo, app.Branch)
	if err != nil {
		return "", "", err
	}
	fmt.Fprintf(out, "Building commit %s.\n", commit[:12])
	archive, err := s.Source.Archive(ctx, app.Repo, commit)
	if err != nil {
		return "", commit, err
	}
	defer archive.Close()
	image, err = s.Core.Build(ctx, app.ID, commit[:12], archive, out)
	return image, commit, err
}

// start runs the new container and waits until it has stayed up for a
// moment.
func (s *Server) start(ctx context.Context, app store.App, image, container string, out io.Writer) error {
	vars, err := s.Store.Env(ctx, app.ID)
	if err != nil {
		return err
	}
	// Most frameworks read the port to listen on from PORT.
	env := []string{"PORT=" + strconv.Itoa(app.Port)}
	var sealed []string
	for _, v := range vars {
		if v.Secret {
			sealed = append(sealed, v.Value)
		} else {
			env = append(env, v.Name+"="+v.Value)
		}
	}
	fmt.Fprintf(out, "Starting %s.\n", image)
	err = s.Core.RunApp(ctx, engine.Spec{
		ID: container, App: app.ID, Image: image, Env: env, Network: app.ID,
		MemoryBytes: app.MemoryMB << 20, CPUs: app.CPUs, Pids: defaultPids,
	}, sealed)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(startupLimit)
	var upSince time.Time
	for {
		st, err := s.containerStatus(ctx, container)
		if err != nil {
			return err
		}
		switch {
		case st.State == "running" && upSince.IsZero():
			upSince = time.Now()
		case st.State == "running" && time.Since(upSince) >= startupGrace:
			return nil
		case st.State == "stopped":
			fmt.Fprintln(out, "\nThe app stopped right after starting. Its last output:")
			s.Core.Logs(ctx, container, false, 4<<10, out)
			return errors.New("the app stopped right after starting")
		}
		if time.Now().After(deadline) {
			return errors.New("the app did not start in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startupPoll):
		}
	}
}

func (s *Server) containerStatus(ctx context.Context, id string) (engine.Status, error) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return engine.Status{}, err
	}
	for _, c := range list {
		if c.ID == id {
			return c, nil
		}
	}
	return engine.Status{}, fmt.Errorf("container %s is gone", id)
}

// removeOldContainers removes every container of the app except keep.
func (s *Server) removeOldContainers(ctx context.Context, app, keep string, out io.Writer) {
	list, err := s.Core.List(ctx)
	if err != nil {
		s.Log.Error("list containers", "err", err)
		return
	}
	for _, c := range list {
		if c.App == app && c.ID != keep {
			if err := s.Core.Remove(ctx, c.ID); err != nil {
				s.Log.Error("remove old container", "id", c.ID, "err", err)
				continue
			}
			fmt.Fprintf(out, "Removed the old version (%s).\n", c.ID)
		}
	}
}

// syncRoutes sends each app's domain to its live container. The panel owns
// the proxy's routes: routes added by hand with zelie debug are replaced.
// next names containers that are about to go live, by app.
func (s *Server) syncRoutes(ctx context.Context, next map[string]string) error {
	if s.Proxy == nil {
		return nil
	}
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		return err
	}
	list, err := s.Core.List(ctx)
	if err != nil {
		return err
	}
	ips := make(map[string]netip.Addr, len(list))
	for _, c := range list {
		ips[c.ID] = c.IP
	}
	var routes []proxy.Route
	for _, a := range apps {
		if a.Domain == "" {
			continue
		}
		container, ok := next[a.ID]
		if !ok {
			live, err := s.Store.LiveDeployment(ctx, a.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			container = fmt.Sprintf("%s-%d", a.ID, live.ID)
		}
		ip := ips[container]
		if !ip.IsValid() {
			continue
		}
		routes = append(routes, proxy.Route{Host: a.Domain, Upstream: netip.AddrPortFrom(ip, uint16(a.Port)).String()})
	}
	cfg, err := s.Proxy.Config(ctx)
	if err != nil {
		return err
	}
	cfg.Routes = routes
	return s.Proxy.Apply(ctx, cfg)
}

// cappedWriter stops writing after a limit and says so once.
type cappedWriter struct {
	w    io.Writer
	left int64
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.left <= 0 {
		return len(p), nil
	}
	n := len(p)
	if int64(n) > c.left {
		p = p[:c.left]
	}
	c.left -= int64(len(p))
	if _, err := c.w.Write(p); err != nil {
		return 0, err
	}
	if c.left <= 0 {
		io.WriteString(c.w, "\n[the rest of the output was cut off]\n")
	}
	return n, nil
}
