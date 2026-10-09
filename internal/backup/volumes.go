package backup

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/Caria-Core/zelie/internal/engine"
)

// Volume is one of an app's volumes in a backup. Dir is the folder it gets
// in the archive: where the app sees it, without the leading slash, so an
// archive opened by hand reads like the app's own file tree.
type Volume struct {
	Dir  string
	Root *os.Root
}

// Folders a restore works in, at the top of each volume. They are left
// out of backups: one can only be there after a restore broke off.
const (
	staging = ".zelie-restore"
	old     = ".zelie-old"
)

// TarStats says what went into an archive.
type TarStats struct {
	Files int
	// Bytes is the size of the files, which a restore needs on disk.
	Bytes int64
	// Changed counts files that changed while they were copied, which
	// only happens when the app keeps running during its backup.
	Changed int
}

// WriteTar writes the volumes into one tar archive. File owners are kept
// as numbers: they are the container's users, which mean nothing on the
// host. Sockets, pipes and devices are left out, and so are extended
// attributes.
func WriteTar(ctx context.Context, w io.Writer, vols []Volume) (TarStats, error) {
	tw := NewTarWriter(w)
	var st TarStats
	for _, v := range vols {
		t := tarWalk{ctx: ctx, tw: tw, dir: v.Dir, links: map[inode]string{}, stats: &st}
		if err := engine.WalkTree(v.Root, ".", t.visit); err != nil {
			return st, err
		}
	}
	return st, tw.Close()
}

type inode struct{ dev, ino uint64 }

type tarWalk struct {
	ctx   context.Context
	tw    *TarWriter
	dir   string
	links map[inode]string
	stats *TarStats
}

// visit writes one item. A folder is written before what is in it, and every
// item is reached from the folder that holds it, so a tree that is nested
// deep costs no more than one that is not.
func (t *tarWalk) visit(n *engine.TreeNode) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	name := n.Path()
	if name == staging || name == old {
		return fs.SkipDir
	}
	hdr := header(n.Info(), n.Stat.Uid, n.Stat.Gid)
	hdr.Name = path.Join(t.dir, name)
	switch n.Stat.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		hdr.Name += "/"
		return t.tw.WriteHeader(hdr)
	case unix.S_IFLNK:
		target, err := n.Readlink()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // removed while the backup ran
		}
		if err != nil {
			return err
		}
		hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
		return t.tw.WriteHeader(hdr)
	case unix.S_IFREG:
		if n.Stat.Nlink > 1 {
			key := inode{uint64(n.Stat.Dev), uint64(n.Stat.Ino)}
			if first, ok := t.links[key]; ok {
				hdr.Typeflag, hdr.Linkname = tar.TypeLink, first
				return t.tw.WriteHeader(hdr)
			}
			t.links[key] = hdr.Name
		}
		return t.file(n, hdr)
	}
	return nil
}

// file copies one file. The header already carries its size; if the file
// shrinks or grows meanwhile, the copy is padded or cut to that size so the
// archive stays whole, and the file is counted as changed.
func (t *tarWalk) file(n *engine.TreeNode, hdr *tar.Header) error {
	before := n.Info()
	// Not through a link and not waiting for a writer, in case the file
	// became one or a pipe since it was looked at.
	f, err := n.Open()
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return nil // removed, or replaced by a link or a folder
	}
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return err
	}
	hdr.Size = before.Size()
	data, changed, err := t.tw.WriteFile(t.ctx, hdr, f)
	if err != nil {
		return err
	}
	if after, err := f.Stat(); err == nil && (after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime())) {
		changed = true
	}
	t.stats.Files++
	t.stats.Bytes += data
	if changed {
		t.stats.Changed++
	}
	return nil
}

