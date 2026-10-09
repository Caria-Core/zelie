// Package sftpd serves SFTP for game servers. It runs as a user of its own
// with no access to the volumes: the panel says who may log in and to which
// server, and every file operation goes to the core as a typed request. It
// speaks only the sftp subsystem, with no shell, no commands and no
// forwarding.
package sftpd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/hostkey"
)

// Files is the part of the core's file API that SFTP uses.
type Files interface {
	OpenFolder(ctx context.Context, ref core.FileRef, dir string) (core.Folder, error)
	StatFile(ctx context.Context, ref core.FileRef, p string) (core.FileEntry, error)
	DownloadRange(ctx context.Context, ref core.FileRef, p string, offset, length int64) (io.ReadCloser, int64, error)
	UploadFile(ctx context.Context, ref core.FileRef, p string, size int64, body io.Reader) error
	MakeFolder(ctx context.Context, ref core.FileRef, p string) error
	RenameFile(ctx context.Context, ref core.FileRef, from, to string) error
	RemoveFile(ctx context.Context, ref core.FileRef, p string) error
	SetFileMode(ctx context.Context, ref core.FileRef, p string, mode fs.FileMode) error
	TruncateFile(ctx context.Context, ref core.FileRef, p string, size int64) error
	SetFileTimes(ctx context.Context, ref core.FileRef, p string, atime, mtime time.Time) error
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
	// HostKeyPath is where the host key is kept. The core makes it; this
	// server only reads it.
	HostKeyPath string
	// MaxPerIP is how many connections one address may hold open. Zero
	// means the default of 8.
	MaxPerIP int
	// MaxHandshakes is how many connections may be logging in at once,
	// whatever their addresses. Zero means 64.
	MaxHandshakes int
	// CheckEvery is how often a live connection's grant is put to the panel
	// again. Zero means a minute.
	CheckEvery time.Duration
	// IdleTimeout closes a logged-in connection that has carried no SFTP
	// traffic for this long. Zero means 15 minutes.
	IdleTimeout time.Duration
	// IdleExit is how long the server stays up with no connection open.
	// Zero means five minutes.
	IdleExit time.Duration

	hostKey ssh.Signer
	conf    *ssh.ServerConfig

	mu         sync.Mutex
	perIP      map[string]int
	handshakes int
}

// Fingerprint is the SHA-256 fingerprint of the host key, as ssh shows it.
func (s *Server) Fingerprint() string { return ssh.FingerprintSHA256(s.hostKey.PublicKey()) }

// LoadHostKey reads the host key. A missing key is an error: the socket
// starts the server again once the core has made it.
func (s *Server) LoadHostKey() error {
	key, err := hostkey.Load(s.HostKeyPath)
	if err != nil {
		return err
	}
	s.hostKey = key
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

// LimitKey is the address a per-address limit counts under. A whole /64
// shares one, since that is what a single IPv6 customer gets to rotate
// through; IPv4, and IPv6 written as IPv4, is kept as it is.
func LimitKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.WithZone("").Unmap()
	if a.Is4() {
		return a.String()
	}
	return netip.PrefixFrom(a, 64).Masked().String()
}

func ipOf(a net.Addr) string {
	if host, _, err := net.SplitHostPort(a.String()); err == nil {
		return host
	}
	return a.String()
}

// Serve accepts connections on l until l is closed, ctx ends, or no
// connection has been open for IdleExit. It returns nil in each case. The
// listener belongs to systemd, which keeps the port and starts the server
// again when someone connects, so closing it here loses nothing.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	if s.conf == nil {
		if err := s.LoadHostKey(); err != nil {
			return err
		}
	}
	idle := s.IdleExit
	if idle == 0 {
		idle = 5 * time.Minute
	}
	var (
		mu    sync.Mutex
		open  int
		idled bool
		conns sync.WaitGroup
	)
	timer := time.AfterFunc(idle, func() {
		mu.Lock()
		defer mu.Unlock()
		if open == 0 {
			idled = true
			l.Close()
		}
	})
	defer timer.Stop()
	returned := make(chan struct{})
	defer close(returned)
	go func() {
		select {
		case <-ctx.Done():
			l.Close()
		case <-returned:
		}
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			mu.Lock()
			done := idled
			mu.Unlock()
			if done {
				// A connection accepted as the timer fired is served out.
				conns.Wait()
				return nil
			}
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		mu.Lock()
		open++
		timer.Stop()
		mu.Unlock()
		conns.Add(1)
		go func() {
			defer conns.Done()
			s.handle(ctx, c)
			mu.Lock()
			defer mu.Unlock()
			if open--; open == 0 {
				timer.Reset(idle)
			}
		}()
	}
}

