package engine

import (
	"context"
	"errors"
	"net/netip"
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
	case "-F":
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

func TestHostForward(t *testing.T) {
	f := &fakeIptables{chains: map[string][]string{"FORWARD": {"-j ufw-before-forward"}}}
	withFakeIptables(t, f)
	ctx := context.Background()

	// Nothing forwarded, nothing made.
	if err := ensureForwardRules(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.chains[forwardChain]; ok || len(f.chains["FORWARD"]) != 1 {
		t.Fatalf("touched iptables for no forwards: %v", f.chains)
	}

	ip := netip.MustParseAddr("10.210.4.2")
	a := portMap{Forward{Port: 25565, Proto: "tcp", Target: 25565}, ip}
	b := portMap{Forward{Port: 25565, Proto: "udp", Target: 25565}, ip}
	if err := ensureForwardRules(ctx, []portMap{a, b}, true); err != nil {
		t.Fatal(err)
	}
	if f.chains["FORWARD"][0] != "-j ZELIE-FORWARD" || len(f.chains["FORWARD"]) != 2 {
		t.Errorf("FORWARD %v: the jump must come before ufw's", f.chains["FORWARD"])
	}
	want := []string{
		"! -i zelie+ -o zelie+ -d 10.210.4.2/32 -p tcp --dport 25565 -m conntrack --ctstate DNAT -j ACCEPT",
		"! -i zelie+ -o zelie+ -d 10.210.4.2/32 -p udp --dport 25565 -m conntrack --ctstate DNAT -j ACCEPT",
	}
	if !slices.Equal(f.chains[forwardChain], want) {
		t.Errorf("ZELIE-FORWARD %v", f.chains[forwardChain])
	}

	// The check every minute adds nothing that is there, and puts the jump
	// back after a reload.
	f.chains["FORWARD"] = []string{"-j ufw-before-forward"}
	f.calls = nil
	if err := ensureForwardRules(ctx, []portMap{a, b}, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "-A "+forwardChain) || strings.HasPrefix(c, "-F") {
			t.Errorf("changed rules that were in place: %s", c)
		}
	}
	if f.chains["FORWARD"][0] != "-j ZELIE-FORWARD" || len(f.chains[forwardChain]) != 2 {
		t.Errorf("after a reload: %v %v", f.chains["FORWARD"], f.chains[forwardChain])
	}

	// The container got a new address: the old rule goes.
	moved := portMap{a.Forward, netip.MustParseAddr("10.210.4.9")}
	if err := ensureForwardRules(ctx, []portMap{moved}, true); err != nil {
		t.Fatal(err)
	}
	if got := f.chains[forwardChain]; len(got) != 1 || !strings.Contains(got[0], "10.210.4.9/32") {
		t.Errorf("after the address changed: %v", got)
	}

	// Clearing empties the chain but leaves it, and the jump, alone.
	if err := ensureForwardRules(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	if len(f.chains[forwardChain]) != 0 || f.chains["FORWARD"][0] != "-j ZELIE-FORWARD" {
		t.Errorf("after clearing: %v %v", f.chains["FORWARD"], f.chains[forwardChain])
	}
}

// quietIptables keeps a test that reaches the host's rules from running the
// machine's own iptables.
func quietIptables(t *testing.T) {
	withFakeIptables(t, &fakeIptables{chains: map[string][]string{"INPUT": nil}})
}
