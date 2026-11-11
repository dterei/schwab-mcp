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
	"regexp"
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

// stateDigest matches a digest as state.json holds one: a JSON string of
// 32 hex digits.
var stateDigest = regexp.MustCompile(`"[0-9a-f]{32}"`)

// runClean runs the harness, and fails the test if the report, the lines
// printed, the run's error or the state that the run leaves hold any of
// fl's secrets. The state keeps each identifier, token and value as a
// digest, which the check blanks: an account number's 8 digits could turn
// up among a digest's 32 by chance.
func (fl *fakeLive) runClean(t *testing.T) error {
	t.Helper()
	err := fl.run(t)
	if b, readErr := os.ReadFile(filepath.Join(fl.r.out, "report.txt")); readErr == nil {
		fl.noSecrets(t, "the report", string(b))
	}
	if b, readErr := os.ReadFile(fl.r.stateFile); readErr == nil {
		fl.noSecrets(t, "the state", stateDigest.ReplaceAllString(string(b), `"digest"`))
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
// until then, or for good while filled is zero, with an execution at each
// of earlier before that, as of an order filled in parts.
type fakeOrder struct {
	account         schwabtest.FixtureAccount
	id              int64
	entered, filled time.Time
	earlier         []time.Time
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
	var executions []string
	for _, at := range append(slices.Clone(o.earlier), o.filled) {
		if !at.IsZero() && !now.Before(at) {
			executions = append(executions, `{"activityType":"EXECUTION","executionType":"FILL",`+
				`"quantity":1.0,"orderRemainingQuantity":0.0,"executionLegs":[{"legId":1,"price":603.4,"quantity":1.0,"mismarkedQuantity":0.0,`+
				`"instrumentId":1873219,"time":"`+schwabTime(at)+`"}]}`)
		}
	}
	if len(executions) > 0 {
		filled = `,"orderActivityCollection":[` + strings.Join(executions, ",") + `]`
	}
	if !o.filled.IsZero() && !now.Before(o.filled) {
		status = "FILLED"
		filled = `,"closeTime":"` + schwabTime(o.filled) + `"` + filled
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
	serveOrdersOf(fl, func() []fakeOrder { return orders }, kept)
}

// serveOrdersOf is serveOrders of the orders that orders returns as each
// request arrives.
func serveOrdersOf(fl *fakeLive, orders func() []fakeOrder, kept func(entered, now time.Time) bool) {
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
		for _, o := range orders() {
			if of(o) && !o.entered.Before(from) && !o.entered.After(to) && kept(o.entered, now) {
				es = append(es, o.json(now))
			}
		}
		writeJSON(w, http.StatusOK, "["+strings.Join(es, ",")+"]")
	}
	fl.f.OverridePath("GET /trader/v1/accounts/{hash}/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, o := range orders() {
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
// scaled by the dividend's factor, 1 - amount over the close before it:
// only, when it is set, those of the one symbol only.
type fakeMarket struct {
	exDate   time.Time
	amount   float64
	adjusted bool
	only     string
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
	if m.adjusted && (m.only == "" || m.only == sym) && !now.Before(m.exDate) {
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
// line printed, error or state, past its digests, holds an identifier, a
// token, a symbol, a security's description or an amount of the fixtures, a
// path of the run's or the planted user name; that every request waits its
// turn in the rate limit; that nothing is written under the owner's cache;
// and, on a guard apart from the run's, that a POST is refused. One fake
// clock serves every wait, so the runs take seconds.
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
		{"the default run at 6 requests a minute", testPacingOnTheFake},
		{"a guard apart from the run refuses a POST", func(t *testing.T) { guardRefusesPosts(t, newFakeLive(t)) }},
		{"refusals that echo secrets", testEchoedSecretsOnTheFake},
		{"failures of the network", testNetworkFailuresOnTheFake},
		{"Q1", testDepthOnTheFake},
		{"Q4", testSymbolFilterOnTheFake},
		{"Q5", testOrdersDepthOnTheFake},
		{"Q6", testScopeOnTheFake},
		{"Q8", testAdjustmentOnTheFake},
		{"Q10", testHashesOnTheFake},
		{"Q11", testShapesOnTheFake},
		{"Q12", testChangesOnTheFake},
		{"Q12 with a cut answer", testChangesCutOnTheFake},
		{"Q12 with windows by tradeDate", testChangesByTradeDateOnTheFake},
		{"Q12's verdicts on what a row does", testChangesVerdictsOnTheFake},
		{"Q1 and Q12 without a row", testNoRowsOnTheFake},
		{"Q15", testRefreshOnTheFake},
		{"extras", testExtrasOnTheFake},
		{"an edge page", testEdgeOnTheFake},
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
		"Q5":     "longer than 60 days: an order entered 172 days ago came back",
		"Q11":    "13 shapes among 14 rows of 6 types",
		"Q12":    "inconclusive: no earlier run to compare with; this run's rows are kept for the next",
		"extras": "a window's start is inclusive and its end inclusive; a window takes a row by its time",
		"Q8":     adjustNeedsTwo,
		"Q15":    "a refresh leaves older access tokens working for 30s at least, and keeps the refresh token",
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

// Every request of the default run, the extras' bare ones and Q15's with a
// token of their own among them, waits its turn in the rate limit that
// every process using the app key shares: at 6 a minute, one at a time, the
// requests reach the fake at least 10 seconds apart on the run's clock.
func testPacingOnTheFake(t *testing.T) {
	fl := newProbeLive(t, probeNow)
	same := func(d time.Duration) time.Duration { return d }
	c := NewClient(fl.r.auth, ClientConfig{RateFile: fl.r.rateFile, RatePerMinute: 6, Burst: 1})
	c.now, c.sleep, c.jitter = fl.clock.Now, fl.clock.Sleep, same
	fl.r.client = c
	fl.r.deadline = time.Time{} // the run takes some 16 minutes at this rate
	var mu sync.Mutex
	var arrived []time.Time
	fl.f.OnAPI(func(*http.Request) {
		mu.Lock()
		defer mu.Unlock()
		arrived = append(arrived, fl.clock.Now())
	})
	if err := fl.runClean(t); err != nil {
		t.Fatal(err)
	}
	report := fl.report(t)
	wantEvidence(t, report, "extras",
		"a garbage token: 401, application/json, Client not authorized; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"no token: 401, application/json, Client not authorized; correlation headers: Schwab-Client-Correlid, X-Request-Id")
	wantVerdicts(t, report, map[string]string{"Q15": "a refresh leaves older access tokens working for 30s at least, and keeps the refresh token"})
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(arrived); i++ {
		if gap := arrived[i].Sub(arrived[i-1]); gap < 10*time.Second {
			t.Errorf("requests %d and %d of %d came %v apart", i, i+1, len(arrived), gap)
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
// counts once the newest window answers again; a second between requests,
// and an allowance spent before the walk reaches 2000, or in reaching it,
// which leaves no request for the control; four 429s, one for each of the
// Client's tries, and in a subtest of its own an Invalid account number, at
// the walk's third window, neither of which becomes the verdict, that
// account's walk ending interrupted; each way the cap can meet a request
// for several types, under which the oldest window that held rows, and no
// other, is narrowed until the oldest row comes back; windows by tradeDate,
// under which that window cannot be narrowed; rows of one time over the
// cap, which no narrower window leaves out; windows that leave out a row at
// their end, under which a narrowed window that comes back empty ends the
// narrowing, and rows over the cap at a window's start leave no narrower
// window to ask; and an allowance spent narrowing, whose oldest row the
// verdict gives as a date or earlier.
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
	t.Run("an allowance spent reaching 2000", func(t *testing.T) {
		t.Parallel()
		// From mid-2029, an account without rows reaches 2000 with the last
		// request of its allowance, which leaves none for the control.
		fl := newProbeLive(t, time.Date(2029, 6, 1, 15, 0, 0, 0, time.UTC), "Q1")
		var third atomic.Int32 // {account 3}'s requests for transactions
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.PathValue("hash") == thirdAccount.Hash {
				third.Add(1)
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, fl.report(t), "Q1", "{account 3}: no row back to 2000-01-01, where the walk stops; every window answered 200 with []")
		if n := third.Load(); n != 1+walkAllowance {
			t.Errorf("{account 3} had %d requests for transactions, want the check's and %d", n, walkAllowance)
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
	// At the cap of two, {account 1}'s and {account 2}'s oldest windows that
	// held rows hold two each, and each is narrowed once; {account 2}'s
	// newest window, of three rows, is cut too, but not narrowed.
	cut := func(n int, date, narrowed string) string {
		return fmt.Sprintf("{account %d}: the oldest row is of %s, New York time; the 3 windows before it answered 200 with []; "+
			"the oldest window that held rows may have been cut at the cap, so it was narrowed and asked again %s; the window of 2000: 200 with []", n, date, narrowed)
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
			wantEvidence(t, fl.report(t), "Q1", cut(1, "2023-06-12", "1 time"), cut(2, "2025-01-14", "1 time"), cut(3, "2024-05-01", tc.narrowed))
		})
	}
	t.Run("the cap with windows by tradeDate", func(t *testing.T) {
		t.Parallel()
		// Two rows on {account 3} in the walk's second window by their
		// tradeDate, the older of them from before the window's start by its
		// time, so that the window cannot be narrowed to end at it.
		fl := newProbeLive(t, probeNow, "Q1")
		fl.r.pageCap = 2
		fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 2, FilterOn: schwabtest.FilterOnTradeDate})
		for i, r := range []struct{ time, trade string }{
			{"2024-09-01T12:00:00+0000", "2024-09-02T04:00:00+0000"},
			{"2025-01-10T15:00:00+0000", "2025-01-10T05:00:00+0000"},
		} {
			id := strconv.Itoa(84000000701 + i)
			fl.more = append(fl.more, id)
			fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", id, "accountNumber", strconv.Quote(thirdAccount.Number),
				"time", strconv.Quote(r.time), "tradeDate", strconv.Quote(r.trade)))
		}
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q1": "a window older than an account's history answers 200 with []" +
			"; the oldest rows, by New York date: {account 1} 2023-06-12, {account 2} 2025-01-14"})
		wantEvidence(t, report, "Q1", "{account 3}: the walk was interrupted after 5 requests by an oldest window that held as many rows as the cap, "+
			"the oldest of them at or before its start, so it settles nothing")
	})
	t.Run("the cap with rows of one time", func(t *testing.T) {
		t.Parallel()
		// Three rows on {account 3} at one time, at a cap of two: no
		// narrower window leaves one out, so the walk stops asking.
		fl := newProbeLive(t, probeNow, "Q1")
		fl.r.pageCap = 2
		fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 2})
		at := strconv.Quote(schwabTime(time.Date(2024, 5, 1, 15, 0, 0, 0, time.UTC)))
		for i := range 3 {
			id := strconv.Itoa(84000000711 + i)
			fl.more = append(fl.more, id)
			fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", id, "accountNumber", strconv.Quote(thirdAccount.Number), "time", at, "tradeDate", at))
		}
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, fl.report(t), "Q1", "{account 3}: the walk was interrupted after 7 requests by an oldest window that held as many rows as the cap, "+
			"none older than another, so it settles nothing; the oldest window that held rows may have been cut at the cap, so it was narrowed and asked again 1 time")
	})
	t.Run("the cap with an exclusive end", func(t *testing.T) {
		t.Parallel()
		// At a cap of two, with windows that leave out a row at their end:
		// {account 1}'s and {account 2}'s oldest windows are narrowed until
		// one comes back empty, which ends the narrowing; and two rows on
		// {account 3} at the start of its second window, which no older
		// window holds, leave no narrower window to ask. A rate of 60 a
		// minute keeps the run's clock on whole seconds, so that a row's
		// time, which shows whole seconds, can be a window's start.
		fl := newProbeLive(t, probeNow, "Q1")
		fl.r.pageCap = 2
		fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 2, ExclusiveEnd: true})
		c := NewClient(fl.r.auth, ClientConfig{RateFile: fl.r.rateFile, RatePerMinute: 60})
		c.now, c.sleep, c.jitter = fl.clock.Now, fl.clock.Sleep, func(d time.Duration) time.Duration { return d }
		fl.r.client = c
		var n atomic.Int32
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) != schwabtest.TransactionsRoute || r.PathValue("hash") != thirdAccount.Hash || n.Add(1) != 3 {
				return
			}
			// The walk's second window, after the check's and the walk's first.
			start, err := time.Parse(schwabTimeFormat, r.URL.Query().Get("startDate"))
			if err != nil || start.Nanosecond() != 0 {
				t.Errorf("the second window starts at %q", r.URL.Query().Get("startDate"))
				return
			}
			at := strconv.Quote(schwabTime(start))
			for i := range 2 {
				id := strconv.Itoa(84000000721 + i)
				fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", id, "accountNumber", strconv.Quote(thirdAccount.Number), "time", at, "tradeDate", at))
			}
		})
		fl.more = append(fl.more, "84000000721", "84000000722")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q1": "a window older than an account's history answers 200 with []" +
			"; the oldest rows, by New York date: {account 1} 2023-06-12, {account 2} 2025-01-14"})
		wantEvidence(t, report, "Q1", cut(1, "2023-06-12", "1 time"), cut(2, "2025-01-14", "1 time"),
			"{account 3}: the walk was interrupted after 5 requests by an oldest window that held as many rows as the cap, "+
				"the oldest of them at or before its start, so it settles nothing")
	})
	t.Run("an allowance spent narrowing", func(t *testing.T) {
		t.Parallel()
		// Sixty daily rows on {account 3} at a cap of two: narrowing two
		// days at a time, the walk spends its allowance first, so that the
		// oldest row it saw may not be the oldest.
		fl := newProbeLive(t, probeNow, "Q1")
		fl.r.pageCap = 2
		fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 2})
		for i := range 60 {
			at := schwabTime(time.Date(2024, 5, 1, 15, 0, 0, 0, time.UTC).AddDate(0, 0, i))
			id := strconv.Itoa(84000000801 + i)
			fl.more = append(fl.more, id)
			fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", id, "accountNumber", strconv.Quote(thirdAccount.Number),
				"time", strconv.Quote(at), "tradeDate", strconv.Quote(at)))
		}
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q1": "a window older than an account's history answers 200 with []" +
			"; the oldest rows, by New York date: {account 1} 2023-06-12, {account 2} 2025-01-14, {account 3} 2024-06-04 or earlier"})
		wantEvidence(t, report, "Q1", "{account 3}: the walk spent its allowance of 30 requests before it ended; "+
			"the oldest row it saw is of 2024-06-04, New York time, and older ones may be left; "+
			"the oldest window that held rows may have been cut at the cap, so it was narrowed and asked again 24 times")
	})
}

