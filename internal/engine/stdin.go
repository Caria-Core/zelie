package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// A game server reads its console from standard input. containerd's shim
// takes it from a named pipe and passes it on, and closes the process's
// input as soon as no one has the pipe open for writing. So something has
// to keep a write end open for the container's whole life, and it has to
// outlive the core: Zelie's updates restart the core while games run on.
//
// That something is a small holder process, `sleep`, started by the core
// with the pipe as its output. The core's unit uses KillMode=process, so
// stopping the core leaves the holder alone, as it leaves the containers.
// The holder is found again through a pid file in the container's directory
// and ended when the container is stopped or removed. Commands are written
// by opening the pipe for each write, so no descriptor of an older core is
// needed. Output does not depend on any of this: the shim writes it to the
// log file itself.

// ErrInputClosed means the container is not reading its standard input.
var ErrInputClosed = errors.New("the container is not reading its input")

const (
	stdinWait    = 2 * time.Second
	stdinTimeout = 5 * time.Second
	// holderSleep is about 68 years, the longest every sleep accepts.
	holderSleep = "2147483647"
)

func (e *Engine) stdinPath(id string) string { return filepath.Join(e.containerDir(id), "stdin") }
func (e *Engine) holderPidPath(id string) string {
	return filepath.Join(e.containerDir(id), "stdin.pid")
}

// withStdin adds the container's input pipe to how its output is kept.
func withStdin(c cio.Creator, fifo string) cio.Creator {
	return func(id string) (cio.IO, error) {
		i, err := c(id)
		if err != nil {
			return nil, err
		}
		return stdinIO{IO: i, stdin: fifo}, nil
	}
}

type stdinIO struct {
	cio.IO
	stdin string
}

func (s stdinIO) Config() cio.Config {
	c := s.IO.Config()
	c.Stdin = s.stdin
	return c
}

// openPipe opens the write end of a container's input pipe. That fails
// while no one reads the other end. Right after the task was created the
// shim may not have gotten to it yet, so wait lets it try for a moment.
func (e *Engine) openPipe(id string, wait bool) (*os.File, error) {
	deadline := time.Now().Add(stdinWait)
	for {
		f, err := os.OpenFile(e.stdinPath(id), os.O_WRONLY|syscall.O_NONBLOCK, 0)
		switch {
		case err == nil:
			return f, nil
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("container %s has no input: %w", id, errdefs.ErrFailedPrecondition)
		case errors.Is(err, syscall.ENXIO) && !wait:
			return nil, fmt.Errorf("container %s: %w", id, ErrInputClosed)
		case errors.Is(err, syscall.ENXIO) && time.Now().Before(deadline):
			time.Sleep(20 * time.Millisecond)
		default:
			return nil, fmt.Errorf("open the input of %s: %w", id, err)
		}
	}
}

// holderRunning reports whether the container's holder process is alive.
func (e *Engine) holderRunning(id string) bool {
	pid, ok := e.holderPid(id)
	if !ok {
		return false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(comm)) == "sleep"
}

func (e *Engine) holderPid(id string) (int, bool) {
	b, err := os.ReadFile(e.holderPidPath(id))
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, err == nil && pid > 1
}

// startHolder keeps the container's input pipe open from now on.
func (e *Engine) startHolder(id string, wait bool) error {
	f, err := e.openPipe(id, wait)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command("sleep", holderSleep)
	// Its own session, so nothing sent to the core's group reaches it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = f
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hold the input of %s: %w", id, err)
	}
	// Reaps it if this core outlives it; if not, init does.
	go cmd.Wait()
	if err := os.WriteFile(e.holderPidPath(id), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		cmd.Process.Kill()
		return err
	}
	return nil
}

// stopHolder ends the container's holder, which closes its input.
func (e *Engine) stopHolder(id string) {
	if e.holderRunning(id) {
		pid, _ := e.holderPid(id)
		syscall.Kill(pid, syscall.SIGKILL)
	}
	os.Remove(e.holderPidPath(id))
}

// restoreHolders starts a holder for every container that has an input
// and none. It is for containers made before holders were kept, and for a
// holder that was killed; a container whose input is already closed cannot
// be helped and is skipped.
func (e *Engine) restoreHolders(ctx context.Context) {
	list, err := e.client.Containers(e.ctx(ctx))
	if err != nil {
		return
	}
	for _, c := range list {
		if _, err := os.Stat(e.stdinPath(c.ID())); err == nil && !e.holderRunning(c.ID()) {
			e.startHolder(c.ID(), false)
		}
	}
}

// WriteStdin writes to the standard input of a container that was created
// with Spec.Stdin.
func (e *Engine) WriteStdin(ctx context.Context, id string, data []byte) error {
	if !validID().MatchString(id) {
		return fmt.Errorf("container id %q must be lowercase letters, digits and dashes", id)
	}
	f, err := e.openPipe(id, false)
	if err != nil {
		return err
	}
	defer f.Close()
	f.SetWriteDeadline(time.Now().Add(stdinTimeout))
	if _, err := f.Write(data); err != nil {
		if errors.Is(err, syscall.EPIPE) {
			return fmt.Errorf("container %s: %w", id, ErrInputClosed)
		}
		return fmt.Errorf("write to the input of %s: %w", id, err)
	}
	return nil
}

// signals are the ones a panel may send. They are what eggs and consoles
// use to stop or reload a server.
var signals = map[string]syscall.Signal{
	"SIGTERM": syscall.SIGTERM, "SIGINT": syscall.SIGINT, "SIGKILL": syscall.SIGKILL,
	"SIGHUP": syscall.SIGHUP, "SIGQUIT": syscall.SIGQUIT,
	"SIGUSR1": syscall.SIGUSR1, "SIGUSR2": syscall.SIGUSR2,
}

// ParseSignal turns a name such as SIGINT into a signal a container may be
// sent.
func ParseSignal(name string) (syscall.Signal, bool) {
	s, ok := signals[name]
	return s, ok
}

// Signal sends a signal to the container's main process.
func (e *Engine) Signal(ctx context.Context, id string, sig syscall.Signal) error {
	ctx = e.ctx(ctx)
	container, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return err
	}
	return task.Kill(ctx, sig)
}

// runAs makes the process run as the given user and group, with no others.
func runAs(u IDs) oci.SpecOpts {
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
		if s.Process == nil {
			s.Process = &specs.Process{}
		}
		s.Process.User = specs.User{UID: u.UID, GID: u.GID}
		return nil
	}
}
