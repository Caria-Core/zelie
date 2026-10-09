package panel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// The console is a WebSocket at GET /api/games/{app}/console?token=...
// The token comes from POST /api/games/{app}/console/token: valid for a
// minute, good for one connection, and tied to the account, the server and
// the session that asked. The browser cannot set headers on a socket, and
// the token keeps the session cookie out of the address.
//
// Every message is a JSON text frame.
//
// Server to client:
//
//	{"type":"history","commands":["..."]}      the account's last commands, first message only
//	{"type":"state","state":"..."}             on open and whenever it changes: starting, running,
//	                                           stopping, stopped, crashed, installing (or unknown
//	                                           when the core cannot be asked)
//	{"type":"line","data":"..."}               a console line, colour codes included; on open
//	                                           the last 500, then live. "[…N lines skipped]" marks
//	                                           what a slow client missed.
//	{"type":"install","data":"..."}            a line of the install script while one runs
//	{"type":"eula"}                            the game refused to start until its EULA is accepted;
//	                                           POST /api/games/{app}/eula records it
//	{"type":"error","code":"...","message":"...","params":{}}
//
// Client to server:
//
//	{"type":"command","data":"say hi"}         one line of at most 1024 bytes, written to the game
//	{"type":"power","action":"start|stop|restart|kill"}
//
// At most 10 messages a second are taken from a socket. The socket closes
// when its session ends, when the account loses access, and when the client
// stops answering pings.

const (
	consoleTokenTTL = 60 * time.Second
	consoleMaxData  = 1024
	consoleRatePerS = 10
)

// How the socket is watched. Tests shorten them.
var (
	consoleRecheck  = time.Minute
	consolePing     = 20 * time.Second
	consolePongWait = 60 * time.Second
	// A client that takes nothing for this long is dropped.
	consoleWriteWait = 15 * time.Second
)

var (
	errConsoleToken      = msg.Define(http.StatusForbidden, "console.bad_token", "The console link has expired. Open the console again.")
	errConsoleOrigin     = msg.Define(http.StatusForbidden, "console.bad_origin", "The console only opens from the panel's own page.")
	errConsoleDenied     = msg.Define(http.StatusForbidden, "console.forbidden", "You cannot use this server's console.")
	errConsoleNotRunning = msg.Define(0, "console.not_running", "The server is not running.")
	errConsoleBadCommand = msg.Define(0, "console.bad_command", "A command is one line of at most {max} bytes.")
	errConsoleTooFast    = msg.Define(0, "console.too_fast", "Too many messages at once. Wait a moment.")
	errConsoleBadMessage = msg.Define(0, "console.bad_message", "The message was not understood.")
	errConsoleWrite      = msg.Define(0, "console.write_failed", "The server's console did not take the command.")
	errConsoleSlow       = msg.Define(0, "console.too_slow", "The console could not keep up.")
	errConsoleSignout    = msg.Define(0, "console.signed_out", "You were signed out or lost access to this server.")
)

// canConsole says whether the account may use the server's console and its
// power buttons. Everything about who may is decided here.
func canConsole(a store.Account, _ store.App) bool {
	return a.Admin
}

type consoleClaims struct {
	Account int64  `json:"a"`
	App     string `json:"g"`
	Session []byte `json:"s"`
	Expires int64  `json:"e"`
	Nonce   []byte `json:"n"`
}

const consoleSealPurpose = "console"

func (s *Server) consoleToken(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	l := loginFrom(r.Context())
	if !canConsole(l.account, a) {
		writeError(w, errConsoleDenied.Err())
		return
	}
	c := consoleClaims{
		Account: l.account.ID, App: a.ID, Session: l.session.Hash,
		Expires: s.now().Add(consoleTokenTTL).Unix(), Nonce: make([]byte, 16),
	}
	rand.Read(c.Nonce)
	b, _ := json.Marshal(c)
	token := base64.RawURLEncoding.EncodeToString(s.Sealer.Seal(b, consoleSealPurpose))
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "expires_in": int(consoleTokenTTL / time.Second)})
}

// openConsoleToken checks a token for the server without using it up.
func (s *Server) openConsoleToken(token, app string) (consoleClaims, bool) {
	var c consoleClaims
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(token) > 1024 {
		return c, false
	}
	plain, err := s.Sealer.Open(raw, consoleSealPurpose)
	if err != nil || json.Unmarshal(plain, &c) != nil {
		return c, false
	}
	return c, c.App == app && s.now().Unix() <= c.Expires
}

// sameOrigin reports whether the request comes from a page of the panel
// itself. A socket is not bound by the browser's same-origin rules, and the
// cookie is not the only thing a page of another site could bring along, so
// the origin is checked here.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path != "" || u.User != nil {
		return false
	}
	// The panel is only ever reached over HTTPS, through the proxy.
	scheme := "https"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return u.Scheme == scheme && strings.EqualFold(u.Host, r.Host)
}

