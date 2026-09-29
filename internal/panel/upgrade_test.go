package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
)

func (e *appEnv) mountOf(t *testing.T, app string) string {
	t.Helper()
	live, err := e.s.Store.LiveDeployment(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	m := e.core.mounts[fmt.Sprintf("%s-%d", app, live.ID)]
	if len(m) != 1 {
		t.Fatalf("%s mounts %v", app, m)
	}
	return m[0].Name
}

func TestDatabaseUpgrade(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres", "version": "17"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "pg"})
	oldVolume := e.mountOf(t, "pg")

	if _, out := e.b.do("GET", "/api/apps/pg", nil); out["upgrade_to"] != "18" {
		t.Fatalf("upgrade_to %v", out["upgrade_to"])
	}
	for _, v := range []string{"17", "16", ""} {
		if code, _ := e.b.do("POST", "/api/apps/pg/upgrade", map[string]any{"version": v}); code != http.StatusBadRequest {
			t.Errorf("upgrade to %q: %d", v, code)
		}
	}
	if code, out := e.b.do("POST", "/api/apps/pg/upgrade", map[string]any{"version": "18"}); code != http.StatusCreated {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	d := e.settle(t, "pg")
	if d.State != store.DeployLive || d.Cause != store.CauseUpgrade || !strings.HasPrefix(d.Image, "postgres:18") {
		t.Fatalf("upgrade %+v", d)
	}
	_, out := e.b.do("GET", "/api/apps/pg", nil)
	if out["engine_version"] != "18" || out["image"] != "postgres:18" || out["upgrade_to"] != nil {
		t.Errorf("after the upgrade %v", out)
	}
	if v := e.mountOf(t, "pg"); v == oldVolume {
		t.Error("the new version runs on the old files")
	}
	list, _ := e.backups(t, "pg")
	if len(list) == 0 || list[0]["reason"] != "upgrade" {
		t.Fatalf("backups %v", list)
	}
	if r := e.core.bk.restored; len(r) != 1 || !strings.HasPrefix(r[0], "pg/") {
		t.Errorf("loaded %v", r)
	}
	if w := e.settle(t, "web"); w.Cause != store.CauseRestore || w.State != store.DeployLive {
		t.Errorf("the linked app did not start again: %+v", w)
	}

	// The old files are kept until removed.
	code, body, _ := e.raw("GET", "/api/apps/pg/kept-volumes")
	if code != http.StatusOK || !strings.Contains(body, `"version":"17"`) {
		t.Fatalf("kept volumes: %d %s", code, body)
	}
	k, _ := e.s.Store.KeptVolumes(context.Background(), "pg")
	if len(k) != 1 || k[0].Name != oldVolume || !e.core.volumes[oldVolume] {
		t.Fatalf("kept %+v", k)
	}
	if code, _ := e.b.do("DELETE", fmt.Sprintf("/api/apps/pg/kept-volumes/%d", k[0].ID), nil); code != http.StatusNoContent {
		t.Fatalf("remove kept volume: %d", code)
	}
	if e.core.volumes[oldVolume] {
		t.Error("the old files are still there")
	}
}

func TestDatabaseUpgradeFailsBack(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres", "version": "17"})
	first := e.settle(t, "pg")
	oldVolume := e.mountOf(t, "pg")
	e.core.bk.failLoad = map[string]bool{"*": true}

	e.b.do("POST", "/api/apps/pg/upgrade", map[string]any{"version": "18"})
	d := e.settle(t, "pg")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "upgrade.load_failed" {
		t.Fatalf("upgrade %+v", d)
	}
	_, out := e.b.do("GET", "/api/apps/pg", nil)
	if out["engine_version"] != "17" || out["image"] != "postgres:17" || out["state"] != "running" {
		t.Errorf("after the failed upgrade %v", out)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "pg"); live.ID != first.ID {
		t.Errorf("live %+v", live)
	}
	if v := e.mountOf(t, "pg"); v != oldVolume {
		t.Errorf("runs on %s, not its own files %s", v, oldVolume)
	}
	if k, _ := e.s.Store.KeptVolumes(context.Background(), "pg"); len(k) != 0 {
		t.Errorf("kept %+v", k)
	}
	if vols, _ := e.s.Store.Volumes(context.Background(), "pg"); len(vols) != 1 || len(e.core.volumes) != 1 {
		t.Errorf("volumes %+v, core %v", vols, e.core.volumes)
	}
}
