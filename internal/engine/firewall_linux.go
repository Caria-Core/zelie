package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// applyFirewall installs Zelie's rules for containers:
//
//   - Containers cannot open connections to the host itself, such as SSH or
//     a database listening on all addresses, except to ask the DNS server
//     on their network's gateway. Replies to connections the host started,
//     like the proxy talking to an app, still get through.
//   - An app with host access may also connect to TCP port 3306 on its own
//     network's gateway address (see SetHostAccess).
//   - Containers on different networks cannot reach each other, except
//     through the openings links make: one address to another, on one TCP
//     port.
//   - Ports forwarded to a container (see Forward) are open from outside,
//     to that container's port only. The address translation lives in a
//     second table, since NAT in an inet table needs a newer kernel than
//     the rest of the rules do.
//   - Nothing else gets in from outside the bridges: a Docker container
//     or a machine that routes the range through this host cannot open
//     connections to containers, or ask the DNS servers on the gateways.
//
// The rules live in a table of their own, so they work next to ufw,
// firewalld or hand-written rules without touching them. The table is
// replaced as a whole in one transaction: there is never a moment with no
// rules or with half of them.
func applyFirewall(fw *firewall) error {
	c, err := nftables.New()
	if err != nil {
		return err
	}
	if err := buildFirewall(c, fw); err != nil {
		return err
	}
	if err := c.Flush(); err != nil {
		return fmt.Errorf("install host firewall rules: %w", err)
	}
	return nil
}

// buildFirewall queues the rules on c without sending them.
func buildFirewall(c *nftables.Conn, fw *firewall) error {
	table := &nftables.Table{Family: nftables.TableFamilyINet, Name: "zelie"}
	c.AddTable(table) // so the delete below never fails on a fresh machine
	c.DelTable(table)
	c.AddTable(table)

	policy := nftables.ChainPolicyAccept
	input := c.AddChain(&nftables.Chain{
		Name: "input", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookInput, Priority: nftables.ChainPriorityFilter, Policy: &policy,
	})
	forward := c.AddChain(&nftables.Chain{
		Name: "forward", Table: table, Type: nftables.ChainTypeFilter,
		Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter, Policy: &policy,
	})

	links := &nftables.Set{
		Table:         table,
		Name:          "links",
		KeyType:       nftables.MustConcatSetType(nftables.TypeIPAddr, nftables.TypeIPAddr, nftables.TypeInetService),
		Concatenation: true,
	}
	elems := make([]nftables.SetElement, 0, len(fw.allow))
	for _, a := range fw.allow {
		from, to := a.from.As4(), a.to.As4()
		key := append(append(from[:], to[:]...), binaryutil.BigEndian.PutUint16(a.port)...)
		elems = append(elems, nftables.SetElement{Key: append(key, 0, 0)})
	}
	if err := c.AddSet(links, elems); err != nil {
		return err
	}

	rule := func(chain *nftables.Chain, exprs ...[]expr.Any) {
		var all []expr.Any
		for _, e := range exprs {
			all = append(all, e...)
		}
		c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: all})
	}
	accept := []expr.Any{&expr.Verdict{Kind: expr.VerdictAccept}}
	drop := []expr.Any{&expr.Verdict{Kind: expr.VerdictDrop}}

	rule(input, fromBridge, established, accept)
	for _, proto := range []byte{unix.IPPROTO_UDP, unix.IPPROTO_TCP} {
		rule(input, fromBridge, ipv4, l4proto(proto), toContainerRange, dport(53), accept)
	}
	// An app with host access reaches the database port on its own bridge's
	// address, and nothing else of the host.
	for _, h := range fw.host {
		rule(input, ifname(expr.MetaKeyIIFNAME, h.bridge), ipv4, toAddr(h.gw), l4proto(unix.IPPROTO_TCP), dport(HostPort), accept)
	}
	rule(input, fromBridge, drop)
	// The host's own addresses on the bridges answer for anyone who can route
	// to them. Only the host itself, which arrives on lo, and the bridges may.
	rule(input, ifname(expr.MetaKeyIIFNAME, "lo"), accept)
	rule(input, notFromBridge, ipv4, toContainerRange, drop)

	rule(forward, fromBridge, toBridge, established, accept)
	// Traffic from outside that was sent to a forwarded port. Nothing here
	// opens more than the one port; it also keeps a rule added later by
	// someone else from catching it first.
	for _, f := range fw.forwards {
		rule(forward, notFromBridge, ipv4, toAddr(f.to), l4proto(protoNumber(f.Proto)), dport(f.Target), dnatted, accept)
	}
	// Containers of one app share a bridge. Their traffic only passes
	// through here when br_netfilter is loaded, as Docker does.
	for _, b := range fw.bridges {
		rule(forward, ifname(expr.MetaKeyIIFNAME, b), ifname(expr.MetaKeyOIFNAME, b), accept)
	}
	rule(forward, fromBridge, toBridge, ipv4, l4proto(unix.IPPROTO_TCP), []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 12, Len: 4},
		&expr.Payload{DestRegister: 9, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Payload{DestRegister: 10, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Lookup{SourceRegister: 1, SetName: links.Name, SetID: links.ID},
	}, accept)
	rule(forward, fromBridge, toBridge, drop)
	// Without this, whatever comes from another interface and is not a
	// forward falls through to the chain's accept policy: a Docker container,
	// or a machine on the network that routes the range through this host,
	// would reach every port of every container. Replies to what a container
	// started still pass, and the forwards were accepted above.
	rule(forward, notFromBridge, toBridge, notEstablished, drop)

	return addForwards(c, fw.forwards)
}

