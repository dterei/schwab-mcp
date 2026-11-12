package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// recordInput returns what the derivation reads of row, an API
// transaction of id id that one list read returned, as the store derives
// its columns and legs from its raw JSON; each security leg's security is
// its instrument ID, standing in for the grouping.
func recordInput(t *testing.T, id int64, row schwab.Transaction) txnInput {
	t.Helper()
	r, legs := txnFields(row, row.AccountNumber)
	var ps []txnProblem
	if err := json.Unmarshal([]byte(r.problems), &ps); err != nil {
		t.Fatal(err)
	}
	digest, err := cutDigest(row.Raw)
	if err != nil {
		t.Fatal(err)
	}
	activity, source := activityID(row)
	in := txnInput{
		id: id, firstRead: 1, latestListRead: 1, time: row.Time, tradeDate: row.TradeDate, tradeDay: row.TradeDay(),
		digests: [][32]byte{digest}, changed: [][]string{nil}, activityID: activity, idSource: source,
		typ: r.typ.String, status: r.status.String, description: r.description, text: row.Description,
		net: r.net, legs: legsOf(legs), problems: ps, numberSent: r.numberSent.String,
	}
	for i, l := range in.legs {
		if l.isSecurity() && legs[i].instrumentID.Valid {
			in.legs[i].security = sql.NullInt64{Int64: legs[i].instrumentID.Int64, Valid: true}
		}
	}
	return in
}

// centsOf returns c, a whole number of a unit, as a column.
func centsOf(c int64) sql.NullInt64 { return sql.NullInt64{Int64: c, Valid: true} }

// The fixtures' rows are booked by the rules as NEEDS 8.2 says: ...941,
// ...942 and ...947 sold, ...943 to ...946 bought, ...948 and ...950
// dividends, ...949 interest, ...951 to ...953 deposits and ...954 a tax;
// their fees are minus their cash legs' costs, named or not, and their
// gross amounts, before fees, are their security legs' costs, or the cash
// of a cash row. They check out: no flag but the security of ...948's
// symbol, which no security in the test holds.
func TestClassifyFixtures(t *testing.T) {
	t.Parallel()
	type want struct {
		rule, category, family string
		fees, gross            int64
		quantity               int64 // micros; 0 for none
		symbol                 string
	}
	golden := map[int64]want{
		73508261941: {"trade.sell", "sell", "sell", 69, 31200, -1_000_000, "XYZ   260918C00050000"},
		73508261942: {"trade.sell", "sell", "sell", 4, 60340, -1_000_000, "META"},
		73508261943: {"trade.buy", "buy", "buy", 0, -60546, 3_000_000, "AAPL"},
		73508261944: {"trade.buy", "buy", "buy", 67, -40000, 2_000_000, "XYZ   250221P00040000"},
		73508261945: {"trade.buy", "buy", "buy", 282, -105000, 1_000_000, "/ESH25_P5050:XCME"},
		73508261946: {"trade.reinvest", "buy", "buy", 0, -270, 37_800, "BND"},
		73508261947: {"trade.sell_fraction", "sell", "sell", 0, 8327, -451_300, "VBR"},
		73508261948: {"doi.dividend", "dividend", "income", 0, 2500, 0, "XYZ"},
		73508261949: {"doi.interest", "interest", "income", 0, 42, 0, ""},
		73508261950: {"doi.dividend", "dividend", "income", 0, 386, 0, "BND"},
		73508261951: {"cash.in", "deposit", "cash", 0, 1000000, 0, ""},
		73508261952: {"cash.electronic", "deposit", "cash", 0, 250000, 0, ""},
		73508261953: {"cash.in", "deposit", "cash", 0, 15000, 0, ""},
		73508261954: {"journal.w8", "tax", "tax", 0, -116, 0, "BND"},
	}
	in := accountInputs{account: 1}
	for i, row := range append(fixtureRows(t, individual), fixtureRows(t, options)...) {
		in.txns = append(in.txns, recordInput(t, int64(i+1), row))
	}
	// BND's security, as the grouping would give it, held its name then.
	in.names = []symbolName{{security: 4417302, value: "BND", first: midnight("2024-01-02"), last: midnight("2024-06-03")}}
	out := deriveAccount(in)
	if len(out.entries) != len(golden) {
		t.Fatalf("%d entries, want %d", len(out.entries), len(golden))
	}
	for _, e := range out.entries {
		var id int64
		if _, err := fmt.Sscanf(e.ref, "T%d", &id); err != nil {
			t.Fatalf("entry %s: %v", e.ref, err)
		}
		w, ok := golden[id]
		if !ok {
			t.Errorf("entry %s is not a fixture's", e.ref)
			continue
		}
		got := want{e.rule, e.category, e.family, e.fees.Int64, e.gross.Int64, e.quantity.Int64, e.symbol.String}
		if got != w || !e.fees.Valid || !e.gross.Valid || e.quantity.Valid != (w.quantity != 0) {
			t.Errorf("%s: %+v, want %+v", e.ref, got, w)
		}
		if e.amount.Int64+e.fees.Int64 != e.gross.Int64 || !e.counted || e.countedWhy != "api" || e.subject != "a:1:"+e.ref[1:] {
			t.Errorf("%s: amount %v, counted %v %s, subject %s", e.ref, e.amount, e.counted, e.countedWhy, e.subject)
		}
		var flags []string
		for _, f := range e.flags {
			flags = append(flags, f.code+" "+f.detail)
		}
		if wantFlags := []string(nil); id == 73508261948 {
			if !slices.Equal(flags, []string{"unresolved_security XYZ"}) {
				t.Errorf("%s is flagged %q, want only its symbol unresolved", e.ref, flags)
			}
		} else if !slices.Equal(flags, wantFlags) {
			t.Errorf("%s is flagged %q", e.ref, flags)
		}
		if id == 73508261950 && e.security != centsOf(4417302) {
			t.Errorf("%s's ~BND gives security %v", e.ref, e.security)
		}
	}
}

