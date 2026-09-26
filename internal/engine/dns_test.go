package engine

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/miekg/dns"
)

func TestDNSAnswers(t *testing.T) {
	web := netip.MustParseAddr("10.210.0.2")
	pg := netip.MustParseAddr("10.210.1.2")
	upstream := fakeUpstream(t)
	d := &dnsServer{
		upstream: []string{upstream},
		lookup: func(_ context.Context, from netip.Addr, name string) ([]netip.Addr, bool) {
			if from == web && name == "db" {
				return []netip.Addr{pg}, true
			}
			if from == web && name == "cache" {
				return nil, true // linked, nothing running
			}
			return nil, false
		},
	}
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
