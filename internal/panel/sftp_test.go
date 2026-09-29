package panel

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/sftpd"
	"github.com/Caria-Core/zelie/internal/store"
)

const sftpUID = 991

// sftpAsk sends a request to the panel as the SFTP service.
func sftpAsk(e *appEnv, uid uint32, method, path string, body any) (int, map[string]any) {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: uid}))
	rec := httptest.NewRecorder()
	e.b.h.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func newSFTPEnv(t *testing.T) *appEnv {
	t.Helper()
	e := newFilesEnv(t)
	e.s.SFTPUID = func() uint32 { return sftpUID }
	return e
}

func (e *appEnv) login(t *testing.T, req sftpd.AuthRequest) (int, map[string]any) {
	t.Helper()
	if req.Server == "" {
		req.Server = "survival"
	}
	req.IP = "198.51.100.9"
	return sftpAsk(e, sftpUID, "POST", "/local/sftp/auth", req)
}

func authorizedKey(t *testing.T, pub any, comment string) (string, ssh.PublicKey) {
	t.Helper()
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment, k
}

func ed25519Key(t *testing.T, comment string) (string, ssh.PublicKey) {
	t.Helper()
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return authorizedKey(t, pub, comment)
}

func TestSSHKeysOfAnAccount(t *testing.T) {
	e := newSFTPEnv(t)
	line, pub := ed25519Key(t, "laptop")

	code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": "My laptop", "key": line})
	if code != http.StatusCreated || out["name"] != "My laptop" || out["fingerprint"] != ssh.FingerprintSHA256(pub) || out["type"] != "ssh-ed25519" {
		t.Fatalf("add: %d %v", code, out)
	}
	id := int64(out["id"].(float64))

	// Without a name, the key's own comment is used.
	line2, _ := ed25519Key(t, "from-comment")
	if code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"key": line2}); code != http.StatusCreated || out["name"] != "from-comment" {
		t.Errorf("named by comment: %d %v", code, out)
	}

	if code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": "again", "key": line}); code != http.StatusConflict || out["code"] != "sshkey.exists" {
		t.Errorf("the same key twice: %d %v", code, out)
	}

	rec := e.b.record("GET", "/api/account/ssh-keys", nil)
	var list []sshKeyJSON
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list) != 2 || list[0].ID != id || list[0].Fingerprint != ssh.FingerprintSHA256(pub) {
		t.Errorf("list: %s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "AAAA") {
		t.Errorf("the list holds the key itself: %s", rec.Body)
	}

	rsa1024, _ := rsa.GenerateKey(rand.Reader, 1024)
	rsa2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	p256, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	weak, _ := authorizedKey(t, &rsa1024.PublicKey, "weak")
	strong, _ := authorizedKey(t, &rsa2048.PublicKey, "strong")
	curve, _ := authorizedKey(t, &p256.PublicKey, "curve")
	for name, tc := range map[string]struct {
		key  string
		code int
		want string
	}{
		"a weak rsa key":     {weak, http.StatusBadRequest, "sshkey.weak"},
		"a 2048-bit rsa key": {strong, http.StatusCreated, ""},
		"an ecdsa key":       {curve, http.StatusCreated, ""},
		"nothing":            {"", http.StatusBadRequest, "sshkey.invalid"},
		"words":              {"ssh-ed25519 not-base64", http.StatusBadRequest, "sshkey.invalid"},
		"a private key":      {"-----BEGIN OPENSSH PRIVATE KEY-----\nAAAA\n-----END OPENSSH PRIVATE KEY-----", http.StatusBadRequest, "sshkey.invalid"},
		"options in front":   {`command="id" ` + line2, http.StatusBadRequest, "sshkey.invalid"},
		"two keys":           {line + "\n" + line2, http.StatusBadRequest, "sshkey.invalid"},
		"a huge line":        {strings.Repeat("a", 9<<10), http.StatusBadRequest, "sshkey.invalid"},
	} {
		code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": name, "key": tc.key})
		if code != tc.code || (tc.want != "" && out["code"] != tc.want) {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	fresh, _ := ed25519Key(t, "")
	if code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": strings.Repeat("n", 65), "key": fresh}); code != http.StatusBadRequest || out["code"] != "sshkey.name" {
		t.Errorf("a long name: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"key": fresh}); code != http.StatusBadRequest || out["code"] != "sshkey.name" {
		t.Errorf("no name and no comment: %d %v", code, out)
	}

	if code, _ := e.b.do("DELETE", "/api/account/ssh-keys/999", nil); code != http.StatusNotFound {
		t.Errorf("deleting a key that is not there: %d", code)
	}
	if code, _ := e.b.do("DELETE", "/api/account/ssh-keys/"+strconv.FormatInt(id, 10), nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

func TestSSHKeyLimit(t *testing.T) {
	e := newSFTPEnv(t)
	for i := range maxSSHKeys {
		line, _ := ed25519Key(t, "k")
		if code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": "key", "key": line}); code != http.StatusCreated {
			t.Fatalf("key %d: %d %v", i, code, out)
		}
	}
	line, _ := ed25519Key(t, "one too many")
	if code, out := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": "key", "key": line}); code != http.StatusConflict || out["code"] != "sshkey.too_many" {
		t.Errorf("past the limit: %d %v", code, out)
	}
}

func TestSFTPPasswordLogin(t *testing.T) {
	e := newSFTPEnv(t)
	vols, _ := e.s.Store.Volumes(context.Background(), "survival")

	if code, _ := e.login(t, sftpd.AuthRequest{Password: "anything"}); code != http.StatusForbidden {
		t.Errorf("a server with no password: %d", code)
	}
	code, out := e.b.do("GET", "/api/games/survival/sftp", nil)
	if code != http.StatusOK || out["password_set"] != false || out["user"] != "survival" || out["port"] != 2222.0 {
		t.Fatalf("before: %d %v", code, out)
	}

	code, out = e.b.do("POST", "/api/games/survival/sftp/password", nil)
	pw, _ := out["password"].(string)
	if code != http.StatusOK || len(pw) != sftpPasswordLen || strings.ContainsAny(pw, "0oO1lI") {
		t.Fatalf("make a password: %d %v", code, out)
	}
	if hash, _ := e.s.Store.SFTPPassword(context.Background(), "survival"); hash == "" || strings.Contains(hash, pw) || !strings.HasPrefix(hash, "$argon2id$") {
		t.Errorf("the stored password is %q", hash)
	}
	if _, out := e.b.do("GET", "/api/games/survival/sftp", nil); out["password_set"] != true || out["password"] != nil {
		t.Errorf("after: %v", out)
	}

	code, out = e.login(t, sftpd.AuthRequest{Password: pw})
	if code != http.StatusOK || out["server"] != "survival" || out["volume"] != vols[0].Name || out["uid"] != 988.0 || out["gid"] != 988.0 {
		t.Fatalf("log in: %d %v", code, out)
	}
	// The account's own password is not a way in.
	if code, _ := e.login(t, sftpd.AuthRequest{Password: "long enough pw"}); code != http.StatusForbidden {
		t.Errorf("the panel password: %d", code)
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Password: pw + "x"}); code != http.StatusForbidden {
		t.Errorf("a wrong password: %d", code)
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Server: "nothing-here", Password: pw}); code != http.StatusForbidden {
		t.Errorf("a server that does not exist: %d", code)
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Password: ""}); code != http.StatusForbidden {
		t.Errorf("an empty password: %d", code)
	}

	// Making another one ends the old.
	_, out = e.b.do("POST", "/api/games/survival/sftp/password", nil)
	pw2 := out["password"].(string)
	if pw2 == pw {
		t.Error("the same password twice")
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Password: pw}); code != http.StatusForbidden {
		t.Errorf("the replaced password: %d", code)
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Password: pw2}); code != http.StatusOK {
		t.Errorf("the new password: %d", code)
	}
	if code, _ := e.b.do("DELETE", "/api/games/survival/sftp/password", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Password: pw2}); code != http.StatusForbidden {
		t.Errorf("a removed password: %d", code)
	}

	// An app that is not a game server has no SFTP, even with a password row.
	if code, out := e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "busybox:1.37", "port": 8080}); code != http.StatusCreated {
		t.Fatalf("create app: %d %v", code, out)
	}
	e.s.Store.SetSFTPPassword(context.Background(), "web", "$argon2id$v=19$m=1,t=1,p=1$AAAA$AAAA", e.s.now())
	if code, _ := e.login(t, sftpd.AuthRequest{Server: "web", Password: "x"}); code != http.StatusForbidden {
		t.Errorf("an app that is not a game server: %d", code)
	}
}

