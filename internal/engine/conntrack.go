package engine

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"

	"github.com/mdlayher/netlink"
)

// A forward is translated when a flow starts: the connection tracker keeps
// the container address it chose, and every later packet of the flow follows
// it. That is right for TCP, whose connections end with their container. A
// player's UDP socket never ends, and its flow outlives the container, so
// after a restart the game server's new address would go unused until the
// player stays silent for the flow's timeout. The same holds for a flow that
// started while the forward was missing, as it is while a game restarts: the
// host took the packets itself, the tracker kept them untranslated, and the
// player's retries keep that entry alive. The flows are deleted when the
// forward changes.

// Parts of the connection tracker's netlink interface, from the kernel's
// nfnetlink_conntrack.h.
const (
	nfnlSubsysCtnetlink = 1
	ctnlMsgNew          = 0
	ctnlMsgGet          = 1
	ctnlMsgDelete       = 2

	ctaTupleOrig  = 1
	ctaTupleReply = 2

	ctaTupleIP    = 1
	ctaTupleProto = 2

	ctaIPv4Src = 1
	ctaIPv4Dst = 2

	ctaProtoNum     = 1
	ctaProtoSrcPort = 2
	ctaProtoDstPort = 3

	afInet   = 2
	protoUDP = 17
)

func ctMessageType(msg uint16) netlink.HeaderType {
	return netlink.HeaderType(nfnlSubsysCtnetlink<<8 | msg)
}

// ctConn is the part of a netlink connection to the connection tracker that
// is used here.
type ctConn interface {
	Execute(netlink.Message) ([]netlink.Message, error)
}

// tuple is one direction of a flow.
type tuple struct {
	src, dst         netip.Addr
	srcPort, dstPort uint16
	proto            uint8
}

// flow is a tracked connection. orig is what the sender sent, reply what the
// receiver answers with: after a translation of the destination, the reply
// comes from the address the packets were sent on to.
type flow struct{ orig, reply tuple }

// dropStaleFlows deletes the tracked UDP flows that reach a forwarded port
// without going where the forward points now. old is what was installed
// before and may be empty; current is what is installed now; local holds the
// host's own addresses. These flows are deleted:
//   - Flows translated to a container address, for a port that an old or a
//     current forward covers, unless a current forward leads there.
//   - Flows to a port of a current forward that were never translated: they
//     started while the forward was missing and went to the host itself. Only
//     those sent to an address the forward covers count, and not the ones a
//     container sent, which the forward never applies to.
//
// The kernel cannot filter a dump of its flows, so all IPv4 flows are read and
// the ones that match are deleted one by one.
func dropStaleFlows(c ctConn, local []netip.Addr, old, current []portMap) error {
	if !hasUDP(old) && !hasUDP(current) {
		return nil
	}
	msgs, err := c.Execute(netlink.Message{
		Header: netlink.Header{Type: ctMessageType(ctnlMsgGet), Flags: netlink.Request | netlink.Dump},
		Data:   []byte{afInet, 0, 0, 0},
	})
	if err != nil {
		return fmt.Errorf("list tracked connections: %w", err)
	}
	var errs []error
	for _, m := range msgs {
		fl, err := parseFlow(m)
		if err != nil {
			return fmt.Errorf("read tracked connection: %w", err)
		}
		if !fl.stale(local, old, current) {
			continue
		}
		// A flow that ended meanwhile is gone, which is what was wanted.
		if _, err := c.Execute(deleteFlow(fl.orig)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// stale reports whether dropStaleFlows deletes the flow.
func (f flow) stale(local []netip.Addr, old, current []portMap) bool {
	if f.orig.proto != protoUDP {
		return false
	}
	reaches := func(g Forward) bool {
		return f.orig.dstPort == g.Port && (g.everyAddress() || f.orig.dst == g.IP)
	}
	udp := func(list []portMap, match func(portMap) bool) bool {
		return slices.ContainsFunc(list, func(g portMap) bool { return g.Proto == "udp" && reaches(g.Forward) && match(g) })
	}
	if udp(current, func(g portMap) bool { return f.reply.src == g.to && f.reply.srcPort == g.Target }) {
		return false
	}
	always := func(portMap) bool { return true }
	if f.reply.src != f.orig.dst {
		// Translated. One that leads to a container is the forward's; one
		// sent on to anywhere else is some other rule's.
		return NetworkRange.Contains(f.reply.src) && (udp(old, always) || udp(current, always))
	}
	// Not translated: the host took it itself, or only routes it on. The
	// forward never applies to what a container sends or to the loopback.
	fromContainer := NetworkRange.Contains(f.orig.src) && !slices.Contains(local, f.orig.src)
	if f.orig.dst.IsLoopback() || fromContainer {
		return false
	}
	// A forward with an address covers that one; one without covers every
	// address of the host, and a flow to any other is only passing through.
	return udp(current, func(g portMap) bool { return !g.everyAddress() || slices.Contains(local, f.orig.dst) })
}

// hostAddrs lists the IPv4 addresses of the host's own interfaces.
func hostAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	var out []netip.Addr
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			if ip, ok := netip.AddrFromSlice(n.IP.To4()); ok {
				out = append(out, ip)
			}
		}
	}
	return out, nil
}

