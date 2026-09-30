package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// A game server runs like an app in one way: a deployment starts its
// container, and the supervisor brings it back when it stops by itself.
// What differs is here. The container runs as an ordinary user, its
// console is open, "running" means the egg's done line came out, and the
// server is asked to stop in its own way before anything is killed.
//
// Whether a server should be running is the app's stopped flag, kept in the
// database: a stop the user asked for sets it, a start clears it. A server
// that is not marked stopped and has no running container has crashed, or
// the machine restarted, and either way the supervisor starts it again with
// its usual delay and limit.

// Pterodactyl's images expect to run as this user, and Wings gives it to
// every server.
const (
	gameUID = 988
	gameGID = 988
)

// gameStopGrace is how long a server gets to stop after being asked, before
// it is killed. Tests shorten it.
var gameStopGrace = 60 * time.Second

const (
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateRunning  = "running"
	stateStopping = "stopping"
	stateCrashed  = "crashed"
	stateUnknown  = "unknown"
)

var (
	errNotInstalled   = msg.Define(http.StatusConflict, "game.not_installed", "The install did not finish, so the server cannot start. Install it again first.")
	errAlreadyRunning = msg.Define(http.StatusConflict, "game.already_running", "The server is already running.")
	errBadPower       = msg.Define(http.StatusBadRequest, "game.bad_power", "The action must be start, stop, restart or kill.")
	errPrepare        = msg.Define(0, "game.prepare_failed", "The server's files could not be made ready: {detail}")
)

// gameRuns is what the panel knows about the container of each game server
// that is not in the database: whether its done line has come out, and
// whether it is being stopped.
type gameRuns struct {
	mu    sync.Mutex
	byApp map[string]gameRun
}

type gameRun struct {
	container string
	state     string // starting, running or stopping
}

func (g *gameRuns) set(app, container, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.byApp == nil {
		g.byApp = map[string]gameRun{}
	}
	g.byApp[app] = gameRun{container, state}
}

// advance moves a container from starting to running. It does nothing for a
// container that has been replaced or is being stopped.
func (g *gameRuns) advance(app, container string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r, ok := g.byApp[app]; ok && r.container == container && r.state == stateStarting {
		g.byApp[app] = gameRun{container, stateRunning}
	}
}

func (g *gameRuns) get(app string) (gameRun, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.byApp[app]
	return r, ok
}

func (g *gameRuns) forget(app string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.byApp, app)
}

// isInstallContainer tells an install's container from the server's own.
func isInstallContainer(c engine.Status) bool {
	return c.App != "" && strings.HasPrefix(c.ID, c.App+"-install-")
}

// gameParts loads what starting or stopping a server needs.
func (s *Server) gameParts(ctx context.Context, appID string) (store.GameServer, *egg.Egg, store.Node, error) {
	g, err := s.Store.GameServer(ctx, appID)
	if err != nil {
		return g, nil, store.Node{}, err
	}
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return g, nil, store.Node{}, err
	}
	e, err := egg.Parse(stored.Raw)
	if err != nil {
		return g, nil, store.Node{}, err
	}
	node, err := s.Store.Node(ctx, store.ThisNode)
	return g, e, node, err
}

// gameMemory is what the container's limit is for a server that is given
// mb. The game's own memory setting covers its heap only; the process needs
// more around it, and a smaller server needs proportionally more.
func gameMemory(mb int64) int64 {
	over := int64(105)
	switch {
	case mb < 2048:
		over = 115
	case mb < 4096:
		over = 110
	}
	return mb * over / 100 << 20
}

