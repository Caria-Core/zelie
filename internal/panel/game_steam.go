package panel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/query"
	"github.com/Caria-Core/zelie/internal/steam"
	"github.com/Caria-Core/zelie/internal/store"
)

// Steam games get their files from SteamCMD, which the egg's install script
// runs, and most eggs' images run again on every start. Zelie asks Steam
// itself, once an hour and only while some server is a Steam game, which
// build is the latest, and compares it with the build in the server's own
// files. Nothing here talks to a third party.

const (
	// steamRound is how often the panel looks at its Steam servers. The
	// build is only asked for once an hour; the rest of the round decides
	// whether an empty server can be updated now.
	steamRound      = 5 * time.Minute
	steamCheckEvery = time.Hour
	// steamCheckTimeout covers SteamCMD updating itself and logging in.
	steamCheckTimeout = 10 * time.Minute

	// The container that asks Steam has a name Zelie keeps for itself, and a
	// network of its own that only reaches the internet.
	steamContainer = "zelie-steamcheck"
	steamLogBytes  = 4 << 20
)

var (
	errNotSteam      = msg.Define(http.StatusConflict, "game.not_steam", "This game is not installed with SteamCMD.")
	errNoSteamUpdate = msg.Define(http.StatusConflict, "game.no_update", "There is no newer build to update to.")
	errStopToUpdate  = msg.Define(http.StatusConflict, "game.stop_to_update", "This game only updates when it is installed again. Stop the server first.")
)

// steamTracker holds what the panel last learned from Steam.
type steamTracker struct {
	mu sync.Mutex
	// latest is the newest build of each branch, by app and then by branch.
	latest    map[int64]map[string]string
	checkedAt time.Time // when the builds in latest were read
	triedAt   time.Time // when Steam was last asked, whether or not it answered
	// updated is the build each server was last updated to on its own, so a
	// build the server does not take is not tried again every few minutes.
	updated map[string]string
}

// build is the latest build of the app on a branch, empty when Steam did not
// list the branch.
func (t *steamTracker) build(app int64, branch string) (string, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if branch == "" {
		branch = steam.PublicBranch
	}
	return t.latest[app][branch], t.checkedAt
}

// srcdsAppID is the variable Pterodactyl's Steam eggs keep the app in.
const srcdsAppID = "SRCDS_APPID"

var appUpdate = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`\+app_update\s+"?(\d+)`) })

// steamAppID finds the Steam app an egg installs: its SRCDS_APPID variable,
// or a literal id in the install script's app_update. It returns 0 for a game
// that does not come from Steam.
func steamAppID(e *egg.Egg, vars map[string]string) int64 {
	if id, err := strconv.ParseInt(strings.TrimSpace(vars[srcdsAppID]), 10, 64); err == nil && id > 0 {
		return id
	}
	if m := appUpdate().FindStringSubmatch(e.Install.Script); m != nil {
		if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && id > 0 {
			return id
		}
	}
	return 0
}

// steamApp is the Steam app of a game server. Servers made before the id
// was kept get it from their egg the first time it is asked for.
func (s *Server) steamApp(ctx context.Context, g store.GameServer, e *egg.Egg) int64 {
	if g.SteamAppID != 0 {
		return g.SteamAppID
	}
	id := steamAppID(e, g.Variables)
	if id != 0 {
		if err := s.Store.SetSteamAppID(ctx, g.AppID, id); err != nil {
			s.Log.Error("record steam app", "server", g.AppID, "err", err)
		}
	}
	return id
}

type gameSteamJSON struct {
	AppID int64 `json:"app_id"`
	// InstalledBuild is the build in the server's files, and LatestBuild the
	// newest one on the branch they came from, which is the public branch
	// unless the server was installed from a beta. Either is empty when not
	// known.
	InstalledBuild  string     `json:"installed_build"`
	LatestBuild     string     `json:"latest_build"`
	UpdateAvailable bool       `json:"update_available"`
	CheckedAt       *time.Time `json:"checked_at"`
	AutoUpdate      bool       `json:"auto_update"`
}

