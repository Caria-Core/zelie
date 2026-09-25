package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// follow copies f to w and keeps copying new data as it is appended, like
// tail -f, until ctx is done. Polling is used instead of inotify because the
// shim appends in small writes and a quarter-second delay is fine for a
// console.
func follow(ctx context.Context, f io.Reader, w io.Writer, every time.Duration) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for {
			n, err := f.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return werr
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