// leg returns a leg as the rules read it.
func leg(asset string, s sign) legClass { return legClass{assetType: asset, sign: s} }

// Each rule for API transactions (13.3 of the design) books what it
// matches, the first rule that matches deciding, and raises its flags; the
// wordings Schwab's answers hold are known to the rules.
func TestClassifyAPI(t *testing.T) {
	t.Parallel()
	cash := leg("CURRENCY", positive)
	none := sql.NullInt64{}
	cases := []struct {
		name                         string
		c                            apiClass
		rule, category, family, flag string
	}{
		{"an unknown type", apiClass{typ: "FOO", net: centsOf(1)}, "unknown.type", "other", "other", "unknown_type FOO"},
		{"no type", apiClass{net: centsOf(1)}, "unknown.type", "other", "other", "unknown_type "},
		{"a reinvestment", apiClass{typ: "TRADE", wording: "Dividend Reinvest", net: centsOf(-270), legs: []legClass{leg("COLLECTIVE_INVESTMENT", positive)}}, "trade.reinvest", "buy", "buy", ""},
		{"a buy", apiClass{typ: "TRADE", net: centsOf(-100), legs: []legClass{cash, leg("EQUITY", positive), leg("EQUITY", positive)}}, "trade.buy", "buy", "buy", ""},
		{"a fractional sale", apiClass{typ: "TRADE", wording: "Sell Trade", legs: []legClass{leg("COLLECTIVE_INVESTMENT", negative)}}, "trade.sell_fraction", "sell", "sell", ""},
		{"a sale", apiClass{typ: "TRADE", legs: []legClass{leg("OPTION", negative)}}, "trade.sell", "sell", "sell", ""},
		{"a spread paid for", apiClass{typ: "TRADE", net: centsOf(-50), legs: []legClass{leg("OPTION", positive), leg("OPTION", negative)}}, "trade.mixed", "buy", "buy", "mixed_legs "},
		{"a spread paid", apiClass{typ: "TRADE", net: centsOf(50), legs: []legClass{leg("OPTION", positive), leg("OPTION", negative)}}, "trade.mixed", "sell", "sell", "mixed_legs "},
		{"a spread off the grid", apiClass{typ: "TRADE", net: none, legs: []legClass{leg("OPTION", positive), leg("OPTION", negative)}}, "trade.mixed", "other", "other", "mixed_legs "},
		{"a trade of nothing", apiClass{typ: "TRADE", legs: []legClass{cash}}, "trade.none", "other", "other", "no_security_leg "},
		{"a trade of 0", apiClass{typ: "TRADE", legs: []legClass{leg("EQUITY", zero)}}, "trade.other", "other", "other", "zero_quantity "},
		{"a trade of 0 and 1", apiClass{typ: "TRADE", legs: []legClass{leg("EQUITY", positive), leg("EQUITY", zero)}}, "trade.other", "other", "other", "zero_quantity "},
		{"a trade off the grid", apiClass{typ: "TRADE", legs: []legClass{leg("EQUITY", unknownSign)}}, "trade.other", "other", "other", ""},
		{"a dividend", apiClass{typ: "DIVIDEND_OR_INTEREST", wording: "Qualified Dividend~SYM", legs: []legClass{cash}}, "doi.dividend", "dividend", "income", ""},
		{"a foreign tax", apiClass{typ: "DIVIDEND_OR_INTEREST", wording: "Foreign Tax Withheld~SYM"}, "doi.foreign_tax", "tax", "tax", "unknown_wording Foreign Tax Withheld~SYM"},
		{"an ADR fee", apiClass{typ: "DIVIDEND_OR_INTEREST", wording: "ADR Fee~SYM"}, "doi.adr_fee", "fee", "fee", "unknown_wording ADR Fee~SYM"},
		{"interest", apiClass{typ: "DIVIDEND_OR_INTEREST", wording: "SCHWAB1 INT 03/16-04/15"}, "doi.interest", "interest", "income", ""},
		{"other income of a symbol", apiClass{typ: "DIVIDEND_OR_INTEREST", wording: "Capital Gain~SYM"}, "doi.other_sym", "dividend", "income", "unknown_wording Capital Gain~SYM"},
		{"other income", apiClass{typ: "DIVIDEND_OR_INTEREST", wording: "BANK INT 12345678 SCHWAB BANK"}, "doi.other", "interest", "income", "unknown_wording BANK INT ...678 SCHWAB BANK"},
		{"W-8 on interest", apiClass{typ: "JOURNAL", wording: "W-8 Withholding", net: centsOf(-3)}, "journal.w8", "tax", "tax", ""},
		{"a W-8 refund", apiClass{typ: "JOURNAL", wording: "W-8 Withholding~SYM", net: centsOf(3)}, "journal.w8", "tax", "tax", ""},
		{"a sweep", apiClass{typ: "JOURNAL", wording: "BANK SWEEP TO BROKERAGE", net: centsOf(100)}, "journal.sweep", "other", "sweep", ""},
		{"a transfer between types", apiClass{typ: "JOURNAL", wording: "TRF FDS FRM TYPE 2", net: centsOf(100)}, "journal.sweep", "other", "sweep", ""},
		{"shares in", apiClass{typ: "JOURNAL", wording: "JOURNAL", net: centsOf(0), legs: []legClass{leg("EQUITY", positive)}}, "journal.shares", "transfer_in", "shares", ""},
		{"shares out", apiClass{typ: "JOURNAL", legs: []legClass{leg("EQUITY", negative)}}, "journal.shares", "transfer_out", "shares", ""},
		{"shares both ways", apiClass{typ: "JOURNAL", legs: []legClass{leg("EQUITY", negative), leg("EQUITY", positive)}}, "journal.shares", "other", "shares", ""},
		{"cash in", apiClass{typ: "JOURNAL", wording: "JOURNAL FRM ...123", net: centsOf(100), legs: []legClass{cash}}, "journal.cash", "deposit", "journal", ""},
		{"cash out", apiClass{typ: "JOURNAL", net: centsOf(-100)}, "journal.cash", "withdrawal", "journal", ""},
		{"no cash", apiClass{typ: "JOURNAL", net: centsOf(0)}, "journal.cash", "other", "journal", "zero_amount "},
		{"cash off the grid", apiClass{typ: "JOURNAL", net: none}, "journal.cash", "other", "journal", ""},
		{"an ACH in", apiClass{typ: "ACH_RECEIPT", net: centsOf(100)}, "cash.in", "deposit", "cash", ""},
		{"a wire in that took", apiClass{typ: "WIRE_IN", wording: "Wire Incoming", net: centsOf(-100)}, "cash.in", "withdrawal", "cash", "unexpected_sign "},
		{"funds received", apiClass{typ: "CASH_RECEIPT", wording: "Funds Received", net: centsOf(15000)}, "cash.in", "deposit", "cash", ""},
		{"a wire out", apiClass{typ: "WIRE_OUT", net: centsOf(-100)}, "cash.out", "withdrawal", "cash", ""},
		{"a disbursement that gave", apiClass{typ: "CASH_DISBURSEMENT", net: centsOf(100)}, "cash.out", "deposit", "cash", "unexpected_sign "},
		{"an ACH out", apiClass{typ: "ACH_DISBURSEMENT", net: centsOf(0)}, "cash.out", "withdrawal", "cash", ""},
		{"an electronic deposit", apiClass{typ: "ELECTRONIC_FUND", wording: "ACH Credit Received", net: centsOf(0)}, "cash.electronic", "deposit", "cash", ""},
		{"an electronic withdrawal", apiClass{typ: "ELECTRONIC_FUND", net: centsOf(-1)}, "cash.electronic", "withdrawal", "cash", ""},
		{"an expiration", apiClass{typ: "RECEIVE_AND_DELIVER", wording: "Removed due to Expiration XYZ 09/18/2026 50.00 C", legs: []legClass{leg("OPTION", positive)}}, "awaits_probe", "other", "holding", "awaits_probe expiration?"},
		{"shares delivered", apiClass{typ: "RECEIVE_AND_DELIVER", legs: []legClass{leg("EQUITY", negative)}}, "awaits_probe", "other", "holding", "awaits_probe out"},
		{"shares received", apiClass{typ: "RECEIVE_AND_DELIVER", legs: []legClass{leg("EQUITY", positive)}}, "awaits_probe", "other", "holding", "awaits_probe in"},
		{"a memorandum", apiClass{typ: "MEMORANDUM"}, "awaits_probe", "other", "other", "awaits_probe "},
		{"a margin call", apiClass{typ: "MARGIN_CALL"}, "awaits_probe", "other", "other", "awaits_probe "},
		{"a money market", apiClass{typ: "MONEY_MARKET"}, "awaits_probe", "other", "other", "awaits_probe "},
		{"an SMA adjustment", apiClass{typ: "SMA_ADJUSTMENT"}, "awaits_probe", "other", "other", "awaits_probe "},
	}
	for _, c := range cases {
		cl := classifyAPI(c.c)
		var flags []string
		for _, f := range cl.flags {
			flags = append(flags, f.code+" "+f.detail)
		}
		if cl.rule != c.rule || cl.category != c.category || cl.family != c.family || strings.Join(flags, "; ") != c.flag {
			t.Errorf("%s: %s %s %s %q, want %s %s %s %q", c.name, cl.rule, cl.category, cl.family, flags, c.rule, c.category, c.family, c.flag)
		}
	}

	// Every TRADE matches a trade rule, whatever its legs and net amount.
	signs := []sign{negative, zero, positive, unknownSign}
	var legSets [][]legClass
	legSets = append(legSets, nil, []legClass{cash})
	for _, a := range signs {
		legSets = append(legSets, []legClass{cash, leg("EQUITY", a)})
		for _, b := range signs {
			legSets = append(legSets, []legClass{leg("OPTION", a), leg("OPTION", b)})
			for _, c := range signs {
				legSets = append(legSets, []legClass{leg("OPTION", a), leg("OPTION", b), leg("EQUITY", c)})
			}
		}
	}
	for _, legs := range legSets {
		for _, net := range []sql.NullInt64{none, centsOf(-1), centsOf(0), centsOf(1)} {
			cl := classifyAPI(apiClass{typ: "TRADE", net: net, legs: legs})
			if !strings.HasPrefix(cl.rule, "trade.") || !slices.Contains(categories, cl.category) || cl.family == "" {
				t.Errorf("a TRADE of legs %v and net %v: %+v", legs, net, cl)
			}
		}
	}

	// The wordings that Schwab's answers hold, with the types they come
	// with, are known.
	types := map[string]string{
		"Dividend Reinvest": "TRADE", "Sell Trade": "TRADE", "Ordinary Dividend~SYM": "DIVIDEND_OR_INTEREST",
		"W-8 Withholding~SYM": "JOURNAL", "Free Balance Interest Adjustment": "DIVIDEND_OR_INTEREST",
		"Wire Incoming": "WIRE_IN", "ACH Credit Received": "ELECTRONIC_FUND", "Funds Received": "CASH_RECEIPT",
		"BANK SWEEP TO BROKERAGE": "JOURNAL", "TRF FDS TO TYPE #": "JOURNAL", "TRF FDS FRM TYPE #": "JOURNAL",
	}
	if len(types) != len(knownWordings) {
		t.Fatalf("the test knows %d wordings, classify.go %d", len(types), len(knownWordings))
	}
	for _, w := range knownWordings {
		typ, ok := types[w]
		if !ok {
			t.Fatalf("the test does not know the type of %q", w)
		}
		s := positive
		if w == "Sell Trade" {
			s = negative
		}
		cl := classifyAPI(apiClass{typ: typ, wording: strings.ReplaceAll(w, "#", "1"), net: centsOf(1), legs: []legClass{leg("EQUITY", s)}})
		if cl.flags.has("unknown_wording") || cl.flags.has("unknown_type") || cl.rule == "doi.other" || cl.rule == "doi.other_sym" {
			t.Errorf("%q, a known wording, is classified %+v", w, cl)
		}
	}
}

