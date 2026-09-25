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

	id1, _ := s.CreateDeployment(ctx, "web", "abc", now)
	d1, _ := s.Deployment(ctx, "web", id1)
	d1.Image = "zelie.local/web:abc"
	if err := s.GoLive(ctx, d1, now); err != nil {
		t.Fatal(err)
	}
	id2, _ := s.CreateDeployment(ctx, "web", "def", now)
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

	id3, _ := s.CreateDeployment(ctx, "web", "ghi", now)
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
