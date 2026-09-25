package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/api/types/runc/options"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/contrib/seccomp"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Namespace is the containerd namespace that holds everything Zelie creates.
const Namespace = "zelie"

const (
	labelUsernsBase = "zelie.userns.base"
	labelNetwork    = "zelie.network"
	labelNetNS      = "zelie.netns"
	labelIP         = "zelie.ip"

	// Each container gets its own block of 65536 host IDs, starting well above
	// any range a distribution hands out to regular users or /etc/subuid.
	usernsFirst = 1 << 30
	usernsSize  = 65536
	usernsMax   = 32768 // blocks, which keeps every ID below 2^31

	// The builder always gets the last block, so the files in its cache
	// keep belonging to it from one build to the next.
	builderBase = usernsFirst + (usernsMax-1)*usernsSize
)

// BuilderHostID is the host user and group that root inside the builder is.
// Directories the builder writes to must belong to it.
const BuilderHostID = builderBase

// LocalImages is the prefix of images built on this machine. They are never
// pulled from a registry.
const LocalImages = "zelie.local/"

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidID reports whether id is safe to use as a container name and in paths.
func ValidID(id string) bool { return validID.MatchString(id) }

// Spec describes a container to run.
type Spec struct {
	ID    string
	Image string
	Args  []string // replaces the image's command when set
	Env   []string

	// Network is the network to join. Containers on the same network can
	// reach each other. Without one the container has only loopback.
	Network string

	MemoryBytes int64   // required
	CPUs        float64 // required, may be fractional
	Pids        int64   // required

	// Mounts are host directories bound into the container. They are for
	// the core's own use, such as a build's source and cache, and are not
	// part of the core's API.
	Mounts []Mount

	// Builder prepares the container to build images: see builderOpts. Like
	// Mounts, only the core sets it.
	Builder bool
}

// Mount binds a host directory into a container.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

// Validate checks the spec before anything is created.
func (s Spec) Validate() error {
	switch {
	case !validID.MatchString(s.ID):
		return fmt.Errorf("container id %q must be lowercase letters, digits and dashes", s.ID)
	case s.Image == "":
		return errors.New("image is required")
	case s.Network != "" && !validID.MatchString(s.Network):
		return fmt.Errorf("network name %q must be lowercase letters, digits and dashes", s.Network)
	case s.MemoryBytes <= 0:
		return errors.New("a memory limit is required")
	case s.CPUs <= 0:
		return errors.New("a CPU limit is required")
	case s.Pids <= 0:
		return errors.New("a process limit is required")
	}
	for _, m := range s.Mounts {
		if !filepath.IsAbs(m.Source) || !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target {
			return fmt.Errorf("mount %s on %s: paths must be absolute and clean", m.Source, m.Target)
		}
	}
	return nil
}

// Engine runs containers through Zelie's containerd.
type Engine struct {
	client   *containerd.Client
	paths    Paths
	networks *networks
	resolv   string

	// createMu makes creating containers one at a time, so two requests
	// can never pick the same ID range or race on the same name.
	createMu sync.Mutex
}

