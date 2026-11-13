package nytime

import (
	"testing"
	"time"
)

// DateOf gives the date in New York, whatever the instant's zone: at
// midnight on both sides of each change of the clocks, in the hour that
// happens twice when they go back, in the evening, when UTC is on the next
// date, and "" for the zero time.
func TestDateOf(t *testing.T) {
	for _, tt := range []struct {
		at   time.Time
		want Date
	}{
		{time.Time{}, ""},
		// Midnight in New York, and a second before it, around the changes
		// of 2026: 2026-03-08 and 2026-11-01.
		{time.Date(2026, 3, 8, 4, 59, 59, 0, time.UTC), "2026-03-07"},
		{time.Date(2026, 3, 8, 5, 0, 0, 0, time.UTC), "2026-03-08"},
		{time.Date(2026, 3, 9, 3, 59, 59, 0, time.UTC), "2026-03-08"},
		{time.Date(2026, 3, 9, 4, 0, 0, 0, time.UTC), "2026-03-09"},
		{time.Date(2026, 11, 1, 3, 59, 59, 0, time.UTC), "2026-10-31"},
		{time.Date(2026, 11, 1, 4, 0, 0, 0, time.UTC), "2026-11-01"},
		{time.Date(2026, 11, 2, 4, 59, 59, 0, time.UTC), "2026-11-01"},
		{time.Date(2026, 11, 2, 5, 0, 0, 0, time.UTC), "2026-11-02"},
		// The hour that happens twice, both times, and the hour that is
		// skipped.
		{time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), "2026-11-01"},
		{time.Date(2026, 11, 1, 6, 30, 0, 0, time.UTC), "2026-11-01"},
		{time.Date(2026, 3, 8, 7, 30, 0, 0, time.UTC), "2026-03-08"},
		// A close at 16:00 is on its date, which in UTC it is too; 20:30 in
		// New York is the next date in UTC.
		{time.Date(2026, 7, 2, 20, 0, 0, 0, time.UTC), "2026-07-02"},
		{time.Date(2026, 7, 3, 0, 30, 0, 0, time.UTC), "2026-07-02"},
		// An instant in another zone is converted first: 00:05 in Los
		// Angeles is 03:05 in New York, and 09:00 in Tokyo the evening
		// before.
		{time.Date(2026, 12, 25, 0, 5, 0, 0, time.FixedZone("PST", -8*3600)), "2026-12-25"},
		{time.Date(2026, 12, 24, 21, 5, 0, 0, time.FixedZone("PST", -8*3600)), "2026-12-25"},
		{time.Date(2026, 3, 28, 9, 0, 0, 0, time.FixedZone("JST", 9*3600)), "2026-03-27"},
		// The year changes at New York's midnight.
		{time.Date(2027, 1, 1, 4, 59, 59, 0, time.UTC), "2026-12-31"},
		{time.Date(2027, 1, 1, 5, 0, 0, 0, time.UTC), "2027-01-01"},
	} {
		if got := DateOf(tt.at); got != tt.want {
			t.Errorf("DateOf(%v) = %q, want %q", tt.at, got, tt.want)
		}
	}
}

// NewYork is New York's zone: its name, and its offsets in winter and in
// summer.
func TestNewYork(t *testing.T) {
	loc := NewYork()
	if loc.String() != "America/New_York" {
		t.Errorf("NewYork() is %q", loc)
	}
	for at, want := range map[time.Time]int{
		time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC): -5 * 3600,
		time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC): -4 * 3600,
	} {
		if _, offset := at.In(loc).Zone(); offset != want {
			t.Errorf("at %v New York is %+ds from UTC, want %+ds", at, offset, want)
		}
	}
}

// The zone built in has the system's changes of the clocks, each at the
// same instant, with the same names and offsets, from 1970 to 2100, where
// the system has a zone. When it stops agreeing, the tz database has
// changed New York's rules, and newyork.tzif needs to be taken again from
// Go's lib/time/zoneinfo.zip.
func TestBuiltInZoneMatchesTheSystems(t *testing.T) {
	sys, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no system time zone database:", err)
	}
	built, err := time.LoadLocationFromTZData("America/New_York", TZData())
	if err != nil {
		t.Fatal(err)
	}
	// New York's clocks change at 02:00 local time, 06:00 or 07:00 in UTC,
	// so each day is compared on both sides of those two instants.
	changes := 0
	_, last := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).In(sys).Zone()
	for day := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC); day.Year() < 2100; day = day.AddDate(0, 0, 1) {
		for _, at := range []time.Time{
			day.Add(6*time.Hour - time.Second), day.Add(6 * time.Hour),
			day.Add(7*time.Hour - time.Second), day.Add(7 * time.Hour),
		} {
			sysName, sysOffset := at.In(sys).Zone()
			builtName, builtOffset := at.In(built).Zone()
			if sysName != builtName || sysOffset != builtOffset {
				t.Fatalf("at %v the built-in zone is %s, %+ds from UTC, and the system's %s, %+ds",
					at, builtName, builtOffset, sysName, sysOffset)
			}
			if sysOffset != last {
				changes++
				last = sysOffset
			}
		}
	}
	// Two changes a year, from 1970 to 2100.
	if changes < 2*(2100-1970) {
		t.Errorf("only %d changes of the clocks from 1970 to 2100", changes)
	}
}

// TZData returns a copy: changing it changes neither the zone built in nor
// what a later call returns.
func TestTZDataIsACopy(t *testing.T) {
	a := TZData()
	clear(a)
	if b := TZData(); len(b) == 0 || b[0] == 0 {
		t.Fatal("TZData returned the bytes it holds, which a caller changed")
	}
	if _, err := time.LoadLocationFromTZData("America/New_York", TZData()); err != nil {
		t.Fatal(err)
	}
}
