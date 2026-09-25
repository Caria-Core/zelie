package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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
	if !validID.MatchString(name) {
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

// RemoveVolume deletes a volume and everything in it. A volume a container
// still has mounted is refused, stopped or not.
func (e *Engine) RemoveVolume(ctx context.Context, name string) error {
	if !validID.MatchString(name) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	ctx = e.ctx(ctx)
	e.createMu.Lock()
	defer e.createMu.Unlock()
	containers, err := e.client.Containers(ctx)
	if err != nil {
		return err
	}
	for _, c := range containers {
		labels, err := c.Labels(ctx)
		if err != nil {
			return err
		}
		for _, v := range strings.Split(labels[labelVolumes], ",") {
			if v == name {
				return fmt.Errorf("volume %s is used by container %s: %w", name, c.ID(), errdefs.ErrFailedPrecondition)
			}
		}
	}
	dir := e.volumeDir(name)
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("volume %s: %w", name, errdefs.ErrNotFound)
	}
	return os.RemoveAll(dir)
}

// VolumeSizes returns how much disk each volume takes, by name.
func (e *Engine) VolumeSizes() (map[string]int64, error) {
	entries, err := os.ReadDir(e.paths.Volumes)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int64{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(entries))
	for _, d := range entries {
		if !d.IsDir() || !validID.MatchString(d.Name()) {
			continue
		}
		n, err := diskUsage(filepath.Join(e.paths.Volumes, d.Name()))
		if err != nil {
			return nil, err
		}
		out[d.Name()] = n
	}
	return out, nil
}

// diskUsage adds up the blocks a directory tree takes, like du: sparse files
// count what they use, and a file with several links counts once.
func diskUsage(root string) (int64, error) {
	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A file removed while walking is not an error.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += info.Size()
			return nil
		}
		if st.Nlink > 1 && !d.IsDir() {
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