func Connect(ctx context.Context, p Paths) (*Engine, error) {
	c, err := containerd.New(p.Socket, containerd.WithDefaultNamespace(Namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", p.Socket, err)
	}
	resolv, err := resolvConf(p)
	if err != nil {
		c.Close()
		return nil, err
	}
	if err := guardHost(); err != nil {
		c.Close()
		return nil, err
	}
	return &Engine{client: c, paths: p, networks: newNetworks(p), resolv: resolv}, nil
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
func (e *Engine) Run(ctx context.Context, s Spec) (err error) {
	if err := s.Validate(); err != nil {
		return err
	}
	ctx = e.ctx(ctx)

	image, err := e.image(ctx, s.Image)
	if err != nil {
		return err
	}

	e.createMu.Lock()
	defer e.createMu.Unlock()

	// Check first: the cleanup below must never touch an existing container's
	// network or files.
	if _, err := e.client.LoadContainer(ctx, s.ID); err == nil {
		return fmt.Errorf("container %s: %w", s.ID, errdefs.ErrAlreadyExists)
	}

	var cleanup []func()
	defer func() {
		if err != nil {
			for i := len(cleanup) - 1; i >= 0; i-- {
				cleanup[i]()
			}
		}
	}()

	base := uint32(builderBase)
	if !s.Builder {
		if base, err = e.freeUsernsBase(ctx); err != nil {
			return err
		}
	} else if err := e.builderBaseFree(ctx); err != nil {
		return err
	}
	idmap := []specs.LinuxIDMapping{{ContainerID: 0, HostID: base, Size: usernsSize}}

	// The hosts file is bind-mounted, so it must exist before the task is
	// created. Its content is filled in once the container has an address.
	hostsFile := filepath.Join(e.containerDir(s.ID), "hosts")
	if err := os.MkdirAll(e.containerDir(s.ID), 0o755); err != nil {
		return err
	}
	cleanup = append(cleanup, func() { os.RemoveAll(e.containerDir(s.ID)) })
	if err := os.WriteFile(hostsFile, nil, 0o644); err != nil {
		return err
	}

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
		// A private cgroup namespace lets the container read its own limits
		// from /sys/fs/cgroup. Runtimes such as the JVM size themselves from
		// those files and would otherwise assume the whole machine.
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.CgroupNamespace}),
		oci.WithMounts([]specs.Mount{
			{
				Destination: "/sys/fs/cgroup",
				Type:        "cgroup",
				Source:      "cgroup",
				Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
			},
			{Destination: "/etc/resolv.conf", Type: "bind", Source: e.resolv, Options: []string{"rbind", "ro"}},
			{Destination: "/etc/hosts", Type: "bind", Source: hostsFile, Options: []string{"rbind", "ro"}},
		}),
	}
	for _, m := range s.Mounts {
		opts := []string{"rbind", "rw", "nosuid", "nodev"}
		if m.ReadOnly {
			opts[1] = "ro"
		}
		specOpts = append(specOpts, oci.WithMounts([]specs.Mount{{Destination: m.Target, Type: "bind", Source: m.Source, Options: opts}}))
	}
	if s.Builder {
		specOpts = append(specOpts, builderOpts)
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
			// The container's output goes through pipes the shim creates.
			// Images such as nginx log to /dev/stdout, and opening that
			// reopens the pipe, which only its owner may do. Owned by host
			// root, that would fail for the container's root.
			IoUid: base,
			IoGid: base,
		}),
		containerd.WithContainerLabels(map[string]string{labelUsernsBase: strconv.FormatUint(uint64(base), 10)}),
		containerd.WithNewSpec(specOpts...),
	)
	if err != nil {
		return fmt.Errorf("create container %s: %w", s.ID, err)
	}
	cleanup = append(cleanup, func() { container.Delete(context.WithoutCancel(ctx), containerd.WithSnapshotCleanup) })

	if err := os.MkdirAll(e.paths.Logs, 0o700); err != nil {
		return err
	}
	// Creating the task sets up the namespaces but does not start the
	// process yet. runc creates the network namespace inside the container's
	// user namespace, which the kernel requires before the container may
	// mount its own /sys.
	task, err := container.NewTask(ctx, cio.LogFile(LogPathFor(e.paths, s.ID)))
	if err != nil {
		return fmt.Errorf("create task %s: %w", s.ID, err)
	}
	cleanup = append(cleanup, func() { task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill) })

	// Pin the namespace to a file so it outlives this call and can be
	// detached from the network when the container is removed.
	ns, err := netns.NewNetNSFromPID(e.paths.NetNS, task.Pid())
	if err != nil {
		return fmt.Errorf("pin network namespace of %s: %w", s.ID, err)
	}
	cleanup = append(cleanup, func() { ns.Remove() })
	labels := map[string]string{labelNetNS: ns.GetPath()}

	hosts := "127.0.0.1\tlocalhost\n::1\tlocalhost\n"
	if s.Network != "" {
		ip, err := e.networks.attach(ctx, s.Network, s.ID, ns.GetPath())
		if err != nil {
			return err
		}
		cleanup = append(cleanup, func() { e.networks.detach(context.WithoutCancel(ctx), s.Network, s.ID, ns.GetPath()) })
		labels[labelNetwork] = s.Network
		labels[labelIP] = ip.String()
		hosts += fmt.Sprintf("%s\t%s\n", ip, s.ID)
	}
	// Written in place: replacing the file would leave the bind mount
	// pointing at the old, empty one.
	if err := os.WriteFile(hostsFile, []byte(hosts), 0o644); err != nil {
		return err
	}
	if _, err := container.SetLabels(ctx, labels); err != nil {
		return err
	}

	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	return nil
}