// consoleAccess loads what a socket belongs to and checks it is still
// allowed: the session lives, is the token's, and its account may use the
// console of an app that still exists.
var errNoAccess = errors.New("the account may not use this console")

func (s *Server) consoleAccess(ctx context.Context, c consoleClaims) (store.Account, store.App, error) {
	var acct store.Account
	x, err := s.Store.Session(ctx, c.Session, s.now())
	if err != nil {
		return acct, store.App{}, err
	}
	if !x.Verified || x.UserID != c.Account {
		return acct, store.App{}, store.ErrNotFound
	}
	if acct, err = s.Store.AccountByID(ctx, c.Account); err != nil {
		return acct, store.App{}, err
	}
	a, err := s.Store.App(ctx, c.App)
	if err == nil && !a.RunsEgg() {
		err = store.ErrNotFound
	}
	if err != nil {
		return acct, a, err
	}
	if !canConsole(acct, a) {
		return acct, a, errNoAccess
	}
	return acct, a, nil
}

func (s *Server) consoleSocket(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeError(w, errConsoleOrigin.Err())
		return
	}
	token := r.URL.Query().Get("token")
	claims, ok := s.openConsoleToken(token, r.PathValue("app"))
	// A browser also sends its cookie: it has to be the session the token
	// was made for.
	if cookie := sessionHash(r); ok && cookie != nil && subtle.ConstantTimeCompare(cookie, claims.Session) != 1 {
		ok = false
	}
	if !ok {
		writeError(w, errConsoleToken.Err())
		return
	}
	acct, a, err := s.consoleAccess(r.Context(), claims)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound):
		writeError(w, errConsoleToken.Err())
		return
	case errors.Is(err, errNoAccess):
		writeError(w, errConsoleDenied.Err())
		return
	default:
		s.fail(w, "open console", err)
		return
	}
	// Last, so a token that fails a check is not spent by it. It is kept by
	// its nonce: the same token has several spellings in base64.
	if !s.consoleUsed.take(string(claims.Nonce), time.Unix(claims.Expires, 0), s.now()) {
		writeError(w, errConsoleToken.Err())
		return
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	s.watchers.Add(1)
	defer s.watchers.Done()
	(&consoleConn{s: s, conn: conn, claims: claims, account: acct.ID, app: a.ID}).run(r.Context())
}

// consoleConn is one open socket.
type consoleConn struct {
	s       *Server
	conn    *websocket.Conn
	claims  consoleClaims
	account int64
	app     string

	sub    *consoleSub
	cancel context.CancelFunc
	once   sync.Once
}

// end closes the socket with a reason, and stops the goroutines around it.
func (c *consoleConn) end(code websocket.StatusCode, reason string) {
	c.once.Do(func() {
		c.conn.Close(code, reason)
		c.cancel()
	})
}

func (c *consoleConn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	defer cancel()
	defer c.conn.CloseNow()
	stop := context.AfterFunc(c.s.baseContext(), cancel)
	defer stop()
	c.conn.SetReadLimit(4 << 10)

	h := c.s.consoles.hub(c.app)
	c.s.loadStoppedConsole(ctx, h)
	c.sub = newConsoleSub()
	a, err := c.s.Store.App(ctx, c.app)
	if err != nil {
		return
	}
	g, err := c.s.Store.GameServer(ctx, c.app)
	if err != nil {
		return
	}
	hist, _ := json.Marshal(struct {
		Type     string   `json:"type"`
		Commands []string `json:"commands"`
	}{"history", c.s.consoleHist.get(c.app, c.account)})
	c.sub.pushForced(hist)
	h.subscribe(c.s, c.sub, c.s.consoleStatus(ctx, a, g))
	defer h.unsubscribe(c.sub)

	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for _, f := range []func(context.Context){c.write, c.ping, c.watch} {
		wg.Go(func() { f(ctx) })
	}

	rate := rateLimit{perSecond: consoleRatePerS}
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue
		}
		if !rate.allow(time.Now()) {
			c.sendError(errConsoleTooFast.With())
			continue
		}
		c.handle(ctx, h, data)
	}
}

// write sends what is queued for the socket. A client that stops reading
// makes a write time out, which closes the socket.
func (c *consoleConn) write(ctx context.Context) {
	for {
		b, ok, over := c.sub.pop()
		if over {
			c.end(websocket.StatusPolicyViolation, errConsoleSlow.English)
			return
		}
		if !ok {
			select {
			case <-c.sub.wake:
			case <-c.sub.gone:
				c.end(websocket.StatusGoingAway, "server removed")
				return
			case <-ctx.Done():
				return
			}
			continue
		}
		wctx, cancel := context.WithTimeout(ctx, consoleWriteWait)
		err := c.conn.Write(wctx, websocket.MessageText, b)
		cancel()
		if err != nil {
			c.cancel()
			return
		}
	}
}

