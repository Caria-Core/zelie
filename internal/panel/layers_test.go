package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

const (
	mb = int64(1) << 20
	gb = int64(1) << 30
)

// layerClock is the clock of a test that moves it, safe to read from the
// deployments running in the background.
type layerClock struct{ at atomic.Int64 }

func (c *layerClock) now() time.Time { return time.Unix(c.at.Load(), 0) }

func (c *layerClock) advance(d time.Duration) { c.at.Add(int64(d / time.Second)) }

var layerEpoch = time.Unix(1_800_000_000, 0)

// clock gives the test a clock of its own, which stands still until it is moved.
func (e *appEnv) clock() *layerClock {
	c := &layerClock{}
	c.at.Store(layerEpoch.Unix())
	e.s.Now = c.now
	return c
}

// ownFiles sets what the core says the running containers of apps hold
// outside their volumes, by app.
func (e *appEnv) ownFiles(app string, bytes int64) {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if e.core.layerBytes == nil {
		e.core.layerBytes = map[string]int64{}
	}
	e.core.layerBytes[app] = bytes
}

// firstLayerCheck runs the check once with every app within its allowance, as
// the first check on a server whose apps are all fine does. What comes after
// is no longer the first time.
func (e *appEnv) firstLayerCheck(t *testing.T) {
	t.Helper()
	e.core.mu.Lock()
	saved := e.core.layerBytes
	e.core.layerBytes = nil
	e.core.mu.Unlock()
	e.s.checkLayers(context.Background())
	e.core.mu.Lock()
	e.core.layerBytes = saved
	e.core.mu.Unlock()
}

