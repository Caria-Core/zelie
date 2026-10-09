package core

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
)

// The file manager of a game server. Everything happens in the core, through
// the volume's os.Root: the volume holds what the game and its players made,
// so any path in it, a symbolic link included, may point anywhere, and the
// root is what keeps each step inside. The panel only sends typed requests.
// The volume may be in use; files are changed the way a person at a shell
// would, by replacing them whole. Only a truncate works on a file in place.

const (
	// MaxEditBytes is the largest file the editor opens or saves.
	MaxEditBytes = 4 << 20

	maxUploadBytes  = 1 << 30
	maxListed       = 10000
	maxPathBytes    = 4096
	maxFilePaths    = 1000
	sniffBytes      = 8 << 10
	maxExtractBytes = 8 << 30
	maxLinkBytes    = 4096
	noLimit         = math.MaxInt64
)

var (
	errFilePath     = msg.Define(http.StatusBadRequest, "files.bad_path", "That path is not inside the server's files.")
	errFileGone     = msg.Define(http.StatusNotFound, "files.not_found", "There is no {path} in the server's files.")
	errFileExists   = msg.Define(http.StatusConflict, "files.exists", "{path} already exists.")
	errFileNotDir   = msg.Define(http.StatusConflict, "files.not_folder", "{path} is not a folder.")
	errFileNotFile  = msg.Define(http.StatusConflict, "files.not_file", "{path} is not a regular file.")
	errFileRoot     = msg.Define(http.StatusBadRequest, "files.root", "The server's top folder cannot be moved, renamed or deleted.")
	errFileInside   = msg.Define(http.StatusBadRequest, "files.move_inside", "A folder cannot be moved into itself.")
	errFileName     = msg.Define(http.StatusBadRequest, "files.bad_name", "That is not a name a file can have.")
	errFileMany     = msg.Define(http.StatusBadRequest, "files.too_many", "Too many items were chosen at once.")
	errFileBinary   = msg.Define(http.StatusUnprocessableEntity, "files.binary", "{path} is not a text file, so it cannot be edited here. Download it instead.")
	errFileBig      = msg.Define(http.StatusRequestEntityTooLarge, "files.too_large", "The file is larger than {limit}.")
	errFileFull     = msg.Define(http.StatusConflict, "files.volume_full", "The server's disk limit leaves {room} free, which is not enough.")
	errFileNoRoom   = msg.Define(http.StatusUnprocessableEntity, "files.no_room", "Not enough disk space on the machine: {free} is free, and Zelie keeps 1 GB free for the apps.")
	errFileChanged  = msg.Define(http.StatusConflict, "files.changed", "{path} changed while it was being read. Try again.")
	errFileTooDeep  = msg.Define(http.StatusConflict, "files.too_deep", "Folders in here are nested too deep to go through. Remove the deepest ones first.")
	errFilesBusy    = msg.Define(http.StatusConflict, "files.busy", "Another archive is being made or unpacked for this server.")
	errNotArchive   = msg.Define(http.StatusBadRequest, "files.not_archive", "{path} is not a .zip, .tar, .tar.gz or .tgz file.")
	errArchiveBad   = msg.Define(http.StatusUnprocessableEntity, "files.archive_bad", "The archive could not be read: {detail}")
	errArchivePath  = msg.Define(http.StatusUnprocessableEntity, "files.archive_escape", "The archive was stopped at {entry}, which would go outside the folder it is unpacked into.")
	errArchiveLink  = msg.Define(http.StatusUnprocessableEntity, "files.archive_link", "The archive was stopped at {entry}, a link that points outside the folder it is unpacked into.")
	errArchiveOther = msg.Define(http.StatusUnprocessableEntity, "files.archive_special", "The archive was stopped at {entry}, which is a device or another kind of file that is not allowed.")
	errArchiveBytes = msg.Define(http.StatusUnprocessableEntity, "files.archive_too_large", "The archive was stopped: it holds more than {limit}.")
	errArchiveCount = msg.Define(http.StatusUnprocessableEntity, "files.archive_too_many", "The archive was stopped: it holds more than {limit} entries.")
)

// maxArchiveEntries is how many entries an archive may hold or make. A
// variable so a test does not need a hundred thousand of them.
var maxArchiveEntries = 100000

// errOverLimit is what a write that would pass its limit stops with.
var errOverLimit = errors.New("over the limit")

// errBadRequest marks a request that was not put together right, as opposed
// to one that asks for something that cannot be done.
var errBadRequest = errors.New("invalid request")

// FileOwner is who the files belong to as the container sees it. The volume
// keeps container IDs on disk, so it is also what the files carry.
type FileOwner struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
}

func (o FileOwner) check() error {
	if !(engine.IDs{UID: o.UID, GID: o.GID}).Valid() {
		return fmt.Errorf("%w: owner %d:%d is outside the container's IDs", errBadRequest, o.UID, o.GID)
	}
	return nil
}

// FileRef says which volume a file request is for and how to treat it.
type FileRef struct {
	Volume string
	FileOwner
	// Room is how many more bytes the volume may take under its limit; nil
	// when that is not known.
	Room *int64
}

// FileEntry is one item of a folder.
type FileEntry struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Mode     string    `json:"mode"`
	Modified time.Time `json:"modified"`
	Dir      bool      `json:"dir"`
	Symlink  bool      `json:"symlink"`
	// Target is where a link points, as it is written. It is not followed.
	Target string `json:"target,omitempty"`
}

// FileList is a folder's content, folders first.
type FileList struct {
	Path      string      `json:"path"`
	Entries   []FileEntry `json:"entries"`
	Truncated bool        `json:"truncated,omitempty"`
}

// ExtractResult says what an unpacked archive made.
type ExtractResult struct {
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

// filePath turns a path from the panel into one below the volume's top. A
// path is always relative to the volume, so /etc/passwd is the volume's own
// etc/passwd. ".." is refused instead of being resolved: nothing that asks for
// it means well.
func filePath(p string) (string, error) {
	if len(p) > maxPathBytes || strings.ContainsRune(p, 0) {
		return "", errFilePath.Err()
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return "", errFilePath.Err()
		}
	}
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" {
		return ".", nil
	}
	return c, nil
}

// escapes reports whether a cleaned relative path leaves the folder it is
// relative to.
func escapes(p string) bool { return p == ".." || strings.HasPrefix(p, "../") }

func fileName(n string) bool {
	return n != "" && n != "." && n != ".." && len(n) <= 255 && !strings.ContainsAny(n, "/\x00")
}

func label(p string) string {
	if p == "." {
		return "/"
	}
	if len(p) > 200 {
		return p[:200] + "…"
	}
	return p
}

