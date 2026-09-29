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
	"github.com/Caria-Core/zelie/internal/core"
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
	keep       []string // what the last sweep was told
	caches     []string // apps whose build cache went
	external   map[string]core.ExternalListener
	extSets    []string     // "app port sealed?" of each SetExternal
	takenPorts map[int]bool // ports something else on the server holds
	version    string       // the version the core runs
	updatedTo  string
	suggest    string   // the test command builds find
	buildEnv   []string // the variables the last build got, opened
	args       map[string][]string
	cpuUsec    int64
	testExit   int
	tests      []engine.Spec
	failBuild  bool
	crash      bool              // new containers stop right away
	crashImage string            // containers of this image stop right away
	digests    map[string]string // what each registry tag points at now
	next       byte

	installs    []core.InstallRequest
	installExit int  // what the install containers exit with
	installHang bool // install containers never exit

	// Game servers: the specs their containers were made from (those with
	// a console), what they printed, what was written to them and how they
	// take being stopped.
	games       map[string]engine.Spec
	gameLog     map[string]string
	consoles    map[string][]string
	signals     map[string][]string
	prepares    []preparedVolume
	prepareNote []string
	prepareErr  error
	stdinErr    error
	stubborn    bool // ignores the stop command and SIGTERM; only a kill ends it
	// A volume was prepared while a container had it running.
	preparedBusy bool

	links map[string][]engine.Link

	forwards  map[string][]engine.Forward
	cleared   []string // apps whose forwards were cleared
	usedPorts []int    // what the machine holds

	volumes     map[string]bool
	sizes       map[string]int64
	mounts      map[string][]engine.VolumeMount // by container
	overlapping bool                            // two containers had the same volume running

	bk  coreBackups
	off coreOffsite
	// Containers another request removes first: removing them again finds
	// nothing.
	vanishing map[string]bool
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

func (c *appCore) Run(ctx context.Context, s engine.Spec) error { return c.runApp(s, nil) }

func (c *appCore) RunApp(ctx context.Context, s engine.Spec, sealed []string, linked ...core.LinkedVar) (string, error) {
	if err := c.runApp(s, sealed, linked...); err != nil {
		return "", err
	}
	return c.Pin(ctx, s.Image)
}

// Pin pins tags the test gave a digest; others come back as they are, as
// when the core cannot pin.
func (c *appCore) Pin(_ context.Context, image string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d, ok := c.digests[image]; ok {
		return image + "@" + d, nil
	}
	return image, nil
}

func (c *appCore) runApp(s engine.Spec, sealed []string, linked ...core.LinkedVar) error {
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
	// As the core does: only a linked app gets another app's secret.
	for _, v := range linked {
		if !slices.ContainsFunc(c.links[s.App], func(l engine.Link) bool { return l.To == v.From }) {
			return &core.Error{Status: http.StatusBadRequest, Message: s.App + " is not linked to " + v.From}
		}
		opened, err := c.keys.Open(v.Sealed, v.From)
		if err != nil {
			return err
		}
		_, value, _ := strings.Cut(opened, "=")
		env = append(env, v.Name+"="+strings.ReplaceAll(v.Template, "{secret}", value))
	}
	for _, v := range s.Volumes {
		if !c.volumes[v.Name] {
			return &core.Error{Status: http.StatusNotFound, Message: "volume not found"}
		}
		for id, other := range c.mounts {
			if c.containers[id].State == "running" && slices.ContainsFunc(other, func(o engine.VolumeMount) bool { return o.Name == v.Name }) {
				c.overlapping = true
			}
		}
	}
	if c.mounts == nil {
		c.mounts = map[string][]engine.VolumeMount{}
	}
	c.mounts[s.ID] = s.Volumes
	c.next++
	state := "running"
	if c.crash || (c.crashImage != "" && s.Image == c.crashImage) {
		state = "stopped"
	}
	if strings.HasSuffix(s.ID, "-test") {
		c.tests = append(c.tests, s)
		c.tests[len(c.tests)-1].Env = env
	}
	if s.Stdin {
		if c.games == nil {
			c.games = map[string]engine.Spec{}
		}
		opened := s
		opened.Env = env
		c.games[s.ID] = opened
	}
	c.containers[s.ID] = engine.Status{ID: s.ID, App: s.App, Image: s.Image, State: state, IP: netip.AddrFrom4([4]byte{10, 210, 0, c.next})}
	c.env[s.ID] = env
	if c.args == nil {
		c.args = map[string][]string{}
	}
	c.args[s.ID] = s.Args
	return nil
}

type preparedVolume struct {
	Volume string
	Req    core.PrepareRequest
}

