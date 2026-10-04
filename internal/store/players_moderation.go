package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"
)

const (
	maxNoteText  = 1000
	maxBanReason = 200
	maxAuditText = 300
	// flagChunk keeps the id list of one query under SQLite's limit on
	// variables.
	flagChunk = 500
)

// PlayerNote is what an administrator wrote about a player. By is the
// account's email, empty once the account is gone.
type PlayerNote struct {
	ID       int64
	PlayerID string
	Tag      string
	Note     string
	By       string
	At       time.Time
}

// PlayerBan is a ban made through the panel. ExpiresAt is zero for a ban
// that never ends. LiftedAt is zero while it stands; LiftedBy is empty when
// it ended by itself.
type PlayerBan struct {
	ID        int64
	PlayerID  string
	Name      string
	Reason    string
	CreatedBy int64 // account id; zero when the account is gone
	By        string
	CreatedAt time.Time
	ExpiresAt time.Time
	LiftedAt  time.Time
	LiftedBy  string
}

// Active says whether the ban holds at the given time.
func (b PlayerBan) Active(at time.Time) bool {
	return b.LiftedAt.IsZero() && (b.ExpiresAt.IsZero() || b.ExpiresAt.After(at))
}

// PlayerReport is one report a player made against another.
type PlayerReport struct {
	ID           int64
	ReporterID   string
	ReporterName string
	TargetID     string
	TargetName   string
	Subject      string
	Message      string
	At           time.Time
}

// AuditEntry is one thing an account did.
type AuditEntry struct {
	ID     int64
	At     time.Time
	By     string
	Action string
	Target string
	Detail string
}

// PlayerFlags is what the lists show next to a player.
type PlayerFlags struct {
	Banned bool
	Notes  int
}

// ChatFilter narrows PlayerChatSearch. Empty fields match everything.
type ChatFilter struct {
	Player  string
	Channel string
	Search  string
	Before  int64
	Limit   int
}

func nullTime(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(n.Int64, 0)
}

