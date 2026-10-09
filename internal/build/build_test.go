package build

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/engine"
)

// fakeEngine plays the containers: each step writes a line to its log, and
// the unpack and build steps leave behind what the real ones would.
type fakeEngine struct {
	paths      engine.Paths
	dockerfile bool   // the source has a Dockerfile
	fail       string // step that exits with an error
	specs      []engine.Spec
	imported   string
	secrets    map[string]string // what the build step found in /secrets
	pruned     []engine.Spec     // cache trims, kept apart from the build's steps
}

func mount(s engine.Spec, target string) string {
	for _, m := range s.Mounts {
		if m.Target == target {
			return m.Source
		}
	}
	return ""
}

func (f *fakeEngine) Run(_ context.Context, s engine.Spec) error {
	step := strings.TrimPrefix(s.ID, "zelie-build-")
	os.MkdirAll(f.paths.Logs, 0o700)
	os.WriteFile(engine.LogPathFor(f.paths, s.ID), []byte(step+" output\n"), 0o600)
	if step == "prune" {
		f.pruned = append(f.pruned, s)
		return nil
	}
	f.specs = append(f.specs, s)
	switch step {
	case "unpack":
		if f.dockerfile {
			os.WriteFile(filepath.Join(mount(s, "/src"), "Dockerfile"), []byte("FROM scratch\n"), 0o644)
		}
	case "plan":
		os.WriteFile(filepath.Join(mount(s, "/plan"), "railpack-plan.json"),
			[]byte(`{"steps":[{"name":"install","commands":[{"cmd":"npm ci"}]},{"name":"build","commands":[{"cmd":"npm run build"},{"path":"/x"}]}],"deploy":{"startCommand":"node index.js"}}`), 0o644)
	case "build":
		os.WriteFile(filepath.Join(mount(s, "/out"), "image.tar"), []byte("oci"), 0o644)
		if dir := mount(s, "/secrets"); dir != "" {
			f.secrets = map[string]string{}
			entries, _ := os.ReadDir(dir)
			for _, e := range entries {
				b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				f.secrets[e.Name()] = string(b)
			}
		}
	}
	return nil
}

func (f *fakeEngine) Wait(_ context.Context, id string) (uint32, error) {
	if "zelie-build-"+f.fail == id {
		return 1, nil
	}
	return 0, nil
}

func (f *fakeEngine) Remove(context.Context, string) error { return nil }

func (f *fakeEngine) ImportImage(_ context.Context, r io.Reader, name string) error {
	b, _ := io.ReadAll(r)
	if string(b) != "oci" {
		return errors.New("wrong archive")
	}
	f.imported = name
	return nil
}

func newTestBuilder(t *testing.T, f *fakeEngine) *Builder {
	dir := t.TempDir()
	f.paths = engine.Paths{Logs: filepath.Join(dir, "logs"), Bin: filepath.Join(dir, "bin")}
	b := New(f, f.paths, filepath.Join(dir, "build"))
	b.chown = func(string, int, int) error { return nil }
	return b
}

func request() Request {
	return Request{App: "web", Version: "abc123", Source: strings.NewReader("source")}
}

func TestBuildWithRailpack(t *testing.T) {
	f := &fakeEngine{}
	b := newTestBuilder(t, f)
	var out bytes.Buffer
	res, err := b.Build(context.Background(), request(), &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if res.Image != "zelie.local/web:abc123" || f.imported != res.Image {
		t.Errorf("image %q, imported %q", res.Image, f.imported)
	}
	if res.Builder != "railpack" || res.BuildCommand != "npm run build" || res.StartCommand != "node index.js" {
		t.Errorf("detected %+v", res)
	}
	var steps []string
	for _, s := range f.specs {
		steps = append(steps, strings.TrimPrefix(s.ID, "zelie-build-"))
		if !s.Builder {
			t.Errorf("%s does not run as the builder", s.ID)
		}
	}
	if strings.Join(steps, ",") != "unpack,plan,build" {
		t.Fatalf("steps %v", steps)
	}
	for _, line := range []string{"unpack output", "plan output", "build output", "Railpack"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output lacks %q:\n%s", line, out.String())
		}
	}

	unpack, plan, build := f.specs[0], f.specs[1], f.specs[2]
	if unpack.Network != "" || len(unpack.Mounts) != 2 || unpack.Nesting {
		t.Errorf("unpack must have no network and nothing but its archive and output: %+v", unpack)
	}
	if plan.Nesting || mount(plan, "/cache") != "" {
		t.Errorf("plan must not nest or see the build cache: %+v", plan)
	}
	if !build.Nesting {
		t.Error("build step cannot nest")
	}
	for _, s := range []engine.Spec{plan, build} {
		for _, m := range s.Mounts {
			if m.Target == "/src" && !m.ReadOnly {
				t.Errorf("%s can write to the source", s.ID)
			}
		}
	}
	if !strings.Contains(strings.Join(build.Args, " "), "build-arg:cache-key=web") {
		t.Errorf("package caches are not kept per app: %v", build.Args)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "jobs", "web")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the build's files were left behind")
	}
}

