package dataview

import (
	"encoding/csv"
	"io"
	"regexp"
	"strings"
	"sync"
)

// csvWriter writes rows as CSV. A NULL is an empty field, as most tools
// read it.
type csvWriter struct{ w *csv.Writer }

func newCSV(w io.Writer) *csvWriter { return &csvWriter{csv.NewWriter(w)} }

func (c *csvWriter) write(row []*string) error {
	rec := make([]string, len(row))
	for i, v := range row {
		if v != nil {
			rec[i] = formulaSafe(*v)
		}
	}
	return c.w.Write(rec)
}

func (c *csvWriter) flush() error {
	c.w.Flush()
	return c.w.Error()
}

var plainNumber = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
})

// formulaSafe keeps a spreadsheet from running a cell as a formula. What
// the apps put in their tables is not to be trusted, and a cell that starts
// with one of these characters is a formula to Excel, LibreOffice and
// Sheets. A leading apostrophe makes it text. Numbers stay as they are:
// they are not formulas, and a minus sign is no reason to alter them.
func formulaSafe(s string) string {
	if s == "" || strings.IndexByte("=+-@\t\r", s[0]) < 0 || plainNumber().MatchString(s) {
		return s
	}
	return "'" + s
}
