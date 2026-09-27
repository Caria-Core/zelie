package panel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

// The backup side of appCore: files by app, and what was restored where.
type coreBackups struct {
	files      map[string][]string
	restored   []string // app/file into container or volume
	failBackup string   // the database refuses to be dumped
	failures   int      // how many backups fail before one works; -1 is all
	n          int
}

func (c *appCore) CreateBackup(_ context.Context, app, container, kind string) (core.Backup, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if st, ok := c.containers[container]; !ok || st.App != app || st.State != "running" {
		return core.Backup{}, &core.Error{Status: http.StatusConflict, Message: "container " + container + " is not running"}
	}
	if c.bk.failBackup != "" && c.bk.failures != 0 {
		if c.bk.failures > 0 {
			c.bk.failures--
		}
		return core.Backup{}, &core.Error{Status: http.StatusUnprocessableEntity, Message: c.bk.failBackup}
	}
	if c.bk.files == nil {
		c.bk.files = map[string][]string{}
	}
	ext := map[string]string{"postgres": "sql", "mariadb": "sql", "redis": "rdb"}[kind]
	c.bk.n++
	name := fmt.Sprintf("20270101T00000%dZ-abcd.%s.zst.age", c.bk.n, ext)
	c.bk.files[app] = append(c.bk.files[app], name)
	return core.Backup{Name: name, Bytes: 1000, Created: time.Now()}, nil
}

func (c *appCore) DownloadBackup(_ context.Context, app, name string, w io.Writer) error {
	_, err := io.WriteString(w, "age-encryption.org/v1 "+app+"/"+name)
	return err
}

func (c *appCore) RemoveBackup(_ context.Context, app, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	i := slices.Index(c.bk.files[app], name)
	if i < 0 {
		return &core.Error{Status: http.StatusNotFound, Message: "backup, container or volume not found"}
	}
	c.bk.files[app] = slices.Delete(c.bk.files[app], i, i+1)
	return nil
}

func (c *appCore) RestoreBackup(_ context.Context, app, name, kind, container, volume string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.bk.files[app], name) {
		return &core.Error{Status: http.StatusNotFound, Message: "backup, container or volume not found"}
	}
	if kind == "redis" {
		// Its volume is refused while it runs.
		for id, st := range c.containers {
			if st.State == "running" && slices.ContainsFunc(c.mounts[id], func(m engine.VolumeMount) bool { return m.Name == volume }) {
				return &core.Error{Status: http.StatusConflict, Message: "volume in use"}
			}
		}
		c.bk.restored = append(c.bk.restored, app+"/"+name+" into volume")
		return nil
	}
	if st, ok := c.containers[container]; !ok || st.State != "running" {
		return &core.Error{Status: http.StatusConflict, Message: "not running"}
	}
	// A linked app must be down while its database is replaced.
	for _, st := range c.containers {
		if st.State == "running" && slices.ContainsFunc(c.links[st.App], func(l engine.Link) bool { return l.To == app }) {
			return fmt.Errorf("%s still runs during the restore", st.App)
		}
	}
	c.bk.restored = append(c.bk.restored, app+"/"+name+" into "+container)
	return nil
}

func (c *appCore) RecoveryKey(_ context.Context, host string, w io.Writer) error {
	_, err := io.WriteString(w, "# Zelie backup recovery key for "+host+"\nAGE-SECRET-KEY-1TEST\n")
	return err
}

// raw does a request and returns the body as it came.
func (e *appEnv) raw(method, path string) (int, string, http.Header) {
	e.b.t.Helper()
	rec := e.b.record(method, path, nil)
	return rec.Code, rec.Body.String(), rec.Header()
}

// backUp makes a backup by hand and returns it, once it is done.
func (e *appEnv) backUp(t *testing.T, app string) map[string]any {
	t.Helper()
	if code, out := e.b.do("POST", "/api/apps/"+app+"/backups", nil); code != http.StatusAccepted {
		t.Fatalf("back up: %d %v", code, out)
	}
	e.s.jobs.Wait()
	list, _ := e.backups(t, app)
	return list[0]
}

