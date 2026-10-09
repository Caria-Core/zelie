package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/github"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

const (
	sealGitHubKey     = "github app key"
	sealGitHubWebhook = "github webhook secret"
)

// Nobody has proved who they are when a webhook arrives, and its signature
// can only be checked once the whole body is here. So a body is limited in
// size, in time and in how many are read at once. GitHub allows 25 MB, but a
// push is a few kilobytes, and a huge one a few megabytes.
const (
	maxWebhook     = 5 << 20
	webhookReaders = 2
)

// How long a webhook waits for its turn to be read, and how long it then
// has to arrive. A slow sender frees its turn before the next one gives up.
// Tests shorten both.
var (
	webhookWait     = 10 * time.Second
	webhookReadTime = 10 * time.Second
)

// webhookGate lets a few webhook bodies be read at a time.
type webhookGate struct {
	once  sync.Once
	slots chan struct{}
}

// enter waits for a turn and returns the function that gives it back, or
// false if the turn did not come in time.
func (g *webhookGate) enter(ctx context.Context) (leave func(), ok bool) {
	g.once.Do(func() { g.slots = make(chan struct{}, webhookReaders) })
	ctx, cancel := context.WithTimeout(ctx, webhookWait)
	defer cancel()
	select {
	case g.slots <- struct{}{}:
		return func() { <-g.slots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// hasSignature reports whether a header has the shape of GitHub's signature:
// sha256= and a SHA-256 in hex.
func hasSignature(header string) bool {
	sum, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	b, err := hex.DecodeString(sum)
	return err == nil && len(b) == sha256.Size
}

var validAccount = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`) })

// ghConn is a loaded GitHub App: a client and the secret its webhooks are
// signed with.
type ghConn struct {
	app    store.GitHubApp
	client *github.Client
	secret []byte
}

// ghCache holds the loaded App, so every webhook does not decrypt the key
// again.
type ghCache struct {
	mu     sync.Mutex
	conn   *ghConn
	loaded bool
}

func (s *Server) githubAPI() string {
	if s.GitHubAPI != "" {
		return s.GitHubAPI
	}
	return github.API
}

func (s *Server) githubWeb() string {
	if s.GitHubWeb != "" {
		return s.GitHubWeb
	}
	return github.Web
}

func (s *Server) githubHTTP() *http.Client {
	if s.GitHubHTTP != nil {
		return s.GitHubHTTP
	}
	return http.DefaultClient
}

// github returns the panel's GitHub App, or nil if none is connected.
func (s *Server) github(ctx context.Context) (*ghConn, error) {
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	if s.gh.loaded {
		return s.gh.conn, nil
	}
	app, err := s.Store.GitHubApp(ctx)
	if errors.Is(err, store.ErrNotFound) {
		s.gh.loaded = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	pem, err := s.Sealer.Open(app.Key, sealGitHubKey)
	if err != nil {
		return nil, fmt.Errorf("open the GitHub App key: %w", err)
	}
	key, err := github.ParseKey(pem)
	if err != nil {
		return nil, err
	}
	secret, err := s.Sealer.Open(app.WebhookSecret, sealGitHubWebhook)
	if err != nil {
		return nil, fmt.Errorf("open the webhook secret: %w", err)
	}
	s.gh.conn = &ghConn{
		app:    app,
		client: &github.Client{HTTP: s.githubHTTP(), API: s.githubAPI(), AppID: app.AppID, Key: key, Now: s.Now},
		secret: secret,
	}
	s.gh.loaded = true
	return s.gh.conn, nil
}

func (s *Server) forgetGitHub() {
	s.gh.mu.Lock()
	defer s.gh.mu.Unlock()
	s.gh.conn, s.gh.loaded = nil, false
}

var (
	errBadOrg        = msg.Define(http.StatusBadRequest, "github.bad_org", "That is not a GitHub organization name.")
	errGitHubOn      = msg.Define(http.StatusConflict, "github.connected", "GitHub is already connected.")
	errGitHubOff     = msg.Define(http.StatusConflict, "github.not_connected", "GitHub is not connected.")
	errManifestState = msg.Define(http.StatusBadRequest, "github.link_expired", "This link has expired or was not started here. Connect GitHub again.")
	errGitHubAnswer  = msg.Define(http.StatusBadGateway, "github.failed", "GitHub could not be reached or refused: {detail}")
	errOpenPanelAt   = msg.Define(http.StatusBadRequest, "github.open_panel_at", "Open the panel at {panel} to connect GitHub.")
	errNoOwnAddress  = msg.Define(http.StatusBadRequest, "github.no_address", "The panel does not know its own address.")
	// Webhooks are answered to GitHub, not to a person.
	errWebhook = msg.Define(http.StatusBadRequest, "github.webhook_refused", "Webhook refused: {detail}")
)

// panelBase is the address the browser reached the panel at, such as
// https://panel.example.com. GitHub sends people and webhooks back there.
func (s *Server) panelBase(r *http.Request) (string, error) {
	host := r.Host
	name := requestHost(r)
	if s.Proxy != nil {
		cfg, err := s.Proxy.Config(r.Context())
		if err != nil {
			return "", err
		}
		if cfg.Panel != "" && !strings.EqualFold(name, cfg.Panel) {
			return "", errOpenPanelAt.Err("panel", cfg.Panel)
		}
	}
	if name == "" {
		return "", errNoOwnAddress.Err()
	}
	return "https://" + strings.ToLower(host), nil
}

type githubJSON struct {
	Connected     bool                  `json:"connected"`
	Slug          string                `json:"slug,omitempty"`
	Owner         string                `json:"owner,omitempty"`
	HTMLURL       string                `json:"html_url,omitempty"`
	InstallURL    string                `json:"install_url,omitempty"`
	Installations []github.Installation `json:"installations,omitempty"`
	// Webhook says whether GitHub has managed to reach the panel.
	Webhook *webhookJSON `json:"webhook,omitempty"`
	// Private is true when the App was made for a panel address GitHub
	// cannot reach, so it has no webhook.
	Private bool `json:"private,omitempty"`
	// Error is set when GitHub could not be asked about the App.
	Error *msg.Msg `json:"error,omitempty"`
}

type webhookJSON struct {
	// State is ok, failing, or none when GitHub has sent nothing yet.
	State string    `json:"state"`
	Last  time.Time `json:"last,omitzero"`
	// Status is GitHub's description of the last failure.
	Status string `json:"status,omitempty"`
}

func (s *Server) githubStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conn, err := s.github(ctx)
	if err != nil {
		s.fail(w, "load GitHub App", err)
		return
	}
	if conn == nil {
		writeJSON(w, http.StatusOK, githubJSON{})
		return
	}
	out := githubJSON{
		Connected: true, Slug: conn.app.Slug, Owner: conn.app.Owner, HTMLURL: conn.app.HTMLURL,
		InstallURL: s.githubWeb() + "/apps/" + url.PathEscape(conn.app.Slug) + "/installations/new",
	}
	if u, err := url.Parse(conn.app.BaseURL); err == nil && !github.PublicHost(u.Hostname()) {
		out.Private = true
	}
	out.Installations, err = conn.client.Installations(ctx)
	if err != nil {
		out.Error = new(errGitHubAnswer.With("detail", err.Error()))
		writeJSON(w, http.StatusOK, out)
		return
	}
	// An App without a webhook has no deliveries to list; GitHub answers
	// 404 for those.
	if !out.Private {
		deliveries, err := conn.client.Deliveries(ctx)
		switch {
		case errors.Is(err, github.ErrNotFound):
			out.Webhook = &webhookJSON{State: "none"}
		case err != nil:
			out.Error = new(errGitHubAnswer.With("detail", err.Error()))
		default:
			out.Webhook = webhookState(deliveries)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// webhookState reads GitHub's delivery log. Only the latest delivery
// counts: once the panel is reachable, old failures no longer matter.
func webhookState(list []github.Delivery) *webhookJSON {
	if len(list) == 0 {
		return &webhookJSON{State: "none"}
	}
	d := list[0]
	if d.StatusCode >= 200 && d.StatusCode < 300 {
		return &webhookJSON{State: "ok", Last: d.DeliveredAt}
	}
	status := d.Status
	if d.StatusCode != 0 {
		status = strconv.Itoa(d.StatusCode) + " " + status
	}
	return &webhookJSON{State: "failing", Last: d.DeliveredAt, Status: truncate(status, 200)}
}

// githubManifest starts creating the panel's GitHub App. The interface
// posts the manifest to GitHub, which shows the owner what the App may do
// and sends them back with a code.
func (s *Server) githubManifest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Org string `json:"org"`
	}
	if !decode(w, r, &req) {
		return
	}
	req.Org = strings.TrimSpace(req.Org)
	if req.Org != "" && !validAccount().MatchString(req.Org) {
		writeError(w, errBadOrg.Err())
		return
	}
	ctx := r.Context()
	if conn, err := s.github(ctx); err != nil {
		s.fail(w, "load GitHub App", err)
		return
	} else if conn != nil {
		writeError(w, errGitHubOn.Err())
		return
	}
	base, err := s.panelBase(r)
	if err != nil {
		s.failWith(w, "panel address", err)
		return
	}
	b := make([]byte, 24)
	rand.Read(b)
	state := base64.RawURLEncoding.EncodeToString(b)
	s.guards.pending.put(loginFrom(ctx).session.Hash, "github-manifest", state, s.now())

	host, _ := url.Parse(base)
	manifest, _ := json.Marshal(github.Manifest(appName(host.Hostname()), base))
	action := s.githubWeb() + "/settings/apps/new"
	if req.Org != "" {
		action = s.githubWeb() + "/organizations/" + req.Org + "/settings/apps/new"
	}
	writeJSON(w, http.StatusOK, map[string]string{"action": action + "?state=" + state, "manifest": string(manifest)})
}

// appName suggests a name for the App. GitHub App names are unique across
// GitHub and at most 34 characters; the owner can change it on GitHub's
// page.
func appName(host string) string {
	return truncate("Zelie "+host, 34)
}

// githubCreated finishes creating the App with the code GitHub sent back.
func (s *Server) githubCreated(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if !decode(w, r, &req) {
		return
	}
	ctx := r.Context()
	l := loginFrom(ctx)
	v, ok := s.guards.pending.take(l.session.Hash, "github-manifest", s.now())
	if !ok || subtle.ConstantTimeCompare([]byte(v.(string)), []byte(req.State)) != 1 {
		writeError(w, errManifestState.Err())
		return
	}
	base, err := s.panelBase(r)
	if err != nil {
		s.failWith(w, "panel address", err)
		return
	}
	creds, err := github.Convert(ctx, s.githubHTTP(), s.githubAPI(), req.Code)
	if err != nil {
		writeError(w, errGitHubAnswer.Err("detail", err.Error()))
		return
	}
	err = s.Store.SetGitHubApp(ctx, store.GitHubApp{
		AppID: creds.ID, Slug: creds.Slug, Owner: creds.Owner.Login, HTMLURL: creds.HTMLURL, BaseURL: base,
		Key:           s.Sealer.Seal([]byte(creds.PEM), sealGitHubKey),
		WebhookSecret: s.Sealer.Seal([]byte(creds.WebhookSecret), sealGitHubWebhook),
		CreatedAt:     s.now(),
	})
	if err != nil {
		s.fail(w, "save GitHub App", err)
		return
	}
	s.forgetGitHub()
	s.Log.Info("GitHub connected", "app", creds.Slug, "owner", creds.Owner.Login, "user", l.account.ID)
	writeJSON(w, http.StatusCreated, map[string]string{
		"slug":        creds.Slug,
		"install_url": s.githubWeb() + "/apps/" + url.PathEscape(creds.Slug) + "/installations/new",
	})
}

// githubDisconnect forgets the App. The App itself stays on GitHub until
// its owner deletes it there; the panel cannot.
func (s *Server) githubDisconnect(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.DeleteGitHubApp(r.Context()); err != nil {
		s.fail(w, "delete GitHub App", err)
		return
	}
	s.forgetGitHub()
	s.Log.Info("GitHub disconnected", "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// githubRepos lists every repository the App was given, for picking one.
func (s *Server) githubRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conn, err := s.github(ctx)
	if err != nil {
		s.fail(w, "load GitHub App", err)
		return
	}
	if conn == nil {
		writeError(w, errGitHubOff.Err())
		return
	}
	insts, err := conn.client.Installations(ctx)
	if err != nil {
		writeError(w, errGitHubAnswer.Err("detail", err.Error()))
		return
	}
	out := []github.Repository{}
	for _, in := range insts {
		repos, err := conn.client.Repositories(ctx, in.ID)
		if err != nil {
			writeError(w, errGitHubAnswer.Err("detail", err.Error()))
			return
		}
		out = append(out, repos...)
	}
	slices.SortFunc(out, func(a, b github.Repository) int {
		return strings.Compare(strings.ToLower(a.FullName), strings.ToLower(b.FullName))
	})
	writeJSON(w, http.StatusOK, out)
}

// turnAway answers a webhook whose body was not read, or not all of it, and
// ends the connection. Otherwise the server first waits for the rest of a
// body that may never come, and the answer waits with it.
func turnAway(w http.ResponseWriter, e *msg.Error) {
	w.Header().Set("Connection", "close")
	writeError(w, e)
}

// githubWebhook receives events from GitHub. It needs no login: the
// signature, made with the secret only GitHub and the panel know, is the
// proof.
func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conn, err := s.github(ctx)
	if err != nil {
		s.fail(w, "load GitHub App", err)
		return
	}
	if conn == nil {
		writeError(w, errGitHubOff.Err().WithStatus(http.StatusNotFound))
		return
	}
	signature := r.Header.Get("X-Hub-Signature-256")
	if !hasSignature(signature) {
		s.Log.Warn("webhook without a signature", "ip", clientIP(r))
		turnAway(w, errWebhook.Err("detail", "bad signature").WithStatus(http.StatusUnauthorized))
		return
	}
	if r.ContentLength > maxWebhook {
		s.Log.Warn("webhook payload too large", "ip", clientIP(r), "bytes", r.ContentLength)
		turnAway(w, errWebhook.Err("detail", "the payload is too large").WithStatus(http.StatusRequestEntityTooLarge))
		return
	}
	leave, ok := s.webhooks.enter(ctx)
	if !ok {
		turnAway(w, errWebhook.Err("detail", "too many payloads at once, try again").WithStatus(http.StatusServiceUnavailable))
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Now().Add(webhookReadTime)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		s.Log.Warn("webhook read deadline", "err", err)
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhook))
	rc.SetReadDeadline(time.Time{})
	leave()
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		s.Log.Warn("webhook payload too large", "ip", clientIP(r))
		turnAway(w, errWebhook.Err("detail", "the payload is too large").WithStatus(http.StatusRequestEntityTooLarge))
		return
	case err != nil:
		turnAway(w, errWebhook.Err("detail", "the payload did not arrive").WithStatus(http.StatusRequestTimeout))
		return
	}
	if !github.Verify(conn.secret, body, signature) {
		s.Log.Warn("webhook with a bad signature", "ip", clientIP(r))
		writeError(w, errWebhook.Err("detail", "bad signature").WithStatus(http.StatusUnauthorized))
		return
	}
	delivery := r.Header.Get("X-GitHub-Delivery")
	if delivery == "" || len(delivery) > 100 {
		writeError(w, errWebhook.Err("detail", "missing delivery id"))
		return
	}
	first, err := s.Store.FirstDelivery(ctx, delivery, s.now())
	if err != nil {
		s.fail(w, "record delivery", err)
		return
	}
	if !first {
		writeJSON(w, http.StatusOK, map[string]string{"result": "already handled"})
		return
	}
	switch r.Header.Get("X-GitHub-Event") {
	case "push":
		s.githubPush(w, r, body)
	case "ping":
		writeJSON(w, http.StatusOK, map[string]string{"result": "pong"})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored"})
	}
}

func (s *Server) githubPush(w http.ResponseWriter, r *http.Request, body []byte) {
	var p struct {
		Ref        string `json:"ref"`
		After      string `json:"after"`
		Deleted    bool   `json:"deleted"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		HeadCommit *struct {
			Message string `json:"message"`
		} `json:"head_commit"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		writeError(w, errWebhook.Err("detail", "invalid push payload"))
		return
	}
	branch, ok := strings.CutPrefix(p.Ref, "refs/heads/")
	// Tags and deleted branches have nothing to deploy.
	if !ok || p.Deleted || !validCommit().MatchString(p.After) || strings.Trim(p.After, "0") == "" {
		writeJSON(w, http.StatusAccepted, map[string]string{"result": "ignored"})
		return
	}
	ctx := r.Context()
	apps, err := s.Store.PushTargets(ctx, p.Repository.FullName, branch)
	if err != nil {
		s.fail(w, "find apps", err)
		return
	}
	message := ""
	if p.HeadCommit != nil {
		message, _, _ = strings.Cut(p.HeadCommit.Message, "\n")
		message = truncate(strings.TrimSpace(message), 200)
	}
	var ids []string
	for _, a := range apps {
		// A push does not start an app the user stopped.
		if a.Stopped {
			continue
		}
		if _, err := s.deploy(ctx, a, store.Deployment{Version: p.After, Cause: store.CausePush, Message: message}); err != nil {
			s.fail(w, "deploy", err)
			return
		}
		ids = append(ids, a.ID)
	}
	s.Log.Info("push", "repo", p.Repository.FullName, "branch", branch, "commit", p.After[:12], "apps", ids)
	writeJSON(w, http.StatusAccepted, map[string]any{"result": "deploying", "apps": len(ids)})
}

// appSource reads a repository through the GitHub App, which also reaches
// private repositories and can mark commits.
type appSource struct {
	c    *github.Client
	inst int64
}

func (a appSource) Resolve(ctx context.Context, repo, branch string) (string, error) {
	if err := checkRepo(repo, branch); err != nil {
		return "", err
	}
	commit, err := a.c.Resolve(ctx, a.inst, repo, branch)
	if err != nil {
		return "", err
	}
	if !validCommit().MatchString(commit) {
		return "", errors.New("GitHub gave an unexpected answer")
	}
	return commit, nil
}

func (a appSource) Archive(ctx context.Context, repo, commit string) (io.ReadCloser, error) {
	if !validRepo().MatchString(repo) || !validCommit().MatchString(commit) {
		return nil, errors.New("invalid repository or commit")
	}
	return a.c.Archive(ctx, a.inst, repo, commit)
}

// sourceFor picks how to read repo: through the App when it was given the
// repository, as a public repository otherwise. It also returns where the
// App can report back, or nil.
func (s *Server) sourceFor(ctx context.Context, repo string, out io.Writer) (Source, *commitStatus) {
	conn, err := s.github(ctx)
	if err != nil {
		s.Log.Error("load GitHub App", "err", err)
	}
	if conn == nil {
		return s.Source, nil
	}
	inst, err := conn.client.RepoInstallation(ctx, repo)
	switch {
	case errors.Is(err, github.ErrNotFound):
		fmt.Fprintf(out, "Zelie's GitHub App was not given %s, reading it as a public repository.\n", repo)
		return s.Source, nil
	case err != nil:
		fmt.Fprintf(out, "Could not ask GitHub about %s (%v), reading it as a public repository.\n", repo, err)
		return s.Source, nil
	}
	src := appSource{c: conn.client, inst: inst}
	return src, &commitStatus{src: src, repo: repo, base: conn.app.BaseURL, log: s.Log.Warn}
}

// commitStatus marks a commit on GitHub with how its deployment went. A
// failure to do so is logged and does not stop the deployment.
type commitStatus struct {
	src    appSource
	repo   string
	base   string
	commit string
	app    string
	log    func(msg string, args ...any)
}

func (c *commitStatus) set(ctx context.Context, state, description string) {
	if c == nil || c.commit == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	target := c.base + "/a/" + c.app
	if err := c.src.c.SetStatus(ctx, c.src.inst, c.repo, c.commit, state, description, target); err != nil {
		c.log("set commit status", "repo", c.repo, "commit", c.commit[:12], "err", err)
	}
}
