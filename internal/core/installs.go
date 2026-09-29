package core

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/Caria-Core/zelie/internal/engine"
)

// Where an install container finds its script and the server's files, as
// Pterodactyl's installers expect them.
const (
	InstallScriptPath = "/mnt/install/install.sh"
	InstallVolumePath = "/mnt/server"
)

// InstallRequest is a game server's install container: an image that
// runs a script with the server's volume mounted. The panel waits for it
// and reads its output like any other container's.
type InstallRequest struct {
	ID    string `json:"id"`
	App   string `json:"app"`
	Image string `json:"image"`
	// Entrypoint is the shell that runs the script, such as bash or ash.
	Entrypoint string   `json:"entrypoint"`
	Script     string   `json:"script"`
	Env        []string `json:"env,omitempty"`
	// Volume is the server's volume, mounted at InstallVolumePath.
	Volume      string  `json:"volume"`
	Network     string  `json:"network,omitempty"`
	MemoryBytes int64   `json:"memory_bytes"`
	CPUs        float64 `json:"cpus"`
	Pids        int64   `json:"pids"`
}

// A shell's name or path, nothing that could carry arguments.
var entrypointRe = regexp.MustCompile(`^[A-Za-z0-9._/+-]{1,128}$`)

// spec turns the request into a container that runs like any other one:
// the same user namespace, seccomp profile and limits.
func (r InstallRequest) spec() (engine.Spec, error) {
	switch {
	case r.App == "" || r.Volume == "":
		return engine.Spec{}, errors.New("an install needs an app and a volume")
	case !entrypointRe.MatchString(r.Entrypoint):
		return engine.Spec{}, errors.New("the install entrypoint must be a shell such as bash")
	case strings.ContainsRune(r.Script, 0):
		return engine.Spec{}, errors.New("the install script cannot hold a null byte")
	}
	s := engine.Spec{
		ID: r.ID, App: r.App, Image: r.Image, Network: r.Network,
		Args:        []string{r.Entrypoint, InstallScriptPath},
		Env:         slices.Clone(r.Env),
		Volumes:     []engine.VolumeMount{{Name: r.Volume, Target: InstallVolumePath}},
		Files:       []engine.File{{Target: InstallScriptPath, Content: []byte(r.Script)}},
		MemoryBytes: r.MemoryBytes, CPUs: r.CPUs, Pids: r.Pids,
	}
	return s, s.Validate()
}

func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	var req InstallRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	spec, err := req.spec()
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.Engine.Run(r.Context(), spec); err != nil {
		s.fail(w, "install", req.ID, err)
		return
	}
	// What ran, by digest, so the panel can say which build installed it.
	image, err := s.Engine.Pin(r.Context(), spec.Image)
	if err != nil {
		s.Log.Error("pin image", "image", spec.Image, "err", err)
		image = spec.Image
	}
	s.Log.Info("install started", "id", req.ID, "app", req.App, "image", image)
	writeJSON(w, http.StatusCreated, runResponse{Image: image})
}

// RunInstall starts a game server's install container and returns the image
// that runs, pinned by digest. Wait, Logs and Remove work on it like on any
// container.
func (c *Client) RunInstall(ctx context.Context, req InstallRequest) (string, error) {
	var res runResponse
	err := c.do(ctx, http.MethodPost, "/v1/installs", req, &res)
	return res.Image, err
}
