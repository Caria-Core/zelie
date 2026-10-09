package panel

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/store"
)

// lines splits a log the way the hub keeps it, newest last.
func lines(log string) []string { return strings.Split(strings.TrimSpace(log), "\n") }

func TestDiagnosisRules(t *testing.T) {
	javaEgg := &egg.Egg{Images: []egg.Image{
		{Label: "Java 17", Ref: "ghcr.io/example/yolks:java_17"},
		{Label: "Java 21", Ref: "ghcr.io/example/yolks:java_21"},
	}}
	tests := []struct {
		name string
		log  string
		exit int
		disk bool // the volume is over its limit
		want string
		java int
	}{
		{"java heap", "[12:00:01] [Server thread/ERROR]: Encountered an unexpected exception\njava.lang.OutOfMemoryError: Java heap space", -1, false, "oom", 0},
		{"java gc overhead", "Exception in thread \"Server thread\" java.lang.OutOfMemoryError: GC overhead limit exceeded", 1, false, "oom", 0},
		{"node heap limit", "<--- Last few GCs --->\nFATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory", 134, false, "oom", 0},
		{"node heap text only", "FATAL ERROR: Ineffective mark-compacts near heap limit Allocation failed - JavaScript heap out of memory", 134, false, "oom", 0},
		{"killed with 137", "./start.sh: line 3:    42 Killed                  java -Xmx1G -jar server.jar", 137, false, "oom", 0},
		{"killed without an exit code", "bash: line 1:    42 Killed                  node index.js", -1, false, "oom", 0},
		{"silent 137", "[12:00:01] [Server thread/INFO]: Preparing spawn area: 87%", 137, false, "oom_killed", 0},
		{"java too old", "Exception in thread \"main\" java.lang.UnsupportedClassVersionError: net/minecraft/bundler/Main has been compiled by a more recent version of the Java Runtime (class file version 65.0), this version of the Java Runtime only recognizes class file versions up to 61.0", 1, false, "java_version", 21},
		{"java 8 needed", "java.lang.UnsupportedClassVersionError: Foo : Unsupported major.minor version 52.0 (class file version 52.0)", 1, false, "java_version", 8},
		{"eula", "[12:00:03] [ServerMain/WARN]: Failed to load eula.txt\n[12:00:03] [ServerMain/INFO]: You need to agree to the EULA in order to run the server. Go to eula.txt for more info.", 0, false, "eula", 0},
		{"port in use", "java.net.BindException: Address already in use\n[Server thread/WARN]: **** FAILED TO BIND TO PORT!", 1, false, "port_in_use", 0},
		{"node port", "Error: listen EADDRINUSE: address already in use :::3000", 1, false, "port_in_use", 0},
		{"missing jar", "Error: Unable to access jarfile server.jar", 1, false, "missing_file", 0},
		{"missing main class", "Error: Could not find or load main class net.minecraft.server.Main\nCaused by: java.lang.ClassNotFoundException: net.minecraft.server.Main", 1, false, "missing_file", 0},
		{"node missing main file", "node:internal/modules/cjs/loader:1228\n  throw err;\n  ^\n\nError: Cannot find module '/home/container/index.js'\n    at Module._resolveFilename", 1, false, "missing_file", 0},
		{"node missing package", "Error: Cannot find module 'express'\nRequire stack:\n- /home/container/index.js", 1, false, "missing_module", 0},
		{"node missing scoped package", "Error: Cannot find module '@discordjs/voice'\nRequire stack:\n- /home/container/index.js", 1, false, "missing_module", 0},
		{"disk full", "java.io.IOException: No space left on device\n\tat java.base/sun.nio.ch.FileDispatcherImpl.write0(Native Method)", 1, false, "disk_full", 0},
		{"volume over its limit", "[Server thread/INFO]: Saving chunks", 1, true, "disk_full", 0},
		{"level.dat", "[Server thread/ERROR]: Exception reading ./world/level.dat\njava.util.zip.ZipException: Not in GZIP format", 1, false, "world_corrupt", 0},
		{"failed to load level", "[Server thread/ERROR]: Failed to load level world", 1, false, "world_corrupt", 0},
		{"nothing known", "[12:00:01] [Server thread/INFO]: Done (3.2s)!\n[12:00:09] [Server thread/INFO]: Stopping the server", 1, false, "", 0},
		{"a clean exit", "Goodbye", 0, false, "", 0},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			in := &diagInput{lines: lines(c.log), exit: c.exit, egg: javaEgg, diskMB: 1024, volumeFull: c.disk}
			slicesReverse(in.lines)
			r, found, ok := firstRule(in)
			if c.want == "" {
				if ok {
					t.Fatalf("matched %s", r.id)
				}
				return
			}
			if !ok || r.id != c.want {
				t.Fatalf("matched %q (%v), want %q", r.id, ok, c.want)
			}
			if c.java != 0 && found["java"] != c.java {
				t.Errorf("java %v, want %d", found["java"], c.java)
			}
		})
	}
}

