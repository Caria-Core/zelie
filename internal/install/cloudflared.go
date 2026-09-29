package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

// The Cloudflare Tunnel connector, pinned like everything else Zelie
// downloads.
const cloudflaredVersion = "2026.9.3"

var cloudflaredDebs = map[string]string{
	"amd64": "bc073ef293d504cf5ac533bd0aa1c824ef6b4f358765ccaa6628a8a95cacb4b7",
	"arm64": "bcce0111878f13d26e66b1d2ea7f270c8bde4bd549e32ce74d32474521583ca3",
}

// Cloudflare shows a new tunnel's install command with the token at the end.
// Tokens are base64 of a small JSON document.
var tunnelToken = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`cloudflared(?:\.exe)?\s+service\s+install\s+([A-Za-z0-9+/=_-]{40,})`)
})

// TunnelToken takes the token out of the command Cloudflare shows, pasted
// whole, with or without sudo and the lines before it.
func TunnelToken(command string) (string, error) {
	m := tunnelToken().FindStringSubmatch(command)
	if m == nil {
		return "", errors.New(`that is not a tunnel's install command; it ends in "cloudflared service install" and a long token`)
	}
	return m[1], nil
}

// Cloudflared installs the connector from Cloudflare's package and runs it
// as a service with the tunnel's token.
type Cloudflared struct {
	Arch   string
	Client *http.Client
	Exec   func(ctx context.Context, name string, args ...string) (string, error)
}

func (c *Cloudflared) Install(ctx context.Context, token string) error {
	sum, ok := cloudflaredDebs[c.Arch]
	if !ok {
		return fmt.Errorf("no cloudflared package for %q", c.Arch)
	}
	url := fmt.Sprintf("https://github.com/cloudflare/cloudflared/releases/download/%s/cloudflared-linux-%s.deb", cloudflaredVersion, c.Arch)
	deb, err := fetch(ctx, c.Client, url, sum)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "zelie-cloudflared")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "cloudflared.deb")
	if err := os.WriteFile(file, deb, 0o644); err != nil {
		return err
	}
	if out, err := c.Exec(ctx, "dpkg", "-i", file); err != nil {
		return fmt.Errorf("dpkg -i cloudflared: %v: %s", err, out)
	}
	// The output never has the token in it; the error is kept short so it
	// cannot either.
	if out, err := c.Exec(ctx, "cloudflared", "service", "install", token); err != nil {
		return fmt.Errorf("cloudflared service install: %v: %s", err, out)
	}
	return nil
}

// fetch downloads url and checks it against its SHA-256.
func fetch(ctx context.Context, client *http.Client, url, want string) ([]byte, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	got := sha256.Sum256(data)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s", url)
	}
	return data, nil
}