func TestWrongSFTPPasswordsAreLimited(t *testing.T) {
	e := newSFTPEnv(t)
	_, out := e.b.do("POST", "/api/games/survival/sftp/password", nil)
	pw := out["password"].(string)
	line, pub := ed25519Key(t, "k")
	if code, _ := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": "k", "key": line}); code != http.StatusCreated {
		t.Fatal("add key")
	}
	for i := range 10 {
		if code, _ := e.login(t, sftpd.AuthRequest{Password: "wrong"}); code != http.StatusForbidden {
			t.Fatalf("attempt %d: %d", i, code)
		}
	}
	if code, out := e.login(t, sftpd.AuthRequest{Password: pw}); code != http.StatusTooManyRequests || out["code"] != "sftp.wait" {
		t.Errorf("the right password after ten wrong ones: %d %v", code, out)
	}
	// Keys cannot be guessed, so they are not held up.
	if code, _ := e.login(t, sftpd.AuthRequest{Key: pub.Marshal()}); code != http.StatusOK {
		t.Errorf("a key while passwords are limited: %d", code)
	}
}

func TestSFTPKeyLogin(t *testing.T) {
	e := newSFTPEnv(t)
	line, pub := ed25519Key(t, "k")
	if code, _ := e.b.do("POST", "/api/account/ssh-keys", map[string]string{"name": "k", "key": line}); code != http.StatusCreated {
		t.Fatal("add key")
	}
	vols, _ := e.s.Store.Volumes(context.Background(), "survival")

	code, out := e.login(t, sftpd.AuthRequest{Key: pub.Marshal()})
	if code != http.StatusOK || out["volume"] != vols[0].Name || out["account"] == nil {
		t.Fatalf("a key on the account: %d %v", code, out)
	}
	_, other := ed25519Key(t, "x")
	for name, req := range map[string]sftpd.AuthRequest{
		"a key that is on no account": {Key: other.Marshal()},
		"bytes that are not a key":    {Key: []byte("junk")},
		"a server that is not there":  {Server: "nope", Key: pub.Marshal()},
	} {
		if code, _ := e.login(t, req); code != http.StatusForbidden {
			t.Errorf("%s: %d", name, code)
		}
	}
	// A key that was removed stops working at once.
	keys, _ := e.s.Store.SSHKeys(context.Background(), loginID(t, e))
	if err := e.s.Store.DeleteSSHKey(context.Background(), keys[0].UserID, keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if code, _ := e.login(t, sftpd.AuthRequest{Key: pub.Marshal()}); code != http.StatusForbidden {
		t.Errorf("a removed key: %d", code)
	}
}

func loginID(t *testing.T, e *appEnv) int64 {
	t.Helper()
	acct, err := e.s.Store.AccountByEmail(context.Background(), "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return acct.ID
}

func TestOnlyAdministratorsManageGamesOverSFTP(t *testing.T) {
	// Keys reach a server through mayManageGame, which follows managesGame.
	if !mayManageGame(store.Account{User: store.User{Admin: true}}) || mayManageGame(store.Account{}) {
		t.Error("only an administrator may manage a game server")
	}
}

func TestWhoMayAskAsSFTP(t *testing.T) {
	e := newSFTPEnv(t)
	for name, tc := range map[string]struct {
		uid          uint32
		method, path string
		want         int
	}{
		"the service asks to log in":          {sftpUID, "POST", "/local/sftp/auth", http.StatusForbidden}, // an empty request is refused, not unrouted
		"the service reads the web api":       {sftpUID, "GET", "/api/setup", http.StatusNotFound},
		"the service asks for a setup link":   {sftpUID, "POST", "/local/setup-link", http.StatusNotFound},
		"the service reads the game":          {sftpUID, "GET", "/api/games/survival", http.StatusNotFound},
		"the proxy reaches the sftp routes":   {proxyUID, "POST", "/local/sftp/auth", http.StatusMethodNotAllowed},
		"root reaches the sftp routes":        {0, "POST", "/local/sftp/config", http.StatusNotFound},
		"someone else reaches the sftp route": {1000, "POST", "/local/sftp/auth", http.StatusForbidden},
	} {
		code, _ := sftpAsk(e, tc.uid, tc.method, tc.path, sftpd.AuthRequest{Server: "survival", IP: "127.0.0.1"})
		if code != tc.want {
			t.Errorf("%s: %d, want %d", name, code, tc.want)
		}
	}
	// Without a user for the service, nothing gets in as it.
	e.s.SFTPUID = func() uint32 { return 0 }
	if code, _ := sftpAsk(e, sftpUID, "POST", "/local/sftp/config", sftpd.ConfigRequest{}); code != http.StatusForbidden {
		t.Errorf("the service before its user exists: %d", code)
	}
}

func TestSFTPPortAndHostKey(t *testing.T) {
	e := newSFTPEnv(t)
	code, out := e.b.do("GET", "/api/sftp", nil)
	if code != http.StatusOK || out["port"] != 2222.0 || out["running"] != false || out["host_key"] != "" {
		t.Fatalf("before the service reports: %d %v", code, out)
	}
	code, out = sftpAsk(e, sftpUID, "POST", "/local/sftp/config", sftpd.ConfigRequest{Fingerprint: "SHA256:abc"})
	if code != http.StatusOK || out["port"] != 2222.0 {
		t.Fatalf("config: %d %v", code, out)
	}
	if _, out := e.b.do("GET", "/api/sftp", nil); out["running"] != true || out["host_key"] != "SHA256:abc" {
		t.Errorf("after: %v", out)
	}
	e.core.address.Address = "203.0.113.7"
	if _, out := e.b.do("GET", "/api/games/survival/sftp", nil); out["host_key"] != "SHA256:abc" || out["host"] != "203.0.113.7" {
		t.Errorf("game: %v", out)
	}

	for _, port := range []int{0, 80, 1023, 65536} {
		if code, out := e.b.do("PUT", "/api/sftp", map[string]int{"port": port}); code != http.StatusBadRequest || out["code"] != "sftp.bad_port" {
			t.Errorf("port %d: %d %v", port, code, out)
		}
	}
	pool, _ := e.s.Store.Allocations(context.Background(), store.ThisNode)
	if code, out := e.b.do("PUT", "/api/sftp", map[string]int{"port": pool[0].Port}); code != http.StatusConflict || out["code"] != "sftp.port_taken" {
		t.Errorf("a port of the pool: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", "/api/sftp", map[string]int{"port": 2300}); code != http.StatusOK || out["port"] != 2300.0 {
		t.Fatalf("set: %d %v", code, out)
	}
	if _, out := sftpAsk(e, sftpUID, "POST", "/local/sftp/config", sftpd.ConfigRequest{Fingerprint: "SHA256:abc"}); out["port"] != 2300.0 {
		t.Errorf("the service is told %v", out)
	}
}

func TestSFTPRoom(t *testing.T) {
	e := newSFTPEnv(t)
	vols, _ := e.s.Store.Volumes(context.Background(), "survival")
	if _, out := sftpAsk(e, sftpUID, "POST", "/local/sftp/room", sftpd.RoomRequest{Server: "survival"}); out["room"] != nil {
		t.Errorf("before a measurement: %v", out)
	}
	e.s.sizes.set(map[string]int64{vols[0].Name: vols[0].LimitMB<<20 - 1000}, e.s.now())
	if _, out := sftpAsk(e, sftpUID, "POST", "/local/sftp/room", sftpd.RoomRequest{Server: "survival"}); out["room"] != 1000.0 {
		t.Errorf("room %v", out)
	}
}
