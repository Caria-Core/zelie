package panel

import (
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

var errNoVolumes = msg.Define(http.StatusConflict, "backup.no_volumes", "This app has no volumes, so there is nothing to back up. Add one under Storage.")

// hasVolumes answers 409 for an app that is not a database and has no
// volumes: it keeps nothing between deployments to back up.
func (s *Server) hasVolumes(w http.ResponseWriter, r *http.Request, a store.App) bool {
	if a.IsDatabase() {
		return true
	}
	vols, err := s.Store.Volumes(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "list volumes", err)
		return false
	}
	if len(vols) == 0 {
		writeError(w, errNoVolumes.Err())
		return false
	}
	return true
}

// volumeRefs names an app's volumes for the core, each with the folder it
// gets in a backup: where the app sees it, without the leading slash.
func (s *Server) volumeRefs(ctx context.Context, a store.App) ([]core.VolumeRef, error) {
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	if len(vols) == 0 {
		return nil, errNoVolumes.Err()
	}
	refs := make([]core.VolumeRef, len(vols))
	for i, v := range vols {
		refs[i] = core.VolumeRef{Name: v.Name, Dir: strings.TrimPrefix(v.Path, "/")}
	}
	return refs, nil
}

// takeBackup makes the backup file for makeBackup, which records it.
func (s *Server) takeBackup(ctx context.Context, a store.App, id int64, plan store.BackupPlan, reason string) (core.Backup, error) {
	if a.IsDatabase() {
		container, err := s.liveContainer(ctx, a)
		if err != nil {
			return core.Backup{}, err
		}
		return s.Core.CreateBackup(ctx, a.ID, container, a.Engine)
	}
	// A restore stops the app before its safety backup.
	if reason == store.BackupRestore {
		return s.backUpVolumes(ctx, a, id, false)
	}
	if !plan.Stop {
		defer s.quiesceGame(ctx, a)()
		return s.backUpVolumes(ctx, a, id, true)
	}
	var info core.Backup
	err := s.whileStopped(ctx, a, store.CauseBackup, func() error {
		var err error
		info, err = s.backUpVolumes(ctx, a, id, false)
		return err
	})
	return info, err
}

// backUpVolumes backs up the app's volumes and records what went in. live
// copies them while the app runs.
func (s *Server) backUpVolumes(ctx context.Context, a store.App, id int64, live bool) (core.Backup, error) {
	refs, err := s.volumeRefs(ctx, a)
	if err != nil {
		return core.Backup{}, err
	}
	info, err := s.Core.BackUpVolumes(ctx, a.ID, refs, live)
	if err != nil {
		return info, err
	}
	dirs := make([]string, len(refs))
	for i, r := range refs {
		dirs[i] = r.Dir
	}
	return info, s.Store.SetBackupContents(ctx, id, dirs, info.Size, info.Changed)
}

// stopForBackup stops what runs of the app and reports whether anything
// did. A game server is stopped the way its egg says, so it saves first.
func (s *Server) stopForBackup(ctx context.Context, a store.App) (bool, error) {
	if a.IsGame() {
		_, e, _, err := s.gameParts(ctx, a.ID)
		if err != nil {
			return false, err
		}
		return s.stopGame(ctx, a, e, io.Discard)
	}
	list, err := s.Core.List(ctx)
	if err != nil {
		return false, err
	}
	stopped := false
	for _, c := range list {
		if c.App == a.ID && c.State == "running" {
			stopped = true
			if err := s.Core.Stop(ctx, c.ID, volumeStopGrace); err != nil {
				return stopped, err
			}
		}
	}
	return stopped, nil
}

// whileStopped runs fn with the app's containers stopped, and the
// supervisor and new deployments kept off it. An app that was running is
// started again afterwards, whatever fn did, with cause.
func (s *Server) whileStopped(ctx context.Context, a store.App, cause string, fn func() error) error {
	s.pauses.hold(a.ID)
	defer s.pauses.release(a.ID)
	stopped := false
	err := func() error {
		unlock := s.deploys.lock(a.ID)
		defer unlock()
		var err error
		if stopped, err = s.stopForBackup(ctx, a); err != nil {
			return err
		}
		return fn()
	}()
	if stopped {
		s.startAgain(ctx, a.ID, cause)
	}
	return err
}

// runVolumeRestore stops the app, backs up its volumes as they are, puts
// the backup's files in their place and starts the app again if it was
// running.
func (s *Server) runVolumeRestore(ctx context.Context, a store.App, b store.Backup, user int64) (out restoreJSON) {
	out = restoreJSON{Backup: b.ID, State: "failed", Restarted: []string{}}
	defer func() { out.At = s.now() }()
	err := s.whileStopped(ctx, a, store.CauseRestore, func() error {
		safety, err := s.makeBackup(ctx, a, store.BackupRestore)
		if err != nil {
			out.Error, out.SafetyFailed = new(backupFailure(err)), true
			return err
		}
		out.Safety = &safety.CreatedAt
		refs, err := s.volumeRefs(ctx, a)
		if err != nil {
			return err
		}
		return s.Core.RestoreVolumes(ctx, a.ID, b.File, refs, b.Size)
	})
	if err != nil {
		s.Log.Error("restore failed", "app", a.ID, "backup", b.File, "err", err)
		if out.Error == nil {
			out.Error = new(backupFailure(err))
		}
		return out
	}
	s.Store.SetRestored(ctx, b.ID, s.now())
	s.Log.Info("backup restored", "app", a.ID, "backup", b.File, "user", user)
	out.State = "done"
	return out
}
