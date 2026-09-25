package core

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/containerd/errdefs"
)

type fakeEngine struct {
	ran     []engine.Spec
	stopped map[string]time.Duration
}

func (f *fakeEngine) Run(_ context.Context, s engine.Spec) error {
	if s.ID == "taken" {
		return errdefs.ErrAlreadyExists
	}
	f.ran = append(f.ran, s)
	return nil
}

func (f *fakeEngine) Stop(_ context.Context, id string, grace time.Duration) error {
	if id == "missing" {
		return errdefs.ErrNotFound
	}
	if f.stopped == nil {
		f.stopped = map[string]time.Duration{}
	}
	f.stopped[id] = grace
	return nil
}

func (f *fakeEngine) Remove(context.Context, string) error { return nil }

func (f *fakeEngine) List(context.Context) ([]engine.Status, error) {
	return []engine.Status{{ID: "web", Image: "busybox", State: "running", Pid: 42, Userns: 1 << 30}}, nil
}

func request(t *testing.T, s *Server, p *peer.Peer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if p != nil {
		req = req.WithContext(peer.WithPeer(req.Context(), *p))
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func newServer() (*Server, *fakeEngine) {
	f := &fakeEngine{}
	return &Server{
		Engine:  f,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Allowed: peer.Policy{UIDs: []uint32{999}},
	}, f
}

func TestPeerCheck(t *testing.T) {
	s, _ := newServer()
	cases := []struct {
		name string
		peer *peer.Peer
		want int
	}{
		{"root", &peer.Peer{UID: 0}, http.StatusOK},
		{"panel user", &peer.Peer{UID: 999}, http.StatusOK},
		{"someone else", &peer.Peer{UID: 1000}, http.StatusForbidden},
		{"unknown peer", nil, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := request(t, s, c.peer, "GET", "/v1/containers", "").Code; got != c.want {
				t.Errorf("status %d, want %d", got, c.want)
			}
		})
	}
}

func TestRun(t *testing.T) {
	root := &peer.Peer{UID: 0}
	valid := `{"id":"web","image":"busybox","memory_bytes":67108864,"cpus":0.5,"pids":64}`
	cases := []struct {
		name string
		body string
		want int
	}{
		{"valid", valid, http.StatusCreated},
		{"unknown field", `{"id":"web","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,"privileged":true}`, http.StatusBadRequest},
		// Builder mode and host mounts belong to the core's own build
		// flow; whoever talks to the socket cannot ask for them.
		{"builder", `{"id":"web","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,"builder":true}`, http.StatusBadRequest},
		{"mounts", `{"id":"web","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,"mounts":[{"source":"/","target":"/host"}]}`, http.StatusBadRequest},
		{"no limits", `{"id":"web","image":"busybox"}`, http.StatusBadRequest},
		{"bad id", `{"id":"../etc","image":"busybox","memory_bytes":1,"cpus":1,"pids":1}`, http.StatusBadRequest},
		{"trailing data", valid + `{}`, http.StatusBadRequest},
		{"already exists", `{"id":"taken","image":"busybox","memory_bytes":1,"cpus":1,"pids":1}`, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := newServer()
			if got := request(t, s, root, "POST", "/v1/containers", c.body).Code; got != c.want {
				t.Errorf("status %d, want %d", got, c.want)
			}
		})
	}
}

func TestStop(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	if got := request(t, s, root, "POST", "/v1/containers/web/stop", "").Code; got != http.StatusNoContent {
		t.Fatalf("status %d", got)
	}
	if f.stopped["web"] != 10*time.Second {
		t.Errorf("default grace %v, want 10s", f.stopped["web"])
	}
	if got := request(t, s, root, "POST", "/v1/containers/web/stop", `{"grace_seconds":3600}`).Code; got != http.StatusBadRequest {
		t.Errorf("an hour of grace should be refused, got %d", got)
	}
	if got := request(t, s, root, "POST", "/v1/containers/missing/stop", "").Code; got != http.StatusNotFound {
		t.Errorf("missing container: status %d", got)
	}
}

func TestList(t *testing.T) {
	s, _ := newServer()
	rec := request(t, s, &peer.Peer{UID: 0}, "GET", "/v1/containers", "")
	if !strings.Contains(rec.Body.String(), `"id":"web"`) {
		t.Errorf("unexpected body %s", rec.Body)
	}
}
