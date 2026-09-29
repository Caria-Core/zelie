package panel

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// A game server's files live in one volume, where the images Pterodactyl
// eggs use expect them.
const gameVolumePath = "/home/container"

const (
	minGameMemoryMB   = 128
	defaultGameMemory = 1024
	defaultGameDiskMB = 10 << 10
	maxGamePorts      = 16

	// The install gets at least this much, whatever the server is limited
	// to: installers unpack and compile things the running game never does.
	installMemoryMB = 1024
	installCPUs     = 1
	installPids     = 1024
	// installTimeout keeps an installer that hangs from holding the server
	// for ever. Tests shorten it.
	installTimeoutDefault = time.Hour
)

var installTimeout = installTimeoutDefault

var (
	errEggChoice     = msg.Define(http.StatusBadRequest, "game.egg_choice", "Choose an egg from the list, or give the address of one, not both.")
	errNoEgg         = msg.Define(http.StatusNotFound, "game.no_egg", "There is no egg called {egg} in the list.")
	errEggUnusable   = msg.Define(http.StatusUnprocessableEntity, "game.egg_unusable", "The egg could not be used: {detail}")
	errEggImage      = msg.Define(http.StatusBadRequest, "game.bad_image", "This egg does not offer the image {image}.")
	errBadVariable   = msg.Define(http.StatusBadRequest, "game.bad_variable", "{detail}")
	errLockedVar     = msg.Define(http.StatusBadRequest, "game.variable_locked", "{name} cannot be changed for this server.")
	errUnknownVar    = msg.Define(http.StatusBadRequest, "game.unknown_variable", "This egg has no variable {name}.")
	errGameMemory    = msg.Define(http.StatusBadRequest, "game.bad_memory", "Memory must be between {min} MB and {max} MB.")
	errGameCPUs      = msg.Define(http.StatusBadRequest, "game.bad_cpus", "CPUs must be more than 0 and at most {max}.")
	errGamePorts     = msg.Define(http.StatusBadRequest, "game.bad_ports", "A server takes between 1 and {max} ports.")
	errNoGame        = msg.Define(http.StatusNotFound, "game.not_found", "There is no such game server.")
	errInstalling    = msg.Define(http.StatusConflict, "game.installing", "The install is still running.")
	errStopToInstall = msg.Define(http.StatusConflict, "game.stop_to_reinstall", "Stop the server before installing it again.")
	errNotAnApp      = msg.Define(http.StatusConflict, "game.not_an_app", "This is a game server, which has its own page.")
	errInstallExit   = msg.Define(0, "game.install_failed", "The install script exited with code {code}.")
	errInstallTime   = msg.Define(0, "game.install_timeout", "The install did not finish within {minutes} minutes.")
	errInstallBackup = msg.Define(0, "game.install_backup_failed", "The backup before installing again failed, so nothing was changed: {detail}")
)

// EggFetcher downloads egg files. Tests replace it; nil uses the real
// sources.
type EggFetcher interface {
	// Catalog fetches an entry of the curated list.
	Catalog(ctx context.Context, e egg.Entry) (*egg.Egg, []byte, error)
	// URL fetches an egg from an https address.
	URL(ctx context.Context, address string) (*egg.Egg, []byte, error)
}

type liveEggs struct{}

func (liveEggs) Catalog(ctx context.Context, e egg.Entry) (*egg.Egg, []byte, error) {
	return egg.Fetch(ctx, nil, e)
}

func (liveEggs) URL(ctx context.Context, address string) (*egg.Egg, []byte, error) {
	return egg.FetchURL(ctx, nil, address)
}

func (s *Server) eggs() EggFetcher {
	if s.Eggs != nil {
		return s.Eggs
	}
	return liveEggs{}
}

// notGame answers 409 for a game server, which the endpoints that deploy
// or change an app do not handle.
func (s *Server) notGame(w http.ResponseWriter, a store.App) bool {
	if a.IsGame() {
		writeError(w, errNotAnApp.Err())
		return false
	}
	return true
}

type catalogEntryJSON struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Game string `json:"game"`
}

func (s *Server) eggCatalog(w http.ResponseWriter, r *http.Request) {
	out := make([]catalogEntryJSON, 0, len(egg.Catalog))
	for _, e := range egg.Catalog {
		out = append(out, catalogEntryJSON{ID: e.ID, Name: e.Name, Game: e.Game})
	}
	writeJSON(w, http.StatusOK, out)
}