func (c *appCore) PrepareVolume(_ context.Context, name string, req core.PrepareRequest) (core.PrepareResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, st := range c.containers {
		if st.State == "running" && slices.ContainsFunc(c.mounts[id], func(v engine.VolumeMount) bool { return v.Name == name }) {
			c.preparedBusy = true
		}
	}
	if c.prepareErr != nil {
		return core.PrepareResponse{}, c.prepareErr
	}
	c.prepares = append(c.prepares, preparedVolume{name, req})
	return core.PrepareResponse{Notes: c.prepareNote}, nil
}

func (c *appCore) WriteStdin(_ context.Context, id string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdinErr != nil {
		return c.stdinErr
	}
	if _, ok := c.containers[id]; !ok {
		return &core.Error{Status: http.StatusNotFound, Message: "container not found"}
	}
	if c.consoles == nil {
		c.consoles = map[string][]string{}
	}
	c.consoles[id] = append(c.consoles[id], string(data))
	if string(data) == "stop\n" && !c.stubborn {
		c.exitLocked(id)
	}
	return nil
}

func (c *appCore) Signal(_ context.Context, id, signal string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.containers[id]; !ok {
		return &core.Error{Status: http.StatusNotFound, Message: "container not found"}
	}
	if c.signals == nil {
		c.signals = map[string][]string{}
	}
	c.signals[id] = append(c.signals[id], signal)
	if signal == "SIGKILL" || !c.stubborn {
		c.exitLocked(id)
	}
	return nil
}

func (c *appCore) exitLocked(id string) {
	if st, ok := c.containers[id]; ok {
		st.State = "stopped"
		c.containers[id] = st
	}
}

// emit prints to a game container's console.
func (c *appCore) emit(id, text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gameLog == nil {
		c.gameLog = map[string]string{}
	}
	c.gameLog[id] += text
}

func (c *appCore) SetLinks(_ context.Context, app string, links []engine.Link) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.links == nil {
		c.links = map[string][]engine.Link{}
	}
	c.links[app] = links
	return nil
}

func (c *appCore) Stop(_ context.Context, id string, _ int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.containers[id]; ok {
		st.State = "stopped"
		c.containers[id] = st
	}
	return nil
}

func (c *appCore) Remove(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.containers, id)
	if c.vanishing[id] {
		// Someone else removed it between the list and now.
		return &core.Error{Status: http.StatusNotFound, Message: "container not found"}
	}
	return nil
}

func (c *appCore) Logs(ctx context.Context, id string, follow bool, _ int64, w io.Writer) error {
	c.mu.Lock()
	_, game := c.games[id]
	c.mu.Unlock()
	if game {
		sent := 0
		for {
			c.mu.Lock()
			text := c.gameLog[id]
			c.mu.Unlock()
			if len(text) > sent {
				if _, err := io.WriteString(w, text[sent:]); err != nil {
					return err
				}
				sent = len(text)
			}
			if !follow {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Millisecond):
			}
		}
	}
	if strings.Contains(id, "-install-") {
		_, err := io.WriteString(w, "downloading server.jar\n")
		if follow {
			<-ctx.Done()
		}
		return err
	}
	_, err := io.WriteString(w, "listen EADDRINUSE\n")
	return err
}

func (c *appCore) RunInstall(ctx context.Context, req core.InstallRequest) (string, error) {
	c.mu.Lock()
	c.installs = append(c.installs, req)
	c.mu.Unlock()
	if err := c.runApp(engine.Spec{ID: req.ID, App: req.App, Image: req.Image, Env: req.Env,
		Volumes: []engine.VolumeMount{{Name: req.Volume, Target: core.InstallVolumePath}}}, nil); err != nil {
		return "", err
	}
	return c.Pin(ctx, req.Image)
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
	return build.Result{Image: engine.LocalImages + app + ":" + version, TestCommand: c.suggest,
		Builder: "railpack", BuildCommand: "npm run build", StartCommand: "node index.js"}, nil
}

func (c *appCore) Wait(ctx context.Context, id string) (int, error) {
	c.mu.Lock()
	_, game := c.games[id]
	hang := c.installHang && strings.Contains(id, "-install-")
	c.mu.Unlock()
	if game {
		// A server's process ends when it is stopped or exits.
		for {
			c.mu.Lock()
			st, ok := c.containers[id]
			c.mu.Unlock()
			if !ok {
				return 0, &core.Error{Status: http.StatusNotFound, Message: "container not found"}
			}
			if st.State != "running" {
				return 0, nil
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Millisecond):
			}
		}
	}
	if hang {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.containers[id]; ok {
		s.State = "stopped"
		c.containers[id] = s
	}
	if strings.Contains(id, "-install-") {
		return c.installExit, nil
	}
	return c.testExit, nil
}

func (c *appCore) RemoveImage(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removed = append(c.removed, name)
	return nil
}

func (c *appCore) SweepImages(_ context.Context, keep []string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keep = keep
	return nil, nil
}

