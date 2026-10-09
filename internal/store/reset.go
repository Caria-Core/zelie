package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var (
	// ErrWhichAccount means there is more than one administrator, so the
	// reset needs an email.
	ErrWhichAccount = errors.New("there is more than one administrator; name one by email")
	// ErrBadResetToken means the reset link is wrong, used or expired.
	ErrBadResetToken = errors.New("this reset link is not valid")
)

// SetResetToken replaces the reset token with one for the account with
// email, or for the only administrator when email is empty.
func (s *Store) SetResetToken(ctx context.Context, email string, hash []byte, expires time.Time) (User, error) {
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var rows *sql.Rows
		var err error
		if email != "" {
			rows, err = tx.QueryContext(ctx, "SELECT id, email, admin FROM users WHERE email = ?", email)
		} else {
			rows, err = tx.QueryContext(ctx, "SELECT id, email, admin FROM users WHERE admin = 1")
		}
		if err != nil {
			return err
		}
		var found []User
		for rows.Next() {
			var x User
			if err := rows.Scan(&x.ID, &x.Email, &x.Admin); err != nil {
				rows.Close()
				return err
			}
			found = append(found, x)
		}
		rows.Close()
		switch {
		case len(found) == 0:
			return ErrNotFound
		case len(found) > 1:
			return ErrWhichAccount
		}
		u = found[0]
		_, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO reset_token (one, user_id, hash, expires_at) VALUES (1, ?, ?, ?)", u.ID, hash, expires.Unix())
		return err
	})
	return u, err
}

// CheckResetToken says whether ResetLogin would take this token, without
// changing anything, so the caller can refuse before hashing a password.
func (s *Store) CheckResetToken(ctx context.Context, tokenHash []byte, now time.Time) error {
	_, err := resetTokenUser(ctx, s.db, tokenHash, now)
	return err
}

func resetTokenUser(ctx context.Context, q querier, tokenHash []byte, now time.Time) (User, error) {
	var u User
	err := q.QueryRowContext(ctx,
		"SELECT u.id, u.email, u.admin FROM reset_token t JOIN users u ON u.id = t.user_id WHERE t.hash = ? AND t.expires_at > ?",
		tokenHash, now.Unix()).Scan(&u.ID, &u.Email, &u.Admin)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrBadResetToken
	}
	return u, err
}

// ResetLogin spends the reset token: the account gets password, loses its
// second factors, SSH keys and sessions, and every login limit is lifted.
// The keys go too: one added by whoever had the account would otherwise
// keep opening SFTP after the owner is back.
func (s *Store) ResetLogin(ctx context.Context, tokenHash []byte, password string, now time.Time) (User, error) {
	var u User
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		if u, err = resetTokenUser(ctx, tx, tokenHash, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE users SET password = ?, totp_secret = NULL, totp_step = 0 WHERE id = ?", password, u.ID); err != nil {
			return err
		}
		for _, table := range []string{"passkeys", "recovery_codes", "ssh_keys", "sessions"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE user_id = ?", u.ID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM reset_token"); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM login_failures")
		return err
	})
	return u, err
}
