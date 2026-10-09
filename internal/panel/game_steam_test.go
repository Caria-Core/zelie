package panel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/steam"
	"github.com/Caria-Core/zelie/internal/store"
)

// steamCore adds to the test core what a Steam server needs: SteamCMD's
// output, and the game's manifest in the server's volume.
type steamCore struct {
	*appCore
	mu       sync.Mutex
	output   string
	manifest string
	peeked   []string
	runs     [][]string // arguments of each SteamCMD container
	fail     bool
}

func (c *steamCore) RunApp(ctx context.Context, s engine.Spec, sealed []string, linked ...core.LinkedVar) (string, error) {
	if s.ID == steamContainer {
		c.mu.Lock()
		c.runs = append(c.runs, s.Args)
		if c.fail {
			c.mu.Unlock()
			return "", errors.New("no network")
		}
		c.mu.Unlock()
	}
	return c.appCore.RunApp(ctx, s, sealed, linked...)
}

func (c *steamCore) Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error {
	if id == steamContainer {
		c.mu.Lock()
		defer c.mu.Unlock()
		_, err := io.WriteString(w, c.output)
		return err
	}
	return c.appCore.Logs(ctx, id, follow, tail, w)
}

func (c *steamCore) ReadFile(_ context.Context, _ core.FileRef, path string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.peeked = append(c.peeked, path)
	if c.manifest == "" {
		return nil, &core.Error{Status: http.StatusNotFound, Message: "no such file"}
	}
	return []byte(c.manifest), nil
}

func (c *steamCore) setManifest(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manifest = text
}

func manifestWith(build string) string {
	return `"AppState" { "appid" "258550" "buildid" "` + build + `" }`
}

func newSteamEnv(t *testing.T) (*appEnv, *steamCore) {
	t.Helper()
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["rust"] = rustEgg(t)
	if err := e.s.Store.AddAllocations(context.Background(), 1, "0.0.0.0", []int{25568, 25569}, e.s.now()); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile("../steam/testdata/app_info.txt")
	if err != nil {
		t.Fatal(err)
	}
	sc := &steamCore{appCore: e.core, output: string(output), manifest: manifestWith("20900000")}
	e.s.Core = sc
	return e, sc
}

