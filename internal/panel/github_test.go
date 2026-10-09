package panel

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/store"
)

// fakeGitHub answers the parts of GitHub's API the panel uses. It checks
// that App endpoints get a JWT signed with the App's key and repository
// endpoints get an installation token.
type fakeGitHub struct {
	t      *testing.T
	key    *rsa.PrivateKey
	srv    *httptest.Server
	secret string

	mu         sync.Mutex
	installed  map[string]bool // repositories the App was given
	commit     string
	statuses   []string
	deliveries []map[string]any
	noHook     bool // GitHub answers 404 for the deliveries of an App without a webhook
}

const fakeToken = "ghs_installation"

func newFakeGitHub(t *testing.T) *fakeGitHub {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGitHub{t: t, key: key, secret: "whsec", installed: map[string]bool{"owner/private": true}, commit: strings.Repeat("c", 40)}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGitHub) appAuth(r *http.Request) bool {
	jwt, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if !ok || len(parts) != 3 {
		return false
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(&g.key.PublicKey, crypto.SHA256, sum[:], sig) == nil
}

func (g *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	reply := func(code int, v any) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/app-manifests/good-code/conversions":
		reply(http.StatusCreated, map[string]any{
			"id": 99, "slug": "zelie-panel", "html_url": "https://github.com/apps/zelie-panel", "webhook_secret": g.secret,
			"pem":   string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(g.key)})),
			"owner": map[string]string{"login": "efe"},
		})
		return
	case strings.HasPrefix(p, "/app"):
		if !g.appAuth(r) {
			reply(http.StatusUnauthorized, map[string]string{"message": "bad jwt"})
			return
		}
		switch {
		case p == "/app/installations":
			reply(http.StatusOK, []map[string]any{{"id": 5, "account": map[string]string{"login": "efe"}, "repository_selection": "selected"}})
		case p == "/app/installations/5/access_tokens":
			reply(http.StatusCreated, map[string]any{"token": fakeToken, "expires_at": time.Now().Add(time.Hour)})
		case p == "/app/hook/deliveries" && g.noHook:
			reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
		case p == "/app/hook/deliveries":
			reply(http.StatusOK, g.deliveries)
		default:
			http.NotFound(w, r)
		}
		return
	case strings.HasPrefix(p, "/repos/") && strings.HasSuffix(p, "/installation"):
		if !g.appAuth(r) {
			reply(http.StatusUnauthorized, nil)
			return
		}
		if !g.installed[strings.TrimSuffix(strings.TrimPrefix(p, "/repos/"), "/installation")] {
			reply(http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		reply(http.StatusOK, map[string]any{"id": 5})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+fakeToken {
		reply(http.StatusUnauthorized, map[string]string{"message": "bad token"})
		return
	}
	switch {
	case p == "/installation/repositories":
		reply(http.StatusOK, map[string]any{"repositories": []map[string]any{
			{"full_name": "owner/private", "private": true, "default_branch": "main"},
			{"full_name": "Owner/another", "private": false, "default_branch": "trunk"},
		}})
	case p == "/repos/owner/private/commits/main":
		w.Write([]byte(g.commit))
	case strings.HasPrefix(p, "/repos/owner/private/tarball/"):
		w.Write([]byte("private tarball"))
	case strings.HasPrefix(p, "/repos/owner/private/statuses/"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		g.statuses = append(g.statuses, body["state"]+" "+strings.TrimPrefix(p, "/repos/owner/private/statuses/")[:4]+" "+body["target_url"])
		reply(http.StatusCreated, nil)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGitHub) statusList() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return strings.Join(g.statuses, "; ")
}

// connect goes through the manifest flow as the interface would.
func (e *appEnv) connect(t *testing.T, g *fakeGitHub) {
	t.Helper()
	e.s.GitHubAPI, e.s.GitHubWeb, e.s.GitHubHTTP = g.srv.URL, "https://github.example", g.srv.Client()
	code, out := e.b.do("POST", "/api/github/manifest", map[string]string{})
	if code != http.StatusOK {
		t.Fatalf("manifest: %d %v", code, out)
	}
	u, _ := url.Parse(out["action"].(string))
	if code, out := e.b.do("POST", "/api/github/app", map[string]string{"code": "good-code", "state": u.Query().Get("state")}); code != http.StatusCreated {
		t.Fatalf("created: %d %v", code, out)
	}
}

// hook sends a webhook the way GitHub does.
func (e *appEnv) hook(event, delivery string, payload any, secret string) (int, map[string]any) {
	body, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	req := httptest.NewRequest("POST", "https://panel.example.com/api/github/webhook", bytes.NewReader(body))
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: proxyUID}))
	req.Header.Set("X-Forwarded-For", "140.82.115.1")
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.b.h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func push(ref, after, message string) map[string]any {
	return map[string]any{"ref": ref, "after": after, "repository": map[string]string{"full_name": "Owner/Private"},
		"head_commit": map[string]string{"message": message}}
}

