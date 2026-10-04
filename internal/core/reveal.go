package core

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
)

// RevealableNames are the sealed variables the core will open for the panel
// to show: the passwords of databases' app users. Everything else sealed
// here, such as an app's tokens or a database's root password, stays closed.
// The panel checks that every engine's password variable is listed.
var RevealableNames = []string{"POSTGRES_PASSWORD", "MARIADB_PASSWORD", "REDIS_PASSWORD"}

type revealRequest struct {
	App    string `json:"app"`
	Sealed string `json:"sealed"`
}

// reveal opens a database password for the panel to show an administrator.
// Only the panel's own routes lead here, and the value is never logged.
func (s *Server) reveal(w http.ResponseWriter, r *http.Request) {
	if s.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this core has no secret key"))
		return
	}
	var req revealRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	opened, err := s.Secrets.Open(req.Sealed, req.App)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name, value, _ := strings.Cut(opened, "=")
	if !slices.Contains(RevealableNames, name) {
		s.Log.Warn("refused to reveal a sealed value", "app", req.App, "name", name)
		writeError(w, http.StatusForbidden, errors.New("this value is not one to show"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"value": value})
}

// Reveal opens a sealed database password.
func (c *Client) Reveal(ctx context.Context, app, sealed string) (string, error) {
	var out struct {
		Value string `json:"value"`
	}
	if err := c.do(ctx, http.MethodPost, "/v1/secrets/reveal", revealRequest{App: app, Sealed: sealed}, &out); err != nil {
		return "", err
	}
	return out.Value, nil
}
