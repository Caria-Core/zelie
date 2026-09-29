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
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(8, 0, 0)
	for t.Before(limit) {
		y, m, d := t.Date()
		switch {
		case !has(s.month, int(m)):
			t = time.Date(y, m+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			t = time.Date(y, m, d+1, 0, 0, 0, 0, loc)
		case !has(s.hour, t.Hour()):
			t = time.Date(y, m, d, t.Hour()+1, 0, 0, 0, loc)
		case !has(s.minute, t.Minute()):
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
		if s.Matches(at) {
			return at
		}
	}
	return time.Time{}
}