func slicesReverse(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

func TestDiagnosisOnlyReadsRecentLines(t *testing.T) {
	// An old out-of-memory error, followed by a lot of normal output, is
	// not what the server died of, and only the newest lines are kept.
	old := []string{"java.lang.OutOfMemoryError: Java heap space"}
	for range 300 {
		old = append(old, "[Server thread/INFO]: tick")
	}
	kept := lastLines(old, diagnoseLines)
	in := &diagInput{lines: kept, exit: 1, egg: &egg.Egg{}}
	slicesReverse(in.lines)
	if _, _, ok := firstRule(in); ok {
		t.Error("an error past the newest lines still matched")
	}
}

func TestDiagnosisPriority(t *testing.T) {
	// The EULA line comes with other noise; it is the one to report.
	in := &diagInput{lines: lines("java.lang.OutOfMemoryError: Java heap space\nYou need to agree to the EULA in order to run the server."), exit: 1, egg: &egg.Egg{}}
	slicesReverse(in.lines)
	if r, _, ok := firstRule(in); !ok || r.id != "eula" {
		t.Errorf("matched %q", r.id)
	}
}

func TestDiagnosisFailuresToStartComeFromBeforeTheGameWasReady(t *testing.T) {
	tests := []struct {
		name string
		log  string
		late int // how many of the newest lines came after the game was ready
		want string
	}{
		{"before", "Unable to access jarfile server.jar\nStopping", 0, "missing_file"},
		{"after", "Done\nAlex was slain by Steve using [Unable to access jarfile]\nStopping", 2, ""},
		{"port after", "Done\nNamed entity Wolf['Address already in use'/12] died\nStopping", 2, ""},
		{"eula after", "Done\nSteve whispers: you need to agree to the EULA", 1, ""},
		{"java after", "Done\nclass file version 70", 1, ""},
		{"module after", "Done\nCannot find module 'express'", 1, ""},
		{"before and after", "Address already in use\nDone\nUnable to access jarfile", 2, "port_in_use"},
		{"other causes still count", "Done\njava.lang.OutOfMemoryError: Java heap space", 1, "oom"},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			in := &diagInput{lines: lines(c.log), late: c.late, exit: 1, egg: &egg.Egg{}}
			slicesReverse(in.lines)
			r, _, ok := firstRule(in)
			if c.want == "" {
				if ok {
					t.Fatalf("matched %s", r.id)
				}
				return
			}
			if !ok || r.id != c.want {
				t.Fatalf("matched %q (%v), want %q", r.id, ok, c.want)
			}
		})
	}
}

func TestDiagnosisPrefersTheNewestLine(t *testing.T) {
	tests := []struct {
		name string
		log  string
		disk bool
		want string
	}{
		{"memory last", "No space left on device\njava.lang.OutOfMemoryError: Java heap space", false, "oom"},
		{"disk last", "java.lang.OutOfMemoryError: Java heap space\nNo space left on device", false, "disk_full"},
		{"world last", "java.lang.OutOfMemoryError: Java heap space\nFailed to load level world", false, "world_corrupt"},
		{"same line, listed first", "No space left on device: java.lang.OutOfMemoryError", false, "disk_full"},
		{"a full volume holds", "java.lang.OutOfMemoryError: Java heap space\nSaving chunks", true, "disk_full"},
		{"a line beats a silent kill", "Failed to load level world", false, "world_corrupt"},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			in := &diagInput{lines: lines(c.log), exit: 137, egg: &egg.Egg{}, volumeFull: c.disk}
			slicesReverse(in.lines)
			if r, _, ok := firstRule(in); !ok || r.id != c.want {
				t.Fatalf("matched %q (%v), want %q", r.id, ok, c.want)
			}
		})
	}
}

