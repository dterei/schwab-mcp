package schwab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	"time"
)

// The modes of a live run: the checks and the default probes, or one
// opt-in probe alone.
const (
	modeDefault  = "default"
	modeRate     = "rate"
	modeBalances = "watch=balances"
	modeFills    = "watch=fills"
)

// errRunDeadline is why a run stops at its deadline.
var errRunDeadline = errors.New("the run's deadline passed")

// errEdgePage is why a run stops at an Access Denied page of Schwab's edge
// network, after which more requests can be refused for about a quarter of
// an hour.
var errEdgePage = errors.New("Schwab's edge network refused a request with an Access Denied page")

// errLoginRefused and errAppRefused are why a run stops when Schwab refuses
// a refresh of the saved login, as it does once a later login has replaced
// it, or refuses the app's key or secret: no request after it could work.
var (
	errLoginRefused = errors.New("Schwab refused the saved login; run `schwabtx login` first")
	errAppRefused   = errors.New("Schwab refused the app's key or secret; check SCHWAB_APP_KEY and SCHWAB_APP_SECRET")
)

// A liveRun is a run of the live mode. Its caller gives it everything it
// uses, so that CI runs it against the fake in seconds, with a fake clock
// and nothing written outside a temporary folder, while TestLive gives it
// the real clock, the saved login, Schwab's origin and the owner's folders.
// runLive fills in the rest as the run goes; the probes read it. runLive
// puts its guard in the Auth's HTTP client and takes its warnings, so an
// Auth serves one run.
type liveRun struct {
	now    func() time.Time                                 // one clock for every time
	sleep  func(ctx context.Context, d time.Duration) error // and every wait
	auth   *Auth                                            // its Config.Now, sleep and jitter come from the clock
	client *Client                                          // its now, sleep and jitter too
	origin string                                           // the guard's: https://api.schwabapi.com, or the fake's
	// out is the folder of the report and the recording; stateFile and
	// keyFile are the state and its key; tokenFile and rateFile are the
	// Auth's and the Client's files, for the scrubber.
	out, stateFile, keyFile, tokenFile, rateFile string
	// home and cacheDir are the scrubber's ~, user name (home's last
	// element) and cache folder.
	home, cacheDir string
	module         string        // the module's root, which no folder of the run may be inside
	snapshot       time.Duration // when the scheduled snapshot runs, after midnight in loc
	loc            *time.Location
	pageCap        int       // the transactions in an answer from which a window is taken as cut short
	mode           string    // modeDefault, modeRate, modeBalances or modeFills
	budget         int       // how many requests may go, token requests included; 0 for the mode's
	deadline       time.Time // when the run must end, by its clock; zero for no limit
	record         bool      // keep each answer raw, under out/raw
	commit         string    // the commit and whether the tree is modified, or "unknown"
	probes         []liveProbe
	show           func(line string) // prints a line of the report, as -v does

	// What the run keeps as it goes, set by runLive.
	scrub    *liveScrubber
	report   *liveReport
	state    *stateStore
	guard    *guard
	rec      *recorder
	census   *census
	accounts []liveAccount // as the account numbers list them
	mu       sync.Mutex    // guards the report, which a warning may write from the Auth
	failures []string      // why the run fails, each without values
	failing  int           // the findings that fail
	notRun   []string      // the parts of the run that did not run, or stopped part way

	// What the probes share, set as they run: the history rows, the rows
	// that the probes have read, by their account's place in the list; and,
	// under mu, how many reads of the account API that a working login
	// passes went, and how many of them Schwab refused as unauthorized, and
	// the findings that read has written, each once.
	history          map[int][]Transaction
	counted, refused int
	reported         map[string]bool
}

// A liveAccount is an account of the run, as the account numbers list it.
type liveAccount struct {
	n            int // its place in the list, from 1, by which the report names it
	number, hash string
}

// endpoint returns the template by which the report names a path of the
// account's: accounts/{account n} and then rest.
func (a liveAccount) endpoint(rest string) string {
	return "accounts/{account " + strconv.Itoa(a.n) + "}" + rest
}

