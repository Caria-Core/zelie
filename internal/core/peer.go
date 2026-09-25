package core

import (
	"context"
	"net"
)

// Peer is the process on the other end of the socket, as reported by the
// kernel.
type Peer struct {
	UID uint32
	PID int32
}

// PeerPolicy lists the users allowed to talk to the core. Root is always
// allowed.
type PeerPolicy struct {
	UIDs []uint32
}

func (p PeerPolicy) Allows(peer Peer) bool {
	if peer.UID == 0 {
		return true
	}
	for _, u := range p.UIDs {
		if u == peer.UID {
			return true
		}
	}
	return false
}

type peerKey struct{}

// withPeer records the peer's credentials when a connection is accepted. If
// they cannot be read, no peer is stored and every request on the connection
// is refused.
func withPeer(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	p, err := peerCredentials(uc)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, p)
}

func peerFrom(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(peerKey{}).(Peer)
	return p, ok
}
