package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// DefaultSFTPPort is where the SFTP service listens until it is changed. It
// is Pterodactyl's port too, so a machine that runs Wings may need another.
const DefaultSFTPPort = 2222

// SSHKey is a public key of an account, for logging in over SFTP.
type SSHKey struct {
	ID          int64
	UserID      int64
	Name        string
	Fingerprint string
	// PublicKey is the key in the SSH wire format.
	PublicKey []byte
	CreatedAt time.Time
}

// AddSSHKey stores a key. The fingerprint is unique among all accounts, so
// a key already in use returns ErrExists.
func (s *Store) AddSSHKey(ctx context.Context, k SSHKey) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO ssh_keys (user_id, name, fingerprint, public_key, created_at) VALUES (?, ?, ?, ?, ?)",
		k.UserID, k.Name, k.Fingerprint, k.PublicKey, k.CreatedAt.Unix())
	if err != nil {
		return 0, uniqueErr(err)
	}
	return res.LastInsertId()
}

// SSHKeys lists an account's keys, oldest first.
func (s *Store) SSHKeys(ctx context.Context, userID int64) ([]SSHKey, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, user_id, name, fingerprint, public_key, created_at FROM ssh_keys WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		var k SSHKey
		var at int64
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Fingerprint, &k.PublicKey, &at); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(at, 0)
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteSSHKey removes one of an account's keys.
func (s *Store) DeleteSSHKey(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM ssh_keys WHERE user_id = ? AND id = ?", userID, id)
	return oneRow(res, err)
}

// SSHKeyOwner finds the account a key belongs to, and the key.
func (s *Store) SSHKeyOwner(ctx context.Context, fingerprint string) (Account, SSHKey, error) {
	var k SSHKey
	var at int64
	err := s.db.QueryRowContext(ctx, "SELECT id, user_id, name, fingerprint, public_key, created_at FROM ssh_keys WHERE fingerprint = ?", fingerprint).
		Scan(&k.ID, &k.UserID, &k.Name, &k.Fingerprint, &k.PublicKey, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, k, ErrNotFound
	}
	if err != nil {
		return Account{}, k, err
	}
	k.CreatedAt = time.Unix(at, 0)
	a, err := s.AccountByID(ctx, k.UserID)
	return a, k, err
}

// SFTPPassword returns the hash of a game server's SFTP password, or ""
// when it has none.
func (s *Store) SFTPPassword(ctx context.Context, appID string) (string, error) {
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT hash FROM sftp_passwords WHERE app_id = ?", appID).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

// SetSFTPPassword replaces a server's SFTP password hash. An empty hash
// removes the password.
func (s *Store) SetSFTPPassword(ctx context.Context, appID, hash string, at time.Time) error {
	if hash == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM sftp_passwords WHERE app_id = ?", appID)
		return err
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO sftp_passwords (app_id, hash, created_at) VALUES (?, ?, ?) "+
		"ON CONFLICT (app_id) DO UPDATE SET hash = excluded.hash, created_at = excluded.created_at", appID, hash, at.Unix())
	return err
}

// SFTPPort is the port the SFTP service listens on for a node.
func (s *Store) SFTPPort(ctx context.Context, nodeID int64) (int, error) {
	var port int
	err := s.db.QueryRowContext(ctx, "SELECT port FROM sftp_config WHERE node_id = ?", nodeID).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultSFTPPort, nil
	}
	return port, err
}

// SetSFTPPort changes the port the SFTP service listens on.
func (s *Store) SetSFTPPort(ctx context.Context, nodeID int64, port int) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO sftp_config (node_id, port) VALUES (?, ?) ON CONFLICT (node_id) DO UPDATE SET port = excluded.port", nodeID, port)
	return err
}