func TestDiagnosisDropsTheLinesOfPlayerNames(t *testing.T) {
	// A Steam name can be any text, and the server prints it in the lines of
	// a join, a leave and a kill.
	got := withoutPlayerLines("rust", []string{
		"\x1b[32m1.2.3.4:5/76561198000000002/Address already in use joined [windows/76561198000000002]\x1b[0m",
		"Unable to access jarfile[76561198000000002] disconnecting: Kicked",
		"Server startup complete",
	})
	if len(got) != 1 || got[0] != "Server startup complete" {
		t.Errorf("kept %q", got)
	}
	got = withoutPlayerLines("minecraft-paper", []string{
		"[12:00:00 INFO]: * Steve Unable to access jarfile",
		"[12:00:01 INFO]: Done (3.2s)! For help, type \"help\"",
	})
	if len(got) != 1 || !strings.Contains(got[0], "Done") {
		t.Errorf("kept %q", got)
	}
}

// printed has the server print log and waits for the console to have read it.
func (e *appEnv) printed(t *testing.T, name, log string) {
	t.Helper()
	e.core.emit(e.liveContainer(t, name), log)
	h := e.s.consoles.hub(name)
	last := lines(log)[len(lines(log))-1]
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.lines) > 0 && h.lines[len(h.lines)-1] == last
	})
}

// crashWith runs the server, has it print log, and ends its process with exit.
func (e *appEnv) crashWith(t *testing.T, name, log string, exit int) {
	t.Helper()
	if code, out := e.power(t, name, "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, name)
	id := e.liveContainer(t, name)
	e.printed(t, name, log)
	h := e.s.consoles.hub(name)
	e.core.mu.Lock()
	if e.core.gameExit == nil {
		e.core.gameExit = map[string]int{}
	}
	e.core.gameExit[id] = exit
	e.core.mu.Unlock()
	e.crash(t, name)
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.exited
	})
}

func TestDiagnosisOutOfMemoryAndItsFix(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil) // 2048 MB, and the machine has 3 GB

	// Not crashed: nothing to say, even with the words in the log.
	if code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil); code != http.StatusOK || len(out) != 0 {
		t.Fatalf("stopped: %d %v", code, out)
	}
	e.crashWith(t, "survival", "[Server thread/ERROR]: java.lang.OutOfMemoryError: Java heap space\n", 1)
	code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil)
	cause, _ := out["cause"].(map[string]any)
	fix, _ := out["fix"].(map[string]any)
	if code != http.StatusOK || cause["code"] != "diagnosis.oom" || fix["kind"] != fixRaiseMemory {
		t.Fatalf("diagnosis: %d %v", code, out)
	}
	// 2048 * 1.5 is 3072, which is all the machine has.
	if p := fix["params"].(map[string]any); p["memory_mb"] != 3072.0 || p["size"] != "3 GB" {
		t.Errorf("fix params %v", p)
	}

	// The client's own numbers are not used.
	body := map[string]any{"kind": fixRaiseMemory, "params": map[string]any{"memory_mb": 1 << 20}}
	code, out = e.b.do("POST", "/api/games/survival/diagnosis/fix", body)
	if code != http.StatusOK || out["memory_mb"] != 3072.0 || out["state"] != "crashed" {
		t.Fatalf("fix: %d %v", code, out)
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); a.MemoryMB != 3072 {
		t.Errorf("stored memory %d", a.MemoryMB)
	}
	// It did not start the server, and with the machine's memory used up
	// there is no more to offer.
	if n := len(e.running("survival")); n != 0 {
		t.Errorf("%d containers running", n)
	}
	_, out = e.b.do("GET", "/api/games/survival/diagnosis", nil)
	if out["fix"] != nil || out["cause"] == nil {
		t.Errorf("after the fix: %v", out)
	}
}

