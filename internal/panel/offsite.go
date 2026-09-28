package panel

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Off-site copies (K74). The core sends backups to the S3 bucket and holds
// its keys; the panel decides what goes, tries again after a failure and
// deletes what is past its time.

// A copy that failed to go is tried again after a minute, then twice as
// long each time, up to offsiteMaxWait.
const offsiteMaxWait = 6 * time.Hour

// never is when a send that cannot work is tried again.
var never = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// Copies of a backup brought back from off-site storage stay here this
// long, for the restore or download they were fetched for.
const fetchedKeep = 24 * time.Hour

var (
	errOffsiteKeep  = msg.Define(http.StatusBadRequest, "backup.bad_offsite_keep", "Off-site copies are kept at least as long as the backups here, and at most 3650 days.")
	errNotOffsite   = msg.Define(http.StatusConflict, "backup.not_offsite", "Set up off-site storage first.")
	errOffsiteNever = msg.Define(0, "offsite.never", "The backup file was gone before it could be sent.")
)

func offsiteWait(tries int) time.Duration {
	if tries > 10 {
		return offsiteMaxWait
	}
	return min(time.Minute<<(tries-1), offsiteMaxWait)
}

// offsiteSet reports whether the core has somewhere to send backups.
func (s *Server) offsiteSet(ctx context.Context) bool {
	info, err := s.Core.Offsite(ctx)
	if err != nil {
		s.Log.Warn("off-site storage: ask the core", "err", err)
		return false
	}
	return info.Set
}

// queueOffsite marks a new backup to be sent, when its plan says so and
// there is somewhere to send it.
func (s *Server) queueOffsite(ctx context.Context, b store.Backup, plan store.BackupPlan) {
	if !plan.Offsite || !s.offsiteSet(ctx) {
		return
	}
	until := b.CreatedAt.Add(time.Duration(plan.OffsiteDays) * 24 * time.Hour)
	if err := s.Store.SendOffsite(ctx, b.ID, until); err != nil {
		s.Log.Error("off-site storage: queue", "backup", b.ID, "err", err)
		return
	}
	s.kickUploads()
}

func (s *Server) kickUploads() {
	select {
	case s.uploadKick <- struct{}{}:
	default:
	}
}

// runUploads sends backups one at a time. It is apart from the schedule,
// so a long upload does not hold back the next backups.
func (s *Server) runUploads(ctx context.Context) {
	t := time.NewTicker(backupEvery)
	defer t.Stop()
	for {
		s.uploadsOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.uploadKick:
		}
	}
}

func (s *Server) uploadsOnce(ctx context.Context) {
	due, err := s.Store.OffsiteDue(ctx, s.now())
	if err != nil {
		s.Log.Error("off-site storage: list", "err", err)
		return
	}
	if len(due) == 0 || !s.offsiteSet(ctx) {
		return
	}
	for _, b := range due {
		if ctx.Err() != nil {
			return
		}
		s.upload(ctx, b)
	}
}

func (s *Server) upload(ctx context.Context, b store.Backup) {
	key := strconv.FormatInt(b.ID, 10)
	s.uploading.take(key)
	err := s.Core.UploadBackup(ctx, b.AppID, b.File)
	s.uploading.done(key)
	if err == nil {
		err = s.Store.OffsiteSent(ctx, b.ID, s.now())
		if errors.Is(err, store.ErrNotFound) {
			// Deleted while it was on its way.
			s.removeOffsite(ctx, b)
			return
		}
		if err != nil {
			s.Log.Error("off-site storage: record", "backup", b.ID, "err", err)
		}
		s.Log.Info("backup sent off-site", "app", b.AppID, "backup", b.File)
		return
	}
	if ctx.Err() != nil {
		return
	}
	if isNotFound(err) {
		s.Store.OffsiteFailedAt(ctx, b.ID, errOffsiteNever.With(), b.OffsiteTries+1, never)
		return
	}
	tries := b.OffsiteTries + 1
	s.Log.Warn("backup not sent off-site", "app", b.AppID, "backup", b.File, "tries", tries, "err", err)
	if err := s.Store.OffsiteFailedAt(ctx, b.ID, backupFailure(err), tries, s.now().Add(offsiteWait(tries))); err != nil {
		s.Log.Error("off-site storage: record", "backup", b.ID, "err", err)
	}
}

// removeOffsite deletes a backup's off-site copy. With no destination
// set, there is nothing Zelie may still delete: the bucket is the user's.
func (s *Server) removeOffsite(ctx context.Context, b store.Backup) error {
	err := s.Core.RemoveOffsiteBackup(ctx, b.AppID, b.File)
	var ce *core.Error
	if errors.As(err, &ce) && ce.Code == "offsite.not_set" {
		return nil
	}
	return err
}

// ensureLocal brings a backup kept only off-site back to this server.
func (s *Server) ensureLocal(ctx context.Context, b *store.Backup) error {
	if b.Local {
		return nil
	}
	if err := s.Core.FetchBackup(ctx, b.AppID, b.File); err != nil {
		return err
	}
	b.Local = true
	b.KeepUntil = s.now().Add(fetchedKeep)
	return s.Store.SetLocal(ctx, b.ID, true, b.KeepUntil)
}

