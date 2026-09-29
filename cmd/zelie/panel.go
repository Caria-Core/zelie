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

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/memtrim"
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

	go memtrim.Run(ctx)

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

	sealer, err := panel.LoadSealer(filepath.Join(panelState, "panel.key"))
	if err != nil {
		log.Error("start panel", "err", err)
		return 1
	}

	s := &panel.Server{
		Store: db, Sealer: sealer, Core: core.NewClient(core.DefaultSocket),
		Proxy: proxy.NewClient(proxySocket), Source: panel.NewPublicGitHub(),
		Log: log, ProxyUID: proxyUID, DataDir: panelState,
	}
	if err := s.Serve(ctx, panelSocket); err != nil {
		log.Error("panel stopped", "err", err)
		return 1
	}
	return 0
}

// resetLogin prints a one-time link that sets a new password for an
// administrator locked out of the panel, and removes their second steps.
func resetLogin(args []string, stdout, stderr io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "zelie: reset-login needs root")
		return 1
	}
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: zelie reset-login [email]")
		return 2
	}
	email := ""
	if len(args) == 1 {
		email = args[0]
	}
	ctx := context.Background()
	token, account, err := panel.NewClient(panelSocket).ResetLink(ctx, email)
	if err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Open this link to set a new password for %s. It works once, for an hour:\n\n  https://%s/reset#%s\n\n"+
		"It also removes the account's passkeys, authenticator app and recovery codes, and logs it out everywhere.\n"+
		"The panel asks for a new second step right after.\n", account, panelHost(ctx), token)
	return 0
}

// panelHost is the panel's address as the proxy serves it.
func panelHost(ctx context.Context) string {
	if cfg, err := proxy.NewClient(proxySocket).Config(ctx); err == nil && cfg.Panel != "" {
		return cfg.Panel
	}
	return "<panel address>"
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
	fmt.Fprintf(stdout, "Open this link to create the first administrator. It works once, for 24 hours:\n\n  https://%s/setup#%s\n", panelHost(ctx), token)
	return 0
}
