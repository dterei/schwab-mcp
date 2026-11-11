// Package nytime is New York's time zone, where Schwab keeps its books and
// the NYSE trades, and the calendar dates that it gives to instants. It is a
// leaf, so that both internal/schwab and the NYSE's calendar can use it.
package nytime

import (
	"bytes"
	_ "embed" // for newYorkZone
	"sync"
	"time"
)

// Date is a calendar date, written as in "2006-01-02", or "" for none. Dates
// in this form sort and compare as strings.
type Date string

// DateOf returns the date of t in New York, or "" for the zero time. It
// turns an instant into a calendar date and knows nothing of market
// sessions: five past midnight in New York is on the new date, though the
// session it follows closed the day before.
func DateOf(t time.Time) Date {
	if t.IsZero() {
		return ""
	}
	return Date(t.In(newYork()).Format(time.DateOnly))
}

// newYorkZone is the time zone America/New_York in the tz database's binary
// form, from release 2025c as in Go's lib/time/zoneinfo.zip, for systems
// without a time zone database, such as Windows without Go installed.
//
//go:embed newyork.tzif
var newYorkZone []byte

// NewYork returns New York's time zone: the system's, which its updates
// keep current, or else the one built in. The package does not import
// time/tzdata, which would change every program's time zones, not only this
// one.
func NewYork() *time.Location {
	return newYork()
}

// newYork loads New York's time zone once, for NewYork.
var newYork = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	loc, err := time.LoadLocationFromTZData("America/New_York", newYorkZone)
	if err != nil {
		panic("nytime: the built-in time zone America/New_York is unreadable: " + err.Error())
	}
	return loc
})

// TZData returns a copy of the time zone built in, in the tz database's
// binary form, as time.LoadLocationFromTZData reads it.
func TZData() []byte {
	return bytes.Clone(newYorkZone)
}
