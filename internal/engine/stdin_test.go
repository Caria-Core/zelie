package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestParseSignal(t *testing.T) {
	for name, want := range map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM, "SIGKILL": syscall.SIGKILL} {
		if got, ok := ParseSignal(name); !ok || got != want {
			t.Errorf("%s: %v %v", name, got, ok)
		}
	}
	for _, name := range []string{"", "INT", "SIGSTOP", "SIGSEGV", "9"} {
		if _, ok := ParseSignal(name); ok {
			t.Errorf("%q accepted", name)
		}
	}
}

// A pipe the way the shim reads it: a reader that has it open, and what is
// written arrives, until the writer end goes.
func TestWriteStdin(t *testing.T) {
	e := &Engine{paths: Paths{Data: t.TempDir()}}
	const id = "game-1"
	if err := os.MkdirAll(e.containerDir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(e.stdinPath(id), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Nobody reads yet, as after the container has gone.
	if err := e.WriteStdin(ctx, id, []byte("stop\n")); !errors.Is(err, ErrInputClosed) {
		t.Fatalf("with no reader: %v", err)
	}

	r, err := os.OpenFile(e.stdinPath(id), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := e.openStdin(id, true); err != nil {
		t.Fatal(err)
	}
	if err := e.WriteStdin(ctx, id, []byte("say hi\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	var got string
	for deadline := time.Now().Add(2 * time.Second); got == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		n, _ := r.Read(buf)
		got = string(buf[:n])
	}
	if got != "say hi\n" {
		t.Errorf("read %q", got)
	}

	// While the engine holds the pipe the reader sees no end of file, and
	// once it lets go it does.
	r.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("read %d bytes, %v, before the pipe closed", n, err)
	}
	e.closeStdin(id)
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.Read(buf); !errors.Is(err, io.EOF) {
		t.Errorf("after closing: %v", err)
	}

	// A container that was not made with input has no pipe.
	if err := e.WriteStdin(ctx, "other", []byte("x")); err == nil {
		t.Error("wrote to a container without input")
	}
	if err := e.WriteStdin(ctx, "Bad_ID", []byte("x")); err == nil {
		t.Error("accepted an invalid id")
	}
}
