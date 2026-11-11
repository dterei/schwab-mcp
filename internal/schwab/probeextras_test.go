package schwab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A boundsSeen is whether each of the four windows around a row's time t
// held the row: [t-1s, t.000], [t-1s, t.999], [t.000, t+1s] and [t.001,
// t+1s].
type boundsSeen [4]bool

// boundsWindows returns the four windows of a boundsSeen around t, a row's
// time, which shows whole seconds.
func boundsWindows(t time.Time) [4]window {
	ms := time.Millisecond
	return [4]window{{t.Add(-time.Second), t}, {t.Add(-time.Second), t.Add(999 * ms)}, {t, t.Add(time.Second)}, {t.Add(ms), t.Add(time.Second)}}
}

// The words for a bound of a window.
const (
	boundIn  = "inclusive"
	boundOut = "exclusive"
)

// decide returns what b shows of the windows' bounds: whether the start and
// the end are inclusive, with ok set; whether Schwab does not keep a
// bound's milliseconds, coarse; and whether the row's time may be finer
// than it shows, hidden. The first window and the last both holding the
// row show coarse bounds, since no time is both at or before t.000 and at
// or after t.001: Schwab drops the milliseconds, taking t.001 to t, or
// rounds them, taking t.001 to t and t.999 to t+1s, and either way whole
// seconds decide the bounds, [t-1s, t] and [t, t+1s] both holding the
// row. The last holding it while [t-1s, t.999] does not, which a row shown
// at t lies in unless Schwab drops the milliseconds, shows them dropped,
// coarse, with the row left out of [t-1s, t] by an exclusive end or a time
// finer than it shows, which settles neither bound. The last holding it
// otherwise, while the first does not, shows a time finer than the row
// shows, which settles neither bound, or bounds rounded to whole seconds
// with an exclusive end, which four windows cannot tell apart: hidden,
// with ok false, and the start and the end as rounded bounds would have
// them. ok is false too when b settles nothing, as when the row is not in
// the windows that must hold it, or a window holds it while one that
// takes in all it does not: [t-1s, t.999] all of [t-1s, t.000], and
// [t.000, t+1s] all of [t.001, t+1s].
func (b boundsSeen) decide() (start, end string, coarse, hidden, ok bool) {
	switch {
	case b[0] && !b[1], b[3] && !b[2]:
		return "", "", false, false, false
	case b[0] && b[3]:
		return boundIn, boundIn, true, false, true
	case b[3] && !b[1]:
		return "", "", true, false, false
	case b[3]:
		return boundIn, boundOut, false, true, false
	case b[0]:
		end = boundIn
	case b[1]:
		end = boundOut
	default:
		return "", "", false, false, false
	}
	start = boundOut
	if b[2] {
		start = boundIn
	}
	return start, end, false, false, true
}

// extrasRows are the history rows that the extras ask about, from one
// account's: one or two rows whose time no other row shares, for the
// bounds; a row whose time and tradeDate lie two hours or more apart, a
// month-end interest row if there is one, for which time a window takes a
// row by; and the newest row with an id, to get by its id.
type extrasRows struct {
	bounds   []Transaction
	monthEnd *Transaction
	byID     *Transaction
}

// pickExtras returns the rows of extrasRows from rows, newest first.
func pickExtras(rows []Transaction) extrasRows {
	rows = slices.Clone(rows)
	slices.SortStableFunc(rows, func(a, b Transaction) int { return b.Time.Compare(a.Time) })
	var x extrasRows
	for i, row := range rows {
		if row.ActivityID == 0 || row.Time.IsZero() {
			continue
		}
		if x.byID == nil {
			x.byID = &rows[i]
		}
		alone := !slices.ContainsFunc(rows, func(o Transaction) bool {
			return !sameRow(o, row) && absDur(o.Time.Sub(row.Time)) <= 2*time.Second
		})
		if alone && len(x.bounds) < 2 && row.Time.Nanosecond() == 0 {
			x.bounds = append(x.bounds, row)
		}
		apart := !row.TradeDate.IsZero() && absDur(row.Time.Sub(row.TradeDate)) >= 2*time.Hour
		if apart && (x.monthEnd == nil || row.Description == "Free Balance Interest Adjustment" && x.monthEnd.Description != row.Description) {
			x.monthEnd = &rows[i]
		}
	}
	return x
}

