package history

import (
	"database/sql"
	"math"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
)

// A Conversion says how a float that Schwab sent became a whole number of
// a finer unit, such as cents: exactly, rounded from noise, or not at all.
type Conversion int

const (
	// Exact means that the float is a whole number of the unit.
	Exact Conversion = iota
	// Rounded means that the float is within a ten-thousandth of a unit of
	// a whole number of it: binary noise from Schwab's own arithmetic, such
	// as 0.30000000000000004 or -236.999999999999. The store keeps the
	// whole number and flags the value rounded.
	Rounded
	// Off means that the float is further from a whole number of the unit,
	// or too large to convert surely. The store keeps no number and flags
	// the value inexact; the raw JSON keeps what Schwab sent.
	Off
)

func (c Conversion) String() string {
	switch c {
	case Exact:
		return "exact"
	case Rounded:
		return "rounded"
	}
	return "off"
}

// maxGrid is 2^51. Below it, a value's product with the scale is within a
// quarter of a unit of the whole number it stands for, and a double's
// rounding error is at most an eighth, so rounding cannot miss.
const maxGrid = 1 << 51

// noise is how far from a whole number of the unit a product may be and
// still be taken for it: far below any step of the unit, and far above a
// double's error at any value the store keeps exactly.
const noise = 1e-4

// cents returns f, an amount of money, in cents. It is Exact when
// math.Round(f*100), divided back by 100, gives f, and |f*100| < 2^51;
// Rounded when f*100 is within a ten-thousandth of that whole number, and
// below 2^51; Off otherwise, when c is 0 and the column stays NULL.
func cents(f float64) (c int64, how Conversion) {
	return onGrid(f, 100)
}

// micros returns f, a quantity, in millionths, as cents does at 10^6.
func micros(f float64) (m int64, how Conversion) {
	return onGrid(f, 1e6)
}

// onGrid returns f in units of 1/scale.
func onGrid(f, scale float64) (int64, Conversion) {
	x := f * scale
	if !(math.Abs(x) < maxGrid) { // also NaN
		return 0, Off
	}
	r := math.Round(x)
	switch {
	case r/scale == f:
		return int64(r), Exact
	case math.Abs(x-r) <= noise:
		return int64(r), Rounded
	}
	return 0, Off
}

// instantLayout is how the store writes an instant: UTC, always with three
// digits of fraction, so that every instant has one width and text order is
// time order. SQLite's date functions read it.
const instantLayout = "2006-01-02T15:04:05.000Z"

// instant returns t as the store writes it. The store never binds a
// time.Time, which the driver would write in t.String()'s form.
func instant(t time.Time) string {
	return t.UTC().Format(instantLayout)
}

// parseInstant reads an instant that the store wrote.
func parseInstant(s string) (time.Time, error) {
	return time.Parse(instantLayout, s)
}

// free returns free text that Schwab, a file or you wrote, as a column that
// views show holds it: through mask.Text, so that an account number in it
// shows only its last three digits, or NULL when it is empty. Free text is
// masked as it is stored, before anything is composed of it.
func free(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: mask.Text(s), Valid: true}
}
