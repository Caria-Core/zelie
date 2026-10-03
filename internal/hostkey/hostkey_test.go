package hostkey

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func fp(k ssh.Signer) string { return ssh.FingerprintSHA256(k.PublicKey()) }

func TestEnsureMakesAKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "zelie-sftp")
	key, err := Ensure(dir, os.Getgid(), "")
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(dir, File)); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("key file: %v %v", st, err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o750 {
		t.Errorf("folder: %v %v", st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, tmpName)); err == nil {
		t.Error("the temporary file is left behind")
	}
	again, err := Ensure(dir, os.Getgid(), "")
	if err != nil || fp(again) != fp(key) {
		t.Errorf("the key changed: %v", err)
	}
	loaded, err := Load(filepath.Join(dir, File))
	if err != nil || fp(loaded) != fp(key) {
		t.Errorf("load: %v", err)
	}
}

func TestEnsureFixesTheModeOfAnExistingKey(t *testing.T) {
	dir := t.TempDir()
	if _, err := Ensure(dir, os.Getgid(), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, File), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, os.Getgid(), ""); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, File)); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
}

func oldKey(t *testing.T) (string, ssh.Signer) {
	t.Helper()
	legacy := t.TempDir()
	// Ensure is the easiest way to get a valid ed25519 key file.
	k, err := Ensure(legacy, os.Getgid(), "")
	if err != nil {
		t.Fatal(err)
	}
	return legacy, k
}

func TestEnsureMigratesTheOldKey(t *testing.T) {
	legacy, old := oldKey(t)
	dir := filepath.Join(t.TempDir(), "new")
	key, err := Ensure(dir, os.Getgid(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	if fp(key) != fp(old) {
		t.Error("the fingerprint changed")
	}
	if loaded, err := Load(filepath.Join(dir, File)); err != nil || fp(loaded) != fp(old) {
		t.Errorf("stored key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, File)); err != nil {
		t.Errorf("the old key was removed: %v", err)
	}
}

func TestEnsureRefusesAnOldKeyThatIsALink(t *testing.T) {
	_, outside := oldKey(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	_, err := Ensure(target, os.Getgid(), "")
	if err != nil {
		t.Fatal(err)
	}
	legacy := t.TempDir()
	if err := os.Symlink(filepath.Join(target, File), filepath.Join(legacy, File)); err != nil {
		t.Fatal(err)
	}
	key, err := Ensure(filepath.Join(t.TempDir(), "new"), os.Getgid(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	linked, _ := Load(filepath.Join(target, File))
	if fp(key) == fp(linked) || fp(key) == fp(outside) {
		t.Error("the key behind the link was taken")
	}
}

func TestEnsureSkipsOldKeysItCannotUse(t *testing.T) {
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(ec, "")
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"garbage": []byte("not a key"),
		"ecdsa":   pem.EncodeToMemory(block),
		"huge":    make([]byte, maxSize+1),
	} {
		legacy := t.TempDir()
		if err := os.WriteFile(filepath.Join(legacy, File), body, 0o600); err != nil {
			t.Fatal(err)
		}
		key, err := Ensure(filepath.Join(t.TempDir(), "new"), os.Getgid(), legacy)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if key.PublicKey().Type() != ssh.KeyAlgoED25519 {
			t.Errorf("%s: got a %s key", name, key.PublicKey().Type())
		}
	}
}

func TestEnsureDoesNotFollowAPlantedTempFile(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(victim, []byte("root:x:0:0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, tmpName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, os.Getgid(), ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(victim)
	st, _ := os.Stat(victim)
	if string(b) != "root:x:0:0\n" || st.Mode().Perm() != 0o644 {
		t.Errorf("the file behind the link was changed: %q %v", b, st.Mode())
	}
	if st, err := os.Lstat(filepath.Join(dir, File)); err != nil || !st.Mode().IsRegular() {
		t.Errorf("key file: %v %v", st, err)
	}
}

func TestEnsureRefusesAFolderThatIsALink(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(link, os.Getgid(), ""); err == nil {
		t.Error("a link was accepted as the key folder")
	}
	if _, err := os.Stat(filepath.Join(real, File)); err == nil {
		t.Error("a key was written behind the link")
	}
}

func TestEnsureRefusesAKeyThatIsALink(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(t.TempDir(), "k")
	if _, err := Ensure(filepath.Dir(other), os.Getgid(), ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(other), File), filepath.Join(dir, File)); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(dir, os.Getgid(), ""); err == nil {
		t.Error("a linked key was accepted")
	}
}
