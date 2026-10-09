package cron

import (
	"errors"
	"testing"
	"time"
	_ "time/tzdata" // the tests do not depend on the machine's zone files
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestNext(t *testing.T) {
	tests := []struct{ expr, from, want string }{
		{"* * * * *", "2026-03-10 12:00", "2026-03-10 12:01"},
		{"*/15 * * * *", "2026-03-10 12:00", "2026-03-10 12:15"},
		{"*/15 * * * *", "2026-03-10 12:46", "2026-03-10 13:00"},
		{"0 * * * *", "2026-03-10 12:00", "2026-03-10 13:00"},
		{"0 */6 * * *", "2026-03-10 13:00", "2026-03-10 18:00"},
		{"0 4 * * *", "2026-03-10 03:59", "2026-03-10 04:00"},
		{"0 4 * * *", "2026-03-10 04:00", "2026-03-11 04:00"},
		// 2026-03-10 is a Tuesday.
		{"0 4 * * 1", "2026-03-10 04:00", "2026-03-16 04:00"},
		{"30 8 * * 1-5", "2026-03-13 09:00", "2026-03-16 08:30"},
		{"0 0 1,15 * *", "2026-03-10 00:00", "2026-03-15 00:00"},
		{"0 12 10-12 * *", "2026-03-12 12:00", "2026-04-10 12:00"},
		{"0 0 * 6 *", "2026-03-10 00:00", "2026-06-01 00:00"},
		{"5-10/2 * * * *", "2026-03-10 12:05", "2026-03-10 12:07"},
		{"10/20 * * * *", "2026-03-10 12:10", "2026-03-10 12:30"},
		{"0 0 29 2 *", "2026-03-10 00:00", "2028-02-29 00:00"},
		{"0 0 * * 7", "2026-03-10 00:00", "2026-03-15 00:00"},
		{"0 0 * * 0", "2026-03-10 00:00", "2026-03-15 00:00"},
		{"59 23 31 12 *", "2026-12-31 23:59", "2027-12-31 23:59"},
	}
	for _, tt := range tests {
		s, err := Parse(tt.expr)
		if err != nil {
			t.Errorf("Parse(%q): %v", tt.expr, err)
			continue
		}
		if got := s.Next(at(tt.from)); !got.Equal(at(tt.want)) {
			t.Errorf("%q after %s: got %s, want %s", tt.expr, tt.from, got.Format("2006-01-02 15:04"), tt.want)
		}
	}
}

func TestDayOfMonthOrDayOfWeek(t *testing.T) {
	// Both restricted: the 13th or any Friday.
	s, err := Parse("0 0 13 * 5")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		day  string
		want bool
	}{
		{"2026-03-13 00:00", true},  // a Friday and the 13th
		{"2026-03-06 00:00", true},  // a Friday
		{"2026-04-13 00:00", true},  // a Monday, the 13th
		{"2026-03-12 00:00", false}, // a Thursday
	} {
		if got := s.Matches(at(tt.day)); got != tt.want {
			t.Errorf("Matches(%s) = %v, want %v", tt.day, got, tt.want)
		}
	}
	// A star on one side leaves the decision to the other.
	s, _ = Parse("0 0 13 * *")
	if s.Matches(at("2026-03-06 00:00")) || !s.Matches(at("2026-03-13 00:00")) {
		t.Error("day of month alone is wrong")
	}
	s, _ = Parse("0 0 */2 * 5")
	if s.Matches(at("2026-03-06 00:00")) { // a Friday, but day 6 is not odd
		t.Error("a star step on the day of month must still count as a star")
	}
}

func TestParseErrors(t *testing.T) {
	for _, expr := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 0 *", "* * * 13 *", "* * * * 8",
		"a * * * *", "5-1 * * * *", "*/0 * * * *", "*/x * * * *", "1,,2 * * * *", "-1 * * * *", "01 * * * *", "1- * * * *", "*/100 * * * *",
	} {
		if _, err := Parse(expr); !errors.Is(err, ErrSyntax) {
			t.Errorf("Parse(%q) = %v, want a syntax error", expr, err)
		}
	}
}

func TestNextNever(t *testing.T) {
	s, err := Parse("0 0 30 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(at("2026-01-01 00:00")); !got.IsZero() {
		t.Errorf("got %s for a date that does not exist", got)
	}
}

