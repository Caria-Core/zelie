package egg

import (
	"strings"
	"testing"
)

func apply(t *testing.T, parser, content string, reps ...Replace) (string, []string) {
	t.Helper()
	out, skipped, err := ApplyFile(parser, []byte(content), reps, nil)
	if err != nil {
		t.Fatalf("%s: %v", parser, err)
	}
	return string(out), skipped
}

func TestApplyFileProperties(t *testing.T) {
	in := "#Minecraft server properties\n#Mon Sep 29 12:00:00 UTC 2026\nmotd=A Minecraft Server\nserver-ip=10.0.0.5\nserver-port=25565\nquery.port = 25565\n\nonline-mode=true\n"
	got, _ := apply(t, "properties", in,
		Replace{Key: "server-ip", Value: ""},
		Replace{Key: "server-port", Value: "{{server.build.default.port}}"},
		Replace{Key: "query.port", Value: "25570"},
		Replace{Key: "max-players", Value: "20"},
		Replace{Key: "online-mode", Value: "false", IfValue: "false"},
		Replace{Key: "motd", Value: "never", IfValue: "other"},
		Replace{Key: "level-seed", Value: "x", IfValue: "y"},
	)
	// The unexpanded placeholder shows that expand was nil above; the
	// port is checked in the expand test.
	want := "#Minecraft server properties\n#Mon Sep 29 12:00:00 UTC 2026\nmotd=A Minecraft Server\nserver-ip=\nserver-port={{server.build.default.port}}\nquery.port = 25570\n\nonline-mode=true\nmax-players=20\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	// Line endings and a missing final newline stay as they were.
	got, _ = apply(t, "properties", "a=1\r\nb=2", Replace{Key: "a", Value: "9"})
	if got != "a=9\r\nb=2" {
		t.Errorf("crlf: %q", got)
	}
	got, _ = apply(t, "properties", "", Replace{Key: "a", Value: "9"})
	if got != "a=9\n" {
		t.Errorf("empty file: %q", got)
	}
}

func TestApplyFileExpandsValues(t *testing.T) {
	expand := func(s string) string { return Expand(s, map[string]string{"NAME": "My Server"}, 25570) }
	out, _, err := ApplyFile("properties", []byte("server-port=1\n"), []Replace{
		{Key: "server-port", Value: "{{server.build.default.port}}"},
		{Key: "motd", Value: "{{env.NAME}} on {{server.build.default.port}}"},
	}, expand)
	if err != nil || string(out) != "server-port=25570\nmotd=My Server on 25570\n" {
		t.Errorf("%q, %v", out, err)
	}
	// IfValue is compared as written, not expanded.
	out, _, _ = ApplyFile("properties", []byte("a={{x}}\n"), []Replace{{Key: "a", Value: "1", IfValue: "{{x}}"}}, expand)
	if string(out) != "a=1\n" {
		t.Errorf("%q", out)
	}
}

