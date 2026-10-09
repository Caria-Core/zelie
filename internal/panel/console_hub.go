package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

const (
	consoleBacklog = 500
	consoleMaxLine = 8 << 10
)

// What a socket may have waiting to be sent. A client that reads slower than
// the game prints loses lines, not the server's memory. Tests lower them.
var (
	consoleQueueLines = 1024
	consoleQueueBytes = 1 << 20
)

// consoleQueueHard is how many messages other than lines may wait before the
// socket is dropped as unresponsive.
const consoleQueueHard = 4096

// waitRetryMax caps the wait between asks for a container's exit, in multiples
// of watchRetry, when the core keeps failing them.
const waitRetryMax = 30

// consoleDrain is how long a container's console is still read after the
// container exits, for the output that was still on its way.
var consoleDrain = 500 * time.Millisecond

// consolePoll is how often a watched server's state is looked at, and its
// install log read.
var consolePoll = time.Second

// lineSplitter cuts console output into lines. Colour codes stay in: the
// interface draws them. A line over consoleMaxLine is cut, and what follows
// it up to the next newline is dropped, so a game that never prints one
// cannot fill the memory.
type lineSplitter struct {
	emit     func(line string)
	rest     []byte
	skipping bool
}

func (l *lineSplitter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if l.skipping {
			if i < 0 {
				return n, nil
			}
			l.skipping = false
			p = p[i+1:]
			continue
		}
		if i < 0 {
			l.rest = append(l.rest, p...)
			if len(l.rest) > consoleMaxLine {
				l.emit(cleanLine(l.rest))
				l.rest, l.skipping = l.rest[:0], true
			}
			return n, nil
		}
		l.rest = append(l.rest, p[:i]...)
		l.emit(cleanLine(l.rest))
		l.rest = l.rest[:0]
		p = p[i+1:]
	}
	return n, nil
}

