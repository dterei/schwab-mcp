package schwab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/filelock"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// probeNow is when the probes' runs on the fake take place: after every row
// of the fixtures, so that the history walk reads them all, with two of
// them in the last 60 days, for Q12.
var probeNow = time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)

// The fixtures' accounts by their place in the account numbers, as the
// report labels them.
var (
	firstAccount  = schwabtest.FixtureAccounts[0] // {account 1}, a cash account with nine rows
	secondAccount = schwabtest.FixtureAccounts[1] // {account 2}, a margin account with five
	thirdAccount  = schwabtest.FixtureAccounts[2] // {account 3}, without rows
)

// The rows of the fixtures that the probes' subtests change or name, by
// their activityId.
const (
	optionClose = 73508261941 // {account 2}'s XYZ option, 2026-08-04
	metaSale    = 73508261942 // {account 2}'s META sale, 2026-03-13, an execution of metaOrder
	xyzDividend = 73508261948 // {account 2}'s XYZ dividend, 2026-07-15
	metaOrder   = 1004517688020
)

// newProbeLive returns a run in the default mode on the fake at at, as
// newFakeLiveAt does, with the probes of liveProbes that names names, or
// every default probe without any, and the fake's orders and Market Data
// served as for the default run: the order of the META sale, entered
// minutes before its fill, kept for a year, and SPY's and VTI's closes,
// with a dividend ex on 2026-09-18.
func newProbeLive(t *testing.T, at time.Time, names ...string) *fakeLive {
	t.Helper()
	fl := newFakeLiveAt(t, at)
	fl.r.probes = probesNamed(t, modeDefault, names...)
	serveOrders(fl, []fakeOrder{metaSaleOrder}, keptFor(MaxTransactionWindow))
	fakeMarket{exDate: marketExDate, amount: 1.9}.serve(fl)
	return fl
}

// probesNamed returns the probes of liveProbes that names names, in
// liveProbes' order, or every probe of mode without any.
func probesNamed(t *testing.T, mode string, names ...string) []liveProbe {
	t.Helper()
	var ps []liveProbe
	for _, p := range liveProbes {
		if len(names) == 0 && p.mode == mode || slices.Contains(names, p.name) {
			ps = append(ps, p)
		}
	}
	if len(names) > 0 && len(ps) != len(names) {
		t.Fatalf("probes %q: found %d", names, len(ps))
	}
	return ps
}

// again returns another run of fl's login, fake and state, as a later run
// would be, with the same probes and mode, at the time at, to which it
// moves fl's clock, and its report in an out folder of its own; with login
// set, after a new login, as the owner's weekly one.
func (fl *fakeLive) again(t *testing.T, at time.Time, login bool) *fakeLive {
	t.Helper()
	fl.clock.set(at)
	if login {
		fl.login(t)
	}
	next := &fakeLive{f: fl.f, clock: fl.clock, dir: fl.dir}
	r := next.newRun(t, "run-"+at.UTC().Format("20060102T150405"))
	r.probes, r.mode, r.pageCap = fl.r.probes, fl.r.mode, fl.r.pageCap
	if fl.r.deadline.IsZero() {
		r.deadline = time.Time{}
	}
	next.r = r
	return next
}

// runClean runs the harness, and fails the test if the report, the lines
// printed or the run's error hold any of fl's secrets.
func (fl *fakeLive) runClean(t *testing.T) error {
	t.Helper()
	err := fl.run(t)
	if b, readErr := os.ReadFile(filepath.Join(fl.r.out, "report.txt")); readErr == nil {
		fl.noSecrets(t, "the report", string(b))
	}
	fl.noSecrets(t, "the lines printed", fl.lines()...)
	fl.noSecrets(t, "the run's error", fmt.Sprint(err))
	return err
}

// verdictOf returns what probe's section of report gives as its verdict,
// without a W-8 mark, or "" when it has none.
func verdictOf(report, probe string) string {
	sec := sectionOf(report, "probe "+probe)
	line, _, _ := strings.Cut(sec, "\n")
	return strings.TrimPrefix(strings.TrimPrefix(line, w8Mark), "verdict: ")
}

// sectionOf returns the lines of report's first section titled title,
// without its heading, or "".
func sectionOf(report, title string) string {
	_, rest, ok := strings.Cut(report, "\n## "+title+"\n")
	if !ok {
		return ""
	}
	sec, _, _ := strings.Cut(rest, "\n\n")
	return sec
}

// wantVerdicts fails the test for each probe of want whose verdict in fl's
// report is not the one want gives.
func wantVerdicts(t *testing.T, report string, want map[string]string) {
	t.Helper()
	for _, probe := range slices.Sorted(func(yield func(string) bool) {
		for p := range want {
			if !yield(p) {
				return
			}
		}
	}) {
		if got := verdictOf(report, probe); got != want[probe] {
			t.Errorf("probe %s's verdict:\n got %s\nwant %s", probe, got, want[probe])
		}
	}
}

// wantEvidence fails the test unless probe's section of report holds each
// of lines, in order, each whole.
func wantEvidence(t *testing.T, report, probe string, lines ...string) {
	t.Helper()
	sec := sectionOf(report, "probe "+probe)
	if sec == "" {
		t.Errorf("no section of probe %s in:\n%s", probe, report)
		return
	}
	wantLines(t, sec+"\n", lines...)
}

// schwabTime returns t as Schwab gives an instant in an answer.
func schwabTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05+0000") }

// A fakeOrder is an order as the fake serves it, on the paths of orders,
// which no Route serves: entered at entered, and filled at filled, working
// until then, or for good while filled is zero.
type fakeOrder struct {
	account         schwabtest.FixtureAccount
	id              int64
	entered, filled time.Time
}

// metaSaleOrder is the order of the META sale, entered five minutes before
// its fill.
var metaSaleOrder = fakeOrder{
	account: secondAccount, id: metaOrder,
	entered: time.Date(2026, 3, 13, 13, 50, 9, 0, time.UTC), filled: time.Date(2026, 3, 13, 13, 55, 9, 0, time.UTC),
}

// json returns o as Schwab sends an order, as of now.
func (o fakeOrder) json(now time.Time) string {
	status, filled := "WORKING", ""
	if !o.filled.IsZero() && !now.Before(o.filled) {
		status = "FILLED"
		filled = `,"closeTime":"` + schwabTime(o.filled) + `","orderActivityCollection":[{"activityType":"EXECUTION","executionType":"FILL",` +
			`"quantity":1.0,"orderRemainingQuantity":0.0,"executionLegs":[{"legId":1,"price":603.4,"quantity":1.0,"mismarkedQuantity":0.0,` +
			`"instrumentId":1873219,"time":"` + schwabTime(o.filled) + `"}]}]`
	}
	return fmt.Sprintf(`{"session":"NORMAL","duration":"DAY","orderType":"MARKET","complexOrderStrategyType":"NONE","quantity":1.0,`+
		`"orderLegCollection":[{"orderLegType":"EQUITY","legId":1,"instrument":{"assetType":"EQUITY","symbol":"META","instrumentId":1873219},`+
		`"instruction":"SELL","positionEffect":"CLOSING","quantity":1.0}],"orderStrategyType":"SINGLE","orderId":%d,"cancelable":false,`+
		`"editable":false,"status":"%s","enteredTime":"%s"%s,"accountNumber":%s}`,
		o.id, status, schwabTime(o.entered), filled, o.account.Number)
}

// keptFor returns the rule of a list of orders kept for d: it holds an
// order entered at most d before now.
func keptFor(d time.Duration) func(entered, now time.Time) bool {
	return func(entered, now time.Time) bool { return now.Sub(entered) <= d }
}

