package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	// ErrSetupDone means an administrator exists and setup is closed for good.
	ErrSetupDone = errors.New("setup is already complete")
	// ErrBadSetupToken means the setup link is wrong, used or expired.
	ErrBadSetupToken = errors.New("this setup link is not valid; run 'zelie setup-link' on the server for a new one")
)

// User is a panel account.
type User struct {
	ID    int64
	Email string
	Admin bool
}

// SetupOpen reports whether the panel still has no users.
func (s *Store) SetupOpen(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n)
	return n == 0, err
}

// SetSetupToken replaces the setup token. Only the hash of the token is
// kept.
func (s *Store) SetSetupToken(ctx context.Context, hash []byte, expires time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := setupOpen(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT OR REPLACE INTO setup_token (one, hash, expires_at) VALUES (1, ?, ?)",
			hash, expires.Unix())
		return err
	})
}

// CheckSetupToken says what CreateFirstAdmin would answer to this token,
// without changing anything, so the caller can refuse before the costly
// work of hashing a password.
func (s *Store) CheckSetupToken(ctx context.Context, tokenHash []byte, now time.Time) error {
	return checkSetupToken(ctx, s.db, tokenHash, now)
}

// CreateFirstAdmin spends the setup token and creates the first
// administrator. Once it succeeds, setup is closed.
func (s *Store) CreateFirstAdmin(ctx context.Context, tokenHash []byte, email, password string, now time.Time) (User, error) {
	u := User{Email: email, Admin: true}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := checkSetupToken(ctx, tx, tokenHash, now); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			"INSERT INTO users (email, password, admin, created_at) VALUES (?, ?, 1, ?)",
			email, password, now.Unix())
		if err != nil {
			return err
		}
		if u.ID, err = res.LastInsertId(); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM setup_token")
		return err
	})
	return u, err
}

// querier is what a *sql.DB and a *sql.Tx share, for the checks that run
// both on their own and inside a transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func setupOpen(ctx context.Context, q querier) error {
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM users").Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrSetupDone
	}
	return nil
}

func checkSetupToken(ctx context.Context, q querier, tokenHash []byte, now time.Time) error {
	if err := setupOpen(ctx, q); err != nil {
		return err
	}
	var n int
	err := q.QueryRowContext(ctx,
		"SELECT count(*) FROM setup_token WHERE hash = ? AND expires_at > ?",
		tokenHash, now.Unix()).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBadSetupToken
	}
	return nil
}
