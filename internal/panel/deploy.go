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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/github"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

// Proxy is how the panel tells the proxy where each domain goes.
type Proxy interface {
	Config(ctx context.Context) (proxy.Config, error)
	Apply(ctx context.Context, cfg proxy.Config) error
	Stats(ctx context.Context) (proxy.Stats, error)
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

var (
	errTestsTimeout   = msg.Define(0, "deploy.tests_timeout", "The tests did not finish within {minutes} minutes.")
	errTestsFailed    = msg.Define(0, "deploy.tests_failed", "The tests failed with exit code {code}.")
	errStoppedAtStart = msg.Define(0, "deploy.stopped_at_start", "The app stopped right after starting. Its log shows why.")
	errDBNoAnswer     = msg.Define(0, "deploy.database_no_answer", "The database did not accept connections on port {port} within {seconds} seconds.")
	errNoAnswer       = msg.Define(0, "deploy.no_answer", "The app did not answer at {path} on port {port} within {seconds} seconds.")
	errBadAnswer      = msg.Define(0, "deploy.bad_answer", "The app answered {status} at {path} on port {port}. It has to answer with a status below 500 within {seconds} seconds.")
	errNotStarted     = msg.Define(0, "deploy.not_started", "The app did not start in time.")
	errStopOld        = msg.Define(0, "deploy.stop_old", "The old version could not be stopped: {detail}")
	errRoute          = msg.Define(0, "deploy.route", "{domain} could not be pointed at the new version: {detail}")
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

const railpackBuildCmd = "RAILPACK_BUILD_CMD"

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
	s.background(app.ID, func(ctx context.Context) { s.runDeployment(ctx, app.ID, id) })
	return id, nil
}

// background runs work on an app once its earlier work is done, until it
// finishes or the app is deleted or stopped.
func (s *Server) background(app string, run func(ctx context.Context)) {
	s.deploys.wg.Add(1)
	go func() {
		defer s.deploys.wg.Done()
		unlock := s.deploys.lock(app)
		defer unlock()
		ctx, cancel := context.WithCancel(s.baseContext())
		defer cancel()
		s.deploys.running(app, cancel)
		defer s.deploys.running(app, nil)
		run(ctx)
	}()
}

func (s *Server) deployLogPath(id int64) string {
	return filepath.Join(s.DataDir, "deploys", strconv.FormatInt(id, 10)+".log")
}

// openDeployLog opens the file a deployment's output is kept in. What it
// returns is cut off at maxDeployLog.
func (s *Server) openDeployLog(id int64) (io.Writer, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(s.deployLogPath(id)), 0o700); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(s.deployLogPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, err
	}
	return &cappedWriter{w: f, left: maxDeployLog}, f.Close, nil
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
	out, closeLog, err := s.openDeployLog(id)
	if err != nil {
		s.Log.Error("deployment log", "err", err)
		return
	}
	defer closeLog()

	set := func(state string) {
		d.State = state
		if err := s.Store.SetDeployment(ctx, d, s.now()); err != nil {
			s.Log.Error("save deployment", "app", appID, "id", id, "err", err)
		}
	}
	var status *commitStatus
	fail := func(err error) {
		fmt.Fprintf(out, "\nDeployment failed: %v\n", err)
		d.Error = new(msg.Wrap(err))
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

	if d.Cause == store.CauseRecover {
		s.lastOutput(ctx, app.ID, out)
	}
	if d.Cause == store.CauseUpgrade {
		set(store.DeployStarting)
		if err := s.runUpgrade(ctx, app, &d, out); err != nil {
			fail(err)
		}
		return
	}
	if d.Cause == store.CauseUpdate && app.IsDatabase() {
		fmt.Fprintln(out, "Backing up the database before the update.")
		if !s.backupBusy.take(app.ID) {
			fail(errBackupBusy.Err())
			return
		}
		b, err := s.makeBackup(ctx, app, store.BackupUpdate)
		s.backupBusy.done(app.ID)
		if err != nil {
			fail(errUpdateBackup.Err("detail", err.Error()))
			return
		}
		fmt.Fprintf(out, "Backed up (%d bytes). It is on the Backups tab.\n", b.Bytes)
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
		if err := s.Store.SetDetected(ctx, app.ID, store.Detected{Builder: res.Builder, Build: res.BuildCommand, Start: res.StartCommand}); err != nil {
			s.Log.Error("save detected commands", "app", app.ID, "err", err)
		}
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

	vols, err := s.Store.Volumes(ctx, app.ID)
	if err != nil {
		fail(err)
		return
	}
	if over := s.overLimit(vols); over != nil {
		// Stopped, or Zelie would keep trying to bring it back.
		if err := s.Store.SetStopped(ctx, app.ID, true); err != nil {
			s.Log.Error("stop app", "app", app.ID, "err", err)
		}
		fail(over)
		return
	}

	set(store.DeployStarting)
	// Two versions writing to the same files at once could corrupt them, so
	// an app with volumes has a short gap between versions instead.
	stoppedOld := false
	if len(vols) > 0 {
		if stoppedOld, err = s.stopRunning(ctx, app.ID, out); err != nil {
			fail(errStopOld.Err("detail", err.Error()))
			return
		}
	}
	container := fmt.Sprintf("%s-%d", app.ID, id)
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		s.Core.Remove(ctx, container)
		if stoppedOld {
			s.restoreLive(ctx, app, vols, out)
		}
	}
	if d.Image, err = s.start(ctx, app, d.Image, container, vols, out); err != nil {
		undo()
		fail(err)
		return
	}
	if err := s.syncRoutes(ctx, map[string]string{app.ID: container}); err != nil {
		undo()
		fail(errRoute.Err("domain", app.Domain, "detail", err.Error()))
		return
	}
	if err := s.Store.GoLive(ctx, d, s.now()); err != nil {
		fail(err)
		return
	}
	fmt.Fprintln(out, "The new version is live.")
	s.imageUpdates.wentLive(app.ID, d.Image)
	if d.Cause != store.CauseRecover {
		s.crashes.reset(app.ID)
	}
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
	env, sealed, err := s.vars(ctx, app)
	if err != nil {
		return res, commit, err
	}
	// Railpack takes its build command from its own variable. Dockerfile
	// builds ignore it.
	if app.BuildCommand != "" {
		env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, railpackBuildCmd+"=") })
		env = append(env, railpackBuildCmd+"="+app.BuildCommand)
		fmt.Fprintf(out, "Build command: %s\n", app.BuildCommand)
	}
	res, err = s.Core.Build(ctx, app.ID, commit[:12], env, sealed, archive, out)
	return res, commit, err
}