func TestConnectGitHub(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.s.GitHubAPI, e.s.GitHubWeb, e.s.GitHubHTTP = g.srv.URL, "https://github.example", g.srv.Client()

	_, out := e.b.do("GET", "/api/github", nil)
	if out["connected"] != false {
		t.Fatalf("status before: %v", out)
	}
	code, out := e.b.do("POST", "/api/github/manifest", map[string]string{"org": "Caria-Core"})
	if code != http.StatusOK {
		t.Fatalf("manifest: %d %v", code, out)
	}
	if !strings.HasPrefix(out["action"].(string), "https://github.example/organizations/Caria-Core/settings/apps/new?state=") {
		t.Errorf("action %v", out["action"])
	}
	var m map[string]any
	json.Unmarshal([]byte(out["manifest"].(string)), &m)
	if m["hook_attributes"].(map[string]any)["url"] != "https://panel.example.com/api/github/webhook" || m["redirect_url"] != "https://panel.example.com/github/created" {
		t.Errorf("manifest %v", m)
	}
	if perms := m["default_permissions"].(map[string]any); len(perms) != 3 || perms["contents"] != "read" || perms["statuses"] != "write" {
		t.Errorf("permissions %v", perms)
	}

	// The state ties GitHub's answer to this session; a wrong one ends the
	// attempt.
	if code, _ := e.b.do("POST", "/api/github/app", map[string]string{"code": "good-code", "state": "forged"}); code != http.StatusBadRequest {
		t.Fatalf("forged state: %d", code)
	}
	u, _ := url.Parse(out["action"].(string))
	if code, _ := e.b.do("POST", "/api/github/app", map[string]string{"code": "good-code", "state": u.Query().Get("state")}); code != http.StatusBadRequest {
		t.Fatalf("state reused after a failed attempt: %d", code)
	}
	if code, _ := e.b.do("POST", "/api/github/manifest", map[string]string{"org": "bad/org"}); code != http.StatusBadRequest {
		t.Errorf("bad org: %d", code)
	}

	e.connect(t, g)
	if code, _ := e.b.do("POST", "/api/github/manifest", map[string]string{}); code != http.StatusConflict {
		t.Errorf("connected twice: %d", code)
	}
	app, _ := e.s.Store.GitHubApp(context.Background())
	if bytes.Contains(app.Key, []byte("PRIVATE KEY")) || bytes.Contains(app.WebhookSecret, []byte(g.secret)) {
		t.Error("the App's key or secret is stored in the clear")
	}

	g.deliveries = []map[string]any{{"event": "push", "status_code": 502, "status": "Bad Gateway", "delivered_at": time.Now()}}
	_, out = e.b.do("GET", "/api/github", nil)
	if out["connected"] != true || out["slug"] != "zelie-panel" || out["install_url"] != "https://github.example/apps/zelie-panel/installations/new" {
		t.Fatalf("status: %v", out)
	}
	if hook := out["webhook"].(map[string]any); hook["state"] != "failing" || hook["status"] != "502 Bad Gateway" {
		t.Errorf("webhook %v", hook)
	}
	if insts := out["installations"].([]any); len(insts) != 1 {
		t.Errorf("installations %v", insts)
	}

	if code, _ := e.b.do("GET", "/api/github/repos", nil); code != http.StatusOK {
		t.Fatalf("repos: %d", code)
	}

	if code, _ := e.b.do("DELETE", "/api/github", nil); code != http.StatusNoContent {
		t.Fatalf("disconnect: %d", code)
	}
	if _, out := e.b.do("GET", "/api/github", nil); out["connected"] != false {
		t.Error("still connected")
	}
}

