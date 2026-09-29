package panel

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Caria-Core/zelie/internal/peer"
)

// consoleEnv is a panel with a server the console can be opened on.
type consoleEnv struct {
	*appEnv
	srv *httptest.Server

	clockMu sync.Mutex
	clock   time.Time
}

func (e *consoleEnv) now() time.Time {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	return e.clock
}

func (e *consoleEnv) advance(d time.Duration) {
	e.clockMu.Lock()
	defer e.clockMu.Unlock()
	e.clock = e.clock.Add(d)
}

func newConsoleEnv(t *testing.T) *consoleEnv {
	t.Helper()
	// Registered first, so it runs after the server's goroutines are done.
	polls, rechecks, drain, ping, pong, wait, lines, bytes := consolePoll, consoleRecheck, consoleDrain, consolePing, consolePongWait, consoleWriteWait, consoleQueueLines, consoleQueueBytes
	t.Cleanup(func() {
		consolePoll, consoleRecheck, consoleDrain, consolePing, consolePongWait, consoleWriteWait, consoleQueueLines, consoleQueueBytes = polls, rechecks, drain, ping, pong, wait, lines, bytes
	})
	e := newPowerEnv(t)
	consolePoll, consoleRecheck, consoleDrain = 5*time.Millisecond, time.Hour, 20*time.Millisecond
	srv := httptest.NewUnstartedServer(e.s.Handler())
	srv.Config.ConnContext = func(ctx context.Context, _ net.Conn) context.Context {
		return peer.WithPeer(ctx, peer.Peer{UID: proxyUID})
	}
	srv.Start()
	t.Cleanup(srv.Close)
	ce := &consoleEnv{appEnv: e, srv: srv, clock: e.s.Now()}
	e.s.Now = ce.now
	return ce
}

// token asks for a console token the way the interface does.
func (e *consoleEnv) token(t *testing.T, app string) string {
	t.Helper()
	code, out := e.b.do("POST", "/api/games/"+app+"/console/token", nil)
	if code != http.StatusOK || out["expires_in"] != 60.0 {
		t.Fatalf("token: %d %v", code, out)
	}
	return out["token"].(string)
}

// open connects to the console as the browser would.
func (e *consoleEnv) open(app, token string, mod func(http.Header)) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	h.Set("Origin", "https://panel.example.com")
	if e.b.cookie != nil {
		h.Set("Cookie", e.b.cookie.Name+"="+e.b.cookie.Value)
	}
	if mod != nil {
		mod(h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/api/games/"+app+"/console?token="+token,
		&websocket.DialOptions{Host: "panel.example.com", HTTPHeader: h})
}

func (e *consoleEnv) connect(t *testing.T, app string) *websocket.Conn {
	t.Helper()
	c, resp, err := e.open(app, e.token(t, app), nil)
	if err != nil {
		t.Fatalf("open: %v %v", err, resp)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func read(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("message %q: %v", b, err)
	}
	return m
}

// readUntil returns the messages up to and including the first that ok
// accepts.
func readUntil(t *testing.T, c *websocket.Conn, ok func(map[string]any) bool) []map[string]any {
	t.Helper()
	var got []map[string]any
	for {
		m := read(t, c)
		got = append(got, m)
		if ok(m) {
			return got
		}
	}
}

func isLine(text string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == "line" && m["data"] == text }
}

// pump reads a socket in the background, so its side never stalls.
func pump(c *websocket.Conn) <-chan string {
	ch := make(chan string, 1<<12)
	go func() {
		defer close(ch)
		for {
			_, b, err := c.Read(context.Background())
			if err != nil {
				return
			}
			select {
			case ch <- string(b):
			default:
			}
		}
	}()
	return ch
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("send: %v", err)
	}
}

// stdin is what has been written to the server's console.
func (e *consoleEnv) stdin(id string) []string {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	return slices.Clone(e.core.consoles[id])
}

func (e *consoleEnv) waitStdin(t *testing.T, id string, n int) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if got := e.stdin(id); len(got) >= n {
			return got
		}
	}
	t.Fatalf("stdin has %q, want %d writes", e.stdin(id), n)
	return nil
}

// running has a server up with its console open to the game.
func (e *consoleEnv) running(t *testing.T, name string) string {
	t.Helper()
	e.newGame(t, name, consoleEggURL, nil)
	e.power(t, name, "start")
	e.settle(t, name)
	return e.liveContainer(t, name)
}

