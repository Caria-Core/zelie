package engine

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// Each network has a DNS server on its gateway address. It answers the
// names of the asking app's links and passes every other name on to the
// host's own DNS servers. An address changes with every deployment, and a
// name looked up again follows it.

// linkTTL is short so a client that caches finds a new deployment quickly.
const linkTTL = 5

// What one network may keep the server busy with. A name that is passed on
// ties up a goroutine and a socket for seconds, and the server runs inside the
// root core, so a container that floods it must not be able to use up the
// core's memory or file descriptors. Past a limit a question is answered with
// a failure and a connection is closed.
//
// The limits are for a network, which is an app's /24, and not for a container
// address: an app with many containers would otherwise have as many shares as
// containers, and could fill what is left for the rest. Only the names passed
// on count towards a limit shared by all networks. The names of links are
// answered from memory, and they must not fail because other containers have
// the host's DNS servers busy.
const (
	dnsMaxLookupsPerNet  = 32
	dnsMaxForwards       = 256
	dnsMaxForwardsPerNet = 32
	dnsMaxConns          = 64
	dnsMaxConnsPerNet    = 8

	// The largest answer a forwarded question may ask for. The client
	// allocates what the asker says it can take, up to 64 KiB.
	dnsMaxAnswer = 4096
)

type dnsServer struct {
	lookup   func(ctx context.Context, from netip.Addr, name string) ([]netip.Addr, bool)
	upstream []string // host:port
	lookups  *limiter // link names being looked up
	forwards *limiter // names being passed on
	conns    *limiter // open TCP connections
	port     uint16   // 53, unless a test says otherwise

	mu        sync.Mutex
	listening map[netip.Addr]*dnsListener
	ctx       context.Context
	errs      chan error
}

// dnsListener is the UDP and TCP server on one gateway address.
type dnsListener struct {
	servers []*dns.Server
	started sync.WaitGroup // done once a server is serving, or never will
	unwatch func() bool    // ends the shutdown that waits for the core to stop
}

func newDNSServer(lookup func(context.Context, netip.Addr, string) ([]netip.Addr, bool), upstream []string) *dnsServer {
	return &dnsServer{
		lookup:   lookup,
		upstream: upstream,
		lookups:  newLimiter(0, dnsMaxLookupsPerNet),
		forwards: newLimiter(dnsMaxForwards, dnsMaxForwardsPerNet),
		conns:    newLimiter(dnsMaxConns, dnsMaxConnsPerNet),
	}
}

// shutdown stops both servers. A server asked to stop before it has started
// would refuse and then run on, so it waits for the start first.
func (l *dnsListener) shutdown() {
	l.started.Wait()
	for _, s := range l.servers {
		s.Shutdown()
	}
}

// StartDNS starts the DNS server of every network; they run until ctx is
// cancelled. Networks made later get theirs as their first container
// starts. A server that fails later reports it on the channel.
func (e *Engine) StartDNS(ctx context.Context) (<-chan error, error) {
	d := e.dns
	d.mu.Lock()
	d.ctx = ctx
	d.errs = make(chan error, 1)
	d.mu.Unlock()
	// Networks left over from before, with no container on them, go first.
	if err := e.freeUnusedNetworks(ctx); err != nil {
		return nil, err
	}
	nets, err := e.networks.all()
	if err != nil {
		return nil, err
	}
	for _, nw := range nets {
		if err := d.listen(nw.gateway()); err != nil {
			return nil, fmt.Errorf("DNS server for network %s: %w", nw.Name, err)
		}
	}
	return d.errs, nil
}

// listen starts the server on addr unless it already runs. Before ServeDNS
// it does nothing.
func (d *dnsServer) listen(addr netip.Addr) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx == nil || d.listening[addr] != nil {
		return nil
	}
	// The gateway address only exists once the network's first container
	// has started, so the servers bind to it ahead of time.
	lc := freebind()
	port := cmp.Or(d.port, 53)
	hostport := netip.AddrPortFrom(addr, port).String()
	pc, err := lc.ListenPacket(d.ctx, "udp4", hostport)
	if err != nil {
		return err
	}
	tl, err := lc.Listen(d.ctx, "tcp4", hostport)
	if err != nil {
		pc.Close()
		return err
	}
	l := &dnsListener{servers: []*dns.Server{
		{PacketConn: pc, Handler: dns.HandlerFunc(d.serve)},
		{Listener: &limitedListener{Listener: tl, conns: d.conns}, Handler: dns.HandlerFunc(d.serve), ReadTimeout: 5 * time.Second},
	}}
	l.started.Add(len(l.servers))
	for _, s := range l.servers {
		ready := sync.OnceFunc(l.started.Done)
		s.NotifyStartedFunc = ready
		go func() {
			err := s.ActivateAndServe()
			ready()
			if err != nil && d.ctx.Err() == nil {
				select {
				case d.errs <- err:
				default:
				}
			}
		}()
	}
	// The servers end with the core. Once one is stopped for its network,
	// nothing is left waiting for that.
	l.unwatch = context.AfterFunc(d.ctx, l.shutdown)
	if d.listening == nil {
		d.listening = map[netip.Addr]*dnsListener{}
	}
	d.listening[addr] = l
	return nil
}

