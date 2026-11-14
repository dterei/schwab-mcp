package lookup

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Money is an amount in cents, or none: a figure that Schwab did not send,
// or that could not be read. Its zero value is none, which MarshalJSON
// writes as null, so that a figure never set reads as missing, not as 0.
type Money struct {
	cents int64
	ok    bool
}

// MoneyOf returns *v in cents, or none for nil.
func MoneyOf(v *float64) Money {
	if v == nil {
		return Money{}
	}
	return Amount(*v)
}

// Amount returns x in cents, rounded as Scaled rounds it.
func Amount(x float64) Money {
	cents, ok := Scaled(x, 2)
	return Money{cents: cents, ok: ok}
}

// FromCents returns an amount of cents, such as the store keeps, or none
// for a trillion dollars or more, as Amount does, so that sums of amounts
// cannot overflow.
func FromCents(cents int64) Money {
	if cents <= -maxCents || cents >= maxCents {
		return Money{}
	}
	return Money{cents: cents, ok: true}
}

// maxCents is a trillion dollars, which no real figure reaches.
const maxCents = 1e14

// Cents returns m in cents, and whether it is an amount rather than none.
func (m Money) Cents() (int64, bool) { return m.cents, m.ok }

// Plus returns m + n, or none when either is none.
func (m Money) Plus(n Money) Money {
	if !m.ok || !n.ok {
		return Money{}
	}
	return Money{cents: m.cents + n.cents, ok: true}
}

// Minus returns m - n, or none when either is none.
func (m Money) Minus(n Money) Money {
	return m.Plus(Money{cents: -n.cents, ok: n.ok})
}

// String returns m with two decimals and a leading - when it is negative,
// as schwabtx's transactions table prints amounts, or "-" for none. Zero is
// 0.00, never -0.00.
func (m Money) String() string {
	if !m.ok {
		return "-"
	}
	return FormatScaled(m.cents, 2, 2)
}

// MarshalJSON writes m as a JSON number of the digits that String writes,
// or as null for none.
func (m Money) MarshalJSON() ([]byte, error) {
	if !m.ok {
		return []byte("null"), nil
	}
	return []byte(m.String()), nil
}

// Scaled returns x rounded to places decimals, half away from zero, as a
// whole number of tenths, hundredths or the like. It rounds the shortest
// decimal that reads back as x, which is the number as Schwab wrote it, so
// that 1.005 rounds up, though the double nearest to it is below it. ok is
// false for a trillion or more, which no real figure reaches, so that sums
// of the results cannot overflow.
func Scaled(x float64, places int) (int64, bool) {
	if math.Abs(x) >= 1e12 {
		return 0, false
	}
	whole, frac, _ := strings.Cut(strconv.FormatFloat(math.Abs(x), 'f', -1, 64), ".")
	frac += strings.Repeat("0", places+1)
	n, err := strconv.ParseInt(whole+frac[:places], 10, 64)
	if err != nil { // NaN, which JSON cannot hold
		return 0, false
	}
	if frac[places] >= '5' {
		n++
	}
	if x < 0 {
		n = -n
	}
	return n, true
}

// FormatScaled writes n, a whole number of tenths, hundredths or the like,
// as places says, as a decimal with at least keep decimals: the zeros that
// end the others are dropped, with the point when none is left. It never
// writes -0.
func FormatScaled(n int64, places, keep int) string {
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	digits := fmt.Sprintf("%0*d", places+1, n)
	whole, frac := digits[:len(digits)-places], strings.TrimRight(digits[len(digits)-places:], "0")
	frac += strings.Repeat("0", max(0, keep-len(frac)))
	if frac == "" {
		return sign + whole
	}
	return sign + whole + "." + frac
}

// maxMicros is a trillion units in millionths, the most that Scaled gives
// for 6 decimals.
const maxMicros = 1e18

// Qty is a quantity, of shares, contracts or units, in millionths, or
// none: one that could not be read, or too large to be one. Its zero value
// is none, which MarshalJSON writes as null.
type Qty struct {
	micros int64
	ok     bool
}

// QtyOf returns x rounded to 6 decimals, as Scaled rounds it, or none when
// it is too large to be a quantity.
func QtyOf(x float64) Qty {
	n, ok := Scaled(x, 6)
	return Qty{micros: n, ok: ok}
}

// QtyFromMicros returns a quantity of n millionths, such as the store
// keeps, or none for a trillion or more, as QtyOf does.
func QtyFromMicros(n int64) Qty {
	if n <= -maxMicros || n >= maxMicros {
		return Qty{}
	}
	return Qty{micros: n, ok: true}
}

// String returns q without the zeros that end it, 12, 2.1788 or -2, or "-"
// for none.
func (q Qty) String() string {
	if !q.ok {
		return "-"
	}
	return FormatScaled(q.micros, 6, 0)
}

// MarshalJSON writes q as a JSON number of the digits that String writes,
// or as null for none.
func (q Qty) MarshalJSON() ([]byte, error) {
	if !q.ok {
		return []byte("null"), nil
	}
	return []byte(q.String()), nil
}

// Price is a price in millionths, or none: one that could not be read, or
// too large to be one. Its zero value is none, which MarshalJSON writes as
// null.
type Price struct {
	micros int64
	ok     bool
}

// PriceOf returns x rounded to 6 decimals, as Scaled rounds it, or none
// when it is too large to be a price.
func PriceOf(x float64) Price {
	n, ok := Scaled(x, 6)
	return Price{micros: n, ok: ok}
}

// String returns p with at least 2 decimals, 81.50 or 251.3075, or "-" for
// none.
func (p Price) String() string {
	if !p.ok {
		return "-"
	}
	return FormatScaled(p.micros, 6, 2)
}

// MarshalJSON writes p as a JSON number of the digits that String writes,
// or as null for none.
func (p Price) MarshalJSON() ([]byte, error) {
	if !p.ok {
		return []byte("null"), nil
	}
	return []byte(p.String()), nil
}
