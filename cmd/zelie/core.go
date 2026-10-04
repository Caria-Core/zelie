package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/install"
	"github.com/Caria-Core/zelie/internal/memtrim"
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
	// The containerd client unpacks some layers in this process too; see
	// engine.UnitFile for why it must not depend on unpigz.
	os.Setenv("CONTAINERD_DISABLE_PIGZ", "1")
	os.Setenv("CONTAINERD_DISABLE_IGZIP", "1")
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go memtrim.Run(ctx)

	policy := panelPolicy(log)
	var sftpUID uint32
	// The SFTP server may use the file routes and nothing else.
	setUpSFTP(ctx, log)
	if uid, ok := lookupUID(install.SFTPUser); ok {
		policy.Routes = map[uint32][]string{uid: core.SFTPRoutes}
		sftpUID = uid
	} else {
		log.Warn("SFTP user not found, SFTP cannot reach the core", "user", install.SFTPUser)
	}

	sftpSocket := &core.SFTPSocket{
		DropIn:     filepath.Join(install.UnitDir, install.SFTPSocket+".d", "port.conf"),
		HostKeyDir: sftpKeyDir,
		OldKeyDir:  sftpOldState,
		User:       install.SFTPUser,
		Run:        runCmd,
	}
	// The server only reads its host key, so it has to exist before the
	// first connection.
	if _, err := sftpSocket.EnsureHostKey(); err != nil {
		log.Error("make the SFTP host key", "err", err)
	}

	// Updates only swap the binary, so containerd's unit changes reach old
	// installs here. A restart also restarts this core (Requires=), and the
	// second start finds nothing to change.
	switch changed, err := engine.RefreshUnit(ctx, engine.DefaultPaths, nil); {
	case err != nil:
		log.Error("refresh the containerd unit", "err", err)
	case changed:
		log.Info("containerd unit updated, restarted containerd")
	}

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

	sftpVolumes, err := core.LoadSFTPVolumes("/var/lib/zelie/sftp-volumes.json")
	if err != nil {
		log.Error("start core", "err", err)
		return 1
	}

	s := &core.Server{
		SFTPUID: sftpUID, SFTPVolumes: sftpVolumes,
		SFTPSocket: sftpSocket,
		Engine:     e, Paths: engine.DefaultPaths, Log: log, Allowed: policy,
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
