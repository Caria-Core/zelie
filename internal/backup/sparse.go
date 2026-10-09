package backup

import (
	"archive/tar"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// A file with holes goes into the archive as the stretches of it that hold
// data, so the time a backup takes follows what the file takes on disk and
// not its length. A game server or a container can make a file of terabytes
// that takes no space.
//
// The entry is the sparse format of GNU tar, in its PAX version 1.0, which
// tar, bsdtar and Go's archive/tar all expand to the whole file, so an
// archive can be restored by hand or by an older Zelie. A PAX header before
// the entry says so and gives the name and the length of the file:
//
//	GNU.sparse.major=1  GNU.sparse.minor=0
//	GNU.sparse.name=<path>  GNU.sparse.realsize=<length>
//
// The entry's own header has a made-up name, and its size is that of its
// content. The content starts with a map, text padded with zeros to a whole
// block: the number of pieces, then the offset and the length of each, a
// number to a line. It is followed by the data of the pieces, one after the
// other. A file that ends in a hole has a last piece of length 0 at its
// length, as GNU tar writes it.
//
// Go's archive/tar writer refuses these records, so the headers are written
// here, next to it.
const (
	blockSize = 512

	// maxMapBytes is as long as the map may be. Go's tar reader, which an older
	// release restores with, refuses a map of more than a MiB, so a file with
	// more pieces than that holds is written with the shortest holes between
	// them filled in.
	maxMapBytes = 1<<20 - 2*blockSize

	// maxHeaderBytes is what the headers and the map of one entry may take
	// when a restore looks for the map. Go allows a MiB for each header and a
	// MiB for the map.
	maxHeaderBytes = 4 << 20
)

// extent is a stretch of a file that holds data.
type extent struct{ off, n int64 }

func (e extent) end() int64 { return e.off + e.n }

// textLen is the bytes the extent takes in the map.
func (e extent) textLen() int { return decimalLen(e.off) + decimalLen(e.n) + 2 }

func decimalLen(v int64) int { return len(strconv.FormatInt(v, 10)) }

// dataRuns calls fn for each stretch of f below size that holds data, in
// order, and never for an empty one. It reports false, having called
// nothing, when the file system cannot say where the holes are.
//
// The file may be in use, so it can change between the two seeks that find a
// stretch. Data that went away by then is skipped, and a file that got
// shorter than the place found just ends there.
func dataRuns(f io.Seeker, size int64, fn func(off, n int64) error) (bool, error) {
	var pos int64
	for first := true; pos < size; first = false {
		off, err := f.Seek(pos, unix.SEEK_DATA)
		switch {
		case errors.Is(err, syscall.ENXIO):
			return true, nil // nothing but a hole to the end
		case first && (errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)):
			return false, nil
		case err != nil:
			return true, err
		}
		if off >= size {
			return true, nil
		}
		end, err := f.Seek(off, unix.SEEK_HOLE)
		switch {
		case errors.Is(err, syscall.ENXIO):
			return true, nil // the file got shorter
		case err != nil:
			return true, err
		}
		end = min(end, size)
		if end <= off {
			// Punched out since it was found, so there is nothing to give.
			pos = off + 1
			continue
		}
		if err := fn(off, end-off); err != nil {
			return true, err
		}
		pos = end
	}
	return true, nil
}

// extentList collects the data of a file for a map that has to stay short.
// When there are more pieces than the map has room for, the shortest holes
// between them are filled in, so what is written grows by the least it can.
type extentList struct {
	list   []extent
	text   int   // bytes the map takes, without its first line
	max    int   // the most text the map may take
	fill   int64 // holes of this length or less are filled in
	data   int64 // bytes in the pieces, as they were found
	filled int64 // bytes of holes that were filled in, which are zeros
}

func (l *extentList) add(off, n int64) {
	l.data += n
	if k := len(l.list) - 1; k >= 0 {
		if gap := off - l.list[k].end(); gap <= l.fill {
			l.text -= l.list[k].textLen()
			l.list[k].n = off + n - l.list[k].off
			l.text += l.list[k].textLen()
			l.filled += gap
			return
		}
	}
	e := extent{off, n}
	l.list = append(l.list, e)
	l.text += e.textLen()
	if l.text > l.max {
		l.shorten()
	}
}

