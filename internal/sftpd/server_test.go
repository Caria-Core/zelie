package sftpd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/hostkey"
)

// fakeFiles stands in for the core: each volume is a folder, and every path
// goes through an os.Root like the core's, so a link that leads out fails
// the same way. It remembers the paths it was asked about.
type fakeFiles struct {
	mu    sync.Mutex
	dirs  map[string]string
	paths []string
	rooms []*int64
	// made lists folders that are not on a disk: n items, and then err
	// instead of the end.
	made map[string]madeFolder
	// opened counts the folders that were closed.
	opened atomic.Int32
	// failMode makes setting a mode fail.
	failMode error
}

type madeFolder struct {
	n   int
	err error
}

func (f *fakeFiles) root(ref core.FileRef, p string) (*os.Root, string, error) {
	f.mu.Lock()
	f.paths = append(f.paths, p)
	dir, ok := f.dirs[ref.Volume]
	f.mu.Unlock()
	if !ok {
		return nil, "", &core.Error{Status: http.StatusNotFound, Message: "no such volume"}
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return nil, "", &core.Error{Status: http.StatusBadRequest, Message: "That path is not inside the server's files."}
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", err
	}
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" {
		c = "."
	}
	return r, c, nil
}

func fail(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return &core.Error{Status: http.StatusNotFound, Message: "not found"}
	default:
		return &core.Error{Status: http.StatusConflict, Message: err.Error()}
	}
}

func entry(name string, fi fs.FileInfo) core.FileEntry {
	return core.FileEntry{Name: name, Size: fi.Size(), Mode: fi.Mode().String(), Modified: fi.ModTime(), Dir: fi.IsDir(), Symlink: fi.Mode()&fs.ModeSymlink != 0}
}

// fakeFolder serves entries one at a time, as the core's stream does.
type fakeFolder struct {
	next   func() (core.FileEntry, error)
	closed *atomic.Int32
}

func (f *fakeFolder) Next() (core.FileEntry, error) { return f.next() }
func (f *fakeFolder) Close() error                  { f.closed.Add(1); return nil }

func (f *fakeFiles) OpenFolder(_ context.Context, ref core.FileRef, dir string) (core.Folder, error) {
	// A folder that is made up as it is read, of any size, for the ones too
	// big to put on a disk in a test.
	f.mu.Lock()
	made, ok := f.made[dir]
	f.mu.Unlock()
	if ok {
		i := 0
		return &fakeFolder{closed: &f.opened, next: func() (core.FileEntry, error) {
			if i == made.n {
				if made.err != nil {
					return core.FileEntry{}, made.err
				}
				return core.FileEntry{}, io.EOF
			}
			i++
			return core.FileEntry{Name: "file-" + strconv.Itoa(i), Size: int64(i), Mode: "-rw-r--r--"}, nil
		}}, nil
	}
	r, p, err := f.root(ref, dir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	d, err := r.Open(p)
	if err != nil {
		return nil, fail(err)
	}
	defer d.Close()
	des, err := d.ReadDir(-1)
	if err != nil {
		return nil, fail(err)
	}
	var entries []core.FileEntry
	for _, de := range des {
		if fi, err := de.Info(); err == nil {
			entries = append(entries, entry(de.Name(), fi))
		}
	}
	return &fakeFolder{closed: &f.opened, next: func() (core.FileEntry, error) {
		if len(entries) == 0 {
			return core.FileEntry{}, io.EOF
		}
		e := entries[0]
		entries = entries[1:]
		return e, nil
	}}, nil
}

func (f *fakeFiles) setFailMode(err error) {
	f.mu.Lock()
	f.failMode = err
	f.mu.Unlock()
}

func (f *fakeFiles) SetFileMode(_ context.Context, ref core.FileRef, p string, mode fs.FileMode) error {
	f.mu.Lock()
	failMode := f.failMode
	f.mu.Unlock()
	if failMode != nil {
		return failMode
	}
	r, c, err := f.root(ref, p)
	if err != nil {
		return err
	}
	defer r.Close()
	return fail(r.Chmod(c, mode))
}

func (f *fakeFiles) SetFileTimes(_ context.Context, ref core.FileRef, p string, atime, mtime time.Time) error {
	f.mu.Lock()
	failMode := f.failMode
	f.mu.Unlock()
	if failMode != nil {
		return failMode
	}
	r, c, err := f.root(ref, p)
	if err != nil {
		return err
	}
	defer r.Close()
	return fail(r.Chtimes(c, atime, mtime))
}

func (f *fakeFiles) TruncateFile(_ context.Context, ref core.FileRef, p string, size int64) error {
	f.mu.Lock()
	f.rooms = append(f.rooms, ref.Room)
	f.mu.Unlock()
	r, c, err := f.root(ref, p)
	if err != nil {
		return err
	}
	defer r.Close()
	file, err := r.OpenFile(c, os.O_WRONLY, 0)
	if err != nil {
		return fail(err)
	}
	defer file.Close()
	return fail(file.Truncate(size))
}

func (f *fakeFiles) StatFile(_ context.Context, ref core.FileRef, p string) (core.FileEntry, error) {
	r, c, err := f.root(ref, p)
	if err != nil {
		return core.FileEntry{}, err
	}
	defer r.Close()
	fi, err := r.Lstat(c)
	if err != nil {
		return core.FileEntry{}, fail(err)
	}
	return entry(path.Base(c), fi), nil
}

func (f *fakeFiles) DownloadRange(_ context.Context, ref core.FileRef, p string, offset, length int64) (io.ReadCloser, int64, error) {
	r, c, err := f.root(ref, p)
	if err != nil {
		return nil, 0, err
	}
	defer r.Close()
	file, err := r.Open(c)
	if err != nil {
		return nil, 0, fail(err)
	}
	defer file.Close()
	b := make([]byte, length)
	n, _ := file.ReadAt(b, offset)
	return io.NopCloser(bytes.NewReader(b[:n])), int64(n), nil
}

func (f *fakeFiles) UploadFile(_ context.Context, ref core.FileRef, p string, _ int64, body io.Reader) error {
	f.mu.Lock()
	f.rooms = append(f.rooms, ref.Room)
	f.mu.Unlock()
	r, c, err := f.root(ref, p)
	if err != nil {
		return err
	}
	defer r.Close()
	// Like the core, through a temporary file, so a broken upload leaves nothing.
	tmp := c + ".tmp"
	file, err := r.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fail(err)
	}
	_, err = io.Copy(file, body)
	file.Close()
	if err == nil {
		err = r.Rename(tmp, c)
	}
	if err != nil {
		r.Remove(tmp)
		return fail(err)
	}
	return nil
}

