package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/Caria-Core/zelie/internal/msg"
)

// Exec runs a command in the database's container, as root there and with
// the container's environment, which holds the database's password.
type Exec func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error)

// The commands are fixed here. The panel names a kind, never a command, so
// it cannot use a backup to run something else in a container.
type kind struct {
	ext  string
	dump []string
	// A dump that stopped early still exits 0 now and then; a complete one
	// ends with this line, or starts with this for Redis.
	trailer, header string
	// load replaces the database with the file the restore copied in.
	load []string
}

// The user and database every database is made with.
const dbUser = "app"

const restoreFile = "/tmp/zelie-restore"

var kinds = map[string]kind{
	"postgres": {
		ext:     "sql",
		dump:    []string{"pg_dump", "-U", dbUser, "-d", dbUser, "--no-password"},
		trailer: "-- PostgreSQL database dump complete",
		// The whole file loads in one transaction: a failure leaves the
		// database empty rather than half restored, and the safety backup
		// taken just before has what was there.
		load: []string{"sh", "-c", `psql -X -U app -d postgres -v ON_ERROR_STOP=1 -q -c 'DROP DATABASE IF EXISTS app WITH (FORCE)' -c 'CREATE DATABASE app OWNER app' &&
psql -X -U app -d app -v ON_ERROR_STOP=1 -q --single-transaction -f ` + restoreFile + `
rc=$?; rm -f ` + restoreFile + `; exit $rc`},
	},
	"mariadb": {
		ext:     "sql",
		dump:    []string{"sh", "-c", `MYSQL_PWD="$MARIADB_PASSWORD" exec mariadb-dump -u app --single-transaction --routines --triggers --events app`},
		trailer: "-- Dump completed",
		load: []string{"sh", "-c", `export MYSQL_PWD="$MARIADB_PASSWORD"
mariadb -u app -e 'DROP DATABASE IF EXISTS app; CREATE DATABASE app' && mariadb -u app app < ` + restoreFile + `
rc=$?; rm -f ` + restoreFile + `; exit $rc`},
	},
	"redis": {
		ext: "rdb",
		// Written to a file first: redis-cli mixes its own messages into
		// standard output.
		dump:   []string{"sh", "-c", `f=/tmp/zelie-backup-$$.rdb; REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --no-auth-warning --rdb "$f" >&2 && cat "$f"; rc=$?; rm -f "$f"; exit $rc`},
		header: "REDIS",
	},
}

// A backup or restore the database refused or broke off, or a backup file
// that is not right, fails with one of these. They are meant for the user;
// other errors are Zelie's own.
var (
	errDumpFailed  = msg.Define(http.StatusUnprocessableEntity, "backup.dump_failed", "The dump failed with exit code {code}: {detail}")
	errDumpKind    = msg.Define(http.StatusUnprocessableEntity, "backup.dump_kind", "The dump is not a {kind} file: {detail}")
	errDumpCut     = msg.Define(http.StatusUnprocessableEntity, "backup.dump_cut", "The dump stopped before its end: {detail}")
	errCopyFailed  = msg.Define(http.StatusUnprocessableEntity, "restore.copy_failed", "Copying the backup into the database failed with exit code {code}: {detail}")
	errCopyShort   = msg.Define(http.StatusUnprocessableEntity, "restore.copy_short", "The backup did not arrive whole in the database: {sent} bytes were sent, and it reports {got}.")
	errLoadFailed  = msg.Define(http.StatusUnprocessableEntity, "restore.load_failed", "Loading the backup failed with exit code {code}: {detail}")
	errNotRedis    = msg.Define(http.StatusUnprocessableEntity, "restore.not_redis", "The backup is not a Redis file.")
	errDamaged     = msg.Define(http.StatusUnprocessableEntity, "restore.damaged", "The backup is damaged: {detail}")
	errCutShort    = msg.Define(http.StatusUnprocessableEntity, "restore.cut_short", "The backup is damaged: it ends in the middle of {file}.")
	errLinkOut     = msg.Define(http.StatusUnprocessableEntity, "restore.link_out", "The backup links {file} to {target}, outside its volume.")
	errThroughLink = msg.Define(http.StatusUnprocessableEntity, "restore.through_link", "The backup writes through the symbolic link {link}.")
)

// Kinds lists the kinds of backup a database can have.
func Kinds() []string { return []string{"postgres", "mariadb", "redis"} }

// KindVolumes is the backup of an app's volumes, as one tar archive.
const KindVolumes = "volumes"

// Ext is the extension of the files a kind makes.
func Ext(k string) (string, bool) {
	if k == KindVolumes {
		return "tar", true
	}
	kd, ok := kinds[k]
	return kd.ext, ok
}

// LoadsInPlace reports whether a kind restores into the running database.
// Redis does not: its files are replaced while it is stopped.
func LoadsInPlace(k string) bool { return kinds[k].load != nil }

