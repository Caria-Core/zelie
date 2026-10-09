package core

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/peer"
)

func TestEditorSaveKeepsToTheRoom(t *testing.T) {
	e := newFileEnv(t)
	e.write("cfg.txt", "0123456789")
	put := func(path, room, body string, extra ...string) (int, string) {
		return e.do("PUT", "content", e.query(append([]string{"path", path, "room", room}, extra...)...), body)
	}

	// A server at its limit can still have a file edited, as long as the
	// file does not grow.
	if code, body := put("cfg.txt", "0", "abcdefghij"); code != http.StatusNoContent {
		t.Errorf("same size, no room: %d %s", code, body)
	}
	if code, body := put("cfg.txt", "0", "abcdefghijk"); code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("growing with no room: %d %s", code, body)
	}
	if got, _ := os.ReadFile(filepath.Join(e.vol, "cfg.txt")); string(got) != "abcdefghij" {
		t.Errorf("a refused save changed the file: %q", got)
	}
	if code, body := put("cfg.txt", "5", "abcdefghijklmno"); code != http.StatusNoContent {
		t.Errorf("growing by what is left: %d %s", code, body)
	}

	// A new file takes room.
	if code, body := put("new.txt", "0", "x", "new", "1"); code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("new file with no room: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(e.vol, "new.txt")); err == nil {
		t.Error("the file was made without room")
	}
	if code, body := put("new.txt", "3", "xyz", "new", "1"); code != http.StatusNoContent {
		t.Errorf("new file at the room: %d %s", code, body)
	}
	e.noTemp()
}

