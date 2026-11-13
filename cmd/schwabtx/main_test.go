package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The app that the fake takes.
const (
	testKey      = schwabtest.AppKey
	testSecret   = schwabtest.AppSecret
	testCallback = schwabtest.CallbackURL
)

// testRefresh is the refresh token of a token file that a test writes by
// hand, for schwabtx run in a process of its own; the fake takes it once
// AllowRefresh is told of it. A login gets the fake's own tokens.
const testRefresh = "refresh-token-1"

// fakeSchwab is the fake Schwab of package schwabtest, which the CLI's tests
// drive, with what they gave it kept beside it: each account's rows, from
// which expected works out what the tool should print, and the parts of the
// accounts' details that the snapshot tests change. Accounts and rows are
// added, changed and removed through its own methods, so that it keeps
// step with the fake, and only the test's goroutine calls those; the rest
// is the Server's, which a hook may call too.
type fakeSchwab struct {
	*schwabtest.Server
	t        *testing.T
	accounts []*fakeAccount // in the order they were added
	// The parts of every account's details that a test can set: its
	// positions, in place of fakePositions; its currentBalances; and its
	// accountNumber, in place of its number as a string.
	positionsJSON, balancesJSON, numberJSON string
}

// A fakeAccount is an account that a test gave the fake.
type fakeAccount struct {
	number  string
	details string // its element of GET /accounts, as the fake holds it
	rows    []fakeRow
}

// A fakeRow is a transaction that a test gave the fake, and its handle on
// the fake's copy.
type fakeRow struct {
	fakeTx
	ref schwabtest.RowRef
}

// A fakeTx is a transaction as a test gave it: its id and time, by which
// expected ranks it, and the JSON the fake sends for it.
type fakeTx struct {
	ID   int64
	When time.Time
	JSON string
}

// newFakeSchwab starts a fake Schwab without accounts, which the end of the
// test closes.
func newFakeSchwab(t *testing.T) *fakeSchwab {
	return &fakeSchwab{Server: schwabtest.New(t), t: t}
}

// fakePositions are the positions of every fake account, with the keys
// that Schwab sends on every position, and the open profit or loss that it
// sends on every long one. The "&", the line separator U+2028 and the
// numbers such as 250.0 show whether a snapshot keeps Schwab's text
// exactly.
const fakePositions = `{"shortQuantity":0,"averagePrice":246.91,"currentDayProfitLoss":0,"currentDayProfitLossPercentage":0,` +
	`"longQuantity":5,"settledLongQuantity":5,"settledShortQuantity":0,` +
	`"instrument":{"assetType":"COLLECTIVE_INVESTMENT","symbol":"VTI","description":"VANGUARD TOTAL STOCK MARKET ETF"},` +
	`"marketValue":1300.5,"maintenanceRequirement":0,"longOpenProfitLoss":65.95,"currentDayCost":0},` +
	`{"shortQuantity":0,"averagePrice":17.5,"currentDayProfitLoss":0.0,"currentDayProfitLossPercentage":0.0,` +
	`"longQuantity":10,"settledLongQuantity":10,"settledShortQuantity":0,` +
	`"instrument":{"assetType":"EQUITY","symbol":"T","description":"AT&T` + "\u2028" + `INC"},` +
	`"marketValue":250.0,"maintenanceRequirement":0.0,"longOpenProfitLoss":75.0,"currentDayCost":0.0}`

// details returns the element of GET /accounts for the account with the
// given number: the keys that Schwab sends on every account, and its
// positions, which the fake leaves out unless fields=positions asks for
// them, as Schwab does. It is written by hand, so that tests can check that
// the tool keeps it as sent, and indented, so that they can check that the
// tool puts it on one line.
func (f *fakeSchwab) details(number string) string {
	f.t.Helper()
	positions := fakePositions
	if f.positionsJSON != "" {
		positions = f.positionsJSON
	}
	balances := `{"liquidationValue":12345.60,"cashBalance":100.0}`
	if f.balancesJSON != "" {
		balances = f.balancesJSON
	}
	accountNumber := strconv.Quote(number)
	if f.numberJSON != "" {
		accountNumber = f.numberJSON
	}
	compact := fmt.Sprintf(`{"securitiesAccount":{"type":"MARGIN","accountNumber":%s,"roundTrips":0,"isDayTrader":false,`+
		`"isClosingOnlyRestricted":false,"pfcbFlag":false,"positions":[%s],`+
		`"initialBalances":{"liquidationValue":12345.60,"cashBalance":100.0},`+
		`"currentBalances":%s,"projectedBalances":{"cashAvailableForTrading":100.0}},`+
		`"aggregatedBalance":{"currentLiquidationValue":12345.60,"liquidationValue":12345.60}}`,
		accountNumber, positions, balances)
	var indented bytes.Buffer
	if err := json.Indent(&indented, []byte(compact), "", "  "); err != nil {
		f.t.Fatalf("fake Schwab: indenting an account's details: %v", err)
	}
	return indented.String()
}

// setPositionsJSON gives every account the positions in js, a list's
// elements, in place of fakePositions.
func (f *fakeSchwab) setPositionsJSON(js string) {
	f.t.Helper()
	f.positionsJSON = js
	f.redetail()
}

// setBalancesJSON gives every account js as its currentBalances.
func (f *fakeSchwab) setBalancesJSON(js string) {
	f.t.Helper()
	f.balancesJSON = js
	f.redetail()
}

// setNumberJSON sends js, such as a number, as every account's
// accountNumber in GET /accounts.
func (f *fakeSchwab) setNumberJSON(js string) {
	f.t.Helper()
	f.numberJSON = js
	f.redetail()
}

// redetail gives every account its details again, once a part of them has
// changed. Each keeps its place, its hash and its rows.
func (f *fakeSchwab) redetail() {
	f.t.Helper()
	for _, a := range f.accounts {
		a.details = f.details(a.number)
		f.SetDetails(f.hash(a.number), a.details)
	}
}

