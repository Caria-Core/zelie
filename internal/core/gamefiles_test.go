package core

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/containerd/errdefs"
)

func prepareBody(t *testing.T, req PrepareRequest) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPrepareVolumeEditsConfigFiles(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	dir := t.TempDir()
	f.volumeDirs = map[string]string{"srv-vol": dir}
	os.WriteFile(filepath.Join(dir, "server.properties"), []byte("# props\nserver-port=1\nmotd=x\n"), 0o640)
	os.MkdirAll(filepath.Join(dir, "config"), 0o755)

	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	req := PrepareRequest{UID: uid, GID: gid, Files: []ConfigFile{
		{Path: "server.properties", Parser: "properties", Changes: []ConfigChange{{Key: "server-port", Value: "25570"}}},
		// Made when missing, with its folder.
		{Path: "/config/paper-global.yml", Parser: "yaml", Changes: []ConfigChange{{Key: "proxies.velocity.enabled", Value: "true"}}},
		// A file the game makes itself, and nothing to change in it yet.
		{Path: "later.cfg", Parser: "file", Changes: []ConfigChange{{Key: "a ", Value: "a 1"}}},
		// Broken content is a note, and the rest goes on.
		{Path: "bad.json", Parser: "json", Changes: []ConfigChange{{Key: "a", Value: "1"}}},
		{Path: "../escape.properties", Parser: "properties", Changes: []ConfigChange{{Key: "a", Value: "1"}}},
		{Path: "server.properties", Parser: "toml", Changes: []ConfigChange{{Key: "a", Value: "1"}}},
	}}
	os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o644)

	rec := request(t, s, root, "POST", "/v1/volumes/srv-vol/prepare", prepareBody(t, req))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var res PrepareResponse
	json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Notes) != 3 {
		t.Errorf("notes: %q", res.Notes)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "server.properties"))
	if string(got) != "# props\nserver-port=25570\nmotd=x\n" {
		t.Errorf("server.properties: %q", got)
	}
	if st, _ := os.Stat(filepath.Join(dir, "server.properties")); st.Mode().Perm() != 0o640 {
		t.Errorf("mode %v changed", st.Mode().Perm())
	}
	got, _ = os.ReadFile(filepath.Join(dir, "config", "paper-global.yml"))
	if !strings.Contains(string(got), "enabled: true") {
		t.Errorf("paper-global.yml: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "later.cfg")); !os.IsNotExist(err) {
		t.Errorf("an empty result made a file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.properties")); err == nil {
		t.Error("a file was made outside the volume")
	}
	// No temporary file is left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".zelie-") {
			t.Errorf("left %s", e.Name())
		}
	}
}

// The panel writes Minecraft's eula.txt this way: made when missing, and
// changed when it says eula=false, as the server leaves it.
func TestEULAFileIsMadeOrChanged(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	dir := t.TempDir()
	f.volumeDirs = map[string]string{"srv-vol": dir}
	req := PrepareRequest{UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), Files: []ConfigFile{
		{Path: "eula.txt", Parser: "properties", Changes: []ConfigChange{{Key: "eula", Value: "true"}}},
	}}
	prepare := func() {
		t.Helper()
		if rec := request(t, s, root, "POST", "/v1/volumes/srv-vol/prepare", prepareBody(t, req)); rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
	check := func(want string) {
		t.Helper()
		if got, _ := os.ReadFile(filepath.Join(dir, "eula.txt")); string(got) != want {
			t.Errorf("eula.txt: %q, want %q", got, want)
		}
	}
	prepare()
	check("eula=true\n")
	os.WriteFile(filepath.Join(dir, "eula.txt"), []byte("#By changing the setting below to TRUE you are indicating your agreement to the EULA.\neula=false\n"), 0o644)
	prepare()
	check("#By changing the setting below to TRUE you are indicating your agreement to the EULA.\neula=true\n")
}