// Dump writes a database's dump to w.
func Dump(ctx context.Context, exec Exec, k string, w io.Writer) error {
	kd, ok := kinds[k]
	if !ok {
		return fmt.Errorf("no backup for %q", k)
	}
	check := &ends{max: 256}
	var stderr tail
	code, err := exec(ctx, kd.dump, nil, io.MultiWriter(w, check), &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return errDumpFailed.Err("code", code, "detail", stderr.String())
	}
	if kd.header != "" && !bytes.HasPrefix(check.head, []byte(kd.header)) {
		return errDumpKind.Err("kind", k, "detail", stderr.String())
	}
	if kd.trailer != "" && !bytes.Contains(check.tail, []byte(kd.trailer)) {
		return errDumpCut.Err("detail", stderr.String())
	}
	return nil
}

// Load replaces a running database with a dump. The dump is copied into
// the container and checked before the database is touched.
func Load(ctx context.Context, exec Exec, k string, dump io.Reader) error {
	kd, ok := kinds[k]
	if !ok || kd.load == nil {
		return fmt.Errorf("%q does not load in place", k)
	}
	sum := sha256.New()
	counted := &counter{}
	var out, stderr tail
	code, err := exec(ctx, []string{"sh", "-c", "umask 077; cat > " + restoreFile + " && wc -c < " + restoreFile + " && sha256sum " + restoreFile},
		io.TeeReader(dump, io.MultiWriter(sum, counted)), &out, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return errCopyFailed.Err("code", code, "detail", stderr.String())
	}
	f := strings.Fields(out.String())
	if len(f) < 2 || f[0] != strconv.FormatInt(counted.n, 10) || f[1] != hex.EncodeToString(sum.Sum(nil)) {
		exec(context.WithoutCancel(ctx), []string{"rm", "-f", restoreFile}, nil, nil, nil)
		return errCopyShort.Err("sent", counted.n, "got", strings.TrimSpace(out.String()))
	}
	stderr = tail{}
	code, err = exec(ctx, kd.load, nil, nil, &stderr)
	if err != nil {
		return err
	}
	if code != 0 {
		return errLoadFailed.Err("code", code, "detail", stderr.String())
	}
	return nil
}

// RestoreRedis puts an RDB file in place in a stopped Redis's volume. With
// append-only files on, Redis ignores a plain dump.rdb and starts empty;
// the dump has to become the base of a new append-only directory.
func RestoreRedis(vol *os.Root, rdb io.Reader) error {
	const (
		dir  = "appendonlydir"
		base = "appendonly.aof.1.base.rdb"
		incr = "appendonly.aof.1.incr.aof"
	)
	st, err := vol.Stat(".")
	if err != nil {
		return err
	}
	uid, gid := owner(st)
	fresh, old := dir+".zelie-new", dir+".zelie-old"
	vol.RemoveAll(fresh)
	if err := vol.Mkdir(fresh, 0o700); err != nil {
		return err
	}
	check := &ends{max: 16}
	if err := writeFile(vol, fresh+"/"+base, io.TeeReader(rdb, check)); err != nil {
		return err
	}
	if !bytes.HasPrefix(check.head, []byte("REDIS")) {
		vol.RemoveAll(fresh)
		return errNotRedis.Err()
	}
	manifest := "file " + base + " seq 1 type b\nfile " + incr + " seq 1 type i\n"
	if err := writeFile(vol, fresh+"/"+incr, strings.NewReader("")); err != nil {
		return err
	}
	if err := writeFile(vol, fresh+"/appendonly.aof.manifest", strings.NewReader(manifest)); err != nil {
		return err
	}
	for _, p := range []string{fresh, fresh + "/" + base, fresh + "/" + incr, fresh + "/appendonly.aof.manifest"} {
		if err := vol.Lchown(p, uid, gid); err != nil {
			return err
		}
	}
	vol.RemoveAll(old)
	if err := vol.Rename(dir, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := vol.Rename(fresh, dir); err != nil {
		vol.Rename(old, dir)
		return err
	}
	// A plain dump.rdb would only confuse whoever looks later.
	vol.Remove("dump.rdb")
	return vol.RemoveAll(old)
}

func owner(fi fs.FileInfo) (uid, gid int) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1 // left as they are
}

func writeFile(root *os.Root, name string, r io.Reader) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// ends keeps the first and last bytes of what goes through it.
type ends struct {
	max        int
	head, tail []byte
}

func (e *ends) Write(b []byte) (int, error) {
	if len(e.head) < e.max {
		e.head = append(e.head, b[:min(len(b), e.max-len(e.head))]...)
	}
	e.tail = append(e.tail, b[max(0, len(b)-e.max):]...)
	if len(e.tail) > e.max {
		e.tail = e.tail[len(e.tail)-e.max:]
	}
	return len(b), nil
}

type counter struct{ n int64 }

func (c *counter) Write(b []byte) (int, error) { c.n += int64(len(b)); return len(b), nil }

// tail keeps the last few kilobytes of a command's output for its error.
type tail struct{ ends }

func (t *tail) Write(b []byte) (int, error) {
	t.max = 4096
	return t.ends.Write(b)
}

func (t *tail) String() string { return strings.TrimSpace(string(t.ends.tail)) }
