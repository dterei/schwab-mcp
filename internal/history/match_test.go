package history

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// A matchWorld is an account's API transactions, files and decisions, on
// values, for the matcher's tests.
type matchWorld struct {
	api       []ledgerEntry
	txns      []txnInput
	files     []*fileInput
	decisions []decisionInput
	runs      []coverageRun
	complete  []schwabfile.Coverage
	policies  map[string]string
	secs      *securities
}

// addAPI adds an API transaction of activity ID id, booked on day, of
// family and category, of amount in cents, and of symbol: with a quantity
// in shares (not 0), on a leg of its security, else as a dividend's ~SYM.
// It returns the entry, which the test may change before it runs.
func (w *matchWorld) addAPI(id int64, day, family, category string, amount int64, symbol string, quantity int64) *ledgerEntry {
	e := ledgerEntry{account: 1}
	e.txn = sql.NullInt64{Int64: id, Valid: true}
	e.ref, e.subject = "T"+strconv.FormatInt(id, 10), "a:1:"+strconv.FormatInt(id, 10)
	e.day = nullString(day)
	e.time = nullString(day + "T15:00:" + fmt.Sprintf("%02d", id%60) + ".000Z")
	e.family, e.category, e.rule = family, category, "test"
	e.amount, e.fees = centsOf(amount), centsOf(0)
	e.symbol = nullString(symbol)
	e.counted, e.countedWhy = true, "api"
	t := txnInput{id: id, activityID: id, tradeDay: nytime.Date(day)}
	if quantity != 0 {
		e.quantity = centsOf(quantity * 1e6)
		t.legs = []legInput{{assetType: "EQUITY", symbol: symbol, amountMicros: centsOf(quantity * 1e6)}}
	}
	w.api = append(w.api, e)
	w.txns = append(w.txns, t)
	return &w.api[len(w.api)-1]
}

// run derives the files' transactions and matches them, and returns the
// API entries and file entries as the matcher left them, and its matches
// and candidates.
func (w *matchWorld) run() (api, files []ledgerEntry, ms []matchOutput, cs []candidateOutput) {
	fl := deriveFiles(accountInputs{account: 1, files: w.files, fileDecisions: w.decisions, secs: w.secs}, nil, nil)
	api = slices.Clone(w.api)
	for i := range api {
		api[i].flags = slices.Clone(api[i].flags)
	}
	ms, cs = matchAccount(matchInput{
		api: api, txns: w.txns, files: fl.entries, fileTxns: fl.txns, decided: fl.decided, all: fl.all,
		policies: w.policies, apiRuns: w.runs, complete: w.complete,
	})
	return api, fl.entries, ms, cs
}

// shortRef shortens a file transaction's ref, Ca0000000.2, to Ca.2.
func shortRef(ref string) string {
	if !strings.HasPrefix(ref, "C") {
		return ref
	}
	_, ord, _ := strings.Cut(ref, ".")
	return ref[:2] + "." + ord
}

// matchesOf writes matches as "method Ca.1+Ca.2 = T1+T2", in order.
func matchesOf(api, files []ledgerEntry, ms []matchOutput) []string {
	refs := map[string]string{}
	for _, e := range files {
		refs[e.csvSubject] = shortRef(e.ref)
	}
	txns := map[int64]string{}
	for _, e := range api {
		txns[e.txn.Int64] = e.ref
	}
	var out []string
	for _, m := range ms {
		var fs, as []string
		for _, s := range m.files {
			fs = append(fs, refs[s])
		}
		for _, t := range m.txns {
			as = append(as, txns[t])
		}
		slices.Sort(fs)
		slices.Sort(as)
		out = append(out, m.method+" "+strings.Join(fs, "+")+" = "+strings.Join(as, "+"))
	}
	slices.Sort(out)
	return out
}

// countsOf writes each file entry as "Ca.1 counted_why", with the codes
// of its flags among codes, in the order of the entries' refs.
func countsOf(files []ledgerEntry, codes ...string) []string {
	var out []string
	for _, e := range files {
		words := []string{shortRef(e.ref), e.countedWhy}
		for _, f := range e.flags.sorted() {
			if slices.Contains(codes, f.code) {
				words = append(words, f.code)
			}
		}
		out = append(out, strings.Join(words, " "))
	}
	slices.Sort(out)
	return out
}