// Q4 under the fake's settings: symbol= dropping the rows that name the
// symbol only after a "~", by default, keeping them, and filtering nothing,
// which no verdict may come from; a control answer without the symbol's
// TRADE, which no verdict of its account's may come from either, and one
// without the dividend and the W-8 journal, of which nothing is said. No
// window around BND's or XYZ's rows holds a row of another symbol, so the
// widening makes each verdict: BND's window back to 2024-03-14 takes in the
// VBR sale, and XYZ's, back to 2026-03-12, the META sale. A refusal of
// symbol= that quotes the symbol, in its message or in a code, shows a
// label in its place, and a symbol that the scrubber does not hide, a
// benchmark's, is never sent.
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
	t.Run("a control answer without the symbol's TRADE", func(t *testing.T) {
		t.Parallel()
		// {account 1}'s window without symbol= holds the VBR sale alone, as
		// a cap could leave it, while the window with symbol= holds BND's
		// TRADE: the filter's answer is not held against a control.
		fl := newProbeLive(t, probeNow, "Q1", "Q4")
		control := "[" + fixtureRow(t, 73508261947) + "]"
		fl.f.OnAPI(func(r *http.Request) {
			q := r.URL.Query()
			if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.PathValue("hash") == firstAccount.Hash &&
				!q.Has("symbol") && strings.HasPrefix(q.Get("startDate"), "2024-03-14") {
				fl.f.Script(schwabtest.Answer(http.StatusOK, control, "Content-Type", "application/json"))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q4": "the filter drops dividends"})
		wantEvidence(t, report, "Q4", "{account 1}: inconclusive: no control answer held the symbol's TRADE",
			"{account 2}: the dividend was "+filterDropped)
	})
	t.Run("a control answer without the dividend and the W-8 journal", func(t *testing.T) {
		t.Parallel()
		// {account 1}'s window without symbol= holds BND's TRADE and the VBR
		// sale alone, as a cap could leave it: what became of the dividend
		// and the W-8 journal, which no control answer held, is not said.
		fl := newProbeLive(t, probeNow, "Q1", "Q4")
		control := "[" + fixtureRow(t, 73508261946) + "," + fixtureRow(t, 73508261947) + "]"
		fl.f.OnAPI(func(r *http.Request) {
			q := r.URL.Query()
			if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.PathValue("hash") == firstAccount.Hash &&
				!q.Has("symbol") && strings.HasPrefix(q.Get("startDate"), "2024-03-14") {
				fl.f.Script(schwabtest.Answer(http.StatusOK, control, "Content-Type", "application/json"))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q4": "the filter drops dividends"})
		wantEvidence(t, report, "Q4", "{account 1}: filtered, the symbol's TRADE came back, and a row of another symbol did not",
			"{account 2}: the dividend was "+filterDropped)
		if strings.Contains(report, "{account 1}: the dividend was") || strings.Contains(report, "{account 1}: the W-8 journal was") {
			t.Errorf("the report says what became of rows that no control held:\n%s", sectionOf(report, "probe Q4"))
		}
	})
	// A refusal of symbol= that quotes the symbol, in each place of an
	// error that holds Schwab's own words: its message, its OAuth code, the
	// code nested in that code's description, and its fault's code.
	for _, tc := range []struct {
		name, body string
		says       string // what the verdict gives of it, after its status
		failed     string // what the failure's line gives, after its status
	}{
		{
			"in a message", `{"message":"'%s' is not a valid value for symbol"}`,
			`, '\{symbol \d+\}' is not a valid value for symbol`, `'\{symbol \d+\}' is not a valid value for symbol`,
		},
		{"in an OAuth error's code", `{"error":"no rows of %s here"}`, ``, `no rows of \{symbol \d+\} here`},
		{
			"in the code of an OAuth error's description", `{"error":"invalid_request","error_description":"{\"error\":\"no rows of %s\"}"}`,
			``, `invalid_request \(no rows of \{symbol \d+\}\)`,
		},
		{
			"in a fault's code", `{"fault":{"faultstring":"refused","detail":{"errorcode":"symbol.%s.refused"}}}`,
			`, refused`, `refused \(symbol\.\{symbol \d+\}\.refused\)`,
		},
	} {
		t.Run("a refusal that quotes the symbol "+tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "Q4")
			fl.f.OnAPI(func(r *http.Request) {
				if sym := r.URL.Query().Get("symbol"); sym != "" {
					fl.f.Script(schwabtest.Answer(http.StatusBadRequest, fmt.Sprintf(tc.body, sym), "Content-Type", "application/json"))
				}
			})
			if err := fl.runClean(t); err != nil { // BND and XYZ are among the fixtures' values
				t.Fatal(err)
			}
			report := fl.report(t)
			if v := verdictOf(report, "Q4"); !regexp.MustCompile(`^inconclusive: a request with symbol= failed: HTTP 400` + tc.says + `$`).MatchString(v) {
				t.Errorf("Q4's verdict: %s", v)
			}
			if !regexp.MustCompile(`\nfailed: HTTP 400 from Schwab: ` + tc.failed + `\n`).MatchString(report) {
				t.Errorf("the report:\n%s", report)
			}
		})
	}
	t.Run("a benchmark's symbol", func(t *testing.T) {
		t.Parallel()
		// {account 3} with a trade of VTI, a dividend of it and a wire,
		// which would make a plan, but for the scrubber, which does not hide
		// a symbol that the report names.
		fl := newProbeLive(t, probeNow, "Q1", "Q4")
		at := func(day int) string { return strconv.Quote(schwabTime(time.Date(2025, 5, day, 15, 0, 0, 0, time.UTC))) }
		number := strconv.Quote(thirdAccount.Number)
		fl.f.AddRows(thirdAccount.Hash,
			strings.ReplaceAll(fixtureRow(t, 73508261946, "activityId", "84000000901", "accountNumber", number, "time", at(1), "tradeDate", at(1)), `"BND"`, `"VTI"`),
			fixtureRow(t, 73508261950, "activityId", "84000000902", "accountNumber", number, "time", at(2), "tradeDate", at(2), "description", `"Ordinary Dividend~VTI"`),
			fixtureRow(t, 73508261951, "activityId", "84000000903", "accountNumber", number, "time", at(3), "tradeDate", at(3)))
		fl.more = append(fl.more, "84000000901", "84000000902", "84000000903")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q4": "the filter drops dividends; it drops W-8 journals"})
		wantEvidence(t, report, "Q4", "{account 3}: no symbol of the history rows that the report's scrubber hides has both a TRADE and a dividend or a W-8 journal")
		if slices.ContainsFunc(fl.f.Requests(), func(r schwabtest.Request) bool { return strings.Contains(r.Query, "symbol=VTI") }) {
			t.Error("a request with symbol=VTI was sent")
		}
	})
}

// Q6 counts a read of the checks that Schwab refuses as unauthorized, with
// a 401 or a 403, but not the reads that Schwab may refuse for reasons of
// its own, those whose answers the probes ask about and Q12's windows,
// which end days ahead: here with a 403, the windows of Q1's walk before
// 2020, Q5's every account's orders from 61 days back, Q12's windows, and
// the extras' windows of more than a year, their window without types,
// their date alone, their row by its id, and their unknown hash and
// unknown activityId.
func testScopeOnTheFake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{"401", schwabtest.NotAuthorized},
		{"403", schwabtest.Answer(http.StatusForbidden, `{"message":"a guess: not for this login"}`, "Content-Type", "application/json")},
	} {
		t.Run("a check refused with a "+tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q6")
			fl.f.Queue(schwabtest.AccountRoute, tc.answer)
			if err := fl.runClean(t); err == nil || !strings.Contains(err.Error(), "the check of accounts/{account 1} failed with an error") {
				t.Errorf("the run's error: %v", err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q6": `the login's scope is "api"; 1 read of the account API refused as unauthorized`})
			wantEvidence(t, report, "Q6", "reads counted: 14, those of the account API that a working login passes: the account numbers, "+
				"the preferences, the checks, and Q4's windows; not those that Schwab may refuse for reasons of its own: Q1's walk, "+
				"Q5's old orders and the extras', whose answers the probes ask about, and Q12's windows, which end days ahead")
		})
	}
	t.Run("the probes' questions refused", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5", "Q12", "extras", "Q6")
		refused, ahead, windows := 0, 0, 0 // ahead: Q12's windows refused
		var mu sync.Mutex
		fl.f.OnAPI(func(r *http.Request) {
			route := schwabtest.Route(r.Pattern)
			q := r.URL.Query()
			start, err1 := time.Parse(schwabTimeFormat, q.Get("startDate"))
			end, err2 := time.Parse(schwabTimeFormat, q.Get("endDate"))
			mu.Lock()
			defer mu.Unlock()
			if route == schwabtest.TransactionsRoute {
				windows++
			}
			// Q12's windows alone end over a day after the run. A row asked
			// for by its id once the checks' windows have gone is the
			// extras': the row by its id, or the unknown activityId.
			switch {
			case route == schwabtest.TransactionsRoute && err2 == nil && end.After(fl.clock.Now().Add(oneDay)):
				ahead++
			case route == schwabtest.AccountRoute && r.PathValue("hash") == schwabtest.UnknownHash,
				route == schwabtest.TransactionRoute && windows > checksTransactions,
				route == schwabtest.TransactionsRoute && (q.Get("startDate") < "2020" || !q.Has("types") || err1 != nil ||
					err2 == nil && end.Sub(start) > MaxTransactionWindow),
				r.URL.Path == "/trader/v1/orders":
				refused++
			default:
				return
			}
			fl.f.Script(schwabtest.Answer(http.StatusForbidden, `{"message":"a guess: not for this login"}`, "Content-Type", "application/json"))
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{
			"Q6":  `the login's scope is "api"; no read of the account API was refused as unauthorized`,
			"Q12": "inconclusive: no account's window was read",
		})
		forbidden := "refused, HTTP 403, a guess: not for this login"
		wantEvidence(t, report, "extras", "a window of 365 days and a second: "+forbidden, "a window of 366 days: "+forbidden,
			"a window without types: "+forbidden, "a startDate that is a date alone: "+forbidden, "a row by its id: "+forbidden,
			"an unknown hash: "+forbidden, "an unknown activityId: "+forbidden)
		wantEvidence(t, report, "Q5", "every account's orders from 61 days back: "+forbidden)
		mu.Lock()
		defer mu.Unlock()
		if refused != 13 || ahead != 3 {
			t.Errorf("%d requests refused, want the walk's 5, Q5's 1 and the extras' 7; and %d of Q12's windows, want 3", refused, ahead)
		}
	})
}

// noRowCompared is Q12's verdict on a run that compared no row with the
// last run's.
const noRowCompared = "inconclusive: no row to compare: none of the last run's was seen again or shown gone, and none of this run's was shown late"

// Accounts that Schwab answers every window of with [], as it does an
// account whose history it does not serve, show Q1 nothing of where a
// history ends, whether the older windows answer [] too or are refused,
// and Q12, run again, no row to compare: both are inconclusive, with the
// walks as evidence. So is Q12 when the only row new to the state since
// the last run, which kept none, is one from the hour before that run, too
// near it to be shown late, or one after it, dated that day's midnight in
// New York, inside its window, which it could not have held.
func testNoRowsOnTheFake(t *testing.T) {
	t.Run("every window empty", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q12")
		fl.f.Override(schwabtest.TransactionsRoute, schwabtest.Answer(http.StatusOK, "[]", "Content-Type", "application/json"))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{
			"Q1":  "inconclusive: no account with rows walked past its history",
			"Q12": "inconclusive: no earlier run to compare with; this run's rows are kept for the next",
		})
		wantEvidence(t, report, "Q1", "{account 1}: no row back to 2000-01-01, where the walk stops; every window answered 200 with []; the window of 2000: 200 with []")
		later := fl.again(t, probeNow.AddDate(0, 0, 3), false)
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report = later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": noRowCompared})
		wantEvidence(t, report, "Q12", "{account 1}: 0 rows as they were; 0 rows new to the state")
	})
	first := time.Date(2027, 1, 4, 15, 0, 0, 0, time.UTC) // no row of the fixtures in the 60 days before
	for _, tc := range []struct {
		name            string
		time, tradeDate time.Time
		line            string
	}{
		{
			"a row from the hour before the last run alone", first.Add(-30 * time.Minute), first.Add(-30 * time.Minute),
			"{account 1}: 0 rows as they were; 1 row new to the state; 1 row from the hour before the last run, too near it to be shown late",
		},
		{
			"a row after the last run alone", first.Add(2 * time.Hour), time.Date(2027, 1, 4, 5, 0, 0, 0, time.UTC),
			"{account 1}: 0 rows as they were; 1 row new to the state",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, first, "Q12")
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			fl.more = append(fl.more, "84000000777")
			fl.f.AddRows(firstAccount.Hash, fixtureRow(t, 73508261943, "activityId", "84000000777",
				"time", strconv.Quote(schwabTime(tc.time)), "tradeDate", strconv.Quote(schwabTime(tc.tradeDate))))
			later := fl.again(t, first.AddDate(0, 0, 3), false)
			later.more = fl.more
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": noRowCompared})
			wantEvidence(t, report, "Q12", tc.line)
		})
	}
	t.Run("older windows refused", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1")
		fl.f.Override(schwabtest.TransactionsRoute, schwabtest.Answer(http.StatusOK, "[]", "Content-Type", "application/json"))
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.URL.Query().Get("startDate") < "2020" {
				fl.f.Script(schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: too old"}`, "Content-Type", "application/json"))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q1": "inconclusive: no account with rows walked past its history"})
		wantEvidence(t, report, "Q1", "{account 1}: no row in the windows it read; then a window was refused, HTTP 400, a guess: too old, "+
			"and the newest window, asked again at once, answered 200; the window of 2000: refused, HTTP 400, a guess: too old")
	})
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
// leaves out the order that its id finds, one kept a few days longer, which
// gives back an order entered 62 days ago, too young to count as old, and
// one kept for 120 days, which leaves out the order too, the verdict giving
// the bound that each shows, under the order's age rounded up to whole
// days, which holds it, as a list kept for 90 days shows of an order of 90
// days and 6 hours, and of one a few seconds short of 90 days when the
// probe began, which the list leaves out only after a 429's wait, when it
// is older than that, alone or beside a working order that comes back; an
// order entered weeks before its fill, which a list kept for a year gives
// back when asked for around its entry, not its fill, one entered over a
// year ago, which no list need give back, and one that says it was entered
// after its fill; the youngest execution past 67 days, which is the one
// asked about, though an older order that comes back sets the bound; a
// list that holds an order of nearly a year, the one bound that shows a
// year; the list of the last year refused, which the two days around the
// entry settle alone; lists that give back an order younger than the one
// they leave out, which bound how long they keep orders from both sides: a
// working order of 100 days, or another order of the two days around the
// entry; and lists that disagree, giving back an order entered an hour
// before the one they leave out, or that very order, which they disagree
// on though the two days around its entry answer after a 429's wait.
func testOrdersDepthOnTheFake(t *testing.T) {
	const unserved172 = "under 173 days: two days around the entry of an order entered 172 days ago leave it out, though its id finds it"
	t.Run("a list kept for 60 days", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		serveOrders(fl, []fakeOrder{metaSaleOrder}, keptFor(orderAgeLimit))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": unserved172})
		wantEvidence(t, report, "Q5", "the order of an execution 172 days old, got by its id, was entered 172 days ago",
			"two days around the order's entry: 200, 0 orders, without the order",
			"the account's orders of the last year: 200, 0 orders, none entered more than 67 days ago",
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
		wantVerdicts(t, report, map[string]string{"Q5": "longer than 60 days: an order entered 172 days ago came back"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: refused, HTTP 400, a guess: refused",
			"the account's orders of the last year: 200, 1 order, some entered more than 67 days ago")
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
		wantVerdicts(t, report, map[string]string{"Q5": "longer than 60 days: an order entered 224 days ago came back"})
		wantEvidence(t, report, "Q5", "the order of an execution 172 days old, got by its id, was entered 224 days ago",
			"two days around the order's entry: 200, 1 order, the order among them")
		around := "fromEnteredTime=" + strings.ReplaceAll(gtc.entered.Add(-oneDay).Format(schwabTimeFormat), ":", "%3A")
		if !slices.ContainsFunc(fl.f.Requests(), func(r schwabtest.Request) bool { return strings.Contains(r.Query, around) }) {
			t.Errorf("no list asked for from %s", around)
		}
	})
	t.Run("an order entered after its fill", func(t *testing.T) {
		t.Parallel()
		// An order that says it was entered ten days ago, though its
		// execution is 172 days old, says nothing of a list's age.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		odd := metaSaleOrder
		odd.entered = probeNow.AddDate(0, 0, -10)
		serveOrders(fl, []fakeOrder{odd}, keptFor(orderAgeLimit))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q5": "inconclusive: the order got by its id was entered within the last 67 days, though its fill is older"})
	})
	t.Run("an order entered over a year ago", func(t *testing.T) {
		t.Parallel()
		// Run on 2026-09-20, the META sale is 191 days old, and its order,
		// good till canceled, was entered 370 days ago: a list kept for a
		// year leaves it out of the two days around its entry too.
		fl := newProbeLive(t, time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC), "Q1", "Q5")
		gtc := metaSaleOrder
		gtc.entered = time.Date(2025, 9, 15, 14, 0, 0, 0, time.UTC)
		serveOrders(fl, []fakeOrder{gtc}, keptFor(MaxTransactionWindow))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q5": "inconclusive: the order of an execution 191 days old, got by its id, " +
			"was entered 370 days ago, too long ago for two days around its entry to lie in the last year"})
		if slices.ContainsFunc(fl.f.Requests(), func(r schwabtest.Request) bool { return strings.Contains(r.Query, "fromEnteredTime") }) {
			t.Error("a list of orders was asked for")
		}
	})
	// Orders of {account 2} besides the META sale's, and executions of
	// them in the history rows.
	order := func(id int64, age time.Duration) fakeOrder {
		filled := probeNow.Add(-age)
		return fakeOrder{account: secondAccount, id: id, entered: filled.Add(-5 * time.Minute), filled: filled}
	}
	execution := func(fl *fakeLive, activity int64, o fakeOrder) {
		fl.more = append(fl.more, strconv.FormatInt(activity, 10), strconv.FormatInt(o.id, 10))
		fl.f.AddRows(secondAccount.Hash, fixtureRow(t, metaSale, "activityId", strconv.FormatInt(activity, 10), "orderId", strconv.FormatInt(o.id, 10),
			"time", strconv.Quote(schwabTime(o.filled)), "tradeDate", strconv.Quote(schwabTime(o.filled))))
	}
	t.Run("the youngest execution past 67 days", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		young, picked := order(1004517700101, 64*oneDay), order(1004517700102, 92*oneDay)
		execution(fl, 84000000711, young)
		execution(fl, 84000000712, picked)
		serveOrders(fl, []fakeOrder{metaSaleOrder, young, picked}, keptFor(MaxTransactionWindow))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "longer than 60 days: an order entered 172 days ago came back"})
		wantEvidence(t, report, "Q5", "the order of an execution 92 days old, got by its id, was entered 92 days ago",
			"two days around the order's entry: 200, 1 order, the order among them",
			"the account's orders of the last year: 200, 3 orders, some entered more than 67 days ago")
	})
	t.Run("a list kept for 63 days", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		recent := order(1004517700103, 62*oneDay)
		fl.more = append(fl.more, strconv.FormatInt(recent.id, 10))
		serveOrders(fl, []fakeOrder{metaSaleOrder, recent}, keptFor(63*oneDay))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": unserved172})
		wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 0 orders, without the order",
			"the account's orders of the last year: 200, 1 order, none entered more than 67 days ago")
	})
	t.Run("a list kept for 120 days", func(t *testing.T) {
		t.Parallel()
		// It leaves out the META sale's order, entered 172 days ago, as a
		// list kept for 60 days does: the verdict gives the bound.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		serveOrders(fl, []fakeOrder{metaSaleOrder}, keptFor(120*oneDay))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q5": unserved172})
	})
	t.Run("a list kept for 90 days", func(t *testing.T) {
		t.Parallel()
		// It leaves out the order of an execution 90 days and 6 hours old,
		// entered 5 minutes before it: the bound, rounded up to whole
		// days, holds the order's age.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		picked := order(1004517700108, 90*oneDay+6*time.Hour)
		execution(fl, 84000000713, picked)
		serveOrders(fl, []fakeOrder{metaSaleOrder, picked}, keptFor(90*oneDay))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "under 91 days: two days around the entry of an order entered 90 days ago leave it out, though its id finds it"})
		wantEvidence(t, report, "Q5", "the order of an execution 90 days old, got by its id, was entered 90 days ago",
			"two days around the order's entry: 200, 0 orders, without the order")
	})
	// The order of an execution 5 minutes short of 90 days old was entered
	// 90 days less 5 seconds before it is got by its id. The first try of
	// the two days around its entry draws a 429 that asks the run to wait 30
	// seconds, after which a list kept for 90 days leaves out the order, by
	// then over 90 days old: the bound holds its age then, alone, or beside
	// a working order entered 70 days ago that comes back.
	working := fakeOrder{account: secondAccount, id: 1004517700110, entered: probeNow.Add(-70 * oneDay)}
	for _, tc := range []struct {
		name    string
		working bool
		want    string
	}{
		{
			"a list kept for 90 days that answers after a 429", false,
			"under 91 days: two days around the entry of an order entered 89 days ago leave it out, though its id finds it",
		},
		{
			"a list kept for 90 days that answers after a 429, and gives back a working order", true,
			"from 70 to 91 days: an order entered 70 days ago came back, " +
				"and two days around the entry of one entered 89 days ago leave it out, though its id finds it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "Q5")
			picked := order(1004517700109, 90*oneDay-5*time.Minute)
			execution(fl, 84000000714, picked)
			orders := []fakeOrder{metaSaleOrder}
			if tc.working {
				fl.more = append(fl.more, strconv.FormatInt(working.id, 10))
				orders = append(orders, working)
			}
			var mu sync.Mutex
			got, slowed := false, false
			fl.f.OnAPI(func(r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				q := r.URL.Query()
				from, err1 := time.Parse(schwabTimeFormat, q.Get("fromEnteredTime"))
				to, err2 := time.Parse(schwabTimeFormat, q.Get("toEnteredTime"))
				switch {
				case strings.HasSuffix(r.URL.Path, "/orders/"+strconv.FormatInt(picked.id, 10)) && !got:
					got = true
					picked.entered = fl.clock.Now().Add(-90*oneDay + 5*time.Second)
				case err1 == nil && err2 == nil && to.Sub(from) == 2*oneDay && !slowed:
					slowed = true
					fl.f.Script(schwabtest.Answer(http.StatusTooManyRequests, `{"message":"a guess: slow down"}`,
						"Content-Type", "application/json", "Retry-After", "30"))
				}
			})
			serveOrdersOf(fl, func() []fakeOrder {
				mu.Lock()
				defer mu.Unlock()
				return append(slices.Clone(orders), picked)
			}, keptFor(90*oneDay))
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q5": tc.want})
			wantEvidence(t, report, "Q5", "the order of an execution 89 days old, got by its id, was entered 89 days ago",
				"two days around the order's entry: 200, 0 orders, without the order")
			mu.Lock()
			defer mu.Unlock()
			if !slowed {
				t.Error("no 429 for the two days around the entry")
			}
		})
	}
	t.Run("the list of the last year refused", func(t *testing.T) {
		t.Parallel()
		// The two days around the entry hold the order, which settles it
		// alone.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		fl.f.OnAPI(func(r *http.Request) {
			q := r.URL.Query()
			from, err1 := time.Parse(schwabTimeFormat, q.Get("fromEnteredTime"))
			to, err2 := time.Parse(schwabTimeFormat, q.Get("toEnteredTime"))
			if err1 == nil && err2 == nil && to.Sub(from) == MaxTransactionWindow {
				fl.f.Script(schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: too long"}`, "Content-Type", "application/json"))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "longer than 60 days: an order entered 172 days ago came back"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 1 order, the order among them",
			"the account's orders of the last year: refused, HTTP 400, a guess: too long")
	})
	t.Run("a list that holds an order of nearly a year", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		first := order(1004517700105, 340*oneDay)
		fl.more = append(fl.more, strconv.FormatInt(first.id, 10))
		serveOrders(fl, []fakeOrder{metaSaleOrder, first}, keptFor(MaxTransactionWindow))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "a year: an order entered 340 days ago came back"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 1 order, the order among them",
			"the account's orders of the last year: 200, 2 orders, some entered more than 67 days ago")
	})
	t.Run("a list kept for 120 days that gives back a working order", func(t *testing.T) {
		t.Parallel()
		// An order of {account 2} entered 100 days ago, never filled, comes
		// back, while the META sale's, entered 172 days ago, does not: the
		// lists agree, and bound how long they keep orders from both sides.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		working := fakeOrder{account: secondAccount, id: 1004517700106, entered: probeNow.Add(-100 * oneDay)}
		fl.more = append(fl.more, strconv.FormatInt(working.id, 10))
		serveOrders(fl, []fakeOrder{metaSaleOrder, working}, keptFor(120*oneDay))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "from 100 to 173 days: an order entered 100 days ago came back, " +
			"and two days around the entry of one entered 172 days ago leave it out, though its id finds it"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 0 orders, without the order",
			"the account's orders of the last year: 200, 1 order, some entered more than 67 days ago")
	})
	t.Run("a list that gives back another order of the two days around the entry", func(t *testing.T) {
		t.Parallel()
		// An order entered 12 hours after the META sale's lies in the two
		// days around its entry, and a list kept for 6 hours less than the
		// META sale's order's age gives it back alone.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		next := fakeOrder{account: secondAccount, id: 1004517700107, entered: metaSaleOrder.entered.Add(12 * time.Hour)}
		fl.more = append(fl.more, strconv.FormatInt(next.id, 10))
		serveOrders(fl, []fakeOrder{metaSaleOrder, next}, keptFor(probeNow.Sub(metaSaleOrder.entered)-6*time.Hour))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "from 171 to 173 days: an order entered 171 days ago came back, " +
			"and two days around the entry of one entered 172 days ago leave it out, though its id finds it"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 1 order, without the order",
			"the account's orders of the last year: 200, 1 order, some entered more than 67 days ago")
	})
	t.Run("lists that disagree", func(t *testing.T) {
		t.Parallel()
		// The lists leave out the META sale's order, though one entered an
		// hour before it comes back.
		fl := newProbeLive(t, probeNow, "Q1", "Q5")
		other := fakeOrder{account: secondAccount, id: 1004517700104, entered: metaSaleOrder.entered.Add(-time.Hour)}
		fl.more = append(fl.more, strconv.FormatInt(other.id, 10))
		serveOrders(fl, []fakeOrder{metaSaleOrder, other}, func(entered, now time.Time) bool {
			return !entered.Equal(metaSaleOrder.entered) && keptFor(MaxTransactionWindow)(entered, now)
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q5": "inconclusive: the lists disagree: an order entered 172 days ago came back, " +
			"while two days around the entry of one entered 172 days ago left it out"})
		wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 1 order, without the order",
			"the account's orders of the last year: 200, 1 order, some entered more than 67 days ago")
	})
	for _, tc := range []struct {
		name   string
		slowed bool // with the first try of the two days around the entry answered 429, which asks the run to wait 30 seconds
	}{
		{"lists that disagree on the order itself", false},
		{"lists that disagree on the order itself, the two days around its entry after a 429", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The two days around the entry of the META sale's order answer
			// [], while the list of the last year gives that very order
			// back: the order that came back is as old as the one left out,
			// the likeliest way for the lists to disagree, however much
			// older it was by the time the two days around its entry left
			// it out.
			fl := newProbeLive(t, probeNow, "Q1", "Q5")
			around := metaSaleOrder.entered.Add(-oneDay).Format(schwabTimeFormat)
			var tries atomic.Int32
			fl.f.OnAPI(func(r *http.Request) {
				if r.URL.Query().Get("fromEnteredTime") != around {
					return
				}
				if tries.Add(1) == 1 && tc.slowed {
					fl.f.Script(schwabtest.Answer(http.StatusTooManyRequests, `{"message":"a guess: slow down"}`,
						"Content-Type", "application/json", "Retry-After", "30"))
					return
				}
				fl.f.Script(schwabtest.Answer(http.StatusOK, `[]`, "Content-Type", "application/json"))
			})
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q5": "inconclusive: the lists disagree: an order entered 172 days ago came back, " +
				"while two days around the entry of one entered 172 days ago left it out"})
			wantEvidence(t, report, "Q5", "two days around the order's entry: 200, 0 orders, without the order",
				"the account's orders of the last year: 200, 1 order, some entered more than 67 days ago")
		})
	}
}

