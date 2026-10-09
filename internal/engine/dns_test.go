package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestDNSAnswers(t *testing.T) {
	web := netip.MustParseAddr("10.210.0.2")
	pg := netip.MustParseAddr("10.210.1.2")
	upstream := fakeUpstream(t)
	d := newDNSServer(func(_ context.Context, from netip.Addr, name string) ([]netip.Addr, bool) {
		if from == web && name == "db" {
			return []netip.Addr{pg}, true
		}
		if from == web && name == "cache" {
			return nil, true // linked, nothing running
		}
		return nil, false
	}, []string{upstream})
	ask := func(from netip.Addr, name string, qtype uint16) *dns.Msg {
		req := new(dns.Msg).SetQuestion(dns.Fqdn(name), qtype)
		return d.answer(context.Background(), from, req, false)
	}

	r := ask(web, "DB", dns.TypeA)
	if r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != pg.String() {
		t.Errorf("linked name: %v", r)
	}
	if r := ask(web, "db", dns.TypeAAAA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("AAAA of a linked name should be empty: %v", r)
	}
	if r := ask(web, "cache", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 0 {
		t.Errorf("linked but stopped: %v", r)
	}
	// Other apps, and names that are not links, find nothing, and the
	// question never leaves the machine.
	for _, q := range []struct {
		from netip.Addr
		name string
	}{{pg, "db"}, {web, "redis"}} {
		if r := ask(q.from, q.name, dns.TypeA); r.Rcode != dns.RcodeNameError {
			t.Errorf("%s from %s: %v", q.name, q.from, r)
		}
	}
	if r := ask(web, "example.com", dns.TypeA); r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
		t.Errorf("forwarded name: %v", r)
	}
}

// fakeUpstream answers every question with 192.0.2.1.
func fakeUpstream(t *testing.T) string {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		m := new(dns.Msg).SetReply(req)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: req.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.ParseIP("192.0.2.1"),
		})
		w.WriteMsg(m)
	})}
	go s.ActivateAndServe()
	t.Cleanup(func() { s.Shutdown() })
	return pc.LocalAddr().String()
}

// recorder is the writer of one question; it keeps the answer.
type recorder struct {
	dns.ResponseWriter
	from net.Addr
	mu   sync.Mutex
	got  *dns.Msg
}

func (r *recorder) RemoteAddr() net.Addr { return r.from }

func (r *recorder) WriteMsg(m *dns.Msg) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = m
	return nil
}

func (r *recorder) answer() *dns.Msg {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got
}

func udpFrom(ip string) net.Addr { return &net.UDPAddr{IP: net.ParseIP(ip), Port: 40000} }

// slowUpstream is a DNS server that holds every question until release is
// closed, as one does that is out of reach. waiting counts the questions it
// holds.
func slowUpstream(t *testing.T) (addr string, waiting *atomic.Int32, release chan struct{}) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	waiting, release = new(atomic.Int32), make(chan struct{})
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		waiting.Add(1)
		<-release
		w.WriteMsg(new(dns.Msg).SetReply(req))
	})}
	go s.ActivateAndServe()
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		s.Shutdown()
	})
	return pc.LocalAddr().String(), waiting, release
}

