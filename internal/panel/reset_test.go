package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
)

// resetLink asks for a reset link the way zelie reset-login does, as root
// on the panel's socket.
func resetLink(t *testing.T, h http.Handler, email string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email})
	req := httptest.NewRequest("POST", "/local/reset-link", strings.NewReader(string(b)))
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: 0}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

func TestResetLogin(t *testing.T) {
	_, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, out["secret"].(string), *now)})
	b.do("POST", "/api/logout", nil)
	// Locked out: the password is forgotten and guessed too often.
	for range 10 {
		b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "forgotten one"})
		b.ip += "1"
	}

	if code, _ := resetLink(t, h, "b@example.com"); code != http.StatusNotFound {
		t.Errorf("reset for nobody: %d", code)
	}
	code, out := resetLink(t, h, "")
	if code != http.StatusOK || out["email"] != "a@example.com" {
		t.Fatalf("reset link: %d %v", code, out)
	}
	token := out["token"].(string)

	if code, _ := b.do("POST", "/api/reset", map[string]string{"token": token + "x", "password": "a new password"}); code != http.StatusForbidden {
		t.Errorf("wrong token: %d", code)
	}
	if code, _ := b.do("POST", "/api/reset", map[string]string{"token": token, "password": "short"}); code != http.StatusBadRequest {
		t.Errorf("short password: %d", code)
	}
	code, out = b.do("POST", "/api/reset", map[string]string{"token": token, "password": "a new password"})
	if code != http.StatusOK || out["enroll"] != true {
		t.Fatalf("reset: %d %v", code, out)
	}
	// The session must set up a second step before anything else.
	if _, me := b.do("GET", "/api/me", nil); me["enroll"] != true || me["verified"] == true {
		t.Errorf("after the reset %v", me)
	}
	if code, _ := b.do("GET", "/api/apps", nil); code == http.StatusOK {
		t.Error("reached the apps without a second step")
	}
	b.do("POST", "/api/logout", nil)

	if code, _ := b.do("POST", "/api/reset", map[string]string{"token": token, "password": "another password"}); code != http.StatusForbidden {
		t.Errorf("the link twice: %d", code)
	}
	// The limits are lifted and the new password works.
	if code, out := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "a new password"}); code != http.StatusOK || out["enroll"] != true {
		t.Errorf("log in after the reset: %d %v", code, out)
	}
}