func TestDiagnosisFixRefusesWhatIsNotOffered(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	fixURL := "/api/games/survival/diagnosis/fix"

	// Stopped and never crashed.
	if code, out := e.b.do("POST", fixURL, map[string]any{"kind": fixReinstall}); code != http.StatusConflict || out["code"] != "diagnosis.fix_gone" {
		t.Errorf("stopped: %d %v", code, out)
	}

	e.crashWith(t, "survival", "Error: Unable to access jarfile server.jar\n", 1)
	for _, kind := range []string{fixRaiseMemory, fixRaiseDisk, fixAcceptEULA, fixSwitchImage, fixOpenNetwork, "format_disk", ""} {
		if code, out := e.b.do("POST", fixURL, map[string]any{"kind": kind}); code != http.StatusConflict || out["code"] != "diagnosis.fix_gone" {
			t.Errorf("%q: %d %v", kind, code, out)
		}
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); a.MemoryMB != 2048 {
		t.Errorf("memory changed to %d", a.MemoryMB)
	}
	// The link kinds change nothing, even when they are offered.
	e.crashWith(t, "survival", "java.net.BindException: Address already in use\n", 1)
	if code, out := e.b.do("POST", fixURL, map[string]any{"kind": fixOpenNetwork}); code != http.StatusConflict || out["code"] != "diagnosis.fix_gone" {
		t.Errorf("link: %d %v", code, out)
	}

	// The offered one works: the install runs again.
	e.crashWith(t, "survival", "Error: Unable to access jarfile server.jar\n", 1)
	if code, out := e.b.do("POST", fixURL, map[string]any{"kind": fixReinstall}); code != http.StatusOK || out["install"].(map[string]any)["state"] != "installing" {
		t.Errorf("reinstall: %d %v", code, out)
	}
	e.settle(t, "survival")

	if code, _ := e.b.do("POST", "/api/games/none/diagnosis/fix", map[string]any{"kind": fixReinstall}); code != http.StatusNotFound {
		t.Errorf("no server: %d", code)
	}
	if code, _ := e.b.do("GET", "/api/games/none/diagnosis", nil); code != http.StatusNotFound {
		t.Errorf("no server: %d", code)
	}
}

func TestDiagnosisIsEmptyForARunningServer(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "java.lang.OutOfMemoryError: Java heap space\nDone (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")
	if code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil); code != http.StatusOK || len(out) != 0 {
		t.Errorf("running: %d %v", code, out)
	}
	if code, out := e.b.do("POST", "/api/games/survival/diagnosis/fix", map[string]any{"kind": fixRaiseMemory}); code != http.StatusConflict {
		t.Errorf("fix while running: %d %v", code, out)
	}
}

func TestDiagnosisAfterZelieGaveUp(t *testing.T) {
	e := newPowerEnv(t)
	now := time.Unix(1_800_000_000, 0)
	e.s.Now = func() time.Time { return now }
	e.newGame(t, "survival", consoleEggURL, nil)
	e.crashWith(t, "survival", "Error: Unable to access jarfile server.jar\n", 1)
	for range 5 {
		now = now.Add(time.Minute)
		e.s.superviseOnce(context.Background())
		e.s.deploys.wg.Wait()
		// The server prints the same again each time it comes back.
		e.printed(t, "survival", "Error: Unable to access jarfile server.jar\n")
		e.crash(t, "survival")
	}
	if e.s.crashes.gaveUp("survival") == nil {
		t.Fatal("Zelie did not give up")
	}
	if _, out := e.b.do("GET", "/api/games/survival/diagnosis", nil); out["cause"] == nil {
		t.Errorf("gave up: %v", out)
	}
}

