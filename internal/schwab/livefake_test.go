package schwab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// liveNow is when the runs on the fake take place: the year before it holds
// rows of two of the fixtures' accounts, among them the row that
// transaction.json answers by its id.
var liveNow = time.Date(2025, 3, 1, 15, 0, 0, 0, time.UTC)

// liveCommit is the commit that the runs on the fake are given, so that
// they run no git.
const liveCommit = "0123456789abcdef0123456789abcdef01234567"

// plantedUser is the user name, the home folder's last element, of a run
// on the fake.
const plantedUser = "zqplanteduser"

// A fakeLive is a live run on the fake, as CI runs the harness: everything
// but TestLive's front door, with one fake clock and every file in a
// temporary folder.
type fakeLive struct {
	r     *liveRun
	f     *fakeOAuth
	clock *fakeTime
	dir   string // the temporary folder that holds the run's files
	mu    sync.Mutex
	shown []string // the lines printed, as -v prints them
}

// newFakeLive returns a run in the default mode, logged in to a fake over
// HTTPS that holds the fixtures' world. Its clock serves the run's waits,
// the Auth's and the Client's, and the fake's; its home folder, cache
// folder, token and rate files, out folder and state are under one
// temporary folder, the home folder's last element a planted user name.
func newFakeLive(t *testing.T) *fakeLive {
	t.Helper()
	f := schwabtest.NewTLS(t)
	f.LoadFixtures()
	clock := &fakeTime{now: liveNow}
	f.SetNow(clock.Now)
	dir := t.TempDir()
	home := filepath.Join(dir, "home", plantedUser)
	cache := filepath.Join(dir, "cache")
	tokenFile := filepath.Join(home, "config", "schwabtx", "token.json")
	cfg := Config{AppKey: testKey, AppSecret: testSecret, BaseURL: f.URL(), TokenFile: tokenFile, HTTPClient: f.Client(), Now: clock.Now}
	login, err := NewAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := login.Login(t.Context(), testCode); err != nil { // as schwabtx login would
		t.Fatal(err)
	}
	a, err := NewAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	same := func(d time.Duration) time.Duration { return d }
	a.sleep, a.jitter = clock.Sleep, same
	c := NewClient(a, ClientConfig{RateFile: filepath.Join(cache, "schwabtx", "rate-test")})
	c.now, c.sleep, c.jitter = clock.Now, clock.Sleep, same
	module, err := moduleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	// A snapshot 12 hours off the clock's time of day, in a fixed zone,
	// which the rate probe never meets, whatever the machine's zone.
	loc := time.FixedZone("UTC-5", -5*60*60)
	at := liveNow.In(loc)
	snapshot := (time.Duration(at.Hour())*time.Hour + time.Duration(at.Minute())*time.Minute + 12*time.Hour) % (24 * time.Hour)
	live := filepath.Join(cache, "schwabtx", "live")
	fl := &fakeLive{f: f, clock: clock, dir: dir}
	fl.r = &liveRun{
		now: clock.Now, sleep: clock.Sleep, auth: a, client: c, origin: f.URL(),
		out: filepath.Join(live, "run"), stateFile: filepath.Join(live, "state.json"), keyFile: filepath.Join(live, "state.key"),
		tokenFile: a.tokenFile, rateFile: c.limit.path, home: home, cacheDir: cache, module: module,
		snapshot: snapshot, loc: loc, pageCap: MaxTransactions, mode: modeDefault,
		deadline: liveNow.Add(9 * time.Minute), commit: liveCommit,
		show: func(line string) {
			fl.mu.Lock()
			defer fl.mu.Unlock()
			fl.shown = append(fl.shown, line)
		},
	}
	return fl
}

// run runs the harness.
func (fl *fakeLive) run(t *testing.T) error {
	t.Helper()
	return runLive(t.Context(), fl.r)
}

// report returns report.txt.
func (fl *fakeLive) report(t *testing.T) string {
	t.Helper()
	return readFile(t, filepath.Join(fl.r.out, "report.txt"))
}

// lines returns the lines printed.
func (fl *fakeLive) lines() []string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return slices.Clone(fl.shown)
}

// secrets returns what no report, line printed or error of a run on the
// fake may hold: every identifier of the fixtures, the fake's correlation
// IDs, the run's temporary folder, in which all its paths lie, and the
// planted user name.
func (fl *fakeLive) secrets(t *testing.T) []string {
	t.Helper()
	secrets := []string{plantedUser, fl.dir, "00000000-0000-4000-8000-", schwabtest.StreamerCustomerID, schwabtest.StreamerCorrelID, schwabtest.AkamaiReference}
	if resolved, err := filepath.EvalSymlinks(fl.dir); err == nil {
		secrets = append(secrets, resolved)
	}
	for _, a := range schwabtest.FixtureAccounts {
		secrets = append(secrets, a.Number, a.Hash, a.Nickname, a.DisplayID)
	}
	rows, err := decodeList[Transaction](readTestdata(t, "transactions.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		for _, id := range []int64{row.ActivityID, row.OrderID, row.PositionID} {
			if id != 0 {
				secrets = append(secrets, strconv.FormatInt(id, 10))
			}
		}
	}
	return secrets
}

// noSecrets fails the test for each secret of fl's in texts, which it
// names without the text.
func (fl *fakeLive) noSecrets(t *testing.T, what string, texts ...string) {
	t.Helper()
	all := strings.ToLower(strings.Join(texts, "\n"))
	for _, s := range fl.secrets(t) {
		if strings.Contains(all, strings.ToLower(s)) {
			t.Errorf("%s holds %q, in some case", what, s)
		}
	}
}

// liveCacheListing returns what is under the user cache folder's
// schwabtx/live, where a live run's files go by default.
func liveCacheListing() []string {
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil
	}
	var names []string
	_ = filepath.WalkDir(filepath.Join(cache, "schwabtx", "live"), func(p string, _ fs.DirEntry, err error) error {
		if err == nil {
			names = append(names, p)
		}
		return nil
	})
	return names
}

