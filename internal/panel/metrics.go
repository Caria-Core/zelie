package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

const (
	metricsEvery = time.Minute
	// metricsKeep is how far back the graphs go.
	metricsKeep = 24 * time.Hour
	// summaryWindow is what the numbers in an app's header cover.
	summaryWindow = 5 * time.Minute
)

// meter keeps the last reading of each app, so the next one gives what
// happened in between: counters only ever grow.
type meter struct {
	mu   sync.Mutex
	last map[string]reading
}

type reading struct {
	container string
	at        time.Time
	cpuUsec   int64
	rx, tx    int64
	since     time.Time // when the proxy started counting
	counts    proxy.Counts
}

func (s *Server) runMetrics(ctx context.Context) {
	t := time.NewTicker(metricsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.recordMetrics(ctx)
		}
	}
}

// recordMetrics reads what each running app used since the last minute
// and saves it.
func (s *Server) recordMetrics(ctx context.Context) {
	now := s.now()
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.Log.Error("metrics: list apps", "err", err)
		return
	}
	// Without the proxy an app's requests are unknown this minute, not zero.
	stats, statsErr := s.Proxy.Stats(ctx)
	var list []store.Metric
	for _, a := range apps {
		live, err := s.Store.LiveDeployment(ctx, a.ID)
		if err != nil {
			continue
		}
		container := fmt.Sprintf("%s-%d", a.ID, live.ID)
		u, err := s.Core.Usage(ctx, container)
		if err != nil {
			continue
		}
		cur := reading{container: container, at: now, cpuUsec: u.CPUUsec, rx: u.RxBytes, tx: u.TxBytes}
		if statsErr == nil {
			cur.since, cur.counts = stats.Since, stats.Hosts[strings.ToLower(a.Domain)]
		}
		s.meter.mu.Lock()
		if s.meter.last == nil {
			s.meter.last = map[string]reading{}
		}
		prev, ok := s.meter.last[a.ID]
		s.meter.last[a.ID] = cur
		s.meter.mu.Unlock()
		// A new container starts its counters over; its first minute is
		// measured from the next reading.
		elapsed := now.Sub(prev.at)
		if !ok || prev.container != container || elapsed <= 0 || u.CPUUsec < prev.cpuUsec {
			continue
		}
		m := store.Metric{AppID: a.ID, At: now.Truncate(time.Minute), MemoryBytes: u.MemoryBytes,
			CPU:     float64(u.CPUUsec-prev.cpuUsec) / float64(elapsed.Microseconds()),
			RxBytes: max(0, u.RxBytes-prev.rx), TxBytes: max(0, u.TxBytes-prev.tx)}
		if statsErr == nil {
			base := prev.counts
			if !prev.since.Equal(cur.since) {
				// The proxy restarted and counts from zero again.
				base = proxy.Counts{}
			}
			m.Requests = int64(cur.counts.Requests - min(base.Requests, cur.counts.Requests))
			m.ClientErrors = int64(cur.counts.ClientErrors - min(base.ClientErrors, cur.counts.ClientErrors))
			m.ServerErrors = int64(cur.counts.ServerErrors - min(base.ServerErrors, cur.counts.ServerErrors))
		}
		list = append(list, m)
	}
	if err := s.Store.AddMetrics(ctx, list); err != nil {
		s.Log.Error("metrics: save", "err", err)
	}
	if err := s.Store.PruneMetrics(ctx, now.Add(-metricsKeep)); err != nil {
		s.Log.Error("metrics: prune", "err", err)
	}
}

type pointJSON struct {
	At           time.Time `json:"at"`
	MemoryBytes  int64     `json:"memory_bytes"`
	CPU          float64   `json:"cpu"`
	RxBytes      int64     `json:"rx_bytes"`
	TxBytes      int64     `json:"tx_bytes"`
	Requests     int64     `json:"requests"`
	ClientErrors int64     `json:"client_errors"`
	ServerErrors int64     `json:"server_errors"`
}

type metricsJSON struct {
	// Step is the time each point covers, in seconds.
	Step   int         `json:"step"`
	Points []pointJSON `json:"points"`
	// Crashes counts the times the app stopped by itself in the last day.
	Crashes int `json:"crashes"`
}

// appMetrics returns an app's readings for the last hour, a point a
// minute, or the last day, a point every five minutes.
func (s *Server) appMetrics(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	span, step := time.Hour, time.Minute
	if r.URL.Query().Get("range") == "24h" {
		span, step = metricsKeep, 5*time.Minute
	}
	now := s.now()
	list, err := s.Store.Metrics(r.Context(), a.ID, now.Add(-span))
	if err != nil {
		s.fail(w, "load metrics", err)
		return
	}
	crashes, err := s.Store.Recoveries(r.Context(), a.ID, now.Add(-24*time.Hour))
	if err != nil {
		s.fail(w, "count crashes", err)
		return
	}
	writeJSON(w, http.StatusOK, metricsJSON{Step: int(step / time.Second), Points: buckets(list, step), Crashes: crashes})
}

// buckets merges minutes into points of step: memory and CPU are averaged,
// bytes and requests added up.
func buckets(list []store.Metric, step time.Duration) []pointJSON {
	out := []pointJSON{}
	n := 0
	for _, m := range list {
		at := m.At.Truncate(step)
		if len(out) == 0 || !out[len(out)-1].At.Equal(at) {
			if n > 0 {
				avg(&out[len(out)-1], n)
			}
			out = append(out, pointJSON{At: at})
			n = 0
		}
		p := &out[len(out)-1]
		n++
		p.MemoryBytes += m.MemoryBytes
		p.CPU += m.CPU
		p.RxBytes += m.RxBytes
		p.TxBytes += m.TxBytes
		p.Requests += m.Requests
		p.ClientErrors += m.ClientErrors
		p.ServerErrors += m.ServerErrors
	}
	if n > 0 {
		avg(&out[len(out)-1], n)
	}
	return out
}

func avg(p *pointJSON, n int) {
	p.MemoryBytes /= int64(n)
	p.CPU /= float64(n)
}

// summary is the short line in an app's header: requests a minute and the
// share that failed over the last few minutes, and crashes today.
type summaryJSON struct {
	RequestsPerMin *float64 `json:"requests_per_min,omitempty"`
	ErrorRate      *float64 `json:"error_rate,omitempty"`
	Crashes        int      `json:"crashes"`
}

func (s *Server) summary(ctx context.Context, a store.App) (summaryJSON, error) {
	var out summaryJSON
	now := s.now()
	var err error
	if out.Crashes, err = s.Store.Recoveries(ctx, a.ID, now.Add(-24*time.Hour)); err != nil {
		return out, err
	}
	if a.Domain == "" {
		return out, nil
	}
	list, err := s.Store.Metrics(ctx, a.ID, now.Add(-summaryWindow))
	if err != nil || len(list) == 0 {
		return out, err
	}
	var req, fails int64
	for _, m := range list {
		req += m.Requests
		fails += m.ServerErrors
	}
	rpm := float64(req) / float64(len(list))
	out.RequestsPerMin = &rpm
	if req > 0 {
		rate := float64(fails) / float64(req)
		out.ErrorRate = &rate
	}
	return out, nil
}
