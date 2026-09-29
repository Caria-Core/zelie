package core

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/Caria-Core/zelie/internal/peer"
)

func TestPeekVolume(t *testing.T) {
	root := &peer.Peer{UID: 0}
	s, f := newServer()
	dir := t.TempDir()
	f.volumeDirs = map[string]string{"srv-vol": dir}
	os.MkdirAll(filepath.Join(dir, "steamapps"), 0o755)
	os.WriteFile(filepath.Join(dir, "steamapps", "appmanifest_1.acf"), []byte(`"AppState" { "buildid" "7" }`), 0o644)
	// A link out of the volume, as a player with file access could make.
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("host file"), 0o644)
	os.Symlink(secret, filepath.Join(dir, "steamapps", "link"))
	os.WriteFile(filepath.Join(dir, "big"), make([]byte, MaxPeek+1), 0o644)

	peek := func(path string) (int, PeekResponse) {
		body, _ := json.Marshal(PeekRequest{Path: path})
		rec := request(t, s, root, "POST", "/v1/volumes/srv-vol/peek", string(body))
		var res PeekResponse
		json.Unmarshal(rec.Body.Bytes(), &res)
		return rec.Code, res
	}
	if code, res := peek("steamapps/appmanifest_1.acf"); code != http.StatusOK || string(res.Content) != `"AppState" { "buildid" "7" }` {
		t.Errorf("manifest: %d %q", code, res.Content)
	}
	if code, res := peek("/steamapps/appmanifest_1.acf"); code != http.StatusOK || len(res.Content) == 0 {
		t.Errorf("leading slash: %d", code)
	}
	for path, want := range map[string]int{
		"steamapps/missing.acf": http.StatusNotFound,
		"../escape":             http.StatusBadRequest,
		"steamapps/link":        http.StatusUnprocessableEntity,
		"steamapps":             http.StatusUnprocessableEntity,
		"big":                   http.StatusUnprocessableEntity,
	} {
		if code, _ := peek(path); code != want {
			t.Errorf("%s: %d, want %d", path, code, want)
		}
	}
	rec := request(t, s, root, "POST", "/v1/volumes/Bad_Name/peek", `{"path":"a"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad volume name: %d", rec.Code)
	}
}
