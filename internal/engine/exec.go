package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/opencontainers/runtime-spec/specs-go"
)

// Exec runs a command in a running container and returns its exit code. It
// runs as root inside the container, with the container's environment, so a
// database's own tools can read the password it was started with.
//
// When stdin is set the command reads it to the end. The last bytes of stdin
// can be lost on the way through the shim, so a caller that must know all of
// it arrived checks that for itself.
func (e *Engine) Exec(ctx context.Context, id string, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error) {
	ctx = e.ctx(ctx)
	container, err := e.client.LoadContainer(ctx, id)
	if err != nil {
		return 0, err
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return 0, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return 0, err
	}
	proc := *spec.Process
	proc.Args = args
	proc.Terminal = false
	proc.User = specs.User{UID: 0, GID: 0}
	proc.Cwd = "/"

	var b [8]byte
	rand.Read(b[:])
	execID := "exec-" + hex.EncodeToString(b[:])

	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if stdin == nil {
		stdin = eofReader{}
	}
	// The shim keeps stdin open until told otherwise, and ignores being
	// told before the process has started: a short input would then never
	// end. So stdin closes only once Start has returned.
	var p containerd.Process
	started := make(chan struct{})
	var once sync.Once
	markStarted := func() { once.Do(func() { close(started) }) }
	defer markStarted()
	in := &closingReader{r: stdin, close: func() {
		<-started
		if p != nil {
			p.CloseIO(ctx, containerd.WithStdinCloser)
		}
	}}
	p, err = task.Exec(ctx, execID, &proc, cio.NewCreator(cio.WithStreams(in, stdout, stderr), cio.WithFIFODir(e.containerDir(id))))
	if err != nil {
		p = nil
		return 0, err
	}
	defer p.Delete(context.WithoutCancel(ctx), containerd.WithProcessKill)

	exited, err := p.Wait(ctx)
	if err != nil {
		return 0, err
	}
	if err := p.Start(ctx); err != nil {
		return 0, err
	}
	markStarted()
	select {
	case st := <-exited:
		// All of the output has been copied once the IO is done.
		p.IO().Wait()
		return st.ExitCode(), st.Error()
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// closingReader calls close once its reader is done.
type closingReader struct {
	r     io.Reader
	close func()
	once  sync.Once
}

func (c *closingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	if err != nil {
		c.once.Do(c.close)
		if !errors.Is(err, io.EOF) {
			// The command sees the end of its input either way; the
			// caller learns why from what the command did with it.
			err = io.EOF
		}
	}
	return n, err
}
