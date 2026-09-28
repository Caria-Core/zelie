package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ExternalAccess is a database open to desktop tools on a loopback port.
type ExternalAccess struct {
	AppID     string
	Port      int
	CreatedAt time.Time
}

// ExternalAccess returns the database's, or ErrNotFound.
func (s *Store) ExternalAccess(ctx context.Context, appID string) (ExternalAccess, error) {
	var x ExternalAccess
	var created int64
	err := s.db.QueryRowContext(ctx, "SELECT app_id, port, created_at FROM external_access WHERE app_id = ?", appID).Scan(&x.AppID, &x.Port, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.CreatedAt = time.Unix(created, 0)
	return x, err
}

// ExternalAccesses lists every database open to desktop tools.
func (s *Store) ExternalAccesses(ctx context.Context) ([]ExternalAccess, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT app_id, port, created_at FROM external_access ORDER BY port")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExternalAccess
	for rows.Next() {
		var x ExternalAccess
		var created int64
		if err := rows.Scan(&x.AppID, &x.Port, &created); err != nil {
			return nil, err
		}
		x.CreatedAt = time.Unix(created, 0)
		out = append(out, x)
	}
	return out, rows.Err()
}

// SetExternalAccess saves the database's port. ErrExists means another
// database has it.
func (s *Store) SetExternalAccess(ctx context.Context, x ExternalAccess) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO external_access (app_id, port, created_at) VALUES (?, ?, ?) ON CONFLICT (app_id) DO UPDATE SET port = excluded.port",
		x.AppID, x.Port, x.CreatedAt.Unix())
	return uniqueErr(err)
}

func (s *Store) RemoveExternalAccess(ctx context.Context, appID string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM external_access WHERE app_id = ?", appID)
	return err
}
