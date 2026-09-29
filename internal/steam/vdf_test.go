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
	// The public branch, not the other branches or the depot's manifests
	// that are also called public; the app with no public branch is left out.
	want := map[int64]string{258550: "20913457", 2394010: "20871234"}
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

func TestInstalledBuild(t *testing.T) {
	got, err := InstalledBuild(fixture(t, "appmanifest_258550.acf"))
	if err != nil || got != "20913457" {
		t.Errorf("build %q, %v", got, err)
	}
	for _, bad := range []string{"", `"AppState" { "appid" "1" }`, `"AppState" { "buildid" "abc" }`, `"AppState" {`} {
		if _, err := InstalledBuild([]byte(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
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
