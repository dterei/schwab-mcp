package nyse

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// yearOf returns the calendar's closures and early closes in year: the
// weekdays without a session and the sessions that close early, as
// month-day strings such as "07-03", and the number of sessions.
func yearOf(t *testing.T, year int) (closed, early []string, sessions int) {
	t.Helper()
	ss, err := Between(nytime.Date(fmt.Sprintf("%d-01-01", year)), nytime.Date(fmt.Sprintf("%d-12-31", year)))
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]bool{}
	for _, s := range ss {
		md := string(s.Date[5:])
		open[md] = true
		if s.Early {
			early = append(early, md)
		}
	}
	for day := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC); day.Year() == year; day = day.AddDate(0, 0, 1) {
		if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday && !open[day.Format("01-02")] {
			closed = append(closed, day.Format("01-02"))
		}
	}
	return closed, early, len(ss)
}

// The calendar's closures and early closes in 2024-2028 are exactly those
// of NYSE's published calendars, none more and none fewer, and so are its
// numbers of sessions. The sources are NYSE's releases of 2021-12-27
// (2022-2024), 2024-11-08 (2025-2027) and 2025-12-23 (2026-2028), and its
// announcement of the closure on 2025-01-09.
func TestPublishedCalendars(t *testing.T) {
	for _, tt := range []struct {
		year          int
		closed, early []string
		sessions      int
	}{
		{
			2024,
			[]string{"01-01", "01-15", "02-19", "03-29", "05-27", "06-19", "07-04", "09-02", "11-28", "12-25"},
			[]string{"07-03", "11-29", "12-24"},
			252,
		},
		{
			// Thursday 2025-01-09 for President Carter; Friday 2025-12-26 is a
			// full session, though federal offices closed.
			2025,
			[]string{"01-01", "01-09", "01-20", "02-17", "04-18", "05-26", "06-19", "07-04", "09-01", "11-27", "12-25"},
			[]string{"07-03", "11-28", "12-24"},
			250,
		},
		{
			// July 4 is a Saturday: Friday July 3 is closed and Thursday July
			// 2 a full session.
			2026,
			[]string{"01-01", "01-19", "02-16", "04-03", "05-25", "06-19", "07-03", "09-07", "11-26", "12-25"},
			[]string{"11-27", "12-24"},
			251,
		},
		{
			// June 19 and December 25 are Saturdays and July 4 a Sunday; the
			// day after Thanksgiving is the only early close.
			2027,
			[]string{"01-01", "01-18", "02-15", "03-26", "05-31", "06-18", "07-05", "09-06", "11-25", "12-24"},
			[]string{"11-26"},
			251,
		},
		{
			// New Year's Day is a Saturday, and no holiday is observed for it.
			2028,
			[]string{"01-17", "02-21", "04-14", "05-29", "06-19", "07-04", "09-04", "11-23", "12-25"},
			[]string{"07-03", "11-24"},
			251,
		},
	} {
		closed, early, sessions := yearOf(t, tt.year)
		if !slices.Equal(closed, tt.closed) {
			t.Errorf("%d: closed on %v, NYSE published %v", tt.year, closed, tt.closed)
		}
		if !slices.Equal(early, tt.early) {
			t.Errorf("%d: early closes on %v, NYSE published %v", tt.year, early, tt.early)
		}
		if sessions != tt.sessions {
			t.Errorf("%d: %d sessions, want %d", tt.year, sessions, tt.sessions)
		}
	}
}

