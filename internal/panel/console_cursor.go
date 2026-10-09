package panel

import (
	"slices"

	"github.com/Caria-Core/zelie/internal/engine"
)

// maxCutsKept is how many lines that look like the core's cut line a cursor
// remembers, and the recorder notes of what it read. A real cut comes with
// every 64 MiB of output, so a few are enough. The limit is for a container
// that prints such lines on purpose. If it prints this many after a real cut,
// the cursor forgets the real one and the next reconnect reads the log from
// its start. The recorder keeps the first ones it finds and trusts no others.
const maxCutsKept = 32

// logCursor remembers how much of a container's log has been passed on, so
// that a stream read again from the log's start carries on where the last one
// stopped. Lines are counted over all streams. A log is told apart by its
// first line: the core clears a log that grows too large and starts the
// cleared file with a cut line (see engine.IsLogCut), different each time.
//
// The container writes to the log too, so a line that looks like a cut line
// proves nothing. The cursor only notes where each one was passed on, and
// believes it when a stream starts with it. A stream whose first line it does
// not know is a log of which nothing has been passed on.
type logCursor struct {
	started bool
	total   int    // lines passed on, over all streams
	head    string // first line of the log, as the latest stream found it
	headAt  int    // how many lines had been passed on before head
	cuts    []cutSeen

	// ahead is what the player recorder read before the first stream began.
	ahead logRead
}

// cutSeen is a line that looked like the core's cut line, and where it was.
type cutSeen struct {
	line string
	at   int
}

// logRead is what the player recorder found in a container's log before the
// console's first stream began.
type logRead struct {
	lines int // how many lines it read
	// first is the first of them, which says whether the log is still the one
	// that was read when the stream begins.
	first string
	// cuts are the lines that look like the core's cut line, and where. The
	// stream tells them from a log that was cleared meanwhile by what stands
	// in their place.
	cuts []cutSeen
	// lostFrom is where the first look-alike was that did not fit in cuts,
	// when lost is set. From there on cuts says nothing about the log.
	lost     bool
	lostFrom int
}

// add notes the next line of the log.
func (r *logRead) add(line string) {
	if r.lines == 0 {
		r.first = line
	}
	if engine.IsLogCut(line) {
		switch {
		case len(r.cuts) < maxCutsKept:
			r.cuts = append(r.cuts, cutSeen{line, r.lines})
		case !r.lost:
			r.lost, r.lostFrom = true, r.lines
		}
	}
	r.lines++
}

// notes reports whether cuts has every look-alike of the log's line at index.
func (r *logRead) notes(index int) bool { return !r.lost || index < r.lostFrom }

// resume takes the first line of a stream read from the log's start and
// returns how many lines of the stream were passed on already.
func (c *logCursor) resume(first string) int {
	if !c.started {
		c.started, c.head, c.headAt = true, first, c.total
		if first != c.ahead.first {
			// Cleared since the recorder read it: none of this was read.
			c.ahead = logRead{}
		}
		return 0
	}
	if first == c.head {
		return c.total - c.headAt
	}
	for _, k := range c.cuts {
		if k.line == first {
			c.head, c.headAt = first, k.at
			return c.total - k.at
		}
	}
	// Another log. Nothing of it was passed on, and the recorder has not read
	// it either.
	c.head, c.headAt, c.ahead = first, c.total, logRead{}
	return 0
}

// passed counts a line that was passed on.
func (c *logCursor) passed(line string) {
	if engine.IsLogCut(line) {
		if c.total < c.ahead.lines && c.ahead.notes(c.total) && !slices.Contains(c.ahead.cuts, cutSeen{line, c.total}) {
			// The recorder read something else in this place, so the log was
			// cleared while the stream was still going over what it read.
			// Nothing from here on was read before. Past the last look-alike
			// the recorder kept there is no telling a cleared log from one
			// more look-alike, and reading lines twice does more harm than
			// missing a cut: it would record the same chat and visits again.
			c.ahead.lines = c.total
		}
		if c.total > c.headAt && !c.sawCut(line) {
			if len(c.cuts) == maxCutsKept {
				c.cuts = append(c.cuts[:0], c.cuts[1:]...)
			}
			c.cuts = append(c.cuts, cutSeen{line, c.total})
		}
	}
	c.total++
}

func (c *logCursor) sawCut(line string) bool {
	for _, k := range c.cuts {
		if k.line == line {
			return true
		}
	}
	return false
}

// readBefore reports whether the line just passed on is one of the lines the
// player recorder read before the first stream began.
func (c *logCursor) readBefore() bool { return c.total <= c.ahead.lines }
