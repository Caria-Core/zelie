package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
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
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/netns"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Namespace is the containerd namespace that holds everything Zelie creates.
const Namespace = "zelie"

const (
	labelUsernsBase = "zelie.userns.base"
	labelNetwork    = "zelie.network"
	labelNetNS      = "zelie.netns"
	labelIP         = "zelie.ip"
	labelApp        = "zelie.app"
	labelVolumes    = "zelie.volumes" // comma separated

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

var validID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`) })

// ValidID reports whether id is safe to use as a container name and in paths.
func ValidID(id string) bool { return validID().MatchString(id) }

// Spec describes a container to run.
type Spec struct {
	ID    string
	App   string // the app the container belongs to, if any
	Image string
	Args  []string // replaces the image's command when set
	Env   []string

	// Network is the network to join. Containers on the same network can
	// reach each other. Without one the container has only loopback.
	Network string

	MemoryBytes int64   // required
	CPUs        float64 // required, may be fractional
	Pids        int64   // required

	// Volumes are the app's own files that outlive the container.
	Volumes []VolumeMount

	// Mounts are host directories bound into the container. They are for
	// the core's own use, such as a build's source and cache, and are not
	// part of the core's API.
	Mounts []Mount

	// Files are written by the core and bound read-only into the container,
	// such as an install script. They live in the container's own
	// directory and go when it does. Like Mounts, only the core sets them.
	Files []File

	// User is the user and group the process runs as inside the container,
	// in place of the image's own. Game servers run as an ordinary user, the
	// way Pterodactyl's images expect.
	User *IDs
	// WorkDir replaces the image's working directory.
	WorkDir string
	// OpenFiles is the limit on open files, soft and hard, up to
	// MaxOpenFiles. Zero leaves containerd's default of 1024. It is
	// lowered to what containerd itself may give.
	OpenFiles uint64
	// Stdin keeps the container's standard input open for WriteStdin. It
	// stays open until the container is stopped or removed.
	Stdin bool

	// Builder runs the container as the builder: in the builder's own ID
	// block, so what it writes stays readable by the next build step. Only
	// one builder container exists at a time. Like Mounts, only the core
	// sets it.
	Builder bool
	// Nesting lets a builder run containers of its own: see nestingOpts.
	Nesting bool
}

// IDs is a user and group inside a container.
type IDs struct{ UID, GID uint32 }

// Valid reports whether both are IDs the container has. It has one block of
// them; anything beyond is nobody.
func (i IDs) Valid() bool { return i.UID < usernsSize && i.GID < usernsSize }

// Mount binds a host directory into a container.
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

// File is a file to put in a container. It belongs to root on the host and
// is readable by everyone, so the container's own root can read it.
type File struct {
	Target  string
	Content []byte
}

// MaxFileSize is the largest File the engine writes.
const MaxFileSize = 512 << 10

// Validate checks the spec before anything is created.
func (s Spec) Validate() error {
	switch {
	case !validID().MatchString(s.ID):
		return fmt.Errorf("container id %q must be lowercase letters, digits and dashes", s.ID)
	case s.Image == "":
		return errors.New("image is required")
	case s.App != "" && !validID().MatchString(s.App):
		return fmt.Errorf("app id %q must be lowercase letters, digits and dashes", s.App)
	case s.Network != "" && !validID().MatchString(s.Network):
		return fmt.Errorf("network name %q must be lowercase letters, digits and dashes", s.Network)
	case s.MemoryBytes <= 0:
		return errors.New("a memory limit is required")
	case s.CPUs <= 0:
		return errors.New("a CPU limit is required")
	case s.Pids <= 0:
		return errors.New("a process limit is required")
	}
	if s.Nesting && !s.Builder {
		return errors.New("only the builder may run nested containers")
	}
	if s.Builder && (s.User != nil || s.Stdin) {
		return errors.New("the builder has no user or input of its own")
	}
	if s.User != nil && !s.User.Valid() {
		return fmt.Errorf("user %d:%d is outside the container's IDs", s.User.UID, s.User.GID)
	}
	if s.OpenFiles > MaxOpenFiles {
		return fmt.Errorf("the limit on open files is at most %d", MaxOpenFiles)
	}
	if s.WorkDir != "" && (!filepath.IsAbs(s.WorkDir) || filepath.Clean(s.WorkDir) != s.WorkDir) {
		return fmt.Errorf("working directory %q must be an absolute, clean path", s.WorkDir)
	}
	targets := map[string]bool{}
	for _, v := range s.Volumes {
		if !validID().MatchString(v.Name) {
			return fmt.Errorf("volume name %q must be lowercase letters, digits and dashes", v.Name)
		}
		if err := CheckVolumeTarget(v.Target); err != nil {
			return err
		}
		if targets[v.Target] {
			return fmt.Errorf("two volumes are mounted on %s", v.Target)
		}
		targets[v.Target] = true
	}
	if s.Builder && len(s.Volumes) > 0 {
		return errors.New("the builder has no volumes")
	}
	for _, m := range s.Mounts {
		if !filepath.IsAbs(m.Source) || !filepath.IsAbs(m.Target) || filepath.Clean(m.Target) != m.Target {
			return fmt.Errorf("mount %s on %s: paths must be absolute and clean", m.Source, m.Target)
		}
	}
	for _, f := range s.Files {
		if err := CheckVolumeTarget(f.Target); err != nil {
			return fmt.Errorf("file %s: %w", f.Target, err)
		}
		if len(f.Content) > MaxFileSize {
			return fmt.Errorf("file %s is larger than %d KiB", f.Target, MaxFileSize>>10)
		}
		if targets[f.Target] {
			return fmt.Errorf("two mounts are on %s", f.Target)
		}
		targets[f.Target] = true
	}
	return nil
}

// Engine runs containers through Zelie's containerd.
type Engine struct {
	// log receives what the engine does on its own, such as putting back
	// firewall rules that were removed. Nil discards it.
	log *slog.Logger

	client   *containerd.Client
	paths    Paths
	networks *networks
	peers    peers
	dns      *dnsServer

	// createMu makes creating containers one at a time, so two requests
	// can never pick the same ID range or race on the same name.
	createMu sync.Mutex

	logs logCap
}

// Option changes how Connect sets up the engine.
type Option func(*Engine)

// WithLog sends what the engine does on its own to l. It has to be given to
// Connect: the first firewall refresh runs before Connect returns, and what
// goes wrong in it would otherwise be lost.
func WithLog(l *slog.Logger) Option { return func(e *Engine) { e.log = l } }

func Connect(ctx context.Context, p Paths, opts ...Option) (*Engine, error) {
	c, err := containerd.New(p.Socket, containerd.WithDefaultNamespace(Namespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd at %s: %w", p.Socket, err)
	}
	servers, err := upstreamDNS()
	if err != nil {
		c.Close()
		return nil, err
	}
	upstream := make([]string, len(servers))
	for i, s := range servers {
		upstream[i] = net.JoinHostPort(s, "53")
	}
	e := &Engine{client: c, paths: p, networks: newNetworks(p)}
	for _, o := range opts {
		o(e)
	}
	e.dns = newDNSServer(e.lookup, upstream)
	e.logs.rules = e.keepHostRules
	if err := e.refresh(ctx); err != nil {
		c.Close()
		return nil, err
	}
	e.restoreHolders(ctx)
	e.removeLeftovers()
	return e, nil
}

func (e *Engine) Close() error { return e.client.Close() }

func (e *Engine) logger() *slog.Logger {
	if e.log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return e.log
}

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
	// Runs last, once the container and its address are gone.
	cleanup = append(cleanup, func() { e.refresh(context.WithoutCancel(ctx)) })
	cleanup = append(cleanup, func() { os.RemoveAll(e.containerDir(s.ID)) })
	if err := os.WriteFile(hostsFile, nil, 0o644); err != nil {
		return err
	}
	// Without a network there is no one to ask.
	resolv := filepath.Join(e.containerDir(s.ID), "resolv.conf")
	if err := os.WriteFile(resolv, nil, 0o644); err != nil {
		return err
	}
	if s.Network != "" {
		nw, err := e.networks.ensure(s.Network)
		if err != nil {
			return err
		}
		// Runs once the container is gone, so a network this call made does
		// not outlive a failed start. A network other containers use is kept.
		cleanup = append(cleanup, func() {
			if err := e.freeNetwork(context.WithoutCancel(ctx), s.Network); err != nil {
				e.logger().Error("free the network of a container that did not start", "network", s.Network, "err", err)
			}
		})
		if resolv, err = resolvConf(e.paths, nw); err != nil {
			return err
		}
		if err := e.dns.listen(nw.gateway()); err != nil {
			return fmt.Errorf("DNS server for network %s: %w", s.Network, err)
		}
	}

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(image),
		oci.WithHostname(s.ID),
		oci.WithUserNamespace(idmap, idmap),
		oci.WithNoNewPrivileges,
		seccomp.WithDefaultProfile(),
		oci.WithDroppedCapabilities([]string{"CAP_NET_RAW", "CAP_MKNOD"}),
		oci.WithCgroup("zelie.slice:zelie:" + s.ID),
		// A private cgroup namespace lets the container read its own limits
		// from /sys/fs/cgroup. Runtimes such as the JVM size themselves from
		// those files and would otherwise assume the whole machine.
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.CgroupNamespace}),
		// containerd puts an empty tmpfs on /run, Docker does not. Images
		// expect what their Dockerfile made there: MariaDB 10.11 stops at
		// once without its /run/mysqld.
		oci.WithoutRunMount,
		oci.WithMounts([]specs.Mount{
			{
				Destination: "/sys/fs/cgroup",
				Type:        "cgroup",
				Source:      "cgroup",
				Options:     []string{"nosuid", "noexec", "nodev", "relatime", "ro"},
			},
			{Destination: "/etc/resolv.conf", Type: "bind", Source: resolv, Options: []string{"rbind", "ro"}},
			{Destination: "/etc/hosts", Type: "bind", Source: hostsFile, Options: []string{"rbind", "ro"}},
		}),
	}
	specOpts = append(specOpts, limitOpts(s)...)
	vols, err := e.volumeMounts(s.Volumes, idmap)
	if err != nil {
		return err
	}
	specOpts = append(specOpts, oci.WithMounts(vols))
	for _, m := range s.Mounts {
		opts := []string{"rbind", "rw", "nosuid", "nodev"}
		if m.ReadOnly {
			opts[1] = "ro"
		}
		specOpts = append(specOpts, oci.WithMounts([]specs.Mount{{Destination: m.Target, Type: "bind", Source: m.Source, Options: opts}}))
	}
	for i, f := range s.Files {
		src := filepath.Join(e.containerDir(s.ID), "files", strconv.Itoa(i))
		if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(src, f.Content, 0o644); err != nil {
			return err
		}
		specOpts = append(specOpts, oci.WithMounts([]specs.Mount{{Destination: f.Target, Type: "bind", Source: src, Options: []string{"rbind", "ro", "nosuid", "nodev"}}}))
	}
	if s.Nesting {
		specOpts = append(specOpts, nestingOpts)
	}
	if len(s.Args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(s.Args...))
	}
	if len(s.Env) > 0 {
		specOpts = append(specOpts, oci.WithEnv(s.Env))
	}
	ioUID, ioGID := base, base
	if s.User != nil {
		specOpts = append(specOpts, runAs(*s.User))
		ioUID, ioGID = base+s.User.UID, base+s.User.GID
	}
	if s.WorkDir != "" {
		specOpts = append(specOpts, oci.WithProcessCwd(s.WorkDir))
	}
	if n := e.openFiles(s.OpenFiles); n > 0 {
		specOpts = append(specOpts, withOpenFiles(n))
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
			IoUid: ioUID,
			IoGid: ioGID,
		}),
		containerd.WithContainerLabels(map[string]string{
			labelUsernsBase: strconv.FormatUint(uint64(base), 10),
			labelApp:        s.App,
			labelVolumes:    volumeNames(s.Volumes),
		}),
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
	creator := cio.LogFile(LogPathFor(e.paths, s.ID))
	if s.Stdin {
		fifo := e.stdinPath(s.ID)
		os.Remove(fifo)
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			return fmt.Errorf("create the input of %s: %w", s.ID, err)
		}
		creator = withStdin(creator, fifo)
	}
	task, err := container.NewTask(ctx, creator)
	if err != nil {
		return fmt.Errorf("create task %s: %w", s.ID, err)
	}
	cleanup = append(cleanup, func() { task.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill) })
	if s.Stdin {
		// Held open from here on: the shim gives the process end of file
		// once nobody has the fifo open for writing.
		if err := e.startHolder(s.ID, true); err != nil {
			return err
		}
		cleanup = append(cleanup, func() { e.stopHolder(s.ID) })
	}

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
		// A container with this name that was cut off mid-removal may still
		// hold an address. No container of this name exists now, so any
		// address it holds is stale.
		e.networks.detach(ctx, s.Network, s.ID, ns.GetPath())
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
	// The firewall lets the container's links through before it starts.
	if err := e.refresh(ctx); err != nil {
		return err
	}

	if err := task.Start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", s.ID, err)
	}
	e.watchLogs()
	return nil
}

// image returns an image ready to run. Local images must have been imported.
// A pinned image that is already here is used as it is, so starting it again
// never depends on the registry; anything else is pulled.
func (e *Engine) image(ctx context.Context, ref string) (containerd.Image, error) {
	if strings.HasPrefix(ref, LocalImages) {
		image, err := e.client.GetImage(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", ref, err)
		}
		return e.unpacked(ctx, image)
	}
	name, err := ImageName(ref)
	if err != nil {
		return nil, err
	}
	if Pinned(name) {
		// The name alone does not prove what is behind it.
		if image, err := e.client.GetImage(ctx, name); err == nil && namesDigest(name, image.Target().Digest) {
			return e.unpacked(ctx, image)
		}
	}
	image, err := e.client.Pull(ctx, name, containerd.WithPullUnpack)
	if err != nil {
		return nil, fmt.Errorf("pull %s: %w", name, err)
	}
	return image, nil
}

func (e *Engine) unpacked(ctx context.Context, image containerd.Image) (containerd.Image, error) {
	if ok, err := image.IsUnpacked(ctx, defaults.DefaultSnapshotter); err != nil {
		return nil, err
	} else if !ok {
		if err := image.Unpack(ctx, defaults.DefaultSnapshotter); err != nil {
			return nil, fmt.Errorf("unpack %s: %w", image.Name(), err)
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
	// Held until the image refers to what was imported, or the garbage
	// collector may take it.
	err := func() error {
		ctx, done, err := e.client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)
		return importArchive(ctx, e.client.ContentStore(), e.client.ImageService(), platforms.Default(), r, name)
	}()
	if err != nil {
		return fmt.Errorf("import %s: %w", name, err)
	}
	_, err = e.image(ctx, name)
	return err
}

// importArchive stores the archive's image under name and names nothing
// else. containerd's own Import also creates an image for every manifest
// that the archive's index gives a name, and moves it if it exists. A build
// can write whatever it likes into the index, and would take over any image
// here, such as the builder's.
//
// Replacing an image of the same name is fine: containers keep their
// snapshot, not the name.
func importArchive(ctx context.Context, cs content.Store, is images.Store, match platforms.Matcher, r io.Reader, name string) error {
	index, err := archive.ImportIndex(ctx, cs, r)
	if err != nil {
		return err
	}
	// Walks the way containerd's Import does, so that what the image refers
	// to is labelled and kept.
	children := func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if desc.Digest != index.Digest {
			return images.Children(ctx, cs, desc)
		}
		raw, err := content.ReadBlob(ctx, cs, desc)
		if err != nil {
			return nil, err
		}
		var idx ocispec.Index
		if err := json.Unmarshal(raw, &idx); err != nil {
			return nil, err
		}
		return idx.Manifests, nil
	}
	walk := images.SetChildrenLabels(cs, images.FilterPlatforms(children, match))
	if err := images.WalkNotEmpty(ctx, walk, index); err != nil {
		return err
	}
	img := images.Image{Name: name, Target: index}
	if _, err := is.Update(ctx, img, "target"); !errdefs.IsNotFound(err) {
		return err
	}
	_, err = is.Create(ctx, img)
	return err
}

// namesDigest reports whether a pinned image name carries digest.
func namesDigest(name string, digest digest.Digest) bool {
	named, err := reference.ParseNormalizedNamed(name)
	if err != nil {
		return false
	}
	c, ok := named.(reference.Canonical)
	return ok && c.Digest() == digest
}

// containerOOMScoreAdj is the OOM score adjustment of container processes.
// Containers would otherwise inherit containerd's -998 through the shim, and
// when the host runs out of memory the kernel would kill Zelie's own
// processes, which sit at 0, before any container.
const containerOOMScoreAdj = 500

// limitOpts holds the container to its limits.
func limitOpts(s Spec) []oci.SpecOpts {
	return []oci.SpecOpts{
		oci.WithMemoryLimit(uint64(s.MemoryBytes)),
		// Memory plus swap equal to the memory limit leaves no room for swap.
		// Without it a container could go far past its limit by filling the
		// host's swap, and the memory it shows would stay at the limit.
		oci.WithMemorySwap(s.MemoryBytes),
		oci.WithCPUCFS(int64(s.CPUs*100000), 100000),
		oci.WithPidsLimit(s.Pids),
		withOOMScoreAdj(containerOOMScoreAdj),
	}
}

func withOOMScoreAdj(score int) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.OOMScoreAdj = &score
		return nil
	}
}

// nestingOpts gives a builder what it needs to run build steps as containers
// of its own. Everything added is confined to the container's user
// namespace; none of it is a privilege on the host. With these BuildKit runs
// each step in its own sandbox, where the step cannot see the builder.
var nestingOpts oci.SpecOpts = func(ctx context.Context, c oci.Client, ctr *containers.Container, s *specs.Spec) error {
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
		labels, err := labelsOf(ctx, c)
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
		labels, err := labelsOf(ctx, c)
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
	if _, err := task.Delete(ctx); err != nil {
		return err
	}
	e.stopHolder(id)
	// A stopped container no longer gets the ports forwarded to it.
	return e.refresh(ctx)
}

// Remove stops the container if needed and deletes it with its snapshot and
// its log. Once started it runs to the end even if ctx is cancelled: a
// network plugin killed halfway leaves the container's address taken.
func (e *Engine) Remove(ctx context.Context, id string) error {
	ctx = context.WithoutCancel(ctx)
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
	e.stopHolder(id)
	// The container is gone; what follows only frees its network and files.
	var errs []error
	if path := labels[labelNetNS]; path != "" {
		if name := labels[labelNetwork]; name != "" {
			errs = append(errs, e.networks.detach(ctx, name, id, path))
		}
		errs = append(errs, netns.LoadNetNS(path).Remove())
	}
	if name := labels[labelNetwork]; name != "" {
		e.createMu.Lock()
		errs = append(errs, e.freeNetwork(ctx, name))
		e.createMu.Unlock()
	}
	errs = append(errs, os.RemoveAll(e.containerDir(id)))
	errs = append(errs, e.refresh(ctx))
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
	App     string
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
		st, err := statusOf(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// labelsOf returns the labels containerd listed with the container. Reading
// them again would fail for a container removed in the meantime, and with it
// every scan of all containers, such as a start that happens to overlap
// someone else's removal.
func labelsOf(ctx context.Context, c containerd.Container) (map[string]string, error) {
	info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
	return info.Labels, err
}

func statusOf(ctx context.Context, c containerd.Container) (Status, error) {
	info, err := c.Info(ctx, containerd.WithoutRefreshedMetadata)
	if err != nil {
		return Status{}, err
	}
	// A container without a task has been stopped: Stop deletes the
	// task once the process has exited.
	st := Status{ID: c.ID(), Image: info.Image, State: "stopped"}
	if v, err := strconv.ParseUint(info.Labels[labelUsernsBase], 10, 32); err == nil {
		st.Userns = uint32(v)
	}
	st.Network = info.Labels[labelNetwork]
	st.App = info.Labels[labelApp]
	st.IP, _ = netip.ParseAddr(info.Labels[labelIP])
	if task, err := c.Task(ctx, nil); err == nil {
		if s, err := task.Status(ctx); err == nil {
			st.State = string(s.Status)
		}
		st.Pid = task.Pid()
	}
	return st, nil
}

func volumeNames(vols []VolumeMount) string {
	names := make([]string, len(vols))
	for i, v := range vols {
		names[i] = v.Name
	}
	return strings.Join(names, ",")
}
