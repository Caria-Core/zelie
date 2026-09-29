package store

import (
	"context"
	"database/sql"
	"time"
)

// AddFailure records a failed attempt, and forgets those of kind made at
// keepSince or earlier.
func (s *Store) AddFailure(ctx context.Context, kind string, key []byte, at, keepSince time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM login_failures WHERE kind = ? AND at <= ?", kind, keepSince.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO login_failures (kind, key, at) VALUES (?, ?, ?)", kind, key, at.Unix())
		return err
	})
}

// Failures counts the failed attempts of kind and key after a time, and
// returns when the oldest of them was.
func (s *Store) Failures(ctx context.Context, kind string, key []byte, since time.Time) (int, time.Time, error) {
	var n int
	var oldest sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT count(*), min(at) FROM login_failures WHERE kind = ? AND key = ? AND at > ?", kind, key, since.Unix()).Scan(&n, &oldest)
	return n, time.Unix(oldest.Int64, 0), err
}

// ResetFailures forgets the failed attempts of kind and key.
func (s *Store) ResetFailures(ctx context.Context, kind string, key []byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM login_failures WHERE kind = ? AND key = ?", kind, key)
	return err
}

// ResetAllFailures forgets every failed attempt, for an owner who got
// back in from the server.
func (s *Store) ResetAllFailures(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM login_failures")
	return err
}
