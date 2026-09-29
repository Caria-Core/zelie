package panel

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
)

// consoleEgg with the feature Minecraft's eggs carry.
var eulaEgg = strings.Replace(consoleEgg, `"name": "Console Game",`, `"name": "Console Game", "features": ["eula"],`, 1)

const eulaEggURL = "https://example.com/eula.json"

func newEULAEnv(t *testing.T) *consoleEnv {
	t.Helper()
	e := newConsoleEnv(t)
	e.s.Eggs.(*fakeEggs).files[eulaEggURL] = eulaEgg
	return e
}

// unacceptedGame adds an installed server whose egg asks for the EULA that
// nobody accepted, as one made before the wizard asked would be.
func (e *appEnv) unacceptedGame(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	raw := []byte(eulaEgg)
	stored, err := e.s.Store.AddEgg(ctx, "Console Game", eulaEggURL, raw, e.s.now())
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.s.place(ctx, store.ThisNode, needs{Ports: 1})
	if err != nil {
		t.Fatal(err)
	}
	a := store.App{
		ID: name, Kind: store.KindGame, Source: store.SourceImage, Image: "ghcr.io/example/java:21",
		MemoryMB: 1024, CPUs: 1, HealthPath: "/", CreatedAt: e.s.now(), Port: p.Allocations[0].Port,
	}
	if err := e.s.Store.CreateApp(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := e.s.assign(ctx, name, p); err != nil {
		t.Fatal(err)
	}
	g := store.GameServer{AppID: name, EggID: stored.ID, Image: a.Image, Startup: "./run", Variables: map[string]string{"WORLD": "w"}}
	if err := e.s.Store.CreateGameServer(ctx, g); err != nil {
		t.Fatal(err)
	}
	path, disk := gameVolumePath, int64(1024)
	if _, err := e.s.createVolume(ctx, a, volumeRequest{Path: &path, LimitMB: &disk}); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Store.SetInstall(ctx, name, store.InstallDone, 0, e.s.now()); err != nil {
		t.Fatal(err)
	}
}

func TestCreatingAServerNeedsTheEULA(t *testing.T) {
	e := newEULAEnv(t)
	body := map[string]any{"name": "mc", "egg_url": eulaEggURL}
	code, out := e.b.do("POST", "/api/games", body)
	if code != http.StatusBadRequest || out["code"] != "game.eula_required" {
		t.Fatalf("without acceptance: %d %v", code, out)
	}
	if _, err := e.s.Store.App(context.Background(), "mc"); err == nil {
		t.Error("a refused request left a server behind")
	}

	// An egg without the feature does not ask.
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "plain", "egg_url": consoleEggURL}); code != http.StatusCreated {
		t.Fatalf("plain egg: %d %v", code, out)
	}
	e.install(t, "plain")
	code, out = e.b.do("GET", "/api/games/plain", nil)
	if code != http.StatusOK || out["eula_needed"] != nil || len(out["features"].([]any)) != 0 {
		t.Errorf("plain egg: %d %v", code, out)
	}

	body["accept_eula"] = true
	if code, out := e.b.do("POST", "/api/games", body); code != http.StatusCreated {
		t.Fatalf("with acceptance: %d %v", code, out)
	}
	e.install(t, "mc")
	g, err := e.s.Store.GameServer(context.Background(), "mc")
	if err != nil || g.EULAAcceptedAt.IsZero() {
		t.Fatalf("acceptance not stored: %+v %v", g, err)
	}
	code, out = e.b.do("GET", "/api/games/mc", nil)
	feats := out["features"].([]any)
	if code != http.StatusOK || len(feats) != 1 || feats[0] != "eula" || out["eula_needed"] != nil {
		t.Errorf("get: %d %v", code, out)
	}
}

func TestEggPreviewListsFeatures(t *testing.T) {
	e := newEULAEnv(t)
	code, out := e.b.do("POST", "/api/eggs/preview", map[string]any{"egg_url": eulaEggURL})
	feats, _ := out["features"].([]any)
	if code != http.StatusOK || len(feats) != 1 || feats[0] != "eula" {
		t.Errorf("preview: %d %v", code, out)
	}
	code, out = e.b.do("POST", "/api/eggs/preview", map[string]any{"egg_url": consoleEggURL})
	if feats, _ := out["features"].([]any); code != http.StatusOK || feats == nil || len(feats) != 0 {
		t.Errorf("preview without features: %d %v", code, out)
	}
}

