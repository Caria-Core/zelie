package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

// A files app runs the files in its volume with a generic egg's startup
// command: Node.js, Python and the like. It is started and stopped like a
// game server, through the same egg code, but it is an app: its domain goes
// through the proxy to a port inside the container, its databases are linked,
// and it takes no port from the pool.

// filesAppPort is where a files app listens until its owner changes it. It
// is also what the egg's SERVER_PORT holds.
const filesAppPort = 8080

const defaultFilesDiskMB = 5 << 10

var (
	errNoRuntime = msg.Define(http.StatusNotFound, "app.no_runtime", "There is no runtime called {runtime} in the list.")
)

// notFiles answers 409 for a files app, which has no builds to deploy or roll
// back.
func (s *Server) notFiles(w http.ResponseWriter, a store.App) bool {
	if a.IsFiles() {
		writeError(w, errFilesApp.Err())
		return false
	}
	return true
}

// filesPower does a power action for a files app through the game server
// code, and answers the request.
func (s *Server) filesPower(w http.ResponseWriter, r *http.Request, a store.App, action string) {
	g, err := s.Store.GameServer(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "load files app", err)
		return
	}
	res, bad := s.power(r.Context(), a, g, action, loginFrom(r.Context()).account.ID)
	if bad != nil {
		writeError(w, bad)
		return
	}
	out := map[string]any{"state": res.State}
	if res.Deployment != 0 {
		out["deployment"] = res.Deployment
	}
	writeJSON(w, res.Status, out)
}

type filesRequest struct {
	Name string `json:"name"`
	// Runtime is the catalog id of a generic egg, such as nodejs.
	Runtime string `json:"runtime"`
	// Image is one of the egg's images; the first by default.
	Image     string            `json:"image"`
	MemoryMB  int64             `json:"memory_mb"`
	CPUs      float64           `json:"cpus"`
	DiskMB    int64             `json:"disk_mb"`
	Domain    string            `json:"domain"`
	Variables map[string]string `json:"variables"`
}

// createFilesApp makes a files app and starts its install in the
// background. The volume starts empty unless the egg fetches a repository.
func (s *Server) createFilesApp(w http.ResponseWriter, r *http.Request) {
	var req filesRequest
	if !decode(w, r, &req) {
		return
	}
	if bad := checkAppID(req.Name); bad != nil {
		writeError(w, bad)
		return
	}
	ctx := r.Context()
	entry, ok := egg.Lookup(req.Runtime)
	if !ok || entry.Kind != egg.KindRuntime {
		writeError(w, errNoRuntime.Err("runtime", truncate(req.Runtime, 64)))
		return
	}
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Domain), "."))
	if domain != "" && !proxy.ValidDomain(domain) {
		writeError(w, errBadDomain.Err("domain", domain))
		return
	}
	e, raw, err := s.eggs().Catalog(ctx, entry)
	if err != nil {
		s.failWith(w, "load egg", errEggUnusable.Err("detail", err.Error()))
		return
	}
	image, bad := pickImage(e, req.Image)
	if bad != nil {
		writeError(w, bad)
		return
	}
	given := withFilesDefaults(e, req.Variables)
	vars, bad := gameVariables(e, nil, given, true)
	if bad != nil {
		writeError(w, bad)
		return
	}
	h, err := s.Core.Host(ctx)
	if err != nil {
		s.coreFailed(w, "read host", err)
		return
	}
	a := store.App{
		ID: req.Name, Source: store.SourceFiles, Image: image, Port: filesAppPort, Domain: domain,
		MemoryMB: req.MemoryMB, CPUs: req.CPUs, HealthPath: "/", CreatedAt: s.now(),
	}
	if a.MemoryMB == 0 {
		a.MemoryMB = min(max(entry.MemoryMB, defaultMemoryMB), h.MemoryBytes>>20)
	}
	disk := req.DiskMB
	if disk == 0 {
		disk = max(minVolumeMB, min(defaultFilesDiskMB, h.DiskBytes>>20/2))
	}
	if bad := s.gameLimits(&a, &disk, h); bad != nil {
		writeError(w, bad)
		return
	}
	if !s.domainFree(w, r, a) {
		return
	}
	switch err := s.Store.CreateApp(ctx, a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, errAppExists.Err())
		return
	case err != nil:
		s.fail(w, "create files app", err)
		return
	}
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		s.removeAppVolumes(ctx, a.ID)
		s.Store.DeleteApp(ctx, a.ID)
	}
	stored, err := s.Store.AddEgg(ctx, e.Name, entry.ID, raw, s.now())
	if err != nil {
		undo()
		s.fail(w, "store egg", err)
		return
	}
	g := store.GameServer{AppID: a.ID, EggID: stored.ID, Image: image, Startup: e.Startup, Variables: vars}
	if err := s.Store.CreateGameServer(ctx, g); err != nil {
		undo()
		s.fail(w, "create files app", err)
		return
	}
	path := gameVolumePath
	if _, err := s.createVolume(ctx, a, volumeRequest{Path: &path, LimitMB: &disk}); err != nil {
		undo()
		s.failWith(w, "create volume", err)
		return
	}
	s.Log.Info("files app created", "app", a.ID, "runtime", entry.ID, "user", loginFrom(ctx).account.ID)
	if _, err := s.startInstall(ctx, a, store.CauseInstall, store.InstallFailed); err != nil {
		s.fail(w, "start install", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": a.ID})
}

// withFilesDefaults sets the values that suit an app whose files the user
// brings. Generic eggs try to clone a repository while installing unless
// USER_UPLOAD says the files are already there, and with no address to clone
// that only fails, so it is set unless the user chose a repository or said
// otherwise.
func withFilesDefaults(e *egg.Egg, given map[string]string) map[string]string {
	out := make(map[string]string, len(given)+1)
	for k, v := range given {
		out[k] = v
	}
	has := func(env string) bool {
		for _, v := range e.Variables {
			if v.Env == env {
				return true
			}
		}
		return false
	}
	if _, set := out["USER_UPLOAD"]; !set && has("USER_UPLOAD") && has("GIT_ADDRESS") && strings.TrimSpace(out["GIT_ADDRESS"]) == "" {
		out["USER_UPLOAD"] = "1"
	}
	return out
}
