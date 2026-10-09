package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// A volume is a directory on the host that outlives the containers using
// it. Its files are stored with the IDs a container sees (root is 0), and
// each container mounts it through an ID mapping that matches its own user
// namespace. Containers of the same app get a different ID block with each
// deployment, and the files stay theirs all the same.

// VolumeMount puts a volume at Target inside the container.
type VolumeMount struct {
	Name, Target string
}

// Paths a volume may not be mounted on or under: the container would lose
// files the kernel or Zelie put there.
var reservedTargets = []string{"/proc", "/sys", "/dev", "/etc/hosts", "/etc/resolv.conf"}

// CheckVolumeTarget reports whether a volume may be mounted at target.
func CheckVolumeTarget(target string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || target == "/" {
		return fmt.Errorf("volume path %q must be an absolute, clean path other than /", target)
	}
	for _, r := range reservedTargets {
		if target == r || strings.HasPrefix(target, r+"/") {
			return fmt.Errorf("a volume cannot be mounted on %s", target)
		}
	}
	return nil
}

func (e *Engine) volumeDir(name string) string {
	return filepath.Join(e.paths.Volumes, name)
}

// CreateVolume makes an empty volume. Its top directory belongs to root in
// the container, which is who images expect to own a fresh mount.
func (e *Engine) CreateVolume(name string) error {
	if !validID().MatchString(name) {
		return fmt.Errorf("volume name %q must be lowercase letters, digits and dashes", name)
	}
	// Only root may look inside: the files carry container IDs, and a
	// set-user-ID file owned by 0 must not be reachable by host users.
	if err := os.MkdirAll(e.paths.Volumes, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(e.paths.Volumes, 0o700); err != nil {
		return err
	}
	err := os.Mkdir(e.volumeDir(name), 0o755)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("volume %s: %w", name, errdefs.ErrAlreadyExists)
	}
	return err
}

// removedPrefix starts the name of a directory a volume waits in while its
// files are deleted. No volume name starts with a dot.
const removedPrefix = ".removing-"

// RemoveVolume deletes a volume and everything in it. A volume a container
// still has mounted is refused, stopped or not.
func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	if !validID().MatchString(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	waiting, err := e.moveVolumeAside(ctx, name)
	if err != nil {
		return err
	}
	// A big volume takes minutes to delete. Starting and removing containers
	// must not wait for that, so the lock is gone by now.
	return os.RemoveAll(waiting)
}

// moveVolumeAside checks that no container uses the volume and renames it, so
// the name is free at once. The check and the rename happen under createMu: no
// container can be created with the volume in between. It returns the
// directory that now holds the files.
func (e *Engine) moveVolumeAside(ctx context.Context, name string) (string, error) {
	ctx = e.ctx(ctx)
	e.createMu.Lock()
	defer e.createMu.Unlock()
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		labels, err := labelsOf(ctx, c)
		if err != nil {
			return "", err
		}
		for _, v := range strings.Split(labels[labelVolumes], ",") {
			if v == name {
				return "", fmt.Errorf("volume %s is used by container %s: %w", name, c.ID(), errdefs.ErrFailedPrecondition)
			}
		}
	}
	if _, err := os.Lstat(e.volumeDir(name)); errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("volume %s: %w", name, errdefs.ErrNotFound)
	}
	return e.setAside(name)
}

// setAside renames the volume into a directory of its own, next to the
// others, and returns that directory.
func (e *Engine) setAside(name string) (string, error) {
	waiting, err := os.MkdirTemp(e.paths.Volumes, removedPrefix)
	if err != nil {
		return "", err
	}
	if err := os.Rename(e.volumeDir(name), filepath.Join(waiting, name)); err != nil {
		os.Remove(waiting)
		return "", err
	}
	return waiting, nil
}

// removeLeftovers deletes what volumes still waiting for deletion left behind
// when the core stopped. It returns at once; the files go in the background.
func (e *Engine) removeLeftovers() {
	left, _ := filepath.Glob(filepath.Join(e.paths.Volumes, removedPrefix+"*"))
	if len(left) == 0 {
		return
	}
	go func() {
		for _, dir := range left {
			os.RemoveAll(dir)
		}
	}()
}

