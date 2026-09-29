package panel

import (
	"context"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/egg"
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
	// The ports it locks cannot be chosen; the server sets them.
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "locked", "egg": "rust", "variables": map[string]string{"RCON_PASS": "x", "QUERY_PORT": "1"}}); code != http.StatusBadRequest || out["code"] != "game.variable_locked" {
		t.Errorf("locked port: %d %v", code, out)
	}
}