// The limit counts blocks, so a file with holes gives back what it takes,
// not its length.
func TestEditorSaveCreditsOnlyWhatASparseFileTakes(t *testing.T) {
	e := newFileEnv(t)
	f, err := os.Create(filepath.Join(e.vol, "sparse.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Skipf("no sparse files here: %v", err)
	}
	f.Close()
	st, err := os.Lstat(filepath.Join(e.vol, "sparse.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := onDisk(st); got >= 1<<20 {
		t.Skipf("this file system gives a sparse file %d bytes of blocks", got)
	}
	// A server with no room left gets back almost nothing for a file that
	// takes almost nothing, so it cannot save a megabyte over it.
	code, body := e.do("PUT", "content", e.query("path", "sparse.txt", "room", "0"), strings.Repeat("x", 1<<20))
	if code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("saving a megabyte over a sparse file with no room: %d %s", code, body)
	}
	e.noTemp()
}

// tarGzOf makes a .tar.gz with the entries write puts in it.
func tarGzOf(t *testing.T, write func(tw *tar.Writer) error) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := write(tw); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// Entries that make nothing are still read through, so they count, whatever
// they are or carry.
func TestExtractCountsTarHeadersThatMakeNothing(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	maxArchiveEntries = 10

	folder := tarGzOf(t, func(tw *tar.Writer) error {
		for range maxArchiveEntries + 1 {
			if err := tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
				return err
			}
		}
		return nil
	})
	pax := tarGzOf(t, func(tw *tar.Writer) error {
		for range maxArchiveEntries + 1 {
			if err := tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": "x"}}); err != nil {
				return err
			}
		}
		return nil
	})
	for name, archive := range map[string][]byte{"folder": folder, "pax": pax} {
		e.write(name+".tgz", string(archive))
		if code, body := e.extract(name+".tgz", "into", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_too_many" {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
}

// What the tar reader gets through is bounded by what may be written, even
// when the entries it reads make no file.
func TestExtractStopsReadingPastWhatMayBeWritten(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	maxArchiveEntries = 10

	body := strings.Repeat("z", 100<<10)
	e.write("dots.tgz", string(makeTarGz(t,
		tarEntry{hdr: tar.Header{Name: "."}, body: body},
		tarEntry{hdr: tar.Header{Name: "."}, body: body},
		tarEntry{hdr: tar.Header{Name: "."}, body: body},
	)))
	room := int64(0)
	if code, out := e.extract("dots.tgz", "into", &room); code != http.StatusConflict || fileCode(out) != "files.volume_full" {
		t.Errorf("entries that make nothing, no room: %d %s", code, out)
	}
}

// zipOfEmptyRecords is a zip whose central directory holds n records with no
// name, which is as small as a record gets.
func zipOfEmptyRecords(n int) []byte {
	var b bytes.Buffer
	for range n {
		rec := make([]byte, 46)
		binary.LittleEndian.PutUint32(rec, 0x02014b50)
		binary.LittleEndian.PutUint16(rec[4:], 20)
		binary.LittleEndian.PutUint16(rec[6:], 20)
		b.Write(rec)
	}
	end := make([]byte, 22)
	binary.LittleEndian.PutUint32(end, 0x06054b50)
	binary.LittleEndian.PutUint16(end[8:], uint16(n))
	binary.LittleEndian.PutUint16(end[10:], uint16(n))
	binary.LittleEndian.PutUint32(end[12:], uint32(n*46))
	b.Write(end)
	return b.Bytes()
}

type countingReaderAt struct {
	r io.ReaderAt
	n int64
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}

func TestZipDirectoryIsNotReadWholeBeforeItIsCounted(t *testing.T) {
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	maxArchiveEntries = 10
	z := zipOfEmptyRecords(20000)

	// zip.NewReader alone reads every record into memory first.
	if zr, err := zip.NewReader(bytes.NewReader(z), int64(len(z))); err != nil || len(zr.File) != 20000 {
		t.Fatalf("the crafted archive is not what the test needs: %v", err)
	}
	counted := &countingReaderAt{r: bytes.NewReader(z)}
	dr := newDirReader(counted)
	_, err := zip.NewReader(dr, int64(len(z)))
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != "files.archive_too_many" {
		t.Fatalf("got %v, want the entry limit", err)
	}
	if counted.n > int64(len(z))/2 {
		t.Errorf("read %d of %d bytes before giving up", counted.n, len(z))
	}

	// Once it is open, the entries' content is read as needed.
	var plain bytes.Buffer
	zw := zip.NewWriter(&plain)
	w, _ := zw.Create("a")
	w.Write(bytes.Repeat([]byte("a"), 1<<20))
	zw.Close()
	dr = &dirReader{r: bytes.NewReader(plain.Bytes()), left: 1 << 20}
	zr, err := zip.NewReader(dr, int64(plain.Len()))
	if err != nil {
		t.Fatal(err)
	}
	dr.done = true
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := io.Copy(io.Discard, rc); err != nil || n != 1<<20 {
		t.Errorf("read %d bytes of an entry: %v", n, err)
	}
}

// The unit test above shows what the reader does. This one shows the
// extractor uses it: the answer would be the same without it, but only after
// the whole directory was held in memory.
func TestExtractDoesNotHoldAZipsWholeDirectory(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	maxArchiveEntries = 1000
	const records = 200000
	e.write("big.zip", string(zipOfEmptyRecords(records)))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	code, body := e.extract("big.zip", "into", nil)
	runtime.ReadMemStats(&after)
	if code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_too_many" {
		t.Errorf("%d %s", code, body)
	}
	// All the records would come to some 40 MB.
	if got := after.TotalAlloc - before.TotalAlloc; got > 10<<20 {
		t.Errorf("extracting allocated %d bytes for an archive refused for its directory", got)
	}
}

