package install

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SFTPStateFile says whether SFTP is turned off on this server. A server
// without the file has it on, as every server did before the switch existed.
// The file is root's: the installer sets it with --no-sftp, and the core
// changes it when an administrator uses the panel.
const SFTPStateFile = "/var/lib/zelie/sftp.json"

type sftpState struct {
	Off bool `json:"off"`
}

// SFTPOff reports whether SFTP is turned off. A file that cannot be read or
// understood counts as off: a server that was closed on purpose must not open
// because of a damaged file.
func SFTPOff(path string) bool {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	var st sftpState
	return json.Unmarshal(b, &st) != nil || st.Off
}

// SetSFTPOff saves the choice.
func SetSFTPOff(path string, off bool) error {
	b, err := json.Marshal(sftpState{Off: off})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// stopSFTP makes sure the socket neither runs nor starts at boot. It looks
// first, so a server that is off already costs two quick questions and no
// change.
func stopSFTP(ctx context.Context, run ExecFunc) error {
	active, _ := run(ctx, "systemctl", "is-active", SFTPSocket)
	enabled, _ := run(ctx, "systemctl", "is-enabled", SFTPSocket)
	if strings.TrimSpace(active) != "active" && strings.TrimSpace(enabled) != "enabled" {
		return nil
	}
	if out, err := run(ctx, "systemctl", "disable", "--now", SFTPSocket); err != nil {
		return fmt.Errorf("systemctl disable %s: %v: %s", SFTPSocket, err, strings.TrimSpace(out))
	}
	return nil
}
