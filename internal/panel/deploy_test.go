package panel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

// slowSource holds a build at its first step until it is released or the
// deployment is cancelled.
type slowSource struct {
	*fakeSource
	entered chan struct{}
	release chan struct{}
}

func newSlowSource(f *fakeSource) *slowSource {
	return &slowSource{fakeSource: f, entered: make(chan struct{}, 8), release: make(chan struct{})}
}

func (f *slowSource) Resolve(ctx context.Context, repo, branch string) (string, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
		return f.fakeSource.Resolve(ctx, repo, branch)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (f *slowSource) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-f.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the build never started")
	}
}

func TestRebuildingACommitLeavesTheLiveImageAlone(t *testing.T) {
	e := newAppEnv(t)
	e.core.suggest = "npm test"
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	first := e.settle(t, "web")
	if first.State != store.DeployLive {
		t.Fatalf("first %+v", first)
	}

	// The same commit again, now with tests that fail.
	e.core.testExit = 1
	e.b.do("POST", "/api/apps/web/deployments", nil)
	second := e.settle(t, "web")
	if second.State != store.DeployFailed || second.Version != first.Version {
		t.Fatalf("second %+v", second)
	}
	if second.Image == first.Image {
		t.Fatalf("the rebuild took the name of the live image, %s", first.Image)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "web"); live.ID != first.ID || live.Image != first.Image {
		t.Errorf("live deployment %+v", live)
	}

	// What the failed build made goes at once; what runs stays.
	e.core.mu.Lock()
	removed := slices.Clone(e.core.removed)
	e.core.mu.Unlock()
	if !slices.Contains(removed, second.Image) || slices.Contains(removed, first.Image) {
		t.Errorf("removed images %v", removed)
	}
	if d, _ := e.s.Store.Deployment(context.Background(), "web", second.ID); !d.Pruned {
		t.Error("the failed build's image is not marked as removed")
	}
}

func TestRestartDoesNotUndoADeployment(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	first := e.settle(t, "web")

	// A restart asked for while a deployment builds brings back the new
	// version, not the one that was live when it was asked for.
	slow := newSlowSource(e.source)
	e.s.Source = slow
	e.source.commit = strings.Repeat("b", 40)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	slow.waitEntered(t)
	if code, _ := e.b.do("POST", "/api/apps/web/restart", nil); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	close(slow.release)
	e.s.deploys.wg.Wait()
	list, _ := e.s.Store.Deployments(context.Background(), "web", 3)
	restart, deploy := list[0], list[1]
	if deploy.State != store.DeployReplaced || deploy.Image == first.Image {
		t.Fatalf("deployment %+v", deploy)
	}
	if restart.State != store.DeployLive || restart.Image != deploy.Image || restart.Version != deploy.Version {
		t.Fatalf("the restart put back %s, want %s: %+v", restart.Image, deploy.Image, restart)
	}

	// The same when the deployment has not started yet: the restart is not
	// a reason to skip it. Both wait for the app and either may go first;
	// either way the new version is what ends up live.
	e.s.Source = e.source
	e.source.commit = strings.Repeat("c", 40)
	unlock := e.s.deploys.lock("web")
	e.b.do("POST", "/api/apps/web/deployments", nil)
	e.b.do("POST", "/api/apps/web/restart", nil)
	unlock()
	e.s.deploys.wg.Wait()
	list, _ = e.s.Store.Deployments(context.Background(), "web", 2)
	restart, deploy = list[0], list[1]
	if deploy.State == store.DeploySkipped || deploy.Version != strings.Repeat("c", 40) {
		t.Fatalf("the deployment was skipped for a restart: %+v", deploy)
	}
	if restart.State != store.DeployLive && restart.State != store.DeployReplaced {
		t.Fatalf("restart %+v", restart)
	}
	if live, _ := e.s.Store.LiveDeployment(context.Background(), "web"); live.Image != deploy.Image {
		t.Errorf("live is %s, want %s: %+v", live.Image, deploy.Image, live)
	}
}

func TestStoppedAppIsNotStartedByAQueuedDeployment(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx:alpine"})
	live := e.settle(t, "web")
	ctx := context.Background()
	container := fmt.Sprintf("web-%d", live.ID)

	// Something holds the app, as a backup does. A restart waits behind
	// it, and the user stops the app for good in the meantime.
	unlock := e.s.deploys.lock("web")
	if code, _ := e.b.do("POST", "/api/apps/web/restart", nil); code != http.StatusCreated {
		t.Fatalf("restart: %d", code)
	}
	e.s.Store.SetStopped(ctx, "web", true)
	e.core.Stop(ctx, container, 0)
	unlock()
	d := e.settle(t, "web")
	if d.State != store.DeploySkipped {
		t.Fatalf("restart of a stopped app: %+v", d)
	}
	list, _ := e.core.List(ctx)
	if len(list) != 1 || list[0].ID != container || list[0].State != "stopped" {
		t.Errorf("containers %+v", list)
	}

	// What starts an app again after a backup or restore leaves it alone
	// too.
	before, _ := e.s.Store.Deployments(ctx, "web", -1)
	for _, cause := range []string{store.CauseBackup, store.CauseRestore} {
		e.s.startAgain(ctx, "web", cause)
	}
	e.s.deploys.wg.Wait()
	if after, _ := e.s.Store.Deployments(ctx, "web", -1); len(after) != len(before) {
		t.Errorf("%d deployments queued for a stopped app", len(after)-len(before))
	}
}

