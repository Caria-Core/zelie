package core

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestSeekTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log")
	os.WriteFile(path, []byte("first line\nsecond line\nthird\n"), 0o600)
	// Lines start at bytes 0, 11 and 23; the file is 29 bytes long.
	for _, c := range []struct {
		n    int64
		want string
	}{
		{100, "first line\nsecond line\nthird\n"},
		{18, "second line\nthird\n"}, // window starts exactly at a line
		{19, "second line\nthird\n"}, // window starts one byte into the line before
		{14, "third\n"},
		{6, "third\n"},
		{3, ""}, // no whole line fits
	} {
		f, _ := os.Open(path)
		if err := seekTail(f, c.n); err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(f)
		f.Close()
		if string(got) != c.want {
			t.Errorf("tail %d = %q, want %q", c.n, got, c.want)
		}
	}
}
