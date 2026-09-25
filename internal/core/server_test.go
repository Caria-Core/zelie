package core

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/containerd/errdefs"
)

type fakeEngine struct {
	ran     []engine.Spec
	stopped map[string]time.Duration

	removedImages []string
	volumes       []string
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

func (f *fakeEngine) CreateVolume(name string) error {
	if name == "taken" {
		return errdefs.ErrAlreadyExists
	}
	f.volumes = append(f.volumes, name)
	return nil
}

func (f *fakeEngine) RemoveVolume(_ context.Context, name string) error {
	if name == "in-use" {
		return errdefs.ErrFailedPrecondition
	}
	return nil
}

func (f *fakeEngine) VolumeSizes() (map[string]int64, error) {
	return map[string]int64{"data": 4096}, nil
}

func (f *fakeEngine) Usage(id string) (engine.Usage, error) {
	if id != "web" {
		return engine.Usage{}, errdefs.ErrNotFound
	}
	return engine.Usage{MemoryBytes: 1 << 20, CPUUsec: 500}, nil
}

func (f *fakeEngine) Wait(_ context.Context, id string) (uint32, error) {
	if id == "missing" {
		return 0, errdefs.ErrNotFound
	}
	return 3, nil
}

func (f *fakeEngine) RemoveImage(_ context.Context, name string) error {
	f.removedImages = append(f.removedImages, name)
	return nil
}

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

func TestSealedEnv(t *testing.T) {
	s, f := newServer()
	keys, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Secrets = keys
	root := &peer.Peer{UID: 0}

	rec := request(t, s, root, "GET", "/v1/secrets/key", "")
	var out struct{ Key string }
	json.NewDecoder(rec.Body).Decode(&out)
	pub, err := secret.ParsePublicKey(out.Key)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := secret.Seal(pub, "web", "TOKEN", "hunter2")
	body := func(app string) string {
		return `{"id":"web-1","app":"` + app + `","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,"env":["PORT=3000"],"sealed_env":["` + sealed + `"]}`
	}
	if got := request(t, s, root, "POST", "/v1/containers", body("web")).Code; got != http.StatusCreated {
		t.Fatalf("run: %d", got)
	}
	if env := strings.Join(f.ran[0].Env, " "); env != "PORT=3000 TOKEN=hunter2" || f.ran[0].App != "web" {
		t.Errorf("env %q app %q", env, f.ran[0].App)
	}
	if rec := request(t, s, root, "POST", "/v1/containers", body("other")); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "hunter2") {
		t.Fatalf("value sealed for another app: %d %s", rec.Code, rec.Body)
	}
}

func TestRemoveImage(t *testing.T) {
	s, f := newServer()
	panel := &peer.Peer{UID: 999}
	for _, name := range []string{"", "nginx:alpine", "docker.io/zelie.local/x:1"} {
		if rec := request(t, s, panel, "DELETE", "/v1/images?name="+name, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("%q: %d", name, rec.Code)
		}
	}
	if rec := request(t, s, panel, "DELETE", "/v1/images?name=zelie.local/web:abc", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body)
	}
	if len(f.removedImages) != 1 || f.removedImages[0] != "zelie.local/web:abc" {
		t.Errorf("removed %v", f.removedImages)
	}
}

func TestWait(t *testing.T) {
	s, _ := newServer()
	panel := &peer.Peer{UID: 999}
	rec := request(t, s, panel, "POST", "/v1/containers/web/wait", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"exit_code":3}` {
		t.Fatalf("wait: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, s, panel, "POST", "/v1/containers/missing/wait", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
	if rec := request(t, s, &peer.Peer{UID: 1000}, "POST", "/v1/containers/web/wait", ""); rec.Code != http.StatusForbidden {
		t.Errorf("stranger: %d", rec.Code)
	}
}

func TestUsageAndHost(t *testing.T) {
	s, _ := newServer()
	s.Host = func() (engine.Host, error) { return engine.Host{CPUs: 2, MemoryBytes: 3 << 30}, nil }
	panel := &peer.Peer{UID: 999}
	if rec := request(t, s, panel, "GET", "/v1/containers/web/usage", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"memory_bytes":1048576`) {
		t.Errorf("usage: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, s, panel, "GET", "/v1/containers/other/usage", ""); rec.Code != http.StatusNotFound {
		t.Errorf("usage of a stopped container: %d", rec.Code)
	}
	if rec := request(t, s, panel, "GET", "/v1/host", ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"cpus":2`) {
		t.Errorf("host: %d %s", rec.Code, rec.Body)
	}
}

func TestVolumes(t *testing.T) {
	s, f := newServer()
	root := &peer.Peer{UID: 0}
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"create", "POST", "/v1/volumes", `{"name":"data"}`, http.StatusCreated},
		{"create taken", "POST", "/v1/volumes", `{"name":"taken"}`, http.StatusConflict},
		{"create bad name", "POST", "/v1/volumes", `{"name":"../etc"}`, http.StatusBadRequest},
		{"remove", "DELETE", "/v1/volumes/data", "", http.StatusNoContent},
		{"remove in use", "DELETE", "/v1/volumes/in-use", "", http.StatusConflict},
		{"list", "GET", "/v1/volumes", "", http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := request(t, s, root, c.method, c.path, c.body)
			if rec.Code != c.want {
				t.Errorf("status %d, want %d: %s", rec.Code, c.want, rec.Body)
			}
		})
	}
	if len(f.volumes) != 1 || f.volumes[0] != "data" {
		t.Errorf("created %v", f.volumes)
	}

	body := `{"id":"web","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,"volumes":[{"name":"data","target":"/data"}]}`
	if rec := request(t, s, root, "POST", "/v1/containers", body); rec.Code != http.StatusCreated {
		t.Fatalf("run with a volume: %d %s", rec.Code, rec.Body)
	}
	if got := f.ran[len(f.ran)-1].Volumes; len(got) != 1 || got[0] != (engine.VolumeMount{Name: "data", Target: "/data"}) {
		t.Errorf("volumes passed to the engine: %v", got)
	}
	for _, target := range []string{"/proc/x", "/", "data", "/etc/hosts"} {
		body := `{"id":"web","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,"volumes":[{"name":"data","target":"` + target + `"}]}`
		if rec := request(t, s, root, "POST", "/v1/containers", body); rec.Code != http.StatusBadRequest {
			t.Errorf("volume on %s: status %d", target, rec.Code)
		}
	}
}
