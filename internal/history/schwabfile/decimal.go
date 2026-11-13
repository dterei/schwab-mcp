package schwabfile

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// A Decimal is a number as a file writes it: exact, absent, a placeholder,
// or text that is not a number. Files write money and quantities as text,
// and the store keeps them exactly from it, never through a float.
type Decimal struct {
	// Text is the cell as written, trimmed.
	Text string
	// Present is false for a blank cell.
	Present bool
	// Placeholder is the placeholder the cell holds in place of a number:
	// "--", "N/A" or "Missing", as written. Realized Gain/Loss writes one
	// in the money cells of a lot without a history, such as one
	// transferred in.
	Placeholder string

	// The value, when the cell is a number: (-1)^neg * coef / 10^scale,
	// coef the digits without leading zeros, and without trailing zeros
	// while scale > 0, so that each value has one spelling. ok is false
	// for a cell that is blank, a placeholder or not a number.
	ok    bool
	neg   bool
	coef  string
	scale int
}

// placeholders are what a file writes in a cell in place of a number, in
// the case ParseDecimal compares them in.
var placeholders = map[string]bool{"--": true, "n/a": true, "missing": true}

// ParseDecimal reads a number as schwab.com's files write it: it trims
// the cell, strips "$" and ",", reads "(x)" and "($x)" as -x and "-$x"
// as -x, takes a blank cell for absent and "--", "N/A" and "Missing"
// for placeholders, and reads the rest as an exact decimal: a sign, digits
// and at most one point. It fails, naming the text, when a cell that is
// neither blank nor a placeholder is not such a number; the Decimal it
// returns then still holds the text.
func ParseDecimal(s string) (Decimal, error) {
	d := Decimal{Text: strings.TrimSpace(s)}
	if d.Text == "" {
		return d, nil
	}
	d.Present = true
	if placeholders[strings.ToLower(d.Text)] {
		d.Placeholder = d.Text
		return d, nil
	}
	t := d.Text
	parens := strings.HasPrefix(t, "(") && strings.HasSuffix(t, ")")
	if parens {
		t = strings.TrimSpace(t[1 : len(t)-1])
	}
	t = strings.NewReplacer("$", "", ",", "").Replace(t)
	neg := false
	switch {
	case strings.HasPrefix(t, "-"):
		neg, t = true, t[1:]
	case strings.HasPrefix(t, "+"):
		t = t[1:]
	}
	if parens && neg {
		return d, fmt.Errorf("%q is not a number", d.Text)
	}
	whole, frac, _ := strings.Cut(t, ".")
	if whole+frac == "" || !allDigits(whole) || !allDigits(frac) {
		return d, fmt.Errorf("%q is not a number", d.Text)
	}
	coef := strings.TrimLeft(whole+frac, "0")
	scale := len(frac)
	for scale > 0 && strings.HasSuffix(coef, "0") {
		coef, scale = coef[:len(coef)-1], scale-1
	}
	if coef == "" {
		coef, scale = "0", 0
	}
	d.ok, d.neg, d.coef, d.scale = true, (neg || parens) && coef != "0", coef, scale
	return d, nil
}

// allDigits reports whether s holds only ASCII digits; "" does.
func allDigits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// Number reports whether d is a number, not blank, a placeholder or other
// text.
func (d Decimal) Number() bool {
	return d.ok
}

// Cents returns d in cents, when it is a number that is a whole number of
// cents and fits in an int64.
func (d Decimal) Cents() (int64, bool) {
	return d.units(2)
}

// Micros returns d in millionths, when it is a number with at most six
// decimals that fits in an int64. A quantity with more decimals has no
// whole number of millionths, and the store keeps none for it.
func (d Decimal) Micros() (int64, bool) {
	return d.units(6)
}

// units returns d * 10^places when that is a whole number that fits in an
// int64.
func (d Decimal) units(places int) (int64, bool) {
	if !d.ok || d.scale > places {
		return 0, false
	}
	digits := d.coef
	if d.coef != "0" {
		digits += strings.Repeat("0", places-d.scale)
	}
	if d.neg {
		digits = "-" + digits
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// Float returns d as the nearest float64, when it is a number whose
// magnitude a float64 holds.
func (d Decimal) Float() (float64, bool) {
	if !d.ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(d.Canon(), 64)
	if err != nil || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// Canon returns the exact value of d in one spelling, whatever the file
// wrote: an optional minus, the digits without leading zeros and, when
// there is a fraction, a point and its digits without trailing zeros, such
// as "-1234.5" for "($1,234.50)" and "0" for "-0.00". It is "" when d is
// not a number.
func (d Decimal) Canon() string {
	if !d.ok {
		return ""
	}
	digits := d.coef
	if d.scale > 0 {
		if pad := d.scale + 1 - len(digits); pad > 0 {
			digits = strings.Repeat("0", pad) + digits
		}
		digits = digits[:len(digits)-d.scale] + "." + digits[len(digits)-d.scale:]
	}
	if d.neg {
		return "-" + digits
	}
	return digits
}

// rat returns d as an exact rational, when it is a number.
func (d Decimal) rat() (*big.Rat, bool) {
	if !d.ok {
		return nil, false
	}
	r, ok := new(big.Rat).SetString(d.Canon())
	return r, ok
}

// sum adds the numbers ds exactly. It reports false when one of them is
// present but not a number, a placeholder included, since the sum is then
// unknown; a blank cell adds nothing.
func sum(ds ...Decimal) (*big.Rat, bool) {
	total := new(big.Rat)
	for _, d := range ds {
		if !d.Present {
			continue
		}
		r, ok := d.rat()
		if !ok {
			return nil, false
		}
		total.Add(total, r)
	}
	return total, true
}

// ratText writes r, a sum of decimals, as a decimal with the fewest
// digits that write it exactly, or as many as 20.
func ratText(r *big.Rat) string {
	for places := range 20 {
		s := r.FloatString(places)
		if back, ok := new(big.Rat).SetString(s); ok && back.Cmp(r) == 0 {
			return s
		}
	}
	return r.FloatString(20)
}