// firewallIntact reports whether the rules applyFirewall installed are still
// in the kernel. Another firewall reloading, or someone flushing the ruleset,
// takes them away without a word.
func firewallIntact() (bool, error) {
	c, err := nftables.New()
	if err != nil {
		return false, err
	}
	return tablesIntact(c)
}

func tablesIntact(c *nftables.Conn) (bool, error) {
	tables, err := c.ListTables()
	if err != nil {
		return false, fmt.Errorf("list nftables tables: %w", err)
	}
	var filter, nat *nftables.Table
	for _, t := range tables {
		switch {
		case t.Family == nftables.TableFamilyINet && t.Name == "zelie":
			filter = t
		case t.Family == nftables.TableFamilyIPv4 && t.Name == "zelie-nat":
			nat = t
		}
	}
	if filter == nil || nat == nil {
		return false, nil
	}
	// A flushed table keeps its chains and loses the rules in them. A chain
	// that cannot be read is as good as gone: the rules are put back.
	rules, err := c.GetRules(filter, &nftables.Chain{Name: "forward"})
	return err == nil && len(rules) > 0, nil
}

// addForwards replaces the NAT table that sends forwarded host ports to
// their containers. Traffic from outside is translated before routing;
// traffic from the host itself, such as a player on the same machine, in
// the output chain. Only packets addressed to the host are touched, so
// traffic the host merely routes on its way is left alone.
func addForwards(c *nftables.Conn, forwards []portMap) error {
	table := &nftables.Table{Family: nftables.TableFamilyIPv4, Name: "zelie-nat"}
	c.AddTable(table)
	c.DelTable(table)
	c.AddTable(table)
	if len(forwards) == 0 {
		// The table stays empty for the next change to fill.
		return nil
	}
	pre := c.AddChain(&nftables.Chain{
		Name: "prerouting", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookPrerouting, Priority: nftables.ChainPriorityNATDest,
	})
	out := c.AddChain(&nftables.Chain{
		Name: "output", Table: table, Type: nftables.ChainTypeNAT,
		Hooknum: nftables.ChainHookOutput, Priority: nftables.ChainPriorityNATDest,
	})
	for _, f := range forwards {
		var to []expr.Any
		if a := f.IP; a.IsValid() && !a.IsUnspecified() {
			to = toAddr(a)
		} else {
			to = toLocal
		}
		dnat := []expr.Any{
			&expr.Immediate{Register: 1, Data: f.to.AsSlice()},
			&expr.Immediate{Register: 2, Data: binaryutil.BigEndian.PutUint16(f.Target)},
			&expr.NAT{Type: expr.NATTypeDestNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegProtoMin: 2},
		}
		match := func(chain *nftables.Chain, first ...[]expr.Any) {
			var all []expr.Any
			for _, e := range append(first, to, l4proto(protoNumber(f.Proto)), dport(f.Port), dnat) {
				all = append(all, e...)
			}
			c.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: all})
		}
		match(pre, notFromBridge)
		// A connection to 127.0.0.1 keeps its source address through
		// translation and would be dropped on its way out.
		match(out, notLoopback)
	}
	return nil
}

