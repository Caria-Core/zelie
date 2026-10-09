package panel

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

const (
	cutOne = "[zelie:log-cut 1760000000000]"
	cutTwo = "[zelie:log-cut 1760000009000]"
)

// read is a stream of the log's lines as watchGame handles them. It returns
// the lines passed on, and whether the player recorder would have been given
// each of them.
func (c *logCursor) read(log []string, upTo int) (passed []string, recorded []bool) {
	var skipping int
	for i, line := range log {
		if i == upTo {
			break
		}
		if i == 0 {
			skipping = c.resume(line)
		}
		if skipping > 0 {
			skipping--
			continue
		}
		c.passed(line)
		passed = append(passed, line)
		recorded = append(recorded, !c.readBefore())
	}
	return passed, recorded
}

// readAhead is a cursor for a log the player recorder read n lines of before
// the first stream began.
func readAhead(log []string, n int) *logCursor {
	var read logRead
	for _, line := range log[:n] {
		read.add(line)
	}
	return &logCursor{ahead: read}
}

func TestCursorSkipsWhatWasPassedOnAfterAReconnect(t *testing.T) {
	var c logCursor
	log := []string{"a", "b", "c"}
	got, _ := c.read(log, -1)
	if !slices.Equal(got, log) {
		t.Fatalf("first stream: %q", got)
	}
	log = append(log, "d")
	if got, _ := c.read(log, -1); !slices.Equal(got, []string{"d"}) {
		t.Errorf("after a reconnect: %q, want only the new line", got)
	}
	if got, _ := c.read(log, -1); len(got) != 0 {
		t.Errorf("a reconnect with nothing new passed on %q", got)
	}
}

// After a log is cleared, counting every line since the panel began would hide
// the new output until the container had printed that many lines again.
func TestCursorCountsFromTheCutLine(t *testing.T) {
	var c logCursor
	old := make([]string, 1000)
	for i := range old {
		old[i] = "old"
	}
	c.read(old, -1)
	// The log was cleared while the stream was away. It starts with a cut
	// line the cursor has not seen, so all of it is new.
	rewound := []string{cutOne, "x", "y"}
	if got, _ := c.read(rewound, -1); !slices.Equal(got, rewound) {
		t.Fatalf("after the cut: %q", got)
	}
	log := append(rewound, "z")
	if got, _ := c.read(log, -1); !slices.Equal(got, []string{"z"}) {
		t.Errorf("reconnect after a cut: %q, want only z", got)
	}
}

func TestCursorFollowsACutInTheSameStream(t *testing.T) {
	var c logCursor
	first := []string{"a", "b", "c"}
	c.read(first, -1)
	// What follows on the same connection: the cut line arrives like any
	// other line, in the middle of the stream.
	for _, line := range []string{cutOne, "x"} {
		c.passed(line)
	}
	if got, _ := c.read([]string{cutOne, "x", "y"}, -1); !slices.Equal(got, []string{"y"}) {
		t.Errorf("reconnect: %q, want only y", got)
	}
}

func TestCursorTakesAnotherCutAsNew(t *testing.T) {
	var c logCursor
	c.read([]string{cutOne, "x", "y", "z"}, -1)
	// Cleared again while the stream was away: nothing of this file was seen.
	log := []string{cutTwo, "p", "q"}
	if got, _ := c.read(log, -1); !slices.Equal(got, log) {
		t.Errorf("after a second cut: %q, want everything", got)
	}
	if got, _ := c.read(append(log, "r"), -1); !slices.Equal(got, []string{"r"}) {
		t.Errorf("after that: %q, want only r", got)
	}
}

func TestCursorTakesALogWithoutItsCutLineAsNew(t *testing.T) {
	var c logCursor
	c.read([]string{cutOne, "x", "y"}, -1)
	// A log that no longer starts with a cut line is another file.
	log := []string{"fresh", "start"}
	if got, _ := c.read(log, -1); !slices.Equal(got, log) {
		t.Errorf("got %q", got)
	}
}

