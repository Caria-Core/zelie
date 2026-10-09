package panel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

func TestConsoleSecrets(t *testing.T) {
	got := consoleSecrets(map[string]string{
		"RCON_PASS":       "efefef14abcd",
		"admin_password":  "hunter2hunter2",
		"API_TOKEN":       "tok-123456",
		"LICENSE_KEY":     "k-987654",
		"CLIENT_SECRET":   "efefef14abcd", // the same value twice counts once
		"SERVER_PASSWORD": "short",
		"EMPTY_PASS":      "",
		"WORLD":           "my world long name",
	})
	if len(got) != 4 {
		t.Fatalf("got %v", got)
	}
	mask := secretMasker(got)
	line := ":/home/container$ ./RustDedicated +rcon.password efefef14abcd +admin hunter2hunter2 +x tok-123456 k-987654 world"
	want := ":/home/container$ ./RustDedicated +rcon.password •••••• +admin •••••• +x •••••• •••••• world"
	if out := mask(line); out != want {
		t.Errorf("got  %q\nwant %q", out, want)
	}
	if out := secretMasker(nil)("plain"); out != "plain" {
		t.Error(out)
	}
}

func TestMaskerHidesTheLongestSecretWhole(t *testing.T) {
	mask := secretMasker([]string{"abcdef", "abcdefghij"})
	if out := mask("pw abcdefghij"); out != "pw ••••••" {
		t.Errorf("got %q", out)
	}
}

type recorderEnv struct {
	*appEnv
	log *bytes.Buffer
	mu  sync.Mutex
}

func (b *recorderEnv) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.log.Write(p)
}

func (b *recorderEnv) logged() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.log.String()
}

func newRecorder(t *testing.T, egg string) (*playerRecorder, *recorderEnv) {
	t.Helper()
	e := &recorderEnv{appEnv: newAppEnv(t), log: &bytes.Buffer{}}
	ctx := context.Background()
	if err := e.s.Store.CreateApp(ctx, store.App{ID: "game", Source: store.SourceImage, Image: "x", Port: 1, MemoryMB: 64, CPUs: 1, CreatedAt: e.s.now()}); err != nil {
		t.Fatal(err)
	}
	stored, err := e.s.Store.AddEgg(ctx, "Game", egg, []byte("{}"), e.s.now())
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.Store.CreateGameServer(ctx, store.GameServer{AppID: "game", EggID: stored.ID, Image: "x", Variables: map[string]string{}, InstallState: store.InstallDone}); err != nil {
		t.Fatal(err)
	}
	g, err := e.s.Store.GameServer(ctx, "game")
	if err != nil {
		t.Fatal(err)
	}
	e.s.Log = slog.New(slog.NewTextHandler(e, nil))
	e.s.ctx = ctx
	return e.s.newPlayerRecorder("game", g), e
}

func TestNoParserNoRecorder(t *testing.T) {
	if r, _ := newRecorder(t, "valheim"); r != nil {
		t.Error("recorder for a game without a parser")
	}
}

func TestRecorderBatchesAndFlushesOnATimer(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = 30 * time.Millisecond
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	ctx := context.Background()

	r.feed("[12:00:00 INFO]: Steve joined the game")
	r.feed("[12:00:01 INFO]: <Steve> hello")
	if got, _ := e.s.Store.Players(ctx, "game", "", 10, 0); len(got) != 0 {
		t.Fatalf("written before the delay: %+v", got)
	}
	waitFor(t, func() bool {
		got, _ := e.s.Store.Players(ctx, "game", "", 10, 0)
		return len(got) == 1 && got[0].Online
	})
	chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 10)
	if len(chat) != 1 || chat[0].Text != "hello" || chat[0].At.IsZero() {
		t.Fatalf("chat %+v", chat)
	}
	// No events waiting, no timer.
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		t.Error("timer left running")
	}
}

func TestRecorderFlushesAtTheCap(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = time.Hour
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	for i := 0; i < playerFlushMax; i++ {
		r.feed(fmt.Sprintf("[12:00:00 INFO]: <Steve> m%d", i))
	}
	chat, err := e.s.Store.PlayerChat(context.Background(), "game", "", 0, 1000)
	if err != nil || len(chat) != playerFlushMax {
		t.Fatalf("%d messages, %v", len(chat), err)
	}
}

