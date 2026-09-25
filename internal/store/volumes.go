package store

import (
	"context"
	"crypto/rand"
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
