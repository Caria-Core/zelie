package store

import (
	"context"
	"time"
)

// Link connects an app to a database.
type Link struct {
	AppID, DBID string
	// Prefix starts the names of the variables the app gets, so one app
	// can have several databases.
	Prefix    string
	CreatedAt time.Time
}

func (s *Store) CreateLink(ctx context.Context, l Link) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO links (app_id, db_id, prefix, created_at) VALUES (?, ?, ?, ?)",
		l.AppID, l.DBID, l.Prefix, l.CreatedAt.Unix())
	return uniqueErr(err)
}

func (s *Store) UpdateLink(ctx context.Context, l Link) error {
	res, err := s.db.ExecContext(ctx, "UPDATE links SET prefix = ? WHERE app_id = ? AND db_id = ?", l.Prefix, l.AppID, l.DBID)
	return oneRow(res, err)
}

func (s *Store) DeleteLink(ctx context.Context, appID, dbID string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM links WHERE app_id = ? AND db_id = ?", appID, dbID)
	return oneRow(res, err)
}

// Links returns the databases an app is linked to, or with appID empty,
// the apps linked to the database dbID.
func (s *Store) Links(ctx context.Context, appID, dbID string) ([]Link, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT app_id, db_id, prefix, created_at FROM links
		WHERE (? = '' OR app_id = ?) AND (? = '' OR db_id = ?) ORDER BY rowid`, appID, appID, dbID, dbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Link
	for rows.Next() {
		var l Link
		var created int64
		if err := rows.Scan(&l.AppID, &l.DBID, &l.Prefix, &created); err != nil {
			return nil, err
		}
		l.CreatedAt = time.Unix(created, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}