func (e *appEnv) stopped(t *testing.T, app string) bool {
	t.Helper()
	a, err := e.s.Store.App(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return a.Stopped
}

func (e *appEnv) newImageApp(t *testing.T, id string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"id": id, "source": "image", "image": "nginx"}
	for k, v := range extra {
		body[k] = v
	}
	if code, out := e.b.do("POST", "/api/apps", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.settle(t, id)
}

// graceOf returns the warning on an app's page: when it will be stopped and
// the code of the message that says why. Empty when there is none.
func (e *appEnv) graceOf(t *testing.T, path string) (until time.Time, code string) {
	t.Helper()
	_, out := e.b.do("GET", path, nil)
	g, _ := out["layer_grace"].(map[string]any)
	if g == nil {
		return time.Time{}, ""
	}
	until, err := time.Parse(time.RFC3339, g["until"].(string))
	if err != nil {
		t.Fatal(err)
	}
	why, _ := g["why"].(map[string]any)
	return until, why["code"].(string)
}

func TestOwnFilesCountTowardsTheDiskAllowance(t *testing.T) {
	e := newAppEnv(t)
	e.newImageApp(t, "mc", map[string]any{"volumes": []map[string]any{{"path": "/data", "limit_mb": 6144}}})
	e.firstLayerCheck(t)
	e.core.sizes = map[string]int64{volumesOf(t, e, "mc")[0].Name: 5000 * mb}
	e.s.checkVolumes(context.Background())

	// The volume is within its limit and so are the two together.
	e.ownFiles("mc", 400*mb)
	e.s.checkLayers(context.Background())
	if e.stopped(t, "mc") {
		t.Fatal("the app was stopped with room left")
	}

	// 5000 MB in the volume and 1200 MB outside it make more than 6 GB.
	e.ownFiles("mc", 1200*mb)
	e.s.checkLayers(context.Background())
	if !e.stopped(t, "mc") {
		t.Fatal("the app kept running over its disk allowance")
	}
	_, out := e.b.do("GET", "/api/apps/mc", nil)
	why, _ := out["stopped_for"].(map[string]any)
	if why["code"] != "app.layer_full" {
		t.Fatalf("stopped_for = %v", out["stopped_for"])
	}
	params, _ := why["params"].(map[string]any)
	if params["outside"] != "1.2 GB" || params["limit"] != "6 GB" || params["used"] != "6.1 GB" {
		t.Errorf("params %v", params)
	}
	if out["volume_full"] != nil || out["layer_grace"] != nil {
		t.Errorf("no volume is over its own limit, but volume_full is %v and layer_grace %v", out["volume_full"], out["layer_grace"])
	}

	// A new container starts with empty files, so the app starts again.
	if code, out := e.b.do("POST", "/api/apps/mc/start", nil); code >= 300 {
		t.Fatalf("start: %d %v", code, out)
	}
	if d := e.settle(t, "mc"); d.State != store.DeployLive {
		t.Fatalf("start: %+v", d)
	}
	if _, out := e.b.do("GET", "/api/apps/mc", nil); out["stopped_for"] != nil {
		t.Errorf("the reason stays after the app was started: %v", out["stopped_for"])
	}
	// Nor does it come back when the user stops the app by hand.
	if code, _ := e.b.do("POST", "/api/apps/mc/stop", nil); code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
	if _, out := e.b.do("GET", "/api/apps/mc", nil); out["stopped_for"] != nil {
		t.Errorf("the old reason is on an app the user stopped: %v", out["stopped_for"])
	}
}

// What an app's volumes can hold is the room for its own files too, but an app
// with no volume, or only small ones, may write up to 5 GB outside them all the
// same.
func TestTheDiskAllowanceHasAFloor(t *testing.T) {
	e := newAppEnv(t)
	e.newImageApp(t, "web", nil)
	e.newImageApp(t, "blog", map[string]any{"volumes": []map[string]any{{"path": "/data", "limit_mb": 512}}})
	e.firstLayerCheck(t)

	e.ownFiles("web", 4900*mb)
	e.ownFiles("blog", 4900*mb)
	e.s.checkLayers(context.Background())
	if e.stopped(t, "web") || e.stopped(t, "blog") {
		t.Fatal("an app was stopped at 4900 MB")
	}
	e.ownFiles("web", 5300*mb)
	e.ownFiles("blog", 5300*mb)
	e.s.checkLayers(context.Background())
	for _, app := range []string{"web", "blog"} {
		if !e.stopped(t, app) {
			t.Errorf("%s kept running with 5300 MB outside any volume", app)
		}
	}
	_, out := e.b.do("GET", "/api/apps/web", nil)
	why, _ := out["stopped_for"].(map[string]any)
	if params, _ := why["params"].(map[string]any); params["limit"] != "5 GB" {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}
}

func TestDiskFloorIsNotTheDefaultVolumeSize(t *testing.T) {
	if diskFloorMB != 5120 {
		t.Errorf("diskFloorMB = %d", diskFloorMB)
	}
	if diskAllowanceMB(nil) != 5120 || diskAllowanceMB([]store.Volume{{LimitMB: 2048}, {LimitMB: 1024}}) != 5120 ||
		diskAllowanceMB([]store.Volume{{LimitMB: 4096}, {LimitMB: 2048}}) != 6144 {
		t.Error("the allowance is the volumes' limits added up, and at least the floor")
	}
}

// An app that goes over after the check has run is stopped at once, whether
// the reason is its size or that it cannot be measured.
func TestAnAppThatCannotBeMeasuredIsStoppedAtOnce(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "old", nil)
	e.firstLayerCheck(t)
	// An app made after the check ran was never given the day.
	clk.advance(time.Hour)
	e.newImageApp(t, "web", nil)
	e.core.mu.Lock()
	e.core.layerUnmeasured = map[string]bool{"web": true}
	e.core.mu.Unlock()

	e.s.checkLayers(context.Background())
	if !e.stopped(t, "web") {
		t.Fatal("the app kept running with its disk use unknown")
	}
	_, out := e.b.do("GET", "/api/apps/web", nil)
	if why, _ := out["stopped_for"].(map[string]any); why["code"] != "app.layer_unmeasured" {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}
}

