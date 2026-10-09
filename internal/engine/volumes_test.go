package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRemovedVolumeFreesItsNameAtOnce(t *testing.T) {
	e := &Engine{paths: Paths{Volumes: filepath.Join(t.TempDir(), "volumes")}}
	if err := e.CreateVolume("data"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.volumeDir("data"), "world.dat"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	waiting, err := e.setAside("data")
	if err != nil {
		t.Fatal(err)
	}
	// The files wait to be deleted, out of sight, and the name is free: a new
	// volume of that name can be made while the old one is still going.
	if b, err := os.ReadFile(filepath.Join(waiting, "data", "world.dat")); err != nil || string(b) != "old" {
		t.Errorf("files at %s: %q, %v", waiting, b, err)
	}
	if err := e.CreateVolume("data"); err != nil {
		t.Errorf("the name was not free: %v", err)
	}
	sizes, unmeasured, err := e.VolumeSizes()
	if err != nil || len(sizes) != 1 || len(unmeasured) != 0 {
		t.Errorf("volumes while the old one waits: %v, %v, %v", sizes, unmeasured, err)
	}
	if err := os.RemoveAll(waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.volumeDir("data")); err != nil {
		t.Errorf("the new volume went with the old: %v", err)
	}
}

func TestLeftoverVolumesAreDeletedAtStart(t *testing.T) {
	dir := t.TempDir()
	e := &Engine{paths: Paths{Volumes: dir}}
	os.MkdirAll(filepath.Join(dir, removedPrefix+"123", "data"), 0o755)
	os.WriteFile(filepath.Join(dir, removedPrefix+"123", "data", "world.dat"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, removedPrefix+"456", "db"), 0o755)
	os.MkdirAll(filepath.Join(dir, "live"), 0o755)

	e.removeLeftovers()
	deadline := time.Now().Add(2 * time.Second)
	for {
		left, _ := filepath.Glob(filepath.Join(dir, removedPrefix+"*"))
		if len(left) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still there: %v", left)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "live")); err != nil {
		t.Errorf("a volume in use was deleted: %v", err)
	}
}
