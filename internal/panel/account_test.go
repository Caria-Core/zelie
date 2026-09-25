package panel

import (
	"net/http"
	"testing"
	"time"
)

// enrolled returns a browser logged in with an authenticator app, the app's
// secret and the server's clock.
func enrolled(t *testing.T) (*browser, http.Handler, string, *time.Time) {
	t.Helper()
	_, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	secret := out["secret"].(string)
	if code, _ := b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, secret, *now)}); code != http.StatusOK {
		t.Fatalf("enrol: %d", code)
	}
	return b, h, secret, now
}

func TestSensitiveChangesNeedRecentConfirmation(t *testing.T) {
	b, _, secret, now := enrolled(t)

	// Finishing the login counts as confirming.
	if code, out := b.do("POST", "/api/2fa/recovery", nil); code != http.StatusOK || len(out["recovery_codes"].([]any)) != 10 {
		t.Fatalf("right after login: %d %v", code, out)
	}
	*now = now.Add(confirmWindow + time.Minute)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/2fa/recovery"},
		{"DELETE", "/api/2fa/totp"},
		{"POST", "/api/2fa/totp/new"},
		{"POST", "/api/2fa/passkey/options"},
		{"POST", "/api/account/password"},
	} {
		if code, out := b.do(c.method, c.path, map[string]string{}); code != http.StatusForbidden || out["confirm"] != true {
			t.Errorf("%s %s without confirming: %d %v", c.method, c.path, code, out)
		}
	}
	if code, _ := b.do("POST", "/api/confirm", map[string]string{"totp": "000000"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", code)
	}
	if code, _ := b.do("POST", "/api/confirm", map[string]string{"totp": totpNow(t, secret, *now)}); code != http.StatusNoContent {
		t.Fatalf("confirm: %d", code)
	}
	if code, _ := b.do("POST", "/api/confirm", map[string]string{"totp": totpNow(t, secret, *now)}); code != http.StatusUnauthorized {
		t.Fatalf("the same code worked twice: %d", code)
	}
	if code, _ := b.do("POST", "/api/2fa/recovery", nil); code != http.StatusOK {
		t.Fatalf("after confirming: %d", code)
	}
}

func TestConfirmWithRecoveryCode(t *testing.T) {
	b, _, _, now := enrolled(t)
	_, out := b.do("POST", "/api/2fa/recovery", nil)
	codes := out["recovery_codes"].([]any)
	*now = now.Add(confirmWindow + time.Minute)
	if code, _ := b.do("POST", "/api/confirm", map[string]string{"recovery": codes[0].(string)}); code != http.StatusNoContent {
		t.Fatalf("confirm: %d", code)
	}
	if _, acct := b.do("GET", "/api/account", nil); acct["recovery_left"] != float64(9) {
		t.Fatalf("recovery code not used up: %v", acct["recovery_left"])
	}
}

func TestLastFactorStays(t *testing.T) {
	b, _, _, _ := enrolled(t)
	if code, out := b.do("DELETE", "/api/2fa/totp", nil); code != http.StatusConflict {
		t.Fatalf("removed the only second factor: %d %v", code, out)
	}
	if _, me := b.do("GET", "/api/me", nil); me["verified"] != true {
		t.Fatalf("me: %v", me)
	}
}

func TestChangePasswordEndsOtherSessions(t *testing.T) {
	b, h, secret, now := enrolled(t)
	other := &browser{t: t, h: h, ip: "203.0.113.9"}
	other.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"})
	*now = now.Add(31 * time.Second)
	if code, _ := other.do("POST", "/api/login/totp", map[string]string{"code": totpNow(t, secret, *now)}); code != http.StatusNoContent {
		t.Fatalf("second login: %d", code)
	}
	if _, acct := b.do("GET", "/api/account", nil); len(acct["sessions"].([]any)) != 2 {
		t.Fatalf("sessions: %v", acct["sessions"])
	}

	if code, _ := b.do("POST", "/api/account/password", map[string]string{"current": "wrong", "new": "a new long password"}); code != http.StatusBadRequest {
		t.Fatalf("wrong current password: %d", code)
	}
	if code, _ := b.do("POST", "/api/account/password", map[string]string{"current": "long enough pw", "new": "a new long password"}); code != http.StatusNoContent {
		t.Fatalf("change password: %d", code)
	}
	if code, _ := other.do("GET", "/api/account", nil); code != http.StatusUnauthorized {
		t.Fatalf("other browser still logged in: %d", code)
	}
	if code, _ := b.do("GET", "/api/account", nil); code != http.StatusOK {
		t.Fatalf("this browser was logged out: %d", code)
	}
	fresh := &browser{t: t, h: h, ip: "203.0.113.10"}
	if code, _ := fresh.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "a new long password"}); code != http.StatusOK {
		t.Fatalf("login with the new password: %d", code)
	}
}

func TestEndSession(t *testing.T) {
	b, h, _, _ := enrolled(t)
	other := &browser{t: t, h: h, ip: "203.0.113.9"}
	other.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"})

	_, acct := b.do("GET", "/api/account", nil)
	var otherID string
	for _, x := range acct["sessions"].([]any) {
		x := x.(map[string]any)
		if x["current"] == false {
			otherID = x["id"].(string)
			if x["verified"] != false {
				t.Errorf("half login shown as verified: %v", x)
			}
		}
	}
	if code, _ := b.do("DELETE", "/api/sessions/"+otherID, nil); code != http.StatusNoContent {
		t.Fatalf("end session: %d", code)
	}
	if code, _ := b.do("DELETE", "/api/sessions/"+otherID, nil); code != http.StatusNotFound {
		t.Fatalf("end it again: %d", code)
	}
	if _, me := other.do("GET", "/api/me", nil); me["logged_in"] != false {
		t.Fatalf("ended session still works: %v", me)
	}
}

func TestSessionsOfAnotherAccountAreOutOfReach(t *testing.T) {
	b, h, _, _ := enrolled(t)
	_, acct := b.do("GET", "/api/account", nil)
	id := acct["sessions"].([]any)[0].(map[string]any)["id"].(string)
	stranger := &browser{t: t, h: h, ip: "203.0.113.9"}
	if code, _ := stranger.do("DELETE", "/api/sessions/"+id, nil); code != http.StatusUnauthorized {
		t.Fatalf("logged out visitor ended a session: %d", code)
	}
}
