// Package dataview reads what a database holds, for the panel's data
// viewer. It never writes: queries are built here from a typed request,
// names are checked against the database's own catalog, and every query
// runs in a read-only transaction with a time limit, through the client
// that ships in the database's image.
package dataview

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Caria-Core/zelie/internal/msg"
)

// Exec runs a command in the database's container, with the container's
// environment, which holds the database's password.
type Exec func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error)

// How long a query may take: a page is meant to be quick, and a slow one
// should be narrowed with a filter rather than hold the database.
const (
	PageTimeout   = 5 * time.Second
	ExportTimeout = 10 * time.Minute
)

// PageSize is how many rows a page holds.
const PageSize = 50

// A value longer than this is cut in a page; a row asked for in full keeps
// up to MaxFull characters of each.
const (
	MaxCell = 1000
	MaxFull = 64 << 10
)

var (
	errUnknownTable  = msg.Define(http.StatusNotFound, "data.no_table", "There is no table named {table}.")
	errUnknownColumn = msg.Define(http.StatusBadRequest, "data.no_column", "The table {table} has no column named {column}.")
	errBadOp         = msg.Define(http.StatusBadRequest, "data.bad_filter", "{op} is not a filter.")
	errBadValue      = msg.Define(http.StatusBadRequest, "data.bad_value", "A filter value cannot hold a zero byte.")
	errTimeout       = msg.Define(http.StatusUnprocessableEntity, "data.timeout", "The query took longer than {seconds} seconds. Narrow it down with a filter.")
	errQuery         = msg.Define(http.StatusUnprocessableEntity, "data.query_failed", "The database refused the query: {detail}")
)

// Column describes one column of a table.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	// Key is set for the columns of the primary key.
	Key bool `json:"key,omitempty"`
	// Binary columns are shown as hex.
	Binary bool `json:"binary,omitempty"`
}

// Table is a table or view. Rows is the database's own estimate, -1 when it
// has none yet; counting a large table exactly would take too long.
type Table struct {
	Schema  string   `json:"schema"`
	Name    string   `json:"name"`
	View    bool     `json:"view,omitempty"`
	Rows    int64    `json:"rows"`
	Bytes   int64    `json:"bytes"`
	Columns []Column `json:"columns"`
}

// Filter narrows the rows to those whose column matches.
type Filter struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  string `json:"value,omitempty"`
}

// The filters there are.
const (
	OpEq       = "eq"
	OpNe       = "ne"
	OpContains = "contains"
	OpStarts   = "starts"
	OpGt       = "gt"
	OpLt       = "lt"
	OpNull     = "null"
	OpNotNull  = "notnull"
)

// Query asks for a page of a table's rows.
type Query struct {
	Schema  string   `json:"schema"`
	Table   string   `json:"table"`
	Filters []Filter `json:"filters,omitempty"`
	// Search looks for text in every column.
	Search string `json:"search,omitempty"`
	Sort   string `json:"sort,omitempty"`
	Desc   bool   `json:"desc,omitempty"`
	Offset int    `json:"offset,omitempty"`
	// Full keeps each value up to MaxFull characters, for one row opened
	// on its own.
	Full bool `json:"full,omitempty"`
}

// Page is what a query found.
type Page struct {
	Columns []string    `json:"columns"`
	Rows    [][]*string `json:"rows"`
	// Cut lists the values that were cut short, as row and column.
	Cut [][2]int `json:"cut,omitempty"`
	// More says there are rows after this page.
	More bool `json:"more"`
}

// dialect is what differs between Postgres and MariaDB.
type dialect struct {
	client  []string
	prelude func(timeout time.Duration) string
	end     string
	catalog string
	quote   func(name string) string
	// text is a column as text, cut to max characters, or whole with max 0.
	text func(c Column, max int) string
	row  func(values []string) string
	// like matches a column's text against a pattern.
	like func(c Column) string
	// timedOut recognizes the database's own words for a query cut short.
	timedOut string
}