// stop shuts the server on addr down, for a network that is gone.
func (d *dnsServer) stop(addr netip.Addr) {
	d.mu.Lock()
	l := d.listening[addr]
	delete(d.listening, addr)
	d.mu.Unlock()
	if l == nil {
		return
	}
	l.unwatch()
	l.shutdown()
}

func (d *dnsServer) serve(w dns.ResponseWriter, req *dns.Msg) {
	from := addrOf(w.RemoteAddr())
	_, tcp := w.RemoteAddr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	w.WriteMsg(d.answer(ctx, from, req, tcp))
}

// addrOf is the IP address of a peer, without the port.
func addrOf(a net.Addr) netip.Addr {
	ap, _ := netip.ParseAddrPort(a.String())
	return ap.Addr().Unmap()
}

// networkOf is what the limits are shared by: the first address of the /24 a
// container network has. An address outside the container networks has a
// share of its own.
func networkOf(a netip.Addr) netip.Addr {
	if a.Is4() && NetworkRange.Contains(a) {
		p, _ := a.Prefix(24)
		return p.Addr()
	}
	return a
}

func (d *dnsServer) answer(ctx context.Context, from netip.Addr, req *dns.Msg, tcp bool) *dns.Msg {
	m := new(dns.Msg)
	if req.Opcode != dns.OpcodeQuery || len(req.Question) != 1 {
		return m.SetRcode(req, dns.RcodeNotImplemented)
	}
	q := req.Question[0]
	name := strings.ToLower(strings.TrimSuffix(q.Name, "."))
	// A name without a dot is always one of ours. Passed on, it would tell
	// the host's DNS servers what the apps look for.
	if strings.Contains(name, ".") {
		return d.forward(ctx, from, req, tcp)
	}
	nw := networkOf(from)
	if !d.lookups.acquire(nw) {
		return m.SetRcode(req, dns.RcodeServerFailure)
	}
	defer d.lookups.release(nw)
	addrs, ok := d.lookup(ctx, from, name)
	if !ok {
		m.SetRcode(req, dns.RcodeNameError)
		m.Authoritative = true
		return m
	}
	m.SetReply(req)
	m.Authoritative = true
	if q.Qclass == dns.ClassINET && (q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY) {
		for _, a := range addrs {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: linkTTL},
				A:   a.AsSlice(),
			})
		}
	}
	return m
}

func (d *dnsServer) forward(ctx context.Context, from netip.Addr, req *dns.Msg, tcp bool) *dns.Msg {
	nw := networkOf(from)
	if !d.forwards.acquire(nw) {
		return new(dns.Msg).SetRcode(req, dns.RcodeServerFailure)
	}
	defer d.forwards.release(nw)
	if opt := req.IsEdns0(); opt != nil && opt.UDPSize() > dnsMaxAnswer {
		opt.SetUDPSize(dnsMaxAnswer)
	}
	c := &dns.Client{Net: "udp", Timeout: 3 * time.Second}
	if tcp {
		c.Net = "tcp"
	}
	for _, up := range d.upstream {
		if r, _, err := c.ExchangeContext(ctx, req, up); err == nil {
			return r
		}
	}
	return new(dns.Msg).SetRcode(req, dns.RcodeServerFailure)
}

// limiter caps how many things are in progress at once, in all and for each
// key, which is the network that asks (see networkOf). A limit of zero is no
// limit.
type limiter struct {
	mu      sync.Mutex
	total   int
	by      map[netip.Addr]int
	max     int
	maxEach int
}

func newLimiter(max, maxEach int) *limiter {
	return &limiter{max: max, maxEach: maxEach, by: map[netip.Addr]int{}}
}

// acquire takes a place for key and reports whether there was one.
func (l *limiter) acquire(key netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if (l.max > 0 && l.total >= l.max) || (l.maxEach > 0 && l.by[key] >= l.maxEach) {
		return false
	}
	l.total++
	l.by[key]++
	return true
}

func (l *limiter) release(key netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.by[key]--; l.by[key] <= 0 {
		delete(l.by, key)
	}
}

// limitedListener turns away connections beyond the limiter's.
type limitedListener struct {
	net.Listener
	conns *limiter
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		nw := networkOf(addrOf(c.RemoteAddr()))
		if !l.conns.acquire(nw) {
			c.Close()
			continue
		}
		return &limitedConn{Conn: c, release: sync.OnceFunc(func() { l.conns.release(nw) })}, nil
	}
}

type limitedConn struct {
	net.Conn
	release func()
}

func (c *limitedConn) Close() error {
	c.release()
	return c.Conn.Close()
}