// pruneBackups deletes what is past its time. A backup whose file here is
// due to go but whose off-site copy is not stays listed, as off-site only.
func (s *Server) pruneBackups(ctx context.Context) {
	now := s.now()
	expired, err := s.Store.ExpiredBackups(ctx, now)
	if err != nil {
		s.Log.Error("backups: list expired", "err", err)
		return
	}
	for _, b := range expired {
		if b.File != "" {
			if err := s.Core.RemoveBackup(ctx, b.AppID, b.File); err != nil && !isNotFound(err) {
				s.Log.Error("backups: remove", "app", b.AppID, "backup", b.File, "err", err)
				continue
			}
		}
		if b.Offsite == store.OffsiteDone && b.OffsiteUntil.After(now) {
			if err := s.Store.SetLocal(ctx, b.ID, false, time.Time{}); err != nil {
				s.Log.Error("backups: record", "id", b.ID, "err", err)
			}
			continue
		}
		s.forgetBackup(ctx, b)
	}
	gone, err := s.Store.ExpiredOffsite(ctx, now)
	if err != nil {
		s.Log.Error("backups: list expired off-site", "err", err)
		return
	}
	for _, b := range gone {
		s.forgetBackup(ctx, b)
	}
}

// forgetBackup deletes a backup's off-site copy, if it has one, and then
// the record of it.
func (s *Server) forgetBackup(ctx context.Context, b store.Backup) {
	if b.Offsite == store.OffsiteDone {
		if err := s.removeOffsite(ctx, b); err != nil {
			s.Log.Error("backups: remove off-site", "app", b.AppID, "backup", b.File, "err", err)
			return
		}
	}
	if err := s.Store.DeleteBackup(ctx, b.ID); err != nil {
		s.Log.Error("backups: forget", "id", b.ID, "err", err)
	}
}

// sendNow sends a backup off-site again right away, after a failure or
// when it was never sent.
func (s *Server) sendNow(w http.ResponseWriter, r *http.Request) {
	b, ok := s.backupFrom(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !s.offsiteSet(ctx) {
		writeError(w, errNotOffsite.Err())
		return
	}
	if b.Offsite == store.OffsiteDone || !b.Local {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	days := 30
	if a, err := s.Store.App(ctx, b.AppID); err == nil {
		if plan, err := s.planFor(ctx, a); err == nil {
			days = plan.OffsiteDays
		}
	}
	if err := s.Store.SendOffsite(ctx, b.ID, b.CreatedAt.Add(time.Duration(days)*24*time.Hour)); err != nil {
		s.fail(w, "queue off-site copy", err)
		return
	}
	s.kickUploads()
	w.WriteHeader(http.StatusAccepted)
}

// getOffsite describes the destination, and whether the recovery file
// that opens what is there was saved.
func (s *Server) getOffsite(w http.ResponseWriter, r *http.Request) {
	info, err := s.Core.Offsite(r.Context())
	if err != nil {
		s.coreFailed(w, "off-site storage", err)
		return
	}
	out := struct {
		core.OffsiteInfo
		RecoverySavedAt *time.Time `json:"recovery_saved_at"`
	}{OffsiteInfo: info}
	if at, err := s.Store.RecoverySavedAt(r.Context()); err == nil && !at.IsZero() {
		out.RecoverySavedAt = &at
	}
	writeJSON(w, http.StatusOK, out)
}

// setOffsite hands a destination to the core, which checks it and keeps
// its keys. The folder defaults to one named after this server, so two
// servers can share a bucket.
func (s *Server) setOffsite(w http.ResponseWriter, r *http.Request) {
	var req core.OffsiteConfig
	if !decode(w, r, &req) {
		return
	}
	if strings.Trim(strings.TrimSpace(req.Prefix), "/") == "" {
		req.Prefix = "zelie/" + hostFolder()
	}
	info, err := s.Core.SetOffsite(r.Context(), req)
	if err != nil {
		s.coreFailed(w, "set off-site storage", err)
		return
	}
	s.Log.Info("off-site storage set", "endpoint", info.Endpoint, "bucket", info.Bucket, "prefix", info.Prefix, "user", loginFrom(r.Context()).account.ID)
	s.kickUploads()
	writeJSON(w, http.StatusOK, info)
}

// hostFolder is the server's name as a folder name.
func hostFolder() string {
	name, _ := os.Hostname()
	name = strings.ToLower(name)
	var b strings.Builder
	for _, c := range name {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" || len(out) > 60 {
		return "server"
	}
	return out
}

// removeOffsiteStorage forgets the destination. Backups waiting to be sent are
// not sent; what is already there stays, as the user's.
func (s *Server) removeOffsiteStorage(w http.ResponseWriter, r *http.Request) {
	if err := s.Core.RemoveOffsite(r.Context()); err != nil {
		s.coreFailed(w, "remove off-site storage", err)
		return
	}
	if err := s.Store.DropOffsiteQueue(r.Context()); err != nil {
		s.fail(w, "drop off-site queue", err)
		return
	}
	s.Log.Info("off-site storage removed", "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}
