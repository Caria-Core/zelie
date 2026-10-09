package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/install"
)

// release serves a signed release the way GitHub does.
func release(t *testing.T, key ed25519.PrivateKey, bin string, tamper func(files map[string][]byte)) string {
	sum := sha256.Sum256([]byte(bin))
	sums := []byte(hex.EncodeToString(sum[:]) + "  zelie-linux-amd64\n" + strings.Repeat("0", 64) + "  install.sh\n")
	files := map[string][]byte{
		"SHA256SUMS":        sums,
		"SHA256SUMS.sig":    ed25519.Sign(key, sums),
		"zelie-linux-amd64": []byte(bin),
	}
	if tamper != nil {
		tamper(files)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[strings.TrimPrefix(r.URL.Path, "/v1.2.3/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

func TestFetch(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	f := &Fetcher{Base: release(t, priv, "new binary", nil), Key: pub}
	bin, err := f.Binary(context.Background(), "v1.2.3", "amd64")
	if err != nil || string(bin) != "new binary" {
		t.Fatalf("%q, %v", bin, err)
	}
	if _, err := f.Binary(context.Background(), "v1.2.3", "arm64"); err == nil {
		t.Error("an architecture the release lacks")
	}
	if _, err := f.Binary(context.Background(), "latest; rm -rf /", "amd64"); err == nil {
		t.Error("a version that is not one")
	}

	// Someone swaps the binary: the checksum no longer matches.
	f.Base = release(t, priv, "new binary", func(m map[string][]byte) { m["zelie-linux-amd64"] = []byte("evil") })
	if _, err := f.Binary(context.Background(), "v1.2.3", "amd64"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("swapped binary: %v", err)
	}
	// And the list too, but they cannot sign it.
	f.Base = release(t, priv, "new binary", func(m map[string][]byte) {
		sum := sha256.Sum256([]byte("evil"))
		m["zelie-linux-amd64"] = []byte("evil")
		m["SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + "  zelie-linux-amd64\n")
	})
	if _, err := f.Binary(context.Background(), "v1.2.3", "amd64"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("swapped list: %v", err)
	}
	// Signed with another key.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	f.Base = release(t, other, "new binary", nil)
	if _, err := f.Binary(context.Background(), "v1.2.3", "amd64"); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("another key: %v", err)
	}
}

func TestEmbeddedKeyMatchesInstallScript(t *testing.T) {
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), strings.TrimSpace(string(publicKey))) {
		t.Error("install.sh and the binary carry different release keys")
	}
	if _, err := ParseKey(publicKey); err != nil {
		t.Error(err)
	}
}

// exec replaces the shell, so its EXIT trap never runs and the download
// would stay in /tmp.
func TestInstallScriptCleansUpBeforeItExecs(t *testing.T) {
	script, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	before, _, ok := strings.Cut(string(script), "exec /usr/local/bin/zelie")
	if !ok {
		t.Fatal("install.sh no longer ends in exec")
	}
	if !strings.Contains(before, "\nrm -rf \"$tmp\"\n") {
		t.Error("install.sh leaves its temporary directory behind when it execs the installer")
	}
}

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v0.1.4", "v0.1.3", true},
		{"v0.2.0", "v0.1.9", true},
		{"v0.10.0", "v0.9.0", true},
		{"v1.0.0", "v0.99.99", true},
		{"v0.1.3", "v0.1.3", false},
		{"v0.1.2", "v0.1.3", false},
		{"v0.1.4", "dev", false},
		{"v0.1.4-rc1", "v0.1.3", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%s, %s) = %v", c.a, c.b, got)
		}
	}
}

func setup(t *testing.T) string {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, filepath.Dir(install.Binary)), 0o755)
	os.MkdirAll(filepath.Join(root, filepath.Dir(StateFile)), 0o755)
	os.WriteFile(filepath.Join(root, install.Binary), []byte("old binary"), 0o755)
	if err := Place(root, []byte("new binary")); err != nil {
		t.Fatal(err)
	}
	return root
}

func read(root, path string) string {
	b, _ := os.ReadFile(filepath.Join(root, path))
	return string(b)
}