// cleanLine makes one line of what the game printed: no line ending, at most
// consoleMaxLine bytes, valid UTF-8.
func cleanLine(b []byte) string {
	if n := len(b); n > 0 && b[n-1] == '\r' {
		b = b[:n-1]
	}
	if len(b) > consoleMaxLine {
		b = b[:consoleMaxLine]
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// consoleItem is one message waiting for a socket. Lines that did not fit
// are counted in one item, in the place they were dropped.
type consoleItem struct {
	b       []byte
	skipped int
}

// consoleSub is what one socket has waiting to be written. Whoever produces
// output never waits for it.
type consoleSub struct {
	mu   sync.Mutex
	q    []consoleItem
	size int
	over bool
	wake chan struct{}
	// gone is closed when the hub lets the socket go, as when the server
	// is deleted.
	gone chan struct{}
	once sync.Once
}

func newConsoleSub() *consoleSub {
	return &consoleSub{wake: make(chan struct{}, 1), gone: make(chan struct{})}
}

func (c *consoleSub) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// push queues a message. A line that would make the queue too long is
// counted and dropped instead.
func (c *consoleSub) push(b []byte, line bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.over {
		return
	}
	switch {
	case line && (len(c.q) >= consoleQueueLines || c.size+len(b) > consoleQueueBytes):
		if n := len(c.q); n > 0 && c.q[n-1].skipped > 0 {
			c.q[n-1].skipped++
		} else {
			c.q = append(c.q, consoleItem{skipped: 1})
		}
	case !line && len(c.q) >= consoleQueueHard:
		c.over = true
	default:
		c.q = append(c.q, consoleItem{b: b})
		c.size += len(b)
	}
	c.signal()
}

// pushForced queues what has to arrive whole, such as the backlog.
func (c *consoleSub) pushForced(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.over {
		c.q = append(c.q, consoleItem{b: b})
		c.size += len(b)
	}
	c.signal()
}

// pop returns the next message. over says the socket has to be dropped.
func (c *consoleSub) pop() (b []byte, ok, over bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.over {
		return nil, false, true
	}
	if len(c.q) == 0 {
		return nil, false, false
	}
	it := c.q[0]
	c.q[0] = consoleItem{}
	c.q = c.q[1:]
	if it.skipped > 0 {
		return encodeConsole("line", fmt.Sprintf("[…%d lines skipped]", it.skipped)), true, false
	}
	c.size -= len(it.b)
	return it.b, true, false
}

func (c *consoleSub) release() { c.once.Do(func() { close(c.gone) }) }

func encodeConsole(kind, data string) []byte {
	b, _ := json.Marshal(struct {
		Type string `json:"type"`
		Data string `json:"data"`
	}{kind, data})
	return b
}

func encodeState(state string) []byte {
	b, _ := json.Marshal(struct {
		Type  string `json:"type"`
		State string `json:"state"`
	}{"state", state})
	return b
}

// consoleHub is the one place a game server's console output is read, for
// however many browsers watch it. It keeps the last lines for those that
// open later.
type consoleHub struct {
	app string

	mu        sync.Mutex
	container string
	cancel    context.CancelFunc // stops the follower of container
	loaded    bool               // there is a console to show, or none is to be looked for
	lines     []string
	eula      bool // the container's console said it wants the EULA accepted
	exit      int  // how the container's process ended, when exited is set
	exited    bool
	install   []string
	state     string
	subs      map[*consoleSub]struct{}
	stopPoll  context.CancelFunc
	taps      map[chan string]struct{}

	// pushed counts the lines the console printed, of which lines holds the
	// newest. Once the game has said it is ready, readyAt is the number of the
	// line that said so.
	pushed  int
	ready   bool
	readyAt int

	// The install log being read, and how far.
	installMu    sync.Mutex
	installID    int64
	installOff   int64
	installSplit *lineSplitter
}

// lastLines returns the newest n of lines.
func lastLines(lines []string, n int) []string {
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func appendRing(ring []string, line string) []string {
	ring = append(ring, line)
	if len(ring) >= 2*consoleBacklog {
		ring = append(ring[:0], ring[len(ring)-consoleBacklog:]...)
	}
	return ring
}

// push adds a line to the console of container, or to the install output
// when container is empty, and hands it to the sockets.
func (h *consoleHub) push(container, kind, text string) {
	b := encodeConsole(kind, text)
	h.mu.Lock()
	defer h.mu.Unlock()
	if kind == "install" {
		h.install = appendRing(h.install, text)
	} else {
		if h.container != container {
			return
		}
		h.lines = appendRing(h.lines, text)
		h.pushed++
		for tap := range h.taps {
			select {
			case tap <- text:
			default:
			}
		}
	}
	for sub := range h.subs {
		sub.push(b, true)
	}
}

// tap returns the lines the console prints from now on, for code that waits
// for one. A tap that is not read loses lines. Call stop when done.
func (h *consoleHub) tap() (lines <-chan string, stop func()) {
	ch := make(chan string, 256)
	h.mu.Lock()
	if h.taps == nil {
		h.taps = map[chan string]struct{}{}
	}
	h.taps[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.taps, ch)
		h.mu.Unlock()
	}
}

// replace makes container the console being shown. Sockets stay; the lines
// they have seen belong to the one before.
func (h *consoleHub) replace(container string, cancel context.CancelFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
	h.container, h.cancel, h.lines, h.loaded, h.eula, h.exited = container, cancel, nil, true, false, false
	h.pushed, h.ready = 0, false
}

// markReady notes that the line just printed is the one that said the game
// is ready.
func (h *consoleHub) markReady(container string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.container == container && !h.ready && h.pushed > 0 {
		h.ready, h.readyAt = true, h.pushed-1
	}
}

// beforeReady says how many of the oldest of the last n lines were printed
// before the game said it was ready, which is all of them while it has not.
// Nobody can be in the game before then, so such a line was written by the
// server alone. The caller holds h.mu.
func (h *consoleHub) beforeReady(n int) int {
	if !h.ready {
		return n
	}
	return min(max(h.readyAt-(h.pushed-n), 0), n)
}

// setExit records the exit code of container's process, for the crash
// doctor to read.
func (h *consoleHub) setExit(container string, code int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.container == container {
		h.exit, h.exited = code, true
	}
}

// refusesEULA recognises what Minecraft servers print when eula.txt does
// not say eula=true.
func refusesEULA(line string) bool {
	return strings.Contains(strings.ToLower(line), "you need to agree to the eula in order to run the server")
}

var eulaMessage = []byte(`{"type":"eula"}`)

// eulaNeeded tells the sockets that the server stopped for want of the
// EULA's acceptance.
func (h *consoleHub) eulaNeeded(container string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.container != container {
		return
	}
	// Kept for a socket that opens after the line, as one does that
	// the browser reconnects a moment later.
	h.eula = true
	for sub := range h.subs {
		sub.push(eulaMessage, false)
	}
}

func (h *consoleHub) setState(state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == state {
		return
	}
	h.state = state
	b := encodeState(state)
	for sub := range h.subs {
		sub.push(b, false)
	}
}

// subscribe adds a socket and gives it the state and what the console has
// shown so far, ahead of everything that comes after.
func (h *consoleHub) subscribe(s *Server, sub *consoleSub, state string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != state {
		h.state = state
		b := encodeState(state)
		for other := range h.subs {
			other.push(b, false)
		}
	}
	sub.pushForced(encodeState(h.state))
	for _, l := range lastLines(h.lines, consoleBacklog) {
		sub.pushForced(encodeConsole("line", l))
	}
	if h.eula {
		sub.pushForced(eulaMessage)
	}
	if h.state == "installing" {
		for _, l := range lastLines(h.install, consoleBacklog) {
			sub.pushForced(encodeConsole("install", l))
		}
	}
	if h.subs == nil {
		h.subs = map[*consoleSub]struct{}{}
	}
	h.subs[sub] = struct{}{}
	if len(h.subs) == 1 {
		ctx, cancel := context.WithCancel(s.baseContext())
		h.stopPoll = cancel
		s.watchers.Add(1)
		go func() {
			defer s.watchers.Done()
			s.pollConsole(ctx, h)
		}()
	}
}

func (h *consoleHub) unsubscribe(sub *consoleSub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, sub)
	if len(h.subs) == 0 && h.stopPoll != nil {
		h.stopPoll()
		h.stopPoll = nil
	}
}

// close lets every socket go and stops following the console.
func (h *consoleHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.cancel()
	}
	for sub := range h.subs {
		sub.release()
	}
}

