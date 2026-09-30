package core

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"golang.org/x/sys/unix"
)

// Cloning a game server copies its volume into a new one. It is the backup
// code with the file in between left out: the files are written as a tar
// stream and unpacked from it at once, so nothing is compressed, encrypted or
// kept on disk twice, and they land with the owners they had. Those are
// container IDs, and every game server runs as the same user in its own user
// namespace, so no owner needs mapping.

// copyDir is the folder the files get inside the stream. It never reaches
// the disk; it only has to be the same on both sides.
const copyDir = "volume"

var errNoRoomCopy = msg.Define(http.StatusUnprocessableEntity, "copy.no_room", "Not enough disk space: the copy needs up to {need} and {free} is free, and Zelie keeps 1 GB free for the apps.")

type copyVolumeRequest struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// freeAt is what the disk holding dir has left for an unprivileged writer.
func freeAt(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// copyVolume fills an empty volume with the files of another. Both must be
// the file volume of a game server or files app, which is what the panel
// lists for SFTP: a database's volume is never copied, and a volume that
// already has files is never written over. The source may be in use.
func (s *Server) copyVolume(w http.ResponseWriter, r *http.Request) {
	var req copyVolumeRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !engine.ValidID(req.From) || !engine.ValidID(req.To) || req.From == req.To {
		writeError(w, http.StatusBadRequest, errors.New("invalid volume names"))
		return
	}
	if !s.SFTPVolumes.Has(req.From) || !s.SFTPVolumes.Has(req.To) {
		s.Log.Warn("copy between volumes that are not game files", "from", req.From, "to", req.To)
		writeError(w, http.StatusForbidden, errors.New("only the files of game servers and files apps can be copied"))
		return
	}
	ctx := r.Context()
	dst, err := s.Engine.OpenVolume(ctx, req.To)
	if err != nil {
		s.volumeFailed(w, "copy volume", req.To, err)
		return
	}
	defer dst.Close()
	if names, err := readNames(dst); err != nil {
		s.volumeFailed(w, "copy volume", req.To, err)
		return
	} else if len(names) > 0 {
		writeError(w, http.StatusConflict, errors.New("the volume to copy into is not empty"))
		return
	}
	src, err := s.Engine.ReadVolume(req.From)
	if err != nil {
		s.volumeFailed(w, "copy volume", req.From, err)
		return
	}
	defer src.Close()

	need, err := s.Engine.VolumeSize(req.From)
	if err == nil && s.Paths.Volumes != "" {
		var free int64
		if free, err = freeAt(s.Paths.Volumes); err == nil && free-need < diskReserve {
			err = errNoRoomCopy.Err("need", sizeLabel(need), "free", sizeLabel(free))
		}
	}
	if err != nil {
		s.copyFailed(w, req, err)
		return
	}

	start := time.Now()
	if err := copyFiles(ctx, src, dst); err != nil {
		s.copyFailed(w, req, err)
		return
	}
	s.Log.Info("volume copied", "from", req.From, "to", req.To, "size", need, "took", time.Since(start).Round(time.Millisecond))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) copyFailed(w http.ResponseWriter, req copyVolumeRequest, err error) {
	var me *msg.Error
	if errors.As(err, &me) {
		s.Log.Warn("volume copy failed", "from", req.From, "to", req.To, "err", err)
		writeError(w, me.Status, err)
		return
	}
	s.volumeFailed(w, "copy volume", req.From, err)
}

// copyFiles writes src as a tar stream and unpacks it into dst.
func copyFiles(ctx context.Context, src, dst *os.Root) error {
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, readErr = backup.WriteTar(ctx, pw, []backup.Volume{{Dir: copyDir, Root: src}})
		pw.CloseWithError(readErr)
	}()
	_, err := backup.RestoreTar(ctx, pr, []backup.Volume{{Dir: copyDir, Root: dst}})
	// Lets the reader stop if the unpacking ended first.
	pr.CloseWithError(err)
	wg.Wait()
	// A failure to read the source is the cause of the "damaged" stream the
	// unpacking then complains about.
	if readErr != nil && !errors.Is(readErr, io.ErrClosedPipe) {
		return readErr
	}
	return err
}

func readNames(root *os.Root) ([]string, error) {
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Readdirnames(-1)
}

// CopyVolume fills the empty volume to with the files of from, owners
// included. Both are volumes of game servers or files apps.
func (c *Client) CopyVolume(ctx context.Context, from, to string) error {
	return c.do(ctx, http.MethodPost, "/v1/volume-copies", copyVolumeRequest{From: from, To: to}, nil)
}