func TestUpdateThatComesUp(t *testing.T) {
	root := setup(t)
	if read(root, install.Binary) != "new binary" || read(root, Old) != "old binary" {
		t.Fatalf("placed %q, kept %q", read(root, install.Binary), read(root, Old))
	}
	var ran []string
	f := &Finisher{
		Root: root, Timeout: time.Second, Now: time.Now,
		Exec: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			ran = append(ran, cmd)
			// A server without the SFTP unit, as one from before it existed.
			if strings.HasSuffix(cmd, "zelie-sftp.service") || strings.HasSuffix(cmd, "zelie-sftp.socket") {
				return "Unit zelie-sftp.socket not found.", errors.New("exit status 5")
			}
			return "", nil
		},
		Healthy: func(_ context.Context, v string) error { return nil },
	}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	want := []string{"systemctl restart zelie-core zelie-proxy zelie-panel", "systemctl try-restart zelie-sftp.service", "systemctl restart zelie-sftp.socket"}
	if !res.OK || !slices.Equal(ran, want) {
		t.Errorf("%+v, ran %v", res, ran)
	}
	if last, _ := Last(root); !last.OK || last.To != "v1.2.3" {
		t.Errorf("saved %+v", last)
	}
}

func TestUpdateThatFailsGoesBack(t *testing.T) {
	root := setup(t)
	restarts := 0
	f := &Finisher{
		Root: root, Timeout: 1500 * time.Millisecond, Now: time.Now,
		Exec: func(context.Context, string, ...string) (string, error) { restarts++; return "", nil },
		// Only the old version ever answers.
		Healthy: func(_ context.Context, v string) error {
			if read(root, install.Binary) == "old binary" && v == "v1.2.2" {
				return nil
			}
			return errors.New("the panel does not answer")
		},
	}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || !strings.Contains(res.Error, "did not come up") || !strings.Contains(res.Error, "v1.2.2 is running again") {
		t.Errorf("%+v", res)
	}
	if read(root, install.Binary) != "old binary" || restarts != 6 {
		t.Errorf("binary %q after %d restarts", read(root, install.Binary), restarts)
	}
	if last, _ := Last(root); last.OK || last.Error == "" {
		t.Errorf("saved %+v", last)
	}
	// The failure is saved as messages, not only as English text.
	if res.Reason == nil || res.Reason.Code != "update.not_up" || res.Reason.Params["version"] != "v1.2.3" {
		t.Errorf("reason %+v", res.Reason)
	}
	if res.Back == nil || res.Back.Code != "update.back" || res.Back.Params["version"] != "v1.2.2" {
		t.Errorf("back %+v", res.Back)
	}
	last, _ := Last(root)
	if last.Reason == nil || last.Reason.Code != "update.not_up" || last.Back == nil || last.Back.Code != "update.back" {
		t.Errorf("messages not saved: %+v", last)
	}
}

func TestUpdateThatCannotGoBack(t *testing.T) {
	root := setup(t)
	os.Remove(filepath.Join(root, Old))
	f := &Finisher{
		Root: root, Timeout: time.Second, Now: time.Now,
		Exec: func(context.Context, string, ...string) (string, error) {
			return "Failed to restart", errors.New("exit status 1")
		},
		Healthy: func(context.Context, string) error { return nil },
	}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || res.Reason == nil || res.Reason.Code != "update.restart_failed" || !strings.Contains(res.Reason.Text, "Failed to restart") {
		t.Errorf("reason %+v", res.Reason)
	}
	if res.Back == nil || res.Back.Code != "update.old_unread" {
		t.Errorf("back %+v", res.Back)
	}
	if !strings.Contains(res.Error, res.Reason.Text) || !strings.Contains(res.Error, res.Back.Text) {
		t.Errorf("error %q lacks the messages", res.Error)
	}
}

func TestLastReadsAnEarlierFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.Dir(StateFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"from":"v1.0.0","to":"v1.0.1","ok":false,"error":"the panel does not answer","at":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(root, StateFile), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	last, err := Last(root)
	if err != nil || last.Error != "the panel does not answer" || last.Reason != nil {
		t.Errorf("%+v, %v", last, err)
	}
}

// panelDatabase puts a panel database in the server's place, as the old
// version left it: stopped uncleanly, with a log not yet in the file.
func panelDatabase(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, filepath.Dir(install.PanelDB))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, install.PanelDB), []byte("schema 31"), 0o600)
	os.WriteFile(filepath.Join(root, install.PanelDB+"-wal"), []byte("log of 31"), 0o600)
}

