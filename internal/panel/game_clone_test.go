package panel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

func (c *appCore) CopyVolume(_ context.Context, from, to string) error {
	if c.copyHook != nil {
		c.copyHook(from, to)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.copyErr != nil:
		return c.copyErr
	case !c.volumes[from] || !c.volumes[to]:
		return &core.Error{Status: http.StatusNotFound, Message: "volume not found"}
	case !slices.Contains(c.sftpVols, from) || !slices.Contains(c.sftpVols, to):
		return &core.Error{Status: http.StatusForbidden, Message: "only the files of game servers and files apps can be copied"}
	}
	c.copies = append(c.copies, from+" "+to)
	return nil
}

func (e *appEnv) volumeOf(t *testing.T, app string) store.Volume {
	t.Helper()
	vols, err := e.s.Store.Volumes(context.Background(), app)
	if err != nil || len(vols) != 1 {
		t.Fatalf("volumes of %s: %v, %v", app, vols, err)
	}
	return vols[0]
}

// clone asks for a copy and, when it is accepted, waits for the files.
func (e *appEnv) clone(t *testing.T, from, name string) map[string]any {
	t.Helper()
	code, out := e.b.do("POST", "/api/games/"+from+"/clone", map[string]any{"name": name})
	if code != http.StatusCreated {
		t.Fatalf("clone: %d %v", code, out)
	}
	return out
}

