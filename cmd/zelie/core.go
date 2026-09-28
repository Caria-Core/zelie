package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"strconv"
	"syscall"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/secret"
)

// panelUser is the unprivileged account the panel runs as. Until the panel
// exists the account may be missing, and then only root can use the core.
const panelUser = "zelie"

func runCore(stderr io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "zelie: the core needs root")
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	policy := panelPolicy(log)

	e, err := engine.Connect(ctx, engine.DefaultPaths)
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}
	defer e.Close()

	keys, err := secret.LoadOrCreate("/var/lib/zelie/secrets.key")
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}

	backupKey, err := backup.LoadOrCreateKey("/var/lib/zelie/backup.key")
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}

	offsite, err := core.LoadOffsite("/var/lib/zelie/offsite.json")
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}

	external, listeners, err := core.LoadExternal("/var/lib/zelie/external.json")
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}

	// Without DNS, apps cannot find their links or anything else, so the
	// core stops with it.
	dnsErrs, err := e.StartDNS(ctx)
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}
	var dnsErr error
	go func() {
		select {
		case dnsErr = <-dnsErrs:
			stop()
		case <-ctx.Done():
		}
	}()

	s := &core.Server{
		Engine: e, Paths: engine.DefaultPaths, Log: log, Allowed: policy,
		Builder:  build.New(e, engine.DefaultPaths, "/var/lib/zelie/build"),
		Secrets:  keys,
		Backups:  &backup.Dir{Root: "/var/lib/zelie/backups", Key: backupKey},
		Offsite:  offsite,
		External: external,
		Updater:  newUpdater(),
	}
	s.StartExternal(listeners)
	if err := s.Serve(ctx, core.DefaultSocket); err != nil {
		log.Error("core stopped", "err", err)
		return 1
	}
	if dnsErr != nil {
		log.Error("DNS server stopped", "err", dnsErr)
		return 1
	}
	return 0
}

func lookupUID(name string) (uint32, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, false
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	return uint32(uid), err == nil
}
