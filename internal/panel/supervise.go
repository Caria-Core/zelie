package panel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/msg"
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
	gaveUp *msg.Msg    // why Zelie stopped bringing it back
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

var errCrashing = msg.Define(0, "app.gave_up", "It stopped by itself {count} times within {minutes} minutes, so Zelie left it down.")

// gaveUp returns why Zelie stopped bringing the app back, or nil.
func (c *crashes) gaveUp(app string) *msg.Msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.byApp[app]; ok {
		return r.gaveUp
	}
	return nil
}

// wakeSupervise starts the supervisor, which runs while an app is meant to
// be up.
func (s *Server) wakeSupervise() {
	s.loops.wake("supervise", superviseEvery, nil, s.superviseOnce)
}

// liveApp is an app that is meant to be up, with the deployment that serves
// it.
type liveApp struct {
	app  store.App
	live store.Deployment
}

// liveApps lists the apps the user has not stopped that have a live
// deployment. whole is false when an app could not be looked at, so a short
// list proves nothing.
func (s *Server) liveApps(ctx context.Context, job string) (list []liveApp, whole bool, err error) {
	apps, err := s.Store.Apps(ctx)
	if err != nil {
		return nil, false, err
	}
	whole = true
	for _, a := range apps {
		if a.Stopped {
			continue
		}
		live, err := s.Store.LiveDeployment(ctx, a.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			s.Log.Error(job+": load deployment", "app", a.ID, "err", err)
			whole = false
			continue
		}
		list = append(list, liveApp{a, live})
	}
	return list, whole, nil
}

// wakeApps starts what watches the running apps. It is called when an app
// goes live or is started again.
func (s *Server) wakeApps() {
	s.wakeSupervise()
	s.wakeMetrics()
	s.wakeImageCheck()
}

// superviseOnce brings back live apps that have stopped: after a crash, or
// because the server restarted and took every container down with it. It
// returns false when no app is meant to be up, so there is nothing to
// supervise until one is.
func (s *Server) superviseOnce(ctx context.Context) bool {
	up, whole, err := s.liveApps(ctx, "supervise")
	if err != nil {
		s.Log.Error("supervise: list apps", "err", err)
		return true
	}
	if len(up) == 0 {
		return !whole
	}
	list, err := s.Core.List(ctx)
	if err != nil {
		s.Log.Error("supervise: list containers", "err", err)
		return true
	}
	states := make(map[string]string, len(list))
	for _, c := range list {
		states[c.ID] = c.State
	}
	for _, u := range up {
		// A restore stopped it, and starts it again when done.
		if s.pauses.paused(u.app.ID) {
			continue
		}
		state, ok := states[fmt.Sprintf("%s-%d", u.app.ID, u.live.ID)]
		if ok && state != "stopped" {
			continue
		}
		s.bringBack(ctx, u.app, u.live)
	}
	return true
}

// bringBack starts the live version of an app again, unless it has crashed
// too often or too recently.
func (s *Server) bringBack(ctx context.Context, a store.App, live store.Deployment) {
	// The round chose this app a moment ago. If the user has stopped it,
	// started it or put a new version on it since, it did not crash.
	if settled, err := s.Store.SettledOn(ctx, a.ID, live.ID); err != nil {
		s.Log.Error("supervise: look at app", "app", a.ID, "err", err)
		return
	} else if !settled {
		return
	}
	now := s.now()
	s.crashes.mu.Lock()
	r := s.crashes.record(a.ID)
	if r.gaveUp != nil || now.Before(r.next) {
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
		m := errCrashing.With("count", len(r.times), "minutes", int(crashWindow/time.Minute))
		r.gaveUp = &m
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