func (f *fakeFiles) MakeFolder(_ context.Context, ref core.FileRef, p string) error {
	r, c, err := f.root(ref, p)
	if err != nil {
		return err
	}
	defer r.Close()
	return fail(r.Mkdir(c, 0o755))
}

func (f *fakeFiles) RenameFile(_ context.Context, ref core.FileRef, from, to string) error {
	r, a, err := f.root(ref, from)
	if err != nil {
		return err
	}
	defer r.Close()
	_, b, err := f.root(ref, to)
	if err != nil {
		return err
	}
	return fail(r.Rename(a, b))
}

func (f *fakeFiles) RemoveFile(_ context.Context, ref core.FileRef, p string) error {
	r, c, err := f.root(ref, p)
	if err != nil {
		return err
	}
	defer r.Close()
	return fail(r.Remove(c))
}

// fakePanel knows two servers, each with a password, and the keys of one
// account. It is not the panel's logic (that is tested there), only enough
// to see what the SFTP side does with an answer.
type fakePanel struct {
	passwords map[string]string
	keys      map[string][]byte // server -> public key
	seenIPs   []string

	mu       sync.Mutex
	revoked  map[string]bool
	checkErr error
	checks   int
}

func (p *fakePanel) revoke(server string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.revoked == nil {
		p.revoked = map[string]bool{}
	}
	p.revoked[server] = true
}

func (p *fakePanel) Check(_ context.Context, g Grant) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks++
	if p.checkErr != nil {
		return p.checkErr
	}
	if p.revoked[g.Server] {
		return ErrDenied
	}
	return nil
}

func (p *fakePanel) grant(server string) Grant {
	return Grant{Server: server, Volume: "vol-" + server, UID: 988, GID: 988}
}

func (p *fakePanel) Password(_ context.Context, server, password, ip string) (Grant, error) {
	p.seenIPs = append(p.seenIPs, ip)
	if want, ok := p.passwords[server]; ok && want == password {
		return p.grant(server), nil
	}
	return Grant{}, ErrDenied
}

func (p *fakePanel) Key(_ context.Context, server string, key ssh.PublicKey, ip string) (Grant, error) {
	if want, ok := p.keys[server]; ok && bytes.Equal(want, key.Marshal()) {
		return p.grant(server), nil
	}
	return Grant{}, ErrDenied
}

