package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Caria-Core/zelie/internal/install"
)

// The new version migrates the panel's database as soon as it starts, and a
// version that finds a schema newer than the one it knows refuses to open
// it. An update that goes back would then bring up a panel that cannot start.
// So the update keeps a copy of the database from before the new version ran
// and puts it back with the old binary. DBCopy is where it waits, out of the
// panel's own directory.
const DBCopy = "/var/lib/zelie/panel.db.before-update"

// The database file has its write-ahead log beside it. A panel that was
// stopped cleanly leaves none, but one that was killed leaves one with
// changes the database file does not have yet.
const walSuffix = "-wal"

// saveDatabase stops the panel and copies its database aside. It reports
// whether there was one to copy, and whether the panel is stopped, which it
// is after an error too once the stop worked. The panel stays stopped: the
// restart that follows brings it up on the new version, after the copy.
func (f *Finisher) saveDatabase(ctx context.Context) (bool, bool, error) {
	live := filepath.Join(f.Root, install.PanelDB)
	// Lstat: a link under the database's name is something to refuse below,
	// not a reason to think there is no database.
	if _, err := os.Lstat(live); errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	} else if err != nil {
		return false, false, err
	}
	if err := f.stopPanel(ctx); err != nil {
		return false, false, err
	}
	// A copy left by an update that went wrong is of no use to this one.
	f.dropDatabaseCopy()
	saved := filepath.Join(f.Root, DBCopy)
	// The log goes first and the database last, so a copy that has the
	// database has its log too.
	if err := copyFile(saved+walSuffix, live+walSuffix, -1, -1); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, true, err
	}
	if err := copyFile(saved, live, -1, -1); err != nil {
		f.dropDatabaseCopy()
		return false, true, err
	}
	return true, true, nil
}

// restoreDatabase puts the copy made by saveDatabase back, for the version
// the update goes back to. What the new version left beside the database
// goes: its log would not fit the old file.
func (f *Finisher) restoreDatabase(ctx context.Context) error {
	if err := f.stopPanel(ctx); err != nil {
		return err
	}
	live := filepath.Join(f.Root, install.PanelDB)
	saved := filepath.Join(f.Root, DBCopy)
	dir, err := os.Stat(filepath.Dir(live))
	if err != nil {
		return err
	}
	uid, gid := -1, -1
	if st, ok := dir.Sys().(*syscall.Stat_t); ok {
		uid, gid = int(st.Uid), int(st.Gid)
	}
	for _, suffix := range []string{walSuffix, "-shm"} {
		if err := os.Remove(live + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := copyFile(live, saved, uid, gid); err != nil {
		return err
	}
	if err := copyFile(live+walSuffix, saved+walSuffix, uid, gid); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (f *Finisher) stopPanel(ctx context.Context) error {
	return f.panelService(ctx, "stop")
}

func (f *Finisher) startPanel(ctx context.Context) error {
	return f.panelService(ctx, "start")
}

func (f *Finisher) panelService(ctx context.Context, verb string) error {
	if out, err := f.Exec(ctx, "systemctl", verb, install.PanelService); err != nil {
		return fmt.Errorf("systemctl %s %s: %v: %s", verb, install.PanelService, err, strings.TrimSpace(out))
	}
	return nil
}

// dropDatabaseCopy removes the copy, once the update it was for is over.
func (f *Finisher) dropDatabaseCopy() {
	saved := filepath.Join(f.Root, DBCopy)
	os.Remove(saved)
	os.Remove(saved + walSuffix)
}

// openPlain opens path for reading if it is a plain file. The panel owns its
// directory, so what it leaves under the database's name is not to be
// trusted: this runs as root, and a link would make it read any file on the
// server, a pipe would make it wait for ever with the panel stopped.
func openPlain(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	// A second name for the file could lead out of the directory as well.
	if sys, ok := st.Sys().(*syscall.Stat_t); !st.Mode().IsRegular() || ok && sys.Nlink != 1 {
		f.Close()
		return nil, fmt.Errorf("%s is not a plain file", path)
	}
	return f, nil
}

// copyFile copies src to dst in one step, readable by its owner alone. A
// uid or gid of -1 leaves the owner as it is.
func copyFile(dst, src string, uid, gid int) (err error) {
	in, err := openPlain(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(out.Name())
		}
	}()
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if err == nil {
		err = chown(out, uid, gid)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(out.Name(), dst)
	}
	return err
}

// chown gives f to uid and gid, when it is not theirs already. A negative
// id stands for whoever has it now.
func chown(f *os.File, uid, gid int) error {
	if st, err := f.Stat(); err == nil {
		if sys, ok := st.Sys().(*syscall.Stat_t); ok {
			if uid == int(sys.Uid) {
				uid = -1
			}
			if gid == int(sys.Gid) {
				gid = -1
			}
		}
	}
	if uid < 0 && gid < 0 {
		return nil
	}
	return f.Chown(uid, gid)
}