func TestLast(t *testing.T) {
	s, _ := Parse("0 4 * * *")
	if got := s.Last(at("2026-03-10 04:03"), 5*time.Minute); !got.Equal(at("2026-03-10 04:00")) {
		t.Errorf("got %s", got)
	}
	if got := s.Last(at("2026-03-10 04:06"), 5*time.Minute); !got.IsZero() {
		t.Errorf("a slot older than the window was found: %s", got)
	}
	if got := s.Last(at("2026-03-10 04:00"), 0); !got.Equal(at("2026-03-10 04:00")) {
		t.Errorf("the current minute counts: %s", got)
	}
}

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

const stamp = "2006-01-02 15:04 -0700"

// in reads a time with its offset, as in stamp, in loc. The offset tells
// apart the two times the clocks show on the day they are set back.
func in(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	got, err := time.Parse(stamp, s)
	if err != nil {
		t.Fatal(err)
	}
	return got.In(loc)
}

// next calls Next and fails the test, rather than hanging it, when it does
// not return.
func next(t *testing.T, s Schedule, from time.Time) time.Time {
	t.Helper()
	done := make(chan time.Time, 1)
	go func() { done <- s.Next(from) }()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("Next(%s) did not return", from.Format(stamp))
		return time.Time{}
	}
}

// The clocks change differently around the world: forward over an hour at
// 2:00 in the United States, over the first hour of the day in Chile, and
// back at 3:00 in Germany. These are the days where Next used to loop for
// ever (the zones west of UTC) or run twice or not at all.
func TestNextWhenClocksChange(t *testing.T) {
	ny, berlin, chile := zone(t, "America/New_York"), zone(t, "Europe/Berlin"), zone(t, "America/Santiago")
	tests := []struct {
		loc            *time.Location
		expr, from, at string
	}{
		// The clocks skip 2:00 to 3:00 on 2026-03-08 in New York.
		{ny, "0 3 * * *", "2026-03-07 12:00 -0500", "2026-03-08 03:00 -0400"},
		{ny, "0 4 * * *", "2026-03-07 12:00 -0500", "2026-03-08 04:00 -0400"},
		{ny, "0 3 * * 0", "2026-03-02 12:00 -0500", "2026-03-08 03:00 -0400"},
		{ny, "0 12 * * *", "2026-03-07 13:00 -0500", "2026-03-08 12:00 -0400"},
		{ny, "*/30 * * * *", "2026-03-08 01:30 -0500", "2026-03-08 03:00 -0400"},
		// A time the clocks skipped runs when they come back.
		{ny, "30 2 * * *", "2026-03-07 12:00 -0500", "2026-03-08 03:00 -0400"},
		{ny, "30 2 * * *", "2026-03-08 03:00 -0400", "2026-03-09 02:30 -0400"},
		// Midnight is skipped in Chile on 2026-09-06.
		{chile, "0 0 * * 0", "2026-09-01 12:00 -0400", "2026-09-06 01:00 -0300"},
		{chile, "0 3 * * 0", "2026-09-01 12:00 -0400", "2026-09-06 03:00 -0300"},
		// Germany skips 2:00 to 3:00 on 2026-03-29.
		{berlin, "30 2 * * *", "2026-03-28 12:00 +0100", "2026-03-29 03:00 +0200"},
		{berlin, "0 3 * * *", "2026-03-28 12:00 +0100", "2026-03-29 03:00 +0200"},
		// The clocks show 2:00 to 3:00 twice on 2026-10-25. A schedule fixed
		// to an hour runs the first time only; one that runs every hour,
		// every time.
		{berlin, "30 2 * * *", "2026-10-24 12:00 +0200", "2026-10-25 02:30 +0200"},
		{berlin, "30 2 * * *", "2026-10-25 02:30 +0200", "2026-10-26 02:30 +0100"},
		{berlin, "30 2 * * *", "2026-10-25 02:00 +0100", "2026-10-26 02:30 +0100"},
		{berlin, "0 2,3 * * *", "2026-10-25 02:00 +0200", "2026-10-25 03:00 +0100"},
		{berlin, "30 * * * *", "2026-10-25 02:30 +0200", "2026-10-25 02:30 +0100"},
		{berlin, "*/20 * * * *", "2026-10-25 02:50 +0200", "2026-10-25 02:00 +0100"},
		{ny, "30 1 * * *", "2026-10-31 12:00 -0400", "2026-11-01 01:30 -0400"},
		{ny, "30 1 * * *", "2026-11-01 01:30 -0400", "2026-11-02 01:30 -0500"},
	}
	for _, tt := range tests {
		s, err := Parse(tt.expr)
		if err != nil {
			t.Fatal(err)
		}
		from := in(t, tt.loc, tt.from)
		if got := next(t, s, from); got.Format(stamp) != tt.at {
			t.Errorf("%q in %s after %s: got %s, want %s", tt.expr, tt.loc, tt.from, got.Format(stamp), tt.at)
		}
	}
}

