package panel

import (
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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
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
	if e.core.builds[0] != "web:cccccccccccc:private tarball" {
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
	if last := e.core.builds[len(e.core.builds)-1]; last != "web:dddddddddddd:private tarball" {
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
	if d := e.settle(t, "web"); d.State != store.DeployLive {
		t.Fatalf("deployment %+v", d)
	}
	if e.core.builds[0] != "web:aaaaaaaaaaaa:tarball" {
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
