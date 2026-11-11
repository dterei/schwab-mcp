// Package nyse is the NYSE's calendar of regular sessions from 2000 on: which
// dates have a session, and when each opens and closes, in New York.
//
// The calendar is NYSE's rules and two short tables. The rules are Rule
// 7.2's ten holidays, with its moves of a holiday on a weekend, and the
// kinds of early close at 13:00 that NYSE schedules: the day after
// Thanksgiving, December 24, July 3, and before 2013 July 5. The tables
// hold what no rule gives: the days since 2000 on which NYSE closed for an
// event, and the one session it closed early beyond its rules, the Friday
// after Christmas 2003. The rules reproduce NYSE's own list of holidays,
// closings and early closes for 2000-2010 and its published calendars for
// 2024-2028.
//
// Past VerifiedThrough the calendar answers from the rules, which NYSE sets
// years ahead; only a closure for an event it has not yet announced is
// unknown there.
//
// The package does its date arithmetic on calendar dates, and makes an
// instant only for a session's open and close, in New York's zone. It never
// reads the machine's zone or clock.
package nyse

import (
	"errors"
	"fmt"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// Version is the calendar's version. It changes whenever a rule, an entry
// of a table or VerifiedThrough does, so that a store can recompute what it
// worked out with an older calendar.
const Version = 1

// First is the first date the calendar answers for. Before it NYSE's rules
// differed, and the calendar refuses rather than guess.
const First nytime.Date = "2000-01-01"

// VerifiedThrough is the last year checked against the calendar NYSE
// published for it. NYSE publishes three years ahead late in each year: its
// release of 2025-12-23 covers 2026-2028.
const VerifiedThrough = 2028

// ErrBeforeFirst is the error, as errors.Is reports it, for a date before
// First, or a time before the close of the calendar's first session.
var ErrBeforeFirst = errors.New("before the NYSE calendar, which starts on 2000-01-01")

// A Session is one regular trading session of the NYSE.
type Session struct {
	Date nytime.Date // the session's date in New York
	// Open is 09:30 New York time on Date, and Close 16:00, or 13:00 on an
	// early close.
	Open, Close time.Time
	Early       bool // whether the session closed early, at 13:00
}

// On returns the session on d, and whether there is one: none on a weekend,
// a holiday or a day NYSE closed. It fails for a date that is not one, or
// that is before First.
func On(d nytime.Date) (s Session, ok bool, err error) {
	day, err := parse(d)
	if err != nil {
		return Session{}, false, err
	}
	s, ok = sessionOn(day)
	return s, ok, nil
}

// LastClosed returns the session with the latest close at or before t: the
// session that a snapshot of the accounts taken at t stands for. At 16:00
// exactly it is that day's session; during a session, the one before it.
// It fails when t is before the close of the calendar's first session.
func LastClosed(t time.Time) (Session, error) {
	day := dayOf(t)
	for range 15 {
		if day.Before(first) {
			return Session{}, &beforeFirstClose{t}
		}
		if s, ok := sessionOn(day); ok && !s.Close.After(t) {
			return s, nil
		}
		day = day.AddDate(0, 0, -1)
	}
	// The longest run of days without a session since 2000 is six, from
	// 2001-09-11 to 2001-09-16, so this does not happen.
	return Session{}, fmt.Errorf("no NYSE session closed in the 14 days to %s", t.UTC().Format(time.RFC3339))
}

// During returns the session in progress at t, from its open up to its
// close, and whether there is one.
func During(t time.Time) (Session, bool) {
	day := dayOf(t)
	if day.Before(first) {
		return Session{}, false
	}
	s, ok := sessionOn(day)
	if !ok || t.Before(s.Open) || !t.Before(s.Close) {
		return Session{}, false
	}
	return s, true
}

// Between returns the sessions from the date from to the date to, both
// included, in order; none when to is before from. It fails for a date that
// is not one, or when from is before First.
func Between(from, to nytime.Date) ([]Session, error) {
	start, err := parse(from)
	if err != nil {
		return nil, err
	}
	end, err := time.Parse(time.DateOnly, string(to))
	if err != nil {
		return nil, fmt.Errorf("%q is not a date", to)
	}
	var out []Session
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		if s, ok := sessionOn(day); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

// beforeFirstClose is LastClosed's error for a time before the close of the
// calendar's first session, which may be on or after First.
type beforeFirstClose struct{ t time.Time }

func (e *beforeFirstClose) Error() string {
	return fmt.Sprintf("no NYSE session closed by %s: the first closed on 2000-01-03 at 16:00 New York time", e.t.UTC().Format(time.RFC3339))
}

func (e *beforeFirstClose) Is(target error) bool { return target == ErrBeforeFirst }

// first is First as a calendar date.
var first = time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)

// parse returns d as a calendar date: midnight in UTC, on which the
// package does its arithmetic.
func parse(d nytime.Date) (time.Time, error) {
	day, err := time.Parse(time.DateOnly, string(d))
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a date", d)
	}
	if day.Before(first) {
		return time.Time{}, fmt.Errorf("%s is %w", d, ErrBeforeFirst)
	}
	return day, nil
}

