package panel

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/store"
)

func TestVolumes(t *testing.T) {
	e := newAppEnv(t)
	code, out := e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "itzg/minecraft-server",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 2048}}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	first := e.settle(t, "mc")
	if first.State != store.DeployLive {
		t.Fatalf("first deployment %+v", first)
	}
	vols := volumesOf(t, e, "mc")
	if !strings.HasPrefix(vols[0].Name, "vol-") || len(vols[0].Name) != 20 {
		t.Errorf("volume name %q", vols[0].Name)
	}
	want := []engine.VolumeMount{{Name: vols[0].Name, Target: "/data"}}
	if got := e.core.mounts[fmt.Sprintf("mc-%d", first.ID)]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("mounts %v, want %v", got, want)
	}

	// The old version stops before the new one starts.
	e.b.do("POST", "/api/apps/mc/restart", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployLive {
		t.Fatalf("restart %+v", d)
	}
	if e.core.overlapping {
		t.Error("two versions ran with the same volume")
	}

	for _, c := range []struct {
		body map[string]any
		want int
	}{
		{map[string]any{"path": "/proc/x"}, http.StatusBadRequest},
		{map[string]any{"path": "data"}, http.StatusBadRequest},
		{map[string]any{"path": "/data/worlds"}, http.StatusConflict},
		{map[string]any{"path": "/plugins", "limit_mb": 1}, http.StatusBadRequest},
		{map[string]any{"path": "/plugins", "limit_mb": 1 << 30}, http.StatusBadRequest},
		{map[string]any{"path": "/plugins"}, http.StatusCreated},
	} {
		if code, out := e.b.do("POST", "/api/apps/mc/volumes", c.body); code != c.want {
			t.Errorf("add %v: %d %v, want %d", c.body, code, out, c.want)
		}
	}
	if code, _ := e.b.do("PATCH", "/api/apps/mc/volumes/2", map[string]any{"path": "/data"}); code != http.StatusConflict {
		t.Errorf("moving onto another volume: %d", code)
	}

	// Deleting needs the app stopped.
	second := volumesOf(t, e, "mc")[1]
	if code, _ := e.b.do("DELETE", "/api/apps/mc/volumes/2", nil); code != http.StatusConflict {
		t.Errorf("delete while running: %d", code)
	}
	e.b.do("POST", "/api/apps/mc/stop", nil)
	if code, out := e.b.do("DELETE", "/api/apps/mc/volumes/2", nil); code != http.StatusNoContent {
		t.Fatalf("delete while stopped: %d %v", code, out)
	}
	if e.core.volumes[second.Name] {
		t.Error("the core still has the deleted volume")
	}

	// Deleting the app deletes what is left.
	if code, _ := e.b.do("DELETE", "/api/apps/mc", nil); code != http.StatusNoContent {
		t.Fatalf("delete app: %d", code)
	}
	if len(e.core.volumes) != 0 {
		t.Errorf("volumes left after deleting the app: %v", e.core.volumes)
	}
}

func TestVolumeOverItsLimitStopsTheApp(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "itzg/minecraft-server",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 100}}})
	e.settle(t, "mc")

	e.core.sizes = map[string]int64{volumesOf(t, e, "mc")[0].Name: 101 << 20}
	e.s.checkVolumes(context.Background())
	a, _ := e.s.Store.App(context.Background(), "mc")
	if !a.Stopped {
		t.Fatal("the app kept running over its limit")
	}
	_, out := e.b.do("GET", "/api/apps/mc", nil)
	if full, _ := out["volume_full"].(map[string]any); full["code"] != "volume.full" || !strings.Contains(fmt.Sprint(full["params"]), "path:/data") {
		t.Errorf("volume_full %v", out["volume_full"])
	}

	for _, path := range []string{"start", "restart", "deployments"} {
		if code, out := e.b.do("POST", "/api/apps/mc/"+path, nil); code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "over its") {
			t.Errorf("%s over the limit: %d %v", path, code, out)
		}
	}

	// A push or a recovery that starts anyway leaves the app stopped, so it
	// is not retried over and over.
	e.s.Store.SetStopped(context.Background(), "mc", false)
	live, _ := e.s.Store.LiveDeployment(context.Background(), "mc")
	e.s.deploy(context.Background(), a, store.Deployment{Version: live.Version, Image: live.Image, Cause: store.CauseRecover})
	if d := e.settle(t, "mc"); d.State != store.DeployFailed {
		t.Fatalf("recovery over the limit: %+v", d)
	}
	if a, _ := e.s.Store.App(context.Background(), "mc"); !a.Stopped {
		t.Error("the app is not marked stopped after failing over its limit")
	}

	if code, _ := e.b.do("PATCH", "/api/apps/mc/volumes/1", map[string]any{"limit_mb": 200}); code != http.StatusOK {
		t.Fatalf("raise the limit: %d", code)
	}
	e.b.do("POST", "/api/apps/mc/start", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployLive {
		t.Fatalf("start after raising the limit: %+v", d)
	}
}

