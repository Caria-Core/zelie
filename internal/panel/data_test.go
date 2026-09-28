package panel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/dataview"
)

// checkContainer answers as the core does for a container that is not the
// app's running one.
func (c *appCore) checkContainer(app, container string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.containers[container]; !ok || st.App != app || st.State != "running" {
		return &core.Error{Status: http.StatusConflict, Message: "container " + container + " is not running"}
	}
	return nil
}

func (c *appCore) DataTables(_ context.Context, app, container, engine string) ([]dataview.Table, error) {
	if err := c.checkContainer(app, container); err != nil {
		return nil, err
	}
	return []dataview.Table{{Schema: "public", Name: "orders", Rows: 2, Columns: []dataview.Column{{Name: "id", Type: engine}}}}, nil
}

func (c *appCore) DataRows(_ context.Context, app, container, engine string, q dataview.Query) (dataview.Page, error) {
	if err := c.checkContainer(app, container); err != nil {
		return dataview.Page{}, err
	}
	if q.Table != "orders" {
		return dataview.Page{}, &core.Error{Status: http.StatusNotFound, Code: "data.no_table", Message: "There is no table named " + q.Table + ".",
			Params: map[string]any{"table": q.Table}}
	}
	one := "1"
	return dataview.Page{Columns: []string{"id"}, Rows: [][]*string{{&one}}}, nil
}

func (c *appCore) DataExport(_ context.Context, app, container, engine string, q dataview.Query, w io.Writer) error {
	if err := c.checkContainer(app, container); err != nil {
		return err
	}
	fmt.Fprintf(w, "id\n1\n")
	return nil
}

func (c *appCore) DataKeys(_ context.Context, app, container, pattern, cursor string) (dataview.KeyPage, error) {
	if err := c.checkContainer(app, container); err != nil {
		return dataview.KeyPage{}, err
	}
	return dataview.KeyPage{Keys: []dataview.Key{{ID: "k", Name: "k", Type: "string", TTL: -1}}, Cursor: "0"}, nil
}

func (c *appCore) DataKey(_ context.Context, app, container, key string) (dataview.Value, error) {
	if err := c.checkContainer(app, container); err != nil {
		return dataview.Value{}, err
	}
	v := "hello"
	return dataview.Value{Key: dataview.Key{ID: key, Name: key, Type: "string"}, Size: 5, Columns: []string{"value"}, Rows: [][]*string{{&v}}}, nil
}

func TestData(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/databases", map[string]any{"id": "cache", "engine": "redis"})
	e.settle(t, "cache")
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")

	code, body, _ := e.raw("GET", "/api/apps/pg/data/tables")
	if code != http.StatusOK || !strings.Contains(body, `"name":"orders"`) {
		t.Fatalf("tables: %d %s", code, body)
	}
	if code, out := e.b.do("POST", "/api/apps/pg/data/rows", map[string]any{"schema": "public", "table": "orders"}); code != http.StatusOK || out["columns"] == nil {
		t.Errorf("rows: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/apps/pg/data/rows", map[string]any{"schema": "public", "table": "nope"}); code != http.StatusNotFound || out["code"] != "data.no_table" {
		t.Errorf("unknown table: %d %v", code, out)
	}
	for _, c := range []struct{ method, path, code string }{
		{"GET", "/api/apps/web/data/tables", "data.not_database"},
		{"GET", "/api/apps/cache/data/tables", "data.redis_tables"},
		{"POST", "/api/apps/pg/data/keys", "data.not_redis"},
	} {
		var body any
		if c.method == "POST" {
			body = map[string]any{}
		}
		if code, out := e.b.do(c.method, c.path, body); code < 400 || out["code"] != c.code {
			t.Errorf("%s: %d %v", c.path, code, out)
		}
	}
	if code, out := e.b.do("POST", "/api/apps/cache/data/keys", map[string]any{"pattern": "*"}); code != http.StatusOK || out["cursor"] != "0" {
		t.Errorf("keys: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/apps/cache/data/key", map[string]any{"id": "k"}); code != http.StatusOK || out["size"] != 5.0 {
		t.Errorf("key: %d %v", code, out)
	}

	// Export: confirmed, then one download by link.
	code, out := e.b.do("POST", "/api/apps/pg/data/export", map[string]any{"schema": "public", "table": "orders"})
	if code != http.StatusOK {
		t.Fatalf("export: %d %v", code, out)
	}
	link := out["url"].(string)
	code, body, h := e.raw("GET", link)
	if code != http.StatusOK || body != "id\n1\n" || !strings.HasPrefix(h.Get("Content-Disposition"), `attachment; filename="pg-orders-`) {
		t.Errorf("download: %d %q %v", code, body, h)
	}
	if code, _, _ := e.raw("GET", link); code != http.StatusNotFound {
		t.Errorf("second download: %d", code)
	}

	// A stopped database has nothing to show.
	e.b.do("POST", "/api/apps/pg/stop", nil)
	if code, out := e.b.do("POST", "/api/apps/pg/data/rows", map[string]any{"table": "orders"}); code != http.StatusConflict {
		t.Errorf("stopped: %d %v", code, out)
	}

	// Without a recent confirmation, no export.
	e.s.Now = func() time.Time { return time.Unix(1_800_000_000, 0).Add(time.Hour) }
	if code, out := e.b.do("POST", "/api/apps/pg/data/export", map[string]any{"table": "orders"}); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("export without confirming: %d %v", code, out)
	}
}
