package dataview

import (
	"encoding/csv"
	"io"
)

// csvWriter writes rows as CSV. A NULL is an empty field, as most tools
// read it.
type csvWriter struct{ w *csv.Writer }

func newCSV(w io.Writer) *csvWriter { return &csvWriter{csv.NewWriter(w)} }

func (c *csvWriter) write(row []*string) error {
	rec := make([]string, len(row))
	for i, v := range row {
		if v != nil {
			rec[i] = *v
		}
	}
	return c.w.Write(rec)
}

func (c *csvWriter) flush() error {
	c.w.Flush()
	return c.w.Error()
}
