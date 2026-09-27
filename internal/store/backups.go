package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Why a backup was made.
const (
	BackupScheduled = "scheduled"
	BackupManual    = "manual"
	// BackupRestore is the safety backup taken just before a restore.
	BackupRestore = "restore"
)

// Backup states.
const (
	BackupRunning = "running"
	BackupDone    = "done"
	BackupFailed  = "failed"
)

// BackupPlan says when an app is backed up and for how long its backups
// are kept.
type BackupPlan struct {
	AppID    string
	Enabled  bool
	Minute   int // of the day, in the server's time zone
	KeepDays int
}

// DefaultBackupPlan is what a new database gets: every night at three,
// kept for a week.
func DefaultBackupPlan(app string) BackupPlan {
	return BackupPlan{AppID: app, Enabled: true, Minute: 3 * 60, KeepDays: 7}
}

// BackupPlan returns an app's plan, or ErrNotFound when it has none.
func (s *Store) BackupPlan(ctx context.Context, app string) (BackupPlan, error) {
	p := BackupPlan{AppID: app}
	err := s.db.QueryRowContext(ctx, "SELECT enabled, minute, keep_days FROM backup_plans WHERE app_id = ?", app).
		Scan(&p.Enabled, &p.Minute, &p.KeepDays)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// SetBackupPlan saves a plan. Backups already made are kept for the new
// number of days, counted from when each was made.
func (s *Store) SetBackupPlan(ctx context.Context, p BackupPlan) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO backup_plans (app_id, enabled, minute, keep_days) VALUES (?, ?, ?, ?)
		ON CONFLICT (app_id) DO UPDATE SET enabled = excluded.enabled, minute = excluded.minute, keep_days = excluded.keep_days`,
		p.AppID, p.Enabled, p.Minute, p.KeepDays); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE backups SET keep_until = created_at + ? WHERE app_id = ?", p.KeepDays*86400, p.AppID); err != nil {
		return err
	}
	return tx.Commit()
}

// BackupPlans returns every plan.
func (s *Store) BackupPlans(ctx context.Context) ([]BackupPlan, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT app_id, enabled, minute, keep_days FROM backup_plans ORDER BY app_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupPlan
	for rows.Next() {
		var p BackupPlan
		if err := rows.Scan(&p.AppID, &p.Enabled, &p.Minute, &p.KeepDays); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Backup is one backup, made or tried.
type Backup struct {
	ID     int64
	AppID  string
	Engine string // what the app was when it was made
	Reason string
	State  string
	File   string // the core's name for it, once made
	Bytes  int64
	Error  string

	CreatedAt  time.Time
	FinishedAt time.Time // zero while running
	KeepUntil  time.Time
	RestoredAt time.Time // zero if it was never put back
}

const backupColumns = "id, app_id, engine, reason, state, file, bytes, error, created_at, finished_at, keep_until, restored_at"

func scanBackup(row scanner) (Backup, error) {
	var b Backup
	var created, keep int64
	var finished, restored sql.NullInt64
	err := row.Scan(&b.ID, &b.AppID, &b.Engine, &b.Reason, &b.State, &b.File, &b.Bytes, &b.Error, &created, &finished, &keep, &restored)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	b.CreatedAt, b.KeepUntil = time.Unix(created, 0), time.Unix(keep, 0)
	if finished.Valid {
		b.FinishedAt = time.Unix(finished.Int64, 0)
	}
	if restored.Valid {
		b.RestoredAt = time.Unix(restored.Int64, 0)
	}
	return b, err
}

// StartBackup records a backup that is about to be made and returns its id.
func (s *Store) StartBackup(ctx context.Context, b Backup) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO backups (app_id, engine, reason, state, created_at, keep_until) VALUES (?, ?, ?, ?, ?, ?)",
		b.AppID, b.Engine, b.Reason, BackupRunning, b.CreatedAt.Unix(), b.KeepUntil.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishBackup records how a backup ended: with a file, or with an error.
func (s *Store) FinishBackup(ctx context.Context, id int64, file string, bytes int64, failure string, at time.Time) error {
	state := BackupDone
	if failure != "" {
		state = BackupFailed
	}
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET state = ?, file = ?, bytes = ?, error = ?, finished_at = ? WHERE id = ?",
		state, file, bytes, failure, at.Unix(), id)
	return oneRow(res, err)
}

// SetRestored records that a backup was put back.
func (s *Store) SetRestored(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET restored_at = ? WHERE id = ?", at.Unix(), id)
	return oneRow(res, err)
}

func (s *Store) Backup(ctx context.Context, id int64) (Backup, error) {
	return scanBackup(s.db.QueryRowContext(ctx, "SELECT "+backupColumns+" FROM backups WHERE id = ?", id))
}

// Backups returns an app's backups, newest first. With app empty, it
// returns every backup whose app no longer exists.
func (s *Store) Backups(ctx context.Context, app string) ([]Backup, error) {
	q := "SELECT " + backupColumns + " FROM backups WHERE app_id = ? ORDER BY id DESC"
	args := []any{app}
	if app == "" {
		q = "SELECT " + backupColumns + " FROM backups WHERE app_id NOT IN (SELECT id FROM apps) ORDER BY app_id, id DESC"
		args = nil
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Backup{}
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ExpiredBackups returns the backups whose time is up. The newest backup
// made of an app that still exists is never among them: if backups stop
// working, the last good one stays.
func (s *Store) ExpiredBackups(ctx context.Context, now time.Time) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+backupColumns+` FROM backups b WHERE keep_until < ? AND state != ?
		AND NOT (app_id IN (SELECT id FROM apps) AND state = ?
			AND id = (SELECT max(id) FROM backups WHERE app_id = b.app_id AND state = ?))
		ORDER BY id`, now.Unix(), BackupRunning, BackupDone, BackupDone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM backups WHERE id = ?", id)
	return oneRow(res, err)
}

// FailUnfinishedBackups marks backups that were running when the panel
// stopped as failed. The panel calls it on start.
func (s *Store) FailUnfinishedBackups(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE backups SET state = ?, error = ?, finished_at = ? WHERE state = ?",
		BackupFailed, "the panel restarted during this backup", now.Unix(), BackupRunning)
	return err
}

// RecoverySavedAt returns when the backup recovery file was last saved,
// or the zero time.
func (s *Store) RecoverySavedAt(ctx context.Context) (time.Time, error) {
	var at int64
	err := s.db.QueryRowContext(ctx, "SELECT saved_at FROM backup_key WHERE one = 1").Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return time.Unix(at, 0), err
}

func (s *Store) SetRecoverySaved(ctx context.Context, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO backup_key (one, saved_at) VALUES (1, ?) ON CONFLICT (one) DO UPDATE SET saved_at = excluded.saved_at", at.Unix())
	return err
}