// The calendar agrees with NYSE's own history. For 2000-2010 its early
// closes are exactly the 25 that NYSE's "Special Closings" (revised through
// January 2011) lists, and its closures exactly those of
// exchange_calendars 4.13.2, whose special ones are the six days NYSE's
// list gives. Its numbers of sessions for 2000-2022 are exchange_calendars'.
// exchange_calendars generates its data from its own rules, so it is a
// second reading of the rules, which NYSE's list confirms to 2010.
func TestNYSEHistory(t *testing.T) {
	closed := map[int][]string{
		2000: {"01-17", "02-21", "04-21", "05-29", "07-04", "09-04", "11-23", "12-25"},
		2001: {"01-01", "01-15", "02-19", "04-13", "05-28", "07-04", "09-03", "09-11", "09-12", "09-13", "09-14", "11-22", "12-25"},
		2002: {"01-01", "01-21", "02-18", "03-29", "05-27", "07-04", "09-02", "11-28", "12-25"},
		2003: {"01-01", "01-20", "02-17", "04-18", "05-26", "07-04", "09-01", "11-27", "12-25"},
		2004: {"01-01", "01-19", "02-16", "04-09", "05-31", "06-11", "07-05", "09-06", "11-25", "12-24"},
		2005: {"01-17", "02-21", "03-25", "05-30", "07-04", "09-05", "11-24", "12-26"},
		2006: {"01-02", "01-16", "02-20", "04-14", "05-29", "07-04", "09-04", "11-23", "12-25"},
		2007: {"01-01", "01-02", "01-15", "02-19", "04-06", "05-28", "07-04", "09-03", "11-22", "12-25"},
		2008: {"01-01", "01-21", "02-18", "03-21", "05-26", "07-04", "09-01", "11-27", "12-25"},
		2009: {"01-01", "01-19", "02-16", "04-10", "05-25", "07-03", "09-07", "11-26", "12-25"},
		2010: {"01-01", "01-18", "02-15", "04-02", "05-31", "07-05", "09-06", "11-25", "12-24"},
	}
	early := map[int][]string{
		2000: {"07-03", "11-24"},
		2001: {"07-03", "11-23", "12-24"},
		2002: {"07-05", "11-29", "12-24"},
		2003: {"07-03", "11-28", "12-24", "12-26"},
		2004: {"11-26"},
		2005: {"11-25"},
		2006: {"07-03", "11-24"},
		2007: {"07-03", "11-23", "12-24"},
		2008: {"07-03", "11-28", "12-24"},
		2009: {"11-27", "12-24"},
		2010: {"11-26"},
	}
	sessions := map[int]int{
		2000: 252, 2001: 248, 2002: 252, 2003: 252, 2004: 252, 2005: 252, 2006: 251, 2007: 251,
		2008: 253, 2009: 252, 2010: 252, 2011: 252, 2012: 250, 2013: 252, 2014: 252, 2015: 252,
		2016: 252, 2017: 251, 2018: 251, 2019: 252, 2020: 253, 2021: 252, 2022: 251,
	}
	earlyCount := 0
	for year := 2000; year <= 2022; year++ {
		c, e, n := yearOf(t, year)
		if want, ok := closed[year]; ok && !slices.Equal(c, want) {
			t.Errorf("%d: closed on %v, NYSE's history says %v", year, c, want)
		}
		if want, ok := early[year]; ok {
			earlyCount += len(want)
			if !slices.Equal(e, want) {
				t.Errorf("%d: early closes on %v, NYSE's history says %v", year, e, want)
			}
		}
		if n != sessions[year] {
			t.Errorf("%d: %d sessions, want %d", year, n, sessions[year])
		}
	}
	if earlyCount != 25 {
		t.Errorf("the table holds %d early closes for 2000-2010, and NYSE's list 25", earlyCount)
	}

	// The closures since 2010, and the days around them.
	for d, want := range map[nytime.Date]bool{
		"2012-10-26": true, "2012-10-29": false, "2012-10-30": false, "2012-10-31": true, // Hurricane Sandy
		"2018-11-30": true, "2018-12-04": true, "2018-12-05": false, "2018-12-06": true, // President Bush
		"2025-01-08": true, "2025-01-09": false, "2025-01-10": true, // President Carter
		"2001-09-10": true, "2001-09-17": true, // around September 11
	} {
		if _, ok := mustOn(t, d); ok != want {
			t.Errorf("a session on %s: %v, want %v", d, ok, want)
		}
	}
	// The one early close outside the rules, and the irregular closes that
	// the calendar leaves at 16:00.
	for d, want := range map[nytime.Date]int{"2003-12-26": 13, "2005-06-01": 16, "2009-07-02": 16} {
		if s, ok := mustOn(t, d); !ok || s.Close.Hour() != want || s.Early != (want == 13) {
			t.Errorf("On(%s) = %+v, %v; want a close at %d:00", d, s, ok, want)
		}
	}
}