// installedBuild reads the build id and branch SteamCMD left in the server's
// files. The build is empty when the server has not been installed or the
// file is not there. It reads the way the file manager does, which works
// while the server runs; a peek into the volume would be refused then.
func (s *Server) installedBuild(ctx context.Context, a store.App, g store.GameServer, appID int64) steam.Manifest {
	if g.InstallState != store.InstallDone {
		return steam.Manifest{}
	}
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return steam.Manifest{}
	}
	i := slices.IndexFunc(vols, func(v store.Volume) bool { return v.Path == gameVolumePath })
	if i < 0 {
		return steam.Manifest{}
	}
	ref := core.FileRef{Volume: vols[i].Name, FileOwner: core.FileOwner{UID: gameUID, GID: gameGID}}
	manifest, err := s.Core.ReadFile(ctx, ref, steam.ManifestPath(appID))
	if err != nil {
		if !isNotFound(err) {
			s.Log.Warn("read steam manifest", "server", a.ID, "err", err)
		}
		return steam.Manifest{}
	}
	m, err := steam.ParseManifest(manifest)
	if err != nil {
		s.Log.Warn("read steam manifest", "server", a.ID, "err", err)
		return steam.Manifest{}
	}
	return m
}

func (s *Server) steamOut(ctx context.Context, a store.App, g store.GameServer, e *egg.Egg) *gameSteamJSON {
	app := s.steamApp(ctx, g, e)
	if app == 0 {
		return nil
	}
	out := &gameSteamJSON{AppID: app, AutoUpdate: g.SteamAutoUpdate}
	installed := s.installedBuild(ctx, a, g, app)
	out.InstalledBuild = installed.Build
	latest, at := s.steam.build(app, installed.Branch)
	out.LatestBuild = latest
	if !at.IsZero() {
		out.CheckedAt = &at
	}
	out.UpdateAvailable = out.InstalledBuild != "" && latest != "" && out.InstalledBuild != latest
	return out
}

type steamSettingsRequest struct {
	AutoUpdate bool `json:"auto_update"`
}

// setSteam changes the server's Steam settings.
func (s *Server) setSteam(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	var req steamSettingsRequest
	if !decode(w, r, &req) {
		return
	}
	_, e, _, err := s.gameParts(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "load egg", err)
		return
	}
	if s.steamApp(r.Context(), g, e) == 0 {
		writeError(w, errNotSteam.Err())
		return
	}
	if err := s.Store.SetSteamAutoUpdate(r.Context(), a.ID, req.AutoUpdate); err != nil {
		s.fail(w, "save steam settings", err)
		return
	}
	s.Log.Info("game steam settings", "server", a.ID, "auto_update", req.AutoUpdate, "user", loginFrom(r.Context()).account.ID)
	out, err := s.gameOut(r.Context(), a)
	if err != nil {
		s.fail(w, "describe game server", err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// updateSteam brings the server to the latest build now.
func (s *Server) updateSteam(w http.ResponseWriter, r *http.Request) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	id, bad := s.steamUpdate(r.Context(), a, g, loginFrom(r.Context()).account.ID)
	if bad != nil {
		writeError(w, bad)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]int64{"deployment": id})
}

// updatesOnStart says whether the egg's image updates the game each time it
// starts. Pterodactyl's Steam images do, unless the egg's AUTO_UPDATE
// variable is 0; an egg with no such variable leaves it on.
func updatesOnStart(g store.GameServer) bool {
	return g.Variables["AUTO_UPDATE"] != "0"
}