// A file transaction may be an API transaction of the families and days
// that its family allows (15.1 of the design): the same trade from a day
// before to three after; a reinvestment up to five days after; a cancel
// either trade; income, tax and fees a day before to five after, tax with
// income too, fees with income, cash and other; cash movements, journals
// and sweeps with one another, two days before to three after; share
// movements, holdings and proceeds with holdings, shares, trades, cash and
// other, two before to seven after; and what is other or unknown with
// anything, three before to seven after. A day outside the window, or a
// family outside the list, links nothing, and the file transaction,
// outside the API's coverage, counts.
func TestMatchFamiliesAndWindows(t *testing.T) {
	t.Parallel()
	const base = "2026-03-10"
	for _, c := range []struct {
		action string
		qty    int64
		family string // the API transaction's
		days   int    // the file's day less the API's
		want   bool
	}{
		{"Buy", 5, "buy", -1, true},
		{"Buy", 5, "buy", 3, true},
		{"Buy", 5, "buy", -2, false},
		{"Buy", 5, "buy", 4, false},
		{"Buy", 5, "sell", 0, false},
		{"Sell", 5, "sell", 2, true},
		{"Sell to Open", 1, "buy", 0, false},
		{"Reinvest Shares", 1, "buy", 0, true},
		{"Reinvest Shares", 1, "buy", 5, true},
		{"Reinvest Shares", 1, "buy", -1, false},
		{"Reinvest Shares", 1, "buy", 6, false},
		{"Cancel Buy", 5, "buy", 0, true},
		{"Cancel Sell", 5, "buy", 1, true},
		{"Cancel Buy", 5, "income", 0, false},
		{"Cancel Buy", 5, "sell", -1, true},
		{"Qualified Dividend", 0, "income", -1, true},
		{"Qualified Dividend", 0, "income", 5, true},
		{"Qualified Dividend", 0, "income", 6, false},
		{"Qualified Dividend", 0, "income", -2, false},
		{"Qualified Dividend", 0, "tax", 0, false},
		{"NRA Tax Adj", 0, "tax", 1, true},
		{"NRA Tax Adj", 0, "income", 0, true},
		{"NRA Tax Adj", 0, "fee", 0, false},
		{"Service Fee", 0, "fee", 0, true},
		{"Service Fee", 0, "income", 0, true},
		{"Service Fee", 0, "cash", 0, true},
		{"Service Fee", 0, "other", 0, true},
		{"Service Fee", 0, "buy", 0, false},
		{"MoneyLink Transfer", 0, "cash", -2, true},
		{"MoneyLink Transfer", 0, "journal", 3, true},
		{"MoneyLink Transfer", 0, "sweep", 0, true},
		{"MoneyLink Transfer", 0, "cash", -3, false},
		{"MoneyLink Transfer", 0, "cash", 4, false},
		{"MoneyLink Transfer", 0, "income", 0, false},
		{"Journal", 0, "cash", 0, true},
		{"Journal", 0, "cash", 3, true},
		{"Futures MM Sweep", 0, "sweep", 0, true},
		{"Futures MM Sweep", 0, "other", 0, false},
		{"Journaled Shares", 10, "shares", 0, true},
		{"Journaled Shares", 10, "holding", -2, true},
		{"Journaled Shares", 10, "holding", 7, true},
		{"Journaled Shares", 10, "holding", 8, false},
		{"Journaled Shares", 10, "holding", -3, false},
		{"Journal", 10, "buy", 0, true},
		{"Expired", 1, "other", 0, true},
		{"Expired", 1, "cash", 0, true},
		{"Expired", 1, "income", 0, false},
		{"Cash In Lieu", 0, "sell", 0, true},
		{"Cash In Lieu", 0, "journal", 0, false},
		{"Misc Cash Entry", 0, "income", -3, true},
		{"Misc Cash Entry", 0, "buy", 7, true},
		{"Misc Cash Entry", 0, "sweep", -4, false},
		{"Misc Cash Entry", 0, "sweep", 8, false},
		{"Something New", 0, "journal", 7, true},
		{"Something New", 0, "journal", 8, false},
	} {
		var w matchWorld
		f := testFile(1, "a", "2026-04-01", "", "")
		addRow(f, addDays(base, c.days), c.action, "XYZ", -5000, c.qty)
		w.files = []*fileInput{f}
		var qty int64
		if c.qty != 0 {
			qty = c.qty
		}
		w.addAPI(1, base, c.family, "other", -5000, "XYZ", qty)
		_, files, ms, _ := w.run()
		if got := files[0].countedWhy == "linked"; got != c.want || len(ms) != map[bool]int{true: 1, false: 0}[c.want] {
			t.Errorf("%s %d days after an API %s: linked %v, want %v", c.action, c.days, c.family, got, c.want)
		}
		if !c.want && files[0].countedWhy != "file" {
			t.Errorf("%s %d days after an API %s, not linked, counts %q", c.action, c.days, c.family, files[0].countedWhy)
		}
	}
}