func TestConsoleTokenChecks(t *testing.T) {
	e := newConsoleEnv(t)
	e.newGame(t, "survival", consoleEggURL, map[string]any{"ports": 1})
	e.newGame(t, "creative", consoleEggURL, map[string]any{"ports": 1})
	now := e.now

	status := func(app, token string, mod func(http.Header)) int {
		c, resp, err := e.open(app, token, mod)
		if err == nil {
			c.CloseNow()
			return http.StatusSwitchingProtocols
		}
		if resp == nil {
			t.Fatalf("open: %v", err)
		}
		return resp.StatusCode
	}

	if code, _ := (&browser{t: t, h: e.b.h, ip: "198.51.100.8"}).do("POST", "/api/games/survival/console/token", nil); code != http.StatusUnauthorized {
		t.Errorf("token while signed out: %d", code)
	}
	if code, out := e.b.do("POST", "/api/games/nothing/console/token", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("token for no server: %d %v", code, out)
	}

	// A good token opens once.
	tok := e.token(t, "survival")
	if got := status("survival", tok, nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("first use: %d", got)
	}
	if got := status("survival", tok, nil); got != http.StatusForbidden {
		t.Errorf("second use: %d", got)
	}

	// It is for the server that asked.
	if got := status("creative", e.token(t, "survival"), nil); got != http.StatusForbidden {
		t.Errorf("other server: %d", got)
	}

	// It runs out.
	tok = e.token(t, "survival")
	e.advance(consoleTokenTTL + time.Second)
	if got := status("survival", tok, nil); got != http.StatusForbidden {
		t.Errorf("expired: %d", got)
	}
	// Signing in happened at the old time; the session lasts days.
	tok = e.token(t, "survival")
	e.advance(consoleTokenTTL - time.Second)
	if got := status("survival", tok, nil); got != http.StatusSwitchingProtocols {
		t.Errorf("just before it expires: %d", got)
	}

	// A token is not a login: it is for one account and the session that
	// asked.
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(e.b.cookie)
	forge := func(mod func(*consoleClaims)) string {
		c := consoleClaims{Account: 1, App: "survival", Session: sessionHash(req), Expires: now().Add(time.Minute).Unix(), Nonce: []byte(fmt.Sprint(time.Now().UnixNano()))}
		mod(&c)
		b, _ := json.Marshal(c)
		return base64.RawURLEncoding.EncodeToString(e.s.Sealer.Seal(b, consoleSealPurpose))
	}
	if got := status("survival", forge(func(*consoleClaims) {}), nil); got != http.StatusSwitchingProtocols {
		t.Fatalf("forged with the right claims: %d", got)
	}
	if got := status("survival", forge(func(c *consoleClaims) { c.Account = 2 }), nil); got != http.StatusForbidden {
		t.Errorf("another account: %d", got)
	}
	other := sha256.Sum256([]byte("another session"))
	if got := status("survival", forge(func(c *consoleClaims) { c.Session = other[:] }), nil); got != http.StatusForbidden {
		t.Errorf("a session that does not exist: %d", got)
	}
	// The cookie the browser sends has to be the token's session.
	otherCookie := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	if got := status("survival", e.token(t, "survival"), func(h http.Header) { h.Set("Cookie", cookieName+"="+otherCookie) }); got != http.StatusForbidden {
		t.Errorf("another session's cookie: %d", got)
	}
	// Sealed for something else.
	b, _ := json.Marshal(consoleClaims{Account: 1, App: "survival", Session: sessionHash(req), Expires: now().Add(time.Minute).Unix()})
	if got := status("survival", base64.RawURLEncoding.EncodeToString(e.s.Sealer.Seal(b, "login-pow")), nil); got != http.StatusForbidden {
		t.Errorf("sealed for another purpose: %d", got)
	}
	if got := status("survival", "garbage", nil); got != http.StatusForbidden {
		t.Errorf("garbage: %d", got)
	}

	// Another site's page cannot open a socket, whatever it holds.
	for _, origin := range []string{"https://evil.example.com", "http://panel.example.com", "null", "https://panel.example.com.evil.example.com", ""} {
		if got := status("survival", e.token(t, "survival"), func(h http.Header) {
			if origin == "" {
				h.Del("Origin")
			} else {
				h.Set("Origin", origin)
			}
		}); got != http.StatusForbidden {
			t.Errorf("origin %q: %d", origin, got)
		}
	}
	// The token was not spent by a request that failed the origin check.
	tok = e.token(t, "survival")
	if got := status("survival", tok, func(h http.Header) { h.Set("Origin", "https://evil.example.com") }); got != http.StatusForbidden {
		t.Fatalf("wrong origin: %d", got)
	}
	if got := status("survival", tok, nil); got != http.StatusSwitchingProtocols {
		t.Errorf("token after a wrong origin: %d", got)
	}
}

