package panel

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/core"
)

type coreUpload struct {
	app  string
	size int64
	data []byte
}

func (c *appCore) StartUpload(_ context.Context, app string, size int64) (core.Upload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bk.uploads == nil {
		c.bk.uploads = map[string]*coreUpload{}
	}
	c.bk.n++
	id := fmt.Sprintf("%032x", c.bk.n)
	c.bk.uploads[id] = &coreUpload{app: app, size: size}
	return core.Upload{ID: id, App: app, Size: size}, nil
}

func (c *appCore) Upload(_ context.Context, id string) (core.Upload, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.bk.uploads[id]
	if !ok {
		return core.Upload{}, &core.Error{Status: http.StatusNotFound, Message: "not found"}
	}
	return core.Upload{ID: id, App: u.app, Size: u.size, Received: int64(len(u.data))}, nil
}

func (c *appCore) AppendUpload(_ context.Context, id string, offset int64, piece io.Reader) (core.Upload, error) {
	b, err := io.ReadAll(piece)
	if err != nil {
		return core.Upload{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.bk.uploads[id]
	if !ok {
		return core.Upload{}, &core.Error{Status: http.StatusNotFound, Message: "not found"}
	}
	if offset != int64(len(u.data)) {
		return core.Upload{}, &core.Error{Status: http.StatusConflict, Code: "upload.offset", Message: "wrong offset",
			Params: map[string]any{"received": len(u.data), "offset": offset}}
	}
	u.data = append(u.data, b...)
	return core.Upload{ID: id, App: u.app, Size: u.size, Received: int64(len(u.data))}, nil
}

func (c *appCore) RemoveUpload(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.bk.uploads, id)
	return nil
}

func (c *appCore) ImportUpload(_ context.Context, id, kind string) (core.Backup, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.bk.uploads[id]
	delete(c.bk.uploads, id)
	if bytes.HasPrefix(u.data, []byte("PGDMP")) {
		return core.Backup{}, &core.Error{Status: http.StatusUnprocessableEntity, Code: "import.pg_custom", Message: "custom format"}
	}
	c.bk.n++
	name := fmt.Sprintf("20270101T00000%dZ-up.sql.zst.age", c.bk.n)
	if c.bk.files == nil {
		c.bk.files = map[string][]string{}
	}
	c.bk.files[u.app] = append(c.bk.files[u.app], name)
	var adapted backup.Adapted
	if n := bytes.Count(u.data, []byte("USE ")); n > 0 {
		adapted = backup.Adapted{"USE": n}
	}
	return core.Backup{Name: name, Bytes: int64(len(u.data)), Created: time.Now(), Adapted: adapted}, nil
}

func TestDumpUpload(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "pg"})
	dump := []byte("USE `old`;\nCREATE TABLE t (x int);\n")

	for _, c := range []struct {
		app, name string
		code      string
	}{
		{"web", "dump.sql", "upload.not_database"},
		{"pg", "dump.rdb", "upload.type"},
		{"pg", ".sql", "upload.type"},
		{"pg", "shop.dump", "import.pg_custom"},
	} {
		if code, out := e.b.do("POST", "/api/apps/"+c.app+"/uploads", map[string]any{"name": c.name, "size": 10}); code < 400 || out["code"] != c.code {
			t.Errorf("%s %s: %d %v", c.app, c.name, code, out)
		}
	}

	code, out := e.b.do("POST", "/api/apps/pg/uploads", map[string]any{"name": "Shop.SQL", "size": len(dump)})
	if code != http.StatusCreated || out["received"] != 0.0 || out["chunk"] != float64(dumpChunk) {
		t.Fatalf("start: %d %v", code, out)
	}
	id := out["id"].(string)
	base := "/api/apps/pg/uploads/" + id
	if code, out := e.b.do("PUT", base+"?offset=0", dump[:10]); code != http.StatusOK || out["received"] != 10.0 {
		t.Fatalf("first piece: %d %v", code, out)
	}
	// The page was reloaded: the same file goes on where it stopped.
	if code, out := e.b.do("POST", "/api/apps/pg/uploads", map[string]any{"name": "Shop.SQL", "size": len(dump)}); code != http.StatusOK || out["id"] != id || out["received"] != 10.0 {
		t.Fatalf("again: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", base+"?offset=0", dump[:10]); code != http.StatusConflict || out["code"] != "upload.offset" {
		t.Errorf("repeat: %d %v", code, out)
	}
	if code, out := e.b.do("PUT", base+"?offset=10", dump[10:]); code != http.StatusOK || out["received"] != float64(len(dump)) {
		t.Fatalf("second piece: %d %v", code, out)
	}
	// Someone else's upload is not found.
	if code, _ := e.b.do("PUT", "/api/apps/pg/uploads/"+strings.Repeat("0", 32)+"?offset=0", dump); code != http.StatusNotFound {
		t.Errorf("other upload: %d", code)
	}

	if code, out := e.b.do("POST", base+"/finish", nil); code != http.StatusAccepted {
		t.Fatalf("finish: %d %v", code, out)
	}
	e.s.jobs.Wait()
	_, got := e.b.do("GET", "/api/apps/pg/backups", nil)
	restore := got["restore"].(map[string]any)
	if restore["state"] != "done" || restore["safety"] == nil {
		t.Fatalf("restore: %v", restore)
	}
	if r := restore["restarted"].([]any); len(r) != 1 || r[0] != "web" {
		t.Errorf("restarted %v", r)
	}
	list, _ := e.backups(t, "pg")
	// Newest first: the safety backup, then the upload.
	if len(list) != 2 || list[0]["reason"] != "restore" || list[1]["reason"] != "uploaded" || list[1]["restored_at"] == nil ||
		list[1]["adapted"].(map[string]any)["USE"] != 1.0 || list[1]["offsite"] != nil {
		t.Fatalf("backups: %v", list)
	}
	if restore["backup"] != list[1]["id"] {
		t.Errorf("restore of %v, upload is %v", restore["backup"], list[1]["id"])
	}
	// The upload is used up.
	if code, _ := e.b.do("POST", base+"/finish", nil); code != http.StatusNotFound {
		t.Errorf("finish again: %d", code)
	}
}

