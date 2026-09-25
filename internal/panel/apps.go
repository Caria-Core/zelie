package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/Caria-Core/zelie/internal/store"
)

type deploymentJSON struct {
	// Kept is true while the image exists, so it can be rolled back to.
	Kept       bool       `json:"kept"`
	ID         int64      `json:"id"`
	Version    string     `json:"version"`
	Image      string     `json:"image,omitempty"`
	State      string     `json:"state"`
	Error      string     `json:"error,omitempty"`
	Cause      string     `json:"cause"`
	Message    string     `json:"message,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

func deploymentOut(d store.Deployment) deploymentJSON {
	out := deploymentJSON{ID: d.ID, Version: d.Version, Image: d.Image, State: d.State, Error: d.Error, Cause: d.Cause, Message: d.Message, CreatedAt: d.CreatedAt,
		Kept: d.Image != "" && !d.Pruned}
	if !d.FinishedAt.IsZero() {
		out.FinishedAt = &d.FinishedAt
	}
	return out
}

type appJSON struct {
	ID       string  `json:"id"`
	Source   string  `json:"source"`
	Image    string  `json:"image,omitempty"`
	Repo     string  `json:"repo,omitempty"`
	Branch   string  `json:"branch,omitempty"`
	Port     int     `json:"port"`
	Domain   string  `json:"domain,omitempty"`
	MemoryMB int64   `json:"memory_mb"`
	CPUs     float64 `json:"cpus"`
	// AutoDeploy deploys every push to the branch.
	AutoDeploy   bool           `json:"auto_deploy"`
	HealthPath   string         `json:"health_path"`
	TestCommand  string         `json:"test_command"`
	BuildCommand string         `json:"build_command"`
	StartCommand string         `json:"start_command"`
	Detected     store.Detected `json:"detected"`
	RestartPulls bool           `json:"restart_pulls"`
	// State is the live container's: running, stopped, or none when
	// nothing has gone live yet.
	State string `json:"state"`
	// Stopped is set when the user stopped the app.
	Stopped bool `json:"stopped"`
	// Crashing says why Zelie stopped bringing the app back up.
	Crashing string          `json:"crashing,omitempty"`
	Latest   *deploymentJSON `json:"latest,omitempty"`
}

// appOut describes an app for the interface. containers is the core's list,
// fetched once by the caller.
func (s *Server) appOut(ctx context.Context, a store.App, containers []engine.Status) (appJSON, error) {
	out := appJSON{ID: a.ID, Source: a.Source, Image: a.Image, Repo: a.Repo, Branch: a.Branch,
		Port: a.Port, Domain: a.Domain, MemoryMB: a.MemoryMB, CPUs: a.CPUs, AutoDeploy: a.AutoDeploy, HealthPath: a.HealthPath, TestCommand: a.TestCommand, BuildCommand: a.BuildCommand, StartCommand: a.StartCommand, Detected: a.Detected, RestartPulls: a.RestartPulls, State: "none",
		Stopped: a.Stopped, Crashing: s.crashes.gaveUp(a.ID)}
	recent, err := s.Store.Deployments(ctx, a.ID, 1)
	if err != nil {
		return out, err
	}
	if len(recent) > 0 {
		d := deploymentOut(recent[0])
		out.Latest = &d
	}
	live, err := s.Store.LiveDeployment(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.State = "stopped"
	for _, c := range containers {
		if c.ID == fmt.Sprintf("%s-%d", a.ID, live.ID) {
			out.State = c.State
		}
	}
	return out, nil
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.Store.Apps(r.Context())
	if err != nil {
		s.fail(w, "list apps", err)
		return
	}
	containers, err := s.Core.List(r.Context())
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	out := make([]appJSON, 0, len(apps))
	for _, a := range apps {
		aj, err := s.appOut(r.Context(), a, containers)
		if err != nil {
			s.fail(w, "describe app", err)
			return
		}
		out = append(out, aj)
	}
	writeJSON(w, http.StatusOK, out)
}

// appRequest is what the interface sends to create or change an app.
// Pointers tell a missing field from a zero one.
type appRequest struct {
	ID       string   `json:"id"`
	Source   string   `json:"source"`
	Image    *string  `json:"image"`
	Repo     *string  `json:"repo"`
	Branch   *string  `json:"branch"`
	Port     *int     `json:"port"`
	Domain   *string  `json:"domain"`
	MemoryMB *int64   `json:"memory_mb"`
	CPUs     *float64 `json:"cpus"`
	// AutoDeploy is only for apps built from GitHub.
	AutoDeploy *bool   `json:"auto_deploy"`
	HealthPath *string `json:"health_path"`
	// TestCommand is only for apps built from GitHub. Empty turns tests
	// off.
	TestCommand *string `json:"test_command"`
	// Empty uses what the build chose. BuildCommand is only for apps
	// built from GitHub.
	BuildCommand *string `json:"build_command"`
	StartCommand *string `json:"start_command"`
	RestartPulls *bool   `json:"restart_pulls"`
}

// apply copies the fields that were sent onto a and checks the result.
func (req appRequest) apply(a *store.App) error {
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&a.Image, req.Image)
	set(&a.Repo, req.Repo)
	set(&a.Branch, req.Branch)
	set(&a.Domain, req.Domain)
	set(&a.HealthPath, req.HealthPath)
	a.Domain = strings.ToLower(strings.TrimSuffix(a.Domain, "."))
	if req.Port != nil {
		a.Port = *req.Port
	}
	if req.MemoryMB != nil {
		a.MemoryMB = *req.MemoryMB
	}
	if req.CPUs != nil {
		a.CPUs = *req.CPUs
	}
	for _, c := range []struct {
		dst *string
		src *string
	}{{&a.BuildCommand, req.BuildCommand}, {&a.StartCommand, req.StartCommand}} {
		if c.src == nil {
			continue
		}
		cmd := strings.TrimSpace(*c.src)
		if err := checkCommand(cmd); err != nil {
			return err
		}
		*c.dst = cmd
	}
	if req.BuildCommand != nil && a.BuildCommand != "" && a.Source != store.SourceGitHub {
		return errors.New("only apps built from GitHub have a build command")
	}
	if req.TestCommand != nil {
		cmd := strings.TrimSpace(*req.TestCommand)
		switch {
		case a.Source != store.SourceGitHub:
			return errors.New("only apps built from GitHub run tests")
		}
		if err := checkCommand(cmd); err != nil {
			return err
		}
		a.TestCommand, a.TestSet = cmd, true
	}
	if req.RestartPulls != nil {
		if a.Source != store.SourceGitHub {
			return errors.New("only apps built from GitHub can pull on restart")
		}
		a.RestartPulls = *req.RestartPulls
	}
	if req.AutoDeploy != nil {
		if a.Source != store.SourceGitHub {
			return errors.New("only apps built from GitHub deploy on push")
		}
		a.AutoDeploy = *req.AutoDeploy
	}
	switch {
	case a.Source == store.SourceImage && (a.Image == "" || len(a.Image) > 255 || strings.ContainsAny(a.Image, " \t\n")):
		return errors.New("enter an image, such as nginx:alpine")
	case a.Source == store.SourceImage && strings.HasPrefix(a.Image, engine.LocalImages):
		return errors.New("that image name is reserved for images Zelie builds")
	case a.Source == store.SourceImage && !validImage(a.Image):
		return fmt.Errorf("%q is not a valid image name", a.Image)
	case a.Source == store.SourceGitHub:
		if err := checkRepo(a.Repo, a.Branch); err != nil {
			return err
		}
	}
	switch {
	case a.Port < 1 || a.Port > 65535:
		return errors.New("the port must be between 1 and 65535")
	case a.Domain != "" && !proxy.ValidDomain(a.Domain):
		return fmt.Errorf("%q is not a valid domain name", a.Domain)
	case a.MemoryMB < 16 || a.MemoryMB > 1<<20:
		return errors.New("the memory limit must be at least 16 MB")
	case a.CPUs <= 0 || a.CPUs > 1024:
		return errors.New("the CPU limit is out of range")
	case !validHealthPath(a.HealthPath):
		return errors.New("the health check path must start with / and hold no spaces")
	}
	return nil
}

func checkCommand(cmd string) error {
	if len(cmd) > 500 || strings.ContainsAny(cmd, "\x00\r\n") {
		return errors.New("a command must be one line of at most 500 characters")
	}
	return nil
}

func validHealthPath(p string) bool {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || len(p) > 200 {
		return false
	}
	for _, r := range p {
		if r <= ' ' || r == 0x7f || r == '#' {
			return false
		}
	}
	return true
}

func validImage(ref string) bool {
	_, err := reference.ParseNormalizedNamed(ref)
	return err == nil
}

func checkAppID(id string) error {
	// Containers are named <app>-<deployment>, which must fit in 63
	// characters, and names starting with zelie are Zelie's own.
	if !engine.ValidID(id) || len(id) > 40 || strings.HasPrefix(id, "zelie") {
		return errors.New("the name must be up to 40 lowercase letters, digits and dashes, and not start with zelie")
	}
	return nil
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req appRequest
	if !decode(w, r, &req) {
		return
	}
	if err := checkAppID(req.ID); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	a := store.App{ID: req.ID, Source: req.Source, MemoryMB: defaultMemoryMB, CPUs: defaultCPUs, HealthPath: "/", CreatedAt: s.now()}
	switch a.Source {
	case store.SourceGitHub:
		a.Branch, a.Port, a.AutoDeploy = "main", 3000, true
	case store.SourceImage:
		a.Port = 80
	default:
		writeError(w, http.StatusBadRequest, errors.New("the source must be github or image"))
		return
	}
	if err := req.apply(&a); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.domainFree(w, r, a) {
		return
	}
	switch err := s.Store.CreateApp(r.Context(), a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, errors.New("an app with that name or domain already exists"))
		return
	case err != nil:
		s.fail(w, "create app", err)
		return
	}
	s.Log.Info("app created", "app", a.ID, "user", loginFrom(r.Context()).account.ID)
	if _, err := s.deploy(r.Context(), a, store.Deployment{}); err != nil {
		s.fail(w, "deploy", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": a.ID})
}

// domainFree rejects the panel's own domain, which the proxy already serves.
func (s *Server) domainFree(w http.ResponseWriter, r *http.Request, a store.App) bool {
	if a.Domain == "" || s.Proxy == nil {
		return true
	}
	cfg, err := s.Proxy.Config(r.Context())
	if err != nil {
		s.fail(w, "read proxy config", err)
		return false
	}
	if cfg.Panel == a.Domain {
		writeError(w, http.StatusConflict, errors.New("that is the panel's own domain"))
		return false
	}
	return true
}

// appFrom loads the app named in the path, writing a 404 if there is none.
func (s *Server) appFrom(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	a, err := s.Store.App(r.Context(), r.PathValue("app"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("no such app"))
		return a, false
	case err != nil:
		s.fail(w, "load app", err)
		return a, false
	}
	return a, true
}

type envJSON struct {
	Name   string `json:"name"`
	Value  string `json:"value"` // empty for secrets, which are never sent back
	Secret bool   `json:"secret"`
}

func (s *Server) getApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	containers, err := s.Core.List(ctx)
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	aj, err := s.appOut(ctx, a, containers)
	if err != nil {
		s.fail(w, "describe app", err)
		return
	}
	vars, err := s.Store.Env(ctx, a.ID)
	if err != nil {
		s.fail(w, "load variables", err)
		return
	}
	deps, err := s.Store.Deployments(ctx, a.ID, 20)
	if err != nil {
		s.fail(w, "load deployments", err)
		return
	}
	out := struct {
		appJSON
		Env         []envJSON        `json:"env"`
		Deployments []deploymentJSON `json:"deployments"`
	}{appJSON: aj, Env: []envJSON{}, Deployments: []deploymentJSON{}}
	for _, v := range vars {
		e := envJSON{Name: v.Name, Secret: v.Secret}
		if !v.Secret {
			e.Value = v.Value
		}
		out.Env = append(out.Env, e)
	}
	for _, d := range deps {
		out.Deployments = append(out.Deployments, deploymentOut(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// updateApp changes an app's settings. A new domain takes effect at once;
// everything else with the next deployment.
func (s *Server) updateApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	var req appRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ID != "" || req.Source != "" {
		writeError(w, http.StatusBadRequest, errors.New("an app's name and source cannot change"))
		return
	}
	oldDomain := a.Domain
	if err := req.apply(&a); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !s.domainFree(w, r, a) {
		return
	}
	switch err := s.Store.UpdateApp(r.Context(), a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, errors.New("another app already uses that domain"))
		return
	case err != nil:
		s.fail(w, "update app", err)
		return
	}
	if a.Domain != oldDomain {
		if err := s.syncRoutes(r.Context(), nil); err != nil {
			s.fail(w, "update routes", err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// setEnv replaces an app's variables. A secret sent with keep and no value
// keeps its sealed value, which the interface never saw.
func (s *Server) setEnv(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	var req []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Secret bool   `json:"secret"`
		Keep   bool   `json:"keep"`
	}
	if !decode(w, r, &req) {
		return
	}
	if len(req) > 200 {
		writeError(w, http.StatusBadRequest, errors.New("too many variables"))
		return
	}
	ctx := r.Context()
	old, err := s.Store.Env(ctx, a.ID)
	if err != nil {
		s.fail(w, "load variables", err)
		return
	}
	sealedBefore := map[string]string{}
	for _, v := range old {
		if v.Secret {
			sealedBefore[v.Name] = v.Value
		}
	}
	var key secret.PublicKey
	seen := map[string]bool{}
	vars := make([]store.EnvVar, 0, len(req))
	for _, v := range req {
		switch {
		case !secret.ValidName(v.Name):
			writeError(w, http.StatusBadRequest, fmt.Errorf("%q is not a valid variable name", v.Name))
			return
		case seen[v.Name]:
			writeError(w, http.StatusBadRequest, fmt.Errorf("%s is set twice", v.Name))
			return
		case len(v.Value) > 32<<10:
			writeError(w, http.StatusBadRequest, fmt.Errorf("the value of %s is too long", v.Name))
			return
		}
		seen[v.Name] = true
		if !v.Secret {
			vars = append(vars, store.EnvVar{Name: v.Name, Value: v.Value})
			continue
		}
		if v.Keep {
			sealed, ok := sealedBefore[v.Name]
			if !ok {
				writeError(w, http.StatusBadRequest, fmt.Errorf("%s has no saved value to keep", v.Name))
				return
			}
			vars = append(vars, store.EnvVar{Name: v.Name, Value: sealed, Secret: true})
			continue
		}
		if key == (secret.PublicKey{}) {
			if key, err = s.Core.SecretKey(ctx); err != nil {
				s.coreFailed(w, "get secret key", err)
				return
			}
		}
		sealed, err := secret.Seal(key, a.ID, v.Name, v.Value)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		vars = append(vars, store.EnvVar{Name: v.Name, Value: sealed, Secret: true})
	}
	if err := s.Store.SetEnv(ctx, a.ID, vars); err != nil {
		s.fail(w, "save variables", err)
		return
	}
	// Names only: values never reach the log.
	s.Log.Info("variables changed", "app", a.ID, "count", len(vars), "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) newDeployment(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok || !s.unstop(w, r, a) {
		return
	}
	id, err := s.deploy(r.Context(), a, store.Deployment{})
	if err != nil {
		s.fail(w, "deploy", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// restartApp starts the live version again in a new container, without
// building. It goes through the same health check, so a restart that fails
// leaves the running container alone.
func (s *Server) restartApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok || !s.unstop(w, r, a) {
		return
	}
	if a.RestartPulls && a.Source == store.SourceGitHub {
		id, err := s.deploy(r.Context(), a, store.Deployment{Cause: store.CauseRestart})
		if err != nil {
			s.fail(w, "deploy", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
		return
	}
	live, err := s.Store.LiveDeployment(r.Context(), a.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, errors.New("nothing is live yet; deploy first"))
		return
	}
	if err != nil {
		s.fail(w, "load deployment", err)
		return
	}
	s.redeploy(w, r, a, live, store.CauseRestart)
}

// rollback puts an earlier deployment's image live again.
func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok || !s.unstop(w, r, a) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := s.Store.Deployment(r.Context(), a.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, errors.New("no such deployment"))
		return
	case err != nil:
		s.fail(w, "load deployment", err)
		return
	case d.State != store.DeployReplaced && d.State != store.DeployLive:
		writeError(w, http.StatusConflict, errors.New("only a version that was live can be rolled back to"))
		return
	case d.Pruned:
		writeError(w, http.StatusConflict, errors.New("that version's image was deleted to free space"))
		return
	}
	s.redeploy(w, r, a, d, store.CauseRollback)
}

// unstop clears the stopped flag: deploying or restarting means the user
// wants the app running.
func (s *Server) unstop(w http.ResponseWriter, r *http.Request, a store.App) bool {
	if !a.Stopped {
		return true
	}
	if err := s.Store.SetStopped(r.Context(), a.ID, false); err != nil {
		s.fail(w, "start app", err)
		return false
	}
	return true
}

func (s *Server) stopHandler(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	if err := s.stopApp(r.Context(), a); err != nil {
		s.coreFailed(w, "stop app", err)
		return
	}
	s.Log.Info("app stopped", "app", a.ID, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// startHandler starts a stopped app: its live version again, or a first
// deployment if nothing has gone live yet.
func (s *Server) startHandler(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok || !s.unstop(w, r, a) {
		return
	}
	s.crashes.reset(a.ID)
	live, err := s.Store.LiveDeployment(r.Context(), a.ID)
	if errors.Is(err, store.ErrNotFound) {
		id, err := s.deploy(r.Context(), a, store.Deployment{})
		if err != nil {
			s.fail(w, "deploy", err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
		return
	}
	if err != nil {
		s.fail(w, "load deployment", err)
		return
	}
	s.redeploy(w, r, a, live, store.CauseRestart)
}

func (s *Server) redeploy(w http.ResponseWriter, r *http.Request, a store.App, from store.Deployment, cause string) {
	id, err := s.deploy(r.Context(), a, store.Deployment{Version: from.Version, Image: from.Image, Cause: cause, Message: from.Message})
	if err != nil {
		s.fail(w, "deploy", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// deploymentLog streams a deployment's log as server-sent events until the
// deployment has finished.
func (s *Server) deploymentLog(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such deployment"))
		return
	}
	ctx := r.Context()
	if _, err := s.Store.Deployment(ctx, a.ID, id); err != nil {
		writeError(w, http.StatusNotFound, errors.New("no such deployment"))
		return
	}
	ev := newEventStream(w)
	defer ev.close()
	var f *os.File
	defer func() {
		if f != nil {
			f.Close()
		}
	}()
	for {
		// Check before reading, so the last lines are sent after the
		// deployment ends.
		d, err := s.Store.Deployment(ctx, a.ID, id)
		if err != nil {
			return
		}
		finished := !d.FinishedAt.IsZero()
		if f == nil {
			f, _ = os.Open(s.deployLogPath(id))
		}
		if f != nil {
			if _, err := io.Copy(ev, f); err != nil {
				return
			}
		}
		if finished {
			ev.send("done", d.State)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// appLogs streams the output of the app's live container.
func (s *Server) appLogs(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	live, err := s.Store.LiveDeployment(r.Context(), a.ID)
	if errors.Is(err, store.ErrNotFound) {
		ev := newEventStream(w)
		defer ev.close()
		ev.send("notice", "nothing is live yet")
		return
	}
	if err != nil {
		s.fail(w, "load deployment", err)
		return
	}
	s.streamLogs(w, r, fmt.Sprintf("%s-%d", a.ID, live.ID))
}

// deleteApp removes the app's containers, its domain and everything the
// panel knows about it.
func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// A deployment in progress would start a container for an app that no
	// longer exists.
	s.deploys.cancel(a.ID)
	unlock := s.deploys.lock(a.ID)
	defer unlock()
	list, err := s.Core.List(ctx)
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	for _, c := range list {
		if c.App == a.ID {
			if err := s.Core.Remove(ctx, c.ID); err != nil {
				s.coreFailed(w, "remove container", err)
				return
			}
		}
	}
	if err := s.Store.DeleteApp(ctx, a.ID); err != nil {
		s.fail(w, "delete app", err)
		return
	}
	if err := s.syncRoutes(ctx, nil); err != nil {
		s.fail(w, "update routes", err)
		return
	}
	s.Log.Info("app deleted", "app", a.ID, "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusNoContent)
}
