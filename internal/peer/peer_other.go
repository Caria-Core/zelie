//go:build !linux

package peer

import (
	"errors"
	"net"
)

// The core only runs on Linux. Elsewhere no peer is ever trusted.
func credentials(*net.UnixConn) (Peer, error) {
	return Peer{}, errors.New("peer credentials are only supported on Linux")
}
