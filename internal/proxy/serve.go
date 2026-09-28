package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/version"
)

// Addrs says where the proxy listens.
type Addrs struct {
	HTTP   string // e.g. ":80"
	HTTPS  string // e.g. ":443"; empty behind a tunnel
	Socket string // control socket for the panel
}

// Serve loads the saved configuration and serves until ctx is done. On
// shutdown open requests get up to half a minute to finish.
func (p *Proxy) Serve(ctx context.Context, a Addrs, allowed peer.Policy) error {
	if err := p.Load(ctx); err != nil {
		return err
	}

	web := &http.Server{
		Addr:              a.HTTP,
		Handler:           http.HandlerFunc(p.serveHTTP),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	secure := &http.Server{
		Addr:    a.HTTPS,
		Handler: http.HandlerFunc(p.serveHTTPS),
		TLSConfig: &tls.Config{
			GetCertificate: p.getCertificate,
			NextProtos:     []string{"h2", "http/1.1", "acme-tls/1"},
			MinVersion:     tls.VersionTLS12,
		},
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version.Get().Version})
	})
	mux.HandleFunc("GET /v1/config", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, p.Config())
	})
	mux.HandleFunc("PUT /v1/config", func(w http.ResponseWriter, r *http.Request) {
		var cfg Config
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
			return
		}
		if err := p.Apply(r.Context(), cfg); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	control := &http.Server{
		Handler:           peer.Require(allowed, p.Log, mux),
		ConnContext:       peer.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
	}

	if err := os.MkdirAll(filepath.Dir(a.Socket), 0o755); err != nil {
		return err
	}
	if err := os.Remove(a.Socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	cl, err := net.Listen("unix", a.Socket)
	if err != nil {
		return err
	}
	// peer.Require decides who gets in; the file mode only lets them knock.
	if err := os.Chmod(a.Socket, 0o666); err != nil {
		cl.Close()
		return err
	}
	wl, err := net.Listen("tcp", a.HTTP)
	if err != nil {
		cl.Close()
		return fmt.Errorf("listen on %s: %w", a.HTTP, err)
	}
	errs := make(chan error, 3)
	// Behind a tunnel there is no HTTPS port.
	if a.HTTPS != "" {
		sl, err := net.Listen("tcp", a.HTTPS)
		if err != nil {
			cl.Close()
			wl.Close()
			return fmt.Errorf("listen on %s: %w", a.HTTPS, err)
		}
		go func() { errs <- secure.ServeTLS(sl, "", "") }()
	}
	go func() { errs <- control.Serve(cl) }()
	go func() { errs <- web.Serve(wl) }()
	p.Log.Info("proxy listening", "http", a.HTTP, "https", a.HTTPS, "socket", a.Socket)

	var first error
	select {
	case <-ctx.Done():
	case first = <-errs:
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range []*http.Server{control, web, secure} {
		s.Shutdown(shutdown)
	}
	if errors.Is(first, http.ErrServerClosed) {
		first = nil
	}
	return first
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// Client changes the proxy's configuration over its control socket.
type Client struct{ http *http.Client }

func NewClient(socket string) *Client {
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *Client) Config(ctx context.Context) (Config, error) {
	var cfg Config
	err := c.do(ctx, http.MethodGet, nil, &cfg)
	return cfg, err
}

// Version asks the running proxy which version it is.
func (c *Client) Version(ctx context.Context) (string, error) {
	return getVersion(ctx, c.http, "http://proxy/v1/version")
}

// getVersion reads {"version": …} from a Zelie process's socket.
func getVersion(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Version string `json:"version"`
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: %s", url, resp.Status)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out)
	return out.Version, err
}

func (c *Client) Apply(ctx context.Context, cfg Config) error {
	return c.do(ctx, http.MethodPut, cfg, nil)
}

func (c *Client) do(ctx context.Context, method string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://proxy/v1/config", r)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("reach the Zelie proxy: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
