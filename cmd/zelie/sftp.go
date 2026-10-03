package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/hostkey"
	"github.com/Caria-Core/zelie/internal/install"
	"github.com/Caria-Core/zelie/internal/sftpd"
)

// The host key is in a folder only root can write: the SFTP user must not be
// able to plant links where the core writes. sftpOldState is where earlier
// versions kept it.
const (
	sftpKeyDir   = "/etc/zelie-sftp"
	sftpOldState = "/var/lib/zelie-sftp"
)

func runSFTP(args []string, stderr io.Writer) int {
	// The unit asks this before starting the server; see install.SFTPUnit.
	if len(args) == 1 && args[0] == "--check" {
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintln(stderr, "zelie: usage: zelie sftp")
		return 2
	}
	if os.Geteuid() == 0 {
		fmt.Fprintln(stderr, "zelie: the SFTP server must not run as root")
		return 1
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	l, err := systemdListener(os.Getenv, os.Getpid())
	if err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	defer l.Close()

	s := &sftpd.Server{
		Panel:       sftpd.NewPanelClient(panelSocket),
		Files:       core.NewClient(core.DefaultSocket),
		Log:         log,
		HostKeyPath: filepath.Join(sftpKeyDir, hostkey.File),
	}
	if err := s.Serve(ctx, l); err != nil {
		log.Error("SFTP server stopped", "err", err)
		return 1
	}
	log.Info("SFTP server stopped")
	return 0
}

// systemdListener takes the socket systemd passes to a service it started
// for a socket unit: the first one, as file descriptor 3. The port belongs
// to systemd, so a server started by hand has none and does not open one.
func systemdListener(getenv func(string) string, pid int) (net.Listener, error) {
	const first = 3 // SD_LISTEN_FDS_START
	if getenv("LISTEN_PID") != strconv.Itoa(pid) {
		return nil, errors.New("no socket from systemd: the SFTP server is started by zelie-sftp.socket")
	}
	if n, err := strconv.Atoi(getenv("LISTEN_FDS")); err != nil || n < 1 {
		return nil, errors.New("no socket from systemd: LISTEN_FDS is not set")
	}
	syscall.CloseOnExec(first)
	f := os.NewFile(first, "zelie-sftp.socket")
	defer f.Close()
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("use the socket from systemd: %w", err)
	}
	return l, nil
}

// setUpSFTP gives a server that was installed before SFTP existed its user
// service and socket. An update only restarts the services it knew of, so this is
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