// wantLines fails the test unless text holds each of lines, in order, each
// whole.
func wantLines(t *testing.T, text string, lines ...string) {
	t.Helper()
	rest := "\n" + text
	for _, l := range lines {
		i := strings.Index(rest, "\n"+l+"\n")
		if i < 0 {
			t.Errorf("no line %q in order in:\n%s", l, text)
			return
		}
		rest = rest[i+len(l)+1:]
	}
}

// A run on the fake reads the account numbers and the user's preferences
// first, then checks every account endpoint once: the accounts both ways,
// each account both ways, its last year of transactions and the first of
// them by id. Every request is a read, and every answer is in the report,
// with its status, Content-Type, correlation headers, records and findings,
// as are the keys seen and the guard's accounting. The report holds no
// identifier of the fixtures, no path of the run's and not the user name,
// and nor do the lines printed, each of which is in the report. The state
// and its key are made, and nothing is written to the owner's cache.
func TestLiveOnTheFake(t *testing.T) {
	before := liveCacheListing()
	fl := newFakeLive(t)
	if err := fl.run(t); err != nil {
		t.Fatal(err)
	}
	report := fl.report(t)
	t.Log(report)
	acct := "200, application/json, 1 record; correlation headers: Schwab-Client-Correlid, X-Request-Id"
	wantLines(t, report,
		"## the run", "toolchain: "+runtime.Version(), "commit: "+liveCommit, "time: 2025-03-01T15:00:00Z", "mode: default", "probes: none",
		"## accounts/accountNumbers", "200, application/json, 3 records; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"## userPreference", acct,
		"## accounts", "200, application/json, 3 records; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"## accounts?fields=positions",
		"## accounts/{account 1}", acct, "## accounts/{account 1}?fields=positions", acct,
		"## accounts/{account 1}/transactions", "200, application/json, 5 records; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"## accounts/{account 1}/transactions/{id}", acct, assetTypeSentTwice,
		"## accounts/{account 2}/transactions", "200, application/json, 2 records; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"## accounts/{account 2}/transactions/{id}", acct,
		"## accounts/{account 3}/transactions", "200, application/json, 0 records; correlation headers: Schwab-Client-Correlid, X-Request-Id",
		"## the keys seen", "AccountNumber: 3 records", "  new to liveKeys: none", "  of liveKeys, not seen: none",
		"## the guard", "budget: 180 requests, token requests included; 15 sent", "answers by status: 200: 15", "token requests: 0",
		"429s: 0", "failures of the network: 0", "refusals: none", "stopped: no", "did not run: none",
		"## before sharing", "W-8: no line reveals W-8 withholding rows", "the run passed",
	)
	if strings.Contains(report, "## accounts/{account 3}/transactions/{id}") {
		t.Error("an account without rows got a read by id")
	}
	fl.noSecrets(t, "the report", report)
	fl.noSecrets(t, "the lines printed", fl.lines()...)
	for _, line := range fl.lines() {
		if !strings.Contains(report, line+"\n") {
			t.Errorf("printed %q, which is not in the report", line)
		}
	}
	rs := fl.f.Requests()
	if len(rs) != 15 {
		t.Errorf("%d requests, want 15", len(rs))
	}
	for i, r := range rs {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.Path, "/trader/v1/") || i < 2 && r.Route != []schwabtest.Route{schwabtest.AccountNumbersRoute, schwabtest.UserPreferenceRoute}[i] {
			t.Errorf("request %d: %s %s, %s", i, r.Method, r.Path, r.Route)
		}
	}
	var st stateJSON
	if err := json.Unmarshal([]byte(readFile(t, fl.r.stateFile)), &st); err != nil || st.Version != stateVersion {
		t.Errorf("the state: %+v, %v", st, err)
	}
	if runtime.GOOS != "windows" {
		for path, want := range map[string]os.FileMode{
			fl.r.out: 0o700, filepath.Join(fl.r.out, "report.txt"): 0o600, fl.r.stateFile: 0o600, fl.r.keyFile: 0o600,
		} {
			if info, err := os.Stat(path); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s: %v, %v; want mode %v", path, info.Mode(), err, want)
			}
		}
	}
	if after := liveCacheListing(); !slices.Equal(before, after) {
		t.Errorf("the owner's live folder held %q, and now %q", before, after)
	}
}

