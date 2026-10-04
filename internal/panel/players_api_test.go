package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
	"github.com/coder/websocket"
)

const steamID1 = "76561198000000001"

// A started Minecraft server whose console has seen Steve join.
func newMinecraftGame(t *testing.T, before ...func(*appEnv)) (*appEnv, string) {
	t.Helper()
	e, id := newPlayerGame(t, before...)
	e.core.emit(id, "Done\n")
	e.waitState(t, "survival", "running")
	e.core.emit(id, "[12:00:00 INFO]: UUID of player Steve is 069a79f4-44e9-4726-a5be-fca90e38aaf5\n"+
		"[12:00:00 INFO]: Steve[/10.0.0.7:5555] logged in with entity id 4 at (0, 0, 0)\n"+
		"[12:00:00 INFO]: Steve joined the game\n"+
		"[12:00:03 INFO]: <Steve> hi\n")
	waitFor(t, func() bool {
		chat, _ := e.s.Store.PlayerChat(context.Background(), "survival", "", 0, 10)
		return len(chat) == 1
	})
	return e, id
}

const steveID = "069a79f4-44e9-4726-a5be-fca90e38aaf5"

func (e *appEnv) auditActions(t *testing.T, app string) []string {
	t.Helper()
	code, out := e.b.do("GET", "/api/games/"+app+"/audit", nil)
	if code != http.StatusOK {
		t.Fatalf("audit: %d %v", code, out)
	}
	var got []string
	for _, x := range out["entries"].([]any) {
		got = append(got, x.(map[string]any)["action"].(string))
	}
	return got
}

func TestPlayerRoutesNeedALogin(t *testing.T) {
	e := newAppEnv(t)
	out := &browser{t: t, h: e.b.h, ip: "198.51.100.8"}
	for _, r := range [][2]string{
		{"GET", "/api/games/g/players/online"}, {"GET", "/api/games/g/players"}, {"GET", "/api/games/g/players/x"},
		{"POST", "/api/games/g/players/x/kick"}, {"POST", "/api/games/g/players/x/ban"},
		{"POST", "/api/games/g/players/x/notes"}, {"DELETE", "/api/games/g/players/x/notes/1"},
		{"POST", "/api/games/g/players/x/op"}, {"POST", "/api/games/g/players/x/whitelist"},
		{"GET", "/api/games/g/chat"}, {"GET", "/api/games/g/reports"}, {"GET", "/api/games/g/bans"},
		{"DELETE", "/api/games/g/bans/1"}, {"GET", "/api/games/g/audit"},
		{"PUT", "/api/server/steam-key"}, {"DELETE", "/api/server/steam-key"},
	} {
		if code, _ := out.do(r[0], r[1], nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s without logging in: %d", r[0], r[1], code)
		}
	}
}

func TestMinecraftOnlineList(t *testing.T) {
	e, _ := newMinecraftGame(t)
	e.core.stdinHook = func(id, line string) {
		if line == "list\n" {
			e.core.emit(id, "[12:00:05 INFO]: There are 2/20 players online:\n[12:00:05 INFO]: Steve, Newbie\n")
		}
	}
	code, out := e.b.do("GET", "/api/games/survival/players/online", nil)
	if code != http.StatusOK || out["running"] != true || out["source"] != "console" || out["error"] != "" {
		t.Fatalf("%d %v", code, out)
	}
	list := out["players"].([]any)
	steve, newbie := list[0].(map[string]any), list[1].(map[string]any)
	if len(list) != 2 || steve["id"] != steveID || steve["ip"] != "10.0.0.7" || newbie["id"] != "name:Newbie" {
		t.Errorf("players %v", list)
	}

	// A console that says nothing is an error in the answer, not a failure.
	old := minecraftListWait
	minecraftListWait = 50 * time.Millisecond
	t.Cleanup(func() { minecraftListWait = old })
	e.core.stdinHook = nil
	code, out = e.b.do("GET", "/api/games/survival/players/online", nil)
	if code != http.StatusOK || out["error_code"] != "players.no_answer" || len(out["players"].([]any)) != 0 {
		t.Errorf("%d %v", code, out)
	}
}