func TestCloneGameCopiesSettingsAndPorts(t *testing.T) {
	e := newRustEnv(t)
	ctx := context.Background()
	if code, out := e.b.do("POST", "/api/games", map[string]any{
		"name": "rusty", "egg": "rust", "memory_mb": 2048, "cpus": 2, "disk_mb": 4096,
		"variables": map[string]string{"RCON_PASS": "s3cret-value", "MAX_PLAYERS": "12"},
	}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "rusty")
	installs := len(e.core.installs)

	// What belongs to the original alone: a schedule, a backup, an SFTP login.
	if code, out := e.b.do("POST", "/api/games/rusty/schedules", map[string]any{"name": "nightly", "cron": "30 8 * * *", "enabled": true, "tasks": []any{backupTask()}}); code != http.StatusCreated {
		t.Fatalf("schedule: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/games/rusty/backups", nil); code != http.StatusAccepted {
		t.Fatalf("backup: %d %v", code, out)
	}
	e.s.jobs.Wait()
	if err := e.s.Store.SetSFTPPassword(ctx, "rusty", "hash", e.s.now()); err != nil {
		t.Fatal(err)
	}

	out := e.clone(t, "rusty", "rusty-2")
	if out["id"] != "rusty-2" || out["install"].(map[string]any)["state"] != "installing" || out["install"].(map[string]any)["clone_of"] != "rusty" {
		t.Errorf("answer: %v", out["install"])
	}
	d := e.settle(t, "rusty-2")
	if d.Cause != store.CauseClone || d.State != store.DeployInstalled || d.Message != "rusty" {
		t.Fatalf("deployment %+v", d)
	}

	// The same server: egg, image, startup, limits and values that are not
	// ports or made-up secrets.
	src, _ := e.s.Store.App(ctx, "rusty")
	dup, _ := e.s.Store.App(ctx, "rusty-2")
	sg, _ := e.s.Store.GameServer(ctx, "rusty")
	dg, _ := e.s.Store.GameServer(ctx, "rusty-2")
	if dup.Kind != store.KindGame || dup.Image != src.Image || dup.MemoryMB != 2048 || dup.CPUs != 2 || dup.Stopped {
		t.Errorf("app %+v", dup)
	}
	if dg.EggID != sg.EggID || dg.Image != sg.Image || dg.Startup != sg.Startup || dg.SteamAppID != sg.SteamAppID || dg.InstallState != store.InstallDone {
		t.Errorf("game %+v, original %+v", dg, sg)
	}
	if dg.Variables["RCON_PASS"] != "s3cret-value" || dg.Variables["MAX_PLAYERS"] != "12" {
		t.Errorf("variables %v", dg.Variables)
	}
	if v := e.volumeOf(t, "rusty-2"); v.Path != gameVolumePath || v.LimitMB != 4096 || v.Name == e.volumeOf(t, "rusty").Name {
		t.Errorf("volume %+v", v)
	}

	// Each port has a new counterpart with the same job.
	sp, smain, svars := e.serverPorts(t, "rusty")
	dp, dmain, dvars := e.serverPorts(t, "rusty-2")
	if len(dp) != len(sp) {
		t.Fatalf("ports %v, original %v", dp, sp)
	}
	for _, p := range dp {
		if slices.Contains(sp, p) {
			t.Errorf("port %d belongs to both", p)
		}
	}
	if dmain == smain || !slices.Contains(dp, dmain) {
		t.Errorf("main port %d, original %d, ports %v", dmain, smain, dp)
	}
	rank := func(ports []int, port string) int {
		for i, p := range ports {
			if port == strconv.Itoa(p) {
				return i
			}
		}
		return -1
	}
	for _, env := range []string{"QUERY_PORT", "RCON_PORT", "APP_PORT"} {
		if rank(dp, dvars[env]) < 0 || rank(dp, dvars[env]) != rank(sp, svars[env]) {
			t.Errorf("%s = %s (ports %v), original %s (ports %v)", env, dvars[env], dp, svars[env], sp)
		}
	}
	if rank(dp, strconv.Itoa(dmain)) != rank(sp, strconv.Itoa(smain)) {
		t.Errorf("main port is number %d of %v, was number %d of %v", rank(dp, strconv.Itoa(dmain)), dp, rank(sp, strconv.Itoa(smain)), sp)
	}

	// The files were copied between the two volumes; nothing was installed.
	if want := e.volumeOf(t, "rusty").Name + " " + e.volumeOf(t, "rusty-2").Name; !slices.Equal(e.core.copies, []string{want}) {
		t.Errorf("copies %v, want %q", e.core.copies, want)
	}
	if len(e.core.installs) != installs {
		t.Errorf("the install ran again: %v", e.core.installs[installs:])
	}

	// It is stopped and ready, and has none of the original's own things.
	code, got := e.b.do("GET", "/api/games/rusty-2", nil)
	if code != http.StatusOK || got["state"] != "stopped" || got["install"].(map[string]any)["state"] != "installed" || got["install"].(map[string]any)["clone_of"] != nil {
		t.Errorf("game: %d %v", code, got["install"])
	}
	if _, out := e.b.do("GET", "/api/games/rusty-2/schedules", nil); len(out["schedules"].([]any)) != 0 {
		t.Errorf("schedules %v", out["schedules"])
	}
	if list := e.gameBackups(t, "rusty-2"); len(list) != 0 {
		t.Errorf("backups %v", list)
	}
	if _, out := e.b.do("GET", "/api/games/rusty-2/sftp", nil); out["password_set"] == true {
		t.Errorf("sftp %v", out)
	}
	// The original is as it was.
	if again, _ := e.s.Store.GameServer(ctx, "rusty"); again.Variables["QUERY_PORT"] != svars["QUERY_PORT"] {
		t.Errorf("the original changed: %v", again.Variables)
	}
}

func TestCloneMakesNewSecretsButKeepsChosenOnes(t *testing.T) {
	e := newRustEnv(t)
	ctx := context.Background()
	// The egg leaves RCON_PASS empty, so the original got a made-up one.
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "made-up", "egg": "rust"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "made-up")
	e.clone(t, "made-up", "made-up-2")
	e.install(t, "made-up-2")
	a, _ := e.s.Store.GameServer(ctx, "made-up")
	b, _ := e.s.Store.GameServer(ctx, "made-up-2")
	if len(a.Variables["RCON_PASS"]) != secretLength || len(b.Variables["RCON_PASS"]) != secretLength || a.Variables["RCON_PASS"] == b.Variables["RCON_PASS"] {
		t.Errorf("passwords %q and %q", a.Variables["RCON_PASS"], b.Variables["RCON_PASS"])
	}
	// A copy of a copy is fine too, with a third one.
	e.clone(t, "made-up-2", "made-up-3")
	e.install(t, "made-up-3")
	c, _ := e.s.Store.GameServer(ctx, "made-up-3")
	if c.Variables["RCON_PASS"] == b.Variables["RCON_PASS"] || c.Variables["RCON_PASS"] == a.Variables["RCON_PASS"] {
		t.Errorf("the third server shares a password: %q", c.Variables["RCON_PASS"])
	}
}

