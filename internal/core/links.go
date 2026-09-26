package core

import (
	"context"
	"net/http"
	"net/url"

	"github.com/Caria-Core/zelie/internal/engine"
)

type linkJSON struct {
	Name string `json:"name"`
	To   string `json:"to"`
	Port uint16 `json:"port"`
}

func (s *Server) links(w http.ResponseWriter, r *http.Request) {
	links, err := s.Engine.Links(r.PathValue("app"))
	if err != nil {
		s.fail(w, "read links", r.PathValue("app"), err)
		return
	}
	out := make([]linkJSON, 0, len(links))
	for _, l := range links {
		out = append(out, linkJSON(l))
	}
	writeJSON(w, http.StatusOK, out)
}

// setLinks replaces an app's links. Running containers are affected at once.
func (s *Server) setLinks(w http.ResponseWriter, r *http.Request) {
	app := r.PathValue("app")
	var req []linkJSON
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	links := make([]engine.Link, 0, len(req))
	for _, l := range req {
		links = append(links, engine.Link(l))
	}
	if err := engine.CheckLinks(app, links); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Engine.SetLinks(r.Context(), app, links); err != nil {
		s.fail(w, "set links", app, err)
		return
	}
	s.Log.Info("links set", "app", app, "links", len(links))
	w.WriteHeader(http.StatusNoContent)
}

// Links returns the links of an app.
func (c *Client) Links(ctx context.Context, app string) ([]engine.Link, error) {
	var list []linkJSON
	if err := c.do(ctx, http.MethodGet, "/v1/links/"+url.PathEscape(app), nil, &list); err != nil {
		return nil, err
	}
	out := make([]engine.Link, 0, len(list))
	for _, l := range list {
		out = append(out, engine.Link(l))
	}
	return out, nil
}

// SetLinks replaces the links of an app: which other apps it may reach, on
// which port, and by what name.
func (c *Client) SetLinks(ctx context.Context, app string, links []engine.Link) error {
	body := make([]linkJSON, 0, len(links))
	for _, l := range links {
		body = append(body, linkJSON(l))
	}
	return c.do(ctx, http.MethodPut, "/v1/links/"+url.PathEscape(app), body, nil)
}
