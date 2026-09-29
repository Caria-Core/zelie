package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

// Why a backup was made.
const (
	BackupScheduled = "scheduled"
	BackupManual    = "manual"
	// BackupRestore is the safety backup taken just before a restore.
	BackupRestore = "restore"
	// BackupFound was made elsewhere and found in off-site storage.
	BackupFound = "found"
	// BackupUploaded is a dump from another server that someone uploaded.
	BackupUploaded = "uploaded"
	// BackupUpdate is taken just before a database moves to a new image.
	BackupUpdate = "update"
)

// Backup states.
const (
	BackupRunning = "running"
	BackupDone    = "done"
	BackupFailed  = "failed"
)

// Where a backup's off-site copy is. A backup that is not sent has none.
const (
	OffsitePending = "pending"
	OffsiteDone    = "done"
	OffsiteFailed  = "failed"
)

// BackupPlan says when an app is backed up and for how long its backups
// are kept.
type BackupPlan struct {
	AppID    string
	Enabled  bool
	Minute   int // of the day, in the server's time zone
	KeepDays int
	// Stop stops an app for its volume backup instead of copying the
	// files while it runs.
	Stop bool
	// Offsite sends each backup to the off-site storage, when there is
	// one, where it is kept for OffsiteDays.
	Offsite     bool
	OffsiteDays int
}

// DefaultBackupPlan is what a new database gets: every night at three,
// kept for a week.
func DefaultBackupPlan(app string) BackupPlan {
	return BackupPlan{AppID: app, Enabled: true, Minute: 3 * 60, KeepDays: 7, Offsite: true, OffsiteDays: 30}
}

// AppBackupPlan is what an app has until its plan is changed: the same
// hours, but off, since volumes such as game worlds can be large.
func AppBackupPlan(app string) BackupPlan {
	return BackupPlan{AppID: app, Minute: 3 * 60, KeepDays: 7, Offsite: true, OffsiteDays: 30}
}

// BackupPlan returns an app's plan, or ErrNotFound when it has none.
func (s *Store) BackupPlan(ctx context.Context, app string) (BackupPlan, error) {
	p := BackupPlan{AppID: app}
	err := s.db.QueryRowContext(ctx, "SELECT "+planColumns+" FROM backup_plans WHERE app_id = ?", app).
		Scan(&p.AppID, &p.Enabled, &p.Minute, &p.KeepDays, &p.Stop, &p.Offsite, &p.OffsiteDays)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

const planColumns = "app_id, enabled, minute, keep_days, stop, offsite, offsite_days"

// SetBackupPlan saves a plan. Backups already made are kept for the new
// numbers of days, counted from when each was made.
func (s *Store) SetBackupPlan(ctx context.Context, p BackupPlan) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO backup_plans (`+planColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (app_id) DO UPDATE SET enabled = excluded.enabled, minute = excluded.minute, keep_days = excluded.keep_days,
			stop = excluded.stop, offsite = excluded.offsite, offsite_days = excluded.offsite_days`,
		p.AppID, p.Enabled, p.Minute, p.KeepDays, p.Stop, p.Offsite, p.OffsiteDays); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE backups SET keep_until = created_at + ? WHERE app_id = ?", p.KeepDays*86400, p.AppID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE backups SET offsite_until = created_at + ? WHERE app_id = ? AND offsite != ''", p.OffsiteDays*86400, p.AppID); err != nil {
		return err
	}
	return tx.Commit()
}

