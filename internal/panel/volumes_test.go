package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

func TestVolumes(t *testing.T) {
	e := newAppEnv(t)
	code, out := e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "itzg/minecraft-server",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 2048}}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	first := e.settle(t, "mc")
	if first.State != store.DeployLive {
		t.Fatalf("first deployment %+v", first)
	}
	want := []engine.VolumeMount{{Name: "vol-1", Target: "/data"}}
	if got := e.core.mounts[fmt.Sprintf("mc-%d", first.ID)]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("mounts %v, want %v", got, want)
	}

	// The old version stops before the new one starts.
	e.b.do("POST", "/api/apps/mc/restart", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployLive {
		t.Fatalf("restart %+v", d)
	}
	if e.core.overlapping {
		t.Error("two versions ran with the same volume")
	}

	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"path": "/proc/x"}, http.StatusBadRequest},
		{map[string]any{"path": "data"}, http.StatusBadRequest},
		{map[string]any{"path": "/data/worlds"}, http.StatusConflict},
		{map[string]any{"path": "/plugins", "limit_mb": 1}, http.StatusBadRequest},
		{map[string]any{"path": "/plugins", "limit_mb": 1 << 30}, http.StatusBadRequest},
		{map[string]any{"path": "/plugins"}, http.StatusCreated},
	} {
		if code, out := e.b.do("POST", "/api/apps/mc/volumes", c.body); code != c.want {
			t.Errorf("add %v: %d %v, want %d", c.body, code, out, c.want)
		}
	}
	if code, _ := e.b.do("PATCH", "/api/apps/mc/volumes/2", map[string]any{"path": "/data"}); code != http.StatusConflict {
		t.Errorf("moving onto another volume: %d", code)
	}

	// Deleting needs the app stopped.
	if code, _ := e.b.do("DELETE", "/api/apps/mc/volumes/2", nil); code != http.StatusConflict {
		t.Errorf("delete while running: %d", code)
	}
	e.b.do("POST", "/api/apps/mc/stop", nil)
	if code, out := e.b.do("DELETE", "/api/apps/mc/volumes/2", nil); code != http.StatusNoContent {
		t.Fatalf("delete while stopped: %d %v", code, out)
	}
	if e.core.volumes["vol-2"] {
		t.Error("the core still has the deleted volume")
	}

	// Deleting the app deletes what is left.
	if code, _ := e.b.do("DELETE", "/api/apps/mc", nil); code != http.StatusNoContent {
		t.Fatalf("delete app: %d", code)
	}
	if len(e.core.volumes) != 0 {
		t.Errorf("volumes left after deleting the app: %v", e.core.volumes)
	}
}

func TestVolumeOverItsLimitStopsTheApp(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "itzg/minecraft-server",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 100}}})
	e.settle(t, "mc")

	e.core.sizes = map[string]int64{"vol-1": 101 << 20}
	e.s.checkVolumes(context.Background())
	a, _ := e.s.Store.App(context.Background(), "mc")
	if !a.Stopped {
		t.Fatal("the app kept running over its limit")
	}
	_, out := e.b.do("GET", "/api/apps/mc", nil)
	if full, _ := out["volume_full"].(string); !strings.Contains(full, "/data") {
		t.Errorf("volume_full %q", full)
	}

	e.b.do("POST", "/api/apps/mc/start", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployFailed || !strings.Contains(d.Error, "over its") {
		t.Fatalf("start over the limit: %+v", d)
	}

	if code, _ := e.b.do("PATCH", "/api/apps/mc/volumes/1", map[string]any{"limit_mb": 200}); code != http.StatusOK {
		t.Fatalf("raise the limit: %d", code)
	}
	e.b.do("POST", "/api/apps/mc/start", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployLive {
		t.Fatalf("start after raising the limit: %+v", d)
	}
}

func TestFailedVersionWithVolumesBringsTheOldOneBack(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "good:1",
		"volumes": []map[string]any{{"path": "/data"}}})
	live := e.settle(t, "mc")

	e.core.crashImage = "bad:2"
	e.b.do("PATCH", "/api/apps/mc", map[string]any{"image": "bad:2"})
	e.b.do("POST", "/api/apps/mc/deployments", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployFailed {
		t.Fatalf("bad version %+v", d)
	}
	st, err := e.s.containerStatus(context.Background(), fmt.Sprintf("mc-%d", live.ID))
	if err != nil || st.State != "running" || st.Image != "good:1" {
		t.Errorf("the old version is not back: %+v %v", st, err)
	}
	if e.core.overlapping {
		t.Error("two versions ran with the same volume")
	}
}
