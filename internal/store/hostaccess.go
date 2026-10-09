package store

import "context"

// HostAccess reports whether the app may connect to the host's database port.
func (s *Store) HostAccess(ctx context.Context, appID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM host_access WHERE app_id = ?", appID).Scan(&n)
	return n > 0, err
}

// SetHostAccess turns the app's access on or off.
func (s *Store) SetHostAccess(ctx context.Context, appID string, on bool) error {
	q := "DELETE FROM host_access WHERE app_id = ?"
	if on {
		q = "INSERT OR IGNORE INTO host_access (app_id) VALUES (?)"
	}
	_, err := s.db.ExecContext(ctx, q, appID)
	return err
}

// HostAccesses lists the apps with host access.
func (s *Store) HostAccesses(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT app_id FROM host_access ORDER BY app_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
