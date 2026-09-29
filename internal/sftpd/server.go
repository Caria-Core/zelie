// Package sftpd serves SFTP for game servers. It runs as a user of its own
// with no access to the volumes: the panel says who may log in and to which
// server, and every file operation goes to the core as a typed request. It
// speaks only the sftp subsystem, with no shell, no commands and no
// forwarding.
package sftpd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/core"
)

// Files is the part of the core's file API that SFTP uses.
type Files interface {
	ListFiles(ctx context.Context, ref core.FileRef, dir string) (core.FileList, error)
	StatFile(ctx context.Context, ref core.FileRef, p string) (core.FileEntry, error)
	DownloadRange(ctx context.Context, ref core.FileRef, p string, offset, length int64) (io.ReadCloser, int64, error)
	UploadFile(ctx context.Context, ref core.FileRef, p string, size int64, body io.Reader) error
	MakeFolder(ctx context.Context, ref core.FileRef, p string) error
	RenameFile(ctx context.Context, ref core.FileRef, from, to string) error
	RemoveFile(ctx context.Context, ref core.FileRef, p string) error
}

const (
	handshakeTimeout   = 30 * time.Second
	panelTimeout       = 20 * time.Second
	maxSessionsPerConn = 4
	maxUserBytes       = 64
	grantKey           = "zelie-grant"
)

// Server is the SFTP service.
type Server struct {
	Panel Panel
	Files Files
	Log   *slog.Logger
	// HostKeyPath is where the host key is kept; it is made on first use.
	HostKeyPath string
	// MaxPerIP is how many connections one address may hold open. Zero
	// means the default of 8.
	MaxPerIP int
	// Poll is how often the panel is asked which port to listen on. Zero
	// means every 15 seconds.
	Poll time.Duration
	// Listen opens the port; nil listens on all addresses. Tests replace it.
	Listen func(port int) (net.Listener, error)

	hostKey ssh.Signer
	conf    *ssh.ServerConfig

	mu    sync.Mutex
	perIP map[string]int
}

// Fingerprint is the SHA-256 fingerprint of the host key, as ssh shows it.
func (s *Server) Fingerprint() string { return ssh.FingerprintSHA256(s.hostKey.PublicKey()) }

// LoadHostKey reads the host key, or makes one if there is none yet.
func (s *Server) LoadHostKey() error {
	pemBytes, err := os.ReadFile(s.HostKeyPath)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return gerr
		}
		block, gerr := ssh.MarshalPrivateKey(priv, "zelie sftp host key")
		if gerr != nil {
			return gerr
		}
		pemBytes = pem.EncodeToMemory(block)
		if gerr := os.MkdirAll(filepath.Dir(s.HostKeyPath), 0o700); gerr != nil {
			return gerr
		}
		tmp := s.HostKeyPath + ".tmp"
		if gerr := os.WriteFile(tmp, pemBytes, 0o600); gerr != nil {
			return gerr
		}
		if gerr := os.Rename(tmp, s.HostKeyPath); gerr != nil {
			return gerr
		}
	} else if err != nil {
		return err
	}
	s.hostKey, err = ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return fmt.Errorf("read host key %s: %w", s.HostKeyPath, err)
	}
	s.conf = s.serverConfig()
	return nil
}

func (s *Server) serverConfig() *ssh.ServerConfig {
	c := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-Zelie",
		MaxAuthTries:  4,
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			return s.grant(conn, func(ctx context.Context, server, ip string) (Grant, error) {
				return s.Panel.Password(ctx, server, string(password), ip)
			})
		},
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			return s.grant(conn, func(ctx context.Context, server, ip string) (Grant, error) {
				return s.Panel.Key(ctx, server, key, ip)
			})
		},
	}
	c.AddHostKey(s.hostKey)
	return c
}

func (s *Server) grant(conn ssh.ConnMetadata, ask func(ctx context.Context, server, ip string) (Grant, error)) (*ssh.Permissions, error) {
	user := conn.User()
	if user == "" || len(user) > maxUserBytes {
		return nil, ErrDenied
	}
	ctx, cancel := context.WithTimeout(context.Background(), panelTimeout)
	defer cancel()
	g, err := ask(ctx, user, ipOf(conn.RemoteAddr()))
	if err != nil {
		if !errors.Is(err, ErrDenied) {
			s.Log.Warn("ask the panel about a login", "err", err)
		}
		return nil, ErrDenied
	}
	b, _ := json.Marshal(g)
	return &ssh.Permissions{Extensions: map[string]string{grantKey: string(b)}}, nil
}