// runTests runs the app's test command in the new image, with the app's
// variables, and fails if it exits with anything but 0.
func (s *Server) runTests(ctx context.Context, app store.App, image string, deployment int64, out io.Writer) error {
	fmt.Fprintf(out, "Running the tests: %s\n", app.TestCommand)
	env, sealed, linked, err := s.appEnv(ctx, app)
	if err != nil {
		return err
	}
	// Test runners that would otherwise wait for changes run once.
	env = append(env, "CI=true")
	id := fmt.Sprintf("%s-%d-test", app.ID, deployment)
	_, err = s.Core.RunApp(ctx, engine.Spec{
		ID: id, App: app.ID, Image: image, Args: []string{"sh", "-c", app.TestCommand}, Env: env, Network: app.ID,
		MemoryBytes: app.MemoryMB << 20, CPUs: app.CPUs, Pids: defaultPids,
	}, sealed, linked...)
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
		return errTestsTimeout.Err("minutes", int(testTimeout/time.Minute))
	case err != nil:
		return err
	case code != 0:
		return errTestsFailed.Err("code", code)
	}
	fmt.Fprintln(out, "The tests passed.")
	return nil
}

// appEnv returns the app's variables for a container: the plain ones with
// PORT and those of its databases, the sealed ones, and the ones the core
// makes from its databases' passwords.
func (s *Server) appEnv(ctx context.Context, app store.App) (env, sealed []string, linked []core.LinkedVar, err error) {
	env, sealed, err = s.vars(ctx, app)
	if err != nil || app.IsDatabase() {
		return env, sealed, nil, err
	}
	dbEnv, linked, err := s.linkedEnv(ctx, app)
	// Most frameworks read the port to listen on from PORT.
	return append(append([]string{"PORT=" + strconv.Itoa(app.Port)}, env...), dbEnv...), sealed, linked, err
}

// vars returns the app's own variables, plain and sealed.
func (s *Server) vars(ctx context.Context, app store.App) (env, sealed []string, err error) {
	vars, err := s.Store.Env(ctx, app.ID)
	if err != nil {
		return nil, nil, err
	}
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
// or staying up for a while otherwise, like a bot with no web side. It
// returns the image that runs, pinned by digest.
func (s *Server) start(ctx context.Context, app store.App, image, container string, vols []store.Volume, out io.Writer) (string, error) {
	env, sealed, linked, err := s.appEnv(ctx, app)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "Starting %s.\n", image)
	var args []string
	if app.StartCommand != "" {
		fmt.Fprintf(out, "Start command: %s\n", app.StartCommand)
		args = []string{"sh", "-c", app.StartCommand}
	}
	db, isDB := engineOf(app)
	if isDB {
		args = db.Args
	}
	pinned, err := s.Core.RunApp(ctx, engine.Spec{
		ID: container, App: app.ID, Image: image, Args: args, Env: env, Network: app.ID, Volumes: volumeMounts(vols),
		MemoryBytes: app.MemoryMB << 20, CPUs: app.CPUs, Pids: defaultPids,
	}, sealed, linked...)
	if err != nil {
		return "", err
	}
	if pinned == "" {
		pinned = image
	}
	return pinned, s.waitHealthy(ctx, app, container, isDB, out)
}

