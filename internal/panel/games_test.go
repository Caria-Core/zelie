package panel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

const testEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "Test Game",
	"description": "A game for tests.",
	"docker_images": {"Java 21": "ghcr.io/example/java:21", "Java 17": "ghcr.io/example/java:17"},
	"startup": "java -Xmx{{SERVER_MEMORY}}M -jar server.jar --port {{SERVER_PORT}}",
	"config": {"stop": "stop", "startup": "{\"done\": \"Done\"}", "files": "{}"},
	"scripts": {"installation": {
		"script": "#!/bin/bash\r\necho installing\r\n",
		"container": "ghcr.io/example/installer:latest",
		"entrypoint": "bash"
	}},
	"variables": [
		{"name": "Version", "description": "What to install.", "env_variable": "VERSION", "default_value": "1.21", "user_viewable": true, "user_editable": true, "rules": "required|string|max:20"},
		{"name": "Build", "env_variable": "BUILD", "default_value": "stable", "user_viewable": true, "user_editable": false, "rules": "required|string"},
		{"name": "Slots", "env_variable": "SLOTS", "default_value": "20", "user_viewable": true, "user_editable": true, "rules": "required|integer"}
	]
}`

const noScriptEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "Ready Made",
	"docker_images": {"Default": "ghcr.io/example/ready:1"},
	"startup": "./run",
	"config": {"stop": "^C", "startup": "{}", "files": "{}"},
	"variables": []
}`

// fakeEggs serves eggs from memory and remembers what was asked.
type fakeEggs struct {
	mu    sync.Mutex
	files map[string]string // by catalog id or address
	asked []string
	fail  error
}

func (f *fakeEggs) get(key string) (*egg.Egg, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, key)
	if f.fail != nil {
		return nil, nil, f.fail
	}
	raw, ok := f.files[key]
	if !ok {
		return nil, nil, errors.New("not found")
	}
	e, err := egg.Parse([]byte(raw))
	return e, []byte(raw), err
}

func (f *fakeEggs) Catalog(_ context.Context, e egg.Entry) (*egg.Egg, []byte, error) {
	return f.get(e.ID)
}

func (f *fakeEggs) URL(_ context.Context, address string) (*egg.Egg, []byte, error) {
	return f.get(address)
}

func newGameEnv(t *testing.T) (*appEnv, *fakeEggs) {
	t.Helper()
	e := newAppEnv(t)
	f := &fakeEggs{files: map[string]string{
		"minecraft-paper":          testEgg,
		"https://example.com/e.js": noScriptEgg,
	}}
	e.s.Eggs = f
	if err := e.s.Store.AddAllocations(context.Background(), store.ThisNode, store.AnyAddress, []int{25565, 25566, 25567}, e.s.now()); err != nil {
		t.Fatal(err)
	}
	return e, f
}

// install waits for the server's install to finish and returns its
// deployment.
func (e *appEnv) install(t *testing.T, app string) store.Deployment {
	t.Helper()
	d := e.settle(t, app)
	if d.FinishedAt.IsZero() {
		t.Fatalf("install has not finished: %+v", d)
	}
	return d
}