func ipOf(a net.Addr) string {
	if host, _, err := net.SplitHostPort(a.String()); err == nil {
		return host
	}
	return a.String()
}

// Run serves until ctx ends. The panel says which port to listen on, and
// is asked again from time to time: a port changed in the panel takes
// effect within one poll, and connections already open stay open.
func (s *Server) Run(ctx context.Context) error {
	if err := s.LoadHostKey(); err != nil {
		return err
	}
	poll := s.Poll
	if poll == 0 {
		poll = 15 * time.Second
	}
	var current net.Listener
	port := 0
	defer func() {
		if current != nil {
			current.Close()
		}
	}()
	for first := true; ; first = false {
		want, err := s.Panel.Config(ctx, s.Fingerprint())
		switch {
		case err != nil:
			// The panel may still be starting; a running service keeps what it has.
			if first || current == nil {
				s.Log.Warn("waiting for the panel", "err", err)
			}
		case want != port:
			l, err := s.listen(want)
			if err != nil {
				s.Log.Error("cannot listen for SFTP, will try again", "port", want, "err", err)
				break
			}
			if current != nil {
				current.Close()
			}
			current, port = l, want
			s.Log.Info("SFTP listening", "port", want, "fingerprint", s.Fingerprint())
			go s.Serve(ctx, l)
		}
		wait := poll
		if current == nil {
			wait = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (s *Server) listen(port int) (net.Listener, error) {
	if s.Listen != nil {
		return s.Listen(port)
	}
	return net.Listen("tcp", ":"+strconv.Itoa(port))
}

// Serve accepts connections on l until l is closed or ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	if s.conf == nil {
		if err := s.LoadHostKey(); err != nil {
			return err
		}
	}
	go func() {
		<-ctx.Done()
		l.Close()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.handle(ctx, c)
	}
}

func (s *Server) hold(ip string) bool {
	limit := s.MaxPerIP
	if limit == 0 {
		limit = 8
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perIP == nil {
		s.perIP = map[string]int{}
	}
	if s.perIP[ip] >= limit {
		return false
	}
	s.perIP[ip]++
	return true
}

func (s *Server) release(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	ip := ipOf(c.RemoteAddr())
	if !s.hold(ip) {
		c.Close()
		return
	}
	defer s.release(ip)
	defer c.Close()

	c.SetDeadline(time.Now().Add(handshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(c, s.conf)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	defer sconn.Close()
	var g Grant
	if err := json.Unmarshal([]byte(sconn.Permissions.Extensions[grantKey]), &g); err != nil {
		return
	}
	s.Log.Info("SFTP login", "server", g.Server, "account", g.Account, "ip", ip)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		sconn.Wait()
		cancel()
	}()
	// Nothing is forwarded, so every global request is refused.
	go ssh.DiscardRequests(reqs)

	var sessions atomic.Int32
	var wg sync.WaitGroup
	defer wg.Wait()
	for ch := range chans {
		if ch.ChannelType() != "session" {
			ch.Reject(ssh.Prohibited, "only sftp is served")
			continue
		}
		if sessions.Add(1) > maxSessionsPerConn {
			sessions.Add(-1)
			ch.Reject(ssh.ResourceShortage, "too many sessions")
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer sessions.Add(-1)
			s.session(ctx, ch, g)
		}()
	}
}

// session answers the requests of one channel. The only one it accepts is
// the sftp subsystem; a shell, a command, a terminal or anything else is
// refused.
func (s *Server) session(ctx context.Context, nc ssh.NewChannel, g Grant) {
	ch, reqs, err := nc.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	started := false
	for req := range reqs {
		var sub struct{ Name string }
		if req.Type != "subsystem" || started || ssh.Unmarshal(req.Payload, &sub) != nil || sub.Name != "sftp" {
			if req.WantReply {
				req.Reply(false, nil)
			}
			continue
		}
		started = true
		req.Reply(true, nil)
		go func() {
			h := s.handlers(ctx, g)
			rs := sftp.NewRequestServer(ch, h)
			if err := rs.Serve(); err != nil && !errors.Is(err, io.EOF) {
				s.Log.Debug("SFTP session ended", "server", g.Server, "err", err)
			}
			rs.Close()
			ch.Close()
		}()
	}
}
