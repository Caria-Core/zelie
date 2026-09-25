package panel

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/core"
)

func TestAppsNeedFullLogin(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	if code, _ := b.do("GET", "/api/apps", nil); code != http.StatusUnauthorized {
		t.Fatalf("logged out: %d", code)
	}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	if code, _ := b.do("GET", "/api/apps", nil); code != http.StatusUnauthorized {
		t.Fatalf("before the second step: %d", code)
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

func TestCoreFailuresAreHidden(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rec := httptest.NewRecorder()
	s.coreFailed(rec, "run", &core.Error{Status: http.StatusConflict, Message: "container web: already exists"})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("the core's answer to a bad request was not passed on: %d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	s.coreFailed(rec, "run", errors.New("dial unix /run/zelie/core.sock: connection refused"))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "/run/zelie") {
		t.Errorf("an internal error leaked: %d %s", rec.Code, rec.Body)
	}
}