func TestPushDeploys(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.connect(t, g)

	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/private"})
	first := e.settle(t, "web")
	if first.State != store.DeployLive || first.Version != g.commit || first.Cause != store.CauseManual {
		t.Fatalf("first deployment %+v", first)
	}
	if e.core.builds[0] != fmt.Sprintf("web:cccccccccccc-%d:private tarball", first.ID) {
		t.Errorf("build %q", e.core.builds[0])
	}
	if got := g.statusList(); got != "pending cccc https://panel.example.com/a/web; success cccc https://panel.example.com/a/web" {
		t.Errorf("statuses %q", got)
	}

	after := strings.Repeat("d", 40)
	if code, _ := e.hook("push", "d1", push("refs/heads/main", after, "Fix the header\n\nlonger text"), "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("bad signature: %d", code)
	}
	if code, out := e.hook("push", "d1", push("refs/heads/main", after, "Fix the header\n\nlonger text"), g.secret); code != http.StatusAccepted || out["apps"] != 1.0 {
		t.Fatalf("push: %d %v", code, out)
	}
	d := e.settle(t, "web")
	if d.State != store.DeployLive || d.Version != after || d.Cause != store.CausePush || d.Message != "Fix the header" {
		t.Fatalf("push deployment %+v", d)
	}
	// A push deploys the pushed commit, not whatever the branch holds now.
	if last := e.core.builds[len(e.core.builds)-1]; last != fmt.Sprintf("web:dddddddddddd-%d:private tarball", d.ID) {
		t.Errorf("build %q", last)
	}

	count := func() int {
		list, _ := e.s.Store.Deployments(context.Background(), "web", 100)
		return len(list)
	}
	n := count()
	if _, out := e.hook("push", "d1", push("refs/heads/main", after, "again"), g.secret); out["result"] != "already handled" {
		t.Errorf("replayed delivery: %v", out)
	}
	e.hook("push", "d2", push("refs/heads/other", after, ""), g.secret)
	e.hook("push", "d3", push("refs/tags/v1", after, ""), g.secret)
	e.hook("push", "d4", map[string]any{"ref": "refs/heads/main", "after": strings.Repeat("0", 40), "deleted": true, "repository": map[string]string{"full_name": "owner/private"}}, g.secret)
	e.hook("ping", "d5", map[string]any{}, g.secret)
	f := false
	if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"auto_deploy": f}); code != http.StatusNoContent {
		t.Fatalf("turn off: %d", code)
	}
	e.hook("push", "d6", push("refs/heads/main", strings.Repeat("e", 40), ""), g.secret)
	e.s.deploys.wg.Wait()
	if count() != n {
		t.Errorf("%d deployments started that should not have", count()-n)
	}
}

func TestPublicRepoWithGitHubConnected(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.connect(t, g)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "someone/public"})
	d := e.settle(t, "web")
	if d.State != store.DeployLive {
		t.Fatalf("deployment %+v", d)
	}
	if e.core.builds[0] != fmt.Sprintf("web:aaaaaaaaaaaa-%d:tarball", d.ID) {
		t.Errorf("build %q", e.core.builds[0])
	}
	if g.statusList() != "" {
		t.Error("marked a commit in a repository the App was not given")
	}
	if code, _ := e.b.do("PATCH", "/api/apps/web", map[string]any{"auto_deploy": true}); code != http.StatusNoContent {
		t.Error("auto deploy on a GitHub app")
	}
	e.b.do("POST", "/api/apps", map[string]any{"id": "img", "source": "image", "image": "nginx"})
	if code, _ := e.b.do("PATCH", "/api/apps/img", map[string]any{"auto_deploy": true}); code != http.StatusBadRequest {
		t.Error("auto deploy on an image app")
	}
}