// Q8 under the fake's settings: an app without the Market Data product, and
// two runs across an ex-date, four weeks apart, so the second after a new
// login, whose closes before it are as they were, or scaled by the
// dividend's factor; the closes of the first run kept by a run on the
// ex-date that cannot read its own, with when the first read them; closes
// adjusted for one symbol alone, which disagree; a first run on the
// ex-date's morning, which reads closes adjusted already, so that no
// verdict may come from it; daily runs across the ex-date, the one on its
// morning kept beside the one before its last close; and the runs that the
// state keeps.
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
		// On the ex-date, before the open, SPY's history is refused and VTI's
		// empty: the first run's closes are kept, read before the ex-date.
		middle := fl.again(t, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), true)
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
			wantVerdicts(t, fl.report(t), map[string]string{"Q8": adjustNeedsTwo})
			later := fl.again(t, time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC), true)
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q8": "daily closes are " + tc.want})
			wantEvidence(t, report, "Q8", "SPY: across the ex-date 2026-09-18, its closes are "+tc.want, "VTI: across the ex-date 2026-09-18, its closes are "+tc.want)
		})
	}
	t.Run("closes adjusted for one symbol alone", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q8")
		fakeMarket{exDate: marketExDate, amount: 1.9, adjusted: true, only: "SPY"}.serve(fl)
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		later := fl.again(t, time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC), true)
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q8": "inconclusive: the symbols disagree"})
		wantEvidence(t, report, "Q8", "SPY: across the ex-date 2026-09-18, its closes are "+adjustDividend, "VTI: across the ex-date 2026-09-18, its closes are "+adjustNone)
	})
	t.Run("a first run on the ex-date's morning", func(t *testing.T) {
		t.Parallel()
		// At 08:00 in New York on the ex-date, before its candle, the closes
		// are adjusted already: the next run finds them as they were.
		fl := newProbeLive(t, time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC), "Q8")
		fakeMarket{exDate: marketExDate, amount: 1.9, adjusted: true}.serve(fl)
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q8": adjustNeedsTwo})
		later := fl.again(t, time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC), true)
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q8": adjustNeedsTwo})
		wantEvidence(t, report, "Q8", "SPY: no earlier run came before the last close before its ex-date, 2026-09-18",
			"VTI: no earlier run came before the last close before its ex-date, 2026-09-18")
	})
	for _, adjusted := range []bool{false, true} {
		want := map[bool]string{false: adjustNone, true: adjustDividend}[adjusted]
		t.Run("daily runs across the ex-date, closes "+want, func(t *testing.T) {
			t.Parallel()
			// At 10:00 in New York on Thursday, before the last close before
			// the ex-date; on the ex-date's morning, which keeps Thursday's
			// closes for the next run; and on Monday, which compares with
			// Thursday's.
			fl := newProbeLive(t, time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC), "Q8")
			fakeMarket{exDate: marketExDate, amount: 1.9, adjusted: adjusted}.serve(fl)
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			exDate := fl.again(t, time.Date(2026, 9, 18, 14, 0, 0, 0, time.UTC), false)
			if err := exDate.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, exDate.report(t), "Q8", "SPY: its ex-date, 2026-09-18, is not over yet")
			monday := fl.again(t, time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC), false)
			if err := monday.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := monday.report(t)
			wantVerdicts(t, report, map[string]string{"Q8": "daily closes are " + want})
			wantEvidence(t, report, "Q8", "SPY: across the ex-date 2026-09-18, its closes are "+want, "VTI: across the ex-date 2026-09-18, its closes are "+want)
		})
	}
	t.Run("the runs kept", func(t *testing.T) {
		t.Parallel()
		// Each run keeps its closes after the runs' before it, the last
		// keptRuns of them, here a state of keptRuns runs a day apart; a run
		// that cannot read its closes keeps the runs' before it.
		fl := newProbeLive(t, probeNow, "Q8")
		st, err := openState(fl.r.stateFile, fl.r.keyFile, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		var earlier []symbolCloses
		for i := range keptRuns {
			earlier = append(earlier, symbolCloses{AsOf: "2026-08-03", Closes: map[string]float64{"2026-08-03": 600}, Read: probeNow.AddDate(0, 0, i-keptRuns)})
		}
		if err := st.put("Q8", adjustState{Runs: map[string][]symbolCloses{"SPY": earlier, "VTI": earlier}}); err != nil {
			t.Fatal(err)
		}
		if err := st.save(); err != nil {
			t.Fatal(err)
		}
		runs := func(fl *fakeLive) []symbolCloses {
			t.Helper()
			st, err := openState(fl.r.stateFile, fl.r.keyFile, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			var as adjustState
			if _, err := st.get("Q8", &as); err != nil {
				t.Fatal(err)
			}
			return as.Runs["SPY"]
		}
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		kept := runs(fl)
		if len(kept) != keptRuns || !kept[0].Read.Equal(earlier[1].Read) || kept[len(kept)-1].Read.Before(probeNow) || len(kept[len(kept)-1].Closes) != keptCloses {
			t.Errorf("%d runs kept, the first read at %v, the last at %v with %d closes", len(kept), kept[0].Read, kept[len(kept)-1].Read, len(kept[len(kept)-1].Closes))
		}
		later := fl.again(t, probeNow.Add(oneDay), false)
		later.f.OverridePath("GET /marketdata/v1/pricehistory", func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusBadRequest, `{"message":"a guess: no history"}`)
		})
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		if after := runs(later); !slices.EqualFunc(after, kept, func(a, b symbolCloses) bool { return a.Read.Equal(b.Read) }) {
			t.Errorf("after a run without closes, %d runs kept", len(after))
		}
	})
}

// adjustNeedsTwo is Q8's verdict while no two runs lie either side of an
// ex-date.
const adjustNeedsTwo = "inconclusive: it needs a run before the last close before an ex-date, and one after the ex-date; this run's closes are kept for the next"

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

// Q12 shows no row gone from an answer that may have been cut at the cap,
// nor late against a last run's answer that may have been: the row that the
// cap left out of that answer is new to the state, though it was there.
// Neither says that no row did what it cannot show. A cut answer leaves it
// undecided whether a row missing from it changed or went, whichever it did
// since, and whether it went, beside a row that it shows changed; and
// whether rows that it left out came late, as a row dated weeks back that
// the cap leaves out did, while one that holds every row of the state shows
// that none changed or went. With all three undecided, the verdict is
// inconclusive. A cut last answer leaves it undecided whether a row new to
// the state came late, and whether rows that it left out changed or went,
// as one that it left out went since. And the state keeps a row that a cut
// answer left out, so that a complete answer after it shows the row
// changed, or gone, while whether rows that the cut answer left out did the
// other stays undecided.
func testChangesCutOnTheFake(t *testing.T) {
	const (
		cutMissing = "the answer may have been cut at the cap, so 1 row missing from it cannot be shown changed or gone, " +
			"nor rows that it left out shown late"
		lastCutNone = "the last run's answer may have been cut at the cap, so rows that it left out cannot be shown changed or gone, " +
			"though no row new to the state could have come late"
	)
	// The XYZ dividend goes, or changes, and the second run's answer, cut at
	// the cap, leaves it out.
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, fl *fakeLive)
	}{
		{"this run's answer", func(_ *testing.T, fl *fakeLive) { fl.f.RemoveRow(secondAccount.Hash, xyzDividend) }},
		{"this run's answer, cut short of a row that changed", func(t *testing.T, fl *fakeLive) {
			fl.f.ReplaceRow(secondAccount.Hash, xyzDividend, fixtureRow(t, xyzDividend, "netAmount", "26"))
			fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 1})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q12")
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			tc.change(t, fl)
			later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
			later.r.pageCap = 1
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": "inconclusive: undecided since the last run: " +
				"whether 1 row changed or went, and whether rows that a cut answer left out came late"})
			wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; "+cutMissing)
		})
	}
	t.Run("this run's answer, with a row changed", func(t *testing.T) {
		t.Parallel()
		// What changed is said, beside what the cut answer leaves undecided.
		fl := newProbeLive(t, probeNow, "Q12")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
		fl.f.ReplaceRow(secondAccount.Hash, optionClose, fixtureRow(t, optionClose, "netAmount", "311.32"))
		later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
		later.r.pageCap = 1
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows changed after they posted; " +
			"undecided: whether 1 row went, and whether rows that a cut answer left out came late"})
		wantEvidence(t, report, "Q12", "{account 2}: 0 rows as they were; 0 rows new to the state; 1 row changed, 28 days old, in netAmount (1); "+cutMissing)
	})
	for _, tc := range []struct {
		name string
		late bool // with a row dated 55 days before the first run, which comes after it
	}{
		{"this run's answer, holding every row of the state", false},
		{"this run's answer, holding every row of the state, and cut short of a row that came late", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// {account 2}'s 2 rows reach a cap of 2, though none is
			// missing: no row went. A row that came late, older than
			// both, is the one that a cap that keeps the newest leaves
			// out.
			fl := newProbeLive(t, probeNow, "Q12")
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			if tc.late {
				at := strconv.Quote(schwabTime(probeNow.Add(-55 * oneDay)))
				fl.more = append(fl.more, "84000000111")
				fl.f.AddRows(secondAccount.Hash, fixtureRow(t, xyzDividend, "activityId", "84000000111", "time", at, "tradeDate", at))
				fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 2})
			}
			later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
			later.more = fl.more
			later.r.pageCap = 2
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": "no row changed or went since the last run; undecided: whether rows that a cut answer left out came late"})
			wantEvidence(t, report, "Q12", "{account 2}: 2 rows as they were; 0 rows new to the state; "+
				"the answer may have been cut at the cap, so rows that it left out cannot be shown late, though no row of the state is missing from it")
		})
	}
	t.Run("the last run's answer", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q12")
		fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 1})
		fl.r.pageCap = 1
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.f.SetTransactionRules(schwabtest.TransactionRules{})
		later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
		later.r.pageCap = MaxTransactions
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "inconclusive: undecided since the last run: " +
			"whether rows that the last run's cut answer left out changed or went, and whether 1 row came late"})
		wantEvidence(t, report, "Q12", "{account 1}: 0 rows as they were; 0 rows new to the state",
			"{account 2}: 1 row as they were; 1 row new to the state; the last run's answer may have been cut at the cap, "+
				"so 1 row new to the state cannot be shown late, nor rows that it left out shown changed or gone")
	})
	t.Run("the last run's answer, cut short of a row that went since", func(t *testing.T) {
		t.Parallel()
		// The cap of 1 leaves the XYZ dividend out of the first run's
		// answer, and so out of the state: gone from the second's, it
		// is missing from neither.
		fl := newProbeLive(t, probeNow, "Q12")
		fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 1})
		fl.r.pageCap = 1
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.f.SetTransactionRules(schwabtest.TransactionRules{})
		fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
		later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
		later.r.pageCap = MaxTransactions
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "no row came late since the last run; " +
			"undecided: whether rows that the last run's cut answer left out changed or went"})
		wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; "+lastCutNone)
	})
	// Three runs a day apart, the second's answer cut at the cap, which
	// leaves out the XYZ dividend; the third, complete again, finds it
	// changed, or gone.
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, fl *fakeLive)
		want   string
		found  string
	}{
		{
			"a row that a cut answer left out, changed after it",
			func(t *testing.T, fl *fakeLive) {
				fl.f.ReplaceRow(secondAccount.Hash, xyzDividend, fixtureRow(t, xyzDividend, "netAmount", "26"))
			},
			"rows changed after they posted; undecided: whether rows that the last run's cut answer left out went",
			"1 row changed, 50 days old, in netAmount (1)",
		},
		{
			"a row that a cut answer left out, gone after it",
			func(_ *testing.T, fl *fakeLive) { fl.f.RemoveRow(secondAccount.Hash, xyzDividend) },
			"rows went; undecided: whether rows that the last run's cut answer left out changed", "1 row gone",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q12")
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			fl.f.SetTransactionRules(schwabtest.TransactionRules{Cap: 1})
			cut := fl.again(t, probeNow.AddDate(0, 0, 1), false)
			cut.r.pageCap = 1
			if err := cut.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, cut.report(t), "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; "+cutMissing)
			fl.f.SetTransactionRules(schwabtest.TransactionRules{})
			tc.change(t, fl)
			last := cut.again(t, probeNow.AddDate(0, 0, 2), false)
			last.r.pageCap = MaxTransactions
			if err := last.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := last.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": tc.want})
			wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; "+tc.found+"; "+lastCutNone)
		})
	}
}

