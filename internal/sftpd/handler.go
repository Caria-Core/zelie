package sftpd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pkg/sftp"

	"github.com/Caria-Core/zelie/internal/core"
)

// The paths a client sends are already absolute and clean, which is all
// pkg/sftp does to them. They go to the core as they are: it is the core
// that keeps every one inside the server's volume.

// maxPending is how much of a file may arrive ahead of the part before it.
const maxPending = 32 << 20

type handler struct {
	s     *Server
	ctx   context.Context
	grant Grant
	ref   core.FileRef
}

func (s *Server) handlers(ctx context.Context, g Grant) sftp.Handlers {
	h := &handler{s: s, ctx: ctx, grant: g, ref: core.FileRef{Volume: g.Volume, FileOwner: core.FileOwner{UID: g.UID, GID: g.GID}}}
	return sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h}
}

// failure is an error from the core, carrying the SFTP status it means.
type failure struct {
	msg  string
	code error
}

func (f *failure) Error() string { return f.msg }
func (f *failure) Unwrap() error { return f.code }

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		code := sftp.ErrSSHFxFailure
		switch ce.Status {
		case http.StatusNotFound:
			code = sftp.ErrSSHFxNoSuchFile
		case http.StatusForbidden:
			code = sftp.ErrSSHFxPermissionDenied
		}
		return &failure{ce.Message, code}
	}
	return &failure{"the server's files cannot be reached right now", sftp.ErrSSHFxFailure}
}

func (h *handler) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	st, err := h.s.Files.StatFile(h.ctx, h.ref, r.Filepath)
	if err != nil {
		return nil, mapErr(err)
	}
	if st.Dir {
		return nil, &failure{r.Filepath + " is a folder", sftp.ErrSSHFxFailure}
	}
	return &reader{h: h, path: r.Filepath}, nil
}

// reader reads by asking the core for the range each time. Reads of one
// file come in several at once and out of order, so a stream would not do.
type reader struct {
	h    *handler
	path string
}

func (r *reader) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	body, _, err := r.h.s.Files.DownloadRange(r.h.ctx, r.h.ref, r.path, off, int64(len(p)))
	if err != nil {
		return 0, mapErr(err)
	}
	defer body.Close()
	n, err := io.ReadFull(body, p)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return n, io.EOF
	}
	return n, err
}

func (h *handler) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	flags := r.Pflags()
	if flags.Append {
		return nil, sftp.ErrSSHFxOpUnsupported
	}
	st, err := h.s.Files.StatFile(h.ctx, h.ref, r.Filepath)
	switch {
	case err == nil:
		if st.Dir {
			return nil, &failure{r.Filepath + " is a folder", sftp.ErrSSHFxFailure}
		}
		if flags.Excl {
			return nil, &failure{r.Filepath + " exists already", sftp.ErrSSHFxFailure}
		}
		// A file is always replaced whole, so an open that would keep part
		// of the old one cannot be honoured.
		if !flags.Trunc && st.Size > 0 {
			return nil, sftp.ErrSSHFxOpUnsupported
		}
	default:
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Status != http.StatusNotFound {
			return nil, mapErr(err)
		}
		if !flags.Creat {
			return nil, &failure{r.Filepath + " does not exist", sftp.ErrSSHFxNoSuchFile}
		}
	}
	ref := h.ref
	room, err := h.s.Panel.Room(h.ctx, h.grant.Server)
	if err != nil {
		// The core still stops a write at the machine's free disk.
		h.s.Log.Warn("ask the panel for the disk room", "server", h.grant.Server, "err", err)
	}
	ref.Room = room
	return newUpload(h.ctx, h.s.Files, ref, r.Filepath), nil
}

// upload streams a file to the core as it arrives. SFTP clients send
// several blocks at once, so a block ahead of the next one wanted waits, up
// to maxPending, until the ones before it are there.
type upload struct {
	pw   *io.PipeWriter
	done chan error

	mu      sync.Mutex
	next    int64
	pending map[int64][]byte
	held    int
	err     error
	closed  bool
}

func newUpload(ctx context.Context, files Files, ref core.FileRef, path string) *upload {
	pr, pw := io.Pipe()
	u := &upload{pw: pw, done: make(chan error, 1), pending: map[int64][]byte{}}
	go func() {
		err := files.UploadFile(ctx, ref, path, -1, pr)
		pr.CloseWithError(errors.New("the upload ended"))
		u.done <- mapErr(err)
	}()
	return u
}

