package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// A clone is a new server made from an existing one: the same egg, image,
// startup command, variables and limits, and a copy of the files. It is made
// like a new server and its files copy in the background like an install,
// under the same log and install state, so the console shows it the same way.
//
// What belongs to the original alone is left behind: its schedules, backups,
// SFTP password, domain, console history and metrics.

var (
	errCloneNoRoom = msg.Define(http.StatusUnprocessableEntity, "game.clone_no_room", "Not enough disk space to copy {server}: its files take {need}, {free} is free, and Zelie keeps 1 GB free for the apps.")
	errCloneNoDisk = msg.Define(http.StatusConflict, "game.clone_no_volume", "This server has no files to copy.")
	errCloneFailed = msg.Define(http.StatusNotFound, "game.clone_failed", "The copy did not finish, so the new server was removed: {detail}")
	errCloneCore   = msg.Define(0, "game.clone_core_failed", "The Zelie core could not copy the files. See the server log.")
)

// cloneReserve is what the core keeps free when it copies files, so a copy
// never fills the disk the servers write to.
const cloneReserve = 1 << 30

// cloneFailures remembers why the last copies failed. A copy that fails is
// removed, and the page the user is looking at would only say 404; for a few
// minutes it says why instead.
type cloneFailures struct {
	mu   sync.Mutex
	list map[string]failedClone
}

type failedClone struct {
	text string
	at   time.Time
}

const (
	cloneFailureKeep = 10 * time.Minute
	cloneFailureMax  = 16
)

func (c *cloneFailures) add(app, text string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.list == nil {
		c.list = map[string]failedClone{}
	}
	for len(c.list) >= cloneFailureMax {
		oldest := ""
		for id, f := range c.list {
			if oldest == "" || f.at.Before(c.list[oldest].at) {
				oldest = id
			}
		}
		delete(c.list, oldest)
	}
	c.list[app] = failedClone{text, now}
}

// gone is the error for a server that does not exist: the reason its copy
// failed if that is why, and plain otherwise.
func (c *cloneFailures) gone(app string, now time.Time, plain msg.Template) *msg.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.list[app]; ok && now.Sub(f.at) < cloneFailureKeep {
		return errCloneFailed.Err("detail", f.text)
	}
	delete(c.list, app)
	return plain.Err()
}

type cloneRequest struct {
	// Name is the new server's; it defaults to the original's with -copy.
	Name string `json:"name"`
}

