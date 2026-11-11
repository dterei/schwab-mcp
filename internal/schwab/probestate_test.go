package schwab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveBenchmarks are the public symbols whose daily closes Q8 keeps in the
// state, to see whether Schwab adjusts past closes for a dividend. They are
// the probe's own, not the owner's, so the report may name them.
var liveBenchmarks = []string{"SPY", "VTI"}

// keptCloses is how many of a symbol's last daily closes the state keeps of
// each run, and keptRuns of how many runs: three weeks of daily runs, or
// five months of weekly ones.
const (
	keptCloses = 90
	keptRuns   = 20
)

// An adjustState is Q8's state: each benchmark's daily closes, by New York
// date, as each of the last keptRuns runs that read them read them, in the
// order of the runs, so that a run between an ex-date's last close and its
// end, which reads closes adjusted already or not yet, does not take the
// place of the run before that close that a later run compares with.
type adjustState struct {
	Runs map[string][]symbolCloses `json:"runs"`
}

// A symbolCloses is one symbol's daily closes as one run read them: by New
// York date, the date of the last, and when the run read them.
type symbolCloses struct {
	AsOf   string             `json:"asOf"`
	Closes map[string]float64 `json:"closes"`
	Read   time.Time          `json:"read"`
}

// A dividendQuote is what a quote says of a symbol's last dividend: its
// ex-date, a New York date, and its amount.
type dividendQuote struct {
	exDate string
	amount float64
}

// notAuthorized reports whether err is Schwab's 401 "Client not
// authorized", its answer to an app without the product that a request
// needs, such as Market Data.
func notAuthorized(err error) bool {
	se, ok := errors.AsType[*Error](err)
	return ok && se.Status == http.StatusUnauthorized &&
		(strings.Contains(se.Message, "Client not authorized") || slices.ContainsFunc(se.Errors, func(s string) bool {
			return strings.Contains(s, "Client not authorized")
		}))
}

