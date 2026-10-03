package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskUsageStaysInside(t *testing.T) {
	outside, vol := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(outside, "big"), make([]byte, 4<<20), 0o644)
	os.WriteFile(filepath.Join(vol, "small"), make([]byte, 8192), 0o644)
	os.Symlink(outside, filepath.Join(vol, "out"))
	os.Symlink(filepath.Join(outside, "big"), filepath.Join(vol, "bigfile"))
	n, err := diskUsage(vol)
	if err != nil {
		t.Fatal(err)
	}
	if n >= 1<<20 {
		t.Errorf("counted %d bytes, which includes files outside the volume", n)
	}
	if n < 8192 {
		t.Errorf("counted %d bytes, less than the file inside", n)
	}
	if n, err := diskUsage(filepath.Join(vol, "gone")); err != nil || n != 0 {
		t.Errorf("missing dir: %d, %v", n, err)
	}
}
