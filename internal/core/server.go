// Package core is the privileged half of Zelie. It runs as root, owns
// containerd, and accepts a small set of typed requests over a Unix socket.
// Nothing here listens on the network.
package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/peer"
	"github.com/Caria-Core/zelie/internal/secret"
	"github.com/containerd/errdefs"
)

// Engine is what the core needs from the container engine.
type Engine interface {
	Run(ctx context.Context, s engine.Spec) error
	Stop(ctx context.Context, id string, grace time.Duration) error
	Remove(ctx context.Context, id string) error
	List(ctx context.Context) ([]engine.Status, error)
	RemoveImage(ctx context.Context, name string) error
	Wait(ctx context.Context, id string) (uint32, error)
	Usage(id string) (engine.Usage, error)
}

// Limits a single request may ask for. They keep a confused or compromised
// panel from asking for absurd resources in one call.
const (
	maxBodyBytes = 1 << 20
	maxGrace     = 5 * time.Minute
)

type Server struct {
	Engine  Engine
	Builder Builder
	Secrets *secret.Keys
	Paths   engine.Paths
	Log     *slog.Logger
	Allowed peer.Policy
	// Host describes the server; tests replace it.
	Host func() (engine.Host, error)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/containers", s.list)
	mux.HandleFunc("POST /v1/containers", s.run)
	mux.HandleFunc("POST /v1/containers/{id}/stop", s.stop)
	mux.HandleFunc("DELETE /v1/containers/{id}", s.remove)
	mux.HandleFunc("POST /v1/containers/{id}/wait", s.wait)
	mux.HandleFunc("GET /v1/containers/{id}/logs", s.logs)
	mux.HandleFunc("GET /v1/containers/{id}/usage", s.usage)
	mux.HandleFunc("GET /v1/host", s.host)
	mux.HandleFunc("POST /v1/builds", s.build)
	mux.HandleFunc("DELETE /v1/images", s.removeImage)
	mux.HandleFunc("GET /v1/secrets/key", s.secretKey)
	return peer.Require(s.Allowed, s.Log, mux)
}

