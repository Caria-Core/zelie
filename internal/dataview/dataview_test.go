package dataview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/msg"
)

// fake answers the catalog query with catalog, and any other with rows. It
// keeps what it was sent.
type fake struct {
	catalog, rows []string
	stderr        string
	code          uint32
	sent          []string
}

func (f *fake) exec(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	b, _ := io.ReadAll(stdin)
	f.sent = append(f.sent, string(b))
	out := f.rows
	if strings.Contains(string(b), "'t', ") {
		out = f.catalog
	} else if f.code != 0 {
		io.WriteString(stderr, f.stderr)
		return f.code, nil
	}
	for _, l := range out {
		fmt.Fprintln(stdout, l)
	}
	return 0, nil
}

var pgCatalog = []string{
	`["t", "public", "orders", false, 3, 16384]`,
	`["t", "public", "we\"ird", false, -1, 8192]`,
	`["t", "extra", "recent", true, 0, 0]`,
	`["c", "public", "orders", "id", "integer", false, true, false]`,
	`["c", "public", "orders", "note", "text", true, false, false]`,
	`["c", "public", "orders", "blob", "bytea", true, false, true]`,
	`["c", "public", "we\"ird", "co\"l", "text", true, false, false]`,
	`["c", "extra", "recent", "id", "integer", true, false, false]`,
}

var myCatalog = []string{
	`["t", "app", "users", 0, 3, 32768]`,
	`["t", "app", "active_users", 1, -1, 0]`,
	`["c", "app", "users", "id", "int(11)", 0, 1, 0]`,
	`["c", "app", "users", "email", "varchar(191)", 1, 0, 0]`,
	`["c", "app", "users", "avatar", "blob", 1, 0, 1]`,
}

func TestTables(t *testing.T) {
	f := &fake{catalog: pgCatalog}
	tables, err := Tables(context.Background(), f.exec, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 3 || tables[0].Name != "orders" || tables[0].Rows != 3 || len(tables[0].Columns) != 3 ||
		!tables[0].Columns[0].Key || !tables[0].Columns[2].Binary || !tables[2].View || tables[1].Rows != -1 {
		t.Fatalf("%+v", tables)
	}
	if !strings.HasPrefix(f.sent[0], "BEGIN READ ONLY;\nSET LOCAL statement_timeout = 5000;") || !strings.HasSuffix(f.sent[0], "COMMIT;\n") {
		t.Errorf("not read only:\n%s", f.sent[0])
	}

	f = &fake{catalog: myCatalog}
	tables, err = Tables(context.Background(), f.exec, "mariadb")
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 2 || !tables[1].View || !tables[0].Columns[0].Key || tables[0].Columns[1].Key || !tables[0].Columns[2].Binary {
		t.Fatalf("%+v", tables)
	}
	if !strings.Contains(f.sent[0], "NO_BACKSLASH_ESCAPES") || !strings.Contains(f.sent[0], "max_statement_time = 5;") ||
		!strings.Contains(f.sent[0], "START TRANSACTION READ ONLY;") {
		t.Errorf("not read only:\n%s", f.sent[0])
	}
}

func query(t *testing.T, engine string, catalog []string, q Query) string {
	t.Helper()
	f := &fake{catalog: catalog}
	if _, err := Rows(context.Background(), f.exec, engine, q); err != nil {
		t.Fatal(err)
	}
	sql := f.sent[1]
	return sql[strings.Index(sql, "SELECT"):strings.LastIndex(sql, "COMMIT")]
}

func TestBuild(t *testing.T) {
	got := query(t, "postgres", pgCatalog, Query{Schema: "public", Table: "orders",
		Filters: []Filter{{Column: "note", Op: OpContains, Value: "50%_off!"}, {Column: "id", Op: OpGt, Value: "1' OR '1'='1"}},
		Sort:    "note", Desc: true, Offset: 100})
	want := `SELECT json_build_array(left("id"::text, 1001), left("note"::text, 1001), left("blob"::text, 1001)) FROM "public"."orders"` +
		` WHERE "note"::text ILIKE '%50!%!_off!!%' ESCAPE '!' AND "id" > '1'' OR ''1''=''1' ORDER BY "note" DESC LIMIT 51 OFFSET 100;` + "\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	// Names that need quoting come out quoted; with no key, no order.
	got = query(t, "postgres", pgCatalog, Query{Schema: "public", Table: `we"ird`, Filters: []Filter{{Column: `co"l`, Op: OpNull}}})
	if want := `SELECT json_build_array(left("co""l"::text, 1001)) FROM "public"."we""ird" WHERE "co""l" IS NULL LIMIT 51;` + "\n"; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	got = query(t, "mariadb", myCatalog, Query{Schema: "app", Table: "users", Search: `a\'b`, Filters: []Filter{{Column: "email", Op: OpNe, Value: "x"}}})
	want = "SELECT JSON_ARRAY(LEFT(CAST(`id` AS CHAR), 1001), LEFT(CAST(`email` AS CHAR), 1001), LEFT(CONCAT('0x', HEX(LEFT(`avatar`, 500))), 1001)) FROM `app`.`users`" +
		" WHERE (`email` <> 'x' OR `email` IS NULL) AND (CAST(`id` AS CHAR) LIKE '%a\\''b%' ESCAPE '!' OR CAST(`email` AS CHAR) LIKE '%a\\''b%' ESCAPE '!'" +
		" OR HEX(`avatar`) LIKE '%a\\''b%' ESCAPE '!') ORDER BY `id` LIMIT 51;\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}

	// A row opened on its own keeps longer values.
	got = query(t, "postgres", pgCatalog, Query{Schema: "public", Table: "orders", Full: true, Filters: []Filter{{Column: "id", Op: OpEq, Value: "7"}}})
	if !strings.Contains(got, `left("note"::text, 65537)`) || !strings.HasSuffix(got, `WHERE "id" = '7' ORDER BY "id" LIMIT 2;`+"\n") {
		t.Errorf("full: %s", got)
	}
}

// Nothing from the request reaches the SQL unless the catalog knows it.
func TestRefuses(t *testing.T) {
	for _, c := range []struct {
		q    Query
		code string
	}{
		{Query{Schema: "public", Table: `orders"; DROP TABLE orders; --`}, "data.no_table"},
		{Query{Schema: "pg_catalog", Table: "pg_authid"}, "data.no_table"},
		{Query{Schema: "public", Table: "orders", Sort: "id; DROP TABLE orders"}, "data.no_column"},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "nope", Op: OpEq}}}, "data.no_column"},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "id", Op: "; DELETE"}}}, "data.bad_filter"},
		{Query{Schema: "public", Table: "orders", Filters: []Filter{{Column: "id", Op: OpEq, Value: "a\x00b"}}}, "data.bad_value"},
	} {
		f := &fake{catalog: pgCatalog}
		_, err := Rows(context.Background(), f.exec, "postgres", c.q)
		var me *msg.Error
		if !errors.As(err, &me) || me.Code != c.code || len(f.sent) != 1 {
			t.Errorf("%+v: %v, sent %d", c.q, err, len(f.sent))
		}
	}
}

