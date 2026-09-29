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
	"github.com/Caria-Core/zelie/internal/msg"
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
	Error      *msg.Msg   `json:"error,omitempty"`
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
	// Engine and EngineVersion are set for a database.
	Engine        string `json:"engine,omitempty"`
	EngineVersion string `json:"engine_version,omitempty"`
	// Kind is app or game. A database is an app with an engine.
	Kind string `json:"kind"`
	// Runtime is the catalog id of the egg a files app runs, such as nodejs.
	Runtime string `json:"runtime,omitempty"`
	// State is the live container's: running, stopped, or none when
	// nothing has gone live yet.
	State string `json:"state"`
	// Stopped is set when the user stopped the app.
	Stopped bool `json:"stopped"`
	// Crashing says why Zelie stopped bringing the app back up.
	Crashing *msg.Msg `json:"crashing,omitempty"`
	// VolumeFull says which volume keeps the app from running.
	VolumeFull *msg.Msg        `json:"volume_full,omitempty"`
	Latest     *deploymentJSON `json:"latest,omitempty"`
	// Update is a newer build of the image's tag, found by the daily check.
	Update *imageUpdate `json:"update,omitempty"`
	// UpgradeTo is a newer major version a database can move to.
	UpgradeTo string `json:"upgrade_to,omitempty"`
}

// appOut describes an app for the interface. containers is the core's list,
// fetched once by the caller.
func (s *Server) appOut(ctx context.Context, a store.App, containers []engine.Status) (appJSON, error) {
	out := appJSON{ID: a.ID, Source: a.Source, Image: a.Image, Repo: a.Repo, Branch: a.Branch,
		Port: a.Port, Domain: a.Domain, MemoryMB: a.MemoryMB, CPUs: a.CPUs, AutoDeploy: a.AutoDeploy, HealthPath: a.HealthPath, TestCommand: a.TestCommand, BuildCommand: a.BuildCommand, StartCommand: a.StartCommand, Detected: a.Detected, RestartPulls: a.RestartPulls, Engine: a.Engine, EngineVersion: a.EngineVersion, Kind: a.Kind, State: "none",
		Stopped: a.Stopped, Crashing: s.crashes.gaveUp(a.ID), Update: s.imageUpdates.get(a.ID), UpgradeTo: upgradeTo(a)}
	if a.IsFiles() {
		if g, err := s.Store.GameServer(ctx, a.ID); err == nil {
			if stored, err := s.Store.Egg(ctx, g.EggID); err == nil {
				out.Runtime = stored.Source
			}
		}
	}
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return out, err
	}
	if over := s.overLimit(vols); over != nil {
		out.VolumeFull = &over.Msg
	}
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
	// Volumes are only for a new app; later they have their own requests.
	Volumes []volumeRequest `json:"volumes"`
}

