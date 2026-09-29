package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/install"
)

// The binary being replaced is kept next to the new one until the next
// update, for the rollback and for anyone who wants to go back by hand.
const (
	Old       = install.Binary + ".old"
	StateFile = "/var/lib/zelie/update.json"
	// Unit is the transient systemd unit that finishes an update. Its
	// fixed name also keeps two updates from running at once.
	Unit = "zelie-update"
	// HealthTimeout is how long the new version has to come up.
	HealthTimeout = 90 * time.Second
)

// Result is the outcome of the last update, as the panel shows it.
type Result struct {
	From    string    `json:"from"`
	To      string    `json:"to"`
	Running bool      `json:"running,omitempty"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	At      time.Time `json:"at"`
}

// Place puts bin where the services run from, keeping the current binary
// as Old. Root is prepended to the paths; empty on a server.
func Place(root string, bin []byte) error {
	target := filepath.Join(root, install.Binary)
	cur, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(root, Old), cur); err != nil {
		return err
	}
	// Renamed into place: the running processes keep the file they
	// started from until they restart.
	return writeAtomic(target, bin)
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Finisher restarts the services on the new binary and checks that they
// come up. It runs from Old, so a new binary that does not even start
// cannot keep the old one from coming back.
type Finisher struct {
	Root string
	Exec func(ctx context.Context, name string, args ...string) (string, error)
	// Healthy reports whether every service answers with version.
	Healthy func(ctx context.Context, version string) error
	Timeout time.Duration
	Now     func() time.Time
}

// Run finishes the update from one version to another and records how it
// went.
func (f *Finisher) Run(ctx context.Context, from, to string) Result {
	res := Result{From: from, To: to, At: f.Now()}
	err := f.restartAndWait(ctx, to)
	if err == nil {
		res.OK = true
		f.save(res)
		return res
	}
	res.Error = err.Error()
	// Back to the binary that worked.
	if bin, rerr := os.ReadFile(filepath.Join(f.Root, Old)); rerr != nil {
		res.Error += "; the old binary could not be read back: " + rerr.Error()
	} else if werr := writeAtomic(filepath.Join(f.Root, install.Binary), bin); werr != nil {
		res.Error += "; the old binary could not be put back: " + werr.Error()
	} else if rerr := f.restartAndWait(ctx, from); rerr != nil {
		res.Error += "; after going back: " + rerr.Error()
	} else {
		res.Error += "; " + from + " is running again"
	}
	f.save(res)
	return res
}

func (f *Finisher) restartAndWait(ctx context.Context, version string) error {
	args := append([]string{"restart"}, install.Services...)
	if out, err := f.Exec(ctx, "systemctl", args...); err != nil {
		return fmt.Errorf("systemctl restart: %v: %s", err, strings.TrimSpace(out))
	}
	// The SFTP server follows the others, but does not decide the update:
	// it may not be set up yet on a server that never had it, and its port
	// may be taken by another program.
	f.Exec(ctx, "systemctl", "restart", install.SFTPService)
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	var err error
	for {
		if err = f.Healthy(ctx, version); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not come up in %s: %w", version, f.Timeout, err)
		case <-time.After(time.Second):
		}
	}
}

func (f *Finisher) save(r Result) {
	b, _ := json.Marshal(r)
	writeState(filepath.Join(f.Root, StateFile), b)
}

func writeState(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Started records that an update is under way, before the unit runs.
func Started(root, from, to string, now time.Time) error {
	b, _ := json.Marshal(Result{From: from, To: to, Running: true, At: now})
	return writeState(filepath.Join(root, StateFile), b)
}

// Last reads the outcome of the last update, if there was one.
func Last(root string) (Result, error) {
	var r Result
	b, err := os.ReadFile(filepath.Join(root, StateFile))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}
