package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// txnJSON returns a transaction as Schwab sends one, of the account
// numbered number: activity ID id, at t, of type typ with description desc
// and net amount net, and one cash leg holding net. Its trade date is
// midnight in New York of t's date there.
func txnJSON(number string, id int64, t time.Time, typ, desc string, net float64) string {
	day := midnight(nytime.DateOf(t))
	return fmt.Sprintf(`{"activityId":%d,"time":%q,"description":%q,"accountNumber":%q,"type":%q,"status":"VALID",`+
		`"subAccount":"CASH","tradeDate":%q,"netAmount":%s,"transferItems":[{"instrument":{"assetType":"CURRENCY",`+
		`"status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency","instrumentId":1,"closingPrice":0.0},`+
		`"amount":%s,"cost":0.0}]}`,
		id, txnTime(t), desc, number, typ, txnTime(day), txnFloat(net), txnFloat(net))
}

// txnFloat writes x as a JSON number.
func txnFloat(x float64) string { return strconv.FormatFloat(x, 'f', -1, 64) }

// txnTime writes t as Schwab writes a transaction's times.
func txnTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05+0000") }

// dividend135 is a dividend of ...135 in 2025, which the fixtures do not
// hold, for the cases that read ...135's 2025 from Schwab.
var dividend135 = txnJSON(individual, 73508262101, ny(2025, time.October, 1, 10, 40), "DIVIDEND_OR_INTEREST",
	"Ordinary Dividend~BND", 98.12)

// xyzDividend135 is a dividend of XYZ to ...135 in 2025, which Schwab
// holds, for the case of a symbol in both sources.
var xyzDividend135 = txnJSON(individual, 73508262102, ny(2025, time.August, 15, 10, 30), "DIVIDEND_OR_INTEREST",
	"Ordinary Dividend~XYZ", 1.23)

// withTxnRows adds rows, each the JSON of a transaction, to the fake's
// account numbered number.
func withTxnRows(number string, rows ...string) envOption {
	return withFake(func(f *schwabtest.Server) { f.AddRows(schwabtest.HashFor(number), rows...) })
}

// txnRowsOf returns the rows of an answer of get_transactions.
func txnRowsOf(r result) []map[string]any {
	var out []map[string]any
	rs, _ := r.value["transactions"].([]any)
	for _, x := range rs {
		out = append(out, x.(map[string]any))
	}
	return out
}

// txnCoverageOf returns the coverage row of the account masked as masked, or
// nil.
func txnCoverageOf(r result, masked string) map[string]any {
	cs, _ := r.value["coverage"].([]any)
	for _, x := range cs {
		if c := x.(map[string]any); c["account"] == masked {
			return c
		}
	}
	return nil
}

// txnRefsOf returns the refs of rows, each with its account, in order.
func txnRefsOf(rows []map[string]any) []string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprint(r["ref"], "@", r["account"]))
	}
	return out
}

// txnWithCursor returns args, a JSON object, with cursor set to cur.
func txnWithCursor(t *testing.T, args, cur string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatal(err)
	}
	m["cursor"] = cur
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// txnPages calls tool with args and follows next_cursor to the last page, at
// most most pages, checking each as env.call does.
func (e *env) txnPages(tool, args string, most int) []result {
	e.t.Helper()
	var out []result
	for cur := ""; len(out) < most; {
		a := args
		if cur != "" {
			a = txnWithCursor(e.t, args, cur)
		}
		r := e.call(tool, a)
		if r.isError {
			e.t.Fatalf("page %d of %s: %s", len(out)+1, args, r.text)
		}
		out = append(out, r)
		next, _ := r.value["next_cursor"].(string)
		if next == "" {
			return out
		}
		cur = next
	}
	e.t.Fatalf("%s: more than %d pages", args, most)
	return nil
}

// get_transactions' cases (design 15.2). empty is a window without rows
// and without a store; full is design 10.5's example: ...398's 2025 is
// whole in the shared store and read from it, ...135's is not and is read
// from Schwab, where a dividend of it waits, and ...783 has none; the
// other cases filter by type and symbol, page, read the store whatever it
// covers, add the detail and the rows the store does not count, and read
// from Schwab an account whose days the store holds only in part.
var getTransactionsCases = []toolCase{
	{
		tool: "get_transactions", name: "empty", opts: []envOption{withoutStore()},
		args: `{"from":"2026-10-01","to":"2026-10-31"}`,
		check: func(t *testing.T, _ *env, r result) {
			if len(txnRowsOf(r)) != 0 || len(r.value["totals"].([]any)) != 0 || r.value["source"] != "schwab" {
				t.Errorf("an empty window: %s", r.text)
			}
			if !slices.Contains(r.warnings(), "store_unavailable") || !strings.Contains(r.text, "[no_store]") {
				t.Errorf("no warning says that there is no store: %v", r.warnings())
			}
		},
	},
	{
		tool: "get_transactions", name: "full", opts: []envOption{withTxnRows(individual, dividend135)},
		args: `{"categories":["dividend"],"from":"2025-01-01","to":"2025-12-31"}`,
		check: func(t *testing.T, _ *env, r result) {
			if r.value["source"] != "schwab+store" {
				t.Errorf("source %v, want schwab+store", r.value["source"])
			}
			for masked, from := range map[string]string{"...135": "schwab", "...398": "store", "...783": "schwab"} {
				if c := txnCoverageOf(r, masked); c == nil || c["read_from"] != from {
					t.Errorf("%s was read from %v, want %s", masked, c["read_from"], from)
				}
			}
			want := []string{"T73508262101@...135", "T73508262001@...398", "C2cdf9679.2@...398"}
			if got := txnRefsOf(txnRowsOf(r)); !slices.Equal(got, want) {
				t.Errorf("rows %v, want %v", got, want)
			}
			totals, _ := json.Marshal(r.value["totals"])
			if string(totals) != `[{"amount":198.48,"category":"dividend","count":3,"fees":0}]` {
				t.Errorf("totals %s", totals)
			}
			if w := r.warnings(); !slices.Equal(w, []string{"store_waiting", "before_decisions"}) {
				t.Errorf("warnings %v", w)
			}
		},
	},
	{
		tool: "get_transactions", name: "types", opts: []envOption{withTxnRows(individual, dividend135)},
		args: `{"types":["trade"],"from":"2025-01-01","to":"2025-12-31"}`,
		check: func(t *testing.T, _ *env, r result) {
			for _, row := range txnRowsOf(r) {
				if row["type"] != "TRADE" || row["row_kind"] != "api" {
					t.Errorf("a row of another type, or of a file: %v", row)
				}
			}
			if len(txnRowsOf(r)) != 2 || !slices.Contains(r.warnings(), "types_filter") {
				t.Errorf("types: %s", r.text)
			}
		},
	},
	{
		tool: "get_transactions", name: "symbol", opts: []envOption{withTxnRows(individual, xyzDividend135)},
		args: `{"symbol":"xyz","from":"2025-01-01","to":"2025-12-31"}`,
		check: func(t *testing.T, e *env, r result) {
			for _, row := range txnRowsOf(r) {
				if row["symbol"] != "XYZ" {
					t.Errorf("a row of another symbol: %v", row)
				}
			}
			if got := txnRefsOf(txnRowsOf(r)); !slices.Equal(got, []string{
				"T73508262002@...398", "T73508262102@...135",
				"C2cdf9679.2@...398", "C2cdf9679.1@...398",
			}) {
				t.Errorf("the rows of XYZ: %v", got)
			}
			// Spaces around the symbol are no part of it, as in get_positions.
			padded := e.call("get_transactions", `{"symbol":" xyz ","from":"2025-01-01","to":"2025-12-31"}`)
			got, _ := json.Marshal([]any{txnRefsOf(txnRowsOf(padded)), padded.value["totals"]})
			want, _ := json.Marshal([]any{txnRefsOf(txnRowsOf(r)), r.value["totals"]})
			if string(got) != string(want) {
				t.Errorf("the rows and totals of \" xyz \": %s, want those of \"xyz\": %s", got, want)
			}
		},
	},
	{
		tool: "get_transactions", name: "pages", opts: []envOption{withTxnRows(individual, dividend135)},
		args: `{"from":"2025-01-01","to":"2025-12-31","limit":2}`, check: txnCheckPages,
	},
	{
		tool: "get_transactions", name: "source_store", args: `{"source":"store","account":"135","from":"2024-01-01","to":"2025-06-30"}`,
		check: func(t *testing.T, _ *env, r result) {
			c := txnCoverageOf(r, "...135")
			whole, _ := json.Marshal(c["whole"])
			if r.value["source"] != "store" || c["read_from"] != "store" || string(whole) != `[{"from":"2024-01-01","to":"2024-12-30"}]` {
				t.Errorf("the store as far as it covers: %v", c)
			}
			if !slices.Contains(r.warnings(), "store_coverage_partial") || len(txnRowsOf(r)) != 5 {
				t.Errorf("partial coverage: %s", r.text)
			}
		},
	},
	{
		tool: "get_transactions", name: "detail_full", opts: []envOption{withTxnRows(individual, dividend135)},
		args: `{"from":"2025-06-01","to":"2025-11-30","detail":"full","include_uncounted":true}`,
		check: func(t *testing.T, _ *env, r result) {
			counted := map[string]any{}
			for _, row := range txnRowsOf(r) {
				d, _ := row["detail"].(map[string]any)
				if legs, _ := d["legs"].([]any); len(legs) == 0 {
					t.Errorf("%v has no legs", row["ref"])
				}
				counted[row["ref"].(string)] = row["counted"]
			}
			if counted["C2cdf9679.4"] != false || counted["T73508262002"] != true || counted["T73508262101"] != nil {
				t.Errorf("counted: %v", counted)
			}
		},
	},
	{
		tool: "get_transactions", name: "covers_some", args: `{"account":"135","from":"2024-07-01","to":"2025-06-30"}`,
		check: func(t *testing.T, _ *env, r result) {
			c := txnCoverageOf(r, "...135")
			partial, _ := json.Marshal(c["partial"])
			if c["read_from"] != "schwab" || string(partial) != `[{"from":"2024-07-01","source":"api","to":"2024-12-30"}]` {
				t.Errorf("...135: %v", c)
			}
			if !slices.Contains(r.warnings(), "store_covers_some") {
				t.Errorf("warnings %v", r.warnings())
			}
		},
	},
}

// txnCheckPages follows the pages case's cursor to its last page: every row
// once, in the order of the first page's, each page with the same totals
// and coverage.
func txnCheckPages(t *testing.T, e *env, r result) {
	all := e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)
	pages := e.txnPages("get_transactions", `{"from":"2025-01-01","to":"2025-12-31","limit":3}`, 10)
	var got []string
	for i, p := range pages {
		got = append(got, txnRefsOf(txnRowsOf(p))...)
		for _, field := range []string{"totals", "coverage"} {
			a, _ := json.Marshal(p.value[field])
			b, _ := json.Marshal(all.value[field])
			if string(a) != string(b) {
				t.Errorf("page %d's %s differ from one call's:\n%s\n%s", i+1, field, a, b)
			}
		}
	}
	if want := txnRefsOf(txnRowsOf(all)); len(want) < 5 || !slices.Equal(got, want) {
		t.Errorf("pages of 3 hold %v, one call %v", got, want)
	}
	if n := len(txnRowsOf(r)); n != 2 {
		t.Errorf("the first page holds %d rows, want 2", n)
	}
}

// get_transaction's cases (design 15.2): an unknown id; a Schwab row with
// legs and the store's view; a C ref, which only the store knows; Schwab's
// JSON; and, without a login, the store's row of a T ref.
var getTransactionCases = []toolCase{
	{
		tool: "get_transaction", name: "empty", args: `{"id":"99999999999","account":"135"}`,
		check: func(t *testing.T, _ *env, r result) {
			if r.code() != "not_found" || !strings.Contains(r.text, "99999999999") || !strings.Contains(r.text, "...135") {
				t.Errorf("an unknown id: %s", r.text)
			}
		},
	},
	{
		tool: "get_transaction", name: "full", args: `{"id":"T73508261946"}`,
		check: func(t *testing.T, _ *env, r result) {
			if r.value["source"] != "schwab+store" || r.value["store"] == nil || r.value["raw"] != nil {
				t.Errorf("a Schwab row with the store's view: %s", r.text)
			}
		},
	},
	{
		tool: "get_transaction", name: "c_ref", args: `{"id":"C2cdf9679.2"}`,
		check: func(t *testing.T, _ *env, r result) {
			if r.value["source"] != "store" || r.value["transaction"].(map[string]any)["row_kind"] != "file" {
				t.Errorf("a C ref: %s", r.text)
			}
		},
	},
	{
		tool: "get_transaction", name: "include_raw", args: `{"id":"73508261946","include_raw":true}`,
		check: func(t *testing.T, _ *env, r result) {
			raw, _ := r.value["raw"].(map[string]any)
			if raw["accountNumber"] != "...135" || raw["activityId"] != 73508261946.0 {
				t.Errorf("raw: %v", raw)
			}
		},
	},
	{
		tool: "get_transaction", name: "no_login", opts: []envOption{withoutLogin()}, args: `{"id":"73508261946"}`,
		check: func(t *testing.T, _ *env, r result) {
			if r.value["source"] != "store" || !slices.Equal(r.warnings(), []string{"api_unavailable"}) ||
				!strings.Contains(r.text, "[not_logged_in]") {
				t.Errorf("the store's row without a login: %s", r.text)
			}
		},
	},
}

