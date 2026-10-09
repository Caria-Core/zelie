package panel

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/store"
)

// coreFiles is the core's file manager as far as the panel can tell: what it
// was asked, with whom as the owner, and a few files to answer from.
type coreFiles struct {
	ops   []string
	refs  []core.FileRef
	files map[string][]byte
	err   error
	sizes []int64 // the size each upload said it had
}

func (c *appCore) fileCall(ref core.FileRef, op string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fs.ops = append(c.fs.ops, op)
	c.fs.refs = append(c.fs.refs, ref)
	return c.fs.err
}

func (c *appCore) ListFiles(_ context.Context, ref core.FileRef, dir string) (core.FileList, error) {
	if err := c.fileCall(ref, "list "+dir); err != nil {
		return core.FileList{}, err
	}
	return core.FileList{Path: dir, Entries: []core.FileEntry{{Name: "server.jar", Size: 12, Mode: "-rw-r--r--"}}}, nil
}

func (c *appCore) ReadFile(_ context.Context, ref core.FileRef, path string) ([]byte, error) {
	if err := c.fileCall(ref, "read "+path); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.fs.files[path]
	if !ok {
		return nil, &core.Error{Status: http.StatusNotFound, Message: "no such file", Code: "files.not_found", Params: map[string]any{"path": path}}
	}
	return b, nil
}

func (c *appCore) WriteFile(_ context.Context, ref core.FileRef, path string, content []byte, create bool) error {
	op := "write " + path
	if create {
		op = "create " + path
	}
	if err := c.fileCall(ref, op); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.fs.files[path]; taken && create {
		return &core.Error{Status: http.StatusConflict, Message: "exists", Code: "files.exists", Params: map[string]any{"path": path}}
	}
	if c.fs.files == nil {
		c.fs.files = map[string][]byte{}
	}
	c.fs.files[path] = content
	return nil
}

func (c *appCore) MakeFolder(_ context.Context, ref core.FileRef, path string) error {
	return c.fileCall(ref, "mkdir "+path)
}

func (c *appCore) RenameFile(_ context.Context, ref core.FileRef, from, to string) error {
	return c.fileCall(ref, "rename "+from+" "+to)
}

func (c *appCore) DeleteFiles(_ context.Context, ref core.FileRef, paths []string) error {
	return c.fileCall(ref, "delete "+strings.Join(paths, " "))
}