// allTypes is the types of a request for every type of transaction, as
// the Client's Transactions sends them.
func allTypes() string {
	names := make([]string, 0, len(AllTransactionTypes()))
	for _, t := range AllTransactionTypes() {
		names = append(names, string(t))
	}
	return strings.Join(names, ",")
}

// rawWindow returns the query of a request for transactions in w, as
// txValues does but without its checks, for the extras that send what the
// Client's query refuses; without types when types is false.
func rawWindow(w window, types bool) url.Values {
	q := url.Values{"startDate": {w.start.UTC().Format(schwabTimeFormat)}, "endDate": {w.end.UTC().Format(schwabTimeFormat)}}
	if types {
		q.Set("types", allTypes())
	}
	return q
}

// sendBare sends one GET of path with authorization as its Authorization
// header, or none when it is "", once the rate limit lets it go, and
// returns the answer, read as the Client's send reads one: the extras'
// requests with a garbage token and with none, which the Client cannot
// send. The scrubber learns what the answer holds as it arrives, as it
// does of every answer of the run's.
func (r *liveRun) sendBare(ctx context.Context, path, authorization string) (*reply, error) {
	if err := r.client.take(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.auth.cfg.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.auth.cfg.HTTPClient.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // which would give the whole address
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBody)+1))
	if err != nil || len(body) > maxBody {
		return nil, errors.New("the answer could not be read whole")
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// probeExtras sends the extras, single requests whose answers milestone 2
// relies on, each reported on its own line, of one account with history
// rows: a window whose start equals its end, at a row's time; windows of
// 365 days and a second, and of 366 days; one without types, against the
// same with every type; a startDate that is a date alone; for one or two
// rows at a known time t, four windows, [t-1s, t.000], [t-1s, t.999],
// [t.000, t+1s] and [t.001, t+1s], which say whether the bounds are
// inclusive and whether Schwab keeps their milliseconds, as decide says:
// the last holding the row shows that Schwab drops or rounds them, or that
// the row's time is finer than it shows, which settles neither bound, and
// the four cannot tell a finer time from bounds rounded with an exclusive
// end; two windows around a row whose time
// and tradeDate lie apart, which say which a window takes a row by; a row
// by its id, against its bytes in the list; an unknown hash, and an unknown
// activityId, unless a history row has it; and the 401 for a garbage token
// and for none.
func probeExtras(ctx context.Context, r *liveRun) verdict {
	var a liveAccount
	for _, acct := range r.accounts {
		if acct.hash != "" && (a.hash == "" || len(r.history[a.n]) == 0 && len(r.history[acct.n]) > 0) {
			a = acct
		}
	}
	if a.hash == "" {
		return inconclusive("no account has a hash")
	}
	x := pickExtras(r.history[a.n])
	endpoint, path := a.endpoint("/transactions"), a.path()+"/transactions"
	// held asks for q, and reports whether its answer holds row, and how
	// many rows it holds, or what Schwab said.
	held := func(q url.Values, row *Transaction) (in bool, n int, said string, ok bool) {
		rep, err := r.ask(ctx, endpoint, path, q, "transactions")
		if err != nil {
			return false, 0, "refused, " + schwabSays(err), false
		}
		rows, err := decodeList[Transaction](rep.body)
		if err != nil {
			return false, 0, "200, but not a list", false
		}
		if row != nil {
			in = slices.ContainsFunc(rows, func(o Transaction) bool { return o.ActivityID == row.ActivityID })
		}
		return in, len(rows), "200, " + plural(len(rows), "row"), true
	}
	now := r.now()
	var v verdict
	add := func(format string, args ...any) {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf(format, args...)})
	}

	at, atRow := now.Truncate(time.Second), (*Transaction)(nil)
	if len(x.bounds) > 0 {
		at, atRow = x.bounds[0].Time, &x.bounds[0]
	}
	in, _, said, ok := held(rawWindow(window{at, at}, true), atRow)
	if ok && atRow != nil {
		said += map[bool]string{true: ", the row's among them", false: ", without the row"}[in]
	}
	add("a window whose start is its end, at %s: %s", map[bool]string{true: "a row's time", false: "the run's time"}[atRow != nil], said)
	_, _, said, _ = held(rawWindow(window{now.Add(-MaxTransactionWindow - time.Second), now}, true), nil)
	add("a window of 365 days and a second: %s", said)
	_, _, said, _ = held(rawWindow(window{now.Add(-366 * oneDay), now}, true), nil)
	add("a window of 366 days: %s", said)

	around := window{now.Add(-30 * oneDay), now}
	if atRow != nil {
		around = window{at.Add(-oneDay), at.Add(oneDay)}
	}
	without, err := r.ask(ctx, endpoint, path, rawWindow(around, false), "transactions")
	if err != nil {
		add("a window without types: refused, %s", schwabSays(err))
	} else {
		every, err := r.ask(ctx, endpoint, path, rawWindow(around, true), "transactions")
		if err != nil {
			add("a window without types: %d; the same window with every type: refused, %s", without.status, schwabSays(err))
		} else {
			add("a window without types: 200; against the same window with every type, %s", sameIDs(without.body, every.body))
		}
	}
	date := url.Values{"startDate": {now.Add(-oneDay).UTC().Format(time.DateOnly)}, "endDate": {now.UTC().Format(schwabTimeFormat)}}
	date.Set("types", rawWindow(around, true).Get("types"))
	_, _, said, _ = held(date, nil)
	add("a startDate that is a date alone: %s", said)

	bounds := r.extrasBounds(x.bounds, held, add)
	filter := r.extrasFilterOn(x.monthEnd, held, add)

	if x.byID != nil {
		id := strconv.FormatInt(x.byID.ActivityID, 10)
		rep, err := r.ask(ctx, a.endpoint("/transactions/"+idLabel), path+"/"+id, nil, "transaction")
		switch {
		case err != nil:
			add("a row by its id: refused, %s", schwabSays(err))
		case bytes.Equal(bytes.TrimSpace(rep.body), bytes.TrimSpace(x.byID.Raw)):
			add("a row by its id is byte for byte its row in the list")
		case compactEqual(rep.body, x.byID.Raw):
			add("a row by its id is its row in the list but for white space")
		default:
			add("a row by its id differs from its row in the list")
		}
	}
	_, err = r.ask(ctx, "accounts/{unknown hash}", "/trader/v1/accounts/"+url.PathEscape(schwabtest.UnknownHash), nil, "account")
	add("an unknown hash: %s", map[bool]string{true: "200", false: "refused, " + schwabSays(err)}[err == nil])
	if slices.ContainsFunc(r.history[a.n], func(t Transaction) bool { return t.ActivityID == schwabtest.UnknownActivityID }) {
		add("an unknown activityId: not asked, since a history row has it")
	} else {
		_, err = r.ask(ctx, a.endpoint("/transactions/"+idLabel), path+"/"+strconv.FormatInt(schwabtest.UnknownActivityID, 10), nil, "transaction")
		add("an unknown activityId: %s", map[bool]string{true: "200", false: "refused, " + schwabSays(err)}[err == nil])
	}
	for _, tok := range []struct{ name, header string }{{"a garbage token", "Bearer not-a-token"}, {"no token", ""}} {
		rep, err := r.sendBare(ctx, "/trader/v1/accounts/accountNumbers", tok.header)
		if err != nil {
			add("%s: failed, %s", tok.name, schwabSays(err))
			continue
		}
		e := newError(rep.status, rep.header, rep.body)
		answered := strconv.Itoa(rep.status) + ", " + contentType(rep.header)
		if s := errorSays(e); s != "" && rep.status != http.StatusOK {
			answered += ", " + s
		}
		add("%s: %s; %s", tok.name, answered, correlationNames(rep.header))
	}
	v.answer = bounds + "; " + filter
	return v
}

