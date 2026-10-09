package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/hostkey"
	"github.com/Caria-Core/zelie/internal/peer"
)

func newSFTPSocket(t *testing.T) (*Server, *SFTPSocket, *[]string) {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	dir := t.TempDir()
	sock := &SFTPSocket{
		DropIn:     filepath.Join(dir, "zelie-sftp.socket.d", "port.conf"),
		HostKeyDir: filepath.Join(dir, "etc", "zelie-sftp"),
		User:       me.Username,
		StateFile:  filepath.Join(dir, "var", "sftp.json"),
		Run: func(_ context.Context, name string, args ...string) (string, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			if len(args) > 0 && args[0] == "is-active" {
				return "active\n", nil
			}
			return "", nil
		},
	}
	s, _ := newServer()
	s.Allowed.Routes = map[uint32][]string{4242: SFTPRoutes}
	s.SFTPUID = 4242
	s.SFTPSocket = sock
	return s, sock, &ran
}

func TestSFTPPortIsWrittenOnceAndAppliedOnChange(t *testing.T) {
	s, sock, ran := newSFTPSocket(t)
	panel := &peer.Peer{UID: 999}
	put := func(port string) int {
		return request(t, s, panel, "PUT", "/v1/sftp/port", `{"port":`+port+`}`).Code
	}
	if code := put("2300"); code != http.StatusNoContent {
		t.Fatalf("set: %d", code)
	}
	b, _ := os.ReadFile(sock.DropIn)
	if string(b) != "[Socket]\nListenStream=\nListenStream=2300\n" {
		t.Errorf("drop-in:\n%s", b)
	}
	want := []string{"systemctl daemon-reload", "systemctl restart zelie-sftp.socket"}
	if strings.Join(*ran, ";") != strings.Join(want, ";") {
		t.Errorf("ran %v, want %v", *ran, want)
	}
	// The same port again changes nothing and drops no connection.
	*ran = nil
	if code := put("2300"); code != http.StatusNoContent || len(*ran) != 0 {
		t.Errorf("repeat: %d, ran %v", code, *ran)
	}
	if code := put("2301"); code != http.StatusNoContent || len(*ran) != 2 {
		t.Errorf("change: %d, ran %v", code, *ran)
	}
	for _, port := range []string{"0", "80", "1023", "65536", `"x"`} {
		if code := put(port); code != http.StatusBadRequest {
			t.Errorf("port %s: %d", port, code)
		}
	}
}

func TestSFTPStatusMakesTheHostKey(t *testing.T) {
	s, sock, _ := newSFTPSocket(t)
	panel := &peer.Peer{UID: 999}
	get := func() SFTPStatus {
		rec := request(t, s, panel, "GET", "/v1/sftp", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("status: %d %s", rec.Code, rec.Body)
		}
		var st SFTPStatus
		if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	first := get()
	if !first.Listening || !strings.HasPrefix(first.HostKey, "SHA256:") {
		t.Fatalf("first: %+v", first)
	}
	if fi, err := os.Stat(filepath.Join(sock.HostKeyDir, "host_key")); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("host key file: %v %v", fi, err)
	}
	if fi, err := os.Stat(sock.HostKeyDir); err != nil || fi.Mode().Perm() != 0o750 {
		t.Errorf("host key folder: %v %v", fi, err)
	}
	if again := get(); again.HostKey != first.HostKey {
		t.Errorf("the host key changed: %s, then %s", first.HostKey, again.HostKey)
	}
}

func TestSFTPUserMayNotUseTheSocketRoutes(t *testing.T) {
	s, sock, ran := newSFTPSocket(t)
	sftp := &peer.Peer{UID: 4242}
	if rec := request(t, s, sftp, "PUT", "/v1/sftp/port", `{"port":2300}`); rec.Code != http.StatusForbidden {
		t.Errorf("the SFTP user changing the port: %d", rec.Code)
	}
	if rec := request(t, s, sftp, "GET", "/v1/sftp", ""); rec.Code != http.StatusForbidden {
		t.Errorf("the SFTP user asking for the status: %d", rec.Code)
	}
	if _, err := os.Stat(sock.DropIn); err == nil || len(*ran) != 0 {
		t.Errorf("a refused request changed something: %v", *ran)
	}
}

