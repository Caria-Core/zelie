package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/msg"
)

// A dump from another server arrives here in pieces, so a large one
// survives a dropped connection, and becomes a backup once it is whole.
// Until then it sits in the clear next to the backups, readable only by
// root, like the database's own files.

// MaxUploadChunk is the most one piece of an upload may carry.
const MaxUploadChunk = 32 << 20

// An upload nobody finished is deleted after this.
const uploadExpiry = 24 * time.Hour

var (
	errUploadOffset     = msg.Define(http.StatusConflict, "upload.offset", "The upload has {received} bytes, not {offset}.")
	errUploadIncomplete = msg.Define(http.StatusConflict, "upload.incomplete", "The upload is not complete: {received} of {size} bytes arrived.")
	errUploadTooLong    = msg.Define(http.StatusBadRequest, "upload.too_long", "The upload is longer than the {size} bytes it said it would be.")
	errNoRoomUpload     = msg.Define(http.StatusUnprocessableEntity, "upload.no_room", "Not enough disk space: the file needs up to {need} and {free} is free, and Zelie keeps 1 GB free for the apps.")
)

var validUpload = regexp.MustCompile(`^[0-9a-f]{32}$`)

type uploadMeta struct {
	App     string    `json:"app"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// Upload is an upload in progress as the core describes it.
type Upload struct {
	ID       string `json:"id"`
	App      string `json:"app"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
}

type importRequest struct {
	Kind string `json:"kind"`
}

func (s *Server) uploadsDir() string { return filepath.Join(s.Backups.Root, ".uploads") }

func (s *Server) uploadPath(id string) (string, error) {
	if !validUpload.MatchString(id) {
		return "", fmt.Errorf("upload %q: %w", id, backup.ErrInvalid)
	}
	return filepath.Join(s.uploadsDir(), id), nil
}

func (s *Server) loadUpload(id string) (Upload, error) {
	p, err := s.uploadPath(id)
	if err != nil {
		return Upload{}, err
	}
	b, err := os.ReadFile(p + ".json")
	if err != nil {
		return Upload{}, err
	}
	var m uploadMeta
	if err := json.Unmarshal(b, &m); err != nil {
		return Upload{}, err
	}
	st, err := os.Stat(p)
	if err != nil {
		return Upload{}, err
	}
	return Upload{ID: id, App: m.App, Size: m.Size, Received: st.Size()}, nil
}

func (s *Server) dropUpload(id string) {
	if p, err := s.uploadPath(id); err == nil {
		os.Remove(p)
		os.Remove(p + ".json")
	}
}

// expireUploads deletes the uploads that were left unfinished.
func (s *Server) expireUploads() {
	entries, _ := os.ReadDir(s.uploadsDir())
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !validUpload.MatchString(id) {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > uploadExpiry {
			s.Log.Info("unfinished upload deleted", "upload", id)
			s.dropUpload(id)
		}
	}
}

func (s *Server) createUpload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		App  string `json:"app"`
		Size int64  `json:"size"`
	}
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !backup.ValidApp(req.App) || req.Size <= 0 {
		writeError(w, http.StatusBadRequest, errors.New("an upload needs an app and a size"))
		return
	}
	s.expireUploads()
	// The file itself, then the backup made from it.
	if err := s.roomFor(2*req.Size, errNoRoomUpload); err != nil {
		s.backupFailed(w, "start upload", req.App, err)
		return
	}
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	p, _ := s.uploadPath(id)
	meta, _ := json.Marshal(uploadMeta{App: req.App, Size: req.Size, Created: time.Now().UTC()})
	err := os.MkdirAll(s.uploadsDir(), 0o700)
	if err == nil {
		err = os.WriteFile(p+".json", meta, 0o600)
	}
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
			err = f.Close()
		}
	}
	if err != nil {
		s.dropUpload(id)
		s.fail(w, "start upload", req.App, err)
		return
	}
	s.Log.Info("upload started", "app", req.App, "upload", id, "bytes", req.Size)
	writeJSON(w, http.StatusCreated, Upload{ID: id, App: req.App, Size: req.Size})
}