func TestOneContainerCannotTieUpTheDNSServer(t *testing.T) {
	web := netip.MustParseAddr("10.210.0.2")
	upstream, waiting, release := slowUpstream(t)
	d := newDNSServer(func(_ context.Context, from netip.Addr, name string) ([]netip.Addr, bool) {
		return []netip.Addr{web}, true
	}, []string{upstream})
	d.ctx = context.Background()
	ask := func(from, name string) *recorder {
		w := &recorder{from: udpFrom(from)}
		d.serve(w, new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA))
		return w
	}
	answered := func(w *recorder, rcode int) bool {
		r := w.answer()
		return r != nil && r.Rcode == rcode
	}

	var wg sync.WaitGroup
	for range dnsMaxForwardsPerNet {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ask("10.210.0.2", "slow.example.com")
		}()
	}
	for waiting.Load() < dnsMaxForwardsPerNet {
		time.Sleep(time.Millisecond)
	}

	// The container is at its limit: it is told so at once, without a
	// goroutine or a socket held for it.
	over := make(chan *recorder, 1)
	go func() { over <- ask("10.210.0.2", "slow.example.com") }()
	select {
	case w := <-over:
		if !answered(w, dns.RcodeServerFailure) {
			t.Errorf("a question past the limit: %v", w.answer())
		}
	case <-time.After(2 * time.Second):
		t.Error("a question past the limit was taken up instead of turned away")
	}
	// The names of its links are answered all the same, and so are another
	// container's.
	if w := ask("10.210.0.2", "db"); !answered(w, dns.RcodeSuccess) {
		t.Errorf("a link name from the container at its limit: %v", w.answer())
	}
	if w := ask("10.210.1.2", "db"); !answered(w, dns.RcodeSuccess) {
		t.Errorf("a link name from another container: %v", w.answer())
	}

	close(release)
	wg.Wait()
	if w := ask("10.210.0.2", "slow.example.com"); !answered(w, dns.RcodeSuccess) {
		t.Errorf("a question after the others were answered: %v", w.answer())
	}
	for name, l := range map[string]*limiter{"lookups": d.lookups, "forwards": d.forwards} {
		if l.total != 0 || len(l.by) != 0 {
			t.Errorf("%s: places still taken: %d, %v", name, l.total, l.by)
		}
	}
}

// Containers that have the host's DNS servers busy must not take the names of
// links from the rest.
func TestFullForwardingDoesNotStopLinkNames(t *testing.T) {
	web := netip.MustParseAddr("10.210.0.2")
	upstream, waiting, release := slowUpstream(t)
	d := newDNSServer(func(_ context.Context, from netip.Addr, name string) ([]netip.Addr, bool) {
		return []netip.Addr{web}, true
	}, []string{upstream})
	d.ctx = context.Background()
	// Three containers fill all that may be passed on.
	const each, sources = 2, 3
	d.forwards = newLimiter(each*sources, each)
	ask := func(from, name string) *dns.Msg {
		w := &recorder{from: udpFrom(from)}
		d.serve(w, new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA))
		return w.answer()
	}

	var wg sync.WaitGroup
	for i := range sources {
		for range each {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ask(fmt.Sprintf("10.210.%d.2", i), "slow.example.com")
			}()
		}
	}
	for waiting.Load() < each*sources {
		time.Sleep(time.Millisecond)
	}

	if r := ask("10.210.9.2", "example.com"); r == nil || r.Rcode != dns.RcodeServerFailure {
		t.Errorf("a name to pass on, with the limit reached: %v", r)
	}
	for _, from := range []string{"10.210.9.2", "10.210.0.2", "10.210.1.2"} {
		if r := ask(from, "db"); r == nil || r.Rcode != dns.RcodeSuccess || len(r.Answer) != 1 {
			t.Errorf("the link name from %s while the limit is reached: %v", from, r)
		}
	}
	close(release)
	wg.Wait()
}

// An app with many containers has one share of the names that may be passed
// on, as the network is the app's. Counted for each container, eight of them
// would take all that the other apps may use.
func TestOneNetworkCannotFillTheForwardingSlots(t *testing.T) {
	web := netip.MustParseAddr("10.210.0.2")
	upstream, waiting, release := slowUpstream(t)
	d := newDNSServer(func(_ context.Context, from netip.Addr, name string) ([]netip.Addr, bool) {
		return []netip.Addr{web}, true
	}, []string{upstream})
	d.ctx = context.Background()
	const perNet, total, containers = 2, 6, 4
	d.forwards = newLimiter(total, perNet)
	ask := func(from, name string) *dns.Msg {
		w := &recorder{from: udpFrom(from)}
		d.serve(w, new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA))
		return w.answer()
	}

	// Four containers of one app, each asking for more than its share.
	var wg sync.WaitGroup
	var turnedAway atomic.Int32
	for i := range containers {
		for range perNet {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r := ask(fmt.Sprintf("10.210.5.%d", i+2), "slow.example.com"); r != nil && r.Rcode == dns.RcodeServerFailure {
					turnedAway.Add(1)
				}
			}()
		}
	}
	until := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				close(release)
				t.Fatalf("waited for %s: %d questions are at the upstream, %d were turned away", what, waiting.Load(), turnedAway.Load())
			}
			time.Sleep(time.Millisecond)
		}
	}
	until("the share of one network to be taken up", func() bool { return waiting.Load() >= perNet })
	until("the rest to be turned away", func() bool { return turnedAway.Load() >= containers*perNet-perNet })

	// All that the app got is its share. Another app's name is still passed on.
	if n := waiting.Load(); n != perNet {
		t.Errorf("%d questions reached the upstream for one network, want %d", n, perNet)
	}
	other := make(chan *dns.Msg, 1)
	go func() { other <- ask("10.210.6.2", "example.com") }()
	until("a name of another network to be passed on", func() bool { return waiting.Load() > perNet })
	close(release)
	wg.Wait()
	if r := <-other; r == nil || r.Rcode != dns.RcodeSuccess {
		t.Errorf("a name from another network, with one network at its limit: %v", r)
	}
}