func TestApplyFileLines(t *testing.T) {
	in := "server.hostname \"old\"\nserver.worldsize 3000\nserver.seed 1\n"
	got, _ := apply(t, "file", in,
		Replace{Key: "server.hostname ", Value: `server.hostname "My Rust"`},
		Replace{Key: "server.worldsize ", Value: "server.worldsize 4000", IfValue: "3000"},
		Replace{Key: "server.seed ", Value: "server.seed 2", IfValue: "9"},
		Replace{Key: "server.tags ", Value: `server.tags "x"`},
	)
	want := "server.hostname \"My Rust\"\nserver.worldsize 4000\nserver.seed 1\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestApplyFileINI(t *testing.T) {
	in := `; Ark settings
ServerName=old

[ServerSettings]
ServerPassword=
MaxPlayers=70

[/Script/Engine.GameSession]
MaxPlayers=70

[SessionSettings]
SessionName=x
`
	got, _ := apply(t, "ini", in,
		Replace{Key: "ServerName", Value: "new"},
		Replace{Key: "ServerSettings.ServerPassword", Value: "secret"},
		Replace{Key: "ServerSettings.RCONPort", Value: "27020"},
		Replace{Key: "/Script/Engine.GameSession.MaxPlayers", Value: "20"},
		Replace{Key: "SessionSettings.SessionName", Value: "y", IfValue: "z"},
		Replace{Key: "Network.Port", Value: "7777"},
		Replace{Key: "Top", Value: "1"},
	)
	want := `; Ark settings
ServerName=new
Top=1

[ServerSettings]
ServerPassword=secret
MaxPlayers=70
RCONPort=27020

[/Script/Engine.GameSession]
MaxPlayers=20

[SessionSettings]
SessionName=x

[Network]
Port=7777
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

const paperGlobal = `# This is the global configuration file for Paper.
_version: 29

proxies:
  velocity:
    enabled: false
    online-mode: true
    secret: ''
  bungee-cord:
    online-mode: true

chunk-loading-basic:
  autoconfig-send-distance: true
  player-max-chunk-send-rate: 100.0

messages:
  kick:
    connection-throttle: Connection throttled! Please wait before reconnecting.
`

func TestApplyFileYAML(t *testing.T) {
	got, skipped := apply(t, "yaml", paperGlobal,
		Replace{Key: "proxies.velocity.enabled", Value: "true"},
		Replace{Key: "proxies.velocity.secret", Value: "hunter2: 1"},
		Replace{Key: "proxies.velocity.online-mode", Value: "false", IfValue: "true"},
		Replace{Key: "proxies.bungee-cord.online-mode", Value: "false", IfValue: "false"},
		Replace{Key: "chunk-loading-basic.player-max-chunk-send-rate", Value: "50"},
		Replace{Key: "messages.kick.connection-throttle", Value: "Slow down"},
		Replace{Key: "unsupported-settings.allow-headless-pistons", Value: "true"},
		Replace{Key: "unsupported-settings.note", Value: "1.20"},
		Replace{Key: "_version.deeper", Value: "1"},
	)
	if len(skipped) != 1 || !strings.HasPrefix(skipped[0], "_version.deeper:") {
		t.Errorf("skipped %v", skipped)
	}
	for _, want := range []string{
		"# This is the global configuration file for Paper.",
		"_version: 29",
		"    enabled: true",
		"secret: 'hunter2: 1'",
		"online-mode: false",
		"player-max-chunk-send-rate: 50",
		"connection-throttle: Slow down",
		"unsupported-settings:",
		"allow-headless-pistons: true",
		`note: "1.20"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// The conditional replacement that did not match changed nothing, and
	// the velocity one that did, did.
	if strings.Count(got, "online-mode: true") != 1 {
		t.Errorf("online-mode:\n%s", got)
	}
	// Order is kept: new keys go last.
	if strings.Index(got, "proxies:") > strings.Index(got, "messages:") || strings.Index(got, "messages:") > strings.Index(got, "unsupported-settings:") {
		t.Errorf("order changed:\n%s", got)
	}
}

func TestApplyFileYAMLWildcardsAndLists(t *testing.T) {
	in := "servers:\n  lobby:\n    port: 25565\n  survival:\n    port: 25566\nlist:\n  - name: a\n  - name: b\n"
	got, _ := apply(t, "yaml", in,
		Replace{Key: "servers.*.motd", Value: "hi"},
		Replace{Key: "servers.*.port", Value: "1", IfValue: "25566"},
		Replace{Key: "list.1.name", Value: "c"},
		Replace{Key: "list.*.name", Value: "d", IfValue: "a"},
		Replace{Key: "nothing.*.x", Value: "1"},
	)
	// A wildcard reaches every entry, and adds a missing key to each; a
	// path that runs into nothing makes no empty section.
	if strings.Count(got, "motd: hi") != 2 || strings.Contains(got, "nothing") {
		t.Errorf("wildcard:\n%s", got)
	}
	if !strings.Contains(got, "survival:\n    port: 1") || !strings.Contains(got, "port: 25565") {
		t.Errorf("if-value on a wildcard:\n%s", got)
	}
	if !strings.Contains(got, "name: d") || !strings.Contains(got, "name: c") || strings.Contains(got, "name: a") || strings.Contains(got, "name: b") {
		t.Errorf("list:\n%s", got)
	}
}

func TestApplyFileYAMLEmptyAndBroken(t *testing.T) {
	got, _ := apply(t, "yaml", "", Replace{Key: "a.b", Value: "1"})
	if got != "a:\n  b: 1\n" {
		t.Errorf("empty file: %q", got)
	}
	if _, _, err := ApplyFile("yaml", []byte("a: [1, 2\nb: :"), []Replace{{Key: "a", Value: "1"}}, nil); err == nil {
		t.Error("broken YAML was accepted")
	}
}