// server stands in for systemd and the panel: the new binary's panel
// migrates the database as it starts, and the old binary's refuses a
// database it does not know.
func server(root string, ran *[]string) (func(context.Context, string, ...string) (string, error), func(context.Context, string) error) {
	exec := func(_ context.Context, name string, args ...string) (string, error) {
		cmd := name + " " + strings.Join(args, " ")
		*ran = append(*ran, cmd)
		if strings.HasPrefix(cmd, "systemctl restart zelie-core") && read(root, install.Binary) == "new binary" {
			os.WriteFile(filepath.Join(root, install.PanelDB), []byte("schema 32"), 0o600)
			os.WriteFile(filepath.Join(root, install.PanelDB+"-wal"), []byte("log of 32"), 0o600)
			os.WriteFile(filepath.Join(root, install.PanelDB+"-shm"), []byte("index of 32"), 0o600)
		}
		return "", nil
	}
	healthy := func(_ context.Context, v string) error {
		switch {
		case read(root, install.Binary) == "old binary" && v == "v1.2.2" && read(root, install.PanelDB) == "schema 31":
			return nil
		case read(root, install.Binary) == "old binary":
			return errors.New("database schema 32 is newer than this version of Zelie understands")
		}
		return errors.New("the new version does not answer")
	}
	return exec, healthy
}

func TestUpdateThatFailsPutsTheDatabaseBack(t *testing.T) {
	root := setup(t)
	panelDatabase(t, root)
	var ran []string
	exec, healthy := server(root, &ran)
	f := &Finisher{Root: root, Timeout: 1500 * time.Millisecond, Now: time.Now, Exec: exec, Healthy: healthy}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || !strings.Contains(res.Error, "v1.2.2 is running again") {
		t.Fatalf("%+v", res)
	}
	if read(root, install.PanelDB) != "schema 31" || read(root, install.PanelDB+"-wal") != "log of 31" {
		t.Errorf("database %q, log %q", read(root, install.PanelDB), read(root, install.PanelDB+"-wal"))
	}
	if _, err := os.Stat(filepath.Join(root, install.PanelDB+"-shm")); err == nil {
		t.Error("the new version's index is still there")
	}
	for _, left := range []string{DBCopy, DBCopy + "-wal"} {
		if _, err := os.Stat(filepath.Join(root, left)); err == nil {
			t.Errorf("%s was kept after going back", left)
		}
	}
	// The panel is stopped before each restart, so nothing writes while the
	// database is copied or put back.
	stop, restart := "systemctl stop zelie-panel", "systemctl restart zelie-core zelie-proxy zelie-panel"
	var order []string
	for _, c := range ran {
		if c == stop || c == restart {
			order = append(order, c)
		}
	}
	if !slices.Equal(order, []string{stop, restart, stop, restart}) {
		t.Errorf("ran %v", ran)
	}
}

func TestUpdateThatComesUpDropsTheDatabaseCopy(t *testing.T) {
	root := setup(t)
	panelDatabase(t, root)
	var ran []string
	exec, _ := server(root, &ran)
	f := &Finisher{
		Root: root, Timeout: time.Second, Now: time.Now, Exec: exec,
		Healthy: func(context.Context, string) error {
			// Mid-way, the copy is what an update that fails would go back to.
			if read(root, DBCopy) != "schema 31" || read(root, DBCopy+"-wal") != "log of 31" {
				return errors.New("no copy of the database to go back to")
			}
			return nil
		},
	}
	if res := f.Run(context.Background(), "v1.2.2", "v1.2.3"); !res.OK {
		t.Fatalf("%+v", res)
	}
	if read(root, install.PanelDB) != "schema 32" {
		t.Errorf("the migrated database was touched: %q", read(root, install.PanelDB))
	}
	if _, err := os.Stat(filepath.Join(root, DBCopy)); err == nil {
		t.Error("the copy was kept after a good update")
	}
}