// ctxReader stops reading once ctx is done. A file can be much larger than the
// space it takes, so checking between files is not enough to stop a copy.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func header(fi fs.FileInfo, uid, gid uint32) *tar.Header {
	mode := int64(fi.Mode().Perm())
	if fi.Mode()&fs.ModeSetuid != 0 {
		mode |= 0o4000
	}
	if fi.Mode()&fs.ModeSetgid != 0 {
		mode |= 0o2000
	}
	if fi.Mode()&fs.ModeSticky != 0 {
		mode |= 0o1000
	}
	return &tar.Header{Typeflag: tar.TypeDir, Mode: mode, ModTime: fi.ModTime(), Uid: int(uid), Gid: int(gid), Format: tar.FormatPAX}
}

// RestoreTar replaces the volumes' files with what the archive holds.
// Everything is unpacked next to the current files first; only when the
// whole archive is in are the old files swapped out, so a broken archive
// changes nothing. Folders in the archive with no volume are skipped, and a
// volume with no folder in the archive is left as it is. It returns the
// folders that were restored.
//
// The archive is not trusted. Every path stays inside its volume, and
// nothing is written through a symbolic link the archive itself made.
func RestoreTar(ctx context.Context, r io.Reader, vols []Volume) ([]string, error) {
	return RestoreTarCapped(ctx, r, vols, -1, nil)
}

// RestoreTarCapped is RestoreTar that stops with over once it has had to
// write more than limit bytes of file data. Holes are not written, so the
// limit counts what lands on disk, not the sizes in the archive. A negative
// limit means no limit.
func RestoreTarCapped(ctx context.Context, r io.Reader, vols []Volume, limit int64, over error) ([]string, error) {
	budget := &budget{left: limit, over: over}
	unpacks := make([]*unpack, len(vols))
	for i, v := range vols {
		if err := v.Root.RemoveAll(staging); err != nil {
			return nil, err
		}
		if err := v.Root.Mkdir(staging, 0o700); err != nil {
			return nil, err
		}
		unpacks[i] = &unpack{root: v.Root, symlinks: map[string]bool{}, budget: budget}
	}
	cleanUp := func() {
		for _, v := range vols {
			v.Root.RemoveAll(staging)
		}
	}
	rec := &recorder{r: r}
	tr := tar.NewReader(rec)
	for {
		if err := ctx.Err(); err != nil {
			cleanUp()
			return nil, err
		}
		// What is left of the entry before is read now, so that what the
		// recorder keeps from here is the headers of the next one. For an
		// entry in an older sparse format, which Go expands, that means its
		// holes are read as zeros even when it was skipped. The read stops
		// when the restore is cancelled.
		if _, err := io.Copy(io.Discard, ctxReader{ctx, tr}); err != nil {
			cleanUp()
			if cerr := ctx.Err(); cerr != nil {
				return nil, cerr
			}
			return nil, errDamaged.Err("detail", err.Error())
		}
		rec.start()
		hdr, err := tr.Next()
		rec.on = false
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanUp()
			return nil, errDamaged.Err("detail", err.Error())
		}
		var body io.Reader = ctxReader{ctx, tr}
		var sparse *sparseEntry
		if isSparse(hdr) {
			// The reader would write the holes out as zeros. It has read the
			// map, so what follows in the archive is the pieces, which are
			// read here and the reader starts again after them.
			if sparse, err = readSparse(rec, ctxReader{ctx, rec}, hdr); err != nil {
				cleanUp()
				return nil, err
			}
		}
		if err := restoreEntry(vols, unpacks, hdr, body, sparse); err != nil {
			cleanUp()
			return nil, err
		}
		if sparse != nil {
			if err := sparse.finish(); err != nil {
				cleanUp()
				switch {
				case ctx.Err() != nil:
					return nil, ctx.Err()
				case errors.Is(err, io.ErrUnexpectedEOF):
					return nil, errCutShort.Err("file", hdr.Name)
				}
				return nil, errDamaged.Err("detail", err.Error())
			}
			tr = tar.NewReader(rec)
		}
	}
	var restored []string
	for i, v := range vols {
		if !unpacks[i].found {
			v.Root.RemoveAll(staging)
			continue
		}
		if err := unpacks[i].swap(); err != nil {
			cleanUp()
			return restored, err
		}
		restored = append(restored, v.Dir)
	}
	return restored, nil
}

