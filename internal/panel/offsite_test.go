package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/store"
)

// The off-site side of appCore: the destination, and the copies in it by
// app/file.
type coreOffsite struct {
	info       core.OffsiteInfo
	set        []core.OffsiteConfig
	remote     map[string]bool
	failUpload int // how many uploads fail before one works
	fetched    []string
	meta       map[string]core.BackupMeta
	opens      bool // the keys here open what is there
}

func (c *appCore) Offsite(context.Context) (core.OffsiteInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.off.info, nil
}

func (c *appCore) SetOffsite(_ context.Context, cfg core.OffsiteConfig) (core.OffsiteInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg.SecretKey == "wrong" {
		return core.OffsiteInfo{}, &core.Error{Status: http.StatusUnprocessableEntity, Message: "The storage did not accept the access key or the secret key.", Code: "offsite.wrong_keys"}
	}
	c.off.set = append(c.off.set, cfg)
	c.off.info = core.OffsiteInfo{Set: true, Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Prefix: cfg.Prefix, AccessKey: cfg.AccessKey}
	return c.off.info, nil
}

func (c *appCore) RemoveOffsite(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.off.info = core.OffsiteInfo{}
	return nil
}

func (c *appCore) UploadBackup(_ context.Context, app, name string, meta core.BackupMeta) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.off.info.Set {
		return &core.Error{Status: http.StatusConflict, Message: "No off-site storage is set up.", Code: "offsite.not_set"}
	}
	if !slices.Contains(c.bk.files[app], name) {
		return &core.Error{Status: http.StatusNotFound, Message: "backup, container or volume not found"}
	}
	if c.off.failUpload > 0 {
		c.off.failUpload--
		return &core.Error{Status: http.StatusUnprocessableEntity, Message: "Could not reach s3.example.com: timeout", Code: "offsite.unreachable",
			Params: map[string]any{"endpoint": "s3.example.com", "detail": "timeout"}}
	}
	if c.off.remote == nil {
		c.off.remote = map[string]bool{}
	}
	c.off.remote[app+"/"+name] = true
	if c.off.meta == nil {
		c.off.meta = map[string]core.BackupMeta{}
	}
	c.off.meta[app+"/"+name] = meta
	return nil
}

func (c *appCore) OffsiteBackups(context.Context) (core.OffsiteList, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := core.OffsiteList{Key: "age1other", Opens: c.off.opens}
	for key := range c.off.remote {
		app, name, _ := strings.Cut(key, "/")
		b := core.OffsiteBackup{App: app, Name: name, Bytes: 1000, Created: time.Unix(1_800_000_000, 0)}
		if m, ok := c.off.meta[key]; ok && c.off.opens {
			b.Meta = &m
		} else {
			b.Locked = true
		}
		out.Backups = append(out.Backups, b)
	}
	slices.SortFunc(out.Backups, func(a, b core.OffsiteBackup) int { return strings.Compare(a.App+a.Name, b.App+b.Name) })
	return out, nil
}

func (c *appCore) AddOldKey(_ context.Context, recovery string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !strings.Contains(recovery, "AGE-SECRET-KEY-1") {
		return nil, &core.Error{Status: http.StatusBadRequest, Message: "This file has no backup key in it.", Code: "backup.no_key"}
	}
	c.off.opens = true
	return []string{"age1other"}, nil
}

func (c *appCore) RemoveOffsiteBackup(_ context.Context, app, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.off.info.Set {
		return &core.Error{Status: http.StatusConflict, Message: "No off-site storage is set up.", Code: "offsite.not_set"}
	}
	delete(c.off.remote, app+"/"+name)
	return nil
}

func (c *appCore) FetchBackup(_ context.Context, app, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.off.remote[app+"/"+name] {
		return &core.Error{Status: http.StatusNotFound, Message: "This backup is no longer in the off-site storage.", Code: "offsite.gone"}
	}
	c.off.fetched = append(c.off.fetched, app+"/"+name)
	if c.bk.files == nil {
		c.bk.files = map[string][]string{}
	}
	if !slices.Contains(c.bk.files[app], name) {
		c.bk.files[app] = append(c.bk.files[app], name)
	}
	return nil
}

