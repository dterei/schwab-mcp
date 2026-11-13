package schwabfile

import (
	"regexp"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// optionSymbol is an option's symbol as History and Realized Gain/Loss
// write it: the root, the expiration as MM/DD/YYYY, the strike in dollars
// and C or P, separated by spaces, as in "XYZ 09/04/2026 17.50 P".
var optionSymbol = regexp.MustCompile(`^([A-Z0-9]{1,6}) +([0-9]{1,2}/[0-9]{1,2}/[0-9]{4}) +([0-9]{1,5})(?:\.([0-9]*))? +([CP])$`)

// ParseOption returns the contract that an option's symbol in a History
// or Realized Gain/Loss export names, such as "XYZ 09/04/2026 17.50 P":
// the root, the expiration as MM/DD/YYYY, the strike in dollars and C or
// P, separated by spaces, trimmed. Its contract is the one that
// schwab.ParseOCC gives for the OCC symbol of the same option. It reports
// false for anything else, such as a stock's symbol or an expiration that
// is not a date, and for what an OCC symbol cannot write: an expiration
// outside 1969 to 2068, whose two digits of year ParseOCC would read as
// another year, or a strike finer than a thousandth of a dollar, or of
// $100,000 or more.
func ParseOption(symbol string) (schwab.OCC, bool) {
	m := optionSymbol.FindStringSubmatch(strings.TrimSpace(symbol))
	if m == nil {
		return schwab.OCC{}, false
	}
	exp, err := time.Parse("1/2/2006", m[2])
	if err != nil || exp.Year() < 1969 || exp.Year() > 2068 {
		return schwab.OCC{}, false
	}
	frac := strings.TrimRight(m[4], "0")
	if len(frac) > 3 {
		return schwab.OCC{}, false
	}
	frac += strings.Repeat("0", 3-len(frac))
	var strike int64
	for _, c := range m[3] + frac {
		strike = strike*10 + int64(c-'0')
	}
	putCall := "CALL"
	if m[5] == "P" {
		putCall = "PUT"
	}
	return schwab.OCC{
		Root:              m[1],
		Expiration:        schwab.Date(exp.Format(time.DateOnly)),
		PutCall:           putCall,
		StrikeThousandths: strike,
	}, true
}