type consoleHubs struct {
	mu sync.Mutex
	m  map[string]*consoleHub
}

func (c *consoleHubs) hub(app string) *consoleHub {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*consoleHub{}
	}
	h, ok := c.m[app]
	if !ok {
		h = &consoleHub{app: app}
		c.m[app] = h
	}
	return h
}

func (c *consoleHubs) forget(app string) {
	c.mu.Lock()
	h := c.m[app]
	delete(c.m, app)
	c.mu.Unlock()
	if h != nil {
		h.close()
	}
}

// consoleHistory keeps the commands each account sent to each server, for
// the arrow keys. Memory only: it is nobody's record.
type consoleHistory struct {
	mu sync.Mutex
	m  map[string]map[int64][]string
}

const consoleHistoryLen = 50

func (c *consoleHistory) add(app string, account int64, command string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]map[int64][]string{}
	}
	if c.m[app] == nil {
		c.m[app] = map[int64][]string{}
	}
	list := c.m[app][account]
	if n := len(list); n > 0 && list[n-1] == command {
		return
	}
	list = append(list, command)
	if len(list) > consoleHistoryLen {
		list = list[len(list)-consoleHistoryLen:]
	}
	c.m[app][account] = list
}

func (c *consoleHistory) get(app string, account int64) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.m[app][account]...)
}

func (c *consoleHistory) forget(app string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, app)
}

// doneMatcher finds the line that says an app is ready. Generic eggs keep a
// placeholder where a game's ready line goes. Their app is up once it runs,
// or once it answers on its port.
func doneMatcher(a store.App, e *egg.Egg) *egg.Done {
	if a.IsFiles() {
		return new(egg.Egg).DoneMatcher()
	}
	return e.DoneMatcher()
}

