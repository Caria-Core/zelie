package store

import (
	"context"
	"errors"
	"fmt"
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

// The narrow updates change one thing and leave what else the caller's copy
// of the app has out of date alone.
func TestAppUpdatesThatChangeOneThing(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	a := App{ID: "srv", Source: SourceImage, Image: "ghcr.io/example/java:21", Port: 25565, MemoryMB: 1024, CPUs: 1, CreatedAt: time.Unix(1_800_000_000, 0)}
	if err := s.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}

	if err := s.SetLimits(ctx, "srv", 2048, 2); err != nil {
		t.Fatal(err)
	}
	got, _ := s.App(ctx, "srv")
	if got.MemoryMB != 2048 || got.CPUs != 2 || got.Image != a.Image || got.Port != 25565 {
		t.Errorf("after SetLimits: %+v", got)
	}

	// The tag it started from is swapped for the build, once.
	pinned := a.Image + "@sha256:abc"
	if ok, err := s.PinImage(ctx, "srv", a.Image, pinned); err != nil || !ok {
		t.Fatalf("pin: %v, %v", ok, err)
	}
	if ok, err := s.PinImage(ctx, "srv", a.Image, "other"); err != nil || ok {
		t.Errorf("pin from a tag the app no longer has: %v, %v", ok, err)
	}
	got, _ = s.App(ctx, "srv")
	if got.Image != pinned || got.MemoryMB != 2048 {
		t.Errorf("after PinImage: %+v", got)
	}

	if err := s.SetImage(ctx, "srv", "ghcr.io/example/java:17"); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.App(ctx, "srv"); got.Image != "ghcr.io/example/java:17" || got.CPUs != 2 {
		t.Errorf("after SetImage: %+v", got)
	}
	if err := s.SetImage(ctx, "nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("image of a missing app: %v", err)
	}
	if err := s.SetLimits(ctx, "nope", 1, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("limits of a missing app: %v", err)
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

func TestCheckSetupTokenChangesNothing(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()

	if err := s.CheckSetupToken(ctx, []byte("x"), now); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("no token yet: %v", err)
	}
	s.SetSetupToken(ctx, []byte("t"), now.Add(time.Hour))
	if err := s.CheckSetupToken(ctx, []byte("other"), now); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("a wrong token: %v", err)
	}
	if err := s.CheckSetupToken(ctx, []byte("t"), now.Add(2*time.Hour)); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("an expired token: %v", err)
	}
	// Asking does not spend the token.
	for range 2 {
		if err := s.CheckSetupToken(ctx, []byte("t"), now); err != nil {
			t.Fatalf("a good token: %v", err)
		}
	}
	if _, err := s.CreateFirstAdmin(ctx, []byte("t"), "a@example.com", "hash", now); err != nil {
		t.Fatalf("create after checking: %v", err)
	}
	if err := s.CheckSetupToken(ctx, []byte("t"), now); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("after setup: %v", err)
	}
}

func TestResetLogin(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	s.SetSetupToken(ctx, []byte("t"), now.Add(time.Hour))
	u, err := s.CreateFirstAdmin(ctx, []byte("t"), "a@example.com", "hash", now)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO users (email, password, admin, created_at) VALUES ('b@example.com', 'hash', 1, ?)", now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	otherID, _ := res.LastInsertId()
	for i, id := range []int64{u.ID, u.ID, otherID} {
		key := SSHKey{UserID: id, Name: "k", Fingerprint: fmt.Sprint("SHA256:", i), PublicKey: []byte("key"), CreatedAt: now}
		if _, err := s.AddSSHKey(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	s.AddPasskey(ctx, Passkey{ID: []byte("p"), UserID: u.ID, Name: "p", Credential: []byte("{}"), CreatedAt: now})

	if err := s.CheckResetToken(ctx, []byte("r"), now); !errors.Is(err, ErrBadResetToken) {
		t.Fatalf("no token yet: %v", err)
	}
	if _, err := s.SetResetToken(ctx, "a@example.com", []byte("r"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckResetToken(ctx, []byte("wrong"), now); !errors.Is(err, ErrBadResetToken) {
		t.Fatalf("a wrong token: %v", err)
	}
	if err := s.CheckResetToken(ctx, []byte("r"), now.Add(2*time.Hour)); !errors.Is(err, ErrBadResetToken) {
		t.Fatalf("an expired token: %v", err)
	}
	if err := s.CheckResetToken(ctx, []byte("r"), now); err != nil {
		t.Fatalf("a good token: %v", err)
	}
	if _, err := s.ResetLogin(ctx, []byte("r"), "new hash", now); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckResetToken(ctx, []byte("r"), now); !errors.Is(err, ErrBadResetToken) {
		t.Errorf("the token after it was spent: %v", err)
	}

	// Whoever had the account may have added a key, so none is left; the
	// other administrator's stays.
	if keys, _ := s.SSHKeys(ctx, u.ID); len(keys) != 0 {
		t.Errorf("%d SSH keys left after the reset", len(keys))
	}
	if keys, _ := s.SSHKeys(ctx, otherID); len(keys) != 1 {
		t.Errorf("the other account has %d SSH keys, want 1", len(keys))
	}
	if a, _ := s.AccountByID(ctx, u.ID); a.Passkeys != 0 || a.Password != "new hash" {
		t.Errorf("account after the reset: %+v", a)
	}
}

func TestCreateSessionForgetsExpiredOnes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	s.SetSetupToken(ctx, []byte("t"), now.Add(time.Hour))
	u, err := s.CreateFirstAdmin(ctx, []byte("t"), "a@example.com", "hash", now)
	if err != nil {
		t.Fatal(err)
	}
	session := func(hash string, created, expires time.Time) Session {
		return Session{Hash: []byte(hash), UserID: u.ID, CreatedAt: created, SeenAt: created, ExpiresAt: expires, IP: "198.51.100.7"}
	}
	count := func() (n int) {
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM sessions").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Three sessions made earlier, when none of them had expired yet.
	before := now.Add(-3 * time.Hour)
	for _, x := range []Session{
		session("old", before, now.Add(-time.Hour)),
		session("just gone", before, now),
		session("kept", before, now.Add(time.Hour)),
	} {
		if err := s.CreateSession(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(); n != 3 {
		t.Fatalf("%d sessions, want 3", n)
	}
	if err := s.CreateSession(ctx, session("new", now, now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Errorf("%d sessions after a new one, want 2: the expired ones should be gone", n)
	}
	for _, hash := range []string{"kept", "new"} {
		if _, err := s.Session(ctx, []byte(hash), now); err != nil {
			t.Errorf("session %q: %v", hash, err)
		}
	}
}