func TestBuildWithDockerfile(t *testing.T) {
	f := &fakeEngine{dockerfile: true}
	b := newTestBuilder(t, f)
	if _, err := b.Build(context.Background(), request(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 2 || !strings.Contains(strings.Join(f.specs[1].Args, " "), "dockerfile.v0") {
		t.Fatalf("steps: %+v", f.specs)
	}
}

func TestDockerfileSymlinkIsNotFollowed(t *testing.T) {
	f := &fakeEngine{}
	b := newTestBuilder(t, f)
	// The unpack step of a hostile archive leaves a symlink to a host file.
	target := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(target, []byte("FROM scratch\n"), 0o600)
	b.Engine = &symlinkEngine{fakeEngine: f, target: target}
	if _, err := b.Build(context.Background(), request(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(f.specs) != 3 {
		t.Fatalf("a symlinked Dockerfile was used: %d steps", len(f.specs))
	}
}

type symlinkEngine struct {
	*fakeEngine
	target string
}

func (e *symlinkEngine) Run(ctx context.Context, s engine.Spec) error {
	err := e.fakeEngine.Run(ctx, s)
	if s.ID == "zelie-build-unpack" {
		os.Symlink(e.target, filepath.Join(mount(s, "/src"), "Dockerfile"))
	}
	return err
}

func TestFailedStepStopsTheBuild(t *testing.T) {
	f := &fakeEngine{fail: "plan"}
	b := newTestBuilder(t, f)
	_, err := b.Build(context.Background(), request(), io.Discard)
	var se *StepError
	if !errors.As(err, &se) || se.Step != "plan" {
		t.Fatalf("err = %v", err)
	}
	if len(f.specs) != 2 || f.imported != "" {
		t.Error("the build went on after a failed step")
	}
}

func TestBadRequests(t *testing.T) {
	b := newTestBuilder(t, &fakeEngine{})
	for _, r := range []Request{
		{App: "../x", Version: "1", Source: strings.NewReader("")},
		{App: "web", Version: "a b", Source: strings.NewReader("")},
		{App: "web", Version: "", Source: strings.NewReader("")},
	} {
		if _, err := b.Build(context.Background(), r, io.Discard); err == nil {
			t.Errorf("accepted %+v", r)
		}
	}
}

func TestSourceSizeLimit(t *testing.T) {
	f := &fakeEngine{}
	b := newTestBuilder(t, f)
	r := request()
	r.Source = io.LimitReader(zeros{}, MaxSource+1)
	if _, err := b.Build(context.Background(), r, io.Discard); !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if len(f.specs) != 0 {
		t.Error("a step ran for a source that was too large")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestSuggestTest(t *testing.T) {
	for _, c := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"package.json": `{"scripts":{"test":"vitest run"}}`}, "npm test"},
		{map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "pnpm-lock.yaml": ""}, "pnpm test"},
		{map[string]string{"package.json": `{"scripts":{"test":"jest"}}`, "yarn.lock": ""}, "yarn test"},
		{map[string]string{"package.json": `{"scripts":{"test":"echo \"Error: no test specified\" && exit 1"}}`}, ""},
		{map[string]string{"package.json": `{"scripts":{"start":"node ."}}`}, ""},
		{map[string]string{"package.json": `not json`}, ""},
		{map[string]string{"requirements.txt": "flask"}, ""},
	} {
		dir := t.TempDir()
		for name, body := range c.files {
			os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
		}
		if got := suggestTest(dir); got != c.want {
			t.Errorf("%v: %q, want %q", c.files, got, c.want)
		}
	}

	// A package.json that is a symlink could point anywhere on the host.
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	os.WriteFile(target, []byte(`{"scripts":{"test":"jest"}}`), 0o644)
	os.Symlink(target, filepath.Join(dir, "package.json"))
	if got := suggestTest(dir); got != "" {
		t.Errorf("followed a symlink: %q", got)
	}
}

func TestVariablesReachTheBuild(t *testing.T) {
	vars := []Var{{Name: "NEXT_PUBLIC_URL", Value: "https://example.com"}, {Name: "NPM_TOKEN", Value: "npm_s3cret", Secret: true}, {Name: "PATH", Value: "/custom"}}

	f := &fakeEngine{}
	b := newTestBuilder(t, f)
	req := request()
	req.Vars = vars
	var out bytes.Buffer
	if _, err := b.Build(context.Background(), req, &out); err != nil {
		t.Fatal(err)
	}
	plan, build := f.specs[1], f.specs[2]
	for _, s := range f.specs {
		if args := strings.Join(s.Args, " "); strings.Contains(args, "npm_s3cret") || strings.Contains(args, "example.com") && s.ID != "zelie-build-plan" {
			t.Errorf("%s has a value on its command line: %s", s.ID, args)
		}
	}
	if args := strings.Join(plan.Args, " "); !strings.Contains(args, "--env NEXT_PUBLIC_URL --env NPM_TOKEN") || strings.Contains(args, "PATH") {
		t.Errorf("plan args %s", args)
	}
	if !slices.Contains(plan.Env, "NPM_TOKEN=npm_s3cret") || slices.Contains(plan.Env, "PATH=/custom") {
		t.Errorf("plan env %v", plan.Env)
	}
	args := strings.Join(build.Args, " ")
	for _, want := range []string{"--secret id=NPM_TOKEN,src=/secrets/NPM_TOKEN", "--secret id=PATH,src=/secrets/PATH", "build-arg:secrets-hash="} {
		if !strings.Contains(args, want) {
			t.Errorf("build args lack %q: %s", want, args)
		}
	}
	if f.secrets["NPM_TOKEN"] != "npm_s3cret" || len(f.secrets) != 3 {
		t.Errorf("secret files %v", f.secrets)
	}
	if strings.Contains(out.String(), "npm_s3cret") || !strings.Contains(out.String(), "NPM_TOKEN") {
		t.Errorf("output:\n%s", out.String())
	}

	// A Dockerfile gets plain variables as build arguments too, but never
	// a secret one, which would be saved in the image.
	f = &fakeEngine{dockerfile: true}
	b = newTestBuilder(t, f)
	req = request()
	req.Vars = vars
	b.Build(context.Background(), req, io.Discard)
	args = strings.Join(f.specs[1].Args, " ")
	if !strings.Contains(args, "build-arg:NEXT_PUBLIC_URL=https://example.com") || strings.Contains(args, "npm_s3cret") {
		t.Errorf("dockerfile build args: %s", args)
	}

	if varsHash(vars) == varsHash([]Var{vars[0], {Name: "NPM_TOKEN", Value: "other", Secret: true}, vars[2]}) {
		t.Error("the hash does not change with a value")
	}
	req = request()
	req.Vars = []Var{{Name: "../x", Value: "y"}}
	if _, err := newTestBuilder(t, &fakeEngine{}).Build(context.Background(), req, io.Discard); err == nil {
		t.Error("a variable name with a path in it was accepted")
	}
}

func TestPruneAfterEveryBuild(t *testing.T) {
	for _, fail := range []string{"", "build"} {
		f := &fakeEngine{fail: fail}
		b := newTestBuilder(t, f)
		var out bytes.Buffer
		_, err := b.Build(context.Background(), request(), &out)
		if (err != nil) != (fail != "") {
			t.Fatalf("fail %q: %v", fail, err)
		}
		if len(f.pruned) != 1 {
			t.Fatalf("fail %q: pruned %d times", fail, len(f.pruned))
		}
		p := f.pruned[0]
		args := strings.Join(p.Args, " ")
		if !strings.Contains(args, "prune --all --keep-storage ") || p.Network != "" || !p.Nesting ||
			mount(p, "/cache") != b.buildkitDir("web") {
			t.Errorf("prune step %+v", p)
		}
		if strings.Contains(out.String(), "prune output") {
			t.Errorf("the prune's output is in the build log:\n%s", out.String())
		}
	}
}

func TestCacheLimit(t *testing.T) {
	limit, err := cacheLimit(t.TempDir())
	if err != nil || limit <= 0 || limit > maxCache {
		t.Fatalf("limit %d, %v", limit, err)
	}
}

// A Dockerfile chooses its own cache mounts and may bring its own
// frontend, so apps must not meet in one BuildKit state.
func TestAppsDoNotShareABuildCache(t *testing.T) {
	f := &fakeEngine{dockerfile: true}
	b := newTestBuilder(t, f)
	// What an older version left, shared by every app.
	old := filepath.Join(b.cacheDir("buildkit"), "planted")
	os.MkdirAll(filepath.Dir(old), 0o755)
	os.WriteFile(old, []byte("x"), 0o644)

	var caches []string
	trims := 0
	for _, app := range []string{"web", "api"} {
		req := request()
		req.App = app
		if _, err := b.Build(context.Background(), req, io.Discard); err != nil {
			t.Fatal(err)
		}
		build := f.specs[len(f.specs)-1]
		caches = append(caches, mount(build, "/cache"))
		// The app's own cache is the first to be trimmed.
		if want, first := b.buildkitDir(app), mount(f.pruned[trims], "/cache"); mount(build, "/cache") != want || first != want {
			t.Errorf("%s builds in %s and trims %s, want %s", app, mount(build, "/cache"), first, want)
		}
		trims = len(f.pruned)
	}
	if caches[0] == caches[1] {
		t.Fatalf("both apps build in %s", caches[0])
	}
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Error("the cache shared by every app was kept")
	}

	// Taking an app away takes its cache with it.
	if err := b.RemoveCache("web"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(caches[0]); !errors.Is(err, os.ErrNotExist) {
		t.Error("the cache of a removed app was kept")
	}
	if _, err := os.Stat(caches[1]); err != nil {
		t.Errorf("another app's cache went too: %v", err)
	}
}

func TestCacheLimitIsSharedBetweenApps(t *testing.T) {
	b := newTestBuilder(t, &fakeEngine{})
	for _, app := range []string{"a", "b", "c", "d"} {
		os.MkdirAll(b.buildkitDir(app), 0o755)
	}
	whole, err := cacheLimit(b.cacheDir("apps"))
	if err != nil {
		t.Fatal(err)
	}
	share, err := b.cacheShare("a")
	if err != nil || share != whole/4 {
		t.Errorf("share %d of %d, %v", share, whole, err)
	}
}

// Each build only knows the number of apps it sees. Caches trimmed when
// there were fewer apps must not stay at their bigger parts.
func TestOtherCachesFollowTheirShrinkingShare(t *testing.T) {
	f := &fakeEngine{}
	b := newTestBuilder(t, f)
	os.MkdirAll(b.cacheDir("apps"), 0o755)
	whole, err := cacheLimit(b.cacheDir("apps"))
	if err != nil {
		t.Fatal(err)
	}
	recorded := map[string]int64{"a": whole / 2, "b": whole / 3, "c": whole / 4}
	for app, part := range recorded {
		os.MkdirAll(b.buildkitDir(app), 0o755)
		os.WriteFile(b.shareFile(app), []byte(strconv.FormatInt(part, 10)), 0o600)
	}
	// "e" has a cache that was never trimmed, and "f" has nothing but the
	// tools Railpack downloaded.
	os.MkdirAll(b.buildkitDir("e"), 0o755)
	os.MkdirAll(b.appCache("f"), 0o755)

	trimmed := func() (names []string) {
		for _, p := range f.pruned {
			names = append(names, filepath.Base(filepath.Dir(mount(p, "/cache"))))
		}
		f.pruned = nil
		return names
	}
	req := request()
	req.App = "d"
	if _, err := b.Build(context.Background(), req, io.Discard); err != nil {
		t.Fatal(err)
	}
	// Six apps share the limit now. The build's own cache is trimmed first,
	// then two more, the ones furthest above their part.
	share := whole / 6
	if got, want := trimmed(), []string{"d", "e", "a"}; !slices.Equal(got, want) {
		t.Fatalf("trimmed %v, want %v", got, want)
	}
	for _, app := range []string{"d", "e", "a"} {
		if text, _ := os.ReadFile(b.shareFile(app)); string(text) != strconv.FormatInt(share, 10) {
			t.Errorf("%s was trimmed to %q, want %d", app, text, share)
		}
	}
	for app, part := range map[string]int64{"b": whole / 3, "c": whole / 4} {
		if text, _ := os.ReadFile(b.shareFile(app)); string(text) != strconv.FormatInt(part, 10) {
			t.Errorf("%s was trimmed to %q, which it should not have been", app, text)
		}
	}

	// The next builds finish the rest, then have nothing more to do.
	for range 2 {
		if _, err := b.Build(context.Background(), req, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := trimmed(), []string{"d", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Errorf("trimmed %v, want %v", got, want)
	}
}

func TestFailedTrimIsNotRecorded(t *testing.T) {
	f := &fakeEngine{fail: "prune"}
	b := newTestBuilder(t, f)
	os.MkdirAll(b.buildkitDir("a"), 0o755)
	var out bytes.Buffer
	if _, err := b.Build(context.Background(), request(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Could not trim the build cache") {
		t.Errorf("log:\n%s", out.String())
	}
	if len(f.pruned) != 1 {
		t.Errorf("%d trims after a failed one", len(f.pruned))
	}
	if _, err := os.Stat(b.shareFile("web")); !errors.Is(err, os.ErrNotExist) {
		t.Error("a share was recorded for a cache that was not trimmed")
	}
}
