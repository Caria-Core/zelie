package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestFollowSeesAppendedData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- follow(ctx, f, &out, 10*time.Millisecond) }()

	w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("second\n")
	w.Close()

	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "second") {
		if time.Now().After(deadline) {
			t.Fatalf("appended line never arrived, got %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "first\nsecond\n" {
		t.Errorf("got %q", got)
	}
}

// A container's log is cut when it grows too large. A reader that stays where
// it was would see nothing until the file passed that place again.
func TestFollowReadsACutFileFromItsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte("a long first line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- follow(ctx, f, &out, 10*time.Millisecond) }()
	waitForOutput := func(want string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !strings.Contains(out.String(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("%q never arrived, got %q", want, out.String())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitForOutput("a long first line")

	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.WriteString("after\n")
	waitForOutput("after")

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "a long first line\nafter\n" {
		t.Errorf("got %q", got)
	}
}

// clearedAtEOF is a log file that is replaced the first time it has been read
// to its end, which is when the core clears a log it follows. Doing it there
// makes the order of events the same on every run.
type clearedAtEOF struct {
	*os.File
	once  sync.Once
	clear func()
}

func (c *clearedAtEOF) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	if errors.Is(err, io.EOF) {
		c.once.Do(c.clear)
	}
	return n, err
}

// The first line of a cleared log is the core's marker. Behind a partial
// line it would not be a line of its own, and nobody would know the log was
// cleared.
func TestFollowStartsAClearedLogOnALineOfItsOwn(t *testing.T) {
	const marker = "[zelie:log-cut 7]\n"
	tests := []struct {
		name, old, cleared string
	}{
		{"cleared log shorter than the old one", "a prompt that never ends the line", marker},
		{"cleared log already longer", "> ", marker + "output that came in at once\n"},
		{"old log ended its line", "done\n", marker + "more\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "c.log")
			if err := os.WriteFile(path, []byte(tc.old), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			f := &clearedAtEOF{File: file, clear: func() {
				if err := os.WriteFile(path, []byte(tc.cleared), 0o600); err != nil {
					t.Error(err)
				}
			}}

			ctx, cancel := context.WithCancel(context.Background())
			var out syncBuffer
			done := make(chan error, 1)
			go func() { done <- follow(ctx, f, &out, 5*time.Millisecond) }()
			want := tc.old + tc.cleared
			if !strings.HasSuffix(tc.old, "\n") {
				want = tc.old + "\n" + tc.cleared
			}
			deadline := time.Now().Add(2 * time.Second)
			for out.String() != want {
				if time.Now().After(deadline) {
					t.Fatalf("got %q, want %q", out.String(), want)
				}
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A new log that has grown past the place being read before the next look is
// not shorter than that place, so only its start gives it away.
func TestRewindNoticesALogThatGrewPastTheOldPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	old := "[zelie:log-cut 1]\nsome output\n"
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var w cutWatch
	if cut, err := w.rewindIfCut(f); err != nil || cut {
		t.Fatalf("on the first look cut is %v, err %v", cut, err)
	}
	if _, err := io.ReadAll(f); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(old+"and some more\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cut, err := w.rewindIfCut(f); err != nil || cut {
		t.Fatalf("the file only grew, but cut is %v, err %v", cut, err)
	}

	fresh := "[zelie:log-cut 2]\n" + strings.Repeat("new output\n", 20)
	if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	if cut, err := w.rewindIfCut(f); err != nil || !cut {
		t.Fatalf("a new log longer than the old position went unnoticed: cut %v, err %v", cut, err)
	}
	if pos, _ := f.Seek(0, io.SeekCurrent); pos != 0 {
		t.Errorf("the file is at %d, want its start", pos)
	}
	// What it learned of the new log is what it compares with next.
	io.ReadAll(f)
	if cut, err := w.rewindIfCut(f); err != nil || cut {
		t.Errorf("nothing happened since, but cut is %v, err %v", cut, err)
	}
}

// A file that was short when it was first seen is compared with more of its
// start later, not with the little it was.
func TestRewindLearnsMoreOfTheStartAsTheFileGrows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var w cutWatch
	w.rewindIfCut(f)
	io.ReadAll(f)
	grown := "abcdefghijklmnopqrstuvwxyz" + strings.Repeat("0123456789", 10)
	if err := os.WriteFile(path, []byte(grown), 0o600); err != nil {
		t.Fatal(err)
	}
	if cut, err := w.rewindIfCut(f); err != nil || cut {
		t.Fatalf("the file grew from what was seen: cut %v, err %v", cut, err)
	}
	io.ReadAll(f)
	// Same first bytes as before, but not the same file.
	other := "abcdefghijklmnopqrstuvwxyz" + strings.Repeat("9876543210", 12)
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	if cut, err := w.rewindIfCut(f); err != nil || !cut {
		t.Errorf("a different file with the same short start went unnoticed: cut %v, err %v", cut, err)
	}
}

// follow can start in the middle of a file, as a tail view does. That is no
// reason to miss that the file is cleared and grows past the place it was
// read to.
func TestFollowFromTheMiddleNoticesALogThatGrewPastTheOldPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	if err := os.WriteFile(path, []byte("first line\nsecond line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Seek(int64(len("first line\n")), io.SeekStart); err != nil {
		t.Fatal(err)
	}
	const fresh = "[zelie:log-cut 5]\nthe first line of the new log, long enough to pass the old place\n"
	f := &clearedAtEOF{File: file, clear: func() {
		if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
			t.Error(err)
		}
	}}

	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- follow(ctx, f, &out, 5*time.Millisecond) }()
	want := "second line\n" + fresh
	deadline := time.Now().Add(2 * time.Second)
	for out.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("got %q, want %q", out.String(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// clearedAfterRead is a log file that is replaced right after its first read
// that returns data, which is what happens to a follower that is far behind: it
// is in the middle of reading when the core clears the log.
type clearedAfterRead struct {
	*os.File
	once  sync.Once
	clear func()
}

func (c *clearedAfterRead) Read(p []byte) (int, error) {
	n, err := c.File.Read(p)
	if n > 0 {
		c.once.Do(c.clear)
	}
	return n, err
}

// A follower that is still catching up when the log is cleared, and that finds
// the new log longer than the place it had reached, would read it from the
// middle and again from its start. The fragment would show twice.
func TestFollowNoticesAClearedLogWhileItIsStillCatchingUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.log")
	old := strings.Repeat("an old line\n", 4000)
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fresh := "[zelie:log-cut 9]\n" + strings.Repeat("a new line\n", 8000)
	f := &clearedAfterRead{File: file, clear: func() {
		if err := os.WriteFile(path, []byte(fresh), 0o600); err != nil {
			t.Error(err)
		}
	}}

	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuffer
	done := make(chan error, 1)
	go func() { done <- follow(ctx, f, &out, 5*time.Millisecond) }()
	// One read of the old log, which ends in the middle of a line.
	read := old[:32<<10]
	want := read + "\n" + fresh
	deadline := time.Now().Add(2 * time.Second)
	for out.String() != want {
		if time.Now().After(deadline) {
			t.Fatalf("got %d bytes, want %d; it ends with %q", len(out.String()), len(want), tail(out.String(), 60))
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
