package panel

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestServerUpdate(t *testing.T) {
	e := newAppEnv(t)
	e.core.version = "v0.1.3"
	latest := Release{Version: "v0.2.0", Notes: "Updates from the panel.", Published: time.Unix(1_800_000_000, 0)}
	e.s.Releases = func(context.Context) (Release, error) { return latest, nil }

	// Nothing checked yet: nothing to offer.
	if code, out := e.b.do("GET", "/api/server", nil); code != http.StatusOK || out["version"] != "v0.1.3" || out["available"] != nil {
		t.Fatalf("before a check: %d %v", code, out)
	}
	if code, _ := e.b.do("POST", "/api/server/update", nil); code != http.StatusNotFound {
		t.Errorf("update with nothing newer: %d", code)
	}
	code, out := e.b.do("POST", "/api/server/check", nil)
	avail, _ := out["available"].(map[string]any)
	if code != http.StatusOK || avail["version"] != "v0.2.0" || avail["notes"] != "Updates from the panel." {
		t.Fatalf("after a check: %d %v", code, out)
	}

	// Updating asks to confirm first, like other changes to the server.
	confirmedAt := e.s.now()
	e.s.Now = func() time.Time { return confirmedAt.Add(time.Hour) }
	if code, out := e.b.do("POST", "/api/server/update", nil); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("without confirming: %d %v", code, out)
	}
	e.s.Now = func() time.Time { return confirmedAt }
	if code, _ := e.b.do("POST", "/api/server/update", nil); code != http.StatusAccepted {
		t.Fatalf("update: %d", code)
	}
	if e.core.updatedTo != "v0.2.0" {
		t.Errorf("core asked for %q", e.core.updatedTo)
	}

	// Once running the newest, nothing is offered; a release older than
	// what runs never is.
	e.core.version = "v0.2.0"
	if _, out := e.b.do("GET", "/api/server", nil); out["available"] != nil {
		t.Errorf("up to date: %v", out)
	}
	e.core.version = "dev"
	if _, out := e.b.do("GET", "/api/server", nil); out["available"] != nil {
		t.Errorf("a dev build is offered %v", out["available"])
	}

	// A failed check is reported, not hidden.
	e.s.Releases = func(context.Context) (Release, error) { return Release{}, errors.New("GitHub answered 503") }
	if _, out := e.b.do("POST", "/api/server/check", nil); out["check_error"] != "GitHub answered 503" {
		t.Errorf("failed check: %v", out)
	}
}
