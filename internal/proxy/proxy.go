package proxy

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/certmagic"
)

// Proxy serves the configured routes. Apply swaps in a new configuration
// without dropping open connections.
type Proxy struct {
	StateDir    string
	PanelSocket string // Unix socket the panel listens on
	Log         *slog.Logger

	mu      sync.Mutex // serialises Apply
	cfg     Config
	panelRT *http.Transport
	routes  atomic.Pointer[map[string]http.Handler]
	tls     atomic.Pointer[certSource]
}

// certSource answers TLS handshakes for one TLS mode.
type certSource struct {
	mode   string
	email  string
	cache  *certmagic.Cache
	magic  *certmagic.Config
	issuer *certmagic.ACMEIssuer
	hosts  map[string]bool

	selfMu     sync.Mutex
	selfSigned map[string]*tls.Certificate
}

// transport is shared by every route. It never uses a proxy from the
// environment: requests go straight to the container.
var transport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost:   32,
	IdleConnTimeout:       90 * time.Second,
	ResponseHeaderTimeout: 5 * time.Minute,
}

func (p *Proxy) configFile() string { return filepath.Join(p.StateDir, "config.json") }

// Load applies the configuration saved by the last Apply, so the proxy can
// start serving on its own after a reboot, before the panel is up.
func (p *Proxy) Load(ctx context.Context) error {
	b, err := os.ReadFile(p.configFile())
	if errors.Is(err, os.ErrNotExist) {
		return p.Apply(ctx, Config{TLS: TLSACME})
	}
	if err != nil {
		return err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("read %s: %w", p.configFile(), err)
	}
	return p.Apply(ctx, cfg)
}

func (p *Proxy) Config() Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

// Apply validates cfg, saves it and starts serving it.
func (p *Proxy) Apply(ctx context.Context, cfg Config) error {
	cfg.Routes = slices.Clone(cfg.Routes) // Validate normalises in place
	if err := cfg.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	routes := make(map[string]http.Handler, len(cfg.Routes)+1)
	hosts := make(map[string]bool, len(cfg.Routes)+1)
	for _, r := range cfg.Routes {
		routes[r.Host] = newReverseProxy(&url.URL{Scheme: "http", Host: r.Upstream}, transport, p.Log)
		hosts[r.Host] = true
	}
	if cfg.Panel != "" {
		if p.PanelSocket == "" {
			return errors.New("no panel socket configured")
		}
		if p.panelRT == nil {
			p.panelRT = unixTransport(p.PanelSocket)
		}
		routes[cfg.Panel] = newReverseProxy(&url.URL{Scheme: "http", Host: "panel"}, p.panelRT, p.Log)
		hosts[cfg.Panel] = true
	}

	src, err := p.certSourceFor(ctx, cfg, hosts)
	if err != nil {
		return err
	}

	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(p.configFile(), b); err != nil {
		return err
	}

	p.routes.Store(&routes)
	// A replaced certificate cache would otherwise keep renewing in the
	// background forever.
	if old := p.tls.Swap(src); old != nil && old.cache != nil && old.cache != src.cache {
		old.cache.Stop()
	}
	p.cfg = cfg
	p.Log.Info("proxy configuration applied", "routes", len(cfg.Routes), "tls", cfg.TLS)
	return nil
}

// certSourceFor reuses the current certificate setup when the mode and
// email are unchanged, and only adds or drops the hosts that changed.
func (p *Proxy) certSourceFor(ctx context.Context, cfg Config, hosts map[string]bool) (*certSource, error) {
	old := p.tls.Load()
	if cfg.TLS == TLSTunnel {
		return &certSource{mode: TLSTunnel, hosts: hosts}, nil
	}
	if cfg.TLS == TLSSelfSigned {
		src := &certSource{mode: TLSSelfSigned, hosts: hosts, selfSigned: map[string]*tls.Certificate{}}
		if old != nil && old.mode == TLSSelfSigned {
			src.selfSigned = old.selfSigned
		}
		return src, nil
	}

	src := &certSource{mode: TLSACME, email: cfg.Email, hosts: hosts}
	if old != nil && old.mode == TLSACME && old.email == cfg.Email {
		src.cache, src.magic, src.issuer = old.cache, old.magic, old.issuer
	} else {
		// Certificates are only requested for routed hosts, never on demand,
		// so a stranger pointing a domain at this server cannot make it ask
		// for one.
		var magic *certmagic.Config
		src.cache = certmagic.NewCache(certmagic.CacheOptions{
			GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) { return magic, nil },
		})
		magic = certmagic.New(src.cache, certmagic.Config{
			Storage: &certmagic.FileStorage{Path: filepath.Join(p.StateDir, "certificates")},
		})
		src.issuer = certmagic.NewACMEIssuer(magic, certmagic.ACMEIssuer{
			CA:     certmagic.LetsEncryptProductionCA,
			TestCA: certmagic.LetsEncryptStagingCA,
			Email:  cfg.Email,
			Agreed: true,
		})
		magic.Issuers = []certmagic.Issuer{src.issuer}
		src.magic = magic
	}

	var add, drop []string
	for h := range hosts {
		if old == nil || old.magic != src.magic || !old.hosts[h] {
			add = append(add, h)
		}
	}
	if old != nil && old.magic == src.magic {
		for h := range old.hosts {
			if !hosts[h] {
				drop = append(drop, h)
			}
		}
	}
	if len(drop) > 0 {
		subjects := make([]certmagic.SubjectIssuer, 0, len(drop))
		for _, h := range drop {
			subjects = append(subjects, certmagic.SubjectIssuer{Subject: h})
		}
		src.cache.RemoveManaged(subjects)
	}
	if len(add) > 0 {
		// Asynchronous: a domain whose DNS is not ready yet must not block
		// the others. certmagic keeps retrying and renewing in the background.
		if err := src.magic.ManageAsync(context.WithoutCancel(ctx), add); err != nil {
			return nil, err
		}
	}
	return src, nil
}