// serveOrders serves orders on fl's fake: each by its id, and the lists of
// an account's and of every account's orders entered in the query's span,
// of those that kept keeps as of the fake's clock.
func serveOrders(fl *fakeLive, orders []fakeOrder, kept func(entered, now time.Time) bool) {
	list := func(w http.ResponseWriter, r *http.Request, of func(fakeOrder) bool) {
		q := r.URL.Query()
		from, err1 := time.Parse(schwabTimeFormat, q.Get("fromEnteredTime"))
		to, err2 := time.Parse(schwabTimeFormat, q.Get("toEnteredTime"))
		if err1 != nil || err2 != nil {
			writeJSON(w, http.StatusBadRequest, `{"message":"a guess: this span is not recorded"}`)
			return
		}
		now := fl.clock.Now()
		var es []string
		for _, o := range orders {
			if of(o) && !o.entered.Before(from) && !o.entered.After(to) && kept(o.entered, now) {
				es = append(es, o.json(now))
			}
		}
		writeJSON(w, http.StatusOK, "["+strings.Join(es, ",")+"]")
	}
	fl.f.OverridePath("GET /trader/v1/accounts/{hash}/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, o := range orders {
			if o.account.Hash == r.PathValue("hash") && strconv.FormatInt(o.id, 10) == r.PathValue("id") {
				writeJSON(w, http.StatusOK, o.json(fl.clock.Now()))
				return
			}
		}
		writeJSON(w, http.StatusNotFound, `{"message":"a guess: no such order"}`)
	})
	fl.f.OverridePath("GET /trader/v1/accounts/{hash}/orders", func(w http.ResponseWriter, r *http.Request) {
		list(w, r, func(o fakeOrder) bool { return o.account.Hash == r.PathValue("hash") })
	})
	fl.f.OverridePath("GET /trader/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		list(w, r, func(fakeOrder) bool { return true })
	})
}

// marketExDate is the ex-date of the fake's last dividend of SPY and VTI.
var marketExDate = time.Date(2026, 9, 18, 0, 0, 0, 0, newYork())

// A fakeMarket serves Market Data's quotes and daily price history of
// liveBenchmarks, for Q8, as of the fake's clock: a close for each weekday
// of the last year, before today, and the last dividend, of amount, ex on
// exDate once the clock has reached it and a quarter before until then.
// With adjusted set, the closes before an ex-date that has passed are
// scaled by the dividend's factor, 1 - amount over the close before it.
type fakeMarket struct {
	exDate   time.Time
	amount   float64
	adjusted bool
}

// close returns sym's close on day, a New York date, as traded.
func (m fakeMarket) close(sym string, day time.Time) float64 {
	base := map[string]float64{"SPY": 650, "VTI": 320}[sym]
	return math.Round((base+0.05*float64(day.Sub(time.Date(2025, 1, 1, 0, 0, 0, 0, newYork()))/oneDay))*100) / 100
}

// closes returns sym's closes as of now, by New York date in order.
func (m fakeMarket) closes(sym string, now time.Time) (days []time.Time, closes []float64) {
	ny := now.In(newYork())
	today := time.Date(ny.Year(), ny.Month(), ny.Day(), 0, 0, 0, 0, newYork())
	factor := 1.0
	if m.adjusted && !now.Before(m.exDate) {
		before := m.exDate.AddDate(0, 0, -1)
		for before.Weekday() == time.Saturday || before.Weekday() == time.Sunday {
			before = before.AddDate(0, 0, -1)
		}
		factor = 1 - m.amount/m.close(sym, before)
	}
	for day := today.AddDate(-1, 0, 0); day.Before(today); day = day.AddDate(0, 0, 1) {
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		c := m.close(sym, day)
		if day.Before(m.exDate) {
			c = math.Round(c*factor*100) / 100
		}
		days, closes = append(days, day), append(closes, c)
	}
	return days, closes
}

// serve serves m on fl's fake.
func (m fakeMarket) serve(fl *fakeLive) {
	fl.f.OverridePath("GET /marketdata/v1/quotes", func(w http.ResponseWriter, r *http.Request) {
		now := fl.clock.Now()
		ex := m.exDate
		if now.Before(ex) {
			ex = ex.AddDate(0, -3, 0)
		}
		var parts []string
		for sym := range strings.SplitSeq(r.URL.Query().Get("symbols"), ",") {
			parts = append(parts, fmt.Sprintf(`%q:{"assetMainType":"EQUITY","symbol":%q,"quote":{"closePrice":%.2f},`+
				`"fundamental":{"divAmount":%.2f,"divExDate":"%sT00:00:00Z","divPayAmount":%.2f}}`,
				sym, sym, m.close(sym, now), 4*m.amount, ex.Format(time.DateOnly), m.amount))
		}
		writeJSON(w, http.StatusOK, "{"+strings.Join(parts, ",")+"}")
	})
	fl.f.OverridePath("GET /marketdata/v1/pricehistory", func(w http.ResponseWriter, r *http.Request) {
		sym := r.URL.Query().Get("symbol")
		days, closes := m.closes(sym, fl.clock.Now())
		var b strings.Builder
		for i, day := range days {
			if i > 0 {
				b.WriteByte(',')
			}
			c := closes[i]
			// Schwab's daily candles are at midnight in Chicago.
			fmt.Fprintf(&b, `{"open":%.2f,"high":%.2f,"low":%.2f,"close":%.2f,"volume":1000000,"datetime":%d}`,
				c, c, c, c, day.Add(time.Hour).UnixMilli())
		}
		writeJSON(w, http.StatusOK, `{"candles":[`+b.String()+`],"symbol":"`+sym+`","empty":false}`)
	})
}

// fixtureRow returns the JSON of the row of transactions.json with the
// given activityId, changed as withKeys changes an object.
func fixtureRow(t *testing.T, id int64, changes ...string) string {
	t.Helper()
	for _, e := range elements(t, readTestdata(t, "transactions.json")) {
		var probe struct {
			ActivityID int64 `json:"activityId"`
		}
		if json.Unmarshal(e, &probe) == nil && probe.ActivityID == id {
			var b bytes.Buffer
			if err := json.Compact(&b, e); err != nil {
				t.Fatal(err)
			}
			return withKeys(b.String(), changes...)
		}
	}
	t.Fatalf("no row %d in transactions.json", id)
	return ""
}

// TestLiveOnTheFake runs the live mode's harness, everything but TestLive's
// front door, against the fake over HTTPS, with the probes of the open
// questions: the default run with every default probe, each opt-in mode,
// and each probe again under the fake's settings for what Schwab does that
// is not known, each a subtest. It checks every verdict; that no report,
// line printed or error holds an identifier of the fixtures, a path of the
// run's or the planted user name; that nothing is written under the
// owner's cache; and, on a guard apart from the run's, that a POST is
// refused. One fake clock serves every wait, so the runs take seconds.
func TestLiveOnTheFake(t *testing.T) {
	before := liveCacheListing()
	t.Cleanup(func() {
		if after := liveCacheListing(); !slices.Equal(before, after) {
			t.Errorf("the owner's live folder held %q, and now %q", before, after)
		}
	})
	for _, sub := range []struct {
		name string
		test func(t *testing.T)
	}{
		{"the default run", testDefaultRun},
		{"a guard apart from the run refuses a POST", func(t *testing.T) { guardRefusesPosts(t, newFakeLive(t)) }},
		{"Q1", testDepthOnTheFake},
		{"Q4", testSymbolFilterOnTheFake},
		{"Q5", testOrdersDepthOnTheFake},
		{"Q6", testScopeOnTheFake},
		{"Q8", testAdjustmentOnTheFake},
		{"Q10", testHashesOnTheFake},
		{"Q11", testShapesOnTheFake},
		{"Q12", testChangesOnTheFake},
		{"Q12 with a cut answer", testChangesCutOnTheFake},
		{"Q15", testRefreshOnTheFake},
		{"extras", testExtrasOnTheFake},
		{"watch=balances", testBalancesOnTheFake},
		{"watch=fills", testFillsOnTheFake},
		{"rate", testRateOnTheFake},
	} {
		t.Run(sub.name, func(t *testing.T) {
			t.Parallel() // each run has a fake, a clock and a folder of its own
			sub.test(t)
		})
	}
}

