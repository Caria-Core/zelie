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
	// Containers ask their network's gateway for names. A running core
	// already answers there, and then this fails harmlessly.
	ctx, cancel := context.WithCancel(context.Background())
	e.StartDNS(ctx)
	t.Cleanup(cancel)
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

func TestRemoveDeletesLog(t *testing.T) {
	e := connect(t)
	run(t, e, Spec{ID: "it-rmlog", Image: testImage, Args: []string{"echo", "old output"},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	waitForLog(t, "it-rmlog", "old output")
	if err := e.Remove(context.Background(), "it-rmlog"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LogPathFor(DefaultPaths, "it-rmlog")); !os.IsNotExist(err) {
		t.Errorf("log still there after remove: %v", err)
	}
}

// Images such as nginx log to files that are symlinks to /dev/stdout, and
// opening one reopens the container's output pipe.
func TestContainerCanReopenItsOutput(t *testing.T) {
	e := connect(t)
	run(t, e, Spec{ID: "it-reopen", Image: testImage, MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 64,
		Args: []string{"sh", "-c", "echo to-stdout > /dev/stdout && echo to-stderr > /dev/stderr && sleep 60"}})
	waitForLog(t, "it-reopen", "to-stdout")
	waitForLog(t, "it-reopen", "to-stderr")
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
	if s := status(t, e, "it-stop"); s.State != "stopped" {
		t.Errorf("state after stop: %+v", s)
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
	// It was the network's last container, so the network went with it.
	nets, _ := e.networks.all()
	for _, nw := range nets {
		if nw.Name == "it-a" {
			t.Errorf("network it-a is still kept: %+v", nw)
			if _, err := net.InterfaceByName(nw.bridge()); err == nil {
				t.Errorf("bridge %s is still there", nw.bridge())
			}
		}
	}
}

// TestNetworkStaysWhileUsed removes one of two containers on a network: the
// network stays for the other, and goes with it.
func TestNetworkStaysWhileUsed(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	for _, id := range []string{"it-net-one", "it-net-two"} {
		run(t, e, Spec{ID: id, Network: "it-shared", Image: testImage, Args: []string{"sleep", "120"},
			MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	}
	kept := func() (network, bool) {
		nets, _ := e.networks.all()
		for _, nw := range nets {
			if nw.Name == "it-shared" {
				return nw, true
			}
		}
		return network{}, false
	}
	nw, ok := kept()
	if !ok {
		t.Fatal("no network it-shared")
	}
	if err := e.Remove(ctx, "it-net-one"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kept(); !ok {
		t.Fatal("the network went while a container was still on it")
	}
	if _, err := net.InterfaceByName(nw.bridge()); err != nil {
		t.Errorf("bridge %s went while in use: %v", nw.bridge(), err)
	}
	if err := e.Remove(ctx, "it-net-two"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kept(); ok {
		t.Error("the network stayed after its last container")
	}
	if _, err := net.InterfaceByName(nw.bridge()); err == nil {
		t.Errorf("bridge %s stayed", nw.bridge())
	}
}

const builderImage = "docker.io/moby/buildkit:v0.33.0"

// TestBuilderBuildsAnImage builds a Dockerfile inside a builder container,
// imports the result and runs it, which is the path every deploy takes.
func TestBuilderBuildsAnImage(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	// Not under /tmp: on tmpfs the build cache would count as the builder's
	// memory.
	dir, err := os.MkdirTemp("/var/lib/zelie", "it-build-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	for _, sub := range []string{"src", "cache", "out"} {
		os.Mkdir(dir+"/"+sub, 0o700)
		os.Chown(dir+"/"+sub, BuilderHostID, BuilderHostID)
	}
	// The step prints its view of processes: in a sandbox it cannot see the
	// builder.
	dockerfile := "FROM " + testImage + "\nRUN ps -o comm > /ps && echo built > /built\nCMD cat /ps /built; sleep 60\n"
	os.WriteFile(dir+"/src/Dockerfile", []byte(dockerfile), 0o644)

	run(t, e, Spec{
		ID: "it-builder", Image: builderImage, Network: "it-build",
		Args: []string{"buildctl-daemonless.sh", "build", "--frontend", "dockerfile.v0",
			"--local", "context=/src", "--local", "dockerfile=/src",
			"--output", "type=oci,dest=/out/image.tar"},
		Env:         []string{"BUILDKITD_FLAGS=--root /cache --oci-worker-net=host"},
		MemoryBytes: 1 << 30, CPUs: 1, Pids: 1024,
		Mounts: []Mount{
			{Source: dir + "/src", Target: "/src", ReadOnly: true},
			{Source: dir + "/cache", Target: "/cache"},
			{Source: dir + "/out", Target: "/out"},
		},
		Builder: true,
		Nesting: true,
	})
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	code, err := e.Wait(waitCtx, "it-builder")
	if err != nil || code != 0 {
		b, _ := os.ReadFile(LogPathFor(DefaultPaths, "it-builder"))
		t.Fatalf("build exited %d, %v:\n%s", code, err, b)
	}
	if st := status(t, e, "it-builder"); st.Userns != BuilderHostID {
		t.Errorf("builder got ID block %d, want %d", st.Userns, BuilderHostID)
	}

	f, err := os.Open(dir + "/out/image.tar")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := e.ImportImage(ctx, f, LocalImages+"it-built:1"); err != nil {
		t.Fatal(err)
	}
	run(t, e, Spec{ID: "it-built", Image: LocalImages + "it-built:1", MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32})
	out := waitForLog(t, "it-built", "built")
	if strings.Contains(out, "buildkitd") {
		t.Errorf("a build step could see the builder:\n%s", out)
	}

	// Removing the image leaves the container that runs it alone, and a
	// new container can no longer be made from it.
	if err := e.RemoveImage(ctx, LocalImages+"it-built:1"); err != nil {
		t.Fatal(err)
	}
	if err := e.RemoveImage(ctx, LocalImages+"it-built:1"); err != nil {
		t.Errorf("removing it twice: %v", err)
	}
	if st := status(t, e, "it-built"); st.State != "running" && st.State != "stopped" {
		t.Errorf("container after its image was removed: %s", st.State)
	}
	if err := e.Run(ctx, Spec{ID: "it-built-2", Image: LocalImages + "it-built:1", MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32}); err == nil {
		e.Remove(ctx, "it-built-2")
		t.Error("a container was made from a removed image")
	}
}

// An image is marked unused and then deleted, the way the core's sweep does.
func TestUnusedImages(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	const ref = "hello-world:latest"
	name, _ := ImageName(ref)
	run(t, e, Spec{ID: "it-hello", Image: ref, MemoryBytes: 32 << 20, CPUs: 0.5, Pids: 16})
	e.Remove(ctx, "it-hello")

	find := func() (Image, bool) {
		list, err := e.Images(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, img := range list {
			if img.Name == name {
				return img, true
			}
		}
		return Image{}, false
	}
	if img, ok := find(); !ok || !img.UnusedSince.IsZero() {
		t.Fatalf("pulled image: %+v, %v", img, ok)
	}
	since := time.Now().Add(-8 * 24 * time.Hour).Truncate(time.Second)
	if err := e.SetUnused(ctx, name, since); err != nil {
		t.Fatal(err)
	}
	if img, _ := find(); !img.UnusedSince.Equal(since) {
		t.Errorf("unused since %v, want %v", img.UnusedSince, since)
	}
	if err := e.SetUnused(ctx, name, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if img, _ := find(); !img.UnusedSince.IsZero() {
		t.Errorf("mark not cleared: %v", img.UnusedSince)
	}
	if err := e.RemoveImage(ctx, name); err != nil {
		t.Fatal(err)
	}
	if _, ok := find(); ok {
		t.Error("the image is still there")
	}
}

func TestOnlyOneBuilder(t *testing.T) {
	e := connect(t)
	s := Spec{ID: "it-builder-a", Image: testImage, Args: []string{"sleep", "60"}, MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32, Builder: true}
	run(t, e, s)
	s.ID = "it-builder-b"
	if err := e.Run(context.Background(), s); err == nil {
		e.Remove(context.Background(), s.ID)
		t.Fatal("a second builder started with the same ID block")
	}
}

func TestLocalImagesAreNotPulled(t *testing.T) {
	e := connect(t)
	err := e.Run(context.Background(), Spec{ID: "it-nolocal", Image: LocalImages + "does-not-exist:1", MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32})
	if err == nil || strings.Contains(err.Error(), "pull") {
		e.Remove(context.Background(), "it-nolocal")
		t.Fatalf("missing local image: %v", err)
	}
}

func TestShortImageNames(t *testing.T) {
	e := connect(t)
	run(t, e, Spec{ID: "it-short", Image: "busybox:latest", Args: []string{"sh", "-c", "echo short; sleep 60"}, MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 32})
	waitForLog(t, "it-short", "short")
}
