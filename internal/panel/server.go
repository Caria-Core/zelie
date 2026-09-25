// Package panel is the unprivileged half of Zelie: the web interface, its API
// and user accounts. It never listens on the network. The proxy hands it web
// traffic over a Unix socket, and it asks the core to do anything that needs
// root.
package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/store"
)

// setupLinkTTL is how long a setup link stays valid.
const setupLinkTTL = 24 * time.Hour

type Server struct {
	Store  *store.Store
	Sealer *Sealer
	Log    *slog.Logger
	// ProxyUID is the user the proxy runs as. Requests from it are web
	// traffic; requests from root come from the zelie command on the server.
	ProxyUID uint32
	Now      func() time.Time

	guards *guards
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) Handler() http.Handler {
	local := http.NewServeMux()
	local.HandleFunc("POST /local/setup-link", s.setupLink)

	s.guards = newGuards()
	web := http.NewServeMux()
	web.HandleFunc("GET /api/setup", s.setupStatus)
	web.HandleFunc("POST /api/setup", s.setup)
	web.HandleFunc("POST /api/login", s.login)
	web.HandleFunc("POST /api/login/totp", s.loginTOTP)
	web.HandleFunc("POST /api/login/recovery", s.loginRecovery)
	web.HandleFunc("POST /api/login/passkey/options", s.loginPasskeyOptions)
	web.HandleFunc("POST /api/login/passkey", s.loginPasskey)
	web.HandleFunc("GET /api/me", s.me)
	web.HandleFunc("POST /api/logout", s.logout)
	web.HandleFunc("POST /api/2fa/totp/new", s.enrolling(s.newTOTP))
	web.HandleFunc("POST /api/2fa/totp", s.enrolling(s.confirmTOTP))
	web.HandleFunc("POST /api/2fa/passkey/options", s.enrolling(s.passkeyOptions))
	web.HandleFunc("POST /api/2fa/passkey", s.enrolling(s.addPasskey))
	// Browsers say where a request comes from; anything that changes state
	// must come from the panel's own pages.
	webSafe := http.NewCrossOriginProtection().Handler(web)

	return peer.Require(peer.Policy{UIDs: []uint32{s.ProxyUID}}, s.Log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, _ := peer.From(r.Context())
		if p.UID == 0 {
			local.ServeHTTP(w, r)
			return
		}
		webSafe.ServeHTTP(w, r)
	}))
}

// Serve listens on the socket until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o755); err != nil {
		return err
	}
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// peer.Require decides who gets in; the file mode only lets them knock.
	if err := os.Chmod(socket, 0o666); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ConnContext:       peer.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.Log.Info("panel listening", "socket", socket)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// setupLink makes a new one-time setup token. Only root on the server can ask
// for one, and only while the panel has no users.
func (s *Server) setupLink(w http.ResponseWriter, r *http.Request) {
	token := make([]byte, 32)
	rand.Read(token)
	hash := sha256.Sum256(token)
	err := s.Store.SetSetupToken(r.Context(), hash[:], s.now().Add(setupLinkTTL))
	switch {
	case errors.Is(err, store.ErrSetupDone):
		writeError(w, http.StatusConflict, err)
		return
	case err != nil:
		s.fail(w, "setup link", err)
		return
	}
	s.Log.Info("setup link created")
	writeJSON(w, http.StatusOK, map[string]string{"token": base64.RawURLEncoding.EncodeToString(token)})
}

func (s *Server) setupStatus(w http.ResponseWriter, r *http.Request) {
	open, err := s.Store.SetupOpen(r.Context())
	if err != nil {
		s.fail(w, "setup status", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"open": open})
}

// fail logs an internal error and tells the client only that something went
// wrong, so details of the server never reach the browser.
func (s *Server) fail(w http.ResponseWriter, what string, err error) {
	s.Log.Error(what, "err", err)
	writeError(w, http.StatusInternalServerError, errors.New("something went wrong on the server"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// Client talks to the panel's socket from the server itself.
type Client struct{ http *http.Client }

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

// SetupLink asks the panel for a new setup token.
func (c *Client) SetupLink(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://panel/local/setup-link", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach the Zelie panel: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return "", fmt.Errorf("panel answered %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(out.Error)
	}
	return out.Token, nil
}
