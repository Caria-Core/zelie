// Package memtrim gives memory back to the operating system while a Zelie
// process sits idle, so a panel that is only looked at now and then stays
// small.
package memtrim

import (
	"context"
	"runtime/debug"
	"runtime/metrics"
	"time"
)

const (
	// The first pass waits for start-up work, which touches code and
	// allocates far more than steady state does.
	settle = 15 * time.Second
	every  = 30 * time.Second

	// Free heap the runtime keeps for itself is only handed back above this,
	// so a process with a busy heap does not collect and refault it over and
	// over.
	keepFree = 4 << 20
)

// Run trims once the process has settled and then every half minute,
// until ctx is done.
func Run(ctx context.Context) {
	wait := settle
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		trim()
		wait = every
	}
}

func trim() {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/free:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindUint64 && sample[0].Value.Uint64() > keepFree {
		debug.FreeOSMemory()
	}
	unmapCode()
}
