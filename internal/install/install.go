// Package install sets Zelie up on a fresh server: its users, its services
// and how the panel is reached. Every step can run again, so an install that
// stopped halfway is finished by running it once more.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/proxy"
)

// How the panel is reached.
type Mode string

const (
	// ModeDomain: the proxy answers on 80 and 443 and gets certificates
	// from Let's Encrypt.
	ModeDomain Mode = "domain"
	// ModeIP: no domain; the proxy serves a self-signed certificate.
	ModeIP Mode = "ip"
	// ModeTunnel: a Cloudflare Tunnel brings the traffic; the proxy only
	// listens on a loopback port.
	ModeTunnel Mode = "tunnel"
)

// ErrAdminExists is what SetupLink returns on a server set up before.
var ErrAdminExists = errors.New("an administrator exists already")

// DefaultTunnelPort is where the proxy listens behind a tunnel. It clashes
// with neither the usual web ports nor Pterodactyl's and Pelican's 8080.
const DefaultTunnelPort = 8480

// Options are the answers the install needs.
type Options struct {
	Mode  Mode
	Host  string // the panel's domain, or the server's IP address
	Email string // for Let's Encrypt, in domain mode
	Port  int    // loopback port, in tunnel mode
	// Cloudflared is the command Cloudflare shows for connecting a new
	// tunnel, pasted as it is. Only needed when no connector runs yet.
	Cloudflared string
}