// A file the core refuses is on the list with its reason, and nothing is
// restored or backed up.
func TestDumpUploadRefused(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	_, out := e.b.do("POST", "/api/apps/pg/uploads", map[string]any{"name": "shop.sql.gz", "size": 5})
	base := "/api/apps/pg/uploads/" + out["id"].(string)
	e.b.do("PUT", base+"?offset=0", []byte("PGDMP"))
	e.b.do("POST", base+"/finish", nil)
	e.s.jobs.Wait()
	_, got := e.b.do("GET", "/api/apps/pg/backups", nil)
	restore := got["restore"].(map[string]any)
	if restore["state"] != "failed" || restore["error"].(map[string]any)["code"] != "import.pg_custom" || restore["safety"] != nil {
		t.Fatalf("restore: %v", restore)
	}
	list, _ := e.backups(t, "pg")
	if len(list) != 1 || list[0]["state"] != "failed" || list[0]["reason"] != "uploaded" || len(e.core.bk.restored) != 0 {
		t.Fatalf("backups: %v, restored %v", list, e.core.bk.restored)
	}
}

func TestDumpUploadNeedsConfirming(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.s.Now = func() time.Time { return time.Unix(1_800_000_000, 0).Add(time.Hour) }
	if code, out := e.b.do("POST", "/api/apps/pg/uploads", map[string]any{"name": "shop.sql", "size": 5}); code != http.StatusForbidden || out["confirm"] != true {
		t.Errorf("without confirming: %d %v", code, out)
	}
}
