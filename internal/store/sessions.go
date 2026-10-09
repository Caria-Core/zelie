package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a logged-in browser. Hash is the SHA-256 of the cookie value.
type Session struct {
	Hash        []byte
	UserID      int64
	Verified    bool // the second factor has been checked
	CreatedAt   time.Time
	SeenAt      time.Time
	ExpiresAt   time.Time
	ConfirmedAt time.Time // last time the user proved it was them
	IP          string
	Agent       string
}

// CreateSession adds a session, and forgets those that have expired: reads
// only skip them, so without this they would stay for good, each with an
// address and a browser name.
func (s *Store) CreateSession(ctx context.Context, x Session) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", x.CreatedAt.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO sessions (hash, user_id, verified, created_at, seen_at, expires_at, confirmed_at, ip, agent)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			x.Hash, x.UserID, x.Verified, x.CreatedAt.Unix(), x.SeenAt.Unix(), x.ExpiresAt.Unix(), unixOrZero(x.ConfirmedAt), x.IP, x.Agent)
		return err
	})
}

const sessionColumns = "hash, user_id, verified, created_at, seen_at, expires_at, confirmed_at, ip, agent"

type scanner interface{ Scan(dest ...any) error }

func scanSession(row scanner) (Session, error) {
	var x Session
	var created, seen, expires, confirmed int64
	err := row.Scan(&x.Hash, &x.UserID, &x.Verified, &created, &seen, &expires, &confirmed, &x.IP, &x.Agent)
	x.CreatedAt, x.SeenAt, x.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(expires, 0)
	if confirmed != 0 {
		x.ConfirmedAt = time.Unix(confirmed, 0)
	}
	return x, err
}

// Session returns the session with this hash if it has not expired.
func (s *Store) Session(ctx context.Context, hash []byte, now time.Time) (Session, error) {
	x, err := scanSession(s.db.QueryRowContext(ctx,
		"SELECT "+sessionColumns+" FROM sessions WHERE hash = ? AND expires_at > ?", hash, now.Unix()))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// Sessions lists an account's unexpired sessions, most recently used first.
func (s *Store) Sessions(ctx context.Context, userID int64, now time.Time) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT "+sessionColumns+" FROM sessions WHERE user_id = ? AND expires_at > ? ORDER BY seen_at DESC", userID, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// TouchSession records activity and moves the expiry.
func (s *Store) TouchSession(ctx context.Context, hash []byte, seen, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET seen_at = ?, expires_at = ? WHERE hash = ?", seen.Unix(), expires.Unix(), hash)
	return err
}

// VerifySession marks the second factor as checked. Finishing a login also
// counts as confirming.
func (s *Store) VerifySession(ctx context.Context, hash []byte, now, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET verified = 1, confirmed_at = ?, expires_at = ? WHERE hash = ?", now.Unix(), expires.Unix(), hash)
	return err
}

// ConfirmSession records that the user proved it was them again.
func (s *Store) ConfirmSession(ctx context.Context, hash []byte, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET confirmed_at = ? WHERE hash = ?", now.Unix(), hash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE hash = ?", hash)
	return err
}

// DeleteUserSession ends one of an account's sessions. The account is part
// of the query so no one can end another account's session.
func (s *Store) DeleteUserSession(ctx context.Context, userID int64, hash []byte) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND hash = ?", userID, hash)
	return oneRow(res, err)
}

// oneRow turns a statement that changed nothing into ErrNotFound.
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteOtherSessions ends every session of the account except keep.
func (s *Store) DeleteOtherSessions(ctx context.Context, userID int64, keep []byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE user_id = ? AND hash != ?", userID, keep)
	return err
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