// gameForwards opens each of the server's ports, for TCP and for UDP, to
// the same port inside the container.
func (s *Server) gameForwards(ctx context.Context, appID string) ([]engine.Forward, error) {
	ports, err := s.Store.AppAllocations(ctx, appID)
	if err != nil {
		return nil, err
	}
	out := make([]engine.Forward, 0, 2*len(ports))
	for _, p := range ports {
		var ip netip.Addr
		if p.IP != store.AnyAddress {
			if ip, err = netip.ParseAddr(p.IP); err != nil {
				return nil, err
			}
		}
		for _, proto := range []string{"tcp", "udp"} {
			out = append(out, engine.Forward{IP: ip, Port: uint16(p.Port), Proto: proto, Target: uint16(p.Port)})
		}
	}
	return out, nil
}

// gameConfigFiles is the egg's list of files to edit, with the values
// filled in for this server, and the EULA file when it was accepted.
func gameConfigFiles(e *egg.Egg, g store.GameServer, vars map[string]string, port int, out io.Writer) []core.ConfigFile {
	var files []core.ConfigFile
	if e.HasFeature(egg.FeatureEULA) && !g.EULAAcceptedAt.IsZero() {
		// Put first so the egg's own list, which is cut at a limit, never
		// pushes it out.
		files = append(files, core.ConfigFile{Path: "eula.txt", Parser: "properties", Changes: []core.ConfigChange{{Key: "eula", Value: "true"}}})
	}
	for _, f := range e.Files {
		if len(files) == 64 {
			fmt.Fprintln(out, "The egg lists more than 64 config files; the rest are left alone.")
			break
		}
		cf := core.ConfigFile{Path: f.Path, Parser: f.Parser, Changes: []core.ConfigChange{}}
		for _, r := range f.Find {
			value := egg.Expand(r.Value, vars, port)
			if len(value) > 4<<10 {
				fmt.Fprintf(out, "The value for %s in %s is too long, so it is left alone.\n", r.Key, f.Path)
				continue
			}
			cf.Changes = append(cf.Changes, core.ConfigChange{Key: r.Key, Value: value, IfValue: r.IfValue})
		}
		files = append(files, cf)
	}
	return files
}

