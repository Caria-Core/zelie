package install

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/proxy"
)

func TestCheck(t *testing.T) {
	good := []Options{
		{Mode: ModeTunnel, Host: "Ist.Cariacore.com ", Port: 8480},
		{Mode: ModeDomain, Host: "panel.example.com", Email: "me@example.com"},
		{Mode: ModeIP, Host: "203.0.113.9"},
	}
	for i := range good {
		if err := good[i].Check(); err != nil {
			t.Errorf("%+v: %v", good[i], err)
		}
	}
	if good[0].Host != "ist.cariacore.com" {
		t.Errorf("host not cleaned: %q", good[0].Host)
	}
	bad := []Options{
		{Mode: "web", Host: "a.example.com"},
		{Mode: ModeTunnel, Host: "not a domain", Port: 8480},
		{Mode: ModeTunnel, Host: "a.example.com", Port: 80},
		{Mode: ModeDomain, Host: "a.example.com", Email: "nope"},
		{Mode: ModeIP, Host: "a.example.com"},
	}
	for _, o := range bad {
		if err := o.Check(); err == nil {
			t.Errorf("%+v passed", o)
		}
	}
}

const token = "eyJhIjoiMTIzNDU2Nzg5MGFiY2RlZiIsInQiOiJhYmNkZWYtMTIzNCIsInMiOiJzZWNyZXQifQ=="

func TestTunnelToken(t *testing.T) {
	for _, cmd := range []string{
		"sudo cloudflared service install " + token,
		"cloudflared service install " + token,
		"curl -L --output cloudflared.deb https://example/cloudflared.deb &&\nsudo dpkg -i cloudflared.deb &&\nsudo cloudflared service install " + token + "\n",
	} {
		if got, err := TunnelToken(cmd); err != nil || got != token {
			t.Errorf("%q: %q, %v", cmd, got, err)
		}
	}
	for _, cmd := range []string{"", "cloudflared service install", "cloudflared tunnel run " + token, "rm -rf /; cloudflared service install short"} {
		if _, err := TunnelToken(cmd); err == nil {
			t.Errorf("%q passed", cmd)
		}
	}
}

func TestUnits(t *testing.T) {
	tunnel := Units(Options{Mode: ModeTunnel, Port: 8480})
	p := tunnel["zelie-proxy.service"]
	if !strings.Contains(p, "proxy -http 127.0.0.1:8480 -https ''") || strings.Contains(p, "CAP_NET_BIND_SERVICE") {
		t.Errorf("tunnel proxy unit:\n%s", p)
	}
	domain := Units(Options{Mode: ModeDomain})
	p = domain["zelie-proxy.service"]
	if !strings.Contains(p, "proxy -http :80 -https ':443'") || !strings.Contains(p, "AmbientCapabilities=CAP_NET_BIND_SERVICE") {
		t.Errorf("domain proxy unit:\n%s", p)
	}
	for name, u := range domain {
		if name != "zelie-core.service" && (!strings.Contains(u, "User=") || !strings.Contains(u, "NoNewPrivileges=yes")) {
			t.Errorf("%s runs with too much:\n%s", name, u)
		}
	}
	if c := domain["zelie-core.service"]; !strings.Contains(c, "KillMode=process") || strings.Contains(c, "User=") {
		t.Errorf("core unit:\n%s", c)
	}
}

// fakeServer is a root directory that looks like a Debian server, and a
// record of the commands run on it.
type fakeServer struct {
	root     string
	users    map[string]bool
	commands []string
	proxy    proxy.Config
	token    string
	active   bool // cloudflared runs already
	running  bool // Zelie's services run
	stale    bool // they run a binary that was replaced
}

func newFakeServer(t *testing.T) *fakeServer {
	root := t.TempDir()
	for _, d := range []string{"run/systemd/system", "etc", "sys/fs/cgroup", "usr/local/bin", "etc/systemd/system"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "etc/os-release"), []byte("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\n"), 0o644)
	os.WriteFile(filepath.Join(root, "sys/fs/cgroup/cgroup.controllers"), []byte("cpu memory\n"), 0o644)
	return &fakeServer{root: root, users: map[string]bool{}}
}