func TestNetworkOf(t *testing.T) {
	for in, want := range map[string]string{
		"10.210.5.2":   "10.210.5.0",
		"10.210.5.1":   "10.210.5.0",
		"10.210.255.9": "10.210.255.0",
		"10.211.5.2":   "10.211.5.2", // not a container network
		"127.0.0.1":    "127.0.0.1",
	} {
		if got := networkOf(netip.MustParseAddr(in)); got != netip.MustParseAddr(want) {
			t.Errorf("networkOf(%s) = %s, want %s", in, got, want)
		}
	}
}

// fakeListener hands out connections that claim to come from the addresses it
// is given.
type fakeListener struct {
	net.Listener
	from chan string
}

func (l *fakeListener) Accept() (net.Conn, error) {
	return &fakeConn{from: &net.TCPAddr{IP: net.ParseIP(<-l.from), Port: 40000}}, nil
}

type fakeConn struct {
	net.Conn
	from net.Addr
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.from }
func (c *fakeConn) Close() error         { return nil }

// The TCP connections an app may hold are counted for its network too.
func TestDNSConnectionsAreLimitedForANetwork(t *testing.T) {
	from := make(chan string, 8)
	l := &limitedListener{Listener: &fakeListener{from: from}, conns: newLimiter(0, 2)}
	// Three containers of one app, then one of another.
	for _, ip := range []string{"10.210.7.2", "10.210.7.3", "10.210.7.4", "10.210.8.2"} {
		from <- ip
	}
	first, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	second, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// The third is turned away and the next one is taken, from the other app.
	third, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if got := third.RemoteAddr().String(); got != "10.210.8.2:40000" {
		t.Errorf("the connection after the two of the first app came from %s, want the other app's", got)
	}
	first.Close()
	second.Close()
	third.Close()
}

func TestLimiter(t *testing.T) {
	a, b, c := netip.MustParseAddr("10.210.0.2"), netip.MustParseAddr("10.210.1.2"), netip.MustParseAddr("10.210.2.2")
	l := newLimiter(3, 2)
	if !l.acquire(a) || !l.acquire(a) {
		t.Fatal("the first two places of one address were refused")
	}
	if l.acquire(a) {
		t.Error("a third place for one address")
	}
	if !l.acquire(b) {
		t.Fatal("a place for another address was refused")
	}
	if l.acquire(c) {
		t.Error("a place past the total")
	}
	l.release(a)
	if !l.acquire(c) {
		t.Error("a freed place was not given out")
	}

	// Zero leaves a limit out.
	each := newLimiter(0, 2)
	for _, from := range []netip.Addr{a, b, c} {
		if !each.acquire(from) || !each.acquire(from) || each.acquire(from) {
			t.Errorf("no total, two for each: address %v", from)
		}
	}
	total := newLimiter(2, 0)
	if !total.acquire(a) || !total.acquire(a) || total.acquire(b) {
		t.Error("a total of two and none for each")
	}
}

func TestDNSConnectionsAreLimited(t *testing.T) {
	tl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &limitedListener{Listener: tl, conns: newLimiter(10, 2)}
	defer l.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp4", tl.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	dial()
	first := <-accepted
	dial()
	<-accepted

	// The third is closed at once; the server never hands it on.
	over := dial()
	over.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Errorf("the connection past the limit stayed open: %v", err)
	}
	select {
	case c := <-accepted:
		t.Errorf("accepted %v past the limit", c.RemoteAddr())
	default:
	}

	// A closed connection gives its place back, however often it is closed.
	first.Close()
	first.Close()
	dial()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Error("no connection was accepted after one closed")
	}
}

