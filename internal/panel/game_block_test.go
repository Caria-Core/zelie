package panel

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/Caria-Core/zelie/internal/store"
)

// testPool is a pool of the given ports on one address; ports in held
// belong to a server. Ids follow the order given.
func testPool(ip string, held []int, ports ...int) []store.Allocation {
	var out []store.Allocation
	for i, p := range ports {
		a := store.Allocation{ID: int64(i + 1), IP: ip, Port: p}
		if slices.Contains(held, p) {
			a.AppID = "other"
		}
		out = append(out, a)
	}
	return out
}

func portsOf(p placement) []int { return portNumbers(p.Allocations) }

func TestPlaceBlock(t *testing.T) {
	// 25565 is alone, 25567 is held, 25569 and 25570 are the first two
	// free ports in a row.
	list := testPool("0.0.0.0", []int{25567}, 25565, 25567, 25568, 25569, 25570, 25572, 25573, 25574)
	p, err := placeBlock(list, needs{Ports: 2, Block: 2})
	if err != nil || !slices.Equal(portsOf(p), []int{25568, 25569}) {
		t.Fatalf("lowest run: %v, %v", portsOf(p), err)
	}
	// Three in a row skips the pair.
	if p, err = placeBlock(list, needs{Ports: 3, Block: 3}); err != nil || !slices.Equal(portsOf(p), []int{25568, 25569, 25570}) {
		t.Fatalf("three: %v, %v", portsOf(p), err)
	}
	if p, err = placeBlock(list, needs{Ports: 4, Block: 4}); err == nil {
		t.Fatalf("no run of four, got %v", portsOf(p))
	}

	// More ports than the run: the rest are the lowest free, after the run
	// and after the ports asked for by variable.
	if p, err = placeBlock(list, needs{Ports: 4, Block: 2, Chosen: []int64{8}}); err != nil || !slices.Equal(portsOf(p), []int{25568, 25569, 25574, 25565}) {
		t.Fatalf("with a variable: %v, %v", portsOf(p), err)
	}
	// A port a variable asked for is not part of the run.
	if p, err = placeBlock(list, needs{Ports: 3, Block: 2, Chosen: []int64{4}}); err != nil || !slices.Equal(portsOf(p), []int{25572, 25573, 25569}) {
		t.Fatalf("variable inside the first run: %v, %v", portsOf(p), err)
	}

	// The game port chosen by hand must start a free run.
	chosen := func(port int, more ...int64) needs {
		n := needs{Ports: 2, Block: 2, Primary: true}
		for _, a := range list {
			if a.Port == port {
				n.Chosen = append(n.Chosen, a.ID)
			}
		}
		n.Chosen = append(n.Chosen, more...)
		n.Ports += len(more)
		return n
	}
	if p, err = placeBlock(list, chosen(25572)); err != nil || !slices.Equal(portsOf(p), []int{25572, 25573}) {
		t.Errorf("chosen: %v, %v", portsOf(p), err)
	}
	for name, port := range map[string]int{"last of the pool": 25574, "next is held": 25565, "gap": 25570} {
		if _, err = placeBlock(list, chosen(port)); err == nil {
			t.Errorf("%s: placed", name)
		}
	}
	// The run may not take a port a variable chose.
	if _, err = placeBlock(list, chosen(25572, 7)); err == nil {
		t.Error("run over a port a variable holds")
	}

	// Another address does not continue a run.
	mixed := testPool("0.0.0.0", nil, 25565, 25566)
	mixed[1].IP = "192.0.2.1"
	if _, err = placeBlock(mixed, needs{Ports: 2, Block: 2}); err == nil {
		t.Error("a run across two addresses")
	}
}

func newBlockEnv(t *testing.T) *appEnv {
	t.Helper()
	e := newRustEnv(t)
	// Valheim needs the game port and the next one; the Rust egg gives it
	// variables to test with.
	e.s.Eggs.(*fakeEggs).files["valheim"] = rustEgg(t)
	e.s.Eggs.(*fakeEggs).files["7-days-to-die"] = rustEgg(t)
	return e
}

