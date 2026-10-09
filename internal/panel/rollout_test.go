package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
)

func TestHealthCheck(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web", "domain": "web.example.com", "health_path": "/healthz"})
	first := e.settle(t, "web")
	if first.State != store.DeployLive {
		t.Fatalf("first %+v", first)
	}
	if len(e.checked) == 0 || e.checked[0] != "web.example.com http://10.210.0.1:3000/healthz" {
		t.Fatalf("checked %v", e.checked)
	}

	// A version that answers with a server error never gets the domain.
	e.mu.Lock()
	e.health = 502
	e.mu.Unlock()
	e.b.do("POST", "/api/apps/web/deployments", nil)
	d := e.settle(t, "web")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "deploy.bad_answer" ||
		fmt.Sprint(d.Error.Params["status"], d.Error.Params["path"]) != "502/healthz" {
		t.Fatalf("unhealthy deployment %+v", d)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "web"); live.ID != first.ID {
		t.Error("the unhealthy version went live")
	}

	// Without a domain nothing is asked; staying up is enough.
	e.b.do("POST", "/api/apps", map[string]any{"id": "bot", "source": "github", "repo": "owner/bot"})
	if d := e.settle(t, "bot"); d.State != store.DeployLive {
		t.Fatalf("bot %+v", d)
	}
	for _, c := range e.checked {
		if strings.Contains(c, ":3000") && !strings.HasPrefix(c, "web.example.com") {
			t.Errorf("checked an app without a domain: %s", c)
		}
	}

	for _, p := range []string{"healthz", "//evil.example", "/a b", "/x#y"} {
		if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"health_path": p}); code != http.StatusBadRequest {
			t.Errorf("path %q: %d", p, code)
		}
	}
}

func TestRestartAndRollback(t *testing.T) {
	e := newAppEnv(t)
	e.core.crash = true
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")
	if code, _ := e.b.do("POST", "/api/apps/web/restart", nil); code != http.StatusConflict {
		t.Fatalf("restarted with nothing live: %d", code)
	}
	e.core.crash = false
	e.b.do("POST", "/api/apps/web/deployments", nil)
	first := e.settle(t, "web")

	e.source.commit = strings.Repeat("b", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	second := e.settle(t, "web")
	builds := len(e.core.builds)

	if code, _ := e.b.do("POST", "/api/apps/web/restart", nil); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	restarted := e.settle(t, "web")
	if restarted.State != store.DeployLive || restarted.Cause != store.CauseRestart || restarted.Image != second.Image || restarted.Version != second.Version {
		t.Fatalf("restart %+v", restarted)
	}
	if len(e.core.builds) != builds {
		t.Error("a restart built the app")
	}

	if code, _ := e.b.do("POST", fmt.Sprintf("/api/apps/web/deployments/%d/rollback", first.ID), nil); code != http.StatusCreated {
		t.Fatalf("rollback: %d", code)
	}
	back := e.settle(t, "web")
	if back.State != store.DeployLive || back.Cause != store.CauseRollback || back.Image != first.Image {
		t.Fatalf("rollback %+v", back)
	}
	if len(e.core.builds) != builds {
		t.Error("a rollback built the app")
	}
	list, _ := e.core.List(context.Background())
	if len(list) != 1 || list[0].Image != back.Image {
		t.Errorf("containers after rollback %+v", list)
	}

	e.core.failBuild = true
	e.b.do("POST", "/api/apps/web/deployments", nil)
	failed := e.settle(t, "web")
	if code, _ := e.b.do("POST", fmt.Sprintf("/api/apps/web/deployments/%d/rollback", failed.ID), nil); code != http.StatusConflict {
		t.Errorf("rolled back to a failed deployment: %d", code)
	}
}

func TestOldImagesAreRemoved(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")
	for _, c := range "bcdefg" {
		e.source.commit = strings.Repeat(string(c), 40)
		e.b.do("POST", "/api/apps/web/deployments", nil)
		e.settle(t, "web")
	}
	// Seven versions went live; the oldest two lost their image, one at a
	// time.
	list, _ := e.s.Store.Deployments(context.Background(), "web", 10)
	oldest := list[len(list)-1]
	if got, want := strings.Join(e.core.removed, " "), oldest.Image+" "+list[len(list)-2].Image; got != want {
		t.Errorf("removed %q, want %q", got, want)
	}
	if !oldest.Pruned {
		t.Fatalf("oldest %+v", oldest)
	}
	if code, _ := e.b.do("POST", fmt.Sprintf("/api/apps/web/deployments/%d/rollback", oldest.ID), nil); code != http.StatusConflict {
		t.Errorf("rolled back to a removed image: %d", code)
	}

	// A failed build's image is not worth keeping.
	e.core.crash = true
	e.source.commit = strings.Repeat("9", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	e.settle(t, "web")
	e.core.crash = false
	e.source.commit = strings.Repeat("8", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	e.settle(t, "web")
	if !strings.Contains(strings.Join(e.core.removed, " "), "zelie.local/web:999999999999-") {
		t.Errorf("the crashed version's image was kept: %v", e.core.removed)
	}
}

func TestImagesArePinned(t *testing.T) {
	e := newAppEnv(t)
	const old, newer = "sha256:" + "1111111111111111111111111111111111111111111111111111111111111111", "sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
	e.core.digests = map[string]string{"nginx:1.27": old}
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx:1.27"})
	first := e.settle(t, "web")
	if first.State != store.DeployLive || first.Image != "nginx:1.27@"+old || first.Version != "nginx:1.27" {
		t.Fatalf("first %+v", first)
	}

	// A new build of the tag is out. Restarting keeps what ran; deploying
	// takes the new one.
	e.core.mu.Lock()
	e.core.digests["nginx:1.27"] = newer
	e.core.mu.Unlock()
	e.b.do("POST", "/api/apps/web/restart", nil)
	if d := e.settle(t, "web"); d.State != store.DeployLive || d.Image != first.Image {
		t.Fatalf("restart %+v", d)
	}
	e.b.do("POST", "/api/apps/web/deployments", nil)
	if d := e.settle(t, "web"); d.State != store.DeployLive || d.Image != "nginx:1.27@"+newer {
		t.Fatalf("deploy %+v", d)
	}
}

func TestLiveDeploymentsGetPinned(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx:1.27"})
	if d := e.settle(t, "web"); d.Image != "nginx:1.27" {
		t.Fatalf("made before pinning: %+v", d)
	}
	e.core.mu.Lock()
	e.core.digests = map[string]string{"nginx:1.27": "sha256:" + strings.Repeat("3", 64)}
	e.core.mu.Unlock()
	e.s.pinLive(context.Background())
	if d, _ := e.s.Store.LiveDeployment(context.Background(), "web"); d.Image != "nginx:1.27@sha256:"+strings.Repeat("3", 64) {
		t.Fatalf("after pinLive %+v", d)
	}
}
