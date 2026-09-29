package panel

import (
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/store"
)

// The Files tab. The panel only says which volume and who should own what
// is written; the core does the work inside the volume (core/files.go). Each
// change is logged with the user, the server and the path.

// filesFrom finds the server in the path and the reference the core needs
// for its files.
func (s *Server) filesFrom(w http.ResponseWriter, r *http.Request) (store.App, core.FileRef, bool) {
	a, g, ok := s.gameFrom(w, r)
	if !ok {
		return a, core.FileRef{}, false
	}
	// The installer writes to the same files, and half an install is not a
	// thing to edit.
	if g.InstallState == store.InstallRunning {
		writeError(w, errInstalling.Err())
		return a, core.FileRef{}, false
	}
	vols, err := s.Store.Volumes(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "load volumes", err)
		return a, core.FileRef{}, false
	}
	for _, v := range vols {
		if v.Path != gameVolumePath {
			continue
		}
		ref := core.FileRef{Volume: v.Name, FileOwner: core.FileOwner{UID: gameUID, GID: gameGID}}
		// The limit is measured, not enforced, so this is what the last
		// measurement leaves. Nothing is said when there is none yet.
		if used, ok := s.sizes.get(v.Name); ok {
			room := max(v.LimitMB<<20-used, 0)
			ref.Room = &room
		}
		return a, ref, true
	}
	writeError(w, errNoVolume.Err())
	return a, core.FileRef{}, false
}

func (s *Server) fileLog(r *http.Request, a store.App, what string, paths ...string) {
	s.Log.Info("game file "+what, "server", a.ID, "user", loginFrom(r.Context()).account.ID, "path", strings.Join(paths, ", "))
}

func (s *Server) listGameFiles(w http.ResponseWriter, r *http.Request) {
	_, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Core.ListFiles(r.Context(), ref, r.URL.Query().Get("path"))
	if err != nil {
		s.coreFailed(w, "list files", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// readGameFile sends a text file for the editor as it is.
func (s *Server) readGameFile(w http.ResponseWriter, r *http.Request) {
	_, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	body, err := s.Core.ReadFile(r.Context(), ref, r.URL.Query().Get("path"))
	if err != nil {
		s.coreFailed(w, "read file", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

// writeGameFile saves the editor's text, or with new=1 makes a file and
// refuses a name that is taken.
func (s *Server) writeGameFile(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	// One byte more than the editor may save, so the core says it is too
	// large instead of the file being cut.
	body, err := io.ReadAll(io.LimitReader(r.Body, core.MaxEditBytes+1))
	if err != nil {
		writeError(w, errBadBody.Err())
		return
	}
	p := r.URL.Query().Get("path")
	if err := s.Core.WriteFile(r.Context(), ref, p, body, r.URL.Query().Get("new") == "1"); err != nil {
		s.coreFailed(w, "write file", err)
		return
	}
	s.fileLog(r, a, "written", p)
	w.WriteHeader(http.StatusNoContent)
}

type gamePathRequest struct {
	Path string `json:"path"`
}

func (s *Server) makeGameFolder(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	var req gamePathRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.Core.MakeFolder(r.Context(), ref, req.Path); err != nil {
		s.coreFailed(w, "make folder", err)
		return
	}
	s.fileLog(r, a, "folder made", req.Path)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) renameGameFile(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.Core.RenameFile(r.Context(), ref, req.From, req.To); err != nil {
		s.coreFailed(w, "rename file", err)
		return
	}
	s.fileLog(r, a, "renamed", req.From, req.To)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteGameFiles(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Paths []string `json:"paths"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := s.Core.DeleteFiles(r.Context(), ref, req.Paths); err != nil {
		s.coreFailed(w, "delete files", err)
		return
	}
	s.fileLog(r, a, "deleted", req.Paths...)
	w.WriteHeader(http.StatusNoContent)
}

// uploadGameFile streams the request's body into a file. The core stops it
// at the largest a file may be and at what the disk limit leaves.
func (s *Server) uploadGameFile(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if err := s.Core.UploadFile(r.Context(), ref, p, r.ContentLength, r.Body); err != nil {
		s.coreFailed(w, "upload file", err)
		return
	}
	s.fileLog(r, a, "uploaded", p)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) downloadGameFile(w http.ResponseWriter, r *http.Request) {
	_, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	body, size, err := s.Core.DownloadFile(r.Context(), ref, p)
	if err != nil {
		s.coreFailed(w, "download file", err)
		return
	}
	defer body.Close()
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(strings.TrimRight(p, "/"))})
	if disposition == "" {
		disposition = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", disposition)
	h.Set("Cache-Control", "no-store")
	if size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
	}
	io.Copy(w, body)
}

func (s *Server) compressGameFiles(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Dir   string   `json:"dir"`
		Paths []string `json:"paths"`
		Name  string   `json:"name"`
	}
	if !decode(w, r, &req) {
		return
	}
	name, err := s.Core.CompressFiles(r.Context(), ref, req.Dir, req.Paths, req.Name)
	if err != nil {
		s.coreFailed(w, "compress files", err)
		return
	}
	s.fileLog(r, a, "compressed", append([]string{"-> " + path.Join(req.Dir, name)}, req.Paths...)...)
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func (s *Server) extractGameFile(w http.ResponseWriter, r *http.Request) {
	a, ref, ok := s.filesFrom(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
		Dir  string `json:"dir"`
	}
	if !decode(w, r, &req) {
		return
	}
	res, err := s.Core.ExtractFile(r.Context(), ref, req.Path, req.Dir)
	if err != nil {
		// It may have unpacked part of the archive before it stopped.
		s.fileLog(r, a, "extract stopped", req.Path, "-> "+req.Dir)
		s.coreFailed(w, "extract file", err)
		return
	}
	s.fileLog(r, a, "extracted", req.Path, "-> "+req.Dir)
	writeJSON(w, http.StatusOK, res)
}
