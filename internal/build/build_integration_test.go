//go:build integration

// These tests need root, a Linux machine and Zelie's containerd running
// (zelie engine install). Run them with hack/vm-test.sh.

package build

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/engine"
)

func connect(t *testing.T) *engine.Engine {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	e, err := engine.Connect(context.Background(), engine.DefaultPaths)
	if err != nil {
		t.Skip("Zelie's containerd is not running")
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// archive makes a source archive the way GitHub does, with extra entries
// appended as given.
func archive(t *testing.T, files map[string]string, extra ...*tar.Header) *bytes.Buffer {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: "repo-abc/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for _, h := range extra {
		tw.WriteHeader(h)
	}
	tw.Close()
	gz.Close()
	return &buf
}

func newBuilder(t *testing.T, e *engine.Engine) *Builder {
	dir, err := os.MkdirTemp("/var/lib/zelie", "it-builds-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return New(e, engine.DefaultPaths, dir)
}

func TestBuildDockerfile(t *testing.T) {
	b := newBuilder(t, connect(t))
	var out bytes.Buffer
	src := archive(t, map[string]string{"Dockerfile": "FROM docker.io/library/busybox:latest\nRUN echo ok > /ok\n"})
	res, err := b.Build(context.Background(), Request{App: "it-app", Version: "1", Source: src}, &out)
	if err != nil || res.Image != "zelie.local/it-app:1" || res.TestCommand != "" {
		t.Fatalf("Build = %+v, %v\n%s", res, err, out.String())
	}
}

func TestFailingBuildSaysWhy(t *testing.T) {
	b := newBuilder(t, connect(t))
	var out bytes.Buffer
	src := archive(t, map[string]string{"Dockerfile": "FROM docker.io/library/busybox:latest\nRUN echo broken-on-purpose && false\n"})
	_, err := b.Build(context.Background(), Request{App: "it-app", Version: "2", Source: src}, &out)
	var se *StepError
	if !errors.As(err, &se) || se.Step != "build" || !strings.Contains(out.String(), "broken-on-purpose") {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
}

// An archive that points a symlink at the build cache and then writes
// through it must not reach the cache: the unpack step has none mounted.
func TestUnpackCannotReachTheCache(t *testing.T) {
	b := newBuilder(t, connect(t))
	src := archive(t, map[string]string{"Dockerfile": "FROM docker.io/library/busybox:latest\n"},
		&tar.Header{Name: "repo-abc/cache", Typeflag: tar.TypeSymlink, Linkname: "/cache"},
		&tar.Header{Name: "repo-abc/cache/planted", Typeflag: tar.TypeReg, Mode: 0o644},
	)
	b.Build(context.Background(), Request{App: "it-app", Version: "3", Source: src}, &bytes.Buffer{})
	if _, err := os.Lstat(b.cacheDir("buildkit") + "/planted"); err == nil {
		t.Fatal("the archive wrote into the build cache")
	}
}
