package store

import (
	"context"
	"testing"
	"time"
)

// A deleted app's deployment ids are not handed out again: logs and other
// files kept under an id must never pass to another app.
func TestDeploymentIDsAreNotReused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	newApp := func(id string) {
		if err := s.CreateApp(ctx, App{ID: id, Source: SourceImage, Image: "busybox:1.37", Port: 80, MemoryMB: 128, CPUs: 1, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	deploy := func(app string) int64 {
		id, err := s.CreateDeployment(ctx, Deployment{AppID: app}, now)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	newApp("mc")
	first, second := deploy("mc"), deploy("mc")
	if second <= first {
		t.Fatalf("ids %d then %d", first, second)
	}
	if err := s.DeleteApp(ctx, "mc"); err != nil {
		t.Fatal(err)
	}
	newApp("rust")
	if id := deploy("rust"); id <= second {
		t.Errorf("after deleting mc, rust got id %d; mc's last was %d", id, second)
	}
}

func newDeployApp(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.CreateApp(context.Background(), App{ID: id, Source: SourceImage, Image: "busybox:1.37", Port: 80, MemoryMB: 128, CPUs: 1, CreatedAt: time.Unix(1_800_000_000, 0)}); err != nil {
		t.Fatal(err)
	}
}

// A restart brings back what is live when it runs, so a deployment that
// builds something new must not be dropped in its favor, as it would be for
// another build.
func TestARestartDoesNotReplaceAQueuedBuild(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	newDeployApp(t, s, "web")
	create := func(d Deployment) Deployment {
		d.AppID = "web"
		id, err := s.CreateDeployment(ctx, d, now)
		if err != nil {
			t.Fatal(err)
		}
		d.ID = id
		return d
	}
	build := create(Deployment{Cause: CauseManual})
	restart := create(Deployment{Cause: CauseRestart, Image: "zelie.local/web:a"})
	if newer, err := s.HasNewer(ctx, build); err != nil || newer {
		t.Errorf("a restart replaces a queued build: %v, %v", newer, err)
	}

	// A restart that builds the newest commit does.
	pulling := create(Deployment{Cause: CauseRestart})
	if newer, _ := s.HasNewer(ctx, build); !newer {
		t.Error("a restart that builds does not replace a queued build")
	}
	// A restart of the live version is replaced by anything newer.
	if newer, _ := s.HasNewer(ctx, restart); !newer {
		t.Error("a restart was not replaced by the one after it")
	}
	if newer, _ := s.HasNewer(ctx, pulling); newer {
		t.Error("the newest deployment has a newer one")
	}
	again := create(Deployment{Cause: CauseRestore, Image: "zelie.local/web:a"})
	if newer, _ := s.HasNewer(ctx, pulling); newer {
		t.Error("a restart of the live version replaces a restart that builds")
	}
	if newer, _ := s.HasNewer(ctx, again); newer {
		t.Error("the newest deployment has a newer one")
	}
}

func TestDeploymentKeepsItsSettings(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	newDeployApp(t, s, "web")
	id, err := s.CreateDeployment(ctx, Deployment{AppID: "web", Version: "x"}, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Deployment(ctx, "web", id)
	if err != nil || d.Settings != nil {
		t.Fatalf("new deployment: %+v, %v", d, err)
	}
	app := App{StartCommand: "node server.js", Port: 3000, MemoryMB: 256, CPUs: 0.5, HealthPath: "/up"}
	d.Settings = new(app.RunSettings())
	d.State, d.Image = DeployLive, "zelie.local/web:x"
	if err := s.GoLive(ctx, d, now); err != nil {
		t.Fatal(err)
	}
	live, err := s.LiveDeployment(ctx, "web")
	if err != nil || live.Settings == nil || *live.Settings != app.RunSettings() {
		t.Fatalf("live deployment %+v, %v", live, err)
	}
	later := App{ID: "web", StartCommand: "typo", Port: 1, MemoryMB: 1, CPUs: 1, HealthPath: "/"}
	if got := live.Settings.ApplyTo(later); got.StartCommand != app.StartCommand || got.Port != 3000 || got.MemoryMB != 256 || got.CPUs != 0.5 || got.HealthPath != "/up" || got.ID != "web" {
		t.Errorf("applied: %+v", got)
	}
}

func TestDeleteDeployments(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	newDeployApp(t, s, "web")
	newDeployApp(t, s, "other")
	create := func(app, state string) int64 {
		id, err := s.CreateDeployment(ctx, Deployment{AppID: app}, now)
		if err != nil {
			t.Fatal(err)
		}
		if state != DeployQueued {
			if err := s.SetDeployment(ctx, Deployment{ID: id, State: state}, now); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	failed, building, other := create("web", DeployFailed), create("web", DeployBuilding), create("other", DeployFailed)
	live := create("web", DeployQueued)
	if err := s.GoLive(ctx, Deployment{ID: live, AppID: "web"}, now); err != nil {
		t.Fatal(err)
	}

	// Only finished ones of this app go; the live one and the one in
	// progress stay, whatever was asked.
	if err := s.DeleteDeployments(ctx, "web", []int64{failed, building, other, live}); err != nil {
		t.Fatal(err)
	}
	var left []int64
	list, _ := s.Deployments(ctx, "web", -1)
	for _, d := range list {
		left = append(left, d.ID)
	}
	if len(left) != 2 || left[0] != live || left[1] != building {
		t.Errorf("left %v, want %d and %d", left, live, building)
	}
	if _, err := s.Deployment(ctx, "other", other); err != nil {
		t.Errorf("another app's deployment went: %v", err)
	}
	if err := s.DeleteDeployments(ctx, "web", nil); err != nil {
		t.Error(err)
	}
}

func TestSettledOn(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	if err := s.CreateApp(ctx, App{ID: "web", Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 128, CPUs: 1, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	settled := func(live int64) bool {
		t.Helper()
		ok, err := s.SettledOn(ctx, "web", live)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	deploy := func() Deployment {
		t.Helper()
		id, err := s.CreateDeployment(ctx, Deployment{AppID: "web", Image: "nginx"}, now)
		if err != nil {
			t.Fatal(err)
		}
		d, _ := s.Deployment(ctx, "web", id)
		return d
	}

	first := deploy()
	if settled(first.ID) {
		t.Error("a deployment that is not live serves the app")
	}
	if err := s.GoLive(ctx, first, now); err != nil {
		t.Fatal(err)
	}
	if !settled(first.ID) {
		t.Error("the live deployment of a running app does not settle it")
	}
	if settled(first.ID + 100) {
		t.Error("an app is settled on a deployment that is not its own")
	}

	// A deployment on its way, until it ends in any way.
	next := deploy()
	if settled(first.ID) {
		t.Error("an app with a deployment on its way is settled")
	}
	next.State = DeployFailed
	if err := s.SetDeployment(ctx, next, now); err != nil {
		t.Fatal(err)
	}
	if !settled(first.ID) {
		t.Error("a failed deployment left the app unsettled")
	}
	next = deploy()
	if err := s.GoLive(ctx, next, now); err != nil {
		t.Fatal(err)
	}
	if settled(first.ID) || !settled(next.ID) {
		t.Error("the app did not move to the deployment that went live")
	}

	if err := s.SetStopped(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if settled(next.ID) {
		t.Error("a stopped app is settled")
	}
	if ok, err := s.SettledOn(ctx, "gone", next.ID); err != nil || ok {
		t.Errorf("an app that does not exist is settled: %v, %v", ok, err)
	}
}