// For the first day after the check began, an app that was there before it and
// cannot be measured gets the same warning as one that is over: nothing says
// yet that it is.
func TestAnAppThatCannotBeMeasuredGetsTheDayIfItWasThereBeforeTheCheck(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "old", nil)
	e.firstLayerCheck(t)
	clk.advance(time.Hour)
	e.core.mu.Lock()
	e.core.layerUnmeasured = map[string]bool{"old": true}
	e.core.mu.Unlock()

	e.s.checkLayers(context.Background())
	if e.stopped(t, "old") {
		t.Fatal("the app was stopped in its day")
	}
	until, code := e.graceOf(t, "/api/apps/old")
	if !until.Equal(layerEpoch.Add(24*time.Hour)) || code != "app.layer_unmeasured" {
		t.Errorf("grace until %v for %q", until, code)
	}

	clk.advance(23 * time.Hour)
	e.s.checkLayers(context.Background())
	if !e.stopped(t, "old") {
		t.Fatal("the app is still running a day after the check began")
	}
	_, out := e.b.do("GET", "/api/apps/old", nil)
	if why, _ := out["stopped_for"].(map[string]any); why["code"] != "app.layer_unmeasured" {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}
}

// The first time the check runs on a server, the apps already over get a day.
func TestAppsOverOnTheFirstCheckAreWarnedAndStoppedADayLater(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "web", nil)
	e.ownFiles("web", 6*gb)

	e.s.checkLayers(context.Background())
	if e.stopped(t, "web") {
		t.Fatal("the app was stopped by the first check")
	}
	until, code := e.graceOf(t, "/api/apps/web")
	if !until.Equal(layerEpoch.Add(24*time.Hour)) || code != "app.layer_full" {
		t.Fatalf("grace until %v for %q", until, code)
	}
	if _, out := e.b.do("GET", "/api/apps/web", nil); out["stopped_for"] != nil {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}

	// Still over a minute short of the day: the deadline has not moved.
	clk.advance(24*time.Hour - time.Minute)
	e.ownFiles("web", 7*gb)
	e.s.checkLayers(context.Background())
	if e.stopped(t, "web") {
		t.Fatal("the app was stopped before its day was over")
	}
	if u, _ := e.graceOf(t, "/api/apps/web"); !u.Equal(until) {
		t.Errorf("the deadline moved to %v", u)
	}
	_, out := e.b.do("GET", "/api/apps/web", nil)
	g := out["layer_grace"].(map[string]any)["why"].(map[string]any)["params"].(map[string]any)
	if g["outside"] != "7 GB" {
		t.Errorf("the warning keeps old numbers: %v", g)
	}

	clk.advance(time.Minute)
	e.s.checkLayers(context.Background())
	if !e.stopped(t, "web") {
		t.Fatal("the app kept running after its day")
	}
	_, out = e.b.do("GET", "/api/apps/web", nil)
	if why, _ := out["stopped_for"].(map[string]any); why["code"] != "app.layer_full" {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}
	if out["layer_grace"] != nil {
		t.Errorf("a stopped app still has a warning: %v", out["layer_grace"])
	}
}

func TestAnAppThatGoesOverAfterTheFirstCheckIsStoppedAtOnce(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "old", nil)
	e.newImageApp(t, "fresh", nil)
	e.ownFiles("old", 6*gb)
	e.s.checkLayers(context.Background())
	if e.stopped(t, "old") || e.stopped(t, "fresh") {
		t.Fatal("an app was stopped by the first check")
	}

	clk.advance(time.Hour)
	e.ownFiles("fresh", 6*gb)
	e.s.checkLayers(context.Background())
	if !e.stopped(t, "fresh") {
		t.Error("an app that went over after the first check was given a day")
	}
	if e.stopped(t, "old") {
		t.Error("the app that has its day was stopped")
	}
}

