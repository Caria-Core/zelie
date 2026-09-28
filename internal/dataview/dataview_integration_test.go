//go:build integration

// These tests start real database containers. Run them with
// hack/vm-test.sh.

package dataview

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

func cells(p Page) string {
	var rows []string
	for _, r := range p.Rows {
		var c []string
		for _, v := range r {
			if v == nil {
				c = append(c, "NULL")
			} else {
				c = append(c, *v)
			}
		}
		rows = append(rows, strings.Join(c, ","))
	}
	return strings.Join(rows, "|")
}

func TestPostgres(t *testing.T) {
	e := connect(t)
	exec := start(t, e, engine.Spec{ID: "it-data-pg", Image: "postgres:18",
		Env: []string{"POSTGRES_USER=app", "POSTGRES_DB=app", "POSTGRES_PASSWORD=it-password"}},
		[]string{"psql", "-U", "app", "-d", "app", "-c", "select 1"})
	sh(t, exec, `psql -X -U app -d app -v ON_ERROR_STOP=1 <<'SQL'
create table orders (id int primary key, note text, paid numeric, raw bytea, at timestamptz);
insert into orders values (1, 'it''s 50% off', 9.5, '\x00ff', '2026-01-01'), (2, null, 12, null, null), (3, 'back\slash', 1, null, null);
insert into orders select g, repeat('x', 2000), g, null, null from generate_series(4, 120) g;
create schema "we""ird";
create table "we""ird"."ta""ble" ("co""l" text);
insert into "we""ird"."ta""ble" values ('quoted');
create view slow as select pg_sleep(10)::text as s;
create table log (x int);
create function sneak() returns int language sql as 'insert into log values (1) returning 1';
create view sneaky as select sneak() as x;
analyze;
SQL`)
	ctx := context.Background()
	tables, err := Tables(ctx, exec, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	o, _ := find(tables, "public", "orders")
	if o.Rows != 120 || len(o.Columns) != 5 || !o.Columns[0].Key || !o.Columns[3].Binary || o.Columns[2].Type != "numeric" {
		t.Errorf("orders: %+v", o)
	}
	if _, ok := find(tables, `we"ird`, `ta"ble`); !ok {
		t.Errorf("tables: %+v", tables)
	}

	p, err := Rows(ctx, exec, "postgres", Query{Schema: "public", Table: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != PageSize || !p.More || cells(Page{Rows: p.Rows[:3]}) != `1,it's 50% off,9.5,\x00ff,2026-01-01 00:00:00+00|2,NULL,12,NULL,NULL|3,back\slash,1,NULL,NULL` || len(p.Cut) != PageSize-3 {
		t.Errorf("page: %s, more %v, cut %d", cells(Page{Rows: p.Rows[:3]}), p.More, len(p.Cut))
	}
	for _, c := range []struct {
		q    Query
		want string
	}{
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "note", Op: OpContains, Value: "50%"}}}, `1,it's 50% off,9.5,\x00ff,2026-01-01 00:00:00+00`},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "note", Op: OpContains, Value: `k\s`}}}, `3,back\slash,1,NULL,NULL`},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "note", Op: OpNull}}}, `2,NULL,12,NULL,NULL`},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "id", Op: OpLt, Value: "3"}, {Column: "note", Op: OpNe, Value: "x"}}, Sort: "id", Desc: true}, `2,NULL,12,NULL,NULL|1,it's 50% off,9.5,\x00ff,2026-01-01 00:00:00+00`},
		{Query{Schema: "public", Table: "orders", Search: "IT'S"}, `1,it's 50% off,9.5,\x00ff,2026-01-01 00:00:00+00`},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "note", Op: OpEq, Value: "' OR 1=1 --"}}}, ``},
		{Query{Schema: `we"ird`, Table: `ta"ble`}, `quoted`},
	} {
		p, err := Rows(ctx, exec, "postgres", c.q)
		if err != nil || cells(p) != c.want {
			t.Errorf("%+v: %q %v", c.q, cells(p), err)
		}
	}
	p, err = Rows(ctx, exec, "postgres", Query{Schema: "public", Table: "orders", Full: true, Filters: []Filter{{Column: "id", Op: OpEq, Value: "4"}}})
	if err != nil || len(p.Rows) != 1 || len(*p.Rows[0][1]) != 2000 || p.Cut != nil {
		t.Errorf("full row: %v %v", err, p.Cut)
	}
	if _, err := Rows(ctx, exec, "postgres", Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "id", Op: OpEq, Value: "abc"}}}); !isCode(err, "data.query_failed") ||
		!strings.Contains(err.Error(), "invalid input syntax for type integer") {
		t.Errorf("bad value: %v", err)
	}

	// Slow reads stop; a view that writes cannot.
	began := time.Now()
	if _, err := Rows(ctx, exec, "postgres", Query{Schema: "public", Table: "slow"}); !isCode(err, "data.timeout") || time.Since(began) > 9*time.Second {
		t.Errorf("slow: %v after %v", err, time.Since(began))
	}
	if _, err := Rows(ctx, exec, "postgres", Query{Schema: "public", Table: "sneaky"}); !isCode(err, "data.query_failed") || !strings.Contains(err.Error(), "read-only transaction") {
		t.Errorf("sneaky: %v", err)
	}
	if got := sh(t, exec, `psql -X -U app -d app -tAc "select count(*) from log"`); got != "0" {
		t.Errorf("the view wrote %s rows", got)
	}

	var csv bytes.Buffer
	if err := Export(ctx, exec, "postgres", Query{Schema: "public", Table: "orders"}, &csv); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(csv.String()), "\n")
	if len(lines) != 121 || lines[0] != "id,note,paid,raw,at" || lines[1] != `1,it's 50% off,9.5,\x00ff,2026-01-01 00:00:00+00` || len(lines[4]) < 2000 {
		t.Errorf("export: %d lines, %q", len(lines), lines[:2])
	}
}

