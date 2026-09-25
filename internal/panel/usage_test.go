package panel

import (
	"net/http"
	"testing"
	"time"
)

func TestUsageAndHost(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	_, u := e.b.do("GET", "/api/apps/web/usage", nil)
	if u["running"] != true || u["memory_bytes"] != float64(84<<20) || u["cpu"] != nil {
		t.Fatalf("first reading %v", u)
	}
	time.Sleep(10 * time.Millisecond)
	_, u = e.b.do("GET", "/api/apps/web/usage", nil)
	if cpu, ok := u["cpu"].(float64); !ok || cpu <= 0 {
		t.Errorf("second reading %v", u)
	}
	code, h := e.b.do("GET", "/api/host", nil)
	if code != http.StatusOK || h["cpus"] != 2.0 || h["memory_bytes"] != float64(3<<30) {
		t.Errorf("host %d %v", code, h)
	}
	e.b.do("POST", "/api/apps/web/stop", nil)
	if _, u := e.b.do("GET", "/api/apps/web/usage", nil); u["running"] != false {
		t.Errorf("stopped app %v", u)
	}
}

func TestRate(t *testing.T) {
	var s samples
	t0 := time.Unix(0, 0)
	if r := s.rate("c", 1000, t0); r != -1 {
		t.Errorf("first %v", r)
	}
	if r := s.rate("c", 501000, t0.Add(time.Second)); r != 0.5 {
		t.Errorf("half a CPU: %v", r)
	}
	if r := s.rate("c", 10, t0.Add(2*time.Second)); r != -1 {
		t.Errorf("after a restart the counter went down: %v", r)
	}
}