func TestCloneSecret(t *testing.T) {
	plain := func(env, def string, rules ...string) egg.Variable {
		return egg.Variable{Env: env, Default: def, Rules: rules}
	}
	made := strings.Repeat("aB3", 8)
	for _, tc := range []struct {
		name  string
		v     egg.Variable
		value string
		fresh bool
	}{
		{"generated admin password", plain("RCON_PASS", ""), made, true},
		{"generated over a placeholder", plain("RCON_PASSWORD", "changeme"), made, true},
		{"typed by the owner", plain("RCON_PASS", ""), "hunter2hunter2", false},
		{"typed, and as long as a made-up one", plain("RCON_PASS", ""), "correct-horse-battery-stap", false},
		{"the egg's own value", plain("RCON_PASS", made), made, false},
		{"empty stays empty", plain("RCON_PASS", ""), "", false},
		{"join password", plain("SERVER_PASS", ""), made, false},
		{"not a secret", plain("WORLD", ""), made, false},
		{"rules that bound the length", plain("RCON_PASS", "", "required", "max:12"), strings.Repeat("aB3", 4), true},
	} {
		got := cloneSecret(tc.v, tc.value)
		if fresh := got != tc.value; fresh != tc.fresh {
			t.Errorf("%s: %q became %q", tc.name, tc.value, got)
		}
		if tc.fresh && (len(got) != len(tc.value) || tc.v.Check(got) != nil) {
			t.Errorf("%s: new value %q", tc.name, got)
		}
	}
}

func TestCloneGameWithABlock(t *testing.T) {
	e := newBlockEnv(t)
	ids := e.poolIDs(t)
	// The run is two ports; QUERY_PORT gets a third, chosen away from it.
	if code, out := e.b.do("POST", "/api/games", map[string]any{
		"name": "vh", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"}, "ports": 3,
		"allocations": map[string]any{"primary": ids[25570], "variables": map[string]int64{"QUERY_PORT": ids[25576]}},
	}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "vh")
	if ports, main, _ := e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25570, 25571, 25576}) || main != 25570 {
		t.Fatalf("original ports %v, main %d", ports, main)
	}

	out := e.clone(t, "vh", "vh-2")
	e.install(t, "vh-2")
	// The lowest free run of two, and the lowest free port after it.
	ports, main, vars := e.serverPorts(t, "vh-2")
	if !slices.Equal(ports, []int{25565, 25566, 25567}) || main != 25565 {
		t.Errorf("ports %v, main %d", ports, main)
	}
	if vars["QUERY_PORT"] != "25567" {
		t.Errorf("QUERY_PORT = %s", vars["QUERY_PORT"])
	}
	if out["block"] != 2.0 || out["block_broken"] != nil {
		t.Errorf("block %v, broken %v", out["block"], out["block_broken"])
	}

	// With no run of two left in the pool, the copy is refused and keeps
	// nothing.
	list, _ := e.s.Store.Allocations(context.Background(), store.ThisNode)
	for _, a := range list {
		if a.AppID == "" && a.Port != 25580 {
			e.s.Store.DeleteAllocation(context.Background(), store.ThisNode, a.ID)
		}
	}
	snap := e.snapshot(t)
	code, got := e.b.do("POST", "/api/games/vh/clone", map[string]any{"name": "vh-3"})
	if code != http.StatusConflict || got["code"] != "allocation.no_block" {
		t.Errorf("no run left: %d %v", code, got)
	}
	e.unchanged(t, snap, "vh-3")
}

// state is what a refused copy must leave as it was.
type cloneSnapshot struct {
	ports   int
	volumes int
	copies  int
}

func (e *appEnv) snapshot(t *testing.T) cloneSnapshot {
	t.Helper()
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	return cloneSnapshot{ports: len(mustAllocsAll(t, e)), volumes: len(e.core.volumes), copies: len(e.core.copies)}
}

