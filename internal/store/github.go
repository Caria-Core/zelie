package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GitHubApp is the panel's own GitHub App. Key and WebhookSecret are
// encrypted with the panel key; the store never sees them in the clear.
type GitHubApp struct {
	AppID         int64
	Slug          string
	Owner         string
	HTMLURL       string
	BaseURL       string
	Key           []byte
	WebhookSecret []byte
	CreatedAt     time.Time
}

func (s *Store) GitHubApp(ctx context.Context) (GitHubApp, error) {
	var g GitHubApp
	var created int64
	err := s.db.QueryRowContext(ctx, "SELECT app_id, slug, owner, html_url, base_url, private_key, webhook_secret, created_at FROM github_app").
		Scan(&g.AppID, &g.Slug, &g.Owner, &g.HTMLURL, &g.BaseURL, &g.Key, &g.WebhookSecret, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	g.CreatedAt = time.Unix(created, 0)
	return g, err
}

// SetGitHubApp saves the App, replacing the one before, if any.
func (s *Store) SetGitHubApp(ctx context.Context, g GitHubApp) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO github_app (one, app_id, slug, owner, html_url, base_url, private_key, webhook_secret, created_at) VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)",
		g.AppID, g.Slug, g.Owner, g.HTMLURL, g.BaseURL, g.Key, g.WebhookSecret, g.CreatedAt.Unix())
	return err
}

func (s *Store) DeleteGitHubApp(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM github_app")
	return err
}

// deliveryMemory is how long a webhook delivery id is remembered. GitHub
// only lets a delivery be sent again for three days.
const deliveryMemory = 7 * 24 * time.Hour

// FirstDelivery records a webhook delivery id and reports whether it is new.
func (s *Store) FirstDelivery(ctx context.Context, id string, now time.Time) (bool, error) {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM github_deliveries WHERE received_at < ?", now.Add(-deliveryMemory).Unix()); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO github_deliveries (id, received_at) VALUES (?, ?)", id, now.Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