// An app that was stopped while the first check ran, and is started already
// over its allowance within the day, is in the same place as one that ran.
func TestAnAppFirstMeasuredInTheDayGetsTheDay(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "web", nil)
	e.newImageApp(t, "later", nil)
	e.core.mu.Lock()
	e.core.layerHidden = map[string]bool{"later": true}
	e.core.mu.Unlock()
	e.s.checkLayers(context.Background())

	clk.advance(3 * time.Hour)
	e.core.mu.Lock()
	e.core.layerHidden = nil
	e.core.mu.Unlock()
	e.ownFiles("later", 6*gb)
	e.s.checkLayers(context.Background())
	if e.stopped(t, "later") {
		t.Fatal("an app from before the check was stopped on its first measurement")
	}
	if until, code := e.graceOf(t, "/api/apps/later"); !until.Equal(layerEpoch.Add(24*time.Hour)) || code != "app.layer_full" {
		t.Errorf("grace until %v for %q", until, code)
	}
}

// What is kept in the database is all the check needs: a panel that restarts
// or is updated during the day stops the app when the day is over.
func TestTheDeadlineSurvivesARestart(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "web", nil)
	e.ownFiles("web", 6*gb)
	e.s.checkLayers(context.Background())

	// A panel that starts with nothing in memory.
	restarted := &Server{Store: e.s.Store, Core: e.core, Log: e.s.Log, Now: clk.now}
	clk.advance(12 * time.Hour)
	restarted.checkLayers(context.Background())
	if e.stopped(t, "web") {
		t.Fatal("the restarted panel stopped the app in the middle of its day")
	}
	if l, _ := restarted.Store.AppLayers(context.Background(), "web"); !l.Deadline.Equal(layerEpoch.Add(24 * time.Hour)) {
		t.Fatalf("the deadline is %v", l.Deadline)
	}
	clk.advance(12 * time.Hour)
	restarted.checkLayers(context.Background())
	if !e.stopped(t, "web") {
		t.Fatal("the restarted panel did not stop the app at the deadline")
	}

	// The reason is read from the database too.
	again := &Server{Store: e.s.Store, Core: e.core, Log: e.s.Log, Now: clk.now}
	a, _ := e.s.Store.App(context.Background(), "web")
	why, grace, err := again.layerNotes(context.Background(), a)
	if err != nil || why == nil || why.Code != "app.layer_full" || grace != nil {
		t.Errorf("after a restart: %+v %+v %v", why, grace, err)
	}
}

func TestAnAppBackUnderTheAllowanceLosesItsDeadline(t *testing.T) {
	e := newAppEnv(t)
	clk := e.clock()
	e.newImageApp(t, "web", nil)
	e.ownFiles("web", 6*gb)
	e.s.checkLayers(context.Background())
	if u, _ := e.graceOf(t, "/api/apps/web"); u.IsZero() {
		t.Fatal("no warning")
	}

	clk.advance(time.Hour)
	e.ownFiles("web", 1*gb)
	e.s.checkLayers(context.Background())
	if u, _ := e.graceOf(t, "/api/apps/web"); !u.IsZero() {
		t.Errorf("the warning stays after the app is back under: %v", u)
	}
	if l, _ := e.s.Store.AppLayers(context.Background(), "web"); !l.Deadline.IsZero() {
		t.Errorf("the deadline stays: %v", l.Deadline)
	}

	// Over again, it is no longer the first time, and no day is given.
	clk.advance(time.Hour)
	e.ownFiles("web", 6*gb)
	e.s.checkLayers(context.Background())
	if !e.stopped(t, "web") {
		t.Error("the app was given a second day")
	}
}

// A container of an app that was stopped by the user, or deleted meanwhile, is
// not Zelie's to stop.
func TestOwnFilesOfAStoppedOrDeletedAppAreLeftAlone(t *testing.T) {
	e := newAppEnv(t)
	e.newImageApp(t, "web", nil)
	e.firstLayerCheck(t)
	e.ownFiles("web", 7*gb)
	e.core.mu.Lock()
	e.core.layers = append(e.core.layers, engine.LayerSize{Container: "gone-1", App: "gone", Bytes: 1 << 40})
	e.core.mu.Unlock()
	e.b.do("POST", "/api/apps/web/stop", nil)

	e.s.checkLayers(context.Background())
	if _, out := e.b.do("GET", "/api/apps/web", nil); out["stopped_for"] != nil {
		t.Errorf("an app the user stopped was given a reason: %v", out["stopped_for"])
	}
}

