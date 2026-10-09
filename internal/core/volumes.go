package core

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/containerd/errdefs"
)

type volumeRequest struct {
	Name string `json:"name"`
}

type volumeMountJSON struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

type volumeJSON struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
	// Unmeasured is set for a volume that could not be measured. It has no
	// size, which is not the same as an empty one.
	Unmeasured bool `json:"unmeasured,omitempty"`
}

func (s *Server) createVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !engine.ValidID(req.Name) {
		writeError(w, http.StatusBadRequest, errors.New("invalid volume name"))
		return
	}
	if err := s.Engine.CreateVolume(req.Name); err != nil {
		s.volumeFailed(w, "create volume", req.Name, err)
		return
	}
	s.Log.Info("volume created", "volume", req.Name)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) removeVolume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !engine.ValidID(name) {
		writeError(w, http.StatusBadRequest, errors.New("invalid volume name"))
		return
	}
	if err := s.Engine.RemoveVolume(r.Context(), name); err != nil {
		s.volumeFailed(w, "remove volume", name, err)
		return
	}
	s.Log.Info("volume removed", "volume", name)
	w.WriteHeader(http.StatusNoContent)
}

// volumes lists every volume with the disk it takes. Measuring walks every
// file, so the panel asks now and then, not on each page view.
func (s *Server) volumes(w http.ResponseWriter, r *http.Request) {
	sizes, unmeasured, err := s.Engine.VolumeSizes()
	if err != nil {
		s.volumeFailed(w, "measure volumes", "", err)
		return
	}
	s.noteUnmeasured(unmeasured)
	out := make([]volumeJSON, 0, len(sizes)+len(unmeasured))
	for name, n := range sizes {
		out = append(out, volumeJSON{Name: name, Bytes: n})
	}
	for name := range unmeasured {
		out = append(out, volumeJSON{Name: name, Unmeasured: true})
	}
	writeJSON(w, http.StatusOK, out)
}

// noteUnmeasured logs each volume that cannot be measured when it first
// cannot, not at every check: the panel asks every minute, and the reason
// stays the same.
func (s *Server) noteUnmeasured(now map[string]error) {
	s.unmeasuredMu.Lock()
	defer s.unmeasuredMu.Unlock()
	for name, err := range now {
		if !s.unmeasured[name] {
			s.Log.Warn("volume cannot be measured", "volume", name, "err", err)
		}
	}
	s.unmeasured = make(map[string]bool, len(now))
	for name := range now {
		s.unmeasured[name] = true
	}
}

func (s *Server) volumeFailed(w http.ResponseWriter, op, name string, err error) {
	switch {
	case errdefs.IsNotFound(err):
		writeError(w, http.StatusNotFound, errors.New("volume not found"))
	case errdefs.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, errors.New("volume already exists"))
	case errdefs.IsFailedPrecondition(err):
		writeError(w, http.StatusConflict, errors.New("a container still uses this volume"))
	default:
		s.fail(w, op, name, err)
	}
}

// CreateVolume makes an empty volume.
func (c *Client) CreateVolume(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/volumes", volumeRequest{Name: name}, nil)
}

// RemoveVolume deletes a volume and its files. No container may use it.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/volumes/"+url.PathEscape(name), nil, nil)
}

// VolumeSizes returns the disk each volume takes, by name, and the names of
// the volumes that could not be measured.
func (c *Client) VolumeSizes(ctx context.Context) (sizes map[string]int64, unmeasured []string, err error) {
	var list []volumeJSON
	if err := c.do(ctx, http.MethodGet, "/v1/volumes", nil, &list); err != nil {
		return nil, nil, err
	}
	sizes = make(map[string]int64, len(list))
	for _, v := range list {
		if v.Unmeasured {
			unmeasured = append(unmeasured, v.Name)
			continue
		}
		sizes[v.Name] = v.Bytes
	}
	return sizes, unmeasured, nil
}