type gameRequest struct {
	Name   string `json:"name"`
	Egg    string `json:"egg"`
	EggURL string `json:"egg_url"`
	// Image is one of the egg's images; the first by default.
	Image     string            `json:"image"`
	MemoryMB  int64             `json:"memory_mb"`
	CPUs      float64           `json:"cpus"`
	DiskMB    int64             `json:"disk_mb"`
	Variables map[string]string `json:"variables"`
	// Ports is how many ports the server takes from the pool; the first
	// is the one players connect to.
	Ports int `json:"ports"`
}

// loadEgg fetches the egg the request names. Its error is a *msg.Error.
func (s *Server) loadEgg(ctx context.Context, req gameRequest) (e *egg.Egg, source string, raw []byte, err error) {
	switch {
	case (req.Egg == "") == (req.EggURL == ""):
		return nil, "", nil, errEggChoice.Err()
	case req.Egg != "":
		entry, ok := egg.Lookup(req.Egg)
		if !ok {
			return nil, "", nil, errNoEgg.Err("egg", truncate(req.Egg, 64))
		}
		source = entry.ID
		e, raw, err = s.eggs().Catalog(ctx, entry)
	default:
		source = req.EggURL
		e, raw, err = s.eggs().URL(ctx, req.EggURL)
	}
	if err != nil {
		return nil, "", nil, errEggUnusable.Err("detail", err.Error())
	}
	return e, source, raw, nil
}

// pickImage returns the egg's image the request asks for, matching its
// reference or its label.
func pickImage(e *egg.Egg, want string) (string, *msg.Error) {
	if want == "" {
		return e.Images[0].Ref, nil
	}
	for _, img := range e.Images {
		if want == img.Ref || want == img.Label {
			return img.Ref, nil
		}
	}
	return "", errEggImage.Err("image", truncate(want, 128))
}

// gameVariables merges the request's values into the egg's defaults and
// checks every one against its rules. Only variables the egg lets users
// edit may be set. Its error is a *msg.Error.
func gameVariables(e *egg.Egg, given map[string]string) (map[string]string, *msg.Error) {
	known := make(map[string]egg.Variable, len(e.Variables))
	for _, v := range e.Variables {
		known[v.Env] = v
	}
	for _, name := range slices.Sorted(maps.Keys(given)) {
		v, ok := known[name]
		if !ok {
			return nil, errUnknownVar.Err("name", truncate(name, 64))
		}
		if !v.UserEditable {
			return nil, errLockedVar.Err("name", name)
		}
	}
	out := make(map[string]string, len(e.Variables))
	for _, v := range e.Variables {
		value, ok := given[v.Env]
		if !ok {
			value = v.Default
		}
		if err := v.Check(value); err != nil {
			return nil, errBadVariable.Err("name", v.Env, "detail", err.Error())
		}
		out[v.Env] = value
	}
	return out, nil
}

// gameVars are the variables of a game server's containers: the egg's, and
// the values the runtime sets whatever the egg declares.
func gameVars(a store.App, g store.GameServer, node store.Node) map[string]string {
	env := make(map[string]string, len(g.Variables)+len(egg.RuntimeVars))
	for k, v := range g.Variables {
		env[k] = v
	}
	tz := "UTC"
	if name := time.Local.String(); name != "" && name != "Local" {
		tz = name
	}
	env["SERVER_MEMORY"] = strconv.FormatInt(a.MemoryMB, 10)
	// The address to listen on inside the container; which host address
	// players use is decided by the port forward.
	env["SERVER_IP"] = "0.0.0.0"
	env["SERVER_PORT"] = strconv.Itoa(a.Port)
	env["TZ"] = tz
	env["STARTUP"] = g.Startup
	env["P_SERVER_LOCATION"] = node.Name
	env["P_SERVER_UUID"] = a.ID
	return env
}

// gameEnv is gameVars as a list, for a container. The install script gets
// the startup command as the egg wrote it.
func gameEnv(a store.App, g store.GameServer, node store.Node) []string {
	return envList(gameVars(a, g, node))
}