// BackupPlans returns every plan.
func (s *Store) BackupPlans(ctx context.Context) ([]BackupPlan, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+planColumns+" FROM backup_plans ORDER BY app_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackupPlan
	for rows.Next() {
		var p BackupPlan
		if err := rows.Scan(&p.AppID, &p.Enabled, &p.Minute, &p.KeepDays, &p.Stop, &p.Offsite, &p.OffsiteDays); err != nil {
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
	Engine string // what the app was when it was made; empty for volumes
	Reason string
	State  string
	File   string // the core's name for it, once made
	Bytes  int64
	Error  *msg.Msg // why it failed
	// For a backup of volumes: the folders in it, how much its files take
	// and how many changed while they were copied.
	Volumes []string
	Size    int64
	Changed int
	// For an uploaded dump: what was changed so it loads here, by kind.
	Adapted map[string]int

	CreatedAt  time.Time
	FinishedAt time.Time // zero while running
	KeepUntil  time.Time
	RestoredAt time.Time // zero if it was never put back

	// The off-site copy: its state (empty when there is none), why it
	// failed, how often it was tried, when it was sent or is tried
	// again, and until when it is kept.
	Offsite      string
	OffsiteError *msg.Msg
	OffsiteTries int
	OffsiteAt    time.Time
	OffsiteUntil time.Time
	// Local is false once only the off-site copy is left.
	Local bool
}

const backupColumns = `id, app_id, engine, reason, state, file, bytes, error, error_msg, created_at, finished_at, keep_until, restored_at, volumes, size, changed,
	offsite, offsite_error, offsite_error_msg, offsite_tries, offsite_at, offsite_until, local, adapted`

func scanBackup(row scanner) (Backup, error) {
	var b Backup
	var created, keep, offsiteAt, offsiteUntil int64
	var finished, restored sql.NullInt64
	var volumes, text, js, otext, ojs, adapted string
	err := row.Scan(&b.ID, &b.AppID, &b.Engine, &b.Reason, &b.State, &b.File, &b.Bytes, &text, &js, &created, &finished, &keep, &restored,
		&volumes, &b.Size, &b.Changed, &b.Offsite, &otext, &ojs, &b.OffsiteTries, &offsiteAt, &offsiteUntil, &b.Local, &adapted)
	b.Error = readMsg(text, js)
	b.OffsiteError = readMsg(otext, ojs)
	b.OffsiteAt, b.OffsiteUntil = timeOrZero(offsiteAt), timeOrZero(offsiteUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return b, ErrNotFound
	}
	if volumes != "" {
		b.Volumes = strings.Split(volumes, "\n")
	}
	if adapted != "" {
		json.Unmarshal([]byte(adapted), &b.Adapted)
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

func timeOrZero(unix int64) time.Time {
	if unix == 0 {
		return time.Time{}
	}
	return time.Unix(unix, 0)
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

// FinishBackup records how a backup ended: with a file, or with why not.
func (s *Store) FinishBackup(ctx context.Context, id int64, file string, bytes int64, failure *msg.Msg, at time.Time) error {
	state := BackupDone
	if failure != nil {
		state = BackupFailed
	}
	text, js := msgColumns(failure)
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET state = ?, file = ?, bytes = ?, error = ?, error_msg = ?, finished_at = ? WHERE id = ?",
		state, file, bytes, text, js, at.Unix(), id)
	return oneRow(res, err)
}

// SetBackupContents records what a volume backup holds.
func (s *Store) SetBackupContents(ctx context.Context, id int64, volumes []string, size int64, changed int) error {
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET volumes = ?, size = ?, changed = ? WHERE id = ?",
		strings.Join(volumes, "\n"), size, changed, id)
	return oneRow(res, err)
}

// SetAdapted records what an uploaded dump needed changed to load here.
func (s *Store) SetAdapted(ctx context.Context, id int64, adapted map[string]int) error {
	js := ""
	if len(adapted) > 0 {
		b, _ := json.Marshal(adapted)
		js = string(b)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET adapted = ? WHERE id = ?", js, id)
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

// ExpiredBackups returns the backups whose file here is due to go. The
// newest backup made of an app that still exists is never among them: if
// backups stop working, the last good one stays.
func (s *Store) ExpiredBackups(ctx context.Context, now time.Time) ([]Backup, error) {
	return s.queryBackups(ctx, `SELECT `+backupColumns+` FROM backups b WHERE local = 1 AND keep_until < ? AND state != ?
		AND NOT (app_id IN (SELECT id FROM apps) AND state = ?
			AND id = (SELECT max(id) FROM backups WHERE app_id = b.app_id AND state = ?))
		ORDER BY id`, now.Unix(), BackupRunning, BackupDone, BackupDone)
}

// ExpiredOffsite returns the backups left only off-site whose time is up
// there too.
func (s *Store) ExpiredOffsite(ctx context.Context, now time.Time) ([]Backup, error) {
	return s.queryBackups(ctx, "SELECT "+backupColumns+" FROM backups WHERE local = 0 AND offsite_until < ? ORDER BY id", now.Unix())
}

// OffsiteDue returns the backups to send now, oldest first.
func (s *Store) OffsiteDue(ctx context.Context, now time.Time) ([]Backup, error) {
	return s.queryBackups(ctx, "SELECT "+backupColumns+" FROM backups WHERE state = ? AND local = 1 AND offsite IN (?, ?) AND offsite_at <= ? ORDER BY id",
		BackupDone, OffsitePending, OffsiteFailed, now.Unix())
}

func (s *Store) queryBackups(ctx context.Context, q string, args ...any) ([]Backup, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
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

// AddFoundBackup records a backup found in off-site storage, kept there
// until offsite_until and not here.
func (s *Store) AddFoundBackup(ctx context.Context, b Backup) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO backups (app_id, engine, reason, state, file, bytes, created_at, finished_at, keep_until,
		volumes, size, offsite, offsite_at, offsite_until, local) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`,
		b.AppID, b.Engine, BackupFound, BackupDone, b.File, b.Bytes, b.CreatedAt.Unix(), b.CreatedAt.Unix(), b.CreatedAt.Unix(),
		strings.Join(b.Volumes, "\n"), b.Size, OffsiteDone, b.OffsiteAt.Unix(), b.OffsiteUntil.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// BackupFiles returns every backup file on record, as app/file.
func (s *Store) BackupFiles(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT app_id, file FROM backups WHERE file != ''")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var app, file string
		if err := rows.Scan(&app, &file); err != nil {
			return nil, err
		}
		out[app+"/"+file] = true
	}
	return out, rows.Err()
}

// SendOffsite marks a backup to be sent now, and kept off-site until.
func (s *Store) SendOffsite(ctx context.Context, id int64, until time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE backups SET offsite = ?, offsite_error = '', offsite_error_msg = '', offsite_tries = 0,
		offsite_at = 0, offsite_until = ? WHERE id = ?`, OffsitePending, until.Unix(), id)
	return oneRow(res, err)
}

// OffsiteSent records that a backup's copy is off-site.
func (s *Store) OffsiteSent(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET offsite = ?, offsite_error = '', offsite_error_msg = '', offsite_at = ? WHERE id = ?",
		OffsiteDone, at.Unix(), id)
	return oneRow(res, err)
}

// OffsiteFailedAt records a failed send, to be tried again at next.
func (s *Store) OffsiteFailedAt(ctx context.Context, id int64, failure msg.Msg, tries int, next time.Time) error {
	text, js := msgColumns(&failure)
	res, err := s.db.ExecContext(ctx, "UPDATE backups SET offsite = ?, offsite_error = ?, offsite_error_msg = ?, offsite_tries = ?, offsite_at = ? WHERE id = ?",
		OffsiteFailed, text, js, tries, next.Unix(), id)
	return oneRow(res, err)
}

// DropOffsiteQueue forgets the backups still waiting to be sent, when
// there is no longer anywhere to send them.
func (s *Store) DropOffsiteQueue(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backups SET offsite = '', offsite_error = '', offsite_error_msg = '', offsite_tries = 0,
		offsite_at = 0, offsite_until = 0 WHERE offsite IN (?, ?)`, OffsitePending, OffsiteFailed)
	return err
}

// SetLocal records whether a backup's file is here. One brought back from
// off-site is kept here until keep.
func (s *Store) SetLocal(ctx context.Context, id int64, local bool, keep time.Time) error {
	q, args := "UPDATE backups SET local = ? WHERE id = ?", []any{local, id}
	if local {
		q, args = "UPDATE backups SET local = 1, keep_until = ? WHERE id = ?", []any{keep.Unix(), id}
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	return oneRow(res, err)
}

func (s *Store) DeleteBackup(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM backups WHERE id = ?", id)
	return oneRow(res, err)
}

var errPanelRestarted = msg.Define(0, "backup.panel_restarted", "The panel restarted during this backup.")

// FailUnfinishedBackups marks backups that were running when the panel
// stopped as failed. The panel calls it on start.
func (s *Store) FailUnfinishedBackups(ctx context.Context, now time.Time) error {
	text, js := msgColumns(new(errPanelRestarted.With()))
	_, err := s.db.ExecContext(ctx, "UPDATE backups SET state = ?, error = ?, error_msg = ?, finished_at = ? WHERE state = ?",
		BackupFailed, text, js, now.Unix(), BackupRunning)
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