func TestConsoleOpensWithTheLastLines(t *testing.T) {
	e := newConsoleEnv(t)
	id := e.running(t, "survival")
	var out strings.Builder
	for i := range 600 {
		fmt.Fprintf(&out, "line %d\n", i)
	}
	e.core.emit(id, out.String()+"Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")

	c := e.connect(t, "survival")
	msgs := readUntil(t, c, isLine("Done (1s)! For help, type \"help\""))
	if msgs[0]["type"] != "history" || len(msgs[0]["commands"].([]any)) != 0 {
		t.Errorf("first message %v", msgs[0])
	}
	var lines []string
	var states []string
	for _, m := range msgs[1:] {
		switch m["type"] {
		case "line":
			lines = append(lines, m["data"].(string))
		case "state":
			states = append(states, m["state"].(string))
		}
	}
	if len(lines) != 500 || lines[0] != "line 101" || lines[498] != "line 599" {
		t.Errorf("%d lines, from %q to %q", len(lines), lines[0], lines[len(lines)-2])
	}
	if len(states) == 0 || states[0] != "running" {
		t.Errorf("states %v", states)
	}
}

func TestConsoleStateFollowsTheServer(t *testing.T) {
	e := newConsoleEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	c := e.connect(t, "survival")
	if m := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" }); m[len(m)-1]["state"] != "stopped" {
		t.Fatalf("first state %v", m)
	}

	// The power message does what the endpoint does.
	send(t, c, map[string]string{"type": "power", "action": "start"})
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" && m["state"] == "starting" })
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	readUntil(t, c, isLine("Done (1s)! For help, type \"help\""))
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" && m["state"] == "running" })

	send(t, c, map[string]string{"type": "power", "action": "stop"})
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" && m["state"] == "stopping" })
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" && m["state"] == "stopped" })
	if got := e.stdin(id); len(got) != 1 || got[0] != "stop\n" {
		t.Errorf("stdin %q", got)
	}

	send(t, c, map[string]string{"type": "power", "action": "fly"})
	if m := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "error" }); m[len(m)-1]["code"] != "game.bad_power" {
		t.Errorf("bad action: %v", m[len(m)-1])
	}
}

func TestConsoleCommands(t *testing.T) {
	e := newConsoleEnv(t)
	id := e.running(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")

	c := e.connect(t, "survival")
	readUntil(t, c, isLine("Done (1s)! For help, type \"help\""))
	send(t, c, map[string]string{"type": "command", "data": "say hi"})
	if got := e.waitStdin(t, id, 1); got[0] != "say hi\n" {
		t.Errorf("stdin %q", got)
	}

	// Not one line, or too long.
	for _, bad := range []string{"say hi\nstop", "say hi\r", "", strings.Repeat("a", 1025)} {
		send(t, c, map[string]string{"type": "command", "data": bad})
		if m := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "error" }); m[len(m)-1]["code"] != "console.bad_command" {
			t.Errorf("command %.20q: %v", bad, m[len(m)-1])
		}
	}
	send(t, c, map[string]string{"type": "command", "data": strings.Repeat("b", 1024)})
	e.waitStdin(t, id, 2)
	send(t, c, map[string]string{"type": "nonsense"})
	if m := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "error" }); m[len(m)-1]["code"] != "console.bad_message" {
		t.Errorf("unknown message: %v", m[len(m)-1])
	}
	if got := e.stdin(id); len(got) != 2 {
		t.Errorf("stdin %d writes", len(got))
	}

	// The history is the account's, oldest first, without repeats, and
	// comes with the next connection.
	send(t, c, map[string]string{"type": "command", "data": "say hi"})
	e.waitStdin(t, id, 3)
	c2 := e.connect(t, "survival")
	h := read(t, c2)
	if cmds := h["commands"].([]any); h["type"] != "history" || len(cmds) != 3 || cmds[0] != "say hi" || cmds[2] != "say hi" {
		t.Errorf("history %v", h)
	}
}

