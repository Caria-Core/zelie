package panel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
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

	// playerEventsPerMinute is the most player events one server gets
	// recorded in a minute. The rows are bounded on their own, so this limits
	// churn, and it is set well above what players make: five hundred joins in
	// the minute a Rust wipe opens, with chat at several lines a second, is
	// under a thousand. More than that is a plugin gone wrong or a container
	// printing player lines to fill the database. Leaves are not counted, as
	// a dropped one would keep its player shown as in.
	playerEventsPerMinute = 3000

	// readFailEvery is how often a console that cannot be read is logged.
	readFailEvery = time.Minute

	reasonServerStopped = "server stopped"
)

type logReader interface {
	Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error
}

// playerRecorder turns the console of one running game server into player
// records. It exists only while a server with a parser runs, and has no
// timer unless events are waiting to be written.
type playerRecorder struct {
	app       string
	parser    players.Parser
	newParser func() players.Parser
	store     *store.Store
	log       *slog.Logger
	now       func() time.Time
	// retry is how long begin waits to read the console again after a failure.
	retry time.Duration
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
	lastFull  time.Time
	missed    map[string]time.Time
	// container and lines say how far into which container's console the
	// recorder has read. They are stored with every batch, so a panel that
	// starts later knows what was recorded and what the game printed since.
	container string
	lines     int
	// cut says the log was cleared since the position was stored. If no
	// events follow, the position still has to be written, or it would keep
	// counting the lines of the log that is gone.
	cut bool
	// The allowance of the minute that began at windowStart.
	windowStart time.Time
	windowCount int
	limited     bool
}

// newPlayerRecorder returns nil for a server whose game has no parser.
func (s *Server) newPlayerRecorder(app string, g store.GameServer) *playerRecorder {
	ctx := s.baseContext()
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return nil
	}
	newParser := func() players.Parser { return players.ParserFor(stored.Source) }
	parser := newParser()
	if parser == nil {
		return nil
	}
	return &playerRecorder{
		app: app, parser: parser, newParser: newParser, store: s.Store, log: s.Log, now: s.now, retry: s.watchRetry(),
		ctx: context.WithoutCancel(ctx), missed: map[string]time.Time{},
	}
}

func (r *playerRecorder) writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.ctx, 10*time.Second)
}