// detailsBody returns what a snapshot of the fake's accounts should hold:
// Schwab's answer to GET /accounts?fields=positions, on one line. The fake
// keeps no bodies, so it is worked out from the details it was given.
func (f *fakeSchwab) detailsBody() string {
	f.t.Helper()
	ds := make([]string, len(f.accounts))
	for i, a := range f.accounts {
		ds[i] = a.details
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte("["+strings.Join(ds, ",")+"]")); err != nil {
		f.t.Fatalf("fake Schwab: compacting the accounts' details: %v", err)
	}
	return b.String()
}

// addAccount gives the fake an account with count transactions, the newest at
// newest and each one step older than the last.
func (f *fakeSchwab) addAccount(number string, idBase int64, count int, newest time.Time, step time.Duration) {
	f.t.Helper()
	a := &fakeAccount{number: number, details: f.details(number)}
	f.AddAccount(schwabtest.Account{Number: number, Details: a.details})
	f.accounts = append(f.accounts, a)
	txs := make([]fakeTx, count)
	for i := range count {
		when := newest.Add(-time.Duration(i) * step)
		id := idBase + int64(count-i)
		js := fmt.Sprintf(`{"activityId":%d,"time":%q,"accountNumber":%q,"type":"TRADE","status":"VALID","subAccount":"CASH",`+
			`"tradeDate":%q,"netAmount":-1234.56,"description":"",`+
			`"transferItems":[{"instrument":{"assetType":"CURRENCY","symbol":"CURRENCY_USD"},"amount":0.01,"cost":-0.01,"feeType":"SEC_FEE"},`+
			`{"instrument":{"assetType":"EQUITY","symbol":"VTI","description":"VANGUARD TOTAL STOCK MARKET ETF"},"amount":5,"cost":-1234.55,"price":246.91,"positionEffect":"OPENING"}]}`,
			id, when.UTC().Format("2006-01-02T15:04:05-0700"), number, when.UTC().Format("2006-01-02T15:04:05-0700"))
		txs[i] = fakeTx{ID: id, When: when, JSON: js}
	}
	f.addRows(number, txs...)
}

// addRows gives the account with the given number the transactions txs,
// after its others. The fake reads each one's id, time and type from its
// JSON.
func (f *fakeSchwab) addRows(number string, txs ...fakeTx) {
	f.t.Helper()
	a := f.account(number)
	js := make([]string, len(txs))
	for i, tx := range txs {
		js[i] = tx.JSON
	}
	refs := f.AddRows(f.hash(number), js...)
	if len(refs) != len(txs) {
		f.t.Fatalf("fake Schwab: account %s took %d of %d rows", number, len(refs), len(txs))
	}
	for i, tx := range txs {
		a.rows = append(a.rows, fakeRow{fakeTx: tx, ref: refs[i]})
	}
}

// account returns the account with the given number, failing the test if
// the fake has none.
func (f *fakeSchwab) account(number string) *fakeAccount {
	f.t.Helper()
	for _, a := range f.accounts {
		if a.number == number {
			return a
		}
	}
	f.t.Fatalf("fake Schwab: no account %s", number)
	return nil
}

// hash returns the hash of the account with the given number, by which the
// endpoints for one account take it.
func (f *fakeSchwab) hash(number string) string { return schwabtest.HashFor(number) }

// rows returns the transactions of the account with the given number, in
// the order they were added: newest first, for addAccount's.
func (f *fakeSchwab) rows(number string) []fakeTx {
	f.t.Helper()
	a := f.account(number)
	txs := make([]fakeTx, len(a.rows))
	for i, r := range a.rows {
		txs[i] = r.fakeTx
	}
	return txs
}

// setRow replaces the JSON of the ith transaction of the account with the
// given number, which keeps its place. expected goes on ranking it by the
// id and time it was added with, so js should keep its time.
func (f *fakeSchwab) setRow(number string, i int, js string) {
	f.t.Helper()
	r := &f.account(number).rows[i]
	r.ref.Replace(schwabtest.Row{JSON: js})
	r.JSON = js
}

// removeAccount takes the account with the given number out of the fake,
// with its rows, as when it was not ticked at the last login.
func (f *fakeSchwab) removeAccount(number string) {
	f.t.Helper()
	f.RemoveAccount(f.hash(number))
	f.accounts = slices.DeleteFunc(f.accounts, func(a *fakeAccount) bool { return a.number == number })
}

// expected returns the ids of the n most recent transactions newer than cutoff.
func (f *fakeSchwab) expected(n int, cutoff time.Time) []string {
	var all []fakeTx
	for _, a := range f.accounts {
		for _, r := range a.rows {
			if !r.When.Before(cutoff) {
				all = append(all, r.fakeTx)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].When.Equal(all[j].When) {
			return all[i].When.After(all[j].When)
		}
		return all[i].ID > all[j].ID
	})
	if len(all) > n {
		all = all[:n]
	}
	ids := make([]string, len(all))
	for i, tx := range all {
		ids[i] = strconv.FormatInt(tx.ID, 10)
	}
	return ids
}

// queries returns the query of each request to route that the fake was
// sent with a working access token, in the order they came.
func (f *fakeSchwab) queries(route schwabtest.Route) []url.Values {
	f.t.Helper()
	var qs []url.Values
	for _, r := range f.Requests() {
		if r.Route != route {
			continue
		}
		q, err := url.ParseQuery(r.Query)
		if err != nil {
			f.t.Fatalf("fake Schwab: the query of %s: %v", r.URL, err)
		}
		qs = append(qs, q)
	}
	return qs
}