func protoNumber(proto string) byte {
	if proto == "udp" {
		return unix.IPPROTO_UDP
	}
	return unix.IPPROTO_TCP
}

// toAddr matches packets sent to one address.
func toAddr(a netip.Addr) []expr.Any {
	b := a.As4()
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b[:]},
	}
}

// The bit of a connection's conntrack status that says its destination was
// translated (IPS_DST_NAT).
const ctStatusDNAT = 1 << 5

// Matching the first bytes of the name is how nft's "zelie*" works.
var (
	fromBridge = []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("zelie")},
	}
	toBridge = []expr.Any{
		&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte("zelie")},
	}
	notFromBridge = []expr.Any{
		&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte("zelie")},
	}
	// toLocal matches packets sent to an address of the host itself.
	toLocal = []expr.Any{
		&expr.Fib{Register: 1, FlagDADDR: true, ResultADDRTYPE: true},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.NativeEndian.PutUint32(unix.RTN_LOCAL)},
	}
	notLoopback = []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: []byte{255, 0, 0, 0}, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{127, 0, 0, 0}},
	}
	// dnatted matches connections whose destination was translated.
	dnatted = []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATUS},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(ctStatusDNAT),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
	}
	established    = ctEstablished(expr.CmpOpNeq)
	notEstablished = ctEstablished(expr.CmpOpEq)

	ipv4 = []expr.Any{
		&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{unix.NFPROTO_IPV4}},
	}
	toContainerRange = func() []expr.Any {
		prefix := NetworkRange.Masked()
		addr := prefix.Addr().As4()
		mask := [4]byte{}
		for i := range prefix.Bits() {
			mask[i/8] |= 0x80 >> (i % 8)
		}
		return []expr.Any{
			&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: 16, Len: 4},
			&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask[:], Xor: []byte{0, 0, 0, 0}},
			&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: addr[:]},
		}
	}()
)

// ctEstablished matches packets of connections that were already going (or
// are related to one) when op is not-equal, and the rest when it is equal.
func ctEstablished(op expr.CmpOp) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: op, Register: 1, Data: []byte{0, 0, 0, 0}},
	}
}

func l4proto(p byte) []expr.Any {
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: []byte{p}},
	}
}

func dport(p uint16) []expr.Any {
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseTransportHeader, Offset: 2, Len: 2},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: binaryutil.BigEndian.PutUint16(p)},
	}
}

// ifname matches a whole interface name, not just its start.
func ifname(key expr.MetaKey, name string) []expr.Any {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return []expr.Any{
		&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: b},
	}
}

// deleteLink removes a network interface, such as a network's bridge. One
// that is already gone is not an error.
func deleteLink(name string) error {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return nil
	}
	c, err := netlink.Dial(unix.NETLINK_ROUTE, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	// struct ifinfomsg: family, padding, type, index, flags, change.
	msg := make([]byte, 16)
	binary.NativeEndian.PutUint32(msg[4:], uint32(ifi.Index))
	_, err = c.Execute(netlink.Message{
		Header: netlink.Header{Type: unix.RTM_DELLINK, Flags: netlink.Request | netlink.Acknowledge},
		Data:   msg,
	})
	if err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}

// dropStaleForwards deletes the tracked flows that do not follow the forwards
// (see dropStaleFlows).
func dropStaleForwards(old, current []portMap) error {
	local, addrErr := hostAddrs()
	c, err := netlink.Dial(unix.NETLINK_NETFILTER, nil)
	if err != nil {
		return err
	}
	defer c.Close()
	// Without the host's addresses the flows that were translated are still
	// found; the error is reported all the same.
	return errors.Join(addrErr, dropStaleFlows(c, local, old, current))
}
