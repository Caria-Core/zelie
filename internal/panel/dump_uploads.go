package panel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// A dump from another server is uploaded in pieces straight to the core,
// which keeps it. The panel only remembers whose it is. Once whole, it
// becomes a backup of the database and is restored like any other.

// dumpChunk is how much the browser sends at a time: small enough to
// resend cheaply when a connection drops, large enough to be quick.
const dumpChunk = 16 << 20

var (
	errDumpIntoApp = msg.Define(http.StatusConflict, "upload.not_database", "Only a database can be restored from a dump.")
	errDumpType    = msg.Define(http.StatusBadRequest, "upload.type", "A {engine} database takes {types} files.")
	errNoDump      = msg.Define(http.StatusNotFound, "upload.not_found", "This upload is gone. Start it again.")
	errBadOffset   = msg.Define(http.StatusBadRequest, "upload.bad_offset", "Say where the piece starts, in bytes.")
)

// dumpTypes are the files each engine takes, compressed or not. What is
// inside is checked again when the file is read.
var dumpTypes = map[string][]string{
	"postgres": {".sql", ".sql.gz", ".sql.zst"},
	"mariadb":  {".sql", ".sql.gz", ".sql.zst"},
	"redis":    {".rdb", ".rdb.gz", ".rdb.zst"},
}

type dumpUpload struct {
	ID      string
	App     string
	Name    string
	Size    int64
	Account int64
}

// dumpUploads are the uploads in progress, one per database at most.
type dumpUploads struct {
	mu    sync.Mutex
	byApp map[string]dumpUpload
}

func (d *dumpUploads) get(app string) (dumpUpload, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	u, ok := d.byApp[app]
	return u, ok
}

func (d *dumpUploads) set(u dumpUpload) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byApp == nil {
		d.byApp = map[string]dumpUpload{}
	}
	d.byApp[u.App] = u
}

func (d *dumpUploads) remove(app, id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.byApp[app].ID == id {
		delete(d.byApp, app)
	}
}

