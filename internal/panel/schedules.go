package panel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/cron"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// A game server's schedule runs its tasks in order when its cron expression
// fires, in the time zone of the panel. One goroutine looks at every
// schedule each scheduleEvery.
//
// A schedule remembers the minute it last fired for. That is claimed in the
// database before the tasks start, so a panel that restarts a moment later
// does not run the same minute again, and a schedule never runs twice at once.
// A minute the panel missed is run only if it is less than scheduleCatchup
// old; otherwise it is skipped, so a panel that was down all night does not
// restart every server at once when it comes back.

// Tests change these.
var (
	scheduleEvery   = 15 * time.Second
	scheduleCatchup = 5 * time.Minute
	// schedulePoll is how often a task that waits for a server to stop
	// looks at it.
	schedulePoll = 500 * time.Millisecond
	// scheduleSleep waits for a task's delay.
	scheduleSleep = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
)

const (
	maxSchedules  = 20
	maxTasks      = 20
	maxTaskDelay  = 900
	maxScheduleNm = 64
)

var (
	errNoSchedule   = msg.Define(http.StatusNotFound, "schedule.not_found", "There is no such schedule.")
	errBadCron      = msg.Define(http.StatusBadRequest, "schedule.bad_cron", "The time is not a valid cron expression: {detail}")
	errScheduleName = msg.Define(http.StatusBadRequest, "schedule.bad_name", "Give the schedule a name of up to 64 characters.")
	errNoTasks      = msg.Define(http.StatusBadRequest, "schedule.no_tasks", "A schedule needs at least one task.")
	errManyTasks    = msg.Define(http.StatusBadRequest, "schedule.too_many_tasks", "A schedule can have at most 20 tasks.")
	errManySchedule = msg.Define(http.StatusConflict, "schedule.too_many", "A server can have at most 20 schedules.")
	errBadTask      = msg.Define(http.StatusBadRequest, "schedule.bad_task", "Task {n} must be a command, a power action or a backup.")
	errTaskCommand  = msg.Define(http.StatusBadRequest, "schedule.bad_command", "The command of task {n} must be one line of up to {max} characters.")
	errTaskDelay    = msg.Define(http.StatusBadRequest, "schedule.bad_delay", "The delay of task {n} must be between 0 and 900 seconds.")
	errScheduleBusy = msg.Define(http.StatusConflict, "schedule.busy", "This schedule is running already.")
	errTaskFailed   = msg.Define(0, "schedule.task_failed", "Task {n} ({action}) failed: {detail}")
	errStopTimeout  = msg.Define(0, "schedule.stop_timeout", "The server did not stop in time.")
)

type taskJSON struct {
	Action string `json:"action"`
	Data   string `json:"data"`
	Delay  int    `json:"delay"`
	// Continue keeps going with the next task if this one fails.
	Continue bool `json:"continue"`
}

// scheduleRequest is what creating or changing a schedule takes.
type scheduleRequest struct {
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	OnlyRunning bool       `json:"only_running"`
	Enabled     bool       `json:"enabled"`
	Tasks       []taskJSON `json:"tasks"`
}

type scheduleJSON struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Cron        string     `json:"cron"`
	OnlyRunning bool       `json:"only_running"`
	Enabled     bool       `json:"enabled"`
	Tasks       []taskJSON `json:"tasks"`
	// NextRunAt is left out for a schedule that is off, or never fires.
	NextRunAt *time.Time `json:"next_run_at,omitempty"`
	LastRunAt *time.Time `json:"last_run_at,omitempty"`
	// LastState is running, done, failed or skipped; empty before the first
	// run.
	LastState string   `json:"last_state,omitempty"`
	LastError *msg.Msg `json:"last_error,omitempty"`
	Running   bool     `json:"running"`
}

type schedulesJSON struct {
	TimeZone  string         `json:"time_zone"`
	Schedules []scheduleJSON `json:"schedules"`
}

