package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

var (
	errUpgradeVersion  = msg.Define(http.StatusBadRequest, "upgrade.bad_version", "{engine} {version} is not a newer version Zelie can move this database to.")
	errUpgradeNotDB    = msg.Define(http.StatusConflict, "upgrade.not_database", "Only databases are upgraded this way.")
	errUpgradeDBFirst  = msg.Define(http.StatusConflict, "upgrade.start_db_first", "Start the database first: its data is dumped for the new version.")
	errUpgradeNoRoom   = msg.Define(http.StatusConflict, "upgrade.no_room", "The server has {free} free and the upgrade needs about {need}: the old data stays until you remove it.")
	errUpgradeBackup   = msg.Define(http.StatusInternalServerError, "upgrade.backup_failed", "The backup before the upgrade failed, so nothing was changed: {detail}")
	errUpgradeLoad     = msg.Define(http.StatusInternalServerError, "upgrade.load_failed", "The data did not load into the new version, so the old one runs again with its own files: {detail}")
	errUpgradeKeptGone = msg.Define(http.StatusNotFound, "upgrade.no_kept", "There is no such kept volume.")
)

type upgradeRequest struct {
	Version string `json:"version"`
}

// upgradeTo returns the newest version of a database's engine when it is
// newer than the one it runs. The engine lists its versions newest first.
func upgradeTo(a store.App) string {
	e, ok := engineOf(a)
	if !ok || !backup.LoadsInPlace(a.Engine) {
		return ""
	}
	if i := slices.Index(e.Versions, a.EngineVersion); i > 0 {
		return e.Versions[0]
	}
	return ""
}

