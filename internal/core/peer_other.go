//go:build !linux

package core

import (
	"errors"
	"net"
)

// The core only runs on Linux. Elsewhere no peer is ever trusted.
func peerCredentials(*net.UnixConn) (Peer, error) {
	return Peer{}, errors.New("peer credentials are only supported on Linux")
}
