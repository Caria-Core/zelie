package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	images        []engine.Image
	volumes       []string
	links         map[string][]engine.Link

	containers []engine.Status // replaces List's answer when set
	exec       func(args []string, stdin io.Reader, stdout io.Writer) uint32
	volumeDir  string
	// More volumes by name, and those a running container holds.
	volumeDirs  map[string]string
	usedVolumes map[string]bool
}

func (f *fakeEngine) Exec(_ context.Context, _ string, args []string, stdin io.Reader, stdout, _ io.Writer) (uint32, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	return f.exec(args, stdin, stdout), nil
}

func (f *fakeEngine) volumePath(name string) (string, error) {
	if name == "data" && f.volumeDir != "" {
		return f.volumeDir, nil
	}
	if d, ok := f.volumeDirs[name]; ok {
		return d, nil
	}
	return "", errdefs.ErrNotFound
}

func (f *fakeEngine) OpenVolume(_ context.Context, name string) (*os.Root, error) {
	if f.usedVolumes[name] {
		return nil, fmt.Errorf("volume %s in use: %w", name, errdefs.ErrFailedPrecondition)
	}
	return f.ReadVolume(name)
}

func (f *fakeEngine) ReadVolume(name string) (*os.Root, error) {
	dir, err := f.volumePath(name)
	if err != nil {
		return nil, err
	}
	return os.OpenRoot(dir)
}

func (f *fakeEngine) VolumeSize(name string) (int64, error) {
	dir, err := f.volumePath(name)
	if err != nil {
		return 0, err
	}
	var n int64
	err = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			fi, _ := d.Info()
			n += fi.Size()
		}
		return err
	})
	return n, err
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

func (f *fakeEngine) Links(app string) ([]engine.Link, error) { return f.links[app], nil }

func (f *fakeEngine) SetLinks(_ context.Context, app string, links []engine.Link) error {
	if f.links == nil {
		f.links = map[string][]engine.Link{}
	}
	f.links[app] = links
	return nil
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
	f.images = slices.DeleteFunc(f.images, func(i engine.Image) bool { return i.Name == name })
	return nil
}

func (f *fakeEngine) Images(context.Context) ([]engine.Image, error) {
	return slices.Clone(f.images), nil
}

func (f *fakeEngine) SetUnused(_ context.Context, name string, since time.Time) error {
	for i := range f.images {
		if f.images[i].Name == name {
			f.images[i].UnusedSince = since
			return nil
		}
	}
	return errors.New("no such image")
}

func (f *fakeEngine) List(context.Context) ([]engine.Status, error) {
	if f.containers != nil {
		return f.containers, nil
	}
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

func TestLinkedEnv(t *testing.T) {
	s, f := newServer()
	keys, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Secrets = keys
	root := &peer.Peer{UID: 0}
	sealed, _ := secret.Seal(keys.Public(), "pg", "POSTGRES_PASSWORD", "hunter2")
	body := func(app string) string {
		return `{"id":"` + app + `-1","app":"` + app + `","image":"busybox","memory_bytes":1,"cpus":1,"pids":1,
			"linked_env":[{"name":"DATABASE_URL","template":"postgresql://app:{secret}@pg:5432/app","from":"pg","sealed":"` + sealed + `"}]}`
	}
	f.SetLinks(context.Background(), "web", []engine.Link{{Name: "pg", To: "pg", Port: 5432}})
	if rec := request(t, s, root, "POST", "/v1/containers", body("web")); rec.Code != http.StatusCreated {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	if env := strings.Join(f.ran[0].Env, " "); env != "DATABASE_URL=postgresql://app:hunter2@pg:5432/app" {
		t.Errorf("env %q", env)
	}
	// An app that is not linked gets nothing, whatever the panel asks.
	if rec := request(t, s, root, "POST", "/v1/containers", body("other")); rec.Code != http.StatusBadRequest || strings.Contains(rec.Body.String(), "hunter2") {
		t.Errorf("unlinked app: %d %s", rec.Code, rec.Body)
	}
	if len(f.ran) != 1 {
		t.Errorf("ran %d containers", len(f.ran))
	}
}

func TestLinks(t *testing.T) {
	s, f := newServer()
	root := &peer.Peer{UID: 0}
	for _, c := range []struct {
		body string
		want int
	}{
		{`[{"name":"db","to":"pg","port":5432}]`, http.StatusNoContent},
		{`[{"name":"db","to":"web","port":5432}]`, http.StatusBadRequest},
		{`[{"name":"db","to":"pg","port":0}]`, http.StatusBadRequest},
		{`[{"name":"db.x","to":"pg","port":5432}]`, http.StatusBadRequest},
		{`[{"name":"db","to":"pg","port":5432},{"name":"db","to":"redis","port":6379}]`, http.StatusBadRequest},
	} {
		if rec := request(t, s, root, "PUT", "/v1/links/web", c.body); rec.Code != c.want {
			t.Errorf("%s: status %d, want %d: %s", c.body, rec.Code, c.want, rec.Body)
		}
	}
	want := []engine.Link{{Name: "db", To: "pg", Port: 5432}}
	if got := f.links["web"]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("links %v, want %v", got, want)
	}
	rec := request(t, s, root, "GET", "/v1/links/web", "")
	if strings.TrimSpace(rec.Body.String()) != `[{"name":"db","to":"pg","port":5432}]` {
		t.Errorf("list: %s", rec.Body)
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
