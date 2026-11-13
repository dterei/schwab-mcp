package history

import (
	"crypto/sha256"
	"encoding/hex"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The tool exports (Portfolio Performance and Ghostfolio) write numbers,
// symbols, identifiers and instants in these forms. Every number is worked
// out from the store's whole cents and millionths, or from the shortest
// decimal text of a REAL, through math/big: never through a float, whose
// rounding would make two exports of one store differ.

// roundRat writes r rounded half away from zero to places decimal places,
// without trailing zeros, and without a point when nothing follows it:
// 2.70 ÷ 0.0378 to 10 places is 71.4285714286, and 2.000 is 2.
func roundRat(r *big.Rat, places int) string {
	units := roundUnits(r, places)
	neg := units.Sign() < 0
	digits := units.Abs(units).String()
	if pad := places + 1 - len(digits); pad > 0 {
		digits = strings.Repeat("0", pad) + digits
	}
	whole, frac := digits[:len(digits)-places], strings.TrimRight(digits[len(digits)-places:], "0")
	s := whole
	if frac != "" {
		s += "." + frac
	}
	if neg {
		s = "-" + s
	}
	return s
}

// roundUnits returns r in units of 10^-places, rounded half away from
// zero: 1.555 to 2 places is 156 hundredths, and -1.555 is -156.
func roundUnits(r *big.Rat, places int) *big.Int {
	num := new(big.Int).Mul(r.Num(), new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(places)), nil))
	q, m := new(big.Int).QuoRem(num, r.Denom(), new(big.Int))
	if m.Abs(m).Lsh(m, 1).Cmp(r.Denom()) >= 0 {
		q.Add(q, big.NewInt(int64(num.Sign())))
	}
	return q
}

// decimalRat returns v, a REAL that the store holds (a price, a market
// value), as the exact rational that its shortest decimal text writes:
// strconv.FormatFloat(v, 'f', -1, 64), as the store's export writes it, so
// that 71.42857 is 7142857/100000 and not the double nearest it. It returns
// nil for a NaN or an infinity, which the store never holds.
func decimalRat(v float64) *big.Rat {
	r, ok := new(big.Rat).SetString(formatPrice(v))
	if !ok {
		return nil
	}
	return r
}

// occCompact writes an option contract as an OCC symbol without the root's
// padding, as Yahoo writes options and Portfolio Performance's own
// importers do: the root, the expiration as YYMMDD, C or P, and the strike
// in thousandths of a dollar as eight digits, so that XYZ   250221P00040000
// is XYZ250221P00040000. It returns "" for a contract that the form cannot
// hold: no root, an expiration that is not a date, neither PUT nor CALL, or
// a strike that is negative or longer than eight digits.
func occCompact(o schwab.OCC) string {
	exp, err := time.Parse(time.DateOnly, string(o.Expiration))
	if o.Root == "" || err != nil || o.StrikeThousandths < 0 || o.StrikeThousandths > 99_999_999 {
		return ""
	}
	var side string
	switch o.PutCall {
	case "PUT":
		side = "P"
	case "CALL":
		side = "C"
	default:
		return ""
	}
	strike := strconv.FormatInt(o.StrikeThousandths, 10)
	return o.Root + exp.Format("060102") + side + strings.Repeat("0", 8-len(strike)) + strike
}

// classShare is a class share's symbol as Schwab may write it: BRK.B, BRK/B
// or BRK B.
var classShare = regexp.MustCompile(`^([A-Z]{1,5})[./ ]([A-Z]{1,2})$`)

// yahooForm returns a ticker as Yahoo and Ghostfolio write it: a class
// share's BRK.B, BRK/B or BRK B as BRK-B, and any other ticker unchanged.
func yahooForm(symbol string) string {
	return classShare.ReplaceAllString(symbol, "$1-$2")
}

// toolNamespace is the namespace of the tool exports' name-based UUIDs,
// ee8f3721-61a6-4223-b291-35fb2c5a4013: a random UUID of version 4, made
// for them once and never to change, since Ghostfolio knows accounts and
// assets by these UUIDs.
var toolNamespace = [16]byte{0xee, 0x8f, 0x37, 0x21, 0x61, 0xa6, 0x42, 0x23, 0xb2, 0x91, 0x35, 0xfb, 0x2c, 0x5a, 0x40, 0x13}

// uuid8 returns the name-based UUID of name in the namespace ns, of version
// 8 with SHA-256, as RFC 9562 makes a name-based UUID with a hash other
// than MD5 or SHA-1: the first 16 bytes of the SHA-256 of ns's 16 bytes
// and then name in UTF-8, with the version and the variant set. One name
// always gives one UUID, so an export names an account or an asset as every
// export before it did.
func uuid8(ns [16]byte, name string) string {
	h := sha256.New()
	h.Write(ns[:])
	h.Write([]byte(name))
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = b[6]&0x0f | 0x80
	b[8] = b[8]&0x3f | 0x80
	return uuidText(b)
}

// uuidText writes a UUID in its usual form: 32 lower-case hex digits in
// groups of 8, 4, 4, 4 and 12.
func uuidText(b [16]byte) string {
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// nyClock returns the New York date of the instant t and the time that New
// York's clocks show then, in seconds since midnight (0 to 86 399): 11:05:12
// is 39 912. On the days the clocks change it is the clock's reading that
// counts, not the seconds since midnight. The zero time has neither.
func nyClock(t time.Time) (nytime.Date, int) {
	if t.IsZero() {
		return "", 0
	}
	n := t.In(nytime.NewYork())
	return nytime.Date(n.Format(time.DateOnly)), n.Hour()*3600 + n.Minute()*60 + n.Second()
}

// gfInstant returns the instant that Ghostfolio is given for an activity of
// day: noon UTC of the day, plus offset milliseconds, which keep a day's
// activities in their order, as in 2025-01-21T12:00:39.912Z. It returns ""
// for a day that is not a date.
func gfInstant(day nytime.Date, offset int64) string {
	t, err := time.Parse(time.DateOnly, string(day))
	if err != nil {
		return ""
	}
	return t.Add(12*time.Hour + time.Duration(offset)*time.Millisecond).Format(instantLayout)
}
