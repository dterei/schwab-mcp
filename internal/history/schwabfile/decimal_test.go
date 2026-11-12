package schwabfile

import (
	"math"
	"strings"
	"testing"
)

// ParseDecimal reads numbers as schwab.com's files write them: dollars
// with or without separators, the minus before or after the dollar sign,
// accounting parentheses with or without it, a plus, a point at either
// end; a blank cell is absent and "--", "N/A" and "Missing" are
// placeholders, in any case; anything else is refused, with its text kept.
func TestDecimal(t *testing.T) {
	for _, c := range []struct {
		in, canon      string
		cents, micros  int64
		centsOK, micOK bool
	}{
		{"$1234.56", "1234.56", 123456, 1234560000, true, true},
		{" -$1,234.56 ", "-1234.56", -123456, -1234560000, true, true},
		{"$-1,234.56", "-1234.56", -123456, -1234560000, true, true},
		{"(10.65)", "-10.65", -1065, -10650000, true, true},
		{"($10.65)", "-10.65", -1065, -10650000, true, true},
		{"( $1,000.00 )", "-1000", -100000, -1000000000, true, true},
		{"1,000", "1000", 100000, 1000000000, true, true},
		{"+5", "5", 500, 5000000, true, true},
		{"$12.3456", "12.3456", 0, 12345600, false, true},
		{"0.0123", "0.0123", 0, 12300, false, true},
		{"0.1234567", "0.1234567", 0, 0, false, false},
		{"98.765", "98.765", 0, 98765000, false, true},
		{".5", "0.5", 50, 500000, true, true},
		{"5.", "5", 500, 5000000, true, true},
		{"-0.00", "0", 0, 0, true, true},
		{"($0.00)", "0", 0, 0, true, true},
		{"007.50", "7.5", 750, 7500000, true, true},
		{"1.10000", "1.1", 110, 1100000, true, true},
		{"92233720368547758.07", "92233720368547758.07", 9223372036854775807, 0, true, false},
		{"92233720368547758.08", "92233720368547758.08", 0, 0, false, false},
		{"-92233720368547758.08", "-92233720368547758.08", -9223372036854775808, 0, true, false},
		{"123456789012345678901234567890", "123456789012345678901234567890", 0, 0, false, false},
	} {
		d, err := ParseDecimal(c.in)
		if err != nil {
			t.Errorf("ParseDecimal(%q): %v", c.in, err)
			continue
		}
		if !d.Present || !d.Number() || d.Placeholder != "" || d.Text != strings.TrimSpace(c.in) || d.Canon() != c.canon {
			t.Errorf("ParseDecimal(%q) = %+v, canon %q, want %q", c.in, d, d.Canon(), c.canon)
		}
		if cents, ok := d.Cents(); cents != c.cents || ok != c.centsOK {
			t.Errorf("%q in cents is %d, %v, want %d, %v", c.in, cents, ok, c.cents, c.centsOK)
		}
		if micros, ok := d.Micros(); micros != c.micros || ok != c.micOK {
			t.Errorf("%q in millionths is %d, %v, want %d, %v", c.in, micros, ok, c.micros, c.micOK)
		}
		if f, ok := d.Float(); !ok || math.Abs(f-mustFloat(t, c.canon)) > 0 {
			t.Errorf("%q as a float is %v, %v", c.in, f, ok)
		}
	}
	for _, in := range []string{"", "   "} {
		d, err := ParseDecimal(in)
		if err != nil || d.Present || d.Number() || d.Canon() != "" {
			t.Errorf("ParseDecimal(%q) = %+v, %v, want absent", in, d, err)
		}
		if _, ok := d.Cents(); ok {
			t.Errorf("%q has cents", in)
		}
	}
	for _, in := range []string{"--", "N/A", "n/a", "Missing", " MISSING "} {
		d, err := ParseDecimal(in)
		if err != nil || !d.Present || d.Number() || d.Placeholder != strings.TrimSpace(in) || d.Canon() != "" {
			t.Errorf("ParseDecimal(%q) = %+v, %v, want a placeholder", in, d, err)
		}
		if _, ok := d.Float(); ok {
			t.Errorf("placeholder %q has a float", in)
		}
	}
	for _, in := range []string{"abc", "-", "$", ".", "1.2.3", "1e5", "(-5)", "-(5)", "--5", "5-", "$ 5", "1 000", "12%", "(5", "0x10", "١٢"} {
		d, err := ParseDecimal(in)
		if err == nil || !d.Present || d.Number() || d.Text != in || !strings.Contains(err.Error(), `"`+in+`"`) {
			t.Errorf("ParseDecimal(%q) = %+v, %v, want refused with its text", in, d, err)
		}
		if _, ok := d.Micros(); ok {
			t.Errorf("%q has millionths", in)
		}
	}
	if _, ok := (Decimal{}).Float(); ok {
		t.Error("the zero Decimal has a float")
	}
	if d, _ := ParseDecimal("1" + strings.Repeat("0", 400)); func() bool { _, ok := d.Float(); return ok }() {
		t.Error("a number beyond a float64 has a float")
	}
}

// mustFloat parses a canonical decimal as a float.
func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	d, err := ParseDecimal(s)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := d.rat()
	f, _ := r.Float64()
	return f
}

// sum adds exactly, skips blanks and is unknown when a cell is a
// placeholder or not a number; ratText writes a sum in the fewest digits.
func TestSum(t *testing.T) {
	parse := func(ss ...string) []Decimal {
		var ds []Decimal
		for _, s := range ss {
			d, _ := ParseDecimal(s)
			ds = append(ds, d)
		}
		return ds
	}
	if r, ok := sum(parse("$0.10", "$0.20", "", "-$0.30")...); !ok || r.Sign() != 0 || ratText(r) != "0" {
		t.Errorf("0.10 + 0.20 - 0.30 = %v, %v", r, ok)
	}
	if r, ok := sum(parse("$1,000.005", "0.0000001")...); !ok || ratText(r) != "1000.0050001" {
		t.Errorf("the sum is %s", ratText(r))
	}
	for _, cells := range [][]string{{"$1.00", "--"}, {"abc"}} {
		if _, ok := sum(parse(cells...)...); ok {
			t.Errorf("%q sums", cells)
		}
	}
}