// begin gets the recorder ready before the console is read. A new container
// starts with nobody in it, so visits left open by the last one are closed.
// For a server found running at startup the console is read once to teach
// the parser what the game said before. What it read is returned: those lines
// are not recorded again, except the ones the game printed after the last
// panel stopped recording, and the console uses it to tell whether the log is
// still the one that was read when its stream begins.
func (r *playerRecorder) begin(ctx context.Context, logs logReader, container string, resumed bool) (read logRead) {
	r.mu.Lock()
	r.container = container
	r.mu.Unlock()
	if !resumed {
		wctx, cancel := r.writeCtx()
		defer cancel()
		if err := r.store.CloseOpenSessions(wctx, r.app, r.now(), reasonServerStopped); err != nil {
			r.log.Error("close player sessions", "server", r.app, "err", err)
		}
		if err := r.store.SetPlayerLogPos(wctx, r.app, container, 0); err != nil {
			r.log.Error("note the console position", "server", r.app, "err", err)
		}
	} else {
		read = r.catchUp(ctx, logs, container)
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	r.prune()
	return read
}

// catchUp reads the whole console of a container that ran on while the panel
// was off, and returns what it found in it. The events of the lines the last
// panel recorded are dropped; those of the lines after them are queued. When
// the last panel's position is not known, as for a server that was started by
// an older version, all of it counts as recorded.
func (r *playerRecorder) catchUp(ctx context.Context, logs logReader, container string) logRead {
	var recorded int
	var known bool
	wctx, cancel := r.writeCtx()
	c, n, err := r.store.PlayerLogPos(wctx, r.app)
	cancel()
	switch {
	case err == nil && c == container:
		recorded, known = n, true
	case err != nil && !errors.Is(err, store.ErrNotFound):
		r.log.Error("read the console position", "server", r.app, "err", err)
	}
	// A read that fails is made again from the start with a new parser, and
	// lines whose events are queued already are not queued twice.
	queued := 0
	var failedAt time.Time
	for {
		var read logRead
		w := &lineSplitter{emit: func(line string) {
			read.add(line)
			lines := read.lines
			events := r.parser.Feed(ansi().ReplaceAllString(line, ""))
			if !known || lines <= recorded || lines <= queued {
				return
			}
			queued = lines
			r.mu.Lock()
			r.lines = lines
			batch := r.queueLocked(events)
			r.mu.Unlock()
			if batch {
				r.flush()
			}
		}}
		err := logs.Logs(ctx, container, false, 0, w)
		// A container with no log is a console with nothing in it. Asking
		// again would not make one.
		if err == nil || isNotFound(err) || ctx.Err() != nil {
			lines := read.lines
			r.mu.Lock()
			r.lines = max(r.lines, lines)
			r.mu.Unlock()
			if !known && (err == nil || isNotFound(err)) {
				// Start from here, so the next panel to find this server
				// running knows what this one has seen.
				wctx, cancel := r.writeCtx()
				err := r.store.SetPlayerLogPos(wctx, r.app, container, lines)
				cancel()
				if err != nil {
					r.log.Error("note the console position", "server", r.app, "err", err)
				}
			}
			return read
		}
		if now := r.now(); failedAt.IsZero() || now.Sub(failedAt) >= readFailEvery {
			failedAt = now
			r.log.Error("read the console to catch up on players", "server", r.app, "err", err)
		}
		r.parser = r.newParser()
		select {
		case <-ctx.Done():
		case <-time.After(r.retry):
		}
	}
}

// feed reads one console line, without colour codes.
func (r *playerRecorder) feed(line string) {
	events := r.parser.Feed(line)
	r.mu.Lock()
	if engine.IsLogCut(line) {
		// The core starts a cleared log with this line, and catchUp counts
		// the lines of the log it finds, so the position starts over here. A
		// look-alike printed by the game only makes it too small: the lines
		// after it are recorded again at the next catch-up.
		r.lines = 0
		if !r.done {
			r.cut = true
			r.armLocked()
		}
	}
	r.lines++
	batch := r.queueLocked(events)
	r.mu.Unlock()
	if batch {
		r.flush()
	}
}

// queueLocked adds the events of one line to the batch waiting to be written.
// It returns true when the batch is full.
func (r *playerRecorder) queueLocked(events []players.Event) bool {
	if r.done || len(events) == 0 {
		return false
	}
	now := r.now()
	for _, ev := range events {
		if ev.Kind == players.NearMiss {
			r.nearMissLocked(ev, now)
			continue
		}
		// A dropped leave would keep its player shown as in with the play time
		// growing. A leave stores no row of its own, so it cannot fill the
		// table.
		if ev.Kind != players.Leave && !r.allowLocked(now) {
			continue
		}
		// The console's clock is not to be trusted: a time ahead of ours
		// would keep the record out of reach of the pruning.
		if ev.At.IsZero() || ev.At.After(now) {
			ev.At = now
		}
		r.buf = append(r.buf, ev)
	}
	switch {
	case len(r.buf) >= playerFlushMax:
		return true
	case len(r.buf) > 0:
		r.armLocked()
	}
	return false
}

// armLocked makes sure what waits is written within playerFlushEvery.
func (r *playerRecorder) armLocked() {
	if r.timer == nil {
		r.timer = time.AfterFunc(playerFlushEvery, r.flush)
	}
}

// allowLocked counts an event against the allowance of the minute. Once it is
// used up the rest of the minute's events are dropped, and the log says so.
func (r *playerRecorder) allowLocked(now time.Time) bool {
	if r.windowStart.IsZero() || now.Before(r.windowStart) || now.Sub(r.windowStart) >= time.Minute {
		r.windowStart, r.windowCount, r.limited = now, 0, false
	}
	if r.windowCount++; r.windowCount <= playerEventsPerMinute {
		return true
	}
	if !r.limited {
		r.limited = true
		r.log.Warn("too many player events; dropping the rest of this minute", "server", r.app, "limit", playerEventsPerMinute)
	}
	return false
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
	batch, container, lines, cut := r.buf, r.container, r.lines, r.cut
	r.buf, r.cut = nil, false
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.mu.Unlock()
	if len(batch) == 0 {
		if !cut || container == "" {
			return
		}
		wctx, cancel := r.writeCtx()
		defer cancel()
		if err := r.store.SetPlayerLogPos(wctx, r.app, container, lines); err != nil {
			r.log.Error("note the console position", "server", r.app, "err", err)
		}
		return
	}
	wctx, cancel := r.writeCtx()
	defer cancel()
	dropped, err := r.store.RecordPlayerEventsAt(wctx, r.app, batch, container, lines)
	if err != nil {
		r.log.Error("record player events", "server", r.app, "events", len(batch), "err", err)
	} else if now := r.now(); dropped > 0 && now.Sub(r.lastFull) >= time.Minute {
		r.lastFull = now
		r.log.Warn("too many players on record; not recording the events of more", "server", r.app, "dropped", dropped)
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
	if cut, err := r.store.CapPlayerData(wctx, r.app); err != nil {
		r.log.Error("cap player records", "server", r.app, "err", err)
	} else if cut.Sessions+cut.Chat+cut.Reports > 0 {
		r.log.Warn("too many player records; removed the oldest", "server", r.app, "sessions", cut.Sessions, "chat", cut.Chat, "reports", cut.Reports)
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
