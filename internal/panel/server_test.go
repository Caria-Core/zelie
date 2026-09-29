package panel

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/store"
)

const proxyUID = 990

func newServer(t *testing.T) *Server {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Server{Store: db, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), ProxyUID: proxyUID}
}

func request(h http.Handler, uid uint32, method, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: uid}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWhoGetsWhat(t *testing.T) {
	h := newServer(t).Handler()
	cases := []struct {
		name   string
		uid    uint32
		method string
		path   string
		want   int
	}{
		{"root asks for a setup link", 0, "POST", "/local/setup-link", http.StatusOK},
		{"web visitor asks for a setup link", proxyUID, "POST", "/local/setup-link", http.StatusMethodNotAllowed},
		{"web visitor asks for a reset link", proxyUID, "POST", "/local/reset-link", http.StatusMethodNotAllowed},
		{"web visitor reads setup status", proxyUID, "GET", "/api/setup", http.StatusOK},
		{"other local user", 1000, "GET", "/api/setup", http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if rec := request(h, c.uid, c.method, c.path); rec.Code != c.want {
				t.Errorf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
}

func TestSetupLink(t *testing.T) {
	s := newServer(t)
	h := s.Handler()
	rec := request(h, 0, "POST", "/local/setup-link")
	var out struct{ Token string }
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil || len(out.Token) < 40 {
		t.Fatalf("token %q, %v", out.Token, err)
	}
}