// restoreEntry puts one entry in the volume it belongs to, if any.
func restoreEntry(vols []Volume, unpacks []*unpack, hdr *tar.Header, body io.Reader, sparse *sparseEntry) error {
	i, rel, ok := owning(vols, hdr.Name)
	if !ok {
		return nil
	}
	if hdr.Typeflag == tar.TypeLink {
		j, target, ok := owning(vols, hdr.Linkname)
		if !ok || j != i || target == "." {
			return errLinkOut.Err("file", hdr.Name, "target", hdr.Linkname)
		}
		hdr.Linkname = target
	}
	return unpacks[i].entry(hdr, rel, body, sparse)
}

// owning finds the volume an archive path belongs to and the path inside
// it: "." for the volume itself.
func owning(vols []Volume, name string) (int, string, bool) {
	name = strings.TrimSuffix(name, "/")
	if name == "" || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return 0, "", false
	}
	for i, v := range vols {
		if name == v.Dir {
			return i, ".", true
		}
		if rel, ok := strings.CutPrefix(name, v.Dir+"/"); ok {
			return i, rel, true
		}
	}
	return 0, "", false
}

type unpack struct {
	root     *os.Root
	found    bool
	top      *tar.Header // the volume folder's own owner and mode
	symlinks map[string]bool
	dirs     []dirTimes
	budget   *budget
}

// budget is the disk space the files of one restore may still take.
type budget struct {
	left int64
	over error
}

func (b *budget) take(n int64) error {
	if b.left < 0 {
		return nil
	}
	if n > b.left {
		return b.over
	}
	b.left -= n
	return nil
}

// copySparse writes body to f and leaves a hole for every block of zeros, so
// a sparse file in the archive does not grow to its full size on disk. The
// final truncate keeps a hole at the end in the file's length.
func copySparse(f *os.File, body io.Reader, b *budget) error {
	n, hole, err := copyBlocks(f, body, b)
	if err == nil && hole {
		err = f.Truncate(n)
	}
	return err
}

// copyBlocks is copySparse without the truncate. It says how many bytes
// body held and whether it ended in a hole, which the file is then short by.
func copyBlocks(f *os.File, body io.Reader, b *budget) (n int64, hole bool, err error) {
	buf := make([]byte, 64<<10)
	for {
		m, rerr := fill(body, buf)
		if m > 0 {
			if isZero(buf[:m]) {
				if _, err := f.Seek(int64(m), io.SeekCurrent); err != nil {
					return n, hole, err
				}
				hole = true
			} else {
				if err := b.take(int64(m)); err != nil {
					return n, hole, err
				}
				if _, err := f.Write(buf[:m]); err != nil {
					return n, hole, err
				}
				hole = false
			}
			n += int64(m)
		}
		if rerr == io.EOF {
			return n, hole, nil
		}
		if rerr != nil {
			return n, hole, rerr
		}
	}
}

