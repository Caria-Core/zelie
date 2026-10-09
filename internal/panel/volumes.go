package panel

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// Volume limits are checked by measuring, not enforced by the file system:
// an app can go over for as long as one measurement takes. Tests
// shorten the interval.
var volumeCheckEvery = time.Minute

const (
	maxVolumes      = 8
	minVolumeMB     = 64
	defaultVolumeMB = 1024
	// A new version of an app with volumes starts only after the old one
	// has stopped. This is how long the old one gets to save its state.
	volumeStopGrace = 30
)

// volumeSizes is what the last measurement found, by core volume name.
type volumeSizes struct {
	mu    sync.Mutex
	bytes map[string]int64
	at    time.Time
}

func (v *volumeSizes) get(name string) (int64, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	n, ok := v.bytes[name]
	return n, ok
}

func (v *volumeSizes) set(m map[string]int64, at time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.bytes, v.at = m, at
}

var (
	errVolumeFull      = msg.Define(http.StatusConflict, "volume.full", "The volume at {path} holds {used}, over its {limit} limit. Raise the limit to start the app.")
	errVolumePath      = msg.Define(http.StatusBadRequest, "volume.bad_path", "The path must start with / and name a directory such as /data. /proc, /sys and /dev are taken.")
	errVolumeBadChars  = msg.Define(http.StatusBadRequest, "volume.bad_chars", "That path cannot hold a volume.")
	errVolumeSmall     = msg.Define(http.StatusBadRequest, "volume.small", "A volume's limit must be at least {min} MB.")
	errVolumeOverDisk  = msg.Define(http.StatusBadRequest, "volume.over_disk", "The limit is larger than the server's disk ({disk}).")
	errVolumeNoPath    = msg.Define(http.StatusBadRequest, "volume.no_path", "A volume needs a path.")
	errVolumeTooMany   = msg.Define(http.StatusBadRequest, "volume.too_many", "An app can have at most {max} volumes.")
	errVolumeOverlaps  = msg.Define(http.StatusConflict, "volume.overlaps", "{path} overlaps the volume at {other}.")
	errVolumeExists    = msg.Define(http.StatusConflict, "volume.exists", "There is already a volume at {path}.")
	errVolumeDatabase  = msg.Define(http.StatusBadRequest, "volume.database_one", "A database keeps its files in the one volume it has.")
	errVolumeDBPath    = msg.Define(http.StatusBadRequest, "volume.database_path", "A database's volume stays where its engine keeps its files.")
	errVolumeDBDelete  = msg.Define(http.StatusBadRequest, "volume.database_delete", "A database's volume goes when the database is deleted.")
	errVolumeFilesMain = msg.Define(http.StatusConflict, "volume.files_main", "This volume holds the app's files, which the file manager and the startup use, so it stays where it is.")
	errVolumeStopFirst = msg.Define(http.StatusConflict, "volume.stop_first", "Stop the app before deleting one of its volumes.")
	errNoVolume        = msg.Define(http.StatusNotFound, "volume.not_found", "There is no such volume.")
)

