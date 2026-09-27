//go:build integration

// These tests start real database containers. Run them with
// hack/vm-test.sh.

package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

func connect(t *testing.T) *engine.Engine {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(engine.DefaultPaths.Socket); err != nil {
		t.Skip("Zelie's containerd is not running")
	}
	e, err := engine.Connect(context.Background(), engine.DefaultPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// start runs a database the way the panel does and waits until sql answers.
func start(t *testing.T, e *engine.Engine, s engine.Spec, ready []string) Exec {
	t.Helper()
	ctx := context.Background()
	e.Remove(ctx, s.ID)
	s.MemoryBytes, s.CPUs, s.Pids = 512<<20, 1, 256
	if err := e.Run(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Remove(context.Background(), s.ID) })
	exec := func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
		return e.Exec(ctx, s.ID, args, stdin, stdout, stderr)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		// Postgres answers on its socket while its first-start script
		// still runs, then restarts; ask twice in a row.
		code, _ := exec(ctx, ready, nil, nil, nil)
		if code == 0 {
			time.Sleep(3 * time.Second)
			if code, _ = exec(ctx, ready, nil, nil, nil); code == 0 {
				return exec
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became ready", s.ID)
		}
		time.Sleep(time.Second)
	}
}

func sh(t *testing.T, exec Exec, script string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	code, err := exec(context.Background(), []string{"sh", "-c", script}, nil, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("%s: exit %d, %v: %s", script, code, err, errOut.String())
	}
	return strings.TrimSpace(out.String())
}

// backUp dumps the database and returns what puts that dump back.
func backUp(t *testing.T, exec Exec, kind string) (restore func()) {
	t.Helper()
	d := newDir(t)
	w, err := d.Create("db", kind)
	if err != nil {
		t.Fatal(err)
	}
	if err := Dump(context.Background(), exec, kind, w); err != nil {
		w.Abort()
		t.Fatal(err)
	}
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s backup: %d bytes", kind, info.Bytes)
	return func() {
		t.Helper()
		r, err := d.Open("db", info.Name)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if err := Load(context.Background(), exec, kind, r); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgres(t *testing.T) {
	e := connect(t)
	exec := start(t, e, engine.Spec{ID: "it-backup-pg", Image: "postgres:18",
		Env: []string{"POSTGRES_USER=app", "POSTGRES_DB=app", "POSTGRES_PASSWORD=it-password"}},
		[]string{"psql", "-U", "app", "-d", "app", "-c", "select 1"})
	sh(t, exec, `psql -X -U app -d app -v ON_ERROR_STOP=1 -c "create table t (x int, s text); insert into t select g, repeat('x', 100) from generate_series(1, 50000) g; create view v as select count(*) from t"`)
	restore := backUp(t, exec, "postgres")
	sh(t, exec, `psql -X -U app -d app -c "drop view v; delete from t; insert into t values (-1, 'after')"`)
	restore()
	if got := sh(t, exec, `psql -X -U app -d app -tAc "select count(*), min(x) from t; select * from v"`); got != "50000|1\n50000" {
		t.Errorf("after restore: %q", got)
	}
}

func TestMariaDB(t *testing.T) {
	e := connect(t)
	exec := start(t, e, engine.Spec{ID: "it-backup-my", Image: "mariadb:11.8",
		Env:  []string{"MARIADB_USER=app", "MARIADB_DATABASE=app", "MARIADB_ROOT_HOST=localhost", "MARIADB_PASSWORD=it-password"},
		Args: []string{"sh", "-c", `export MARIADB_ROOT_PASSWORD="$(head -c 24 /dev/urandom | base64)"; exec docker-entrypoint.sh mariadbd`}},
		[]string{"sh", "-c", `MYSQL_PWD="$MARIADB_PASSWORD" mariadb -u app app -e "select 1"`})
	my := `MYSQL_PWD="$MARIADB_PASSWORD" mariadb -u app app -N -e `
	sh(t, exec, my+`"create table t (x int); insert into t values (1),(2),(3); create table log (x int);
create trigger tr after insert on t for each row insert into log values (new.x);
create procedure p() select count(*) from t;"`)
	restore := backUp(t, exec, "mariadb")
	sh(t, exec, my+`"drop procedure p; drop trigger tr; delete from t"`)
	restore()
	sh(t, exec, my+`"insert into t values (4)"`)
	// The rows came back with the procedure, and the trigger too: it
	// logged the new row. The first three went in before it existed.
	if got := sh(t, exec, my+`"call p(); select count(*) from log"`); got != "4\n1" {
		t.Errorf("after restore: %q", got)
	}
}

func TestRedis(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	vol := "it-backup-redis"
	e.RemoveVolume(ctx, vol)
	if err := e.CreateVolume(vol); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.RemoveVolume(context.Background(), vol) })
	spec := engine.Spec{ID: "it-backup-redis", Image: "redis:8", Env: []string{"REDIS_PASSWORD=it-password"},
		Args:    []string{"sh", "-c", `exec docker-entrypoint.sh redis-server --appendonly yes --requirepass "$REDIS_PASSWORD"`},
		Volumes: []engine.VolumeMount{{Name: vol, Target: "/data"}}}
	ready := []string{"sh", "-c", `REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli ping | grep -q PONG`}
	exec := start(t, e, spec, ready)
	cli := `REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --no-auth-warning `
	sh(t, exec, cli+"set k before")
	d := newDir(t)
	w, _ := d.Create("cache", "rdb")
	if err := Dump(ctx, exec, "redis", w); err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	sh(t, exec, cli+"set k after")

	// Refused while Redis runs.
	if _, err := e.OpenVolume(ctx, vol); err == nil {
		t.Fatal("opened a volume in use")
	}
	if err := e.Stop(ctx, spec.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	root, err := e.OpenVolume(ctx, vol)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := d.Open("cache", info.Name)
	err = RestoreRedis(root, r)
	r.Close()
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	exec = start(t, e, spec, ready)
	if got := sh(t, exec, cli+"get k"); got != "before" {
		t.Errorf("after restore: %q", got)
	}
	// Owned by redis inside, as Redis made them.
	if got := sh(t, exec, "stat -c %U /data/appendonlydir/appendonly.aof.1.base.rdb"); got != "redis" {
		t.Errorf("owner %q", got)
	}
}

// TestVolumes backs up a running app's volume, changes it, and puts the
// backup back while the app is stopped. The app sees its files as they
// were, owned by the same users inside the container.
func TestVolumes(t *testing.T) {
	e := connect(t)
	ctx := context.Background()
	vol := "it-backup-files"
	e.RemoveVolume(ctx, vol)
	if err := e.CreateVolume(vol); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.RemoveVolume(context.Background(), vol) })
	spec := engine.Spec{ID: "it-backup-files", Image: "alpine:3.22", Args: []string{"sleep", "infinity"},
		Volumes: []engine.VolumeMount{{Name: vol, Target: "/srv/data"}}}
	exec := start(t, e, spec, []string{"true"})
	sh(t, exec, `cd /srv/data && mkdir -p world/region && head -c 3000000 /dev/urandom > world/region/r.0.0.mca &&
echo level > world/level.dat && ln -s world/level.dat current && ln world/level.dat level-link &&
chown -R 1000:1000 world && chown -h 1000:1000 current && chmod 640 world/level.dat`)
	before := sh(t, exec, `cd /srv/data && find . | sort | xargs stat -c '%n %U:%G %a %s %F' && sha256sum world/region/r.0.0.mca`)

	// Copied while it runs.
	root, err := e.ReadVolume(vol)
	if err != nil {
		t.Fatal(err)
	}
	d := newDir(t)
	w, _ := d.Create("files", "tar")
	st, err := WriteTar(ctx, w, []Volume{{Dir: "srv/data", Root: root}})
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("volume backup: %d files, %d bytes in, %d bytes out, %d changed", st.Files, st.Bytes, info.Bytes, st.Changed)

	sh(t, exec, `cd /srv/data && echo changed > world/level.dat && rm -r world/region && echo new > new.txt`)
	if _, err := e.OpenVolume(ctx, vol); err == nil {
		t.Fatal("opened a volume in use for a restore")
	}
	if err := e.Stop(ctx, spec.ID, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	root, err = e.OpenVolume(ctx, vol)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := d.Open("files", info.Name)
	restored, err := RestoreTar(ctx, r, []Volume{{Dir: "srv/data", Root: root}})
	r.Close()
	root.Close()
	if err != nil || len(restored) != 1 {
		t.Fatalf("restore: %v %v", restored, err)
	}
	exec = start(t, e, spec, []string{"true"})
	after := sh(t, exec, `cd /srv/data && find . | sort | xargs stat -c '%n %U:%G %a %s %F' && sha256sum world/region/r.0.0.mca`)
	if after != before {
		t.Errorf("after restore:\n%s\nwant:\n%s", after, before)
	}
}
