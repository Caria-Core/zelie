package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"
)

// follow copies f to w and keeps copying new data as it is appended, like
// tail -f, until ctx is done. Polling is used instead of inotify because the
// shim appends in small writes and a quarter-second delay is fine for a
// console. A file that is cut, as a container log is once it grows too large,
// is read again from its start.
func follow(ctx context.Context, f io.Reader, w io.Writer, every time.Duration) error {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	t := time.NewTicker(every)
	defer t.Stop()
	var watch cutWatch
	endsLine := true
	for {
		for {
			// Before every read, not once a poll: a follower that is far
			// behind spends a long time reading, and the log can be cleared
			// and grow past its place meanwhile.
			cut, err := watch.rewindIfCut(f)
			if err != nil {
				return err
			}
			// The cleared log opens with a line of its own. After a partial
			// line it would be taken for the rest of that line.
			if cut && !endsLine {
				if _, err := w.Write([]byte{'\n'}); err != nil {
					return err
				}
				endsLine = true
			}
			n, err := f.Read(buf)
			if n > 0 {
				endsLine = buf[n-1] == '\n'
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

// headLen is how much of a file's start is remembered. The line the core
// writes at the start of a cleared log is about 30 bytes, and two of them
// differ well before the end.
const headLen = 64

// logFile is what is needed to notice that a log was cut.
type logFile interface {
	io.Seeker
	io.ReaderAt
	Stat() (os.FileInfo, error)
}

// cutWatch remembers how the file it follows begins.
type cutWatch struct {
	head [headLen]byte
	n    int
}

// rewindIfCut moves r back to its start when the file was cut, and reports
// whether it did. Left alone, the reader would wait for the file to grow past
// its old place again and miss everything written meanwhile. A file is cut
// when it is shorter than the place being read, or when it starts with
// something other than it did, which is how a new log shows that it has grown
// past that place before anyone looked. Readers that are not files are left as
// they are.
func (c *cutWatch) rewindIfCut(r io.Reader) (bool, error) {
	f, ok := r.(logFile)
	if !ok {
		return false, nil
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return false, err
	}
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	var now [headLen]byte
	n, err := f.ReadAt(now[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	cut := st.Size() < pos || !bytes.HasPrefix(now[:n], c.head[:c.n])
	// A file that was short when first seen has more to compare with later.
	c.n = copy(c.head[:], now[:n])
	if !cut {
		return false, nil
	}
	_, err = f.Seek(0, io.SeekStart)
	return err == nil, err
}