func (c *appCore) UploadFile(_ context.Context, ref core.FileRef, path string, size int64, body io.Reader) error {
	if err := c.fileCall(ref, "upload "+path); err != nil {
		return err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fs.files == nil {
		c.fs.files = map[string][]byte{}
	}
	c.fs.files[path] = b
	c.fs.sizes = append(c.fs.sizes, size)
	return nil
}

func (c *appCore) DownloadFile(_ context.Context, ref core.FileRef, path string) (io.ReadCloser, int64, error) {
	if err := c.fileCall(ref, "download "+path); err != nil {
		return nil, 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.fs.files[path]
	if !ok {
		return nil, 0, &core.Error{Status: http.StatusNotFound, Message: "no such file", Code: "files.not_found"}
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (c *appCore) CompressFiles(_ context.Context, ref core.FileRef, dir string, paths []string, name string) (string, error) {
	if err := c.fileCall(ref, "compress "+dir+" "+strings.Join(paths, " ")); err != nil {
		return "", err
	}
	if name == "" {
		name = "archive.tar.gz"
	}
	return name, nil
}

func (c *appCore) ExtractFile(_ context.Context, ref core.FileRef, path, dir string) (core.ExtractResult, error) {
	if err := c.fileCall(ref, "extract "+path+" "+dir); err != nil {
		return core.ExtractResult{}, err
	}
	return core.ExtractResult{Entries: 3, Bytes: 30}, nil
}

// syncBuffer collects a log that the server writes from other goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newFilesEnv(t *testing.T) *appEnv {
	t.Helper()
	e, _ := newGameEnv(t)
	if code, out := e.b.do("POST", "/api/games", map[string]any{"name": "survival", "egg": "minecraft-paper"}); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	e.install(t, "survival")
	return e
}

func TestGameFilesAreForwardedToTheCore(t *testing.T) {
	e := newFilesEnv(t)
	log := &syncBuffer{}
	e.s.Log = slog.New(slog.NewTextHandler(log, nil))
	vols, _ := e.s.Store.Volumes(context.Background(), "survival")

	code, out := e.b.do("GET", "/api/games/survival/files/list?path=plugins", nil)
	if code != http.StatusOK || out["path"] != "plugins" {
		t.Fatalf("list: %d %v", code, out)
	}
	ref := e.core.fs.refs[0]
	if ref.Volume != vols[0].Name || ref.UID != 988 || ref.GID != 988 || ref.Room != nil {
		t.Errorf("ref %+v", ref)
	}

	// Once the volume has been measured, the core hears how much room is
	// left under the limit.
	e.s.sizes.set(map[string]int64{vols[0].Name: vols[0].LimitMB<<20 - 1000}, nil, e.s.now())
	e.b.do("GET", "/api/games/survival/files/list?path=", nil)
	if room := e.core.fs.refs[1].Room; room == nil || *room != 1000 {
		t.Errorf("room %v", room)
	}
	e.s.sizes.set(map[string]int64{vols[0].Name: vols[0].LimitMB<<20 + 5}, nil, e.s.now())
	e.b.do("GET", "/api/games/survival/files/list?path=", nil)
	if room := e.core.fs.refs[2].Room; room == nil || *room != 0 {
		t.Errorf("room over the limit %v", room)
	}
	// A volume that could not be measured is not known to have room, which
	// is not the same as no measurement yet.
	e.s.sizes.set(nil, []string{vols[0].Name}, e.s.now())
	e.b.do("GET", "/api/games/survival/files/list?path=", nil)
	if room := e.core.fs.refs[3].Room; room == nil || *room != 0 {
		t.Errorf("room of a volume that cannot be measured %v", room)
	}

	if rec := e.b.record("PUT", "/api/games/survival/files/content?path=eula.txt&new=1", []byte("eula=true\n")); rec.Code != http.StatusNoContent {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	rec := e.b.record("GET", "/api/games/survival/files/content?path=eula.txt", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "eula=true\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("read: %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	code, out = e.b.do("PUT", "/api/games/survival/files/content?path=eula.txt&new=1", nil)
	if code != http.StatusConflict || out["code"] != "files.exists" {
		t.Errorf("create again: %d %v", code, out)
	}
	code, out = e.b.do("GET", "/api/games/survival/files/content?path=missing", nil)
	if code != http.StatusNotFound || out["code"] != "files.not_found" || out["params"].(map[string]any)["path"] != "missing" {
		t.Errorf("missing: %d %v", code, out)
	}

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/games/survival/files/mkdir", map[string]any{"path": "plugins"}},
		{"POST", "/api/games/survival/files/rename", map[string]any{"from": "a", "to": "b"}},
		{"POST", "/api/games/survival/files/delete", map[string]any{"paths": []string{"x", "y"}}},
		{"POST", "/api/games/survival/files/compress", map[string]any{"dir": "", "paths": []string{"world"}}},
		{"POST", "/api/games/survival/files/extract", map[string]any{"path": "a.zip", "dir": "into"}},
	} {
		if code, out := e.b.do(c.method, c.path, c.body); code != http.StatusNoContent && code != http.StatusOK {
			t.Errorf("%s: %d %v", c.path, code, out)
		}
	}
	if code, out := e.b.do("POST", "/api/games/survival/files/mkdir", map[string]any{"path": "p", "owner": 0}); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d %v", code, out)
	}
	code, out = e.b.do("POST", "/api/games/survival/files/compress", map[string]any{"dir": "", "paths": []string{"world"}, "name": "w"})
	if code != http.StatusOK || out["name"] != "w" {
		t.Errorf("compress: %d %v", code, out)
	}
	code, out = e.b.do("POST", "/api/games/survival/files/extract", map[string]any{"path": "a.zip"})
	if code != http.StatusOK || out["entries"] != 3.0 {
		t.Errorf("extract: %d %v", code, out)
	}

	// The writes are in the log, with who did them.
	for _, want := range []string{
		"game file written", "game file folder made", "game file renamed", "game file deleted", "game file compressed", "game file extracted",
		"server=survival", "path=eula.txt", "path=plugins", "path=\"a, b\"", "path=\"x, y\"",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log lacks %q:\n%s", want, log)
		}
	}
	if !strings.Contains(log.String(), "user=") {
		t.Errorf("the log has no user:\n%s", log)
	}
	// A read is not an event.
	if strings.Contains(log.String(), "game file read") {
		t.Error("reads are logged")
	}
}

func TestGameFileUploadAndDownload(t *testing.T) {
	e := newFilesEnv(t)
	log := &syncBuffer{}
	e.s.Log = slog.New(slog.NewTextHandler(log, nil))
	payload := strings.Repeat("jar", 50_000)
	rec := e.b.record("PUT", "/api/games/survival/files/upload?path=plugins/big.jar", []byte(payload))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	if got := string(e.core.fs.files["plugins/big.jar"]); got != payload {
		t.Errorf("the core got %d bytes", len(got))
	}
	if e.core.fs.sizes[0] != int64(len(payload)) {
		t.Errorf("the size the core was told: %d", e.core.fs.sizes[0])
	}
	if !strings.Contains(log.String(), "game file uploaded") || !strings.Contains(log.String(), "plugins/big.jar") {
		t.Errorf("upload not logged:\n%s", log)
	}

	rec = e.b.record("GET", "/api/games/survival/files/download?path=plugins/big.jar", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != payload {
		t.Fatalf("download: %d, %d bytes", rec.Code, rec.Body.Len())
	}
	h := rec.Header()
	if h.Get("Content-Disposition") != "attachment; filename=big.jar" || h.Get("Content-Type") != "application/octet-stream" || h.Get("Content-Length") != "150000" {
		t.Errorf("headers %v", h)
	}
	// A name that would break out of the header is quoted, and one with
	// other characters is encoded, a new line included.
	for name, want := range map[string]string{
		`a"b.txt`:    `attachment; filename="a\"b.txt"`,
		"ünï cödé":   "attachment; filename*=utf-8''%C3%BCn%C3%AF%20c%C3%B6d%C3%A9",
		"line\nfeed": "attachment; filename*=utf-8''line%0Afeed",
	} {
		e.core.fs.files[name] = []byte("x")
		rec = e.b.record("GET", "/api/games/survival/files/download?path="+url.QueryEscape(name), nil)
		if got := rec.Header().Get("Content-Disposition"); got != want {
			t.Errorf("disposition for %q: %q, want %q", name, got, want)
		}
	}
	if code, out := e.b.do("GET", "/api/games/survival/files/download?path=nothing", nil); code != http.StatusNotFound || out["code"] != "files.not_found" {
		t.Errorf("download missing: %d %v", code, out)
	}
}

func TestGameFileErrorsFromTheCore(t *testing.T) {
	e := newFilesEnv(t)
	e.core.fs.err = &core.Error{Status: http.StatusBadRequest, Message: "not inside", Code: "files.bad_path"}
	code, out := e.b.do("GET", "/api/games/survival/files/list?path=..", nil)
	if code != http.StatusBadRequest || out["code"] != "files.bad_path" {
		t.Errorf("a refused path: %d %v", code, out)
	}
	// A failure of the core itself says only that it is down.
	e.core.fs.err = &core.Error{Status: http.StatusInternalServerError, Message: "secret detail"}
	code, out = e.b.do("GET", "/api/games/survival/files/list?path=", nil)
	if code < 500 || strings.Contains(out["error"].(string), "secret") {
		t.Errorf("a core failure: %d %v", code, out)
	}
}

func TestGameFilesNeedAServerThatIsThere(t *testing.T) {
	e := newFilesEnv(t)
	if code, out := e.b.do("GET", "/api/games/nothing/files/list?path=", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("no server: %d %v", code, out)
	}
	if code, _ := e.b.do("POST", "/api/apps", map[string]any{"id": "web", "source": "image", "image": "nginx"}); code != http.StatusCreated {
		t.Fatal("create app")
	}
	e.settle(t, "web")
	if code, out := e.b.do("GET", "/api/games/web/files/list?path=", nil); code != http.StatusNotFound || out["code"] != "game.not_found" {
		t.Errorf("an app that is not a game: %d %v", code, out)
	}
	// Not while the installer runs.
	if err := e.s.Store.SetInstall(context.Background(), "survival", store.InstallRunning, 0, e.s.now()); err != nil {
		t.Fatal(err)
	}
	n := len(e.core.fs.ops)
	if code, out := e.b.do("GET", "/api/games/survival/files/list?path=", nil); code != http.StatusConflict || out["code"] != "game.installing" {
		t.Errorf("while installing: %d %v", code, out)
	}
	if len(e.core.fs.ops) != n {
		t.Error("the core was asked while the install ran")
	}
}

func TestGameFileRoutesNeedALogin(t *testing.T) {
	e := newFilesEnv(t)
	out := &browser{t: t, h: e.b.h, ip: "198.51.100.9"}
	for _, r := range [][2]string{
		{"GET", "/api/games/survival/files/list?path="},
		{"GET", "/api/games/survival/files/content?path=a"},
		{"PUT", "/api/games/survival/files/content?path=a"},
		{"POST", "/api/games/survival/files/mkdir"},
		{"POST", "/api/games/survival/files/rename"},
		{"POST", "/api/games/survival/files/delete"},
		{"PUT", "/api/games/survival/files/upload?path=a"},
		{"GET", "/api/games/survival/files/download?path=a"},
		{"POST", "/api/games/survival/files/compress"},
		{"POST", "/api/games/survival/files/extract"},
		{"GET", "/api/games/survival/files/favorites"},
		{"PUT", "/api/games/survival/files/favorites"},
		{"DELETE", "/api/games/survival/files/favorites?path=a"},
		{"PUT", "/api/games/survival/variables"},
	} {
		if code, _ := out.do(r[0], r[1], nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d", r[0], r[1], code)
		}
	}
	if len(e.core.fs.ops) != 0 {
		t.Errorf("the core was asked: %v", e.core.fs.ops)
	}
}

func TestGameFileFavorites(t *testing.T) {
	e := newFilesEnv(t)
	const base = "/api/games/survival/files/favorites"
	paths := func() []any {
		code, out := e.b.do("GET", base, nil)
		if code != http.StatusOK {
			t.Fatalf("list: %d %v", code, out)
		}
		return out["paths"].([]any)
	}
	if got := paths(); len(got) != 0 {
		t.Fatalf("at first: %v", got)
	}

	// Paths are kept relative and cleaned, and a second star changes nothing.
	for _, p := range []string{"/oxide/oxide.config.json", "plugins/", "oxide//oxide.config.json"} {
		if code, out := e.b.do("PUT", base, map[string]any{"path": p}); code != http.StatusNoContent {
			t.Fatalf("add %q: %d %v", p, code, out)
		}
	}
	if got := paths(); len(got) != 2 || !slices.Contains(got, any("oxide/oxide.config.json")) || !slices.Contains(got, any("plugins")) {
		t.Errorf("after adding: %v", got)
	}
	for _, p := range []string{"", "/", ".", "../etc", "a/../../b", "a\x00b"} {
		if code, out := e.b.do("PUT", base, map[string]any{"path": p}); code != http.StatusBadRequest || out["code"] != "files.bad_favorite" {
			t.Errorf("add %q: %d %v", p, code, out)
		}
	}
	if len(e.core.fs.ops) != 0 {
		t.Errorf("the core was asked: %v", e.core.fs.ops)
	}

	// A rename in the file manager takes the stars along.
	if code, out := e.b.do("POST", "/api/games/survival/files/rename", map[string]any{"from": "plugins", "to": "mods"}); code != http.StatusNoContent {
		t.Fatalf("rename: %d %v", code, out)
	}
	if got := paths(); len(got) != 2 || !slices.Contains(got, any("mods")) {
		t.Errorf("after rename: %v", got)
	}

	if code, out := e.b.do("DELETE", base+"?path=mods", nil); code != http.StatusNoContent {
		t.Fatalf("remove: %d %v", code, out)
	}
	if got := paths(); len(got) != 1 {
		t.Errorf("after removing: %v", got)
	}
	if code, out := e.b.do("DELETE", base+"?path=..%2Fx", nil); code != http.StatusBadRequest {
		t.Errorf("remove a bad path: %d %v", code, out)
	}

	for i := range store.MaxFileFavorites {
		e.b.do("PUT", base, map[string]any{"path": "f" + strconv.Itoa(i)})
	}
	if code, out := e.b.do("PUT", base, map[string]any{"path": "one-too-many"}); code != http.StatusConflict || out["code"] != "files.too_many_favorites" {
		t.Errorf("past the limit: %d %v", code, out)
	}
}
