package github

import (
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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestJWT(t *testing.T) {
	key := testKey(t)
	now := time.Unix(1_800_000_000, 0)
	c := &Client{AppID: 42, Key: key, Now: func() time.Time { return now }}
	jwt, err := c.jwt()
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt %q", jwt)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat, Exp int64
		Iss      string
	}
	json.Unmarshal(raw, &claims)
	if claims.Iss != "42" || claims.Iat != now.Unix()-60 || claims.Exp-claims.Iat > 600 {
		t.Errorf("claims %+v", claims)
	}
}

func TestVerify(t *testing.T) {
	secret, body := []byte("s3cret"), []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !Verify(secret, body, good) {
		t.Error("good signature rejected")
	}
	for _, bad := range []string{"", strings.TrimPrefix(good, "sha256="), "sha1=" + good[7:], "sha256=zz", good[:len(good)-2] + "00"} {
		if Verify(secret, body, bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if Verify([]byte("other"), body, good) {
		t.Error("accepted another secret")
	}
}

func TestConvert(t *testing.T) {
	key := testKey(t)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/app-manifests/abc123/conversions" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 7, "slug": "zelie-x", "pem": pemKey, "webhook_secret": "w", "owner": map[string]string{"login": "efe"}})
	}))
	defer srv.Close()
	c, err := Convert(context.Background(), srv.Client(), srv.URL, "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != 7 || c.Slug != "zelie-x" || c.Owner.Login != "efe" {
		t.Errorf("credentials %+v", c)
	}
	if _, err := Convert(context.Background(), srv.Client(), srv.URL, "../x"); err == nil {
		t.Error("accepted a code with a path in it")
	}
	if _, err := Convert(context.Background(), srv.Client(), srv.URL, "used"); err == nil {
		t.Error("accepted an unknown code")
	}
}

func TestInstallationTokenIsReused(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var issued atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/5/access_tokens":
			n := issued.Add(1)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"token": "tok" + string(rune('0'+n)), "expires_at": now.Add(time.Hour)})
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/commits/"):
			if r.Header.Get("Authorization") != "Bearer tok"+string(rune('0'+issued.Load())) {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(strings.Repeat("c", 40)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), API: srv.URL, AppID: 1, Key: testKey(t), Now: func() time.Time { return now }}
	for range 3 {
		if _, err := c.Resolve(context.Background(), 5, "o/r", "main"); err != nil {
			t.Fatal(err)
		}
	}
	if issued.Load() != 1 {
		t.Errorf("%d tokens for three requests", issued.Load())
	}
	now = now.Add(55 * time.Minute)
	c.Resolve(context.Background(), 5, "o/r", "main")
	if issued.Load() != 2 {
		t.Error("a token about to expire was reused")
	}
}
