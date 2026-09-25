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

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
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

	var policy core.PeerPolicy
	if u, err := user.Lookup(panelUser); err == nil {
		if uid, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
			policy.UIDs = append(policy.UIDs, uint32(uid))
		}
	} else {
		log.Warn("panel user not found, only root may use the core", "user", panelUser)
	}

	e, err := engine.Connect(ctx, engine.DefaultPaths)
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}
	defer e.Close()

	s := &core.Server{Engine: e, Paths: engine.DefaultPaths, Log: log, Allowed: policy}
	if err := s.Serve(ctx, core.DefaultSocket); err != nil {
		log.Error("core stopped", "err", err)
		return 1
	}
	return 0
}
