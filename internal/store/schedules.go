package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

// Kinds of schedule task.
const (
	TaskCommand = "command"
	TaskPower   = "power"
	TaskBackup  = "backup"
)

// How the last run of a schedule went.
const (
	RunRunning = "running"
	RunDone    = "done"
	RunFailed  = "failed"
	RunSkipped = "skipped"
)

// ScheduleTask is one step of a schedule.
type ScheduleTask struct {
	ID int64
	// Action is TaskCommand, TaskPower or TaskBackup. Data is the console
	// command, or the power action; empty for a backup.
	Action string
	Data   string
	// Delay is how many seconds to wait before the task.
	Delay int
	// KeepGoing runs the tasks after this one even if it fails.
	KeepGoing bool
}

// Schedule runs its tasks in order whenever its cron expression fires.
type Schedule struct {
	ID          int64
	AppID       string
	Name        string
	Cron        string
	OnlyRunning bool
	Enabled     bool
	CreatedAt   time.Time
	// LastSlot is the minute the schedule last fired for; slots up to it
	// are done with.
	LastSlot time.Time
	// LastRun is when the last run started, zero if there was none.
	LastRun   time.Time
	LastState string
	LastError *msg.Msg
	Tasks     []ScheduleTask
}

const scheduleColumns = "id, app_id, name, cron, only_running, enabled, created_at, last_slot, last_run, last_state, last_error, last_error_msg"

func scanSchedule(row scanner) (Schedule, error) {
	var x Schedule
	var created, slot, run int64
	var text, js string
	err := row.Scan(&x.ID, &x.AppID, &x.Name, &x.Cron, &x.OnlyRunning, &x.Enabled, &created, &slot, &run, &x.LastState, &text, &js)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.CreatedAt, x.LastSlot, x.LastRun = time.Unix(created, 0), time.Unix(slot, 0), timeOrZero(run)
	x.LastError = readMsg(text, js)
	return x, err
}

// loadTasks fills in the tasks of every schedule in list.
func (s *Store) loadTasks(ctx context.Context, list []Schedule) error {
	if len(list) == 0 {
		return nil
	}
	index := map[int64]int{}
	for i, x := range list {
		index[x.ID] = i
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, schedule_id, action, data, delay, keep_going FROM schedule_tasks ORDER BY schedule_id, position")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t ScheduleTask
		var sid int64
		if err := rows.Scan(&t.ID, &sid, &t.Action, &t.Data, &t.Delay, &t.KeepGoing); err != nil {
			return err
		}
		if i, ok := index[sid]; ok {
			list[i].Tasks = append(list[i].Tasks, t)
		}
	}
	return rows.Err()
}

// Schedules returns a server's schedules, oldest first. An empty app id
// returns every schedule.
func (s *Store) Schedules(ctx context.Context, app string) ([]Schedule, error) {
	q, args := "SELECT "+scheduleColumns+" FROM schedules ORDER BY id", []any(nil)
	if app != "" {
		q, args = "SELECT "+scheduleColumns+" FROM schedules WHERE app_id = ? ORDER BY id", []any{app}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var list []Schedule
	for rows.Next() {
		x, err := scanSchedule(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, x)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, s.loadTasks(ctx, list)
}

// Schedule returns one schedule.
func (s *Store) Schedule(ctx context.Context, id int64) (Schedule, error) {
	x, err := scanSchedule(s.db.QueryRowContext(ctx, "SELECT "+scheduleColumns+" FROM schedules WHERE id = ?", id))
	if err != nil {
		return x, err
	}
	list := []Schedule{x}
	err = s.loadTasks(ctx, list)
	return list[0], err
}

func insertTasks(ctx context.Context, tx *sql.Tx, id int64, tasks []ScheduleTask) error {
	for i, t := range tasks {
		if _, err := tx.ExecContext(ctx, "INSERT INTO schedule_tasks (schedule_id, position, action, data, delay, keep_going) VALUES (?, ?, ?, ?, ?, ?)",
			id, i, t.Action, t.Data, t.Delay, t.KeepGoing); err != nil {
			return err
		}
	}
	return nil
}

// CreateSchedule adds a schedule and returns its id. Its slots start now:
// LastSlot is what the caller sets, normally the current minute.
func (s *Store) CreateSchedule(ctx context.Context, x Schedule) (int64, error) {
	var id int64
	err := s.tx(ctx, func(tx *sql.Tx) (err error) {
		if id, err = nextID(ctx, tx, "schedule_seq", "schedules"); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO schedules (id, app_id, name, cron, only_running, enabled, created_at, last_slot) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			id, x.AppID, x.Name, x.Cron, x.OnlyRunning, x.Enabled, x.CreatedAt.Unix(), x.LastSlot.Unix()); err != nil {
			return err
		}
		return insertTasks(ctx, tx, id, x.Tasks)
	})
	return id, err
}

// UpdateSchedule replaces a schedule's settings and tasks. LastSlot moves
// too, so a change never fires for a minute that has passed.
func (s *Store) UpdateSchedule(ctx context.Context, x Schedule) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE schedules SET name = ?, cron = ?, only_running = ?, enabled = ?, last_slot = ? WHERE id = ? AND app_id = ?",
			x.Name, x.Cron, x.OnlyRunning, x.Enabled, x.LastSlot.Unix(), x.ID, x.AppID)
		if err := oneRow(res, err); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM schedule_tasks WHERE schedule_id = ?", x.ID); err != nil {
			return err
		}
		return insertTasks(ctx, tx, x.ID, x.Tasks)
	})
}

func (s *Store) DeleteSchedule(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM schedules WHERE id = ?", id)
	return oneRow(res, err)
}

// ClaimSlot records that the schedule fires for slot, and reports whether
// it did so first: false means the slot was already handled.
func (s *Store) ClaimSlot(ctx context.Context, id int64, slot time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE schedules SET last_slot = ? WHERE id = ? AND last_slot < ?", slot.Unix(), id, slot.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StartRun records that a run has begun.
func (s *Store) StartRun(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE schedules SET last_run = ?, last_state = ?, last_error = '', last_error_msg = '' WHERE id = ?", at.Unix(), RunRunning, id)
	return oneRow(res, err)
}

// FinishRun records how a run ended. A skipped run also sets when it was.
func (s *Store) FinishRun(ctx context.Context, id int64, state string, failure *msg.Msg, at time.Time) error {
	text, js := msgColumns(failure)
	res, err := s.db.ExecContext(ctx, "UPDATE schedules SET last_state = ?, last_error = ?, last_error_msg = ?, last_run = CASE WHEN ? = ? THEN ? ELSE last_run END WHERE id = ?",
		state, text, js, state, RunSkipped, at.Unix(), id)
	return oneRow(res, err)
}

var errRunInterrupted = msg.Define(0, "schedule.interrupted", "The panel restarted during this run.")

// FailUnfinishedRuns marks runs that were going when the panel stopped as
// failed. The panel calls it on start.
func (s *Store) FailUnfinishedRuns(ctx context.Context) error {
	text, js := msgColumns(new(errRunInterrupted.With()))
	_, err := s.db.ExecContext(ctx, "UPDATE schedules SET last_state = ?, last_error = ?, last_error_msg = ? WHERE last_state = ?", RunFailed, text, js, RunRunning)
	return err
}