func TestFailedDeploymentBringsBackTheSettingsItRanWith(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "good:1",
		"volumes": []map[string]any{{"path": "/data"}}})
	e.settle(t, "mc")
	e.b.do("PATCH", "/api/apps/mc", map[string]any{"start_command": "node server.js"})
	e.b.do("POST", "/api/apps/mc/deployments", nil)
	live := e.settle(t, "mc")
	if live.State != store.DeployLive {
		t.Fatalf("live %+v", live)
	}

	// The typo is saved for the next deployment. The volume makes the old
	// version stop first, so it has to come back after the new one fails.
	e.core.crashArgs = "sever"
	e.b.do("PATCH", "/api/apps/mc", map[string]any{"start_command": "node sever.js"})
	e.b.do("POST", "/api/apps/mc/deployments", nil)
	d := e.settle(t, "mc")
	if d.State != store.DeployFailed || d.Error == nil || d.Error.Code != "deploy.stopped_at_start" {
		t.Fatalf("typo deployment %+v", d)
	}
	container := fmt.Sprintf("mc-%d", live.ID)
	st, err := e.s.containerStatus(context.Background(), container)
	if err != nil || st.State != "running" {
		t.Fatalf("the old version is not back: %+v %v", st, err)
	}
	if got := strings.Join(e.core.args[container], " "); got != "sh -c node server.js" {
		t.Errorf("the old version came back with %q", got)
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); strings.Contains(b, "did not start either") {
		t.Errorf("log:\n%s", b)
	}
	if now, _ := e.s.Store.LiveDeployment(context.Background(), "mc"); now.ID != live.ID {
		t.Errorf("live deployment moved to %d", now.ID)
	}
}

// The port is saved for the next deployment like the other settings, but
// the live version keeps listening where it went live.
func TestRestoredVersionIsRoutedToItsOwnPort(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "good:1", "port": 3000, "domain": "mc.example.com",
		"volumes": []map[string]any{{"path": "/data"}}})
	live := e.settle(t, "mc")
	if live.State != store.DeployLive {
		t.Fatalf("live %+v", live)
	}

	e.core.crashImage = "bad:1"
	e.b.do("PATCH", "/api/apps/mc", map[string]any{"port": 8080, "image": "bad:1"})
	e.b.do("POST", "/api/apps/mc/deployments", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployFailed {
		t.Fatalf("deployment %+v", d)
	}
	container := fmt.Sprintf("mc-%d", live.ID)
	if env := e.core.env[container]; !slices.Contains(env, "PORT=3000") {
		t.Fatalf("the old version came back with %q", env)
	}
	ip := e.core.containers[container].IP.String()
	if got, want := e.proxy.routes(), "mc.example.com="+ip+":3000"; got != want {
		t.Errorf("routes %q, want %q", got, want)
	}

	// Nor does another app's route sync send the domain to the saved port.
	e.s.syncRoutes(context.Background(), nil)
	if got, want := e.proxy.routes(), "mc.example.com="+ip+":3000"; got != want {
		t.Errorf("routes after a sync %q, want %q", got, want)
	}
}

