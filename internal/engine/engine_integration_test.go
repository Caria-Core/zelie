//go:build integration

// These tests need root, a Linux machine and Zelie's containerd running
// (zelie engine install). Run them with hack/vm-test.sh.

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const testImage = "docker.io/library/busybox:latest"

func connect(t *testing.T) *Engine {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(DefaultPaths.Socket); err != nil {
		t.Skip("Zelie's containerd is not running")
	}
	e, err := Connect(context.Background(), DefaultPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

func run(t *testing.T, e *Engine, s Spec) {
	t.Helper()
	ctx := context.Background()
	e.Remove(ctx, s.ID) // left over from an earlier failed run
	os.Remove(LogPathFor(DefaultPaths, s.ID))
	if err := e.Run(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Remove(context.Background(), s.ID) })
}

// waitForLog waits until the container has written want to its log.
func waitForLog(t *testing.T, id, want string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		b, _ := os.ReadFile(LogPathFor(DefaultPaths, id))
		if strings.Contains(string(b), want) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("log of %s never contained %q, got:\n%s", id, want, b)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func status(t *testing.T, e *Engine, id string) Status {
	t.Helper()
	list, err := e.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("container %s not listed", id)
	return Status{}
}

func TestContainerIsConfined(t *testing.T) {
	e := connect(t)
	script := `cat /proc/self/uid_map; cat /sys/fs/cgroup/memory.max /sys/fs/cgroup/cpu.max /sys/fs/cgroup/pids.max
touch /sys/fs/cgroup/x 2>/dev/null && echo cgroup-writable
grep -E '^(CapEff|NoNewPrivs|Seccomp):' /proc/self/status
echo done; sleep 60`
	run(t, e, Spec{ID: "it-confined", Image: testImage, Args: []string{"sh", "-c", script},
		MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32})
	out := waitForLog(t, "it-confined", "done")

	st := status(t, e, "it-confined")
	if want := fmt.Sprintf("0 %d 65536", st.Userns); !strings.Contains(strings.Join(strings.Fields(out), " "), want) {
		t.Errorf("uid map should be %q, log:\n%s", want, out)
	}
	for _, want := range []string{"67108864", "50000 100000", "32", "NoNewPrivs:\t1", "Seccomp:\t2"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cgroup-writable") {
		t.Error("the container can write to its cgroup")
	}

	// Root inside must be an unprivileged user outside.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", st.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), fmt.Sprintf("Uid:\t%d", st.Userns)) {
		t.Errorf("host uid of the container process is not %d", st.Userns)
	}
}

func TestContainersGetSeparateIDRanges(t *testing.T) {
	e := connect(t)
	spec := Spec{Image: testImage, Args: []string{"sleep", "60"}, MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8}
	a, b := spec, spec
	a.ID, b.ID = "it-range-a", "it-range-b"
	run(t, e, a)
	run(t, e, b)
	if ra, rb := status(t, e, a.ID).Userns, status(t, e, b.ID).Userns; ra == rb || ra == 0 || rb == 0 {
		t.Errorf("containers share or lack an ID range: %d and %d", ra, rb)
	}
}

func TestContainerSurvivesContainerdRestart(t *testing.T) {
	e := connect(t)
	run(t, e, Spec{ID: "it-survive", Image: testImage, Args: []string{"sleep", "120"},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	before := status(t, e, "it-survive").Pid

	if out, err := exec.Command("systemctl", "restart", "zelie-containerd").CombinedOutput(); err != nil {
		t.Fatalf("restart containerd: %v: %s", err, out)
	}
	// The client reconnects on its own, but give containerd a moment.
	var after Status
	deadline := time.Now().Add(20 * time.Second)
	for {
		list, err := e.List(context.Background())
		if err == nil {
			for _, s := range list {
				if s.ID == "it-survive" {
					after = s
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("containerd did not come back: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if after.State != "running" || after.Pid != before {
		t.Errorf("container did not survive: before pid %d, after %+v", before, after)
	}
}

func TestStopEscalatesToKill(t *testing.T) {
	e := connect(t)
	// A shell running as PID 1 ignores SIGTERM, so only SIGKILL stops it.
	run(t, e, Spec{ID: "it-stop", Image: testImage, Args: []string{"sh", "-c", "sleep 600"},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	start := time.Now()
	if err := e.Stop(context.Background(), "it-stop", time.Second); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("stop took %v with a one second grace period", took)
	}
	if s := status(t, e, "it-stop"); s.State == "running" {
		t.Errorf("still running after stop: %+v", s)
	}
}
