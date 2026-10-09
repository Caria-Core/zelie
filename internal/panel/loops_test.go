package panel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
	"github.com/opencontainers/go-digest"
)

func TestLoopStartsOnWakeAndEndsWhenNothingIsLeft(t *testing.T) {
	var l loops
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	var work atomic.Bool
	work.Store(true)
	round := func(context.Context) bool {
		calls.Add(1)
		return work.Load()
	}
	nudge := make(chan struct{}, 1)

	if l.wake("job", time.Hour, nudge, round) || l.running("job") {
		t.Fatal("a job started before the loops were let go")
	}
	l.start(ctx)
	if !l.wake("job", time.Hour, nudge, round) {
		t.Fatal("wake did not start the job")
	}
	waitFor(t, func() bool { return calls.Load() == 1 })
	if l.wake("job", time.Hour, nudge, round) {
		t.Error("wake started a job that was running")
	}

	// The next round finds nothing and the job ends.
	work.Store(false)
	nudge <- struct{}{}
	waitFor(t, func() bool { return !l.running("job") })
	if calls.Load() != 2 {
		t.Errorf("%d rounds, want 2", calls.Load())
	}

	// New work starts it again.
	if !l.wake("job", time.Hour, nudge, round) {
		t.Fatal("wake did not start the job again")
	}
	waitFor(t, func() bool { return calls.Load() == 3 && !l.running("job") })
}

// A round that looked just before the new work was saved says there is none;
// the wake that came meanwhile has to make it look again.
func TestWakeDuringARoundIsNotLost(t *testing.T) {
	var l loops
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.start(ctx)
	var calls atomic.Int32
	inRound, release := make(chan struct{}), make(chan struct{})
	round := func(context.Context) bool {
		if calls.Add(1) == 1 {
			close(inRound)
			<-release
		}
		return false
	}
	l.wake("job", time.Hour, nil, round)
	<-inRound
	if l.wake("job", time.Hour, nil, round) {
		t.Fatal("started a second copy of a running job")
	}
	close(release)
	waitFor(t, func() bool { return calls.Load() == 2 && !l.running("job") })
}

func TestLoopsEndWithTheirContext(t *testing.T) {
	var l loops
	ctx, cancel := context.WithCancel(context.Background())
	l.start(ctx)
	l.wake("job", time.Hour, nil, func(context.Context) bool { return true })
	waitFor(t, func() bool { return l.running("job") })
	cancel()
	waitFor(t, func() bool { return !l.running("job") })
	if l.wake("job", time.Hour, nil, func(context.Context) bool { return true }) {
		t.Error("a job started after shutdown")
	}
}

// running reports whether the named job is running.
func (l *loops) running(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.jobs[name]
	return ok
}

// armLoops lets the background jobs run in a test, with short rounds, and
// ends them with it.
func armLoops(t *testing.T, e *appEnv) {
	t.Helper()
	oldSupervise, oldSchedule, oldBackup, oldVolume := superviseEvery, scheduleEvery, backupEvery, volumeCheckEvery
	superviseEvery, scheduleEvery, backupEvery, volumeCheckEvery = 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	e.s.loops.start(ctx)
	t.Cleanup(func() {
		cancel()
		// A deployment on its way would still wake the jobs.
		e.s.deploys.wg.Wait()
		e.s.jobs.Wait()
		waitFor(t, func() bool {
			e.s.loops.mu.Lock()
			defer e.s.loops.mu.Unlock()
			return len(e.s.loops.jobs) == 0
		})
		superviseEvery, scheduleEvery, backupEvery, volumeCheckEvery = oldSupervise, oldSchedule, oldBackup, oldVolume
	})
}