// cloneGame makes a copy of a game server or files app and starts copying
// its files in the background.
func (s *Server) cloneGame(w http.ResponseWriter, r *http.Request) {
	src, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	var req cloneRequest
	if !decode(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = src.ID + "-copy"
	}
	if bad := checkAppID(name); bad != nil {
		writeError(w, bad)
		return
	}
	ctx := r.Context()
	a, err := s.cloneServer(ctx, src, g, name)
	if err != nil {
		s.failWith(w, "clone game server", err)
		return
	}
	out, err := s.gameOut(ctx, a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// cloneServer creates the copy and queues its files. Its error is a
// *msg.Error for the user, the core's failure, or the panel's own; on any of
// them nothing of the copy is left.
func (s *Server) cloneServer(ctx context.Context, src store.App, g store.GameServer, name string) (store.App, error) {
	switch g.InstallState {
	case store.InstallRunning:
		return store.App{}, errInstalling.Err()
	case store.InstallFailed:
		return store.App{}, errNotInstalled.Err()
	}
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return store.App{}, fmt.Errorf("load egg: %w", err)
	}
	e, err := egg.Parse(stored.Raw)
	if err != nil {
		return store.App{}, fmt.Errorf("read egg: %w", err)
	}
	vols, err := s.Store.Volumes(ctx, src.ID)
	if err != nil {
		return store.App{}, fmt.Errorf("list volumes: %w", err)
	}
	i := slices.IndexFunc(vols, func(v store.Volume) bool { return v.Path == gameVolumePath })
	if i < 0 {
		return store.App{}, errCloneNoDisk.Err()
	}
	from := vols[i]

	h, err := s.Core.Host(ctx)
	if err != nil {
		return store.App{}, s.coreError("read host", err)
	}
	a := store.App{
		ID: name, Kind: src.Kind, Source: src.Source, Image: src.Image, Port: src.Port,
		MemoryMB: src.MemoryMB, CPUs: src.CPUs, HealthPath: src.HealthPath, CreatedAt: s.now(),
	}
	disk := from.LimitMB
	if bad := s.gameLimits(&a, &disk, h); bad != nil {
		return store.App{}, bad
	}
	// The core checks again when it copies; this is to say so before a
	// server is made. The size is the last measurement, if there is one.
	if used, _ := s.sizes.get(from.Name); used > 0 && h.DiskFreeBytes-used < cloneReserve {
		return store.App{}, errCloneNoRoom.Err("server", src.ID, "need", formatMB(used>>20), "free", formatMB(h.DiskFreeBytes>>20))
	}

	// Only a game server takes ports from the pool.
	var (
		held             []store.Allocation
		srcRun, srcExtra []int
		runLen           = 1
		need             needs
		p                placement
	)
	block := blockSize(stored.Source)
	if src.IsGame() {
		if held, err = s.Store.AppAllocations(ctx, src.ID); err != nil {
			return store.App{}, fmt.Errorf("list ports: %w", err)
		}
		srcRun, srcExtra = splitPorts(held, src.Port, block)
		runLen = max(block, 1)
		need = needs{Ports: runLen + len(srcExtra), Block: block}
		if p, err = s.place(ctx, store.ThisNode, need); err != nil {
			return store.App{}, err
		}
		a.Port = p.Allocations[0].Port
	}

	switch err := s.Store.CreateApp(ctx, a); {
	case errors.Is(err, store.ErrExists):
		return store.App{}, errAppExists.Err()
	case err != nil:
		return store.App{}, fmt.Errorf("create app: %w", err)
	}
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		s.removeAppVolumes(ctx, a.ID)
		s.Store.DeleteApp(ctx, a.ID)
		s.syncSFTPVolumes(ctx)
	}
	if src.IsGame() {
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
			return store.App{}, err
		}
	}

	// Each port the original holds has a counterpart in the copy, with the
	// same job: the game port and the ports that follow it, then the others
	// in order. The variables that named a port name its counterpart.
	newPort := map[int]int{}
	for i, port := range srcRun {
		newPort[port] = p.Allocations[i].Port
	}
	for i, port := range srcExtra {
		newPort[port] = p.Allocations[runLen+i].Port
	}
	vars := maps.Clone(g.Variables)
	if vars == nil {
		vars = map[string]string{}
	}
	for _, v := range e.Variables {
		value, set := vars[v.Env]
		switch {
		case !set:
		case isPortVariable(v):
			if port, err := strconv.Atoi(value); err == nil && newPort[port] != 0 {
				vars[v.Env] = strconv.Itoa(newPort[port])
			}
		default:
			vars[v.Env] = cloneSecret(v, value)
		}
	}
	clone := store.GameServer{
		AppID: a.ID, EggID: g.EggID, Image: g.Image, Startup: g.Startup, Variables: vars,
		SteamAppID: g.SteamAppID, EULAAcceptedAt: g.EULAAcceptedAt,
	}
	if err := s.Store.CreateGameServer(ctx, clone); err != nil {
		undo()
		return store.App{}, fmt.Errorf("create game server: %w", err)
	}
	if g.SteamAutoUpdate {
		if err := s.Store.SetSteamAutoUpdate(ctx, a.ID, true); err != nil {
			undo()
			return store.App{}, fmt.Errorf("copy steam settings: %w", err)
		}
	}
	path := gameVolumePath
	to, err := s.createVolume(ctx, a, volumeRequest{Path: &path, LimitMB: &disk})
	if err != nil {
		undo()
		return store.App{}, err
	}
	// The core copies only between volumes on this list.
	s.syncSFTPVolumes(ctx)

	id, err := s.recordDeployment(ctx, store.Deployment{AppID: a.ID, Version: a.Image, Cause: store.CauseClone, Message: src.ID})
	if err == nil {
		err = s.Store.SetInstall(ctx, a.ID, store.InstallRunning, id, time.Time{})
	}
	if err != nil {
		undo()
		return store.App{}, fmt.Errorf("queue copy: %w", err)
	}
	s.Log.Info("game server cloned", "server", a.ID, "from", src.ID, "user", loginFrom(ctx).account.ID)
	job := cloneJob{app: a.ID, source: src, deployment: id, fromVolume: from.Name, toVolume: to.Name}
	s.background(a.ID, func(ctx context.Context) { s.runClone(ctx, job) })
	return a, nil
}