// shorten fills in the shortest holes, as few as bring the map down to seven
// eighths of what it may take. The room that gives keeps it from being done
// again for the next few pieces.
func (l *extentList) shorten() {
	if len(l.list) < 2 {
		return
	}
	// Hole i lies after piece i, and rank[i] is its place among the holes,
	// shortest first.
	order := make([]int, len(l.list)-1)
	for i := range order {
		order[i] = i
	}
	gap := func(i int) int64 { return l.list[i+1].off - l.list[i].end() }
	slices.SortFunc(order, func(a, b int) int {
		if c := cmp.Compare(gap(a), gap(b)); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	rank := make([]int, len(order))
	for r, i := range order {
		rank[i] = r
	}
	// Each hole that is filled takes at least three bytes off the map, so
	// the more are filled the shorter it is, and a search can tell how many
	// it takes.
	textWith := func(k int) int {
		var text int
		cur := l.list[0]
		for i, e := range l.list[1:] {
			if rank[i] < k {
				cur.n = e.end() - cur.off
				continue
			}
			text += cur.textLen()
			cur = e
		}
		return text + cur.textLen()
	}
	want := l.max - l.max/8
	k := min(sort.Search(len(order), func(k int) bool { return textWith(k+1) <= want })+1, len(order))
	l.fill = max(l.fill, gap(order[k-1]))

	out := l.list[:0]
	for i, e := range l.list {
		if i > 0 && rank[i-1] < k {
			last := &out[len(out)-1]
			l.filled += e.off - last.end()
			last.n = e.end() - last.off
			continue
		}
		out = append(out, e)
	}
	l.list = out
	l.text = 0
	for _, e := range out {
		l.text += e.textLen()
	}
}

// fillLimit is the most holes may be filled in for a file, in bytes. A file
// with a lot of small pieces far apart, which a container can make with little
// disk, would otherwise have terabytes of zeros read, compressed and
// encrypted for its backup. Up to four times what the file takes on disk is
// allowed, and at least a GiB: a database with page compression punches
// each 16 KiB page down to 4 KiB, which needs three times its size filled,
// and must still be backed up.
func fillLimit(alloc, data int64) int64 { return max(4*alloc, data, 1<<30) }

// sparseMap is the map of a file of size bytes with data in the extents.
func sparseMap(exts []extent, size int64) []byte {
	count := len(exts)
	// A file that ends in a hole says where it ends.
	trailing := count == 0 || exts[count-1].end() < size
	if trailing {
		count++
	}
	b := strconv.AppendInt(nil, int64(count), 10)
	b = append(b, '\n')
	for _, e := range exts {
		b = strconv.AppendInt(b, e.off, 10)
		b = append(b, '\n')
		b = strconv.AppendInt(b, e.n, 10)
		b = append(b, '\n')
	}
	if trailing {
		b = strconv.AppendInt(b, size, 10)
		b = append(b, "\n0\n"...)
	}
	return append(b, make([]byte, blockPadding(int64(len(b))))...)
}

func blockPadding(n int64) int64 { return -n & (blockSize - 1) }

// TarWriter is a tar.Writer that can also write a file with holes.
type TarWriter struct {
	*tar.Writer
	w      io.Writer
	mapMax int // the most text a map may take
}

// NewTarWriter returns a TarWriter that writes the archive to w.
func NewTarWriter(w io.Writer) *TarWriter { return &TarWriter{tar.NewWriter(w), w, maxMapBytes} }

// WriteFile writes the header hdr and the content of f, an open regular file
// that is hdr.Size bytes long. A file that takes less disk than its length
// goes in as a sparse entry, and only its data is read. If f is not the
// length it was, because it is in use, the entry is padded or cut to the
// length of the header, so the archive stays whole, and changed says so. data
// is the number of bytes of content that went into the archive.
func (t *TarWriter) WriteFile(ctx context.Context, hdr *tar.Header, f *os.File) (data int64, changed bool, err error) {
	hdr.Typeflag = tar.TypeReg
	exts, sparse, err := holes(ctx, f, hdr.Name, hdr.Size, t.mapMax)
	if err != nil {
		return 0, false, err
	}
	if sparse {
		return t.writeSparse(ctx, hdr, exts, f)
	}
	if err := t.WriteHeader(hdr); err != nil {
		return 0, false, err
	}
	copied, err := io.CopyN(t.Writer, ctxReader{ctx, f}, hdr.Size)
	if err != nil && !errors.Is(err, io.EOF) {
		return copied, false, err
	}
	if changed = copied < hdr.Size; changed {
		if _, err := io.CopyN(t.Writer, zeros{}, hdr.Size-copied); err != nil {
			return copied, changed, err
		}
	}
	return hdr.Size, changed, nil
}

// holes finds the data of f, which is size bytes long and called name in the
// archive. It says no, not having read any, when the file takes as much disk
// as its length or the file system cannot tell where its data is.
func holes(ctx context.Context, f *os.File, name string, size int64, mapMax int) (exts []extent, sparse bool, err error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	// Blocks are what the volume's limit counts, so what a file takes is
	// what time may be spent on.
	sys, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || sys.Blocks*512 >= size {
		return nil, false, nil
	}
	exts, sparse, err = findExtents(ctx, f, name, size, sys.Blocks*512, mapMax)
	// Looking for the data moved the position.
	if _, serr := f.Seek(0, io.SeekStart); err == nil {
		err = serr
	}
	if err != nil {
		return nil, false, err
	}
	return exts, sparse, nil
}

// findExtents lists the data of f, a file of size bytes that takes alloc bytes
// on disk, in pieces that fit a map of mapMax bytes. It fails when that
// takes more zeros than fillLimit allows.
func findExtents(ctx context.Context, f io.Seeker, name string, size, alloc int64, mapMax int) ([]extent, bool, error) {
	l := &extentList{max: mapMax}
	known, err := dataRuns(f, size, func(off, n int64) error {
		l.add(off, n)
		return ctx.Err()
	})
	if err != nil || !known {
		return nil, false, err
	}
	if l.filled > fillLimit(alloc, l.data) {
		return nil, false, errTooFragmented.Err("file", clipName(name))
	}
	if len(l.list) == 1 && l.list[0].off == 0 && l.list[0].n >= size {
		return nil, false, nil
	}
	return l.list, true, nil
}

// writeSparse writes the entry for a file whose data is in exts. The pieces
// are read from r, which may no longer have them: what is missing is zeros.
func (t *TarWriter) writeSparse(ctx context.Context, hdr *tar.Header, exts []extent, r io.ReaderAt) (data int64, changed bool, err error) {
	for _, e := range exts {
		data += e.n
	}
	m := sparseMap(exts, hdr.Size)
	content := int64(len(m)) + data
	// The padding of the entry before is written now, and the rest of the
	// headers by hand, since the writer would drop the records.
	if err := t.Flush(); err != nil {
		return 0, false, err
	}
	if _, err := t.w.Write(sparseHeaders(hdr, content)); err != nil {
		return 0, false, err
	}
	if _, err := t.w.Write(m); err != nil {
		return 0, false, err
	}
	for _, e := range exts {
		got, err := io.CopyN(t.w, ctxReader{ctx, io.NewSectionReader(r, e.off, e.n)}, e.n)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, false, err
		}
		if got < e.n {
			changed = true
			if _, err := io.CopyN(t.w, zeros{}, e.n-got); err != nil {
				return 0, false, err
			}
		}
	}
	if _, err := t.w.Write(make([]byte, blockPadding(content))); err != nil {
		return 0, false, err
	}
	return data, changed, nil
}

// Largest values the fields of a tar header hold in octal.
const (
	maxOctal7  = 1<<21 - 1
	maxOctal11 = 1<<33 - 1
)

// sparseHeaders is what comes before the map and the data of a sparse entry:
// the PAX header with its records, and the header of the file with the size
// of its content.
func sparseHeaders(hdr *tar.Header, content int64) []byte {
	records := []paxRecord{
		{"GNU.sparse.major", "1"},
		{"GNU.sparse.minor", "0"},
		{"GNU.sparse.name", hdr.Name},
		{"GNU.sparse.realsize", strconv.FormatInt(hdr.Size, 10)},
	}
	mtime := hdr.ModTime.Unix()
	if mtime < 0 || mtime > maxOctal11 || hdr.ModTime.Nanosecond() != 0 {
		records = append(records, paxRecord{"mtime", paxTime(hdr.ModTime)})
		mtime = min(max(mtime, 0), maxOctal11)
	}
	uid, gid := int64(hdr.Uid), int64(hdr.Gid)
	if uid > maxOctal7 || uid < 0 {
		records = append(records, paxRecord{"uid", strconv.FormatInt(uid, 10)})
		uid = min(max(uid, 0), maxOctal7)
	}
	if gid > maxOctal7 || gid < 0 {
		records = append(records, paxRecord{"gid", strconv.FormatInt(gid, 10)})
		gid = min(max(gid, 0), maxOctal7)
	}
	size := content
	if size > maxOctal11 {
		records = append(records, paxRecord{"size", strconv.FormatInt(size, 10)})
		size = 0
	}
	var recs []byte
	for _, r := range records {
		recs = append(recs, r.encode()...)
	}

	// Names that are made up, as GNU tar makes them. The real one is in the
	// records.
	base := path.Base(hdr.Name)
	mode := hdr.Mode & 0o7777
	var out []byte
	px := ustarHeader(tar.TypeXHeader, "PaxHeaders.0/"+base, 0o644, 0, 0, int64(len(recs)), mtime)
	out = append(out, px[:]...)
	out = append(out, recs...)
	out = append(out, make([]byte, blockPadding(int64(len(recs))))...)
	fh := ustarHeader(tar.TypeReg, "GNUSparseFile.0/"+base, mode, uid, gid, size, mtime)
	return append(out, fh[:]...)
}

type paxRecord struct{ key, value string }

// encode is the record as the PAX format has it: its own length, a space,
// the key, a "=", the value and a newline.
func (r paxRecord) encode() string {
	size := len(r.key) + len(r.value) + 3
	size += len(strconv.Itoa(size))
	rec := strconv.Itoa(size) + " " + r.key + "=" + r.value + "\n"
	// The length may have gained a digit by being counted.
	if len(rec) != size {
		rec = strconv.Itoa(len(rec)) + " " + r.key + "=" + r.value + "\n"
	}
	return rec
}

// paxTime is a time as the records mtime, atime and ctime have it: seconds,
// and the fraction after a point when there is one.
func paxTime(t time.Time) string {
	secs, nsecs := t.Unix(), t.Nanosecond()
	if nsecs == 0 {
		return strconv.FormatInt(secs, 10)
	}
	sign := ""
	if secs < 0 {
		sign = "-"
		secs, nsecs = -(secs + 1), 1e9-nsecs
	}
	return strings.TrimRight(fmt.Sprintf("%s%d.%09d", sign, secs, nsecs), "0")
}

// ustarHeader is a header block of the ustar format. The numbers are within
// what its fields hold.
func ustarHeader(typ byte, name string, mode, uid, gid, size, mtime int64) (b [blockSize]byte) {
	copy(b[0:100], name)
	putOctal(b[100:108], mode)
	putOctal(b[108:116], uid)
	putOctal(b[116:124], gid)
	putOctal(b[124:136], size)
	putOctal(b[136:148], mtime)
	b[156] = typ
	copy(b[257:265], "ustar\x0000")
	putOctal(b[329:337], 0)
	putOctal(b[337:345], 0)
	// The checksum counts its own field as spaces.
	copy(b[148:156], "        ")
	var sum int64
	for _, c := range b {
		sum += int64(c)
	}
	putOctal(b[148:155], sum)
	b[154], b[155] = 0, ' '
	return b
}

// putOctal writes v in all of dst but its last byte, which is a zero.
func putOctal(dst []byte, v int64) {
	s := strconv.FormatInt(v, 8)
	for i := range dst {
		dst[i] = '0'
	}
	copy(dst[len(dst)-1-len(s):], s)
	dst[len(dst)-1] = 0
}

// parseNumber reads a number of a header field: octal, or base 256 when the
// top bit is set, which is how a field that is too small for the number is
// written by some tools.
func parseNumber(b []byte) (int64, bool) {
	if len(b) > 0 && b[0]&0x80 != 0 {
		if b[0]&0x40 != 0 {
			return 0, false // negative
		}
		v := int64(b[0] & 0x3f)
		for _, c := range b[1:] {
			if v > math.MaxInt64>>8 {
				return 0, false
			}
			v = v<<8 | int64(c)
		}
		return v, true
	}
	s := strings.Trim(string(b), " \x00")
	if s == "" {
		return 0, true
	}
	v, err := strconv.ParseInt(s, 8, 64)
	return v, err == nil && v >= 0
}

// isSparse says whether hdr is the entry of a sparse file in the format above,
// which tar.Reader would expand.
func isSparse(hdr *tar.Header) bool {
	return hdr.Typeflag == tar.TypeReg && hdr.PAXRecords["GNU.sparse.major"] == "1" && hdr.PAXRecords["GNU.sparse.minor"] == "0"
}

// recorder keeps what is read through it from the time it is started, up to
// a limit. Go's tar reader reads the map of a sparse entry itself and then
// hands out the file with the holes written out as zeros, which for a file
// of terabytes is more than a restore can do. It does not say where the holes
// are, but the blocks it read for the map are here.
type recorder struct {
	r    io.Reader
	buf  []byte
	on   bool
	over bool // more was read than the limit
}

func (c *recorder) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.on && n > 0 {
		if len(c.buf)+n > maxHeaderBytes {
			c.over = true
		} else {
			c.buf = append(c.buf, p[:n]...)
		}
	}
	return n, err
}