// dayOf returns t's date in New York, as a calendar date.
func dayOf(t time.Time) time.Time {
	y, m, d := t.In(nytime.NewYork()).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// sessionOn returns the session on day, a calendar date from First on, and
// whether there is one.
func sessionOn(day time.Time) (Session, bool) {
	if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return Session{}, false
	}
	date := nytime.Date(day.Format(time.DateOnly))
	if _, ok := closures[date]; ok {
		return Session{}, false
	}
	for _, h := range holidays(day.Year()) {
		if h.Equal(day) {
			return Session{}, false
		}
	}
	early := earlyClose(day)
	closeHour := 16
	if early {
		closeHour = 13
	}
	y, m, d := day.Date()
	return Session{
		Date:  date,
		Open:  time.Date(y, m, d, 9, 30, 0, 0, nytime.NewYork()),
		Close: time.Date(y, m, d, closeHour, 0, 0, 0, nytime.NewYork()),
		Early: early,
	}, true
}

// holidays returns the weekdays of year on which NYSE Rule 7.2 closes the
// exchange. A holiday on a Saturday closes the Friday before and one on a
// Sunday the Monday after, but for New Year's Day on a Saturday, which
// closes no day: the Friday before ends a monthly and the yearly accounting
// period, which the rule makes an exception for.
func holidays(year int) []time.Time {
	date := func(m time.Month, d int) time.Time {
		return time.Date(year, m, d, 0, 0, 0, 0, time.UTC)
	}
	hs := make([]time.Time, 0, 10)
	if newYear := date(time.January, 1); newYear.Weekday() != time.Saturday {
		hs = append(hs, observed(newYear))
	}
	hs = append(hs,
		nthWeekday(year, time.January, time.Monday, 3),  // Martin Luther King, Jr. Day
		nthWeekday(year, time.February, time.Monday, 3), // Washington's Birthday
		easter(year).AddDate(0, 0, -2),                  // Good Friday
		lastWeekday(year, time.May, time.Monday),        // Memorial Day
	)
	if year >= 2022 {
		hs = append(hs, observed(date(time.June, 19))) // Juneteenth National Independence Day
	}
	return append(hs,
		observed(date(time.July, 4)),                     // Independence Day
		nthWeekday(year, time.September, time.Monday, 1), // Labor Day
		thanksgiving(year),
		observed(date(time.December, 25)), // Christmas Day
	)
}