// splitPorts sorts the ports a server holds into the run that starts at its
// game port (just that port unless the game needs a block of them) and the
// others, by number.
func splitPorts(held []store.Allocation, primary, block int) (run, others []int) {
	if block > 1 && blockIntact(held, primary, block) {
		for i := range block {
			run = append(run, primary+i)
		}
	} else {
		run = []int{primary}
	}
	for _, a := range held {
		if !slices.Contains(run, a.Port) {
			others = append(others, a.Port)
		}
	}
	return run, others
}

type cloneJob struct {
	app        string
	source     store.App
	deployment int64
	fromVolume string
	toVolume   string
}

// runClone copies the original's files into the new server's volume and
// records how it went. A copy that fails takes the new server with it: half
// a server is of no use to anyone.
func (s *Server) runClone(ctx context.Context, c cloneJob) {
	d, err := s.Store.Deployment(ctx, c.app, c.deployment)
	if err != nil {
		s.Log.Error("load copy", "app", c.app, "id", c.deployment, "err", err)
		return
	}
	out, closeLog, err := s.openDeployLog(c.deployment)
	if err != nil {
		s.Log.Error("copy log", "err", err)
		return
	}
	defer closeLog()

	bg := context.WithoutCancel(ctx)
	set := func(state string) {
		d.State = state
		if err := s.Store.SetDeployment(bg, d, s.now()); err != nil {
			s.Log.Error("save copy", "app", c.app, "id", c.deployment, "err", err)
		}
	}
	fail := func(err error) {
		s.Log.Warn("copy failed", "app", c.app, "from", c.source.ID, "err", err)
		fmt.Fprintf(out, "\nCopy failed: %v\n", err)
		// A copy that was cancelled is a server being deleted; it needs no
		// explanation.
		if ctx.Err() == nil {
			s.cloneFailures.add(c.app, whyCloneFailed(err).Text, s.now())
		}
		if err := s.removeClone(bg, c.app); err != nil {
			// Left as a server whose install failed, for the user to delete.
			s.Log.Error("remove failed copy", "app", c.app, "err", err)
			d.Error = new(whyCloneFailed(err))
			set(store.DeployFailed)
			s.Store.SetInstall(bg, c.app, store.InstallFailed, c.deployment, time.Time{})
		}
	}
	set(store.DeployInstalling)

	fmt.Fprintf(out, "Copying the files of %s.\n", c.source.ID)
	if err := s.copyFilesOf(ctx, c, out); err != nil {
		fail(err)
		return
	}
	fmt.Fprintf(out, "Copied. %s is ready and stopped.\n", c.app)
	s.installed(bg, c.app, c.deployment, set)
}

// copyFilesOf copies the volume while holding the original's lock, so it is
// not started, stopped or restored halfway through. A running Minecraft
// server is told to write its world out first, as for a backup; any other
// running game is copied as it is.
func (s *Server) copyFilesOf(ctx context.Context, c cloneJob, out io.Writer) error {
	unlock := s.deploys.lock(c.source.ID)
	defer unlock()
	if s.gameState(ctx, c.source) == stateRunning {
		fmt.Fprintf(out, "%s is running, so its files are copied as they are.\n", c.source.ID)
		defer s.quiesceGame(ctx, c.source)()
	}
	return s.Core.CopyVolume(ctx, c.fromVolume, c.toVolume)
}

// removeClone deletes a copy that failed: its volume, ports and record.
func (s *Server) removeClone(ctx context.Context, app string) error {
	if err := s.removeAppVolumes(ctx, app); err != nil {
		return err
	}
	if err := s.removeDeployLogs(ctx, app); err != nil {
		return err
	}
	if err := s.Store.DeleteApp(ctx, app); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.gameRuns.forget(app)
	s.consoles.forget(app)
	s.consoleHist.forget(app)
	s.syncSFTPVolumes(ctx)
	return nil
}

// whyCloneFailed is the message for why the files did not copy.
func whyCloneFailed(err error) msg.Msg {
	var ce *core.Error
	if errors.As(err, &ce) {
		if ce.Status < 500 {
			return ce.Msg()
		}
		return errCloneCore.With()
	}
	return msg.Wrap(err)
}
