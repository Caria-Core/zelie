package install

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultSFTPPort is where the SFTP server listens until the panel says
// otherwise. It is Pterodactyl's port too.
const DefaultSFTPPort = 2222

// SetUpSFTP gives a server that was installed before SFTP existed the SFTP
// server: its user, its units, and a place in the firewall. An update
// restarts the services it knew of, so the new ones have to come from the
// core, which starts on the new binary. It does nothing on a server that
// has both already, and reports whether it made anything.
func SetUpSFTP(ctx context.Context, run ExecFunc, root string) (bool, error) {
	units := map[string]string{SFTPService + ".service": SFTPUnit(), SFTPSocket: SFTPSocketUnit()}
	made, err := ensureUser(ctx, run, SFTPUser)
	if err != nil {
		return false, err
	}
	missing := made
	for name := range units {
		if _, err := os.Stat(filepath.Join(root, UnitDir, name)); err != nil {
			missing = true
		}
	}
	if !missing {
		return false, nil
	}
	for name, body := range units {
		if err := os.WriteFile(filepath.Join(root, UnitDir, name), []byte(body), 0o644); err != nil {
			return false, err
		}
	}
	if out, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
		return false, fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(out))
	}
	if out, err := run(ctx, "systemctl", "enable", "--now", SFTPSocket); err != nil {
		return false, fmt.Errorf("systemctl enable %s: %v: %s", SFTPSocket, err, strings.TrimSpace(out))
	}
	openSFTPPort(ctx, run)
	return true, nil
}

// openSFTPPort allows the default SFTP port when ufw is active. A port the
// administrator chose later is theirs to open; the panel says so. A firewall
// that refuses is no reason to stop an install, so it is only reported.
func openSFTPPort(ctx context.Context, run ExecFunc) (bool, string, error) {
	out, err := run(ctx, "ufw", "status")
	if err != nil || !strings.Contains(out, "Status: active") {
		return false, "ufw is not active", nil
	}
	port := fmt.Sprintf("%d/tcp", DefaultSFTPPort)
	if out, err := run(ctx, "ufw", "allow", port, "comment", "Zelie SFTP"); err != nil {
		return false, fmt.Sprintf("ufw would not allow %s: %s", port, strings.TrimSpace(out)), nil
	}
	return true, port + " allowed in ufw", nil
}