func TestPage(t *testing.T) {
	long := strings.Repeat("é", MaxCell+1)
	rows := []string{`["1", null, "\\x00ff"]`, `["2", "` + long + `", null]`}
	for i := 3; i <= PageSize+1; i++ {
		rows = append(rows, fmt.Sprintf(`["%d", "n", null]`, i))
	}
	f := &fake{catalog: pgCatalog, rows: rows}
	p, err := Rows(context.Background(), f.exec, "postgres", Query{Schema: "public", Table: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) != PageSize || !p.More || p.Rows[0][1] != nil || *p.Rows[0][2] != `\x00ff` || len(p.Cut) != 1 || p.Cut[0] != [2]int{1, 1} ||
		len([]rune(*p.Rows[1][1])) != MaxCell || strings.Join(p.Columns, ",") != "id,note,blob" {
		t.Fatalf("%d rows, more %v, cut %v", len(p.Rows), p.More, p.Cut)
	}
	f.rows = rows[:3]
	if p, _ = Rows(context.Background(), f.exec, "postgres", Query{Schema: "public", Table: "orders"}); p.More || len(p.Rows) != 3 {
		t.Errorf("last page: %d rows, more %v", len(p.Rows), p.More)
	}
}

func TestErrors(t *testing.T) {
	for _, c := range []struct {
		engine, stderr, code, text string
	}{
		{"postgres", "psql:<stdin>:4: ERROR:  canceling statement due to statement timeout\n", "data.timeout", "The query took longer than 5 seconds. Narrow it down with a filter."},
		{"mariadb", "ERROR 1969 (70100) at line 4: Query execution was interrupted (max_statement_time exceeded)\n", "data.timeout", ""},
		{"postgres", "psql:<stdin>:4: ERROR:  invalid input syntax for type integer: \"abc\"\nLINE 1: ...\n", "data.query_failed", "The database refused the query: invalid input syntax for type integer: \"abc\"\nLINE 1: ..."},
	} {
		cat := pgCatalog
		if c.engine == "mariadb" {
			cat = myCatalog
		}
		f := &fake{catalog: cat, code: 1, stderr: c.stderr}
		table := map[string]string{"postgres": "orders", "mariadb": "users"}[c.engine]
		schema := map[string]string{"postgres": "public", "mariadb": "app"}[c.engine]
		_, err := Rows(context.Background(), f.exec, c.engine, Query{Schema: schema, Table: table})
		var me *msg.Error
		if !errors.As(err, &me) || me.Code != c.code || (c.text != "" && me.Text != c.text) {
			t.Errorf("%s: %v", c.engine, err)
		}
	}
}

func TestExport(t *testing.T) {
	long := strings.Repeat("x", MaxFull+10)
	f := &fake{catalog: pgCatalog, rows: []string{`["1", "a,\"b\"\nc", null]`, `["2", "` + long + `", "\\x01"]`}}
	var out bytes.Buffer
	if err := Export(context.Background(), f.exec, "postgres", Query{Schema: "public", Table: "orders"}, &out); err != nil {
		t.Fatal(err)
	}
	want := "id,note,blob\n1,\"a,\"\"b\"\"\nc\",\n2," + long + ",\\x01\n"
	if out.String() != want {
		t.Errorf("got %q", out.String()[:80])
	}
	if !strings.Contains(f.sent[1], `SELECT json_build_array("id"::text, "note"::text, "blob"::text) FROM "public"."orders" ORDER BY "id";`) ||
		!strings.Contains(f.sent[1], "statement_timeout = 600000") {
		t.Errorf("export sql: %s", f.sent[1])
	}
}