// A liveProbe is one of the probes of the open questions, which runs in one
// mode of the live run, under a recover, and returns its verdict.
type liveProbe struct {
	name string // as the report names it, such as "Q1"
	mode string // the mode it runs in
	run  func(ctx context.Context, r *liveRun) verdict
	// ready, if set, says before any request of the run whether the probe
	// can run now, as the rate probe needs a rate file that no other
	// process has written for two minutes. An error stops the run before
	// it sends anything, with why.
	ready func(ctx context.Context, r *liveRun) error
}

// A verdict is what a probe found: its answer to its question, such as "a
// year" or "inconclusive", and the evidence for it, as lines of the report.
// w8 marks an answer that reveals W-8 withholding rows, as the report marks
// such a line for the owner to decide on before sharing it.
type verdict struct {
	answer   string
	evidence []reportLine
	w8       bool
}

// livePlan returns how many requests a run in mode may send, token
// requests included, and about how long it takes, for a run of accounts
// accounts that starts at now:
//   - the default run, the checks and the default probes: 60 requests and
//     40 for each account, in about 2 minutes and 30 seconds more for each
//     account, the history walk's waits among them;
//   - the rate probe: 320, in about 10 minutes of one-minute steps;
//   - the watch of balances: 170, about 110 reads and 37 refreshes,
//     polling from 15:45 to 10:00 in New York;
//   - the watch of fills: 10 and 60 for each account polled, a poll a
//     minute for 30 minutes.
func livePlan(mode string, accounts int, now time.Time) (budget int, takes time.Duration) {
	switch mode {
	case modeRate:
		return 320, 10 * time.Minute
	case modeBalances:
		_, end := balancesWatch(now)
		return 170, end.Sub(now)
	case modeFills:
		return 10 + 60*accounts, 31 * time.Minute
	}
	return 60 + 40*accounts, 2*time.Minute + time.Duration(accounts)*30*time.Second
}

// runLive runs r: it reads the account numbers and the user's preferences,
// which the report's scrubber is built from, and then, in the default
// mode, checks every account endpoint's answer, and runs the probes of r's
// mode, writing the report as it goes. Every request goes through the
// guard, so that the run is read-only by construction. It returns why the
// run failed, scrubbed: a finding that fails, a stop before the end, a
// panic, or a line that the self-check caught.
func runLive(ctx context.Context, r *liveRun) (err error) {
	r.scrub = newLiveScrubber()
	r.scrub.setHome(r.home)
	for _, p := range []struct{ label, path string }{
		{"{cache folder}", r.cacheDir},
		{"{out folder}", r.out},
		{"{state folder}", filepath.Dir(r.stateFile)},
		{"{state file}", r.stateFile},
		{"{key file}", r.keyFile},
		{"{token file}", r.tokenFile},
		{"{rate file}", r.rateFile},
		{"{module}", r.module},
	} {
		r.scrub.addPath(p.label, p.path)
	}
	// The app's key and secret, and the login's tokens, which a refusal
	// could echo; tokens sent or given later are learned as they go.
	r.scrub.addIdent("app key", r.auth.cfg.AppKey)
	r.scrub.addIdent("app secret", r.auth.cfg.AppSecret)
	if t, err := readToken(r.auth.tokenFile); err == nil {
		r.scrub.addIdent("access token", t.AccessToken)
		r.scrub.addIdent("refresh token", t.RefreshToken)
	}
	defer func() {
		if recover() != nil {
			err = errors.New("the live run panicked")
		}
		if err != nil {
			text := r.scrub.scrub(err.Error())
			if r.scrub.holds(text) {
				text = "the live run failed, and its error held something that the run saw, so it is not shown"
			}
			err = errors.New(unmark(text))
		}
	}()
	if err := r.setUp(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if !r.deadline.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadlineCause(ctx, time.Now().Add(r.deadline.Sub(r.now())), errRunDeadline)
		defer stop()
	}
	if err := r.connect(ctx, cancel); err != nil {
		return err
	}
	r.run(ctx)
	return r.end(ctx)
}

