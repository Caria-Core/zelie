package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/store"
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
		if name != "zelie-core.service" && name != SFTPSocket && (!strings.Contains(u, "User=") || !strings.Contains(u, "NoNewPrivileges=yes")) {
			t.Errorf("%s runs with too much:\n%s", name, u)
		}
	}
	if c := domain["zelie-core.service"]; !strings.Contains(c, "KillMode=process") || strings.Contains(c, "User=") {
		t.Errorf("core unit:\n%s", c)
	}
	// The update copies the panel's database from the directory systemd makes
	// for the panel, and the panel opens it there.
	if u := domain["zelie-panel.service"]; !strings.Contains(u, "\nStateDirectory="+filepath.Base(PanelState)+"\n") || filepath.Dir(PanelDB) != PanelState {
		t.Errorf("panel unit does not keep its state in %s:\n%s", PanelState, u)
	}
	// The SFTP server is a user of its own that binds a high port and does
	// nothing else, whichever way the panel is reached.
	for _, u := range []map[string]string{tunnel, domain} {
		sftp := u["zelie-sftp.service"]
		for _, want := range []string{"User=zelie-sftp\n", "ExecStart=" + Binary + " sftp\n", "CapabilityBoundingSet=\n", "NoNewPrivileges=yes", "ProtectSystem=strict", "StateDirectory=zelie-sftp", "Requires=zelie-sftp.socket\n", "Restart=on-failure\n", "SystemCallFilter=@system-service\n", "MemoryDenyWriteExecute=yes\n", "MemoryMax=512M\n"} {
			if !strings.Contains(sftp, want) {
				t.Errorf("SFTP unit lacks %q:\n%s", want, sftp)
			}
		}
		// The check comes before the start, so a binary that lacks the command
		// is skipped, not restarted in a loop.
		cond, start := strings.Index(sftp, "ExecCondition="+Binary+" sftp --check\n"), strings.Index(sftp, "ExecStart=")
		if cond < 0 || cond > start {
			t.Errorf("SFTP unit does not check the binary first:\n%s", sftp)
		}
		if strings.Contains(sftp, "[Install]") || strings.Contains(sftp, "Restart=always") {
			t.Errorf("SFTP unit is enabled itself or restarts after an idle exit:\n%s", sftp)
		}
		if sock := u[SFTPSocket]; !strings.Contains(sock, "ListenStream=2222\n") || !strings.Contains(sock, "Accept=no\n") || !strings.Contains(sock, "WantedBy=sockets.target") {
			t.Errorf("SFTP socket:\n%s", sock)
		}
		if strings.Contains(sftp, "AmbientCapabilities") {
			t.Errorf("SFTP unit may bind low ports:\n%s", sftp)
		}
	}
}

