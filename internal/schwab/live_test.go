package schwab

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The -live mode's flags. A run with a flag is never served from the test
// cache, so a live run always asks Schwab.
var (
	live         = flag.Bool("live", false, "probe the real Schwab API with the saved login (never in CI)")
	liveOut      = flag.String("live.out", "", "the folder of the live report; default <user cache dir>/schwabtx/live/<UTC time>")
	liveStateDir = flag.String("live.state", "", "the folder of the live run's state and its key; default <user cache dir>/schwabtx/live")
	liveRecord   = flag.Bool("live.record", false, "also keep each answer of the live run raw, under the out folder's raw folder")
	liveBudget   = flag.Int("live.budget", 0, "the most requests the live run sends, token requests included; default the mode's")
	liveRate     = flag.Bool("live.rate", false, "run the rate limit probe alone (needs -timeout 0)")
	liveWatch    = flag.String("live.watch", "", "run a watch alone, of balances or of fills (needs -timeout 0)")
	liveSnapshot = flag.String("live.snapshot", "21:05", "the local time of day at which the scheduled snapshot runs, which the rate probe keeps clear of")
)

// TestLive probes the real Schwab API with the saved login, never in CI:
//
//	go test ./internal/schwab -run '^TestLive$' -live -v
//
// It checks every answer against the records, runs the probes of the open
// questions, and writes a report without identifiers or values to
// report.txt in its out folder. Three things keep it out of CI: CI's one
// command has no -live; it refuses to run where CI, GITHUB_ACTIONS or
// SCHWAB_BASE_URL is set, before it reads any other variable or file; and
// CI holds no login. go test ./... -live does reach this package, while
// every other package fails on the flag, so the refusal is what stops it
// there.
//
// It takes the app's key and secret from SCHWAB_APP_KEY and
// SCHWAB_APP_SECRET, and the login from SCHWAB_TOKEN_FILE or the default
// token file; .env is not read, so export it first. It never logs in: run
// schwabtx login first. Everything else is runLive's, which CI runs against
// the fake.
func TestLive(t *testing.T) {
	if !*live {
		t.Skip("-live probes the real Schwab API with the saved login")
	}
	if err := refuseCI(os.Getenv); err != nil {
		t.Fatal(err)
	}
	mode, snapshot, err := liveMode(*liveRate, *liveWatch, *liveSnapshot, *liveBudget)
	if err != nil {
		t.Fatal(err)
	}
	key, secret := strings.TrimSpace(os.Getenv("SCHWAB_APP_KEY")), strings.TrimSpace(os.Getenv("SCHWAB_APP_SECRET"))
	if key == "" || secret == "" {
		t.Fatal("SCHWAB_APP_KEY and SCHWAB_APP_SECRET must be set; .env is not read, so export it first")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal("no home folder: ", err)
	}
	// Until the run's own scrubber takes over, this one keeps the home
	// folder, the user name and the login's files out of what the front
	// door prints, each line self-checked as the run's are.
	pre := newLiveScrubber()
	pre.setHome(home)
	clean := func(text string) string {
		if text = pre.scrub(text); pre.holds(text) {
			return "(not shown: it held something that the run saw)"
		}
		return text
	}
	fatal := func(what string, err error) {
		t.Helper()
		t.Fatal(what + ": " + clean(err.Error()))
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		fatal("no user cache folder", err)
	}
	pre.addPath("{cache folder}", cache)
	// LookupEnv, not Getenv: gosec's taint analysis of this package takes
	// Getenv for a source and the Auth's own reading of the path for a
	// sink, though the token file is the user's to choose, as in schwabtx.
	tokenFile, _ := os.LookupEnv("SCHWAB_TOKEN_FILE")
	if tokenFile = strings.TrimSpace(tokenFile); tokenFile == "" {
		if tokenFile, err = DefaultTokenFile(); err != nil {
			fatal("no token file", err)
		}
	}
	pre.addPath("{token file}", tokenFile)
	a, err := NewAuth(Config{
		AppKey: key, AppSecret: secret, TokenFile: tokenFile,
		Warn: func(msg string) { t.Log("warning: " + clean(msg)) },
	})
	if err != nil {
		fatal("the login", err)
	}
	st, err := a.Status()
	if err != nil {
		fatal("the login", err)
	}
	pre.addPath("{token file}", st.TokenFile)
	warning, err := loginReady(st, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if warning != "" {
		t.Log(warning)
	}
	module, err := moduleRoot(".")
	if err != nil {
		fatal("the module", err)
	}
	now := time.Now()
	out := *liveOut
	if out == "" {
		out = defaultLiveOut(cache, now)
	}
	stateDir := *liveStateDir
	if stateDir == "" {
		stateDir = filepath.Join(cache, "schwabtx", "live")
	}
	c := NewClient(a, ClientConfig{})
	r := &liveRun{
		now: time.Now, sleep: sleep, auth: a, client: c, origin: DefaultBaseURL,
		out: out, stateFile: filepath.Join(stateDir, "state.json"), keyFile: filepath.Join(stateDir, "state.key"),
		tokenFile: st.TokenFile, rateFile: c.limit.path, home: home, cacheDir: cache, module: module,
		snapshot: snapshot, loc: time.Local, pageCap: MaxTransactions, mode: mode, budget: *liveBudget,
		deadline: liveDeadline(t.Deadline()), record: *liveRecord, commit: gitCommit(t.Context(), module),
		probes: liveProbes, show: func(line string) { t.Log(line) },
	}
	err = runLive(t.Context(), r)
	if _, statErr := os.Stat(filepath.Join(out, "report.txt")); statErr == nil {
		// The owner needs to find the report, so its folder is named, with
		// the home folder as ~ and the user name hidden: the folders a run
		// writes name no account.
		where := newLiveScrubber()
		where.setHome(home)
		t.Log("the report: " + where.scrub(filepath.Join(out, "report.txt")))
	}
	if err != nil {
		t.Error(err) // scrubbed by runLive
	}
}

// refuseCI returns why a live run must not go on where getenv reads the
// environment: CI or GITHUB_ACTIONS set means a CI job, which must never
// reach Schwab, and SCHWAB_BASE_URL set means a server other than Schwab's,
// which a live report would be taken to stand for. TestLive calls it before
// it reads any other variable or file, and fails, not skips, on it.
func refuseCI(getenv func(string) string) error {
	for _, name := range []string{"CI", "GITHUB_ACTIONS"} {
		if getenv(name) != "" {
			return fmt.Errorf("-live never runs in CI, and %s is set", name)
		}
	}
	if getenv("SCHWAB_BASE_URL") != "" {
		return errors.New("-live probes Schwab itself, so it never runs with SCHWAB_BASE_URL set")
	}
	return nil
}

// liveMode returns the mode that the flags ask for, and the snapshot's time
// of day, as a time after midnight: the default mode, or the rate probe or
// a watch alone, never two.
func liveMode(rate bool, watch, snapshot string, budget int) (mode string, at time.Duration, err error) {
	if budget < 0 {
		return "", 0, errors.New("-live.budget takes a number of requests, or 0 for the mode's")
	}
	switch {
	case rate && watch != "":
		return "", 0, errors.New("-live.rate and -live.watch each run alone; give one of them")
	case rate:
		mode = modeRate
	case watch == "balances":
		mode = modeBalances
	case watch == "fills":
		mode = modeFills
	case watch == "":
		mode = modeDefault
	default:
		return "", 0, errors.New("-live.watch takes balances or fills")
	}
	clock, err := time.Parse("15:04", snapshot)
	if err != nil || len(snapshot) != len("15:04") {
		return "", 0, errors.New("-live.snapshot takes a time of day, such as 21:05")
	}
	return mode, time.Duration(clock.Hour())*time.Hour + time.Duration(clock.Minute())*time.Minute, nil
}

// loginReady returns an error unless st, the saved login, is one that
// Schwab has not refused and that has not lapsed by now, and a warning when
// it lapses within a day, which a long run could outlast.
func loginReady(st Status, now time.Time) (warning string, err error) {
	switch {
	case !st.LoggedIn:
		return "", errors.New("no login is saved; run `schwabtx login` first")
	case st.Rejected != nil:
		return "", errors.New("Schwab refused the saved login; run `schwabtx login` first")
	case !st.ExpiresAt.IsZero() && !now.Before(st.ExpiresAt):
		return "", errors.New("the saved login has lapsed; run `schwabtx login` first")
	case !st.ExpiresAt.IsZero() && st.ExpiresAt.Sub(now) < 24*time.Hour:
		return fmt.Sprintf("warning: the login lapses in %v; a long run may outlast it", st.ExpiresAt.Sub(now).Round(time.Minute)), nil
	}
	return "", nil
}

// liveDeadline returns the run's deadline: a minute before the test's,
// deadline when ok, so that the run stops itself, and writes its report,
// before the test's time limit panics, which nothing can recover from; or
// no deadline when the test has none, as with -timeout 0.
func liveDeadline(deadline time.Time, ok bool) time.Time {
	if !ok {
		return time.Time{}
	}
	return deadline.Add(-time.Minute)
}

// defaultLiveOut returns the out folder of a run at now: one of its own,
// under cache, the user cache folder.
func defaultLiveOut(cache string, now time.Time) string {
	return filepath.Join(cache, "schwabtx", "live", now.UTC().Format("2006-01-02T150405Z"))
}

// A live run refuses to go on where CI, GITHUB_ACTIONS or SCHWAB_BASE_URL is
// set, and so in CI itself, where both of the first are.
func TestRefuseCI(t *testing.T) {
	env := func(vars ...string) func(string) string {
		return func(name string) string {
			for i := 0; i+1 < len(vars); i += 2 {
				if vars[i] == name {
					return vars[i+1]
				}
			}
			return ""
		}
	}
	for _, tc := range []struct {
		vars []string
		want string
	}{
		{nil, ""},
		{[]string{"HOME", "/home/x", "SCHWAB_APP_KEY", "k"}, ""},
		{[]string{"CI", "true"}, "-live never runs in CI, and CI is set"},
		{[]string{"CI", "1", "GITHUB_ACTIONS", "true"}, "-live never runs in CI, and CI is set"},
		{[]string{"GITHUB_ACTIONS", "true"}, "-live never runs in CI, and GITHUB_ACTIONS is set"},
		{[]string{"SCHWAB_BASE_URL", "http://127.0.0.1:8080"}, "-live probes Schwab itself, so it never runs with SCHWAB_BASE_URL set"},
	} {
		err := refuseCI(env(tc.vars...))
		if got := fmt.Sprint(err); tc.want == "" && err != nil || tc.want != "" && got != tc.want {
			t.Errorf("refuseCI(%q) = %v, want %q", tc.vars, err, tc.want)
		}
	}
	if os.Getenv("CI") != "" || os.Getenv("GITHUB_ACTIONS") != "" {
		if refuseCI(os.Getenv) == nil {
			t.Error("refuseCI takes this CI job")
		}
	}
}

// The flags ask for the default mode, or the rate probe or a watch alone,
// never two at once; the snapshot's time is a time of day, and the budget
// a number of requests.
func TestLiveMode(t *testing.T) {
	for _, tc := range []struct {
		rate     bool
		watch    string
		snapshot string
		budget   int
		mode     string
		at       time.Duration
	}{
		{false, "", "21:05", 0, modeDefault, 21*time.Hour + 5*time.Minute},
		{true, "", "00:00", 300, modeRate, 0},
		{false, "balances", "23:59", 0, modeBalances, 23*time.Hour + 59*time.Minute},
		{false, "fills", "09:30", 0, modeFills, 9*time.Hour + 30*time.Minute},
		{true, "fills", "21:05", 0, "", 0},
		{false, "orders", "21:05", 0, "", 0},
		{false, "", "9:05", 0, "", 0},
		{false, "", "24:00", 0, "", 0},
		{false, "", "21:60", 0, "", 0},
		{false, "", "", 0, "", 0},
		{false, "", "21:05", -1, "", 0},
	} {
		mode, at, err := liveMode(tc.rate, tc.watch, tc.snapshot, tc.budget)
		if mode != tc.mode || at != tc.at || (err == nil) != (tc.mode != "") {
			t.Errorf("liveMode(%v, %q, %q, %d) = %q, %v, %v; want %q, %v", tc.rate, tc.watch, tc.snapshot, tc.budget, mode, at, err, tc.mode, tc.at)
		}
	}
}

// A live run needs a saved login that Schwab has not refused and that has
// not lapsed, and warns of one that lapses within a day.
func TestLoginReady(t *testing.T) {
	now := testNow
	for _, tc := range []struct {
		name    string
		st      Status
		warning string
		err     string
	}{
		{"none", Status{}, "", "no login is saved; run `schwabtx login` first"},
		{"refused", Status{LoggedIn: true, Rejected: ErrLoginExpired}, "", "Schwab refused the saved login; run `schwabtx login` first"},
		{"lapsed", Status{LoggedIn: true, ExpiresAt: now}, "", "the saved login has lapsed; run `schwabtx login` first"},
		{"lapsing", Status{LoggedIn: true, ExpiresAt: now.Add(5 * time.Hour)}, "warning: the login lapses in 5h0m0s; a long run may outlast it", ""},
		{"fresh", Status{LoggedIn: true, ExpiresAt: now.Add(6 * 24 * time.Hour)}, "", ""},
		{"of no known age", Status{LoggedIn: true}, "", ""},
	} {
		warning, err := loginReady(tc.st, now)
		if warning != tc.warning || fmt.Sprint(err) != tc.err && (err != nil || tc.err != "") {
			t.Errorf("%s: loginReady = %q, %v; want %q, %q", tc.name, warning, err, tc.warning, tc.err)
		}
	}
}

// The run's deadline is a minute before the test's, or none without one,
// and its out folder is named by its time in UTC.
func TestLiveDeadline(t *testing.T) {
	if got := liveDeadline(testNow, true); !got.Equal(testNow.Add(-time.Minute)) {
		t.Errorf("liveDeadline(%v) = %v", testNow, got)
	}
	if got := liveDeadline(time.Time{}, false); !got.IsZero() {
		t.Errorf("liveDeadline without a deadline = %v", got)
	}
	if got, want := defaultLiveOut("cache", testNow.In(time.FixedZone("UTC+5", 5*60*60))), filepath.Join("cache", "schwabtx", "live", "2026-10-08T120000Z"); got != want {
		t.Errorf("defaultLiveOut = %s, want %s", got, want)
	}
}

// Each mode has its budget, token requests included, and its plan's time,
// against which the run's deadline and the login are held: the default run
// 60 and 40 an account, the rate probe 320, the watch of balances 170 from
// the next 15:45 to 10:00 in New York, and the watch of fills 10 and 60 an
// account polled, for half an hour.
func TestLivePlan(t *testing.T) {
	ny := newYork()
	for _, tc := range []struct {
		mode     string
		accounts int
		now      time.Time
		budget   int
		takes    time.Duration
	}{
		{modeDefault, 0, testNow, 60, 2 * time.Minute},
		{modeDefault, 3, testNow, 180, 3*time.Minute + 30*time.Second},
		{modeRate, 3, testNow, 320, 10 * time.Minute},
		{modeFills, 3, testNow, 190, 31 * time.Minute},
		{modeBalances, 3, time.Date(2026, 11, 2, 9, 0, 0, 0, ny), 170, 25 * time.Hour},
		{modeBalances, 3, time.Date(2026, 11, 2, 15, 50, 0, 0, ny), 170, 18*time.Hour + 10*time.Minute},
		{modeBalances, 3, time.Date(2026, 11, 2, 16, 0, 0, 0, ny), 170, 42 * time.Hour},
		{modeBalances, 3, time.Date(2026, 11, 2, 2, 0, 0, 0, ny), 170, 32 * time.Hour},
	} {
		budget, takes := livePlan(tc.mode, tc.accounts, tc.now)
		if budget != tc.budget || takes != tc.takes {
			t.Errorf("livePlan(%s, %d, %v) = %d, %v; want %d, %v", tc.mode, tc.accounts, tc.now, budget, takes, tc.budget, tc.takes)
		}
	}
}
