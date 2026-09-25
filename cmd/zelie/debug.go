package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"text/tabwriter"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
)

// The debug commands drive containerd directly as root. They exist to test the
// engine by hand until the core process and its socket take over this job.
const debugUsage = `Usage: zelie debug <command>

Commands:
  run [flags] <id> <image> [args...]   Pull an image and start a container
  stop <id>                            Stop a container
  rm <id>                              Stop and delete a container
  ps                                   List containers
  logs <id>                            Print a container's output
`

func debug(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, debugUsage)
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "zelie: debug commands need root")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if args[0] == "logs" {
		if len(args) != 2 || !engine.ValidID(args[1]) {
			fmt.Fprint(stderr, debugUsage)
			return 2
		}
		// Reading the log needs no containerd connection.
		f, err := os.Open(engine.LogPathFor(engine.DefaultPaths, args[1]))
		if err != nil {
			fmt.Fprintf(stderr, "zelie: %v\n", err)
			return 1
		}
		defer f.Close()
		io.Copy(stdout, f)
		return 0
	}

	e, err := engine.Connect(ctx, engine.DefaultPaths)
	if err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	defer e.Close()

	switch args[0] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(stderr)
		mem := fs.Int64("memory", 256, "memory limit in MiB")
		cpus := fs.Float64("cpus", 1, "CPU limit, may be fractional")
		pids := fs.Int64("pids", 256, "process limit")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		if fs.NArg() < 2 {
			fmt.Fprint(stderr, debugUsage)
			return 2
		}
		spec := engine.Spec{
			ID:          fs.Arg(0),
			Image:       fs.Arg(1),
			Args:        fs.Args()[2:],
			MemoryBytes: *mem << 20,
			CPUs:        *cpus,
			Pids:        *pids,
		}
		if err := e.Run(ctx, spec); err != nil {
			fmt.Fprintf(stderr, "zelie: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "started %s\n", spec.ID)
	case "stop", "rm":
		if len(args) != 2 {
			fmt.Fprint(stderr, debugUsage)
			return 2
		}
		var err error
		if args[0] == "stop" {
			err = e.Stop(ctx, args[1], 10*time.Second)
		} else {
			err = e.Remove(ctx, args[1])
		}
		if err != nil {
			fmt.Fprintf(stderr, "zelie: %v\n", err)
			return 1
		}
	case "ps":
		list, err := e.List(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "zelie: %v\n", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATE\tPID\tUSERNS\tIMAGE")
		for _, c := range list {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", c.ID, c.State, c.Pid, c.Userns, c.Image)
		}
		tw.Flush()
	default:
		fmt.Fprint(stderr, debugUsage)
		return 2
	}
	return 0
}