func TestConsoleHistoryKeepsFifty(t *testing.T) {
	var h consoleHistory
	for i := range 80 {
		h.add("srv", 1, fmt.Sprint("cmd ", i))
	}
	h.add("srv", 2, "mine")
	got := h.get("srv", 1)
	if len(got) != 50 || got[0] != "cmd 30" || got[49] != "cmd 79" {
		t.Errorf("history %d: %q ... %q", len(got), got[0], got[len(got)-1])
	}
	if got := h.get("srv", 2); len(got) != 1 || got[0] != "mine" {
		t.Errorf("other account: %q", got)
	}
	h.forget("srv")
	if len(h.get("srv", 1)) != 0 {
		t.Error("history outlived the server")
	}
}

func TestConsoleCommandNeedsARunningServer(t *testing.T) {
	e := newConsoleEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	c := e.connect(t, "survival")
	for range 2 {
		send(t, c, map[string]string{"type": "command", "data": "say hi"})
		if m := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "error" }); m[len(m)-1]["code"] != "console.not_running" {
			t.Fatalf("command to a stopped server: %v", m[len(m)-1])
		}
	}
	// The socket stays open.
	send(t, c, map[string]string{"type": "power", "action": "start"})
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" && m["state"] == "starting" })
}

func TestConsoleRateLimit(t *testing.T) {
	e := newConsoleEnv(t)
	id := e.running(t, "survival")
	c := e.connect(t, "survival")
	read(t, c)

	for range 60 {
		send(t, c, map[string]string{"type": "command", "data": "list"})
	}
	// Whatever was let through has reached the game by now.
	time.Sleep(200 * time.Millisecond)
	got := len(e.stdin(id))
	if got < 10 || got > 25 {
		t.Errorf("%d of 60 commands were taken", got)
	}
	fast := 0
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		_, b, err := c.Read(ctx)
		cancel()
		if err != nil {
			break
		}
		if strings.Contains(string(b), `"console.too_fast"`) {
			fast++
		}
	}
	if fast != 60-got {
		t.Errorf("%d commands taken and %d told to slow down", got, fast)
	}
}

func TestConsoleSeveralBrowsers(t *testing.T) {
	e := newConsoleEnv(t)
	id := e.running(t, "survival")
	a, b := e.connect(t, "survival"), e.connect(t, "survival")
	e.core.emit(id, "hello both\n")
	readUntil(t, a, isLine("hello both"))
	readUntil(t, b, isLine("hello both"))
}

func TestConsoleFollowsTheNewContainer(t *testing.T) {
	e := newConsoleEnv(t)
	first := e.running(t, "survival")
	c := e.connect(t, "survival")
	e.core.emit(first, "from the first\n")
	readUntil(t, c, isLine("from the first"))

	e.power(t, "survival", "restart")
	e.settle(t, "survival")
	second := e.liveContainer(t, "survival")
	if second == first {
		t.Fatal("the restart did not make a new container")
	}
	e.core.emit(second, "from the second\n")
	readUntil(t, c, isLine("from the second"))
	// Someone who opens it now sees only the new one.
	d := e.connect(t, "survival")
	msgs := readUntil(t, d, isLine("from the second"))
	for _, m := range msgs {
		if m["data"] == "from the first" {
			t.Error("the old container's line is still shown")
		}
	}
}

func TestConsoleShowsTheLastRunOfAStoppedServer(t *testing.T) {
	e := newConsoleEnv(t)
	id := e.running(t, "survival")
	e.core.emit(id, "the reason it crashed\n")
	time.Sleep(50 * time.Millisecond)
	e.power(t, "survival", "kill")
	e.waitState(t, "survival", "stopped")
	// A panel that started after the server stopped has no follower.
	e.s.consoles.forget("survival")

	c := e.connect(t, "survival")
	readUntil(t, c, isLine("the reason it crashed"))
}

func TestConsoleShowsTheInstall(t *testing.T) {
	e := newConsoleEnv(t)
	e.core.mu.Lock()
	e.core.installHang = true
	e.core.mu.Unlock()
	old := installTimeout
	installTimeout = time.Minute
	t.Cleanup(func() { installTimeout = old })
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "slow", "egg_url": consoleEggURL}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	c := e.connect(t, "slow")
	msgs := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "install" && m["data"] == "downloading server.jar" })
	sawState := false
	for _, m := range msgs {
		sawState = sawState || m["type"] == "state" && m["state"] == "installing"
	}
	if !sawState {
		t.Errorf("no installing state in %v", msgs)
	}
	e.s.deploys.cancel("slow")
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "state" && m["state"] != "installing" })
}