var (
	errBadSource       = msg.Define(http.StatusBadRequest, "app.bad_source", "The source must be github or image.")
	errAppExists       = msg.Define(http.StatusConflict, "app.exists", "An app with that name or domain already exists.")
	errPanelDomain     = msg.Define(http.StatusConflict, "app.panel_domain", "That is the panel's own domain.")
	errNoApp           = msg.Define(http.StatusNotFound, "app.not_found", "There is no such app.")
	errFixedFields     = msg.Define(http.StatusBadRequest, "app.fixed", "An app's name and source cannot change.")
	errVolumesOneByOne = msg.Define(http.StatusBadRequest, "app.volumes_one_by_one", "Volumes are changed one at a time.")
	errDatabaseFields  = msg.Define(http.StatusBadRequest, "db.fixed", "Only a database's memory and CPU can change.")
	errDomainTaken     = msg.Define(http.StatusConflict, "app.domain_taken", "Another app already uses that domain.")
	errTooManyVars     = msg.Define(http.StatusBadRequest, "env.too_many", "There are too many variables.")
	errDatabaseVars    = msg.Define(http.StatusBadRequest, "env.database", "Zelie sets a database's variables.")
	errBadVarName      = msg.Define(http.StatusBadRequest, "env.bad_name", "{name} is not a valid variable name.")
	errVarTwice        = msg.Define(http.StatusBadRequest, "env.twice", "{name} is set twice.")
	errVarFromLink     = msg.Define(http.StatusConflict, "env.from_link", "{name} comes from a linked database. Change the link's prefix to set your own.")
	errVarTooLong      = msg.Define(http.StatusBadRequest, "env.too_long", "The value of {name} is too long.")
	errVarNothingKept  = msg.Define(http.StatusBadRequest, "env.nothing_kept", "{name} has no saved value to keep.")
	errFilesApp        = msg.Define(http.StatusConflict, "app.files_app", "This app runs your files, so there is nothing to build, deploy or roll back. Restart it to run your changes.")
	errFilesFields     = msg.Define(http.StatusBadRequest, "app.files_fixed", "A files app has no repository, image or build. Its startup is on the Startup tab.")
	errNothingLive     = msg.Define(http.StatusConflict, "deploy.nothing_live", "Nothing is live yet. Deploy first.")
	errNoDeployment    = msg.Define(http.StatusNotFound, "deploy.not_found", "There is no such deployment.")
	errNotLiveBefore   = msg.Define(http.StatusConflict, "deploy.never_live", "Only a version that was live can be rolled back to.")
	errImagePruned     = msg.Define(http.StatusConflict, "deploy.pruned", "That version's image was deleted to free space.")
	errBuildNotGitHub  = msg.Define(http.StatusBadRequest, "app.build_github_only", "Only apps built from GitHub have a build command.")
	errTestsNotGitHub  = msg.Define(http.StatusBadRequest, "app.tests_github_only", "Only apps built from GitHub run tests.")
	errPullsNotGitHub  = msg.Define(http.StatusBadRequest, "app.pulls_github_only", "Only apps built from GitHub can pull on restart.")
	errPushNotGitHub   = msg.Define(http.StatusBadRequest, "app.push_github_only", "Only apps built from GitHub deploy on push.")
	errNoImage         = msg.Define(http.StatusBadRequest, "app.no_image", "Enter an image, such as nginx:alpine.")
	errReservedImage   = msg.Define(http.StatusBadRequest, "app.reserved_image", "That image name is reserved for images Zelie builds.")
	errBadImage        = msg.Define(http.StatusBadRequest, "app.bad_image", "{image} is not a valid image name.")
	errBadPort         = msg.Define(http.StatusBadRequest, "app.bad_port", "The port must be between 1 and 65535.")
	errBadDomain       = msg.Define(http.StatusBadRequest, "app.bad_domain", "{domain} is not a valid domain name.")
	errBadMemory       = msg.Define(http.StatusBadRequest, "app.bad_memory", "The memory limit must be at least 16 MB.")
	errBadCPUs         = msg.Define(http.StatusBadRequest, "app.bad_cpus", "The CPU limit is out of range.")
	errBadHealthPath   = msg.Define(http.StatusBadRequest, "app.bad_health_path", "The health check path must start with / and hold no spaces.")
	errBadCommand      = msg.Define(http.StatusBadRequest, "app.bad_command", "A command must be one line of at most 500 characters.")
	errBadAppName      = msg.Define(http.StatusBadRequest, "app.bad_name", "The name must be up to 40 lowercase letters, digits and dashes. It cannot start with zelie, which Zelie keeps for its own parts.")
)

// apply copies the fields that were sent onto a and checks the result.
func (req appRequest) apply(a *store.App) *msg.Error {
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
		if bad := checkCommand(cmd); bad != nil {
			return bad
		}
		*c.dst = cmd
	}
	if req.BuildCommand != nil && a.BuildCommand != "" && a.Source != store.SourceGitHub {
		return errBuildNotGitHub.Err()
	}
	if req.TestCommand != nil {
		cmd := strings.TrimSpace(*req.TestCommand)
		switch {
		case a.Source != store.SourceGitHub:
			return errTestsNotGitHub.Err()
		}
		if bad := checkCommand(cmd); bad != nil {
			return bad
		}
		a.TestCommand, a.TestSet = cmd, true
	}
	if req.RestartPulls != nil {
		if a.Source != store.SourceGitHub {
			return errPullsNotGitHub.Err()
		}
		a.RestartPulls = *req.RestartPulls
	}
	if req.AutoDeploy != nil {
		if a.Source != store.SourceGitHub {
			return errPushNotGitHub.Err()
		}
		a.AutoDeploy = *req.AutoDeploy
	}
	switch {
	case a.Source == store.SourceImage:
		if bad := checkImageRef(a.Image); bad != nil {
			return bad
		}
	case a.Source == store.SourceGitHub:
		if bad := checkRepo(a.Repo, a.Branch); bad != nil {
			return bad
		}
	}
	switch {
	case a.Port < 1 || a.Port > 65535:
		return errBadPort.Err()
	case a.Domain != "" && !proxy.ValidDomain(a.Domain):
		return errBadDomain.Err("domain", a.Domain)
	case a.MemoryMB < 16 || a.MemoryMB > 1<<20:
		return errBadMemory.Err()
	case a.CPUs <= 0 || a.CPUs > 1024:
		return errBadCPUs.Err()
	case !validHealthPath(a.HealthPath):
		return errBadHealthPath.Err()
	}
	return nil
}

