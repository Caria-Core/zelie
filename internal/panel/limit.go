package panel

import (
	"sync"
	"time"
)

// failures counts failed attempts per key, such as an IP address or an
// account, and blocks the key once it has too many in the window.
type failures struct {
	max    int
	window time.Duration

	mu   sync.Mutex
	seen map[string][]time.Time
}

func newFailures(max int, window time.Duration) *failures {
	return &failures{max: max, window: window, seen: map[string][]time.Time{}}
}

// Blocked reports whether key has used up its attempts.
func (f *failures) Blocked(key string, now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.recent(key, now)) >= f.max
}

// Add records a failed attempt.
func (f *failures) Add(key string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Someone cycling through many addresses must not grow the map without
	// bound.
	if len(f.seen) > 10_000 {
		for k := range f.seen {
			f.recent(k, now)
		}
	}
	f.seen[key] = append(f.recent(key, now), now)
}

// Reset forgets key, after a successful login.
func (f *failures) Reset(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.seen, key)
}

func (f *failures) recent(key string, now time.Time) []time.Time {
	times := f.seen[key]
	i := 0
	for i < len(times) && now.Sub(times[i]) >= f.window {
		i++
	}
	times = times[i:]
	if len(times) == 0 {
		delete(f.seen, key)
		return nil
	}
	f.seen[key] = times
	return times
}
