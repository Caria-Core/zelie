package engine

import (
	"net/netip"
	"strings"
	"testing"
)

func TestCheckForwards(t *testing.T) {
	tcp := Forward{Port: 25565, Proto: "tcp", Target: 25565}
	udp := Forward{Port: 25565, Proto: "udp", Target: 25565}
	bad := map[string][]Forward{
		"a protocol":     {{Port: 25565, Proto: "sctp", Target: 1}},
		"a low port":     {{Port: 22, Proto: "tcp", Target: 22}},
		"no target":      {{Port: 25565, Proto: "tcp"}},
		"an IPv6 host":   {{IP: netip.MustParseAddr("2001:db8::1"), Port: 25565, Proto: "tcp", Target: 1}},
		"a port twice":   {tcp, tcp},
		"twice, any/one": {tcp, {IP: netip.MustParseAddr("192.0.2.1"), Port: 25565, Proto: "tcp", Target: 1}},
		"too many":       make([]Forward, maxForwards+1),
	}
	for name, list := range bad {
		if err := CheckForwards("mc", list); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := CheckForwards("Bad Name", nil); err == nil {
		t.Error("a bad app id was accepted")
	}
	// Both protocols on one port, and different addresses, are fine.
	one := Forward{IP: netip.MustParseAddr("192.0.2.1"), Port: 30000, Proto: "tcp", Target: 1}
	two := Forward{IP: netip.MustParseAddr("192.0.2.2"), Port: 30000, Proto: "tcp", Target: 1}
	if err := CheckForwards("mc", []Forward{tcp, udp, one, two}); err != nil {
		t.Errorf("valid forwards: %v", err)
	}
	if err := CheckForwards("mc", nil); err != nil {
		t.Errorf("clearing: %v", err)
	}
}

func TestForwardClashes(t *testing.T) {
	any4 := Forward{IP: netip.MustParseAddr("0.0.0.0"), Port: 25565, Proto: "tcp"}
	unset := Forward{Port: 25565, Proto: "tcp"}
	one := Forward{IP: netip.MustParseAddr("192.0.2.1"), Port: 25565, Proto: "tcp"}
	other := Forward{IP: netip.MustParseAddr("192.0.2.2"), Port: 25565, Proto: "tcp"}
	if !any4.clashes(unset) || !unset.clashes(one) || !one.clashes(any4) {
		t.Error("every address must clash with a specific one, and with itself")
	}
	if one.clashes(other) {
		t.Error("two specific addresses share a port")
	}
	udp := one
	udp.Proto = "udp"
	if one.clashes(udp) {
		t.Error("tcp and udp are different ports")
	}
}

func TestForwardsFollowTheContainer(t *testing.T) {
	a := portMap{Forward{Port: 25565, Proto: "tcp", Target: 25565}, netip.MustParseAddr("10.210.4.2")}
	b := portMap{Forward{Port: 25565, Proto: "udp", Target: 25565}, netip.MustParseAddr("10.210.4.2")}
	fw := &firewall{forwards: []portMap{b, a, a}}
	fw.sort()
	if len(fw.forwards) != 2 || fw.forwards[0] != a {
		t.Errorf("sorted %v", fw.forwards)
	}
	moved := &firewall{forwards: []portMap{{a.Forward, netip.MustParseAddr("10.210.4.9")}, b}}
	moved.sort()
	if fw.equal(moved) {
		t.Error("a new container address left the firewall as it was")
	}
	same := &firewall{forwards: []portMap{a, b}}
	same.sort()
	if !fw.equal(same) {
		t.Error("the same forwards differ")
	}
}

func TestForwardRule(t *testing.T) {
	f := portMap{Forward{Port: 30000, Proto: "udp", Target: 25565}, netip.MustParseAddr("10.210.4.2")}
	got := strings.Join(forwardRule(f), " ")
	want := "! -i zelie+ -o zelie+ -d 10.210.4.2/32 -p udp --dport 25565 -m conntrack --ctstate DNAT -j ACCEPT"
	if got != want {
		t.Errorf("rule %q", got)
	}
}
