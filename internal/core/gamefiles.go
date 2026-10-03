package core

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"syscall"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
)

// What a game server needs from the core besides its container: its files
// made ready before each start, and a way to talk to its console.

var errInputClosed = msg.Define(http.StatusConflict, "container.input_closed", "The server is not reading its console.")

const (
	maxStdinBytes    = 4 << 10 // one console line; also what a pipe takes in one piece
	maxConfigFiles   = 64
	maxConfigChanges = 256
	maxConfigText    = 4 << 10
)

// PrepareRequest makes a game server's volume ready to start: the config
// files its egg asks for are edited, and every file is given to the user the
// server runs as. It is refused while a container of the volume runs.
type PrepareRequest struct {
	// UID and GID are the owner as the container sees it. The volume keeps
	// container IDs on disk, so this is also what the files carry.
	UID   uint32       `json:"uid"`
	GID   uint32       `json:"gid"`
	Files []ConfigFile `json:"files,omitempty"`
}

// ConfigFile is a file to edit, with its values already filled in.
type ConfigFile struct {
	Path    string         `json:"path"`
	Parser  string         `json:"parser"`
	Changes []ConfigChange `json:"changes"`
}

type ConfigChange struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	IfValue string `json:"if_value,omitempty"`
	// Add appends the line of a "file" parser change when it is missing.
	Add bool `json:"add,omitempty"`
}

// PrepareResponse lists what could not be done. Nothing in it stops the
// server from starting; the panel shows it in the start's log.
type PrepareResponse struct {
	Notes []string `json:"notes"`
}

func (r PrepareRequest) check() error {
	if !(engine.IDs{UID: r.UID, GID: r.GID}).Valid() {
		return fmt.Errorf("owner %d:%d is outside the container's IDs", r.UID, r.GID)
	}
	if len(r.Files) > maxConfigFiles {
		return fmt.Errorf("at most %d config files", maxConfigFiles)
	}
	for _, f := range r.Files {
		if len(f.Path) > maxConfigText || len(f.Changes) > maxConfigChanges {
			return fmt.Errorf("config file %.64q is too large a request", f.Path)
		}
		for _, c := range f.Changes {
			if len(c.Key) > maxConfigText || len(c.Value) > maxConfigText || len(c.IfValue) > maxConfigText {
				return fmt.Errorf("a change to %.64q is too long", f.Path)
			}
		}
	}
	return nil
}

func (s *Server) prepareVolume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !engine.ValidID(name) {
		writeError(w, http.StatusBadRequest, errors.New("invalid volume name"))
		return
	}
	var req PrepareRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := req.check(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	root, err := s.Engine.OpenVolume(r.Context(), name)
	if err != nil {
		s.volumeFailed(w, "prepare volume", name, err)
		return
	}
	defer root.Close()
	notes, err := prepareVolume(root, req)
	if err != nil {
		s.fail(w, "prepare volume", name, err)
		return
	}
	for _, n := range notes {
		s.Log.Warn("config file", "volume", name, "note", n)
	}
	s.Log.Info("volume prepared", "volume", name, "files", len(req.Files))
	writeJSON(w, http.StatusOK, PrepareResponse{Notes: notes})
}

// prepareVolume edits the config files and then changes owners, so a file
// made on the way belongs to the server too. A file that cannot be edited
// is a note, not a failure: an egg's file list is written for the game's
// default files, and a server may lack them. Failing to change owners is
// an error, as the server could not write its own files.
func prepareVolume(root *os.Root, req PrepareRequest) (notes []string, err error) {
	for _, f := range req.Files {
		skipped, err := editConfigFile(root, f)
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s was not changed: %v", f.Path, err))
			continue
		}
		for _, k := range skipped {
			notes = append(notes, fmt.Sprintf("%s: %s was skipped", f.Path, k))
		}
	}
	return notes, chownAll(root, int(req.UID), int(req.GID))
}

// insideVolume turns a path from an egg into one below the volume's top.
func insideVolume(p string) (string, error) {
	clean := path.Clean(strings.TrimLeft(p, "/"))
	switch {
	case clean == "." || clean == ".." || strings.HasPrefix(clean, "../"):
		return "", fmt.Errorf("%q is not a file inside the server's directory", p)
	case strings.ContainsRune(clean, 0):
		return "", errors.New("the path holds a null byte")
	}
	return clean, nil
}

