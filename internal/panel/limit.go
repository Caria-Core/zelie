package panel

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"
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
	locks  keyLocks
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

// Add records a failed attempt. When it cannot, the caller must not answer
// as if it had: an attempt that goes uncounted is one the limit never sees.
func (f *failures) Add(key string, now time.Time) error {
	if err := f.store.AddFailure(context.Background(), f.kind, f.hash(key), now, now.Add(-f.window)); err != nil {
		return fmt.Errorf("record a failed %s attempt: %w", f.kind, err)
	}
	return nil
}

// Lock holds key until the returned function is called. A request holds it
// from the moment it reads the count until its failure is added, so the
// attempts at one key are checked one after another. Without it a burst of
// parallel requests all read the same count and every one of them gets
// checked. It gives up when ctx ends, since the client is gone by then.
func (f *failures) Lock(ctx context.Context, key string) (unlock func(), err error) {
	return f.locks.lock(ctx, string(f.hash(key)))
}

// lockBoth takes a's lock on ka, then b's on kb. Everyone takes them in
// this order, so two requests cannot wait on each other.
func lockBoth(ctx context.Context, a *failures, ka string, b *failures, kb string) (unlock func(), err error) {
	unlockA, err := a.Lock(ctx, ka)
	if err != nil {
		return nil, err
	}
	unlockB, err := b.Lock(ctx, kb)
	if err != nil {
		unlockA()
		return nil, err
	}
	return func() { unlockB(); unlockA() }, nil
}

// Reset forgets key, after a successful login.
func (f *failures) Reset(key string) {
	if err := f.store.ResetFailures(context.Background(), f.kind, f.hash(key)); err != nil {
		f.log.Error("login limits: reset", "kind", f.kind, "err", err)
	}
}

// keyLocks hands out one lock per key and forgets a key once nobody holds
// or waits for it, so keys chosen by a stranger do not pile up.
type keyLocks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	taken chan struct{} // holds a value while the lock is held
	users int           // the holder and those waiting
}

func (k *keyLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*keyLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &keyLock{taken: make(chan struct{}, 1)}
		k.m[key] = l
	}
	l.users++
	k.mu.Unlock()

	leave := func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(k.m, key)
		}
	}
	select {
	case l.taken <- struct{}{}:
		return func() { <-l.taken; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}
