package panel

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
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
	r.feed("[12:05:00 INFO]: Steve left the game")
	r.finish(false, time.Time{})
	got, _ := e.s.Store.Players(context.Background(), "game", "", 10, 0)
	if len(got) != 1 || got[0].ID != "069a79f4-44e9-4726-a5be-fca90e38aaf5" {
		t.Fatalf("players %+v", got)
	}
}