func isTimeout(err error) bool {
	ne, ok := err.(net.Error)
	return ok && ne.Timeout()
}

// freeLoopbackPort finds a port that is free for both UDP and TCP on the loopback.
func freeLoopbackPort(t *testing.T) uint16 {
	t.Helper()
	for range 50 {
		pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := pc.LocalAddr().(*net.UDPAddr).Port
		l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		pc.Close()
		if err == nil {
			l.Close()
			return uint16(port)
		}
	}
	t.Skip("no port free for UDP and TCP")
	return 0
}

// A network that is freed and made again must not leave anything behind: it
// happens with every build and every Steam check.
func TestDNSServersCanBeStoppedAndStartedAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newDNSServer(nil, nil)
	d.ctx, d.errs = ctx, make(chan error, 1)
	gateway := netip.MustParseAddr("127.0.0.1")
	// A port found free can be taken by another socket before the first
	// listen; then another one is tried.
	for try := 0; ; try++ {
		d.port = freeLoopbackPort(t)
		err := d.listen(gateway)
		if err == nil {
			d.stop(gateway)
			break
		}
		if !errors.Is(err, syscall.EADDRINUSE) || try == 4 {
			t.Fatal(err)
		}
	}

	before := runtime.NumGoroutine()
	for range 30 {
		if err := d.listen(gateway); err != nil {
			t.Fatal(err)
		}
		d.stop(gateway)
	}
	select {
	case err := <-d.errs:
		t.Fatalf("a server failed: %v", err)
	default:
	}

	// Stopped means the sockets are free again, even if the stop came right
	// after the start.
	hostport := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(d.port)))
	pc, err := net.ListenPacket("udp4", hostport)
	if err != nil {
		t.Fatalf("UDP port still taken: %v", err)
	}
	pc.Close()
	l, err := net.Listen("tcp4", hostport)
	if err != nil {
		t.Fatalf("TCP port still taken: %v", err)
	}
	l.Close()

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines before, %d after 30 starts and stops", before, runtime.NumGoroutine())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDNSServersAnswerOverUDPAndTCP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loopback := netip.MustParseAddr("127.0.0.1")
	d := newDNSServer(func(_ context.Context, from netip.Addr, name string) ([]netip.Addr, bool) {
		return []netip.Addr{netip.MustParseAddr("10.210.1.2")}, name == "db" && from == loopback
	}, nil)
	d.ctx, d.errs, d.port = ctx, make(chan error, 1), freeLoopbackPort(t)
	if err := d.listen(loopback); err != nil {
		t.Fatal(err)
	}
	defer d.stop(loopback)

	at := net.JoinHostPort("127.0.0.1", strconv.Itoa(int(d.port)))
	for _, network := range []string{"udp", "tcp"} {
		c := &dns.Client{Net: network, Timeout: 2 * time.Second}
		r, _, err := c.Exchange(new(dns.Msg).SetQuestion("db.", dns.TypeA), at)
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		if len(r.Answer) != 1 || r.Answer[0].(*dns.A).A.String() != "10.210.1.2" {
			t.Errorf("%s: %v", network, r)
		}
	}
}

func TestForwardedQuestionsAskForSmallAnswers(t *testing.T) {
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sizes := make(chan uint16, 1)
	s := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, req *dns.Msg) {
		sizes <- req.IsEdns0().UDPSize()
		w.WriteMsg(new(dns.Msg).SetReply(req))
	})}
	go s.ActivateAndServe()
	t.Cleanup(func() { s.Shutdown() })

	d := newDNSServer(nil, []string{pc.LocalAddr().String()})
	req := new(dns.Msg).SetQuestion("example.com.", dns.TypeA)
	req.SetEdns0(65000, false)
	if r := d.forward(context.Background(), netip.MustParseAddr("10.210.0.2"), req, false); r.Rcode != dns.RcodeSuccess {
		t.Fatalf("answer: %v", r)
	}
	if got := <-sizes; got != dnsMaxAnswer {
		t.Errorf("the upstream was told %d bytes, want %d", got, dnsMaxAnswer)
	}
}