// steamUpdate updates a game server, and returns the deployment or install
// whose log shows how it went. Starting the server again is enough for an
// egg that updates when it starts; a stopped server, and one whose egg does
// not update on start, install again, which backs its files up first.
func (s *Server) steamUpdate(ctx context.Context, a store.App, g store.GameServer, user int64) (int64, *msg.Error) {
	_, e, _, err := s.gameParts(ctx, a.ID)
	if err != nil {
		return 0, s.internalError("update game server", err)
	}
	app := s.steamApp(ctx, g, e)
	switch {
	case app == 0:
		return 0, errNotSteam.Err()
	case g.InstallState == store.InstallRunning:
		return 0, errInstalling.Err()
	case g.InstallState != store.InstallDone:
		return 0, errNotInstalled.Err()
	}
	installed := s.installedBuild(ctx, a, g, app)
	latest, _ := s.steam.build(app, installed.Branch)
	if installed.Build == "" || latest == "" || installed.Build == latest {
		return 0, errNoSteamUpdate.Err()
	}

	state := s.gameState(ctx, a)
	running := state == stateStarting || state == stateRunning || state == stateStopping
	if running && updatesOnStart(g) {
		res, bad := s.power(ctx, a, g, "restart", user)
		if bad != nil {
			return 0, bad
		}
		s.Log.Info("steam update", "server", a.ID, "from", installed.Build, "to", latest, "how", "restart", "user", user)
		return res.Deployment, nil
	}
	if running {
		return 0, errStopToUpdate.Err()
	}

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
		return 0, s.internalError("begin install", err)
	case !began:
		return 0, errInstalling.Err()
	}
	id, err := s.startInstall(ctx, a, store.CauseReinstall, g.InstallState)
	if err != nil {
		s.Store.SetInstall(context.WithoutCancel(ctx), a.ID, g.InstallState, g.InstallID, time.Time{})
		return 0, s.internalError("start install", err)
	}
	s.Log.Info("steam update", "server", a.ID, "from", installed.Build, "to", latest, "how", "install", "user", user)
	return id, nil
}

// steamServer is a game server that comes from Steam.
type steamServer struct {
	app  store.App
	game store.GameServer
	egg  *egg.Egg
	id   int64
}

// steamServers lists the game servers that come from Steam. whole is false
// when a server could not be looked at, so a short list proves nothing.
func (s *Server) steamServers(ctx context.Context) (out []steamServer, whole bool) {
	games, err := s.Store.GameServers(ctx)
	if err != nil {
		s.Log.Error("steam: list game servers", "err", err)
		return nil, false
	}
	whole = true
	for _, g := range games {
		a, err := s.Store.App(ctx, g.AppID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.Log.Error("steam: load server", "server", g.AppID, "err", err)
			whole = false
			continue
		}
		_, e, _, err := s.gameParts(ctx, g.AppID)
		if err != nil {
			s.Log.Error("steam: load egg", "server", g.AppID, "err", err)
			whole = false
			continue
		}
		if id := s.steamApp(ctx, g, e); id != 0 {
			out = append(out, steamServer{app: a, game: g, egg: e, id: id})
		}
	}
	return out, whole
}

// steamImages is the SteamCMD image while a Steam server exists, so the
// image sweep does not delete it between two checks.
func (s *Server) steamImages(ctx context.Context) []string {
	if servers, _ := s.steamServers(ctx); len(servers) == 0 {
		return nil
	}
	return []string{steam.Image}
}

// wakeSteam starts the Steam check, which follows the Steam servers for as
// long as there are any. Call it when a game server is made or its settings
// change, since either can make it a Steam server.
func (s *Server) wakeSteam() {
	s.loops.wake("steam", steamRound, nil, s.steamRound)
}

// steamRound looks at the Steam servers once. It returns false when there are
// none.
func (s *Server) steamRound(ctx context.Context) bool {
	servers, whole := s.steamServers(ctx)
	if len(servers) == 0 {
		return !whole
	}
	s.steam.mu.Lock()
	due := s.now().Sub(s.steam.triedAt) >= steamCheckEvery
	if due {
		s.steam.triedAt = s.now()
	}
	s.steam.mu.Unlock()
	if due {
		ids := make([]int64, 0, len(servers))
		for _, sv := range servers {
			if !slices.Contains(ids, sv.id) {
				ids = append(ids, sv.id)
			}
		}
		slices.Sort(ids)
		builds, err := s.fetchSteamBuilds(ctx, ids)
		if err != nil {
			s.Log.Warn("steam: latest builds", "err", err)
		} else {
			s.steam.mu.Lock()
			if s.steam.latest == nil {
				s.steam.latest = map[int64]map[string]string{}
			}
			for id, b := range builds {
				s.steam.latest[id] = b
			}
			s.steam.checkedAt = s.now()
			s.steam.mu.Unlock()
		}
	}
	s.updateEmptyServers(ctx, servers)
	return true
}