// On a server that uses none of it, no round has anything to watch, and none
// asks the core or the proxy for anything.
func TestNothingRunsOnAnEmptyServer(t *testing.T) {
	e := newAppEnv(t)
	ctx := context.Background()
	rounds := map[string]func(context.Context) bool{
		"supervise": e.s.superviseOnce, "metrics": e.s.recordMetrics, "image-check": e.s.checkImages, "volumes": e.s.checkVolumes,
		"steam": e.s.steamRound, "schedules": e.s.schedulesOnce, "backups": e.s.backupsOnce, "uploads": e.s.uploadsOnce,
	}
	for name, round := range rounds {
		if round(ctx) {
			t.Errorf("%s has nothing to watch and wants to go on", name)
		}
	}
	e.core.mu.Lock()
	lists := e.core.lists
	e.core.mu.Unlock()
	e.proxy.mu.Lock()
	reads := e.proxy.statsCalls
	e.proxy.mu.Unlock()
	if lists != 0 || reads != 0 {
		t.Errorf("an empty server asked the core for its containers %d times and the proxy for its counts %d times", lists, reads)
	}
}

// pinNginx has the core pin nginx to a digest and the registry agree, so the
// image check has something to follow.
func pinNginx(e *appEnv) {
	e.core.digests = map[string]string{"nginx": digestN("1").String()}
	e.s.Registry = &fakeRegistry{tags: map[string]digest.Digest{"nginx": digestN("1")}}
}

func TestRoundsGoOnWhileTheirFeatureIsUsed(t *testing.T) {
	e := newAppEnv(t)
	pinNginx(e)
	ctx := context.Background()
	check := func(name string, round func(context.Context) bool, want bool) {
		t.Helper()
		if got := round(ctx); got != want {
			t.Errorf("%s wants to go on: %v, want %v", name, got, want)
		}
	}

	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 100}}})
	e.settle(t, "web")
	check("supervise", e.s.superviseOnce, true)
	check("metrics", e.s.recordMetrics, true)
	check("image check", e.s.checkImages, true)
	check("volumes", e.s.checkVolumes, true)

	// An app has no backup plan until it gets one, and then the clock runs.
	check("backups", e.s.backupsOnce, false)
	plan := map[string]any{"enabled": true, "minute": 180, "keep_days": 7}
	if code, out := e.b.do("PUT", "/api/apps/web/backups/plan", plan); code != http.StatusOK {
		t.Fatalf("plan: %d %v", code, out)
	}
	check("backups with a plan", e.s.backupsOnce, true)
	plan["enabled"] = false
	e.b.do("PUT", "/api/apps/web/backups/plan", plan)
	check("backups with the plan off", e.s.backupsOnce, false)
	// The newest good backup of an app stays whatever its age, so it is
	// nothing to watch until a newer one makes it something.
	first := int64(e.backUp(t, "web")["id"].(float64))
	check("backups with one backup", e.s.backupsOnce, false)
	e.backUp(t, "web")
	check("backups with an old one to expire", e.s.backupsOnce, true)
	if code, _ := e.b.do("DELETE", fmt.Sprintf("/api/backups/%d", first), nil); code != http.StatusNoContent {
		t.Fatalf("delete backup: %d", code)
	}
	check("backups with the newest left", e.s.backupsOnce, false)

	// Stopped, the app needs no supervisor and no measuring, but its tag is
	// still followed and its volume still measured.
	if code, _ := e.b.do("POST", "/api/apps/web/stop", nil); code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
	e.core.mu.Lock()
	lists := e.core.lists
	e.core.mu.Unlock()
	e.proxy.mu.Lock()
	reads := e.proxy.statsCalls
	e.proxy.mu.Unlock()
	check("supervise", e.s.superviseOnce, false)
	check("metrics", e.s.recordMetrics, false)
	check("image check", e.s.checkImages, true)
	check("volumes", e.s.checkVolumes, true)
	e.core.mu.Lock()
	listedAgain := e.core.lists != lists
	e.core.mu.Unlock()
	e.proxy.mu.Lock()
	readAgain := e.proxy.statsCalls != reads
	e.proxy.mu.Unlock()
	if listedAgain || readAgain {
		t.Error("with every app stopped, the supervisor listed containers or the metrics read the proxy")
	}

	if code, _ := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete app: %d", code)
	}
	check("image check", e.s.checkImages, false)
	check("volumes", e.s.checkVolumes, false)
	// The backup outlives its app and expires like any other.
	check("backups of a deleted app", e.s.backupsOnce, true)
}

