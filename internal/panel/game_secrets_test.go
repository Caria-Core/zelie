package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/egg"
)

func TestSecretKindOf(t *testing.T) {
	for env, want := range map[string]secretKind{
		"RCON_PASS":          adminSecret,
		"RCON_PASSWORD":      adminSecret,
		"VR_RCON_PASSWORD":   adminSecret,
		"ARK_ADMIN_PASSWORD": adminSecret,
		"ADMIN_PASSWORD":     adminSecret,
		"ADMINPASS":          adminSecret,
		"DB_PASSWORD":        adminSecret,
		"API_TOKEN":          adminSecret,
		"APP_SECRET":         adminSecret,
		"ROOT_PASSWD":        adminSecret,
		"SERVER_PASSWORD":    joinSecret,
		"JOIN_PASSWORD":      joinSecret,
		"GAME_PASS":          joinSecret,
		"PASSWORD":           joinSecret,
		"SRV_PW":             joinSecret,
		"SRCDS_BETAPASS":     joinSecret,
		"STEAM_PASS":         notSecret,
		"STEAM_AUTH_TOKEN":   notSecret,
		"DISCORD_TOKEN":      notSecret,
		"RCON_PORT":          notSecret,
		"RCON_ENABLED":       notSecret,
		"ADMIN_USER":         notSecret,
		"STEAM_GSLT":         notSecret,
		"SERVER_NAME":        notSecret,
		"SRCDS_APPID":        notSecret,
	} {
		if got := secretKindOf(env); got != want {
			t.Errorf("%s: kind %d, want %d", env, got, want)
		}
	}
}

func TestSecretValue(t *testing.T) {
	plain := func(env string, rules ...string) egg.Variable { return egg.Variable{Env: env, Rules: rules} }
	tests := []struct {
		name     string
		v        egg.Variable
		value    string
		generate bool
	}{
		{"empty admin", plain("RCON_PASS"), "", true},
		{"blank admin", plain("RCON_PASS"), "  ", true},
		{"placeholder", plain("ADMIN_PASSWORD"), "CHANGEME", true},
		{"placeholder with separator", plain("ADMIN_PASSWORD"), "change_me", true},
		{"placeholder with case", plain("RCON_PASSWORD"), "Password", true},
		{"placeholder word", plain("VR_RCON_PASSWORD"), "somepassword", true},
		{"placeholder number", plain("RCON_PASS"), "12345", true},
		{"chosen value", plain("RCON_PASS"), "hunter2hunter2", false},
		{"join password", plain("SERVER_PASSWORD", "nullable"), "", false},
		{"join placeholder", plain("PASSWORD", "required"), "secret", true},
		{"join placeholder with separator", plain("SERVER_PASSWORD", "nullable"), "Change-Me", true},
		{"join chosen value", plain("SERVER_PASSWORD", "nullable"), "letmeplay", false},
		{"join required and empty", plain("SERVER_PASSWORD", "required"), "", true},
		{"join optional and empty", plain("SERVER_PASSWORD", "nullable"), "", false},
		{"other service", plain("STEAM_PASS", "required"), "", false},
		{"not a secret", plain("SERVER_NAME"), "", false},
		{"number", plain("RCON_PASS", "required", "integer"), "", false},
		{"too short to be strong", plain("RCON_PASS", "max:6"), "", false},
		{"choice", plain("RCON_PASS", "in:a,b"), "", false},
	}
	for _, tc := range tests {
		got := secretValue(tc.v, tc.value)
		if tc.generate {
			if got == tc.value || got == "" {
				t.Errorf("%s: kept %q", tc.name, got)
			}
			if err := tc.v.Check(got); err != nil {
				t.Errorf("%s: value %q breaks the rules: %v", tc.name, got, err)
			}
		} else if got != tc.value {
			t.Errorf("%s: changed %q to %q", tc.name, tc.value, got)
		}
	}
}