func TestCreateGameAndInstall(t *testing.T) {
	e, eggs := newGameEnv(t)
	e.core.address = core.PublicAddress{Address: "203.0.113.7"}
	code, out := e.b.do("POST", "/api/games", map[string]any{
		"name": "survival", "egg": "minecraft-paper", "memory_mb": 2048, "cpus": 2,
		"variables": map[string]string{"VERSION": "1.20.4"}, "ports": 2,
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if out["egg"] != "Test Game" || out["image"] != "ghcr.io/example/java:21" || out["memory_mb"] != 2048.0 {
		t.Errorf("created: %v", out)
	}
	d := e.install(t, "survival")
	if d.Cause != store.CauseInstall || d.State != store.DeployInstalled || d.Image != "ghcr.io/example/installer:latest" {
		t.Fatalf("install deployment %+v", d)
	}

	// One install container, built the way Pterodactyl's are.
	if len(e.core.installs) != 1 {
		t.Fatalf("installs: %+v", e.core.installs)
	}
	req := e.core.installs[0]
	vols, _ := e.s.Store.Volumes(context.Background(), "survival")
	if len(vols) != 1 || vols[0].Path != "/home/container" || req.Volume != vols[0].Name {
		t.Errorf("volume %+v, install used %q", vols, req.Volume)
	}
	if req.Image != "ghcr.io/example/installer:latest" || req.Entrypoint != "bash" || req.Network != "survival" || req.App != "survival" {
		t.Errorf("request %+v", req)
	}
	if strings.Contains(req.Script, "\r") || !strings.Contains(req.Script, "echo installing\n") {
		t.Errorf("script %q", req.Script)
	}
	env := strings.Join(req.Env, " ")
	for _, want := range []string{"VERSION=1.20.4", "BUILD=stable", "SLOTS=20", "SERVER_MEMORY=2048", "SERVER_PORT=25565", "SERVER_IP=0.0.0.0", "P_SERVER_UUID=survival", "TZ="} {
		if !strings.Contains(env, want) {
			t.Errorf("env lacks %s: %s", want, env)
		}
	}
	if req.MemoryBytes != 2048<<20 || req.CPUs != 2 || req.Pids != installPids {
		t.Errorf("limits %d %v %d", req.MemoryBytes, req.CPUs, req.Pids)
	}
	if list, _ := e.core.List(context.Background()); len(list) != 0 {
		t.Errorf("the install container was not removed: %+v", list)
	}

	// Its output is the deployment's log, kept after the container went.
	rec := e.b.record("GET", "/api/apps/survival/deployments/"+itoa(d.ID)+"/log", nil)
	if !strings.Contains(rec.Body.String(), "downloading server.jar") || !strings.Contains(rec.Body.String(), "event: done") {
		t.Errorf("log: %s", rec.Body)
	}

	code, out = e.b.do("GET", "/api/games/survival", nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, out)
	}
	install := out["install"].(map[string]any)
	if install["state"] != "installed" || install["deployment"] != float64(d.ID) || install["installed_at"] == nil {
		t.Errorf("install: %v", install)
	}
	ports := out["ports"].([]any)
	if len(ports) != 2 || ports[0].(map[string]any)["port"] != 25565.0 || ports[0].(map[string]any)["default"] != true || ports[1].(map[string]any)["default"] != false {
		t.Errorf("ports: %v", ports)
	}
	if ports[0].(map[string]any)["address"] != "203.0.113.7" {
		t.Errorf("port address: %v", ports[0])
	}
	vars := out["variables"].([]any)
	if len(vars) != 3 || vars[0].(map[string]any)["value"] != "1.20.4" || vars[0].(map[string]any)["description"] != "What to install." || vars[1].(map[string]any)["editable"] != false {
		t.Errorf("variables: %v", vars)
	}
	if out["startup"] != "java -Xmx{{SERVER_MEMORY}}M -jar server.jar --port {{SERVER_PORT}}" {
		t.Errorf("startup %v", out["startup"])
	}

	// The server is a game, told apart from apps, and the endpoints that
	// deploy an app leave it alone.
	a, _ := e.s.Store.App(context.Background(), "survival")
	if !a.IsGame() || a.Port != 25565 || a.Image != "ghcr.io/example/java:21" || a.IsDatabase() {
		t.Errorf("app %+v", a)
	}
	for _, r := range [][2]string{{"POST", "/api/apps/survival/deployments"}, {"POST", "/api/apps/survival/restart"}, {"POST", "/api/apps/survival/start"}, {"PATCH", "/api/apps/survival"}} {
		if code, out := e.b.do(r[0], r[1], map[string]any{}); code != http.StatusConflict || out["code"] != "game.not_an_app" {
			t.Errorf("%s %s: %d %v", r[0], r[1], code, out)
		}
	}

	// The egg is kept: a second server of the same game shares it.
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "creative", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("second: %d %v", code, out)
	}
	e.install(t, "creative")
	g1, _ := e.s.Store.GameServer(context.Background(), "survival")
	g2, _ := e.s.Store.GameServer(context.Background(), "creative")
	if g1.EggID != g2.EggID {
		t.Errorf("eggs %d and %d", g1.EggID, g2.EggID)
	}
	kept, err := e.s.Store.Egg(context.Background(), g1.EggID)
	if err != nil || kept.Source != "minecraft-paper" || kept.Name != "Test Game" || string(kept.Raw) != testEgg {
		t.Errorf("stored egg %+v, %v", kept, err)
	}
	if got := e.core.installs[1]; !strings.Contains(strings.Join(got.Env, " "), "SERVER_PORT=25567") {
		// survival took 25565 and 25566.
		t.Errorf("second server's env %v", got.Env)
	}
	if len(eggs.asked) != 2 {
		t.Errorf("fetched %v", eggs.asked)
	}

	// Deleting the server gives its ports back and takes its files.
	if code, _ := e.b.do("DELETE", "/api/apps/survival", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if list, _ := e.s.Store.AppAllocations(context.Background(), "survival"); len(list) != 0 {
		t.Errorf("ports kept: %+v", list)
	}
	if _, err := e.s.Store.GameServer(context.Background(), "survival"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("game server data kept: %v", err)
	}
	if e.core.volumes[vols[0].Name] {
		t.Error("volume kept")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestGameRequestsAreChecked(t *testing.T) {
	e, eggs := newGameEnv(t)
	create := func(body map[string]any) (int, map[string]any) {
		if _, ok := body["name"]; !ok {
			body["name"] = "srv"
		}
		return e.b.do("POST", "/api/games", body)
	}
	for name, c := range map[string]struct {
		body map[string]any
		code int
		err  string
	}{
		"no egg":            {map[string]any{}, 400, "game.egg_choice"},
		"two eggs":          {map[string]any{"egg": "minecraft-paper", "egg_url": "https://example.com/e.js"}, 400, "game.egg_choice"},
		"not in the list":   {map[string]any{"egg": "quake"}, 404, "game.no_egg"},
		"egg does not load": {map[string]any{"egg_url": "https://example.com/missing.json"}, 422, "game.egg_unusable"},
		"bad name":          {map[string]any{"egg": "minecraft-paper", "name": "Not Valid"}, 400, "app.bad_name"},
		"other image":       {map[string]any{"egg": "minecraft-paper", "image": "alpine"}, 400, "game.bad_image"},
		"unknown variable":  {map[string]any{"egg": "minecraft-paper", "variables": map[string]string{"NOPE": "1"}}, 400, "game.unknown_variable"},
		"locked variable":   {map[string]any{"egg": "minecraft-paper", "variables": map[string]string{"BUILD": "nightly"}}, 400, "game.variable_locked"},
		"bad value":         {map[string]any{"egg": "minecraft-paper", "variables": map[string]string{"SLOTS": "many"}}, 400, "game.bad_variable"},
		"required is empty": {map[string]any{"egg": "minecraft-paper", "variables": map[string]string{"VERSION": ""}}, 400, "game.bad_variable"},
		"too little memory": {map[string]any{"egg": "minecraft-paper", "memory_mb": 16}, 400, "game.bad_memory"},
		"too much memory":   {map[string]any{"egg": "minecraft-paper", "memory_mb": 1 << 20}, 400, "game.bad_memory"},
		"too many cpus":     {map[string]any{"egg": "minecraft-paper", "cpus": 64}, 400, "game.bad_cpus"},
		"no ports":          {map[string]any{"egg": "minecraft-paper", "ports": -1}, 400, "game.bad_ports"},
		"too many ports":    {map[string]any{"egg": "minecraft-paper", "ports": 17}, 400, "game.bad_ports"},
		"pool is too small": {map[string]any{"egg": "minecraft-paper", "ports": 4}, 409, "allocation.none_free"},
		"unknown field":     {map[string]any{"egg": "minecraft-paper", "privileged": true}, 400, "server.bad_request"},
	} {
		if code, out := create(c.body); code != c.code || out["code"] != c.err {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if apps, _ := e.s.Store.Apps(context.Background()); len(apps) != 0 {
		t.Errorf("refused requests left apps behind: %+v", apps)
	}
	if list, _ := e.s.Store.Allocations(context.Background(), store.ThisNode); list[0].AppID != "" {
		t.Errorf("a port was kept: %+v", list)
	}
	if len(e.core.installs) != 0 {
		t.Errorf("an install ran: %+v", e.core.installs)
	}
	_ = eggs

	// The name is taken by any app.
	if code, _ := e.b.do("POST", "/api/apps", map[string]any{"id": "taken", "source": "image", "image": "nginx"}); code != http.StatusCreated {
		t.Fatalf("app: %d", code)
	}
	e.settle(t, "taken")
	if code, out := create(map[string]any{"egg": "minecraft-paper", "name": "taken"}); code != http.StatusConflict || out["code"] != "app.exists" {
		t.Errorf("taken name: %d %v", code, out)
	}
	if list, _ := e.s.Store.Allocations(context.Background(), store.ThisNode); list[0].AppID != "" {
		t.Errorf("a port was kept after a name clash: %+v", list)
	}

	// A single line of an egg that is only reachable by address.
	if code, out := create(map[string]any{"egg_url": "https://example.com/e.js"}); code != http.StatusCreated {
		t.Fatalf("by address: %d %v", code, out)
	}
	d := e.install(t, "srv")
	if d.State != store.DeployInstalled || len(e.core.installs) != 0 {
		t.Errorf("an egg without a script: %+v, installs %+v", d, e.core.installs)
	}
	g, _ := e.s.Store.GameServer(context.Background(), "srv")
	if g.InstallState != store.InstallDone || g.Image != "ghcr.io/example/ready:1" {
		t.Errorf("game server %+v", g)
	}
	kept, _ := e.s.Store.Egg(context.Background(), g.EggID)
	if kept.Source != "https://example.com/e.js" {
		t.Errorf("source %q", kept.Source)
	}
}

func TestFailedInstall(t *testing.T) {
	e, _ := newGameEnv(t)
	e.core.installExit = 3
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	d := e.install(t, "srv")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "game.install_failed" || d.Error.Params["code"] != 3.0 {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
	g, _ := e.s.Store.GameServer(context.Background(), "srv")
	if g.InstallState != store.InstallFailed || !g.InstalledAt.IsZero() || g.InstallID != d.ID {
		t.Errorf("game server %+v", g)
	}
	rec := e.b.record("GET", "/api/apps/srv/deployments/"+itoa(d.ID)+"/log", nil)
	if !strings.Contains(rec.Body.String(), "downloading server.jar") || !strings.Contains(rec.Body.String(), "exited with code 3") {
		t.Errorf("log: %s", rec.Body)
	}
	if list, _ := e.core.List(context.Background()); len(list) != 0 {
		t.Errorf("the install container was not removed: %+v", list)
	}
	// The files stay for the next try; nothing removed the volume.
	if vols, _ := e.s.Store.Volumes(context.Background(), "srv"); len(vols) != 1 || !e.core.volumes[vols[0].Name] {
		t.Errorf("volumes %+v", vols)
	}
}

func TestInstallTimeout(t *testing.T) {
	e, _ := newGameEnv(t)
	old := installTimeout
	installTimeout = 50 * time.Millisecond
	t.Cleanup(func() { installTimeout = old })
	e.core.installHang = true
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	d := e.install(t, "srv")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "game.install_timeout" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
	if list, _ := e.core.List(context.Background()); len(list) != 0 {
		t.Errorf("the hung installer was not removed: %+v", list)
	}
}

func TestReinstall(t *testing.T) {
	e, _ := newGameEnv(t)
	e.core.installExit = 1
	e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"})
	if d := e.install(t, "srv"); d.State != store.DeployFailed {
		t.Fatalf("first install %+v", d)
	}
	first := e.settle(t, "srv")

	// Installing again backs up the files first and keeps them.
	e.core.installExit = 0
	code, out := e.b.do("POST", "/api/games/srv/reinstall", nil)
	if code != http.StatusCreated || out["id"] == nil {
		t.Fatalf("reinstall: %d %v", code, out)
	}
	d := e.install(t, "srv")
	if d.ID == first.ID || d.Cause != store.CauseReinstall || d.State != store.DeployInstalled {
		t.Fatalf("deployment %+v", d)
	}
	backups, _ := e.s.Store.Backups(context.Background(), "srv")
	if len(backups) != 1 || backups[0].Reason != store.BackupReinstall || backups[0].State != store.BackupDone {
		t.Fatalf("backups %+v", backups)
	}
	if len(e.core.installs) != 2 {
		t.Errorf("installs %d", len(e.core.installs))
	}
	rec := e.b.record("GET", "/api/apps/srv/deployments/"+itoa(d.ID)+"/log", nil)
	if !strings.Contains(rec.Body.String(), "Backing up the server's files") {
		t.Errorf("log: %s", rec.Body)
	}
	g, _ := e.s.Store.GameServer(context.Background(), "srv")
	if g.InstallState != store.InstallDone || g.InstallID != d.ID {
		t.Errorf("game server %+v", g)
	}

	// A failed backup changes nothing: no install, and the server is as
	// it was.
	e.core.bk.failBackup, e.core.bk.failures = "the disk is full", -1
	if code, _ := e.b.do("POST", "/api/games/srv/reinstall", nil); code != http.StatusCreated {
		t.Fatalf("reinstall: %d", code)
	}
	d = e.install(t, "srv")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "game.install_backup_failed" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
	if len(e.core.installs) != 2 {
		t.Errorf("an install ran after a failed backup: %d", len(e.core.installs))
	}
	if g, _ := e.s.Store.GameServer(context.Background(), "srv"); g.InstallState != store.InstallDone {
		t.Errorf("state after a failed backup: %+v", g)
	}

	// Not while it installs, and not while it runs.
	ctx := context.Background()
	e.s.Store.SetInstall(ctx, "srv", store.InstallRunning, d.ID, time.Time{})
	if code, out := e.b.do("POST", "/api/games/srv/reinstall", nil); code != http.StatusConflict || out["code"] != "game.installing" {
		t.Errorf("while installing: %d %v", code, out)
	}
	e.s.Store.SetInstall(ctx, "srv", store.InstallDone, d.ID, time.Time{})
	e.core.mu.Lock()
	e.core.containers["srv-1"] = engineStatus("srv-1", "srv", "running")
	e.core.mu.Unlock()
	if code, out := e.b.do("POST", "/api/games/srv/reinstall", nil); code != http.StatusConflict || out["code"] != "game.stop_to_reinstall" {
		t.Errorf("while running: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/games/nope/reinstall", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("unknown server: %d %v", code, out)
	}
}

func TestBeginInstallOnlyOnce(t *testing.T) {
	e, _ := newGameEnv(t)
	e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"})
	e.install(t, "srv")
	ctx := context.Background()
	first, err := e.s.Store.BeginInstall(ctx, "srv")
	second, err2 := e.s.Store.BeginInstall(ctx, "srv")
	if err != nil || err2 != nil || !first || second {
		t.Errorf("began %v then %v (%v, %v)", first, second, err, err2)
	}
}

func TestUnfinishedInstallsFail(t *testing.T) {
	e, _ := newGameEnv(t)
	e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"})
	d := e.install(t, "srv")
	ctx := context.Background()
	e.s.Store.SetInstall(ctx, "srv", store.InstallRunning, d.ID, time.Time{})
	e.core.mu.Lock()
	e.core.containers["srv-install-9"] = engineStatus("srv-install-9", "srv", "running")
	e.core.containers["srv-3"] = engineStatus("srv-3", "srv", "running")
	e.core.mu.Unlock()
	if err := e.s.Store.FailUnfinishedInstalls(ctx); err != nil {
		t.Fatal(err)
	}
	if g, _ := e.s.Store.GameServer(ctx, "srv"); g.InstallState != store.InstallFailed {
		t.Errorf("state %q", g.InstallState)
	}
	e.s.removeStaleInstalls(ctx)
	list, _ := e.core.List(ctx)
	if len(list) != 1 || list[0].ID != "srv-3" {
		t.Errorf("containers after cleaning up: %+v", list)
	}
}

func TestEggCatalogAndLogin(t *testing.T) {
	e, _ := newGameEnv(t)
	rec := e.b.record("GET", "/api/eggs/catalog", nil)
	if rec.Code != http.StatusOK || !containsAll(rec.Body.String(), `"id":"minecraft-paper"`, `"game":"Rust"`) {
		t.Errorf("catalog: %d %s", rec.Code, rec.Body)
	}
	out := &browser{t: t, h: e.b.h, ip: "198.51.100.8"}
	for _, r := range [][2]string{
		{"GET", "/api/eggs/catalog"}, {"POST", "/api/games"}, {"GET", "/api/games/x"}, {"POST", "/api/games/x/reinstall"},
	} {
		if code, _ := out.do(r[0], r[1], nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s without logging in: %d", r[0], r[1], code)
		}
	}
	if code, out := e.b.do("GET", "/api/games/nope", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("unknown server: %d %v", code, out)
	}
}

func TestEggPreview(t *testing.T) {
	e, _ := newGameEnv(t)
	code, out := e.b.do("POST", "/api/eggs/preview", map[string]any{"egg": "minecraft-paper"})
	if code != http.StatusOK || out["name"] != "Test Game" || len(out["images"].([]any)) != 2 || len(out["variables"].([]any)) != 3 {
		t.Errorf("preview: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/eggs/preview", map[string]any{}); code != http.StatusBadRequest || out["code"] != "game.egg_choice" {
		t.Errorf("preview without an egg: %d %v", code, out)
	}
	if apps, _ := e.s.Store.Apps(context.Background()); len(apps) != 0 {
		t.Errorf("preview made apps: %+v", apps)
	}
}

func engineStatus(id, app, state string) engine.Status {
	return engine.Status{ID: id, App: app, State: state}
}