func TestConsoleSlowClientLosesLinesNotTheServer(t *testing.T) {
	e := newConsoleEnv(t)
	consoleQueueLines, consoleQueueBytes, consoleWriteWait = 4, 8<<10, time.Minute
	id := e.running(t, "survival")

	slow, quick := e.connect(t, "survival"), e.connect(t, "survival")
	seen := pump(quick)
	// The slow one reads nothing while a lot is printed.
	e.core.emit(id, strings.Repeat(strings.Repeat("x", 1023)+"\n", 4<<10))
	// The other does not wait for it. It may lose lines too, when it is
	// slower than the game; what comes after the burst reaches it.
	deadline := time.After(20 * time.Second)
	for got := false; !got; {
		e.core.emit(id, "after the burst\n")
		select {
		case b := <-seen:
			got = strings.Contains(b, "after the burst")
		case <-deadline:
			t.Fatal("the second browser got nothing while the first was slow")
		case <-time.After(50 * time.Millisecond):
		}
	}

	var skipped string
	for skipped == "" {
		m := read(t, slow)
		if s, _ := m["data"].(string); strings.HasPrefix(s, "[…") && strings.HasSuffix(s, " lines skipped]") {
			skipped = s
		}
	}
}

func TestConsoleQueue(t *testing.T) {
	old := consoleQueueLines
	consoleQueueLines = 3
	t.Cleanup(func() { consoleQueueLines = old })
	q := newConsoleSub()
	for i := range 10 {
		q.push(encodeConsole("line", fmt.Sprint(i)), true)
	}
	var got []string
	for {
		b, ok, _ := q.pop()
		if !ok {
			break
		}
		var m map[string]string
		json.Unmarshal(b, &m)
		got = append(got, m["data"])
	}
	// Three fit, and the seven after them are counted where they were lost.
	if want := []string{"0", "1", "2", "[…7 lines skipped]"}; !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
	// Lines that come once there is room again follow the marker.
	q.push(encodeConsole("line", "again"), true)
	if b, _, _ := q.pop(); !strings.Contains(string(b), "again") {
		t.Errorf("popped %s", b)
	}
	// Other messages are not lost to a full queue, up to a limit.
	for range consoleQueueHard + 1 {
		q.push(encodeState("running"), false)
	}
	if _, _, over := q.pop(); !over {
		t.Error("a socket that takes nothing is kept")
	}
}

func TestConsoleClosesWithTheSession(t *testing.T) {
	e := newConsoleEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	consoleRecheck = 10 * time.Millisecond
	c := e.connect(t, "survival")
	read(t, c)
	if code, _ := e.b.do("POST", "/api/logout", nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
				t.Errorf("closed with %v", err)
			}
			return
		}
	}
}

func TestConsoleDropsAClientThatDoesNotAnswerPings(t *testing.T) {
	e := newConsoleEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	consolePing, consolePongWait = 10*time.Millisecond, 150*time.Millisecond

	// Reading answers the pings.
	alive := e.connect(t, "survival")
	go func() {
		time.Sleep(400 * time.Millisecond)
		send(t, alive, map[string]string{"type": "nonsense"})
	}()
	readUntil(t, alive, func(m map[string]any) bool { return m["type"] == "error" })

	// A client that never reads cannot.
	silent := e.connect(t, "survival")
	time.Sleep(600 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := silent.Read(ctx); err != nil {
			if ctx.Err() != nil {
				t.Fatal("the socket is still open")
			}
			return
		}
	}
}

func TestRateLimit(t *testing.T) {
	r := rateLimit{perSecond: 10}
	now := time.Unix(1000, 0)
	ok := 0
	for range 30 {
		if r.allow(now) {
			ok++
		}
	}
	if ok != 10 {
		t.Errorf("burst of %d", ok)
	}
	if !r.allow(now.Add(150 * time.Millisecond)) {
		t.Error("nothing refilled")
	}
	if r.allow(now.Add(150 * time.Millisecond)) {
		t.Error("refilled too much")
	}
}
