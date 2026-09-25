// Command zelie is the Zelie server panel. The same binary will run the
// privileged core and the unprivileged web panel as two separate processes.
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
  engine install   Install or update Zelie's containerd and runc (needs root)
  version          Print the version and exit
  help             Show this help
`

func main() {
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
