package core

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Caria-Core/zelie/internal/peer"
)

// fileEnv is a volume with a sibling folder next to it, which is where a
// path that escapes would land.
type fileEnv struct {
	t       *testing.T
	s       *Server
	vol     string // the volume's directory
	outside string // a directory beside it, with a file in it
	secret  string // a file beside it
}

func newFileEnv(t *testing.T) *fileEnv {
	t.Helper()
	base := t.TempDir()
	e := &fileEnv{t: t, vol: filepath.Join(base, "vol"), outside: filepath.Join(base, "outside"), secret: filepath.Join(base, "secret.txt")}
	for _, d := range []string{e.vol, e.outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(e.secret, []byte("top secret"), 0o600)
	os.WriteFile(filepath.Join(e.outside, "x"), []byte("outside x"), 0o600)
	s, f := newServer()
	f.volumeDirs = map[string]string{"srv-vol": e.vol}
	e.s = s
	return e
}

func (e *fileEnv) owner() FileOwner {
	return FileOwner{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
}

func (e *fileEnv) query(kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		q.Set(kv[i], kv[i+1])
	}
	q.Set("uid", strconv.Itoa(os.Getuid()))
	q.Set("gid", strconv.Itoa(os.Getgid()))
	return q.Encode()
}

func (e *fileEnv) do(method, op, query, body string) (int, string) {
	e.t.Helper()
	rec := request(e.t, e.s, &peer.Peer{UID: 0}, method, "/v1/volumes/srv-vol/files/"+op+"?"+query, body)
	return rec.Code, rec.Body.String()
}

// post sends a JSON request, with the owner filled in.
func (e *fileEnv) post(op string, req map[string]any) (int, string) {
	e.t.Helper()
	if op == "mkdir" || op == "compress" || op == "extract" {
		req["uid"], req["gid"] = os.Getuid(), os.Getgid()
	}
	b, _ := json.Marshal(req)
	code, out := e.do("POST", op, "", string(b))
	return code, out
}

func fileCode(body string) string {
	var m struct{ Code string }
	json.Unmarshal([]byte(body), &m)
	return m.Code
}

func (e *fileEnv) write(name, content string) {
	e.t.Helper()
	p := filepath.Join(e.vol, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *fileEnv) noTemp() {
	e.t.Helper()
	filepath.WalkDir(e.vol, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".zelie-") {
			e.t.Errorf("left a temporary file: %s", p)
		}
		return nil
	})
}