func (f *fakeServer) setStale(t *testing.T, stale bool) {
	dir := filepath.Join(f.root, "proc/4242")
	os.MkdirAll(dir, 0o755)
	os.Remove(filepath.Join(dir, "exe"))
	target := Binary
	if stale {
		target += " (deleted)"
	}
	if err := os.Symlink(target, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeServer) installer(t *testing.T, opts Options, out *bytes.Buffer) *Installer {
	self := filepath.Join(t.TempDir(), "zelie")
	os.WriteFile(self, []byte("binary"), 0o755)
	return &Installer{
		Opts: opts, Out: out, Root: f.root, Self: self,
		Exec: func(_ context.Context, name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			f.commands = append(f.commands, cmd)
			switch {
			case name == "id":
				if !f.users[args[1]] {
					return "no such user", errors.New("exit status 1")
				}
			case name == "useradd":
				f.users[args[len(args)-1]] = true
			case strings.HasPrefix(cmd, "systemctl is-active zelie-"):
				if !f.running {
					return "inactive", errors.New("exit status 3")
				}
				return "active\n", nil
			case strings.HasPrefix(cmd, "systemctl show --property MainPID --value zelie-"):
				return "4242\n", nil
			case cmd == "systemctl is-active cloudflared":
				if !f.active {
					return "inactive", errors.New("exit status 3")
				}
				return "active\n", nil
			}
			return "", nil
		},
		Engine:      func(context.Context) error { return nil },
		Proxy:       func(_ context.Context, cfg proxy.Config) error { f.proxy = cfg; return nil },
		SetupLink:   func(context.Context) (string, error) { return "https://" + opts.Host + "/setup#abc", nil },
		Cloudflared: func(_ context.Context, token string) error { f.token = token; return nil },
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestInstallBehindTunnel(t *testing.T) {
	f := newFakeServer(t)
	opts := Options{Mode: ModeTunnel, Host: "ist.cariacore.com", Port: freePort(t), Cloudflared: "sudo cloudflared service install " + token}
	var out bytes.Buffer
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, Binary)); string(b) != "binary" {
		t.Error("binary not installed")
	}
	if !f.users[PanelUser] || !f.users[ProxyUser] {
		t.Errorf("users %v", f.users)
	}
	for _, name := range unitNames() {
		if _, err := os.Stat(filepath.Join(f.root, UnitDir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, c := range f.commands {
		// Fresh services start once; restarting them would race the
		// setup link.
		if strings.HasPrefix(c, "systemctl restart") {
			t.Errorf("first run: %s", c)
		}
	}
	if f.proxy.TLS != proxy.TLSTunnel || f.proxy.Panel != "ist.cariacore.com" {
		t.Errorf("proxy %+v", f.proxy)
	}
	if f.token != token {
		t.Errorf("connector got %q", f.token)
	}
	text := out.String()
	for _, want := range []string{"https://ist.cariacore.com/setup#abc", "http://127.0.0.1:", "Debian GNU/Linux 12"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, token) {
		t.Error("the output shows the tunnel token")
	}

	// Again, as after an interruption: nothing is made twice, and a
	// connector that now runs is left alone.
	f.commands, f.token, f.active = nil, "", true
	out.Reset()
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("second run: %v\n%s", err, out.String())
	}
	for _, c := range f.commands {
		if strings.HasPrefix(c, "useradd") || strings.HasPrefix(c, "systemctl restart") {
			t.Errorf("second run: %s", c)
		}
	}
	if f.token != "" || !strings.Contains(out.String(), "a connector already runs") {
		t.Errorf("second run touched the connector:\n%s", out.String())
	}

	// Running services on the current binary and unchanged units: nothing
	// restarts.
	f.commands, f.running = nil, true
	f.setStale(t, false)
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("run on current binary: %v", err)
	}
	for _, c := range f.commands {
		if strings.HasPrefix(c, "systemctl restart") {
			t.Errorf("nothing changed, yet: %s", c)
		}
	}

	// A new binary in place, as install.sh leaves it: all three restart.
	f.commands = nil
	f.setStale(t, true)
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("run after an update: %v", err)
	}
	if !slices.Contains(f.commands, "systemctl restart zelie-core zelie-proxy zelie-panel") {
		t.Errorf("after an update: %v", f.commands)
	}

	// A new port changes the proxy's unit only, and the running proxy
	// restarts to take it.
	f.commands = nil
	f.setStale(t, false)
	opts.Port = freePort(t)
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("third run: %v", err)
	}
	var restarts []string
	for _, c := range f.commands {
		if strings.HasPrefix(c, "systemctl restart") {
			restarts = append(restarts, c)
		}
	}
	if strings.Join(restarts, "|") != "systemctl restart zelie-proxy" {
		t.Errorf("restarts %v", restarts)
	}
}

func TestReinstallWithAnAdministrator(t *testing.T) {
	f := newFakeServer(t)
	f.active = true
	var out bytes.Buffer
	in := f.installer(t, Options{Mode: ModeTunnel, Host: "ist.cariacore.com", Port: freePort(t)}, &out)
	in.SetupLink = func(context.Context) (string, error) { return "", ErrAdminExists }
	if err := in.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Log in at https://ist.cariacore.com") || strings.Contains(out.String(), "setup#") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestInstallNeedsConnectorCommand(t *testing.T) {
	f := newFakeServer(t)
	var out bytes.Buffer
	err := f.installer(t, Options{Mode: ModeTunnel, Host: "ist.cariacore.com", Port: freePort(t)}, &out).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "paste the install command") {
		t.Errorf("err %v", err)
	}
}

func TestInstallRefusesOtherSystems(t *testing.T) {
	f := newFakeServer(t)
	os.WriteFile(filepath.Join(f.root, "etc/os-release"), []byte("ID=fedora\n"), 0o644)
	var out bytes.Buffer
	err := f.installer(t, Options{Mode: ModeIP, Host: "203.0.113.9"}, &out).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "fedora") || len(f.commands) != 0 {
		t.Errorf("err %v, ran %v", err, f.commands)
	}
}

func TestInstallBusyPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	f := newFakeServer(t)
	var out bytes.Buffer
	port := l.Addr().(*net.TCPAddr).Port
	err = f.installer(t, Options{Mode: ModeTunnel, Host: "ist.cariacore.com", Port: port}, &out).Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Errorf("err %v", err)
	}
}