// restore restores a backup and returns how it went.
func (e *appEnv) restore(t *testing.T, app string, id int64) map[string]any {
	t.Helper()
	if code, out := e.b.do("POST", fmt.Sprintf("/api/backups/%d/restore", id), nil); code != http.StatusAccepted {
		t.Fatalf("restore: %d %v", code, out)
	}
	e.s.jobs.Wait()
	_, out := e.b.do("GET", "/api/apps/"+app+"/backups", nil)
	return out["restore"].(map[string]any)
}

func (e *appEnv) backups(t *testing.T, app string) (list []map[string]any, plan map[string]any) {
	t.Helper()
	code, out := e.b.do("GET", "/api/apps/"+app+"/backups", nil)
	if code != http.StatusOK {
		t.Fatalf("list backups: %d %v", code, out)
	}
	for _, b := range out["backups"].([]any) {
		list = append(list, b.(map[string]any))
	}
	return list, out["plan"].(map[string]any)
}

func TestBackupAndRestore(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	e.settle(t, "web")
	e.b.do("POST", "/api/apps/web/links", map[string]any{"db": "pg"})

	list, plan := e.backups(t, "pg")
	if len(list) != 0 || plan["enabled"] != true || plan["minute"] != 180.0 || plan["keep_days"] != 7.0 {
		t.Fatalf("new database: %v %v", list, plan)
	}
	if code, _ := e.b.do("GET", "/api/apps/web/backups", nil); code != http.StatusBadRequest {
		t.Errorf("an app's backups: %d", code)
	}

	out := e.backUp(t, "pg")
	if out["state"] != "done" || out["reason"] != "manual" || out["bytes"] != 1000.0 {
		t.Fatalf("back up: %v", out)
	}
	first := int64(out["id"].(float64))

	code, body, h := e.raw("GET", fmt.Sprintf("/api/backups/%d/download", first))
	if code != http.StatusOK || !strings.HasPrefix(body, "age-encryption.org/v1") || !strings.Contains(h.Get("Content-Disposition"), `filename="pg-`) || !strings.HasSuffix(h.Get("Content-Disposition"), `.sql.zst.age"`) {
		t.Errorf("download: %d %q %v", code, body, h)
	}

	// The live web container, before the restore.
	webBefore := e.settle(t, "web")

	out = e.restore(t, "pg", first)
	if out["state"] != "done" || out["safety"] == nil || out["backup"] != float64(first) || out["at"] == "0001-01-01T00:00:00Z" {
		t.Fatalf("restore: %v", out)
	}
	if r := out["restarted"].([]any); len(r) != 1 || r[0] != "web" {
		t.Errorf("restarted %v", r)
	}
	pgLive := e.settle(t, "pg")
	if want := fmt.Sprintf("pg/20270101T000001Z-abcd.sql.zst.age into pg-%d", pgLive.ID); len(e.core.bk.restored) != 1 || e.core.bk.restored[0] != want {
		t.Errorf("restored %v, want %s", e.core.bk.restored, want)
	}
	// web came back as a new deployment of the same version.
	if d := e.settle(t, "web"); d.ID == webBefore.ID || d.Cause != store.CauseRestore || d.State != store.DeployLive || d.Image != webBefore.Image {
		t.Errorf("web after restore: %+v", d)
	}
	list, _ = e.backups(t, "pg")
	if len(list) != 2 || list[1]["restored_at"] == nil || list[0]["reason"] != "restore" {
		t.Errorf("backups after restore: %v", list)
	}

	// No safety backup, no restore.
	e.core.bk.failBackup, e.core.bk.failures = "pg_dump: error: connection refused", -1
	out = e.restore(t, "pg", first)
	if out["state"] != "failed" || !strings.Contains(out["error"].(string), "Nothing was changed") || len(e.core.bk.restored) != 1 {
		t.Errorf("restore without a safety backup: %v, restored %v", out, e.core.bk.restored)
	}
	// The failure is on the list, in the database's words.
	out = e.backUp(t, "pg")
	if out["state"] != "failed" || out["error"] != "pg_dump: error: connection refused" {
		t.Errorf("failed backup: %v", out)
	}
	e.core.bk.failBackup = ""

	for _, c := range []struct{ method, path string }{
		{"PUT", "/api/apps/pg/backups/plan"},
	} {
		for _, body := range []map[string]any{{"enabled": true, "minute": 1440, "keep_days": 7}, {"enabled": true, "minute": 60, "keep_days": 0}} {
			if code, _ := e.b.do(c.method, c.path, body); code != http.StatusBadRequest {
				t.Errorf("plan %v: %d", body, code)
			}
		}
	}
	if code, _ := e.b.do("PUT", "/api/apps/pg/backups/plan", map[string]any{"enabled": false, "minute": 90, "keep_days": 30}); code != http.StatusOK {
		t.Errorf("plan: %d", code)
	}
	if _, plan := e.backups(t, "pg"); plan["enabled"] != false || plan["minute"] != 90.0 || plan["keep_days"] != 30.0 {
		t.Errorf("plan %v", plan)
	}
}