func TestMariaDB(t *testing.T) {
	e := connect(t)
	exec := start(t, e, engine.Spec{ID: "it-data-maria", Image: "mariadb:11.8",
		Env: []string{"MARIADB_USER=app", "MARIADB_DATABASE=app", "MARIADB_PASSWORD=it-password", "MARIADB_RANDOM_ROOT_PASSWORD=1"}},
		[]string{"sh", "-c", `MYSQL_PWD="$MARIADB_PASSWORD" mariadb -u app app -e "select 1"`})
	sh(t, exec, `MYSQL_PWD="$MARIADB_PASSWORD" mariadb -u app app <<'SQL'
create table users (id int primary key, email varchar(191), avatar blob, note text);
insert into users values (1, 'a@example.com', x'00ff', 'back\\slash'), (2, 'b@example.com', null, 'it''s'), (3, null, null, 'tab	and
newline');
create table `+"`we``ird`"+` (`+"`c``ol`"+` text);
insert into `+"`we``ird`"+` values ('quoted');
create view slow as select sleep(10) as s;
SQL`)
	ctx := context.Background()
	tables, err := Tables(ctx, exec, "mariadb")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := find(tables, "app", "users")
	if len(u.Columns) != 4 || !u.Columns[0].Key || !u.Columns[2].Binary {
		t.Errorf("users: %+v", u)
	}
	p, err := Rows(ctx, exec, "mariadb", Query{Schema: "app", Table: "users"})
	if want := `1,a@example.com,0x00FF,back\slash|2,b@example.com,NULL,it's|3,NULL,NULL,tab` + "\tand\nnewline"; err != nil || cells(p) != want {
		t.Errorf("page: %q %v", cells(p), err)
	}
	for _, c := range []struct {
		q    Query
		want string
	}{
		{Query{Schema: "app", Table: "users", Filters: []Filter{{Column: "note", Op: OpContains, Value: `k\s`}}}, `1,a@example.com,0x00FF,back\slash`},
		{Query{Schema: "app", Table: "users", Filters: []Filter{{Column: "note", Op: OpEq, Value: `it's`}}}, `2,b@example.com,NULL,it's`},
		{Query{Schema: "app", Table: "users", Search: "B@EXAMPLE"}, `2,b@example.com,NULL,it's`},
		{Query{Schema: "app", Table: "users", Filters: []Filter{{Column: "email", Op: OpEq, Value: `\' OR 1=1 -- `}}}, ``},
		{Query{Schema: "app", Table: "we`ird"}, `quoted`},
	} {
		p, err := Rows(ctx, exec, "mariadb", c.q)
		if err != nil || cells(p) != c.want {
			t.Errorf("%+v: %q %v", c.q, cells(p), err)
		}
	}
	began := time.Now()
	if _, err := Rows(ctx, exec, "mariadb", Query{Schema: "app", Table: "slow"}); !isCode(err, "data.timeout") || time.Since(began) > 9*time.Second {
		t.Errorf("slow: %v after %v", err, time.Since(began))
	}
}

