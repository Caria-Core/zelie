package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/install"
	"github.com/Caria-Core/zelie/internal/sftpd"
)

const sftpState = "/var/lib/zelie-sftp"

func runSFTP(stderr io.Writer) int {
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "zelie: the SFTP server must not run as root")
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	s := &sftpd.Server{
		Panel:       sftpd.NewPanelClient(panelSocket),
		Files:       core.NewClient(core.DefaultSocket),
		Log:         log,
		HostKeyPath: filepath.Join(sftpState, "host_key"),
	}
	if err := s.Run(ctx); err != nil {
		log.Error("SFTP server stopped", "err", err)
		return 1
	}
	return 0
}

// setUpSFTP gives a server that was installed before SFTP existed its user
// and service. An update only restarts the services it knew of, so this is
// where the new one appears. A failure is logged and does not keep the core
// from starting.
func setUpSFTP(ctx context.Context, log *slog.Logger) {
	made, err := install.SetUpSFTP(ctx, runCmd, "")
	switch {
	case err != nil:
		log.Error("set up the SFTP server", "err", err)
	case made:
		log.Info("SFTP server set up", "user", install.SFTPUser)
	}
}

// uidLookup finds a user's ID and remembers it once found. A user that
// does not exist yet is looked for again, at most every few seconds, since
// the update that creates it may be running.
func uidLookup(name string) func() uint32 {
	var mu sync.Mutex
	var uid uint32
	var tried time.Time
	return func() uint32 {
		mu.Lock()
		defer mu.Unlock()
		if uid == 0 && time.Since(tried) > 5*time.Second {
			tried = time.Now()
			if id, ok := lookupUID(name); ok {
				uid = id
			}
		}
		return uid
	}
}