func (c *recorder) start() {
	c.buf, c.on, c.over = c.buf[:0], true, false
}

// sparseEntry is what is left of a sparse entry once its map is read: the
// pieces, which are read from the archive as they come.
type sparseEntry struct {
	r    io.Reader // the archive, at the first piece
	exts []extent
	size int64 // the length the file has when restored
	left int64 // bytes of the entry still in the archive, the padding too
}

// readSparse finds the map of the entry hdr in the blocks the reader took
// for it, which are in rec, and checks it. The archive, which is r, is then
// at the first piece.
func readSparse(rec *recorder, r io.Reader, hdr *tar.Header) (*sparseEntry, error) {
	if rec.over {
		return nil, errDamaged.Err("detail", "the headers of a sparse file are too long")
	}
	// The padding of the entry before comes first; the rest is whole blocks.
	blocks := rec.buf[len(rec.buf)%blockSize:]
	var at int
	for {
		if len(blocks) < (at+1)*blockSize {
			return nil, errDamaged.Err("detail", "a sparse file has no header")
		}
		h := blocks[at*blockSize : (at+1)*blockSize]
		n, ok := parseNumber(h[124:136])
		at++
		switch h[156] {
		case tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			if !ok || n > maxHeaderBytes {
				return nil, errDamaged.Err("detail", "a sparse file has a header that is too long")
			}
			at += int((n + blockSize - 1) / blockSize)
			continue
		}
		break
	}
	// What the entry has in the archive is in its header, or in a record
	// when it does not fit there. The reader has put the length of the file
	// in place of it.
	content, ok := parseNumber(blocks[(at-1)*blockSize+124 : (at-1)*blockSize+136])
	if v, has := hdr.PAXRecords["size"]; has {
		var err error
		content, err = strconv.ParseInt(v, 10, 64)
		ok = err == nil && content >= 0
	}
	if !ok {
		return nil, errDamaged.Err("detail", "a sparse file has no size")
	}
	mapBlocks := blocks[at*blockSize:]
	exts, err := parseMap(mapBlocks, hdr.Size)
	if err != nil {
		return nil, err
	}
	var data int64
	for _, e := range exts {
		data += e.n
	}
	if content != int64(len(mapBlocks))+data {
		return nil, errDamaged.Err("detail", "a sparse file is not as long as its pieces")
	}
	return &sparseEntry{r: r, exts: exts, size: hdr.Size, left: data + blockPadding(content)}, nil
}