func TestOffsiteCopies(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/databases", map[string]any{"id": "pg", "engine": "postgres"})
	e.settle(t, "pg")
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	row := func(id int64) store.Backup {
		t.Helper()
		b, err := e.s.Store.Backup(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Nowhere to send it: it stays here only.
	id := int64(e.backUp(t, "pg")["id"].(float64))
	if b := row(id); b.Offsite != "" || !b.Local {
		t.Fatalf("without a destination: %+v", b)
	}
	if code, _ := e.b.do("POST", fmt.Sprintf("/api/backups/%d/offsite", id), nil); code != http.StatusConflict {
		t.Errorf("send without a destination: %d", code)
	}

	// The panel names the folder after the server when none is given.
	if code, out := e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "wrong"}); code != http.StatusUnprocessableEntity || out["code"] != "offsite.wrong_keys" {
		t.Errorf("wrong keys: %d %v", code, out)
	}
	code, out := e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "s"})
	if code != http.StatusOK || out["prefix"] != "zelie/"+hostFolder() {
		t.Fatalf("set: %d %v", code, out)
	}
	if _, out := e.b.do("GET", "/api/offsite", nil); out["set"] != true || out["bucket"] != "b" || out["recovery_saved_at"] != nil {
		t.Errorf("get: %v", out)
	}
	if _, out := e.b.do("GET", "/api/apps/pg/backups", nil); out["offsite_set"] != true || out["plan"].(map[string]any)["offsite_days"] != 30.0 {
		t.Errorf("list: %v", out)
	}

	// A new backup goes as soon as it is made; a failed send is tried
	// again after a minute, then two.
	e.core.off.failUpload = 2
	first := int64(e.backUp(t, "pg")["id"].(float64))
	if b := row(first); b.Offsite != store.OffsitePending || !b.OffsiteUntil.Equal(b.CreatedAt.Add(30*24*time.Hour)) {
		t.Fatalf("queued: %+v", b)
	}
	e.s.uploadsOnce(ctx)
	b := row(first)
	if b.Offsite != store.OffsiteFailed || b.OffsiteTries != 1 || !b.OffsiteAt.Equal(now.Add(time.Minute)) || b.OffsiteError.Code != "offsite.unreachable" {
		t.Fatalf("first failure: %+v", b)
	}
	list, _ := e.backups(t, "pg")
	if list[0]["offsite"] != "failed" || list[0]["offsite_error"].(map[string]any)["code"] != "offsite.unreachable" {
		t.Errorf("failure on the list: %v", list[0])
	}
	e.s.uploadsOnce(ctx)
	if b := row(first); b.OffsiteTries != 1 {
		t.Errorf("tried again too soon: %+v", b)
	}
	now = now.Add(time.Minute)
	e.s.uploadsOnce(ctx)
	if b := row(first); b.OffsiteTries != 2 || !b.OffsiteAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("second failure: %+v", b)
	}
	now = now.Add(2 * time.Minute)
	e.s.uploadsOnce(ctx)
	if b := row(first); b.Offsite != store.OffsiteDone || b.OffsiteError != nil || !e.core.off.remote["pg/"+b.File] {
		t.Fatalf("sent: %+v", b)
	}

	// The backup from before the destination can be sent by hand.
	if code, _ := e.b.do("POST", fmt.Sprintf("/api/backups/%d/offsite", id), nil); code != http.StatusAccepted {
		t.Errorf("send by hand: %d", code)
	}
	e.s.uploadsOnce(ctx)
	if b := row(id); b.Offsite != store.OffsiteDone {
		t.Errorf("sent by hand: %+v", b)
	}

	// Deleting a backup deletes its copy.
	extra := int64(e.backUp(t, "pg")["id"].(float64))
	e.s.uploadsOnce(ctx)
	eb := row(extra)
	if !e.core.off.remote["pg/"+eb.File] {
		t.Fatal("not sent")
	}
	if code, _ := e.b.do("DELETE", fmt.Sprintf("/api/backups/%d", extra), nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if e.core.off.remote["pg/"+eb.File] {
		t.Error("deleted backup kept off-site")
	}

	// Off-site copies never go before the ones here.
	if code, out := e.b.do("PUT", "/api/apps/pg/backups/plan", map[string]any{"enabled": true, "minute": 180, "keep_days": 14, "offsite": true, "offsite_days": 7}); code != http.StatusBadRequest || out["code"] != "backup.bad_offsite_keep" {
		t.Errorf("shorter off-site: %d %v", code, out)
	}
	// Left out, they keep their values.
	if code, out := e.b.do("PUT", "/api/apps/pg/backups/plan", map[string]any{"enabled": true, "minute": 180, "keep_days": 7}); code != http.StatusOK || out["offsite_days"] != 30.0 || out["offsite"] != true {
		t.Errorf("plan without off-site fields: %d %v", code, out)
	}
	e.b.do("PUT", "/api/apps/pg/backups/plan", map[string]any{"enabled": true, "minute": 180, "keep_days": 7, "offsite": false, "offsite_days": 30})
	off := int64(e.backUp(t, "pg")["id"].(float64))
	if b := row(off); b.Offsite != "" {
		t.Errorf("sent with off-site turned off: %+v", b)
	}
	e.b.do("PUT", "/api/apps/pg/backups/plan", map[string]any{"enabled": true, "minute": 180, "keep_days": 7, "offsite": true, "offsite_days": 30})

	// Without a destination, what waits is not sent.
	e.core.off.failUpload = 1
	waiting := int64(e.backUp(t, "pg")["id"].(float64))
	e.s.uploadsOnce(ctx)
	if code, _ := e.b.do("DELETE", "/api/offsite", nil); code != http.StatusNoContent {
		t.Fatalf("remove destination: %d", code)
	}
	if b := row(waiting); b.Offsite != "" {
		t.Errorf("still queued without a destination: %+v", b)
	}
	e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "s"})

	// Past a week the files here go, but the ones off-site stay until
	// their 30 days are up. (The session ends too: from here on the test
	// calls the server directly.)
	now = now.Add(8 * 24 * time.Hour)
	e.s.backupsOnce(ctx)
	e.s.uploadsOnce(ctx)
	fb := row(first)
	if fb.Local || slices.Contains(e.core.bk.files["pg"], fb.File) || !e.core.off.remote["pg/"+fb.File] {
		t.Fatalf("after a week: %+v, files %v", fb, e.core.bk.files)
	}
	if out := e.s.backupOut(fb); out.Local || out.Offsite != "done" || out.OffsiteUntil == nil {
		t.Errorf("listed as %+v", out)
	}
	if _, err := e.s.Store.Backup(ctx, off); err != store.ErrNotFound {
		t.Errorf("a backup never sent outlived its week: %v", err)
	}
	rows, _ := e.s.Store.Backups(ctx, "pg")
	if newest := rows[0]; newest.Reason != store.BackupScheduled || !newest.Local || newest.Offsite != store.OffsiteDone {
		t.Errorf("the new scheduled backup: %+v", newest)
	}

	// Restoring one kept off-site brings it back first, for a day.
	a, _ := e.s.Store.App(ctx, "pg")
	res := e.s.runRestore(ctx, a, fb, 0)
	e.s.deploys.wg.Wait()
	fb = row(first)
	if res.State != "done" || len(e.core.off.fetched) != 1 || !fb.Local || !fb.KeepUntil.Equal(now.Add(fetchedKeep)) {
		t.Fatalf("restore from off-site: %+v %+v", res, fb)
	}
	now = now.Add(2 * fetchedKeep)
	e.s.backupsOnce(ctx)
	if b := row(first); b.Local {
		t.Errorf("the fetched copy stayed: %+v", b)
	}

	// Past 30 days the off-site copy goes too, and the backup with it.
	now = now.Add(30 * 24 * time.Hour)
	e.s.backupsOnce(ctx)
	if _, err := e.s.Store.Backup(ctx, first); err != store.ErrNotFound {
		t.Errorf("expired off-site backup: %v", err)
	}
	if e.core.off.remote["pg/"+fb.File] {
		t.Error("expired off-site copy kept")
	}
}

