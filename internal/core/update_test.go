package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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
		Revert:    func() error { return nil },
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

// Two requests that arrive while the first is still downloading must not
// both place a binary: the second Place would keep the first's new binary
// as the one to go back to.
func TestUpdatesDoNotOverlap(t *testing.T) {
	old := version.Version
	version.Version = "v0.1.3"
	t.Cleanup(func() { version.Version = old })

	s, _ := newServer()
	panel := &peer.Peer{UID: 999}
	downloading, release := make(chan struct{}, 1), make(chan struct{})
	var mu sync.Mutex
	var fetched, placed, started int
	s.Updater = &Updater{
		Fetch: func(context.Context, string) ([]byte, error) {
			mu.Lock()
			fetched++
			first := fetched == 1
			mu.Unlock()
			if first {
				downloading <- struct{}{}
				<-release
			}
			return []byte("new"), nil
		},
		Place:     func([]byte) error { mu.Lock(); placed++; mu.Unlock(); return nil },
		Start:     func(context.Context, string, string) error { mu.Lock(); started++; mu.Unlock(); return nil },
		Revert:    func() error { return nil },
		Running:   func(context.Context) bool { return false },
		Last:      func() (update.Result, error) { return update.Result{}, nil },
		Installed: func() bool { return true },
	}

	first := make(chan int)
	go func() {
		first <- request(t, s, panel, "POST", "/v1/update", `{"version":"v0.2.0"}`).Code
	}()
	<-downloading
	rec := request(t, s, panel, "POST", "/v1/update", `{"version":"v0.2.0"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "update.running") {
		t.Errorf("second request: %d %s", rec.Code, rec.Body)
	}
	close(release)
	if code := <-first; code != http.StatusAccepted {
		t.Errorf("first request: %d", code)
	}
	mu.Lock()
	if fetched != 1 || placed != 1 || started != 1 {
		t.Errorf("fetched %d, placed %d, started %d", fetched, placed, started)
	}
	mu.Unlock()

	// Done with the first, a later one may go.
	s.Updater.Fetch = func(context.Context, string) ([]byte, error) { return []byte("newer"), nil }
	if rec := request(t, s, panel, "POST", "/v1/update", `{"version":"v0.3.0"}`); rec.Code != http.StatusAccepted {
		t.Errorf("after the first: %d %s", rec.Code, rec.Body)
	}
}

// A unit that does not start leaves the new binary where the next restart
// would run it, with nothing to check it. It goes back.
func TestFailedStartPutsTheOldBinaryBack(t *testing.T) {
	old := version.Version
	version.Version = "v0.1.3"
	t.Cleanup(func() { version.Version = old })

	s, _ := newServer()
	reverted := 0
	s.Updater = &Updater{
		Fetch:     func(context.Context, string) ([]byte, error) { return []byte("new"), nil },
		Place:     func([]byte) error { return nil },
		Start:     func(context.Context, string, string) error { return errors.New("systemd-run: no") },
		Revert:    func() error { reverted++; return nil },
		Running:   func(context.Context) bool { return false },
		Last:      func() (update.Result, error) { return update.Result{}, nil },
		Installed: func() bool { return true },
	}
	rec := request(t, s, &peer.Peer{UID: 999}, "POST", "/v1/update", `{"version":"v0.2.0"}`)
	if rec.Code != http.StatusInternalServerError || reverted != 1 {
		t.Errorf("%d %s, reverted %d times", rec.Code, rec.Body, reverted)
	}
	// And the lock is let go, so the next try can run.
	s.Updater.Start = func(context.Context, string, string) error { return nil }
	if rec := request(t, s, &peer.Peer{UID: 999}, "POST", "/v1/update", `{"version":"v0.2.0"}`); rec.Code != http.StatusAccepted {
		t.Errorf("retry: %d %s", rec.Code, rec.Body)
	}
}

// With the old binary not back, the next restart would run a release nothing
// has checked, and the log has to say so.
func TestFailedRevertIsLoggedAsSuch(t *testing.T) {
	old := version.Version
	version.Version = "v0.1.3"
	t.Cleanup(func() { version.Version = old })

	s, _ := newServer()
	var logged strings.Builder
	s.Log = slog.New(slog.NewTextHandler(&logged, nil))
	s.Updater = &Updater{
		Fetch:     func(context.Context, string) ([]byte, error) { return []byte("new"), nil },
		Place:     func([]byte) error { return nil },
		Start:     func(context.Context, string, string) error { return errors.New("systemd-run: no") },
		Revert:    func() error { return errors.New("disk full") },
		Running:   func(context.Context) bool { return false },
		Last:      func() (update.Result, error) { return update.Result{}, nil },
		Installed: func() bool { return true },
	}
	if rec := request(t, s, &peer.Peer{UID: 999}, "POST", "/v1/update", `{"version":"v0.2.0"}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logged.String(), "new binary is still in place") || !strings.Contains(logged.String(), "disk full") {
		t.Errorf("log: %s", logged.String())
	}
}
