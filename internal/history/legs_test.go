package history

import (
	"testing"
)

// The legs view adds up (13.2 of the design): of every entry the view
// holds, API rows and the rows made for file transactions alike, with a
// day and an account on every leg and a time on every API leg (a file's
// row has a date alone), the legs' cost_cents sum to the
// entry's amount where a leg is of a security, and the CURRENCY legs'
// cash_cents sum to it elsewhere; but where the entry is flagged
// legs_mismatch or no_legs, which says it does not. The fixtures' API rows
// are the trades, dividends and transfers of two accounts; the file rows
// are a buy and a sell with fees, a dividend with a symbol, a wire with
// fees, and a credit of interest.
func TestLegsView(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	recordFixtures(t, s)
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260301-090000", "csv"), historyCSV(
		csvRow{"02/02/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "$1.25", "-$51.25"},
		csvRow{"02/03/2026", "Sell", "XYZ", "XYZ CORP", "2", "$12.00", "$0.65", "$23.35"},
		csvRow{"02/04/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"},
		csvRow{"02/05/2026", "Wire Sent", "", "WIRED FUNDS DISBURSED", "", "", "$15.00", "-$1015.00"},
		csvRow{"02/06/2026", "Credit Interest", "", "SCHWAB1 INT", "", "", "", "$0.07"},
	))
	for query, want := range map[string]int64{
		// Every entry but those flagged has legs, and both sources are
		// there, files with a security leg and without.
		`SELECT count(*) FROM entries e WHERE NOT EXISTS (SELECT 1 FROM legs l WHERE l.ref = e.ref AND l.account_id = e.account_id)
			AND NOT EXISTS (SELECT 1 FROM flags f WHERE f.subject_ref = e.ref AND f.code IN ('legs_mismatch', 'no_legs'))`: 0,
		`SELECT count(DISTINCT ref) FROM legs WHERE source = 'file' AND asset_type IS NOT 'CURRENCY'`: 2,
		`SELECT count(DISTINCT ref) FROM legs WHERE source = 'file' AND asset_type = 'CURRENCY'`:      5,
		`SELECT count(DISTINCT ref) FROM legs WHERE source = 'api'`:                                   14,
		// No leg lacks its day or its account, nor an API leg its time; a
		// file's row has a date alone.
		`SELECT count(*) FROM legs WHERE day IS NULL OR account_id IS NULL OR account IS NULL`: 0,
		`SELECT count(*) FROM legs WHERE source = 'api' AND time IS NULL`:                      0,
		// Each entry's legs add up to its amount.
		`SELECT count(*) FROM entries e JOIN (SELECT ref, account_id,
				max(asset_type IS NOT 'CURRENCY') AS security,
				sum(cost_cents) AS cost, sum(CASE WHEN asset_type = 'CURRENCY' THEN cash_cents END) AS cash
				FROM legs GROUP BY ref, account_id) l ON l.ref = e.ref AND l.account_id = e.account_id
			WHERE (CASE WHEN l.security THEN l.cost ELSE l.cash END) IS NOT e.amount_cents
			AND NOT EXISTS (SELECT 1 FROM flags f WHERE f.subject_ref = e.ref AND f.code IN ('legs_mismatch', 'no_legs'))`: 0,
	} {
		if got := storeInt(t, s, query); got != want {
			t.Errorf("%s: %d, want %d", query, got, want)
		}
	}
}