// offsiteDown is a core that cannot say where backups go.
type offsiteDown struct{ *appCore }

func (offsiteDown) Offsite(context.Context) (core.OffsiteInfo, error) {
	return core.OffsiteInfo{}, errors.New("the core is restarting")
}

func TestUploadRoundWaitsOnlyForCopiesItCanSend(t *testing.T) {
	e := newAppEnv(t)
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "s"})

	e.core.mu.Lock()
	e.core.off.failUpload = 1
	e.core.mu.Unlock()
	e.backUp(t, "pg")
	// The send fails and is tried again in a minute: until then the sender
	// has nothing to do but stay.
	for range 2 {
		if !e.s.uploadsOnce(ctx) {
			t.Fatal("the sender gave up on a copy that waits for its next try")
		}
	}
	now = now.Add(time.Minute)
	if !e.s.uploadsOnce(ctx) {
		t.Fatal("the sender stopped in the round that sent")
	}
	if e.s.uploadsOnce(ctx) {
		t.Error("the sender goes on with nothing left to send")
	}

	// A copy that waits where nothing can take it: the sender ends, and
	// setting a destination calls it back.
	e.backUp(t, "pg")
	e.core.mu.Lock()
	e.core.off.info = core.OffsiteInfo{}
	e.core.mu.Unlock()
	if e.s.uploadsOnce(ctx) {
		t.Error("the sender goes on with nowhere to send to")
	}

	// A core that cannot be asked is no reason to end it.
	e.s.Core = offsiteDown{e.core}
	if !e.s.uploadsOnce(ctx) {
		t.Error("the sender ended because the core did not answer")
	}
}

func TestMetricsOfAFailedProxyReadAreNotTheWholeCount(t *testing.T) {
	e := newAppEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx", "domain": "web.example.com"})
	e.settle(t, "web")
	ctx := context.Background()
	started := now
	count := func(requests uint64) {
		e.proxy.mu.Lock()
		defer e.proxy.mu.Unlock()
		e.proxy.stats = proxy.Stats{Since: started, Hosts: map[string]proxy.Counts{"web.example.com": {Requests: requests}}}
	}
	fail := func(err error) {
		e.proxy.mu.Lock()
		defer e.proxy.mu.Unlock()
		e.proxy.statsErr = err
	}
	minute := func() {
		now = now.Add(time.Minute)
		e.s.recordMetrics(ctx)
	}

	count(100)
	e.s.recordMetrics(ctx)
	count(160)
	minute()
	// The proxy does not answer once, though it keeps running and counting.
	fail(errors.New("timeout"))
	count(200)
	minute()
	fail(nil)
	count(220)
	minute()

	list, err := e.s.Store.Metrics(ctx, "web", time.Time{})
	if err != nil || len(list) != 3 {
		t.Fatalf("metrics %+v: %v", list, err)
	}
	if list[0].Requests != 60 {
		t.Errorf("first minute %d requests", list[0].Requests)
	}
	if list[1].Requests != 0 {
		t.Errorf("the minute without an answer has %d requests", list[1].Requests)
	}
	// What came in during the minute before is counted with this one.
	if list[2].Requests != 60 {
		t.Errorf("minute after the missed one has %d requests, want 60", list[2].Requests)
	}
}