func TestRecorderFinish(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = time.Hour
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	ctx := context.Background()
	r.feed("[12:00:00 INFO]: Steve joined the game")
	r.feed("[12:00:00 INFO]: Alex joined the game")
	r.feed("[12:00:05 INFO]: Alex left the game")

	// A console that is only left leaves the players in.
	r.finish(false, time.Time{})
	got, _ := e.s.Store.Players(ctx, "game", "", 10, 0)
	if len(got) != 2 {
		t.Fatalf("flushed %d players", len(got))
	}
	online := 0
	for _, p := range got {
		if p.Online {
			online++
		}
	}
	if online != 1 {
		t.Fatalf("%d online after a console that was only left", online)
	}
	r.feed("[12:00:06 INFO]: Bob joined the game")
	if got, _ := e.s.Store.Players(ctx, "game", "", 10, 0); len(got) != 2 {
		t.Error("a finished recorder took a line")
	}

	// The container exiting closes them.
	r.finish(true, e.s.now().Add(time.Minute))
	got, _ = e.s.Store.Players(ctx, "game", "", 10, 0)
	for _, p := range got {
		if p.Online {
			t.Errorf("%s is still online", p.Name)
		}
	}
	sess, _ := e.s.Store.PlayerSessions(ctx, "game", "name:Steve", 5)
	if len(sess) != 1 || sess[0].Reason != reasonServerStopped {
		t.Errorf("sessions %+v", sess)
	}
}

func TestRecorderLogsNearMissesOncePerMinute(t *testing.T) {
	r, e := newRecorder(t, "rust")
	long := strings.Repeat("x", 500)
	r.feed("1.2.3.4 oddly joined [windows " + long)
	r.feed("1.2.3.4 oddly joined [again")
	r.feed("Bob disconnecting")
	out := e.logged()
	if n := strings.Count(out, "player line not understood"); n != 2 {
		t.Fatalf("%d near-miss logs:\n%s", n, out)
	}
	if strings.Contains(out, long) || !strings.Contains(out, "kind=join") || !strings.Contains(out, "kind=leave") {
		t.Errorf("log:\n%s", out)
	}
}

func TestRecorderBeginReadsTheOldConsoleOnResume(t *testing.T) {
	r, e := newRecorder(t, "minecraft-paper")
	e.core.mu.Lock()
	e.core.games = map[string]engine.Spec{"game-1": {}}
	e.core.containers = map[string]engine.Status{"game-1": {ID: "game-1", App: "game", State: "running"}}
	e.core.mu.Unlock()
	e.core.emit("game-1", "[12:00:00 INFO]: UUID of player Steve is 069a79f4-44e9-4726-a5be-fca90e38aaf5\n[12:00:01 INFO]: Steve joined the game\n")
	if skip := r.begin(context.Background(), e.s.Core, "game-1", true); skip != 2 {
		t.Fatalf("skip %d", skip)
	}
	// Nothing was recorded from the old lines, but the parser knows the id.
	r.feed("[12:05:00 INFO]: <Steve> hi")
	r.feed("[12:05:01 INFO]: Steve left the game")
	r.finish(false, time.Time{})
	got, _ := e.s.Store.Players(context.Background(), "game", "", 10, 0)
	if len(got) != 1 || got[0].ID != "069a79f4-44e9-4726-a5be-fca90e38aaf5" {
		t.Fatalf("players %+v", got)
	}
}

// onConsole puts lines in the console of a running container the recorder
// can read.
func (e *recorderEnv) onConsole(container string, lines ...string) {
	e.core.mu.Lock()
	if e.core.games == nil {
		e.core.games = map[string]engine.Spec{}
	}
	e.core.games[container] = engine.Spec{}
	if e.core.containers == nil {
		e.core.containers = map[string]engine.Status{}
	}
	e.core.containers[container] = engine.Status{ID: container, App: "game", State: "running"}
	e.core.mu.Unlock()
	e.core.emit(container, strings.Join(lines, "\n")+"\n")
}

func (e *recorderEnv) recorder(t *testing.T) *playerRecorder {
	t.Helper()
	g, err := e.s.Store.GameServer(context.Background(), "game")
	if err != nil {
		t.Fatal(err)
	}
	r := e.s.newPlayerRecorder("game", g)
	r.retry = time.Millisecond
	return r
}