// fileFailed answers a failed file request. A message written for the person
// goes as it is; an error of the file system becomes the one that fits.
func (s *Server) fileFailed(w http.ResponseWriter, op, volume, name string, err error) {
	var me *msg.Error
	switch {
	case errors.As(err, &me):
		writeError(w, me.Status, me)
	case errors.Is(err, errBadRequest):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, errFileGone.Err("path", label(name)))
	case errors.Is(err, fs.ErrExist), errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EISDIR):
		writeError(w, http.StatusConflict, errFileExists.Err("path", label(name)))
	case errors.Is(err, syscall.ENOTDIR):
		writeError(w, http.StatusConflict, errFileNotDir.Err("path", label(name)))
	case errors.Is(err, syscall.ELOOP), strings.Contains(err.Error(), "path escapes from parent"):
		writeError(w, http.StatusBadRequest, errFilePath.Err())
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		writeError(w, http.StatusConflict, errFileFull.Err("room", "0 KB"))
	case errors.Is(err, errOverLimit):
		writeError(w, http.StatusRequestEntityTooLarge, errFileBig.Err("limit", sizeLabel(maxUploadBytes)))
	case errors.Is(err, engine.ErrTooDeep):
		s.Log.Warn(op+" failed", "volume", volume, "path", name, "err", err)
		writeError(w, http.StatusConflict, errFileTooDeep.Err())
	default:
		s.Log.Error(op+" failed", "volume", volume, "path", name, "err", err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("%s failed, see the core log", op))
	}
}

// filesRoot opens the volume in the request. It does not mind a running
// container: a file manager is used while the game runs.
func (s *Server) filesRoot(w http.ResponseWriter, name, op string) (*os.Root, bool) {
	if !engine.ValidID(name) {
		writeError(w, http.StatusBadRequest, errors.New("invalid volume name"))
		return nil, false
	}
	root, err := s.Engine.ReadVolume(name)
	if err != nil {
		s.volumeFailed(w, op, name, err)
		return nil, false
	}
	return root, true
}

// fileQuery reads what the requests without a JSON body carry in the query.
func fileQuery(r *http.Request, needOwner bool) (p string, owner FileOwner, room *int64, err error) {
	q := r.URL.Query()
	if p, err = filePath(q.Get("path")); err != nil {
		return
	}
	if needOwner || q.Has("uid") || q.Has("gid") {
		uid, e1 := strconv.ParseUint(q.Get("uid"), 10, 32)
		gid, e2 := strconv.ParseUint(q.Get("gid"), 10, 32)
		if e1 != nil || e2 != nil {
			return "", owner, nil, fmt.Errorf("%w: uid and gid must be numbers", errBadRequest)
		}
		owner = FileOwner{UID: uint32(uid), GID: uint32(gid)}
		if err = owner.check(); err != nil {
			return
		}
	}
	if v := q.Get("room"); v != "" {
		n, e := strconv.ParseInt(v, 10, 64)
		if e != nil || n < 0 {
			return "", owner, nil, fmt.Errorf("%w: room must be a number of bytes", errBadRequest)
		}
		room = &n
	}
	return
}

// ceiling is the most a write may take, and what to say when it is passed.
type ceiling struct {
	bytes int64
	over  *msg.Error
}

// limitFor works out how much a write may take: the fixed limit, if there is
// one, the room the volume's limit leaves, and what the machine's disk has
// free above the reserve, whichever is smallest.
func limitFor(root *os.Root, room *int64, fixed int64, fixedOver *msg.Error) (ceiling, error) {
	c := ceiling{bytes: noLimit, over: fixedOver}
	if fixed > 0 {
		c.bytes = fixed
	}
	if room != nil && *room < c.bytes {
		n := max(*room, 0)
		c = ceiling{n, errFileFull.Err("room", sizeLabel(n))}
	}
	free, err := freeIn(root)
	if err != nil {
		return c, err
	}
	if free-diskReserve < c.bytes {
		c = ceiling{max(free-diskReserve, 0), errFileNoRoom.Err("free", sizeLabel(free))}
	}
	return c, nil
}

// onDisk is the room a file gives back when it goes: its size, or the blocks
// it holds when it has holes, which is what the volume's limit counts.
func onDisk(st fs.FileInfo) int64 {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return min(st.Size(), int64(sys.Blocks)*512)
	}
	return st.Size()
}

func freeIn(root *os.Root) (int64, error) {
	f, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// openRegular opens a file to read it. Not blocking, so a named pipe in its
// place does not hold the request up.
func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, nil, errFileNotFile.Err("path", label(name))
	}
	return f, st, nil
}

type putOptions struct {
	Owner FileOwner
	// Exclusive refuses a name that is taken.
	Exclusive bool
	// Mode is the new file's permissions. Zero keeps those of the file it
	// replaces, or 0644 for a new one.
	Mode fs.FileMode
	// Limit is the most bytes to take. Reaching past it fails with
	// errOverLimit and leaves nothing behind.
	Limit int64
}

// replaceFile writes r to name through a temporary file beside it, so a
// reader, or a game that is loading it, sees the old file or the whole new
// one. A link at name is replaced, not followed.
func replaceFile(root *os.Root, name string, r io.Reader, o putOptions) (int64, error) {
	mode := o.Mode
	switch st, err := root.Lstat(name); {
	case err == nil:
		if o.Exclusive {
			return 0, &fs.PathError{Op: "create", Path: name, Err: fs.ErrExist}
		}
		if st.IsDir() {
			return 0, &fs.PathError{Op: "write", Path: name, Err: syscall.EISDIR}
		}
		if mode == 0 && st.Mode().IsRegular() {
			mode = st.Mode().Perm()
		}
	case !errors.Is(err, fs.ErrNotExist):
		return 0, err
	}
	if mode == 0 {
		mode = 0o644
	}
	tmp := path.Join(path.Dir(name), ".zelie-"+rand.Text()+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	src := r
	if o.Limit < noLimit {
		src = io.LimitReader(r, o.Limit+1)
	}
	n, err := io.Copy(f, src)
	if err == nil && n > o.Limit {
		err = errOverLimit
	}
	if err == nil {
		err = f.Chown(int(o.Owner.UID), int(o.Owner.GID))
	}
	if err == nil {
		err = f.Chmod(mode)
	}
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
		return n, err
	}
	return n, nil
}