// The default run on the fake runs the checks and then every default
// probe, each with its verdict, under the fake's defaults, sending only
// reads, within its budget and its deadline, and marking each line that
// reveals W-8 rows.
func testDefaultRun(t *testing.T) {
	fl := newProbeLive(t, probeNow)
	err := fl.runClean(t)
	report := fl.report(t)
	if err != nil {
		t.Fatal(err)
	}
	wantLines(t, report, "probes: Q10, Q1, Q2, Q4, Q5, Q11, Q12, extras, Q8, Q15, Q6")
	wantVerdicts(t, report, map[string]string{
		"Q10": "inconclusive: no earlier run under another login or another app to compare with; this run's hashes are kept for the next",
		"Q1": "a window older than an account's history answers 200 with []; " +
			"the oldest rows, by New York date: {account 1} 2023-06-12, {account 2} 2025-01-14",
		"Q2":     "from 18:00 to midnight, no fill seen; from midnight to 04:00, tradeDate is the fill's instant",
		"Q4":     "the filter drops dividends; it drops W-8 journals",
		"Q5":     "a year: an order entered more than 60 days ago came back",
		"Q11":    "13 shapes among 14 rows of 6 types",
		"Q12":    "inconclusive: no earlier run to compare with; this run's rows are kept for the next",
		"extras": "a window's start is inclusive and its end inclusive; a window takes a row by its time",
		"Q8":     "inconclusive: it needs a run before an ex-date and one after; this run's closes are kept for the next",
		"Q15":    "a refresh leaves older access tokens working, and keeps the refresh token",
		"Q6":     `the login's scope is "api"; no read of the account API was refused as unauthorized`,
	})
	wantEvidence(t, report, "Q1",
		"{account 1}: the oldest row is of 2023-06-12, New York time; the 3 windows before it answered 200 with []; the window of 2000: 200 with []",
		"{account 2}: the oldest row is of 2025-01-14, New York time; the 3 windows before it answered 200 with []; the window of 2000: 200 with []",
		"{account 3}: no row back to 2000-01-01, where the walk stops; every window answered 200 with []; the window of 2000: 200 with []")
	wantEvidence(t, report, "Q2",
		"fills from 18:00 to midnight, New York time: 0; tradeDate the fill's instant: 0, the next midnight: 0, the same day's midnight: 0, another time: 0",
		"fills from midnight to 04:00, New York time: 1; tradeDate the fill's instant: 1, the next midnight: 0, the same day's midnight: 0, another time: 0")
	wantEvidence(t, report, "Q11", "TRADE: 7 rows, 7 shapes",
		`  1: "Dividend Reinvest"; legs (COLLECTIVE_INVESTMENT, amount +, cost not 0, OPENING); ids activityId, positionId; the legs' costs sum to netAmount: yes`,
		"DIVIDEND_OR_INTEREST: 3 rows, 2 shapes",
		`  2: "Ordinary Dividend~SYM"; legs (CURRENCY, amount +, cost 0); ids activityId; the legs' costs sum to netAmount: no`,
		"JOURNAL: 1 row, 1 shape",
		w8Mark+`  1: "W-8 Withholding~SYM"; legs (CURRENCY, amount -, cost 0); ids activityId; the legs' costs sum to netAmount: no`)
	wantEvidence(t, report, "extras",
		"a window whose start is its end, at a row's time: 200, 1 row, the row's among them",
		"a window of 365 days and a second: refused, HTTP 400, a guess: Schwab's answer to a window longer than it serves is not recorded",
		"a window without types: 200; against the same window with every type, the same 1 row",
		"a startDate that is a date alone: refused, HTTP 400, '2026-08-31' is not a valid value for startDate",
		"by time or tradeDate, of a month-end interest row: the window around its time holds it, and the window around its tradeDate does not",
		"a row by its id is byte for byte its row in the list",
		"an unknown hash: refused, HTTP 400, Invalid account number",
		"an unknown activityId: refused, HTTP 404, Not Found (a guess: Schwab's answer is not recorded)",
		"a garbage token: 401, application/json, Client not authorized; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"no token: 401, application/json, Client not authorized; correlation headers: Schwab-Client-Correlid, X-Request-Id")
	wantLines(t, report, "## accounts/{account 2}/orders/{id}", "200, application/json; no correlation header",
		"## accounts/{account 2}/orders", "200, application/json; no correlation header", "## orders")
	wantLines(t, report, "## the guard", "budget: 180 requests, token requests included; 97 sent",
		"answers by status: 200: 90, 400: 4, 401: 2, 404: 1", "token requests: 1", "refusals: none", "stopped: no", "did not run: none",
		"## before sharing",
		"W-8: 4 lines, marked [W-8], reveal W-8 withholding rows, which show nonresident tax status; decide on each before sharing the report",
		"the run passed")
	for _, r := range fl.f.Requests() {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Path, "/trader/v1/") && !strings.HasPrefix(r.Path, "/marketdata/v1/") {
			t.Errorf("sent %s %s", r.Method, r.Path)
		}
	}
	if fl.clock.Now().After(fl.r.deadline) {
		t.Errorf("the run ended at %v, after its deadline", fl.clock.Now())
	}
	for _, line := range fl.lines() {
		if !strings.Contains(report, line+"\n") {
			t.Errorf("printed %q, which is not in the report", line)
		}
	}
}

// onTransactions calls f at each arrival of a request for transactions at
// fl's fake, with how many have arrived, this one included.
func onTransactions(fl *fakeLive, f func(n int32)) {
	var n atomic.Int32
	fl.f.OnAPI(func(r *http.Request) {
		if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute {
			f(n.Add(1))
		}
	})
}

// checksTransactions is how many requests for transactions the checks
// send on the fixtures' world: one for each account.
const checksTransactions = 3