func (s *Server) scheduleOut(x store.Schedule) scheduleJSON {
	out := scheduleJSON{ID: x.ID, Name: x.Name, Cron: x.Cron, OnlyRunning: x.OnlyRunning, Enabled: x.Enabled, Tasks: []taskJSON{},
		LastState: x.LastState, LastError: x.LastError, Running: s.schedulesBusy.has(strconv.FormatInt(x.ID, 10))}
	for _, t := range x.Tasks {
		out.Tasks = append(out.Tasks, taskJSON{Action: t.Action, Data: t.Data, Delay: t.Delay, Continue: t.KeepGoing})
	}
	if x.Enabled {
		if cs, err := cron.Parse(x.Cron); err == nil {
			if next := cs.Next(s.now()); !next.IsZero() {
				out.NextRunAt = &next
			}
		}
	}
	if !x.LastRun.IsZero() {
		out.LastRunAt = &x.LastRun
	}
	return out
}

func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Store.Schedules(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "list schedules", err)
		return
	}
	out := schedulesJSON{TimeZone: zoneLabel(s.now()), Schedules: []scheduleJSON{}}
	for _, x := range list {
		out.Schedules = append(out.Schedules, s.scheduleOut(x))
	}
	writeJSON(w, http.StatusOK, out)
}

// checkSchedule turns a request into a schedule, or says what is wrong.
func (s *Server) checkSchedule(req scheduleRequest, app string) (store.Schedule, *msg.Error) {
	x := store.Schedule{AppID: app, Name: strings.TrimSpace(req.Name), Cron: strings.Join(strings.Fields(req.Cron), " "),
		OnlyRunning: req.OnlyRunning, Enabled: req.Enabled}
	if n := len([]rune(x.Name)); n == 0 || n > maxScheduleNm {
		return x, errScheduleName.Err()
	}
	if _, err := cron.Parse(x.Cron); err != nil {
		return x, errBadCron.Err("detail", strings.TrimPrefix(err.Error(), cron.ErrSyntax.Error()+": "))
	}
	switch n := len(req.Tasks); {
	case n == 0:
		return x, errNoTasks.Err()
	case n > maxTasks:
		return x, errManyTasks.Err()
	}
	for i, t := range req.Tasks {
		n := i + 1
		task := store.ScheduleTask{Action: t.Action, Data: t.Data, Delay: t.Delay, KeepGoing: t.Continue}
		if t.Delay < 0 || t.Delay > maxTaskDelay {
			return x, errTaskDelay.Err("n", n)
		}
		switch t.Action {
		case store.TaskCommand:
			task.Data = strings.TrimSpace(t.Data)
			if task.Data == "" || len(task.Data) > consoleMaxData || strings.ContainsAny(task.Data, "\r\n\x00") {
				return x, errTaskCommand.Err("n", n, "max", consoleMaxData)
			}
		case store.TaskPower:
			if !slices.Contains([]string{"start", "stop", "restart", "kill"}, t.Data) {
				return x, errBadPower.Err()
			}
		case store.TaskBackup:
			task.Data = ""
		default:
			return x, errBadTask.Err("n", n)
		}
		x.Tasks = append(x.Tasks, task)
	}
	// A minute that has begun is never run for a schedule made or changed
	// during it.
	x.LastSlot = s.now().Truncate(time.Minute)
	return x, nil
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return
	}
	var req scheduleRequest
	if !decode(w, r, &req) {
		return
	}
	x, merr := s.checkSchedule(req, a.ID)
	if merr != nil {
		writeError(w, merr)
		return
	}
	ctx := r.Context()
	have, err := s.Store.Schedules(ctx, a.ID)
	if err != nil {
		s.fail(w, "list schedules", err)
		return
	}
	if len(have) >= maxSchedules {
		writeError(w, errManySchedule.Err())
		return
	}
	x.CreatedAt = s.now()
	if x.ID, err = s.Store.CreateSchedule(ctx, x); err != nil {
		s.fail(w, "create schedule", err)
		return
	}
	s.Log.Info("schedule created", "server", a.ID, "schedule", x.ID, "name", x.Name, "cron", x.Cron, "tasks", len(x.Tasks), "user", loginFrom(ctx).account.ID)
	created, err := s.Store.Schedule(ctx, x.ID)
	if err != nil {
		s.fail(w, "load schedule", err)
		return
	}
	writeJSON(w, http.StatusCreated, s.scheduleOut(created))
}

