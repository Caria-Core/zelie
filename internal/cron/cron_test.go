package cron

import (
	"errors"
	"testing"
	"time"
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