// startGame is a deployment of a game server: it stops what runs, makes
// the files ready and starts the container. The server counts as live once
// its container runs; whether the game is up is followed separately.
func (s *Server) startGame(ctx context.Context, app store.App, d store.Deployment, vols []store.Volume, out io.Writer, set func(string), fail func(error)) {
	g, e, node, err := s.gameParts(ctx, app.ID)
	if err != nil {
		fail(err)
		return
	}
	if g.InstallState != store.InstallDone {
		fail(errNotInstalled.Err())
		return
	}
	if len(vols) == 0 {
		fail(errors.New("the server has no volume"))
		return
	}
	set(store.DeployStarting)
	if _, err := s.stopGame(ctx, app, e, out); err != nil {
		fail(errStopOld.Err("detail", err.Error()))
		return
	}

	vars := gameVars(app, g, node)
	// A files app also gets what any app does: its own variables, PORT, and
	// its databases' variables. The egg's values win where a name is in both.
	var sealed []string
	var linked []core.LinkedVar
	if app.IsFiles() {
		var plain []string
		if plain, sealed, linked, err = s.appEnv(ctx, app); err != nil {
			fail(err)
			return
		}
		for _, kv := range plain {
			k, v, _ := strings.Cut(kv, "=")
			if _, set := vars[k]; !set {
				vars[k] = v
			}
		}
	}
	startup := egg.Expand(g.Startup, vars, app.Port)
	vars["STARTUP"] = startup
	vars["HOME"] = gameVolumePath
	fmt.Fprintf(out, "Startup command: %s\n", startup)

	// The volume may only be touched while nothing runs on it, which the
	// stop above made sure of.
	res, err := s.Core.PrepareVolume(ctx, vols[0].Name, core.PrepareRequest{
		UID: gameUID, GID: gameGID, Files: gameConfigFiles(e, g, vars, app.Port, out),
	})
	if err != nil {
		fail(errPrepare.Err("detail", err.Error()))
		return
	}
	for _, note := range res.Notes {
		fmt.Fprintln(out, note)
	}

	forwards, err := s.gameForwards(ctx, app.ID)
	if err == nil {
		err = s.Core.SetForwards(ctx, app.ID, forwards)
	}
	if err != nil {
		fail(fmt.Errorf("open the server's ports: %w", err))
		return
	}

	container := fmt.Sprintf("%s-%d", app.ID, d.ID)
	undo := func() { s.Core.Remove(context.WithoutCancel(ctx), container) }
	fmt.Fprintf(out, "Starting %s.\n", d.Image)
	pinned, err := s.Core.RunApp(ctx, engine.Spec{
		ID: container, App: app.ID, Image: d.Image, Env: envList(vars), Network: app.ID, Volumes: volumeMounts(vols),
		MemoryBytes: gameMemory(app.MemoryMB), CPUs: app.CPUs, Pids: defaultPids,
		User: &engine.IDs{UID: gameUID, GID: gameGID}, WorkDir: gameVolumePath, Stdin: true, OpenFiles: engine.MaxOpenFiles,
	}, sealed, linked...)
	if err != nil {
		undo()
		fail(err)
		return
	}
	if pinned != "" {
		d.Image = pinned
		// The first start settles which build the server runs. From here on
		// starts use it from the local store, and a registry that is down or
		// a tag that moved changes nothing until a reinstall.
		if !engine.Pinned(app.Image) && engine.Pinned(pinned) {
			app.Image = pinned
			if err := s.Store.UpdateApp(ctx, app); err != nil {
				s.Log.Error("record game server image", "server", app.ID, "err", err)
			}
		}
	}
	if app.IsFiles() {
		// Before the container counts as live, as for any app.
		if err := s.syncRoutes(ctx, map[string]string{app.ID: container}); err != nil {
			undo()
			fail(errRoute.Err("domain", app.Domain, "detail", err.Error()))
			return
		}
	}
	s.gameRuns.set(app.ID, container, stateStarting)
	s.watchGame(app, container, e)
	if err := s.Store.GoLive(ctx, d, s.now()); err != nil {
		undo()
		fail(err)
		return
	}
	fmt.Fprintln(out, "The server is starting. It counts as running once its console says it is ready.")
	if d.Cause != store.CauseRecover {
		s.crashes.reset(app.ID)
	}
	s.Log.Info("game server started", "server", app.ID, "id", d.ID, "image", d.Image)
	s.removeOldContainers(ctx, app.ID, container, out)
}

// stopGame stops the server's running containers the way its egg says, and
// reports whether there were any.
func (s *Server) stopGame(ctx context.Context, app store.App, e *egg.Egg, out io.Writer) (bool, error) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return false, err
	}
	stopped := false
	for _, c := range list {
		if c.App != app.ID || c.State != "running" || isInstallContainer(c) {
			continue
		}
		stopped = true
		s.gameRuns.set(app.ID, c.ID, stateStopping)
		if err := s.stopContainer(ctx, c.ID, e, out); err != nil {
			return stopped, err
		}
	}
	return stopped, nil
}

