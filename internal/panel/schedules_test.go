package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

// scheduleEnv is a game server that is running, with a clock the test moves
// and delays that are recorded instead of waited for.
type scheduleEnv struct {
	*appEnv
	clock  time.Time
	mu     sync.Mutex
	delays []time.Duration
	gate   chan struct{} // when set, every delay waits for it
}

func newScheduleEnv(t *testing.T) *scheduleEnv {
	t.Helper()
	e := newPowerEnv(t)
	se := &scheduleEnv{appEnv: e, clock: time.Unix(1_800_000_000, 0).UTC()}
	e.s.Now = func() time.Time { return se.clock }
	old, oldPoll := scheduleSleep, schedulePoll
	scheduleSleep = func(ctx context.Context, d time.Duration) error {
		se.mu.Lock()
		se.delays = append(se.delays, d)
		gate := se.gate
		se.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	schedulePoll = 2 * time.Millisecond
	t.Cleanup(func() { scheduleSleep, schedulePoll = old, oldPoll })
	e.newGame(t, "srv", "https://example.com/e.js", map[string]any{"ports": 1})
	return se
}

func (se *scheduleEnv) at(hour, minute int) {
	se.clock = time.Date(se.clock.Year(), se.clock.Month(), se.clock.Day(), hour, minute, 0, 0, time.UTC)
}

func (se *scheduleEnv) tick(t *testing.T) {
	t.Helper()
	se.s.schedulesOnce(context.Background())
	se.s.jobs.Wait()
}

func (se *scheduleEnv) create(t *testing.T, body map[string]any) int64 {
	t.Helper()
	if _, ok := body["cron"]; !ok {
		body["cron"] = "30 8 * * *"
	}
	if _, ok := body["name"]; !ok {
		body["name"] = "nightly"
	}
	if _, ok := body["enabled"]; !ok {
		body["enabled"] = true
	}
	code, out := se.b.do("POST", "/api/games/srv/schedules", body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	return int64(out["id"].(float64))
}

func (se *scheduleEnv) schedule(t *testing.T, id int64) store.Schedule {
	t.Helper()
	x, err := se.s.Store.Schedule(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (se *scheduleEnv) backupCount(t *testing.T) int {
	t.Helper()
	list, err := se.s.Store.Backups(context.Background(), "srv")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range list {
		if b.Reason == store.BackupSchedule && b.State == store.BackupDone {
			n++
		}
	}
	return n
}

func cmd(text string) map[string]any { return map[string]any{"action": "command", "data": text} }
func power(action string) map[string]any {
	return map[string]any{"action": "power", "data": action}
}
func backupTask() map[string]any { return map[string]any{"action": "backup"} }

func TestScheduleAPI(t *testing.T) {
	se := newScheduleEnv(t)
	id := se.create(t, map[string]any{"tasks": []any{cmd("say hi"), map[string]any{"action": "backup", "delay": 30, "continue": true}}})

	code, out := se.b.do("GET", "/api/games/srv/schedules", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, out)
	}
	list := out["schedules"].([]any)
	x := list[0].(map[string]any)
	if len(list) != 1 || x["name"] != "nightly" || x["cron"] != "30 8 * * *" || x["running"] != false || x["last_state"] != nil {
		t.Fatalf("schedule %v", x)
	}
	// Made at 08:00 UTC, so the first run is at 08:30 the same day.
	next, _ := time.Parse(time.RFC3339, x["next_run_at"].(string))
	if !next.Equal(time.Date(2027, 1, 15, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("next run %v", x["next_run_at"])
	}
	tasks := x["tasks"].([]any)
	if len(tasks) != 2 || tasks[1].(map[string]any)["delay"] != 30.0 || tasks[1].(map[string]any)["continue"] != true {
		t.Errorf("tasks %v", tasks)
	}

	// Changing it replaces the tasks; turning it off takes the next run away.
	code, out = se.b.do("PUT", fmt.Sprintf("/api/games/srv/schedules/%d", id), map[string]any{
		"name": "later", "cron": " 0  5 * * 1 ", "enabled": false, "tasks": []any{power("restart")},
	})
	if code != http.StatusOK || out["cron"] != "0 5 * * 1" || out["next_run_at"] != nil || len(out["tasks"].([]any)) != 1 {
		t.Fatalf("update: %d %v", code, out)
	}

	bad := map[string]struct {
		body map[string]any
		code string
	}{
		"no name":     {map[string]any{"name": " ", "cron": "* * * * *", "tasks": []any{backupTask()}}, "schedule.bad_name"},
		"bad cron":    {map[string]any{"name": "x", "cron": "61 * * * *", "tasks": []any{backupTask()}}, "schedule.bad_cron"},
		"no tasks":    {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{}}, "schedule.no_tasks"},
		"bad action":  {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{map[string]any{"action": "format"}}}, "schedule.bad_task"},
		"bad power":   {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{power("explode")}}, "game.bad_power"},
		"empty":       {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{cmd(" ")}}, "schedule.bad_command"},
		"two lines":   {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{cmd("a\nb")}}, "schedule.bad_command"},
		"long delay":  {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{map[string]any{"action": "backup", "delay": 901}}}, "schedule.bad_delay"},
		"short delay": {map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{map[string]any{"action": "backup", "delay": -1}}}, "schedule.bad_delay"},
	}
	for name, tc := range bad {
		if code, out := se.b.do("POST", "/api/games/srv/schedules", tc.body); code != http.StatusBadRequest || out["code"] != tc.code {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}

	// Another server's schedule is not reachable through this one.
	se.newGame(t, "other", "https://example.com/e.js", map[string]any{"ports": 1})
	if code, out := se.b.do("DELETE", fmt.Sprintf("/api/games/other/schedules/%d", id), nil); code != http.StatusNotFound || out["code"] != "schedule.not_found" {
		t.Errorf("foreign delete: %d %v", code, out)
	}
	if code, _ := se.b.do("DELETE", fmt.Sprintf("/api/games/srv/schedules/%d", id), nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

func TestScheduleLimit(t *testing.T) {
	se := newScheduleEnv(t)
	for range maxSchedules {
		se.create(t, map[string]any{"tasks": []any{backupTask()}})
	}
	code, out := se.b.do("POST", "/api/games/srv/schedules", map[string]any{"name": "x", "cron": "* * * * *", "tasks": []any{backupTask()}})
	if code != http.StatusConflict || out["code"] != "schedule.too_many" {
		t.Errorf("%d %v", code, out)
	}
}

func TestSchedulerRunsEachSlotOnce(t *testing.T) {
	se := newScheduleEnv(t)
	id := se.create(t, map[string]any{"tasks": []any{cmd("say hi"), map[string]any{"action": "backup", "delay": 30}}})
	container := se.startGame(t, "srv")

	se.at(8, 29)
	se.tick(t)
	if se.backupCount(t) != 0 || se.schedule(t, id).LastState != "" {
		t.Fatal("it ran before its time")
	}

	se.at(8, 30)
	se.clock = se.clock.Add(20 * time.Second)
	se.tick(t)
	x := se.schedule(t, id)
	if x.LastState != store.RunDone || x.LastError != nil || !x.LastRun.Equal(se.clock) {
		t.Fatalf("schedule %+v", x)
	}
	if got := se.stdin(container); !slices.Equal(got, []string{"say hi\n"}) {
		t.Errorf("console %q", got)
	}
	if se.backupCount(t) != 1 {
		t.Errorf("backups %d", se.backupCount(t))
	}
	if !slices.Equal(se.delays, []time.Duration{0, 30 * time.Second}) {
		t.Errorf("delays %v", se.delays)
	}

	// Looked at again during the same minute, and the next: nothing runs,
	// and a panel that restarts does not start over either.
	se.tick(t)
	se.at(8, 31)
	se.tick(t)
	if se.backupCount(t) != 1 || len(se.stdin(container)) != 1 {
		t.Errorf("it ran again: %d backups", se.backupCount(t))
	}

	// Tomorrow's slot runs.
	se.clock = se.clock.AddDate(0, 0, 1)
	se.at(8, 30)
	se.tick(t)
	if se.backupCount(t) != 2 {
		t.Errorf("backups %d", se.backupCount(t))
	}
}

func TestSchedulerSkipsAMissedSlot(t *testing.T) {
	se := newScheduleEnv(t)
	id := se.create(t, map[string]any{"tasks": []any{backupTask()}})

	// The panel was down at 08:30 and comes back ten minutes late.
	se.at(8, 40)
	se.tick(t)
	if se.backupCount(t) != 0 || se.schedule(t, id).LastState != "" {
		t.Fatal("a slot that old was run")
	}
	// A few minutes late is still run.
	se.clock = se.clock.AddDate(0, 0, 1)
	se.at(8, 33)
	se.tick(t)
	if se.backupCount(t) != 1 {
		t.Errorf("backups %d", se.backupCount(t))
	}
}

func TestSchedulerIgnoresChangesMadeInTheSameMinute(t *testing.T) {
	se := newScheduleEnv(t)
	se.at(8, 30)
	// Made at 08:30 for 08:30: that minute has begun already.
	id := se.create(t, map[string]any{"tasks": []any{backupTask()}})
	se.tick(t)
	if se.schedule(t, id).LastState != "" {
		t.Error("a new schedule ran for the minute it was made in")
	}
	// A disabled one does not run, and does not catch up when turned on.
	se.b.do("PUT", fmt.Sprintf("/api/games/srv/schedules/%d", id), map[string]any{"name": "n", "cron": "30 8 * * *", "enabled": false, "tasks": []any{backupTask()}})
	se.at(8, 31)
	se.tick(t)
	se.b.do("PUT", fmt.Sprintf("/api/games/srv/schedules/%d", id), map[string]any{"name": "n", "cron": "30 8 * * *", "enabled": true, "tasks": []any{backupTask()}})
	se.at(8, 32)
	se.tick(t)
	if se.backupCount(t) != 0 {
		t.Error("it caught up on a slot from while it was off")
	}
}

func TestSchedulerOnlyWhenRunning(t *testing.T) {
	se := newScheduleEnv(t)
	id := se.create(t, map[string]any{"only_running": true, "tasks": []any{backupTask()}})

	// The server is stopped: the run is skipped, and says so.
	se.at(8, 30)
	se.tick(t)
	x := se.schedule(t, id)
	if x.LastState != store.RunSkipped || se.backupCount(t) != 0 || x.LastRun.IsZero() {
		t.Fatalf("stopped: %+v", x)
	}
	se.startGame(t, "srv")
	se.clock = se.clock.AddDate(0, 0, 1)
	se.at(8, 30)
	se.tick(t)
	if se.schedule(t, id).LastState != store.RunDone || se.backupCount(t) != 1 {
		t.Errorf("running: %+v", se.schedule(t, id))
	}

	// Run now ignores the setting.
	se.s.Store.GameServer(context.Background(), "srv")
	se.power(t, "srv", "stop")
	se.waitState(t, "srv", "stopped")
	if code, out := se.b.do("POST", fmt.Sprintf("/api/games/srv/schedules/%d/run", id), nil); code != http.StatusAccepted {
		t.Fatalf("run now: %d %v", code, out)
	}
	se.s.jobs.Wait()
	if se.backupCount(t) != 2 {
		t.Errorf("run now made %d backups", se.backupCount(t))
	}
}

func TestSchedulerStopsAtAFailureUnlessToldToContinue(t *testing.T) {
	se := newScheduleEnv(t)
	// The server is stopped, so the command cannot be sent.
	stops := se.create(t, map[string]any{"name": "stops", "tasks": []any{cmd("save"), backupTask()}})
	goes := se.create(t, map[string]any{"name": "goes on", "tasks": []any{map[string]any{"action": "command", "data": "save", "continue": true}, backupTask()}})

	se.at(8, 30)
	se.tick(t)
	a, b := se.schedule(t, stops), se.schedule(t, goes)
	if a.LastState != store.RunFailed || a.LastError == nil || a.LastError.Code != "schedule.task_failed" || a.LastError.Params["n"] != 1.0 {
		t.Errorf("stops: %+v %+v", a, a.LastError)
	}
	if b.LastState != store.RunFailed || b.LastError == nil || b.LastError.Params["n"] != 1.0 {
		t.Errorf("goes on: %+v %+v", b, b.LastError)
	}
	// Only the one that goes on made its backup.
	if se.backupCount(t) != 1 {
		t.Errorf("backups %d", se.backupCount(t))
	}
}

func TestSchedulerPowerTasks(t *testing.T) {
	se := newScheduleEnv(t)
	first := se.startGame(t, "srv")
	// Stopping waits for the server to be down, so the backup that follows
	// finds it stopped.
	id := se.create(t, map[string]any{"tasks": []any{power("stop"), backupTask(), power("start"), power("start")}})

	se.at(8, 30)
	se.tick(t)
	se.s.deploys.wg.Wait()
	x := se.schedule(t, id)
	if x.LastState != store.RunDone {
		t.Fatalf("schedule %+v %+v", x, x.LastError)
	}
	if se.backupCount(t) != 1 || se.core.bk.runningFor[0] != 0 {
		t.Errorf("backups %d, running during it %v", se.backupCount(t), se.core.bk.runningFor)
	}
	// Started again, and the second start found it running and was fine.
	if now := se.running("srv"); len(now) != 1 || now[0] == first {
		t.Errorf("running %v after %s", now, first)
	}
}

func TestScheduleNeverRunsTwiceAtOnce(t *testing.T) {
	se := newScheduleEnv(t)
	id := se.create(t, map[string]any{"cron": "* * * * *", "tasks": []any{backupTask()}})
	se.gate = make(chan struct{})

	se.at(8, 30)
	se.s.schedulesOnce(context.Background())
	// Its first task waits for its delay. The next minute is due, and is
	// given up rather than run beside it.
	waitFor(t, func() bool { return se.s.schedulesBusy.has(fmt.Sprint(id)) })
	if code, out := se.b.do("POST", fmt.Sprintf("/api/games/srv/schedules/%d/run", id), nil); code != http.StatusConflict || out["code"] != "schedule.busy" {
		t.Errorf("run now: %d %v", code, out)
	}
	_, out := se.b.do("GET", "/api/games/srv/schedules", nil)
	if out["schedules"].([]any)[0].(map[string]any)["running"] != true {
		t.Errorf("not shown as running: %v", out)
	}
	se.at(8, 31)
	se.s.schedulesOnce(context.Background())
	close(se.gate)
	se.s.jobs.Wait()
	if se.backupCount(t) != 1 {
		t.Errorf("backups %d", se.backupCount(t))
	}
	// The given-up minute is not run afterwards either.
	se.tick(t)
	if se.backupCount(t) != 1 {
		t.Errorf("backups %d", se.backupCount(t))
	}
}

func TestDeletingTheServerDeletesItsSchedules(t *testing.T) {
	se := newScheduleEnv(t)
	se.create(t, map[string]any{"tasks": []any{backupTask()}})
	if code, _ := se.b.do("DELETE", "/api/apps/srv", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if list, err := se.s.Store.Schedules(context.Background(), ""); err != nil || len(list) != 0 {
		t.Errorf("schedules %v %v", list, err)
	}
}

func TestUnfinishedRunsFailWhenThePanelStarts(t *testing.T) {
	se := newScheduleEnv(t)
	id := se.create(t, map[string]any{"tasks": []any{backupTask()}})
	ctx := context.Background()
	se.s.Store.StartRun(ctx, id, se.clock)
	if err := se.s.Store.FailUnfinishedRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if x := se.schedule(t, id); x.LastState != store.RunFailed || x.LastError == nil || x.LastError.Code != "schedule.interrupted" {
		t.Errorf("%+v %+v", x, x.LastError)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatal("timed out")
}
