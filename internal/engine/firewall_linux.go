package engine

import (
	"encoding/binary"
	"fmt"
	"net"

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
//   - Containers on different networks cannot reach each other, except
//     through the openings links make: one address to another, on one TCP
//     port.
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
	rule(input, fromBridge, drop)

	rule(forward, fromBridge, toBridge, established, accept)
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

	if err := c.Flush(); err != nil {
		return fmt.Errorf("install host firewall rules: %w", err)
	}
	return nil
}

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
	established = []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{
			SourceRegister: 1,
			DestRegister:   1,
			Len:            4,
			Mask:           binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED),
			Xor:            binaryutil.NativeEndian.PutUint32(0),
		},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: []byte{0, 0, 0, 0}},
	}
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
