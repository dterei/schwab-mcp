package history

import (
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The name-based UUIDs are RFC 9562's version 8 with SHA-256: the draft's
// own vector, in the DNS namespace, and the tool exports' namespace with the
// names that 6.7 of the design gives, worked out apart from this code.
func TestUUID8(t *testing.T) {
	t.Parallel()
	dns := [16]byte{0x6b, 0xa7, 0xb8, 0x10, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	if got := uuidText(dns); got != "6ba7b810-9dad-11d1-80b4-00c04fd430c8" {
		t.Fatalf("the DNS namespace is written %s", got)
	}
	if got, want := uuid8(dns, "www.example.com"), "5c146b14-3c52-8afd-938a-375d0df1fbf6"; got != want {
		t.Errorf("www.example.com in the DNS namespace: %s, want %s", got, want)
	}
	if got := uuidText(toolNamespace); got != "ee8f3721-61a6-4223-b291-35fb2c5a4013" {
		t.Errorf("the tool exports' namespace is %s", got)
	}
	for _, c := range []struct{ name, want string }{
		{"account A1 ...135", "e6461b57-da15-845e-b90b-7d04787642e5"},
		{"account A2 ...398", "2a1620c1-a41c-8592-88fc-7092e7c386d4"},
		{"profile fees", "86890ed8-5f41-88fe-994e-a8d55fb5a16f"},
		{"profile interest", "1aaa2776-c2e6-8a46-b37b-63919ecbda2a"},
		{"profile taxes", "ea68cc1f-2a7b-84bd-9ee8-7f46a5c7dccd"},
		{"security option:XYZ250221P00040000", "fc94e404-4c85-87f0-962e-bb88e7134119"},
		{"security cusip:9128ZZQ15", "c705d699-9368-8698-a8e8-f840d9267e86"},
		{"", "d19c1743-08bd-8c5a-97c3-2097ac8280c8"},
		{"é", "c1d047bf-f16a-88c5-9582-e28b09497f2f"},
	} {
		got := uuid8(toolNamespace, c.name)
		if got != c.want {
			t.Errorf("uuid8(%q) = %s, want %s", c.name, got, c.want)
		}
		// Version 8 and the RFC's variant, as Ghostfolio's validator wants.
		if got[14] != '8' || !strings.ContainsRune("89ab", rune(got[19])) {
			t.Errorf("uuid8(%q) = %s is not of version 8 and the RFC's variant", c.name, got)
		}
	}
}

// ratOf returns the fraction a/b.
func ratOf(a, b int64) *big.Rat { return big.NewRat(a, b) }

// roundRat rounds half away from zero, as 7 of the design says, and writes
// no trailing zero, no point without digits after it, and no minus zero; the
// two quotients of 7 are Ghostfolio's unit prices of the fixtures'
// reinvestment and fractional sale.
func TestRoundRat(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		r      *big.Rat
		places int
		want   string
	}{
		{ratOf(270*10_000, 37_800), 10, "71.4285714286"},
		{ratOf(8327*10_000, 451_300), 10, "184.511411478"},
		{ratOf(31_000, 20_000), 8, "1.55"},
		{ratOf(1, 3), 8, "0.33333333"},
		{ratOf(2, 3), 8, "0.66666667"},
		{ratOf(-2, 3), 8, "-0.66666667"},
		{ratOf(1005, 1000), 2, "1.01"},
		{ratOf(-1005, 1000), 2, "-1.01"},
		{ratOf(1, 2), 0, "1"},
		{ratOf(-1, 2), 0, "-1"},
		{ratOf(-5, 2), 0, "-3"},
		{ratOf(49, 100), 0, "0"},
		{ratOf(2000, 1000), 3, "2"},
		{ratOf(1, 1), 10, "1"},
		{ratOf(0, 1), 8, "0"},
		{ratOf(-1, 1_000_000_000_000), 8, "0"},
		{ratOf(4, 1_000_000_000), 8, "0"},
		{ratOf(5, 1_000_000_000), 8, "0.00000001"},
		{ratOf(123_456_789_012, 1), 2, "123456789012"},
		{ratOf(7, 100), 2, "0.07"},
		{ratOf(-7, 100), 2, "-0.07"},
	} {
		if got := roundRat(c.r, c.places); got != c.want {
			t.Errorf("roundRat(%s, %d) = %s, want %s", c.r.RatString(), c.places, got, c.want)
		}
	}
	// roundUnits gives the same rounding as a whole number of units.
	for _, c := range []struct {
		r      *big.Rat
		places int
		want   int64
	}{
		{ratOf(1555, 1000), 2, 156},
		{ratOf(-1555, 1000), 2, -156},
		{ratOf(1554, 1000), 2, 155},
		{ratOf(-1554, 1000), 2, -155},
		{ratOf(37_800*7_142_857, 100_000*1_000_000), 2, 270},
	} {
		if got := roundUnits(c.r, c.places); got.Int64() != c.want || !got.IsInt64() {
			t.Errorf("roundUnits(%s, %d) = %s, want %d", c.r.RatString(), c.places, got, c.want)
		}
	}
}

// decimalRat reads a REAL as the decimal that the store's export writes
// for it, exactly, not as the binary double nearest it.
func TestDecimalRat(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		v    float64
		want string
	}{
		{71.42857, "7142857/100000"},
		{0.1, "1/10"},
		{184.51, "18451/100"},
		{2, "2"},
		{-310, "-310"},
		{-3.12, "-78/25"},
		{1e21, "1000000000000000000000"},
		{0, "0"},
	} {
		got := decimalRat(c.v)
		if got == nil || got.RatString() != c.want {
			t.Errorf("decimalRat(%v) = %v, want %s", c.v, got, c.want)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got := decimalRat(v); got != nil {
			t.Errorf("decimalRat(%v) = %s, want nil", v, got.RatString())
		}
	}
}

// occCompact writes either spelling of a contract that the store reads,
// the OCC symbol and schwab.com's, as Yahoo writes options, and nothing for
// a contract that the OCC form cannot hold.
func TestOCCCompact(t *testing.T) {
	t.Parallel()
	parse := func(s string) schwab.OCC {
		t.Helper()
		o, ok := schwab.ParseOCC(s)
		if !ok {
			o, ok = schwabfile.ParseOption(s)
		}
		if !ok {
			t.Fatalf("%q is not a contract", s)
		}
		return o
	}
	for _, c := range []struct{ symbol, want string }{
		{"XYZ   250221P00040000", "XYZ250221P00040000"},
		{"XYZ   260918C00050000", "XYZ260918C00050000"},
		{"SPXW  261218C06500000", "SPXW261218C06500000"},
		{"XYZ 09/18/2026 50.00 C", "XYZ260918C00050000"},
		{"XYZ 02/21/2025 40.50 P", "XYZ250221P00040500"},
		{"XYZ 02/21/2025 0.5 P", "XYZ250221P00000500"},
	} {
		if got := occCompact(parse(c.symbol)); got != c.want {
			t.Errorf("occCompact(%q) = %q, want %q", c.symbol, got, c.want)
		}
	}
	good := schwab.OCC{Root: "XYZ", Expiration: "2025-02-21", PutCall: "PUT", StrikeThousandths: 40_000}
	if got := occCompact(good); got != "XYZ250221P00040000" {
		t.Fatalf("occCompact(%+v) = %q", good, got)
	}
	for _, c := range []struct {
		name string
		edit func(*schwab.OCC)
	}{
		{"no root", func(o *schwab.OCC) { o.Root = "" }},
		{"no expiration", func(o *schwab.OCC) { o.Expiration = "" }},
		{"an expiration that is not a date", func(o *schwab.OCC) { o.Expiration = "2025-02-30" }},
		{"neither put nor call", func(o *schwab.OCC) { o.PutCall = "P" }},
		{"a strike of nine digits", func(o *schwab.OCC) { o.StrikeThousandths = 100_000_000 }},
		{"a negative strike", func(o *schwab.OCC) { o.StrikeThousandths = -1 }},
	} {
		o := good
		c.edit(&o)
		if got := occCompact(o); got != "" {
			t.Errorf("%s: occCompact = %q, want none", c.name, got)
		}
	}
	o := good
	o.StrikeThousandths = 99_999_999
	if got := occCompact(o); got != "XYZ250221P99999999" {
		t.Errorf("the largest strike: %q", got)
	}
}

// yahooForm writes class shares as Yahoo does, whichever way Schwab spells
// them, and leaves every other ticker as it is.
func TestYahooForm(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ symbol, want string }{
		{"BRK.B", "BRK-B"},
		{"BRK/B", "BRK-B"},
		{"BRK B", "BRK-B"},
		{"BF.A", "BF-A"},
		{"ABCDE.XY", "ABCDE-XY"},
		{"ABCDEF.B", "ABCDEF.B"},
		{"PSA.PRK", "PSA.PRK"},
		{"brk.b", "brk.b"},
		{"BRK-B", "BRK-B"},
		{"AAPL", "AAPL"},
		{"$SPX", "$SPX"},
		{"XYZ   250221P00040000", "XYZ   250221P00040000"},
		{"", ""},
	} {
		if got := yahooForm(c.symbol); got != c.want {
			t.Errorf("yahooForm(%q) = %q, want %q", c.symbol, got, c.want)
		}
	}
}

