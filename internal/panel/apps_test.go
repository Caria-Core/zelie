package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/Caria-Core/zelie/internal/store"
)

func init() {
	startupGrace, startupLimit, startupPoll = 20*time.Millisecond, 2*time.Second, 5*time.Millisecond
	testSettle = time.Millisecond
}

// appCore keeps containers in memory the way the core would, and opens
// sealed variables with a real key.
type appCore struct {
	mu         sync.Mutex
	keys       *secret.Keys
	containers map[string]engine.Status
	env        map[string][]string
	builds     []string
	removed    []string // images
	suggest    string   // the test command builds find
	buildEnv   []string // the variables the last build got, opened
	testExit   int
	tests      []engine.Spec
	failBuild  bool
	crash      bool // new containers stop right away
	next       byte
}

func newAppCore(t *testing.T) *appCore {
	keys, err := secret.LoadOrCreate(filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &appCore{keys: keys, containers: map[string]engine.Status{}, env: map[string][]string{}}
}

func (c *appCore) List(context.Context) ([]engine.Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []engine.Status
	for _, s := range c.containers {
		out = append(out, s)
	}
	return out, nil
}

func (c *appCore) Run(ctx context.Context, s engine.Spec) error { return c.RunApp(ctx, s, nil) }

func (c *appCore) RunApp(_ context.Context, s engine.Spec, sealed []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	env := slices.Clone(s.Env)
	for _, v := range sealed {
		opened, err := c.keys.Open(v, s.App)
		if err != nil {
			return err
		}
		env = append(env, opened)
	}
	c.next++
	state := "running"
	if c.crash {
		state = "stopped"
	}
	if strings.HasSuffix(s.ID, "-test") {
		c.tests = append(c.tests, s)
		c.tests[len(c.tests)-1].Env = env
	}
	c.containers[s.ID] = engine.Status{ID: s.ID, App: s.App, Image: s.Image, State: state, IP: netip.AddrFrom4([4]byte{10, 210, 0, c.next})}
	c.env[s.ID] = env
	return nil
}

func (c *appCore) Stop(context.Context, string, int) error { return nil }

func (c *appCore) Remove(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.containers, id)
	return nil
}

func (c *appCore) Logs(_ context.Context, _ string, _ bool, _ int64, w io.Writer) error {
	_, err := io.WriteString(w, "listen EADDRINUSE\n")
	return err
}

func (c *appCore) Build(_ context.Context, app, version string, env, sealed []string, src io.Reader, out io.Writer) (build.Result, error) {
	b, _ := io.ReadAll(src)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buildEnv = slices.Clone(env)
	for _, v := range sealed {
		opened, err := c.keys.Open(v, app)
		if err != nil {
			return build.Result{}, err
		}
		c.buildEnv = append(c.buildEnv, opened)
	}
	c.builds = append(c.builds, app+":"+version+":"+string(b))
	fmt.Fprintln(out, "npm install")
	if c.failBuild {
		return build.Result{}, errors.New("the build step failed with exit code 1")
	}
	return build.Result{Image: engine.LocalImages + app + ":" + version, TestCommand: c.suggest}, nil
}

func (c *appCore) Wait(_ context.Context, id string) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.containers[id]; ok {
		s.State = "stopped"
		c.containers[id] = s
	}
	return c.testExit, nil
}

func (c *appCore) RemoveImage(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = append(c.removed, name)
	return nil
}

func (c *appCore) SecretKey(context.Context) (secret.PublicKey, error) { return c.keys.Public(), nil }

type fakeProxy struct {
	mu  sync.Mutex
	cfg proxy.Config
}

func (p *fakeProxy) Config(context.Context) (proxy.Config, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg, nil
}

func (p *fakeProxy) Apply(_ context.Context, cfg proxy.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
	return nil
}

func (p *fakeProxy) routes() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.cfg.Routes {
		out = append(out, r.Host+"="+r.Upstream)
	}
	return strings.Join(out, " ")
}

type fakeSource struct{ commit string }

func (f *fakeSource) Resolve(_ context.Context, repo, branch string) (string, error) {
	if repo == "owner/missing" {
		return "", errors.New("GitHub has no public repository owner/missing with a branch main")
	}
	return f.commit, nil
}

func (f *fakeSource) Archive(context.Context, string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("tarball")), nil
}

type appEnv struct {
	b      *browser
	s      *Server
	core   *appCore
	proxy  *fakeProxy
	source *fakeSource

	mu      sync.Mutex
	health  int      // what the health check gets
	checked []string // what it asked
}