// A copy that could not be made means an update that could not be undone:
// it stops before anything runs the new binary.
func TestUpdateStopsWhenTheDatabaseCannotBeSaved(t *testing.T) {
	root := setup(t)
	panelDatabase(t, root)
	// The copy goes in a directory that is not there.
	os.RemoveAll(filepath.Join(root, filepath.Dir(DBCopy)))
	var ran []string
	exec, healthy := server(root, &ran)
	f := &Finisher{Root: root, Timeout: 1500 * time.Millisecond, Now: time.Now, Exec: exec, Healthy: healthy}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || res.Reason == nil || res.Reason.Code != "update.db_unsaved" || res.Back == nil || res.Back.Code != "update.back" {
		t.Fatalf("%+v", res)
	}
	if read(root, install.Binary) != "old binary" || read(root, install.PanelDB) != "schema 31" {
		t.Errorf("binary %q, database %q", read(root, install.Binary), read(root, install.PanelDB))
	}
	// The panel was stopped for the copy, and the restart brings it back, on
	// the old binary.
	if last := ran[len(ran)-1]; last != "systemctl restart zelie-sftp.socket" || !slices.Contains(ran, "systemctl restart zelie-core zelie-proxy zelie-panel") {
		t.Errorf("ran %v", ran)
	}
}

// If the database cannot be saved and the old binary cannot be put back
// either, nothing restarts the services. The panel was stopped for the copy,
// so it is started again, or the update would leave no panel to show why it
// failed. It is the new version, against the old core and proxy and with no
// copy of its database, and the result says so.
func TestUpdateStartsThePanelWhenNothingCanGoBack(t *testing.T) {
	root := setup(t)
	panelDatabase(t, root)
	os.RemoveAll(filepath.Join(root, filepath.Dir(DBCopy)))
	os.Remove(filepath.Join(root, Old))
	var ran []string
	exec, _ := server(root, &ran)
	f := &Finisher{Root: root, Timeout: time.Second, Now: time.Now, Exec: exec, Healthy: func(context.Context, string) error { return errors.New("no") }}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || res.Reason == nil || res.Reason.Code != "update.db_unsaved" {
		t.Fatalf("%+v", res)
	}
	if res.Back == nil || res.Back.Code != "update.old_unplaced_panel_up" || res.Back.Params["version"] != "v1.2.3" || res.Back.Params["from"] != "v1.2.2" {
		t.Fatalf("back %+v", res.Back)
	}
	// The cause is in it, and the whole thing is in the text saved for
	// earlier languages.
	if detail, _ := res.Back.Params["detail"].(string); !strings.Contains(detail, Old) || !strings.Contains(res.Error, res.Back.Text) {
		t.Errorf("detail %q, error %q", detail, res.Error)
	}
	if !slices.Equal(ran, []string{"systemctl stop zelie-panel", "systemctl start zelie-panel"}) {
		t.Errorf("ran %v", ran)
	}

	// And when even that fails, the result says so.
	f.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
		if args[0] == "start" {
			return "Failed to start zelie-panel.service", errors.New("exit status 1")
		}
		return exec(ctx, name, args...)
	}
	res = f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.Back == nil || res.Back.Code != "update.old_unplaced_panel_down" || !strings.Contains(res.Back.Text, "Failed to start") {
		t.Errorf("back %+v", res.Back)
	}
}

// A panel that was never stopped has nothing to be started, and a panel the
// restart already brought up is not started again: the result must not say
// it was.
func TestUpdateDoesNotStartThePanelThatIsRunning(t *testing.T) {
	// The database was saved, the new version did not come up, and the old
	// binary is gone. The restart started the panel.
	root := setup(t)
	panelDatabase(t, root)
	os.Remove(filepath.Join(root, Old))
	var ran []string
	exec, _ := server(root, &ran)
	f := &Finisher{Root: root, Timeout: time.Second, Now: time.Now, Exec: exec, Healthy: func(context.Context, string) error { return errors.New("no") }}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || res.Back == nil || res.Back.Code != "update.old_unplaced_db_kept" || res.Back.Params["copy"] != DBCopy {
		t.Fatalf("%+v %+v", res, res.Back)
	}
	if slices.Contains(ran, "systemctl start zelie-panel") {
		t.Errorf("ran %v", ran)
	}
	if read(root, DBCopy) != "schema 31" {
		t.Errorf("the copy is %q", read(root, DBCopy))
	}

	// The panel would not stop: it is still the old one, running.
	root = setup(t)
	panelDatabase(t, root)
	os.Remove(filepath.Join(root, Old))
	ran = nil
	f = &Finisher{Root: root, Timeout: time.Second, Now: time.Now, Healthy: func(context.Context, string) error { return errors.New("no") },
		Exec: func(_ context.Context, name string, args ...string) (string, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return "Job for zelie-panel.service canceled.", errors.New("exit status 1")
		},
	}
	res = f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || res.Reason == nil || res.Reason.Code != "update.db_unsaved" || res.Back == nil || res.Back.Code != "update.old_unread" {
		t.Fatalf("%+v %+v", res, res.Back)
	}
	if !slices.Equal(ran, []string{"systemctl stop zelie-panel"}) {
		t.Errorf("ran %v", ran)
	}
}