func checkCommand(cmd string) *msg.Error {
	if len(cmd) > 500 || strings.ContainsAny(cmd, "\x00\r\n") {
		return errBadCommand.Err()
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

func checkAppID(id string) *msg.Error {
	// Containers are named <app>-<deployment>, which must fit in 63
	// characters, and names starting with zelie are Zelie's own.
	if !engine.ValidID(id) || len(id) > 40 || strings.HasPrefix(id, "zelie") {
		return errBadAppName.Err()
	}
	return nil
}

func (s *Server) createApp(w http.ResponseWriter, r *http.Request) {
	var req appRequest
	if !decode(w, r, &req) {
		return
	}
	if bad := checkAppID(req.ID); bad != nil {
		writeError(w, bad)
		return
	}
	a := store.App{ID: req.ID, Source: req.Source, MemoryMB: defaultMemoryMB, CPUs: defaultCPUs, HealthPath: "/", CreatedAt: s.now()}
	switch a.Source {
	case store.SourceGitHub:
		a.Branch, a.Port, a.AutoDeploy = "main", 3000, true
	case store.SourceImage:
		a.Port = 80
	default:
		writeError(w, errBadSource.Err())
		return
	}
	if bad := req.apply(&a); bad != nil {
		writeError(w, bad)
		return
	}
	if !s.domainFree(w, r, a) {
		return
	}
	switch err := s.Store.CreateApp(r.Context(), a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, errAppExists.Err())
		return
	case err != nil:
		s.fail(w, "create app", err)
		return
	}
	for _, vr := range req.Volumes {
		if _, err := s.createVolume(r.Context(), a, vr); err != nil {
			ctx := context.WithoutCancel(r.Context())
			s.removeAppVolumes(ctx, a.ID)
			s.Store.DeleteApp(ctx, a.ID)
			s.failWith(w, "create volume", err)
			return
		}
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
		writeError(w, errPanelDomain.Err())
		return false
	}
	return true
}

// appFrom loads the app named in the path, writing a 404 if there is none.
func (s *Server) appFrom(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	a, err := s.Store.App(r.Context(), r.PathValue("app"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoApp.Err())
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
	if !ok || !s.notGame(w, a) {
		return
	}
	var req appRequest
	if !decode(w, r, &req) {
		return
	}
	if req.ID != "" || req.Source != "" {
		writeError(w, errFixedFields.Err())
		return
	}
	if req.Volumes != nil {
		writeError(w, errVolumesOneByOne.Err())
		return
	}
	if a.IsDatabase() && (req.Image != nil || req.Repo != nil || req.Branch != nil || req.Port != nil || req.Domain != nil ||
		req.AutoDeploy != nil || req.HealthPath != nil || req.TestCommand != nil || req.BuildCommand != nil || req.StartCommand != nil || req.RestartPulls != nil) {
		writeError(w, errDatabaseFields.Err())
		return
	}
	if a.IsFiles() && (req.Image != nil || req.Repo != nil || req.Branch != nil || req.AutoDeploy != nil || req.TestCommand != nil ||
		req.BuildCommand != nil || req.StartCommand != nil || req.RestartPulls != nil) {
		writeError(w, errFilesFields.Err())
		return
	}
	oldDomain := a.Domain
	if bad := req.apply(&a); bad != nil {
		writeError(w, bad)
		return
	}
	if a.IsFiles() {
		// Eggs run with the same floor for memory as game servers.
		h, err := s.Core.Host(r.Context())
		if err != nil {
			s.coreFailed(w, "read host", err)
			return
		}
		if bad := s.gameLimits(&a, new(int64(1)), h); bad != nil {
			writeError(w, bad)
			return
		}
	}
	if !s.domainFree(w, r, a) {
		return
	}
	switch err := s.Store.UpdateApp(r.Context(), a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, errDomainTaken.Err())
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
		writeError(w, errTooManyVars.Err())
		return
	}
	if a.IsDatabase() {
		writeError(w, errDatabaseVars.Err())
		return
	}
	ctx := r.Context()
	plain, linked, err := s.linkedEnv(ctx, a)
	if err != nil {
		s.fail(w, "load links", err)
		return
	}
	fromLinks := map[string]bool{}
	for _, v := range plain {
		name, _, _ := strings.Cut(v, "=")
		fromLinks[name] = true
	}
	for _, v := range linked {
		fromLinks[v.Name] = true
	}
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
			writeError(w, errBadVarName.Err("name", v.Name))
			return
		case seen[v.Name]:
			writeError(w, errVarTwice.Err("name", v.Name))
			return
		case fromLinks[v.Name]:
			writeError(w, errVarFromLink.Err("name", v.Name))
			return
		case len(v.Value) > 32<<10:
			writeError(w, errVarTooLong.Err("name", v.Name))
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
				writeError(w, errVarNothingKept.Err("name", v.Name))
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
			s.fail(w, "seal variable", err)
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
	if !ok || !s.notGame(w, a) || !s.notFiles(w, a) || !s.unstop(w, r, a) {
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
	if !ok || !s.notGame(w, a) {
		return
	}
	if a.IsFiles() {
		s.filesPower(w, r, a, "restart")
		return
	}
	if !s.unstop(w, r, a) {
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
		writeError(w, errNothingLive.Err())
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
	if !ok || !s.notGame(w, a) || !s.notFiles(w, a) || !s.unstop(w, r, a) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	d, err := s.Store.Deployment(r.Context(), a.ID, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoDeployment.Err())
		return
	case err != nil:
		s.fail(w, "load deployment", err)
		return
	case d.State != store.DeployReplaced && d.State != store.DeployLive:
		writeError(w, errNotLiveBefore.Err())
		return
	case d.Pruned:
		writeError(w, errImagePruned.Err())
		return
	}
	s.redeploy(w, r, a, d, store.CauseRollback)
}

// unstop clears the stopped flag: deploying or restarting means the user
// wants the app running. An app with a volume over its limit may not run.
func (s *Server) unstop(w http.ResponseWriter, r *http.Request, a store.App) bool {
	if err := s.unstopApp(r.Context(), a); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

func (s *Server) unstopApp(ctx context.Context, a store.App) *msg.Error {
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return s.internalError("list volumes", err)
	}
	if over := s.overLimit(vols); over != nil {
		return over
	}
	if !a.Stopped {
		return nil
	}
	if err := s.Store.SetStopped(ctx, a.ID, false); err != nil {
		return s.internalError("start app", err)
	}
	return nil
}

func (s *Server) stopHandler(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok || !s.notGame(w, a) {
		return
	}
	if a.IsFiles() {
		s.filesPower(w, r, a, "stop")
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
	if !ok || !s.notGame(w, a) {
		return
	}
	if a.IsFiles() {
		s.filesPower(w, r, a, "start")
		return
	}
	if !s.unstop(w, r, a) {
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
		writeError(w, errNoDeployment.Err())
		return
	}
	ctx := r.Context()
	if _, err := s.Store.Deployment(ctx, a.ID, id); err != nil {
		writeError(w, errNoDeployment.Err())
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
		ev.send("notice", errNothingLive.With())
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
	// Once started, the removal finishes even if the browser goes away:
	// half of it would leave containers and volumes nothing refers to.
	ctx := context.WithoutCancel(r.Context())
	// A deployment in progress would start a container for an app that no
	// longer exists.
	s.deploys.cancel(a.ID)
	defer s.gameRuns.forget(a.ID)
	defer s.consoles.forget(a.ID)
	defer s.consoleHist.forget(a.ID)
	unlock := s.deploys.lock(a.ID)
	defer unlock()
	list, err := s.Core.List(ctx)
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	for _, c := range list {
		if c.App == a.ID {
			// The cancelled deployment may be removing it too.
			if err := s.Core.Remove(ctx, c.ID); err != nil && !isNotFound(err) {
				s.coreFailed(w, "remove container", err)
				return
			}
		}
	}
	if err := s.removeAppVolumes(ctx, a.ID); err != nil {
		s.coreFailed(w, "remove volumes", err)
		return
	}
	if err := s.unlinkAll(ctx, a); err != nil {
		s.coreFailed(w, "remove links", err)
		return
	}
	s.removeAppImages(ctx, a.ID)
	if ports, err := s.Store.AppAllocations(ctx, a.ID); err != nil {
		s.fail(w, "list ports", err)
		return
	} else if len(ports) > 0 {
		// Closing the ports here; the pool takes them back with the app.
		if err := s.Core.ClearForwards(ctx, a.ID); err != nil {
			s.coreFailed(w, "close ports", err)
			return
		}
	}
	if _, err := s.Store.ExternalAccess(ctx, a.ID); err == nil {
		// Its containers are gone, and its user with them.
		if err := s.Core.RemoveExternal(ctx, a.ID, a.Engine, ""); err != nil {
			s.coreFailed(w, "remove outside access", err)
			return
		}
	}
	if err := s.removeDeployLogs(ctx, a.ID); err != nil {
		s.fail(w, "remove deployment logs", err)
		return
	}
	err = s.Store.DeleteApp(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		// Another request deleted it meanwhile, as when a browser sends a
		// delete again after losing the first answer.
		writeError(w, errNoApp.Err())
		return
	}
	if err != nil {
		s.fail(w, "delete app", err)
		return
	}
	if a.RunsEgg() {
		s.syncSFTPVolumes(ctx)
	}
	if err := s.syncRoutes(ctx, nil); err != nil {
		s.fail(w, "update routes", err)
		return
	}
	s.Log.Info("app deleted", "app", a.ID, "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusNoContent)
}