func TestRecorderCatchesUpOnWhatTheGamePrintedWhilePanelWasOff(t *testing.T) {
	r, e := newRecorder(t, "minecraft-paper")
	ctx := context.Background()
	e.onConsole("game-1", "[12:00:00 INFO]: Steve joined the game", "[12:00:01 INFO]: Alex joined the game",
		// The panel is not running from here.
		"[12:05:00 INFO]: Steve left the game", "[12:05:01 INFO]: <Alex> anyone there?", "[12:06:00 INFO]: Bob joined the game")
	r.begin(ctx, e.s.Core, "game-1", false)
	r.feed("[12:00:00 INFO]: Steve joined the game")
	r.feed("[12:00:01 INFO]: Alex joined the game")
	r.finish(false, time.Time{})
	if c, n, err := e.s.Store.PlayerLogPos(ctx, "game"); err != nil || c != "game-1" || n != 2 {
		t.Fatalf("position %q %d %v", c, n, err)
	}

	r = e.recorder(t)
	if skip := r.begin(ctx, e.s.Core, "game-1", true); skip != 5 {
		t.Fatalf("skip %d", skip)
	}
	r.finish(false, time.Time{})
	steve, _ := e.s.Store.Player(ctx, "game", "name:Steve")
	alex, _ := e.s.Store.Player(ctx, "game", "name:Alex")
	bob, _ := e.s.Store.Player(ctx, "game", "name:Bob")
	if steve.Online || !alex.Online || !bob.Online {
		t.Errorf("steve %+v alex %+v bob %+v", steve, alex, bob)
	}
	chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 10)
	if len(chat) != 1 || chat[0].Text != "anyone there?" {
		t.Errorf("chat %+v", chat)
	}

	// Another restart with nothing new records nothing again.
	r = e.recorder(t)
	r.begin(ctx, e.s.Core, "game-1", true)
	r.finish(false, time.Time{})
	if chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 10); len(chat) != 1 {
		t.Errorf("chat after another restart: %+v", chat)
	}
	if sess, _ := e.s.Store.PlayerSessions(ctx, "game", "name:Alex", 10); len(sess) != 1 {
		t.Errorf("sessions after another restart: %+v", sess)
	}
}

// failingLogs fails its first read after passing on some of the console.
type failingLogs struct {
	logReader
	failAfter int // bytes
	failed    bool
}

func (f *failingLogs) Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error {
	if f.failed {
		return f.logReader.Logs(ctx, id, follow, tail, w)
	}
	f.failed = true
	var all bytes.Buffer
	if err := f.logReader.Logs(ctx, id, follow, tail, &all); err != nil {
		return err
	}
	w.Write(all.Bytes()[:f.failAfter])
	return errors.New("reach the Zelie core: connection refused")
}

func TestRecorderCatchUpReadsTheConsoleAgainWhenItFails(t *testing.T) {
	lines := []string{"[12:00:00 INFO]: Steve joined the game", "[12:00:01 INFO]: <Steve> hi", "[12:00:02 INFO]: <Steve> there", "[12:00:03 INFO]: Alex joined the game"}
	cut := len(lines[0]) + len(lines[1]) + 2 + 5 // inside the third line

	t.Run("position known", func(t *testing.T) {
		r, e := newRecorder(t, "minecraft-paper")
		ctx := context.Background()
		e.onConsole("game-1", lines...)
		r.begin(ctx, e.s.Core, "game-1", false) // nothing recorded yet
		r.finish(false, time.Time{})

		r = e.recorder(t)
		if skip := r.begin(ctx, &failingLogs{logReader: e.s.Core, failAfter: cut}, "game-1", true); skip != 4 {
			t.Fatalf("skip %d", skip)
		}
		r.finish(false, time.Time{})
		if chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 10); len(chat) != 2 {
			t.Errorf("chat %+v", chat)
		}
		if got, _ := e.s.Store.Players(ctx, "game", "", 10, 0); len(got) != 2 {
			t.Errorf("players %+v", got)
		}
		if sess, _ := e.s.Store.PlayerSessions(ctx, "game", "name:Steve", 10); len(sess) != 1 {
			t.Errorf("sessions %+v", sess)
		}
		if !strings.Contains(e.logged(), "catch up on players") {
			t.Errorf("the failure was not logged:\n%s", e.logged())
		}
	})

	t.Run("position unknown", func(t *testing.T) {
		r, e := newRecorder(t, "minecraft-paper")
		ctx := context.Background()
		e.onConsole("game-1", lines...)
		if skip := r.begin(ctx, &failingLogs{logReader: e.s.Core, failAfter: cut}, "game-1", true); skip != 4 {
			t.Fatalf("skip %d", skip)
		}
		r.finish(false, time.Time{})
		// Everything the console holds is old, and the read that failed
		// must not make any of it new.
		if got, _ := e.s.Store.Players(ctx, "game", "", 10, 0); len(got) != 0 {
			t.Errorf("players %+v", got)
		}
		// The next panel starts from here.
		if c, n, err := e.s.Store.PlayerLogPos(ctx, "game"); err != nil || c != "game-1" || n != 4 {
			t.Errorf("position %q %d %v", c, n, err)
		}
	})
}

