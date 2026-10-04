package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Caria-Core/zelie/internal/players"
)

// Console lines are capped well above these, so a player cannot make the
// database grow by what one chat line holds.
const (
	maxPlayerName = 64
	maxChatText   = 1000
	maxReportText = 2000
)

const (
	// ReasonRejoined closes a session when the same player joins again
	// without a leave in between.
	ReasonRejoined = "rejoined"
	// ReasonPanelRestarted closes sessions a stopped server left open.
	ReasonPanelRestarted = "panel restarted"
)

// Player is what is known of one player of a server.
type Player struct {
	ID          string
	Name        string
	FirstSeen   time.Time
	LastSeen    time.Time
	LastIP      string
	PlaySeconds int64
	Online      bool
}

// PlayerSession is one visit.
type PlayerSession struct {
	ID       int64
	PlayerID string
	Name     string
	IP       string
	JoinedAt time.Time
	LeftAt   time.Time // zero while the player is in
	Reason   string
}

// PlayerChat is one chat message.
type PlayerChat struct {
	ID       int64
	PlayerID string
	Name     string
	Channel  string
	Text     string
	At       time.Time
}

// PlayerLink is a player who connected from an address another one used.
type PlayerLink struct {
	PlayerID string
	Name     string
	IP       string
	LastSeen time.Time
}

// PruneResult is how many rows PrunePlayerData removed.
type PruneResult struct {
	Sessions, Players, Chat, Reports int64
}

type openSession struct {
	id       int64
	joinedAt int64
}