func (s *Server) getUpload(w http.ResponseWriter, r *http.Request) {
	u, err := s.loadUpload(r.PathValue("id"))
	if err != nil {
		s.backupFailed(w, "load upload", "", err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// appendUpload adds one piece at the end. offset says where the sender
// thinks the end is; when it is wrong, nothing is written and the answer
// says where the end really is.
func (s *Server) appendUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("offset must be a number"))
		return
	}
	if !s.busy.take("upload " + id) {
		writeError(w, http.StatusConflict, errors.New("another piece of this upload is being written"))
		return
	}
	defer s.busy.done("upload " + id)
	u, err := s.loadUpload(id)
	if err != nil {
		s.backupFailed(w, "upload", "", err)
		return
	}
	if offset != u.Received {
		writeError(w, http.StatusConflict, errUploadOffset.Err("received", u.Received, "offset", offset))
		return
	}
	p, _ := s.uploadPath(id)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		s.fail(w, "upload", u.App, err)
		return
	}
	defer f.Close()
	room := u.Size - u.Received
	n, err := io.Copy(f, io.LimitReader(r.Body, min(room, MaxUploadChunk)+1))
	switch {
	case n > room:
		f.Truncate(u.Received)
		writeError(w, http.StatusBadRequest, errUploadTooLong.Err("size", u.Size))
		return
	case n > MaxUploadChunk:
		f.Truncate(u.Received)
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("a piece may carry up to %d bytes", MaxUploadChunk))
		return
	}
	// What arrived before a broken connection stays: the sender goes on
	// from there.
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if err != nil {
		s.Log.Warn("upload piece cut short", "app", u.App, "upload", id, "bytes", n, "err", err)
	}
	now := time.Now()
	os.Chtimes(p+".json", now, now)
	writeJSON(w, http.StatusOK, Upload{ID: id, App: u.App, Size: u.Size, Received: u.Received + n})
}

func (s *Server) removeUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.uploadPath(id); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.dropUpload(id)
	w.WriteHeader(http.StatusNoContent)
}

// importUpload turns a whole upload into a backup of its app, adapted to
// load here. The upload is gone afterwards, unless Zelie itself failed and
// the same file can be tried again.
func (s *Server) importUpload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req importRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ext, ok := backup.Ext(req.Kind)
	if !ok || req.Kind == backup.KindVolumes {
		writeError(w, http.StatusBadRequest, fmt.Errorf("no import for %q", req.Kind))
		return
	}
	if !s.busy.take("upload " + id) {
		writeError(w, http.StatusConflict, errors.New("this upload is still being written"))
		return
	}
	defer s.busy.done("upload " + id)
	u, err := s.loadUpload(id)
	if err != nil {
		s.backupFailed(w, "import upload", "", err)
		return
	}
	if u.Received != u.Size {
		writeError(w, http.StatusConflict, errUploadIncomplete.Err("received", u.Received, "size", u.Size))
		return
	}
	info, err := s.importFile(r.Context(), u, req.Kind, ext)
	var me *msg.Error
	if err == nil || errors.As(err, &me) {
		s.dropUpload(id)
	}
	if err != nil {
		s.backupFailed(w, "import upload", u.App, err)
		return
	}
	s.Log.Info("upload imported", "app", u.App, "upload", id, "backup", info.Name, "adapted", info.Adapted)
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) importFile(ctx context.Context, u Upload, kind, ext string) (backup.Info, error) {
	if err := s.roomFor(u.Size, errNoRoomBackup); err != nil {
		return backup.Info{}, err
	}
	p, _ := s.uploadPath(u.ID)
	f, err := os.Open(p)
	if err != nil {
		return backup.Info{}, err
	}
	defer f.Close()
	bw, err := s.Backups.Create(u.App, ext)
	if err != nil {
		return backup.Info{}, err
	}
	adapted, err := backup.ImportDump(kind, contextReader{ctx, f}, bw)
	if err != nil {
		bw.Abort()
		return backup.Info{}, err
	}
	info, err := bw.Commit()
	info.Adapted = adapted
	return info, err
}

// contextReader stops reading once ctx is done, so an import the panel gave
// up on does not run to the end.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

// StartUpload makes room for a file of size bytes for app.
func (c *Client) StartUpload(ctx context.Context, app string, size int64) (Upload, error) {
	var u Upload
	err := c.do(ctx, http.MethodPost, "/v1/uploads", map[string]any{"app": app, "size": size}, &u)
	return u, err
}

func (c *Client) Upload(ctx context.Context, id string) (Upload, error) {
	var u Upload
	err := c.do(ctx, http.MethodGet, "/v1/uploads/"+url.PathEscape(id), nil, &u)
	return u, err
}

// AppendUpload sends one piece of an upload, which starts at offset.
func (c *Client) AppendUpload(ctx context.Context, id string, offset int64, piece io.Reader) (Upload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://core/v1/uploads/"+url.PathEscape(id)+"?offset="+strconv.FormatInt(offset, 10), piece)
	if err != nil {
		return Upload{}, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return Upload{}, fmt.Errorf("reach the Zelie core: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Upload{}, readError(resp)
	}
	var u Upload
	err = json.NewDecoder(resp.Body).Decode(&u)
	return u, err
}

func (c *Client) RemoveUpload(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/uploads/"+url.PathEscape(id), nil, nil)
}

// ImportUpload makes a backup of a whole upload, as a dump of kind.
func (c *Client) ImportUpload(ctx context.Context, id, kind string) (Backup, error) {
	var info Backup
	err := c.do(ctx, http.MethodPost, "/v1/uploads/"+url.PathEscape(id)+"/import", importRequest{Kind: kind}, &info)
	return info, err
}
