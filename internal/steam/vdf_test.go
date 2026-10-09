package steam

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The output has the shape SteamCMD's app_info_print prints, written by
// hand from Valve's documented format: log lines, then one block per app,
// with Windows line endings as SteamCMD prints them.
func TestLatestBuilds(t *testing.T) {
	got, err := LatestBuilds(fixture(t, "app_info.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Every branch of the app, not the depot's manifests that are also
	// called public; an app with no branch at all would be left out.
	want := map[int64]map[string]string{
		258550:  {"public": "20913457", "aux01": "20990001", "legacy": "11111111"},
		2394010: {"public": "20871234"},
		4000000: {"beta": "5"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("builds %v, want %v", got, want)
	}
}

func TestParseAppInfoKeepsQuotesAndNewlines(t *testing.T) {
	apps, err := ParseAppInfo(fixture(t, "app_info.txt"))
	if err != nil {
		t.Fatal(err)
	}
	desc, ok := apps[258550].Text("common", "description")
	if !ok || desc != "A \"dedicated\" server; not a game.\r\nSecond line." {
		t.Errorf("description %q", desc)
	}
	if len(apps) != 3 {
		t.Errorf("found %d apps, want 3", len(apps))
	}
}

func TestParseAppInfoWithoutApps(t *testing.T) {
	if _, err := ParseAppInfo([]byte("Connecting anonymously to Steam Public...FAILED\n")); err == nil {
		t.Error("output with no app was accepted")
	}
	// A block cut off by the container being killed is not an app.
	if _, err := ParseAppInfo([]byte("\"1\"\n{\n\t\"depots\"\n\t{\n")); err == nil {
		t.Error("a cut-off block was accepted")
	}
}

func TestParseManifest(t *testing.T) {
	got, err := ParseManifest(fixture(t, "appmanifest_258550.acf"))
	if want := (Manifest{Build: "20913457", Branch: "public"}); err != nil || got != want {
		t.Errorf("manifest %+v, %v, want %+v", got, err, want)
	}
	for _, bad := range []string{"", `"AppState" { "appid" "1" }`, `"AppState" { "buildid" "abc" }`, `"AppState" {`} {
		if _, err := ParseManifest([]byte(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// Games installed from a beta say so in the manifest, and no key at all
// means the public branch.
func TestManifestBranch(t *testing.T) {
	for name, c := range map[string]struct{ text, branch string }{
		"none":             {`"AppState" { "buildid" "7" }`, "public"},
		"empty":            {`"AppState" { "buildid" "7" "UserConfig" { "BetaKey" "" } }`, "public"},
		"user config":      {`"AppState" { "buildid" "7" "UserConfig" { "BetaKey" "Aux01" } }`, "aux01"},
		"mounted config":   {`"AppState" { "buildid" "7" "MountedConfig" { "BetaKey" "staging" } }`, "staging"},
		"mounted wins":     {`"AppState" { "buildid" "7" "UserConfig" { "BetaKey" "public" } "MountedConfig" { "BetaKey" "staging" } }`, "staging"},
		"mounted is empty": {`"AppState" { "buildid" "7" "UserConfig" { "BetaKey" "staging" } "MountedConfig" { "BetaKey" "" } }`, "staging"},
		"explicit public":  {`"AppState" { "buildid" "7" "UserConfig" { "BetaKey" "public" } }`, "public"},
		"byte order mark":  {"\xef\xbb\xbf" + `"AppState" { "buildid" "7" "UserConfig" { "BetaKey" "x" } }`, "x"},
	} {
		m, err := ParseManifest([]byte(c.text))
		if err != nil || m.Build != "7" || m.Branch != c.branch {
			t.Errorf("%s: %+v, %v, want branch %q", name, m, err, c.branch)
		}
	}
}

func TestParseSyntax(t *testing.T) {
	doc, err := Parse([]byte("// a comment\n\"a\" { b c  \"d e\" \"f\\\\g\" }\n\"Z\" \"1\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := doc.Text("A", "b"); v != "c" {
		t.Errorf("bare words: %q", v)
	}
	if v, _ := doc.Text("a", "d e"); v != `f\g` {
		t.Errorf("escape: %q", v)
	}
	if v, ok := doc.Text("z"); !ok || v != "1" {
		t.Errorf("second key: %q %v", v, ok)
	}
	if _, ok := doc.Text("a"); ok {
		t.Error("an object has no text")
	}
	if doc.Get("a", "nope", "b") != nil {
		t.Error("a missing key was found")
	}

	deep := strings.Repeat(`"a" {`, maxDepth+2)
	if _, err := Parse([]byte(deep)); err == nil {
		t.Error("deep nesting was accepted")
	}
}

func TestInfoCommand(t *testing.T) {
	got := InfoCommand([]int64{258550, 2394010})
	want := []string{"steamcmd", "+login", "anonymous", "+app_info_update", "1", "+app_info_print", "258550", "+app_info_print", "2394010", "+quit"}
	if !slices.Equal(got, want) {
		t.Errorf("command %v", got)
	}
}
