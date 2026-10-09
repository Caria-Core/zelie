package core

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

// A measurement that never ends must not hold up the panel for good: the
// walk is over files a container controls.
func TestMeasuringHasADeadline(t *testing.T) {
	old := MeasureTimeout
	MeasureTimeout = 50 * time.Millisecond
	t.Cleanup(func() { MeasureTimeout = old })
	f := &fakeEngine{measuring: make(chan struct{})}
	s := &Server{Engine: f, Log: slog.New(slog.DiscardHandler)}
	c := buildClient(t, s)
	// Runs before the server is closed, which waits for requests in progress.
	t.Cleanup(func() { close(f.measuring) })

	start := time.Now()
	if _, _, err := c.VolumeSizes(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}
