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
	"github.com/Caria-Core/zelie/internal/msg"
)

// The binary being replaced is kept next to the new one until the next
// update, for the rollback. Going back by hand needs the panel's database
// from before the update as well, and that copy does not outlive the update.
const (
	Old       = install.Binary + ".old"
	StateFile = "/var/lib/zelie/update.json"
	// Unit is the transient systemd unit that finishes an update. Its
	// fixed name also keeps two updates from running at once.
	Unit = "zelie-update"
	// HealthTimeout is how long the new version has to come up.
	HealthTimeout = 90 * time.Second
)

// Why an update failed, and what became of the version it replaced.
var (
	errRestart  = msg.Define(0, "update.restart_failed", "The services could not be restarted: {detail}")
	errNotUp    = msg.Define(0, "update.not_up", "{version} did not come up in {timeout}: {detail}")
	msgBack     = msg.Define(0, "update.back", "{version} is running again.")
	errNoOld    = msg.Define(0, "update.old_unread", "The old version could not be read back: {detail}")
	errNoPutOld = msg.Define(0, "update.old_unplaced", "The old version could not be put back: {detail}")
	errBackDown = msg.Define(0, "update.back_failed", "{version} did not come back up either: {detail}")
	errNoSave   = msg.Define(0, "update.db_unsaved", "The panel's database could not be copied before the update: {detail}")
	errBackNoDB = msg.Define(0, "update.back_without_db", "{version} is running again, but the panel's database could not be put back: {detail}. Its copy is {copy}.")
)

// Result is the outcome of the last update, as the panel shows it.
type Result struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Running bool   `json:"running,omitempty"`
	OK      bool   `json:"ok"`
	// Reason is why the update failed and Back what became of the old
	// version, for the web interface to say in the user's language. Error
	// is both in English, as earlier versions saved it.
	Reason *msg.Msg  `json:"reason,omitempty"`
	Back   *msg.Msg  `json:"back,omitempty"`
	Error  string    `json:"error,omitempty"`
	At     time.Time `json:"at"`
}

// Place puts bin where the services run from, keeping the current binary
// as Old. Root is prepended to the paths; empty on a server.
func Place(root string, bin []byte) error {
	target := filepath.Join(root, install.Binary)
	removeStrays(filepath.Dir(target))
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

// removeStrays deletes what a writeAtomic that was cut short left in dir:
// a download's worth of bytes that nothing would ever clean up. Updates
// do not overlap, so none of them is in use.
func removeStrays(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		for _, name := range []string{install.Binary, Old} {
			if strings.HasPrefix(e.Name(), filepath.Base(name)+".tmp-") {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// Revert puts Old back as the binary the services run from, for an update
// that did not come up or could not be started at all.
func Revert(root string) error {
	bin, err := os.ReadFile(filepath.Join(root, Old))
	if err != nil {
		return fmt.Errorf("the old binary could not be read back: %w", err)
	}
	if err := writeAtomic(filepath.Join(root, install.Binary), bin); err != nil {
		return fmt.Errorf("the old binary could not be put back: %w", err)
	}
	return nil
}

// writeAtomic replaces path with data in one step. The file is written
// under a name of its own, so two writers never share a half-written one.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o755)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
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
	saved, err := f.saveDatabase(ctx)
	if err != nil {
		err = errNoSave.Err("detail", err.Error())
	} else {
		err = f.restartAndWait(ctx, to)
	}
	if err == nil {
		f.dropDatabaseCopy()
		res.OK = true
		f.save(res)
		return res
	}
	reason := msg.Wrap(err)
	res.Reason = &reason
	// Back to the binary that worked, and to the database it understands.
	var back msg.Msg
	if bin, rerr := os.ReadFile(filepath.Join(f.Root, Old)); rerr != nil {
		back = errNoOld.With("detail", rerr.Error())
	} else if werr := writeAtomic(filepath.Join(f.Root, install.Binary), bin); werr != nil {
		back = errNoPutOld.With("detail", werr.Error())
	} else {
		var dberr error
		if saved {
			dberr = f.restoreDatabase(ctx)
		}
		switch rerr := f.restartAndWait(ctx, from); {
		case rerr != nil:
			detail := msg.Wrap(rerr).Text
			if dberr != nil {
				detail += "; the panel's database could not be put back either: " + dberr.Error()
			}
			back = errBackDown.With("version", from, "detail", detail)
		case dberr != nil:
			// The copy stays, for whoever puts it back by hand.
			back = errBackNoDB.With("version", from, "detail", dberr.Error(), "copy", DBCopy)
		default:
			back = msgBack.With("version", from)
			f.dropDatabaseCopy()
		}
	}
	res.Back = &back
	res.Error = strings.TrimSuffix(reason.Text, ".") + ". " + back.Text
	f.save(res)
	return res
}

func (f *Finisher) restartAndWait(ctx context.Context, version string) error {
	args := append([]string{"restart"}, install.Services...)
	if out, err := f.Exec(ctx, "systemctl", args...); err != nil {
		return errRestart.Err("detail", fmt.Sprintf("%v: %s", err, strings.TrimSpace(out)))
	}
	// The SFTP server follows the others, but does not decide the update:
	// it may not be set up yet on a server that never had it, and its port
	// may be taken by another program. A server that is not running stays
	// stopped until someone connects, so it costs nothing to update.
	f.Exec(ctx, "systemctl", "try-restart", install.SFTPService+".service")
	f.Exec(ctx, "systemctl", "restart", install.SFTPSocket)
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	var err error
	for {
		if err = f.Healthy(ctx, version); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errNotUp.Err("version", version, "timeout", f.Timeout.String(), "detail", err.Error())
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
