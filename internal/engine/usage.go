package engine

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// cgroupRoot is where the systemd cgroup driver puts containers: in
// zelie.slice, one scope each (see WithCgroup in Run).
var cgroupRoot = "/sys/fs/cgroup/zelie.slice"

// Usage is what a running container uses.
type Usage struct {
	MemoryBytes int64
	// CPUUsec is the CPU time used so far; two readings give the rate.
	CPUUsec int64
}

// Usage reads a container's memory and CPU time from its cgroup.
func (e *Engine) Usage(id string) (Usage, error) {
	if !ValidID(id) {
		return Usage{}, fmt.Errorf("invalid container id %q", id)
	}
	return readUsage(filepath.Join(cgroupRoot, "zelie-"+id+".scope"))
}

func readUsage(dir string) (Usage, error) {
	var u Usage
	b, err := os.ReadFile(filepath.Join(dir, "memory.current"))
	if err != nil {
		return u, err
	}
	if u.MemoryBytes, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err != nil {
		return u, err
	}
	b, err = os.ReadFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return u, err
	}
	u.CPUUsec, err = field(bytes.NewReader(b), "usage_usec")
	return u, err
}

// Host is what the server has to give.
type Host struct {
	CPUs          int
	MemoryBytes   int64
	DiskBytes     int64 // of the file system that holds Zelie's data
	DiskFreeBytes int64
}

// HostInfo describes the server. dir is a directory on the disk that holds
// the containers.
func HostInfo(dir string) (Host, error) {
	h := Host{CPUs: runtime.NumCPU()}
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return h, err
	}
	defer f.Close()
	kb, err := field(f, "MemTotal:")
	if err != nil {
		return h, err
	}
	h.MemoryBytes = kb << 10
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return h, err
	}
	h.DiskBytes = int64(st.Blocks) * int64(st.Bsize)
	h.DiskFreeBytes = int64(st.Bavail) * int64(st.Bsize)
	return h, nil
}

// field finds a line starting with name in r and returns the number after
// it.
func field(r io.Reader, name string) (int64, error) {
	s := bufio.NewScanner(r)
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) >= 2 && f[0] == name {
			return strconv.ParseInt(f[1], 10, 64)
		}
	}
	return 0, fmt.Errorf("%s not found", name)
}