// A row's time in the ledger's order is the store's instant form: Schwab's
// rows and the store's merge in one order only if the server writes their
// times as the store does.
func TestStoreInstantIsTheStores(t *testing.T) {
	t.Parallel()
	r, err := history.OpenReader(t.Context(), history.ReaderOptions{Path: sharedStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	numbers, err := r.AccountNumbers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var accounts []history.LedgerAccount
	for _, n := range numbers {
		accounts = append(accounts, history.LedgerAccount{ID: n.ID, Key: "A" + strconv.FormatInt(n.ID, 10)})
	}
	p, err := r.Ledger(t.Context(), history.LedgerQuery{Accounts: accounts, From: "2023-01-01", To: "2025-12-31", All: true, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(schwabtest.Fixture(t, "transactions.json"), &raw); err != nil {
		t.Fatal(err)
	}
	times := map[string]time.Time{}
	for _, b := range raw {
		var row schwab.Transaction
		if err := json.Unmarshal(b, &row); err != nil {
			t.Fatal(err)
		}
		times["T"+strconv.FormatInt(row.ActivityID, 10)] = row.Time
	}
	checked := 0
	for _, x := range p.Rows {
		if at, ok := times[x.Ref]; ok {
			checked++
			if got := storeInstant(at); got != x.Key.Time {
				t.Errorf("%s: the server writes its time %q, the store %q", x.Ref, got, x.Key.Time)
			}
		}
		if x.Source == "file" && x.Key.Time != storeInstant(time.Time{}) {
			t.Errorf("%s, a file's row, has the time %q", x.Ref, x.Key.Time)
		}
	}
	if checked < 5 {
		t.Errorf("only %d of the store's rows are the fixtures'", checked)
	}
}

// The transaction tools' cases of the shared tests. They are added here,
// once the core's package-level vars have added theirs, rather than through
// addToolCases, addLeaks and addSizeCases, whose result, which no caller
// reads, unparam reports once a second file ignores it.
func init() {
	toolCases = append(append(toolCases, getTransactionsCases...), getTransactionCases...)
	leakPlantings = append(leakPlantings, plantTransactions)
	leakCalls = append(leakCalls, transactionLeakCalls...)
	sizeCases = append(sizeCases, transactionSizeCases...)
}

// The leak world's numbers that only the transaction tools' rows hold:
// another account in a transfer leg, an unknown key's integer, and a user
// object's userId.
const (
	otherAccount = "98765432"
	unknownInt   = "87654321"
	userID       = "12345678"
)

// The leak world's rows of ...135 that Schwab holds (design 15.3): a
// journal to ...398, with a user object as the spec's UserDetails has it
// and two keys no table knows, one of text and one of an 8-digit integer;
// a trade whose legs' descriptions hold ...398 glued to letters and an
// account no one knows; two journals to the 7-digit account, whose number
// one writes in two groups, which mask.Text leaves; and a row whose time
// is a number, which its problem quotes.
var leakRows = []string{
	`{"activityId":73508263001,"time":"2026-10-05T14:00:00+0000","description":"JOURNAL TO 61803398 FROM ` + otherAccount + `",` +
		`"accountNumber":"29470135",` +
		`"type":"JOURNAL","status":"VALID","subAccount":"CASH","tradeDate":"2026-10-05T04:00:00+0000","netAmount":-100.0,` +
		`"user":{"firstName":"Jane","lastName":"Doe","login":"jdoe1234","systemUserName":"jdoe","userId":` + userID + `},` +
		`"newKey":"note for jdoe1234","newNumber":` + unknownInt + `,` +
		`"transferItems":[{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency",` +
		`"instrumentId":1,"closingPrice":0.0},"amount":-100.0,"cost":0.0}]}`,
	`{"activityId":73508263002,"time":"2026-10-06T14:00:00+0000","accountNumber":"29470135","type":"TRADE","status":"VALID",` +
		`"subAccount":"CASH","tradeDate":"2026-10-06T14:00:00+0000","netAmount":-10.0,"orderId":1004517688099,"transferItems":[` +
		`{"instrument":{"assetType":"EQUITY","status":"ACTIVE","symbol":"XYZ","description":"ACCT61803398 TRF","instrumentId":7003,` +
		`"closingPrice":10.0},"amount":1.0,"cost":-10.0,"price":10.0,"positionEffect":"OPENING"},` +
		`{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"TRF TO ` + otherAccount + `",` +
		`"instrumentId":1,"closingPrice":0.0},"amount":0.0,"cost":0.0,"feeType":"COMMISSION"}]}`,
	`{"activityId":73508263003,"time":"2026-10-07T14:00:00+0000","description":"Kids 7654321 gift","accountNumber":"29470135",` +
		`"type":"JOURNAL","status":"VALID","subAccount":"CASH","tradeDate":"2026-10-07T04:00:00+0000","netAmount":-5.0,` +
		`"transferItems":[{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency",` +
		`"instrumentId":1,"closingPrice":0.0},"amount":-5.0,"cost":0.0}]}`,
	`{"activityId":73508263005,"time":"2026-10-09T14:00:00+0000","description":"Allowance 765-4321 weekly","accountNumber":"29470135",` +
		`"type":"JOURNAL","status":"VALID","subAccount":"CASH","tradeDate":"2026-10-09T04:00:00+0000","netAmount":-2.0,` +
		`"transferItems":[{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency",` +
		`"instrumentId":1,"closingPrice":0.0},"amount":-2.0,"cost":0.0}]}`,
	`{"activityId":73508263004,"time":29470135,"description":"Funds Received","accountNumber":"29470135","type":"CASH_RECEIPT",` +
		`"status":"VALID","subAccount":"CASH","tradeDate":"2026-10-08T04:00:00+0000","netAmount":7.0,` +
		`"transferItems":[{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency",` +
		`"instrumentId":1,"closingPrice":0.0},"amount":7.0,"cost":0.0}]}`,
}

// leakTransactionsCalls are the leak world's calls of get_transactions,
// whose cursors' digests the world's check reads.
var leakTransactionsCalls = []string{
	`{"account":"398","source":"schwab","from":"2026-10-01","to":"2026-10-31"}`,
	`{"from":"2026-10-01","to":"2026-11-13","detail":"full"}`,
	`{"account":"29470135","from":"2026-10-01","to":"2026-11-13","limit":1}`,
	`{"account":"...135","source":"store","from":"2025-06-01","to":"2025-06-30","include_uncounted":true,"detail":"full"}`,
}

// The world of TestNoAccountNumberLeaks that the transaction tools add:
// Schwab's rows of ...135 above; Schwab's error bodies, which quote an
// account number, on the first request for transactions and on those for
// one transaction; and a History export of ...135 in the world's store
// whose descriptions hold numbers, which the store masks but for the
// 7-digit one, written whole and in two groups, which only the known
// numbers catch. Nothing sent may name the planted user, and no cursor's
// digest may be one of its arguments with a planted number for the
// account's hash.
func plantTransactions(t *testing.T, w *leakWorld) {
	w.plant(otherAccount, unknownInt, userID)
	w.opts = append(w.opts, withFake(func(f *schwabtest.Server) {
		// The row whose time is a number is served by the time it would have.
		f.AddRows(schwabtest.HashFor(individual), leakRows[:4]...)
		f.AddRow(schwabtest.HashFor(individual), schwabtest.Row{JSON: leakRows[4], Time: ny(2026, time.October, 8, 10, 0)})
		f.Queue(schwabtest.TransactionsRoute, schwabtest.Answer(400, `{"message":"Invalid account number 29470135"}`))
		f.Queue(schwabtest.TransactionRoute, schwabtest.NotAuthorized, schwabtest.Whitelabel, schwabtest.Whitelabel)
	}))
	if w.store == "" {
		t.Fatal("the core's planting made no store for the world")
	}
	txnImport(t, w.store, individual, "Individual_XXX135_Transactions_20250701-120000.csv",
		`"06/05/2025","Journal","","JOURNAL TO 61803398","","","","-$100.00"`,
		`"06/12/2025","Journal","","Kids 7654321 transfer","","","","-$50.00"`,
		`"06/13/2025","Journal","","Tuition 765 – 4321","","","","-$25.00"`,
		`"06/20/2025","Wire Received","","WIRE FROM `+otherAccount+`","","","","$500.00"`)
	w.masked = append(w.masked, "JOURNAL TO ...398", "ACCT...398 TRF", "TRF TO ...432", "Kids ...321 gift", "Kids ...321 transfer",
		"Allowance ...321 weekly", "Tuition ...321",
		"WIRE FROM ...432", `"accountNumber":"...135"`, `"user":{"firstName":"[withheld]"`, `"newNumber":"...321"`,
		"Invalid account number ...135", "Client not authorized")
	w.forbidden = append(w.forbidden, "jdoe", "Jane", "Doe")
	w.checks = append(w.checks, checkCursorDigests)
	w.sides = append(w.sides, leakSide{
		// A gateway's fault and an OAuth error, each holding numbers that no
		// account the server knows has, in its code too; preferences that
		// cannot be read; and an account that only the store knows.
		name: "Schwab refuses the transactions",
		opts: func(string) []envOption {
			return []envOption{withFake(func(f *schwabtest.Server) {
				f.Override(schwabtest.TransactionsRoute, schwabtest.Answer(http.StatusBadGateway,
					`{"fault":{"faultstring":"Bad `+unknownLong+`","detail":{"errorcode":"acct.`+unknownLong+`"}}}`))
				f.Override(schwabtest.TransactionRoute, schwabtest.Answer(http.StatusForbidden,
					`{"error":"Account `+unknownLong+` is restricted for `+unknownShort+`"}`))
				f.Override(schwabtest.UserPreferenceRoute, schwabtest.Answer(http.StatusInternalServerError,
					`{"message":"no preferences for `+unknownLong+`"}`))
				f.RemoveAccount(schwabtest.HashFor(empty))
			})}
		},
		calls: []leakCall{
			{tool: "get_transactions", args: `{"source":"schwab","account":"135","from":"2026-10-01","to":"2026-10-31"}`},
			{tool: "get_transaction", args: `{"id":"73508269999","account":"135"}`},
			{tool: "get_transaction", args: `{"id":"73508269999","account":"783"}`},
		},
	})
}

// txnImport imports a History export of June 2025 of the account numbered
// number, named name, of rows, each a line of the file, into the store at
// path.
func txnImport(t *testing.T, path, number, name string, rows ...string) {
	t.Helper()
	s, err := history.Open(t.Context(), history.Options{Path: path, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	csv := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n" + strings.Join(rows, "\n") + "\n"
	_, err = s.Import(t.Context(), []history.FileInput{{Name: name, Content: []byte(csv)}},
		history.ImportOptions{Account: number, From: "2025-06-01", To: "2025-06-30", Completeness: "complete"})
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// transactionLeakCalls are the leak world's calls of the transaction tools.
var transactionLeakCalls = []leakCall{
	{tool: "get_transactions", args: leakTransactionsCalls[0]},
	{tool: "get_transactions", args: leakTransactionsCalls[1]},
	{tool: "get_transactions", args: leakTransactionsCalls[2]},
	{tool: "get_transactions", args: leakTransactionsCalls[3]},
	{tool: "get_transaction", args: `{"id":"73508261946"}`},
	{tool: "get_transaction", args: `{"id":"73508261950"}`},
	{tool: "get_transaction", args: `{"id":"73508263001","account":"29470135","include_raw":true}`},
	{tool: "get_transaction", args: `{"id":"29470135","account":"135"}`},
	{tool: "get_transactions", args: `{"account":29470135}`},
	{tool: "get_transactions", args: `{"cursor":"29470135"}`},
}

// checkCursorDigests fails when a cursor that the server sent holds the
// digest of one of the leak calls' canonical arguments with a planted
// number in place of the account's key: a digest over a number would give
// the number away to anyone holding the transcript (design 8.5).
func checkCursorDigests(t *testing.T, e *env) {
	var digests []string
	for _, line := range e.sent() {
		for _, part := range strings.Split(line, `"next_cursor":"`)[1:] {
			cur, _, _ := strings.Cut(part, `"`)
			b, err := base64.RawURLEncoding.DecodeString(cur)
			var body cursorBody
			if err != nil || json.Unmarshal(b, &body) != nil {
				t.Errorf("a cursor that does not decode: %.80s", cur)
				continue
			}
			digests = append(digests, body.F)
		}
	}
	if len(digests) == 0 {
		t.Fatal("the leak calls sent no cursor")
	}
	c := &call{now: testNow, mask: testMasker("")}
	for _, args := range leakTransactionsCalls {
		var in getTransactionsIn
		if err := json.Unmarshal([]byte(args), &in); err != nil {
			t.Fatal(err)
		}
		q, err := c.txnQueryOf(in)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range e.planted {
			for _, account := range []string{n, mask.Number(n), in.Account} {
				f, err := argsDigest(q.args(account))
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(digests, f) && account != in.Account {
					t.Errorf("a cursor's digest is of %s with %s for the account", args, account)
				}
			}
		}
	}
}

// get_transactions' answers within the cap (design 8.3, 15.7): a coverage
// table of 60 accounts, which passes the cap on its own, is too_big, which
// asks for one account; and a row of 150 legs, with detail full, shows its
// first legs and says so in cut and list_cut, its figures and the totals
// whole, as get_transaction does after it drops Schwab's JSON. The 60
// accounts are read from the store, which knows three of them, so that no
// request reads their transactions; one that it does not know is read from
// nowhere.
var transactionSizeCases = []sizeCase{
	{name: "get_transactions over 60 accounts", opts: []envOption{withFake(func(f *schwabtest.Server) {
		prefs := []string{}
		for i := range 57 {
			n := sixtyAccount(i)
			f.AddAccount(schwabtest.Account{Number: n, Details: `{"securitiesAccount":{"type":"CASH","accountNumber":"` + n + `"}}`})
			prefs = append(prefs, `{"accountNumber":"`+n+`","nickName":"Brokerage account `+strconv.Itoa(i)+
				` of the family trust, held jointly by the two of us since we opened it"}`)
		}
		f.SetUserPreference(`{"accounts":[` + strings.Join(prefs, ",") + `]}`)
	})}, run: func(t *testing.T, e *env) {
		e.plant(sixtyAccount(0), sixtyAccount(56))
		r := e.call("get_transactions", `{"source":"store","from":"2026-11-13","to":"2026-11-13"}`)
		if r.code() != "too_big" || !strings.Contains(r.text, "one account") {
			t.Errorf("60 accounts' coverage: %.300s", r.text)
		}
		r = e.call("get_transactions", `{"source":"store","account":"`+sixtyAccount(7)+`","from":"2026-11-13","to":"2026-11-13"}`)
		if c := txnCoverageOf(r, "...007"); r.isError || r.value["source"] != "store" || c["read_from"] != "none" {
			t.Errorf("one account of them, which the store does not know: %.300s", r.text)
		}
		for _, req := range e.fake.Requests() {
			if req.Route == schwabtest.TransactionsRoute {
				t.Fatalf("source store asked Schwab for transactions: %s", req.URL)
			}
		}
	}},
	{
		name: "get_transactions and get_transaction with a row of 150 legs", opts: []envOption{withTxnRows(individual, manyLegs)},
		run: func(t *testing.T, e *env) {
			r := e.call("get_transactions", `{"account":"135","from":"2026-10-01","to":"2026-10-31","detail":"full"}`)
			if r.isError || !slices.Contains(r.warnings(), "list_cut") {
				t.Fatalf("a row of 150 legs: %.300s", r.text)
			}
			var got struct {
				Cut          []cutList `json:"cut"`
				Transactions []struct {
					Ref    string `json:"ref"`
					Amount any    `json:"amount"`
					Detail struct {
						Legs []json.RawMessage `json:"legs"`
					} `json:"detail"`
				} `json:"transactions"`
				Totals []struct {
					Count int `json:"count"`
				} `json:"totals"`
			}
			if err := json.Unmarshal(r.raw, &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Cut) != 1 || got.Cut[0].Path != "transactions.0.detail.legs" || got.Cut[0].Total != 150 ||
				got.Cut[0].Shown != len(got.Transactions[0].Detail.Legs) || got.Cut[0].Shown == 0 {
				t.Errorf("cut %+v of %d legs shown", got.Cut, len(got.Transactions[0].Detail.Legs))
			}
			if got.Transactions[0].Amount != -401.49 || len(got.Totals) != 1 || got.Totals[0].Count != 1 {
				t.Errorf("the row's figures or the totals were cut: %v %+v", got.Transactions[0].Amount, got.Totals)
			}
			one := e.call("get_transaction", `{"id":"73508264001","account":"135","include_raw":true}`)
			if w := one.warnings(); one.isError || len(w) < 2 || w[0] != "raw_omitted" || w[1] != "list_cut" || one.value["raw"] != nil {
				t.Errorf("get_transaction of 150 legs: %.300s", one.text)
			}
		},
	},
}

// sixtyAccount returns the number of the size case's ith account beyond
// the fixtures': made up, 8 digits.
func sixtyAccount(i int) string { return fmt.Sprintf("5%07d", 1000+i) }

// manyLegs is a trade of ...135 with 150 legs: one of XYZ and 149 fees.
var manyLegs = func() string {
	legs := []string{`{"instrument":{"assetType":"EQUITY","status":"ACTIVE","symbol":"XYZ","instrumentId":7003,"closingPrice":10.0},` +
		`"amount":40.0,"cost":-400.0,"price":10.0,"positionEffect":"OPENING"}`}
	for range 149 {
		legs = append(legs, `{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency",`+
			`"instrumentId":1,"closingPrice":0.0},"amount":0.01,"cost":-0.01,"feeType":"COMMISSION"}`)
	}
	return `{"activityId":73508264001,"time":"2026-10-09T14:00:00+0000","accountNumber":"29470135","type":"TRADE","status":"VALID",` +
		`"subAccount":"CASH","tradeDate":"2026-10-09T14:00:00+0000","netAmount":-401.49,"orderId":1004517688020,` +
		`"transferItems":[` + strings.Join(legs, ",") + `]}`
}()

// An account's days never come from both sources (design 2.3, 15.2): with
// ...398's whole history in the store ending a day before to, the whole
// account is read from Schwab, so that its History row and its API row of
// one trade never both appear.
func TestTransactionsNeverSplitAnAccount(t *testing.T) {
	t.Parallel()
	newYear := txnJSON(options, 73508262003, ny(2026, time.January, 1, 12, 0), "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment",
		0.05)
	e := newEnv(t, withTxnRows(options, append(slices.Clone(sharedRows398), newYear)...))
	r := e.call("get_transactions", `{"account":"398","from":"2025-11-01","to":"2026-01-01","include_uncounted":true}`)
	if r.isError {
		t.Fatal(r.text)
	}
	if c := txnCoverageOf(r, "...398"); c["read_from"] != "schwab" {
		t.Errorf("...398 was read from %v", c["read_from"])
	}
	refs := txnRefsOf(txnRowsOf(r))
	if !slices.Equal(refs, []string{"T73508262003@...398", "T73508262002@...398"}) {
		t.Errorf("rows %v: the trade of 2025-11-20 once, and the interest of 2026-01-01, from Schwab", refs)
	}
	// The same days but the last are whole in the store, which reads them.
	r = e.call("get_transactions", `{"account":"398","from":"2025-11-01","to":"2025-12-31","include_uncounted":true}`)
	if c := txnCoverageOf(r, "...398"); c["read_from"] != "store" ||
		!slices.Equal(txnRefsOf(txnRowsOf(r)), []string{"T73508262002@...398", "C2cdf9679.6@...398"}) {
		t.Errorf("the store's days: %v from %v", txnRefsOf(txnRowsOf(r)), c["read_from"])
	}
	// One of its rows that wait for the user's decision is of these days.
	r = e.call("get_transactions", `{"account":"398","from":"2025-07-01","to":"2025-12-31"}`)
	if !strings.Contains(r.text, "1 row of ...398 in these days waits for the user's decision and is not counted") {
		t.Errorf("one row that waits: %v", r.warnings())
	}
}

// Pages cover every row once by limit too (design 8.5): in pages of 4
// rows, far fewer than the page budget holds, ...398's rows of 2025 in the
// store, more than a page's worth, merge with Schwab's rows of ...135,
// older than any of them, as one page of every row holds them. Each page
// reads limit rows of the store after its cursor: with fewer, a page that
// ends at an older row of Schwab's would pass store rows that no page
// holds.
func TestPagesByLimitCoverEveryRowOnce(t *testing.T) {
	t.Parallel()
	var rows []string
	for i := range 6 {
		rows = append(rows, txnJSON(individual, 73508265501+int64(i), ny(2025, time.January, 2+i, 10, 0),
			"DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.01))
	}
	e := newEnv(t, withTxnRows(individual, rows...))
	const args = `{"from":"2025-01-01","to":"2025-12-31","include_uncounted":true,"limit":%d}`
	want := txnRefsOf(txnRowsOf(e.call("get_transactions", fmt.Sprintf(args, 200))))
	pages := e.txnPages("get_transactions", fmt.Sprintf(args, 4), 50)
	var got []string
	for _, p := range pages {
		if n := len(txnRowsOf(p)); n > 4 {
			t.Errorf("a page of %d rows, over the limit", n)
		}
		got = append(got, txnRefsOf(txnRowsOf(p))...)
	}
	if len(want) < 15 || !slices.Equal(got, want) {
		t.Errorf("pages of 4 hold\n%v\none page holds\n%v", got, want)
	}
}

// Pages cover every row once, in order (design 8.5, 15.7): 56 rows of
// ...135 read from Schwab, 36 of them in one window, and 8 of ...398 read
// from the shared store, which holds its whole year, with the rows it does
// not count; the newest row of each account shares its day, time and ref
// with the other's, and the first page, of one row, splits the two. Schwab's
// cap is 30 here, in the fake and in the window cache of a tool that is
// get_transactions but for that cap, so that the window of 36 rows is
// halved as one of 3,000 would be, and the page budget is small, so that
// the walk takes several pages: 3,000 rows walked through pages that the
// client checks took 20 seconds under -race. Every page stays within the
// budget.
func TestPagesCoverEveryRowOnce(t *testing.T) {
	t.Parallel()
	var rows []string
	add := func(id int64, at time.Time) {
		rows = append(rows, txnJSON(individual, id, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.01))
	}
	// The row that ties with ...398's trade of 2025-11-20 in the store.
	add(73508262002, time.Date(2025, time.November, 20, 15, 31, 0, 0, time.UTC))
	for i := range 35 {
		add(70000000+int64(i), ny(2025, time.July, 2, 0, 30).Add(7*time.Second+time.Duration(i)*340_000*time.Second))
	}
	for i := range 20 {
		add(70100000+int64(i), ny(2025, time.January, 2, 12, 0).Add(7*time.Second+time.Duration(i)*770_000*time.Second))
	}
	e := newEnv(t, withTxnRows(individual, rows...), withFake(func(f *schwabtest.Server) {
		f.SetTransactionRules(schwabtest.TransactionRules{Cap: 30})
	}), withOptions(func(o *Options) { o.PageBytes, o.MaxBytes = 6000, 7200 }), withTools(func(s *Server) {
		windows := newWindowCache()
		windows.full = 30
		testTool(s, "get_transactions_30", getTransactionsDef.args, func(ctx context.Context, c *call, in getTransactionsIn) (getTransactionsOut, error) {
			return getTransactions(ctx, c, windows, in)
		})
	}))

	const tool = "get_transactions_30"
	args := `{"from":"2025-01-01","to":"2025-12-31","include_uncounted":true}`
	first := e.call(tool, `{"from":"2025-01-01","to":"2025-12-31","include_uncounted":true,"limit":1}`)
	cur, _ := first.value["next_cursor"].(string)
	if cur == "" || len(txnRowsOf(first)) != 1 {
		t.Fatalf("the first page: %.500s", first.text)
	}
	if r := e.call(tool, txnWithCursor(t, `{"from":"2025-01-02","to":"2025-12-31","include_uncounted":true}`, cur)); r.code() != "bad_cursor" {
		t.Errorf("a cursor of other arguments: %.200s", r.text)
	}
	pages := append([]result{first}, e.txnPages(tool, txnWithCursor(t, args, cur), 50)...)
	type key = history.LedgerKey
	var got []key
	for i, p := range pages {
		if n := len(p.text); n > e.srv.o.PageBytes && len(txnRowsOf(p)) > 1 {
			t.Errorf("page %d holds %d rows in %d bytes, over the budget", i+1, len(txnRowsOf(p)), n)
		}
		for _, row := range txnRowsOf(p) {
			k := key{Day: nytime.Date(row["date"].(string)), Ref: row["ref"].(string), Account: row["account"].(string)}
			if at, ok := row["time"].(string); ok {
				tm, err := time.Parse(time.RFC3339, at)
				if err != nil {
					t.Fatal(err)
				}
				k.Time = storeInstant(tm)
			}
			if hash, ok := row["account_hash"].(string); ok {
				k.Account = hash
			}
			if n := len(got); n > 0 && keyCompare(k, got[n-1]) >= 0 {
				t.Errorf("page %d: %v does not come after %v", i+1, k, got[n-1])
			}
			got = append(got, k)
		}
		if p.value["complete"] != true {
			t.Errorf("page %d is not complete", i+1)
		}
	}
	if len(pages) < 4 {
		t.Errorf("%d pages: the budget cut too few", len(pages))
	}
	// Every row: ...135's 56, the fixtures' one of it in 2025, and ...398's 8
	// in the store, the History rows among them, each once.
	refs := map[string]int{}
	for _, k := range got {
		refs[k.Ref+"@"+k.Account[:4]]++
	}
	for _, ref := range []string{
		"T73508262002@742C", "T73508262002@1C56", "T73508262001@1C56", "T73508261943@742C",
		"C2cdf9679.1@1C56", "C2cdf9679.4@1C56", "C2cdf9679.6@1C56", "T70000034@742C", "T70100019@742C",
	} {
		if refs[ref] != 1 {
			t.Errorf("%s is on %d pages", ref, refs[ref])
		}
	}
	if len(got) != 65 || len(refs) != 65 {
		t.Errorf("the pages hold %d rows, %d of them apart, want 65", len(got), len(refs))
	}
	if len(got) > 1 && (got[0].Ref != "T73508262002" || got[1].Ref != "T73508262002" || got[0].Account == got[1].Account) {
		t.Errorf("the rows that share day, time and ref: %v, %v", got[0], got[1])
	}
	var requests int
	for _, req := range e.fake.Requests() {
		if req.Route == schwabtest.TransactionsRoute && strings.Contains(req.URL, schwabtest.HashFor(individual)) {
			requests++
		}
	}
	if requests != 4 {
		t.Errorf("...135's windows took %d requests, want 4: the newer window, its halves, and the older", requests)
	}
}

// Two of Schwab's rows of one account at one instant are ordered by their
// refs (design 10.5's key), so that a page that ends between them loses
// neither.
func TestRowsOfOneInstant(t *testing.T) {
	t.Parallel()
	at := ny(2025, time.March, 3, 12, 0)
	e := newEnv(t, withoutStore(), withTxnRows(individual,
		txnJSON(individual, 70200000, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.01),
		txnJSON(individual, 70200001, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.02)))
	var got []string
	for _, p := range e.txnPages("get_transactions", `{"source":"schwab","account":"135","from":"2025-03-03","to":"2025-03-03","limit":1}`, 5) {
		got = append(got, txnRefsOf(txnRowsOf(p))...)
	}
	if !slices.Equal(got, []string{"T70200001@...135", "T70200000@...135"}) {
		t.Errorf("pages of one row hold %v", got)
	}
}

// A walk that the budget cuts short (design 5.5, 10.5, 15.7): with each
// request for a window taking 150 ms and a budget of a second, the first
// call reads the windows its budget allows and answers only the rows newer
// than every window left unread, complete false, totals null, the warning
// partial and a cursor; the calls that follow it read the windows left, and
// the rest from the cache; no row comes twice, none is missing, and no
// window is read twice.
func TestPartialPageAtDeadline(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore(), withOptions(func(o *Options) { o.CallTimeout = time.Second }), withFake(func(f *schwabtest.Server) {
		f.OnAPI(func(r *http.Request) {
			if r.Pattern == string(schwabtest.TransactionsRoute) {
				time.Sleep(150 * time.Millisecond)
			}
		})
	}))
	args := `{"source":"schwab","from":"2024-03-01","to":"2025-12-31"}`
	first := e.call("get_transactions", args)
	if first.isError || first.value["complete"] != false || first.value["totals"] != nil || !slices.Contains(first.warnings(), "partial") {
		t.Fatalf("a walk cut short: %.600s", first.text)
	}
	var got []string
	r := first
	for range 10 {
		got = append(got, txnRefsOf(txnRowsOf(r))...)
		cur, _ := r.value["next_cursor"].(string)
		if cur == "" {
			break
		}
		r = e.call("get_transactions", txnWithCursor(t, args, cur))
		if r.isError {
			t.Fatal(r.text)
		}
	}
	want := []string{
		"T73508261943@...135", "T73508261944@...398", "T73508261945@...398", "T73508261949@...135",
		"T73508261946@...135", "T73508261954@...135", "T73508261950@...135", "T73508261947@...135",
	}
	if !slices.Equal(got, want) || r.value["complete"] != true || r.value["totals"] == nil {
		t.Errorf("the walk holds %v, want %v, ending complete with totals: %.300s", got, want, r.text)
	}
	windows := 0
	for _, req := range e.fake.Requests() {
		if req.Route == schwabtest.TransactionsRoute {
			windows++
		}
	}
	if windows != 6 {
		t.Errorf("the walk asked Schwab for %d windows, want 6: two of each account, each once", windows)
	}
}

// get_transactions checks its arguments before it reads anything and
// never quotes them: a date that is none, from after to, a type that is
// not Schwab's, and a range over 724 days of an account read from Schwab;
// a to after today is today, from is 90 days before to when left out, and
// types are taken in any case. The store reads any range.
func TestTransactionArguments(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for _, tc := range []struct{ args, code, says string }{
		{`{"from":"2025-02-30"}`, "bad_argument", "from takes a New York date"},
		{`{"to":"2025-13-01"}`, "bad_argument", "to takes a New York date"},
		{`{"from":"2025-06-02","to":"2025-06-01"}`, "bad_argument", "from comes after to"},
		{`{"from":"2026-11-14"}`, "bad_argument", "from comes after to, or after today"},
		{`{"types":["TRADE","QUIZ_SHOW"]}`, "bad_argument", "types takes Schwab's transaction types: TRADE, RECEIVE_AND_DELIVER,"},
		{`{"source":"schwab","from":"2023-12-31","to":"2025-12-24"}`, "bad_argument", "at most 724 days of an account from Schwab"},
		{`{"account":"999"}`, "account_not_found", `No account matches "999"`},
		{`{"limit":201}`, "bad_argument", "limit takes a whole number from 1 to 200"},
	} {
		r := e.call("get_transactions", tc.args)
		if r.code() != tc.code || !strings.Contains(r.text, tc.says) {
			t.Errorf("%s: %s, want [%s] saying %q", tc.args, r.text, tc.code, tc.says)
		}
		for _, value := range []string{"2025-02-30", "2025-13-01", "QUIZ_SHOW", "2023-12-31"} {
			if strings.Contains(tc.args, value) && strings.Contains(r.text, value) {
				t.Errorf("%s: the error quotes %s: %s", tc.args, value, r.text)
			}
		}
	}
	if n := e.fake.APICalls() - 2; n > 2 { // the login's own, and the directory's two
		t.Errorf("the refusals sent %d requests for transactions", n)
	}
	r := e.call("get_transactions", `{"to":"2027-01-01","types":["trade"],"source":"schwab","account":"135"}`)
	asked, _ := json.Marshal(txnCoverageOf(r, "...135")["asked"])
	if r.isError || string(asked) != `{"from":"2026-08-15","to":"2026-11-13"}` || slices.Contains(r.warnings(), "types_filter") {
		t.Errorf("the default days, and types of Schwab's rows alone: %s; %.300s", asked, r.text)
	}
	if r := e.call("get_transactions", `{"source":"schwab","account":"135","from":"2024-01-01","to":"2025-12-24"}`); r.isError {
		t.Errorf("724 days from Schwab: %.300s", r.text)
	}
	if r := e.call("get_transactions", `{"source":"store","account":"135","from":"2000-01-01","to":"2025-12-31"}`); r.isError ||
		len(txnRowsOf(r)) != 8 {
		t.Errorf("26 years from the store: %.300s", r.text)
	}
}

// A cursor belongs to its tool and its arguments, the defaults applied, the
// lists in any order, each type once and in any case, the symbol in any
// case, and the account as its key: a changed limit is allowed, other
// arguments are bad_cursor, and so is one whose position names no row; a
// cursor of no position starts from the beginning; and a cursor survives a
// restart of the server.
func TestTransactionCursor(t *testing.T) {
	t.Parallel()
	args := `{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"types":["trade","dividend_or_interest","journal"],"symbol":"bnd","limit":1}`
	e := newEnv(t)
	first := e.call("get_transactions", args)
	cur, _ := first.value["next_cursor"].(string)
	if cur == "" {
		t.Fatalf("no cursor: %.300s", first.text)
	}
	page := func(e *env, args string) []string { return txnRefsOf(txnRowsOf(e.call("get_transactions", args))) }
	want := page(e, txnWithCursor(t, `{"account":"...135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["buy","deposit","dividend","interest","sell","tax"],"types":["JOURNAL","DIVIDEND_OR_INTEREST","TRADE"],"symbol":"BND","limit":5}`, cur))
	if len(want) != 2 {
		t.Fatalf("the second page: %v", want)
	}
	again := newEnv(t)
	if got := page(again, txnWithCursor(t, `{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["dividend","buy","sell","interest","tax","deposit"],"types":["Dividend_Or_Interest","journal","trade","TRADE"],"symbol":"Bnd","limit":5}`, cur)); !slices.Equal(got, want) {
		t.Errorf("after a restart: %v, want %v", got, want)
	}
	for _, other := range []string{
		`{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","symbol":"bnd"}`,
		`{"account":"135","source":"auto","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"symbol":"bnd"}`,
		`{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"symbol":"bnd","detail":"full"}`,
		`{"account":"398","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"symbol":"bnd"}`,
		`{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"types":["trade","dividend_or_interest"],"symbol":"bnd"}`,
		`{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-29","categories":["sell","buy","dividend","interest","tax","deposit"],"types":["trade","dividend_or_interest","journal"],"symbol":"bnd"}`,
		`{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"types":["trade","dividend_or_interest","journal"],"symbol":"bndx"}`,
		`{"account":"135","source":"store","from":"2023-06-12","to":"2024-12-30","categories":["sell","buy","dividend","interest","tax","deposit"],"types":["trade","dividend_or_interest","journal"],"symbol":"bnd","include_uncounted":true}`,
	} {
		if r := e.call("get_transactions", txnWithCursor(t, other, cur)); r.code() != "bad_cursor" {
			t.Errorf("%s with another call's cursor: %.200s", other, r.text)
		}
	}
	c := &call{tool: "get_transactions", now: testNow, mask: testMasker("")}
	var in getTransactionsIn
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		t.Fatal(err)
	}
	q, err := c.txnQueryOf(in)
	if err != nil {
		t.Fatal(err)
	}
	canonical := q.args(schwabtest.HashFor(individual))
	for name, k := range map[string]*[4]string{
		"no ref":     {"2024-04-04", "", "", schwabtest.HashFor(individual)},
		"no account": {"2024-04-04", "", "T73508261946", ""},
		"no date":    {"2024-04-44", "", "T73508261946", schwabtest.HashFor(individual)},
	} {
		bad, err := c.newCursor(canonical, k)
		if err != nil {
			t.Fatal(err)
		}
		if r := e.call("get_transactions", txnWithCursor(t, args, string(*bad))); r.code() != "bad_cursor" {
			t.Errorf("a cursor of %s: %.200s", name, r.text)
		}
	}
	start, err := c.newCursor(canonical, (*[4]string)(nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := page(e, txnWithCursor(t, args, string(*start))); !slices.Equal(got, txnRefsOf(txnRowsOf(first))) {
		t.Errorf("a cursor of no position: %v, want the first page, %v", got, txnRefsOf(txnRowsOf(first)))
	}
}

// get_transaction reads its id's shape: Schwab's activity ID with or
// without its T, a T~ ref or a C ref; anything else is bad_argument. An
// error echoes the id whole only in the shape of what it names, a ref or
// an activity ID of 10 digits or more, else as a selector's digits, so
// that an account number given as an id shows its last three digits only.
// The account is the one given, else the one whose ref the local history
// holds, else the only one shared; two that hold the ref need it.
func TestGetTransactionIDs(t *testing.T) {
	t.Parallel()
	c := &call{tool: "get_transaction", now: testNow, mask: testMasker("")}
	for _, tc := range []struct{ id, ref, shown string }{
		{"73508261946", "T73508261946", "73508261946"},
		{"t73508261946", "T73508261946", "t73508261946"},
		{"29470135", "T29470135", "...135"},
		{"T29470135", "T29470135", "T...135"},
		{"7350826194", "T7350826194", "7350826194"},
		{"735082619", "T735082619", "...619"},
		{"T~20260302T143027.1f2e3d4c5b6a", "T~20260302T143027.1f2e3d4c5b6a", "T~20260302T143027.1f2e3d4c5b6a"},
		{"T~none.1f2e3d4c5b6a", "T~none.1f2e3d4c5b6a", "T~none.1f2e3d4c5b6a"},
		{"T~20260302T143027.1f2e3d4c5b6a.2", "T~20260302T143027.1f2e3d4c5b6a.2", "T~20260302T143027.1f2e3d4c5b6a.2"},
		{"C2cdf9679.2", "C2cdf9679.2", "C2cdf9679.2"},
		// Ids of no ref's exact shape, or that hold a known number, show
		// as a selector's digits do; zeros before a number do not count.
		{"T~29470135", "T~29470135", "T~...135"},
		{"T~5550001234", "T~5550001234", "T~...234"},
		{"C1.999888777", "C1.999888777", "C...777"},
		{"C29470135.1", "C29470135.1", "C...351"},
		{"0029470135", "T29470135", "...135"},
		{"T0029470135", "T29470135", "T...135"},
		{"5550001234", "T5550001234", "5550001234"},
	} {
		id, err := c.txnIDOf(tc.id)
		if err != nil || id.ref != tc.ref || string(id.shown(c)) != tc.shown {
			t.Errorf("%s: %+v shown %q, %v; want %s shown %q", tc.id, id, id.shown(c), err, tc.ref, tc.shown)
		}
	}
	for _, bad := range []string{"0", "T", "C2cdf9679", "Cxyz.1", "73508261946x", "account 29470135", "12345678901234567890"} {
		if _, err := c.txnIDOf(bad); err == nil {
			t.Errorf("the id %q was taken", bad)
		}
	}
	e := newEnv(t)
	for _, tc := range []struct{ args, code, says string }{
		{`{"id":"29470135","account":"135"}`, "not_found", "Schwab has no transaction ...135 in account ...135."},
		{`{"id":"73508261946","account":"398"}`, "not_found", "Schwab has no transaction 73508261946 in account ...398."},
		{`{"id":"C2cdf9679.2","account":"135"}`, "not_found", "The local history has no transaction C2cdf9679.2 in account ...135."},
		{`{"id":"T~20260302T143027.1f2e3d4c5b6a"}`, "bad_argument", "account is needed: the local history does not hold this id"},
		{
			`{"id":"T~20260302T143027.1f2e3d4c5b6a","account":"135"}`, "not_found",
			"Schwab sent no transaction T~20260302T143027.1f2e3d4c5b6a in account ...135 on the days around its time.",
		},
		{`{"id":"T~none.1f2e3d4c5b6a"}`, "not_found", "The local history has no transaction T~none.1f2e3d4c5b6a."},
		{`{"id":"T~29470135"}`, "not_found", "The local history has no transaction T~...135."},
		{`{"id":"C1.999888777"}`, "not_found", "The local history has no transaction C...777."},
		{`{"id":"0029470135","account":"135"}`, "not_found", "Schwab has no transaction ...135 in account ...135."},
		{`{"id":"99999999999"}`, "bad_argument", "account is needed: the local history does not hold this id, and the login shares 3 accounts"},
		{`{"id":"one"}`, "bad_argument", "id takes a ref"},
	} {
		if r := e.call("get_transaction", tc.args); r.code() != tc.code || !strings.Contains(r.text, tc.says) {
			t.Errorf("%s: %s, want [%s] saying %q", tc.args, r.text, tc.code, tc.says)
		}
	}
	v := &dirView{accounts: []dirAccount{
		{number: individual, hash: "H1", shared: true, storeID: 1},
		{number: options, hash: "H2", shared: true, storeID: 2},
	}}
	found := []history.EntryDetail{{Entry: history.LedgerRow{AccountID: 1}}, {Entry: history.LedgerRow{AccountID: 2}}}
	if _, err := pickTxnAccount(c, v, "", found); err == nil || !strings.Contains(err.Error(), "holds this id in ...135 and ...398; pass account") {
		t.Errorf("a ref of two accounts: %v", err)
	}
	if a, err := pickTxnAccount(c, v, "", found[1:]); err != nil || a.number != options {
		t.Errorf("a ref of one account: %v, %v", a.number, err)
	}
	if a, err := pickTxnAccount(c, v, "398", found); err != nil || a.number != options {
		t.Errorf("the account given: %v, %v", a.number, err)
	}
	if a, err := pickTxnAccount(c, &dirView{accounts: v.accounts[:1]}, "", nil); err != nil || a.number != individual {
		t.Errorf("the only account: %v, %v", a.number, err)
	}
}

// A window that Schwab refuses as too old, after it served a newer one,
// ends that account's walk, which the answer says (empty_not_proof) and
// coverage marks as reaching back only so far; refused as the first, it is
// the account's failure, which the others' answer says (account_failed),
// or the call's, when every account failed. An older window that fails
// otherwise, or that Schwab refuses for the account, is the account's
// failure too. An older window that Schwab sends nothing for, though it
// sent rows of newer days, is no proof either.
func TestOlderWindows(t *testing.T) {
	t.Parallel()
	args := `{"source":"schwab","from":"2024-03-01","to":"2025-12-31"}`
	e := newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		f.SetTransactionRules(schwabtest.TransactionRules{RefuseBefore: ny(2025, time.January, 1, 0, 0)})
	}))
	r := e.call("get_transactions", args)
	c := txnCoverageOf(r, "...135")
	whole, _ := json.Marshal(c["whole"])
	if r.isError || c["reached_back"] != "2025-01-30" || string(whole) != `[{"from":"2025-01-30","to":"2025-12-31"}]` || c["error"] != nil {
		t.Errorf("an older window refused: %v", c)
	}
	if !slices.Contains(r.warnings(), "empty_not_proof") || !strings.Contains(r.text, "refused the transactions of ...135 from 2024-03-01 to 2025-01-29 as too old") {
		t.Errorf("no word of the refusal: %v", r.warnings())
	}
	if len(txnRowsOf(r)) != 1 || r.value["totals"] == nil {
		t.Errorf("the rows of the newer window: %v", txnRefsOf(txnRowsOf(r)))
	}

	e = newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		f.SetTransactionRules(schwabtest.TransactionRules{RefuseBefore: ny(2026, time.January, 1, 0, 0)})
	}))
	if r := e.call("get_transactions", args); r.code() != "schwab_refused" || !strings.Contains(r.text, "a guess: never seen") {
		t.Errorf("every account's first window refused: %.300s", r.text)
	}
	e = newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		f.Queue(schwabtest.TransactionsRoute, schwabtest.Answer(400, `{"message":"no transactions for you"}`))
	}))
	r = e.call("get_transactions", args)
	if c := txnCoverageOf(r, "...135"); r.isError || c["error"] != "schwab_refused" || c["read_from"] != "none" ||
		!slices.Contains(r.warnings(), "account_failed") || !strings.Contains(r.text, "...135 could not be read: [schwab_refused]") {
		t.Errorf("one account's first window refused: %.600s", r.text)
	}
	if c := txnCoverageOf(r, "...398"); c["read_from"] != "schwab" || c["error"] != nil {
		t.Errorf("...398 beside it: %v", c)
	}

	// An older window that fails otherwise is the account's failure: the
	// fourth request is ...135's older window, after each account's newer.
	// Schwab refusing the account is asked again once the directory is read
	// again.
	invalid := schwabtest.Answer(http.StatusBadRequest, `{"message":"Invalid account number"}`)
	for name, answers := range map[string][]http.HandlerFunc{
		"404":                    {schwabtest.Answer(http.StatusNotFound, `{"message":"no such thing"}`)},
		"Invalid account number": {invalid, invalid},
	} {
		e = newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
			f.Queue(schwabtest.TransactionsRoute, append([]http.HandlerFunc{nil, nil, nil}, answers...)...)
		}))
		r = e.call("get_transactions", args)
		if c := txnCoverageOf(r, "...135"); r.isError || c["read_from"] != "none" || c["error"] == nil ||
			!slices.Contains(r.warnings(), "account_failed") || slices.Contains(r.warnings(), "empty_not_proof") {
			t.Errorf("an older window answered %s: %.600s", name, r.text)
		}
	}

	// The empty older window's days are not whole: empty_not_proof says
	// they may be missing.
	e = newEnv(t, withoutStore())
	r = e.call("get_transactions", `{"source":"schwab","account":"135","from":"2024-07-01","to":"2025-06-30"}`)
	if !slices.Contains(r.warnings(), "empty_not_proof") || !strings.Contains(r.text, "Schwab sent no transactions of ...135 from 2024-07-01 to 2024-12-29") {
		t.Errorf("an empty older window: %v", r.warnings())
	}
	c = txnCoverageOf(r, "...135")
	whole, _ = json.Marshal(c["whole"])
	if c["reached_back"] != "2024-07-01" || string(whole) != `[{"from":"2024-12-30","to":"2025-06-30"}]` {
		t.Errorf("two windows read, the older empty: reached back to %v, whole %s", c["reached_back"], whole)
	}
	if r := e.call("get_transactions", `{"source":"schwab","account":"783","from":"2024-07-01","to":"2025-06-30"}`); slices.Contains(r.warnings(), "empty_not_proof") {
		t.Errorf("an account with no rows at all: %v", r.warnings())
	}
}

