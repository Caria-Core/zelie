package panel

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
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
	site   string // Sec-Fetch-Site, when a test needs to say where a request comes from
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
	var in io.Reader = &buf
	if raw, ok := body.([]byte); ok {
		buf.Write(raw) // sent as it is, not as JSON
	} else if rd, ok := body.(io.Reader); ok {
		in = rd // for a body that is sent slowly
	} else if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, "https://panel.example.com"+path, in)
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: proxyUID}))
	req.Header.Set("X-Forwarded-For", b.ip)
	if b.origin != "" {
		req.Header.Set("Origin", b.origin)
		req.Header.Set("Sec-Fetch-Site", "cross-site")
	}
	if b.site != "" {
		req.Header.Set("Sec-Fetch-Site", b.site)
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

// loginSolving logs in the way the page does: when the panel asks for a
// puzzle it is solved and the same request sent again. It also returns the
// size of the puzzle, zero when there was none.
func loginSolving(b *browser, email, password string) (code int, out map[string]any, bits int) {
	b.t.Helper()
	req := map[string]any{"email": email, "password": password}
	code, out = b.do("POST", "/api/login", req)
	if out["code"] != "login.pow" {
		return code, out, 0
	}
	bits = int(out["params"].(map[string]any)["bits"].(float64))
	req["pow"] = solvePow(b.t, out)
	code, out = b.do("POST", "/api/login", req)
	return code, out, bits
}

// A stranger who knows the owner's email can lock their own address out of
// the account, but not the owner, who comes from somewhere else.
func TestLoginLimits(t *testing.T) {
	s, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)

	stranger := &browser{t: t, h: h, ip: "203.0.113.5"}
	for i := range 10 {
		if code, out, _ := loginSolving(stranger, "a@example.com", "wrong password"); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d %v", i, code, out)
		}
		*now = now.Add(time.Second)
	}
	// After ten wrong passwords that address is out of tries for the
	// account, even with the right one, and stays so after a restart.
	h = s.Handler()
	stranger.h = h
	for _, password := range []string{"wrong password", "long enough pw"} {
		code, out := stranger.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": password})
		if code != http.StatusTooManyRequests || out["code"] != "login.too_many" || out["params"].(map[string]any)["minutes"] != 15.0 {
			t.Fatalf("after 10 failures, with %q: %d %v", password, code, out)
		}
	}

	// The owner gets in from another address, with a bigger puzzle since
	// the account has had ten wrong passwords.
	owner := &browser{t: t, h: h, ip: "198.51.100.7"}
	code, out, bits := loginSolving(owner, "a@example.com", "long enough pw")
	if code != http.StatusOK {
		t.Fatalf("the owner: %d %v", code, out)
	}
	if want := powBits(10); bits != want {
		t.Errorf("the owner's puzzle had %d bits, want %d", bits, want)
	}
	// Getting in clears the account's count, so the puzzle is gone, but not
	// the stranger's address, which is still out.
	owner.do("POST", "/api/logout", nil)
	if code, out, bits := loginSolving(owner, "a@example.com", "long enough pw"); code != http.StatusOK || bits != 0 {
		t.Errorf("the owner again: %d %v, a puzzle of %d bits", code, out, bits)
	}
	if code, _ := stranger.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "wrong password"}); code != http.StatusTooManyRequests {
		t.Errorf("the stranger after the owner got in: %d", code)
	}
	*now = now.Add(15 * time.Minute)
	if code, _, _ := loginSolving(stranger, "a@example.com", "wrong password"); code != http.StatusUnauthorized {
		t.Errorf("the stranger after waiting: %d", code)
	}
}

// Wrong passwords for an account from many addresses lock no one out, but
// make the puzzle bigger for everyone who logs in to it, up to a limit.
func TestLoginPuzzleGrowsWithAnAccountsFailures(t *testing.T) {
	s, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)

	for _, n := range []int{0, powAfter - 1, powAfter, powAfter + 3, powAfter + 6, powTopAt - 1, powTopAt, 3 * powTopAt} {
		// Counted as the login counts them, each from an address of its own.
		s.guards.byAccount.Reset("a@example.com")
		for range n {
			if err := s.guards.byAccount.Add("a@example.com", *now); err != nil {
				t.Fatal(err)
			}
		}
		// A visitor whose own address has not failed once.
		visitor := &browser{t: t, h: h, ip: fmt.Sprintf("192.0.2.%d", 1+n%250)}
		code, out := visitor.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"})
		params, _ := out["params"].(map[string]any)
		bits, _ := params["bits"].(float64)
		if want := powNeeded(0, n); want == 0 {
			if code != http.StatusOK {
				t.Errorf("after %d failures for the account: %d %v", n, code, out)
			}
			visitor.do("POST", "/api/logout", nil)
		} else if code != http.StatusForbidden || out["code"] != "login.pow" || int(bits) != want {
			t.Errorf("after %d failures for the account: %d %v, want a puzzle of %d bits", n, code, out, want)
		}
	}
}

