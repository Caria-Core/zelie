package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/players"
)

func playerStore(t *testing.T, apps ...string) *Store {
	t.Helper()
	s := open(t)
	for _, id := range apps {
		if err := s.CreateApp(context.Background(), App{ID: id, Source: SourceImage, Image: "nginx", Port: 80, MemoryMB: 64, CPUs: 1, CreatedAt: time.Unix(1_800_000_000, 0)}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

var t0 = time.Unix(1_800_000_000, 0)

func join(id, name, ip string, at int) players.Event {
	return players.Event{Kind: players.Join, PlayerID: id, Name: name, IP: ip, At: t0.Add(time.Duration(at) * time.Second)}
}

func leave(id, name, reason string, at int) players.Event {
	return players.Event{Kind: players.Leave, PlayerID: id, Name: name, Reason: reason, At: t0.Add(time.Duration(at) * time.Second)}
}

func chat(id, name, text string, at int) players.Event {
	return players.Event{Kind: players.Chat, PlayerID: id, Name: name, Channel: "global", Text: text, At: t0.Add(time.Duration(at) * time.Second)}
}

func TestPlayerSessions(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	err := s.RecordPlayerEvents(ctx, "rust", []players.Event{
		join("1", "Ann", "1.1.1.1", 0),
		join("2", "Bob", "2.2.2.2", 10),
		leave("1", "Ann", "disconnect", 100),
		join("1", "Ann", "1.1.1.9", 200),
		// Joining again without a leave closes the visit before it.
		join("2", "Bob2", "2.2.2.2", 310),
		{Kind: players.NearMiss, Text: "ignored"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ann, err := s.Player(ctx, "rust", "1")
	if err != nil {
		t.Fatal(err)
	}
	if ann.PlaySeconds != 100 || !ann.Online || ann.LastIP != "1.1.1.9" || !ann.FirstSeen.Equal(t0) || !ann.LastSeen.Equal(t0.Add(200*time.Second)) {
		t.Errorf("ann: %+v", ann)
	}
	bob, _ := s.Player(ctx, "rust", "2")
	if bob.PlaySeconds != 300 || bob.Name != "Bob2" || !bob.Online {
		t.Errorf("bob: %+v", bob)
	}
	sess, err := s.PlayerSessions(ctx, "rust", "2", 10)
	if err != nil || len(sess) != 2 {
		t.Fatalf("%v %+v", err, sess)
	}
	if sess[1].Reason != ReasonRejoined || !sess[1].LeftAt.Equal(t0.Add(310*time.Second)) || !sess[0].LeftAt.IsZero() {
		t.Errorf("sessions: %+v", sess)
	}

	// The server stops with both in.
	if err := s.CloseOpenSessions(ctx, "rust", t0.Add(1000*time.Second), "server stopped"); err != nil {
		t.Fatal(err)
	}
	ann, _ = s.Player(ctx, "rust", "1")
	bob, _ = s.Player(ctx, "rust", "2")
	if ann.PlaySeconds != 100+800 || bob.PlaySeconds != 300+690 || ann.Online || bob.Online {
		t.Errorf("after stop: %+v %+v", ann, bob)
	}
	sess, _ = s.PlayerSessions(ctx, "rust", "1", 10)
	if sess[0].Reason != "server stopped" || !sess[0].LeftAt.Equal(t0.Add(1000*time.Second)) {
		t.Errorf("last session: %+v", sess[0])
	}
	// Nothing is open, so a second call changes nothing.
	if err := s.CloseOpenSessions(ctx, "rust", t0.Add(5000*time.Second), "again"); err != nil {
		t.Fatal(err)
	}
	if ann2, _ := s.Player(ctx, "rust", "1"); ann2.PlaySeconds != ann.PlaySeconds {
		t.Errorf("play time changed: %d", ann2.PlaySeconds)
	}
}

func TestLeaveWithoutJoin(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	// Ann has chatted, so she is on record, though her join is not.
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{chat("1", "Ann", "hi", 0), leave("1", "Ann", "x", 5)}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Player(ctx, "rust", "1")
	if err != nil || p.PlaySeconds != 0 || p.Online || !p.LastSeen.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := s.Player(ctx, "rust", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing player: %v", err)
	}
}

func TestLeaveOfAPlayerWithNoRecordStoresNothing(t *testing.T) {
	old := maxPlayers
	maxPlayers = 3
	t.Cleanup(func() { maxPlayers = old })
	ctx := context.Background()
	s := playerStore(t, "rust")
	// A console can print leave lines for any names. They must not use up
	// the room the players that join have.
	var events []players.Event
	for i := range 10 {
		events = append(events, leave(fmt.Sprintf("x%d", i), "Ghost", "gone", i))
	}
	dropped, err := s.RecordPlayerEventsAt(ctx, "rust", events, "", 0)
	if err != nil || dropped != 0 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	if got, _ := s.Players(ctx, "rust", "", 10, 0); len(got) != 0 {
		t.Fatalf("players %+v", got)
	}
	dropped, err = s.RecordPlayerEventsAt(ctx, "rust", []players.Event{join("1", "A", "", 20), join("2", "B", "", 21), join("3", "C", "", 22)}, "", 0)
	if err != nil || dropped != 0 {
		t.Fatalf("joins: dropped %d, %v", dropped, err)
	}

	// The ones that joined can leave, in the batch that joined them or after.
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{leave("1", "A", "left", 30), leave("9", "Z", "left", 31)}); err != nil {
		t.Fatal(err)
	}
	if sess, _ := s.PlayerSessions(ctx, "rust", "1", 5); len(sess) != 1 || sess[0].LeftAt.IsZero() || sess[0].Reason != "left" {
		t.Errorf("sessions %+v", sess)
	}
	if _, err := s.Player(ctx, "rust", "9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a player who only left: %v", err)
	}
	// The room is used up, so a fourth player is not recorded, and neither is
	// his leave.
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{join("4", "D", "", 40), leave("4", "D", "bye", 49)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Player(ctx, "rust", "4"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a fourth player: %v", err)
	}
}

func TestVisitWithinOneBatchIsRecordedWithItsLeave(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{join("1", "A", "", 0), leave("1", "A", "bye", 9), leave("2", "B", "bye", 9)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Players(ctx, "rust", "", 10, 0)
	if len(got) != 1 || got[0].ID != "1" || got[0].Online || got[0].PlaySeconds != 9 {
		t.Errorf("players %+v", got)
	}
}

func TestCloseStaleSessions(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "a", "b")
	for _, app := range []string{"a", "b"} {
		if err := s.RecordPlayerEvents(ctx, app, []players.Event{join("1", "Ann", "", 0), chat("1", "Ann", "hi", 60)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseStaleSessions(ctx, []string{"b"}, ReasonPanelRestarted); err != nil {
		t.Fatal(err)
	}
	a, _ := s.Player(ctx, "a", "1")
	b, _ := s.Player(ctx, "b", "1")
	if a.Online || a.PlaySeconds != 60 {
		t.Errorf("a: %+v", a)
	}
	if !b.Online || b.PlaySeconds != 0 {
		t.Errorf("b: %+v", b)
	}
	sess, _ := s.PlayerSessions(ctx, "a", "1", 1)
	if sess[0].Reason != ReasonPanelRestarted || !sess[0].LeftAt.Equal(t0.Add(60*time.Second)) {
		t.Errorf("session: %+v", sess[0])
	}
}

func TestPlayerChatAndReports(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust", "other")
	var evs []players.Event
	for i := 0; i < 5; i++ {
		evs = append(evs, chat("1", "Ann", fmt.Sprintf("m%d", i), i), chat("2", "Bob", fmt.Sprintf("b%d", i), i))
	}
	evs = append(evs, players.Event{Kind: players.Report, PlayerID: "1", Name: "Ann", Target: "2", TargetName: "Bob", Reason: "cheat", Text: "aim", At: t0})
	if err := s.RecordPlayerEvents(ctx, "rust", evs); err != nil {
		t.Fatal(err)
	}
	all, err := s.PlayerChat(ctx, "rust", "", 0, 4)
	if err != nil || len(all) != 4 || all[0].Text != "b4" {
		t.Fatalf("%v %+v", err, all)
	}
	next, _ := s.PlayerChat(ctx, "rust", "", all[3].ID, 100)
	if len(next) != 6 || next[0].ID >= all[3].ID {
		t.Errorf("next page: %+v", next)
	}
	ann, _ := s.PlayerChat(ctx, "rust", "1", 0, 100)
	if len(ann) != 5 || ann[0].Text != "m4" || ann[0].Channel != "global" {
		t.Errorf("ann: %+v", ann)
	}
	if other, _ := s.PlayerChat(ctx, "other", "", 0, 10); len(other) != 0 {
		t.Errorf("other server: %+v", other)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM player_reports WHERE app_id = 'rust' AND reporter_id = '1' AND target_name = 'Bob' AND subject = 'cheat'").Scan(&n); err != nil || n != 1 {
		t.Errorf("reports: %d %v", n, err)
	}
}

func TestLongTextIsClipped(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	long := ""
	for len(long) < 3000 {
		long += "é"
	}
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{chat("1", "Ann", long, 0)}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.PlayerChat(ctx, "rust", "", 0, 1)
	if len(got[0].Text) > maxChatText || got[0].Text == "" {
		t.Errorf("text is %d bytes", len(got[0].Text))
	}
}

func TestPlayersList(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	err := s.RecordPlayerEvents(ctx, "rust", []players.Event{
		join("1", "Anna_B", "10.0.0.1", 0),
		join("2", "Bob", "10.0.0.2", 10),
		join("3", "100%", "192.168.0.1", 20),
	})
	if err != nil {
		t.Fatal(err)
	}
	names := func(search string, limit, offset int) []string {
		got, err := s.Players(ctx, "rust", search, limit, offset)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, p := range got {
			out = append(out, p.Name)
		}
		return out
	}
	eq := func(got []string, want ...string) {
		t.Helper()
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("got %v want %v", got, want)
		}
	}
	eq(names("", 10, 0), "100%", "Bob", "Anna_B")
	eq(names("", 1, 1), "Bob")
	eq(names("an", 10, 0), "Anna_B")
	eq(names("10.0.0", 10, 0), "Bob", "Anna_B")
	eq(names("3", 10, 0), "100%")
	// % and _ are plain characters in a search.
	eq(names("%", 10, 0), "100%")
	eq(names("a_b", 10, 0), "Anna_B")
	eq(names("A__B", 10, 0))
}

func TestPlayersSharingIP(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust", "other")
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{
		join("1", "Ann", "5.5.5.5", 0), leave("1", "Ann", "", 1),
		join("1", "Ann", "6.6.6.6", 2), leave("1", "Ann", "", 3),
		join("2", "Bob", "5.5.5.5", 10), leave("2", "Bob", "", 11),
		join("2", "Bob", "5.5.5.5", 12), leave("2", "Bob", "", 13),
		join("3", "Cy", "7.7.7.7", 20), leave("3", "Cy", "", 21),
		join("4", "Di", "6.6.6.6", 30),
	}); err != nil {
		t.Fatal(err)
	}
	// The same address on another server means nothing here.
	if err := s.RecordPlayerEvents(ctx, "other", []players.Event{join("9", "Zed", "5.5.5.5", 40)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PlayersSharingIP(ctx, "rust", "1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PlayerID != "4" || got[0].IP != "6.6.6.6" || got[1].PlayerID != "2" || got[1].IP != "5.5.5.5" {
		t.Errorf("got %+v", got)
	}
}

func TestPrunePlayerData(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "a", "b")
	day := 24 * 3600
	for _, app := range []string{"a", "b"} {
		if err := s.RecordPlayerEvents(ctx, app, []players.Event{
			join("old", "Old", "", 0), leave("old", "Old", "", 10), chat("old", "Old", "x", 20),
			join("noted", "Noted", "", 0), leave("noted", "Noted", "", 10),
			join("here", "Here", "", 0), // still in
			join("new", "New", "", 40*day), leave("new", "New", "", 40*day+5), chat("new", "New", "y", 40*day+6),
			{Kind: players.Report, PlayerID: "new", Target: "old", At: t0.Add(5 * time.Second)},
			{Kind: players.Report, PlayerID: "new", Target: "old", At: t0.Add(time.Duration(40*day) * time.Second)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO player_notes (app_id, player_id, created_at) VALUES ('a', 'noted', 1)"); err != nil {
		t.Fatal(err)
	}
	cut := func(days int) time.Time { return t0.Add(time.Duration(days*day) * time.Second) }
	res, err := s.PrunePlayerData(ctx, "a", cut(30), cut(1), cut(30))
	if err != nil {
		t.Fatal(err)
	}
	// Server a: old's session and old (but not noted, not here); b is untouched.
	if res.Sessions != 2 || res.Players != 1 || res.Chat != 1 || res.Reports != 1 {
		t.Errorf("prune a: %+v", res)
	}
	if _, err := s.Player(ctx, "a", "old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old kept: %v", err)
	}
	for _, id := range []string{"noted", "here", "new"} {
		if _, err := s.Player(ctx, "a", id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := s.Player(ctx, "b", "old"); err != nil {
		t.Errorf("b was pruned: %v", err)
	}
	res, err = s.PrunePlayerData(ctx, "", cut(30), cut(1), cut(30))
	if err != nil {
		t.Fatal(err)
	}
	if res.Players != 2 || res.Chat != 1 {
		t.Errorf("prune all: %+v", res)
	}
}

func TestPlayerDataGoesWithTheApp(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{join("1", "Ann", "1.1.1.1", 0), chat("1", "Ann", "hi", 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO audit_log (at, app_id, action) VALUES (1, 'rust', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM apps WHERE id = 'rust'"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"players", "player_sessions", "player_chat"} {
		var n int
		if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows, %v", table, n, err)
		}
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 1 {
		t.Errorf("audit rows: %d %v", n, err)
	}
}

func TestCloseOpenSessionsSparesLaterJoins(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{join("1", "Ann", "", 0), join("2", "Bob", "", 50)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseOpenSessions(ctx, "rust", t0.Add(30*time.Second), "server stopped"); err != nil {
		t.Fatal(err)
	}
	ann, _ := s.Player(ctx, "rust", "1")
	bob, _ := s.Player(ctx, "rust", "2")
	if ann.Online || ann.PlaySeconds != 30 || !bob.Online || bob.PlaySeconds != 0 {
		t.Errorf("ann %+v bob %+v", ann, bob)
	}
}

func TestPlayerLogPosition(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	now := time.Now()
	at := func(kind players.Kind, id string, ago time.Duration) players.Event {
		return players.Event{Kind: kind, PlayerID: id, Name: "Ann", At: now.Add(-ago)}
	}
	if _, _, err := s.PlayerLogPos(ctx, "rust"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("before anything: %v", err)
	}
	if err := s.SetPlayerLogPos(ctx, "rust", "rust-1", 0); err != nil {
		t.Fatal(err)
	}
	// The position moves with the events that were read up to it.
	if _, err := s.RecordPlayerEventsAt(ctx, "rust", []players.Event{at(players.Join, "1", time.Hour)}, "rust-1", 12); err != nil {
		t.Fatal(err)
	}
	if c, n, err := s.PlayerLogPos(ctx, "rust"); err != nil || c != "rust-1" || n != 12 {
		t.Errorf("position %q %d %v", c, n, err)
	}
	// Events written with no container leave it alone. They are of lines the
	// position is not past, so it can still be trusted.
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{at(players.Leave, "1", 30*time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if c, n, err := s.PlayerLogPos(ctx, "rust"); err != nil || c != "rust-1" || n != 12 {
		t.Errorf("position %q %d %v", c, n, err)
	}
	// A new container starts over.
	if err := s.SetPlayerLogPos(ctx, "rust", "rust-2", 0); err != nil {
		t.Fatal(err)
	}
	if c, n, _ := s.PlayerLogPos(ctx, "rust"); c != "rust-2" || n != 0 {
		t.Errorf("position %q %d", c, n)
	}
}

func TestPlayerLogPositionIsLostWhenSomeoneRecordedSince(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "rust")
	if _, err := s.RecordPlayerEventsAt(ctx, "rust", []players.Event{{Kind: players.Join, PlayerID: "1", Name: "Ann", At: time.Now()}}, "rust-1", 12); err != nil {
		t.Fatal(err)
	}
	// A version that does not move the position records a player later.
	later := players.Event{Kind: players.Chat, PlayerID: "2", Name: "Bob", Text: "hi", At: time.Now().Add(time.Hour)}
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{later}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PlayerLogPos(ctx, "rust"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("position after a record it was not in: %v", err)
	}
	// Noting a new one, as a panel does once it has caught up, makes it good
	// again, and a clock that was ahead does not spoil it.
	if err := s.SetPlayerLogPos(ctx, "rust", "rust-1", 20); err != nil {
		t.Fatal(err)
	}
	if c, n, err := s.PlayerLogPos(ctx, "rust"); err != nil || c != "rust-1" || n != 20 {
		t.Errorf("position %q %d %v", c, n, err)
	}
}

func TestPlayersInAtOnceAreBounded(t *testing.T) {
	old := maxOpenSessions
	maxOpenSessions = 3
	t.Cleanup(func() { maxOpenSessions = old })
	ctx := context.Background()
	s := playerStore(t, "rust")
	dropped, err := s.RecordPlayerEventsAt(ctx, "rust", []players.Event{
		join("1", "A", "", 0), join("2", "B", "", 1), join("3", "C", "", 2), join("4", "D", "", 3), join("5", "E", "", 4),
		// A player who is in already is not a new one.
		join("1", "A", "", 5),
	}, "", 0)
	if err != nil || dropped != 2 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	if got, _ := s.Players(ctx, "rust", "", 10, 0); len(got) != 3 {
		t.Errorf("%d players", len(got))
	}

	// Room again once someone leaves.
	dropped, err = s.RecordPlayerEventsAt(ctx, "rust", []players.Event{leave("2", "B", "", 10), join("4", "D", "", 11)}, "", 0)
	if err != nil || dropped != 0 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	if d, err := s.Player(ctx, "rust", "4"); err != nil || !d.Online {
		t.Errorf("D %+v %v", d, err)
	}
}

func TestPlayersOnRecordAreBounded(t *testing.T) {
	old := maxPlayers
	maxPlayers = 3
	t.Cleanup(func() { maxPlayers = old })
	ctx := context.Background()
	s := playerStore(t, "rust")
	dropped, err := s.RecordPlayerEventsAt(ctx, "rust", []players.Event{
		join("1", "A", "", 0), chat("2", "B", "hi", 1), join("3", "C", "", 2),
		join("4", "D", "", 3), chat("5", "E", "me too", 4), join("6", "F", "", 5),
		// The ones on record go on being recorded.
		chat("1", "A", "still here", 6), join("2", "B", "", 7),
	}, "", 0)
	if err != nil || dropped != 3 {
		t.Fatalf("dropped %d, %v", dropped, err)
	}
	if got, _ := s.Players(ctx, "rust", "", 10, 0); len(got) != 3 {
		t.Errorf("%d players", len(got))
	}
	if c, _ := s.PlayerChat(ctx, "rust", "", 0, 10); len(c) != 2 {
		t.Errorf("chat %+v", c)
	}
	if b, err := s.Player(ctx, "rust", "2"); err != nil || !b.Online {
		t.Errorf("B %+v %v", b, err)
	}

	// Another server has its own room, and a report does not take any.
	if err := s.CreateApp(ctx, App{ID: "other", Source: SourceImage, Image: "nginx", Port: 81, MemoryMB: 64, CPUs: 1, CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	report := players.Event{Kind: players.Report, PlayerID: "9", Name: "I", Target: "10", Text: "cheats", At: t0}
	dropped, err = s.RecordPlayerEventsAt(ctx, "other", []players.Event{join("4", "D", "", 0), report}, "", 0)
	if err != nil || dropped != 0 {
		t.Fatalf("other server: dropped %d, %v", dropped, err)
	}
	if r, _ := s.PlayerReports(ctx, "other", "", 10); len(r) != 1 {
		t.Errorf("reports %+v", r)
	}
}

func TestOldestFinishedRecordsGoPastTheirLimit(t *testing.T) {
	oldSessions, oldChat, oldReports := maxSessions, maxChatRows, maxReportRows
	maxSessions, maxChatRows, maxReportRows = 2, 3, 1
	t.Cleanup(func() { maxSessions, maxChatRows, maxReportRows = oldSessions, oldChat, oldReports })
	ctx := context.Background()
	s := playerStore(t, "rust", "other")
	var events []players.Event
	for i := 0; i < 5; i++ {
		events = append(events, join("1", "A", "", 10*i), leave("1", "A", "", 10*i+5), chat("1", "A", fmt.Sprintf("m%d", i), 10*i),
			players.Event{Kind: players.Report, PlayerID: "1", Target: "2", Text: fmt.Sprintf("r%d", i), At: t0.Add(time.Duration(i) * time.Second)})
	}
	// Still in: never counted, never removed.
	events = append(events, join("2", "B", "", 1))
	for _, app := range []string{"rust", "other"} {
		if _, err := s.RecordPlayerEventsAt(ctx, app, events, "", 0); err != nil {
			t.Fatal(err)
		}
	}
	res, err := s.CapPlayerData(ctx, "rust")
	if err != nil || res.Sessions != 3 || res.Chat != 2 || res.Reports != 4 || res.Players != 0 {
		t.Fatalf("capped %+v, %v", res, err)
	}
	sess, _ := s.PlayerSessions(ctx, "rust", "1", 10)
	if len(sess) != 2 || sess[0].JoinedAt != t0.Add(40*time.Second) {
		t.Errorf("sessions %+v", sess)
	}
	if b, _ := s.PlayerSessions(ctx, "rust", "2", 10); len(b) != 1 {
		t.Errorf("the open session: %+v", b)
	}
	c, _ := s.PlayerChat(ctx, "rust", "", 0, 10)
	if len(c) != 3 || c[0].Text != "m4" || c[2].Text != "m2" {
		t.Errorf("chat %+v", c)
	}
	if r, _ := s.PlayerReports(ctx, "rust", "", 10); len(r) != 1 || r[0].Message != "r4" {
		t.Errorf("reports %+v", r)
	}
	// The other server was not asked.
	if c, _ := s.PlayerChat(ctx, "other", "", 0, 10); len(c) != 5 {
		t.Errorf("other server's chat: %d", len(c))
	}
	// Nothing over the limit, nothing to do.
	if res, err := s.CapPlayerData(ctx, "rust"); err != nil || res.Sessions+res.Chat+res.Reports != 0 {
		t.Errorf("capped again %+v, %v", res, err)
	}
}

func TestBanInForce(t *testing.T) {
	ctx := context.Background()
	s := playerStore(t, "mc")
	now := time.Now()
	add := func(id, name string, from, to time.Duration) int64 {
		b := PlayerBan{PlayerID: id, Name: name, CreatedAt: now.Add(from)}
		if to != 0 {
			b.ExpiresAt = now.Add(to)
		}
		n, err := s.AddPlayerBan(ctx, "mc", b)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	add("u1", "Steve", -2*time.Hour, -time.Hour) // over
	lifted := add("u2", "Steve", -time.Hour, 0)
	if _, err := s.LiftPlayerBan(ctx, "mc", lifted, 0, now); err != nil {
		t.Fatal(err)
	}
	held := func(id, name string) bool {
		ok, err := s.BanInForce(ctx, "mc", id, name, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if held("u1", "Steve") || held("u2", "Steve") {
		t.Error("a ban that is over or lifted holds")
	}
	add("name:steve", "steve", -time.Minute, 0)
	if !held("name:steve", "") || held("u1", "") {
		t.Error("by id")
	}
	if !held("u1", "Steve") || !held("u1", "STEVE") || held("u1", "Alex") {
		t.Error("by name, whatever the case")
	}
}
