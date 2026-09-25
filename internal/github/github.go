// Package github talks to GitHub as the panel's own GitHub App.
//
// Each panel creates its own App on its owner's account through GitHub's
// manifest flow, so nobody copies tokens around and Zelie the project never
// sits between a panel and GitHub. The App only asks for what deploying
// needs: reading code and setting commit statuses.
package github

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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default addresses. Tests point a Client elsewhere.
const (
	API = "https://api.github.com"
	Web = "https://github.com"
)

// ErrNotFound means GitHub has no such thing, or the App cannot see it.
var ErrNotFound = errors.New("not found on GitHub")

// Manifest describes the App a panel creates for itself. base is the
// panel's address, such as https://panel.example.com.
func Manifest(name, base string) map[string]any {
	return map[string]any{
		"name":            name,
		"url":             base,
		"hook_attributes": map[string]any{"url": base + "/api/github/webhook", "active": true},
		"redirect_url":    base + "/github/created",
		"setup_url":       base + "/github",
		"setup_on_update": true,
		"public":          false,
		"default_permissions": map[string]string{
			"contents": "read",
			"metadata": "read",
			"statuses": "write",
		},
		"default_events": []string{"push"},
	}
}

// Credentials are what GitHub hands over once, when the App is created.
type Credentials struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	HTMLURL       string `json:"html_url"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// Convert trades the code from the manifest flow for the new App's
// credentials. The code works once and for an hour.
func Convert(ctx context.Context, hc *http.Client, api, code string) (Credentials, error) {
	var c Credentials
	if code == "" || len(code) > 100 || strings.ContainsAny(code, "/?#% ") {
		return c, errors.New("GitHub sent an invalid code")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api+"/app-manifests/"+code+"/conversions", nil)
	if err != nil {
		return c, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if err := do(hc, req, http.StatusCreated, &c); err != nil {
		return c, fmt.Errorf("finish creating the GitHub App: %w", err)
	}
	if c.ID == 0 || c.Slug == "" || c.PEM == "" || c.WebhookSecret == "" {
		return c, errors.New("GitHub's answer is missing the App's keys")
	}
	if _, err := ParseKey([]byte(c.PEM)); err != nil {
		return c, err
	}
	return c, nil
}

// ParseKey reads the App's private key as GitHub gives it.
func ParseKey(b []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("the GitHub App key is not PEM")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("read the GitHub App key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the GitHub App key is not an RSA key")
	}
	return rk, nil
}

// Verify checks a webhook's X-Hub-Signature-256 header against its body.
func Verify(secret, body []byte, header string) bool {
	sig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Client acts as one App.
type Client struct {
	HTTP  *http.Client
	API   string
	AppID int64
	Key   *rsa.PrivateKey
	Now   func() time.Time

	mu     sync.Mutex
	tokens map[int64]token // by installation
}

type token struct {
	value   string
	expires time.Time
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// jwt signs the short-lived token that identifies the App itself.
func (c *Client) jwt() (string, error) {
	now := c.now()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	// Backdated a minute for clock drift; GitHub allows ten minutes at most.
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(c.AppID, 10),
	})
	signed := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// asApp makes a request authenticated as the App.
func (c *Client) asApp(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	jwt, err := c.jwt()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	return req, nil
}

// asInstallation makes a request with an installation's access token,
// which only reaches the repositories that installation was given.
func (c *Client) asInstallation(ctx context.Context, inst int64, method, path string, body io.Reader) (*http.Request, error) {
	tok, err := c.installationToken(ctx, inst)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.API+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	return req, nil
}

func (c *Client) installationToken(ctx context.Context, inst int64) (string, error) {
	c.mu.Lock()
	t, ok := c.tokens[inst]
	c.mu.Unlock()
	// Tokens last an hour; take a new one well before that.
	if ok && c.now().Before(t.expires.Add(-10*time.Minute)) {
		return t.value, nil
	}
	req, err := c.asApp(ctx, http.MethodPost, "/app/installations/"+strconv.FormatInt(inst, 10)+"/access_tokens", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := do(c.HTTP, req, http.StatusCreated, &out); err != nil {
		return "", fmt.Errorf("get access to the repository: %w", err)
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[int64]token{}
	}
	c.tokens[inst] = token{out.Token, out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

// Installation is an account the App was installed on.
type Installation struct {
	ID      int64  `json:"id"`
	Account string `json:"account"`
	// All is true when the App may read every repository of the account.
	All bool `json:"all"`
}

func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	req, err := c.asApp(ctx, http.MethodGet, "/app/installations?per_page=100", nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
	}
	if err := do(c.HTTP, req, http.StatusOK, &raw); err != nil {
		return nil, err
	}
	out := make([]Installation, 0, len(raw))
	for _, r := range raw {
		out = append(out, Installation{ID: r.ID, Account: r.Account.Login, All: r.RepositorySelection == "all"})
	}
	return out, nil
}

// Repository is one the App can read.
type Repository struct {
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
}

// maxRepositories caps how many repositories of one installation are
// listed, so an account with thousands does not stall the page.
const maxRepositories = 1000

func (c *Client) Repositories(ctx context.Context, inst int64) ([]Repository, error) {
	var out []Repository
	for page := 1; len(out) < maxRepositories; page++ {
		req, err := c.asInstallation(ctx, inst, http.MethodGet, "/installation/repositories?per_page=100&page="+strconv.Itoa(page), nil)
		if err != nil {
			return nil, err
		}
		var res struct {
			Repositories []Repository `json:"repositories"`
		}
		if err := do(c.HTTP, req, http.StatusOK, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Repositories...)
		if len(res.Repositories) < 100 {
			break
		}
	}
	return out, nil
}

// RepoInstallation finds the installation that can read repo, or
// ErrNotFound if the App was not given access to it.
func (c *Client) RepoInstallation(ctx context.Context, repo string) (int64, error) {
	req, err := c.asApp(ctx, http.MethodGet, "/repos/"+repo+"/installation", nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		ID int64 `json:"id"`
	}
	if err := do(c.HTTP, req, http.StatusOK, &out); err != nil {
		return 0, err
	}
	return out.ID, nil
}

// Resolve returns the commit a branch points at.
func (c *Client) Resolve(ctx context.Context, inst int64, repo, branch string) (string, error) {
	req, err := c.asInstallation(ctx, inst, http.MethodGet, "/repos/"+repo+"/commits/"+url.PathEscape(branch), nil)
	if err != nil {
		return "", err
	}
	// This media type makes GitHub answer with the bare commit hash.
	req.Header.Set("Accept", "application/vnd.github.sha")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		return "", fmt.Errorf("%s has no branch %s", repo, branch)
	default:
		return "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 100))
	return strings.TrimSpace(string(b)), err
}

// Archive downloads the code at a commit as a gzipped tar. GitHub answers
// with a redirect to a short-lived signed address, which the HTTP client
// follows; it drops the Authorization header on the way, as it should.
func (c *Client) Archive(ctx context.Context, inst int64, repo, commit string) (io.ReadCloser, error) {
	req, err := c.asInstallation(ctx, inst, http.MethodGet, "/repos/"+repo+"/tarball/"+commit, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download from GitHub: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download from GitHub: %s", resp.Status)
	}
	return resp.Body, nil
}

// Commit status states.
const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailure = "failure"
)

// SetStatus puts a mark next to a commit on GitHub.
func (c *Client) SetStatus(ctx context.Context, inst int64, repo, commit, state, description, target string) error {
	body, _ := json.Marshal(map[string]string{
		"state":       state,
		"description": truncate(description, 140),
		"target_url":  target,
		"context":     "Zelie",
	})
	req, err := c.asInstallation(ctx, inst, http.MethodPost, "/repos/"+repo+"/statuses/"+commit, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(c.HTTP, req, http.StatusCreated, nil)
}

// Delivery is GitHub's record of sending one webhook.
type Delivery struct {
	Event       string    `json:"event"`
	StatusCode  int       `json:"status_code"`
	Status      string    `json:"status"`
	DeliveredAt time.Time `json:"delivered_at"`
}

// Deliveries lists the App's most recent webhook deliveries, newest first.
// It shows whether GitHub can reach the panel at all.
func (c *Client) Deliveries(ctx context.Context) ([]Delivery, error) {
	req, err := c.asApp(ctx, http.MethodGet, "/app/hook/deliveries?per_page=10", nil)
	if err != nil {
		return nil, err
	}
	var out []Delivery
	return out, do(c.HTTP, req, http.StatusOK, &out)
}

// do sends req and decodes a JSON answer into out, if out is not nil.
func do(hc *http.Client, req *http.Request, want int, out any) error {
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("reach GitHub: %w", err)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, 16<<20)
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode != want {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(body).Decode(&e)
		if e.Message != "" {
			return fmt.Errorf("GitHub answered %s: %s", resp.Status, truncate(e.Message, 200))
		}
		return fmt.Errorf("GitHub answered %s", resp.Status)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(body).Decode(out); err != nil {
		return fmt.Errorf("read GitHub's answer: %w", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8Start(s[n]) {
		n--
	}
	return s[:n]
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