// Q12 under windows by tradeDate, where a window's edge can lie between a
// row's tradeDate and its time, shows no row gone or late that a window
// left out by its tradeDate, and leaves it undecided whether such a row
// changed or went, or came late: a row at 13:55 on 2026-08-26, dated
// 2026-08-20, which the last run's window, which ends at the midnight that
// begins 2026-08-26, held by its tradeDate, and that is gone 12 days later,
// alone, or beside a row gone from both runs' windows, which shows that
// rows went; and the XYZ dividend at 14:40, dated midnight, which a window
// from 10:00 that day leaves out, though it is still there, as it was or
// changed. A window ends days after its run, past the midnight that may
// date an evening fill: such a fill at 20:30, dated the next midnight,
// before a run at 22:00, is held by that run's window, as a window by time
// would hold it, and is shown as it was, changed or gone 12 days later.
// When it posted after that run, it is shown late beside such a fill at
// 21:30 of another account that the run held, which shows that its reads
// would have shown the fill; alone, it is left undecided whether it came
// late, or was there all along and changed, as it is when it did change,
// under a Schwab that serves a window only up to the request, which left it
// out of that run's answer; and so it is of such a fill on a Friday, dated
// the Monday, that posted after a run at 22:00 that Friday, beside such a
// fill of the run before, of another account, whose window failed in the
// last run. Such a fill at 21:30 is shown changed too, and, when it posted
// after that run, too near it to be shown late, though it may have been
// there all along and changed. Such a fill, which the
// last run held, before the start of a window that holds it by its
// tradeDate, the state keeps, which leaves nothing undecided either, and
// one that posted after the last run, whose window held both its times,
// came late, though this window's start leaves out its time; and such a
// fill before the start of the last run's window, dated inside it, a run an
// hour later finds new, beside one at 20:30 that evening, dated after the
// last run, at 22:00, which it cannot show late either. Behind a cut answer
// of a run at 22:00, the cut alone leaves it undecided whether such a fill
// at 20:30, which posted after the run, came late, while one at 21:30 is
// too near it to be shown late. The fill at 20:30, left out of the state
// of an earlier version whose window ended at the run, which kept no other
// end, 12 days later as it was, changed or gone, is not compared: the run
// starts afresh. Such a version reads the state of this one as of a window
// that ended at the run.
func testChangesByTradeDateOnTheFake(t *testing.T) {
	byTradeDate := schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate}
	for _, tc := range []struct {
		name string
		gone bool // with the XYZ dividend gone too, which both windows hold
		want string
		line string
	}{
		{
			"a row past the window's end, held by its tradeDate", false, "no row came late since the last run; undecided: whether 1 row changed or went",
			"{account 2}: 2 rows as they were; 0 rows new to the state; ",
		},
		{
			"a row past the window's end, held by its tradeDate, beside a row gone", true, "rows went; undecided: whether 1 row changed",
			"{account 2}: 1 row as they were; 0 rows new to the state; 1 row gone; ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The window of a run on Thursday 2026-08-20 ends at the midnight
			// in New York that begins the fourth weekday after it, Wednesday
			// 2026-08-26, after the row's tradeDate and before its time.
			first := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
			fl := newProbeLive(t, first, "Q12")
			fl.f.SetTransactionRules(byTradeDate)
			fl.more = append(fl.more, "84000000104")
			fl.f.AddRows(secondAccount.Hash, fixtureRow(t, metaSale, "activityId", "84000000104",
				"time", `"2026-08-26T13:55:09+0000"`, "tradeDate", `"2026-08-20T04:00:00+0000"`))
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, fl.report(t), "Q12", "{account 2}: 3 rows, kept for the next run; no earlier run to compare with")
			fl.f.RemoveRow(secondAccount.Hash, 84000000104)
			if tc.gone {
				fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
			}
			later := fl.again(t, first.AddDate(0, 0, 12), true)
			later.more = fl.more
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": tc.want})
			wantEvidence(t, report, "Q12", tc.line+"1 row missing from the answer, with its time or tradeDate outside a run's window, so not shown changed or gone")
		})
	}
	for _, tc := range []struct {
		name    string
		changed bool // with the XYZ dividend changed between the runs
	}{
		{"a row inside the window's start, left out by its tradeDate", false},
		{"a row inside the window's start, left out by its tradeDate, changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q12")
			fl.f.SetTransactionRules(byTradeDate)
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, fl.report(t), "Q12", "{account 2}: 2 rows, kept for the next run; no earlier run to compare with")
			if tc.changed {
				fl.f.ReplaceRow(secondAccount.Hash, xyzDividend, fixtureRow(t, xyzDividend, "netAmount", "26"))
			}
			// The window starts at 2026-07-15T10:00Z, between the dividend's
			// tradeDate, 04:00Z, and its time, 14:40Z.
			later := fl.again(t, time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC), true)
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": "no row came late since the last run; undecided: whether 1 row changed or went"})
			wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; "+
				"1 row missing from the answer, with its time or tradeDate outside a run's window, so not shown changed or gone")
		})
	}
	// Evening fills at 20:30 and 21:30 in New York, dated the next midnight.
	const at2030, at2130 = `"2026-08-21T00:30:00+0000"`, `"2026-08-21T01:30:00+0000"`
	const unserved = "1 row new to the state, dated after the last run, whose window Schwab may have served only up to the request, so not shown changed or late"
	for _, tc := range []struct {
		name, time string
		after      string // what the fill does after the last run: "changed", "went", or "posted", when it was not there before
		upTo       bool   // whether Schwab serves a window only up to the request
		beside     bool   // whether another such fill, at 21:30, of {account 1}, is there at both runs
		want, line string
	}{
		{
			"an evening fill before the last run, dated the next midnight, as it was", at2030, "", false, false,
			"no row changed, went or came late since the last run", "3 rows as they were; 0 rows new to the state",
		},
		{
			"an evening fill before the last run, dated the next midnight, changed, in windows served only up to the request", at2030, "changed", true, false,
			"no row went since the last run; undecided: whether 1 row changed, and whether 1 row came late",
			"2 rows as they were; 1 row new to the state; " + unserved,
		},
		{
			"an evening fill before the last run, dated the next midnight, changed", at2030, "changed", false, false,
			"rows changed after they posted", "2 rows as they were; 0 rows new to the state; 1 row changed, 12 days old, in netAmount (1)",
		},
		{
			"an evening fill before the last run, dated the next midnight, gone", at2030, "went", false, false,
			"rows went", "2 rows as they were; 0 rows new to the state; 1 row gone",
		},
		{
			"an evening fill before the last run, dated the next midnight, posted after it", at2030, "posted", false, false,
			"no row went since the last run; undecided: whether 1 row changed, and whether 1 row came late",
			"2 rows as they were; 1 row new to the state; " + unserved,
		},
		{
			"an evening fill before the last run, dated the next midnight, posted after it, beside one of another account that the last run held", at2030, "posted", false, true,
			"rows came late", "2 rows as they were; 1 row new to the state; 1 row came late, posted from 1 hour to 13 days after its time",
		},
		{
			"an evening fill half an hour before the last run, dated the next midnight, changed", at2130, "changed", false, false,
			"rows changed after they posted", "2 rows as they were; 0 rows new to the state; 1 row changed, 12 days old, in netAmount (1)",
		},
		{
			"an evening fill half an hour before the last run, dated the next midnight, posted after it", at2130, "posted", false, false,
			"no row went or came late since the last run; undecided: whether 1 row changed",
			"2 rows as they were; 1 row new to the state; 1 row from the hour before the last run, dated after it, " +
				"whose window Schwab may have served only up to the request, so not shown changed, and too near it to be shown late",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC) // 22:00 in New York
			fl := newProbeLive(t, first, "Q12")
			rules := byTradeDate
			rules.UpToNow = tc.upTo
			fl.f.SetTransactionRules(rules)
			fl.more = append(fl.more, "84000000105")
			fill := func(changes ...string) string {
				return fixtureRow(t, metaSale, append([]string{"activityId", "84000000105", "time", tc.time, "tradeDate", `"2026-08-21T04:00:00+0000"`}, changes...)...)
			}
			kept := 2 // the rows that the first run's answer holds
			if tc.after != "posted" {
				fl.f.AddRows(secondAccount.Hash, fill())
				if !tc.upTo {
					kept++
				}
			}
			if tc.beside {
				fl.more = append(fl.more, "84000000117")
				fl.f.AddRows(firstAccount.Hash, fixtureRow(t, metaSale, "activityId", "84000000117", "accountNumber", strconv.Quote(firstAccount.Number),
					"time", at2130, "tradeDate", `"2026-08-21T04:00:00+0000"`))
			}
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, fl.report(t), "Q12", "{account 2}: "+plural(kept, "row")+", kept for the next run; no earlier run to compare with")
			switch tc.after {
			case "changed":
				fl.f.ReplaceRow(secondAccount.Hash, 84000000105, fill("netAmount", "603.37"))
			case "went":
				fl.f.RemoveRow(secondAccount.Hash, 84000000105)
			case "posted":
				fl.f.AddRows(secondAccount.Hash, fill())
			}
			later := fl.again(t, first.AddDate(0, 0, 12), true)
			later.more = fl.more
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": tc.want})
			wantEvidence(t, report, "Q12", "{account 2}: "+tc.line)
		})
	}
	t.Run("an evening fill before the last run, dated after it, posted after it, beside a fill dated ahead that an earlier run held", func(t *testing.T) {
		t.Parallel()
		// {account 1}'s fill at 21:30 on a Thursday, dated the next midnight,
		// is in its answer at a run at 22:00, and its window fails at a run
		// a day later, whose answers hold no row dated after they came, so
		// the state keeps {account 1}'s of the first run. {account 2}'s fill
		// at 20:30 on the Friday, dated the Monday, posts after the second
		// run, whose reads the first run's answer says nothing of.
		first := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC) // 22:00 in New York
		fl := newProbeLive(t, first, "Q12")
		fl.f.SetTransactionRules(byTradeDate)
		fl.more = append(fl.more, "84000000105", "84000000117")
		fl.f.AddRows(firstAccount.Hash, fixtureRow(t, metaSale, "activityId", "84000000117", "accountNumber", strconv.Quote(firstAccount.Number),
			"time", at2130, "tradeDate", `"2026-08-21T04:00:00+0000"`))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		var arrivals atomic.Int32
		fl.f.OnAPI(func(r *http.Request) {
			// The checks' read of {account 1}'s rows, then Q12's.
			if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.PathValue("hash") == firstAccount.Hash && arrivals.Add(1) == 2 {
				fl.f.Script(schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: refused"}`, "Content-Type", "application/json"))
			}
		})
		second := fl.again(t, first.Add(oneDay), false)
		second.more = fl.more
		if err := second.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, second.report(t), "Q12", "{account 1}: the window of the last 60 days failed")
		fl.f.AddRows(secondAccount.Hash, fixtureRow(t, metaSale, "activityId", "84000000105", "time", `"2026-08-22T00:30:00+0000"`, "tradeDate", `"2026-08-24T04:00:00+0000"`))
		later := second.again(t, first.AddDate(0, 0, 13), true)
		later.more = fl.more
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "no row went since the last run; undecided: whether 1 row changed, and whether 1 row came late"})
		wantEvidence(t, report, "Q12", "{account 1}: 1 row as they were; 0 rows new to the state", "{account 2}: 2 rows as they were; 1 row new to the state; "+unserved)
	})
	for _, after := range []string{"as it was", "changed", "went"} {
		t.Run("an evening fill before the last run, dated the next midnight, left out by an earlier version's window, "+after, func(t *testing.T) {
			t.Parallel()
			first := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC) // 22:00 in New York
			fl := newProbeLive(t, first, "Q12")
			fl.f.SetTransactionRules(byTradeDate)
			fl.more = append(fl.more, "84000000105")
			fill := func(changes ...string) string {
				return fixtureRow(t, metaSale, append([]string{"activityId", "84000000105", "time", at2030, "tradeDate", `"2026-08-21T04:00:00+0000"`}, changes...)...)
			}
			fl.f.AddRows(secondAccount.Hash, fill())
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			st, err := openState(fl.r.stateFile, fl.r.keyFile, func(string) {})
			if err != nil {
				t.Fatal(err)
			}
			// An earlier version, whose window ended at its run, reads this
			// one's window as ending there too.
			var earlier struct {
				Accounts map[string]struct {
					End time.Time `json:"end"`
				} `json:"accounts"`
			}
			if _, err := st.get("Q12", &earlier); err != nil {
				t.Fatal(err)
			}
			for _, a := range earlier.Accounts {
				if a.End.Before(first) || a.End.After(fl.clock.Now()) {
					t.Errorf("an earlier version reads the window as ending at %v, not during the run, from %v to %v", a.End, first, fl.clock.Now())
				}
			}
			// The state as such a version kept it: without the window's end,
			// and without the fill, which its window left out by its
			// tradeDate.
			var rs rowsState
			if _, err := st.get("Q12", &rs); err != nil {
				t.Fatal(err)
			}
			for k, a := range rs.Accounts {
				delete(a.Rows, st.digest("activity id", "84000000105"))
				a.End = time.Time{}
				rs.Accounts[k] = a
			}
			if err := st.put("Q12", rs); err != nil {
				t.Fatal(err)
			}
			if err := st.save(); err != nil {
				t.Fatal(err)
			}
			kept := "3 rows"
			switch after {
			case "changed":
				fl.f.ReplaceRow(secondAccount.Hash, 84000000105, fill("netAmount", "603.37"))
			case "went":
				fl.f.RemoveRow(secondAccount.Hash, 84000000105)
				kept = "2 rows"
			}
			later := fl.again(t, first.AddDate(0, 0, 12), true)
			later.more = fl.more
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": "inconclusive: no earlier run to compare with; this run's rows are kept for the next"})
			wantEvidence(t, report, "Q12", "{account 2}: "+kept+", kept for the next run; no earlier run to compare with")
		})
	}
	for _, tc := range []struct {
		name       string
		posted     bool // whether the fill posts after the last run, rather than before it
		want, line string
	}{
		{
			"an evening fill before the window's start, dated inside it", false,
			"no row changed, went or came late since the last run", "3 rows as they were; 0 rows new to the state",
		},
		{
			"an evening fill before the window's start, dated inside it, posted after the last run", true,
			"rows came late", "2 rows as they were; 1 row new to the state; 1 row came late, posted from 52 to 61 days after its time",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q12")
			fl.f.SetTransactionRules(byTradeDate)
			fl.more = append(fl.more, "84000000106")
			fill := fixtureRow(t, metaSale, "activityId", "84000000106",
				"time", `"2026-07-11T00:30:00+0000"`, "tradeDate", `"2026-07-11T04:00:00+0000"`)
			kept := "3 rows"
			if tc.posted {
				kept = "2 rows"
			} else {
				fl.f.AddRows(secondAccount.Hash, fill)
			}
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			wantEvidence(t, fl.report(t), "Q12", "{account 2}: "+kept+", kept for the next run; no earlier run to compare with")
			if tc.posted {
				fl.f.AddRows(secondAccount.Hash, fill)
			}
			// The window starts at 2026-07-11T02:00Z, between the fill's time
			// and its tradeDate, by which it is still served: the state keeps
			// it, or, when it posted after the last run, whose window held
			// both its times, it came late.
			later := fl.again(t, time.Date(2026, 9, 9, 2, 0, 0, 0, time.UTC), true)
			later.more = fl.more
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": tc.want})
			wantEvidence(t, report, "Q12", "{account 2}: "+tc.line)
		})
	}
	t.Run("an evening fill before the last window's start, dated inside it, new since, beside one dated after the last run", func(t *testing.T) {
		t.Parallel()
		// The last run's window starts at 2026-07-03T02:00Z, between the
		// first fill's time and its tradeDate, and the fill comes after that
		// run: a window by its time would not have held it. The second, at
		// 20:30 in New York, before that run at 22:00, and dated the next
		// midnight, comes after it too, and that run's answer held no row
		// dated after it came.
		first := time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)
		fl := newProbeLive(t, first, "Q12")
		fl.f.SetTransactionRules(byTradeDate)
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.more = append(fl.more, "84000000110", "84000000111")
		fl.f.AddRows(secondAccount.Hash,
			fixtureRow(t, metaSale, "activityId", "84000000110", "time", `"2026-07-03T00:30:00+0000"`, "tradeDate", `"2026-07-03T04:00:00+0000"`),
			fixtureRow(t, metaSale, "activityId", "84000000111", "time", `"2026-09-01T00:30:00+0000"`, "tradeDate", `"2026-09-01T04:00:00+0000"`))
		later := fl.again(t, first.Add(time.Hour), false)
		later.more = fl.more
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "no row went since the last run; undecided: whether 1 row changed, and whether 2 rows came late"})
		wantEvidence(t, report, "Q12", "{account 2}: 2 rows as they were; 2 rows new to the state; "+
			"1 row new to the state, with its time or tradeDate outside the last run's window, so not shown late; "+unserved)
	})
	t.Run("evening fills before the last run, dated the next midnight, posted after its cut answer", func(t *testing.T) {
		t.Parallel()
		// The cap leaves the XYZ dividend out of the last run's answer: the
		// cut alone leaves it undecided whether the dividend and the fill at
		// 20:30 came late, and the fill at 21:30 is too near that run to be
		// shown late.
		first := time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC) // 22:00 in New York
		fl := newProbeLive(t, first, "Q12")
		fl.f.SetTransactionRules(schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate, Cap: 1})
		fl.r.pageCap = 1
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.more = append(fl.more, "84000000105", "84000000117")
		fl.f.AddRows(secondAccount.Hash,
			fixtureRow(t, metaSale, "activityId", "84000000105", "time", at2030, "tradeDate", `"2026-08-21T04:00:00+0000"`),
			fixtureRow(t, metaSale, "activityId", "84000000117", "time", at2130, "tradeDate", `"2026-08-21T04:00:00+0000"`))
		fl.f.SetTransactionRules(byTradeDate)
		later := fl.again(t, first.AddDate(0, 0, 12), true)
		later.more = fl.more
		later.r.pageCap = MaxTransactions
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "inconclusive: undecided since the last run: " +
			"whether rows that the last run's cut answer left out changed or went, and whether 2 rows came late"})
		wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 3 rows new to the state; "+
			"1 row from the hour before the last run, too near it to be shown late; "+
			"the last run's answer may have been cut at the cap, so 2 rows new to the state cannot be shown late, nor rows that it left out shown changed or gone")
	})
}