func TestDiagnosisWrongJavaAndEULA(t *testing.T) {
	javaEgg := strings.Replace(consoleEgg, `"docker_images": {"Java 21": "ghcr.io/example/java:21"}`, `"docker_images": {"Java 17": "ghcr.io/example/java:17", "Java 21": "ghcr.io/example/java:21"}`, 1)
	e := newPowerEnv(t)
	const url = "https://example.com/java.json"
	e.s.Eggs.(*fakeEggs).files[url] = javaEgg
	e.newGame(t, "survival", url, map[string]any{"image": "Java 17"})
	e.crashWith(t, "survival", "Exception in thread \"main\" java.lang.UnsupportedClassVersionError: Main (class file version 65.0), this version of the Java Runtime only recognizes class file versions up to 61.0\n", 1)
	code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil)
	cause, _ := out["cause"].(map[string]any)
	fix, _ := out["fix"].(map[string]any)
	if code != http.StatusOK || cause["code"] != "diagnosis.java_version" || cause["params"].(map[string]any)["java"] != 21.0 || fix["kind"] != fixSwitchImage {
		t.Fatalf("diagnosis: %d %v", code, out)
	}
	code, out = e.b.do("POST", "/api/games/survival/diagnosis/fix", map[string]any{"kind": fixSwitchImage})
	if code != http.StatusOK || out["image"] != "ghcr.io/example/java:21" {
		t.Fatalf("fix: %d %v", code, out)
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); a.Image != "ghcr.io/example/java:21" {
		t.Errorf("app image %q", a.Image)
	}

	// Without an image for that Java there is only the explanation.
	e2 := newPowerEnv(t)
	e2.newGame(t, "plain", consoleEggURL, nil)
	e2.crashWith(t, "plain", "java.lang.UnsupportedClassVersionError: Main (class file version 69.0)\n", 1)
	_, out = e2.b.do("GET", "/api/games/plain/diagnosis", nil)
	if out["cause"] == nil || out["fix"] != nil {
		t.Errorf("no image: %v", out)
	}

	// EULA: the fix records the acceptance.
	e3 := newPowerEnv(t)
	e3.unacceptedGame(t, "mc")
	e3.crashWith(t, "mc", "[ServerMain/INFO]: You need to agree to the EULA in order to run the server. Go to eula.txt for more info.\n", 0)
	_, out = e3.b.do("GET", "/api/games/mc/diagnosis", nil)
	if fix, _ := out["fix"].(map[string]any); fix["kind"] != fixAcceptEULA {
		t.Fatalf("eula: %v", out)
	}
	if code, out := e3.b.do("POST", "/api/games/mc/diagnosis/fix", map[string]any{"kind": fixAcceptEULA}); code != http.StatusOK || out["eula_needed"] != nil {
		t.Fatalf("accept: %d %v", code, out)
	}
	if g, _ := e3.s.Store.GameServer(context.Background(), "mc"); g.EULAAcceptedAt.IsZero() {
		t.Error("the EULA was not recorded")
	}
	// It is accepted now, so a second look offers nothing to accept.
	if _, out := e3.b.do("GET", "/api/games/mc/diagnosis", nil); out["fix"] != nil {
		t.Errorf("after accepting: %v", out)
	}
}

func TestDiagnosisDiskAndFilesApps(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "site", nil)
	e.crashWith(t, "site", "Error: Cannot find module 'express'\nRequire stack:\n- /home/container/index.js\n", 1)
	_, out := e.b.do("GET", "/api/games/site/diagnosis", nil)
	if c, _ := out["cause"].(map[string]any); c["code"] != "diagnosis.missing_module" || c["params"].(map[string]any)["module"] != "express" || out["fix"] != nil {
		t.Errorf("module: %v", out)
	}
	// A files app gets no reinstall for a missing file, nor a network page.
	e.crashWith(t, "site", "Error: Cannot find module '/home/container/index.js'\n", 1)
	if _, out := e.b.do("GET", "/api/games/site/diagnosis", nil); out["cause"] == nil || out["fix"] != nil {
		t.Errorf("missing file: %v", out)
	}
	e.crashWith(t, "site", "Error: listen EADDRINUSE: address already in use :::8080\n", 1)
	if _, out := e.b.do("GET", "/api/games/site/diagnosis", nil); out["cause"] == nil || out["fix"] != nil {
		t.Errorf("port: %v", out)
	}
	// Memory can be raised for it like for a game server.
	e.crashWith(t, "site", "FATAL ERROR: Reached heap limit Allocation failed - JavaScript heap out of memory\n", 134)
	_, out = e.b.do("GET", "/api/games/site/diagnosis", nil)
	if f, _ := out["fix"].(map[string]any); f["kind"] != fixRaiseMemory {
		t.Fatalf("files memory: %v", out)
	}
	before, _ := e.s.Store.App(context.Background(), "site")
	if code, out := e.b.do("POST", "/api/games/site/diagnosis/fix", map[string]any{"kind": fixRaiseMemory}); code != http.StatusOK {
		t.Fatalf("files fix: %d %v", code, out)
	}
	if after, _ := e.s.Store.App(context.Background(), "site"); after.MemoryMB <= before.MemoryMB {
		t.Errorf("memory %d, was %d", after.MemoryMB, before.MemoryMB)
	}
}

