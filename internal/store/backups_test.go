package store

import (
	"context"
	"testing"
	"time"
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
		if failure == "" {
			file = "x.sql.zst.age"
		}
		if err := s.FinishBackup(ctx, id, file, 10, failure, at); err != nil {
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