func (p *fakePanel) Room(context.Context, string) (*int64, error) {
	n := int64(1 << 30)
	return &n, nil
}

type env struct {
	addr    string
	files   *fakeFiles
	panel   *fakePanel
	alpha   string // volume folders
	beta    string
	outside string
	server  *Server
	key     ssh.Signer
	// served gets what Serve returned, once it has.
	served chan error
}

func newEnv(t *testing.T) *env { return newEnvIdle(t, 0) }

// newEnvIdle is newEnv with a server that leaves after idle with no
// connection open; zero means the default.
func newEnvIdle(t *testing.T, idle time.Duration) *env { return newEnvIdleWith(t, idle, nil) }

func newEnvIdleWith(t *testing.T, idle time.Duration, set func(*Server)) *env {
	t.Helper()
	base := t.TempDir()
	e := &env{alpha: filepath.Join(base, "alpha"), beta: filepath.Join(base, "beta"), outside: filepath.Join(base, "outside")}
	for _, d := range []string{e.alpha, e.beta, e.outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(e.alpha, "server.properties"), []byte("motd=alpha\n"), 0o644)
	os.WriteFile(filepath.Join(e.beta, "server.properties"), []byte("motd=beta\n"), 0o644)
	os.WriteFile(filepath.Join(base, "secret.txt"), []byte("top secret"), 0o600)
	os.WriteFile(filepath.Join(e.outside, "x"), []byte("outside"), 0o600)
	os.Symlink(e.outside, filepath.Join(e.alpha, "out"))
	os.Symlink("../secret.txt", filepath.Join(e.alpha, "secret-link"))

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var err error
	if e.key, err = ssh.NewSignerFromKey(priv); err != nil {
		t.Fatal(err)
	}
	e.files = &fakeFiles{dirs: map[string]string{"vol-alpha": e.alpha, "vol-beta": e.beta}}
	e.panel = &fakePanel{
		passwords: map[string]string{"alpha": "alpha-secret", "beta": "beta-secret"},
		keys:      map[string][]byte{"alpha": e.key.PublicKey().Marshal()},
	}
	keyDir := filepath.Join(base, "state")
	if _, err := hostkey.Ensure(keyDir, os.Getgid(), ""); err != nil {
		t.Fatal(err)
	}
	e.server = &Server{Panel: e.panel, Files: e.files, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HostKeyPath: filepath.Join(keyDir, hostkey.File), MaxPerIP: 6, IdleExit: idle}
	if set != nil {
		set(e.server)
	}
	if err := e.server.LoadHostKey(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.served = make(chan error, 1)
	go func() { e.served <- e.server.Serve(ctx, l) }()
	t.Cleanup(func() { cancel(); <-e.served })
	e.addr = l.Addr().String()
	return e
}

func (e *env) dial(user string, auth ssh.AuthMethod) (*ssh.Client, error) {
	return ssh.Dial("tcp", e.addr, &ssh.ClientConfig{
		User: user, Auth: []ssh.AuthMethod{auth}, Timeout: 10 * time.Second,
		HostKeyCallback: ssh.FixedHostKey(e.server.hostKey.PublicKey()),
	})
}

func (e *env) sftpAs(t *testing.T, user string, auth ssh.AuthMethod) *sftp.Client {
	t.Helper()
	c, err := e.dial(user, auth)
	if err != nil {
		t.Fatalf("log in as %s: %v", user, err)
	}
	t.Cleanup(func() { c.Close() })
	s, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestHostKeyIsKept(t *testing.T) {
	e := newEnv(t)
	again := &Server{HostKeyPath: e.server.HostKeyPath}
	if err := again.LoadHostKey(); err != nil {
		t.Fatal(err)
	}
	if again.Fingerprint() != e.server.Fingerprint() || !strings.HasPrefix(again.Fingerprint(), "SHA256:") {
		t.Errorf("the host key changed: %s and %s", again.Fingerprint(), e.server.Fingerprint())
	}
	if st, err := os.Stat(e.server.HostKeyPath); err != nil || st.Mode().Perm() != 0o640 {
		t.Errorf("host key file: %v %v", st, err)
	}
}

func TestExitsWhenIdle(t *testing.T) {
	e := newEnvIdle(t, 50*time.Millisecond)
	select {
	case err := <-e.served:
		if err != nil {
			t.Errorf("an idle exit is not an error: %v", err)
		}
		e.served <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not leave while idle")
	}
}

func TestStaysWhileAConnectionIsOpen(t *testing.T) {
	e := newEnvIdle(t, 100*time.Millisecond)
	c, err := e.dial("alpha", ssh.Password("alpha-secret"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.served:
		t.Fatal("the server left with a connection open")
	case <-time.After(500 * time.Millisecond):
	}
	// The idle time counts from the last connection closing.
	c.Close()
	select {
	case err := <-e.served:
		if err != nil {
			t.Errorf("an idle exit is not an error: %v", err)
		}
		e.served <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("the server did not leave after the connection closed")
	}
}

func TestPasswordAndKeyLogin(t *testing.T) {
	e := newEnv(t)
	for name, c := range map[string]*sftp.Client{
		"password": e.sftpAs(t, "alpha", ssh.Password("alpha-secret")),
		"key":      e.sftpAs(t, "alpha", ssh.PublicKeys(e.key)),
	} {
		f, err := c.Open("/server.properties")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, _ := io.ReadAll(f)
		f.Close()
		if string(b) != "motd=alpha\n" {
			t.Errorf("%s: read %q", name, b)
		}
	}
	if ip := e.panel.seenIPs[0]; ip != "127.0.0.1" {
		t.Errorf("the panel was told the address %q", ip)
	}
}

func TestLoginsThatAreRefused(t *testing.T) {
	e := newEnv(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	other, _ := ssh.NewSignerFromKey(otherPriv)
	for name, tc := range map[string]struct {
		user string
		auth ssh.AuthMethod
	}{
		"wrong password":                  {"alpha", ssh.Password("nope")},
		"the other server's password":     {"alpha", ssh.Password("beta-secret")},
		"the panel password":              {"alpha", ssh.Password("the-panel-password")},
		"a key that is not on an account": {"alpha", ssh.PublicKeys(other)},
		"a key for another server":        {"beta", ssh.PublicKeys(e.key)},
		"a server that is not there":      {"gamma", ssh.Password("alpha-secret")},
		"no user":                         {"", ssh.Password("alpha-secret")},
		"a very long user":                {strings.Repeat("a", 200), ssh.Password("x")},
	} {
		if c, err := e.dial(tc.user, tc.auth); err == nil {
			c.Close()
			t.Errorf("%s: logged in", name)
		}
	}
}

func TestEachServerSeesItsOwnFiles(t *testing.T) {
	e := newEnv(t)
	a := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	b := e.sftpAs(t, "beta", ssh.Password("beta-secret"))
	f, err := a.Create("/only-alpha.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("a"))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.alpha, "only-alpha.txt")); err != nil {
		t.Error("the file is not in alpha's volume")
	}
	if _, err := os.Stat(filepath.Join(e.beta, "only-alpha.txt")); err == nil {
		t.Error("the file is in beta's volume")
	}
	if _, err := b.Stat("/only-alpha.txt"); err == nil {
		t.Error("beta sees alpha's file")
	}
	infos, err := b.ReadDir("/")
	if err != nil || len(infos) != 1 || infos[0].Name() != "server.properties" {
		t.Errorf("beta lists %v, %v", infos, err)
	}
}

func TestFileOperations(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))

	infos, err := c.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, fi := range infos {
		names[fi.Name()] = true
	}
	if !names["server.properties"] {
		t.Errorf("the listing lacks server.properties: %v", names)
	}

	if err := c.Mkdir("/plugins"); err != nil {
		t.Fatal(err)
	}
	if err := c.Mkdir("/plugins"); err == nil {
		t.Error("made a folder that exists")
	}

	// A file large enough for the client to send many blocks at once.
	data := make([]byte, 6<<20+123)
	rand.Read(data)
	f, err := c.Create("/plugins/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadFrom(bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(e.alpha, "plugins", "big.bin")); err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("the uploaded file differs (%d bytes, %v)", len(got), err)
	}
	if len(e.files.rooms) == 0 || e.files.rooms[len(e.files.rooms)-1] == nil {
		t.Error("the upload was not given the disk room")
	}

	r, err := c.Open("/plugins/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("the downloaded file differs (%d bytes, %v)", len(got), err)
	}
	if st, err := c.Stat("/plugins/big.bin"); err != nil || st.Size() != int64(len(data)) || st.IsDir() {
		t.Errorf("stat: %v %v", st, err)
	}

	empty, err := c.Create("/plugins/empty")
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(e.alpha, "plugins", "empty")); err != nil || st.Size() != 0 {
		t.Errorf("empty upload: %v %v", st, err)
	}

	if err := c.Rename("/plugins/big.bin", "/plugins/renamed.bin"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat("/plugins/big.bin"); err == nil {
		t.Error("the old name is still there")
	}
	if err := c.RemoveDirectory("/plugins"); err == nil {
		t.Error("removed a folder with files in it")
	}
	if err := c.Remove("/plugins"); err == nil {
		t.Error("remove took a folder")
	}
	if err := c.RemoveDirectory("/plugins/renamed.bin"); err == nil {
		t.Error("rmdir took a file")
	}
	for _, p := range []string{"/plugins/renamed.bin", "/plugins/empty"} {
		if err := c.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.RemoveDirectory("/plugins"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Stat("/plugins"); err == nil {
		t.Error("the folder is still there")
	}
	// A mode and a length are applied, not only answered.
	props := filepath.Join(e.alpha, "server.properties")
	if err := c.Chmod("/server.properties", 0o600); err != nil {
		t.Errorf("chmod: %v", err)
	}
	if st, _ := os.Stat(props); st.Mode() != 0o600 {
		t.Errorf("mode after chmod 600: %v", st.Mode())
	}
	// Only the nine bits of read, write and execute are the client's to set.
	if err := c.Chmod("/server.properties", 0o4755); err != nil {
		t.Errorf("chmod 4755: %v", err)
	}
	if st, _ := os.Stat(props); st.Mode() != 0o755 {
		t.Errorf("mode after chmod 4755: %v, want 755 with no special bit", st.Mode())
	}
	e.files.rooms = nil
	if err := c.Truncate("/server.properties", 4); err != nil {
		t.Errorf("truncate: %v", err)
	}
	if got, _ := os.ReadFile(props); string(got) != "motd" {
		t.Errorf("content after truncate to 4: %q", got)
	}
	if len(e.files.rooms) != 1 || e.files.rooms[0] == nil {
		t.Errorf("the truncate was not told the room the volume has: %v", e.files.rooms)
	}
	if err := c.Chmod("/nothing", 0o600); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("chmod of a file that is not there: %v", err)
	}
	if err := c.Truncate("/nothing", 0); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("truncate of a file that is not there: %v", err)
	}
	// The owner is not the client's to change, and it is told so.
	for name, err := range map[string]error{"chown": c.Chown("/server.properties", 0, 0), "chgrp": c.Chown("/server.properties", -1, 0)} {
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("%s: %v, want a permission error", name, err)
		}
	}
	// Times are what clients send after an upload, and they are set.
	at, mt := time.Unix(1700000000, 0), time.Unix(1600000000, 0)
	if err := c.Chtimes("/server.properties", at, mt); err != nil {
		t.Errorf("setting times: %v", err)
	}
	if st, _ := os.Stat(props); !st.ModTime().Equal(mt) {
		t.Errorf("modified %v after setting %v", st.ModTime(), mt)
	}
	if err := c.Chtimes("/nothing", at, mt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("times of a file that is not there: %v", err)
	}
	if err := c.Symlink("/server.properties", "/link"); err == nil {
		t.Error("made a link")
	}
}

// OpenSSH's sftp put -p and scp -p set the mode on the open file before they
// close it. The core has no file at the path until the upload is done, so
// the mode waits for that.
func TestModeSetOnAnOpenUploadIsApplied(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	mode := func(name string) fs.FileMode {
		st, err := os.Stat(filepath.Join(e.alpha, name))
		if err != nil {
			t.Fatal(err)
		}
		return st.Mode()
	}

	// A new file.
	f, err := c.Create("/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("new content"))
	if err := f.Chmod(0o640); err != nil {
		t.Fatalf("chmod on an open upload: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := mode("new.txt"); got != 0o640 {
		t.Errorf("new.txt is %v, want the mode that was set", got)
	}
	if b, _ := os.ReadFile(filepath.Join(e.alpha, "new.txt")); string(b) != "new content" {
		t.Errorf("content %q", b)
	}

	// A file that is replaced.
	f, err = c.OpenFile("/server.properties", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("motd=again\n"))
	if err := f.Chmod(0o4700); err != nil {
		t.Fatalf("chmod on an open upload: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := mode("server.properties"); got != 0o700 {
		t.Errorf("server.properties is %v, want 700 with no special bit", got)
	}

	// If the mode cannot be set when the file is in, the client hears of it
	// when it closes the file, not never.
	f, err = c.Create("/failed.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("x"))
	f.Chmod(0o600)
	e.files.setFailMode(errors.New("the disk went away"))
	err = f.Close()
	e.files.setFailMode(nil)
	if err == nil {
		t.Error("an upload whose mode could not be set closed without an error")
	}

	// A file that is abandoned halfway keeps nothing, mode included.
	f, err = c.Create("/half.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("half"))
	f.Chmod(0o600)
	c.Close()
	for range 100 {
		if _, err := os.Stat(filepath.Join(e.alpha, "half.txt.tmp")); err != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(e.alpha, "half.txt")); err == nil {
		t.Error("an upload that was cut off left a file")
	}
}

// OpenSSH's sftp put -p and scp -p also set the times of the open file, which
// are the times the file has once it is in.
func TestTimesSetOnAnOpenUploadAreApplied(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	modified := func(name string) time.Time {
		st, err := os.Stat(filepath.Join(e.alpha, name))
		if err != nil {
			t.Fatal(err)
		}
		return st.ModTime()
	}
	at, mt := time.Unix(1700000000, 0), time.Unix(1600000000, 0)

	// A new file, with the mode as well.
	f, err := c.Create("/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("new content"))
	if err := f.Chmod(0o640); err != nil {
		t.Fatal(err)
	}
	if err := c.Chtimes("/new.txt", at, mt); err != nil {
		t.Fatalf("times on an open upload: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := modified("new.txt"); !got.Equal(mt) {
		t.Errorf("new.txt was modified %v, want %v", got, mt)
	}
	if st, _ := os.Stat(filepath.Join(e.alpha, "new.txt")); st.Mode() != 0o640 {
		t.Errorf("new.txt is %v", st.Mode())
	}

	// A file that is replaced.
	f, err = c.OpenFile("/server.properties", os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("motd=again\n"))
	if err := c.Chtimes("/server.properties", at, mt); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := modified("server.properties"); !got.Equal(mt) {
		t.Errorf("server.properties was modified %v, want %v", got, mt)
	}

	// If the times cannot be set when the file is in, the client hears of it
	// when it closes the file.
	f, err = c.Create("/failed.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("x"))
	c.Chtimes("/failed.txt", at, mt)
	e.files.setFailMode(errors.New("the disk went away"))
	err = f.Close()
	e.files.setFailMode(nil)
	if err == nil {
		t.Error("an upload whose times could not be set closed without an error")
	}
}

// Some clients set the length of a file before they send any of it.
func TestLengthSetOnAnOpenUpload(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	size := func(name string) int64 {
		st, err := os.Stat(filepath.Join(e.alpha, name))
		if err != nil {
			t.Fatal(err)
		}
		return st.Size()
	}

	// The whole length first, then all of it.
	f, _ := c.Create("/pre.bin")
	if err := f.Truncate(10); err != nil {
		t.Fatalf("setting the length of an open upload: %v", err)
	}
	f.Write([]byte("0123456789"))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := size("pre.bin"); got != 10 {
		t.Errorf("pre.bin is %d bytes", got)
	}

	// A length above what was sent leaves a hole at the end.
	f, _ = c.Create("/hole.bin")
	f.Truncate(100)
	f.Write([]byte("0123456789"))
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := size("hole.bin"); got != 100 {
		t.Errorf("hole.bin is %d bytes, want the length that was set", got)
	}

	// Cutting what was already sent is not something to answer yes to.
	f, _ = c.Create("/cut.bin")
	f.Write([]byte("0123456789"))
	if err := f.Truncate(4); err == nil {
		t.Error("a length below what was sent was accepted")
	}
	f.Close()
}

// A folder is listed whole, however many items it holds.
func TestFolderOfAnySizeIsListedWhole(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	e.files.made = map[string]madeFolder{"/big": {n: 25000}, "/breaks": {n: 150, err: &core.Error{Status: 500, Message: "listing the folder failed, see the core log"}}}

	infos, err := c.ReadDir("/big")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 25000 {
		t.Fatalf("%d items, want 25000", len(infos))
	}
	seen := map[string]bool{}
	for _, fi := range infos {
		seen[fi.Name()] = true
	}
	if len(seen) != 25000 || !seen["file-1"] || !seen["file-25000"] {
		t.Errorf("%d different names", len(seen))
	}
	if e.files.opened.Load() != 1 {
		t.Errorf("%d listings were closed, want 1", e.files.opened.Load())
	}

	// A listing that breaks off is an error to the client, not a short list.
	infos, err = c.ReadDir("/breaks")
	if err == nil {
		t.Errorf("a listing that broke off after %d items came back whole", len(infos))
	}
	if e.files.opened.Load() != 2 {
		t.Errorf("%d listings were closed, want 2", e.files.opened.Load())
	}

	// A folder that is not there is still said so.
	if _, err := c.ReadDir("/nothing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a folder that is not there: %v", err)
	}
}

func TestWritesThatCannotBeHonoured(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	// Keeping part of an old file is not something a whole-file replace does.
	if f, err := c.OpenFile("/server.properties", os.O_WRONLY); err == nil {
		f.Close()
		t.Error("opened an existing file for writing without truncating it")
	}
	if f, err := c.OpenFile("/server.properties", os.O_WRONLY|os.O_APPEND); err == nil {
		f.Close()
		t.Error("opened a file for appending")
	}
	if f, err := c.OpenFile("/server.properties", os.O_WRONLY|os.O_CREATE|os.O_EXCL); err == nil {
		f.Close()
		t.Error("created a file that exists")
	}
	if f, err := c.OpenFile("/nothing", os.O_WRONLY|os.O_TRUNC); err == nil {
		f.Close()
		t.Error("opened a file that is not there, without creating it")
	}
	if got, _ := os.ReadFile(filepath.Join(e.alpha, "server.properties")); string(got) != "motd=alpha\n" {
		t.Errorf("a refused open changed the file: %q", got)
	}
	if f, err := c.OpenFile("/server.properties", os.O_WRONLY|os.O_TRUNC); err != nil {
		t.Errorf("opening for writing with truncation: %v", err)
	} else {
		f.Close()
	}
}

func TestPathsAreNotResolvedHere(t *testing.T) {
	e := newEnv(t)
	c := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	e.files.paths = nil

	// The client cleans a path lexically before the request is handled, so
	// ".." cannot climb above the top. What the core is asked about must
	// never contain it, and never be more than the client's own path.
	for _, p := range []string{"/../secret.txt", "../../secret.txt", "/plugins/../../secret.txt", "/../outside/x"} {
		if f, err := c.Open(p); err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			t.Errorf("%s: opened, read %q", p, b)
		}
	}
	// Links the volume holds are the core's to refuse, and it does.
	for _, p := range []string{"/out/x", "/secret-link", "/out"} {
		f, err := c.Open(p)
		if err == nil {
			b, _ := io.ReadAll(f)
			f.Close()
			if len(b) > 0 {
				t.Errorf("%s: read %q through a link that leaves the volume", p, b)
			}
		}
	}
	if _, err := c.ReadDir("/out"); err == nil {
		t.Error("listed a folder outside the volume through a link")
	}
	if f, err := c.Create("/out/new"); err == nil {
		f.Write([]byte("x"))
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(e.outside, "new")); err == nil {
		t.Error("a file was made outside the volume")
	}
	for _, p := range e.files.paths {
		if strings.Contains(p, "..") {
			t.Errorf("the core was asked about %q", p)
		}
	}
	if len(e.files.paths) == 0 {
		t.Error("no request reached the core")
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(e.alpha), "secret.txt")); string(b) != "top secret" {
		t.Errorf("the secret is now %q", b)
	}
}

func TestOnlySFTPIsServed(t *testing.T) {
	e := newEnv(t)
	c, err := e.dial("alpha", ssh.Password("alpha-secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for name, run := range map[string]func(*ssh.Session) error{
		"a shell":                      func(s *ssh.Session) error { return s.Shell() },
		"a command":                    func(s *ssh.Session) error { return s.Run("id") },
		"a subsystem that is not sftp": func(s *ssh.Session) error { return s.RequestSubsystem("netconf") },
		"a terminal":                   func(s *ssh.Session) error { return s.RequestPty("xterm", 24, 80, ssh.TerminalModes{}) },
	} {
		s, err := c.NewSession()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := run(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
		s.Close()
	}
	if conn, err := c.Dial("tcp", "127.0.0.1:1"); err == nil {
		conn.Close()
		t.Error("a port was forwarded")
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("a remote port was opened")
	}
	// Still usable after all that.
	if s, err := sftp.NewClient(c); err != nil {
		t.Errorf("sftp after the refusals: %v", err)
	} else {
		s.Close()
	}
}

func TestConnectionsPerAddress(t *testing.T) {
	e := newEnv(t)
	var open []net.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	for range 6 {
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	// Let the server count them.
	time.Sleep(200 * time.Millisecond)
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	// A refused connection is closed at once; an accepted one greets first.
	buf := make([]byte, 4)
	n, err := c.Read(buf)
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("the seventh connection from one address got %q, %v", buf[:n], err)
	}
	open[0].Close()
	time.Sleep(200 * time.Millisecond)
	if _, err := e.dial("alpha", ssh.Password("alpha-secret")); err != nil {
		t.Errorf("after one closed: %v", err)
	}
}

// newEnvWith is newEnv with the server's settings changed before it starts.
func newEnvWith(t *testing.T, set func(*Server)) *env {
	t.Helper()
	return newEnvIdleWith(t, 0, set)
}

func waitClosed(t *testing.T, c *ssh.Client, within time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(within):
		t.Fatal("the connection was not closed")
	}
}

func TestRevokedLoginIsClosed(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.CheckEvery = 50 * time.Millisecond })
	c, err := e.dial("alpha", ssh.Password("alpha-secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.ReadDir("/"); err != nil {
		t.Fatal(err)
	}
	e.panel.revoke("alpha")
	waitClosed(t, c, 5*time.Second)
}

func TestLoginStaysWhenThePanelCannotAnswer(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.CheckEvery = 20 * time.Millisecond })
	e.panel.mu.Lock()
	e.panel.checkErr = errors.New("panel is restarting")
	e.panel.mu.Unlock()
	sc := e.sftpAs(t, "alpha", ssh.Password("alpha-secret"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.panel.mu.Lock()
		n := e.panel.checks
		e.panel.mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the grant was not checked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := sc.ReadDir("/"); err != nil {
		t.Errorf("the connection was cut over a panel error: %v", err)
	}
}

func TestIdleLoginIsClosed(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.IdleTimeout = 300 * time.Millisecond })
	c, err := e.dial("alpha", ssh.Password("alpha-secret"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := sftp.NewClient(c)
	if err != nil {
		t.Fatal(err)
	}
	// Traffic keeps it open past the timeout.
	for range 6 {
		if _, err := sc.ReadDir("/"); err != nil {
			t.Fatalf("a busy connection was closed: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitClosed(t, c, 5*time.Second)
}

func TestLimitKey(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.7":               "203.0.113.7",
		"::ffff:203.0.113.7":        "203.0.113.7",
		"2001:db8:1:2:3:4:5:6":      "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::1": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":           "2001:db8:1:3::/64",
		"fe80::1%eth0":              "fe80::/64",
		"unknown":                   "unknown",
	} {
		if got := LimitKey(in); got != want {
			t.Errorf("LimitKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIPv6AddressesShareALimit(t *testing.T) {
	s := &Server{MaxPerIP: 2}
	for _, ip := range []string{"2001:db8::1", "2001:db8::2"} {
		if !s.hold(ip) {
			t.Fatalf("%s refused too early", ip)
		}
	}
	if s.hold("2001:db8::ffff") {
		t.Error("a third address of the same /64 was let in")
	}
	if !s.hold("2001:db8:0:1::1") {
		t.Error("another /64 was refused")
	}
	s.release("2001:db8::9")
	if !s.hold("2001:db8::3") {
		t.Error("a slot was not freed")
	}
}

func TestHandshakesAreCapped(t *testing.T) {
	s := &Server{MaxHandshakes: 2}
	if !s.startHandshake() || !s.startHandshake() {
		t.Fatal("refused under the cap")
	}
	if s.startHandshake() {
		t.Error("a third handshake was let in")
	}
	s.endHandshake()
	if !s.startHandshake() {
		t.Error("a freed handshake slot was not reusable")
	}
}

func TestHandshakeCapOverTheWire(t *testing.T) {
	e := newEnvWith(t, func(s *Server) { s.MaxHandshakes = 2; s.MaxPerIP = 10 })
	var open []net.Conn
	defer func() {
		for _, c := range open {
			c.Close()
		}
	}()
	// Two connections that say nothing hold both slots.
	for range 2 {
		c, err := net.Dial("tcp", e.addr)
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	time.Sleep(200 * time.Millisecond)
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 4)
	if n, err := c.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("a connection over the handshake cap got %q, %v", buf[:n], err)
	}
}