func TestJoinPasswords(t *testing.T) {
	// The alphabet has none of the characters that look alike.
	if strings.ContainsAny(joinAlphabet, "0Oo1lIi") {
		t.Errorf("join alphabet %q has look-alikes", joinAlphabet)
	}
	for _, tc := range []struct {
		name  string
		rules []string
		n     int
	}{
		{"none", nil, 10},
		// Valheim's rules.
		{"valheim", []string{"required", "string", "min:5", "max:20"}, 10},
		{"shorter cap", []string{"required", "max:6"}, 6},
		{"longer minimum", []string{"required", "min:14"}, 14},
	} {
		v := egg.Variable{Env: "PASSWORD", Rules: tc.rules}
		got := secretValue(v, "secret")
		if len(got) != tc.n || strings.Trim(got, joinAlphabet) != "" {
			t.Errorf("%s: got %q, want %d characters of the join alphabet", tc.name, got, tc.n)
		}
		if err := v.Check(got); err != nil {
			t.Errorf("%s: %q breaks the rules: %v", tc.name, got, err)
		}
		if again := secretValue(v, "secret"); again == got {
			t.Errorf("%s: two servers got %q", tc.name, got)
		}
	}
	// An empty password stays empty for a public server, and a value the
	// egg sets to something real stays.
	v := egg.Variable{Env: "SERVER_PASSWORD", Rules: []string{"nullable", "string"}}
	if got := secretValue(v, ""); got != "" {
		t.Errorf("empty join password became %q", got)
	}
	// Too little room for a value worth typing: the egg's own stays.
	if got := secretValue(egg.Variable{Env: "PASSWORD", Rules: []string{"required", "max:4"}}, "secret"); got != "secret" {
		t.Errorf("max:4 gave %q", got)
	}
	// What the request sends is kept.
	e := &egg.Egg{Variables: []egg.Variable{{Env: "PASSWORD", Default: "secret", Rules: []string{"required", "string", "min:5", "max:20"}, UserEditable: true}}}
	vars, bad := gameVariables(e, nil, map[string]string{"PASSWORD": "secret"}, false)
	if bad != nil || vars["PASSWORD"] != "secret" {
		t.Errorf("sent value: %q, %v", vars["PASSWORD"], bad)
	}
	if vars, bad = gameVariables(e, nil, nil, false); bad != nil || vars["PASSWORD"] == "secret" || len(vars["PASSWORD"]) != 10 {
		t.Errorf("default: %q, %v", vars["PASSWORD"], bad)
	}
}

func TestSecretValueFitsRules(t *testing.T) {
	alnum := regexp.MustCompile(`^[A-Za-z0-9]+$`)
	tests := []struct {
		rules []string
		n     int
	}{
		{nil, 24},
		{[]string{"required", "alpha_dash", "max:128"}, 24},
		{[]string{"required", "alpha_dash", "between:1,30"}, 24},
		{[]string{"required", "max:16"}, 16},
		{[]string{"required", "max:8"}, 8},
		{[]string{"required", "min:30"}, 30},
		{[]string{"required", "size:12"}, 12},
		{[]string{"required", `regex:/^[\w.-]*$/`, "max:64"}, 24},
	}
	for _, tc := range tests {
		v := egg.Variable{Env: "RCON_PASS", Rules: tc.rules}
		a, b := secretValue(v, ""), secretValue(v, "")
		if len(a) != tc.n || !alnum.MatchString(a) {
			t.Errorf("%v: got %q, want %d letters and digits", tc.rules, a, tc.n)
		}
		if a == b {
			t.Errorf("%v: two values are the same", tc.rules)
		}
		if err := v.Check(a); err != nil {
			t.Errorf("%v: %v", tc.rules, err)
		}
	}
}

func TestSecretValueRust(t *testing.T) {
	data, err := os.ReadFile("../egg/testdata/rust-plcn.yaml")
	if err != nil {
		t.Fatal(err)
	}
	e, err := egg.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	vars, bad := gameVariables(e, nil, nil, false)
	if bad != nil {
		t.Fatal(bad)
	}
	pass := vars["RCON_PASS"]
	if len(pass) != 24 || strings.Trim(pass, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") != "" {
		t.Errorf("RCON_PASS = %q, want 24 letters and digits", pass)
	}
	if vars["SERVER_HOSTNAME"] != "A Rust Server" {
		t.Errorf("other variables changed: %q", vars["SERVER_HOSTNAME"])
	}
	// What the request sets is kept, and so is what a server already has.
	vars, bad = gameVariables(e, nil, map[string]string{"RCON_PASS": "mine"}, false)
	if bad != nil || vars["RCON_PASS"] != "mine" {
		t.Errorf("given RCON_PASS = %q, %v", vars["RCON_PASS"], bad)
	}
	vars, bad = gameVariables(e, map[string]string{"RCON_PASS": ""}, nil, false)
	if bad != nil || vars["RCON_PASS"] != "" {
		t.Errorf("a value the server already has was replaced: %q, %v", vars["RCON_PASS"], bad)
	}
}