// A guard of the fake's origin, apart from the run, refuses an order and a
// login, and sends neither to the fake: the run's own guard refuses them as
// this one does, and a refusal of its would stop the run.
func TestLiveGuardOnTheFake(t *testing.T) {
	fl := newFakeLive(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	g, err := newGuard(ctx, cancel, fl.f.URL(), fl.f.Client().Transport, 10)
	if err != nil {
		t.Fatal(err)
	}
	client := liveHTTPClient(g)
	before := fl.f.AllRequests()
	for _, tc := range []struct{ path, typ, body string }{
		{"/trader/v1/accounts/" + schwabtest.FixtureAccounts[0].Hash + "/orders", "application/json", `{"orderType":"MARKET"}`},
		{tokenPath, "application/x-www-form-urlencoded", "grant_type=authorization_code&code=" + testCode + "&redirect_uri=" + schwabtest.CallbackURL},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, fl.f.URL()+tc.path, strings.NewReader(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", tc.typ)
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "the live run's guard ") {
			t.Errorf("POST %s: %v", tc.path, err)
		}
	}
	if got := fl.f.AllRequests(); got != before || context.Cause(ctx) == nil {
		t.Errorf("the fake had %d requests, and %d before; the run's cause %v", got, before, context.Cause(ctx))
	}
}

// What a run prints and writes holds nothing that it has seen: a warning
// that names the token file, and Schwab's errors that name an account, its
// hash, its nickname, its display ID and a row, with the correlation ID
// that came with one, are scrubbed in the report, in the lines printed and
// in the run's error. A check that fails with an error fails the run.
func TestLiveScrubsWhatItPrints(t *testing.T) {
	fl := newFakeLive(t)
	a1, a2 := schwabtest.FixtureAccounts[0], schwabtest.FixtureAccounts[1]
	fl.f.Queue(schwabtest.AccountsRoute, schwabtest.Answer(http.StatusBadRequest,
		`{"message":"Invalid account number","errors":["`+a1.Number+` (`+a1.Nickname+`, `+a1.DisplayID+`) is not `+a2.Hash+`"]}`,
		"Content-Type", "application/json", "Schwab-Client-CorrelId", "corr-zq9-planted"))
	fl.f.Queue(schwabtest.TransactionRoute, schwabtest.Answer(http.StatusNotFound,
		`{"message":"no transaction 73508261946 in `+a1.Hash+`"}`, "Content-Type", "application/json"))
	if runtime.GOOS != "windows" {
		if err := os.Chmod(fl.r.tokenFile, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	err := fl.run(t)
	if err == nil || !strings.Contains(err.Error(), "the check of accounts failed with an error") ||
		!strings.Contains(err.Error(), "the check of accounts/{account 1}/transactions/{id} failed with an error") {
		t.Errorf("the run's error: %v", err)
	}
	report := fl.report(t)
	t.Log(report)
	for _, want := range []string{
		"## accounts\nfailed with 400, application/json; correlation headers: Schwab-Client-Correlid\n" +
			"failed: HTTP 400 from Schwab: Invalid account number; {account 1} ({nickname 1}, {display id 1}) is not {hash 2} (correlation ID {correlation id ",
		"failed: HTTP 404 from Schwab: no transaction {activity id ",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report lacks %q", want)
		}
	}
	if runtime.GOOS != "windows" && !strings.Contains(report, "## warning\n{token file} could be read by others; it is now readable only by you\n") {
		t.Error("the report lacks the token file's warning")
	}
	for _, what := range []struct{ name, text string }{
		{"the report", report}, {"the lines printed", strings.Join(fl.lines(), "\n")}, {"the run's error", fmt.Sprint(err)},
	} {
		fl.noSecrets(t, what.name, what.text)
		if strings.Contains(what.text, "corr-zq9-planted") {
			t.Errorf("%s holds the correlation ID", what.name)
		}
	}
}

// writeJSON writes body, JSON, with status, as the fake's own answers are
// written.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// noLongWaits fails the test if waits, the clock's, hold a wait of a second
// or more: a Client that took a refusal for a failure of the network would
// wait 1, 2 and 4 seconds before it tried again.
func noLongWaits(t *testing.T, waits []time.Duration) {
	t.Helper()
	for _, d := range waits {
		if d >= time.Second {
			t.Errorf("waited %v, as for another try", d)
		}
	}
}

// Once the budget is spent the guard refuses the next request and stops the
// run, which the Client gives up on at once, and the report says why and
// what did not run.
func TestLiveStopsWhenTheBudgetIsSpent(t *testing.T) {
	fl := newFakeLive(t)
	fl.r.budget = 3
	err := fl.run(t)
	const spent = "the budget of 3 requests, token requests included, is spent"
	if err == nil || !strings.Contains(err.Error(), "it stopped before its end: "+spent) {
		t.Errorf("the run's error: %v", err)
	}
	report := fl.report(t)
	wantLines(t, report, "## accounts?fields=positions",
		"failed: GET /trader/v1/accounts: the live run's guard refused a request beyond the budget of 3, token requests included",
		"## the guard", "budget: 3 requests, token requests included; 3 sent", "refusals: none",
		"stopped: "+spent, "did not run: the checks (stopped part way)")
	if n := fl.f.APICalls(); n != 3 {
		t.Errorf("the fake had %d requests, want 3", n)
	}
	noLongWaits(t, fl.clock.got())
}

// A request that the guard refuses stops the run at once: the Client does
// not try it again, and the probes after it do not run.
func TestLiveStopsAtARefusal(t *testing.T) {
	fl := newFakeLive(t)
	var getErr error
	var waits []time.Duration
	fl.r.probes = []liveProbe{
		{name: "Z1", mode: modeDefault, run: func(ctx context.Context, r *liveRun) verdict {
			n := len(fl.clock.got())
			_, getErr = r.client.Get(ctx, "/v1/oauth/authorize", url.Values{"client_id": {testKey}})
			waits = fl.clock.got()[n:]
			return verdict{answer: "refused"}
		}},
		{name: "Z2", mode: modeDefault, run: func(context.Context, *liveRun) verdict {
			t.Error("probe Z2 ran after the run stopped")
			return verdict{}
		}},
	}
	before := fl.f.AllRequests()
	err := fl.run(t)
	const refused = "a GET outside /trader/v1/ and /marketdata/v1/"
	if err == nil || !strings.Contains(err.Error(), "it stopped before its end: the live run's guard refused "+refused) {
		t.Errorf("the run's error: %v", err)
	}
	if getErr == nil || !strings.HasSuffix(getErr.Error(), "the live run's guard refused "+refused) {
		t.Errorf("the refused read: %v", getErr)
	}
	noLongWaits(t, waits)
	wantLines(t, fl.report(t), "## probe Z1", "verdict: refused", "## the guard", "refusals: "+refused,
		"stopped: the live run's guard refused "+refused, "did not run: probe Z1 (stopped part way), probe Z2")
	if got := fl.f.AllRequests() - before; got != 15 {
		t.Errorf("the fake had %d requests, want the checks' 15", got)
	}
}

// Three failures of the network in a row stop the run; the error, which
// names the account's path, names it by its label.
func TestLiveStopsAfterThreeNetworkFailures(t *testing.T) {
	fl := newFakeLive(t)
	fl.f.Queue(schwabtest.AccountRoute, schwabtest.Cut, schwabtest.Cut, schwabtest.Cut)
	err := fl.run(t)
	if err == nil || !strings.Contains(err.Error(), "it stopped before its end: three failures of the network in a row") {
		t.Errorf("the run's error: %v", err)
	}
	report := fl.report(t)
	if !strings.Contains(report, "## accounts/{account 1}\nfailed: GET /trader/v1/accounts/{hash 1} after 3 tries: ") {
		t.Errorf("the report:\n%s", report)
	}
	wantLines(t, report, "failures of the network: 3", "stopped: three failures of the network in a row", "did not run: the checks (stopped part way)")
	fl.noSecrets(t, "the report", report)
	if n := fl.f.APICalls(); n != 7 {
		t.Errorf("the fake had %d requests, want 7", n)
	}
}

// An Access Denied page from Schwab's edge stops the run, and its Reference
// is shown only by its label.
func TestLiveStopsAtTheEdge(t *testing.T) {
	fl := newFakeLive(t)
	fl.f.Queue(schwabtest.AccountsRoute, schwabtest.AccessDenied)
	err := fl.run(t)
	if err == nil || !strings.Contains(err.Error(), "it stopped before its end: Schwab's edge network refused a request with an Access Denied page") {
		t.Errorf("the run's error: %v", err)
	}
	report := fl.report(t)
	if !strings.Contains(report, "failed: HTTP 403 from Schwab: an HTML page, Access Denied (reference #{reference 1})") {
		t.Errorf("the report:\n%s", report)
	}
	fl.noSecrets(t, "the report", report)
	if n := fl.f.APICalls(); n != 3 {
		t.Errorf("the fake had %d requests, want 3", n)
	}
}

// A run keeps to its deadline, by its own clock: a mode whose plan would
// outlast it does not start, the default mode's once the account numbers
// say how many accounts there are and an opt-in mode's before any request;
// and a wait or a read past it stops the run, so that the probes after it
// do not run.
func TestLiveKeepsToItsDeadline(t *testing.T) {
	t.Run("the default plan", func(t *testing.T) {
		fl := newFakeLive(t)
		fl.r.deadline = liveNow.Add(time.Minute)
		err := fl.run(t)
		if err == nil || !strings.Contains(err.Error(), "the default run takes about 4m0s, more than the test's time limit leaves; add -timeout 0") ||
			fl.f.APICalls() != 1 {
			t.Errorf("the run's error: %v, after %d requests", err, fl.f.APICalls())
		}
		wantLines(t, fl.report(t), "did not run: everything after the account numbers")
	})
	t.Run("an opt-in plan", func(t *testing.T) {
		fl := newFakeLive(t)
		fl.r.mode = modeRate
		err := fl.run(t)
		if err == nil || !strings.Contains(err.Error(), "the rate run takes about 10m0s, more than the test's time limit leaves; add -timeout 0") ||
			fl.f.APICalls() != 0 {
			t.Errorf("the run's error: %v, after %d requests", err, fl.f.APICalls())
		}
		wantLines(t, fl.report(t), "did not run: everything")
	})
	for _, how := range []string{"wait", "read"} {
		t.Run("a "+how+" past the deadline", func(t *testing.T) {
			fl := newFakeLive(t)
			fl.r.probes = []liveProbe{
				{name: "Z1", mode: modeDefault, run: func(ctx context.Context, r *liveRun) verdict {
					if how == "wait" {
						if err := r.wait(ctx, 10*time.Minute); !errors.Is(err, errRunDeadline) {
							t.Errorf("the wait: %v", err)
						}
						return verdict{answer: "waited"}
					}
					_ = r.sleep(ctx, 10*time.Minute)
					if _, err := r.get(ctx, "accounts", "/trader/v1/accounts", nil, "accounts"); err == nil {
						t.Error("a read past the deadline went")
					}
					return verdict{answer: "read"}
				}},
				{name: "Z2", mode: modeDefault, run: func(context.Context, *liveRun) verdict {
					t.Error("probe Z2 ran after the deadline")
					return verdict{}
				}},
			}
			err := fl.run(t)
			if err == nil || !strings.Contains(err.Error(), "it stopped before its end: the run's deadline passed") {
				t.Errorf("the run's error: %v", err)
			}
			if how == "wait" && !fl.clock.Now().Before(fl.r.deadline) {
				t.Error("the wait went past the deadline")
			}
			if n := fl.f.APICalls(); n != 15 {
				t.Errorf("the fake had %d requests, want the checks' 15", n)
			}
			wantLines(t, fl.report(t), "did not run: probe Z1 (stopped part way), probe Z2")
		})
	}
}

// A run's context ends at its deadline by the real clock as well, so that
// a request in flight is cut short too, however long it hangs.
func TestLiveContextEndsAtTheDeadline(t *testing.T) {
	fl := newFakeLive(t)
	var deadline time.Time
	var ok bool
	probes := []liveProbe{{name: "Z1", mode: modeDefault, run: func(ctx context.Context, _ *liveRun) verdict {
		deadline, ok = ctx.Deadline()
		return verdict{answer: "looked"}
	}}}
	fl.r.probes = probes
	start := time.Now()
	if err := fl.run(t); err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	// The deadline is as far from the run's start by the real clock as by
	// the run's.
	left := fl.r.deadline.Sub(liveNow)
	if !ok || deadline.Before(start.Add(left)) || deadline.After(end.Add(left)) {
		t.Errorf("the context's deadline is %v from the run's start (%v); the run's, by its clock, %v", deadline.Sub(start), ok, left)
	}
	none := newFakeLive(t)
	none.r.probes, none.r.deadline = probes, time.Time{}
	if err := none.run(t); err != nil || ok {
		t.Errorf("without a deadline: %v; the context has one: %v", err, ok)
	}
}

// A mode whose plan would outlast the login does not start, and says to
// log in first.
func TestLiveKeepsToTheLogin(t *testing.T) {
	fl := newFakeLive(t)
	fl.clock.set(liveNow.Add(LoginLifetime - 10*time.Minute))
	fl.r.mode, fl.r.deadline = modeFills, time.Time{}
	err := fl.run(t)
	if err == nil || !strings.Contains(err.Error(), "the login lapses before the watch=fills run would end; run `schwabtx login` first") ||
		fl.f.APICalls() != 0 {
		t.Errorf("the run's error: %v, after %d requests", err, fl.f.APICalls())
	}
}

// The probes of the run's mode run in turn, after the checks, and their
// verdicts and evidence are in the report, a W-8 line marked and counted
// before sharing; a probe that panics is a line without values, and the
// probes after it still run; a probe of another mode does not run. An
// opt-in mode runs its probes alone, after the account numbers and the
// user's preferences, with its own budget.
func TestLiveProbes(t *testing.T) {
	var ran []string
	probes := []liveProbe{
		{name: "Z1", mode: modeDefault, run: func(context.Context, *liveRun) verdict {
			ran = append(ran, "Z1")
			return verdict{answer: "settled", evidence: []reportLine{{text: "rows: 3"}, {text: "a W-8 Withholding row of 29470135", w8: true}}}
		}},
		{name: "Z2", mode: modeDefault, run: func(_ context.Context, r *liveRun) verdict {
			ran = append(ran, "Z2")
			panic("account 29470135 in " + r.out)
		}},
		{name: "Z3", mode: modeRate, run: func(context.Context, *liveRun) verdict {
			ran = append(ran, "Z3")
			return verdict{answer: "rated"}
		}},
		{name: "Z4", mode: modeDefault, run: func(context.Context, *liveRun) verdict {
			ran = append(ran, "Z4")
			return verdict{answer: "inconclusive"}
		}},
	}
	fl := newFakeLive(t)
	fl.r.probes = probes
	err := fl.run(t)
	if fmt.Sprint(err) != "the live run failed: probe Z2 panicked" || !slices.Equal(ran, []string{"Z1", "Z2", "Z4"}) {
		t.Errorf("the run's error: %v; ran %q", err, ran)
	}
	report := fl.report(t)
	wantLines(t, report, "probes: Z1, Z2, Z4",
		"## probe Z1", "verdict: settled", "rows: 3", "[W-8] a W-8 Withholding row of {account 1}",
		"## probe Z2", "panicked, so it has no verdict", "## probe Z4", "verdict: inconclusive",
		"W-8: 1 line, marked [W-8], reveal W-8 withholding rows, which show nonresident tax status; decide on each before sharing the report",
		"the run failed: probe Z2 panicked")
	fl.noSecrets(t, "the report", report)
	fl.noSecrets(t, "the lines printed", fl.lines()...)

	ran = nil
	rate := newFakeLive(t)
	rate.r.probes, rate.r.mode, rate.r.deadline = probes, modeRate, time.Time{}
	if err := rate.run(t); err != nil || !slices.Equal(ran, []string{"Z3"}) {
		t.Errorf("the rate run: %v; ran %q", err, ran)
	}
	wantLines(t, rate.report(t), "mode: rate", "probes: Z3", "## accounts/accountNumbers", "## userPreference", "## probe Z3", "verdict: rated",
		"budget: 320 requests, token requests included; 2 sent")
	if n := rate.f.APICalls(); n != 2 {
		t.Errorf("the rate run sent %d requests, want 2", n)
	}
}

// A probe's quiet reads are each checked, and each written to the report
// only when it fails or holds a finding that the report lacks for its
// endpoint; a finding that fails counts once, however many answers hold
// it, and a failed read is written each time.
func TestLiveReadsQuietly(t *testing.T) {
	fl := newFakeLive(t)
	account := `{"securitiesAccount":` + baseSecuritiesAccount + `,"zqNewKey":1}`
	fl.f.OverridePath("GET /trader/v1/zq/{n}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("n") {
		case "unknown":
			writeJSON(w, http.StatusOK, "["+account+"]")
		case "refused":
			writeJSON(w, http.StatusBadRequest, `{"message":"a guess: refused"}`)
		default:
			writeJSON(w, http.StatusOK, string(readTestdata(t, "transaction.json")))
		}
	})
	fl.r.probes = []liveProbe{{name: "Z1", mode: modeDefault, run: func(ctx context.Context, r *liveRun) verdict {
		for _, read := range []struct{ endpoint, path, kind string }{
			{"zq repeated", "/trader/v1/zq/repeated", "transaction"},
			{"zq repeated", "/trader/v1/zq/repeated", "transaction"},
			{"zq repeated again", "/trader/v1/zq/repeated", "transaction"},
			{"zq unknown", "/trader/v1/zq/unknown", "accounts"},
			{"zq unknown", "/trader/v1/zq/unknown", "accounts"},
			{"zq refused", "/trader/v1/zq/refused", "accounts"},
			{"zq refused", "/trader/v1/zq/refused", "accounts"},
			{"zq orders", "/trader/v1/zq/orders", ""},
		} {
			_, _ = r.read(ctx, read.endpoint, read.path, nil, read.kind)
		}
		return verdict{answer: "read"}
	}}}
	err := fl.run(t)
	if fmt.Sprint(err) != "the live run failed: 1 finding fails" {
		t.Errorf("the run's error: %v", err)
	}
	report := fl.report(t)
	for title, n := range map[string]int{"zq repeated": 1, "zq repeated again": 1, "zq unknown": 1, "zq refused": 2, "zq orders": 0} {
		if got := strings.Count(report, "\n## "+title+"\n"); got != n {
			t.Errorf("%d sections of %s, want %d:\n%s", got, title, n, report)
		}
	}
	wantLines(t, report, "## zq repeated", "200, application/json, 1 record; no correlation header", assetTypeSentTwice,
		"## zq unknown", "200, application/json, 1 record; no correlation header", "unknown key: zqNewKey: no field takes it",
		"## zq refused", "failed with 400, application/json; no correlation header")
}

