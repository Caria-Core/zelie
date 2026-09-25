package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"time"
)

// Installer puts Zelie's containerd and runc on the machine and keeps the
// systemd service running. Running it again is safe: files that are already
// correct are left alone, and containerd is only restarted when something
// changed.
type Installer struct {
	Paths  Paths
	Arch   string
	Client *http.Client
	Log    io.Writer

	// systemctl runs systemctl. Tests replace it.
	systemctl func(ctx context.Context, args ...string) error
}

func (in *Installer) Install(ctx context.Context) error {
	cd, ok := containerdArtifacts[in.Arch]
	if !ok {
		return fmt.Errorf("no containerd build for architecture %q", in.Arch)
	}
	rc, ok := runcArtifacts[in.Arch]
	if !ok {
		return fmt.Errorf("no runc build for architecture %q", in.Arch)
	}
	cn, ok := cniArtifacts[in.Arch]
	if !ok {
		return fmt.Errorf("no CNI plugin build for architecture %q", in.Arch)
	}

	changed := false

	archive, err := in.download(ctx, cd)
	if err != nil {
		return fmt.Errorf("containerd: %w", err)
	}
	bins, err := extract(archive, "bin", containerdBinaries)
	if err != nil {
		return fmt.Errorf("containerd: %w", err)
	}
	for _, name := range containerdBinaries {
		c, err := writeIfChanged(filepath.Join(in.Paths.Bin, name), bins[name], 0o755)
		if err != nil {
			return err
		}
		changed = changed || c
	}

	runc, err := in.download(ctx, rc)
	if err != nil {
		return fmt.Errorf("runc: %w", err)
	}
	c, err := writeIfChanged(in.Paths.Runc(), runc, 0o755)
	if err != nil {
		return err
	}
	changed = changed || c

	// Plugins run once per network change, not as a daemon, so replacing
	// them never needs a restart.
	archive, err = in.download(ctx, cn)
	if err != nil {
		return fmt.Errorf("CNI plugins: %w", err)
	}
	plugins, err := extract(archive, ".", cniPlugins)
	if err != nil {
		return fmt.Errorf("CNI plugins: %w", err)
	}
	for _, name := range cniPlugins {
		if _, err := writeIfChanged(filepath.Join(in.Paths.CNI, name), plugins[name], 0o755); err != nil {
			return err
		}
	}

	for file, content := range map[string]string{
		in.Paths.Config: ConfigFile(in.Paths),
		in.Paths.Unit:   UnitFile(in.Paths),
	} {
		c, err := writeIfChanged(file, []byte(content), 0o644)
		if err != nil {
			return err
		}
		changed = changed || c
	}

	in.logf("containerd %s, runc %s and CNI plugins %s are installed", ContainerdVersion, RuncVersion, CNIVersion)

	systemctl := in.systemctl
	if systemctl == nil {
		systemctl = runSystemctl
	}
	unit := filepath.Base(in.Paths.Unit)
	if err := systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if err := systemctl(ctx, "enable", "--now", unit); err != nil {
		return err
	}
	if changed {
		// Safe for running containers, see UnitFile.
		if err := systemctl(ctx, "restart", unit); err != nil {
			return err
		}
		in.logf("restarted %s", unit)
	}
	return nil
}

// download fetches an artifact and refuses it unless it matches the pinned
// checksum.
func (in *Installer) download(ctx context.Context, a artifact) ([]byte, error) {
	client := in.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", a.URL, resp.Status)
	}
	// The largest artifact is about 40 MB. The limit stops a misbehaving
	// server from filling memory.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 200<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", a.URL, err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != a.SHA256 {
		return nil, fmt.Errorf("checksum mismatch for %s: got %s, want %s", a.URL, got, a.SHA256)
	}
	return data, nil
}

// extract returns the named files from one directory of a .tar.gz.
func extract(archive []byte, dir string, names []string) (map[string][]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	out := make(map[string][]byte)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg || path.Dir(path.Clean(h.Name)) != dir {
			continue
		}
		name := path.Base(h.Name)
		if !slices.Contains(names, name) {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, 200<<20))
		if err != nil {
			return nil, err
		}
		out[name] = data
	}
	for _, n := range names {
		if _, ok := out[n]; !ok {
			return nil, fmt.Errorf("archive has no %s", path.Join(dir, n))
		}
	}
	return out, nil
}

// writeIfChanged replaces file with data unless it already holds exactly that.
// The new content is written next to the target and renamed over it, so a
// crash never leaves a half-written binary behind.
func writeIfChanged(file string, data []byte, mode os.FileMode) (bool, error) {
	if old, err := os.ReadFile(file); err == nil && bytes.Equal(old, data) {
		if fi, err := os.Stat(file); err == nil && fi.Mode().Perm() == mode {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+".*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return false, err
	}
	return true, nil
}

func runSystemctl(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %v: %w: %s", args, err, bytes.TrimSpace(out))
	}
	return nil
}

func (in *Installer) logf(format string, args ...any) {
	if in.Log != nil {
		fmt.Fprintf(in.Log, format+"\n", args...)
	}
}