// earlyClose reports whether NYSE closes at 13:00 on day, a date with a
// session: the day after Thanksgiving; December 24 from Monday to
// Thursday; July 3 on a Monday, a Tuesday or a Thursday, and from 2013 on a
// Wednesday too; before 2013, July 5 on a Friday, when July 3 was a full
// session; and the days of the table.
func earlyClose(day time.Time) bool {
	if _, ok := earlyCloses[nytime.Date(day.Format(time.DateOnly))]; ok {
		return true
	}
	y, m, d := day.Date()
	switch wd := day.Weekday(); {
	case m == time.November:
		return day.Equal(thanksgiving(y).AddDate(0, 0, 1))
	case m == time.December && d == 24:
		return wd >= time.Monday && wd <= time.Thursday
	case m == time.July && d == 3:
		return wd == time.Monday || wd == time.Tuesday || wd == time.Thursday || wd == time.Wednesday && y >= 2013
	case m == time.July && d == 5:
		return wd == time.Friday && y < 2013
	}
	return false
}

// closures are the weekdays since 2000 that NYSE closed on beyond its
// rules, with why. The sources are NYSE's "New York Stock Exchange Special
// Closings, 1885-date", revised through January 2011, to 2010, and NYSE's
// announcements after it.
//
// Two sessions that ended oddly are not here and close at 16:00 in this
// calendar: on 2005-06-01 trading halted at 15:56 and did not resume, and on
// 2009-07-02 NYSE closed at 16:15 to execute orders that a fault had held
// up. Neither was a scheduled close.
var closures = map[nytime.Date]string{
	"2001-09-11": "the attack on the World Trade Center", // NYSE Special Closings
	"2001-09-12": "the attack on the World Trade Center",
	"2001-09-13": "the attack on the World Trade Center",
	"2001-09-14": "the attack on the World Trade Center",
	"2004-06-11": "the National Day of Mourning for President Ronald Reagan", // NYSE Special Closings
	"2007-01-02": "the National Day of Mourning for President Gerald Ford",   // NYSE Special Closings
	"2012-10-29": "Hurricane Sandy",                                          // NYSE, 2012-10-28
	"2012-10-30": "Hurricane Sandy",                                          // NYSE, 2012-10-29; reopened 2012-10-31
	// Proclamation 9830; NYSE's release of 2018-12-01. He died on 2018-11-30,
	// which was a full session.
	"2018-12-05": "the National Day of Mourning for President George H. W. Bush",
	// Proclamation 10876; NYSE's release of 2024-12-30.
	"2025-01-09": "the National Day of Mourning for President Jimmy Carter",
}

// earlyCloses are the sessions since 2000 that NYSE closed at 13:00 beyond
// its rules, with why. The source is NYSE's Special Closings, as for
// closures. The Friday after a Thursday Christmas was a full session in
// 2008, 2014 and 2025.
var earlyCloses = map[nytime.Date]string{
	"2003-12-26": "the Friday after Christmas",
}

// observed returns the weekday on which NYSE observes a holiday on day: the
// Friday before a Saturday, the Monday after a Sunday, else day.
func observed(day time.Time) time.Time {
	switch day.Weekday() {
	case time.Saturday:
		return day.AddDate(0, 0, -1)
	case time.Sunday:
		return day.AddDate(0, 0, 1)
	}
	return day
}

// thanksgiving returns Thanksgiving Day, the fourth Thursday of November.
func thanksgiving(year int) time.Time {
	return nthWeekday(year, time.November, time.Thursday, 4)
}

// nthWeekday returns the nth wd of the month.
func nthWeekday(year int, month time.Month, wd time.Weekday, n int) time.Time {
	day := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, (int(wd-day.Weekday())+7)%7+7*(n-1))
}

// lastWeekday returns the last wd of the month.
func lastWeekday(year int, month time.Month, wd time.Weekday) time.Time {
	day := time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC)
	return day.AddDate(0, 0, -((int(day.Weekday()-wd) + 7) % 7))
}

// easter returns Easter Sunday of year in the Gregorian calendar, by the
// anonymous algorithm of Meeus, Jones and Butcher.
func easter(year int) time.Time {
	a := year % 19
	b, c := year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	n := h + l - 7*m + 114
	return time.Date(year, time.Month(n/31), n%31+1, 0, 0, 0, 0, time.UTC)
}
