package engine

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// freebind lets a socket bind to an address no interface has yet.
func freebind() net.ListenConfig {
	return net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		c.Control(func(fd uintptr) {
			err = unix.SetsockoptInt(int(fd), unix.SOL_IP, unix.IP_FREEBIND, 1)
		})
		return err
	}}
}