func TestCreateGameWithABlock(t *testing.T) {
	e := newBlockEnv(t)
	ids := e.poolIDs(t)
	code, out := e.b.do("POST", "/api/games", map[string]any{"name": "vh", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"}})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "vh")
	// The catalog says two ports; both belong to the run.
	if ports, main, _ := e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25565, 25566}) || main != 25565 {
		t.Errorf("ports %v, main %d", ports, main)
	}
	if out["block"] != 2.0 {
		t.Errorf("block %v", out["block"])
	}

	// A choice of game port takes the run from there, and a variable's port
	// comes after it.
	code, out = e.b.do("POST", "/api/games", map[string]any{
		"name": "vh2", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"},
		"allocations": map[string]any{"primary": ids[25570], "variables": map[string]int64{"QUERY_PORT": ids[25575]}},
	})
	if code != http.StatusCreated {
		t.Fatalf("choose: %d %v", code, out)
	}
	e.install(t, "vh2")
	ports, main, vars := e.serverPorts(t, "vh2")
	if !slices.Equal(ports, []int{25570, 25571, 25575}) || main != 25570 || vars["QUERY_PORT"] != "25575" {
		t.Errorf("ports %v, main %d, variables %v", ports, main, vars)
	}

	// A game port with nothing free after it is refused, and nothing is kept.
	before := len(e.poolIDs(t))
	code, out = e.b.do("POST", "/api/games", map[string]any{
		"name": "vh3", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"},
		"allocations": map[string]any{"primary": ids[25580]},
	})
	if code != http.StatusConflict || out["code"] != "allocation.broken_block" {
		t.Errorf("last port: %d %v", code, out)
	}
	code, out = e.b.do("POST", "/api/games", map[string]any{
		"name": "vh3", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"},
		"allocations": map[string]any{"primary": ids[25571]},
	})
	if code != http.StatusConflict || out["code"] != "allocation.not_free" {
		t.Errorf("held follower: %d %v", code, out)
	}
	if _, err := e.s.Store.App(context.Background(), "vh3"); err == nil || len(e.poolIDs(t)) != before {
		t.Error("a refused request left a server")
	}
}

func TestCreateGameWithoutARun(t *testing.T) {
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["7-days-to-die"] = rustEgg(t)
	// The pool has three ports in a row; the game needs four.
	code, out := e.b.do("POST", "/api/games", map[string]any{"name": "sdtd", "egg": "7-days-to-die", "variables": map[string]string{"RCON_PASS": "x"}})
	if code != http.StatusConflict || out["code"] != "allocation.no_block" {
		t.Errorf("no run: %d %v", code, out)
	}
	if _, err := e.s.Store.App(context.Background(), "sdtd"); err == nil {
		t.Error("a refused request left a server")
	}
}

func TestChangePortsOfAGameWithABlock(t *testing.T) {
	e := newBlockEnv(t)
	ids := e.poolIDs(t)
	if code, out := e.b.do("POST", "/api/games", map[string]any{
		"name": "vh", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"}, "ports": 3,
	}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "vh")
	put := func(body map[string]any) (int, map[string]any) { return e.b.do("PUT", "/api/games/vh/ports", body) }
	// The run is 25565-25566 and a variable holds 25567.
	if ports, _, vars := e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25565, 25566, 25567}) || vars["QUERY_PORT"] != "25567" {
		t.Fatalf("start: %v %v", ports, vars)
	}

	// Moving the game port moves the run with it, and the variable keeps
	// its port.
	code, out := put(map[string]any{"primary": ids[25570]})
	if code != http.StatusOK {
		t.Fatalf("move: %d %v", code, out)
	}
	ports, main, vars := e.serverPorts(t, "vh")
	if !slices.Equal(ports, []int{25567, 25570, 25571}) || main != 25570 || vars["QUERY_PORT"] != "25567" {
		t.Errorf("after the move: %v, main %d, %v", ports, main, vars)
	}
	if out["block"] != 2.0 || out["block_broken"] != nil {
		t.Errorf("answer: %v %v", out["block"], out["block_broken"])
	}
	for _, p := range out["ports"].([]any) {
		q := p.(map[string]any)
		if want := map[float64]float64{25571: 1}[q["port"].(float64)]; q["offset"] == nil && want != 0 || q["offset"] != nil && q["offset"] != want {
			t.Errorf("port %v has offset %v", q["port"], q["offset"])
		}
	}

	// Changing only a variable keeps the run.
	if code, out = put(map[string]any{"variables": map[string]int64{"QUERY_PORT": ids[25575]}}); code != http.StatusOK {
		t.Fatalf("variable: %d %v", code, out)
	}
	if ports, main, _ = e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25570, 25571, 25575}) || main != 25570 {
		t.Errorf("after the variable: %v, main %d", ports, main)
	}

	// The follower listed as an extra, as an older page might, is not a
	// second role.
	if code, out = put(map[string]any{"extra": []int64{ids[25571]}}); code != http.StatusOK {
		t.Fatalf("extra follower: %d %v", code, out)
	}
	if ports, _, _ = e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25570, 25571, 25575}) {
		t.Errorf("after the extra: %v", ports)
	}

	before, _, beforeVars := e.serverPorts(t, "vh")
	for name, c := range map[string]struct {
		body map[string]any
		err  string
	}{
		"nothing after":            {map[string]any{"primary": ids[25580]}, "allocation.broken_block"},
		"follower held elsewhere":  {map[string]any{"primary": ids[25574]}, "allocation.broken_block"},
		"onto the variable's port": {map[string]any{"primary": ids[25574], "variables": map[string]int64{"QUERY_PORT": ids[25575]}}, "allocation.twice"},
		"variable on the run":      {map[string]any{"variables": map[string]int64{"QUERY_PORT": ids[25571]}}, "allocation.twice"},
	} {
		if name == "follower held elsewhere" {
			// 25574's follower 25575 is the variable's port, which it keeps.
			c.err = "allocation.twice"
		}
		if code, out := put(c.body); code == http.StatusOK || out["code"] != c.err {
			t.Errorf("%s: %d %v", name, code, out)
		}
		if after, _, afterVars := e.serverPorts(t, "vh"); !slices.Equal(before, after) || afterVars["QUERY_PORT"] != beforeVars["QUERY_PORT"] {
			t.Errorf("%s changed the server: %v", name, after)
		}
	}

	// The next start opens every port of the run.
	if code, out := e.power(t, "vh", "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "vh")
	var tcp, udp []int
	for _, f := range e.core.forwards["vh"] {
		if f.Proto == "tcp" {
			tcp = append(tcp, int(f.Port))
		} else {
			udp = append(udp, int(f.Port))
		}
	}
	slices.Sort(tcp)
	slices.Sort(udp)
	want := []int{25570, 25571, 25575}
	if !slices.Equal(tcp, want) || !slices.Equal(udp, want) {
		t.Errorf("forwards tcp %v udp %v, want %v", tcp, udp, want)
	}
}

