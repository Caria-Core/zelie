package panel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/peer"
)

// browser is a minimal cookie-keeping client that talks to the handler as
// the proxy would.
type browser struct {
	t      *testing.T
	h      http.Handler
	cookie *http.Cookie
	ip     string
	origin string
}

func (b *browser) do(method, path string, body any) (int, map[string]any) {
	b.t.Helper()
	rec := b.record(method, path, body)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// record does a request and returns the response as it came.
func (b *browser) record(method, path string, body any) *httptest.ResponseRecorder {
	b.t.Helper()
	var buf bytes.Buffer
	if raw, ok := body.([]byte); ok {
		buf.Write(raw) // sent as it is, not as JSON
	} else if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, "https://panel.example.com"+path, &buf)
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: proxyUID}))
	req.Header.Set("X-Forwarded-For", b.ip)
	if b.origin != "" {
		req.Header.Set("Origin", b.origin)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			if c.MaxAge < 0 {
				b.cookie = nil
			} else {
				b.cookie = c
			}
		}
	}
	return rec
}

func newAuthServer(t *testing.T) (*Server, http.Handler, *time.Time) {
	t.Helper()
	s := newServer(t)
	sealer, err := LoadSealer(filepath.Join(t.TempDir(), "panel.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Sealer = sealer
	now := time.Unix(1_800_000_000, 0)
	s.Now = func() time.Time { return now }
	return s, s.Handler(), &now
}

func setupToken(t *testing.T, h http.Handler) string {
	t.Helper()
	var out struct{ Token string }
	json.NewDecoder(request(h, 0, "POST", "/local/setup-link").Body).Decode(&out)
	return out.Token
}

// totpNow computes the code an authenticator app would show, following RFC
// 6238 independently of the code under test.
func totpNow(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha1.New, raw)
	binary.Write(mac, binary.BigEndian, uint64(now.Unix()/30))
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[off:])&0x7fffffff)%1_000_000)
}

func TestSetupAndLogin(t *testing.T) {
	_, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	token := setupToken(t, h)

	if code, _ := b.do("POST", "/api/setup", map[string]string{"token": "wrong", "email": "a@example.com", "password": "long enough pw"}); code != http.StatusForbidden {
		t.Fatalf("setup with a wrong token: %d", code)
	}
	if code, _ := b.do("POST", "/api/setup", map[string]string{"token": token, "email": "a@example.com", "password": "short"}); code != http.StatusBadRequest {
		t.Fatalf("setup with a short password: %d", code)
	}
	if code, out := b.do("POST", "/api/setup", map[string]string{"token": token, "email": "a@example.com", "password": "long enough pw"}); code != http.StatusOK || out["enroll"] != true {
		t.Fatalf("setup: %d %v", code, out)
	}
	if code, _ := b.do("POST", "/api/setup", map[string]string{"token": token, "email": "b@example.com", "password": "long enough pw"}); code != http.StatusConflict {
		t.Fatalf("second setup: %d", code)
	}

	// Enrolling: the only thing the new admin can do is add a second factor.
	if _, me := b.do("GET", "/api/me", nil); me["verified"] != false || me["enroll"] != true {
		t.Fatalf("me before 2FA: %v", me)
	}
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	secret := out["secret"].(string)
	if code, _ := b.do("POST", "/api/2fa/totp", map[string]string{"code": "000000"}); code != http.StatusBadRequest {
		t.Fatalf("wrong confirmation code: %d", code)
	}
	code, out := b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, secret, *now)})
	if code != http.StatusOK || len(out["recovery_codes"].([]any)) != 10 {
		t.Fatalf("confirm totp: %d %v", code, out)
	}
	recovery := out["recovery_codes"].([]any)[0].(string)
	if _, me := b.do("GET", "/api/me", nil); me["verified"] != true {
		t.Fatalf("me after 2FA: %v", me)
	}

	// Log out and back in.
	b.do("POST", "/api/logout", nil)
	if _, me := b.do("GET", "/api/me", nil); me["logged_in"] != false {
		t.Fatalf("me after logout: %v", me)
	}
	if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "wrong password"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong password: %d", code)
	}
	code, me := b.do("POST", "/api/login", map[string]string{"email": "A@example.com", "password": "long enough pw"})
	if code != http.StatusOK || me["verified"] != false || me["enroll"] != false {
		t.Fatalf("login: %d %v", code, me)
	}
	// Half logged in with a factor on file: enrolling another is not allowed.
	if code, _ := b.do("POST", "/api/2fa/totp/new", nil); code != http.StatusUnauthorized {
		t.Fatalf("add a factor before the second step: %d", code)
	}
	// The code used to confirm the app cannot be used again.
	if code, _ := b.do("POST", "/api/login/totp", map[string]string{"code": totpNow(t, secret, *now)}); code != http.StatusUnauthorized {
		t.Fatalf("replayed code: %d", code)
	}
	*now = now.Add(30 * time.Second)
	if code, _ := b.do("POST", "/api/login/totp", map[string]string{"code": totpNow(t, secret, *now)}); code != http.StatusNoContent {
		t.Fatalf("fresh code: %d", code)
	}
	if _, me := b.do("GET", "/api/me", nil); me["verified"] != true {
		t.Fatalf("me after login: %v", me)
	}

	// A recovery code works once.
	b.do("POST", "/api/logout", nil)
	b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"})
	if code, _ := b.do("POST", "/api/login/recovery", map[string]string{"code": strings.ToUpper(recovery)}); code != http.StatusNoContent {
		t.Fatalf("recovery code: %d", code)
	}
	b.do("POST", "/api/logout", nil)
	b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"})
	if code, _ := b.do("POST", "/api/login/recovery", map[string]string{"code": recovery}); code != http.StatusUnauthorized {
		t.Fatalf("used recovery code: %d", code)
	}
}

func TestLoginLimits(t *testing.T) {
	_, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)

	for range 10 {
		b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "wrong password"})
	}
	// Blocked even with the right password, from any address.
	b.ip = "203.0.113.5"
	if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"}); code != http.StatusTooManyRequests {
		t.Fatalf("after 10 failures: %d", code)
	}
	*now = now.Add(15 * time.Minute)
	if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"}); code != http.StatusOK {
		t.Fatalf("after waiting: %d", code)
	}
}

func TestCrossSiteRequestsRefused(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7", origin: "https://evil.example.net"}
	if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "x"}); code != http.StatusForbidden {
		t.Fatalf("cross-site login: %d", code)
	}
}

func TestSessionCookie(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	c := b.cookie
	if c == nil || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" {
		t.Fatalf("cookie %+v", c)
	}
}