// The hard cases of the matching (CSV 6.3 and 6.4): each a few rows, with
// what links and how.
func TestMatcher(t *testing.T) {
	t.Parallel()
	cover := []coverageRun{{source: "api", from: "2026-01-01", to: "2026-12-31"}}
	for _, c := range []struct {
		name string
		make func(w *matchWorld)
		want []string // the matches
		// counts are each file transaction's counting and flags among
		// codes; apiFlags each API transaction's flags among codes.
		counts, apiFlags []string
		codes            []string
	}{
		{
			name: "two identical trades and two identical fills stay two",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)
				addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
				w.addAPI(2, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
			},
			want:   []string{"exact Ca.1 = T1", "exact Ca.2 = T2"},
			counts: []string{"Ca.1 linked", "Ca.2 linked"},
		},
		{
			name: "three file rows and two fills leave one held out",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				for range 3 {
					addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)
				}
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
				w.addAPI(2, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
				w.runs = cover
			},
			want:   []string{"exact Ca.1 = T1", "exact Ca.2 = T2"},
			counts: []string{"Ca.1 linked", "Ca.2 linked", "Ca.3 held_out held_out"},
			codes:  []string{"held_out"},
		},
		{
			name: "one row of an order's fills",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -10000, 10)
				w.files = []*fileInput{f}
				for i, q := range []int64{4, 6} {
					w.addAPI(int64(i+1), "2026-03-10", "buy", "buy", -1000*q, "XYZ", q)
					w.txns[i].orderID = 77
				}
				w.addAPI(3, "2026-03-10", "buy", "buy", -10000, "XYZ", 9) // another quantity: not the row
			},
			want:   []string{"fills Ca.1 = T1+T2"},
			counts: []string{"Ca.1 linked merged_fills"},
			codes:  []string{"merged_fills"},
		},
		{
			name: "two orders whose fills fit leave the row to the steps after",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -10000, 10)
				w.files = []*fileInput{f}
				for i, q := range []int64{4, 6, 3, 7} {
					w.addAPI(int64(i+1), "2026-03-10", "buy", "buy", -1000*q, "XYZ", q)
					w.txns[i].orderID = 77 + int64(i/2)
				}
				w.runs = cover
			},
			counts: []string{"Ca.1 held_out"},
		},
		{
			name: "a spread written one row per leg is one API trade",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy to Open", "XYZ 09/18/2026 50.00 C", -30000, 1)
				addRow(f, "2026-03-10", "Sell to Open", "XYZ 09/18/2026 55.00 C", 10000, 1)
				w.files = []*fileInput{f}
				e := w.addAPI(1, "2026-03-10", "buy", "buy", -20000, "", 0)
				e.flags.add("mixed_legs", "")
				w.txns[0].legs = []legInput{
					{assetType: "OPTION", symbol: "XYZ   260918C00050000", amountMicros: centsOf(1e6)},
					{assetType: "OPTION", symbol: "XYZ   260918C00055000", amountMicros: centsOf(-1e6)},
				}
			},
			want:   []string{"legs Ca.1+Ca.2 = T1"},
			counts: []string{"Ca.1 linked merged_rows", "Ca.2 linked merged_rows"},
			codes:  []string{"merged_rows"},
		},
		{
			name: "a cash merger and its adjustment are one API row, which awaits the probes",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-12", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-12", "Cash Merger Adj", "ABC", 0, -100)
				w.files = []*fileInput{f}
				e := w.addAPI(1, "2026-03-10", "holding", "other", 150000, "ABC", -100)
				e.rule = "awaits_probe"
				e.flags.add("awaits_probe", "out")
			},
			want:     []string{"legs Ca.1+Ca.2 = T1"},
			counts:   []string{"Ca.1 linked merged_rows", "Ca.2 linked merged_rows"},
			codes:    []string{"merged_rows", "awaits_probe"},
			apiFlags: []string{"T1 awaits_probe out; the file calls it Cash Merger and Cash Merger Adj"},
		},
		{
			name: "a redemption and its adjustment, by CUSIP",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-16", "Full Redemption Adj", "000000AA7", 1000000, 0)
				addRow(f, "2026-03-16", "Full Redemption", "000000AA7", 0, -10000)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-16", "other", "other", 1000000, "000000AA7", -10000)
			},
			want:   []string{"legs Ca.1+Ca.2 = T1"},
			counts: []string{"Ca.1 linked", "Ca.2 linked"},
		},
		{
			name: "TD Ameritrade's history links when an API row fits, and counts inside the coverage when none does",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Credit Interest", "", 7, 0)
				addRow(f, "2026-03-11", "Credit Interest", "", 8, 0)
				for _, r := range f.rows {
					r.description = "TDA TRAN - INTEREST"
				}
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "interest", 7, "", 0)
				w.runs = cover
			},
			want:   []string{"exact Ca.1 = T1"},
			counts: []string{"Ca.1 linked tda_history", "Ca.2 tda tda_history"},
			codes:  []string{"tda_history", "held_out"},
		},
		{
			name: "a reinvestment two days after the dividend",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Qualified Dividend", "VTI", 1234, 0)
				addRow(f, "2026-03-12", "Reinvest Shares", "VTI", -1234, 1)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 1234, "VTI", 0)
				w.addAPI(2, "2026-03-10", "buy", "buy", -1234, "VTI", 1)
			},
			want:   []string{"exact Ca.1 = T1", "unique Ca.2 = T2"},
			counts: []string{"Ca.1 linked", "Ca.2 linked differs_from_api"},
			codes:  []string{"differs_from_api"},
		},
		{
			name: "expirations of no amount pair by their contracts",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-09-25", "", "")
				addRow(f, "2026-09-21", "Expired", "XYZ 09/18/2026 55.00 C", 0, 1)
				addRow(f, "2026-09-21", "Expired", "XYZ 09/18/2026 50.00 C", 0, 1)
				for _, r := range f.rows {
					r.asOf = "2026-09-18"
				}
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-09-18", "holding", "other", 0, "XYZ   260918C00050000", 1)
				w.addAPI(2, "2026-09-18", "holding", "other", 0, "XYZ   260918C00055000", 1)
			},
			want:   []string{"exact Ca.1 = T2", "exact Ca.2 = T1"},
			counts: []string{"Ca.1 linked", "Ca.2 linked"},
		},
		{
			name: "a tie stays uncertain, with its candidates",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-11", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
				w.addAPI(2, "2026-03-12", "income", "dividend", 500, "ABCD", 0)
				w.runs = cover
			},
			counts: []string{"Ca.1 uncertain uncertain_match"},
			codes:  []string{"uncertain_match", "held_out"},
		},
		{
			name: "your reject leaves the other",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-11", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
				w.addAPI(2, "2026-03-12", "income", "dividend", 500, "ABCD", 0)
				w.decisions = []decisionInput{{id: 1, kind: "reject", subject: "c:" + f.sha + ":1", value: "a:1:2"}}
			},
			want:   []string{"unique Ca.1 = T1"},
			counts: []string{"Ca.1 linked"},
		},
		{
			name: "a reject withdrawn",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
				w.decisions = []decisionInput{
					{id: 1, kind: "reject", subject: "c:" + f.sha + ":1", value: "a:1:1"},
					{id: 2, kind: "reject", subject: "c:" + f.sha + ":1", value: "auto"},
				}
			},
			want:   []string{"exact Ca.1 = T1"},
			counts: []string{"Ca.1 linked"},
		},
		{
			name: "your links: two file rows naming one API row are one match, whatever a reject says",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-12", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-12", "Stock Merger", "ABC", 0, -100)
				addRow(f, "2026-03-10", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "other", "other", 150000, "", 0)
				w.addAPI(2, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
				w.addAPI(3, "2026-03-10", "income", "dividend", 100, "ABCD", 0)
				w.decisions = []decisionInput{
					{id: 1, kind: "match", subject: "c:" + f.sha + ":1", value: "a:1:1"},
					{id: 2, kind: "match", subject: "c:" + f.sha + ":2", value: "a:1:1,a:1:9"},
					{id: 3, kind: "reject", subject: "c:" + f.sha + ":1", value: "a:1:1"},
					{id: 4, kind: "match", subject: "c:" + f.sha + ":3", value: "a:1:2,a:1:3"},
				}
			},
			want:   []string{"user Ca.1+Ca.2 = T1", "user Ca.3 = T2+T3"},
			counts: []string{"Ca.1 linked", "Ca.2 linked", "Ca.3 linked"},
		},
		{
			name: "a link to nothing, none, and a count of yours",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Qualified Dividend", "ABCD", 500, 0)
				addRow(f, "2026-03-10", "Qualified Dividend", "ABCD", 600, 0)
				addRow(f, "2026-03-10", "Qualified Dividend", "ABCD", 700, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
				w.addAPI(2, "2026-03-10", "income", "dividend", 600, "ABCD", 0)
				w.addAPI(3, "2026-03-10", "income", "dividend", 700, "ABCD", 0)
				w.decisions = []decisionInput{
					{id: 1, kind: "match", subject: "c:" + f.sha + ":1", value: "a:1:8"},
					{id: 2, kind: "match", subject: "c:" + f.sha + ":2", value: "none"},
					{id: 3, kind: "count", subject: "c:" + f.sha + ":3", value: "no"},
					{id: 4, kind: "match", subject: "c:" + f.sha + ":3", value: "a:1:3"},
				}
				w.runs = cover
			},
			want:   []string{"exact Ca.1 = T1"},
			counts: []string{"Ca.1 linked stale_decision", "Ca.2 user", "Ca.3 user"},
			codes:  []string{"stale_decision"},
		},
		{
			name: "a symbol changed: one security links them",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "NEW", -5000, 5)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "OLD", 5).security = centsOf(9)
				w.addAPI(2, "2026-03-10", "buy", "buy", -5000, "OTHER", 5).security = centsOf(8)
				w.secs = &securities{keys: map[string]secInfo{"s:FILE:NEW@2026-04-01": {id: 9}}}
			},
			want: []string{"exact Ca.1 = T1"},
		},
		{
			name: "an as-of date is the day; a withholding is tax; an interest the API calls a dividend disagrees",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-17", "Bank Interest", "", 42, 0)
				f.rows[0].asOf = "2026-03-15"
				addRow(f, "2026-03-10", "NRA Tax Adj", "ABCD", -150, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-15", "income", "dividend", 42, "", 0)
				w.addAPI(2, "2026-03-10", "tax", "tax", -150, "ABCD", 0)
			},
			want:     []string{"exact Ca.1 = T1", "exact Ca.2 = T2"},
			counts:   []string{"Ca.1 linked", "Ca.2 linked"},
			codes:    []string{"category_disagrees"},
			apiFlags: []string{"T1 category_disagrees Ca0000000.1 (Bank Interest) is booked as interest"},
		},
		{
			name: "a quantity on another split basis differs",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 10)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 1)
			},
			want:   []string{"unique Ca.1 = T1"},
			counts: []string{"Ca.1 linked differs_from_api"},
			codes:  []string{"differs_from_api"},
		},
		{
			name: "an API row a complete file lacks, and one before it",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
				w.addAPI(2, "2026-03-11", "income", "dividend", 300, "ABCD", 0)
				w.addAPI(3, "2026-02-27", "income", "dividend", 300, "ABCD", 0)
				w.complete = []schwabfile.Coverage{{From: "2026-03-01", To: "2026-03-31"}}
			},
			want:     []string{"exact Ca.1 = T1"},
			codes:    []string{"not_in_files"},
			apiFlags: []string{"T2 not_in_files"},
		},
		{
			name: "fills whose amounts fit but quantities do not are not the row",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -10000, 10)
				w.files = []*fileInput{f}
				for i, q := range []int64{4, 5} {
					w.addAPI(int64(i+1), "2026-03-10", "buy", "buy", -5000, "XYZ", q)
					w.txns[i].orderID = 77
				}
			},
			counts: []string{"Ca.1 file"},
		},
		{
			name: "one fill of an order, a day before, is a unique match, not fills",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-09", "buy", "buy", -5000, "XYZ", 5)
				w.txns[0].orderID = 77
			},
			want: []string{"unique Ca.1 = T1"},
		},
		{
			name: "a set of rows that two API rows fit is left; so are rows of other symbols or quantities",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-10", "Cash Merger Adj", "ABC", 0, -100)
				addRow(f, "2026-03-11", "Cash Merger", "DEF", 70000, 0)
				addRow(f, "2026-03-11", "Cash Merger Adj", "DEF", 0, -100)
				addRow(f, "2026-03-12", "Cash Merger", "GHI", 80000, 0)
				addRow(f, "2026-03-12", "Cash Merger Adj", "GHI", 0, -100)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "holding", "other", 150000, "ABC", -100)
				w.addAPI(2, "2026-03-10", "holding", "other", 150000, "ABC", -100)
				w.addAPI(3, "2026-03-11", "holding", "other", 70000, "XYZ", -100)
				w.addAPI(4, "2026-03-12", "holding", "other", 80000, "GHI", -50)
			},
			want: []string{"unique Ca.5 = T4"}, // the cash row alone, not with the other quantity
		},
		{
			name: "an API row that two sets fit is left to the steps after",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-10", "Cash Merger Adj", "ABC", 0, -100)
				addRow(f, "2026-03-11", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-11", "Cash Merger Adj", "ABC", 0, -100)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "holding", "other", 150000, "ABC", -100)
			},
			want: []string{"unique Ca.1 = T1"},
		},
		{
			name: "a row in two fitting sets is left",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-10", "Cash Merger Adj", "ABC", 0, -100)
				addRow(f, "2026-03-10", "Cash Merger Adj", "ABC", 0, -100)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "holding", "other", 150000, "ABC", -100)
			},
			want: []string{"unique Ca.1 = T1"},
		},
		{
			name: "two file rows whose best is one API row: neither is its strictly best",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-11", "Qualified Dividend", "ABCD", 500, 0)
				g := testFile(2, "b", "2026-04-02", "", "")
				addRow(g, "2026-03-09", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f, g}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
			},
			counts: []string{"Ca.1 uncertain", "Cb.1 uncertain"},
		},
		{
			name: "no amount needs one symbol; another symbol is never the row",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-09-21", "Expired", "XYZ 09/18/2026 50.00 C", 0, 1)
				addRow(f, "2026-03-11", "Buy", "XYZ", -5000, 5)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-09-18", "holding", "other", 0, "", 0)
				w.addAPI(2, "2026-03-10", "buy", "buy", -5000, "ABC", 5)
			},
			counts: []string{"Ca.1 file", "Ca.2 file"},
		},
		{
			name: "an API row that awaits the probes takes the file's action as a hint, not a disagreement",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Stock Plan Activity", "XYZ", 0, 10)
				w.files = []*fileInput{f}
				e := w.addAPI(1, "2026-03-10", "holding", "other", 0, "XYZ", 10)
				e.rule = "awaits_probe"
				e.flags.add("awaits_probe", "in")
			},
			want:     []string{"unique Ca.1 = T1"},
			codes:    []string{"awaits_probe", "category_disagrees"},
			apiFlags: []string{"T1 awaits_probe in; the file calls it Stock Plan Activity"},
		},
		{
			name: "a row whose only API row a better row took counts from the file, outside the coverage",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-11", "Qualified Dividend", "ABCD", 500, 0)
				addRow(f, "2026-03-12", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
			},
			want:   []string{"unique Ca.1 = T1"},
			counts: []string{"Ca.1 linked", "Ca.2 file"},
			codes:  []string{"uncertain_match"},
		},
		{
			name: "unique links again once a link leaves a row one choice",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-12", "Qualified Dividend", "ABCD", 500, 0)
				addRow(f, "2026-03-13", "Qualified Dividend", "ABCD", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0)
				w.addAPI(2, "2026-03-11", "income", "dividend", 500, "ABCD", 0)
			},
			want:   []string{"unique Ca.1 = T2", "unique Ca.2 = T1"},
			counts: []string{"Ca.1 linked", "Ca.2 linked"},
		},
		{
			name: "trades without an order ID are no order's fills",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -10000, 10)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
				w.addAPI(2, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
			},
			counts: []string{"Ca.1 file"},
		},
		{
			name: "fills outside the row's window, of another symbol, or one you rejected, are not its fills",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-17", "Buy", "XYZ", -10000, 10) // a week after order 7's fills
				addRow(f, "2026-03-20", "Buy", "XYZ", -20000, 10) // order 8's are of ABC
				addRow(f, "2026-03-24", "Buy", "XYZ", -30000, 10) // you rejected one of order 9's
				w.files = []*fileInput{f}
				for i, o := range []struct {
					order  int64
					day    string
					amount int64
					symbol string
				}{
					{7, "2026-03-10", -5000, "XYZ"},
					{7, "2026-03-10", -5000, "XYZ"},
					{8, "2026-03-20", -10000, "ABC"},
					{8, "2026-03-20", -10000, "ABC"},
					{9, "2026-03-24", -15000, "XYZ"},
					{9, "2026-03-24", -15000, "XYZ"},
				} {
					w.addAPI(int64(i+1), o.day, "buy", "buy", o.amount, o.symbol, 5)
					w.txns[i].orderID = o.order
				}
				w.decisions = []decisionInput{{id: 1, kind: "reject", subject: "c:" + f.sha + ":3", value: "a:1:6"}}
			},
			counts: []string{"Ca.1 file", "Ca.2 file", "Ca.3 file"},
		},
		{
			name: "legs outside a buy's window, or of an API row you rejected, are not its legs",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-14", "Buy to Open", "XYZ 09/18/2026 50.00 C", -30000, 1) // 4 days after T1
				addRow(f, "2026-03-14", "Sell to Open", "XYZ 09/18/2026 55.00 C", 10000, 1)
				addRow(f, "2026-03-20", "Cash Merger", "ABC", 150000, 0) // you rejected T2
				addRow(f, "2026-03-20", "Cash Merger Adj", "ABC", 0, -100)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -20000, "", 0)
				w.txns[0].legs = []legInput{
					{assetType: "OPTION", symbol: "XYZ   260918C00050000", amountMicros: centsOf(1e6)},
					{assetType: "OPTION", symbol: "XYZ   260918C00055000", amountMicros: centsOf(-1e6)},
				}
				w.addAPI(2, "2026-03-20", "holding", "other", 150000, "ABC", -100)
				w.decisions = []decisionInput{
					{id: 1, kind: "reject", subject: "c:" + f.sha + ":3", value: "a:1:2"},
					{id: 2, kind: "reject", subject: "c:" + f.sha + ":4", value: "a:1:2"},
				}
			},
			counts: []string{"Ca.1 file", "Ca.2 file", "Ca.3 file", "Ca.4 file"},
		},
		{
			name: "a holding's legs two days before the API row, and seven after",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-08", "Cash Merger", "ABC", 150000, 0)
				addRow(f, "2026-03-08", "Cash Merger Adj", "ABC", 0, -100)
				addRow(f, "2026-03-27", "Cash Merger", "DEF", 70000, 0)
				addRow(f, "2026-03-27", "Cash Merger Adj", "DEF", 0, -100)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "holding", "other", 150000, "ABC", -100)
				w.addAPI(2, "2026-03-20", "holding", "other", 70000, "DEF", -100)
			},
			want: []string{"legs Ca.1+Ca.2 = T1", "legs Ca.3+Ca.4 = T2"},
		},
		{
			name: "fees a cent apart, and prices beyond a hundredth of a percent, differ",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)
				addRow(f, "2026-03-11", "Buy", "XYZ", -6000, 5)
				addRow(f, "2026-03-12", "Buy", "XYZ", -7000, 5)
				f.rows[0].fees, f.rows[0].feesBlank = centsOf(1), false
				f.rows[1].price = sql.NullFloat64{Float64: 12.0024, Valid: true}
				f.rows[2].price = sql.NullFloat64{Float64: 14.0007, Valid: true}
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
				w.addAPI(2, "2026-03-11", "buy", "buy", -6000, "XYZ", 5).price = sql.NullFloat64{Float64: 12, Valid: true}
				w.addAPI(3, "2026-03-12", "buy", "buy", -7000, "XYZ", 5).price = sql.NullFloat64{Float64: 14, Valid: true}
			},
			want:   []string{"exact Ca.1 = T1", "exact Ca.2 = T2", "exact Ca.3 = T3"},
			counts: []string{"Ca.1 linked differs_from_api", "Ca.2 linked differs_from_api", "Ca.3 linked"},
			codes:  []string{"differs_from_api"},
		},
		{
			name: "fees and prices are compared one to one only",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-10", "Buy to Open", "XYZ 09/18/2026 50.00 C", -30000, 1)
				addRow(f, "2026-03-10", "Sell to Open", "XYZ 09/18/2026 55.00 C", 10000, 1)
				addRow(f, "2026-03-11", "Buy", "XYZ", -10000, 10)
				f.rows[2].price = sql.NullFloat64{Float64: 10, Valid: true}
				w.files = []*fileInput{f}
				e := w.addAPI(1, "2026-03-10", "buy", "buy", -20000, "", 0)
				e.fees = centsOf(65)
				w.txns[0].legs = []legInput{
					{assetType: "OPTION", symbol: "XYZ   260918C00050000", amountMicros: centsOf(1e6)},
					{assetType: "OPTION", symbol: "XYZ   260918C00055000", amountMicros: centsOf(-1e6)},
				}
				for i, p := range []float64{9.5, 10.5} {
					w.addAPI(int64(i+2), "2026-03-11", "buy", "buy", -5000, "XYZ", 5).price = sql.NullFloat64{Float64: p, Valid: true}
					w.txns[i+1].orderID = 77
				}
			},
			want:   []string{"fills Ca.3 = T2+T3", "legs Ca.1+Ca.2 = T1"},
			counts: []string{"Ca.1 linked", "Ca.2 linked", "Ca.3 linked"},
			codes:  []string{"differs_from_api"},
		},
		{
			name: "an API row your category booked does not disagree",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-15", "Bank Interest", "", 42, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-15", "income", "dividend", 42, "", 0).rule = "user"
			},
			want:  []string{"exact Ca.1 = T1"},
			codes: []string{"category_disagrees"},
		},
		{
			name: "an equal description breaks a tie",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-11", "Qualified Dividend", "ABCD", 500, 0)
				f.rows[0].description = "ABCD INC"
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "ABCD", 0).text = "Abcd  Inc"
				w.addAPI(2, "2026-03-12", "income", "dividend", 500, "ABCD", 0).text = "OTHER"
			},
			want: []string{"unique Ca.1 = T1"},
		},
		{
			name: "a row with a symbol links an API row without one",
			make: func(w *matchWorld) {
				f := testFile(1, "a", "2026-04-01", "", "")
				addRow(f, "2026-03-11", "Qualified Dividend", "XYZ", 500, 0)
				w.files = []*fileInput{f}
				w.addAPI(1, "2026-03-10", "income", "dividend", 500, "", 0)
			},
			want: []string{"unique Ca.1 = T1"},
		},
	} {
		var w matchWorld
		c.make(&w)
		api, files, ms, _ := w.run()
		if got := matchesOf(api, files, ms); !slices.Equal(got, c.want) {
			t.Errorf("%s: the matches are %q, want %q", c.name, got, c.want)
		}
		if c.counts != nil {
			if got := countsOf(files, c.codes...); !slices.Equal(got, c.counts) {
				t.Errorf("%s: the file transactions count %q, want %q", c.name, got, c.counts)
			}
		}
		var flags []string
		for _, e := range api {
			for _, f := range e.flags.sorted() {
				if slices.Contains(c.codes, f.code) {
					flags = append(flags, strings.TrimSpace(e.ref+" "+f.code+" "+f.detail))
				}
			}
		}
		if !slices.Equal(flags, c.apiFlags) {
			t.Errorf("%s: the API transactions are flagged %q, want %q", c.name, flags, c.apiFlags)
		}

		// The same input in another order gives the same matches.
		r := rand.New(rand.NewPCG(1, 2))
		order := r.Perm(len(w.api))
		var api2 []ledgerEntry
		var txns2 []txnInput
		for _, i := range order {
			api2 = append(api2, w.api[i])
			txns2 = append(txns2, w.txns[i])
		}
		w.api, w.txns = api2, txns2
		slices.Reverse(w.decisions)
		for _, f := range w.files {
			slices.Reverse(f.rows)
		}
		api, files, ms, _ = w.run()
		if got := matchesOf(api, files, ms); !slices.Equal(got, c.want) {
			t.Errorf("%s, in another order: the matches are %q, want %q", c.name, got, c.want)
		}
	}
}

