package panel

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

func TestExternalAccess(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); code != http.StatusOK || out["enabled"] != false {
		t.Fatalf("before: %d %v", code, out)
	}

	// Something else on the server holds the first port.
	e.core.takenPorts = map[int]bool{15432: true}
	code, out := e.b.do("POST", "/api/apps/pg/external", nil)
	password, _ := out["password"].(string)
	if code != http.StatusOK || out["port"] != 15433.0 || out["user"] != "zelie_external" || out["database"] != "app" || len(password) != 32 {
		t.Fatalf("set up: %d %v", code, out)
	}
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); code != http.StatusOK || out["port"] != 15433.0 || out["password"] != nil {
		t.Errorf("after: %d %v", code, out)
	}

	// A new password keeps the port.
	code, out = e.b.do("POST", "/api/apps/pg/external", nil)
	if code != http.StatusOK || out["port"] != 15433.0 || out["password"] == password {
		t.Errorf("new password: %d %v", code, out)
	}
	e.core.mu.Lock()
	sets := slices.Clone(e.core.extSets)
	e.core.mu.Unlock()
	if !slices.Equal(sets, []string{"pg 15432 true", "pg 15433 true", "pg 15433 true"}) {
		t.Errorf("core asked %v", sets)
	}
	// The password never lands in the panel's database.
	if vars, _ := e.s.Store.Env(context.Background(), "pg"); slices.ContainsFunc(vars, func(v store.EnvVar) bool { return v.Name == externalPasswordVar }) {
		t.Error("a Postgres keeps the external password")
	}

	// Setting it up again asks to confirm first; turning it off does not.
	later := e.s.now().Add(time.Hour)
	e.s.Now = func() time.Time { return later }
	if code, out := e.b.do("POST", "/api/apps/pg/external", nil); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("without confirming: %d %v", code, out)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/pg/external", nil); code != http.StatusNoContent {
		t.Fatalf("turn off: %d", code)
	}
	if code, out := e.b.do("GET", "/api/apps/pg/external", nil); out["enabled"] != false {
		t.Errorf("after turning off: %d %v", code, out)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/pg/external", nil); code != http.StatusNotFound {
		t.Errorf("turn off twice: %d", code)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if len(e.core.external) != 0 {
		t.Errorf("core still listens: %v", e.core.external)
	}
}

func TestExternalRedisAndDelete(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "cache", "engine": "redis"})
	e.settle(t, "cache")
	code, out := e.b.do("POST", "/api/apps/cache/external", nil)
	if code != http.StatusOK || out["port"] != 16379.0 || out["database"] != nil {
		t.Fatalf("set up: %d %v", code, out)
	}
	// Redis gets the password on its command line, sealed.
	vars, _ := e.s.Store.Env(context.Background(), "cache")
	i := slices.IndexFunc(vars, func(v store.EnvVar) bool { return v.Name == externalPasswordVar })
	if i < 0 || !vars[i].Secret || strings.Contains(vars[i].Value, out["password"].(string)) {
		t.Fatalf("variables %+v", vars)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/cache", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if len(e.core.external) != 0 {
		t.Errorf("a deleted database's port is open: %v", e.core.external)
	}
}

func TestExternalNotForApps(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	if code, out := e.b.do("POST", "/api/apps/web/external", nil); code != http.StatusConflict || out["code"] != "external.not_database" {
		t.Errorf("an app: %d", code)
	}
}