// Q1's walk under the fake's settings: windows before a date refused, which
// counts once the newest window answers again; four 429s, one for each of
// the Client's tries, and in a subtest of its own an Invalid account
// number, at the walk's third window, neither of which becomes the
// verdict, that account's walk ending interrupted; and each way the cap can
// meet a request for several types, under which the oldest window that
// held rows is narrowed until the oldest row comes back.
func testDepthOnTheFake(t *testing.T) {
	t.Run("windows before 2022 refused", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1")
		fl.f.SetTransactionRules(schwabtest.TransactionRules{RefuseBefore: time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		refused := "HTTP 400, a guess: never seen: Schwab answers [] for a window before an account's history"
		wantVerdicts(t, report, map[string]string{"Q1": "a window older than an account's history is refused: " + refused +
			"; the oldest rows, by New York date: {account 1} 2023-06-12, {account 2} 2025-01-14"})
		wantEvidence(t, report, "Q1",
			"{account 1}: the oldest row is of 2023-06-12, New York time; an older window was refused, "+refused+
				", and the newest window, asked again at once, answered 200; the window of 2000: refused, "+refused,
			"{account 3}: no row in the windows it read; then a window was refused, "+refused+
				", and the newest window, asked again at once, answered 200; the window of 2000: refused, "+refused)
	})
	t.Run("a second between requests, and an allowance spent", func(t *testing.T) {
		t.Parallel()
		// From 2032, an account without rows walks back past its allowance
		// before it reaches 2000.
		fl := newProbeLive(t, time.Date(2032, 6, 1, 15, 0, 0, 0, time.UTC), "Q1")
		var mu sync.Mutex
		var times []time.Time // when each request for transactions came
		var third int         // and how many were {account 3}'s
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) != schwabtest.TransactionsRoute {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if r.PathValue("hash") == thirdAccount.Hash {
				third++
			}
			times = append(times, fl.clock.Now())
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q1": "a window older than an account's history answers 200 with []" +
			"; the oldest rows, by New York date: {account 1} 2023-06-12, {account 2} 2025-01-14"})
		wantEvidence(t, report, "Q1", "{account 3}: the walk spent its allowance of 30 requests before it ended")
		mu.Lock()
		defer mu.Unlock()
		if third != 1+walkAllowance {
			t.Errorf("{account 3} had %d requests for transactions, want the check's and %d", third, walkAllowance)
		}
		walk := times[checksTransactions:]
		for i := 1; i < len(walk); i++ {
			if gap := walk[i].Sub(walk[i-1]); gap < time.Second {
				t.Errorf("requests %d and %d of the walk came %v apart", i, i+1, gap)
			}
		}
	})
	for _, tc := range []struct {
		name    string
		answers []http.HandlerFunc
		why     string
	}{
		{"a 429 at each of the Client's tries", slices.Repeat([]http.HandlerFunc{schwabtest.TooMany}, 4), "a 429"},
		{"Invalid account number", []http.HandlerFunc{schwabtest.InvalidAccount}, "Invalid account number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1")
			onTransactions(fl, func(n int32) {
				if n == checksTransactions+3 {
					fl.f.Queue(schwabtest.TransactionsRoute, tc.answers...)
				}
			})
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q1": "a window older than an account's history answers 200 with []" +
				"; the oldest rows, by New York date: {account 2} 2025-01-14"})
			wantEvidence(t, report, "Q1", "{account 1}: the walk was interrupted after 3 requests by "+tc.why+", so it settles nothing",
				"{account 2}: the oldest row is of 2025-01-14, New York time; the 3 windows before it answered 200 with []; the window of 2000: 200 with []")
		})
	}
	t.Run("rows after two empty windows", func(t *testing.T) {
		t.Parallel()
		// Rows on {account 3} in the walk's first, fourth and sixth
		// windows: two empty windows do not end it, and the rows after them
		// count the empty windows from naught again.
		fl := newProbeLive(t, probeNow, "Q1")
		for i, day := range []time.Time{time.Date(2026, 5, 1, 15, 0, 0, 0, time.UTC), time.Date(2023, 5, 1, 15, 0, 0, 0, time.UTC), time.Date(2021, 5, 1, 15, 0, 0, 0, time.UTC)} {
			id := strconv.Itoa(84000000501 + i)
			fl.more = append(fl.more, id)
			fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", id, "accountNumber", strconv.Quote(thirdAccount.Number),
				"time", strconv.Quote(schwabTime(day)), "tradeDate", strconv.Quote(schwabTime(day))))
		}
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, fl.report(t), "Q1",
			"{account 3}: the oldest row is of 2021-05-01, New York time; the 3 windows before it answered 200 with []; the window of 2000: 200 with []")
	})
	// Six rows of three types on {account 3}, the oldest a journal, older
	// than the other types' rows, all in one window, at a cap of two.
	day := func(d int) string { return schwabTime(time.Date(2024, 5, d, 15, 0, 0, 0, time.UTC)) }
	var capped []string
	for i, r := range []struct {
		typ string
		day int
	}{{"JOURNAL", 1}, {"DIVIDEND_OR_INTEREST", 2}, {"TRADE", 3}, {"JOURNAL", 4}, {"DIVIDEND_OR_INTEREST", 5}, {"TRADE", 6}} {
		capped = append(capped, withKeys(baseTransaction, "activityId", strconv.Itoa(84000000001+i), "accountNumber", strconv.Quote(thirdAccount.Number),
			"type", strconv.Quote(r.typ), "time", strconv.Quote(day(r.day)), "tradeDate", strconv.Quote(day(r.day))))
	}
	for _, tc := range []struct {
		mode     schwabtest.CapMode
		name     string
		narrowed string
	}{
		{schwabtest.NewestAcrossTypes, "the newest rows across the types", "5 times"},
		{schwabtest.InTypeOrder, "the rows cut in the types' order", "3 times"},
		{schwabtest.PerType, "each type capped on its own", "1 time"},
	} {
		t.Run("the cap as "+tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1")
			fl.r.pageCap = 2
			fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 2, CapMode: tc.mode})
			fl.f.AddRows(thirdAccount.Hash, capped...)
			for i := range capped {
				fl.more = append(fl.more, strconv.Itoa(84000000001+i))
			}
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, fl.report(t), "Q1", "{account 3}: the oldest row is of 2024-05-01, New York time; the 3 windows before it answered 200 with []; "+
				"the oldest window that held rows may have been cut at the cap, so it was narrowed and asked again "+tc.narrowed+"; the window of 2000: 200 with []")
		})
	}
}

// Q4 under the fake's settings: symbol= dropping the rows that name the
// symbol only after a "~", by default, keeping them, and filtering nothing,
// which no verdict may come from. No window around BND's or XYZ's rows
// holds a row of another symbol, so the widening makes each verdict:
// BND's window back to 2024-03-14 takes in the VBR sale, and XYZ's, back
// to 2026-03-12, the META sale.
func testSymbolFilterOnTheFake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rules schwabtest.TransactionRules
		want  string
		word  string // what became of the dividends and the W-8 journal
	}{
		{"dropping the rows named after a ~", schwabtest.TransactionRules{}, "the filter drops dividends; it drops W-8 journals", filterDropped},
		{"keeping them", schwabtest.TransactionRules{SymbolKeepsDividends: true}, "the filter keeps dividends; it keeps W-8 journals", filterKept},
		{
			"filtering nothing",
			schwabtest.TransactionRules{IgnoreSymbol: true},
			"inconclusive: every row of another symbol came back filtered, as it would if Schwab ignored symbol=", "",
		},
		{
			"dropping everything",
			schwabtest.TransactionRules{},
			"inconclusive: the filter left out the symbol's own TRADE, as a filter that drops everything would", "none",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "Q4")
			fl.f.SetTransactionRules(tc.rules)
			if tc.word == "none" {
				// symbol= answered [] whatever it names, as a filter on a
				// form that Schwab does not take would be.
				fl.f.OnAPI(func(r *http.Request) {
					if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.URL.Query().Has("symbol") {
						fl.f.Queue(schwabtest.TransactionsRoute, schwabtest.Answer(http.StatusOK, "[]", "Content-Type", "application/json"))
					}
				})
			}
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q4": tc.want})
			plan := "{account 1}: a symbol with a TRADE and a dividend and a W-8 journal, in 1 window, one widened to take in a row of another symbol"
			switch tc.word {
			case "", "none":
				wantEvidence(t, report, "Q4", w8Mark+plan, "{account 1}: "+tc.want)
			default:
				wantEvidence(t, report, "Q4", w8Mark+plan,
					"{account 1}: filtered, the symbol's TRADE came back, and a row of another symbol did not",
					"{account 1}: the dividend was "+tc.word, w8Mark+"{account 1}: the W-8 journal was "+tc.word,
					"{account 2}: a symbol with a TRADE and a dividend, in 1 window, one widened to take in a row of another symbol",
					"{account 2}: filtered, the symbol's TRADE came back, and a row of another symbol did not",
					"{account 2}: the dividend was "+tc.word)
			}
			// The windows were widened to take in the VBR and the META sales.
			var windows []string
			for _, r := range fl.f.Requests() {
				if strings.Contains(r.Query, "symbol=") {
					windows = append(windows, r.Path[len("/trader/v1/accounts/"):][:4]+" "+r.Query)
				}
			}
			for _, want := range []string{"startDate=2024-03-14T23%3A36%3A04.000Z", "startDate=2026-03-12T13%3A55%3A09.000Z"} {
				if !slices.ContainsFunc(windows, func(w string) bool { return strings.Contains(w, want) }) {
					t.Errorf("no window with symbol= from %s: %q", want, windows)
				}
			}
		})
	}
}