func TestRecorderDoesNotTrustTheConsoleClock(t *testing.T) {
	r, e := newRecorder(t, "rust")
	ctx := context.Background()
	r.feed(`{"Channel":0,"Message":"hi","UserId":"76561198000000001","Username":"bob","Time":99999999999}`)
	r.finish(false, time.Time{})
	chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 10)
	if len(chat) != 1 || chat[0].At.After(e.s.now()) {
		t.Fatalf("chat %+v", chat)
	}
	// Nothing that was recorded is out of the pruning's reach.
	if res, err := e.s.Store.PrunePlayerData(ctx, "game", e.s.now().Add(time.Hour), e.s.now().Add(time.Hour), e.s.now().Add(time.Hour)); err != nil || res.Chat != 1 {
		t.Errorf("pruned %+v %v", res, err)
	}
}

func TestRecorderLimitsEventsPerMinute(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = time.Hour
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	clock := &moveable{base: time.Now()}
	r.now = clock.now
	ctx := context.Background()
	for i := 0; i < 3*playerEventsPerMinute; i++ {
		r.feed(fmt.Sprintf("[12:00:00 INFO]: <Steve> m%d", i))
	}
	r.flush()
	chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 5000)
	if len(chat) != playerEventsPerMinute {
		t.Fatalf("%d messages in a minute", len(chat))
	}
	if n := strings.Count(e.logged(), "too many player events"); n != 1 {
		t.Errorf("%d warnings:\n%s", n, e.logged())
	}

	// The next minute has its own allowance.
	clock.advance(time.Minute)
	r.feed("[12:01:00 INFO]: <Steve> later")
	r.flush()
	if chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 5000); len(chat) != playerEventsPerMinute+1 {
		t.Errorf("%d messages after the minute", len(chat))
	}
}

// brokenLogs fails every read. With a status it fails the way the core does
// for a container with no log.
type brokenLogs struct {
	status int
	mu     sync.Mutex
	reads  int
}

func (b *brokenLogs) Logs(context.Context, string, bool, int64, io.Writer) error {
	b.mu.Lock()
	b.reads++
	b.mu.Unlock()
	if b.status != 0 {
		return &core.Error{Status: b.status, Message: "no logs for this container"}
	}
	return errors.New("reach the Zelie core: connection refused")
}

func TestRecorderCatchUpTakesAMissingLogForAnEmptyConsole(t *testing.T) {
	r, e := newRecorder(t, "minecraft-paper")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logs := &brokenLogs{status: 404}
	if skip := r.begin(ctx, logs, "game-1", true); skip != 0 {
		t.Fatalf("skip %d", skip)
	}
	r.finish(false, time.Time{})
	if logs.reads != 1 {
		t.Errorf("%d reads of a log that is not there", logs.reads)
	}
	if strings.Contains(e.logged(), "catch up on players") {
		t.Errorf("a missing log was logged as a failure:\n%s", e.logged())
	}
	// The next panel starts from here.
	if c, n, err := e.s.Store.PlayerLogPos(ctx, "game"); err != nil || c != "game-1" || n != 0 {
		t.Errorf("position %q %d %v", c, n, err)
	}
}

func TestRecorderCatchUpLogsAFailureThatKeepsHappeningOnce(t *testing.T) {
	r, e := newRecorder(t, "minecraft-paper")
	clock := &moveable{base: time.Now()}
	r.now, r.retry = clock.now, time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	logs := &brokenLogs{}
	done := make(chan struct{})
	go func() {
		r.begin(ctx, logs, "game-1", true)
		close(done)
	}()
	waitFor(t, func() bool {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return logs.reads >= 5
	})
	if n := strings.Count(e.logged(), "catch up on players"); n != 1 {
		t.Errorf("%d logs for failures within a minute:\n%s", n, e.logged())
	}
	clock.advance(readFailEvery)
	logs.mu.Lock()
	seen := logs.reads
	logs.mu.Unlock()
	waitFor(t, func() bool {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return logs.reads > seen+1
	})
	if n := strings.Count(e.logged(), "catch up on players"); n != 2 {
		t.Errorf("%d logs a minute later:\n%s", n, e.logged())
	}
	cancel()
	<-done
}

