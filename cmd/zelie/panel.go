package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Caria-Core/zelie/internal/panel"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
)

const (
	panelState = "/var/lib/zelie-panel"
	proxyUser  = "zelie-proxy"
)

func runPanel(stderr io.Writer) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "zelie: the panel must not run as root")
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	proxyUID, ok := lookupUID(proxyUser)
	if !ok {
		log.Warn("proxy user not found, the panel is only reachable from this server", "user", proxyUser)
	}
	if err := os.MkdirAll(panelState, 0o700); err != nil {
		log.Error("start panel", "err", err)
		return 1
	}
	db, err := store.Open(ctx, filepath.Join(panelState, "panel.db"))
	if err != nil {
		log.Error("start panel", "err", err)
		return 1
	}
	defer db.Close()

	s := &panel.Server{Store: db, Log: log, ProxyUID: proxyUID}
	if err := s.Serve(ctx, panelSocket); err != nil {
		log.Error("panel stopped", "err", err)
		return 1
	}
	return 0
}

// setupLink prints a one-time link that makes whoever opens it the first
// administrator.
func setupLink(stdout, stderr io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "zelie: setup-link needs root")
		return 1
	}
	ctx := context.Background()
	token, err := panel.NewClient(panelSocket).SetupLink(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	host := "<panel address>"
	if cfg, err := proxy.NewClient(proxySocket).Config(ctx); err == nil && cfg.Panel != "" {
		host = cfg.Panel
	}
	fmt.Fprintf(stdout, "Open this link to create the first administrator. It works once, for 24 hours:\n\n  https://%s/setup#%s\n", host, token)
	return 0
}
