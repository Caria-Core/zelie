package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		ok   bool
	}{
		{"empty", Config{}, true},
		{"acme route", Config{Email: "a@b.co", Routes: []Route{{"App.Example.com.", "10.210.0.4:8080"}}}, true},
		{"acme without email", Config{Routes: []Route{{"app.example.com", "10.210.0.4:8080"}}}, false},
		{"upstream on the host", Config{TLS: TLSSelfSigned, Routes: []Route{{"app.example.com", "127.0.0.1:22"}}}, false},
		{"upstream outside containers", Config{TLS: TLSSelfSigned, Routes: []Route{{"app.example.com", "192.168.1.10:80"}}}, false},
		{"no port", Config{TLS: TLSSelfSigned, Routes: []Route{{"app.example.com", "10.210.0.4"}}}, false},
		{"bad host", Config{TLS: TLSSelfSigned, Routes: []Route{{"app_example", "10.210.0.4:80"}}}, false},
		{"duplicate host", Config{TLS: TLSSelfSigned, Routes: []Route{{"a.example.com", "10.210.0.4:80"}, {"A.example.com", "10.210.0.5:80"}}}, false},
		{"ip with acme", Config{Email: "a@b.co", Routes: []Route{{"203.0.113.9", "10.210.0.4:80"}}}, false},
		{"ip self-signed", Config{TLS: TLSSelfSigned, Routes: []Route{{"203.0.113.9", "10.210.0.4:80"}}}, true},
		{"unknown mode", Config{TLS: "none"}, false},
		{"panel", Config{Email: "a@b.co", Panel: "panel.example.com"}, true},
		{"panel needs email", Config{Panel: "panel.example.com"}, false},
		{"panel host also routed", Config{TLS: TLSSelfSigned, Panel: "a.example.com", Routes: []Route{{"a.example.com", "10.210.0.4:80"}}}, false},
		{"panel on an ip", Config{TLS: TLSSelfSigned, Panel: "203.0.113.9"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err == nil) != c.ok {
				t.Errorf("Validate() = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

// newTestProxy points upstreams at loopback so a local server can stand in
// for a container.
func newTestProxy(t *testing.T) (*Proxy, string) {
	t.Helper()
	old := upstreams
	upstreams = netip.MustParsePrefix("127.0.0.0/8")
	t.Cleanup(func() { upstreams = old })

	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/fail":
			w.WriteHeader(http.StatusInternalServerError)
		}
		io.WriteString(w, "host="+r.Host+" xff="+r.Header.Get("X-Forwarded-For")+" proto="+r.Header.Get("X-Forwarded-Proto"))
	}))
	t.Cleanup(app.Close)

	p := &Proxy{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg := Config{TLS: TLSSelfSigned, Routes: []Route{{"app.example.com", strings.TrimPrefix(app.URL, "http://")}}}
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	return p, app.URL
}

func TestRouting(t *testing.T) {
	p, _ := newTestProxy(t)

	req := httptest.NewRequest("GET", "https://app.example.com/x", nil)
	req.RemoteAddr = "198.51.100.7:5555"
	rec := httptest.NewRecorder()
	p.serveHTTPS(rec, req)
	if got := rec.Body.String(); !strings.Contains(got, "host=app.example.com") || !strings.Contains(got, "xff=198.51.100.7") || !strings.Contains(got, "proto=https") {
		t.Errorf("app saw %q", got)
	}

	rec = httptest.NewRecorder()
	p.serveHTTPS(rec, httptest.NewRequest("GET", "https://other.example.com/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown host: status %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	p.serveHTTP(rec, httptest.NewRequest("GET", "http://app.example.com/a?b=c", nil))
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://app.example.com/a?b=c" {
		t.Errorf("http redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSelfSignedOnlyForRoutedHosts(t *testing.T) {
	p, _ := newTestProxy(t)
	if _, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: "app.example.com"}); err != nil {
		t.Errorf("routed host: %v", err)
	}
	if _, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Error("got a certificate for a host that is not routed")
	}
}

func TestConfigSurvivesRestart(t *testing.T) {
	p, _ := newTestProxy(t)
	q := &Proxy{StateDir: p.StateDir, Log: p.Log}
	if err := q.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := q.Config().Routes; len(got) != 1 || got[0].Host != "app.example.com" {
		t.Errorf("loaded routes %+v", got)
	}
}

func TestPanelOverUnixSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "panel.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	panel := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "panel host="+r.Host+" xff="+r.Header.Get("X-Forwarded-For"))
	})}
	go panel.Serve(l)
	t.Cleanup(func() { panel.Close() })

	p := &Proxy{StateDir: t.TempDir(), PanelSocket: sock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := p.Apply(context.Background(), Config{TLS: TLSSelfSigned, Panel: "Panel.Example.com"}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "https://panel.example.com/", nil)
	req.RemoteAddr = "198.51.100.7:5555"
	req.Header.Set("X-Forwarded-For", "10.0.0.1") // a visitor must not be able to pick their own address
	rec := httptest.NewRecorder()
	p.serveHTTPS(rec, req)
	if got := rec.Body.String(); got != "panel host=panel.example.com xff=198.51.100.7" {
		t.Errorf("panel saw %q", got)
	}
	if _, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: "panel.example.com"}); err != nil {
		t.Errorf("panel host: %v", err)
	}
}

