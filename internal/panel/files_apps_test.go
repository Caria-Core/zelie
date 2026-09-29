package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/store"
)

// A generic egg the way the pelican-eggs ones are: a placeholder where a
// game's ready line goes, an upload switch, and a variable that is locked.
const runtimeEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "node.js generic",
	"docker_images": {"Node 22": "ghcr.io/example/yolks:nodejs_22", "Node 20": "ghcr.io/example/yolks:nodejs_20"},
	"startup": "if [ -f package.json ]; then npm install; fi; node /home/container/{{MAIN_FILE}}",
	"config": {"stop": "^C", "startup": "{\"done\": \"change this text\"}", "files": "{}"},
	"scripts": {"installation": {"script": "#!/bin/bash\necho installing\n", "container": "ghcr.io/example/installer:latest", "entrypoint": "bash"}},
	"variables": [
		{"name": "Main file", "env_variable": "MAIN_FILE", "default_value": "index.js", "user_viewable": true, "user_editable": true, "rules": "required|string"},
		{"name": "Git address", "env_variable": "GIT_ADDRESS", "default_value": "", "user_viewable": true, "user_editable": true, "rules": "nullable|string"},
		{"name": "User upload", "env_variable": "USER_UPLOAD", "default_value": "0", "user_viewable": true, "user_editable": true, "rules": "required|boolean"},
		{"name": "Executable", "env_variable": "EXECUTABLE", "default_value": "app", "user_viewable": true, "user_editable": false, "rules": "required|string"}
	]
}`

func newRuntimeEnv(t *testing.T) *appEnv {
	t.Helper()
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["nodejs"] = runtimeEgg
	return e
}

// newFiles creates a files app and waits for its install.
func (e *appEnv) newFiles(t *testing.T, name string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"name": name, "runtime": "nodejs"}
	for k, v := range extra {
		body[k] = v
	}
	if code, out := e.b.do("POST", "/api/apps/files", body); code != http.StatusCreated || out["id"] != name {
		t.Fatalf("create: %d %v", code, out)
	}
	if d := e.install(t, name); d.State != store.DeployInstalled {
		t.Fatalf("install %+v", d)
	}
}

func TestCreateFilesApp(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "site", map[string]any{"domain": "Site.Example.com", "variables": map[string]string{"MAIN_FILE": "server.js"}})

	// An app, not a game: it is in the app list, takes nothing from the port
	// pool and keeps the domain for the proxy.
	code, out := e.b.do("GET", "/api/apps/site", nil)
	if code != http.StatusOK || out["kind"] != "app" || out["source"] != "files" || out["runtime"] != "nodejs" ||
		out["domain"] != "site.example.com" || out["port"] != 8080.0 || out["image"] != "ghcr.io/example/yolks:nodejs_22" {
		t.Fatalf("app: %d %v", code, out)
	}
	if ports, _ := e.s.Store.AppAllocations(context.Background(), "site"); len(ports) != 0 {
		t.Errorf("ports %v", ports)
	}
	free, _ := e.s.Store.Allocations(context.Background(), store.ThisNode)
	for _, p := range free {
		if p.AppID != "" {
			t.Errorf("port %d taken by %q", p.Port, p.AppID)
		}
	}
	vols, _ := e.s.Store.Volumes(context.Background(), "site")
	if len(vols) != 1 || vols[0].Path != "/home/container" {
		t.Errorf("volumes %+v", vols)
	}

	// The install saw the egg's variables, the fixed port, and an upload
	// switch that was on because no repository was given.
	env := strings.Join(e.core.installs[0].Env, "\n")
	for _, want := range []string{"MAIN_FILE=server.js", "SERVER_PORT=8080", "USER_UPLOAD=1"} {
		if !strings.Contains("\n"+env+"\n", "\n"+want+"\n") {
			t.Errorf("install env lacks %q:\n%s", want, env)
		}
	}

	// Its egg page works through the game endpoints, with every variable
	// editable, the locked one included.
	code, out = e.b.do("GET", "/api/games/site", nil)
	if code != http.StatusOK || out["state"] != "stopped" || len(out["ports"].([]any)) != 0 {
		t.Fatalf("egg page: %d %v", code, out)
	}
	for _, v := range out["variables"].([]any) {
		if v.(map[string]any)["editable"] != true {
			t.Errorf("variable %v is locked", v)
		}
	}
	if code, out := e.b.do("PUT", "/api/games/site/variables", map[string]any{"variables": map[string]string{"EXECUTABLE": "other"}}); code != http.StatusOK {
		t.Errorf("locked variable: %d %v", code, out)
	}

	// A repository leaves the switch alone, so the installer clones it.
	e.newFiles(t, "cloned", map[string]any{"variables": map[string]string{"GIT_ADDRESS": "https://example.com/r.git"}})
	if env := strings.Join(e.core.installs[1].Env, "\n"); !strings.Contains(env, "USER_UPLOAD=0") {
		t.Errorf("install env:\n%s", env)
	}

	for _, c := range []struct {
		body map[string]any
		want int
		code string
	}{
		{map[string]any{"name": "site", "runtime": "nodejs"}, http.StatusConflict, "app.exists"},
		{map[string]any{"name": "other", "runtime": "nodejs", "domain": "site.example.com"}, http.StatusConflict, "app.exists"},
		{map[string]any{"name": "other", "runtime": "nodejs", "domain": "panel.example.com"}, http.StatusConflict, "app.panel_domain"},
		{map[string]any{"name": "other", "runtime": "nodejs", "domain": "not a domain"}, http.StatusBadRequest, "app.bad_domain"},
		{map[string]any{"name": "other", "runtime": "minecraft-paper"}, http.StatusNotFound, "app.no_runtime"},
		{map[string]any{"name": "other", "runtime": "nope"}, http.StatusNotFound, "app.no_runtime"},
		{map[string]any{"name": "other", "runtime": "nodejs", "image": "x"}, http.StatusBadRequest, "game.bad_image"},
		{map[string]any{"name": "other", "runtime": "nodejs", "variables": map[string]string{"NOPE": "1"}}, http.StatusBadRequest, "game.unknown_variable"},
		{map[string]any{"name": "other", "runtime": "nodejs", "variables": map[string]string{"MAIN_FILE": ""}}, http.StatusBadRequest, "game.bad_variable"},
		{map[string]any{"name": "zelie-x", "runtime": "nodejs"}, http.StatusBadRequest, "app.bad_name"},
		{map[string]any{"name": "other", "runtime": "nodejs", "memory_mb": 32}, http.StatusBadRequest, "game.bad_memory"},
	} {
		if code, out := e.b.do("POST", "/api/apps/files", c.body); code != c.want || out["code"] != c.code {
			t.Errorf("%v: %d %v, want %d %s", c.body, code, out, c.want, c.code)
		}
	}
	if _, err := e.s.Store.App(context.Background(), "other"); err == nil {
		t.Error("a refused app was left behind")
	}
}

func TestFilesAppRoutesToItsPort(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "site", map[string]any{"domain": "site.example.com"})
	e.b.do("PUT", "/api/apps/site/env", []map[string]any{{"name": "GREETING", "value": "hi"}, {"name": "TOKEN", "value": "s3cret", "secret": true}})
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres", "version": "17"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/databases", map[string]any{"id": "cache", "engine": "redis"})
	e.settle(t, "cache")
	for _, db := range []string{"pg", "cache"} {
		if code, out := e.b.do("POST", "/api/apps/site/links", map[string]any{"db": db}); code != http.StatusCreated {
			t.Fatalf("link %s: %d %v", db, code, out)
		}
	}

	// The app does not answer yet: the container runs, the app is starting.
	e.mu.Lock()
	e.health = http.StatusServiceUnavailable
	e.mu.Unlock()
	if code, out := e.power(t, "site", "start"); code != http.StatusAccepted || out["state"] != "starting" {
		t.Fatalf("start: %d %v", code, out)
	}
	if d := e.settle(t, "site"); d.State != store.DeployLive {
		t.Fatalf("deployment %+v", d)
	}
	id := e.liveContainer(t, "site")
	st, _ := e.core.List(context.Background())
	var ip string
	for _, c := range st {
		if c.ID == id {
			ip = c.IP.String()
		}
	}
	if got, want := e.proxy.routes(), "panel.example.com"; got == "" || want == "" {
		t.Fatalf("routes %q", got)
	}
	if got, want := e.proxy.routes(), "site.example.com="+ip+":8080"; got != want {
		t.Errorf("routes %q, want %q", got, want)
	}
	if len(e.core.forwards["site"]) != 0 {
		t.Errorf("forwards %+v", e.core.forwards["site"])
	}
	time.Sleep(30 * time.Millisecond)
	if s := e.gameState(t, "site"); s != "starting" {
		t.Errorf("state %q while the port does not answer", s)
	}
	e.mu.Lock()
	e.health = http.StatusNotFound
	e.mu.Unlock()
	e.waitState(t, "site", "running")
	e.mu.Lock()
	checked := slices.Clone(e.checked)
	e.mu.Unlock()
	if !slices.Contains(checked, "site.example.com http://"+ip+":8080/") {
		t.Errorf("health checks %v", checked)
	}

	// The container got what an app gets besides the egg's variables: its
	// own, its databases', and the port.
	spec := e.core.games[id]
	env := strings.Join(spec.Env, "\n")
	for _, want := range []string{"SERVER_PORT=8080", "PORT=8080", "GREETING=hi", "TOKEN=s3cret", "MAIN_FILE=index.js", "REDIS_HOST=cache", "PGHOST=pg", "HOME=/home/container"} {
		if !strings.Contains("\n"+env+"\n", "\n"+want+"\n") {
			t.Errorf("env lacks %q:\n%s", want, env)
		}
	}
	if !strings.Contains(env, "DATABASE_URL=postgresql://") || !strings.Contains(env, "REDIS_URL=redis://default:") {
		t.Errorf("env lacks the database URLs:\n%s", env)
	}
	if !strings.Contains(env, "STARTUP=if [ -f package.json ]; then npm install; fi; node /home/container/index.js") {
		t.Errorf("env lacks the startup:\n%s", env)
	}

	// The domain moves with the app's settings, to the port that was set.
	if code, out := e.b.do("PATCH", "/api/apps/site", map[string]any{"port": 3000}); code != http.StatusNoContent {
		t.Fatalf("patch: %d %v", code, out)
	}
	e.b.do("PATCH", "/api/apps/site", map[string]any{"domain": "www.example.com"})
	if got, want := e.proxy.routes(), "www.example.com="+ip+":3000"; got != want {
		t.Errorf("routes %q, want %q", got, want)
	}
}

func TestFilesAppWithoutDomainIsRunningOnceUp(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "bot", nil)
	e.power(t, "bot", "start")
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	e.mu.Lock()
	checked := slices.Clone(e.checked)
	e.mu.Unlock()
	if len(checked) != 0 {
		t.Errorf("health checks %v", checked)
	}
	if e.proxy.routes() != "" {
		t.Errorf("routes %q", e.proxy.routes())
	}
}

func TestFilesAppPower(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "bot", nil)

	// The generic endpoints of an app go through the same code as the
	// game's power buttons.
	if code, out := e.b.do("POST", "/api/apps/bot/start", nil); code != http.StatusAccepted || out["state"] != "starting" {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	first := e.liveContainer(t, "bot")
	if code, out := e.b.do("POST", "/api/apps/bot/start", nil); code != http.StatusConflict || out["code"] != "game.already_running" {
		t.Errorf("start twice: %d %v", code, out)
	}

	if code, out := e.b.do("POST", "/api/apps/bot/restart", nil); code != http.StatusAccepted {
		t.Fatalf("restart: %d %v", code, out)
	}
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	if second := e.liveContainer(t, "bot"); second == first || len(e.running("bot")) != 1 {
		t.Errorf("restart left %v after %s", e.running("bot"), first)
	}
	// The egg's stop command and the same prepared volume as a game's.
	if len(e.core.prepares) != 2 || e.core.prepares[1].Req.UID != 988 {
		t.Errorf("prepares %+v", e.core.prepares)
	}

	if code, out := e.b.do("POST", "/api/apps/bot/stop", nil); code != http.StatusAccepted {
		t.Fatalf("stop: %d %v", code, out)
	}
	e.waitState(t, "bot", "stopped")
	if code, out := e.b.do("GET", "/api/apps/bot", nil); code != http.StatusOK || out["stopped"] != true {
		t.Errorf("app: %d %v", code, out)
	}
	if code, _ := e.power(t, "bot", "start"); code != http.StatusAccepted {
		t.Fatalf("start again: %d", code)
	}
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	if code, _ := e.power(t, "bot", "kill"); code != http.StatusOK {
		t.Fatalf("kill: %d", code)
	}
	e.waitState(t, "bot", "stopped")
}

func TestFilesAppCrashComesBack(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "bot", nil)
	old := crashDelay
	crashDelay = time.Millisecond
	t.Cleanup(func() { crashDelay = old })
	e.power(t, "bot", "start")
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	id := e.liveContainer(t, "bot")
	e.core.mu.Lock()
	c := e.core.containers[id]
	c.State = "stopped"
	e.core.containers[id] = c
	e.core.mu.Unlock()
	e.waitState(t, "bot", "crashed")
	e.s.superviseOnce(context.Background())
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	if e.liveContainer(t, "bot") == id {
		t.Error("the app was not started again")
	}
}

func TestFilesAppRefusesWhatIsNotItsOwn(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "site", nil)
	for _, c := range []struct {
		method, path string
		body         any
		want         int
		code         string
	}{
		{"POST", "/api/apps/site/deployments", nil, http.StatusConflict, "app.files_app"},
		{"POST", "/api/apps/site/deployments/1/rollback", nil, http.StatusConflict, "app.files_app"},
		{"POST", "/api/apps/site/update", nil, http.StatusConflict, "app.files_app"},
		{"PATCH", "/api/apps/site", map[string]any{"image": "nginx"}, http.StatusBadRequest, "app.files_fixed"},
		{"PATCH", "/api/apps/site", map[string]any{"repo": "a/b"}, http.StatusBadRequest, "app.files_fixed"},
		{"PATCH", "/api/apps/site", map[string]any{"start_command": "node x.js"}, http.StatusBadRequest, "app.files_fixed"},
		{"PATCH", "/api/apps/site", map[string]any{"source": "image"}, http.StatusBadRequest, "app.fixed"},
		{"PATCH", "/api/apps/site", map[string]any{"memory_mb": 64}, http.StatusBadRequest, "game.bad_memory"},
		{"POST", "/api/apps", map[string]any{"id": "x", "source": "files"}, http.StatusBadRequest, "app.bad_source"},
	} {
		code, out := e.b.do(c.method, c.path, c.body)
		if code != c.want || out["code"] != c.code {
			t.Errorf("%s %s %v: %d %v, want %d %s", c.method, c.path, c.body, code, out, c.want, c.code)
		}
	}
	// What an app has still works: limits, the domain and its health path.
	if code, out := e.b.do("PATCH", "/api/apps/site", map[string]any{"memory_mb": 768, "cpus": 1, "health_path": "/up", "domain": "a.example.com"}); code != http.StatusNoContent {
		t.Errorf("patch: %d %v", code, out)
	}

	// The files volume stays.
	vols, _ := e.s.Store.Volumes(context.Background(), "site")
	if code, out := e.b.do("DELETE", "/api/apps/site/volumes/"+itoa(vols[0].ID), nil); code != http.StatusConflict || out["code"] != "volume.files_main" {
		t.Errorf("delete volume: %d %v", code, out)
	}
	if code, out := e.b.do("PATCH", "/api/apps/site/volumes/"+itoa(vols[0].ID), map[string]any{"path": "/data"}); code != http.StatusConflict || out["code"] != "volume.files_main" {
		t.Errorf("move volume: %d %v", code, out)
	}
	if code, out := e.b.do("PATCH", "/api/apps/site/volumes/"+itoa(vols[0].ID), map[string]any{"limit_mb": 2048}); code != http.StatusOK {
		t.Errorf("resize volume: %d %v", code, out)
	}

	// The other way round: a game is not an app.
	e.newGame(t, "survival", consoleEggURL, nil)
	if code, out := e.b.do("POST", "/api/apps/survival/restart", nil); code != http.StatusConflict || out["code"] != "game.not_an_app" {
		t.Errorf("game restart: %d %v", code, out)
	}
}

func TestFilesAppFilesAndDelete(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "site", nil)
	if code, out := e.b.do("GET", "/api/games/site/files/list?path=/", nil); code != http.StatusOK {
		t.Errorf("list: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/games/site/console/token", nil); code != http.StatusOK || out["token"] == nil {
		t.Errorf("console token: %d %v", code, out)
	}
	e.power(t, "site", "start")
	e.settle(t, "site")
	if code, out := e.b.do("DELETE", "/api/apps/site", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	if _, err := e.s.Store.GameServer(context.Background(), "site"); err == nil {
		t.Error("the egg data was kept")
	}
	if len(e.running("site")) != 0 {
		t.Errorf("containers %v", e.running("site"))
	}
}

func TestCatalogSplit(t *testing.T) {
	e := newRuntimeEnv(t)
	ids := func(path string) []string {
		rec := e.b.record("GET", path, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
		var list []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, x := range list {
			out = append(out, x.ID)
		}
		return out
	}
	games, runtimes := ids("/api/eggs/catalog"), ids("/api/eggs/catalog?kind=runtime")
	for _, want := range []string{"minecraft-paper", "rust", "palworld"} {
		if !slices.Contains(games, want) || slices.Contains(runtimes, want) {
			t.Errorf("%s: games %v, runtimes %v", want, games, runtimes)
		}
	}
	for _, want := range []string{"nodejs", "python", "bun", "deno", "java"} {
		if slices.Contains(games, want) || !slices.Contains(runtimes, want) {
			t.Errorf("%s: games %v, runtimes %v", want, games, runtimes)
		}
	}
	if code, out := e.b.do("GET", "/api/eggs/catalog?kind=other", nil); code != http.StatusBadRequest || out["code"] != "game.bad_egg_kind" {
		t.Errorf("bad kind: %d %v", code, out)
	}

	// A game cannot be made from a generic egg, and its preview shows the
	// variables as editable.
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "g", "egg": "nodejs"}); code != http.StatusBadRequest || out["code"] != "game.runtime_egg" {
		t.Errorf("game from runtime: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/eggs/preview", map[string]any{"egg": "nodejs"}); code != http.StatusOK {
		t.Errorf("preview: %d %v", code, out)
	} else {
		for _, v := range out["variables"].([]any) {
			v := v.(map[string]any)
			if v["editable"] != true {
				t.Errorf("variable %v is locked", v)
			}
			if v["env"] == "USER_UPLOAD" && (v["value"] != "1" || v["default"] != "0") {
				t.Errorf("upload switch %v", v)
			}
		}
	}
}

// Every runtime the catalog offers is pinned to a commit and named like the
// language it runs.
func TestRuntimeCatalogEntries(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range egg.Catalog {
		if seen[e.ID] {
			t.Errorf("%s is listed twice", e.ID)
		}
		seen[e.ID] = true
		if len(e.Commit) != 40 || e.Repo == "" || e.Path == "" || e.Name == "" {
			t.Errorf("entry %+v", e)
		}
	}
	if got := len(egg.OfKind(egg.KindRuntime)); got != 5 {
		t.Errorf("%d runtimes", got)
	}
}
