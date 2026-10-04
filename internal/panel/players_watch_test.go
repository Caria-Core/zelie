package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/players"
	"github.com/Caria-Core/zelie/internal/store"
)

const passwordEgg = `{
	"meta": {"version": "PTDL_v2"},
	"name": "Password Game",
	"docker_images": {"Java 21": "ghcr.io/example/java:21"},
	"startup": "./run +rcon.password {{RCON_PASS}} +name {{NAME}}",
	"config": {"stop": "stop", "startup": "{\"done\": \"Done\"}", "files": "{}"},
	"scripts": {"installation": {"script": "#!/bin/bash\necho installing\n", "container": "ghcr.io/example/installer:latest", "entrypoint": "bash"}},
	"variables": [
		{"name": "Rcon", "env_variable": "RCON_PASS", "default_value": "efefef14abcdef", "user_viewable": true, "user_editable": true, "rules": "required|string"},
		{"name": "Name", "env_variable": "NAME", "default_value": "efefef14", "user_viewable": true, "user_editable": true, "rules": "required|string"}
	]
}`

// A server whose egg has a parser, started and stopped through the panel.
func newPlayerGame(t *testing.T) (*appEnv, string) {
	t.Helper()
	old := playerFlushEvery
	playerFlushEvery = 20 * time.Millisecond
	t.Cleanup(func() { playerFlushEvery = old })
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["minecraft-paper"] = passwordEgg
	code, out := e.b.do("POST", "/api/games", map[string]any{"name": "survival", "egg": "minecraft-paper", "memory_mb": 2048, "ports": 2})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "survival")
	if code, out := e.power(t, "survival", "start"); code >= 300 {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "survival")
	return e, e.liveContainer(t, "survival")
}

func TestConsoleHidesPasswords(t *testing.T) {
	e, id := newPlayerGame(t)
	e.core.emit(id, ":/home/container$ ./run +rcon.password efefef14abcdef +name efefef14\n")
	h := e.s.consoles.hub("survival")
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.lines) > 0
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if got := strings.Join(h.lines, "\n"); got != ":/home/container$ ./run +rcon.password •••••• +name efefef14" {
		t.Errorf("console: %q", got)
	}
}

func TestStoppedConsoleHidesPasswords(t *testing.T) {
	e, id := newPlayerGame(t)
	e.core.emit(id, "rcon.password efefef14abcdef\n")
	e.waitState(t, "survival", "starting")
	if code, _ := e.power(t, "survival", "kill"); code >= 300 {
		t.Fatalf("kill: %d", code)
	}
	e.waitState(t, "survival", "stopped")
	// A console loaded from the log, as after a panel restart.
	h := &consoleHub{app: "survival"}
	e.s.loadStoppedConsole(context.Background(), h)
	if got := strings.Join(h.lines, "\n"); got != "rcon.password ••••••" {
		t.Errorf("console: %q", got)
	}
}

func TestPlayersAreRecordedWhileAServerRuns(t *testing.T) {
	e, id := newPlayerGame(t)
	ctx := context.Background()
	e.core.emit(id, "[12:00:00 INFO]: UUID of player Steve is 069a79f4-44e9-4726-a5be-fca90e38aaf5\n"+
		"[12:00:00 INFO]: Steve[/10.0.0.7:5555] logged in with entity id 4 at (0, 0, 0)\n"+
		"[12:00:00 INFO]: Steve joined the game\n"+
		"[12:00:03 INFO]: <Steve> hi\n")
	const uuid = "069a79f4-44e9-4726-a5be-fca90e38aaf5"
	waitFor(t, func() bool {
		chat, _ := e.s.Store.PlayerChat(ctx, "survival", "", 0, 10)
		return len(chat) == 1
	})
	p, err := e.s.Store.Player(ctx, "survival", uuid)
	if err != nil || !p.Online || p.LastIP != "10.0.0.7" {
		t.Fatalf("player %+v %v", p, err)
	}

	// Nothing says Steve left; the server stopping does.
	if code, _ := e.power(t, "survival", "stop"); code >= 300 {
		t.Fatalf("stop: %d", code)
	}
	e.waitState(t, "survival", "stopped")
	waitFor(t, func() bool {
		p, _ := e.s.Store.Player(ctx, "survival", uuid)
		return !p.Online
	})
	sess, _ := e.s.Store.PlayerSessions(ctx, "survival", uuid, 5)
	if len(sess) != 1 || sess[0].Reason != reasonServerStopped {
		t.Errorf("sessions %+v", sess)
	}

	// The next start begins with nobody in.
	if code, _ := e.power(t, "survival", "start"); code >= 300 {
		t.Fatalf("start: %d", code)
	}
	e.settle(t, "survival")
	e.core.emit(e.liveContainer(t, "survival"), "[12:10:00 INFO]: UUID of player Steve is "+uuid+"\n[12:10:00 INFO]: Steve joined the game\n")
	waitFor(t, func() bool {
		p, _ := e.s.Store.Player(ctx, "survival", uuid)
		return p.Online
	})
}

func TestResumedServerDoesNotRecordItsOldConsoleAgain(t *testing.T) {
	e, id := newPlayerGame(t)
	ctx := context.Background()
	e.core.emit(id, "[12:00:00 INFO]: Steve joined the game\n[12:00:01 INFO]: <Steve> hi\n")
	waitFor(t, func() bool {
		chat, _ := e.s.Store.PlayerChat(ctx, "survival", "", 0, 10)
		return len(chat) == 1
	})

	// A new panel reads the same console from its start.
	e.s.gameRuns = gameRuns{}
	e.s.resumeGames(ctx)
	// The old console has been read again once it is in the hub.
	h := e.s.consoles.hub("survival")
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.lines) == 2
	})
	e.core.emit(id, "[12:01:00 INFO]: <Steve> again\n")
	waitFor(t, func() bool {
		chat, _ := e.s.Store.PlayerChat(ctx, "survival", "", 0, 10)
		return len(chat) == 2
	})
	if p, _ := e.s.Store.Player(ctx, "survival", "name:Steve"); !p.Online {
		t.Errorf("Steve was closed out by the resume: %+v", p)
	}
	sess, _ := e.s.Store.PlayerSessions(ctx, "survival", "name:Steve", 10)
	if len(sess) != 1 {
		t.Errorf("sessions %+v", sess)
	}
}

func TestResumeClosesSessionsOfServersThatAreNotRunning(t *testing.T) {
	e := newPowerEnv(t)
	ctx := context.Background()
	e.newGame(t, "survival", consoleEggURL, nil)
	at := e.s.now()
	err := e.s.Store.RecordPlayerEvents(ctx, "survival", []players.Event{{Kind: players.Join, PlayerID: "1", Name: "Ann", At: at}})
	if err != nil {
		t.Fatal(err)
	}
	e.s.resumeGames(ctx)
	p, err := e.s.Store.Player(ctx, "survival", "1")
	if err != nil || p.Online {
		t.Fatalf("player %+v %v", p, err)
	}
	sess, _ := e.s.Store.PlayerSessions(ctx, "survival", "1", 1)
	if sess[0].Reason != store.ReasonPanelRestarted {
		t.Errorf("session %+v", sess[0])
	}
}
