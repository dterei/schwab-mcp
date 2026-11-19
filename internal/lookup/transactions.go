package lookup

import (
	"strconv"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// When returns when tx happened, for ordering: its time, or else its trade
// date.
func When(tx schwab.Transaction) time.Time {
	if !tx.Time.IsZero() {
		return tx.Time
	}
	return tx.TradeDate
}

// PrimaryItem returns the security leg of a transaction, skipping fee and
// cash legs. ok is false when there isn't one (a dividend paid in cash,
// say).
func PrimaryItem(tx schwab.Transaction) (item schwab.TransferItem, ok bool) {
	for _, it := range tx.TransferItems {
		if it.FeeType == "" && it.Instrument.AssetType != schwab.AssetCurrency && it.Instrument.Symbol != "" {
			return it, true
		}
	}
	return schwab.TransferItem{}, false
}

// Key identifies tx among the transactions of one account.
func Key(tx schwab.Transaction) string {
	if tx.ActivityID != 0 {
		return strconv.FormatInt(tx.ActivityID, 10)
	}
	return string(tx.Raw)
}

// NewYorkMidnight reports whether t is midnight in New York: the instant
// before it is on another date there.
func NewYorkMidnight(t time.Time) bool {
	return schwab.DateInNewYork(t) != schwab.DateInNewYork(t.Add(-time.Nanosecond))
}

// ProblemPlaces returns how many of the records have problems, and the
// first few places, each with its first problem, since one change at
// Schwab tends to repeat on every record; more is how many other places
// have problems. It returns the problems themselves, for the caller to
// word: their What may quote Schwab's values.
func ProblemPlaces(problems [][]schwab.Problem) (records int, first []schwab.Problem, more int) {
	seen := map[string]bool{}
	for _, ps := range problems {
		if len(ps) > 0 {
			records++
		}
		for _, p := range ps {
			if !seen[p.Path] {
				seen[p.Path] = true
				first = append(first, p)
			}
		}
	}
	const show = 3
	if len(first) > show {
		first, more = first[:show], len(first)-show
	}
	return records, first, more
}