func (e *appEnv) unchanged(t *testing.T, before cloneSnapshot, name string) {
	t.Helper()
	if _, err := e.s.Store.App(context.Background(), name); err == nil {
		t.Errorf("%s: a refused copy left a server", name)
	}
	if after := e.snapshot(t); after != before {
		t.Errorf("%s: a refused copy changed %+v to %+v", name, before, after)
	}
}

func TestCloneRefusals(t *testing.T) {
	e := newRustEnv(t)
	ctx := context.Background()
	e.newGame(t, "survival", consoleEggURL, map[string]any{"memory_mb": 2048})
	post := func(from string, name any) (int, map[string]any) {
		return e.b.do("POST", "/api/games/"+from+"/clone", map[string]any{"name": name})
	}
	host := func(h *engine.Host) {
		e.core.mu.Lock()
		e.core.host = h
		e.core.mu.Unlock()
	}

	// Names the way a new server's are checked.
	snap := e.snapshot(t)
	for name, want := range map[string]int{"Bad Name": 400, "zelie-x": 400, strings.Repeat("a", 41): 400} {
		if code, out := post("survival", name); code != want || out["code"] != "app.bad_name" {
			t.Errorf("name %q: %d %v", name, code, out)
		}
	}
	if code, out := post("survival", "survival"); code != http.StatusConflict || out["code"] != "app.exists" {
		t.Errorf("own name: %d %v", code, out)
	}
	e.unchanged(t, snap, "survival-copy")

	// Not enough memory left for the new limits: the machine has shrunk
	// since the original was made.
	host(&engine.Host{CPUs: 2, MemoryBytes: 1 << 30, DiskBytes: 100 << 30, DiskFreeBytes: 60 << 30})
	if code, out := post("survival", "roomy"); code != http.StatusBadRequest || out["code"] != "game.bad_memory" {
		t.Errorf("memory: %d %v", code, out)
	}
	// ...or too few cores.
	host(&engine.Host{CPUs: 0, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, DiskFreeBytes: 60 << 30})
	if code, out := post("survival", "roomy"); code != http.StatusBadRequest || out["code"] != "game.bad_cpus" {
		t.Errorf("cpus: %d %v", code, out)
	}
	e.unchanged(t, snap, "roomy")

	// Not enough disk for the files, going by the last measurement. The
	// core measures again when it copies.
	host(&engine.Host{CPUs: 8, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, DiskFreeBytes: 10 << 30})
	e.s.sizes.set(map[string]int64{e.volumeOf(t, "survival").Name: 9<<30 + 512<<20}, nil, e.s.now())
	if code, out := post("survival", "roomy"); code != http.StatusUnprocessableEntity || out["code"] != "game.clone_no_room" {
		t.Errorf("disk: %d %v", code, out)
	}
	e.unchanged(t, snap, "roomy")
	e.s.sizes.set(nil, nil, e.s.now())
	host(nil)

	// A server that is still installing or failed to install has nothing to
	// copy.
	if err := e.s.Store.SetInstall(ctx, "survival", store.InstallRunning, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if code, out := post("survival", "again"); code != http.StatusConflict || out["code"] != "game.installing" {
		t.Errorf("installing: %d %v", code, out)
	}
	if err := e.s.Store.SetInstall(ctx, "survival", store.InstallFailed, 0, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if code, out := post("survival", "again"); code != http.StatusConflict || out["code"] != "game.not_installed" {
		t.Errorf("failed install: %d %v", code, out)
	}
	e.unchanged(t, snap, "again")

	// Only an administrator copies, and only what exists.
	if code, _ := e.asCustomer(t, requireAdmin(e.s.cloneGame), "POST", "survival", map[string]any{"name": "mine"}); code != http.StatusForbidden {
		t.Errorf("customer: %d", code)
	}
	if code, out := post("nothing", "x"); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("missing: %d %v", code, out)
	}
	e.unchanged(t, snap, "mine")
}

func TestCloneNameDefaultsToCopy(t *testing.T) {
	e := newRustEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	code, out := e.b.do("POST", "/api/games/survival/clone", map[string]any{})
	if code != http.StatusCreated || out["id"] != "survival-copy" {
		t.Fatalf("clone: %d %v", code, out)
	}
	e.install(t, "survival-copy")
}

func TestCloneThatFailsIsRemoved(t *testing.T) {
	e := newRustEnv(t)
	ctx := context.Background()
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "rusty", "egg": "rust", "variables": map[string]string{"RCON_PASS": "x"}}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "rusty")
	held := len(mustAllocs(t, e, "rusty"))
	e.core.copyErr = &core.Error{Status: http.StatusUnprocessableEntity, Message: "Not enough disk space: the copy needs up to 3.0 GB", Code: "copy.no_room", Params: map[string]any{"need": "3.0 GB", "free": "1.1 GB"}}

	e.clone(t, "rusty", "rusty-2")
	e.s.deploys.wg.Wait()

	// Nothing of it is left: the record, the ports, the volume and its log.
	if _, err := e.s.Store.App(ctx, "rusty-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the server is still there: %v", err)
	}
	if got := len(mustAllocs(t, e, "rusty")); got != held {
		t.Errorf("the original holds %d ports, had %d", got, held)
	}
	list, _ := e.s.Store.Allocations(ctx, store.ThisNode)
	for _, a := range list {
		if a.AppID != "" && a.AppID != "rusty" {
			t.Errorf("port %d is still held by %s", a.Port, a.AppID)
		}
	}
	if len(e.core.volumes) != 1 || !e.core.volumes[e.volumeOf(t, "rusty").Name] {
		t.Errorf("volumes %v", e.core.volumes)
	}
	if got := e.core.sftpVols; !slices.Equal(got, []string{e.volumeOf(t, "rusty").Name}) {
		t.Errorf("SFTP may reach %v", got)
	}
	// The page of the server that is no more says why.
	code, out := e.b.do("GET", "/api/games/rusty-2", nil)
	if code != http.StatusNotFound || out["code"] != "game.clone_failed" || !strings.Contains(out["error"].(string), "Not enough disk space") {
		t.Errorf("after the failure: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/apps/rusty-2", nil); code != http.StatusNotFound || out["code"] != "game.clone_failed" {
		t.Errorf("as an app: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/games/never-was", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("another name: %d %v", code, out)
	}
	// The name is free again, and the original is untouched.
	e.core.copyErr = nil
	e.clone(t, "rusty", "rusty-2")
	if d := e.install(t, "rusty-2"); d.State != store.DeployInstalled {
		t.Errorf("second try %+v", d)
	}
	if code, out := e.b.do("GET", "/api/games/rusty-2", nil); code != http.StatusOK {
		t.Errorf("after the second try: %d %v", code, out)
	}

	// A failure that is not the core's shows as it is, and one the core
	// does not explain does not show its insides.
	e.core.copyErr = &core.Error{Status: http.StatusInternalServerError, Message: "copy volume failed, see the core log"}
	e.clone(t, "rusty", "rusty-3")
	e.s.deploys.wg.Wait()
	if _, out := e.b.do("GET", "/api/games/rusty-3", nil); out["code"] != "game.clone_failed" || strings.Contains(out["error"].(string), "see the core log") {
		t.Errorf("core failure: %v", out)
	}
}

func mustAllocs(t *testing.T, e *appEnv, app string) []store.Allocation {
	t.Helper()
	list, err := e.s.Store.AppAllocations(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestCloneFailureNoteExpires(t *testing.T) {
	var f cloneFailures
	now := time.Now()
	f.add("a", "the disk is full", now)
	if err := f.gone("a", now.Add(time.Minute), errNoGame); err.Code != "game.clone_failed" {
		t.Errorf("soon after: %v", err)
	}
	if err := f.gone("a", now.Add(cloneFailureKeep), errNoGame); err.Code != "game.not_found" {
		t.Errorf("later: %v", err)
	}
	for i := range 2 * cloneFailureMax {
		f.add(strconv.Itoa(i), "x", now.Add(time.Duration(i)*time.Second))
	}
	if len(f.list) > cloneFailureMax {
		t.Errorf("%d notes kept", len(f.list))
	}
	if err := f.gone(strconv.Itoa(2*cloneFailureMax-1), now.Add(time.Minute), errNoGame); err.Code != "game.clone_failed" {
		t.Errorf("the newest was dropped: %v", err)
	}
}

func TestCloneOfARunningMinecraftServerSavesFirst(t *testing.T) {
	e := newRustEnv(t)
	saveFlushWait = 5 * time.Second
	t.Cleanup(func() { saveFlushWait = 30 * time.Second })
	e.newGame(t, "survival", consoleEggURL, nil)
	e.answerSaves()
	id := e.startGame(t, "survival")
	e.core.copyHook = func(from, to string) {
		// The server has been told to hold its writes by now.
		if got := e.stdin(id); !slices.Equal(got, []string{"save-off\n", "save-all flush\n"}) {
			t.Errorf("console at the time of the copy: %q", got)
		}
	}

	e.clone(t, "survival", "survival-2")
	if d := e.install(t, "survival-2"); d.State != store.DeployInstalled {
		t.Fatalf("%+v", d)
	}
	if got := e.stdin(id); !slices.Equal(got, []string{"save-off\n", "save-all flush\n", "save-on\n"}) {
		t.Errorf("console got %q", got)
	}
	if len(e.core.copies) != 1 {
		t.Errorf("copies %v", e.core.copies)
	}
	// The original keeps running; the copy does not.
	if _, out := e.b.do("GET", "/api/games/survival", nil); out["state"] != "running" {
		t.Errorf("original: %v", out["state"])
	}
	if _, out := e.b.do("GET", "/api/games/survival-2", nil); out["state"] != "stopped" {
		t.Errorf("copy: %v", out["state"])
	}
}

func TestCloneWaitsForTheOriginalsOtherWork(t *testing.T) {
	e := newRustEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	unlock := e.s.deploys.lock("survival")
	e.clone(t, "survival", "survival-2")
	time.Sleep(50 * time.Millisecond)
	if len(e.core.copies) != 0 {
		t.Error("copied while the original was busy")
	}
	code, out := e.b.do("GET", "/api/games/survival-2", nil)
	if code != http.StatusOK || out["install"].(map[string]any)["state"] != "installing" {
		t.Errorf("while waiting: %d %v", code, out["install"])
	}
	unlock()
	e.install(t, "survival-2")
	if len(e.core.copies) != 1 {
		t.Errorf("copies %v", e.core.copies)
	}
}

func TestCloneFilesApp(t *testing.T) {
	e := newRuntimeEnv(t)
	ctx := context.Background()
	e.newFiles(t, "site", map[string]any{"domain": "site.example.com", "memory_mb": 768, "variables": map[string]string{"MAIN_FILE": "server.js"}})
	before := len(mustAllocsAll(t, e))

	out := e.clone(t, "site", "site-copy")
	if out["id"] != "site-copy" {
		t.Fatalf("clone: %v", out)
	}
	d := e.install(t, "site-copy")
	if d.Cause != store.CauseClone || d.State != store.DeployInstalled {
		t.Fatalf("deployment %+v", d)
	}
	a, _ := e.s.Store.App(ctx, "site-copy")
	src, _ := e.s.Store.App(ctx, "site")
	g, _ := e.s.Store.GameServer(ctx, "site-copy")
	// Its own runtime and files, without the domain, which only one app has.
	if !a.IsFiles() || a.Port != 8080 || a.MemoryMB != 768 || a.Domain != "" || a.Image != src.Image || g.Variables["MAIN_FILE"] != "server.js" {
		t.Errorf("app %+v, variables %v", a, g.Variables)
	}
	if code, out := e.b.do("GET", "/api/apps/site-copy", nil); code != http.StatusOK || out["source"] != "files" || out["state"] == nil {
		t.Errorf("as an app: %d %v", code, out)
	}
	if got := len(mustAllocsAll(t, e)); got != before {
		t.Errorf("pool: %d ports held, had %d", got, before)
	}
	if len(e.core.copies) != 1 {
		t.Errorf("copies %v", e.core.copies)
	}
	if !slices.Contains(e.core.sftpVols, e.volumeOf(t, "site-copy").Name) {
		t.Errorf("SFTP may reach %v", e.core.sftpVols)
	}
}

func mustAllocsAll(t *testing.T, e *appEnv) []store.Allocation {
	t.Helper()
	list, err := e.s.Store.Allocations(context.Background(), store.ThisNode)
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(list, func(a store.Allocation) bool { return a.AppID == "" })
}