// Going back after a failed update gives the binary the old file's second
// name instead of a copy of it, so a disk too full for the update is not too
// full for this.
func TestUpdateThatFailsGoesBackWithoutRoom(t *testing.T) {
	root := setup(t)
	f := &Finisher{
		Root: root, Timeout: time.Second, Now: time.Now,
		Exec: func(context.Context, string, ...string) (string, error) { return "", nil },
		Healthy: func(_ context.Context, v string) error {
			if read(root, install.Binary) == "old binary" && v == "v1.2.2" {
				return nil
			}
			return errors.New("no")
		},
	}
	if res := f.Run(context.Background(), "v1.2.2", "v1.2.3"); res.Back == nil || res.Back.Code != "update.back" {
		t.Fatalf("%+v", res)
	}
	bin, _ := os.Stat(filepath.Join(root, install.Binary))
	old, _ := os.Stat(filepath.Join(root, Old))
	if bin == nil || old == nil || !os.SameFile(bin, old) {
		t.Error("the old version was copied back instead of linked")
	}
	if left, _ := filepath.Glob(filepath.Join(root, install.Binary) + ".tmp*"); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

// The panel owns the folder its database is in, so whatever it leaves under
// the database's names must not make the update, which runs as root, copy a
// file from elsewhere into that folder or wait on a pipe.
func TestUpdateRefusesWhatIsNotTheDatabase(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(secret, path string) error
		file  string
	}{
		{"linked database", os.Symlink, ""},
		{"linked log", os.Symlink, "-wal"},
		{"database with another name", os.Link, ""},
		{"pipe for the log", func(_, path string) error { return syscall.Mkfifo(path, 0o600) }, "-wal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := setup(t)
			panelDatabase(t, root)
			secret := filepath.Join(root, "etc", "secret")
			os.MkdirAll(filepath.Dir(secret), 0o755)
			os.WriteFile(secret, []byte("secret"), 0o600)
			planted := filepath.Join(root, install.PanelDB+c.file)
			os.Remove(planted)
			if err := c.plant(secret, planted); err != nil {
				t.Fatal(err)
			}
			f := &Finisher{
				Root: root, Timeout: time.Second, Now: time.Now,
				Exec:    func(context.Context, string, ...string) (string, error) { return "", nil },
				Healthy: func(context.Context, string) error { return nil },
			}
			done := make(chan Result)
			go func() { done <- f.Run(context.Background(), "v1.2.2", "v1.2.3") }()
			var res Result
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the update waits for what it was to copy")
			}
			if res.OK || res.Reason == nil || res.Reason.Code != "update.db_unsaved" {
				t.Fatalf("%+v", res)
			}
			if read(root, install.Binary) != "old binary" {
				t.Errorf("binary %q", read(root, install.Binary))
			}
			for _, dir := range []string{filepath.Dir(install.PanelDB), filepath.Dir(DBCopy)} {
				filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
					if err == nil && d.Type().IsRegular() && path != planted && read("", path) == "secret" {
						t.Errorf("%s holds the contents of a file from elsewhere", path)
					}
					return nil
				})
			}
			for _, left := range []string{DBCopy, DBCopy + "-wal"} {
				if _, err := os.Lstat(filepath.Join(root, left)); err == nil {
					t.Errorf("%s was kept", left)
				}
			}
		})
	}
}

// A server with no panel database yet has nothing to save, and the panel
// does not need stopping for it.
func TestUpdateWithoutADatabase(t *testing.T) {
	root := setup(t)
	var ran []string
	f := &Finisher{
		Root: root, Timeout: time.Second, Now: time.Now,
		Exec: func(_ context.Context, name string, args ...string) (string, error) {
			ran = append(ran, name+" "+strings.Join(args, " "))
			return "", nil
		},
		Healthy: func(context.Context, string) error { return nil },
	}
	if res := f.Run(context.Background(), "v1.2.2", "v1.2.3"); !res.OK {
		t.Fatalf("%+v", res)
	}
	if slices.Contains(ran, "systemctl stop zelie-panel") {
		t.Errorf("ran %v", ran)
	}
}