// mustOn returns On(d), failing the test on an error.
func mustOn(t *testing.T, d nytime.Date) (Session, bool) {
	t.Helper()
	s, ok, err := On(d)
	if err != nil {
		t.Fatal(err)
	}
	return s, ok
}

// Each case of a holiday on a weekend, and each early close's weekdays:
// New Year's Day on a Sunday closes the Monday, and on a Saturday closes
// nothing; July 4, June 19 and December 25 move to the Friday before a
// Saturday and the Monday after a Sunday; July 3 closes early on a
// Wednesday from 2013, and before it July 5 did on a Friday instead; a
// Friday after a Thursday Christmas is a full session.
func TestWeekendCases(t *testing.T) {
	const (
		closed = iota
		full
		early
	)
	cases := map[nytime.Date]int{
		// New Year's Day on a Sunday.
		"2006-01-02": closed, "2012-01-02": closed, "2017-01-02": closed, "2023-01-02": closed, "2034-01-02": closed,
		// New Year's Day on a Saturday: December 31 ends the year.
		"2004-12-31": full, "2010-12-31": full, "2021-12-31": full, "2027-12-31": full,
		// July 4 on a Saturday: Friday closed, Thursday full.
		"2009-07-03": closed, "2015-07-03": closed, "2020-07-03": closed, "2026-07-03": closed,
		"2009-07-02": full, "2015-07-02": full, "2020-07-02": full, "2026-07-02": full,
		// July 4 on a Sunday: Monday closed, Friday full.
		"2004-07-05": closed, "2010-07-05": closed, "2021-07-05": closed, "2027-07-05": closed,
		"2004-07-02": full, "2010-07-02": full, "2021-07-02": full, "2027-07-02": full,
		// Christmas on a Saturday: Friday December 24 closed, Thursday full.
		"2004-12-24": closed, "2010-12-24": closed, "2021-12-24": closed, "2027-12-24": closed,
		"2004-12-23": full, "2010-12-23": full, "2021-12-23": full, "2027-12-23": full,
		// Christmas on a Sunday: Monday December 26 closed, Friday full.
		"2005-12-26": closed, "2011-12-26": closed, "2016-12-26": closed, "2022-12-26": closed,
		"2005-12-23": full, "2011-12-23": full, "2016-12-23": full, "2022-12-23": full,
		// Juneteenth from 2022 only, and on a Saturday.
		"2021-06-18": full, "2022-06-20": closed, "2027-06-18": closed,
		// July 3 on a Wednesday, before 2013 and from it.
		"2002-07-03": full, "2002-07-05": early,
		"2013-07-03": early, "2019-07-03": early, "2024-07-03": early, "2013-07-05": full,
		"2028-07-03": early,
		// The Friday after a Thursday Christmas.
		"2008-12-26": full, "2014-12-26": full, "2025-12-26": full,
		// President Bush died on 2018-11-30; NYSE closed on 2018-12-05.
		"2018-11-30": full, "2018-12-05": closed,
	}
	for _, d := range slices.Sorted(maps.Keys(cases)) {
		s, ok := mustOn(t, d)
		got := closed
		switch {
		case ok && s.Early:
			got = early
		case ok:
			got = full
		}
		if got != cases[d] {
			t.Errorf("%s is %s, want %s", d, []string{"closed", "a full session", "an early close"}[got],
				[]string{"closed", "a full session", "an early close"}[cases[d]])
		}
	}
}

