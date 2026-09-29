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
	if err := e.startHolder(id, true); err != nil {
		t.Fatal(err)
	}
	pid, ok := e.holderPid(id)
	if !ok {
		t.Fatal("no holder")
	}
	// stopHolder needs /proc to be sure of what it kills, which macOS lacks.
	defer syscall.Kill(pid, syscall.SIGKILL)
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

	// Each write opened and closed the pipe, yet the reader sees no end of
	// file while the holder has it, and does once the holder goes.
	r.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	// Linux waits for the deadline; macOS cannot poll a pipe and says so.
	if n, err := r.Read(buf); n != 0 || !(errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.EAGAIN)) {
		t.Errorf("read %d bytes, %v, before the pipe closed", n, err)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	r.SetReadDeadline(time.Now().Add(2 * time.Second))
	var err2 error
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err2 = r.Read(buf); errors.Is(err2, io.EOF) {
			break
		}
	}
	if !errors.Is(err2, io.EOF) {
		t.Errorf("after the holder ended: %v", err2)
	}

	// A container that was not made with input has no pipe.
	if err := e.WriteStdin(ctx, "other", []byte("x")); err == nil {
		t.Error("wrote to a container without input")
	}
	if err := e.WriteStdin(ctx, "Bad_ID", []byte("x")); err == nil {
		t.Error("accepted an invalid id")
	}
}
