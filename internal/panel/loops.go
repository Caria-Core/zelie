package panel

import (
	"context"
	"sync"
	"time"
)

// Background jobs run only while the server uses what they serve: with no
// schedule there is no schedule clock, with no backup no backup clock. A job
// is a round that returns whether it still has anything to watch. Once it
// says no, the job ends, and whatever creates new work for it calls wake.
type loops struct {
	mu sync.Mutex
	// ctx is set by start. Until then wake does nothing: whoever calls start
	// wakes every job once, and its first round looks at what is there.
	ctx  context.Context
	jobs map[string]*job
}

type job struct {
	// woken is set by a wake that came while a round was running. That round
	// may have looked just before the new work was saved.
	woken bool
}

// start lets jobs run until ctx ends.
func (l *loops) start(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ctx = ctx
}

// wake starts the named job unless it is running, and reports whether it did.
// The job runs round at once, then every period and whenever nudge fires. A
// job that is running looks once more before it would end. Call wake after
// the new work is saved, not before.
func (l *loops) wake(name string, every time.Duration, nudge <-chan struct{}, round func(context.Context) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ctx == nil || l.ctx.Err() != nil {
		return false
	}
	if j, ok := l.jobs[name]; ok {
		j.woken = true
		return false
	}
	if l.jobs == nil {
		l.jobs = map[string]*job{}
	}
	j := &job{}
	l.jobs[name] = j
	go l.run(l.ctx, name, j, every, nudge, round)
	return true
}

func (l *loops) run(ctx context.Context, name string, j *job, every time.Duration, nudge <-chan struct{}, round func(context.Context) bool) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		l.mu.Lock()
		j.woken = false
		l.mu.Unlock()
		if !round(ctx) {
			if l.end(name, j) {
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
			l.end(name, j)
			return
		case <-t.C:
		case <-nudge:
		}
	}
}

// end removes the job unless it was woken since its round began, and reports
// whether it did.
func (l *loops) end(name string, j *job) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if j.woken && l.ctx.Err() == nil {
		return false
	}
	delete(l.jobs, name)
	return true
}