var email = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`) })

// Check reports what is wrong with the answers, if anything.
func (o *Options) Check() error {
	o.Host = strings.ToLower(strings.TrimSpace(o.Host))
	switch o.Mode {
	case ModeDomain, ModeTunnel:
		if !proxy.ValidDomain(o.Host) {
			return fmt.Errorf("%q is not a domain name", o.Host)
		}
	case ModeIP:
		if _, err := netip.ParseAddr(o.Host); err != nil {
			return fmt.Errorf("%q is not an IP address", o.Host)
		}
	default:
		return fmt.Errorf("unknown mode %q", o.Mode)
	}
	if o.Mode == ModeDomain && !email().MatchString(o.Email) {
		return fmt.Errorf("%q is not an email address", o.Email)
	}
	if o.Mode == ModeTunnel && (o.Port < 1024 || o.Port > 65535) {
		return fmt.Errorf("port %d is outside 1024-65535", o.Port)
	}
	return nil
}

// Installer does the install. Its fields that touch the machine are
// replaced in tests.
type Installer struct {
	Opts Options
	Out  io.Writer
	// Root is prepended to every path written; empty on a real server.
	Root string
	// Exec runs a command and returns its combined output.
	Exec ExecFunc
	// Engine installs containerd and runc.
	Engine func(ctx context.Context) error
	// Proxy applies the proxy's configuration once it runs.
	Proxy func(ctx context.Context, cfg proxy.Config) error
	// SetupLink makes the one-time link for the first administrator. It
	// returns ErrAdminExists once there is one.
	SetupLink func(ctx context.Context) (string, error)
	// Cloudflared installs the tunnel's connector; see cloudflared.go.
	Cloudflared func(ctx context.Context, token string) error
	// Self is the binary running the install.
	Self string

	results []result
}

type result struct {
	step, note string
	done       bool
}

// Run installs Zelie and prints what it did.
func (in *Installer) Run(ctx context.Context) error {
	if err := in.Opts.Check(); err != nil {
		return err
	}
	steps := []struct {
		name string
		fn   func(context.Context) (bool, string, error)
	}{
		{"Check the server", in.checkServer},
		{"Install the binary", in.binary},
		{"Create users", in.users},
		{"Install containerd", in.engine},
		{"Install services", in.services},
		{"Open the SFTP port", in.sftpPort},
		{"Configure the proxy", in.proxy},
		{"Connect the tunnel", in.tunnel},
	}
	for _, s := range steps {
		fmt.Fprintf(in.Out, "· %s…\n", s.name)
		done, note, err := s.fn(ctx)
		if err != nil {
			in.summary()
			return fmt.Errorf("%s: %w", strings.ToLower(s.name[:1])+s.name[1:], err)
		}
		in.results = append(in.results, result{s.name, note, done})
	}
	// The panel may still be starting.
	var link string
	var err error
	for range 30 {
		if link, err = in.SetupLink(ctx); err == nil || errors.Is(err, ErrAdminExists) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	in.summary()
	switch {
	case errors.Is(err, ErrAdminExists):
		fmt.Fprintf(in.Out, "\nZelie is up to date and running. Log in at https://%s.\n", in.Opts.Host)
		return nil
	case err != nil:
		return fmt.Errorf("make the setup link: %w", err)
	}
	fmt.Fprintf(in.Out, "\nZelie is installed. Open this link within 24 hours to create the administrator:\n\n  %s\n\n", link)
	if in.Opts.Mode == ModeTunnel {
		fmt.Fprintf(in.Out, "In Cloudflare, send %s, and every app domain you add later, to http://127.0.0.1:%d.\n", in.Opts.Host, in.Opts.Port)
	}
	return nil
}

func (in *Installer) summary() {
	fmt.Fprintln(in.Out)
	for _, r := range in.results {
		mark := "done   "
		if !r.done {
			mark = "skipped"
		}
		if r.note != "" {
			fmt.Fprintf(in.Out, "  %s  %s: %s\n", mark, r.step, r.note)
		} else {
			fmt.Fprintf(in.Out, "  %s  %s\n", mark, r.step)
		}
	}
}

func (in *Installer) path(p string) string { return filepath.Join(in.Root, p) }

func (in *Installer) checkServer(ctx context.Context) (bool, string, error) {
	if _, err := os.Stat(in.path("/run/systemd/system")); err != nil {
		return false, "", errors.New("this server does not run systemd")
	}
	osRelease, err := os.ReadFile(in.path("/etc/os-release"))
	if err != nil {
		return false, "", err
	}
	id := releaseField(string(osRelease), "ID")
	if id != "debian" && id != "ubuntu" {
		return false, "", fmt.Errorf("Zelie runs on Debian and Ubuntu, and this is %q", id)
	}
	if _, err := os.Stat(in.path("/sys/fs/cgroup/cgroup.controllers")); err != nil {
		return false, "", errors.New("this server does not use cgroup v2")
	}
	if in.Opts.Mode == ModeTunnel {
		if err := portFree(in.Opts.Port); err != nil {
			return false, "", err
		}
	}
	return true, releaseField(string(osRelease), "PRETTY_NAME"), nil
}

// portFree reports whether the proxy can listen on the loopback port. An
// install run again finds its own proxy there, which is fine.
func portFree(port int) error {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err == nil {
		l.Close()
		return nil
	}
	if _, err := os.Stat("/run/zelie-proxy/proxy.sock"); err == nil {
		return nil
	}
	return fmt.Errorf("port %d is already in use; choose another with --port", port)
}

func releaseField(osRelease, key string) string {
	for _, line := range strings.Split(osRelease, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

// binary puts the running binary where the services expect it, unless it
// is already there.
func (in *Installer) binary(ctx context.Context) (bool, string, error) {
	target := in.path(Binary)
	if same, _ := filepath.EvalSymlinks(in.Self); same == target {
		return false, "already in " + Binary, nil
	}
	data, err := os.ReadFile(in.Self)
	if err != nil {
		return false, "", err
	}
	tmp := target + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return false, "", err
	}
	// Renamed into place, so a running service keeps its old file.
	if err := os.Rename(tmp, target); err != nil {
		return false, "", err
	}
	return true, Binary, nil
}

func (in *Installer) users(ctx context.Context) (bool, string, error) {
	var made []string
	for _, u := range []string{PanelUser, ProxyUser, SFTPUser} {
		ok, err := ensureUser(ctx, in.Exec, u)
		if err != nil {
			return false, "", err
		}
		if ok {
			made = append(made, u)
		}
	}
	if len(made) == 0 {
		return false, "they exist", nil
	}
	return true, strings.Join(made, ", "), nil
}

// ensureUser makes a system user that cannot log in, unless it exists. It
// reports whether it made one.
func ensureUser(ctx context.Context, run ExecFunc, name string) (bool, error) {
	if _, err := run(ctx, "id", "-u", name); err == nil {
		return false, nil
	}
	if out, err := run(ctx, "useradd", "--system", "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", name); err != nil {
		return false, fmt.Errorf("useradd %s: %v: %s", name, err, out)
	}
	return true, nil
}

// ExecFunc runs a command and returns its combined output.
type ExecFunc func(ctx context.Context, name string, args ...string) (string, error)

func (in *Installer) engine(ctx context.Context) (bool, string, error) {
	return true, "", in.Engine(ctx)
}

func (in *Installer) services(ctx context.Context) (bool, string, error) {
	units := Units(in.Opts)
	// A service that runs and whose unit changes must restart to take the
	// change. One that did not run starts fresh below, and restarting it
	// right after would only race whoever talks to it next.
	// So must one that runs an older binary than the one now in place.
	var restart []string
	all := allServices()
	for i, name := range unitNames() {
		file := in.path(filepath.Join(UnitDir, name))
		changed := true
		if old, err := os.ReadFile(file); err == nil && string(old) == units[name] {
			changed = false
		} else if err := os.WriteFile(file, []byte(units[name]), 0o644); err != nil {
			return false, "", err
		}
		if out, err := in.Exec(ctx, "systemctl", "is-active", all[i]); err != nil || strings.TrimSpace(out) != "active" {
			continue
		}
		if changed || in.staleBinary(ctx, all[i]) {
			restart = append(restart, all[i])
		}
	}
	if out, err := in.Exec(ctx, "systemctl", "daemon-reload"); err != nil {
		return false, "", fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	args := append([]string{"enable", "--now"}, all...)
	if out, err := in.Exec(ctx, "systemctl", args...); err != nil {
		return false, "", fmt.Errorf("systemctl enable: %v: %s", err, out)
	}
	if len(restart) > 0 {
		args = append([]string{"restart"}, restart...)
		if out, err := in.Exec(ctx, "systemctl", args...); err != nil {
			return false, "", fmt.Errorf("systemctl restart: %v: %s", err, out)
		}
	}
	return true, strings.Join(all, ", "), nil
}

// sftpPort lets the SFTP port through ufw, when ufw is on. Other firewalls
// are left to their owners, as Zelie does not know their rules.
func (in *Installer) sftpPort(ctx context.Context) (bool, string, error) {
	return openSFTPPort(ctx, in.Exec)
}

// staleBinary reports whether a running service's process runs a binary
// that has since been replaced: the kernel marks the file it was started
// from as deleted.
func (in *Installer) staleBinary(ctx context.Context, service string) bool {
	out, err := in.Exec(ctx, "systemctl", "show", "--property", "MainPID", "--value", service)
	pid := strings.TrimSpace(out)
	if err != nil || pid == "" || pid == "0" {
		return false
	}
	exe, err := os.Readlink(in.path(filepath.Join("/proc", pid, "exe")))
	return err == nil && (strings.HasSuffix(exe, " (deleted)") || exe != Binary)
}

func (in *Installer) proxy(ctx context.Context) (bool, string, error) {
	cfg := proxy.Config{Panel: in.Opts.Host}
	switch in.Opts.Mode {
	case ModeDomain:
		cfg.TLS, cfg.Email = proxy.TLSACME, in.Opts.Email
	case ModeIP:
		cfg.TLS = proxy.TLSSelfSigned
	case ModeTunnel:
		cfg.TLS = proxy.TLSTunnel
	}
	// The proxy may take a moment to come up after its service starts.
	var err error
	for range 30 {
		if err = in.Proxy(ctx, cfg); err == nil {
			return true, fmt.Sprintf("panel on %s, %s", cfg.Panel, cfg.TLS), nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return false, "", err
}

func (in *Installer) tunnel(ctx context.Context) (bool, string, error) {
	if in.Opts.Mode != ModeTunnel {
		return false, "not behind a tunnel", nil
	}
	if out, err := in.Exec(ctx, "systemctl", "is-active", "cloudflared"); err == nil && strings.TrimSpace(out) == "active" {
		return false, "a connector already runs; add the routes in Cloudflare", nil
	}
	if in.Opts.Cloudflared == "" {
		return false, "", errors.New("no tunnel connector runs here; paste the install command Cloudflare shows for a new tunnel")
	}
	token, err := TunnelToken(in.Opts.Cloudflared)
	if err != nil {
		return false, "", err
	}
	if err := in.Cloudflared(ctx, token); err != nil {
		return false, "", err
	}
	return true, "cloudflared installed and connected", nil
}