// image returns an image ready to run. Local images must have been imported;
// anything else is pulled.
func (e *Engine) image(ctx context.Context, ref string) (containerd.Image, error) {
	if !strings.HasPrefix(ref, LocalImages) {
		image, err := e.client.Pull(ctx, ref, containerd.WithPullUnpack)
		if err != nil {
			return nil, fmt.Errorf("pull %s: %w", ref, err)
		}
		return image, nil
	}
	image, err := e.client.GetImage(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	if ok, err := image.IsUnpacked(ctx, defaults.DefaultSnapshotter); err != nil {
		return nil, err
	} else if !ok {
		if err := image.Unpack(ctx, defaults.DefaultSnapshotter); err != nil {
			return nil, fmt.Errorf("unpack %s: %w", ref, err)
		}
	}
	return image, nil
}

// ImportImage stores an OCI image archive, such as a build's output, under
// name, which must start with LocalImages.
func (e *Engine) ImportImage(ctx context.Context, r io.Reader, name string) error {
	if !strings.HasPrefix(name, LocalImages) {
		return fmt.Errorf("local image names start with %s", LocalImages)
	}
	ctx = e.ctx(ctx)
	// Replacing an image of the same name is fine: containers keep their
	// snapshot, not the name.
	if _, err := e.client.Import(ctx, r, containerd.WithIndexName(name)); err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	_, err := e.image(ctx, name)
	return err
}

// builderOpts gives a builder what it needs to run build steps as containers
// of its own. Everything added is confined to the container's user
// namespace; none of it is a privilege on the host. With these BuildKit runs
// each step in its own sandbox, where the step cannot see the builder.
var builderOpts oci.SpecOpts = func(ctx context.Context, c oci.Client, ctr *containers.Container, s *specs.Spec) error {
	// Mounting the root file system and procfs of a nested container.
	// MKNOD and NET_RAW are dropped from other containers but BuildKit
	// hands them to build steps, which fails if the builder lacks them.
	if err := oci.WithAddedCapabilities([]string{"CAP_SYS_ADMIN", "CAP_MKNOD", "CAP_NET_RAW"})(ctx, c, ctr, s); err != nil {
		return err
	}
	// Mounting a fresh procfs for a step requires a /proc without masked
	// paths. Inside a user namespace the kernel still refuses writes to
	// global /proc/sys settings and reads of /proc/kcore.
	s.Linux.MaskedPaths = nil
	s.Linux.ReadonlyPaths = nil
	// The default profile is built from the capabilities, so it is made
	// again now that they changed.
	profile := seccomp.DefaultProfile(s)
	enosys := uint(syscall.ENOSYS)
	profile.Syscalls = append(profile.Syscalls,
		// runc moves into the new root with pivot_root, which the default
		// profile does not allow even with SYS_ADMIN.
		specs.LinuxSyscall{Names: []string{"pivot_root"}, Action: specs.ActAllow},
		// Kernel keyrings are not namespaced and stay blocked. runc gives up
		// on its session keyring when these are missing but fails when they
		// are refused, so they answer "not implemented".
		specs.LinuxSyscall{Names: []string{"keyctl", "add_key", "request_key"}, Action: specs.ActErrno, ErrnoRet: &enosys},
	)
	s.Linux.Seccomp = profile
	return nil
}

// builderBaseFree makes sure no other builder holds the builder's ID block.
func (e *Engine) builderBaseFree(ctx context.Context) error {
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		labels, err := c.Labels(ctx)
		if err != nil {
			return err
		}
		if labels[labelUsernsBase] == strconv.Itoa(builderBase) {
			return fmt.Errorf("builder %s: %w", c.ID(), errdefs.ErrAlreadyExists)
		}
	}
	return nil
}

func (e *Engine) containerDir(id string) string {
	return filepath.Join(e.paths.Data, "containers", id)
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
	for i := range usernsMax - 1 {
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

// Remove stops the container if needed and deletes it with its snapshot and
// its log.
func (e *Engine) Remove(ctx context.Context, id string) error {
	if err := e.Stop(ctx, id, 10*time.Second); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	ctx = e.ctx(ctx)
	container, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	labels, err := container.Labels(ctx)
	if err != nil {
		return err
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return err
	}
	// The container is gone; what follows only frees its network and files.
	var errs []error
	if path := labels[labelNetNS]; path != "" {
		if name := labels[labelNetwork]; name != "" {
			errs = append(errs, e.networks.detach(ctx, name, id, path))
		}
		errs = append(errs, netns.LoadNetNS(path).Remove())
	}
	errs = append(errs, os.RemoveAll(e.containerDir(id)))
	// A new container with the same name must not start with the old one's
	// output.
	if err := os.Remove(LogPathFor(e.paths, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Wait waits until the container's process has exited and returns its exit
// code.
func (e *Engine) Wait(ctx context.Context, id string) (uint32, error) {
	ctx = e.ctx(ctx)
	container, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return 0, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, err
	}
	exited, err := task.Wait(ctx)
	if err != nil {
		return 0, err
	}
	select {
	case st := <-exited:
		return st.ExitCode(), st.Error()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Status is a short summary of a container.
type Status struct {
	ID      string
	Image   string
	State   string // running, stopped, created, …
	Pid     uint32
	Userns  uint32 // first host ID of the container's user namespace
	Network string
	IP      netip.Addr // zero when the container has no network
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
		// A container without a task has been stopped: Stop deletes the
		// task once the process has exited.
		st := Status{ID: c.ID(), Image: info.Image, State: "stopped"}
		if v, err := strconv.ParseUint(info.Labels[labelUsernsBase], 10, 32); err == nil {
			st.Userns = uint32(v)
		}
		st.Network = info.Labels[labelNetwork]
		st.IP, _ = netip.ParseAddr(info.Labels[labelIP])
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
