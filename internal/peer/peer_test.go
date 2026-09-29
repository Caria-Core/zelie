package peer

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPolicy(t *testing.T) {
	p := Policy{UIDs: []uint32{999}}
	for uid, want := range map[uint32]bool{0: true, 999: true, 1000: false} {
		if got := p.Allows(Peer{UID: uid}); got != want {
			t.Errorf("uid %d: got %v, want %v", uid, got, want)
		}
	}
}

func TestRequireRoutes(t *testing.T) {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	mux.HandleFunc("GET /v1/one/{name}", ok)
	mux.HandleFunc("POST /v1/one/{name}", ok)
	mux.HandleFunc("GET /v1/two", ok)
	policy := Policy{UIDs: []uint32{999}, Routes: map[uint32][]string{500: {"GET /v1/one/{name}"}}}
	h := RequireRoutes(policy, slog.New(slog.NewTextHandler(io.Discard, nil)), mux)

	for _, tc := range []struct {
		uid            uint32
		method, target string
		want           int
	}{
		{0, "GET", "/v1/two", http.StatusNoContent},
		{999, "POST", "/v1/one/x", http.StatusNoContent},
		{500, "GET", "/v1/one/x", http.StatusNoContent},
		{500, "POST", "/v1/one/x", http.StatusForbidden},
		{500, "GET", "/v1/two", http.StatusForbidden},
		{500, "GET", "/v1/none", http.StatusForbidden},
		{1000, "GET", "/v1/one/x", http.StatusForbidden},
	} {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		req = req.WithContext(WithPeer(req.Context(), Peer{UID: tc.uid}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("uid %d %s %s: got %d, want %d", tc.uid, tc.method, tc.target, rec.Code, tc.want)
		}
	}
}