// scheduleFrom is the schedule in the path, if it is the server's.
func (s *Server) scheduleFrom(w http.ResponseWriter, r *http.Request) (store.App, store.Schedule, bool) {
	a, _, ok := s.gameFrom(w, r)
	if !ok {
		return a, store.Schedule{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, errNoSchedule.Err())
		return a, store.Schedule{}, false
	}
	x, err := s.Store.Schedule(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || err == nil && x.AppID != a.ID {
		writeError(w, errNoSchedule.Err())
		return a, x, false
	}
	if err != nil {
		s.fail(w, "load schedule", err)
		return a, x, false
	}
	return a, x, true
}

func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	a, old, ok := s.scheduleFrom(w, r)
	if !ok {
		return
	}
	var req scheduleRequest
	if !decode(w, r, &req) {
		return
	}
	x, merr := s.checkSchedule(req, a.ID)
	if merr != nil {
		writeError(w, merr)
		return
	}
	x.ID = old.ID
	ctx := r.Context()
	if err := s.Store.UpdateSchedule(ctx, x); err != nil {
		s.failWith(w, "update schedule", err)
		return
	}
	s.Log.Info("schedule changed", "server", a.ID, "schedule", x.ID, "name", x.Name, "cron", x.Cron, "enabled", x.Enabled, "tasks", len(x.Tasks), "user", loginFrom(ctx).account.ID)
	saved, err := s.Store.Schedule(ctx, x.ID)
	if err != nil {
		s.fail(w, "load schedule", err)
		return
	}
	writeJSON(w, http.StatusOK, s.scheduleOut(saved))
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	a, x, ok := s.scheduleFrom(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteSchedule(r.Context(), x.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, "delete schedule", err)
		return
	}
	s.Log.Info("schedule deleted", "server", a.ID, "schedule", x.ID, "name", x.Name, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// runScheduleNow runs a schedule's tasks now, whatever its time and its
// "only when running" setting say.
func (s *Server) runScheduleNow(w http.ResponseWriter, r *http.Request) {
	_, x, ok := s.scheduleFrom(w, r)
	if !ok {
		return
	}
	if !s.startScheduleRun(s.baseContext(), x.ID, "manual", loginFrom(r.Context()).account.ID) {
		writeError(w, errScheduleBusy.Err())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// runSchedules starts the schedules that are due, until ctx ends.
func (s *Server) runSchedules(ctx context.Context) {
	t := time.NewTicker(scheduleEvery)
	defer t.Stop()
	for {
		s.schedulesOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) schedulesOnce(ctx context.Context) {
	list, err := s.Store.Schedules(ctx, "")
	if err != nil {
		s.Log.Error("schedules: list", "err", err)
		return
	}
	now := s.now()
	for _, x := range list {
		if !x.Enabled || ctx.Err() != nil {
			continue
		}
		cs, err := cron.Parse(x.Cron)
		if err != nil {
			s.Log.Error("schedules: bad expression", "schedule", x.ID, "cron", x.Cron, "err", err)
			continue
		}
		slot := cs.Last(now, scheduleCatchup)
		if slot.IsZero() || !slot.After(x.LastSlot) {
			continue
		}
		if claimed, err := s.Store.ClaimSlot(ctx, x.ID, slot); err != nil {
			s.Log.Error("schedules: claim", "schedule", x.ID, "err", err)
			continue
		} else if !claimed {
			continue
		}
		if !s.startScheduleRun(ctx, x.ID, "schedule", 0) {
			s.Log.Warn("schedule skipped, its last run is not over", "server", x.AppID, "schedule", x.ID, "name", x.Name)
		}
	}
}

// startScheduleRun runs the schedule in the background, unless it is running
// already, and reports whether it started.
func (s *Server) startScheduleRun(ctx context.Context, id int64, trigger string, user int64) bool {
	key := strconv.FormatInt(id, 10)
	if !s.schedulesBusy.take(key) {
		return false
	}
	s.jobs.Go(func() {
		defer s.schedulesBusy.done(key)
		s.runSchedule(ctx, id, trigger, user)
	})
	return true
}

// runSchedule does a schedule's tasks in order and records how it went. A
// task that fails ends the run unless it is set to continue; the run has
// still failed, with the first failure as its reason.
func (s *Server) runSchedule(ctx context.Context, id int64, trigger string, user int64) {
	x, err := s.Store.Schedule(ctx, id)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Log.Error("schedule run: load", "schedule", id, "err", err)
		}
		return
	}
	log := s.Log.With("server", x.AppID, "schedule", x.ID, "name", x.Name, "trigger", trigger)
	if user != 0 {
		log = log.With("user", user)
	}
	record := func(state string, failure *msg.Msg) {
		if err := s.Store.FinishRun(context.WithoutCancel(ctx), x.ID, state, failure, s.now()); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Error("schedule run: record", "err", err)
		}
	}
	if a, err := s.Store.App(ctx, x.AppID); err != nil {
		return
	} else if x.OnlyRunning && trigger == "schedule" && s.gameState(ctx, a) != stateRunning {
		record(store.RunSkipped, nil)
		log.Info("schedule run", "result", store.RunSkipped, "why", "the server is not running")
		return
	}
	if err := s.Store.StartRun(ctx, x.ID, s.now()); err != nil {
		log.Error("schedule run: start", "err", err)
		return
	}
	var failure *msg.Msg
	done := 0
	for i, t := range x.Tasks {
		err := scheduleSleep(ctx, time.Duration(t.Delay)*time.Second)
		if err == nil {
			err = s.runTask(ctx, x, t)
		}
		if err != nil {
			log.Warn("schedule task failed", "task", i+1, "action", t.Action, "err", err)
			if failure == nil {
				failure = new(errTaskFailed.With("n", i+1, "action", t.Action, "detail", taskFailure(err).Text))
			}
			if !t.KeepGoing || ctx.Err() != nil {
				break
			}
		}
		done++
	}
	state := store.RunDone
	if failure != nil {
		state = store.RunFailed
	}
	record(state, failure)
	log.Info("schedule run", "result", state, "tasks_done", done, "tasks", len(x.Tasks))
}

// taskFailure is what the user reads about why a task failed.
func taskFailure(err error) msg.Msg {
	var m *msg.Error
	if errors.As(err, &m) {
		return m.Msg
	}
	return backupFailure(err)
}

func (s *Server) runTask(ctx context.Context, x store.Schedule, t store.ScheduleTask) error {
	a, err := s.Store.App(ctx, x.AppID)
	if err != nil {
		return err
	}
	switch t.Action {
	case store.TaskCommand:
		container, ok := s.runningGameContainer(ctx, a.ID)
		if !ok {
			return errConsoleNotRunning.Err()
		}
		if err := s.Core.WriteStdin(ctx, container, []byte(t.Data+"\n")); err != nil {
			if isNotFound(err) {
				return errConsoleNotRunning.Err()
			}
			return errConsoleWrite.Err()
		}
		s.Log.Info("game console command", "server", a.ID, "schedule", x.ID, "command", truncate(t.Data, 200))
		return nil
	case store.TaskPower:
		g, err := s.Store.GameServer(ctx, a.ID)
		if err != nil {
			return err
		}
		if _, merr := s.power(ctx, a, g, t.Data, 0); merr != nil {
			// What was asked for is already so.
			if merr.Code == errAlreadyRunning.Code {
				return nil
			}
			return merr
		}
		if t.Data == "stop" || t.Data == "kill" {
			return s.waitStopped(ctx, a.ID)
		}
		return nil
	case store.TaskBackup:
		if !s.backupBusy.take(a.ID) {
			return errBackupBusy.Err()
		}
		defer s.backupBusy.done(a.ID)
		_, err := s.makeBackup(ctx, a, store.BackupSchedule)
		return err
	}
	return errBadTask.Err("n", 0)
}

// waitStopped waits for a server that is stopping, so the next task finds it
// stopped.
func (s *Server) waitStopped(ctx context.Context, id string) error {
	deadline := time.Now().Add(gameStopGrace + 30*time.Second)
	for {
		// Read again each time: stopping sets the app's stopped flag.
		a, err := s.Store.App(ctx, id)
		if err != nil {
			return err
		}
		if s.gameState(ctx, a) == stateStopped {
			return nil
		}
		if time.Now().After(deadline) {
			return errStopTimeout.Err()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(schedulePoll):
		}
	}
}
