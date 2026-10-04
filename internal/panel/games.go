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
	"github.com/Caria-Core/zelie/internal/players"
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
	errEULARequired  = msg.Define(http.StatusBadRequest, "game.eula_required", "This game needs you to accept its EULA first.")
	errNoEULA        = msg.Define(http.StatusConflict, "game.no_eula", "This game has no EULA to accept.")
	errBadEggKind    = msg.Define(http.StatusBadRequest, "game.bad_egg_kind", "The kind must be empty or runtime.")
	errRuntimeEgg    = msg.Define(http.StatusBadRequest, "game.runtime_egg", "{egg} runs your own files. Create it with New app, Files.")
	errBadStartup    = msg.Define(http.StatusBadRequest, "game.bad_startup", "The startup command must be at most {max} characters, without control characters.")
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
	// Sizes the game needs, when it needs more than the usual: the
	// interface offers them for a new server.
	MemoryMB int64 `json:"memory_mb,omitempty"`
	DiskMB   int64 `json:"disk_mb,omitempty"`
	Ports    int   `json:"ports,omitempty"`
	// Block is how many ports in a row from the game port the game needs.
	Block int `json:"block,omitempty"`
}

// eggCatalog lists the games, or with ?kind=runtime the generic eggs that run
// a user's own files. The two are offered in different places.
func (s *Server) eggCatalog(w http.ResponseWriter, r *http.Request) {
	kind := egg.KindGame
	switch r.URL.Query().Get("kind") {
	case "":
	case string(egg.KindRuntime):
		kind = egg.KindRuntime
	default:
		writeError(w, errBadEggKind.Err())
		return
	}
	entries := egg.OfKind(kind)
	out := make([]catalogEntryJSON, 0, len(entries))
	for _, e := range entries {
		out = append(out, catalogEntryJSON{ID: e.ID, Name: e.Name, Game: e.Game, MemoryMB: e.MemoryMB, DiskMB: e.DiskMB, Ports: e.Ports, Block: e.Block})
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
	// Allocations picks which pool ports fill which roles. Roles it does
	// not name take the lowest free ports, as when it is left out.
	Allocations *portPick `json:"allocations"`
	// AcceptEULA is the administrator's answer to the EULA that eggs with
	// the "eula" feature (Minecraft) ask for. Zelie writes it into the
	// server's files before each start.
	AcceptEULA bool `json:"accept_eula"`

	// runtime is set when the egg is read to make a files app, which may
	// use the generic eggs.
	runtime bool
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
		if entry.Kind == egg.KindRuntime && !req.runtime {
			return nil, "", nil, errRuntimeEgg.Err("egg", entry.Name)
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

// chooseImage is the image a request asks for. The egg's images, by
// reference or label, are open to everyone; an administrator may name any
// other. current is what the server has now, which empty asks to keep.
func chooseImage(ctx context.Context, e *egg.Egg, want, current string) (string, *msg.Error) {
	if want == "" && current != "" {
		return current, nil
	}
	image, bad := pickImage(e, want)
	if bad == nil || !loginFrom(ctx).account.Admin {
		return image, bad
	}
	if bad := checkImageRef(want); bad != nil {
		return "", bad
	}
	return want, nil
}

// mayUnlock says whether the request may change what the egg locks. The
// locks are there for the customers a host gives accounts to, not for the
// administrators, and not for a files app, whose owner is the one who would
// have set them.
func mayUnlock(ctx context.Context, a store.App) bool {
	return a.IsFiles() || loginFrom(ctx).account.Admin
}

const maxStartupBytes = 4096

// checkStartup validates a startup command an administrator typed. Blank
// means the egg's own, which the caller resolves.
func checkStartup(cmd string) *msg.Error {
	if len(cmd) > maxStartupBytes {
		return errBadStartup.Err("max", maxStartupBytes)
	}
	for _, c := range cmd {
		if c < ' ' && c != '\n' && c != '\t' || c == 0x7f {
			return errBadStartup.Err("max", maxStartupBytes)
		}
	}
	return nil
}

// checkImageRef validates an image an administrator typed, as an app's
// image is.
func checkImageRef(ref string) *msg.Error {
	switch {
	case ref == "" || len(ref) > 255 || strings.ContainsAny(ref, " \t\n"):
		return errNoImage.Err()
	case strings.HasPrefix(ref, engine.LocalImages):
		return errReservedImage.Err()
	case !validImage(ref):
		return errBadImage.Err("image", truncate(ref, 128))
	}
	return nil
}

// maxVariableBytes keeps a variable's value to something a container's
// environment takes.
const maxVariableBytes = 8 << 10

// gameVariables merges the request's values into the server's current ones,
// or into the egg's defaults where base has none, and checks each value that
// is new against its rules. Only variables the egg lets users edit may be
// set, unless unlock lets every one of them change, as a files app does: its
// owner is the one who would have locked them. Its error is a *msg.Error.
func gameVariables(e *egg.Egg, base, given map[string]string, unlock bool) (map[string]string, *msg.Error) {
	known := make(map[string]egg.Variable, len(e.Variables))
	for _, v := range e.Variables {
		known[v.Env] = v
	}
	for _, name := range slices.Sorted(maps.Keys(given)) {
		v, ok := known[name]
		if !ok {
			return nil, errUnknownVar.Err("name", truncate(name, 64))
		}
		if !v.UserEditable && !unlock {
			return nil, errLockedVar.Err("name", name)
		}
	}
	out := make(map[string]string, len(e.Variables))
	for _, v := range e.Variables {
		value, isNew := given[v.Env]
		if !isNew {
			var ok bool
			if value, ok = base[v.Env]; ok {
				out[v.Env] = value
				continue
			}
			value = initialValue(v)
		}
		switch {
		case strings.ContainsRune(value, 0):
			return nil, errBadVariable.Err("name", v.Env, "detail", v.Env+" must not hold a null character.")
		case len(value) > maxVariableBytes:
			return nil, errBadVariable.Err("name", v.Env, "detail", fmt.Sprintf("%s must be at most %d KiB.", v.Env, maxVariableBytes>>10))
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
	needsEULA := e.HasFeature(egg.FeatureEULA)
	if needsEULA && !req.AcceptEULA {
		writeError(w, errEULARequired.Err())
		return
	}
	image, bad := chooseImage(ctx, e, req.Image, "")
	if bad != nil {
		writeError(w, bad)
		return
	}
	vars, bad := gameVariables(e, nil, req.Variables, mayUnlock(ctx, store.App{}))
	if bad != nil {
		writeError(w, bad)
		return
	}
	wish, bad := wishedPorts(e, req.Allocations)
	if bad != nil {
		writeError(w, bad)
		return
	}
	req.Ports = recommendedPorts(req)
	if req.Ports == 0 {
		req.Ports = 1
	}
	if req.Ports < 1 || req.Ports > maxGamePorts {
		writeError(w, errGamePorts.Err("max", maxGamePorts))
		return
	}
	block := blockSize(source)
	req.Ports = max(req.Ports, wish.count(block))
	if req.Ports > maxGamePorts {
		writeError(w, errGamePorts.Err("max", maxGamePorts))
		return
	}
	h, err := s.Core.Host(ctx)
	if err != nil {
		s.coreFailed(w, "read host", err)
		return
	}
	req.MemoryMB, req.DiskMB = recommendedSize(req, h)
	a := store.App{
		ID: req.Name, Kind: store.KindGame, Source: store.SourceImage, Image: image,
		MemoryMB: req.MemoryMB, CPUs: req.CPUs, HealthPath: "/", CreatedAt: s.now(),
	}
	disk := req.DiskMB
	if bad := s.gameLimits(&a, &disk, h); bad != nil {
		writeError(w, bad)
		return
	}

	need := needs{Ports: req.Ports, Chosen: wish.ids, Primary: wish.primary, Block: block}
	p, err := s.place(ctx, store.ThisNode, need)
	if err != nil {
		s.failWith(w, "place server", err)
		return
	}
	primary, _, _ := wish.split(p.Allocations, block)
	a.Port = primary.Port
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
		if p, err = s.place(ctx, store.ThisNode, need); err != nil {
			break
		}
		primary, _, _ = wish.split(p.Allocations, block)
		a.Port = primary.Port
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
	_, chosen, auto := wish.split(p.Allocations, block)
	given := maps.Clone(req.Variables)
	if given == nil {
		given = map[string]string{}
	}
	for env, port := range chosen {
		vars[env] = strconv.Itoa(port)
		given[env] = vars[env]
	}
	assignPorts(e, vars, given, auto)
	g := store.GameServer{AppID: a.ID, EggID: stored.ID, Image: image, Startup: e.Startup, Variables: vars, SteamAppID: steamAppID(e, vars)}
	if needsEULA {
		g.EULAAcceptedAt = s.now()
	}
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
	s.syncSFTPVolumes(ctx)
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
	Env         string `json:"env"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Value       string `json:"value"`
	Default     string `json:"default"`
	Editable    bool   `json:"editable"`
	// Locked is set when the egg keeps users from changing the variable,
	// which an administrator can still do.
	Locked bool `json:"locked,omitempty"`
	// Port is set for a variable that holds one of the server's ports,
	// which are chosen on the network page.
	Port  bool     `json:"port,omitempty"`
	Rules []string `json:"rules,omitempty"`
}

type gamePortJSON struct {
	ID   int64  `json:"id"`
	IP   string `json:"ip"`
	Port int    `json:"port"`
	// Address is the host players connect to for this port.
	Address string `json:"address"`
	// Default is the port players connect to and SERVER_PORT holds.
	Default bool `json:"default"`
	// UsedBy are the egg's variables that hold this port, such as the
	// query port.
	UsedBy []portUseJSON `json:"used_by"`
	// Offset is how far a port after the game port is from it, for a game
	// that needs a run of ports. Those ports follow the game port and are
	// not chosen on their own.
	Offset int `json:"offset,omitempty"`
}

type gameInstallJSON struct {
	State string `json:"state"`
	// CloneOf names the server whose files are being copied, while the
	// "install" is a copy.
	CloneOf string `json:"clone_of,omitempty"`
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
	ID          string          `json:"id"`
	Egg         string          `json:"egg"`
	Description string          `json:"description,omitempty"`
	Image       string          `json:"image"`
	Images      []gameImageJSON `json:"images"`
	Startup     string          `json:"startup"`
	// EggStartup is the egg's own command, which an empty one restores.
	EggStartup string `json:"egg_startup"`
	// StartupPreview is the startup command with the saved values filled in,
	// as the next start will run it.
	StartupPreview string         `json:"startup_preview"`
	MemoryMB       int64          `json:"memory_mb"`
	CPUs           float64        `json:"cpus"`
	DiskMB         int64          `json:"disk_mb"`
	Ports          []gamePortJSON `json:"ports"`
	// Block is how many ports in a row from the game port the game needs,
	// when it needs more than one. BlockBroken says the server does not
	// hold them all: it was made before the panel kept them together, or
	// the pool changed.
	Block       int                `json:"block,omitempty"`
	BlockBroken bool               `json:"block_broken,omitempty"`
	Variables   []gameVariableJSON `json:"variables"`
	Features    []string           `json:"features"`
	// Players is what the Players tab can show: "full", "list" or "". The
	// game is the family of the parser behind "full".
	Players     string `json:"players"`
	PlayersGame string `json:"players_game"`
	// EULANeeded is set when the egg asks for a EULA that has not been
	// accepted, so the server will not start.
	EULANeeded bool            `json:"eula_needed,omitempty"`
	Install    gameInstallJSON `json:"install"`
	// State is stopped, starting, running, stopping or crashed. Starting
	// lasts until the game says it is ready.
	State string `json:"state"`
	// Crashing says why Zelie stopped bringing a crashing server back.
	Crashing *msg.Msg `json:"crashing,omitempty"`
	// Steam is set for games installed with SteamCMD.
	Steam *gameSteamJSON `json:"steam,omitempty"`
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
		ID: a.ID, Egg: e.Name, Description: e.Description, Image: g.Image, Startup: g.Startup, EggStartup: e.Startup,
		MemoryMB: a.MemoryMB, CPUs: a.CPUs,
		Images:      []gameImageJSON{},
		Ports:       []gamePortJSON{},
		Variables:   []gameVariableJSON{},
		Features:    featuresOut(e),
		Players:     players.Capability(stored.Source),
		PlayersGame: players.Family(stored.Source),
		Install:     gameInstallJSON{State: g.InstallState, Deployment: g.InstallID},
		State:       s.gameState(ctx, a),
		Crashing:    s.crashes.gaveUp(a.ID),
	}
	out.EULANeeded = e.HasFeature(egg.FeatureEULA) && g.EULAAcceptedAt.IsZero()
	if g.InstallState == store.InstallRunning {
		if d, err := s.Store.Deployment(ctx, a.ID, g.InstallID); err == nil && d.Cause == store.CauseClone {
			out.Install.CloneOf = d.Message
		}
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
	var node string
	if len(ports) > 0 {
		if n, err := s.Store.Node(ctx, store.ThisNode); err == nil {
			node = s.nodeOut(ctx, n).Address
		}
	}
	block := blockSize(stored.Source)
	if block > 1 {
		out.Block = block
		out.BlockBroken = !blockIntact(ports, a.Port, block)
	}
	for _, p := range ports {
		host := node
		if p.IP != store.AnyAddress {
			host = p.IP
		}
		gp := gamePortJSON{ID: p.ID, IP: p.IP, Port: p.Port, Address: host, Default: p.Port == a.Port, UsedBy: portUses(e, g.Variables, p.Port)}
		if out.Block > 1 && !out.BlockBroken && p.Port > a.Port && p.Port < a.Port+block {
			gp.Offset = p.Port - a.Port
		}
		out.Ports = append(out.Ports, gp)
	}
	out.Variables = variablesOut(e, g.Variables, mayUnlock(ctx, a))
	// Without the node, only the placeholder for its name stays as written.
	this, _ := s.Store.Node(ctx, store.ThisNode)
	out.StartupPreview = egg.Expand(g.Startup, gameVars(a, g, this), a.Port)
	out.Steam = s.steamOut(ctx, a, g, e)
	return out, nil
}

func featuresOut(e *egg.Egg) []string {
	return append([]string{}, e.Features...)
}

// variablesOut lists the egg's variables with the values a server has, or
// the defaults when values is nil. unlock shows every variable as editable.
func variablesOut(e *egg.Egg, values map[string]string, unlock bool) []gameVariableJSON {
	out := make([]gameVariableJSON, 0, len(e.Variables))
	for _, v := range e.Variables {
		name := v.Name
		if name == "" {
			name = v.Env
		}
		value, ok := values[v.Env]
		if !ok {
			value = v.Default
		}
		out = append(out, gameVariableJSON{
			Env: v.Env, Name: name, Description: v.Description, Value: value, Default: v.Default,
			Editable: v.UserEditable || unlock, Locked: !v.UserEditable, Port: isPortVariable(v), Rules: v.Rules,
		})
	}
	return out
}

type eggPreviewRequest struct {
	Egg    string `json:"egg"`
	EggURL string `json:"egg_url"`
}

type eggPreviewJSON struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Images      []gameImageJSON    `json:"images"`
	Startup     string             `json:"startup"`
	Variables   []gameVariableJSON `json:"variables"`
	Features    []string           `json:"features"`
	// PortVariables are the variables that take an extra port each when the
	// server has more than one.
	PortVariables []portUseJSON `json:"port_variables"`
}

// eggPreview reads an egg without making a server, so the interface can ask
// for its image and settings before creating one.
func (s *Server) eggPreview(w http.ResponseWriter, r *http.Request) {
	var req eggPreviewRequest
	if !decode(w, r, &req) {
		return
	}
	e, _, _, err := s.loadEgg(r.Context(), gameRequest{Egg: req.Egg, EggURL: req.EggURL, runtime: isRuntimeEgg(req.Egg)})
	if err != nil {
		s.failWith(w, "load egg", err)
		return
	}
	runtime := isRuntimeEgg(req.Egg)
	var values map[string]string
	if runtime {
		// What a new files app starts with, so the form shows it.
		values = withFilesDefaults(e, nil)
	}
	out := eggPreviewJSON{Name: e.Name, Description: e.Description, Startup: e.Startup, Images: []gameImageJSON{}, Variables: variablesOut(e, values, runtime || loginFrom(r.Context()).account.Admin), Features: featuresOut(e), PortVariables: portVariableNames(e)}
	for _, img := range e.Images {
		out.Images = append(out.Images, gameImageJSON{Label: img.Label, Ref: img.Ref})
	}
	writeJSON(w, http.StatusOK, out)
}

// isRuntimeEgg tells a catalog id that names a generic egg.
func isRuntimeEgg(id string) bool {
	e, ok := egg.Lookup(id)
	return ok && e.Kind == egg.KindRuntime
}

// gameFrom loads the game server in the path. A files app answers as well:
// it is started from an egg the same way, and the console, files and startup
// pages of both are these endpoints.
func (s *Server) gameFrom(w http.ResponseWriter, r *http.Request) (store.App, store.GameServer, bool) {
	a, err := s.Store.App(r.Context(), r.PathValue("app"))
	if err == nil && !a.RunsEgg() {
		err = store.ErrNotFound
	}
	var g store.GameServer
	if err == nil {
		g, err = s.Store.GameServer(r.Context(), a.ID)
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, s.cloneFailures.gone(r.PathValue("app"), s.now(), errNoGame))
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

// managesGame guards what changes a game server's files and startup
// settings. For now that is an administrator's work; once servers have their
// own users, this is where their permissions are checked.
func (s *Server) managesGame(next http.HandlerFunc) http.HandlerFunc {
	return s.adminOnly(next)
}

// managesGameConfirmed is managesGame for what also needs a recent second
// step.
func (s *Server) managesGameConfirmed(next http.HandlerFunc) http.HandlerFunc {
	return s.confirmed(requireAdmin(next))
}

type gameSettingsRequest struct {
	// Variables holds the values to change; the others stay as they are.
	Variables map[string]string `json:"variables"`
	// Image is one of the egg's images, or for an administrator any image;
	// empty keeps the current one.
	Image string `json:"image"`
	// Startup replaces the egg's startup command; empty goes back to the
	// egg's. Only administrators set it, and nil keeps the current one.
	Startup *string `json:"startup"`
}

// updateGameSettings changes the image and the editable variables a server
// starts with. A running server keeps what it started with until it is
// started again.
func (s *Server) updateGameSettings(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	var req gameSettingsRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	if g.InstallState == store.InstallRunning {
		writeError(w, errInstalling.Err())
		return
	}
	_, e, _, err := s.gameParts(ctx, a.ID)
	if err != nil {
		s.fail(w, "load egg", err)
		return
	}
	image, bad := chooseImage(ctx, e, req.Image, g.Image)
	if bad != nil {
		writeError(w, bad)
		return
	}
	startup := g.Startup
	if req.Startup != nil {
		if !loginFrom(ctx).account.Admin {
			writeError(w, errAdminOnly.Err())
			return
		}
		startup = strings.TrimSpace(*req.Startup)
		if bad := checkStartup(startup); bad != nil {
			writeError(w, bad)
			return
		}
		if startup == "" {
			startup = e.Startup
		}
	}
	vars, bad := gameVariables(e, g.Variables, req.Variables, mayUnlock(ctx, a))
	if bad != nil {
		writeError(w, bad)
		return
	}
	if err := s.Store.SetGameSettings(ctx, a.ID, image, startup, vars); err != nil {
		s.fail(w, "save game settings", err)
		return
	}
	// The server starts from the app's image, which is pinned to a build
	// once it has run. A new choice starts over from the tag.
	if image != g.Image {
		a.Image = image
		if err := s.Store.UpdateApp(ctx, a); err != nil {
			s.fail(w, "save game image", err)
			return
		}
	}
	var changed []string
	for _, name := range slices.Sorted(maps.Keys(req.Variables)) {
		if vars[name] != g.Variables[name] {
			changed = append(changed, name)
		}
	}
	s.Log.Info("game settings changed", "server", a.ID, "user", loginFrom(ctx).account.ID, "image", image, "startup_changed", startup != g.Startup, "variables", changed)
	out, err := s.gameOut(ctx, a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type gameResourcesRequest struct {
	// Zero keeps what the server has now.
	MemoryMB int64   `json:"memory_mb"`
	CPUs     float64 `json:"cpus"`
	DiskMB   int64   `json:"disk_mb"`
}

// updateGameResources changes a server's memory, CPUs and disk. Memory and
// CPUs are read when the server starts, so a running one keeps its limits
// until it is started again; the disk limit is checked from the next
// measurement on.
func (s *Server) updateGameResources(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	if a.IsFiles() {
		writeError(w, errNotForFiles.Err())
		return
	}
	var req gameResourcesRequest
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	a, err := s.setGameResources(ctx, a, req)
	if err != nil {
		s.failWith(w, "change game resources", err)
		return
	}
	out, err := s.gameOut(ctx, a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setGameResources applies a resources request to the server and returns
// the app as saved. Its error is a *msg.Error for the user, the core's
// failure, or the panel's own.
func (s *Server) setGameResources(ctx context.Context, a store.App, req gameResourcesRequest) (store.App, error) {
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return a, fmt.Errorf("list volumes: %w", err)
	}
	i := slices.IndexFunc(vols, func(v store.Volume) bool { return v.Path == gameVolumePath })
	if i < 0 {
		return a, errors.New("the server has no volume")
	}
	vol := vols[i]
	if req.MemoryMB != 0 {
		a.MemoryMB = req.MemoryMB
	}
	if req.CPUs != 0 {
		a.CPUs = req.CPUs
	}
	if req.DiskMB != 0 {
		vol.LimitMB = req.DiskMB
	}
	h, err := s.Core.Host(ctx)
	if err != nil {
		return a, s.coreError("read host", err)
	}
	disk := vol.LimitMB
	if bad := s.gameLimits(&a, &disk, h); bad != nil {
		return a, bad
	}
	if err := s.checkVolume(ctx, vol); err != nil {
		return a, err
	}
	if err := s.Store.UpdateApp(ctx, a); err != nil {
		return a, fmt.Errorf("save game resources: %w", err)
	}
	if err := s.Store.UpdateVolume(ctx, vol); err != nil {
		return a, fmt.Errorf("update volume: %w", err)
	}
	s.Log.Info("game resources changed", "server", a.ID, "user", loginFrom(ctx).account.ID, "memory_mb", a.MemoryMB, "cpus", a.CPUs, "disk_mb", vol.LimitMB)
	return a, nil
}

// acceptEULA records the administrator's acceptance of the game's EULA for
// a server made before it was asked for, or one that refused to start
// without it.
func (s *Server) acceptEULA(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	_, e, _, err := s.gameParts(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "load egg", err)
		return
	}
	if !e.HasFeature(egg.FeatureEULA) {
		writeError(w, errNoEULA.Err())
		return
	}
	if g.EULAAcceptedAt.IsZero() {
		if err := s.Store.AcceptEULA(r.Context(), a.ID, s.now()); err != nil {
			s.fail(w, "accept eula", err)
			return
		}
		s.Log.Info("game eula accepted", "server", a.ID, "user", loginFrom(r.Context()).account.ID)
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
	id, err := s.reinstall(r.Context(), a, g)
	if err != nil {
		s.failWith(w, "reinstall game server", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// reinstall queues the install again and returns its deployment's id. Its
// error is a *msg.Error for the user, the core's failure, or the panel's own.
func (s *Server) reinstall(ctx context.Context, a store.App, g store.GameServer) (int64, error) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return 0, s.coreError("list containers", err)
	}
	for _, c := range list {
		if c.App == a.ID && c.State == "running" {
			return 0, errStopToInstall.Err()
		}
	}
	switch began, err := s.Store.BeginInstall(ctx, a.ID); {
	case err != nil:
		return 0, fmt.Errorf("begin install: %w", err)
	case !began:
		return 0, errInstalling.Err()
	}
	// The next start pulls the image's tag again and pins what it finds,
	// so a reinstall is also how a server moves to a newer build.
	if a.Image != g.Image {
		a.Image = g.Image
		if err := s.Store.UpdateApp(ctx, a); err != nil {
			s.Store.SetInstall(context.WithoutCancel(ctx), a.ID, g.InstallState, g.InstallID, time.Time{})
			return 0, fmt.Errorf("reset image: %w", err)
		}
	}
	id, err := s.startInstall(ctx, a, store.CauseReinstall, g.InstallState)
	if err != nil {
		s.Store.SetInstall(context.WithoutCancel(ctx), a.ID, g.InstallState, g.InstallID, time.Time{})
		return 0, fmt.Errorf("start install: %w", err)
	}
	s.Log.Info("game server install queued again", "server", a.ID, "user", loginFrom(ctx).account.ID)
	return id, nil
}

// startInstall queues an install of the server as a deployment, whose log
// is the installer's output, and returns the deployment's id. prev is the
// state the server had before this install.
func (s *Server) startInstall(ctx context.Context, a store.App, cause, prev string) (int64, error) {
	id, err := s.recordDeployment(ctx, store.Deployment{AppID: a.ID, Version: a.Image, Cause: cause})
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
