//go:build integration

// These tests need root on Linux. Run them with hack/vm-test.sh.

package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

// startCore serves a Server with a fake engine on a real Unix socket, so the
// kernel's peer credentials are checked for real.
func startCore(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir, err := os.MkdirTemp("", "zelie-core-test")
	if err != nil {
		t.Fatal(err)
	}
	// Other users must be able to reach the socket for the test to mean
	// anything.
	os.Chmod(dir, 0o755)
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "core.sock")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Server{Engine: &fakeEngine{}, Paths: engine.DefaultPaths, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go s.Serve(ctx, socket)
	for range 50 {
		if _, err := os.Stat(socket); err == nil {
			return socket
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("core did not start")
	return ""
}

func TestOnlyRootGetsIn(t *testing.T) {
	socket := startCore(t)

	if _, err := NewClient(socket).List(context.Background()); err != nil {
		t.Fatalf("root was refused: %v", err)
	}

	// The same request from an ordinary user must be refused by the peer
	// check, even though the socket file itself is open to everyone.
	out, err := exec.Command("runuser", "-u", "nobody", "--",
		"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
		"--unix-socket", socket, "http://core/v1/containers").CombinedOutput()
	if err != nil {
		t.Fatalf("run curl as nobody: %v: %s", err, out)
	}
	if code := strings.TrimSpace(string(out)); code != "403" {
		t.Errorf("nobody got status %s, want 403", code)
	}
}
