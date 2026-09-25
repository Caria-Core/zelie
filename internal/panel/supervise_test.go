package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

func init() {
	crashDelay = time.Second
}

// crash stops the app's live container the way a crashing process would.
func (e *appEnv) crash(t *testing.T, app string) {
	t.Helper()
	live, err := e.s.Store.LiveDeployment(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	id := fmt.Sprintf("%s-%d", app, live.ID)
	e.core.mu.Lock()
	st := e.core.containers[id]
	st.State = "stopped"
	e.core.containers[id] = st
	e.core.mu.Unlock()
}

func TestCrashedAppIsBroughtBack(t *testing.T) {
	e := newAppEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	first := e.settle(t, "web")

	e.crash(t, "web")
	e.s.superviseOnce(context.Background())
	d := e.settle(t, "web")
	if d.State != store.DeployLive || d.Cause != store.CauseRecover || d.Image != first.Image {
		t.Fatalf("recovery %+v", d)
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); !strings.Contains(b, "stopped by itself") || !strings.Contains(b, "EADDRINUSE") {
		t.Errorf("the log does not say why:\n%s", b)
	}

	// Right after, it waits before trying again.
	count := func() int {
		list, _ := e.s.Store.Deployments(context.Background(), "web", 100)
		return len(list)
	}
	before := count()
	e.crash(t, "web")
	e.s.superviseOnce(context.Background())
	e.s.deploys.wg.Wait()
	if count() != before {
		t.Error("brought back again without waiting")
	}

	// Five crashes within ten minutes and Zelie leaves it down.
	for range 5 {
		now = now.Add(20 * time.Second)
		e.crash(t, "web")
		e.s.superviseOnce(context.Background())
		e.s.deploys.wg.Wait()
	}
	_, app := e.b.do("GET", "/api/apps/web", nil)
	if msg, _ := app["crashing"].(string); !strings.Contains(msg, "5 times") {
		t.Fatalf("crashing: %v", app["crashing"])
	}
	before = count()
	now = now.Add(time.Hour)
	e.crash(t, "web")
	e.s.superviseOnce(context.Background())
	e.s.deploys.wg.Wait()
	if count() != before {
		t.Error("brought back after giving up")
	}

	// A restart by the user starts over.
	if code, _ := e.b.do("POST", "/api/apps/web/restart", nil); code != http.StatusCreated {
		t.Fatal(code)
	}
	e.settle(t, "web")
	if _, app := e.b.do("GET", "/api/apps/web", nil); app["crashing"] != nil {
		t.Errorf("still crashing after a restart: %v", app["crashing"])
	}
}

func TestServerRestartBringsAppsBack(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx", "domain": "web.example.com"})
	e.settle(t, "web")
	// After a reboot the containers are gone altogether.
	e.core.mu.Lock()
	clear(e.core.containers)
	e.core.mu.Unlock()
	e.s.superviseOnce(context.Background())
	d := e.settle(t, "web")
	if d.State != store.DeployLive || d.Cause != store.CauseRecover {
		t.Fatalf("after a reboot %+v", d)
	}
	if !strings.HasPrefix(e.proxy.routes(), "web.example.com=") {
		t.Errorf("routes %q", e.proxy.routes())
	}
}

func TestStoppedAppStaysDown(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.connect(t, g)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/private"})
	live := e.settle(t, "web")
	if code, _ := e.b.do("POST", "/api/apps/web/stop", nil); code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
	if _, app := e.b.do("GET", "/api/apps/web", nil); app["stopped"] != true {
		t.Fatalf("app %v", app)
	}
	count := func() int {
		list, _ := e.s.Store.Deployments(context.Background(), "web", 100)
		return len(list)
	}
	n := count()
	e.s.superviseOnce(context.Background())
	e.hook("push", "s1", push("refs/heads/main", strings.Repeat("e", 40), ""), g.secret)
	e.s.deploys.wg.Wait()
	if count() != n {
		t.Error("a stopped app was started")
	}

	if code, _ := e.b.do("POST", "/api/apps/web/start", nil); code != http.StatusCreated {
		t.Fatalf("start: %d", code)
	}
	d := e.settle(t, "web")
	if d.State != store.DeployLive || d.Cause != store.CauseRestart || d.Image != live.Image {
		t.Fatalf("start %+v", d)
	}
	if _, app := e.b.do("GET", "/api/apps/web", nil); app["stopped"] != false || app["state"] != "running" {
		t.Errorf("after start %v", app)
	}
}