func TestOnlyTheNewestQueuedDeploymentRuns(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/web"})
	e.settle(t, "web")

	unlock := e.s.deploys.lock("web")
	e.b.do("POST", "/api/apps/web/deployments", nil)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	e.b.do("POST", "/api/apps/web/deployments", nil)
	unlock()
	e.s.deploys.wg.Wait()

	list, _ := e.s.Store.Deployments(context.Background(), "web", 3)
	var states []string
	for _, d := range list {
		states = append(states, d.State)
	}
	if got := strings.Join(states, " "); got != "live skipped skipped" {
		t.Errorf("states newest first: %s", got)
	}
}

func TestAppWithoutWebhookSecret(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	g.secret = "" // GitHub gives none to an App without a webhook
	e.connect(t, g)
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "github", "repo": "owner/private"})
	e.settle(t, "web")
	if code, _ := e.hook("push", "x1", push("refs/heads/main", strings.Repeat("d", 40), ""), ""); code != http.StatusUnauthorized {
		t.Errorf("a webhook signed with no secret: %d", code)
	}
}

func TestStatusWithoutWebhook(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.connect(t, g)
	g.deliveries = nil
	g.mu.Lock()
	g.noHook = true
	g.mu.Unlock()
	_, out := e.b.do("GET", "/api/github", nil)
	if out["error"] != nil || out["webhook"].(map[string]any)["state"] != "none" {
		t.Errorf("status of an App without a webhook: %v", out)
	}
}

// countingBody counts what is read from it, and fails after its bytes.
type countingBody struct {
	r io.Reader
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// unsignedHook sends a webhook that nobody has signed yet.
func (e *appEnv) unsignedHook(body io.Reader, length int64, signature string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "https://panel.example.com/api/github/webhook", body)
	req.ContentLength = length
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: proxyUID}))
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", "x")
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	e.b.h.ServeHTTP(rec, req)
	return rec
}

