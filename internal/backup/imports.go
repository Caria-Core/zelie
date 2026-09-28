package backup

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/klauspost/compress/zstd"
)

// A dump someone uploads was made on another server, often by another tool.
// It is read once here and written again as what our database can load: the
// few statements that only make sense where it came from are taken out, and
// the user is told how many.

var (
	errImportEmpty    = msg.Define(http.StatusUnprocessableEntity, "import.empty", "The file is empty.")
	errImportDamaged  = msg.Define(http.StatusUnprocessableEntity, "import.damaged", "The file is damaged: {detail}")
	errImportZip      = msg.Define(http.StatusUnprocessableEntity, "import.zip", "This is a zip archive. Unzip it and upload the .sql file inside.")
	errImportNotSQL   = msg.Define(http.StatusUnprocessableEntity, "import.not_sql", "This is not an SQL dump: it is not plain text.")
	errImportNotRedis = msg.Define(http.StatusUnprocessableEntity, "import.not_redis", "This is not a Redis RDB file.")
	// ErrImportPgCustom is exported for the panel, which says it as soon as
	// a file is named like one.
	ErrImportPgCustom  = msg.Define(http.StatusUnprocessableEntity, "import.pg_custom", "This dump is in pg_dump's custom format. Upload a plain SQL dump (pg_dump -Fp), or turn this one into SQL with pg_restore -f dump.sql.")
	errImportPgCluster = msg.Define(http.StatusUnprocessableEntity, "import.pg_cluster", "This is a dump of a whole PostgreSQL server, made with pg_dumpall. Dump the one database you need with pg_dump.")
	errImportOther     = msg.Define(http.StatusUnprocessableEntity, "import.other_engine", "This is a dump of a {other} database, and this database is {engine}.")
)

// Adapted counts the statements an import took out or changed, by what
// they were.
type Adapted map[string]int

// Uploads longer than this per line pass through untouched: they are data,
// and a statement worth adapting is never that long.
const maxAdaptLine = 1 << 20

var (
	gzipMagic = []byte{0x1f, 0x8b}
	zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}
	zipMagic  = []byte("PK\x03\x04")
)

// ImportDump reads an uploaded dump for a database of kind k, compressed
// or not, and writes what the database can load to w.
func ImportDump(k string, r io.Reader, w io.Writer) (Adapted, error) {
	if _, ok := kinds[k]; !ok {
		return nil, errors.New("no import for " + k)
	}
	in, err := decompress(r)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	br := bufio.NewReaderSize(in, maxAdaptLine)
	head, err := br.Peek(4096)
	if len(head) == 0 {
		if err != nil && err != io.EOF {
			return nil, damaged(err)
		}
		return nil, errImportEmpty.Err()
	}
	if k == "redis" {
		if !bytes.HasPrefix(head, []byte("REDIS")) {
			return nil, errImportNotRedis.Err()
		}
		if _, err := io.Copy(w, br); err != nil {
			return nil, damaged(err)
		}
		return Adapted{}, nil
	}
	if err := checkSQL(k, head); err != nil {
		return nil, err
	}
	f := &sqlFilter{postgres: k == "postgres", start: true, adapted: Adapted{}}
	if err := f.run(br, w); err != nil {
		return nil, err
	}
	return f.adapted, nil
}

type nopCloser struct{ io.Reader }

func (nopCloser) Close() error { return nil }

type zstdCloser struct{ *zstd.Decoder }

func (z zstdCloser) Close() error { z.Decoder.Close(); return nil }

// decompress looks at the first bytes, not at the file's name.
func decompress(r io.Reader) (io.ReadCloser, error) {
	br := bufio.NewReader(r)
	magic, _ := br.Peek(4)
	switch {
	case bytes.HasPrefix(magic, gzipMagic):
		// Several gzip members one after another are one file, as gunzip
		// reads them.
		z, err := gzip.NewReader(br)
		if err != nil {
			return nil, damaged(err)
		}
		return z, nil
	case bytes.HasPrefix(magic, zstdMagic):
		z, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, damaged(err)
		}
		return zstdCloser{z}, nil
	case bytes.HasPrefix(magic, zipMagic):
		return nil, errImportZip.Err()
	}
	return nopCloser{br}, nil
}