// An uncertain file transaction keeps its best candidates, five at most,
// with their scores and why: a day nearer, the same symbol and quantity
// score more.
func TestMatchCandidates(t *testing.T) {
	t.Parallel()
	var w matchWorld
	f := testFile(1, "a", "2026-04-01", "", "")
	addRow(f, "2026-03-10", "Qualified Dividend", "ABCD", 700, 0)
	w.files = []*fileInput{f}
	for i, day := range []string{"2026-03-09", "2026-03-11", "2026-03-08", "2026-03-08", "2026-03-07", "2026-03-06", "2026-03-05"} {
		w.addAPI(int64(i+1), day, "income", "dividend", 700, "ABCD", 0)
	}
	_, files, ms, cs := w.run()
	if len(ms) != 0 {
		t.Errorf("an uncertain row is in %d matches", len(ms))
	}
	var got []string
	for _, c := range cs {
		got = append(got, fmt.Sprintf("%d %g %s", c.txn, c.score, c.why))
	}
	want := []string{
		"1 125 the same amount, a day apart, the same symbol and the same fees",
		"2 125 the same amount, a day apart, the same symbol and the same fees",
		"3 115 the same amount, 2 days apart, the same symbol and the same fees",
		"4 115 the same amount, 2 days apart, the same symbol and the same fees",
		"5 105 the same amount, 3 days apart, the same symbol and the same fees",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the candidates are %q, want %q", got, want)
	}
	if fl := flagsOf(files[0]); len(fl) != 1 || fl[0] != "uncertain_match it could be T1, T2, T3, T4 or T5; say which: schwabtx edit Ca0000000.1 -link T1, or -link none if none is" {
		t.Errorf("the uncertain row is flagged %q", fl)
	}
}