// atSecondTransactions calls do as the second request for transactions
// arrives, before the fake looks at it, so that the walk goes wrong part way
// through: a walk that failed on its first window would end at once, before
// the checks that come after a window worked. do runs on one of the fake's
// goroutines, so it may call only the Server.
func (f *fakeSchwab) atSecondTransactions(do func()) {
	var n atomic.Int32
	f.OnAPI(func(r *http.Request) {
		if schwabtest.Route(r.Pattern) == schwabtest.TransactionsRoute && n.Add(1) == 2 {
			do()
		}
	})
}

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*fakeSchwab, string) {
	f := newFakeSchwab(t)
	tokenFile := filepath.Join(t.TempDir(), "cfg", "token.json")
	t.Setenv("SCHWAB_APP_KEY", testKey)
	t.Setenv("SCHWAB_APP_SECRET", testSecret)
	t.Setenv("SCHWAB_CALLBACK_URL", "")
	t.Setenv("SCHWAB_BASE_URL", f.URL())
	t.Setenv("SCHWAB_TOKEN_FILE", tokenFile)
	t.Setenv("SCHWAB_SNAPSHOT_FILE", filepath.Join(filepath.Dir(tokenFile), "snapshots.jsonl"))
	// Keep the user config directory in the test's temp space too, so that
	// nothing can reach the real one.
	home := t.TempDir()
	t.Setenv("HOME", home)            // macOS
	t.Setenv("XDG_CONFIG_HOME", home) // Linux and other Unix systems
	t.Setenv("AppData", home)         // Windows
	// And the user cache directory, which holds the rate limit.
	t.Setenv("XDG_CACHE_HOME", home)
	t.Setenv("LocalAppData", home)
	timeNow = func() time.Time { return testNow }
	t.Cleanup(func() { timeNow = time.Now })
	// Tests make many requests, so let them go fast, and try again at once.
	clientConfig = schwab.ClientConfig{RatePerMinute: 60_000, Burst: 100, Backoff: time.Millisecond}
	t.Cleanup(func() { clientConfig = schwab.ClientConfig{} })
	return f, tokenFile
}

// doLogin runs `schwabtx login` as a browser would have it: it opens the
// printed link at the fake's authorize page, which signs in and approves at
// once, and pastes back the address the page sends the browser on to. It
// returns what login printed.
func doLogin(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), []string{"login"}, pr, &syncWriter{w: &out}, &syncWriter{w: &out})
	}()

	// The state is random, so read it out of the printed link first.
	prompt := waitForPrompt(t, &out, done)
	i := strings.Index(prompt, "http")
	if i < 0 {
		t.Fatalf("no login link printed: %s", prompt)
	}
	link := strings.Fields(prompt[i:])[0]
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/v1/oauth/authorize" || q.Get("client_id") != testKey || q.Get("redirect_uri") != testCallback || q.Get("state") == "" {
		t.Fatalf("bad authorize link: %s", link)
	}
	fmt.Fprintln(pw, authorize(t, link))
	pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("login failed: %v", err)
	}
	outMu.Lock()
	defer outMu.Unlock()
	return out.String()
}

// authorize opens link, where a login starts, and returns the address that
// the fake's authorize page sends the browser on to: the callback, with the
// code and the state. The browser stops there, since nothing answers at the
// callback.
func authorize(t *testing.T, link string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, link, nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := browser.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("the authorize page answered %s, not a redirect", resp.Status)
	}
	return resp.Header.Get("Location")
}

// waitForPrompt returns login's output once it is waiting for the pasted
// address, failing the test if login returns or stalls first.
func waitForPrompt(t *testing.T, out *bytes.Buffer, done <-chan error) string {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		outMu.Lock()
		s := out.String()
		outMu.Unlock()
		if strings.Contains(s, "> ") {
			return s
		}
		select {
		case err := <-done:
			t.Fatalf("login returned before prompting: %v\n%s", err, s)
		case <-deadline:
			t.Fatalf("login never prompted: %s", s)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

var outMu sync.Mutex

// syncWriter lets the test read the login prompt while login is still running.
type syncWriter struct{ w *bytes.Buffer }

func (s *syncWriter) Write(p []byte) (int, error) {
	outMu.Lock()
	defer outMu.Unlock()
	return s.w.Write(p)
}

func runTx(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var o, e bytes.Buffer
	err = run(context.Background(), args, strings.NewReader(""), &o, &e)
	return o.String(), e.String(), err
}

func idsFromJSON(t *testing.T, out string) []string {
	t.Helper()
	var txs []struct {
		ActivityID json.Number `json:"activityId"`
	}
	if err := json.Unmarshal([]byte(out), &txs); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	ids := make([]string, len(txs))
	for i, tx := range txs {
		ids[i] = tx.ActivityID.String()
	}
	return ids
}

func equal(a, b []string) bool {
	return strings.Join(a, ",") == strings.Join(b, ",")
}

func TestLoginThenMostRecent100AcrossAccounts(t *testing.T) {
	f, tokenFile := setup(t)
	// 150 trades two days apart, 30 trades ten days apart, plus one sitting
	// exactly on a 60-day window boundary to exercise de-duplication.
	f.addAccount("11112222", 1000, 150, testNow.Add(-time.Hour), 48*time.Hour)
	f.addAccount("33334444", 5000, 30, testNow.Add(-3*time.Hour), 240*time.Hour)
	f.addAccount("55556666", 9000, 1, testNow.Add(-60*24*time.Hour), time.Hour)
	// Schwab was reported once to refuse a request without types, so the
	// tool names them on every request.
	f.SetTransactionRules(schwabtest.TransactionRules{RequireTypes: true})

	if _, _, err := runTx(t); err == nil || !strings.Contains(err.Error(), "schwabtx login") {
		t.Fatalf("expected a not-logged-in error, got %v", err)
	}

	login := doLogin(t)
	if code, _ := f.Grants(); code != 1 {
		t.Fatalf("code grants = %d, want 1", code)
	}
	info, err := os.Stat(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits to check.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
	}

	out, _, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	got := idsFromJSON(t, out)
	want := f.expected(100, testNow.Add(-365*24*time.Hour))
	if len(got) != 100 || !equal(got, want) {
		t.Fatalf("wrong transactions\n got %v\nwant %v", got, want)
	}
	if _, refresh := f.Grants(); refresh != 0 {
		t.Fatalf("refreshed a still-valid access token %d times", refresh)
	}

	// The table shows the security leg, not the fee leg, and masks accounts.
	table, stderr, err := runTx(t)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(table), "\n")
	if len(lines) != 101 {
		t.Fatalf("table has %d lines, want header + 100", len(lines))
	}
	for _, want := range []string{"VTI", "246.91", "-1234.56", "...222", "TRADE", "VANGUARD TOTAL STOCK MARKET ETF"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("first row %q is missing %q", lines[1], want)
		}
	}
	if strings.Contains(table, "11112222") {
		t.Error("table shows a full account number without -show-accounts")
	}
	if !strings.Contains(stderr, "100 transactions from 3 accounts") {
		t.Errorf("unexpected summary: %q", stderr)
	}
	// The app secret, the codes, and every token the fake issued, as they
	// are or escaped as in an address, such as the pasted one.
	for _, secret := range f.Secrets() {
		if all := login + table + stderr + out; strings.Contains(all, secret) || strings.Contains(all, url.QueryEscape(secret)) {
			t.Errorf("output leaks %q", secret)
		}
	}
}