// setUp makes the run's folders and files: the out folder and its report,
// the state and its key, and the recording, each refused inside the module
// or a git work tree.
func (r *liveRun) setUp() error {
	for _, d := range []struct{ dir, flag string }{
		{r.out, "-live.out"}, {filepath.Dir(r.stateFile), "-live.state"}, {filepath.Dir(r.keyFile), "-live.state"},
	} {
		if err := refuseInside(d.dir, r.module); err != nil {
			return fmt.Errorf("%w; name another folder with %s", err, d.flag)
		}
	}
	if r.record {
		// Refused before anything is made, as a report that is there is.
		if _, err := os.Lstat(filepath.Join(r.out, "raw")); err == nil {
			return fmt.Errorf("%s holds a recording already, which is never written over; name another folder with -live.out", filepath.Join(r.out, "raw"))
		}
	}
	if err := makePrivateDir(r.out); err != nil {
		return err
	}
	var err error
	if r.report, err = newLiveReport(filepath.Join(r.out, "report.txt"), r.scrub, r.showLine); err != nil {
		return fmt.Errorf("%w; name another folder with -live.out", err)
	}
	if r.state, err = openState(r.stateFile, r.keyFile, r.warn); err != nil {
		return err
	}
	if r.record {
		if r.rec, err = newRecorder(r.out, r.commit); err != nil {
			return fmt.Errorf("%w; name another folder with -live.out", err)
		}
	}
	r.census = newCensus()
	sec := &reportSection{title: "the run"}
	sec.add("toolchain: " + runtime.Version())
	sec.add("commit: " + r.commit)
	sec.add("time: " + r.now().UTC().Format(time.RFC3339))
	sec.add("mode: " + r.mode)
	sec.add("probes: " + r.probeNames())
	if r.record {
		sec.add("recording: each answer, raw, in the out folder's raw folder, which is never to be shared")
	}
	r.write(sec)
	return nil
}

