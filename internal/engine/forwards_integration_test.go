//go:build integration

package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"
)

// gatewayOf is the host's address on the container's network. A player on
// the same machine reaches the forwarded port there, and it works without
// depending on what other addresses the machine has.
func gatewayOf(ip netip.Addr) netip.Addr {
	b := ip.As4()
	b[3] = 1
	return netip.AddrFrom4(b)
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

func echoUDP(addr string) error {
	c, err := net.Dial("udp", addr)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		return err
	}
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if err != nil {
		return err
	}
	if string(buf[:n]) != "ping" {
		return fmt.Errorf("got %q back", buf[:n])
	}
	return nil
}

func TestForwards(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	const app = "it-forward"
	e.SetForwards(ctx, app, nil)
	t.Cleanup(func() { e.SetForwards(context.Background(), app, nil) })

	spec := Spec{
		ID: app, App: app, Network: app, Image: testImage, MemoryBytes: 32 << 20, CPUs: 0.1, Pids: 32,
		// A web server for TCP and an echo for UDP, which serves one
		// datagram at a time.
		Args: []string{"sh", "-c", "httpd -p 9000; while true; do nc -u -l -p 9001 -e cat; done"},
	}
	run(t, e, spec)
	host := freePort(t)
	forwards := []Forward{
		{Port: host, Proto: "tcp", Target: 9000},
		{Port: host, Proto: "udp", Target: 9001},
	}
	if err := e.SetForwards(ctx, app, forwards); err != nil {
		t.Fatal(err)
	}
	at := func(id string) string {
		return net.JoinHostPort(gatewayOf(status(t, e, id).IP).String(), fmt.Sprint(host))
	}

	waitFor(t, func() error { return dialHost(at(spec.ID)) })
	waitFor(t, func() error { return echoUDP(at(spec.ID)) })
	// The container's own port is not open on the host, only the one
	// that was forwarded.
	if err := dialHost(net.JoinHostPort(gatewayOf(status(t, e, spec.ID).IP).String(), "9000")); err == nil {
		t.Error("the container's port is open on the host without a forward")
	}

	// Another app cannot take the same port.
	var taken *PortForwardedError
	if err := e.SetForwards(ctx, "it-forward-other", forwards[:1]); !errors.As(err, &taken) || taken.Port != host {
		t.Errorf("a second app took the port: %v", err)
	}

	// A new container of the app gets a new address, and the forward
	// follows it without being set again.
	if err := e.Remove(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	spec.ID = app + "-2"
	run(t, e, spec)
	waitFor(t, func() error { return dialHost(at(spec.ID)) })
	waitFor(t, func() error { return echoUDP(at(spec.ID)) })

	// While the app has no running container nothing is forwarded.
	if err := e.Stop(ctx, spec.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	e.peers.mu.Lock()
	open := len(e.peers.applied.forwards)
	e.peers.mu.Unlock()
	if open != 0 {
		t.Errorf("%d forwards stay open for a stopped container", open)
	}
	if err := e.Remove(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	spec.ID = app + "-3"
	run(t, e, spec)
	waitFor(t, func() error { return dialHost(at(spec.ID)) })

	if err := e.SetForwards(ctx, app, nil); err != nil {
		t.Fatal(err)
	}
	if err := dialHost(at(spec.ID)); err == nil {
		t.Error("the port is still open after clearing")
	}
}
