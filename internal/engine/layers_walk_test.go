package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A container controls the files that are measured, and the core measures them
// as root. It can put a named pipe where a folder was after the walk has listed
// it: a walk that opens it with a plain open waits for a writer for good, and
// with it every check that comes after. This needs the walk of diskUsage to
// open folders without waiting (WalkTree).
func TestMeasureLayersIsNotHeldUpByAFolderSwappedForAPipe(t *testing.T) {
	const folders = 1500
	dir := t.TempDir()
	for i := range folders {
		d := filepath.Join(dir, fmt.Sprintf("sub%04d", i))
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		for i := folders - 1; i >= 0; i-- {
			select {
			case <-stop:
				return
			default:
			}
			d := filepath.Join(dir, fmt.Sprintf("sub%04d", i))
			os.Remove(filepath.Join(d, "f"))
			os.Remove(d)
			unix.Mkfifo(d, 0o644)
			time.Sleep(20 * time.Microsecond)
		}
	}()
	t.Cleanup(func() { close(stop); <-swapped })

	find := func(id string) (string, string, error) { return "web", dir, nil }
	done := make(chan []LayerSize, 1)
	go func() {
		got, _ := measureLayers(context.Background(), slog.New(slog.DiscardHandler), []string{"web-1"}, find)
		done <- got
	}()
	select {
	case got := <-done:
		if len(got) != 1 || got[0].Unmeasured {
			t.Errorf("got %+v, want the layer measured", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the measurement was held up by a named pipe")
	}
}