func TestFirstMetricsReadingWithoutTheProxyGivesNoCount(t *testing.T) {
	e := newAppEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx", "domain": "web.example.com"})
	e.settle(t, "web")
	ctx := context.Background()
	e.proxy.mu.Lock()
	e.proxy.statsErr = errors.New("timeout")
	e.proxy.mu.Unlock()
	e.s.recordMetrics(ctx)

	// Counting is already far along when the proxy first answers.
	e.proxy.mu.Lock()
	e.proxy.statsErr = nil
	e.proxy.stats = proxy.Stats{Since: now.Add(-24 * time.Hour), Hosts: map[string]proxy.Counts{"web.example.com": {Requests: 500000}}}
	e.proxy.mu.Unlock()
	now = now.Add(time.Minute)
	e.s.recordMetrics(ctx)
	e.proxy.mu.Lock()
	e.proxy.stats.Hosts = map[string]proxy.Counts{"web.example.com": {Requests: 500060}}
	e.proxy.mu.Unlock()
	now = now.Add(time.Minute)
	e.s.recordMetrics(ctx)

	list, err := e.s.Store.Metrics(ctx, "web", time.Time{})
	if err != nil || len(list) != 2 {
		t.Fatalf("metrics %+v: %v", list, err)
	}
	if list[0].Requests != 0 || list[1].Requests != 60 {
		t.Errorf("requests %d then %d, want 0 then 60", list[0].Requests, list[1].Requests)
	}
}

func TestRunningAppsWakeTheirWatchers(t *testing.T) {
	e := newAppEnv(t)
	pinNginx(e)
	armLoops(t, e)
	watchers := []string{"supervise", "metrics", "image-check"}
	for _, name := range watchers {
		if e.s.loops.running(name) {
			t.Fatalf("%s runs on a server with no app", name)
		}
	}
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	for _, name := range watchers {
		waitFor(t, func() bool { return e.s.loops.running(name) })
	}

	// The supervisor is really watching.
	e.crash(t, "web")
	waitFor(t, func() bool {
		list, _ := e.s.Store.Deployments(context.Background(), "web", 10)
		return slices.ContainsFunc(list, func(d store.Deployment) bool { return d.Cause == store.CauseRecover && d.State == store.DeployLive })
	})

	// It ends with the last running app, and starts again with the next start.
	if code, _ := e.b.do("POST", "/api/apps/web/stop", nil); code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
	waitFor(t, func() bool { return !e.s.loops.running("supervise") })
	if code, _ := e.b.do("POST", "/api/apps/web/start", nil); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	waitFor(t, func() bool { return e.s.loops.running("supervise") })
}

// A start that fails before the app goes live leaves the version that was
// live to be brought back, which is the supervisor's work.
func TestStartedAppIsWatchedEvenIfItsDeploymentFails(t *testing.T) {
	e := newAppEnv(t)
	armLoops(t, e)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/apps/web/stop", nil)
	waitFor(t, func() bool { return !e.s.loops.running("supervise") })

	e.core.mu.Lock()
	e.core.crash = true
	e.core.mu.Unlock()
	if code, _ := e.b.do("POST", "/api/apps/web/start", nil); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	if d := e.settle(t, "web"); d.State != store.DeployFailed {
		t.Fatalf("deployment %+v", d)
	}
	if !e.s.loops.running("supervise") {
		t.Error("nothing watches an app that was started and did not come up")
	}
}

// A start wakes the supervisor while its own deployment is being recorded. The
// app is not live yet, and that is no crash.
func TestStartingTheOnlyAppIsNotACrash(t *testing.T) {
	e := newAppEnv(t)
	armLoops(t, e)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	for i := range 40 {
		if code, _ := e.b.do("POST", "/api/apps/web/stop", nil); code != http.StatusNoContent {
			t.Fatalf("stop: %d", code)
		}
		waitFor(t, func() bool { return !e.s.loops.running("supervise") })
		if code, _ := e.b.do("POST", "/api/apps/web/start", nil); code != http.StatusCreated {
			t.Fatalf("start: %d", code)
		}
		e.settle(t, "web")
		if e.recovered("web") {
			t.Fatalf("start %d: the supervisor brought back an app that was being started", i)
		}
	}
}

