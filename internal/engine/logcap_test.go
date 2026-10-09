package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "web.log")
	cut := logCutLine(time.UnixMilli(1760000000123))
	if size, err := capLog(path, 200, cut); size != 0 || err != nil {
		t.Errorf("a missing log: %d, %v", size, err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 150), 0o600); err != nil {
		t.Fatal(err)
	}
	if size, err := capLog(path, 200, cut); size != 0 || err != nil {
		t.Errorf("a small log: %d, %v", size, err)
	}
	if b, _ := os.ReadFile(path); len(b) != 150 {
		t.Fatalf("a small log was changed: %d bytes", len(b))
	}

	// The shim keeps the file open for appending, as it does for a container.
	shim, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer shim.Close()
	shim.WriteString(string(bytes.Repeat([]byte("y"), 100)))

	size, err := capLog(path, 200, cut)
	if size != 250 || err != nil {
		t.Fatalf("a large log: %d, %v", size, err)
	}
	shim.WriteString("after the cut\n")
	b, _ := os.ReadFile(path)
	if bytes.IndexByte(b, 0) >= 0 {
		t.Error("the shim wrote past the end of a log that was cut: the file has a gap")
	}
	if want := "[zelie:log-cut 1760000000123]\n"; !bytes.HasPrefix(b, []byte(want)) || !bytes.HasSuffix(b, []byte("after the cut\n")) || bytes.Contains(b, []byte("x")) {
		t.Errorf("log after the cut: %q", b)
	}
}

func TestLogCutLineIsRecognised(t *testing.T) {
	at := time.UnixMilli(1760000000123)
	if line := logCutLine(at); !IsLogCut(line) {
		t.Errorf("%q is not recognised as the line the core writes", line)
	}
	if logCutLine(at) == logCutLine(at.Add(2*time.Second)) {
		t.Error("two cuts wrote the same line, so a reader cannot tell them apart")
	}
	for _, line := range []string{"", "[zelie:log-cut]", "[zelie:log-cut x]", "[zelie:log-cut 17600", "[zelie:log-cut 1760000000123] more", "log-cut 1760000000123", "[zelie] The log was cleared."} {
		if IsLogCut(line) {
			t.Errorf("%q is taken for a cut line", line)
		}
	}
}

func TestRunningContainers(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"zelie-web.scope", "zelie-it-db2.scope", "zelie-Bad.scope", "zelie-.scope", "other.scope", "zelie-web.slice"} {
		os.Mkdir(filepath.Join(dir, name), 0o755)
	}
	if got, want := runningContainers(dir), []string{"it-db2", "web"}; !slices.Equal(got, want) {
		t.Errorf("running = %v, want %v", got, want)
	}
	if got := runningContainers(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("running without a cgroup: %v", got)
	}
}

func TestLogsAreCappedOnlyWhileContainersRun(t *testing.T) {
	cgroups, logs := t.TempDir(), t.TempDir()
	oldRoot, oldEvery := cgroupRoot, logCapEvery
	cgroupRoot, logCapEvery = cgroups, 5*time.Millisecond
	t.Cleanup(func() { cgroupRoot, logCapEvery = oldRoot, oldEvery })
	e := &Engine{paths: Paths{Logs: logs}}
	running := func() bool {
		e.logs.mu.Lock()
		defer e.logs.mu.Unlock()
		return e.logs.running
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("gave up waiting for %s", what)
			}
			time.Sleep(time.Millisecond)
		}
	}

	// Nothing is measured before the core says so.
	os.Mkdir(filepath.Join(cgroups, "zelie-web.scope"), 0o755)
	e.watchLogs()
	if running() {
		t.Fatal("the loop started before StartLogCap")
	}

	// A sparse file: only its size matters.
	log := filepath.Join(logs, "web.log")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxLogBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.StartLogCap(ctx)
	waitFor("the log to be cut", func() bool { st, _ := os.Stat(log); return st != nil && st.Size() < 1024 })

	// With no container left the loop ends by itself, and a container that
	// starts later starts it again.
	os.Remove(filepath.Join(cgroups, "zelie-web.scope"))
	waitFor("the loop to end", func() bool { return !running() })
	os.Mkdir(filepath.Join(cgroups, "zelie-web.scope"), 0o755)
	e.watchLogs()
	if !running() {
		t.Fatal("the loop did not start for a new container")
	}

	cancel()
	waitFor("the loop to end with the core", func() bool { return !running() })
}

func TestFirewallRulesAreCheckedWhileLogsAreWatched(t *testing.T) {
	cgroups := t.TempDir()
	oldRoot, oldEvery, oldRules := cgroupRoot, logCapEvery, logRulesEvery
	cgroupRoot, logCapEvery, logRulesEvery = cgroups, time.Hour, 5*time.Millisecond
	t.Cleanup(func() { cgroupRoot, logCapEvery, logRulesEvery = oldRoot, oldEvery, oldRules })
	os.Mkdir(filepath.Join(cgroups, "zelie-web.scope"), 0o755)

	var checks atomic.Int32
	e := &Engine{paths: Paths{Logs: t.TempDir()}}
	e.logs.rules = func(context.Context) { checks.Add(1) }
	ctx, cancel := context.WithCancel(context.Background())
	e.StartLogCap(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for checks.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the firewall rules were not checked while a container ran")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	for {
		e.logs.mu.Lock()
		running := e.logs.running
		e.logs.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the loop did not end with the core")
		}
		time.Sleep(time.Millisecond)
	}
	n := checks.Load()
	time.Sleep(30 * time.Millisecond)
	if checks.Load() != n {
		t.Error("the rules were checked after the loop ended")
	}
}

func TestHostRulesAreLeftAloneWhenCheckedRecently(t *testing.T) {
	// The engine has no containerd here: a check that went on to read the
	// containers would crash, which is what shows it did not.
	e := &Engine{}
	e.peers.inputAt = time.Now()
	e.keepHostRules(context.Background())
}