// untouched checks nothing beside the volume was made, changed or removed.
func (e *fileEnv) untouched() {
	e.t.Helper()
	if b, _ := os.ReadFile(e.secret); string(b) != "top secret" {
		e.t.Errorf("the file beside the volume is now %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(e.outside, "x")); string(b) != "outside x" {
		e.t.Errorf("a file in the folder beside the volume is now %q", b)
	}
	entries, _ := os.ReadDir(e.outside)
	if len(entries) != 1 {
		e.t.Errorf("the folder beside the volume holds %d items", len(entries))
	}
	entries, _ = os.ReadDir(filepath.Dir(e.vol))
	if len(entries) != 3 {
		e.t.Errorf("the volume's parent holds %d items, want 3", len(entries))
	}
}

func TestFilesNeverLeaveTheVolume(t *testing.T) {
	e := newFileEnv(t)
	e.write("plain.txt", "plain")
	os.Symlink(e.outside, filepath.Join(e.vol, "etc"))
	os.Symlink("../secret.txt", filepath.Join(e.vol, "rel"))
	os.Symlink("/etc/hosts", filepath.Join(e.vol, "abs"))
	os.Symlink("..", filepath.Join(e.vol, "up"))
	os.Symlink("loop2", filepath.Join(e.vol, "loop1"))
	os.Symlink("loop1", filepath.Join(e.vol, "loop2"))

	// Each is refused or finds nothing, and none of it reaches the outside.
	refused := func(what string, code int, body string) {
		t.Helper()
		if code < 400 || code >= 500 {
			t.Errorf("%s: %d %s", what, code, body)
		}
	}
	for _, p := range []string{"etc/x", "rel", "abs", "up/secret.txt", "loop1", "../secret.txt", "a/../../secret.txt", "..", "etc/../../secret.txt"} {
		code, body := e.do("GET", "content", e.query("path", p), "")
		refused("read "+p, code, body)
		code, body = e.do("GET", "download", e.query("path", p), "")
		refused("download "+p, code, body)
		code, body = e.do("PUT", "content", e.query("path", p), "changed")
		if code == http.StatusNoContent && !slices.Contains([]string{"rel", "abs", "loop1"}, p) {
			t.Errorf("write %s was accepted", p)
		}
		code, body = e.do("PUT", "upload", e.query("path", p), "changed")
		if code == http.StatusNoContent && !slices.Contains([]string{"rel", "abs", "loop1"}, p) {
			t.Errorf("upload %s was accepted", p)
		}
		code, body = e.post("list", map[string]any{"path": p})
		if code == http.StatusOK && !slices.Contains([]string{"up", "abs"}, p) {
			t.Errorf("list %s: %d %s", p, code, body)
		}
	}
	e.untouched()

	// Writing where a link is replaces the link with a file. What it pointed
	// at stays as it was.
	code, _ := e.do("PUT", "content", e.query("path", "rel"), "mine")
	if code != http.StatusNoContent {
		t.Errorf("write over a link: %d", code)
	}
	if st, _ := os.Lstat(filepath.Join(e.vol, "rel")); !st.Mode().IsRegular() {
		t.Errorf("the link is still a link: %v", st.Mode())
	}
	e.untouched()

	for name, req := range map[string]map[string]any{
		"mkdir through a link":  {"path": "etc/new"},
		"mkdir up":              {"path": "../new"},
		"mkdir through up link": {"path": "up/new"},
	} {
		if code, body := e.post("mkdir", req); code < 400 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	for name, req := range map[string]map[string]any{
		"rename out":          {"from": "plain.txt", "to": "../plain.txt"},
		"rename out by link":  {"from": "plain.txt", "to": "etc/plain.txt"},
		"rename in":           {"from": "etc/x", "to": "x"},
		"rename secret":       {"from": "../secret.txt", "to": "s"},
		"rename secret alias": {"from": "up/secret.txt", "to": "s"},
	} {
		if code, body := e.post("rename", req); code < 400 {
			t.Errorf("%s: %d %s", name, code, body)
		}
	}
	e.untouched()

	// Deleting a link removes the link. Deleting through it reaches nothing.
	if code, body := e.post("delete", map[string]any{"paths": []string{"etc/x"}}); code < 400 {
		t.Errorf("delete through a link: %d %s", code, body)
	}
	if code, body := e.post("delete", map[string]any{"paths": []string{"up/secret.txt"}}); code < 400 {
		t.Errorf("delete through a link up: %d %s", code, body)
	}
	e.untouched()
	for _, p := range []string{"etc", "abs", "up", "loop1", "loop2"} {
		if code, body := e.post("delete", map[string]any{"paths": []string{p}}); code != http.StatusNoContent {
			t.Errorf("delete link %s: %d %s", p, code, body)
		}
		if _, err := os.Lstat(filepath.Join(e.vol, p)); err == nil {
			t.Errorf("%s is still there", p)
		}
	}
	e.untouched()
}

// A path with an absolute start is the volume's own, never the host's.
func TestAbsolutePathsAreTheVolumesOwn(t *testing.T) {
	e := newFileEnv(t)
	e.write("etc/passwd", "mine")
	code, body := e.do("GET", "content", e.query("path", "/etc/passwd"), "")
	if code != http.StatusOK || body != "mine" {
		t.Errorf("read: %d %q", code, body)
	}
	code, _ = e.do("PUT", "content", e.query("path", "/etc/hostname"), "made")
	if code != http.StatusNoContent {
		t.Errorf("write: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "etc", "hostname")); string(b) != "made" {
		t.Errorf("written: %q", b)
	}
	if b, _ := os.ReadFile("/etc/hostname"); string(b) == "made" {
		t.Error("the host's file was written")
	}
}

// A link swapped for a folder, or the other way, between one step and the
// next is what a player who can run code in the game could try. The root
// resolves each step against the volume, so the file never lands outside.
func TestLinkSwappedWhileWriting(t *testing.T) {
	e := newFileEnv(t)
	swap := filepath.Join(e.vol, "swap")
	os.Mkdir(swap, 0o755)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			os.RemoveAll(swap)
			os.Symlink(e.outside, swap)
			os.Remove(swap)
			os.Mkdir(swap, 0o755)
		}
	}()
	deadline := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(deadline) {
		e.do("PUT", "content", e.query("path", "swap/file"), "x")
		e.do("PUT", "upload", e.query("path", "swap/up"), "x")
		e.post("mkdir", map[string]any{"path": "swap/dir"})
	}
	close(stop)
	wg.Wait()
	entries, _ := os.ReadDir(e.outside)
	if len(entries) != 1 {
		t.Errorf("something reached the folder beside the volume: %v", entries)
	}
}

func TestListFolder(t *testing.T) {
	e := newFileEnv(t)
	e.write("b.txt", "bb")
	e.write("A.txt", "a")
	e.write("zdir/inner", "i")
	e.write("Adir/inner", "i")
	os.Symlink("b.txt", filepath.Join(e.vol, "link"))
	os.Symlink(e.outside, filepath.Join(e.vol, "away"))
	os.Chmod(filepath.Join(e.vol, "b.txt"), 0o640)

	code, body := e.post("list", map[string]any{"path": ""})
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var out FileList
	json.Unmarshal([]byte(body), &out)
	var names []string
	for _, en := range out.Entries {
		names = append(names, en.Name)
	}
	if want := []string{"Adir", "zdir", "A.txt", "away", "b.txt", "link"}; !slices.Equal(names, want) {
		t.Errorf("order %v, want %v", names, want)
	}
	byName := map[string]FileEntry{}
	for _, en := range out.Entries {
		byName[en.Name] = en
	}
	if l := byName["link"]; !l.Symlink || l.Target != "b.txt" || l.Dir {
		t.Errorf("link: %+v", l)
	}
	// A link to a folder is shown as a link, not followed, even when its
	// target is outside.
	if l := byName["away"]; !l.Symlink || l.Dir || l.Target != e.outside {
		t.Errorf("away: %+v", l)
	}
	if f := byName["b.txt"]; f.Size != 2 || f.Mode != "-rw-r-----" || f.Dir || f.Modified.IsZero() {
		t.Errorf("file: %+v", f)
	}
	if !byName["zdir"].Dir {
		t.Errorf("zdir: %+v", byName["zdir"])
	}

	if code, body = e.post("list", map[string]any{"path": "/zdir/"}); code != http.StatusOK || !strings.Contains(body, `"path":"zdir"`) {
		t.Errorf("list zdir: %d %s", code, body)
	}
	if code, body = e.post("list", map[string]any{"path": "b.txt"}); code != http.StatusConflict || fileCode(body) != "files.not_folder" {
		t.Errorf("list a file: %d %s", code, body)
	}
	if code, body = e.post("list", map[string]any{"path": "nope"}); code != http.StatusNotFound || fileCode(body) != "files.not_found" {
		t.Errorf("list missing: %d %s", code, body)
	}
	if code, body = e.post("list", map[string]any{"path": "away"}); code < 400 {
		t.Errorf("list a link out: %d %s", code, body)
	}
}

func TestReadAndWriteText(t *testing.T) {
	e := newFileEnv(t)
	e.write("server.properties", "a=1\n")
	os.Chmod(filepath.Join(e.vol, "server.properties"), 0o640)

	code, body := e.do("GET", "content", e.query("path", "server.properties"), "")
	if code != http.StatusOK || body != "a=1\n" {
		t.Fatalf("read: %d %q", code, body)
	}
	if code, _ = e.do("PUT", "content", e.query("path", "server.properties"), "a=2\n"); code != http.StatusNoContent {
		t.Fatalf("write: %d", code)
	}
	st, _ := os.Stat(filepath.Join(e.vol, "server.properties"))
	if b, _ := os.ReadFile(filepath.Join(e.vol, "server.properties")); string(b) != "a=2\n" || st.Mode().Perm() != 0o640 {
		t.Errorf("after write: %q %v", b, st.Mode())
	}
	if s := st.Sys().(*syscall.Stat_t); int(s.Uid) != os.Getuid() || int(s.Gid) != os.Getgid() {
		t.Errorf("owner %d:%d", s.Uid, s.Gid)
	}

	// New files refuse a name that is taken.
	if code, body = e.do("PUT", "content", e.query("path", "server.properties", "new", "1"), "x"); code != http.StatusConflict || fileCode(body) != "files.exists" {
		t.Errorf("new over old: %d %s", code, body)
	}
	if code, _ = e.do("PUT", "content", e.query("path", "fresh.txt", "new", "1"), ""); code != http.StatusNoContent {
		t.Errorf("new: %d", code)
	}
	if st, err := os.Stat(filepath.Join(e.vol, "fresh.txt")); err != nil || st.Size() != 0 || st.Mode().Perm() != 0o644 {
		t.Errorf("fresh: %v %v", st, err)
	}
	// The folder must be there.
	if code, body = e.do("PUT", "content", e.query("path", "no/such/file"), "x"); code != http.StatusNotFound {
		t.Errorf("no folder: %d %s", code, body)
	}
	e.write("dir/f", "f")
	if code, body = e.do("PUT", "content", e.query("path", "dir"), "x"); code != http.StatusConflict {
		t.Errorf("write over a folder: %d %s", code, body)
	}
	if code, body = e.do("GET", "content", e.query("path", "dir"), ""); code != http.StatusConflict || fileCode(body) != "files.not_file" {
		t.Errorf("read a folder: %d %s", code, body)
	}
	if code, _ = e.do("PUT", "content", e.query("path", ""), "x"); code != http.StatusBadRequest {
		t.Errorf("write the top: %d", code)
	}
	// No owner, no write.
	if code, _ = e.do("PUT", "content", "path=x", "x"); code != http.StatusBadRequest {
		t.Errorf("no owner: %d", code)
	}
	e.noTemp()
}

func TestTextLimitsAndBinary(t *testing.T) {
	e := newFileEnv(t)
	e.write("bin", "abc\x00def")
	e.write("latin1", "caf\xe9")
	e.write("late-nul", strings.Repeat("a", sniffBytes+10)+"\x00")
	e.write("big", strings.Repeat("a", MaxEditBytes+1))
	e.write("edge", strings.Repeat("a", MaxEditBytes))
	e.write("utf8", "héllo ✓")
	for _, p := range []string{"bin", "latin1"} {
		if code, body := e.do("GET", "content", e.query("path", p), ""); code != http.StatusUnprocessableEntity || fileCode(body) != "files.binary" {
			t.Errorf("read %s: %d %s", p, code, body)
		}
	}
	// A NUL further in still fails the UTF-8 check?  It is valid UTF-8, so
	// it is text as far as the editor can tell; only the start is sniffed.
	if code, _ := e.do("GET", "content", e.query("path", "late-nul"), ""); code != http.StatusOK {
		t.Errorf("late NUL: %d", code)
	}
	if code, body := e.do("GET", "content", e.query("path", "big"), ""); code != http.StatusRequestEntityTooLarge || fileCode(body) != "files.too_large" {
		t.Errorf("read big: %d %s", code, body)
	}
	if code, _ := e.do("GET", "content", e.query("path", "edge"), ""); code != http.StatusOK {
		t.Errorf("read at the limit: %d", code)
	}
	if code, body := e.do("GET", "content", e.query("path", "utf8"), ""); code != http.StatusOK || body != "héllo ✓" {
		t.Errorf("utf8: %d %q", code, body)
	}
	if code, body := e.do("PUT", "content", e.query("path", "out"), strings.Repeat("a", MaxEditBytes+1)); code != http.StatusRequestEntityTooLarge || fileCode(body) != "files.too_large" {
		t.Errorf("write big: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(e.vol, "out")); err == nil {
		t.Error("a file was made from a request that was too big")
	}
	if code, _ := e.do("PUT", "content", e.query("path", "out"), strings.Repeat("a", MaxEditBytes)); code != http.StatusNoContent {
		t.Errorf("write at the limit: %d", code)
	}
	// A named pipe does not hold the request up.
	syscall.Mkfifo(filepath.Join(e.vol, "pipe"), 0o644)
	if code, body := e.do("GET", "content", e.query("path", "pipe"), ""); code != http.StatusConflict || fileCode(body) != "files.not_file" {
		t.Errorf("read a pipe: %d %s", code, body)
	}
	e.noTemp()
}

func TestMakeRenameDelete(t *testing.T) {
	e := newFileEnv(t)
	e.write("a/one", "1")
	e.write("a/deep/two", "2")
	e.write("b", "b")

	if code, body := e.post("mkdir", map[string]any{"path": "new"}); code != http.StatusNoContent {
		t.Fatalf("mkdir: %d %s", code, body)
	}
	st, _ := os.Stat(filepath.Join(e.vol, "new"))
	if !st.IsDir() || st.Mode().Perm() != 0o755 || int(st.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		t.Errorf("new folder: %v", st.Mode())
	}
	if code, body := e.post("mkdir", map[string]any{"path": "new"}); code != http.StatusConflict || fileCode(body) != "files.exists" {
		t.Errorf("mkdir again: %d %s", code, body)
	}
	if code, body := e.post("mkdir", map[string]any{"path": "x/y"}); code != http.StatusNotFound {
		t.Errorf("mkdir without parent: %d %s", code, body)
	}
	if code, body := e.post("mkdir", map[string]any{"path": "/"}); code != http.StatusConflict {
		t.Errorf("mkdir top: %d %s", code, body)
	}

	if code, body := e.post("rename", map[string]any{"from": "b", "to": "a/one"}); code != http.StatusConflict || fileCode(body) != "files.exists" {
		t.Errorf("rename onto a file: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "a/one")); string(b) != "1" {
		t.Errorf("a file was replaced: %q", b)
	}
	if code, body := e.post("rename", map[string]any{"from": "a", "to": "a/deep/inner"}); code != http.StatusBadRequest || fileCode(body) != "files.move_inside" {
		t.Errorf("rename into itself: %d %s", code, body)
	}
	if code, body := e.post("rename", map[string]any{"from": "", "to": "top"}); code != http.StatusBadRequest || fileCode(body) != "files.root" {
		t.Errorf("rename the top: %d %s", code, body)
	}
	if code, body := e.post("rename", map[string]any{"from": "missing", "to": "z"}); code != http.StatusNotFound {
		t.Errorf("rename missing: %d %s", code, body)
	}
	if code, body := e.post("rename", map[string]any{"from": "a", "to": "new/moved"}); code != http.StatusNoContent {
		t.Errorf("move a folder: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "new/moved/deep/two")); string(b) != "2" {
		t.Errorf("moved content: %q", b)
	}

	if code, body := e.post("delete", map[string]any{"paths": []string{}}); code != http.StatusBadRequest {
		t.Errorf("delete nothing: %d %s", code, body)
	}
	if code, body := e.post("delete", map[string]any{"paths": []string{"b", "/"}}); code != http.StatusBadRequest || fileCode(body) != "files.root" {
		t.Errorf("delete the top: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(e.vol, "b")); err != nil {
		t.Error("b went although the request was refused")
	}
	if code, body := e.post("delete", map[string]any{"paths": []string{"b", "new", "gone-already"}}); code != http.StatusNoContent {
		t.Errorf("delete: %d %s", code, body)
	}
	entries, _ := os.ReadDir(e.vol)
	if len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestUpload(t *testing.T) {
	e := newFileEnv(t)
	e.write("dir/old.bin", "old")
	os.Chmod(filepath.Join(e.vol, "dir/old.bin"), 0o755)
	payload := strings.Repeat("0123456789", 100_000)
	if code, body := e.do("PUT", "upload", e.query("path", "dir/new.bin"), payload); code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "dir/new.bin")); string(b) != payload {
		t.Errorf("uploaded %d bytes", len(b))
	}
	st, _ := os.Stat(filepath.Join(e.vol, "dir/new.bin"))
	if int(st.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		t.Error("the upload has another owner")
	}
	// It replaces a file and keeps its mode.
	if code, _ := e.do("PUT", "upload", e.query("path", "dir/old.bin"), "new"); code != http.StatusNoContent {
		t.Errorf("overwrite: %d", code)
	}
	if st, _ := os.Stat(filepath.Join(e.vol, "dir/old.bin")); st.Size() != 3 || st.Mode().Perm() != 0o755 {
		t.Errorf("overwritten: %v %d", st.Mode(), st.Size())
	}
	if code, body := e.do("PUT", "upload", e.query("path", "dir"), "x"); code != http.StatusConflict {
		t.Errorf("upload over a folder: %d %s", code, body)
	}
	if code, body := e.do("PUT", "upload", e.query("path", "nofolder/f"), "x"); code != http.StatusNotFound {
		t.Errorf("upload without a folder: %d %s", code, body)
	}

	// The volume's disk limit: what is left is what fits, and nothing of a
	// file that does not fit stays behind.
	if code, body := e.do("PUT", "upload", e.query("path", "big", "room", "1000"), payload); code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("over the room: %d %s", code, body)
	}
	if code, _ := e.do("PUT", "upload", e.query("path", "fits", "room", "1000"), strings.Repeat("a", 1000)); code != http.StatusNoContent {
		t.Errorf("at the room: %d", code)
	}
	if code, body := e.do("PUT", "upload", e.query("path", "none", "room", "0"), "x"); code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("no room: %d %s", code, body)
	}
	for _, n := range []string{"big", "none"} {
		if _, err := os.Stat(filepath.Join(e.vol, n)); err == nil {
			t.Errorf("%s was kept", n)
		}
	}
	e.noTemp()
}

func TestDownload(t *testing.T) {
	e := newFileEnv(t)
	e.write("dir/world.dat", "\x00\x01binary\xff")
	rec := request(t, e.s, &peer.Peer{UID: 0}, "GET", "/v1/volumes/srv-vol/files/download?"+e.query("path", "dir/world.dat"), "")
	if rec.Code != http.StatusOK || rec.Body.String() != "\x00\x01binary\xff" || rec.Header().Get("Content-Length") != "9" {
		t.Errorf("download: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	if code, _ := e.do("GET", "download", e.query("path", "dir"), ""); code != http.StatusConflict {
		t.Errorf("download a folder: %d", code)
	}
}

func TestFilesInAVolumeThatIsUsed(t *testing.T) {
	// Unlike prepare, the file manager works while the game runs.
	e := newFileEnv(t)
	e.s.Engine.(*fakeEngine).usedVolumes = map[string]bool{"srv-vol": true}
	e.write("f", "1")
	if code, _ := e.do("PUT", "content", e.query("path", "f"), "2"); code != http.StatusNoContent {
		t.Errorf("write to a used volume: %d", code)
	}
	if code, body := e.post("list", map[string]any{"path": ""}); code != http.StatusOK {
		t.Errorf("list a used volume: %d %s", code, body)
	}
	if code, _ := request(t, e.s, &peer.Peer{UID: 0}, "POST", "/v1/volumes/nope/files/list", `{"path":""}`).Code, 0; code != http.StatusNotFound {
		t.Errorf("unknown volume: %d", code)
	}
	if code, _ := request(t, e.s, &peer.Peer{UID: 0}, "POST", "/v1/volumes/Bad_Name/files/list", `{"path":""}`).Code, 0; code != http.StatusBadRequest {
		t.Errorf("bad volume name: %d", code)
	}
	if rec := request(t, e.s, &peer.Peer{UID: 4242}, "POST", "/v1/volumes/srv-vol/files/list", `{"path":""}`); rec.Code == http.StatusOK {
		t.Errorf("a stranger listed the files: %d", rec.Code)
	}
}

type tarEntry struct {
	hdr  tar.Header
	body string
}

func makeTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, en := range entries {
		h := en.hdr
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(en.body))
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(en.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func (e *fileEnv) extract(archive, dir string, room *int64) (int, string) {
	req := map[string]any{"path": archive, "dir": dir}
	if room != nil {
		req["room"] = *room
	}
	return e.post("extract", req)
}

func TestCompressAndExtractRoundTrip(t *testing.T) {
	e := newFileEnv(t)
	e.write("world/level.dat", "level")
	e.write("world/region/r.0.0", strings.Repeat("r", 5000))
	e.write("server.properties", "a=1")
	os.Chmod(filepath.Join(e.vol, "server.properties"), 0o755)
	os.Symlink("level.dat", filepath.Join(e.vol, "world/latest"))
	os.Symlink(e.outside, filepath.Join(e.vol, "world/out"))
	syscall.Mkfifo(filepath.Join(e.vol, "world/pipe"), 0o644)

	code, body := e.post("compress", map[string]any{"dir": "", "paths": []string{"world", "server.properties"}, "name": "backup"})
	if code != http.StatusOK || !strings.Contains(body, `"backup.tar.gz"`) {
		t.Fatalf("compress: %d %s", code, body)
	}
	arc := filepath.Join(e.vol, "backup.tar.gz")
	f, _ := os.Open(arc)
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]tar.Header{}
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		got[h.Name] = *h
		if h.Uid != 0 || h.Gid != 0 || h.Uname != "" {
			t.Errorf("%s carries an owner: %+v", h.Name, h)
		}
	}
	f.Close()
	for _, n := range []string{"world/", "world/level.dat", "world/region/r.0.0", "world/latest", "world/out", "server.properties"} {
		if _, ok := got[n]; !ok {
			t.Errorf("%s is not in the archive: %v", n, slices.Sorted(mapKeys(got)))
		}
	}
	if h := got["world/out"]; h.Typeflag != tar.TypeSymlink || h.Linkname != e.outside {
		t.Errorf("a link is kept as a link: %+v", h)
	}
	if _, ok := got["world/pipe"]; ok {
		t.Error("a named pipe was packed")
	}
	if _, ok := got["backup.tar.gz"]; ok {
		t.Error("the archive holds itself")
	}
	if st, _ := os.Stat(arc); int(st.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		t.Error("the archive has another owner")
	}
	e.noTemp()

	// Same name again: refused, and the first stays.
	if code, body = e.post("compress", map[string]any{"dir": "", "paths": []string{"server.properties"}, "name": "backup.tar.gz"}); code != http.StatusConflict {
		t.Errorf("compress over an archive: %d %s", code, body)
	}
	// Without a name one is made up.
	if code, body = e.post("compress", map[string]any{"dir": "world", "paths": []string{"world/level.dat"}}); code != http.StatusOK || !strings.HasPrefix(body, `{"name":"archive-`) {
		t.Errorf("compress unnamed: %d %s", code, body)
	}
	// Items must be in the folder named.
	for name, req := range map[string]map[string]any{
		"elsewhere": {"dir": "world", "paths": []string{"server.properties"}},
		"the top":   {"dir": "", "paths": []string{""}},
		"escape":    {"dir": "", "paths": []string{"../secret.txt"}},
		"bad name":  {"dir": "", "paths": []string{"server.properties"}, "name": "a/b"},
		"none":      {"dir": "", "paths": []string{}},
		"missing":   {"dir": "", "paths": []string{"nope"}},
	} {
		if code, body := e.post("compress", req); code < 400 {
			t.Errorf("compress %s: %d %s", name, code, body)
		}
	}
	// No room: nothing is left.
	room := int64(50)
	if code, body = e.post("compress", map[string]any{"dir": "", "paths": []string{"world"}, "name": "small", "room": room}); code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("compress without room: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(e.vol, "small.tar.gz")); err == nil {
		t.Error("a partial archive was kept")
	}
	e.noTemp()

	// The link that points outside is refused when the archive is unpacked.
	os.Mkdir(filepath.Join(e.vol, "restore"), 0o755)
	if code, body = e.extract("backup.tar.gz", "restore", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_link" {
		t.Errorf("extract a link out: %d %s", code, body)
	}
	e.untouched()

	// Without it, all comes back.
	os.Remove(filepath.Join(e.vol, "world/out"))
	os.RemoveAll(filepath.Join(e.vol, "restore"))
	os.Mkdir(filepath.Join(e.vol, "restore"), 0o755)
	if code, body = e.post("compress", map[string]any{"dir": "", "paths": []string{"world", "server.properties"}, "name": "backup2"}); code != http.StatusOK {
		t.Fatalf("compress again: %d %s", code, body)
	}
	code, body = e.extract("backup2.tar.gz", "restore", nil)
	if code != http.StatusOK {
		t.Fatalf("extract: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "restore/world/region/r.0.0")); string(b) != strings.Repeat("r", 5000) {
		t.Errorf("unpacked file: %d bytes", len(b))
	}
	if l, _ := os.Readlink(filepath.Join(e.vol, "restore/world/latest")); l != "level.dat" {
		t.Errorf("relative link: %q", l)
	}
	e.untouched()
	if st, _ := os.Stat(filepath.Join(e.vol, "restore/server.properties")); st == nil || st.Mode().Perm() != 0o755 {
		t.Errorf("mode %v", st)
	}
}

func mapKeys[M ~map[K]V, K comparable, V any](m M) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestExtractRefusesWhatEscapes(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	e.write("into/keep", "keep")
	cases := map[string]struct {
		entries []tarEntry
		code    string
	}{
		"dotdot":         {[]tarEntry{{hdr: tar.Header{Name: "../evil"}, body: "x"}}, "files.archive_escape"},
		"deep dotdot":    {[]tarEntry{{hdr: tar.Header{Name: "a/../../../evil"}, body: "x"}}, "files.archive_escape"},
		"absolute":       {[]tarEntry{{hdr: tar.Header{Name: "/etc/evil"}, body: "x"}}, "files.archive_escape"},
		"dotdot dir":     {[]tarEntry{{hdr: tar.Header{Name: "../evil/", Typeflag: tar.TypeDir}}}, "files.archive_escape"},
		"absolute link":  {[]tarEntry{{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "/etc"}}}, "files.archive_link"},
		"relative out":   {[]tarEntry{{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "../.."}}}, "files.archive_link"},
		"nested out":     {[]tarEntry{{hdr: tar.Header{Name: "a/b/l", Typeflag: tar.TypeSymlink, Linkname: "../../../x"}}}, "files.archive_link"},
		"empty link":     {[]tarEntry{{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink}}}, "files.archive_link"},
		"hardlink out":   {[]tarEntry{{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "../secret.txt"}}}, "files.archive_link"},
		"hardlink abs":   {[]tarEntry{{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}}}, "files.archive_link"},
		"hardlink to it": {[]tarEntry{{hdr: tar.Header{Name: "h", Typeflag: tar.TypeLink, Linkname: "nothing"}}}, "files.archive_link"},
		"char device":    {[]tarEntry{{hdr: tar.Header{Name: "null", Typeflag: tar.TypeChar, Devmajor: 1, Devminor: 3}}}, "files.archive_special"},
		"block device":   {[]tarEntry{{hdr: tar.Header{Name: "sda", Typeflag: tar.TypeBlock}}}, "files.archive_special"},
		"fifo":           {[]tarEntry{{hdr: tar.Header{Name: "p", Typeflag: tar.TypeFifo}}}, "files.archive_special"},
		// A link made by the archive is never written through, even one that
		// stays inside the folder.
		"through a link": {[]tarEntry{
			{hdr: tar.Header{Name: "d", Typeflag: tar.TypeDir}},
			{hdr: tar.Header{Name: "d/up", Typeflag: tar.TypeSymlink, Linkname: ".."}},
			{hdr: tar.Header{Name: "d/up/file"}, body: "x"},
		}, "files.archive_link"},
		"through a link 2": {[]tarEntry{
			{hdr: tar.Header{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "."}},
			{hdr: tar.Header{Name: "l/f"}, body: "x"},
		}, "files.archive_escape"},
	}
	for name, c := range cases {
		e.write("a.tgz", string(makeTarGz(t, c.entries...)))
		code, body := e.extract("a.tgz", "into", nil)
		want := c.code
		if name == "through a link" {
			// ".." from d is the folder itself, which is inside; the write
			// through it is what is refused.
			want = "files.archive_escape"
		}
		if code != http.StatusUnprocessableEntity || fileCode(body) != want {
			t.Errorf("%s: %d %s", name, code, body)
		}
		e.untouched()
		if entries, _ := os.ReadDir(filepath.Join(e.vol, "into")); len(entries) > 1 {
			for _, en := range entries {
				if en.Name() != "keep" {
					// What came before the bad entry may stay; nothing else.
					os.RemoveAll(filepath.Join(e.vol, "into", en.Name()))
				}
			}
		}
	}
	e.noTemp()
}

