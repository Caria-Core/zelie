// Command zelie is the Zelie server panel. The same binary will run the
// privileged core and the unprivileged web panel as two separate processes.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/Caria-Core/zelie/internal/version"
)

const usage = `Usage: zelie <command>

Commands:
  version   Print the version and exit
  help      Show this help
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
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "zelie: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
