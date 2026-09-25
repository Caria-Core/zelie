package panel

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestBuildAndStartCommands(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	first := e.settle(t, "web")
	_, app := e.b.do("GET", "/api/apps/web", nil)
	detected := app["detected"].(map[string]any)
	if detected["builder"] != "railpack" || detected["build"] != "npm run build" || detected["start"] != "node index.js" {
		t.Fatalf("detected %v", detected)
	}
	if args := e.core.args[fmt.Sprintf("web-%d", first.ID)]; args != nil {
		t.Errorf("the image's own command was replaced: %v", args)
	}

	e.b.do("PUT", "/api/apps/web/env", []map[string]any{{"name": "RAILPACK_BUILD_CMD", "value": "old"}, {"name": "A", "value": "1"}})
	if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"build_command": "npm run build:prod", "start_command": "node server.js"}); code != http.StatusNoContent {
		t.Fatalf("set: %d", code)
	}
	e.b.do("POST", "/api/apps/web/deployments", nil)
	d := e.settle(t, "web")
	if got := strings.Join(e.core.args[fmt.Sprintf("web-%d", d.ID)], " "); got != "sh -c node server.js" {
		t.Errorf("start args %q", got)
	}
	if !slices.Contains(e.core.buildEnv, "RAILPACK_BUILD_CMD=npm run build:prod") || slices.Contains(e.core.buildEnv, "RAILPACK_BUILD_CMD=old") {
		t.Errorf("build env %v", e.core.buildEnv)
	}

	for _, bad := range []map[string]any{{"start_command": "a\nb"}, {"build_command": strings.Repeat("x", 501)}} {
		if code, _ := e.b.do("PATCH", "/api/apps/web", bad); code != http.StatusBadRequest {
			t.Errorf("%v: %d", bad, code)
		}
	}
	e.b.do("POST", "/api/apps", map[string]any{"id": "img", "source": "image", "image": "nginx"})
	if code, _ := e.b.do("PATCH", "/api/apps/img", map[string]any{"build_command": "make"}); code != http.StatusBadRequest {
		t.Errorf("build command on an image app: %d", code)
	}
	if code, _ := e.b.do("PATCH", "/api/apps/img", map[string]any{"start_command": "nginx -g 'daemon off;'"}); code != http.StatusNoContent {
		t.Errorf("start command on an image app: %d", code)
	}
}

func TestRestartCanPullTheLatestCode(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")
	builds := len(e.core.builds)
	if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"restart_pulls": true}); code != http.StatusNoContent {
		t.Fatal(code)
	}
	e.source.commit = strings.Repeat("b", 40)
	e.b.do("POST", "/api/apps/web/restart", nil)
	d := e.settle(t, "web")
	if d.State != "live" || d.Cause != "restart" || d.Version != strings.Repeat("b", 40) || len(e.core.builds) != builds+1 {
		t.Errorf("restart that pulls: %+v, %d builds", d, len(e.core.builds)-builds)
	}
}
