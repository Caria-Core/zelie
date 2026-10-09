package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRulesComeBackWhenTheTablesAreGone(t *testing.T) {
	quietIptables(t)
	intact, asked := true, 0
	var failure error
	old := rulesIntact
	rulesIntact = func() (bool, error) { asked++; return intact, failure }
	t.Cleanup(func() { rulesIntact = old })

	ctx := context.Background()
	e := &Engine{}
	applied := &firewall{bridges: []string{"zelie0"}}
	e.peers.applied = applied
	check := func() {
		t.Helper()
		e.peers.inputAt = time.Time{}
		if err := e.checkHostRules(ctx); err != nil {
			t.Fatal(err)
		}
	}

	check()
	if e.peers.applied != applied || asked != 1 {
		t.Errorf("rules in place: applied %v, asked %d times", e.peers.applied, asked)
	}
	// Once a minute is enough.
	if err := e.checkHostRules(ctx); err != nil || asked != 1 {
		t.Errorf("checked again within the minute: %v, asked %d times", err, asked)
	}

	intact = false
	check()
	if e.peers.applied != nil {
		t.Error("the tables are gone but the rules still count as applied, so they would never be put back")
	}
	// Nothing was applied yet, so there is nothing to compare.
	asked = 0
	check()
	if asked != 0 {
		t.Errorf("asked the kernel %d times before any rules were applied", asked)
	}

	// An error is the caller's to see, and the check is made again soon.
	e.peers.applied = applied
	failure = errors.New("netlink is down")
	e.peers.inputAt = time.Time{}
	if err := e.checkHostRules(ctx); !errors.Is(err, failure) {
		t.Errorf("error = %v", err)
	}
	if !e.peers.inputAt.IsZero() {
		t.Error("a failed check counted as done")
	}
}