// upgradeDatabase moves a database to a new major version of its engine,
// whose files the old one cannot read. It runs as a deployment, whose log
// tells each step.
func (s *Server) upgradeDatabase(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	e, isDB := engineOf(a)
	if !isDB {
		writeError(w, errUpgradeNotDB.Err())
		return
	}
	var req upgradeRequest
	if !decode(w, r, &req) {
		return
	}
	if to := upgradeTo(a); to == "" || req.Version != to {
		writeError(w, errUpgradeVersion.Err("engine", e.Label, "version", req.Version))
		return
	}
	if a.Stopped {
		writeError(w, errUpgradeDBFirst.Err())
		return
	}
	if err := s.upgradeRoom(r.Context(), a); err != nil {
		s.failWith(w, "check disk", err)
		return
	}
	id, err := s.deploy(r.Context(), a, store.Deployment{Cause: store.CauseUpgrade, Version: e.Name + ":" + req.Version})
	if err != nil {
		s.fail(w, "deploy", err)
		return
	}
	s.Log.Info("database upgrade", "database", a.ID, "from", a.EngineVersion, "to", req.Version, "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// upgradeRoom checks the disk holds the data a second time, with room for
// the dump: the old files stay while the new version gets its own.
func (s *Server) upgradeRoom(ctx context.Context, a store.App) error {
	vols, err := s.Store.Volumes(ctx, a.ID)
	if err != nil {
		return err
	}
	var used int64
	for _, v := range vols {
		n, _ := s.sizes.get(v.Name)
		used += n
	}
	h, err := s.Core.Host(ctx)
	if err != nil {
		return err
	}
	need := used*2 + 256<<20
	if h.DiskFreeBytes < need {
		return errUpgradeNoRoom.Err("free", formatMB(h.DiskFreeBytes>>20), "need", formatMB(need>>20))
	}
	return nil
}

// runUpgrade does the upgrade for runDeployment, which holds the
// database's deploy lock: a dump of the old version, the new version on a
// new volume, the dump loaded into it. Until the new version has its data,
// any failure puts the old volume and image back. The old volume is kept
// afterwards, until the user removes it.
func (s *Server) runUpgrade(ctx context.Context, db store.App, d *store.Deployment, out io.Writer) error {
	e, _ := engineOf(db)
	to := strings.TrimPrefix(d.Version, e.Name+":")
	from := db.EngineVersion
	vols, err := s.Store.Volumes(ctx, db.ID)
	if err != nil {
		return err
	}
	if len(vols) != 1 {
		return fmt.Errorf("%s has %d volumes, not one", db.ID, len(vols))
	}
	old := vols[0]

	fmt.Fprintf(out, "Upgrading %s %s to %s.\nBacking up the database first.\n", e.Label, from, to)
	if !s.backupBusy.take(db.ID) {
		return errBackupBusy.Err()
	}
	defer s.backupBusy.done(db.ID)
	b, err := s.makeBackup(ctx, db, store.BackupUpgrade)
	if err != nil {
		return errUpgradeBackup.Err("detail", err.Error())
	}
	fmt.Fprintf(out, "Backed up (%d bytes). It is on the Backups tab and loads into the new version.\n", b.Bytes)

	// The linked apps stop while the database is away, as for a restore,
	// and start again at the end, whatever happens.
	links, err := s.Store.Links(ctx, "", db.ID)
	if err != nil {
		return err
	}
	apps := []string{}
	for _, l := range links {
		apps = append(apps, l.AppID)
	}
	slices.Sort(apps)
	s.pauses.hold(db.ID)
	defer s.pauses.release(db.ID)
	for _, id := range apps {
		s.pauses.hold(id)
		defer s.pauses.release(id)
		unlock := s.deploys.lock(id)
		defer unlock()
	}
	var stopped []string
	defer func() {
		for _, id := range stopped {
			s.startAgain(context.WithoutCancel(ctx), id, store.CauseRestore)
		}
	}()
	for _, id := range apps {
		ok, err := s.stopRunning(ctx, id, io.Discard)
		if err != nil {
			return err
		}
		if ok {
			fmt.Fprintf(out, "Stopped %s, which is linked to it.\n", id)
			stopped = append(stopped, id)
		}
	}
	if _, err := s.stopRunning(ctx, db.ID, io.Discard); err != nil {
		return errStopOld.Err("detail", err.Error())
	}
	fmt.Fprintf(out, "Stopped %s %s. Its files stay as they are.\n", e.Label, from)

	// From here on a failure puts everything back.
	kept, err := s.Store.KeepVolume(ctx, old, from, s.now())
	if err != nil {
		return err
	}
	var fresh store.Volume
	container := fmt.Sprintf("%s-%d", db.ID, d.ID)
	undo := func() {
		ctx := context.WithoutCancel(ctx)
		fmt.Fprintf(out, "\nPutting %s %s back.\n", e.Label, from)
		s.Core.Remove(ctx, container)
		if fresh.ID != 0 {
			s.Core.RemoveVolume(ctx, fresh.Name)
			s.Store.DeleteVolume(ctx, db.ID, fresh.ID)
		}
		v, err := s.Store.UnkeepVolume(ctx, kept, s.now())
		if err == nil {
			s.wakeVolumes()
			err = s.Store.SetEngineVersion(ctx, db.ID, e.Name+":"+from, from)
		}
		if err != nil {
			fmt.Fprintf(out, "Could not put it back: %v\n", err)
			s.Log.Error("upgrade: undo", "database", db.ID, "err", err)
			return
		}
		db.Image, db.EngineVersion = e.Name+":"+from, from
		s.restoreLive(ctx, db, []store.Volume{v}, out)
	}

	fresh, err = s.createVolume(ctx, db, volumeRequest{Path: &old.Path, LimitMB: &old.LimitMB})
	if err != nil {
		fresh = store.Volume{}
		undo()
		return err
	}
	if err := s.Store.SetEngineVersion(ctx, db.ID, d.Version, to); err != nil {
		undo()
		return err
	}
	db.Image, db.EngineVersion = d.Version, to
	fmt.Fprintf(out, "Starting %s %s on a new volume.\n", e.Label, to)
	d.Image, err = s.start(ctx, db, d.Version, container, []store.Volume{fresh}, out)
	if err != nil {
		undo()
		return err
	}
	fmt.Fprintln(out, "Loading the data into it.")
	if err := s.Core.RestoreBackup(ctx, db.ID, b.File, db.Engine, container, ""); err != nil {
		undo()
		return errUpgradeLoad.Err("detail", err.Error())
	}
	d.Settings = new(db.RunSettings())
	if err := s.goLive(ctx, *d); err != nil {
		undo()
		return err
	}
	fmt.Fprintf(out, "%s %s is live with the data. %s %s's files are kept on the Storage tab until you remove them.\n", e.Label, to, e.Label, from)
	s.removeOldContainers(ctx, db.ID, container, out)
	s.imageUpdates.set(db.ID, nil)

	// The user for desktop tools lived in the old files. Its password was
	// shown once, so outside access is turned off rather than left open
	// with no way in.
	if _, err := s.Store.ExternalAccess(ctx, db.ID); err == nil {
		s.externalMu.Lock()
		err := s.Core.RemoveExternal(ctx, db.ID, db.Engine, "")
		if err == nil {
			err = s.Store.RemoveExternalAccess(ctx, db.ID)
		}
		s.externalMu.Unlock()
		if err != nil {
			s.Log.Error("upgrade: turn off outside access", "database", db.ID, "err", err)
		} else {
			fmt.Fprintln(out, "Outside access was turned off: its user was in the old files. Turn it on again for a new password.")
		}
	}
	return nil
}

type keptVolumeJSON struct {
	ID        int64  `json:"id"`
	Version   string `json:"version"`
	UsedBytes *int64 `json:"used_bytes"`
	KeptAt    any    `json:"kept_at"`
}

func (s *Server) listKeptVolumes(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	list, err := s.Store.KeptVolumes(r.Context(), a.ID)
	if err != nil {
		s.fail(w, "list kept volumes", err)
		return
	}
	out := make([]keptVolumeJSON, 0, len(list))
	for _, k := range list {
		j := keptVolumeJSON{ID: k.ID, Version: k.Version, KeptAt: k.KeptAt}
		if n, ok := s.sizes.get(k.Name); ok {
			j.UsedBytes = &n
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteKeptVolume removes the files of a version a database has been
// upgraded from, for good.
func (s *Server) deleteKeptVolume(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	ctx := context.WithoutCancel(r.Context())
	list, err := s.Store.KeptVolumes(ctx, a.ID)
	if err != nil {
		s.fail(w, "list kept volumes", err)
		return
	}
	i := slices.IndexFunc(list, func(k store.KeptVolume) bool { return fmt.Sprint(k.ID) == r.PathValue("id") })
	if i < 0 {
		writeError(w, errUpgradeKeptGone.Err())
		return
	}
	k := list[i]
	if err := s.Core.RemoveVolume(ctx, k.Name); err != nil && !isNotFound(err) {
		s.coreFailed(w, "remove volume", err)
		return
	}
	if err := s.Store.DeleteKeptVolume(ctx, a.ID, k.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, "delete kept volume", err)
		return
	}
	s.Log.Info("kept volume removed", "database", a.ID, "version", k.Version, "user", loginFrom(ctx).account.ID)
	w.WriteHeader(http.StatusNoContent)
}
