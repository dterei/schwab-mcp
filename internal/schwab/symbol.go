package schwab

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// An OCC is an option contract as its OCC symbol names it.
type OCC struct {
	Root       string // the root without its padding, such as XYZ or SPXW
	Expiration Date
	PutCall    string // PUT or CALL, as an option's Instrument writes it
	// StrikeThousandths is the strike in thousandths of a dollar, as the
	// symbol writes it: 45000 for $45.
	StrikeThousandths int64
}

// ParseOCC returns the contract that an OCC option symbol names, such as
// XYZ   260619P00045000: the root, padded with spaces to six characters, the
// expiration as YYMMDD, C or P, and the strike in thousandths of a dollar,
// eight digits. It reports false for anything else, an expiration that is
// not a date included.
func ParseOCC(symbol string) (OCC, bool) {
	m := occSymbol.FindStringSubmatch(symbol)
	if m == nil {
		return OCC{}, false
	}
	exp, err := time.Parse("060102", m[2])
	if err != nil {
		return OCC{}, false
	}
	strike, err := strconv.ParseInt(m[4], 10, 64)
	if err != nil {
		return OCC{}, false
	}
	putCall := "CALL"
	if m[3] == "P" {
		putCall = "PUT"
	}
	return OCC{
		Root:              strings.TrimRight(m[1], " "),
		Expiration:        Date(exp.Format(time.DateOnly)),
		PutCall:           putCall,
		StrikeThousandths: strike,
	}, true
}

// IsCUSIP reports whether s is a CUSIP: eight letters and digits and a
// check digit that agrees with them, as the CUSIP's own algorithm gives it.
// Schwab sends a delisted security's CUSIP in place of its symbol.
func IsCUSIP(s string) bool {
	if len(s) != 9 {
		return false
	}
	sum := 0
	for i := range 8 {
		var v int
		switch c := s[i]; {
		case '0' <= c && c <= '9':
			v = int(c - '0')
		case 'A' <= c && c <= 'Z':
			v = int(c-'A') + 10
		default:
			return false
		}
		if i%2 == 1 {
			v *= 2
		}
		sum += v/10 + v%10
	}
	return s[8] == byte('0'+(10-sum%10)%10)
}

// SymbolAfterTilde returns the symbol after the last "~" of a
// transaction's description, and whether there is one: "BND" for "W-8
// Withholding~BND". A dividend's or a withholding's security is named only
// there, since its only leg is cash. What follows the "~" must be a symbol
// as Schwab writes one, so that "W-8 Withholding~ACME BANK" names none.
func SymbolAfterTilde(description string) (string, bool) {
	i := strings.LastIndex(description, "~")
	if i < 0 || !isSymbol(description[i+1:]) {
		return "", false
	}
	return description[i+1:], true
}

// isSymbol reports whether s is a symbol: a stock's, a fund's or an
// index's; an OCC option symbol, whose expiration is a date; a future's,
// or an option's on one; cash's; or a CUSIP, which Schwab sends in place of
// the symbol of a delisted security.
func isSymbol(s string) bool {
	if _, ok := ParseOCC(s); ok {
		return true
	}
	return plainSymbol.MatchString(s) || futureSymbol.MatchString(s) || currencySymbol.MatchString(s) || IsCUSIP(s)
}

var (
	// plainSymbol is the symbol of a stock, a fund or an index, such as
	// AAPL, SWVXX, BRK.B, BRK/B or $SPX.
	plainSymbol = regexp.MustCompile(`^\$?[A-Z][A-Z0-9]{0,5}(?:[./][A-Z]{1,3})?$`)
	// occSymbol is an OCC option symbol: the root, padded with spaces to
	// six characters, the expiration as YYMMDD, C or P, and the strike in
	// thousandths of a dollar, eight digits, as in XYZ   260619P00045000.
	occSymbol = regexp.MustCompile(`^(` + occRoot + `)([0-9]{6})([CP])([0-9]{8})$`)
	// futureSymbol is the symbol of a future, or of an option on one, as
	// Schwab writes it: /ESH25:XCME, /ESH25_P5050:XCME, and .ESH25:XCME for
	// such an option's underlying.
	futureSymbol = regexp.MustCompile(`^[/.][A-Z][A-Z0-9]{0,3}[FGHJKMNQUVXZ][0-9]{2}(?:_[CP][0-9]{1,6}(?:\.[0-9]{1,4})?)?:[A-Z]{4}$`)
	// currencySymbol is cash's symbol, such as CURRENCY_USD.
	currencySymbol = regexp.MustCompile(`^CURRENCY_[A-Z]{3}$`)
)

// occRoot is an OCC option symbol's root, padded with spaces to six
// characters.
const occRoot = `(?:[A-Z0-9]{6}|[A-Z0-9]{5} |[A-Z0-9]{4} {2}|[A-Z0-9]{3} {3}|[A-Z0-9]{2} {4}|[A-Z0-9] {5})`