func TestSFTPPortMatchesTheStore(t *testing.T) {
	if DefaultSFTPPort != store.DefaultSFTPPort {
		t.Errorf("the installer opens %d, the panel starts on %d", DefaultSFTPPort, store.DefaultSFTPPort)
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
	if !f.users[PanelUser] || !f.users[ProxyUser] || !f.users[SFTPUser] {
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

	// A new binary in place, as install.sh leaves it: all four restart.
	f.commands = nil
	f.setStale(t, true)
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("run after an update: %v", err)
	}
	if !slices.Contains(f.commands, "systemctl restart zelie-core zelie-proxy zelie-panel zelie-sftp") {
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

func TestInstallBusyWebPort(t *testing.T) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	old := webPorts
	webPorts = []int{l.Addr().(*net.TCPAddr).Port}
	defer func() { webPorts = old }()
	for _, opts := range []Options{{Mode: ModeDomain, Host: "panel.example.com", Email: "a@example.com"}, {Mode: ModeIP, Host: "203.0.113.9"}} {
		f := newFakeServer(t)
		var out bytes.Buffer
		err := f.installer(t, opts, &out).Run(context.Background())
		if err == nil || !strings.Contains(err.Error(), "in use") || !strings.Contains(err.Error(), "--mode tunnel") || len(f.commands) != 0 {
			t.Errorf("%s: err %v, ran %v", opts.Mode, err, f.commands)
		}
	}
}

func TestUFWIsOpenedWhenItIsOn(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		err          error
		wantAllow    bool
	}{
		{"active", "Status: active\n\nTo  Action\n", nil, true},
		{"inactive", "Status: inactive\n", nil, false},
		{"not installed", "", errors.New("not found"), false},
	} {
		var ran []string
		run := func(_ context.Context, name string, args ...string) (string, error) {
			cmd := name + " " + strings.Join(args, " ")
			ran = append(ran, cmd)
			if cmd == "ufw status" {
				return tc.status, tc.err
			}
			return "", nil
		}
		done, _, err := openSFTPPort(context.Background(), run)
		if err != nil || done != tc.wantAllow || slices.Contains(ran, "ufw allow 2222/tcp comment Zelie SFTP") != tc.wantAllow {
			t.Errorf("%s: %v %v, ran %v", tc.name, done, err, ran)
		}
	}
	// A firewall that says no does not stop the install.
	refuse := func(_ context.Context, name string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "allow" {
			return "ERROR", errors.New("exit status 1")
		}
		return "Status: active", nil
	}
	if done, note, err := openSFTPPort(context.Background(), refuse); done || err != nil || !strings.Contains(note, "would not allow") {
		t.Errorf("refused: %v %q %v", done, note, err)
	}
}

func TestSetUpSFTPOnAnOlderInstall(t *testing.T) {
	f := newFakeServer(t)
	f.users[PanelUser], f.users[ProxyUser] = true, true
	exec := func(_ context.Context, name string, args ...string) (string, error) {
		cmd := name + " " + strings.Join(args, " ")
		f.commands = append(f.commands, cmd)
		switch {
		case name == "id" && !f.users[args[1]]:
			return "no such user", errors.New("exit status 1")
		case name == "useradd":
			f.users[args[len(args)-1]] = true
		case cmd == "ufw status":
			return "Status: active\n", nil
		}
		return "", nil
	}
	ctx := context.Background()

	made, err := SetUpSFTP(ctx, exec, f.root)
	if err != nil || !made {
		t.Fatalf("first: %v %v", made, err)
	}
	if !f.users[SFTPUser] {
		t.Error("no user was made")
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, UnitDir, "zelie-sftp.service")); string(b) != SFTPUnit() {
		t.Errorf("unit:\n%s", b)
	}
	for _, want := range []string{"systemctl daemon-reload", "systemctl enable --now zelie-sftp.socket", "ufw allow 2222/tcp comment Zelie SFTP"} {
		if !slices.Contains(f.commands, want) {
			t.Errorf("did not run %q: %v", want, f.commands)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(f.root, UnitDir, "zelie-sftp.socket")); string(b) != SFTPSocketUnit() {
		t.Errorf("socket:\n%s", b)
	}
	// The other services keep running as they are.
	for _, c := range f.commands {
		if strings.HasPrefix(c, "systemctl restart") || strings.Contains(c, "zelie-core") {
			t.Errorf("touched what was there: %s", c)
		}
	}

	// On every later start it finds all in place and runs nothing more
	// than the check for the user.
	f.commands = nil
	made, err = SetUpSFTP(ctx, exec, f.root)
	if err != nil || made || len(f.commands) != 1 || f.commands[0] != "id -u zelie-sftp" {
		t.Errorf("second: %v %v %v", made, err, f.commands)
	}

	// A unit that was lost is made again, as is the socket on a server that
	// has only the service.
	os.Remove(filepath.Join(f.root, UnitDir, "zelie-sftp.socket"))
	if made, err := SetUpSFTP(ctx, exec, f.root); err != nil || !made {
		t.Errorf("after the unit was lost: %v %v", made, err)
	}

	// A unit from an older release, before its sandbox was tightened, is
	// brought up to date. The firewall was dealt with at the first setup, so
	// it is not touched, and a server that runs is not restarted.
	unit := filepath.Join(f.root, UnitDir, "zelie-sftp.service")
	os.WriteFile(unit, []byte("[Service]\nExecStart="+Binary+" sftp\n"), 0o644)
	f.commands = nil
	if made, err := SetUpSFTP(ctx, exec, f.root); err != nil || !made {
		t.Errorf("after the unit went out of date: %v %v", made, err)
	}
	if b, _ := os.ReadFile(unit); string(b) != SFTPUnit() {
		t.Errorf("the outdated unit stayed:\n%s", b)
	}
	if !slices.Contains(f.commands, "systemctl daemon-reload") {
		t.Errorf("the change was not loaded: %v", f.commands)
	}
	for _, c := range f.commands {
		if strings.HasPrefix(c, "ufw") || strings.Contains(c, "restart") {
			t.Errorf("an update of the unit ran %q", c)
		}
	}

	// Failing to make the user is an error to report.
	f = newFakeServer(t)
	bad := func(_ context.Context, name string, args ...string) (string, error) {
		return "boom", errors.New("exit status 1")
	}
	if _, err := SetUpSFTP(ctx, bad, f.root); err == nil {
		t.Error("no error when the user cannot be made")
	}
}

// Another program on the SFTP port, an SSH server on 2222 for one, must not
// stop the install: the Server page, where the port is changed, comes after.
func TestInstallGoesOnWhenTheSFTPPortIsTaken(t *testing.T) {
	f := newFakeServer(t)
	var out bytes.Buffer
	in := f.installer(t, Options{Mode: ModeTunnel, Host: "ist.cariacore.com", Port: freePort(t), Cloudflared: "sudo cloudflared service install " + token}, &out)
	exec := in.Exec
	in.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
		if cmd := name + " " + strings.Join(args, " "); cmd == "systemctl enable --now "+SFTPSocket {
			f.commands = append(f.commands, cmd)
			return "Job for zelie-sftp.socket failed.\nSee \"systemctl status zelie-sftp.socket\" for details.\n", errors.New("exit status 1")
		}
		return exec(ctx, name, args...)
	}
	if err := in.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if f.proxy.Panel != "ist.cariacore.com" {
		t.Errorf("the proxy was not configured: %+v", f.proxy)
	}
	text := out.String()
	for _, want := range []string{"zelie-core, zelie-proxy, zelie-panel;", "zelie-sftp.socket did not start (Job for zelie-sftp.socket failed. See", "choose another SFTP port", "/setup#abc"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	// The services that matter start without the socket in the same call.
	if !slices.Contains(f.commands, "systemctl enable --now zelie-core zelie-proxy zelie-panel") {
		t.Errorf("commands %v", f.commands)
	}

	// A failure of the services themselves still stops the install.
	f = newFakeServer(t)
	in = f.installer(t, Options{Mode: ModeIP, Host: "203.0.113.9"}, &out)
	exec = in.Exec
	in.Exec = func(ctx context.Context, name string, args ...string) (string, error) {
		if len(args) > 2 && args[0] == "enable" && args[2] == "zelie-core" {
			return "Failed to start zelie-core.service", errors.New("exit status 1")
		}
		return exec(ctx, name, args...)
	}
	if err := in.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "systemctl enable") {
		t.Errorf("err %v", err)
	}
}