func TestScheduleClockRunsWhileAScheduleIsOn(t *testing.T) {
	se := newScheduleEnv(t)
	armLoops(t, se.appEnv)
	if se.s.loops.running("schedules") {
		t.Fatal("the schedule clock runs with no schedule")
	}
	id := se.create(t, map[string]any{"tasks": []any{cmd("say hi")}})
	waitFor(t, func() bool { return se.s.loops.running("schedules") })

	path := fmt.Sprintf("/api/games/srv/schedules/%d", id)
	body := map[string]any{"name": "nightly", "cron": "30 8 * * *", "enabled": false, "tasks": []any{cmd("say hi")}}
	if code, out := se.b.do("PUT", path, body); code != http.StatusOK {
		t.Fatalf("turn off: %d %v", code, out)
	}
	waitFor(t, func() bool { return !se.s.loops.running("schedules") })
	body["enabled"] = true
	if code, out := se.b.do("PUT", path, body); code != http.StatusOK {
		t.Fatalf("turn on: %d %v", code, out)
	}
	waitFor(t, func() bool { return se.s.loops.running("schedules") })
	if code, _ := se.b.do("DELETE", path, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	waitFor(t, func() bool { return !se.s.loops.running("schedules") })
}

func TestBackupClockRunsWhileThereIsAPlanOrABackup(t *testing.T) {
	e := newAppEnv(t)
	armLoops(t, e)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 100}}})
	e.settle(t, "web")
	if e.s.loops.running("backups") {
		t.Fatal("the backup clock runs with no plan and no backup")
	}

	plan := map[string]any{"enabled": true, "minute": 180, "keep_days": 7}
	e.b.do("PUT", "/api/apps/web/backups/plan", plan)
	waitFor(t, func() bool { return e.s.loops.running("backups") })
	plan["enabled"] = false
	e.b.do("PUT", "/api/apps/web/backups/plan", plan)
	waitFor(t, func() bool { return !e.s.loops.running("backups") })

	// The newest good backup of an app stays however old it gets, so it
	// needs no clock. A newer one makes it something to expire.
	first := int64(e.backUp(t, "web")["id"].(float64))
	waitFor(t, func() bool { return !e.s.loops.running("backups") })
	e.backUp(t, "web")
	waitFor(t, func() bool { return e.s.loops.running("backups") })
	if code, _ := e.b.do("DELETE", fmt.Sprintf("/api/backups/%d", first), nil); code != http.StatusNoContent {
		t.Fatalf("delete backup: %d", code)
	}
	waitFor(t, func() bool { return !e.s.loops.running("backups") })

	// A new database comes with a plan.
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	waitFor(t, func() bool { return e.s.loops.running("backups") })
	plan["enabled"] = false
	if code, out := e.b.do("PUT", "/api/apps/pg/backups/plan", plan); code != http.StatusOK {
		t.Fatalf("plan: %d %v", code, out)
	}
	waitFor(t, func() bool { return !e.s.loops.running("backups") })

	// The last backup of a deleted app expires like any other.
	if code, _ := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete app: %d", code)
	}
	waitFor(t, func() bool { return e.s.loops.running("backups") })
}