// What holds of every year from 2000 to 2100: no session on a weekend; a
// session opens at 09:30 and closes at 16:00, or at 13:00 when early; the
// rules close 9 weekdays a year before 2022 and 10 from it, one fewer when
// New Year's Day is a Saturday; a year has 248 to 253 sessions; and
// LastClosed and During find each session at its close and its open, and
// the one before it a second before its close.
func TestProperties(t *testing.T) {
	var prev Session
	for year := 2000; year <= 2100; year++ {
		ss, err := Between(nytime.Date(fmt.Sprintf("%d-01-01", year)), nytime.Date(fmt.Sprintf("%d-12-31", year)))
		if err != nil {
			t.Fatal(err)
		}
		if n := len(ss); n < 248 || n > 253 {
			t.Errorf("%d has %d sessions", year, n)
		}
		weekdays := 0
		for day := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC); day.Year() == year; day = day.AddDate(0, 0, 1) {
			if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
				weekdays++
			}
		}
		for d := range closures {
			if strings.HasPrefix(string(d), strconv.Itoa(year)) {
				weekdays--
			}
		}
		want := 9
		if year >= 2022 {
			want = 10
		}
		if time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Weekday() == time.Saturday {
			want--
		}
		if held := weekdays - len(ss); held != want {
			t.Errorf("the rules close %d weekdays in %d, want %d", held, year, want)
		}
		for _, s := range ss {
			ny := s.Open.In(nytime.NewYork())
			if wd := ny.Weekday(); wd == time.Saturday || wd == time.Sunday {
				t.Errorf("a session on %s, a %s", s.Date, wd)
			}
			if nytime.DateOf(s.Open) != s.Date || nytime.DateOf(s.Close) != s.Date {
				t.Errorf("the session of %s opens at %v and closes at %v", s.Date, s.Open, s.Close)
			}
			wantClose := "16:00"
			if s.Early {
				wantClose = "13:00"
			}
			if open, cl := ny.Format("15:04:05"), s.Close.In(nytime.NewYork()).Format("15:04"); open != "09:30:00" || cl != wantClose {
				t.Errorf("the session of %s is %s to %s", s.Date, open, cl)
			}
			if got, err := LastClosed(s.Close); err != nil || got != s {
				t.Errorf("LastClosed(%v) = %+v, %v; want %s", s.Close, got, err, s.Date)
			}
			if got, err := LastClosed(s.Close.Add(-time.Second)); prev.Date != "" && (err != nil || got != prev) {
				t.Errorf("LastClosed(%v) = %+v, %v; want %s", s.Close.Add(-time.Second), got, err, prev.Date)
			}
			if got, ok := During(s.Open); !ok || got != s {
				t.Errorf("During(%v) = %+v, %v; want %s", s.Open, got, ok, s.Date)
			}
			if _, ok := During(s.Open.Add(-time.Second)); ok {
				t.Errorf("a session is in progress a second before %s's open", s.Date)
			}
			if _, ok := During(s.Close); ok {
				t.Errorf("a session is in progress at %s's close", s.Date)
			}
			prev = s
		}
	}
}

// Good Friday is two days before Easter, which easter computes as Gauss's
// algorithm does, written apart from it, for every year from 1900 to 2200;
// and the calendar is closed on each Good Friday from 2000 to 2100.
func TestEaster(t *testing.T) {
	for year := 1900; year <= 2200; year++ {
		if got, want := easter(year), gaussEaster(year); !got.Equal(want) {
			t.Errorf("Easter %d: %s, and by Gauss's algorithm %s", year, got.Format(time.DateOnly), want.Format(time.DateOnly))
		}
		if year < 2000 || year > 2100 {
			continue
		}
		goodFriday := nytime.Date(gaussEaster(year).AddDate(0, 0, -2).Format(time.DateOnly))
		if _, ok := mustOn(t, goodFriday); ok {
			t.Errorf("a session on Good Friday %s", goodFriday)
		}
	}
	for year, want := range map[int]string{2025: "2025-04-20", 2026: "2026-04-05", 2027: "2027-03-28", 2028: "2028-04-16"} {
		if got := easter(year).Format(time.DateOnly); got != want {
			t.Errorf("Easter %d is %s, want %s", year, got, want)
		}
	}
}