func TestApplyFileJSON(t *testing.T) {
	in := `{
    "Name": "Server",
    "Port": 27015,
    "Public": false,
    "Rcon": {
        "Enabled": false,
        "Password": ""
    },
    "Maps": ["a", "b"],
    "Owners": [{"Id": 1}, {"Id": 2}]
}
`
	got, skipped := apply(t, "json", in,
		Replace{Key: "Name", Value: "My <Server>"},
		Replace{Key: "Port", Value: "27016"},
		Replace{Key: "Public", Value: "true"},
		Replace{Key: "Rcon.Password", Value: "12345"},
		Replace{Key: "Rcon.Enabled", Value: "true", IfValue: "true"},
		Replace{Key: "Rcon.Port", Value: "28016"},
		Replace{Key: "Query.Enabled", Value: "true"},
		Replace{Key: "Owners.*.Id", Value: "9", IfValue: "2"},
		Replace{Key: "Maps.1", Value: "c"},
		Replace{Key: "Name.x", Value: "1"},
	)
	if len(skipped) != 1 {
		t.Errorf("skipped %v", skipped)
	}
	want := `{
    "Name": "My <Server>",
    "Port": 27016,
    "Public": true,
    "Rcon": {
        "Enabled": false,
        "Password": "12345",
        "Port": 28016
    },
    "Maps": [
        "a",
        "c"
    ],
    "Owners": [
        {
            "Id": 1
        },
        {
            "Id": 9
        }
    ],
    "Query": {
        "Enabled": true
    }
}
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	// A number the file holds stays a number and a text stays text.
	got, _ = apply(t, "json", `{"a": "1", "b": 1}`, Replace{Key: "a", Value: "2"}, Replace{Key: "b", Value: "two"})
	if !strings.Contains(got, `"a": "2"`) || !strings.Contains(got, `"b": "two"`) {
		t.Errorf("types: %s", got)
	}
	if _, _, err := ApplyFile("json", []byte(`{"a": `), []Replace{{Key: "a", Value: "1"}}, nil); err == nil {
		t.Error("broken JSON was accepted")
	}
	got, _ = apply(t, "json", "", Replace{Key: "a.b", Value: "x"})
	if got != "{\n  \"a\": {\n    \"b\": \"x\"\n  }\n}\n" {
		t.Errorf("empty: %q", got)
	}
}

func TestApplyFileXML(t *testing.T) {
	in := `<?xml version="1.0" encoding="utf-8"?>
<!-- Server settings -->
<ServerSettings>
  <Server>
    <Name>Old</Name>
    <Port>27015</Port>
    <Password/>
    <Tags><Tag>a</Tag></Tags>
  </Server>
  <Debug>false</Debug>
</ServerSettings>
`
	got, _ := apply(t, "xml", in,
		Replace{Key: "ServerSettings.Server.Name", Value: "A & B"},
		Replace{Key: "Server.Port", Value: "27016", IfValue: "27015"},
		Replace{Key: "Server.Password", Value: "s3"},
		Replace{Key: "Debug", Value: "true", IfValue: "true"},
		Replace{Key: "Server.Missing", Value: "1"},
		Replace{Key: "Server.Tags", Value: "no"},
	)
	want := `<?xml version="1.0" encoding="utf-8"?>
<!-- Server settings -->
<ServerSettings>
  <Server>
    <Name>A &amp; B</Name>
    <Port>27016</Port>
    <Password>s3</Password>
    <Tags><Tag>a</Tag></Tags>
  </Server>
  <Debug>false</Debug>
</ServerSettings>
`
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if _, _, err := ApplyFile("xml", []byte("<a><b></a>"), []Replace{{Key: "a.b", Value: "1"}}, nil); err == nil {
		t.Error("broken XML was accepted")
	}
}

func TestApplyFileUnknownParser(t *testing.T) {
	if _, _, err := ApplyFile("toml", []byte("a=1"), []Replace{{Key: "a", Value: "2"}}, nil); err == nil {
		t.Error("an unknown parser was accepted")
	}
}

func TestDoneMatcher(t *testing.T) {
	e := &Egg{Done: []string{`)! For help, type `, `regex:^\[\d+:\d+\] Server started$`, `regex:(`, ""}}
	d := e.DoneMatcher()
	for line, want := range map[string]bool{
		`[12:00:01 INFO]: Done (3.2s)! For help, type "help"`: true,
		"[12:00] Server started":                              true,
		"[12:00] Server started later":                        false,
		"loading world":                                       false,
		"":                                                    false,
	} {
		if got := d.Match(line); got != want {
			t.Errorf("%q: %v", line, got)
		}
	}
	if d.Empty() || !(&Egg{}).DoneMatcher().Empty() {
		t.Error("Empty")
	}
}
