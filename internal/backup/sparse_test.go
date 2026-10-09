package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Caria-Core/zelie/internal/msg"
)

// extendAllocates reports whether setting the length of a file takes disk
// space for the holes in it, as it does on APFS when they are a few
// megabytes wide. A restored file cannot be checked for holes there.
func extendAllocates(t *testing.T) bool {
	t.Helper()
	p := filepath.Join(t.TempDir(), "probe")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for i := range 6 {
		f.WriteAt([]byte("x"), int64(i)*(5<<20))
	}
	if err := f.Truncate(1 << 30); err != nil {
		t.Fatal(err)
	}
	return allocated(t, p) > 16<<20
}

// deadline is the context for work that takes milliseconds when holes are
// skipped and hours when they are read, so that a change that brings the
// hours back fails the test in seconds instead of holding it up.
func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func readAt(t *testing.T, path string, off int64, n int) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := make([]byte, n)
	if _, err := f.ReadAt(b, off); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(b)
}

// A file that is mostly holes costs what its data costs, in a backup and in
// a restore, however long it is.
func TestSparseFileCostsOnlyItsData(t *testing.T) {
	const size = 1 << 40
	src, dst := t.TempDir(), t.TempDir()
	sparseFile(t, filepath.Join(src, "world.dat"), size, map[int64]string{
		0:        "head",
		size / 2: "middle",
		size - 4: "tail",
	})
	write(t, src, "small.txt", "small")

	var buf bytes.Buffer
	st, err := WriteTar(deadline(t), &buf, []Volume{{"data", openRoot(t, src)}})
	if err != nil {
		t.Fatalf("a backup of a terabyte of holes: %v", err)
	}
	// A few blocks each, however a file system rounds them up.
	if buf.Len() > 8<<20 {
		t.Errorf("the archive is %d bytes", buf.Len())
	}
	// A restore needs the room for what lands on disk, and so does the
	// number the backup keeps.
	if st.Files != 2 || st.Bytes > 8<<20 {
		t.Errorf("stats = %+v", st)
	}

	if _, err := RestoreTar(deadline(t), &buf, []Volume{{"data", openRoot(t, dst)}}); err != nil {
		t.Fatalf("a restore of a terabyte of holes: %v", err)
	}
	p := filepath.Join(dst, "world.dat")
	if fi, err := os.Stat(p); err != nil || fi.Size() != size {
		t.Fatalf("restored %v, %v; want %d bytes", fi, err, int64(size))
	}
	if got := readAt(t, p, 0, 4) + readAt(t, p, size/2, 6) + readAt(t, p, size-4, 4); got != "headmiddletail" {
		t.Errorf("content: %q", got)
	}
	if got := readAt(t, p, 1<<30, 8); got != "\x00\x00\x00\x00\x00\x00\x00\x00" {
		t.Errorf("a hole reads %q", got)
	}
	if a := allocated(t, p); a > 8<<20 {
		t.Errorf("the restored file takes %d bytes on disk", a)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "small.txt")); string(b) != "small" {
		t.Errorf("small.txt = %q", b)
	}
}

// Where the holes are and are not, the file comes back as it was.
func TestSparseShapesRoundTrip(t *testing.T) {
	const block = 1 << 16
	data := strings.Repeat("d", 3*block)
	cases := map[string]struct {
		size int64
		at   map[int64]string
	}{
		"only a hole":          {1 << 30, nil},
		"data at the start":    {1 << 30, map[int64]string{0: data}},
		"data at the end":      {1 << 30, map[int64]string{1<<30 - int64(len(data)): data}},
		"data in the middle":   {1 << 30, map[int64]string{1 << 29: data}},
		"data between holes":   {1 << 30, map[int64]string{0: data, 1 << 28: data, 1<<30 - int64(len(data)): data}},
		"a short last piece":   {1<<30 + 5, map[int64]string{1<<30 + 1: "wxyz"}},
		"data that is zeros":   {1 << 30, map[int64]string{1 << 20: strings.Repeat("\x00", 2*block)}},
		"many small pieces":    {1 << 28, manyPieces(40)},
		"data and a last hole": {1 << 30, map[int64]string{100: "abc"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			sparseFile(t, filepath.Join(src, "f"), c.size, c.at)
			mtime := time.Unix(1700000000, 123456789)
			if err := os.Chtimes(filepath.Join(src, "f"), mtime, mtime); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Join(src, "f"), 0o751); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			if _, err := WriteTar(deadline(t), &buf, []Volume{{"data", openRoot(t, src)}}); err != nil {
				t.Fatal(err)
			}
			if _, err := RestoreTar(deadline(t), &buf, []Volume{{"data", openRoot(t, dst)}}); err != nil {
				t.Fatal(err)
			}
			got := filepath.Join(dst, "f")
			fi, err := os.Stat(got)
			if err != nil || fi.Size() != c.size {
				t.Fatalf("restored %v, %v; want %d bytes", fi, err, c.size)
			}
			if fi.Mode() != 0o751 || !fi.ModTime().Equal(mtime) {
				t.Errorf("mode %v, modified %v; want 751 and %v", fi.Mode(), fi.ModTime(), mtime)
			}
			for off, text := range c.at {
				if have := readAt(t, got, off, len(text)); have != text {
					t.Errorf("at %d: %q, want %q", off, have[:min(len(have), 20)], text[:min(len(text), 20)])
				}
			}
			if a := allocated(t, got); a > 4<<20 && !extendAllocates(t) {
				t.Errorf("the restored file takes %d bytes on disk", a)
			}
		})
	}
}

func manyPieces(n int) map[int64]string {
	at := map[int64]string{}
	for i := range n {
		at[int64(i)*(5<<20)] = "piece " + strconv.Itoa(i)
	}
	return at
}

// logical is the content of a file of size bytes with the text at the places.
func logical(size int64, at map[int64]string) []byte {
	b := make([]byte, size)
	for off, text := range at {
		copy(b[off:], text)
	}
	return b
}

// backupOf makes the archive of a sparse file called data/f, and the
// file's content.
func backupOf(t *testing.T, size int64, at map[int64]string) ([]byte, []byte) {
	t.Helper()
	src := t.TempDir()
	sparseFile(t, filepath.Join(src, "f"), size, at)
	if err := os.Chmod(filepath.Join(src, "f"), 0o640); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := WriteTar(context.Background(), &buf, []Volume{{"data", openRoot(t, src)}}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), logical(size, at)
}