// fill reads until buf is full or body ends. Unlike io.ReadFull it returns
// io.EOF, not io.ErrUnexpectedEOF, for a short last block, so a body that
// is cut short can still be told from one that ended.
func fill(body io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := body.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func isZero(p []byte) bool {
	for _, c := range p {
		if c != 0 {
			return false
		}
	}
	return true
}

type dirTimes struct {
	name string
	hdr  *tar.Header
}

// entry makes one entry of the archive. A sparse file is given as sparse,
// and its body is not read.
func (u *unpack) entry(hdr *tar.Header, rel string, body io.Reader, sparse *sparseEntry) error {
	u.found = true
	if rel == "." {
		if hdr.Typeflag == tar.TypeDir {
			u.top = hdr
		}
		return nil
	}
	if rel == staging || rel == old || strings.HasPrefix(rel, staging+"/") || strings.HasPrefix(rel, old+"/") {
		return nil
	}
	if err := u.throughLink(rel); err != nil {
		return err
	}
	name := staging + "/" + rel
	if err := u.root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := u.root.Mkdir(name, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		u.dirs = append(u.dirs, dirTimes{name, hdr})
		return nil
	case tar.TypeReg, tar.TypeGNUSparse:
		// The reader has expanded the old GNU sparse type ('S') already, so
		// its body is read like a regular file's, holes as zeros.
		f, err := u.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if sparse != nil {
			err = copyPieces(f, sparse, u.budget)
		} else {
			err = copySparse(f, body, u.budget)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return errCutShort.Err("file", hdr.Name)
		}
		if err != nil {
			return err
		}
		return u.attrs(name, hdr)
	case tar.TypeSymlink:
		if err := u.root.Symlink(hdr.Linkname, name); err != nil {
			return err
		}
		u.symlinks[rel] = true
		return u.root.Lchown(name, hdr.Uid, hdr.Gid)
	case tar.TypeLink:
		// RestoreTar made the target a path inside this volume.
		if err := u.throughLink(hdr.Linkname + "/x"); err != nil {
			return err
		}
		return u.root.Link(staging+"/"+hdr.Linkname, name)
	}
	return nil
}

// throughLink refuses a path under a symbolic link the archive made. Our
// own archives never have one; writing through it could reach files the
// restore does not mean to touch.
func (u *unpack) throughLink(rel string) error {
	for p := path.Dir(rel); p != "."; p = path.Dir(p) {
		if u.symlinks[p] {
			return errThroughLink.Err("link", p)
		}
	}
	return nil
}

func (u *unpack) attrs(name string, hdr *tar.Header) error {
	if err := u.root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
		return err
	}
	// After the owner: changing the owner clears the set-user-ID bit.
	if err := u.root.Chmod(name, fileMode(hdr.Mode)); err != nil {
		return err
	}
	return u.root.Chtimes(name, hdr.ModTime, hdr.ModTime)
}

func fileMode(m int64) fs.FileMode {
	mode := fs.FileMode(m & 0o777)
	if m&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if m&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if m&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// swap puts the unpacked files in place of the old ones. If a rename
// fails, the ones already done are undone.
func (u *unpack) swap() error {
	// Folders get their owner, mode and time last, deepest first: writing
	// into a folder changes its time, and its mode may forbid writing.
	for _, d := range slices.Backward(u.dirs) {
		if err := u.attrs(d.name, d.hdr); err != nil {
			return err
		}
	}
	if err := u.root.RemoveAll(old); err != nil {
		return err
	}
	if err := u.root.Mkdir(old, 0o700); err != nil {
		return err
	}
	current, err := readNames(u.root, ".")
	if err != nil {
		return err
	}
	unpacked, err := readNames(u.root, staging)
	if err != nil {
		return err
	}
	type move struct{ from, to string }
	var done []move
	undo := func() {
		for _, m := range slices.Backward(done) {
			u.root.Rename(m.to, m.from)
		}
	}
	for _, n := range current {
		if n == staging || n == old {
			continue
		}
		if err := u.root.Rename(n, old+"/"+n); err != nil {
			undo()
			return err
		}
		done = append(done, move{n, old + "/" + n})
	}
	for _, n := range unpacked {
		if err := u.root.Rename(staging+"/"+n, n); err != nil {
			undo()
			return err
		}
		done = append(done, move{staging + "/" + n, n})
	}
	if u.top != nil {
		if err := u.root.Lchown(".", u.top.Uid, u.top.Gid); err != nil {
			return err
		}
		if err := u.root.Chmod(".", fileMode(u.top.Mode)); err != nil {
			return err
		}
	}
	u.root.Remove(staging)
	if err := u.root.RemoveAll(old); err != nil {
		return err
	}
	if u.top != nil {
		u.root.Chtimes(".", u.top.ModTime, u.top.ModTime)
	}
	return nil
}

func readNames(root *os.Root, dir string) ([]string, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}