func newAppEnv(t *testing.T) *appEnv {
	t.Helper()
	s, h, now := newAuthServer(t)
	e := &appEnv{s: s, core: newAppCore(t), proxy: &fakeProxy{cfg: proxy.Config{Panel: "panel.example.com"}}, source: &fakeSource{commit: strings.Repeat("a", 40)}, health: 200}
	s.Core, s.Proxy, s.Source, s.DataDir = e.core, e.proxy, e.source, t.TempDir()
	s.HealthCheck = func(_ context.Context, url, host string) (int, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.checked = append(e.checked, host+" "+url)
		return e.health, nil
	}
	e.b = &browser{t: t, h: h, ip: "198.51.100.7"}
	e.b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := e.b.do("POST", "/api/2fa/totp/new", nil)
	if code, _ := e.b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, out["secret"].(string), *now)}); code != http.StatusOK {
		t.Fatalf("enrol: %d", code)
	}
	t.Cleanup(s.deploys.wg.Wait)
	return e
}

// settle waits for the app's newest deployment to finish and returns it.
func (e *appEnv) settle(t *testing.T, app string) store.Deployment {
	t.Helper()
	e.s.deploys.wg.Wait()
	list, err := e.s.Store.Deployments(context.Background(), app, 1)
	if err != nil || len(list) == 0 {
		t.Fatalf("no deployment: %v", err)
	}
	return list[0]
}

func TestDeployFromGitHub(t *testing.T) {
	e := newAppEnv(t)
	code, out := e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web", "domain": "Web.Example.com"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	d := e.settle(t, "web")
	if d.State != store.DeployLive || d.Version != strings.Repeat("a", 40) || d.Image != "zelie.local/web:aaaaaaaaaaaa" {
		t.Fatalf("deployment %+v", d)
	}
	if e.core.builds[0] != "web:aaaaaaaaaaaa:tarball" {
		t.Errorf("build %q", e.core.builds[0])
	}
	if got := e.proxy.routes(); got != "web.example.com=10.210.0.1:3000" {
		t.Errorf("routes %q", got)
	}

	// Variables, one secret, go into the next deployment.
	if code, _ := e.b.do("PUT", "/api/apps/web/env", []map[string]any{
		{"name": "NODE_ENV", "value": "production"},
		{"name": "TOKEN", "value": "hunter2", "secret": true},
	}); code != http.StatusNoContent {
		t.Fatalf("set env: %d", code)
	}
	e.source.commit = strings.Repeat("b", 40)
	if code, _ := e.b.do("POST", "/api/apps/web/deployments", nil); code != http.StatusCreated {
		t.Fatalf("deploy: %d", code)
	}
	d2 := e.settle(t, "web")
	if d2.State != store.DeployLive {
		t.Fatalf("second deployment %+v", d2)
	}
	container := fmt.Sprintf("web-%d", d2.ID)
	if env := strings.Join(e.core.env[container], " "); env != "PORT=3000 NODE_ENV=production TOKEN=hunter2" {
		t.Errorf("env %q", env)
	}
	if env := strings.Join(e.core.buildEnv, " "); env != "NODE_ENV=production TOKEN=hunter2" {
		t.Errorf("build env %q", env)
	}
	list, _ := e.core.List(context.Background())
	if len(list) != 1 || list[0].ID != container {
		t.Errorf("old version not removed: %+v", list)
	}
	if got := e.proxy.routes(); got != "web.example.com=10.210.0.2:3000" {
		t.Errorf("routes after the second deployment %q", got)
	}
	if old, _ := e.s.Store.Deployment(context.Background(), "web", d.ID); old.State != store.DeployReplaced {
		t.Errorf("first deployment is %s", old.State)
	}
}

func TestFailedDeploymentKeepsTheOldVersion(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web", "domain": "web.example.com"})
	first := e.settle(t, "web")
	routes := e.proxy.routes()

	e.core.failBuild = true
	e.b.do("POST", "/api/apps/web/deployments", nil)
	if d := e.settle(t, "web"); d.State != store.DeployFailed || !strings.Contains(d.Error, "exit code 1") {
		t.Fatalf("failed build: %+v", d)
	}
	e.core.failBuild, e.core.crash = false, true
	e.b.do("POST", "/api/apps/web/deployments", nil)
	d := e.settle(t, "web")
	if d.State != store.DeployFailed || !strings.Contains(d.Error, "stopped right after starting") {
		t.Fatalf("crashing app: %+v", d)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "web"); live.ID != first.ID {
		t.Errorf("live deployment moved to %d", live.ID)
	}
	if e.proxy.routes() != routes {
		t.Errorf("routes changed: %q", e.proxy.routes())
	}
	if _, err := e.s.containerStatus(context.Background(), fmt.Sprintf("web-%d", d.ID)); err == nil {
		t.Error("the crashed container was left behind")
	}

	// The log says what happened.
	b, _ := readFile(e.s.deployLogPath(d.ID))
	if !strings.Contains(b, "EADDRINUSE") || !strings.Contains(b, "Deployment failed") {
		t.Errorf("log:\n%s", b)
	}
}

