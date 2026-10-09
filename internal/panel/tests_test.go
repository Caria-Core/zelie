package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
)

func TestTestsRunBeforeGoingLive(t *testing.T) {
	e := newAppEnv(t)
	e.core.suggest = "npm test"
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")
	e.b.do("PUT", "/api/apps/web/env", []map[string]any{{"name": "TOKEN", "value": "hunter2", "secret": true}})
	e.b.do("POST", "/api/apps/web/deployments", nil)
	first := e.settle(t, "web")
	if first.State != store.DeployLive {
		t.Fatalf("first %+v", first)
	}
	_, app := e.b.do("GET", "/api/apps/web", nil)
	if app["test_command"] != "npm test" {
		t.Fatalf("suggested command not saved: %v", app["test_command"])
	}
	if len(e.core.tests) != 2 {
		t.Fatalf("%d test runs", len(e.core.tests))
	}
	run := e.core.tests[1]
	if strings.Join(run.Args, " ") != "sh -c npm test" || run.ID != fmt.Sprintf("web-%d-test", first.ID) || run.Network != "" {
		t.Errorf("test container %+v", run)
	}
	if !slices.Contains(run.Env, "CI=true") || !slices.Contains(run.Env, "TOKEN=hunter2") {
		t.Errorf("test env %v", run.Env)
	}
	if _, err := e.s.containerStatus(context.Background(), run.ID); err == nil {
		t.Error("the test container was left behind")
	}
	if b, _ := readFile(e.s.deployLogPath(first.ID)); !strings.Contains(b, "The tests passed.") {
		t.Errorf("log:\n%s", b)
	}

	// Failing tests keep the old version.
	e.core.testExit = 1
	e.source.commit = strings.Repeat("b", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	d := e.settle(t, "web")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "deploy.tests_failed" || d.Error.Text != "The tests failed with exit code 1." {
		t.Fatalf("failing tests %+v", d)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "web"); live.ID != first.ID {
		t.Error("a version with failing tests went live")
	}

	// A restart does not build, so it does not test.
	runs := len(e.core.tests)
	e.b.do("POST", "/api/apps/web/restart", nil)
	if d := e.settle(t, "web"); d.State != store.DeployLive || len(e.core.tests) != runs {
		t.Errorf("restart %+v, %d test runs", d, len(e.core.tests)-runs)
	}

	// Clearing the command turns tests off for good; the next build's
	// suggestion does not bring them back.
	if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"test_command": ""}); code != http.StatusNoContent {
		t.Fatalf("clear: %d", code)
	}
	e.b.do("POST", "/api/apps/web/deployments", nil)
	if d := e.settle(t, "web"); d.State != store.DeployLive || len(e.core.tests) != runs {
		t.Errorf("after clearing: %+v, %d test runs", d, len(e.core.tests)-runs)
	}
	if _, app := e.b.do("GET", "/api/apps/web", nil); app["test_command"] != "" {
		t.Errorf("command came back: %v", app["test_command"])
	}

	for _, bad := range []string{"npm test\nrm -rf /", strings.Repeat("x", 501)} {
		if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"test_command": bad}); code != http.StatusBadRequest {
			t.Errorf("%q: %d", bad[:10], code)
		}
	}
	e.b.do("POST", "/api/apps", map[string]any{"id": "img", "source": "image", "image": "nginx"})
	if code, _ := e.b.do("PATCH", "/api/apps/img", map[string]any{"test_command": "true"}); code != http.StatusBadRequest {
		t.Errorf("tests on an image app: %d", code)
	}
}

// A test suite may empty the database it is pointed at, so the tests of a
// new build get neither a network nor a database's variables.
func TestTestsAreIsolatedFromLinkedDatabases(t *testing.T) {
	e := newAppEnv(t)
	e.core.suggest = "npm test"
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")
	e.b.do("PUT", "/api/apps/web/env", []map[string]any{{"name": "TOKEN", "value": "hunter2", "secret": true}})
	if code, out := e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "pg"}); code != http.StatusCreated {
		t.Fatalf("link: %d %v", code, out)
	}

	e.b.do("POST", "/api/apps/web/deployments", nil)
	d := e.settle(t, "web")
	if d.State != store.DeployLive {
		t.Fatalf("deployment %+v", d)
	}
	run := e.core.tests[len(e.core.tests)-1]
	if run.ID != fmt.Sprintf("web-%d-test", d.ID) {
		t.Fatalf("last test run %+v", run)
	}
	if run.Network != "" {
		t.Errorf("the tests joined network %q", run.Network)
	}
	for _, kv := range run.Env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "PG") || name == "DATABASE_URL" {
			t.Errorf("the tests got %s", name)
		}
	}
	if !slices.Contains(run.Env, "TOKEN=hunter2") || !slices.Contains(run.Env, "CI=true") || !slices.Contains(run.Env, "PORT=3000") {
		t.Errorf("test env %v", run.Env)
	}
	// The app itself still gets its database.
	if env := e.core.env[fmt.Sprintf("web-%d", d.ID)]; !slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "DATABASE_URL=") }) {
		t.Errorf("the app lost its database variables: %v", env)
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); !strings.Contains(b, "without a network") {
		t.Errorf("log:\n%s", b)
	}
}
