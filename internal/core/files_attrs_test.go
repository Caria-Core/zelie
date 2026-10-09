package core

import (
	"context"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Caria-Core/zelie/internal/peer"
)

func TestChmodSetsOnlyPermissionBits(t *testing.T) {
	e := newFileEnv(t)
	e.write("a.sh", "echo")
	e.write("dir/b.txt", "b")
	os.Symlink("a.sh", filepath.Join(e.vol, "link"))
	os.Symlink(e.outside, filepath.Join(e.vol, "out"))
	mode := func(name string) fs.FileMode {
		st, err := os.Stat(filepath.Join(e.vol, name))
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode()
	}
	chmod := func(p string, m uint32) (int, string) {
		return e.post("chmod", map[string]any{"path": p, "mode": m})
	}

	if code, body := chmod("a.sh", 0o755); code != http.StatusNoContent {
		t.Fatalf("chmod: %d %s", code, body)
	}
	if got := mode("a.sh"); got != 0o755 {
		t.Errorf("a.sh is %v", got)
	}
	if code, _ := chmod("dir", 0o700); code != http.StatusNoContent || mode("dir").Perm() != 0o700 || !mode("dir").IsDir() {
		t.Errorf("a folder: %d, %v", code, mode("dir"))
	}
	// A link in the volume leads to its target, inside the volume.
	if code, _ := chmod("link", 0o600); code != http.StatusNoContent || mode("a.sh").Perm() != 0o600 {
		t.Errorf("through a link: %d, %v", code, mode("a.sh"))
	}

	// The special bits are never set from outside, and the ones a file had
	// go with a new mode, as they would with chmod 0644.
	if code, _ := chmod("a.sh", 0o7777); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if got := mode("a.sh"); got != 0o777 {
		t.Errorf("mode 7777 gave %v, want only the permission bits", got)
	}
	os.Chmod(filepath.Join(e.vol, "a.sh"), 0o755|fs.ModeSetuid|fs.ModeSetgid)
	if code, _ := chmod("a.sh", 0o644); code != http.StatusNoContent {
		t.Fatal(code)
	}
	if got := mode("a.sh"); got != 0o644 {
		t.Errorf("a file that was set-user-ID is %v after chmod 644", got)
	}

	// What is not in the volume is not changed.
	for _, p := range []string{"out/x", "../outside/x", "nothing"} {
		code, body := chmod(p, 0o777)
		if code < 400 {
			t.Errorf("chmod %s: %d %s", p, code, body)
		}
	}
	if st, _ := os.Stat(filepath.Join(e.outside, "x")); st.Mode().Perm() != 0o600 {
		t.Errorf("a file outside the volume is %v", st.Mode())
	}
	e.untouched()
}