func TestRecorderDoesNotReplayWhatAnOlderVersionRecorded(t *testing.T) {
	r, e := newRecorder(t, "minecraft-paper")
	ctx := context.Background()
	e.onConsole("game-1", "[12:00:00 INFO]: Steve joined the game", "[12:00:01 INFO]: <Steve> hi")
	r.begin(ctx, e.s.Core, "game-1", false)
	r.feed("[12:00:00 INFO]: Steve joined the game")
	r.finish(false, time.Time{})

	// The update was rolled back, and the version that ran then recorded the
	// chat without noting how far it had read.
	old := players.Event{Kind: players.Chat, PlayerID: "name:Steve", Name: "Steve", Channel: "global", Text: "hi", At: e.s.now().Add(time.Minute)}
	if err := e.s.Store.RecordPlayerEvents(ctx, "game", []players.Event{old}); err != nil {
		t.Fatal(err)
	}

	r = e.recorder(t)
	if skip := r.begin(ctx, e.s.Core, "game-1", true); skip != 2 {
		t.Fatalf("skip %d", skip)
	}
	r.finish(false, time.Time{})
	if chat, _ := e.s.Store.PlayerChat(ctx, "game", "", 0, 10); len(chat) != 1 {
		t.Errorf("chat %+v", chat)
	}
	if c, n, err := e.s.Store.PlayerLogPos(ctx, "game"); err != nil || c != "game-1" || n != 2 {
		t.Errorf("position %q %d %v", c, n, err)
	}
}

func TestRecorderKeepsLeavesWhenTheMinuteIsUsedUp(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = time.Hour
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	clock := &moveable{base: time.Now()}
	r.now = clock.now
	ctx := context.Background()
	r.feed("[12:00:00 INFO]: Steve joined the game")
	for i := 0; i < playerEventsPerMinute; i++ {
		r.feed(fmt.Sprintf("[12:00:00 INFO]: <Steve> m%d", i))
	}
	r.feed("[12:00:01 INFO]: Alex joined the game")
	r.feed("[12:00:02 INFO]: Steve left the game")
	r.flush()
	steve, _ := e.s.Store.Player(ctx, "game", "name:Steve")
	if steve.Online {
		t.Error("Steve is shown in after his leave")
	}
	if _, err := e.s.Store.Player(ctx, "game", "name:Alex"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a join past the allowance was recorded: %v", err)
	}
}

func TestRecorderLeavesOfPlayersNobodySawJoinStoreNothing(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = time.Hour
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	clock := &moveable{base: time.Now()}
	r.now = clock.now
	ctx := context.Background()
	// Leaves are not held to the allowance, so nothing but the store keeps
	// a console that prints them from filling the table.
	for i := 0; i < 2*playerEventsPerMinute; i++ {
		r.feed(fmt.Sprintf("[12:00:00 INFO]: Ghost%d left the game", i))
	}
	r.feed("[12:00:01 INFO]: Steve joined the game")
	r.flush()
	got, _ := e.s.Store.Players(ctx, "game", "", 10, 0)
	if len(got) != 1 || got[0].ID != "name:Steve" || !got[0].Online {
		t.Errorf("players %+v", got)
	}
}

func TestRecorderWarnsOnceAMinuteWhenItRefusesPlayers(t *testing.T) {
	old := playerFlushEvery
	playerFlushEvery = time.Hour
	t.Cleanup(func() { playerFlushEvery = old })
	r, e := newRecorder(t, "minecraft-paper")
	clock := &moveable{base: time.Now()}
	r.now = clock.now
	join := func(from, to int) {
		for i := from; i < to; i++ {
			r.feed(fmt.Sprintf("[12:00:00 INFO]: P%d joined the game", i))
		}
		r.flush()
	}
	// As many players in at once as the store takes.
	const full = 10000
	for n := 0; n < full; n += playerEventsPerMinute {
		join(n, min(n+playerEventsPerMinute, full))
		clock.advance(time.Minute)
	}
	if strings.Contains(e.logged(), "not recording") {
		t.Fatalf("warned before anything was refused:\n%s", e.logged())
	}
	join(full, full+5)
	join(full+5, full+10)
	if n := strings.Count(e.logged(), "not recording"); n != 1 {
		t.Fatalf("%d warnings within a minute:\n%s", n, e.logged())
	}
	clock.advance(time.Minute)
	join(full+10, full+15)
	if n := strings.Count(e.logged(), "not recording"); n != 2 {
		t.Errorf("%d warnings a minute later:\n%s", n, e.logged())
	}
}