func damaged(err error) error {
	var me *msg.Error
	if errors.As(err, &me) {
		return err
	}
	return errImportDamaged.Err("detail", err.Error())
}

func checkSQL(k string, head []byte) error {
	switch {
	case bytes.HasPrefix(head, []byte("PGDMP")):
		if k != "postgres" {
			return errImportOther.Err("other", "PostgreSQL", "engine", "MariaDB")
		}
		return ErrImportPgCustom.Err()
	case bytes.IndexByte(head, 0) >= 0:
		return errImportNotSQL.Err()
	case bytes.Contains(head, []byte("-- PostgreSQL database cluster dump")):
		return errImportPgCluster.Err()
	case k == "mariadb" && bytes.Contains(head, []byte("-- PostgreSQL database dump")):
		return errImportOther.Err("other", "PostgreSQL", "engine", "MariaDB")
	case k == "postgres" && (bytes.Contains(head, []byte("-- MySQL dump")) || bytes.Contains(head, []byte("-- MariaDB dump")) ||
		bytes.Contains(head, []byte("-- phpMyAdmin SQL Dump"))):
		return errImportOther.Err("other", "MySQL or MariaDB", "engine", "PostgreSQL")
	}
	return nil
}

// A rule drops one kind of statement. Statements that are not whole on
// their line are dropped up to the line that ends them.
type rule struct {
	name string
	re   *regexp.Regexp
}

var (
	// mysqldump and phpMyAdmin name the database they came from; ours is
	// always app, and the app user may not create another. Binary log
	// settings need a privilege the app user does not have.
	mariadbRules = []rule{
		{"CREATE DATABASE", regexp.MustCompile(`(?i)^CREATE DATABASE\b`)},
		{"USE", regexp.MustCompile("(?i)^USE\\s+(`[^`]*`|\\w+)\\s*;$")},
		{"GTID_PURGED", regexp.MustCompile(`(?i)^SET @@GLOBAL\.GTID_PURGED\s*=`)},
		{"SQL_LOG_BIN", regexp.MustCompile(`(?i)^SET @@SESSION\.SQL_LOG_BIN\s*=`)},
	}
	// The roles a dump names do not exist here, and everything it makes
	// belongs to app anyway: as pg_restore --no-owner --no-privileges. The
	// database is made empty just before it loads, so there is nothing to
	// drop, and it is not made or switched to by the dump.
	postgresRules = []rule{
		{"CREATE DATABASE", regexp.MustCompile(`(?i)^CREATE DATABASE\s`)},
		{"ALTER DATABASE", regexp.MustCompile(`(?i)^ALTER DATABASE\s`)},
		{"OWNER TO", regexp.MustCompile(`(?i)^ALTER\s.*\sOWNER TO\s`)},
		{"GRANT", regexp.MustCompile(`(?i)^GRANT\s`)},
		{"REVOKE", regexp.MustCompile(`(?i)^REVOKE\s`)},
		{"DEFAULT PRIVILEGES", regexp.MustCompile(`(?i)^ALTER DEFAULT PRIVILEGES\s`)},
		{"DROP", regexp.MustCompile(`(?i)^DROP\s`)},
		{`\connect`, regexp.MustCompile(`^\\c(onnect)?(\s|$)`)},
	}
	// The user that made a view, trigger, routine or event on the old
	// server is not here, and only an administrator may name another.
	// Without it, they belong to app.
	definer     = regexp.MustCompile("(?i)DEFINER\\s*=\\s*(`[^`]*`|'[^']*'|\\w+)@(`[^`]*`|'[^']*'|[\\w.%-]+)")
	dollarQuote = regexp.MustCompile(`\$[A-Za-z_]*\$`)
)

