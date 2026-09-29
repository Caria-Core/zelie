package memtrim

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Trimming must leave the process able to run its own code and read its own
// read-only data afterwards.
func TestTrimKeepsProcessWorking(t *testing.T) {
	for range 3 {
		trim()
		if got := strings.ToUpper(strings.Repeat("ab", 3)); got != "ABABAB" {
			t.Fatalf("got %q", got)
		}
	}
}

func TestRunStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
