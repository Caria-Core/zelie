package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/containerd/errdefs"
)

type backupRequest struct {
	App       string `json:"app"`
	Container string `json:"container,omitempty"`
	Kind      string `json:"kind"`
	// Volumes and Live are for a backup of an app's volumes. Live copies
	// them while the app runs; otherwise its containers must be stopped.
	Volumes []VolumeRef `json:"volumes,omitempty"`
	Live    bool        `json:"live,omitempty"`
}

type restoreRequest struct {
	Kind string `json:"kind"`
	// Container is the running database, for kinds that load in place.
	Container string `json:"container,omitempty"`
	// Volume holds a stopped database's files, for the others.
	Volume string `json:"volume,omitempty"`
	// Volumes are the stopped app's volumes, and Size how much the
	// backup's files take, for a restore of a volume backup.
	Volumes []VolumeRef `json:"volumes,omitempty"`
	Size    int64       `json:"size,omitempty"`
}

// busy keeps a backup and a restore of the same app from running at once.
type busy struct {
	mu   sync.Mutex
	apps map[string]bool
}

func (b *busy) take(app string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.apps[app] {
		return false
	}
	if b.apps == nil {
		b.apps = map[string]bool{}
	}
	b.apps[app] = true
	return true
}

func (b *busy) done(app string) {
	b.mu.Lock()
	delete(b.apps, app)
	b.mu.Unlock()
}

var errBusy = errors.New("a backup or restore of this app is already running")

// containerOf checks that id is a running container of app, so a backup of
// one app can never be taken from another's container.
func (s *Server) containerOf(ctx context.Context, app, id string) error {
	list, err := s.Engine.List(ctx)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.ID == id {
			if c.App != app {
				return fmt.Errorf("container %s is not %s's: %w", id, app, errdefs.ErrInvalidArgument)
			}
			if c.State != "running" {
				return fmt.Errorf("container %s is not running: %w", id, errdefs.ErrFailedPrecondition)
			}
			return nil
		}
	}
	return fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
}