func TestOffsiteWait(t *testing.T) {
	for tries, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 5: 16 * time.Minute, 9: 256 * time.Minute, 10: offsiteMaxWait, 60: offsiteMaxWait} {
		if got := offsiteWait(tries); got != want {
			t.Errorf("offsiteWait(%d) = %v, want %v", tries, got, want)
		}
	}
}

// Backups another server left in the bucket: found, locked until its
// recovery file is added, then listed with those of deleted apps and
// restored into a new database of the same name.
func TestFoundOffsite(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("PUT", "/api/offsite", map[string]any{"endpoint": "https://s3.example.com", "bucket": "b", "access_key": "a", "secret_key": "s"})
	e.core.off.remote = map[string]bool{"old-db/20260101T030000Z-aaaa.sql.zst.age": true, "files/20260101T030000Z-bbbb.tar.zst.age": true}
	e.core.off.meta = map[string]core.BackupMeta{
		"old-db/20260101T030000Z-aaaa.sql.zst.age": {Engine: "postgres"},
		"files/20260101T030000Z-bbbb.tar.zst.age":  {Volumes: []string{"data"}, Size: 5000},
	}

	code, out := e.b.do("GET", "/api/offsite/found", nil)
	if code != http.StatusOK || out["opens"] != false || len(out["backups"].([]any)) != 2 || out["backups"].([]any)[0].(map[string]any)["locked"] != true {
		t.Fatalf("found, locked: %d %v", code, out)
	}
	if _, out := e.b.do("POST", "/api/offsite/found", nil); out["added"] != 0.0 {
		t.Errorf("added locked backups: %v", out)
	}
	if code, out := e.b.do("POST", "/api/backups/keys", map[string]any{"recovery": "hello"}); code != http.StatusBadRequest || out["code"] != "backup.no_key" {
		t.Errorf("not a key: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/backups/keys", map[string]any{"recovery": "# x\nAGE-SECRET-KEY-1ABC\n"}); code != http.StatusOK || out["added"] != 1.0 {
		t.Fatalf("add key: %d %v", code, out)
	}
	_, out = e.b.do("GET", "/api/offsite/found", nil)
	first := out["backups"].([]any)[1].(map[string]any)
	if out["opens"] != true || first["app"] != "old-db" || first["engine"] != "postgres" || first["locked"] != nil {
		t.Fatalf("found, open: %v", out)
	}
	if _, out := e.b.do("POST", "/api/offsite/found", nil); out["added"] != 2.0 {
		t.Fatalf("add: %v", out)
	}
	if _, out := e.b.do("GET", "/api/offsite/found", nil); len(out["backups"].([]any)) != 0 {
		t.Errorf("found again after adding: %v", out)
	}

	// Listed with the backups of deleted apps, off-site only.
	_, list := e.b.do("GET", "/api/backups/deleted", nil)
	raw, _ := e.s.Store.Backups(context.Background(), "")
	if len(raw) != 2 || raw[0].Reason != store.BackupFound || raw[0].Local || raw[0].Offsite != store.OffsiteDone || raw[0].AppID != "files" ||
		raw[0].Size != 5000 || len(raw[0].Volumes) != 1 {
		t.Fatalf("deleted list: %v %+v", list, raw)
	}

	// A database named like it takes the backup, brought over first.
	e.b.do("POST", "/api/databases", map[string]any{"id": "old-db", "engine": "postgres"})
	e.settle(t, "old-db")
	backups, _ := e.backups(t, "old-db")
	if len(backups) != 1 || backups[0]["reason"] != "found" {
		t.Fatalf("on the new database: %v", backups)
	}
	res := e.restore(t, "old-db", int64(backups[0]["id"].(float64)))
	if res["state"] != "done" || !slices.Contains(e.core.off.fetched, "old-db/20260101T030000Z-aaaa.sql.zst.age") {
		t.Errorf("restore: %v, fetched %v", res, e.core.off.fetched)
	}
}
