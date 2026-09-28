package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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

func (c *appCore) UploadBackup(_ context.Context, app, name string) error {
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
	return nil
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