func (s *Server) execIn(id string) backup.Exec {
	return func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
		return s.Engine.Exec(ctx, id, args, stdin, stdout, stderr)
	}
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request) {
	var req backupRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Kind == backup.KindVolumes {
		s.backUpVolumes(w, r, req)
		return
	}
	ext, ok := backup.Ext(req.Kind)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no backup for %q", req.Kind))
		return
	}
	ctx := r.Context()
	if err := s.containerOf(ctx, req.App, req.Container); err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	if !s.busy.take(req.App) {
		writeError(w, http.StatusConflict, errBusy)
		return
	}
	defer s.busy.done(req.App)
	start := time.Now()
	bw, err := s.Backups.Create(req.App, ext)
	if err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	if err := backup.Dump(ctx, s.execIn(req.Container), req.Kind, bw); err != nil {
		bw.Abort()
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	info, err := bw.Commit()
	if err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	s.Log.Info("backup made", "app", req.App, "backup", info.Name, "bytes", info.Bytes, "took", time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) listBackups(w http.ResponseWriter, r *http.Request) {
	list, err := s.Backups.List(r.PathValue("app"))
	if err != nil {
		s.backupFailed(w, "list backups", r.PathValue("app"), err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// downloadBackup sends the file as it is on disk: encrypted. Nothing leaves
// the server in the clear.
func (s *Server) downloadBackup(w http.ResponseWriter, r *http.Request) {
	f, err := s.Backups.OpenRaw(r.PathValue("app"), r.PathValue("name"))
	if err != nil {
		s.backupFailed(w, "download backup", r.PathValue("app"), err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.backupFailed(w, "download backup", r.PathValue("app"), err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	io.Copy(w, f)
}

func (s *Server) removeBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	if err := s.Backups.Remove(app, name); err != nil {
		s.backupFailed(w, "remove backup", app, err)
		return
	}
	s.Log.Info("backup removed", "app", app, "backup", name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	app, name := r.PathValue("app"), r.PathValue("name")
	var req restoreRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ext, ok := backup.Ext(req.Kind)
	if !ok || !strings.HasSuffix(name, "."+ext+".zst.age") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("%s is not a %s backup", name, req.Kind))
		return
	}
	if req.Kind == backup.KindVolumes {
		s.restoreVolumes(w, r, app, name, req)
		return
	}
	ctx := r.Context()
	inPlace := backup.LoadsInPlace(req.Kind)
	if inPlace {
		if err := s.containerOf(ctx, app, req.Container); err != nil {
			s.backupFailed(w, "restore", app, err)
			return
		}
	}
	if !s.busy.take(app) {
		writeError(w, http.StatusConflict, errBusy)
		return
	}
	defer s.busy.done(app)
	src, err := s.Backups.Open(app, name)
	if err != nil {
		s.backupFailed(w, "restore", app, err)
		return
	}
	defer src.Close()
	if inPlace {
		err = backup.Load(ctx, s.execIn(req.Container), req.Kind, src)
	} else {
		var vol *os.Root
		if vol, err = s.Engine.OpenVolume(ctx, req.Volume); err == nil {
			err = backup.RestoreRedis(vol, src)
			vol.Close()
		}
	}
	if err != nil {
		s.backupFailed(w, "restore", app, err)
		return
	}
	s.Log.Info("backup restored", "app", app, "backup", name)
	w.WriteHeader(http.StatusNoContent)
}

// recoveryKey hands out the key that opens every backup. The panel only
// asks for it after the user confirmed who they are.
func (s *Server) recoveryKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, s.Backups.Key.Recovery(r.URL.Query().Get("host"), time.Now()))
}

func (s *Server) backupFailed(w http.ResponseWriter, op, app string, err error) {
	var me *msg.Error
	switch {
	case errors.As(err, &me):
		s.Log.Warn(op+" failed", "app", app, "err", err)
		writeError(w, me.Status, err)
	case errors.Is(err, os.ErrNotExist), errdefs.IsNotFound(err):
		writeError(w, http.StatusNotFound, errors.New("backup, container or volume not found"))
	case errdefs.IsInvalidArgument(err), errors.Is(err, backup.ErrInvalid):
		writeError(w, http.StatusBadRequest, err)
	case errdefs.IsFailedPrecondition(err):
		writeError(w, http.StatusConflict, err)
	default:
		s.fail(w, op, app, err)
	}
}

// Backup is a backup file as the core describes it.
type Backup = backup.Info

// CreateBackup backs up a database from its running container.
func (c *Client) CreateBackup(ctx context.Context, app, container, kind string) (Backup, error) {
	var info Backup
	err := c.do(ctx, http.MethodPost, "/v1/backups", backupRequest{App: app, Container: container, Kind: kind}, &info)
	return info, err
}

// Backups lists an app's backup files, newest first.
func (c *Client) Backups(ctx context.Context, app string) ([]Backup, error) {
	var list []Backup
	err := c.do(ctx, http.MethodGet, "/v1/backups/"+url.PathEscape(app), nil, &list)
	return list, err
}

// DownloadBackup copies the encrypted file to w.
func (c *Client) DownloadBackup(ctx context.Context, app, name string, w io.Writer) error {
	return c.do(ctx, http.MethodGet, "/v1/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name), nil, w)
}

func (c *Client) RemoveBackup(ctx context.Context, app, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name), nil, nil)
}

// RestoreBackup puts a backup back: into the running container for kinds
// that load in place, into the stopped database's volume for the others.
func (c *Client) RestoreBackup(ctx context.Context, app, name, kind, container, volume string) error {
	return c.do(ctx, http.MethodPost, "/v1/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name)+"/restore",
		restoreRequest{Kind: kind, Container: container, Volume: volume}, nil)
}

// BackUpVolumes backs up an app's volumes into one archive. Live copies
// them while the app runs; otherwise the app must be stopped.
func (c *Client) BackUpVolumes(ctx context.Context, app string, vols []VolumeRef, live bool) (Backup, error) {
	var info Backup
	err := c.do(ctx, http.MethodPost, "/v1/backups", backupRequest{App: app, Kind: backup.KindVolumes, Volumes: vols, Live: live}, &info)
	return info, err
}

// RestoreVolumes replaces the files of a stopped app's volumes with a
// backup's. size is how much the backup's files take, from when it was made.
func (c *Client) RestoreVolumes(ctx context.Context, app, name string, vols []VolumeRef, size int64) error {
	return c.do(ctx, http.MethodPost, "/v1/backups/"+url.PathEscape(app)+"/"+url.PathEscape(name)+"/restore",
		restoreRequest{Kind: backup.KindVolumes, Volumes: vols, Size: size}, nil)
}

// RecoveryKey returns the recovery file for the backups.
func (c *Client) RecoveryKey(ctx context.Context, host string, w io.Writer) error {
	return c.do(ctx, http.MethodGet, "/v1/backups-key?host="+url.QueryEscape(host), nil, w)
}
