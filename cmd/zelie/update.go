package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/install"
	"github.com/Caria-Core/zelie/internal/panel"
	"github.com/Caria-Core/zelie/internal/proxy"
	"github.com/Caria-Core/zelie/internal/update"
)

// newUpdater is how the core updates Zelie on a server it was installed on.
func newUpdater() *core.Updater {
	return &core.Updater{
		Fetch: func(ctx context.Context, v string) ([]byte, error) {
			return (&update.Fetcher{}).Binary(ctx, v, runtime.GOARCH)
		},
		Place:  func(bin []byte) error { return update.Place("", bin) },
		Revert: func() error { return update.Revert("") },
		Start: func(ctx context.Context, from, to string) error {
			if err := update.Started("", from, to, time.Now()); err != nil {
				return err
			}
			// From the binary being replaced, which is known to run.
			out, err := runCmd(ctx, "systemd-run", "--unit", update.Unit, "--collect", "--quiet",
				update.Old, "update-finish", "--from", from, "--to", to)
			if err != nil {
				return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(out))
			}
			return nil
		},
		Running: func(ctx context.Context) bool {
			out, _ := runCmd(ctx, "systemctl", "is-active", update.Unit)
			s := strings.TrimSpace(out)
			return s == "active" || s == "activating"
		},
		Last: func() (update.Result, error) { return update.Last("") },
		Installed: func() bool {
			exe, err := os.Readlink("/proc/self/exe")
			return err == nil && strings.TrimSuffix(exe, " (deleted)") == install.Binary
		},
	}
}

// updateFinish restarts the services on the new binary and goes back to
// the old one if they do not come up. It runs in its own unit, started by
// the core.
func updateFinish(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("update-finish", flag.ContinueOnError)
	fs.SetOutput(stderr)
	from := fs.String("from", "", "version being replaced")
	to := fs.String("to", "", "version being installed")
	if err := fs.Parse(args); err != nil || !update.Valid(*to) {
		fmt.Fprintln(stderr, "zelie: usage: zelie update-finish --from vX --to vY")
		return 2
	}
	f := &update.Finisher{
		Exec: runCmd, Timeout: update.HealthTimeout, Now: time.Now,
		Healthy: func(ctx context.Context, want string) error {
			checks := []struct {
				name string
				get  func(context.Context) (string, error)
			}{
				{"core", core.NewClient(core.DefaultSocket).Version},
				{"proxy", proxy.NewClient(proxySocket).Version},
				{"panel", panel.NewClient(panelSocket).Version},
			}
			for _, c := range checks {
				got, err := c.get(ctx)
				if err != nil {
					return fmt.Errorf("the %s does not answer: %w", c.name, err)
				}
				if got != want {
					return fmt.Errorf("the %s runs %s", c.name, got)
				}
			}
			return nil
		},
	}
	res := f.Run(context.Background(), *from, *to)
	if !res.OK {
		fmt.Fprintf(stderr, "zelie: update to %s failed: %s\n", *to, res.Error)
		return 1
	}
	fmt.Fprintf(stdout, "Zelie %s is running.\n", *to)
	return 0
}