func TestSmallNAndFewTransactions(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 7, testNow.Add(-time.Hour), 24*time.Hour)
	f.addAccount("33334444", 5000, 0, testNow, time.Hour)
	doLogin(t)

	out, _, err := runTx(t, "-json", "-n", "3")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := idsFromJSON(t, out), f.expected(3, time.Time{}); !equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Fewer than 100 exist: print them all rather than fail.
	out, _, err = runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	if got := idsFromJSON(t, out); len(got) != 7 {
		t.Fatalf("got %d transactions, want 7", len(got))
	}
}

func TestExpiredAccessTokenIsRefreshed(t *testing.T) {
	f, tokenFile := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	// Whether a refresh ends Schwab's older access tokens is not known, so
	// the tool must use the new one.
	f.SetRevokeOlder(true)

	// 31 minutes later the access token has lapsed.
	timeNow = func() time.Time { return testNow.Add(31 * time.Minute) }
	if _, _, err := runTx(t, "-json"); err != nil {
		t.Fatal(err)
	}
	if _, refresh := f.Grants(); refresh != 1 {
		t.Fatalf("refresh grants = %d, want 1", refresh)
	}
	b, _ := os.ReadFile(tokenFile)
	if !strings.Contains(string(b), schwabtest.AccessToken(2)) {
		t.Fatalf("refreshed token was not saved: %s", b)
	}
}

func TestRevokedAccessTokenIsRefreshedOn401(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	f.RevokeAccess()

	// Schwab refusing a token it has only just issued is not the token's
	// fault, so the tool reports it rather than refresh.
	if _, _, err := runTx(t, "-json"); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("a 401 right after the login: %v, want Schwab's HTTP 401", err)
	}
	if _, refresh := f.Grants(); refresh != 0 {
		t.Fatalf("refresh grants = %d, want 0", refresh)
	}

	// Ten minutes later, the fake's refresh handler issues a fresh token, so
	// one retry fixes it.
	timeNow = func() time.Time { return testNow.Add(10 * time.Minute) }
	if _, _, err := runTx(t, "-json"); err != nil {
		t.Fatal(err)
	}
	if _, refresh := f.Grants(); refresh != 1 {
		t.Fatalf("refresh grants = %d, want 1", refresh)
	}
}

func TestExpiredRefreshTokenAsksForLogin(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)

	// Eight days later Schwab has ended the login.
	f.EndLogin()
	timeNow = func() time.Time { return testNow.Add(8 * 24 * time.Hour) }

	_, _, err := runTx(t)
	if err == nil || !strings.Contains(err.Error(), "schwabtx login") {
		t.Fatalf("expected a re-login error, got %v", err)
	}
}