// unixTransport reaches the panel. It lives on the host and never listens on
// the network, so this is the only way in from outside.
func unixTransport(socket string) *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
	}
}

func newReverseProxy(target *url.URL, rt http.RoundTripper, log *slog.Logger) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.SetXForwarded()
			if tunnelled(r.In) {
				// The visitor reached Cloudflare over HTTPS; the tunnel's
				// connector says from where.
				r.Out.Header.Set("X-Forwarded-For", r.In.Header.Get("Cf-Connecting-Ip"))
				r.Out.Header.Set("X-Forwarded-Proto", "https")
			}
			r.Out.Header.Del("Cf-Connecting-Ip")
			// Apps expect the name the visitor used, not the container address.
			r.Out.Host = r.In.Host
		},
		Transport:     rt,
		FlushInterval: -1, // pass streamed responses through as they arrive
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("upstream unreachable", "host", r.Host, "upstream", target.Host, "err", err)
			http.Error(w, "The app behind this address is not responding.", http.StatusBadGateway)
		},
	}
}

// serveHTTPS routes a request by its host name.
func (p *Proxy) serveHTTPS(w http.ResponseWriter, r *http.Request) {
	if h := p.route(r.Host); h != nil {
		h.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// serveHTTP answers ACME challenges and sends everything else to HTTPS.
// Behind a tunnel it serves the routes itself.
func (p *Proxy) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if src := p.tls.Load(); src != nil && src.mode == TLSTunnel {
		p.serveTunnel(w, r)
		return
	}
	if src := p.tls.Load(); src != nil && src.issuer != nil && src.issuer.HandleHTTPChallenge(w, r) {
		return
	}
	if p.route(r.Host) == nil {
		http.NotFound(w, r)
		return
	}
	target := url.URL{Scheme: "https", Host: stripPort(r.Host), Path: r.URL.Path, RawQuery: r.URL.RawQuery}
	http.Redirect(w, r, target.String(), http.StatusPermanentRedirect)
}

type tunnelKey struct{}

// serveTunnel routes a request that came through the tunnel's connector.
// Only a connection from this machine is the connector: the address of the
// visitor it passes on is believed from nobody else.
func (p *Proxy) serveTunnel(w http.ResponseWriter, r *http.Request) {
	h := p.route(r.Host)
	if h == nil {
		http.NotFound(w, r)
		return
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil && ap.Addr().IsLoopback() {
		if ip, err := netip.ParseAddr(r.Header.Get("Cf-Connecting-Ip")); err == nil {
			r.Header.Set("Cf-Connecting-Ip", ip.String())
			r = r.WithContext(context.WithValue(r.Context(), tunnelKey{}, true))
		}
	}
	h.ServeHTTP(w, r)
}

func tunnelled(r *http.Request) bool {
	ok, _ := r.Context().Value(tunnelKey{}).(bool)
	return ok
}

func (p *Proxy) route(host string) http.Handler {
	routes := p.routes.Load()
	if routes == nil {
		return nil
	}
	return (*routes)[strings.ToLower(stripPort(host))]
}

func (p *Proxy) getCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	src := p.tls.Load()
	if src == nil {
		return nil, errors.New("no TLS configuration")
	}
	name := strings.ToLower(hello.ServerName)
	if src.mode == TLSACME {
		if !src.hosts[name] {
			return nil, fmt.Errorf("no route for %q", name)
		}
		return src.magic.GetCertificate(hello)
	}
	if name == "" {
		// Clients connecting to a bare IP send no server name.
		if a, err := netip.ParseAddrPort(hello.Conn.LocalAddr().String()); err == nil {
			name = a.Addr().Unmap().String()
		}
	}
	if !src.hosts[name] {
		return nil, fmt.Errorf("no route for %q", name)
	}
	src.selfMu.Lock()
	defer src.selfMu.Unlock()
	if c, ok := src.selfSigned[name]; ok {
		return c, nil
	}
	c, err := selfSignedCert(name)
	if err != nil {
		return nil, err
	}
	src.selfSigned[name] = c
	return c, nil
}

func selfSignedCert(name string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip, err := netip.ParseAddr(name); err == nil {
		tmpl.IPAddresses = []net.IP{ip.AsSlice()}
	} else {
		tmpl.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.Trim(h, "[]")
	}
	return host
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
