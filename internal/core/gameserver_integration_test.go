//go:build integration

package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

// A game server as the panel starts it, with busybox for the game: it prints
// a done line, writes files, serves what it wrote over HTTP and stops when
// it reads "stop" on its console. The panel sets the container's command
// through the image's entrypoint; busybox has none, so the test gives it
// one through Spec.Args.
const gameScript = `cd /home/container
echo "[00:00:01] Done (1s)! For help, type help"
id -u > uid.txt
echo from-the-game > from-game.txt
cp server.properties seen.properties
httpd -p 9000 -h /home/container
while read line; do
  echo "console: $line"
  if [ "$line" = stop ]; then echo stopping; exit 0; fi
done
echo input-ended
`

func statOf(t *testing.T, path string) syscall.Stat_t {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestGameServerRuns(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if _, err := os.Stat(engine.DefaultPaths.Socket); err != nil {
		t.Skip("Zelie's containerd is not running")
	}
	ctx := context.Background()
	e, err := engine.Connect(ctx, engine.DefaultPaths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })

	const (
		app    = "it-game"
		volume = "it-game-vol"
		id     = "it-game-1"
	)
	dir := filepath.Join(engine.DefaultPaths.Volumes, volume)
	os.RemoveAll(dir)
	e.Remove(ctx, id)
	os.Remove(engine.LogPathFor(engine.DefaultPaths, id))
	if err := e.CreateVolume(volume); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.SetForwards(context.Background(), app, nil)
		e.Remove(context.Background(), id)
		os.RemoveAll(dir)
	})

	// What an install leaves: files that belong to root, in the volume and in
	// a folder of it, and a link that points out of the volume.
	os.WriteFile(filepath.Join(dir, "server.properties"), []byte("# props\nserver-port=1\nmotd=hi\n"), 0o644)
	os.MkdirAll(filepath.Join(dir, "world"), 0o755)
	os.WriteFile(filepath.Join(dir, "world", "level.dat"), []byte("x"), 0o644)
	os.Symlink("/etc/passwd", filepath.Join(dir, "link"))
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, nil, 0o644)
	os.Symlink(outside, filepath.Join(dir, "world", "outside-link"))

	prepare := func() {
		t.Helper()
		root, err := e.OpenVolume(ctx, volume)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		notes, err := prepareVolume(root, PrepareRequest{UID: 988, GID: 988, Files: []ConfigFile{
			{Path: "server.properties", Parser: "properties", Changes: []ConfigChange{{Key: "server-port", Value: "9000"}}},
			{Path: "config/extra.json", Parser: "json", Changes: []ConfigChange{{Key: "net.port", Value: "9000"}}},
		}})
		if err != nil || len(notes) != 0 {
			t.Fatalf("prepare: %v, notes %q", err, notes)
		}
	}
	prepare()

	// The volume keeps container IDs on disk, so the files now carry 988.
	for _, p := range []string{".", "server.properties", "world", "world/level.dat", "config", "config/extra.json", "link", "world/outside-link"} {
		if st := statOf(t, filepath.Join(dir, p)); st.Uid != 988 || st.Gid != 988 {
			t.Errorf("%s is owned by %d:%d on disk, want 988:988", p, st.Uid, st.Gid)
		}
	}
	if st := statOf(t, outside); st.Uid != 0 {
		t.Errorf("a file outside the volume was given away: %d", st.Uid)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "server.properties")); string(b) != "# props\nserver-port=9000\nmotd=hi\n" {
		t.Errorf("server.properties: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "config", "extra.json")); !strings.Contains(string(b), `"port": 9000`) {
		t.Errorf("extra.json: %q", b)
	}

	spec := engine.Spec{
		ID: id, App: app, Network: app, Image: installTestImage,
		Args:        []string{"sh", "-c", gameScript},
		Env:         []string{"HOME=/home/container", "STARTUP=unused"},
		Volumes:     []engine.VolumeMount{{Name: volume, Target: "/home/container"}},
		MemoryBytes: 64 << 20, CPUs: 0.5, Pids: 64,
		User: &engine.IDs{UID: 988, GID: 988}, WorkDir: "/home/container", Stdin: true,
	}
	if err := e.Run(ctx, spec); err != nil {
		t.Fatal(err)
	}
	installLog(t, id, "Done (1s)!")

	// What the game wrote belongs to the same user, and it ran as it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "seen.properties")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the game never wrote its files")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "uid.txt")); string(b) != "988\n" {
		t.Errorf("the game ran as user %q", b)
	}
	for _, p := range []string{"uid.txt", "from-game.txt", "seen.properties"} {
		if st := statOf(t, filepath.Join(dir, p)); st.Uid != 988 || st.Gid != 988 {
			t.Errorf("%s is owned by %d:%d on disk, want 988:988", p, st.Uid, st.Gid)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "seen.properties")); !strings.Contains(string(b), "server-port=9000") {
		t.Errorf("the game did not see the edited config: %q", b)
	}

	// A player reaches the forwarded port.
	var ip string
	list, _ := e.List(ctx)
	for _, c := range list {
		if c.ID == id {
			ip = c.IP.String()
		}
	}
	if ip == "" {
		t.Fatal("no address")
	}
	b := net.ParseIP(ip).To4()
	gateway := net.IPv4(b[0], b[1], b[2], 1).String()
	hostPort := freeTCPPort(t)
	if err := e.SetForwards(ctx, app, []engine.Forward{{Port: hostPort, Proto: "tcp", Target: 9000}}); err != nil {
		t.Fatal(err)
	}
	var body string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Get(fmt.Sprintf("http://%s/from-game.txt", net.JoinHostPort(gateway, fmt.Sprint(hostPort))))
		if err == nil {
			data, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(data)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the forwarded port never answered: %v", err)
		}
	}
	if body != "from-the-game\n" {
		t.Errorf("served %q", body)
	}

	// The console: a line arrives, and "stop" ends the game the way the
	// egg's stop command does.
	if err := e.WriteStdin(ctx, id, []byte("say hello\n")); err != nil {
		t.Fatal(err)
	}
	installLog(t, id, "console: say hello")

	// The core restarts, as it does in an update, and the game goes on
	// taking commands.
	e.Close()
	if e, err = engine.Connect(ctx, engine.DefaultPaths); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteStdin(ctx, id, []byte("after restart\n")); err != nil {
		t.Fatal(err)
	}
	installLog(t, id, "console: after restart")
	if err := e.WriteStdin(ctx, id, []byte("stop\n")); err != nil {
		t.Fatal(err)
	}
	installLog(t, id, "stopping")
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	code, err := e.Wait(waitCtx, id)
	if err != nil || code != 0 {
		t.Fatalf("the game exited with %d, %v", code, err)
	}
	if err := e.Stop(ctx, id, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteStdin(ctx, id, []byte("more\n")); !errors.Is(err, engine.ErrInputClosed) {
		t.Errorf("writing to a stopped game: %v", err)
	}

	// The files are made ready again for the next start, including the ones
	// the game made.
	os.WriteFile(filepath.Join(dir, "world", "new-as-root"), []byte("x"), 0o600)
	prepare()
	if st := statOf(t, filepath.Join(dir, "world", "new-as-root")); st.Uid != 988 {
		t.Errorf("a new file is owned by %d", st.Uid)
	}

	// Removing the container takes its directory, and the input pipe in it.
	if err := e.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engine.DefaultPaths.Data, "containers", id)); !os.IsNotExist(err) {
		t.Errorf("the container's directory is still there: %v", err)
	}
}

func freeTCPPort(t *testing.T) uint16 {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}
