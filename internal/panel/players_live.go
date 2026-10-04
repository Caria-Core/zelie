package panel

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

var (
	errPlayerNotRunning = msg.Define(http.StatusConflict, "players.not_running", "The server is not running.")
	errPlayerNoAnswer   = msg.Define(http.StatusBadGateway, "players.no_answer", "The server did not answer. It may still be starting.")
	errPlayerNoSupport  = msg.Define(http.StatusBadRequest, "players.unsupported", "This game does not support that.")
)

// How long the console gets to answer the list command.
var minecraftListWait = 3 * time.Second

// playerGame is a game server with what the players pages need to know of
// its game.
type playerGame struct {
	app    store.App
	game   store.GameServer
	egg    *egg.Egg
	source string // catalog id of the egg
	cap    string // players.Capability
	family string // players.Family
}

func (s *Server) loadPlayerGame(ctx context.Context, appID string) (playerGame, error) {
	a, err := s.Store.App(ctx, appID)
	if err != nil {
		return playerGame{}, err
	}
	g, e, _, err := s.gameParts(ctx, appID)
	if err != nil {
		return playerGame{}, err
	}
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return playerGame{}, err
	}
	return playerGame{app: a, game: g, egg: e, source: stored.Source,
		cap: players.Capability(stored.Source), family: players.Family(stored.Source)}, nil
}

func (s *Server) playerGameFrom(w http.ResponseWriter, r *http.Request) (playerGame, bool) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return playerGame{}, false
	}
	pg, err := s.loadPlayerGame(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "load game server", err)
		return pg, false
	}
	return pg, true
}

// variable is a variable of the server's egg as it will start with.
func (pg playerGame) variable(name string) string {
	if v, ok := pg.game.Variables[name]; ok {
		return v
	}
	for _, v := range pg.egg.Variables {
		if v.Env == name {
			return v.Default
		}
	}
	return ""
}

func (s *Server) playerEndpoint(ip netip.Addr, port int) string {
	if s.PlayerAddr != nil {
		return s.PlayerAddr(ip, port)
	}
	return netip.AddrPortFrom(ip, uint16(port)).String()
}

// runningAddr is the running server's container and its address on the
// panel's network, which does not depend on port forwards.
func (s *Server) runningAddr(ctx context.Context, app string) (string, netip.Addr, bool) {
	id, ok := s.runningGameContainer(ctx, app)
	if !ok {
		return "", netip.Addr{}, false
	}
	st, err := s.containerStatus(ctx, id)
	if err != nil {
		return id, netip.Addr{}, true
	}
	return id, st.IP, true
}

// rustRCON opens the remote console of a running Rust server. The caller
// closes it: nothing stays connected between requests.
func (s *Server) rustRCON(ctx context.Context, pg playerGame, ip netip.Addr) (*players.RCON, error) {
	port, err := strconv.Atoi(pg.variable("RCON_PORT"))
	pass := pg.variable("RCON_PASS")
	if err != nil || port < 1 || port > 65535 || pass == "" || !ip.IsValid() {
		return nil, errPlayerNoAnswer.Err()
	}
	c, err := players.DialRCON(ctx, s.playerEndpoint(ip, port), pass)
	if err != nil {
		// The error is already free of the password.
		s.Log.Info("players: remote console", "server", pg.app.ID, "err", err)
		return nil, errPlayerNoAnswer.Err()
	}
	return c, nil
}

// sendPlayerCommands runs console commands on the running server: over
// WebRCON for Rust, on the console for Minecraft.
func (s *Server) sendPlayerCommands(ctx context.Context, pg playerGame, container string, ip netip.Addr, cmds ...string) error {
	switch pg.family {
	case players.GameRust:
		c, err := s.rustRCON(ctx, pg, ip)
		if err != nil {
			return err
		}
		defer c.Close()
		for _, cmd := range cmds {
			if _, err := c.Run(ctx, cmd); err != nil {
				s.Log.Info("players: remote console", "server", pg.app.ID, "err", err)
				return errPlayerNoAnswer.Err()
			}
		}
		return nil
	case players.GameMinecraft:
		for _, cmd := range cmds {
			if err := s.Core.WriteStdin(ctx, container, []byte(cmd+"\n")); err != nil {
				if isNotFound(err) {
					return errPlayerNotRunning.Err()
				}
				s.Log.Warn("players: console command", "server", pg.app.ID, "err", err)
				return errConsoleWrite.Err()
			}
		}
		return nil
	}
	return errPlayerNoSupport.Err()
}

type onlinePlayerJSON struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	IP               string             `json:"ip"`
	Ping             *int               `json:"ping"`
	ConnectedSeconds *int               `json:"connected_seconds"`
	Banned           bool               `json:"banned"`
	Notes            int                `json:"notes"`
	Steam            *players.SteamInfo `json:"steam"`
}

type onlineJSON struct {
	Running   bool               `json:"running"`
	Source    string             `json:"source"`
	Players   []onlinePlayerJSON `json:"players"`
	Error     string             `json:"error"`
	ErrorCode string             `json:"error_code,omitempty"`
}

