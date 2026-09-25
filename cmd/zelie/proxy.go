package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/proxy"
)

const (
	proxySocket = "/run/zelie-proxy/proxy.sock"
	proxyState  = "/var/lib/zelie-proxy"
	panelSocket = "/run/zelie-panel/panel.sock"
)

func runProxy(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	httpAddr := fs.String("http", ":80", "address for plain HTTP")
	httpsAddr := fs.String("https", ":443", "address for HTTPS")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// The proxy faces the internet. It gets the right to bind ports 80 and
	// 443 from systemd and nothing more.
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "zelie: the proxy must not run as root")
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	p := &proxy.Proxy{StateDir: proxyState, PanelSocket: panelSocket, Log: log}
	addrs := proxy.Addrs{HTTP: *httpAddr, HTTPS: *httpsAddr, Socket: proxySocket}
	if err := p.Serve(ctx, addrs, panelPolicy(log)); err != nil {
		log.Error("proxy stopped", "err", err)
		return 1
	}
	return 0
}

// panelPolicy lets root and the panel user through a control socket.
func panelPolicy(log *slog.Logger) peer.Policy {
	var policy peer.Policy
	if uid, ok := lookupUID(panelUser); ok {
		policy.UIDs = append(policy.UIDs, uid)
	} else {
		log.Warn("panel user not found, only root may use this socket", "user", panelUser)
	}
	return policy
}
