package panel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
)

type fakeCore struct {
	ran  []engine.Spec
	logs string
}

func (f *fakeCore) List(context.Context) ([]engine.Status, error) {
	return []engine.Status{{ID: "web", Image: "nginx", State: "running", IP: netip.MustParseAddr("10.210.1.2")}}, nil
}
func (f *fakeCore) Run(_ context.Context, s engine.Spec) error { f.ran = append(f.ran, s); return nil }
func (f *fakeCore) Stop(context.Context, string, int) error {
	return &core.Error{Status: http.StatusNotFound, Message: "container not found"}
}
func (f *fakeCore) Remove(context.Context, string) error { return io.ErrUnexpectedEOF }
func (f *fakeCore) Logs(_ context.Context, _ string, _ bool, _ int64, w io.Writer) error {
	_, err := io.WriteString(w, f.logs)
	return err
}

// signedIn returns a browser that has finished setup and its second step.
func signedIn(t *testing.T) (*browser, *fakeCore) {
	t.Helper()
	s, h, now := newAuthServer(t)
	fc := &fakeCore{}
	s.Core = fc
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	if code, _ := b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, out["secret"].(string), *now)}); code != http.StatusOK {
		t.Fatalf("enrol: %d", code)
	}
	return b, fc
}

func TestContainersNeedFullLogin(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	if code, _ := b.do("GET", "/api/containers", nil); code != http.StatusUnauthorized {
		t.Fatalf("logged out: %d", code)
	}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	if code, _ := b.do("GET", "/api/containers", nil); code != http.StatusUnauthorized {
		t.Fatalf("before the second step: %d", code)
	}
}

func TestContainers(t *testing.T) {
	b, fc := signedIn(t)
	if code, _ := b.do("GET", "/api/containers", nil); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if code, _ := b.do("POST", "/api/containers", map[string]any{"id": "web", "image": "nginx"}); code != http.StatusCreated {
		t.Fatalf("run: %d", code)
	}
	if got := fc.ran[0]; got.MemoryBytes != 512<<20 || got.CPUs != 1 || got.Pids != 512 {
		t.Errorf("defaults not applied: %+v", got)
	}
	if code, _ := b.do("POST", "/api/containers", map[string]any{"id": "Bad ID", "image": "nginx"}); code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := b.do("POST", "/api/containers", map[string]any{"id": "big", "image": "nginx", "memory_mb": 1 << 40}); code != http.StatusBadRequest {
		t.Fatalf("absurd memory: %d", code)
	}
	if code, out := b.do("POST", "/api/containers/web/stop", nil); code != http.StatusNotFound || out["error"] != "container not found" {
		t.Fatalf("core's not found not passed on: %d %v", code, out)
	}
	if code, out := b.do("DELETE", "/api/containers/web", nil); code != http.StatusBadGateway || strings.Contains(out["error"].(string), "EOF") {
		t.Fatalf("internal error leaked or wrong status: %d %v", code, out)
	}
}

func TestLogEventsKeepCharactersWhole(t *testing.T) {
	rec := httptest.NewRecorder()
	ev := newEventStream(rec)
	defer ev.close()
	text := []byte("héllo\n")
	ev.Write(text[:2]) // "h" and the first byte of "é"
	ev.Write(text[2:])
	want := "event: output\ndata: \"h\"\n\nevent: output\ndata: \"éllo\\n\"\n\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type %q", ct)
	}
}
