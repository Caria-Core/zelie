// Command zelie is the Zelie server panel. The same binary runs the
// privileged core, the web proxy, the unprivileged panel and the SFTP server
// as separate processes.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/version"
)

const usage = `Usage: zelie <command>

Commands:
  install          Set Zelie up on this server (needs root)
  core             Run the privileged core (needs root)
  proxy            Run the web proxy (must not run as root)
  panel            Run the web panel (must not run as root)
  sftp             Run the SFTP server for game servers (must not run as root)
  setup-link       Print a one-time link to create the first administrator
  reset-login      Print a one-time link to set a new password when locked out
  engine install   Install or update Zelie's containerd and runc (needs root)
  debug            Send requests to the core by hand while developing
  version          Print the version and exit
  help             Show this help
`

func main() {
	noHugePages()
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version.Get())
		return 0
	case "engine":
		if len(args) == 2 && args[1] == "install" {
			return engineInstall(stdout, stderr)
		}
		fmt.Fprintf(stderr, "zelie: usage: zelie engine install\n")
		return 2
	case "install":
		return runInstall(args[1:], os.Stdin, stdout, stderr)
	case "core":
		return runCore(stderr)
	case "update-finish":
		// Run by the core during an update, not by hand.
		return updateFinish(args[1:], stdout, stderr)
	case "proxy":
		return runProxy(args[1:], stderr)
	case "panel":
		return runPanel(stderr)
	case "sftp":
		return runSFTP(args[1:], stderr)
	case "setup-link":
		return setupLink(stdout, stderr)
	case "reset-login":
		return resetLogin(args[1:], stdout, stderr)
	case "debug":
		return debug(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "zelie: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func engineInstall(stdout, stderr io.Writer) int {
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "zelie: engine install needs root")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	in := &engine.Installer{Paths: engine.DefaultPaths, Arch: runtime.GOARCH, Log: stdout}
	if err := in.Install(ctx); err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	return 0
}