// zipEndRecords is the end of a zip64 archive that claims records entries:
// the zip64 end record, its locator and the plain end record, whose fields
// say to look at the other two.
func zipEndRecords(records uint64, at int64) []byte {
	b := make([]byte, zip64EndLen+zip64LocLen+zipEndLen)
	le := binary.LittleEndian
	copy(b, zip64EndSig)
	le.PutUint64(b[4:], 44)
	le.PutUint16(b[12:], 45)
	le.PutUint16(b[14:], 45)
	le.PutUint64(b[24:], records)
	le.PutUint64(b[32:], records)
	le.PutUint64(b[48:], uint64(at))
	loc := b[zip64EndLen:]
	copy(loc, zip64LocSig)
	le.PutUint64(loc[8:], uint64(at))
	le.PutUint32(loc[16:], 1)
	end := loc[zip64LocLen:]
	copy(end, zipEndSig)
	le.PutUint16(end[8:], 0xffff)
	le.PutUint16(end[10:], 0xffff)
	le.PutUint32(end[12:], 0xffffffff)
	le.PutUint32(end[16:], 0xffffffff)
	return b
}

// sparseZip64 makes a file of size bytes that takes no disk space and ends
// like a zip64 archive of that many entries.
func sparseZip64(t *testing.T, path string, size int64, records uint64) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Skipf("no sparse files here: %v", err)
	}
	end := zipEndRecords(records, size-int64(zip64EndLen+zip64LocLen+zipEndLen))
	if _, err := f.WriteAt(end, size-int64(len(end))); err != nil {
		t.Fatal(err)
	}
}

// The directory is not read until the count is known to be fine, but
// zip.NewReader makes room for the count before it reads anything: a file
// that is only sparse can ask for gigabytes with a record 98 bytes long.
func TestExtractRefusesAZipClaimingMoreEntriesThanItCanHold(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	sparseZip64(t, filepath.Join(e.vol, "world-backup.zip"), 32<<30, 1e9)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	code, body := e.extract("world-backup.zip", "into", nil)
	runtime.ReadMemStats(&after)
	if code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_too_many" {
		t.Errorf("%d %s", code, body)
	}
	// One entry takes 8 bytes of the room made for them.
	if got := after.TotalAlloc - before.TotalAlloc; got > 10<<20 {
		t.Errorf("extracting allocated %d bytes for an archive refused for its end record", got)
	}
	if left, _ := os.ReadDir(filepath.Join(e.vol, "into")); len(left) != 0 {
		t.Errorf("unpacked %d items", len(left))
	}
}

// The rewrite is done by a program in the volume while the archive is being
// extracted, as a container's own process could. Every extract has to stay
// small whichever of the two counts it happens to read.
func TestExtractReadsTheEndOfAZipAsItWasCounted(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	path := filepath.Join(e.vol, "world-backup.zip")
	const size = 32 << 30
	sparseZip64(t, path, size, 1)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	countAt := size - int64(zip64EndLen+zip64LocLen+zipEndLen) + zip64RecordsAt

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [8]byte
		for n := uint64(0); ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			binary.LittleEndian.PutUint64(b[:], 1+n%2*2e7)
			f.WriteAt(b[:], countAt)
		}
	}()
	defer func() {
		close(stop)
		<-done
	}()

	for i := range 40 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		e.extract("world-backup.zip", "into", nil)
		runtime.ReadMemStats(&after)
		if got := after.TotalAlloc - before.TotalAlloc; got > 10<<20 {
			t.Fatalf("extract %d allocated %d bytes", i, got)
		}
	}
}

