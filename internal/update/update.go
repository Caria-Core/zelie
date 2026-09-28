// Package update replaces the running Zelie with a signed release. The core
// fetches and checks the release; a short-lived systemd unit, run from the
// binary being replaced, restarts the services and puts the old binary back
// if the new one does not come up.
package update

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// publicKey checks every release. install.sh carries the same key; a test
// keeps the two equal.
//
//go:embed release.pub
var publicKey []byte

// Repo is where releases are published.
const Repo = "Caria-Core/zelie"

var validVersion = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// Valid reports whether v is a release version such as v1.2.3.
func Valid(v string) bool { return validVersion.MatchString(v) }

// Newer reports whether version a comes after b. A build that is not a
// release, such as "dev", is never older than anything: it cannot be told
// what it would be updated to.
func Newer(a, b string) bool {
	ma, mb := validVersion.FindStringSubmatch(a), validVersion.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x > y
		}
	}
	return false
}

// Fetcher downloads releases.
type Fetcher struct {
	Client *http.Client
	// Base is where release files are, with the version appended; tests
	// point it at a local server.
	Base string
	// Key replaces the embedded public key in tests.
	Key ed25519.PublicKey
}

func (f *Fetcher) base() string {
	if f.Base != "" {
		return f.Base
	}
	return "https://github.com/" + Repo + "/releases/download/"
}

func (f *Fetcher) key() (ed25519.PublicKey, error) {
	if f.Key != nil {
		return f.Key, nil
	}
	return ParseKey(publicKey)
}

// ParseKey reads an Ed25519 public key in PEM, as openssl writes it.
func ParseKey(data []byte) (ed25519.PublicKey, error) {
	b, _ := pem.Decode(data)
	if b == nil {
		return nil, errors.New("no PEM block in the public key")
	}
	k, err := x509.ParsePKIXPublicKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the public key is not Ed25519")
	}
	return pub, nil
}

// Binary downloads the release's binary for arch and returns it once its
// checksum matches the signed list.
func (f *Fetcher) Binary(ctx context.Context, version, arch string) ([]byte, error) {
	if !Valid(version) {
		return nil, fmt.Errorf("invalid version %q", version)
	}
	key, err := f.key()
	if err != nil {
		return nil, err
	}
	sums, err := f.get(ctx, version, "SHA256SUMS", 64<<10)
	if err != nil {
		return nil, err
	}
	sig, err := f.get(ctx, version, "SHA256SUMS.sig", 1<<10)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key, sums, sig) {
		return nil, errors.New("the release's signature does not match Zelie's key")
	}
	name := "zelie-linux-" + arch
	want, err := sumFor(sums, name)
	if err != nil {
		return nil, err
	}
	bin, err := f.get(ctx, version, name, 256<<20)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("%s does not match its signed checksum", name)
	}
	return bin, nil
}

// sumFor finds a file's checksum in the output of sha256sum.
func sumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		sum, file, ok := strings.Cut(sc.Text(), "  ")
		if ok && file == name && len(sum) == 64 {
			return sum, nil
		}
	}
	return "", fmt.Errorf("the release has no %s", name)
}

func (f *Fetcher) get(ctx context.Context, version, file string, limit int64) ([]byte, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute}
	}
	url := f.base() + version + "/" + file
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("download %s: larger than expected", url)
	}
	return b, nil
}
