package panel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Tests shorten these.
var (
	backupEvery = time.Minute
	// A scheduled backup that failed is tried again after backupRetry, up
	// to backupTries times before its next day.
	backupRetry = 30 * time.Minute
)

const backupTries = 3

// keyset is a set of app ids that can be taken one at a time.
type keyset struct {
	mu  sync.Mutex
	set map[string]bool
}

func (k *keyset) take(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.set[id] {
		return false
	}
	if k.set == nil {
		k.set = map[string]bool{}
	}
	k.set[id] = true
	return true
}

func (k *keyset) done(id string) {
	k.mu.Lock()
	delete(k.set, id)
	k.mu.Unlock()
}

func (k *keyset) has(id string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.set[id]
}

// pauses keeps the supervisor away from apps a restore has stopped. Two
// restores can hold the same app, so it counts.
type pauses struct {
	mu sync.Mutex
	n  map[string]int
}

func (p *pauses) hold(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == nil {
		p.n = map[string]int{}
	}
	p.n[id]++
}

func (p *pauses) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n[id]--; p.n[id] <= 0 {
		delete(p.n, id)
	}
}

func (p *pauses) paused(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n[id] > 0
}

var (
	errBackupBusy    = msg.Define(http.StatusConflict, "backup.busy", "A backup or restore of this app is already running.")
	errDBNotStarted  = msg.Define(http.StatusConflict, "backup.db_not_started", "The database has not started yet.")
	errDBNotRunning  = msg.Define(http.StatusConflict, "backup.db_not_running", "The database is not running.")
	errCoreBackup    = msg.Define(0, "backup.core_failed", "The Zelie core could not make it. See the server log.")
	errBadMinute     = msg.Define(http.StatusBadRequest, "backup.bad_time", "The time must be within the day.")
	errBadKeep       = msg.Define(http.StatusBadRequest, "backup.bad_keep", "Backups are kept for 1 to 365 days.")
	errNoBackup      = msg.Define(http.StatusNotFound, "backup.not_found", "There is no such backup.")
	errBackupNotMade = msg.Define(http.StatusConflict, "backup.not_made", "This backup was not made.")
	errAppDeleted    = msg.Define(http.StatusConflict, "restore.app_deleted", "There is no app named {app} here. Create one with volumes at the same paths, then restore this backup into it.")
	errDBDeleted     = msg.Define(http.StatusConflict, "restore.db_deleted", "There is no {engine} database named {app} here. Create one, then restore this backup into it.")
	errAppIntoDB     = msg.Define(http.StatusConflict, "restore.app_into_db", "This is a backup of an app's volumes, and {app} is a database.")
	errDBIntoApp     = msg.Define(http.StatusConflict, "restore.db_into_app", "This is a backup of a {engine} database, and {app} is an app.")
	errWrongEngine   = msg.Define(http.StatusConflict, "restore.wrong_engine", "This is a backup of a {engine} database, and {app} is {other}.")
	errStartDBFirst  = msg.Define(http.StatusConflict, "restore.start_db_first", "Start the database first: what it holds now is backed up before the restore.")
)

// liveContainer returns the database's running container.
func (s *Server) liveContainer(ctx context.Context, a store.App) (string, error) {
	live, err := s.Store.LiveDeployment(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		return "", errDBNotStarted.Err()
	}
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s-%d", a.ID, live.ID)
	st, err := s.containerStatus(ctx, id)
	if err != nil || st.State != "running" {
		return "", errDBNotRunning.Err()
	}
	return id, nil
}

// planFor returns an app's backup plan, or what it has by default.
func (s *Server) planFor(ctx context.Context, a store.App) (store.BackupPlan, error) {
	plan, err := s.Store.BackupPlan(ctx, a.ID)
	if errors.Is(err, store.ErrNotFound) {
		if a.IsDatabase() {
			return store.DefaultBackupPlan(a.ID), nil
		}
		return store.AppBackupPlan(a.ID), nil
	}
	return plan, err
}

