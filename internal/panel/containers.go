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
	"github.com/Caria-Core/zelie/internal/dataview"
	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/secret"
)

// Core is what the panel asks the privileged core to do. *core.Client
// implements it; tests use a fake.
type Core interface {
	List(ctx context.Context) ([]engine.Status, error)
	Run(ctx context.Context, s engine.Spec) error
	RunApp(ctx context.Context, s engine.Spec, sealedEnv []string, linked ...core.LinkedVar) (string, error)
	RunInstall(ctx context.Context, req core.InstallRequest) (string, error)
	PrepareVolume(ctx context.Context, name string, req core.PrepareRequest) (core.PrepareResponse, error)
	ListFiles(ctx context.Context, ref core.FileRef, dir string) (core.FileList, error)
	ReadFile(ctx context.Context, ref core.FileRef, path string) ([]byte, error)
	WriteFile(ctx context.Context, ref core.FileRef, path string, content []byte, create bool) error
	MakeFolder(ctx context.Context, ref core.FileRef, path string) error
	RenameFile(ctx context.Context, ref core.FileRef, from, to string) error
	DeleteFiles(ctx context.Context, ref core.FileRef, paths []string) error
	UploadFile(ctx context.Context, ref core.FileRef, path string, size int64, body io.Reader) error
	DownloadFile(ctx context.Context, ref core.FileRef, path string) (io.ReadCloser, int64, error)
	CompressFiles(ctx context.Context, ref core.FileRef, dir string, paths []string, name string) (string, error)
	ExtractFile(ctx context.Context, ref core.FileRef, path, dir string) (core.ExtractResult, error)
	WriteStdin(ctx context.Context, id string, data []byte) error
	Signal(ctx context.Context, id, signal string) error
	Pin(ctx context.Context, image string) (string, error)
	SetLinks(ctx context.Context, app string, links []engine.Link) error
	SetForwards(ctx context.Context, app string, forwards []engine.Forward) error
	ClearForwards(ctx context.Context, app string) error
	UsedPorts(ctx context.Context) ([]int, error)
	PublicAddress(ctx context.Context) (core.PublicAddress, error)
	Build(ctx context.Context, app, version string, env, sealedEnv []string, source io.Reader, out io.Writer) (build.Result, error)
	Wait(ctx context.Context, id string) (int, error)
	SecretKey(ctx context.Context) (secret.PublicKey, error)
	Reveal(ctx context.Context, app, sealed string) (string, error)
	Stop(ctx context.Context, id string, graceSeconds int) error
	Remove(ctx context.Context, id string) error
	Logs(ctx context.Context, id string, follow bool, tail int64, w io.Writer) error
	RemoveImage(ctx context.Context, name string) error
	SweepImages(ctx context.Context, keep []string) ([]string, error)
	RemoveBuildCache(ctx context.Context, app string) error
	SetExternal(ctx context.Context, app, engine, container string, port, target int, sealedPassword string) error
	RemoveExternal(ctx context.Context, app, engine, container string) error
	SyncExternal(ctx context.Context, list []core.ExternalListener) error
	SetSFTPVolumes(ctx context.Context, names []string) error
	SetSFTPPort(ctx context.Context, port int) error
	SFTP(ctx context.Context) (core.SFTPStatus, error)
	UpdateStatus(ctx context.Context) (core.UpdateStatus, error)
	Update(ctx context.Context, version string) error
	Usage(ctx context.Context, id string) (engine.Usage, error)
	Host(ctx context.Context) (engine.Host, error)
	CreateVolume(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, name string) error
	VolumeSizes(ctx context.Context) (map[string]int64, error)
	CreateBackup(ctx context.Context, app, container, kind string) (core.Backup, error)
	DownloadBackup(ctx context.Context, app, name string, w io.Writer) error
	RemoveBackup(ctx context.Context, app, name string) error
	RestoreBackup(ctx context.Context, app, name, kind, container, volume string) error
	BackUpVolumes(ctx context.Context, app string, vols []core.VolumeRef, live bool) (core.Backup, error)
	RestoreVolumes(ctx context.Context, app, name string, vols []core.VolumeRef, size int64) error
	CopyVolume(ctx context.Context, from, to string) error
	RecoveryKey(ctx context.Context, host string, w io.Writer) error
	Offsite(ctx context.Context) (core.OffsiteInfo, error)
	SetOffsite(ctx context.Context, cfg core.OffsiteConfig) (core.OffsiteInfo, error)
	RemoveOffsite(ctx context.Context) error
	UploadBackup(ctx context.Context, app, name string, meta core.BackupMeta) error
	OffsiteBackups(ctx context.Context) (core.OffsiteList, error)
	AddOldKey(ctx context.Context, recovery string) ([]string, error)
	RemoveOffsiteBackup(ctx context.Context, app, name string) error
	FetchBackup(ctx context.Context, app, name string) error
	StartUpload(ctx context.Context, app string, size int64) (core.Upload, error)
	Upload(ctx context.Context, id string) (core.Upload, error)
	AppendUpload(ctx context.Context, id string, offset int64, piece io.Reader) (core.Upload, error)
	RemoveUpload(ctx context.Context, id string) error
	ImportUpload(ctx context.Context, id, kind string) (core.Backup, error)
	DataTables(ctx context.Context, app, container, engine string) ([]dataview.Table, error)
	DataRows(ctx context.Context, app, container, engine string, q dataview.Query) (dataview.Page, error)
	DataExport(ctx context.Context, app, container, engine string, q dataview.Query, w io.Writer) error
	DataKeys(ctx context.Context, app, container, pattern, cursor string) (dataview.KeyPage, error)
	DataKey(ctx context.Context, app, container, key string) (dataview.Value, error)
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
		notice := errNoLogs.With()
		if errors.As(err, &ce) && ce.Status == http.StatusNotFound {
			notice = errLogsEmpty.With()
		} else {
			s.Log.Error("container logs", "id", id, "err", err)
		}
		ev.send("notice", notice)
	}
}

var (
	errNoLogs    = msg.Define(0, "logs.unreadable", "The logs could not be read.")
	errLogsEmpty = msg.Define(0, "logs.none", "This container has no logs yet.")
)

var errCoreDown = msg.Define(http.StatusBadGateway, "core.failed", "The Zelie core did not answer. See the server log.")

// coreFailed passes on what the core said about a bad request, and hides
// the details of anything else.
func (s *Server) coreFailed(w http.ResponseWriter, what string, err error) {
	writeError(w, s.coreError(what, err))
}

// coreError is the message for a failed request to the core: its own when it
// has one for the person, otherwise a note that the core is down.
func (s *Server) coreError(what string, err error) *msg.Error {
	var ce *core.Error
	if errors.As(err, &ce) && ce.Status < 500 {
		return &msg.Error{Status: ce.Status, Msg: ce.Msg()}
	}
	s.Log.Error(what, "err", err)
	return errCoreDown.Err()
}

// isNotFound reports whether the core said the thing asked for does not
// exist.
func isNotFound(err error) bool {
	var ce *core.Error
	return errors.As(err, &ce) && ce.Status == http.StatusNotFound
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

// send writes one event; its data is v as JSON. A "notice" carries a
// msg.Msg, for the web interface to show in the user's language.
func (ev *eventStream) send(name string, v any) {
	ev.mu.Lock()
	defer ev.mu.Unlock()
	ev.event(name, v)
}

func (ev *eventStream) event(name string, v any) error {
	b, _ := json.Marshal(v)
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