// A store that cannot be read never fails a call of auto: each account is
// read from Schwab, and the warning store_unavailable says why; an account
// that only the store knows is then read from nowhere, with the store's
// error in its coverage. Under source store it is the call's error, before
// any account's, a missing store and a config that names no store that may
// be used included. get_transaction reads a T ref from Schwab without the
// store's view, saying why when there is a store, and fails for a C ref. A
// store that a newer schwabtx derived is read, with a warning.
func TestTransactionsStoreStates(t *testing.T) {
	t.Parallel()
	newer := storeCopy(t)
	exec(t, rawStore(t, newer), "PRAGMA user_version = 2")
	e := newEnv(t, withStore(newer))
	r := e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)
	if r.isError || !slices.Contains(r.warnings(), "store_unavailable") || !strings.Contains(r.text, "[store_newer]") ||
		txnCoverageOf(r, "...398")["read_from"] != "schwab" {
		t.Errorf("auto with a newer store: %.600s", r.text)
	}
	if r := e.call("get_transactions", `{"source":"store"}`); r.code() != "store_newer" {
		t.Errorf("source store with a newer store: %.300s", r.text)
	}
	if r := e.call("get_transaction", `{"id":"C2cdf9679.2"}`); r.code() != "store_newer" {
		t.Errorf("a C ref with a newer store: %.300s", r.text)
	}
	if r := e.call("get_transaction", `{"id":"73508261946","account":"135"}`); r.isError ||
		!slices.Equal(r.warnings(), []string{"store_unavailable"}) || r.value["store"] != nil {
		t.Errorf("a T ref with a newer store: %.300s", r.text)
	}
	missing := newEnv(t, withoutStore())
	for _, args := range []string{`{"source":"store"}`, `{"source":"store","account":"999"}`} {
		if r := missing.call("get_transactions", args); r.code() != "no_store" {
			t.Errorf("%s without a store: %.300s", args, r.text)
		}
	}
	if r := missing.call("get_transaction", `{"id":"C2cdf9679.2"}`); r.code() != "no_store" {
		t.Errorf("a C ref without a store: %.300s", r.text)
	}
	none := newEnv(t, withConfig(func(c *config.Config) { c.StoreFile = config.Setting{} }))
	if r := none.call("get_transactions", `{"source":"store"}`); r.code() != "no_store" {
		t.Errorf("source store with no store that may be used: %.300s", r.text)
	}
	if r := none.call("get_transactions", `{"account":"135","from":"2025-01-01","to":"2025-01-31"}`); r.isError ||
		!strings.Contains(r.text, "[no_store]") {
		t.Errorf("auto with no store that may be used: %.300s", r.text)
	}
	for name, e := range map[string]*env{"no store": missing, "no store that may be used": none} {
		if r := e.call("get_transaction", `{"id":"73508261946","account":"135"}`); r.isError || len(r.warnings()) != 0 ||
			r.value["store"] != nil || r.value["source"] != "schwab" {
			t.Errorf("a T ref with %s: %.300s", name, r.text)
		}
	}

	rules := storeCopy(t)
	exec(t, rawStore(t, rules), "UPDATE store SET derived_with = 2")
	r = newEnv(t, withStore(rules)).call("get_transactions", `{"account":"398","from":"2025-01-01","to":"2025-12-31"}`)
	if r.isError || !slices.Contains(r.warnings(), "store_newer_rules") || txnCoverageOf(r, "...398")["read_from"] != "store" {
		t.Errorf("a store of newer rules: %.300s", r.text)
	}

	// A store whose accounts can be read but not its ledger, which knows an
	// account that Schwab does not share.
	broken := storeCopy(t)
	txnImport(t, broken, "12345678", "Individual_XXX678_Transactions_20250701-120000.csv",
		`"06/05/2025","Journal","","JOURNAL IN","","","","$10.00"`)
	exec(t, rawStore(t, broken), "DROP VIEW coverages")
	e = newEnv(t, withStore(broken))
	r = e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)
	if c := txnCoverageOf(r, "...678"); r.isError || c["read_from"] != "none" || c["error"] != "store_unusable" ||
		txnCoverageOf(r, "...398")["read_from"] != "schwab" || !slices.Equal(r.warnings(), []string{"store_unavailable", "before_decisions"}) {
		t.Errorf("auto with a ledger that cannot be read: %.600s", r.text)
	}
	if r := e.call("get_transactions", `{"source":"store"}`); r.code() != "store_unusable" {
		t.Errorf("source store with a ledger that cannot be read: %.300s", r.text)
	}
}