func TestZipCountCheckLooksAtEveryEndRecord(t *testing.T) {
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	le := binary.LittleEndian
	plain := func(records uint16) []byte {
		b := make([]byte, zipEndLen)
		copy(b, zipEndSig)
		le.PutUint16(b[8:], records)
		le.PutUint16(b[10:], records)
		return b
	}
	pad := func(n int) []byte { return bytes.Repeat([]byte{'x'}, n) }
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	// The zip64 records and a locator that points at them, with front before
	// them.
	zip64 := func(front []byte, records uint64) []byte {
		return cat(front, zipEndRecords(records, int64(len(front))))
	}
	// The plain end record and the locator, which point at a record that is
	// not where they say.
	lost := func(at int64) []byte { return cat(pad(100), zipEndRecords(1e9, at)[zip64EndLen:]) }

	// The plain end record counts to 65535, and the zip64 ones say so with
	// that number, so the limit is above it for them.
	const high = 70000
	cases := []struct {
		name string
		max  int
		file []byte
		want bool // refused
	}{
		{"within the limit", 100, plain(100), false},
		{"over the limit", 100, plain(101), true},
		{"no end record at all", high, pad(1000), false},
		{"shorter than an end record", high, pad(10), false},
		{"nothing", high, nil, false},
		{"zip64 within the limit", high, zip64(pad(500), 100), false},
		{"zip64 over the limit", high, zip64(pad(500), high+1), true},
		{"zip64 far over the limit", high, zip64(pad(500), 1e9), true},
		// zip.NewReader takes the last signature in the file, but another
		// search would pick one of the others.
		{"a decoy after the real one", 100, cat(plain(101), pad(30), plain(5)), true},
		{"a decoy before the real one", 100, cat(plain(5), pad(30), plain(101)), true},
		{"zip64 behind a plain end record", high, cat(zip64(pad(100), 1e9), pad(40), plain(5)), true},
		{"a locator that points outside the file", high, lost(1 << 40), false},
		{"a locator that points at nothing", high, lost(0), false},
	}
	for _, c := range cases {
		maxArchiveEntries = c.max
		_, err := checkZipCount(bytes.NewReader(c.file), int64(len(c.file)))
		var me *msg.Error
		switch {
		case c.want && (!errors.As(err, &me) || me.Code != "files.archive_too_many"):
			t.Errorf("%s: got %v, want the entry limit", c.name, err)
		case !c.want && err != nil:
			t.Errorf("%s: got %v, want it let through", c.name, err)
		}
	}

	// An archive that really holds entries goes through to the reader.
	maxArchiveEntries = 100
	var z bytes.Buffer
	zw := zip.NewWriter(&z)
	for i := range 50 {
		w, _ := zw.Create(fmt.Sprintf("f%d", i))
		w.Write([]byte("x"))
	}
	zw.Close()
	if _, err := checkZipCount(bytes.NewReader(z.Bytes()), int64(z.Len())); err != nil {
		t.Errorf("a plain archive of 50 entries: %v", err)
	}
}

// tailFile is a file of size bytes that is empty except for end, which is
// its last bytes.
type tailFile struct {
	size int64
	end  []byte
}