// The entry of a sparse file is what GNU tar writes for one, which is why
// anything that reads tar archives can expand it.
func TestSparseEntryIsInTheGNUFormat(t *testing.T) {
	const size = 40 << 20
	archive, _ := backupOf(t, size, map[int64]string{0: "head", size - 4: "tail"})
	if len(archive)%blockSize != 0 {
		t.Fatalf("the archive is %d bytes, not whole blocks", len(archive))
	}

	// What a tar reader makes of the records and the header.
	tr := tar.NewReader(bytes.NewReader(archive))
	var hdr *tar.Header
	for hdr == nil || hdr.Name != "data/f" {
		var err error
		if hdr, err = tr.Next(); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]string{
		"GNU.sparse.major":    "1",
		"GNU.sparse.minor":    "0",
		"GNU.sparse.name":     "data/f",
		"GNU.sparse.realsize": strconv.Itoa(size),
	}
	for k, v := range want {
		if hdr.PAXRecords[k] != v {
			t.Errorf("%s = %q, want %q", k, hdr.PAXRecords[k], v)
		}
	}
	for k := range hdr.PAXRecords {
		if _, ok := want[k]; !ok && k != "mtime" {
			t.Errorf("a record this format does not need: %s", k)
		}
	}
	if hdr.Size != size || hdr.Mode != 0o640 || hdr.Typeflag != tar.TypeReg {
		t.Errorf("header: size %d, mode %o, type %c", hdr.Size, hdr.Mode, hdr.Typeflag)
	}

	// The blocks themselves: the header of the records, the records, the
	// header of the file with a made-up name and the size of what follows,
	// then the map and the pieces.
	i := bytes.Index(archive, []byte("GNUSparseFile.0/f"))
	if i < 0 || i%blockSize != 0 {
		t.Fatalf("no header of a sparse file at a block, at %d", i)
	}
	h := archive[i : i+blockSize]
	if h[156] != '0' || string(h[257:263]) != "ustar\x00" {
		t.Errorf("type %q, magic %q", h[156], h[257:263])
	}
	var sum int64
	for j, c := range h {
		if j >= 148 && j < 156 {
			c = ' '
		}
		sum += int64(c)
	}
	if got, ok := parseNumber(h[148:156]); !ok || got != sum {
		t.Errorf("checksum %d, want %d", got, sum)
	}
	content, _ := parseNumber(h[124:136])
	exts, err := parseMap(archive[i+blockSize:i+2*blockSize], size)
	if err != nil {
		t.Fatal(err)
	}
	if len(exts) != 2 || exts[0].off != 0 || exts[1].end() != size {
		t.Errorf("pieces %+v", exts)
	}
	var data int64
	for _, e := range exts {
		data += e.n
	}
	if content != blockSize+data {
		t.Errorf("the entry says it has %d bytes, a map and %d bytes of data are %d", content, data, blockSize+data)
	}
	end := i + blockSize + int(content)
	if px := archive[i-2*blockSize : i-blockSize]; px[156] != 'x' {
		t.Errorf("the header of the records is of type %q, want x", px[156])
	}
	if got := string(archive[end-blockSize : end]); !strings.Contains(got, "tail") {
		t.Errorf("the entry ends with %q", got[len(got)-20:])
	}
}

// What an older release does with an archive: the tar reader's view of the
// file, with a hole for every block of zeros.
func restoreAsBefore(t *testing.T, archive []byte, dir string) {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Join(dir, strings.TrimPrefix(h.Name, "data/"))
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(name, 0o755)
		case tar.TypeReg:
			f, err := os.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if err := copySparse(f, tr, &budget{left: -1}); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}
}

// fileShapes are files of size bytes with data at some places and holes
// between them.
func fileShapes(size int64) map[string]map[int64]string {
	return map[string]map[int64]string{
		"data to the end":    {0: "head", size / 3: "middle", size - 4: "tail"},
		"a hole at the end":  {0: "head", size / 3: "middle"},
		"only a hole":        nil,
		"data at the end":    {size - 4: "tail"},
		"a single big piece": {size / 2: strings.Repeat("big", 1<<18)},
	}
}

// A backup restored by a release that has never heard of sparse entries
// gets the right file, not a file made of the map.
func TestSparseBackupRestoresWithAnOlderRelease(t *testing.T) {
	const size = 40 << 20
	for shape, at := range fileShapes(size) {
		t.Run(shape, func(t *testing.T) {
			archive, want := backupOf(t, size, at)
			dst := t.TempDir()
			restoreAsBefore(t, archive, dst)
			got, err := os.ReadFile(filepath.Join(dst, "f"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("the file differs: %d bytes, want %d", len(got), len(want))
			}
		})
	}
}

// The same for the tar a person has: they get the file back, also when it
// ends in a hole.
func TestSparseBackupExtractsWithTar(t *testing.T) {
	const size = 40 << 20
	tried := map[string]bool{}
	for _, name := range []string{"gtar", "bsdtar", "tar"} {
		exe, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if tried[exe] {
			continue
		}
		tried[exe] = true
		for shape, at := range fileShapes(size) {
			t.Run(name+"/"+shape, func(t *testing.T) {
				archive, want := backupOf(t, size, at)
				file := filepath.Join(t.TempDir(), "backup.tar")
				if err := os.WriteFile(file, archive, 0o644); err != nil {
					t.Fatal(err)
				}
				dst := t.TempDir()
				out, err := exec.Command(exe, "-xf", file, "-C", dst, "--no-same-owner").CombinedOutput()
				if err != nil {
					t.Fatalf("%s: %v\n%s", exe, err, out)
				}
				if len(out) > 0 {
					t.Logf("%s said: %s", exe, out)
				}
				got, err := os.ReadFile(filepath.Join(dst, "data", "f"))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("the file differs: %d bytes, want %d", len(got), len(want))
				}
				if exists(filepath.Join(dst, "GNUSparseFile.0")) {
					t.Error("a file made of the map")
				}
			})
		}
	}
	if len(tried) == 0 {
		t.Skip("no tar program here")
	}
}

// A tar program makes the same entry, so an archive made by hand restores the
// same way and quickly.
func TestRestoreReadsSparseEntriesOfTarPrograms(t *testing.T) {
	const size = 1 << 30
	at := map[int64]string{100: "abc", size / 2: "middle", size - 4: "tail"}
	src := t.TempDir()
	if err := os.Mkdir(filepath.Join(src, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	sparseFile(t, filepath.Join(src, "data", "g"), size, at)
	tried := map[string]bool{}
	for _, name := range []string{"gtar", "bsdtar", "tar"} {
		exe, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			exe = real
		}
		if tried[exe] {
			continue
		}
		tried[exe] = true
		t.Run(name, func(t *testing.T) {
			// bsdtar writes sparse entries in the PAX format it is told to
			// use; GNU tar has to be asked for version 1.0.
			args := []string{"--format=posix", "--sparse-version=1.0"}
			if out, err := exec.Command(exe, "--version").CombinedOutput(); err == nil && strings.Contains(string(out), "bsdtar") {
				args = []string{"--format", "pax"}
			}
			file := filepath.Join(t.TempDir(), "made.tar")
			args = append(args, "-cf", file, "-C", src, "data")
			if out, err := exec.Command(exe, args...).CombinedOutput(); err != nil {
				t.Skipf("%s %v: %v\n%s", exe, args, err, out)
			}
			made, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(made, []byte("GNU.sparse.realsize")) {
				t.Skipf("%s did not write a sparse entry", exe)
			}
			dst := t.TempDir()
			if _, err := RestoreTar(deadline(t), bytes.NewReader(made), []Volume{{"data", openRoot(t, dst)}}); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dst, "g")
			if fi, err := os.Stat(p); err != nil || fi.Size() != size {
				t.Fatalf("restored %v, %v", fi, err)
			}
			for off, text := range at {
				if got := readAt(t, p, off, len(text)); got != text {
					t.Errorf("at %d: %q, want %q", off, got, text)
				}
			}
			if a := allocated(t, p); a > 4<<20 && !extendAllocates(t) {
				t.Errorf("the restored file takes %d bytes on disk", a)
			}
		})
	}
	if len(tried) == 0 {
		t.Skip("no tar program here")
	}
}

