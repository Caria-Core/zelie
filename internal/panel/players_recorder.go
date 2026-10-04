package panel

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

// How long what the players table and its neighbours hold is kept.
const (
	playerHistoryRetention = 30 * 24 * time.Hour // sessions, and players not seen since
	playerChatRetention    = 7 * 24 * time.Hour
	playerReportRetention  = 30 * 24 * time.Hour
)

// playerFlushEvery is how long events wait to be written; tests shorten it.
var playerFlushEvery = 2 * time.Second

const (
	playerFlushMax   = 200
	playerPruneEvery = time.Hour
	nearMissEvery    = time.Minute
	nearMissMaxLen   = 300

	reasonServerStopped = "server stopped"
)

type logReader interface {
	Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error
}

// playerRecorder turns the console of one running game server into player
// records. It exists only while a server with a parser runs, and has no
// timer unless events are waiting to be written.
type playerRecorder struct {
	app    string
	parser players.Parser
	store  *store.Store
	log    *slog.Logger
	now    func() time.Time
	// ctx is for the writes; they must outlive the panel's shutdown by the
	// moment it takes to flush.
	ctx context.Context

	// writeMu keeps batches in the order they were read.
	writeMu   sync.Mutex
	mu        sync.Mutex
	buf       []players.Event
	timer     *time.Timer
	done      bool
	lastPrune time.Time
	missed    map[string]time.Time
}

// newPlayerRecorder returns nil for a server whose game has no parser.
func (s *Server) newPlayerRecorder(app string, g store.GameServer) *playerRecorder {
	ctx := s.baseContext()
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return nil
	}
	parser := players.ParserFor(stored.Source)
	if parser == nil {
		return nil
	}
	return &playerRecorder{
		app: app, parser: parser, store: s.Store, log: s.Log, now: s.now,
		ctx: context.WithoutCancel(ctx), missed: map[string]time.Time{},
	}
}

func (r *playerRecorder) writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.ctx, 10*time.Second)
}

// begin gets the recorder ready before the console is read. A new container
// starts with nobody in it, so visits left open by the last one are closed.
// For a server found running at startup the console is read once to teach
// the parser what the game said before, and the number of lines in it is
// returned: those are not recorded again.
func (r *playerRecorder) begin(ctx context.Context, logs logReader, container string, resumed bool) (skip int) {
	if !resumed {
		wctx, cancel := r.writeCtx()
		defer cancel()
		if err := r.store.CloseOpenSessions(wctx, r.app, r.now(), reasonServerStopped); err != nil {
			r.log.Error("close player sessions", "server", r.app, "err", err)
		}
	} else {
		w := &lineSplitter{emit: func(line string) {
			skip++
			r.parser.Feed(ansi().ReplaceAllString(line, ""))
		}}
		logs.Logs(ctx, container, false, 0, w)
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.prune()
	return skip
}

// feed reads one console line, without colour codes.
func (r *playerRecorder) feed(line string) {
	events := r.parser.Feed(line)
	if len(events) == 0 {
		return
	}
	now := r.now()
	var batch bool
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	for _, ev := range events {
		if ev.Kind == players.NearMiss {
			r.nearMissLocked(ev, now)
			continue
		}
		if ev.At.IsZero() {
			ev.At = now
		}
		r.buf = append(r.buf, ev)
	}
	switch {
	case len(r.buf) >= playerFlushMax:
		batch = true
	case len(r.buf) > 0 && r.timer == nil:
		r.timer = time.AfterFunc(playerFlushEvery, r.flush)
	}
	r.mu.Unlock()
	if batch {
		r.flush()
	}
}

// nearMissLocked logs a line the parser thought was a player event but
// could not read, at most once a minute for each kind.
func (r *playerRecorder) nearMissLocked(ev players.Event, now time.Time) {
	if last, ok := r.missed[ev.Reason]; ok && now.Sub(last) < nearMissEvery {
		return
	}
	r.missed[ev.Reason] = now
	r.log.Info("player line not understood", "server", r.app, "kind", ev.Reason, "line", truncate(ev.Text, nearMissMaxLen))
}

func (r *playerRecorder) flush() {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.mu.Lock()
	batch := r.buf
	r.buf = nil
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	wctx, cancel := r.writeCtx()
	defer cancel()
	if err := r.store.RecordPlayerEvents(wctx, r.app, batch); err != nil {
		r.log.Error("record player events", "server", r.app, "events", len(batch), "err", err)
	}
	r.prune()
}

// prune removes old records, at most once an hour. The caller holds writeMu. It runs when something
// is being written anyway, so a quiet server costs nothing.
func (r *playerRecorder) prune() {
	now := r.now()
	if !r.lastPrune.IsZero() && now.Sub(r.lastPrune) < playerPruneEvery {
		return
	}
	r.lastPrune = now
	wctx, cancel := r.writeCtx()
	defer cancel()
	if _, err := r.store.PrunePlayerData(wctx, r.app, now.Add(-playerHistoryRetention), now.Add(-playerChatRetention), now.Add(-playerReportRetention)); err != nil {
		r.log.Error("prune player records", "server", r.app, "err", err)
	}
}

// finish writes what is waiting and stops the recorder. exited says the
// container is gone, at the given time; a console that was only left
// (the panel shutting down, a new container taking over) closes nothing,
// since the players may still be in.
func (r *playerRecorder) finish(exited bool, at time.Time) {
	r.flush()
	r.mu.Lock()
	r.done = true
	r.mu.Unlock()
	if !exited {
		return
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	wctx, cancel := r.writeCtx()
	defer cancel()
	if err := r.store.CloseOpenSessions(wctx, r.app, at, reasonServerStopped); err != nil {
		r.log.Error("close player sessions", "server", r.app, "err", err)
	}
}