func TestMinecraftActions(t *testing.T) {
	e, id := newMinecraftGame(t)
	base := "/api/games/survival/players/" + steveID

	if code, out := e.b.do("POST", base+"/kick", map[string]string{"reason": "spam \"now\"\nplease"}); code != http.StatusNoContent {
		t.Fatalf("kick: %d %v", code, out)
	}
	if code, _ := e.b.do("POST", base+"/op", map[string]bool{"on": true}); code != http.StatusNoContent {
		t.Fatal("op")
	}
	if code, _ := e.b.do("POST", base+"/op", map[string]bool{"on": false}); code != http.StatusNoContent {
		t.Fatal("deop")
	}
	if code, _ := e.b.do("POST", base+"/whitelist", map[string]bool{"on": true}); code != http.StatusNoContent {
		t.Fatal("whitelist")
	}
	if code, _ := e.b.do("POST", base+"/whitelist", map[string]bool{"on": false}); code != http.StatusNoContent {
		t.Fatal("unwhitelist")
	}
	want := []string{"kick Steve spam 'now'please\n", "op Steve\n", "deop Steve\n", "whitelist add Steve\n", "whitelist remove Steve\n"}
	if got := e.stdin(id); !slices.Equal(got, want) {
		t.Errorf("console got %q", got)
	}
	if got := e.auditActions(t, "survival"); !slices.Equal(got, []string{"whitelist_remove", "whitelist_add", "deop", "op", "kick"}) {
		t.Errorf("audit %v", got)
	}

	// A ban that is for good, and then the player is shown as banned.
	if code, out := e.b.do("POST", base+"/ban", map[string]any{"reason": "cheating", "minutes": 0}); code != http.StatusNoContent {
		t.Fatalf("ban: %d %v", code, out)
	}
	if code, out := e.b.do("POST", base+"/ban", map[string]any{"reason": "again", "minutes": 0}); code != http.StatusConflict || out["code"] != "players.already_banned" {
		t.Errorf("second ban: %d %v", code, out)
	}
	_, out := e.b.do("GET", "/api/games/survival/bans", nil)
	bans := out["bans"].([]any)
	ban := bans[0].(map[string]any)
	if len(bans) != 1 || ban["active"] != true || ban["expires_at"] != nil || ban["by"] != "a@example.com" || ban["reason"] != "cheating" {
		t.Fatalf("bans %v", bans)
	}
	_, out = e.b.do("GET", "/api/games/survival/players/"+steveID, nil)
	if out["player"].(map[string]any)["banned"] != true || len(out["bans"].([]any)) != 1 || len(out["sessions"].([]any)) != 1 {
		t.Errorf("detail %v", out)
	}
	url := fmt.Sprintf("/api/games/survival/bans/%v", ban["id"])
	if code, out := e.b.do("DELETE", url, nil); code != http.StatusNoContent {
		t.Fatalf("unban: %d %v", code, out)
	}
	if code, out := e.b.do("DELETE", url, nil); code != http.StatusConflict || out["code"] != "players.ban_over" {
		t.Errorf("unban twice: %d %v", code, out)
	}
	if got := e.stdin(id); got[len(got)-2] != "ban Steve cheating\n" || got[len(got)-1] != "pardon Steve\n" {
		t.Errorf("console got %q", got)
	}
	_, out = e.b.do("GET", "/api/games/survival/bans?all=1", nil)
	lifted := out["bans"].([]any)[0].(map[string]any)
	if lifted["active"] != false || lifted["lifted_by"] != "a@example.com" {
		t.Errorf("lifted ban %v", lifted)
	}

	// Rust-only and unknown things are refused.
	if code, out := e.b.do("POST", "/api/games/survival/players/nobody/kick", map[string]string{}); code != http.StatusNotFound || out["code"] != "players.not_found" {
		t.Errorf("unknown player: %d %v", code, out)
	}
	if code, out := e.b.do("POST", base+"/ban", map[string]any{"minutes": -1}); code != http.StatusBadRequest {
		t.Errorf("negative ban: %d %v", code, out)
	}
}