// gaussEaster returns Easter Sunday of year in the Gregorian calendar by
// Gauss's algorithm, with its two exceptions.
func gaussEaster(year int) time.Time {
	a, b, c := year%19, year%4, year%7
	k := year / 100
	p := (13 + 8*k) / 25
	q := k / 4
	m := (15 - p + k - q) % 30
	n := (4 + k - q) % 7
	d := (19*a + m) % 30
	e := (2*b + 4*c + 6*d + n) % 7
	if d == 29 && e == 6 {
		return time.Date(year, time.April, 19, 0, 0, 0, 0, time.UTC)
	}
	if d == 28 && e == 6 && (11*m+11)%30 < 19 {
		return time.Date(year, time.April, 18, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(year, time.March, 22+d+e, 0, 0, 0, 0, time.UTC)
}

// The session that a snapshot stands for, and the one in progress when it
// was taken, in the worked examples of the calendar's research note, given
// in New York's time (ET) or Los Angeles' (PT), where the scheduled run
// takes its snapshot at 21:05: the close is inclusive; a snapshot during a
// session, an early close's included, stands for the session before; a run
// after New York's midnight stands for the day before; weekends, holidays,
// the year's end, both changes of the clocks and the closure for President
// Carter.
func TestStandsFor(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skip("no time zone database for Los Angeles:", err)
	}
	ny := nytime.NewYork()
	at := func(loc *time.Location, s string) time.Time {
		t.Helper()
		v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tt := range []struct {
		at        time.Time
		standsFor nytime.Date
		during    nytime.Date // the session in progress, if any
	}{
		{at(ny, "2026-11-10 15:45:00"), "2026-11-09", "2026-11-10"},
		{at(ny, "2026-11-10 15:59:59"), "2026-11-09", "2026-11-10"},
		{at(ny, "2026-11-10 16:00:00"), "2026-11-10", ""},
		{at(ny, "2026-11-10 16:30:00"), "2026-11-10", ""},
		{at(la, "2026-11-10 21:05:00"), "2026-11-10", ""}, // Wednesday 00:05 in New York
		{at(ny, "2026-11-09 09:00:00"), "2026-11-06", ""}, // before Monday's open
		{at(ny, "2026-11-11 16:30:00"), "2026-11-11", ""}, // Veterans Day is a session
		{at(ny, "2026-11-26 16:30:00"), "2026-11-25", ""}, // Thanksgiving
		{at(la, "2026-11-26 21:05:00"), "2026-11-25", ""},
		{at(ny, "2026-11-27 12:59:00"), "2026-11-25", "2026-11-27"}, // during an early close
		{at(ny, "2026-11-27 13:00:00"), "2026-11-27", ""},
		{at(ny, "2026-11-27 15:45:00"), "2026-11-27", ""}, // a close at 16:00 would say 11-25
		{at(la, "2026-11-27 21:05:00"), "2026-11-27", ""},
		{at(la, "2026-11-28 21:05:00"), "2026-11-27", ""}, // Saturday's run
		{at(la, "2026-11-29 21:05:00"), "2026-11-27", ""}, // Sunday's run
		{at(ny, "2026-12-24 15:45:00"), "2026-12-24", ""}, // Christmas Eve closes at 13:00
		{at(la, "2026-12-24 21:05:00"), "2026-12-24", ""},
		{at(la, "2026-12-31 21:05:00"), "2026-12-31", ""}, // 2027-01-01 00:05 in New York
		{at(la, "2027-01-01 21:05:00"), "2026-12-31", ""},
		{at(ny, "2027-03-26 16:30:00"), "2027-03-25", ""}, // Good Friday
		{at(la, "2026-07-02 21:05:00"), "2026-07-02", ""}, // July 3 is the holiday; July 2 a full session
		{at(ny, "2028-07-03 15:45:00"), "2028-07-03", ""}, // an early close
		{at(la, "2027-12-31 21:05:00"), "2027-12-31", ""}, // New Year's Day on a Saturday
		{at(la, "2026-03-08 21:05:00"), "2026-03-06", ""}, // the clocks went forward that morning
		{at(la, "2026-11-01 21:05:00"), "2026-10-30", ""}, // the clocks went back that morning
		{at(ny, "2025-01-09 16:30:00"), "2025-01-08", ""}, // closed for President Carter
	} {
		s, err := LastClosed(tt.at)
		if err != nil || s.Date != tt.standsFor {
			t.Errorf("LastClosed(%v) = %s, %v; want %s", tt.at, s.Date, err, tt.standsFor)
		}
		if s, ok := During(tt.at); s.Date != tt.during || ok != (tt.during != "") {
			t.Errorf("During(%v) = %s, %v; want %q", tt.at, s.Date, ok, tt.during)
		}
	}
}

// One instant gives one answer in whatever zone it is written: UTC, New
// York, Los Angeles or Tokyo, where it is often on another date.
func TestZoneIndependence(t *testing.T) {
	var zones []*time.Location
	for _, name := range []string{"UTC", "America/New_York", "America/Los_Angeles", "Asia/Tokyo"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skip("no time zone database:", err)
		}
		zones = append(zones, loc)
	}
	for _, s := range []string{
		"2026-11-27T18:00:00Z", // the early close, 03:00 the next day in Tokyo
		"2026-11-27T17:59:59Z",
		"2026-11-10T21:00:00Z", // 16:00 in New York in winter
		"2026-07-02T20:00:00Z", // 16:00 in New York in summer
		"2026-07-02T19:59:59Z",
		"2026-03-09T04:05:00Z", // Sunday 21:05 in Los Angeles, after the clocks changed
		"2027-01-01T05:05:00Z",
		"2025-01-09T21:30:00Z",
	} {
		base, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		wantClosed, err := LastClosed(base)
		if err != nil {
			t.Fatal(err)
		}
		wantDuring, wantOK := During(base)
		for _, loc := range zones {
			at := base.In(loc)
			if got, err := LastClosed(at); err != nil || got != wantClosed {
				t.Errorf("LastClosed(%v) = %s, %v; in UTC %s", at, got.Date, err, wantClosed.Date)
			}
			if got, ok := During(at); got != wantDuring || ok != wantOK {
				t.Errorf("During(%v) = %s, %v; in UTC %s, %v", at, got.Date, ok, wantDuring.Date, wantOK)
			}
		}
	}
}

// Dates before 2000 are refused, with ErrBeforeFirst, as are times before
// the first session's close, 2000-01-03 at 16:00; what is not a date is
// refused too. The calendar answers from its first date on.
func TestBeforeFirst(t *testing.T) {
	ny := nytime.NewYork()
	if _, _, err := On("1999-12-31"); !errors.Is(err, ErrBeforeFirst) {
		t.Errorf("On(1999-12-31): %v", err)
	}
	if _, ok, err := On(First); ok || err != nil {
		t.Errorf("On(%s), a Saturday: %v, %v", First, ok, err)
	}
	if s, ok, err := On("2000-01-03"); !ok || err != nil || s.Close != time.Date(2000, 1, 3, 16, 0, 0, 0, ny) {
		t.Errorf("On(2000-01-03) = %+v, %v, %v", s, ok, err)
	}
	for _, d := range []nytime.Date{"", "2026-02-30", "2026-1-05", "05/01/2026"} {
		if _, _, err := On(d); err == nil || errors.Is(err, ErrBeforeFirst) {
			t.Errorf("On(%q): %v", d, err)
		}
		if _, err := Between("2026-01-01", d); err == nil {
			t.Errorf("Between(2026-01-01, %q) did not fail", d)
		}
	}
	firstClose := time.Date(2000, 1, 3, 16, 0, 0, 0, ny)
	for _, at := range []time.Time{firstClose.Add(-time.Second), time.Date(2000, 1, 1, 0, 0, 0, 0, ny), time.Date(1999, 12, 31, 16, 0, 0, 0, ny), {}} {
		s, err := LastClosed(at)
		if !errors.Is(err, ErrBeforeFirst) {
			t.Errorf("LastClosed(%v) = %+v, %v", at, s, err)
			continue
		}
		// The time may be on or after First, so the error says when the
		// first session closed, not that the time is before the calendar.
		want := "no NYSE session closed by " + at.UTC().Format(time.RFC3339) + ": the first closed on 2000-01-03 at 16:00 New York time"
		if err.Error() != want {
			t.Errorf("LastClosed(%v): %q, want %q", at, err, want)
		}
	}
	if s, err := LastClosed(firstClose); err != nil || s.Date != "2000-01-03" {
		t.Errorf("LastClosed(%v) = %+v, %v", firstClose, s, err)
	}
	if _, ok := During(time.Date(1999, 12, 31, 10, 0, 0, 0, ny)); ok {
		t.Error("a session in progress on 1999-12-31")
	}
	if s, ok := During(time.Date(2000, 1, 3, 10, 0, 0, 0, ny)); !ok || s.Date != "2000-01-03" {
		t.Errorf("During 2000-01-03 10:00: %+v, %v", s, ok)
	}
	if _, err := Between("1999-12-30", "2000-01-05"); !errors.Is(err, ErrBeforeFirst) {
		t.Errorf("Between(1999-12-30, 2000-01-05): %v", err)
	}
	ss, err := Between(First, "2000-01-05")
	if err != nil || len(ss) != 3 || ss[0].Date != "2000-01-03" || ss[2].Date != "2000-01-05" {
		t.Errorf("Between(%s, 2000-01-05) = %+v, %v", First, ss, err)
	}
	if ss, err := Between("2026-11-10", "2026-11-09"); err != nil || len(ss) != 0 {
		t.Errorf("Between a date and the day before = %+v, %v", ss, err)
	}
}

// calendarDigests holds, for each Version, the digest of every session from
// 2000 to 2100, open and close, and of VerifiedThrough.
var calendarDigests = map[int]string{
	1: "075a121cba676e8d9a215d9a33ae622f235138ffed9dc82eec5d4a4008d1c8d6",
}

// A change to the rules, the tables or VerifiedThrough comes with a new
// Version, so that stores work out their market dates again: the calendar's
// digest is the one recorded for Version.
func TestCalendarVersion(t *testing.T) {
	ss, err := Between(First, "2100-12-31")
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	fmt.Fprintf(h, "verified through %d\n", VerifiedThrough)
	for _, s := range ss {
		fmt.Fprintf(h, "%s %s %s\n", s.Date, s.Open.UTC().Format(time.RFC3339), s.Close.UTC().Format(time.RFC3339))
	}
	got := hex.EncodeToString(h.Sum(nil))
	want, ok := calendarDigests[Version]
	switch {
	case !ok:
		t.Fatalf("no digest is recorded for Version %d: record this calendar's, %s", Version, got)
	case got != want && slices.Contains(slices.Collect(maps.Values(calendarDigests)), got):
		t.Fatalf("the calendar is that of an older Version, not of Version %d", Version)
	case got != want:
		t.Fatalf("the calendar changed: set Version to %d and record its digest, %s, so that stores work out their market dates again",
			slices.Max(slices.Collect(maps.Keys(calendarDigests)))+1, got)
	}
}