func (s *Server) hold(ip string) bool {
	ip = LimitKey(ip)
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
	ip = LimitKey(ip)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perIP[ip]--; s.perIP[ip] <= 0 {
		delete(s.perIP, ip)
	}
}

// startHandshake counts a connection that has not logged in yet. Addresses
// are cheap to change, so the number of them is capped as a whole.
func (s *Server) startHandshake() bool {
	limit := s.MaxHandshakes
	if limit == 0 {
		limit = 64
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handshakes >= limit {
		return false
	}
	s.handshakes++
	return true
}

func (s *Server) endHandshake() {
	s.mu.Lock()
	s.handshakes--
	s.mu.Unlock()
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	ip := ipOf(c.RemoteAddr())
	if !s.hold(ip) {
		c.Close()
		return
	}
	defer s.release(ip)
	defer c.Close()

	if !s.startHandshake() {
		return
	}
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	sconn, chans, reqs, err := ssh.NewServerConn(c, s.conf)
	s.endHandshake()
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

	var active activity
	active.touch()
	go s.recheck(ctx, sconn, g)
	go s.expireIdle(ctx, sconn, &active)
	budget := newBudget()

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
			s.session(ctx, ch, g, budget, &active)
		}()
	}
}

// session answers the requests of one channel. The only one it accepts is
// the sftp subsystem; a shell, a command, a terminal or anything else is
// refused.
func (s *Server) session(ctx context.Context, nc ssh.NewChannel, g Grant, b *budget, active *activity) {
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
			h := s.handlers(ctx, g, b)
			rs := sftp.NewRequestServer(&watched{ch, active}, h)
			if err := rs.Serve(); err != nil && !errors.Is(err, io.EOF) {
				s.Log.Debug("SFTP session ended", "server", g.Server, "err", err)
			}
			rs.Close()
			ch.Close()
		}()
	}
}

// recheck puts the grant to the panel every CheckEvery and closes the
// connection when the panel refuses it: the password changed or went, the key
// was removed, the account lost its rights, or the server is gone. When the
// panel cannot be reached the connection stays: a restart of the panel
// should not cut off uploads, and a refusal is the only answer that says the
// access is over. New logins need the panel anyway.
func (s *Server) recheck(ctx context.Context, sconn *ssh.ServerConn, g Grant) {
	every := s.CheckEvery
	if every == 0 {
		every = time.Minute
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cctx, cancel := context.WithTimeout(ctx, panelTimeout)
		err := s.Panel.Check(cctx, g)
		cancel()
		switch {
		case err == nil:
		case errors.Is(err, ErrDenied):
			s.Log.Info("SFTP login ended, the panel no longer grants it", "server", g.Server, "account", g.Account)
			sconn.Close()
			return
		case ctx.Err() == nil:
			s.Log.Warn("ask the panel whether a login still holds", "server", g.Server, "err", err)
		}
	}
}

// expireIdle closes the connection once no SFTP traffic has passed for
// IdleTimeout, so a forgotten client does not hold a login open.
func (s *Server) expireIdle(ctx context.Context, sconn *ssh.ServerConn, a *activity) {
	idle := s.IdleTimeout
	if idle == 0 {
		idle = 15 * time.Minute
	}
	for {
		wait := time.Until(a.last().Add(idle))
		if wait <= 0 {
			s.Log.Info("SFTP login ended, idle", "user", sconn.User())
			sconn.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// activity remembers when an SFTP channel last carried data.
type activity struct{ at atomic.Int64 }

func (a *activity) touch()          { a.at.Store(time.Now().UnixNano()) }
func (a *activity) last() time.Time { return time.Unix(0, a.at.Load()) }

// watched is a channel that records every read and write in activity.
type watched struct {
	ssh.Channel
	a *activity
}

func (w *watched) Read(p []byte) (int, error) {
	n, err := w.Channel.Read(p)
	if n > 0 {
		w.a.touch()
	}
	return n, err
}

func (w *watched) Write(p []byte) (int, error) {
	n, err := w.Channel.Write(p)
	if n > 0 {
		w.a.touch()
	}
	return n, err
}