// A download cut short must not stay in the folder for ever.
func TestPlaceRemovesWhatAnEarlierOneLeft(t *testing.T) {
	root := setup(t)
	dir := filepath.Join(root, filepath.Dir(install.Binary))
	strays := []string{"zelie.tmp-123", "zelie.old.tmp-456"}
	for _, name := range append(strays, "zelie-other") {
		os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600)
	}
	if err := Place(root, []byte("newer binary")); err != nil {
		t.Fatal(err)
	}
	for _, name := range strays {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s is still there", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "zelie-other")); err != nil {
		t.Error("a file that is not Zelie's was removed")
	}
}

func TestRevert(t *testing.T) {
	root := setup(t)
	if err := Revert(root); err != nil || read(root, install.Binary) != "old binary" {
		t.Errorf("%v, binary %q", err, read(root, install.Binary))
	}
	os.Remove(filepath.Join(root, Old))
	if err := Revert(root); err == nil || !strings.Contains(err.Error(), "read back") {
		t.Errorf("without the old binary: %v", err)
	}
}

// A full disk is a likely reason for an update to fail, so going back must
// not need room: the old binary gets a second name.
func TestRevertNeedsNoRoom(t *testing.T) {
	root := setup(t)
	if err := Revert(root); err != nil {
		t.Fatal(err)
	}
	bin, _ := os.Stat(filepath.Join(root, install.Binary))
	old, _ := os.Stat(filepath.Join(root, Old))
	if bin == nil || old == nil || !os.SameFile(bin, old) || read(root, install.Binary) != "old binary" {
		t.Errorf("binary %q is not the old file", read(root, install.Binary))
	}
	// Going back twice leaves nothing behind.
	if err := Revert(root); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(root, filepath.Dir(install.Binary)))
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("%s was left behind", e.Name())
		}
	}
}

// Where the old binary cannot get a second name, it is copied.
func TestRevertCopiesWhenItCannotLink(t *testing.T) {
	root := setup(t)
	// The name the link would take is a folder with something in it.
	blocked := filepath.Join(root, install.Binary+".tmp-link", "x")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Revert(root); err != nil || read(root, install.Binary) != "old binary" {
		t.Errorf("%v, binary %q", err, read(root, install.Binary))
	}
	bin, _ := os.Stat(filepath.Join(root, install.Binary))
	old, _ := os.Stat(filepath.Join(root, Old))
	if os.SameFile(bin, old) {
		t.Error("linked all the same")
	}
}

// Two writers of the same file must each publish a whole one, and leave no
// half-written file behind.
func TestWriteAtomicByTwoAtOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zelie")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- writeAtomic(path, []byte(strings.Repeat(string(rune('a'+i)), 1<<20)))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) != 1<<20 || strings.Count(string(b), string(b[:1])) != len(b) {
		t.Errorf("published %d bytes, %v", len(b), err)
	}
	if left, _ := filepath.Glob(path + ".tmp*"); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", fi.Mode())
	}
}

// A database that cannot be put back is said, and its copy is kept for
// whoever puts it back by hand. Here the new version failed before it
// changed the database, so the old one comes up on it all the same.
func TestUpdateThatCannotPutTheDatabaseBackKeepsTheCopy(t *testing.T) {
	root := setup(t)
	panelDatabase(t, root)
	var ran []string
	_, healthy := server(root, &ran)
	copy := filepath.Join(root, DBCopy)
	f := &Finisher{Root: root, Timeout: 1500 * time.Millisecond, Now: time.Now,
		Exec: func(context.Context, string, ...string) (string, error) { return "", nil },
		Healthy: func(ctx context.Context, version string) error {
			if version == "v1.2.3" {
				os.Chmod(copy, 0)
			}
			return healthy(ctx, version)
		}}
	res := f.Run(context.Background(), "v1.2.2", "v1.2.3")
	if res.OK || res.Back == nil || res.Back.Code != "update.back_without_db" || res.Back.Params["copy"] != DBCopy {
		t.Fatalf("%+v %+v", res, res.Back)
	}
	if read(root, install.Binary) != "old binary" {
		t.Errorf("binary %q", read(root, install.Binary))
	}
	if _, err := os.Stat(copy); err != nil {
		t.Errorf("the copy is gone: %v", err)
	}
}
