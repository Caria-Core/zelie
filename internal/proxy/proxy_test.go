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
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/coder/websocket"
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

func TestStripPort(t *testing.T) {
	for host, want := range map[string]string{
		"app.example.com":      "app.example.com",
		"app.example.com:8443": "app.example.com",
		"203.0.113.9:443":      "203.0.113.9",
		"[2001:db8::1]":        "2001:db8::1",
		"[2001:db8::1]:443":    "2001:db8::1",
		"2001:db8::1":          "2001:db8::1",
		"[2001:db8::1]:":       "2001:db8::1",
		"":                     "",
	} {
		if got := stripPort(host); got != want {
			t.Errorf("stripPort(%q) = %q, want %q", host, got, want)
		}
	}
}

func TestURLHost(t *testing.T) {
	for host, want := range map[string]string{
		"app.example.com": "app.example.com",
		"203.0.113.9":     "203.0.113.9",
		"2001:db8::1":     "[2001:db8::1]",
		"::ffff:10.0.0.1": "[::ffff:10.0.0.1]",
	} {
		if got := URLHost(host); got != want {
			t.Errorf("URLHost(%q) = %q, want %q", host, got, want)
		}
	}
}

// A browser at https://[2001:db8::1]/ sends the address in brackets and
// leaves the default port out of the Host header.
func TestIPv6Host(t *testing.T) {
	p, appURL := newTestProxy(t)
	cfg := Config{TLS: TLSSelfSigned, Routes: []Route{{"2001:db8::1", strings.TrimPrefix(appURL, "http://")}}}
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"[2001:db8::1]", "[2001:db8::1]:8443"} {
		req := httptest.NewRequest("GET", "https://"+host+"/x", nil)
		rec := httptest.NewRecorder()
		p.serveHTTPS(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "host="+host) {
			t.Errorf("%s: %d %q", host, rec.Code, rec.Body)
		}
	}

	req := httptest.NewRequest("GET", "http://[2001:db8::1]:8080/a?b=c", nil)
	rec := httptest.NewRecorder()
	p.serveHTTP(rec, req)
	if rec.Code != http.StatusPermanentRedirect || rec.Header().Get("Location") != "https://[2001:db8::1]/a?b=c" {
		t.Errorf("redirect: %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

// Every configuration swap hands the certificates made so far to the next
// one. Handshakes that began before the swap and after it must not write
// the same map under different locks.
func TestSelfSignedCertificatesAreSharedSafely(t *testing.T) {
	p, appURL := newTestProxy(t)
	upstream := strings.TrimPrefix(appURL, "http://")
	var routes []Route
	var names []string
	for i := 1; i <= 40; i++ {
		name := "203.0.113." + strconv.Itoa(i)
		names = append(names, name)
		routes = append(routes, Route{name, upstream})
	}
	cfg := Config{TLS: TLSSelfSigned, Routes: routes}
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	before := p.tls.Load()
	if err := p.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if after := p.tls.Load(); after == before || after.self != before.self {
		t.Fatal("a new configuration does not take over the certificates of the one before")
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := p.getCertificate(&tls.ClientHelloInfo{ServerName: names[i%len(names)]}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	for range 20 {
		if err := p.Apply(context.Background(), cfg); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	wg.Wait()
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

func TestWebSocketToThePanel(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "panel.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	panel := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		// The panel checks the origin against the host it is given.
		c.Write(r.Context(), websocket.MessageText, []byte("host="+r.Host+" origin="+r.Header.Get("Origin")+" xff="+r.Header.Get("X-Forwarded-For")))
		for {
			typ, b, err := c.Read(r.Context())
			if err != nil {
				return
			}
			c.Write(r.Context(), typ, b)
		}
	}), ReadHeaderTimeout: 100 * time.Millisecond, IdleTimeout: 100 * time.Millisecond}
	go panel.Serve(l)
	t.Cleanup(func() { panel.Close() })

	p := &Proxy{StateDir: t.TempDir(), PanelSocket: sock, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := p.Apply(context.Background(), Config{TLS: TLSSelfSigned, Panel: "panel.example.com"}); err != nil {
		t.Fatal(err)
	}
	// The servers' header and idle timeouts, made short, must not touch a
	// socket that has been upgraded.
	front := httptest.NewUnstartedServer(http.HandlerFunc(p.serveHTTPS))
	front.Config.ReadHeaderTimeout, front.Config.IdleTimeout = 100*time.Millisecond, 100*time.Millisecond
	front.Start()
	t.Cleanup(front.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(front.URL, "http")+"/socket", &websocket.DialOptions{
		Host:       "panel.example.com",
		HTTPHeader: http.Header{"Origin": {"https://panel.example.com"}},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()
	_, b, err := c.Read(ctx)
	if err != nil || !strings.HasPrefix(string(b), "host=panel.example.com origin=https://panel.example.com xff=127.0.0.1") {
		t.Fatalf("panel saw %q: %v", b, err)
	}
	for _, word := range []string{"one", "two"} {
		time.Sleep(250 * time.Millisecond)
		if err := c.Write(ctx, websocket.MessageText, []byte(word)); err != nil {
			t.Fatal(err)
		}
		if _, b, err := c.Read(ctx); err != nil || string(b) != word {
			t.Fatalf("echo %q: %v", b, err)
		}
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

// requestsOf replaces the certificate requests with ones that only record
// their context, and returns what the latest request for a host was given.
func requestsOf(t *testing.T) func(host string) context.Context {
	t.Helper()
	var mu sync.Mutex
	started := map[string]context.Context{}
	old := manageAsync
	manageAsync = func(ctx context.Context, _ *certmagic.Config, hosts []string) error {
		mu.Lock()
		defer mu.Unlock()
		for _, h := range hosts {
			started[h] = ctx
		}
		return nil
	}
	t.Cleanup(func() { manageAsync = old })
	return func(host string) context.Context {
		mu.Lock()
		defer mu.Unlock()
		return started[host]
	}
}

// A certificate request retries for days, so it has to end with the host it
// was for.
func TestCertificateRequestsEndWithTheirHost(t *testing.T) {
	ctxOf := requestsOf(t)

	p := &Proxy{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	route := func(host string) Route { return Route{host, "10.210.0.4:8080"} }
	apply := func(email string, routes ...Route) {
		t.Helper()
		// The request that applies a configuration is over long before the
		// certificate comes.
		reqCtx, end := context.WithCancel(context.Background())
		err := p.Apply(reqCtx, Config{Email: email, Routes: routes})
		end()
		if err != nil {
			t.Fatal(err)
		}
	}

	apply("a@b.co", route("shop.example.com"), route("blog.example.com"))
	shop, blog := ctxOf("shop.example.com"), ctxOf("blog.example.com")
	if shop == nil || blog == nil {
		t.Fatal("certificates were not requested for both hosts")
	}
	if shop.Err() != nil || blog.Err() != nil {
		t.Fatal("a request ended with the request that applied the configuration")
	}

	// The same host again is not asked for twice.
	apply("a@b.co", route("shop.example.com"))
	if ctxOf("shop.example.com") != shop || shop.Err() != nil {
		t.Error("a host that stayed was asked for again or stopped")
	}
	if blog.Err() == nil {
		t.Error("the dropped host is still asking for a certificate")
	}

	// Another address for the certificate authority starts over, and ends
	// every request of the old one.
	apply("c@d.co", route("shop.example.com"))
	if shop.Err() == nil {
		t.Error("a request survived a change of email")
	}
	if again := ctxOf("shop.example.com"); again == shop || again.Err() != nil {
		t.Error("the host was not asked for again under the new email")
	}

	// A host that comes back is asked for again.
	apply("c@d.co")
	gone := ctxOf("shop.example.com")
	if gone.Err() == nil {
		t.Error("the last host was dropped and still asks")
	}
	apply("c@d.co", route("shop.example.com"))
	if back := ctxOf("shop.example.com"); back == gone || back.Err() != nil {
		t.Error("a host that came back was not asked for again")
	}
}

// A configuration that cannot be saved is not applied, so the hosts it
// brought are not asked for either; the next try starts them.
func TestFailedApplyEndsTheRequestsItStarted(t *testing.T) {
	ctxOf := requestsOf(t)
	p := &Proxy{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	route := func(host string) Route { return Route{host, "10.210.0.4:8080"} }
	ctx := context.Background()
	if err := p.Apply(ctx, Config{Email: "a@b.co", Routes: []Route{route("shop.example.com")}}); err != nil {
		t.Fatal(err)
	}
	shop := ctxOf("shop.example.com")

	// A directory where the file goes makes the save fail.
	if err := os.Remove(p.configFile()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p.configFile(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, Config{Email: "a@b.co", Routes: []Route{route("shop.example.com"), route("blog.example.com")}}); err == nil {
		t.Fatal("the configuration was applied though it could not be saved")
	}
	failed := ctxOf("blog.example.com")
	if failed == nil || failed.Err() == nil {
		t.Error("the host of a configuration that was not applied still asks for a certificate")
	}
	if shop.Err() != nil {
		t.Error("a host that was already served stopped asking")
	}

	if err := os.Remove(p.configFile()); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, Config{Email: "a@b.co", Routes: []Route{route("shop.example.com"), route("blog.example.com")}}); err != nil {
		t.Fatal(err)
	}
	if again := ctxOf("blog.example.com"); again == failed || again.Err() != nil {
		t.Error("the host was not asked for again once the configuration was applied")
	}
}