func TestBrokenBlockOfAnOlderServer(t *testing.T) {
	e := newBlockEnv(t)
	ids := e.poolIDs(t)
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "vh", "egg": "valheim", "variables": map[string]string{"RCON_PASS": "x"}}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "vh")
	// A server made before the run was kept together: its ports are 25565
	// and 25569.
	a, _ := e.s.Store.App(context.Background(), "vh")
	g, _ := e.s.Store.GameServer(context.Background(), "vh")
	if err := e.s.Store.ReplaceGamePorts(context.Background(), store.ThisNode, "vh", []int64{ids[25565], ids[25569]}, 25565, g.Variables); err != nil || a.Port != 25565 {
		t.Fatalf("break the run: %v", err)
	}
	// Another server took the port after the game port.
	e.newGame(t, "other", consoleEggURL, map[string]any{"ports": 1})
	if ports, _, _ := e.serverPorts(t, "other"); !slices.Equal(ports, []int{25566}) {
		t.Fatalf("other server: %v", ports)
	}
	code, out := e.b.do("GET", "/api/games/vh", nil)
	if code != http.StatusOK || out["block"] != 2.0 || out["block_broken"] != true {
		t.Fatalf("get: %d %v %v", code, out["block"], out["block_broken"])
	}
	for _, p := range out["ports"].([]any) {
		if p.(map[string]any)["offset"] != nil {
			t.Errorf("a broken run has offsets: %v", p)
		}
	}

	// Other changes go through, with the ports it holds as they are.
	code, out = e.b.do("PUT", "/api/games/vh/ports", map[string]any{"extra": []int64{ids[25569]}, "variables": map[string]int64{"QUERY_PORT": ids[25572]}})
	if code != http.StatusOK {
		t.Fatalf("change a variable: %d %v", code, out)
	}
	if ports, _, _ := e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25565, 25569, 25572}) {
		t.Errorf("ports %v", ports)
	}
	// The game port moving to a free run mends it.
	code, out = e.b.do("PUT", "/api/games/vh/ports", map[string]any{"primary": ids[25574], "extra": []int64{ids[25569]}, "variables": map[string]int64{"QUERY_PORT": ids[25572]}})
	if code != http.StatusOK || out["block_broken"] != nil {
		t.Fatalf("mend: %d %v", code, out)
	}
	if ports, _, _ := e.serverPorts(t, "vh"); !slices.Equal(ports, []int{25569, 25572, 25574, 25575}) {
		t.Errorf("mended ports %v", ports)
	}
}