// Q6 counts the reads of the account API refused as unauthorized, here
// one check's.
func testScopeOnTheFake(t *testing.T) {
	fl := newProbeLive(t, probeNow, "Q6")
	fl.f.Queue(schwabtest.AccountRoute, schwabtest.NotAuthorized)
	if err := fl.runClean(t); err == nil || !strings.Contains(err.Error(), "the check of accounts/{account 1} failed with an error") {
		t.Errorf("the run's error: %v", err)
	}
	wantVerdicts(t, fl.report(t), map[string]string{"Q6": `the login's scope is "api"; 1 read of the account API refused as unauthorized`})
}

// Q11 counts a description off Schwab's wording as other text, and never
// prints it.
func testShapesOnTheFake(t *testing.T) {
	fl := newProbeLive(t, probeNow, "Q1", "Q11")
	fl.more = append(fl.more, "ACME", "84000000401")
	fl.f.AddRows(firstAccount.Hash, fixtureRow(t, 73508261951, "activityId", "84000000401", "description", `"Wire Incoming from ACME BANK"`))
	if err := fl.runClean(t); err != nil {
		t.Fatal(err)
	}
	report := fl.report(t)
	wantVerdicts(t, report, map[string]string{"Q11": "14 shapes among 15 rows of 6 types"})
	wantEvidence(t, report, "Q11", "WIRE_IN: 2 rows, 2 shapes",
		"  1: other text; legs (CURRENCY, amount +, cost 0); ids activityId; the legs' costs sum to netAmount: no",
		"rows with a description off Schwab's wording, which is never printed: 1; with -live.record its text is in the raw folder only")
}

// Q5 under the fake's settings: a list of orders kept for 60 days, which
// leaves out the order that its id finds, and an order entered weeks
// before its fill, which a list kept for a year gives back when asked for
// around its entry, not its fill.
func testOrdersDepthOnTheFake(t *testing.T) {
	t.Run("a list kept for 60 days", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		serveOrders(fl, []fakeOrder{metaSaleOrder}, keptFor(orderAgeLimit))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "60 days: two days around an older order's entry leave it out, though its id finds it"})
		wantEvidence(t, report, "Q5", "the order of an execution 172 days old, got by its id, was entered 172 days ago",
			"two days around the order's entry: 200, 0 orders, without the order",
			"the account's orders of the last year: 200, 0 orders, none entered more than 60 days ago",
			"the account's orders of the last 60 days: 200, 0 orders", "every account's orders from 61 days back: 200, 0 orders")
	})
	t.Run("the window around the entry refused", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		around := metaSaleOrder.entered.Add(-oneDay).Format(schwabTimeFormat)
		fl.f.OnAPI(func(r *http.Request) {
			if r.URL.Query().Get("fromEnteredTime") == around {
				fl.f.Script(schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: refused"}`, "Content-Type", "application/json"))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "a year: an order entered more than 60 days ago came back"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: refused, HTTP 400, a guess: refused",
			"the account's orders of the last year: 200, 1 order, some entered more than 60 days ago")
	})
	t.Run("an order entered weeks before its fill", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		gtc := metaSaleOrder
		gtc.entered = gtc.filled.AddDate(0, 0, -52)
		serveOrders(fl, []fakeOrder{gtc}, keptFor(MaxTransactionWindow))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "a year: an order entered more than 60 days ago came back"})
		wantEvidence(t, report, "Q5", "the order of an execution 172 days old, got by its id, was entered 224 days ago",
			"two days around the order's entry: 200, 1 order, the order among them")
		around := "fromEnteredTime=" + strings.ReplaceAll(gtc.entered.Add(-oneDay).Format(schwabTimeFormat), ":", "%3A")
		if !slices.ContainsFunc(fl.f.Requests(), func(r schwabtest.Request) bool { return strings.Contains(r.Query, around) }) {
			t.Errorf("no list asked for from %s", around)
		}
	})
}

// Q8 under the fake's settings: an app without the Market Data product,
// and two runs across an ex-date, four weeks apart, so the second after a
// new login, whose closes before it are as they were, or scaled by the
// dividend's factor.
func testAdjustmentOnTheFake(t *testing.T) {
	t.Run("no Market Data product", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q8", "Q6")
		fl.f.OverridePath("GET /marketdata/v1/quotes", schwabtest.NotAuthorized)
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{
			"Q8": "the app has no Market Data product: Schwab answered 401 Client not authorized",
			"Q6": `the login's scope is "api"; no read of the account API was refused as unauthorized`,
		})
	})
	t.Run("closes kept across a run that could not read them", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q8")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		// A week later, SPY's history is refused and VTI's empty.
		middle := fl.again(t, probeNow.AddDate(0, 0, 8), true)
		fl.f.OverridePath("GET /marketdata/v1/pricehistory", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("symbol") == "SPY" {
				writeJSON(w, http.StatusBadRequest, `{"message":"a guess: no history"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"candles":[],"symbol":"VTI","empty":true}`)
		})
		if err := middle.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, middle.report(t), "Q8", "SPY: the price history failed: HTTP 400, a guess: no history", "VTI: the price history held no closes")
		fakeMarket{exDate: marketExDate, amount: 1.9}.serve(fl)
		later := fl.again(t, time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC), true)
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q8": "daily closes are " + adjustNone})
		wantEvidence(t, report, "Q8", "SPY: across the ex-date 2026-09-18, its closes are "+adjustNone,
			"VTI: across the ex-date 2026-09-18, its closes are "+adjustNone)
	})
	for _, tc := range []struct {
		adjusted bool
		want     string
	}{{false, adjustNone}, {true, adjustDividend}} {
		t.Run("closes "+tc.want, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q8")
			fakeMarket{exDate: marketExDate, amount: 1.9, adjusted: tc.adjusted}.serve(fl)
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantVerdicts(t, fl.report(t), map[string]string{"Q8": "inconclusive: it needs a run before an ex-date and one after; this run's closes are kept for the next"})
			later := fl.again(t, time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC), true)
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q8": "daily closes are " + tc.want})
			wantEvidence(t, report, "Q8", "SPY: across the ex-date 2026-09-18, its closes are "+tc.want, "VTI: across the ex-date 2026-09-18, its closes are "+tc.want)
		})
	}
}

// Q10 under the fake's settings: two runs on one state, under two logins,
// with the same hashes, and with one rehashed between them, as Schwab is
// reported to do on a new login.
func testHashesOnTheFake(t *testing.T) {
	rehashed := schwabtest.HashFor("rehashed")
	for _, tc := range []struct {
		name   string
		rehash bool
		want   string
		first  string
	}{
		{"the same hashes", false, "hashes stayed the same across logins", "the same"},
		{"a hash changed", true, "hashes changed across logins", "changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q10")
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			if tc.rehash {
				fl.f.Rehash(firstAccount.Hash, rehashed)
			}
			later := fl.again(t, probeNow.AddDate(0, 0, 7), true)
			later.more = append(later.more, rehashed)
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q10": tc.want})
			wantEvidence(t, report, "Q10", "{account 1}: under other logins, "+tc.first+"; under other apps, not seen yet",
				"{account 2}: under other logins, the same; under other apps, not seen yet")
		})
	}
}

// Q12 shows no row gone from an answer that may have been cut at the cap.
func testChangesCutOnTheFake(t *testing.T) {
	fl := newProbeLive(t, probeNow, "Q12")
	if err := fl.runClean(t); err != nil {
		t.Fatal(err)
	}
	fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
	later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
	later.r.pageCap = 1
	if err := later.runClean(t); err != nil {
		t.Fatal(err)
	}
	report := later.report(t)
	wantVerdicts(t, report, map[string]string{"Q12": "no row changed, went or came late since the last run"})
	wantEvidence(t, report, "Q12",
		"{account 2}: 1 row as they were; 0 rows new to the state; the answer may have been cut at the cap, so no row can be shown gone")
}

