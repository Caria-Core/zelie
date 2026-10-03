package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/hostkey"
)

// The SFTP server starts when someone connects: systemd holds the port in a
// socket unit and hands the connection over. The core, which is root, is the
// one that can change that port and create the host key, so the panel asks
// it to, and asks it whether the socket is up.

const sftpSocketUnit = "zelie-sftp.socket"

// SFTPSocket controls the SFTP socket. The paths and the command runner are
// fields so tests can point them elsewhere.
type SFTPSocket struct {
	// DropIn is the file that holds the port.
	DropIn string
	// HostKeyDir is the root-owned folder with the SFTP server's host key,
	// and User the account whose group may read it. OldKeyDir is where
	// earlier versions kept the key, in the SFTP user's own folder.
	HostKeyDir string
	OldKeyDir  string
	User       string
	// Run runs a command and returns what it printed.
	Run func(ctx context.Context, name string, args ...string) (string, error)

	mu sync.Mutex
}

// SFTPStatus is what the panel learns about the SFTP server.
type SFTPStatus struct {
	// Listening is whether the socket is up. The server itself runs only
	// while someone is connected.
	Listening bool   `json:"listening"`
	HostKey   string `json:"host_key"`
}

type sftpPortRequest struct {
	Port int `json:"port"`
}

// SetPort makes the socket listen on port. The drop-in first clears the
// port the unit names, or systemd would listen on both. Nothing happens if
// the file already says so, so a panel that repeats itself at every start
// does not drop a connection. If the socket will not start on the new port,
// most likely because another program has it, the old port is put back so
// SFTP keeps working where it was.
func (c *SFTPSocket) SetPort(ctx context.Context, port int) error {
	want := fmt.Sprintf("[Socket]\nListenStream=\nListenStream=%d\n", port)
	c.mu.Lock()
	defer c.mu.Unlock()
	old, err := os.ReadFile(c.DropIn)
	switch {
	case err == nil && string(old) == want:
		return nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	had := err == nil
	if err := c.apply(ctx, []byte(want), true); err != nil {
		if rerr := c.apply(context.WithoutCancel(ctx), old, had); rerr != nil {
			return fmt.Errorf("%w; putting the old port back: %v", err, rerr)
		}
		return err
	}
	return nil
}

// apply writes the drop-in, or removes it when there should be none, and
// restarts the socket on it.
func (c *SFTPSocket) apply(ctx context.Context, content []byte, keep bool) error {
	if keep {
		if err := os.MkdirAll(filepath.Dir(c.DropIn), 0o755); err != nil {
			return err
		}
		tmp := c.DropIn + ".tmp"
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, c.DropIn); err != nil {
			return err
		}
	} else if err := os.Remove(c.DropIn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if out, err := c.Run(ctx, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(out))
	}
	if out, err := c.Run(ctx, "systemctl", "restart", sftpSocketUnit); err != nil {
		return fmt.Errorf("systemctl restart %s: %v: %s", sftpSocketUnit, err, strings.TrimSpace(out))
	}
	return nil
}

// EnsureHostKey makes the host key if there is none and returns it. The
// core calls it at start, so a fresh install has the key before the first
// connection, and Status calls it again in case the key went missing.
func (c *SFTPSocket) EnsureHostKey() (ssh.Signer, error) {
	u, err := user.Lookup(c.User)
	if err != nil {
		return nil, fmt.Errorf("find the SFTP user: %w", err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil, errors.New("the SFTP user has no numeric ID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return hostkey.Ensure(c.HostKeyDir, gid, c.OldKeyDir)
}

// Status reports whether the socket is up and the host key's fingerprint.
// It makes the host key if there is none, so the fingerprint can be shown
// before anyone has connected.
func (c *SFTPSocket) Status(ctx context.Context) (SFTPStatus, error) {
	key, err := c.EnsureHostKey()
	if err != nil {
		return SFTPStatus{}, err
	}
	out, _ := c.Run(ctx, "systemctl", "is-active", sftpSocketUnit)
	return SFTPStatus{Listening: strings.TrimSpace(out) == "active", HostKey: ssh.FingerprintSHA256(key.PublicKey())}, nil
}

func (s *Server) setSFTPPort(w http.ResponseWriter, r *http.Request) {
	var req sftpPortRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Port < 1024 || req.Port > 65535 {
		writeError(w, http.StatusBadRequest, errors.New("the port must be from 1024 to 65535"))
		return
	}
	if s.SFTPSocket == nil {
		writeError(w, http.StatusConflict, errors.New("this core does not manage the SFTP socket"))
		return
	}
	unlock, ok := s.guardSFTPPort(w, req.Port)
	if !ok {
		return
	}
	defer unlock()
	if err := s.SFTPSocket.SetPort(r.Context(), req.Port); err != nil {
		s.Log.Error("set the SFTP port", "port", req.Port, "err", err)
		writeError(w, http.StatusInternalServerError, errors.New("could not change the SFTP port"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sftpStatus(w http.ResponseWriter, r *http.Request) {
	if s.SFTPSocket == nil {
		writeError(w, http.StatusConflict, errors.New("this core does not manage the SFTP socket"))
		return
	}
	st, err := s.SFTPSocket.Status(r.Context())
	if err != nil {
		s.Log.Error("read the SFTP status", "err", err)
		writeError(w, http.StatusInternalServerError, errors.New("could not read the SFTP status"))
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// SetSFTPPort tells the core which port the SFTP socket listens on.
func (c *Client) SetSFTPPort(ctx context.Context, port int) error {
	return c.do(ctx, http.MethodPut, "/v1/sftp/port", sftpPortRequest{Port: port}, nil)
}

// SFTP asks whether the SFTP socket is up, and for the host key.
func (c *Client) SFTP(ctx context.Context) (SFTPStatus, error) {
	var st SFTPStatus
	err := c.do(ctx, http.MethodGet, "/v1/sftp", nil, &st)
	return st, err
}