// An uploaded dump is a backup like any other: the one before it is no longer
// the newest, so it has a day when it goes.
func TestUploadedDumpWakesTheBackupClock(t *testing.T) {
	e := newAppEnv(t)
	armLoops(t, e)
	ctx := context.Background()
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	plan := map[string]any{"enabled": false, "minute": 180, "keep_days": 7}
	if code, out := e.b.do("PUT", "/api/apps/pg/backups/plan", plan); code != http.StatusOK {
		t.Fatalf("plan: %d %v", code, out)
	}
	e.backUp(t, "pg")
	waitFor(t, func() bool { return !e.s.loops.running("backups") })

	up, err := e.s.Core.StartUpload(ctx, "pg", 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.Core.AppendUpload(ctx, up.ID, 0, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	a, err := e.s.Store.App(ctx, "pg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.importDump(ctx, a, dumpUpload{ID: up.ID, App: "pg", Name: "shop.sql", Size: 4}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.s.loops.running("backups") })
}

func TestSenderRunsWhileACopyWaits(t *testing.T) {
	e := newAppEnv(t)
	armLoops(t, e)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	row := func(id int64) store.Backup {
		t.Helper()
		b, err := e.s.Store.Backup(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// With nowhere to send it, a backup is not queued.
	early := int64(e.backUp(t, "pg")["id"].(float64))
	if e.s.loops.running("uploads") {
		t.Fatal("the sender runs with nothing to send")
	}

	e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "s"})
	e.core.mu.Lock()
	e.core.off.failUpload = 1
	e.core.mu.Unlock()
	failing := int64(e.backUp(t, "pg")["id"].(float64))
	waitFor(t, func() bool { return row(failing).Offsite == store.OffsiteFailed })
	// Its next try is in a minute; the sender stays for it.
	if !e.s.loops.running("uploads") {
		t.Error("the sender left a copy that waits for its next try")
	}

	// A destination that came after the backup: sending it by hand starts
	// the sender, which goes when it is done.
	if code, _ := e.b.do("POST", fmt.Sprintf("/api/backups/%d/offsite", early), nil); code != http.StatusAccepted {
		t.Fatalf("send by hand: %d", code)
	}
	waitFor(t, func() bool { return row(early).Offsite == store.OffsiteDone })
	if code, _ := e.b.do("POST", fmt.Sprintf("/api/backups/%d/offsite", failing), nil); code != http.StatusAccepted {
		t.Fatalf("send again: %d", code)
	}
	waitFor(t, func() bool { return row(failing).Offsite == store.OffsiteDone })
	waitFor(t, func() bool { return !e.s.loops.running("uploads") })
}

func TestVolumeCheckRunsWhileAVolumeExists(t *testing.T) {
	e := newAppEnv(t)
	// Deleting a volume needs the app stopped. It is stopped before the jobs
	// are let go, so the supervisor cannot meet the stop halfway.
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/apps/web/stop", nil)
	armLoops(t, e)
	if e.s.loops.running("volumes") {
		t.Fatal("the volume check runs with no volume")
	}

	if code, out := e.b.do("POST", "/api/apps/web/volumes", map[string]any{"path": "/data", "limit_mb": 100}); code != http.StatusCreated {
		t.Fatalf("add volume: %d %v", code, out)
	}
	waitFor(t, func() bool { return e.s.loops.running("volumes") })
	vol := volumesOf(t, e, "web")[0]
	if code, out := e.b.do("DELETE", fmt.Sprintf("/api/apps/web/volumes/%d", vol.ID), nil); code != http.StatusNoContent {
		t.Fatalf("delete volume: %d %v", code, out)
	}
	waitFor(t, func() bool { return !e.s.loops.running("volumes") })
}

func TestSteamCheckRunsForSteamServers(t *testing.T) {
	e, sc := newSteamEnv(t)
	ctx := context.Background()
	e.newGame(t, "mc", "https://example.com/e.js", map[string]any{"ports": 1})
	if e.s.steamRound(ctx) {
		t.Error("a server that is not from Steam keeps the check going")
	}
	sc.mu.Lock()
	asked := len(sc.runs)
	sc.mu.Unlock()
	if asked != 0 {
		t.Errorf("SteamCMD ran %d times with no Steam server", asked)
	}

	armLoops(t, e)
	e.newRust(t)
	waitFor(t, func() bool { return e.s.loops.running("steam") })
}

// On a server with no plan and no backup of its own, backups found off-site
// still have a day when they go, and only the clock can end them.
func TestFoundBackupsAreExpiredByTheBackupClock(t *testing.T) {
	e := newAppEnv(t)
	var clock atomic.Int64
	clock.Store(e.s.now().Unix())
	e.s.Now = func() time.Time { return time.Unix(clock.Load(), 0) }
	armLoops(t, e)
	e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "s"})
	e.core.off.remote = map[string]bool{"old-db/20260101T030000Z-aaaa.sql.zst.age": true}
	e.core.off.meta = map[string]core.BackupMeta{"old-db/20260101T030000Z-aaaa.sql.zst.age": {Engine: "postgres"}}
	if code, out := e.b.do("POST", "/api/backups/keys", map[string]any{"recovery": "# x\nAGE-SECRET-KEY-1ABC\n"}); code != http.StatusOK {
		t.Fatalf("add key: %d %v", code, out)
	}
	if e.s.loops.running("backups") {
		t.Fatal("the backup clock runs with no plan and no backup")
	}

	if _, out := e.b.do("POST", "/api/offsite/found", nil); out["added"] != 1.0 {
		t.Fatalf("add: %v", out)
	}
	waitFor(t, func() bool { return e.s.loops.running("backups") })
	left := func() int {
		list, _ := e.s.Store.Backups(context.Background(), "")
		return len(list)
	}
	if left() != 1 {
		t.Fatalf("%d backups on the list", left())
	}
	clock.Add(int64(31 * 24 * time.Hour / time.Second))
	waitFor(t, func() bool { return left() == 0 })
	waitFor(t, func() bool { return !e.s.loops.running("backups") })
}

// SRCDS_APPID is how a Source game names its Steam app, so a server whose
// variable is set to one becomes a Steam server without being made again.
const srcdsEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "Source Game",
	"docker_images": {"Java 21": "ghcr.io/example/java:21"},
	"startup": "./srcds -port {{server.build.default.port}}",
	"config": {"stop": "quit", "startup": "{\"done\": [\"Ready\"]}", "files": "{}"},
	"scripts": {"installation": {"script": "#!/bin/bash\necho installing\n", "container": "ghcr.io/example/installer:latest", "entrypoint": "bash"}},
	"variables": [
		{"name": "Steam app", "env_variable": "SRCDS_APPID", "default_value": "", "user_viewable": true, "user_editable": true, "rules": "nullable|string"}
	]
}`

func TestSteamCheckStartsWhenAVariableNamesTheApp(t *testing.T) {
	e, _ := newSteamEnv(t)
	const url = "https://example.com/srcds.json"
	e.s.Eggs.(*fakeEggs).files[url] = srcdsEgg
	e.newGame(t, "src", url, map[string]any{"ports": 1})
	armLoops(t, e)
	if e.s.steamRound(context.Background()) || e.s.loops.running("steam") {
		t.Fatal("the Steam check has a server to follow before the variable is set")
	}

	body := map[string]any{"variables": map[string]string{"SRCDS_APPID": "258550"}}
	if code, out := e.b.do("PUT", "/api/games/src/variables", body); code != http.StatusOK {
		t.Fatalf("variables: %d %v", code, out)
	}
	waitFor(t, func() bool { return e.s.loops.running("steam") })
}

func TestCloneOfASteamServerWakesTheSteamCheck(t *testing.T) {
	e := newRustEnv(t)
	output, err := os.ReadFile("../steam/testdata/app_info.txt")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Core = &steamCore{appCore: e.core, output: string(output), manifest: manifestWith("20900000")}
	e.newRust(t)
	armLoops(t, e)
	if e.s.loops.running("steam") {
		t.Fatal("the Steam check runs before the jobs are let go")
	}

	e.clone(t, "rusty", "rusty-copy")
	waitFor(t, func() bool { return e.s.loops.running("steam") })
}

func TestImageCheckRunsForPinnedImages(t *testing.T) {
	e := newAppEnv(t)
	pinNginx(e)
	armLoops(t, e)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	waitFor(t, func() bool { return !e.s.loops.running("image-check") })
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	waitFor(t, func() bool { return e.s.loops.running("image-check") })
}
