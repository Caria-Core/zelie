//go:build integration

package core

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

const installTestImage = "docker.io/library/busybox:latest"

func installRequest(id, volume, script string) InstallRequest {
	return InstallRequest{
		ID: id, App: "it-install", Image: installTestImage, Entrypoint: "sh", Script: script,
		Env:    []string{"SERVER_PORT=25565", "VERSION=1.21"},
		Volume: volume, MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 64,
	}
}

// runInstallTest starts the install as the core does and waits for it.
func runInstallTest(t *testing.T, e *engine.Engine, req InstallRequest) uint32 {
	t.Helper()
	ctx := context.Background()
	e.Remove(ctx, req.ID)
	os.Remove(engine.LogPathFor(engine.DefaultPaths, req.ID))
	spec, err := req.spec()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Run(ctx, spec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Remove(context.Background(), req.ID) })
	waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	code, err := e.Wait(waitCtx, req.ID)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func installLog(t *testing.T, id, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(engine.LogPathFor(engine.DefaultPaths, id))
		if strings.Contains(string(b), want) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("log of %s never contained %q, got:\n%s", id, want, b)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestInstallWritesToTheVolume(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(engine.DefaultPaths.Socket); err != nil {
		t.Skip("Zelie's containerd is not running")
	}
	e, err := engine.Connect(context.Background(), engine.DefaultPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	const volume = "it-install-vol"
	dir := filepath.Join(engine.DefaultPaths.Volumes, volume)
	os.RemoveAll(dir)
	if err := e.CreateVolume(volume); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	script := `echo "installing $VERSION on port $SERVER_PORT"
echo hello > /mnt/server/hello.txt
mkdir /mnt/server/world && chown 1000:1000 /mnt/server/world
echo more >> /mnt/install/install.sh 2>/dev/null && echo script-writable
grep -c . /mnt/install/install.sh
echo finished
`
	if code := runInstallTest(t, e, installRequest("it-install-ok", volume, script)); code != 0 {
		t.Fatalf("install exited with %d:\n%s", code, installLog(t, "it-install-ok", "finished"))
	}
	out := installLog(t, "it-install-ok", "finished")
	if !strings.Contains(out, "installing 1.21 on port 25565") {
		t.Errorf("the variables did not arrive:\n%s", out)
	}
	if strings.Contains(out, "script-writable") {
		t.Errorf("the install can change its script:\n%s", out)
	}

	// Files land in the volume as the IDs a container sees, so any later
	// container, whatever its ID block, finds root's files owned by root.
	if b, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil || string(b) != "hello\n" {
		t.Fatalf("hello.txt: %q, %v", b, err)
	}
	for path, want := range map[string]uint32{"hello.txt": 0, "world": 1000} {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(dir, path), &st); err != nil {
			t.Fatal(err)
		}
		if st.Uid != want || st.Gid != want {
			t.Errorf("%s is owned by %d:%d on disk, want %d", path, st.Uid, st.Gid, want)
		}
	}

	// Removing the container takes the script with it, and the files stay.
	if err := e.Remove(context.Background(), "it-install-ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engine.DefaultPaths.Data, "containers", "it-install-ok")); !os.IsNotExist(err) {
		t.Errorf("the container's directory is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.txt")); err != nil {
		t.Errorf("hello.txt went with the container: %v", err)
	}

	// A script that fails reports its exit code, and what it wrote stays.
	failing := "echo half-way > /mnt/server/partial.txt\necho about-to-fail\nexit 3\n"
	if code := runInstallTest(t, e, installRequest("it-install-bad", volume, failing)); code != 3 {
		t.Errorf("failing install exited with %d, want 3", code)
	}
	installLog(t, "it-install-bad", "about-to-fail")
	if _, err := os.Stat(filepath.Join(dir, "partial.txt")); err != nil {
		t.Errorf("partial.txt: %v", err)
	}
}
