package sftpd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"

	"github.com/Caria-Core/zelie/internal/core"
)

// drain is a core that reads uploads to the end and keeps nothing.
type drain struct{ Files }

func (drain) UploadFile(_ context.Context, _ core.FileRef, _ string, _ int64, body io.Reader) error {
	_, err := io.Copy(io.Discard, body)
	return err
}

func TestPendingBytesAreCappedPerConnection(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	ctx := context.Background()
	a := newUpload(ctx, drain{}, core.FileRef{}, "/a", b)
	c := newUpload(ctx, drain{}, core.FileRef{}, "/c", b)
	if _, err := a.WriteAt(make([]byte, 60), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WriteAt(make([]byte, 60), 1000); err == nil {
		t.Fatal("pending data over the connection's cap was kept")
	}
	// Failing the second file gives nothing back that it did not hold, and
	// closing the first returns its share.
	a.Close()
	d := newUpload(ctx, drain{}, core.FileRef{}, "/d", b)
	if _, err := d.WriteAt(make([]byte, 60), 1000); err != nil {
		t.Errorf("room was not given back: %v", err)
	}
	d.Close()
	if b.bytes != 0 {
		t.Errorf("%d bytes still counted", b.bytes)
	}
}

func TestPendingBytesAreGivenBackWhenTheGapFills(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	u := newUpload(context.Background(), drain{}, core.FileRef{}, "/a", b)
	if _, err := u.WriteAt(make([]byte, 10), 10); err != nil {
		t.Fatal(err)
	}
	if b.bytes != 10 {
		t.Fatalf("held %d, want 10", b.bytes)
	}
	if _, err := u.WriteAt(make([]byte, 10), 0); err != nil {
		t.Fatal(err)
	}
	if b.bytes != 0 {
		t.Errorf("held %d after the gap filled", b.bytes)
	}
	if err := u.Close(); err != nil {
		t.Error(err)
	}
}

func TestEmptyOutOfOrderWriteIsNotStored(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	u := newUpload(context.Background(), drain{}, core.FileRef{}, "/a", b)
	for range 1000 {
		if _, err := u.WriteAt(nil, 50); err != nil {
			t.Fatal(err)
		}
	}
	if len(u.pending) != 0 || b.bytes != 0 {
		t.Errorf("%d empty blocks stored", len(u.pending))
	}
	u.Close()
}

func TestWriteHandlesAreCapped(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 2}
	if !b.openFile() || !b.openFile() {
		t.Fatal("refused under the cap")
	}
	if b.openFile() {
		t.Error("a third write handle was given")
	}
	b.closeFile()
	if !b.openFile() {
		t.Error("a closed handle was not given back")
	}
}

// sizing is a core that reads uploads like drain and remembers the lengths it
// was asked to make.
type sizing struct {
	drain
	lengths []int64
}

func (s *sizing) TruncateFile(_ context.Context, _ core.FileRef, _ string, size int64) error {
	s.lengths = append(s.lengths, size)
	return nil
}

// A block that waits for the ones before it will end the file where it ends,
// so a length cannot be set below that, or the file would come out longer
// than the client asked for, with nothing said.
func TestLengthBelowABlockThatWaitsIsRefused(t *testing.T) {
	files := &sizing{}
	u := newUpload(context.Background(), files, core.FileRef{}, "/a", newBudget())
	const gap, block = 1 << 20, 64 << 10
	if _, err := u.WriteAt(make([]byte, block), gap); err != nil {
		t.Fatal(err)
	}
	for _, size := range []int64{block, gap, gap + block - 1} {
		if err := u.keep(attrs{size: &size}); !errors.Is(err, sftp.ErrSSHFxOpUnsupported) {
			t.Errorf("a length of %d, with a block waiting until %d: %v", size, gap+block, err)
		}
	}
	size := int64(gap + block)
	if err := u.keep(attrs{size: &size}); err != nil {
		t.Fatal(err)
	}
	if _, err := u.WriteAt(make([]byte, gap), 0); err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	if len(files.lengths) != 0 {
		t.Errorf("the file was made %v long, it is already that", files.lengths)
	}
}