type sqlFilter struct {
	postgres bool
	// start is set when the next line begins a statement.
	start bool
	// skipping drops lines until the statement being dropped ends.
	skipping bool
	// copying is inside a COPY's data, which is never touched.
	copying bool
	// dollar is the tag of the dollar-quoted string a line is in, like a
	// function's body.
	dollar  string
	adapted Adapted
}

func (f *sqlFilter) run(br *bufio.Reader, w io.Writer) error {
	bw := bufio.NewWriterSize(w, 1<<20)
	for {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// A very long line is data: it goes through as it is, piece by
			// piece, unless it belongs to a statement being dropped.
			if err := f.long(br, bw, line); err != nil {
				return err
			}
			continue
		}
		if len(line) > 0 {
			if werr := f.line(bw, line); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return damaged(err)
		}
	}
	return bw.Flush()
}

func (f *sqlFilter) long(br *bufio.Reader, w io.Writer, first []byte) error {
	var last []byte
	for part, err := first, bufio.ErrBufferFull; ; part, err = br.ReadSlice('\n') {
		if !f.skipping {
			if _, werr := w.Write(part); werr != nil {
				return werr
			}
		}
		if len(part) > 0 {
			last = part
		}
		if err == io.EOF || err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return damaged(err)
		}
	}
	ends := bytes.HasSuffix(bytes.TrimRight(last, " \t\r\n"), []byte(";"))
	if f.skipping && ends {
		f.skipping, f.start = false, true
	} else if !f.copying && f.dollar == "" {
		f.start = ends
	}
	return nil
}

func (f *sqlFilter) line(w io.Writer, line []byte) error {
	text := bytes.TrimRight(line, " \t\r\n")
	ends := bytes.HasSuffix(text, []byte(";"))
	switch {
	case f.copying:
		if bytes.Equal(text, []byte(`\.`)) {
			f.copying, f.start = false, true
		}
	case f.skipping:
		if ends {
			f.skipping, f.start = false, true
		}
		return nil
	case len(text) == 0 || bytes.HasPrefix(text, []byte("--")):
		// Neither starts nor ends a statement.
	case !f.postgres:
		if bytes.HasPrefix(text, []byte("INSERT ")) {
			break
		}
		if name, ok := match(mariadbRules, text); ok {
			f.drop(name, ends)
			return nil
		}
		if bytes.HasPrefix(text, []byte("CREATE ")) || bytes.HasPrefix(text, []byte("/*!")) {
			if n := len(definer.FindAllIndex(line, -1)); n > 0 {
				f.adapted["DEFINER"] += n
				line = definer.ReplaceAll(line, nil)
			}
		}
	case f.dollar == "" && (f.start || text[0] == '\\'):
		// psql's own commands, such as \connect, stand on a line of their
		// own wherever they are.
		meta := text[0] == '\\'
		if name, ok := match(postgresRules, text); ok {
			f.drop(name, ends || meta)
			return nil
		}
		if meta {
			f.start = true
			break
		}
		if bytes.HasPrefix(text, []byte("COPY ")) && bytes.HasSuffix(text, []byte("FROM stdin;")) {
			f.copying = true
			break
		}
		fallthrough
	default:
		f.quotes(text)
		f.start = ends && f.dollar == ""
	}
	_, err := w.Write(line)
	return err
}

func match(rules []rule, text []byte) (string, bool) {
	for _, r := range rules {
		if r.re.Match(text) {
			return r.name, true
		}
	}
	return "", false
}

func (f *sqlFilter) drop(name string, ends bool) {
	f.adapted[name]++
	f.skipping = !ends
	f.start = ends
}

// quotes follows dollar-quoted strings, so a line inside a function's body
// is never taken for a statement.
func (f *sqlFilter) quotes(text []byte) {
	for _, tag := range dollarQuote.FindAll(text, -1) {
		switch {
		case f.dollar == "":
			f.dollar = string(tag)
		case f.dollar == string(tag):
			f.dollar = ""
		}
	}
}
