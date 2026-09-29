// Package hostkey keeps the SSH host key of the SFTP server. The core makes
// it before anyone has connected, so the panel can show its fingerprint, and
// the server reads it when it starts.
package hostkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
)

// LoadOrCreate reads the key at path, or makes an ed25519 key there if there
// is none yet. created says whether it made one.
func LoadOrCreate(path string) (key ssh.Signer, created bool, err error) {
	pemBytes, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, false, gerr
		}
		block, gerr := ssh.MarshalPrivateKey(priv, "zelie sftp host key")
		if gerr != nil {
			return nil, false, gerr
		}
		pemBytes = pem.EncodeToMemory(block)
		if gerr := os.MkdirAll(filepath.Dir(path), 0o700); gerr != nil {
			return nil, false, gerr
		}
		tmp := path + ".tmp"
		if gerr := os.WriteFile(tmp, pemBytes, 0o600); gerr != nil {
			return nil, false, gerr
		}
		if gerr := os.Rename(tmp, path); gerr != nil {
			return nil, false, gerr
		}
		created = true
	} else if err != nil {
		return nil, false, err
	}
	key, err = ssh.ParsePrivateKey(pemBytes)
	if err != nil {
		return nil, false, fmt.Errorf("read host key %s: %w", path, err)
	}
	return key, created, nil
}