func TestOpenFoldersAreCapped(t *testing.T) {
	b := &budget{maxFolders: 2}
	if !b.openFolder() || !b.openFolder() {
		t.Fatal("refused under the cap")
	}
	if b.openFolder() {
		t.Error("a third folder was opened")
	}
	b.closeFolder()
	if !b.openFolder() {
		t.Error("a closed folder was not given back")
	}
}

// pkg/sftp asks for the listing when a client opens a folder, so a client
// that opens folders and never reads them would hold a request to the core
// for each, without limit.
func TestOpenFolderHandlesAreRefusedBeyondTheCap(t *testing.T) {
	files := &fakeFiles{made: map[string]madeFolder{"/d": {n: 10}}}
	s := &Server{Files: files}
	h := s.handlers(context.Background(), Grant{Volume: "v"}, newBudget()).FileList

	var open []sftp.ListerAt
	for i := range maxOpenFolders {
		la, err := h.Filelist(&sftp.Request{Method: "List", Filepath: "/d"})
		if err != nil {
			t.Fatalf("folder %d: %v", i+1, err)
		}
		open = append(open, la)
	}
	_, err := h.Filelist(&sftp.Request{Method: "List", Filepath: "/d"})
	if !errors.Is(err, sftp.ErrSSHFxFailure) {
		t.Fatalf("folder %d: %v, want a failure status", maxOpenFolders+1, err)
	}

	open[3].(io.Closer).Close()
	// A handle is closed once by pkg/sftp, but a second close must not give
	// the slot back twice.
	open[3].(io.Closer).Close()
	la, err := h.Filelist(&sftp.Request{Method: "List", Filepath: "/d"})
	if err != nil {
		t.Fatalf("after closing one: %v", err)
	}
	if _, err := h.Filelist(&sftp.Request{Method: "List", Filepath: "/d"}); !errors.Is(err, sftp.ErrSSHFxFailure) {
		t.Errorf("one slot was given back twice: %v", err)
	}
	open[3] = la
	for _, la := range open {
		la.(io.Closer).Close()
	}
	if got := files.opened.Load(); got != maxOpenFolders+1 {
		t.Errorf("%d listings were closed, want %d", got, maxOpenFolders+1)
	}
	if _, err := h.Filelist(&sftp.Request{Method: "List", Filepath: "/d"}); err != nil {
		t.Errorf("after closing them all: %v", err)
	}
}

// A listing the core refuses does not keep a slot.
func TestFailedListingKeepsNoSlot(t *testing.T) {
	files := &fakeFiles{dirs: map[string]string{"v": t.TempDir()}}
	s := &Server{Files: files}
	b := &budget{maxFolders: 1}
	h := s.handlers(context.Background(), Grant{Volume: "v"}, b).FileList
	for range 3 {
		if _, err := h.Filelist(&sftp.Request{Method: "List", Filepath: "/missing"}); !errors.Is(err, sftp.ErrSSHFxNoSuchFile) {
			t.Fatalf("got %v, want no such file", err)
		}
	}
	if b.folders != 0 {
		t.Errorf("%d slots still taken", b.folders)
	}
}

// A block that is sent twice is refused, and must not stay counted against
// the connection after the upload has failed.
func TestDuplicateBlockLeavesNothingCounted(t *testing.T) {
	b := &budget{maxBytes: 100, maxFiles: 8}
	u := newUpload(context.Background(), drain{}, core.FileRef{}, "/a", b)
	if _, err := u.WriteAt(make([]byte, 10), 50); err != nil {
		t.Fatal(err)
	}
	if _, err := u.WriteAt(make([]byte, 10), 50); err == nil {
		t.Fatal("a block at the same place was kept")
	}
	if b.bytes != 0 {
		t.Errorf("%d bytes still counted", b.bytes)
	}
	u.Close()
	if b.bytes != 0 {
		t.Errorf("%d bytes counted after closing", b.bytes)
	}
}

