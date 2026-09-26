package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/secret"
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

// LinkedVar is a variable made from a secret of another app, such as a
// database URL with the database's password in it.
type LinkedVar struct {
	Name string
	// Template is the value, with {secret} where the secret goes.
	Template string
	// From is the app the secret belongs to, and Sealed the secret, sealed
	// for it.
	From, Sealed string
}

type linkedVarJSON struct {
	Name     string `json:"name"`
	Template string `json:"template"`
	From     string `json:"from"`
	Sealed   string `json:"sealed"`
}

// openLinked makes the linked variables of a run request. An app only gets
// a secret of an app it is linked to: the panel cannot hand one app's
// secrets to another by asking.
func (s *Server) openLinked(req runRequest) ([]string, error) {
	if len(req.LinkedEnv) == 0 {
		return nil, nil
	}
	if s.Secrets == nil || req.App == "" {
		return nil, errors.New("linked variables need an app and a core with a secret key")
	}
	links, err := s.Engine.Links(req.App)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range req.LinkedEnv {
		if !secret.ValidName(v.Name) {
			return nil, fmt.Errorf("%q is not a valid variable name", v.Name)
		}
		linked := false
		for _, l := range links {
			linked = linked || l.To == v.From
		}
		if !linked {
			return nil, fmt.Errorf("%s is not linked to %s", req.App, v.From)
		}
		opened, err := s.Secrets.Open(v.Sealed, v.From)
		if err != nil {
			return nil, err
		}
		_, value, _ := strings.Cut(opened, "=")
		out = append(out, v.Name+"="+strings.ReplaceAll(v.Template, "{secret}", value))
	}
	return out, nil
}
