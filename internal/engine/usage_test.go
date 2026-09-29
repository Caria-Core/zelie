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

	// The network, through a process of the container.
	proc := t.TempDir()
	old := procRoot
	procRoot = proc
	t.Cleanup(func() { procRoot = old })
	os.MkdirAll(filepath.Join(proc, "4242", "net"), 0o755)
	os.WriteFile(filepath.Join(proc, "4242", "net", "dev"), []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:     120       2    0    0    0     0          0         0      120       2    0    0    0     0       0          0
  eth0: 5367218    4012    0    0    0     0          0         0   913562    3120    0    0    0     0       0          0
`), 0o644)
	os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte("4242\n4250\n"), 0o644)
	u, err = readUsage(dir)
	if err != nil || u.RxBytes != 5367218 || u.TxBytes != 913562 {
		t.Fatalf("network %+v, %v", u, err)
	}
}