// oldGNUSparse is the entry of a sparse file in the old GNU format, type 'S',
// which tar programs wrote before the PAX formats. The first four pieces are
// in the header and the rest in blocks after it. The entry holds the data of
// the pieces only.
func oldGNUSparse(name string, size int64, exts []extent, data string) []byte {
	pieces := func(dst []byte, exts []extent) {
		for i, e := range exts {
			putOctal(dst[i*24:i*24+12], e.off)
			putOctal(dst[i*24+12:i*24+24], e.n)
		}
	}
	b := ustarHeader('S', name, 0o644, int64(os.Getuid()), int64(os.Getgid()), int64(len(data)), 1700000000)
	copy(b[257:265], "ustar  \x00")
	pieces(b[386:482], exts[:min(len(exts), 4)])
	putOctal(b[483:495], size)
	more := exts[min(len(exts), 4):]
	if len(more) > 0 {
		b[482] = 1
	}
	copy(b[148:156], "        ")
	var sum int64
	for _, c := range b {
		sum += int64(c)
	}
	putOctal(b[148:155], sum)
	b[154], b[155] = 0, ' '

	out := b[:]
	for len(more) > 0 {
		var ext [blockSize]byte
		n := min(len(more), 21)
		pieces(ext[:504], more[:n])
		if more = more[n:]; len(more) > 0 {
			ext[504] = 1
		}
		out = append(out, ext[:]...)
	}
	out = append(out, data...)
	return append(out, make([]byte, blockPadding(int64(len(data))))...)
}

// olderPAXSparse is a sparse entry in version 0.0 or 0.1 of the PAX format:
// the pieces are in the records, and the entry holds the data of the pieces
// only.
func olderPAXSparse(minor int, name string, size int64, exts []extent, data string) []byte {
	records := []paxRecord{{"GNU.sparse.size", strconv.FormatInt(size, 10)}, {"GNU.sparse.numblocks", strconv.Itoa(len(exts))}}
	hdrName := name
	if minor == 0 {
		for _, e := range exts {
			records = append(records, paxRecord{"GNU.sparse.offset", strconv.FormatInt(e.off, 10)}, paxRecord{"GNU.sparse.numbytes", strconv.FormatInt(e.n, 10)})
		}
	} else {
		var m []string
		for _, e := range exts {
			m = append(m, strconv.FormatInt(e.off, 10), strconv.FormatInt(e.n, 10))
		}
		hdrName = "GNUSparseFile.0/" + path.Base(name)
		records = append(records, paxRecord{"GNU.sparse.name", name}, paxRecord{"GNU.sparse.map", strings.Join(m, ",")})
	}
	var recs []byte
	for _, r := range records {
		recs = append(recs, r.encode()...)
	}
	px := ustarHeader(tar.TypeXHeader, "PaxHeaders.0/f", 0o644, 0, 0, int64(len(recs)), 1700000000)
	out := append(px[:], recs...)
	out = append(out, make([]byte, blockPadding(int64(len(recs))))...)
	fh := ustarHeader(tar.TypeReg, hdrName, 0o644, int64(os.Getuid()), int64(os.Getgid()), int64(len(data)), 1700000000)
	out = append(out, fh[:]...)
	out = append(out, data...)
	return append(out, make([]byte, blockPadding(int64(len(data))))...)
}

// Go's reader expands the old GNU type and the older PAX versions, holes as
// zeros, and a restore writes the file like a regular one. An entry that is
// skipped must not leave the ones after it unread.
func TestRestoreReadsOlderSparseFormats(t *testing.T) {
	const size = 6000
	at := map[int64]string{0: "head", 1000: "one", 2000: "two", 3000: "three", 4000: "four", 5000: "five"}
	var exts []extent
	var data strings.Builder
	for _, off := range slices.Sorted(maps.Keys(at)) {
		exts = append(exts, extent{off, int64(len(at[off]))})
		data.WriteString(at[off])
	}
	formats := map[string]func(name string) []byte{
		"old GNU": func(name string) []byte { return oldGNUSparse(name, size, exts, data.String()) },
		"PAX 0.0": func(name string) []byte { return olderPAXSparse(0, name, size, exts, data.String()) },
		"PAX 0.1": func(name string) []byte { return olderPAXSparse(1, name, size, exts, data.String()) },
	}
	for format, entry := range formats {
		for _, name := range []string{"data/old", "other/old"} {
			t.Run(format+" "+name, func(t *testing.T) {
				dst := t.TempDir()
				archive := archiveOf(entry(name), plainEntry(t, "data/after", "after"))
				restored, err := RestoreTar(context.Background(), archive, []Volume{{"data", openRoot(t, dst)}})
				if err != nil || !slices.Equal(restored, []string{"data"}) {
					t.Fatalf("restored %v, %v", restored, err)
				}
				if got, _ := os.ReadFile(filepath.Join(dst, "after")); string(got) != "after" {
					t.Errorf("the entry after it was restored as %q", got)
				}
				got, err := os.ReadFile(filepath.Join(dst, "old"))
				if name == "other/old" {
					if err == nil {
						t.Error("an entry outside the volume was restored")
					}
					return
				}
				if err != nil || !bytes.Equal(got, logical(size, at)) {
					t.Errorf("the file is %d bytes, %v", len(got), err)
				}
			})
		}
	}
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// A file without holes is an ordinary entry, and so are the archives made
// before there were sparse entries.
func TestFilesWithoutHolesAreOrdinaryEntries(t *testing.T) {
	src := t.TempDir()
	fileOf(t, filepath.Join(src, "dense"), 1<<20)
	write(t, src, "empty", "")
	var buf bytes.Buffer
	if _, err := WriteTar(context.Background(), &buf, []Volume{{"data", openRoot(t, src)}}); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if isSparse(h) {
			t.Errorf("%s was written as a sparse file", h.Name)
		}
		if h.Name == "data/dense" && h.Size != 1<<20 {
			t.Errorf("dense is %d bytes in the archive", h.Size)
		}
	}
}