// Counting a file transaction (15.3 of the design), on values: linked, it
// never counts, and is flagged when no API transaction of its match does
// either; inside the API's coverage, from the day before its day to the
// day after its posted date, without a counterpart, it is held out (by a
// -json file's coverage too, said in the flag), unless your policy for its
// action counts it; a day short of the coverage, it counts; a deliberate
// miss inside the coverage (another amount) is held out, so nothing counts
// twice.
func TestCountFiles(t *testing.T) {
	t.Parallel()
	var w matchWorld
	f := testFile(1, "a", "2026-04-01", "", "")
	addRow(f, "2026-03-10", "Buy", "XYZ", -5000, 5)             // linked to T1, which is gone
	addRow(f, "2026-03-11", "Bank Interest", "", 42, 0)         // held out by api
	addRow(f, "2026-03-21", "Bank Interest", "", 43, 0)         // held out by api_file, where your policy counts it
	addRow(f, "2026-03-25", "Credit Interest", "", 44, 0)       // held out by api_file
	addRow(f, "2026-03-31", "Credit Interest", "", 45, 0)       // the day after it is not covered
	addRow(f, "2026-03-12", "Wire Funds", "", -100000, 0)       // a miss: the API's is a cent more
	addRow(f, "2026-03-05", "Qualified Dividend", "ABCD", 9, 0) // the day before it is not covered
	f.rows[2].asOf = "2026-03-20"
	w.files = []*fileInput{f}
	e := w.addAPI(1, "2026-03-10", "buy", "buy", -5000, "XYZ", 5)
	e.counted, e.countedWhy = false, "gone"
	w.addAPI(2, "2026-03-12", "cash", "withdrawal", -100001, "", 0)
	w.runs = []coverageRun{{source: "api", from: "2026-03-05", to: "2026-03-19"}, {source: "api_file", from: "2026-03-20", to: "2026-03-31"}}
	w.policies = map[string]string{"bankinterest": "count"}
	_, files, _, _ := w.run()
	got := countsOf(files, "held_out", "linked_api_uncounted")
	want := []string{
		"Ca.1 linked linked_api_uncounted", "Ca.2 policy", "Ca.3 policy", "Ca.4 held_out held_out", "Ca.5 file",
		"Ca.6 held_out held_out", "Ca.7 file",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the file transactions count %q, want %q", got, want)
	}
	for _, e := range files {
		if e.flags.has("held_out") && slices.Contains(flagsOf(e), "held_out api_file") != (shortRef(e.ref) == "Ca.4") {
			t.Errorf("%s is flagged %q", e.ref, flagsOf(e))
		}
	}
	if fl := flagsOf(files[0]); !slices.Contains(fl, "linked_api_uncounted T1 gone; to count it from the file: schwabtx edit Ca0000000.1 -link none") {
		t.Errorf("the row linked to a gone transaction is flagged %q", fl)
	}
	w.policies = nil
	_, files, _, _ = w.run()
	if got := countsOf(files); got[1] != "Ca.2 held_out" || got[2] != "Ca.3 held_out" {
		t.Errorf("without your policy, the bank interest counts %q", got[1:3])
	}
}

