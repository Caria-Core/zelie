package panel

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestDeleteAppRemovesWhatItBuilt(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	d := e.settle(t, "web")
	if !strings.HasPrefix(d.Image, "zelie.local/web:") {
		t.Fatalf("image %q", d.Image)
	}
	if code, _ := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if !slices.Contains(e.core.removed, d.Image) || !slices.Equal(e.core.caches, []string{"web"}) {
		t.Errorf("images removed %v, build caches %v", e.core.removed, e.core.caches)
	}
}

func TestSweepKeepsWhatAppsNeed(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "site", "source": "image", "image": "nginx:alpine"})
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "site")
	d := e.settle(t, "web")
	e.s.sweepImages(context.Background())
	e.core.mu.Lock()
	keep := slices.Clone(e.core.keep)
	e.core.mu.Unlock()
	for _, img := range []string{"nginx:alpine", d.Image} {
		if !slices.Contains(keep, img) {
			t.Errorf("%s is not kept: %v", img, keep)
		}
	}

	e.b.do("DELETE", "/api/apps/site", nil)
	e.s.sweepImages(context.Background())
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	if slices.Contains(e.core.keep, "nginx:alpine") {
		t.Errorf("a deleted app's image is still kept: %v", e.core.keep)
	}
}
