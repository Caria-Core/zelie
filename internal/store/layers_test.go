package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

var testWhy = msg.Define(0, "test.layer_why", "Over by {by}.")

func TestTheFirstLayerCheckIsRememberedForGood(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	at, created, err := s.FirstLayerCheck(ctx, t0)
	if err != nil || !created || !at.Equal(t0) {
		t.Fatalf("first: %v %v %v", at, created, err)
	}
	at, created, err = s.FirstLayerCheck(ctx, t0.Add(time.Hour))
	if err != nil || created || !at.Equal(t0) {
		t.Fatalf("second: %v %v %v", at, created, err)
	}
	// A restart or an update does not make it the first time again.
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if at, created, err = s.FirstLayerCheck(ctx, t0.Add(48*time.Hour)); err != nil || created || !at.Equal(t0) {
		t.Fatalf("after reopening: %v %v %v", at, created, err)
	}
}

func TestADeadlineAndAWarningStayWithTheApp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateApp(ctx, App{ID: "web", Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 64, CPUs: 1, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if l, err := s.AppLayers(ctx, "web"); err != nil || !l.Deadline.IsZero() || l.Warning != nil || l.StoppedFor != nil {
		t.Fatalf("a new app has %+v, %v", l, err)
	}

	until := t0.Add(24 * time.Hour)
	if err := s.WarnLayers(ctx, "web", until, testWhy.With("by", "1 GB")); err != nil {
		t.Fatal(err)
	}
	// A later warning brings new numbers and leaves the deadline where it was.
	if err := s.WarnLayers(ctx, "web", until.Add(time.Hour), testWhy.With("by", "2 GB")); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, err := s.AppLayers(ctx, "web")
	if err != nil || !l.Deadline.Equal(until) || l.Warning == nil || l.Warning.Params["by"] != "2 GB" {
		t.Fatalf("after reopening: %+v, %v", l, err)
	}

	if err := s.ClearLayerWarning(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.AppLayers(ctx, "web"); !l.Deadline.IsZero() || l.Warning != nil {
		t.Errorf("cleared, but %+v", l)
	}
	// Over again, it gets a deadline of its own.
	later := t0.Add(72 * time.Hour)
	s.WarnLayers(ctx, "web", later, testWhy.With("by", "1 GB"))
	if l, _ := s.AppLayers(ctx, "web"); !l.Deadline.Equal(later) {
		t.Errorf("deadline %v, want %v", l.Deadline, later)
	}

	if err := s.WarnLayers(ctx, "gone", until, testWhy.With("by", "1 GB")); !errors.Is(err, ErrNotFound) {
		t.Errorf("warning an app that is not there: %v", err)
	}
}

// A reason is kept only for an app that is stopped, and goes when the app is
// started again, with the deadline.
func TestTheReasonForAStopIsKeptUntilTheAppStarts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateApp(ctx, App{ID: "web", Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 64, CPUs: 1, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	why := testWhy.With("by", "1 GB")
	if err := s.SetStoppedFor(ctx, "web", why); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a reason for an app that runs: %v", err)
	}
	if l, _ := s.AppLayers(ctx, "web"); l.StoppedFor != nil {
		t.Fatalf("the reason was kept for a running app: %+v", l)
	}

	s.WarnLayers(ctx, "web", t0.Add(time.Hour), why)
	if err := s.SetStopped(ctx, "web", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStoppedFor(ctx, "web", why); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	l, _ := s.AppLayers(ctx, "web")
	if l.StoppedFor == nil || l.StoppedFor.Code != "test.layer_why" || !l.Deadline.IsZero() || l.Warning != nil {
		t.Fatalf("after reopening: %+v", l)
	}

	if err := s.SetStopped(ctx, "web", false); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.AppLayers(ctx, "web"); l.StoppedFor != nil {
		t.Errorf("the reason stays after the app was started: %+v", l)
	}
}

// Deleting the app takes its notes with it.
func TestDeletingAnAppForgetsItsLayerNotes(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.CreateApp(ctx, App{ID: "web", Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 64, CPUs: 1, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	s.WarnLayers(ctx, "web", t0, testWhy.With("by", "1 GB"))
	if err := s.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM app_layers").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows left", n)
	}
}
