package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
)

// AppLayers is what the check of an app's files outside its volumes has
// noted about it.
type AppLayers struct {
	// Deadline is when the app is stopped if it is still over its disk limit;
	// zero when it has none. Warning is what its page says until then.
	Deadline time.Time
	Warning  *msg.Msg
	// StoppedFor is why Zelie stopped the app. It stays until the app is
	// started again.
	StoppedFor *msg.Msg
}

// AppLayers returns what is noted about the app, which is nothing at all for
// most apps.
func (s *Store) AppLayers(ctx context.Context, appID string) (AppLayers, error) {
	var l AppLayers
	var deadline sql.NullInt64
	var warning, stopped string
	err := s.db.QueryRowContext(ctx, "SELECT deadline, warning, stopped_for FROM app_layers WHERE app_id = ?", appID).Scan(&deadline, &warning, &stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	if deadline.Valid {
		l.Deadline = time.Unix(deadline.Int64, 0)
	}
	l.Warning = readMsg("", warning)
	l.StoppedFor = readMsg("", stopped)
	return l, nil
}

// FirstLayerCheck returns when the check of the containers' files first ran
// on this server, and records now if it never did. The second result says
// whether this call was that first time.
func (s *Store) FirstLayerCheck(ctx context.Context, now time.Time) (time.Time, bool, error) {
	var first time.Time
	var created bool
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO layer_check (one, first_at) VALUES (1, ?)", now.Unix())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n == 1
		var at int64
		if err := tx.QueryRowContext(ctx, "SELECT first_at FROM layer_check WHERE one = 1").Scan(&at); err != nil {
			return err
		}
		first = time.Unix(at, 0)
		return nil
	})
	return first, created, err
}

// WarnLayers notes that the app is over its disk limit and will be stopped at
// deadline, with the message its page shows. An app that has a deadline keeps
// it; only the message changes.
func (s *Store) WarnLayers(ctx context.Context, appID string, deadline time.Time, warning msg.Msg) error {
	_, js := msgColumns(&warning)
	res, err := s.db.ExecContext(ctx, `INSERT INTO app_layers (app_id, deadline, warning) SELECT id, ?, ? FROM apps WHERE id = ?
		ON CONFLICT (app_id) DO UPDATE SET deadline = coalesce(deadline, excluded.deadline), warning = excluded.warning`,
		deadline.Unix(), js, appID)
	return oneRow(res, err)
}

// ClearLayerWarning forgets the deadline and the warning of an app that is
// within its disk limit again.
func (s *Store) ClearLayerWarning(ctx context.Context, appID string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE app_layers SET deadline = NULL, warning = '' WHERE app_id = ?", appID)
	return err
}

// SetStoppedFor records why Zelie stopped the app, and drops its warning. It
// does so only while the app is marked stopped, so a reason is never left on
// an app whose stop did not take hold.
func (s *Store) SetStoppedFor(ctx context.Context, appID string, why msg.Msg) error {
	_, js := msgColumns(&why)
	res, err := s.db.ExecContext(ctx, `INSERT INTO app_layers (app_id, stopped_for) SELECT id, ? FROM apps WHERE id = ? AND stopped
		ON CONFLICT (app_id) DO UPDATE SET deadline = NULL, warning = '', stopped_for = excluded.stopped_for`, js, appID)
	return oneRow(res, err)
}
