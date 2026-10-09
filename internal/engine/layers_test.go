package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
)

func TestUpperDirIsFoundInTheSnapshotsMounts(t *testing.T) {
	const root = "/var/lib/zelie/containerd"
	snap := root + "/io.containerd.snapshotter.v1.overlayfs/snapshots/7"
	overlay := func(opts ...string) []mount.Mount {
		return []mount.Mount{{Type: "overlay", Source: "overlay", Options: opts}}
	}
	tests := []struct {
		name    string
		mounts  []mount.Mount
		want    string
		wantErr bool
	}{
		{"overlay", overlay("workdir="+snap+"/work", "upperdir="+snap+"/fs", "lowerdir=/a:/b"), snap + "/fs", false},
		{"a snapshot with no layer below it", []mount.Mount{{Type: "bind", Source: snap + "/fs", Options: []string{"rw", "rbind"}}}, snap + "/fs", false},
		{"overlay without an upper directory", overlay("lowerdir=/a:/b"), "", true},
		{"no mounts", nil, "", true},
		{"two mounts", append(overlay("upperdir="+snap+"/fs"), overlay("upperdir="+snap+"/fs")...), "", true},
		{"another kind of mount", []mount.Mount{{Type: "erofs", Source: snap}}, "", true},
		{"outside the root", overlay("upperdir=/etc"), "", true},
		{"climbing out of the root", overlay("upperdir=" + root + "/../../etc"), "", true},
		{"the root itself", overlay("upperdir=" + root), "", true},
		{"a sibling with the root as prefix", overlay("upperdir=" + root + "-other/fs"), "", true},
		{"not absolute", overlay("upperdir=snapshots/7/fs"), "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := upperDir(root, tc.mounts)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Errorf("upperDir = %q, %v; want %q, error %v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// layerFiles makes a directory for a container's own files with a file of
// the given size in it.
func layerFiles(t *testing.T, size int) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cache"), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMeasureLayers(t *testing.T) {
	small, big := layerFiles(t, 8<<10), layerFiles(t, 3<<20)
	// A link out of the layer is not followed: a container can point it at
	// anything the core can read.
	outside := layerFiles(t, 16<<20)
	if err := os.Symlink(outside, filepath.Join(small, "out")); err != nil {
		t.Fatal(err)
	}
	// Files that cannot be walked: the directory is a file.
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs := map[string]string{"web-1": small, "web-2": big, "blog-1": notADir}
	find := func(id string) (string, string, error) {
		if id == "gone-1" {
			return "", "", fmt.Errorf("container %s: %w", id, errdefs.ErrNotFound)
		}
		return id[:len(id)-2], dirs[id], nil
	}

	ids := []string{"web-1", "gone-1", "blog-1", "web-2"}
	got, err := measureLayers(context.Background(), slog.New(slog.DiscardHandler), ids, find)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %+v, want web-1, blog-1 and web-2 (gone-1 was removed meanwhile)", got)
	}
	web1, blog, web2 := got[0], got[1], got[2]
	if web1.Container != "web-1" || web1.App != "web" || web1.Unmeasured || web1.Bytes < 8<<10 || web1.Bytes >= 1<<20 {
		t.Errorf("web-1: %+v", web1)
	}
	if blog.Container != "blog-1" || !blog.Unmeasured || blog.Bytes != 0 {
		t.Errorf("a layer that cannot be measured must say so: %+v", blog)
	}
	// It did not stop the container after it from being measured.
	if web2.Container != "web-2" || web2.Unmeasured || web2.Bytes < 3<<20 {
		t.Errorf("web-2: %+v", web2)
	}
}

// Not finding where the files are says something about containerd, not about
// the container. Reporting it as the container's files being unmeasurable
// would stop every app whenever containerd is slow to answer.
func TestMeasureLayersFailsWhenTheFilesCannotBeFound(t *testing.T) {
	find := func(id string) (string, string, error) { return "", "", errors.New("containerd is not answering") }
	got, err := measureLayers(context.Background(), slog.New(slog.DiscardHandler), []string{"web-1"}, find)
	if err == nil || len(got) != 0 {
		t.Errorf("got %+v, %v; want an error and no sizes", got, err)
	}
}

func TestMeasureLayersStopsWhenAskedTo(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	find := func(id string) (string, string, error) { return "web", t.TempDir(), nil }
	if _, err := measureLayers(ctx, slog.New(slog.DiscardHandler), []string{"web-1"}, find); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v", err)
	}
}

// With nothing running there is nothing to measure, and containerd is not
// asked: the engine here has no connection to it at all.
func TestLayerSizesAsksNobodyWhenNothingRuns(t *testing.T) {
	old := cgroupRoot
	cgroupRoot = t.TempDir()
	t.Cleanup(func() { cgroupRoot = old })
	e := &Engine{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	got, err := e.LayerSizes(context.Background())
	if err != nil || len(got) != 0 {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A build step or SteamCMD belongs to no app, so no limit applies to it and
// walking its files would be work for nothing. Pointing it at a directory that
// cannot be walked shows whether it was.
func TestMeasureLayersLeavesOutContainersOfNoApp(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	web := layerFiles(t, 8<<10)
	find := func(id string) (string, string, error) {
		if id == "zelie-build-plan" {
			return "", notADir, nil
		}
		return "web", web, nil
	}
	got, err := measureLayers(context.Background(), slog.New(slog.DiscardHandler), []string{"zelie-build-plan", "web-1"}, find)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Container != "web-1" {
		t.Errorf("got %+v, want only web-1", got)
	}
}

// quitsAfter is a context that ends after it has been asked n times whether it
// has, so a test ends it in the middle of a walk without waiting for a clock.
type quitsAfter struct {
	context.Context
	n int
}

func (c *quitsAfter) Err() error {
	if c.n--; c.n < 0 {
		return context.Canceled
	}
	return nil
}

// A request the panel gave up on must not leave the core walking a huge tree,
// and a walk cut short is no sign that the container's files cannot be
// measured: the call fails, and no app is stopped for it.
func TestMeasureLayersStopsInTheMiddleOfAWalk(t *testing.T) {
	dir := t.TempDir()
	for i := range 50 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	find := func(id string) (string, string, error) { return "web", dir, nil }
	ctx := &quitsAfter{Context: context.Background(), n: 10}
	got, err := measureLayers(ctx, slog.New(slog.DiscardHandler), []string{"web-1"}, find)
	if !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Errorf("got %+v, %v; want the call to fail with the context's error", got, err)
	}
	walk := &quitsAfter{Context: context.Background(), n: 10}
	if _, err := diskUsageCtx(walk, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("diskUsageCtx err = %v", err)
	}
	if walk.n != -1 {
		t.Errorf("the walk asked the context %d times after it had ended", -1-walk.n)
	}
}