// Q12 under the fake's settings: two runs, 12 days apart, the second after
// a new login, so that their windows slide, with a row changed, a row gone
// and a row that came late between them, and a row that the second run's
// window no longer takes in, which is not shown gone.
func testChangesOnTheFake(t *testing.T) {
	first := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	fl := newProbeLive(t, first, "Q12")
	at := func(month time.Month, day int) string {
		return strconv.Quote(schwabTime(time.Date(2026, month, day, 15, 0, 0, 0, time.UTC)))
	}
	slid := fixtureRow(t, xyzDividend, "activityId", "84000000101", "time", at(6, 25), "tradeDate", at(6, 25))
	fl.more = append(fl.more, "84000000101", "84000000102", "84000000103")
	fl.f.AddRows(secondAccount.Hash, slid)
	if err := fl.runClean(t); err != nil {
		t.Fatal(err)
	}
	wantEvidence(t, fl.report(t), "Q12", "{account 2}: 3 rows, kept for the next run; no earlier run to compare with")
	fl.f.ReplaceRow(secondAccount.Hash, optionClose, fixtureRow(t, optionClose, "netAmount", "311.32"))
	fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
	fl.f.AddRows(secondAccount.Hash, fixtureRow(t, xyzDividend, "activityId", "84000000102", "time", at(8, 10), "tradeDate", at(8, 10)),
		fixtureRow(t, xyzDividend, "activityId", "84000000103", "time", at(8, 25), "tradeDate", at(8, 25)))
	later := fl.again(t, probeNow, true)
	later.more = fl.more
	if err := later.runClean(t); err != nil {
		t.Fatal(err)
	}
	report := later.report(t)
	wantVerdicts(t, report, map[string]string{"Q12": "rows changed after they posted, and went, and came late"})
	// The state keeps the rows of the second run's window alone: the row
	// that slid out of it is pruned, and the row gone is dropped.
	var st rowsState
	if _, err := later.r.state.get("Q12", &st); err != nil {
		t.Fatal(err)
	}
	if rows := st.Accounts[later.r.state.digest("account number", secondAccount.Number)].Rows; len(rows) != 3 {
		t.Errorf("the state keeps %d rows of {account 2}, want 3", len(rows))
	}
	wantEvidence(t, report, "Q12", "{account 1}: 0 rows as they were; 0 rows new to the state",
		"{account 2}: 0 rows as they were; 2 rows new to the state; 1 row changed, 27 days old, in netAmount (1); "+
			"1 row came late, first seen 22 days after its time; 1 row gone")
}

// Q15 under the fake's settings: a refresh that ends the older access
// tokens or not, and changes the refresh token or not, each verdict after
// waiting on the run's clock until the token is a minute old; and another
// process's refresh between the probe's reading of the token file and its
// own refresh, which no verdict may come from.
func testRefreshOnTheFake(t *testing.T) {
	for _, tc := range []struct {
		revoke, rotate bool
		old            int
		want           string
	}{
		{false, false, http.StatusOK, "a refresh leaves older access tokens working, and keeps the refresh token"},
		{true, false, http.StatusUnauthorized, "a refresh ends older access tokens, and keeps the refresh token"},
		{false, true, http.StatusOK, "a refresh leaves older access tokens working, and changes the refresh token"},
		{true, true, http.StatusUnauthorized, "a refresh ends older access tokens, and changes the refresh token"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q15")
			fl.f.SetRevokeOlder(tc.revoke)
			fl.f.SetRotate(tc.rotate)
			before, err := readToken(fl.tokenFile())
			if err != nil {
				t.Fatal(err)
			}
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q15": tc.want})
			kept := map[bool]string{true: "a new one", false: "the one before it"}[tc.rotate]
			wantEvidence(t, report, "Q15",
				fmt.Sprintf("after the refresh, the token before it answered %d, and %d again 30s later; the new token answered 200", tc.old, tc.old),
				"the refresh token after the refresh is "+kept, "no earlier run under this login, to compare its refresh token with")
			if _, refresh := fl.f.Grants(); refresh != 1 {
				t.Errorf("%d refreshes", refresh)
			}
			if i := slices.IndexFunc(fl.clock.got(), func(d time.Duration) bool { return d > 50*time.Second }); i < 0 ||
				fl.clock.got()[i] > refreshAfter+time.Second {
				t.Errorf("the probe did not wait for the token to be a minute old: %v", fl.clock.got())
			}
			after, err := readToken(fl.tokenFile())
			if err != nil || after.AccessToken == before.AccessToken || (after.RefreshToken == before.RefreshToken) == tc.rotate {
				t.Errorf("the token file after the run: %v", err)
			}
			if !slices.Contains(fl.clock.got(), refreshAgain) {
				t.Errorf("no wait of %v before the old token's last try: %v", refreshAgain, fl.clock.got())
			}
			// A run an hour later, under the same login, sees whether the
			// refresh token changed since.
			later := fl.again(t, fl.clock.Now().Add(time.Hour), false)
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			since := map[bool]string{true: "changed", false: "stayed the same"}[tc.rotate]
			wantEvidence(t, later.report(t), "Q15", "since the last run under this login, the refresh token "+since)
		})
	}
	t.Run("a token with under five minutes left", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q15")
		fl.f.SetExpiresIn(360)
		fl.login(t)
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q15": "inconclusive: the access token has under 5m0s left, so it could lapse within the probe; try again in about 7m0s"})
		if _, refresh := fl.f.Grants(); refresh != 0 {
			t.Errorf("%d refreshes", refresh)
		}
	})
	t.Run("a control refused", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q15")
		onAccountNumbers(fl, func(n int32) {
			if n == 2 { // the run's first read, then the probe's control
				fl.f.RevokeAccess()
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q15": "inconclusive: the token before the refresh was refused already, as Schwab is reported to end tokens early"})
	})
	t.Run("another process refreshes in between", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q15")
		var arrivals atomic.Int32
		fl.f.OnAPI(func(r *http.Request) {
			// The run's first read, then the probe's control.
			if schwabtest.Route(r.Pattern) != schwabtest.AccountNumbersRoute || arrivals.Add(1) != 2 {
				return
			}
			other, err := NewAuth(fl.authConfig())
			if err != nil {
				t.Error(err)
				return
			}
			saved, err := readToken(fl.tokenFile())
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := other.Refresh(r.Context(), saved.AccessToken); err != nil {
				t.Error(err)
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q15": "inconclusive: the refresh made 0 token requests, " +
			"where a verdict needs exactly one, as when another process refreshed in between"})
		if _, refresh := fl.f.Grants(); refresh != 1 {
			t.Errorf("%d refreshes, want the other process's one", refresh)
		}
	})
}

// The extras under the fake's settings: a window that leaves out a row at
// its start, or at its end; a row whose time is finer than its JSON shows,
// which settles neither bound; windows that take a row by its tradeDate;
// and a longest window of 366 days.
func testExtrasOnTheFake(t *testing.T) {
	t.Run("the tokens of the 401s", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "extras")
		var mu sync.Mutex
		var sent []string
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) == schwabtest.AccountNumbersRoute {
				mu.Lock()
				defer mu.Unlock()
				auth, ok := r.Header["Authorization"]
				sent = append(sent, fmt.Sprint(ok, auth))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		defer mu.Unlock()
		if len(sent) != 3 || sent[1] != "true [Bearer not-a-token]" || sent[2] != "false []" {
			t.Errorf("the account numbers were sent with %q", sent)
		}
	})
	for _, tc := range []struct {
		name, byID, want string
	}{
		{"spaced otherwise", strings.ReplaceAll(fixtureRow(t, 73508261943), ",", ", "), "a row by its id is its row in the list but for white space"},
		{"another value", fixtureRow(t, 73508261943, "netAmount", "-605.47"), "a row by its id differs from its row in the list"},
	} {
		t.Run("a row by its id "+tc.name+", and the unknown activityId held", func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "extras")
			fl.f.SetRowByID(firstAccount.Hash, 73508261943, tc.byID)
			fl.f.AddRows(firstAccount.Hash, withKeys(baseTransaction, "activityId", strconv.FormatInt(schwabtest.UnknownActivityID, 10),
				"time", strconv.Quote(schwabTime(time.Date(2024, 1, 2, 15, 0, 0, 0, time.UTC)))))
			fl.more = append(fl.more, strconv.FormatInt(schwabtest.UnknownActivityID, 10))
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, fl.report(t), "extras", tc.want, "an unknown activityId: not asked, since a history row has it")
		})
	}
	inclusive := "a window's start is inclusive and its end inclusive"
	for _, tc := range []struct {
		name     string
		rules    schwabtest.TransactionRules
		finer    bool // with a row whose time is finer than it shows
		want     string
		evidence []string
	}{
		{
			"an exclusive start",
			schwabtest.TransactionRules{ExclusiveStart: true},
			false, "a window's start is exclusive and its end inclusive; a window takes a row by its time",
			[]string{
				"a window whose start is its end, at a row's time: 200, 0 rows, without the row",
				"the bounds, row 1: the start is exclusive and the end inclusive", "the bounds, row 2: the start is exclusive and the end inclusive",
			},
		},
		{
			"an exclusive end",
			schwabtest.TransactionRules{ExclusiveEnd: true},
			false, "a window's start is inclusive and its end exclusive; a window takes a row by its time",
			[]string{
				"a window whose start is its end, at a row's time: 200, 0 rows, without the row",
				"the bounds, row 1: the start is inclusive and the end exclusive", "the bounds, row 2: the start is inclusive and the end exclusive",
			},
		},
		{
			"a time finer than it shows",
			schwabtest.TransactionRules{},
			true, inclusive + "; 1 row kept a finer time than it shows; a window takes a row by its time",
			[]string{
				"a window whose start is its end, at a row's time: 200, 0 rows, without the row",
				"the bounds, row 1: [t.001, t+1s] holds it, so Schwab keeps a finer time than it shows, and it settles neither bound",
				"the bounds, row 2: the start is inclusive and the end inclusive",
				"a row by its id is byte for byte its row in the list",
			},
		},
		{
			"windows by tradeDate",
			schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate},
			false, inclusive + "; a window takes a row by its tradeDate",
			[]string{
				"the bounds, row 1: the start is inclusive and the end inclusive",
				"the bounds, row 2: the windows that must hold it do not, so it settles nothing",
				"by time or tradeDate, of a month-end interest row: the window around its tradeDate holds it, and the window around its time does not",
			},
		},
		{
			"a longest window of 366 days",
			schwabtest.TransactionRules{MaxWindow: func(start time.Time) time.Time { return start.Add(366 * oneDay) }},
			false, inclusive + "; a window takes a row by its time",
			[]string{"a window of 365 days and a second: 200, 0 rows", "a window of 366 days: 200, 0 rows"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "extras")
			fl.f.SetTransactionRules(tc.rules)
			if tc.finer {
				shown := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
				fl.f.AddRow(firstAccount.Hash, schwabtest.Row{
					JSON: fixtureRow(t, 73508261943, "activityId", "84000000201", "time", strconv.Quote(schwabTime(shown)), "tradeDate", strconv.Quote(schwabTime(shown))),
					Time: shown.Add(400 * time.Millisecond),
				})
				fl.more = append(fl.more, "84000000201")
			}
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"extras": tc.want})
			wantEvidence(t, report, "extras", tc.evidence...)
		})
	}
}

