package secret

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSealAndOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.key")
	k, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(k.Public(), "web", "STRIPE_KEY", "sk_live_x=y")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := k.Open(sealed, "web"); err != nil || got != "STRIPE_KEY=sk_live_x=y" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := k.Open(sealed, "other"); !errors.Is(err, ErrWrongApp) {
		t.Fatalf("opened for another app: %v", err)
	}

	// The key survives a restart, and the file is private.
	again, err := LoadOrCreate(path)
	if err != nil || again.Public() != k.Public() {
		t.Fatalf("reload changed the key: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode())
	}

	stranger, _ := LoadOrCreate(filepath.Join(t.TempDir(), "other.key"))
	if _, err := stranger.Open(sealed, "web"); err == nil {
		t.Fatal("another key opened the value")
	}
}

func TestBadInput(t *testing.T) {
	k, _ := LoadOrCreate(filepath.Join(t.TempDir(), "k"))
	if _, err := Seal(k.Public(), "web", "1BAD", "x"); err == nil {
		t.Error("sealed an invalid name")
	}
	for _, s := range []string{"", "!!!", "aGVsbG8="} {
		if _, err := k.Open(s, "web"); err == nil {
			t.Errorf("opened %q", s)
		}
	}
	pk, err := ParsePublicKey(k.Public().String())
	if err != nil || pk != k.Public() {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ParsePublicKey("short"); err == nil {
		t.Error("parsed a bad key")
	}
}
