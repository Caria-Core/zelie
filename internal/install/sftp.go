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
// core, which starts on the new binary. The same goes for a unit a newer
// release changed, such as a tighter sandbox: it is compared by content, not
// by whether the file exists. A server running the old unit is left alone
// until it exits, which it does when nobody is connected, and starts again
// on the new one. It does nothing on a server that has both up to date, and
// reports whether it made or changed anything.
//
// On a server where SFTP is turned off the units are still kept current, so
// turning it on later works, but the socket is not started and no firewall
// rule is made.
func SetUpSFTP(ctx context.Context, run ExecFunc, root string) (bool, error) {
	units := map[string]string{SFTPService + ".service": SFTPUnit(), SFTPSocket: SFTPSocketUnit()}
	fresh, err := ensureUser(ctx, run, SFTPUser)
	if err != nil {
		return false, err
	}
	changed := fresh
	for name, body := range units {
		file := filepath.Join(root, UnitDir, name)
		old, err := os.ReadFile(file)
		if err == nil && string(old) == body {
			continue
		}
		if err != nil {
			fresh = true
		}
		changed = true
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			return false, err
		}
	}
	off := SFTPOff(filepath.Join(root, SFTPStateFile))
	if !changed && !off {
		return false, nil
	}
	if changed {
		if out, err := run(ctx, "systemctl", "daemon-reload"); err != nil {
			return false, fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(out))
		}
	}
	if off {
		return changed, stopSFTP(ctx, run)
	}
	if out, err := run(ctx, "systemctl", "enable", "--now", SFTPSocket); err != nil {
		return false, fmt.Errorf("systemctl enable %s: %v: %s", SFTPSocket, err, strings.TrimSpace(out))
	}
	// A port the administrator closed on purpose is not opened again by an
	// update of the units.
	if fresh {
		openSFTPPort(ctx, run)
	}
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