func TestRestoreRedis(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "cache", "engine": "redis"})
	before := e.settle(t, "cache")
	id := int64(e.backUp(t, "cache")["id"].(float64))
	out := e.restore(t, "cache", id)
	if out["state"] != "done" || len(e.core.bk.restored) != 1 || !strings.HasSuffix(e.core.bk.restored[0], "into volume") {
		t.Fatalf("restore: %v, restored %v", out, e.core.bk.restored)
	}
	if d := e.settle(t, "cache"); d.ID == before.ID || d.Cause != store.CauseRestore || d.State != store.DeployLive {
		t.Errorf("redis after restore: %+v", d)
	}
}

func TestBackupsNeedConfirming(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	id := int64(e.backUp(t, "pg")["id"].(float64))

	code, body, h := e.raw("GET", "/api/backups/recovery")
	if code != http.StatusOK || !strings.Contains(body, "AGE-SECRET-KEY-1") || !strings.Contains(body, "panel.example.com") || !strings.Contains(h.Get("Content-Disposition"), "zelie-recovery.txt") {
		t.Errorf("recovery: %d %q", code, body)
	}
	if code, out := e.b.do("GET", "/api/apps/pg/backups", nil); code != http.StatusOK || out["recovery_saved_at"] == nil {
		t.Errorf("recovery not recorded: %v", out)
	}

	e.s.Now = func() time.Time { return time.Unix(1_800_000_000, 0).Add(time.Hour) }
	for _, c := range []struct{ method, path string }{
		{"POST", fmt.Sprintf("/api/backups/%d/restore", id)},
		{"DELETE", fmt.Sprintf("/api/backups/%d", id)},
		{"GET", "/api/backups/recovery"},
	} {
		if code, out := e.b.do(c.method, c.path, nil); code != http.StatusForbidden || out["confirm"] != true {
			t.Errorf("%s %s without confirming: %d %v", c.method, c.path, code, out)
		}
	}
	if len(e.core.bk.restored) != 0 || len(e.core.bk.files["pg"]) != 1 {
		t.Errorf("did something anyway: %v %v", e.core.bk.restored, e.core.bk.files)
	}
}

