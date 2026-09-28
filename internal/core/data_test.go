package core

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
)

func TestReadData(t *testing.T) {
	s, f, _ := backupServer(t)
	root := &peer.Peer{UID: 0}
	var sent []string
	f.exec = func(args []string, stdin io.Reader, stdout io.Writer) uint32 {
		b, _ := io.ReadAll(stdin)
		sent = append(sent, string(b))
		if strings.Contains(string(b), "'t', ") {
			fmt.Fprintln(stdout, `["t", "public", "t", false, 2, 8192]`)
			fmt.Fprintln(stdout, `["c", "public", "t", "x", "integer", true, false, false]`)
		} else {
			fmt.Fprintln(stdout, `["1"]`)
			fmt.Fprintln(stdout, `[null]`)
		}
		return 0
	}
	rec := request(t, s, root, "POST", "/v1/data/db", `{"container":"db-1","engine":"postgres","op":"tables"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"t"`) {
		t.Fatalf("tables: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, s, root, "POST", "/v1/data/db", `{"container":"db-1","engine":"postgres","op":"rows","query":{"schema":"public","table":"t"}}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"rows":[["1"],[null]]`) {
		t.Fatalf("rows: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, s, root, "POST", "/v1/data/db", `{"container":"db-1","engine":"postgres","op":"export","query":{"schema":"public","table":"t"}}`)
	if rec.Code != http.StatusOK || rec.Body.String() != "x\n1\n\n" || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("export: %d %q", rec.Code, rec.Body)
	}
	rec = request(t, s, root, "POST", "/v1/data/db", `{"container":"db-1","engine":"postgres","op":"rows","query":{"schema":"public","table":"nope"}}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "data.no_table") {
		t.Errorf("unknown table: %d %s", rec.Code, rec.Body)
	}

	n := len(sent)
	for _, c := range []struct {
		body string
		code int
	}{
		// Other apps' containers, and ops that do not fit.
		{`{"container":"web-1","engine":"postgres","op":"tables"}`, http.StatusBadRequest},
		{`{"container":"old-1","engine":"postgres","op":"tables"}`, http.StatusBadRequest},
		{`{"container":"db-1","engine":"postgres","op":"keys"}`, http.StatusBadRequest},
		{`{"container":"db-1","engine":"redis","op":"rows"}`, http.StatusBadRequest},
		{`{"container":"db-1","engine":"sqlite","op":"tables"}`, http.StatusBadRequest},
	} {
		if rec := request(t, s, root, "POST", "/v1/data/db", c.body); rec.Code != c.code {
			t.Errorf("%s: %d %s", c.body, rec.Code, rec.Body)
		}
	}
	if len(sent) != n {
		t.Errorf("ran %d queries for refused requests", len(sent)-n)
	}
}
