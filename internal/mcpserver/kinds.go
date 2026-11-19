package mcpserver

import (
	"math"
	"strconv"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// Every string that a tool returns has one of these kinds, so that a field
// that could carry an account number is a choice made where the field is
// declared, and TestOutputStringFieldsHaveKinds fails on a plain string.
// FreeText, MaskedNumber and Path hold text from outside, so only the
// masker makes them (TestOnlyTheMaskerMakesMaskedText); the others hold
// what cannot be an account number. An output holds no map, whose keys
// would be strings of no kind, and no interface; the one field of raw JSON,
// a json.RawMessage, holds Schwab's JSON as masker.json rewrote it, and
// says so with the tag masked:"json".

// FreeText is text that Schwab, a file or the user wrote, with every run of
// 8 or more digits, every known account number and every secret masked;
// text the store keeps, which the store masked when it stored it; or text
// the server composed from masked pieces. Only the masker makes one.
type FreeText string

// MaskedNumber is an account number as Schwab masks it: "..." and its last
// three digits. Only the masker makes one.
type MaskedNumber string

// Path is a file's path as a client may see it: under the home directory
// as ~/... with each component masked as mask.FileName masks a name and
// known numbers replaced, elsewhere only its base name, masked the same
// way. Only the masker makes one.
type Path string

// Ident is a value that cannot hold an account number: a symbol, a CUSIP,
// an account hash, a store ref, a Schwab ID written as a string, one of
// Schwab's codes for a type or status, or one of the server's own codes.
type Ident string

// Date is a New York calendar date, YYYY-MM-DD.
type Date string

// Instant is an RFC 3339 time in New York with its offset, such as
// 2026-11-13T15:59:58-05:00, with milliseconds only when they are not zero.
type Instant string

// Cursor is an opaque page cursor (cursor.go).
type Cursor string

// Num is a figure that Schwab sends as a float64, kept as sent, or none.
// Its zero value is none, which MarshalJSON writes as null, so that a
// figure never set reads as missing, not as 0.
type Num struct {
	v  float64
	ok bool
}

// NumOf returns *v, or none for nil.
func NumOf(v *float64) Num {
	if v == nil {
		return Num{}
	}
	return NumValue(*v)
}

// NumValue returns v, or none when it is not a number JSON can hold.
func NumValue(v float64) Num {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return Num{}
	}
	return Num{v: v, ok: true}
}

// NumNonZero returns v, or none when it is 0, which Schwab sends in market
// data where it has no value.
func NumNonZero(v float64) Num {
	if v == 0 {
		return Num{}
	}
	return NumValue(v)
}

// Value returns n's value, and whether it has one.
func (n Num) Value() (float64, bool) { return n.v, n.ok }

// MarshalJSON writes n in the shortest form that reads back as it, as
// encoding/json writes a float64, or null for none.
func (n Num) MarshalJSON() ([]byte, error) {
	if !n.ok {
		return []byte("null"), nil
	}
	// encoding/json's choice of format, so that a figure reads as Schwab
	// wrote it: 'e' only for very large or small magnitudes, with a
	// one-digit exponent where it has one.
	format := byte('f')
	if abs := math.Abs(n.v); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b := strconv.AppendFloat(nil, n.v, format, -1, 64)
	if format == 'e' {
		if k := len(b); k >= 4 && b[k-4] == 'e' && b[k-3] == '-' && b[k-2] == '0' {
			b[k-2] = b[k-1]
			b = b[:k-1]
		}
	}
	return b, nil
}

// instantLayout and instantMilliLayout are how an Instant is written,
// without and with milliseconds.
const (
	instantLayout      = "2006-01-02T15:04:05-07:00"
	instantMilliLayout = "2006-01-02T15:04:05.000-07:00"
)

// instantOf returns t as an Instant, in New York time, cut to the
// millisecond.
func instantOf(t time.Time) Instant {
	t = t.In(nytime.NewYork()).Truncate(time.Millisecond)
	if t.Nanosecond() == 0 {
		return Instant(t.Format(instantLayout))
	}
	return Instant(t.Format(instantMilliLayout))
}

// instantPtr returns t as an Instant, or nil for the zero time, which is
// none.
func instantPtr(t time.Time) *Instant {
	if t.IsZero() {
		return nil
	}
	return new(instantOf(t))
}

// datePtr returns d as a Date, or nil for "", which is none.
func datePtr(d nytime.Date) *Date {
	if d == "" {
		return nil
	}
	return new(Date(d))
}

// identPtr returns s as an Ident, or nil for "", which is none.
func identPtr(s string) *Ident {
	if s == "" {
		return nil
	}
	return new(Ident(s))
}
