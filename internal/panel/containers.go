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

	"github.com/Caria-Core/zelie/internal/build"
	"github.com/Caria-Core/zelie/internal/core"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/secret"
)

// Core is what the panel asks the privileged core to do. *core.Client
// implements it; tests use a fake.
type Core interface {
	List(ctx context.Context) ([]engine.Status, error)
	Run(ctx context.Context, s engine.Spec) error
	RunApp(ctx context.Context, s engine.Spec, sealedEnv []string) error
	Build(ctx context.Context, app, version string, env, sealedEnv []string, source io.Reader, out io.Writer) (build.Result, error)
	Wait(ctx context.Context, id string) (int, error)
	SecretKey(ctx context.Context) (secret.PublicKey, error)
	Stop(ctx context.Context, id string, graceSeconds int) error
	Remove(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error
	RemoveImage(ctx context.Context, name string) error
	Usage(ctx context.Context, id string) (engine.Usage, error)
	Host(ctx context.Context) (engine.Host, error)
}

// Limits an app gets when the request does not say otherwise.
const (
	defaultMemoryMB = 512
	defaultCPUs     = 1
	defaultPids     = 512
	// logTail is how much earlier output a log view starts with.
	logTail = 64 << 10
)

// streamLogs streams a container's output as server-sent events. Each
// "output" event carries a chunk of text as a JSON string; a "notice" says
// why the stream ended early. ("error" would clash with the browser's own
// connection error event.)
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, id string) {
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
