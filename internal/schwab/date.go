package schwab

import (
	_ "embed" // for newYorkZone
	"sync"
	"time"
)

// Date is a calendar date, written as in "2006-01-02", or "" for none. Dates
// in this form sort and compare as strings.
type Date string

// DateInNewYork returns the date of t in New York, where Schwab keeps its
// books, or "" for the zero time. It turns an instant into a calendar date
// and knows nothing of market sessions: five past midnight in New York is
// on the new date, though the session it follows closed the day before.
func DateInNewYork(t time.Time) Date {
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

// newYork returns New York's time zone: the system's, which its updates keep
// current, or else the one built in. The package does not import
// time/tzdata, which would change every program's time zones, not only this
// one.
var newYork = sync.OnceValue(func() *time.Location {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return loc
	}
	loc, err := time.LoadLocationFromTZData("America/New_York", newYorkZone)
	if err != nil {
		panic("schwab: the built-in time zone America/New_York is unreadable: " + err.Error())
	}
	return loc
})
