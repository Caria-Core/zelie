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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/Caria-Core/zelie/internal/core"
)

// fakeFiles stands in for the core: each volume is a folder, and every path
// goes through an os.Root like the core's, so a link that leads out fails
// the same way. It remembers the paths it was asked about.
type fakeFiles struct {
	mu    sync.Mutex
	dirs  map[string]string
	paths []string
	rooms []*int64
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

func (f *fakeFiles) ListFiles(_ context.Context, ref core.FileRef, dir string) (core.FileList, error) {
	r, p, err := f.root(ref, dir)
	if err != nil {
		return core.FileList{}, err
	}
	defer r.Close()
	d, err := r.Open(p)
	if err != nil {
		return core.FileList{}, fail(err)
	}
	defer d.Close()
	des, err := d.ReadDir(-1)
	if err != nil {
		return core.FileList{}, fail(err)
	}
	out := core.FileList{Path: p}
	for _, de := range des {
		fi, err := de.Info()
		if err == nil {
			out.Entries = append(out.Entries, entry(de.Name(), fi))
		}
	}
	return out, nil
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

func (p *fakePanel) Config(context.Context, string) (int, error) { return 2222, nil }

type env struct {
	addr    string
	files   *fakeFiles
	panel   *fakePanel
	alpha   string // volume folders
	beta    string
	outside string
	server  *Server
	key     ssh.Signer
}

func newEnv(t *testing.T) *env {
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
	e.server = &Server{Panel: e.panel, Files: e.files, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HostKeyPath: filepath.Join(base, "state", "host_key"), MaxPerIP: 6}
	if err := e.server.LoadHostKey(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.server.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
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
	if st, err := os.Stat(e.server.HostKeyPath); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("host key file: %v %v", st, err)
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
	if err := c.Chmod("/server.properties", 0o600); err != nil {
		t.Errorf("setstat must be accepted and ignored: %v", err)
	}
	if err := c.Symlink("/server.properties", "/link"); err == nil {
		t.Error("made a link")
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
