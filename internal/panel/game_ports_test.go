package panel

import (
	"context"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/egg"
	"github.com/Caria-Core/zelie/internal/store"
)

// rustEgg is Pelican's Rust egg, as the catalog serves it.
func rustEgg(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../egg/testdata/rust-plcn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func envNames(vars []egg.Variable) []string {
	var out []string
	for _, v := range vars {
		out = append(out, v.Env)
	}
	return out
}

func TestPortVariables(t *testing.T) {
	e, err := egg.Parse([]byte(rustEgg(t)))
	if err != nil {
		t.Fatal(err)
	}
	// In the order the startup command names them, not the egg's own.
	got := envNames(portVariables(e))
	if want := []string{"QUERY_PORT", "RCON_PORT", "APP_PORT"}; !slices.Equal(got, want) {
		t.Errorf("port variables %v, want %v", got, want)
	}

	for _, c := range []struct {
		env, def string
		want     bool
	}{
		{"QUERY_PORT", "27017", true},
		{"RCON_PORT", "", true},
		{"SERVER_PORT", "25565", false},
		{"APP_PORT", "-1", false},
		{"WEB_PORT", "http", false},
		{"PORTAL", "1", false},
		{"MAX_PLAYERS", "40", false},
	} {
		if got := isPortVariable(egg.Variable{Env: c.env, Default: c.def}); got != c.want {
			t.Errorf("%s=%q: %v", c.env, c.def, got)
		}
	}
}

func TestAssignPorts(t *testing.T) {
	e, _ := egg.Parse([]byte(rustEgg(t)))
	fresh := func() map[string]string {
		return map[string]string{"QUERY_PORT": "27017", "RCON_PORT": "28016", "APP_PORT": "28082", "MAX_PLAYERS": "40"}
	}

	vars := fresh()
	assignPorts(e, vars, nil, []int{30001, 30002, 30003, 30004})
	if vars["QUERY_PORT"] != "30001" || vars["RCON_PORT"] != "30002" || vars["APP_PORT"] != "30003" || vars["MAX_PLAYERS"] != "40" {
		t.Errorf("all ports: %v", vars)
	}

	// Fewer ports than variables: the rest keep the egg's defaults.
	vars = fresh()
	assignPorts(e, vars, nil, []int{30001})
	if vars["QUERY_PORT"] != "30001" || vars["RCON_PORT"] != "28016" || vars["APP_PORT"] != "28082" {
		t.Errorf("one extra port: %v", vars)
	}

	// A variable the request set is not touched and takes no port.
	vars = fresh()
	vars["QUERY_PORT"] = "31000"
	assignPorts(e, vars, map[string]string{"QUERY_PORT": "31000"}, []int{30001, 30002})
	if vars["QUERY_PORT"] != "31000" || vars["RCON_PORT"] != "30001" || vars["APP_PORT"] != "30002" {
		t.Errorf("set by the user: %v", vars)
	}

	// No extra ports: nothing changes.
	vars = fresh()
	assignPorts(e, vars, nil, nil)
	if vars["QUERY_PORT"] != "27017" {
		t.Errorf("no extra ports: %v", vars)
	}
}

func TestCreateRustServer(t *testing.T) {
	e, eggs := newGameEnv(t)
	eggs.files["rust"] = rustEgg(t)
	// The pool of the test has three ports; Rust takes four.
	if err := e.s.Store.AddAllocations(context.Background(), 1, "0.0.0.0", []int{25568, 25569}, e.s.now()); err != nil {
		t.Fatal(err)
	}
	catalog := e.b.record("GET", "/api/eggs/catalog", nil).Body.String()
	if !strings.Contains(catalog, `"id":"rust"`) || !strings.Contains(catalog, `"ports":4`) || !strings.Contains(catalog, `"memory_mb":8192`) {
		t.Errorf("catalog: %s", catalog)
	}
	code, out := e.b.do("POST", "/api/eggs/preview", map[string]any{"egg": "rust"})
	if code != http.StatusOK {
		t.Fatalf("preview: %d %v", code, out)
	}
	if uses := out["port_variables"].([]any); len(uses) != 3 || uses[0].(map[string]any)["env"] != "QUERY_PORT" {
		t.Errorf("preview port variables: %v", uses)
	}

	// A catalog egg with no ports in the request takes what the game needs,
	// as far as the machine allows: the test machine has 3 GB.
	code, out = e.b.do("POST", "/api/games", map[string]any{
		"name": "rusty", "egg": "rust", "variables": map[string]string{"RCON_PASS": "s3cret"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if out["memory_mb"] != 3072.0 || out["disk_mb"] != float64(30<<10) {
		t.Errorf("sizes: memory %v, disk %v", out["memory_mb"], out["disk_mb"])
	}
	e.install(t, "rusty")

	req := e.core.installs[0]
	if req.Image != "ghcr.io/pelican-eggs/installers:debian" || req.Entrypoint != "bash" {
		t.Errorf("installer %s %s", req.Image, req.Entrypoint)
	}
	env := strings.Join(req.Env, " ")
	for _, want := range []string{
		"SERVER_PORT=25565", "QUERY_PORT=25566", "RCON_PORT=25567", "APP_PORT=25568",
		"SRCDS_APPID=258550", "RCON_PASS=s3cret", "MAX_PLAYERS=40", "WORLD_SIZE=3000",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("install env lacks %s: %s", want, env)
		}
	}
	if !strings.Contains(req.Script, "+app_update ${SRCDS_APPID}") {
		t.Errorf("script: %.200s", req.Script)
	}

	code, out = e.b.do("GET", "/api/games/rusty", nil)
	if code != http.StatusOK {
		t.Fatalf("get: %d %v", code, out)
	}
	ports := out["ports"].([]any)
	if len(ports) != 4 {
		t.Fatalf("ports: %v", ports)
	}
	usedBy := func(i int) []string {
		var names []string
		for _, u := range ports[i].(map[string]any)["used_by"].([]any) {
			names = append(names, u.(map[string]any)["env"].(string))
		}
		return names
	}
	for i, want := range [][]string{nil, {"QUERY_PORT"}, {"RCON_PORT"}, {"APP_PORT"}} {
		if got := usedBy(i); !slices.Equal(got, want) {
			t.Errorf("port %d used by %v, want %v", i, got, want)
		}
	}
	if steam := out["steam"].(map[string]any); steam["app_id"] != 258550.0 {
		t.Errorf("steam: %v", steam)
	}

	// The startup command the server runs carries each port.
	a, _ := e.s.Store.App(context.Background(), "rusty")
	g, _ := e.s.Store.GameServer(context.Background(), "rusty")
	node, _ := e.s.Store.Node(context.Background(), 1)
	startup := egg.Expand(g.Startup, gameVars(a, g, node), a.Port)
	for _, want := range []string{"+server.port 25565", "+server.queryport 25566", "+rcon.port 25567", "+app.port 25568", `+rcon.password "s3cret"`} {
		if !strings.Contains(startup, want) {
			t.Errorf("startup lacks %s: %s", want, startup)
		}
	}

	// The egg leaves the password empty and requires one.
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "nopass", "egg": "rust"}); code != http.StatusBadRequest || out["code"] != "game.bad_variable" {
		t.Errorf("without a password: %d %v", code, out)
	}
	// The ports it locks cannot be chosen by a customer; the server sets them.
	if code, out := e.asCustomer(t, e.s.createGame, "POST", "", map[string]any{"name": "locked", "egg": "rust", "variables": map[string]string{"RCON_PASS": "x", "QUERY_PORT": "1"}}); code != http.StatusBadRequest || out["code"] != "game.variable_locked" {
		t.Errorf("locked port: %d %v", code, out)
	}
}

// poolIDs maps each port of the node's pool to its allocation id.
func (e *appEnv) poolIDs(t *testing.T) map[int]int64 {
	t.Helper()
	list, err := e.s.Store.Allocations(context.Background(), store.ThisNode)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[int]int64{}
	for _, a := range list {
		ids[a.Port] = a.ID
	}
	return ids
}

// serverPorts is the ports the server holds, by number, with the values of
// its port variables.
func (e *appEnv) serverPorts(t *testing.T, name string) (ports []int, main int, vars map[string]string) {
	t.Helper()
	list, err := e.s.Store.AppAllocations(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range list {
		ports = append(ports, a.Port)
	}
	a, _ := e.s.Store.App(context.Background(), name)
	g, _ := e.s.Store.GameServer(context.Background(), name)
	return ports, a.Port, g.Variables
}

func newRustEnv(t *testing.T) *appEnv {
	t.Helper()
	e := newPowerEnv(t)
	e.s.Eggs.(*fakeEggs).files["rust"] = rustEgg(t)
	var more []int
	for p := 25568; p <= 25580; p++ {
		more = append(more, p)
	}
	if err := e.s.Store.AddAllocations(context.Background(), store.ThisNode, store.AnyAddress, more, e.s.now()); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestChoosePortsForANewServer(t *testing.T) {
	e := newRustEnv(t)
	ids := e.poolIDs(t)
	code, out := e.b.do("POST", "/api/games", map[string]any{
		"name": "rusty", "egg": "rust", "variables": map[string]string{"RCON_PASS": "s3cret"},
		"allocations": map[string]any{"primary": ids[25570], "variables": map[string]int64{"QUERY_PORT": ids[25572]}},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "rusty")
	// The roles left out take the lowest free ports, in the order the
	// startup command uses them.
	ports, main, vars := e.serverPorts(t, "rusty")
	if want := []int{25565, 25566, 25570, 25572}; !slices.Equal(ports, want) {
		t.Errorf("ports %v, want %v", ports, want)
	}
	if main != 25570 || vars["QUERY_PORT"] != "25572" || vars["RCON_PORT"] != "25565" || vars["APP_PORT"] != "25566" {
		t.Errorf("main %d, variables %v", main, vars)
	}

	// Choosing only the game port leaves the rest to automatic placement.
	code, out = e.b.do("POST", "/api/games", map[string]any{
		"name": "second", "egg": "rust", "variables": map[string]string{"RCON_PASS": "x"},
		"allocations": map[string]any{"primary": ids[25580]},
	})
	if code != http.StatusCreated {
		t.Fatalf("second: %d %v", code, out)
	}
	e.install(t, "second")
	ports, main, vars = e.serverPorts(t, "second")
	if want := []int{25567, 25568, 25569, 25580}; !slices.Equal(ports, want) || main != 25580 || vars["QUERY_PORT"] != "25567" {
		t.Errorf("second: ports %v, main %d, variables %v", ports, main, vars)
	}

	free := func() int {
		list, _ := e.s.Store.Allocations(context.Background(), store.ThisNode)
		n := 0
		for _, a := range list {
			if a.AppID == "" {
				n++
			}
		}
		return n
	}
	before := free()
	for name, c := range map[string]struct {
		pick map[string]any
		code int
		err  string
	}{
		"taken":           {map[string]any{"primary": ids[25570]}, 409, "allocation.not_free"},
		"taken variable":  {map[string]any{"variables": map[string]int64{"RCON_PORT": ids[25572]}}, 409, "allocation.not_free"},
		"not in the pool": {map[string]any{"primary": 9999}, 404, "allocation.not_found"},
		"twice":           {map[string]any{"primary": ids[25571], "variables": map[string]int64{"RCON_PORT": ids[25571]}}, 400, "allocation.twice"},
		"not a role":      {map[string]any{"variables": map[string]int64{"RCON_PASS": ids[25571]}}, 400, "game.no_port_role"},
	} {
		code, out := e.b.do("POST", "/api/games", map[string]any{"name": "nope", "egg": "rust", "variables": map[string]string{"RCON_PASS": "x"}, "allocations": c.pick})
		if code != c.code || out["code"] != c.err {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if free() != before {
		t.Errorf("a refused request kept ports")
	}
	if _, err := e.s.Store.App(context.Background(), "nope"); err == nil {
		t.Errorf("a refused request left a server")
	}
}

func TestChangeAServersPorts(t *testing.T) {
	e := newRustEnv(t)
	e.newGame(t, "survival", consoleEggURL, map[string]any{"ports": 1})
	ids := e.poolIDs(t)
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "rusty", "egg": "rust", "variables": map[string]string{"RCON_PASS": "x"}}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "rusty")
	// survival holds 25565; rusty 25566-25569, with the query port on 25567.
	put := func(body map[string]any) (int, map[string]any) {
		return e.b.do("PUT", "/api/games/rusty/ports", body)
	}

	// The main port and one variable move; the others stay with their
	// variables.
	code, out := put(map[string]any{"primary": ids[25570], "variables": map[string]int64{"QUERY_PORT": ids[25571]}})
	if code != http.StatusOK {
		t.Fatalf("change: %d %v", code, out)
	}
	ports, main, vars := e.serverPorts(t, "rusty")
	if want := []int{25568, 25569, 25570, 25571}; !slices.Equal(ports, want) {
		t.Errorf("ports %v, want %v", ports, want)
	}
	if main != 25570 || vars["QUERY_PORT"] != "25571" || vars["RCON_PORT"] != "25568" || vars["APP_PORT"] != "25569" {
		t.Errorf("main %d, variables %v", main, vars)
	}
	if got := out["ports"].([]any); len(got) != 4 {
		t.Errorf("answer ports %v", got)
	}
	// The ones given up are free again.
	for _, p := range []int{25566, 25567} {
		list, _ := e.s.Store.Allocations(context.Background(), store.ThisNode)
		for _, a := range list {
			if a.Port == p && a.AppID != "" {
				t.Errorf("port %d is still held by %s", p, a.AppID)
			}
		}
	}

	// Swapping two roles is one change.
	if code, out = put(map[string]any{"primary": ids[25570], "variables": map[string]int64{"QUERY_PORT": ids[25567], "RCON_PORT": ids[25571]}}); code != http.StatusOK {
		t.Fatalf("swap: %d %v", code, out)
	}
	if _, _, vars = e.serverPorts(t, "rusty"); vars["QUERY_PORT"] != "25567" || vars["RCON_PORT"] != "25571" || vars["APP_PORT"] != "25569" {
		t.Errorf("after the swap %v", vars)
	}

	// Extra ports are added and dropped.
	if code, out = put(map[string]any{"extra": []int64{ids[25572], ids[25573]}}); code != http.StatusOK {
		t.Fatalf("extra: %d %v", code, out)
	}
	if ports, main, _ = e.serverPorts(t, "rusty"); len(ports) != 6 || main != 25570 {
		t.Errorf("with extras: %v, main %d", ports, main)
	}
	if code, out = put(map[string]any{"extra": []int64{ids[25573]}}); code != http.StatusOK {
		t.Fatalf("drop: %d %v", code, out)
	}
	if ports, _, _ = e.serverPorts(t, "rusty"); !slices.Equal(ports, []int{25567, 25569, 25570, 25571, 25573}) {
		t.Errorf("after dropping one: %v", ports)
	}

	for name, body := range map[string]map[string]any{
		"held by another": {"primary": ids[25565]},
		"not in the pool": {"primary": 9999},
		"twice":           {"primary": ids[25570], "extra": []int64{ids[25570]}},
		"not a role":      {"variables": map[string]int64{"RCON_PASS": ids[25574]}},
	} {
		wantCode := map[string]int{"held by another": 409, "not in the pool": 404, "twice": 400, "not a role": 400}[name]
		wantErr := map[string]string{"held by another": "allocation.not_free", "not in the pool": "allocation.not_found", "twice": "allocation.twice", "not a role": "game.no_port_role"}[name]
		before, _, beforeVars := e.serverPorts(t, "rusty")
		if code, out := put(body); code != wantCode || out["code"] != wantErr {
			t.Errorf("%s: %d %v", name, code, out)
		}
		if after, _, afterVars := e.serverPorts(t, "rusty"); !slices.Equal(before, after) || !maps.Equal(beforeVars, afterVars) {
			t.Errorf("%s changed the server: %v", name, after)
		}
	}

	// Not while it runs.
	if code, out := e.power(t, "rusty", "start"); code != http.StatusAccepted {
		t.Fatalf("start: %d %v", code, out)
	}
	e.settle(t, "rusty")
	if code, out := put(map[string]any{"primary": ids[25571]}); code != http.StatusConflict || out["code"] != "game.stop_for_ports" {
		t.Errorf("while running: %d %v", code, out)
	}
	if code, out := e.power(t, "rusty", "kill"); code != http.StatusAccepted && code != http.StatusOK {
		t.Fatalf("kill: %d %v", code, out)
	}
	e.waitState(t, "rusty", "stopped")

	// The next start opens the new ports.
	if code, out := put(map[string]any{"primary": ids[25574], "variables": map[string]int64{"QUERY_PORT": ids[25575]}}); code != http.StatusOK {
		t.Fatalf("after stopping: %d %v", code, out)
	}
	if code, out := e.power(t, "rusty", "start"); code != http.StatusAccepted {
		t.Fatalf("start again: %d %v", code, out)
	}
	e.settle(t, "rusty")
	var got []int
	for _, f := range e.core.forwards["rusty"] {
		if f.Proto == "tcp" {
			got = append(got, int(f.Port))
		}
	}
	slices.Sort(got)
	if want, _, _ := e.serverPorts(t, "rusty"); !slices.Equal(got, want) || !slices.Contains(got, 25574) {
		t.Errorf("forwards %v, want the server's ports %v", got, want)
	}
	// The container runs with the new main port.
	env := strings.Join(e.core.games[e.liveContainer(t, "rusty")].Env, "\n")
	if !strings.Contains(env, "SERVER_PORT=25574") || !strings.Contains(env, "QUERY_PORT=25575") {
		t.Errorf("env %s", env)
	}
}
