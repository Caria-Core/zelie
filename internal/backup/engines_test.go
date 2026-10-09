package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Caria-Core/zelie/internal/msg"
)

// fakeExec answers like a container would: out for the command, and for the
// copy step what arrived, possibly cut short.
type fakeExec struct {
	out      string
	code     uint32
	cut      int // bytes of stdin lost on the way
	got      []byte
	commands [][]string
}

func (f *fakeExec) exec(_ context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	f.commands = append(f.commands, args)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.got = b[:len(b)-f.cut]
		// What wc and sha256sum inside the container would print.
		sum := sha256hex(f.got)
		io.WriteString(stdout, strings.Join([]string{itoa(len(f.got)), sum, restoreFile}, "\n"))
		return 0, nil
	}
	if stdout != nil {
		io.WriteString(stdout, f.out)
	}
	return f.code, nil
}

func TestDumpChecksItsEnd(t *testing.T) {
	var w bytes.Buffer
	f := &fakeExec{out: "CREATE TABLE t();\n-- PostgreSQL database dump complete\n\n"}
	if err := Dump(context.Background(), f.exec, "postgres", &w); err != nil {
		t.Fatal(err)
	}
	if w.String() != f.out {
		t.Errorf("wrote %q", w.String())
	}
	f.out = "CREATE TABLE t();\nCOPY public.t"
	if err := Dump(context.Background(), f.exec, "postgres", io.Discard); err == nil {
		t.Error("a dump without its last line passed")
	}
	f.out, f.code = "-- Dump completed on x\n", 2
	if err := Dump(context.Background(), f.exec, "mariadb", io.Discard); err == nil {
		t.Error("a failed dump passed")
	}
	f.out, f.code = "Transfer finished", 0
	if err := Dump(context.Background(), f.exec, "redis", io.Discard); err == nil {
		t.Error("redis-cli's messages passed as an RDB file")
	}
	if err := Dump(context.Background(), f.exec, "mongodb", io.Discard); err == nil {
		t.Error("unknown kind passed")
	}
}

func TestLoadNeedsTheWholeFile(t *testing.T) {
	dump := strings.Repeat("insert into t values (1);\n", 1000)
	f := &fakeExec{}
	if err := Load(context.Background(), f.exec, "postgres", strings.NewReader(dump)); err != nil {
		t.Fatal(err)
	}
	if string(f.got) != dump || len(f.commands) != 2 || !strings.Contains(f.commands[1][2], "DROP DATABASE") {
		t.Fatalf("commands %q", f.commands)
	}

	f = &fakeExec{cut: 10}
	err := Load(context.Background(), f.exec, "mariadb", strings.NewReader(dump))
	if err == nil || !strings.Contains(err.Error(), "whole") {
		t.Fatalf("a cut copy gave %v", err)
	}
	// Nothing was dropped: only the copy and its clean-up ran.
	for _, c := range f.commands {
		if strings.Contains(strings.Join(c, " "), "DROP") {
			t.Errorf("ran %q after a bad copy", c)
		}
	}
	if err := Load(context.Background(), f.exec, "redis", strings.NewReader("REDIS")); err == nil {
		t.Error("redis loaded in place")
	}
}

// A backup that cannot be read to its end (a flipped bit, a truncated copy)
// reaches the container as a shorter file that passes the count and checksum
// of what arrived. It must not be loaded.
func TestLoadRefusesABrokenBackup(t *testing.T) {
	dump := strings.Repeat("insert into t values (1);\n", 1000)
	broken := io.MultiReader(strings.NewReader(dump[:len(dump)/2]), iotest.ErrReader(errors.New("failed to decrypt and authenticate payload chunk")))
	f := &fakeExec{}
	err := Load(context.Background(), f.exec, "postgres", broken)
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != "restore.damaged" || !strings.Contains(me.Text, "payload chunk") {
		t.Fatalf("a broken backup gave %v", err)
	}
	// Nothing was dropped, and the half that was copied in is removed.
	for _, c := range f.commands {
		if strings.Contains(strings.Join(c, " "), "DROP") {
			t.Errorf("ran %q after a broken backup", c)
		}
	}
	if last := f.commands[len(f.commands)-1]; len(last) < 3 || last[0] != "rm" || last[2] != restoreFile {
		t.Errorf("the copy was left in the container: %q", f.commands)
	}
}

// The load drops the database before it fills it, so it runs to its end even
// when the request that asked for it is gone; before it starts, a request
// that is gone stops it.
func TestLoadIsNotCutOffOnceItDrops(t *testing.T) {
	dump := "insert into t values (1);\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var loaded bool
	exec := func(c context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			io.WriteString(stdout, strings.Join([]string{itoa(len(b)), sha256hex(b), restoreFile}, "\n"))
			cancel() // the panel went away, or the core is stopping
			return 0, nil
		}
		if strings.Contains(strings.Join(args, " "), "DROP DATABASE") {
			loaded = true
		}
		return 0, nil
	}
	if err := Load(ctx, exec, "postgres", strings.NewReader(dump)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a request that went away before the load: %v", err)
	}
	if loaded {
		t.Fatal("the database was dropped for a request that was gone")
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	exec = func(c context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
		if stdin != nil {
			b, _ := io.ReadAll(stdin)
			io.WriteString(stdout, strings.Join([]string{itoa(len(b)), sha256hex(b), restoreFile}, "\n"))
			return 0, nil
		}
		cancel() // gone while the load runs
		return 0, c.Err()
	}
	if err := Load(ctx, exec, "postgres", strings.NewReader(dump)); err != nil {
		t.Errorf("a load cut off by its request going away: %v", err)
	}
}

func TestRestoreRedis(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "appendonlydir"), 0o700)
	os.WriteFile(filepath.Join(dir, "appendonlydir", "appendonly.aof.3.base.rdb"), []byte("REDIS-old"), 0o600)
	os.WriteFile(filepath.Join(dir, "dump.rdb"), []byte("REDIS-older"), 0o600)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := RestoreRedis(root, strings.NewReader("REDIS0012-new")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "appendonlydir", "appendonly.aof.1.base.rdb"))
	manifest, _ := os.ReadFile(filepath.Join(dir, "appendonlydir", "appendonly.aof.manifest"))
	if string(got) != "REDIS0012-new" || !strings.Contains(string(manifest), "appendonly.aof.1.base.rdb seq 1 type b") {
		t.Errorf("base %q, manifest %q", got, manifest)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("left %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, "appendonlydir", "appendonly.aof.3.base.rdb")); err == nil {
		t.Error("old files kept")
	}

	// Not an RDB file: nothing changes.
	if err := RestoreRedis(root, strings.NewReader("<html>")); err == nil {
		t.Error("accepted a non-RDB file")
	}
	got, _ = os.ReadFile(filepath.Join(dir, "appendonlydir", "appendonly.aof.1.base.rdb"))
	if string(got) != "REDIS0012-new" {
		t.Errorf("a failed restore changed the data: %q", got)
	}
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func itoa(n int) string { return strconv.Itoa(n) }
