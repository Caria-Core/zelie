package engine

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/mdlayher/netlink"
)

// fakeTracker holds tracked flows and answers the way the kernel does: a dump
// lists them all, a delete takes one out by its original tuple.
type fakeTracker struct {
	flows   []flow
	deleted []tuple
	calls   int
	failOn  netip.Addr // a delete of a flow from this client fails
	failErr error
}

func (f *fakeTracker) Execute(m netlink.Message) ([]netlink.Message, error) {
	f.calls++
	switch m.Header.Type {
	case ctMessageType(ctnlMsgGet):
		var out []netlink.Message
		for _, fl := range f.flows {
			data := append([]byte{afInet, 0, 0, 0}, encodeTuple(ctaTupleOrig, fl.orig)...)
			data = append(data, encodeTuple(ctaTupleReply, fl.reply)...)
			out = append(out, netlink.Message{Header: netlink.Header{Type: ctMessageType(ctnlMsgNew)}, Data: data})
		}
		return out, nil
	case ctMessageType(ctnlMsgDelete):
		fl, err := parseFlow(m)
		if err != nil {
			return nil, err
		}
		if fl.orig.src == f.failOn {
			return nil, f.failErr
		}
		f.deleted = append(f.deleted, fl.orig)
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected message type %v", m.Header.Type)
}

func ip(s string) netip.Addr { return netip.MustParseAddr(s) }

// udpFlow is a client's flow to a forwarded port of the host, translated to a
// container.
func udpFlow(client string, clientPort uint16, host string, port uint16, container string, target uint16) flow {
	return flow{
		orig:  tuple{src: ip(client), dst: ip(host), srcPort: clientPort, dstPort: port, proto: protoUDP},
		reply: tuple{src: ip(container), dst: ip(client), srcPort: target, dstPort: clientPort, proto: protoUDP},
	}
}

// plainFlow is a flow nothing translated: the reply comes from where the
// packets went.
func plainFlow(client string, clientPort uint16, host string, port uint16) flow {
	return flow{
		orig:  tuple{src: ip(client), dst: ip(host), srcPort: clientPort, dstPort: port, proto: protoUDP},
		reply: tuple{src: ip(host), dst: ip(client), srcPort: port, dstPort: clientPort, proto: protoUDP},
	}
}

func forwardTo(f Forward, container string) portMap { return portMap{f, ip(container)} }

// hostAddresses are the addresses of the host in these tests: a public one,
// another, and the gateway of a container network.
var hostAddresses = []netip.Addr{ip("127.0.0.1"), ip("192.0.2.1"), ip("192.0.2.9"), ip("10.210.3.1")}

func TestStaleFlowsAreDropped(t *testing.T) {
	game := Forward{Port: 27015, Proto: "udp", Target: 27015}
	oldGame := forwardTo(game, "10.210.3.2")
	newGame := forwardTo(game, "10.210.3.3")
	gone := forwardTo(Forward{Port: 27020, Proto: "udp", Target: 27020}, "10.210.4.2")
	onOneAddress := forwardTo(Forward{IP: ip("192.0.2.1"), Port: 27030, Proto: "udp", Target: 27030}, "10.210.5.2")

	tcp := udpFlow("198.51.100.7", 50007, "192.0.2.1", 27015, "10.210.3.2", 27015)
	tcp.orig.proto = 6
	cases := []struct {
		why  string
		flow flow
		drop bool
	}{
		{"still on the old container", udpFlow("198.51.100.1", 50001, "192.0.2.1", 27015, "10.210.3.2", 27015), true},
		{"already on the new one", udpFlow("198.51.100.2", 50002, "192.0.2.1", 27015, "10.210.3.3", 27015), false},
		{"some other port", udpFlow("198.51.100.3", 50003, "192.0.2.1", 27016, "10.210.9.9", 27016), false},
		{"TCP ends with its container", tcp, false},
		{"not to a container", udpFlow("198.51.100.4", 50004, "192.0.2.1", 27015, "203.0.113.5", 27015), false},
		{"a forward that was removed", udpFlow("198.51.100.5", 50005, "192.0.2.1", 27020, "10.210.4.2", 27020), true},
		{"another host address than the forward's", udpFlow("198.51.100.6", 50006, "192.0.2.9", 27030, "10.210.5.2", 27030), false},
		{"the forward's address, old container", udpFlow("198.51.100.8", 50008, "192.0.2.1", 27030, "10.210.5.3", 27030), true},
		{"one container to another, not translated", plainFlow("10.210.7.2", 50009, "10.210.7.3", 27015), false},

		// A player's retries while the game restarts: no forward existed, the
		// host took the packets, and the entry stays as the host's.
		{"a player's flow from the restart gap", plainFlow("198.51.100.1", 50011, "192.0.2.1", 27015), true},
		{"a gap flow to another host address", plainFlow("198.51.100.1", 50012, "192.0.2.9", 27015), true},
		{"a gap flow from the host itself", plainFlow("10.210.3.1", 50013, "10.210.3.1", 27015), true},
		{"a gap flow to the forward's own address", plainFlow("198.51.100.1", 50014, "192.0.2.1", 27030), true},
		{"a gap flow to another address than the forward's", plainFlow("198.51.100.1", 50015, "192.0.2.9", 27030), false},
		{"a flow the host only routes", plainFlow("198.51.100.1", 50016, "203.0.113.5", 27015), false},
		{"the host's own loopback", plainFlow("127.0.0.1", 50017, "127.0.0.1", 27015), false},
		{"a container reaching the host's address", plainFlow("10.210.3.2", 50018, "192.0.2.1", 27015), false},
		{"a port without a forward", plainFlow("198.51.100.1", 50019, "192.0.2.1", 27016), false},
		{"a port whose forward was removed", plainFlow("198.51.100.1", 50020, "192.0.2.1", 27020), false},
	}
	tracker := &fakeTracker{}
	var want []tuple
	for _, c := range cases {
		tracker.flows = append(tracker.flows, c.flow)
		if c.drop {
			want = append(want, c.flow.orig)
		}
	}
	if err := dropStaleFlows(tracker, hostAddresses, []portMap{oldGame, gone, onOneAddress}, []portMap{newGame, onOneAddress}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(tracker.deleted) != fmt.Sprint(want) {
		var got []string
		for _, d := range tracker.deleted {
			got = append(got, fmt.Sprintf("%v:%d", d.src, d.srcPort))
		}
		t.Errorf("deleted %v", got)
		for _, c := range cases {
			if has := slices.Contains(tracker.deleted, c.flow.orig); has != c.drop {
				t.Errorf("%s: deleted %v, want %v", c.why, has, c.drop)
			}
		}
	}
}

func TestPlayerFlowFromTheRestartGapEndsWhenTheForwardReturns(t *testing.T) {
	// A game restart in three steps, as the firewall sees them: the server
	// runs, the forward is taken away while it stops, and a forward to the new
	// container comes back. A player's retries in between start a flow that
	// goes to the host.
	game := Forward{Port: 27015, Proto: "udp", Target: 27015}
	running := []portMap{forwardTo(game, "10.210.3.2")}
	before := udpFlow("198.51.100.1", 50001, "192.0.2.1", 27015, "10.210.3.2", 27015)
	gap := plainFlow("198.51.100.2", 50002, "192.0.2.1", 27015)
	tracker := &fakeTracker{flows: []flow{before, gap}}

	if err := dropStaleFlows(tracker, hostAddresses, running, nil); err != nil {
		t.Fatal(err)
	}
	if len(tracker.deleted) != 1 || tracker.deleted[0] != before.orig {
		t.Fatalf("when the forward goes: deleted %v, want only the translated flow", tracker.deleted)
	}
	tracker.deleted = nil
	back := []portMap{forwardTo(game, "10.210.3.3")}
	if err := dropStaleFlows(tracker, hostAddresses, nil, back); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(tracker.deleted, gap.orig) {
		t.Errorf("when the forward returns: deleted %v, the flow of the gap is still there", tracker.deleted)
	}
}

func TestFlowsAreLeftAloneWithoutUDPForwards(t *testing.T) {
	tracker := &fakeTracker{flows: []flow{udpFlow("198.51.100.1", 50001, "192.0.2.1", 27015, "10.210.3.2", 27015)}}
	tcp := forwardTo(Forward{Port: 25565, Proto: "tcp", Target: 25565}, "10.210.3.2")
	if err := dropStaleFlows(tracker, hostAddresses, []portMap{tcp}, nil); err != nil {
		t.Fatal(err)
	}
	if tracker.calls != 0 {
		t.Errorf("the connection tracker was asked %d times for TCP forwards", tracker.calls)
	}
}

func TestOnlyFlowsOfTheForwardAreDeletedAtStart(t *testing.T) {
	// As the core starts it knows no earlier forwards: flows to the current
	// target stay and the ones to any other container address go.
	game := forwardTo(Forward{Port: 27015, Proto: "udp", Target: 27015}, "10.210.3.3")
	tracker := &fakeTracker{flows: []flow{
		udpFlow("198.51.100.1", 50001, "192.0.2.1", 27015, "10.210.3.2", 27015),
		udpFlow("198.51.100.2", 50002, "192.0.2.1", 27015, "10.210.3.3", 27015),
	}}
	if err := dropStaleFlows(tracker, hostAddresses, nil, []portMap{game}); err != nil {
		t.Fatal(err)
	}
	if len(tracker.deleted) != 1 || tracker.deleted[0] != tracker.flows[0].orig {
		t.Errorf("deleted %v", tracker.deleted)
	}
}

func TestFlowThatEndedMeanwhileIsNoError(t *testing.T) {
	game := forwardTo(Forward{Port: 27015, Proto: "udp", Target: 27015}, "10.210.3.2")
	stale := udpFlow("198.51.100.1", 50001, "192.0.2.1", 27015, "10.210.3.2", 27015)
	other := udpFlow("198.51.100.2", 50002, "192.0.2.1", 27015, "10.210.3.2", 27015)

	tracker := &fakeTracker{flows: []flow{stale, other}, failOn: stale.orig.src, failErr: &netlink.OpError{Op: "receive", Err: syscall.ENOENT}}
	if err := dropStaleFlows(tracker, hostAddresses, []portMap{game}, nil); err != nil {
		t.Errorf("a flow that was already gone: %v", err)
	}
	if len(tracker.deleted) != 1 || tracker.deleted[0] != other.orig {
		t.Errorf("the other flow was not deleted: %v", tracker.deleted)
	}

	// Any other failure is reported, after the rest was tried.
	tracker = &fakeTracker{flows: []flow{stale, other}, failOn: stale.orig.src, failErr: &netlink.OpError{Op: "receive", Err: os.ErrPermission}}
	if err := dropStaleFlows(tracker, hostAddresses, []portMap{game}, nil); !errors.Is(err, os.ErrPermission) {
		t.Errorf("a refused delete: %v", err)
	}
	if len(tracker.deleted) != 1 {
		t.Errorf("deleted %v after a failure", tracker.deleted)
	}
}

func TestDeleteMessageNamesTheOriginalTuple(t *testing.T) {
	orig := tuple{src: ip("198.51.100.1"), dst: ip("192.0.2.1"), srcPort: 50001, dstPort: 27015, proto: protoUDP}
	m := deleteFlow(orig)
	if m.Header.Type != 0x0102 || m.Header.Flags != netlink.Request|netlink.Acknowledge {
		t.Errorf("header %+v: want conntrack delete, requested and acknowledged", m.Header)
	}
	fl, err := parseFlow(m)
	if err != nil || fl.orig != orig {
		t.Errorf("tuple %+v, %v", fl.orig, err)
	}
	// Bytes as the kernel reads them: an nfgenmsg for IPv4, then the nested
	// original tuple with its addresses and big-endian ports.
	want := []byte{
		2, 0, 0, 0, // AF_INET, version 0, resource 0
		0x34, 0x00, 0x01, 0x80, // CTA_TUPLE_ORIG, nested
		0x14, 0x00, 0x01, 0x80, // CTA_TUPLE_IP, nested
		0x08, 0x00, 0x01, 0x00, 198, 51, 100, 1,
		0x08, 0x00, 0x02, 0x00, 192, 0, 2, 1,
		0x1c, 0x00, 0x02, 0x80, // CTA_TUPLE_PROTO, nested
		0x05, 0x00, 0x01, 0x00, 17, 0, 0, 0,
		0x06, 0x00, 0x02, 0x00, 0xc3, 0x51, 0, 0,
		0x06, 0x00, 0x03, 0x00, 0x69, 0x87, 0, 0,
	}
	if string(m.Data) != string(want) {
		t.Errorf("message data\n% x\nwant\n% x", m.Data, want)
	}
}