func (t *tailFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= t.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), t.size-off))
	clear(p[:n])
	start := t.size - int64(len(t.end))
	if lo, hi := max(off, start), min(off+int64(n), t.size); lo < hi {
		copy(p[lo-off:hi-off], t.end[lo-start:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// A program in the container can rewrite the end of its own archive between
// the check and the read that allocates for the count.
func TestZipIsReadAsItWasCounted(t *testing.T) {
	const size = 32 << 30
	const claimed = 1e7
	file := &tailFile{size: size, end: zipEndRecords(1, size-int64(zip64EndLen+zip64LocLen+zipEndLen))}
	pinned, err := checkZipCount(file, size)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(file.end[zip64RecordsAt:], claimed)

	alloc := func(r io.ReaderAt) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		zip.NewReader(r, size)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	// The claim is big enough to show without the check's reader.
	if got := alloc(file); got < 50<<20 {
		t.Fatalf("the rewritten archive made reading allocate %d bytes, the test needs it to ask for much more", got)
	}
	if got := alloc(pinned); got > 10<<20 {
		t.Errorf("reading the checked archive allocated %d bytes", got)
	}
}

// grownFile is a tailFile that, once grown is set, also has bytes past its
// size, as a file does when a program in the container makes it longer after
// it was measured.
type grownFile struct {
	tailFile
	at    int64
	past  []byte
	grown bool
}

func (g *grownFile) ReadAt(p []byte, off int64) (int, error) {
	if g.grown && off >= g.at && off-g.at < int64(len(g.past)) {
		n := copy(p, g.past[off-g.at:])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}
	return g.tailFile.ReadAt(p, off)
}

// A locator that names a place past the end of the file passes the check,
// since no record can be read from there. The file can get a record there
// after the check, though, and the reader must not find it.
func TestZipIsNotReadPastTheSizeThatWasChecked(t *testing.T) {
	const size = 32 << 30
	const claimed = 1e7
	// The locator and the end record, which point past the end of the file.
	end := zipEndRecords(1, size+100)[zip64EndLen:]
	record := zipEndRecords(claimed, 0)[:zip64EndLen]
	// A directory of 200 bytes that ends where the record begins.
	binary.LittleEndian.PutUint64(record[40:], 200)
	file := &grownFile{tailFile: tailFile{size: size, end: end}, at: size + 100, past: record}
	pinned, err := checkZipCount(file, size)
	if err != nil {
		t.Fatal(err)
	}
	file.grown = true

	alloc := func(r io.ReaderAt) uint64 {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		zip.NewReader(r, size)
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	if got := alloc(file); got < 50<<20 {
		t.Fatalf("the grown file made reading allocate %d bytes, the test needs it to ask for much more", got)
	}
	if got := alloc(pinned); got > 10<<20 {
		t.Errorf("reading the checked archive allocated %d bytes", got)
	}
}

func TestPinnedReaderAnswersFromWhatWasRead(t *testing.T) {
	data := []byte("0123456789abcdef")
	pr := &pinnedReader{r: io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))}
	got, err := pr.pin(4, 6)
	if err != nil || string(got) != "456789" {
		t.Fatalf("pin: %q %v", got, err)
	}
	copy(data, "XXXXXXXXXXXXXXXX")

	read := func(off int64, n int) string {
		b := make([]byte, n)
		m, err := pr.ReadAt(b, off)
		if err != nil || m != n {
			t.Fatalf("read %d at %d: %d %v", n, off, m, err)
		}
		return string(b)
	}
	if got := read(5, 3); got != "567" {
		t.Errorf("inside the pinned bytes: %q", got)
	}
	if got := read(0, 4); got != "XXXX" {
		t.Errorf("outside them: %q", got)
	}
	// A read that is only partly pinned is the file's, so nothing else is
	// mixed in.
	if got := read(2, 6); got != "XXXXXX" {
		t.Errorf("partly inside: %q", got)
	}
	if _, err := pr.pin(14, 8); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("pin past the end: %v", err)
	}
}

// A tail made of thousands of end records that name other places is thousands
// of pinned parts. They are kept once each and in order, so a read does not
// look through them all.
func TestPinnedPartsAreKeptOnceAndInOrder(t *testing.T) {
	data := make([]byte, 1<<16)
	for i := range data {
		data[i] = byte(1 + i%100)
	}
	pr := &pinnedReader{r: io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data)))}
	type pin struct {
		off int64
		n   int
	}
	var pins []pin
	rnd := uint32(1)
	next := func(n uint32) uint32 {
		rnd = rnd*1664525 + 1013904223
		return rnd >> 8 % n
	}
	for range 3000 {
		p := pin{int64(next(uint32(len(data) - 100))), []int{20, 56}[next(2)]}
		pins = append(pins, p)
		if _, err := pr.pin(p.off, p.n); err != nil {
			t.Fatal(err)
		}
		// The same place again is not another part.
		before := len(pr.parts)
		if _, err := pr.pin(p.off, p.n); err != nil || len(pr.parts) != before {
			t.Fatalf("pinning %v again: %d parts, was %d, %v", p, len(pr.parts), before, err)
		}
	}
	if !sort.SliceIsSorted(pr.parts, func(i, j int) bool { return pr.parts[i].off < pr.parts[j].off }) {
		t.Error("the parts are not in order")
	}
	// Whatever was pinned is answered from the parts, and what lies across
	// their ends is not: the file is changed to tell the two apart.
	copy(data, bytes.Repeat([]byte{0xAA}, len(data)))
	for range 5000 {
		off, n := int64(next(uint32(len(data)-60))), int(next(60))+1
		want := false
		for _, p := range pins {
			if off >= p.off && off+int64(n) <= p.off+int64(p.n) {
				want = true
			}
		}
		b := make([]byte, n)
		if _, err := pr.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		if got := !bytes.Equal(b, bytes.Repeat([]byte{0xAA}, n)); got != want {
			t.Fatalf("%d bytes at %d: answered from a part = %v, want %v", n, off, got, want)
		}
	}
}

