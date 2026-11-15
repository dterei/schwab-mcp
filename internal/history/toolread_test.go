package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A toolStore is the store of the tool exports' tests (11.3 of the design),
// with what they name of it. Every value it holds is made up: the
// fixtures' and the files' accounts, refs, securities and amounts.
type toolStore struct {
	s *Store
	// individual and options are the ids of the fixtures' accounts that
	// hold transactions (...135 and ...398), files that of fileAccount
	// (...398 too), which only History files hold, and empty that of the
	// fixtures' third (...783), which only snapshots hold.
	individual, options, files, empty int64
	// history and matched are the History files that newToolStore imports:
	// fileAccount's, and the individual account's, whose rows the store
	// matches with the account's API rows.
	history, matched string
	// later and correction are the History files of fileAccount that
	// importLater imports: the first holds a row "as of" a day before it
	// was posted and a dividend, which the second corrects.
	later, correction string
}

// The instants of the API rows that newToolStore reads after the fixtures'
// window, in New York: a journal of cash out of the options account and
// its other side a day later, a journal of shares out of the individual
// account and its other side, and a spread of two options.
var (
	toolPaysAt     = time.Date(2026, time.September, 8, 10, 0, 0, 0, nytime.NewYork())
	toolReceivesAt = time.Date(2026, time.September, 9, 9, 30, 0, 0, nytime.NewYork())
	toolDeliversAt = time.Date(2026, time.September, 10, 10, 15, 0, 0, nytime.NewYork())
	toolTakesAt    = time.Date(2026, time.September, 10, 10, 16, 0, 0, nytime.NewYork())
	toolSpreadAt   = time.Date(2026, time.September, 15, 11, 20, 5, 0, nytime.NewYork())
)

// The evenings of newToolStore's snapshots, after the close in New York,
// and the morning of a snapshot taken during the second evening's session.
var (
	toolEvening1 = time.Date(2026, time.November, 9, 18, 5, 0, 0, nytime.NewYork())
	toolEvening2 = time.Date(2026, time.November, 10, 18, 5, 0, 0, nytime.NewYork())
	toolMorning  = time.Date(2026, time.November, 10, 11, 0, 0, 0, nytime.NewYork())
)

// toolSharesRaw returns the raw JSON of a JOURNAL of quantity shares of
// AAPL, the fixtures' instrument, in the account whose number is number,
// with activity ID id, at at, priced at price when it is not "".
func toolSharesRaw(id int64, number string, at time.Time, quantity float64, price, description string) string {
	if price != "" {
		price = `, "price": ` + price
	}
	return fmt.Sprintf(`{"activityId": %d, "time": %q, "description": %q, "accountNumber": %q, "type": "JOURNAL",
		"status": "VALID", "subAccount": "CASH", "tradeDate": %q, "netAmount": 0.0, "transferItems": [{"instrument":
		{"assetType": "EQUITY", "status": "ACTIVE", "symbol": "AAPL", "instrumentId": 1973757747, "closingPrice": 230.1,
		"type": "COMMON_STOCK"}, "amount": %v, "cost": 0.0%s}]}`,
		id, schwabTime(at), description, number, schwabTime(midnight(nytime.DateOf(at))), quantity, price)
}

// toolDividendRaw returns the raw JSON of a dividend of amount dollars of
// symbol in the individual account, with activity ID id, at at.
func toolDividendRaw(id int64, at time.Time, symbol string, amount float64) string {
	return fmt.Sprintf(`{"activityId": %d, "time": %q, "description": "Ordinary Dividend~%s", "accountNumber": %q,
		"type": "DIVIDEND_OR_INTEREST", "status": "VALID", "subAccount": "CASH", "tradeDate": %q, "netAmount": %v,
		"transferItems": [{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
		"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": %v, "cost": 0.0}]}`,
		id, schwabTime(at), symbol, individual, schwabTime(midnight(nytime.DateOf(at))), amount, amount)
}

// toolSpreadRaw is the raw JSON of a TRADE of the options account with two
// option legs, a put bought and a put sold, and two fee legs.
var toolSpreadRaw = fmt.Sprintf(`{"activityId": 73600000105, "time": %q, "accountNumber": %q, "type": "TRADE",
	"status": "VALID", "subAccount": "MARGIN", "tradeDate": %q, "orderId": 1004517700105, "netAmount": -131.34,
	"transferItems": [
	{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD", "description": "USD currency",
		"instrumentId": 1, "closingPrice": 0.0}, "amount": 1.3, "cost": -1.3, "feeType": "COMMISSION"},
	{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD", "description": "USD currency",
		"instrumentId": 1, "closingPrice": 0.0}, "amount": 0.04, "cost": -0.04, "feeType": "OPT_REG_FEE"},
	{"instrument": {"assetType": "OPTION", "status": "ACTIVE", "symbol": "XYZ   261016P00045000",
		"description": "XYZ 10/16/2026 45.00 P", "instrumentId": 219950001, "closingPrice": 2.05,
		"expirationDate": "2026-10-16T04:00:00+0000", "optionPremiumMultiplier": 100, "putCall": "PUT",
		"strikePrice": 45.0, "type": "VANILLA", "underlyingSymbol": "XYZ"},
		"amount": 1.0, "cost": -210.0, "price": 2.1, "positionEffect": "OPENING"},
	{"instrument": {"assetType": "OPTION", "status": "ACTIVE", "symbol": "XYZ   261016P00040000",
		"description": "XYZ 10/16/2026 40.00 P", "instrumentId": 219950002, "closingPrice": 0.75,
		"expirationDate": "2026-10-16T04:00:00+0000", "optionPremiumMultiplier": 100, "putCall": "PUT",
		"strikePrice": 40.0, "type": "VANILLA", "underlyingSymbol": "XYZ"},
		"amount": -1.0, "cost": 80.0, "price": 0.8, "positionEffect": "OPENING"}]}`,
	schwabTime(toolSpreadAt), options, schwabTime(midnight(nytime.DateOf(toolSpreadAt))))

// toolNoDayRaw is the raw JSON of a dividend of the options account with
// neither a trade date nor a time, which has no day.
var toolNoDayRaw = fmt.Sprintf(`{"activityId": 73600000106, "description": "Ordinary Dividend~META", "accountNumber": %q,
	"type": "DIVIDEND_OR_INTEREST", "status": "VALID", "subAccount": "MARGIN", "netAmount": 0.53, "transferItems": [
	{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD", "description": "USD currency",
	"instrumentId": 1, "closingPrice": 0.0}, "amount": 0.53, "cost": 0.0}]}`, options)

// The History files of the tool exports' store. fileAccount's first holds
// the cases that only files hold: a journal of cash that nothing pairs,
// shares journaled in and transferred out, a bond bought under its CUSIP,
// its coupon and more of it journaled in at a price, a file option bought
// and expired, a cash merger's two rows, a split, a Buy and its Cancel Buy,
// margin interest and a foreign tax reclaim, besides Buys of the
// securities that those name.
var (
	toolHistory = historyCSV(
		csvRow{"07/01/2026", "Cash Merger", "EFGH", "EFGH CORP CASH MERGER", "", "", "", "$1,200.00"},
		csvRow{"07/01/2026", "Cash Merger Adj", "EFGH", "EFGH CORP", "-30", "", "", ""},
		csvRow{"06/15/2026", "Stock Split", "ABCD", "ABCD INDUSTRIES INC", "6", "", "", ""},
		csvRow{"06/01/2026", "Journaled Shares", "9128ZZQ15", "EXAMPLE TREASURY NOTE 4.25% 08/15/2034", "5,000", "$99.80", "", ""},
		csvRow{"05/18/2026", "Expired", "XYZ 05/15/2026 50.00 C", "CALL XYZ CORP $50 EXP 05/15/26", "-2", "", "", ""},
		csvRow{"05/15/2026", "Bond Interest", "9128ZZQ15", "EXAMPLE TREASURY NOTE 4.25% 08/15/2034", "", "", "", "$212.50"},
		csvRow{"04/20/2026", "Foreign Tax Reclaim", "ABCD", "ABCD INDUSTRIES INC", "", "", "", "$1.25"},
		csvRow{"04/06/2026", "Buy to Open", "XYZ 05/15/2026 50.00 C", "CALL XYZ CORP $50 EXP 05/15/26", "2", "$1.25", "$1.32", "-$251.32"},
		csvRow{"04/01/2026", "Buy", "9128ZZQ15", "EXAMPLE TREASURY NOTE 4.25% 08/15/2034", "10,000", "$99.50", "", "-$9,962.15"},
		csvRow{"03/20/2026", "Margin Interest", "", "MARGIN INTEREST", "", "", "", "-$4.17"},
		csvRow{"03/16/2026", "Security Transfer", "ABCD", "ABCD INDUSTRIES INC", "-4", "", "", ""},
		csvRow{"03/09/2026", "Journaled Shares", "ABCD", "ABCD INDUSTRIES INC", "10", "", "", ""},
		csvRow{"03/02/2026", "Journal", "", "JOURNAL FRM ...777", "", "", "", "$250.00"},
		csvRow{"02/24/2026", "Buy", "MNOP", "MNOP HOLDINGS INC", "12", "$41.50", "", "-$498.00"},
		csvRow{"02/18/2026", "Cancel Buy", "QRST", "QRST INC", "5", "$20.00", "", "$100.00"},
		csvRow{"02/17/2026", "Buy", "QRST", "QRST INC", "5", "$20.00", "", "-$100.00"},
		csvRow{"02/10/2026", "Buy", "EFGH", "EFGH CORP", "30", "$38.00", "", "-$1,140.00"},
	)
	// toolMatched is the individual account's: a dividend that either of
	// two API rows could be, which is uncertain; interest on a day that its
	// API rows cover, which none of them is, which is held out; and a wire
	// that an API row is, which is linked to it.
	toolMatched = historyCSV(
		csvRow{"09/22/2026", "Qualified Dividend", "AAPL", "APPLE INC", "", "", "", "$0.26"},
		csvRow{"05/31/2024", "Bank Interest", "", "BANK INT 050124-053124 SCHWAB BANK", "", "", "", "$0.37"},
		csvRow{"12/15/2023", "Wire Funds Received", "", "WIRE FUNDS RECEIVED", "", "", "", "$10,000.00"},
	)
	// toolLater holds a dividend posted after the first exports' last day
	// "as of" a day before it, and a dividend of MNOP, which toolCorrection,
	// exported later, holds with another amount: a correction, which the
	// store flags near_duplicate on both.
	toolLater = historyCSV(
		csvRow{"11/16/2026 as of 11/09/2026", "Qualified Dividend", "ABCD", "ABCD INDUSTRIES INC", "", "", "", "$4.20"},
		csvRow{"11/13/2026", "Qualified Dividend", "MNOP", "MNOP HOLDINGS INC", "", "", "", "$3.60"},
	)
	toolCorrection = historyCSV(csvRow{"11/13/2026", "Qualified Dividend", "MNOP", "MNOP HOLDINGS INC", "", "", "", "$3.06"})
)