// changesClaimed returns what a verdict of Q12's says that rows did, of
// "changed", "went" and "came late", and what it says that no row did.
func changesClaimed(verdict string) (did, none []string) {
	head, _, _ := strings.Cut(verdict, "; ")
	if found, ok := strings.CutPrefix(head, "rows "); ok {
		for k := range strings.SplitSeq(found, ", and ") {
			did = append(did, strings.TrimSuffix(k, " after they posted"))
		}
		return did, nil
	}
	if denied, ok := strings.CutPrefix(head, "no row "); ok {
		denied = strings.TrimSuffix(strings.TrimPrefix(denied, "of the accounts compared "), " since the last run")
		return nil, strings.Split(strings.ReplaceAll(denied, " or ", ", "), ", ")
	}
	return nil, nil
}

// Q12's verdict says neither that no row did what a row did that an answer
// could show, nor that rows did what none did, wherever the row lies, what
// it does between two runs and whichever answers are cut. A row of
// {account 2} stays as it was, changes, goes, or posts after the first run,
// coming late; neither answer is cut at a cap of 1, or this run's, the last
// run's or both are. The row lies inside both windows, which go by time;
// or, under windows by tradeDate, it is a Friday's evening fill at 20:30,
// dated the fourth weekday after, the latest that lastDated allows, before
// a run at 10:00 on the Saturday, in windows that drop a bound's
// milliseconds and leave out a row dated at their end, as Schwab's may; or
// a Thursday's evening fill at 21:30, dated the next midnight, before a
// run at 22:00; or its tradeDate lies before this window's start, and its
// time after; or it is an evening fill before the last window's start,
// dated inside it, before a run an hour later. Or, under windows by time,
// it is an evening fill at 20:30, dated the next midnight, before this
// window's start, which falls between the two. No answer can show that a
// row came late that posts from the hour before the last run, which Q12
// does not call late, nor one that posts across this window's start, which
// this window leaves out by its tradeDate or by its time.
func testChangesVerdictsOnTheFake(t *testing.T) {
	var byTime schwabtest.TransactionRules
	byTradeDate := schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate}
	endLeftOut := schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate, ExclusiveEnd: true, WholeSeconds: true}
	for _, p := range []struct {
		name          string
		first, second time.Time // the runs
		time, date    string    // the row's time and tradeDate
		rules         schwabtest.TransactionRules
		lateUnseen    bool // whether no answer can show that the row came late
	}{
		{
			"inside both windows", probeNow, probeNow.Add(oneDay),
			`"2026-08-22T15:00:00+0000"`, `"2026-08-22T15:00:00+0000"`, byTime, false,
		},
		{
			"a Friday's evening fill dated the fourth weekday after", time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC), time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC),
			`"2026-08-22T00:30:00+0000"`, `"2026-08-27T04:00:00+0000"`, endLeftOut, false,
		},
		{
			"an evening fill from the hour before the last run", time.Date(2026, 8, 21, 2, 0, 0, 0, time.UTC), time.Date(2026, 9, 2, 2, 0, 0, 0, time.UTC),
			`"2026-08-21T01:30:00+0000"`, `"2026-08-21T04:00:00+0000"`, byTradeDate, true,
		},
		{
			"across this window's start", probeNow, time.Date(2026, 9, 18, 10, 0, 0, 0, time.UTC),
			`"2026-07-20T14:40:00+0000"`, `"2026-07-20T04:00:00+0000"`, byTradeDate, true,
		},
		{
			"before the last window's start, dated inside it", time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC),
			`"2026-07-03T00:30:00+0000"`, `"2026-07-03T04:00:00+0000"`, byTradeDate, false,
		},
		{
			"before this window's start, dated inside it, by time", probeNow, time.Date(2026, 9, 8, 2, 0, 0, 0, time.UTC),
			`"2026-07-10T00:30:00+0000"`, `"2026-07-10T04:00:00+0000"`, byTime, true,
		},
	} {
		for _, event := range []string{"as it was", "changed", "went", "came late"} {
			for _, cut := range []string{"neither answer", "this run's answer", "the last run's answer", "both answers"} {
				t.Run(p.name+", "+event+", "+cut+" cut", func(t *testing.T) {
					t.Parallel()
					rules := p.rules
					fl := newProbeLive(t, p.first, "Q12")
					first := rules
					if cut == "the last run's answer" || cut == "both answers" {
						first.Cap, fl.r.pageCap = 1, 1
					}
					fl.f.SetTransactionRules(first)
					fl.more = append(fl.more, "84000000116")
					row := func(changes ...string) string {
						return fixtureRow(t, metaSale, append([]string{"activityId", "84000000116", "time", p.time, "tradeDate", p.date}, changes...)...)
					}
					if event != "came late" {
						fl.f.AddRows(secondAccount.Hash, row())
					}
					if err := fl.runClean(t); err != nil {
						t.Fatal(err)
					}
					switch event {
					case "changed":
						fl.f.ReplaceRow(secondAccount.Hash, 84000000116, row("netAmount", "603.37"))
					case "went":
						fl.f.RemoveRow(secondAccount.Hash, 84000000116)
					case "came late":
						fl.f.AddRows(secondAccount.Hash, row())
					}
					later := fl.again(t, p.second, true)
					later.more = fl.more
					second := rules
					later.r.pageCap = MaxTransactions
					if cut == "this run's answer" || cut == "both answers" {
						second.Cap, later.r.pageCap = 1, 1
					}
					fl.f.SetTransactionRules(second)
					if err := later.runClean(t); err != nil {
						t.Fatal(err)
					}
					verdict := verdictOf(later.report(t), "Q12")
					did, none := changesClaimed(verdict)
					if slices.ContainsFunc(did, func(k string) bool { return k != event }) {
						t.Errorf("the verdict says that rows did what none did: %s", verdict)
					}
					shown := event != "came late" || !p.lateUnseen // whether an answer could show what the row did
					if shown && slices.Contains(none, event) {
						t.Errorf("the verdict says that no row %s, though one did: %s", event, verdict)
					}
				})
			}
		}
	}
}

// keptRows returns how many rows of {account 2} the state that fl's run
// left keeps for Q12, and how many of them it marks gone.
func keptRows(t *testing.T, fl *fakeLive) (rows, gone int) {
	t.Helper()
	var st rowsState
	if _, err := fl.r.state.get("Q12", &st); err != nil {
		t.Fatal(err)
	}
	for _, e := range st.Accounts[fl.r.state.digest("account number", secondAccount.Number)].Rows {
		rows++
		if !e.Gone.IsZero() {
			gone++
		}
	}
	return rows, gone
}

