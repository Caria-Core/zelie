package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestReopenKeepsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	ctx := context.Background()
	for range 2 {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestSetup(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()

	if _, err := s.CreateFirstAdmin(ctx, []byte("x"), "a@example.com", "hash", now); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("setup without a token: %v", err)
	}
	if err := s.SetSetupToken(ctx, []byte("old"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetupToken(ctx, []byte("new"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFirstAdmin(ctx, []byte("old"), "a@example.com", "hash", now); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("replaced token still works: %v", err)
	}
	if _, err := s.CreateFirstAdmin(ctx, []byte("new"), "a@example.com", "hash", now.Add(2*time.Hour)); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("expired token still works: %v", err)
	}
	u, err := s.CreateFirstAdmin(ctx, []byte("new"), "a@example.com", "hash", now)
	if err != nil || !u.Admin || u.ID == 0 {
		t.Fatalf("CreateFirstAdmin = %+v, %v", u, err)
	}

	if _, err := s.CreateFirstAdmin(ctx, []byte("new"), "b@example.com", "hash", now); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second admin through setup: %v", err)
	}
	if err := s.SetSetupToken(ctx, []byte("again"), now.Add(time.Hour)); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("new setup link after setup: %v", err)
	}
	if open, err := s.SetupOpen(ctx); open || err != nil {
		t.Fatalf("SetupOpen = %v, %v", open, err)
	}
}