// probeNames returns the names of the probes of r's mode, as the report
// lists them.
func (r *liveRun) probeNames() string {
	var names []string
	for _, p := range r.probes {
		if p.mode == r.mode {
			names = append(names, p.name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// connect sends every request of r's Auth and Client through a guard of
// r's origin, which stops the run with cancel, and tells the report of the
// Auth's warnings and the Client's. Each answer, whichever request drew
// it, is seen as it arrives, before its sender reads it: the scrubber
// learns the secrets that its request carried, its correlation IDs and
// the tokens of a grant, the recording keeps it, and one that is not a
// success goes to readRefusal.
func (r *liveRun) connect(ctx context.Context, cancel context.CancelCauseFunc) error {
	base, err := url.Parse(r.auth.cfg.BaseURL)
	if err != nil {
		return err
	}
	var next http.RoundTripper
	if hc := r.auth.cfg.HTTPClient; hc != nil {
		next = hc.Transport
	}
	budget := r.budget
	if budget <= 0 {
		budget, _ = livePlan(r.mode, 0, r.now())
	}
	g, err := newGuard(ctx, cancel, r.origin, next, budget)
	if err != nil {
		return err
	}
	if !g.isOrigin(base.Host) || base.Scheme != "https" {
		return errors.New("the login's base URL is not the run's origin")
	}
	g.now = r.now
	g.answered = func(req *http.Request, sent time.Time, resp *http.Response) {
		r.scrub.learnRequest(req)
		r.scrub.learnHeader(resp.Header)
		if r.rec != nil {
			r.rec.record(req, sent, resp)
		}
		switch {
		case resp.StatusCode/100 != 2:
			r.readRefusal(resp)
		case req.URL.EscapedPath() == tokenPath:
			body, _ := readWhole(resp)
			r.scrub.learnAnswer(body)
		}
	}
	r.guard = g
	r.auth.cfg.HTTPClient = liveHTTPClient(g)
	r.auth.cfg.Warn = r.warn
	return nil
}

// readRefusal reads resp, an answer that is not a success, and gives it a
// body that reads the same: the scrubber learns what it holds, and an
// Access Denied page of Schwab's edge network stops the run before the
// answer reaches the request's sender, so that nothing more is sent,
// whether it came to a probe's read, a request of its own or a token
// request.
func (r *liveRun) readRefusal(resp *http.Response) {
	body, _ := readWhole(resp)
	e := newError(resp.StatusCode, resp.Header, body)
	r.scrub.learnError(e)
	if e.Reference != "" {
		r.stop(errEdgePage)
	}
}

// run runs the parts of r in turn, until the run stops: the account numbers
// and the user's preferences, either of which failing fails the run, then
// the checks, in the default mode, and the probes of r's mode. A mode whose
// plan would outlast the run's deadline, or the login, does not start: an
// opt-in mode's before any request, and the default mode's, which depends
// on the accounts, after the account numbers. Nor does a run with a probe
// that is not ready, before any request.
func (r *liveRun) run(ctx context.Context) {
	if r.mode != modeDefault && !r.planFits(0) {
		r.notRun = append(r.notRun, "everything")
		return
	}
	for _, p := range r.probes {
		if p.mode != r.mode || p.ready == nil {
			continue
		}
		if err := p.ready(ctx, r); err != nil {
			r.stop(fmt.Errorf("probe %s cannot start: %w", p.name, err))
			r.notRun = append(r.notRun, "everything")
			return
		}
	}
	r.attempt(ctx, "the account numbers", r.readAccounts)
	if ctx.Err() == nil && r.mode == modeDefault && !r.planFits(len(r.accounts)) {
		r.notRun = append(r.notRun, "everything after the account numbers")
		return
	}
	if r.budget <= 0 {
		budget, _ := livePlan(r.mode, len(r.accounts), r.now())
		r.guard.setBudget(budget)
	}
	r.attempt(ctx, "the user's preferences", func(ctx context.Context) {
		if _, err := r.get(ctx, "userPreference", "/trader/v1/userPreference", nil, "userPreference"); err != nil && ctx.Err() == nil {
			r.fail("the user's preferences could not be read")
		}
	})
	if r.mode == modeDefault {
		r.attempt(ctx, "the checks", r.checks)
	}
	for _, p := range r.probes {
		if p.mode == r.mode {
			r.runProbe(ctx, p)
		}
	}
}

// planFits reports whether r's plan, for accounts accounts, ends before the
// run's deadline and before the login lapses. When it does not, it stops
// the run, saying what to do.
func (r *liveRun) planFits(accounts int) bool {
	now := r.now()
	_, takes := livePlan(r.mode, accounts, now)
	ends := now.Add(takes)
	if !r.deadline.IsZero() && ends.After(r.deadline) {
		r.stop(fmt.Errorf("the %s run takes about %v, more than the test's time limit leaves; add -timeout 0", r.mode, takes.Round(time.Minute)))
		return false
	}
	if st, err := r.auth.Status(); err == nil && !st.ExpiresAt.IsZero() && ends.After(st.ExpiresAt) {
		r.stop(fmt.Errorf("the login lapses before the %s run would end; run `schwabtx login` first", r.mode))
		return false
	}
	return true
}

// attempt runs step, a part of the run called name, under a recover that
// turns a panic into a line without values, such as "probe Q4 panicked",
// and reports whether it ran to its end without one. A step that the run's
// stop comes before is not run, and one that it comes in the middle of
// stops part way; the report lists both.
func (r *liveRun) attempt(ctx context.Context, name string, step func(ctx context.Context)) (ok bool) {
	if ctx.Err() != nil {
		r.notRun = append(r.notRun, name)
		return false
	}
	defer func() {
		if recover() != nil {
			r.fail(name + " panicked")
			ok = false
		}
		if ctx.Err() != nil {
			r.notRun = append(r.notRun, name+" (stopped part way)")
		}
	}()
	step(ctx)
	return true
}

// runProbe runs p and writes its verdict, or that it panicked.
func (r *liveRun) runProbe(ctx context.Context, p liveProbe) {
	var v verdict
	ran := false
	ok := r.attempt(ctx, "probe "+p.name, func(ctx context.Context) {
		ran = true
		v = p.run(ctx, r)
	})
	if !ran {
		return
	}
	sec := &reportSection{title: "probe " + p.name}
	if !ok {
		sec.add("panicked, so it has no verdict")
	} else {
		sec.lines = append(sec.lines, reportLine{text: "verdict: " + v.answer, w8: v.w8})
		sec.lines = append(sec.lines, v.evidence...)
	}
	r.write(sec)
}

// readAccounts reads the account numbers, which the report's scrubber is
// built from, labeling each account and its hash by its place in the list.
// Without them the run stops.
func (r *liveRun) readAccounts(ctx context.Context) {
	rep, err := r.get(ctx, "accounts/accountNumbers", "/trader/v1/accounts/accountNumbers", nil, "accountNumbers")
	if err != nil {
		r.stop(errors.New("the account numbers could not be read, and the report's scrubber is built from them"))
		return
	}
	nums, _ := decodeList[AccountNumber](rep.body)
	for i, n := range nums {
		r.accounts = append(r.accounts, liveAccount{n: i + 1, number: n.AccountNumber, hash: string(n.HashValue)})
	}
}

// checks reads every account endpoint once, so that each answer is checked
// against the records: the accounts, without their positions and with; and
// for each account, the account, both ways, its transactions of the last
// MaxTransactionWindow, and the first of them by its id. A check that
// fails with an error fails the run.
func (r *liveRun) checks(ctx context.Context) {
	positions := url.Values{"fields": {string(AccountFieldPositions)}}
	check := func(endpoint, path string, query url.Values, kind string) *reply {
		rep, err := r.get(ctx, endpoint, path, query, kind)
		if err != nil && ctx.Err() == nil {
			r.fail("the check of " + endpoint + " failed with an error")
		}
		return rep
	}
	check("accounts", "/trader/v1/accounts", nil, "accounts")
	check("accounts?fields=positions", "/trader/v1/accounts", positions, "accounts")
	for _, a := range r.accounts {
		if a.hash == "" {
			r.fail("the account numbers gave " + a.endpoint("") + " no hash")
			continue
		}
		path := "/trader/v1/accounts/" + url.PathEscape(a.hash)
		check(a.endpoint(""), path, nil, "account")
		check(a.endpoint("?fields=positions"), path, positions, "account")
		end := r.now()
		q, err := TransactionQuery{Start: end.Add(-MaxTransactionWindow), End: end}.values()
		if err != nil {
			panic(err) // a window of MaxTransactionWindow is always one
		}
		rep := check(a.endpoint("/transactions"), path+"/transactions", q, "transactions")
		if id := firstActivityID(rep); id != "" {
			check(a.endpoint("/transactions/"+idLabel), path+"/transactions/"+id, nil, "transaction")
		}
	}
}

// firstActivityID returns the first id in rep, an answer of transactions,
// or "" for none.
func firstActivityID(rep *reply) string {
	if rep == nil {
		return ""
	}
	rows, err := decodeList[Transaction](rep.body)
	if err != nil {
		return ""
	}
	for _, row := range rows {
		if row.ActivityID != 0 {
			return strconv.FormatInt(row.ActivityID, 10)
		}
	}
	return ""
}

// get reads path with query, an endpoint that answers with kind, and
// reports the answer under endpoint, a template such as
// accounts/{account 1}/transactions: its status, Content-Type, the names
// of its correlation headers and its records, and what checkAnswer finds,
// once the scrubber has learned the answer's identifiers. A finding that
// fails fails the run. A read that fails is reported by Schwab's status
// and words, or by what else failed, without the addresses that a failure
// of the network names. A kind of "" is an endpoint whose answers hold no
// records of this package's, such as the orders' or Market Data's, which
// is reported without records or findings. Once the run has stopped, or
// past its deadline, it sends nothing and returns ctx's error.
func (r *liveRun) get(ctx context.Context, endpoint, path string, query url.Values, kind string) (*reply, error) {
	return r.fetch(ctx, r.client, endpoint, path, query, kind, false, false)
}

// read is get for a probe that reads an endpoint many times, as Q12 does:
// it checks every answer as get does, but writes one to the report only
// when it fails or holds a finding that the report does not have yet for
// its endpoint, and counts a finding that fails once, however many answers
// hold it.
func (r *liveRun) read(ctx context.Context, endpoint, path string, query url.Values, kind string) (*reply, error) {
	return r.fetch(ctx, r.client, endpoint, path, query, kind, true, false)
}

// ask is read for a request whose answer is what a probe asks about, such
// as a window older than an account's history, a window longer than Schwab
// serves, or an id that no row has. Schwab may refuse such a request with
// a 401 or a 403 for reasons of its own, so, unlike get's and read's, its
// refusal is not counted among the reads of the account API that a working
// login passes, which Q6 reports.
func (r *liveRun) ask(ctx context.Context, endpoint, path string, query url.Values, kind string) (*reply, error) {
	return r.fetch(ctx, r.client, endpoint, path, query, kind, true, true)
}

// fetch is get through c, a Client of r's Auth, or read when quiet is set;
// asked marks a request whose refusal is not counted, as ask's is.
func (r *liveRun) fetch(ctx context.Context, c *Client, endpoint, path string, query url.Values, kind string, quiet, asked bool) (*reply, error) {
	if r.passed() {
		r.stop(errRunDeadline)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sec := &reportSection{title: endpoint}
	rep, err := c.get(ctx, path, query)
	se, isErr := errors.AsType[*Error](err)
	if !asked && strings.HasPrefix(path, "/trader/v1/") && (err == nil || isErr) {
		r.mu.Lock()
		r.counted++
		if isErr && (se.Status == http.StatusUnauthorized || se.Status == http.StatusForbidden) {
			r.refused++
		}
		r.mu.Unlock()
	}
	if err != nil {
		r.stopIfRefused(err)
		if isErr {
			sec.addf("failed with %d, %s; %s", se.Status, contentType(se.Header), correlationNames(se.Header))
		}
		sec.add("failed: " + failureText(err))
		r.write(sec)
		return nil, err
	}
	if kind == "accountNumbers" {
		nums, _ := decodeList[AccountNumber](rep.body)
		for i, n := range nums {
			r.scrub.addAccount(i+1, n.AccountNumber, string(n.HashValue))
		}
	}
	r.scrub.learnAnswer(rep.body)
	if kind == "" {
		sec.addf("%d, %s; %s", rep.status, contentType(rep.header), correlationNames(rep.header))
		if !quiet {
			r.write(sec)
		}
		return rep, nil
	}
	sec.addf("%d, %s, %s; %s", rep.status, contentType(rep.header), r.records(kind, rep), correlationNames(rep.header))
	news := false
	for _, f := range checkAnswer(kind, rep.body) {
		if quiet && !r.firstReport(endpoint, f) {
			continue
		}
		news = true
		sec.add(f.String())
		if f.fails {
			r.failing++
		}
	}
	if !quiet || news {
		r.write(sec)
	}
	return rep, nil
}

// firstReport reports whether f, a finding of an answer of endpoint, is
// one that read has not reported for that endpoint yet, and notes it.
func (r *liveRun) firstReport(endpoint string, f finding) bool {
	key := fmt.Sprint(endpoint, "\x00", f.check, "\x00", f.path, "\x00", f.what, "\x00", f.fails)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reported[key] {
		return false
	}
	if r.reported == nil {
		r.reported = map[string]bool{}
	}
	r.reported[key] = true
	return true
}

// contentType returns h's Content-Type as the report gives it.
func contentType(h http.Header) string {
	if ct := h.Get("Content-Type"); ct != "" {
		return ct
	}
	return "no Content-Type"
}

// correlationNames returns the names of h's correlation headers, without
// their values, as the report gives them.
func correlationNames(h http.Header) string {
	var names []string
	for name := range h {
		if isCorrelationHeader(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "no correlation header"
	}
	slices.Sort(names)
	return "correlation headers: " + strings.Join(names, ", ")
}

// records returns how many records rep, an answer of kind, one of
// answerKinds, holds, as the report says it, and adds them to the census of
// the keys seen.
func (r *liveRun) records(kind string, rep *reply) string {
	k := answerKinds[slices.IndexFunc(answerKinds, func(k answerKind) bool { return k.name == kind })]
	records, err := k.decode(rep)
	if err != nil {
		return "no records read"
	}
	dec := json.NewDecoder(bytes.NewReader(rep.body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		for _, rec := range recordsOf(v) {
			r.census.add(k.typ, rec)
		}
	}
	if len(records) == 1 {
		return "1 record"
	}
	return strconv.Itoa(len(records)) + " records"
}

// passed reports whether the run's deadline has passed, by its clock.
func (r *liveRun) passed() bool {
	return !r.deadline.IsZero() && !r.now().Before(r.deadline)
}

// wait waits d on the run's clock, for a probe, unless the run's deadline
// would come first, which stops the run: then it returns why the run
// stopped.
func (r *liveRun) wait(ctx context.Context, d time.Duration) error {
	if !r.deadline.IsZero() && r.now().Add(d).After(r.deadline) {
		r.stop(errRunDeadline)
		return context.Cause(ctx)
	}
	return r.sleep(ctx, d)
}

// stop stops the run for cause, unless it has stopped already, so that the
// guard sends nothing more.
func (r *liveRun) stop(cause error) {
	r.guard.stop(cause)
}

// stopIfRefused stops the run, saying what to do, when err, a request's
// failure, says that Schwab refused the saved login or the app's key or
// secret. A login that Schwab has ended shows only so: the token file holds
// it as it was, and only a refresh asks Schwab.
func (r *liveRun) stopIfRefused(err error) {
	switch {
	case errors.Is(err, ErrLoginExpired):
		r.stop(errLoginRefused)
	case errors.Is(err, ErrAppRejected):
		r.stop(errAppRefused)
	}
}

// fail records why the run fails, a reason without values.
func (r *liveRun) fail(why string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures = append(r.failures, why)
}

// write writes sec to the report. A report that cannot be written fails
// the run.
func (r *liveRun) write(sec *reportSection) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.report.write(sec); err != nil {
		r.failures = append(r.failures, "the report could not be written: "+err.Error())
	}
}

// warn writes a warning of the Auth's or the Client's, such as one that
// names the token file, to the report, scrubbed as every line is.
func (r *liveRun) warn(msg string) {
	sec := &reportSection{title: "warning"}
	sec.add(msg)
	r.write(sec)
}

// showLine prints line, a line of the report, if r prints any.
func (r *liveRun) showLine(line string) {
	if r.show != nil {
		r.show(line)
	}
}

// end writes the report's last sections, saves the state and finishes the
// report, and returns why the run failed, if it did.
func (r *liveRun) end(ctx context.Context) error {
	r.write(r.keysSeen())
	stopped := context.Cause(ctx)
	r.write(r.guardSection(stopped))
	if err := r.state.save(); err != nil {
		r.fail("the state could not be saved: " + err.Error())
	}
	if r.rec != nil {
		if err := r.rec.failure(); err != nil {
			r.fail("the recording stopped: " + err.Error())
		}
	}
	var why []string
	if stopped != nil {
		why = append(why, "it stopped before its end: "+stopped.Error())
	}
	switch {
	case r.failing == 1:
		why = append(why, "1 finding fails")
	case r.failing > 1:
		why = append(why, fmt.Sprintf("%d findings fail", r.failing))
	}
	r.mu.Lock()
	why = append(why, r.failures...)
	r.mu.Unlock()
	sec := &reportSection{title: "before sharing"}
	if r.report.w8 == 0 {
		sec.add("W-8: no line reveals W-8 withholding rows")
	} else {
		sec.addf("W-8: %s, marked %s, reveal W-8 withholding rows, which show nonresident tax status; decide on each before sharing the report",
			plural(r.report.w8, "line"), strings.TrimSpace(w8Mark))
	}
	if len(why) == 0 {
		sec.add("the run passed")
	} else {
		sec.add("the run failed: " + strings.Join(why, "; "))
	}
	r.write(sec)
	r.mu.Lock()
	if err := r.report.finish(); err != nil {
		why = append(why, "the report could not be written: "+err.Error())
	}
	why = append(why, r.report.problems...)
	r.mu.Unlock()
	if len(why) == 0 {
		return nil
	}
	return errors.New("the live run failed: " + strings.Join(slices.Compact(why), "; "))
}

// keysSeen returns the section of the keys seen, for each type of the
// records: the records seen, the keys new to liveKeys, the keys of liveKeys
// not seen, and the optional fields present on every record.
func (r *liveRun) keysSeen() *reportSection {
	sec := &reportSection{title: "the keys seen"}
	types := slices.Collect(maps.Keys(r.census.records))
	for typ := range liveKeys {
		if !slices.Contains(types, typ) {
			types = append(types, typ)
		}
	}
	slices.SortFunc(types, func(a, b reflect.Type) int { return strings.Compare(a.Name(), b.Name()) })
	for _, typ := range types {
		records := r.census.records[typ]
		if len(records) == 0 {
			sec.add(typ.Name() + ": no record seen")
			continue
		}
		seen := map[string]int{}
		for _, rec := range records {
			for k := range rec {
				seen[k]++
			}
		}
		var fresh, unseen, everywhere []string
		for _, k := range slices.Sorted(maps.Keys(seen)) {
			if !slices.Contains(liveKeys[typ], k) {
				fresh = append(fresh, k)
			}
		}
		for _, k := range liveKeys[typ] {
			if seen[k] == 0 {
				unseen = append(unseen, k)
			}
		}
		for _, f := range infoOf(typ).fields {
			if f.optional && typ.Field(f.index).Type != problemsType && seen[f.name] == len(records) {
				everywhere = append(everywhere, f.name)
			}
		}
		sec.addf("%s: %s", typ.Name(), plural(len(records), "record"))
		sec.add("  new to liveKeys: " + listOrNone(fresh))
		sec.add("  of liveKeys, not seen: " + listOrNone(unseen))
		sec.add("  optional, on every record: " + listOrNone(everywhere))
	}
	return sec
}

// guardSection returns the section of the guard's accounting: the budget
// and the requests sent, the answers by status, the token requests, the
// 429s, the failures of the network, the refusals, and why the run
// stopped, if it did, with what did not run.
func (r *liveRun) guardSection(stopped error) *reportSection {
	c := r.guard.count()
	sec := &reportSection{title: "the guard"}
	sec.addf("budget: %d requests, token requests included; %d sent", c.budget, c.sent)
	var statuses []string
	for _, s := range slices.Sorted(maps.Keys(c.statuses)) {
		statuses = append(statuses, fmt.Sprintf("%d: %d", s, c.statuses[s]))
	}
	sec.add("answers by status: " + listOrNone(statuses))
	sec.addf("token requests: %d", c.tokens)
	sec.addf("429s: %d", c.statuses[http.StatusTooManyRequests])
	sec.addf("failures of the network: %d", c.failures)
	sec.add("refusals: " + listOrNone(c.refusals))
	if stopped == nil {
		sec.add("stopped: no")
	} else {
		sec.add("stopped: " + stopped.Error())
	}
	sec.add("did not run: " + listOrNone(r.notRun))
	return sec
}

// plural returns n and noun, made plural for any n but 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// listOrNone returns items, joined by commas, or "none".
func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
