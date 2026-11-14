package lookup

import (
	"encoding/json"
	"math"
	"testing"
)

// Money is rounded to cents as Schwab wrote it, half away from zero, not
// as the double nearest to what it wrote rounds.
func TestScaled(t *testing.T) {
	for _, tc := range []struct {
		x      float64
		places int
		want   int64
		ok     bool
	}{
		{1.005, 2, 101, true},
		{0.285, 2, 29, true},
		{2.675, 2, 268, true},
		{12.345, 2, 1235, true},
		{-0.125, 2, -13, true},
		{-1.005, 2, -101, true},
		{1.004999, 2, 100, true},
		{81.29223999999999, 2, 8129, true},
		{-236.999999999999, 2, -23700, true},
		{0.30000000000000004, 6, 300000, true},
		{2.1788, 6, 2178800, true},
		{0.0000005, 6, 1, true},
		{-0.004, 2, 0, true},
		{0, 2, 0, true},
		{5e-324, 2, 0, true},
		{999999999999.99, 2, 99999999999999, true},
		{999999999999.9999, 6, 999999999999999900, true},
		{1e12, 2, 0, false},
		{-1e12, 2, 0, false},
		{1e300, 2, 0, false},
		{math.Inf(-1), 2, 0, false},
		{math.NaN(), 2, 0, false},
	} {
		if got, ok := Scaled(tc.x, tc.places); got != tc.want || ok != tc.ok {
			t.Errorf("Scaled(%v, %d) = %d, %t; want %d, %t", tc.x, tc.places, got, ok, tc.want, tc.ok)
		}
	}
}

// Amounts print with two decimals, a leading - when negative and no other
// sign, and none prints as -.
func TestMoneyString(t *testing.T) {
	for _, tc := range []struct {
		m    Money
		want string
	}{
		{Amount(-310), "-310.00"},
		{Amount(-0.004), "0.00"},
		{Amount(math.Copysign(0, -1)), "0.00"},
		{Amount(0.05), "0.05"},
		{Amount(-0.5), "-0.50"},
		{Amount(12501.52), "12501.52"},
		{Amount(1e13), "-"},
		{MoneyOf(nil), "-"},
		{Amount(1).Plus(MoneyOf(nil)), "-"},
		{MoneyOf(nil).Minus(Amount(1)), "-"},
		{Amount(5503.84).Minus(Amount(-310)), "5813.84"},
	} {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("%+v printed %q, want %q", tc.m, got, tc.want)
		}
	}
	for _, tc := range []struct {
		n            int64
		places, keep int
		want         string
	}{
		{0, 6, 0, "0"},
		{-2000000, 6, 0, "-2"},
		{2178800, 6, 0, "2.1788"},
		{81500000, 6, 2, "81.50"},
		{251307500, 6, 2, "251.3075"},
		{1, 6, 2, "0.000001"},
		{-1, 2, 2, "-0.01"},
	} {
		if got := FormatScaled(tc.n, tc.places, tc.keep); got != tc.want {
			t.Errorf("FormatScaled(%d, %d, %d) = %q, want %q", tc.n, tc.places, tc.keep, got, tc.want)
		}
	}
}

// Quantities print as many decimals as they have, up to 6, and prices at
// least 2.
func TestQtyAndPrice(t *testing.T) {
	for _, tc := range []struct {
		x               float64
		quantity, price string
	}{
		{0, "0", "0.00"},
		{math.Copysign(0, -1), "0", "0.00"},
		{-0.0000004, "0", "0.00"},
		{12, "12", "12.00"},
		{-2, "-2", "-2.00"},
		{2.1788, "2.1788", "2.1788"},
		{81.5, "81.5", "81.50"},
		{1.23456789, "1.234568", "1.234568"},
		{0.30000000000000004, "0.3", "0.30"},
		{-236.999999999999, "-237", "-237.00"},
		{1e12, "-", "-"},
	} {
		if got := QtyOf(tc.x).String(); got != tc.quantity {
			t.Errorf("QtyOf(%v) printed %q, want %q", tc.x, got, tc.quantity)
		}
		if got := PriceOf(tc.x).String(); got != tc.price {
			t.Errorf("PriceOf(%v) printed %q, want %q", tc.x, got, tc.price)
		}
	}
}

// A figure marshals as a JSON number of the digits that it prints, and a
// figure that is none, its zero value included, as null, never as 0.
func TestFiguresMarshal(t *testing.T) {
	for _, tc := range []struct {
		name string
		v    any
		want string
	}{
		{"money", Amount(-310), "-310.00"},
		{"money of cents", FromCents(150217), "1502.17"},
		{"money of no cents", FromCents(0), "0.00"},
		{"money of a negative zero", Amount(math.Copysign(0, -1)), "0.00"},
		{"no money", Money{}, "null"},
		{"money unsent", MoneyOf(nil), "null"},
		{"money too large", Amount(1e13), "null"},
		{"cents too large", FromCents(1e14), "null"},
		{"cents too small", FromCents(-1e14), "null"},
		{"cents as large as can be", FromCents(1e14 - 1), "999999999999.99"},
		{"a sum with none in it", Amount(1).Plus(Money{}), "null"},
		{"quantity", QtyOf(2.1788), "2.1788"},
		{"quantity short", QtyOf(-2), "-2"},
		{"quantity of millionths", QtyFromMicros(-1500000), "-1.5"},
		{"quantity of no millionths", QtyFromMicros(0), "0"},
		{"millionths too many", QtyFromMicros(1e18), "null"},
		{"no quantity", Qty{}, "null"},
		{"quantity too large", QtyOf(1e12), "null"},
		{"price", PriceOf(81.5), "81.50"},
		{"price of millionths", PriceOf(251.3075), "251.3075"},
		{"no price", Price{}, "null"},
		{"price too large", PriceOf(math.Inf(1)), "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.v)
			if err != nil || string(b) != tc.want {
				t.Errorf("marshalled %s, %v; want %s", b, err, tc.want)
			}
			if s := tc.v.(interface{ String() string }).String(); tc.want != "null" && s != tc.want {
				t.Errorf("printed %q, marshalled %s", s, b)
			}
		})
	}

	// In a struct, every field that is none is null, and every other one a
	// number that reads back as itself.
	type row struct {
		Value Money `json:"value"`
		Cost  Money `json:"cost"`
		Qty   Qty   `json:"qty"`
		Price Price `json:"price"`
	}
	b, err := json.Marshal(row{Value: Amount(3748.92)})
	if want := `{"value":3748.92,"cost":null,"qty":null,"price":null}`; err != nil || string(b) != want {
		t.Errorf("marshalled %s, %v; want %s", b, err, want)
	}
	var back map[string]*float64
	if err := json.Unmarshal(b, &back); err != nil || back["value"] == nil || *back["value"] != 3748.92 || back["cost"] != nil {
		t.Errorf("read back %v, %v", back, err)
	}
}

// Cents gives an amount's cents, and says when there is none.
func TestCents(t *testing.T) {
	if c, ok := Amount(-1.005).Cents(); c != -101 || !ok {
		t.Errorf("Amount(-1.005).Cents() = %d, %t; want -101, true", c, ok)
	}
	if c, ok := FromCents(42).Cents(); c != 42 || !ok {
		t.Errorf("FromCents(42).Cents() = %d, %t; want 42, true", c, ok)
	}
	for _, m := range []Money{{}, MoneyOf(nil), FromCents(1e14), Amount(1).Minus(Money{})} {
		if c, ok := m.Cents(); c != 0 || ok {
			t.Errorf("%+v.Cents() = %d, %t; want none", m, c, ok)
		}
	}
}