func (c *consoleConn) ping(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(consolePing):
		}
		pctx, cancel := context.WithTimeout(ctx, consolePongWait)
		err := c.conn.Ping(pctx)
		cancel()
		if err != nil {
			c.conn.CloseNow()
			c.cancel()
			return
		}
	}
}

// watch ends the socket when the session or the access it was opened with
// is gone.
func (c *consoleConn) watch(ctx context.Context) {
	t := time.NewTicker(consoleRecheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		_, _, err := c.s.consoleAccess(ctx, c.claims)
		if err != nil && ctx.Err() == nil && (errors.Is(err, store.ErrNotFound) || errors.Is(err, errNoAccess)) {
			c.end(websocket.StatusPolicyViolation, errConsoleSignout.English)
			return
		}
	}
}

func (c *consoleConn) sendError(m msg.Msg) {
	b, _ := json.Marshal(struct {
		Type    string         `json:"type"`
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Params  map[string]any `json:"params,omitempty"`
	}{"error", m.Code, m.Text, m.Params})
	c.sub.push(b, false)
}

func (c *consoleConn) handle(ctx context.Context, h *consoleHub, data []byte) {
	var in struct {
		Type   string `json:"type"`
		Data   string `json:"data"`
		Action string `json:"action"`
	}
	if json.Unmarshal(data, &in) != nil {
		c.sendError(errConsoleBadMessage.With())
		return
	}
	switch in.Type {
	case "command":
		c.command(ctx, in.Data)
	case "power":
		c.power(ctx, h, in.Action)
	default:
		c.sendError(errConsoleBadMessage.With())
	}
}

func (c *consoleConn) command(ctx context.Context, cmd string) {
	if cmd == "" || len(cmd) > consoleMaxData || strings.ContainsAny(cmd, "\r\n\x00") {
		c.sendError(errConsoleBadCommand.With("max", consoleMaxData))
		return
	}
	id, ok := c.s.runningGameContainer(ctx, c.app)
	if !ok {
		c.sendError(errConsoleNotRunning.With())
		return
	}
	if err := c.s.Core.WriteStdin(ctx, id, []byte(cmd+"\n")); err != nil {
		if isNotFound(err) {
			c.sendError(errConsoleNotRunning.With())
			return
		}
		c.s.Log.Warn("console command", "server", c.app, "user", c.account, "err", err)
		c.sendError(errConsoleWrite.With())
		return
	}
	c.s.consoleHist.add(c.app, c.account, cmd)
	logged := cmd
	if len(logged) > 200 {
		logged = truncate(logged, 200) + " (cut)"
	}
	c.s.Log.Info("game console command", "server", c.app, "user", c.account, "command", logged)
}

func (c *consoleConn) power(ctx context.Context, h *consoleHub, action string) {
	a, err := c.s.Store.App(ctx, c.app)
	if err != nil {
		c.sendError(c.s.internalError("console power", err).Msg)
		return
	}
	g, err := c.s.Store.GameServer(ctx, c.app)
	if err != nil {
		c.sendError(c.s.internalError("console power", err).Msg)
		return
	}
	// Whoever asked for the action may close the page before it is done;
	// a half-done stop or restart is worse than either.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res, perr := c.s.power(pctx, a, g, action, c.account)
	if perr != nil {
		c.sendError(perr.Msg)
		return
	}
	h.setState(res.State)
}

// runningGameContainer is the container of the server that is up and not
// on its way down.
func (s *Server) runningGameContainer(ctx context.Context, app string) (string, bool) {
	list, err := s.Core.List(ctx)
	if err != nil {
		return "", false
	}
	for _, c := range list {
		if c.App != app || c.State != "running" || isInstallContainer(c) {
			continue
		}
		if r, ok := s.gameRuns.get(app); ok && r.container == c.ID && r.state == stateStopping {
			return "", false
		}
		return c.ID, true
	}
	return "", false
}

// rateLimit lets through perSecond messages a second, with a burst as
// large.
type rateLimit struct {
	perSecond float64
	tokens    float64
	last      time.Time
}

func (r *rateLimit) allow(now time.Time) bool {
	if r.last.IsZero() {
		r.tokens = r.perSecond
	} else {
		r.tokens = min(r.perSecond, r.tokens+now.Sub(r.last).Seconds()*r.perSecond)
	}
	r.last = now
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