// itemsOf is a folder of n items called a0, a1 and so on, that ends with err
// where it would end, or with io.EOF.
type itemsOf struct {
	n      int
	err    error
	i      int
	closed bool
}

func (f *itemsOf) Next() (core.FileEntry, error) {
	if f.i == f.n {
		if f.err != nil {
			return core.FileEntry{}, f.err
		}
		return core.FileEntry{}, io.EOF
	}
	f.i++
	return core.FileEntry{Name: "a" + strconv.Itoa(f.i-1)}, nil
}

func (f *itemsOf) Close() error { f.closed = true; return nil }

// A client asks for a folder a page at a time. Whatever the folder holds, it
// is handed over in those pages, whole, and in order.
func TestFolderListIsServedInPages(t *testing.T) {
	folder := &itemsOf{n: 250}
	ctx, cancel := context.WithCancel(context.Background())
	l := &folderList{folder: folder, cancel: cancel}
	var names []string
	off := int64(0)
	for {
		dst := make([]os.FileInfo, 100)
		n, err := l.ListAt(dst, off)
		for _, fi := range dst[:n] {
			names = append(names, fi.Name())
		}
		off += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if n != 100 {
			t.Fatalf("a page of %d, with more to come", n)
		}
	}
	if len(names) != 250 || names[0] != "a0" || names[249] != "a249" {
		t.Errorf("%d items, from %q to %q", len(names), names[0], names[len(names)-1])
	}
	if n, err := l.ListAt(make([]os.FileInfo, 100), off); n != 0 || err != io.EOF {
		t.Errorf("after the end: %d, %v", n, err)
	}
	// Asking again for what was handed out is not something a stream can do.
	if _, err := l.ListAt(make([]os.FileInfo, 10), 0); !errors.Is(err, sftp.ErrSSHFxOpUnsupported) {
		t.Errorf("listing again from the start: %v", err)
	}
	l.Close()
	if !folder.closed || ctx.Err() == nil {
		t.Errorf("closing the list left the stream open: closed %v, context %v", folder.closed, ctx.Err())
	}
}

// A folder that breaks off is an error. The items before it are not handed
// over as if they were all of them, or a client that mirrors the folder would
// miss files without a word.
func TestFolderListThatBreaksOffIsAnError(t *testing.T) {
	broke := &core.Error{Status: 500, Message: "listing the folder failed, see the core log"}
	l := &folderList{folder: &itemsOf{n: 5, err: broke}, cancel: func() {}}
	n, err := l.ListAt(make([]os.FileInfo, 100), 0)
	if n != 0 || err == nil || err == io.EOF {
		t.Fatalf("got %d items and %v", n, err)
	}
	if !errors.Is(err, sftp.ErrSSHFxFailure) {
		t.Errorf("err = %v, want a failure status", err)
	}
}

// recording is a core that notes what it was asked to do, in order.
type recording struct {
	drain
	mu    sync.Mutex
	calls []string
}

func (r *recording) note(format string, a ...any) {
	r.mu.Lock()
	r.calls = append(r.calls, fmt.Sprintf(format, a...))
	r.mu.Unlock()
}

func (r *recording) UploadFile(ctx context.Context, ref core.FileRef, p string, size int64, body io.Reader) error {
	err := r.drain.UploadFile(ctx, ref, p, size, body)
	r.note("upload %s", p)
	return err
}

func (r *recording) SetFileMode(_ context.Context, _ core.FileRef, p string, mode fs.FileMode) error {
	r.note("mode %s %o", p, mode)
	return nil
}

func (r *recording) TruncateFile(_ context.Context, _ core.FileRef, p string, size int64) error {
	r.note("length %s %d", p, size)
	return nil
}