// sameIDs says whether two answers of transactions hold the same rows, by
// their ids.
func sameIDs(a, b []byte) string {
	ids := func(body []byte) ([]int64, bool) {
		rows, err := decodeList[Transaction](body)
		if err != nil {
			return nil, false
		}
		var ids []int64
		for _, row := range rows {
			ids = append(ids, row.ActivityID)
		}
		slices.Sort(ids)
		return ids, true
	}
	ia, okA := ids(a)
	ib, okB := ids(b)
	switch {
	case !okA || !okB:
		return "one answer is not a list"
	case slices.Equal(ia, ib):
		return "the same " + plural(len(ia), "row")
	}
	return fmt.Sprintf("%s, where every type gives %d", plural(len(ia), "row"), len(ib))
}

// compactEqual reports whether a and b are the same JSON once white space
// is left out.
func compactEqual(a, b []byte) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}

// extrasBounds sends the four windows around each of rows, through held,
// and returns what they say of a window's bounds, adding a line for each
// row.
func (r *liveRun) extrasBounds(rows []Transaction, held func(url.Values, *Transaction) (bool, int, string, bool), add func(string, ...any)) string {
	if len(rows) == 0 {
		add("the bounds: no row of the history has a time of its own")
		return "the bounds: inconclusive, with no row to ask about"
	}
	var starts, ends []string
	hidden, coarse, dropped := 0, 0, 0
	rounded := "" // the bounds by whole seconds of a row whose windows show rounded bounds or a finer time
	for i := range rows {
		var b boundsSeen
		failed := false
		for k, w := range boundsWindows(rows[i].Time) {
			in, _, _, ok := held(rawWindow(w, true), &rows[i])
			b[k], failed = in, failed || !ok
		}
		start, end, whole, finer, ok := b.decide()
		if whole && !failed {
			dropped++
		}
		switch {
		case failed:
			add("the bounds, row %d: a window was refused, so it settles nothing", i+1)
		case finer:
			hidden++
			rounded = "a window's start is " + start + " and its end " + end
			add("the bounds, row %d: [t.001, t+1s] holds it and [t-1s, t.000] does not, so either Schwab keeps a finer time than the row shows, "+
				"which settles neither bound, or it rounds a bound's milliseconds, under which by whole seconds the start is %s and the end %s", i+1, start, end)
		case whole && !ok:
			add("the bounds, row %d: [t.001, t+1s] holds it and [t-1s, t.999] does not, so Schwab drops a bound's milliseconds, "+
				"and an exclusive end, or a finer time than the row shows, leaves it out of [t-1s, t]: it settles neither bound", i+1)
		case !ok:
			add("the bounds, row %d: the windows that must hold it do not, so it settles nothing", i+1)
		case whole:
			coarse++
			starts, ends = append(starts, start), append(ends, end)
			add("the bounds, row %d: [t-1s, t.000] and [t.001, t+1s] both hold it, so Schwab does not keep a bound's milliseconds: it drops or rounds them; "+
				"by whole seconds the start is %s and the end %s", i+1, start, end)
		default:
			starts, ends = append(starts, start), append(ends, end)
			add("the bounds, row %d: the start is %s and the end %s", i+1, start, end)
		}
	}
	finer := ""
	if hidden > 0 {
		finer = "; " + plural(hidden, "row") + " kept a finer time than it shows"
	}
	switch {
	case len(starts) == 0 && hidden > 0:
		return "the bounds: inconclusive, since either a row's time is finer than it shows, or Schwab rounds a bound's milliseconds, " +
			"and by whole seconds " + rounded
	case len(starts) == 0 && dropped > 0:
		return "the bounds: inconclusive, though Schwab drops a bound's milliseconds"
	case len(starts) == 0:
		return "the bounds: inconclusive"
	case len(slices.Compact(slices.Sorted(slices.Values(starts)))) > 1 || len(slices.Compact(slices.Sorted(slices.Values(ends)))) > 1,
		coarse > 0 && coarse < len(starts):
		return "the bounds: inconclusive, since the rows disagree" + finer
	case coarse > 0:
		return "a window's start is " + starts[0] + " and its end " + ends[0] + " by whole seconds, since Schwab does not keep a bound's milliseconds" + finer
	}
	return "a window's start is " + starts[0] + " and its end " + ends[0] + finer
}