// parseMap reads the map of a file of size bytes. b is the blocks of the map
// and nothing else: the reader stopped at the end of the one with the last
// line.
func parseMap(b []byte, size int64) ([]extent, error) {
	bad := func(what string) ([]extent, error) {
		return nil, errDamaged.Err("detail", "the map of a sparse file "+what)
	}
	next := func() (int64, bool) {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return 0, false
		}
		v, err := strconv.ParseInt(string(b[:i]), 10, 64)
		b = b[i+1:]
		return v, err == nil && v >= 0
	}
	count, ok := next()
	// A piece takes at least four bytes of the map, which is all there is to
	// ask for room by.
	if !ok || count > int64(len(b))/4 {
		return bad("is damaged")
	}
	exts := make([]extent, 0, count)
	var end int64
	for range count {
		off, ok1 := next()
		n, ok2 := next()
		if !ok1 || !ok2 {
			return bad("is damaged")
		}
		if off < end || off > size || n > size-off {
			return bad("has a piece out of place")
		}
		end = off + n
		if n > 0 {
			exts = append(exts, extent{off, n})
		}
	}
	return exts, nil
}

// copyPieces restores a sparse entry into f, which is empty. Only the data is
// written, and a block of zeros in it is left as a hole, so a restore takes
// time and room in proportion to what the file takes.
func copyPieces(f *os.File, sf *sparseEntry, b *budget) error {
	for _, e := range sf.exts {
		if _, err := f.Seek(e.off, io.SeekStart); err != nil {
			return err
		}
		got, _, err := copyBlocks(f, io.LimitReader(sf.r, e.n), b)
		sf.left -= got
		if err != nil {
			return err
		}
		if got < e.n {
			return io.ErrUnexpectedEOF
		}
	}
	// Holes at the end, and the blocks of zeros that were not written.
	return f.Truncate(sf.size)
}

// finish reads what is left of the entry, so the archive is at the next one.
func (s *sparseEntry) finish() error {
	n, err := io.CopyN(io.Discard, s.r, s.left)
	s.left -= n
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

// clipName keeps a file name a container chose short enough for an error
// and the logs.
func clipName(name string) string {
	if len(name) > 200 {
		return name[:200] + "…"
	}
	return name
}
