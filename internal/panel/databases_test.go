package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

func TestDatabase(t *testing.T) {
	e := newAppEnv(t)
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"id": "pg", "engine": "oracle"}, http.StatusBadRequest},
		{map[string]any{"id": "pg", "engine": "postgres", "version": "9"}, http.StatusBadRequest},
		{map[string]any{"id": "zelie-db", "engine": "postgres"}, http.StatusBadRequest},
		{map[string]any{"id": "pg", "engine": "postgres"}, http.StatusCreated},
		{map[string]any{"id": "pg", "engine": "redis"}, http.StatusConflict},
	} {
		if code, out := e.b.do("POST", "/api/databases", c.body); code != c.want {
			t.Errorf("create %v: %d %v, want %d", c.body, code, out, c.want)
		}
	}
	d := e.settle(t, "pg")
	if d.State != store.DeployLive || d.Image != "postgres:18" {
		t.Fatalf("deployment %+v", d)
	}
	container := fmt.Sprintf("pg-%d", d.ID)
	env := e.core.env[container]
	password := value(env, "POSTGRES_PASSWORD")
	if len(password) != 32 || value(env, "POSTGRES_USER") != "app" || value(env, "PORT") != "" {
		t.Errorf("database env %v", env)
	}
	if m := e.core.mounts[container]; len(m) != 1 || m[0].Target != "/var/lib/postgresql" {
		t.Errorf("mounts %v", m)
	}
	_, out := e.b.do("GET", "/api/apps/pg", nil)
	if out["engine"] != "postgres" || out["engine_version"] != "18" {
		t.Errorf("app %v", out)
	}

	// Zelie owns what makes it a database.
	for _, c := range []struct {
		method, path string
		body         any
		want         int
	}{
		{"PATCH", "/api/apps/pg", map[string]any{"image": "postgres:9"}, http.StatusBadRequest},
		{"PATCH", "/api/apps/pg", map[string]any{"memory_mb": 1024}, http.StatusNoContent},
		{"PUT", "/api/apps/pg/env", []map[string]any{{"name": "X", "value": "1"}}, http.StatusBadRequest},
		{"POST", "/api/apps/pg/volumes", map[string]any{"path": "/more"}, http.StatusBadRequest},
		{"DELETE", "/api/apps/pg/volumes/1", nil, http.StatusBadRequest},
	} {
		if code, out := e.b.do(c.method, c.path, c.body); code != c.want {
			t.Errorf("%s %s: %d %v, want %d", c.method, c.path, code, out, c.want)
		}
	}

	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/databases", map[string]any{"id": "cache", "engine": "redis"})
	e.settle(t, "cache")
	if code, out := e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "pg"}); code != http.StatusCreated {
		t.Fatalf("link: %d %v", code, out)
	}
	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"db": "pg"}, http.StatusConflict},
		{map[string]any{"db": "web"}, http.StatusBadRequest},
		{map[string]any{"db": "cache", "prefix": "bad-"}, http.StatusBadRequest},
		{map[string]any{"db": "cache"}, http.StatusCreated},
	} {
		if code, out := e.b.do("POST", "/api/apps/web/links", c.body); code != c.want {
			t.Errorf("link %v: %d %v, want %d", c.body, code, out, c.want)
		}
	}
	if code, _ := e.b.do("POST", "/api/apps/pg/links", map[string]any{"db": "cache"}); code != http.StatusBadRequest {
		t.Errorf("linking a database to another: %d", code)
	}
	want := []engine.Link{{Name: "pg", To: "pg", Port: 5432}, {Name: "cache", To: "cache", Port: 6379}}
	if got := e.core.links["web"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("core links %v, want %v", got, want)
	}

	// A second Postgres needs a prefix.
	e.b.do("POST", "/api/databases", map[string]any{"id": "analytics", "engine": "postgres", "version": "17"})
	e.settle(t, "analytics")
	code, out := e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "analytics"})
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "ANALYTICS_") {
		t.Errorf("clashing link: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "analytics", "prefix": "ANALYTICS_"}); code != http.StatusCreated {
		t.Fatalf("prefixed link: %d %v", code, out)
	}

	// The variables arrive with the next start.
	e.b.do("POST", "/api/apps/web/restart", nil)
	live := e.settle(t, "web")
	env = e.core.env[fmt.Sprintf("web-%d", live.ID)]
	for name, want := range map[string]string{
		"DATABASE_URL":           "postgresql://app:" + password + "@pg:5432/app",
		"PGHOST":                 "pg",
		"PGPASSWORD":             password,
		"REDIS_HOST":             "cache",
		"ANALYTICS_PGHOST":       "analytics",
		"ANALYTICS_DATABASE_URL": "postgresql://app:",
	} {
		if got := value(env, name); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.HasPrefix(value(env, "REDIS_URL"), "redis://default:") {
		t.Errorf("REDIS_URL %q", value(env, "REDIS_URL"))
	}
	// Nothing with a password is stored by the panel in the clear.
	vars, _ := e.s.Store.Env(context.Background(), "pg")
	for _, v := range vars {
		if strings.Contains(v.Value, password) {
			t.Errorf("%s holds the password in the clear", v.Name)
		}
	}

	if code, _ := e.b.do("PUT", "/api/apps/web/env", []map[string]any{{"name": "PGHOST", "value": "x"}}); code != http.StatusConflict {
		t.Errorf("own variable named like a linked one: %d", code)
	}
	if code, _ := e.b.do("PATCH", "/api/apps/web/links/cache", map[string]any{"prefix": "SESSIONS_"}); code != http.StatusNoContent {
		t.Errorf("change prefix: %d", code)
	}

	// Deleting a database unlinks it.
	if code, _ := e.b.do("DELETE", "/api/apps/pg", nil); code != http.StatusNoContent {
		t.Fatalf("delete database: %d", code)
	}
	links, _ := e.s.Store.Links(context.Background(), "web", "")
	if len(links) != 2 || slices.ContainsFunc(e.core.links["web"], func(l engine.Link) bool { return l.To == "pg" }) {
		t.Errorf("links after deleting pg: %v, core %v", links, e.core.links["web"])
	}
	if code, _ := e.b.do("DELETE", "/api/apps/web/links/cache", nil); code != http.StatusNoContent {
		t.Errorf("unlink: %d", code)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete app: %d", code)
	}
	if len(e.core.links["web"]) != 0 {
		t.Errorf("core still links the deleted app: %v", e.core.links["web"])
	}
}

func TestDatabaseEngines(t *testing.T) {
	e := newAppEnv(t)
	for _, c := range []struct{ engine, image, path string }{
		{"mariadb", "mariadb:11.8", "/var/lib/mysql"},
		{"redis", "redis:8", "/data"},
	} {
		if code, out := e.b.do("POST", "/api/databases", map[string]any{"id": c.engine, "engine": c.engine}); code != http.StatusCreated {
			t.Fatalf("create %s: %d %v", c.engine, code, out)
		}
		d := e.settle(t, c.engine)
		container := fmt.Sprintf("%s-%d", c.engine, d.ID)
		if d.State != store.DeployLive || d.Image != c.image || e.core.mounts[container][0].Target != c.path {
			t.Errorf("%s: %+v %v", c.engine, d, e.core.mounts[container])
		}
	}
	if args := e.core.args[fmt.Sprintf("redis-%d", e.settle(t, "redis").ID)]; !strings.Contains(strings.Join(args, " "), "--requirepass") {
		t.Errorf("redis args %v", args)
	}
}

func value(env []string, name string) string {
	for _, v := range env {
		if n, val, _ := strings.Cut(v, "="); n == name {
			return val
		}
	}
	return ""
}

func TestShowPassword(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	d := e.settle(t, "pg")
	password := value(e.core.env[fmt.Sprintf("pg-%d", d.ID)], "POSTGRES_PASSWORD")
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")

	code, out := e.b.do("POST", "/api/apps/pg/password", nil)
	if code != http.StatusOK || out["password"] != password {
		t.Fatalf("show: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/apps/web/password", nil); code != http.StatusConflict || out["code"] != "external.not_database" {
		t.Errorf("an app: %d %v", code, out)
	}
	if code, out := e.asCustomer(t, requireAdmin(e.s.showPassword), "POST", "pg", nil); code != http.StatusForbidden || out["password"] != nil {
		t.Errorf("not an administrator: %d %v", code, out)
	}
	// Without a recent second step it asks for one first.
	confirmedAt := e.s.now()
	e.s.Now = func() time.Time { return confirmedAt.Add(time.Hour) }
	if code, out := e.b.do("POST", "/api/apps/pg/password", nil); code != http.StatusForbidden || out["confirm"] != true || out["password"] != nil {
		t.Errorf("without confirming: %d %v", code, out)
	}
}

// The core opens only the passwords it is told may be shown.
func TestEnginePasswordsAreRevealable(t *testing.T) {
	for _, e := range dbEngines {
		if !slices.Contains(core.RevealableNames, e.PasswordVar) {
			t.Errorf("%s: the core would not show %s", e.Name, e.PasswordVar)
		}
		if e.RootPasswordVar != "" && slices.Contains(core.RevealableNames, e.RootPasswordVar) {
			t.Errorf("%s: the core would show the root password", e.Name)
		}
	}
}

func TestGameServersLinkToDatabases(t *testing.T) {
	e, _ := newGameEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	d := e.settle(t, "pg")
	password := value(e.core.env[fmt.Sprintf("pg-%d", d.ID)], "POSTGRES_PASSWORD")
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "survival", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "survival")

	for _, c := range []struct {
		h    http.HandlerFunc
		verb string
	}{{e.s.addLink, "POST"}, {e.s.updateLink, "PATCH"}, {e.s.deleteLink, "DELETE"}} {
		if code, out := e.asCustomer(t, c.h, c.verb, "survival", map[string]any{"db": "pg"}); code != http.StatusForbidden || out["code"] != "session.admin_only" {
			t.Errorf("%s as a customer: %d %v", c.verb, code, out)
		}
	}
	if links, _ := e.s.Store.Links(context.Background(), "survival", ""); len(links) != 0 {
		t.Fatalf("a refused link was saved: %v", links)
	}
	if code, out := e.b.do("POST", "/api/apps/survival/links", map[string]any{"db": "pg"}); code != http.StatusCreated {
		t.Fatalf("link: %d %v", code, out)
	}
	if got := e.core.links["survival"]; len(got) != 1 || got[0].To != "pg" {
		t.Errorf("core links %v", got)
	}

	if code, out := e.b.do("POST", "/api/games/survival/power", map[string]any{"action": "start"}); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "survival")
	var env []string
	for _, spec := range e.core.games {
		env = spec.Env
	}
	if value(env, "PGHOST") != "pg" || value(env, "DATABASE_URL") != "postgresql://app:"+password+"@pg:5432/app" {
		t.Errorf("game env %v", env)
	}
	if value(env, "PORT") != "" {
		t.Errorf("a game server got PORT: %v", env)
	}
}
