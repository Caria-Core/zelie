package proxy

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
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
