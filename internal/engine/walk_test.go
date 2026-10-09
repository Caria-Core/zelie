package engine

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// deepChain makes n folders called name inside each other below dir, one
// openat at a time, which is how a tenant gets past the path length a single
// call allows.
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

// lowerDepth makes the walk give up at n folders for the length of the test.
func lowerDepth(t *testing.T, n int) {
	t.Helper()
	was := MaxWalkDepth
	MaxWalkDepth = n
	t.Cleanup(func() { MaxWalkDepth = was })
}

func walkPaths(t *testing.T, dir string) []string {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var got []string
	err = WalkTree(root, ".", func(n *TreeNode) error {
		got = append(got, n.Path())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestWalkTreeVisitsFoldersBeforeTheirContent(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "world", "region"), 0o755)
	os.WriteFile(filepath.Join(dir, "world", "region", "r.mca"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "a b\nc"), []byte("x"), 0o644)
	os.Symlink("world", filepath.Join(dir, "link"))

	got := walkPaths(t, dir)
	slices.Sort(got)
	want := []string{".", "a b\nc", "link", "world", "world/region", "world/region/r.mca"}
	if !slices.Equal(got, want) {
		t.Errorf("visited %q, want %q", got, want)
	}
	// A folder comes before what is in it.
	order := walkPaths(t, dir)
	for i, p := range order {
		if parent := filepath.Dir(p); parent != "." && !slices.Contains(order[:i], parent) {
			t.Errorf("%s came before its folder", p)
		}
	}
}

func TestWalkTreeLeavesOutWhatFnSkips(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "skip", "below"), 0o755)
	os.MkdirAll(filepath.Join(dir, "keep"), 0o755)
	os.WriteFile(filepath.Join(dir, "skip", "below", "f"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "keep", "f"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "skip-file"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "z"), []byte("x"), 0o644)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	var got []string
	err = WalkTree(root, ".", func(n *TreeNode) error {
		if strings.HasPrefix(n.Path(), "skip") {
			return fs.SkipDir
		}
		got = append(got, n.Path())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{".", "keep", "keep/f", "z"}; !slices.Equal(got, want) {
		t.Errorf("visited %q, want %q", got, want)
	}
	// Skipping the start leaves out everything.
	n := 0
	if err := WalkTree(root, ".", func(*TreeNode) error { n++; return fs.SkipDir }); err != nil || n != 1 {
		t.Errorf("skipping the start: %d calls, %v", n, err)
	}
}

func TestWalkTreeDoesNotFollowLinks(t *testing.T) {
	outside, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	os.Symlink(outside, filepath.Join(dir, "out"))
	os.Symlink(".", filepath.Join(dir, "self"))
	got := walkPaths(t, dir)
	slices.Sort(got)
	if want := []string{".", "out", "self"}; !slices.Equal(got, want) {
		t.Errorf("visited %q, want %q", got, want)
	}
}

// A name that is not valid UTF-8 is a legal name on Linux. fs.WalkDir over
// root.FS() fails on a folder called that, and with it everything built on it.
func TestWalkTreeMeetsFoldersWithAnyName(t *testing.T) {
	dir := t.TempDir()
	bad := "caf\xe9"
	if err := os.Mkdir(filepath.Join(dir, bad), 0o755); err != nil {
		t.Skipf("this file system refuses a name that is not UTF-8: %v", err)
	}
	os.WriteFile(filepath.Join(dir, bad, "inside"), []byte("x"), 0o644)
	got := walkPaths(t, dir)
	if !slices.Contains(got, bad+"/inside") {
		t.Errorf("visited %q, which lacks the file below %q", got, bad)
	}
	if _, err := diskUsage(dir); err != nil {
		t.Error(err)
	}
}

// The chain is longer than any path the system takes in one call, and each
// folder is opened from the one above it, so it is walked all the same.
func TestWalkTreeGoesDeeperThanAPathCanBe(t *testing.T) {
	dir := t.TempDir()
	deepChain(t, dir, strings.Repeat("d", 100), 60)
	got := walkPaths(t, dir)
	if len(got) != 61 {
		t.Fatalf("visited %d items, want 61", len(got))
	}
	if _, err := diskUsage(dir); err != nil {
		t.Error(err)
	}
}

func TestWalkTreeStopsAtTheLimitOnDepth(t *testing.T) {
	lowerDepth(t, 20)
	dir := t.TempDir()
	deepChain(t, dir, "d", 25)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	seen := 0
	err = WalkTree(root, ".", func(*TreeNode) error { seen++; return nil })
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("got %v, want ErrTooDeep", err)
	}
	if seen < 20 {
		t.Errorf("stopped after %d items", seen)
	}
}

