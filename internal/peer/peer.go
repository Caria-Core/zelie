// Package peer checks who is on the other end of a local Unix socket. The
// kernel reports the peer's user, so a process cannot claim to be someone
// else the way it could with a token or a header.
package peer

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
)

// Peer is the process on the other end of the socket.
type Peer struct {
	UID uint32
	PID int32
}

// Policy lists the users allowed in. Root is always allowed.
type Policy struct {
	UIDs []uint32
	// Routes holds users that may use some routes only, by the pattern the
	// route was registered with, such as "GET /v1/host". A user in UIDs may
	// use every route, whatever is listed here.
	Routes map[uint32][]string
}

func (p Policy) Allows(peer Peer) bool {
	return peer.UID == 0 || slices.Contains(p.UIDs, peer.UID)
}

type key struct{}

// ConnContext is meant for http.Server.ConnContext. It records the peer when a
// connection is accepted. If the credentials cannot be read no peer is stored,
// and every request on that connection is refused.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	p, err := credentials(uc)
	if err != nil {
		return ctx
	}
	return context.WithValue(ctx, key{}, p)
}

// WithPeer returns a context carrying p. Tests use it to stand in for a real
// connection.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, key{}, p)
}

func From(ctx context.Context) (Peer, bool) {
	p, ok := ctx.Value(key{}).(Peer)
	return p, ok
}

// Require refuses requests from anyone the policy does not allow.
func Require(policy Policy, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := From(r.Context())
		if !ok || !policy.Allows(p) {
			log.Warn("rejected request", "uid", p.UID, "pid", p.PID, "path", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"not allowed"}` + "\n"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireRoutes is Require for a mux, and lets the users of Policy.Routes
// through to their own routes.
func RequireRoutes(policy Policy, log *slog.Logger, mux *http.ServeMux) http.Handler {
	full := Require(policy, log, mux)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := From(r.Context()); ok && !policy.Allows(p) {
			if routes, limited := policy.Routes[p.UID]; limited {
				if _, pattern := mux.Handler(r); pattern != "" && slices.Contains(routes, pattern) {
					mux.ServeHTTP(w, r)
					return
				}
			}
		}
		full.ServeHTTP(w, r)
	})
}