func TestTunnel(t *testing.T) {
	p, app := newTestProxy(t)
	cfg := Config{TLS: TLSTunnel, Routes: []Route{{"app.example.com", strings.TrimPrefix(app, "http://")}}}
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	get := func(remote, cf string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "http://app.example.com/a", nil)
		req.RemoteAddr = remote
		if cf != "" {
			req.Header.Set("Cf-Connecting-Ip", cf)
		}
		rec := httptest.NewRecorder()
		p.serveHTTP(rec, req)
		return rec
	}

	// From the connector on this machine: served, not redirected, with the
	// visitor's address.
	rec := get("127.0.0.1:40000", "203.0.113.9")
	if got := rec.Body.String(); rec.Code != http.StatusOK || !strings.Contains(got, "xff=203.0.113.9 ") || !strings.Contains(got, "proto=https") {
		t.Errorf("through the tunnel: %d %q", rec.Code, got)
	}
	// Anyone else claiming to be Cloudflare is not believed.
	rec = get("198.51.100.7:5555", "203.0.113.9")
	if got := rec.Body.String(); !strings.Contains(got, "xff=198.51.100.7 ") || !strings.Contains(got, "proto=http") {
		t.Errorf("from elsewhere: %q", got)
	}
	// Nor is a header that is not an address.
	rec = get("127.0.0.1:40000", "1.2.3.4, 5.6.7.8")
	if got := rec.Body.String(); !strings.Contains(got, "xff=127.0.0.1 ") {
		t.Errorf("bad header: %q", got)
	}
	if rec := get("127.0.0.1:40000", ""); rec.Code != http.StatusOK {
		t.Errorf("no header: %d", rec.Code)
	}
	req := httptest.NewRequest("GET", "http://other.example.com/", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	rec = httptest.NewRecorder()
	p.serveHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown host: %d", rec.Code)
	}
}

func TestCounting(t *testing.T) {
	p, _ := newTestProxy(t)
	get := func(host, path string) {
		rec := httptest.NewRecorder()
		p.serveHTTPS(rec, httptest.NewRequest("GET", "https://"+host+path, nil))
	}
	get("app.example.com", "/")
	get("app.example.com", "/missing")
	get("app.example.com", "/fail")
	get("other.example.com", "/") // no route: not an app's request

	// The config changes; the counts go on.
	cfg := Config{TLS: TLSSelfSigned, Routes: []Route{{"app.example.com", "127.0.0.1:1"}}}
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	get("app.example.com", "/") // nothing answers there: 502

	st := p.Stats()
	want := Counts{Requests: 4, ClientErrors: 1, ServerErrors: 2}
	if got := st.Hosts["app.example.com"]; got != want || len(st.Hosts) != 1 {
		t.Errorf("stats %+v, want %+v", st.Hosts, want)
	}
}