// onlinePlayers asks the running server who is on it. The question is
// asked now, for this request only: nothing polls.
func (s *Server) onlinePlayers(w http.ResponseWriter, r *http.Request) {
	pg, ok := s.playerGameFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := onlineJSON{Players: []onlinePlayerJSON{}}
	container, ip, running := s.runningAddr(ctx, pg.app.ID)
	if !running || pg.cap == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Running = true
	var err error
	switch {
	case pg.family == players.GameRust:
		out.Source = "rcon"
		out.Players, err = s.rustOnline(ctx, pg, ip)
	case pg.family == players.GameMinecraft:
		out.Source = "console"
		out.Players, err = s.minecraftOnline(ctx, pg, container)
	default:
		out.Source = "query"
		out.Players, err = s.queryOnline(ctx, pg, ip)
	}
	if err != nil {
		var m *msg.Error
		if !errors.As(err, &m) {
			m = errPlayerNoAnswer.Err()
		}
		out.Players, out.Error, out.ErrorCode = []onlinePlayerJSON{}, m.Text, m.Code
		writeJSON(w, http.StatusOK, out)
		return
	}
	s.decorateOnline(ctx, pg.app.ID, out.Players)
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) rustOnline(ctx context.Context, pg playerGame, ip netip.Addr) ([]onlinePlayerJSON, error) {
	c, err := s.rustRCON(ctx, pg, ip)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	text, err := c.Run(ctx, "playerlist")
	if err != nil {
		s.Log.Info("players: remote console", "server", pg.app.ID, "err", err)
		return nil, errPlayerNoAnswer.Err()
	}
	rows, err := players.ParseRustPlayerList(text)
	if err != nil {
		s.Log.Info("players: remote console", "server", pg.app.ID, "err", err)
		return nil, errPlayerNoAnswer.Err()
	}
	out := make([]onlinePlayerJSON, 0, len(rows))
	for _, p := range rows {
		ping, secs := p.Ping, p.ConnectedSeconds
		out = append(out, onlinePlayerJSON{ID: p.ID, Name: p.Name, IP: hostOf(p.Address), Ping: &ping, ConnectedSeconds: &secs})
	}
	return out, nil
}

func hostOf(addr string) string {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		return ap.Addr().String()
	}
	return addr
}

// minecraftOnline writes list on the console and reads the answer from the
// lines the console prints next.
func (s *Server) minecraftOnline(ctx context.Context, pg playerGame, container string) ([]onlinePlayerJSON, error) {
	lines, stop := s.consoles.hub(pg.app.ID).tap()
	defer stop()
	if err := s.Core.WriteStdin(ctx, container, []byte("list\n")); err != nil {
		if isNotFound(err) {
			return nil, errPlayerNotRunning.Err()
		}
		return nil, errConsoleWrite.Err()
	}
	var list players.MinecraftList
	timeout := time.NewTimer(minecraftListWait)
	defer timeout.Stop()
wait:
	for {
		select {
		case line := <-lines:
			if list.Feed(ansi().ReplaceAllString(line, "")) {
				break wait
			}
		case <-timeout.C:
			return nil, errPlayerNoAnswer.Err()
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([]onlinePlayerJSON, 0, len(list.Names))
	for _, name := range list.Names {
		p := onlinePlayerJSON{ID: "name:" + name, Name: name}
		if known, err := s.Store.PlayerByName(ctx, pg.app.ID, name); err == nil {
			p.ID, p.IP = known.ID, known.LastIP
		}
		out = append(out, p)
	}
	return out, nil
}

// queryOnline asks a Steam game with Valve's query protocol, at the query
// port: the egg's QUERY_PORT when it has one, else the game's own port plus
// what the game is known to add.
func (s *Server) queryOnline(ctx context.Context, pg playerGame, ip netip.Addr) ([]onlinePlayerJSON, error) {
	if !ip.IsValid() {
		return nil, errPlayerNoAnswer.Err()
	}
	port := pg.app.Port + players.QueryPortOffset(pg.source)
	if q, err := strconv.Atoi(pg.variable("QUERY_PORT")); err == nil && q > 0 && q < 65536 {
		port = q
	}
	rows, err := players.A2SPlayers(ctx, s.playerEndpoint(ip, port))
	if err != nil {
		s.Log.Info("players: query", "server", pg.app.ID, "err", err)
		return nil, errPlayerNoAnswer.Err()
	}
	out := make([]onlinePlayerJSON, 0, len(rows))
	for _, p := range rows {
		// Servers list players that are still connecting with no name.
		if p.Name == "" {
			continue
		}
		secs := int(p.Duration / time.Second)
		out = append(out, onlinePlayerJSON{ID: "name:" + p.Name, Name: p.Name, ConnectedSeconds: &secs})
	}
	return out, nil
}

// decorateOnline adds what the panel knows: bans, notes and Steam profiles.
func (s *Server) decorateOnline(ctx context.Context, app string, list []onlinePlayerJSON) {
	ids := make([]string, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	flags, err := s.Store.PlayerFlagsFor(ctx, app, ids, s.now())
	if err != nil {
		s.Log.Error("player flags", "server", app, "err", err)
	}
	steam := s.steamFor(ctx, ids)
	for i := range list {
		f := flags[list[i].ID]
		list[i].Banned, list[i].Notes, list[i].Steam = f.Banned, f.Notes, steam[list[i].ID]
	}
}

// cleanReason makes what an administrator typed fit on one console line.
func cleanReason(s string) string { return players.CleanArg(s, 200) }