// sparseFileAt makes a file of size bytes that takes the disk of the text put
// at the places, and skips the test where the file system keeps no holes.
func sparseFileAt(t *testing.T, path string, size int64, at map[int64][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Skipf("no sparse files here: %v", err)
	}
	for off, b := range at {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
	if st, err := f.Stat(); err != nil || st.Sys().(*syscall.Stat_t).Blocks*512 > size/2 {
		t.Skip("this file system does not keep holes")
	}
	if _, err := f.Seek(0, unix.SEEK_DATA); err != nil && !errors.Is(err, syscall.ENXIO) {
		t.Skipf("this file system cannot say where the holes are: %v", err)
	}
}

func rootOf(t *testing.T, dir string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

// endsAfter is a context that reports it is cancelled once it has been asked
// left times.
type endsAfter struct {
	context.Context
	left atomic.Int64
}

func (c *endsAfter) Err() error {
	if c.left.Add(-1) < 0 {
		return context.Canceled
	}
	return nil
}

// A file can be far larger than the disk it takes. Archiving it has to stop
// when whoever asked for it goes away, in the middle of its data as well.
func TestCompressStopsInsideAFileWhenTheRequestEnds(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 128<<20)
	rand.NewChaCha8([32]byte{1}).Read(data)
	sparseFileAt(t, filepath.Join(dir, "big"), 16<<30, map[int64][]byte{0: data})

	// The request ends after the work has asked a couple of hundred times, and
	// the data takes thousands of reads. A timer would depend on how fast the
	// machine compresses.
	ctx := &endsAfter{Context: context.Background()}
	ctx.left.Store(200)
	start := time.Now()
	err := packFiles(ctx, rootOf(t, dir), ".", []string{"big"}, "out.tar.gz", FileOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, noLimit)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want the cancellation", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("went on for %v after the request ended", took)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %d files, want only the big one", len(entries))
	}
}

// Packing a file reads what the file takes on disk, not its length, so a
// file of terabytes made of holes is no work.
func TestCompressReadsOnlyTheDataOfASparseFile(t *testing.T) {
	const size = 16 << 30
	dir := t.TempDir()
	sparseFileAt(t, filepath.Join(dir, "world.dat"), size, map[int64][]byte{0: []byte("head"), size - 4: []byte("tail")})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := packFiles(ctx, rootOf(t, dir), ".", []string{"world.dat"}, "out.tar.gz", FileOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}, noLimit); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "out.tar.gz"))
	if err != nil || fi.Size() > 1<<20 {
		t.Fatalf("the archive: %v, %v", fi, err)
	}
	f, _ := os.Open(filepath.Join(dir, "out.tar.gz"))
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	h, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	// Any tar reader expands the entry to the file.
	if h.Name != "world.dat" || h.Size != size || h.PAXRecords["GNU.sparse.realsize"] != strconv.Itoa(size) {
		t.Errorf("header: %q %d %v", h.Name, h.Size, h.PAXRecords)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(tr, head); err != nil || string(head) != "head" {
		t.Errorf("the entry starts with %q, %v", head, err)
	}
}

