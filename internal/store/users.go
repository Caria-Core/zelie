package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound means the thing asked for does not exist.
var ErrNotFound = errors.New("not found")

// Account is a user with what is needed to log them in.
type Account struct {
	User
	Password   string
	TOTPSecret []byte // encrypted; nil until an authenticator app is set up
	Passkeys   int
}

// HasSecondFactor reports whether the account has an authenticator app or a
// passkey. Until it does, the panel only lets it set one up.
func (a Account) HasSecondFactor() bool { return a.TOTPSecret != nil || a.Passkeys > 0 }

const accountQuery = `
SELECT id, email, admin, password, totp_secret,
	(SELECT count(*) FROM passkeys WHERE user_id = users.id)
FROM users `

func (s *Store) AccountByEmail(ctx context.Context, email string) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, accountQuery+"WHERE email = ?", email))
}

func (s *Store) AccountByID(ctx context.Context, id int64) (Account, error) {
	return scanAccount(s.db.QueryRowContext(ctx, accountQuery+"WHERE id = ?", id))
}

func scanAccount(row *sql.Row) (Account, error) {
	var a Account
	err := row.Scan(&a.ID, &a.Email, &a.Admin, &a.Password, &a.TOTPSecret, &a.Passkeys)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// SetTOTP stores an encrypted authenticator secret. step is the time step of
// the code that confirmed it, which may not be used again.
func (s *Store) SetTOTP(ctx context.Context, userID int64, secret []byte, step int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET totp_secret = ?, totp_step = ? WHERE id = ?", secret, step, userID)
	return err
}

// UseTOTPStep records that a code from this time step was used. It fails if
// a code from the same or a later step was used before, which stops a code
// that was seen over someone's shoulder from being replayed.
func (s *Store) UseTOTPStep(ctx context.Context, userID, step int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE users SET totp_step = ? WHERE id = ? AND totp_step < ?", step, userID, step)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetRecoveryCodes replaces the account's recovery codes.
func (s *Store) SetRecoveryCodes(ctx context.Context, userID int64, hashes [][]byte) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ?", userID); err != nil {
			return err
		}
		for _, h := range hashes {
			if _, err := tx.ExecContext(ctx, "INSERT INTO recovery_codes (user_id, hash) VALUES (?, ?)", userID, h); err != nil {
				return err
			}
		}
		return nil
	})
}

// UseRecoveryCode spends a recovery code. It reports false if the code is
// wrong or already used.
func (s *Store) UseRecoveryCode(ctx context.Context, userID int64, hash []byte) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM recovery_codes WHERE user_id = ? AND hash = ?", userID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Passkey is a WebAuthn credential registered to an account.
type Passkey struct {
	ID         []byte
	UserID     int64
	Name       string
	Credential []byte // JSON, owned by the WebAuthn code
	CreatedAt  time.Time
}

func (s *Store) AddPasskey(ctx context.Context, p Passkey) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO passkeys (id, user_id, name, credential, created_at) VALUES (?, ?, ?, ?, ?)",
		p.ID, p.UserID, p.Name, p.Credential, p.CreatedAt.Unix())
	return err
}

func (s *Store) Passkeys(ctx context.Context, userID int64) ([]Passkey, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, name, credential, created_at FROM passkeys WHERE user_id = ? ORDER BY created_at", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Passkey
	for rows.Next() {
		p := Passkey{UserID: userID}
		var created int64
		if err := rows.Scan(&p.ID, &p.Name, &p.Credential, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = time.Unix(created, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpdatePasskey saves the credential after a login, which carries the
// authenticator's new signature counter.
func (s *Store) UpdatePasskey(ctx context.Context, id, credential []byte, used time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE passkeys SET credential = ?, used_at = ? WHERE id = ?", credential, used.Unix(), id)
	return err
}
