package engine

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestHostAccessIsSaved(t *testing.T) {
	e := &Engine{paths: Paths{Data: t.TempDir()}}
	if apps, err := e.loadHostAccess(); err != nil || len(apps) != 0 {
		t.Fatalf("a fresh install has %v, %v", apps, err)
	}
	for _, c := range []struct {
		app  string
		on   bool
		want []string
	}{
		{"web", true, []string{"web"}},
		{"api", true, []string{"api", "web"}},
		{"web", true, []string{"api", "web"}},
		{"web", false, []string{"api"}},
		{"gone", false, []string{"api"}},
		{"api", false, []string{}},
	} {
		if err := e.saveHostAccess(c.app, c.on); err != nil {
			t.Fatal(err)
		}
		// A new engine on the same directory is a restarted core.
		got, err := (&Engine{paths: e.paths}).loadHostAccess()
		if err != nil || !slices.Equal(got, c.want) && (len(got) != 0 || len(c.want) != 0) {
			t.Errorf("after %s %v: %v, %v", c.app, c.on, got, err)
		}
	}
	if err := e.SetHostAccess(context.Background(), "Bad Name", true); err == nil {
		t.Error("an app id with spaces was accepted")
	}
}

func TestHostAccessFollowsTheNetwork(t *testing.T) {
	web := network{Name: "web", Index: 3, Subnet: "10.210.3.0/24"}
	api := network{Name: "api", Index: 4, Subnet: "10.210.4.0/24"}
	rules, addrs := hostAccessRules([]network{api, web}, []string{"web", "missing"})
	if len(rules) != 1 || rules[0].bridge != "zelie3" || rules[0].gw != netip.MustParseAddr("10.210.3.1") {
		t.Errorf("rules %v", rules)
	}
	if len(addrs) != 1 || addrs["web"] != rules[0].gw {
		t.Errorf("addresses %v", addrs)
	}

	// The app lost its network and got another index: the rule moves with it.
	web.Index, web.Subnet = 9, "10.210.9.0/24"
	rules, addrs = hostAccessRules([]network{api, web}, []string{"web"})
	if rules[0].bridge != "zelie9" || addrs["web"] != netip.MustParseAddr("10.210.9.1") {
		t.Errorf("after the index changed: %v %v", rules, addrs)
	}
	if got := rules[0].iptablesRule(); !slices.Equal(got, []string{"-i", "zelie9", "-d", "10.210.9.1/32", "-p", "tcp", "--dport", "3306", "-j", "ACCEPT"}) {
		t.Errorf("iptables rule %v", got)
	}

	// A change of the openings is a change of the firewall.
	a := &firewall{bridges: []string{"zelie9"}, host: rules}
	if a.equal(&firewall{bridges: []string{"zelie9"}}) {
		t.Error("the firewall ignores host access")
	}
	if !a.equal(&firewall{bridges: []string{"zelie9"}, host: slices.Clone(rules)}) {
		t.Error("the same openings count as a change")
	}
}

func TestHostNameIsAnsweredOnlyWithHostAccess(t *testing.T) {
	web, other := netip.MustParseAddr("10.210.3.2"), netip.MustParseAddr("10.210.4.2")
	e := &Engine{}
	e.peers.at = time.Now()
	e.peers.appOf = map[netip.Addr]string{web: "web", other: "api"}
	e.peers.hostAddr = map[string]netip.Addr{"web": netip.MustParseAddr("10.210.3.1")}

	d := newDNSServer(e.lookup, []string{fakeUpstream(t)})
	ask := func(from netip.Addr, name string, qtype uint16) *dns.Msg {
		return d.answer(context.Background(), from, new(dns.Msg).SetQuestion(dns.Fqdn(name), qtype), false)
	}
	r := ask(web, "Host.Zelie.Internal", dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "10.210.3.1" {
		t.Errorf("app with host access: %v", r)
	}
	if r := ask(web, HostName, dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("AAAA: %v", r)
	}
	// Another app, and a stranger, are told there is no such name, and the
	// question is not passed on.
	for _, from := range []netip.Addr{other, netip.MustParseAddr("192.0.2.1")} {
		if r := ask(from, HostName, dns.TypeA); r.Rcode != dns.RcodeNameError {
			t.Errorf("%s: %v", from, r)
		}
	}
	// Turned off again: nothing is left of the answer.
	e.peers.hostAddr = map[string]netip.Addr{}
	if r := ask(web, HostName, dns.TypeA); r.Rcode != dns.RcodeNameError {
		t.Errorf("after turning it off: %v", r)
	}
}