func TestPowNeeded(t *testing.T) {
	for _, c := range []struct{ address, account, want int }{
		{0, 0, 0},
		{powAfter - 1, powAfter - 1, 0},
		{powAfter, 0, powMinBits},
		{0, powAfter, powMinBits},
		{powAfter + 3, 0, powMinBits + 1},
		{0, powAfter + 3, powMinBits + 1},
		{powAfter + 3, powAfter + 6, powMinBits + 2},
		{powTopAt, 0, powMaxBits},
		{0, powTopAt, powMaxBits},
		{0, 1000, powMaxBits},
	} {
		if got := powNeeded(c.address, c.account); got != c.want {
			t.Errorf("powNeeded(%d, %d) = %d, want %d", c.address, c.account, got, c.want)
		}
	}
}

// Thirty wrong passwords from an address, whichever accounts they were
// for, block the address.
func TestLoginLimitOfAnAddress(t *testing.T) {
	s, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)

	for i := range 30 {
		if err := s.guards.byIP.Add("203.0.113.5", *now); err != nil {
			t.Fatalf("failure %d: %v", i, err)
		}
	}
	guesser := &browser{t: t, h: h, ip: "203.0.113.5"}
	code, out := guesser.do("POST", "/api/login", map[string]string{"email": "other@example.com", "password": "wrong password"})
	if code != http.StatusTooManyRequests || out["code"] != "login.too_many" {
		t.Errorf("after 30 failures: %d %v", code, out)
	}
	guesser.ip = "203.0.113.6"
	if code, _ := guesser.do("POST", "/api/login", map[string]string{"email": "other@example.com", "password": "wrong password"}); code != http.StatusUnauthorized {
		t.Errorf("another address: %d", code)
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

// solvePow finds a nonce for a puzzle the way the browser does.
func solvePow(t *testing.T, out map[string]any) map[string]string {
	t.Helper()
	p, _ := out["params"].(map[string]any)
	challenge, _ := p["challenge"].(string)
	bits, _ := p["bits"].(float64)
	if challenge == "" || bits < powMinBits {
		t.Fatalf("no puzzle in %v", out)
	}
	for n := 0; ; n++ {
		nonce := strconv.Itoa(n)
		if zeroBits(sha256.Sum256([]byte(challenge+":"+nonce))) >= int(bits) {
			return map[string]string{"challenge": challenge, "nonce": nonce}
		}
	}
}

func TestLoginPuzzle(t *testing.T) {
	_, h, _ := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	b.do("POST", "/api/logout", nil)
	login := func(password string, pow any) (int, map[string]any) {
		return b.do("POST", "/api/login", map[string]any{"email": "a@example.com", "password": password, "pow": pow})
	}
	for range powAfter {
		if code, _ := login("wrong password", nil); code != http.StatusUnauthorized {
			t.Fatalf("wrong password: %d", code)
		}
	}
	// Now even the right password needs the puzzle solved first.
	code, out := login("long enough pw", nil)
	if code != http.StatusForbidden || out["code"] != "login.pow" {
		t.Fatalf("without the puzzle: %d %v", code, out)
	}
	answer := solvePow(t, out)
	if code, _ := login("long enough pw", map[string]string{"challenge": answer["challenge"], "nonce": answer["nonce"] + "1"}); code != http.StatusForbidden {
		t.Errorf("a wrong answer: %d", code)
	}
	if code, out := login("long enough pw", answer); code != http.StatusOK {
		t.Fatalf("with the puzzle: %d %v", code, out)
	}
	b.do("POST", "/api/logout", nil)
	if code, _ := login("long enough pw", answer); code != http.StatusForbidden {
		t.Errorf("the same answer twice: %d", code)
	}
	// A puzzle is for the address it was given to.
	_, out = login("long enough pw", nil)
	answer = solvePow(t, out)
	b.ip = "203.0.113.5"
	for range powAfter {
		login("wrong password", nil)
	}
	if code, _ := login("long enough pw", answer); code != http.StatusForbidden {
		t.Errorf("another address's answer: %d", code)
	}
}