// stopContainer asks a game server to stop with the egg's signal or console
// command, waits for it and kills it when it takes longer than the grace
// period. A console that cannot be written to gets a SIGTERM instead, as
// after the core restarted.
func (s *Server) stopContainer(ctx context.Context, id string, e *egg.Egg, out io.Writer) error {
	// A stop that has begun is not given up when whoever asked for it moves
	// on: killing a server halfway through saving loses its world. Only a
	// kill cuts it short.
	ctx = context.WithoutCancel(ctx)
	sent := false
	if name, ok := e.StopSignal(); ok {
		if _, known := engine.ParseSignal(name); !known {
			name = "SIGTERM"
		}
		fmt.Fprintf(out, "Stopping the server with %s.\n", name)
		sent = s.Core.Signal(ctx, id, name) == nil
	} else if e.Stop != "" {
		fmt.Fprintf(out, "Stopping the server with the command %q.\n", e.Stop)
		sent = s.Core.WriteStdin(ctx, id, []byte(e.Stop+"\n")) == nil
		if !sent {
			fmt.Fprintln(out, "The console did not take the command.")
		}
	}
	if !sent {
		fmt.Fprintln(out, "Stopping the server with SIGTERM.")
		s.Core.Signal(ctx, id, "SIGTERM")
	}
	waitCtx, cancel := context.WithTimeout(ctx, gameStopGrace)
	_, err := s.Core.Wait(waitCtx, id)
	timedOut := errors.Is(waitCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil && timedOut {
		fmt.Fprintf(out, "The server did not stop within %d seconds, so it is killed.\n", int(gameStopGrace/time.Second))
	}
	// Removes the stopped task, and kills a server that is still up.
	if err := s.Core.Stop(ctx, id, 0); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

var ansi = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`) })

// watchRetry is how long the follower waits before it reads the console
// again after the core dropped it. Tests shorten it.
var watchRetry = time.Second

// resumeGames follows the servers that are running when the panel starts,
// which run on through a panel restart. Their console from the start is read
// again to see whether they are up.
func (s *Server) resumeGames(ctx context.Context) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.Log.Error("resume game servers", "err", err)
		return
	}
	list, err := s.Core.List(ctx)
	if err != nil {
		s.Log.Error("resume game servers", "err", err)
		return
	}
	for _, a := range apps {
		if !a.RunsEgg() {
			continue
		}
		for _, c := range list {
			if c.App != a.ID || c.State != "running" || isInstallContainer(c) {
				continue
			}
			_, e, _, err := s.gameParts(ctx, a.ID)
			if err != nil {
				s.Log.Error("resume game server", "server", a.ID, "err", err)
				continue
			}
			s.gameRuns.set(a.ID, c.ID, stateStarting)
			s.watchGame(a, c.ID, e)
		}
	}
}

// gameState says what a game server is doing now.
func (s *Server) gameState(ctx context.Context, a store.App) string {
	list, err := s.Core.List(ctx)
	if err != nil {
		s.Log.Error("game server state", "server", a.ID, "err", err)
		return stateUnknown
	}
	for _, c := range list {
		if c.App != a.ID || c.State != "running" || isInstallContainer(c) {
			continue
		}
		if r, ok := s.gameRuns.get(a.ID); ok && r.container == c.ID {
			return r.state
		}
		// Not seen since the panel started; the follower has not caught up.
		return stateStarting
	}
	if a.Stopped {
		return stateStopped
	}
	recent, err := s.Store.Deployments(ctx, a.ID, 1)
	if err != nil {
		s.Log.Error("game server state", "server", a.ID, "err", err)
		return stateUnknown
	}
	if len(recent) > 0 && recent[0].FinishedAt.IsZero() && recent[0].Cause != store.CauseInstall && recent[0].Cause != store.CauseReinstall && recent[0].Cause != store.CauseClone {
		return stateStarting
	}
	if _, err := s.Store.LiveDeployment(ctx, a.ID); errors.Is(err, store.ErrNotFound) {
		return stateStopped
	}
	// It ran, is meant to run and does not: the supervisor brings it back,
	// unless it has given up.
	return stateCrashed
}

type powerRequest struct {
	Action string `json:"action"`
}

// powerResult is what a power action answers with.
type powerResult struct {
	Status     int
	State      string
	Deployment int64 // set by a start or restart
}

// gamePower starts, stops, restarts or kills a game server.
func (s *Server) gamePower(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	var req powerRequest
	if !decode(w, r, &req) {
		return
	}
	res, err := s.power(r.Context(), a, g, req.Action, loginFrom(r.Context()).account.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	out := map[string]any{"state": res.State}
	if res.Deployment != 0 {
		out["deployment"] = res.Deployment
	}
	writeJSON(w, res.Status, out)
}

// internalError logs err and answers with the message that hides it.
func (s *Server) internalError(what string, err error) *msg.Error {
	s.Log.Error(what, "err", err)
	return errServer.Err()
}

// power does what the power endpoint and the console's power message ask.
func (s *Server) power(ctx context.Context, a store.App, g store.GameServer, action string, user int64) (powerResult, *msg.Error) {
	switch action {
	case "start", "restart":
		switch g.InstallState {
		case store.InstallRunning:
			return powerResult{}, errInstalling.Err()
		case store.InstallDone:
		default:
			return powerResult{}, errNotInstalled.Err()
		}
		if action == "start" {
			if state := s.gameState(ctx, a); state == stateStarting || state == stateRunning {
				return powerResult{}, errAlreadyRunning.Err()
			}
		}
		if err := s.unstopApp(ctx, a); err != nil {
			return powerResult{}, err
		}
		s.crashes.reset(a.ID)
		id, err := s.deploy(ctx, a, store.Deployment{Cause: store.CauseRestart})
		if err != nil {
			return powerResult{}, s.internalError("deploy", err)
		}
		s.Log.Info("game server power", "server", a.ID, "action", action, "user", user)
		return powerResult{Status: http.StatusAccepted, State: stateStarting, Deployment: id}, nil
	case "stop":
		// Marked first, so the supervisor does not bring it back.
		if err := s.Store.SetStopped(ctx, a.ID, true); err != nil {
			return powerResult{}, s.internalError("stop game server", err)
		}
		s.deploys.cancel(a.ID)
		state := s.gameState(ctx, a)
		_, e, _, err := s.gameParts(ctx, a.ID)
		if err != nil {
			return powerResult{}, s.internalError("stop game server", err)
		}
		if state == stateStarting || state == stateRunning {
			state = stateStopping
			s.markStopping(ctx, a.ID)
		} else if state != stateStopping {
			state = stateStopped
		}
		s.background(a.ID, func(ctx context.Context) {
			if _, err := s.stopGame(ctx, a, e, io.Discard); err != nil {
				s.Log.Error("stop game server", "server", a.ID, "err", err)
			}
		})
		s.Log.Info("game server power", "server", a.ID, "action", "stop", "user", user)
		return powerResult{Status: http.StatusAccepted, State: state}, nil
	case "kill":
		if err := s.Store.SetStopped(ctx, a.ID, true); err != nil {
			return powerResult{}, s.internalError("kill game server", err)
		}
		s.deploys.cancel(a.ID)
		if err := s.killGame(ctx, a.ID); err != nil {
			return powerResult{}, s.coreError("kill game server", err)
		}
		s.Log.Info("game server power", "server", a.ID, "action", "kill", "user", user)
		return powerResult{Status: http.StatusOK, State: stateStopped}, nil
	default:
		return powerResult{}, errBadPower.Err()
	}
}

// markStopping records that the server's running container is on its way
// down, so a view of it right away says so.
func (s *Server) markStopping(ctx context.Context, app string) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return
	}
	for _, c := range list {
		if c.App == app && c.State == "running" && !isInstallContainer(c) {
			s.gameRuns.set(app, c.ID, stateStopping)
		}
	}
}

// killGame stops the server's containers at once, without asking them.
func (s *Server) killGame(ctx context.Context, app string) error {
	list, err := s.Core.List(ctx)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.App != app || c.State != "running" || isInstallContainer(c) {
			continue
		}
		if err := s.Core.Signal(ctx, c.ID, "SIGKILL"); err != nil && !isNotFound(err) {
			return err
		}
		// Takes the stopped task away, and so its forwarded ports.
		if err := s.Core.Stop(ctx, c.ID, 5); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

// envList turns variables into the KEY=value list containers take.
func envList(vars map[string]string) []string {
	out := make([]string, 0, len(vars))
	for k, v := range vars {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}
