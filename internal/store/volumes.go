package store

import (
	"context"
	"strconv"
	"time"
)

// Volume is a directory an app keeps between deployments.
type Volume struct {
	ID        int64
	AppID     string
	Path      string // where the app sees it
	LimitMB   int64
	CreatedAt time.Time
}

// Name is what the core calls the volume.
func (v Volume) Name() string { return "vol-" + strconv.FormatInt(v.ID, 10) }

// CreateVolume saves a new volume and returns its id.
func (s *Store) CreateVolume(ctx context.Context, v Volume) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO volumes (app_id, path, limit_mb, created_at) VALUES (?, ?, ?, ?)",
		v.AppID, v.Path, v.LimitMB, v.CreatedAt.Unix())
	if err != nil {
		return 0, uniqueErr(err)
	}
	return res.LastInsertId()
}

// Volumes returns an app's volumes, oldest first. An empty appID returns
// every app's.
func (s *Store) Volumes(ctx context.Context, appID string) ([]Volume, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, app_id, path, limit_mb, created_at FROM volumes WHERE ? = '' OR app_id = ? ORDER BY id", appID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Volume
	for rows.Next() {
		var v Volume
		var created int64
		if err := rows.Scan(&v.ID, &v.AppID, &v.Path, &v.LimitMB, &created); err != nil {
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
