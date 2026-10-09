package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// An id that belonged to a deleted backup or schedule must not come round
// again: an off-site upload on its way, or a run in progress, holds the old
// id and would write to whatever has it now.
func TestIdsAreNotReused(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	if err := s.CreateApp(ctx, App{ID: "mc", Source: SourceImage, Image: "itzg/minecraft-server", Port: 25565, MemoryMB: 2048, CPUs: 2, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	first, err := s.StartBackup(ctx, Backup{AppID: "mc", Reason: BackupManual, CreatedAt: now, KeepUntil: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteBackup(ctx, first); err != nil {
		t.Fatal(err)
	}
	second, err := s.StartBackup(ctx, Backup{AppID: "mc", Reason: BackupManual, CreatedAt: now, KeepUntil: now})
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Errorf("backup %d got the id of the deleted backup %d", second, first)
	}
	// A late answer for the deleted one finds nothing, not the new backup.
	if err := s.OffsiteSent(ctx, first, now); err != ErrNotFound {
		t.Errorf("off-site copy of a deleted backup: %v", err)
	}
	if b, _ := s.Backup(ctx, second); b.Offsite != "" {
		t.Errorf("the new backup was marked: %+v", b)
	}
	if err := s.DeleteBackup(ctx, second); err != nil {
		t.Fatal(err)
	}
	found, err := s.AddFoundBackup(ctx, Backup{AppID: "mc", File: "20270101T000000Z-abcd.tar.zst.age", CreatedAt: now, OffsiteAt: now, OffsiteUntil: now})
	if err != nil {
		t.Fatal(err)
	}
	if found <= second {
		t.Errorf("found backup %d got the id of the deleted backup %d", found, second)
	}

	task := []ScheduleTask{{Action: "command", Data: "save-all"}}
	one, err := s.CreateSchedule(ctx, Schedule{AppID: "mc", Name: "a", Cron: "0 4 * * *", Enabled: true, CreatedAt: now, LastSlot: now, Tasks: task})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSchedule(ctx, one); err != nil {
		t.Fatal(err)
	}
	two, err := s.CreateSchedule(ctx, Schedule{AppID: "mc", Name: "b", Cron: "0 4 * * *", Enabled: true, CreatedAt: now, LastSlot: now, Tasks: task})
	if err != nil {
		t.Fatal(err)
	}
	if two <= one {
		t.Errorf("schedule %d got the id of the deleted schedule %d", two, one)
	}
}

// Rows made before ids were handed out apart are counted: the first new id
// is above every old one.
func TestIdsContinueFromOldRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "panel.db")
	all := migrations
	defer func() { migrations = all }()
	i := slices.IndexFunc(all, func(m string) bool { return strings.Contains(m, "CREATE TABLE backup_seq") })
	migrations = all[:i]
	old, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	if err := old.CreateApp(ctx, App{ID: "mc", Source: SourceImage, Image: "itzg/minecraft-server", Port: 25565, MemoryMB: 2048, CPUs: 2, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.ExecContext(ctx, "INSERT INTO backups (id, app_id, engine, reason, state, created_at, keep_until) VALUES (40, 'mc', '', 'manual', 'done', 1, 1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.db.ExecContext(ctx, "INSERT INTO schedules (id, app_id, name, cron, created_at) VALUES (7, 'mc', 'a', '0 4 * * *', 1)"); err != nil {
		t.Fatal(err)
	}
	old.Close()

	migrations = all
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if id, err := s.StartBackup(ctx, Backup{AppID: "mc", Reason: BackupManual, CreatedAt: now, KeepUntil: now}); err != nil || id != 41 {
		t.Errorf("first backup after the update: %d, %v", id, err)
	}
	if id, err := s.CreateSchedule(ctx, Schedule{AppID: "mc", Name: "b", Cron: "0 4 * * *", CreatedAt: now, LastSlot: now}); err != nil || id != 8 {
		t.Errorf("first schedule after the update: %d, %v", id, err)
	}
}

func TestHoldBackup(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	day := 24 * time.Hour
	now := time.Unix(1_800_000_000, 0)
	if err := s.CreateApp(ctx, App{ID: "mc", Source: SourceImage, Image: "itzg/minecraft-server", Port: 25565, MemoryMB: 2048, CPUs: 2, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	add := func(reason string, age time.Duration) int64 {
		at := now.Add(-age)
		id, err := s.StartBackup(ctx, Backup{AppID: "mc", Reason: reason, CreatedAt: at, KeepUntil: at.Add(7 * day)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBackup(ctx, id, "x.tar.zst.age", 10, nil, at); err != nil {
			t.Fatal(err)
		}
		return id
	}
	expired := func(id int64) bool {
		list, err := s.ExpiredBackups(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(list, func(b Backup) bool { return b.ID == id })
	}
	old := add(BackupManual, 21*day)
	if expired(old) {
		t.Fatal("the newest backup expired")
	}
	// The safety backup before a restore is newer, so the old one is no longer
	// the newest.
	add(BackupRestore, 0)
	if !expired(old) {
		t.Fatal("setup: the old backup should be due now")
	}
	if err := s.HoldBackup(ctx, old, now.Add(day)); err != nil {
		t.Fatal(err)
	}
	if expired(old) {
		t.Error("a held backup expired")
	}
	// Holding never shortens how long a backup is kept.
	keep := add(BackupManual, day)
	if err := s.HoldBackup(ctx, keep, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Backup(ctx, keep); !b.KeepUntil.Equal(now.Add(-day).Add(7 * day)) {
		t.Errorf("keep until %v", b.KeepUntil)
	}
	if err := s.HoldBackup(ctx, 9999, now); err != ErrNotFound {
		t.Errorf("holding a backup that is not there: %v", err)
	}
}

func TestInterruptedRestores(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Unix(1_800_000_000, 0)
	if got, err := s.TakeInterruptedRestores(ctx); err != nil || len(got) != 0 {
		t.Fatalf("nothing began: %v %v", got, err)
	}
	for _, r := range []struct {
		app    string
		backup int64
	}{{"pg", 3}, {"maria", 5}, {"pg", 6}} {
		if err := s.BeginRestore(ctx, r.app, r.backup, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.EndRestore(ctx, "maria"); err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeInterruptedRestores(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].AppID != "pg" || got[0].BackupID != 6 || !got[0].StartedAt.Equal(now) {
		t.Errorf("interrupted %+v", got)
	}
	if again, _ := s.TakeInterruptedRestores(ctx); len(again) != 0 {
		t.Errorf("taken twice: %+v", again)
	}
}