// createGame makes a game server and starts its install in the
// background.
func (s *Server) createGame(w http.ResponseWriter, r *http.Request) {
	var req gameRequest
	if !decode(w, r, &req) {
		return
	}
	if bad := checkAppID(req.Name); bad != nil {
		writeError(w, bad)
		return
	}
	ctx := r.Context()
	e, source, raw, err := s.loadEgg(ctx, req)
	if err != nil {
		s.failWith(w, "load egg", err)
		return
	}
	image, bad := pickImage(e, req.Image)
	if bad != nil {
		writeError(w, bad)
		return
	}
	vars, bad := gameVariables(e, req.Variables)
	if bad != nil {
		writeError(w, bad)
		return
	}
	if req.Ports == 0 {
		req.Ports = 1
	}
	if req.Ports < 1 || req.Ports > maxGamePorts {
		writeError(w, errGamePorts.Err("max", maxGamePorts))
		return
	}
	h, err := s.Core.Host(ctx)
	if err != nil {
		s.coreFailed(w, "read host", err)
		return
	}
	a := store.App{
		ID: req.Name, Kind: store.KindGame, Source: store.SourceImage, Image: image,
		MemoryMB: req.MemoryMB, CPUs: req.CPUs, HealthPath: "/", CreatedAt: s.now(),
	}
	disk := req.DiskMB
	if bad := s.gameLimits(&a, &disk, h); bad != nil {
		writeError(w, bad)
		return
	}

	p, err := s.place(ctx, store.ThisNode, needs{Ports: req.Ports})
	if err != nil {
		s.failWith(w, "place server", err)
		return
	}
	a.Port = p.Allocations[0].Port
	switch err := s.Store.CreateApp(ctx, a); {
	case errors.Is(err, store.ErrExists):
		writeError(w, errAppExists.Err())
		return
	case err != nil:
		s.fail(w, "create game server", err)
		return
	}
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		s.removeAppVolumes(ctx, a.ID)
		s.Store.DeleteApp(ctx, a.ID)
	}
	// Another request may take a port between place and assign; the next
	// try starts from place again.
	for try := 0; ; try++ {
		err = s.assign(ctx, a.ID, p)
		if !errors.Is(err, store.ErrInUse) || try == 2 {
			break
		}
		if p, err = s.place(ctx, store.ThisNode, needs{Ports: req.Ports}); err != nil {
			break
		}
		a.Port = p.Allocations[0].Port
		if err = s.Store.UpdateApp(ctx, a); err != nil {
			break
		}
	}
	if err != nil {
		undo()
		if errors.Is(err, store.ErrInUse) {
			err = errNoFreePort.Err()
		}
		s.failWith(w, "assign ports", err)
		return
	}
	stored, err := s.Store.AddEgg(ctx, e.Name, source, raw, s.now())
	if err != nil {
		undo()
		s.fail(w, "store egg", err)
		return
	}
	g := store.GameServer{AppID: a.ID, EggID: stored.ID, Image: image, Startup: e.Startup, Variables: vars}
	if err := s.Store.CreateGameServer(ctx, g); err != nil {
		undo()
		s.fail(w, "create game server", err)
		return
	}
	path := gameVolumePath
	if _, err := s.createVolume(ctx, a, volumeRequest{Path: &path, LimitMB: &disk}); err != nil {
		undo()
		s.failWith(w, "create volume", err)
		return
	}
	s.Log.Info("game server created", "server", a.ID, "egg", source, "user", loginFrom(ctx).account.ID)
	if _, err := s.startInstall(ctx, a, store.CauseInstall, store.InstallFailed); err != nil {
		s.fail(w, "start install", err)
		return
	}
	out, err := s.gameOut(ctx, a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// gameLimits fills in and checks the limits of a new server against the
// machine. disk is what the request asked for, in MB, or 0.
func (s *Server) gameLimits(a *store.App, disk *int64, h engine.Host) *msg.Error {
	if a.MemoryMB == 0 {
		a.MemoryMB = min(defaultGameMemory, h.MemoryBytes>>20)
	}
	if a.CPUs == 0 {
		a.CPUs = min(defaultCPUs, float64(h.CPUs))
	}
	if *disk == 0 {
		*disk = max(minVolumeMB, min(defaultGameDiskMB, h.DiskBytes>>20/2))
	}
	if a.MemoryMB < minGameMemoryMB || a.MemoryMB > h.MemoryBytes>>20 {
		return errGameMemory.Err("min", minGameMemoryMB, "max", h.MemoryBytes>>20)
	}
	if a.CPUs < 0 || a.CPUs > float64(h.CPUs) {
		return errGameCPUs.Err("max", h.CPUs)
	}
	return nil
}

type gameVariableJSON struct {
	Env         string   `json:"env"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Value       string   `json:"value"`
	Default     string   `json:"default"`
	Editable    bool     `json:"editable"`
	Rules       []string `json:"rules,omitempty"`
}

type gamePortJSON struct {
	ID   int64  `json:"id"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	// Default is the port players connect to and SERVER_PORT holds.
	Default bool `json:"default"`
}

type gameInstallJSON struct {
	State string `json:"state"`
	// Deployment is the deployment whose log holds the last install's
	// output: GET /api/apps/{app}/deployments/{id}/log.
	Deployment  int64      `json:"deployment,omitempty"`
	InstalledAt *time.Time `json:"installed_at,omitempty"`
}

type gameImageJSON struct {
	Label string `json:"label"`
	Ref   string `json:"ref"`
}

type gameJSON struct {
	ID          string             `json:"id"`
	Egg         string             `json:"egg"`
	Description string             `json:"description,omitempty"`
	Image       string             `json:"image"`
	Images      []gameImageJSON    `json:"images"`
	Startup     string             `json:"startup"`
	MemoryMB    int64              `json:"memory_mb"`
	CPUs        float64            `json:"cpus"`
	DiskMB      int64              `json:"disk_mb"`
	Ports       []gamePortJSON     `json:"ports"`
	Variables   []gameVariableJSON `json:"variables"`
	Install     gameInstallJSON    `json:"install"`
	// State is stopped, starting, running, stopping or crashed. Starting
	// lasts until the game says it is ready.
	State string `json:"state"`
	// Crashing says why Zelie stopped bringing a crashing server back.
	Crashing *msg.Msg `json:"crashing,omitempty"`
}

// gameOut describes a game server for the interface.
func (s *Server) gameOut(ctx context.Context, a store.App) (gameJSON, error) {
	g, err := s.Store.GameServer(ctx, a.ID)
	if err != nil {
		return gameJSON{}, err
	}
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return gameJSON{}, err
	}
	e, err := egg.Parse(stored.Raw)
	if err != nil {
		return gameJSON{}, err
	}
	out := gameJSON{
		ID: a.ID, Egg: e.Name, Description: e.Description, Image: g.Image, Startup: g.Startup,
		MemoryMB: a.MemoryMB, CPUs: a.CPUs,
		Images:    []gameImageJSON{},
		Ports:     []gamePortJSON{},
		Variables: []gameVariableJSON{},
		Install:   gameInstallJSON{State: g.InstallState, Deployment: g.InstallID},
		State:     s.gameState(ctx, a),
		Crashing:  s.crashes.gaveUp(a.ID),
	}
	if !g.InstalledAt.IsZero() {
		out.Install.InstalledAt = &g.InstalledAt
	}
	for _, img := range e.Images {
		out.Images = append(out.Images, gameImageJSON{Label: img.Label, Ref: img.Ref})
	}
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return out, err
	}
	for _, v := range vols {
		if v.Path == gameVolumePath {
			out.DiskMB = v.LimitMB
		}
	}
	ports, err := s.Store.AppAllocations(ctx, a.ID)
	if err != nil {
		return out, err
	}
	for _, p := range ports {
		out.Ports = append(out.Ports, gamePortJSON{ID: p.ID, IP: p.IP, Port: p.Port, Default: p.Port == a.Port})
	}
	for _, v := range e.Variables {
		name := v.Name
		if name == "" {
			name = v.Env
		}
		out.Variables = append(out.Variables, gameVariableJSON{
			Env: v.Env, Name: name, Description: v.Description, Value: g.Variables[v.Env], Default: v.Default,
			Editable: v.UserEditable, Rules: v.Rules,
		})
	}
	return out, nil
}

