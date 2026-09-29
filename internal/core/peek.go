package core

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"syscall"

	"github.com/Caria-Core/zelie/internal/engine"
)

// MaxPeek is the most a peek returns. It is for small text files the panel
// reads to learn something about a server, such as a game's manifest.
const MaxPeek = 1 << 20

// PeekRequest names one file inside a volume.
type PeekRequest struct {
	Path string `json:"path"`
}

// PeekResponse holds the file as it is.
type PeekResponse struct {
	Content []byte `json:"content"`
}

func (s *Server) peekVolume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !engine.ValidID(name) {
		writeError(w, http.StatusBadRequest, errors.New("invalid volume name"))
		return
	}
	var req PeekRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	clean, err := insideVolume(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	root, err := s.Engine.OpenVolume(r.Context(), name)
	if err != nil {
		s.volumeFailed(w, "read volume", name, err)
		return
	}
	defer root.Close()
	content, err := peekFile(root, clean)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, errors.New("no such file"))
	case err != nil:
		writeError(w, http.StatusUnprocessableEntity, err)
	default:
		writeJSON(w, http.StatusOK, PeekResponse{Content: content})
	}
}

// peekFile reads a regular file through the volume's root, so a link a
// player made in the volume cannot lead the read out of it.
func peekFile(root *os.Root, name string) ([]byte, error) {
	// Not blocking, so a named pipe in its place does not hold us up.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}
	if st.Size() > MaxPeek {
		return nil, errors.New("the file is too large to read this way")
	}
	return io.ReadAll(io.LimitReader(f, MaxPeek))
}

// PeekVolumeFile returns the file at path inside a volume, which may be in
// use. A file that is not there is a *Error with status 404.
func (c *Client) PeekVolumeFile(ctx context.Context, volume, path string) ([]byte, error) {
	var out PeekResponse
	err := c.do(ctx, http.MethodPost, "/v1/volumes/"+url.PathEscape(volume)+"/peek", PeekRequest{Path: path}, &out)
	return out.Content, err
}
