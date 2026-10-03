package backup

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// tree lists a directory as "path mode [-> target | = body]" lines.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		line := rel + " " + fi.Mode().String()
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case fi.Mode().IsRegular():
			b, _ := os.ReadFile(p)
			line += " = " + string(b)
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTarRoundTrip(t *testing.T) {
	data, uploads := t.TempDir(), t.TempDir()
	write(t, data, "world/level.dat", "level")
	write(t, data, "world/region/r.0.0.mca", strings.Repeat("x", 100000))
	write(t, data, "server.properties", "motd=hi")
	os.Chmod(filepath.Join(data, "server.properties"), 0o600)
	os.Symlink("world/level.dat", filepath.Join(data, "current"))
	os.Link(filepath.Join(data, "server.properties"), filepath.Join(data, "props-link"))
	os.Mkdir(filepath.Join(data, "empty"), 0o750)
	write(t, uploads, "a.png", "png")
	// Left over from a restore that broke off; not part of the app's files.
	write(t, data, staging+"/junk", "junk")

	// As root, as the core runs: files belong to the container's users.
	asRoot := os.Getuid() == 0
	if asRoot {
		os.Lchown(filepath.Join(data, "world"), 1000, 1000)
		os.Lchown(filepath.Join(data, "world/level.dat"), 1000, 1001)
		os.Lchown(filepath.Join(data, "current"), 1000, 1000)
		os.Chmod(filepath.Join(data, "server.properties"), 0o750|fs.ModeSetuid)
	}

	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	os.Chtimes(filepath.Join(data, "world/level.dat"), mtime, mtime)

	var buf bytes.Buffer
	st, err := WriteTar(context.Background(), &buf, []Volume{{"data", openRoot(t, data)}, {"srv/uploads", openRoot(t, uploads)}})
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 4 || st.Changed != 0 || st.Bytes != int64(len("level")+100000+len("motd=hi")+len("png")) {
		t.Fatalf("stats %+v", st)
	}
	var names []string
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
	}
	for _, want := range []string{"data/", "data/world/level.dat", "data/current", "srv/uploads/a.png"} {
		if !slices.Contains(names, want) {
			t.Errorf("archive has no %s: %v", want, names)
		}
	}
	for _, n := range names {
		if strings.Contains(n, staging) {
			t.Errorf("archive has %s", n)
		}
	}

	// Into volumes that changed since: the new file goes, the old come back.
	os.RemoveAll(filepath.Join(data, staging))
	wantData, wantUploads := tree(t, data), tree(t, uploads)
	write(t, data, "world/level.dat", "changed")
	write(t, data, "new-since", "new")
	os.Remove(filepath.Join(data, "current"))
	os.RemoveAll(filepath.Join(uploads, "a.png"))

	restored, err := RestoreTar(context.Background(), bytes.NewReader(buf.Bytes()), []Volume{{"data", openRoot(t, data)}, {"srv/uploads", openRoot(t, uploads)}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restored, []string{"data", "srv/uploads"}) {
		t.Errorf("restored %v", restored)
	}
	if got := tree(t, data); !slices.Equal(got, wantData) {
		t.Errorf("data after restore:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantData, "\n"))
	}
	if got := tree(t, uploads); !slices.Equal(got, wantUploads) {
		t.Errorf("uploads after restore: %v, want %v", got, wantUploads)
	}
	fi, _ := os.Stat(filepath.Join(data, "world/level.dat"))
	if !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime %v, want %v", fi.ModTime(), mtime)
	}
	if asRoot {
		for name, want := range map[string][2]uint32{"world": {1000, 1000}, "world/level.dat": {1000, 1001}, "current": {1000, 1000}, "empty": {0, 0}} {
			fi, _ := os.Lstat(filepath.Join(data, name))
			st := fi.Sys().(*syscall.Stat_t)
			if st.Uid != want[0] || st.Gid != want[1] {
				t.Errorf("%s owned by %d:%d, want %v", name, st.Uid, st.Gid, want)
			}
		}
		if fi, _ := os.Stat(filepath.Join(data, "server.properties")); fi.Mode()&fs.ModeSetuid == 0 {
			t.Errorf("set-user-ID bit lost: %v", fi.Mode())
		}
	}
	a, _ := os.Stat(filepath.Join(data, "server.properties"))
	b, _ := os.Stat(filepath.Join(data, "props-link"))
	if !os.SameFile(a, b) {
		t.Error("hard link came back as two files")
	}
}