// With source store, an account that the local history does not know, as
// one opened since the last import, holds none of the answer's rows, and
// the warning store_coverage_partial says so, naming it; under auto it is
// read from Schwab, with no such warning. Here the store knows ...135
// alone.
func TestStoreDoesNotKnowAnAccount(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	s, err := history.Open(t.Context(), history.Options{Path: path, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	read := history.Read{SentAt: testNow.Add(-time.Hour), AnsweredAt: testNow.Add(-time.Hour + time.Second)}
	numbers := []schwab.AccountNumber{{AccountNumber: individual, HashValue: schwab.AccountHash(schwabtest.HashFor(individual))}}
	if err := s.RecordAccountNumbers(t.Context(), read, numbers); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withStore(path))
	r := e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31","source":"store"}`)
	for _, masked := range []string{"...398", "...783"} {
		says := "The local history does not know " + masked + ", so this answer holds none of its rows; " +
			"source auto or schwab reads them from Schwab."
		if c := txnCoverageOf(r, masked); r.isError || c["read_from"] != "none" || c["error"] != nil || !strings.Contains(r.text, says) {
			t.Errorf("source store, %s that the store does not know: %.600s", masked, r.text)
		}
	}
	if strings.Contains(r.text, "does not know ...135") {
		t.Errorf("source store, ...135, which the store knows: %.600s", r.text)
	}
	r = e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)
	if r.isError || txnCoverageOf(r, "...398")["read_from"] != "schwab" || strings.Contains(r.text, "does not know") {
		t.Errorf("auto: %.600s", r.text)
	}
}