func TestExtractZipRefusals(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	build := func(add func(zw *zip.Writer)) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		add(zw)
		zw.Close()
		e.write("a.zip", buf.String())
	}
	build(func(zw *zip.Writer) {
		w, _ := zw.Create("../evil")
		w.Write([]byte("x"))
	})
	if code, body := e.extract("a.zip", "into", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_escape" {
		t.Errorf("zip slip: %d %s", code, body)
	}
	build(func(zw *zip.Writer) {
		w, _ := zw.Create("/abs/evil")
		w.Write([]byte("x"))
	})
	if code, body := e.extract("a.zip", "into", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_escape" {
		t.Errorf("zip absolute: %d %s", code, body)
	}
	link := func(target string) {
		build(func(zw *zip.Writer) {
			h := &zip.FileHeader{Name: "l"}
			h.SetMode(os.ModeSymlink | 0o777)
			w, _ := zw.CreateHeader(h)
			w.Write([]byte(target))
		})
	}
	link(e.outside)
	if code, body := e.extract("a.zip", "into", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_link" {
		t.Errorf("zip link out: %d %s", code, body)
	}
	link("../../secret.txt")
	if code, body := e.extract("a.zip", "into", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_link" {
		t.Errorf("zip relative link out: %d %s", code, body)
	}
	build(func(zw *zip.Writer) {
		h := &zip.FileHeader{Name: "dev"}
		h.SetMode(os.ModeDevice | 0o666)
		zw.CreateHeader(h)
	})
	if code, body := e.extract("a.zip", "into", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_special" {
		t.Errorf("zip device: %d %s", code, body)
	}
	e.untouched()

	// A good one, with a folder that has no entry of its own.
	build(func(zw *zip.Writer) {
		w, _ := zw.Create("pack/data/file.txt")
		w.Write([]byte("hello"))
		h := &zip.FileHeader{Name: "pack/run.sh"}
		h.SetMode(0o755)
		w, _ = zw.CreateHeader(h)
		w.Write([]byte("#!/bin/sh"))
		zw.Create("pack/empty/")
	})
	code, body := e.extract("a.zip", "into", nil)
	if code != http.StatusOK {
		t.Fatalf("zip: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "into/pack/data/file.txt")); string(b) != "hello" {
		t.Errorf("zip content %q", b)
	}
	if st, err := os.Stat(filepath.Join(e.vol, "into/pack/run.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("zip mode: %v %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(e.vol, "into/pack/empty")); err != nil || !st.IsDir() {
		t.Errorf("zip folder: %v %v", st, err)
	}
	st, _ := os.Stat(filepath.Join(e.vol, "into/pack/data"))
	if int(st.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		t.Error("a folder from the archive has another owner")
	}

	// Not an archive.
	e.write("x.txt", "hi")
	if code, body := e.extract("x.txt", "", nil); code != http.StatusBadRequest || fileCode(body) != "files.not_archive" {
		t.Errorf("not an archive: %d %s", code, body)
	}
	e.write("broken.zip", "PK not really")
	if code, body := e.extract("broken.zip", "", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_bad" {
		t.Errorf("broken zip: %d %s", code, body)
	}
	e.write("broken.tar.gz", "not gzip")
	if code, body := e.extract("broken.tar.gz", "", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_bad" {
		t.Errorf("broken gzip: %d %s", code, body)
	}
	if code, body := e.extract("a.zip", "no-such-folder", nil); code != http.StatusNotFound {
		t.Errorf("missing target: %d %s", code, body)
	}
	if code, body := e.extract("a.zip", "x.txt", nil); code != http.StatusConflict {
		t.Errorf("target is a file: %d %s", code, body)
	}
	if code, body := e.extract("../a.zip", "", nil); code != http.StatusBadRequest {
		t.Errorf("archive outside: %d %s", code, body)
	}
}

func TestExtractLimits(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)

	// What it may write is bounded by the volume's room, and a tar cannot
	// say less than it holds.
	e.write("big.tgz", string(makeTarGz(t, tarEntry{hdr: tar.Header{Name: "a"}, body: strings.Repeat("a", 4000)}, tarEntry{hdr: tar.Header{Name: "b"}, body: strings.Repeat("b", 4000)})))
	room := int64(5000)
	code, body := e.extract("big.tgz", "into", &room)
	if code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("over the room: %d %s", code, body)
	}
	if _, err := os.Stat(filepath.Join(e.vol, "into/b")); err == nil {
		t.Error("the second file was written past the room")
	}
	e.noTemp()
	room = 8000
	if code, body = e.extract("big.tgz", "into", &room); code != http.StatusOK {
		t.Errorf("at the room: %d %s", code, body)
	}

	// A zip that declares more than it may.
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("zeros")
	w.Write(make([]byte, 1<<20))
	zw.Close()
	e.write("z.zip", zbuf.String())
	room = 1000
	if code, body = e.extract("z.zip", "into", &room); code != http.StatusConflict || fileCode(body) != "files.volume_full" {
		t.Errorf("zip over the room: %d %s", code, body)
	}

	// Too many entries.
	defer func(n int) { maxArchiveEntries = n }(maxArchiveEntries)
	maxArchiveEntries = 100
	var entries []tarEntry
	for i := 0; i < maxArchiveEntries+1; i++ {
		entries = append(entries, tarEntry{hdr: tar.Header{Name: "d" + strconv.Itoa(i) + "/", Typeflag: tar.TypeDir}})
	}
	e.write("many.tgz", string(makeTarGz(t, entries...)))
	os.Mkdir(filepath.Join(e.vol, "many"), 0o755)
	if code, body = e.extract("many.tgz", "many", nil); code != http.StatusUnprocessableEntity || fileCode(body) != "files.archive_too_many" {
		t.Errorf("many entries: %d %s", code, body)
	}
	if dirs, _ := os.ReadDir(filepath.Join(e.vol, "many")); len(dirs) > maxArchiveEntries {
		t.Errorf("%d folders were made", len(dirs))
	}
	e.noTemp()
}