func TestRestoreKeepsHolesOfArchivesWithoutPieces(t *testing.T) {
	const size = 64 << 20
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := make([]byte, size)
	copy(body, "head")
	copy(body[size-4:], "tail")
	hdr := &tar.Header{Name: "data/old.dat", Typeflag: tar.TypeReg, Mode: 0o644, Size: size, Uid: os.Getuid(), Gid: os.Getgid()}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()

	dst := t.TempDir()
	if _, err := RestoreTar(context.Background(), &buf, []Volume{{"data", openRoot(t, dst)}}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dst, "old.dat")
	if fi, err := os.Stat(p); err != nil || fi.Size() != size {
		t.Fatalf("restored %v, %v", fi, err)
	}
	if readAt(t, p, 0, 4) != "head" || readAt(t, p, size-4, 4) != "tail" {
		t.Error("content differs")
	}
	if a := allocated(t, p); a > 4<<20 && !extendAllocates(t) {
		t.Errorf("takes %d bytes on disk; the holes were written out", a)
	}
}

// A sparse entry put together by hand, to say what a damaged or hostile
// archive can look like.
type rawEntry struct {
	name string // in the archive
	size int64  // the length of the file
	exts []extent
	data string // the data of the pieces, one after the other
	// text is the map as it is written, when it is not the one for exts.
	text string
	// content is the size in the entry's header, when it is not the size of
	// the map and the data.
	content int64
	// cut leaves this many bytes off the end of the entry.
	cut int
}

func (r rawEntry) bytes() []byte {
	m := sparseMap(r.exts, r.size)
	if r.text != "" {
		m = append([]byte(r.text), make([]byte, blockPadding(int64(len(r.text))))...)
	}
	content := int64(len(m) + len(r.data))
	size := content
	if r.content != 0 {
		size = r.content
	}
	hdr := &tar.Header{Name: r.name, Size: r.size, Mode: 0o644, Uid: os.Getuid(), Gid: os.Getgid(), ModTime: time.Unix(1700000000, 0)}
	out := sparseHeaders(hdr, size)
	out = append(out, m...)
	out = append(out, r.data...)
	out = append(out, make([]byte, blockPadding(content))...)
	return out[:len(out)-r.cut]
}

// plainEntry is an ordinary entry, without the end of the archive.
func plainEntry(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)), Uid: os.Getuid(), Gid: os.Getgid()}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte(body))
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// archiveOf joins entries and ends the archive.
func archiveOf(parts ...[]byte) *bytes.Reader {
	b := slices.Concat(parts...)
	return bytes.NewReader(append(b, make([]byte, 2*blockSize)...))
}

func TestRestoreRefusesDamagedSparseEntries(t *testing.T) {
	x := func(n int) string { return strings.Repeat("x", n) }
	cases := map[string]rawEntry{
		"a piece past the end":           {size: 100, exts: []extent{{50, 100}}, data: x(100)},
		"a piece that wraps around":      {size: 100, text: "1\n1\n9223372036854775807\n", data: "x"},
		"pieces out of order":            {size: 100, exts: []extent{{40, 10}, {10, 10}}, data: x(20)},
		"pieces that overlap":            {size: 100, exts: []extent{{0, 20}, {10, 20}}, data: x(40)},
		"a negative offset":              {size: 100, text: "1\n-5\n10\n", data: x(10)},
		"a piece that is no number":      {size: 100, text: "1\n0\nten\n", data: x(10)},
		"a count that is no number":      {size: 100, text: "many\n0\n10\n", data: x(10)},
		"fewer pieces than counted":      {size: 100, text: "3\n0\n10\n", data: x(10)},
		"more data than the pieces have": {size: 100, exts: []extent{{0, 10}}, data: x(20)},
		"less data than the pieces have": {size: 100, exts: []extent{{0, 20}}, data: x(10)},
		"a size that is too big":         {size: 100, exts: []extent{{0, 10}}, data: x(10), content: 5000},
		"a size that is too small":       {size: 100, exts: []extent{{0, 10}}, data: x(10), content: 600},
		"a piece cut short":              {size: 100, exts: []extent{{0, 100}}, data: x(100), cut: 3 * blockSize},
		// Read as it is, the zeros would be taken for the end of the archive
		// and the entries after them never be restored.
		"blocks after the pieces": {size: 100, exts: []extent{{0, 10}}, data: x(10) + strings.Repeat("\x00", 2*blockSize)},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			e.name = "data/f"
			dst := t.TempDir()
			write(t, dst, "keep", "keep")
			want := tree(t, dst)
			// Entries after it: a damaged one must not leave the rest to be
			// read from the wrong place.
			archive := archiveOf(e.bytes(), plainEntry(t, "data/after", "after"))
			if _, err := RestoreTar(context.Background(), archive, []Volume{{"data", openRoot(t, dst)}}); err == nil {
				t.Fatal("the archive was restored")
			}
			if got := tree(t, dst); !slices.Equal(got, want) {
				t.Errorf("volume changed: %v", got)
			}
		})
	}
}