// A read of the local history that still waits when the call's time runs
// out ends the call, [timed_out]: the accounts do not go to Schwab as if
// the store could not be read, with no time left to read them. The
// directory is read, and kept, while there is no store; then the store
// appears, kept locked by another connection, so that the call's opening
// of it waits.
func TestTransactionsStoreReadOutlastsTheCall(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "history.db")
	e := newEnv(t, withStore(path), withOptions(func(o *Options) {
		o.CallTimeout, o.StoreLockWait, o.StoreBusy = 300*time.Millisecond, time.Minute, 20*time.Millisecond
	}))
	c := &call{
		s: e.srv, tool: "get_transactions", mask: e.srv.mask, accounts: e.srv.accounts, store: e.srv.store,
		now: testNow, budget: time.Minute, deadline: time.Now().Add(time.Minute),
	}
	if _, err := c.directory(t.Context()); err != nil {
		t.Fatal(err)
	}
	copyFile(t, sharedStore(t), path)
	db := rawStore(t, path)
	exec(t, db, "PRAGMA journal_mode = WAL", "PRAGMA locking_mode = EXCLUSIVE", "BEGIN IMMEDIATE", "UPDATE store SET derived_with = derived_with")
	defer exec(t, db, "ROLLBACK")
	if r := e.call("get_transactions", `{"account":"398","from":"2025-01-01","to":"2025-12-31"}`); r.code() != "timed_out" {
		t.Errorf("a read of the store that outlasts the call: %.300s", r.text)
	}
}

// Text the transaction tools compose keeps its identifiers (design 7.8,
// 15.4): dates in a warning, an activity ID of 11 digits in an error, a
// correlation ID in Schwab's detail beside a number of Schwab's own words,
// which is masked, refs and a store's flag detail that holds a date, and
// hashes; while the masked numbers name the accounts.
func TestTransactionTextKeepsIdentifiers(t *testing.T) {
	t.Parallel()
	correl := "5b1d2c3e-1234-5678-9abc-def012345678"
	e := newEnv(t, withFake(func(f *schwabtest.Server) {
		f.Queue(schwabtest.TransactionsRoute, schwabtest.Answer(400, `{"message":"no account 29470135 on 2026-09-14"}`,
			"Schwab-Client-CorrelID", correl))
	}))
	var texts []string
	for _, c := range []struct{ tool, args string }{
		{"get_transactions", `{"source":"schwab","from":"2025-01-01","to":"2025-12-31"}`},
		{"get_transactions", `{"source":"schwab","account":"135","from":"2024-07-01","to":"2025-06-30"}`},
		{"get_transactions", `{"from":"2025-01-01","to":"2025-12-31","include_uncounted":true,"detail":"full"}`},
		{"get_transaction", `{"id":"73508261999","account":"135"}`},
		{"get_transaction", `{"id":"T73508262001"}`},
	} {
		texts = append(texts, e.call(c.tool, c.args).text)
	}
	all := strings.Join(texts, "\n")
	for _, id := range []string{
		correl, "from 2024-07-01 to 2024-12-29", "73508261999", "C2cdf9679.3", "T73508262002", "AAPL last seen 2026-01-05",
		schwabtest.HashFor(individual), "2025-03-27T10:40:03-04:00", "no account ...135 on ...914",
	} {
		if !strings.Contains(all, id) {
			t.Errorf("the answers lost %s", id)
		}
	}
	if strings.Contains(all, individual) {
		t.Error("an answer holds a full account number")
	}
}

