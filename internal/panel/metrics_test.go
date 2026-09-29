package panel

import (
	"context"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

func TestMetrics(t *testing.T) {
	e := newAppEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx", "domain": "Web.example.com"})
	e.settle(t, "web")
	started := now
	count := func(req, client, server uint64) {
		e.proxy.mu.Lock()
		e.proxy.stats = proxy.Stats{Since: started, Hosts: map[string]proxy.Counts{"web.example.com": {Requests: req, ClientErrors: client, ServerErrors: server}}}
		e.proxy.mu.Unlock()
	}
	ctx := context.Background()

	// The first reading is where counting starts.
	count(100, 5, 2)
	e.s.recordMetrics(ctx)
	if list, _ := e.s.Store.Metrics(ctx, "web", time.Time{}); len(list) != 0 {
		t.Fatalf("saved a first reading %+v", list)
	}
	now = now.Add(time.Minute)
	count(160, 7, 3)
	e.s.recordMetrics(ctx)
	// The proxy restarts and counts from zero.
	now, started = now.Add(time.Minute), now.Add(30*time.Second)
	count(10, 0, 1)
	e.s.recordMetrics(ctx)

	list, _ := e.s.Store.Metrics(ctx, "web", time.Time{})
	if len(list) != 2 {
		t.Fatalf("metrics %+v", list)
	}
	m := list[0]
	if m.Requests != 60 || m.ClientErrors != 2 || m.ServerErrors != 1 || m.MemoryBytes != 84<<20 || m.CPU <= 0 {
		t.Errorf("first minute %+v", m)
	}
	if m := list[1]; m.Requests != 10 || m.ServerErrors != 1 {
		t.Errorf("after the proxy restarted %+v", m)
	}

	_, out := e.b.do("GET", "/api/apps/web/metrics", nil)
	if pts, _ := out["points"].([]any); len(pts) != 2 || out["step"] != 60.0 {
		t.Errorf("hour %v", out)
	}
	_, out = e.b.do("GET", "/api/apps/web/metrics?range=24h", nil)
	if pts, _ := out["points"].([]any); len(pts) != 1 || out["step"] != 300.0 || pts[0].(map[string]any)["requests"] != 70.0 {
		t.Errorf("day %v", out)
	}
	_, out = e.b.do("GET", "/api/apps/web/usage", nil)
	if out["requests_per_min"] != 35.0 || out["error_rate"] != 2.0/70 || out["crashes"] != 0.0 {
		t.Errorf("summary %v", out)
	}

	e.crash(t, "web")
	e.s.superviseOnce(ctx)
	if d := e.settle(t, "web"); d.Cause != store.CauseRecover {
		t.Fatalf("not brought back: %+v", d)
	}
	if _, out = e.b.do("GET", "/api/apps/web/usage", nil); out["crashes"] != 1.0 {
		t.Errorf("crashes %v", out["crashes"])
	}

	// A day later the readings are gone.
	now = now.Add(25 * time.Hour)
	e.s.recordMetrics(ctx)
	if list, _ := e.s.Store.Metrics(ctx, "web", time.Time{}); len(list) != 0 {
		t.Errorf("kept %d old readings", len(list))
	}
}