// gameFrom loads the game server in the path.
func (s *Server) gameFrom(w http.ResponseWriter, r *http.Request) (store.App, store.GameServer, bool) {
	a, err := s.Store.App(r.Context(), r.PathValue("app"))
	if err == nil && !a.IsGame() {
		err = store.ErrNotFound
	}
	var g store.GameServer
	if err == nil {
		g, err = s.Store.GameServer(r.Context(), a.ID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errNoGame.Err())
		return a, g, false
	case err != nil:
		s.fail(w, "load game server", err)
		return a, g, false
	}
	return a, g, true
}

func (s *Server) getGame(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	out, err := s.gameOut(r.Context(), a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// reinstallGame runs the install again. The server's files are backed up
// first and not deleted: the script decides what it overwrites.
func (s *Server) reinstallGame(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	list, err := s.Core.List(ctx)
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	for _, c := range list {
		if c.App == a.ID && c.State == "running" {
			writeError(w, errStopToInstall.Err())
			return
		}
	}
	switch began, err := s.Store.BeginInstall(ctx, a.ID); {
	case err != nil:
		s.fail(w, "begin install", err)
		return
	case !began:
		writeError(w, errInstalling.Err())
		return
	}
	// The next start pulls the image's tag again and pins what it finds,
	// so a reinstall is also how a server moves to a newer build.
	if a.Image != g.Image {
		a.Image = g.Image
		if err := s.Store.UpdateApp(ctx, a); err != nil {
			s.Store.SetInstall(context.WithoutCancel(ctx), a.ID, g.InstallState, g.InstallID, time.Time{})
			s.fail(w, "reset image", err)
			return
		}
	}
	id, err := s.startInstall(ctx, a, store.CauseReinstall, g.InstallState)
	if err != nil {
		s.Store.SetInstall(context.WithoutCancel(ctx), a.ID, g.InstallState, g.InstallID, time.Time{})
		s.fail(w, "start install", err)
		return
	}
	s.Log.Info("game server install queued again", "server", a.ID, "user", loginFrom(ctx).account.ID)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// startInstall queues an install of the server as a deployment, whose log
// is the installer's output, and returns the deployment's id. prev is the
// state the server had before this install.
func (s *Server) startInstall(ctx context.Context, a store.App, cause, prev string) (int64, error) {
	id, err := s.Store.CreateDeployment(ctx, store.Deployment{AppID: a.ID, Version: a.Image, Cause: cause}, s.now())
	if err != nil {
		return 0, err
	}
	if err := s.Store.SetInstall(ctx, a.ID, store.InstallRunning, id, time.Time{}); err != nil {
		return 0, err
	}
	s.background(a.ID, func(ctx context.Context) { s.runInstall(ctx, a.ID, id, prev) })
	return id, nil
}

// runInstall runs the egg's install container against the server's volume
// and records how it went. The server is never started from here. prev is
// the install state the server had before, for a reinstall that stops
// before touching any file.
func (s *Server) runInstall(ctx context.Context, appID string, id int64, prev string) {
	d, err := s.Store.Deployment(ctx, appID, id)
	if err != nil {
		s.Log.Error("load install", "app", appID, "id", id, "err", err)
		return
	}
	out, closeLog, err := s.openDeployLog(id)
	if err != nil {
		s.Log.Error("install log", "err", err)
		return
	}
	defer closeLog()

	// Writes after a cancel, as when the server is deleted, still record
	// how far it got.
	bg := context.WithoutCancel(ctx)
	set := func(state string) {
		d.State = state
		if err := s.Store.SetDeployment(bg, d, s.now()); err != nil {
			s.Log.Error("save install", "app", appID, "id", id, "err", err)
		}
	}
	fail := func(err error, state string) {
		fmt.Fprintf(out, "\nInstall failed: %v\n", err)
		d.Error = new(msg.Wrap(err))
		set(store.DeployFailed)
		if err := s.Store.SetInstall(bg, appID, state, id, time.Time{}); err != nil {
			s.Log.Error("save install state", "app", appID, "err", err)
		}
		s.Log.Warn("install failed", "app", appID, "id", id, "err", err)
	}

	a, err := s.Store.App(ctx, appID)
	if err != nil {
		fail(err, store.InstallFailed)
		return
	}
	g, err := s.Store.GameServer(ctx, appID)
	if err != nil {
		fail(err, store.InstallFailed)
		return
	}
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		fail(err, store.InstallFailed)
		return
	}
	e, err := egg.Parse(stored.Raw)
	if err != nil {
		fail(err, store.InstallFailed)
		return
	}
	node, err := s.Store.Node(ctx, store.ThisNode)
	if err != nil {
		fail(err, store.InstallFailed)
		return
	}
	vols, err := s.Store.Volumes(ctx, appID)
	if err == nil && len(vols) == 0 {
		err = errors.New("the server has no volume")
	}
	if err != nil {
		fail(err, store.InstallFailed)
		return
	}
	set(store.DeployInstalling)

	if d.Cause == store.CauseReinstall {
		fmt.Fprintln(out, "Backing up the server's files before installing again.")
		if !s.backupBusy.take(appID) {
			// The files are as they were, so the server stays as it was.
			fail(errBackupBusy.Err(), prev)
			return
		}
		b, err := s.makeBackup(ctx, a, store.BackupReinstall)
		s.backupBusy.done(appID)
		if err != nil {
			fail(errInstallBackup.Err("detail", backupFailure(err).Text), prev)
			return
		}
		fmt.Fprintf(out, "Backed up (%d bytes). It is on the Backups tab.\n", b.Bytes)
	}

	if strings.TrimSpace(e.Install.Script) == "" || e.Install.Image == "" {
		fmt.Fprintf(out, "%s has no install script, so there is nothing to install.\n", e.Name)
		s.installed(bg, appID, id, set)
		return
	}
	entrypoint := e.Install.Entrypoint
	if entrypoint == "" {
		entrypoint = "bash"
	}
	container := fmt.Sprintf("%s-install-%d", appID, id)
	// A container of this name left by an install that was cut off.
	s.Core.Remove(bg, container)
	fmt.Fprintf(out, "Installing %s with %s.\n", e.Name, e.Install.Image)
	pinned, err := s.Core.RunInstall(ctx, core.InstallRequest{
		ID: container, App: appID, Image: e.Install.Image, Entrypoint: entrypoint,
		Script: strings.ReplaceAll(e.Install.Script, "\r\n", "\n"),
		Env:    gameEnv(a, g, node), Volume: vols[0].Name, Network: appID,
		MemoryBytes: max(a.MemoryMB, installMemoryMB) << 20, CPUs: max(a.CPUs, installCPUs), Pids: installPids,
	})
	if err != nil {
		fail(fmt.Errorf("start the installer: %w", err), store.InstallFailed)
		return
	}
	defer s.Core.Remove(bg, container)
	// Recorded like an app's image: which build ran the install.
	if pinned != "" {
		d.Image = pinned
		set(store.DeployInstalling)
	}

	logCtx, stopLogs := context.WithCancel(ctx)
	logsDone := make(chan struct{})
	go func() {
		s.Core.Logs(logCtx, container, true, 0, out)
		close(logsDone)
	}()
	waitCtx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	code, err := s.Core.Wait(waitCtx, container)
	if err == nil {
		// The last of the output may still be on its way.
		time.Sleep(testSettle)
	}
	stopLogs()
	<-logsDone
	switch {
	case errors.Is(waitCtx.Err(), context.DeadlineExceeded):
		fail(errInstallTime.Err("minutes", int(installTimeout/time.Minute)), store.InstallFailed)
	case err != nil:
		fail(err, store.InstallFailed)
	case code != 0:
		fail(errInstallExit.Err("code", code), store.InstallFailed)
	default:
		s.installed(bg, appID, id, set)
	}
}

// installed records a finished install.
func (s *Server) installed(ctx context.Context, appID string, id int64, set func(string)) {
	set(store.DeployInstalled)
	if err := s.Store.SetInstall(ctx, appID, store.InstallDone, id, s.now()); err != nil {
		s.Log.Error("save install state", "app", appID, "err", err)
	}
	s.Log.Info("install finished", "app", appID, "id", id)
}

// removeStaleInstalls removes install containers left behind when the panel
// stopped in the middle of one. The installs themselves are marked failed
// on start.
func (s *Server) removeStaleInstalls(ctx context.Context) {
	list, err := s.Core.List(ctx)
	if err != nil {
		s.Log.Error("list containers", "err", err)
		return
	}
	for _, c := range list {
		if c.App != "" && strings.HasPrefix(c.ID, c.App+"-install-") {
			if err := s.Core.Remove(ctx, c.ID); err != nil && !isNotFound(err) {
				s.Log.Error("remove stale install", "id", c.ID, "err", err)
			}
		}
	}
}
