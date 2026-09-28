package core

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/update"
	"github.com/Caria-Core/zelie/internal/version"
)

var (
	errNotNewer     = msg.Define(http.StatusConflict, "update.not_newer", "{version} is not newer than the version running.")
	errUpdating     = msg.Define(http.StatusConflict, "update.running", "An update is already running.")
	errDownload     = msg.Define(http.StatusBadGateway, "update.download", "The release could not be downloaded or checked: {detail}")
	errNotInstalled = msg.Define(http.StatusConflict, "update.not_installed", "This Zelie was not installed with zelie install, so it cannot update itself.")
)

// Updater does the parts of an update that touch the machine. Tests
// replace it.
type Updater struct {
	// Fetch downloads a release's binary and checks its signature.
	Fetch func(ctx context.Context, version string) ([]byte, error)
	// Place puts it where the services run from, keeping the old one.
	Place func(bin []byte) error
	// Start runs the unit that restarts the services and checks them.
	Start func(ctx context.Context, from, to string) error
	// Running reports whether that unit runs now.
	Running func(ctx context.Context) bool
	// Last is the outcome of the last update.
	Last func() (update.Result, error)
	// Installed reports whether the services run from the installed binary.
	Installed func() bool
}

type updateRequest struct {
	Version string `json:"version"`
}

var validRelease = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Get().Version})
}

// startUpdate downloads a release, checks it and hands the restart to a
// unit of its own, which outlives the core's own restart.
func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !validRelease.MatchString(req.Version) {
		writeError(w, http.StatusBadRequest, errors.New("invalid version"))
		return
	}
	u := s.Updater
	if u == nil || !u.Installed() {
		writeError(w, http.StatusConflict, errNotInstalled.Err())
		return
	}
	current := version.Get().Version
	if !update.Newer(req.Version, current) {
		writeError(w, http.StatusConflict, errNotNewer.Err("version", req.Version))
		return
	}
	ctx := r.Context()
	if u.Running(ctx) {
		writeError(w, http.StatusConflict, errUpdating.Err())
		return
	}
	bin, err := u.Fetch(ctx, req.Version)
	if err != nil {
		s.Log.Warn("update download failed", "version", req.Version, "err", err)
		writeError(w, http.StatusBadGateway, errDownload.Err("detail", err.Error()))
		return
	}
	if err := u.Place(bin); err != nil {
		s.fail(w, "place update", req.Version, err)
		return
	}
	if err := u.Start(context.WithoutCancel(ctx), current, req.Version); err != nil {
		s.fail(w, "start update", req.Version, err)
		return
	}
	s.Log.Info("update started", "from", current, "to", req.Version)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) updateStatus(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Version string         `json:"version"`
		Last    *update.Result `json:"last,omitempty"`
	}{Version: version.Get().Version}
	if s.Updater != nil {
		if last, err := s.Updater.Last(); err == nil && last.To != "" {
			last.Running = last.Running && s.Updater.Running(r.Context())
			out.Last = &last
		}
	}
	writeJSON(w, http.StatusOK, out)
}
