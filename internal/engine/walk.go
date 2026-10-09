package engine

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// MaxWalkDepth is how many folders deep WalkTree goes. A level holds one file
// open, and nothing but a hostile tree nests this far. A variable so a test
// does not need a thousand folders.
var MaxWalkDepth = 1024

// ErrTooDeep is what WalkTree stops with at a folder nested deeper than it
// goes.
var ErrTooDeep = errors.New("folders are nested too deep")

// TreeNode is one item WalkTree meets. It is only good inside the call that
// gets it.
type TreeNode struct {
	// Stat is what lstat says: a link is described, not followed.
	Stat unix.Stat_t

	parent *TreeNode // nil at the start
	name   string    // the item's name in dir; empty at the start
	dir    int       // the folder that holds the item; the item itself at the start
	depth  int       // folders between the start and the item
}

// Path is relative to the folder the walk began in, and "." for that folder
// itself. It is put together when asked for: kept for each item, a tree a
// thousand folders deep would cost every level a copy of the path above it.
func (n *TreeNode) Path() string {
	if n.parent == nil {
		return "."
	}
	parts := make([]string, n.depth)
	for p := n; p.parent != nil; p = p.parent {
		parts[p.depth-1] = p.name
	}
	return strings.Join(parts, "/")
}

// where names the item in an error. A hostile tree has paths hundreds of
// kilobytes long, which are no use in an error or a log, so a deep one is
// cut down to its first and its last name.
func (n *TreeNode) where() string {
	if n.depth <= 8 {
		return n.Path()
	}
	first := n
	for first.depth > 1 {
		first = first.parent
	}
	return first.name + "/.../" + n.name
}

// IsDir reports whether the item is a folder.
func (n *TreeNode) IsDir() bool { return n.Stat.Mode&unix.S_IFMT == unix.S_IFDIR }

// Info describes the item as os.Lstat would, from what the walk already
// asked. The start is called ".".
func (n *TreeNode) Info() fs.FileInfo {
	name := n.name
	if name == "" {
		name = "."
	}
	return &nodeInfo{name: name, st: n.Stat}
}

// Readlink returns where the link points, as it is written.
func (n *TreeNode) Readlink() (string, error) {
	for size := 256; ; size *= 2 {
		buf := make([]byte, size)
		m, err := unix.Readlinkat(n.dir, n.name, buf)
		if err != nil {
			return "", &fs.PathError{Op: "readlink", Path: n.where(), Err: err}
		}
		if m < size {
			return string(buf[:m]), nil
		}
	}
}

// Open opens the item to read it, from the folder that holds it and never
// through a link. It does not wait for a writer, so a named pipe in the
// place of a file holds nothing up.
func (n *TreeNode) Open() (*os.File, error) {
	name := n.name
	if name == "" {
		name = "."
	}
	fd, err := unix.Openat(n.dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: n.where(), Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Lchown changes the owner of the item itself, never of what a link points to.
func (n *TreeNode) Lchown(uid, gid int) error {
	var err error
	if n.name == "" {
		err = unix.Fchown(n.dir, uid, gid)
	} else {
		err = unix.Fchownat(n.dir, n.name, uid, gid, unix.AT_SYMLINK_NOFOLLOW)
	}
	if err != nil {
		return &fs.PathError{Op: "lchown", Path: n.where(), Err: err}
	}
	return nil
}

// WalkTree calls fn for the folder start of root and for everything below it,
// a folder before its content.
//
// What is in the tree is not trusted and may change as the walk goes on. Each
// folder is opened from the one above it by its bare name, never through a
// link, so the walk cannot be led out of the root and costs time in
// proportion to the number of items however deep they are. Names are kept as
// the file system has them: fs.WalkDir over root.FS() fails on a folder whose
// name is not valid UTF-8. An item that is gone, or is not the folder it was
// when listed, is passed over. fn stops the walk by returning an error, or
// leaves out the item it was called for, and what is below it, by returning
// fs.SkipDir.
func WalkTree(root *os.Root, start string, fn func(*TreeNode) error) error {
	top, err := root.OpenFile(start, os.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return err
	}
	defer top.Close()
	n := &TreeNode{dir: int(top.Fd())}
	if err := unix.Fstat(n.dir, &n.Stat); err != nil {
		return &fs.PathError{Op: "fstat", Path: start, Err: err}
	}
	if err := fn(n); err != nil {
		if errors.Is(err, fs.SkipDir) {
			return nil
		}
		return err
	}
	return walkChildren(top, n, fn)
}

func walkChildren(d *os.File, parent *TreeNode, fn func(*TreeNode) error) error {
	fd := int(d.Fd())
	// One node serves the items of the folder in turn, so a walk holds a node
	// for each level it is in, not for each item it has met.
	var n TreeNode
	for {
		names, err := d.Readdirnames(256)
		for _, name := range names {
			n = TreeNode{parent: parent, name: name, dir: fd, depth: parent.depth + 1}
			if err := unix.Fstatat(fd, name, &n.Stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				if passedOver(err) {
					continue
				}
				return &fs.PathError{Op: "lstat", Path: n.where(), Err: err}
			}
			if err := fn(&n); err != nil {
				if errors.Is(err, fs.SkipDir) {
					continue
				}
				return err
			}
			if !n.IsDir() {
				continue
			}
			if n.depth >= MaxWalkDepth {
				return &fs.PathError{Op: "open", Path: n.where(), Err: ErrTooDeep}
			}
			sub, err := openFolder(fd, name)
			if err != nil {
				if passedOver(err) {
					continue
				}
				return err
			}
			err = walkChildren(sub, &n, fn)
			sub.Close()
			if err != nil {
				return err
			}
		}
		// A folder removed while it is read has nothing more to give.
		if errors.Is(err, io.EOF) || errors.Is(err, syscall.ENOENT) {
			return nil
		}
		if err != nil {
			return &fs.PathError{Op: "readdir", Path: parent.where(), Err: err}
		}
	}
}

// openFolder opens the folder called name in the one open as parent. A file
// of another kind in its place is refused without being opened: a named pipe
// would hold the open up until someone writes to it.
func openFolder(parent int, name string) (*os.File, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), name), nil
}

// passedOver reports whether err only says the item changed after it was
// listed: removed, or replaced by something that is not a folder or is a link.
func passedOver(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

// nodeInfo is what a walk saw of an item, as an fs.FileInfo.
type nodeInfo struct {
	name string
	st   unix.Stat_t
}

func (i *nodeInfo) Name() string       { return i.name }
func (i *nodeInfo) Size() int64        { return i.st.Size }
func (i *nodeInfo) ModTime() time.Time { return time.Unix(i.st.Mtim.Unix()) }
func (i *nodeInfo) IsDir() bool        { return i.Mode().IsDir() }
func (i *nodeInfo) Sys() any           { return nil }

func (i *nodeInfo) Mode() fs.FileMode {
	m := fs.FileMode(i.st.Mode & 0o777)
	switch i.st.Mode & unix.S_IFMT {
	case unix.S_IFBLK:
		m |= fs.ModeDevice
	case unix.S_IFCHR:
		m |= fs.ModeDevice | fs.ModeCharDevice
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFIFO:
		m |= fs.ModeNamedPipe
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	case unix.S_IFSOCK:
		m |= fs.ModeSocket
	}
	if i.st.Mode&unix.S_ISGID != 0 {
		m |= fs.ModeSetgid
	}
	if i.st.Mode&unix.S_ISUID != 0 {
		m |= fs.ModeSetuid
	}
	if i.st.Mode&unix.S_ISVTX != 0 {
		m |= fs.ModeSticky
	}
	return m
}