func TestEULAIsWrittenBeforeEachStartWhenAccepted(t *testing.T) {
	e := newEULAEnv(t)
	e.newGame(t, "mc", eulaEggURL, map[string]any{"accept_eula": true})
	e.power(t, "mc", "start")
	e.settle(t, "mc")

	eulaFiles := func(i int) int {
		n := 0
		for _, f := range e.core.prepares[i].Req.Files {
			if f.Path != "eula.txt" {
				continue
			}
			n++
			if f.Parser != "properties" || len(f.Changes) != 1 || f.Changes[0].Key != "eula" || f.Changes[0].Value != "true" {
				t.Errorf("eula.txt: %+v", f)
			}
		}
		return n
	}
	if len(e.core.prepares) != 1 || eulaFiles(0) != 1 {
		t.Fatalf("prepares: %+v", e.core.prepares)
	}
	// The egg's own files are still there.
	if len(e.core.prepares[0].Req.Files) != 3 {
		t.Errorf("files: %+v", e.core.prepares[0].Req.Files)
	}
	e.power(t, "mc", "restart")
	e.settle(t, "mc")
	if len(e.core.prepares) != 2 || eulaFiles(1) != 1 {
		t.Errorf("after a restart: %+v", e.core.prepares)
	}

	// Without the feature nothing is added.
	e.newGame(t, "plain", consoleEggURL, map[string]any{"ports": 1})
	e.power(t, "plain", "start")
	e.settle(t, "plain")
	if last := e.core.prepares[len(e.core.prepares)-1]; len(last.Req.Files) != 2 {
		t.Errorf("plain egg files: %+v", last.Req.Files)
	}
}

func TestAcceptEULAEndpoint(t *testing.T) {
	e := newEULAEnv(t)
	e.unacceptedGame(t, "old")

	code, out := e.b.do("GET", "/api/games/old", nil)
	if code != http.StatusOK || out["eula_needed"] != true {
		t.Fatalf("before: %d %v", code, out)
	}
	// Nothing is written for a server that has not accepted.
	e.power(t, "old", "start")
	e.settle(t, "old")
	for _, f := range e.core.prepares[0].Req.Files {
		if f.Path == "eula.txt" {
			t.Errorf("eula.txt was written before acceptance: %+v", f)
		}
	}
	e.power(t, "old", "kill")
	e.waitState(t, "old", "stopped")

	if code, out := e.b.do("POST", "/api/games/nothing/eula", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("unknown server: %d %v", code, out)
	}
	code, out = e.b.do("POST", "/api/games/old/eula", nil)
	if code != http.StatusOK || out["eula_needed"] != nil {
		t.Fatalf("accept: %d %v", code, out)
	}
	if g, _ := e.s.Store.GameServer(context.Background(), "old"); g.EULAAcceptedAt.IsZero() {
		t.Error("acceptance not stored")
	}
	// Asking again changes nothing.
	if code, _ := e.b.do("POST", "/api/games/old/eula", nil); code != http.StatusOK {
		t.Errorf("again: %d", code)
	}

	e.power(t, "old", "start")
	e.settle(t, "old")
	n := len(e.core.prepares)
	found := false
	for _, f := range e.core.prepares[n-1].Req.Files {
		found = found || f.Path == "eula.txt"
	}
	if !found {
		t.Errorf("eula.txt not written after acceptance: %+v", e.core.prepares[n-1].Req.Files)
	}

	// A game without the feature has nothing to accept.
	e.newGame(t, "plain", consoleEggURL, nil)
	if code, out := e.b.do("POST", "/api/games/plain/eula", nil); code != http.StatusConflict || out["code"] != "game.no_eula" {
		t.Errorf("plain: %d %v", code, out)
	}
}

func TestEULAEndpointIsForAdmins(t *testing.T) {
	e := newEULAEnv(t)
	e.unacceptedGame(t, "old")
	nobody := &browser{t: t, h: e.b.h, ip: "198.51.100.9"}
	if code, _ := nobody.do("POST", "/api/games/old/eula", nil); code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", code)
	}
}

func TestConsoleSaysWhenTheEULAStoppedTheServer(t *testing.T) {
	e := newEULAEnv(t)
	e.unacceptedGame(t, "old")
	e.power(t, "old", "start")
	e.settle(t, "old")
	id := e.liveContainer(t, "old")
	c := e.connect(t, "old")

	e.core.emit(id, "[12:00:01 INFO]: Loading libraries\n")
	e.core.emit(id, "[12:00:03 INFO]: You need to agree to the EULA in order to run the server. Go to eula.txt for more info.\n")
	msgs := readUntil(t, c, func(m map[string]any) bool { return m["type"] == "eula" })
	if len(msgs) == 0 {
		t.Fatal("no eula message")
	}
	// Colour codes and case do not hide it.
	e.core.emit(id, "\x1b[31mYOU NEED TO AGREE TO THE EULA IN ORDER TO RUN THE SERVER\x1b[m\n")
	readUntil(t, c, func(m map[string]any) bool { return m["type"] == "eula" })

	// A game without the feature that prints the same words is left alone.
	plain := e.running(t, "plain")
	d := e.connect(t, "plain")
	e.core.emit(plain, "You need to agree to the EULA in order to run the server\n")
	e.core.emit(plain, "after\n")
	for _, m := range readUntil(t, d, isLine("after")) {
		if m["type"] == "eula" {
			t.Errorf("unexpected %v", m)
		}
	}
}
