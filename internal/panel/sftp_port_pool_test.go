package panel

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
)

// The SFTP port must never reach a game server, or the forward that game
// gets would send SFTP logins to it.
func TestSFTPPortStaysOutOfGames(t *testing.T) {
	e := newRustEnv(t)
	ctx := context.Background()
	sftp, err := e.s.Store.SFTPPort(ctx, store.ThisNode)
	if err != nil {
		t.Fatal(err)
	}

	if code, out := e.b.do("POST", "/api/nodes/1/allocations", map[string]any{"ports": "2000-2300"}); code != http.StatusConflict || out["code"] != "allocation.sftp_port" {
		t.Errorf("a range with the SFTP port: %d %v", code, out)
	}
	if list, _ := e.s.Store.Allocations(ctx, store.ThisNode); slices.ContainsFunc(list, func(a store.Allocation) bool { return a.Port == sftp }) {
		t.Fatal("the refused range was added")
	}

	// A pool from v0.6.0 may already hold it, below the other ports.
	if err := e.s.Store.AddAllocations(ctx, store.ThisNode, store.AnyAddress, []int{sftp}, e.s.now()); err != nil {
		t.Fatal(err)
	}
	ids := e.poolIDs(t)

	p, err := e.s.place(ctx, store.ThisNode, needs{Ports: 3})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range p.Allocations {
		if a.Port == sftp {
			t.Errorf("place handed out the SFTP port: %+v", p.Allocations)
		}
	}
	if _, err := e.s.place(ctx, store.ThisNode, needs{Ports: 1, Chosen: []int64{ids[sftp]}, Primary: true}); err == nil {
		t.Error("place took the SFTP port when it was asked for")
	}

	e.newGame(t, "survival", consoleEggURL, map[string]any{"ports": 1})
	code, out := e.b.do("PUT", "/api/games/survival/ports", map[string]any{"primary": ids[sftp]})
	if code != http.StatusConflict || out["code"] != "allocation.sftp_port" {
		t.Errorf("changing ports to the SFTP port: %d %v", code, out)
	}
}

func TestChangingPortsClearsTheCoreForwards(t *testing.T) {
	e := newRustEnv(t)
	e.newGame(t, "survival", consoleEggURL, map[string]any{"ports": 1})
	ids := e.poolIDs(t)
	e.core.cleared = nil
	if code, out := e.b.do("PUT", "/api/games/survival/ports", map[string]any{"primary": ids[25570]}); code != http.StatusOK {
		t.Fatalf("change: %d %v", code, out)
	}
	if !slices.Equal(e.core.cleared, []string{"survival"}) {
		t.Errorf("the core was told to clear %v", e.core.cleared)
	}
}
