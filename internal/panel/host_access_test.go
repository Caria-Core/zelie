package panel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

func (e *appEnv) coreHostAccess(app string) bool {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	return e.core.hostAcc[app]
}

func (e *appEnv) storeHostAccess(t *testing.T, app string) bool {
	t.Helper()
	on, err := e.s.Store.HostAccess(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return on
}

func (e *appEnv) auditOf(t *testing.T, app string) []string {
	t.Helper()
	list, err := e.s.Store.Audit(context.Background(), app, 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, a := range list {
		out = append(out, a.Action)
	}
	return out
}

func TestHostAccess(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")

	code, out := e.b.do("GET", "/api/apps/web/host-access", nil)
	if code != http.StatusOK || out["on"] != false || out["name"] != "host.zelie.internal" || out["port"] != 3306.0 {
		t.Fatalf("before: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": true}); code != http.StatusOK || out["on"] != true {
		t.Fatalf("turn on: %d %v", code, out)
	}
	if !e.storeHostAccess(t, "web") || !e.coreHostAccess("web") {
		t.Error("host access is not on in both the store and the core")
	}
	if _, out := e.b.do("GET", "/api/apps/web/host-access", nil); out["on"] != true {
		t.Errorf("after: %v", out)
	}
	// Turning it on again changes nothing, and is not written down twice.
	e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": true})
	if code, _ := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": false}); code != http.StatusOK {
		t.Fatalf("turn off: %d", code)
	}
	if e.storeHostAccess(t, "web") || e.coreHostAccess("web") {
		t.Error("host access stayed on")
	}
	if got := e.auditOf(t, "web"); !slices.Equal(got, []string{"host_access_off", "host_access_on"}) {
		t.Errorf("audit %v", got)
	}
	if code, out := e.b.do("PUT", "/api/apps/nothing/host-access", map[string]any{"on": true}); code != http.StatusNotFound {
		t.Errorf("an app that does not exist: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": true, "port": 22}); code != http.StatusBadRequest {
		t.Errorf("a port in the request: %d %v", code, out)
	}
}

func TestHostAccessNeedsAnAdministratorWhoConfirmed(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")

	if code, out := e.asCustomer(t, requireAdmin(e.s.setHostAccess), "PUT", "web", map[string]any{"on": true}); code != http.StatusForbidden || out["code"] != "session.admin_only" {
		t.Errorf("not an administrator: %d %v", code, out)
	}
	if e.storeHostAccess(t, "web") || e.coreHostAccess("web") {
		t.Error("a customer turned host access on")
	}

	// Without a recent second step it asks for one first.
	confirmedAt := e.s.now()
	e.s.Now = func() time.Time { return confirmedAt.Add(time.Hour) }
	if code, out := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": true}); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("without confirming: %d %v", code, out)
	}
	if e.storeHostAccess(t, "web") || e.coreHostAccess("web") || len(e.auditOf(t, "web")) != 0 {
		t.Error("host access changed without a confirmation")
	}
	// Reading it needs only a login.
	if code, out := e.b.do("GET", "/api/apps/web/host-access", nil); code != http.StatusOK || out["on"] != false {
		t.Errorf("read: %d %v", code, out)
	}
}

func TestHostAccessIsTakenBackWhenTheCoreFails(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")

	e.core.mu.Lock()
	e.core.hostErr = errors.New("the core is restarting")
	e.core.mu.Unlock()
	if code, out := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": true}); code != http.StatusBadGateway {
		t.Fatalf("turn on: %d %v", code, out)
	}
	if e.storeHostAccess(t, "web") {
		t.Error("the store kept a change the core did not get")
	}
	if len(e.auditOf(t, "web")) != 0 {
		t.Error("a change that failed was written down")
	}

	e.core.mu.Lock()
	e.core.hostErr = nil
	e.core.mu.Unlock()
	if code, _ := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": true}); code != http.StatusOK {
		t.Fatalf("turn on again: %d", code)
	}
	// Turning it off must not be remembered as off when the core stays on.
	e.core.mu.Lock()
	e.core.hostErr = errors.New("the core is restarting")
	e.core.mu.Unlock()
	if code, _ := e.b.do("PUT", "/api/apps/web/host-access", map[string]any{"on": false}); code != http.StatusBadGateway {
		t.Fatalf("turn off: %d", code)
	}
	if !e.storeHostAccess(t, "web") {
		t.Error("the panel says off while the core may still have it on")
	}
}

func TestHostAccessOnlyForWebAppsAndGames(t *testing.T) {
	e, _ := newGameEnv(t)
	ctx := context.Background()
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "survival", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "survival")
	if err := e.s.Store.CreateApp(ctx, store.App{ID: "site", Source: store.SourceFiles, Image: "x", Port: 8080, MemoryMB: 64, CPUs: 1, CreatedAt: e.s.now()}); err != nil {
		t.Fatal(err)
	}

	for _, app := range []string{"pg", "site"} {
		for _, verb := range []string{"GET", "PUT"} {
			if code, out := e.b.do(verb, "/api/apps/"+app+"/host-access", map[string]any{"on": true}); code != http.StatusConflict || out["code"] != "hostaccess.not_supported" {
				t.Errorf("%s %s: %d %v", verb, app, code, out)
			}
		}
		if e.storeHostAccess(t, app) || e.coreHostAccess(app) {
			t.Errorf("%s got host access", app)
		}
	}

	// A game server can have it, and it is gone with the server.
	if code, out := e.b.do("PUT", "/api/apps/survival/host-access", map[string]any{"on": true}); code != http.StatusOK {
		t.Fatalf("game: %d %v", code, out)
	}
	if !e.coreHostAccess("survival") {
		t.Fatal("the core was not told")
	}
	if got := e.auditActions(t, "survival"); !slices.Equal(got, []string{"host_access_on"}) {
		t.Errorf("audit %v", got)
	}
	if code, out := e.b.do("DELETE", "/api/apps/survival", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	if e.coreHostAccess("survival") {
		t.Error("the core keeps host access of a deleted server")
	}
	if list, _ := e.s.Store.HostAccesses(ctx); len(list) != 0 {
		t.Errorf("store keeps %v", list)
	}
}

func TestHostAccessIsSyncedAtStart(t *testing.T) {
	e := newAppEnv(t)
	ctx := context.Background()
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/apps", map[string]any{"id": "api", "source": "image", "image": "nginx"})
	e.settle(t, "api")
	// The core has one on that the panel's database does not know, as after
	// restoring an older database.
	e.core.hostAcc = map[string]bool{"api": true}
	if err := e.s.Store.SetHostAccess(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	e.s.syncHostAccess(ctx)
	if !e.coreHostAccess("web") || e.coreHostAccess("api") {
		t.Errorf("core %v", e.core.hostAcc)
	}
}