// extrasFilterOn sends two windows around row, whose time and tradeDate lie
// apart, through held: one that holds its time and not its tradeDate, and
// one the other way, and returns which a window takes a row by, adding a
// line for them.
func (r *liveRun) extrasFilterOn(row *Transaction, held func(url.Values, *Transaction) (bool, int, string, bool), add func(string, ...any)) string {
	if row == nil {
		add("by time or tradeDate: no row of the history has its time and tradeDate two hours apart")
		return "by time or tradeDate: inconclusive, with no row to ask about"
	}
	t, td := row.Time, row.TradeDate
	byTime, byTrade := window{td.Add(time.Hour), t.Add(time.Hour)}, window{td.Add(-time.Hour), t.Add(-time.Hour)}
	if t.Before(td) {
		byTime, byTrade = window{t.Add(-time.Hour), td.Add(-time.Hour)}, window{t.Add(time.Hour), td.Add(time.Hour)}
	}
	inTime, _, saidTime, okTime := held(rawWindow(byTime, true), row)
	inTrade, _, saidTrade, okTrade := held(rawWindow(byTrade, true), row)
	kind := "a row"
	if row.Description == "Free Balance Interest Adjustment" {
		kind = "a month-end interest row"
	}
	switch {
	case !okTime || !okTrade:
		add("by time or tradeDate, of %s: a window was refused: %s; %s", kind, saidTime, saidTrade)
		return "by time or tradeDate: inconclusive"
	case inTime && !inTrade:
		add("by time or tradeDate, of %s: the window around its time holds it, and the window around its tradeDate does not", kind)
		return "a window takes a row by its time"
	case inTrade && !inTime:
		add("by time or tradeDate, of %s: the window around its tradeDate holds it, and the window around its time does not", kind)
		return "a window takes a row by its tradeDate"
	}
	add("by time or tradeDate, of %s: the window around its time %s it, and the window around its tradeDate %s it", kind,
		map[bool]string{true: "holds", false: "does not hold"}[inTime], map[bool]string{true: "holds", false: "does not hold"}[inTrade])
	return "by time or tradeDate: inconclusive"
}