func TestConfigFilesStayInsideTheVolume(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "vol")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(dir, 0o755)
	os.MkdirAll(outside, 0o755)
	secret := filepath.Join(outside, "secret.properties")
	os.WriteFile(secret, []byte("a=1\n"), 0o644)
	os.Symlink(secret, filepath.Join(dir, "link.properties"))
	os.Symlink(outside, filepath.Join(dir, "dirlink"))
	os.Symlink("../outside/secret.properties", filepath.Join(dir, "relative.properties"))

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	change := []ConfigChange{{Key: "a", Value: "2"}}
	for _, p := range []string{"link.properties", "dirlink/secret.properties", "dirlink/new.properties", "relative.properties", "../outside/secret.properties", "a/../../outside/secret.properties", ".."} {
		if _, err := editConfigFile(root, ConfigFile{Path: p, Parser: "properties", Changes: change}); err == nil {
			t.Errorf("%s: the edit went ahead", p)
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "a=1\n" {
		t.Errorf("a file outside the volume changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(outside, "new.properties")); err == nil {
		t.Error("a file was made outside the volume")
	}

	// Links that stay inside are followed.
	os.WriteFile(filepath.Join(dir, "real.properties"), []byte("a=1\n"), 0o644)
	os.Symlink("real.properties", filepath.Join(dir, "inside.properties"))
	if _, err := editConfigFile(root, ConfigFile{Path: "inside.properties", Parser: "properties", Changes: change}); err != nil {
		t.Error(err)
	}

	// Too large, and not a regular file.
	os.WriteFile(filepath.Join(dir, "big.properties"), make([]byte, 4<<20+1), 0o644)
	if _, err := editConfigFile(root, ConfigFile{Path: "big.properties", Parser: "properties", Changes: change}); err == nil {
		t.Error("a file over the limit was read")
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.properties"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := editConfigFile(root, ConfigFile{Path: "pipe.properties", Parser: "properties", Changes: change}); err == nil {
		t.Error("a named pipe was edited")
	}
}

// otherGroup finds a group the test user is in besides its own, which it
// may hand files to without being root.
func otherGroup(t *testing.T) int {
	t.Helper()
	groups, _ := os.Getgroups()
	for _, g := range groups {
		if g != os.Getgid() {
			return g
		}
	}
	t.Skip("the test user is in no other group")
	return 0
}

func TestChownStaysInsideTheVolume(t *testing.T) {
	gid := otherGroup(t)
	base := t.TempDir()
	dir := filepath.Join(base, "vol")
	os.MkdirAll(filepath.Join(dir, "world", "region"), 0o755)
	os.WriteFile(filepath.Join(dir, "world", "region", "r.0.0.mca"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(base, "outside"), []byte("x"), 0o644)
	os.Symlink(filepath.Join(base, "outside"), filepath.Join(dir, "world", "link"))
	os.Symlink(base, filepath.Join(dir, "up"))

	root, _ := os.OpenRoot(dir)
	defer root.Close()
	if err := chownAll(root, os.Getuid(), gid); err != nil {
		t.Fatal(err)
	}
	group := func(p string) int {
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		return int(st.Gid)
	}
	for _, p := range []string{dir, "world", "world/region", "world/region/r.0.0.mca", "world/link", "up"} {
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(dir, p)
		}
		if group(full) != gid {
			t.Errorf("%s was not changed", p)
		}
	}
	// What the links pointed at is not part of the volume.
	if group(filepath.Join(base, "outside")) == gid || group(base) == gid {
		t.Error("something outside the volume was changed")
	}
}

func TestPrepareVolumeIsChecked(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	f.volumeDirs = map[string]string{"srv-vol": t.TempDir(), "busy": t.TempDir()}
	f.usedVolumes = map[string]bool{"busy": true}
	ok := `{"uid":988,"gid":988}`
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"owner outside the container": {"/v1/volumes/srv-vol/prepare", `{"uid":70000,"gid":0}`, http.StatusBadRequest},
		"unknown field":               {"/v1/volumes/srv-vol/prepare", `{"uid":1,"gid":1,"path":"/etc"}`, http.StatusBadRequest},
		"bad volume name":             {"/v1/volumes/Bad_Name/prepare", ok, http.StatusBadRequest},
		"no such volume":              {"/v1/volumes/nope/prepare", ok, http.StatusNotFound},
		"a container runs":            {"/v1/volumes/busy/prepare", ok, http.StatusConflict},
		"too many files":              {"/v1/volumes/srv-vol/prepare", `{"uid":1,"gid":1,"files":[` + strings.Repeat(`{"path":"a","parser":"file","changes":[]},`, 64) + `{"path":"a","parser":"file","changes":[]}]}`, http.StatusBadRequest},
		"valid":                       {"/v1/volumes/srv-vol/prepare", `{"uid":` + strconv.Itoa(os.Getuid()) + `,"gid":` + strconv.Itoa(os.Getgid()) + `}`, http.StatusOK},
	} {
		if got := request(t, s, root, "POST", c.path, c.body).Code; got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
	}
	if got := request(t, s, &peer.Peer{UID: 1000}, "POST", "/v1/volumes/srv-vol/prepare", ok).Code; got != http.StatusForbidden {
		t.Errorf("someone else: status %d", got)
	}
}

func TestStdinRequest(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	if code := request(t, s, root, "POST", "/v1/containers/mc-1/stdin", `{"data":"say hello\n"}`).Code; code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if got := f.stdin["mc-1"]; len(got) != 1 || got[0] != "say hello\n" {
		t.Errorf("written %q", got)
	}
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"empty":             {"/v1/containers/mc-1/stdin", `{"data":""}`, http.StatusBadRequest},
		"too long":          {"/v1/containers/mc-1/stdin", `{"data":"` + strings.Repeat("x", 4097) + `"}`, http.StatusBadRequest},
		"exactly the limit": {"/v1/containers/mc-1/stdin", `{"data":"` + strings.Repeat("x", 4096) + `"}`, http.StatusNoContent},
		"invalid id":        {"/v1/containers/Bad_ID/stdin", `{"data":"x"}`, http.StatusBadRequest},
		"unknown field":     {"/v1/containers/mc-1/stdin", `{"data":"x","fd":2}`, http.StatusBadRequest},
		"not json":          {"/v1/containers/mc-1/stdin", `say hello`, http.StatusBadRequest},
	} {
		if got := request(t, s, root, "POST", c.path, c.body).Code; got != c.want {
			t.Errorf("%s: status %d, want %d", name, got, c.want)
		}
	}
	if code := request(t, s, &peer.Peer{UID: 1000}, "POST", "/v1/containers/mc-1/stdin", `{"data":"x"}`).Code; code != http.StatusForbidden {
		t.Errorf("someone else: status %d", code)
	}

	// A container that does not read its input is a conflict the panel
	// can act on, with a code of its own.
	f.stdinErr = engine.ErrInputClosed
	rec := request(t, s, root, "POST", "/v1/containers/mc-1/stdin", `{"data":"stop\n"}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "container.input_closed") {
		t.Errorf("closed input: %d %s", rec.Code, rec.Body)
	}
	f.stdinErr = errdefs.ErrNotFound
	if code := request(t, s, root, "POST", "/v1/containers/mc-1/stdin", `{"data":"x"}`).Code; code != http.StatusNotFound {
		t.Errorf("missing container: status %d", code)
	}
}

func TestSignalRequest(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	if code := request(t, s, root, "POST", "/v1/containers/mc-1/signal", `{"signal":"SIGINT"}`).Code; code != http.StatusNoContent {
		t.Fatalf("status %d", code)
	}
	if f.signalled["mc-1"] != syscall.SIGINT {
		t.Errorf("sent %v", f.signalled)
	}
	for _, body := range []string{`{"signal":"SIGSTOP"}`, `{"signal":"9"}`, `{"signal":""}`, `{}`} {
		if code := request(t, s, root, "POST", "/v1/containers/mc-1/signal", body).Code; code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, code)
		}
	}
}

func TestRunTakesAGameSpec(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	body := `{"id":"mc-1","app":"mc","image":"ghcr.io/example/java:21","memory_bytes":1073741824,"cpus":1,"pids":512,` +
		`"user":{"uid":988,"gid":988},"work_dir":"/home/container","stdin":true}`
	if code := request(t, s, root, "POST", "/v1/containers", body).Code; code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	got := f.ran[0]
	if got.User == nil || got.User.UID != 988 || got.WorkDir != "/home/container" || !got.Stdin {
		t.Errorf("spec %+v", got)
	}
	for _, bad := range []string{`"user":{"uid":65536,"gid":0}`, `"work_dir":"home"`} {
		b := `{"id":"mc-2","image":"x","memory_bytes":1,"cpus":1,"pids":1,` + bad + `}`
		if code := request(t, s, root, "POST", "/v1/containers", b).Code; code != http.StatusBadRequest {
			t.Errorf("%s: status %d", bad, code)
		}
	}
}

func TestConfigFileIsMadeForAddedLines(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	add := ConfigFile{Path: "server/rust/cfg/server.cfg", Parser: "file", Changes: []ConfigChange{
		{Key: "server.hostname ", Value: `server.hostname "Mine"`, Add: true},
	}}
	if _, err := editConfigFile(root, add); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, add.Path))
	if want := "server.hostname \"Mine\"\n"; string(got) != want {
		t.Errorf("server.cfg: %q, want %q", got, want)
	}

	none := ConfigFile{Path: "other/dir/server.cfg", Parser: "file", Changes: []ConfigChange{
		{Key: "server.hostname ", Value: `server.hostname "Mine"`},
	}}
	if _, err := editConfigFile(root, none); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other")); err == nil {
		t.Error("a file with nothing to add was made")
	}
}
