package panel

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

// startGame starts a server and waits until it runs.
func (e *appEnv) startGame(t *testing.T, name string) string {
	t.Helper()
	if code, out := e.power(t, name, "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, name)
	id := e.liveContainer(t, name)
	if strings.Contains(e.core.games[id].Image, "java") {
		e.core.emit(id, ")! For help, type \n")
	}
	e.waitState(t, name, "running")
	return id
}

func (e *appEnv) stdin(id string) []string {
	e.core.mu.Lock()
	defer e.core.mu.Unlock()
	return slices.Clone(e.core.consoles[id])
}

// answerSaves makes the fake server print what Minecraft does for a flush.
func (e *appEnv) answerSaves() {
	e.core.stdinHook = func(id, line string) {
		if line == "save-all flush\n" {
			e.core.emit(id, "[12:00:00] [Server thread/INFO]: Saved the game\n")
		}
	}
}

func deployCount(t *testing.T, e *appEnv, app string) int {
	t.Helper()
	list, err := e.s.Store.Deployments(context.Background(), app, 100)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func (e *appEnv) gameBackups(t *testing.T, name string) []map[string]any {
	t.Helper()
	code, out := e.b.do("GET", "/api/games/"+name+"/backups", nil)
	if code != http.StatusOK {
		t.Fatalf("list backups: %d %v", code, out)
	}
	var list []map[string]any
	for _, b := range out["backups"].([]any) {
		list = append(list, b.(map[string]any))
	}
	return list
}

func TestMinecraftBackupSavesAroundTheCopy(t *testing.T) {
	e := newPowerEnv(t)
	saveFlushWait = 5 * time.Second
	t.Cleanup(func() { saveFlushWait = 30 * time.Second })
	e.newGame(t, "survival", consoleEggURL, nil)
	e.answerSaves()
	id := e.startGame(t, "survival")

	if code, out := e.b.do("POST", "/api/games/survival/backups", nil); code != http.StatusAccepted {
		t.Fatalf("back up: %d %v", code, out)
	}
	e.s.jobs.Wait()

	if got := e.stdin(id); !slices.Equal(got, []string{"save-off\n", "save-all flush\n", "save-on\n"}) {
		t.Errorf("console got %q", got)
	}
	list := e.gameBackups(t, "survival")
	if len(list) != 1 || list[0]["state"] != "done" || list[0]["reason"] != "manual" {
		t.Fatalf("backups %v", list)
	}
	// It was copied while the server ran, which the save commands allow.
	if !slices.Equal(e.core.bk.live, []bool{true}) || e.core.bk.runningFor[0] != 1 {
		t.Errorf("live %v, running %v", e.core.bk.live, e.core.bk.runningFor)
	}
	_, out := e.b.do("GET", "/api/games/survival/backups", nil)
	if out["flush"] != true {
		t.Errorf("the list does not say the server is told to save: %v", out)
	}
}

func TestMinecraftBackupWritesAgainWhenTheServerNeverSaysSaved(t *testing.T) {
	e := newPowerEnv(t)
	saveFlushWait = 50 * time.Millisecond
	t.Cleanup(func() { saveFlushWait = 30 * time.Second })
	e.newGame(t, "survival", consoleEggURL, nil)
	id := e.startGame(t, "survival")

	e.b.do("POST", "/api/games/survival/backups", nil)
	e.s.jobs.Wait()
	if got := e.stdin(id); !slices.Equal(got, []string{"save-off\n", "save-all flush\n", "save-on\n"}) {
		t.Errorf("console got %q", got)
	}
	// A backup that fails still turns saving back on.
	e.core.bk.failBackup, e.core.bk.failures = "the disk is full", 1
	e.b.do("POST", "/api/games/survival/backups", nil)
	e.s.jobs.Wait()
	if got := e.stdin(id); len(got) != 6 || got[5] != "save-on\n" {
		t.Errorf("console got %q", got)
	}
}

func TestOtherGamesAreBackedUpAsTheyRun(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "rusty", "https://example.com/e.js", map[string]any{"ports": 1})
	id := e.startGame(t, "rusty")
	e.b.do("POST", "/api/games/rusty/backups", nil)
	e.s.jobs.Wait()
	if got := e.stdin(id); len(got) != 0 {
		t.Errorf("a game that is not Minecraft got %q", got)
	}
	if list := e.gameBackups(t, "rusty"); len(list) != 1 || list[0]["state"] != "done" {
		t.Errorf("backups %v", list)
	}
	if _, out := e.b.do("GET", "/api/games/rusty/backups", nil); out["flush"] != nil {
		t.Errorf("flush is set: %v", out["flush"])
	}

	// A stopped Minecraft server gets no commands either.
	e.newGame(t, "stopped", consoleEggURL, nil)
	e.b.do("POST", "/api/games/stopped/backups", nil)
	e.s.jobs.Wait()
	for id, lines := range e.core.consoles {
		if strings.HasPrefix(id, "stopped-") && len(lines) != 0 {
			t.Errorf("console %v", e.core.consoles)
		}
	}
}

func TestRestoreGameBackupStopsAndStartsTheServer(t *testing.T) {
	e := newPowerEnv(t)
	saveFlushWait = 5 * time.Second
	t.Cleanup(func() { saveFlushWait = 30 * time.Second })
	e.newGame(t, "survival", consoleEggURL, nil)
	e.answerSaves()
	first := e.startGame(t, "survival")
	e.b.do("POST", "/api/games/survival/backups", nil)
	e.s.jobs.Wait()
	b := e.gameBackups(t, "survival")[0]

	code, out := e.b.do("POST", fmt.Sprintf("/api/games/survival/backups/%d/restore", int64(b["id"].(float64))), nil)
	if code != http.StatusAccepted {
		t.Fatalf("restore: %d %v", code, out)
	}
	e.s.jobs.Wait()
	e.s.deploys.wg.Wait()

	_, out = e.b.do("GET", "/api/games/survival/backups", nil)
	restore := out["restore"].(map[string]any)
	if restore["state"] != "done" {
		t.Fatalf("restore %v", restore)
	}
	// The egg's own stop command ran, the files went back, and the server
	// runs again in a new container.
	if !slices.Contains(e.stdin(first), "stop\n") {
		t.Errorf("the server was not asked to stop: %q", e.stdin(first))
	}
	if len(e.core.bk.restored) != 1 {
		t.Errorf("restored %v", e.core.bk.restored)
	}
	if now := e.running("survival"); len(now) != 1 || now[0] == first {
		t.Errorf("running %v after %s", now, first)
	}
	reasons := []string{}
	for _, x := range e.gameBackups(t, "survival") {
		reasons = append(reasons, x["reason"].(string))
	}
	slices.Sort(reasons)
	if !slices.Equal(reasons, []string{store.BackupManual, store.BackupRestore}) {
		t.Errorf("reasons %v", reasons)
	}

	// A server that was stopped stays stopped.
	e.power(t, "survival", "stop")
	e.waitState(t, "survival", "stopped")
	before := deployCount(t, e, "survival")
	if _, out := e.b.do("POST", fmt.Sprintf("/api/games/survival/backups/%d/restore", int64(b["id"].(float64))), nil); out != nil && out["code"] != nil {
		t.Fatalf("restore: %v", out)
	}
	e.s.jobs.Wait()
	e.s.deploys.wg.Wait()
	if e.gameState(t, "survival") != "stopped" || deployCount(t, e, "survival") != before {
		t.Errorf("the stopped server was started: %s", e.gameState(t, "survival"))
	}
}

func TestGameBackupRoutesBelongToTheServer(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "one", "https://example.com/e.js", map[string]any{"ports": 1})
	e.newGame(t, "two", "https://example.com/e.js", map[string]any{"ports": 1})
	e.b.do("POST", "/api/games/one/backups", nil)
	e.s.jobs.Wait()
	b := int64(e.gameBackups(t, "one")[0]["id"].(float64))

	for _, tc := range []struct{ method, path string }{
		{"GET", fmt.Sprintf("/api/games/two/backups/%d/download", b)},
		{"POST", fmt.Sprintf("/api/games/two/backups/%d/restore", b)},
		{"DELETE", fmt.Sprintf("/api/games/two/backups/%d", b)},
	} {
		if code, out := e.b.do(tc.method, tc.path, nil); code != http.StatusNotFound || out["code"] != "backup.not_found" {
			t.Errorf("%s %s: %d %v", tc.method, tc.path, code, out)
		}
	}
	// A plain app is not a game server.
	e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"})
	if code, out := e.b.do("GET", "/api/games/web/backups", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("app: %d %v", code, out)
	}

	rec := e.b.record("GET", fmt.Sprintf("/api/games/one/backups/%d/download", b), nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "age-encryption.org") {
		t.Errorf("download: %d %s", rec.Code, rec.Body)
	}
	if code, _ := e.b.do("DELETE", fmt.Sprintf("/api/games/one/backups/%d", b), nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	if list := e.gameBackups(t, "one"); len(list) != 0 {
		t.Errorf("backups %v", list)
	}
}