// Q12 under the fake's settings: two runs, 12 days apart, the second after
// a new login, so that their windows slide, with a row changed, in a value
// and by a key it lost, a row gone and a row that came late between them,
// and a row that the second run's window no longer takes in, which is not
// shown gone, but pruned, while the state keeps the row gone, marked so;
// and two runs three days apart, between which come the rows of two fills
// before the first run that its answer left out: one two minutes before it,
// whose row may have posted in the ordinary course, which is not shown
// late, and one five hours before it, whose row came late, from five hours
// to four days after its time. Rows gone and rows that came late are
// compared too, so that the verdict comes of every row of the state gone,
// and of a row that came late to an account that had none; a run a day
// later, while those rows are gone still, shows them neither gone again nor
// missing, and one a day after that, when they have come back, one of them
// changed, compares them with the rows that the state kept, rather than
// showing them late. A row that posts while the second run's read of its
// window waits out a 429 is bounded by when the answer came, not by when
// the probe began; and a row whose time changes keeps its new time, by
// which it is shown gone three weeks later. And two runs a day apart, where
// the window of an account, or of two, fails in the second, which leaves
// all that their rows did undecided, though a row of one went, while the
// verdict speaks of the accounts compared alone, or says nothing when the
// other's window failed in the first run, which left nothing of it to
// compare with; and where an account's window fails in both runs, or in the
// first alone, before a row of it comes late, which leaves what its rows
// did undecided as well, since no earlier run read them. The first run says
// nothing either way.
func testChangesOnTheFake(t *testing.T) {
	t.Run("rows changed, gone and late", func(t *testing.T) {
		t.Parallel()
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
		fl.f.ReplaceRow(secondAccount.Hash, optionClose, fixtureRow(t, optionClose, "netAmount", "311.32", "positionId", ""))
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
		// that slid out of it is pruned, and the row gone is kept, marked so.
		if rows, gone := keptRows(t, later); rows != 4 || gone != 1 {
			t.Errorf("the state keeps %d rows of {account 2}, %d of them marked gone; want 4, 1 of them marked gone", rows, gone)
		}
		wantEvidence(t, report, "Q12", "{account 1}: 0 rows as they were; 0 rows new to the state",
			"{account 2}: 0 rows as they were; 2 rows new to the state; 1 row changed, 27 days old, in netAmount (1), positionId (1); "+
				"1 row came late, posted from 10 to 23 days after its time; 1 row gone")
	})
	t.Run("a row whose time changes", func(t *testing.T) {
		t.Parallel()
		// A row of 2026-07-20, which a day later says 2026-08-20, keeps its
		// new time: three weeks on, gone from a complete answer, it lies in
		// both runs' windows by that time, though not by its first.
		fl := newProbeLive(t, probeNow, "Q12")
		on20th := func(month time.Month) string {
			return strconv.Quote(schwabTime(time.Date(2026, month, 20, 15, 0, 0, 0, time.UTC)))
		}
		fl.more = append(fl.more, "84000000109")
		fl.f.AddRows(secondAccount.Hash, fixtureRow(t, xyzDividend, "activityId", "84000000109", "time", on20th(7), "tradeDate", on20th(7)))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.f.ReplaceRow(secondAccount.Hash, 84000000109, fixtureRow(t, xyzDividend, "activityId", "84000000109", "time", on20th(8), "tradeDate", on20th(8)))
		next := fl.again(t, probeNow.Add(oneDay), false)
		next.more = fl.more
		if err := next.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, next.report(t), map[string]string{"Q12": "rows changed after they posted"})
		fl.f.RemoveRow(secondAccount.Hash, 84000000109)
		last := fl.again(t, probeNow.AddDate(0, 0, 22), true)
		last.more = fl.more
		if err := last.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := last.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows went"})
		wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; 1 row gone")
	})
	t.Run("rows from the hours before the last run", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q12")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		midnight := strconv.Quote(schwabTime(time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC))) // the fills' day's, in New York
		fl.more = append(fl.more, "84000000107", "84000000108")
		fl.f.AddRows(secondAccount.Hash,
			fixtureRow(t, metaSale, "activityId", "84000000107", "time", strconv.Quote(schwabTime(probeNow.Add(-2*time.Minute))), "tradeDate", midnight),
			fixtureRow(t, metaSale, "activityId", "84000000108", "time", strconv.Quote(schwabTime(probeNow.Add(-5*time.Hour))), "tradeDate", midnight))
		later := fl.again(t, probeNow.AddDate(0, 0, 3), false)
		later.more = fl.more
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows came late"})
		wantEvidence(t, report, "Q12", "{account 2}: 2 rows as they were; 2 rows new to the state; "+
			"1 row came late, posted from 5 hours to 4 days after its time; 1 row from the hour before the last run, too near it to be shown late")
	})
	t.Run("every row of the state gone", func(t *testing.T) {
		t.Parallel()
		// Both of {account 2}'s rows go, and no row of the state is seen
		// again: the rows gone are the ones compared. A day later, still
		// gone, they are neither shown gone again nor missing; and when they
		// come back, one of them changed, they are compared with the rows
		// that the state kept, rather than shown late.
		fl := newProbeLive(t, probeNow, "Q12")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		dividend, option := fixtureRow(t, xyzDividend), fixtureRow(t, optionClose, "netAmount", "311.32")
		fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
		fl.f.RemoveRow(secondAccount.Hash, optionClose)
		later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows went"})
		wantEvidence(t, report, "Q12", "{account 2}: 0 rows as they were; 0 rows new to the state; 2 rows gone")
		if rows, gone := keptRows(t, later); rows != 2 || gone != 2 {
			t.Errorf("the state keeps %d rows of {account 2}, %d of them marked gone; want 2, both marked gone", rows, gone)
		}
		still := later.again(t, probeNow.AddDate(0, 0, 2), false)
		if err := still.runClean(t); err != nil {
			t.Fatal(err)
		}
		report = still.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": noRowCompared})
		wantEvidence(t, report, "Q12", "{account 2}: 0 rows as they were; 0 rows new to the state")
		fl.f.AddRows(secondAccount.Hash, dividend, option)
		back := still.again(t, probeNow.AddDate(0, 0, 3), false)
		if err := back.runClean(t); err != nil {
			t.Fatal(err)
		}
		report = back.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows changed after they posted"})
		wantEvidence(t, report, "Q12", "{account 2}: 1 row as they were; 0 rows new to the state; 1 row changed, 30 days old, in netAmount (1); "+
			"2 rows back that an earlier run showed gone")
		if rows, gone := keptRows(t, back); rows != 2 || gone != 0 {
			t.Errorf("the state keeps %d rows of {account 2}, %d of them marked gone; want 2, neither marked gone", rows, gone)
		}
	})
	t.Run("a row that came late alone", func(t *testing.T) {
		t.Parallel()
		// The last run kept no row, as of an account quiet until a row
		// dated back comes: the row of 10 days before that run, which
		// came late, is the one compared.
		fl := newProbeLive(t, probeNow, "Q12")
		fl.f.RemoveRow(secondAccount.Hash, xyzDividend)
		fl.f.RemoveRow(secondAccount.Hash, optionClose)
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		at := strconv.Quote(schwabTime(probeNow.Add(-10 * oneDay)))
		fl.more = append(fl.more, "84000000114")
		fl.f.AddRows(secondAccount.Hash, fixtureRow(t, xyzDividend, "activityId", "84000000114", "time", at, "tradeDate", at))
		later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
		later.more = fl.more
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows came late"})
		wantEvidence(t, report, "Q12", "{account 2}: 0 rows as they were; 1 row new to the state; 1 row came late, posted from 10 to 12 days after its time")
	})
	t.Run("a row that posts while its window's read waits", func(t *testing.T) {
		t.Parallel()
		// A row from 2 hours before the first run posts as Q12's read of
		// {account 2}, nearly 22 hours after that run, is sent again
		// after a 429 that asks it to wait 30 seconds: over 24 hours
		// after the row's time, past when the probe began.
		fl := newProbeLive(t, probeNow, "Q12")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		at := strconv.Quote(schwabTime(probeNow.Add(-2 * time.Hour)))
		row := fixtureRow(t, xyzDividend, "activityId", "84000000113", "time", at, "tradeDate", at)
		fl.more = append(fl.more, "84000000113")
		var arrivals atomic.Int32
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) != schwabtest.TransactionsRoute || r.PathValue("hash") != secondAccount.Hash {
				return
			}
			// The checks' read of {account 2}'s rows, Q12's, and Q12's again.
			switch arrivals.Add(1) {
			case 2:
				fl.f.Script(schwabtest.Answer(http.StatusTooManyRequests, `{"message":"a guess: slow down"}`,
					"Content-Type", "application/json", "Retry-After", "30"))
			case 3:
				fl.f.AddRows(secondAccount.Hash, row)
			}
		})
		later := fl.again(t, probeNow.Add(22*time.Hour-20*time.Second), false)
		later.more = fl.more
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := later.report(t)
		wantVerdicts(t, report, map[string]string{"Q12": "rows came late"})
		wantEvidence(t, report, "Q12", "{account 2}: 2 rows as they were; 1 row new to the state; 1 row came late, posted from 2 to 25 hours after its time")
	})
	failed := "the window of the last 60 days failed"
	// What comes between the runs: {account 2}'s XYZ dividend goes, which a
	// failed window of {account 2}'s hides, or a row of {account 3} dated 5
	// days before the first run posts, which came late.
	gone := func(fl *fakeLive) { fl.f.RemoveRow(secondAccount.Hash, xyzDividend) }
	cameLate := func(fl *fakeLive) {
		at := strconv.Quote(schwabTime(probeNow.Add(-5 * oneDay)))
		fl.more = append(fl.more, "84000000115")
		fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", "84000000115", "accountNumber", strconv.Quote(thirdAccount.Number),
			"time", at, "tradeDate", at))
	}
	for _, tc := range []struct {
		name          string
		first, second []schwabtest.FixtureAccount // those whose window fails in each run
		between       func(fl *fakeLive)
		want          string
		evidence      []string
	}{
		{
			"an account's window failed", nil,
			[]schwabtest.FixtureAccount{secondAccount},
			gone,
			"no row of the accounts compared changed, went or came late since the last run; undecided: {account 2}'s rows, whose window failed",
			[]string{"{account 2}: " + failed, "{account 3}: 1 row as they were; 0 rows new to the state"},
		},
		{
			"two accounts' windows failed", nil,
			[]schwabtest.FixtureAccount{firstAccount, secondAccount},
			gone,
			"no row of the accounts compared changed, went or came late since the last run; " +
				"undecided: {account 1}'s and {account 2}'s rows, whose windows failed",
			[]string{"{account 1}: " + failed, "{account 2}: " + failed, "{account 3}: 1 row as they were; 0 rows new to the state"},
		},
		{
			"the windows of the accounts to compare failed",
			[]schwabtest.FixtureAccount{thirdAccount},
			[]schwabtest.FixtureAccount{firstAccount, secondAccount},
			gone,
			noRowCompared,
			[]string{"{account 1}: " + failed, "{account 2}: " + failed, "{account 3}: 1 row, kept for the next run; no earlier run to compare with"},
		},
		{
			"an account's window failed in both runs",
			[]schwabtest.FixtureAccount{thirdAccount},
			[]schwabtest.FixtureAccount{thirdAccount},
			func(*fakeLive) {},
			"no row of the accounts compared changed, went or came late since the last run; undecided: {account 3}'s rows, whose window failed",
			[]string{"{account 1}: 0 rows as they were; 0 rows new to the state", "{account 2}: 2 rows as they were; 0 rows new to the state", "{account 3}: " + failed},
		},
		{
			"an account's window failed in the last run, before a row came late to it",
			[]schwabtest.FixtureAccount{thirdAccount},
			nil, cameLate,
			"no row of the accounts compared changed, went or came late since the last run; undecided: {account 3}'s rows, which no earlier run read",
			[]string{"{account 2}: 2 rows as they were; 0 rows new to the state", "{account 3}: 2 rows, kept for the next run; no earlier run to compare with"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// {account 3}'s row of 10 days ago is read in both runs, unless
			// its window fails.
			fl := newProbeLive(t, probeNow, "Q12")
			fail := func(accounts []schwabtest.FixtureAccount) {
				var mu sync.Mutex
				arrivals := map[string]int{}
				fl.f.OnAPI(func(r *http.Request) {
					if schwabtest.Route(r.Pattern) != schwabtest.TransactionsRoute {
						return
					}
					mu.Lock()
					defer mu.Unlock()
					// The checks' read of an account's rows, then Q12's.
					hash := r.PathValue("hash")
					if arrivals[hash]++; arrivals[hash] == 2 && slices.ContainsFunc(accounts, func(a schwabtest.FixtureAccount) bool { return a.Hash == hash }) {
						fl.f.Script(schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: refused"}`, "Content-Type", "application/json"))
					}
				})
			}
			at := strconv.Quote(schwabTime(probeNow.Add(-10 * oneDay)))
			fl.more = append(fl.more, "84000000112")
			fl.f.AddRows(thirdAccount.Hash, withKeys(baseTransaction, "activityId", "84000000112", "accountNumber", strconv.Quote(thirdAccount.Number),
				"time", at, "tradeDate", at))
			fail(tc.first)
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			// No earlier run read any account, whose window failed or not.
			wantVerdicts(t, fl.report(t), map[string]string{"Q12": "inconclusive: no earlier run to compare with; this run's rows are kept for the next"})
			tc.between(fl)
			fail(tc.second)
			later := fl.again(t, probeNow.AddDate(0, 0, 1), false)
			later.more = fl.more
			if err := later.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := later.report(t)
			wantVerdicts(t, report, map[string]string{"Q12": tc.want})
			wantEvidence(t, report, "Q12", tc.evidence...)
		})
	}
}

// Q15 under the fake's settings: a refresh that ends the older access
// tokens or not, and changes the refresh token or not, each verdict after
// waiting on the run's clock until the token is a minute old; a refresh
// that ends them only some seconds later; an older token refused at once
// and working again 30 seconds later, or answering 503 either time, or the
// new token refused, which no verdict may come from; three runs under one
// login, the last of which holds the refresh token against the run before
// it, not the first; a run after a new login, whose refresh token is not
// held against the last login's; and another process's refresh between the
// probe's reading of the token file and its own refresh, or a refresh that
// gives back the same access token, which no verdict may come from.
func testRefreshOnTheFake(t *testing.T) {
	for _, tc := range []struct {
		revoke, rotate bool
		old            int
		want           string
	}{
		{false, false, http.StatusOK, "a refresh leaves older access tokens working for 30s at least, and keeps the refresh token"},
		{true, false, http.StatusUnauthorized, "a refresh ends older access tokens, and keeps the refresh token"},
		{false, true, http.StatusOK, "a refresh leaves older access tokens working for 30s at least, and changes the refresh token"},
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
				fmt.Sprintf("after the refresh, the token before it answered %d, and %d when tried again 30s later; the new token answered 200", tc.old, tc.old),
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
	t.Run("a run after a new login", func(t *testing.T) {
		t.Parallel()
		// The refresh token is held against the last run's under the same
		// login only: a new login's is another token, and says nothing of
		// whether a refresh changes it.
		fl := newProbeLive(t, probeNow, "Q15")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		later := fl.again(t, probeNow.AddDate(0, 0, 7), true)
		if err := later.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, later.report(t), "Q15", "no earlier run under this login, to compare its refresh token with")
	})
	t.Run("a refresh that ends older tokens within 30s", func(t *testing.T) {
		t.Parallel()
		// The token before the refresh still works at once, and is refused
		// from 20 seconds after the new one was first used.
		fl := newProbeLive(t, probeNow, "Q15")
		older, newer := "Bearer "+schwabtest.AccessToken(1), "Bearer "+schwabtest.AccessToken(2)
		var mu sync.Mutex
		var firstNew time.Time
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) != schwabtest.AccountNumbersRoute {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch auth := r.Header.Get("Authorization"); {
			case auth == newer && firstNew.IsZero():
				firstNew = fl.clock.Now()
			case auth == older && !firstNew.IsZero() && !fl.clock.Now().Before(firstNew.Add(20*time.Second)):
				fl.f.Queue(schwabtest.AccountNumbersRoute, schwabtest.NotAuthorized)
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q15": "a refresh ends older access tokens within 30s, and keeps the refresh token"})
		wantEvidence(t, report, "Q15", "after the refresh, the token before it answered 200, and 401 when tried again 30s later; the new token answered 200")
	})
	for _, tc := range []struct {
		old, again http.HandlerFunc // what answers the token before the refresh, after it and 30s later; nil for the fake
		statuses   [2]int
	}{
		{schwabtest.NotAuthorized, nil, [2]int{http.StatusUnauthorized, http.StatusOK}},
		{nil, schwabtest.Unavailable, [2]int{http.StatusOK, http.StatusServiceUnavailable}},
		{schwabtest.NotAuthorized, schwabtest.Unavailable, [2]int{http.StatusUnauthorized, http.StatusServiceUnavailable}},
		{schwabtest.Unavailable, nil, [2]int{http.StatusServiceUnavailable, http.StatusOK}},
		{schwabtest.Unavailable, schwabtest.NotAuthorized, [2]int{http.StatusServiceUnavailable, http.StatusUnauthorized}},
	} {
		t.Run(fmt.Sprintf("an older token answering %d, then %d 30s later", tc.statuses[0], tc.statuses[1]), func(t *testing.T) {
			t.Parallel()
			// The pair says nothing: a token refused at once that works
			// again, or a 503, which says nothing of the token, either
			// time. The GETs after the refresh go once each.
			fl := newProbeLive(t, probeNow, "Q15")
			older := "Bearer " + schwabtest.AccessToken(1)
			var mu sync.Mutex
			n := 0
			fl.f.OnAPI(func(r *http.Request) {
				if schwabtest.Route(r.Pattern) != schwabtest.AccountNumbersRoute || r.Header.Get("Authorization") != older {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				// The run's read, the control, then the token after the
				// refresh, at once and 30s later.
				n++
				if h := map[int]http.HandlerFunc{3: tc.old, 4: tc.again}[n]; h != nil {
					fl.f.Queue(schwabtest.AccountNumbersRoute, h)
				}
			})
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q15": fmt.Sprintf("inconclusive: the token before the refresh answered %d, then %d 30s later",
				tc.statuses[0], tc.statuses[1])})
			wantEvidence(t, report, "Q15", fmt.Sprintf("after the refresh, the token before it answered %d, and %d when tried again 30s later; "+
				"the new token answered 200", tc.statuses[0], tc.statuses[1]))
		})
	}
	t.Run("the new token refused", func(t *testing.T) {
		t.Parallel()
		fl := newProbeLive(t, probeNow, "Q15")
		newer := "Bearer " + schwabtest.AccessToken(2)
		var refused atomic.Bool
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) == schwabtest.AccountNumbersRoute && r.Header.Get("Authorization") == newer && refused.CompareAndSwap(false, true) {
				fl.f.Queue(schwabtest.AccountNumbersRoute, schwabtest.NotAuthorized)
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q15": "inconclusive: the new token answered 401"})
		wantEvidence(t, report, "Q15", "after the refresh, the token before it answered 200, and 200 when tried again 30s later; the new token answered 401")
	})
	t.Run("three runs under one login", func(t *testing.T) {
		t.Parallel()
		// The second run's refresh changes the refresh token, and the
		// third's keeps it: the third holds it against the second's, the
		// last run under the login, not the first's.
		fl := newProbeLive(t, probeNow, "Q15")
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		fl.f.SetRotate(true)
		second := fl.again(t, probeNow.Add(time.Hour), false)
		if err := second.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, second.report(t), "Q15", "since the last run under this login, the refresh token changed")
		fl.f.SetRotate(false)
		third := second.again(t, probeNow.Add(2*time.Hour), false)
		if err := third.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantEvidence(t, third.report(t), "Q15", "since the last run under this login, the refresh token stayed the same")
	})
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
	t.Run("a refresh that gives back the same access token", func(t *testing.T) {
		t.Parallel()
		// The old and the new token would be one, and say nothing.
		fl := newProbeLive(t, probeNow, "Q15")
		saved, err := readToken(fl.tokenFile())
		if err != nil {
			t.Fatal(err)
		}
		fl.f.FailTokens(schwabtest.Failure{Handler: func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, fmt.Sprintf(`{"expires_in":1800,"token_type":"Bearer","scope":"api","refresh_token":%q,"access_token":%q}`,
				saved.RefreshToken, saved.AccessToken))
		}})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		wantVerdicts(t, fl.report(t), map[string]string{"Q15": "inconclusive: the token file does not hold the refresh's new access token, " +
			"as when another process saved one in between"})
	})
}

// An edgeTransport sends requests on through next, but answers the first
// for which deny holds with the Access Denied page of Schwab's edge
// network, and keeps what is sent after it.
type edgeTransport struct {
	next   http.RoundTripper
	page   string
	deny   func(req *http.Request) bool
	mu     sync.Mutex
	denied bool
	after  []string
}

func (e *edgeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	e.mu.Lock()
	if e.denied {
		e.after = append(e.after, req.Method+" "+req.URL.Path)
	}
	deny := !e.denied && e.deny(req)
	e.denied = e.denied || deny
	e.mu.Unlock()
	if !deny {
		return e.next.RoundTrip(req)
	}
	closeBody(req)
	resp := stubAnswer(req, http.StatusForbidden, e.page)
	resp.Header.Set("Content-Type", "text/html")
	return resp, nil
}

// An Access Denied page from Schwab's edge network stops the run at once,
// whichever request draws it: the extras' request with a garbage token,
// Q15's control with the token before its refresh, which the probes send
// themselves, and Q15's refresh, which the Auth sends. Nothing is sent
// after the page, and the probes after it do not run.
func testEdgeOnTheFake(t *testing.T) {
	var accountNumbers atomic.Int32
	for _, tc := range []struct {
		name   string
		probes []string
		deny   func(req *http.Request) bool
		notRun string
	}{
		{"the extras' garbage token", []string{"Q1", "extras", "Q15", "Q6"}, func(req *http.Request) bool {
			return req.Header.Get("Authorization") == "Bearer not-a-token"
		}, "probe extras (stopped part way), probe Q15, probe Q6"},
		{"Q15's control", []string{"Q15", "Q6"}, func(req *http.Request) bool {
			// The run's first read of the account numbers, then Q15's control.
			return req.URL.Path == "/trader/v1/accounts/accountNumbers" && accountNumbers.Add(1) == 2
		}, "probe Q15 (stopped part way), probe Q6"},
		{"Q15's refresh", []string{"Q15", "Q6"}, func(req *http.Request) bool {
			return req.Method == http.MethodPost && req.URL.Path == tokenPath
		}, "probe Q15 (stopped part way), probe Q6"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fl := newProbeLive(t, probeNow, tc.probes...)
			e := &edgeTransport{next: fl.r.auth.cfg.HTTPClient.Transport, page: string(readTestdata(t, "errors/akamai_403.html")), deny: tc.deny}
			fl.r.auth.cfg.HTTPClient = &http.Client{Transport: e}
			err := fl.runClean(t)
			if err == nil || !strings.Contains(err.Error(), "it stopped before its end: "+errEdgePage.Error()) {
				t.Errorf("the run's error: %v", err)
			}
			e.mu.Lock()
			defer e.mu.Unlock()
			if !e.denied || len(e.after) > 0 {
				t.Errorf("the page was sent: %v; and after it, %q", e.denied, e.after)
			}
			wantLines(t, fl.report(t), "## the guard", "stopped: "+errEdgePage.Error(), "did not run: "+tc.notRun)
		})
	}
}

