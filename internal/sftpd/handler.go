package sftpd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"math"
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

// Across one connection, all open files together hold at most this much
// out-of-order data, at most this many are open for writing, and at most
// this many folders are open for listing. An open folder keeps a request to
// the core going, whether or not the client reads from it.
const (
	maxPendingConn = 64 << 20
	maxWriteFiles  = 64
	maxOpenFolders = 32
)

// budget is what the open uploads and listings of one connection share.
type budget struct {
	mu                             sync.Mutex
	bytes, files, folders          int64
	maxBytes, maxFiles, maxFolders int64
}

func newBudget() *budget {
	return &budget{maxBytes: maxPendingConn, maxFiles: maxWriteFiles, maxFolders: maxOpenFolders}
}

func (b *budget) openFolder() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.folders >= b.maxFolders {
		return false
	}
	b.folders++
	return true
}

func (b *budget) closeFolder() {
	b.mu.Lock()
	b.folders--
	b.mu.Unlock()
}

func (b *budget) openFile() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files >= b.maxFiles {
		return false
	}
	b.files++
	return true
}

func (b *budget) closeFile() {
	b.mu.Lock()
	b.files--
	b.mu.Unlock()
}

func (b *budget) hold(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bytes+n > b.maxBytes {
		return false
	}
	b.bytes += n
	return true
}

func (b *budget) release(n int64) {
	b.mu.Lock()
	b.bytes -= n
	b.mu.Unlock()
}

type handler struct {
	s     *Server
	ctx   context.Context
	grant Grant
	ref   core.FileRef
	b     *budget

	// The uploads of this session that are open, by path, which is where an
	// attribute set on one has to wait for the file.
	mu      sync.Mutex
	uploads map[string]*upload
}

func (s *Server) handlers(ctx context.Context, g Grant, b *budget) sftp.Handlers {
	h := &handler{s: s, ctx: ctx, grant: g, b: b, ref: core.FileRef{Volume: g.Volume, FileOwner: core.FileOwner{UID: g.UID, GID: g.GID}}}
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
	if !h.b.openFile() {
		return nil, &failure{"too many files are open for writing", sftp.ErrSSHFxFailure}
	}
	ref := h.ref
	room, err := h.s.Panel.Room(h.ctx, h.grant.Server)
	if err != nil {
		// The core still stops a write at the machine's free disk.
		h.s.Log.Warn("ask the panel for the disk room", "server", h.grant.Server, "err", err)
	}
	ref.Room = room
	u := newUpload(h.ctx, h.s.Files, ref, r.Filepath, h.b)
	h.track(r.Filepath, u)
	return u, nil
}

func (h *handler) track(p string, u *upload) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.uploads == nil {
		h.uploads = map[string]*upload{}
	}
	h.uploads[p] = u
	u.release = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.uploads[p] == u {
			delete(h.uploads, p)
		}
	}
}

func (h *handler) uploading(p string) *upload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.uploads[p]
}

// upload streams a file to the core as it arrives. SFTP clients send
// several blocks at once, so a block ahead of the next one wanted waits, up
// to maxPending, until the ones before it are there.
type upload struct {
	pw   *io.PipeWriter
	done chan error
	b    *budget

	ctx   context.Context
	files Files
	ref   core.FileRef
	path  string
	// release is called once, when the upload is over.
	release func()

	mu      sync.Mutex
	next    int64
	pending map[int64][]byte
	held    int
	err     error
	closed  bool
	// What the client set on the open file. The core has no file at the path
	// until the upload ends, so these are applied then.
	set attrs
}

// attrs is what a client sets on a file. A field that is nil was not set.
type attrs struct {
	mode  *fs.FileMode
	size  *int64
	times *fileTimes
}

// fileTimes are the access and modification time, which SFTP sets together.
type fileTimes struct{ atime, mtime time.Time }

func (a attrs) none() bool { return a.mode == nil && a.size == nil && a.times == nil }

func newUpload(ctx context.Context, files Files, ref core.FileRef, path string, b *budget) *upload {
	pr, pw := io.Pipe()
	u := &upload{pw: pw, done: make(chan error, 1), b: b, pending: map[int64][]byte{},
		ctx: ctx, files: files, ref: ref, path: path}
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
		if len(p) == 0 {
			return 0, nil
		}
		if _, dup := u.pending[off]; dup {
			return 0, u.fail(&failure{"a file can only be written from start to end", sftp.ErrSSHFxOpUnsupported})
		}
		if u.held+len(p) > maxPending || !u.b.hold(int64(len(p))) {
			return 0, u.fail(&failure{"the file's blocks arrived too far out of order", sftp.ErrSSHFxFailure})
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
		u.b.release(int64(len(b)))
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
	u.b.release(int64(u.held))
	u.held = 0
	clear(u.pending)
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
	defer u.b.closeFile()
	defer u.released()
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
	if u.err == nil {
		u.err = u.settle()
	}
	return u.err
}

func (u *upload) released() {
	if u.release != nil {
		u.release()
	}
}

// keep takes the mode, the length or the times a client set on the file while
// it was being uploaded, as scp -p and sftp put -p do with the mode and the
// times, and some clients do with the final length before they send any of it.
func (u *upload) keep(a attrs) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return &failure{"the file is closed", sftp.ErrSSHFxFailure}
	}
	// What was sent cannot be taken back, as a shorter length would do.
	if a.size != nil && *a.size < u.sent() {
		return &failure{"a file that is being uploaded cannot be made shorter than what was sent", sftp.ErrSSHFxOpUnsupported}
	}
	if a.mode != nil {
		u.set.mode = a.mode
	}
	if a.size != nil {
		u.set.size = a.size
	}
	if a.times != nil {
		u.set.times = a.times
	}
	return nil
}