func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// AddPlayerNote saves a note and returns its id. by is the account's id.
func (s *Store) AddPlayerNote(ctx context.Context, app, player, tag, note string, by int64, at time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO player_notes (app_id, player_id, tag, note, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		app, player, tag, clip(note, maxNoteText), nullID(by), at.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PlayerNotes lists a player's notes, the newest first.
func (s *Store) PlayerNotes(ctx context.Context, app, player string) ([]PlayerNote, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.id, n.player_id, n.tag, n.note, coalesce(u.email, ''), n.created_at
		FROM player_notes n LEFT JOIN users u ON u.id = n.created_by
		WHERE n.app_id = ? AND n.player_id = ? ORDER BY n.id DESC`, app, player)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerNote{}
	for rows.Next() {
		var n PlayerNote
		var at int64
		if err := rows.Scan(&n.ID, &n.PlayerID, &n.Tag, &n.Note, &n.By, &at); err != nil {
			return nil, err
		}
		n.At = time.Unix(at, 0)
		out = append(out, n)
	}
	return out, rows.Err()
}

// DeletePlayerNote removes one of a player's notes, or returns ErrNotFound.
func (s *Store) DeletePlayerNote(ctx context.Context, app, player string, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM player_notes WHERE app_id = ? AND player_id = ? AND id = ?", app, player, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PlayerFlagsFor says which of the players are banned at the given time and
// how many notes each has. Players with neither are left out of the result.
func (s *Store) PlayerFlagsFor(ctx context.Context, app string, ids []string, at time.Time) (map[string]PlayerFlags, error) {
	out := map[string]PlayerFlags{}
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), flagChunk)]
		ids = ids[len(chunk):]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, 0, len(chunk)+3)
		args = append(args, app, at.Unix())
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT player_id, sum(notes), max(banned) FROM (
				SELECT player_id, 1 AS notes, 0 AS banned FROM player_notes WHERE app_id = ?1
				UNION ALL
				SELECT player_id, 0, 1 FROM player_bans WHERE app_id = ?1 AND lifted_at IS NULL AND (expires_at IS NULL OR expires_at > ?2)
			) WHERE player_id IN (`+marks+`) GROUP BY player_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var f PlayerFlags
			var banned int
			if err := rows.Scan(&id, &f.Notes, &banned); err != nil {
				rows.Close()
				return nil, err
			}
			f.Banned = banned == 1
			out[id] = f
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CountPlayers counts what Players would list without its page.
func (s *Store) CountPlayers(ctx context.Context, app, search string) (int, error) {
	q := "SELECT count(*) FROM players p WHERE p.app_id = ?1"
	args := []any{app}
	if search = strings.TrimSpace(search); search != "" {
		q += ` AND (p.name LIKE ?2 ESCAPE '\' OR p.last_ip LIKE ?3 ESCAPE '\' OR p.player_id = ?4)`
		args = append(args, "%"+likeEscape(search)+"%", likeEscape(search)+"%", search)
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// PlayerByName finds the player of that name seen last, for a console that
// only says names. It returns ErrNotFound for a name nobody had.
func (s *Store) PlayerByName(ctx context.Context, app, name string) (Player, error) {
	p, err := scanPlayer(s.db.QueryRowContext(ctx, "SELECT "+playerColumns+` FROM players p
		WHERE p.app_id = ? AND p.name = ? COLLATE NOCASE ORDER BY p.last_seen DESC LIMIT 1`, app, name))
	if err == sql.ErrNoRows {
		return p, ErrNotFound
	}
	return p, err
}

// SearchPlayerChat lists chat the way PlayerChat does, narrowed by channel
// and by part of the text or of the sender's name.
func (s *Store) SearchPlayerChat(ctx context.Context, app string, f ChatFilter) ([]PlayerChat, error) {
	q := `SELECT id, player_id, name, channel, text, at FROM player_chat
		WHERE app_id = ?1 AND (?2 = '' OR player_id = ?2) AND (?3 = 0 OR id < ?3) AND (?4 = '' OR channel = ?4)`
	args := []any{app, f.Player, f.Before, f.Channel}
	if search := strings.TrimSpace(f.Search); search != "" {
		q += ` AND (text LIKE ?5 ESCAPE '\' OR name LIKE ?5 ESCAPE '\')`
		args = append(args, "%"+likeEscape(search)+"%")
	}
	q += " ORDER BY id DESC LIMIT " + strconv.Itoa(f.Limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerChat{}
	for rows.Next() {
		var c PlayerChat
		var at int64
		if err := rows.Scan(&c.ID, &c.PlayerID, &c.Name, &c.Channel, &c.Text, &at); err != nil {
			return nil, err
		}
		c.At = time.Unix(at, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PlayerReports lists reports, the newest first: those against one player,
// or every server's when target is empty.
func (s *Store) PlayerReports(ctx context.Context, app, target string, limit int) ([]PlayerReport, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, reporter_id, reporter_name, target_id, target_name, subject, message, at
		FROM player_reports WHERE app_id = ?1 AND (?2 = '' OR target_id = ?2) ORDER BY at DESC, id DESC LIMIT ?3`, app, target, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerReport{}
	for rows.Next() {
		var r PlayerReport
		var at int64
		if err := rows.Scan(&r.ID, &r.ReporterID, &r.ReporterName, &r.TargetID, &r.TargetName, &r.Subject, &r.Message, &at); err != nil {
			return nil, err
		}
		r.At = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

const banColumns = `b.id, b.player_id, b.name, b.reason, coalesce(b.created_by, 0), coalesce(c.email, ''), b.created_at,
	b.expires_at, b.lifted_at, coalesce(l.email, '')`

const banJoins = ` FROM player_bans b LEFT JOIN users c ON c.id = b.created_by LEFT JOIN users l ON l.id = b.lifted_by`

func scanBan(row scanner) (PlayerBan, error) {
	var b PlayerBan
	var created int64
	var expires, lifted sql.NullInt64
	if err := row.Scan(&b.ID, &b.PlayerID, &b.Name, &b.Reason, &b.CreatedBy, &b.By, &created, &expires, &lifted, &b.LiftedBy); err != nil {
		return b, err
	}
	b.CreatedAt, b.ExpiresAt, b.LiftedAt = time.Unix(created, 0), nullTime(expires), nullTime(lifted)
	return b, nil
}

func (s *Store) queryBans(ctx context.Context, q string, args ...any) ([]PlayerBan, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerBan{}
	for rows.Next() {
		b, err := scanBan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// AddPlayerBan records a ban and returns its id.
func (s *Store) AddPlayerBan(ctx context.Context, app string, b PlayerBan) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO player_bans (app_id, player_id, name, reason, created_by, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		app, b.PlayerID, clip(b.Name, maxPlayerName), clip(b.Reason, maxBanReason), nullID(b.CreatedBy), b.CreatedAt.Unix(), nullUnix(b.ExpiresAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PlayerBan returns one ban of the server, or ErrNotFound.
func (s *Store) PlayerBan(ctx context.Context, app string, id int64) (PlayerBan, error) {
	b, err := scanBan(s.db.QueryRowContext(ctx, "SELECT "+banColumns+banJoins+" WHERE b.app_id = ? AND b.id = ?", app, id))
	if err == sql.ErrNoRows {
		return b, ErrNotFound
	}
	return b, err
}

// PlayerBans lists a server's bans, the newest first. Only those in force
// at the given time are listed unless all is set. A player narrows it to
// one.
func (s *Store) PlayerBans(ctx context.Context, app, player string, all bool, at time.Time) ([]PlayerBan, error) {
	q := "SELECT " + banColumns + banJoins + " WHERE b.app_id = ?1 AND (?2 = '' OR b.player_id = ?2)"
	if !all {
		q += " AND b.lifted_at IS NULL AND (b.expires_at IS NULL OR b.expires_at > ?3)"
	} else {
		q += " AND ?3 = ?3"
	}
	return s.queryBans(ctx, q+" ORDER BY b.id DESC LIMIT 500", app, player, at.Unix())
}

// ExpiredPlayerBans lists the timed bans that ended by themselves and have
// not been lifted in the game yet.
func (s *Store) ExpiredPlayerBans(ctx context.Context, app string, at time.Time) ([]PlayerBan, error) {
	return s.queryBans(ctx, "SELECT "+banColumns+banJoins+" WHERE b.app_id = ? AND b.lifted_at IS NULL AND b.expires_at IS NOT NULL AND b.expires_at <= ? ORDER BY b.id", app, at.Unix())
}

// HasTimedBans says whether the server has a ban that ends on its own and
// has not been lifted, which is when someone has to watch the clock.
func (s *Store) HasTimedBans(ctx context.Context, app string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM player_bans WHERE app_id = ? AND lifted_at IS NULL AND expires_at IS NOT NULL LIMIT 1", app).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// LiftPlayerBan ends a ban. by is the account's id, or zero when the ban
// ran out. It reports whether this call ended it: a ban that was lifted
// already stays as it was.
func (s *Store) LiftPlayerBan(ctx context.Context, app string, id, by int64, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE player_bans SET lifted_at = ?, lifted_by = ? WHERE app_id = ? AND id = ? AND lifted_at IS NULL", at.Unix(), nullID(by), app, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AddAudit records what an account did. account is zero for what the panel
// did on its own, and app is empty for what belongs to no server.
func (s *Store) AddAudit(ctx context.Context, at time.Time, account int64, app, action, target, detail string) error {
	var appID any
	if app != "" {
		appID = app
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO audit_log (at, account_id, app_id, action, target, detail) VALUES (?, ?, ?, ?, ?, ?)",
		at.Unix(), nullID(account), appID, action, clip(target, maxAuditText), clip(detail, maxAuditText))
	return err
}

// Audit lists a server's entries, the newest first.
func (s *Store) Audit(ctx context.Context, app string, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.at, coalesce(u.email, ''), a.action, a.target, a.detail
		FROM audit_log a LEFT JOIN users u ON u.id = a.account_id
		WHERE a.app_id = ? ORDER BY a.id DESC LIMIT ?`, app, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at int64
		if err := rows.Scan(&e.ID, &at, &e.By, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(at, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

// SteamKey returns the sealed Steam Web API key, or ErrNotFound.
func (s *Store) SteamKey(ctx context.Context) ([]byte, error) {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, "SELECT sealed FROM steam_web_key").Scan(&sealed)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return sealed, err
}

// SetSteamKey saves the sealed key, replacing the one before.
func (s *Store) SetSteamKey(ctx context.Context, sealed []byte) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO steam_web_key (one, sealed) VALUES (1, ?)", sealed)
	return err
}

func (s *Store) DeleteSteamKey(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM steam_web_key")
	return err
}