// makeBackup backs an app up now: a database's dump, or another app's
// volumes. The caller holds the app in s.backupBusy. A failure is recorded
// as well as returned: nobody may think there is a backup when there is
// not.
func (s *Server) makeBackup(ctx context.Context, a store.App, reason string) (store.Backup, error) {
	plan, err := s.planFor(ctx, a)
	if err != nil {
		return store.Backup{}, err
	}
	now := s.now()
	b := store.Backup{AppID: a.ID, Engine: a.Engine, Reason: reason, State: store.BackupRunning, CreatedAt: now,
		KeepUntil: now.Add(time.Duration(plan.KeepDays) * 24 * time.Hour)}
	if b.ID, err = s.Store.StartBackup(ctx, b); err != nil {
		return b, err
	}
	info, err := s.takeBackup(ctx, a, b.ID, plan, reason)
	var failure *msg.Msg
	if err != nil {
		failure = new(backupFailure(err))
		s.Log.Warn("backup failed", "app", a.ID, "reason", reason, "err", err)
	} else {
		s.Log.Info("backup made", "app", a.ID, "reason", reason, "bytes", info.Bytes)
	}
	if ferr := s.Store.FinishBackup(context.WithoutCancel(ctx), b.ID, info.Name, info.Bytes, failure, s.now()); ferr != nil {
		return b, ferr
	}
	if err == nil {
		s.queueOffsite(ctx, b, plan)
	}
	b, _ = s.Store.Backup(ctx, b.ID)
	return b, err
}

// backupFailure is what the user reads about a failed backup.
func backupFailure(err error) msg.Msg {
	var ce *core.Error
	if errors.As(err, &ce) {
		if ce.Status < 500 {
			return ce.Msg()
		}
		return errCoreBackup.With()
	}
	return msg.Wrap(err)
}