func TestRestoreMatchesVolumesByPath(t *testing.T) {
	data := t.TempDir()
	write(t, data, "a", "old a")
	var buf bytes.Buffer
	if _, err := WriteTar(context.Background(), &buf, []Volume{{"data", openRoot(t, data)}}); err != nil {
		t.Fatal(err)
	}
	// The app now has /data and /cache; /cache is not in the backup.
	write(t, data, "a", "new a")
	cache := t.TempDir()
	write(t, cache, "c", "cache")
	restored, err := RestoreTar(context.Background(), &buf, []Volume{{"cache", openRoot(t, cache)}, {"data", openRoot(t, data)}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restored, []string{"data"}) {
		t.Errorf("restored %v", restored)
	}
	if b, _ := os.ReadFile(filepath.Join(data, "a")); string(b) != "old a" {
		t.Errorf("data/a = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(cache, "c")); string(b) != "cache" {
		t.Errorf("cache/c = %q", b)
	}
	if _, err := os.Stat(filepath.Join(cache, staging)); !errors.Is(err, fs.ErrNotExist) {
		t.Error("staging folder left in cache")
	}
}

type entry struct {
	name, link string
	typ        byte
	body       string
}

func archive(t *testing.T, entries ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Linkname: e.link, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Uid: os.Getuid(), Gid: os.Getgid()}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	return &buf
}

func TestRestoreRefusesHostileArchives(t *testing.T) {
	outside := t.TempDir()
	cases := map[string][]entry{
		"through a symlink": {
			{name: "data/l", link: outside, typ: tar.TypeSymlink},
			{name: "data/l/x", typ: tar.TypeReg, body: "x"},
		},
		"through a relative symlink": {
			{name: "data/l", link: "..", typ: tar.TypeSymlink},
			{name: "data/l/x", typ: tar.TypeReg, body: "x"},
		},
		"hard link out of the volume": {
			{name: "data/x", link: "other/secret", typ: tar.TypeLink},
		},
		"hard link through a symlink": {
			{name: "data/l", link: outside, typ: tar.TypeSymlink},
			{name: "data/x", link: "data/l/secret", typ: tar.TypeLink},
		},
		"cut short": nil,
	}
	write(t, outside, "secret", "secret")
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			data, other := t.TempDir(), t.TempDir()
			write(t, data, "keep", "keep")
			write(t, other, "secret", "secret")
			want := tree(t, data)
			buf := archive(t, entries...)
			if entries == nil {
				full := archive(t, entry{name: "data/big", typ: tar.TypeReg, body: strings.Repeat("y", 4096)})
				buf = bytes.NewBuffer(full.Bytes()[:1000])
			}
			_, err := RestoreTar(context.Background(), buf, []Volume{{"data", openRoot(t, data)}, {"other", openRoot(t, other)}})
			if err == nil {
				t.Fatal("restore went through")
			}
			if got := tree(t, data); !slices.Equal(got, want) {
				t.Errorf("volume changed: %v", got)
			}
			if got := tree(t, outside); !slices.Equal(got, []string{". " + mustMode(t, outside), "secret -rw-r--r-- = secret"}) {
				t.Errorf("outside changed: %v", got)
			}
		})
	}
}

func mustMode(t *testing.T, dir string) string {
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().String()
}

func TestRestoreSkipsBadNames(t *testing.T) {
	data := t.TempDir()
	buf := archive(t,
		entry{name: "data/../escape", typ: tar.TypeReg, body: "x"},
		entry{name: "/data/abs", typ: tar.TypeReg, body: "x"},
		entry{name: "elsewhere/y", typ: tar.TypeReg, body: "y"},
		entry{name: "data/ok", typ: tar.TypeReg, body: "ok"},
	)
	if _, err := RestoreTar(context.Background(), buf, []Volume{{"data", openRoot(t, data)}}); err != nil {
		t.Fatal(err)
	}
	got := tree(t, data)
	if len(got) != 2 || got[1] != "ok -rw-r--r-- = ok" {
		t.Errorf("volume: %v", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(data), "escape")); err == nil {
		t.Error("a file escaped the volume")
	}
}

func allocated(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Blocks * 512
}

func TestRestoreKeepsHoles(t *testing.T) {
	const size = 256 << 20
	src, dst := t.TempDir(), t.TempDir()
	f, err := os.Create(filepath.Join(src, "world.dat"))
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte("head"), 0)
	f.WriteAt([]byte("tail"), size-4)
	f.Close()
	// A file that ends in a hole must keep its length.
	g, _ := os.Create(filepath.Join(src, "trailing.dat"))
	g.WriteAt([]byte("x"), 0)
	g.Truncate(10 << 20)
	g.Close()

	var buf bytes.Buffer
	vols := func(dir string) []Volume { return []Volume{{"data", openRoot(t, dir)}} }
	if _, err := WriteTar(context.Background(), &buf, vols(src)); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreTar(context.Background(), &buf, vols(dst)); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]int64{"world.dat": size, "trailing.dat": 10 << 20} {
		p := filepath.Join(dst, name)
		if fi, err := os.Stat(p); err != nil || fi.Size() != want {
			t.Fatalf("%s: %v, %v; want %d bytes", name, fi, err, want)
		}
		// APFS allocates a file that is extended right after it was written,
		// so the disk usage of the trailing hole is only checked for world.dat.
		if a := allocated(t, p); a > 4<<20 && name == "world.dat" {
			t.Errorf("%s takes %d bytes on disk; holes were written out", name, a)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dst, "world.dat"))
	if string(b[:4]) != "head" || string(b[size-4:]) != "tail" || !bytes.Equal(b[4:size-4], make([]byte, size-8)) {
		t.Error("content differs")
	}
}

func TestRestoreStopsAtLimit(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write(t, src, "a", strings.Repeat("a", 100<<10))
	write(t, src, "b", strings.Repeat("b", 100<<10))
	var buf bytes.Buffer
	if _, err := WriteTar(context.Background(), &buf, []Volume{{"data", openRoot(t, src)}}); err != nil {
		t.Fatal(err)
	}
	over := errors.New("no room")
	_, err := RestoreTarCapped(context.Background(), &buf, []Volume{{"data", openRoot(t, dst)}}, 150<<10, over)
	if !errors.Is(err, over) {
		t.Fatalf("err = %v, want the limit error", err)
	}
	if left, _ := os.ReadDir(dst); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}