// newToolStore returns the store of the tool exports' tests (11.3 of the
// design), built once for every check of a test: the fixtures' API rows,
// complete reads of the individual and the options accounts, with their
// nicknames; reads of both after the fixtures' window, of a journal of cash
// out of one and into the other a day later, a journal of shares out of
// one and into the other, a spread of two options, two dividends and a
// dividend with no day; fileAccount's History file, imported with
// -account, since two accounts end in 398; and the individual account's
// History file (toolMatched), imported in one call with snapshots of the
// fixtures' accounts on two evenings, which price the short put and
// 00507V109, delisted between them, with a negative balance on the second,
// and a snapshot taken during the second evening's session. importLater
// adds what a later export goes on to.
func newToolStore(t *testing.T) *toolStore {
	t.Helper()
	s := newStore(t, Options{})
	ctx := t.Context()
	recordFixtures(t, s)
	var prefs schwab.UserPreference
	if err := json.Unmarshal(schwabtest.Fixture(t, "user_preference.json"), &prefs); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordPreferences(ctx, readAt(fixturesEnd), prefs); err != nil {
		t.Fatal(err)
	}
	// Read on the second snapshot's evening, so that its AAPL and the
	// transactions' are one security, as reads near each other make it.
	end := time.Date(2026, time.November, 10, 23, 30, 0, 0, time.UTC)
	record(t, s, listRead(options, fixturesEnd, end, end,
		decodeRow(t, journalRaw(73600000101, options, toolPaysAt, -500, "JOURNAL TO ...135")),
		decodeRow(t, toolSharesRaw(73600000104, options, toolTakesAt, 2, "", "JOURNALED SHARES FRM ...135")),
		decodeRow(t, toolSpreadRaw),
		decodeRow(t, toolNoDayRaw)))
	record(t, s, listRead(individual, fixturesEnd, end, end,
		decodeRow(t, journalRaw(73600000102, individual, toolReceivesAt, 500, "JOURNAL FRM ...398")),
		decodeRow(t, toolSharesRaw(73600000103, individual, toolDeliversAt, -2, "230.1", "JOURNALED SHARES TO ...398")),
		decodeRow(t, toolDividendRaw(73600000107, time.Date(2026, time.September, 21, 9, 0, 0, 0, nytime.NewYork()), "AAPL", 0.26)),
		decodeRow(t, toolDividendRaw(73600000108, time.Date(2026, time.September, 23, 9, 0, 0, 0, nytime.NewYork()), "AAPL", 0.26))))
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260720-090000", "csv"), toolHistory)
	snapshots := snapshotLine(t, toolEvening1,
		fixtureAccount(t, individual, `"symbol": "00507V109"`, `"symbol": "ZZDLX"`, `"marketValue": 0.0`, `"marketValue": 12.5`),
		fixtureAccount(t, options), fixtureAccount(t, empty)) +
		snapshotLine(t, toolEvening2,
			fixtureAccount(t, individual, `"marketValue": 0.0`, `"marketValue": 11.8`),
			fixtureAccount(t, options, `"marketValue": -310.0`, `"marketValue": -290.0`,
				`"cashBalance": 5210.44`, `"cashBalance": -12.34`, `"cashBalance": 5210.44`, `"cashBalance": -12.34`),
			fixtureAccount(t, empty)) +
		snapshotLine(t, toolMorning, fixtureAccount(t, options, `"marketValue": -310.0`, `"marketValue": -300.0`,
			`"cashBalance": 5210.44`, `"cashBalance": 4000.0`, `"cashBalance": 5210.44`, `"cashBalance": 4000.0`))
	importFiles(t, s, ImportOptions{}, "Individual_XXX135_Transactions_20261005-090000.csv", toolMatched, "snapshots.jsonl", snapshots)
	ts := &toolStore{s: s, history: toolHistory, matched: toolMatched, later: toolLater, correction: toolCorrection}
	for _, a := range []struct {
		id     *int64
		number string
	}{{&ts.individual, individual}, {&ts.options, options}, {&ts.files, fileAccount}, {&ts.empty, empty}} {
		*a.id = storeInt(t, s, "SELECT account_id FROM account WHERE number = ?", a.number)
	}
	return ts
}

// importLater imports the History files of fileAccount that a later export
// goes on to (11.3 of the design), in one call, as complete exports: the
// row "as of" a day before the day it was posted, and the dividend and its
// correction.
func (ts *toolStore) importLater(t *testing.T) {
	t.Helper()
	importFiles(t, ts.s, ImportOptions{Account: fileAccount, Completeness: "complete"},
		exportName("20261120-090000", "csv"), ts.later, exportName("20261123-090000", "csv"), ts.correction)
}

// fileRefs returns how the refs of the transactions that rows of the
// History file whose content is content opened start: C, the start of the
// file's SHA-256 in hex, and the dot before the row's ordinal.
func fileRefs(content string) string {
	return "C" + fileHex([]byte(content))[:8] + "."
}