// A run's folders are refused inside the module or a git work tree, an out
// folder that holds a report or a recording is never written over, and in
// each case nothing is sent or made.
func TestLiveRefusesItsFolders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setUp func(t *testing.T, r *liveRun)
		want  string
	}{
		{"the out folder inside the module", func(t *testing.T, r *liveRun) {
			r.module = plantModule(t)
			r.out = filepath.Join(r.module, "out")
		}, "{out folder} is inside the module; name another folder with -live.out"},
		{"the state inside a git work tree", func(t *testing.T, r *liveRun) {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
				t.Fatal(err)
			}
			r.stateFile, r.keyFile = filepath.Join(dir, "live", "state.json"), filepath.Join(dir, "live", "state.key")
		}, "{state folder} is inside a git work tree; name another folder with -live.state"},
		{"an out folder that holds a report", func(t *testing.T, r *liveRun) {
			if err := os.MkdirAll(r.out, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(r.out, "report.txt"), []byte("an earlier report"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, "{out folder}" + string(filepath.Separator) + "report.txt holds a report already, which is never written over; name another folder with -live.out"},
		{"an out folder that holds a recording", func(t *testing.T, r *liveRun) {
			r.record = true
			if err := os.MkdirAll(filepath.Join(r.out, "raw"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "{out folder}" + string(filepath.Separator) + "raw holds a recording already, which is never written over; name another folder with -live.out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fl := newFakeLive(t)
			tc.setUp(t, fl.r)
			err := fl.run(t)
			if fmt.Sprint(err) != tc.want {
				t.Errorf("the run's error: %v, want %q", err, tc.want)
			}
			if fl.f.APICalls() != 0 {
				t.Errorf("%d requests", fl.f.APICalls())
			}
			if _, err := os.Stat(fl.r.keyFile); err == nil {
				t.Error("the state's key was made")
			}
			if b, err := os.ReadFile(filepath.Join(fl.r.out, "report.txt")); err == nil && string(b) != "an earlier report" {
				t.Errorf("a report was written:\n%s", b)
			}
		})
	}
}

// With -live.record, each API answer is kept raw, its bytes as sent and
// what came with them, and a token request never is.
func TestLiveRecords(t *testing.T) {
	fl := newFakeLive(t)
	fl.r.record = true
	if err := fl.clock.Sleep(t.Context(), 31*time.Minute); err != nil { // the access token lapses, so the run refreshes it
		t.Fatal(err)
	}
	fl.r.deadline = fl.clock.Now().Add(9 * time.Minute)
	if err := fl.run(t); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(fl.r.out, "raw")
	entries, err := os.ReadDir(raw)
	if err != nil || len(entries) != 2*15 {
		t.Fatalf("the recording holds %d files, %v; want two for each of the 15 answers", len(entries), err)
	}
	body, a := readRaw(t, raw, 1)
	for _, e := range elements(t, readTestdata(t, "account_numbers.json")) {
		if !strings.Contains(string(body), string(e)) {
			t.Errorf("001.body lacks %s:\n%s", e, body)
		}
	}
	if a.Request.Method != http.MethodGet || a.Request.Path != "/trader/v1/accounts/accountNumbers" || a.Response.Status != http.StatusOK ||
		a.Response.Proto != "HTTP/2.0" || !slices.Equal(a.Response.Headers["Content-Type"], []string{"application/json"}) ||
		len(a.Response.Headers["Schwab-Client-Correlid"]) != 1 || a.Commit != liveCommit || !a.Sent.Equal(liveNow.Add(31*time.Minute)) {
		t.Errorf("001.json: %+v", a)
	}
	for n := 1; n <= 15; n++ {
		if _, a := readRaw(t, raw, n); a.Request.Path == tokenPath || !strings.HasPrefix(a.Request.Path, "/trader/v1/") {
			t.Errorf("answer %d kept is of %s", n, a.Request.Path)
		}
	}
	wantLines(t, fl.report(t), "recording: each answer, raw, in the out folder's raw folder, which is never to be shared", "token requests: 1")
}

// The state carries what a probe keeps from one run to the next, under
// digests that stay the same, since the key does; the state holds no
// value that was digested.
func TestLiveStateAcrossRuns(t *testing.T) {
	var digests []string
	probe := liveProbe{name: "Z1", mode: modeDefault, run: func(_ context.Context, r *liveRun) verdict {
		d := r.state.digest("account number", r.accounts[0].number)
		var before string
		ok, err := r.state.get("Z1", &before)
		if err != nil {
			t.Error(err)
		}
		if err := r.state.put("Z1", d); err != nil {
			t.Error(err)
		}
		digests = append(digests, d)
		if ok && before == d {
			return verdict{answer: "same"}
		}
		return verdict{answer: "new"}
	}}
	first := newFakeLive(t)
	first.r.probes = []liveProbe{probe}
	if err := first.run(t); err != nil {
		t.Fatal(err)
	}
	second := newFakeLive(t)
	second.r.probes = []liveProbe{probe}
	second.r.stateFile, second.r.keyFile = first.r.stateFile, first.r.keyFile
	if err := second.run(t); err != nil {
		t.Fatal(err)
	}
	wantLines(t, first.report(t), "## probe Z1", "verdict: new")
	wantLines(t, second.report(t), "## probe Z1", "verdict: same")
	if len(digests) != 2 || digests[0] != digests[1] {
		t.Errorf("digests %q", digests)
	}
	if strings.Contains(readFile(t, first.r.stateFile), schwabtest.FixtureAccounts[0].Number) {
		t.Error("the state holds the account number")
	}
}

// A finding that fails, such as a key that no field takes or an object sent
// twice, fails the run, and the report names it by its key path; a finding
// that is only reported does not.
func TestLiveFailsOnFindings(t *testing.T) {
	fl := newFakeLive(t)
	account := `{"securitiesAccount":` + baseSecuritiesAccount + `,"zqNewKey":1,"aggregatedBalance":{},"aggregatedBalance":{}}`
	fl.f.Queue(schwabtest.AccountsRoute, schwabtest.Answer(http.StatusOK, "["+account+"]", "Content-Type", "application/json"))
	err := fl.run(t)
	if fmt.Sprint(err) != "the live run failed: 3 findings fail" {
		t.Errorf("the run's error: %v", err)
	}
	wantLines(t, fl.report(t), "## accounts", "200, application/json, 1 record; no correlation header",
		"problem: aggregatedBalance: Schwab sent it more than once, so what it holds is uncertain",
		"unknown key: zqNewKey: no field takes it",
		"repeated key: aggregatedBalance: sent 2 times, an object or array among the copies",
		"## accounts/{account 1}/transactions/{id}", assetTypeSentTwice,
		"the run failed: 3 findings fail")
}

// The keys seen are listed for every type of record that the census met,
// and for every type of liveKeys: the records, the keys that liveKeys does
// not name, the keys of liveKeys that no record held, and the optional
// fields that every record held.
func TestLiveKeysSeen(t *testing.T) {
	r := &liveRun{census: newCensus()}
	for _, rec := range []string{
		withKeys(baseTransaction, "activityType", `"ACTIVITY_CORRECTION"`, "orderId", "1"),
		withKeys(baseTransaction, "orderId", "2"),
	} {
		var v any
		if err := json.Unmarshal([]byte(rec), &v); err != nil {
			t.Fatal(err)
		}
		r.census.add(reflect.TypeFor[Transaction](), v)
	}
	var lines []string
	for _, l := range r.keysSeen().lines {
		lines = append(lines, l.text)
	}
	wantLines(t, strings.Join(lines, "\n")+"\n", "AccountNumber: no record seen",
		"Transaction: 2 records", "  new to liveKeys: activityType",
		"  of liveKeys, not seen: settlementDate, description, positionId, qualifiedDividend",
		"  optional, on every record: orderId", "UserPreference: no record seen")
}

// Without the account numbers, which the report's scrubber is built from,
// the run stops; without the user's preferences it goes on, and fails.
func TestLiveNeedsItsFirstAnswers(t *testing.T) {
	fl := newFakeLive(t)
	fl.f.Queue(schwabtest.AccountNumbersRoute, schwabtest.InvalidAccount)
	err := fl.run(t)
	if err == nil || !strings.Contains(err.Error(), "it stopped before its end: the account numbers could not be read, and the report's scrubber is built from them") ||
		fl.f.APICalls() != 1 {
		t.Errorf("the run's error: %v, after %d requests", err, fl.f.APICalls())
	}
	wantLines(t, fl.report(t), "## accounts/accountNumbers", "failed with 400, application/json; correlation headers: Schwab-Client-Correlid",
		"did not run: the account numbers (stopped part way), the user's preferences, the checks")

	fl = newFakeLive(t)
	fl.f.Queue(schwabtest.UserPreferenceRoute, schwabtest.InvalidAccount)
	err = fl.run(t)
	if fmt.Sprint(err) != "the live run failed: the user's preferences could not be read" || fl.f.APICalls() != 15 {
		t.Errorf("the run's error: %v, after %d requests", err, fl.f.APICalls())
	}
}

// An account of the account numbers' answer is named by its place in it,
// its number and its hash alike, even when an account before it lacks a
// hash, which fails the run.
func TestLiveLabelsAccountsByTheirPlace(t *testing.T) {
	fl := newFakeLive(t)
	a2 := schwabtest.FixtureAccounts[1]
	fl.f.Queue(schwabtest.AccountNumbersRoute, schwabtest.Answer(http.StatusOK,
		`[{"accountNumber":"`+schwabtest.FixtureAccounts[0].Number+`"},{"accountNumber":"`+a2.Number+`","hashValue":"`+a2.Hash+`"}]`,
		"Content-Type", "application/json"))
	fl.f.Queue(schwabtest.TransactionsRoute, schwabtest.Answer(http.StatusNotFound,
		`{"message":"no window of `+a2.Hash+`"}`, "Content-Type", "application/json"))
	err := fl.run(t)
	if err == nil || !strings.Contains(err.Error(), "the account numbers gave accounts/{account 1} no hash") ||
		!strings.Contains(err.Error(), "the check of accounts/{account 2}/transactions failed with an error") {
		t.Errorf("the run's error: %v", err)
	}
	report := fl.report(t)
	if !strings.Contains(report, "## accounts/{account 2}/transactions\nfailed with 404") ||
		!strings.Contains(report, "failed: HTTP 404 from Schwab: no window of {hash 2}\n") {
		t.Errorf("the report:\n%s", report)
	}
	fl.noSecrets(t, "the report", report)
}

// What a probe says of an answer that it read itself, past the run's own
// reads, holds no correlation ID either: the guard learns those of every
// answer as it arrives. Nor does it hold the home folder, shown as ~, or the
// user name, in a path elsewhere.
func TestLiveScrubsWhatAProbeSays(t *testing.T) {
	fl := newFakeLive(t)
	elsewhere := string(filepath.Separator) + filepath.Join("srv", plantedUser, "x")
	fl.r.probes = []liveProbe{{name: "Z1", mode: modeDefault, run: func(ctx context.Context, r *liveRun) verdict {
		_, err := r.client.Get(ctx, "/trader/v1/accounts/"+schwabtest.UnknownHash, nil)
		return verdict{answer: fmt.Sprint(err), evidence: []reportLine{{text: "looked in " + filepath.Join(r.home, "y") + " and " + elsewhere}}}
	}}}
	if err := fl.run(t); err != nil {
		t.Fatal(err)
	}
	report := fl.report(t)
	sep := string(filepath.Separator)
	if !strings.Contains(report, "## probe Z1\nverdict: HTTP 400 from Schwab: Invalid account number (correlation ID {correlation id ") ||
		!strings.Contains(report, "\nlooked in ~"+sep+"y and "+sep+"srv"+sep+"{user}"+sep+"x\n") {
		t.Errorf("the report:\n%s", report)
	}
	fl.noSecrets(t, "the report", report)
}

// A run whose login talks to another server than the guard's origin, or
// whose origin is not https, sends nothing.
func TestLiveTakesItsOrigin(t *testing.T) {
	for origin, want := range map[string]string{
		DefaultBaseURL:           "the login's base URL is not the run's origin",
		"http://127.0.0.1:41234": `the live run's origin "http://127.0.0.1:41234" is not an https address alone`,
	} {
		fl := newFakeLive(t)
		fl.r.origin = origin
		before := fl.f.AllRequests() // the login's
		if err := fl.run(t); fmt.Sprint(err) != want || fl.f.AllRequests() != before {
			t.Errorf("origin %s: %v, after %d requests; want %q", origin, err, fl.f.AllRequests()-before, want)
		}
	}
}

// The report's words for what an answer lacks: a Content-Type, correlation
// headers, records of its kind; the names of correlation headers in order;
// and the first row with an id, past rows without one.
func TestLiveReportWords(t *testing.T) {
	if got := contentType(http.Header{}); got != "no Content-Type" {
		t.Errorf("contentType = %q", got)
	}
	if got := correlationNames(http.Header{"Content-Type": {"x"}}); got != "no correlation header" {
		t.Errorf("correlationNames = %q", got)
	}
	if got := correlationNames(http.Header{"X-Request-Id": {"1"}, "Schwab-Client-Correlid": {"2"}}); got != "correlation headers: Schwab-Client-Correlid, X-Request-Id" {
		t.Errorf("correlationNames = %q", got)
	}
	r := &liveRun{census: newCensus()}
	for kind, want := range map[string]string{
		"accounts":     "no records read",
		"transactions": "0 records",
		"account":      "1 record",
	} {
		body := map[string]string{"accounts": `{"x":1}`, "transactions": "[]", "account": `{"securitiesAccount":` + baseSecuritiesAccount + `}`}[kind]
		if got := r.records(kind, &reply{header: http.Header{}, body: []byte(body)}); got != want {
			t.Errorf("records(%s, %s) = %q, want %q", kind, body, got, want)
		}
	}
	for _, tc := range []struct{ body, want string }{
		{`[` + withKeys(baseTransaction, "activityId", "") + `,` + withKeys(baseTransaction, "activityId", "73508261946") + `]`, "73508261946"},
		{`[` + withKeys(baseTransaction, "activityId", "") + `]`, ""},
		{`{}`, ""},
	} {
		if got := firstActivityID(&reply{header: http.Header{}, body: []byte(tc.body)}); got != tc.want {
			t.Errorf("firstActivityID(%s) = %q, want %q", tc.body, got, tc.want)
		}
	}
	if got := firstActivityID(nil); got != "" {
		t.Errorf("firstActivityID(nil) = %q", got)
	}
}

// The guard's accounting in the report counts every answer by its status,
// a 429 that the Client tried again after among them.
func TestLiveCountsWhatItSent(t *testing.T) {
	fl := newFakeLive(t)
	fl.f.Queue(schwabtest.AccountsRoute, schwabtest.TooMany)
	if err := fl.run(t); err != nil {
		t.Fatal(err)
	}
	wantLines(t, fl.report(t), "## the guard", "budget: 180 requests, token requests included; 16 sent",
		"answers by status: 200: 15, 429: 1", "token requests: 0", "429s: 1", "failures of the network: 0")
}

// A run that cannot save its state, keep its recording or write its report
// fails, saying so.
func TestLiveFailsWhenItCannotKeepItsFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spoil func(r *liveRun) error
		want  string
	}{
		{"the state", func(r *liveRun) error { return os.MkdirAll(filepath.Join(r.stateFile, "x"), 0o700) }, "the state could not be saved: "},
		{"the recording", func(r *liveRun) error {
			return os.WriteFile(filepath.Join(r.out, "raw", fmt.Sprintf("%03d.body", r.rec.n+1)), nil, 0o600)
		}, "the recording stopped: keeping answer 16: "},
		{"the report", func(r *liveRun) error {
			report := filepath.Join(r.out, "report.txt")
			return errors.Join(os.Remove(report), os.MkdirAll(filepath.Join(report, "x"), 0o700))
		}, "the report could not be written: open "}, // a section's, before the end's
	} {
		t.Run(tc.name, func(t *testing.T) {
			fl := newFakeLive(t)
			fl.r.record = true
			fl.r.probes = []liveProbe{{name: "Z1", mode: modeDefault, run: func(ctx context.Context, r *liveRun) verdict {
				if err := tc.spoil(r); err != nil {
					t.Fatal(err)
				}
				if _, err := r.get(ctx, "accounts", "/trader/v1/accounts", nil, "accounts"); err != nil {
					t.Error(err)
				}
				return verdict{answer: "broke " + tc.name}
			}}}
			err := fl.run(t)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the run's error: %v, want %q", err, tc.want)
			}
			fl.noSecrets(t, "the run's error", fmt.Sprint(err))
		})
	}
}
