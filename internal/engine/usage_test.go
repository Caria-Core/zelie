package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadUsage(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "memory.current"), []byte("88080384\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "cpu.stat"), []byte("usage_usec 1520000\nuser_usec 1000000\nsystem_usec 520000\n"), 0o644)
	u, err := readUsage(dir)
	if err != nil || u.MemoryBytes != 88080384 || u.CPUUsec != 1520000 {
		t.Fatalf("usage %+v, %v", u, err)
	}
	if _, err := readUsage(t.TempDir()); err == nil {
		t.Error("read the usage of a missing cgroup")
	}
}
