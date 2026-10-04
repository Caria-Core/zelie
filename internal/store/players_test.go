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
	if err := s.RecordPlayerEvents(ctx, "rust", []players.Event{leave("1", "Ann", "x", 5)}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Player(ctx, "rust", "1")
	if err != nil || p.PlaySeconds != 0 || p.Online {
		t.Fatalf("%v %+v", err, p)
	}
	if _, err := s.Player(ctx, "rust", "nobody"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing player: %v", err)
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
