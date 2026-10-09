package core

import (
	"context"
	"net/http"

	"github.com/Caria-Core/zelie/internal/engine"
)

type layerJSON struct {
	Container  string `json:"container"`
	App        string `json:"app"`
	Bytes      int64  `json:"bytes"`
	Unmeasured bool   `json:"unmeasured,omitempty"`
}

// layers lists the running containers with the disk their own files take,
// outside any volume. Measuring walks every file, so the panel asks now and
// then, as it does for volumes.
func (s *Server) layers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Engine.LayerSizes(r.Context())
	if err != nil {
		s.fail(w, "measure container files", "", err)
		return
	}
	out := make([]layerJSON, len(list))
	for i, l := range list {
		out[i] = layerJSON{Container: l.Container, App: l.App, Bytes: l.Bytes, Unmeasured: l.Unmeasured}
	}
	writeJSON(w, http.StatusOK, out)
}

// LayerSizes returns the disk the own files of each running container take.
func (c *Client) LayerSizes(ctx context.Context) ([]engine.LayerSize, error) {
	ctx, cancel := context.WithTimeout(ctx, MeasureTimeout)
	defer cancel()
	var list []layerJSON
	if err := c.do(ctx, http.MethodGet, "/v1/layers", nil, &list); err != nil {
		return nil, err
	}
	out := make([]engine.LayerSize, len(list))
	for i, l := range list {
		out[i] = engine.LayerSize{Container: l.Container, App: l.App, Bytes: l.Bytes, Unmeasured: l.Unmeasured}
	}
	return out, nil
}
