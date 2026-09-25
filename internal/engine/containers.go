package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/runc/options"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Namespace is the containerd namespace that holds everything Zelie creates.
const Namespace = "zelie"

const (
	labelUsernsBase = "zelie.userns.base"

	// Each container gets its own block of 65536 host IDs, starting well above
	// any range a distribution hands out to regular users or /etc/subuid.
	usernsFirst = 1 << 30
	usernsSize  = 65536
	usernsMax   = 32768 // blocks, which keeps every ID below 2^31
)

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidID reports whether id is safe to use as a container name and in paths.
func ValidID(id string) bool { return validID.MatchString(id) }

// Spec describes a container to run.
type Spec struct {
	ID    string
	Image string
	Args  []string // replaces the image's command when set
	Env   []string

	MemoryBytes int64   // required
	CPUs        float64 // required, may be fractional
	Pids        int64   // required
}

func (s Spec) validate() error {
	switch {
	case !validID.MatchString(s.ID):
		return fmt.Errorf("container id %q must be lowercase letters, digits and dashes", s.ID)
	case s.Image == "":
		return errors.New("image is required")
	case s.MemoryBytes <= 0:
		return errors.New("a memory limit is required")
	case s.CPUs <= 0:
		return errors.New("a CPU limit is required")
	case s.Pids <= 0:
		return errors.New("a process limit is required")
	}
	return nil
}

// Engine runs containers through Zelie's containerd.
type Engine struct {
	client *containerd.Client
	paths  Paths
}

func Connect(ctx context.Context, p Paths) (*Engine, error) {
	c, err := containerd.New(p.Socket, containerd.WithDefaultNamespace(Namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", p.Socket, err)
	}
	return &Engine{client: c, paths: p}, nil
}

func (e *Engine) Close() error { return e.client.Close() }

func (e *Engine) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, Namespace)
}

// LogPathFor is where a container's output goes. The shim writes to it
// directly, so logging keeps working while containerd or Zelie restarts.
func LogPathFor(p Paths, id string) string {
	return filepath.Join(p.Logs, id+".log")
}

// Run pulls the image if needed, creates the container and starts it.
func (e *Engine) Run(ctx context.Context, s Spec) error {
	if err := s.validate(); err != nil {
		return err
	}
	ctx = e.ctx(ctx)

	image, err := e.client.Pull(ctx, s.Image, containerd.WithPullUnpack)
	if err != nil {
		return fmt.Errorf("pull %s: %w", s.Image, err)
	}

	base, err := e.freeUsernsBase(ctx)
	if err != nil {
		return err
	}
	idmap := []specs.LinuxIDMapping{{ContainerID: 0, HostID: base, Size: usernsSize}}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithHostname(s.ID),
		oci.WithUserNamespace(idmap, idmap),
		oci.WithNoNewPrivileges,
		seccomp.WithDefaultProfile(),
		oci.WithDroppedCapabilities([]string{"CAP_NET_RAW", "CAP_MKNOD"}),
		oci.WithMemoryLimit(uint64(s.MemoryBytes)),
		oci.WithCPUCFS(int64(s.CPUs*100000), 100000),
		oci.WithPidsLimit(s.Pids),
		oci.WithCgroup("zelie.slice:zelie:" + s.ID),
	}
	if len(s.Args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(s.Args...))
	}
	if len(s.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(s.Env))
	}

	container, err := e.client.NewContainer(ctx, s.ID,
		containerd.WithImage(image),
		// With idmapped mounts the image files are mapped, not copied. On
		// kernels without them containerd falls back to a chowned copy.
		containerd.WithNewSnapshot(s.ID, image, containerd.WithUserNSRemapperLabels(idmap, idmap)),
		containerd.WithRuntime("io.containerd.runc.v2", &options.Options{
			BinaryName:    e.paths.Runc(),
			SystemdCgroup: true,
		}),
		containerd.WithContainerLabels(map[string]string{labelUsernsBase: strconv.FormatUint(uint64(base), 10)}),
		containerd.WithNewSpec(specOpts...),
	)
	if err != nil {
		return fmt.Errorf("create container %s: %w", s.ID, err)
	}

	if err := os.MkdirAll(e.paths.Logs, 0o700); err != nil {
		return err
	}
	task, err := container.NewTask(ctx, cio.LogFile(LogPathFor(e.paths, s.ID)))
	if err != nil {
		container.Delete(ctx, containerd.WithSnapshotCleanup)
		return fmt.Errorf("create task %s: %w", s.ID, err)
	}
	if err := task.Start(ctx); err != nil {
		task.Delete(ctx)
		container.Delete(ctx, containerd.WithSnapshotCleanup)
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	return nil
}

// freeUsernsBase finds an ID block no other container uses. The block is
// stored as a container label, so containerd is the only record of it.
func (e *Engine) freeUsernsBase(ctx context.Context) (uint32, error) {
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return 0, err
	}
	used := make(map[uint32]bool, len(containers))
	for _, c := range containers {
		labels, err := c.Labels(ctx)
		if err != nil {
			return 0, err
		}
		if v, err := strconv.ParseUint(labels[labelUsernsBase], 10, 32); err == nil {
			used[uint32(v)] = true
		}
	}
	for i := range usernsMax {
		base := uint32(usernsFirst + i*usernsSize)
		if !used[base] {
			return base, nil
		}
	}
	return 0, errors.New("no free user namespace range left")
}

// Stop asks the container to exit and kills it if it has not exited within
// the grace period.
func (e *Engine) Stop(ctx context.Context, id string, grace time.Duration) error {
	ctx = e.ctx(ctx)
	container, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := container.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	exited, err := task.Wait(ctx)
	if err != nil {
		return err
	}
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	select {
	case <-exited:
	case <-time.After(grace):
		if err := task.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		<-exited
	case <-ctx.Done():
		return ctx.Err()
	}
	_, err = task.Delete(ctx)
	return err
}

// Remove stops the container if needed and deletes it with its snapshot.
func (e *Engine) Remove(ctx context.Context, id string) error {
	if err := e.Stop(ctx, id, 10*time.Second); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	ctx = e.ctx(ctx)
	container, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	return container.Delete(ctx, containerd.WithSnapshotCleanup)
}

// Status is a short summary of a container.
type Status struct {
	ID     string
	Image  string
	State  string // running, stopped, created, …
	Pid    uint32
	Userns uint32 // first host ID of the container's user namespace
}

func (e *Engine) List(ctx context.Context) ([]Status, error) {
	ctx = e.ctx(ctx)
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(containers))
	for _, c := range containers {
		info, err := c.Info(ctx)
		if err != nil {
			return nil, err
		}
		st := Status{ID: c.ID(), Image: info.Image, State: "created"}
		if v, err := strconv.ParseUint(info.Labels[labelUsernsBase], 10, 32); err == nil {
			st.Userns = uint32(v)
		}
		if task, err := c.Task(ctx, nil); err == nil {
			if s, err := task.Status(ctx); err == nil {
				st.State = string(s.Status)
			}
			st.Pid = task.Pid()
		}
		out = append(out, st)
	}
	return out, nil
}