// The disk case has an environment of its own: two power environments in one
// test would change the test timings under each other's watchers.
func TestDiagnosisDiskFull(t *testing.T) {
	// The log says it, and the fix goes through the volume's check.
	g := newPowerEnv(t)
	g.newGame(t, "survival", consoleEggURL, nil)
	g.crashWith(t, "survival", "java.io.IOException: No space left on device\n", 1)
	_, out := g.b.do("GET", "/api/games/survival/diagnosis", nil)
	f, _ := out["fix"].(map[string]any)
	if f["kind"] != fixRaiseDisk || f["params"].(map[string]any)["disk_mb"] != float64(15<<10) {
		t.Fatalf("disk: %v", out)
	}
	if code, out := g.b.do("POST", "/api/games/survival/diagnosis/fix", map[string]any{"kind": fixRaiseDisk}); code != http.StatusOK || out["disk_mb"] != float64(15<<10) {
		t.Fatalf("disk fix: %d %v", code, out)
	}
}

func TestDiagnosisOfAFailedInstall(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	ctx := context.Background()
	g, _ := e.s.Store.GameServer(ctx, "survival")
	f, closeLog, err := e.s.openDeployLog(g.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(f, "Downloading server.jar\ncurl: (23) Failure writing output to destination, No space left on device\nInstall failed: exit status 23\n")
	closeLog()
	if err := e.s.Store.SetInstall(ctx, "survival", store.InstallFailed, g.InstallID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, out := e.b.do("GET", "/api/games/survival/diagnosis", nil)
	fix, _ := out["fix"].(map[string]any)
	if c, _ := out["cause"].(map[string]any); c["code"] != "diagnosis.disk_full" || fix["kind"] != fixRaiseDisk {
		t.Fatalf("install: %v", out)
	}

	// An install that failed on something else says nothing, and the
	// server's own rules do not apply to it.
	if err := os.WriteFile(e.s.deployLogPath(g.InstallID), []byte("java.lang.OutOfMemoryError: Java heap space\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, out := e.b.do("GET", "/api/games/survival/diagnosis", nil); len(out) != 0 {
		t.Errorf("other install failure: %v", out)
	}
}

func TestDiagnosisIgnoresWhatPlayersMake(t *testing.T) {
	e, id := newPlayerGame(t)
	// Once the server is up, players make it print the words of other causes
	// in ways no chat filter can list. The memory error is what it died of.
	e.printed(t, "survival", "[12:00:00 INFO]: Done (3.2s)! For help, type \"help\"\n"+
		"[12:00:01 INFO]: Steve joined the game\n"+
		"[12:00:02 INFO]: <Steve> Unable to access jarfile\n"+
		"[12:00:03 INFO]: * Steve Unable to access jarfile\n"+
		"[12:00:04 INFO]: [Not Secure] * Steve Address already in use\n"+
		"[12:00:05 INFO]: Steve issued server command: /Address already in use\n"+
		"[12:00:06 INFO]: Named entity Wolf['Unable to access jarfile'/12, l='ServerLevel[world]'] died: Wolf was slain\n"+
		"[12:00:07 INFO]: Alex was slain by Steve using [Address already in use]\n"+
		"[12:00:08 INFO]: You need to agree to the EULA in order to run the server\n"+
		"[12:00:09 ERROR]: java.lang.OutOfMemoryError: Java heap space\n")
	e.core.mu.Lock()
	e.core.gameExit = map[string]int{id: 1}
	e.core.mu.Unlock()
	e.crash(t, "survival")
	h := e.s.consoles.hub("survival")
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.exited
	})

	code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil)
	cause, _ := out["cause"].(map[string]any)
	if code != http.StatusOK || cause["code"] != "diagnosis.oom" {
		t.Fatalf("diagnosis: %d %v", code, out)
	}
}

