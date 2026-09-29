package core

import (
	"net/netip"
	"strings"
	"testing"
)

func TestPickAddress(t *testing.T) {
	a := func(name, ip string) ifaceAddr { return ifaceAddr{name, netip.MustParseAddr(ip)} }
	cases := []struct {
		name    string
		addrs   []ifaceAddr
		route   string
		want    string
		private bool
	}{
		{"public card", []ifaceAddr{a("lo", "127.0.0.1"), a("eth0", "203.0.113.7")}, "eth0", "203.0.113.7", false},
		{"first public wins", []ifaceAddr{a("eth0", "203.0.113.7"), a("eth1", "198.51.100.2")}, "eth0", "203.0.113.7", false},
		{"public after private", []ifaceAddr{a("eth0", "10.0.0.4"), a("eth1", "198.51.100.2")}, "eth0", "198.51.100.2", false},
		{"own bridges skipped", []ifaceAddr{a("zelie0", "203.0.113.1"), a("docker0", "172.17.0.1"), a("veth1", "198.51.100.9"), a("br-1f", "198.51.100.10"), a("cni0", "198.51.100.11"), a("eth0", "203.0.113.7")}, "eth0", "203.0.113.7", false},
		{"nat", []ifaceAddr{a("lo", "127.0.0.1"), a("eth0", "192.168.1.20")}, "eth0", "192.168.1.20", true},
		{"cgnat is not public", []ifaceAddr{a("eth0", "100.72.1.5")}, "eth0", "100.72.1.5", true},
		{"private on the default route", []ifaceAddr{a("wg0", "10.8.0.2"), a("eth0", "192.168.1.20")}, "eth0", "192.168.1.20", true},
		{"private not on the route", []ifaceAddr{a("wg0", "10.8.0.2")}, "eth0", "", false},
		{"route unknown", []ifaceAddr{a("eth0", "192.168.1.20")}, "", "192.168.1.20", true},
		{"link local is not an address", []ifaceAddr{a("eth0", "169.254.3.3")}, "eth0", "", false},
		{"ipv6 is ignored", []ifaceAddr{a("eth0", "2001:db8::1")}, "eth0", "", false},
		{"mapped ipv4", []ifaceAddr{a("eth0", "::ffff:203.0.113.7")}, "eth0", "203.0.113.7", false},
		{"nothing", nil, "", "", false},
	}
	for _, c := range cases {
		got, ok := pickAddress(c.addrs, c.route)
		if got.Address != c.want || got.Private != c.private || ok != (c.want != "") {
			t.Errorf("%s: %+v, %v; want %s private=%v", c.name, got, ok, c.want, c.private)
		}
	}
}

func TestDefaultRouteCard(t *testing.T) {
	const table = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
eth1	00000000	0100A8C0	0002	0	0	0	00000000	0	0	0
eth0	00000000	0100A8C0	0003	0	0	100	00000000	0	0	0
`
	if got := defaultRouteCard(strings.NewReader(table)); got != "eth0" {
		t.Errorf("default route card = %q", got)
	}
	if got := defaultRouteCard(strings.NewReader("Iface\tDestination\n")); got != "" {
		t.Errorf("no route = %q", got)
	}
}