func TestTruncateCutsAndExtendsWithinTheRoom(t *testing.T) {
	e := newFileEnv(t)
	e.write("f.txt", "0123456789")
	os.Symlink("f.txt", filepath.Join(e.vol, "link"))
	os.Mkdir(filepath.Join(e.vol, "dir"), 0o755)
	syscall.Mkfifo(filepath.Join(e.vol, "pipe"), 0o644)
	size := func() int64 {
		st, err := os.Stat(filepath.Join(e.vol, "f.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}
	truncate := func(p string, n int64, room *int64) (int, string) {
		req := map[string]any{"path": p, "size": n}
		if room != nil {
			req["room"] = *room
		}
		return e.post("truncate", req)
	}
	room := func(n int64) *int64 { return &n }

	if code, body := truncate("f.txt", 4, nil); code != http.StatusNoContent || size() != 4 {
		t.Fatalf("cut to 4: %d %s, size %d", code, body, size())
	}
	if b, _ := os.ReadFile(filepath.Join(e.vol, "f.txt")); string(b) != "0123" {
		t.Errorf("content %q", b)
	}
	// It grows by what the room allows, the new part being a hole.
	if code, body := truncate("f.txt", 14, room(10)); code != http.StatusNoContent || size() != 14 {
		t.Errorf("extend by the room: %d %s, size %d", code, body, size())
	}
	if code, body := truncate("f.txt", 25, room(10)); code != http.StatusConflict || fileCode(body) != "files.volume_full" || size() != 14 {
		t.Errorf("extend past the room: %d %s, size %d", code, body, size())
	}
	// The disk of the machine has a say as well, whatever the room says.
	if code, body := truncate("f.txt", 1<<62, room(1<<62)); code < 400 || size() != 14 {
		t.Errorf("a size no disk holds: %d %s, size %d", code, body, size())
	}
	// Cutting needs no room at all.
	if code, _ := truncate("f.txt", 2, room(0)); code != http.StatusNoContent || size() != 2 {
		t.Errorf("cut with no room: %d, size %d", code, size())
	}
	if code, _ := truncate("link", 1, nil); code != http.StatusNoContent || size() != 1 {
		t.Errorf("through a link: %d, size %d", code, size())
	}
	if code, body := truncate("f.txt", -1, nil); code != http.StatusBadRequest {
		t.Errorf("a negative size: %d %s", code, body)
	}
	for _, p := range []string{"dir", "pipe", ".", "nothing", "../outside/x"} {
		if code, body := truncate(p, 0, nil); code < 400 {
			t.Errorf("truncate %s: %d %s", p, code, body)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(e.outside, "x")); string(b) != "outside x" {
		t.Errorf("a file outside the volume is %q", b)
	}
	e.noTemp()
}

func TestChtimesSetsTheTimesOfWhatItNames(t *testing.T) {
	e := newFileEnv(t)
	e.write("a.txt", "a")
	e.write("dir/b.txt", "b")
	e.write("target.txt", "t")
	os.Symlink("target.txt", filepath.Join(e.vol, "link"))
	os.Symlink(e.outside, filepath.Join(e.vol, "out"))
	os.Symlink(filepath.Join(e.outside, "x"), filepath.Join(e.vol, "outfile"))
	old := time.Unix(1500000000, 0)
	for _, p := range []string{"a.txt", "dir", "dir/b.txt", "target.txt", "link", "outfile"} {
		os.Chtimes(filepath.Join(e.vol, p), old, old)
	}
	os.Chtimes(filepath.Join(e.outside, "x"), old, old)
	chtimes := func(p string, at, mt int64) (int, string) {
		return e.post("chtimes", map[string]any{"path": p, "atime": at, "mtime": mt})
	}
	modified := func(p string) time.Time {
		st, err := os.Lstat(filepath.Join(e.vol, p))
		if err != nil {
			t.Fatal(err)
		}
		return st.ModTime()
	}

	for _, p := range []string{"a.txt", "dir", "dir/b.txt"} {
		if code, body := chtimes(p, 1700000000, 1600000000); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", p, code, body)
		}
		if got := modified(p); !got.Equal(time.Unix(1600000000, 0)) {
			t.Errorf("%s was modified %v", p, got)
		}
	}
	// The access time is set as well, with the modification time.
	var st unix.Stat_t
	if err := unix.Stat(filepath.Join(e.vol, "a.txt"), &st); err != nil || st.Atim.Sec != 1700000000 {
		t.Errorf("accessed %v, %v", st.Atim, err)
	}
	// A link is not followed: its own times change, and the file it points
	// to, inside the volume or outside it, does not.
	for _, p := range []string{"link", "outfile"} {
		if code, body := chtimes(p, 1700000000, 1600000000); code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", p, code, body)
		}
		if got := modified(p); !got.Equal(time.Unix(1600000000, 0)) {
			t.Errorf("the link %s was modified %v", p, got)
		}
	}
	if got := modified("target.txt"); !got.Equal(old) {
		t.Errorf("the file behind a link was modified %v", got)
	}
	if st, _ := os.Stat(filepath.Join(e.outside, "x")); !st.ModTime().Equal(old) {
		t.Errorf("a file outside the volume was modified %v", st.ModTime())
	}

	// The volume's own folder is the one the path "." names.
	if code, body := chtimes(".", 1700000000, 1600000000); code != http.StatusNoContent {
		t.Errorf("the volume itself: %d %s", code, body)
	}
	if st, _ := os.Stat(e.vol); !st.ModTime().Equal(time.Unix(1600000000, 0)) {
		t.Errorf("the volume was modified %v", st.ModTime())
	}
	// What is not in the volume is not changed, and a time is a number of
	// seconds since 1970.
	for name, c := range map[string]struct {
		p      string
		at, mt int64
		want   int
	}{
		"missing":     {"nothing", 1, 1, http.StatusNotFound},
		"past a link": {"out/x", 1, 1, http.StatusBadRequest},
		"above":       {"../outside/x", 1, 1, http.StatusBadRequest},
		"in a file":   {"a.txt/x", 1, 1, http.StatusConflict},
		"before 1970": {"a.txt", -1, 1, http.StatusBadRequest},
		"too far on":  {"a.txt", 1, 1 << 50, http.StatusBadRequest},
	} {
		if code, body := chtimes(c.p, c.at, c.mt); code != c.want {
			t.Errorf("%s: %d %s, want %d", name, code, body, c.want)
		}
	}
	if st, _ := os.Stat(filepath.Join(e.outside, "x")); !st.ModTime().Equal(old) {
		t.Errorf("a file outside the volume was modified %v", st.ModTime())
	}

	// The latest time that is taken stays a time far on. It is made from
	// seconds, not nanoseconds, so it does not wrap round to before 1970.
	// How far on it is kept is up to the file system.
	if code, body := chtimes("a.txt", maxFileTime, maxFileTime); code != http.StatusNoContent {
		t.Fatalf("the latest time: %d %s", code, body)
	}
	if got := modified("a.txt"); !got.After(time.Unix(1700000000, 0)) {
		t.Errorf("the latest time was kept as %v", got)
	}

	// A named pipe where a folder should be does not hold the request up:
	// opening one waits for a writer, and takes a thread of the core with it.
	pipe := filepath.Join(e.vol, "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipe", filepath.Join(e.vol, "pipelink")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"pipe/x", "pipelink/x"} {
		code, body := e.beforeTheEnd(pipe, func() (int, string) { return chtimes(p, 1, 1) })
		if code != http.StatusConflict || fileCode(body) != "files.not_folder" {
			t.Errorf("%s: %d %s", p, code, body)
		}
	}
	// The pipe itself has times like any other item.
	if code, body := chtimes("pipe", 1700000000, 1600000000); code != http.StatusNoContent {
		t.Errorf("the pipe itself: %d %s", code, body)
	}
	e.untouched()
}

// A folder is listed from the place it is, and the path of a named pipe, or of
// a link to one, is not a folder. It is told so, and the request is not held up.
func TestListingANamedPipeDoesNotWait(t *testing.T) {
	e := newFileEnv(t)
	e.write("dir/a.txt", "a")
	e.write("file.txt", "f")
	pipe := filepath.Join(e.vol, "pipe")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("pipe", filepath.Join(e.vol, "pipelink")); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"list", "entries"} {
		for _, p := range []string{"pipe", "pipelink", "file.txt"} {
			code, body := e.beforeTheEnd(pipe, func() (int, string) { return e.post(op, map[string]any{"path": p}) })
			if code != http.StatusConflict || fileCode(body) != "files.not_folder" {
				t.Errorf("%s %s: %d %s", op, p, code, body)
			}
		}
		if code, body := e.post(op, map[string]any{"path": "dir"}); code != http.StatusOK || !strings.Contains(body, "a.txt") {
			t.Errorf("%s dir: %d %s", op, code, body)
		}
	}
	e.untouched()
}

// beforeTheEnd runs a request that waits for ever if it opens the named pipe
// the wrong way. If it is still waiting after a good while, the pipe is
// opened from the other side so that the request ends, and the test fails.
func (e *fileEnv) beforeTheEnd(pipe string, do func() (int, string)) (int, string) {
	e.t.Helper()
	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() {
		code, body := do()
		done <- result{code, body}
	}()
	select {
	case r := <-done:
		return r.code, r.body
	case <-time.After(5 * time.Second):
	}
	e.t.Error("the request waited for a named pipe")
	if w, err := os.OpenFile(pipe, os.O_RDWR, 0); err == nil {
		defer w.Close()
	}
	r := <-done
	return r.code, r.body
}

// fileRef is what the client names a volume with.
func (e *fileEnv) fileRef() FileRef {
	return FileRef{Volume: "srv-vol", FileOwner: e.owner()}
}

func TestClientSetsModeAndLength(t *testing.T) {
	e := newFileEnv(t)
	e.write("f.txt", "0123456789")
	c := buildClient(t, e.s)
	ctx := context.Background()
	at, mt := time.Unix(1700000000, 0), time.Unix(1600000000, 0)
	if err := c.SetFileTimes(ctx, e.fileRef(), "f.txt", at, mt); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(e.vol, "f.txt")); !st.ModTime().Equal(mt) {
		t.Errorf("modified %v", st.ModTime())
	}
	if err := c.SetFileMode(ctx, e.fileRef(), "f.txt", 0o755|fs.ModeSetuid); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(e.vol, "f.txt")); st.Mode() != 0o755 {
		t.Errorf("mode %v", st.Mode())
	}
	room := int64(0)
	ref := e.fileRef()
	ref.Room = &room
	if err := c.TruncateFile(ctx, ref, "f.txt", 3); err != nil {
		t.Fatal(err)
	}
	err := c.TruncateFile(ctx, ref, "f.txt", 4)
	if ce, ok := err.(*Error); !ok || ce.Status != http.StatusConflict || ce.Code != "files.volume_full" {
		t.Errorf("growing with no room: %v", err)
	}
	if st, _ := os.Stat(filepath.Join(e.vol, "f.txt")); st.Size() != 3 {
		t.Errorf("size %d", st.Size())
	}
}

