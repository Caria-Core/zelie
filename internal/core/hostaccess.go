package core

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/Caria-Core/zelie/internal/engine"
)

type hostAccessRequest struct {
	On bool `json:"on"`
}

// setHostAccess turns an app's access to the host's database port on or off.
// It applies at once, to running containers too.
func (s *Server) setHostAccess(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req hostAccessRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !engine.ValidID(app) {
		writeError(w, http.StatusBadRequest, errors.New("app id must be lowercase letters, digits and dashes"))
		return
	}
	if err := s.Engine.SetHostAccess(r.Context(), app, req.On); err != nil {
		s.fail(w, "set host access", app, err)
		return
	}
	s.Log.Info("host access set", "app", app, "on", req.On)
	w.WriteHeader(http.StatusNoContent)
}

// SetHostAccess lets the app's containers connect to port 3306 of the host,
// or stops them. They find it under engine.HostName.
func (c *Client) SetHostAccess(ctx context.Context, app string, on bool) error {
	return c.do(ctx, http.MethodPut, "/v1/host-access/"+url.PathEscape(app), hostAccessRequest{On: on}, nil)
}