func TestActionsNeedARunningServer(t *testing.T) {
	e, _ := newMinecraftGame(t)
	e.power(t, "survival", "kill")
	e.waitState(t, "survival", "stopped")
	base := "/api/games/survival/players/" + steveID
	for _, c := range [][2]string{{"POST", base + "/kick"}, {"POST", base + "/ban"}, {"POST", base + "/op"}} {
		if code, out := e.b.do(c[0], c[1], map[string]any{}); code != http.StatusConflict || out["code"] != "players.not_running" {
			t.Errorf("%s: %d %v", c[1], code, out)
		}
	}
	code, out := e.b.do("GET", "/api/games/survival/players/online", nil)
	if code != http.StatusOK || out["running"] != false || out["error"] != "" {
		t.Errorf("online: %d %v", code, out)
	}
	// Notes and history do not need the game.
	if code, _ := e.b.do("POST", base+"/notes", map[string]string{"tag": "watch", "note": "ok"}); code != http.StatusNoContent {
		t.Errorf("note while stopped: %d", code)
	}
}

func TestNotes(t *testing.T) {
	e, _ := newMinecraftGame(t)
	base := "/api/games/survival/players/" + steveID
	for _, bad := range []map[string]string{{"tag": "nope", "note": "x"}, {"tag": "", "note": "  "}, {"tag": "", "note": strings.Repeat("x", 1001)}} {
		if code, out := e.b.do("POST", base+"/notes", bad); code != http.StatusBadRequest || out["code"] != "players.bad_note" {
			t.Errorf("%v: %d %v", bad, code, out)
		}
	}
	e.b.do("POST", base+"/notes", map[string]string{"tag": "watch", "note": "first"})
	e.b.do("POST", base+"/notes", map[string]string{"tag": "", "note": "second"})
	_, out := e.b.do("GET", base, nil)
	notes := out["notes"].([]any)
	first := notes[1].(map[string]any)
	if len(notes) != 2 || notes[0].(map[string]any)["note"] != "second" || first["tag"] != "watch" || first["by"] != "a@example.com" {
		t.Fatalf("notes %v", notes)
	}
	if out["player"].(map[string]any)["notes"] != 2.0 {
		t.Errorf("count %v", out["player"])
	}
	url := fmt.Sprintf("%s/notes/%v", base, first["id"])
	if code, _ := e.b.do("DELETE", url, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, out := e.b.do("DELETE", url, nil); code != http.StatusNotFound || out["code"] != "players.note_not_found" {
		t.Errorf("delete twice: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/games/survival/players/nobody/notes", map[string]string{"note": "x"}); code != http.StatusNotFound {
		t.Errorf("note on nobody: %d %v", code, out)
	}
	if got := e.auditActions(t, "survival"); !slices.Equal(got, []string{"note_delete", "note_add", "note_add"}) {
		t.Errorf("audit %v", got)
	}
}

func TestChatPagingAndReports(t *testing.T) {
	e, _ := newMinecraftGame(t)
	ctx := context.Background()
	var events []players.Event
	at := time.Unix(1_800_000_000, 0)
	for i := range 5 {
		ch := "global"
		if i%2 == 1 {
			ch = "team"
		}
		events = append(events, players.Event{Kind: players.Chat, PlayerID: "p2", Name: "Alex", Channel: ch, Text: fmt.Sprintf("m%d", i), At: at.Add(time.Duration(i) * time.Second)})
	}
	for i, reporter := range []string{"r1", "r2", "r1"} {
		events = append(events, players.Event{Kind: players.Report, PlayerID: reporter, Name: "Rep" + reporter, Target: "t1", TargetName: "Cheater", Reason: "cheat", Text: "he flies", At: at.Add(time.Duration(i) * time.Minute)})
	}
	events = append(events, players.Event{Kind: players.Report, PlayerID: "r3", Name: "Rep3", Target: "t2", TargetName: "Other", At: at.Add(time.Hour)})
	if err := e.s.Store.RecordPlayerEvents(ctx, "survival", events); err != nil {
		t.Fatal(err)
	}

	_, out := e.b.do("GET", "/api/games/survival/chat?player=p2&limit=2", nil)
	page := out["messages"].([]any)
	if len(page) != 2 || page[0].(map[string]any)["text"] != "m4" {
		t.Fatalf("page %v", page)
	}
	before := page[1].(map[string]any)["id"]
	_, out = e.b.do("GET", fmt.Sprintf("/api/games/survival/chat?player=p2&limit=10&before=%v", before), nil)
	if rest := out["messages"].([]any); len(rest) != 3 || rest[0].(map[string]any)["text"] != "m2" {
		t.Errorf("next page %v", rest)
	}
	_, out = e.b.do("GET", "/api/games/survival/chat?channel=team", nil)
	if team := out["messages"].([]any); len(team) != 2 {
		t.Errorf("team chat %v", team)
	}
	_, out = e.b.do("GET", "/api/games/survival/chat?search=m3", nil)
	if found := out["messages"].([]any); len(found) != 1 {
		t.Errorf("search %v", found)
	}

	_, out = e.b.do("GET", "/api/games/survival/reports", nil)
	targets := out["targets"].([]any)
	t2, t1 := targets[0].(map[string]any), targets[1].(map[string]any)
	if len(targets) != 2 || t2["target_id"] != "t2" || t1["count"] != 3.0 || t1["reporters"] != 2.0 || t1["target_name"] != "Cheater" || len(t1["items"].([]any)) != 3 {
		t.Errorf("targets %v", targets)
	}
	_, out = e.b.do("GET", "/api/games/survival/players/t1", nil)
	if code, _ := e.b.do("GET", "/api/games/survival/players/t1", nil); code != http.StatusNotFound {
		t.Errorf("t1 has no row of its own: %v", out)
	}
	_, out = e.b.do("GET", "/api/games/survival/players?search=Alex", nil)
	if out["total"] != 1.0 || len(out["players"].([]any)) != 1 {
		t.Errorf("search players %v", out)
	}
}

// A clock the tests move.
type moveable struct {
	mu   sync.Mutex
	base time.Time
	add  time.Duration
}

func (m *moveable) now() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.base.Add(m.add)
}

func (m *moveable) advance(d time.Duration) {
	m.mu.Lock()
	m.add += d
	m.mu.Unlock()
}

func TestTimedBanIsLiftedWhenItRunsOut(t *testing.T) {
	old := banCheckEvery
	banCheckEvery = 5 * time.Millisecond
	t.Cleanup(func() { banCheckEvery = old })
	// Real time moving on keeps the session valid; only the jump is ours.
	clock := &moveable{base: time.Now()}
	e, id := newMinecraftGame(t, func(e *appEnv) { clock.base = e.s.now(); e.s.Now = clock.now })
	base := "/api/games/survival/players/" + steveID

	if code, out := e.b.do("POST", base+"/ban", map[string]any{"reason": "grief", "minutes": 30}); code != http.StatusNoContent {
		t.Fatalf("ban: %d %v", code, out)
	}
	time.Sleep(50 * time.Millisecond)
	if slices.Contains(e.stdin(id), "pardon Steve\n") {
		t.Fatal("lifted early")
	}
	clock.advance(31 * time.Minute)
	waitFor(t, func() bool { return slices.Contains(e.stdin(id), "pardon Steve\n") })
	waitFor(t, func() bool {
		bans, _ := e.s.Store.PlayerBans(context.Background(), "survival", "", true, e.s.now())
		return len(bans) == 1 && !bans[0].LiftedAt.IsZero()
	})
	bans, _ := e.s.Store.PlayerBans(context.Background(), "survival", "", true, e.s.now())
	if bans[0].LiftedBy != "" {
		t.Errorf("lifted by %q", bans[0].LiftedBy)
	}
	// Nothing is left to watch, so the keeper is gone.
	waitFor(t, func() bool {
		e.s.banKeepers.mu.Lock()
		defer e.s.banKeepers.mu.Unlock()
		return len(e.s.banKeepers.m) == 0
	})
	if got := e.auditActions(t, "survival"); got[0] != "ban_expired" {
		t.Errorf("audit %v", got)
	}
}

func TestExpiredBanIsLiftedWhenTheServerStarts(t *testing.T) {
	e, _ := newMinecraftGame(t)
	ctx := context.Background()
	e.power(t, "survival", "kill")
	e.waitState(t, "survival", "stopped")
	e.s.Store.AddPlayerBan(ctx, "survival", store.PlayerBan{PlayerID: steveID, Name: "Steve", CreatedAt: e.s.now().Add(-2 * time.Hour), ExpiresAt: e.s.now().Add(-time.Hour)})
	e.s.Store.AddPlayerBan(ctx, "survival", store.PlayerBan{PlayerID: "p2", Name: "Alex", CreatedAt: e.s.now(), ExpiresAt: e.s.now().Add(time.Hour)})

	if code, out := e.power(t, "survival", "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	if len(e.stdin(id)) != 0 {
		t.Fatal("told the game before it was ready")
	}
	e.core.emit(id, "Done\n")
	waitFor(t, func() bool { return slices.Contains(e.stdin(id), "pardon Steve\n") })
	waitFor(t, func() bool {
		bans, _ := e.s.Store.PlayerBans(ctx, "survival", "", false, e.s.now())
		return len(bans) == 1 && bans[0].PlayerID == "p2"
	})
	if slices.Contains(e.stdin(id), "pardon Alex\n") {
		t.Error("lifted a ban that had not run out")
	}
}

func TestNoKeeperWithoutTimedBans(t *testing.T) {
	e, _ := newMinecraftGame(t)
	e.b.do("POST", "/api/games/survival/players/"+steveID+"/ban", map[string]any{"minutes": 0})
	time.Sleep(20 * time.Millisecond)
	e.s.banKeepers.mu.Lock()
	defer e.s.banKeepers.mu.Unlock()
	if len(e.s.banKeepers.m) != 0 {
		t.Error("a keeper runs for a ban that never ends")
	}
}

const rustTestEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "Rust Test",
	"docker_images": {"Rust": "ghcr.io/example/rust:1"},
	"startup": "./run +rcon.port {{RCON_PORT}} +rcon.password {{RCON_PASS}}",
	"config": {"stop": "quit", "startup": "{\"done\": \"Server startup complete\"}", "files": "{}"},
	"scripts": {"installation": {"script": "#!/bin/bash\necho installing\n", "container": "ghcr.io/example/installer:latest", "entrypoint": "bash"}},
	"variables": [
		{"name": "Rcon port", "env_variable": "RCON_PORT", "default_value": "28016", "user_viewable": true, "user_editable": true, "rules": "required|string"},
		{"name": "Rcon password", "env_variable": "RCON_PASS", "default_value": "rconpass-12345", "user_viewable": true, "user_editable": true, "rules": "required|string"}
	]
}`

// fakeRust is a WebRCON server. It records the commands it gets.
type fakeRust struct {
	mu       sync.Mutex
	cmds     []string
	conns    int
	live     int
	silent   bool
	hostport string
}

func newFakeRust(t *testing.T, password string, playerlist string) *fakeRust {
	t.Helper()
	f := &fakeRust{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+password {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		f.mu.Lock()
		f.conns++
		f.live++
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.live--
			f.mu.Unlock()
		}()
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var req struct {
				Identifier int
				Message    string
			}
			json.Unmarshal(data, &req)
			f.mu.Lock()
			f.cmds = append(f.cmds, req.Message)
			silent := f.silent
			f.mu.Unlock()
			if silent {
				continue
			}
			reply := "ok"
			if req.Message == "playerlist" {
				reply = playerlist
			}
			b, _ := json.Marshal(map[string]any{"Identifier": req.Identifier, "Message": reply})
			c.Write(r.Context(), websocket.MessageText, b)
		}
	}))
	t.Cleanup(srv.Close)
	f.hostport = strings.TrimPrefix(srv.URL, "http://")
	return f
}

func (f *fakeRust) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.cmds)
}

func newRustGame(t *testing.T, f func(*appEnv) *fakeRust) (*appEnv, *fakeRust) {
	t.Helper()
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["rust"] = rustTestEgg
	rust := f(e)
	e.s.PlayerAddr = func(netip.Addr, int) string { return rust.hostport }
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "rusty", "egg": "rust", "memory_mb": 2048, "ports": 2}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "rusty")
	e.power(t, "rusty", "start")
	e.settle(t, "rusty")
	e.core.emit(e.liveContainer(t, "rusty"), "Server startup complete\n")
	e.waitState(t, "rusty", "running")
	return e, rust
}

func TestRustOnlineListAndActions(t *testing.T) {
	list := `[{"SteamID":"` + steamID1 + `","DisplayName":"Ann","Ping":31,"Address":"203.0.113.9:40000","ConnectedSeconds":95.2}]`
	e, rust := newRustGame(t, func(*appEnv) *fakeRust { return newFakeRust(t, "rconpass-12345", list) })
	ctx := context.Background()
	e.s.Store.RecordPlayerEvents(ctx, "rusty", []players.Event{{Kind: players.Join, PlayerID: steamID1, Name: "Ann", IP: "203.0.113.9"}})

	code, out := e.b.do("GET", "/api/games/rusty/players/online", nil)
	if code != http.StatusOK || out["source"] != "rcon" || out["error"] != "" {
		t.Fatalf("%d %v", code, out)
	}
	p := out["players"].([]any)[0].(map[string]any)
	if p["id"] != steamID1 || p["ip"] != "203.0.113.9" || p["ping"] != 31.0 || p["connected_seconds"] != 95.0 || p["steam"] != nil {
		t.Errorf("player %v", p)
	}
	// Nothing stays connected between requests.
	waitFor(t, func() bool {
		rust.mu.Lock()
		defer rust.mu.Unlock()
		return rust.live == 0
	})

	base := "/api/games/rusty/players/" + steamID1
	if code, out := e.b.do("POST", base+"/kick", map[string]string{"reason": "say \"bye\"; quit"}); code != http.StatusNoContent {
		t.Fatalf("kick: %d %v", code, out)
	}
	if code, out := e.b.do("POST", base+"/ban", map[string]any{"reason": "cheats", "minutes": 0}); code != http.StatusNoContent {
		t.Fatalf("ban: %d %v", code, out)
	}
	_, out = e.b.do("GET", "/api/games/rusty/bans", nil)
	banID := out["bans"].([]any)[0].(map[string]any)["id"]
	if code, out := e.b.do("DELETE", fmt.Sprintf("/api/games/rusty/bans/%v", banID), nil); code != http.StatusNoContent {
		t.Fatalf("unban: %d %v", code, out)
	}
	want := []string{
		"playerlist",
		`kick "` + steamID1 + `" "say 'bye', quit"`,
		`banid "` + steamID1 + `" "Ann" "cheats"`, `kick "` + steamID1 + `" "cheats"`, "server.writecfg",
		`unban "` + steamID1 + `"`, "server.writecfg",
	}
	if got := rust.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n%q\nwant\n%q", got, want)
	}
	// op and whitelist are Minecraft's.
	if code, out := e.b.do("POST", base+"/op", map[string]bool{"on": true}); code != http.StatusBadRequest || out["code"] != "players.unsupported" {
		t.Errorf("op on Rust: %d %v", code, out)
	}
	// A player the server listed before the log was read can be acted on, but
	// only by a real SteamID.
	if code, _ := e.b.do("POST", "/api/games/rusty/players/76561198000000042/kick", map[string]string{}); code != http.StatusNoContent {
		t.Errorf("unrecorded player: %d", code)
	}
	if code, _ := e.b.do("POST", "/api/games/rusty/players/not-an-id/kick", map[string]string{}); code != http.StatusNotFound {
		t.Errorf("bad id: %d", code)
	}
}

func TestRustRCONDownIsAnErrorInTheAnswer(t *testing.T) {
	e, rust := newRustGame(t, func(*appEnv) *fakeRust { return newFakeRust(t, "other-password", "[]") })
	code, out := e.b.do("GET", "/api/games/rusty/players/online", nil)
	if code != http.StatusOK || out["running"] != true || out["error_code"] != "players.no_answer" || len(out["players"].([]any)) != 0 {
		t.Errorf("%d %v", code, out)
	}
	if strings.Contains(fmt.Sprint(out), "rconpass") {
		t.Errorf("the password is in the answer: %v", out)
	}
	e.s.Store.RecordPlayerEvents(context.Background(), "rusty", []players.Event{{Kind: players.Join, PlayerID: steamID1, Name: "Ann"}})
	code, out = e.b.do("POST", "/api/games/rusty/players/"+steamID1+"/ban", map[string]any{"minutes": 0})
	if code != http.StatusBadGateway || out["code"] != "players.no_answer" {
		t.Errorf("ban with the console down: %d %v", code, out)
	}
	if bans, _ := e.s.Store.PlayerBans(context.Background(), "rusty", "", true, e.s.now()); len(bans) != 0 {
		t.Error("a ban was saved that never reached the game")
	}
	_ = rust
}

func TestRustRCONTimesOut(t *testing.T) {
	e, _ := newRustGame(t, func(*appEnv) *fakeRust {
		f := newFakeRust(t, "rconpass-12345", "[]")
		f.silent = true
		return f
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	pg, err := e.s.loadPlayerGame(ctx, "rusty")
	if err != nil {
		t.Fatal(err)
	}
	_, ip, _ := e.s.runningAddr(ctx, "rusty")
	if _, err := e.s.rustOnline(ctx, pg, ip); err == nil {
		t.Error("no error from a console that never answers")
	}
}

func TestQueryGamesListWhoIsOn(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 100)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n == 9 && string(buf[5:9]) == "\xff\xff\xff\xff" {
				pc.WriteTo([]byte("\xff\xff\xff\xffA\x01\x02\x03\x04"), from)
				continue
			}
			pc.WriteTo([]byte("\xff\xff\xff\xffD\x01\x00Zed\x00\x05\x00\x00\x00\x00\x00\x20\x42"), from)
		}
	}()
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["counter-strike-2"] = consoleEgg
	e.s.PlayerAddr = func(_ netip.Addr, port int) string { return pc.LocalAddr().String() }
	e.newGame(t, "cs", "counter-strike-2", nil)
	e.startGame(t, "cs")

	_, out := e.b.do("GET", "/api/games/cs", nil)
	if out["players"] != "list" || out["players_game"] != "" {
		t.Errorf("capability %v %v", out["players"], out["players_game"])
	}
	code, out := e.b.do("GET", "/api/games/cs/players/online", nil)
	list := out["players"].([]any)
	if code != http.StatusOK || out["source"] != "query" || len(list) != 1 || list[0].(map[string]any)["name"] != "Zed" || list[0].(map[string]any)["connected_seconds"] != 40.0 {
		t.Errorf("%d %v", code, out)
	}
	// The history, chat and actions are not there.
	if code, out := e.b.do("POST", "/api/games/cs/players/name:Zed/kick", map[string]string{}); code != http.StatusBadRequest || out["code"] != "players.unsupported" {
		t.Errorf("kick: %d %v", code, out)
	}
}

func TestPlayersCapabilityInGameJSON(t *testing.T) {
	e, _ := newMinecraftGame(t)
	_, out := e.b.do("GET", "/api/games/survival", nil)
	if out["players"] != "full" || out["players_game"] != "minecraft" {
		t.Errorf("capability %v %v", out["players"], out["players_game"])
	}
}

func fakeSteamWeb(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("key") != strings.Repeat("ab", 16) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		if strings.Contains(r.URL.Path, "GetPlayerBans") {
			io := `{"players":[{"SteamId":"` + steamID1 + `","NumberOfVACBans":2,"DaysSinceLastBan":9,"NumberOfGameBans":0,"CommunityBanned":false}]}`
			w.Write([]byte(io))
			return
		}
		w.Write([]byte(`{"response":{"players":[{"steamid":"` + steamID1 + `","avatarfull":"https://x/a.jpg","profileurl":"https://steamcommunity.com/id/ann","timecreated":1400000000}]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSteamKey(t *testing.T) {
	list := `[{"SteamID":"` + steamID1 + `","DisplayName":"Ann","Ping":1,"Address":"1.2.3.4:5","ConnectedSeconds":1}]`
	e, _ := newRustGame(t, func(*appEnv) *fakeRust { return newFakeRust(t, "rconpass-12345", list) })
	steam := fakeSteamWeb(t)
	e.s.SteamWebAPI = steam.URL
	key := strings.Repeat("ab", 16)

	if code, out := e.b.do("PUT", "/api/server/steam-key", map[string]string{"key": "short"}); code != http.StatusBadRequest || out["code"] != "players.steam_key_shape" {
		t.Errorf("short key: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", "/api/server/steam-key", map[string]string{"key": strings.Repeat("cd", 16)}); code != http.StatusBadRequest || out["code"] != "players.steam_key_rejected" {
		t.Errorf("wrong key: %d %v", code, out)
	}
	if _, err := e.s.Store.SteamKey(context.Background()); err == nil {
		t.Fatal("a rejected key was saved")
	}
	_, out := e.b.do("GET", "/api/server", nil)
	if out["steam_key_set"] != false {
		t.Errorf("server %v", out)
	}
	if code, out := e.b.do("PUT", "/api/server/steam-key", map[string]string{"key": key}); code != http.StatusNoContent {
		t.Fatalf("put: %d %v", code, out)
	}
	sealed, err := e.s.Store.SteamKey(context.Background())
	if err != nil || strings.Contains(string(sealed), key) {
		t.Errorf("stored %q, %v", sealed, err)
	}
	_, out = e.b.do("GET", "/api/server", nil)
	if out["steam_key_set"] != true {
		t.Errorf("server %v", out)
	}

	_, out = e.b.do("GET", "/api/games/rusty/players/online", nil)
	info, _ := out["players"].([]any)[0].(map[string]any)["steam"].(map[string]any)
	if info["vac_bans"] != 2.0 || info["days_since_last_ban"] != 9.0 || info["profile_url"] == "" || info["account_created"] == nil {
		t.Errorf("steam info %v", info)
	}

	if code, out := e.b.do("DELETE", "/api/server/steam-key", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, out = e.b.do("GET", "/api/games/rusty/players/online", nil)
	if out["players"].([]any)[0].(map[string]any)["steam"] != nil {
		t.Error("steam info after the key was removed")
	}
	_, out = e.b.do("GET", "/api/server", nil)
	if out["steam_key_set"] != false {
		t.Errorf("server %v", out)
	}
}

func TestSteamKeyChecksNeedAConfirmation(t *testing.T) {
	e := newAppEnv(t)
	e.s.SteamWebAPI = fakeSteamWeb(t).URL
	later := e.s.now().Add(confirmWindow + time.Minute)
	e.s.Now = func() time.Time { return later }
	if code, out := e.b.do("PUT", "/api/server/steam-key", map[string]string{"key": strings.Repeat("ab", 16)}); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("%d %v", code, out)
	}
}