// newOptInLive returns a run on the fake at at, as newFakeLiveAt does, in
// mode, an opt-in mode, with its probe and without a deadline, as with
// -timeout 0.
func newOptInLive(t *testing.T, at time.Time, mode string) *fakeLive {
	t.Helper()
	fl := newFakeLiveAt(t, at)
	fl.r.mode, fl.r.deadline, fl.r.probes = mode, time.Time{}, probesNamed(t, mode)
	return fl
}

// withBalances returns account, an element of accounts.json, with its
// initial balances' liquidationValue and accountValue at initial, and its
// current balances' liquidationValue at current.
func withBalances(t *testing.T, account []byte, initial, current float64) string {
	t.Helper()
	var v map[string]any
	dec := json.NewDecoder(bytes.NewReader(account))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	sa, _ := v["securitiesAccount"].(map[string]any)
	ib, _ := sa["initialBalances"].(map[string]any)
	cb, _ := sa["currentBalances"].(map[string]any)
	if ib == nil || cb == nil {
		t.Fatal("an account without balances")
	}
	ib["liquidationValue"], ib["accountValue"], cb["liquidationValue"] = initial, initial, current
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The watch of balances on the fake, from 13:00 in New York, which waits
// for 15:45, then samples every 10 minutes to 10:00 the next day: each
// account's initial balances move at 18:00 to the current total as it was
// from 16:00, the close, which moves again at 20:00, in the overnight
// session.
func testBalancesOnTheFake(t *testing.T) {
	ny := newYork()
	at := func(h int) time.Time { return time.Date(2026, 9, 1, h, 0, 0, 0, ny) }
	fl := newOptInLive(t, at(13), modeBalances)
	accounts := elements(t, readTestdata(t, "accounts.json"))
	// The details of each account in each span of the watch: before the
	// close, from it, from the move, and from 20:00.
	var spans [3][4]string
	for i, a := range accounts {
		base := 1000 * float64(i+1)
		for j, b := range [4][2]float64{{base, base + 1}, {base, base + 2}, {base + 2, base + 2}, {base + 2, base + 3}} {
			spans[i][j] = withBalances(t, a, b[0], b[1])
		}
	}
	fl.f.OnAPI(func(r *http.Request) {
		if schwabtest.Route(r.Pattern) != schwabtest.AccountsRoute {
			return
		}
		now := fl.clock.Now()
		span := 0
		for _, from := range []time.Time{at(16), at(18), at(20)} {
			if !now.Before(from) {
				span++
			}
		}
		for i, a := range schwabtest.FixtureAccounts {
			fl.f.SetDetails(a.Hash, spans[i][span])
		}
	})
	if err := fl.runClean(t); err != nil {
		t.Fatal(err)
	}
	report := fl.report(t)
	wantVerdicts(t, report, map[string]string{"Q3": "the initial balances move between 17:55 and 18:05 New York time, " +
		"and then equal the current total sampled from 16:00 to 16:15: yes; sampled at 20:00: no"})
	moved := "the initial balances moved between 17:55 and 18:05 New York time; they then equal the current total sampled from 16:00 to 16:15: yes; sampled at 20:00: no"
	wantEvidence(t, report, "Q3", "110 samples read, 0 samples failed, every 10m0s from 15:45 New York time",
		"{account 1}: "+moved, "{account 2}: "+moved, "{account 3}: "+moved)
	if n := fl.f.APICalls(); n != 2+110 {
		t.Errorf("%d requests, want the account numbers, the preferences and 110 samples", n)
	}
}

// The watch of fills on the fake: an order entered four minutes into the
// watch and filled a minute later, whose row comes two and a half minutes
// after the fill, so between the polls 2 and 3 minutes after it; and a
// watch in which nothing fills.
func testFillsOnTheFake(t *testing.T) {
	t.Run("a fill", func(t *testing.T) {
		t.Parallel()
		fl := newOptInLive(t, probeNow, modeFills)
		filled := probeNow.Add(5 * time.Minute)
		const id = 1004517700001
		serveOrders(fl, []fakeOrder{{account: secondAccount, id: id, entered: filled.Add(-time.Minute), filled: filled}}, keptFor(MaxTransactionWindow))
		fl.more = append(fl.more, strconv.Itoa(id), "84000000301")
		row := fixtureRow(t, metaSale, "activityId", "84000000301", "orderId", strconv.Itoa(id),
			"time", strconv.Quote(schwabTime(filled)), "tradeDate", strconv.Quote(schwabTime(filled)))
		var added atomic.Bool
		onTransactions(fl, func(int32) {
			if !fl.clock.Now().Before(filled.Add(150*time.Second)) && added.CompareAndSwap(false, true) {
				fl.f.AddRows(secondAccount.Hash, row)
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day",
			"rows that came during the watch: 1, from 2 to 3 minutes after the fills")
		if n := fl.f.APICalls(); n != 2+30*2*3 {
			t.Errorf("%d requests, want the account numbers, the preferences and 30 polls of two for each account", n)
		}
	})
	t.Run("no fill", func(t *testing.T) {
		t.Parallel()
		fl := newOptInLive(t, probeNow, modeFills)
		serveOrders(fl, nil, keptFor(MaxTransactionWindow))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q13": "inconclusive: no order of the last day was filled; place a trade by hand, then run the watch"})
	})
}

// otherProcess returns a limiter of fl's rate file of its own, as another
// process using the app key has, which writes the file on a clock that is
// fl's moved on by skew.
func otherProcess(fl *fakeLive, skew time.Duration) func() error {
	l := &limiter{path: fl.r.client.limit.path, sem: make(chan struct{}, 1), lock: filelock.Acquire, tryLock: filelock.TryAcquire, wait: rateLockWait}
	return func() error {
		return l.change(context.Background(), func() time.Time { return fl.clock.Now().Add(skew) }, func(string) {},
			func(s rateState, _ time.Time) (rateState, bool) { return s, true })
	}
}

// onAccountNumbers calls f at each arrival of a request for the account
// numbers at fl's fake, with how many have arrived, this one included.
func onAccountNumbers(fl *fakeLive, f func(n int32)) {
	var n atomic.Int32
	fl.f.OnAPI(func(r *http.Request) {
		if schwabtest.Route(r.Pattern) == schwabtest.AccountNumbersRoute {
			f(n.Add(1))
		}
	})
}

// The rate probe on the fake: its steps and its burst without a 429; a 429
// at 60 a minute; another process's use of the key in the middle of a
// step, which stops it; and the refusals to start, of a rate file written
// a minute ago and of a scheduled snapshot that the run would come near,
// and the stop when the snapshot nears in the middle of the run, as when
// the computer slept.
func testRateOnTheFake(t *testing.T) {
	snapshotIn := func(fl *fakeLive, d time.Duration) string {
		local := fl.clock.Now().Add(d).In(fl.r.loc)
		fl.r.snapshot = time.Duration(local.Hour())*time.Hour + time.Duration(local.Minute())*time.Minute
		return local.Format("15:04")
	}
	for _, tc := range []struct {
		name    string
		setUp   func(t *testing.T, fl *fakeLive) string // returns the snapshot's time, if it set one
		want    string
		refused string // why the run did not start, if it did not
	}{
		{"no 429", nil, "no 429 came up to 100 a minute, nor at a burst of 5", ""},
		{"a 429 at 60 a minute", func(_ *testing.T, fl *fakeLive) string {
			onAccountNumbers(fl, func(n int32) {
				if n == 1+20+40+34 {
					fl.f.Queue(schwabtest.AccountNumbersRoute, schwabtest.TooMany)
				}
			})
			return ""
		}, "the first 429 came at 60 a minute, at request 34 of 60, 33s after the first", ""},
		{"another process uses the key", func(t *testing.T, fl *fakeLive) string {
			write := otherProcess(fl, time.Millisecond)
			onAccountNumbers(fl, func(n int32) {
				if n == 1+20+10 {
					if err := write(); err != nil {
						t.Error(err)
					}
				}
			})
			return ""
		}, "inconclusive: another process used the app key, at 40 a minute; no 429 came up to 20 a minute", ""},
		{"another process uses the key while the probe waits", func(t *testing.T, fl *fakeLive) string {
			write := otherProcess(fl, time.Millisecond)
			var armed, done atomic.Bool
			onAccountNumbers(fl, func(n int32) { armed.Store(n == 1+20+10) })
			fl.clock.onSleep = func(time.Duration) {
				if armed.Load() && done.CompareAndSwap(false, true) {
					if err := write(); err != nil {
						t.Error(err)
					}
				}
			}
			return ""
		}, "inconclusive: another process used the app key, at 40 a minute; no 429 came up to 20 a minute", ""},
		{"a 503 at 40 a minute", func(_ *testing.T, fl *fakeLive) string {
			onAccountNumbers(fl, func(n int32) {
				if n == 1+20+7 {
					fl.f.Queue(schwabtest.AccountNumbersRoute, schwabtest.Unavailable)
				}
			})
			return ""
		}, "inconclusive: a 503 came, at request 7 of 40, 9s after the first, at 40 a minute; no 429 came up to 20 a minute", ""},
		{"the rate file written a minute ago", func(t *testing.T, fl *fakeLive) string {
			if err := otherProcess(fl, -time.Minute)(); err != nil {
				t.Fatal(err)
			}
			return ""
		}, "", "a process used the app key less than 2m0s ago; run it when nothing has used the key for 2m0s"},
		{"the snapshot near the start", func(_ *testing.T, fl *fakeLive) string {
			return snapshotIn(fl, 20*time.Minute)
		}, "", "its 10m0s would come within 30m0s of the scheduled snapshot at %s; run it at another time, or give the snapshot's time with -live.snapshot"},
		{"the snapshot near the plan's end", func(_ *testing.T, fl *fakeLive) string {
			return snapshotIn(fl, 35*time.Minute)
		}, "", "its 10m0s would come within 30m0s of the scheduled snapshot at %s; run it at another time, or give the snapshot's time with -live.snapshot"},
		{"the snapshot near after a sleep", func(_ *testing.T, fl *fakeLive) string {
			onAccountNumbers(fl, func(n int32) {
				if n == 1+20+5 {
					fl.clock.set(fl.clock.Now().Add(15 * time.Minute))
				}
			})
			return snapshotIn(fl, 42*time.Minute)
		}, "inconclusive: the scheduled snapshot at %s is near, at 40 a minute; no 429 came up to 20 a minute", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newOptInLive(t, probeNow, modeRate)
			snapshot := ""
			if tc.setUp != nil {
				snapshot = tc.setUp(t, fl)
			}
			err := fl.runClean(t)
			report := fl.report(t)
			if tc.refused != "" {
				want := "it stopped before its end: probe Q14 cannot start: " + strings.ReplaceAll(tc.refused, "%s", snapshot)
				if err == nil || !strings.Contains(err.Error(), want) || fl.f.APICalls() != 0 {
					t.Errorf("the run's error: %v, after %d requests; want %q", err, fl.f.APICalls(), want)
				}
				wantLines(t, report, "did not run: everything")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantVerdicts(t, report, map[string]string{"Q14": strings.ReplaceAll(tc.want, "%s", snapshot)})
			if tc.setUp == nil {
				wantEvidence(t, report, "Q14", "20 a minute: 20 requests in 57s, no 429", "40 a minute: 40 requests in 59s, no 429",
					"60 a minute: 60 requests in 59s, no 429", "80 a minute: 80 requests in 59s, no 429", "100 a minute: 100 requests in 59s, no 429",
					"the burst of 5 after 2m0s of quiet: 5 requests in 0s, no 429")
				if n := fl.f.APICalls(); n != 2+300+5 {
					t.Errorf("%d requests, want the account numbers, the preferences and 305", n)
				}
			}
		})
	}
}
