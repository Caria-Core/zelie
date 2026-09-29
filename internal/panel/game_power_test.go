package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/store"
)

// A game that says when it is ready, stops on a console command, and has
// config files to edit.
const consoleEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "Console Game",
	"docker_images": {"Java 21": "ghcr.io/example/java:21"},
	"startup": "java -Xmx{{SERVER_MEMORY}}M -jar server.jar --port {{server.build.default.port}} --world {{WORLD}}",
	"config": {
		"stop": "stop",
		"startup": "{\"done\": [\")! For help, type \", \"regex:^Ready on \\\\d+$\"]}",
		"files": "{\"server.properties\": {\"parser\": \"properties\", \"find\": {\"server-port\": \"{{server.build.default.port}}\", \"level-name\": \"{{env.WORLD}}\", \"online-mode\": {\"true\": \"false\"}}}, \"config/extra.yml\": {\"parser\": \"yaml\", \"find\": {\"net.port\": \"{{SERVER_PORT}}\"}}}"
	},
	"scripts": {"installation": {"script": "#!/bin/bash\necho installing\n", "container": "ghcr.io/example/installer:latest", "entrypoint": "bash"}},
	"variables": [
		{"name": "World", "env_variable": "WORLD", "default_value": "my world", "user_viewable": true, "user_editable": true, "rules": "required|string"}
	]
}`

const consoleEggURL = "https://example.com/console.json"

func newPowerEnv(t *testing.T) *appEnv {
	t.Helper()
	e, eggs := newGameEnv(t)
	eggs.files[consoleEggURL] = consoleEgg
	ctx, cancel := context.WithCancel(context.Background())
	e.s.ctx = ctx
	oldGrace, oldRetry := gameStopGrace, watchRetry
	gameStopGrace, watchRetry = 2*time.Second, 5*time.Millisecond
	t.Cleanup(func() { gameStopGrace, watchRetry = oldGrace, oldRetry })
	t.Cleanup(func() {
		cancel()
		e.s.watchers.Wait()
	})
	return e
}

// newGame creates a game server from the egg and waits for its install.
func (e *appEnv) newGame(t *testing.T, name, eggURL string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"name": name, "egg_url": eggURL, "memory_mb": 2048, "ports": 2}
	for k, v := range extra {
		body[k] = v
	}
	if code, out := e.b.do("POST", "/api/games", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if d := e.install(t, name); d.State != store.DeployInstalled && d.State != store.DeployFailed {
		t.Fatalf("install %+v", d)
	}
}

func (e *appEnv) power(t *testing.T, name, action string) (int, map[string]any) {
	t.Helper()
	return e.b.do("POST", "/api/games/"+name+"/power", map[string]string{"action": action})
}

func (e *appEnv) gameState(t *testing.T, name string) string {
	t.Helper()
	code, out := e.b.do("GET", "/api/games/"+name, nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, out)
	}
	return out["state"].(string)
}

// waitState waits for the server's state to become want.
func (e *appEnv) waitState(t *testing.T, name, want string) {
	t.Helper()
	var got string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if got = e.gameState(t, name); got == want {
			return
		}
	}
	t.Fatalf("state is %q, want %q", got, want)
}

// liveContainer is the name of the server's newest container.
func (e *appEnv) liveContainer(t *testing.T, name string) string {
	t.Helper()
	live, err := e.s.Store.LiveDeployment(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s-%d", name, live.ID)
}

func (e *appEnv) running(app string) []string {
	list, _ := e.core.List(context.Background())
	var out []string
	for _, c := range list {
		if c.App == app && c.State == "running" {
			out = append(out, c.ID)
		}
	}
	return out
}

func TestStartRunsTheServerLikeWings(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, map[string]any{"variables": map[string]string{"WORLD": "my world"}})

	if e.gameState(t, "survival") != "stopped" {
		t.Errorf("a server that never ran is %q", e.gameState(t, "survival"))
	}
	code, out := e.power(t, "survival", "start")
	if code != http.StatusAccepted || out["state"] != "starting" {
		t.Fatalf("start: %d %v", code, out)
	}
	d := e.settle(t, "survival")
	if d.State != store.DeployLive || d.Cause != store.CauseRestart {
		t.Fatalf("deployment %+v", d)
	}
	id := e.liveContainer(t, "survival")
	spec, ok := e.core.games[id]
	if !ok {
		t.Fatalf("no server container %s: %v", id, e.core.containers)
	}

	// The image's own entrypoint runs, as the ordinary user, with its
	// console open and the volume as its home.
	if spec.Image != "ghcr.io/example/java:21" || len(spec.Args) != 0 || spec.App != "survival" || spec.Network != "survival" {
		t.Errorf("spec %+v", spec)
	}
	if spec.User == nil || spec.User.UID != 988 || spec.User.GID != 988 || spec.WorkDir != "/home/container" || !spec.Stdin {
		t.Errorf("user, directory or console: %+v", spec)
	}
	vols, _ := e.s.Store.Volumes(context.Background(), "survival")
	if len(spec.Volumes) != 1 || spec.Volumes[0].Target != "/home/container" || spec.Volumes[0].Name != vols[0].Name {
		t.Errorf("volumes %+v", spec.Volumes)
	}
	if spec.MemoryBytes != gameMemory(2048) || spec.CPUs != 1 || spec.Pids != defaultPids {
		t.Errorf("limits %d %v %d", spec.MemoryBytes, spec.CPUs, spec.Pids)
	}
	env := strings.Join(spec.Env, "\n")
	for _, want := range []string{
		"STARTUP=java -Xmx2048M -jar server.jar --port 25565 --world my world",
		"HOME=/home/container", "SERVER_PORT=25565", "SERVER_MEMORY=2048", "SERVER_IP=0.0.0.0", "WORLD=my world", "P_SERVER_UUID=survival",
	} {
		if !strings.Contains("\n"+env+"\n", "\n"+want+"\n") {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}

	// Both ports, TCP and UDP, go to the same port in the container.
	var got []string
	for _, f := range e.core.forwards["survival"] {
		got = append(got, fmt.Sprintf("%d/%s>%d", f.Port, f.Proto, f.Target))
	}
	if want := []string{"25565/tcp>25565", "25565/udp>25565", "25566/tcp>25566", "25566/udp>25566"}; !slices.Equal(got, want) {
		t.Errorf("forwards %v", got)
	}

	// The files were made ready first, with the values filled in, and
	// nothing ran on the volume then.
	if len(e.core.prepares) != 1 || e.core.preparedBusy {
		t.Fatalf("prepared %+v, while busy: %v", e.core.prepares, e.core.preparedBusy)
	}
	p := e.core.prepares[0]
	if p.Volume != vols[0].Name || p.Req.UID != 988 || p.Req.GID != 988 || len(p.Req.Files) != 2 {
		t.Fatalf("prepare %+v", p)
	}
	props := p.Req.Files[0]
	if props.Path != "server.properties" || props.Parser != "properties" || len(props.Changes) != 3 ||
		props.Changes[0].Value != "25565" || props.Changes[1].Value != "my world" || props.Changes[2].IfValue != "true" || props.Changes[2].Value != "false" {
		t.Errorf("server.properties: %+v", props)
	}
	if extra := p.Req.Files[1]; extra.Path != "config/extra.yml" || extra.Changes[0].Key != "net.port" || extra.Changes[0].Value != "25565" {
		t.Errorf("extra.yml: %+v", extra)
	}

	// Until the game says it is ready, it is starting.
	if s := e.gameState(t, "survival"); s != "starting" {
		t.Errorf("state %q", s)
	}
	e.core.emit(id, "[12:00:00 INFO]: Preparing spawn area: 40%\n")
	time.Sleep(20 * time.Millisecond)
	if s := e.gameState(t, "survival"); s != "starting" {
		t.Errorf("state %q before the done line", s)
	}
	e.core.emit(id, "\x1b[32m[12:00:09 INFO]: Done (8.2s)! For help, type \"help\"\x1b[m\n")
	e.waitState(t, "survival", "running")

	// Starting a running server is refused, and so is a wrong action.
	if code, out := e.power(t, "survival", "start"); code != http.StatusConflict || out["code"] != "game.already_running" {
		t.Errorf("start twice: %d %v", code, out)
	}
	if code, out := e.power(t, "survival", "reboot"); code != http.StatusBadRequest || out["code"] != "game.bad_power" {
		t.Errorf("bad action: %d %v", code, out)
	}
	if code, out := e.power(t, "nothing", "start"); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("unknown server: %d %v", code, out)
	}
	if code, _ := e.b.do("POST", "/api/games/survival/power", map[string]any{"action": "start", "extra": 1}); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", code)
	}
}

func TestRegexDoneLine(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	// A line that only contains the regular expression's text is not it.
	e.core.emit(id, "log: Ready on 25565\n")
	time.Sleep(20 * time.Millisecond)
	if s := e.gameState(t, "survival"); s != "starting" {
		t.Errorf("state %q", s)
	}
	e.core.emit(id, "Ready on 25565\r\n")
	e.waitState(t, "survival", "running")
}

func TestLineSplitter(t *testing.T) {
	var seen []string
	w := &lineSplitter{emit: func(line string) { seen = append(seen, line) }}
	// Lines arrive in pieces, with colours and Windows line endings; the
	// colours stay for the interface to draw.
	for _, chunk := range []string{"first\nsec", "ond\x1b[1;32m line\x1b[0m\r\nDo", "ne\n\nafter\n"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write: %d %v", n, err)
		}
	}
	if want := []string{"first", "second\x1b[1;32m line\x1b[0m", "Done", "", "after"}; !slices.Equal(seen, want) {
		t.Errorf("seen %q", seen)
	}

	// A line with no end is cut and its rest dropped, without growing
	// without bound; the next line is whole.
	seen = nil
	for range 3 {
		w.Write(make([]byte, 40<<10))
		if len(w.rest) > consoleMaxLine {
			t.Fatalf("kept %d bytes", len(w.rest))
		}
	}
	w.Write([]byte("tail of it\nnext\n"))
	if len(seen) != 2 || len(seen[0]) != consoleMaxLine || seen[1] != "next" {
		t.Errorf("after a long line: %d lines %q", len(seen), seen)
	}

	// A long line that does end, and bytes that are not UTF-8.
	seen = nil
	w = &lineSplitter{emit: func(line string) { seen = append(seen, line) }}
	w.Write([]byte(strings.Repeat("é", consoleMaxLine) + "\n"))
	w.Write([]byte("bad \xff byte\n"))
	if len(seen) != 2 || len(seen[0]) > consoleMaxLine || !utf8.ValidString(seen[0]) || seen[1] != "bad \uFFFD byte" {
		t.Errorf("seen %d lines", len(seen))
	}
}

func TestStopUsesTheEggsCommand(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")

	code, out := e.power(t, "survival", "stop")
	if code != http.StatusAccepted || out["state"] != "stopping" {
		t.Fatalf("stop: %d %v", code, out)
	}
	e.settle(t, "survival")
	e.waitState(t, "survival", "stopped")
	if got := e.core.consoles[id]; len(got) != 1 || got[0] != "stop\n" {
		t.Errorf("console %q", got)
	}
	if len(e.core.signals[id]) != 0 {
		t.Errorf("signals %v", e.core.signals[id])
	}
	if list := e.running("survival"); len(list) != 0 {
		t.Errorf("still running: %v", list)
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); !a.Stopped {
		t.Error("the stop was not remembered")
	}

	// A stop the user asked for is not a crash: it stays down.
	before, _ := e.s.Store.Deployments(context.Background(), "survival", 100)
	e.s.superviseOnce(context.Background())
	e.s.deploys.wg.Wait()
	if after, _ := e.s.Store.Deployments(context.Background(), "survival", 100); len(after) != len(before) {
		t.Errorf("the supervisor started it again: %d deployments, was %d", len(after), len(before))
	}

	// Stopping a server that is down is fine, and it starts again.
	if code, out := e.power(t, "survival", "stop"); code != http.StatusAccepted || out["state"] != "stopped" {
		t.Errorf("stop again: %d %v", code, out)
	}
	e.settle(t, "survival")
	if code, _ := e.power(t, "survival", "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	e.settle(t, "survival")
	if list := e.running("survival"); len(list) != 1 {
		t.Errorf("running %v", list)
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); a.Stopped {
		t.Error("still marked as stopped")
	}
}

func TestStopUsesTheEggsSignal(t *testing.T) {
	e := newPowerEnv(t)
	// This egg stops with ^C and has no done line, so it counts as running
	// as soon as its container does.
	e.newGame(t, "rusty", "https://example.com/e.js", nil)
	e.power(t, "rusty", "start")
	e.settle(t, "rusty")
	id := e.liveContainer(t, "rusty")
	e.waitState(t, "rusty", "running")

	e.power(t, "rusty", "stop")
	e.settle(t, "rusty")
	e.waitState(t, "rusty", "stopped")
	if got := e.core.signals[id]; len(got) != 1 || got[0] != "SIGINT" {
		t.Errorf("signals %v", got)
	}
	if len(e.core.consoles[id]) != 0 {
		t.Errorf("console %q", e.core.consoles[id])
	}
}

func TestStopFallsBackToSIGTERM(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	// As after the core restarted: the console cannot be written to.
	e.core.mu.Lock()
	e.core.stdinErr = &core.Error{Status: http.StatusConflict, Code: "container.input_closed", Message: "closed"}
	e.core.mu.Unlock()
	e.power(t, "survival", "stop")
	e.settle(t, "survival")
	e.waitState(t, "survival", "stopped")
	if got := e.core.signals[id]; len(got) != 1 || got[0] != "SIGTERM" {
		t.Errorf("signals %v", got)
	}
}

func TestStopKillsAfterTheGracePeriod(t *testing.T) {
	e := newPowerEnv(t)
	gameStopGrace = 40 * time.Millisecond
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.mu.Lock()
	e.core.stubborn = true
	e.core.mu.Unlock()

	e.power(t, "survival", "stop")
	e.settle(t, "survival")
	e.waitState(t, "survival", "stopped")
	if got := e.core.consoles[id]; len(got) != 1 {
		t.Errorf("console %q", got)
	}
	if list := e.running("survival"); len(list) != 0 {
		t.Errorf("a server that ignored the command is still running: %v", list)
	}
}

func TestKill(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.mu.Lock()
	e.core.stubborn = true
	e.core.mu.Unlock()

	code, out := e.power(t, "survival", "kill")
	if code != http.StatusOK || out["state"] != "stopped" {
		t.Fatalf("kill: %d %v", code, out)
	}
	if got := e.core.signals[id]; !slices.Equal(got, []string{"SIGKILL"}) {
		t.Errorf("signals %v", got)
	}
	if len(e.core.consoles[id]) != 0 {
		t.Errorf("console %q, a kill does not ask", e.core.consoles[id])
	}
	if e.gameState(t, "survival") != "stopped" {
		t.Errorf("state %q", e.gameState(t, "survival"))
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); !a.Stopped {
		t.Error("the kill was not remembered")
	}
	// It stays down.
	before, _ := e.s.Store.Deployments(context.Background(), "survival", 100)
	e.s.superviseOnce(context.Background())
	e.s.deploys.wg.Wait()
	if after, _ := e.s.Store.Deployments(context.Background(), "survival", 100); len(after) != len(before) {
		t.Error("the supervisor started it after a kill")
	}
}

func TestRestartStopsBeforeItPreparesFiles(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	first := e.liveContainer(t, "survival")

	code, out := e.power(t, "survival", "restart")
	if code != http.StatusAccepted || out["state"] != "starting" {
		t.Fatalf("restart: %d %v", code, out)
	}
	e.settle(t, "survival")
	second := e.liveContainer(t, "survival")
	if first == second {
		t.Fatal("the same container")
	}
	if got := e.core.consoles[first]; len(got) != 1 || got[0] != "stop\n" {
		t.Errorf("the old server was not stopped with its command: %q", got)
	}
	if got := e.running("survival"); !slices.Equal(got, []string{second}) {
		t.Errorf("running %v", got)
	}
	if len(e.core.prepares) != 2 || e.core.preparedBusy {
		t.Errorf("prepared %d times, while busy: %v", len(e.core.prepares), e.core.preparedBusy)
	}
	// The old container is gone, and only the new one is followed.
	if _, ok := e.core.containers[first]; ok {
		t.Error("the old container is still there")
	}
	// A restart works on a stopped server too.
	e.power(t, "survival", "stop")
	e.settle(t, "survival")
	if code, _ := e.power(t, "survival", "restart"); code != http.StatusAccepted {
		t.Fatalf("restart a stopped server: %d", code)
	}
	e.settle(t, "survival")
	if len(e.running("survival")) != 1 {
		t.Error("not running")
	}
}

func TestCrashIsBroughtBackAndCounted(t *testing.T) {
	e := newPowerEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	first := e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")

	// The process exits by itself.
	e.crash(t, "survival")
	if s := e.gameState(t, "survival"); s != "crashed" {
		t.Errorf("state %q", s)
	}
	e.s.superviseOnce(context.Background())
	d := e.settle(t, "survival")
	if d.State != store.DeployLive || d.Cause != store.CauseRecover || d.ID == first.ID {
		t.Fatalf("recovery %+v", d)
	}
	if e.liveContainer(t, "survival") == id || len(e.running("survival")) != 1 {
		t.Errorf("running %v", e.running("survival"))
	}
	// It was made ready again, and told nobody it was asked to start.
	if len(e.core.prepares) != 2 {
		t.Errorf("prepared %d times", len(e.core.prepares))
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); !strings.Contains(b, "stopped by itself") {
		t.Errorf("the log does not say why:\n%s", b)
	}
	e.waitState(t, "survival", "starting")

	// Five within ten minutes and Zelie stops trying, which the server
	// says.
	for range 5 {
		now = now.Add(time.Minute)
		e.crash(t, "survival")
		e.s.superviseOnce(context.Background())
		e.s.deploys.wg.Wait()
	}
	_, out := e.b.do("GET", "/api/games/survival", nil)
	if m, _ := out["crashing"].(map[string]any); m["code"] != "app.gave_up" {
		t.Errorf("crashing %v", out["crashing"])
	}
	if out["state"] != "crashed" {
		t.Errorf("state %v", out["state"])
	}
	// The user starting it again clears that.
	if code, _ := e.power(t, "survival", "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d", code)
	}
	e.settle(t, "survival")
	if _, out := e.b.do("GET", "/api/games/survival", nil); out["crashing"] != nil {
		t.Errorf("crashing %v", out["crashing"])
	}
}

func TestAServerThatIsDownAfterARestartComesBack(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	// The machine restarted: nothing runs, and nobody stopped the server.
	e.core.mu.Lock()
	for id := range e.core.containers {
		delete(e.core.containers, id)
	}
	e.core.mu.Unlock()
	e.s.superviseOnce(context.Background())
	d := e.settle(t, "survival")
	if d.Cause != store.CauseRecover || d.State != store.DeployLive || len(e.running("survival")) != 1 {
		t.Errorf("deployment %+v, running %v", d, e.running("survival"))
	}
}

func TestStartIsRefusedWithoutAnInstall(t *testing.T) {
	e := newPowerEnv(t)
	e.core.installExit = 1
	e.newGame(t, "broken", consoleEggURL, nil)
	if code, out := e.power(t, "broken", "start"); code != http.StatusConflict || out["code"] != "game.not_installed" {
		t.Errorf("failed install: %d %v", code, out)
	}

	e.core.installExit = 0
	e.core.mu.Lock()
	e.core.installHang = true
	e.core.mu.Unlock()
	old := installTimeout
	installTimeout = time.Minute
	defer func() { installTimeout = old }()
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "slow", "egg_url": consoleEggURL}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if code, out := e.power(t, "slow", "start"); code != http.StatusConflict || out["code"] != "game.installing" {
		t.Errorf("running install: %d %v", code, out)
	}
	// Let it end, or the test waits for the install for a minute.
	e.s.deploys.cancel("slow")
	if len(e.core.games) != 0 {
		t.Error("a server started")
	}
}

func TestStartNeedsFilesThatCanBePrepared(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.core.mu.Lock()
	e.core.prepareErr = fmt.Errorf("disk full")
	e.core.mu.Unlock()
	e.power(t, "survival", "start")
	d := e.settle(t, "survival")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "game.prepare_failed" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
	if len(e.running("survival")) != 0 {
		t.Error("it started with files that were not ready")
	}

	// A note about a file is shown in the log, and the server starts.
	e.core.mu.Lock()
	e.core.prepareErr = nil
	e.core.prepareNote = []string{"config/extra.yml: net.port was skipped"}
	e.core.mu.Unlock()
	e.power(t, "survival", "start")
	d = e.settle(t, "survival")
	if d.State != store.DeployLive {
		t.Fatalf("deployment %+v", d)
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); !strings.Contains(b, "net.port was skipped") || !strings.Contains(b, "Startup command: java -Xmx2048M") {
		t.Errorf("log:\n%s", b)
	}
}

func TestDeleteStopsARunningServer(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	if code, _ := e.b.do("DELETE", "/api/apps/survival", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if len(e.core.containers) != 0 {
		t.Errorf("containers left: %v", e.core.containers)
	}
	if !slices.Contains(e.core.cleared, "survival") || len(e.core.forwards["survival"]) != 0 {
		t.Errorf("ports still open: %v", e.core.forwards)
	}
	if _, ok := e.s.gameRuns.get("survival"); ok {
		t.Error("the server is still tracked")
	}
	for name, kept := range e.core.volumes {
		if kept {
			t.Errorf("volume %s left", name)
		}
	}
}

func TestRunningServersAreFollowedAfterAPanelRestart(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")

	// A new panel knows nothing but what the containers and their logs
	// say.
	e.s.gameRuns = gameRuns{}
	if s := e.gameState(t, "survival"); s != "starting" {
		t.Errorf("state %q before it looked", s)
	}
	e.s.resumeGames(context.Background())
	e.waitState(t, "survival", "running")
}

func TestGameMemory(t *testing.T) {
	for mb, want := range map[int64]int64{512: 588, 1024: 1177, 2048: 2252, 4096: 4300, 8192: 8601} {
		if got := gameMemory(mb) >> 20; got != want {
			t.Errorf("%d MB: %d MB", mb, got)
		}
	}
}

func TestPowerNeedsALogin(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	// A request nobody signed in for.
	nobody := &browser{t: t, h: e.b.h, ip: "198.51.100.9"}
	if code, _ := nobody.do("POST", "/api/games/survival/power", map[string]string{"action": "start"}); code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", code)
	}
}

var (
	javaTag    = "ghcr.io/example/java:21"
	javaDigest = "sha256:" + strings.Repeat("a", 64)
	javaPinned = javaTag + "@" + javaDigest
)

func TestGameImageIsPinnedOnFirstStart(t *testing.T) {
	e := newPowerEnv(t)
	e.core.digests = map[string]string{javaTag: javaDigest, "ghcr.io/example/installer:latest": "sha256:" + strings.Repeat("b", 64)}
	e.newGame(t, "survival", consoleEggURL, nil)

	// The install records which build ran it.
	if d, _ := e.s.Store.Deployments(context.Background(), "survival", 1); d[0].Image != "ghcr.io/example/installer:latest@sha256:"+strings.Repeat("b", 64) {
		t.Errorf("install image %q", d[0].Image)
	}

	// The first start pulls the tag and remembers what it got.
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	first := e.liveContainer(t, "survival")
	if got := e.core.games[first].Image; got != javaTag {
		t.Errorf("first start ran %q", got)
	}
	a, _ := e.s.Store.App(context.Background(), "survival")
	if a.Image != javaPinned {
		t.Fatalf("recorded image %q", a.Image)
	}
	if d, _ := e.s.Store.LiveDeployment(context.Background(), "survival"); d.Image != javaPinned {
		t.Errorf("deployment image %q", d.Image)
	}

	// Later starts, restarts and recoveries use it, whatever the tag is now.
	e.core.digests[javaTag] = "sha256:" + strings.Repeat("c", 64)
	e.power(t, "survival", "restart")
	e.settle(t, "survival")
	if got := e.core.games[e.liveContainer(t, "survival")].Image; got != javaPinned {
		t.Errorf("restart ran %q", got)
	}
	e.crash(t, "survival")
	e.s.superviseOnce(context.Background())
	e.settle(t, "survival")
	if got := e.core.games[e.liveContainer(t, "survival")].Image; got != javaPinned {
		t.Errorf("recovery ran %q", got)
	}

	// A reinstall lets the next start take the tag again.
	e.power(t, "survival", "stop")
	e.settle(t, "survival")
	if code, out := e.b.do("POST", "/api/games/survival/reinstall", nil); code != http.StatusCreated {
		t.Fatalf("reinstall: %d %v", code, out)
	}
	e.settle(t, "survival")
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	if got := e.core.games[e.liveContainer(t, "survival")].Image; got != javaTag {
		t.Errorf("after a reinstall the start ran %q", got)
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); a.Image != javaTag+"@sha256:"+strings.Repeat("c", 64) {
		t.Errorf("recorded image %q", a.Image)
	}
}