type volumeJSON struct {
	ID      int64  `json:"id"`
	Path    string `json:"path"`
	LimitMB int64  `json:"limit_mb"`
	// UsedBytes is nil until the volume has been measured.
	UsedBytes *int64    `json:"used_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type volumeRequest struct {
	Path    *string `json:"path"`
	LimitMB *int64  `json:"limit_mb"`
}

func (s *Server) volumeOut(v store.Volume) volumeJSON {
	out := volumeJSON{ID: v.ID, Path: v.Path, LimitMB: v.LimitMB, CreatedAt: v.CreatedAt}
	if n, ok := s.sizes.get(v.Name); ok {
		out.UsedBytes = &n
	}
	return out
}

// overLimit returns why the app may not run: a volume that has grown past
// its limit. Empty means none has.
func (s *Server) overLimit(vols []store.Volume) *msg.Error {
	for _, v := range vols {
		if n, ok := s.sizes.get(v.Name); ok && n > v.LimitMB<<20 {
			return errVolumeFull.Err("path", v.Path, "used", formatMB(n>>20), "limit", formatMB(v.LimitMB))
		}
	}
	return nil
}

func formatMB(mb int64) string {
	if mb >= 1024 && mb%1024 == 0 {
		return strconv.FormatInt(mb/1024, 10) + " GB"
	}
	if mb >= 1024 {
		return strconv.FormatFloat(float64(mb)/1024, 'f', 1, 64) + " GB"
	}
	return strconv.FormatInt(mb, 10) + " MB"
}

// checkVolume validates what the user asked for, against the server's disk.
// Its error is a *msg.Error for the user, or the core's failure.
func (s *Server) checkVolume(ctx context.Context, v store.Volume) error {
	if err := engine.CheckVolumeTarget(v.Path); err != nil {
		return errVolumePath.Err()
	}
	if len(v.Path) > 255 || strings.ContainsAny(v.Path, ",:\x00") {
		return errVolumeBadChars.Err()
	}
	if v.LimitMB < minVolumeMB {
		return errVolumeSmall.Err("min", minVolumeMB)
	}
	h, err := s.Core.Host(ctx)
	if err != nil {
		return err
	}
	if v.LimitMB > h.DiskBytes>>20 {
		return errVolumeOverDisk.Err("disk", formatMB(h.DiskBytes>>20))
	}
	return nil
}

func (s *Server) listVolumes(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	vols, err := s.Store.Volumes(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "list volumes", err)
		return
	}
	out := make([]volumeJSON, 0, len(vols))
	for _, v := range vols {
		out = append(out, s.volumeOut(v))
	}
	writeJSON(w, http.StatusOK, out)
}

// addVolume makes a new, empty volume. The app sees it from its next start.
func (s *Server) addVolume(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	if a.IsDatabase() {
		writeError(w, errVolumeDatabase.Err())
		return
	}
	var req volumeRequest
	if !decode(w, r, &req) {
		return
	}
	v, err := s.createVolume(r.Context(), a, req)
	if err != nil {
		s.failWith(w, "create volume", err)
		return
	}
	s.Log.Info("volume added", "app", a.ID, "path", v.Path, "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusCreated, s.volumeOut(v))
}

// createVolume records a volume and has the core make it. Its error is a
// *msg.Error for the user, the core's failure, or the panel's own.
func (s *Server) createVolume(ctx context.Context, a store.App, req volumeRequest) (store.Volume, error) {
	v := store.Volume{AppID: a.ID, LimitMB: defaultVolumeMB, CreatedAt: s.now()}
	if req.Path == nil {
		return v, errVolumeNoPath.Err()
	}
	v.Path = strings.TrimSpace(*req.Path)
	if req.LimitMB != nil {
		v.LimitMB = *req.LimitMB
	}
	if err := s.checkVolume(ctx, v); err != nil {
		return v, err
	}
	existing, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return v, err
	}
	if len(existing) >= maxVolumes {
		return v, errVolumeTooMany.Err("max", maxVolumes)
	}
	for _, e := range existing {
		if strings.HasPrefix(v.Path+"/", e.Path+"/") || strings.HasPrefix(e.Path+"/", v.Path+"/") {
			return v, errVolumeOverlaps.Err("path", v.Path, "other", e.Path)
		}
	}
	v, err = s.Store.CreateVolume(ctx, v)
	if errors.Is(err, store.ErrExists) {
		return v, errVolumeExists.Err("path", v.Path)
	}
	if err != nil {
		return v, err
	}
	if err := s.Core.CreateVolume(ctx, v.Name); err != nil {
		s.Store.DeleteVolume(context.WithoutCancel(ctx), a.ID, v.ID)
		return v, err
	}
	s.wakeVolumes()
	return v, nil
}

// updateVolume changes a volume's path or limit. A new path is used from
// the app's next start.
func (s *Server) updateVolume(w http.ResponseWriter, r *http.Request) {
	a, v, ok := s.volumeFrom(w, r)
	if !ok {
		return
	}
	var req volumeRequest
	if !decode(w, r, &req) {
		return
	}
	if req.Path != nil {
		if a.IsDatabase() {
			writeError(w, errVolumeDBPath.Err())
			return
		}
		if a.RunsEgg() && v.Path == gameVolumePath {
			writeError(w, errVolumeFilesMain.Err())
			return
		}
		v.Path = strings.TrimSpace(*req.Path)
	}
	if req.LimitMB != nil {
		v.LimitMB = *req.LimitMB
	}
	if err := s.checkVolume(r.Context(), v); err != nil {
		s.failWith(w, "check volume", err)
		return
	}
	others, err := s.Store.Volumes(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "list volumes", err)
		return
	}
	for _, e := range others {
		if e.ID != v.ID && (strings.HasPrefix(v.Path+"/", e.Path+"/") || strings.HasPrefix(e.Path+"/", v.Path+"/")) {
			writeError(w, errVolumeOverlaps.Err("path", v.Path, "other", e.Path))
			return
		}
	}
	if err := s.Store.UpdateVolume(r.Context(), v); err != nil {
		s.fail(w, "update volume", err)
		return
	}
	writeJSON(w, http.StatusOK, s.volumeOut(v))
}

// deleteVolume deletes a volume and its files, for good. The app must be
// stopped: a running app would lose its files mid-write.
func (s *Server) deleteVolume(w http.ResponseWriter, r *http.Request) {
	a, v, ok := s.volumeFrom(w, r)
	if !ok {
		return
	}
	if a.IsDatabase() {
		writeError(w, errVolumeDBDelete.Err())
		return
	}
	if a.RunsEgg() && v.Path == gameVolumePath {
		writeError(w, errVolumeFilesMain.Err())
		return
	}
	// Finish once started, like deleting an app.
	ctx := context.WithoutCancel(r.Context())
	unlock := s.deploys.lock(a.ID)
	defer unlock()
	list, err := s.Core.List(ctx)
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	for _, c := range list {
		if c.App == a.ID && c.State != "stopped" {
			writeError(w, errVolumeStopFirst.Err())
			return
		}
	}
	// Stopped containers still hold the volume. The app's next start makes
	// a new one anyway.
	for _, c := range list {
		if c.App == a.ID {
			if err := s.Core.Remove(ctx, c.ID); err != nil {
				s.coreFailed(w, "remove container", err)
				return
			}
		}
	}
	if err := s.Core.RemoveVolume(ctx, v.Name); err != nil && !isNotFound(err) {
		s.coreFailed(w, "remove volume", err)
		return
	}
	if err := s.Store.DeleteVolume(ctx, a.ID, v.ID); err != nil {
		s.fail(w, "delete volume", err)
		return
	}
	s.Log.Info("volume deleted", "app", a.ID, "path", v.Path, "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// removeAppVolumes deletes every volume of an app whose containers are
// gone, as the app itself is deleted.
func (s *Server) removeAppVolumes(ctx context.Context, appID string) error {
	vols, err := s.Store.Volumes(ctx, appID)
	if err != nil {
		return err
	}
	kept, err := s.Store.KeptVolumes(ctx, appID)
	if err != nil {
		return err
	}
	names := []string{}
	for _, v := range vols {
		names = append(names, v.Name)
	}
	for _, k := range kept {
		names = append(names, k.Name)
	}
	for _, name := range names {
		if err := s.Core.RemoveVolume(ctx, name); err != nil && !isNotFound(err) {
			return err
		}
	}
	return nil
}

func (s *Server) volumeFrom(w http.ResponseWriter, r *http.Request) (store.App, store.Volume, bool) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return a, store.Volume{}, false
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	vols, err := s.Store.Volumes(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "list volumes", err)
		return a, store.Volume{}, false
	}
	for _, v := range vols {
		if v.ID == id {
			return a, v, true
		}
	}
	writeError(w, errNoVolume.Err())
	return a, store.Volume{}, false
}

func volumeMounts(vols []store.Volume) []engine.VolumeMount {
	out := make([]engine.VolumeMount, len(vols))
	for i, v := range vols {
		out[i] = engine.VolumeMount{Name: v.Name, Target: v.Path}
	}
	return out
}

// wakeVolumes starts the volume check, which runs while a volume exists.
func (s *Server) wakeVolumes() {
	s.loops.wake("volumes", volumeCheckEvery, nil, s.checkVolumes)
}

// checkVolumes measures the volumes, which it does now and then, and stops
// an app whose volume has grown past its limit. It returns false when there
// is no volume to measure.
func (s *Server) checkVolumes(ctx context.Context) bool {
	vols, err := s.Store.Volumes(ctx, "")
	if err != nil {
		s.Log.Error("volumes: list", "err", err)
		return true
	}
	if len(vols) == 0 {
		return false
	}
	sizes, err := s.Core.VolumeSizes(ctx)
	if err != nil {
		s.Log.Error("volumes: measure", "err", err)
		return true
	}
	s.sizes.set(sizes, s.now())
	byApp := map[string][]store.Volume{}
	for _, v := range vols {
		byApp[v.AppID] = append(byApp[v.AppID], v)
	}
	for appID, vols := range byApp {
		over := s.overLimit(vols)
		if over == nil {
			continue
		}
		a, err := s.Store.App(ctx, appID)
		if err != nil || a.Stopped {
			continue
		}
		s.Log.Warn("volume over its limit, stopping the app", "app", appID, "reason", over.Text)
		if err := s.stopApp(ctx, a); err != nil {
			s.Log.Error("volumes: stop app", "app", appID, "err", err)
		}
	}
	return true
}