func TestDiagnosisIgnoresAFailureToStartPrintedByAPlayer(t *testing.T) {
	e, id := newPlayerGame(t)
	// A name on an item is in the line, which no chat filter knows. The
	// server was up by then, so it is not a failure to start.
	e.printed(t, "survival", "[12:00:00 INFO]: Done (3.2s)! For help, type \"help\"\n"+
		"[12:00:01 INFO]: Alex was slain by Steve using [Unable to access jarfile]\n")
	e.core.mu.Lock()
	e.core.gameExit = map[string]int{id: 1}
	e.core.mu.Unlock()
	e.crash(t, "survival")
	h := e.s.consoles.hub("survival")
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.exited
	})

	code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil)
	if code != http.StatusOK || len(out) != 0 {
		t.Fatalf("diagnosis: %d %v", code, out)
	}
}

func TestDiagnosisStillReadsAFailedStart(t *testing.T) {
	e, id := newPlayerGame(t)
	e.printed(t, "survival", "[12:00:00 ERROR]: Unable to access jarfile server.jar\n")
	e.core.mu.Lock()
	e.core.gameExit = map[string]int{id: 1}
	e.core.mu.Unlock()
	e.crash(t, "survival")
	h := e.s.consoles.hub("survival")
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.exited
	})

	code, out := e.b.do("GET", "/api/games/survival/diagnosis", nil)
	cause, _ := out["cause"].(map[string]any)
	if code != http.StatusOK || cause["code"] != "diagnosis.missing_file" {
		t.Fatalf("diagnosis: %d %v", code, out)
	}
}

func TestDiagnosisOfAStoppedConsoleKnowsWhenTheGameWasReady(t *testing.T) {
	e, id := newPlayerGame(t)
	e.core.emit(id, "[12:00:00 INFO]: Done (3.2s)! For help, type \"help\"\n"+
		"[12:00:01 INFO]: * Steve Unable to access jarfile\n"+
		"[12:00:02 INFO]: Alex was slain by Steve using [Address already in use]\n")
	e.waitState(t, "survival", "running")
	if code, _ := e.power(t, "survival", "kill"); code >= 300 {
		t.Fatalf("kill: %d", code)
	}
	e.waitState(t, "survival", "stopped")

	// A console read back from the log, as after a panel restart.
	h := &consoleHub{app: "survival"}
	e.s.loadStoppedConsole(context.Background(), h)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.ready || h.readyAt != 0 || len(h.lines) != 3 {
		t.Fatalf("ready %v at %d, %d lines", h.ready, h.readyAt, len(h.lines))
	}
	if n := h.beforeReady(3); n != 0 {
		t.Errorf("%d lines before the game was ready", n)
	}
}

func TestConsoleKnowsWhereTheGameBecameReady(t *testing.T) {
	h := &consoleHub{container: "c1"}
	before := func(n int) int {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.beforeReady(n)
	}
	for range 3 {
		h.push("c1", "line", "tick")
	}
	if n := before(3); n != 3 {
		t.Errorf("before the game was ready: %d of 3", n)
	}
	h.push("c1", "line", "Done")
	h.markReady("c1")
	h.push("c1", "line", "tick")
	h.push("c1", "line", "Done")
	h.markReady("c1") // the first stays
	if n := before(6); n != 3 {
		t.Errorf("of all 6: %d before", n)
	}
	if n := before(2); n != 0 {
		t.Errorf("of the newest 2: %d before", n)
	}
	if n := before(4); n != 1 {
		t.Errorf("of the newest 4: %d before", n)
	}

	// The lines the ring dropped were before it too.
	h.replace("c2", func() {})
	h.push("c2", "line", "Done")
	h.markReady("c2")
	for range 3 * consoleBacklog {
		h.push("c2", "line", "tick")
	}
	h.mu.Lock()
	kept := len(h.lines)
	h.mu.Unlock()
	if n := before(kept); n != 0 || kept == 3*consoleBacklog+1 {
		t.Errorf("after the ready line left the ring: %d of %d lines before it", n, kept)
	}
}

// lineOf is a console line size bytes long with its newline, so a log of them
// can be cut at a place the test knows.
func lineOf(size int, text string) string { return fmt.Sprintf("%*s\n", size-1, text) }

// diagnosisAfterRestart has the panel forget the server's console, as a restart
// or an update does, and asks for the diagnosis from what the log gives back.
func (e *appEnv) diagnosisAfterRestart(t *testing.T, name string) map[string]any {
	t.Helper()
	e.s.consoles.forget(name)
	code, out := e.b.do("GET", "/api/games/"+name+"/diagnosis", nil)
	if code != http.StatusOK {
		t.Fatalf("diagnosis: %d %v", code, out)
	}
	return out
}

