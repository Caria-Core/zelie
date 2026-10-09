// Package cron parses and evaluates five-field cron expressions: minute,
// hour, day of month, month and day of week.
package cron

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule is a parsed expression.
type Schedule struct {
	minute, hour, dom, month, dow uint64 // one bit per allowed value
	// domStar and dowStar say the field began with "*". Like classic cron,
	// a schedule that limits both the day of month and the day of week
	// matches a day when either does; if one is a star, the other decides.
	domStar, dowStar bool
	// hourStar says the hour field began with "*". Such a schedule keeps its
	// pace when the clocks change; see fires for the others.
	hourStar bool
}

// ErrSyntax is wrapped by every error Parse returns.
var ErrSyntax = errors.New("invalid cron expression")

type field struct {
	name     string
	min, max int
}

var fields = [5]field{
	{"minute", 0, 59},
	{"hour", 0, 23},
	{"day of month", 1, 31},
	{"month", 1, 12},
	{"day of week", 0, 7},
}

// Parse reads an expression such as "*/15 4-6 * * 1,3,5". Each field takes
// "*", numbers, ranges (a-b), lists (a,b,c) and steps (*/n, a-b/n, a/n).
// Sunday is 0 or 7.
func Parse(expr string) (Schedule, error) {
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Schedule{}, fmt.Errorf("%w: want 5 fields, got %d", ErrSyntax, len(parts))
	}
	var bits [5]uint64
	for i, p := range parts {
		b, err := parseField(p, fields[i])
		if err != nil {
			return Schedule{}, fmt.Errorf("%w: %s: %v", ErrSyntax, fields[i].name, err)
		}
		bits[i] = b
	}
	// 7 is another way to write Sunday.
	if bits[4]&(1<<7) != 0 {
		bits[4] = bits[4]&^(1<<7) | 1
	}
	return Schedule{
		minute: bits[0], hour: bits[1], dom: bits[2], month: bits[3], dow: bits[4],
		domStar: strings.HasPrefix(parts[2], "*"), dowStar: strings.HasPrefix(parts[4], "*"),
		hourStar: strings.HasPrefix(parts[1], "*"),
	}, nil
}

func parseField(s string, f field) (uint64, error) {
	var bits uint64
	for item := range strings.SplitSeq(s, ",") {
		if item == "" {
			return 0, errors.New("empty list item")
		}
		rng, step, hasStep := strings.Cut(item, "/")
		lo, hi := f.min, f.max
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")
			var err error
			if lo, err = number(a, f); err != nil {
				return 0, err
			}
			if hi, err = number(b, f); err != nil {
				return 0, err
			}
			if lo > hi {
				return 0, fmt.Errorf("range %s runs backwards", rng)
			}
		default:
			n, err := number(rng, f)
			if err != nil {
				return 0, err
			}
			lo, hi = n, n
			if hasStep {
				hi = f.max
			}
		}
		by := 1
		if hasStep {
			n, err := strconv.Atoi(step)
			if err != nil || n < 1 || n > f.max {
				return 0, fmt.Errorf("bad step %q", step)
			}
			by = n
		}
		for v := lo; v <= hi; v += by {
			bits |= 1 << v
		}
	}
	return bits, nil
}

func number(s string, f field) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || s != strconv.Itoa(n) {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if n < f.min || n > f.max {
		return 0, fmt.Errorf("%d is outside %d-%d", n, f.min, f.max)
	}
	return n, nil
}

func has(bits uint64, v int) bool { return bits&(1<<v) != 0 }

func (s Schedule) dayMatches(t time.Time) bool {
	dom := has(s.dom, t.Day())
	dow := has(s.dow, int(t.Weekday()))
	if s.domStar || s.dowStar {
		return dom && dow
	}
	return dom || dow
}

// Matches reports whether the schedule fires in t's minute.
func (s Schedule) Matches(t time.Time) bool {
	return has(s.minute, t.Minute()) && has(s.hour, t.Hour()) && has(s.month, int(t.Month())) && s.dayMatches(t)
}