func TestSFTPPortGoesBackWhenTheNewOneWillNotOpen(t *testing.T) {
	s, sock, ran := newSFTPSocket(t)
	panel := &peer.Peer{UID: 999}
	if code := request(t, s, panel, "PUT", "/v1/sftp/port", `{"port":2300}`).Code; code != http.StatusNoContent {
		t.Fatalf("set: %d", code)
	}
	// systemd cannot bind 2400, as when another program has it.
	run := sock.Run
	sock.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if b, _ := os.ReadFile(sock.DropIn); len(args) > 0 && args[0] == "restart" && strings.Contains(string(b), "2400") {
			*ran = append(*ran, "systemctl restart (fails)")
			return "Job failed", errors.New("exit status 1")
		}
		return run(ctx, name, args...)
	}
	*ran = nil
	if code := request(t, s, panel, "PUT", "/v1/sftp/port", `{"port":2400}`).Code; code != http.StatusInternalServerError {
		t.Errorf("a port that will not open: %d", code)
	}
	if b, _ := os.ReadFile(sock.DropIn); string(b) != "[Socket]\nListenStream=\nListenStream=2300\n" {
		t.Errorf("the drop-in after going back:\n%s", b)
	}
	if last := (*ran)[len(*ran)-1]; last != "systemctl restart zelie-sftp.socket" {
		t.Errorf("the socket was not started on the old port: %v", *ran)
	}

	// With no drop-in before, going back removes it.
	os.Remove(sock.DropIn)
	if code := request(t, s, panel, "PUT", "/v1/sftp/port", `{"port":2400}`).Code; code != http.StatusInternalServerError {
		t.Errorf("a port that will not open, from the default: %d", code)
	}
	if _, err := os.Stat(sock.DropIn); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the drop-in is still there: %v", err)
	}
}

func TestSFTPHostKeyMovesFromTheOldFolder(t *testing.T) {
	_, sock, _ := newSFTPSocket(t)
	sock.OldKeyDir = filepath.Join(t.TempDir(), "old")
	old, err := hostkey.Ensure(sock.OldKeyDir, os.Getgid(), "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := sock.EnsureHostKey()
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(key.PublicKey()) != ssh.FingerprintSHA256(old.PublicKey()) {
		t.Error("the fingerprint changed")
	}
}

func TestSFTPCanBeTurnedOffAndOn(t *testing.T) {
	s, sock, ran := newSFTPSocket(t)
	panel := &peer.Peer{UID: 999}
	put := func(on string) int {
		return request(t, s, panel, "PUT", "/v1/sftp/enabled", `{"on":`+on+`}`).Code
	}
	status := func() SFTPStatus {
		var st SFTPStatus
		json.Unmarshal(request(t, s, panel, "GET", "/v1/sftp", "").Body.Bytes(), &st)
		return st
	}
	if !status().On {
		t.Error("a server without the state file has SFTP off")
	}
	*ran = nil

	if code := put("false"); code != http.StatusNoContent {
		t.Fatalf("turn off: %d", code)
	}
	b, _ := os.ReadFile(sock.StateFile)
	if string(b) != "{\"off\":true}\n" {
		t.Errorf("state file: %q", b)
	}
	want := "systemctl disable --now zelie-sftp.socket;systemctl stop zelie-sftp.service"
	if got := strings.Join(*ran, ";"); got != want {
		t.Errorf("ran %q, want %q", got, want)
	}
	if status().On {
		t.Error("the status says on")
	}

	// A new port is kept for later but does not start the socket.
	*ran = nil
	if code := request(t, s, panel, "PUT", "/v1/sftp/port", `{"port":2300}`).Code; code != http.StatusNoContent {
		t.Fatalf("port while off: %d", code)
	}
	if got := strings.Join(*ran, ";"); got != "systemctl daemon-reload" {
		t.Errorf("a port change while off ran %q", got)
	}

	*ran = nil
	if code := put("true"); code != http.StatusNoContent {
		t.Fatalf("turn on: %d", code)
	}
	if got := strings.Join(*ran, ";"); got != "systemctl enable --now zelie-sftp.socket" {
		t.Errorf("ran %q", got)
	}
	if !status().On {
		t.Error("the status says off")
	}
	if code := request(t, s, panel, "PUT", "/v1/sftp/enabled", `{"on":"yes"}`).Code; code != http.StatusBadRequest {
		t.Errorf("a bad body: %d", code)
	}
	sftp := &peer.Peer{UID: 4242}
	if code := request(t, s, sftp, "PUT", "/v1/sftp/enabled", `{"on":false}`).Code; code != http.StatusForbidden {
		t.Errorf("the SFTP user turned SFTP off: %d", code)
	}
}

func TestSFTPStaysOffWhenTheCoreFails(t *testing.T) {
	s, sock, _ := newSFTPSocket(t)
	panel := &peer.Peer{UID: 999}
	run := sock.Run
	sock.Run = func(ctx context.Context, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "enable" {
			return "port in use", errors.New("exit status 1")
		}
		return run(ctx, name, args...)
	}
	request(t, s, panel, "PUT", "/v1/sftp/enabled", `{"on":false}`)
	if code := request(t, s, panel, "PUT", "/v1/sftp/enabled", `{"on":true}`).Code; code != http.StatusInternalServerError {
		t.Errorf("turn on with the port taken: %d", code)
	}
	if b, _ := os.ReadFile(sock.StateFile); !strings.Contains(string(b), "true") {
		t.Errorf("SFTP was marked on although the socket did not start: %q", b)
	}
}
