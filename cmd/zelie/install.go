package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/install"
	"github.com/Caria-Core/zelie/internal/panel"
	"github.com/Caria-Core/zelie/internal/proxy"
)

func runInstall(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "how the panel is reached: domain, tunnel or ip")
	host := fs.String("host", "", "the panel's domain, or this server's IP address")
	mail := fs.String("email", "", "email for Let's Encrypt, in domain mode")
	port := fs.Int("port", install.DefaultTunnelPort, "loopback port for the proxy, in tunnel mode")
	noSFTP := fs.Bool("no-sftp", false, "install with SFTP turned off: no port is opened and no firewall rule is added")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "zelie: install needs root")
		return 1
	}
	// Installing again keeps the port the tunnel already sends traffic to,
	// unless --port says otherwise.
	given := false
	fs.Visit(func(f *flag.Flag) { given = given || f.Name == "port" })
	if p, ok := install.InstalledTunnelPort(""); ok && !given {
		*port = p
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := install.Options{Mode: install.Mode(*mode), Host: *host, Email: *mail, Port: *port, NoSFTP: *noSFTP}
	in := bufio.NewReader(stdin)
	if err := ask(ctx, in, stdout, &opts); err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	cf := &install.Cloudflared{Arch: runtime.GOARCH, Exec: runCmd}
	inst := &install.Installer{
		Opts: opts, Out: stdout, Exec: runCmd, Self: self,
		Engine: func(ctx context.Context) error {
			return (&engine.Installer{Paths: engine.DefaultPaths, Arch: runtime.GOARCH, Log: io.Discard}).Install(ctx)
		},
		Proxy: func(ctx context.Context, cfg proxy.Config) error {
			pc := proxy.NewClient(proxySocket)
			old, err := pc.Config(ctx)
			if err != nil {
				return err
			}
			// Running the install again keeps the apps' routes.
			cfg.Routes = old.Routes
			return pc.Apply(ctx, cfg)
		},
		SetupLink: func(ctx context.Context) (string, error) {
			token, err := panel.NewClient(panelSocket).SetupLink(ctx)
			if errors.Is(err, panel.ErrSetupDone) {
				return "", install.ErrAdminExists
			}
			if err != nil {
				return "", err
			}
			return "https://" + proxy.URLHost(opts.Host) + "/setup#" + token, nil
		},
		Cloudflared: cf.Install,
	}
	if err := inst.Run(ctx); err != nil {
		fmt.Fprintf(stderr, "\nzelie: %v\nFix that and run the install again; what is done already stays done.\n", err)
		return 1
	}
	return 0
}

// ask fills in what the flags left out.
func ask(ctx context.Context, in *bufio.Reader, out io.Writer, o *install.Options) error {
	interactive := o.Mode == ""
	if interactive {
		fmt.Fprint(out, `How will people reach the panel?

  1) A domain that points to this server.
     Zelie gets HTTPS certificates from Let's Encrypt. Ports 80 and 443
     must be free and reachable from the internet. Recommended when
     nothing else serves websites on this server.

  2) A Cloudflare Tunnel.
     No port opens to the internet, and Cloudflare handles HTTPS.
     Recommended when your domain is on Cloudflare, when another web
     server already uses ports 80 and 443, or when the server is behind
     NAT.

  3) This server's IP address, with a self-signed certificate.
     Only for trying Zelie out: browsers warn about the certificate, and
     passkeys need a domain, so you log in with an authenticator app.

`)
		switch line(in, out, "Choose 1, 2 or 3") {
		case "1":
			o.Mode = install.ModeDomain
		case "2":
			o.Mode = install.ModeTunnel
		case "3":
			o.Mode = install.ModeIP
		default:
			return fmt.Errorf("choose 1, 2 or 3")
		}
	}
	if o.Host == "" {
		if o.Mode == install.ModeIP {
			o.Host = line(in, out, "This server's IP address")
		} else {
			o.Host = line(in, out, "The panel's domain, such as panel.example.com")
		}
	}
	if o.Mode == install.ModeDomain && o.Email == "" {
		o.Email = line(in, out, "Email for Let's Encrypt, which warns you if a certificate has trouble")
	}
	if o.Mode == install.ModeTunnel {
		if p := ""; interactive {
			if p = line(in, out, fmt.Sprintf("Local port the tunnel sends traffic to (Enter keeps %d)", o.Port)); p != "" {
				n, err := strconv.Atoi(p)
				if err != nil {
					return fmt.Errorf("%q is not a port", p)
				}
				o.Port = n
			}
		}
		if status, err := runCmd(ctx, "systemctl", "is-active", "cloudflared"); err != nil || strings.TrimSpace(status) != "active" {
			o.Cloudflared = pasteCommand(in, out)
		}
	}
	return o.Check()
}

func pasteCommand(in *bufio.Reader, out io.Writer) string {
	fmt.Fprint(out, `
No tunnel connector runs on this server yet. In Cloudflare, create a tunnel
(Zero Trust, Networks, Tunnels), choose Debian, and paste the command it shows
here. Zelie installs the connector; it never gets access to your account.
`)
	return line(in, out, "Command")
}

func line(in *bufio.Reader, out io.Writer, prompt string) string {
	fmt.Fprintf(out, "%s: ", prompt)
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

// runCmd runs a command and returns what it printed. The error never repeats
// the arguments, which may hold a tunnel token.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}