// What does not count pairs with nothing: a file's sweep that is an API
// transaction's, or a cancel held out, is no one's half, and is not
// flagged unpaired.
func TestUncountedDoNotPair(t *testing.T) {
	t.Parallel()
	entries := []ledgerEntry{
		pairEntry("S1", "sweep", "other", "2026-03-10", -100),
		pairEntry("S2", "sweep", "other", "2026-03-10", 100),
		pairEntry("S3", "sweep", "other", "2026-03-10", 100),
		pairEntry("C1", "reversal", "buy", "2026-03-11", 100),
	}
	entries[1].counted, entries[3].counted = false, false
	pairWithin(entries)
	finishPairs(entries, nil)
	if got := pairsOf(entries); got["1:S1"] != "other sweep 1:S3" || got["1:S2"] != "other  0:" || got["1:C1"] != "buy  0:" {
		t.Errorf("the pairs are %q", got)
	}
	cands := []pairCandidate{
		candidate(1, "J1", "2026-03-10", -100, ""), candidate(2, "J2", "2026-03-10", 100, ""), candidate(2, "J3", "2026-03-10", 100, ""),
	}
	cands[2].uncounted = true
	if got := pairAccounts(cands, nil); got[entryKey{1, "J1"}].with != (entryKey{2, "J2"}) || got[entryKey{2, "J3"}].flag != "" {
		t.Errorf("across accounts: %+v", got)
	}
}

