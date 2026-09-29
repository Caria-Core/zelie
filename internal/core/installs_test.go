package core

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
)

func TestInstall(t *testing.T) {
	root := &peer.Peer{UID: 0}
	body := func(extra string) string {
		return `{"id":"mc-install-1","app":"mc","image":"ghcr.io/example/installer:latest","entrypoint":"bash","script":"echo hi\n",` +
			`"env":["SERVER_PORT=25565"],"volume":"vol-1","network":"mc","memory_bytes":1073741824,"cpus":1,"pids":1024` + extra + `}`
	}
	s, f := newServer()
	if code := request(t, s, root, "POST", "/v1/installs", body("")).Code; code != http.StatusCreated {
		t.Fatalf("status %d", code)
	}
	if len(f.ran) != 1 {
		t.Fatalf("ran %d containers", len(f.ran))
	}
	got := f.ran[0]
	if got.Args[0] != "bash" || got.Args[1] != InstallScriptPath {
		t.Errorf("args %v", got.Args)
	}
	if len(got.Volumes) != 1 || got.Volumes[0].Name != "vol-1" || got.Volumes[0].Target != InstallVolumePath {
		t.Errorf("volumes %+v", got.Volumes)
	}
	if len(got.Files) != 1 || got.Files[0].Target != InstallScriptPath || string(got.Files[0].Content) != "echo hi\n" {
		t.Errorf("files %+v", got.Files)
	}
	if got.App != "mc" || got.Network != "mc" || got.Env[0] != "SERVER_PORT=25565" {
		t.Errorf("spec %+v", got)
	}

	for name, b := range map[string]string{
		"a shell with arguments": `{"id":"i","app":"mc","image":"x","entrypoint":"bash -c","script":"","volume":"v","memory_bytes":1,"cpus":1,"pids":1}`,
		"no volume":              `{"id":"i","app":"mc","image":"x","entrypoint":"bash","script":"","memory_bytes":1,"cpus":1,"pids":1}`,
		"no limits":              `{"id":"i","app":"mc","image":"x","entrypoint":"bash","script":"","volume":"v"}`,
		"mounts":                 body(`,"mounts":[{"source":"/","target":"/host"}]`),
		"a script that is too big": `{"id":"i","app":"mc","image":"x","entrypoint":"bash","script":"` + strings.Repeat("x", engine.MaxFileSize+1) +
			`","volume":"v","memory_bytes":1,"cpus":1,"pids":1}`,
	} {
		if code := request(t, s, root, "POST", "/v1/installs", b).Code; code != http.StatusBadRequest {
			t.Errorf("%s: status %d", name, code)
		}
	}
	if code := request(t, s, &peer.Peer{UID: 1000}, "POST", "/v1/installs", body("")).Code; code != http.StatusForbidden {
		t.Errorf("someone else: status %d", code)
	}
}
