package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/Caria-Core/zelie/internal/store"
)

// firstRead says when its body is first read.
type firstRead struct {
	io.Reader
	once sync.Once
	read chan struct{}
}

func (f *firstRead) Read(p []byte) (int, error) {
	f.once.Do(func() { close(f.read) })
	return f.Reader.Read(p)
}

// A client that sends the signed challenge slowly must not hold the
// account's second-step lock meanwhile: it would keep the owner's own
// attempts waiting until the proxy gives up on it.
func TestSlowPasskeyBodyDoesNotHoldTheSecondStepLock(t *testing.T) {
	s, h, now := newAuthServer(t)
	b := &browser{t: t, h: h, ip: "198.51.100.7"}
	b.do("POST", "/api/setup", map[string]string{"token": setupToken(t, h), "email": "a@example.com", "password": "long enough pw"})
	_, out := b.do("POST", "/api/2fa/totp/new", nil)
	if code, _ := b.do("POST", "/api/2fa/totp", map[string]string{"code": totpNow(t, out["secret"].(string), *now)}); code != http.StatusOK {
		t.Fatalf("enrol: %d", code)
	}
	// A passkey to be asked for. It is never used to sign anything.
	a, err := s.Store.AccountByEmail(context.Background(), "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := json.Marshal(webauthn.Credential{ID: []byte("passkey")})
	if err := s.Store.AddPasskey(context.Background(), store.Passkey{ID: []byte("passkey"), UserID: a.ID, Name: "key", Credential: cred, CreatedAt: *now}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name            string
		options, finish string
		wrongCode       func() (string, map[string]string)
		prepare         func(t *testing.T)
	}{
		{
			name: "login", options: "/api/login/passkey/options", finish: "/api/login/passkey",
			wrongCode: func() (string, map[string]string) { return "/api/login/totp", map[string]string{"code": "000000"} },
			prepare: func(t *testing.T) {
				b.do("POST", "/api/logout", nil)
				if code, _ := b.do("POST", "/api/login", map[string]string{"email": "a@example.com", "password": "long enough pw"}); code != http.StatusOK {
					t.Fatalf("password step: %d", code)
				}
			},
		},
		{
			name: "confirm", options: "/api/confirm/passkey/options", finish: "/api/confirm/passkey",
			wrongCode: func() (string, map[string]string) { return "/api/confirm", map[string]string{"totp": "000000"} },
			prepare: func(t *testing.T) {
				// Finish the login the other case left halfway. The next
				// code, since the last one is used up.
				*now = now.Add(30 * time.Second)
				if code, _ := b.do("POST", "/api/login/totp", map[string]string{"code": totpNow(t, out["secret"].(string), *now)}); code != http.StatusNoContent {
					t.Fatalf("second step: %d", code)
				}
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.prepare(t)
			if code, out := b.do("POST", c.options, nil); code != http.StatusOK {
				t.Fatalf("options: %d %v", code, out)
			}

			pr, pw := io.Pipe()
			defer pw.Close()
			body := &firstRead{Reader: pr, read: make(chan struct{})}
			slow := &browser{t: t, h: h, ip: b.ip, cookie: b.cookie}
			slowDone := make(chan int, 1)
			go func() { slowDone <- slow.record("POST", c.finish, body).Code }()
			select {
			case <-body.read:
			case <-time.After(5 * time.Second):
				t.Fatal("the slow request never started reading")
			}

			path, req := c.wrongCode()
			other := &browser{t: t, h: h, ip: b.ip, cookie: b.cookie}
			otherDone := make(chan int, 1)
			go func() {
				code, _ := other.do("POST", path, req)
				otherDone <- code
			}()
			select {
			case code := <-otherDone:
				if code != http.StatusUnauthorized {
					t.Errorf("a wrong code: %d", code)
				}
			case <-time.After(3 * time.Second):
				t.Error("a wrong code waited for the slow request to finish")
			}

			pw.Close()
			select {
			case <-slowDone:
			case <-time.After(5 * time.Second):
				t.Error("the slow request did not end when its body did")
			}
		})
	}
}
