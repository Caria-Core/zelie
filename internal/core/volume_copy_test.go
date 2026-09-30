package core

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
)

// copyServer has a source volume with a few kinds of files, an empty volume
// to copy into and one that is not a game's.
func copyServer(t *testing.T) (s *Server, src, dst, db string) {
	t.Helper()
	s, f := newServer()
	src, dst, db = t.TempDir(), t.TempDir(), t.TempDir()
	f.volumeDirs = map[string]string{"srv-a": src, "srv-b": dst, "db-vol": db}
	s.SFTPVolumes, _ = LoadSFTPVolumes(filepath.Join(t.TempDir(), "sftp-volumes.json"))
	if err := s.SFTPVolumes.Set([]string{"srv-a", "srv-b"}); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "world", "region"), 0o755))
	must(os.WriteFile(filepath.Join(src, "world", "region", "r.0.0.mca"), []byte("chunks"), 0o640))
	must(os.WriteFile(filepath.Join(src, "server.properties"), []byte("motd=hi"), 0o644))
	must(os.Symlink("world/region", filepath.Join(src, "latest")))
	// A link that points out of the volume stays a link and is never followed.
	must(os.Symlink("/etc", filepath.Join(src, "etc-link")))
	must(os.Link(filepath.Join(src, "server.properties"), filepath.Join(src, "props-again")))
	return s, src, dst, db
}

func TestCopyVolume(t *testing.T) {
	s, src, dst, _ := copyServer(t)
	root := &peer.Peer{UID: 0}
	rec := request(t, s, root, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-b"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("copy: %d %s", rec.Code, rec.Body)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "world", "region", "r.0.0.mca")); string(b) != "chunks" {
		t.Errorf("region file = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dst, "world", "region", "r.0.0.mca")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v, %v", fi, err)
	}
	for link, target := range map[string]string{"latest": "world/region", "etc-link": "/etc"} {
		if got, err := os.Readlink(filepath.Join(dst, link)); err != nil || got != target {
			t.Errorf("%s -> %q, %v; want a link to %q", link, got, err, target)
		}
	}
	a, _ := os.Stat(filepath.Join(dst, "server.properties"))
	b, _ := os.Stat(filepath.Join(dst, "props-again"))
	if a == nil || b == nil || !os.SameFile(a, b) {
		t.Error("the hard link became two files")
	}
	if _, err := os.Stat(filepath.Join(dst, ".zelie-restore")); err == nil {
		t.Error("the staging folder is left behind")
	}
	// Owners are copied as the numbers they are. Only root can give files
	// to someone else, so this checks what the test's own user can see.
	want, _ := os.Stat(filepath.Join(src, "server.properties"))
	got, _ := os.Stat(filepath.Join(dst, "server.properties"))
	if want.Sys().(*syscall.Stat_t).Uid != got.Sys().(*syscall.Stat_t).Uid {
		t.Error("the owner changed")
	}
	// The source is untouched.
	if b, _ := os.ReadFile(filepath.Join(src, "server.properties")); string(b) != "motd=hi" {
		t.Errorf("source changed: %q", b)
	}
}

func TestCopyVolumeRefusals(t *testing.T) {
	s, _, dst, _ := copyServer(t)
	root := &peer.Peer{UID: 0}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"not json":        {`{`, http.StatusBadRequest},
		"unknown field":   {`{"from":"srv-a","to":"srv-b","delete":true}`, http.StatusBadRequest},
		"bad name":        {`{"from":"../srv-a","to":"srv-b"}`, http.StatusBadRequest},
		"onto itself":     {`{"from":"srv-a","to":"srv-a"}`, http.StatusBadRequest},
		"from a database": {`{"from":"db-vol","to":"srv-b"}`, http.StatusForbidden},
		"into a database": {`{"from":"srv-a","to":"db-vol"}`, http.StatusForbidden},
		"not listed":      {`{"from":"srv-a","to":"srv-x"}`, http.StatusForbidden},
	} {
		if rec := request(t, s, root, "POST", "/v1/volume-copies", tc.body); rec.Code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}

	// A volume that already has files is never written over.
	os.WriteFile(filepath.Join(dst, "keep.txt"), []byte("mine"), 0o644)
	if rec := request(t, s, root, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-b"}`); rec.Code != http.StatusConflict {
		t.Errorf("into a volume with files: %d %s", rec.Code, rec.Body)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 1 {
		t.Errorf("the volume holds %d entries after a refused copy", len(entries))
	}
}

func TestCopyVolumeRunningServers(t *testing.T) {
	s, _, dst, _ := copyServer(t)
	f := s.Engine.(*fakeEngine)
	root := &peer.Peer{UID: 0}

	// The new server's volume is not written into while a container holds it.
	f.usedVolumes = map[string]bool{"srv-b": true}
	if rec := request(t, s, root, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-b"}`); rec.Code != http.StatusConflict {
		t.Errorf("into a volume in use: %d %s", rec.Code, rec.Body)
	}
	// The source may be running: that is what cloning a live server does.
	f.usedVolumes = map[string]bool{"srv-a": true}
	if rec := request(t, s, root, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-b"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("from a volume in use: %d %s", rec.Code, rec.Body)
	}
	if _, err := os.Stat(filepath.Join(dst, "server.properties")); err != nil {
		t.Errorf("the live copy is missing files: %v", err)
	}

	// A listed volume that does not exist.
	s.SFTPVolumes.Set([]string{"srv-a", "srv-x"})
	if rec := request(t, s, root, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-x"}`); rec.Code != http.StatusNotFound {
		t.Errorf("into a volume that does not exist: %d %s", rec.Code, rec.Body)
	}
}

func TestCopyVolumeNeedsRoom(t *testing.T) {
	s, src, dst, _ := copyServer(t)
	s.Paths.Volumes = t.TempDir()
	root := &peer.Peer{UID: 0}
	// A sparse file that no disk has room for.
	huge := filepath.Join(src, "huge.bin")
	if err := os.WriteFile(huge, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(huge, 1<<42); err != nil {
		t.Skipf("no sparse files this big here: %v", err)
	}
	if size, err := s.Engine.VolumeSize("srv-a"); err != nil || size < 1<<42 {
		t.Skipf("the fake engine counts %d bytes: %v", size, err)
	}
	rec := request(t, s, root, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-b"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Not enough disk space") {
		t.Errorf("copy without room: %d %s", rec.Code, rec.Body)
	}
	if entries, _ := os.ReadDir(dst); len(entries) != 0 {
		t.Errorf("a refused copy wrote %d entries", len(entries))
	}
}

func TestSFTPUserMayNotCopyVolumes(t *testing.T) {
	s, _, _, _ := copyServer(t)
	s.Allowed.Routes = map[uint32][]string{4242: SFTPRoutes}
	s.SFTPUID = 4242
	rec := request(t, s, &peer.Peer{UID: 4242}, "POST", "/v1/volume-copies", `{"from":"srv-a","to":"srv-b"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("as the SFTP user: %d", rec.Code)
	}
}