// read reads the store as a tool export of the account of id account, 0
// for every account, up to the day to does (readToolInput), in one read
// transaction.
func (ts *toolStore) read(t *testing.T, account int64, to nytime.Date) *toolInput {
	t.Helper()
	var in *toolInput
	if err := ts.s.withRead(t.Context(), func(r *rtx) error {
		var err error
		in, err = readToolInput(r, account, to)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if in.account != account || in.to != to {
		t.Fatalf("readToolInput(%d, %q) gives an input of account %d up to %q", account, to, in.account, in.to)
	}
	return in
}

// toolWord writes v as a line of toolLines holds it: as it is, or quoted
// when it holds a space or is empty.
func toolWord(v string) string {
	if v == "" || strings.ContainsAny(v, " \t") {
		return strconv.Quote(v)
	}
	return v
}

// A toolLine builds a line of toolLines: words, and fields named, each
// written only when it holds a value.
type toolLine struct{ b strings.Builder }

// word adds v, a word (toolWord).
func (l *toolLine) word(v string) *toolLine { return l.add(toolWord(v)) }

// add adds token as it is.
func (l *toolLine) add(token string) *toolLine {
	if l.b.Len() > 0 {
		l.b.WriteByte(' ')
	}
	l.b.WriteString(token)
	return l
}

// text adds the field name of text v, but for "".
func (l *toolLine) text(name, v string) *toolLine {
	if v != "" {
		l.add(name + "=" + toolWord(v))
	}
	return l
}

// int adds the field name of whole number v, but for NULL.
func (l *toolLine) int(name string, v sql.NullInt64) *toolLine {
	if v.Valid {
		l.add(name + "=" + strconv.FormatInt(v.Int64, 10))
	}
	return l
}

// float adds the field name of number v, but for NULL.
func (l *toolLine) float(name string, v sql.NullFloat64) *toolLine {
	if v.Valid {
		l.add(name + "=" + strconv.FormatFloat(v.Float64, 'g', -1, 64))
	}
	return l
}

// time adds the field name of instant v in UTC, but for the zero time.
func (l *toolLine) time(name string, v time.Time) *toolLine {
	if !v.IsZero() {
		l.add(name + "=" + v.UTC().Format(time.RFC3339))
	}
	return l
}

// toolLines returns what in holds, by the field of toolInput that holds it,
// a line for each value, every field of the value that holds one written:
// the input as checkToolRead and the checks after it compare it.
func toolLines(in *toolInput) map[string][]string {
	out := map[string][]string{"store": {in.store}}
	add := func(field string, l *toolLine) { out[field] = append(out[field], l.b.String()) }
	account := func(id int64) string { return "A" + strconv.FormatInt(id, 10) }
	for _, a := range in.accounts {
		add("accounts", new(toolLine).word(account(a.id)).word(a.masked).text("nickname", a.nickname))
	}
	for _, r := range in.rows {
		l := new(toolLine).word(r.ref).word(account(r.account)).word(r.source).text("day", string(r.day)).
			text("posted", string(r.posted)).time("time", r.time).text("kind", r.kind).text("description", r.description).
			int("amount", r.amount).int("fees", r.fees).int("gross", r.gross).int("quantity", r.quantity).
			text("as_of", string(r.quantityAsOf)).float("price", r.price).text("symbol", r.symbol).
			text("key", r.securityKey).text("category", r.category).text("pair", r.pairKind).text("pair_ref", r.pairRef)
		if r.pairAccount != 0 {
			l.add("pair_account=" + account(r.pairAccount))
		}
		l.text("flags", strings.Join(r.flags, ";"))
		if p := r.partner; p != (toolPartner{}) {
			l.add("partner:").text("day", string(p.day)).time("time", p.time).int("amount", p.amount).float("price", p.price).
				text("counted", strconv.FormatBool(p.counted)).text("category", p.category).text("kind", p.kind).
				text("description", p.description)
		}
		add("rows", l)
	}
	for _, g := range in.legs {
		add("legs", new(toolLine).word(account(g.account)).word(g.ref).add("leg="+strconv.Itoa(g.leg)).text("symbol", g.symbol).
			text("key", g.securityKey).int("quantity", g.quantity).int("cost", g.cost).float("price", g.price))
	}
	for _, s := range in.securities {
		add("securities", new(toolLine).word(s.key).text("asset_type", s.assetType).text("type", s.typ).text("symbol", s.symbol).
			text("cusip", s.cusip).text("description", s.description).text("put_call", s.putCall).float("strike", s.strike).
			text("expiration", string(s.expiration)).float("multiplier", s.multiplier).text("underlying", s.underlying))
	}
	for _, n := range in.names {
		add("names", new(toolLine).word(n.key).word(n.kind).word(n.value).text("from", n.from).time("first", n.firstSeen).time("last", n.lastSeen))
	}
	for _, p := range in.positions {
		add("positions", new(toolLine).word(p.key).word(account(p.account)).word(string(p.marketDate)).time("fetched", p.fetchedAt).
			int("long", p.long).int("short", p.short).float("value", p.marketValue))
	}
	for _, s := range in.snapshots {
		add("snapshots", new(toolLine).word(account(s.account)).word(string(s.marketDate)).time("fetched", s.fetchedAt).int("cash", s.cash).
			text("type", s.accountType).float("margin", s.margin).float("short", s.short))
	}
	for _, w := range in.waiting {
		add("waiting", new(toolLine).word(w.ref).word(account(w.account)).word(string(w.day)).word(w.kind).int("amount", w.amount).word(w.why))
	}
	for _, c := range in.coverage {
		add("coverage", new(toolLine).word(account(c.AccountID)).word(c.Account).word(c.Source).word(string(c.From)).word(string(c.To)))
	}
	for _, u := range in.uncounted {
		add("uncounted", new(toolLine).word(u.ref).word(account(u.account)).word(u.why).text("matched_with", u.matchedWith))
	}
	for _, l := range in.links {
		add("links", new(toolLine).word(l.file).word(l.api))
	}
	return out
}

// lines returns toolLines of in, with the refs of the store's file
// transactions named by their file: H for fileAccount's first History file,
// M for the individual account's, L for the later one and R for its
// correction, as H.3 for "C<the file's start>.3".
func (ts *toolStore) lines(in *toolInput) map[string][]string {
	files := strings.NewReplacer(
		fileRefs(ts.history), "H.", fileRefs(ts.matched), "M.", fileRefs(ts.later), "L.", fileRefs(ts.correction), "R.")
	out := toolLines(in)
	for _, ls := range out {
		for i, l := range ls {
			ls[i] = files.Replace(l)
		}
	}
	return out
}

// toolSelected returns what a read of the account of id account, 0 for
// every account, up to the day to, "" for no bound, holds of in, a read of
// every account with no bound (2 of the design): the ledger rows of the
// account up to to and those without a day; the legs of those of its rows
// that have a day up to to; the positions of every account up to to; the
// account's snapshots of every day; the rows of the account up to to that
// wait for you; the coverage and the uncounted entries of the account; and
// all the accounts, securities, names, days of the rows under each symbol
// and links of rows of two securities.
func toolSelected(in *toolInput, account int64, to nytime.Date) *toolInput {
	out := *in
	out.account, out.to = account, to
	of := func(id int64) bool { return account == 0 || id == account }
	upTo := func(d nytime.Date) bool { return to == "" || d <= to }
	days := map[rowKey]nytime.Date{}
	for _, r := range in.rows {
		days[rowKey{r.account, r.ref}] = r.day
	}
	out.rows = slices.DeleteFunc(slices.Clone(in.rows), func(r toolRow) bool { return !of(r.account) || r.day != "" && !upTo(r.day) })
	out.legs = slices.DeleteFunc(slices.Clone(in.legs), func(l toolLeg) bool {
		d := days[rowKey{l.account, l.ref}]
		return !of(l.account) || d == "" || !upTo(d)
	})
	out.positions = slices.DeleteFunc(slices.Clone(in.positions), func(p toolPosition) bool { return !upTo(p.marketDate) })
	out.snapshots = slices.DeleteFunc(slices.Clone(in.snapshots), func(s toolSnapshot) bool { return !of(s.account) })
	out.waiting = slices.DeleteFunc(slices.Clone(in.waiting), func(w toolWaiting) bool { return !of(w.account) || !upTo(w.day) })
	out.coverage = slices.DeleteFunc(slices.Clone(in.coverage), func(c ExportCoverage) bool { return !of(c.AccountID) })
	out.uncounted = slices.DeleteFunc(slices.Clone(in.uncounted), func(u toolUncounted) bool { return !of(u.account) })
	return &out
}

// compareToolLines fails the test for each field of toolLines whose lines
// in got are not those of want, in their order; what says what was read.
func compareToolLines(t *testing.T, what string, got, want map[string][]string) {
	t.Helper()
	fields := slices.Sorted(maps.Keys(want))
	for f := range got {
		if !slices.Contains(fields, f) {
			fields = append(fields, f)
		}
	}
	for _, f := range fields {
		if !slices.Equal(got[f], want[f]) {
			t.Errorf("%s: %s holds\n\t%s\nwant\n\t%s", what, f, strings.Join(got[f], "\n\t"), strings.Join(want[f], "\n\t"))
		}
	}
}

// toolInsert returns lines with added inserted before the line before,
// which lines must hold.
func toolInsert(t *testing.T, lines []string, before string, added ...string) []string {
	t.Helper()
	i := slices.Index(lines, before)
	if i < 0 {
		t.Fatalf("no line %q to insert before", before)
	}
	return slices.Insert(slices.Clone(lines), i, added...)
}

// toolStoreLines is what a read of every account of newToolStore's store,
// with no bound, holds, as toolStore.lines writes it, but for when the
// store was made: every account, nickname or none; every counted row, by
// day (a row with none first), time, account and ref, with its partner's
// columns; the legs of the spread; the securities and their symbols and
// descriptions; the positions of both evenings; the snapshots, one of them
// with a negative cash balance and two with none; the rows that wait for
// you, held out and uncertain; the coverage; the entries that do not
// count, the linked one with the API row it is; and the days of the rows
// and legs under each symbol of each security, counted or not (the
// uncertain AAPL dividend of 09-22 among them), of every account and day,
// but for a row without one (the META dividend). It holds no links of two
// securities: the one link, of the wire, is of rows of none.
var toolStoreLines = map[string][]string{
	"accounts": {
		`A1 ...135 nickname=Individual`,
		`A2 ...398 nickname="Options & Margin"`,
		`A3 ...398`,
		`A4 ...783`,
	},
	"rows": {
		`T73600000106 A2 api kind=DIVIDEND_OR_INTEREST description="Ordinary Dividend~META" amount=53 fees=0 gross=53 symbol=META category=dividend flags=problems;unresolved_security`,
		`T73508261953 A1 api day=2023-06-12 time=2023-06-12T14:20:31Z kind=CASH_RECEIPT description="Funds Received" amount=15000 fees=0 gross=15000 category=deposit`,
		`T73508261952 A1 api day=2023-07-19 time=2023-07-19T07:32:10Z kind=ELECTRONIC_FUND description="ACH Credit Received" amount=250000 fees=0 gross=250000 category=deposit`,
		`T73508261951 A1 api day=2023-12-15 time=2023-12-15T16:02:55Z kind=WIRE_IN description="Wire Incoming" amount=1000000 fees=0 gross=1000000 category=deposit`,
		`T73508261947 A1 api day=2024-03-15 time=2024-03-15T23:36:04Z kind=TRADE description="Sell Trade" amount=8327 fees=0 gross=8327 quantity=-451300 price=184.51 symbol=VBR key=i:4417355 category=sell`,
		`T73508261950 A1 api day=2024-04-04 time=2024-04-04T15:54:14Z kind=DIVIDEND_OR_INTEREST description="Ordinary Dividend~BND" amount=386 fees=0 gross=386 symbol=BND key=i:4417302 category=dividend`,
		`T73508261954 A1 api day=2024-04-04 time=2024-04-04T15:54:15Z kind=JOURNAL description="W-8 Withholding~BND" amount=-116 fees=0 gross=-116 symbol=BND key=i:4417302 category=tax`,
		`T73508261946 A1 api day=2024-04-04 time=2024-04-04T15:56:07Z kind=TRADE description="Dividend Reinvest" amount=-270 fees=0 gross=-270 quantity=37800 price=71.42857 symbol=BND key=i:4417302 category=buy`,
		`T73508261949 A1 api day=2024-04-30 time=2024-05-01T01:18:44Z kind=DIVIDEND_OR_INTEREST description="Free Balance Interest Adjustment" amount=42 fees=0 gross=42 category=interest`,
		`T73508261945 A2 api day=2025-01-14 time=2025-01-14T05:15:12Z kind=TRADE amount=-105282 fees=282 gross=-105000 quantity=1000000 price=21 symbol=/ESH25_P5050:XCME key=i:205187341 category=buy`,
		`T73508261944 A2 api day=2025-01-21 time=2025-01-21T16:05:12Z kind=TRADE amount=-40067 fees=67 gross=-40000 quantity=2000000 price=2 symbol="XYZ   250221P00040000" key=i:219771034 category=buy`,
		`T73508261943 A1 api day=2025-06-02 time=2025-06-02T14:30:27Z kind=TRADE amount=-60546 fees=0 gross=-60546 quantity=3000000 price=201.82 symbol=AAPL key=i:1973757747 category=buy`,
		`H.17 A3 file day=2026-02-10 posted=2026-02-10 kind=Buy description="EFGH CORP" amount=-114000 fees=0 gross=-114000 quantity=30000000 as_of=2026-07-20 price=38 symbol=EFGH key=s:FILE:EFGH@2026-07-20 category=buy`,
		`H.16 A3 file day=2026-02-17 posted=2026-02-17 kind=Buy description="QRST INC" amount=-10000 fees=0 gross=-10000 quantity=5000000 as_of=2026-07-20 price=20 symbol=QRST key=s:FILE:QRST@2026-07-20 category=buy pair=reversal pair_ref=H.15 pair_account=A3 partner: day=2026-02-18 amount=10000 price=20 counted=true category=buy kind="Cancel Buy" description="QRST INC"`,
		`H.15 A3 file day=2026-02-18 posted=2026-02-18 kind="Cancel Buy" description="QRST INC" amount=10000 fees=0 gross=10000 quantity=-5000000 as_of=2026-07-20 price=20 symbol=QRST key=s:FILE:QRST@2026-07-20 category=buy pair=reversal pair_ref=H.16 pair_account=A3 partner: day=2026-02-17 amount=-10000 price=20 counted=true category=buy kind=Buy description="QRST INC"`,
		`H.14 A3 file day=2026-02-24 posted=2026-02-24 kind=Buy description="MNOP HOLDINGS INC" amount=-49800 fees=0 gross=-49800 quantity=12000000 as_of=2026-07-20 price=41.5 symbol=MNOP key=s:FILE:MNOP@2026-07-20 category=buy`,
		`H.13 A3 file day=2026-03-02 posted=2026-03-02 kind=Journal description="JOURNAL FRM ...777" amount=25000 fees=0 gross=25000 as_of=2026-07-20 category=deposit flags=unpaired_journal`,
		`H.12 A3 file day=2026-03-09 posted=2026-03-09 kind="Journaled Shares" description="ABCD INDUSTRIES INC" amount=0 fees=0 gross=0 quantity=10000000 as_of=2026-07-20 symbol=ABCD key=s:FILE:ABCD@2026-07-20 category=transfer_in flags=unpaired_journal`,
		`T73508261942 A2 api day=2026-03-13 time=2026-03-13T13:55:09Z kind=TRADE amount=60336 fees=4 gross=60340 quantity=-1000000 price=603.4 symbol=META key=i:1940083512 category=sell`,
		`H.11 A3 file day=2026-03-16 posted=2026-03-16 kind="Security Transfer" description="ABCD INDUSTRIES INC" amount=0 fees=0 gross=0 quantity=-4000000 as_of=2026-07-20 symbol=ABCD key=s:FILE:ABCD@2026-07-20 category=transfer_out flags=unpaired_journal`,
		`H.10 A3 file day=2026-03-20 posted=2026-03-20 kind="Margin Interest" description="MARGIN INTEREST" amount=-417 fees=0 gross=-417 as_of=2026-07-20 category=interest`,
		`H.9 A3 file day=2026-04-01 posted=2026-04-01 kind=Buy description="EXAMPLE TREASURY NOTE 4.25% 08/15/2034" amount=-996215 fees=0 gross=-996215 quantity=10000000000 as_of=2026-07-20 price=99.5 symbol=9128ZZQ15 key=s:FILE:9128ZZQ15@2026-07-20 category=buy`,
		`H.8 A3 file day=2026-04-06 posted=2026-04-06 kind="Buy to Open" description="CALL XYZ CORP $50 EXP 05/15/26" amount=-25132 fees=132 gross=-25000 quantity=2000000 as_of=2026-07-20 price=1.25 symbol="XYZ 05/15/2026 50.00 C" key="s:FILE:XYZ 05/15/2026 50.00 C@2026-07-20" category=buy`,
		`H.7 A3 file day=2026-04-20 posted=2026-04-20 kind="Foreign Tax Reclaim" description="ABCD INDUSTRIES INC" amount=125 fees=0 gross=125 as_of=2026-07-20 symbol=ABCD key=s:FILE:ABCD@2026-07-20 category=tax`,
		`H.6 A3 file day=2026-05-15 posted=2026-05-15 kind="Bond Interest" description="EXAMPLE TREASURY NOTE 4.25% 08/15/2034" amount=21250 fees=0 gross=21250 as_of=2026-07-20 symbol=9128ZZQ15 key=s:FILE:9128ZZQ15@2026-07-20 category=interest`,
		`H.5 A3 file day=2026-05-18 posted=2026-05-18 kind=Expired description="CALL XYZ CORP $50 EXP 05/15/26" amount=0 fees=0 gross=0 quantity=-2000000 as_of=2026-07-20 symbol="XYZ 05/15/2026 50.00 C" key="s:FILE:XYZ 05/15/2026 50.00 C@2026-07-20" category=other`,
		`H.4 A3 file day=2026-06-01 posted=2026-06-01 kind="Journaled Shares" description="EXAMPLE TREASURY NOTE 4.25% 08/15/2034" amount=0 fees=0 gross=0 quantity=5000000000 as_of=2026-07-20 price=99.8 symbol=9128ZZQ15 key=s:FILE:9128ZZQ15@2026-07-20 category=transfer_in flags=unpaired_journal`,
		`H.3 A3 file day=2026-06-15 posted=2026-06-15 kind="Stock Split" description="ABCD INDUSTRIES INC" amount=0 fees=0 gross=0 quantity=6000000 as_of=2026-07-20 symbol=ABCD key=s:FILE:ABCD@2026-07-20 category=other`,
		`H.1 A3 file day=2026-07-01 posted=2026-07-01 kind="Cash Merger" description="EFGH CORP CASH MERGER" amount=120000 fees=0 gross=120000 as_of=2026-07-20 symbol=EFGH key=s:FILE:EFGH@2026-07-20 category=sell flags=two_row_event`,
		`H.2 A3 file day=2026-07-01 posted=2026-07-01 kind="Cash Merger Adj" description="EFGH CORP" amount=0 fees=0 gross=0 quantity=-30000000 as_of=2026-07-20 symbol=EFGH key=s:FILE:EFGH@2026-07-20 category=other flags=two_row_event`,
		`T73508261948 A2 api day=2026-07-15 time=2026-07-15T14:40:03Z kind=DIVIDEND_OR_INTEREST description="Ordinary Dividend~XYZ" amount=2500 fees=0 gross=2500 symbol=XYZ key=i:1854092 category=dividend flags=security_by_name`,
		`T73508261941 A2 api day=2026-08-04 time=2026-08-04T15:22:41Z kind=TRADE amount=31131 fees=69 gross=31200 quantity=-1000000 price=3.12 symbol="XYZ   260918C00050000" key=i:219834765 category=sell`,
		`T73600000101 A2 api day=2026-09-08 time=2026-09-08T14:00:00Z kind=JOURNAL description="JOURNAL TO ...135" amount=-50000 fees=0 gross=-50000 category=journal pair=journal pair_ref=T73600000102 pair_account=A1 partner: day=2026-09-09 time=2026-09-09T13:30:00Z amount=50000 counted=true category=journal kind=JOURNAL description="JOURNAL FRM ...398"`,
		`T73600000102 A1 api day=2026-09-09 time=2026-09-09T13:30:00Z kind=JOURNAL description="JOURNAL FRM ...398" amount=50000 fees=0 gross=50000 category=journal pair=journal pair_ref=T73600000101 pair_account=A2 partner: day=2026-09-08 time=2026-09-08T14:00:00Z amount=-50000 counted=true category=journal kind=JOURNAL description="JOURNAL TO ...135"`,
		`T73600000103 A1 api day=2026-09-10 time=2026-09-10T14:15:00Z kind=JOURNAL description="JOURNALED SHARES TO ...398" amount=0 fees=0 gross=0 quantity=-2000000 price=230.1 symbol=AAPL key=i:1973757747 category=journal pair=journal pair_ref=T73600000104 pair_account=A2 partner: day=2026-09-10 time=2026-09-10T14:16:00Z amount=0 counted=true category=journal kind=JOURNAL description="JOURNALED SHARES FRM ...135"`,
		`T73600000104 A2 api day=2026-09-10 time=2026-09-10T14:16:00Z kind=JOURNAL description="JOURNALED SHARES FRM ...135" amount=0 fees=0 gross=0 quantity=2000000 symbol=AAPL key=i:1973757747 category=journal pair=journal pair_ref=T73600000103 pair_account=A1 partner: day=2026-09-10 time=2026-09-10T14:15:00Z amount=0 price=230.1 counted=true category=journal kind=JOURNAL description="JOURNALED SHARES TO ...398"`,
		`T73600000105 A2 api day=2026-09-15 time=2026-09-15T15:20:05Z kind=TRADE amount=-13134 fees=134 gross=-13000 category=buy flags=mixed_legs`,
		`T73600000107 A1 api day=2026-09-21 time=2026-09-21T13:00:00Z kind=DIVIDEND_OR_INTEREST description="Ordinary Dividend~AAPL" amount=26 fees=0 gross=26 symbol=AAPL key=i:1973757747 category=dividend`,
		`T73600000108 A1 api day=2026-09-23 time=2026-09-23T13:00:00Z kind=DIVIDEND_OR_INTEREST description="Ordinary Dividend~AAPL" amount=26 fees=0 gross=26 symbol=AAPL key=i:1973757747 category=dividend`,
	},
	"legs": {
		`A2 T73600000105 leg=2 symbol="XYZ   261016P00045000" key=i:219950001 quantity=1000000 cost=-21000 price=2.1`,
		`A2 T73600000105 leg=3 symbol="XYZ   261016P00040000" key=i:219950002 quantity=-1000000 cost=8000 price=0.8`,
	},
	"securities": {
		`c:00507V109 asset_type=EQUITY symbol=00507V109 cusip=00507V109`,
		`c:0XYZ..RI60045000 asset_type=OPTION type=VANILLA symbol="XYZ   260619P00045000" cusip=0XYZ..RI60045000 description="XYZ 06/19/2026 45.00 P" put_call=PUT strike=45 expiration=2026-06-19 underlying=XYZ`,
		`c:30303M102 asset_type=EQUITY symbol=META cusip=30303M102`,
		`c:808515779 asset_type=MUTUAL_FUND type=NO_LOAD_TAXABLE symbol=SWVXX cusip=808515779 description="SCHWAB VALUE ADVANTAGE MONEY FUND"`,
		`c:922908769 asset_type=COLLECTIVE_INVESTMENT type=EXCHANGE_TRADED_FUND symbol=VTI cusip=922908769 description="VANGUARD TOTAL STOCK MARKET ETF"`,
		`i:1854092 asset_type=EQUITY type=COMMON_STOCK symbol=XYZ cusip=98389B100 description="XYZ CORP COMMON STOCK"`,
		`i:1940083512 asset_type=EQUITY type=COMMON_STOCK symbol=META description="META PLATFORMS INC CLASS A"`,
		`i:1973757747 asset_type=EQUITY type=COMMON_STOCK symbol=AAPL cusip=037833100`,
		`i:205187341 asset_type=OPTION type=VANILLA symbol=/ESH25_P5050:XCME description="E-MINI S&P 500 OPTIONS MAR 25 5050 PUT" put_call=PUT strike=5050 expiration=2025-03-21 multiplier=50 underlying=.ESH25:XCME`,
		`i:219771034 asset_type=OPTION type=VANILLA symbol="XYZ   250221P00040000" description="XYZ 02/21/2025 40.00 P" put_call=PUT strike=40 expiration=2025-02-21 multiplier=100 underlying=XYZ`,
		`i:219834765 asset_type=OPTION type=VANILLA symbol="XYZ   260918C00050000" description="XYZ 09/18/2026 50.00 C" put_call=CALL strike=50 expiration=2026-09-18 multiplier=100 underlying=XYZ`,
		`i:219950001 asset_type=OPTION type=VANILLA symbol="XYZ   261016P00045000" description="XYZ 10/16/2026 45.00 P" put_call=PUT strike=45 expiration=2026-10-16 multiplier=100 underlying=XYZ`,
		`i:219950002 asset_type=OPTION type=VANILLA symbol="XYZ   261016P00040000" description="XYZ 10/16/2026 40.00 P" put_call=PUT strike=40 expiration=2026-10-16 multiplier=100 underlying=XYZ`,
		`i:4417302 asset_type=COLLECTIVE_INVESTMENT type=EXCHANGE_TRADED_FUND symbol=BND description="VANGUARD TOTAL BOND MARKET ETF"`,
		`i:4417355 asset_type=COLLECTIVE_INVESTMENT type=EXCHANGE_TRADED_FUND symbol=VBR description="VANGUARD SMALL-CAP VALUE ETF"`,
		`s:FILE:9128ZZQ15@2026-07-20 symbol=9128ZZQ15`,
		`s:FILE:ABCD@2026-07-20 symbol=ABCD`,
		`s:FILE:EFGH@2026-07-20 symbol=EFGH`,
		`s:FILE:MNOP@2026-07-20 symbol=MNOP`,
		`s:FILE:QRST@2026-07-20 symbol=QRST`,
		`"s:FILE:XYZ 05/15/2026 50.00 C@2026-07-20" asset_type=OPTION symbol="XYZ 05/15/2026 50.00 C"`,
	},
	"names": {
		`c:00507V109 symbol 00507V109 first=2026-11-10T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:00507V109 symbol ZZDLX first=2026-11-09T23:05:00Z last=2026-11-09T23:05:00Z`,
		`c:0XYZ..RI60045000 description "XYZ 06/19/2026 45.00 P" first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:0XYZ..RI60045000 symbol "XYZ   260619P00045000" first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:30303M102 symbol META first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:808515779 description "SCHWAB VALUE ADVANTAGE MONEY FUND" first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:808515779 symbol SWVXX first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:922908769 description "VANGUARD TOTAL STOCK MARKET ETF" first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`c:922908769 symbol VTI first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`i:1854092 description "XYZ CORP COMMON STOCK" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:1854092 symbol XYZ first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:1940083512 description "META PLATFORMS INC CLASS A" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:1940083512 symbol META first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:1973757747 symbol AAPL first=2025-06-03T00:00:01Z last=2026-11-10T23:30:01Z`,
		`i:1973757747 symbol AAPL from=c:037833100 first=2026-11-09T23:05:00Z last=2026-11-10T23:05:00Z`,
		`i:1973757747 symbol AAPL from=s:FILE:AAPL@2026-10-05 first=2026-10-05T13:00:00Z last=2026-10-05T13:00:00Z`,
		`i:205187341 description "E-MINI S&P 500 OPTIONS MAR 25 5050 PUT" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:205187341 symbol /ESH25_P5050:XCME first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:219771034 description "XYZ 02/21/2025 40.00 P" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:219771034 symbol "XYZ   250221P00040000" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:219834765 description "XYZ 09/18/2026 50.00 C" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:219834765 symbol "XYZ   260918C00050000" first=2026-08-05T00:00:01Z last=2026-08-05T00:00:01Z`,
		`i:219950001 description "XYZ 10/16/2026 45.00 P" first=2026-11-10T23:30:01Z last=2026-11-10T23:30:01Z`,
		`i:219950001 symbol "XYZ   261016P00045000" first=2026-11-10T23:30:01Z last=2026-11-10T23:30:01Z`,
		`i:219950002 description "XYZ 10/16/2026 40.00 P" first=2026-11-10T23:30:01Z last=2026-11-10T23:30:01Z`,
		`i:219950002 symbol "XYZ   261016P00040000" first=2026-11-10T23:30:01Z last=2026-11-10T23:30:01Z`,
		`i:4417302 description "VANGUARD TOTAL BOND MARKET ETF" first=2025-06-03T00:00:01Z last=2025-06-03T00:00:01Z`,
		`i:4417302 symbol BND first=2025-06-03T00:00:01Z last=2025-06-03T00:00:01Z`,
		`i:4417355 description "VANGUARD SMALL-CAP VALUE ETF" first=2025-06-03T00:00:01Z last=2025-06-03T00:00:01Z`,
		`i:4417355 symbol VBR first=2025-06-03T00:00:01Z last=2025-06-03T00:00:01Z`,
		`s:FILE:9128ZZQ15@2026-07-20 symbol 9128ZZQ15 first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`s:FILE:ABCD@2026-07-20 symbol ABCD first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`s:FILE:EFGH@2026-07-20 symbol EFGH first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`s:FILE:MNOP@2026-07-20 symbol MNOP first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`s:FILE:QRST@2026-07-20 symbol QRST first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`"s:FILE:XYZ 05/15/2026 50.00 C@2026-07-20" symbol "XYZ 05/15/2026 50.00 C" first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
	},
	"positions": {
		`c:00507V109 A1 2026-11-09 fetched=2026-11-09T23:05:00Z long=10000000 short=0 value=12.5`,
		`c:00507V109 A1 2026-11-10 fetched=2026-11-10T23:05:00Z long=10000000 short=0 value=11.8`,
		`c:0XYZ..RI60045000 A2 2026-11-09 fetched=2026-11-09T23:05:00Z long=0 short=2000000 value=-310`,
		`c:0XYZ..RI60045000 A2 2026-11-10 fetched=2026-11-10T23:05:00Z long=0 short=2000000 value=-290`,
		`c:30303M102 A2 2026-11-09 fetched=2026-11-09T23:05:00Z long=1000000 short=0 value=603.4`,
		`c:30303M102 A2 2026-11-10 fetched=2026-11-10T23:05:00Z long=1000000 short=0 value=603.4`,
		`c:808515779 A1 2026-11-09 fetched=2026-11-09T23:05:00Z long=1220890000 short=0 value=1220.89`,
		`c:808515779 A1 2026-11-10 fetched=2026-11-10T23:05:00Z long=1220890000 short=0 value=1220.89`,
		`c:922908769 A1 2026-11-09 fetched=2026-11-09T23:05:00Z long=12000000 short=0 value=3748.92`,
		`c:922908769 A1 2026-11-10 fetched=2026-11-10T23:05:00Z long=12000000 short=0 value=3748.92`,
		`i:1973757747 A1 2026-11-09 fetched=2026-11-09T23:05:00Z long=2178800 short=0 value=495.7`,
		`i:1973757747 A1 2026-11-10 fetched=2026-11-10T23:05:00Z long=2178800 short=0 value=495.7`,
	},
	"snapshots": {
		`A1 2026-11-09 fetched=2026-11-09T23:05:00Z cash=153217 type=CASH`,
		`A1 2026-11-10 fetched=2026-11-10T23:05:00Z cash=153217 type=CASH`,
		`A2 2026-11-09 fetched=2026-11-09T23:05:00Z cash=521044 type=MARGIN margin=0 short=0`,
		`A2 2026-11-10 fetched=2026-11-10T23:05:00Z cash=-1234 type=MARGIN margin=0 short=0`,
		`A4 2026-11-09 fetched=2026-11-09T23:05:00Z type=MARGIN margin=0 short=0`,
		`A4 2026-11-10 fetched=2026-11-10T23:05:00Z type=MARGIN margin=0 short=0`,
	},
	"waiting": {
		`M.2 A1 2024-05-31 "Bank Interest" amount=37 held_out`,
		`M.1 A1 2026-09-22 "Qualified Dividend" amount=26 uncertain`,
	},
	"coverage": {
		`A1 ...135 api 2023-06-12 2025-06-01`,
		`A1 ...135 api 2026-09-02 2026-11-09`,
		`A1 ...135 file_filtered 2023-12-15 2026-09-22`,
		`A1 ...135 snapshot 2026-11-09 2026-11-10`,
		`A2 ...398 api 2025-01-14 2026-08-03`,
		`A2 ...398 api 2026-09-02 2026-11-09`,
		`A2 ...398 snapshot 2026-11-09 2026-11-10`,
		`A3 ...398 file_unknown 2026-02-10 2026-07-01`,
		`A4 ...783 snapshot 2026-11-09 2026-11-10`,
	},
	"uncounted": {
		`M.1 A1 uncertain`,
		`M.2 A1 held_out`,
		`M.3 A1 linked matched_with=T73508261951`,
	},
}

// checkToolRead requires that a tool export read the tool exports' store
// as newToolStore leaves it as the design says (2 of the design): a read of
// every account with no bound holds every field as the store does; a read
// of one account, or up to a day, holds what that selection takes of it
// (toolSelected), and nothing else. It returns what the read of every
// account holds, as toolStore.lines writes it, which the later checks of
// the store change. TestToolExport runs it, and the later checks, on its
// store, which the package builds once.
func checkToolRead(t *testing.T, ts *toolStore) map[string][]string {
	t.Helper()
	all := ts.read(t, 0, "")
	want := maps.Clone(toolStoreLines)
	want["store"] = storeStrings(t, ts.s, "SELECT created_at FROM store")
	compareToolLines(t, "every account", ts.lines(all), want)
	// What no read takes: a snapshot taken during a session.
	if n := storeInt(t, ts.s, "SELECT count(*) FROM positions WHERE in_session_date IS NOT NULL"); n != 2 {
		t.Errorf("the store holds %d positions of a snapshot taken during a session, want 2", n)
	}

	// Each bound, at a day that some of what it bounds falls on, and that
	// some falls after: one account's rows, legs, snapshots, waiting rows,
	// coverage and uncounted entries, and the other's, with the row without
	// a day; the rows, the legs of the spread and the rows that wait for you
	// up to a day; and every account's positions up to the first evening.
	for _, c := range []struct {
		account int64
		to      nytime.Date
	}{{ts.individual, "2026-09-22"}, {ts.options, "2026-09-15"}, {0, "2026-09-14"}, {ts.options, "2026-11-09"}} {
		compareToolLines(t, fmt.Sprintf("A%d up to %s", c.account, c.to), ts.lines(ts.read(t, c.account, c.to)),
			ts.lines(toolSelected(all, c.account, c.to)))
	}
	return want
}

// checkToolReadLater requires, once importLater has imported the later
// files, that a read of every account up to 2026-11-13 hold what want, the
// read of checkToolRead, holds and the later files' rows, names and
// coverage: the row posted on 11-16 "as of" 11-09 is read by its day; their
// rows come after every other; each later export's symbols are names of
// the security that the account's first export named. It returns what a
// read of every account then holds.
func checkToolReadLater(t *testing.T, ts *toolStore, want map[string][]string) map[string][]string {
	t.Helper()
	want = maps.Clone(want)
	want["rows"] = append(slices.Clone(want["rows"]),
		`L.1 A3 file day=2026-11-09 posted=2026-11-16 kind="Qualified Dividend" description="ABCD INDUSTRIES INC" amount=420 fees=0 gross=420 as_of=2026-11-20 symbol=ABCD key=s:FILE:ABCD@2026-07-20 category=dividend`,
		`L.2 A3 file day=2026-11-13 posted=2026-11-13 kind="Qualified Dividend" description="MNOP HOLDINGS INC" amount=360 fees=0 gross=360 as_of=2026-11-20 symbol=MNOP key=s:FILE:MNOP@2026-07-20 category=dividend flags=missing_from_later;near_duplicate`,
		`R.1 A3 file day=2026-11-13 posted=2026-11-13 kind="Qualified Dividend" description="MNOP HOLDINGS INC" amount=306 fees=0 gross=306 as_of=2026-11-23 symbol=MNOP key=s:FILE:MNOP@2026-07-20 category=dividend flags=near_duplicate`)
	want["names"] = toolInsert(t, want["names"], `s:FILE:ABCD@2026-07-20 symbol ABCD first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`s:FILE:ABCD@2026-07-20 symbol ABCD from=s:FILE:ABCD@2026-11-20 first=2026-11-20T14:00:00Z last=2026-11-20T14:00:00Z`)
	want["names"] = toolInsert(t, want["names"], `s:FILE:MNOP@2026-07-20 symbol MNOP first=2026-07-20T13:00:00Z last=2026-07-20T13:00:00Z`,
		`s:FILE:MNOP@2026-07-20 symbol MNOP from=s:FILE:MNOP@2026-11-23 first=2026-11-23T14:00:00Z last=2026-11-23T14:00:00Z`,
		`s:FILE:MNOP@2026-07-20 symbol MNOP from=s:FILE:MNOP@2026-11-20 first=2026-11-20T14:00:00Z last=2026-11-20T14:00:00Z`)
	want["coverage"] = toolInsert(t, want["coverage"], `A3 ...398 file_unknown 2026-02-10 2026-07-01`, `A3 ...398 file 2026-11-13 2026-11-16`)
	compareToolLines(t, "every account up to 2026-11-13, after the later files", ts.lines(ts.read(t, 0, "2026-11-13")), want)
	return want
}

// toolDecisions are, after toolLaterDecision, what checkToolReadDecided
// reads: a decision that keeps a journal paired with its other side, and
// one that this side does not count.
func toolDecisions(ts *toolStore) []Decision {
	return []Decision{
		{Kind: "pair", Subject: fmt.Sprintf("a:%d:73600000102", ts.individual), Value: fmt.Sprintf("a:%d:73600000101", ts.options)},
		{Kind: "count", Subject: fmt.Sprintf("a:%d:73600000101", ts.options), Value: "no"},
	}
}

// toolLaterDecision is the decision of 11.3 of the design, once the later
// files are in: you uncount the dividend that Schwab corrected, whose
// correction keeps its flag.
func toolLaterDecision(ts *toolStore) Decision {
	return Decision{Kind: "count", Subject: subjectOf(ts.later, 2), Value: "no"}
}

// checkToolReadDecided requires, once toolLaterDecision and toolDecisions
// are decided, that a read of every account hold what want, the read of
// checkToolReadLater, holds, but for the rows that you uncounted, which are
// uncounted entries; and when your decision keeps a journal paired with its
// other side, which you uncount, the journal reads its partner as not
// counted.
func checkToolReadDecided(t *testing.T, ts *toolStore, want map[string][]string) {
	t.Helper()
	want = maps.Clone(want)
	rows := want["rows"][:0:0]
	for _, l := range want["rows"] {
		switch {
		case strings.HasPrefix(l, "L.2 "), strings.HasPrefix(l, "T73600000101 "):
			continue
		case strings.HasPrefix(l, "T73600000102 "):
			l = strings.Replace(l, " counted=true ", " counted=false ", 1)
		}
		rows = append(rows, l)
	}
	want["rows"] = rows
	want["uncounted"] = append(slices.Clone(want["uncounted"]), `T73600000101 A2 user`, `L.2 A3 user`)
	compareToolLines(t, "every account after your decisions", ts.lines(ts.read(t, 0, "")), want)
}

// TestToolReadNamesOfOneSource reads, from a real store, three securities
// that one source names by a ticker and, from a delisting or a cash merger
// on, by a CUSIP, and that the store dates both names of at one instant: an
// API read by its answer, a transactions -json import by the import, and a
// History file by its export. Of the API's names, a tie of a ticker and a
// CUSIP gives the CUSIP, so the first two are delisted in both tools, the
// one whose sale you uncount too; a History file's CUSIP names never delist
// a security, so the third is written with its ticker and named by its
// CUSIP, in PP's symbols_changed and in Ghostfolio's yahoo_idle (I-2, I-3
// and I-12 of the consolidation).
func TestToolReadNamesOfOneSource(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ny := nytime.NewYork()
	trade := func(id, instrument int64, symbol string, at time.Time, quantity, net float64) string {
		return fmt.Sprintf(`{"activityId": %d, "time": %q, "accountNumber": %q, "type": "TRADE", "status": "VALID",
			"subAccount": "CASH", "tradeDate": %q, "netAmount": %v, "transferItems": [{"instrument": {"assetType": "EQUITY",
			"status": "ACTIVE", "symbol": %q, "instrumentId": %d, "closingPrice": 10.0}, "amount": %v, "cost": %v,
			"price": 10.0, "positionEffect": "OPENING"}]}`,
			id, schwabTime(at), individual, schwabTime(midnight(nytime.DateOf(at))), net, symbol, instrument, quantity, net)
	}
	read := time.Date(2022, time.December, 31, 5, 0, 0, 0, time.UTC)
	record(t, s, listRead(individual, time.Date(2021, time.January, 1, 5, 0, 0, 0, time.UTC), read, read,
		decodeRow(t, trade(73700000101, 900000001, "TKRQ", time.Date(2021, time.March, 1, 10, 0, 0, 0, ny), 10, -100)),
		decodeRow(t, trade(73700000102, 900000001, "69389D107", time.Date(2022, time.June, 15, 10, 0, 0, 0, ny), -10, 5))))
	importFiles(t, s, ImportOptions{}, "transactions.json", cliJSON(t,
		trade(73700000201, 900000002, "TKRP", time.Date(2021, time.March, 2, 10, 0, 0, 0, ny), 10, -100),
		trade(73700000202, 900000002, "69389E105", time.Date(2022, time.June, 16, 10, 0, 0, 0, ny), -10, 5)))
	item := map[int]string{0: "66000301", 1: "66000301", 2: "66000301"}
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260115-090000", "json"), historyJSON("01/01/2025", "01/14/2026", item,
		csvRow{"12/01/2025", "Cash Merger", "69389C109", "MRGR CORP CASH MERGER", "", "", "", "$500.00"},
		csvRow{"12/01/2025", "Cash Merger Adj", "69389C109", "MRGR CORP", "-10", "", "", ""},
		csvRow{"03/03/2025", "Buy", "MRGR", "MRGR CORP", "10", "$40.00", "", "-$400.00"}))
	uncounted := fmt.Sprintf("a:%d:73700000102", storeInt(t, s, "SELECT account_id FROM account WHERE number = ?", individual))
	if _, err := s.Decide(t.Context(), Decision{Kind: "count", Subject: uncounted, Value: "no"}); err != nil {
		t.Fatal(err)
	}
	var in *toolInput
	if err := s.withRead(t.Context(), func(r *rtx) error {
		var err error
		in, err = readToolInput(r, 0, "2026-11-12")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key, ticker, cusip string
		delisted           bool
	}{
		{"i:900000001", "TKRQ", "69389D107", true}, {"i:900000002", "TKRP", "69389E105", true}, {"x:66000301", "MRGR", "69389C109", false},
	} {
		var seen []time.Time
		for _, n := range in.names {
			if n.key == c.key && n.kind == "symbol" {
				seen = append(seen, n.firstSeen, n.lastSeen)
			}
		}
		if len(seen) < 4 || slices.ContainsFunc(seen, func(at time.Time) bool { return !at.Equal(seen[0]) }) {
			t.Errorf("%s: its symbols are seen at %v, not at one instant", c.key, seen)
		}
		identity, manual, ticker, whys := c.cusip, "delisted", "", "no_ticker delisted"
		if !c.delisted {
			identity, manual, ticker, whys = c.ticker, "", c.ticker, whyNamedByCUSIP
		}
		for _, tool := range []string{toolPP, toolGF} {
			e := *in
			e.tool = tool
			events, _ := toolEvents(&e)
			n := 0
			for _, ev := range events {
				if sec := ev.security; sec != nil && sec.key == c.key {
					n++
					if sec.delisted != c.delisted || sec.identity() != identity || sec.manual != manual || sec.ppTicker() != ticker || whysOf(sec) != whys {
						t.Errorf("%s %s: %s is of %s, delisted %v, MANUAL %q, PP ticker %q, symbols changed %q", c.key, tool, ev.ref, sec.identity(), sec.delisted, sec.manual, sec.ppTicker(), whysOf(sec))
					}
				}
			}
			if n == 0 {
				t.Errorf("%s %s: no records", c.key, tool)
			}
			// Its last row is of 2025-12-01, within the year: Ghostfolio's
			// list has it for its CUSIP alone.
			idle := slices.ContainsFunc(yahooIdle(&e, events), func(i toolIdle) bool { return i.symbol == c.ticker && i.why == whyNamedByCUSIP })
			if tool == toolGF && idle == c.delisted {
				t.Errorf("%s: yahoo_idle lists %s named by its CUSIP: %v", c.key, c.ticker, idle)
			}
		}
	}
}

// TestToolReadRenameRestated reads, from a real store, a security renamed
// between two History files that hold its first buy, whose entry takes the
// later file's ticker (fileEntry), as does a second buy that the later file
// alone holds, of a day between the two; and an API key of the old ticker,
// which sold it under that ticker before the later file: the file key's
// names are dated by the two files' exports. Nothing but the old ticker ties
// the two keys, neither of a CUSIP, so both tools write each on its own and
// list it with the other (I-4 of the consolidation).
func TestToolReadRenameRestated(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ny := nytime.NewYork()
	item := map[int]string{0: "66000401"}
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20250115-090000", "json"), historyJSON("01/01/2024", "01/14/2025", item,
		csvRow{"03/11/2024", "Buy", "JKLM", "JKLM CORP", "10", "$150.00", "", "-$1500.00"}))
	// The later file holds a second buy, of a day between the two files,
	// under the new ticker too, as of its export.
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20261111-090000", "json"), historyJSON("01/01/2024", "11/10/2026",
		map[int]string{0: "66000401", 1: "66000401"},
		csvRow{"06/02/2025", "Buy", "JKLN", "JKLN CORP", "5", "$160.00", "", "-$800.00"},
		csvRow{"03/11/2024", "Buy", "JKLN", "JKLN CORP", "10", "$150.00", "", "-$1500.00"}))
	sale := fmt.Sprintf(`{"activityId": 73700000401, "time": %q, "accountNumber": %q, "type": "TRADE", "status": "VALID",
		"subAccount": "CASH", "tradeDate": %q, "netAmount": 1700, "transferItems": [{"instrument": {"assetType": "EQUITY",
		"status": "ACTIVE", "symbol": "JKLM", "instrumentId": 900000401, "closingPrice": 10.0}, "amount": -10, "cost": -1700,
		"price": 170.0, "positionEffect": "CLOSING"}]}`,
		schwabTime(time.Date(2026, time.March, 16, 10, 0, 0, 0, ny)), individual, schwabTime(midnight("2026-03-16")))
	read := time.Date(2026, time.March, 16, 22, 30, 0, 0, time.UTC)
	record(t, s, listRead(individual, time.Date(2026, time.March, 1, 5, 0, 0, 0, time.UTC), read, read, decodeRow(t, sale)))
	var in *toolInput
	if err := s.withRead(t.Context(), func(r *rtx) error {
		var err error
		in, err = readToolInput(r, 0, "2026-11-12")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]nytime.Date{}
	for _, n := range in.names {
		if n.key == "x:66000401" && n.kind == "symbol" {
			seen[n.value] = nytime.DateOf(n.firstSeen)
		}
	}
	if seen["JKLM"] != "2025-01-15" || seen["JKLN"] != "2026-11-11" {
		t.Fatalf("the file's key: names first seen %v", seen)
	}
	for _, tool := range []string{toolPP, toolGF} {
		e := *in
		e.tool = tool
		events, _ := toolEvents(&e)
		var refs []string
		for _, ev := range events {
			sec := ev.security
			ticker, _, _ := ppIdentifiers(sec)
			refs = append(refs, ev.ref)
			if !sec.unsure || sec.identity() != sec.key || sec.gfDataSource() != "MANUAL" || ticker != "" {
				t.Errorf("%s %s of %s: unsure %v, identity %s, %s, PP ticker %q", tool, ev.ref, sec.key, sec.unsure, sec.identity(), sec.gfDataSource(), ticker)
			}
		}
		if len(refs) != 3 {
			t.Errorf("%s: records %q", tool, refs)
		}
		if u := unsureText(keysUnsure(&e, events)); !slices.Equal(u, []string{
			"i:900000401: x:66000401 schwabtx edit K:i:900000401 -same K:x:66000401", "x:66000401: i:900000401 schwabtx edit K:x:66000401 -same K:i:900000401",
		}) {
			t.Errorf("%s: unsure %q", tool, u)
		}
	}
}

// TestToolReadJoinedHistoryKey reads, from a real store, a security whose
// History file's key and API key the store joined itself, their names
// meeting within days (groupKeys' rule 2), so that its canonical key is the
// API's: a History file exported on 2026-01-04 names it TKNB, its buy of
// 2015 too, while the API's rows of a read of 2026-01-05 name it TKNA up to
// May 2025 and TKNB from June, after its rename; and another company,
// which no snapshot held and the store knows no CUSIP of, took TKNA in
// October 2025, read at once. Q5 holds each name with the key that had it,
// when it is not the security's own. The store holds S's position, two months after the read,
// under the position's key, apart: S knows no CUSIP, and shares TKNA with
// the other company and TKNB with S's position, so both tools write S and
// the other company each on its own, and list each with what the store knows
// of it (I-4 of the consolidation).
func TestToolReadJoinedHistoryKey(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ny := nytime.NewYork()
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260104-090000", "json"), historyJSON("01/01/2015", "01/03/2026",
		map[int]string{0: "66000800"}, csvRow{"03/02/2015", "Buy", "TKNB", "TKNB HOLDINGS INC", "10", "$10.00", "", "-$100.00"}))
	trade := func(id, instrument int64, symbol string, at time.Time, quantity, net float64) schwab.Transaction {
		return decodeRow(t, fmt.Sprintf(`{"activityId": %d, "time": %q, "accountNumber": %q, "type": "TRADE", "status": "VALID",
			"subAccount": "CASH", "tradeDate": %q, "netAmount": %v, "transferItems": [{"instrument": {"assetType": "EQUITY",
			"status": "ACTIVE", "symbol": %q, "instrumentId": %d, "closingPrice": 10.0}, "amount": %v, "cost": %v,
			"price": 10.0, "positionEffect": "OPENING"}]}`,
			id, schwabTime(at), individual, schwabTime(midnight(nytime.DateOf(at))), net, symbol, instrument, quantity, net))
	}
	read := time.Date(2026, time.January, 5, 23, 30, 0, 0, time.UTC)
	record(t, s, listRead(individual, time.Date(2024, time.January, 1, 5, 0, 0, 0, time.UTC), read, read,
		trade(73700000801, 1000800, "TKNA", time.Date(2024, time.March, 1, 10, 0, 0, 0, ny), 10, -100),
		trade(73700000802, 1000800, "TKNA", time.Date(2025, time.May, 15, 10, 0, 0, 0, ny), -5, 60),
		trade(73700000803, 1000800, "TKNB", time.Date(2025, time.June, 16, 10, 0, 0, 0, ny), 5, -70),
		trade(73700000804, 1000800, "TKNB", time.Date(2026, time.January, 2, 10, 0, 0, 0, ny), 1, -15),
		trade(73700000805, 1000801, "TKNA", time.Date(2025, time.October, 1, 10, 0, 0, 0, ny), 2, -80),
		trade(73700000806, 1000801, "TKNA", time.Date(2025, time.November, 3, 10, 0, 0, 0, ny), -2, 82)))
	importFile(t, s, "snapshots.jsonl", snapshotLine(t, time.Date(2026, time.March, 2, 23, 5, 0, 0, time.UTC), json.RawMessage(`{"securitiesAccount": {
		"type": "CASH", "accountNumber": "`+individual+`", "roundTrips": 0,
		"positions": [{"shortQuantity": 0.0, "averagePrice": 10.0, "longQuantity": 11.0, "marketValue": 165.0,
			"instrument": {"assetType": "EQUITY", "cusip": "ZZQ80A107", "symbol": "TKNB", "instrumentId": 1000800}}],
		"initialBalances": {"cashBalance": 1000.0, "liquidationValue": 1165.0},
		"currentBalances": {"cashBalance": 1000.0, "liquidationValue": 1165.0}}}`)))
	if got := storeStrings(t, s, "SELECT security_key FROM security_keys WHERE key IN ('x:66000800', 'i:1000800', 'i:1000801') ORDER BY key"); !slices.Equal(got, []string{"i:1000800", "i:1000801", "i:1000800"}) {
		t.Fatalf("the store holds the keys as of the securities %q", got)
	}
	var in *toolInput
	if err := s.withRead(t.Context(), func(r *rtx) error {
		var err error
		in, err = readToolInput(r, 0, "2026-11-12")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, n := range in.names {
		if n.key == "i:1000800" && n.kind == "symbol" && n.from != "" {
			from = append(from, n.value+" "+n.from)
		}
	}
	if !slices.Contains(from, "TKNB x:66000800") {
		t.Fatalf("S's names of other keys %q", from)
	}
	for _, tool := range []string{toolPP, toolGF} {
		e := *in
		e.tool = tool
		ix := newToolIndex(&e)
		ix.groupAll()
		if sa, qa := ix.security("i:1000800", ""), ix.security("i:1000801", ""); sa.rep == qa.rep || qa.identity() != "i:1000801" {
			t.Errorf("%s: S is of %s, Q of %s, one security %v", tool, sa.identity(), qa.identity(), sa.rep == qa.rep)
		}
		// What the store knows of each: S's description of the History file,
		// its tickers by source, its rows of the file and of the API; Q's
		// ticker of the API's read and its rows; the position's key, of S's
		// CUSIP and ticker, held.
		events, _ := toolEvents(&e)
		s := "[TKNB HOLDINGS INC, TKNA api 2026-01-05, TKNB api 2026-01-05, TKNB history 2026-01-04, rows 2015-03-02..2026-01-02]"
		q := "[, TKNA api 2026-01-05, rows 2025-10-01..2025-11-03]"
		want := []string{
			"i:1000800 " + s + ": i:1000801 " + q + " schwabtx edit K:i:1000800 -same K:i:1000801; " +
				"c:ZZQ80A107 ZZQ80A107 [, TKNB api 2026-03-02, held] schwabtx edit K:i:1000800 -same K:c:ZZQ80A107",
			"i:1000801 " + q + ": i:1000800 " + s + " schwabtx edit K:i:1000801 -same K:i:1000800",
		}
		if got := unsureText(keysUnsure(&e, events), true); !slices.Equal(got, want) {
			t.Errorf("%s: unsure %q", tool, got)
		}
	}
}

// TestToolReadLinks reads, from a real store, the links of a History
// file's rows to the API's rows of two of its securities (QL): the
// individual account's file, exported a year before the API's read, names
// VBR and BND by keys of its own, which the store holds apart from the
// API's, their names a year apart, and links its sale of VBR and BND's
// dividend and tax to the API's rows, which QL reads as one pair of keys
// each; its wire, linked too, is of no security, and its buys, of VBR and
// of another key, come before the API's rows, linked to none. A later
// file's buy of AAPL, exported a day after the read, is linked to the
// API's, whose key the store joined the file's to: one key, no pair. QL is
// the store's, of every account and day, whatever the export selects.
func TestToolReadLinks(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	recordFixtures(t, s)
	importFiles(t, s, ImportOptions{}, "Individual_XXX135_Transactions_20240601-090000.json", historyJSON("02/01/2023", "05/31/2024",
		map[int]string{0: "66000903", 1: "66000903", 2: "66000901", 4: "66000902", 5: "66000901"},
		csvRow{"04/04/2024", "NRA Tax Adj", "BND", "VANGUARD TOTAL BOND MARKET ETF", "", "", "", "-$1.16"},
		csvRow{"04/04/2024", "Qualified Dividend", "BND", "VANGUARD TOTAL BOND MARKET ETF", "", "", "", "$3.86"},
		csvRow{"03/15/2024", "Sell", "VBR", "VANGUARD SMALL CAP VALUE ETF", "0.4513", "$184.51", "", "$83.27"},
		csvRow{"12/15/2023", "Wire Received", "", "WIRED FUNDS RECEIVED", "", "", "", "$10,000.00"},
		csvRow{"03/13/2023", "Buy", "ZZLK", "ZZLK HOLDINGS INC", "5", "$20.00", "", "-$100.00"},
		csvRow{"03/10/2023", "Buy", "VBR", "VANGUARD SMALL CAP VALUE ETF", "2", "$160.00", "", "-$320.00"}))
	importFiles(t, s, ImportOptions{}, "Individual_XXX135_Transactions_20250604-090000.csv",
		historyCSV(csvRow{"06/02/2025", "Buy", "AAPL", "APPLE INC", "3", "$201.82", "", "-$605.46"}))
	keys := []string{
		"s:FILE:AAPL@2025-06-04 i:1973757747", "s:FILE:BND@2024-06-01 x:66000903", "s:FILE:VBR@2024-06-01 x:66000901",
		"s:FILE:ZZLK@2024-06-01 x:66000902", "x:66000901 x:66000901", "x:66000902 x:66000902", "x:66000903 x:66000903",
	}
	if got := storeStrings(t, s, "SELECT key || ' ' || security_key FROM security_keys WHERE key LIKE 'x:%' OR key LIKE 's:FILE:%' ORDER BY key"); !slices.Equal(got, keys) {
		t.Fatalf("the store holds the files' keys as of the securities %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM matches"); n != 5 {
		t.Fatalf("the store links %d rows, want 5", n)
	}
	want := []toolLink{{file: "x:66000901", api: "i:4417355"}, {file: "x:66000903", api: "i:4417302"}}
	for _, c := range []struct {
		account int64
		to      nytime.Date
	}{{0, ""}, {storeInt(t, s, "SELECT account_id FROM account WHERE number = ?", options), "2023-06-30"}} {
		var in *toolInput
		if err := s.withRead(t.Context(), func(r *rtx) error {
			var err error
			in, err = readToolInput(r, c.account, c.to)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(in.links, want) {
			t.Errorf("A%d up to %q: links %+v, want %+v", c.account, c.to, in.links, want)
		}
	}
}

// TestToolReadMarginBalances reads, from a real store, a margin account's
// snapshot whose cash balance reads 0 beside a margin balance in its
// current balances, as Schwab can report a debit, and none in its initial
// ones: Q7 reads the current ones, and PP's cash check passes the account
// over, listed.
func TestToolReadMarginBalances(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, time.November, 10, 23, 5, 0, 0, time.UTC)
	importFile(t, s, "snapshots.jsonl", snapshotLine(t, at, json.RawMessage(`{"securitiesAccount": {"accountNumber": "29470135", "type": "MARGIN",
		"initialBalances": {"cashBalance": 0, "marginBalance": 0, "shortBalance": 0},
		"currentBalances": {"cashBalance": 0, "marginBalance": -2000.0, "shortBalance": 0}}}`)))
	var in *toolInput
	if err := s.withRead(t.Context(), func(r *rtx) error {
		var err error
		in, err = readToolInput(r, 0, "2026-11-12")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(in.snapshots) != 1 {
		t.Fatalf("snapshots %+v", in.snapshots)
	}
	if snap := in.snapshots[0]; snap.accountType != "MARGIN" || snap.cash != centsOf(0) || snap.margin != (sql.NullFloat64{Float64: -2000, Valid: true}) || snap.short != (sql.NullFloat64{Valid: true}) {
		t.Errorf("the snapshot %+v", snap)
	}
	in.tool = toolPP
	events, _ := toolEvents(in)
	if diffs, unchecked := cashCheck(in, events); len(diffs) != 0 || len(unchecked) != 1 || unchecked[0].snapshot != "2026-11-10" || unchecked[0].why != "margin" {
		t.Errorf("differences %+v, unchecked %+v", diffs, unchecked)
	}
}
