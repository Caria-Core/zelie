package core

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// What a client of SFTP may change on a file in place. The owner is not one
// of them: every file in a server's volume belongs to the user the game runs
// as, and the core is what keeps it so.

// permMask is the part of a mode that is set from outside. The special bits
// are left out on purpose: no one who can reach the volume has a use for a
// set-user-ID file there, and the core is not the place to make one.
const permMask = 0o777

type chmodRequest struct {
	Path string `json:"path"`
	// Mode is the permission bits. Anything above them is ignored.
	Mode uint32 `json:"mode"`
}

// chmodFile sets the permission bits of a file or folder, or of what a link
// in the volume points to. The set-user-ID, set-group-ID and sticky bits it
// had are cleared, and it never sets them.
func (s *Server) chmodFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req chmodRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err != nil {
		s.fileFailed(w, "change mode", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "change mode")
	if !ok {
		return
	}
	defer root.Close()
	if err := root.Chmod(p, fs.FileMode(req.Mode&permMask)); err != nil {
		s.fileFailed(w, "change mode", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type truncateRequest struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Room *int64 `json:"room,omitempty"`
}

// truncateFile cuts a file to a length, or makes it that long with a hole.
// Unlike the rest of the file manager it works on the file where it is, as
// truncate does in a shell: there is no new version to put in place. A file
// can grow by no more than the volume has room for, so this is no way to
// make a file of a size the limit would not have let it write.
func (s *Server) truncateFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req truncateRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err == nil && p == "." {
		err = errFileNotFile.Err("path", "/")
	}
	if err == nil && (req.Size < 0 || (req.Room != nil && *req.Room < 0)) {
		err = fmt.Errorf("%w: size and room are numbers of bytes, not below zero", errBadRequest)
	}
	if err != nil {
		s.fileFailed(w, "truncate file", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "truncate file")
	if !ok {
		return
	}
	defer root.Close()
	// Looked at before it is opened, so a folder or a named pipe is told
	// apart from a file by what it is. The open does not wait for a reader,
	// and what it opened is looked at again.
	st, err := root.Stat(p)
	if err == nil && !st.Mode().IsRegular() {
		err = errFileNotFile.Err("path", label(p))
	}
	var f *os.File
	if err == nil {
		f, err = root.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	}
	if err == nil {
		defer f.Close()
		if st, err = f.Stat(); err == nil && !st.Mode().IsRegular() {
			err = errFileNotFile.Err("path", label(p))
		}
	}
	if err == nil && req.Size > st.Size() {
		var c ceiling
		if c, err = limitFor(root, req.Room, 0, nil); err == nil && req.Size-st.Size() > c.bytes {
			err = c.over
		}
	}
	if err == nil {
		err = f.Truncate(req.Size)
	}
	if err != nil {
		s.fileFailed(w, "truncate file", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxFileTime is the latest time that is taken for a file, in seconds. SFTP
// sends times in 32 bits, so this is far beyond anything a client has.
const maxFileTime = 1 << 40

type chtimesRequest struct {
	Path string `json:"path"`
	// Atime and Mtime are seconds since 1970.
	Atime int64 `json:"atime"`
	Mtime int64 `json:"mtime"`
}

// chtimesFile sets the access and modification time of a file or folder, as
// scp -p and sftp put -p do for what they uploaded. A link is not followed:
// if the path is one, its own times are set, so a link in the volume is no way
// to reach another file.
func (s *Server) chtimesFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req chtimesRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p, err := filePath(req.Path)
	if err == nil && (req.Atime < 0 || req.Atime > maxFileTime || req.Mtime < 0 || req.Mtime > maxFileTime) {
		err = fmt.Errorf("%w: times are seconds since 1970, not below zero", errBadRequest)
	}
	if err != nil {
		s.fileFailed(w, "change times", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "change times")
	if !ok {
		return
	}
	defer root.Close()
	if err := setTimes(root, p, req.Atime, req.Mtime); err != nil {
		s.fileFailed(w, "change times", name, p, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setTimes sets the times of the item at p, in seconds since 1970. The folder
// it is in is opened through the root, and the item is then named from that
// folder's handle with the last step not followed.
func setTimes(root *os.Root, p string, atime, mtime int64) error {
	dir, base := path.Dir(p), path.Base(p)
	// O_DIRECTORY makes a named pipe in the place of the folder fail at once.
	// Without it the open waits for a writer, and the thread waits with it.
	d, err := root.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer d.Close()
	// Built from the seconds: a time of nanoseconds would wrap past 2262.
	ts := []unix.Timespec{{Sec: atime}, {Sec: mtime}}
	if err := unix.UtimesNanoAt(int(d.Fd()), base, ts, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return &fs.PathError{Op: "utimensat", Path: p, Err: err}
	}
	return nil
}

// SetFileMode sets the permission bits of a file or folder of a volume. Only
// the nine bits of read, write and execute are set.
func (c *Client) SetFileMode(ctx context.Context, ref FileRef, p string, mode fs.FileMode) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "chmod"), chmodRequest{Path: p, Mode: uint32(mode.Perm())}, nil)
}

// TruncateFile makes a file of a volume the given length.
func (c *Client) TruncateFile(ctx context.Context, ref FileRef, p string, size int64) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "truncate"), truncateRequest{Path: p, Size: size, Room: ref.Room}, nil)
}

// SetFileTimes sets the access and modification time of a file or folder of a
// volume, to the second.
func (c *Client) SetFileTimes(ctx context.Context, ref FileRef, p string, atime, mtime time.Time) error {
	return c.do(ctx, http.MethodPost, filesURL(ref, "chtimes"), chtimesRequest{Path: p, Atime: atime.Unix(), Mtime: mtime.Unix()}, nil)
}