// editConfigFile changes one file. The volume's root keeps every step of
// the path, symbolic links included, inside the volume: what an installer or
// a player put there cannot point the edit at the host.
func editConfigFile(root *os.Root, f ConfigFile) (skipped []string, err error) {
	name, err := insideVolume(f.Path)
	if err != nil {
		return nil, err
	}
	old, mode, existed, err := readConfigFile(root, name)
	if err != nil {
		return nil, err
	}
	changes := make([]egg.Replace, len(f.Changes))
	for i, c := range f.Changes {
		changes[i] = egg.Replace{Key: c.Key, Value: c.Value, IfValue: c.IfValue, Add: c.Add}
	}
	out, skipped, err := egg.ApplyFile(f.Parser, old, changes, nil)
	if err != nil {
		return nil, err
	}
	// A file that a game makes itself, and that has nothing to change yet,
	// is left for it to make.
	if bytes.Equal(out, old) || (!existed && len(out) == 0) {
		return skipped, nil
	}
	if len(out) > egg.MaxConfigFile {
		return nil, fmt.Errorf("the result is larger than %d MiB", egg.MaxConfigFile>>20)
	}
	return skipped, writeConfigFile(root, name, out, mode)
}

func readConfigFile(root *os.Root, name string) (content []byte, mode fs.FileMode, existed bool, err error) {
	// Not blocking, so a named pipe in its place does not hold us up.
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, 0o644, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, 0, false, errors.New("it is not a regular file")
	}
	content, err = io.ReadAll(io.LimitReader(f, egg.MaxConfigFile+1))
	if err != nil {
		return nil, 0, false, err
	}
	if len(content) > egg.MaxConfigFile {
		return nil, 0, false, fmt.Errorf("it is larger than %d MiB", egg.MaxConfigFile>>20)
	}
	return content, st.Mode().Perm(), true, nil
}

// writeConfigFile replaces the file in one step, so a crash leaves the old
// one or the new one.
func writeConfigFile(root *os.Root, name string, content []byte, mode fs.FileMode) error {
	dir := path.Dir(name)
	if dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path.Join(dir, ".zelie-"+rand.Text()+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = f.Write(content)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, name)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}

// chownAll gives every file of the volume to uid:gid. Symbolic links are
// changed themselves and never followed, and the root keeps the walk inside
// the volume, so nothing outside it is touched.
func chownAll(root *os.Root, uid, gid int) error {
	return fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		// Most files of a server that ran before already belong to it.
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) == uid && int(st.Gid) == gid {
			return nil
		}
		return root.Lchown(p, uid, gid)
	})
}

type stdinRequest struct {
	Data string `json:"data"`
}

// stdin writes to the console of a game server's container.
func (s *Server) stdin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	var req stdinRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Data == "" || len(req.Data) > maxStdinBytes {
		writeError(w, http.StatusBadRequest, fmt.Errorf("input must be between 1 and %d bytes", maxStdinBytes))
		return
	}
	err := s.Engine.WriteStdin(r.Context(), id, []byte(req.Data))
	switch {
	case errors.Is(err, engine.ErrInputClosed):
		writeError(w, http.StatusConflict, errInputClosed.Err())
	case err != nil:
		s.fail(w, "write input", id, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

type signalRequest struct {
	Signal string `json:"signal"`
}

// signal sends one of a short list of signals to a container.
func (s *Server) signal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	var req signalRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	sig, ok := engine.ParseSignal(req.Signal)
	if !ok {
		writeError(w, http.StatusBadRequest, fmt.Errorf("signal %.32q is not one of the signals that can be sent", req.Signal))
		return
	}
	if err := s.Engine.Signal(r.Context(), id, sig); err != nil {
		s.fail(w, "signal", id, err)
		return
	}
	s.Log.Info("signal sent", "id", id, "signal", req.Signal)
	w.WriteHeader(http.StatusNoContent)
}

// PrepareVolume edits a game server's config files and gives its files to
// the user the server runs as. The volume must not be in use.
func (c *Client) PrepareVolume(ctx context.Context, name string, req PrepareRequest) (PrepareResponse, error) {
	var out PrepareResponse
	err := c.do(ctx, http.MethodPost, "/v1/volumes/"+url.PathEscape(name)+"/prepare", req, &out)
	return out, err
}

// WriteStdin writes to a container's console. A container that is not
// reading it gives a *Error with status 409.
func (c *Client) WriteStdin(ctx context.Context, id string, data []byte) error {
	return c.do(ctx, http.MethodPost, "/v1/containers/"+url.PathEscape(id)+"/stdin", stdinRequest{Data: string(data)}, nil)
}

// Signal sends a signal such as SIGINT to a container's main process.
func (c *Client) Signal(ctx context.Context, id, signal string) error {
	return c.do(ctx, http.MethodPost, "/v1/containers/"+url.PathEscape(id)+"/signal", signalRequest{Signal: signal}, nil)
}