var dialects = map[string]dialect{
	"postgres": {
		client: []string{"psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-U", "app", "-d", "app"},
		prelude: func(t time.Duration) string {
			return fmt.Sprintf("BEGIN READ ONLY;\nSET LOCAL statement_timeout = %d;\nSET LOCAL standard_conforming_strings = on;\n", t.Milliseconds())
		},
		end: "COMMIT;\n",
		catalog: `SELECT json_build_array('t', n.nspname, c.relname, c.relkind IN ('v', 'm'), c.reltuples::bigint, pg_total_relation_size(c.oid))
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p', 'v', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'
ORDER BY n.nspname, c.relname;
SELECT json_build_array('c', n.nspname, c.relname, a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
	COALESCE(a.attnum = ANY (i.indkey), false), a.atttypid = 'bytea'::regtype)
FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_index i ON i.indrelid = c.oid AND i.indisprimary
WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'p', 'v', 'm')
	AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_%'
ORDER BY n.nspname, c.relname, a.attnum;
`,
		quote: pgIdent,
		text: func(c Column, max int) string {
			if max == 0 {
				return pgIdent(c.Name) + "::text"
			}
			return fmt.Sprintf("left(%s::text, %d)", pgIdent(c.Name), max+1)
		},
		row:      func(v []string) string { return "json_build_array(" + strings.Join(v, ", ") + ")" },
		like:     func(c Column) string { return pgIdent(c.Name) + "::text ILIKE " },
		timedOut: "statement timeout",
	},
	"mariadb": {
		client: []string{"sh", "-c", `MYSQL_PWD="$MARIADB_PASSWORD" exec mariadb -u app -N -B -r app`},
		prelude: func(t time.Duration) string {
			// Without backslash escapes a quoted value is only ever closed
			// by a doubled quote, as in Postgres.
			return fmt.Sprintf("SET SESSION sql_mode = CONCAT(@@sql_mode, ',NO_BACKSLASH_ESCAPES');\nSET SESSION max_statement_time = %g;\nSTART TRANSACTION READ ONLY;\n", t.Seconds())
		},
		end: "COMMIT;\n",
		catalog: `SELECT JSON_ARRAY('t', TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE = 'VIEW', COALESCE(TABLE_ROWS, -1), COALESCE(DATA_LENGTH + INDEX_LENGTH, 0))
FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME;
SELECT JSON_ARRAY('c', TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE = 'YES', COLUMN_KEY = 'PRI',
	DATA_TYPE IN ('binary', 'varbinary', 'tinyblob', 'blob', 'mediumblob', 'longblob', 'geometry', 'point', 'linestring', 'polygon'))
FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION;
`,
		quote: myIdent,
		text: func(c Column, max int) string {
			v := "CAST(" + myIdent(c.Name) + " AS CHAR)"
			if c.Binary {
				v = "CONCAT('0x', HEX(" + myIdent(c.Name) + "))"
				if max > 0 {
					v = fmt.Sprintf("CONCAT('0x', HEX(LEFT(%s, %d)))", myIdent(c.Name), max/2)
				}
			}
			if max == 0 {
				return v
			}
			return fmt.Sprintf("LEFT(%s, %d)", v, max+1)
		},
		row: func(v []string) string { return "JSON_ARRAY(" + strings.Join(v, ", ") + ")" },
		like: func(c Column) string {
			if c.Binary {
				return "HEX(" + myIdent(c.Name) + ") LIKE "
			}
			return "CAST(" + myIdent(c.Name) + " AS CHAR) LIKE "
		},
		timedOut: "max_statement_time exceeded",
	},
}

// Supports reports whether the viewer reads tables of an engine.
func Supports(engineName string) bool {
	_, ok := dialects[engineName]
	return ok
}

func pgIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func myIdent(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

// literal quotes a value. Both engines are told to take a backslash as it
// is, so doubling the quote is all there is to it.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// pattern quotes a LIKE pattern with ! as its escape character.
func pattern(prefix, s, suffix string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return literal(prefix+r.Replace(s)+suffix) + " ESCAPE '!'"
}

// Tables lists the tables and views, with their columns.
func Tables(ctx context.Context, exec Exec, engineName string) ([]Table, error) {
	e, ok := dialects[engineName]
	if !ok {
		return nil, fmt.Errorf("no data viewer for %q", engineName)
	}
	tables := []Table{}
	byName := map[[2]string]int{}
	err := run(ctx, exec, e, PageTimeout, e.catalog, func(line []byte) error {
		var f []json.RawMessage
		if err := json.Unmarshal(line, &f); err != nil || len(f) < 6 {
			return fmt.Errorf("catalog line %q", line)
		}
		var kind, schema, name string
		json.Unmarshal(f[0], &kind)
		json.Unmarshal(f[1], &schema)
		json.Unmarshal(f[2], &name)
		switch kind {
		case "t":
			t := Table{Schema: schema, Name: name, View: flag(f[3]), Rows: number(f[4]), Bytes: number(f[5]), Columns: []Column{}}
			byName[[2]string{schema, name}] = len(tables)
			tables = append(tables, t)
		case "c":
			i, ok := byName[[2]string{schema, name}]
			if !ok || len(f) < 8 {
				return nil
			}
			c := Column{Nullable: flag(f[5]), Key: flag(f[6]), Binary: flag(f[7])}
			json.Unmarshal(f[3], &c.Name)
			json.Unmarshal(f[4], &c.Type)
			tables[i].Columns = append(tables[i].Columns, c)
		}
		return nil
	})
	return tables, err
}

// flag reads a JSON true or false, or MariaDB's 1 or 0.
func flag(b json.RawMessage) bool {
	s := string(b)
	return s == "true" || s == "1"
}

func number(b json.RawMessage) int64 {
	var n float64
	json.Unmarshal(b, &n)
	return int64(n)
}

func find(tables []Table, schema, name string) (Table, bool) {
	for _, t := range tables {
		if t.Schema == schema && t.Name == name {
			return t, true
		}
	}
	return Table{}, false
}

func column(t Table, name string) (Column, bool) {
	for _, c := range t.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return Column{}, false
}

// build turns a query into SQL. Every name in it comes from the catalog,
// never from the request; the request only picks among them.
func build(e dialect, t Table, q Query, limit, max int) (string, error) {
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = e.text(c, max)
	}
	var where []string
	for _, f := range q.Filters {
		c, ok := column(t, f.Column)
		if !ok {
			return "", errUnknownColumn.Err("table", t.Name, "column", f.Column)
		}
		if strings.ContainsRune(f.Value, 0) {
			return "", errBadValue.Err()
		}
		id := e.quote(c.Name)
		switch f.Op {
		case OpEq:
			where = append(where, id+" = "+literal(f.Value))
		case OpNe:
			where = append(where, "("+id+" <> "+literal(f.Value)+" OR "+id+" IS NULL)")
		case OpGt:
			where = append(where, id+" > "+literal(f.Value))
		case OpLt:
			where = append(where, id+" < "+literal(f.Value))
		case OpContains:
			where = append(where, e.like(c)+pattern("%", f.Value, "%"))
		case OpStarts:
			where = append(where, e.like(c)+pattern("", f.Value, "%"))
		case OpNull:
			where = append(where, id+" IS NULL")
		case OpNotNull:
			where = append(where, id+" IS NOT NULL")
		default:
			return "", errBadOp.Err("op", f.Op)
		}
	}
	if q.Search != "" {
		if strings.ContainsRune(q.Search, 0) {
			return "", errBadValue.Err()
		}
		var any []string
		for _, c := range t.Columns {
			any = append(any, e.like(c)+pattern("%", q.Search, "%"))
		}
		where = append(where, "("+strings.Join(any, " OR ")+")")
	}
	var b strings.Builder
	b.WriteString("SELECT " + e.row(cols) + " FROM " + e.quote(t.Schema) + "." + e.quote(t.Name))
	if len(where) > 0 {
		b.WriteString(" WHERE " + strings.Join(where, " AND "))
	}
	if q.Sort != "" {
		c, ok := column(t, q.Sort)
		if !ok {
			return "", errUnknownColumn.Err("table", t.Name, "column", q.Sort)
		}
		b.WriteString(" ORDER BY " + e.quote(c.Name))
		if q.Desc {
			b.WriteString(" DESC")
		}
	} else if keys := keyColumns(e, t); keys != "" {
		// A page has to come out the same each time it is asked for.
		b.WriteString(" ORDER BY " + keys)
	}
	if limit > 0 {
		fmt.Fprintf(&b, " LIMIT %d", limit)
		if q.Offset > 0 {
			fmt.Fprintf(&b, " OFFSET %d", q.Offset)
		}
	}
	b.WriteString(";\n")
	return b.String(), nil
}

func keyColumns(e dialect, t Table) string {
	var keys []string
	for _, c := range t.Columns {
		if c.Key {
			keys = append(keys, e.quote(c.Name))
		}
	}
	return strings.Join(keys, ", ")
}