// boundsWords returns the words of the windows' bounds as the report names
// them, for a test.
func boundsWords(b boundsSeen) string {
	start, end, coarse, hidden, ok := b.decide()
	switch {
	case hidden:
		return "hidden, or rounded " + start + " " + end
	case coarse && !ok:
		return "by whole seconds, none"
	case !ok:
		return "none"
	case coarse:
		return "by whole seconds " + start + " " + end
	}
	return strings.Join([]string{start, end}, " ")
}

// The four windows around a row's time t decide the bounds: [t.001, t+1s]
// holding the row and [t-1s, t.000] too shows that Schwab does not keep a
// bound's milliseconds, both bounds being inclusive by whole seconds,
// since no time lies in both; and so does [t.001, t+1s] holding it while
// [t-1s, t.999] does not, which then settles neither bound. [t.001, t+1s]
// holding it otherwise shows a finer time than the row shows, which
// settles neither, or bounds rounded to whole seconds, whose start is then
// inclusive and whose end exclusive; else [t-1s, t.999] must hold it,
// [t-1s, t.000] holding it too makes the end inclusive, and [t.000, t+1s]
// holding it the start. Windows that miss the row where they must hold it
// settle nothing, nor do windows that hold it while a window that takes in
// all they do does not.
func TestBoundsDecide(t *testing.T) {
	for want, b := range map[string]boundsSeen{
		"by whole seconds inclusive inclusive":   {true, true, true, true},
		"by whole seconds, none":                 {false, false, true, true},
		"hidden, or rounded inclusive exclusive": {false, true, true, true},
		"inclusive inclusive":                    {true, true, true, false},
		"exclusive exclusive":                    {false, true, false, false},
	} {
		if got := boundsWords(b); got != want {
			t.Errorf("windows holding the row %v: %q, want %q", b, got, want)
		}
	}
	for i := range 16 {
		b := boundsSeen{i&1 != 0, i&2 != 0, i&4 != 0, i&8 != 0}
		var want string
		switch {
		case b[0] && !b[1], b[3] && !b[2]:
			want = "none"
		case b[0] && b[3]:
			want = "by whole seconds inclusive inclusive"
		case b[3] && !b[1]:
			want = "by whole seconds, none"
		case b[3]:
			want = "hidden, or rounded inclusive exclusive"
		case !b[1]:
			want = "none"
		default:
			want = map[bool]string{true: boundIn, false: boundOut}[b[2]] + " " + map[bool]string{true: boundIn, false: boundOut}[b[0]]
		}
		if got := boundsWords(b); got != want {
			t.Errorf("windows holding the row %v: %q, want %q", b, got, want)
		}
	}
	at := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	ms := time.Millisecond
	if ws := boundsWindows(at); ws != [4]window{{at.Add(-time.Second), at}, {at.Add(-time.Second), at.Add(999 * ms)}, {at, at.Add(time.Second)}, {at.Add(ms), at.Add(time.Second)}} {
		t.Errorf("boundsWindows = %v", ws)
	}
}