// makeDirs makes dir and the folders above it that are missing, each given to
// the owner. A folder that is there already is left as it is.
func makeDirs(root *os.Root, dir string, o FileOwner, made map[string]bool) error {
	if dir == "." {
		return nil
	}
	cur := ""
	for _, part := range strings.Split(dir, "/") {
		cur = path.Join(cur, part)
		if made[cur] {
			continue
		}
		st, err := root.Stat(cur)
		switch {
		case err == nil:
			if !st.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: cur, Err: syscall.ENOTDIR}
			}
		case !errors.Is(err, fs.ErrNotExist):
			return err
		default:
			if err := root.Mkdir(cur, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err := root.Lchown(cur, int(o.UID), int(o.GID)); err != nil {
				return err
			}
		}
		if made != nil {
			made[cur] = true
		}
	}
	return nil
}

type listRequest struct {
	Path string `json:"path"`
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req listRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dir, err := filePath(req.Path)
	if err != nil {
		s.fileFailed(w, "list files", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "list files")
	if !ok {
		return
	}
	defer root.Close()
	list, err := listFolder(root, dir)
	if err != nil {
		s.fileFailed(w, "list files", name, dir, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// openFolder opens a folder of the volume to read it.
func openFolder(root *os.Root, dir string) (*os.File, error) {
	// O_DIRECTORY makes a named pipe fail at once. Opening one waits for a
	// writer, which takes a thread of the core with it for good.
	return root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
}

// entryOf describes an item that was read from the folder dir. It is false
// for one that is gone since.
func entryOf(root *os.Root, dir string, e fs.DirEntry) (FileEntry, bool) {
	info, err := e.Info()
	if err != nil {
		return FileEntry{}, false
	}
	fe := FileEntry{
		Name: e.Name(), Size: info.Size(), Mode: info.Mode().String(), Modified: info.ModTime().UTC(),
		Dir: info.IsDir(), Symlink: info.Mode()&fs.ModeSymlink != 0,
	}
	if fe.Symlink {
		fe.Target, _ = root.Readlink(path.Join(dir, e.Name()))
	}
	return fe, true
}

func listFolder(root *os.Root, dir string) (FileList, error) {
	f, err := openFolder(root, dir)
	if err != nil {
		return FileList{}, err
	}
	defer f.Close()
	entries, err := f.ReadDir(maxListed + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return FileList{}, err
	}
	out := FileList{Path: dir, Entries: make([]FileEntry, 0, len(entries))}
	if len(entries) > maxListed {
		entries, out.Truncated = entries[:maxListed], true
	}
	for _, e := range entries {
		if fe, ok := entryOf(root, dir, e); ok {
			out.Entries = append(out.Entries, fe)
		}
	}
	slices.SortFunc(out.Entries, func(a, b FileEntry) int {
		if a.Dir != b.Dir {
			if a.Dir {
				return -1
			}
			return 1
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out, nil
}

// readFile sends a text file for the editor.
func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, _, _, err := fileQuery(r, false)
	if err != nil {
		s.fileFailed(w, "read file", name, r.URL.Query().Get("path"), err)
		return
	}
	root, ok := s.filesRoot(w, name, "read file")
	if !ok {
		return
	}
	defer root.Close()
	f, st, err := openRegular(root, p)
	if err != nil {
		s.fileFailed(w, "read file", name, p, err)
		return
	}
	defer f.Close()
	if st.Size() > MaxEditBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errFileBig.Err("limit", sizeLabel(MaxEditBytes)))
		return
	}
	body, err := io.ReadAll(io.LimitReader(f, MaxEditBytes+1))
	if err != nil {
		s.fileFailed(w, "read file", name, p, err)
		return
	}
	if len(body) > MaxEditBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errFileBig.Err("limit", sizeLabel(MaxEditBytes)))
		return
	}
	// Text is what the editor can save back without changing it.
	if slices.Contains(body[:min(len(body), sniffBytes)], 0) || !utf8.Valid(body) {
		writeError(w, http.StatusUnprocessableEntity, errFileBinary.Err("path", label(p)))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

// writeFile saves the editor's text, or makes a new file with it.
func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, owner, room, err := fileQuery(r, true)
	if err != nil {
		s.fileFailed(w, "write file", name, r.URL.Query().Get("path"), err)
		return
	}
	if p == "." {
		writeError(w, http.StatusBadRequest, errFileNotFile.Err("path", "/"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxEditBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, errFileBig.Err("limit", sizeLabel(MaxEditBytes)))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	root, ok := s.filesRoot(w, name, "write file")
	if !ok {
		return
	}
	defer root.Close()
	// Saving over a file gives its room back, so a server at its limit can
	// still have a file edited.
	if st, lerr := root.Lstat(p); room != nil && lerr == nil && st.Mode().IsRegular() {
		more := satAdd(*room, onDisk(st))
		room = &more
	}
	c, err := limitFor(root, room, 0, nil)
	if err == nil {
		_, err = replaceFile(root, p, bytes.NewReader(body), putOptions{Owner: owner, Exclusive: r.URL.Query().Get("new") == "1", Limit: c.bytes})
	}
	if errors.Is(err, errOverLimit) {
		err = c.over
	}
	if err != nil {
		s.fileFailed(w, "write file", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type mkdirRequest struct {
	FileOwner
	Path string `json:"path"`
}

func (s *Server) makeFolder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req mkdirRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err == nil {
		err = req.check()
	}
	if err == nil && p == "." {
		err = errFileExists.Err("path", "/")
	}
	if err != nil {
		s.fileFailed(w, "make folder", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "make folder")
	if !ok {
		return
	}
	defer root.Close()
	if err := root.Mkdir(p, 0o755); err != nil {
		s.fileFailed(w, "make folder", name, p, err)
		return
	}
	if err := root.Lchown(p, int(req.UID), int(req.GID)); err != nil {
		root.Remove(p)
		s.fileFailed(w, "make folder", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type renameRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// renameFile moves a file or folder within the volume. It never replaces
// what is at the new path.
func (s *Server) renameFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req renameRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	from, err := filePath(req.From)
	var to string
	if err == nil {
		to, err = filePath(req.To)
	}
	switch {
	case err != nil:
	case from == "." || to == ".":
		err = errFileRoot.Err()
	case from == to:
		err = errFileExists.Err("path", label(to))
	case strings.HasPrefix(to, from+"/"):
		err = errFileInside.Err()
	}
	if err != nil {
		s.fileFailed(w, "rename file", name, req.From, err)
		return
	}
	root, ok := s.filesRoot(w, name, "rename file")
	if !ok {
		return
	}
	defer root.Close()
	if _, err := root.Lstat(from); err != nil {
		s.fileFailed(w, "rename file", name, from, err)
		return
	}
	if _, err := root.Lstat(to); err == nil {
		s.fileFailed(w, "rename file", name, to, fs.ErrExist)
		return
	} else if !errors.Is(err, fs.ErrNotExist) {
		s.fileFailed(w, "rename file", name, to, err)
		return
	}
	if err := root.Rename(from, to); err != nil {
		s.fileFailed(w, "rename file", name, to, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deleteRequest struct {
	Paths []string `json:"paths"`
}

// deleteFiles removes files and folders with what is in them. A link is
// removed itself; what it points at is not touched.
func (s *Server) deleteFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req deleteRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Paths) == 0 || len(req.Paths) > maxFilePaths {
		writeError(w, http.StatusBadRequest, errFileMany.Err())
		return
	}
	paths := make([]string, len(req.Paths))
	for i, raw := range req.Paths {
		p, err := filePath(raw)
		if err == nil && p == "." {
			err = errFileRoot.Err()
		}
		if err != nil {
			s.fileFailed(w, "delete files", name, raw, err)
			return
		}
		paths[i] = p
	}
	root, ok := s.filesRoot(w, name, "delete files")
	if !ok {
		return
	}
	defer root.Close()
	for _, p := range paths {
		if err := root.RemoveAll(p); err != nil {
			s.fileFailed(w, "delete files", name, p, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadFile takes a file streamed in the request body.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, owner, room, err := fileQuery(r, true)
	if err == nil && p == "." {
		err = errFileNotFile.Err("path", "/")
	}
	if err != nil {
		s.fileFailed(w, "upload file", name, r.URL.Query().Get("path"), err)
		return
	}
	root, ok := s.filesRoot(w, name, "upload file")
	if !ok {
		return
	}
	defer root.Close()
	c, err := limitFor(root, room, maxUploadBytes, errFileBig.Err("limit", sizeLabel(maxUploadBytes)))
	if err == nil && r.ContentLength > c.bytes {
		err = c.over
	}
	if err == nil {
		_, err = replaceFile(root, p, contextReader{r.Context(), r.Body}, putOptions{Owner: owner, Limit: c.bytes})
		if errors.Is(err, errOverLimit) {
			err = c.over
		}
	}
	if err != nil {
		s.fileFailed(w, "upload file", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// downloadFile streams a file as it is.
func (s *Server) downloadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, _, _, err := fileQuery(r, false)
	if err != nil {
		s.fileFailed(w, "download file", name, r.URL.Query().Get("path"), err)
		return
	}
	root, ok := s.filesRoot(w, name, "download file")
	if !ok {
		return
	}
	defer root.Close()
	f, st, err := openRegular(root, p)
	if err != nil {
		s.fileFailed(w, "download file", name, p, err)
		return
	}
	defer f.Close()
	// offset and length ask for part of the file, as SFTP reads do. Past
	// the end there is nothing to send, not an error.
	offset, length := int64(0), int64(-1)
	q := r.URL.Query()
	for _, part := range []struct {
		key string
		to  *int64
	}{{"offset", &offset}, {"length", &length}} {
		if !q.Has(part.key) {
			continue
		}
		n, err := strconv.ParseInt(q.Get(part.key), 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("%w: %s must be a number of bytes", errBadRequest, part.key))
			return
		}
		*part.to = n
	}
	size := st.Size()
	offset = min(offset, size)
	n := size - offset
	if length >= 0 {
		n = min(n, length)
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			s.fileFailed(w, "download file", name, p, err)
			return
		}
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
	// A file that grows while it is sent is cut at the size announced.
	io.Copy(w, io.LimitReader(f, n))
}

// statFile describes one item. A link is described, not followed.
func (s *Server) statFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req listRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err != nil {
		s.fileFailed(w, "stat file", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "stat file")
	if !ok {
		return
	}
	defer root.Close()
	entry, err := statEntry(root, p)
	if err != nil {
		s.fileFailed(w, "stat file", name, p, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func statEntry(root *os.Root, p string) (FileEntry, error) {
	info, err := root.Lstat(p)
	if err != nil {
		return FileEntry{}, err
	}
	fe := FileEntry{
		Name: path.Base(p), Size: info.Size(), Mode: info.Mode().String(), Modified: info.ModTime().UTC(),
		Dir: info.IsDir(), Symlink: info.Mode()&fs.ModeSymlink != 0,
	}
	if p == "." {
		fe.Name = "/"
	}
	if fe.Symlink {
		fe.Target, _ = root.Readlink(p)
	}
	return fe, nil
}

// removeFile removes one file, link or empty folder. Unlike deleteFiles it
// never goes into a folder.
func (s *Server) removeFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req listRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err == nil && p == "." {
		err = errFileRoot.Err()
	}
	if err != nil {
		s.fileFailed(w, "remove file", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "remove file")
	if !ok {
		return
	}
	defer root.Close()
	if err := root.Remove(p); err != nil {
		s.fileFailed(w, "remove file", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type compressRequest struct {
	FileOwner
	// Dir holds the chosen items; the archive is made there.
	Dir   string   `json:"dir"`
	Paths []string `json:"paths"`
	// Name is the archive's file name; one is made up when it is empty.
	Name string `json:"name,omitempty"`
	Room *int64 `json:"room,omitempty"`
}

type compressResponse struct {
	Name string `json:"name"`
}

// compressFiles packs the chosen items of a folder into a .tar.gz beside
// them. Links go in as links and files that are not files or folders are
// left out.
func (s *Server) compressFiles(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req compressRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dir, err := filePath(req.Dir)
	if err == nil {
		err = req.check()
	}
	if err == nil && (len(req.Paths) == 0 || len(req.Paths) > maxFilePaths) {
		err = errFileMany.Err()
	}
	var items []string
	for _, raw := range req.Paths {
		if err != nil {
			break
		}
		var p string
		if p, err = filePath(raw); err == nil && (p == "." || path.Dir(p) != dir) {
			err = errFilePath.Err()
		}
		if !slices.Contains(items, p) {
			items = append(items, p)
		}
	}
	archive := req.Name
	if err == nil {
		if archive == "" {
			archive = "archive-" + time.Now().UTC().Format("20060102-150405")
		}
		archive = strings.TrimSuffix(archive, ".tar.gz") + ".tar.gz"
		if !fileName(archive) {
			err = errFileName.Err()
		}
	}
	if err != nil {
		s.fileFailed(w, "compress files", name, req.Dir, err)
		return
	}
	if !s.busy.take("files " + name) {
		writeError(w, http.StatusConflict, errFilesBusy.Err())
		return
	}
	defer s.busy.done("files " + name)
	root, ok := s.filesRoot(w, name, "compress files")
	if !ok {
		return
	}
	defer root.Close()
	c, err := limitFor(root, req.Room, 0, nil)
	if err == nil {
		err = packFiles(r.Context(), root, dir, items, path.Join(dir, archive), req.FileOwner, c.bytes)
	}
	if errors.Is(err, errOverLimit) {
		err = c.over
	}
	if err != nil {
		s.fileFailed(w, "compress files", name, dir, err)
		return
	}
	writeJSON(w, http.StatusOK, compressResponse{Name: archive})
}

type capWriter struct {
	w    io.Writer
	left int64
}

func (c *capWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > c.left {
		return 0, errOverLimit
	}
	c.left -= int64(len(p))
	return c.w.Write(p)
}

// packFiles writes the items to out as a gzipped tar. Names in it are
// relative to dir. It is made under another name and renamed when whole.
func packFiles(ctx context.Context, root *os.Root, dir string, items []string, out string, o FileOwner, limit int64) error {
	if _, err := root.Lstat(out); err == nil {
		return &fs.PathError{Op: "create", Path: out, Err: fs.ErrExist}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp := path.Join(dir, ".zelie-"+rand.Text()+".tmp")
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(&capWriter{f, limit})
	tw := backup.NewTarWriter(gz)
	entries := 0
	err = func() error {
		for _, item := range items {
			li, err := root.Lstat(item)
			if err != nil {
				return err
			}
			// A link chosen by itself goes in as a link; the walk would
			// follow it.
			if !li.IsDir() {
				if entries++; entries > maxArchiveEntries {
					return errArchiveCount.Err("limit", maxArchiveEntries)
				}
				if err := packEntry(ctx, rootEntry{root, item}, tw, dir, item, li); err != nil {
					return err
				}
				continue
			}
			err = engine.WalkTree(root, item, func(n *engine.TreeNode) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				p := path.Join(item, n.Path())
				if p == tmp {
					return nil
				}
				if entries++; entries > maxArchiveEntries {
					return errArchiveCount.Err("limit", maxArchiveEntries)
				}
				return packEntry(ctx, n, tw, dir, p, n.Info())
			})
			if err != nil {
				return err
			}
		}
		if err := tw.Close(); err != nil {
			return err
		}
		return gz.Close()
	}()
	if err == nil {
		err = f.Chown(int(o.UID), int(o.GID))
	}
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if _, lerr := root.Lstat(out); lerr == nil {
			err = &fs.PathError{Op: "create", Path: out, Err: fs.ErrExist}
		}
	}
	if err == nil {
		err = root.Rename(tmp, out)
	}
	if err != nil {
		root.Remove(tmp)
	}
	return err
}

// entrySource is where packEntry reads an item from. A walk reaches an item
// from the folder that holds it, which costs the same however deep it is; one
// named in the request is found from the top of the volume.
type entrySource interface {
	Readlink() (string, error)
	Open() (*os.File, error)
}

type rootEntry struct {
	root *os.Root
	p    string
}

func (e rootEntry) Readlink() (string, error) { return e.root.Readlink(e.p) }

// Open does not wait for a writer, so a named pipe in the place of a file
// does not hold the request up.
func (e rootEntry) Open() (*os.File, error) {
	return e.root.OpenFile(e.p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func packEntry(ctx context.Context, src entrySource, tw *backup.TarWriter, dir, p string, info fs.FileInfo) error {
	var link string
	var err error
	switch m := info.Mode().Type(); {
	case m == fs.ModeSymlink:
		if link, err = src.Readlink(); err != nil {
			return err
		}
	case m != 0 && m != fs.ModeDir:
		// Sockets, pipes and devices are not something to keep.
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = p
	if dir != "." {
		hdr.Name = strings.TrimPrefix(p, dir+"/")
	}
	if info.IsDir() {
		hdr.Name += "/"
	}
	// Who owned it here means nothing where it is unpacked.
	hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
	if !info.Mode().IsRegular() {
		return tw.WriteHeader(hdr)
	}
	f, err := src.Open()
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errFileNotFile.Err("path", label(p))
	}
	if st.Size() != hdr.Size {
		return errFileChanged.Err("path", label(p))
	}
	// A file with holes goes in as its data, so packing a sparse file takes
	// what it takes on disk, and the copy stops when the request does.
	if _, changed, err := tw.WriteFile(ctx, hdr, f); err != nil || changed {
		if err == nil {
			err = errFileChanged.Err("path", label(p))
		}
		return err
	}
	return nil
}

type extractRequest struct {
	FileOwner
	Path string `json:"path"`
	// Dir is where the archive is unpacked; its own folder when empty.
	Dir  string `json:"dir,omitempty"`
	Room *int64 `json:"room,omitempty"`
}

// extractFile unpacks a .zip, .tar, .tar.gz or .tgz into a folder of the
// volume. It is stopped at the first entry that would go outside that
// folder or is not allowed, and at a limit on what an archive may hold; what
// was unpacked until then stays.
func (s *Server) extractFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req extractRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err == nil {
		err = req.check()
	}
	dir := path.Dir(p)
	if err == nil && req.Dir != "" {
		dir, err = filePath(req.Dir)
	}
	format := archiveFormat(p)
	if err == nil && format == "" {
		err = errNotArchive.Err("path", label(p))
	}
	if err != nil {
		s.fileFailed(w, "extract file", name, req.Path, err)
		return
	}
	if !s.busy.take("files " + name) {
		writeError(w, http.StatusConflict, errFilesBusy.Err())
		return
	}
	defer s.busy.done("files " + name)
	root, ok := s.filesRoot(w, name, "extract file")
	if !ok {
		return
	}
	defer root.Close()
	c, err := limitFor(root, req.Room, maxExtractBytes, errArchiveBytes.Err("limit", sizeLabel(maxExtractBytes)))
	var res ExtractResult
	if err == nil {
		res, err = unpack(r.Context(), root, p, format, dir, req.FileOwner, c)
	}
	if err != nil {
		s.fileFailed(w, "extract file", name, p, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func archiveFormat(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".zip"):
		return "zip"
	case strings.HasSuffix(n, ".tar.gz"), strings.HasSuffix(n, ".tgz"):
		return "tgz"
	case strings.HasSuffix(n, ".tar"):
		return "tar"
	}
	return ""
}

type entryKind int

const (
	kindFile entryKind = iota
	kindDir
	kindSymlink
	kindHardlink
	kindOther
)

// extractor puts an archive's entries into a folder, checking each one.
type extractor struct {
	ctx   context.Context
	root  *os.Root
	dir   string
	owner FileOwner
	left  int64 // bytes it may still write
	limit ceiling
	made  map[string]bool
	// links are the symbolic links this archive made, so nothing after them
	// is written through one.
	links   map[string]bool
	entries int
	written int64
	// headers counts every tar header read, the ones that make nothing too.
	headers int
}

func unpack(ctx context.Context, root *os.Root, archive, format, dir string, o FileOwner, limit ceiling) (ExtractResult, error) {
	if st, err := root.Stat(dir); err != nil {
		return ExtractResult{}, err
	} else if !st.IsDir() {
		return ExtractResult{}, errFileNotDir.Err("path", label(dir))
	}
	f, st, err := openRegular(root, archive)
	if err != nil {
		return ExtractResult{}, err
	}
	defer f.Close()
	x := &extractor{ctx: ctx, root: root, dir: dir, owner: o, left: limit.bytes, limit: limit, made: map[string]bool{}, links: map[string]bool{}}
	if format == "zip" {
		err = x.zip(f, st.Size())
	} else {
		err = x.tar(contextReader{ctx, f}, format == "tgz")
	}
	return ExtractResult{Entries: x.entries, Bytes: x.written}, err
}

func (x *extractor) tar(r io.Reader, gzipped bool) error {
	if gzipped {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return errArchiveBad.Err("detail", err.Error())
		}
		defer gz.Close()
		r = gz
	}
	// Entries that make nothing, such as a "./", are still read through, so
	// the reader gets what may be written plus room for the headers.
	r = &capReader{r: r, left: satAdd(x.left, int64(maxArchiveEntries)*tarHeaderBytes), over: x.limit.over}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, tar.ErrInsecurePath) {
			// The names are checked below, with the rest.
			err = nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return x.readFailed(err)
		}
		if x.headers++; x.headers > maxArchiveEntries {
			return errArchiveCount.Err("limit", maxArchiveEntries)
		}
		var kind entryKind
		switch hdr.Typeflag {
		case tar.TypeReg:
			kind = kindFile
		case tar.TypeDir:
			kind = kindDir
		case tar.TypeSymlink:
			kind = kindSymlink
		case tar.TypeLink:
			kind = kindHardlink
		case tar.TypeXGlobalHeader:
			continue
		default:
			kind = kindOther
		}
		if err := x.add(hdr.Name, kind, fs.FileMode(hdr.Mode), hdr.Size, hdr.Linkname, tr); err != nil {
			return x.readFailed(err)
		}
	}
}

func (x *extractor) zip(f *os.File, size int64) error {
	var me *msg.Error
	pinned, err := checkZipCount(f, size)
	if err != nil {
		if errors.As(err, &me) {
			return err
		}
		return errArchiveBad.Err("detail", err.Error())
	}
	dr := newDirReader(pinned)
	zr, err := zip.NewReader(dr, size)
	dr.done = true
	if errors.As(err, &me) {
		return err
	}
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		return errArchiveBad.Err("detail", err.Error())
	}
	if len(zr.File) > maxArchiveEntries {
		return errArchiveCount.Err("limit", maxArchiveEntries)
	}
	var declared uint64
	for _, zf := range zr.File {
		declared += zf.UncompressedSize64
		if declared > uint64(x.left) {
			return x.limit.over
		}
	}
	for _, zf := range zr.File {
		mode := zf.Mode()
		var kind entryKind
		switch {
		case mode.IsDir() || strings.HasSuffix(zf.Name, "/"):
			kind = kindDir
		case mode&fs.ModeSymlink != 0:
			kind = kindSymlink
		case mode.IsRegular():
			kind = kindFile
		default:
			kind = kindOther
		}
		var body io.ReadCloser
		var link string
		if kind == kindFile || kind == kindSymlink {
			if body, err = zf.Open(); err != nil {
				return errArchiveBad.Err("detail", err.Error())
			}
		}
		if kind == kindSymlink {
			b, err := io.ReadAll(io.LimitReader(body, maxLinkBytes+1))
			body.Close()
			if err != nil {
				return x.readFailed(err)
			}
			link, body = string(b), nil
			if len(b) > maxLinkBytes {
				link = "/"
			}
		}
		var r io.Reader
		if body != nil {
			r = contextReader{x.ctx, body}
		}
		err := x.add(zf.Name, kind, mode, int64(zf.UncompressedSize64), link, r)
		if body != nil {
			body.Close()
		}
		if err != nil {
			return x.readFailed(err)
		}
	}
	return nil
}

const (
	// tarHeaderBytes is what the headers of one entry may take in a tar
	// stream: its blocks, the padding after its content and a long name.
	tarHeaderBytes = 4096
	// zipRecordBytes is what a record of a zip's central directory may take
	// on average, and zipTailBytes what is read around it to find it. A
	// record is 46 bytes and a name, and Go holds some 200 bytes in memory for
	// each one it reads, so a bigger share here lets a hostile archive take
	// that much more memory before it is counted.
	zipRecordBytes = 256
	zipTailBytes   = 128 << 10
)

// capReader fails with over once more than left bytes have been read.
type capReader struct {
	r    io.Reader
	left int64
	over error
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, c.over
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// dirReader lets zip.NewReader read only as much of the archive as the most
// entries allowed can take. NewReader holds an entry in memory for every
// record of the central directory before the caller can count them, so
// without this an archive of empty records is many times its own size in
// memory.
type dirReader struct {
	r    io.ReaderAt
	left int64
	done bool
}

func newDirReader(r io.ReaderAt) *dirReader {
	return &dirReader{r: r, left: int64(maxArchiveEntries)*zipRecordBytes + zipTailBytes}
}

func (d *dirReader) ReadAt(p []byte, off int64) (int, error) {
	if !d.done {
		if d.left -= int64(len(p)); d.left < 0 {
			return 0, errArchiveCount.Err("limit", maxArchiveEntries)
		}
	}
	return d.r.ReadAt(p, off)
}

// The records at the end of a zip that say how many entries it has. The
// zip64 ones come in two parts: a locator, which sits right before the end
// record and names the place of the record that holds the count.
const (
	zipEndSig       = "PK\x05\x06"
	zipEndLen       = 22
	zip64LocSig     = "PK\x06\x07"
	zip64LocLen     = 20
	zip64EndSig     = "PK\x06\x06"
	zip64EndLen     = 56
	zipEndRecordsAt = 10
	zip64RecordsAt  = 32
)

// pinnedReader serves the bytes checkZipCount looked at from memory, and
// nothing past the size it was given. An archive in a live volume can be
// rewritten, or made longer, by the program that runs in it, so a second
// read of the same place could give another count than the one that was
// checked, and a read past the end could find a record that never was.
type pinnedReader struct {
	r     io.ReaderAt
	tail  pinnedPart
	parts []pinnedPart // the records, by offset
	reach int64        // the longest of them
}

type pinnedPart struct {
	off int64
	b   []byte
}

func (p pinnedPart) holds(off int64, n int) ([]byte, bool) {
	if off >= p.off && off+int64(n) <= p.off+int64(len(p.b)) {
		return p.b[off-p.off:][:n], true
	}
	return nil, false
}

// held is the n bytes at off when a pinned part has all of them.
func (p *pinnedReader) held(off int64, n int) ([]byte, bool) {
	if b, ok := p.tail.holds(off, n); ok {
		return b, true
	}
	// A record is short, so only those that start just before off can hold it.
	i := sort.Search(len(p.parts), func(i int) bool { return p.parts[i].off > off })
	for ; i > 0 && off-p.parts[i-1].off < p.reach; i-- {
		if b, ok := p.parts[i-1].holds(off, n); ok {
			return b, true
		}
	}
	return nil, false
}

func (p *pinnedReader) read(off int64, n int) ([]byte, error) {
	b := make([]byte, n)
	if got, err := p.r.ReadAt(b, off); got < n {
		if err == nil || errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}

// pinTail keeps the last n bytes of the file, which hold the end records.
func (p *pinnedReader) pinTail(size int64, n int) ([]byte, error) {
	b, err := p.read(size-int64(n), n)
	if err != nil {
		return nil, err
	}
	p.tail = pinnedPart{off: size - int64(n), b: b}
	return b, nil
}

// pin reads n bytes at off once and keeps them. It is for records, which are
// short; the places they are asked for are the archive's to choose, so a
// part is kept once however many times it is named.
func (p *pinnedReader) pin(off int64, n int) ([]byte, error) {
	if b, ok := p.held(off, n); ok {
		return b, nil
	}
	b, err := p.read(off, n)
	if err != nil {
		return nil, err
	}
	i := sort.Search(len(p.parts), func(i int) bool { return p.parts[i].off > off })
	p.parts = slices.Insert(p.parts, i, pinnedPart{off: off, b: b})
	p.reach = max(p.reach, int64(n))
	return b, nil
}

func (p *pinnedReader) ReadAt(b []byte, off int64) (int, error) {
	if held, ok := p.held(off, len(b)); ok {
		return copy(b, held), nil
	}
	return p.r.ReadAt(b, off)
}

// checkZipCount refuses a zip whose end records claim more entries than
// maxArchiveEntries. zip.NewReader makes room for that many entries before
// it reads one, so a claim with no entries behind it, in a file that is only
// sparse, would cost the memory anyway. dirReader cannot stop that: it only
// limits what is read afterwards.
//
// Every end record signature in the last bytes is looked at, not only the
// one zip.NewReader picks, so a difference in how it searches is of no use
// to an archive made for it. The cost is that a small zip is refused as well
// when it holds, stored as it is, a zip64 archive of more entries than the
// limit.
//
// The reader it returns is the one to give zip.NewReader: it answers from
// what was checked, so the count that is allocated for is the one that was
// below the limit, and it ends at size, so a record the file got after it
// was measured is not found.
func checkZipCount(r io.ReaderAt, size int64) (io.ReaderAt, error) {
	pr := &pinnedReader{r: io.NewSectionReader(r, 0, size)}
	n := int(min(size, zipTailBytes))
	tail, err := pr.pinTail(size, n)
	if err != nil {
		return nil, err
	}
	over := func(records uint64) error {
		if records > uint64(maxArchiveEntries) {
			return errArchiveCount.Err("limit", maxArchiveEntries)
		}
		return nil
	}
	for i := len(tail) - zipEndLen; i >= 0; i-- {
		if string(tail[i:i+4]) != zipEndSig {
			continue
		}
		if err := over(uint64(binary.LittleEndian.Uint16(tail[i+zipEndRecordsAt:]))); err != nil {
			return nil, err
		}
		at := size - int64(n) + int64(i)
		if at < zip64LocLen {
			continue
		}
		loc, err := pr.pin(at-zip64LocLen, zip64LocLen)
		if err != nil {
			return nil, err
		}
		if string(loc[:4]) != zip64LocSig {
			continue
		}
		off := binary.LittleEndian.Uint64(loc[8:])
		if size < zip64EndLen || off > uint64(size-zip64EndLen) {
			continue // the reader ends before such a record does
		}
		rec, err := pr.pin(int64(off), zip64EndLen)
		if err != nil {
			return nil, err
		}
		if string(rec[:4]) != zip64EndSig {
			continue
		}
		if err := over(binary.LittleEndian.Uint64(rec[zip64RecordsAt:])); err != nil {
			return nil, err
		}
	}
	return pr, nil
}

func satAdd(a, b int64) int64 {
	if a > noLimit-b {
		return noLimit
	}
	return a + b
}

// readFailed tells a broken archive from the errors that are ours to report.
func (x *extractor) readFailed(err error) error {
	var me *msg.Error
	var pe *fs.PathError
	switch {
	case errors.As(err, &me), errors.As(err, &pe), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.Is(err, errOverLimit):
		return err
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT):
		return err
	}
	return errArchiveBad.Err("detail", err.Error())
}

// archiveName is an entry's name below the folder it is unpacked into, or
// empty for the folder itself.
func archiveName(raw string) (string, error) {
	if raw == "" || len(raw) > maxPathBytes || strings.HasPrefix(raw, "/") || strings.ContainsRune(raw, 0) {
		return "", errArchivePath.Err("entry", label(raw))
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", errArchivePath.Err("entry", label(raw))
		}
	}
	c := path.Clean(raw)
	if c == "." {
		return "", nil
	}
	return c, nil
}

// add makes one entry. body is its content for a file.
func (x *extractor) add(raw string, kind entryKind, mode fs.FileMode, size int64, link string, body io.Reader) error {
	if err := x.ctx.Err(); err != nil {
		return err
	}
	name, err := archiveName(raw)
	if err != nil || name == "" {
		return err
	}
	if x.entries++; x.entries > maxArchiveEntries {
		return errArchiveCount.Err("limit", maxArchiveEntries)
	}
	for p := path.Dir(name); p != "."; p = path.Dir(p) {
		if x.links[p] {
			return errArchivePath.Err("entry", label(raw))
		}
	}
	full := path.Join(x.dir, name)
	parent := path.Dir(full)
	switch kind {
	case kindDir:
		return makeDirs(x.root, full, x.owner, x.made)
	case kindFile:
		if size > x.left {
			return x.limit.over
		}
		if err := makeDirs(x.root, parent, x.owner, x.made); err != nil {
			return err
		}
		return x.put(full, mode, body)
	case kindHardlink:
		// Made as a copy: the content is what matters, and a real link
		// would tie the new file to the old one's owner and mode.
		src, err := archiveName(link)
		if err != nil || src == "" {
			return errArchiveLink.Err("entry", label(raw))
		}
		for p := path.Dir(src); p != "."; p = path.Dir(p) {
			if x.links[p] {
				return errArchiveLink.Err("entry", label(raw))
			}
		}
		if err := makeDirs(x.root, parent, x.owner, x.made); err != nil {
			return err
		}
		f, st, err := openRegular(x.root, path.Join(x.dir, src))
		if err != nil {
			return errArchiveLink.Err("entry", label(raw))
		}
		defer f.Close()
		if st.Size() > x.left {
			return x.limit.over
		}
		return x.put(full, mode, f)
	case kindSymlink:
		// Where a link points is judged as written: relative, and staying
		// inside the folder it is unpacked into.
		if link == "" || strings.HasPrefix(link, "/") || strings.ContainsRune(link, 0) || escapes(path.Join(path.Dir(name), link)) {
			return errArchiveLink.Err("entry", label(raw))
		}
		if err := makeDirs(x.root, parent, x.owner, x.made); err != nil {
			return err
		}
		if st, err := x.root.Lstat(full); err == nil {
			if st.IsDir() {
				return &fs.PathError{Op: "symlink", Path: full, Err: fs.ErrExist}
			}
			if err := x.root.Remove(full); err != nil {
				return err
			}
		}
		if err := x.root.Symlink(link, full); err != nil {
			return err
		}
		x.links[name] = true
		return x.root.Lchown(full, int(x.owner.UID), int(x.owner.GID))
	}
	return errArchiveOther.Err("entry", label(raw))
}

// put writes a file's content. Files are made with the permissions of a
// plain file, executable for those the archive made so; special bits from
// an archive are not kept.
func (x *extractor) put(full string, mode fs.FileMode, body io.Reader) error {
	perm := fs.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	n, err := replaceFile(x.root, full, body, putOptions{Owner: x.owner, Mode: perm, Limit: x.left})
	x.left -= n
	x.written += n
	if errors.Is(err, errOverLimit) {
		return x.limit.over
	}
	return err
}

// ListFiles lists a folder of a volume.
func (c *Client) ListFiles(ctx context.Context, ref FileRef, dir string) (FileList, error) {
	var out FileList
	err := c.do(ctx, http.MethodPost, filesURL(ref, "list"), listRequest{Path: dir}, &out)
	return out, err
}

// ReadFile returns a text file of a volume, up to MaxEditBytes.
func (c *Client) ReadFile(ctx context.Context, ref FileRef, p string) ([]byte, error) {
	resp, err := c.raw(ctx, http.MethodGet, filesURL(ref, "content", "path", p), nil, -1)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, MaxEditBytes+1))
}

// WriteFile replaces a file of a volume, or with create makes it and refuses
// a name that is taken.
func (c *Client) WriteFile(ctx context.Context, ref FileRef, p string, content []byte, create bool) error {
	kv := []string{"path", p}
	if create {
		kv = append(kv, "new", "1")
	}
	resp, err := c.raw(ctx, http.MethodPut, filesURL(ref, "content", kv...), strings.NewReader(string(content)), int64(len(content)))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// StatFile describes one item of a volume.
func (c *Client) StatFile(ctx context.Context, ref FileRef, p string) (FileEntry, error) {
	var out FileEntry
	err := c.do(ctx, http.MethodPost, filesURL(ref, "stat"), listRequest{Path: p}, &out)
	return out, err
}

// RemoveFile removes a file or an empty folder of a volume.
func (c *Client) RemoveFile(ctx context.Context, ref FileRef, p string) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "remove"), listRequest{Path: p}, nil)
}

func (c *Client) MakeFolder(ctx context.Context, ref FileRef, p string) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "mkdir"), mkdirRequest{FileOwner: ref.FileOwner, Path: p}, nil)
}

func (c *Client) RenameFile(ctx context.Context, ref FileRef, from, to string) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "rename"), renameRequest{From: from, To: to}, nil)
}

func (c *Client) DeleteFiles(ctx context.Context, ref FileRef, paths []string) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "delete"), deleteRequest{Paths: paths}, nil)
}

// UploadFile stores what body carries as a file of a volume. size is its
// length when known, or -1.
func (c *Client) UploadFile(ctx context.Context, ref FileRef, p string, size int64, body io.Reader) error {
	resp, err := c.raw(ctx, http.MethodPut, filesURL(ref, "upload", "path", p), body, size)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// DownloadFile opens a file of a volume, which the caller closes, and says
// how long it is.
func (c *Client) DownloadFile(ctx context.Context, ref FileRef, p string) (io.ReadCloser, int64, error) {
	return c.DownloadRange(ctx, ref, p, 0, -1)
}

// DownloadRange is DownloadFile for length bytes from offset, or to the end
// when length is negative. What lies past the end of the file is not sent.
func (c *Client) DownloadRange(ctx context.Context, ref FileRef, p string, offset, length int64) (io.ReadCloser, int64, error) {
	kv := []string{"path", p}
	if offset > 0 {
		kv = append(kv, "offset", strconv.FormatInt(offset, 10))
	}
	if length >= 0 {
		kv = append(kv, "length", strconv.FormatInt(length, 10))
	}
	resp, err := c.raw(ctx, http.MethodGet, filesURL(ref, "download", kv...), nil, -1)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// CompressFiles packs items of one folder into a .tar.gz there and returns
// its name.
func (c *Client) CompressFiles(ctx context.Context, ref FileRef, dir string, paths []string, name string) (string, error) {
	var out compressResponse
	err := c.do(ctx, http.MethodPost, filesURL(ref, "compress"), compressRequest{FileOwner: ref.FileOwner, Dir: dir, Paths: paths, Name: name, Room: ref.Room}, &out)
	return out.Name, err
}

// ExtractFile unpacks an archive of a volume into dir, or into the folder it
// is in when dir is empty.
func (c *Client) ExtractFile(ctx context.Context, ref FileRef, p, dir string) (ExtractResult, error) {
	var out ExtractResult
	err := c.do(ctx, http.MethodPost, filesURL(ref, "extract"), extractRequest{FileOwner: ref.FileOwner, Path: p, Dir: dir, Room: ref.Room}, &out)
	return out, err
}

// filesURL is the address of a file request. The owner and the room go in
// the query of every request, which the ones with a JSON body do not read.
func filesURL(ref FileRef, op string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	q.Set("uid", strconv.FormatUint(uint64(ref.UID), 10))
	q.Set("gid", strconv.FormatUint(uint64(ref.GID), 10))
	if ref.Room != nil {
		q.Set("room", strconv.FormatInt(*ref.Room, 10))
	}
	return "/v1/volumes/" + url.PathEscape(ref.Volume) + "/files/" + op + "?" + q.Encode()
}

// raw makes a request whose body and answer are not JSON. A failed answer
// is turned into an error; a good one is returned open.
func (c *Client) raw(ctx context.Context, method, target string, body io.Reader, size int64) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://core"+target, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reach the Zelie core: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp, nil
}