func TestLastFactorCannotBeRemoved(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	s.SetSetupToken(ctx, []byte("t"), now.Add(time.Hour))
	u, err := s.CreateFirstAdmin(ctx, []byte("t"), "a@example.com", "hash", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"k1", "k2"} {
		if err := s.AddPasskey(ctx, Passkey{ID: []byte(id), UserID: u.ID, Name: id, Credential: []byte("{}"), CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeletePasskey(ctx, u.ID, []byte("k1")); err != nil {
		t.Fatalf("delete one of two: %v", err)
	}
	if err := s.DeletePasskey(ctx, u.ID, []byte("k1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete it again: %v", err)
	}
	if err := s.DeletePasskey(ctx, u.ID, []byte("k2")); !errors.Is(err, ErrLastFactor) {
		t.Fatalf("delete the last one: %v", err)
	}
	if err := s.RemoveTOTP(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remove an app that was never added: %v", err)
	}
	s.SetTOTP(ctx, u.ID, []byte("sealed"), 1)
	if err := s.DeletePasskey(ctx, u.ID, []byte("k2")); err != nil {
		t.Fatalf("delete the passkey once the app is there: %v", err)
	}
	if err := s.RemoveTOTP(ctx, u.ID); !errors.Is(err, ErrLastFactor) {
		t.Fatalf("remove the app when it is the last: %v", err)
	}
}

func TestApps(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	a := App{ID: "web", Source: SourceGitHub, Repo: "owner/web", Branch: "main", Port: 3000, Domain: "web.example.com", MemoryMB: 512, CPUs: 1, CreatedAt: now}
	if err := s.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateApp(ctx, a); !errors.Is(err, ErrExists) {
		t.Fatalf("same id twice: %v", err)
	}
	b := App{ID: "api", Source: SourceImage, Image: "nginx", Port: 80, Domain: "WEB.example.com", MemoryMB: 256, CPUs: 1, CreatedAt: now}
	if err := s.CreateApp(ctx, b); !errors.Is(err, ErrExists) {
		t.Fatalf("same domain twice: %v", err)
	}
	b.Domain = ""
	if err := s.CreateApp(ctx, b); err != nil {
		t.Fatalf("second app without a domain: %v", err)
	}

	if err := s.SetEnv(ctx, "web", []EnvVar{{Name: "PORT", Value: "3000"}, {Name: "TOKEN", Value: "sealed", Secret: true}}); err != nil {
		t.Fatal(err)
	}
	if env, _ := s.Env(ctx, "web"); len(env) != 2 || !env[1].Secret {
		t.Fatalf("env %+v", env)
	}

	id1, _ := s.CreateDeployment(ctx, Deployment{AppID: "web", Version: "abc"}, now)
	d1, _ := s.Deployment(ctx, "web", id1)
	d1.Image = "zelie.local/web:abc"
	if err := s.GoLive(ctx, d1, now); err != nil {
		t.Fatal(err)
	}
	id2, _ := s.CreateDeployment(ctx, Deployment{AppID: "web", Version: "def"}, now)
	d2, _ := s.Deployment(ctx, "web", id2)
	d2.Image = "zelie.local/web:def"
	s.GoLive(ctx, d2, now)
	live, err := s.LiveDeployment(ctx, "web")
	if err != nil || live.ID != id2 {
		t.Fatalf("live %+v, %v", live, err)
	}
	if old, _ := s.Deployment(ctx, "web", id1); old.State != DeployReplaced {
		t.Errorf("old deployment is %s", old.State)
	}
	if _, err := s.Deployment(ctx, "api", id1); !errors.Is(err, ErrNotFound) {
		t.Error("a deployment was found under another app")
	}

	id3, _ := s.CreateDeployment(ctx, Deployment{AppID: "web", Version: "ghi"}, now)
	s.FailUnfinished(ctx, now)
	if d3, _ := s.Deployment(ctx, "web", id3); d3.State != DeployFailed || d3.FinishedAt.IsZero() {
		t.Errorf("unfinished deployment: %+v", d3)
	}

	if err := s.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Deployments(ctx, "web", 10); len(list) != 0 {
		t.Error("deployments outlived their app")
	}
	if env, _ := s.Env(ctx, "web"); len(env) != 0 {
		t.Error("variables outlived their app")
	}
}

func TestGitHub(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	if _, err := s.GitHubApp(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before connecting: %v", err)
	}
	g := GitHubApp{AppID: 1, Slug: "zelie", Owner: "efe", BaseURL: "https://p", Key: []byte("k"), WebhookSecret: []byte("w"), CreatedAt: now}
	s.SetGitHubApp(ctx, g)
	g.Slug = "zelie-2"
	s.SetGitHubApp(ctx, g)
	if got, err := s.GitHubApp(ctx); err != nil || got.Slug != "zelie-2" || string(got.Key) != "k" {
		t.Fatalf("app %+v, %v", got, err)
	}

	for _, a := range []App{
		{ID: "web", Repo: "Owner/Web", Branch: "main", AutoDeploy: true},
		{ID: "staging", Repo: "owner/web", Branch: "dev", AutoDeploy: true},
		{ID: "manual", Repo: "owner/web", Branch: "main"},
	} {
		a.Source, a.Port, a.MemoryMB, a.CPUs, a.CreatedAt = SourceGitHub, 3000, 512, 1, now
		if err := s.CreateApp(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	if apps, _ := s.PushTargets(ctx, "OWNER/web", "main"); len(apps) != 1 || apps[0].ID != "web" {
		t.Errorf("push targets %+v", apps)
	}

	id1, _ := s.CreateDeployment(ctx, Deployment{AppID: "web", Version: "a", Cause: CausePush, Message: "m"}, now)
	d1, _ := s.Deployment(ctx, "web", id1)
	if d1.Cause != CausePush || d1.Message != "m" {
		t.Errorf("deployment %+v", d1)
	}
	if newer, _ := s.HasNewer(ctx, d1); newer {
		t.Error("newer before there is one")
	}
	s.CreateDeployment(ctx, Deployment{AppID: "staging", Version: "b"}, now)
	if newer, _ := s.HasNewer(ctx, d1); newer {
		t.Error("another app's deployment counted")
	}
	s.CreateDeployment(ctx, Deployment{AppID: "web", Version: "c"}, now)
	if newer, _ := s.HasNewer(ctx, d1); !newer {
		t.Error("newer not seen")
	}

	if first, _ := s.FirstDelivery(ctx, "x", now); !first {
		t.Error("first delivery")
	}
	if first, _ := s.FirstDelivery(ctx, "x", now.Add(time.Hour)); first {
		t.Error("the same delivery twice")
	}
	if first, _ := s.FirstDelivery(ctx, "x", now.Add(8*24*time.Hour)); !first {
		t.Error("a delivery id is kept forever")
	}
}
