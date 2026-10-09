package panel

import (
	"context"
	"net/http"
	"slices"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Some games open ports next to the one they are given without an egg
// variable to say so: Valheim's query port is always the game port plus one.
// The catalog says how many ports in a row such a game needs (Block, the game
// port included). The server holds them like any other port of the pool, so
// they are forwarded like the rest, but they are never chosen on their own:
// choosing the game port chooses the run.

var (
	errNoBlock     = msg.Define(http.StatusConflict, "allocation.no_block", "This game needs {count} ports in a row, and the pool has no such run free. Add a range of ports to the pool first.")
	errBrokenBlock = msg.Define(http.StatusConflict, "allocation.broken_block", "Port {port} needs the {count} ports from it in a row, and they are not all free in the pool.")
)

// blockSize is how many ports in a row the game of a stored egg needs. An
// egg that came from an address has no such note.
func blockSize(source string) int {
	if entry, ok := egg.Lookup(source); ok {
		return max(entry.Block, 1)
	}
	return 1
}

func (s *Server) gameBlock(ctx context.Context, g store.GameServer) int {
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return 1
	}
	return blockSize(stored.Source)
}

// addrPort is a port on one address of the pool. The same number may be in
// the pool on several addresses.
type addrPort struct {
	ip   string
	port int
}

// runAt lists the n allocations of the pool that start at first: the same
// address and the ports one after the other. ok is false when one is missing
// or the usable check turns one down.
func runAt(byAddr map[addrPort]store.Allocation, first store.Allocation, n int, usable func(store.Allocation) bool) ([]store.Allocation, bool) {
	run := []store.Allocation{first}
	for i := 1; i < n; i++ {
		a, ok := byAddr[addrPort{first.IP, first.Port + i}]
		if !ok || !usable(a) {
			return nil, false
		}
		run = append(run, a)
	}
	return run, true
}

func portIndex(list []store.Allocation) map[addrPort]store.Allocation {
	m := make(map[addrPort]store.Allocation, len(list))
	for _, a := range list {
		m[addrPort{a.IP, a.Port}] = a
	}
	return m
}

// blockIntact says whether the server holds the block that starts at its
// game port.
func blockIntact(held []store.Allocation, port, block int) bool {
	if block < 2 {
		return true
	}
	i := slices.IndexFunc(held, func(a store.Allocation) bool { return a.Port == port })
	if i < 0 {
		return false
	}
	_, ok := runAt(portIndex(held), held[i], block, func(store.Allocation) bool { return true })
	return ok
}

// placeBlock is place for a game that needs a run of ports. The game port
// starts the run: the one asked for, or else the lowest run that is free.
// The run comes first in the result, then the ports asked for by variable,
// then whatever else the server needs, lowest first.
func placeBlock(list []store.Allocation, w needs) (placement, error) {
	vars := w.Chosen
	if w.Primary {
		vars = w.Chosen[1:]
	}
	inVars := map[int64]bool{}
	for _, id := range vars {
		inVars[id] = true
	}
	free := func(a store.Allocation) bool { return a.AppID == "" && !inVars[a.ID] }
	byAddr := portIndex(list)

	var run []store.Allocation
	if w.Primary {
		first := list[slices.IndexFunc(list, func(a store.Allocation) bool { return a.ID == w.Chosen[0] })]
		var ok bool
		if run, ok = runAt(byAddr, first, w.Block, free); !ok {
			return placement{}, errBrokenBlock.Err("port", first.Port, "count", w.Block)
		}
	} else {
		for _, a := range list {
			if !free(a) {
				continue
			}
			if r, ok := runAt(byAddr, a, w.Block, free); ok {
				run = r
				break
			}
		}
		if run == nil {
			return placement{}, errNoBlock.Err("count", w.Block)
		}
	}

	p := placement{Allocations: slices.Clone(run)}
	inRun := map[int64]bool{}
	for _, a := range run {
		inRun[a.ID] = true
	}
	for _, id := range vars {
		i := slices.IndexFunc(list, func(a store.Allocation) bool { return a.ID == id })
		p.Allocations = append(p.Allocations, list[i])
	}
	for _, a := range list {
		if free(a) && !inRun[a.ID] && len(p.Allocations) < w.Ports {
			p.Allocations = append(p.Allocations, a)
		}
	}
	if len(p.Allocations) < w.Ports {
		return placement{}, errNoFreePort.Err()
	}
	return p, nil
}