// A window whose answer holds pageCap rows or more may have been cut short,
// so it is split until no answer does. The newest 100 come back whichever
// rows Schwab keeps at its cap, the newest, as seen live, or the oldest, and
// however the cap meets a request for several types, which is not known: an
// answer can then be cut short at the cap or hold more than it.
func TestTruncatedResponsesAreSplit(t *testing.T) {
	// The types of the rows in turn. Trades outnumber the others, so that a
	// cap on each type's rows leaves out some of the newest rows too.
	several := []string{"TRADE", "TRADE", "DIVIDEND_OR_INTEREST", "TRADE", "JOURNAL"}
	for _, tc := range []struct {
		name   string
		types  []string // the types of the rows in turn; TRADE alone when nil
		mode   schwabtest.CapMode
		oldest bool // the oldest rows are kept at the cap
	}{
		{"newest kept", nil, schwabtest.NewestAcrossTypes, false},
		{"oldest kept", nil, schwabtest.NewestAcrossTypes, true},
		{"several types, newest kept across them", several, schwabtest.NewestAcrossTypes, false},
		{"several types, oldest kept across them", several, schwabtest.NewestAcrossTypes, true},
		{"several types, cut in type order", several, schwabtest.InTypeOrder, false},
		{"several types, cut in type order, oldest first", several, schwabtest.InTypeOrder, true},
		{"several types, each capped", several, schwabtest.PerType, false},
		{"several types, each capped, oldest kept", several, schwabtest.PerType, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := setup(t)
			f.addAccount("11112222", 1000, 300, testNow.Add(-time.Hour), 6*time.Hour) // 240 per 60-day window
			if tc.types != nil {
				for i, tx := range f.rows("11112222") {
					f.setRow("11112222", i, strings.Replace(tx.JSON, `"type":"TRADE"`, `"type":"`+tc.types[i%len(tc.types)]+`"`, 1))
				}
			}
			doLogin(t)
			f.SetTransactionRules(schwabtest.TransactionRules{Cap: 50, CapMode: tc.mode, KeepOldest: tc.oldest})

			// The cap is Schwab's 3000 in real life; shrink it to match the fake.
			cfg, _ := loadConfig()
			auth, err := openLogin(cfg, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			c := newClient(auth)
			c.pageCap = 50
			accounts, err := c.accountNumbers(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			txs, err := fetchRecent(context.Background(), c, accounts, fetchOptions{
				N: 100, Lookback: 365 * 24 * time.Hour, Window: 60 * 24 * time.Hour,
				Types: schwab.AllTransactionTypes(), Now: testNow, Warn: func(s string) { t.Errorf("unexpected warning: %s", s) },
			})
			if err != nil {
				t.Fatal(err)
			}
			got := make([]string, len(txs))
			for i, tx := range txs {
				got[i] = strconv.FormatInt(tx.ActivityID, 10)
			}
			if want := f.expected(100, time.Time{}); !equal(got, want) {
				t.Fatalf("got %v\nwant %v", got, want)
			}
		})
	}
}

// showsMasked reports whether s names the account 12345678 as the tables
// show it, "account ...678", and holds no 4 of its digits in a row, which
// its last 4 or the whole number would.
func showsMasked(s string) bool {
	const number = "12345678"
	for i := 0; i+4 <= len(number); i++ {
		if strings.Contains(s, number[i:i+4]) {
			return false
		}
	}
	_, after, ok := strings.Cut(s, "account ...678")
	return ok && (after == "" || after[0] < '0' || after[0] > '9')
}

func TestOlderWindowRefusedKeepsWhatWeHave(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("12345678", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
	doLogin(t)
	// Schwab has not been seen to refuse an old window, but the tool copes.
	oldest := testNow.Add(-61 * 24 * time.Hour)
	f.SetTransactionRules(schwabtest.TransactionRules{RefuseBefore: oldest})

	out, stderr, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	// The tool narrows the refused window, so it gets everything Schwab serves.
	if got, want := idsFromJSON(t, out), f.expected(100, oldest); !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if !strings.Contains(stderr, "older ones are missing") || !showsMasked(stderr) {
		t.Errorf("no warning about the missing history of account ...678: %q", stderr)
	}
}

// Schwab answers [] for a window older than an account's history, as for
// any window without transactions, so the same walk keeps every row and
// warns of nothing.
func TestOlderWindowsAnsweredEmpty(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
	doLogin(t)

	out, stderr, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := idsFromJSON(t, out), f.expected(100, time.Time{}); !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if stderr != "" {
		t.Errorf("warnings: %q", stderr)
	}
	// An empty window does not show that the history has ended, so the walk
	// goes on to the end of the lookback.
	walked := testNow
	for _, q := range f.queries(schwabtest.TransactionsRoute) {
		start, err := time.Parse(time.RFC3339, q.Get("startDate"))
		if err != nil {
			t.Fatal(err)
		}
		if start.Before(walked) {
			walked = start
		}
	}
	if want := testNow.Add(-365 * 24 * time.Hour); !walked.Equal(want) {
		t.Errorf("walked back to %v, want the whole lookback, to %v", walked, want)
	}
}

// An account that Schwab stops taking part way through a run fails the run;
// it is not taken for the end of the account's history, whether the refusal
// comes alone or, as one report shows it, in a list of one.
func TestRefusedAccountFailsTheRun(t *testing.T) {
	for _, refusal := range []struct {
		name   string
		answer http.HandlerFunc
	}{
		{"alone", schwabtest.InvalidAccount},
		{"in a list of one", schwabtest.InvalidAccountList},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			f, _ := setup(t)
			f.addAccount("12345678", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
			doLogin(t)
			f.atSecondTransactions(func() { f.Override(schwabtest.TransactionsRoute, refusal.answer) })
			_, stderr, err := runTx(t)
			if !errors.Is(err, schwab.ErrInvalidAccount) || !showsMasked(err.Error()) {
				t.Errorf("got %v, want the refused account", err)
			}
			if strings.Contains(stderr, "older ones are missing") {
				t.Errorf("took the refused account for refused history: %q", stderr)
			}
			if n := len(f.queries(schwabtest.TransactionsRoute)); n < 2 {
				t.Errorf("%d requests for transactions, want the refusal after one was answered", n)
			}
		})
	}
}

// Only Schwab's 400 for a later window is taken for history it will not
// serve. Anything else that fails a later window fails the run: a 429 after
// every try, a 403 from Schwab's network, a 503 after every try, and a
// refresh that Schwab refuses for the app's key or secret, whose 400 is the
// token endpoint's.
func TestLaterWindowFailureFailsTheRun(t *testing.T) {
	override := func(h http.HandlerFunc) func(*fakeSchwab) {
		return func(f *fakeSchwab) { f.Override(schwabtest.TransactionsRoute, h) }
	}
	for _, tc := range []struct {
		name   string
		fail   func(f *fakeSchwab) // on the fake's goroutine, as the second request for transactions arrives
		status int                 // of the error that fails the run
		is     error               // that the error must be, if not nil
	}{
		{"rate limited", override(schwabtest.TooMany), http.StatusTooManyRequests, schwab.ErrRateLimited},
		{"turned away by Schwab's network", override(schwabtest.AccessDenied), http.StatusForbidden, nil},
		{"unavailable", override(schwabtest.Unavailable), http.StatusServiceUnavailable, nil},
		{"the app rejected", func(f *fakeSchwab) {
			f.RevokeAccess()
			f.FailTokens(schwabtest.Failure{Status: http.StatusBadRequest, Body: `{"error":"invalid_client","error_description":"Unauthorized"}`})
		}, http.StatusBadRequest, schwab.ErrAppRejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := setup(t)
			f.addAccount("12345678", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
			doLogin(t)
			timeNow = func() time.Time { return testNow.Add(2 * time.Minute) } // a token not just issued, nor stale
			f.atSecondTransactions(func() { tc.fail(f) })
			out, stderr, err := runTx(t)
			var se *schwab.Error
			if !errors.As(err, &se) || se.Status != tc.status || tc.is != nil && !errors.Is(err, tc.is) || !showsMasked(err.Error()) {
				t.Errorf("got %v, printed %d bytes, warned %q; want the run to fail with HTTP %d", err, len(out), stderr, tc.status)
			}
			if strings.Contains(stderr, "older ones are missing") {
				t.Errorf("took the failure for refused history: %q", stderr)
			}
			if len(f.queries(schwabtest.TransactionsRoute)) == 0 {
				t.Error("the run failed before a request for transactions was answered")
			}
		})
	}
}

// Records that share a time, and records whose ids could not be read, are
// each kept: the only repeats dropped are a record seen in two windows.
func TestRecordsThatShareATimeAreKept(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 3, testNow.Add(-time.Hour), 0) // three fills at one instant
	f.addAccount("33334444", 5000, 3, testNow.Add(-time.Hour), time.Hour)
	for i, tx := range f.rows("33334444")[:2] {
		f.setRow("33334444", i, strings.Replace(tx.JSON, fmt.Sprintf(`"activityId":%d`, tx.ID), fmt.Sprintf(`"activityId":"%d"`, tx.ID), 1))
	}
	doLogin(t)
	out, _, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(out), &raws); err != nil || len(raws) != 6 {
		t.Errorf("printed %d transactions, want 6 (%v)", len(raws), err)
	}
}

