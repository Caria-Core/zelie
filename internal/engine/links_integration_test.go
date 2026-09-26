//go:build integration

package engine

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/netns"
	cnins "github.com/containernetworking/plugins/pkg/ns"
	"github.com/miekg/dns"
)

// askFrom asks the DNS server of the container's network for name, from
// inside the container's network namespace.
func askFrom(t *testing.T, pid uint32, name string) *dns.Msg {
	t.Helper()
	var r *dns.Msg
	var askErr error
	err := netns.LoadNetNS(fmt.Sprintf("/proc/%d/ns/net", pid)).Do(func(cnins.NetNS) error {
		cfg, err := dns.ClientConfigFromFile("/proc/" + fmt.Sprint(pid) + "/root/etc/resolv.conf")
		if err != nil {
			askErr = err
			return nil
		}
		c := &dns.Client{Timeout: 2 * time.Second}
		r, _, askErr = c.Exchange(new(dns.Msg).SetQuestion(dns.Fqdn(name), dns.TypeA), net.JoinHostPort(cfg.Servers[0], "53"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if askErr != nil {
		t.Fatalf("ask %s: %v", name, askErr)
	}
	return r
}

func answers(r *dns.Msg) []string {
	var out []string
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.A); ok {
			out = append(out, a.A.String())
		}
	}
	return out
}

func TestLinks(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	small := Spec{Image: testImage, MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 16}
	db, web, stranger := small, small, small
	db.ID, db.App, db.Network = "it-link-db", "it-link-db", "it-link-db"
	db.Args = []string{"sh", "-c", "httpd -p 9090; httpd -f -p 8080"}
	web.ID, web.App, web.Network, web.Args = "it-link-web", "it-link-web", "it-link-web", []string{"sleep", "300"}
	stranger.ID, stranger.App, stranger.Network, stranger.Args = "it-link-other", "it-link-other", "it-link-other", []string{"sleep", "300"}
	e.SetLinks(ctx, web.App, nil)
	t.Cleanup(func() { e.SetLinks(context.Background(), web.App, nil) })
	for _, s := range []Spec{db, web, stranger} {
		run(t, e, s)
	}
	dbIP := status(t, e, db.ID).IP
	waitFor(t, func() error { return dialHost(net.JoinHostPort(dbIP.String(), "8080")) })
	webPid, strangerPid := status(t, e, web.ID).Pid, status(t, e, stranger.ID).Pid
	at := func(port int) string { return net.JoinHostPort(dbIP.String(), fmt.Sprint(port)) }

	if err := dialFrom(t, webPid, at(8080)); err == nil {
		t.Error("reached another app before linking")
	}
	if r := askFrom(t, webPid, "db"); r.Rcode != dns.RcodeNameError {
		t.Errorf("db resolved before linking: %v", r)
	}

	if err := e.SetLinks(ctx, web.App, []Link{{Name: "db", To: db.App, Port: 8080}}); err != nil {
		t.Fatal(err)
	}
	if err := dialFrom(t, webPid, at(8080)); err != nil {
		t.Errorf("linked port: %v", err)
	}
	if err := dialFrom(t, webPid, at(9090)); err == nil {
		t.Error("reached a port the link does not open")
	}
	if err := dialFrom(t, strangerPid, at(8080)); err == nil {
		t.Error("an app without the link reached the linked one")
	}
	if got := answers(askFrom(t, webPid, "db")); strings.Join(got, ",") != dbIP.String() {
		t.Errorf("db resolved to %v, want %s", got, dbIP)
	}
	if r := askFrom(t, strangerPid, "db"); r.Rcode != dns.RcodeNameError {
		t.Errorf("db resolved for an app without the link: %v", r)
	}
	// The link is one way.
	if err := dialFrom(t, status(t, e, db.ID).Pid, net.JoinHostPort(status(t, e, web.ID).IP.String(), "8080")); err == nil {
		t.Error("the linked app reached back")
	}
	if got := answers(askFrom(t, webPid, "deb.debian.org")); len(got) == 0 {
		t.Error("names outside Zelie do not resolve")
	}

	// A new deployment gets a new address; the name and the opening follow.
	if err := e.Remove(ctx, db.ID); err != nil {
		t.Fatal(err)
	}
	db.ID = "it-link-db2"
	run(t, e, db)
	newIP := status(t, e, db.ID).IP
	if newIP == dbIP {
		t.Log("the new container got the same address")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := answers(askFrom(t, webPid, "db"))
		if strings.Join(got, ",") == newIP.String() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("db resolved to %v, want %s", got, newIP)
		}
		time.Sleep(500 * time.Millisecond)
	}
	waitFor(t, func() error { return dialFrom(t, webPid, net.JoinHostPort(newIP.String(), "8080")) })

	// Unlinking closes it again.
	if err := e.SetLinks(ctx, web.App, nil); err != nil {
		t.Fatal(err)
	}
	if err := dialFrom(t, webPid, net.JoinHostPort(newIP.String(), "8080")); err == nil {
		t.Error("reached the app after unlinking")
	}
}

// TestLinkedNameFromInside resolves a link the way an app does: through
// the resolv.conf the container was given.
func TestLinkedNameFromInside(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	small := Spec{Image: testImage, MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 16}
	db, web := small, small
	db.ID, db.App, db.Network, db.Args = "it-inside-db", "it-inside-db", "it-inside-db", []string{"httpd", "-f", "-p", "8080"}
	web.ID, web.App, web.Network = "it-inside-web", "it-inside-web", "it-inside-web"
	web.Args = []string{"sh", "-c", "until wget -q -T 2 -O /dev/null http://db:8080/ 2>&1 | grep -q 404; do sleep 1; done; echo reached; sleep 300"}
	if err := e.SetLinks(ctx, web.App, []Link{{Name: "db", To: db.App, Port: 8080}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.SetLinks(context.Background(), web.App, nil) })
	run(t, e, db)
	run(t, e, web)
	waitForLog(t, web.ID, "reached")
}

func dialHost(addr string) error {
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		c.Close()
	}
	return err
}

func waitFor(t *testing.T, f func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