// When the core could not measure at all, nothing is known about any app, and
// none is stopped for it.
func TestNoAppIsStoppedWhenTheMeasurementFails(t *testing.T) {
	e := newAppEnv(t)
	e.newImageApp(t, "web", nil)
	e.firstLayerCheck(t)
	e.ownFiles("web", 7*gb)
	e.core.mu.Lock()
	e.core.layersErr = http.ErrHandlerTimeout
	e.core.mu.Unlock()

	if !e.s.checkLayers(context.Background()) {
		t.Error("a failed measurement ended the job")
	}
	if e.stopped(t, "web") {
		t.Fatal("the app was stopped by a failed measurement")
	}
}

// A game server stops the way its egg says, with the time to save, and is told
// why.
func TestAGameServerOverItsDiskWithItsOwnFilesIsStoppedGracefully(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")
	e.firstLayerCheck(t)

	vol := volumesOf(t, e, "survival")[0]
	e.core.mu.Lock()
	e.core.sizes = map[string]int64{vol.Name: vol.LimitMB<<20 - 10*mb}
	e.core.mu.Unlock()
	e.s.checkVolumes(context.Background())
	// Within the volume's limit and the 5 GB floor are both the allowance.
	allowance := max(vol.LimitMB, diskFloorMB) * mb
	e.ownFiles("survival", allowance)
	e.s.checkLayers(context.Background())
	e.settle(t, "survival")
	e.waitState(t, "survival", "stopped")

	if got := e.stdin(id); len(got) != 1 || got[0] != "stop\n" {
		t.Errorf("console %q, want the egg's stop command", got)
	}
	if len(e.core.signals[id]) != 0 {
		t.Errorf("signals %v", e.core.signals[id])
	}
	_, out := e.b.do("GET", "/api/games/survival", nil)
	if why, _ := out["stopped_for"].(map[string]any); why["code"] != "app.layer_full" {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}
}

// A game server over its allowance on the first check has its day as well, and
// the warning is on its page.
func TestAGameServerOverOnTheFirstCheckIsWarned(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	e.ownFiles("survival", 40*gb)

	e.s.checkLayers(context.Background())
	if e.stopped(t, "survival") {
		t.Fatal("the server was stopped by the first check")
	}
	until, code := e.graceOf(t, "/api/games/survival")
	if !until.Equal(e.s.now().Add(24*time.Hour)) || code != "app.layer_full" {
		t.Errorf("grace until %v for %q", until, code)
	}
}