// A volume the core cannot measure has no size to compare with its limit. If
// that let the app run, a tenant could switch its own limit off by nesting
// folders deeper than the measurement goes.
func TestVolumeThatCannotBeMeasuredStopsTheApp(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "itzg/minecraft-server",
		"volumes": []map[string]any{{"path": "/data", "limit_mb": 100}}})
	e.settle(t, "mc")
	vol := volumesOf(t, e, "mc")[0]

	// It was measured once, and fine. Now the measurement fails.
	e.core.sizes = map[string]int64{vol.Name: 1 << 20}
	e.s.checkVolumes(context.Background())
	if a, _ := e.s.Store.App(context.Background(), "mc"); a.Stopped {
		t.Fatal("the app was stopped with its volume within the limit")
	}
	e.core.unmeasured = []string{vol.Name}
	e.s.checkVolumes(context.Background())
	a, _ := e.s.Store.App(context.Background(), "mc")
	if !a.Stopped {
		t.Fatal("the app kept running with a volume that cannot be measured")
	}
	_, out := e.b.do("GET", "/api/apps/mc", nil)
	if why, _ := out["volume_full"].(map[string]any); why["code"] != "volume.unmeasured" || !strings.Contains(fmt.Sprint(why["params"]), "path:/data") {
		t.Errorf("volume_full %v", out["volume_full"])
	}
	if code, out := e.b.do("POST", "/api/apps/mc/start", nil); code != http.StatusConflict || out["code"] != "volume.unmeasured" {
		t.Errorf("start: %d %v", code, out)
	}

	// A measurement that works again lets it start.
	e.core.unmeasured = nil
	e.s.checkVolumes(context.Background())
	e.b.do("POST", "/api/apps/mc/start", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployLive {
		t.Fatalf("start after it could be measured: %+v", d)
	}
}

func TestFailedVersionWithVolumesBringsTheOldOneBack(t *testing.T) {
	e := newAppEnv(t)
	e.b.do("POST", "/api/apps", map[string]any{"id": "mc", "source": "image", "image": "good:1",
		"volumes": []map[string]any{{"path": "/data"}}})
	live := e.settle(t, "mc")

	e.core.crashImage = "bad:2"
	e.b.do("PATCH", "/api/apps/mc", map[string]any{"image": "bad:2"})
	e.b.do("POST", "/api/apps/mc/deployments", nil)
	if d := e.settle(t, "mc"); d.State != store.DeployFailed {
		t.Fatalf("bad version %+v", d)
	}
	st, err := e.s.containerStatus(context.Background(), fmt.Sprintf("mc-%d", live.ID))
	if err != nil || st.State != "running" || st.Image != "good:1" {
		t.Errorf("the old version is not back: %+v %v", st, err)
	}
	if e.core.overlapping {
		t.Error("two versions ran with the same volume")
	}
}

func volumesOf(t *testing.T, e *appEnv, app string) []store.Volume {
	t.Helper()
	vols, err := e.s.Store.Volumes(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return vols
}

// A game server's main volume is where the file manager, SFTP and the egg's
// startup look for it, so it stays where it is, as a files app's does.
func TestGameServersMainVolumeStaysWhereItIs(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	main := volumesOf(t, e, "survival")[0]
	id := itoa(main.ID)
	if code, out := e.b.do("PATCH", "/api/apps/survival/volumes/"+id, map[string]any{"path": "/data"}); code != http.StatusConflict || out["code"] != "volume.files_main" {
		t.Errorf("move: %d %v", code, out)
	}
	if code, out := e.b.do("DELETE", "/api/apps/survival/volumes/"+id, nil); code != http.StatusConflict || out["code"] != "volume.files_main" {
		t.Errorf("delete: %d %v", code, out)
	}
	if code, out := e.b.do("PATCH", "/api/apps/survival/volumes/"+id, map[string]any{"limit_mb": main.LimitMB + 1024}); code != http.StatusOK {
		t.Errorf("resize: %d %v", code, out)
	}
	if got := volumesOf(t, e, "survival")[0]; got.Path != main.Path {
		t.Errorf("the volume is now at %s", got.Path)
	}
}

// A game server that outgrows its disk stops the way its egg says, with the
// time to save, not with a SIGTERM and a kill ten seconds later.
func TestGameServerOverItsLimitStopsLikeThePowerButton(t *testing.T) {
	e := newPowerEnv(t)
	e.newGame(t, "survival", consoleEggURL, nil)
	e.power(t, "survival", "start")
	e.settle(t, "survival")
	id := e.liveContainer(t, "survival")
	e.core.emit(id, "Done (1s)! For help, type \"help\"\n")
	e.waitState(t, "survival", "running")

	vol := volumesOf(t, e, "survival")[0]
	e.core.sizes = map[string]int64{vol.Name: vol.LimitMB<<20 + 1}
	e.s.checkVolumes(context.Background())
	e.settle(t, "survival")
	e.waitState(t, "survival", "stopped")

	if got := e.core.consoles[id]; len(got) != 1 || got[0] != "stop\n" {
		t.Errorf("console %q, want the egg's stop command", got)
	}
	if len(e.core.signals[id]) != 0 {
		t.Errorf("signals %v", e.core.signals[id])
	}
	if a, _ := e.s.Store.App(context.Background(), "survival"); !a.Stopped {
		t.Error("the server is not kept down")
	}
}

// A files app runs an egg and has the same stop button as a game server, so
// over its limit it stops the way that button stops it.
func TestFilesAppOverItsLimitStopsLikeItsStopButton(t *testing.T) {
	e := newRuntimeEnv(t)
	e.newFiles(t, "bot", nil)
	e.power(t, "bot", "start")
	e.settle(t, "bot")
	e.waitState(t, "bot", "running")
	id := e.liveContainer(t, "bot")

	vol := volumesOf(t, e, "bot")[0]
	e.core.sizes = map[string]int64{vol.Name: vol.LimitMB<<20 + 1}
	e.s.checkVolumes(context.Background())
	e.settle(t, "bot")
	e.waitState(t, "bot", "stopped")

	// The egg stops with ^C, which is a signal to the process, not the
	// container's stop with its ten seconds.
	if got := e.core.signals[id]; len(got) != 1 || got[0] != "SIGINT" {
		t.Errorf("signals %v, want the egg's stop signal", got)
	}
	if a, _ := e.s.Store.App(context.Background(), "bot"); !a.Stopped {
		t.Error("the app is not kept down")
	}
}
