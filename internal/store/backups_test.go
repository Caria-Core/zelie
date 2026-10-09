package store

import (
	"context"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

func TestBackups(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	day := 24 * time.Hour
	now := time.Unix(1_800_000_000, 0)
	for _, id := range []string{"db", "gone"} {
		if err := s.CreateApp(ctx, App{ID: id, Source: SourceImage, Image: "postgres:18", Port: 5432, MemoryMB: 512, CPUs: 1, Engine: "postgres", CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetBackupPlan(ctx, DefaultBackupPlan(id)); err != nil {
			t.Fatal(err)
		}
	}
	add := func(app string, age time.Duration, failure string) int64 {
		at := now.Add(-age)
		id, err := s.StartBackup(ctx, Backup{AppID: app, Engine: "postgres", Reason: BackupScheduled, CreatedAt: at, KeepUntil: at.Add(7 * day)})
		if err != nil {
			t.Fatal(err)
		}
		file := ""
		var why *msg.Msg
		if failure == "" {
			file = "x.sql.zst.age"
		} else {
			why = new(msg.Other.With("detail", failure))
		}
		if err := s.FinishBackup(ctx, id, file, 10, why, at); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldest := add("db", 20*day, "")
	newest := add("db", 10*day, "") // past its time, but the newest good one
	failed := add("db", 9*day, "the dump failed")
	fresh := add("db", day, "")
	goneOnly := add("gone", 10*day, "")

	expired := func() map[int64]bool {
		list, err := s.ExpiredBackups(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		m := map[int64]bool{}
		for _, b := range list {
			m[b.ID] = true
		}
		return m
	}
	got := expired()
	// fresh is the newest good one now, so newest may go.
	if !got[oldest] || !got[newest] || !got[failed] || got[fresh] || got[goneOnly] {
		t.Errorf("expired %v", got)
	}

	// Keeping for 30 days applies to backups already made.
	p := DefaultBackupPlan("db")
	p.KeepDays = 30
	if err := s.SetBackupPlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := expired(); got[oldest] || got[newest] {
		t.Errorf("after keeping longer: %v", got)
	}
	if p2, _ := s.BackupPlan(ctx, "db"); p2 != p {
		t.Errorf("plan %+v", p2)
	}

	// A deleted database's backups outlive it, and expire like any.
	if err := s.DeleteApp(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	orphans, _ := s.Backups(ctx, "")
	if len(orphans) != 1 || orphans[0].ID != goneOnly {
		t.Errorf("orphans %+v", orphans)
	}
	if _, err := s.BackupPlan(ctx, "gone"); err != ErrNotFound {
		t.Errorf("plan outlived its app: %v", err)
	}
	if got := expired(); !got[goneOnly] {
		t.Errorf("a deleted app's last backup never expires: %v", got)
	}

	// Running backups are not expired, and are failed on restart.
	id, _ := s.StartBackup(ctx, Backup{AppID: "db", Engine: "postgres", Reason: BackupManual, CreatedAt: now.Add(-40 * day), KeepUntil: now.Add(-33 * day)})
	if got := expired(); got[id] {
		t.Error("a running backup expired")
	}
	s.FailUnfinishedBackups(ctx, now)
	if b, _ := s.Backup(ctx, id); b.State != BackupFailed || b.FinishedAt.IsZero() {
		t.Errorf("unfinished backup %+v", b)
	}
	if err := s.SetRestored(ctx, fresh, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Backup(ctx, fresh); !b.RestoredAt.Equal(now) {
		t.Errorf("restored at %v", b.RestoredAt)
	}

	if at, _ := s.RecoverySavedAt(ctx); !at.IsZero() {
		t.Error("recovery saved before it was")
	}
	s.SetRecoverySaved(ctx, now)
	if at, _ := s.RecoverySavedAt(ctx); !at.Equal(now) {
		t.Errorf("recovery saved at %v", at)
	}
}

func TestVolumeBackupRecord(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	if err := s.CreateApp(ctx, App{ID: "mc", Source: SourceImage, Image: "itzg/minecraft-server", Port: 25565, MemoryMB: 2048, CPUs: 2, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	p := AppBackupPlan("mc")
	p.Enabled, p.Stop = true, true
	if err := s.SetBackupPlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.BackupPlan(ctx, "mc"); got != p {
		t.Errorf("plan %+v, want %+v", got, p)
	}
	id, err := s.StartBackup(ctx, Backup{AppID: "mc", Reason: BackupManual, CreatedAt: now, KeepUntil: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackupContents(ctx, id, []string{"data", "srv/plugins"}, 5000, 2); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackup(ctx, id, "x.tar.zst.age", 900, nil, now); err != nil {
		t.Fatal(err)
	}
	b, err := s.Backup(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Volumes) != 2 || b.Volumes[1] != "srv/plugins" || b.Size != 5000 || b.Changed != 2 || b.Engine != "" {
		t.Errorf("backup %+v", b)
	}
}

func TestFailureMessages(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	id, _ := s.StartBackup(ctx, Backup{AppID: "db", Engine: "postgres", Reason: BackupManual, CreatedAt: now, KeepUntil: now})
	// A row from before messages had codes holds only its English text.
	if _, err := s.db.ExecContext(ctx, "UPDATE backups SET state = ?, error = ? WHERE id = ?", BackupFailed, "the dump failed", id); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Backup(ctx, id)
	if b.Error == nil || b.Error.Code != msg.Other.Code || b.Error.Text != "the dump failed" {
		t.Errorf("old row: %+v", b.Error)
	}
	if err := s.FailUnfinishedBackups(ctx, now); err != nil {
		t.Fatal(err)
	}
	id2, _ := s.StartBackup(ctx, Backup{AppID: "db", Engine: "postgres", Reason: BackupManual, CreatedAt: now, KeepUntil: now})
	s.FailUnfinishedBackups(ctx, now)
	b, _ = s.Backup(ctx, id2)
	if b.State != BackupFailed || b.Error == nil || b.Error.Code != "backup.panel_restarted" {
		t.Errorf("unfinished: %+v %+v", b, b.Error)
	}
}

func TestWhatThereIsToDoAboutBackups(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	day := 24 * time.Hour
	now := time.Unix(1_800_000_000, 0)
	never := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.CreateApp(ctx, App{ID: "db", Source: SourceImage, Image: "postgres:18", Port: 5432, MemoryMB: 512, CPUs: 1, Engine: "postgres", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	expirable := func() bool {
		t.Helper()
		ok, err := s.HasExpirable(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	waiting := func() bool {
		t.Helper()
		ok, err := s.OffsiteWaiting(ctx, never)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	start := func() int64 {
		t.Helper()
		id, err := s.StartBackup(ctx, Backup{AppID: "db", Engine: "postgres", Reason: BackupManual, CreatedAt: now, KeepUntil: now.Add(day)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	finish := func(id int64, failure *msg.Msg) {
		t.Helper()
		if err := s.FinishBackup(ctx, id, "x.sql.zst.age", 10, failure, now); err != nil {
			t.Fatal(err)
		}
	}
	if expirable() || waiting() {
		t.Fatal("a database with no backup has something to do")
	}

	id := start()
	if !expirable() {
		t.Error("a backup that is running is not on the clock")
	}
	if waiting() {
		t.Error("a backup that is not made is waiting to be sent")
	}
	finish(id, nil)
	// The newest good backup of an app that exists stays however old it is,
	// so on its own it gives the clock nothing to do.
	if expirable() {
		t.Error("the last good backup of an app is on the clock")
	}
	if waiting() {
		t.Error("a backup that was not queued is waiting to be sent")
	}

	// A newer one takes its place, and the older may go when its time is up.
	second := start()
	finish(second, nil)
	if !expirable() {
		t.Error("a backup that a newer one replaced is not on the clock")
	}
	if err := s.DeleteBackup(ctx, id); err != nil {
		t.Fatal(err)
	}
	if expirable() {
		t.Error("the newest backup is on the clock after the old one went")
	}
	// A failed one is not the last good one.
	failed := start()
	finish(failed, new(msg.Other.With("detail", "the dump failed")))
	if !expirable() {
		t.Error("a failed backup is not on the clock")
	}
	if err := s.DeleteBackup(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if expirable() {
		t.Error("nothing is left to expire")
	}

	// Its backups outlive the app and expire like any.
	if err := s.DeleteApp(ctx, "db"); err != nil {
		t.Fatal(err)
	}
	if !expirable() {
		t.Error("the last backup of a deleted app is not on the clock")
	}
	if err := s.DeleteBackup(ctx, second); err != nil {
		t.Fatal(err)
	}

	// Found off-site and not here: it goes when its time there is up.
	if _, err := s.AddFoundBackup(ctx, Backup{AppID: "elsewhere", Engine: "postgres", File: "x.sql.zst.age", CreatedAt: now, OffsiteAt: now, OffsiteUntil: now.Add(day)}); err != nil {
		t.Fatal(err)
	}
	if !expirable() {
		t.Error("a backup found off-site is not on the clock")
	}
}

func TestOffsiteWaiting(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	never := time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.CreateApp(ctx, App{ID: "db", Source: SourceImage, Image: "postgres:18", Port: 5432, MemoryMB: 512, CPUs: 1, Engine: "postgres", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	waiting := func() bool {
		t.Helper()
		ok, err := s.OffsiteWaiting(ctx, never)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	id, err := s.StartBackup(ctx, Backup{AppID: "db", Engine: "postgres", Reason: BackupManual, CreatedAt: now, KeepUntil: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishBackup(ctx, id, "x.sql.zst.age", 10, nil, now); err != nil {
		t.Fatal(err)
	}
	if waiting() {
		t.Error("a backup that was not queued is waiting")
	}

	// Queued, then failed with another try in a minute, then failed for good.
	if err := s.SendOffsite(ctx, id, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !waiting() {
		t.Error("a queued backup is not waiting")
	}
	if err := s.OffsiteFailedAt(ctx, id, msg.Other.With("detail", "timeout"), 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !waiting() {
		t.Error("a backup to be sent again is not waiting")
	}
	if err := s.OffsiteFailedAt(ctx, id, msg.Other.With("detail", "gone"), 2, never); err != nil {
		t.Fatal(err)
	}
	if waiting() {
		t.Error("a backup that cannot be sent is waiting")
	}
	if err := s.OffsiteFailedAt(ctx, id, msg.Other.With("detail", "timeout"), 1, now); err != nil {
		t.Fatal(err)
	}
	if err := s.OffsiteSent(ctx, id, now); err != nil {
		t.Fatal(err)
	}
	if waiting() {
		t.Error("a backup that was sent is waiting")
	}
}
