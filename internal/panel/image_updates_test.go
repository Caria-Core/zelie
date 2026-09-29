package panel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
	"github.com/opencontainers/go-digest"
)

// fakeRegistry knows what each tag points at and what each digest says
// its version is.
type fakeRegistry struct {
	mu       sync.Mutex
	tags     map[string]digest.Digest
	versions map[digest.Digest]string
	asked    int // version lookups
}

func (f *fakeRegistry) Digest(_ context.Context, ref string) (digest.Digest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tags[ref], nil
}

func (f *fakeRegistry) Version(_ context.Context, ref string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked++
	return f.versions[digestOf(ref)], nil
}

func digestN(n string) digest.Digest { return digest.Digest("sha256:" + strings.Repeat(n, 64)) }

func TestDatabaseUpdate(t *testing.T) {
	e := newAppEnv(t)
	reg := &fakeRegistry{tags: map[string]digest.Digest{"postgres:18": digestN("1")},
		versions: map[digest.Digest]string{digestN("1"): "18.5", digestN("2"): "18.6"}}
	e.s.Registry = reg
	e.core.digests = map[string]string{"postgres:18": digestN("1").String()}
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	first := e.settle(t, "pg")

	e.s.checkImages(context.Background())
	if _, out := e.b.do("GET", "/api/apps/pg", nil); out["update"] != nil {
		t.Fatalf("update with nothing new: %v", out["update"])
	}

	// A new build of postgres:18 is out.
	reg.mu.Lock()
	reg.tags["postgres:18"] = digestN("2")
	reg.mu.Unlock()
	e.s.checkImages(context.Background())
	e.s.checkImages(context.Background())
	_, out := e.b.do("GET", "/api/apps/pg", nil)
	if up, _ := out["update"].(map[string]any); up["current"] != "18.5" || up["version"] != "18.6" {
		t.Fatalf("update %v", out["update"])
	}
	if reg.asked != 2 {
		t.Errorf("asked the registry for %d versions; a digest's version never changes", reg.asked)
	}

	// The backup fails: nothing changes.
	e.core.bk.failBackup, e.core.bk.failures = "disk full", 1
	if code, _ := e.b.do("POST", "/api/apps/pg/update", nil); code != http.StatusCreated {
		t.Fatalf("update: %d", code)
	}
	d := e.settle(t, "pg")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "update.backup_failed" {
		t.Fatalf("update with a failed backup %+v", d)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "pg"); live.ID != first.ID {
		t.Fatal("the database moved without a backup")
	}

	e.core.mu.Lock()
	e.core.digests["postgres:18"] = digestN("2").String()
	e.core.mu.Unlock()
	e.b.do("POST", "/api/apps/pg/update", nil)
	d = e.settle(t, "pg")
	if d.State != store.DeployLive || d.Cause != store.CauseUpdate || d.Image != "postgres:18@"+digestN("2").String() {
		t.Fatalf("update %+v", d)
	}
	list, _ := e.backups(t, "pg")
	if len(list) == 0 || list[0]["reason"] != "update" || list[0]["state"] != "done" {
		t.Fatalf("no backup before the update: %v", list)
	}
	if _, out := e.b.do("GET", "/api/apps/pg", nil); out["update"] != nil {
		t.Errorf("update still offered after it went live: %v", out["update"])
	}
}

func TestUpdateNeedsAnImage(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")
	if code, out := e.b.do("POST", "/api/apps/web/update", nil); code != http.StatusConflict || out["code"] != "update.not_image" {
		t.Errorf("update a github app: %d %v", code, out)
	}
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/apps/pg/stop", nil)
	if code, out := e.b.do("POST", "/api/apps/pg/update", nil); code != http.StatusConflict || out["code"] != "update.start_db_first" {
		t.Errorf("update a stopped database: %d %v", code, out)
	}
}