// Next returns the first time after t at which the schedule fires, in t's
// zone, or the zero time if there is none within eight years, as for
// "0 0 30 2 *".
func (s Schedule) Next(t time.Time) time.Time {
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(8, 0, 0)
	for t.Before(limit) {
		y, m, d := t.Date()
		switch {
		case s.missed(t):
			return t
		case !has(s.month, int(m)):
			t = jump(t, y, m+1, 1, 0)
		case !s.dayMatches(t):
			t = jump(t, y, m, d+1, 0)
		case !has(s.hour, t.Hour()):
			t = jump(t, y, m, d, t.Hour()+1)
		case !has(s.minute, t.Minute()), s.repeat(t):
			t = t.Add(time.Minute)
		default:
			return t
		}
	}
	return time.Time{}
}

// Last returns the latest time at or before t, within the given window,
// at which the schedule fires, or the zero time.
func (s Schedule) Last(t time.Time, window time.Duration) time.Time {
	t = t.Truncate(time.Minute)
	for at := t; !at.Before(t.Add(-window)); at = at.Add(-time.Minute) {
		if s.fires(at) {
			return at
		}
	}
	return time.Time{}
}

// fires is Matches, adjusted for the clocks being changed. When they are
// set back, a minute is shown twice and a schedule fixed to hours runs only
// for the first. When they are set forward, a minute that was skipped runs
// at the first minute after the change.
func (s Schedule) fires(t time.Time) bool {
	return s.Matches(t) && !s.repeat(t) || s.missed(t)
}

// repeat reports whether t is the second time the clocks show its minute,
// for a schedule that runs only once for it.
func (s Schedule) repeat(t time.Time) bool {
	return !s.hourStar && setBack(t) > 0
}

// missed reports whether t is the first minute after the clocks were set
// forward, and the schedule was due in the minutes they skipped.
func (s Schedule) missed(t time.Time) bool {
	if s.hourStar {
		return false
	}
	start, _ := t.ZoneBounds()
	if start.IsZero() || !t.Equal(start) {
		return false
	}
	_, off := t.Zone()
	_, was := start.Add(-time.Second).Zone()
	if off <= was {
		return false
	}
	// The skipped minutes, read off the clock that now shows t.
	wall := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	for w := wall.Add(-time.Duration(off-was) * time.Second); w.Before(wall); w = w.Add(time.Minute) {
		if s.Matches(w) {
			return true
		}
	}
	return false
}

// setBack returns by how much the clocks were set back at the start of t's
// zone period if t lies within that much of it, so the clocks showed t's
// minute once before.
func setBack(t time.Time) time.Duration {
	start, _ := t.ZoneBounds()
	if start.IsZero() {
		return 0
	}
	_, off := t.Zone()
	_, was := start.Add(-time.Second).Zone()
	if d := time.Duration(was-off) * time.Second; d > 0 && t.Sub(start) < d {
		return d
	}
	return 0
}

// jump moves t towards the given hour of the given day on the clocks in its
// zone, and never past a change of the clocks. Away from a change it gets
// there. Near one time.Date cannot be trusted: it may pick either moment for
// a time shown twice, and any for a time that is skipped. There jump only
// goes on to the next full hour in real time, or to the change itself, and
// Next looks at every minute from there.
func jump(t time.Time, y int, m time.Month, d, h int) time.Time {
	next := time.Date(y, m, d, h, 0, 0, 0, t.Location())
	_, end := t.ZoneBounds()
	if next.After(t) && (end.IsZero() || next.Before(end)) && shows(next, y, m, d, h) {
		return next
	}
	step := t.Add(time.Duration(60-t.Minute()) * time.Minute)
	if !end.IsZero() && end.Before(step) {
		return end
	}
	return step
}

// shows reports whether the clocks at t read the hour that time.Date was
// asked for.
func shows(t time.Time, y int, m time.Month, d, h int) bool {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC).Equal(time.Date(y, m, d, h, 0, 0, 0, time.UTC))
}
