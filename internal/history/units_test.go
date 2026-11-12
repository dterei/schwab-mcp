package history

import (
	"database/sql"
	"math"
	"testing"
	"time"
)

// Money converts to cents, and quantities to millionths, exactly below
// 2^51 and never at or above it; binary noise is rounded and said to be;
// anything further from the grid is refused.
func TestCentsAndMicros(t *testing.T) {
	t.Parallel()
	cases := []struct {
		f     float64
		scale string
		want  int64
		how   Conversion
	}{
		// Every netAmount and cost of the fixtures, and amounts that
		// multiply to a float just off the cent.
		{0, "cents", 0, Exact},
		{math.Copysign(0, -1), "cents", 0, Exact},
		{-2.7, "cents", -270, Exact},
		{19.99, "cents", 1999, Exact}, // 19.99*100 is 1998.9999999999998
		{0.07, "cents", 7, Exact},
		{1.005, "cents", 0, Off}, // a tenth of a cent
		{-35173.08, "cents", -3517308, Exact},
		{22517998136852.47, "cents", 2251799813685247, Exact},
		{22517998136852.48, "cents", 0, Off}, // 2^51/100
		{1e15, "cents", 0, Off},
		{0.30000000000000004, "cents", 30, Rounded},
		{81.29223999999999, "cents", 0, Off},
		{71.42857, "cents", 0, Off},
		{1.000005, "cents", 0, Off}, // 5e-4 of a cent from the grid: more than noise
		{math.NaN(), "cents", 0, Off},
		{math.Inf(1), "cents", 0, Off},
		{math.Inf(-1), "cents", 0, Off},
		{0.0378, "micros", 37800, Exact},
		{-236.999999999999, "micros", -237000000, Rounded},
		{100, "micros", 100000000, Exact},
		{0.123456, "micros", 123456, Exact},
		{0.1234567, "micros", 0, Off},
		{1e-10, "micros", 0, Rounded}, // 1e-4 of a millionth, the most that is noise
		{2251799813.68, "micros", 2251799813680000, Exact},
		{2251799813.69, "micros", 0, Off}, // past 2^51/10^6
	}
	for _, c := range cases {
		conv := cents
		if c.scale == "micros" {
			conv = micros
		}
		got, how := conv(c.f)
		if got != c.want || how != c.how {
			t.Errorf("%s(%v) = %d, %v; want %d, %v", c.scale, c.f, got, how, c.want, c.how)
		}
	}
}

// An instant is UTC with three digits of fraction, whatever the zone and
// precision it came in, and reads back as the same millisecond.
func TestInstant(t *testing.T) {
	t.Parallel()
	ny := time.FixedZone("EST", -5*3600)
	for in, want := range map[time.Time]string{
		time.Date(2026, 11, 12, 10, 4, 5, 0, ny):                 "2026-11-12T15:04:05.000Z",
		time.Date(2026, 11, 12, 15, 4, 5, 678_900_000, time.UTC): "2026-11-12T15:04:05.678Z",
		time.Date(2026, 1, 2, 3, 4, 5, 999_999_999, time.UTC):    "2026-01-02T03:04:05.999Z",
	} {
		got := instant(in)
		if got != want {
			t.Errorf("instant(%v) = %s, want %s", in, got, want)
		}
		back, err := parseInstant(got)
		if err != nil || !back.Equal(in.Truncate(time.Millisecond)) {
			t.Errorf("parseInstant(%s) = %v, %v; want %v", got, back, err, in.Truncate(time.Millisecond))
		}
	}
}

// Free text is stored masked, so that an account number in it shows only
// its last three digits, and empty text as NULL.
func TestFree(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]sql.NullString{
		"":                            {},
		"Dividend Reinvest":           {String: "Dividend Reinvest", Valid: true},
		"JOURNAL TO 12345678 100 SHS": {String: "JOURNAL TO ...678 100 SHS", Valid: true},
		"Joint 1234-5678":             {String: "Joint ...678", Valid: true},
	} {
		if got := free(in); got != want {
			t.Errorf("free(%q) = %+v, want %+v", in, got, want)
		}
	}
}

// The conversions name themselves in flags' details.
func TestConversionString(t *testing.T) {
	t.Parallel()
	for c, want := range map[Conversion]string{Exact: "exact", Rounded: "rounded", Off: "off"} {
		if c.String() != want {
			t.Errorf("%d.String() = %q, want %q", int(c), c.String(), want)
		}
	}
}
