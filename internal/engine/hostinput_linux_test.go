package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// fakeIptables keeps chains as lists of rules, the way iptables -S shows them.
type fakeIptables struct {
	chains map[string][]string
	calls  []string
}

func (f *fakeIptables) run(_ context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	op, chain, rule := args[0], args[1], strings.Join(args[2:], " ")
	rules, ok := f.chains[chain]
	switch op {
	case "-L":
		if !ok {
			return "No chain/target/match by that name.", errors.New("exit status 1")
		}
	case "-N":
		f.chains[chain] = nil
	case "-C":
		if !slices.Contains(rules, rule) {
			return "Bad rule", errors.New("exit status 1")
		}
	case "-A":
		f.chains[chain] = append(rules, rule)
	case "-I":
		// -I INPUT 1 …: the position comes first.
		f.chains[chain] = append([]string{strings.Join(args[3:], " ")}, rules...)
	}
	return "", nil
}

func withFakeIptables(t *testing.T, f *fakeIptables) {
	old := iptables
	iptables = f.run
	t.Cleanup(func() { iptables = old })
}

func TestHostInput(t *testing.T) {
	f := &fakeIptables{chains: map[string][]string{"INPUT": {"-j ufw-before-input"}}}
	withFakeIptables(t, f)
	if err := ensureInputRules(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.chains["INPUT"][0] != "-j ZELIE-INPUT" || len(f.chains["INPUT"]) != 2 {
		t.Errorf("INPUT %v: the jump must come before ufw's", f.chains["INPUT"])
	}
	want := []string{
		"-i zelie+ -p udp --dport 53 -j ACCEPT",
		"-i zelie+ -p tcp --dport 53 -j ACCEPT",
	}
	if !slices.Equal(f.chains[inputChain], want) {
		t.Errorf("ZELIE-INPUT %v", f.chains[inputChain])
	}

	// Again, as every minute: nothing is added twice.
	f.calls = nil
	if err := ensureInputRules(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if !strings.HasPrefix(c, "-L") && !strings.HasPrefix(c, "-C") {
			t.Errorf("changed rules that were in place: %s", c)
		}
	}

	// A firewall reload that drops the jump gets it back.
	f.chains["INPUT"] = []string{"-j ufw-before-input"}
	if err := ensureInputRules(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.chains["INPUT"][0] != "-j ZELIE-INPUT" {
		t.Errorf("after a reload: %v", f.chains["INPUT"])
	}
}
