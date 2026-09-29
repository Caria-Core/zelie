package panel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/registry"
	"github.com/Caria-Core/zelie/internal/store"
	"github.com/opencontainers/go-digest"
)

const imageCheckEvery = 24 * time.Hour

var (
	errNotImage      = msg.Define(http.StatusConflict, "update.not_image", "Only apps made from an image are updated this way. Deploy the app instead.")
	errUpdateDBFirst = msg.Define(http.StatusConflict, "update.start_db_first", "Start the database first: what it holds is backed up before the update.")
	errUpdateBackup  = msg.Define(http.StatusInternalServerError, "update.backup_failed", "The backup before the update failed, so nothing was changed: {detail}")
)

// Registry answers what an image tag points at now. Tests replace it.
type Registry interface {
	Digest(ctx context.Context, ref string) (digest.Digest, error)
	Version(ctx context.Context, ref string) (string, error)
}

type liveRegistry struct{}

func (liveRegistry) Digest(ctx context.Context, ref string) (digest.Digest, error) {
	return registry.Digest(ctx, ref)
}

func (liveRegistry) Version(ctx context.Context, ref string) (string, error) {
	return registry.Version(ctx, ref)
}

// imageUpdate is a newer build of the tag an app runs. The versions are
// what the images say they are, and may be empty.
type imageUpdate struct {
	Current string        `json:"current,omitempty"`
	Version string        `json:"version,omitempty"`
	Digest  digest.Digest `json:"-"`
}

type imageUpdates struct {
	mu    sync.Mutex
	byApp map[string]imageUpdate
	// versions caches what each digest says it is; a digest never changes.
	versions map[digest.Digest]string
}

func (u *imageUpdates) get(app string) *imageUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up, ok := u.byApp[app]; ok {
		return &up
	}
	return nil
}

func (u *imageUpdates) set(app string, up *imageUpdate) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.byApp == nil {
		u.byApp = map[string]imageUpdate{}
	}
	if up == nil {
		delete(u.byApp, app)
	} else {
		u.byApp[app] = *up
	}
}

// wentLive forgets an update once the app runs it.
func (u *imageUpdates) wentLive(app, image string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if up, ok := u.byApp[app]; ok && digestOf(image) == up.Digest {
		delete(u.byApp, app)
	}
}

func (s *Server) registry() Registry {
	if s.Registry != nil {
		return s.Registry
	}
	return liveRegistry{}
}

// runImageCheck looks once a day for newer builds of the tags that apps
// and databases run. Nothing is downloaded until the user updates.
func (s *Server) runImageCheck(ctx context.Context) {
	t := time.NewTicker(imageCheckEvery)
	defer t.Stop()
	for {
		s.checkImages(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) checkImages(ctx context.Context) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.Log.Error("image check: list apps", "err", err)
		return
	}
	for _, a := range apps {
		if a.Source != store.SourceImage {
			continue
		}
		live, err := s.Store.LiveDeployment(ctx, a.ID)
		if err != nil || !engine.Pinned(live.Image) {
			continue
		}
		up, err := s.imageUpdate(ctx, a.Image, live.Image)
		if err != nil {
			s.Log.Warn("image check", "app", a.ID, "image", a.Image, "err", err)
			continue
		}
		s.imageUpdates.set(a.ID, up)
	}
}

// imageUpdate compares what tag points at now with running, the pinned
// image of the live deployment. It returns nil when they are the same.
func (s *Server) imageUpdate(ctx context.Context, tag, running string) (*imageUpdate, error) {
	latest, err := s.registry().Digest(ctx, tag)
	if err != nil {
		return nil, err
	}
	if latest == digestOf(running) {
		return nil, nil
	}
	return &imageUpdate{
		Current: s.imageVersion(ctx, running),
		Version: s.imageVersion(ctx, tag+"@"+latest.String()),
		Digest:  latest,
	}, nil
}

// imageVersion is the version a pinned image says it is, or "" when it
// says nothing or the registry does not answer.
func (s *Server) imageVersion(ctx context.Context, pinned string) string {
	d := digestOf(pinned)
	u := &s.imageUpdates
	u.mu.Lock()
	v, ok := u.versions[d]
	u.mu.Unlock()
	if ok {
		return v
	}
	v, err := s.registry().Version(ctx, pinned)
	if err != nil {
		s.Log.Warn("image check: version", "image", pinned, "err", err)
		return ""
	}
	u.mu.Lock()
	if u.versions == nil {
		u.versions = map[digest.Digest]string{}
	}
	u.versions[d] = v
	u.mu.Unlock()
	return v
}

// digestOf returns the digest of a pinned image such as postgres:18@sha256:….
func digestOf(image string) digest.Digest {
	_, d, _ := strings.Cut(image, "@")
	return digest.Digest(d)
}

// updateImage deploys what the app's tag points at now. A database is
// backed up first, as part of the deployment; if the new image does not
// start, the old one runs again.
func (s *Server) updateImage(w http.ResponseWriter, r *http.Request) {
	a, ok := s.appFrom(w, r)
	if !ok {
		return
	}
	if a.Source != store.SourceImage {
		writeError(w, errNotImage.Err())
		return
	}
	if a.IsDatabase() && a.Stopped {
		writeError(w, errUpdateDBFirst.Err())
		return
	}
	if !s.unstop(w, r, a) {
		return
	}
	id, err := s.deploy(r.Context(), a, store.Deployment{Cause: store.CauseUpdate})
	if err != nil {
		s.fail(w, "deploy", err)
		return
	}
	s.Log.Info("image update", "app", a.ID, "image", a.Image, "user", loginFrom(r.Context()).account.ID)
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}
