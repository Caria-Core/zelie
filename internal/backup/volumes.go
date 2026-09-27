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
	tw := tar.NewWriter(w)
	var st TarStats
	for _, v := range vols {
		t := tarWalk{ctx: ctx, tw: tw, root: v.Root, dir: v.Dir, links: map[inode]string{}, stats: &st}
		if err := t.walk("."); err != nil {
			return st, err
		}
	}
	return st, tw.Close()
}

type inode struct{ dev, ino uint64 }

type tarWalk struct {
	ctx   context.Context
	tw    *tar.Writer
	root  *os.Root
	dir   string
	links map[inode]string
	stats *TarStats
}

func (t *tarWalk) walk(name string) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	fi, err := t.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // removed while the backup ran
	}
	if err != nil {
		return err
	}
	hdr := header(fi)
	hdr.Name = path.Join(t.dir, name)
	switch {
	case fi.IsDir():
		hdr.Name += "/"
		if err := t.tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := t.root.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		entries, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, e := range entries {
			if name == "." && (e.Name() == staging || e.Name() == old) {
				continue
			}
			if err := t.walk(path.Join(name, e.Name())); err != nil {
				return err
			}
		}
		return nil
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := t.root.Readlink(name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
		return t.tw.WriteHeader(hdr)
	case fi.Mode().IsRegular():
		if sys, ok := fi.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
			key := inode{uint64(sys.Dev), uint64(sys.Ino)}
			if first, ok := t.links[key]; ok {
				hdr.Typeflag, hdr.Linkname = tar.TypeLink, first
				return t.tw.WriteHeader(hdr)
			}
			t.links[key] = hdr.Name
		}
		return t.file(name, hdr, fi)
	}
	return nil
}

// file copies one file. The header already carries its size; if the file
// shrinks or grows meanwhile, the copy is padded or cut to that size so the
// archive stays whole, and the file is counted as changed.
func (t *tarWalk) file(name string, hdr *tar.Header, before fs.FileInfo) error {
	// Non-blocking, in case the file became a pipe since it was looked at.
	f, err := t.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return err
	}
	hdr.Typeflag, hdr.Size = tar.TypeReg, before.Size()
	if err := t.tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.CopyN(t.tw, f, hdr.Size)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	changed := n < hdr.Size
	if changed {
		if _, err := io.CopyN(t.tw, zeros{}, hdr.Size-n); err != nil {
			return err
		}
	}
	if after, err := f.Stat(); err == nil && (after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime())) {
		changed = true
	}
	t.stats.Files++
	t.stats.Bytes += hdr.Size
	if changed {
		t.stats.Changed++
	}
	return nil
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

func header(fi fs.FileInfo) *tar.Header {
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
	hdr := &tar.Header{Typeflag: tar.TypeDir, Mode: mode, ModTime: fi.ModTime(), Format: tar.FormatPAX}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		hdr.Uid, hdr.Gid = int(sys.Uid), int(sys.Gid)
	}
	return hdr
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
	unpacks := make([]*unpack, len(vols))
	for i, v := range vols {
		if err := v.Root.RemoveAll(staging); err != nil {
			return nil, err
		}
		if err := v.Root.Mkdir(staging, 0o700); err != nil {
			return nil, err
		}
		unpacks[i] = &unpack{root: v.Root, symlinks: map[string]bool{}}
	}
	cleanUp := func() {
		for _, v := range vols {
			v.Root.RemoveAll(staging)
		}
	}
	tr := tar.NewReader(r)
	for {
		if err := ctx.Err(); err != nil {
			cleanUp()
			return nil, err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanUp()
			return nil, failed("the backup is damaged: %v", err)
		}
		i, rel, ok := owning(vols, hdr.Name)
		if !ok {
			continue
		}
		if hdr.Typeflag == tar.TypeLink {
			j, target, ok := owning(vols, hdr.Linkname)
			if !ok || j != i || target == "." {
				cleanUp()
				return nil, failed("the backup links %s to %s, outside its volume", hdr.Name, hdr.Linkname)
			}
			hdr.Linkname = target
		}
		if err := unpacks[i].entry(hdr, rel, tr); err != nil {
			cleanUp()
			return nil, err
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
}

type dirTimes struct {
	name string
	hdr  *tar.Header
}

func (u *unpack) entry(hdr *tar.Header, rel string, body io.Reader) error {
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
	case tar.TypeReg:
		f, err := u.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, body)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return failed("the backup is damaged: it ends in the middle of %s", hdr.Name)
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
			return failed("the backup writes through the symbolic link %s", p)
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