func (e *appEnv) newRust(t *testing.T) {
	t.Helper()
	code, out := e.b.do("POST", "/api/games", map[string]any{
		"name": "rusty", "egg": "rust", "variables": map[string]string{"RCON_PASS": "s3cret"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "rusty")
}

func (e *appEnv) steamOf(t *testing.T, name string) map[string]any {
	t.Helper()
	code, out := e.b.do("GET", "/api/games/"+name, nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, out)
	}
	steam, _ := out["steam"].(map[string]any)
	return steam
}

// startRust starts the server and lets it say it is ready.
func (e *appEnv) startRust(t *testing.T) {
	t.Helper()
	if code, out := e.power(t, "rusty", "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "rusty")
	e.core.emit(e.liveContainer(t, "rusty"), "Server startup complete\n")
	e.waitState(t, "rusty", "running")
}

func TestSteamBuildTracking(t *testing.T) {
	e, sc := newSteamEnv(t)
	e.newRust(t)
	ctx := context.Background()

	// Before anything was asked, the installed build is known from the
	// server's files and the latest is not.
	st := e.steamOf(t, "rusty")
	if st["app_id"] != 258550.0 || st["installed_build"] != "20900000" || st["latest_build"] != "" || st["update_available"] != false || st["checked_at"] != nil || st["auto_update"] != false {
		t.Errorf("before the check: %v", st)
	}
	if !slices.Contains(sc.peeked, "steamapps/appmanifest_258550.acf") {
		t.Errorf("peeked %v", sc.peeked)
	}

	e.s.steamRound(ctx)
	want := steam.InfoCommand([]int64{258550})
	if len(sc.runs) != 1 || !slices.Equal(sc.runs[0], want) {
		t.Fatalf("SteamCMD ran with %v, want %v", sc.runs, want)
	}
	if list, _ := e.core.List(ctx); len(list) != 0 {
		t.Errorf("the SteamCMD container stayed: %+v", list)
	}
	// The fixture has other apps too; only the server's own is kept for it.
	st = e.steamOf(t, "rusty")
	if st["latest_build"] != "20913457" || st["installed_build"] != "20900000" || st["update_available"] != true || st["checked_at"] == nil {
		t.Errorf("after the check: %v", st)
	}

	// An hour has not passed: Steam is not asked again.
	e.s.steamRound(ctx)
	if len(sc.runs) != 1 {
		t.Errorf("asked again after no time: %d runs", len(sc.runs))
	}

	// The server takes the update, and nothing is available.
	sc.setManifest(manifestWith("20913457"))
	if st := e.steamOf(t, "rusty"); st["update_available"] != false || st["installed_build"] != "20913457" {
		t.Errorf("after updating: %v", st)
	}
	if keep := e.s.steamImages(ctx); !slices.Equal(keep, []string{steam.Image}) {
		t.Errorf("images to keep: %v", keep)
	}
}

func TestSteamCheckOnlyWithSteamGames(t *testing.T) {
	e, sc := newSteamEnv(t)
	e.newGame(t, "survival", "https://example.com/e.js", map[string]any{"ports": 1})
	e.s.steamRound(context.Background())
	if len(sc.runs) != 0 {
		t.Errorf("Steam was asked with no Steam game: %v", sc.runs)
	}
	if st := e.steamOf(t, "survival"); st != nil {
		t.Errorf("a game that is not from Steam has %v", st)
	}
	if keep := e.s.steamImages(context.Background()); len(keep) != 0 {
		t.Errorf("images to keep: %v", keep)
	}
	if code, out := e.b.do("PUT", "/api/games/survival/steam", map[string]any{"auto_update": true}); code != http.StatusConflict || out["code"] != "game.not_steam" {
		t.Errorf("settings: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/games/survival/steam/update", nil); code != http.StatusConflict || out["code"] != "game.not_steam" {
		t.Errorf("update: %d %v", code, out)
	}
}

func TestSteamCheckFailures(t *testing.T) {
	e, sc := newSteamEnv(t)
	e.newRust(t)
	sc.fail = true
	e.s.steamRound(context.Background())
	if st := e.steamOf(t, "rusty"); st["latest_build"] != "" || st["checked_at"] != nil {
		t.Errorf("a failed check left %v", st)
	}
	sc.fail = false
	sc.output = "Connecting anonymously to Steam Public...FAILED\n"
	e.s.steam.triedAt = time.Time{}
	e.s.steamRound(context.Background())
	if st := e.steamOf(t, "rusty"); st["latest_build"] != "" {
		t.Errorf("output with no app left %v", st)
	}
}

func TestUpdateNowRestartsARunningServer(t *testing.T) {
	e, _ := newSteamEnv(t)
	e.newRust(t)
	e.startRust(t)
	first := e.liveContainer(t, "rusty")

	// Nothing to update to before Steam was asked.
	if code, out := e.b.do("POST", "/api/games/rusty/steam/update", nil); code != http.StatusConflict || out["code"] != "game.no_update" {
		t.Errorf("no update: %d %v", code, out)
	}
	e.s.steamRound(context.Background())

	code, out := e.b.do("POST", "/api/games/rusty/steam/update", nil)
	if code != http.StatusAccepted || out["deployment"] == nil {
		t.Fatalf("update: %d %v", code, out)
	}
	d := e.settle(t, "rusty")
	if d.Cause != store.CauseRestart || d.State != store.DeployLive {
		t.Errorf("deployment %+v", d)
	}
	if e.liveContainer(t, "rusty") == first {
		t.Error("the server was not started again")
	}
	// The image updates the game as it starts: the egg has no AUTO_UPDATE
	// variable, which is the same as on.
	if len(e.core.installs) != 1 {
		t.Errorf("a restart ran %d installs", len(e.core.installs))
	}
}

func TestUpdateNowInstallsAStoppedServer(t *testing.T) {
	e, _ := newSteamEnv(t)
	e.newRust(t)
	e.s.steamRound(context.Background())

	code, out := e.b.do("POST", "/api/games/rusty/steam/update", nil)
	if code != http.StatusAccepted {
		t.Fatalf("update: %d %v", code, out)
	}
	d := e.install(t, "rusty")
	if d.Cause != store.CauseReinstall || d.State != store.DeployInstalled || out["deployment"] != float64(d.ID) {
		t.Errorf("deployment %+v, answer %v", d, out)
	}
	if len(e.core.installs) != 2 {
		t.Errorf("installs: %d", len(e.core.installs))
	}
	if list, _ := e.s.Store.Backups(context.Background(), "rusty"); len(list) != 1 || list[0].Reason != store.BackupReinstall {
		t.Errorf("backups %+v", list)
	}
	if e.gameState(t, "rusty") != "stopped" {
		t.Errorf("the update started the server")
	}
}

func TestUpdateNowNeedsAStopWhenTheEggDoesNotUpdateOnStart(t *testing.T) {
	e, _ := newSteamEnv(t)
	e.newRust(t)
	e.startRust(t)
	ctx := context.Background()
	e.s.steamRound(ctx)
	a, _ := e.s.Store.App(ctx, "rusty")
	g, _ := e.s.Store.GameServer(ctx, "rusty")
	if !updatesOnStart(g) {
		t.Fatal("a server with no AUTO_UPDATE variable should update on start")
	}

	// The egg's own switch for updating on start, set off: a restart would
	// not update, and installing needs the server stopped.
	g.Variables["AUTO_UPDATE"] = "0"
	if updatesOnStart(g) {
		t.Error("AUTO_UPDATE=0 should not update on start")
	}
	if _, bad := e.s.steamUpdate(ctx, a, g, 0); bad == nil || bad.Code != "game.stop_to_update" {
		t.Errorf("update of a running server: %v", bad)
	}
}

func TestAutoUpdateWaitsForAnEmptyServer(t *testing.T) {
	e, _ := newSteamEnv(t)
	e.newRust(t)
	e.startRust(t)
	ctx := context.Background()

	if code, out := e.b.do("PUT", "/api/games/rusty/steam", map[string]any{"auto_update": true}); code != http.StatusOK || out["steam"].(map[string]any)["auto_update"] != true {
		t.Fatalf("turn on: %d %v", code, out)
	}
	var mu sync.Mutex
	players, failure := 2, error(nil)
	var asked []netip.AddrPort
	e.s.PlayerCount = func(_ context.Context, steamGame bool, addr netip.AddrPort) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		asked = append(asked, addr)
		if !steamGame {
			t.Error("a Steam game was asked as if it were Minecraft")
		}
		return players, failure
	}
	deployments := func() int {
		list, _ := e.s.Store.Deployments(ctx, "rusty", 50)
		return len(list)
	}
	before := deployments()

	// Players are on: nothing happens.
	e.s.steamRound(ctx)
	e.s.deploys.wg.Wait()
	if deployments() != before {
		t.Fatal("a server with players was updated")
	}
	// The query goes to the container itself, on the query port.
	st, _ := e.core.List(ctx)
	var ip netip.Addr
	for _, c := range st {
		if c.App == "rusty" && c.State == "running" {
			ip = c.IP
		}
	}
	if len(asked) == 0 || asked[0] != netip.AddrPortFrom(ip, 25566) {
		t.Errorf("asked %v, want %v:25566", asked, ip)
	}

	// The count fails: the server is left alone.
	mu.Lock()
	players, failure = 0, errors.New("no answer")
	mu.Unlock()
	e.s.steamRound(ctx)
	e.s.deploys.wg.Wait()
	if deployments() != before {
		t.Fatal("a server that could not be counted was updated")
	}

	// Empty: it is restarted, once.
	mu.Lock()
	failure = nil
	mu.Unlock()
	e.s.steamRound(ctx)
	e.s.deploys.wg.Wait()
	if deployments() != before+1 {
		t.Fatalf("deployments %d, want %d", deployments(), before+1)
	}
	e.core.emit(e.liveContainer(t, "rusty"), "Server startup complete\n")
	e.waitState(t, "rusty", "running")

	// The build did not change (the fake game ignores it), and asking again
	// every round would restart the server for ever.
	e.s.steamRound(ctx)
	e.s.deploys.wg.Wait()
	if deployments() != before+1 {
		t.Errorf("the same build was tried again: %d deployments", deployments())
	}

	// Off by default and when turned off.
	if code, _ := e.b.do("PUT", "/api/games/rusty/steam", map[string]any{"auto_update": false}); code != http.StatusOK {
		t.Fatal("turn off")
	}
	if st := e.steamOf(t, "rusty"); st["auto_update"] != false {
		t.Errorf("steam %v", st)
	}
}

func TestAutoUpdateOffLeavesTheServerAlone(t *testing.T) {
	e, _ := newSteamEnv(t)
	e.newRust(t)
	e.startRust(t)
	e.s.PlayerCount = func(context.Context, bool, netip.AddrPort) (int, error) { return 0, nil }
	before, _ := e.s.Store.Deployments(context.Background(), "rusty", 50)
	e.s.steamRound(context.Background())
	e.s.deploys.wg.Wait()
	after, _ := e.s.Store.Deployments(context.Background(), "rusty", 50)
	if len(after) != len(before) {
		t.Errorf("an update ran with auto update off")
	}
}

func manifestOnBranch(build, branch string) string {
	return `"AppState" { "appid" "258550" "buildid" "` + build + `" "UserConfig" { "BetaKey" "` + branch + `" } }`
}

// A server installed from a beta is compared with that beta's build, not the
// public one, which differs from it for good.
func TestSteamServerOnABetaBranch(t *testing.T) {
	e, sc := newSteamEnv(t)
	e.newRust(t)
	e.startRust(t)
	ctx := context.Background()
	e.s.PlayerCount = func(context.Context, bool, netip.AddrPort) (int, error) { return 0, nil }
	if code, out := e.b.do("PUT", "/api/games/rusty/steam", map[string]any{"auto_update": true}); code != http.StatusOK {
		t.Fatalf("turn on: %d %v", code, out)
	}
	// The fixture has the public branch at 20913457 and aux01 at 20990001.
	sc.setManifest(manifestOnBranch("20990001", "aux01"))
	deployments := func() int {
		list, _ := e.s.Store.Deployments(ctx, "rusty", 50)
		return len(list)
	}
	before := deployments()

	e.s.steamRound(ctx)
	e.s.deploys.wg.Wait()
	st := e.steamOf(t, "rusty")
	if st["installed_build"] != "20990001" || st["latest_build"] != "20990001" || st["update_available"] != false {
		t.Errorf("up to date on the beta: %v", st)
	}
	if code, out := e.b.do("POST", "/api/games/rusty/steam/update", nil); code != http.StatusConflict || out["code"] != "game.no_update" {
		t.Errorf("update now: %d %v", code, out)
	}
	if deployments() != before {
		t.Errorf("an empty server on the current beta build was restarted")
	}

	// The beta moves on.
	sc.setManifest(manifestOnBranch("20980000", "aux01"))
	if st := e.steamOf(t, "rusty"); st["latest_build"] != "20990001" || st["update_available"] != true {
		t.Errorf("behind on the beta: %v", st)
	}

	// A branch Steam does not list says nothing about updates.
	sc.setManifest(manifestOnBranch("1", "private"))
	if st := e.steamOf(t, "rusty"); st["latest_build"] != "" || st["update_available"] != false {
		t.Errorf("unknown branch: %v", st)
	}
	e.s.steamRound(ctx)
	e.s.deploys.wg.Wait()
	if deployments() != before {
		t.Errorf("a server on a branch nobody listed was restarted")
	}
}

func TestSteamAppID(t *testing.T) {
	e, _ := newSteamEnv(t)
	rust, err := egg.Parse([]byte(rustEgg(t)))
	if err != nil {
		t.Fatal(err)
	}
	if got := steamAppID(rust, map[string]string{"SRCDS_APPID": "258550"}); got != 258550 {
		t.Errorf("from the variable: %d", got)
	}
	// Without the variable, the install script's own app_update.
	rust.Install.Script = "./steamcmd.sh +login anonymous +app_update 740 validate +quit"
	if got := steamAppID(rust, nil); got != 740 {
		t.Errorf("from the script: %d", got)
	}
	rust.Install.Script = "curl -O https://example.com/server.jar"
	if got := steamAppID(rust, map[string]string{"SRCDS_APPID": "x"}); got != 0 {
		t.Errorf("not Steam: %d", got)
	}

	// A server made before the id was kept gets it on first use.
	e.newRust(t)
	if err := e.s.Store.SetSteamAppID(context.Background(), "rusty", 0); err != nil {
		t.Fatal(err)
	}
	if st := e.steamOf(t, "rusty"); st["app_id"] != 258550.0 {
		t.Errorf("steam %v", st)
	}
	if g, _ := e.s.Store.GameServer(context.Background(), "rusty"); g.SteamAppID != 258550 {
		t.Errorf("the id was not kept: %d", g.SteamAppID)
	}
}