// waitHealthy waits until a container that was just started is healthy, as
// start describes.
func (s *Server) waitHealthy(ctx context.Context, app store.App, container string, isDB bool, out io.Writer) error {
	if isDB {
		fmt.Fprintf(out, "Waiting for it to accept connections on port %d.\n", app.Port)
	}
	if app.Domain != "" {
		fmt.Fprintf(out, "Waiting for it to answer at %s on port %d.\n", app.HealthPath, app.Port)
	}
	deadline := time.Now().Add(startupLimit)
	var upSince time.Time
	// The status the app last answered with; 0 until it answers.
	lastStatus := 0
	for {
		st, err := s.containerStatus(ctx, container)
		if err != nil {
			return err
		}
		switch {
		case st.State == "running" && isDB:
			if s.portOpen(ctx, st.IP, app.Port) {
				fmt.Fprintln(out, "It accepts connections.")
				return nil
			}
		case st.State == "running" && app.Domain != "":
			addr := netip.AddrPortFrom(st.IP, uint16(app.Port)).String()
			code, err := s.healthCheck(ctx, "http://"+addr+app.HealthPath, app.Domain)
			if err == nil && code < 500 {
				fmt.Fprintf(out, "It answered with %d.\n", code)
				return nil
			}
			if err == nil {
				lastStatus = code
			}
		case st.State == "running" && upSince.IsZero():
			upSince = time.Now()
		case st.State == "running" && time.Since(upSince) >= startupGrace:
			return nil
		case st.State == "stopped":
			fmt.Fprintln(out, "\nThe app stopped right after starting. Its last output:")
			s.Core.Logs(ctx, container, false, 4<<10, out)
			return errStoppedAtStart.Err()
		}
		if time.Now().After(deadline) {
			if isDB {
				fmt.Fprintln(out, "\nThe database's last output:")
				s.Core.Logs(ctx, container, false, 4<<10, out)
				return errDBNoAnswer.Err("port", app.Port, "seconds", int(startupLimit/time.Second))
			}
			if app.Domain != "" {
				fmt.Fprintln(out, "\nThe app's last output:")
				s.Core.Logs(ctx, container, false, 4<<10, out)
				if lastStatus != 0 {
					return errBadAnswer.Err("status", lastStatus, "path", app.HealthPath, "port", app.Port, "seconds", int(startupLimit/time.Second))
				}
				return errNoAnswer.Err("path", app.HealthPath, "port", app.Port, "seconds", int(startupLimit/time.Second))
			}
			return errNotStarted.Err()
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

// stopRunning stops the app's running containers, giving each time to save
// its state. It reports whether there were any.
func (s *Server) stopRunning(ctx context.Context, app string, out io.Writer) (bool, error) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return false, err
	}
	stopped := false
	for _, c := range list {
		if c.App != app || c.State != "running" {
			continue
		}
		if !stopped {
			fmt.Fprintln(out, "This app keeps files in volumes, so the old version stops before the new one starts.")
		}
		if err := s.Core.Stop(ctx, c.ID, volumeStopGrace); err != nil {
			return stopped, err
		}
		stopped = true
	}
	return stopped, nil
}

// restoreLive starts the live version again after a new one failed to
// replace it.
func (s *Server) restoreLive(ctx context.Context, app store.App, vols []store.Volume, out io.Writer) {
	live, err := s.Store.LiveDeployment(ctx, app.ID)
	if err != nil {
		return
	}
	fmt.Fprintln(out, "\nStarting the previous version again.")
	container := fmt.Sprintf("%s-%d", app.ID, live.ID)
	s.Core.Remove(ctx, container)
	if _, err := s.start(ctx, app, live.Image, container, vols, out); err != nil {
		fmt.Fprintf(out, "The previous version did not start either: %v\n", err)
		s.Log.Error("restore live version", "app", app.ID, "err", err)
		return
	}
	if err := s.syncRoutes(ctx, nil); err != nil {
		s.Log.Error("restore live version routes", "app", app.ID, "err", err)
	}
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
