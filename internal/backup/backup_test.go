package backup

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/klauspost/compress/zstd"
)

func newDir(t *testing.T) *Dir {
	t.Helper()
	root := t.TempDir()
	k, err := LoadOrCreateKey(filepath.Join(root, "keys", "backup.key"))
	if err != nil {
		t.Fatal(err)
	}
	return &Dir{Root: filepath.Join(root, "backups"), Key: k}
}

func TestKeyIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.key")
	a, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.id.String() != b.id.String() {
		t.Error("a second load made a new key")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", st.Mode().Perm())
	}
}

func TestRoundTrip(t *testing.T) {
	d := newDir(t)
	data := make([]byte, 3<<20)
	rand.Read(data[:1<<20]) // part random, part compressible
	w, err := d.Create("db", "sql")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidName(info.Name) || !strings.HasSuffix(info.Name, ".sql.zst.age") || info.Bytes == 0 {
		t.Fatalf("info %+v", info)
	}
	r, err := d.Open("db", info.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	list, err := d.List("db")
	if err != nil || len(list) != 1 || list[0].Name != info.Name || list[0].Bytes != info.Bytes {
		t.Fatalf("list %+v, %v", list, err)
	}
	if err := d.Remove("db", info.Name); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.List("db"); len(list) != 0 {
		t.Errorf("still listed after remove: %+v", list)
	}
}

// The recovery file must open a backup with age and zstd alone.
func TestRecoveryOpensWithoutZelie(t *testing.T) {
	d := newDir(t)
	w, _ := d.Create("db", "sql")
	io.WriteString(w, "select 1;\n")
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	ids, err := age.ParseIdentities(strings.NewReader(d.Key.Recovery("example.com", time.Now())))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := d.OpenRaw("db", info.Name)
	defer f.Close()
	dec, err := age.Decrypt(f, ids...)
	if err != nil {
		t.Fatal(err)
	}
	z, _ := zstd.NewReader(dec)
	defer z.Close()
	got, _ := io.ReadAll(z)
	if string(got) != "select 1;\n" {
		t.Errorf("got %q", got)
	}
}

func TestAbortLeavesNothing(t *testing.T) {
	d := newDir(t)
	w, _ := d.Create("db", "sql")
	io.WriteString(w, "half a dump")
	w.Abort()
	entries, _ := os.ReadDir(filepath.Join(d.Root, "db"))
	if len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestNamesStayInside(t *testing.T) {
	d := newDir(t)
	for _, app := range []string{"..", "../x", "a/b", "", "A"} {
		if _, err := d.List(app); err == nil {
			t.Errorf("app %q accepted", app)
		}
	}
	for _, name := range []string{"../../etc/passwd", "x.sql", "20260101T000000Z-ab.sql.zst.age/..", ".partial-1"} {
		if _, err := d.Open("db", name); err == nil || ValidName(name) {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestWrongKeyFails(t *testing.T) {
	d, other := newDir(t), newDir(t)
	w, _ := d.Create("db", "sql")
	io.WriteString(w, "secret")
	info, _ := w.Commit()
	other.Root = d.Root
	if _, err := other.Open("db", info.Name); err == nil {
		t.Error("opened with another key")
	}
}

// A backup brought back from off-site storage opens like one made here,
// and a damaged one is refused when it is opened.
func TestImport(t *testing.T) {
	d := newDir(t)
	w, _ := d.Create("db", "sql")
	w.Write([]byte("select 1;"))
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(d.Root, "db", info.Name))

	other := &Dir{Root: t.TempDir(), Key: d.Key}
	if err := other.Import("db", info.Name, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	r, err := other.Open("db", info.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "select 1;" {
		t.Errorf("read back %q", got)
	}
	// Already there: kept as it is.
	if err := other.Import("db", info.Name, strings.NewReader("junk")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(other.Root, "db", info.Name)); !bytes.Equal(b, raw) {
		t.Error("an import replaced a backup")
	}

	damaged := append([]byte(nil), raw...)
	damaged[len(damaged)-5] ^= 1
	name := strings.Replace(info.Name, info.Name[:15], "20260101T000000", 1)
	if err := other.Import("db", name, bytes.NewReader(damaged)); err != nil {
		t.Fatal(err)
	}
	if r, err := other.Open("db", name); err == nil {
		_, err = io.ReadAll(r)
		r.Close()
		if err == nil {
			t.Error("a damaged backup opened")
		}
	}
	for _, bad := range []string{"../x", "x.sql.zst.age", ""} {
		if err := other.Import("db", bad, strings.NewReader("")); err == nil {
			t.Errorf("imported as %q", bad)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(other.Root, "db")); len(entries) != 2 {
		t.Errorf("%d files, want 2", len(entries))
	}
}

// Another server's recovery file, added here, opens that server's backups
// in Zelie, and this server's recovery file then carries it along.
func TestOldKeys(t *testing.T) {
	a, b := newDir(t), newDir(t)
	w, _ := a.Create("db", "sql")
	io.WriteString(w, "from a\n")
	info, err := w.Commit()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(a.Root, "db", info.Name))
	b.Import("db", info.Name, bytes.NewReader(raw))
	if _, err := b.Open("db", info.Name); err == nil {
		t.Fatal("opened with the wrong key")
	}
	if b.Key.Opens(a.Key.Public()) {
		t.Error("says it opens a's backups")
	}

	for _, bad := range []string{"", "# nothing\n", "AGE-SECRET-KEY-1NOTAKEY\n", "hello"} {
		if _, err := b.Key.AddOld(bad); err != ErrNoKey {
			t.Errorf("AddOld(%q): %v", bad, err)
		}
	}
	added, err := b.Key.AddOld(a.Key.Recovery("a.example.com", time.Now()))
	if err != nil || len(added) != 1 || added[0] != a.Key.Public() {
		t.Fatalf("added %v, %v", added, err)
	}
	if again, _ := b.Key.AddOld(a.Key.Recovery("a.example.com", time.Now())); len(again) != 0 {
		t.Errorf("added twice: %v", again)
	}
	if own, _ := b.Key.AddOld(b.Key.Recovery("b.example.com", time.Now())); len(own) != 0 {
		t.Errorf("added its own key: %v", own)
	}
	r, err := b.Open("db", info.Name)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	r.Close()
	if string(got) != "from a\n" || !b.Key.Opens(a.Key.Public()) {
		t.Errorf("read %q", got)
	}

	// Kept across restarts, and new backups still use b's own key.
	reloaded, err := LoadOrCreateKey(b.Key.oldPath[:len(b.Key.oldPath)-len(".old")])
	if err != nil || !reloaded.Opens(a.Key.Public()) || reloaded.Public() != b.Key.Public() {
		t.Fatalf("reloaded: %v", err)
	}
	if st, _ := os.Stat(b.Key.oldPath); st.Mode().Perm() != 0o600 {
		t.Errorf("old keys file mode %v", st.Mode().Perm())
	}
	ids, err := age.ParseIdentities(strings.NewReader(reloaded.Recovery("b.example.com", time.Now())))
	if err != nil || len(ids) != 2 {
		t.Fatalf("recovery file: %d keys, %v", len(ids), err)
	}
	f, _ := os.Open(filepath.Join(b.Root, "db", info.Name))
	defer f.Close()
	if _, err := age.Decrypt(f, ids...); err != nil {
		t.Errorf("b's recovery file does not open a's backup: %v", err)
	}

	sealed, _ := b.Key.Seal([]byte(`{"engine":"postgres"}`))
	if got, err := reloaded.Unseal(sealed, 1<<10); err != nil || string(got) != `{"engine":"postgres"}` {
		t.Errorf("unseal: %q %v", got, err)
	}
	if _, err := a.Key.Unseal(sealed, 1<<10); err == nil {
		t.Error("a unsealed b's data")
	}
}
