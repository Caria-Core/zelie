package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/install"
)

// release serves a signed release the way GitHub does.
func release(t *testing.T, key ed25519.PrivateKey, bin string, tamper func(files map[string][]byte)) string {
	sum := sha256.Sum256([]byte(bin))
	sums := []byte(hex.EncodeToString(sum[:]) + "  zelie-linux-amd64\n" + strings.Repeat("0", 64) + "  install.sh\n")
	files := map[string][]byte{
		"SHA256SUMS":        sums,
		"SHA256SUMS.sig":    ed25519.Sign(key, sums),
		"zelie-linux-amd64": []byte(bin),
	}
	if tamper != nil {
		tamper(files)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/v1.2.3/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

func TestFetch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &Fetcher{Base: release(t, priv, "new binary", nil), Key: pub}
	bin, err := f.Binary(context.Background(), "v1.2.3", "amd64")
	if err != nil || string(bin) != "new binary" {
		t.Fatalf("%q, %v", bin, err)
	}
	if _, err := f.Binary(context.Background(), "v1.2.3", "arm64"); err == nil {
		t.Error("an architecture the release lacks")
	}
	if _, err := f.Binary(context.Background(), "latest; rm -rf /", "amd64"); err == nil {
		t.Error("a version that is not one")
	}

	// Someone swaps the binary: the checksum no longer matches.
	f.Base = release(t, priv, "new binary", func(m map[string][]byte) { m["zelie-linux-amd64"] = []byte("evil") })
	if _, err := f.Binary(context.Background(), "v1.2.3", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("swapped binary: %v", err)
	}
	// And the list too, but they cannot sign it.
	f.Base = release(t, priv, "new binary", func(m map[string][]byte) {
		sum := sha256.Sum256([]byte("evil"))
		m["zelie-linux-amd64"] = []byte("evil")
		m["SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + "  zelie-linux-amd64\n")
	})
	if _, err := f.Binary(context.Background(), "v1.2.3", "amd64"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("swapped list: %v", err)
	}
	// Signed with another key.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	f.Base = release(t, other, "new binary", nil)
	if _, err := f.Binary(context.Background(), "v1.2.3", "amd64"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("another key: %v", err)
	}
}

func TestEmbeddedKeyMatchesInstallScript(t *testing.T) {
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), strings.TrimSpace(string(publicKey))) {
		t.Error("install.sh and the binary carry different release keys")
	}
	if _, err := ParseKey(publicKey); err != nil {
		t.Error(err)
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.4", "v0.1.3", true},
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.1.3", "v0.1.3", false},
		{"v0.1.2", "v0.1.3", false},
		{"v0.1.4", "dev", false},
		{"v0.1.4-rc1", "v0.1.3", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func setup(t *testing.T) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, filepath.Dir(install.Binary)), 0o755)
	os.MkdirAll(filepath.Join(root, filepath.Dir(StateFile)), 0o755)
	os.WriteFile(filepath.Join(root, install.Binary), []byte("old binary"), 0o755)
	if err := Place(root, []byte("new binary")); err != nil {
		t.Fatal(err)
	}
	return root
}

func read(root, path string) string {
	b, _ := os.ReadFile(filepath.Join(root, path))
	return string(b)
}

func TestUpdateThatComesUp(t *testing.T) {
	root := setup(t)
	if read(root, install.Binary) != "new binary" || read(root, Old) != "old binary" {
		t.Fatalf("placed %q, kept %q", read(root, install.Binary), read(root, Old))
	}
	var ran []string
	f := &Finisher{
		Root: root, Timeout: time.Second, Now: time.Now,
		Exec: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			ran = append(ran, cmd)
			// A server without the SFTP unit, as one from before it existed.
			if strings.HasSuffix(cmd, "zelie-sftp.service") || strings.HasSuffix(cmd, "zelie-sftp.socket") {
				return "Unit zelie-sftp.socket not found.", errors.New("exit status 5")
			}
			return "", nil
		},
		Healthy: func(_ context.Context, v string) error { return nil },
	}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	want := []string{"systemctl restart zelie-core zelie-proxy zelie-panel", "systemctl try-restart zelie-sftp.service", "systemctl restart zelie-sftp.socket"}
	if !res.OK || !slices.Equal(ran, want) {
		t.Errorf("%+v, ran %v", res, ran)
	}
	if last, _ := Last(root); !last.OK || last.To != "v1.2.3" {
		t.Errorf("saved %+v", last)
	}
}

func TestUpdateThatFailsGoesBack(t *testing.T) {
	root := setup(t)
	restarts := 0
	f := &Finisher{
		Root: root, Timeout: 1500 * time.Millisecond, Now: time.Now,
		Exec: func(context.Context, string, ...string) (string, error) { restarts++; return "", nil },
		// Only the old version ever answers.
		Healthy: func(_ context.Context, v string) error {
			if read(root, install.Binary) == "old binary" && v == "v1.2.2" {
				return nil
			}
			return errors.New("the panel does not answer")
		},
	}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || !strings.Contains(res.Error, "did not come up") || !strings.Contains(res.Error, "v1.2.2 is running again") {
		t.Errorf("%+v", res)
	}
	if read(root, install.Binary) != "old binary" || restarts != 6 {
		t.Errorf("binary %q after %d restarts", read(root, install.Binary), restarts)
	}
	if last, _ := Last(root); last.OK || last.Error == "" {
		t.Errorf("saved %+v", last)
	}
}