// RecordPlayerEvents stores what a parser read, in order, in one
// transaction. Events with no time are stamped now, and near misses are not
// stored.
func (s *Store) RecordPlayerEvents(ctx context.Context, app string, events []players.Event) error {
	if len(events) == 0 {
		return nil
	}
	now := time.Now()
	return s.tx(ctx, func(tx *sql.Tx) error {
		open := map[string]openSession{}
		rows, err := tx.QueryContext(ctx, "SELECT player_id, id, joined_at FROM player_sessions WHERE app_id = ? AND left_at IS NULL", app)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var o openSession
			if err := rows.Scan(&id, &o.id, &o.joinedAt); err != nil {
				rows.Close()
				return err
			}
			open[id] = o
		}
		if err := rows.Close(); err != nil {
			return err
		}

		upsert, err := tx.PrepareContext(ctx, `INSERT INTO players (app_id, player_id, name, first_seen, last_seen, last_ip)
			VALUES (?1, ?2, ?3, ?4, ?4, ?5)
			ON CONFLICT (app_id, player_id) DO UPDATE SET
				name = CASE WHEN excluded.name <> '' AND excluded.last_seen >= last_seen THEN excluded.name ELSE name END,
				first_seen = min(first_seen, excluded.first_seen),
				last_seen = max(last_seen, excluded.last_seen),
				last_ip = CASE WHEN excluded.last_ip <> '' THEN excluded.last_ip ELSE last_ip END`)
		if err != nil {
			return err
		}
		defer upsert.Close()
		endSession, err := tx.PrepareContext(ctx, "UPDATE player_sessions SET left_at = ?, reason = ? WHERE id = ?")
		if err != nil {
			return err
		}
		defer endSession.Close()
		addTime, err := tx.PrepareContext(ctx, "UPDATE players SET play_seconds = play_seconds + ? WHERE app_id = ? AND player_id = ?")
		if err != nil {
			return err
		}
		defer addTime.Close()
		newSession, err := tx.PrepareContext(ctx, "INSERT INTO player_sessions (app_id, player_id, name, ip, joined_at) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer newSession.Close()
		newChat, err := tx.PrepareContext(ctx, "INSERT INTO player_chat (app_id, player_id, name, channel, text, at) VALUES (?, ?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer newChat.Close()
		newReport, err := tx.PrepareContext(ctx, `INSERT INTO player_reports (app_id, reporter_id, reporter_name, target_id, target_name, subject, message, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer newReport.Close()

		// closeSession ends the player's open session, if any.
		closeSession := func(id string, at int64, reason string) error {
			o, ok := open[id]
			if !ok {
				return nil
			}
			delete(open, id)
			at = max(at, o.joinedAt)
			if _, err := endSession.ExecContext(ctx, at, reason, o.id); err != nil {
				return err
			}
			_, err := addTime.ExecContext(ctx, at-o.joinedAt, app, id)
			return err
		}

		for _, ev := range events {
			if ev.Kind == players.NearMiss || ev.PlayerID == "" {
				continue
			}
			at := ev.At
			if at.IsZero() {
				at = now
			}
			t := at.Unix()
			name := clip(ev.Name, maxPlayerName)
			switch ev.Kind {
			case players.Join:
				if _, err := upsert.ExecContext(ctx, app, ev.PlayerID, name, t, ev.IP); err != nil {
					return err
				}
				if err := closeSession(ev.PlayerID, t, ReasonRejoined); err != nil {
					return err
				}
				res, err := newSession.ExecContext(ctx, app, ev.PlayerID, name, ev.IP, t)
				if err != nil {
					return err
				}
				id, err := res.LastInsertId()
				if err != nil {
					return err
				}
				open[ev.PlayerID] = openSession{id: id, joinedAt: t}
			case players.Leave:
				if _, err := upsert.ExecContext(ctx, app, ev.PlayerID, name, t, ev.IP); err != nil {
					return err
				}
				if err := closeSession(ev.PlayerID, t, clip(ev.Reason, 200)); err != nil {
					return err
				}
			case players.Chat:
				if _, err := upsert.ExecContext(ctx, app, ev.PlayerID, name, t, ""); err != nil {
					return err
				}
				if _, err := newChat.ExecContext(ctx, app, ev.PlayerID, name, clip(ev.Channel, 16), clip(ev.Text, maxChatText), t); err != nil {
					return err
				}
			case players.Report:
				if _, err := newReport.ExecContext(ctx, app, ev.PlayerID, name, ev.Target,
					clip(ev.TargetName, maxPlayerName), clip(ev.Reason, 200), clip(ev.Text, maxReportText), t); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// CloseOpenSessions ends every open session of a server at the given time,
// for when the server stops: the game prints no leave for players that are
// still in.
func (s *Store) CloseOpenSessions(ctx context.Context, app string, at time.Time, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return closeOpenSessions(ctx, tx, app, at.Unix(), reason)
	})
}

// CloseStaleSessions ends the open sessions of every server not in running.
// Nobody saw those players leave, so each session ends at the last moment
// the player was seen rather than at a guess.
func (s *Store) CloseStaleSessions(ctx context.Context, running []string, reason string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT DISTINCT app_id FROM player_sessions WHERE left_at IS NULL")
		if err != nil {
			return err
		}
		var stale []string
		for rows.Next() {
			var app string
			if err := rows.Scan(&app); err != nil {
				rows.Close()
				return err
			}
			if !contains(running, app) {
				stale = append(stale, app)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, app := range stale {
			if err := closeOpenSessions(ctx, tx, app, 0, reason); err != nil {
				return err
			}
		}
		return nil
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// closeOpenSessions ends a server's open sessions at at, or at each
// player's last_seen when at is zero. With at, only visits that began by
// then end: a server that was replaced a moment ago must not take its
// successor's first players with it. Play time is added first, while the
// sessions can still be told apart as open.
func closeOpenSessions(ctx context.Context, tx *sql.Tx, app string, at int64, reason string) error {
	playerEnd, sessionEnd := "?2", "?3"
	started := "AND s.joined_at <= ?2"
	args1 := []any{app, at}
	args2 := []any{app, reason, at}
	if at == 0 {
		playerEnd = "p.last_seen"
		sessionEnd = "coalesce((SELECT p.last_seen FROM players p WHERE p.app_id = s.app_id AND p.player_id = s.player_id), s.joined_at)"
		started = ""
		args1, args2 = args1[:1], args2[:2]
	}
	if _, err := tx.ExecContext(ctx, `UPDATE players AS p SET play_seconds = play_seconds + coalesce((
			SELECT sum(max(`+playerEnd+` - s.joined_at, 0)) FROM player_sessions s
			WHERE s.app_id = p.app_id AND s.player_id = p.player_id AND s.left_at IS NULL `+started+`), 0)
		WHERE p.app_id = ?1 AND p.player_id IN (
			SELECT s.player_id FROM player_sessions s WHERE s.app_id = ?1 AND s.left_at IS NULL `+started+`)`, args1...); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE player_sessions AS s SET reason = ?2, left_at = max(s.joined_at, `+sessionEnd+`)
		WHERE s.app_id = ?1 AND s.left_at IS NULL `+strings.ReplaceAll(started, "?2", "?3"), args2...)
	return err
}

// PrunePlayerData deletes what is older than the cutoffs: finished sessions
// and players not seen since sessionsBefore, chat before chatBefore and
// reports before reportsBefore. An empty app means every server. A player
// with a note or a ban is kept, and so is one who is in.
func (s *Store) PrunePlayerData(ctx context.Context, app string, sessionsBefore, chatBefore, reportsBefore time.Time) (PruneResult, error) {
	var r PruneResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		run := func(n *int64, q string, args ...any) error {
			res, err := tx.ExecContext(ctx, q, args...)
			if err != nil {
				return err
			}
			*n, err = res.RowsAffected()
			return err
		}
		if err := run(&r.Sessions, "DELETE FROM player_sessions WHERE (?1 = '' OR app_id = ?1) AND left_at IS NOT NULL AND left_at < ?2", app, sessionsBefore.Unix()); err != nil {
			return err
		}
		if err := run(&r.Chat, "DELETE FROM player_chat WHERE (?1 = '' OR app_id = ?1) AND at < ?2", app, chatBefore.Unix()); err != nil {
			return err
		}
		if err := run(&r.Reports, "DELETE FROM player_reports WHERE (?1 = '' OR app_id = ?1) AND at < ?2", app, reportsBefore.Unix()); err != nil {
			return err
		}
		return run(&r.Players, `DELETE FROM players AS p WHERE (?1 = '' OR p.app_id = ?1) AND p.last_seen < ?2
			AND NOT EXISTS (SELECT 1 FROM player_sessions s WHERE s.app_id = p.app_id AND s.player_id = p.player_id AND s.left_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM player_notes n WHERE n.app_id = p.app_id AND n.player_id = p.player_id)
			AND NOT EXISTS (SELECT 1 FROM player_bans b WHERE b.app_id = p.app_id AND b.player_id = p.player_id)`,
			app, sessionsBefore.Unix())
	})
	return r, err
}

const playerColumns = `p.player_id, p.name, p.first_seen, p.last_seen, p.last_ip, p.play_seconds, EXISTS (
	SELECT 1 FROM player_sessions s WHERE s.app_id = p.app_id AND s.player_id = p.player_id AND s.left_at IS NULL)`

func scanPlayer(row scanner) (Player, error) {
	var p Player
	var first, last int64
	if err := row.Scan(&p.ID, &p.Name, &first, &last, &p.LastIP, &p.PlaySeconds, &p.Online); err != nil {
		return p, err
	}
	p.FirstSeen, p.LastSeen = time.Unix(first, 0), time.Unix(last, 0)
	return p, nil
}

// Player returns one player, or ErrNotFound.
func (s *Store) Player(ctx context.Context, app, id string) (Player, error) {
	p, err := scanPlayer(s.db.QueryRowContext(ctx, "SELECT "+playerColumns+" FROM players p WHERE p.app_id = ? AND p.player_id = ?", app, id))
	if err == sql.ErrNoRows {
		return p, ErrNotFound
	}
	return p, err
}

// Players lists a server's players, the ones seen last first. A search
// matches part of a name, the start of an address, or a whole id.
func (s *Store) Players(ctx context.Context, app, search string, limit, offset int) ([]Player, error) {
	q := "SELECT " + playerColumns + " FROM players p WHERE p.app_id = ?1"
	args := []any{app}
	if search = strings.TrimSpace(search); search != "" {
		q += ` AND (p.name LIKE ?2 ESCAPE '\' OR p.last_ip LIKE ?3 ESCAPE '\' OR p.player_id = ?4)`
		args = append(args, "%"+likeEscape(search)+"%", likeEscape(search)+"%", search)
	}
	q += " ORDER BY p.last_seen DESC, p.player_id LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Player{}
	for rows.Next() {
		p, err := scanPlayer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// PlayerSessions lists a player's visits, the newest first.
func (s *Store) PlayerSessions(ctx context.Context, app, player string, limit int) ([]PlayerSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, player_id, name, ip, joined_at, coalesce(left_at, 0), reason
		FROM player_sessions WHERE app_id = ? AND player_id = ? ORDER BY joined_at DESC, id DESC LIMIT ?`, app, player, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerSession{}
	for rows.Next() {
		var x PlayerSession
		var joined, left int64
		if err := rows.Scan(&x.ID, &x.PlayerID, &x.Name, &x.IP, &joined, &left, &x.Reason); err != nil {
			return nil, err
		}
		x.JoinedAt = time.Unix(joined, 0)
		if left != 0 {
			x.LeftAt = time.Unix(left, 0)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// PlayerChat lists chat, the newest first: a server's, or one player's when
// player is not empty. beforeID, when not zero, returns only messages older
// than that one, which is how the next page is asked for.
func (s *Store) PlayerChat(ctx context.Context, app, player string, beforeID int64, limit int) ([]PlayerChat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, player_id, name, channel, text, at FROM player_chat
		WHERE app_id = ?1 AND (?2 = '' OR player_id = ?2) AND (?3 = 0 OR id < ?3)
		ORDER BY id DESC LIMIT ?4`, app, player, beforeID, limit)
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

// PlayersSharingIP lists the other players who connected from an address
// this player used, each with the address they shared, the ones seen last
// first.
func (s *Store) PlayersSharingIP(ctx context.Context, app, player string, limit int) ([]PlayerLink, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT o.player_id, p.name, o.ip, p.last_seen
		FROM (SELECT DISTINCT ip FROM player_sessions WHERE app_id = ?1 AND player_id = ?2 AND ip <> '') mine
		JOIN player_sessions o ON o.app_id = ?1 AND o.ip = mine.ip AND o.player_id <> ?2
		JOIN players p ON p.app_id = o.app_id AND p.player_id = o.player_id
		GROUP BY o.player_id, o.ip
		ORDER BY p.last_seen DESC, o.player_id, o.ip LIMIT ?3`, app, player, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PlayerLink{}
	for rows.Next() {
		var l PlayerLink
		var seen int64
		if err := rows.Scan(&l.PlayerID, &l.Name, &l.IP, &seen); err != nil {
			return nil, err
		}
		l.LastSeen = time.Unix(seen, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