// nyClock reads the New York date and wall clock of an instant, across
// midnight UTC and on both days the clocks change.
func TestNYClock(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		at      string
		day     nytime.Date
		seconds int
	}{
		{"2025-01-21T16:05:12Z", "2025-01-21", 39_912},
		{"2024-05-01T01:18:44Z", "2024-04-30", 21*3600 + 18*60 + 44},
		{"2024-04-04T04:00:00Z", "2024-04-04", 0},
		{"2024-04-04T03:59:59Z", "2024-04-03", 86_399},
		// 2026-03-08: at 2:00 New York's clocks jump to 3:00, so half an
		// hour later than 1:59 reads 3:30, two and a half hours after
		// midnight.
		{"2026-03-08T07:30:00Z", "2026-03-08", 3*3600 + 30*60},
		// 2025-11-02: 1:30 comes twice; the second time, after the clocks
		// went back, it still reads 1:30.
		{"2025-11-02T06:30:00Z", "2025-11-02", 3600 + 30*60},
		{"2025-11-02T05:30:00Z", "2025-11-02", 3600 + 30*60},
	} {
		at, err := time.Parse(time.RFC3339, c.at)
		if err != nil {
			t.Fatal(err)
		}
		day, seconds := nyClock(at)
		if day != c.day || seconds != c.seconds {
			t.Errorf("nyClock(%s) = %s, %d; want %s, %d", c.at, day, seconds, c.day, c.seconds)
		}
	}
	if day, seconds := nyClock(time.Time{}); day != "" || seconds != 0 {
		t.Errorf("nyClock of the zero time = %q, %d", day, seconds)
	}
}

// gfInstant puts an activity at noon UTC of its day plus its offset: 7 of
// the design's 11:05:12 in New York, and the ends of the window, whose last
// instant keeps the day from UTC-11 to UTC+11.
func TestGFInstant(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		day    nytime.Date
		offset int64
		want   string
	}{
		{"2025-01-21", 39_912, "2025-01-21T12:00:39.912Z"},
		{"2025-01-21", 0, "2025-01-21T12:00:00.000Z"},
		{"2025-01-21", 1, "2025-01-21T12:00:00.001Z"},
		{"2025-01-21", 86_399, "2025-01-21T12:01:26.399Z"},
		{"2025-01-21", 86_400, "2025-01-21T12:01:26.400Z"},
		{"2024-02-29", 0, "2024-02-29T12:00:00.000Z"},
	} {
		if got := gfInstant(c.day, c.offset); got != c.want {
			t.Errorf("gfInstant(%s, %d) = %s, want %s", c.day, c.offset, got, c.want)
		}
	}
	for _, day := range []nytime.Date{"", "2025-02-30", "2025-1-21"} {
		if got := gfInstant(day, 0); got != "" {
			t.Errorf("gfInstant(%q) = %s, want none", day, got)
		}
	}
}
