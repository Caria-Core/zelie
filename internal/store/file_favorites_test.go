package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestFileFavorites(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	if err := s.SetSetupToken(ctx, []byte("t"), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateFirstAdmin(ctx, []byte("t"), "a@example.com", "hash", now)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		if err := s.CreateApp(ctx, App{ID: id, Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 64, CPUs: 1, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	list := func(app string) []string {
		got, err := s.FileFavorites(ctx, a.ID, app)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if got := list("one"); got == nil || len(got) != 0 {
		t.Fatalf("empty list is %#v", got)
	}
	for i, p := range []string{"oxide/oxide.config.json", "plugins", "plugins"} {
		if err := s.AddFileFavorite(ctx, a.ID, "one", p, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if got := list("one"); !slices.Equal(got, []string{"oxide/oxide.config.json", "plugins"}) {
		t.Errorf("after adding: %v", got)
	}
	if got := list("two"); len(got) != 0 {
		t.Errorf("another server: %v", got)
	}

	// A rename takes the stars below the folder along, and only those.
	s.AddFileFavorite(ctx, a.ID, "one", "plugins/a.cfg", now)
	s.AddFileFavorite(ctx, a.ID, "one", "plugins-old", now)
	if err := s.MoveFileFavorites(ctx, "one", "plugins", "mods"); err != nil {
		t.Fatal(err)
	}
	got := list("one")
	slices.Sort(got)
	if !slices.Equal(got, []string{"mods", "mods/a.cfg", "oxide/oxide.config.json", "plugins-old"}) {
		t.Errorf("after rename: %v", got)
	}

	if err := s.RemoveFileFavorite(ctx, a.ID, "one", "mods"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFileFavorite(ctx, a.ID, "one", "never"); err != nil {
		t.Errorf("removing what is not there: %v", err)
	}
	if got := list("one"); slices.Contains(got, "mods") {
		t.Errorf("after removing: %v", got)
	}

	for i := range MaxFileFavorites {
		if err := s.AddFileFavorite(ctx, a.ID, "two", fmt.Sprintf("f%d", i), now); err != nil {
			t.Fatalf("favourite %d: %v", i, err)
		}
	}
	if err := s.AddFileFavorite(ctx, a.ID, "two", "one-too-many", now); !errors.Is(err, ErrTooManyFavorites) {
		t.Errorf("past the limit: %v", err)
	}
	if err := s.AddFileFavorite(ctx, a.ID, "two", "f3", now); err != nil {
		t.Errorf("starring again at the limit: %v", err)
	}

	if err := s.DeleteApp(ctx, "two"); err != nil {
		t.Fatal(err)
	}
	if got := list("two"); len(got) != 0 {
		t.Errorf("after the app went: %v", got)
	}
}
