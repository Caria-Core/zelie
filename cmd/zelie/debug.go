package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"text/tabwriter"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
)

// The debug commands send requests to the core by hand. They exist to test
// the core until the panel does this job.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c := core.NewClient(core.DefaultSocket)

	var err error
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
		err = c.Run(ctx, engine.Spec{
			ID:          fs.Arg(0),
			Image:       fs.Arg(1),
			Args:        fs.Args()[2:],
			MemoryBytes: *mem << 20,
			CPUs:        *cpus,
			Pids:        *pids,
		})
		if err == nil {
			fmt.Fprintf(stdout, "started %s\n", fs.Arg(0))
		}
	case "stop", "rm", "logs":
		if len(args) != 2 {
			fmt.Fprint(stderr, debugUsage)
			return 2
		}
		switch args[0] {
		case "stop":
			err = c.Stop(ctx, args[1], 10)
		case "rm":
			err = c.Remove(ctx, args[1])
		case "logs":
			err = c.Logs(ctx, args[1], stdout)
		}
	case "ps":
		var list []engine.Status
		list, err = c.List(ctx)
		if err == nil {
			tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSTATE\tPID\tUSERNS\tIMAGE")
			for _, s := range list {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\n", s.ID, s.State, s.Pid, s.Userns, s.Image)
			}
			tw.Flush()
		}
	default:
		fmt.Fprint(stderr, debugUsage)
		return 2
	}
	if err != nil {
		fmt.Fprintf(stderr, "zelie: %v\n", err)
		return 1
	}
	return 0
}