// Schwab's rows are read as the store reads them (design 10.5, 12.3): an
// activity ID that an answer holds twice is one row, flagged
// duplicate_in_answer; an INVALID row is listed and counts in no total; a
// row whose amount, or a fee of which, the store cannot read or keep makes
// its category's total of amounts, or of fees, null, and the warning
// problems counts the rows that Schwab sent otherwise; a row at midnight in New York, which Schwab sends for a row of no
// time of day, has no time; and Schwab's descriptions are masked as free
// text, a number that no one knows included.
func TestSchwabRowsAsTheStoreReadsThem(t *testing.T) {
	t.Parallel()
	twice := txnJSON(individual, 73508265001, ny(2025, time.March, 3, 10, 0), "DIVIDEND_OR_INTEREST", "Ordinary Dividend~XYZ", 1.5)
	invalid := strings.Replace(txnJSON(individual, 73508265002, ny(2025, time.March, 4, 10, 0), "DIVIDEND_OR_INTEREST",
		"Ordinary Dividend~XYZ", 2.5), `"status":"VALID"`, `"status":"INVALID"`, 1)
	unreadable := strings.Replace(txnJSON(individual, 73508265003, ny(2025, time.March, 5, 10, 0), "DIVIDEND_OR_INTEREST",
		"Free Balance Interest Adjustment", 0.5), `"netAmount":0.5`, `"netAmount":"0.5"`, 1)
	atMidnight := txnJSON(individual, 73508265004, ny(2025, time.March, 6, 0, 0), "DIVIDEND_OR_INTEREST",
		"Free Balance Interest Adjustment", 0.25)
	feeUnread := `{"activityId":73508265005,"time":"2025-03-07T15:00:00+0000","description":"Bought for 12345678",` +
		`"accountNumber":"29470135","type":"TRADE","status":"VALID","subAccount":"CASH","tradeDate":"2025-03-07T05:00:00+0000",` +
		`"netAmount":-10.65,"orderId":1004517688030,"transferItems":[{"instrument":{"assetType":"EQUITY","status":"ACTIVE",` +
		`"symbol":"XYZ","description":"XYZ 12345678 CORP","instrumentId":7003,"closingPrice":10.0},"amount":1.0,"cost":-10.0,` +
		`"price":10.0,"positionEffect":"OPENING"},{"instrument":{"assetType":"CURRENCY","status":"ACTIVE","symbol":"CURRENCY_USD",` +
		`"description":"USD currency","instrumentId":1,"closingPrice":0.0},"amount":"-0.65","cost":-0.655,"feeType":"COMMISSION"}]}`
	e := newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		f.Queue(schwabtest.TransactionsRoute, schwabtest.Answer(200, "["+strings.Join([]string{twice, invalid, twice, unreadable, atMidnight, feeUnread}, ",")+"]"))
	}))
	r := e.call("get_transactions", `{"source":"schwab","account":"135","from":"2025-03-01","to":"2025-03-31","detail":"full"}`)
	rows := map[string]map[string]any{}
	for _, row := range txnRowsOf(r) {
		rows[row["ref"].(string)] = row
	}
	if len(txnRowsOf(r)) != 5 || !slices.Contains(identsOf(rows["T73508265001"]["flags"]), "duplicate_in_answer") {
		t.Errorf("a row sent twice: %v", txnRefsOf(txnRowsOf(r)))
	}
	if rows["T73508265002"]["status"] != "INVALID" || rows["T73508265004"]["time"] != nil || rows["T73508265003"]["amount"] != nil {
		t.Errorf("the rows: %v", rows)
	}
	totals, _ := json.Marshal(r.value["totals"])
	if string(totals) != `[{"amount":-10.65,"category":"buy","count":1,"fees":null},{"amount":1.5,"category":"dividend","count":1,"fees":0},`+
		`{"amount":null,"category":"interest","count":2,"fees":0}]` {
		t.Errorf("totals %s", totals)
	}
	if !slices.Contains(r.warnings(), "problems") || !strings.Contains(r.text, "2 transactions differed from what Schwab has sent before") {
		t.Errorf("no warning of the two rows' figures that could not be read: %v", r.warnings())
	}
	bought := rows["T73508265005"]
	legs, _ := bought["detail"].(map[string]any)["legs"].([]any)
	if bought["description"] != "Bought for ...678" || len(legs) != 2 || legs[0].(map[string]any)["description"] != "XYZ ...678 CORP" {
		t.Errorf("Schwab's descriptions, masked as free text: %v", bought)
	}
}