// Anyone can send a webhook, and its signature can only be checked once the
// body is here. What the panel holds for such a request is limited.
func TestWebhookBodiesAreLimitedBeforeTheyAreSigned(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.connect(t, g)
	shaped := "sha256=" + strings.Repeat("ab", 32)

	for _, sig := range []string{"", "sha256=", "sha1=" + strings.Repeat("ab", 20), "sha256=" + strings.Repeat("ab", 31), "sha256=" + strings.Repeat("zz", 32)} {
		body := &countingBody{r: strings.NewReader(strings.Repeat("x", 1000))}
		if rec := e.unsignedHook(body, 1000, sig); rec.Code != http.StatusUnauthorized || body.n != 0 {
			t.Errorf("signature %q: %d, %d bytes read", sig, rec.Code, body.n)
		}
	}

	body := &countingBody{r: strings.NewReader("x")}
	if rec := e.unsignedHook(body, maxWebhook+1, shaped); rec.Code != http.StatusRequestEntityTooLarge || body.n != 0 {
		t.Errorf("a payload that says it is too large: %d, %d bytes read", rec.Code, body.n)
	}
	// One that does not say how large it is.
	body = &countingBody{r: io.LimitReader(zeroReader{}, maxWebhook+100)}
	if rec := e.unsignedHook(body, -1, shaped); rec.Code != http.StatusRequestEntityTooLarge || body.n > maxWebhook+(64<<10) {
		t.Errorf("a payload of unknown size: %d, %d bytes read", rec.Code, body.n)
	}
	if rec := e.unsignedHook(iotest.ErrReader(errors.New("connection reset")), -1, shaped); rec.Code != http.StatusRequestTimeout {
		t.Errorf("a payload that stopped: %d", rec.Code)
	}
	if rec := e.unsignedHook(strings.NewReader("{}"), 2, shaped); rec.Code != http.StatusUnauthorized {
		t.Errorf("a payload with a bad signature: %d", rec.Code)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// gatedBody blocks its first read until it is released.
type gatedBody struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *gatedBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return 0, io.EOF
}

func TestOnlyAFewWebhookBodiesAreReadAtOnce(t *testing.T) {
	e := newAppEnv(t)
	g := newFakeGitHub(t)
	e.connect(t, g)
	shaped := "sha256=" + strings.Repeat("ab", 32)
	defer func(d time.Duration) { webhookWait = d }(webhookWait)
	webhookWait = 50 * time.Millisecond

	release := make(chan struct{})
	var wg sync.WaitGroup
	codes := make(chan int, webhookReaders)
	for range webhookReaders {
		body := &gatedBody{started: make(chan struct{}), release: release}
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- e.unsignedHook(body, 100, shaped).Code
		}()
		select {
		case <-body.started:
		case <-time.After(5 * time.Second):
			t.Fatal("a payload was never read")
		}
	}

	// Their turn does not come while the others are slow.
	body := &countingBody{r: strings.NewReader("{}")}
	if rec := e.unsignedHook(body, 2, shaped); rec.Code != http.StatusServiceUnavailable || body.n != 0 {
		t.Errorf("one more payload: %d, %d bytes read", rec.Code, body.n)
	}
	close(release)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusUnauthorized {
			t.Errorf("a slow payload ended with %d", code)
		}
	}

	// Once they are done, the next one is read as usual.
	if rec := e.unsignedHook(strings.NewReader("{}"), 2, shaped); rec.Code != http.StatusUnauthorized {
		t.Errorf("after the others: %d", rec.Code)
	}
}

// Senders that hold back their bodies are let go of after a while, or at
// once when the body was not going to be read. The recorder the other tests
// use has no deadlines and no connection to wait on, so this goes over a
// real one.
func TestSlowWebhookBodyIsCutOff(t *testing.T) {
	e := newAppEnv(t)
	e.connect(t, newFakeGitHub(t))
	defer func(d time.Duration) { webhookReadTime = d }(webhookReadTime)
	webhookReadTime = 200 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(peer.WithPeer(r.Context(), peer.Peer{UID: proxyUID}))
		e.b.h.ServeHTTP(w, r)
	}))
	defer srv.Close()

	// A sender that announces 1000 bytes and sends a few.
	send := func(signature, sent string) (code int, closed bool, took time.Duration, err error) {
		conn, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			return 0, false, 0, err
		}
		defer conn.Close()
		fmt.Fprint(conn, "POST /api/github/webhook HTTP/1.1\r\nHost: panel.example.com\r\nX-GitHub-Event: push\r\nX-GitHub-Delivery: x\r\nContent-Length: 1000\r\n")
		if signature != "" {
			fmt.Fprintf(conn, "X-Hub-Signature-256: %s\r\n", signature)
		}
		fmt.Fprintf(conn, "\r\n%s", sent)
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		start := time.Now()
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return 0, false, 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, resp.Close, time.Since(start), nil
	}

	// More of them than there are turns: each gives its turn back once its
	// time is up, so none of them waits for long.
	signed := "sha256=" + strings.Repeat("ab", 32)
	var wg sync.WaitGroup
	for range webhookReaders + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, closed, took, err := send(signed, `{"ref"`)
			if err != nil || code != http.StatusRequestTimeout || !closed || took > 2*time.Second {
				t.Errorf("a body that stopped: %d after %s, closed %v, %v", code, took, closed, err)
			}
		}()
	}
	wg.Wait()

	// Nothing of this one will be read, so it is not waited for either.
	code, closed, took, err := send("", "")
	if err != nil || code != http.StatusUnauthorized || !closed || took > 2*time.Second {
		t.Errorf("a body nobody reads: %d after %s, closed %v, %v", code, took, closed, err)
	}
}
