//go:build integration

package engine

import (
	"context"
	"net"
	"testing"

	"github.com/miekg/dns"
)

// An app reaches port 3306 of the host through the name Zelie's DNS server
// gives it, only while host access is on, and only on its own gateway.
func TestHostAccess(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	small := Spec{Image: testImage, MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 16}
	web, other := small, small
	web.ID, web.App, web.Network, web.Args = "it-host-web", "it-host-web", "it-host-web", []string{"sleep", "300"}
	other.ID, other.App, other.Network, other.Args = "it-host-other", "it-host-other", "it-host-other", []string{"sleep", "300"}
	e.SetHostAccess(ctx, web.App, false)
	t.Cleanup(func() { e.SetHostAccess(context.Background(), web.App, false) })
	run(t, e, web)
	run(t, e, other)
	webPid, otherPid := status(t, e, web.ID).Pid, status(t, e, other.ID).Pid

	// Stands in for a MariaDB bound to every address of the host: listening on
	// the gateway of both networks.
	for _, s := range []Spec{web, other} {
		nw, err := e.networks.ensure(s.Network)
		if err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("tcp4", net.JoinHostPort(nw.gateway().String(), "3306"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				c.Close()
			}
		}()
	}
	nw, err := e.networks.ensure(web.Network)
	if err != nil {
		t.Fatal(err)
	}
	gateway := nw.gateway().String()

	if err := dialFrom(t, webPid, net.JoinHostPort(gateway, "3306")); err == nil {
		t.Error("reached the host before host access was on")
	}
	if r := askFrom(t, webPid, HostName); r.Rcode != dns.RcodeNameError {
		t.Errorf("%s resolved before host access was on: %v", HostName, r)
	}

	if err := e.SetHostAccess(ctx, web.App, true); err != nil {
		t.Fatal(err)
	}
	got := answers(askFrom(t, webPid, HostName))
	if len(got) != 1 || got[0] != gateway {
		t.Fatalf("%s resolved to %v, want %s", HostName, got, gateway)
	}
	if err := dialFrom(t, webPid, net.JoinHostPort(got[0], "3306")); err != nil {
		t.Errorf("host port 3306 with host access on: %v", err)
	}
	// Only that port, and only for this app.
	if err := dialFrom(t, webPid, net.JoinHostPort(gateway, "22")); err == nil {
		t.Error("reached another port of the host")
	}
	otherNW, err := e.networks.ensure(other.Network)
	if err != nil {
		t.Fatal(err)
	}
	if err := dialFrom(t, otherPid, net.JoinHostPort(otherNW.gateway().String(), "3306")); err == nil {
		t.Error("an app without host access reached the host")
	}
	if err := dialFrom(t, otherPid, net.JoinHostPort(gateway, "3306")); err == nil {
		t.Error("an app reached the host through another app's network")
	}
	if r := askFrom(t, otherPid, HostName); r.Rcode != dns.RcodeNameError {
		t.Errorf("%s resolved for an app without host access: %v", HostName, r)
	}

	if err := e.SetHostAccess(ctx, web.App, false); err != nil {
		t.Fatal(err)
	}
	if err := dialFrom(t, webPid, net.JoinHostPort(gateway, "3306")); err == nil {
		t.Error("reached the host after host access was turned off")
	}
	if r := askFrom(t, webPid, HostName); r.Rcode != dns.RcodeNameError {
		t.Errorf("%s still resolves after host access was turned off: %v", HostName, r)
	}
}
