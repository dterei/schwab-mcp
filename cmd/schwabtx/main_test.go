package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSchwab imitates the OAuth token endpoint and the Trader API endpoints
// the tool uses, enforcing the same auth rules.
type fakeSchwab struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	accounts      []account
	txs           map[string][]fakeTx // by account hash
	validAccess   string
	issued        int
	codeGrants    int
	refreshGrants int
	txCalls       int
	cap           int       // max transactions per response, 0 = unlimited
	oldestAllowed time.Time // startDate before this gets a 400
	rateLimitNext int       // answer this many transaction calls with 429
	detailsFields string    // fields= of the last GET /accounts
	detailsBody   string    // what the last GET /accounts returned
}

type fakeTx struct {
	ID   int64
	When time.Time
	JSON string
}

const (
	testKey      = "test-app-key"
	testSecret   = "test-app-secret"
	testCode     = "C0.abc123@"
	testRefresh  = "refresh-token-1"
	testCallback = "https://127.0.0.1"
)

func newFakeSchwab(t *testing.T) *fakeSchwab {
	f := &fakeSchwab{t: t, txs: map[string][]fakeTx{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", f.handleToken)
	mux.HandleFunc("GET /trader/v1/accounts/accountNumbers", f.handleAccounts)
	mux.HandleFunc("GET /trader/v1/accounts", f.handleAccountDetails)
	mux.HandleFunc("GET /trader/v1/accounts/{hash}/transactions", f.handleTransactions)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSchwab) handleToken(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	user, pass, ok := r.BasicAuth()
	if !ok || user != testKey || pass != testSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		http.Error(w, `{"error":"bad content type"}`, http.StatusBadRequest)
		return
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		if r.FormValue("code") != testCode || r.FormValue("redirect_uri") != testCallback {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.codeGrants++
	case "refresh_token":
		if r.FormValue("refresh_token") != testRefresh {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.refreshGrants++
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	f.issued++
	f.validAccess = fmt.Sprintf("access-%d", f.issued)
	f.writeJSON(w, map[string]any{
		"expires_in": 1800, "token_type": "Bearer", "scope": "api",
		"refresh_token": testRefresh, "access_token": f.validAccess,
	})
}

func (f *fakeSchwab) writeJSON(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		f.t.Errorf("fake Schwab: writing response: %v", err)
	}
}

func (f *fakeSchwab) authed(w http.ResponseWriter, r *http.Request) bool {
	if f.validAccess == "" || r.Header.Get("Authorization") != "Bearer "+f.validAccess {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return false
	}
	return true
}

func (f *fakeSchwab) handleAccounts(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authed(w, r) {
		return
	}
	f.writeJSON(w, f.accounts)
}

// fakePositions are the positions of every fake account. The "&" and the
// numbers such as 250.0 show whether a snapshot keeps Schwab's text exactly.
const fakePositions = `{"shortQuantity":0,"averagePrice":246.91,"longQuantity":5,"marketValue":1300.5,` +
	`"instrument":{"assetType":"COLLECTIVE_INVESTMENT","symbol":"VTI","description":"VANGUARD TOTAL STOCK MARKET ETF"}},` +
	`{"shortQuantity":0,"averagePrice":17.5,"longQuantity":10,"marketValue":250.0,` +
	`"instrument":{"assetType":"EQUITY","symbol":"T","description":"AT&T INC"}}`

// handleAccountDetails answers GET /accounts, with positions only when asked
// for, as Schwab does. The JSON is written by hand so that tests can check
// the tool keeps it as sent, and it goes out indented, so they can check the
// tool puts it on one line.
func (f *fakeSchwab) handleAccountDetails(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authed(w, r) {
		return
	}
	f.detailsFields = r.URL.Query().Get("fields")
	parts := make([]string, len(f.accounts))
	for i, a := range f.accounts {
		positions := ""
		if f.detailsFields == "positions" {
			positions = `"positions":[` + fakePositions + `],`
		}
		parts[i] = fmt.Sprintf(`{"securitiesAccount":{"type":"MARGIN","accountNumber":%q,%s`+
			`"currentBalances":{"liquidationValue":12345.60,"cashBalance":100.0}},`+
			`"aggregatedBalance":{"currentLiquidationValue":12345.60,"liquidationValue":12345.60}}`,
			a.AccountNumber, positions)
	}
	f.detailsBody = "[" + strings.Join(parts, ",") + "]"
	var sent bytes.Buffer
	if err := json.Indent(&sent, []byte(f.detailsBody), "", "  "); err != nil {
		f.t.Errorf("fake Schwab: indenting response: %v", err)
	}
	sent.WriteString("\n")
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.Copy(w, &sent); err != nil {
		f.t.Errorf("fake Schwab: writing response: %v", err)
	}
}

func (f *fakeSchwab) handleTransactions(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.authed(w, r) {
		return
	}
	f.txCalls++
	if f.rateLimitNext > 0 {
		f.rateLimitNext--
		http.Error(w, `{"error":"too many requests"}`, http.StatusTooManyRequests)
		return
	}
	q := r.URL.Query()
	start, err1 := time.Parse(schwabTimeFormat, q.Get("startDate"))
	end, err2 := time.Parse(schwabTimeFormat, q.Get("endDate"))
	if err1 != nil || err2 != nil || q.Get("types") == "" {
		http.Error(w, `{"message":"bad startDate, endDate or types"}`, http.StatusBadRequest)
		return
	}
	if !f.oldestAllowed.IsZero() && start.Before(f.oldestAllowed) {
		http.Error(w, `{"message":"startDate too far back"}`, http.StatusBadRequest)
		return
	}
	all, ok := f.txs[r.PathValue("hash")]
	if !ok {
		http.Error(w, `{"message":"no such account"}`, http.StatusNotFound)
		return
	}
	var out []json.RawMessage
	for _, tx := range all {
		if !tx.When.Before(start) && !tx.When.After(end) { // inclusive, like a careless API
			out = append(out, json.RawMessage(tx.JSON))
		}
	}
	if f.cap > 0 && len(out) > f.cap {
		out = out[len(out)-f.cap:] // keep the oldest ones: the worst case for us
	}
	if out == nil {
		out = []json.RawMessage{}
	}
	f.writeJSON(w, out)
}

// addAccount gives the fake an account with count transactions, the newest at
// newest and each one step older than the last.
func (f *fakeSchwab) addAccount(number string, idBase int64, count int, newest time.Time, step time.Duration) {
	hash := "HASH" + number
	f.accounts = append(f.accounts, account{AccountNumber: number, HashValue: hash})
	f.txs[hash] = nil
	for i := range count {
		when := newest.Add(-time.Duration(i) * step)
		id := idBase + int64(count-i)
		js := fmt.Sprintf(`{"activityId":%d,"time":%q,"accountNumber":%q,"type":"TRADE","status":"VALID","tradeDate":%q,"netAmount":-1234.56,"description":"",`+
			`"transferItems":[{"instrument":{"assetType":"CURRENCY","symbol":"CURRENCY_USD"},"amount":0.01,"cost":-0.01,"feeType":"SEC_FEE"},`+
			`{"instrument":{"assetType":"EQUITY","symbol":"VTI","description":"VANGUARD TOTAL STOCK MARKET ETF"},"amount":5,"cost":-1234.55,"price":246.91,"positionEffect":"OPENING"}]}`,
			id, when.UTC().Format("2006-01-02T15:04:05-0700"), number, when.UTC().Format("2006-01-02T15:04:05-0700"))
		f.txs[hash] = append(f.txs[hash], fakeTx{ID: id, When: when, JSON: js})
	}
}

// expected returns the ids of the n most recent transactions newer than cutoff.
func (f *fakeSchwab) expected(n int, cutoff time.Time) []string {
	var all []fakeTx
	for _, txs := range f.txs {
		for _, tx := range txs {
			if !tx.When.Before(cutoff) {
				all = append(all, tx)
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

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T) (*fakeSchwab, string) {
	f := newFakeSchwab(t)
	tokenFile := filepath.Join(t.TempDir(), "cfg", "token.json")
	t.Setenv("SCHWAB_APP_KEY", testKey)
	t.Setenv("SCHWAB_APP_SECRET", testSecret)
	t.Setenv("SCHWAB_CALLBACK_URL", "")
	t.Setenv("SCHWAB_BASE_URL", f.srv.URL)
	t.Setenv("SCHWAB_TOKEN_FILE", tokenFile)
	t.Setenv("SCHWAB_SNAPSHOT_FILE", filepath.Join(filepath.Dir(tokenFile), "snapshots.jsonl"))
	// Keep the user config directory in the test's temp space too, so that
	// nothing can reach the real one.
	home := t.TempDir()
	t.Setenv("HOME", home)            // macOS
	t.Setenv("XDG_CONFIG_HOME", home) // Linux and other Unix systems
	t.Setenv("AppData", home)         // Windows
	timeNow = func() time.Time { return testNow }
	t.Cleanup(func() { timeNow = time.Now })
	return f, tokenFile
}

// doLogin runs `schwabtx login`, pasting back a redirect address the way a
// browser would show it.
func doLogin(t *testing.T) {
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
	redirect := testCallback + "/?code=" + url.QueryEscape(testCode) + "&session=abc&state=" + q.Get("state")
	fmt.Fprintln(pw, redirect)
	pw.Close()
	if err := <-done; err != nil {
		t.Fatalf("login failed: %v", err)
	}
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

	if _, _, err := runTx(t); err == nil || !strings.Contains(err.Error(), "schwabtx login") {
		t.Fatalf("expected a not-logged-in error, got %v", err)
	}

	doLogin(t)
	if f.codeGrants != 1 {
		t.Fatalf("code grants = %d, want 1", f.codeGrants)
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
	if f.refreshGrants != 0 {
		t.Fatalf("refreshed a still-valid access token %d times", f.refreshGrants)
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
	for _, want := range []string{"VTI", "246.91", "-1234.56", "...2222", "TRADE", "VANGUARD TOTAL STOCK MARKET ETF"} {
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
	for _, secret := range []string{testSecret, testRefresh, "access-1"} {
		if strings.Contains(table+stderr+out, secret) {
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

	// 31 minutes later the access token has lapsed.
	timeNow = func() time.Time { return testNow.Add(31 * time.Minute) }
	if _, _, err := runTx(t, "-json"); err != nil {
		t.Fatal(err)
	}
	if f.refreshGrants != 1 {
		t.Fatalf("refresh grants = %d, want 1", f.refreshGrants)
	}
	b, _ := os.ReadFile(tokenFile)
	if !strings.Contains(string(b), "access-2") {
		t.Fatalf("refreshed token was not saved: %s", b)
	}
}

func TestRevokedAccessTokenIsRefreshedOn401(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	f.validAccess = "revoked-server-side"

	// The fake's refresh handler issues a fresh token, so one retry fixes it.
	if _, _, err := runTx(t, "-json"); err != nil {
		t.Fatal(err)
	}
	if f.refreshGrants != 1 {
		t.Fatalf("refresh grants = %d, want 1", f.refreshGrants)
	}
}

func TestExpiredRefreshTokenAsksForLogin(t *testing.T) {
	f, tokenFile := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)

	b, _ := os.ReadFile(tokenFile)
	if err := os.WriteFile(tokenFile, bytes.ReplaceAll(b, []byte(testRefresh), []byte("stale-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	timeNow = func() time.Time { return testNow.Add(8 * 24 * time.Hour) }

	_, _, err := runTx(t)
	if err == nil || !strings.Contains(err.Error(), "schwabtx login") {
		t.Fatalf("expected a re-login error, got %v", err)
	}
}

func TestTruncatedResponsesAreSplit(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 300, testNow.Add(-time.Hour), 6*time.Hour) // 240 per 60-day window
	doLogin(t)
	f.cap = 50

	// The cap is Schwab's 3000 in real life; shrink it to match the fake.
	cfg, _ := loadConfig()
	auth := newAuthenticator(cfg, newHTTPClient())
	if err := auth.load(); err != nil {
		t.Fatal(err)
	}
	c := newClient(cfg, auth, newHTTPClient())
	c.pageCap = 50
	accounts, err := c.accountNumbers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	txs, err := fetchRecent(context.Background(), c, accounts, fetchOptions{
		N: 100, Lookback: 365 * 24 * time.Hour, Window: 60 * 24 * time.Hour,
		Types: allTransactionTypes, Now: testNow, Warn: func(s string) { t.Errorf("unexpected warning: %s", s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(txs))
	for i, tx := range txs {
		got[i] = tx.ActivityID.String()
	}
	if want := f.expected(100, time.Time{}); !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
}

func TestOlderWindowRefusedKeepsWhatWeHave(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
	doLogin(t)
	f.oldestAllowed = testNow.Add(-61 * 24 * time.Hour)

	out, stderr, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	// The tool narrows the refused window, so it gets everything Schwab serves.
	if got, want := idsFromJSON(t, out), f.expected(100, f.oldestAllowed); !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if !strings.Contains(stderr, "older ones are missing") {
		t.Errorf("no warning about the missing history: %q", stderr)
	}
}

func TestRefusedWindowIsNarrowedToTheLimit(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 40, testNow.Add(-time.Hour), 72*time.Hour) // spans 120 days
	doLogin(t)
	f.oldestAllowed = testNow.Add(-85 * 24 * time.Hour) // inside the second 60-day window

	out, stderr, err := runTx(t, "-json")
	if err != nil {
		t.Fatal(err)
	}
	got, want := idsFromJSON(t, out), f.expected(100, f.oldestAllowed)
	if len(want) != 29 || !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if !strings.Contains(stderr, "older ones are missing") {
		t.Errorf("no warning about the missing history: %q", stderr)
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
	doLogin(t)
	for _, args := range [][]string{
		{"-types", "TRADES"},
		{"-types", ","},
		{"-lookback", "200000"},
		{"-window", "200000"},
		{"-n", "0"},
	} {
		if out, _, err := runTx(t, args...); err == nil {
			t.Errorf("%v: expected an error, got output %q", args, out)
		}
	}
	if f.txCalls != 0 {
		t.Errorf("bad flags still caused %d transaction requests", f.txCalls)
	}
	if out, _, err := runTx(t, "-json", "-types", "trade, dividend_or_interest"); err != nil || len(idsFromJSON(t, out)) != 5 {
		t.Errorf("valid -types failed: %v", err)
	}
}

func TestDateOnlyTransactionKeepsItsDate(t *testing.T) {
	txs, err := decodeTransactions([]byte(`[{"activityId":1,"tradeDate":"2024-03-28","type":"JOURNAL","status":"VALID","netAmount":10,"description":"AT&T <x>"}]`))
	if err != nil {
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
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	f.oldestAllowed = testNow.Add(time.Hour) // every request is refused

	if out, _, err := runTx(t); err == nil {
		t.Fatalf("expected an error, got output %q", out)
	} else if !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "...2222") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

func TestRateLimitIsRetried(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 5, testNow.Add(-time.Hour), time.Hour)
	doLogin(t)
	f.rateLimitNext = 2

	cfg, _ := loadConfig()
	auth := newAuthenticator(cfg, newHTTPClient())
	if err := auth.load(); err != nil {
		t.Fatal(err)
	}
	c := newClient(cfg, auth, newHTTPClient())
	c.retryWait = time.Millisecond
	txs, err := c.transactions(context.Background(), "HASH11112222", testNow.Add(-24*time.Hour), testNow, allTransactionTypes)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 5 || f.txCalls != 3 {
		t.Fatalf("got %d transactions in %d calls, want 5 in 3", len(txs), f.txCalls)
	}
}

func TestExtractCode(t *testing.T) {
	cases := []struct {
		name, in, state, want string
		wantErr               bool
	}{
		{"full address", "https://127.0.0.1/?code=C0.abc%40&session=x&state=s1\n", "s1", "C0.abc@", false},
		{"no state echoed", "https://127.0.0.1/?code=C0.abc%40", "s1", "C0.abc@", false},
		{"wrong state", "https://127.0.0.1/?code=C0.abc%40&state=other", "s1", "", true},
		{"bare encoded code", "C0.abc%40", "s1", "C0.abc@", false},
		{"bare code", "  C0.abc@  ", "s1", "C0.abc@", false},
		{"address without scheme", "127.0.0.1/?code=C0.abc%40&session=x&state=s1", "s1", "C0.abc@", false},
		{"query string only", "code=C0.abc%40&state=s1", "s1", "C0.abc@", false},
		{"query string, wrong state", "code=C0.abc%40&state=other", "s1", "", true},
		{"denied", "https://127.0.0.1/?error=access_denied&error_description=nope", "s1", "", true},
		{"no code", "https://127.0.0.1/", "s1", "", true},
		{"empty", "\n", "s1", "", true},
	}
	for _, tc := range cases {
		got, err := extractCode(tc.in, tc.state)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, err=%v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestParseSchwabTime(t *testing.T) {
	want := time.Date(2024, 3, 28, 21, 10, 42, 0, time.UTC)
	for _, in := range []string{
		"2024-03-28T21:10:42+0000",
		"2024-03-28T21:10:42.000+0000",
		"2024-03-28T21:10:42Z",
		"2024-03-28T21:10:42.000Z",
		"2024-03-28T17:10:42-04:00",
		"2024-03-28T17:10:42-0400",
	} {
		if got, dateOnly := parseSchwabTime(in); !got.Equal(want) || dateOnly {
			t.Errorf("parseSchwabTime(%q) = %v, %v; want %v, false", in, got, dateOnly, want)
		}
	}
	if got, dateOnly := parseSchwabTime("2024-03-28"); !dateOnly || got.Format("2006-01-02") != "2024-03-28" {
		t.Errorf("date-only parsed as %v, %v", got, dateOnly)
	}
	if got, _ := parseSchwabTime("garbage"); !got.IsZero() {
		t.Errorf("garbage parsed as %v", got)
	}
}

func TestMissingCredentials(t *testing.T) {
	setup(t)
	t.Setenv("SCHWAB_APP_SECRET", "")
	if _, _, err := runTx(t); err == nil || !strings.Contains(err.Error(), "SCHWAB_APP_SECRET") {
		t.Fatalf("expected a missing-credentials error, got %v", err)
	}
}