// fetchSteamBuilds asks Steam for the newest build of each branch of each
// app, in a container that lives for that one question.
func (s *Server) fetchSteamBuilds(ctx context.Context, ids []int64) (map[int64]map[string]string, error) {
	bg := context.WithoutCancel(ctx)
	// A container left by a panel that stopped in the middle.
	s.Core.Remove(bg, steamContainer)
	_, err := s.Core.RunApp(ctx, engine.Spec{
		ID: steamContainer, Image: steam.Image, Args: steam.InfoCommand(ids), Network: steamContainer,
		MemoryBytes: 1 << 30, CPUs: 1, Pids: 512,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("start SteamCMD: %w", err)
	}
	defer s.Core.Remove(bg, steamContainer)

	waitCtx, cancel := context.WithTimeout(ctx, steamCheckTimeout)
	defer cancel()
	code, err := s.Core.Wait(waitCtx, steamContainer)
	if err != nil {
		return nil, fmt.Errorf("wait for SteamCMD: %w", err)
	}
	var out bytes.Buffer
	if err := s.Core.Logs(ctx, steamContainer, false, steamLogBytes, &out); err != nil {
		return nil, fmt.Errorf("read SteamCMD's output: %w", err)
	}
	builds, perr := steam.LatestBuilds(out.Bytes())
	if perr != nil {
		return nil, fmt.Errorf("SteamCMD exited with %d: %w (%s)", code, perr, lastLine(out.String()))
	}
	return builds, nil
}

func lastLine(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	return truncate(strings.TrimSpace(text), 200)
}

// updateEmptyServers updates the running servers that ask for it, when
// nobody is on them. A server whose players cannot be counted is left alone.
func (s *Server) updateEmptyServers(ctx context.Context, servers []steamServer) {
	for _, sv := range servers {
		if !sv.game.SteamAutoUpdate || sv.game.InstallState != store.InstallDone {
			continue
		}
		installed := s.installedBuild(ctx, sv.app, sv.game, sv.id)
		latest, _ := s.steam.build(sv.id, installed.Branch)
		if latest == "" || installed.Build == "" || installed.Build == latest {
			continue
		}
		s.steam.mu.Lock()
		tried := s.steam.updated[sv.app.ID] == latest
		s.steam.mu.Unlock()
		if tried || s.gameState(ctx, sv.app) != stateRunning {
			continue
		}
		players, err := s.playerCount(ctx, sv)
		if err != nil {
			s.Log.Info("steam: players not counted, so no update", "server", sv.app.ID, "err", err)
			continue
		}
		if players > 0 {
			continue
		}
		s.steam.mu.Lock()
		if s.steam.updated == nil {
			s.steam.updated = map[string]string{}
		}
		s.steam.updated[sv.app.ID] = latest
		s.steam.mu.Unlock()
		if _, bad := s.steamUpdate(ctx, sv.app, sv.game, 0); bad != nil {
			s.Log.Warn("steam: automatic update", "server", sv.app.ID, "err", bad.Text)
			continue
		}
		s.Log.Info("steam: updated an empty server", "server", sv.app.ID, "from", installed.Build, "to", latest)
	}
}

// playerCount asks the running server how many players are on it, at its
// container's own address so the answer does not depend on port forwards.
// Steam games answer Valve's A2S_INFO on their query port, which is the
// QUERY_PORT variable when the egg has one and the game port otherwise.
func (s *Server) playerCount(ctx context.Context, sv steamServer) (int, error) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return 0, err
	}
	var ip netip.Addr
	for _, c := range list {
		if c.App == sv.app.ID && c.State == "running" && !isInstallContainer(c) {
			ip = c.IP
		}
	}
	if !ip.IsValid() {
		return 0, errors.New("the server has no running container")
	}
	port := sv.app.Port
	if q, err := strconv.Atoi(sv.game.Variables["QUERY_PORT"]); err == nil && q > 0 && q < 65536 {
		port = q
	}
	addr := netip.AddrPortFrom(ip, uint16(port))
	if s.PlayerCount != nil {
		return s.PlayerCount(ctx, true, addr)
	}
	return countPlayers(ctx, true, addr)
}

// countPlayers asks a server at addr with the protocol of its game: A2S for
// Steam games and the Server List Ping for Minecraft.
func countPlayers(ctx context.Context, steamGame bool, addr netip.AddrPort) (int, error) {
	if steamGame {
		info, err := query.A2S(ctx, addr.String())
		return info.Players, err
	}
	status, err := query.Minecraft(ctx, addr.String())
	return status.Online, err
}
