package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
)

// Core is what the panel asks the privileged core to do. *core.Client
// implements it; tests use a fake.
type Core interface {
	List(ctx context.Context) ([]engine.Status, error)
	Run(ctx context.Context, s engine.Spec) error
	Stop(ctx context.Context, id string, graceSeconds int) error
	Remove(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error
}

// Limits a container gets when the request does not say otherwise.
const (
	defaultMemoryMB = 512
	defaultCPUs     = 1
	defaultPids     = 512
	// logTail is how much earlier output a log view starts with.
	logTail = 64 << 10
)

type containerJSON struct {
	ID      string `json:"id"`
	Image   string `json:"image"`
	State   string `json:"state"`
	Network string `json:"network,omitempty"`
	IP      string `json:"ip,omitempty"`
}

func (s *Server) listContainers(w http.ResponseWriter, r *http.Request) {
	list, err := s.Core.List(r.Context())
	if err != nil {
		s.coreFailed(w, "list containers", err)
		return
	}
	out := make([]containerJSON, 0, len(list))
	for _, c := range list {
		cj := containerJSON{ID: c.ID, Image: c.Image, State: c.State, Network: c.Network}
		if c.IP.IsValid() {
			cj.IP = c.IP.String()
		}
		out = append(out, cj)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) runContainer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID       string  `json:"id"`
		Image    string  `json:"image"`
		Network  string  `json:"network"`
		MemoryMB int64   `json:"memory_mb"`
		CPUs     float64 `json:"cpus"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.MemoryMB < 0 || req.MemoryMB > 1<<20 || req.CPUs < 0 || req.CPUs > 1024 {
		writeError(w, http.StatusBadRequest, errors.New("the memory or CPU limit is out of range"))
		return
	}
	spec := engine.Spec{
		ID: req.ID, Image: req.Image, Network: req.Network,
		MemoryBytes: orDefault(req.MemoryMB, defaultMemoryMB) << 20,
		CPUs:        orDefault(req.CPUs, defaultCPUs),
		Pids:        defaultPids,
	}
	// The core checks the spec too; checking here gives the user the
	// message before an image download starts.
	if err := spec.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	l := loginFrom(r.Context())
	if err := s.Core.Run(r.Context(), spec); err != nil {
		s.coreFailed(w, "run container", err)
		return
	}
	s.Log.Info("container started", "id", spec.ID, "image", spec.Image, "user", l.account.ID)
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) stopContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Core.Stop(r.Context(), id, 10); err != nil {
		s.coreFailed(w, "stop container", err)
		return
	}
	s.Log.Info("container stopped", "id", id, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeContainer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Core.Remove(r.Context(), id); err != nil {
		s.coreFailed(w, "remove container", err)
		return
	}
	s.Log.Info("container removed", "id", id, "user", loginFrom(r.Context()).account.ID)
	w.WriteHeader(http.StatusNoContent)
}

// containerLogs streams a container's output as server-sent events. Each
// "output" event carries a chunk of text as a JSON string; a "notice" says
// why the stream ended early. ("error" would clash with the browser's own
// connection error event.)
func (s *Server) containerLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !engine.ValidID(id) {
		writeError(w, http.StatusBadRequest, errors.New("invalid container id"))
		return
	}
	ev := newEventStream(w)
	defer ev.close()
	err := s.Core.Logs(r.Context(), id, true, logTail, ev)
	if err != nil && r.Context().Err() == nil {
		var ce *core.Error
		msg := "the logs could not be read"
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			msg = "this container has no logs yet"
		} else {
			s.Log.Error("container logs", "id", id, "err", err)
		}
		ev.send("notice", msg)
	}
}

// coreFailed passes on what the core said about a bad request, and hides
// the details of anything else.
func (s *Server) coreFailed(w http.ResponseWriter, what string, err error) {
	var ce *core.Error
	if errors.As(err, &ce) && ce.Status < 500 {
		writeError(w, ce.Status, ce)
		return
	}
	s.Log.Error(what, "err", err)
	writeError(w, http.StatusBadGateway, errors.New("the Zelie core did not answer; see the server log"))
}

// eventStream writes server-sent events. Output from a container arrives
// in arbitrary pieces, so a multi-byte character split between two pieces
// is held back until it is whole.
type eventStream struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	partial []byte
	stop    chan struct{}
}

func newEventStream(w http.ResponseWriter) *eventStream {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	ev := &eventStream{w: w, rc: http.NewResponseController(w), stop: make(chan struct{})}
	ev.rc.Flush()
	// Connections with no traffic get closed by some tunnels and proxies
	// after about a minute and a half.
	go func() {
		t := time.NewTicker(25 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ev.stop:
				return
			case <-t.C:
				ev.mu.Lock()
				io.WriteString(ev.w, ": keep-alive\n\n")
				ev.rc.Flush()
				ev.mu.Unlock()
			}
		}
	}()
	return ev
}

func (ev *eventStream) Write(p []byte) (int, error) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	data := append(ev.partial, p...)
	cut := len(data)
	// Hold back an incomplete UTF-8 sequence at the end, at most three bytes.
	for i := len(data) - 1; i >= 0 && i >= len(data)-3; i-- {
		if utf8.RuneStart(data[i]) {
			if !utf8.FullRune(data[i:]) {
				cut = i
			}
			break
		}
	}
	ev.partial = append([]byte(nil), data[cut:]...)
	if cut > 0 {
		if err := ev.event("output", string(data[:cut])); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (ev *eventStream) send(name, text string) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.event(name, text)
}

func (ev *eventStream) event(name, text string) error {
	b, _ := json.Marshal(text)
	if _, err := io.WriteString(ev.w, "event: "+name+"\ndata: "+string(b)+"\n\n"); err != nil {
		return err
	}
	return ev.rc.Flush()
}

func (ev *eventStream) close() { close(ev.stop) }

func orDefault[T int64 | float64](v, fallback T) T {
	if v == 0 {
		return fallback
	}
	return v
}
