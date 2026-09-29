package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"time"
)

// Volume is a directory an app keeps between deployments.
type Volume struct {
	ID        int64
	Name      string // the core's name for it, set when it is created
	AppID     string
	Path      string // where the app sees it
	LimitMB   int64
	CreatedAt time.Time
}

// CreateVolume saves a new volume under a new name for the core.
func (s *Store) CreateVolume(ctx context.Context, v Volume) (Volume, error) {
	b := make([]byte, 8)
	rand.Read(b)
	v.Name = "vol-" + hex.EncodeToString(b)
	res, err := s.db.ExecContext(ctx, "INSERT INTO volumes (name, app_id, path, limit_mb, created_at) VALUES (?, ?, ?, ?, ?)",
		v.Name, v.AppID, v.Path, v.LimitMB, v.CreatedAt.Unix())
	if err != nil {
		return v, uniqueErr(err)
	}
	v.ID, err = res.LastInsertId()
	return v, err
}

// Volumes returns an app's volumes, oldest first. An empty appID returns
// every app's.
func (s *Store) Volumes(ctx context.Context, appID string) ([]Volume, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, app_id, path, limit_mb, created_at FROM volumes WHERE ? = '' OR app_id = ? ORDER BY id", appID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		var created int64
		if err := rows.Scan(&v.ID, &v.Name, &v.AppID, &v.Path, &v.LimitMB, &created); err != nil {
			return nil, err
		}
		v.CreatedAt = time.Unix(created, 0)
		out = append(out, v)
	}
	return out, rows.Err()
}

// UpdateVolume changes where the app sees a volume and how large it may
// grow.
func (s *Store) UpdateVolume(ctx context.Context, v Volume) error {
	res, err := s.db.ExecContext(ctx, "UPDATE volumes SET path = ?, limit_mb = ? WHERE id = ? AND app_id = ?", v.Path, v.LimitMB, v.ID, v.AppID)
	return oneRow(res, uniqueErr(err))
}

func (s *Store) DeleteVolume(ctx context.Context, appID string, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM volumes WHERE id = ? AND app_id = ?", id, appID)
	return oneRow(res, err)
}

// KeptVolume is a database's volume from before an upgrade.
type KeptVolume struct {
	ID      int64
	Name    string
	AppID   string
	Path    string
	LimitMB int64
	Version string // the engine version the files belong to
	KeptAt  time.Time
}

// KeepVolume takes v off its app and keeps it as the data of version.
func (s *Store) KeepVolume(ctx context.Context, v Volume, version string, now time.Time) (KeptVolume, error) {
	k := KeptVolume{Name: v.Name, AppID: v.AppID, Path: v.Path, LimitMB: v.LimitMB, Version: version, KeptAt: now}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM volumes WHERE id = ? AND app_id = ?", v.ID, v.AppID)
		if err := oneRow(res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, "INSERT INTO kept_volumes (name, app_id, path, limit_mb, version, kept_at) VALUES (?, ?, ?, ?, ?, ?)",
			k.Name, k.AppID, k.Path, k.LimitMB, k.Version, now.Unix())
		if err != nil {
			return err
		}
		k.ID, err = res.LastInsertId()
		return err
	})
	return k, err
}

// UnkeepVolume gives a kept volume back to its app, which must have no
// other volume at its path.
func (s *Store) UnkeepVolume(ctx context.Context, k KeptVolume, now time.Time) (Volume, error) {
	v := Volume{Name: k.Name, AppID: k.AppID, Path: k.Path, LimitMB: k.LimitMB, CreatedAt: now}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "DELETE FROM kept_volumes WHERE id = ? AND app_id = ?", k.ID, k.AppID)
		if err := oneRow(res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, "INSERT INTO volumes (name, app_id, path, limit_mb, created_at) VALUES (?, ?, ?, ?, ?)",
			v.Name, v.AppID, v.Path, v.LimitMB, now.Unix())
		if err != nil {
			return uniqueErr(err)
		}
		v.ID, err = res.LastInsertId()
		return err
	})
	return v, err
}

// KeptVolumes lists an app's kept volumes, newest first; appID "" lists
// every app's.
func (s *Store) KeptVolumes(ctx context.Context, appID string) ([]KeptVolume, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, app_id, path, limit_mb, version, kept_at FROM kept_volumes WHERE ? = '' OR app_id = ? ORDER BY id DESC", appID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeptVolume
	for rows.Next() {
		var k KeptVolume
		var kept int64
		if err := rows.Scan(&k.ID, &k.Name, &k.AppID, &k.Path, &k.LimitMB, &k.Version, &kept); err != nil {
			return nil, err
		}
		k.KeptAt = time.Unix(kept, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Store) DeleteKeptVolume(ctx context.Context, appID string, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM kept_volumes WHERE id = ? AND app_id = ?", id, appID)
	return oneRow(res, err)
}