func TestSecretsAreNeverSentBack(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "api", "source": "image", "image": "nginx:alpine"})
	e.settle(t, "api")
	e.b.do("PUT", "/api/apps/api/env", []map[string]any{{"name": "TOKEN", "value": "hunter2", "secret": true}})
	_, out := e.b.do("GET", "/api/apps/api", nil)
	env := out["env"].([]any)[0].(map[string]any)
	if env["value"] != "" || env["secret"] != true {
		t.Fatalf("secret sent back: %v", env)
	}
	if code, _ := e.b.do("PUT", "/api/apps/api/env", []map[string]any{{"name": "TOKEN", "secret": true, "keep": true}}); code != http.StatusNoContent {
		t.Fatalf("keep: %d", code)
	}
	vars, _ := e.s.Store.Env(context.Background(), "api")
	if opened, err := e.core.keys.Open(vars[0].Value, "api"); err != nil || opened != "TOKEN=hunter2" {
		t.Fatalf("kept value %q, %v", opened, err)
	}
	if code, _ := e.b.do("PUT", "/api/apps/api/env", []map[string]any{{"name": "OTHER", "secret": true, "keep": true}}); code != http.StatusBadRequest {
		t.Fatalf("kept a value that never existed: %d", code)
	}
	if strings.Contains(fmt.Sprint(vars), "hunter2") {
		t.Error("the database holds the secret in the clear")
	}
}

func TestAppValidation(t *testing.T) {
	e := newAppEnv(t)
	for _, c := range []map[string]any{
		{"id": "Bad Name", "source": "image", "image": "nginx"},
		{"id": "zelie-x", "source": "image", "image": "nginx"},
		{"id": "a", "source": "ftp"},
		{"id": "a", "source": "image"},
		{"id": "a", "source": "image", "image": "zelie.local/other:1"},
		{"id": "a", "source": "image", "image": "Nginx:alpine"},
		{"id": "a", "source": "github", "repo": "not a repo"},
		{"id": "a", "source": "github", "repo": "owner/web", "branch": "../x"},
		{"id": "a", "source": "image", "image": "nginx", "port": 0},
		{"id": "a", "source": "image", "image": "nginx", "domain": "not a domain"},
		{"id": "a", "source": "image", "image": "nginx", "memory_mb": 1},
	} {
		if code, _ := e.b.do("POST", "/api/apps", c); code != http.StatusBadRequest {
			t.Errorf("%v: %d", c, code)
		}
	}
	if code, _ := e.b.do("POST", "/api/apps", map[string]any{"id": "a", "source": "image", "image": "nginx", "domain": "panel.example.com"}); code != http.StatusConflict {
		t.Errorf("the panel's domain: %d", code)
	}
	e.b.do("POST", "/api/apps", map[string]any{"id": "a", "source": "image", "image": "nginx", "domain": "a.example.com"})
	if code, _ := e.b.do("POST", "/api/apps", map[string]any{"id": "b", "source": "image", "image": "nginx", "domain": "a.example.com"}); code != http.StatusConflict {
		t.Errorf("a taken domain: %d", code)
	}
	if code, _ := e.b.do("PATCH", "/api/apps/a", map[string]any{"source": "github"}); code != http.StatusBadRequest {
		t.Errorf("changed the source: %d", code)
	}
}

func TestDeleteApp(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx", "domain": "web.example.com"})
	e.settle(t, "web")
	if code, _ := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if list, _ := e.core.List(context.Background()); len(list) != 0 {
		t.Errorf("containers left: %+v", list)
	}
	if e.proxy.routes() != "" {
		t.Errorf("routes left: %q", e.proxy.routes())
	}
	if code, _ := e.b.do("GET", "/api/apps/web", nil); code != http.StatusNotFound {
		t.Errorf("app still there: %d", code)
	}
}

func TestDomainChangeMovesTheRoute(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx", "domain": "old.example.com"})
	e.settle(t, "web")
	if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"domain": "new.example.com"}); code != http.StatusNoContent {
		t.Fatalf("patch: %d", code)
	}
	if got := e.proxy.routes(); got != "new.example.com=10.210.0.1:80" {
		t.Errorf("routes %q", got)
	}
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}
