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
	sizes, err := s.Engine.VolumeSizes()
	if err != nil {
		s.volumeFailed(w, "measure volumes", "", err)
		return
	}
	out := make([]volumeJSON, 0, len(sizes))
	for name, n := range sizes {
		out = append(out, volumeJSON{Name: name, Bytes: n})
	}
	writeJSON(w, http.StatusOK, out)
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

// VolumeSizes returns the disk each volume takes, by name.
func (c *Client) VolumeSizes(ctx context.Context) (map[string]int64, error) {
	var list []volumeJSON
	if err := c.do(ctx, http.MethodGet, "/v1/volumes", nil, &list); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(list))
	for _, v := range list {
		out[v.Name] = v.Bytes
	}
	return out, nil
}
