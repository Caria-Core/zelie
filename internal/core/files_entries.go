package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	// entryPage is how many items of a folder are read and sent at a time,
	// which is all of a folder that the core and its client hold in memory.
	entryPage = 256
	// entryStall is how long the core waits for its client to take what it
	// sent before it gives up on the listing.
	entryStall = 2 * time.Minute
)

// entryLine is one line of a streamed listing: an item, or how the listing
// ended. A listing without its end line was cut off.
type entryLine struct {
	Entry *FileEntry `json:"entry,omitempty"`
	End   bool       `json:"end,omitempty"`
	Error string     `json:"error,omitempty"`
}

// streamFolder sends every item of a folder, in the order the file system
// has them, a page at a time. Unlike listFiles it never leaves items out:
// the folder may hold any number of them.
func (s *Server) streamFolder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req listRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	dir, err := filePath(req.Path)
	if err != nil {
		s.fileFailed(w, "list files", name, req.Path, err)
		return
	}
	root, ok := s.filesRoot(w, name, "list files")
	if !ok {
		return
	}
	defer root.Close()
	f, err := openFolder(root, dir)
	if err != nil {
		s.fileFailed(w, "list files", name, dir, err)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	rc := http.NewResponseController(w)
	for {
		if r.Context().Err() != nil {
			return
		}
		entries, err := f.ReadDir(entryPage)
		// A client that stops reading must not hold the folder open for
		// good. Each page gets its own time, from before the first byte of
		// it is written.
		rc.SetWriteDeadline(time.Now().Add(entryStall))
		for _, e := range entries {
			if fe, ok := entryOf(root, dir, e); ok {
				if enc.Encode(entryLine{Entry: &fe}) != nil {
					return
				}
			}
		}
		var last *entryLine
		switch {
		case errors.Is(err, io.EOF):
			last = &entryLine{End: true}
		case err != nil:
			s.Log.Error("list files failed", "volume", name, "path", dir, "err", err)
			last = &entryLine{Error: "listing the folder failed, see the core log"}
		}
		if last != nil && enc.Encode(last) != nil {
			return
		}
		if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return
		}
		if last != nil {
			return
		}
	}
}

// Folder reads the items of a folder as the core sends them, so a folder of
// any size is read with the same little memory.
type Folder interface {
	// Next returns the next item, or io.EOF after the last. A listing that
	// breaks off is an error, never an end.
	Next() (FileEntry, error)
	Close() error
}

type folderStream struct {
	body  io.ReadCloser
	dec   *json.Decoder
	ended bool
}

// OpenFolder starts reading a folder of a volume, every item of it. The
// caller closes it.
func (c *Client) OpenFolder(ctx context.Context, ref FileRef, dir string) (Folder, error) {
	b, err := json.Marshal(listRequest{Path: dir})
	if err != nil {
		return nil, err
	}
	resp, err := c.raw(ctx, http.MethodPost, filesURL(ref, "entries"), bytes.NewReader(b), int64(len(b)))
	if err != nil {
		return nil, err
	}
	return &folderStream{body: resp.Body, dec: json.NewDecoder(resp.Body)}, nil
}

func (f *folderStream) Next() (FileEntry, error) {
	if f.ended {
		return FileEntry{}, io.EOF
	}
	var line entryLine
	if err := f.dec.Decode(&line); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return FileEntry{}, fmt.Errorf("the core's listing broke off: %w", err)
	}
	switch {
	case line.Entry != nil:
		return *line.Entry, nil
	case line.End:
		f.ended = true
		return FileEntry{}, io.EOF
	case line.Error != "":
		return FileEntry{}, &Error{Status: http.StatusInternalServerError, Message: line.Error}
	}
	return FileEntry{}, errors.New("the core sent a listing line that says nothing")
}

func (f *folderStream) Close() error { return f.body.Close() }
