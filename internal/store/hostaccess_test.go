package store

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestHostAccessIsOffUntilSet(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"web", "api"} {
		if err := s.CreateApp(ctx, App{ID: id, Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 64, CPUs: 1, CreatedAt: t0}); err != nil {
			t.Fatal(err)
		}
	}
	if on, err := s.HostAccess(ctx, "web"); err != nil || on {
		t.Fatalf("a new app has host access %v, %v", on, err)
	}
	// Setting it twice changes nothing more.
	for range 2 {
		if err := s.SetHostAccess(ctx, "web", true); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := s.HostAccesses(ctx); err != nil || !slices.Equal(list, []string{"web"}) {
		t.Fatalf("list %v, %v", list, err)
	}
	// It survives a restart, and goes with the app.
	s.Close()
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if on, _ := s.HostAccess(ctx, "web"); !on {
		t.Error("host access was lost when the database was reopened")
	}
	if on, _ := s.HostAccess(ctx, "api"); on {
		t.Error("another app got host access")
	}
	if err := s.SetHostAccess(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if on, _ := s.HostAccess(ctx, "web"); on {
		t.Error("host access stayed on")
	}
	s.SetHostAccess(ctx, "api", true)
	if err := s.DeleteApp(ctx, "api"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.HostAccesses(ctx); len(list) != 0 {
		t.Errorf("a deleted app keeps host access: %v", list)
	}
}
