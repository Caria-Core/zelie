package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/egg"
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
	install   []string
	state     string
	subs      map[*consoleSub]struct{}
	stopPoll  context.CancelFunc
	taps      map[chan string]struct{}

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
	h.container, h.cancel, h.lines, h.loaded, h.eula = container, cancel, nil, true, false
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

// watchGame follows a server's console from its start: to see when the
// game says it is ready, and to keep the lines for whoever opens the
// console. It stops when the container has exited and its output is read.
func (s *Server) watchGame(a store.App, container string, e *egg.Egg) {
	app := a.ID
	if a.IsFiles() {
		// Generic eggs keep a placeholder where a game's ready line goes.
		// Their app is up once it runs, or once it answers on its port.
		e = new(*e)
		e.Done = nil
	}
	done := e.DoneMatcher()
	eula := e.HasFeature(egg.FeatureEULA)
	h := s.consoles.hub(app)
	ctx, cancel := context.WithCancel(s.baseContext())
	h.replace(container, cancel)
	matched := done.Empty()
	answers := a.IsFiles() && a.Domain != ""
	if matched && !answers {
		// The egg says nothing to wait for.
		s.gameRuns.advance(app, container)
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
			// An error other than "gone" is the core being busy; the
			// console is still worth reading.
			if _, err := s.Core.Wait(ctx, container); err == nil || isNotFound(err) {
				select {
				case <-ctx.Done():
				case <-time.After(consoleDrain):
				}
				cancel()
			}
		}()
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
				h.push(container, "line", line)
				plain := ansi.ReplaceAllString(line, "")
				if !matched && done.Match(plain) {
					matched = true
					s.gameRuns.advance(app, container)
				}
				if eula && refusesEULA(plain) {
					h.eulaNeeded(container)
				}
			}}
			s.Core.Logs(ctx, container, true, 0, w)
			select {
			case <-ctx.Done():
			case <-time.After(watchRetry):
			}
		}
	}()
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
	var lines []string
	counted := &countWriter{w: &lineSplitter{emit: func(line string) { lines = append(lines, line) }}}
	if err := s.Core.Logs(ctx, fmt.Sprintf("%s-%d", h.app, d.ID), false, consoleTail, counted); err != nil {
		return
	}
	if counted.n >= consoleTail && len(lines) > 0 {
		// The tail starts in the middle of a line.
		lines = lines[1:]
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.container == "" && len(h.lines) == 0 {
		h.lines = lastLines(lines, consoleBacklog)
	}
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