// The panel claims a slot for the minute Last finds, so it must find a
// fixed time once on the day the clocks go back, and after the change on the
// day they skip it.
func TestLastWhenClocksChange(t *testing.T) {
	berlin := zone(t, "Europe/Berlin")
	tests := []struct {
		expr, now, want string // want is empty when nothing is due
	}{
		{"30 2 * * *", "2026-10-25 02:32 +0200", "2026-10-25 02:30 +0200"},
		// An hour later the clocks show 2:30 again; the schedule ran already.
		{"30 2 * * *", "2026-10-25 02:32 +0100", ""},
		{"30 * * * *", "2026-10-25 02:32 +0100", "2026-10-25 02:30 +0100"},
		{"30 2 * * *", "2026-03-29 03:02 +0200", "2026-03-29 03:00 +0200"},
		{"30 2 * * *", "2026-03-29 03:06 +0200", ""},
		{"30 2 * * *", "2026-03-29 01:59 +0100", ""},
		// Not due in the skipped hour.
		{"30 4 * * *", "2026-03-29 03:02 +0200", ""},
	}
	for _, tt := range tests {
		s, err := Parse(tt.expr)
		if err != nil {
			t.Fatal(err)
		}
		now := in(t, berlin, tt.now)
		got := ""
		if l := s.Last(now, 5*time.Minute); !l.IsZero() {
			got = l.Format(stamp)
		}
		if got != tt.want {
			t.Errorf("%q at %s: got %q, want %q", tt.expr, tt.now, got, tt.want)
		}
	}
}

// Next must agree with Last, minute by minute, wherever the clocks change,
// and must always return.
func TestNextAgreesWithLast(t *testing.T) {
	exprs := []string{
		"0 3 * * *", "30 2 * * *", "0 2 * * *", "15 1 * * *", "45 23 * * *", "0 0 * * *", "0 1,2,3 * * *",
		"30 * * * *", "*/20 * * * *", "0 */2 * * *", "0 0 * * 0", "0 3 * * 0", "0 2 1-7 * 0",
	}
	zones := []string{
		"America/New_York", "America/Chicago", "America/Santiago", "America/Havana", "Europe/Berlin", "Europe/Dublin",
		"Africa/Cairo", "Africa/Casablanca", "Asia/Beirut", "Australia/Sydney", "Australia/Lord_Howe", "Pacific/Chatham",
		"Asia/Kolkata", "UTC",
	}
	for _, name := range zones {
		loc := zone(t, name)
		// Every change of the clocks in 2026, found by looking at each hour.
		var changes []time.Time
		prev := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).In(loc)
		for h := 1; h < 24*365; h++ {
			cur := time.Date(2026, 1, 1, h, 0, 0, 0, time.UTC).In(loc)
			_, before := prev.Zone()
			_, after := cur.Zone()
			if before != after {
				changes = append(changes, cur)
			}
			prev = cur
		}
		for _, expr := range exprs {
			s, err := Parse(expr)
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range changes {
				for from := change.Add(-30 * time.Hour); from.Before(change.Add(6 * time.Hour)); from = from.Add(time.Hour) {
					want := time.Time{}
					for m := from.Truncate(time.Minute).Add(time.Minute); m.Before(from.Add(10 * 24 * time.Hour)); m = m.Add(time.Minute) {
						if s.fires(m) {
							want = m
							break
						}
					}
					if got := next(t, s, from); !got.Equal(want) {
						t.Fatalf("%q in %s after %s: Next gave %s, the minutes say %s", expr, name, from.Format(stamp), got.Format(stamp), want.Format(stamp))
					}
				}
			}
		}
	}
}