func (c *appCore) SetExternal(_ context.Context, app, engine, container string, port, target int, sealed string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sealed != "" {
		if _, err := c.keys.Open(sealed, app); err != nil {
			return err
		}
	}
	c.extSets = append(c.extSets, fmt.Sprintf("%s %d %v", app, port, sealed != ""))
	if c.takenPorts[port] {
		return &core.Error{Status: http.StatusConflict, Code: "external.port_taken", Message: "taken"}
	}
	if c.external == nil {
		c.external = map[string]core.ExternalListener{}
	}
	c.external[app] = core.ExternalListener{App: app, Port: port, Target: target}
	return nil
}

func (c *appCore) RemoveExternal(_ context.Context, app, engine, container string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.external, app)
	return nil
}

func (c *appCore) SyncExternal(_ context.Context, list []core.ExternalListener) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.external = map[string]core.ExternalListener{}
	for _, l := range list {
		c.external[l.App] = l
	}
	return nil
}

func (c *appCore) SetForwards(_ context.Context, app string, forwards []engine.Forward) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.forwards == nil {
		c.forwards = map[string][]engine.Forward{}
	}
	c.forwards[app] = forwards
	return nil
}

func (c *appCore) ClearForwards(_ context.Context, app string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.forwards, app)
	c.cleared = append(c.cleared, app)
	return nil
}

func (c *appCore) UsedPorts(context.Context) ([]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.usedPorts), nil
}

func (c *appCore) UpdateStatus(context.Context) (core.UpdateStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return core.UpdateStatus{Version: c.version}, nil
}

func (c *appCore) Update(_ context.Context, v string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updatedTo = v
	return nil
}

func (c *appCore) RemoveBuildCache(_ context.Context, app string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.caches = append(c.caches, app)
	return nil
}

func (c *appCore) Usage(_ context.Context, id string) (engine.Usage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.containers[id]; !ok || st.State != "running" {
		return engine.Usage{}, errors.New("not running")
	}
	c.cpuUsec += 250_000
	return engine.Usage{MemoryBytes: 84 << 20, CPUUsec: c.cpuUsec}, nil
}

func (c *appCore) Host(context.Context) (engine.Host, error) {
	return engine.Host{CPUs: 2, MemoryBytes: 3 << 30, DiskBytes: 100 << 30, DiskFreeBytes: 60 << 30}, nil
}

func (c *appCore) CreateVolume(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.volumes == nil {
		c.volumes = map[string]bool{}
	}
	c.volumes[name] = true
	return nil
}

func (c *appCore) RemoveVolume(_ context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.containers {
		if slices.ContainsFunc(c.mounts[id], func(v engine.VolumeMount) bool { return v.Name == name }) {
			return &core.Error{Status: http.StatusConflict, Message: "a container still uses this volume"}
		}
	}
	if !c.volumes[name] {
		return &core.Error{Status: http.StatusNotFound, Message: "volume not found"}
	}
	delete(c.volumes, name)
	return nil
}

func (c *appCore) VolumeSizes(context.Context) (map[string]int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int64{}
	for name := range c.volumes {
		out[name] = c.sizes[name]
	}
	return out, nil
}

func (c *appCore) SecretKey(context.Context) (secret.PublicKey, error) { return c.keys.Public(), nil }

type fakeProxy struct {
	mu    sync.Mutex
	cfg   proxy.Config
	stats proxy.Stats
}

func (p *fakeProxy) Stats(context.Context) (proxy.Stats, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stats, nil
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
	s.PortCheck = func(context.Context, netip.Addr, int) bool { return true }
	e.b = &browser{t: t, h: h, ip: "198.51.100.7"}
	e.b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := e.b.do("POST", "/api/2fa/totp/new", nil)
	if code, _ := e.b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, out["secret"].(string), *now)}); code != http.StatusOK {
		t.Fatalf("enrol: %d", code)
	}
	t.Cleanup(s.deploys.wg.Wait)
	// Runs first: a restore starts deployments.
	t.Cleanup(s.jobs.Wait)
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
	if d := e.settle(t, "web"); d.State != store.DeployFailed || d.Error == nil || !strings.Contains(d.Error.Text, "exit code 1") {
		t.Fatalf("failed build: %+v", d)
	}
	e.core.failBuild, e.core.crash = false, true
	e.b.do("POST", "/api/apps/web/deployments", nil)
	d := e.settle(t, "web")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "deploy.stopped_at_start" {
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

// A deployment cancelled by the delete may remove its old container at the
// same time. The delete goes on.
func TestDeleteWhileAContainerGoes(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	d := e.settle(t, "web")
	e.core.vanishing = map[string]bool{fmt.Sprintf("web-%d", d.ID): true}
	if code, out := e.b.do("DELETE", "/api/apps/web", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, _ := e.b.do("GET", "/api/apps/web", nil); code != http.StatusNotFound {
		t.Errorf("app still there: %d", code)
	}
}