// The extras under the fake's settings: a window that leaves out a row at
// its start, or at its end; a row whose time is finer than its JSON shows,
// which settles neither bound; a gateway that cuts each window's bounds to
// whole seconds, which settles both by whole seconds, but not with an
// exclusive end, and which, cutting one row's windows alone, makes the rows
// disagree, as a window that leaves out one row alone does, at its start or
// at its end; a gateway that rounds them, which settles both by whole
// seconds too, but with an exclusive end reads as a time finer than a row
// shows; a window of each row's bounds refused, which settles nothing; a
// first account without rows, whose next account's rows are asked about;
// windows that take a row by its tradeDate; the window around a month-end
// interest row's tradeDate holding it, as the window around its time does,
// or refused, either of which says by neither; a longest window of 366
// days; and, on an account without a month-end interest row, an evening
// fill, whose time comes before its tradeDate.
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
	notKept := "so Schwab does not keep a bound's milliseconds: it drops or rounds them"
	finer := "so either Schwab keeps a finer time than the row shows, which settles neither bound, " +
		"or it rounds a bound's milliseconds, under which by whole seconds the start is inclusive and the end exclusive"
	every := func(string) bool { return true }
	for _, tc := range []struct {
		name     string
		rules    schwabtest.TransactionRules
		finer    bool                    // with a row whose time is finer than it shows
		whole    func(query string) bool // whether a window's bounds are kept to whole seconds
		round    bool                    // rounded to them, rather than cut
		want     string
		evidence []string
	}{
		{
			"an exclusive start",
			schwabtest.TransactionRules{ExclusiveStart: true},
			false, nil, false, "a window's start is exclusive and its end inclusive; a window takes a row by its time",
			[]string{
				"a window whose start is its end, at a row's time: 200, 0 rows, without the row",
				"the bounds, row 1: the start is exclusive and the end inclusive", "the bounds, row 2: the start is exclusive and the end inclusive",
			},
		},
		{
			"an exclusive end",
			schwabtest.TransactionRules{ExclusiveEnd: true},
			false, nil, false, "a window's start is inclusive and its end exclusive; a window takes a row by its time",
			[]string{
				"a window whose start is its end, at a row's time: 200, 0 rows, without the row",
				"the bounds, row 1: the start is inclusive and the end exclusive", "the bounds, row 2: the start is inclusive and the end exclusive",
			},
		},
		{
			"a time finer than it shows",
			schwabtest.TransactionRules{},
			true, nil, false, inclusive + "; 1 row kept a finer time than it shows; a window takes a row by its time",
			[]string{
				"a window whose start is its end, at a row's time: 200, 0 rows, without the row",
				"the bounds, row 1: [t.001, t+1s] holds it and [t-1s, t.000] does not, " + finer,
				"the bounds, row 2: the start is inclusive and the end inclusive",
				"a row by its id is byte for byte its row in the list",
			},
		},
		{
			"bounds cut to whole seconds",
			schwabtest.TransactionRules{},
			false, every, false, inclusive + " by whole seconds, since Schwab does not keep a bound's milliseconds; a window takes a row by its time",
			[]string{
				"the bounds, row 1: [t-1s, t.000] and [t.001, t+1s] both hold it, " + notKept + "; by whole seconds the start is inclusive and the end inclusive",
				"the bounds, row 2: [t-1s, t.000] and [t.001, t+1s] both hold it, " + notKept + "; by whole seconds the start is inclusive and the end inclusive",
			},
		},
		{
			"bounds cut to whole seconds, and an exclusive end",
			schwabtest.TransactionRules{ExclusiveEnd: true},
			false, every, false, "the bounds: inconclusive, though Schwab drops a bound's milliseconds; a window takes a row by its time",
			[]string{
				"the bounds, row 1: [t.001, t+1s] holds it and [t-1s, t.999] does not, so Schwab drops a bound's milliseconds" +
					", and an exclusive end, or a finer time than the row shows, leaves it out of [t-1s, t]: it settles neither bound",
			},
		},
		{
			"bounds cut to whole seconds for one row alone",
			schwabtest.TransactionRules{},
			false, func(q string) bool { return strings.Contains(q, "startDate=2025-06-02T14%3A30%3A2") }, false,
			"the bounds: inconclusive, since the rows disagree; a window takes a row by its time",
			[]string{
				"the bounds, row 1: [t-1s, t.000] and [t.001, t+1s] both hold it, " + notKept + "; by whole seconds the start is inclusive and the end inclusive",
				"the bounds, row 2: the start is inclusive and the end inclusive",
			},
		},
		{
			"bounds rounded to whole seconds",
			schwabtest.TransactionRules{},
			false, every, true, inclusive + " by whole seconds, since Schwab does not keep a bound's milliseconds; a window takes a row by its time",
			[]string{
				"the bounds, row 1: [t-1s, t.000] and [t.001, t+1s] both hold it, " + notKept + "; by whole seconds the start is inclusive and the end inclusive",
				"the bounds, row 2: [t-1s, t.000] and [t.001, t+1s] both hold it, " + notKept + "; by whole seconds the start is inclusive and the end inclusive",
			},
		},
		{
			"bounds rounded to whole seconds, and an exclusive end",
			schwabtest.TransactionRules{ExclusiveEnd: true},
			false, every, true, "the bounds: inconclusive, since either a row's time is finer than it shows, or Schwab rounds a bound's milliseconds, " +
				"and by whole seconds a window's start is inclusive and its end exclusive; a window takes a row by its time",
			[]string{
				"the bounds, row 1: [t.001, t+1s] holds it and [t-1s, t.000] does not, " + finer,
				"the bounds, row 2: [t.001, t+1s] holds it and [t-1s, t.000] does not, " + finer,
			},
		},
		{
			"windows by tradeDate",
			schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate},
			false, nil, false, inclusive + "; a window takes a row by its tradeDate",
			[]string{
				"the bounds, row 1: the start is inclusive and the end inclusive",
				"the bounds, row 2: the windows that must hold it do not, so it settles nothing",
				"by time or tradeDate, of a month-end interest row: the window around its tradeDate holds it, and the window around its time does not",
			},
		},
		{
			"a longest window of 366 days",
			schwabtest.TransactionRules{MaxWindow: func(start time.Time) time.Time { return start.Add(366 * oneDay) }},
			false, nil, false, inclusive + "; a window takes a row by its time",
			[]string{"a window of 365 days and a second: 200, 0 rows", "a window of 366 days: 200, 0 rows"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "extras")
			fl.f.SetTransactionRules(tc.rules)
			if tc.whole != nil {
				// A gateway that keeps a window's bounds to whole seconds,
				// cutting their milliseconds or rounding them.
				keep := time.Time.Truncate
				if tc.round {
					keep = time.Time.Round
				}
				fl.f.OnAPI(func(r *http.Request) {
					if schwabtest.Route(r.Pattern) != schwabtest.TransactionsRoute || !tc.whole(r.URL.RawQuery) {
						return
					}
					q := r.URL.Query()
					for _, k := range []string{"startDate", "endDate"} {
						if at, err := time.Parse(schwabTimeFormat, q.Get(k)); err == nil {
							q.Set(k, keep(at, time.Second).Format(schwabTimeFormat))
						}
					}
					r.URL.RawQuery = q.Encode()
				})
			}
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
	// Answers to windows by their startDate and endDate: around the first
	// row, at 2025-06-02T14:30:27Z, and the second, the month-end interest
	// row at 2024-05-01T01:18:44Z, whose tradeDate is 2024-04-30T04:00Z.
	refused := schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: refused"}`, "Content-Type", "application/json")
	empty := schwabtest.Answer(http.StatusOK, "[]", "Content-Type", "application/json")
	for _, tc := range []struct {
		name     string
		windows  [][2]string // the startDate and endDate of each window answered so
		answer   http.HandlerFunc
		want     string
		evidence []string
	}{
		{
			// The window around its tradeDate, which holds its tradeDate
			// and not its time, answers with the row too, as if a window
			// took a row by either.
			"both windows around a month-end row holding it",
			[][2]string{{"2024-04-30T03:00:00.000Z", "2024-05-01T00:18:44.000Z"}},
			schwabtest.Answer(http.StatusOK, "["+fixtureRow(t, 73508261949)+"]", "Content-Type", "application/json"),
			inclusive + "; by time or tradeDate: inconclusive",
			[]string{"by time or tradeDate, of a month-end interest row: the window around its time holds it, and the window around its tradeDate holds it"},
		},
		{
			// One window cannot rule out a window that takes a row by
			// either time.
			"the window around a month-end row's tradeDate refused",
			[][2]string{{"2024-04-30T03:00:00.000Z", "2024-05-01T00:18:44.000Z"}},
			refused,
			inclusive + "; by time or tradeDate: inconclusive",
			[]string{"by time or tradeDate, of a month-end interest row: a window was refused: 200, 1 row; refused, HTTP 400, a guess: refused"},
		},
		{
			// [t.000, t+1s] around the second row answers [], as if its
			// start were exclusive, while the first row's shows it
			// inclusive.
			"rows that disagree",
			[][2]string{{"2024-05-01T01:18:44.000Z", "2024-05-01T01:18:45.000Z"}},
			empty,
			"the bounds: inconclusive, since the rows disagree; a window takes a row by its time",
			[]string{"the bounds, row 1: the start is inclusive and the end inclusive", "the bounds, row 2: the start is exclusive and the end inclusive"},
		},
		{
			// [t-1s, t.000] around the first row answers [], as if its end
			// were exclusive, while the second row's shows it inclusive.
			"rows that disagree on the end alone",
			[][2]string{{"2025-06-02T14:30:26.000Z", "2025-06-02T14:30:27.000Z"}},
			empty,
			"the bounds: inconclusive, since the rows disagree; a window takes a row by its time",
			[]string{"the bounds, row 1: the start is inclusive and the end exclusive", "the bounds, row 2: the start is inclusive and the end inclusive"},
		},
		{
			// [t-1s, t.000] is refused around both rows: a refusal is no
			// answer without the row, which would make the end exclusive.
			"a window of each row's bounds refused",
			[][2]string{{"2025-06-02T14:30:26.000Z", "2025-06-02T14:30:27.000Z"}, {"2024-05-01T01:18:43.000Z", "2024-05-01T01:18:44.000Z"}},
			refused,
			"the bounds: inconclusive; a window takes a row by its time",
			[]string{"the bounds, row 1: a window was refused, so it settles nothing", "the bounds, row 2: a window was refused, so it settles nothing"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fl := newProbeLive(t, probeNow, "Q1", "extras")
			fl.f.OnAPI(func(r *http.Request) {
				q := r.URL.Query()
				if slices.Contains(tc.windows, [2]string{q.Get("startDate"), q.Get("endDate")}) {
					fl.f.Script(tc.answer)
				}
			})
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"extras": tc.want})
			wantEvidence(t, report, "extras", tc.evidence...)
		})
	}
	t.Run("a first account without rows", func(t *testing.T) {
		t.Parallel()
		// {account 1}'s every window answers []: the extras ask of
		// {account 2}'s rows.
		fl := newProbeLive(t, probeNow, "Q1", "extras")
		fl.f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && r.PathValue("hash") == firstAccount.Hash {
				fl.f.Script(schwabtest.Answer(http.StatusOK, "[]", "Content-Type", "application/json"))
			}
		})
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"extras": inclusive + "; a window takes a row by its time"})
		wantEvidence(t, report, "extras", "a window whose start is its end, at a row's time: 200, 1 row, the row's among them",
			"the bounds, row 1: the start is inclusive and the end inclusive",
			"by time or tradeDate, of a row: the window around its time holds it, and the window around its tradeDate does not")
	})
	for _, tc := range []struct {
		on   schwabtest.FilterOn
		by   string
		line string
	}{
		{schwabtest.FilterOnTime, "time", "the window around its time holds it, and the window around its tradeDate does not"},
		{schwabtest.FilterOnTradeDate, "tradeDate", "the window around its tradeDate holds it, and the window around its time does not"},
	} {
		t.Run("an evening fill, windows by "+tc.by, func(t *testing.T) {
			t.Parallel()
			// {account 1} without its month-end interest row, with a fill at
			// 20:30 in New York, whose tradeDate is the next midnight.
			fl := newProbeLive(t, probeNow, "Q1", "extras")
			fl.f.SetTransactionRules(schwabtest.TransactionRules{FilterOn: tc.on})
			fl.f.RemoveRow(firstAccount.Hash, 73508261949)
			fl.f.AddRows(firstAccount.Hash, fixtureRow(t, 73508261943, "activityId", "84000000601",
				"time", `"2026-08-21T00:30:00+0000"`, "tradeDate", `"2026-08-21T04:00:00+0000"`))
			fl.more = append(fl.more, "84000000601")
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			if v := verdictOf(report, "extras"); !strings.HasSuffix(v, "; a window takes a row by its "+tc.by) {
				t.Errorf("the extras' verdict: %s", v)
			}
			wantEvidence(t, report, "extras", "by time or tradeDate, of a row: "+tc.line)
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
// session. When the read at 18:05, the first after the move, answers 70
// seconds late, the move is bounded by its answer, rounded up to 18:07.
func testBalancesOnTheFake(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slow  time.Duration
		until string
	}{{"reads that answer at once", 0, "18:05"}, {"a read that answers late", 70 * time.Second, "18:07"}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ny := newYork()
			at := func(h, m int) time.Time { return time.Date(2026, 9, 1, h, m, 0, 0, ny) }
			fl := newOptInLive(t, at(13, 0), modeBalances)
			accounts := elements(t, readTestdata(t, "accounts.json"))
			// The details of each account in each span of the watch: before
			// the close, from it, from the move, and from 20:00, its balances
			// in cents, which no output may hold.
			var spans [3][4]string
			for i, a := range accounts {
				base := 100000*(i+1) + 37 // $1000.37, $2000.37 and $3000.37
				for j, b := range [4][2]int{{base, base + 100}, {base, base + 200}, {base + 200, base + 200}, {base + 200, base + 300}} {
					initial, current := float64(b[0])/100, float64(b[1])/100
					spans[i][j] = withBalances(t, a, initial, current)
					fl.more = append(fl.more, strconv.FormatFloat(initial, 'f', -1, 64), strconv.FormatFloat(current, 'f', -1, 64))
				}
			}
			fl.f.OnAPI(func(r *http.Request) {
				if schwabtest.Route(r.Pattern) != schwabtest.AccountsRoute {
					return
				}
				now := fl.clock.Now()
				span := 0
				for _, from := range []time.Time{at(16, 0), at(18, 0), at(20, 0)} {
					if !now.Before(from) {
						span++
					}
				}
				for i, a := range schwabtest.FixtureAccounts {
					fl.f.SetDetails(a.Hash, spans[i][span])
				}
				if now.Equal(at(18, 5)) {
					fl.clock.set(now.Add(tc.slow))
				}
			})
			if err := fl.runClean(t); err != nil {
				t.Fatal(err)
			}
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q3": "the initial balances move between 17:55 and " + tc.until + " New York time, " +
				"and then equal the current total sampled from 16:00 to 16:15: yes; sampled at 20:00: no"})
			moved := "the initial balances moved between 17:55 and " + tc.until +
				" New York time; they then equal the current total sampled from 16:00 to 16:15: yes; sampled at 20:00: no"
			wantEvidence(t, report, "Q3", "110 samples read, 0 samples failed, every 10m0s from 15:45 New York time",
				"{account 1}: "+moved, "{account 2}: "+moved, "{account 3}: "+moved)
			if n := fl.f.APICalls(); n != 2+110 {
				t.Errorf("%d requests, want the account numbers, the preferences and 110 samples", n)
			}
		})
	}
}

// A watchedFill is an order of {account 2} that the watch of fills sees
// filled, on the fake: its fill, and its row, dated tradeDate, or the fill
// when that is zero, which the fake serves from after past the fill on,
// from the start when after is 0, or never when it is negative; and, if it
// is set, an earlier execution of the order, with a row of its own, dated
// tradeDate or itself, which the fake serves from as long after it.
type watchedFill struct {
	filled, tradeDate, earlier time.Time
	after                      time.Duration
}

// watchFills runs the watch of fills on the fake from start, under rules,
// with the orders and rows of fills, and returns the run.
func watchFills(t *testing.T, start time.Time, rules schwabtest.TransactionRules, fills ...watchedFill) *fakeLive {
	t.Helper()
	return watchFillsWith(t, start, rules, nil, fills...)
}

// watchFillsWith is watchFills with arrived, if it is set, called with the
// run as each request of the API arrives at the fake, before a fill's row
// comes, with the requests of transactions so far, this one included.
func watchFillsWith(t *testing.T, start time.Time, rules schwabtest.TransactionRules,
	arrived func(fl *fakeLive, r *http.Request, rows int32), fills ...watchedFill,
) *fakeLive {
	t.Helper()
	fl := newOptInLive(t, start, modeFills)
	fl.f.SetTransactionRules(rules)
	var orders []fakeOrder
	var rows []string     // the rows that come during the watch
	var comes []time.Time // and when each comes
	for i, w := range fills {
		id := 1004517700001 + int64(i)
		o := fakeOrder{account: secondAccount, id: id, entered: w.filled.Add(-time.Minute), filled: w.filled}
		executions := []time.Time{w.filled}
		if !w.earlier.IsZero() {
			o.entered, o.earlier = w.earlier.Add(-time.Minute), []time.Time{w.earlier}
			executions = append(executions, w.earlier)
		}
		orders = append(orders, o)
		fl.more = append(fl.more, strconv.FormatInt(id, 10))
		for j, at := range executions {
			activity := 84000000301 + int64(i) + 50*int64(j)
			fl.more = append(fl.more, strconv.FormatInt(activity, 10))
			tradeDate := w.tradeDate
			if tradeDate.IsZero() {
				tradeDate = at
			}
			row := fixtureRow(t, metaSale, "activityId", strconv.FormatInt(activity, 10), "orderId", strconv.FormatInt(id, 10),
				"time", strconv.Quote(schwabTime(at)), "tradeDate", strconv.Quote(schwabTime(tradeDate)))
			switch {
			case w.after == 0:
				fl.f.AddRows(secondAccount.Hash, row)
			case w.after > 0:
				rows, comes = append(rows, row), append(comes, at.Add(w.after))
			}
		}
	}
	serveOrders(fl, orders, keptFor(MaxTransactionWindow))
	added := make([]atomic.Bool, len(rows))
	var n atomic.Int32
	fl.f.OnAPI(func(r *http.Request) {
		isRows := schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute
		reads := n.Load()
		if isRows {
			reads = n.Add(1)
		}
		if arrived != nil {
			arrived(fl, r, reads)
		}
		if !isRows {
			return
		}
		for i, row := range rows {
			if !fl.clock.Now().Before(comes[i]) && added[i].CompareAndSwap(false, true) {
				fl.f.AddRows(secondAccount.Hash, row)
			}
		}
	})
	if err := fl.runClean(t); err != nil {
		t.Fatal(err)
	}
	return fl
}

