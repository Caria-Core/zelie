package core

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/update"
	"github.com/Caria-Core/zelie/internal/version"
)

func TestStartUpdate(t *testing.T) {
	old := version.Version
	version.Version = "v0.1.3"
	t.Cleanup(func() { version.Version = old })

	s, _ := newServer()
	panel := &peer.Peer{UID: 999}
	var placed []byte
	var started string
	running, installed := false, true
	fetch := func(context.Context, string) ([]byte, error) { return []byte("new"), nil }
	s.Updater = &Updater{
		Fetch:     func(ctx context.Context, v string) ([]byte, error) { return fetch(ctx, v) },
		Place:     func(b []byte) error { placed = b; return nil },
		Start:     func(_ context.Context, from, to string) error { started = from + ">" + to; return nil },
		Running:   func(context.Context) bool { return running },
		Last:      func() (update.Result, error) { return update.Result{From: "v0.1.2", To: "v0.1.3", OK: true}, nil },
		Installed: func() bool { return installed },
	}
	post := func(v string) (int, string) {
		rec := request(t, s, panel, "POST", "/v1/update", `{"version":"`+v+`"}`)
		return rec.Code, rec.Body.String()
	}

	for _, c := range []struct {
		version string
		code    int
		text    string
	}{
		{"v0.1.3", http.StatusConflict, "update.not_newer"},
		{"v0.1.0", http.StatusConflict, "update.not_newer"},
		{"latest", http.StatusBadRequest, ""},
	} {
		if code, body := post(c.version); code != c.code || !strings.Contains(body, c.text) {
			t.Errorf("%s: %d %s", c.version, code, body)
		}
	}
	installed = false
	if code, body := post("v0.2.0"); code != http.StatusConflict || !strings.Contains(body, "update.not_installed") {
		t.Errorf("not installed: %d %s", code, body)
	}
	installed, running = true, true
	if code, body := post("v0.2.0"); code != http.StatusConflict || !strings.Contains(body, "update.running") {
		t.Errorf("already running: %d %s", code, body)
	}
	running = false
	fetch = func(context.Context, string) ([]byte, error) {
		return nil, errors.New("the release's signature does not match Zelie's key")
	}
	if code, body := post("v0.2.0"); code != http.StatusBadGateway || !strings.Contains(body, "update.download") || placed != nil {
		t.Errorf("bad signature: %d %s, placed %q", code, body, placed)
	}
	fetch = func(context.Context, string) ([]byte, error) { return []byte("new"), nil }
	if code, body := post("v0.2.0"); code != http.StatusAccepted || string(placed) != "new" || started != "v0.1.3>v0.2.0" {
		t.Errorf("update: %d %s, placed %q, started %q", code, body, placed, started)
	}

	rec := request(t, s, panel, "GET", "/v1/update", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"version":"v0.1.3"`) || !strings.Contains(rec.Body.String(), `"to":"v0.1.3"`) {
		t.Errorf("status: %d %s", rec.Code, rec.Body)
	}
}
