package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
// input as soon as no one has the pipe open for writing. The engine
// therefore holds one end open for the container's life and writes
// commands to it. Output does not depend on this: the shim writes it to the
// log file by itself, so it goes on while the core restarts.
//
// A core that restarts while such a container runs loses the open end, and
// the container's input is closed for good. WriteStdin then fails, and the
// caller has to use a signal.

// ErrInputClosed means the container is not reading its standard input.
var ErrInputClosed = errors.New("the container is not reading its input")

const (
	stdinWait    = 2 * time.Second
	stdinTimeout = 5 * time.Second
)

type stdins struct {
	mu    sync.Mutex
	files map[string]*os.File
}

func (e *Engine) stdinPath(id string) string { return filepath.Join(e.containerDir(id), "stdin") }

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

// openStdin opens the write end of a container's input pipe and keeps it.
// Opening fails while no one reads the other end. Right after the task was
// created the shim may not have gotten to it yet, so wait lets it try for a
// moment.
func (e *Engine) openStdin(id string, wait bool) (*os.File, error) {
	deadline := time.Now().Add(stdinWait)
	for {
		f, err := os.OpenFile(e.stdinPath(id), os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			e.stdins.mu.Lock()
			defer e.stdins.mu.Unlock()
			if e.stdins.files == nil {
				e.stdins.files = map[string]*os.File{}
			}
			e.stdins.files[id] = f
			return f, nil
		}
		switch {
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

func (e *Engine) closeStdin(id string) {
	e.stdins.mu.Lock()
	defer e.stdins.mu.Unlock()
	if f, ok := e.stdins.files[id]; ok {
		f.Close()
		delete(e.stdins.files, id)
	}
}

// WriteStdin writes to the standard input of a container that was created
// with Spec.Stdin.
func (e *Engine) WriteStdin(ctx context.Context, id string, data []byte) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("container id %q must be lowercase letters, digits and dashes", id)
	}
	e.stdins.mu.Lock()
	f := e.stdins.files[id]
	e.stdins.mu.Unlock()
	if f == nil {
		// The engine started after the container did.
		var err error
		if f, err = e.openStdin(id, false); err != nil {
			return err
		}
	}
	f.SetWriteDeadline(time.Now().Add(stdinTimeout))
	if _, err := f.Write(data); err != nil {
		if errors.Is(err, syscall.EPIPE) {
			e.closeStdin(id)
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
