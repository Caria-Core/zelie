package engine

import (
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// openFilesCeiling is the highest limit on open files a container can be
// given: the hard limit of the process behind containerd's socket, held
// under fs.nr_open. ok is false when either cannot be read.
func openFilesCeiling(socket string) (ceiling uint64, ok bool) {
	pid, err := socketOwner(socket)
	if err != nil {
		return 0, false
	}
	limits, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/limits")
	if err != nil {
		return 0, false
	}
	hard, ok := hardOpenFiles(string(limits))
	if !ok {
		return 0, false
	}
	nrOpen, err := os.ReadFile("/proc/sys/fs/nr_open")
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(nrOpen)), 10, 64)
	if err != nil {
		return 0, false
	}
	return min(hard, n), true
}

// socketOwner is the process that listens on a Unix socket.
func socketOwner(path string) (int, error) {
	c, err := net.DialTimeout("unix", path, 5*time.Second)
	if err != nil {
		return 0, err
	}
	defer c.Close()
	raw, err := c.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}