// The files are checked by a job of their own, woken when a container starts,
// so an app with no volume at all is looked at while no volume exists
// anywhere, and the job ends when nothing runs.
func TestTheLayerJobChecksAnAppWithoutVolumes(t *testing.T) {
	e := newAppEnv(t)
	if _, _, err := e.s.Store.FirstLayerCheck(context.Background(), e.s.now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	armLoops(t, e)
	if e.s.loops.running("layers") {
		t.Fatal("the layer check runs with no container")
	}
	e.ownFiles("web", 6*gb)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	waitFor(t, func() bool { return e.stopped(t, "web") })

	waitFor(t, func() bool { return !e.s.loops.running("layers") })
	e.core.mu.Lock()
	calls := e.core.layerCalls
	e.core.mu.Unlock()
	if calls == 0 {
		t.Error("the core was never asked")
	}
}

// Starting a game server wakes the check, and it ends once nothing runs.
func TestStartingAGameServerWakesTheLayerCheck(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	armLoops(t, e)
	e.core.mu.Lock()
	before := e.core.layerCalls
	e.core.mu.Unlock()
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	waitFor(t, func() bool {
		e.core.mu.Lock()
		defer e.core.mu.Unlock()
		return e.core.layerCalls > before
	})
	e.power(t, "survival", "stop")
	waitFor(t, func() bool { return len(e.running("survival")) == 0 })
}

// With no container running there is nothing to look at, which a caller that
// only checks while something runs can tell from the answer.
func TestLayerCheckSaysWhetherAnythingRuns(t *testing.T) {
	e := newAppEnv(t)
	if e.s.checkLayers(context.Background()) {
		t.Error("nothing runs, but the check says there is more to look at")
	}
	e.newImageApp(t, "web", nil)
	if !e.s.checkLayers(context.Background()) {
		t.Error("a container runs, but the check says there is nothing to look at")
	}
	e.b.do("POST", "/api/apps/web/stop", nil)
	if e.s.checkLayers(context.Background()) {
		t.Error("the container stopped, but the check goes on")
	}
}

func TestOnlyContainersNamedForADeploymentRunTheApp(t *testing.T) {
	tests := []struct {
		app, container string
		want           bool
	}{
		{"web", "web-12", true},
		{"web", "web-7-test", false},
		{"web", "web-install-3", false},
		{"web", "web-", false},
		{"web", "web", false},
		{"web", "web--5", false},
		{"web", "webs-5", false},
		{"web-1", "web-1-5", true},
		{"web", "web-1-5", false},
		{"", "zelie-build-plan", false},
	}
	for _, tc := range tests {
		if got := isServiceContainer(tc.app, tc.container); got != tc.want {
			t.Errorf("isServiceContainer(%q, %q) = %v, want %v", tc.app, tc.container, got, tc.want)
		}
	}
}

// A test run that writes a lot (a browser download, a package cache) is a
// container of the app too. It must not take the live version down: only the
// run is stopped, and the deployment fails with the reason.
func TestATestRunOverTheLimitFailsItsDeploymentAndLeavesTheLiveAppRunning(t *testing.T) {
	e := newAppEnv(t)
	e.core.suggest = "npm test"
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	live := e.settle(t, "web")
	if live.State != store.DeployLive {
		t.Fatalf("first deployment %+v", live)
	}
	liveContainer := fmt.Sprintf("web-%d", live.ID)

	e.core.mu.Lock()
	e.core.holdSteps = true
	e.core.mu.Unlock()
	e.source.commit = strings.Repeat("b", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	var run string
	waitFor(t, func() bool {
		e.core.mu.Lock()
		defer e.core.mu.Unlock()
		if len(e.core.tests) != 2 {
			return false
		}
		run = e.core.tests[1].ID
		return true
	})

	e.core.mu.Lock()
	e.core.layerBytes = map[string]int64{liveContainer: 20 * mb, run: 6 * gb}
	e.core.mu.Unlock()
	if !e.s.checkLayers(context.Background()) {
		t.Error("containers run, but the check says there is nothing to look at")
	}

	d := e.settle(t, "web")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "app.step_full" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
	if p := d.Error.Params; p["outside"] != "6 GB" || p["limit"] != "5 GB" {
		t.Errorf("params %v", p)
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); !strings.Contains(b, "The container was stopped after it wrote 6 GB outside any volume") {
		t.Errorf("the log does not say why:\n%s", b)
	}
	if e.stopped(t, "web") {
		t.Error("the app was stopped because of its test run")
	}
	if st, err := e.s.containerStatus(context.Background(), liveContainer); err != nil || st.State != "running" {
		t.Errorf("the live container: %+v, %v", st, err)
	}
	if now, _ := e.s.Store.LiveDeployment(context.Background(), "web"); now.ID != live.ID {
		t.Error("the live version changed")
	}
	_, out := e.b.do("GET", "/api/apps/web", nil)
	if out["stopped_for"] != nil {
		t.Errorf("stopped_for = %v", out["stopped_for"])
	}
}

// The install of a game server runs in a container of the server, and what it
// writes outside the server's volume is its own matter.
func TestAnInstallOverTheLimitFailsTheInstall(t *testing.T) {
	e, _ := newGameEnv(t)
	e.core.holdSteps = true
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	var installer string
	waitFor(t, func() bool {
		e.core.mu.Lock()
		defer e.core.mu.Unlock()
		if len(e.core.installs) != 1 {
			return false
		}
		installer = e.core.installs[0].ID
		return true
	})
	e.core.mu.Lock()
	e.core.layerUnmeasured = map[string]bool{installer: true}
	e.core.mu.Unlock()
	e.s.checkLayers(context.Background())

	d := e.install(t, "srv")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "app.layer_unmeasured" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
	g, _ := e.s.Store.GameServer(context.Background(), "srv")
	if g.InstallState != store.InstallFailed {
		t.Errorf("install state %q", g.InstallState)
	}
	if e.stopped(t, "srv") {
		t.Error("the server was stopped because of its installer")
	}
}

// Within the limit a test run is left alone, whatever else the app holds.
func TestATestRunWithinTheLimitIsLeftAlone(t *testing.T) {
	e := newAppEnv(t)
	e.core.suggest = "npm test"
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	live := e.settle(t, "web")
	e.core.mu.Lock()
	e.core.holdSteps = true
	e.core.mu.Unlock()
	e.source.commit = strings.Repeat("b", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	var run string
	waitFor(t, func() bool {
		e.core.mu.Lock()
		defer e.core.mu.Unlock()
		if len(e.core.tests) != 2 {
			return false
		}
		run = e.core.tests[1].ID
		return true
	})
	// The two together are over the allowance, but the test run alone is not.
	e.core.mu.Lock()
	e.core.layerBytes = map[string]int64{fmt.Sprintf("web-%d", live.ID): 3 * gb, run: 3 * gb}
	e.core.mu.Unlock()
	e.s.checkLayers(context.Background())
	if st, err := e.s.containerStatus(context.Background(), run); err != nil || st.State != "running" {
		t.Errorf("the test run: %+v, %v", st, err)
	}
	if e.stopped(t, "web") {
		t.Error("the app was stopped")
	}
	// Let the run end, so the deployment does not hang the test.
	e.core.Stop(context.Background(), run, 0)
	e.settle(t, "web")
}

// A test run starts before the app has any container, and writes the most
// while it does. It wakes the check itself.
func TestATestRunOfAFirstDeploymentIsWatchedFromItsStart(t *testing.T) {
	e := newAppEnv(t)
	e.core.suggest = "npm test"
	e.core.holdSteps = true
	e.ownFiles("web", 6*gb)
	armLoops(t, e)
	if e.s.loops.running("layers") {
		t.Fatal("the layer check runs with no container")
	}
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})

	d := e.settle(t, "web")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "app.step_full" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
}

// The reason a job waits for is kept only so long: a container that ended by
// itself as it was measured leaves its entry behind, and the next one clears it.
func TestStoppedStepsAreForgottenWhenNobodyTakesThem(t *testing.T) {
	var st stepStops
	why := errStepFull.Err("outside", "6 GB", "limit", "5 GB")
	st.put("web-1-test", why, layerEpoch)
	st.put("web-2-test", why, layerEpoch.Add(stepStopKeep+time.Second))
	if st.take("web-1-test") != nil {
		t.Error("an entry nobody took stays for good")
	}
	if st.take("web-2-test") != why {
		t.Error("the newest entry is gone")
	}
	if st.take("web-2-test") != nil {
		t.Error("an entry can be taken twice")
	}
	if len(st.why) != 0 {
		t.Errorf("%d entries left", len(st.why))
	}
}

func TestAnInstallIsWatchedFromItsStart(t *testing.T) {
	e, _ := newGameEnv(t)
	e.core.holdSteps = true
	// More than any disk a server is given.
	e.ownFiles("srv", 1<<40)
	armLoops(t, e)
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "srv", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	d := e.install(t, "srv")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "app.step_full" {
		t.Fatalf("deployment %+v %+v", d, d.Error)
	}
}