// Serve listens on the socket until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, socket string) error {
	if err := os.MkdirAll(filepath.Dir(socket), 0o711); err != nil {
		return err
	}
	// A stale socket from a previous run would make Listen fail.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	// Everyone may connect; checkPeer decides who is let through. The kernel
	// tells us who is on the other end, which a file mode alone cannot.
	if err := os.Chmod(socket, 0o666); err != nil {
		l.Close()
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ConnContext:       peer.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		// Requests end when the core stops, so a client following logs does
		// not hold up shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	s.Log.Info("core listening", "socket", socket)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

type runRequest struct {
	ID    string   `json:"id"`
	App   string   `json:"app,omitempty"`
	Image string   `json:"image"`
	Args  []string `json:"args,omitempty"`
	Env   []string `json:"env,omitempty"`
	// SealedEnv holds secret variables sealed for App with the core's key
	// (see package secret). They are opened here and nowhere else.
	SealedEnv   []string `json:"sealed_env,omitempty"`
	Network     string   `json:"network,omitempty"`
	MemoryBytes int64    `json:"memory_bytes"`
	CPUs        float64  `json:"cpus"`
	Pids        int64    `json:"pids"`
}

type stopRequest struct {
	GraceSeconds int `json:"grace_seconds"`
}

type containerJSON struct {
	ID      string `json:"id"`
	App     string `json:"app,omitempty"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Pid     uint32 `json:"pid,omitempty"`
	Userns  uint32 `json:"userns"`
	Network string `json:"network,omitempty"`
	IP      string `json:"ip,omitempty"`
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := decode(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	spec := engine.Spec{
		ID: req.ID, App: req.App, Image: req.Image, Args: req.Args, Network: req.Network,
		Env:         slices.Clone(req.Env),
		MemoryBytes: req.MemoryBytes, CPUs: req.CPUs, Pids: req.Pids,
	}
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(req.SealedEnv) > 0 && (s.Secrets == nil || req.App == "") {
		writeError(w, http.StatusBadRequest, errors.New("sealed variables need an app and a core with a secret key"))
		return
	}
	for _, sealed := range req.SealedEnv {
		v, err := s.Secrets.Open(sealed, req.App)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		spec.Env = append(spec.Env, v)
	}
	if err := s.Engine.Run(r.Context(), spec); err != nil {
		s.fail(w, "run", req.ID, err)
		return
	}
	s.Log.Info("container started", "id", req.ID, "image", req.Image)
	w.WriteHeader(http.StatusCreated)
}

// secretKey hands out the public key that secret variables are sealed with.
func (s *Server) secretKey(w http.ResponseWriter, r *http.Request) {
	if s.Secrets == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("this core has no secret key"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": s.Secrets.Public().String()})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	req := stopRequest{GraceSeconds: 10}
	if r.ContentLength != 0 {
		if err := decode(w, r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	grace := time.Duration(req.GraceSeconds) * time.Second
	if grace < 0 || grace > maxGrace {
		writeError(w, http.StatusBadRequest, fmt.Errorf("grace_seconds must be between 0 and %d", int(maxGrace.Seconds())))
		return
	}
	if err := s.Engine.Stop(r.Context(), id, grace); err != nil {
		s.fail(w, "stop", id, err)
		return
	}
	s.Log.Info("container stopped", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

type usageJSON struct {
	MemoryBytes int64 `json:"memory_bytes"`
	CPUUsec     int64 `json:"cpu_usec"`
}

// usage reports what a running container uses. A container that is not
// running has no usage.
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	u, err := s.Engine.Usage(id)
	if err != nil {
		writeError(w, http.StatusNotFound, errors.New("the container is not running"))
		return
	}
	writeJSON(w, http.StatusOK, usageJSON{MemoryBytes: u.MemoryBytes, CPUUsec: u.CPUUsec})
}

type hostJSON struct {
	CPUs          int   `json:"cpus"`
	MemoryBytes   int64 `json:"memory_bytes"`
	DiskBytes     int64 `json:"disk_bytes"`
	DiskFreeBytes int64 `json:"disk_free_bytes"`
}

func (s *Server) host(w http.ResponseWriter, r *http.Request) {
	info := s.Host
	if info == nil {
		info = func() (engine.Host, error) { return engine.HostInfo(s.Paths.Root) }
	}
	h, err := info()
	if err != nil {
		s.fail(w, "host", "", err)
		return
	}
	writeJSON(w, http.StatusOK, hostJSON{CPUs: h.CPUs, MemoryBytes: h.MemoryBytes, DiskBytes: h.DiskBytes, DiskFreeBytes: h.DiskFreeBytes})
}

// wait answers once the container's process has exited, with its exit
// code. The caller decides how long to wait by closing the request.
func (s *Server) wait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	code, err := s.Engine.Wait(r.Context(), id)
	if err != nil {
		s.fail(w, "wait", id, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]uint32{"exit_code": code})
}

func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	if err := s.Engine.Remove(r.Context(), id); err != nil {
		s.fail(w, "remove", id, err)
		return
	}
	s.Log.Info("container removed", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// removeImage deletes an image Zelie built. Images pulled from a registry
// are not the panel's to remove.
func (s *Server) removeImage(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !strings.HasPrefix(name, engine.LocalImages) || len(name) > 255 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("only images named %s… can be removed", engine.LocalImages))
		return
	}
	if err := s.Engine.RemoveImage(r.Context(), name); err != nil {
		s.fail(w, "remove image", name, err)
		return
	}
	s.Log.Info("image removed", "image", name)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	list, err := s.Engine.List(r.Context())
	if err != nil {
		s.fail(w, "list", "", err)
		return
	}
	out := make([]containerJSON, 0, len(list))
	for _, c := range list {
		cj := containerJSON{ID: c.ID, App: c.App, Image: c.Image, State: c.State, Pid: c.Pid, Userns: c.Userns, Network: c.Network}
		if c.IP.IsValid() {
			cj.IP = c.IP.String()
		}
		out = append(out, cj)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	f, err := os.Open(engine.LogPathFor(s.Paths, id))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, errors.New("no logs for this container"))
		return
	}
	if err != nil {
		s.fail(w, "logs", id, err)
		return
	}
	defer f.Close()
	if tail := r.URL.Query().Get("tail"); tail != "" {
		n, err := strconv.ParseInt(tail, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, errors.New("tail must be a positive number of bytes"))
			return
		}
		if err := seekTail(f, n); err != nil {
			s.fail(w, "logs", id, err)
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("follow") != "1" {
		io.Copy(w, f)
		return
	}
	follow(r.Context(), f, w, 250*time.Millisecond)
}

// seekTail moves f to the start of the first whole line within the last n
// bytes, so a long log can be shown from its end.
func seekTail(f *os.File, n int64) error {
	st, err := f.Stat()
	if err != nil || st.Size() <= n {
		return err
	}
	// Start one byte early: if that byte ends a line, the line after it is
	// whole and is kept.
	start := st.Size() - n - 1
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return err
	}
	skipped, err := bufio.NewReader(io.LimitReader(f, n+1)).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	_, err = f.Seek(start+int64(len(skipped)), io.SeekStart)
	return err
}

// fail maps engine errors to status codes. Internal details go to the log,
// not to the caller.
func (s *Server) fail(w http.ResponseWriter, op, id string, err error) {
	switch {
	case errdefs.IsNotFound(err):
		writeError(w, http.StatusNotFound, errors.New("container not found"))
	case errdefs.IsAlreadyExists(err):
		writeError(w, http.StatusConflict, errors.New("container already exists"))
	default:
		s.Log.Error(op+" failed", "id", id, "err", err)
		writeError(w, http.StatusInternalServerError, fmt.Errorf("%s failed, see the core log", op))
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	if dec.More() {
		return errors.New("invalid request body: trailing data")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