func parseFlow(m netlink.Message) (flow, error) {
	var f flow
	if len(m.Data) < 4 {
		return f, errors.New("short message")
	}
	ad, err := netlink.NewAttributeDecoder(m.Data[4:]) // after the nfgenmsg header
	if err != nil {
		return f, err
	}
	ad.ByteOrder = binary.BigEndian
	for ad.Next() {
		switch ad.Type() {
		case ctaTupleOrig:
			ad.Nested(func(nad *netlink.AttributeDecoder) (err error) { f.orig, err = parseTuple(nad); return })
		case ctaTupleReply:
			ad.Nested(func(nad *netlink.AttributeDecoder) (err error) { f.reply, err = parseTuple(nad); return })
		}
	}
	return f, ad.Err()
}

func parseTuple(ad *netlink.AttributeDecoder) (tuple, error) {
	var t tuple
	for ad.Next() {
		switch ad.Type() {
		case ctaTupleIP:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					switch nad.Type() {
					case ctaIPv4Src:
						t.src, _ = netip.AddrFromSlice(nad.Bytes())
					case ctaIPv4Dst:
						t.dst, _ = netip.AddrFromSlice(nad.Bytes())
					}
				}
				return nil
			})
		case ctaTupleProto:
			ad.Nested(func(nad *netlink.AttributeDecoder) error {
				for nad.Next() {
					switch nad.Type() {
					case ctaProtoNum:
						t.proto = nad.Uint8()
					case ctaProtoSrcPort:
						t.srcPort = nad.Uint16()
					case ctaProtoDstPort:
						t.dstPort = nad.Uint16()
					}
				}
				return nil
			})
		}
	}
	return t, ad.Err()
}

// deleteFlow asks for the flow with this original tuple to be deleted.
func deleteFlow(orig tuple) netlink.Message {
	return netlink.Message{
		Header: netlink.Header{Type: ctMessageType(ctnlMsgDelete), Flags: netlink.Request | netlink.Acknowledge},
		Data:   append([]byte{afInet, 0, 0, 0}, encodeTuple(ctaTupleOrig, orig)...),
	}
}

func encodeTuple(attr uint16, t tuple) []byte {
	ae := netlink.NewAttributeEncoder()
	ae.ByteOrder = binary.BigEndian
	ae.Nested(attr, func(nae *netlink.AttributeEncoder) error {
		nae.Nested(ctaTupleIP, func(n *netlink.AttributeEncoder) error {
			src, dst := t.src.As4(), t.dst.As4()
			n.Bytes(ctaIPv4Src, src[:])
			n.Bytes(ctaIPv4Dst, dst[:])
			return nil
		})
		nae.Nested(ctaTupleProto, func(n *netlink.AttributeEncoder) error {
			n.Uint8(ctaProtoNum, t.proto)
			n.Uint16(ctaProtoSrcPort, t.srcPort)
			n.Uint16(ctaProtoDstPort, t.dstPort)
			return nil
		})
		return nil
	})
	// Encode fails only for values it cannot encode; these are fixed size.
	b, _ := ae.Encode()
	return b
}
