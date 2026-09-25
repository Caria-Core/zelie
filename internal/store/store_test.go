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