// The extras ask about one or two rows whose time no other row shares
// within two seconds and that show no fraction, the newest first; a row
// whose time and tradeDate lie two hours or more apart, a month-end
// interest row before any other; and the newest row with an id, by its id.
func TestPickExtras(t *testing.T) {
	day := func(d, h int) time.Time { return time.Date(2024, 4, d, h, 0, 0, 0, time.UTC) }
	rows := []Transaction{
		{ActivityID: 1, Time: day(1, 10), TradeDate: day(1, 10)},
		{ActivityID: 2, Time: day(2, 10), TradeDate: day(2, 10)},
		{ActivityID: 3, Time: day(2, 10).Add(time.Second), TradeDate: day(2, 4)},
		{ActivityID: 4, Time: day(3, 10).Add(time.Millisecond), TradeDate: day(3, 4)},
		{ActivityID: 5, Time: day(1, 1), TradeDate: day(30, 4).AddDate(0, -1, 0), Description: "Free Balance Interest Adjustment"},
		{Time: day(4, 10)},
	}
	x := pickExtras(rows)
	ids := func(rs []Transaction) []int64 {
		var ids []int64
		for _, r := range rs {
			ids = append(ids, r.ActivityID)
		}
		return ids
	}
	if !slices.Equal(ids(x.bounds), []int64{1, 5}) || x.monthEnd == nil || x.monthEnd.ActivityID != 5 || x.byID == nil || x.byID.ActivityID != 4 {
		t.Errorf("pickExtras: bounds %v, month end %+v, by id %+v", ids(x.bounds), x.monthEnd, x.byID)
	}
	if x := pickExtras(rows[:2]); x.monthEnd != nil || len(x.bounds) != 2 {
		t.Errorf("pickExtras without a row apart from its tradeDate: %+v", x)
	}
}
