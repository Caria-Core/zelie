package engine

import (
	"bytes"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// sent runs fill on a connection that talks to no kernel and returns what it
// would have sent.
func sent(t *testing.T, fill func(*nftables.Conn) error) []netlink.Message {
	t.Helper()
	var out []netlink.Message
	c, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
		out = append(out, req...)
		return req, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := fill(c); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	return out
}

// rulesIn returns the rules sent for a chain, in order, as the kernel gets
// them.
func rulesIn(t *testing.T, msgs []netlink.Message, chain string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, m := range msgs {
		if m.Header.Type != netlink.HeaderType(unix.NFNL_SUBSYS_NFTABLES<<8|unix.NFT_MSG_NEWRULE) {
			continue
		}
		ad, err := netlink.NewAttributeDecoder(m.Data[4:])
		if err != nil {
			t.Fatal(err)
		}
		for ad.Next() {
			if ad.Type() == unix.NFTA_RULE_CHAIN && ad.String() == chain {
				out = append(out, m.Data)
			}
		}
	}
	return out
}

// ruleIn is how the kernel gets a rule made of these parts.
func ruleIn(t *testing.T, chain string, parts ...[]expr.Any) []byte {
	t.Helper()
	msgs := sent(t, func(c *nftables.Conn) error {
		var all []expr.Any
		for _, p := range parts {
			all = append(all, p...)
		}
		c.AddRule(&nftables.Rule{
			Table: &nftables.Table{Family: nftables.TableFamilyINet, Name: "zelie"},
			Chain: &nftables.Chain{Name: chain},
			Exprs: all,
		})
		return nil
	})
	rules := rulesIn(t, msgs, chain)
	if len(rules) != 1 {
		t.Fatalf("%d rules for %s", len(rules), chain)
	}
	return rules[0]
}

func TestOutsideTrafficCannotReachContainers(t *testing.T) {
	fw := &firewall{
		bridges:  []string{"zelie0", "zelie1"},
		forwards: []portMap{{Forward: Forward{Port: 27015, Proto: "udp", Target: 27015}, to: ip("10.210.3.2")}},
	}
	msgs := sent(t, func(c *nftables.Conn) error { return buildFirewall(c, fw) })
	forward, input := rulesIn(t, msgs, "forward"), rulesIn(t, msgs, "input")
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}

	// Last in the forward chain: a new connection into a bridge from anything
	// that is not a bridge, after the drop between bridges. Without it such
	// packets reach the chain's accept policy.
	n := len(forward)
	if n < 2 {
		t.Fatalf("%d forward rules", n)
	}
	if want := ruleIn(t, "forward", fromBridge, toBridge, drop); !bytes.Equal(forward[n-2], want) {
		t.Error("the rule that drops traffic between bridges is not second to last")
	}
	if want := ruleIn(t, "forward", notFromBridge, toBridge, notEstablished, drop); !bytes.Equal(forward[n-1], want) {
		t.Error("the last forward rule does not drop new connections into a bridge from outside")
	}
	// The forwarded port is let in before that.
	opened := ruleIn(t, "forward", notFromBridge, ipv4, toAddr(ip("10.210.3.2")), l4proto(unix.IPPROTO_UDP), dport(27015), dnatted, accept)
	if i := slices.IndexFunc(forward, func(r []byte) bool { return bytes.Equal(r, opened) }); i < 0 || i >= n-1 {
		t.Errorf("the forwarded port is let in at position %d of %d", i, n)
	}

	// The addresses the host has on the bridges answer the host and the
	// containers only.
	if len(input) < 2 {
		t.Fatalf("%d input rules", len(input))
	}
	if want := ruleIn(t, "input", ifname(expr.MetaKeyIIFNAME, "lo"), accept); !bytes.Equal(input[len(input)-2], want) {
		t.Error("loopback is not let through before the drop")
	}
	if want := ruleIn(t, "input", notFromBridge, ipv4, toContainerRange, drop); !bytes.Equal(input[len(input)-1], want) {
		t.Error("the last input rule does not drop traffic to the container range from outside")
	}
}

func TestHostAccessOpensOnePortOnTheGateway(t *testing.T) {
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}
	fw := &firewall{
		bridges: []string{"zelie3", "zelie4"},
		host:    []hostRule{{"zelie3", ip("10.210.3.1")}},
	}
	input := rulesIn(t, sent(t, func(c *nftables.Conn) error { return buildFirewall(c, fw) }), "input")

	open := ruleIn(t, "input", ifname(expr.MetaKeyIIFNAME, "zelie3"), ipv4, toAddr(ip("10.210.3.1")), l4proto(unix.IPPROTO_TCP), dport(3306), accept)
	generic := ruleIn(t, "input", fromBridge, drop)
	at := func(rule []byte) int {
		return slices.IndexFunc(input, func(r []byte) bool { return bytes.Equal(r, rule) })
	}
	if at(open) < 0 {
		t.Fatal("no rule opens port 3306 on the gateway")
	}
	if at(open) > at(generic) {
		t.Error("the opening comes after the rule that drops everything from the bridges")
	}

	// Without host access the rule is not there.
	none := rulesIn(t, sent(t, func(c *nftables.Conn) error { return buildFirewall(c, &firewall{bridges: fw.bridges}) }), "input")
	if len(none) != len(input)-1 {
		t.Errorf("%d input rules without host access, %d with", len(none), len(input))
	}
}