// Rows reads one page of a table.
func Rows(ctx context.Context, exec Exec, engineName string, q Query) (Page, error) {
	e, ok := dialects[engineName]
	if !ok {
		return Page{}, fmt.Errorf("no data viewer for %q", engineName)
	}
	tables, err := Tables(ctx, exec, engineName)
	if err != nil {
		return Page{}, err
	}
	t, ok := find(tables, q.Schema, q.Table)
	if !ok {
		return Page{}, errUnknownTable.Err("table", q.Table)
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	max, limit := MaxCell, PageSize+1
	if q.Full {
		max, limit = MaxFull, 2
	}
	sql, err := build(e, t, q, limit, max)
	if err != nil {
		return Page{}, err
	}
	p := Page{Columns: make([]string, len(t.Columns)), Rows: [][]*string{}}
	for i, c := range t.Columns {
		p.Columns[i] = c.Name
	}
	err = run(ctx, exec, e, PageTimeout, sql, func(line []byte) error {
		if len(p.Rows) == limit-1 {
			p.More = true
			return nil
		}
		var row []*string
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("row %q: %w", line, err)
		}
		for i, v := range row {
			if v != nil && utf8.RuneCountInString(*v) > max {
				s := string([]rune(*v)[:max])
				row[i] = &s
				p.Cut = append(p.Cut, [2]int{len(p.Rows), i})
			}
		}
		p.Rows = append(p.Rows, row)
		return nil
	})
	return p, err
}

// Export writes every row the query finds as CSV, whole, with the column
// names first.
func Export(ctx context.Context, exec Exec, engineName string, q Query, w io.Writer) error {
	e, ok := dialects[engineName]
	if !ok {
		return fmt.Errorf("no data viewer for %q", engineName)
	}
	tables, err := Tables(ctx, exec, engineName)
	if err != nil {
		return err
	}
	t, ok := find(tables, q.Schema, q.Table)
	if !ok {
		return errUnknownTable.Err("table", q.Table)
	}
	sql, err := build(e, t, q, 0, 0)
	if err != nil {
		return err
	}
	cw := newCSV(w)
	head := make([]*string, len(t.Columns))
	for i, c := range t.Columns {
		head[i] = &c.Name
	}
	cw.write(head)
	err = run(ctx, exec, e, ExportTimeout, sql, func(line []byte) error {
		var row []*string
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("row: %w", err)
		}
		return cw.write(row)
	})
	if ferr := cw.flush(); err == nil {
		err = ferr
	}
	return err
}

// run sends sql to the database's client, inside a read-only transaction,
// and hands each line of output to each.
func run(ctx context.Context, exec Exec, e dialect, timeout time.Duration, sql string, each func([]byte) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	done := make(chan error, 1)
	go func() {
		code, err := exec(ctx, e.client, strings.NewReader(e.prelude(timeout)+sql+e.end), pw, &limited{w: &stderr, n: 4096})
		if err == nil && code != 0 {
			err = queryError(e, timeout, code, stderr.String())
		}
		pw.CloseWithError(err)
		done <- err
	}()
	sc := bufio.NewScanner(pr)
	// An exported row keeps its values whole.
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	var failed error
	for sc.Scan() {
		if failed != nil || len(sc.Bytes()) == 0 {
			continue
		}
		failed = each(sc.Bytes())
	}
	if err := sc.Err(); err != nil && failed == nil {
		failed = err
		pr.CloseWithError(err)
		io.Copy(io.Discard, pr)
	}
	if err := <-done; err != nil {
		return err
	}
	return failed
}

func queryError(e dialect, timeout time.Duration, code uint32, stderr string) error {
	if strings.Contains(stderr, e.timedOut) {
		return errTimeout.Err("seconds", int(timeout.Seconds()))
	}
	detail := strings.TrimSpace(stderr)
	// psql says where in its input it was; the user never saw that input.
	if i := strings.Index(detail, "ERROR:"); i >= 0 {
		detail = strings.TrimSpace(detail[i+len("ERROR:"):])
	}
	if detail == "" {
		detail = fmt.Sprintf("exit code %d", code)
	}
	return errQuery.Err("detail", detail)
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(b []byte) (int, error) {
	if l.n > 0 {
		k := min(len(b), l.n)
		l.w.Write(b[:k])
		l.n -= k
	}
	return len(b), nil
}

// IsUser reports whether err is meant for the user rather than Zelie's own.
func IsUser(err error) bool {
	var me *msg.Error
	return errors.As(err, &me)
}