func TestInstalledTunnelPort(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, UnitDir), 0o755)
	if _, ok := InstalledTunnelPort(root); ok {
		t.Error("a port from a server with no proxy unit")
	}
	for _, c := range []struct {
		opts Options
		port int
		ok   bool
	}{
		{Options{Mode: ModeTunnel, Port: 9000}, 9000, true},
		{Options{Mode: ModeTunnel, Port: DefaultTunnelPort}, DefaultTunnelPort, true},
		{Options{Mode: ModeDomain}, 0, false},
		{Options{Mode: ModeIP}, 0, false},
	} {
		os.WriteFile(filepath.Join(root, UnitDir, "zelie-proxy.service"), []byte(Units(c.opts)["zelie-proxy.service"]), 0o644)
		if port, ok := InstalledTunnelPort(root); port != c.port || ok != c.ok {
			t.Errorf("%s: %d, %v", c.opts.Mode, port, ok)
		}
	}
}

// Installing again with another port moves the proxy, and Cloudflare keeps
// sending traffic to the old one, so the install says so.
func TestInstallSaysWhenTheTunnelPortMoves(t *testing.T) {
	f := newFakeServer(t)
	f.active = true
	opts := Options{Mode: ModeTunnel, Host: "ist.cariacore.com", Port: freePort(t)}
	var out bytes.Buffer
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "point the Cloudflare routes") {
		t.Errorf("a first install moved nothing:\n%s", out.String())
	}
	was := opts.Port
	opts.Port = freePort(t)
	out.Reset()
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	want := fmt.Sprintf("the proxy listens on port %d now, not %d: point the Cloudflare routes at http://127.0.0.1:%d", opts.Port, was, opts.Port)
	if !strings.Contains(out.String(), want) {
		t.Errorf("output lacks %q:\n%s", want, out.String())
	}
	out.Reset()
	if err := f.installer(t, opts, &out).Run(context.Background()); err != nil || strings.Contains(out.String(), "point the Cloudflare routes") {
		t.Errorf("a second run with the same port: %v\n%s", err, out.String())
	}
}

func TestInstallLinksBracketAnIPv6Address(t *testing.T) {
	f := newFakeServer(t)
	var out bytes.Buffer
	in := f.installer(t, Options{Mode: ModeIP, Host: "2001:DB8::1"}, &out)
	in.SetupLink = func(context.Context) (string, error) { return "", ErrAdminExists }
	if err := in.Run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Log in at https://[2001:db8::1].") {
		t.Errorf("output:\n%s", out.String())
	}
}