// What is packed from a sparse file unpacks to the same file.
func TestCompressedSparseFileExtractsWhole(t *testing.T) {
	const size = 40 << 20
	e := newFileEnv(t)
	at := map[int64][]byte{0: []byte("head"), size / 3: []byte("middle"), size - 4: []byte("tail")}
	sparseFileAt(t, filepath.Join(e.vol, "world.dat"), size, at)
	if code, body := e.post("compress", map[string]any{"dir": "", "paths": []string{"world.dat"}, "name": "pack"}); code != http.StatusOK {
		t.Fatalf("compress: %d %s", code, body)
	}
	if fi, err := os.Stat(filepath.Join(e.vol, "pack.tar.gz")); err != nil || fi.Size() > 1<<20 {
		t.Fatalf("the archive: %v, %v", fi, err)
	}
	os.Mkdir(filepath.Join(e.vol, "out"), 0o755)
	if code, body := e.extract("pack.tar.gz", "out", nil); code != http.StatusOK {
		t.Fatalf("extract: %d %s", code, body)
	}
	got, err := os.ReadFile(filepath.Join(e.vol, "out", "world.dat"))
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, size)
	for off, b := range at {
		copy(want[off:], b)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the file differs: %d bytes, want %d", len(got), len(want))
	}
}

// An archive can hold a sparse entry whose length is far beyond what its
// data suggests. Extracting writes it out whole, so the length is what counts
// against the room, as it does for any other entry, and it is refused before
// anything is written.
func TestExtractCountsASparseEntryByItsLength(t *testing.T) {
	const size = 1 << 40
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	src := t.TempDir()
	sparseFileAt(t, filepath.Join(src, "huge"), size, map[int64][]byte{0: []byte("head"), size - 4: []byte("tail")})
	archive, err := os.Create(filepath.Join(e.vol, "huge.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(archive)
	tw := backup.NewTarWriter(gz)
	f, _ := os.Open(filepath.Join(src, "huge"))
	defer f.Close()
	if _, _, err := tw.WriteFile(context.Background(), &tar.Header{Name: "huge", Mode: 0o644, Size: size}, f); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gz.Close()
	archive.Close()

	for name, room := range map[string]*int64{"with room": ptr(int64(1 << 20)), "without": nil} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		code, body := e.extract("huge.tar.gz", "into", room)
		runtime.ReadMemStats(&after)
		// Without a volume limit, the archive limit refuses it, or the free
		// space of the machine does first where little is left.
		want := map[string][]string{"with room": {"files.volume_full"}, "without": {"files.archive_too_large", "files.no_room"}}[name]
		if code != http.StatusConflict && code != http.StatusUnprocessableEntity || !slices.Contains(want, fileCode(body)) {
			t.Errorf("%s: %d %s", name, code, body)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("%s: took %v", name, took)
		}
		if got := after.TotalAlloc - before.TotalAlloc; got > 32<<20 {
			t.Errorf("%s: allocated %d bytes", name, got)
		}
		if left, _ := os.ReadDir(filepath.Join(e.vol, "into")); len(left) != 0 {
			t.Errorf("%s: wrote %d items", name, len(left))
		}
	}
	e.noTemp()
}

func ptr[T any](v T) *T { return &v }

// deepChain makes n folders called name inside each other below dir, one
// openat at a time, so the whole is longer than a path can be.
func deepChain(t *testing.T, dir, name string, n int) {
	t.Helper()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	for range n {
		if err := unix.Mkdirat(fd, name, 0o755); err != nil {
			unix.Close(fd)
			t.Fatal(err)
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			t.Fatal(err)
		}
		fd = next
	}
	unix.Close(fd)
}

func TestChownReachesFoldersNestedBeyondAPath(t *testing.T) {
	gid := otherGroup(t)
	dir := t.TempDir()
	deepChain(t, dir, strings.Repeat("d", 100), 60)
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	if err := chownAll(root, os.Getuid(), gid); err != nil {
		t.Fatal(err)
	}
	seen := 0
	err := engine.WalkTree(root, ".", func(n *engine.TreeNode) error {
		seen++
		if int(n.Stat.Gid) != gid {
			t.Errorf("%s was not changed", n.Path()[max(0, len(n.Path())-20):])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen != 61 {
		t.Errorf("saw %d folders, want 61", seen)
	}
}

// An archive is made from each folder's own handle, so a chain of folders
// longer than any path is packed whole, with the names it has.
func TestCompressReachesFoldersNestedBeyondAPath(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("d", 100)
	deepChain(t, dir, name, 60)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	deep := strings.TrimSuffix(strings.Repeat(name+"/", 60), "/")
	f, err := root.OpenFile(deep+"/f.txt", os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("hello")
	f.Close()
	if err := root.Symlink("f.txt", deep+"/link"); err != nil {
		t.Fatal(err)
	}

	owner := FileOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	if err := packFiles(context.Background(), root, ".", []string{name}, "out.tar.gz", owner, noLimit); err != nil {
		t.Fatal(err)
	}
	out, err := root.Open("out.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	gz, err := gzip.NewReader(out)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		got[hdr.Name] = string(body) + hdr.Linkname
	}
	// Sixty folders, the file and the link.
	if len(got) != 62 || got[deep+"/f.txt"] != "hello" || got[deep+"/link"] != "f.txt" {
		t.Errorf("%d entries, file %q, link %q", len(got), got[deep+"/f.txt"], got[deep+"/link"])
	}
}

// A volume that cannot be measured is listed as that, not left out or sent
// as empty: the panel needs to tell it from one with nothing in it, so a
// tenant cannot switch its own limit off.
func TestVolumeListSaysWhichCannotBeMeasured(t *testing.T) {
	s, f := newServer()
	var logged bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logged, nil))
	f.unmeasured = map[string]error{"deep": errors.New("open d/.../d: folders are nested too deep")}
	root := &peer.Peer{UID: 0}

	var list []volumeJSON
	for range 3 {
		rec := request(t, s, root, "GET", "/v1/volumes", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		list = nil
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]volumeJSON{}
	for _, v := range list {
		got[v.Name] = v
	}
	if got["data"].Unmeasured || got["data"].Bytes != 4096 {
		t.Errorf("the volume that was measured: %+v", got["data"])
	}
	if !got["deep"].Unmeasured || got["deep"].Bytes != 0 {
		t.Errorf("the volume that was not: %+v", got["deep"])
	}
	// The panel asks every minute, and the reason does not change.
	if n := strings.Count(logged.String(), "volume cannot be measured"); n != 1 {
		t.Errorf("logged %d times:\n%s", n, logged.String())
	}
	f.unmeasured = nil
	request(t, s, root, "GET", "/v1/volumes", "")
	f.unmeasured = map[string]error{"deep": errors.New("again")}
	request(t, s, root, "GET", "/v1/volumes", "")
	if n := strings.Count(logged.String(), "volume cannot be measured"); n != 2 {
		t.Errorf("a volume that fails again after it was fine was logged %d times in all", n)
	}
}

// A tree nested past what the walk goes through gets an answer that says so,
// from compress and from the start of a server, not a bare failure.
func TestFolderNestedTooDeepIsExplained(t *testing.T) {
	s, f := newServer()
	f.volumeDir = t.TempDir()
	was := engine.MaxWalkDepth
	engine.MaxWalkDepth = 20
	defer func() { engine.MaxWalkDepth = was }()
	deepChain(t, f.volumeDir, "d", 25)
	root := &peer.Peer{UID: 0}

	owner := fmt.Sprintf(`"uid":%d,"gid":%d`, os.Getuid(), os.Getgid())
	rec := request(t, s, root, "POST", "/v1/volumes/data/prepare", `{`+owner+`}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "files.too_deep") {
		t.Errorf("prepare: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, s, root, "POST", "/v1/volumes/data/files/compress", `{"dir":"","paths":["d"],`+owner+`}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "files.too_deep") {
		t.Errorf("compress: %d %s", rec.Code, rec.Body)
	}
}