// VolumeSizes returns how much disk each volume takes, by name. A volume
// that cannot be measured is left out of the sizes and comes back with the
// reason instead, so one tenant cannot keep the others from being measured,
// and the caller can tell a volume it knows nothing about from one with
// nothing in it. The error is for when the volumes cannot be listed at all.
func (e *Engine) VolumeSizes() (sizes map[string]int64, unmeasured map[string]error, err error) {
	entries, err := os.ReadDir(e.paths.Volumes)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int64{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	sizes = make(map[string]int64, len(entries))
	for _, d := range entries {
		if !d.IsDir() || !validID().MatchString(d.Name()) {
			continue
		}
		n, err := diskUsage(filepath.Join(e.paths.Volumes, d.Name()))
		if err != nil {
			if unmeasured == nil {
				unmeasured = map[string]error{}
			}
			unmeasured[d.Name()] = err
			continue
		}
		sizes[d.Name()] = n
	}
	return sizes, unmeasured, nil
}

// diskUsage adds up the blocks a directory tree takes, like du: sparse files
// count what they use, and a file with several links counts once.
//
// A tenant can swap a folder for a link to somewhere else while this runs as
// root, so the tree is walked with WalkTree, which follows no link.
func diskUsage(dir string) (int64, error) {
	root, err := os.OpenRoot(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer root.Close()
	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	var total int64
	err = WalkTree(root, ".", func(n *TreeNode) error {
		st := &n.Stat
		if st.Nlink > 1 && !n.IsDir() {
			key := inode{uint64(st.Dev), uint64(st.Ino)}
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		total += int64(st.Blocks) * 512
		return nil
	})
	return total, err
}

// volumeMounts turns a spec's volumes into bind mounts through idmap.
func (e *Engine) volumeMounts(vols []VolumeMount, idmap []specs.LinuxIDMapping) ([]specs.Mount, error) {
	out := make([]specs.Mount, 0, len(vols))
	for _, v := range vols {
		dir := e.volumeDir(v.Name)
		if st, err := os.Lstat(dir); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("volume %s: %w", v.Name, errdefs.ErrNotFound)
		}
		out = append(out, specs.Mount{
			Destination: v.Target,
			Type:        "bind",
			Source:      dir,
			Options:     []string{"rbind", "rw", "nosuid", "nodev"},
			UIDMappings: idmap,
			GIDMappings: idmap,
		})
	}
	return out, nil
}

// OpenVolume opens a volume for the core to change its files directly, as a
// restore does. It is refused while a container using the volume runs. The
// files came from a container and are not trusted: the root keeps every
// path, symbolic links included, inside the volume.
func (e *Engine) OpenVolume(ctx context.Context, name string) (*os.Root, error) {
	if !validID().MatchString(name) {
		return nil, fmt.Errorf("invalid volume name %q", name)
	}
	ctx = e.ctx(ctx)
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range containers {
		labels, err := labelsOf(ctx, c)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(strings.Split(labels[labelVolumes], ","), name) {
			continue
		}
		task, err := c.Task(ctx, nil)
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		st, err := task.Status(ctx)
		if err != nil {
			return nil, err
		}
		if st.Status != containerd.Stopped {
			return nil, fmt.Errorf("volume %s is in use by running container %s: %w", name, c.ID(), errdefs.ErrFailedPrecondition)
		}
	}
	root, err := os.OpenRoot(e.volumeDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("volume %s: %w", name, errdefs.ErrNotFound)
	}
	return root, err
}

// ReadVolume opens a volume while its containers may still run, for a backup
// that does not stop the app and for the file manager. Unlike OpenVolume it
// does not look at the containers, so whoever uses it must be fine with files
// that change underneath.
func (e *Engine) ReadVolume(name string) (*os.Root, error) {
	if !validID().MatchString(name) {
		return nil, fmt.Errorf("invalid volume name %q", name)
	}
	root, err := os.OpenRoot(e.volumeDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("volume %s: %w", name, errdefs.ErrNotFound)
	}
	return root, err
}

// VolumeSize returns how much disk one volume takes.
func (e *Engine) VolumeSize(name string) (int64, error) {
	if !validID().MatchString(name) {
		return 0, fmt.Errorf("invalid volume name %q", name)
	}
	dir := e.volumeDir(name)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("volume %s: %w", name, errdefs.ErrNotFound)
	}
	return diskUsage(dir)
}
