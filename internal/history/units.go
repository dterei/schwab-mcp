package history

import "time"

// instantLayout is how the store writes an instant: UTC, always with three
// digits of fraction, so that every instant has one width and text order is
// time order. SQLite's date functions read it.
const instantLayout = "2006-01-02T15:04:05.000Z"

// instant returns t as the store writes it. The store never binds a
// time.Time, which the driver would write in t.String()'s form.
func instant(t time.Time) string {
	return t.UTC().Format(instantLayout)
}