func TestRedis(t *testing.T) {
	e := connect(t)
	exec := start(t, e, engine.Spec{ID: "it-data-redis", Image: "redis:8", Env: []string{"REDIS_PASSWORD=it-password"},
		Args: []string{"sh", "-c", `exec docker-entrypoint.sh redis-server --requirepass "$REDIS_PASSWORD"`}},
		[]string{"sh", "-c", `REDISCLI_AUTH="$REDIS_PASSWORD" redis-cli --no-auth-warning ping | grep -q PONG`})
	sh(t, exec, `export REDISCLI_AUTH="$REDIS_PASSWORD"
redis-cli --no-auth-warning set "user:1" "héllo
wörld" >/dev/null
printf '\000\377' | redis-cli --no-auth-warning -x set "user:bin" >/dev/null
redis-cli --no-auth-warning hset "user:2" name Ada role admin >/dev/null
redis-cli --no-auth-warning expire "user:2" 300 >/dev/null
redis-cli --no-auth-warning rpush queue a b c >/dev/null
redis-cli --no-auth-warning zadd rank 1.5 x 2 y >/dev/null
redis-cli --no-auth-warning set "say:\"hi\"" '{"user":"efe","path":"C:\\x"}' >/dev/null`)
	ctx := context.Background()
	p, err := Keys(ctx, exec, "user:*", "0")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Key{}
	for p.Cursor != "0" || len(byName) == 0 {
		for _, k := range p.Keys {
			byName[k.Name] = k
		}
		if p.Cursor == "0" {
			break
		}
		if p, err = Keys(ctx, exec, "user:*", p.Cursor); err != nil {
			t.Fatal(err)
		}
	}
	if len(byName) != 3 || byName["user:2"].Type != "hash" || byName["user:2"].TTL <= 0 || byName["user:1"].TTL != -1 {
		t.Fatalf("keys: %+v", byName)
	}
	bin := byName["user:bin"]
	v, err := KeyValue(ctx, exec, byName["user:1"].ID)
	if err != nil || *v.Rows[0][0] != "héllo\nwörld" {
		t.Errorf("user:1: %+v %v", v, err)
	}
	if v, err = KeyValue(ctx, exec, bin.ID); err != nil || *v.Rows[0][0] != "0x00ff" || len(v.Binary) != 1 {
		t.Errorf("binary: %+v %v", v, err)
	}
	if v, err = KeyValue(ctx, exec, byName["user:2"].ID); err != nil || v.Size != 2 || len(v.Rows) != 2 {
		t.Errorf("hash: %+v %v", v, err)
	}
	if v, err = KeyValue(ctx, exec, "rank"); err != nil || cells(Page{Rows: v.Rows}) != "x,1.5|y,2" {
		t.Errorf("zset: %q %v", cells(Page{Rows: v.Rows}), err)
	}
	// redis-cli leaves quotes in values unescaped in its JSON.
	quoted, err := Keys(ctx, exec, "say:*", "0")
	if err != nil || len(quoted.Keys) != 1 || quoted.Keys[0].Name != `say:"hi"` {
		t.Fatalf("quoted key: %+v %v", quoted, err)
	}
	if v, err = KeyValue(ctx, exec, quoted.Keys[0].ID); err != nil || *v.Rows[0][0] != `{"user":"efe","path":"C:\\x"}` {
		t.Errorf("quoted value: %+v %v", v, err)
	}
	if v, err = KeyValue(ctx, exec, "queue"); err != nil || v.Size != 3 {
		t.Errorf("list: %+v %v", v, err)
	}
}