// Deployments of different apps run together, and each syncs the proxy's
// routes of every app. Whichever sync lands last wins, so none may come
// between a deployment's routes and its going live.
func TestRouteSyncsDoNotOverwriteEachOther(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "a", "source": "image", "image": "nginx:alpine", "domain": "a.example.com"})
	e.b.do("POST", "/api/apps", map[string]any{"id": "b", "source": "image", "image": "nginx:alpine", "domain": "b.example.com"})
	e.settle(t, "a")
	e.settle(t, "b")

	// While a's new container is being routed, b's deployment syncs too.
	var once sync.Once
	var other sync.WaitGroup
	e.proxy.applied = func(proxy.Config) {
		once.Do(func() {
			done := make(chan struct{})
			other.Add(1)
			go func() {
				defer other.Done()
				defer close(done)
				e.s.syncRoutes(context.Background(), nil)
			}()
			// A sync that has to wait its turn is fine.
			select {
			case <-done:
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
	if code, _ := e.b.do("POST", "/api/apps/a/deployments", nil); code != http.StatusCreated {
		t.Fatalf("deploy: %d", code)
	}
	e.s.deploys.wg.Wait()
	other.Wait()

	list, _ := e.core.List(context.Background())
	want := map[string]string{}
	for _, c := range list {
		want[c.App] = c.IP.String() + ":80"
	}
	if len(list) != 2 {
		t.Fatalf("containers %+v", list)
	}
	if got, want := e.proxy.routes(), "a.example.com="+want["a"]+" b.example.com="+want["b"]; got != want {
		t.Errorf("routes %q, want %q", got, want)
	}
}

func TestCancelledDeploymentRecordsHowItEnded(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")

	slow := newSlowSource(e.source)
	e.s.Source = slow
	e.b.do("POST", "/api/apps/web/deployments", nil)
	slow.waitEntered(t)
	if code, _ := e.b.do("POST", "/api/apps/web/stop", nil); code != http.StatusNoContent {
		t.Fatalf("stop: %d", code)
	}
	d := e.settle(t, "web")
	if d.State != store.DeployFailed || d.FinishedAt.IsZero() || d.Error == nil || d.Error.Code != "deploy.cancelled" {
		t.Fatalf("cancelled deployment %+v", d)
	}
	if b, _ := readFile(e.s.deployLogPath(d.ID)); !strings.Contains(b, "Deployment failed") {
		t.Errorf("log:\n%s", b)
	}
}

func TestOldDeploymentsAndTheirLogsAreDeleted(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx:alpine"})
	liveOne := e.settle(t, "web")
	ctx := context.Background()
	now := e.s.now()

	var ids []int64
	for i := range keepDeployments + 10 {
		id, err := e.s.Store.CreateDeployment(ctx, store.Deployment{AppID: "web", Version: "x"}, now)
		if err != nil {
			t.Fatal(err)
		}
		// The third is still waiting.
		if i != 2 {
			if err := e.s.Store.SetDeployment(ctx, store.Deployment{ID: id, State: store.DeployFailed}, now); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(e.s.deployLogPath(id), []byte("log"), 0o600); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	set := func(id int64, state, image string) {
		t.Helper()
		if err := e.s.Store.SetDeployment(ctx, store.Deployment{ID: id, State: state, Image: image}, now); err != nil {
			t.Fatal(err)
		}
	}
	set(ids[0], store.DeployReplaced, "zelie.local/web:one") // a version to roll back to
	set(ids[1], store.DeployReplaced, "zelie.local/web:two") // whose image is gone
	if err := e.s.Store.SetPruned(ctx, "web", "zelie.local/web:two"); err != nil {
		t.Fatal(err)
	}

	id, err := e.s.recordDeployment(ctx, store.Deployment{AppID: "web", Version: "x"})
	if err != nil {
		t.Fatal(err)
	}

	gone := []int64{ids[1]}
	gone = append(gone, ids[3:11]...)
	list, _ := e.s.Store.Deployments(ctx, "web", -1)
	if want := len(ids) + 2 - len(gone); len(list) != want {
		t.Errorf("%d deployments left, want %d", len(list), want)
	}
	for _, d := range list {
		if slices.Contains(gone, d.ID) {
			t.Errorf("deployment %d was kept", d.ID)
		}
	}
	for _, kept := range []int64{liveOne.ID, ids[0], ids[2], ids[11], ids[len(ids)-1], id} {
		if _, err := e.s.Store.Deployment(ctx, "web", kept); err != nil {
			t.Errorf("deployment %d: %v", kept, err)
		}
	}
	for _, n := range append(gone, ids[0], ids[len(ids)-1]) {
		_, err := os.Stat(e.s.deployLogPath(n))
		if want := !slices.Contains(gone, n); (err == nil) != want {
			t.Errorf("log of %d: exists = %v, want %v", n, err == nil, want)
		}
	}
}

// Without knowing which install the log belongs to, nothing is deleted.
func TestDeploymentsStayWhenTheGameServerCannotBeRead(t *testing.T) {
	e := newAppEnv(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	e.s.Store = db
	if err := db.CreateApp(ctx, store.App{ID: "mc", Source: store.SourceImage, Image: "x", Port: 1, MemoryMB: 512, CPUs: 1, CreatedAt: e.s.now()}); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for range keepDeployments + 5 {
		id, err := db.CreateDeployment(ctx, store.Deployment{AppID: "mc", Version: "x"}, e.s.now())
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetDeployment(ctx, store.Deployment{ID: id, State: store.DeployFailed}, e.s.now()); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	// A read that fails for another reason than the server not existing.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec("DROP TABLE game_servers"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GameServer(ctx, "mc"); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("game server: %v", err)
	}
	e.s.pruneDeployments(ctx, "mc")
	if list, _ := db.Deployments(ctx, "mc", -1); len(list) != len(ids) {
		t.Errorf("%d of %d deployments left", len(list), len(ids))
	}
}
