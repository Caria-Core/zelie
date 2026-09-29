package panel

import (
	"context"
	"crypto/sha256"
	"log/slog"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

// failures counts failed attempts per key, such as an IP address or an
// account, and blocks the key once it has too many in the window. The
// counts are in the database, so restarting the panel does not reset them.
// Each is a quick local query, not tied to the request that caused it.
type failures struct {
	store  *store.Store
	log    *slog.Logger
	kind   string
	max    int
	window time.Duration
}

func newFailures(st *store.Store, log *slog.Logger, kind string, max int, window time.Duration) *failures {
	return &failures{store: st, log: log, kind: kind, max: max, window: window}
}

// hash keeps addresses and typed emails out of the database.
func (f *failures) hash(key string) []byte {
	h := sha256.Sum256([]byte(f.kind + "\x00" + key))
	return h[:]
}

// Count returns the failed attempts of key in the window.
func (f *failures) Count(key string, now time.Time) int {
	n, _, err := f.store.Failures(context.Background(), f.kind, f.hash(key), now.Add(-f.window))
	if err != nil {
		f.log.Error("login limits: read", "kind", f.kind, "err", err)
		// When the count cannot be read, the limit holds.
		return f.max
	}
	return n
}

// Wait returns how long key must wait before its next attempt; zero when
// it may try now.
func (f *failures) Wait(key string, now time.Time) time.Duration {
	n, oldest, err := f.store.Failures(context.Background(), f.kind, f.hash(key), now.Add(-f.window))
	if err != nil {
		f.log.Error("login limits: read", "kind", f.kind, "err", err)
		return f.window
	}
	if n < f.max {
		return 0
	}
	// The oldest attempt leaving the window frees one.
	return max(time.Second, oldest.Add(f.window).Sub(now))
}

// Add records a failed attempt.
func (f *failures) Add(key string, now time.Time) {
	if err := f.store.AddFailure(context.Background(), f.kind, f.hash(key), now, now.Add(-f.window)); err != nil {
		f.log.Error("login limits: save", "kind", f.kind, "err", err)
	}
}

// Reset forgets key, after a successful login.
func (f *failures) Reset(key string) {
	if err := f.store.ResetFailures(context.Background(), f.kind, f.hash(key)); err != nil {
		f.log.Error("login limits: reset", "kind", f.kind, "err", err)
	}
}