// readQuotes reads body, an answer of Market Data's quotes, for each
// symbol's last dividend.
func readQuotes(body []byte) (map[string]dividendQuote, error) {
	var m map[string]struct {
		Fundamental struct {
			DivExDate    string  `json:"divExDate"`
			DivPayAmount float64 `json:"divPayAmount"`
		} `json:"fundamental"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	qs := map[string]dividendQuote{}
	for sym, q := range m {
		d := q.Fundamental.DivExDate
		if _, err := time.Parse(time.DateOnly, d[:min(len(d), len(time.DateOnly))]); err != nil {
			d = ""
		}
		qs[sym] = dividendQuote{exDate: d[:min(len(d), len(time.DateOnly))], amount: q.Fundamental.DivPayAmount}
	}
	return qs, nil
}

// readCloses reads body, an answer of Market Data's daily price history,
// for its closes by New York date, keeping the last keptCloses.
func readCloses(body []byte) (symbolCloses, error) {
	var ph struct {
		Candles []struct {
			Close    float64 `json:"close"`
			Datetime int64   `json:"datetime"`
		} `json:"candles"`
	}
	if err := json.Unmarshal(body, &ph); err != nil {
		return symbolCloses{}, err
	}
	sc := symbolCloses{Closes: map[string]float64{}}
	for _, c := range ph.Candles {
		sc.Closes[string(DateInNewYork(time.UnixMilli(c.Datetime)))] = c.Close
	}
	dates := slices.Sorted(maps.Keys(sc.Closes))
	for _, d := range dates[:max(0, len(dates)-keptCloses)] {
		delete(sc.Closes, d)
	}
	if len(dates) > 0 {
		sc.AsOf = dates[len(dates)-1]
	}
	return sc, nil
}

// The answers of Q8 for one symbol.
const (
	adjustNone     = "split-adjusted only"
	adjustDividend = "dividend-adjusted"
)

// sessionClose returns when the regular session of day, a New York date,
// closes: 16:00 in New York.
func sessionClose(day string) time.Time {
	d, err := time.ParseInLocation(time.DateOnly, day, newYork())
	if err != nil {
		panic(err) // the dates of closes come from DateInNewYork
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 16, 0, 0, 0, newYork())
}

// compareCloses decides, for one symbol, whether Schwab adjusted its
// closes for the dividend that q gives, between the closes of runs, the
// earlier runs', and cur, this run's. The adjustment may come at any time
// from the close of the last session before the ex-date, whose price sets
// its factor, to the end of the ex-date. So the earlier run must have read
// its closes before that close, and this run after the ex-date, each by
// when it read them, as a run in between could read closes adjusted
// already, or not yet: of the earlier runs, the latest that read its
// closes before that close counts. The last session before the ex-date is
// the last day before it among this run's closes. Only the closes before
// the ex-date that both runs hold count, and of the earlier run's only
// those of days that had ended when it read them: a close read during its
// day is a price of that day. Unchanged means split-adjusted only, and all
// scaled by 1 - D/P, D the dividend and P the close before the ex-date,
// dividend-adjusted. It returns the answer, or "" with why there is none.
func compareCloses(runs []symbolCloses, cur symbolCloses, q dividendQuote) (answer, why string) {
	runs = slices.DeleteFunc(slices.Clone(runs), func(sc symbolCloses) bool { return len(sc.Closes) == 0 || sc.Read.IsZero() })
	switch {
	case len(runs) == 0:
		return "", "no earlier run kept its closes"
	case q.exDate == "":
		return "", "the quote gave no ex-date"
	case nyDate(cur.Read) <= q.exDate:
		return "", "its ex-date, " + q.exDate + ", is not over yet"
	case cur.AsOf < q.exDate:
		return "", "this run's closes end before its ex-date, " + q.exDate
	}
	lastSession := ""
	for d := range cur.Closes {
		if d < q.exDate && d > lastSession {
			lastSession = d
		}
	}
	var prev symbolCloses
	for _, sc := range runs {
		if (lastSession == "" || sc.Read.Before(sessionClose(lastSession))) && sc.Read.After(prev.Read) {
			prev = sc
		}
	}
	if prev.Read.IsZero() {
		return "", "no earlier run came before the last close before its ex-date, " + q.exDate
	}
	var dates []string
	for d := range prev.Closes {
		if _, ok := cur.Closes[d]; ok && d < q.exDate && d < nyDate(prev.Read) {
			dates = append(dates, d)
		}
	}
	if len(dates) == 0 {
		return "", "the two runs have no close before the ex-date in common"
	}
	slices.Sort(dates)
	unchanged := true
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, d := range dates {
		unchanged = unchanged && math.Abs(cur.Closes[d]-prev.Closes[d]) < 0.005
		ratio := cur.Closes[d] / prev.Closes[d]
		lo, hi = min(lo, ratio), max(hi, ratio)
	}
	if unchanged {
		return adjustNone, ""
	}
	before := prev.Closes[dates[len(dates)-1]]
	if q.amount > 0 && before > 0 {
		f := 1 - q.amount/before
		if hi-lo < 1e-4 && math.Abs((lo+hi)/2-f) < 5e-4 {
			return adjustDividend, ""
		}
	}
	return "", "the closes before the ex-date changed, though not by the dividend's factor"
}

// probeAdjustment is Q8, whether daily price bars are adjusted for
// dividends or only for splits. Each run reads the benchmarks' quotes, for
// their last ex-date, and a year of their daily closes, and keeps the
// closes, public data, in the state with when it read them, beside those
// of the runs before it, up to keptRuns. After an ex-date between two
// runs, an earlier run's before the close of the session before it and
// this run's after it, closes before it unchanged mean split-adjusted
// only, and all scaled by the dividend's factor dividend-adjusted. A 401
// "Client not authorized" means the app has no Market Data product, which
// it reports as such.
func probeAdjustment(ctx context.Context, r *liveRun) verdict {
	q := url.Values{"symbols": {strings.Join(liveBenchmarks, ",")}, "fields": {"quote,fundamental"}}
	rep, err := r.get(ctx, "marketdata/quotes", "/marketdata/v1/quotes", q, "")
	if err != nil {
		if notAuthorized(err) {
			return verdict{answer: "the app has no Market Data product: Schwab answered 401 Client not authorized"}
		}
		return inconclusive("the quotes failed: " + schwabSays(err))
	}
	quotes, err := readQuotes(rep.body)
	if err != nil {
		return inconclusive("the quotes' answer could not be read")
	}
	var prev adjustState
	if _, err := r.state.get("Q8", &prev); err != nil {
		return inconclusive("the state's closes could not be read")
	}
	next := adjustState{Runs: map[string][]symbolCloses{}}
	var v verdict
	answers := map[string]bool{}
	for _, sym := range liveBenchmarks {
		next.Runs[sym] = prev.Runs[sym]
		ph := url.Values{"symbol": {sym}, "periodType": {"year"}, "period": {"1"}, "frequencyType": {"daily"}, "frequency": {"1"}}
		rep, err := r.get(ctx, "marketdata/pricehistory", "/marketdata/v1/pricehistory", ph, "")
		if err != nil {
			v.evidence = append(v.evidence, reportLine{text: sym + ": the price history failed: " + schwabSays(err)})
			continue
		}
		cur, err := readCloses(rep.body)
		if err != nil || len(cur.Closes) == 0 {
			v.evidence = append(v.evidence, reportLine{text: sym + ": the price history held no closes"})
			continue
		}
		cur.Read = r.now()
		runs := append(slices.Clone(prev.Runs[sym]), cur)
		next.Runs[sym] = runs[max(0, len(runs)-keptRuns):]
		answer, why := compareCloses(prev.Runs[sym], cur, quotes[sym])
		if answer == "" {
			v.evidence = append(v.evidence, reportLine{text: sym + ": " + why})
			continue
		}
		answers[answer] = true
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("%s: across the ex-date %s, its closes are %s", sym, quotes[sym].exDate, answer)})
	}
	if err := r.state.put("Q8", next); err != nil {
		r.fail("Q8's state could not be kept")
	}
	switch len(answers) {
	case 0:
		v.answer = "inconclusive: it needs a run before the last close before an ex-date, and one after the ex-date; this run's closes are kept for the next"
	case 1:
		v.answer = "daily closes are " + slices.Collect(maps.Keys(answers))[0]
	default:
		v.answer = "inconclusive: the symbols disagree"
	}
	return v
}

// A hashState is Q10's state: for each account, by its number's digest,
// the digests of the hashes it had, each with the login and the app it was
// seen under.
type hashState struct {
	Accounts map[string][]hashSeen `json:"accounts"`
}

// A hashSeen is an account's hash as one run saw it: its digest, the time
// of the login, and the app key's digest.
type hashSeen struct {
	Hash  string `json:"hash"`
	Login string `json:"login"`
	App   string `json:"app"`
}

// keptHashes is how many hashes the state keeps for each account.
const keptHashes = 20

// What Q10 found of an account's hash against earlier runs under other
// logins, or other apps.
const (
	hashSame    = "the same"
	hashChanged = "changed"
	hashNew     = "not seen yet"
)

// compareHash returns what seen, the earlier runs, say of hash under other
// logins of app, and under other apps, whose runs are under other logins
// too.
func compareHash(seen []hashSeen, hash, login, app string) (logins, apps string) {
	logins, apps = hashNew, hashNew
	for _, s := range seen {
		var word *string
		switch {
		case s.App == app && s.Login != login:
			word = &logins
		case s.App != app:
			word = &apps
		default:
			continue
		}
		switch {
		case s.Hash != hash:
			*word = hashChanged
		case *word == hashNew:
			*word = hashSame
		}
	}
	return logins, apps
}

// probeHashes is Q10, whether account hashes stay the same across logins
// and across apps. Each run keeps, under each account number's digest, its
// hash's digest with the login's time and the app key's digest, and
// compares them with the earlier runs' under other logins of the app, and
// under other apps: the same, changed, or not seen yet. It sends nothing:
// the account numbers are read already.
func probeHashes(_ context.Context, r *liveRun) verdict {
	var st hashState
	if _, err := r.state.get("Q10", &st); err != nil {
		return inconclusive("the state's hashes could not be read")
	}
	if st.Accounts == nil {
		st.Accounts = map[string][]hashSeen{}
	}
	status, _ := r.auth.Status()
	login := "unknown"
	if !status.LoginAt.IsZero() {
		login = status.LoginAt.UTC().Format(time.RFC3339Nano)
	}
	app := r.state.digest("app key", r.auth.cfg.AppKey)
	var v verdict
	all := map[string][]string{}
	for _, a := range r.accounts {
		if a.hash == "" {
			continue
		}
		key, hash := r.state.digest("account number", a.number), r.state.digest("hash", a.hash)
		logins, apps := compareHash(st.Accounts[key], hash, login, app)
		all["logins"] = append(all["logins"], logins)
		all["apps"] = append(all["apps"], apps)
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("%s: under other logins, %s; under other apps, %s", a.label(), logins, apps)})
		mine := hashSeen{Hash: hash, Login: login, App: app}
		if !slices.Contains(st.Accounts[key], mine) {
			st.Accounts[key] = append(st.Accounts[key], mine)
			st.Accounts[key] = st.Accounts[key][max(0, len(st.Accounts[key])-keptHashes):]
		}
	}
	if err := r.state.put("Q10", st); err != nil {
		r.fail("Q10's state could not be kept")
	}
	sum := func(words []string, across string) string {
		switch {
		case slices.Contains(words, hashChanged):
			return "hashes changed across " + across
		case slices.Contains(words, hashSame):
			return "hashes stayed the same across " + across
		}
		return ""
	}
	if len(all["logins"]) == 0 {
		return inconclusive("no account has a hash", v.evidence...)
	}
	logins, apps := sum(all["logins"], "logins"), sum(all["apps"], "apps")
	if slices.Contains(all["logins"], hashChanged) && slices.Contains(all["apps"], hashChanged) {
		// Every run of another app is under another login too.
		apps = "they differ across apps too, which the change across logins could account for"
	}
	var found []string
	for _, s := range []string{logins, apps} {
		if s != "" {
			found = append(found, s)
		}
	}
	if len(found) == 0 {
		v.answer = "inconclusive: no earlier run under another login or another app to compare with; this run's hashes are kept for the next"
		return v
	}
	v.answer = strings.Join(found, "; ")
	return v
}

// changeWindow is the span of Q12's window, and how long the state keeps a
// row.
const changeWindow = 60 * oneDay

// postingLatency is how long after its time a row may take to post in the
// ordinary course, as a fill's row is expected to within minutes, which
// Q13 measures. A row that the last run's answer left out, from no longer
// than this before that run, may have posted after it in the ordinary
// course, so Q12 does not call it late; the report says "the hour before
// the last run".
const postingLatency = time.Hour

// A rowsState is Q12's state: for each account, by its number's digest,
// the last run's window and the rows of the last 60 days.
type rowsState struct {
	Accounts map[string]accountRows `json:"accounts"`
}

// An accountRows is one account's window as the last run asked for it,
// whether its answer was complete, under pageCap rows, and its rows, by the
// digest of each one's activityId.
type accountRows struct {
	Start    time.Time          `json:"start"`
	End      time.Time          `json:"end"`
	Complete bool               `json:"complete"`
	Rows     map[string]rowSeen `json:"rows"`
}

// A rowSeen is one row as the state keeps it: when it was first seen, its
// time and its tradeDate, and a digest of each key path's values, with each
// instrument cut to its assetType and instrumentId, as milestone 2 will
// compare rows.
type rowSeen struct {
	FirstSeen time.Time         `json:"firstSeen"`
	Time      time.Time         `json:"time"`
	TradeDate time.Time         `json:"tradeDate,omitzero"`
	Keys      map[string]string `json:"keys"`
}

// takesRow reports whether w takes in a row at t, dated tradeDate, whichever
// of the two a window goes by, which is not known: both lie in w, or t alone
// for a row without a tradeDate. Most rows are dated the midnight in New
// York before their time, and an evening fill may be dated the next
// midnight, so a window's edge can lie between the two.
func (w window) takesRow(t, tradeDate time.Time) bool {
	return w.inside(t) && (tradeDate.IsZero() || w.inside(tradeDate))
}

// keyValues returns the values of raw, a row, by key path: each scalar's
// JSON, in the order the row holds them, an array's elements under the
// array's path, and each instrument cut to its assetType and instrumentId.
func keyValues(raw json.RawMessage) map[string][]string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	out := map[string][]string{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch v := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(v)) {
				p := joinPath(path, keyName(k))
				if strings.EqualFold(k, "instrument") {
					if in, ok := v[k].(map[string]any); ok {
						cut := map[string]any{}
						for ik, iv := range in {
							if strings.EqualFold(ik, "assetType") || strings.EqualFold(ik, "instrumentId") {
								cut[ik] = iv
							}
						}
						walk(cut, p)
						continue
					}
				}
				walk(v[k], p)
			}
		case []any:
			if len(v) == 0 {
				out[path] = append(out[path], "[]")
			}
			for _, e := range v {
				walk(e, path)
			}
		default:
			b, _ := json.Marshal(v)
			out[path] = append(out[path], string(b))
		}
	}
	walk(v, "")
	return out
}

// rowDigests returns the state's digests of raw's key paths.
func (r *liveRun) rowDigests(raw json.RawMessage) map[string]string {
	ds := map[string]string{}
	for p, vs := range keyValues(raw) {
		ds[p] = r.state.digest("Q12 "+p, strings.Join(vs, "\x00"))
	}
	return ds
}

// What Q12 found of one account's rows against the last run's.
type changesSeen struct {
	compared         bool           // whether the last run asked for this account
	same, fresh      int            // rows as they were, and rows new to the state
	changed          map[string]int // the key paths that changed, with how many rows each
	changedRows      int
	ages             []time.Duration // how old each changed row was
	gone, late       int
	near             int             // rows new to the state from within postingLatency before the last run
	lateLeast        []time.Duration // for each late row, how long after its time the last run read without it
	lateMost         []time.Duration // and how long after it this run's answer came with it: it posted in between
	complete, failed bool
	lastCut          bool // whether the last run's answer may have been cut at the cap
	// missing is the rows of the state missing from this answer, which it
	// can neither compare nor show gone, since the answer may have been cut
	// at the cap, or a window's edge lies between the row's time and its
	// tradeDate; unsure the rows new to the state from before the last run,
	// but for those from the hour before it, that it cannot show late, since
	// that run's answer may have been cut, or its window's edge lies between
	// them.
	missing, unsure int
}

// probeChanges is Q12, whether rows change after they post, and how long
// after. For each account it asks for the last 60 days, every type and no
// symbol, and compares the rows with the state, pruned first to the rows
// whose time or tradeDate lies in the last 60 days: a row whose key paths'
// digests differ changed, and the keys are named; a row is gone only when
// both its time and its tradeDate lie inside both runs' windows and this
// run's answer is complete, under pageCap rows in all and of each type; and
// a row is late when it is new to the state though both lie inside the last
// run's window, that run's answer was complete, and its time was more than
// postingLatency before that run. A late row posted after the last run's
// window ended and before this run's answer came with it, which bound how
// long after its time it came. Both times count, since whether a window
// takes a row by its time or by its tradeDate is not known. What the
// answers cannot show, the verdict says is undecided, rather than that no
// row did it: whether rows changed or went, of a row of the state missing
// from the answer, which it can neither compare nor show gone, and of rows
// that the last run's answer left out if it may have been cut at the cap,
// which never came into the state; whether rows came late, of a row new to
// the state from before the last run, but not shown late nor from the hour
// before that run, and of rows that this run's answer left out if it may
// have been cut; and anything of the rows of an account whose window
// failed, beside which it speaks of the other accounts' alone. With no row
// compared, none of the state's seen again or shown gone and none of this
// run's shown late, as for accounts without activity, it says nothing, nor
// when the answers leave all three undecided.
func probeChanges(ctx context.Context, r *liveRun) verdict {
	var st rowsState
	if _, err := r.state.get("Q12", &st); err != nil {
		return inconclusive("the state's rows could not be read")
	}
	if st.Accounts == nil {
		st.Accounts = map[string]accountRows{}
	}
	now := r.now()
	var v verdict
	var read, compared bool
	var changed, gone, late bool
	seen, missing, unsure := 0, 0, 0 // the rows compared with the last run's, and those left undecided
	cut, lastCut := false, false     // whether an answer compared, this run's or the last's, may have been cut at the cap
	var unread []string              // the accounts with rows in the state whose window failed
	for _, a := range r.accounts {
		if a.hash == "" {
			continue
		}
		key := r.state.digest("account number", a.number)
		cs, next := r.changesOf(ctx, a, st.Accounts[key], now)
		if cs.failed {
			v.evidence = append(v.evidence, reportLine{text: a.label() + ": the window of the last 60 days failed"})
			if cs.compared {
				// Its rows went unread, which the state keeps for the next
				// run to compare with.
				compared = true
				unread = append(unread, a.label()+"'s")
			}
			continue
		}
		st.Accounts[key] = next
		read = true
		if !cs.compared {
			v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("%s: %s, kept for the next run; no earlier run to compare with", a.label(), plural(cs.fresh, "row"))})
			continue
		}
		compared = true
		seen += cs.same + cs.changedRows + cs.gone + cs.late
		missing, unsure = missing+cs.missing, unsure+cs.unsure
		cut, lastCut = cut || !cs.complete, lastCut || cs.lastCut
		changed, gone, late = changed || cs.changedRows > 0, gone || cs.gone > 0, late || cs.late > 0
		v.evidence = append(v.evidence, reportLine{text: cs.line(a)})
	}
	if err := r.state.put("Q12", st); err != nil {
		r.fail("Q12's state could not be kept")
	}
	switch {
	case !read:
		return inconclusive("no account's window was read", v.evidence...)
	case !compared:
		v.answer = "inconclusive: no earlier run to compare with; this run's rows are kept for the next"
		return v
	case seen == 0:
		v.answer = "inconclusive: no row to compare: none of the last run's was seen again or shown gone, and none of this run's was shown late"
		return v
	}
	// What the answers leave undecided, of what no row was found to do:
	// whether rows of the state missing from them, which they can neither
	// compare nor show gone, and rows that the last run's cut answer left
	// out, which never came into the state, changed or went; and whether
	// rows new to the state that they cannot show late, and rows that a cut
	// answer left out, came late.
	did := map[string]bool{"changed": changed, "went": gone, "came late": late}
	undecided := map[string]bool{}
	var open []string
	for _, u := range []struct {
		on    bool
		rows  string
		verbs []string
	}{
		{missing > 0, plural(missing, "row"), []string{"changed", "went"}},
		{lastCut, "rows that the last run's cut answer left out", []string{"changed", "went"}},
		{unsure > 0, plural(unsure, "row"), []string{"came late"}},
		{cut, "rows that a cut answer left out", []string{"came late"}},
	} {
		var verbs []string
		for _, verb := range u.verbs {
			if u.on && !did[verb] {
				verbs = append(verbs, verb)
				undecided[verb] = true
			}
		}
		if len(verbs) > 0 {
			open = append(open, "whether "+u.rows+" "+strings.Join(verbs, " or "))
		}
	}
	// found is what rows did, none what no row of the accounts read did, and
	// open what rows may have done, which the answers leave undecided, or
	// which the rows of an account whose window failed may have done.
	var found, none []string
	for _, k := range []struct{ did, verb string }{{"changed after they posted", "changed"}, {"went", "went"}, {"came late", "came late"}} {
		switch {
		case did[k.verb]:
			found = append(found, k.did)
		case !undecided[k.verb]:
			none = append(none, k.verb)
		}
	}
	noRow := "no row "
	if n := len(unread); n > 0 {
		noRow = "no row of the accounts read "
		accounts, windows := unread[0], "window"
		if n > 1 {
			accounts, windows = strings.Join(unread[:n-1], ", ")+" and "+unread[n-1], "windows"
		}
		open = append(open, accounts+" rows, whose "+windows+" failed")
	}
	switch {
	case len(found) > 0:
		v.answer = "rows " + strings.Join(found, ", and ")
	case len(none) == 0:
		v.answer = "inconclusive: undecided since the last run: " + strings.Join(open, ", and ")
		return v
	case len(none) == 1:
		v.answer = noRow + none[0] + " since the last run"
	default:
		v.answer = noRow + strings.Join(none[:len(none)-1], ", ") + " or " + none[len(none)-1] + " since the last run"
	}
	if len(open) > 0 {
		v.answer += "; undecided: " + strings.Join(open, ", and ")
	}
	return v
}

// changesOf reads a's window of the last 60 days and compares it with
// prev, what the last run kept, returning what it found and what the state
// keeps next.
func (r *liveRun) changesOf(ctx context.Context, a liveAccount, prev accountRows, now time.Time) (changesSeen, accountRows) {
	cs := changesSeen{changed: map[string]int{}, compared: prev.Rows != nil}
	w := window{now.Add(-changeWindow), now}
	rep, err := r.read(ctx, a.endpoint("/transactions"), a.path()+"/transactions", txValues(w, ""), "transactions")
	if err != nil {
		cs.failed = true
		return cs, prev
	}
	// The answer came by now, which may be well after the probe began, past
	// the reads before it and any waits of the rate limit or tries again: a
	// row that it holds had posted by then.
	answered := r.now()
	rows, err := decodeList[Transaction](rep.body)
	if err != nil {
		cs.failed = true
		return cs, prev
	}
	r.addHistory(a.n, rows)
	cs.complete = !cutShort(rows, r.pageCap)
	cs.lastCut = cs.compared && !prev.Complete
	kept := map[string]rowSeen{}
	for id, e := range prev.Rows {
		// Pruned before anything is compared, once neither of its times
		// can lie in this window or a later run's.
		if !maxTime(e.Time, e.TradeDate).Before(w.start) {
			kept[id] = e
		}
	}
	next := accountRows{Start: w.start, End: w.end, Complete: cs.complete, Rows: map[string]rowSeen{}}
	seen := map[string]bool{}
	last := window{prev.Start, prev.End}
	for _, row := range rows {
		if row.ActivityID == 0 {
			continue
		}
		id := r.state.digest("activity id", strconv.FormatInt(row.ActivityID, 10))
		seen[id] = true
		digests := r.rowDigests(row.Raw)
		e, ok := kept[id]
		switch {
		case !ok:
			e = rowSeen{FirstSeen: now}
			cs.fresh++
			if !cs.compared || row.Time.After(prev.End) {
				break // a first run's row, or one after the last run, which it could not have held
			}
			// The last run's answer left it out: when that window held both
			// its times, and the answer was complete, it posted after that
			// run, at least this long after its time; else it may have.
			switch least := prev.End.Sub(row.Time); {
			case least <= postingLatency:
				cs.near++
			case !cs.lastCut && last.takesRow(row.Time, row.TradeDate):
				cs.late++
				cs.lateLeast = append(cs.lateLeast, least)
				cs.lateMost = append(cs.lateMost, answered.Sub(row.Time))
			default:
				cs.unsure++
			}
		default:
			var diff []string
			for p := range maps.Keys(digests) {
				if e.Keys[p] != digests[p] {
					diff = append(diff, p)
				}
			}
			for p := range maps.Keys(e.Keys) {
				if _, ok := digests[p]; !ok {
					diff = append(diff, p)
				}
			}
			if len(diff) == 0 {
				cs.same++
			} else {
				cs.changedRows++
				cs.ages = append(cs.ages, now.Sub(row.Time))
				for _, p := range diff {
					cs.changed[p]++
				}
			}
		}
		e.Time, e.TradeDate, e.Keys = row.Time, row.TradeDate, digests
		next.Rows[id] = e
	}
	for id, e := range kept {
		switch {
		case seen[id]:
		case cs.complete && last.takesRow(e.Time, e.TradeDate) && w.takesRow(e.Time, e.TradeDate):
			cs.gone++
		default:
			cs.missing++
			next.Rows[id] = e // not in this answer, but not shown gone
		}
	}
	return cs, next
}

// line returns the report's line for cs, account a's rows against the last
// run's.
func (cs changesSeen) line(a liveAccount) string {
	days := func(ds []time.Duration) string {
		if len(ds) == 0 {
			return ""
		}
		lo, hi := slices.Min(ds), slices.Max(ds)
		if lo/oneDay == hi/oneDay {
			return plural(int(lo/oneDay), "day")
		}
		return fmt.Sprintf("%d to %d days", lo/oneDay, hi/oneDay)
	}
	parts := []string{plural(cs.same, "row") + " as they were", plural(cs.fresh, "row") + " new to the state"}
	if cs.changedRows > 0 {
		var keys []string
		for _, p := range slices.Sorted(maps.Keys(cs.changed)) {
			keys = append(keys, fmt.Sprintf("%s (%d)", p, cs.changed[p]))
		}
		parts = append(parts, fmt.Sprintf("%s changed, %s old, in %s", plural(cs.changedRows, "row"), days(cs.ages), strings.Join(keys, ", ")))
	}
	if cs.late > 0 {
		its := map[bool]string{true: "its time", false: "their times"}[cs.late == 1]
		parts = append(parts, fmt.Sprintf("%s came late, posted %s after %s", plural(cs.late, "row"),
			spanOf(slices.Min(cs.lateLeast), slices.Max(cs.lateMost)), its))
	}
	if cs.near > 0 {
		parts = append(parts, plural(cs.near, "row")+" from the hour before the last run, too near it to be shown late")
	}
	if cs.gone > 0 {
		parts = append(parts, plural(cs.gone, "row")+" gone")
	}
	switch {
	case !cs.complete && cs.missing > 0:
		parts = append(parts, "the answer may have been cut at the cap, so "+plural(cs.missing, "row")+
			" missing from it cannot be shown changed or gone, nor rows that it left out shown late")
	case !cs.complete:
		parts = append(parts, "the answer may have been cut at the cap, so rows that it left out cannot be shown late, "+
			"though no row of the state is missing from it")
	case cs.missing > 0:
		parts = append(parts, plural(cs.missing, "row")+
			" missing from the answer, with its time or tradeDate outside a run's window, so not shown changed or gone")
	}
	switch {
	case cs.lastCut && cs.unsure > 0:
		parts = append(parts, "the last run's answer may have been cut at the cap, so "+plural(cs.unsure, "row")+
			" new to the state cannot be shown late, nor rows that it left out shown changed or gone")
	case cs.lastCut:
		parts = append(parts, "the last run's answer may have been cut at the cap, so rows that it left out cannot be shown changed or gone, "+
			"though no row new to the state could have come late")
	case cs.unsure > 0:
		parts = append(parts, plural(cs.unsure, "row")+" new to the state, with its time or tradeDate outside the last run's window, so not shown late")
	}
	return a.label() + ": " + strings.Join(parts, "; ")
}

// roughly returns d as the report gives a bound of a span, in whole
// minutes under an hour, in whole hours under two days, and in whole days
// past that: rounded up when up is set, as an upper bound is, and down
// otherwise, so that the span holds what it bounds.
func roughly(d time.Duration, up bool) (int, string) {
	size, unit := oneDay, "day"
	switch {
	case d < time.Hour:
		size, unit = time.Minute, "minute"
	case d < 2*oneDay:
		size, unit = time.Hour, "hour"
	}
	n := int(d / size)
	if up && d%size != 0 {
		n++
	}
	return n, unit
}

// spanOf returns the span from lo to hi as the report gives it, lo rounded
// down and hi up: "from 5 hours to 4 days", "from 10 to 22 days", or "3
// days" when the two are one.
func spanOf(lo, hi time.Duration) string {
	nlo, ulo := roughly(lo, false)
	nhi, uhi := roughly(hi, true)
	switch {
	case ulo == uhi && nlo == nhi:
		return plural(nhi, uhi)
	case ulo == uhi:
		return fmt.Sprintf("from %d to %s", nlo, plural(nhi, uhi))
	}
	return fmt.Sprintf("from %s to %s", plural(nlo, ulo), plural(nhi, uhi))
}

// A refreshSeen is Q15's state: the refresh token's digest as each run left
// it, with the login's time.
type refreshSeen struct {
	Runs []refreshRun `json:"runs"`
}

// A refreshRun is one run's refresh token's digest, under its login.
type refreshRun struct {
	Login   string `json:"login"`
	Refresh string `json:"refresh"`
}

// keptRefreshes is how many runs Q15's state keeps.
const keptRefreshes = 20

// The bounds of Q15: how old the access token must be before a refresh,
// which Schwab refuses for a token issued moments ago, and how long it must
// have left, so that it does not lapse during the probe.
const (
	refreshAfter = time.Minute
	refreshLeft  = 5 * time.Minute
	refreshAgain = 30 * time.Second // when the first token is tried once more
)

// sendWith sends one GET of the account numbers with tok, once the rate
// limit lets it go, as the Client's send does, and returns its status: with
// no try again, and no new token after a 401. Only its status is read: the
// scrubber learns what a refusal holds as it arrives, as of every answer
// of the run's.
func (r *liveRun) sendWith(ctx context.Context, tok string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := r.client.take(ctx); err != nil {
		return 0, err
	}
	rep, err := r.client.send(ctx, r.auth.cfg.BaseURL+"/trader/v1/accounts/accountNumbers", tok)
	if err != nil {
		return 0, err
	}
	return rep.status, nil
}

// probeRefresh is Q15, whether a refresh ends the older access tokens, and
// whether it changes the refresh token. It reads the token file for its
// access token, A1, and its refresh token, R1, together, since the Auth's
// token in memory need not be the file's; waits on the run's clock until A1
// is a minute old, since Schwab's refusal of a token issued moments ago
// leads to no refresh; and with under five minutes left on A1, says when to
// try again. One GET with A1 is the control. Then it refreshes, counting
// the guard's token requests across the refresh: a verdict needs exactly
// one, and the file then holding a new access token, A2, else another
// process refreshed in between. Then one GET with A1, one with A2, and A1
// again 30 seconds later. A1 answering 200 both times means older tokens
// survive a refresh for 30 seconds at least, which is all that shows, 401
// both times that a refresh ends them, and 200 then 401 that it ends them
// within 30 seconds; any other pair, as 401 then 200, says nothing. The
// file's refresh token then equal to R1 means it was kept. It never sends R1 itself: after a rotation that could
// end the refresh token that the file holds. However it ends, it keeps the
// digest of the file's refresh token, to compare with the next run's under
// the same login.
func probeRefresh(ctx context.Context, r *liveRun) (v verdict) {
	defer func() { v.evidence = append(v.evidence, r.refreshAcrossRuns()...) }()
	t1, err := readToken(r.auth.tokenFile)
	if err != nil {
		return inconclusive("the token file could not be read")
	}
	a1, r1 := t1.AccessToken, t1.RefreshToken
	if age := r.now().Sub(t1.AccessObtainedAt); !t1.AccessObtainedAt.IsZero() && age < refreshAfter {
		if err := r.wait(ctx, refreshAfter-age+time.Second); err != nil {
			return inconclusive("the run stopped")
		}
	}
	if left := t1.AccessExpiresAt.Sub(r.now()); left < refreshLeft {
		return inconclusive(fmt.Sprintf("the access token has under %v left, so it could lapse within the probe; try again in about %v",
			refreshLeft, (left + 2*time.Minute).Round(time.Minute)))
	}
	switch status, err := r.sendWith(ctx, a1); {
	case err != nil:
		return inconclusive("the control with the token before the refresh failed: " + schwabSays(err))
	case status == http.StatusUnauthorized:
		return inconclusive("the token before the refresh was refused already, as Schwab is reported to end tokens early")
	case status != http.StatusOK:
		return inconclusive("the control with the token before the refresh answered " + strconv.Itoa(status))
	}
	before := r.guard.count().tokens
	got, err := r.auth.Refresh(ctx, a1)
	requests := r.guard.count().tokens - before
	t2, readErr := readToken(r.auth.tokenFile)
	switch {
	case err != nil:
		r.stopIfRefused(err)
		return inconclusive("the refresh failed: " + schwabSays(err))
	case readErr != nil:
		return inconclusive("the token file could not be read after the refresh")
	}
	if why := refreshUnseen(requests, a1, got, t2.AccessToken); why != "" {
		return inconclusive(why)
	}
	statuses := map[string]int{}
	for _, s := range []struct {
		name, tok string
		wait      time.Duration
	}{{"old", a1, 0}, {"new", t2.AccessToken, 0}, {"old again", a1, refreshAgain}} {
		if s.wait > 0 {
			if err := r.wait(ctx, s.wait); err != nil {
				return inconclusive("the run stopped")
			}
		}
		status, err := r.sendWith(ctx, s.tok)
		if err != nil {
			return inconclusive("a GET after the refresh failed: " + schwabSays(err))
		}
		statuses[s.name] = status
	}
	kept := t2.RefreshToken == r1
	ev := lines(
		fmt.Sprintf("after the refresh, the token before it answered %d, and %d when tried again %v later; the new token answered %d",
			statuses["old"], statuses["old again"], refreshAgain, statuses["new"]),
		"the refresh token after the refresh is "+map[bool]string{true: "the one before it", false: "a new one"}[kept],
	)
	var survive string
	switch {
	case statuses["new"] != http.StatusOK:
		return inconclusive("the new token answered "+strconv.Itoa(statuses["new"]), ev...)
	case statuses["old"] == http.StatusOK && statuses["old again"] == http.StatusOK:
		survive = "a refresh leaves older access tokens working for " + refreshAgain.String() + " at least"
	case statuses["old"] == http.StatusUnauthorized && statuses["old again"] == http.StatusUnauthorized:
		survive = "a refresh ends older access tokens"
	case statuses["old"] == http.StatusOK && statuses["old again"] == http.StatusUnauthorized:
		survive = "a refresh ends older access tokens within " + refreshAgain.String()
	default:
		return inconclusive(fmt.Sprintf("the token before the refresh answered %d, then %d %v later", statuses["old"], statuses["old again"], refreshAgain), ev...)
	}
	keeps := "and keeps the refresh token"
	if !kept {
		keeps = "and changes the refresh token"
	}
	return verdict{answer: survive + ", " + keeps, evidence: ev}
}

// refreshUnseen returns why a refresh from a1 that made requests token
// requests and returned got, after which the token file held held, shows
// nothing of what a refresh does to older tokens, or "": a verdict needs
// exactly one token request, and the file then holding that request's new
// access token, else another process refreshed, or saved a token, in
// between.
func refreshUnseen(requests int, a1, got, held string) string {
	switch {
	case requests != 1:
		return fmt.Sprintf("the refresh made %s, where a verdict needs exactly one, as when another process refreshed in between",
			plural(requests, "token request"))
	case held == a1 || held != got:
		return "the token file does not hold the refresh's new access token, as when another process saved one in between"
	}
	return ""
}

// refreshAcrossRuns compares the refresh token that the token file holds,
// as this run leaves it, with the last run's under the same login, and
// keeps its digest in the state: a refresh token that changes within one
// login shows that some refresh changed it.
func (r *liveRun) refreshAcrossRuns() []reportLine {
	t, err := readToken(r.auth.tokenFile)
	if err != nil {
		return lines("the token file could not be read, to compare its refresh token with the last run's")
	}
	var st refreshSeen
	if _, err := r.state.get("Q15", &st); err != nil {
		return lines("the state's refresh tokens could not be read")
	}
	login := t.RefreshObtainedAt.UTC().Format(time.RFC3339Nano)
	mine := refreshRun{Login: login, Refresh: r.state.digest("refresh token", t.RefreshToken)}
	line := "no earlier run under this login, to compare its refresh token with"
	for _, run := range slices.Backward(st.Runs) {
		if run.Login == login {
			line = "since the last run under this login, the refresh token " + map[bool]string{true: "stayed the same", false: "changed"}[run.Refresh == mine.Refresh]
			break
		}
	}
	st.Runs = append(st.Runs, mine)
	st.Runs = st.Runs[max(0, len(st.Runs)-keptRefreshes):]
	if err := r.state.put("Q15", st); err != nil {
		r.fail("Q15's state could not be kept")
	}
	return lines(line)
}

// Q8 decides only across an ex-date that lies between two runs, by when
// each read its closes: an earlier run before the close of the session
// before the ex-date, the last day before it among this run's closes, the
// latest such of the runs kept, and this run after the ex-date, its closes
// reaching it. It compares the closes before the ex-date that both runs
// hold, of days that had ended when the earlier run read them: unchanged
// to the cent is split-adjusted only, all scaled alike by the dividend's
// factor dividend-adjusted, however small the dividend, and changed
// otherwise neither.
func TestCompareCloses(t *testing.T) {
	at := func(day string, hour, minute int) time.Time {
		d, err := time.ParseInLocation(time.DateOnly, day, newYork())
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(d.Year(), d.Month(), d.Day(), hour, minute, 0, 0, newYork())
	}
	// The last run, on Friday 2026-09-11 before the open, a week before the
	// ex-date, holds the closes of three days before it.
	prev := symbolCloses{
		AsOf: "2026-09-10", Read: at("2026-09-11", 9, 0),
		Closes: map[string]float64{"2026-09-08": 600, "2026-09-09": 610, "2026-09-10": 620},
	}
	withRead := func(sc symbolCloses, read time.Time) symbolCloses {
		sc.Read = read
		return sc
	}
	// This run, on Monday 2026-09-28, holds the closes of each weekday from
	// 2026-09-08 to the Friday before it, those that prev holds scaled by f.
	scaled := func(f float64) symbolCloses {
		cur := symbolCloses{AsOf: "2026-09-25", Read: at("2026-09-28", 10, 0), Closes: map[string]float64{}}
		for day := at("2026-09-08", 0, 0); !day.After(at("2026-09-25", 0, 0)); day = day.AddDate(0, 0, 1) {
			if day.Weekday() != time.Saturday && day.Weekday() != time.Sunday {
				cur.Closes[nyDate(day)] = 630
			}
		}
		for d, c := range prev.Closes {
			cur.Closes[d] = math.Round(c*f*100) / 100
		}
		return cur
	}
	q := dividendQuote{exDate: "2026-09-18", amount: 2}
	factor := 1 - 2.0/620
	// Closes scaled by ratios whose midpoint is the dividend's factor, though
	// they differ: no adjustment for a dividend scales them so.
	uneven := scaled(1)
	for d, ratio := range map[string]float64{"2026-09-08": factor - 0.002, "2026-09-09": factor, "2026-09-10": factor + 0.002} {
		uneven.Closes[d] = math.Round(prev.Closes[d]*ratio*100) / 100
	}
	// A last run during Friday's session, which read Friday's close as it
	// stood then, a price that the day's close need not be.
	during := withRead(prev, at("2026-09-11", 11, 0))
	during.Closes = maps.Clone(prev.Closes)
	during.Closes["2026-09-11"] = 633
	stale := scaled(1)
	stale.AsOf, stale.Read = "2026-09-17", at("2026-09-21", 10, 0)
	for d := range stale.Closes {
		if d > stale.AsOf {
			delete(stale.Closes, d)
		}
	}
	noneInCommon := scaled(1)
	for d := range prev.Closes {
		delete(noneInCommon.Closes, d)
	}
	// A run on the ex-date's morning, which read closes adjusted already.
	adjusted := withRead(scaled(factor), at("2026-09-18", 8, 0))
	adjusted.AsOf = "2026-09-17"
	for d := range adjusted.Closes {
		if d > adjusted.AsOf {
			delete(adjusted.Closes, d)
		}
	}
	// An older run, with a close that this run's do not scale by the
	// dividend's factor.
	odd := symbolCloses{AsOf: "2026-09-08", Read: at("2026-09-09", 9, 0), Closes: map[string]float64{"2026-09-08": 590}}
	const (
		notByFactor = "the closes before the ex-date changed, though not by the dividend's factor"
		lastTooLate = "no earlier run came before the last close before its ex-date, 2026-09-18"
	)
	one := func(sc symbolCloses) []symbolCloses { return []symbolCloses{sc} }
	for _, tc := range []struct {
		name        string
		runs        []symbolCloses
		cur         symbolCloses
		q           dividendQuote
		answer, why string
	}{
		{"unchanged", one(prev), scaled(1), q, adjustNone, ""},
		{"scaled by the dividend's factor", one(prev), scaled(factor), q, adjustDividend, ""},
		{"scaled by a small dividend's factor", one(prev), scaled(1 - 0.3/620), dividendQuote{exDate: "2026-09-18", amount: 0.3}, adjustDividend, ""},
		{"scaled otherwise", one(prev), scaled(0.9), q, "", notByFactor},
		{"scaled unevenly, by the dividend's factor halfway", one(prev), uneven, q, "", notByFactor},
		{"no earlier run", nil, scaled(1), q, "", "no earlier run kept its closes"},
		{"an earlier run's closes without when it read them", one(withRead(prev, time.Time{})), scaled(1), q, "", "no earlier run kept its closes"},
		{"no ex-date", one(prev), scaled(1), dividendQuote{}, "", "the quote gave no ex-date"},
		{"the last run on the ex-date's morning", one(withRead(prev, at("2026-09-18", 8, 0))), scaled(1), q, "", lastTooLate},
		{"the last run on the evening before the ex-date", one(withRead(prev, at("2026-09-17", 20, 0))), scaled(1), q, "", lastTooLate},
		{"the last run at the close before the ex-date", one(withRead(prev, at("2026-09-17", 16, 0))), scaled(1), q, "", lastTooLate},
		{"the last run a minute before the close before the ex-date", one(withRead(prev, at("2026-09-17", 15, 59))), scaled(1), q, adjustNone, ""},
		{"the last run during a session, whose close it read early", one(during), scaled(1), q, adjustNone, ""},
		// A run on the ex-date's morning keeps the run before it, which
		// counts, whether the closes it read were adjusted or not.
		{"a run before the close, then one on the ex-date's morning", []symbolCloses{prev, adjusted}, scaled(factor), q, adjustDividend, ""},
		{"a run on the ex-date's morning, then one before the close, kept out of order", []symbolCloses{adjusted, prev}, scaled(1), q, adjustNone, ""},
		{"the latest run before the close", []symbolCloses{withRead(prev, at("2026-09-09", 9, 0)), during, adjusted}, scaled(1), q, adjustNone, ""},
		{"the latest run before the close, kept before an older one", []symbolCloses{during, odd}, scaled(factor), q, adjustDividend, ""},
		{"no run before the close", []symbolCloses{withRead(prev, at("2026-09-17", 20, 0)), adjusted}, scaled(1), q, "", lastTooLate},
		{"this run on the ex-date, after its close", one(prev), withRead(scaled(1), at("2026-09-18", 17, 0)), q, "", "its ex-date, 2026-09-18, is not over yet"},
		{"an ex-date after this run", one(prev), scaled(1), dividendQuote{exDate: "2026-09-29", amount: 2}, "", "its ex-date, 2026-09-29, is not over yet"},
		{"this run's closes ending before the ex-date", one(prev), stale, q, "", "this run's closes end before its ex-date, 2026-09-18"},
		{
			"an ex-date before the last run", one(prev), scaled(1),
			dividendQuote{exDate: "2026-09-10", amount: 2},
			"", "no earlier run came before the last close before its ex-date, 2026-09-10",
		},
		{"no close in common", one(prev), noneInCommon, q, "", "the two runs have no close before the ex-date in common"},
	} {
		if answer, why := compareCloses(tc.runs, tc.cur, tc.q); answer != tc.answer || why != tc.why {
			t.Errorf("%s: %q, %q; want %q, %q", tc.name, answer, why, tc.answer, tc.why)
		}
	}
}

// A refresh shows what it does to older tokens only when it made exactly
// one token request, and the token file then holds that request's new
// access token: not the old one, nor another that a process saved in
// between.
func TestRefreshUnseen(t *testing.T) {
	const crowd = "as when another process saved one in between"
	for _, tc := range []struct {
		name           string
		requests       int
		got, held, why string
	}{
		{"one request, its token held", 1, "zq-a2", "zq-a2", ""},
		{"no request", 0, "zq-a2", "zq-a2", "the refresh made 0 token requests, where a verdict needs exactly one, as when another process refreshed in between"},
		{"two requests", 2, "zq-a2", "zq-a2", "the refresh made 2 token requests, where a verdict needs exactly one, as when another process refreshed in between"},
		{"the old token held", 1, "zq-a1", "zq-a1", "the token file does not hold the refresh's new access token, " + crowd},
		{"another token held", 1, "zq-a2", "zq-a3", "the token file does not hold the refresh's new access token, " + crowd},
	} {
		if why := refreshUnseen(tc.requests, "zq-a1", tc.got, tc.held); why != tc.why {
			t.Errorf("%s: %q, want %q", tc.name, why, tc.why)
		}
	}
}

// A quote gives each symbol's last ex-date, a date alone, and its amount;
// a price history gives its closes by New York date, the last keptCloses
// of them, and the date of the last.
func TestReadMarketData(t *testing.T) {
	qs, err := readQuotes([]byte(`{"SPY":{"fundamental":{"divExDate":"2026-09-18T00:00:00Z","divPayAmount":1.9}},` +
		`"VTI":{"fundamental":{"divExDate":"soon"}},"BND":{}}`))
	if err != nil || qs["SPY"] != (dividendQuote{exDate: "2026-09-18", amount: 1.9}) || qs["VTI"].exDate != "" || qs["BND"].exDate != "" {
		t.Errorf("readQuotes: %+v, %v", qs, err)
	}
	if _, err := readQuotes([]byte(`[]`)); err == nil {
		t.Error("a list read as quotes")
	}
	var candles []string
	start := time.Date(2026, 1, 1, 6, 0, 0, 0, time.UTC) // midnight in Chicago
	for i := range keptCloses + 10 {
		candles = append(candles, fmt.Sprintf(`{"close":%d,"datetime":%d}`, 100+i, start.AddDate(0, 0, i).UnixMilli()))
	}
	sc, err := readCloses([]byte(`{"candles":[` + strings.Join(candles, ",") + `]}`))
	last := start.AddDate(0, 0, keptCloses+9)
	if err != nil || len(sc.Closes) != keptCloses || sc.AsOf != nyDate(last) || sc.Closes[nyDate(last)] != float64(100+keptCloses+9) ||
		sc.Closes[nyDate(start)] != 0 {
		t.Errorf("readCloses: %d closes as of %s, %v", len(sc.Closes), sc.AsOf, err)
	}
}

// A span of how long after its time a row posted is given in minutes,
// hours or days, as long as it is, its start rounded down and its end up,
// so that it holds the time it bounds.
func TestSpanOf(t *testing.T) {
	for _, tc := range []struct {
		lo, hi time.Duration
		want   string
	}{
		{2 * time.Minute, 3 * oneDay, "from 2 minutes to 3 days"},
		{10 * oneDay, 22 * oneDay, "from 10 to 22 days"},
		{5 * time.Hour, 3*oneDay + 5*time.Hour, "from 5 hours to 4 days"},
		{90 * time.Minute, 47 * time.Hour, "from 1 to 47 hours"},
		{47*time.Hour + 59*time.Minute, 2 * oneDay, "from 47 hours to 2 days"},
		{59*time.Minute + 59*time.Second, time.Hour, "from 59 minutes to 1 hour"},
		{90 * time.Second, 119 * time.Second, "from 1 to 2 minutes"},
		{3 * oneDay, 3 * oneDay, "3 days"},
	} {
		if got := spanOf(tc.lo, tc.hi); got != tc.want {
			t.Errorf("spanOf(%v, %v) = %q, want %q", tc.lo, tc.hi, got, tc.want)
		}
	}
}

// Q10 holds a hash against the runs of other logins of the app, and of
// other apps, apart: the same, changed, or not seen yet; runs of this login
// of this app say nothing.
func TestCompareHash(t *testing.T) {
	seen := []hashSeen{
		{Hash: "h1", Login: "L1", App: "A"},
		{Hash: "h2", Login: "L2", App: "A"},
		{Hash: "h1", Login: "L1", App: "B"},
	}
	for _, tc := range []struct {
		hash, login, app string
		logins, apps     string
	}{
		{"h1", "L1", "A", hashChanged, hashSame}, // L2 had h2
		{"h2", "L2", "A", hashChanged, hashChanged},
		{"h1", "L3", "A", hashChanged, hashSame},
		{"h1", "L1", "B", hashNew, hashChanged}, // A's L2 had h2
		{"h9", "L1", "C", hashNew, hashChanged},
	} {
		if logins, apps := compareHash(seen, tc.hash, tc.login, tc.app); logins != tc.logins || apps != tc.apps {
			t.Errorf("compareHash(%s, %s, %s) = %s, %s; want %s, %s", tc.hash, tc.login, tc.app, logins, apps, tc.logins, tc.apps)
		}
	}
	if logins, apps := compareHash(seen[:1], "h1", "L1", "A"); logins != hashNew || apps != hashNew {
		t.Errorf("only this login's run: %s, %s", logins, apps)
	}
	// A hash that changed, and then stayed the same, changed: a later run
	// that saw this run's hash does not hide an earlier one that did not.
	changedThenSame := []hashSeen{{Hash: "h0", Login: "L1", App: "A"}, {Hash: "h1", Login: "L2", App: "A"}}
	if logins, _ := compareHash(changedThenSame, "h1", "L3", "A"); logins != hashChanged {
		t.Errorf("a hash that changed, then stayed: %s", logins)
	}
}

// Q12 digests a row by key path: each scalar as JSON, an array's elements
// under its path, an empty array as [], and an instrument cut to its
// assetType and instrumentId, so that a change elsewhere in it does not
// count.
func TestKeyValues(t *testing.T) {
	row := `{"activityId":1,"netAmount":-1.5,"qualifiedDividend":null,"transferItems":[` +
		`{"instrument":{"assetType":"EQUITY","symbol":"X","instrumentId":7,"closingPrice":5},"amount":1},` +
		`{"instrument":{"assetType":"CURRENCY"},"amount":-0.65,"feeType":"COMMISSION"}],"tags":[],"odd key":"x"}`
	want := map[string][]string{
		"activityId":                            {"1"},
		"netAmount":                             {"-1.5"},
		"qualifiedDividend":                     {"null"},
		"transferItems.instrument.assetType":    {`"EQUITY"`, `"CURRENCY"`},
		"transferItems.instrument.instrumentId": {"7"},
		"transferItems.amount":                  {"1", "-0.65"},
		"transferItems.feeType":                 {`"COMMISSION"`},
		"tags":                                  {"[]"},
		`"odd key"`:                             {`"x"`},
	}
	got := keyValues(json.RawMessage(row))
	if len(got) != len(want) {
		t.Errorf("keyValues: %q", got)
	}
	for p, vs := range want {
		if !slices.Equal(got[p], vs) {
			t.Errorf("keyValues: %s = %q, want %q", p, got[p], vs)
		}
	}
	if keyValues(json.RawMessage(`{`)) != nil {
		t.Error("keyValues of what is not JSON")
	}
	// A change in an instrument's symbol does not count; a change in its
	// assetType does.
	r := &liveRun{state: &stateStore{key: make([]byte, stateKeySize)}}
	base := r.rowDigests(json.RawMessage(row))
	if !maps.Equal(base, r.rowDigests(json.RawMessage(strings.Replace(row, `"symbol":"X"`, `"symbol":"Y"`, 1)))) ||
		maps.Equal(base, r.rowDigests(json.RawMessage(strings.Replace(row, `"EQUITY"`, `"OPTION"`, 1)))) {
		t.Error("a row's digests follow its instrument's symbol, or not its assetType")
	}
}

// Q12 takes a window to hold a row only when both its time and its
// tradeDate lie in it, bounds included, whichever of the two windows go by;
// a row without a tradeDate, by its time alone.
func TestWindowTakesRow(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2026, 7, d, h, 0, 0, 0, time.UTC) }
	w := window{day(15, 10), day(20, 10)}
	for _, tc := range []struct {
		time, tradeDate time.Time
		want            bool
	}{
		{day(16, 14), day(16, 4), true},
		{day(15, 14), day(15, 4), false}, // dated before the start
		{day(20, 9), day(21, 4), false},  // dated after the end
		{day(15, 9), day(16, 4), false},  // before the start
		{day(15, 10), day(20, 10), true}, // on the bounds
		{day(15, 14), time.Time{}, true},
		{day(14, 14), time.Time{}, false},
	} {
		if got := w.takesRow(tc.time, tc.tradeDate); got != tc.want {
			t.Errorf("takesRow(%v, %v) = %v", tc.time, tc.tradeDate, got)
		}
	}
}

// Q10 under another app: a hash that another app's run saw the same stays
// the same across apps; and when hashes changed across this app's logins
// too, a hash that differs across apps is no more than the logins could
// account for, since every run of another app is under another login.
func TestProbeHashesAcrossApps(t *testing.T) {
	dir := t.TempDir()
	login := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	tokenFile := filepath.Join(dir, "token.json")
	if err := writeToken(tokenFile, &token{AccessToken: "a", RefreshToken: "r", RefreshObtainedAt: login, AccessExpiresAt: login.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Q10 sends nothing; a request that went would fail here, not reach
	// Schwab.
	stub := &stubTransport{answer: func(*http.Request) (*http.Response, error) { return nil, errors.New("Q10 sends nothing") }}
	t.Cleanup(func() {
		if got := stub.sent(); len(got) > 0 {
			t.Errorf("sent %v", got)
		}
	})
	a, err := NewAuth(Config{AppKey: "app-b", AppSecret: "secret", TokenFile: tokenFile, HTTPClient: &http.Client{Transport: stub}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		seen []struct{ hash, login, app string }
		want string
	}{
		{"the same across apps", []struct{ hash, login, app string }{{"HASH", "earlier", "app-a"}}, "hashes stayed the same across apps"},
		{
			"changed across logins and apps",
			[]struct{ hash, login, app string }{{"OLD", "earlier", "app-b"}, {"OTHER", "earlier", "app-a"}},
			"hashes changed across logins; they differ across apps too, which the change across logins could account for",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := openState(filepath.Join(t.TempDir(), "state.json"), filepath.Join(t.TempDir(), "state.key"), func(msg string) { t.Error(msg) })
			if err != nil {
				t.Fatal(err)
			}
			acct := liveAccount{n: 1, number: "29470135", hash: "HASH"}
			var planted hashState
			planted.Accounts = map[string][]hashSeen{}
			key := st.digest("account number", acct.number)
			for _, s := range tc.seen {
				planted.Accounts[key] = append(planted.Accounts[key], hashSeen{Hash: st.digest("hash", s.hash), Login: s.login, App: st.digest("app key", s.app)})
			}
			if err := st.put("Q10", planted); err != nil {
				t.Fatal(err)
			}
			r := &liveRun{state: st, auth: a, accounts: []liveAccount{acct}}
			if v := probeHashes(t.Context(), r); v.answer != tc.want {
				t.Errorf("verdict %q, want %q", v.answer, tc.want)
			}
			var kept hashState
			if _, err := st.get("Q10", &kept); err != nil || len(kept.Accounts[key]) != len(tc.seen)+1 {
				t.Errorf("the state keeps %d runs, %v", len(kept.Accounts[key]), err)
			}
		})
	}
}
