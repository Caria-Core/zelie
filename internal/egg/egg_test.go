package egg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
)

func load(t *testing.T, name string) *Egg {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	e, err := Parse(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return e
}

func findVar(t *testing.T, e *Egg, env string) Variable {
	t.Helper()
	for _, v := range e.Variables {
		if v.Env == env {
			return v
		}
	}
	t.Fatalf("no variable %s", env)
	return Variable{}
}

func TestPaperPTDLv2(t *testing.T) {
	e := load(t, "paper.json")
	if e.Version != "PTDL_v2" || e.Name != "Paper" {
		t.Errorf("version %q name %q", e.Version, e.Name)
	}
	if len(e.Images) != 5 || e.Images[0].Label != "Java 21" || e.Images[0].Ref != "ghcr.io/pelican-eggs/yolks:java_21" {
		t.Errorf("images %+v", e.Images)
	}
	if !strings.Contains(e.Startup, "-jar {{SERVER_JARFILE}}") {
		t.Errorf("startup %q", e.Startup)
	}
	if e.Stop != "stop" {
		t.Errorf("stop %q", e.Stop)
	}
	if _, ok := e.StopSignal(); ok {
		t.Error("stop command read as a signal")
	}
	if !slices.Equal(e.Done, []string{")! For help, type "}) {
		t.Errorf("done %q", e.Done)
	}
	if len(e.Files) != 1 || e.Files[0].Path != "server.properties" || e.Files[0].Parser != "properties" {
		t.Fatalf("files %+v", e.Files)
	}
	if f := e.Files[0].Find; len(f) != 3 || f[1] != (Replace{Key: "server-port", Value: "{{server.build.default.port}}"}) {
		t.Errorf("find %+v", f)
	}
	if e.Install.Image != "ghcr.io/pelican-eggs/installers:alpine" || e.Install.Entrypoint != "ash" {
		t.Errorf("install %+v", e.Install)
	}
	if !strings.HasPrefix(e.Install.Script, "#!/bin/ash\n") || strings.Contains(e.Install.Script, "\r") {
		t.Errorf("script starts %q", e.Install.Script[:20])
	}
	b := findVar(t, e, "BUILD_NUMBER")
	if !slices.Equal(b.Rules, []string{"required", "string", "max:20"}) || b.Default != "latest" || !b.UserEditable {
		t.Errorf("BUILD_NUMBER %+v", b)
	}
	if findVar(t, e, "DL_PATH").UserViewable {
		t.Error("DL_PATH should be hidden")
	}
}

func TestRustPTDLv2(t *testing.T) {
	e := load(t, "rust.json")
	if e.Name != "Rust" || len(e.Images) != 1 {
		t.Errorf("name %q images %+v", e.Name, e.Images)
	}
	if !strings.HasPrefix(e.Startup, "./RustDedicated") {
		t.Errorf("startup %q", e.Startup)
	}
	if e.Stop != "quit" || !slices.Equal(e.Done, []string{"Server startup complete"}) {
		t.Errorf("stop %q done %q", e.Stop, e.Done)
	}
	if len(e.Files) != 1 || e.Files[0].Parser != "file" || e.Files[0].Path != "server/rust/cfg/server.cfg" {
		t.Fatalf("files %+v", e.Files)
	}
	if f := e.Files[0].Find[0]; f.Key != "server.hostname " || f.Value != `server.hostname "{{server.build.env.SERVER_HOSTNAME}}"` {
		t.Errorf("find %+v", f)
	}
	if e.Install.Image == "" || e.Install.Script == "" {
		t.Errorf("install %+v", e.Install)
	}
	fw := findVar(t, e, "FRAMEWORK")
	if err := fw.Check("oxide"); err != nil {
		t.Error(err)
	}
	if err := fw.Check("bepinex"); err == nil {
		t.Error("value outside in: accepted")
	}
}

func TestPalworldPLCNv3(t *testing.T) {
	e := load(t, "palworld.yaml")
	if e.Version != "PLCN_v3" || e.Name != "Palworld" {
		t.Errorf("version %q name %q", e.Version, e.Name)
	}
	if len(e.Images) != 1 || e.Images[0].Label != "SteamCMD_Debian" || e.Images[0].Ref != "ghcr.io/parkervcp/steamcmd:debian" {
		t.Errorf("images %+v", e.Images)
	}
	if len(e.StartupCommands) != 1 || e.StartupCommands[0].Label != "Default" || !strings.Contains(e.Startup, "PalServer-Linux-Shipping") {
		t.Errorf("startup %+v", e.StartupCommands)
	}
	if e.Stop != "shutdown 15" || len(e.Done) != 1 || len(e.Files) != 0 {
		t.Errorf("stop %q done %q files %v", e.Stop, e.Done, e.Files)
	}
	if e.Install.Entrypoint != "bash" || e.Install.Image != "ghcr.io/parkervcp/installers:debian" || e.Install.Script == "" {
		t.Errorf("install %+v", e.Install)
	}
	// Numbers are unquoted in the YAML but always come out as strings.
	if v := findVar(t, e, "SRCDS_APPID"); v.Default != "2394010" || v.UserViewable || v.UserEditable {
		t.Errorf("SRCDS_APPID %+v", v)
	}
	if v := findVar(t, e, "MAX_PLAYERS"); v.Default != "32" || !slices.Equal(v.Rules, []string{"required", "numeric", "between:1,32"}) {
		t.Errorf("MAX_PLAYERS %+v", v)
	}
}

func TestPTDLv1(t *testing.T) {
	e, err := Parse([]byte(`{
		"meta": {"version": "PTDL_v1"},
		"name": "Old",
		"image": "quay.io/pterodactyl/core:java",
		"startup": "java -jar server.jar",
		"config": {"files": "{}", "startup": "{\"done\": [\"Done\", \"Ready\"]}", "logs": "{}", "stop": "^C"},
		"scripts": {"installation": {"script": "a\r\nb", "container": "alpine", "entrypoint": "ash"}},
		"variables": [{"name": "Jar", "env_variable": "JAR", "default_value": "server.jar", "user_viewable": 1, "user_editable": 0, "rules": "required|string"}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Images) != 1 || e.Images[0].Ref != "quay.io/pterodactyl/core:java" {
		t.Errorf("images %+v", e.Images)
	}
	if !slices.Equal(e.Done, []string{"Done", "Ready"}) {
		t.Errorf("done %q", e.Done)
	}
	if sig, ok := e.StopSignal(); !ok || sig != "SIGINT" {
		t.Errorf("signal %q %v", sig, ok)
	}
	if e.Install.Script != "a\nb" {
		t.Errorf("script %q", e.Install.Script)
	}
	if v := e.Variables[0]; !v.UserViewable || v.UserEditable {
		t.Errorf("flags %+v", v)
	}

	e, err = Parse([]byte(`{"meta":{"version":"PTDL_v1"},"name":"Two","images":["a:1","b:2"],"startup":"run"}`))
	if err != nil || len(e.Images) != 2 || e.Images[1].Ref != "b:2" {
		t.Errorf("images list: %+v, %v", e, err)
	}
}

func TestPLCNv1v2(t *testing.T) {
	for _, v := range []string{"PLCN_v1", "PLCN_v2"} {
		e, err := Parse([]byte(`{
			"meta": {"version": "` + v + `"},
			"name": "Mini",
			"docker_images": {"B": "img:b", "A": "img:a"},
			"startup": "./run {{PORT}}",
			"config": {"files": "", "startup": "{}", "logs": "{}", "stop": "^SIGTERM"},
			"scripts": {"installation": {"script": "", "container": "alpine", "entrypoint": "ash"}},
			"variables": [{"name": "Port", "env_variable": "PORT", "default_value": "25565", "user_viewable": true, "user_editable": true, "rules": "required|integer|between:1024,65535"}]
		}`))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		// Source order, not alphabetical: B is the default image.
		if e.Images[0].Label != "B" {
			t.Errorf("%s images %+v", v, e.Images)
		}
		if sig, ok := e.StopSignal(); !ok || sig != "SIGTERM" {
			t.Errorf("%s signal %q", v, sig)
		}
		if len(e.Files) != 0 || len(e.Done) != 0 {
			t.Errorf("%s files %v done %v", v, e.Files, e.Done)
		}
	}
}

func TestPLCNv3Shapes(t *testing.T) {
	e, err := Parse([]byte(`
meta:
  version: PLCN_v3
name: Shapes
docker_images:
  Second: 'img:2'
  First: 'img:1'
startup_commands:
  Zeta: 'run zeta'
  Alpha: 'run alpha'
config:
  files:
    app.yml:
      parser: yaml
      find:
        port: 8080
        debug: false
        mode:
          old: new
          other: newer
  startup:
    done:
      - ready
      - listening
  stop: '^C'
variables:
  - name: Flag
    env_variable: FLAG
    default_value: true
    rules:
      - required
      - boolean
`))
	if err != nil {
		t.Fatal(err)
	}
	if e.Images[0].Label != "Second" || e.Startup != "run zeta" || len(e.StartupCommands) != 2 || e.StartupCommands[1].Label != "Alpha" {
		t.Errorf("order: images %+v startups %+v", e.Images, e.StartupCommands)
	}
	if !slices.Equal(e.Done, []string{"ready", "listening"}) {
		t.Errorf("done %q", e.Done)
	}
	want := []Replace{
		{Key: "port", Value: "8080"},
		{Key: "debug", Value: "false"},
		{Key: "mode", Value: "new", IfValue: "old"},
		{Key: "mode", Value: "newer", IfValue: "other"},
	}
	if got := e.Files[0].Find; !slices.Equal(got, want) {
		t.Errorf("find %+v", got)
	}
	if v := e.Variables[0]; v.Default != "true" || !slices.Equal(v.Rules, []string{"required", "boolean"}) {
		t.Errorf("variable %+v", v)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"unknown version", `{"meta":{"version":"PTDL_v9"},"name":"x"}`, "unsupported egg version"},
		{"no version", `{"name":"x"}`, "meta.version"},
		{"no startup", `{"meta":{"version":"PTDL_v2"},"name":"x","docker_images":{"a":"b"}}`, "startup"},
		{"no image", `{"meta":{"version":"PTDL_v2"},"name":"x","startup":"run"}`, "image"},
		{"no name", `{"meta":{"version":"PTDL_v2"},"docker_images":{"a":"b"},"startup":"run"}`, "name"},
		{"variable without env", `{"meta":{"version":"PTDL_v2"},"name":"x","docker_images":{"a":"b"},"startup":"r","variables":[{"name":"V"}]}`, "env_variable"},
		{"bad embedded config", `{"meta":{"version":"PTDL_v2"},"name":"x","docker_images":{"a":"b"},"startup":"r","config":{"files":"{nope"}}`, "config.files"},
		{"broken json", `{"meta":`, "read egg"},
		{"trailing json", `{"meta":{"version":"PTDL_v2"}} {}`, "read egg"},
		{"plain text", "this is not an egg", "not an object"},
		{"yaml list", "- a\n- b\n", "not an object"},
		{"empty", "", "read egg"},
		{"yaml alias", "meta: &m {version: PLCN_v3}\nname: *m\n", "aliases"},
	} {
		_, err := Parse([]byte(c.in))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.want)
		}
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"NAME": "World", "SERVER_MEMORY": "2048", "SERVER_JARFILE": "server.jar"}
	for _, c := range []struct {
		in   string
		port int
		want string
	}{
		{"hello {{NAME}}", 25565, "hello World"},
		{"{{ NAME }}", 25565, "World"},
		{"{{env.NAME}} {{server.build.env.NAME}} {{server.environment.NAME}}", 0, "World World World"},
		{"-Xmx{{SERVER_MEMORY}}M", 25565, "-Xmx2048M"},
		{"{{server.build.memory}} {{server.build.memory_limit}}", 0, "2048 2048"},
		{"{{server.build.default.port}} {{server.allocations.default.port}}", 25565, "25565 25565"},
		{"{{server.build.default.ip}}:{{SERVER_PORT}}", 7777, "0.0.0.0:7777"},
		{"{{server.build.env.SERVER_PORT}}", 7777, "7777"},
		{"{{server.build.default.port}}", 0, "{{server.build.default.port}}"},
		{"{{MISSING}} {{server.unknown.thing}} {{server.build.env.a.b}}", 1, "{{MISSING}} {{server.unknown.thing}} {{server.build.env.a.b}}"},
		{"${SHELL_VAR} {NAME} {{}}", 1, "${SHELL_VAR} {NAME} {{}}"},
		{"java -jar {{SERVER_JARFILE}}", 1, "java -jar server.jar"},
	} {
		if got := Expand(c.in, vars, c.port); got != c.want {
			t.Errorf("Expand(%q, port %d) = %q, want %q", c.in, c.port, got, c.want)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, c := range []struct {
		rules string
		value string
		ok    bool
	}{
		{"required|string|max:20", "latest", true},
		{"required|string|max:20", "", false},
		{"required|string|max:20", "   ", false},
		{"required|string|max:5", "toolong", false},
		{"required|string|max:5", "héllo", true},
		{"nullable|string|max:5", "", true},
		{"string|min:3", "ab", false},
		{"string|min:3", "abc", true},
		{"string|min:3", "", true},
		{"required|integer", "42", true},
		{"required|integer", "4.2", false},
		{"required|integer", "abc", false},
		{"integer|min:1|max:100", "100", true},
		{"integer|min:1|max:100", "101", false},
		{"integer|min:1|max:100", "0", false},
		{"numeric|max:10", "9.5", true},
		{"numeric", "0x10", false},
		{"numeric", "NaN", false},
		{"integer|between:1024,65535", "80", false},
		{"integer|between:1024,65535", "25565", true},
		{"string|between:2,4", "abcde", false},
		{"string|between:2,4", "abc", true},
		{"boolean", "1", true},
		{"boolean", "false", true},
		{"boolean", "yes", false},
		{"required|in:vanilla,oxide,carbon", "oxide", true},
		{"required|in:vanilla,oxide,carbon", "spigot", false},
		{`in:"a b",c`, "a b", true},
		{"alpha_dash", "my-server_1", true},
		{"alpha_dash", "my server", false},
		{"url", "https://example.com/a.jar", true},
		{"url", "not a url", false},
		{"url", "example.com", false},
		{"regex:/^[a-z]+$/", "abc", true},
		{"regex:/^[a-z]+$/", "abc1", false},
		{"regex:/^[a-z]+$/i", "ABC", true},
		{`regex:/^a\/b$/`, "a/b", true},
		{"regex:#^\\d+$#", "12", true},
		{"regex:#^\\d+$#", "1x", false},
		// Lookahead does not compile in RE2, so the rule is skipped.
		{"regex:/^(?=.*\\d).+$/", "nodigits", true},
		{"regex:/^x$/x", "anything", true},
		{"required|somethingnew:1,2", "x", true},
		{"max:abc", "x", true},
		{"", "anything", true},
	} {
		v := Variable{Name: "Var", Rules: rules(c.rules)}
		err := v.Check(c.value)
		if (err == nil) != c.ok {
			t.Errorf("rules %q value %q: err %v, want ok=%v", c.rules, c.value, err, c.ok)
		}
	}
}

func TestStopSignal(t *testing.T) {
	for stop, want := range map[string]string{
		"^C": "SIGINT", "^SIGKILL": "SIGKILL", "^term": "SIGTERM", "stop": "", "^": "", "": "",
	} {
		got, ok := (&Egg{Stop: stop}).StopSignal()
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v", stop, got, ok)
		}
	}
}

func TestCatalogIsValid(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Catalog {
		if seen[e.ID] {
			t.Errorf("duplicate id %s", e.ID)
		}
		seen[e.ID] = true
		if len(e.Commit) != 40 || e.Path == "" || !strings.Contains(e.Repo, "/") || e.Name == "" || e.Game == "" {
			t.Errorf("incomplete entry %+v", e)
		}
	}
}

func serve(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(h)
	old := RawBase
	RawBase = srv.URL
	t.Cleanup(func() { RawBase = old; srv.Close() })
}

func TestFetch(t *testing.T) {
	body, err := os.ReadFile("testdata/rust.json")
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{Repo: "org/repo", Commit: strings.Repeat("a", 40), Path: "rust/egg.json"}
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/org/repo/"+entry.Commit+"/rust/egg.json" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	})
	e, raw, err := Fetch(context.Background(), nil, entry)
	if err != nil {
		t.Fatal(err)
	}
	if e.Name != "Rust" || string(raw) != string(body) {
		t.Errorf("name %q, raw match %v", e.Name, string(raw) == string(body))
	}

	entry.Path = "missing.json"
	if _, _, err := Fetch(context.Background(), nil, entry); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing file: %v", err)
	}
}

func TestFetchTooLargeAndInvalid(t *testing.T) {
	serve(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "big") {
			w.Write([]byte(`{"pad":"` + strings.Repeat("x", maxSize) + `"}`))
			return
		}
		w.Write([]byte("not an egg"))
	})
	_, _, err := Fetch(context.Background(), nil, Entry{Repo: "o/r", Commit: "c", Path: "big"})
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("too large: %v", err)
	}
	_, _, err = Fetch(context.Background(), nil, Entry{Repo: "o/r", Commit: "c", Path: "text"})
	if err == nil || !strings.Contains(err.Error(), "not an object") {
		t.Errorf("invalid: %v", err)
	}
}

func TestFetchURL(t *testing.T) {
	body, err := os.ReadFile("testdata/paper.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	e, _, err := FetchURL(context.Background(), srv.Client(), srv.URL+"/egg.json")
	if err != nil || e.Name != "Paper" {
		t.Fatalf("%v %v", e, err)
	}

	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	defer plain.Close()
	for _, u := range []string{plain.URL + "/egg.json", "ftp://example.com/egg.json", "example.com/egg.json", "https:///egg.json"} {
		if _, _, err := FetchURL(context.Background(), nil, u); err == nil {
			t.Errorf("%s accepted", u)
		}
	}

	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, plain.URL, http.StatusFound)
	}))
	defer redirect.Close()
	if _, _, err := FetchURL(context.Background(), redirect.Client(), redirect.URL); err == nil {
		t.Error("redirect to plain http followed")
	}
}

func TestFetchURLRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached a local server")
	}))
	defer srv.Close()
	for _, u := range []string{srv.URL, "https://10.0.0.1/egg.json", "https://[::1]/egg.json"} {
		_, _, err := FetchURL(context.Background(), nil, u)
		if err == nil || !strings.Contains(err.Error(), "not a public address") {
			t.Errorf("%s: err = %v", u, err)
		}
	}
}