// The watch of fills on the fake: an order entered four minutes into the
// watch and filled a minute later, whose row comes two and a half minutes
// after the fill, so between the polls 2 and 3 minutes after it, whether
// windows take it by its time or, dated its day's midnight, by its
// tradeDate; a fill 23 seconds past the minute, whose row comes between the
// polls 1m37s and 2m37s after it, which the verdict rounds out to whole
// minutes; a Friday evening's fill whose row, dated Monday, windows by
// tradeDate take in only past the poll, which no read could show if Schwab
// served the windows only up to the request, as no answer then shows that
// it serves the rest; the evening's before Thanksgiving, dated the Friday
// after it, and a Friday evening's dated the Thursday after, as after three
// holidays, which windows that drop a bound's milliseconds and leave out a
// row at their end take in only a second past that midnight; a row that
// comes while a slow read of another account's holds a poll up, and one
// that comes after the read that it held up, which the bounds take from
// when each read went and was answered, not from the poll's start; a fill
// in windows that leave out a row at their end, which a window must end
// after the fill to hold; an evening fill whose reads of orders fail, so
// that the reads of rows after it end at their polls and cannot show its
// row; an order filled in two parts before the watch, each part with a row
// that comes 270 seconds after it, which is timed from its first part; an
// order filled three hours before the watch, whose row is there from the
// start, and one filled at 12:30 the day before, whose row, there from the
// start too and dated that day's midnight, the reads of rows must reach
// back to, by its time or by its tradeDate, in windows that leave out a row
// at their start; a fill whose row never comes, alone, beside one whose row
// comes, and beside one whose row is there from the start; a watch whose
// every read of rows fails; and a watch in which nothing fills. The Friday
// evening's fill is watched beside another whose row never comes, which the
// reads after it could show, as the first's row shows that Schwab serves a
// window past the request; and, in windows served only up to the request,
// beside a fill earlier that day whose row never comes, and beside one
// whose row is there from the start, where the verdict says of it only that
// no read could show its row.
func testFillsOnTheFake(t *testing.T) {
	soon := probeNow.Add(5 * time.Minute) // 11:05 in New York, a Tuesday
	t.Run("a fill", func(t *testing.T) {
		t.Parallel()
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{}, watchedFill{filled: soon, after: 150 * time.Second})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day",
			"rows that came during the watch: 1, from 2 to 3 minutes after the fills")
		if n := fl.f.APICalls(); n != 2+30*2*3 {
			t.Errorf("%d requests, want the account numbers, the preferences and 30 polls of two for each account", n)
		}
	})
	t.Run("a fill off the minute", func(t *testing.T) {
		t.Parallel()
		// At 11:05:23, with its row at 11:07:53: the polls at 11:07 and
		// 11:08 bound the time from 1m37s to 2m37s.
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{}, watchedFill{filled: soon.Add(23 * time.Second), after: 150 * time.Second})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 1 to 3 minutes after the fill"})
		wantEvidence(t, report, "Q13", "rows that came during the watch: 1, from 1 to 3 minutes after the fills")
	})
	midnight := time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)     // the fill's day's, in New York
	dayBefore := time.Date(2026, 8, 31, 16, 30, 0, 0, time.UTC) // 12:30 in New York, the Monday before
	for _, by := range []struct {
		name  string
		rules schwabtest.TransactionRules
	}{
		{"time", schwabtest.TransactionRules{}},
		{"tradeDate that leave out a row at their start", schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate, ExclusiveStart: true}},
	} {
		t.Run("a fill dated its day's midnight, in windows by "+by.name, func(t *testing.T) {
			t.Parallel()
			fl := watchFills(t, probeNow, by.rules, watchedFill{filled: soon, tradeDate: midnight, after: 150 * time.Second})
			wantVerdicts(t, fl.report(t), map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill"})
		})
		t.Run("a fill the day before the watch, dated that day's midnight, in windows by "+by.name, func(t *testing.T) {
			t.Parallel()
			fl := watchFills(t, probeNow, by.rules, watchedFill{filled: dayBefore, tradeDate: midnight.AddDate(0, 0, -1)})
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q13": "inconclusive: every fill's row was there at the first read that could show it, within 1351 minutes of its fill; " +
				"place a trade just before the watch to measure it"})
			wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day", "rows there at the first read that could show them: 1, within 1351 minutes of the fills")
		})
	}
	friday := time.Date(2026, 9, 11, 23, 0, 0, 0, time.UTC) // 19:00 in New York
	monday := time.Date(2026, 9, 14, 4, 0, 0, 0, time.UTC)
	t.Run("a Friday evening's fill dated Monday, and one whose row never comes, in windows by tradeDate", func(t *testing.T) {
		t.Parallel()
		// The first fill's row, dated Monday, shows that Schwab serves a
		// window past the request, so the reads after the second fill could
		// show its row too.
		fl := watchFills(t, friday, schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate},
			watchedFill{filled: friday.Add(5 * time.Minute), tradeDate: monday, after: 150 * time.Second},
			watchedFill{filled: friday.Add(6 * time.Minute), tradeDate: monday, after: -1})
		wantVerdicts(t, fl.report(t), map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill, " +
			"though 1 other fill had none by the watch's end, 23 minutes or more after it"})
	})
	const unread = "filled orders whose rows no read after the fill could show: 1"
	for _, tc := range []struct {
		name     string
		beside   []watchedFill // fills earlier that Friday
		want     string
		evidence []string // the evidence after the polls' line
	}{
		{"", nil, "inconclusive: no read after a fill could show its row", []string{unread}},
		{
			", beside a fill whose row never comes",
			[]watchedFill{{filled: friday.Add(-70 * time.Minute), after: -1}},
			"inconclusive: no read could show the rows of 1 fill, and no other fill's row came during the watch",
			[]string{"filled orders without a row by the watch's end: 1, 99 minutes after the fills", unread},
		},
		{
			", beside a fill whose row is there from the start",
			[]watchedFill{{filled: friday.Add(-3 * time.Hour)}},
			"inconclusive: no read could show the rows of 1 fill, and no other fill's row came during the watch",
			[]string{"rows there at the first read that could show them: 1, within 181 minutes of the fills", unread},
		},
	} {
		t.Run("a Friday evening's fill dated Monday, in windows by tradeDate served only up to the request"+tc.name, func(t *testing.T) {
			t.Parallel()
			// No read holds the row before Monday, nor does an answer show
			// that Schwab serves a window past the request: no read could
			// show it, and the verdict says no more of it than that.
			fills := append([]watchedFill{{filled: friday.Add(5 * time.Minute), tradeDate: monday, after: 150 * time.Second}}, tc.beside...)
			fl := watchFills(t, friday, schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate, UpToNow: true}, fills...)
			report := fl.report(t)
			wantVerdicts(t, report, map[string]string{"Q13": tc.want})
			wantEvidence(t, report, "Q13", append([]string{"30 polls, a minute apart; " + plural(len(fills), "order") + " filled in the last day"}, tc.evidence...)...)
		})
	}
	t.Run("the evening's fill before Thanksgiving, dated the Friday, in windows by tradeDate", func(t *testing.T) {
		t.Parallel()
		wednesday := time.Date(2026, 11, 26, 0, 30, 0, 0, time.UTC) // 19:30 in New York
		friday := time.Date(2026, 11, 27, 5, 0, 0, 0, time.UTC)
		fl := watchFills(t, wednesday, schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate},
			watchedFill{filled: wednesday.Add(5 * time.Minute), tradeDate: friday, after: 150 * time.Second})
		wantVerdicts(t, fl.report(t), map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill"})
	})
	t.Run("a Friday evening's fill dated the fourth weekday after, in windows by tradeDate that drop a bound's milliseconds and leave out a row at their end", func(t *testing.T) {
		t.Parallel()
		// Dated Thursday, as after three holidays, the latest that
		// lastDated allows: a window must end a second past that midnight
		// to hold it, not a millisecond, which these windows drop.
		friday := time.Date(2026, 9, 11, 23, 0, 0, 0, time.UTC) // 19:00 in New York
		thursday := time.Date(2026, 9, 17, 4, 0, 0, 0, time.UTC)
		fl := watchFills(t, friday, schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate, ExclusiveEnd: true, WholeSeconds: true},
			watchedFill{filled: friday.Add(5 * time.Minute), tradeDate: thursday, after: 150 * time.Second})
		wantVerdicts(t, fl.report(t), map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill"})
	})
	t.Run("a row that comes while a slow read holds a poll up", func(t *testing.T) {
		t.Parallel()
		// {account 1}'s read of rows at the poll of 11:08, the 25th, takes 50
		// seconds, and the row comes at 11:08:40, 3m40s after the fill, so
		// {account 2}'s read at 11:08:50 holds it: from 2 to 4 minutes, as
		// the row was not there at 11:08, 3 minutes after the fill.
		slow := func(fl *fakeLive, r *http.Request, rows int32) {
			if rows == 25 && schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute {
				fl.clock.set(fl.clock.Now().Add(50 * time.Second))
			}
		}
		fl := watchFillsWith(t, probeNow, schwabtest.TransactionRules{}, slow, watchedFill{filled: soon, after: 220 * time.Second})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 2 to 4 minutes after the fill"})
		wantEvidence(t, report, "Q13", "rows that came during the watch: 1, from 2 to 4 minutes after the fills")
	})
	t.Run("a read without the row that a slow read holds up", func(t *testing.T) {
		t.Parallel()
		// The fill at 11:04:30, and {account 1}'s read of rows at the poll
		// of 11:08 taking 50 seconds, so that {account 2}'s goes at 11:08:50,
		// 4m20s after the fill, before the row comes at 11:08:55: from 4 to
		// 5 minutes, not from 3, as the poll's start would make it.
		slow := func(fl *fakeLive, r *http.Request, rows int32) {
			if rows == 25 && schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute {
				fl.clock.set(fl.clock.Now().Add(50 * time.Second))
			}
		}
		fl := watchFillsWith(t, probeNow, schwabtest.TransactionRules{}, slow, watchedFill{filled: soon.Add(-30 * time.Second), after: 265 * time.Second})
		wantVerdicts(t, fl.report(t), map[string]string{"Q13": "a fill's trade appears from 4 to 5 minutes after the fill"})
	})
	t.Run("a fill, in windows that leave out a row at their end", func(t *testing.T) {
		t.Parallel()
		// The row's time is the fill's, so a window must end after the
		// fill, at its read, to hold it.
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{ExclusiveEnd: true}, watchedFill{filled: soon, after: 150 * time.Second})
		wantVerdicts(t, fl.report(t), map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill"})
	})
	t.Run("an evening fill whose reads of orders fail, in windows by tradeDate", func(t *testing.T) {
		t.Parallel()
		// At 19:05:20, dated the next midnight, its row 30 seconds later.
		// {account 2}'s reads of orders fail from 19:06 to 19:08, so its
		// reads of rows then end at the poll, and cannot show the row: they
		// say nothing of it, and the row came from 0 to 3 minutes after the
		// fill, not from 1.
		evening := time.Date(2026, 9, 1, 23, 0, 0, 0, time.UTC)
		orders := "/trader/v1/accounts/" + secondAccount.Hash + "/orders"
		failing := func(fl *fakeLive, r *http.Request, _ int32) {
			if now := fl.clock.Now(); r.URL.Path == orders && !now.Before(evening.Add(6*time.Minute)) && now.Before(evening.Add(8*time.Minute)) {
				fl.f.Script(schwabtest.Answer(http.StatusServiceUnavailable, `{"message":"a guess: down"}`, "Content-Type", "application/json"))
			}
		}
		fl := watchFillsWith(t, evening, schwabtest.TransactionRules{FilterOn: schwabtest.FilterOnTradeDate}, failing,
			watchedFill{filled: evening.Add(5*time.Minute + 20*time.Second), tradeDate: time.Date(2026, 9, 2, 4, 0, 0, 0, time.UTC), after: 30 * time.Second})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 0 to 3 minutes after the fill"})
		wantEvidence(t, report, "Q13", "reads that failed: 2 of the orders, 0 of the rows", "rows that came during the watch: 1, from 0 to 3 minutes after the fills")
	})
	t.Run("an order filled in two parts before the watch", func(t *testing.T) {
		t.Parallel()
		// At 10:56:00 and 10:59:40, each part's row 270 seconds after it, the
		// first's at 11:00:30 and the last's at 11:04:10: timed from the
		// first part, whose row comes first, between the polls 4 and 5
		// minutes after it.
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{},
			watchedFill{filled: probeNow.Add(-20 * time.Second), earlier: probeNow.Add(-4 * time.Minute), after: 270 * time.Second})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 4 to 5 minutes after the fill"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day", "rows that came during the watch: 1, from 4 to 5 minutes after the fills")
	})
	t.Run("a fill hours before the watch", func(t *testing.T) {
		t.Parallel()
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{}, watchedFill{filled: probeNow.Add(-3 * time.Hour)})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "inconclusive: every fill's row was there at the first read that could show it, within 181 minutes of its fill; " +
			"place a trade just before the watch to measure it"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day", "rows there at the first read that could show them: 1, within 181 minutes of the fills")
	})
	t.Run("a fill whose row never comes", func(t *testing.T) {
		t.Parallel()
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{}, watchedFill{filled: soon, after: -1})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "no fill's trade appeared by the watch's end, 24 minutes after the fills"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day",
			"filled orders without a row by the watch's end: 1, 24 minutes after the fills")
	})
	t.Run("a fill whose row comes, and one whose row never comes", func(t *testing.T) {
		t.Parallel()
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{},
			watchedFill{filled: soon, after: 150 * time.Second}, watchedFill{filled: soon.Add(time.Minute), after: -1})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "a fill's trade appears from 2 to 3 minutes after the fill, " +
			"though 1 other fill had none by the watch's end, 23 minutes or more after it"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 2 orders filled in the last day",
			"rows that came during the watch: 1, from 2 to 3 minutes after the fills", "filled orders without a row by the watch's end: 1, 23 minutes after the fills")
	})
	t.Run("a fill's row there from the start, and one that never comes", func(t *testing.T) {
		t.Parallel()
		fl := watchFills(t, probeNow, schwabtest.TransactionRules{},
			watchedFill{filled: probeNow.Add(-3 * time.Hour)}, watchedFill{filled: soon, after: -1})
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "inconclusive: no fill's row came during the watch: " +
			"some were there at the first read that could show them, and the others had not come by its end"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 2 orders filled in the last day",
			"rows there at the first read that could show them: 1, within 181 minutes of the fills", "filled orders without a row by the watch's end: 1, 24 minutes after the fills")
	})
	t.Run("reads of rows that fail", func(t *testing.T) {
		t.Parallel()
		fl := newOptInLive(t, probeNow, modeFills)
		serveOrders(fl, []fakeOrder{{account: secondAccount, id: 1004517700001, entered: soon.Add(-time.Minute), filled: soon}}, keptFor(MaxTransactionWindow))
		fl.more = append(fl.more, "1004517700001")
		fl.f.Override(schwabtest.TransactionsRoute, schwabtest.Answer(http.StatusBadRequest, `{"message":"a guess: no rows now"}`))
		if err := fl.runClean(t); err != nil {
			t.Fatal(err)
		}
		report := fl.report(t)
		wantVerdicts(t, report, map[string]string{"Q13": "inconclusive: no read after a fill could show its row"})
		wantEvidence(t, report, "Q13", "30 polls, a minute apart; 1 order filled in the last day", "reads that failed: 0 of the orders, 90 of the rows",
			"filled orders whose rows no read after the fill could show: 1")
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
// step, which stops it; answers of a second and a half, which keep the
// step of 60 a minute from its pace, and stop it, and answers that slow
// only the burst, which stop it too; and the refusals to start, of a rate
// file written a minute ago and of a scheduled snapshot that the run would
// come near, and the stop when the snapshot nears in the middle of the
// run, as when the computer slept.
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
		{"slow answers", func(_ *testing.T, fl *fakeLive) string {
			onAccountNumbers(fl, func(int32) { fl.clock.set(fl.clock.Now().Add(1500 * time.Millisecond)) })
			return ""
		}, "inconclusive: the answers came too slowly to keep the pace: request 12 of 60 would go 17s after the first, 6s behind its place, " +
			"at 60 a minute; no 429 came up to 40 a minute", ""},
		{"slow answers in the burst", func(_ *testing.T, fl *fakeLive) string {
			onAccountNumbers(fl, func(n int32) {
				if n > 1+20+40+60+80+100 {
					fl.clock.set(fl.clock.Now().Add(1500 * time.Millisecond))
				}
			})
			return ""
		}, "inconclusive: the answers came too slowly to keep the pace: request 4 of 5 would go 5s after the first, " +
			"at the burst of 5 after 2m0s of quiet; no 429 came up to 100 a minute", ""},
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
