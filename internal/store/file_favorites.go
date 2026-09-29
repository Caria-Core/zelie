package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
	"unicode/utf8"
)

// MaxFileFavorites is how many paths an account may star in one server.
const MaxFileFavorites = 50

// ErrTooManyFavorites is returned when an account is at MaxFileFavorites.
var ErrTooManyFavorites = errors.New("too many favourites")

// FileFavorites lists an account's starred paths in a server, oldest first.
func (s *Store) FileFavorites(ctx context.Context, userID int64, appID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT path FROM file_favorites WHERE user_id = ? AND app_id = ? ORDER BY created_at, path", userID, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddFileFavorite stars a path. Starring it again changes nothing.
func (s *Store) AddFileFavorite(ctx context.Context, userID int64, appID, path string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var have int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM file_favorites WHERE user_id = ? AND app_id = ? AND path <> ?", userID, appID, path).Scan(&have); err != nil {
			return err
		}
		if have >= MaxFileFavorites {
			return ErrTooManyFavorites
		}
		_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO file_favorites (user_id, app_id, path, created_at) VALUES (?, ?, ?, ?)", userID, appID, path, at.Unix())
		return err
	})
}

// RemoveFileFavorite unstars a path. A path that was not starred is fine.
func (s *Store) RemoveFileFavorite(ctx context.Context, userID int64, appID, path string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM file_favorites WHERE user_id = ? AND app_id = ? AND path = ?", userID, appID, path)
	return err
}

// MoveFileFavorites follows a rename for every account: the path and
// anything below it changes to sit under the new name. A star whose new path
// is already starred is left where it was.
func (s *Store) MoveFileFavorites(ctx context.Context, appID, from, to string) error {
	if from == to || from == "" {
		return nil
	}
	// substr counts characters, not bytes.
	n := utf8.RuneCountInString(from) + 1
	_, err := s.db.ExecContext(ctx, `UPDATE OR IGNORE file_favorites SET path = ? || substr(path, ?)
		WHERE app_id = ? AND (path = ? OR substr(path, 1, ?) = ?)`,
		to, n, appID, from, n, from+"/")
	return err
}