// The headers before an entry are kept while the reader takes them in, up to
// a limit. A sparse entry whose headers run past it is refused for that, not
// for what the cut leaves of them.
func TestRestoreRefusesTooManyHeadersBeforeASparseEntry(t *testing.T) {
	var filler []byte
	// Go's reader takes a MiB for each header, so the headers are a few.
	for range 5 {
		rec := paxRecord{"comment", strings.Repeat("a", 900<<10)}.encode()
		px := ustarHeader(tar.TypeXHeader, "PaxHeaders.0/f", 0o644, 0, 0, int64(len(rec)), 1700000000)
		filler = append(filler, px[:]...)
		filler = append(filler, rec...)
		filler = append(filler, make([]byte, blockPadding(int64(len(rec))))...)
	}
	if len(filler) <= maxHeaderBytes {
		t.Fatalf("the headers are %d bytes", len(filler))
	}
	e := rawEntry{name: "data/f", size: 100, exts: []extent{{0, 10}}, data: "xxxxxxxxxx"}
	dst := t.TempDir()
	_, err := RestoreTar(context.Background(), archiveOf(filler, e.bytes()), []Volume{{"data", openRoot(t, dst)}})
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != errDamaged.Code || !strings.Contains(me.Text, "too long") {
		t.Fatalf("got %v, want %s because the headers are too long", err, errDamaged.Code)
	}
	if left, _ := os.ReadDir(dst); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// The count of pieces is the archive's to choose. The room for them is not.
func TestRestoreAllocatesForTheMapNotTheCount(t *testing.T) {
	e := rawEntry{name: "data/f", size: 1 << 40, text: "1000000000000\n0\n10\n", data: "xxxxxxxxxx"}
	dst := t.TempDir()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := RestoreTar(context.Background(), archiveOf(e.bytes()), []Volume{{"data", openRoot(t, dst)}})
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Error("the archive was restored")
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 16<<20 {
		t.Errorf("a map of a few bytes took %d bytes of memory", got)
	}
}

// A map that is longer than the reader allows is refused, not cut.
func TestRestoreRefusesAMapPastWhatTheReaderAllows(t *testing.T) {
	var text strings.Builder
	n := 150000
	text.WriteString(strconv.Itoa(n) + "\n")
	for i := range n {
		text.WriteString(strconv.Itoa(i*10) + "\n5\n")
	}
	e := rawEntry{name: "data/f", size: int64(n) * 10, text: text.String(), data: strings.Repeat("x", 5*n)}
	if len(e.text) < 1<<20 {
		t.Fatalf("the map is %d bytes", len(e.text))
	}
	dst := t.TempDir()
	if _, err := RestoreTar(context.Background(), archiveOf(e.bytes()), []Volume{{"data", openRoot(t, dst)}}); err == nil {
		t.Error("the archive was restored")
	}
	if left, _ := os.ReadDir(dst); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// Whatever happens to an entry, the ones after it are read from where they
// are: through folders that are not restored, into the staging folder and
// among plain entries.
func TestSparseEntriesAmongOthers(t *testing.T) {
	piece := func(name string) rawEntry {
		return rawEntry{name: name, size: 1000, exts: []extent{{10, 5}, {990, 10}}, data: "abcdeABCDEFGHIJ"}
	}
	archive := archiveOf(
		plainEntry(t, "data/first", "first"),
		piece("data/one").bytes(),
		piece("elsewhere/skipped").bytes(),
		plainEntry(t, "data/second", "second"),
		plainEntry(t, "elsewhere/big", strings.Repeat("z", 5<<20)),
		piece("data/.zelie-restore/staged").bytes(),
		piece("data/two").bytes(),
		plainEntry(t, "data/last", "last"),
	)
	dst := t.TempDir()
	if _, err := RestoreTar(context.Background(), archive, []Volume{{"data", openRoot(t, dst)}}); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range tree(t, dst) {
		got = append(got, strings.Fields(line)[0])
	}
	slices.Sort(got)
	if want := []string{".", "first", "last", "one", "second", "two"}; !slices.Equal(got, want) {
		t.Errorf("restored %v, want %v", got, want)
	}
	for _, name := range []string{"one", "two"} {
		p := filepath.Join(dst, name)
		if fi, err := os.Stat(p); err != nil || fi.Size() != 1000 {
			t.Fatalf("%s: %v %v", name, fi, err)
		}
		if got := readAt(t, p, 10, 5) + readAt(t, p, 990, 10); got != "abcdeABCDEFGHIJ" {
			t.Errorf("%s holds %q", name, got)
		}
		if got := readAt(t, p, 20, 4); got != "\x00\x00\x00\x00" {
			t.Errorf("%s has %q in a hole", name, got)
		}
	}
	for name, want := range map[string]string{"first": "first", "second": "second", "last": "last"} {
		if b, _ := os.ReadFile(filepath.Join(dst, name)); string(b) != want {
			t.Errorf("%s = %q", name, b)
		}
	}
}

// Sparse files among the rest of a tree: in folders, linked twice, next to
// links and plain files, in the order a walk finds them.
func TestSparseFilesInATree(t *testing.T) {
	const size = 40 << 20
	src, dst := t.TempDir(), t.TempDir()
	at := map[int64]string{7: "seven", size - 5: "end!!"}
	sparseFile(t, filepath.Join(src, "a.dat"), size, at)
	os.MkdirAll(filepath.Join(src, "world", "region"), 0o755)
	sparseFile(t, filepath.Join(src, "world", "region", "r.0.0"), size, at)
	if err := os.Link(filepath.Join(src, "world", "region", "r.0.0"), filepath.Join(src, "world", "copy.dat")); err != nil {
		t.Fatal(err)
	}
	os.Symlink("a.dat", filepath.Join(src, "link"))
	write(t, src, "world/level.dat", "level")
	write(t, src, "z.txt", "last")

	var buf bytes.Buffer
	st, err := WriteTar(deadline(t), &buf, []Volume{{"data", openRoot(t, src)}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 4 {
		t.Errorf("%d files, want 4 (the second name is a link)", st.Files)
	}
	if _, err := RestoreTar(deadline(t), &buf, []Volume{{"data", openRoot(t, dst)}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.dat", "world/region/r.0.0", "world/copy.dat"} {
		p := filepath.Join(dst, name)
		if fi, err := os.Stat(p); err != nil || fi.Size() != size {
			t.Fatalf("%s: %v, %v", name, fi, err)
		}
		if got := readAt(t, p, 7, 5) + readAt(t, p, size-5, 5); got != "sevenend!!" {
			t.Errorf("%s holds %q", name, got)
		}
	}
	a, _ := os.Stat(filepath.Join(dst, "world", "region", "r.0.0"))
	b, _ := os.Stat(filepath.Join(dst, "world", "copy.dat"))
	if !os.SameFile(a, b) {
		t.Error("the two names of one file came back as two files")
	}
	if target, _ := os.Readlink(filepath.Join(dst, "link")); target != "a.dat" {
		t.Errorf("link -> %q", target)
	}
	for name, want := range map[string]string{"world/level.dat": "level", "z.txt": "last"} {
		if b, _ := os.ReadFile(filepath.Join(dst, name)); string(b) != want {
			t.Errorf("%s = %q", name, b)
		}
	}
}

// A path in the records is judged like any other.
func TestSparseEntryStaysInTheVolume(t *testing.T) {
	for _, name := range []string{"data/../../escape", "/escape", "data/../escape"} {
		e := rawEntry{name: name, size: 100, exts: []extent{{0, 3}}, data: "abc"}
		dst := t.TempDir()
		RestoreTar(context.Background(), archiveOf(e.bytes(), plainEntry(t, "data/ok", "ok")), []Volume{{"data", openRoot(t, dst)}})
		for _, p := range []string{filepath.Join(dst, "..", "escape"), "/escape"} {
			if exists(p) {
				t.Errorf("%s made %s", name, p)
			}
		}
	}
}

// Holes are free: what counts against the room is the data.
func TestRestoreCountsDataNotHoles(t *testing.T) {
	data := strings.Repeat("x", 100<<10)
	entry := func() *bytes.Reader {
		e := rawEntry{name: "data/f", size: 1 << 40, exts: []extent{{1 << 39, int64(len(data))}}, data: data}
		return archiveOf(e.bytes())
	}
	over := errors.New("no room")
	dst := t.TempDir()
	if _, err := RestoreTarCapped(deadline(t), entry(), []Volume{{"data", openRoot(t, dst)}}, 150<<10, over); err != nil {
		t.Fatalf("a file of a terabyte with 100 KB of data in 150 KB of room: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "f")); err != nil || fi.Size() != 1<<40 {
		t.Errorf("restored %v, %v", fi, err)
	}
	dst = t.TempDir()
	if _, err := RestoreTarCapped(deadline(t), entry(), []Volume{{"data", openRoot(t, dst)}}, 50<<10, over); !errors.Is(err, over) {
		t.Errorf("with room for half the data: %v", err)
	}
	if left, _ := os.ReadDir(dst); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// A sparse entry whose data is long is stopped with the request, as a plain
// one is. The stream has far less than the entry says, so a restore that does
// not stop ends in an error that is not the cancellation.
func TestRestoreStopsInsideTheDataOfASparseEntry(t *testing.T) {
	const data = 1 << 40
	e := rawEntry{name: "data/f", size: 1 << 41, exts: []extent{{0, data}}, content: blockSize + data}
	stream := io.MultiReader(bytes.NewReader(e.bytes()), io.LimitReader(zeros{}, 16<<30))
	dst := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := RestoreTarCapped(ctx, stream, []Volume{{"data", openRoot(t, dst)}}, -1, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancellation", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("went on for %v after the context ended", took)
	}
	if left, _ := os.ReadDir(dst); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// The map of an entry that got past the tar reader is read with the room it
// needs and no more, whatever count it starts with.
func TestParseMapAllocatesForWhatIsThere(t *testing.T) {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := parseMap([]byte("1000000000000\n0\n10\n"), 1<<40)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Error("a map that counts a million million pieces was taken")
	}
	if got := after.TotalAlloc - before.TotalAlloc; got > 1<<20 {
		t.Errorf("took %d bytes of memory", got)
	}
}

// The map is as GNU tar writes it, also for a file that has no data and one
// that ends in a hole.
func TestSparseMapText(t *testing.T) {
	for name, c := range map[string]struct {
		exts []extent
		size int64
		want string
	}{
		"data to the end":   {[]extent{{0, 10}, {90, 10}}, 100, "2\n0\n10\n90\n10\n"},
		"a hole at the end": {[]extent{{0, 10}}, 100, "2\n0\n10\n100\n0\n"},
		"no data":           {nil, 100, "1\n100\n0\n"},
	} {
		m := string(sparseMap(c.exts, c.size))
		if len(m)%blockSize != 0 || !strings.HasPrefix(m, c.want) || strings.Trim(m[len(c.want):], "\x00") != "" {
			t.Errorf("%s: %q", name, m)
		}
	}
}

// What the header fields cannot hold goes in records, and a restore reads
// it from there.
func TestSparseHeadersCarryWhatDoesNotFit(t *testing.T) {
	const data = 10 << 30
	hdr := &tar.Header{
		Name: "data/f", Size: 1 << 40, Mode: 0o4755, Uid: 4000000, Gid: 3000000,
		ModTime: time.Unix(-5, 123456789),
	}
	// Only the headers and the map: the tar reader reads no more of the
	// entry until it is asked for its content.
	head := sparseHeaders(hdr, data+blockSize)
	m := sparseMap([]extent{{0, data}}, 1<<40)
	if len(m) != blockSize {
		t.Fatalf("the map is %d bytes", len(m))
	}
	rec := &recorder{r: io.MultiReader(bytes.NewReader(head), bytes.NewReader(m))}
	rec.start()
	got, err := tar.NewReader(rec).Next()
	if err != nil {
		t.Fatal(err)
	}
	if got.Uid != 4000000 || got.Gid != 3000000 || got.Size != 1<<40 || got.Mode != 0o4755 {
		t.Errorf("uid %d, gid %d, size %d, mode %o", got.Uid, got.Gid, got.Size, got.Mode)
	}
	if !got.ModTime.Equal(time.Unix(-5, 123456789)) {
		t.Errorf("mtime %v", got.ModTime)
	}
	// The size of the entry is in a record, since the field holds 8 GiB.
	if got.PAXRecords["size"] != strconv.Itoa(data+blockSize) {
		t.Errorf("records: %v", got.PAXRecords)
	}
	sf, err := readSparse(rec, rec, got)
	if err != nil {
		t.Fatal(err)
	}
	if sf.left != data || len(sf.exts) != 1 || sf.size != 1<<40 {
		t.Errorf("entry %+v", sf)
	}
}

// What can be read of an entry whose content is not all there. A file in use
// may have less in it than it did, and the archive has to stay whole.
type shortFile struct {
	size int64
	have int64 // what is there to read, from the start
}

func (s shortFile) ReadAt(p []byte, off int64) (int, error) {
	n := int(max(0, min(int64(len(p)), s.have-off)))
	for i := range n {
		p[i] = 'd'
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestSparseEntryOfAFileInUseStaysWhole(t *testing.T) {
	const size = 1 << 20
	exts := []extent{{0, 4096}, {100000, 4096}, {500000, 4096}}
	for name, have := range map[string]int64{
		"as counted":            size,
		"the file got shorter":  50000,
		"the file was emptied":  0,
		"the last piece is cut": 502000,
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := NewTarWriter(&buf)
			hdr := &tar.Header{Name: "data/f", Size: size, Mode: 0o644, Uid: os.Getuid(), Gid: os.Getgid()}
			data, changed, err := tw.writeSparse(context.Background(), hdr, exts, shortFile{size, have})
			if err != nil {
				t.Fatal(err)
			}
			if data != 3*4096 || changed != (have < size) {
				t.Errorf("data %d, changed %v", data, changed)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			dst := t.TempDir()
			if _, err := RestoreTar(context.Background(), bytes.NewReader(buf.Bytes()), []Volume{{"data", openRoot(t, dst)}}); err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(dst, "f")
			if fi, err := os.Stat(p); err != nil || fi.Size() != size {
				t.Fatalf("restored %v, %v", fi, err)
			}
			wantAt := func(off int64) string {
				if off < have {
					return "d"
				}
				return "\x00"
			}
			for _, e := range exts {
				if got := readAt(t, p, e.off, 1); got != wantAt(e.off) {
					t.Errorf("at %d: %q, want %q", e.off, got, wantAt(e.off))
				}
			}
		})
	}
}

// memFile is a file kept in memory: size bytes, of which the extents (start,
// end pairs, in order) hold data. found is called when a seek for data has
// found some, before the answer goes back, which is when a file in use can
// change under the reader.
type memFile struct {
	size    int64
	extents [][2]int64
	found   func(m *memFile, off int64)
}

func (m *memFile) Seek(pos int64, whence int) (int64, error) {
	switch whence {
	case unix.SEEK_DATA:
		for _, e := range m.extents {
			if e[1] > pos && e[1] <= m.size {
				off := max(pos, e[0])
				if m.found != nil {
					m.found(m, off)
				}
				return off, nil
			}
		}
		return 0, syscall.ENXIO
	case unix.SEEK_HOLE:
		if pos >= m.size {
			return 0, syscall.ENXIO
		}
		for _, e := range m.extents {
			if e[0] <= pos && pos < e[1] {
				return min(e[1], m.size), nil
			}
		}
		return pos, nil
	}
	return 0, syscall.EINVAL
}

// punch takes the data at start out of the file.
func (m *memFile) punch(start int64) {
	m.extents = slices.DeleteFunc(m.extents, func(e [2]int64) bool { return e[0] == start })
}

// twoStretchesAndAThird is a file of three stretches of data, whose middle
// one goes away, or the file with it, when the reader has found it and not
// yet asked how far it goes.
func twoStretchesAndAThird(change func(m *memFile)) *memFile {
	m := &memFile{size: 1 << 20, extents: [][2]int64{{0, 4096}, {100000, 104096}, {500000, 504096}}}
	m.found = func(m *memFile, off int64) {
		if off == 100000 {
			change(m)
		}
	}
	return m
}

func TestDataRunsNeverGiveAnEmptyRun(t *testing.T) {
	for name, change := range map[string]func(m *memFile){
		"the data was punched out": func(m *memFile) { m.punch(100000) },
		"the file got shorter":     func(m *memFile) { m.size = 50000 },
	} {
		m := twoStretchesAndAThird(change)
		var runs [][2]int64
		known, err := dataRuns(m, 1<<20, func(off, n int64) error {
			runs = append(runs, [2]int64{off, n})
			return nil
		})
		if err != nil || !known {
			t.Errorf("%s: %v %v", name, known, err)
		}
		for _, r := range runs {
			if r[1] <= 0 {
				t.Errorf("%s: a run of %d bytes at %d", name, r[1], r[0])
			}
		}
		want := [][2]int64{{0, 4096}, {500000, 4096}}
		if name == "the file got shorter" {
			want = want[:1]
		}
		if !slices.Equal(runs, want) {
			t.Errorf("%s: runs %v, want %v", name, runs, want)
		}
	}
}

// A map that would not fit is made shorter by filling in the holes that cost
// the least, and every piece of data is still in it.
func TestExtentListFillsTheShortestHoles(t *testing.T) {
	l := &extentList{max: 2000}
	// Pieces of 10 bytes, with holes of 100 and of 1000000.
	var runs []extent
	off := int64(0)
	for i := range 300 {
		runs = append(runs, extent{off, 10})
		l.add(off, 10)
		off += 10
		if i%10 == 9 {
			off += 1000000
		} else {
			off += 100 + int64(i%7)
		}
		if l.text > l.max {
			t.Fatalf("after %d pieces the map takes %d bytes of %d", i+1, l.text, l.max)
		}
	}
	// Whatever was filled, the data is covered and the pieces are in order.
	var end int64 = -1
	for _, e := range l.list {
		if e.n <= 0 || e.off <= end {
			t.Fatalf("pieces out of order or empty: %+v", l.list)
		}
		end = e.end()
	}
	for _, r := range runs {
		covered := false
		for _, e := range l.list {
			covered = covered || e.off <= r.off && r.end() <= e.end()
		}
		if !covered {
			t.Fatalf("the data at %d is not in the map", r.off)
		}
	}
	// The big holes are the ones left.
	var big int
	for i := 1; i < len(l.list); i++ {
		if l.list[i].off-l.list[i-1].end() >= 1000000 {
			big++
		}
	}
	if big != 29 {
		t.Errorf("%d big holes left of 29", big)
	}
	var text int
	for _, e := range l.list {
		text += e.textLen()
	}
	if text != l.text {
		t.Errorf("the map is counted as %d bytes and is %d", l.text, text)
	}
}

// extentsFile is a file with many pieces, for a test that has no use for one
// on disk. It can say where its data is, and its data is zeros.
type extentsFile struct {
	size int64
	exts []extent
	pos  int64
}

func (f *extentsFile) Seek(pos int64, whence int) (int64, error) {
	i, _ := slices.BinarySearchFunc(f.exts, pos, func(e extent, pos int64) int {
		switch {
		case e.end() <= pos:
			return -1
		default:
			return 1
		}
	})
	switch whence {
	case unix.SEEK_DATA:
		if i == len(f.exts) {
			return 0, syscall.ENXIO
		}
		return max(pos, f.exts[i].off), nil
	case unix.SEEK_HOLE:
		if i < len(f.exts) && f.exts[i].off <= pos {
			return f.exts[i].end(), nil
		}
		return pos, nil
	}
	return 0, syscall.EINVAL
}

func (f *extentsFile) ReadAt(p []byte, off int64) (int, error) {
	clear(p)
	return len(p), nil
}

// A file with more pieces than a map can list still gives an archive that
// the tar reader of an older release reads, at the cost of some holes.
func TestFragmentedFileKeepsAMapTheReaderAllows(t *testing.T) {
	// 300000 pieces of 8 bytes, with holes of about a thousand.
	f := &extentsFile{}
	var data int64
	off := int64(0)
	for i := range 300000 {
		f.exts = append(f.exts, extent{off, 8})
		data += 8
		off += 8 + 1000 + int64(i*7%97)
	}
	f.size = off + 5000
	exts, sparse, err := findExtents(context.Background(), f, "data/f", f.size, data, maxMapBytes)
	if err != nil || !sparse {
		t.Fatal(sparse, err)
	}
	if len(exts) >= len(f.exts) || len(sparseMap(exts, f.size)) > maxMapBytes+blockSize {
		t.Fatalf("%d pieces, a map of %d bytes", len(exts), len(sparseMap(exts, f.size)))
	}

	// The archive is some hundreds of MiB, so it is read as it is written.
	pr, pw := io.Pipe()
	defer pr.Close()
	type result struct {
		written int64
		err     error
	}
	done := make(chan result, 1)
	go func() {
		tw := NewTarWriter(pw)
		hdr := &tar.Header{Name: "data/f", Size: f.size, Mode: 0o644}
		written, _, err := tw.writeSparse(context.Background(), hdr, exts, f)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
		done <- result{written, err}
	}()
	// An older release reads it with Go's tar reader, which allows a map of
	// a MiB.
	tr := tar.NewReader(pr)
	h, err := tr.Next()
	if err != nil {
		t.Fatalf("the tar reader: %v", err)
	}
	if h.Size != f.size {
		t.Errorf("size %d, want %d", h.Size, f.size)
	}
	n, err := io.Copy(io.Discard, tr)
	if err != nil || n != f.size {
		t.Errorf("read %d bytes: %v", n, err)
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the entry: %v", err)
	}
	if res := <-done; res.err != nil {
		t.Fatal(res.err)
	} else if res.written < data || res.written > f.size {
		t.Errorf("wrote %d bytes of data for %d", res.written, data)
	}
}

// scattered is a file of count pieces of n bytes, the first at start and each
// of the others gap(i) bytes after the end of the one before, for a test that
// needs the file system to say where the data is without there being a file.
func scattered(start int64, count int, n int64, gap func(i int) int64) *extentsFile {
	f := &extentsFile{exts: make([]extent, 0, count)}
	off := start
	for i := range count {
		f.exts = append(f.exts, extent{off, n})
		off += n + gap(i)
	}
	f.size = off
	return f
}

func dataOf(f *extentsFile) (n int64) {
	for _, e := range f.exts {
		n += e.n
	}
	return n
}

// When the map has no more room, the shortest holes are filled in, and no
// more of them than it takes: a file a little over the limit keeps most of
// its holes.
func TestExtentListFillsOnlyWhatItMust(t *testing.T) {
	l := &extentList{max: 4000}
	var gaps []int64
	var off int64
	for i := 0; l.fill == 0; i++ {
		l.add(off, 10)
		gap := 100 + int64(i*37%1000)
		gaps = append(gaps, gap)
		off += 10 + gap
	}
	pieces := len(gaps)
	merged := pieces - len(l.list)
	if want := l.max - l.max/8; l.text > want || l.text < want-60 {
		t.Errorf("the map takes %d bytes, where %d would do", l.text, want)
	}
	if merged < 1 || merged*4 > pieces {
		t.Errorf("%d of %d holes were filled in", merged, pieces)
	}
	// The ones left are no shorter than the ones that went.
	for i := 1; i < len(l.list); i++ {
		if gap := l.list[i].off - l.list[i-1].end(); gap < l.fill {
			t.Errorf("a hole of %d is left, and holes of %d were filled in", gap, l.fill)
		}
	}
	// What was filled in is counted.
	var covered int64
	for _, e := range l.list {
		covered += e.n
	}
	if l.filled != covered-l.data {
		t.Errorf("%d bytes counted as filled, %d were", l.filled, covered-l.data)
	}
}

// A file that is a little over what a map holds keeps most of its holes. This
// one has 70000 pieces, with holes of about 20 KiB, a TiB into the file, where
// a map holds about 55000.
func TestFragmentedFileKeepsMostOfItsHoles(t *testing.T) {
	f := scattered(1<<40, 70000, 4096, func(i int) int64 { return 20<<10 + int64(i%13)<<10 })
	exts, sparse, err := findExtents(context.Background(), f, "data/f", f.size, dataOf(f), maxMapBytes)
	if err != nil || !sparse {
		t.Fatal(sparse, err)
	}
	if len(sparseMap(exts, f.size)) > maxMapBytes+blockSize {
		t.Errorf("a map of %d bytes", len(sparseMap(exts, f.size)))
	}
	if len(exts) < 45000 {
		t.Errorf("%d pieces were left of 70000, where 55000 fit", len(exts))
	}
}

// A container can make a file with the pieces of its data so far apart that
// joining them takes terabytes of zeros. The backup says so and does not read
// them.
func TestFragmentedFileThatWouldFillTooMuchIsRefused(t *testing.T) {
	// 60000 pieces of 4 KiB, 234 MiB, in a file of 16 TiB.
	f := scattered(0, 60000, 4096, func(i int) int64 { return 280<<20 + int64(i%17)<<20 })
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, err := findExtents(ctx, f, "data/world/big.bin", f.size, dataOf(f), maxMapBytes)
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != errTooFragmented.Code {
		t.Fatalf("got %v, want %s", err, errTooFragmented.Code)
	}
	if !strings.Contains(me.Text, "data/world/big.bin") {
		t.Errorf("the message does not name the file: %q", me.Text)
	}
	// With room to fill, the same pieces are written.
	if _, sparse, err := findExtents(ctx, f, "data/world/big.bin", f.size, f.size, maxMapBytes); err != nil || !sparse {
		t.Errorf("a file that takes its length on disk: %v, %v", sparse, err)
	}
}

// A database with page compression punches each 16 KiB page down to its 4 KiB
// of data. Joining the pieces takes three times what the file holds, and
// such a file must still be backed up.
func TestPageCompressedDatabaseFileIsNotRefused(t *testing.T) {
	// 10 GiB of 16 KiB pages, 2.5 GiB on disk.
	f := scattered(0, 655360, 4096, func(int) int64 { return 12 << 10 })
	if _, _, err := findExtents(context.Background(), f, "data/ibdata1", f.size, dataOf(f), maxMapBytes); err != nil {
		t.Fatal(err)
	}
	// A name a container chose is cut short in the error.
	long := scattered(0, 60000, 4096, func(i int) int64 { return 280<<20 + int64(i%17)<<20 })
	_, _, err := findExtents(context.Background(), long, strings.Repeat("a", 4000), long.size, dataOf(long), maxMapBytes)
	var me *msg.Error
	if !errors.As(err, &me) || len(me.Text) > 600 {
		t.Errorf("error %v", err)
	}
}

// A file system that compresses gives a file fewer blocks than it has data.
// That is no reason to refuse it.
func TestCompressedFileWithDataBeyondItsBlocksIsNotRefused(t *testing.T) {
	// 100000 pieces of 1 MiB, with holes of 1 MiB, which take 8 MiB on disk.
	f := scattered(0, 100000, 1<<20, func(int) int64 { return 1 << 20 })
	exts, sparse, err := findExtents(context.Background(), f, "data/f", f.size, 8<<20, maxMapBytes)
	if err != nil || !sparse || len(sparseMap(exts, f.size)) > maxMapBytes+blockSize {
		t.Errorf("%v, %v, a map of %d bytes", sparse, err, len(sparseMap(exts, f.size)))
	}
}

// The same through a file, which is what a backup, a clone and Compress
// give the writer: nothing of the file is written when it is refused, and the
// archive goes on with the next.
func TestWriteFileRefusesWhatWouldFillTooMuch(t *testing.T) {
	// A map of a few hundred bytes holds about 20 of 300 pieces.
	pieces := func(step int64) map[int64]string {
		at := map[int64]string{}
		for i := range int64(300) {
			at[i*step] = "x"
		}
		return at
	}
	write := func(t *testing.T, step int64) (*bytes.Buffer, *TarWriter, error) {
		p := filepath.Join(t.TempDir(), "f")
		size := 300 * step
		sparseFile(t, p, size, pieces(step))
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		var buf bytes.Buffer
		tw := NewTarWriter(&buf)
		tw.mapMax = 4096
		_, _, err = tw.WriteFile(context.Background(), &tar.Header{Name: "data/f", Size: size, Mode: 0o644}, f)
		return &buf, tw, err
	}

	t.Run("pieces 64 MiB apart", func(t *testing.T) {
		buf, tw, err := write(t, 64<<20)
		var me *msg.Error
		if !errors.As(err, &me) || me.Code != errTooFragmented.Code || !strings.Contains(me.Text, "data/f") {
			t.Fatalf("got %v, want %s naming the file", err, errTooFragmented.Code)
		}
		if buf.Len() != 0 {
			t.Errorf("%d bytes of the file were written", buf.Len())
		}
		if err := tw.WriteHeader(&tar.Header{Name: "data/after", Mode: 0o644}); err != nil {
			t.Errorf("the archive after it: %v", err)
		}
	})
	t.Run("pieces 256 KiB apart", func(t *testing.T) {
		buf, tw, err := write(t, 256<<10)
		if err != nil {
			t.Fatal(err)
		}
		tw.Close()
		tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(tr)
		if err != nil || int64(len(got)) != h.Size {
			t.Fatalf("read %d bytes of %d: %v", len(got), h.Size, err)
		}
		for i := range int64(300) {
			if got[i*256<<10] != 'x' {
				t.Fatalf("piece %d is not in the archive", i)
			}
		}
	})
}
