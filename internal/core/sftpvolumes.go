package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/peer"
)

// The SFTP server is reachable from the internet, so the core does not take
// its word for which volume a login may use. The panel tells the core which
// volumes belong to game servers, and the core refuses the SFTP user any
// other: a database's files stay out of reach even of a compromised SFTP
// process.

var errNoSFTPVolumes = msg.Define(http.StatusBadRequest, "sftp.bad_volumes", "That is not a list of volume names.")

const maxSFTPVolumes = 100000

// SFTPVolumes is the list of volumes the SFTP user may use, kept in a file
// so it is there again when the core restarts.
type SFTPVolumes struct {
	path string
	mu   sync.RWMutex
	set  map[string]bool
}

// LoadSFTPVolumes reads the list at path. A missing file is an empty list.
func LoadSFTPVolumes(path string) (*SFTPVolumes, error) {
	v := &SFTPVolumes{path: path, set: map[string]bool{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return nil, err
	}
	for _, n := range names {
		v.set[n] = true
	}
	return v, nil
}

// Has reports whether the SFTP user may use the volume.
func (v *SFTPVolumes) Has(name string) bool {
	if v == nil {
		return false
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.set[name]
}

// Set replaces the list and saves it.
func (v *SFTPVolumes) Set(names []string) error {
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	b, err := json.Marshal(names)
	if err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	tmp := v.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(v.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, v.path); err != nil {
		return err
	}
	v.set = make(map[string]bool, len(names))
	for _, n := range names {
		v.set[n] = true
	}
	return nil
}

type sftpVolumesRequest struct {
	Volumes []string `json:"volumes"`
}

func (s *Server) setSFTPVolumes(w http.ResponseWriter, r *http.Request) {
	var req sftpVolumesRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.Volumes) > maxSFTPVolumes || slices.ContainsFunc(req.Volumes, func(n string) bool { return !engine.ValidID(n) }) {
		writeError(w, http.StatusBadRequest, errNoSFTPVolumes.Err())
		return
	}
	if s.SFTPVolumes == nil {
		writeError(w, http.StatusConflict, errors.New("this core keeps no list for SFTP"))
		return
	}
	if err := s.SFTPVolumes.Set(req.Volumes); err != nil {
		s.Log.Error("save the SFTP volumes", "err", err)
		writeError(w, http.StatusInternalServerError, errors.New("could not save the list"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// onlySFTPVolumes refuses the SFTP user any volume that is not listed. Other
// users pass; which routes each may use is peer.RequireRoutes's business.
func (s *Server) onlySFTPVolumes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := peer.From(r.Context()); ok && s.SFTPUID != 0 && p.UID == s.SFTPUID {
			if rest, found := strings.CutPrefix(r.URL.Path, "/v1/volumes/"); found {
				name, _, _ := strings.Cut(rest, "/")
				if !s.SFTPVolumes.Has(name) {
					s.Log.Warn("SFTP user asked for a volume it may not use", "volume", name)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					w.Write([]byte(`{"error":"not allowed"}` + "\n"))
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// SetSFTPVolumes tells the core which volumes the SFTP user may use.
func (c *Client) SetSFTPVolumes(ctx context.Context, names []string) error {
	if names == nil {
		names = []string{}
	}
	return c.do(ctx, http.MethodPut, "/v1/sftp/volumes", sftpVolumesRequest{Volumes: names}, nil)
}