// The container writes to its log, so it can print a line that looks like the
// core's cut line. That must not make the cursor think the log was cleared:
// on the next reconnect the log still starts with its real first line, and
// the rest of it was passed on already.
func TestCursorIgnoresACutLinePrintedByTheContainer(t *testing.T) {
	log := []string{"start", "a", cutOne, "b", "c", "d", "e", "f"}
	// The recorder read the first 5 lines, the fake cut line among them.
	c := readAhead(log, 5)
	_, recorded := c.read(log[:7], -1)
	if want := []bool{false, false, false, false, false, true, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
	got, recorded := c.read(log, -1)
	if !slices.Equal(got, []string{"f"}) || !slices.Equal(recorded, []bool{true}) {
		t.Errorf("after the reconnect: passed %q, recorded %v, want only f recorded", got, recorded)
	}
	if got, _ := c.read(log, -1); len(got) != 0 {
		t.Errorf("a second reconnect passed on %q", got)
	}
}

// A log can be cleared while the first stream is still going over the lines
// the recorder read. From the cut line on, the stream carries lines the
// recorder has not seen, which must reach it.
func TestCursorRecordsALogClearedWhileTheFirstStreamReplays(t *testing.T) {
	log := []string{"a", "b", "c", "d", "e", "f"}
	c := readAhead(log, 5)
	if _, recorded := c.read(log[:2], -1); !slices.Equal(recorded, []bool{false, false}) {
		t.Fatalf("replay recorded %v", recorded)
	}
	var recorded []bool
	for _, line := range []string{cutOne, "x", "y"} {
		c.passed(line)
		recorded = append(recorded, !c.readBefore())
	}
	if want := []bool{true, true, true}; !slices.Equal(recorded, want) {
		t.Errorf("after the cut recorded %v, want %v", recorded, want)
	}
	// The reconnect carries on from the cut line, and nothing is read twice.
	got, recorded := c.read([]string{cutOne, "x", "y", "z"}, -1)
	if !slices.Equal(got, []string{"z"}) || !slices.Equal(recorded, []bool{true}) {
		t.Errorf("after the reconnect: passed %q, recorded %v", got, recorded)
	}
}

// The same when the recorder read a cut line in that place, of another cut.
func TestCursorRecordsALogClearedAgainWhileTheFirstStreamReplays(t *testing.T) {
	log := []string{cutOne, "a", "b", "c", "d"}
	c := readAhead(log, 5)
	_, recorded := c.read([]string{cutOne, "a"}, -1)
	for _, line := range []string{cutTwo, "x"} {
		c.passed(line)
		recorded = append(recorded, !c.readBefore())
	}
	if want := []bool{false, false, true, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
}

// A cut line the recorder read in the same place is the same line, however
// many times the container prints it: the log was not cleared.
func TestCursorSkipsALookAlikeTheRecorderReadInTheSamePlace(t *testing.T) {
	log := []string{"start", cutOne, "a", cutTwo, "b", "c", "d"}
	c := readAhead(log, 6)
	_, recorded := c.read(log, -1)
	if want := []bool{false, false, false, false, false, false, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
}

// A look-alike in another place than the recorder read it is a cut line.
func TestCursorTakesALookAlikeInAnotherPlaceAsACut(t *testing.T) {
	log := []string{"start", cutOne, "a", "b", "c", "d"}
	c := readAhead(log, 5)
	c.read(log[:1], -1)
	var recorded []bool
	for _, line := range []string{"x", cutOne, "y"} {
		c.passed(line)
		recorded = append(recorded, !c.readBefore())
	}
	if want := []bool{false, true, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
}

// A container that printed look-alikes without end before the recorder read
// the log must not make the recorder keep them all.
func TestLogReadKeepsFewCutLines(t *testing.T) {
	var read logRead
	for i := range 1000 {
		read.add(fmt.Sprintf("[zelie:log-cut %d]", i))
	}
	if read.lines != 1000 || len(read.cuts) != maxCutsKept {
		t.Errorf("%d lines, %d cut lines kept, want 1000 and %d", read.lines, len(read.cuts), maxCutsKept)
	}
}

// lookAlikeLog is a log in which a game printed a look-alike of the cut line
// before every chat line, more of them than a recorder keeps notes of.
func lookAlikeLog(pairs int) []string {
	log := []string{"start"}
	for i := range pairs {
		log = append(log, fmt.Sprintf("[zelie:log-cut %d]", i), fmt.Sprintf("<Steve> chat %d", i))
	}
	return log
}

// With more look-alikes than the recorder keeps, the ones it did not keep are
// not cuts either. Taking one for a cut would hand the recorder every line
// from there on a second time.
func TestCursorDoesNotRecordAgainWhenTheRecorderReadManyLookAlikes(t *testing.T) {
	log := lookAlikeLog(maxCutsKept + 8)
	c := readAhead(log, len(log))
	if !c.ahead.lost {
		t.Fatalf("the recorder kept %d look-alikes of %d without losing track", len(c.ahead.cuts), maxCutsKept+8)
	}
	_, recorded := c.read(log, -1)
	for i, again := range recorded {
		if again {
			t.Fatalf("line %d (%q) was given to the recorder a second time", i, log[i])
		}
	}
	if got, _ := c.read(log, -1); len(got) != 0 {
		t.Errorf("a reconnect passed on %d lines again", len(got))
	}
}

// A cut among the look-alikes the recorder kept is still found when it read
// more of them than it keeps: what follows is new to it.
func TestCursorFindsACutAmongTheLookAlikesTheRecorderKept(t *testing.T) {
	log := lookAlikeLog(maxCutsKept + 8)
	c := readAhead(log, len(log))
	// The stream goes over 7 lines, then the log is cleared.
	c.read(log[:7], -1)
	var recorded []bool
	for _, line := range []string{cutTwo, "<Steve> after the cut"} {
		c.passed(line)
		recorded = append(recorded, !c.readBefore())
	}
	if want := []bool{true, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
}

// A real cut comes after the container's own look-alike. The cursor has to
// tell where the real one began, whatever came before it.
func TestCursorFindsTheRealCutAmongLookAlikes(t *testing.T) {
	var c logCursor
	c.read([]string{"start", cutOne, "a"}, -1)
	// The core clears the log. cutTwo is the real marker, and the container
	// goes on printing cutOne.
	c.passed(cutTwo)
	c.passed(cutOne)
	c.passed("b")
	log := []string{cutTwo, cutOne, "b", "c"}
	if got, _ := c.read(log, -1); !slices.Equal(got, []string{"c"}) {
		t.Errorf("reconnect: %q, want only c", got)
	}
}

// A container that prints cut lines without end must not make the cursor
// grow with them, nor make it forget the log it is following.
func TestCursorKeepsFewCutLines(t *testing.T) {
	var c logCursor
	log := []string{"start"}
	c.read(log, -1)
	for i := range 1000 {
		line := fmt.Sprintf("[zelie:log-cut %d]", i)
		c.passed(line)
		log = append(log, line)
	}
	if len(c.cuts) > maxCutsKept {
		t.Errorf("%d cut lines kept, at most %d", len(c.cuts), maxCutsKept)
	}
	log = append(log, "new")
	if got, _ := c.read(log, -1); !slices.Equal(got, []string{"new"}) {
		t.Errorf("reconnect: %q, want only the new line", got)
	}
}

func TestCursorKeepsWhatTheRecorderReadBeforeUntilTheLogIsCleared(t *testing.T) {
	// The panel found the server running and the recorder read its 3 lines.
	log := []string{"a", "b", "c", "d", "e"}
	c := readAhead(log, 3)
	_, recorded := c.read(log, 4)
	if want := []bool{false, false, false, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
	// The stream drops after the fourth line and is read again.
	_, recorded = c.read(log, -1)
	if want := []bool{true}; !slices.Equal(recorded, want) {
		t.Errorf("after the reconnect: recorded %v, want %v", recorded, want)
	}
	// A log that was cut before the recorder read it counts its cut line too.
	cutLog := []string{cutOne, "a", "b", "c"}
	_, recorded = readAhead(cutLog, 2).read(cutLog, -1)
	if want := []bool{false, false, true, true}; !slices.Equal(recorded, want) {
		t.Errorf("a cut log read before: recorded %v, want %v", recorded, want)
	}
	// Once the log is cleared none of its lines were read before, even if
	// there are fewer than the recorder read.
	_, recorded = c.read([]string{cutTwo, "x"}, -1)
	if want := []bool{true, true}; !slices.Equal(recorded, want) {
		t.Errorf("after the log was cleared: recorded %v, want %v", recorded, want)
	}
}

// The log can be cleared in the moment between the recorder reading it and
// the first stream starting. The lines the stream finds are not the ones the
// recorder read, so none of them may be skipped.
func TestCursorRecordsALogClearedBeforeTheFirstStream(t *testing.T) {
	read := []string{"a", "b", "c", "d"}
	c := readAhead(read, len(read))
	cleared := []string{cutOne, "x", "y", "z"}
	got, recorded := c.read(cleared, -1)
	if !slices.Equal(got, cleared) {
		t.Fatalf("passed %q", got)
	}
	if want := []bool{true, true, true, true}; !slices.Equal(recorded, want) {
		t.Errorf("recorded %v, want %v", recorded, want)
	}
}

// The splitter drops what follows a line that is too long, up to the next
// newline. The core ends the line that is open when it clears a log, so the
// cut line is not what gets dropped.
func TestCutLineSurvivesALongLineThatWasCut(t *testing.T) {
	var lines []string
	w := &lineSplitter{emit: func(line string) { lines = append(lines, line) }}
	io.WriteString(w, strings.Repeat("x", consoleMaxLine+100))
	io.WriteString(w, "\n"+cutOne+"\nafter\n")
	if len(lines) != 3 || lines[1] != cutOne || lines[2] != "after" {
		t.Errorf("lines %q", lines)
	}
}

// consoleProbe is a running game server whose console and chat records a test
// looks at.
type consoleProbe struct {
	t  *testing.T
	e  *appEnv
	id string
	h  *consoleHub
}

func newConsoleProbe(t *testing.T) *consoleProbe {
	e, id := newPlayerGame(t)
	return &consoleProbe{t: t, e: e, id: id, h: e.s.consoles.hub("survival")}
}

func (p *consoleProbe) lastLine() string {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	if len(p.h.lines) == 0 {
		return ""
	}
	return p.h.lines[len(p.h.lines)-1]
}

// printed makes the game print a line and waits for the console to show it.
func (p *consoleProbe) printed(text string) {
	p.t.Helper()
	p.e.core.emit(p.id, text+"\n")
	waitFor(p.t, func() bool { return p.lastLine() == text })
}

func (p *consoleProbe) chat() int {
	list, _ := p.e.s.Store.PlayerChat(context.Background(), "survival", "", 0, 100)
	return len(list)
}

// shown is how many times the console shows a line.
func (p *consoleProbe) shown(text string) (n int) {
	p.h.mu.Lock()
	defer p.h.mu.Unlock()
	for _, line := range p.h.lines {
		if line == text {
			n++
		}
	}
	return n
}

// reconnect drops the stream that follows the log and waits for the next one.
func (p *consoleProbe) reconnect() {
	p.t.Helper()
	before := p.e.core.logStreams(p.id)
	p.e.core.dropLogs(p.id)
	waitFor(p.t, func() bool { return p.e.core.logStreams(p.id) > before })
}

// The console of a game whose log was cleared and whose stream was dropped
// afterwards: the lines printed since must still arrive, in the console and
// in the player records.
func TestConsoleKeepsReadingAfterTheLogIsClearedAndTheStreamDrops(t *testing.T) {
	p := newConsoleProbe(t)
	e, id, h := p.e, p.id, p.h

	p.printed("[12:00:00 INFO]: Steve joined the game")
	for i := range 10 {
		p.printed(fmt.Sprintf("[12:00:%02d INFO]: lots of earlier output", i))
	}
	// The game was waiting at a prompt, on a line it had not ended. The core
	// ends that line itself before the cleared log, so the cut line is a
	// line of its own for the console.
	e.core.emit(id, "> ")
	waitFor(t, func() bool { return e.core.logCaughtUp(id) })
	e.core.cutLog(id, cutOne)
	waitFor(t, func() bool { return p.lastLine() == cutOne })
	p.printed("[12:01:00 INFO]: <Steve> after the cut")
	waitFor(t, func() bool { return p.chat() == 1 })

	// The core goes away and comes back: the log is read again from its start.
	p.reconnect()
	p.printed("[12:02:00 INFO]: <Steve> after the reconnect")
	waitFor(t, func() bool { return p.chat() == 2 })

	h.mu.Lock()
	got := strings.Join(h.lines[len(h.lines)-4:], "\n")
	h.mu.Unlock()
	want := strings.Join([]string{"> ", cutOne, "[12:01:00 INFO]: <Steve> after the cut", "[12:02:00 INFO]: <Steve> after the reconnect"}, "\n")
	if got != want {
		t.Errorf("the console ends with\n%s\nwant it to end with\n%s", got, want)
	}

	// Cleared again while the stream was away: all of the new log is new.
	before := e.core.logStreams(id)
	e.core.dropLogs(id)
	e.core.cutLog(id, cutTwo)
	waitFor(t, func() bool { return e.core.logStreams(id) > before })
	p.printed("[12:03:00 INFO]: <Steve> after the second cut")
	waitFor(t, func() bool { return p.chat() == 3 })
	if n := p.shown("[12:03:00 INFO]: <Steve> after the second cut"); n != 1 {
		t.Errorf("the line after the second cut was shown %d times", n)
	}
}

// A game can print a line that looks like the core's cut line. The log was not
// cleared, so a reconnect must not read it all again: the console would show
// every line twice and the players' chat would be recorded twice.
func TestConsoleIgnoresACutLinePrintedByTheGame(t *testing.T) {
	p := newConsoleProbe(t)
	p.printed("[12:00:00 INFO]: Steve joined the game")
	p.printed("[12:00:30 INFO]: <Steve> before the fake")
	waitFor(t, func() bool { return p.chat() == 1 })
	p.printed(cutOne)
	p.printed("[12:01:00 INFO]: <Steve> after the fake")
	waitFor(t, func() bool { return p.chat() == 2 })

	p.reconnect()
	p.printed("[12:02:00 INFO]: <Steve> after the reconnect")
	waitFor(t, func() bool { return p.chat() == 3 })

	for _, line := range []string{"[12:00:00 INFO]: Steve joined the game", "[12:00:30 INFO]: <Steve> before the fake", cutOne, "[12:01:00 INFO]: <Steve> after the fake"} {
		if n := p.shown(line); n != 1 {
			t.Errorf("%q was shown %d times", line, n)
		}
	}
	if n := p.chat(); n != 3 {
		t.Errorf("%d chat messages were recorded, want 3", n)
	}
}