func TestDiagnosisOfALongStartupFindsItsFailureAfterARestart(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	// A modpack prints more than the tail before it gets anywhere. Lines of
	// 64 bytes make the tail end exactly at a line, so it comes back full.
	startup := strings.Repeat(lineOf(64, "[12:00:00 INFO]: Loading libraries, please wait..."), consoleTail/64+100)
	e.crashWith(t, "survival", startup+lineOf(64, "[12:00:09 ERROR]: Unable to access jarfile server.jar"), 1)

	out := e.diagnosisAfterRestart(t, "survival")
	if cause, _ := out["cause"].(map[string]any); cause["code"] != "diagnosis.missing_file" {
		t.Fatalf("diagnosis: %v", out)
	}
}

func TestDiagnosisOfAFilesAppWithALongLogAfterARestart(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "site", nil)
	// An app has no line that says it is ready, so all of its output counts.
	log := strings.Repeat(lineOf(64, "npm warn deprecated inflight@1.0.6: leaks memory"), consoleTail/64+100) +
		lineOf(64, "Error: Cannot find module 'express'")
	e.crashWith(t, "site", log, 1)

	out := e.diagnosisAfterRestart(t, "site")
	if cause, _ := out["cause"].(map[string]any); cause["code"] != "diagnosis.missing_module" {
		t.Fatalf("diagnosis: %v", out)
	}
}

func TestDiagnosisKnowsTheGameWasReadyBeforeTheTailOfItsLog(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	// The line that said so is far back, and what a player made is the
	// newest thing in the log.
	log := lineOf(80, "[12:00:00 INFO]: Done (3.2s)! For help, type \"help\"") +
		strings.Repeat(lineOf(50, "[12:10:00 INFO]: Saving chunks"), consoleTail/50+100) +
		"[12:30:00 INFO]: Alex was slain by Steve using [Unable to access jarfile]\n"
	e.crashWith(t, "survival", log, 1)

	if out := e.diagnosisAfterRestart(t, "survival"); len(out) != 0 {
		t.Fatalf("diagnosis: %v", out)
	}
}

func TestDiagnosisIsNotFooledByChatThatSaysTheGameIsReady(t *testing.T) {
	e, id := newPlayerGame(t)
	// The real line is far back, and the tail has a player's chat with the
	// same words after the name on an item.
	log := lineOf(80, "[12:00:00 INFO]: Done (3.2s)! For help, type \"help\"") +
		strings.Repeat(lineOf(50, "[12:10:00 INFO]: Saving chunks"), consoleTail/50+100) +
		"[12:30:00 INFO]: Alex was slain by Steve using [Unable to access jarfile]\n" +
		"[12:30:01 INFO]: <Steve> Done\n"
	e.printed(t, "survival", log)
	e.core.mu.Lock()
	e.core.gameExit = map[string]int{id: 1}
	e.core.mu.Unlock()
	e.crash(t, "survival")
	h := e.s.consoles.hub("survival")
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.exited
	})

	if out := e.diagnosisAfterRestart(t, "survival"); len(out) != 0 {
		t.Fatalf("diagnosis: %v", out)
	}
}

// scanFails is a core that cannot give a console from its start.
type scanFails struct{ Core }

func (c scanFails) Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error {
	if !follow && tail == 0 {
		return &core.Error{Status: http.StatusInternalServerError, Message: "the log could not be read"}
	}
	return c.Core.Logs(ctx, id, follow, tail, w)
}

func TestStoppedConsoleIsShownWhenItsReadyLineCannotBeLookedFor(t *testing.T) {
	e, id := newPlayerGame(t, func(e *appEnv) { e.s.Core = scanFails{e.s.Core} })
	e.core.emit(id, "[12:00:00 ERROR]: Unable to access jarfile server.jar\n")
	e.waitState(t, "survival", "starting")
	if code, _ := e.power(t, "survival", "kill"); code >= 300 {
		t.Fatalf("kill: %d", code)
	}
	e.waitState(t, "survival", "stopped")

	h := &consoleHub{app: "survival"}
	e.s.loadStoppedConsole(context.Background(), h)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.lines) != 1 || h.ready {
		t.Errorf("lines %q, ready %v", h.lines, h.ready)
	}
}
