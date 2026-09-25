package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Session is a logged-in browser. Hash is the SHA-256 of the cookie value.
type Session struct {
	Hash      []byte
	UserID    int64
	Verified  bool // the second factor has been checked
	CreatedAt time.Time
	SeenAt    time.Time
	ExpiresAt time.Time
	IP        string
	Agent     string
}

func (s *Store) CreateSession(ctx context.Context, x Session) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sessions (hash, user_id, verified, created_at, seen_at, expires_at, ip, agent)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		x.Hash, x.UserID, x.Verified, x.CreatedAt.Unix(), x.SeenAt.Unix(), x.ExpiresAt.Unix(), x.IP, x.Agent)
	return err
}

// Session returns the session with this hash if it has not expired.
func (s *Store) Session(ctx context.Context, hash []byte, now time.Time) (Session, error) {
	x := Session{Hash: hash}
	var created, seen, expires int64
	err := s.db.QueryRowContext(ctx, `
SELECT user_id, verified, created_at, seen_at, expires_at, ip, agent
FROM sessions WHERE hash = ? AND expires_at > ?`, hash, now.Unix()).
		Scan(&x.UserID, &x.Verified, &created, &seen, &expires, &x.IP, &x.Agent)
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	x.CreatedAt, x.SeenAt, x.ExpiresAt = time.Unix(created, 0), time.Unix(seen, 0), time.Unix(expires, 0)
	return x, err
}

// TouchSession records activity and moves the expiry.
func (s *Store) TouchSession(ctx context.Context, hash []byte, seen, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET seen_at = ?, expires_at = ? WHERE hash = ?", seen.Unix(), expires.Unix(), hash)
	return err
}

// VerifySession marks the second factor as checked.
func (s *Store) VerifySession(ctx context.Context, hash []byte, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET verified = 1, expires_at = ? WHERE hash = ?", expires.Unix(), hash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, hash []byte) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE hash = ?", hash)
	return err
}

// DeleteExpiredSessions keeps the table from growing forever.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now.Unix())
	return err
}