// What changes between the listing and the open is passed over, not a reason
// to give up on the whole tree.
func TestWalkTreePassesOverWhatChangedUnderneath(t *testing.T) {
	outside, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	for _, d := range []string{"gone", "file", "link", "stays"} {
		os.Mkdir(filepath.Join(dir, d), 0o755)
		os.WriteFile(filepath.Join(dir, d, "child"), []byte("x"), 0o644)
	}
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	var got []string
	err := WalkTree(root, ".", func(n *TreeNode) error {
		got = append(got, n.Path())
		// The walk has listed the folder; now the tenant swaps it.
		switch n.Path() {
		case "gone":
			os.RemoveAll(filepath.Join(dir, "gone"))
		case "file":
			os.RemoveAll(filepath.Join(dir, "file"))
			os.WriteFile(filepath.Join(dir, "file"), []byte("x"), 0o644)
		case "link":
			os.RemoveAll(filepath.Join(dir, "link"))
			os.Symlink(outside, filepath.Join(dir, "link"))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "stays/child") {
		t.Errorf("the folders that did not change were not walked: %q", got)
	}
	for _, p := range got {
		if strings.HasSuffix(p, "secret") || strings.HasPrefix(p, "link/") || strings.HasPrefix(p, "gone/") || strings.HasPrefix(p, "file/") {
			t.Errorf("walked into %s", p)
		}
	}
}

// A named pipe where a folder was must not hold the walk up: opening one for
// reading waits for a writer.
func TestOpenFolderDoesNotWaitOnANamedPipe(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := unix.Mkfifo(fifo, 0o644); err != nil {
		t.Skip(err)
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	done := make(chan error, 1)
	go func() {
		f, err := openFolder(fd, "pipe")
		if err == nil {
			f.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if !passedOver(err) {
			t.Errorf("opening a pipe as a folder gave %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a named pipe as a folder is waiting for a writer")
	}
}

func TestOneVolumeThatCannotBeMeasuredDoesNotHideTheOthers(t *testing.T) {
	lowerDepth(t, 20)
	vols := t.TempDir()
	e := &Engine{paths: Paths{Volumes: vols}}
	for _, name := range []string{"good", "deep"} {
		if err := e.CreateVolume(name); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(e.volumeDir("good"), "f"), make([]byte, 8192), 0o644)
	deepChain(t, e.volumeDir("deep"), "d", 25)

	sizes, unmeasured, err := e.VolumeSizes()
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := sizes["good"]; !ok || n < 8192 {
		t.Errorf("sizes %v lack the volume that can be measured", sizes)
	}
	// The deep volume is not left out as if it were not there, and it is not
	// counted as empty either.
	if _, ok := sizes["deep"]; ok {
		t.Errorf("sizes %v has a volume that was not measured", sizes)
	}
	if err := unmeasured["deep"]; !errors.Is(err, ErrTooDeep) {
		t.Errorf("unmeasured %v, want the deep volume with ErrTooDeep", unmeasured)
	}
	if _, ok := unmeasured["good"]; ok {
		t.Errorf("unmeasured %v names a volume that was measured", unmeasured)
	}
}

// The error of a tree nested past the limit names where it is without the
// whole path, which in a hostile tree is hundreds of kilobytes. The error
// reaches the log at every check.
func TestTooDeepErrorDoesNotCarryTheWholePath(t *testing.T) {
	lowerDepth(t, 40)
	dir := t.TempDir()
	long := strings.Repeat("n", 200)
	deepChain(t, dir, long, 45)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = WalkTree(root, ".", func(*TreeNode) error { return nil })
	if !errors.Is(err, ErrTooDeep) {
		t.Fatalf("got %v, want ErrTooDeep", err)
	}
	if len(err.Error()) > 1000 {
		t.Errorf("the error is %d bytes long", len(err.Error()))
	}
}

// A node is built from its parent, so its path is right however the walk got
// there, and the node of one item is not the node of the next.
func TestTreeNodePathAndInfo(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(dir, "a", "b", "f"), []byte("hello"), 0o640)
	os.Symlink("target", filepath.Join(dir, "a", "link"))
	root, _ := os.OpenRoot(dir)
	defer root.Close()

	var paths []string
	err := WalkTree(root, "a", func(n *TreeNode) error {
		paths = append(paths, n.Path())
		want, err := root.Lstat(filepath.Join("a", n.Path()))
		if err != nil {
			t.Fatal(err)
		}
		got := n.Info()
		if got.Mode() != want.Mode() || got.Size() != want.Size() || !got.ModTime().Equal(want.ModTime()) || got.IsDir() != want.IsDir() {
			t.Errorf("%s: info %v %d %v, lstat %v %d %v", n.Path(), got.Mode(), got.Size(), got.ModTime(), want.Mode(), want.Size(), want.ModTime())
		}
		switch n.Path() {
		case "link":
			if target, err := n.Readlink(); err != nil || target != "target" {
				t.Errorf("readlink: %q, %v", target, err)
			}
		case "b/f":
			f, err := n.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(f)
			f.Close()
			if string(b) != "hello" {
				t.Errorf("read %q", b)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	if want := []string{".", "b", "b/f", "link"}; !slices.Equal(paths, want) {
		t.Errorf("visited %q, want %q", paths, want)
	}
}

// Opening an item reaches it from its folder, and a link in its place is
// refused, not followed out of the tree.
func TestTreeNodeOpenDoesNotFollowALink(t *testing.T) {
	outside, dir := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link"))
	root, _ := os.OpenRoot(dir)
	defer root.Close()
	err := WalkTree(root, ".", func(n *TreeNode) error {
		if n.Path() != "link" {
			return nil
		}
		f, err := n.Open()
		if err == nil {
			f.Close()
			t.Error("a link was opened")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A path is built for the item that asks for it, not for each level of the
// walk, so going down does not cost more for every level than for the one
// above. Built for every item, a chain of long names at the limit took
// hundreds of megabytes.
func TestWalkTreeCostsNoPathsAtEachLevel(t *testing.T) {
	const levels = 300
	dir := t.TempDir()
	deepChain(t, dir, strings.Repeat("n", 255), levels)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	items := 0
	err = WalkTree(root, ".", func(*TreeNode) error { items++; return nil })
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if items != levels+1 {
		t.Fatalf("visited %d items", items)
	}
	// The paths alone would come to 255 * levels² / 2, over 11 MB.
	if got := after.TotalAlloc - before.TotalAlloc; got > 4<<20 {
		t.Errorf("a walk down %d folders allocated %d bytes", levels, got)
	}
}