// formatMicros writes millionths as a decimal without trailing zeros.
func TestFormatMicros(t *testing.T) {
	t.Parallel()
	for m, want := range map[int64]string{0: "0", 1e6: "1", -2500000: "-2.5", 1: "0.000001", 123456789: "123.456789"} {
		if got := formatMicros(m); got != want {
			t.Errorf("formatMicros(%d) = %q, want %q", m, got, want)
		}
	}
}

// apiCovers reads every day from the day before to the day after, by the
// API's coverage or a -json file's.
func TestAPICovers(t *testing.T) {
	t.Parallel()
	runs := []coverageRun{{source: "api", from: "2026-03-05", to: "2026-03-10"}, {source: "api_file", from: "2026-03-11", to: "2026-03-12"}}
	for _, c := range []struct {
		day, posted    nytime.Date
		inside, byFile bool
	}{
		{"2026-03-06", "2026-03-09", true, false},
		{"2026-03-05", "2026-03-05", false, false},
		{"2026-03-08", "2026-03-10", true, true},
		{"2026-03-10", "2026-03-12", false, false},
		{"2026-03-11", "2026-03-11", true, true},
	} {
		if in, by := apiCovers(runs, c.day, c.posted); in != c.inside || by != c.byFile {
			t.Errorf("apiCovers(%s, %s) = %v, %v, want %v, %v", c.day, c.posted, in, by, c.inside, c.byFile)
		}
	}
}