// listAll reads a folder through the client to the end.
func listAll(t *testing.T, c *Client, ref FileRef, dir string) []FileEntry {
	t.Helper()
	f, err := c.OpenFolder(context.Background(), ref, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []FileEntry
	for {
		fe, err := f.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fe)
	}
}

// The listing the panel shows stops at maxListed. SFTP has no way to say
// that a folder was cut short, so its own listing has no limit.
func TestStreamedListingHasEveryItem(t *testing.T) {
	e := newFileEnv(t)
	dir := filepath.Join(e.vol, "big")
	os.Mkdir(dir, 0o755)
	n := maxListed + 700
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.Mkdir(filepath.Join(dir, "sub"), 0o755)
	os.Symlink("f1", filepath.Join(dir, "link"))
	n += 2

	c := buildClient(t, e.s)
	if list, err := c.ListFiles(context.Background(), e.fileRef(), "big"); err != nil || !list.Truncated || len(list.Entries) != maxListed {
		t.Fatalf("the plain listing: %d entries, truncated %v, %v", len(list.Entries), list.Truncated, err)
	}
	got := listAll(t, c, e.fileRef(), "big")
	if len(got) != n {
		t.Fatalf("%d items streamed, want %d", len(got), n)
	}
	seen := map[string]FileEntry{}
	for _, fe := range got {
		if _, dup := seen[fe.Name]; dup {
			t.Fatalf("%s came twice", fe.Name)
		}
		seen[fe.Name] = fe
	}
	if !seen["sub"].Dir || !seen["link"].Symlink || seen["link"].Target != "f1" {
		t.Errorf("sub: %+v, link: %+v", seen["sub"], seen["link"])
	}
}

