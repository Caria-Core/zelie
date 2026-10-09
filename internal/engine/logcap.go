package engine

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxLogBytes is how large a container's output log may grow. The shim
// appends to the file for as long as the container runs, so without a limit a
// container that prints without end fills the host's disk.
const maxLogBytes = 64 << 20

// logCutPrefix starts the line that opens a log the core has cleared. It is a
// marker rather than a sentence so that each interface can say it in its own
// language, and readers of the log can tell where it was cut. The number makes
// every cut different: a reader that was away can tell whether this is the cut
// it knows.
const logCutPrefix = "[zelie:log-cut "

func logCutLine(at time.Time) string {
	return logCutPrefix + strconv.FormatInt(at.UnixMilli(), 10) + "]"
}

// IsLogCut reports whether line is the one the core writes at the start of a
// log it has cleared. It is always the first line of such a log.
func IsLogCut(line string) bool {
	n, ok := strings.CutPrefix(line, logCutPrefix)
	if !ok {
		return false
	}
	n, ok = strings.CutSuffix(n, "]")
	_, err := strconv.ParseUint(n, 10, 64)
	return ok && err == nil
}

// logCapEvery is how often the logs of running containers are measured.
// Between two checks a log can grow past the limit, but not without bound.
var logCapEvery = 2 * time.Second

// logRulesEvery is how often the loop checks the host's firewall rules.
var logRulesEvery = hostInputEvery

// logCap measures the logs of running containers. Its loop runs only while a
// container does: it starts with the first one and ends when none is left.
// With containers running, it also checks the host's firewall rules, which
// otherwise are looked at only when something asks for a name or a container
// changes.
type logCap struct {
	mu      sync.Mutex
	ctx     context.Context
	running bool
	// rules checks the firewall rules; nil for none.
	rules func(context.Context)
}

// StartLogCap keeps container logs under maxLogBytes until ctx is cancelled.
// Before it, nothing is measured. Containers that outlived the core need
// measuring from the start; with none running the loop waits for the first.
func (e *Engine) StartLogCap(ctx context.Context) {
	e.logs.mu.Lock()
	e.logs.ctx = ctx
	e.logs.mu.Unlock()
	if len(runningContainers(cgroupRoot)) > 0 {
		e.watchLogs()
	}
}

// watchLogs starts the loop unless it runs. It is called when a container
// starts.
func (e *Engine) watchLogs() {
	l := &e.logs
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx == nil || l.running {
		return
	}
	l.running = true
	go e.capLogs(l.ctx)
}

func (e *Engine) capLogs(ctx context.Context) {
	t := time.NewTicker(logCapEvery)
	defer t.Stop()
	rules := time.NewTicker(logRulesEvery)
	defer rules.Stop()
	for {
		select {
		case <-ctx.Done():
			e.logs.mu.Lock()
			e.logs.running = false
			e.logs.mu.Unlock()
			return
		case <-rules.C:
			if e.logs.rules != nil {
				e.logs.rules(ctx)
			}
			continue
		case <-t.C:
		}
		if !e.capLogsOnce() {
			return
		}
	}
}

// capLogsOnce cuts the logs that are too large. It reports false, and ends
// the loop, when no container runs. The decision is made under the lock that
// watchLogs takes, so a container that starts right now starts a new loop.
func (e *Engine) capLogsOnce() bool {
	l := &e.logs
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := runningContainers(cgroupRoot)
	if len(ids) == 0 {
		l.running = false
		return false
	}
	log := e.logger()
	for _, id := range ids {
		size, err := capLog(LogPathFor(e.paths, id), maxLogBytes, logCutLine(time.Now()))
		if size > 0 {
			log.Warn("container log reached its limit and was cleared", "container", id, "bytes", size)
		}
		if err != nil {
			log.Error("limit container log", "container", id, "err", err)
		}
	}
	return true
}

// runningContainers lists the containers that have a cgroup under dir, which
// is where the systemd driver puts a container for as long as it runs (see
// Usage).
func runningContainers(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var ids []string
	for _, d := range entries {
		id, ok := strings.CutPrefix(d.Name(), "zelie-")
		if !ok {
			continue
		}
		if id, ok = strings.CutSuffix(id, ".scope"); ok && ValidID(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// capLog empties the file at path once it is larger than max and starts it
// again with the line cut, which the interfaces show in the language of the
// reader. It returns the size the file had when it was cut, or zero when it
// was left alone. The shim has the file open for appending, so what it writes
// next lands after the line and no zero-filled gap is left behind. The line is
// written at the start rather than appended: a container that prints in the
// moment between clearing and writing would otherwise put its output ahead of
// the line, and a reader that looks for the line first would not find it. The
// bytes of that output the line covers are lost, which is nothing next to what
// the cut throws away.
func capLog(path string, max int64, cut string) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.Size() <= max {
		return 0, err
	}
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	_, err = f.WriteAt([]byte(cut+"\n"), 0)
	return st.Size(), err
}
