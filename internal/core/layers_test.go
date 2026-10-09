package core

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

func TestLayerSizesReachThePanel(t *testing.T) {
	f := &fakeEngine{layers: []engine.LayerSize{
		{Container: "web-1", App: "web", Bytes: 5 << 20},
		{Container: "blog-2", App: "blog", Unmeasured: true},
	}}
	s := &Server{Engine: f, Log: slog.New(slog.DiscardHandler)}
	c := buildClient(t, s)

	got, err := c.LayerSizes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, f.layers) {
		t.Errorf("got %+v, want %+v", got, f.layers)
	}

	// Nothing running is an empty list, not an error.
	f.layers = nil
	if got, err := c.LayerSizes(context.Background()); err != nil || len(got) != 0 {
		t.Errorf("with nothing running: %+v, %v", got, err)
	}

	// A failure to measure is the core's to report. It must not read as an
	// empty list, which would tell the panel that every app is within its
	// limit.
	f.layersErr = errors.New("containerd is not answering")
	_, err = c.LayerSizes(context.Background())
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 500 {
		t.Errorf("a failed measurement came back as %v", err)
	}
}

// The walk over the files of a container must not hold up the panel for good
// either.
func TestMeasuringLayersHasADeadline(t *testing.T) {
	old := MeasureTimeout
	MeasureTimeout = 50 * time.Millisecond
	t.Cleanup(func() { MeasureTimeout = old })
	f := &fakeEngine{measuring: make(chan struct{})}
	s := &Server{Engine: f, Log: slog.New(slog.DiscardHandler)}
	c := buildClient(t, s)
	// Runs before the server is closed, which waits for requests in progress.
	t.Cleanup(func() { close(f.measuring) })

	start := time.Now()
	if _, err := c.LayerSizes(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the deadline", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}
