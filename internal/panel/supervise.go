package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/store"
)

// An app that stops by itself is brought back up, waiting longer after each
// time, until it has stopped crashLimit times within crashWindow. Then Zelie
// leaves it down and says so, until the user deploys or restarts it.
const (
	crashLimit  = 5
	crashWindow = 10 * time.Minute
)

// Tests shorten these.
var (
	superviseEvery = 5 * time.Second
	crashDelay     = 5 * time.Second // doubles with each crash in the window
)

type crashes struct {
	mu    sync.Mutex
	byApp map[string]*crashRecord
}

type crashRecord struct {
	times  []time.Time // crashes within the window
	next   time.Time   // not brought back before this
	gaveUp string      // why Zelie stopped bringing it back
}

func (c *crashes) record(app string) *crashRecord {
	if c.byApp == nil {
		c.byApp = map[string]*crashRecord{}
	}
	r, ok := c.byApp[app]
	if !ok {
		r = &crashRecord{}
		c.byApp[app] = r
	}
	return r
}

// reset forgets an app's crashes, once the user has put it live again.
func (c *crashes) reset(app string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.byApp, app)
}

// gaveUp returns why Zelie stopped bringing the app back, or "".
func (c *crashes) gaveUp(app string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.byApp[app]; ok {
		return r.gaveUp
	}
	return ""
}

// supervise brings back live apps that have stopped: after a crash, or
// because the server restarted and took every container down with it.
func (s *Server) supervise(ctx context.Context) {
	t := time.NewTicker(superviseEvery)
	defer t.Stop()
	for {
		s.superviseOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) superviseOnce(ctx context.Context) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		s.Log.Error("supervise: list apps", "err", err)
		return
	}
	list, err := s.Core.List(ctx)
	if err != nil {
		s.Log.Error("supervise: list containers", "err", err)
		return
	}
	states := make(map[string]string, len(list))
	for _, c := range list {
		states[c.ID] = c.State
	}
	for _, a := range apps {
		// A restore stopped it, and starts it again when done.
		if a.Stopped || s.pauses.paused(a.ID) {
			continue
		}
		live, err := s.Store.LiveDeployment(ctx, a.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.Log.Error("supervise: load deployment", "app", a.ID, "err", err)
			continue
		}
		// A deployment on its way will replace the container anyway.
		if recent, err := s.Store.Deployments(ctx, a.ID, 1); err != nil || (len(recent) > 0 && recent[0].FinishedAt.IsZero()) {
			continue
		}
		state, ok := states[fmt.Sprintf("%s-%d", a.ID, live.ID)]
		if ok && state != "stopped" {
			continue
		}
		s.bringBack(ctx, a, live)
	}
}

// bringBack starts the live version of an app again, unless it has crashed
// too often or too recently.
func (s *Server) bringBack(ctx context.Context, a store.App, live store.Deployment) {
	now := s.now()
	s.crashes.mu.Lock()
	r := s.crashes.record(a.ID)
	if r.gaveUp != "" || now.Before(r.next) {
		s.crashes.mu.Unlock()
		return
	}
	kept := r.times[:0]
	for _, t := range r.times {
		if now.Sub(t) < crashWindow {
			kept = append(kept, t)
		}
	}
	r.times = append(kept, now)
	if len(r.times) >= crashLimit {
		r.gaveUp = fmt.Sprintf("it stopped by itself %d times within %s, so Zelie left it down", len(r.times), crashWindow)
		s.crashes.mu.Unlock()
		s.Log.Warn("app keeps stopping, giving up", "app", a.ID)
		return
	}
	r.next = now.Add(crashDelay << (len(r.times) - 1))
	s.crashes.mu.Unlock()

	s.Log.Info("bringing app back up", "app", a.ID, "attempt", len(r.times))
	if _, err := s.deploy(ctx, a, store.Deployment{Version: live.Version, Image: live.Image, Cause: store.CauseRecover, Message: live.Message}); err != nil {
		s.Log.Error("bring app back up", "app", a.ID, "err", err)
	}
}

// stopApp stops the app's live container and keeps it down until the user
// starts it again.
func (s *Server) stopApp(ctx context.Context, a store.App) error {
	if err := s.Store.SetStopped(ctx, a.ID, true); err != nil {
		return err
	}
	s.deploys.cancel(a.ID)
	unlock := s.deploys.lock(a.ID)
	defer unlock()
	list, err := s.Core.List(ctx)
	if err != nil {
		return err
	}
	for _, c := range list {
		if c.App == a.ID && c.State == "running" {
			if err := s.Core.Stop(ctx, c.ID, 10); err != nil {
				return err
			}
		}
	}
	return nil
}

// lastOutput writes what the app's live container printed last, so the log
// of the deployment that brings it back says why it went down.
func (s *Server) lastOutput(ctx context.Context, app string, w io.Writer) {
	live, err := s.Store.LiveDeployment(ctx, app)
	if err != nil {
		return
	}
	container := fmt.Sprintf("%s-%d", app, live.ID)
	if _, err := s.containerStatus(ctx, container); err != nil {
		fmt.Fprintln(w, "The app's container is gone, as after the server restarted.")
		return
	}
	fmt.Fprintln(w, "The app stopped by itself. Its last output:")
	s.Core.Logs(ctx, container, false, 4<<10, w)
	fmt.Fprintln(w)
}
