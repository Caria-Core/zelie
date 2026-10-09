package panel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Caria-Core/zelie/internal/engine"
	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/Caria-Core/zelie/internal/store"
)

// What a container writes outside its volumes counts towards the app's disk
// allowance, which is the limits of its volumes added up, and at least 5 GB so
// that an app with no volume, or a small one, may still write a little
// outside them. The files are measured now and then, and an app that has
// outgrown the allowance is stopped. Starting it again makes a new container
// with empty files, so nothing keeps it from starting; it is stopped again if
// it fills them again.
//
// Only the containers that run the app count towards that. A test run or an
// install is a container of the app too, but it lives for one job, and
// stopping the app because of it would take the live version down for a test.
// Such a container is stopped alone, and its job fails with the reason.
//
// Apps that were already over the first time the check ran, on a server that
// had never run it, get a day before they are stopped, once. So does an app
// from before that check that was not running then and is first measured
// within that day. Anything that goes over later is stopped at once.

const (
	// diskFloorMB is the least an app may hold in all. It is its own number: a
	// new volume's default size is another thing and may change.
	diskFloorMB = 5120
	// layerGrace is how long the apps that were over when the check first ran
	// are left running.
	layerGrace = 24 * time.Hour
	// stepStopKeep is how long the reason for stopping a test run or an install
	// is kept for the job that waits for it.
	stepStopKeep = time.Hour
)

var (
	errLayerFull       = msg.Define(0, "app.layer_full", "Its files outside the volumes took {outside}. With the volumes it holds {used}, over its {limit} disk limit. Keep what the app writes in a volume, and raise the volumes' limits if they need more room.")
	errLayerUnmeasured = msg.Define(0, "app.layer_unmeasured", "The files it wrote outside the volumes cannot be measured, so its disk limit cannot be checked. Folders nested too deep are the usual cause. Keep what the app writes in a volume, or start it again to begin with empty files.")
	errStepFull        = msg.Define(0, "app.step_full", "The container was stopped after it wrote {outside} outside any volume, over the app's {limit} disk limit.")
)

// layerGraceJSON is the warning an app that is over its disk allowance gets
// before it is stopped.
type layerGraceJSON struct {
	// Until is when the app is stopped if it is still over.
	Until time.Time `json:"until"`
	Why   msg.Msg   `json:"why"`
}

// layerNotes returns what the disk check says about an app for its page: why
// it was stopped, if it was, or the warning it has been given.
func (s *Server) layerNotes(ctx context.Context, a store.App) (stoppedFor *msg.Msg, grace *layerGraceJSON, err error) {
	l, err := s.Store.AppLayers(ctx, a.ID)
	if err != nil {
		return nil, nil, err
	}
	if a.Stopped {
		return l.StoppedFor, nil, nil
	}
	if !l.Deadline.IsZero() && l.Warning != nil {
		grace = &layerGraceJSON{Until: l.Deadline, Why: *l.Warning}
	}
	return nil, grace, nil
}

// diskAllowanceMB is how much disk an app's files may take in all.
func diskAllowanceMB(vols []store.Volume) int64 {
	var total int64
	for _, v := range vols {
		total += v.LimitMB
	}
	return max(total, diskFloorMB)
}