// watchGame follows a server's console from its start: to see when the
// game says it is ready, and to keep the lines for whoever opens the
// console. It stops when the container has exited and its output is read.
//
// resumed says the container was already running when the panel started,
// so the players in it are not new.
func (s *Server) watchGame(a store.App, container string, e *egg.Egg, g store.GameServer, resumed bool) {
	app := a.ID
	done := doneMatcher(a, e)
	eula := e.HasFeature(egg.FeatureEULA)
	h := s.consoles.hub(app)
	ctx, cancel := context.WithCancel(s.baseContext())
	h.replace(container, cancel)
	matched := done.Empty()
	mask := secretMasker(consoleSecrets(g.Variables))
	rec := s.newPlayerRecorder(app, g)
	var exited atomic.Bool
	var exitAt atomic.Int64
	answers := a.IsFiles() && a.Domain != ""
	if matched && !answers {
		// The egg says nothing to wait for.
		if s.gameRuns.advance(app, container) {
			s.gameReady(app, container)
		}
	}
	if answers {
		s.watchers.Add(1)
		go func() {
			defer s.watchers.Done()
			s.awaitAnswer(ctx, a, container, h)
		}()
	}
	s.watchers.Add(2)
	go func() {
		defer s.watchers.Done()
		defer cancel()
		go func() {
			defer s.watchers.Done()
			// An error other than "gone" is the core restarting or busy: the
			// container runs on, so ask again. Giving up would leave the
			// console followed after the server has stopped. The wait grows, so an
			// error that stays does not fill the core's log.
			retry := s.watchRetry()
			for {
				code, err := s.Core.Wait(ctx, container)
				if err == nil {
					h.setExit(container, code)
				}
				if err == nil || isNotFound(err) {
					if rec != nil {
						exitAt.Store(s.now().UnixNano())
					}
					exited.Store(true)
					select {
					case <-ctx.Done():
					case <-time.After(consoleDrain):
					}
					cancel()
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(retry):
				}
				retry = min(2*retry, waitRetryMax*s.watchRetry())
			}
		}()
		skip := 0
		if rec != nil {
			skip = rec.begin(ctx, s.Core, container, resumed)
			defer func() { rec.finish(exited.Load(), time.Unix(0, exitAt.Load())) }()
		}
		// A dropped stream is read again from its start; the lines already
		// passed on are skipped.
		delivered := 0
		for ctx.Err() == nil {
			seen := 0
			w := &lineSplitter{emit: func(line string) {
				if seen++; seen <= delivered {
					return
				}
				delivered++
				// Before anything sees the line: the egg's entrypoint prints
				// the startup command, passwords and all.
				line = mask(line)
				h.push(container, "line", line)
				plain := ansi().ReplaceAllString(line, "")
				if rec != nil && delivered > skip {
					rec.feed(plain)
				}
				if !matched && done.Match(plain) {
					matched = true
					h.markReady(container)
					if s.gameRuns.advance(app, container) {
						s.gameReady(app, container)
					}
				}
				if eula && refusesEULA(plain) {
					h.eulaNeeded(container)
				}
			}}
			s.Core.Logs(ctx, container, true, 0, w)
			select {
			case <-ctx.Done():
			case <-time.After(s.watchRetry()):
			}
		}
	}()
}

// secretMask replaces a secret in the console.
const secretMask = "••••••"

// consoleSecrets are the values of a server's variables that the console
// must not show: passwords, tokens and keys, judged by the variable's name.
// A short value would blank out ordinary words, so it is left alone.
func consoleSecrets(vars map[string]string) []string {
	var out []string
	for name, v := range vars {
		if len(v) < 6 || slices.Contains(out, v) {
			continue
		}
		name = strings.ToUpper(name)
		for _, word := range []string{"PASS", "SECRET", "TOKEN", "KEY"} {
			if strings.Contains(name, word) {
				out = append(out, v)
				break
			}
		}
	}
	return out
}

// secretMasker returns a function that blanks the secrets in a line. The
// longest goes first, so a secret that contains another is hidden whole.
func secretMasker(secrets []string) func(string) string {
	if len(secrets) == 0 {
		return func(line string) string { return line }
	}
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	pairs := make([]string, 0, 2*len(secrets))
	for _, s := range secrets {
		pairs = append(pairs, s, secretMask)
	}
	r := strings.NewReplacer(pairs...)
	return r.Replace
}

// consoleTail is how much of a stopped server's last console is read for
// someone who opens it.
const consoleTail = 256 << 10

// loadStoppedConsole fills the hub from the last container's log when nothing
// has followed a console since the panel started, so a server that crashed
// still shows why.
func (s *Server) loadStoppedConsole(ctx context.Context, h *consoleHub) {
	h.mu.Lock()
	if h.loaded {
		h.mu.Unlock()
		return
	}
	h.loaded = true
	h.mu.Unlock()
	d, err := s.Store.LiveDeployment(ctx, h.app)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	container := fmt.Sprintf("%s-%d", h.app, d.ID)
	var lines []string
	counted := &countWriter{w: &lineSplitter{emit: func(line string) { lines = append(lines, line) }}}
	if err := s.Core.Logs(ctx, container, false, consoleTail, counted); err != nil {
		return
	}
	if counted.n >= consoleTail && len(lines) > 0 {
		// The tail starts in the middle of a line.
		lines = lines[1:]
	}
	g, err := s.Store.GameServer(ctx, h.app)
	ready := -1
	switch {
	case err == nil:
		mask := secretMasker(consoleSecrets(g.Variables))
		for i := range lines {
			lines[i] = mask(lines[i])
		}
		if ready, err = s.readyLine(ctx, g, container, lines); err != nil {
			// The lines are still worth showing. Where the game became ready
			// stays unknown, which counts every line as printed before it.
			s.Log.Warn("find where the game became ready in its last console", "server", h.app, "err", err)
			ready = -1
		}
	case !errors.Is(err, store.ErrNotFound):
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.container == "" && len(h.lines) == 0 {
		h.lines, h.pushed = lastLines(lines, consoleBacklog), len(lines)
		if ready >= 0 {
			h.ready, h.readyAt = true, ready
		}
	}
}

// readyLine finds where the game said it was ready, as a place in lines, which
// are the end of container's console. A line before them gives 0, as they all
// came after it. It gives -1 when the game never said so, or the egg names no
// such line.
func (s *Server) readyLine(ctx context.Context, g store.GameServer, container string, lines []string) (int, error) {
	a, err := s.Store.App(ctx, g.AppID)
	if err != nil {
		return -1, err
	}
	stored, err := s.Store.Egg(ctx, g.EggID)
	if err != nil {
		return -1, err
	}
	e, err := egg.Parse(stored.Raw)
	if err != nil {
		return -1, err
	}
	done := doneMatcher(a, e)
	if done.Empty() {
		return -1, nil
	}
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = ansi().ReplaceAllString(l, "")
	}
	// A player can chat the words the game prints when it is ready.
	mine := players.PlayerLines(stored.Source, plain)
	for i, l := range plain {
		if !mine[i] && done.Match(l) {
			return i, nil
		}
	}

	// The tail may have cut the line off. Reading from the start of the log
	// stops at it, which is within the startup output, so this reads little
	// unless the game never got ready.
	scan, stop := context.WithCancel(ctx)
	defer stop()
	found := false
	w := &lineSplitter{emit: func(line string) {
		if !found && done.Match(ansi().ReplaceAllString(line, "")) {
			found = true
			stop()
		}
	}}
	err = s.Core.Logs(scan, container, false, 0, w)
	switch {
	case found:
		return 0, nil
	case err != nil:
		return -1, err
	}
	return -1, nil
}