// sent is where the data received so far ends. Blocks that wait for the ones
// before them count too, at the place they will have.
func (u *upload) sent() int64 {
	end := u.next
	for off, b := range u.pending {
		end = max(end, off+int64(len(b)))
	}
	return end
}

// settle applies what keep took, now that the core has the file. A length
// above what was sent makes the file that long with a hole at the end, as the
// client asked for before it sent anything. The times come last, since
// changing the length changes them.
func (u *upload) settle() error {
	if u.set.size != nil && *u.set.size > u.next {
		if err := mapErr(u.files.TruncateFile(u.ctx, u.ref, u.path, *u.set.size)); err != nil {
			return err
		}
	}
	if u.set.mode != nil {
		if err := mapErr(u.files.SetFileMode(u.ctx, u.ref, u.path, *u.set.mode)); err != nil {
			return err
		}
	}
	if t := u.set.times; t != nil {
		return mapErr(u.files.SetFileTimes(u.ctx, u.ref, u.path, t.atime, t.mtime))
	}
	return nil
}

// TransferError is called when the connection breaks before the client is
// done. Nothing is kept.
func (u *upload) TransferError(err error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.err == nil {
		u.fail(err)
	}
	u.released()
}

func (h *handler) Filecmd(r *sftp.Request) error {
	ctx, ref := h.ctx, h.ref
	files := h.s.Files
	switch r.Method {
	case "Setstat":
		return h.setstat(r)
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

// setstat applies the attributes a client sets. Permissions are the nine
// bits of read, write and execute, a length is a truncate within the volume's
// room, and times are set to the second. The owner is not the client's to
// change: every file belongs to the user the game runs as.
func (h *handler) setstat(r *sftp.Request) error {
	f := r.AttrFlags()
	if f.UidGid {
		return &failure{"the owner of a file cannot be changed over SFTP: every file belongs to the server's own user", sftp.ErrSSHFxPermissionDenied}
	}
	st := r.Attributes()
	var a attrs
	if f.Permissions {
		m := fs.FileMode(st.Mode & 0o777)
		a.mode = &m
	}
	if f.Size {
		if st.Size > math.MaxInt64 {
			return &failure{"that length is more than a file can have", sftp.ErrSSHFxFailure}
		}
		n := int64(st.Size)
		a.size = &n
	}
	if f.Acmodtime {
		a.times = &fileTimes{time.Unix(int64(st.Atime), 0), time.Unix(int64(st.Mtime), 0)}
	}
	if a.none() {
		return nil
	}
	if u := h.uploading(r.Filepath); u != nil {
		return u.keep(a)
	}
	files, ref := h.s.Files, h.ref
	if a.size != nil {
		room, err := h.s.Panel.Room(h.ctx, h.grant.Server)
		if err != nil {
			h.s.Log.Warn("ask the panel for the disk room", "server", h.grant.Server, "err", err)
		}
		ref.Room = room
		if err := mapErr(files.TruncateFile(h.ctx, ref, r.Filepath, *a.size)); err != nil {
			return err
		}
	}
	if a.mode != nil {
		if err := mapErr(files.SetFileMode(h.ctx, ref, r.Filepath, *a.mode)); err != nil {
			return err
		}
	}
	if a.times != nil {
		return mapErr(files.SetFileTimes(h.ctx, ref, r.Filepath, a.times.atime, a.times.mtime))
	}
	return nil
}

func (h *handler) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	switch r.Method {
	case "List":
		// pkg/sftp asks for the listing when the client opens the folder,
		// and nothing limits how many a client opens.
		if !h.b.openFolder() {
			return nil, &failure{"too many folders are open at once, close one first", sftp.ErrSSHFxFailure}
		}
		// The folder is read as the client asks for it, so one with any
		// number of items is listed whole with the same little memory.
		ctx, stop := context.WithCancel(h.ctx)
		cancel := func() {
			stop()
			h.b.closeFolder()
		}
		folder, err := h.s.Files.OpenFolder(ctx, h.ref, r.Filepath)
		if err != nil {
			cancel()
			return nil, mapErr(err)
		}
		return &folderList{folder: folder, cancel: cancel}, nil
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

// folderList serves a folder's items as the core sends them. The client asks
// for them in order, so it reads each once and cannot go back.
type folderList struct {
	folder core.Folder
	// cancel ends the request to the core and gives the connection's slot
	// back.
	cancel func()
	read   int64
	done   bool
	closed sync.Once
}

func (l *folderList) ListAt(dst []os.FileInfo, off int64) (int, error) {
	if off != l.read {
		return 0, &failure{"a folder can only be listed from start to end", sftp.ErrSSHFxOpUnsupported}
	}
	n := 0
	for n < len(dst) && !l.done {
		e, err := l.folder.Next()
		if errors.Is(err, io.EOF) {
			l.done = true
			break
		}
		if err != nil {
			// The client gets the error, not the items before it, so a
			// listing that broke off never looks whole.
			return 0, mapErr(err)
		}
		dst[n] = info{e}
		n++
	}
	l.read += int64(n)
	if l.done {
		return n, io.EOF
	}
	return n, nil
}

func (l *folderList) Close() error {
	var err error
	l.closed.Do(func() {
		l.cancel()
		err = l.folder.Close()
	})
	return err
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
