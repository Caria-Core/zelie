package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fakeRelease serves a containerd archive and a runc binary and registers them
// under the architecture name "test".
func fakeRelease(t *testing.T, archive, runc []byte, runcSum string) *httptest.Server {
	cni := tarGz(t, map[string]string{
		"./bridge": "b", "./host-local": "h", "./loopback": "l", "./firewall": "f", "./portmap": "p", "./vlan": "not wanted",
	})
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/containerd.tar.gz":
			w.Write(archive)
		case "/runc":
			w.Write(runc)
		case "/cni.tgz":
			w.Write(cni)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	containerdArtifacts["test"] = artifact{URL: srv.URL + "/containerd.tar.gz", SHA256: sum(archive)}
	runcArtifacts["test"] = artifact{URL: srv.URL + "/runc", SHA256: runcSum}
	cniArtifacts["test"] = artifact{URL: srv.URL + "/cni.tgz", SHA256: sum(cni)}
	t.Cleanup(func() {
		delete(containerdArtifacts, "test")
		delete(runcArtifacts, "test")
		delete(cniArtifacts, "test")
	})
	return srv
}

func testPaths(dir string) Paths {
	return Paths{
		Bin:    filepath.Join(dir, "bin"),
		CNI:    filepath.Join(dir, "cni"),
		Config: filepath.Join(dir, "etc/containerd.toml"),
		Root:   filepath.Join(dir, "lib"),
		State:  filepath.Join(dir, "run"),
		Socket: filepath.Join(dir, "run/containerd.sock"),
		Unit:   filepath.Join(dir, "systemd/zelie-containerd.service"),
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"bin/containerd":              "daemon",
		"bin/containerd-shim-runc-v2": "shim",
		"bin/ctr":                     "ctr",
		"bin/containerd-stress":       "not wanted",
	})
	runc := []byte("runc")
	fakeRelease(t, archive, runc, sum(runc))

	dir := t.TempDir()
	var calls []string
	in := &Installer{
		Paths: testPaths(dir),
		Arch:  "test",
		systemctl: func(_ context.Context, args ...string) error {
			calls = append(calls, strings.Join(args, " "))
			return nil
		},
	}

	if err := in.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(calls, "restart zelie-containerd.service") {
		t.Errorf("first install should restart containerd, calls: %v", calls)
	}
	for _, name := range []string{"containerd", "containerd-shim-runc-v2", "ctr", "runc"} {
		fi, err := os.Stat(filepath.Join(dir, "bin", name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("%s has mode %v", name, fi.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "containerd-stress")); !os.IsNotExist(err) {
		t.Errorf("containerd-stress should not be installed")
	}
	if _, err := os.Stat(filepath.Join(dir, "cni", "bridge")); err != nil {
		t.Errorf("bridge plugin missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cni", "vlan")); !os.IsNotExist(err) {
		t.Errorf("vlan plugin should not be installed")
	}

	calls = nil
	if err := in.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(calls, "restart zelie-containerd.service") {
		t.Errorf("second install changed nothing and must not restart, calls: %v", calls)
	}
}

func TestInstallRejectsBadChecksum(t *testing.T) {
	archive := tarGz(t, map[string]string{
		"bin/containerd":              "daemon",
		"bin/containerd-shim-runc-v2": "shim",
		"bin/ctr":                     "ctr",
	})
	fakeRelease(t, archive, []byte("tampered runc"), sum([]byte("runc")))

	dir := t.TempDir()
	in := &Installer{
		Paths:     testPaths(dir),
		Arch:      "test",
		systemctl: func(context.Context, ...string) error { return nil },
	}
	err := in.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("want checksum error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "bin", "runc")); !os.IsNotExist(err) {
		t.Errorf("runc must not be written after a checksum mismatch")
	}
}

func TestExtractMissingBinary(t *testing.T) {
	archive := tarGz(t, map[string]string{"bin/containerd": "daemon"})
	if _, err := extract(archive, "bin", []string{"containerd", "ctr"}); err == nil {
		t.Fatal("want an error for a missing binary")
	}
}

func TestUnitKeepsContainersOnRestart(t *testing.T) {
	unit := UnitFile(DefaultPaths)
	for _, want := range []string{"KillMode=process", "Delegate=yes", "--config /etc/zelie/containerd.toml"} {
		if !strings.Contains(unit, want) {
			t.Errorf("unit file lacks %q", want)
		}
	}
}

func TestArtifactsCoverSupportedArchitectures(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		for name, set := range map[string]map[string]artifact{"containerd": containerdArtifacts, "runc": runcArtifacts, "cni": cniArtifacts} {
			a, ok := set[arch]
			if !ok {
				t.Errorf("no %s artifact for %s", name, arch)
				continue
			}
			if len(a.SHA256) != 64 || !strings.HasPrefix(a.URL, "https://") {
				t.Errorf("%s/%s artifact looks wrong: %+v", name, arch, a)
			}
		}
	}
}
