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
	"syscall"
	"time"

	"github.com/Caria-Core/zelie/internal/backup"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
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
	Images(ctx context.Context) ([]engine.Image, error)
	SetUnused(ctx context.Context, name string, since time.Time) error
	Pin(ctx context.Context, ref string) (string, error)
	Wait(ctx context.Context, id string) (uint32, error)
	WriteStdin(ctx context.Context, id string, data []byte) error
	Signal(ctx context.Context, id string, sig syscall.Signal) error
	Usage(id string) (engine.Usage, error)
	CreateVolume(name string) error
	RemoveVolume(ctx context.Context, name string) error
	VolumeSizes() (map[string]int64, error)
	Links(app string) ([]engine.Link, error)
	SetLinks(ctx context.Context, app string, links []engine.Link) error
	SetForwards(ctx context.Context, app string, forwards []engine.Forward) error
	Exec(ctx context.Context, id string, args []string, stdin io.Reader, stdout, stderr io.Writer) (uint32, error)
	OpenVolume(ctx context.Context, name string) (*os.Root, error)
	ReadVolume(name string) (*os.Root, error)
	VolumeSize(name string) (int64, error)
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
	Backups *backup.Dir
	Offsite *Offsite
	// External holds the loopback ports of databases open to desktop tools.
	External *External
	// Updater updates Zelie itself; nil where it cannot.
	Updater *Updater
	Paths   engine.Paths
	Log     *slog.Logger
	Allowed peer.Policy
	// SFTPUID is the user of the SFTP server, and SFTPVolumes the volumes
	// it may use. Without a user there is no such rule to apply.
	SFTPUID     uint32
	SFTPVolumes *SFTPVolumes
	// SFTPSocket holds the port and the host key of the SFTP server.
	SFTPSocket *SFTPSocket
	// Host describes the server; tests replace it.
	Host func() (engine.Host, error)

	busy busy
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/containers", s.list)
	mux.HandleFunc("POST /v1/containers", s.run)
	mux.HandleFunc("POST /v1/containers/{id}/stop", s.stop)
	mux.HandleFunc("DELETE /v1/containers/{id}", s.remove)
	mux.HandleFunc("POST /v1/containers/{id}/wait", s.wait)
	mux.HandleFunc("POST /v1/containers/{id}/stdin", s.stdin)
	mux.HandleFunc("POST /v1/containers/{id}/signal", s.signal)
	mux.HandleFunc("GET /v1/containers/{id}/logs", s.logs)
	mux.HandleFunc("GET /v1/containers/{id}/usage", s.usage)
	mux.HandleFunc("GET /v1/host", s.host)
	mux.HandleFunc("GET /v1/volumes", s.volumes)
	mux.HandleFunc("POST /v1/volumes", s.createVolume)
	mux.HandleFunc("DELETE /v1/volumes/{name}", s.removeVolume)
	mux.HandleFunc("POST /v1/volumes/{name}/prepare", s.prepareVolume)
	mux.HandleFunc("POST /v1/volumes/{name}/files/list", s.listFiles)
	mux.HandleFunc("GET /v1/volumes/{name}/files/content", s.readFile)
	mux.HandleFunc("PUT /v1/volumes/{name}/files/content", s.writeFile)
	mux.HandleFunc("POST /v1/volumes/{name}/files/stat", s.statFile)
	mux.HandleFunc("POST /v1/volumes/{name}/files/remove", s.removeFile)
	mux.HandleFunc("POST /v1/volumes/{name}/files/mkdir", s.makeFolder)
	mux.HandleFunc("POST /v1/volumes/{name}/files/rename", s.renameFile)
	mux.HandleFunc("POST /v1/volumes/{name}/files/delete", s.deleteFiles)
	mux.HandleFunc("PUT /v1/volumes/{name}/files/upload", s.uploadFile)
	mux.HandleFunc("GET /v1/volumes/{name}/files/download", s.downloadFile)
	mux.HandleFunc("POST /v1/volumes/{name}/files/compress", s.compressFiles)
	mux.HandleFunc("POST /v1/volumes/{name}/files/extract", s.extractFile)
	mux.HandleFunc("POST /v1/volumes/{name}/peek", s.peekVolume)
	mux.HandleFunc("PUT /v1/sftp/volumes", s.setSFTPVolumes)
	mux.HandleFunc("PUT /v1/sftp/port", s.setSFTPPort)
	mux.HandleFunc("GET /v1/sftp", s.sftpStatus)
	mux.HandleFunc("GET /v1/links/{app}", s.links)
	mux.HandleFunc("PUT /v1/links/{app}", s.setLinks)
	mux.HandleFunc("PUT /v1/forwards/{app}", s.setForwards)
	mux.HandleFunc("DELETE /v1/forwards/{app}", s.clearForwards)
	mux.HandleFunc("GET /v1/ports", s.usedPorts)
	mux.HandleFunc("GET /v1/address", s.publicAddress)
	mux.HandleFunc("POST /v1/installs", s.install)
	mux.HandleFunc("POST /v1/builds", s.build)
	mux.HandleFunc("DELETE /v1/builds/{app}/cache", s.removeBuildCache)
	mux.HandleFunc("DELETE /v1/images", s.removeImage)
	mux.HandleFunc("POST /v1/images/sweep", s.sweepImages)
	mux.HandleFunc("POST /v1/images/pin", s.pinImage)
	mux.HandleFunc("GET /v1/version", s.version)
	mux.HandleFunc("GET /v1/update", s.updateStatus)
	mux.HandleFunc("POST /v1/update", s.startUpdate)
	mux.HandleFunc("PUT /v1/external", s.syncExternal)
	mux.HandleFunc("PUT /v1/external/{app}", s.setExternal)
	mux.HandleFunc("DELETE /v1/external/{app}", s.removeExternal)
	mux.HandleFunc("GET /v1/secrets/key", s.secretKey)
	mux.HandleFunc("POST /v1/backups", s.createBackup)
	mux.HandleFunc("GET /v1/backups/{app}", s.listBackups)
	mux.HandleFunc("GET /v1/backups/{app}/{name}", s.downloadBackup)
	mux.HandleFunc("DELETE /v1/backups/{app}/{name}", s.removeBackup)
	mux.HandleFunc("POST /v1/backups/{app}/{name}/restore", s.restoreBackup)
	mux.HandleFunc("GET /v1/backups-key", s.recoveryKey)
	mux.HandleFunc("POST /v1/data/{app}", s.readData)
	mux.HandleFunc("POST /v1/uploads", s.createUpload)
	mux.HandleFunc("GET /v1/uploads/{id}", s.getUpload)
	mux.HandleFunc("PUT /v1/uploads/{id}", s.appendUpload)
	mux.HandleFunc("DELETE /v1/uploads/{id}", s.removeUpload)
	mux.HandleFunc("POST /v1/uploads/{id}/import", s.importUpload)
	mux.HandleFunc("POST /v1/backups-key/old", s.addOldKey)
	mux.HandleFunc("POST /v1/backups/{app}/{name}/offsite", s.uploadBackup)
	mux.HandleFunc("GET /v1/offsite", s.getOffsite)
	mux.HandleFunc("PUT /v1/offsite", s.setOffsite)
	mux.HandleFunc("DELETE /v1/offsite", s.removeOffsite)
	mux.HandleFunc("GET /v1/offsite/backups", s.listOffsite)
	mux.HandleFunc("DELETE /v1/offsite/backups/{app}/{name}", s.removeOffsiteBackup)
	mux.HandleFunc("POST /v1/offsite/backups/{app}/{name}/fetch", s.fetchBackup)
	return s.onlySFTPVolumes(peer.RequireRoutes(s.Allowed, s.Log, mux))
}

