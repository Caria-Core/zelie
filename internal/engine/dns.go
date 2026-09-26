package engine

import (
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

type dnsServer struct {
	lookup   func(ctx context.Context, from netip.Addr, name string) ([]netip.Addr, bool)
	upstream []string // host:port

	mu        sync.Mutex
	listening map[netip.Addr]bool
	ctx       context.Context
	errs      chan error
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
	if d.ctx == nil || d.listening[addr] {
		return nil
	}
	// The gateway address only exists once the network's first container
	// has started, so the servers bind to it ahead of time.
	lc := freebind()
	hostport := netip.AddrPortFrom(addr, 53).String()
	pc, err := lc.ListenPacket(d.ctx, "udp4", hostport)
	if err != nil {
		return err
	}
	l, err := lc.Listen(d.ctx, "tcp4", hostport)
	if err != nil {
		pc.Close()
		return err
	}
	servers := []*dns.Server{
		{PacketConn: pc, Handler: dns.HandlerFunc(d.serve)},
		{Listener: l, Handler: dns.HandlerFunc(d.serve), ReadTimeout: 5 * time.Second},
	}
	for _, s := range servers {
		go func() {
			if err := s.ActivateAndServe(); err != nil && d.ctx.Err() == nil {
				select {
				case d.errs <- err:
				default:
				}
			}
		}()
	}
	go func() {
		<-d.ctx.Done()
		for _, s := range servers {
			s.Shutdown()
		}
	}()
	if d.listening == nil {
		d.listening = map[netip.Addr]bool{}
	}
	d.listening[addr] = true
	return nil
}

func (d *dnsServer) serve(w dns.ResponseWriter, req *dns.Msg) {
	from, _ := netip.ParseAddrPort(w.RemoteAddr().String())
	_, tcp := w.RemoteAddr().(*net.TCPAddr)
	ctx, cancel := context.WithTimeout(d.ctx, 5*time.Second)
	defer cancel()
	w.WriteMsg(d.answer(ctx, from.Addr().Unmap(), req, tcp))
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
		return d.forward(ctx, req, tcp)
	}
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

func (d *dnsServer) forward(ctx context.Context, req *dns.Msg, tcp bool) *dns.Msg {
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