type dumpUploadJSON struct {
	ID       string `json:"id"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
	Chunk    int64  `json:"chunk"`
}

// startDumpUpload begins an upload, or finds the one this person started
// for the same file, so it goes on where it stopped. Starting counts as
// asking for the restore, so it is confirmed like one.
func (s *Server) startDumpUpload(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !a.IsDatabase() {
		writeError(w, errDumpIntoApp.Err())
		return
	}
	types := dumpTypes[a.Engine]
	if a.Engine == "postgres" && hasSuffixFold(req.Name, []string{".dump", ".backup"}) {
		writeError(w, backup.ErrImportPgCustom.Err())
		return
	}
	if !hasSuffixFold(req.Name, types) || req.Size <= 0 {
		writeError(w, errDumpType.Err("engine", engineLabel(a.Engine), "types", strings.Join(types, ", ")))
		return
	}
	if a.Stopped {
		writeError(w, errStartDBFirst.Err())
		return
	}
	ctx := r.Context()
	account := loginFrom(ctx).account.ID
	if old, ok := s.dumps.get(a.ID); ok {
		if old.Name == req.Name && old.Size == req.Size && old.Account == account {
			if u, err := s.Core.Upload(ctx, old.ID); err == nil {
				writeJSON(w, http.StatusOK, dumpUploadJSON{ID: u.ID, Size: u.Size, Received: u.Received, Chunk: dumpChunk})
				return
			}
		}
		s.Core.RemoveUpload(ctx, old.ID)
		s.dumps.remove(a.ID, old.ID)
	}
	u, err := s.Core.StartUpload(ctx, a.ID, req.Size)
	if err != nil {
		s.coreFailed(w, "start upload", err)
		return
	}
	s.dumps.set(dumpUpload{ID: u.ID, App: a.ID, Name: req.Name, Size: req.Size, Account: account})
	s.Log.Info("dump upload started", "app", a.ID, "file", req.Name, "bytes", req.Size, "user", account)
	writeJSON(w, http.StatusCreated, dumpUploadJSON{ID: u.ID, Size: u.Size, Chunk: dumpChunk})
}

func hasSuffixFold(name string, suffixes []string) bool {
	name = strings.ToLower(name)
	for _, s := range suffixes {
		if strings.HasSuffix(name, s) && len(name) > len(s) {
			return true
		}
	}
	return false
}

// dumpFrom finds the upload in the path, if it is this person's.
func (s *Server) dumpFrom(w http.ResponseWriter, r *http.Request) (store.App, dumpUpload, bool) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return a, dumpUpload{}, false
	}
	u, ok := s.dumps.get(a.ID)
	if !ok || u.ID != r.PathValue("id") || u.Account != loginFrom(r.Context()).account.ID {
		writeError(w, errNoDump.Err())
		return a, u, false
	}
	return a, u, true
}

func (s *Server) appendDumpUpload(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.dumpFrom(w, r)
	if !ok {
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		writeError(w, errBadOffset.Err())
		return
	}
	got, err := s.Core.AppendUpload(r.Context(), u.ID, offset, http.MaxBytesReader(w, r.Body, core.MaxUploadChunk))
	if isNotFound(err) {
		s.dumps.remove(u.App, u.ID)
		writeError(w, errNoDump.Err())
		return
	}
	if err != nil {
		s.coreFailed(w, "upload", err)
		return
	}
	writeJSON(w, http.StatusOK, dumpUploadJSON{ID: got.ID, Size: got.Size, Received: got.Received, Chunk: dumpChunk})
}

func (s *Server) cancelDumpUpload(w http.ResponseWriter, r *http.Request) {
	_, u, ok := s.dumpFrom(w, r)
	if !ok {
		return
	}
	if err := s.Core.RemoveUpload(r.Context(), u.ID); err != nil && !isNotFound(err) {
		s.coreFailed(w, "cancel upload", err)
		return
	}
	s.dumps.remove(u.App, u.ID)
	w.WriteHeader(http.StatusNoContent)
}

// finishDumpUpload turns the whole file into a backup and restores it, in
// the background like any restore. The page follows both on the database's
// Backups tab.
func (s *Server) finishDumpUpload(w http.ResponseWriter, r *http.Request) {
	a, u, ok := s.dumpFrom(w, r)
	if !ok {
		return
	}
	if a.Stopped {
		writeError(w, errStartDBFirst.Err())
		return
	}
	if !s.backupBusy.take(a.ID) {
		writeError(w, errBackupBusy.Err())
		return
	}
	s.dumps.remove(a.ID, u.ID)
	ctx := context.WithoutCancel(r.Context())
	user := loginFrom(ctx).account.ID
	s.restores.set(a.ID, restoreJSON{State: "running", Restarted: []string{}, At: s.now()})
	s.jobs.Go(func() {
		defer s.backupBusy.done(a.ID)
		b, err := s.importDump(ctx, a, u)
		if err != nil {
			// After a failure that is not the file's, the core keeps it for
			// another try. Nothing here offers one, and it is a whole dump
			// in the clear.
			if rerr := s.Core.RemoveUpload(ctx, u.ID); rerr != nil && !isNotFound(rerr) {
				s.Log.Error("dump upload: remove", "app", a.ID, "upload", u.ID, "err", rerr)
			}
			s.restores.set(a.ID, restoreJSON{Backup: b.ID, State: "failed", Error: new(backupFailure(err)), Restarted: []string{}, At: s.now()})
			return
		}
		s.restores.set(a.ID, restoreJSON{Backup: b.ID, State: "running", Restarted: []string{}, At: s.now()})
		out := s.runRestore(ctx, a, b, user)
		s.restores.set(a.ID, out)
	})
	w.WriteHeader(http.StatusAccepted)
}

// importDump has the core make a backup of the upload, and records it. A
// failure is on the list too, with its reason, like a failed backup.
func (s *Server) importDump(ctx context.Context, a store.App, u dumpUpload) (store.Backup, error) {
	plan, err := s.planFor(ctx, a)
	if err != nil {
		return store.Backup{}, err
	}
	now := s.now()
	b := store.Backup{AppID: a.ID, Engine: a.Engine, Reason: store.BackupUploaded, State: store.BackupRunning, CreatedAt: now,
		KeepUntil: now.Add(time.Duration(plan.KeepDays) * 24 * time.Hour)}
	if b.ID, err = s.Store.StartBackup(ctx, b); err != nil {
		return b, err
	}
	info, err := s.Core.ImportUpload(ctx, u.ID, a.Engine)
	var failure *msg.Msg
	if err != nil {
		failure = new(backupFailure(err))
		s.Log.Warn("dump upload refused", "app", a.ID, "file", u.Name, "err", err)
	} else {
		s.Log.Info("dump uploaded", "app", a.ID, "file", u.Name, "backup", info.Name, "adapted", info.Adapted)
	}
	if ferr := s.Store.FinishBackup(ctx, b.ID, info.Name, info.Bytes, failure, s.now()); ferr != nil {
		return b, errors.Join(err, ferr)
	}
	if err != nil {
		return b, err
	}
	if len(info.Adapted) > 0 {
		if err := s.Store.SetAdapted(ctx, b.ID, info.Adapted); err != nil {
			return b, err
		}
	}
	return s.Store.Backup(ctx, b.ID)
}