// Records that share a time are ranked by their ids, the highest first, so
// that -n keeps the same ones whatever order Schwab sends them in. Here the
// lowest id, 1001, arrives before the others, in one answer or from the
// account listed first.
func TestRecordsThatShareATimeAreRankedByID(t *testing.T) {
	at := testNow.Add(-time.Hour)
	for _, tc := range []struct {
		name string
		add  func(f *fakeSchwab) // ids 1001 to 1003, all at one time
	}{
		{"in one answer", func(f *fakeSchwab) {
			f.SetTransactionRules(schwabtest.TransactionRules{Order: schwabtest.Shuffled}) // which sends 1001, 1002, 1003
			f.addAccount("11112222", 1000, 3, at, 0)
		}},
		{"across accounts", func(f *fakeSchwab) {
			f.addAccount("11112222", 1000, 1, at, 0)
			f.addAccount("33334444", 1001, 2, at, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := setup(t)
			tc.add(f)
			doLogin(t)
			out, _, err := runTx(t, "-json", "-n", "2")
			if err != nil {
				t.Fatal(err)
			}
			if got, want := idsFromJSON(t, out), []string{"1003", "1002"}; !equal(got, want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

// The most recent transactions are the latest by time, not by trade date: a
// dividend posted in the afternoon is newer than that morning's fill, though
// its tradeDate is midnight.
func TestMostRecentIsByTime(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow.Add(-2*time.Hour), time.Hour)
	posted := testNow.Add(-time.Hour)
	f.addRows("11112222", fakeTx{ID: 999, When: posted, JSON: fmt.Sprintf(
		`{"activityId":999,"time":%q,"tradeDate":"2026-10-08T04:00:00+0000","settlementDate":"2026-10-08T04:00:00+0000",`+
			`"accountNumber":"11112222","type":"DIVIDEND_OR_INTEREST","status":"VALID","subAccount":"CASH",`+
			`"description":"Ordinary Dividend~VTI","netAmount":5,`+
			`"transferItems":[{"instrument":{"assetType":"CURRENCY","symbol":"CURRENCY_USD"},"amount":5,"cost":0}]}`,
		posted.UTC().Format("2006-01-02T15:04:05-0700"))})
	doLogin(t)
	out, stderr, err := runTx(t, "-json", "-n", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got := idsFromJSON(t, out); len(got) != 1 || got[0] != "999" {
		t.Errorf("most recent = %v, want the dividend, 999", got)
	}
	if stderr != "" {
		t.Errorf("warnings: %q", stderr)
	}
}

// An account number that Schwab sends in an unexpected form is named in a
// warning, before the error that the missing hash causes.
func TestUnreadableAccountNumberWarns(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow.Add(-time.Hour), time.Hour)
	f.Override(schwabtest.AccountNumbersRoute,
		schwabtest.Answer(http.StatusOK, `[{"accountNumber":"11112222","hashValue":12345}]`, "Content-Type", "application/json"))
	doLogin(t)
	_, stderr, err := runTx(t)
	if err == nil || !strings.Contains(err.Error(), "no account hash") {
		t.Errorf("got %v, want the missing hash", err)
	}
	if want := "warning: 1 account number differed from what Schwab has sent before " +
		"(hashValue: Schwab sent a number where a string was expected)"; !strings.Contains(stderr, want) {
		t.Errorf("warning: %q\nwant it to hold %q", stderr, want)
	}
}

// A login that lapses part way through a run is reported as such, not taken
// for Schwab refusing older history.
func TestLoginLapsesDuringARun(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
	doLogin(t)
	timeNow = func() time.Time { return testNow.Add(2 * time.Minute) } // a token not just issued, nor stale
	f.atSecondTransactions(func() {
		f.RevokeAccess()
		f.EndLogin()
	})
	_, stderr, err := runTx(t)
	if err == nil || !strings.Contains(err.Error(), "schwabtx login") {
		t.Errorf("got %v, want the refused login", err)
	}
	if strings.Contains(stderr, "older ones are missing") {
		t.Errorf("took the refused login for refused history: %q", stderr)
	}
	if len(f.queries(schwabtest.TransactionsRoute)) == 0 {
		t.Error("the login lapsed before a request for transactions was answered")
	}
}

func TestRefusedWindowIsNarrowedToTheLimit(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("12345678", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
	doLogin(t)
	oldest := testNow.Add(-85 * 24 * time.Hour) // inside the second 60-day window
	f.SetTransactionRules(schwabtest.TransactionRules{RefuseBefore: oldest})

	out, stderr, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	got, want := idsFromJSON(t, out), f.expected(100, oldest)
	if len(want) != 29 || !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if !strings.Contains(stderr, "older ones are missing") || !showsMasked(stderr) {
		t.Errorf("no warning about the missing history of account ...678: %q", stderr)
	}
}

func TestLoginStopsOnCancel(t *testing.T) {
	setup(t)
	pr, pw, err := os.Pipe() // never written to: the user is still at the prompt
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var out bytes.Buffer
	go func() { done <- run(ctx, []string{"login"}, pr, &syncWriter{w: &out}, &syncWriter{w: &out}) }()
	waitForPrompt(t, &out, done) // so the cancel lands while login waits on stdin
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("login ignored the cancelled context")
	}
}

func TestBadFlagsFailBeforeAnyRequest(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	// Two of the five are a journal and a dividend, which Schwab sends only
	// when the types asked for name them.
	for i, typ := range []string{"JOURNAL", "DIVIDEND_OR_INTEREST"} {
		f.setRow("11112222", i, strings.Replace(f.rows("11112222")[i].JSON, `"type":"TRADE"`, `"type":"`+typ+`"`, 1))
	}
	doLogin(t)
	before := f.AllRequests()
	for _, args := range [][]string{
		{"-types", "TRADES"},
		{"-types", ","},
		{"-lookback", "200000"},
		{"-window", "200000"},
		{"-window", "366"},
		{"-n", "0"},
	} {
		if out, _, err := runTx(t, args...); err == nil {
			t.Errorf("%v: expected an error, got output %q", args, out)
		}
	}
	if n := f.AllRequests() - before; n != 0 {
		t.Errorf("bad flags still caused %d requests", n)
	}
	// -types asks for the trades and the dividend alone.
	if out, _, err := runTx(t, "-json", "-types", "trade, dividend_or_interest"); err != nil {
		t.Errorf("valid -types failed: %v", err)
	} else if ids := idsFromJSON(t, out); len(ids) != 4 {
		t.Errorf("-types trade, dividend_or_interest printed %v, want all but the journal", ids)
	}
	// -window takes Schwab's whole year.
	if out, _, err := runTx(t, "-json", "-window", "365"); err != nil {
		t.Errorf("-window 365 failed: %v", err)
	} else if ids := idsFromJSON(t, out); len(ids) != 5 {
		t.Errorf("-window 365 printed %v, want all five", ids)
	}
}

func TestDateOnlyTransactionKeepsItsDate(t *testing.T) {
	var txs []schwab.Transaction
	if err := json.Unmarshal([]byte(`[{"activityId":1,"tradeDate":"2024-03-28","type":"JOURNAL","status":"VALID","netAmount":10,"description":"AT&T <x>"}]`), &txs); err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = time.FixedZone("west", -5*3600)
	defer func() { time.Local = old }()
	var table, js bytes.Buffer
	if err := printTable(&table, txs, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "2024-03-28 ") || strings.Contains(table.String(), "2024-03-27") {
		t.Errorf("date shifted: %s", table.String())
	}
	if err := printJSON(&js, txs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js.String(), "AT&T <x>") {
		t.Errorf("JSON was escaped: %s", js.String())
	}
}

func TestFirstWindowErrorFails(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("12345678", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	f.SetTransactionRules(schwabtest.TransactionRules{RefuseBefore: testNow.Add(time.Hour)}) // every request is refused

	if out, _, err := runTx(t); err == nil {
		t.Fatalf("expected an error, got output %q", out)
	} else if !strings.Contains(err.Error(), "HTTP 400") || !showsMasked(err.Error()) {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	f.Queue(schwabtest.TransactionsRoute, schwabtest.TooMany, schwabtest.TooMany)

	cfg, _ := loadConfig()
	auth, err := openLogin(cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c := newClient(auth)
	txs, err := c.transactions(context.Background(), schwab.AccountHash(f.hash("11112222")), testNow.Add(-24*time.Hour), testNow,
		schwab.AllTransactionTypes())
	if err != nil {
		t.Fatal(err)
	}
	if calls := len(f.queries(schwabtest.TransactionsRoute)); len(txs) != 5 || calls != 3 {
		t.Fatalf("got %d transactions in %d calls, want 5 in 3", len(txs), calls)
	}
}

// Schwab's answers that the tool cannot get past say what to do about them.
func TestSchwabErrorsSayWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		answer http.HandlerFunc // to every try
		want   []string
	}{
		{"rate limited", http.StatusTooManyRequests, schwabtest.TooMany, []string{"HTTP 429 from Schwab after 4 tries", "wait a minute"}},
		{
			"an app not yet Ready For Use", http.StatusInternalServerError, schwabtest.Whitelabel,
			[]string{"Whitelabel Error Page", `until the app says "Ready For Use"`},
		},
		{
			"turned away by Schwab's network", http.StatusForbidden, schwabtest.AccessDenied,
			[]string{"Access Denied (reference #" + schwabtest.AkamaiReference + ")", "traderapi@schwab.com"},
		},
		{
			"Schwab's network, with its servers not answering", http.StatusServiceUnavailable,
			schwabtest.Answer(http.StatusServiceUnavailable,
				"<HTML><HEAD><TITLE>Service Unavailable</TITLE></HEAD><BODY>Reference&#32;&#35;18&#46;77</BODY></HTML>"),
			[]string{"after 4 tries: an HTML page, Service Unavailable (reference #18.77)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := setup(t)
			doLogin(t)
			f.Override(schwabtest.AccountNumbersRoute, tc.answer)
			_, _, err := runTx(t)
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("got %v, want %q in it", err, want)
				}
			}
			if tc.status != http.StatusForbidden && err != nil && strings.Contains(err.Error(), "turned the request away") {
				t.Errorf("got %v, which says the request was turned away", err)
			}
		})
	}
}

func TestMissingCredentials(t *testing.T) {
	setup(t)
	t.Setenv("SCHWAB_APP_SECRET", "")
	if _, _, err := runTx(t); err == nil || !strings.Contains(err.Error(), "SCHWAB_APP_SECRET") {
		t.Fatalf("expected a missing-credentials error, got %v", err)
	}
}

func TestExpiryWarning(t *testing.T) {
	timeNow = func() time.Time { return testNow }
	t.Cleanup(func() { timeNow = time.Now })
	for _, tc := range []struct {
		left time.Duration // until the login lapses
		want string
	}{
		{left: 49 * time.Hour},
		{left: 48 * time.Hour, want: "in about 48 hours"},
		{left: 36*time.Hour + 20*time.Minute, want: "in about 36 hours"},
		{left: 90 * time.Minute, want: "in about 2 hours"},
		{left: time.Hour, want: "in about an hour"},
		{left: 20 * time.Minute, want: "in less than an hour"},
		{left: 0},
		{left: -time.Hour},
	} {
		got := expiryWarning(testNow.Add(tc.left))
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%v left: warned %q", tc.left, got)
		case tc.want != "" && !strings.Contains(got, "expires "+tc.want+"; run `schwabtx login`"):
			t.Errorf("%v left: warned %q, want it to say %q", tc.left, got, tc.want)
		}
	}
	if got := expiryWarning(time.Time{}); got != "" {
		t.Errorf("warned %q without a saved login", got)
	}
}

// A time that is a date alone keeps its date in the table, wherever the
// machine is, as a transaction without a time does.
func TestDateOnlyTimeKeepsItsDate(t *testing.T) {
	var txs []schwab.Transaction
	if err := json.Unmarshal([]byte(`[{"activityId":1,"time":"2024-03-28","type":"JOURNAL","status":"VALID","netAmount":10}]`), &txs); err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = time.FixedZone("west", -8*3600)
	defer func() { time.Local = old }()
	var table bytes.Buffer
	if err := printTable(&table, txs, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(table.String(), "2024-03-28 ") || strings.Contains(table.String(), "2024-03-27") {
		t.Errorf("date shifted: %s", table.String())
	}
}

// A row shows its time in the machine's zone, even a quarter past midnight
// in New York; only midnight in New York shows a date alone.
func TestTableShowsTimes(t *testing.T) {
	var txs []schwab.Transaction
	if err := json.Unmarshal([]byte(`[{"activityId":1,"time":"2025-01-14T05:15:12+0000","type":"TRADE","status":"VALID","netAmount":1},`+
		`{"activityId":2,"time":"2024-03-28T04:00:00+0000","type":"JOURNAL","status":"VALID","netAmount":1}]`), &txs); err != nil {
		t.Fatal(err)
	}
	old := time.Local
	time.Local = time.FixedZone("west", -8*3600)
	defer func() { time.Local = old }()
	var table bytes.Buffer
	if err := printTable(&table, txs, false); err != nil {
		t.Fatal(err)
	}
	if s := table.String(); !strings.Contains(s, "2025-01-13 21:15") || !strings.Contains(s, "2024-03-28 ") || strings.Contains(s, "2024-03-27") {
		t.Errorf("table:\n%s", s)
	}
}

// The table shows an account's number as Schwab masks it, "..." and its
// last 3 digits, unless -show-accounts asks for it whole, and nothing for
// a transaction without one.
func TestTableMasksAccountNumbers(t *testing.T) {
	var txs []schwab.Transaction
	if err := json.Unmarshal([]byte(`[{"activityId":1,"accountNumber":"12345678","tradeDate":"2024-03-28","type":"JOURNAL","status":"VALID","netAmount":1},`+
		`{"activityId":2,"tradeDate":"2024-03-28","type":"JOURNAL","status":"VALID","netAmount":2}]`), &txs); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		show bool
		want string
	}{{false, "...678"}, {true, "12345678"}} {
		var table bytes.Buffer
		if err := printTable(&table, txs, tc.show); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(table.String(), "\n")
		if len(lines) < 3 || strings.Fields(lines[1])[2] != tc.want || strings.Fields(lines[2])[2] != "JOURNAL" {
			t.Errorf("show %t: table\n%s\nwant the account %s in the first row and none in the second", tc.show, table.String(), tc.want)
		}
	}
}

// -json prints Schwab's JSON as it came, indented: "&", "<" and the line
// separator U+2028 are not escaped, on any Go.
func TestPrintJSONKeepsSchwabsText(t *testing.T) {
	var txs []schwab.Transaction
	if err := json.Unmarshal([]byte(`[{"activityId":1,"description":"AT&T <x>`+"\u2028"+`","netAmount":250.0},{}]`), &txs); err != nil {
		t.Fatal(err)
	}
	var js bytes.Buffer
	if err := printJSON(&js, txs); err != nil {
		t.Fatal(err)
	}
	want := "[\n  {\n    \"activityId\": 1,\n    \"description\": \"AT&T <x>\u2028\",\n    \"netAmount\": 250.0\n  },\n  {}\n]\n"
	if js.String() != want {
		t.Errorf("printed\n%q\nwant\n%q", js.String(), want)
	}
	js.Reset()
	if err := printJSON(&js, nil); err != nil || js.String() != "[]\n" {
		t.Errorf("no transactions printed as %q, %v", js.String(), err)
	}
}

// -json prints a transaction as Schwab sent it, all the way from the
// answer: the fake sends a row's bytes as they are, and the output is them
// re-indented, "&", "<", U+2028 and 250.0 as they came.
func TestJSONPrintsWhatSchwabSent(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 1, testNow.Add(-time.Hour), time.Hour)
	tx := strings.Replace(f.rows("11112222")[0].JSON, `"description":""`, `"description":"AT&T <x>`+"\u2028"+`"`, 1)
	tx = strings.Replace(tx, `"netAmount":-1234.56`, `"netAmount":250.0`, 1)
	f.setRow("11112222", 0, tx)
	doLogin(t)
	out, _, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	want.WriteString("[\n  ")
	if err := json.Indent(&want, []byte(tx), "  ", "  "); err != nil {
		t.Fatal(err)
	}
	want.WriteString("\n]\n")
	if out != want.String() {
		t.Errorf("printed\n%s\nwant\n%s", out, want.String())
	}
}

// The warning about records with problems names each place once.
func TestProblemsWarning(t *testing.T) {
	if w := problemsWarning("transaction", [][]schwab.Problem{nil, nil}); w != "" {
		t.Errorf("a warning without problems: %q", w)
	}
	bad := func(what string) []schwab.Problem {
		return []schwab.Problem{{Path: "time", What: what}, {Path: "netAmount", What: "Schwab sent a string where a number was expected"}}
	}
	w := problemsWarning("transaction", [][]schwab.Problem{bad(`Schwab sent "x", which is not a time`), nil, bad(`Schwab sent "y", which is not a time`)})
	want := `2 transactions differed from what Schwab has sent before (time: Schwab sent "x", which is not a time; ` +
		`netAmount: Schwab sent a string where a number was expected)`
	if w != want {
		t.Errorf("warning:\n got %q\nwant %q", w, want)
	}
	many := []schwab.Problem{{Path: "a", What: "w"}, {Path: "b", What: "w"}, {Path: "c", What: "w"}, {Path: "d", What: "w"}, {Path: "e", What: "w"}}
	if w := problemsWarning("account", [][]schwab.Problem{many}); !strings.HasPrefix(w, "1 account differed") || !strings.Contains(w, "c: w; and 2 more)") {
		t.Errorf("warning about many: %q", w)
	}
}

// A transaction that Schwab sends in an unexpected form is listed, with a
// warning, rather than failing the run.
func TestUnreadableTransactionWarns(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 3, testNow.Add(-time.Hour), time.Hour)
	f.setRow("11112222", 0, strings.Replace(f.rows("11112222")[0].JSON, `"netAmount":-1234.56`, `"netAmount":"-1234.56"`, 1))
	doLogin(t)
	out, stderr, err := runTx(t)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "1 transaction differed from what Schwab has sent before (netAmount:") ||
		!strings.Contains(stderr, "-json prints Schwab's own JSON") {
		t.Errorf("no warning: %q", stderr)
	}
	if strings.Count(out, "VTI") != 3 {
		t.Errorf("table:\n%s", out)
	}
}
