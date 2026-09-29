//go:build integration

package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// A game server gets the limit it asks for, as far as containerd's own hard
// limit allows, also as an ordinary user in the container's user namespace.
func TestOpenFiles(t *testing.T) {
	e := connect(t)
	want := e.openFiles(MaxOpenFiles)
	if want == 0 {
		t.Fatal("containerd's hard limit on open files is no more than the default; run zelie engine install to update its unit")
	}
	t.Logf("containers get %d open files", want)
	ceiling, ok := openFilesCeiling(DefaultPaths.Socket)
	t.Logf("ceiling %d, read: %v", ceiling, ok)

	limits := func(id string, s Spec) string {
		t.Helper()
		s.ID, s.Image = id, testImage
		s.Args = []string{"sleep", "600"}
		s.MemoryBytes, s.CPUs, s.Pids = 64<<20, 0.5, 32
		run(t, e, s)
		var out, errOut bytes.Buffer
		code, err := e.Exec(context.Background(), id, []string{"sh", "-c", `echo "$(ulimit -Sn) $(ulimit -Hn)"; grep 'Max open files' /proc/1/limits`}, nil, &out, &errOut)
		if err != nil || code != 0 {
			t.Fatalf("%s: exec %d %v %s", id, code, err, errOut.String())
		}
		return out.String()
	}

	if got := limits("it-nofile-default", Spec{}); !strings.HasPrefix(got, "1024 1024\n") {
		t.Errorf("without a request: %q", got)
	}
	// The way a game server runs: as a user of the container, not its root.
	game := limits("it-nofile-game", Spec{OpenFiles: MaxOpenFiles, User: &IDs{UID: 1000, GID: 1000}, WorkDir: "/tmp", Stdin: true})
	if !strings.HasPrefix(game, fmt.Sprintf("%d %d\n", want, want)) || !strings.Contains(game, fmt.Sprint(want)) {
		t.Errorf("game server: %q, want %d", game, want)
	}
	// Root inside the container, with the same request.
	if got := limits("it-nofile-root", Spec{OpenFiles: MaxOpenFiles}); !strings.HasPrefix(got, fmt.Sprintf("%d %d\n", want, want)) {
		t.Errorf("root: %q, want %d", got, want)
	}
}