func TestExtractReplacesFilesAndLinks(t *testing.T) {
	e := newFileEnv(t)
	os.Mkdir(filepath.Join(e.vol, "into"), 0o755)
	// A file the archive replaces is replaced, not written through: here it
	// is a link to a file outside.
	os.Symlink(e.secret, filepath.Join(e.vol, "into/config.yml"))
	e.write("a.tgz", string(makeTarGz(t, tarEntry{hdr: tar.Header{Name: "./config.yml", Mode: 0o4755}, body: "new"})))
	if code, body := e.extract("a.tgz", "into", nil); code != http.StatusOK {
		t.Fatalf("extract: %d %s", code, body)
	}
	e.untouched()
	st, _ := os.Lstat(filepath.Join(e.vol, "into/config.yml"))
	if !st.Mode().IsRegular() || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Errorf("extracted mode %v", st.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "into/config.yml")); string(b) != "new" {
		t.Errorf("content %q", b)
	}
	// A hardlink entry is a copy of a file of the archive.
	e.write("h.tgz", string(makeTarGz(t,
		tarEntry{hdr: tar.Header{Name: "one"}, body: "same"},
		tarEntry{hdr: tar.Header{Name: "two", Typeflag: tar.TypeLink, Linkname: "one"}},
	)))
	if code, body := e.extract("h.tgz", "into", nil); code != http.StatusOK {
		t.Fatalf("extract links: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "into/two")); string(b) != "same" {
		t.Errorf("hardlink copy %q", b)
	}
	a, _ := os.Stat(filepath.Join(e.vol, "into/one"))
	b, _ := os.Stat(filepath.Join(e.vol, "into/two"))
	if os.SameFile(a, b) {
		t.Error("the two files are one")
	}
	// Plain tar and tgz names.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "p.txt", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("pp"))
	tw.Close()
	e.write("plain.tar", buf.String())
	if code, body := e.extract("plain.tar", "", nil); code != http.StatusOK {
		t.Errorf("plain tar: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "p.txt")); string(b) != "pp" {
		t.Errorf("plain tar content %q", b)
	}
	if archiveFormat("A.TGZ") != "tgz" || archiveFormat("b.Tar.Gz") != "tgz" || archiveFormat("c.ZIP") != "zip" || archiveFormat("d.tar") != "tar" || archiveFormat("e.txt") != "" {
		t.Error("formats are not read by suffix")
	}
}

func TestPathsAreCleaned(t *testing.T) {
	for in, want := range map[string]string{"": ".", "/": ".", ".": ".", "a//b/": "a/b", "/a/./b": "a/b", "./a": "a"} {
		if got, err := filePath(in); err != nil || got != want {
			t.Errorf("filePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"..", "../a", "a/..", "a/../b", "/..", "a\x00b", strings.Repeat("a", maxPathBytes+1)} {
		if got, err := filePath(in); err == nil {
			t.Errorf("filePath(%q) = %q, want an error", in, got)
		}
	}
}

// Client and server agree on the requests, over a real socket.
func TestFilesThroughTheClient(t *testing.T) {
	e := newFileEnv(t)
	c := buildClient(t, e.s)
	ref := FileRef{Volume: "srv-vol", FileOwner: e.owner()}
	ctx := t.Context()
	if err := c.WriteFile(ctx, ref, "a.txt", []byte("hello"), true); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFile(ctx, ref, "a.txt", []byte("again"), true); err == nil {
		t.Error("created over a file")
	}
	got, err := c.ReadFile(ctx, ref, "a.txt")
	if err != nil || string(got) != "hello" {
		t.Errorf("read %q %v", got, err)
	}
	if err := c.MakeFolder(ctx, ref, "d"); err != nil {
		t.Error(err)
	}
	if err := c.UploadFile(ctx, ref, "d/u", 3, strings.NewReader("abc")); err != nil {
		t.Error(err)
	}
	if err := c.UploadFile(ctx, ref, "d/v", -1, io.NopCloser(strings.NewReader("streamed"))); err != nil {
		t.Error(err)
	}
	rc, size, err := c.DownloadFile(ctx, ref, "d/v")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "streamed" || size != 8 {
		t.Errorf("download %q %d", b, size)
	}
	list, err := c.ListFiles(ctx, ref, "d")
	if err != nil || len(list.Entries) != 2 {
		t.Errorf("list %+v %v", list, err)
	}
	if err := c.RenameFile(ctx, ref, "d/u", "d/w"); err != nil {
		t.Error(err)
	}
	name, err := c.CompressFiles(ctx, ref, "d", []string{"d/v", "d/w"}, "pack")
	if err != nil || name != "pack.tar.gz" {
		t.Errorf("compress %q %v", name, err)
	}
	res, err := c.ExtractFile(ctx, ref, "d/pack.tar.gz", "")
	if err != nil || res.Entries != 2 {
		t.Errorf("extract %+v %v", res, err)
	}
	room := int64(0)
	ref.Room = &room
	if err := c.UploadFile(ctx, ref, "d/x", 1, strings.NewReader("x")); err == nil {
		t.Error("uploaded with no room")
	} else if ce, ok := err.(*Error); !ok || ce.Code != "files.volume_full" {
		t.Errorf("error %#v", err)
	}
	ref.Room = nil
	if err := c.DeleteFiles(ctx, ref, []string{"d", "a.txt"}); err != nil {
		t.Error(err)
	}
	if entries, _ := os.ReadDir(e.vol); len(entries) != 0 {
		t.Errorf("left %v", entries)
	}
}

func TestStatRemoveAndRange(t *testing.T) {
	e := newFileEnv(t)
	e.write("dir/file.txt", "0123456789")
	os.Symlink(e.outside, filepath.Join(e.vol, "link"))

	code, body := e.post("stat", map[string]any{"path": "dir/file.txt"})
	var fe FileEntry
	json.Unmarshal([]byte(body), &fe)
	if code != http.StatusOK || fe.Name != "file.txt" || fe.Size != 10 || fe.Dir {
		t.Errorf("stat of a file: %d %s", code, body)
	}
	code, body = e.post("stat", map[string]any{"path": "/"})
	json.Unmarshal([]byte(body), &fe)
	if code != http.StatusOK || !fe.Dir {
		t.Errorf("stat of the top folder: %d %s", code, body)
	}
	code, body = e.post("stat", map[string]any{"path": "link"})
	json.Unmarshal([]byte(body), &fe)
	if code != http.StatusOK || !fe.Symlink || fe.Dir {
		t.Errorf("stat of a link must describe it: %d %s", code, body)
	}
	if code, _ = e.post("stat", map[string]any{"path": "link/x"}); code < 400 {
		t.Errorf("stat through a link that leaves the volume: %d", code)
	}
	if code, _ = e.post("stat", map[string]any{"path": "missing"}); code != http.StatusNotFound {
		t.Errorf("stat of nothing: %d", code)
	}

	for _, tc := range []struct{ query, want string }{
		{"offset=3&length=4", "3456"},
		{"offset=8&length=100", "89"},
		{"offset=10", ""},
		{"offset=99&length=5", ""},
		{"length=2", "01"},
	} {
		code, out := e.do("GET", "download", e.query("path", "dir/file.txt")+"&"+tc.query, "")
		if code != http.StatusOK || out != tc.want {
			t.Errorf("download %s: %d %q, want %q", tc.query, code, out, tc.want)
		}
	}
	if code, _ := e.do("GET", "download", e.query("path", "dir/file.txt")+"&offset=-1", ""); code != http.StatusBadRequest {
		t.Errorf("a negative offset: %d", code)
	}

	// Remove never goes into a folder.
	if code, _ = e.post("remove", map[string]any{"path": "dir"}); code < 400 {
		t.Errorf("removing a folder with something in it: %d", code)
	}
	if code, _ = e.post("remove", map[string]any{"path": "/"}); code != http.StatusBadRequest {
		t.Errorf("removing the top folder: %d", code)
	}
	if code, _ = e.post("remove", map[string]any{"path": "link"}); code != http.StatusNoContent {
		t.Errorf("removing a link: %d", code)
	}
	if code, _ = e.post("remove", map[string]any{"path": "dir/file.txt"}); code != http.StatusNoContent {
		t.Errorf("removing a file: %d", code)
	}
	if code, _ = e.post("remove", map[string]any{"path": "dir"}); code != http.StatusNoContent {
		t.Errorf("removing an empty folder: %d", code)
	}
	e.untouched()
}

func TestSFTPUserGetsFileRoutesOnly(t *testing.T) {
	e := newFileEnv(t)
	e.write("a.txt", "a")
	e.s.Allowed.Routes = map[uint32][]string{4242: SFTPRoutes}
	sftp := &peer.Peer{UID: 4242}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"POST", "/v1/volumes/srv-vol/files/list", `{"path":""}`, http.StatusOK},
		{"POST", "/v1/volumes/srv-vol/files/stat", `{"path":"a.txt"}`, http.StatusOK},
		{"GET", "/v1/volumes/srv-vol/files/download?path=a.txt", "", http.StatusOK},
		{"POST", "/v1/volumes/srv-vol/files/delete", `{"paths":["a.txt"]}`, http.StatusForbidden},
		{"POST", "/v1/volumes/srv-vol/files/compress", `{}`, http.StatusForbidden},
		{"PUT", "/v1/volumes/srv-vol/files/content?path=a.txt", "x", http.StatusForbidden},
		{"GET", "/v1/containers", "", http.StatusForbidden},
		{"POST", "/v1/containers", `{}`, http.StatusForbidden},
		{"POST", "/v1/containers/web/stop", "", http.StatusForbidden},
		{"POST", "/v1/volumes/srv-vol/prepare", "", http.StatusForbidden},
		{"GET", "/v1/secrets/key", "", http.StatusForbidden},
		{"POST", "/v1/update", `{}`, http.StatusForbidden},
	} {
		if rec := request(t, e.s, sftp, tc.method, tc.path, tc.body); rec.Code != tc.want {
			t.Errorf("%s %s as the SFTP user: %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "a.txt")); string(b) != "a" {
		t.Errorf("a refused request changed the file: %q", b)
	}
}