func TestStreamedListingIsSentInPages(t *testing.T) {
	e := newFileEnv(t)
	for i := range 3*entryPage + 5 {
		e.write("d/f"+strconv.Itoa(i), "")
	}
	rec := request(t, e.s, &peer.Peer{UID: 0}, "POST", "/v1/volumes/srv-vol/files/entries", `{"path":"d"}`)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/x-ndjson") {
		t.Fatalf("%d %s", rec.Code, rec.Header())
	}
	lines := strings.Split(strings.TrimSuffix(rec.Body.String(), "\n"), "\n")
	if len(lines) != 3*entryPage+5+1 {
		t.Errorf("%d lines", len(lines))
	}
	if last := lines[len(lines)-1]; last != `{"end":true}` {
		t.Errorf("the last line is %q", last)
	}
	if !rec.Flushed {
		t.Error("the listing was not flushed")
	}
}

// stallRecorder notes when a page is written and when the time to take it
// is set, as the connection would see them.
type stallRecorder struct {
	*httptest.ResponseRecorder
	events []string
}

func (r *stallRecorder) Write(b []byte) (int, error) {
	r.events = append(r.events, "write")
	return r.ResponseRecorder.Write(b)
}

func (r *stallRecorder) Flush() {
	r.events = append(r.events, "flush")
	r.ResponseRecorder.Flush()
}