func (r *recording) SetFileTimes(_ context.Context, _ core.FileRef, p string, atime, mtime time.Time) error {
	r.note("times %s %d %d", p, atime.Unix(), mtime.Unix())
	return nil
}

func (r *recording) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

type noRoom struct{ Panel }

func (noRoom) Room(context.Context, string) (*int64, error) { return nil, nil }

// setstatOf is a SETSTAT or FSETSTAT request with the attributes in the order
// the protocol has them: length, permissions, then access and modification
// time.
func setstatOf(p string, size, mode *uint64, times *[2]uint32) *sftp.Request {
	r := &sftp.Request{Method: "Setstat", Filepath: p}
	if size != nil {
		r.Flags |= 0x1
		r.Attrs = binary.BigEndian.AppendUint64(r.Attrs, *size)
	}
	if mode != nil {
		r.Flags |= 0x4
		r.Attrs = binary.BigEndian.AppendUint32(r.Attrs, uint32(*mode))
	}
	if times != nil {
		r.Flags |= 0x8
		r.Attrs = binary.BigEndian.AppendUint32(r.Attrs, times[0])
		r.Attrs = binary.BigEndian.AppendUint32(r.Attrs, times[1])
	}
	return r
}

// What scp -p and sftp put -p send on the open file: the mode and the times,
// before they close it. The core has no file at the path until then, so they
// are applied once it has, and the times last, since a change of length would
// undo them.
func TestAttributesOfAnOpenUploadAreAppliedAfterTheFileIsIn(t *testing.T) {
	files := &recording{}
	s := &Server{Files: files, Panel: noRoom{}}
	h := &handler{s: s, ctx: context.Background(), b: newBudget()}
	u := newUpload(h.ctx, files, h.ref, "/a", h.b)
	h.track("/a", u)

	size, mode := uint64(100), uint64(0o640)
	if err := h.Filecmd(setstatOf("/a", &size, &mode, &[2]uint32{1700000000, 1600000000})); err != nil {
		t.Fatal(err)
	}
	if got := files.got(); len(got) != 0 {
		t.Fatalf("applied before the file was in: %v", got)
	}
	if _, err := u.WriteAt([]byte("0123456789"), 0); err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{"upload /a", "length /a 100", "mode /a 640", "times /a 1700000000 1600000000"}
	if got := files.got(); !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
}

func TestAttributesOfAFileAreAppliedAtOnce(t *testing.T) {
	files := &recording{}
	s := &Server{Files: files, Panel: noRoom{}}
	h := &handler{s: s, ctx: context.Background(), b: newBudget()}
	size, mode := uint64(5), uint64(0o600)
	if err := h.Filecmd(setstatOf("/a", &size, &mode, &[2]uint32{10, 20})); err != nil {
		t.Fatal(err)
	}
	if err := h.Filecmd(setstatOf("/b", nil, nil, &[2]uint32{30, 40})); err != nil {
		t.Fatal(err)
	}
	want := []string{"length /a 5", "mode /a 600", "times /a 10 20", "times /b 30 40"}
	if got := files.got(); !slices.Equal(got, want) {
		t.Errorf("calls %q, want %q", got, want)
	}
}

// Times are kept for an upload that ends well, and for none that does not.
func TestTimesOfAnUploadThatBreaksAreNotApplied(t *testing.T) {
	files := &recording{}
	h := &handler{s: &Server{Files: files}, ctx: context.Background(), b: newBudget()}
	u := newUpload(h.ctx, files, h.ref, "/a", h.b)
	h.track("/a", u)
	if err := h.Filecmd(setstatOf("/a", nil, nil, &[2]uint32{10, 20})); err != nil {
		t.Fatal(err)
	}
	u.TransferError(errors.New("the connection broke"))
	u.Close()
	for _, c := range files.got() {
		if strings.HasPrefix(c, "times") {
			t.Errorf("applied %q to a file that was never kept", c)
		}
	}
}
