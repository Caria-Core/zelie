//go:build integration

package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/containerd/errdefs"
)

func testVolume(t *testing.T, e *Engine, name string) string {
	t.Helper()
	os.RemoveAll(e.volumeDir(name))
	if err := e.CreateVolume(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(e.volumeDir(name)) })
	return e.volumeDir(name)
}

// Each deployment runs in a new ID block, and the next one must still own
// what the last one wrote.
func TestVolumeKeepsOwnersAcrossContainers(t *testing.T) {
	e := connect(t)
	dir := testVolume(t, e, "it-vol")
	vols := []VolumeMount{{Name: "it-vol", Target: "/data"}}

	run(t, e, Spec{ID: "it-vol-1", Image: testImage, Volumes: vols,
		Args:        []string{"sh", "-c", "echo one > /data/root.txt && mkdir /data/app && chown 1000:1000 /data/app && echo written"},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	waitForLog(t, "it-vol-1", "written")
	first := status(t, e, "it-vol-1").Userns

	for path, want := range map[string]uint32{"root.txt": 0, "app": 1000} {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(dir, path), &st); err != nil {
			t.Fatal(err)
		}
		if st.Uid != want || st.Gid != want {
			t.Errorf("%s is owned by %d:%d on disk, want %d", path, st.Uid, st.Gid, want)
		}
	}

	run(t, e, Spec{ID: "it-vol-2", Image: testImage, Volumes: vols,
		Args:        []string{"sh", "-c", "cat /data/root.txt; echo two >> /data/root.txt; stat -c '%u' /data/app; echo done; sleep 60"},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	out := waitForLog(t, "it-vol-2", "done")
	if status(t, e, "it-vol-2").Userns == first {
		t.Fatal("the second container got the same ID block, so the test proves nothing")
	}
	for _, want := range []string{"one", "1000"} {
		if !containsLine(out, want) {
			t.Errorf("second container's output lacks %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "root.txt")); string(b) != "one\ntwo\n" {
		t.Errorf("root.txt is %q", b)
	}

	// A volume in use cannot be removed.
	if err := e.RemoveVolume(context.Background(), "it-vol"); !errdefs.IsFailedPrecondition(err) {
		t.Errorf("removing a volume in use: %v", err)
	}

	sizes, err := e.VolumeSizes()
	if err != nil {
		t.Fatal(err)
	}
	if sizes["it-vol"] <= 0 {
		t.Errorf("volume size is %d", sizes["it-vol"])
	}
}

func TestVolumeMustExist(t *testing.T) {
	e := connect(t)
	os.RemoveAll(e.volumeDir("it-novol"))
	err := e.Run(context.Background(), Spec{ID: "it-novol", Image: testImage, Args: []string{"true"},
		Volumes:     []VolumeMount{{Name: "it-novol", Target: "/data"}},
		MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 8})
	if !errdefs.IsNotFound(err) {
		e.Remove(context.Background(), "it-novol")
		t.Fatalf("running with a missing volume: %v", err)
	}
}

func containsLine(out, line string) bool {
	return slices.Contains(strings.Split(out, "\n"), line)
}