// runBackups makes the scheduled backups and deletes the ones whose time
// is up.
func (s *Server) runBackups(ctx context.Context) {
	t := time.NewTicker(backupEvery)
	defer t.Stop()
	for {
		s.backupsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) backupsOnce(ctx context.Context) {
	plans, err := s.Store.BackupPlans(ctx)
	if err != nil {
		s.Log.Error("backups: list plans", "err", err)
		return
	}
	for _, p := range plans {
		if !p.Enabled || ctx.Err() != nil {
			continue
		}
		a, err := s.Store.App(ctx, p.AppID)
		// A stopped app does not change; its last backup is current.
		if err != nil || a.Stopped {
			continue
		}
		due, err := s.backupDue(ctx, a, p)
		if err != nil {
			s.Log.Error("backups: check", "app", a.ID, "err", err)
			continue
		}
		if !due || !s.backupBusy.take(a.ID) {
			continue
		}
		s.makeBackup(ctx, a, store.BackupScheduled)
		s.backupBusy.done(a.ID)
	}
	s.pruneBackups(ctx)
}

// lastSlot is the most recent time a backup was due at minute of the day.
func lastSlot(now time.Time, minute int) time.Time {
	y, m, d := now.Date()
	slot := time.Date(y, m, d, minute/60, minute%60, 0, 0, now.Location())
	if slot.After(now) {
		slot = slot.AddDate(0, 0, -1)
	}
	return slot
}

// backupDue reports whether the database's scheduled backup is due: once
// after each day's slot, a few times if it fails. A database made after
// the slot waits for the next one.
func (s *Server) backupDue(ctx context.Context, a store.App, p store.BackupPlan) (bool, error) {
	now := s.now()
	slot := lastSlot(now, p.Minute)
	if a.CreatedAt.After(slot) {
		return false, nil
	}
	list, err := s.Store.Backups(ctx, a.ID)
	if err != nil {
		return false, err
	}
	var tries []store.Backup
	for _, b := range list {
		if b.Reason == store.BackupScheduled && !b.CreatedAt.Before(slot) {
			tries = append(tries, b)
		}
	}
	if len(tries) == 0 {
		return true, nil
	}
	last := tries[0]
	return last.State == store.BackupFailed && len(tries) < backupTries && now.Sub(last.CreatedAt) >= backupRetry, nil
}

type backupJSON struct {
	ID         int64      `json:"id"`
	App        string     `json:"app"`
	Engine     string     `json:"engine"`
	Reason     string     `json:"reason"`
	State      string     `json:"state"`
	Bytes      int64      `json:"bytes"`
	Error      *msg.Msg   `json:"error,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	KeepUntil  time.Time  `json:"keep_until"`
	RestoredAt *time.Time `json:"restored_at,omitempty"`
	Volumes    []string   `json:"volumes,omitempty"`
	Size       int64      `json:"size,omitempty"`
	Changed    int        `json:"changed,omitempty"`
	// Adapted counts what an uploaded dump needed changed to load here.
	Adapted map[string]int `json:"adapted,omitempty"`
	// Offsite is where its off-site copy is: pending, sending, done or
	// failed, or empty for none. Local is false when only that copy is
	// left.
	Offsite      string     `json:"offsite,omitempty"`
	OffsiteError *msg.Msg   `json:"offsite_error,omitempty"`
	OffsiteAt    *time.Time `json:"offsite_at,omitempty"`
	OffsiteUntil *time.Time `json:"offsite_until,omitempty"`
	Local        bool       `json:"local"`
}

func (s *Server) backupOut(b store.Backup) backupJSON {
	out := backupJSON{ID: b.ID, App: b.AppID, Engine: b.Engine, Reason: b.Reason, State: b.State, Bytes: b.Bytes, Error: b.Error,
		CreatedAt: b.CreatedAt, KeepUntil: b.KeepUntil, Volumes: b.Volumes, Size: b.Size, Changed: b.Changed, Adapted: b.Adapted,
		Offsite: b.Offsite, Local: b.Local}
	switch {
	case b.Offsite == store.OffsiteFailed:
		out.OffsiteError = b.OffsiteError
		if b.OffsiteAt.Before(never) {
			out.OffsiteAt = &b.OffsiteAt // when it is tried again
		}
	case b.Offsite != "" && s.uploading.has(strconv.FormatInt(b.ID, 10)):
		out.Offsite = "sending"
	case b.Offsite == store.OffsiteDone:
		out.OffsiteAt = &b.OffsiteAt
	}
	if b.Offsite != "" {
		out.OffsiteUntil = &b.OffsiteUntil
	}
	if !b.FinishedAt.IsZero() {
		out.FinishedAt = &b.FinishedAt
	}
	if !b.RestoredAt.IsZero() {
		out.RestoredAt = &b.RestoredAt
	}
	return out
}

type backupPlanJSON struct {
	Enabled  bool `json:"enabled"`
	Minute   int  `json:"minute"`
	KeepDays int  `json:"keep_days"`
	Stop     bool `json:"stop"`
	// Left out, they stay as they are.
	Offsite     *bool `json:"offsite,omitempty"`
	OffsiteDays *int  `json:"offsite_days,omitempty"`
}

func planOut(p store.BackupPlan) backupPlanJSON {
	return backupPlanJSON{p.Enabled, p.Minute, p.KeepDays, p.Stop, &p.Offsite, &p.OffsiteDays}
}

type backupsJSON struct {
	Plan    backupPlanJSON `json:"plan"`
	Backups []backupJSON   `json:"backups"`
	// Volumes are where the app sees its volumes now, for apps that are
	// not databases.
	Volumes []string `json:"volumes"`
	// Running is set while a backup or restore of the database runs.
	Running bool `json:"running"`
	// OffsiteSet says there is off-site storage to send backups to.
	OffsiteSet bool `json:"offsite_set"`
	// RecoverySavedAt is when the recovery file was last saved; without
	// it the backups cannot be opened if the server is lost.
	RecoverySavedAt *time.Time `json:"recovery_saved_at"`
	TimeZone        string     `json:"time_zone"`
	// Restore is how the last restore since the panel started went.
	Restore *restoreJSON `json:"restore,omitempty"`
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	plan, err := s.planFor(ctx, a)
	if err != nil {
		s.fail(w, "load backup plan", err)
		return
	}
	list, err := s.Store.Backups(ctx, a.ID)
	if err != nil {
		s.fail(w, "list backups", err)
		return
	}
	out := backupsJSON{Plan: planOut(plan), Backups: []backupJSON{}, Volumes: []string{}, Running: s.backupBusy.has(a.ID),
		OffsiteSet: s.offsiteSet(ctx), TimeZone: zoneLabel(s.now())}
	if !a.IsDatabase() {
		vols, err := s.Store.Volumes(ctx, a.ID)
		if err != nil {
			s.fail(w, "list volumes", err)
			return
		}
		for _, v := range vols {
			out.Volumes = append(out.Volumes, v.Path)
		}
	}
	if last, ok := s.restores.get(a.ID); ok {
		out.Restore = &last
	}
	for _, b := range list {
		out.Backups = append(out.Backups, s.backupOut(b))
	}
	if at, err := s.Store.RecoverySavedAt(ctx); err == nil && !at.IsZero() {
		out.RecoverySavedAt = &at
	}
	writeJSON(w, http.StatusOK, out)
}

// listDeletedBackups lists the backups of apps and databases that were
// deleted. They stay until their time is up, in case one went by mistake.
func (s *Server) listDeletedBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.Backups(r.Context(), "")
	if err != nil {
		s.fail(w, "list backups", err)
		return
	}
	out := []backupJSON{}
	for _, b := range list {
		if b.State == store.BackupDone {
			out = append(out, s.backupOut(b))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) backUpNow(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok || !s.hasVolumes(w, r, a) {
		return
	}
	if !s.backupBusy.take(a.ID) {
		writeError(w, errBackupBusy.Err())
		return
	}
	// The page follows along; the backup is on its list from the start.
	ctx := context.WithoutCancel(r.Context())
	s.jobs.Go(func() {
		defer s.backupBusy.done(a.ID)
		s.makeBackup(ctx, a, store.BackupManual)
	})
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) setBackupPlan(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	var req backupPlanJSON
	if !decode(w, r, &req) {
		return
	}
	if req.Minute < 0 || req.Minute >= 24*60 {
		writeError(w, errBadMinute.Err())
		return
	}
	if req.KeepDays < 1 || req.KeepDays > 365 {
		writeError(w, errBadKeep.Err())
		return
	}
	old, err := s.planFor(r.Context(), a)
	if err != nil {
		s.fail(w, "load backup plan", err)
		return
	}
	if req.Offsite == nil {
		req.Offsite = &old.Offsite
	}
	if req.OffsiteDays == nil {
		req.OffsiteDays = &old.OffsiteDays
	}
	// The off-site copy is the one that outlives this server, so it is
	// never deleted before the backup here.
	if *req.OffsiteDays < req.KeepDays || *req.OffsiteDays > 3650 {
		writeError(w, errOffsiteKeep.Err())
		return
	}
	if a.IsDatabase() {
		req.Stop = false // a dump is consistent while the database runs
	} else if req.Enabled && !s.hasVolumes(w, r, a) {
		return
	}
	p := store.BackupPlan{AppID: a.ID, Enabled: req.Enabled, Minute: req.Minute, KeepDays: req.KeepDays, Stop: req.Stop,
		Offsite: *req.Offsite, OffsiteDays: *req.OffsiteDays}
	if err := s.Store.SetBackupPlan(r.Context(), p); err != nil {
		s.fail(w, "save backup plan", err)
		return
	}
	s.Log.Info("backup plan changed", "app", a.ID, "enabled", p.Enabled, "minute", p.Minute, "keep_days", p.KeepDays, "stop", p.Stop,
		"offsite", p.Offsite, "offsite_days", p.OffsiteDays, "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusOK, planOut(p))
}

func (s *Server) backupFrom(w http.ResponseWriter, r *http.Request) (store.Backup, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, errNoBackup.Err())
		return store.Backup{}, false
	}
	b, err := s.Store.Backup(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, errNoBackup.Err())
		return b, false
	}
	if err != nil {
		s.fail(w, "load backup", err)
		return b, false
	}
	if b.State != store.BackupDone || b.File == "" {
		writeError(w, errBackupNotMade.Err())
		return b, false
	}
	return b, true
}

// downloadBackup sends the backup as it is kept: encrypted. The recovery
// file opens it.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.backupFrom(w, r)
	if !ok {
		return
	}
	// What is inside, from the core's name: sql, rdb or tar.
	ext := strings.SplitN(b.File, ".", 2)[1]
	name := fmt.Sprintf("%s-%s.%s", b.AppID, b.CreatedAt.Format("2006-01-02-1504"), ext)
	if err := s.ensureLocal(r.Context(), &b); err != nil {
		s.coreFailed(w, "fetch backup", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := s.Core.DownloadBackup(r.Context(), b.AppID, b.File, w); err != nil {
		s.Log.Error("download backup", "app", b.AppID, "backup", b.File, "err", err)
	}
}

func (s *Server) deleteBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.backupFrom(w, r)
	if !ok {
		return
	}
	if !s.backupBusy.take(b.AppID) {
		writeError(w, errBackupBusy.Err())
		return
	}
	defer s.backupBusy.done(b.AppID)
	if b.Local {
		if err := s.Core.RemoveBackup(r.Context(), b.AppID, b.File); err != nil && !isNotFound(err) {
			s.coreFailed(w, "remove backup", err)
			return
		}
	}
	if b.Offsite == store.OffsiteDone {
		if err := s.removeOffsite(r.Context(), b); err != nil && !isNotFound(err) {
			s.coreFailed(w, "remove off-site backup", err)
			return
		}
	}
	if err := s.Store.DeleteBackup(r.Context(), b.ID); err != nil {
		s.fail(w, "delete backup", err)
		return
	}
	s.Log.Info("backup deleted", "app", b.AppID, "backup", b.File, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// recoveryFile hands out the key that opens every backup, as a file to keep
// off the server.
func (s *Server) recoveryFile(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	var buf strings.Builder
	if err := s.Core.RecoveryKey(r.Context(), host, &buf); err != nil {
		s.coreFailed(w, "recovery key", err)
		return
	}
	if err := s.Store.SetRecoverySaved(r.Context(), s.now()); err != nil {
		s.fail(w, "record recovery key", err)
		return
	}
	s.Log.Info("backup recovery file saved", "user", loginFrom(r.Context()).account.ID)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="zelie-recovery.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(buf.String()))
}

// restoreJSON is how the last restore of a database went. Restores run in
// the background, since a large database takes longer than a browser or a
// proxy in front of the panel waits for an answer.
type restoreJSON struct {
	Backup int64    `json:"backup"`
	State  string   `json:"state"` // running, done or failed
	Error  *msg.Msg `json:"error,omitempty"`
	// SafetyFailed is set when the backup of what was there failed, so
	// nothing was changed.
	SafetyFailed bool `json:"safety_failed,omitempty"`
	// Safety is when the backup of what was there before was made.
	Safety *time.Time `json:"safety,omitempty"`
	// Restarted are the linked apps that were stopped and started again.
	Restarted []string  `json:"restarted"`
	At        time.Time `json:"at"`
}

// restores keeps the last restore of each database, for its page.
type restores struct {
	mu   sync.Mutex
	last map[string]restoreJSON
}

func (r *restores) set(app string, v restoreJSON) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.last == nil {
		r.last = map[string]restoreJSON{}
	}
	r.last[app] = v
}

func (r *restores) get(app string) (restoreJSON, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.last[app]
	return v, ok
}

// zoneLabel names the server's time zone for people: its name when it has
// one, and its offset from UTC.
func zoneLabel(now time.Time) string {
	_, off := now.Zone()
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	utc := fmt.Sprintf("UTC%s%02d:%02d", sign, off/3600, off%3600/60)
	if name := now.Location().String(); name != "Local" && name != "UTC" {
		return name + ", " + utc
	}
	return utc
}

// restoreBackup puts a backup back into its database. What is there now is
// backed up first; without that backup nothing is changed.
func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	b, ok := s.backupFrom(w, r)
	if !ok {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	a, err := s.Store.App(ctx, b.AppID)
	if errors.Is(err, store.ErrNotFound) {
		if b.Engine == "" {
			writeError(w, errAppDeleted.Err("app", b.AppID))
			return
		}
		writeError(w, errDBDeleted.Err("app", b.AppID, "engine", engineLabel(b.Engine)))
		return
	}
	if err != nil {
		s.fail(w, "load app", err)
		return
	}
	switch {
	case a.Engine == b.Engine:
	case b.Engine == "":
		writeError(w, errAppIntoDB.Err("app", a.ID))
		return
	case !a.IsDatabase():
		writeError(w, errDBIntoApp.Err("engine", engineLabel(b.Engine), "app", a.ID))
		return
	default:
		writeError(w, errWrongEngine.Err("engine", engineLabel(b.Engine), "app", a.ID, "other", engineLabel(a.Engine)))
		return
	}
	if !a.IsDatabase() && !s.hasVolumes(w, r, a) {
		return
	}
	if a.IsDatabase() && a.Stopped {
		writeError(w, errStartDBFirst.Err())
		return
	}
	if !s.backupBusy.take(a.ID) {
		writeError(w, errBackupBusy.Err())
		return
	}
	user := loginFrom(r.Context()).account.ID
	s.restores.set(a.ID, restoreJSON{Backup: b.ID, State: "running", Restarted: []string{}, At: s.now()})
	s.jobs.Go(func() {
		defer s.backupBusy.done(a.ID)
		out := s.runRestore(context.WithoutCancel(ctx), a, b, user)
		s.restores.set(a.ID, out)
	})
	w.WriteHeader(http.StatusAccepted)
}

// runRestore backs up what is there, then puts the backup back.
func (s *Server) runRestore(ctx context.Context, a store.App, b store.Backup, user int64) (out restoreJSON) {
	if err := s.ensureLocal(ctx, &b); err != nil {
		s.Log.Error("restore: fetch backup", "app", a.ID, "backup", b.File, "err", err)
		return restoreJSON{Backup: b.ID, State: "failed", Error: new(backupFailure(err)), Restarted: []string{}, At: s.now()}
	}
	if !a.IsDatabase() {
		return s.runVolumeRestore(ctx, a, b, user)
	}
	out = restoreJSON{Backup: b.ID, State: "failed", Restarted: []string{}}
	defer func() { out.At = s.now() }()
	safety, err := s.makeBackup(ctx, a, store.BackupRestore)
	if err != nil {
		out.Error, out.SafetyFailed = new(backupFailure(err)), true
		return out
	}
	out.Safety = &safety.CreatedAt
	restarted, err := s.restore(ctx, a, b)
	if restarted != nil {
		out.Restarted = restarted
	}
	if err != nil {
		s.Log.Error("restore failed", "app", a.ID, "backup", b.File, "err", err)
		out.Error = new(backupFailure(err))
		return out
	}
	s.Store.SetRestored(ctx, b.ID, s.now())
	s.Log.Info("backup restored", "app", a.ID, "backup", b.File, "safety", safety.File, "user", user)
	out.State = "done"
	return out
}

// restore stops the apps linked to the database, puts the backup back and
// starts them again. Redis is stopped too, since its files are replaced.
// Whatever happens, what was running runs again at the end.
func (s *Server) restore(ctx context.Context, db store.App, b store.Backup) (restarted []string, err error) {
	links, err := s.Store.Links(ctx, "", db.ID)
	if err != nil {
		return nil, err
	}
	inPlace := backup.LoadsInPlace(db.Engine)
	held := []string{}
	for _, l := range links {
		held = append(held, l.AppID)
	}
	if !inPlace {
		held = append(held, db.ID)
	}
	// Always in the same order, so two restores cannot wait on each other.
	slices.Sort(held)
	for _, id := range held {
		s.pauses.hold(id)
		defer s.pauses.release(id)
		unlock := s.deploys.lock(id)
		defer unlock()
	}
	// Started again once the locks are gone, database first.
	var stopped []string
	defer func() {
		for _, id := range stopped {
			s.startAgain(ctx, id, store.CauseRestore)
		}
	}()

	list, err := s.Core.List(ctx)
	if err != nil {
		return nil, err
	}
	stop := func(app string) error {
		for _, c := range list {
			if c.App == app && c.State == "running" {
				if err := s.Core.Stop(ctx, c.ID, volumeStopGrace); err != nil {
					return err
				}
				if !slices.Contains(stopped, app) {
					stopped = append(stopped, app)
				}
			}
		}
		return nil
	}
	if !inPlace {
		// First in stopped: it starts again before its apps.
		if err := stop(db.ID); err != nil {
			return nil, err
		}
	}
	for _, l := range links {
		if err := stop(l.AppID); err != nil {
			return nil, err
		}
	}
	if inPlace {
		container, err := s.liveContainer(ctx, db)
		if err != nil {
			return nil, err
		}
		err = s.Core.RestoreBackup(ctx, db.ID, b.File, db.Engine, container, "")
		return stoppedApps(stopped, db.ID), err
	}
	vols, err := s.Store.Volumes(ctx, db.ID)
	if err != nil {
		return nil, err
	}
	if len(vols) != 1 {
		return nil, fmt.Errorf("%s has %d volumes, not one", db.ID, len(vols))
	}
	err = s.Core.RestoreBackup(ctx, db.ID, b.File, db.Engine, "", vols[0].Name)
	return stoppedApps(stopped, db.ID), err
}

func stoppedApps(stopped []string, db string) []string {
	return slices.DeleteFunc(slices.Clone(stopped), func(id string) bool { return id == db })
}

// startAgain starts an app's live version again after Zelie stopped it.
func (s *Server) startAgain(ctx context.Context, id, cause string) {
	a, err := s.Store.App(ctx, id)
	if err != nil {
		return
	}
	live, err := s.Store.LiveDeployment(ctx, id)
	if err != nil {
		return
	}
	s.crashes.reset(id)
	if _, err := s.deploy(ctx, a, store.Deployment{Version: live.Version, Image: live.Image, Cause: cause, Message: live.Message}); err != nil {
		s.Log.Error("start again", "app", id, "cause", cause, "err", err)
	}
}

func engineLabel(name string) string {
	if e, ok := engineOf(store.App{Engine: name}); ok {
		return e.Label
	}
	return name
}
