//go:build integration

package core

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
)

// The file manager changes a volume that a container is running on: the game
// sees the new file, and the file carries the game's user.
func TestFilesChangedWhileTheGameRuns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(engine.DefaultPaths.Socket); err != nil {
		t.Skip("Zelie's containerd is not running")
	}
	ctx := context.Background()
	e, err := engine.Connect(ctx, engine.DefaultPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	const (
		app    = "it-files"
		volume = "it-files-vol"
		id     = "it-files-1"
	)
	dir := filepath.Join(engine.DefaultPaths.Volumes, volume)
	os.RemoveAll(dir)
	e.Remove(ctx, id)
	os.Remove(engine.LogPathFor(engine.DefaultPaths, id))
	if err := e.CreateVolume(volume); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.Remove(context.Background(), id)
		os.RemoveAll(dir)
	})
	os.WriteFile(filepath.Join(dir, "motd.txt"), []byte("before"), 0o644)

	// The game copies the file it was given to another, over and over, and
	// says when it started.
	script := `cd /home/container
echo started
while true; do cat motd.txt > seen.txt; id -u > uid.txt; sleep 0.2; done
`
	if err := e.Run(ctx, engine.Spec{
		ID: id, App: app, Network: app, Image: installTestImage,
		Args:        []string{"sh", "-c", script},
		Volumes:     []engine.VolumeMount{{Name: volume, Target: "/home/container"}},
		MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 64,
		User: &engine.IDs{UID: 988, GID: 988}, WorkDir: "/home/container",
	}); err != nil {
		t.Fatal(err)
	}
	installLog(t, id, "started")

	// Prepare would be refused now; the file manager is not.
	if _, err := e.OpenVolume(ctx, volume); err == nil {
		t.Fatal("OpenVolume opened a volume a container runs on")
	}
	s := &Server{Engine: e, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Allowed: peer.Policy{UIDs: []uint32{999}}}
	root := &peer.Peer{UID: 0}
	do := func(method, op, query, body string) int {
		return request(t, s, root, method, "/v1/volumes/"+volume+"/files/"+op+"?"+query, body).Code
	}
	if code := do("PUT", "content", "path=motd.txt&uid=988&gid=988", "after"); code != http.StatusNoContent {
		t.Fatalf("write: %d", code)
	}
	if code := do("PUT", "upload", "path=plugin.jar&uid=988&gid=988", strings.Repeat("j", 1<<20)); code != http.StatusNoContent {
		t.Fatalf("upload: %d", code)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(filepath.Join(dir, "seen.txt"))
		if string(b) == "after" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the game still sees %q", b)
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, p := range []string{"motd.txt", "plugin.jar", "seen.txt"} {
		if st := statOf(t, filepath.Join(dir, p)); st.Uid != 988 || st.Gid != 988 {
			t.Errorf("%s is owned by %d:%d on disk, want 988:988", p, st.Uid, st.Gid)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "uid.txt")); strings.TrimSpace(string(b)) != "988" {
		t.Errorf("the game ran as %q", b)
	}

	// What the game makes can be packed and unpacked while it runs.
	if code := do("POST", "compress", "", `{"uid":988,"gid":988,"dir":"","paths":["motd.txt","plugin.jar"],"name":"pack"}`); code != http.StatusOK {
		t.Fatalf("compress: %d", code)
	}
	os.Mkdir(filepath.Join(dir, "out"), 0o755)
	if code := do("POST", "extract", "", `{"uid":988,"gid":988,"path":"pack.tar.gz","dir":"out"}`); code != http.StatusOK {
		t.Fatalf("extract: %d", code)
	}
	if st := statOf(t, filepath.Join(dir, "out", "plugin.jar")); st.Uid != 988 || st.Size != 1<<20 {
		t.Errorf("extracted: %d %d", st.Uid, st.Size)
	}
}
