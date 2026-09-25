package panel

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

// samples keeps the last CPU reading of each container, so the next one
// gives a rate.
type samples struct {
	mu   sync.Mutex
	last map[string]sample
}

type sample struct {
	usec int64
	at   time.Time
}

// rate returns the CPU use since the last reading, where 1 is one full CPU,
// or -1 if there is no earlier reading to compare with.
func (s *samples) rate(container string, usec int64, now time.Time) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == nil {
		s.last = map[string]sample{}
	}
	prev, ok := s.last[container]
	s.last[container] = sample{usec, now}
	elapsed := now.Sub(prev.at).Microseconds()
	if !ok || elapsed <= 0 || usec < prev.usec {
		return -1
	}
	return float64(usec-prev.usec) / float64(elapsed)
}

type usageJSON struct {
	Running     bool  `json:"running"`
	MemoryBytes int64 `json:"memory_bytes,omitempty"`
	// CPU is in CPUs, so 0.5 is half of one; absent until two readings.
	CPU *float64 `json:"cpu,omitempty"`
}

// appUsage reports what the app's live container uses now.
func (s *Server) appUsage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	live, err := s.Store.LiveDeployment(r.Context(), a.ID)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, usageJSON{})
		return
	}
	if err != nil {
		s.fail(w, "load deployment", err)
		return
	}
	container := fmt.Sprintf("%s-%d", a.ID, live.ID)
	u, err := s.Core.Usage(r.Context(), container)
	if err != nil {
		writeJSON(w, http.StatusOK, usageJSON{})
		return
	}
	out := usageJSON{Running: true, MemoryBytes: u.MemoryBytes}
	if cpu := s.samples.rate(container, u.CPUUsec, time.Now()); cpu >= 0 {
		out.CPU = &cpu
	}
	writeJSON(w, http.StatusOK, out)
}

// hostInfo tells the interface what the server has, so limits can be set
// against it.
func (s *Server) hostInfo(w http.ResponseWriter, r *http.Request) {
	h, err := s.Core.Host(r.Context())
	if err != nil {
		s.coreFailed(w, "host", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"cpus": h.CPUs, "memory_bytes": h.MemoryBytes, "disk_bytes": h.DiskBytes, "disk_free_bytes": h.DiskFreeBytes,
	})
}
