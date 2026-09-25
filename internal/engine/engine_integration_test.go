//go:build integration

// These tests need root, a Linux machine and Zelie's containerd running
// (zelie engine install). Run them with hack/vm-test.sh.

package engine

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/netns"
	cnins "github.com/containernetworking/plugins/pkg/ns"
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

// dialFrom opens a TCP connection from inside a container's network
// namespace.
func dialFrom(t *testing.T, pid uint32, addr string) error {
	t.Helper()
	var dialErr error
	err := netns.LoadNetNS(fmt.Sprintf("/proc/%d/ns/net", pid)).Do(func(cnins.NetNS) error {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			c.Close()
		}
		dialErr = err
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dialErr
}

func TestNetworkIsolation(t *testing.T) {
	e := connect(t)
	small := Spec{Image: testImage, MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 16}
	web, same, other, none := small, small, small, small
	web.ID, web.Network, web.Args = "it-net-web", "it-a", []string{"httpd", "-f", "-p", "8080"}
	same.ID, same.Network, same.Args = "it-net-same", "it-a", []string{"sleep", "120"}
	other.ID, other.Network, other.Args = "it-net-other", "it-b", []string{"sleep", "120"}
	none.ID, none.Args = "it-net-none", []string{"sleep", "120"}
	for _, s := range []Spec{web, same, other, none} {
		run(t, e, s)
	}
	webIP := status(t, e, web.ID).IP
	if !webIP.IsValid() {
		t.Fatal("web container has no address")
	}
	target := net.JoinHostPort(webIP.String(), "8080")

	if err := dialFrom(t, status(t, e, same.ID).Pid, target); err != nil {
		t.Errorf("same network should connect: %v", err)
	}
	if err := dialFrom(t, status(t, e, other.ID).Pid, target); err == nil {
		t.Error("a container on another network reached the web container")
	}
	if err := dialFrom(t, status(t, e, none.ID).Pid, target); err == nil {
		t.Error("a container without a network reached the web container")
	}

	// The host reaches containers, which the proxy relies on.
	if c, err := net.DialTimeout("tcp", target, 2*time.Second); err != nil {
		t.Errorf("host cannot reach the container: %v", err)
	} else {
		c.Close()
	}

	// Containers must not reach services on the host.
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	gateway := webIP.As4()
	gateway[3] = 1
	port := l.Addr().(*net.TCPAddr).Port
	hostAddr := net.JoinHostPort(netip.AddrFrom4(gateway).String(), fmt.Sprint(port))
	if err := dialFrom(t, status(t, e, same.ID).Pid, hostAddr); err == nil {
		t.Errorf("a container reached a service on the host at %s", hostAddr)
	}
}

func TestRemoveFreesNetwork(t *testing.T) {
	e := connect(t)
	run(t, e, Spec{ID: "it-net-free", Network: "it-a", Image: testImage, Args: []string{"sleep", "120"},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	ip := status(t, e, "it-net-free").IP.String()
	if err := e.Remove(context.Background(), "it-net-free"); err != nil {
		t.Fatal(err)
	}
	b, _ := exec.Command("sh", "-c", "grep -rlx it-net-free "+DefaultPaths.Data+"/ipam || true").Output()
	if strings.TrimSpace(string(b)) != "" {
		t.Errorf("IP lease %s still held after remove: %s", ip, b)
	}
}
