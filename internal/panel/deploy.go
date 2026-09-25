package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/github"
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

// A new version counts as healthy when it answers HTTP within startupLimit
// or, for an app without a domain, stays up for startupGrace. Tests shorten
// these.
var (
	startupGrace = 10 * time.Second
	startupLimit = 60 * time.Second
	startupPoll  = 500 * time.Millisecond
)

// healthClient talks straight to the container: no proxy from the
// environment, no kept connections, no following redirects.
var healthClient = &http.Client{
	Transport:     &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// The tests of a new build must finish within testTimeout. Tests shorten
// testSettle, the time given to the last of their output to arrive.
const testTimeout = 10 * time.Minute

var testSettle = 600 * time.Millisecond

// keepImages is how many of an app's most recent live versions keep their
// image, so they can be rolled back to.
const keepImages = 5

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

// deploy queues a new deployment of the app and returns its id. d may name
// the commit to deploy and what caused the deployment; by default it is the
// newest commit of the app's branch, or its image.
func (s *Server) deploy(ctx context.Context, app store.App, d store.Deployment) (int64, error) {
	d.AppID = app.ID
	if d.Version == "" {
		d.Version = app.Image
		if app.Source == store.SourceGitHub {
			d.Version = app.Branch
		}
	}
	id, err := s.Store.CreateDeployment(ctx, d, s.now())
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
	var status *commitStatus
	fail := func(err error) {
		fmt.Fprintf(out, "\nDeployment failed: %v\n", err)
		d.Error = err.Error()
		set(store.DeployFailed)
		status.set(ctx, github.StatusFailure, "Deployment failed: "+err.Error())
		s.Log.Warn("deployment failed", "app", appID, "id", id, "err", err)
	}

	// Only the newest of several queued deployments is worth the work.
	// Waiting deployments do not start in order, so an older one must also
	// not undo a newer one that already ran.
	if newer, err := s.Store.HasNewer(ctx, d); err != nil {
		fail(err)
		return
	} else if newer {
		fmt.Fprintln(out, "A newer deployment came before this one started, so this one was skipped.")
		set(store.DeploySkipped)
		return
	}

	// Settings may have changed while the deployment waited its turn.
	app, err := s.Store.App(ctx, appID)
	if err != nil {
		fail(err)
		return
	}

	switch {
	case d.Image != "":
		// A restart or rollback runs an image that already exists.
		fmt.Fprintf(out, "Using %s, built before.\n", d.Image)
	case app.Source == store.SourceGitHub:
		set(store.DeployBuilding)
		var src Source
		src, status = s.sourceFor(ctx, app.Repo, out)
		commit := ""
		if d.Cause == store.CausePush {
			commit = d.Version
		}
		res, commit, err := s.build(ctx, app, src, commit, status, out)
		d.Version = commit
		if err != nil {
			fail(err)
			return
		}
		d.Image = res.Image
		if !app.TestSet && res.TestCommand != "" {
			if ok, err := s.Store.SuggestTest(ctx, app.ID, res.TestCommand); err != nil {
				s.Log.Error("save test command", "app", app.ID, "err", err)
			} else if ok {
				app.TestCommand, app.TestSet = res.TestCommand, true
				fmt.Fprintf(out, "Found tests: %s. Change or clear this in the app's settings.\n", res.TestCommand)
			}
		}
		if app.TestCommand != "" {
			set(store.DeployTesting)
			status.set(ctx, github.StatusPending, "Testing")
			if err := s.runTests(ctx, app, d.Image, id, out); err != nil {
				fail(err)
				return
			}
		}
	default:
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
	status.set(ctx, github.StatusSuccess, "Live")
	s.Log.Info("deployment live", "app", app.ID, "id", id, "image", d.Image)
	s.removeOldContainers(ctx, app.ID, container, out)
	s.pruneImages(ctx, app.ID)
}

// pruneImages deletes the images Zelie built for an app, except the newest
// keepImages that went live. Failed builds' images go too.
func (s *Server) pruneImages(ctx context.Context, app string) {
	list, err := s.Store.Unpruned(ctx, app)
	if err != nil {
		s.Log.Error("list images", "app", app, "err", err)
		return
	}
	keep := map[string]bool{}
	for _, d := range list {
		if len(keep) < keepImages && (d.State == store.DeployLive || d.State == store.DeployReplaced) {
			keep[d.Image] = true
		}
	}
	done := map[string]bool{}
	for _, d := range list {
		if keep[d.Image] || done[d.Image] || !strings.HasPrefix(d.Image, engine.LocalImages) {
			continue
		}
		done[d.Image] = true
		if err := s.Core.RemoveImage(ctx, d.Image); err != nil {
			s.Log.Error("remove image", "image", d.Image, "err", err)
			continue
		}
		if err := s.Store.SetPruned(ctx, app, d.Image); err != nil {
			s.Log.Error("record removed image", "image", d.Image, "err", err)
		}
	}
}

// build fetches the app's code and has the core build it. Without a
// commit, it takes the newest one on the app's branch.
func (s *Server) build(ctx context.Context, app store.App, src Source, commit string, status *commitStatus, out io.Writer) (res build.Result, _ string, err error) {
	if commit == "" {
		fmt.Fprintf(out, "Fetching %s, branch %s.\n", app.Repo, app.Branch)
		if commit, err = src.Resolve(ctx, app.Repo, app.Branch); err != nil {
			return res, "", err
		}
	}
	if status != nil {
		status.commit, status.app = commit, app.ID
		status.set(ctx, github.StatusPending, "Building")
	}
	fmt.Fprintf(out, "Building commit %s of %s.\n", commit[:12], app.Repo)
	archive, err := src.Archive(ctx, app.Repo, commit)
	if err != nil {
		return res, commit, err
	}
	defer archive.Close()
	res, err = s.Core.Build(ctx, app.ID, commit[:12], archive, out)
	return res, commit, err
}

// runTests runs the app's test command in the new image, with the app's
// variables, and fails if it exits with anything but 0.
func (s *Server) runTests(ctx context.Context, app store.App, image string, deployment int64, out io.Writer) error {
	fmt.Fprintf(out, "Running the tests: %s\n", app.TestCommand)
	env, sealed, err := s.appEnv(ctx, app)
	if err != nil {
		return err
	}
	// Test runners that would otherwise wait for changes run once.
	env = append(env, "CI=true")
	id := fmt.Sprintf("%s-%d-test", app.ID, deployment)
	err = s.Core.RunApp(ctx, engine.Spec{
		ID: id, App: app.ID, Image: image, Args: []string{"sh", "-c", app.TestCommand}, Env: env, Network: app.ID,
		MemoryBytes: app.MemoryMB << 20, CPUs: app.CPUs, Pids: defaultPids,
	}, sealed)
	if err != nil {
		return fmt.Errorf("start the tests: %w", err)
	}
	defer s.Core.Remove(context.WithoutCancel(ctx), id)

	logCtx, stopLogs := context.WithCancel(ctx)
	logsDone := make(chan struct{})
	go func() {
		s.Core.Logs(logCtx, id, true, 0, out)
		close(logsDone)
	}()
	waitCtx, cancel := context.WithTimeout(ctx, testTimeout)
	defer cancel()
	code, err := s.Core.Wait(waitCtx, id)
	if err == nil {
		time.Sleep(testSettle)
	}
	stopLogs()
	<-logsDone
	switch {
	case errors.Is(waitCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("the tests did not finish within %s", testTimeout)
	case err != nil:
		return err
	case code != 0:
		return fmt.Errorf("the tests failed (exit code %d)", code)
	}
	fmt.Fprintln(out, "The tests passed.")
	return nil
}

// appEnv returns the app's variables for a container: the plain ones and
// the sealed ones, which only the core can open.
func (s *Server) appEnv(ctx context.Context, app store.App) (env, sealed []string, err error) {
	vars, err := s.Store.Env(ctx, app.ID)
	if err != nil {
		return nil, nil, err
	}
	// Most frameworks read the port to listen on from PORT.
	env = []string{"PORT=" + strconv.Itoa(app.Port)}
	for _, v := range vars {
		if v.Secret {
			sealed = append(sealed, v.Value)
		} else {
			env = append(env, v.Name+"="+v.Value)
		}
	}
	return env, sealed, nil
}

// start runs the new container and waits until it is healthy: answering
// HTTP if the app has a domain, since that is what the proxy will send it,
// or staying up for a while otherwise, like a bot with no web side.
func (s *Server) start(ctx context.Context, app store.App, image, container string, out io.Writer) error {
	env, sealed, err := s.appEnv(ctx, app)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Starting %s.\n", image)
	err = s.Core.RunApp(ctx, engine.Spec{
		ID: container, App: app.ID, Image: image, Env: env, Network: app.ID,
		MemoryBytes: app.MemoryMB << 20, CPUs: app.CPUs, Pids: defaultPids,
	}, sealed)
	if err != nil {
		return err
	}

	if app.Domain != "" {
		fmt.Fprintf(out, "Waiting for it to answer at %s on port %d.\n", app.HealthPath, app.Port)
	}
	deadline := time.Now().Add(startupLimit)
	var upSince time.Time
	lastAnswer := ""
	for {
		st, err := s.containerStatus(ctx, container)
		if err != nil {
			return err
		}
		switch {
		case st.State == "running" && app.Domain != "":
			addr := netip.AddrPortFrom(st.IP, uint16(app.Port)).String()
			code, err := s.healthCheck(ctx, "http://"+addr+app.HealthPath, app.Domain)
			if err == nil && code < 500 {
				fmt.Fprintf(out, "It answered with %d.\n", code)
				return nil
			}
			if err != nil {
				lastAnswer = "no answer yet"
			} else {
				lastAnswer = fmt.Sprintf("it answered %d", code)
			}
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
			if lastAnswer != "" {
				fmt.Fprintln(out, "\nThe app's last output:")
				s.Core.Logs(ctx, container, false, 4<<10, out)
				return fmt.Errorf("the app did not answer at %s on port %d within %s (%s)", app.HealthPath, app.Port, startupLimit, lastAnswer)
			}
			return errors.New("the app did not start in time")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(startupPoll):
		}
	}
}

// healthCheck asks the new version for a page, the way the proxy will, and
// returns the status code. Redirects are answers too; they are not followed.
func (s *Server) healthCheck(ctx context.Context, url, host string) (int, error) {
	if s.HealthCheck != nil {
		return s.HealthCheck(ctx, url, host)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Host = host
	req.Header.Set("User-Agent", "Zelie health check")
	resp, err := healthClient.Do(req)
	if err != nil {
		return 0, err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return resp.StatusCode, nil
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