func TestDeletedDatabaseKeepsBackups(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	id := int64(e.backUp(t, "pg")["id"].(float64))
	if code, _ := e.b.do("DELETE", "/api/apps/pg", nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	code, body, _ := e.raw("GET", "/api/backups/deleted")
	if code != http.StatusOK || !strings.Contains(body, `"app":"pg"`) {
		t.Errorf("deleted: %d %s", code, body)
	}
	code, out := e.b.do("POST", fmt.Sprintf("/api/backups/%d/restore", id), nil)
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "create a PostgreSQL database named pg") {
		t.Errorf("restore into a deleted database: %d %v", code, out)
	}
	// A new database of another kind under the name does not take it.
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "redis"})
	e.settle(t, "pg")
	code, out = e.b.do("POST", fmt.Sprintf("/api/backups/%d/restore", id), nil)
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "PostgreSQL") {
		t.Errorf("restore into another kind: %d %v", code, out)
	}
	if code, _ := e.b.do("DELETE", fmt.Sprintf("/api/backups/%d", id), nil); code != http.StatusNoContent || len(e.core.bk.files["pg"]) != 0 {
		t.Errorf("delete backup: %d %v", code, e.core.bk.files)
	}
}

func TestScheduledBackups(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	ctx := context.Background()
	created := time.Unix(1_800_000_000, 0)
	now := created
	e.s.Now = func() time.Time { return now }
	scheduled := func() (n, failed int) {
		list, _ := e.s.Store.Backups(ctx, "pg")
		for _, b := range list {
			if b.Reason == store.BackupScheduled {
				n++
				if b.State == store.BackupFailed {
					failed++
				}
			}
		}
		return
	}

	// Made after today's slot: it waits for tomorrow's.
	e.s.backupsOnce(ctx)
	if n, _ := scheduled(); n != 0 {
		t.Fatalf("backed up before its first slot: %d", n)
	}
	next := lastSlot(created, 180).AddDate(0, 0, 1)
	now = next.Add(time.Minute)
	e.s.backupsOnce(ctx)
	e.s.backupsOnce(ctx)
	if n, failed := scheduled(); n != 1 || failed != 0 {
		t.Fatalf("after the slot: %d backups, %d failed", n, failed)
	}

	// A failure is tried again later, three times at most.
	e.core.bk.failBackup, e.core.bk.failures = "the database was restarting", -1
	now = next.AddDate(0, 0, 1).Add(time.Minute)
	for range 6 {
		e.s.backupsOnce(ctx)
		now = now.Add(backupRetry)
	}
	if n, failed := scheduled(); n != 4 || failed != 3 {
		t.Errorf("failing day: %d backups, %d failed", n, failed)
	}

	// A stopped database is left alone.
	e.core.bk.failBackup = ""
	e.b.do("POST", "/api/apps/pg/stop", nil)
	now = now.AddDate(0, 0, 1)
	e.s.backupsOnce(ctx)
	if n, _ := scheduled(); n != 4 {
		t.Errorf("backed up a stopped database: %d", n)
	}

	// Past a week everything goes but the newest good backup.
	now = now.AddDate(0, 0, 30)
	e.s.backupsOnce(ctx)
	list, _ := e.s.Store.Backups(ctx, "pg")
	if len(list) != 1 || list[0].State != store.BackupDone || len(e.core.bk.files["pg"]) != 1 {
		t.Errorf("after pruning: %+v, files %v", list, e.core.bk.files)
	}
}

func TestLastSlot(t *testing.T) {
	loc := time.FixedZone("x", 3*3600)
	at := func(h, m int) time.Time { return time.Date(2027, 3, 10, h, m, 0, 0, loc) }
	if got := lastSlot(at(4, 0), 180); !got.Equal(at(3, 0)) {
		t.Errorf("after: %v", got)
	}
	if got := lastSlot(at(2, 59), 180); !got.Equal(at(3, 0).AddDate(0, 0, -1)) {
		t.Errorf("before: %v", got)
	}
}

func TestZoneLabel(t *testing.T) {
	at := time.Date(2027, 3, 10, 3, 0, 0, 0, time.FixedZone("x", 3*3600+1800))
	if got := zoneLabel(at); got != "x, UTC+03:30" {
		t.Errorf("fixed: %q", got)
	}
	if got := zoneLabel(at.UTC()); got != "UTC+00:00" {
		t.Errorf("utc: %q", got)
	}
}
