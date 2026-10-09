package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
)

func TestMountPathsMustBeAbsolute(t *testing.T) {
	base := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1}
	for _, m := range []Mount{
		{Source: "relative", Target: "/src"},
		{Source: "/var/lib/x", Target: "src"},
		{Source: "/var/lib/x", Target: "/src/../etc"},
	} {
		s := base
		s.Mounts = []Mount{m}
		if s.Validate() == nil {
			t.Errorf("mount %+v accepted", m)
		}
	}
	base.Mounts = []Mount{{Source: "/var/lib/x", Target: "/src", ReadOnly: true}}
	if err := base.Validate(); err != nil {
		t.Errorf("valid mount rejected: %v", err)
	}
}

func TestOnlyTheBuilderNests(t *testing.T) {
	s := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1, Nesting: true}
	if s.Validate() == nil {
		t.Error("nesting allowed outside the builder")
	}
}

func TestFilesAreChecked(t *testing.T) {
	base := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1}
	for name, s := range map[string]Spec{
		"relative":       {Files: []File{{Target: "install.sh"}}},
		"under /proc":    {Files: []File{{Target: "/proc/x"}}},
		"too large":      {Files: []File{{Target: "/a", Content: make([]byte, MaxFileSize+1)}}},
		"same twice":     {Files: []File{{Target: "/a"}, {Target: "/a"}}},
		"a volume's own": {Volumes: []VolumeMount{{Name: "data", Target: "/a"}}, Files: []File{{Target: "/a"}}},
	} {
		s.ID, s.Image, s.MemoryBytes, s.CPUs, s.Pids = base.ID, base.Image, base.MemoryBytes, base.CPUs, base.Pids
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	base.Files = []File{{Target: "/mnt/install/install.sh", Content: []byte("echo hi\n")}}
	if err := base.Validate(); err != nil {
		t.Errorf("a valid file was rejected: %v", err)
	}
}

func TestUserAndWorkDirAreChecked(t *testing.T) {
	base := Spec{ID: "web", Image: "busybox", MemoryBytes: 1, CPUs: 1, Pids: 1}
	for name, s := range map[string]Spec{
		"user beyond the container's IDs":  {User: &IDs{UID: usernsSize, GID: 0}},
		"group beyond the container's IDs": {User: &IDs{UID: 0, GID: 1 << 30}},
		"relative working directory":       {WorkDir: "home/container"},
		"unclean working directory":        {WorkDir: "/home/../etc"},
		"the builder with a user":          {Builder: true, User: &IDs{UID: 1}},
		"the builder with input":           {Builder: true, Stdin: true},
	} {
		s.ID, s.Image, s.MemoryBytes, s.CPUs, s.Pids = base.ID, base.Image, base.MemoryBytes, base.CPUs, base.Pids
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	base.User, base.WorkDir, base.Stdin = &IDs{UID: 988, GID: 988}, "/home/container", true
	if err := base.Validate(); err != nil {
		t.Errorf("a valid game spec was rejected: %v", err)
	}
}

func TestLimitsLeaveNoRoomForSwapOrForTheCore(t *testing.T) {
	s := Spec{ID: "web", Image: "busybox", MemoryBytes: 256 << 20, CPUs: 0.5, Pids: 64}
	ctx := namespaces.WithNamespace(context.Background(), Namespace)
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, "linux/amd64", &containers.Container{ID: s.ID}, limitOpts(s)...)
	if err != nil {
		t.Fatal(err)
	}
	mem := spec.Linux.Resources.Memory
	if mem.Limit == nil || *mem.Limit != s.MemoryBytes {
		t.Errorf("memory limit = %v, want %d", mem.Limit, s.MemoryBytes)
	}
	// Memory plus swap equal to the limit is what makes memory.swap.max zero.
	if mem.Swap == nil || *mem.Swap != s.MemoryBytes {
		t.Errorf("memory plus swap = %v, want %d", mem.Swap, s.MemoryBytes)
	}
	// containerd's own score is -999 and its children inherit -998. A
	// container must rank above Zelie's processes, which sit at 0.
	if got := spec.Process.OOMScoreAdj; got == nil || *got <= 0 {
		t.Errorf("OOM score adjustment = %v, want above 0", got)
	}
	if got := *spec.Linux.Resources.Pids.Limit; got != s.Pids {
		t.Errorf("process limit = %d, want %d", got, s.Pids)
	}
}

// removedContainer is what containerd's Container answers for a container
// that was deleted after it was listed: its record can no longer be fetched.
type removedContainer struct {
	containerd.Container
	id     string
	listed containers.Container
}

func (c removedContainer) ID() string { return c.id }

func (c removedContainer) Info(_ context.Context, opts ...containerd.InfoOpts) (containers.Container, error) {
	cfg := containerd.InfoConfig{Refresh: true}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.Refresh {
		return containers.Container{}, errdefs.ErrNotFound
	}
	return c.listed, nil
}

func (c removedContainer) Labels(context.Context) (map[string]string, error) {
	return nil, errdefs.ErrNotFound
}

func (c removedContainer) Task(context.Context, cio.Attach) (containerd.Task, error) {
	return nil, errdefs.ErrNotFound
}

func TestScansSurviveAContainerRemovedMeanwhile(t *testing.T) {
	gone := removedContainer{id: "web-1", listed: containers.Container{
		ID:    "web-1",
		Image: "zelie.local/web",
		Labels: map[string]string{
			labelApp: "web", labelNetwork: "web", labelIP: "10.210.3.2", labelUsernsBase: "1073741824",
		},
	}}
	ctx := context.Background()

	labels, err := labelsOf(ctx, gone)
	if err != nil || labels[labelApp] != "web" {
		t.Errorf("labelsOf = %v, %v", labels, err)
	}
	st, err := statusOf(ctx, gone)
	if err != nil {
		t.Fatalf("statusOf: %v", err)
	}
	if st.ID != "web-1" || st.App != "web" || st.Network != "web" || st.IP.String() != "10.210.3.2" || st.State != "stopped" {
		t.Errorf("status = %+v", st)
	}
	if st.Userns != 1<<30 {
		t.Errorf("userns = %d", st.Userns)
	}

	// Only a fresh read fails, which is what List used to do.
	if _, err := gone.Info(ctx); !errors.Is(err, errdefs.ErrNotFound) {
		t.Errorf("a refreshed read should fail here, got %v", err)
	}
}

// Connect refreshes the firewall before it returns, and a failure in that
// first refresh has to reach the log. The logger therefore comes in with
// Connect's options, not as a field set afterwards.
func TestWithLogSetsWhereTheEngineLogs(t *testing.T) {
	var buf bytes.Buffer
	e := &Engine{}
	e.logger().Error("before the option")
	WithLog(slog.New(slog.NewTextHandler(&buf, nil)))(e)
	e.logger().Error("after the option")
	if out := buf.String(); strings.Contains(out, "before") || !strings.Contains(out, "after the option") {
		t.Errorf("log:\n%s", out)
	}
}