func (u *upload) WriteAt(p []byte, off int64) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err != nil {
		return 0, u.err
	}
	switch {
	case off < u.next:
		return 0, u.fail(&failure{"a file can only be written from start to end", sftp.ErrSSHFxOpUnsupported})
	case off > u.next:
		if u.held+len(p) > maxPending {
			return 0, u.fail(&failure{"the file's blocks arrived too far out of order", sftp.ErrSSHFxFailure})
		}
		if _, dup := u.pending[off]; dup {
			return 0, u.fail(&failure{"a file can only be written from start to end", sftp.ErrSSHFxOpUnsupported})
		}
		u.pending[off] = append([]byte(nil), p...)
		u.held += len(p)
		return len(p), nil
	}
	if err := u.push(p); err != nil {
		return 0, err
	}
	for {
		b, ok := u.pending[u.next]
		if !ok {
			return len(p), nil
		}
		delete(u.pending, u.next)
		u.held -= len(b)
		if err := u.push(b); err != nil {
			return 0, err
		}
	}
}

func (u *upload) push(b []byte) error {
	if _, err := u.pw.Write(b); err != nil {
		// The core stopped reading: its own answer says why.
		u.pw.CloseWithError(err)
		return u.fail(u.finish())
	}
	u.next += int64(len(b))
	return nil
}

func (u *upload) fail(err error) error {
	u.err = err
	u.pw.CloseWithError(err)
	return err
}

// finish waits for the core's answer to the upload.
func (u *upload) finish() error {
	err := <-u.done
	u.done <- err
	if err == nil {
		err = &failure{"the upload stopped before the end", sftp.ErrSSHFxFailure}
	}
	return err
}

// Close ends the file. It is where the client learns whether the core kept
// it.
func (u *upload) Close() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return u.err
	}
	u.closed = true
	if u.err == nil && len(u.pending) > 0 {
		u.fail(&failure{"the file has a gap in it", sftp.ErrSSHFxFailure})
	}
	if u.err != nil {
		u.pw.CloseWithError(u.err)
		<-u.done
		return u.err
	}
	u.pw.Close()
	u.err = <-u.done
	return u.err
}

// TransferError is called when the connection breaks before the client is
// done. Nothing is kept.
func (u *upload) TransferError(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err == nil {
		u.fail(err)
	}
}

func (h *handler) Filecmd(r *sftp.Request) error {
	ctx, ref := h.ctx, h.ref
	files := h.s.Files
	switch r.Method {
	case "Setstat":
		// Permissions and times belong to the game user's files, not to
		// whoever is logged in.
		return nil
	case "Rename":
		return mapErr(files.RenameFile(ctx, ref, r.Filepath, r.Target))
	case "Mkdir":
		return mapErr(files.MakeFolder(ctx, ref, r.Filepath))
	case "Remove", "Rmdir":
		st, err := files.StatFile(ctx, ref, r.Filepath)
		if err != nil {
			return mapErr(err)
		}
		if r.Method == "Remove" && st.Dir {
			return &failure{r.Filepath + " is a folder; remove it with rmdir", sftp.ErrSSHFxFailure}
		}
		if r.Method == "Rmdir" && !st.Dir {
			return &failure{r.Filepath + " is not a folder", sftp.ErrSSHFxFailure}
		}
		return mapErr(files.RemoveFile(ctx, ref, r.Filepath))
	}
	return sftp.ErrSSHFxOpUnsupported
}

func (h *handler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		list, err := h.s.Files.ListFiles(h.ctx, h.ref, r.Filepath)
		if err != nil {
			return nil, mapErr(err)
		}
		out := make(listing, len(list.Entries))
		for i, e := range list.Entries {
			out[i] = info{e}
		}
		return out, nil
	case "Stat", "Lstat":
		e, err := h.s.Files.StatFile(h.ctx, h.ref, r.Filepath)
		if err != nil {
			return nil, mapErr(err)
		}
		return listing{info{e}}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

// listing serves a folder's entries in pages, as the client asks.
type listing []os.FileInfo

func (l listing) ListAt(dst []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[off:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}

// info is a core file entry as an os.FileInfo.
type info struct{ e core.FileEntry }

func (i info) Name() string       { return i.e.Name }
func (i info) Size() int64        { return i.e.Size }
func (i info) ModTime() time.Time { return i.e.Modified }
func (i info) IsDir() bool        { return i.e.Dir }
func (i info) Sys() any           { return nil }

// Mode is rebuilt from what the core says: its type, and the permission
// bits of the string fs.FileMode made.
func (i info) Mode() fs.FileMode {
	var m fs.FileMode
	if s := i.e.Mode; len(s) >= 9 {
		for n, c := range s[len(s)-9:] {
			if c != '-' {
				m |= 1 << (8 - n)
			}
		}
	}
	switch {
	case i.e.Dir:
		m |= fs.ModeDir
	case i.e.Symlink:
		m |= fs.ModeSymlink
	}
	return m
}
