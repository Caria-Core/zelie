package core

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"golang.org/x/sys/unix"
)

// VolumeRef names one of an app's volumes and the folder it gets in the
// backup: where the app sees it, without the leading slash.
type VolumeRef struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
}

// diskReserve is left free by backups and restores, so they never fill the
// disk the apps write to.
const diskReserve = 1 << 30

func checkVolumeRefs(vols []VolumeRef) error {
	if len(vols) == 0 {
		return errors.New("no volumes to back up")
	}
	var dirs []string
	for _, v := range vols {
		d := v.Dir
		if d == "" || path.Clean(d) != d || strings.HasPrefix(d, "/") || d == "." || d == ".." || strings.HasPrefix(d, "../") {
			return fmt.Errorf("volume folder %q is not a clean relative path", d)
		}
		for _, o := range dirs {
			if d == o || strings.HasPrefix(d, o+"/") || strings.HasPrefix(o, d+"/") {
				return fmt.Errorf("volume folders %q and %q overlap", o, d)
			}
		}
		dirs = append(dirs, d)
	}
	return nil
}

// freeSpace is what the backups' disk has left for an unprivileged writer.
func (s *Server) freeSpace() (int64, error) {
	if err := os.MkdirAll(s.Backups.Root, 0o700); err != nil {
		return 0, err
	}
	var st unix.Statfs_t
	if err := unix.Statfs(s.Backups.Root, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// roomFor refuses to start when need would leave less than the reserve.
func (s *Server) roomFor(need int64, what string) error {
	free, err := s.freeSpace()
	if err != nil {
		return err
	}
	if free-need < diskReserve {
		return &backup.Error{Msg: fmt.Sprintf("Not enough disk space: %s needs up to %s and %s is free, and Zelie keeps 1 GB free for the apps.",
			what, sizeLabel(need), sizeLabel(free))}
	}
	return nil
}

func sizeLabel(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d KB", (n+1023)/1024)
}

// openVolumes opens every volume, for reading only when live.
func (s *Server) openVolumes(r *http.Request, refs []VolumeRef, live bool) ([]backup.Volume, func(), error) {
	var vols []backup.Volume
	closeAll := func() {
		for _, v := range vols {
			v.Root.Close()
		}
	}
	for _, ref := range refs {
		var root *os.Root
		var err error
		if live {
			root, err = s.Engine.ReadVolume(ref.Name)
		} else {
			root, err = s.Engine.OpenVolume(r.Context(), ref.Name)
		}
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		vols = append(vols, backup.Volume{Dir: ref.Dir, Root: root})
	}
	return vols, closeAll, nil
}

func (s *Server) backUpVolumes(w http.ResponseWriter, r *http.Request, req backupRequest) {
	if err := checkVolumeRefs(req.Volumes); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var need int64
	for _, v := range req.Volumes {
		n, err := s.Engine.VolumeSize(v.Name)
		if err != nil {
			s.backupFailed(w, "back up", req.App, err)
			return
		}
		need += n
	}
	// Compressed, the backup is smaller than the files; counting it at
	// full size errs on the safe side.
	if err := s.roomFor(need, "the backup"); err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	if !s.busy.take(req.App) {
		writeError(w, http.StatusConflict, errBusy)
		return
	}
	defer s.busy.done(req.App)
	vols, closeAll, err := s.openVolumes(r, req.Volumes, req.Live)
	if err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	defer closeAll()
	start := time.Now()
	bw, err := s.Backups.Create(req.App, "tar")
	if err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	st, err := backup.WriteTar(r.Context(), bw, vols)
	if err != nil {
		bw.Abort()
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	info, err := bw.Commit()
	if err != nil {
		s.backupFailed(w, "back up", req.App, err)
		return
	}
	info.Size, info.Changed = st.Bytes, st.Changed
	s.Log.Info("backup made", "app", req.App, "backup", info.Name, "files", st.Files, "size", st.Bytes, "bytes", info.Bytes,
		"changed", st.Changed, "live", req.Live, "took", time.Since(start).Round(time.Millisecond))
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) restoreVolumes(w http.ResponseWriter, r *http.Request, app, name string, req restoreRequest) {
	if err := checkVolumeRefs(req.Volumes); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	// The old files stay until the new ones are all in.
	if err := s.roomFor(req.Size, "the restore"); err != nil {
		s.backupFailed(w, "restore", app, err)
		return
	}
	if !s.busy.take(app) {
		writeError(w, http.StatusConflict, errBusy)
		return
	}
	defer s.busy.done(app)
	src, err := s.Backups.Open(app, name)
	if err != nil {
		s.backupFailed(w, "restore", app, err)
		return
	}
	defer src.Close()
	vols, closeAll, err := s.openVolumes(r, req.Volumes, false)
	if err != nil {
		s.backupFailed(w, "restore", app, err)
		return
	}
	defer closeAll()
	restored, err := backup.RestoreTar(r.Context(), src, vols)
	if err != nil {
		s.backupFailed(w, "restore", app, err)
		return
	}
	skipped := slices.DeleteFunc(slices.Clone(req.Volumes), func(v VolumeRef) bool { return slices.Contains(restored, v.Dir) })
	s.Log.Info("backup restored", "app", app, "backup", name, "volumes", restored, "not in backup", len(skipped))
	w.WriteHeader(http.StatusNoContent)
}