// One activity ID can sit in two of the person's accounts, the two sides of
// a move between them (design 10.5): with ...783's side of ...398's trade
// of 2025-11-20 in the store, detail full shows ...398's row with its own
// legs, a fee and XYZ, not ...783's cash leg; and get_transaction needs the
// account, then shows that account's row, the store's, as Schwab sends no
// such row of ...783.
func TestOneRefInTwoAccounts(t *testing.T) {
	t.Parallel()
	path := storeCopy(t)
	s, err := history.Open(t.Context(), history.Options{Path: path, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	var side schwab.Transaction
	err = json.Unmarshal([]byte(txnJSON(empty, 73508262002, time.Date(2025, time.November, 20, 15, 31, 0, 0, time.UTC), "JOURNAL",
		"INTERNAL TRANSFER", 50.65)), &side)
	if err == nil {
		at := ny(2025, time.December, 1, 9, 0)
		_, err = s.RecordTransactions(t.Context(), history.TransactionsRead{
			Read: history.Read{SentAt: at, AnsweredAt: at.Add(time.Second)}, Account: empty,
			Query: schwab.TransactionQuery{Start: ny(2025, time.November, 1, 0, 0), End: ny(2025, time.December, 1, 0, 0)},
			Rows:  []schwab.Transaction{side},
		})
	}
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withStore(path))
	r := e.call("get_transactions", `{"account":"398","from":"2025-11-20","to":"2025-11-20","detail":"full"}`)
	rows := txnRowsOf(r)
	if len(rows) != 1 || rows[0]["ref"] != "T73508262002" || txnCoverageOf(r, "...398")["read_from"] != "store" {
		t.Fatalf("...398's trade: %.600s", r.text)
	}
	var symbols []string
	for _, l := range rows[0]["detail"].(map[string]any)["legs"].([]any) {
		symbols = append(symbols, fmt.Sprint(l.(map[string]any)["symbol"]))
	}
	if !slices.Equal(symbols, []string{"CURRENCY_USD", "XYZ"}) {
		t.Errorf("...398's trade shows the legs %v", symbols)
	}
	if r := e.call("get_transaction", `{"id":"T73508262002"}`); r.code() != "bad_argument" ||
		!strings.Contains(r.text, "holds this id in ...398 and ...783; pass account") {
		t.Errorf("a ref of two accounts: %.300s", r.text)
	}
	// Schwab holds no such row of ...783: the store's, which says so.
	r = e.call("get_transaction", `{"id":"T73508262002","account":"783"}`)
	if st, _ := r.value["store"].(map[string]any); r.isError || st == nil || st["category"] == "buy" ||
		!slices.Equal(r.warnings(), []string{"api_unavailable"}) || !strings.Contains(r.text, "[not_found]") {
		t.Errorf("...783's side: %.600s", r.text)
	}
}

// identsOf returns a list of strings of an answer.
func identsOf(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

// A window that Schwab may have cut, as full as its answers come, and that
// cannot be halved again, cuts the walk short: totals are null, complete is
// false, the warning partial names the days of each such window, and
// coverage holds none of them whole and counts no rows. The cap is 1 here,
// in the window cache of a tool that is get_transactions but for that cap,
// and both of ...135's windows hold a row.
func TestCappedWindow(t *testing.T) {
	t.Parallel()
	older := txnJSON(individual, 73508265201, ny(2024, time.September, 16, 10, 0), "DIVIDEND_OR_INTEREST",
		"Free Balance Interest Adjustment", 0.03)
	e := newEnv(t, withoutStore(), withTxnRows(individual, older), withTools(func(s *Server) {
		windows := newWindowCache()
		windows.full = 1
		testTool(s, "get_transactions_1", getTransactionsDef.args, func(ctx context.Context, c *call, in getTransactionsIn) (getTransactionsOut, error) {
			return getTransactions(ctx, c, windows, in)
		})
	}))
	r := e.call("get_transactions_1", `{"source":"schwab","account":"135","from":"2024-07-01","to":"2025-06-30"}`)
	c := txnCoverageOf(r, "...135")
	if r.value["complete"] != false || r.value["totals"] != nil || c["rows"] != nil || len(c["whole"].([]any)) != 0 ||
		c["reached_back"] != "2024-07-01" {
		t.Errorf("capped windows: %.800s", r.text)
	}
	for _, days := range []string{"from 2024-12-30 to 2025-06-30", "from 2024-07-01 to 2024-12-29"} {
		if !strings.Contains(r.text, "for part of the transactions of ...135 "+days) {
			t.Errorf("no warning names the capped window %s: %v", days, r.warnings())
		}
	}
	if got := txnRefsOf(txnRowsOf(r)); !slices.Contains(got, "T73508261943@...135") || !slices.Contains(got, "T73508265201@...135") {
		t.Errorf("the rows read: %v", got)
	}
}

// A row that Schwab sends without an activity ID has a T~ ref, of its time
// and digest, which get_transaction takes, in any case, whether or not the
// local history holds the row: one the store lacks is read from Schwab's
// days around its time, the day before and the day after its New York date
// included, since the window keeps rows by their trade day, which may be
// either (design 10.5): an evening's trade dated the next day, and a row
// dated the day before, as Schwab dates some. The row is the one whose ref
// is the one given, raw JSON included. A ref of a time later than now
// names no row, and Schwab is not asked.
func TestTildeRefsFromSchwab(t *testing.T) {
	t.Parallel()
	// noID returns a row of ...135 at at without an activity ID, of the
	// trade day day, holding net.
	noID := func(at time.Time, day nytime.Date, net float64) string {
		row := strings.Replace(txnJSON(individual, 1, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", net), `"activityId":1,`, "", 1)
		return strings.Replace(row, `"tradeDate":"`+txnTime(midnight(nytime.DateOf(at)))+`"`, `"tradeDate":"`+txnTime(midnight(day))+`"`, 1)
	}
	at := time.Date(2025, time.March, 5, 15, 0, 0, 0, time.UTC)
	rows := []string{
		noID(at, "2025-03-05", 0.04),
		noID(time.Date(2025, time.March, 5, 3, 0, 0, 0, time.UTC), "2025-03-05", 0.05),   // 22:00 on March 4
		noID(time.Date(2025, time.March, 12, 14, 0, 0, 0, time.UTC), "2025-03-11", 0.06), // 10:00 on March 12
	}
	for _, name := range []string{"without a store", "with a store that lacks the row"} {
		opts := []envOption{withTxnRows(individual, rows...)}
		if name == "without a store" {
			opts = append(opts, withoutStore())
		}
		e := newEnv(t, opts...)
		list := e.call("get_transactions", `{"source":"schwab","account":"135","from":"2025-03-01","to":"2025-03-31"}`)
		refs := map[string]string{} // by the time a ref holds
		for _, r := range txnRowsOf(list) {
			if s, _ := r["ref"].(string); strings.HasPrefix(s, "T~") {
				refs[s[2:17]] = s
			}
		}
		ref, evening, asOf := refs["20250305T150000"], refs["20250305T030000"], refs["20250312T140000"]
		if ref == "" || evening == "" || asOf == "" {
			t.Fatalf("%s: T~ refs among %v", name, txnRefsOf(txnRowsOf(list)))
		}
		r := e.call("get_transaction", `{"id":"`+ref+`","account":"135","include_raw":true}`)
		tx, _ := r.value["transaction"].(map[string]any)
		if r.isError || tx["ref"] != ref || r.value["source"] != "schwab" || r.value["store"] != nil || r.value["raw"] == nil ||
			tx["amount"] != 0.04 {
			t.Errorf("%s: get_transaction of %s: %.600s", name, ref, r.text)
		}
		for _, tc := range []struct {
			id, ref string
			amount  float64
		}{{evening, evening, 0.05}, {asOf, asOf, 0.06}, {strings.ToLower(evening), evening, 0.05}} {
			r := e.call("get_transaction", `{"id":"`+tc.id+`","account":"135"}`)
			tx, _ := r.value["transaction"].(map[string]any)
			if r.isError || tx["ref"] != tc.ref || tx["amount"] != tc.amount {
				t.Errorf("%s: get_transaction of %s: %.600s", name, tc.id, r.text)
			}
		}
		if r := e.call("get_transaction", `{"id":"`+ref+`"}`); r.code() != "bad_argument" || !strings.Contains(r.text, "account is needed") {
			t.Errorf("%s: no account given: %.300s", name, r.text)
		}
		missing := "T~20250306T150000.000000000000"
		if r := e.call("get_transaction", `{"id":"`+missing+`","account":"135"}`); r.code() != "not_found" ||
			!strings.Contains(r.text, "Schwab sent no transaction "+missing+" in account ...135 on the days around its time.") {
			t.Errorf("%s: a T~ ref of no row: %.300s", name, r.text)
		}
		// A time later than now, later today, in 4 days or in a year
		// mistyped, is no row's that Schwab could send: not_found, with no
		// request.
		before := e.fake.APICalls()
		for _, tc := range []struct{ args, at string }{
			{`{"id":"T~20261113T230000.000000000000","account":"135"}`, "2026-11-13 18:00"},
			{`{"id":"T~20261117T150000.000000000000","account":"135"}`, "2026-11-17 10:00"},
			{`{"id":"T~29991231T000000.000000000000"}`, "2999-12-30 19:00"},
		} {
			if r := e.call("get_transaction", tc.args); r.code() != "not_found" ||
				!strings.Contains(r.text, ": the time it holds, "+tc.at+" ET, is later than now.") {
				t.Errorf("%s: %s: %.300s", name, tc.args, r.text)
			}
		}
		if n := e.fake.APICalls() - before; n != 0 || strings.Contains(e.logs.String(), "Schwab could not be reached") {
			t.Errorf("%s: refs of times later than now sent %d requests, and logged:\n%s", name, n, e.logs)
		}
	}
}

// After Schwab gives an account a new hash, as on a new login, the walk in
// which it refuses the old one reads the directory again once, and asks
// with the new hash from then on, for every later window: the rows and the
// coverage name the account by its new hash, and next_cursor, whose digest
// is of the account's hash, is taken by the next call, which picks the
// account by that hash, so that the pages hold every row once.
// get_transaction answers with the hash that Schwab took, for an activity
// ID and for a T~ ref. And when two accounts have new hashes, the walk of
// every account reads the directory again once, for the first that Schwab
// refuses, and asks for the other with its new hash; an account read from
// the store takes its new hash too, in its rows and in the cursor, so that
// the next page, which keys its rows by that hash, repeats none. A read of
// the directory is warm for 5 minutes, so list_accounts reads it first,
// before the new hashes.
func TestTransactionsOfARehashedAccount(t *testing.T) {
	t.Parallel()
	old, rehashed := schwabtest.HashFor(individual), schwabtest.HashFor("rehashed")
	// asked counts the requests since the first from that asked with hash,
	// and those that read the account numbers.
	asked := func(e *env, from int, hash string) (int, int) {
		withHash, numbers := 0, 0
		for _, r := range e.fake.Requests()[from:] {
			if strings.Contains(r.Path, hash) {
				withHash++
			}
			if r.Route == schwabtest.AccountNumbersRoute {
				numbers++
			}
		}
		return withHash, numbers
	}
	e := newEnv(t, withoutStore())
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.Rehash(old, rehashed)
	from := len(e.fake.Requests())
	var got []string
	for i, p := range e.txnPages("get_transactions", `{"account":"135","from":"2024-01-01","to":"2025-06-30","limit":2}`, 10) {
		for _, row := range txnRowsOf(p) {
			got = append(got, fmt.Sprint(row["ref"]))
			if row["account_hash"] != rehashed {
				t.Errorf("page %d: %v has the hash %v", i+1, row["ref"], row["account_hash"])
			}
		}
		if c := txnCoverageOf(p, "...135"); c["account_hash"] != rehashed {
			t.Errorf("page %d: coverage of %v", i+1, c)
		}
	}
	if refused, numbers := asked(e, from, old); refused != 1 || numbers != 1 {
		t.Errorf("the walk of two windows asked %d times with the old hash and read the account numbers %d times, want once each", refused, numbers)
	}
	whole := txnRefsOf(txnRowsOf(e.call("get_transactions", `{"account":"135","from":"2024-01-01","to":"2025-06-30"}`)))
	if len(got) != 6 || len(whole) != 6 || !slices.Equal(got, func() []string {
		var refs []string
		for _, r := range whole {
			refs = append(refs, strings.TrimSuffix(r, "@...135"))
		}
		return refs
	}()) {
		t.Errorf("pages of 2 hold %v, one call %v", got, whole)
	}

	// get_transaction, of a T~ ref and then, after another new hash, of an
	// activity ID.
	at := time.Date(2025, time.March, 5, 15, 0, 0, 0, time.UTC)
	noID := strings.Replace(txnJSON(individual, 1, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.04), `"activityId":1,`, "", 1)
	var row schwab.Transaction
	if err := json.Unmarshal([]byte(noID), &row); err != nil {
		t.Fatal(err)
	}
	entries, err := history.DeriveAPIEntries([]schwab.Transaction{row}, individual)
	if err != nil {
		t.Fatal(err)
	}
	e = newEnv(t, withoutStore(), withTxnRows(individual, noID))
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	again := schwabtest.HashFor("rehashed again")
	for _, tc := range []struct{ id, from, to string }{{entries[0].Ref, old, rehashed}, {"73508261943", rehashed, again}} {
		e.fake.Rehash(tc.from, tc.to)
		from := len(e.fake.Requests())
		r := e.call("get_transaction", `{"id":"`+tc.id+`","account":"135"}`)
		tx, _ := r.value["transaction"].(map[string]any)
		if r.isError || tx["account_hash"] != tc.to {
			t.Errorf("get_transaction of %s after a new hash: %.400s", tc.id, r.text)
		}
		if refused, numbers := asked(e, from, tc.from); refused != 1 || numbers != 1 {
			t.Errorf("get_transaction of %s asked %d times with the old hash and read the account numbers %d times", tc.id, refused, numbers)
		}
	}
	latest, other := schwabtest.HashFor("rehashed once more"), schwabtest.HashFor("rehashed 398")
	e.fake.Rehash(again, latest)
	e.fake.Rehash(schwabtest.HashFor(options), other)
	from = len(e.fake.Requests())
	r := e.call("get_transactions", `{"from":"2024-01-01","to":"2025-06-30"}`)
	for masked, hash := range map[string]string{"...135": latest, "...398": other} {
		if c := txnCoverageOf(r, masked); c["account_hash"] != hash || c["read_from"] != "schwab" {
			t.Errorf("every account, two of them with new hashes: %s's coverage %v", masked, c)
		}
	}
	refused, _ := asked(e, from, again)
	refused398, numbers := asked(e, from, schwabtest.HashFor(options))
	if refused != 1 || refused398 != 0 || numbers != 1 {
		t.Errorf("every account, two of them with new hashes: asked %d times with ...135's old hash and %d times with ...398's, "+
			"and read the account numbers %d times", refused, refused398, numbers)
	}

	// Every account, with the shared store: ...398's 2025 is read from the
	// store, ...135's from Schwab, whose refusal of ...135's old hash gives
	// ...398 its new hash too, one that sorts before its old one. The store's
	// rows take the new hash in their place on the page and in the cursor,
	// as the next call keys them, so that the pages of 1 hold the rows of one
	// call, each once, though the first ends at a row of ...398's.
	e = newEnv(t, withTxnRows(individual, dividend135))
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	lower := schwabtest.HashFor("398 again")
	if lower >= schwabtest.HashFor(options) {
		t.Fatal("...398's new hash does not sort before its old one: the case shows nothing")
	}
	e.fake.Rehash(old, rehashed)
	e.fake.Rehash(schwabtest.HashFor(options), lower)
	pages := e.txnPages("get_transactions", `{"from":"2025-01-01","to":"2025-12-31","limit":1}`, 20)
	got = nil
	for i, p := range pages {
		for _, row := range txnRowsOf(p) {
			got = append(got, txnRefsOf([]map[string]any{row})...)
			if row["account"] == "...398" && (row["read_from"] != "store" || row["account_hash"] != lower) {
				t.Errorf("page %d: %v", i+1, row)
			}
		}
	}
	whole = txnRefsOf(txnRowsOf(e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)))
	if len(whole) < 3 || txnCoverageOf(pages[0], "...398")["read_from"] != "store" || !slices.Equal(got, whole) {
		t.Errorf("every account, ...398 from the store with a new hash: pages of 1 hold %v, one call %v", got, whole)
	}
}

// A cursor holds a row's account by its key, the account's hash, which a
// new login may change; the rows that a page places after a cursor keep
// it as the call found it, and the page and its cursor give each row the
// key that the next call finds (design 8.5). So no page repeats or skips a
// row across a new hash that a call learns of, as when Schwab refuses the
// hash that the directory, read in the last 5 minutes, still has: the
// account whose row the last page ended at, read from Schwab again, every
// window of it, as after the window cache's 60 seconds; the same account
// when Schwab then fails for it, and it is read from the store; and two
// accounts read from the store, whose transfer to each other ties on day,
// time and ref at the end of the store's page, where the new hashes
// reverse the two sides' order.
func TestCursorsAcrossANewHash(t *testing.T) {
	t.Parallel()
	old := schwabtest.HashFor(individual)
	lower := schwabtest.HashFor("135 again")
	if lower >= old {
		t.Fatal("...135's new hash does not sort before its old one: the cases show nothing")
	}
	// unkept registers get_transactions_unkept, whose window cache keeps no
	// read for the next call.
	unkept := withTools(func(s *Server) {
		var ticks atomic.Int64
		windows := newWindowCache()
		windows.now = func() time.Time { return testNow.Add(time.Duration(ticks.Add(1)) * time.Minute) }
		testTool(s, "get_transactions_unkept", getTransactionsDef.args, func(ctx context.Context, c *call, in getTransactionsIn) (getTransactionsOut, error) {
			return getTransactions(ctx, c, windows, in)
		})
	})
	// pagesAfter follows the cursor of first, page by page, and returns the
	// refs of first and of every page after it.
	pagesAfter := func(e *env, args string, first result) []string {
		refs := txnRefsOf(txnRowsOf(first))
		cur, _ := first.value["next_cursor"].(string)
		for i := 0; cur != ""; i++ {
			if i == 20 {
				t.Fatalf("%s: more than 20 pages", args)
			}
			r := e.call("get_transactions_unkept", txnWithCursor(t, args, cur))
			if r.isError {
				t.Fatalf("page %d of %s: %s", i+2, args, r.text)
			}
			refs = append(refs, txnRefsOf(txnRowsOf(r))...)
			cur, _ = r.value["next_cursor"].(string)
		}
		return refs
	}

	// Every account, from Schwab: the first page ends at ...135's newest row.
	newest := txnJSON(individual, 73508265701, ny(2025, time.December, 30, 10, 0), "DIVIDEND_OR_INTEREST",
		"Free Balance Interest Adjustment", 0.03)
	e := newEnv(t, withoutStore(), withTxnRows(individual, newest), unkept)
	args := `{"from":"2025-01-01","to":"2025-12-31","limit":1}`
	first := e.call("get_transactions_unkept", args)
	if refs := txnRefsOf(txnRowsOf(first)); !slices.Equal(refs, []string{"T73508265701@...135"}) {
		t.Fatalf("the first page holds %v", refs)
	}
	e.fake.Rehash(old, lower)
	got := pagesAfter(e, args, first)
	whole := txnRefsOf(txnRowsOf(e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)))
	if len(whole) < 3 || !slices.Equal(got, whole) {
		t.Errorf("from Schwab, a new hash after the first page: pages of 1 hold %v, one call %v", got, whole)
	}

	// ...135 alone, whose 2025 the store does not hold: the first page reads
	// Schwab and ends at its newest row of the days, which the store holds
	// too; with the new hash, Schwab fails for it, and the rest comes from
	// the store.
	e = newEnv(t, unkept)
	args = `{"account":"135","from":"2024-01-01","to":"2025-05-31","limit":1}`
	first = e.call("get_transactions_unkept", args)
	if row := txnRowsOf(first); len(row) != 1 || row[0]["ref"] != "T73508261949" || row[0]["read_from"] != "schwab" {
		t.Fatalf("the first page: %.600s", first.text)
	}
	e.fake.Rehash(old, lower)
	e.fake.Queue(schwabtest.TransactionsRoute, nil, schwabtest.Unavailable, schwabtest.Unavailable)
	got = pagesAfter(e, args, first)
	whole = txnRefsOf(txnRowsOf(e.call("get_transactions", `{"account":"135","source":"store","from":"2024-01-01","to":"2025-05-31"}`)))
	if len(whole) < 3 || !slices.Equal(got, whole) {
		t.Errorf("a new hash, then the store: pages of 1 hold %v, source store %v", got, whole)
	}

	// Every account, ...135 and ...398 from the store, which holds both sides
	// of a journal between them: one activity ID at one time. ...783 goes to
	// Schwab, which refuses its old hash, and the new hashes put ...398's
	// side first, where the old ones put ...135's.
	path := storeCopy(t)
	s, err := history.Open(t.Context(), history.Options{Path: path, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []struct {
		number string
		net    float64
	}{{individual, -25}, {options, 25}} {
		var row schwab.Transaction
		raw := txnJSON(side.number, 73508265801, ny(2026, time.June, 10, 10, 0), "JOURNAL", "Journal", side.net)
		if err = json.Unmarshal([]byte(raw), &row); err != nil {
			break
		}
		_, err = s.RecordTransactions(t.Context(), history.TransactionsRead{
			Read:    history.Read{SentAt: ny(2026, time.July, 2, 9, 0), AnsweredAt: ny(2026, time.July, 2, 9, 0).Add(time.Second)},
			Account: side.number, Query: schwab.TransactionQuery{Start: ny(2026, time.May, 31, 0, 0), End: ny(2026, time.July, 2, 0, 0)},
			Rows: []schwab.Transaction{row},
		})
		if err != nil {
			break
		}
	}
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	first135, first398 := schwabtest.HashFor("135 anew"), schwabtest.HashFor("398 anew")
	if old < schwabtest.HashFor(options) || first135 > first398 {
		t.Fatal("the new hashes do not reverse the order of the old: the case shows nothing")
	}
	e = newEnv(t, withStore(path))
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.Rehash(schwabtest.HashFor(empty), schwabtest.HashFor("783 anew"))
	e.fake.Rehash(old, first135)
	e.fake.Rehash(schwabtest.HashFor(options), first398)
	pages := e.txnPages("get_transactions", `{"from":"2026-06-01","to":"2026-06-30","limit":1}`, 10)
	got = nil
	for _, p := range pages {
		got = append(got, txnRefsOf(txnRowsOf(p))...)
	}
	for _, masked := range []string{"...135", "...398"} {
		if c := txnCoverageOf(pages[0], masked); c["read_from"] != "store" {
			t.Errorf("the journal's side of %s: %v", masked, c)
		}
	}
	if want := []string{"T73508265801@...398", "T73508265801@...135"}; !slices.Equal(got, want) {
		t.Errorf("a tie at the end of the store's page: pages of 1 hold %v, want %v", got, want)
	}
}

// A window that Schwab may have cut ends the days that coverage calls
// whole, though an older one was read whole: whole runs from the newest
// window back, and the newest here holds two rows of one instant, which no
// halving parts at a cap of 2 rows.
func TestNewerWindowCapped(t *testing.T) {
	t.Parallel()
	at := ny(2025, time.June, 20, 12, 0)
	e := newEnv(t, withoutStore(), withTxnRows(individual,
		txnJSON(individual, 73508265301, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.01),
		txnJSON(individual, 73508265302, at, "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.02),
		txnJSON(individual, 73508265303, ny(2024, time.September, 16, 10, 0), "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.03),
	), withTools(func(s *Server) {
		windows := newWindowCache()
		windows.full = 2
		testTool(s, "get_transactions_2", getTransactionsDef.args, func(ctx context.Context, c *call, in getTransactionsIn) (getTransactionsOut, error) {
			return getTransactions(ctx, c, windows, in)
		})
	}))
	r := e.call("get_transactions_2", `{"source":"schwab","account":"135","from":"2024-07-01","to":"2025-06-30"}`)
	c := txnCoverageOf(r, "...135")
	if r.isError || len(c["whole"].([]any)) != 0 || c["reached_back"] != "2024-07-01" || slices.Contains(r.warnings(), "empty_not_proof") {
		t.Errorf("the newer window capped, the older read whole: %.800s", r.text)
	}
	if !strings.Contains(r.text, "for part of the transactions of ...135 from 2024-12-30 to 2025-06-30") ||
		strings.Contains(r.text, "for part of the transactions of ...135 from 2024-07-01") {
		t.Errorf("the capped window, and only it, is named: %v", r.warnings())
	}
}

// A login that Schwab ends between an account's two windows is no refusal
// of the older window as too old: the refresh's refusal is a 400 too, but
// it is the login's, so the call is login_expired, Schwab's detail beside.
func TestLoginEndsBetweenWindows(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore(), withLoginAt(testNow.Add(-10*time.Minute)), withFake(func(f *schwabtest.Server) {
		f.EndLogin()
		f.Queue(schwabtest.TransactionsRoute, nil, schwabtest.NotAuthorized)
	}))
	r := e.call("get_transactions", `{"source":"schwab","account":"135","from":"2024-03-01","to":"2025-06-30"}`)
	if r.code() != "login_expired" || !strings.Contains(r.text, "Detail: HTTP 400 from Schwab") || strings.Contains(r.text, "too old") {
		t.Errorf("a login ended between two windows: %.600s", r.text)
	}
}

// store_waiting counts the rows that wait for the user's decision, held out
// and uncertain alike: here an export's dividend between two of Schwab's
// that it could be either of.
func TestStoreWaitingCountsUncertain(t *testing.T) {
	t.Parallel()
	path := storeCopy(t)
	s, err := history.Open(t.Context(), history.Options{Path: path, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	read := history.TransactionsRead{
		Read:    history.Read{SentAt: ny(2026, time.July, 1, 9, 0), AnsweredAt: ny(2026, time.July, 1, 9, 0).Add(time.Second)},
		Account: individual,
		Query:   schwab.TransactionQuery{Start: ny(2026, time.June, 1, 0, 0), End: ny(2026, time.July, 1, 0, 0)},
	}
	for _, raw := range []string{
		txnJSON(individual, 73508265401, ny(2026, time.June, 10, 10, 0), "DIVIDEND_OR_INTEREST", "Ordinary Dividend~EFGH", 2),
		txnJSON(individual, 73508265402, ny(2026, time.June, 12, 10, 0), "DIVIDEND_OR_INTEREST", "Ordinary Dividend~EFGH", 2),
	} {
		var row schwab.Transaction
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			t.Fatal(err)
		}
		read.Rows = append(read.Rows, row)
	}
	_, err = s.RecordTransactions(t.Context(), read)
	if err == nil {
		csv := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n" +
			`"06/11/2026","Qualified Dividend","EFGH","EFGH INC","","","","$2.00"` + "\n"
		_, err = s.Import(t.Context(), []history.FileInput{{Name: "Individual_XXX135_Transactions_20260701-120000.csv", Content: []byte(csv)}},
			history.ImportOptions{Account: individual, From: "2026-06-01", To: "2026-06-30", Completeness: "complete"})
	}
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, withStore(path))
	r := e.call("get_transactions", `{"source":"store","account":"135","from":"2026-06-01","to":"2026-06-30"}`)
	w, _ := txnCoverageOf(r, "...135")["waiting"].(map[string]any)
	if r.isError || w["uncertain"] != 1.0 || w["held_out"] != 0.0 ||
		!strings.Contains(r.text, "1 row of ...135 in these days waits for the user's decision") {
		t.Errorf("an uncertain row: %.800s", r.text)
	}
}

// A walk whose budget left no time for its first request answers a page of
// no rows, complete false, with a cursor of no position, which starts the
// next call where this one started; no account's coverage counts rows or
// holds a day. A window kept from an earlier call is read without time,
// but the walk still stops at the newest window that the budget left
// unread, whatever account comes after it, and the page holds no row of
// that window's days, its last day's included: the unread rows could
// come between them. A call that continues a walk and has no time keeps
// the position its cursor gave.
func TestWalkWithNoTime(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore(), withTxnRows(individual, txnJSON(individual, 73508265101, ny(2025, time.December, 31, 10, 0),
		"DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", 0.02)))
	callBy := func(deadline time.Time) *call {
		return &call{
			s: e.srv, tool: "get_transactions", mask: e.srv.mask, accounts: e.srv.accounts, store: e.srv.store,
			now: testNow, budget: time.Second, deadline: deadline,
		}
	}
	wc := newWindowCache()
	c := callBy(time.Now())
	out, err := getTransactions(t.Context(), c, wc, getTransactionsIn{Source: "schwab", From: "2025-01-01", To: "2025-03-31"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Transactions) != 0 || out.Complete || out.Totals != nil || out.NextCursor == nil {
		t.Fatalf("a walk with no time: %d rows, complete %v, totals %v, cursor %v", len(out.Transactions), out.Complete, out.Totals, out.NextCursor)
	}
	b, err := base64.RawURLEncoding.DecodeString(string(*out.NextCursor))
	var body cursorBody
	if err != nil || json.Unmarshal(b, &body) != nil || string(body.K) != "null" {
		t.Errorf("the cursor holds %s, want no position", b)
	}
	if !slices.ContainsFunc(c.warnings, func(w warning) bool { return w.Code == "partial" }) {
		t.Errorf("warnings %v", c.warnings)
	}
	for _, cov := range out.Coverage {
		if cov.Rows != nil || len(cov.Whole) != 0 || cov.ReachedBack != nil {
			t.Errorf("%s's coverage of a walk that read nothing: %+v", cov.Account, cov)
		}
	}

	// ...135's newer window of the longer walk below, kept from a call that
	// had the time; its older one is not kept.
	if _, err := getTransactions(t.Context(), callBy(time.Now().Add(time.Minute)), wc,
		getTransactionsIn{Source: "schwab", Account: "135", From: "2025-01-30", To: "2025-12-31"}); err != nil {
		t.Fatal(err)
	}
	c = callBy(time.Now())
	out, err = getTransactions(t.Context(), c, wc, getTransactionsIn{Source: "schwab", From: "2024-03-01", To: "2025-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Transactions) != 0 || out.Complete || !slices.ContainsFunc(c.warnings, func(w warning) bool {
		return w.Code == "partial" && strings.Contains(string(w.Message), "only rows after 2025-12-31")
	}) {
		t.Errorf("a walk past a kept window: %d rows, warnings %v", len(out.Transactions), c.warnings)
	}

	// A call that continues a walk, with no time either, keeps the position
	// that its cursor gave.
	in := getTransactionsIn{Source: "schwab", From: "2024-03-01", To: "2025-12-31"}
	q, err := c.txnQueryOf(in)
	if err != nil {
		t.Fatal(err)
	}
	pos := &[4]string{"2025-06-02", "2025-06-02T14:30:27.000Z", "T73508261943", schwabtest.HashFor(individual)}
	cur, err := c.newCursor(q.args(""), pos)
	if err != nil {
		t.Fatal(err)
	}
	in.Cursor = string(*cur)
	out, err = getTransactions(t.Context(), callBy(time.Now()), wc, in)
	if err != nil || out.NextCursor == nil {
		t.Fatalf("a continued walk with no time: %v, cursor %v", err, out.NextCursor)
	}
	b, err = base64.RawURLEncoding.DecodeString(string(*out.NextCursor))
	want, _ := json.Marshal(pos)
	if err != nil || json.Unmarshal(b, &body) != nil || string(body.K) != string(want) || len(out.Transactions) != 0 {
		t.Errorf("the cursor holds %s, want the position given, %s", b, want)
	}
}

// Without a login, auto reads each account from the local history as far
// as it holds the days, and says why (api_unavailable), the login's error
// among its words; what needs Schwab, or an account that no one knows,
// fails with the login's error.
func TestTransactionsWithoutLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutLogin())
	r := e.call("get_transactions", `{"from":"2024-01-01","to":"2024-12-31"}`)
	if r.isError || r.value["source"] != "store" || !slices.Contains(r.warnings(), "api_unavailable") ||
		!strings.Contains(r.text, "[not_logged_in]") || len(txnRowsOf(r)) != 5 {
		t.Errorf("auto without a login: %.600s", r.text)
	}
	for _, masked := range []string{"...135", "...398", "...783"} {
		if c := txnCoverageOf(r, masked); c["read_from"] != "store" || c["account_hash"] != nil {
			t.Errorf("%s: %v", masked, c)
		}
	}
	if r := e.call("get_transactions", `{"source":"schwab"}`); r.code() != "not_logged_in" {
		t.Errorf("source schwab without a login: %.300s", r.text)
	}
	if r := e.call("get_transaction", `{"id":"99999999999"}`); r.code() != "not_logged_in" {
		t.Errorf("an id that the store does not hold, without a login: %.300s", r.text)
	}
	// Without a store either, no account is known.
	none := newEnv(t, withoutLogin(), withoutStore())
	for _, c := range []struct{ tool, args string }{
		{"get_transactions", `{}`},
		{"get_transactions", `{"account":"135"}`},
		{"get_transaction", `{"id":"73508261946","account":"135"}`},
	} {
		if r := none.call(c.tool, c.args); r.code() != "not_logged_in" {
			t.Errorf("%s %s without a login or a store: %.300s", c.tool, c.args, r.text)
		}
	}
}

// Under auto, an account that Schwab cannot be read for is read from the
// local history as far as it holds the days, as when the directory could
// not read Schwab (TestTransactionsWithoutLogin), though the directory's
// read is warm: when Schwab ends the login after that read, the account's
// first request fails, and the account is read from the store, the warning
// api_unavailable saying why; once the server knows that the login has
// ended, no account is asked, and every account is read from the store,
// for longer ranges than a walk of Schwab's too. When Schwab answers 503,
// each account fails in turn, and is read from the store, and the accounts
// that fail alike share one warning. The pages of such an answer hold the
// store's rows once, as source store's do.
func TestTransactionsFallBackToTheStore(t *testing.T) {
	t.Parallel()
	args := `{"account":"135","from":"2024-01-01","to":"2025-05-31"}`
	// fromStore fails unless r holds ...135's rows from the store, its
	// coverage whole from from to 2024-12-30, the store's last day of it,
	// with the warnings api_unavailable, which quotes why, and
	// store_coverage_partial.
	fromStore := func(name string, r result, from, why string) {
		t.Helper()
		c := txnCoverageOf(r, "...135")
		whole, _ := json.Marshal(c["whole"])
		if r.isError || r.value["source"] != "store" || c["read_from"] != "store" || c["error"] != nil ||
			string(whole) != `[{"from":"`+from+`","to":"2024-12-30"}]` {
			t.Errorf("%s: %.600s", name, r.text)
		}
		if !slices.Contains(r.warnings(), "api_unavailable") || !slices.Contains(r.warnings(), "store_coverage_partial") ||
			!strings.Contains(r.text, why) {
			t.Errorf("%s: warnings %v, want api_unavailable with %q: %.600s", name, r.warnings(), why, r.text)
		}
		for _, row := range txnRowsOf(r) {
			if row["read_from"] != "store" || row["account"] != "...135" {
				t.Errorf("%s: a row %v", name, row)
			}
		}
	}

	e := newEnv(t, withLoginAt(testNow.Add(-10*time.Minute)))
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.EndLogin()
	e.fake.RevokeAccess()
	r := e.call("get_transactions", args)
	fromStore("a login that Schwab ended after the directory read it", r, "2024-01-01",
		"Schwab could not be read for ...135, so its rows come from the local history, as far as it holds these days: [login_expired]")
	if len(txnRowsOf(r)) == 0 {
		t.Errorf("no rows of the store: %.600s", r.text)
	}
	before := e.fake.APICalls()
	fromStore("a login that the server knows has ended", e.call("get_transactions", args), "2024-01-01",
		"Schwab could not be read, so each account was read from the local history as far as it holds these days: [login_expired]")
	fromStore("more days than a walk of Schwab's reads", e.call("get_transactions", `{"account":"135","from":"2023-01-01","to":"2025-05-31"}`),
		"2023-06-12", "[login_expired]")
	if n := e.fake.APICalls() - before; n != 0 {
		t.Errorf("with a login known to have ended, the server sent %d requests", n)
	}

	e = newEnv(t)
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.Override(schwabtest.TransactionsRoute, schwabtest.Unavailable)
	fromStore("Schwab answering 503", e.call("get_transactions", args), "2024-01-01",
		"so its rows come from the local history, as far as it holds these days: [schwab_unavailable]")
	all := e.call("get_transactions", `{"from":"2024-01-01","to":"2025-05-31"}`)
	if !strings.Contains(all.text, "Schwab could not be read for ...135, ...398 and ...783, so their rows come from the local history") {
		t.Errorf("every account, Schwab answering 503: %.800s", all.text)
	}
	for _, masked := range []string{"...135", "...398", "...783"} {
		if c := txnCoverageOf(all, masked); c["read_from"] != "store" || c["error"] != nil {
			t.Errorf("every account, Schwab answering 503: %s's coverage %v", masked, c)
		}
	}
	var pages []string
	for _, p := range e.txnPages("get_transactions", `{"account":"135","from":"2024-01-01","to":"2025-05-31","limit":2}`, 10) {
		pages = append(pages, txnRefsOf(txnRowsOf(p))...)
	}
	store := txnRefsOf(txnRowsOf(e.call("get_transactions", `{"account":"135","source":"store","from":"2024-01-01","to":"2025-05-31"}`)))
	if len(store) < 3 || !slices.Equal(pages, store) {
		t.Errorf("pages of 2, Schwab answering 503, hold %v; source store %v", pages, store)
	}
}

// The days that the local history holds as whole history are its api and
// file runs merged where they overlap or meet, whatever their order, and
// clipped to the asked days, down to one day (docs/store.md, Coverage).
func TestWholeOf(t *testing.T) {
	t.Parallel()
	w := &txnWalk{q: txnQuery{from: "2025-01-01", to: "2025-12-31"}}
	got := w.wholeOf([]history.CoverageRun{
		{Source: "api", From: "2025-07-01", To: "2025-09-30"},
		{Source: "file", From: "2024-06-01", To: "2025-03-31"},
		{Source: "api", From: "2025-04-01", To: "2025-06-30"},
		{Source: "file", From: "2025-05-01", To: "2025-05-31"},
		{Source: "api", From: "2025-10-02", To: "2025-12-29"},
		{Source: "file", From: "2025-12-31", To: "2026-02-01"},
	})
	if want := []span{{From: "2025-01-01", To: "2025-09-30"}, {From: "2025-10-02", To: "2025-12-29"}, {From: "2025-12-31", To: "2025-12-31"}}; !slices.Equal(got, want) {
		t.Errorf("whole %v, want %v", got, want)
	}
	if got := w.wholeOf(nil); got == nil || len(got) != 0 {
		t.Errorf("no runs: %#v, want []", got)
	}
}