type countWriter struct {
	w io.Writer
	n int
}

func (c *countWriter) Write(p []byte) (int, error) {
	c.n += len(p)
	return c.w.Write(p)
}

// consoleStatus is the state a console shows: what the server is doing,
// or that its install is running.
func (s *Server) consoleStatus(ctx context.Context, a store.App, g store.GameServer) string {
	if g.InstallState == store.InstallRunning {
		return "installing"
	}
	return s.gameState(ctx, a)
}

// pollConsole watches a server's state and, while it installs, its install
// log, for as long as there are sockets.
func (s *Server) pollConsole(ctx context.Context, h *consoleHub) {
	t := time.NewTicker(consolePoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a, err := s.Store.App(ctx, h.app)
		if err != nil {
			continue
		}
		g, err := s.Store.GameServer(ctx, h.app)
		if err != nil {
			continue
		}
		installing := g.InstallState == store.InstallRunning
		s.readInstall(h, g, installing)
		h.setState(s.consoleStatus(ctx, a, g))
	}
}

// readInstall passes on what the install script printed since the last look.
func (s *Server) readInstall(h *consoleHub, g store.GameServer, installing bool) {
	h.installMu.Lock()
	defer h.installMu.Unlock()
	if installing && g.InstallID != h.installID {
		h.mu.Lock()
		h.installID, h.installOff, h.install = g.InstallID, 0, nil
		h.installSplit = &lineSplitter{emit: func(line string) { h.push("", "install", line) }}
		h.mu.Unlock()
	}
	if h.installID == 0 || h.installID != g.InstallID {
		return
	}
	f, err := os.Open(s.deployLogPath(h.installID))
	if err == nil {
		defer f.Close()
		if _, err = f.Seek(h.installOff, io.SeekStart); err == nil {
			n, _ := io.Copy(h.installSplit, io.LimitReader(f, 1<<20))
			h.installOff += n
		}
	}
	if !installing {
		// The last of it has been read.
		h.installID = 0
	}
}

// awaitAnswer moves a files app with a domain from starting to running once
// its port answers HTTP, the way the proxy will ask. An app that never
// answers stays starting, and its console says what Zelie is waiting for.
func (s *Server) awaitAnswer(ctx context.Context, a store.App, container string, h *consoleHub) {
	deadline := time.Now().Add(startupLimit)
	told := false
	for {
		if st, err := s.containerStatus(ctx, container); err == nil && st.State == "running" && st.IP.IsValid() {
			addr := netip.AddrPortFrom(st.IP, uint16(a.Port)).String()
			if code, err := s.healthCheck(ctx, "http://"+addr+a.HealthPath, a.Domain); err == nil && code < 500 {
				s.gameRuns.advance(a.ID, container)
				return
			}
		}
		wait := startupPoll
		if time.Now().After(deadline) {
			if !told {
				told = true
				h.push(container, "line", fmt.Sprintf("Zelie has not seen the app answer on port %d yet. For %s to work it has to listen on 0.0.0.0:%d, which is the SERVER_PORT and PORT variables.", a.Port, a.Domain, a.Port))
			}
			wait = 5 * time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