// What the rules read of a transaction does not change when Schwab adds a
// key, an instrument's closing price or symbol changes, a dividend's
// ticker is restated, or the JSON's white space or key order differs; it
// changes with the status, the type, the net amount or a leg's sign. The
// description the rules read from the raw JSON is the record's.
func TestClassInputs(t *testing.T) {
	t.Parallel()
	var base []byte
	for _, r := range fixtureRows(t, individual) {
		if r.ActivityID == 73508261946 {
			base = r.Raw
		}
	}
	if base == nil {
		t.Fatal("no fixture row 73508261946")
	}
	class := func(raw []byte) apiClass { return recordClass(decodeRow(t, string(raw))) }
	want := class(base)
	var reordered map[string]any
	if err := json.Unmarshal(base, &reordered); err != nil {
		t.Fatal(err)
	}
	compact, err := json.Marshal(reordered) // keys sorted, no white space
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"a key added":                replaceOnce(t, base, `"subAccount": "CASH",`, `"subAccount": "CASH", "qualifiedDividend": true,`),
		"another closing price":      replaceOnce(t, base, `"closingPrice": 71.59`, `"closingPrice": 70.1`),
		"a symbol become its CUSIP":  replaceOnce(t, base, `"symbol": "BND"`, `"symbol": "921937835"`),
		"white space and key order":  compact,
		"the price written longer":   replaceOnce(t, base, `"cost": -2.7`, `"cost": -2.70`),
		"a ticker restated after ~":  []byte(strings.Replace(string(dividend(t)), "~BND", "~VBR", 1)),
		"the same dividend as it is": dividend(t),
	} {
		got := class(raw)
		if name == "a ticker restated after ~" || name == "the same dividend as it is" {
			if !got.equal(class(dividend(t))) {
				t.Errorf("%s: %+v, want the dividend's", name, got)
			}
			continue
		}
		if !got.equal(want) {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
	for name, raw := range map[string][]byte{
		"another status":     replaceOnce(t, base, `"status": "VALID"`, `"status": "PENDING"`),
		"another type":       replaceOnce(t, base, `"type": "TRADE"`, `"type": "JOURNAL"`),
		"another net amount": replaceOnce(t, base, `"netAmount": -2.7`, `"netAmount": -2.71`),
		"a leg's sign":       replaceOnce(t, base, `"amount": 0.0378`, `"amount": -0.0378`),
		"another wording":    replaceOnce(t, base, `"Dividend Reinvest"`, `"Dividend Reinvested"`),
	} {
		if class(raw).equal(want) {
			t.Errorf("%s: the rules read the same", name)
		}
	}
	for _, raw := range []string{
		string(base),
		`{"description": "A", "description": "B"}`,
		`{"description": "A", "description": 5}`,
		`{"Description": "A"}`,
		`{"description": 5}`,
		`[1]`,
		`{"description": "A", "x": }`,
	} {
		if got, want := rawDescription(raw), decodeTxn(raw).Description; got != want {
			t.Errorf("rawDescription(%s) = %q, the record reads %q", raw, got, want)
		}
	}
}

// dividend returns the raw JSON of the fixtures' dividend of BND.
func dividend(t *testing.T) []byte {
	t.Helper()
	for _, r := range fixtureRows(t, individual) {
		if r.ActivityID == 73508261950 {
			return r.Raw
		}
	}
	t.Fatal("no fixture row 73508261950")
	return nil
}

// historyActions are the Action labels of CSV 3 of the research, every
// label seen in a real export among them, and the family of each.
var historyActions = map[string]string{
	"Buy": "buy", "Sell": "sell", "Sell Short": "sell", "Buy to Cover": "buy", "Buy to Open": "buy",
	"Sell to Close": "sell", "Sell to Open": "sell", "Buy to Close": "buy", "Reinvest Shares": "buy",
	"Cancel Buy": "reversal", "Cancel Sell": "reversal",
	"Qualified Dividend": "income", "Cash Dividend": "income", "Non-Qualified Div": "income",
	"Special Dividend": "income", "Special Qual Div": "income", "Special Non Qual Div": "income",
	"Pr Yr Cash Div": "income", "Pr Yr Special Div": "income", "Pr Yr Non-Qual Div": "income",
	"Pr Yr Non Qual Div": "income", "Reinvest Dividend": "income", "Qual Div Reinvest": "income",
	"Pr Yr Div Reinvest": "income", "Long Term Cap Gain Reinvest": "income", "Short Term Cap Gain Reinvest": "income",
	"Long Term Cap Gain": "income", "Short Term Cap Gain": "income", "Div Adjustment": "income",
	"Qual Div Reinvest Adj": "income", "Reinvestment Adj": "income", "Return Of Capital": "income",
	"Credit Interest": "income", "Bank Interest": "income", "Bond Interest": "income", "Margin Interest": "income",
	"Interest Adj": "income", "Promotional Award": "other", "Schwab Sub Inc Cr": "other",
	"NRA Tax Adj": "tax", "NRA Withholding": "tax", "NRA Withhold": "tax", "Pr Yr NRA Tax": "tax",
	"Foreign Tax Paid": "tax", "Foreign Tax Reclaim": "tax", "Foreign Tax Reclaim Adj": "tax", "IRS Withhold Adj": "tax",
	"ADR Mgmt Fee": "fee", "Service Fee": "fee", "Advisor Fee": "fee", "Misc Cash Entry": "other",
	"Adjustment": "other", "Misc Credits": "other", "Wire Fee": "fee", "Auto S1 Fee": "fee",
	"MoneyLink Transfer": "cash", "MoneyLink Deposit": "cash", "MoneyLink Adj": "cash", "Wire Sent": "cash",
	"Wire Funds": "cash", "Wire Received": "cash", "Wire Funds Received": "cash", "Wire Funds Adj": "cash",
	"Funds Received": "cash", "Funds Paid": "cash", "Returned Check": "cash", "Auto S1 Debit": "cash",
	"Auto S1 Credit": "cash", "Visa Purchase": "cash", "Journal": "journal", "Bank Transfer": "cash",
	"Futures MM Sweep": "sweep", "Internal Transfer": "journal", "Security Transfer": "cash",
	"Journaled Shares": "shares", "Stock Plan Activity": "shares", "Stock Split": "holding",
	"Reverse Split": "holding", "Stock Merger": "holding", "Name Change": "holding", "Conversion": "holding",
	"Spin-off": "holding", "Stock Div Dist": "holding", "Cash In Lieu": "proceeds", "Cash Merger": "proceeds",
	"Cash Merger Adj": "holding", "Full Redemption Adj": "proceeds", "Full Redemption": "holding",
	"Expired": "holding", "Assigned": "holding", "Exchange or Exercise": "holding", "Exercised": "holding",
	"Options Frwd Split": "holding", "Options Frwd Split Adj": "holding", "Received": "shares",
}

// Each action of History exports lands in its family (13.4 of the design),
// however it is spelled; Journal, Internal Transfer and Security Transfer
// move shares when they have a quantity; cash of 0.00 is booked as other;
// a cancel's quantity is signed against the trade it cancels, whatever sign
// the file wrote; an action not known is other, flagged.
func TestClassifyFile(t *testing.T) {
	t.Parallel()
	for action, family := range historyActions {
		c := classifyFile(fileClass{action: normalAction(action), amount: centsOf(-100)})
		if c.family != family || c.rule != "file."+family || !slices.Contains(categories, c.category) {
			t.Errorf("%s: %+v, want family %s", action, c, family)
		}
		for _, spelled := range []string{strings.ToUpper(action), strings.ReplaceAll(action, " ", "  "), " " + action + " "} {
			if d := classifyFile(fileClass{action: normalAction(spelled), amount: centsOf(-100)}); d.rule != c.rule || d.category != c.category {
				t.Errorf("%q is %+v, but %q is %+v", spelled, d, action, c)
			}
		}
	}
	if len(fileActions) != len(historyActions)-1 { // the two spellings of Pr Yr Non-Qual Div are one
		t.Errorf("the rules know %d actions, the research %d", len(fileActions), len(historyActions)-1)
	}
	flagged := func(c classification) string {
		var fs []string
		for _, f := range c.flags {
			fs = append(fs, f.code)
		}
		return strings.Join(fs, ",")
	}
	for _, c := range []struct {
		action      string
		hasQuantity bool
		quantity    sign
		amount      sql.NullInt64
		rule        string
		category    string
		flags       string
	}{
		{"Journal", true, positive, sql.NullInt64{}, "file.shares", "transfer_in", ""},
		{"Journal", true, negative, centsOf(0), "file.shares", "transfer_out", ""},
		{"Internal Transfer", false, zero, centsOf(500), "file.journal", "deposit", ""},
		{"Journal", false, zero, centsOf(-500), "file.journal", "withdrawal", ""},
		{"Journal", false, zero, centsOf(0), "file.journal", "other", "zero_amount"},
		{"Security Transfer", true, negative, centsOf(0), "file.shares", "transfer_out", ""},
		{"Security Transfer", false, zero, centsOf(700), "file.cash", "deposit", ""},
		{"MoneyLink Transfer", false, zero, centsOf(0), "file.cash", "other", "zero_amount"},
		{"Wire Sent", false, zero, centsOf(-700), "file.cash", "withdrawal", ""},
		{"Journaled Shares", false, zero, centsOf(0), "file.shares", "other", ""},
		{"Cancel Buy", true, negative, centsOf(1000), "file.reversal", "buy", ""},
		{"Cancel Sell", true, positive, centsOf(-1000), "file.reversal", "sell", ""},
		{"Return Of Capital", false, zero, centsOf(10), "file.income", "dividend", "return_of_capital"},
		{"Bank Interest", false, zero, centsOf(10), "file.income", "interest", ""},
		{"Cash Merger", false, zero, centsOf(10), "file.proceeds", "sell", "two_row_event"},
		{"Cash In Lieu", false, zero, centsOf(10), "file.proceeds", "sell", ""},
		{"Cash Merger Adj", true, negative, centsOf(0), "file.holding", "other", "two_row_event"},
		{"Expired", true, negative, centsOf(0), "file.holding", "other", ""},
		{"Futures MM Sweep", false, zero, centsOf(10), "file.sweep", "other", ""},
		{"Misc Cash Entry", false, zero, centsOf(10), "file.other", "other", "other_action"},
		{"Frobnicate", false, zero, centsOf(10), "file.unknown", "other", "unknown_action"},
	} {
		got := classifyFile(fileClass{action: normalAction(c.action), hasQuantity: c.hasQuantity, quantity: c.quantity, amount: c.amount})
		if got.rule != c.rule || got.category != c.category || flagged(got) != c.flags {
			t.Errorf("%s (quantity %v %d, amount %v): %s %s %q, want %s %s %q", c.action, c.hasQuantity, c.quantity, c.amount,
				got.rule, got.category, flagged(got), c.rule, c.category, c.flags)
		}
	}
	for _, c := range []struct {
		action  string
		written int64
		want    int64
	}{
		{"Cancel Buy", 10, -10},
		{"Cancel Buy", -10, -10},
		{"Cancel Sell", -10, 10},
		{"Cancel Sell", 10, 10},
		{"Buy", 5, 5},
		{"Buy", -5, 5},
		{"Reinvest Shares", 3, 3},
		{"Sell", 5, -5},
		{"Sell to Close", -5, -5},
		{"Journaled Shares", -3, -3},
		{"Journaled Shares", 3, 3},
		{"Expired", -1, -1},
		{"Assigned", 1, 1},
	} {
		if got := signedQuantity(c.action, c.written); got != c.want {
			t.Errorf("%s of %d is %d, want %d", c.action, c.written, got, c.want)
		}
	}
}