// SFTPRoutes are the only routes the SFTP process may use: the file
// operations of a volume, and nothing that starts, stops or configures
// anything. Which volume a login gets is the panel's decision.
var SFTPRoutes = []string{
	"POST /v1/volumes/{name}/files/list",
	"POST /v1/volumes/{name}/files/stat",
	"POST /v1/volumes/{name}/files/mkdir",
	"POST /v1/volumes/{name}/files/rename",
	"POST /v1/volumes/{name}/files/remove",
	"PUT /v1/volumes/{name}/files/upload",
	"GET /v1/volumes/{name}/files/download",
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
	SealedEnv []string `json:"sealed_env,omitempty"`
	// LinkedEnv holds variables made from a secret of an app that App is
	// linked to, such as a database's password.
	LinkedEnv   []linkedVarJSON   `json:"linked_env,omitempty"`
	Network     string            `json:"network,omitempty"`
	Volumes     []volumeMountJSON `json:"volumes,omitempty"`
	MemoryBytes int64             `json:"memory_bytes"`
	CPUs        float64           `json:"cpus"`
	Pids        int64             `json:"pids"`
	// User, WorkDir and Stdin are for game servers: see engine.Spec.
	User    *userJSON `json:"user,omitempty"`
	WorkDir string    `json:"work_dir,omitempty"`
	Stdin   bool      `json:"stdin,omitempty"`
}

type userJSON struct {
	UID uint32 `json:"uid"`
	GID uint32 `json:"gid"`
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
		WorkDir: req.WorkDir, Stdin: req.Stdin,
	}
	if req.User != nil {
		spec.User = &engine.IDs{UID: req.User.UID, GID: req.User.GID}
	}
	for _, v := range req.Volumes {
		spec.Volumes = append(spec.Volumes, engine.VolumeMount{Name: v.Name, Target: v.Target})
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
	linked, err := s.openLinked(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	spec.Env = append(spec.Env, linked...)
	if err := s.Engine.Run(r.Context(), spec); err != nil {
		s.fail(w, "run", req.ID, err)
		return
	}
	// What ran, by digest, so the panel can start the same image again.
	image, err := s.Engine.Pin(r.Context(), spec.Image)
	if err != nil {
		s.Log.Error("pin image", "image", spec.Image, "err", err)
		image = spec.Image
	}
	s.Log.Info("container started", "id", req.ID, "image", image)
	writeJSON(w, http.StatusCreated, runResponse{Image: image})
}

type runResponse struct {
	Image string `json:"image"`
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
	RxBytes     int64 `json:"rx_bytes"`
	TxBytes     int64 `json:"tx_bytes"`
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
	writeJSON(w, http.StatusOK, usageJSON{MemoryBytes: u.MemoryBytes, CPUUsec: u.CPUUsec, RxBytes: u.RxBytes, TxBytes: u.TxBytes})
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
	case errdefs.IsFailedPrecondition(err):
		writeError(w, http.StatusConflict, errors.New("the container cannot do that in its state"))
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

// errorJSON is an error as the core sends it. A message meant for the user
// also has its code and values, so the panel can show it in any language.
type errorJSON struct {
	Error  string         `json:"error"`
	Code   string         `json:"code,omitempty"`
	Params map[string]any `json:"params,omitempty"`
}

func writeError(w http.ResponseWriter, status int, err error) {
	out := errorJSON{Error: err.Error()}
	var m *msg.Error
	if errors.As(err, &m) {
		out.Code, out.Params = m.Code, m.Params
	}
	writeJSON(w, status, out)
}