// isServiceContainer reports whether a container runs the app itself: those
// are named for their deployment, as against a test run or an install.
func isServiceContainer(app, container string) bool {
	n, ok := strings.CutPrefix(container, app+"-")
	return ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// overDisk says why an app may not go on running, from the own files of its
// service containers and the last measurement of its volumes. Nil means it may.
func (s *Server) overDisk(vols []store.Volume, layers []engine.LayerSize) *msg.Error {
	var outside int64
	for _, l := range layers {
		if l.Unmeasured {
			return errLayerUnmeasured.Err()
		}
		outside += l.Bytes
	}
	used := outside
	for _, v := range vols {
		n, _ := s.sizes.get(v.Name)
		used += n
	}
	limit := diskAllowanceMB(vols)
	if used <= limit<<20 {
		return nil
	}
	return errLayerFull.Err("outside", formatMB(outside>>20), "used", formatMB(used>>20), "limit", formatMB(limit))
}

// overDiskAlone says why a one-shot container may not go on running: what it
// wrote outside any volume is more than the app may hold in all.
func overDiskAlone(vols []store.Volume, l engine.LayerSize) *msg.Error {
	if l.Unmeasured {
		return errLayerUnmeasured.Err()
	}
	limit := diskAllowanceMB(vols)
	if l.Bytes <= limit<<20 {
		return nil
	}
	return errStepFull.Err("outside", formatMB(l.Bytes>>20), "limit", formatMB(limit))
}

// seenApps are the apps whose own files have been measured since the panel
// started. An app first measured in the day after the first check, having been
// there before it, gets that day as the apps that were running then did.
type seenApps struct {
	mu   sync.Mutex
	apps map[string]bool
}

// see notes that app was measured and reports whether it had been before.
func (s *seenApps) see(app string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apps == nil {
		s.apps = map[string]bool{}
	}
	seen := s.apps[app]
	s.apps[app] = true
	return seen
}

// stepStops are the one-shot containers Zelie stopped for their files, with the
// reason, until the job that waited for the container has picked it up.
type stepStops struct {
	mu  sync.Mutex
	why map[string]stepStop
}

type stepStop struct {
	why *msg.Error
	at  time.Time
}

// put notes why a container was stopped. A container that ended by itself just
// as it was measured is stopped in vain and nobody takes its entry, so the old
// ones go here, which keeps the list from growing.
func (st *stepStops) put(container string, why *msg.Error, now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.why == nil {
		st.why = map[string]stepStop{}
	}
	for c, e := range st.why {
		if now.Sub(e.at) > stepStopKeep {
			delete(st.why, c)
		}
	}
	st.why[container] = stepStop{why, now}
}

// take returns the reason a container was stopped, and forgets it. Nil means
// it was not stopped for its files.
func (st *stepStops) take(container string) *msg.Error {
	st.mu.Lock()
	defer st.mu.Unlock()
	e := st.why[container]
	delete(st.why, container)
	return e.why
}

// wakeLayers starts the check of the files containers write, which runs while
// a container does. Whatever starts a container wakes it, after the container
// is running.
func (s *Server) wakeLayers() {
	s.loops.wake("layers", volumeCheckEvery, nil, s.checkLayers)
}

// checkLayers measures what the running containers wrote outside their
// volumes and stops an app that has outgrown its disk allowance with them. The
// core measures nothing while no container runs, and then this returns false:
// there is nothing to look at until a container starts.
func (s *Server) checkLayers(ctx context.Context) bool {
	layers, err := s.Core.LayerSizes(ctx)
	if err != nil {
		s.Log.Error("container files: measure", "err", err)
		return true
	}
	now := s.now()
	first, firstCheck, err := s.Store.FirstLayerCheck(ctx, now)
	if err != nil {
		// Without it the grace period cannot be told, and no app is stopped
		// on a guess.
		s.Log.Error("container files: note the first check", "err", err)
		return true
	}
	byApp := map[string][]engine.LayerSize{}
	for _, l := range layers {
		if isServiceContainer(l.App, l.Container) {
			byApp[l.App] = append(byApp[l.App], l)
		} else {
			s.checkOneShot(ctx, l, now)
		}
	}
	for appID, list := range byApp {
		s.checkAppLayers(ctx, appID, list, now, first, firstCheck)
	}
	return len(layers) > 0
}

// checkAppLayers judges one app by its service containers. first is when the
// check first ran on this server, and firstCheck whether that is now.
func (s *Server) checkAppLayers(ctx context.Context, appID string, list []engine.LayerSize, now, first time.Time, firstCheck bool) {
	a, err := s.Store.App(ctx, appID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		s.Log.Error("container files: load app", "app", appID, "err", err)
		return
	}
	if a.Stopped {
		return
	}
	vols, err := s.Store.Volumes(ctx, appID)
	if err != nil {
		s.Log.Error("container files: list volumes", "app", appID, "err", err)
		return
	}
	notes, err := s.Store.AppLayers(ctx, appID)
	if err != nil {
		s.Log.Error("container files: load notes", "app", appID, "err", err)
		return
	}
	seen := s.layerSeen.see(appID)
	over := s.overDisk(vols, list)
	if over == nil {
		if !notes.Deadline.IsZero() || notes.Warning != nil {
			if err := s.Store.ClearLayerWarning(ctx, appID); err != nil {
				s.Log.Error("container files: clear the warning", "app", appID, "err", err)
			}
		}
		return
	}

	// The apps that were over when the check first ran get a day before they
	// are stopped, and keep the deadline they were given. So does an app from
	// before the check that is measured for the first time within that day,
	// having not run until then, and one that cannot be measured: nothing says
	// yet that it is over. An app seen within its allowance that goes over
	// later is stopped at once.
	until := notes.Deadline
	before := !a.CreatedAt.After(first)
	if until.IsZero() && (firstCheck || before && (!seen || over.Code == errLayerUnmeasured.Code)) {
		until = first.Add(layerGrace)
	}
	if now.Before(until) {
		if err := s.Store.WarnLayers(ctx, appID, until, over.Msg); err != nil {
			s.Log.Error("container files: warn app", "app", appID, "err", err)
			return
		}
		if notes.Deadline.IsZero() {
			s.Log.Warn("container files over the app's disk allowance, it will be stopped if they still are", "app", appID, "at", until, "reason", over.Text)
		}
		return
	}

	s.Log.Warn("container files over the app's disk allowance, stopping the app", "app", appID, "reason", over.Text)
	if err := s.stopOverLimit(ctx, a); err != nil {
		s.Log.Error("container files: stop app", "app", appID, "err", err)
	}
	// Only an app that is marked stopped gets the reason, so a stop that did
	// not take hold leaves nothing behind.
	if err := s.Store.SetStoppedFor(ctx, appID, over.Msg); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.Log.Error("container files: note why the app was stopped", "app", appID, "err", err)
	}
}

// checkOneShot stops a test run or an install whose own files are too large,
// and leaves the app and its other containers as they are. The job that waits
// for the container fails with the reason.
func (s *Server) checkOneShot(ctx context.Context, l engine.LayerSize, now time.Time) {
	if _, err := s.Store.App(ctx, l.App); err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.Log.Error("container files: load app", "app", l.App, "container", l.Container, "err", err)
		}
		return
	}
	vols, err := s.Store.Volumes(ctx, l.App)
	if err != nil {
		s.Log.Error("container files: list volumes", "app", l.App, "container", l.Container, "err", err)
		return
	}
	over := overDiskAlone(vols, l)
	if over == nil {
		return
	}
	s.Log.Warn("container files over the app's disk allowance, stopping the container", "app", l.App, "container", l.Container, "reason", over.Text)
	s.stepStops.put(l.Container, over, now)
	if err := s.Core.Stop(ctx, l.Container, 10); err != nil {
		s.stepStops.take(l.Container)
		s.Log.Error("container files: stop container", "app", l.App, "container", l.Container, "err", err)
	}
}
