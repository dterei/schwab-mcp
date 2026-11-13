package schwab

import (
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// Date is a calendar date, written as in "2006-01-02", or "" for none. Dates
// in this form sort and compare as strings.
type Date = nytime.Date

// DateInNewYork returns the date of t in New York, where Schwab keeps its
// books, or "" for the zero time. It turns an instant into a calendar date
// and knows nothing of market sessions: five past midnight in New York is
// on the new date, though the session it follows closed the day before.
func DateInNewYork(t time.Time) Date {
	return nytime.DateOf(t)
}

// newYork returns New York's time zone: the system's, or else the one that
// nytime builds in.
func newYork() *time.Location {
	return nytime.NewYork()
}