func (r *stallRecorder) SetWriteDeadline(time.Time) error {
	r.events = append(r.events, "deadline")
	return nil
}

// A client that stops reading is cut off by the write deadline, so it has to
// be there before anything of a page is written: the first page may be the
// only one a client ever takes.
func TestStreamedListingSetsTheStallDeadlineBeforeEachPage(t *testing.T) {
	e := newFileEnv(t)
	for i := range 2*entryPage + 5 {
		e.write("d/f"+strconv.Itoa(i), "")
	}
	req := httptest.NewRequest("POST", "/v1/volumes/srv-vol/files/entries", strings.NewReader(`{"path":"d"}`))
	req = req.WithContext(peer.WithPeer(req.Context(), peer.Peer{UID: 0}))
	rec := &stallRecorder{ResponseRecorder: httptest.NewRecorder()}
	e.s.Handler().ServeHTTP(rec, req)

	pages, armed := 0, false
	for _, ev := range rec.events {
		switch ev {
		case "deadline":
			armed = true
		case "flush":
			pages++
			armed = false
		case "write":
			if !armed {
				t.Fatalf("page %d was written with no deadline set: %v", pages+1, rec.events[:min(len(rec.events), 8)])
			}
		}
	}
	if pages < 3 {
		t.Errorf("%d pages, want at least 3", pages)
	}
}

func TestStreamedListingFailsLikeTheOthers(t *testing.T) {
	e := newFileEnv(t)
	e.write("file.txt", "x")
	os.Symlink(e.outside, filepath.Join(e.vol, "out"))
	c := buildClient(t, e.s)
	for _, tc := range []struct {
		dir    string
		status int
		code   string
	}{
		{"nothing", http.StatusNotFound, "files.not_found"},
		{"file.txt", http.StatusConflict, "files.not_folder"},
		{"../outside", http.StatusBadRequest, "files.bad_path"},
		{"out", http.StatusBadRequest, "files.bad_path"},
	} {
		_, err := c.OpenFolder(context.Background(), e.fileRef(), tc.dir)
		ce, ok := err.(*Error)
		if !ok || ce.Status != tc.status || ce.Code != tc.code {
			t.Errorf("%s: %v, want %d %s", tc.dir, err, tc.status, tc.code)
		}
	}
}

// A listing that ends without saying so was cut off, and must not look like
// a folder that ends there.
func TestStreamedListingThatBreaksOffIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no end line":    `{"entry":{"name":"a"}}` + "\n" + `{"entry":{"name":"b"}}` + "\n",
		"half a line":    `{"entry":{"name":"a"}}` + "\n" + `{"entry":{"na`,
		"an error line":  `{"entry":{"name":"a"}}` + "\n" + `{"error":"the disk failed"}` + "\n",
		"a line of none": `{"entry":{"name":"a"}}` + "\n" + `{}` + "\n",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(body))
		}))
		c := &Client{http: srv.Client()}
		u, _ := url.Parse(srv.URL)
		c.http.Transport = &http.Transport{DialContext: func(ctx context.Context, _, _ string) (conn net.Conn, err error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", u.Host)
		}}
		f, err := c.OpenFolder(context.Background(), FileRef{Volume: "v"}, "d")
		if err != nil {
			t.Fatal(err)
		}
		if fe, err := f.Next(); err != nil || fe.Name != "a" {
			t.Fatalf("%s: first %+v, %v", name, fe, err)
		}
		var err2 error
		for err2 == nil {
			_, err2 = f.Next()
		}
		if err2 == io.EOF {
			t.Errorf("%s: the listing looked complete", name)
		}
		f.Close()
		srv.Close()
	}
}

func TestStreamedListingEndsAfterTheLastItem(t *testing.T) {
	e := newFileEnv(t)
	e.write("d/a", "")
	c := buildClient(t, e.s)
	f, err := c.OpenFolder(context.Background(), e.fileRef(), "d")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if fe, err := f.Next(); err != nil || fe.Name != "a" {
		t.Fatalf("%+v, %v", fe, err)
	}
	for range 2 {
		if _, err := f.Next(); err != io.EOF {
			t.Errorf("after the last item: %v", err)
		}
	}
}
