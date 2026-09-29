package proxy

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Counts are the requests one host has had since the proxy started.
type Counts struct {
	Requests uint64 `json:"requests"`
	// ClientErrors are 4xx answers; ServerErrors are 5xx, including the
	// 502 the proxy gives when the app does not answer.
	ClientErrors uint64 `json:"client_errors"`
	ServerErrors uint64 `json:"server_errors"`
}

// Stats is what the proxy has counted. Since changes when the proxy
// restarts and the counts start again from zero.
type Stats struct {
	Since time.Time         `json:"since"`
	Hosts map[string]Counts `json:"hosts"`
}

type counter struct {
	requests, client, server atomic.Uint64
}

func (c *counter) count(status int) {
	if c == nil {
		return
	}
	c.requests.Add(1)
	switch {
	case status >= 500:
		c.server.Add(1)
	case status >= 400:
		c.client.Add(1)
	}
}

// counters keeps a counter per host across configuration changes.
type counters struct {
	since  time.Time
	mu     sync.Mutex
	byHost map[string]*counter
}

func (cs *counters) get(host string) *counter {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.byHost == nil {
		cs.byHost = map[string]*counter{}
	}
	c, ok := cs.byHost[host]
	if !ok {
		c = &counter{}
		cs.byHost[host] = c
	}
	return c
}

func (cs *counters) stats() Stats {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := Stats{Since: cs.since, Hosts: make(map[string]Counts, len(cs.byHost))}
	for h, c := range cs.byHost {
		out.Hosts[h] = Counts{Requests: c.requests.Load(), ClientErrors: c.client.Load(), ServerErrors: c.server.Load()}
	}
	return out
}

// Stats returns the requests counted for each host.
func (p *Proxy) Stats() Stats { return p.counters.stats() }

// counted reports whether a failed request is the app's doing, not a
// visitor who went away before the answer.
func counted(r *http.Request) bool { return r.Context().Err() == nil }
