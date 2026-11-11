package schwab

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// liveProbes are the probes that TestLive runs, each in its mode: the
// default run's, in an order in which each finds what it needs, and the
// opt-in modes' probes, each of which runs alone. The hashes come first,
// from the account numbers the run has read; the history walk next, since
// the probes after it read its rows; the refresh late, so that the access
// token it starts from is likely over a minute old; and the scope last, to
// see every read of the run.
var liveProbes = []liveProbe{
	{name: "Q10", mode: modeDefault, run: probeHashes},
	{name: "Q1", mode: modeDefault, run: probeDepth},
	{name: "Q2", mode: modeDefault, run: probeEveningFills},
	{name: "Q4", mode: modeDefault, run: probeSymbolFilter},
	{name: "Q5", mode: modeDefault, run: probeOrdersDepth},
	{name: "Q11", mode: modeDefault, run: probeShapes},
	{name: "Q12", mode: modeDefault, run: probeChanges},
	{name: "extras", mode: modeDefault, run: probeExtras},
	{name: "Q8", mode: modeDefault, run: probeAdjustment},
	{name: "Q15", mode: modeDefault, run: probeRefresh},
	{name: "Q6", mode: modeDefault, run: probeScope},
	{name: "Q3", mode: modeBalances, run: probeBalances},
	{name: "Q13", mode: modeFills, run: probeFills},
	{name: "Q14", mode: modeRate, run: probeRate, ready: rateReady},
}

// oneDay is a day of 24 hours, as the API's limits count them.
const oneDay = 24 * time.Hour

// A window is a span of time that a request asks for, its bounds
// included.
type window struct{ start, end time.Time }

// inside reports whether t lies in w.
func (w window) inside(t time.Time) bool { return !t.Before(w.start) && !t.After(w.end) }

// txValues returns the query of a request for every type of transaction
// in w, of symbol's alone when it is set, as the Client's Transactions
// sends it. A window that Transactions would refuse is a mistake of the
// probe's, so it panics, which the probe's recover reports.
func txValues(w window, symbol string) url.Values {
	q, err := TransactionQuery{Start: w.start, End: w.end, Symbol: symbol}.values()
	if err != nil {
		panic(err)
	}
	return q
}

// label returns how the report names a: by its place in the account
// numbers' answer, as {account 1}.
func (a liveAccount) label() string { return "{account " + strconv.Itoa(a.n) + "}" }

// path returns the path of a's endpoints, with its hash escaped, as the
// Client's methods build it.
func (a liveAccount) path() string { return "/trader/v1/accounts/" + url.PathEscape(a.hash) }

// inconclusive returns the verdict of a probe that cannot answer, and why.
func inconclusive(why string, evidence ...reportLine) verdict {
	return verdict{answer: "inconclusive: " + why, evidence: evidence}
}

// lines returns texts as lines of the report.
func lines(texts ...string) []reportLine {
	ls := make([]reportLine, len(texts))
	for i, t := range texts {
		ls[i] = reportLine{text: t}
	}
	return ls
}

// addHistory adds rows, an answer of the nth account's transactions, to
// the history rows, each once: by its activityId, or by its bytes for a
// row without one.
func (r *liveRun) addHistory(n int, rows []Transaction) {
	if r.history == nil {
		r.history = map[int][]Transaction{}
	}
	have := r.history[n]
	for _, row := range rows {
		if !slices.ContainsFunc(have, func(h Transaction) bool { return sameRow(h, row) }) {
			have = append(have, row)
		}
	}
	r.history[n] = have
}

// sameRow reports whether a and b are one row: the same activityId, or,
// without one, the same bytes.
func sameRow(a, b Transaction) bool {
	if a.ActivityID != 0 || b.ActivityID != 0 {
		return a.ActivityID == b.ActivityID
	}
	return bytes.Equal(a.Raw, b.Raw)
}

// oldestOf returns the time of the oldest of rows, leaving out those whose
// time could not be read, and whether there is one.
func oldestOf(rows []Transaction) (time.Time, bool) {
	var oldest time.Time
	for _, row := range rows {
		if !row.Time.IsZero() && (oldest.IsZero() || row.Time.Before(oldest)) {
			oldest = row.Time
		}
	}
	return oldest, !oldest.IsZero()
}

// cutShort reports whether rows, a window's answer, may have been cut at
// the cap of pageCap: they number pageCap or more in all. That takes in an
// answer capped type by type, which holds pageCap of one type, and can hold
// more than pageCap in all.
func cutShort(rows []Transaction, pageCap int) bool {
	return len(rows) >= pageCap
}

// schwabSays returns what err, a read's failure, says, for the report:
// Schwab's status and words, a message of its wording as it is and any
// other text marked as Schwab's, which the report's scrubber cleans of the
// run's identifiers, nicknames and symbols; or the kind of failure, for one
// that is not Schwab's answer.
func schwabSays(err error) string {
	se, ok := errors.AsType[*Error](err)
	switch {
	case ok && se.says() != "":
		return fmt.Sprintf("HTTP %d, %s", se.Status, errorSays(se))
	case ok:
		return fmt.Sprintf("HTTP %d", se.Status)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "the run's stop"
	}
	return "a failure of the network, or an answer that could not be read"
}

// nyDate returns t's date in New York, as the report gives a row's.
func nyDate(t time.Time) string { return string(DateInNewYork(t)) }

// The history walk's bounds: how far back it goes, how many requests one
// account's walk may send, and how many empty windows in a row, older than
// the oldest row, end it.
var walkFloor = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	walkAllowance = 30
	walkEmpties   = 3
)

// walkControl is a window older than any account's history, which each walk
// that ends asks for too: what an old window gets, whatever an account's
// history.
var walkControl = window{walkFloor, time.Date(2000, 12, 31, 0, 0, 0, 0, time.UTC)}

// How an account's history walk ended.
const (
	walkEnded       = "ended"       // three empty windows in a row past the oldest row, or walkFloor
	walkRefused     = "refused"     // a refusal that counts as Schwab's answer to an old window
	walkInterrupted = "interrupted" // an answer that says nothing of a window's age, or the run's end
	walkSpent       = "spent"       // the allowance ran out first
)

// A walkResult is how one account's history walk went.
type walkResult struct {
	how      string
	sent     int       // the requests it sent, against walkAllowance
	oldest   time.Time // the oldest row's time; zero for none
	empty    int       // the empty windows in a row past the oldest row, when it ended
	narrowed int       // how often the oldest window that held rows was narrowed and asked again
	refusal  string    // what Schwab said, when a refusal counted
	why      string    // why it was interrupted
	control  string    // what walkControl got, when it was asked
}

// passedHistory reports whether res's walk went past its account's
// history: three empty windows, or one or two before walkFloor, older than
// its oldest row. A walk that found no row back to walkFloor did not:
// Schwab answers [] to every window of an account whose history it does
// not serve, as one not enabled for thinkorswim, so that says nothing of
// where a history ends.
func (res walkResult) passedHistory() bool {
	return res.how == walkEnded && res.empty > 0 && !res.oldest.IsZero()
}

// A heldRows is a window that held rows, with the oldest of them, and
// whether its answer may have been cut at the cap.
type heldRows struct {
	w      window
	oldest time.Time
	cut    bool
}

// probeDepth is Q1, how far back transactions go and what an older window
// gets. For each account it walks back from now in windows of exactly
// MaxTransactionWindow, every type, a second apart, until three empty
// windows in a row older than the oldest row, walkFloor, or a refusal that
// counts; an account without rows walks back to walkFloor, an empty window
// not showing that a history has ended. A refusal counts as an old window's
// answer only when it is not a 429, a 401, Invalid account number or the
// edge's page, and the account's newest window, asked again at once,
// answers 200; else the walk ends interrupted, as after a 5xx, with no
// verdict for that account. A walk that found no row is evidence alone: an
// old window's answer to an account without rows, [] or a refusal, says
// nothing of where a history ends. The oldest window that held rows, and
// only it, is split when its answer may have been cut at the cap: its end
// is narrowed to the oldest row returned until fewer come back, and an
// oldest row from before the window's start interrupts the walk. Each
// account's walk sends at most walkAllowance requests, walkControl among
// them. The verdict gives each account's oldest row's New York date, "or
// earlier" when the walk ended before the history did. The rows it reads
// are the history rows that later probes read. Whether older activity
// exists that Schwab does not serve stays the owner's to judge.
func probeDepth(ctx context.Context, r *liveRun) verdict {
	var v verdict
	sent := false
	var ended, refused []string
	var first string // the first counted refusal's words
	var oldest []string
	for _, a := range r.accounts {
		if a.hash == "" {
			continue
		}
		res := r.walk(ctx, a, &sent)
		v.evidence = append(v.evidence, reportLine{text: res.line(a)})
		switch {
		case res.passedHistory():
			ended = append(ended, a.label())
		case res.how == walkRefused && !res.oldest.IsZero():
			refused = append(refused, a.label())
			first = cmp.Or(first, res.refusal)
		}
		if !res.oldest.IsZero() && res.how != walkInterrupted {
			date := nyDate(res.oldest)
			if res.how != walkRefused && !res.passedHistory() {
				date += " or earlier" // the walk ended before the account's history did
			}
			oldest = append(oldest, a.label()+" "+date)
		}
	}
	v.evidence = append(v.evidence, reportLine{text: "whether older activity exists that Schwab does not serve is the owner's to judge: " +
		"compare each account's oldest row with when it opened, when it was enabled for thinkorswim, and schwab.com's History"})
	switch {
	case len(ended)+len(refused) == 0:
		v.answer = "inconclusive: no account with rows walked past its history"
		return v
	case len(refused) == 0:
		v.answer = "a window older than an account's history answers 200 with []"
	case len(ended) == 0:
		v.answer = "a window older than an account's history is refused: " + first
	default:
		v.answer = fmt.Sprintf("a window older than an account's history answers 200 with [] for %s, and is refused for %s: %s",
			strings.Join(ended, ", "), strings.Join(refused, ", "), first)
	}
	if len(oldest) > 0 {
		v.answer += "; the oldest rows, by New York date: " + strings.Join(oldest, ", ")
	}
	return v
}

// walk walks a's history, as probeDepth says, waiting a second before each
// request once sent says that one has gone.
func (r *liveRun) walk(ctx context.Context, a liveAccount, sent *bool) walkResult {
	endpoint, path := a.endpoint("/transactions"), a.path()+"/transactions"
	var res walkResult
	ask := func(w window) ([]Transaction, error) {
		if *sent {
			if err := r.wait(ctx, time.Second); err != nil {
				return nil, err
			}
		}
		*sent = true
		res.sent++
		rep, err := r.ask(ctx, endpoint, path, txValues(w, ""), "transactions")
		if err != nil {
			return nil, err
		}
		rows, err := decodeList[Transaction](rep.body)
		if err != nil {
			return nil, err
		}
		r.addHistory(a.n, rows)
		return rows, nil
	}
	end := r.now()
	newest := window{end.Add(-MaxTransactionWindow), end}
	var held *heldRows
	for i, w := 0, newest; ; i, w = i+1, (window{w.start.Add(-MaxTransactionWindow), w.start}) {
		if res.sent >= walkAllowance {
			res.how = walkSpent
			return res
		}
		rows, err := ask(w)
		if err != nil {
			r.walkFailed(ctx, err, &res, i == 0, func() error { _, err := ask(newest); return err })
			if res.how == walkRefused {
				break
			}
			return res
		}
		if len(rows) > 0 {
			res.empty = 0
			oldest, ok := oldestOf(rows)
			if ok && (res.oldest.IsZero() || oldest.Before(res.oldest)) {
				res.oldest = oldest
			}
			held = &heldRows{w: w, oldest: oldest, cut: cutShort(rows, r.pageCap)}
		} else if !res.oldest.IsZero() {
			if res.empty++; res.empty == walkEmpties {
				res.how = walkEnded
				break
			}
		}
		if !w.start.After(walkFloor) {
			res.how = walkEnded
			break
		}
	}
	// Only the oldest window that held rows is split: the windows after it
	// came back empty, or were refused, so its rows are the oldest there
	// are.
	for held != nil && held.cut {
		next := window{held.w.start, held.oldest}
		switch {
		case res.sent >= walkAllowance:
			res.how = walkSpent
			return res
		case held.oldest.IsZero() || !next.end.Before(held.w.end):
			res.how, res.why = walkInterrupted, "an oldest window that held as many rows as the cap, none older than another"
			return res
		case !next.end.After(next.start):
			// A window that takes rows by another time than theirs, such as
			// their tradeDate, can give one from before its start, and none
			// can be narrowed to end there, nor to end at its start.
			res.how, res.why = walkInterrupted, "an oldest window that held as many rows as the cap, the oldest of them at or before its start"
			return res
		}
		rows, err := ask(next)
		if err != nil {
			res.how, res.why = walkInterrupted, "a failure of a narrowed window: "+schwabSays(err)
			return res
		}
		res.narrowed++
		oldest, ok := oldestOf(rows)
		if !ok {
			break
		}
		if oldest.Before(res.oldest) {
			res.oldest = oldest
		}
		held = &heldRows{w: next, oldest: oldest, cut: cutShort(rows, r.pageCap)}
	}
	if res.sent >= walkAllowance {
		return res
	}
	rows, err := ask(walkControl)
	switch {
	case err != nil:
		res.control = "refused, " + schwabSays(err)
	case len(rows) == 0:
		res.control = "200 with []"
	default:
		res.control = "200 with " + plural(len(rows), "row")
	}
	return res
}

// walkFailed sets res for a window that failed with err, the newest window
// when newest is set: a refusal that counts, when againNewest, the
// account's newest window asked again at once, answers 200, and otherwise
// an interruption. A refusal of the newest window itself never counts,
// since that window is no older than any.
func (r *liveRun) walkFailed(ctx context.Context, err error, res *walkResult, newest bool, againNewest func() error) {
	res.how = walkInterrupted
	se, ok := errors.AsType[*Error](err)
	switch {
	case ctx.Err() != nil:
		res.why = "the run's stop"
	case !ok:
		res.why = "a failure of the network, or an answer that is not a list"
	case se.Status == http.StatusTooManyRequests:
		res.why = "a 429"
	case se.Status == http.StatusUnauthorized:
		res.why = "a 401"
	case errors.Is(se, ErrInvalidAccount):
		res.why = "Invalid account number"
	case se.Reference != "":
		res.why = "the edge's Access Denied page"
	case se.Status >= http.StatusInternalServerError:
		res.why = "a " + strconv.Itoa(se.Status) + " after the Client's tries"
	case se.Status < http.StatusBadRequest:
		res.why = "a " + strconv.Itoa(se.Status)
	case newest:
		res.why = "a refusal of the newest window"
	case res.sent >= walkAllowance:
		res.how = walkSpent
	default:
		// A 4xx, a JSON 403 among them, which Schwab could answer an old
		// window with: it counts if the newest window still answers.
		if err := againNewest(); err != nil {
			res.why = "a refusal, after which the newest window, asked again at once, did not answer 200"
			return
		}
		res.how, res.refusal = walkRefused, schwabSays(err)
	}
}

// line returns the report's line for res, a's walk.
func (res walkResult) line(a liveAccount) string {
	var s string
	date := func(t time.Time) string { return nyDate(t) + ", New York time" }
	switch {
	case res.how == walkInterrupted:
		s = fmt.Sprintf("%s: the walk was interrupted after %s by %s, so it settles nothing", a.label(), plural(res.sent, "request"), res.why)
	case res.how == walkSpent:
		s = fmt.Sprintf("%s: the walk spent its allowance of %d requests before it ended", a.label(), walkAllowance)
		if !res.oldest.IsZero() {
			s += "; the oldest row it saw is of " + date(res.oldest) + ", and older ones may be left"
		}
	case res.how == walkRefused && res.oldest.IsZero():
		s = fmt.Sprintf("%s: no row in the windows it read; then a window was refused, %s, and the newest window, asked again at once, answered 200",
			a.label(), res.refusal)
	case res.how == walkRefused:
		s = fmt.Sprintf("%s: the oldest row is of %s; an older window was refused, %s, and the newest window, asked again at once, answered 200",
			a.label(), date(res.oldest), res.refusal)
	case res.oldest.IsZero():
		s = fmt.Sprintf("%s: no row back to %s, where the walk stops; every window answered 200 with []", a.label(), walkFloor.Format(time.DateOnly))
	case res.empty == 0:
		s = fmt.Sprintf("%s: rows reach back to %s, where the walk stops; the oldest is of %s", a.label(), walkFloor.Format(time.DateOnly), date(res.oldest))
	default:
		s = fmt.Sprintf("%s: the oldest row is of %s; the %s before it answered 200 with []", a.label(), date(res.oldest), plural(res.empty, "window"))
	}
	if res.narrowed > 0 {
		s += "; the oldest window that held rows may have been cut at the cap, so it was narrowed and asked again " + plural(res.narrowed, "time")
	}
	if res.control != "" {
		s += "; the window of 2000: " + res.control
	}
	return s
}

// The evening and overnight fills' classes, by what their tradeDate is,
// and the hours that bound the two spans, in New York.
const (
	fillInstant  = "the fill's instant"
	fillNext     = "the next midnight"
	fillSame     = "the same day's midnight"
	fillOther    = "another time"
	eveningFrom  = 18 // the evening session's first hour
	overnightTil = 4  // and the hour the overnight session ends
)

// A fillTally counts fills by what their tradeDate is.
type fillTally struct{ total, instant, next, same, other int }

// add counts a fill at t, New York time, with tradeDate.
func (f *fillTally) add(t, tradeDate time.Time) {
	f.total++
	switch {
	case tradeDate.Equal(t):
		f.instant++
	case tradeDate.Equal(time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())):
		f.next++
	case tradeDate.Equal(time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())):
		f.same++
	default:
		f.other++
	}
}

// says says what the fills' tradeDate is: one class, or that it differs,
// or that there was no fill.
func (f fillTally) says() string {
	if f.total == 0 {
		return "no fill seen"
	}
	for _, c := range []struct {
		n    int
		word string
	}{{f.instant, fillInstant}, {f.next, fillNext}, {f.same, fillSame}, {f.other, fillOther}} {
		if c.n == f.total {
			return "tradeDate is " + c.word
		}
	}
	return "tradeDate differs from fill to fill"
}

// counts returns the line of the report for f, the fills of a span.
func (f fillTally) counts(span string) string {
	return fmt.Sprintf("fills %s, New York time: %d; tradeDate %s: %d, %s: %d, %s: %d, %s: %d",
		span, f.total, fillInstant, f.instant, fillNext, f.next, fillSame, f.same, fillOther, f.other)
}

// probeEveningFills is Q2, the tradeDate of a fill between 18:00 and 04:00
// in New York, in the futures and overnight sessions: of the history rows'
// executions, TRADE rows with an orderId, at 18:00-23:59 or 00:00-03:59 New
// York time, it counts those whose tradeDate is the instant, the next
// midnight, the same day's midnight or another time. It sends nothing.
func probeEveningFills(_ context.Context, r *liveRun) verdict {
	ny := newYork()
	var evening, overnight fillTally
	for _, a := range r.accounts {
		for _, row := range r.history[a.n] {
			if row.Type != TransactionTrade || row.OrderID == 0 || row.Time.IsZero() {
				continue
			}
			switch t := row.Time.In(ny); {
			case t.Hour() >= eveningFrom:
				evening.add(t, row.TradeDate)
			case t.Hour() < overnightTil:
				overnight.add(t, row.TradeDate)
			}
		}
	}
	ev := lines(evening.counts("from 18:00 to midnight"), overnight.counts("from midnight to 04:00"))
	if evening.total+overnight.total == 0 {
		return inconclusive("no execution of the history rows lies between 18:00 and 04:00 New York time", ev...)
	}
	return verdict{answer: "from 18:00 to midnight, " + evening.says() + "; from midnight to 04:00, " + overnight.says(), evidence: ev}
}

// legSymbols returns the symbols that row's legs name, as an instrument's
// symbol or its underlying's, each once, with whether each is an
// instrument's own.
func legSymbols(row Transaction) map[string]bool {
	syms := map[string]bool{}
	for _, leg := range row.TransferItems {
		if s := leg.Instrument.UnderlyingSymbol; s != "" && !syms[s] {
			syms[s] = false
		}
		if s := leg.Instrument.Symbol; s != "" {
			syms[s] = true
		}
	}
	return syms
}

// describedSymbol returns the symbol after the last "~" of row's
// description, which names a dividend's or a withholding's security, and
// whether there is one.
func describedSymbol(row Transaction) (string, bool) {
	i := strings.LastIndex(row.Description, "~")
	if i < 0 || i+1 == len(row.Description) {
		return "", false
	}
	return row.Description[i+1:], true
}

// namesNeither reports whether row names sym neither in a leg nor after a
// "~" in its description.
func namesNeither(row Transaction, sym string) bool {
	_, inLeg := legSymbols(row)[sym]
	described, ok := describedSymbol(row)
	return !inLeg && (!ok || described != sym)
}

// isW8 reports whether row is a W-8 withholding journal.
func isW8(row Transaction) bool {
	return row.Type == TransactionJournal && strings.HasPrefix(row.Description, "W-8")
}

// A filterPlan is how Q4 asks of one account: a symbol of its history
// rows, a TRADE that names it in a leg, the dividend and the W-8 journal
// that name it after a "~" nearest that TRADE, each if there is one, and
// the windows to send with symbol= and without.
type filterPlan struct {
	symbol       string
	trade        Transaction
	dividend, w8 *Transaction
	windows      []window
	widened      bool // whether a window was widened to take in a row of another symbol
}

// filterPad is how far a Q4 window reaches past the rows it is for: a day,
// so that a window taken by tradeDate, which is midnight in New York on
// most rows, or a time finer than a row shows, still takes the row in.
const filterPad = oneDay

// planFilter returns the plan of Q4 for rows, an account's history rows, or
// why there is none. A symbol qualifies with a TRADE that names it in a leg
// and a dividend or W-8 journal that names it after a "~", if hidden says
// that the report's scrubber hides it: Schwab's refusal of symbol= may
// quote the symbol, and a benchmark's or one of a single character is not
// hidden. The one chosen has both a dividend and a W-8 journal if any does,
// then the TRADE and the row nearest each other, preferring a TRADE that
// names it as its own symbol, not its underlying's. The rows go in one
// window if one can hold them, else one window each; and when no window
// holds a row that names neither the symbol in a leg nor after a "~", the
// window nearest such a row of the history is widened, within
// MaxTransactionWindow, to take it in.
func planFilter(rows []Transaction, hidden func(symbol string) bool) (*filterPlan, string) {
	type candidate struct {
		symbol     string
		own        []Transaction // TRADEs naming it as their own symbol
		underlying []Transaction // and those naming it as their underlying's
		dividends  []Transaction
		w8s        []Transaction
	}
	by := map[string]*candidate{}
	get := func(s string) *candidate {
		if by[s] == nil {
			by[s] = &candidate{symbol: s}
		}
		return by[s]
	}
	for _, row := range rows {
		if row.ActivityID == 0 || row.Time.IsZero() {
			continue // a row that no answer can be matched to
		}
		if row.Type == TransactionTrade {
			for s, own := range legSymbols(row) {
				if own {
					get(s).own = append(get(s).own, row)
				} else {
					get(s).underlying = append(get(s).underlying, row)
				}
			}
		}
		if s, ok := describedSymbol(row); ok {
			switch {
			case row.Type == TransactionDividendOrInterest:
				get(s).dividends = append(get(s).dividends, row)
			case isW8(row):
				get(s).w8s = append(get(s).w8s, row)
			}
		}
	}
	nearest := func(to Transaction, rs []Transaction) *Transaction {
		var best *Transaction
		for i := range rs {
			if best == nil || absDur(rs[i].Time.Sub(to.Time)) < absDur(best.Time.Sub(to.Time)) {
				best = &rs[i]
			}
		}
		return best
	}
	var plans []*filterPlan
	unhidden := false // whether a symbol qualified but for the scrubber
	for _, c := range by {
		trades := c.own
		if len(trades) == 0 {
			trades = c.underlying
		}
		described := slices.Concat(c.dividends, c.w8s)
		if len(trades) == 0 || len(described) == 0 {
			continue
		}
		if !hidden(c.symbol) {
			unhidden = true
			continue
		}
		var best *filterPlan
		var gap time.Duration
		for _, t := range trades {
			d := nearest(t, described)
			if g := absDur(d.Time.Sub(t.Time)); best == nil || g < gap {
				best, gap = &filterPlan{symbol: c.symbol, trade: t}, g
			}
		}
		best.dividend, best.w8 = nearest(best.trade, c.dividends), nearest(best.trade, c.w8s)
		plans = append(plans, best)
	}
	switch {
	case len(plans) == 0 && unhidden:
		return nil, "no symbol of the history rows that the report's scrubber hides has both a TRADE and a dividend or a W-8 journal"
	case len(plans) == 0:
		return nil, "no symbol of the history rows has both a TRADE and a dividend or a W-8 journal"
	}
	gapOf := func(p *filterPlan) time.Duration {
		g := time.Duration(math.MaxInt64)
		for _, d := range []*Transaction{p.dividend, p.w8} {
			if d != nil {
				g = min(g, absDur(d.Time.Sub(p.trade.Time)))
			}
		}
		return g
	}
	slices.SortFunc(plans, func(a, b *filterPlan) int {
		bothA, bothB := a.dividend != nil && a.w8 != nil, b.dividend != nil && b.w8 != nil
		if bothA != bothB {
			if bothA {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(gapOf(a), gapOf(b)), strings.Compare(a.symbol, b.symbol))
	})
	p := plans[0]
	held := []Transaction{p.trade}
	for _, d := range []*Transaction{p.dividend, p.w8} {
		if d != nil {
			held = append(held, *d)
		}
	}
	first, last := held[0].Time, held[0].Time
	for _, h := range held {
		first, last = minTime(first, h.Time), maxTime(last, h.Time)
	}
	if last.Sub(first)+2*filterPad <= MaxTransactionWindow {
		p.windows = []window{{first.Add(-filterPad), last.Add(filterPad)}}
	} else {
		for _, h := range held {
			p.windows = append(p.windows, window{h.Time.Add(-filterPad), h.Time.Add(filterPad)})
		}
	}
	// A row of another symbol in a window shows that the filter filters.
	type widening struct {
		i     int
		w     window
		reach time.Duration
	}
	var best *widening
	for _, row := range rows {
		if row.ActivityID == 0 || row.Time.IsZero() || !namesNeither(row, p.symbol) {
			continue
		}
		for i, w := range p.windows {
			if w.inside(row.Time) {
				return p, ""
			}
			wider := window{minTime(w.start, row.Time.Add(-filterPad)), maxTime(w.end, row.Time.Add(filterPad))}
			reach := wider.end.Sub(wider.start) - w.end.Sub(w.start)
			if wider.end.Sub(wider.start) <= MaxTransactionWindow && (best == nil || reach < best.reach) {
				best = &widening{i, wider, reach}
			}
		}
	}
	if best == nil {
		return nil, "no row of the history that names another symbol lies near enough to the chosen symbol's rows"
	}
	p.windows[best.i], p.widened = best.w, true
	return p, ""
}

// absDur returns the length of d, whichever way it runs.
func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// minTime returns the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// A filterResult is what Q4 found for one account: whether a verdict holds,
// and if not why, and what the filter did with a dividend and a W-8
// journal: kept, dropped, or "" when none was in a control answer.
type filterResult struct {
	ok           bool
	why          string
	dividend, w8 string
}

// The words of a filterResult for a row that names its symbol only after a
// "~".
const (
	filterKept    = "kept"
	filterDropped = "dropped"
)

// runFilter sends p's windows for account a, without symbol= and with it,
// and decides as Q4 says: a verdict needs the TRADE of the symbol back
// filtered, and a row of a control answer that names neither the symbol in
// a leg nor after a "~" left out filtered; then a dividend or W-8 journal
// in a control answer was kept if it came back filtered, and dropped if
// not.
func (r *liveRun) runFilter(ctx context.Context, a liveAccount, p *filterPlan) filterResult {
	endpoint, path := a.endpoint("/transactions"), a.path()+"/transactions"
	ids := func(q url.Values) (map[int64]Transaction, error) {
		rep, err := r.read(ctx, endpoint, path, q, "transactions")
		if err != nil {
			return nil, err
		}
		rows, err := decodeList[Transaction](rep.body)
		if err != nil {
			return nil, err
		}
		got := map[int64]Transaction{}
		for _, row := range rows {
			if row.ActivityID != 0 {
				got[row.ActivityID] = row
			}
		}
		return got, nil
	}
	var res filterResult
	controls, filtered := map[int64]Transaction{}, map[int64]Transaction{}
	other, otherLeftOut := false, false
	for _, w := range p.windows {
		control, err := ids(txValues(w, ""))
		if err != nil {
			res.why = "a control without symbol= failed: " + schwabSays(err)
			return res
		}
		with, err := ids(txValues(w, p.symbol))
		if err != nil {
			res.why = "a request with symbol= failed: " + schwabSays(err)
			return res
		}
		for id, row := range control {
			controls[id] = row
			if namesNeither(row, p.symbol) {
				_, back := with[id]
				other, otherLeftOut = true, otherLeftOut || !back
			}
		}
		maps.Copy(filtered, with)
	}
	_, tradeAsked := controls[p.trade.ActivityID]
	_, tradeBack := filtered[p.trade.ActivityID]
	switch {
	case !tradeAsked:
		res.why = "no control answer held the symbol's TRADE"
	case !tradeBack:
		res.why = "the filter left out the symbol's own TRADE, as a filter that drops everything would"
	case !other:
		res.why = "no control answer held a row of another symbol"
	case !otherLeftOut:
		res.why = "every row of another symbol came back filtered, as it would if Schwab ignored symbol="
	default:
		res.ok = true
		for _, k := range []struct {
			row  *Transaction
			word *string
		}{{p.dividend, &res.dividend}, {p.w8, &res.w8}} {
			if k.row == nil {
				continue
			}
			if _, ok := controls[k.row.ActivityID]; !ok {
				continue
			}
			*k.word = filterDropped
			if _, ok := filtered[k.row.ActivityID]; ok {
				*k.word = filterKept
			}
		}
	}
	return res
}

// probeSymbolFilter is Q4, whether the transactions' symbol filter keeps
// the rows that name their security only after a "~": dividends and W-8
// journals, each reported apart. For each account it chooses a symbol of
// its history rows, as planFilter says, and sends its windows with
// symbol= and without, as runFilter says; with no symbol to choose, or
// either control failing, the account's answer is inconclusive. It never
// names the symbol, sends only one that the report's scrubber hides, so
// that a refusal quoting it shows a label, and marks each line that
// reveals W-8 rows.
func probeSymbolFilter(ctx context.Context, r *liveRun) verdict {
	var v verdict
	var dividends, w8s []string
	var why string
	for _, a := range r.accounts {
		if a.hash == "" || len(r.history[a.n]) == 0 {
			continue
		}
		p, none := planFilter(r.history[a.n], r.scrub.hides)
		if p == nil {
			v.evidence = append(v.evidence, reportLine{text: a.label() + ": " + none})
			why = cmp.Or(why, none)
			continue
		}
		var kinds []string
		if p.dividend != nil {
			kinds = append(kinds, "a dividend")
		}
		if p.w8 != nil {
			kinds = append(kinds, "a W-8 journal")
		}
		plan := fmt.Sprintf("%s: a symbol with a TRADE and %s, in %s", a.label(), strings.Join(kinds, " and "), plural(len(p.windows), "window"))
		if p.widened {
			plan += ", one widened to take in a row of another symbol"
		}
		v.evidence = append(v.evidence, reportLine{text: plan, w8: p.w8 != nil})
		res := r.runFilter(ctx, a, p)
		if !res.ok {
			v.evidence = append(v.evidence, reportLine{text: a.label() + ": inconclusive: " + res.why})
			why = cmp.Or(why, res.why)
			continue
		}
		v.evidence = append(v.evidence, reportLine{text: a.label() + ": filtered, the symbol's TRADE came back, and a row of another symbol did not"})
		if res.dividend != "" {
			v.evidence = append(v.evidence, reportLine{text: a.label() + ": the dividend was " + res.dividend})
			dividends = append(dividends, res.dividend)
		}
		if res.w8 != "" {
			v.evidence = append(v.evidence, reportLine{text: a.label() + ": the W-8 journal was " + res.w8, w8: true})
			w8s = append(w8s, res.w8)
		}
	}
	keeps := func(results []string, rows string) string {
		switch {
		case len(results) == 0:
			return ""
		case !slices.Contains(results, filterDropped):
			return "keeps " + rows
		case !slices.Contains(results, filterKept):
			return "drops " + rows
		}
		return "keeps " + rows + " for some symbols and drops them for others"
	}
	var parts []string
	for _, p := range []string{keeps(dividends, "dividends"), keeps(w8s, "W-8 journals")} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		v.answer = "inconclusive: " + cmp.Or(why, "no account has history rows")
		return v
	}
	v.answer, v.w8 = "the filter "+strings.Join(parts, "; it "), len(w8s) > 0
	return v
}

// The ages that Q5 goes by. orderAgeLimit is the 60 days that some say the
// orders endpoint keeps to. Q5 counts an order as old only once it was
// entered orderOld ago, a week later, since a list kept for 60 days by
// calendar dates, or from some midnight, can hold one entered a day or two
// before. It asks for two days around an order's entry only when the order
// was entered at most orderOldest ago, so that the two days lie in the
// last year, which a list kept for a year holds. And it says that a list
// is kept for a year only when it holds an order entered orderYear ago or
// more, within a month of a year.
const (
	orderAgeLimit = 60 * oneDay
	orderOld      = orderAgeLimit + 7*oneDay
	orderOldest   = MaxTransactionWindow - oneDay
	orderYear     = MaxTransactionWindow - 30*oneDay
)

// An orderSeen is an order of an answer of Schwab's orders, as the probes
// read it: its id, when it was entered, its status, and when it was
// filled, by its last execution or else its closeTime, if it was.
type orderSeen struct {
	id      string
	entered time.Time
	status  string
	filled  time.Time
}

// readOrder reads raw, one order, leniently: an order without some of what
// orderSeen holds still counts.
func readOrder(raw json.RawMessage) orderSeen {
	var o struct {
		OrderID     json.Number     `json:"orderId"`
		EnteredTime json.RawMessage `json:"enteredTime"`
		CloseTime   json.RawMessage `json:"closeTime"`
		Status      string          `json:"status"`
		Activities  []struct {
			Legs []struct {
				Time json.RawMessage `json:"time"`
			} `json:"executionLegs"`
		} `json:"orderActivityCollection"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&o) != nil {
		return orderSeen{}
	}
	var ps []Problem
	at := func(raw json.RawMessage) time.Time {
		t, _ := readTime(raw, "", &ps)
		return t
	}
	seen := orderSeen{id: o.OrderID.String(), entered: at(o.EnteredTime), status: o.Status}
	for _, act := range o.Activities {
		for _, leg := range act.Legs {
			seen.filled = maxTime(seen.filled, at(leg.Time))
		}
	}
	if seen.filled.IsZero() && o.Status == "FILLED" {
		seen.filled = at(o.CloseTime)
	}
	return seen
}

// readOrders reads body, a list of orders.
func readOrders(body []byte) ([]orderSeen, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(body, &raws); err != nil {
		return nil, err
	}
	orders := make([]orderSeen, len(raws))
	for i, raw := range raws {
		orders[i] = readOrder(raw)
	}
	return orders, nil
}

// ordersValues returns the query of a request for the orders entered in w.
func ordersValues(w window) url.Values {
	return url.Values{"fromEnteredTime": {w.start.UTC().Format(schwabTimeFormat)}, "toEnteredTime": {w.end.UTC().Format(schwabTimeFormat)}}
}

// probeOrdersDepth is Q5, whether an account's orders are served for a year
// or for 60 days, which the list filters by the time an order was entered.
// It takes the youngest execution of the history rows that is over
// orderOld and at most orderOldest old, gets its order by id, whose
// enteredTime it keeps in memory only, since a good-till-cancelled order
// can be entered months before its fill, and, unless that time is over
// orderOldest ago too, asks the account's list for two days around it, for
// the last year and for the last 60 days, and every account's list from 61
// days back. The youngest is the likeliest to have been entered within the
// year. It answers with the bound that the lists show: longer than 60
// days, when an order entered over orderOld ago comes back, and a year
// when one entered orderYear ago or more does; under the age of the order
// that its id finds, when the two days around its entry leave it out, as of
// their answer, rounded up to whole days; from the one to the other when
// both show; and "inconclusive" when the get by id fails, when the answers
// say neither, or when an order at least as old as the one left out came
// back.
func probeOrdersDepth(ctx context.Context, r *liveRun) verdict {
	now := r.now()
	days := func(d time.Duration) string { return plural(int(d/oneDay), "day") }
	var pick *Transaction
	var acct liveAccount
	for _, a := range r.accounts {
		for i, row := range r.history[a.n] {
			age := now.Sub(row.Time)
			if a.hash == "" || row.Type != TransactionTrade || row.OrderID == 0 || row.Time.IsZero() || age <= orderOld || age > orderOldest {
				continue
			}
			if pick == nil || row.Time.After(pick.Time) {
				pick, acct = &r.history[a.n][i], a
			}
		}
	}
	if pick == nil {
		return inconclusive(fmt.Sprintf("no execution of the history rows is over %s and at most %s old", days(orderOld), days(orderOldest)))
	}
	id := strconv.FormatInt(pick.OrderID, 10)
	path := acct.path() + "/orders"
	// Each read asks what Schwab serves of old orders, which it may refuse
	// for their age, so none counts against the login, as ask's do not;
	// each is reported, as get's are.
	get := func(endpoint, path string, query url.Values) (*reply, error) {
		return r.fetch(ctx, r.client, endpoint, path, query, "", false, true)
	}
	rep, err := get(acct.endpoint("/orders/"+idLabel), path+"/"+id, nil)
	if err != nil {
		return inconclusive("the order of an execution " + days(now.Sub(pick.Time)) + " old could not be got by its id: " + schwabSays(err))
	}
	order := readOrder(rep.body)
	switch entered := now.Sub(order.entered); {
	case order.entered.IsZero():
		return inconclusive("the order got by its id gave no time it was entered")
	case entered <= orderOld:
		return inconclusive("the order got by its id was entered within the last " + days(orderOld) + ", though its fill is older")
	case entered > orderOldest:
		// A list kept for a year would leave it out too.
		return inconclusive("the order of an execution " + days(now.Sub(pick.Time)) + " old, got by its id, was entered " + days(entered) +
			" ago, too long ago for two days around its entry to lie in the last year")
	}
	ev := lines(fmt.Sprintf("the order of an execution %s old, got by its id, was entered %s ago", days(now.Sub(pick.Time)), days(now.Sub(order.entered))))
	// list asks for the orders entered in w, and says what came back.
	list := func(endpoint, path string, w window) (orders []orderSeen, read bool, said string) {
		rep, err := get(endpoint, path, ordersValues(w))
		if err != nil {
			return nil, false, "refused, " + schwabSays(err)
		}
		orders, err = readOrders(rep.body)
		if err != nil {
			return nil, false, "200, but not a list of orders"
		}
		return orders, true, "200, " + plural(len(orders), "order")
	}
	around, aroundRead, aroundSaid := list(acct.endpoint("/orders"), path, window{order.entered.Add(-oneDay), order.entered.Add(oneDay)})
	// The list left the order out when it answered, which may be well after
	// the probe began, past the get by id and any waits of the rate limit or
	// tries again: the order was older by then.
	aroundAt := r.now()
	listed := slices.ContainsFunc(around, func(o orderSeen) bool { return o.id == id })
	if aroundRead {
		aroundSaid += map[bool]string{true: ", the order among them", false: ", without the order"}[listed]
	}
	year, yearRead, yearSaid := list(acct.endpoint("/orders"), path, window{now.Add(-MaxTransactionWindow), now})
	// kept is how long ago the oldest order that came back was entered:
	// the order got by its id, among the two days around its entry, or any
	// of the last year's.
	var kept time.Duration
	if listed {
		kept = now.Sub(order.entered)
	}
	yearOld := false
	for _, o := range year {
		if !o.entered.IsZero() {
			kept = max(kept, now.Sub(o.entered))
			yearOld = yearOld || now.Sub(o.entered) > orderOld
		}
	}
	if yearRead {
		yearSaid += map[bool]string{true: ", some", false: ", none"}[yearOld] + " entered more than " + days(orderOld) + " ago"
	}
	_, _, sixtySaid := list(acct.endpoint("/orders"), path, window{now.Add(-orderAgeLimit), now})
	_, _, allSaid := list("orders", "/trader/v1/orders", window{now.Add(-orderAgeLimit - oneDay), now})
	ev = append(ev, lines(
		"two days around the order's entry: "+aroundSaid,
		"the account's orders of the last year: "+yearSaid,
		"the account's orders of the last 60 days: "+sixtySaid,
		"every account's orders from 61 days back: "+allSaid,
	)...)
	// The lists disagree only when an order at least as old as the one
	// left out came back, both ages as of the probe's start; a younger one
	// bounds the list from below. The order left out bounds it from above,
	// by its age when the list left it out, rounded up to whole days, so
	// that the bound holds it: a list kept for 90 days leaves out an order
	// entered 90 days and some hours ago.
	age, upper := now.Sub(order.entered), aroundAt.Sub(order.entered)
	old, no := kept > orderOld, aroundRead && !listed
	entered := days(age)
	switch {
	case no && kept >= age:
		return inconclusive("the lists disagree: an order entered "+days(kept)+" ago came back, while two days around the entry of one entered "+
			entered+" ago left it out", ev...)
	case old && no:
		return verdict{
			answer: fmt.Sprintf("%s: an order entered %s ago came back, and two days around the entry of one entered %s ago leave it out, "+
				"though its id finds it", spanOf(kept, upper), days(kept), entered),
			evidence: ev,
		}
	case kept >= orderYear:
		return verdict{answer: "a year: an order entered " + days(kept) + " ago came back", evidence: ev}
	case old:
		return verdict{answer: "longer than 60 days: an order entered " + days(kept) + " ago came back", evidence: ev}
	case no:
		return verdict{
			answer: "under " + plural(roughly(upper, true)) + ": two days around the entry of an order entered " + entered +
				" ago leave it out, though its id finds it",
			evidence: ev,
		}
	}
	return inconclusive("two days around the order's entry were refused, and no old order came back", ev...)
}

// scopeWord matches a scope that the report may print as it is: a few
// words of letters, digits and _.:/-, as OAuth's scopes are.
var scopeWord = regexp.MustCompile(`^[A-Za-z0-9_.:/ -]{1,40}$`)

// probeScope is Q6, whether a login with scope=readonly gives a token that
// cannot trade: it reports the scope that Schwab said it granted, and
// whether any read of the run's account API that a working login passes
// was refused as unauthorized, as get and read count them, not the reads
// that Schwab may refuse for reasons of its own, which ask sends: those
// whose answer a probe asks about, and Q12's windows, which end days
// ahead. Whether such a token cannot trade stays open, since only an order
// could show it. It sends nothing.
func probeScope(_ context.Context, r *liveRun) verdict {
	st, err := r.auth.Status()
	scope := "no scope"
	switch {
	case err != nil:
		scope = "a scope that could not be read"
	case scopeWord.MatchString(st.Scope):
		scope = strconv.Quote(st.Scope)
	case st.Scope != "":
		scope = "a scope of " + plural(len(st.Scope), "character")
	}
	r.mu.Lock()
	counted, refused := r.counted, r.refused
	r.mu.Unlock()
	worked := "no read of the account API was refused as unauthorized"
	if refused > 0 {
		worked = plural(refused, "read") + " of the account API refused as unauthorized"
	}
	return verdict{
		answer: "the login's scope is " + scope + "; " + worked,
		evidence: lines(
			"reads counted: "+strconv.Itoa(counted)+", those of the account API that a working login passes: the account numbers, "+
				"the preferences, the checks, and Q4's windows; not those that Schwab may refuse for reasons of its own: Q1's walk, "+
				"Q5's old orders and the extras', whose answers the probes ask about, and Q12's windows, which end days ahead",
			"whether a token of scope readonly cannot trade stays open: only an order could show it, and the live run sends none"),
	}
}

// enumWord matches a value of one of Schwab's enums, such as an asset type
// or a fee type, which a report may print: anything else is named only as
// unusual.
var enumWord = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,39}$`)

// enum returns s as the report prints a value of an enum: as it is, or
// "an unusual value", or none.
func enum(s, none string) string {
	switch {
	case s == "":
		return none
	case enumWord.MatchString(s):
		return s
	}
	return "an unusual value"
}

// rowShape returns the shape of row, for Q11: its description, if it is on
// Schwab's wording, and else only that it has other text; per leg the asset
// type, the sign of the amount, whether the cost is zero, the fee type and
// the position effect; the ids it holds; and whether the legs' costs sum to
// its netAmount. w8 says whether the shape reveals a W-8 row.
func rowShape(row Transaction) (shape string, w8 bool) {
	desc := "no description"
	if row.Description != "" {
		desc = "other text"
		if entry, ok := onWording(row.Description); ok {
			desc, w8 = strconv.Quote(entry), strings.HasPrefix(entry, "W-8")
		}
	}
	var legs []string
	sum := 0.0
	for _, leg := range row.TransferItems {
		sign := "0"
		switch {
		case leg.Amount > 0:
			sign = "+"
		case leg.Amount < 0:
			sign = "-"
		}
		cost := "cost 0"
		if leg.Cost != 0 {
			cost = "cost not 0"
		}
		l := fmt.Sprintf("%s, amount %s, %s", enum(string(leg.Instrument.AssetType), "no asset type"), sign, cost)
		if leg.FeeType != "" {
			l += ", fee " + enum(leg.FeeType, "")
		}
		if leg.PositionEffect != "" {
			l += ", " + enum(leg.PositionEffect, "")
		}
		legs = append(legs, "("+l+")")
		sum += leg.Cost
	}
	var ids []string
	for _, id := range []struct {
		name string
		v    int64
	}{{"activityId", row.ActivityID}, {"orderId", row.OrderID}, {"positionId", row.PositionID}} {
		if id.v != 0 {
			ids = append(ids, id.name)
		}
	}
	sums := "no"
	if math.Abs(sum-row.NetAmount) < 0.005 {
		sums = "yes"
	}
	return fmt.Sprintf("%s; legs %s; ids %s; the legs' costs sum to netAmount: %s",
		desc, listOrNone(legs), listOrNone(ids), sums), w8
}

// probeShapes is Q11, how kinds of event show up in transactions: of the
// history rows, it counts each shape of row, as rowShape gives it, per type
// of row. A description off Schwab's wording is counted as other text and
// never printed; with -live.record its text is in the raw folder only. It
// sends nothing.
func probeShapes(_ context.Context, r *liveRun) verdict {
	type shapeCount struct {
		shape string
		w8    bool
		n     int
	}
	byType := map[string][]*shapeCount{}
	rows, other := 0, 0
	for _, a := range r.accounts {
		for _, row := range r.history[a.n] {
			rows++
			typ := enum(string(row.Type), "no type")
			shape, w8 := rowShape(row)
			if strings.HasPrefix(shape, "other text") {
				other++
			}
			i := slices.IndexFunc(byType[typ], func(c *shapeCount) bool { return c.shape == shape })
			if i < 0 {
				byType[typ] = append(byType[typ], &shapeCount{shape: shape, w8: w8})
				i = len(byType[typ]) - 1
			}
			byType[typ][i].n++
		}
	}
	if rows == 0 {
		return inconclusive("there are no history rows")
	}
	types := slices.Collect(maps.Keys(byType))
	rank := func(t string) int {
		if i := slices.Index(AllTransactionTypes(), TransactionType(t)); i >= 0 {
			return i
		}
		return len(AllTransactionTypes())
	}
	slices.SortFunc(types, func(a, b string) int { return cmp.Or(cmp.Compare(rank(a), rank(b)), strings.Compare(a, b)) })
	var v verdict
	shapes := 0
	for _, t := range types {
		n := 0
		for _, c := range byType[t] {
			n += c.n
		}
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("%s: %s, %s", t, plural(n, "row"), plural(len(byType[t]), "shape"))})
		for _, c := range byType[t] {
			v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("  %d: %s", c.n, c.shape), w8: c.w8})
		}
		shapes += len(byType[t])
	}
	if other > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("rows with a description off Schwab's wording, which is never printed: %d; "+
			"with -live.record its text is in the raw folder only", other)})
	}
	v.answer = fmt.Sprintf("%s among %s of %s", plural(shapes, "shape"), plural(rows, "row"), plural(len(types), "type"))
	return v
}

// walkFailed takes a refusal for Schwab's answer to an old window only
// when it says something of the window: not a 429, a 401, Invalid account
// number, the edge's page, a 5xx, a 3xx or a failure of the network, nor a
// refusal of the newest window or one after which the newest window, asked
// again, fails; a JSON 403 counts, as a 400 does, and what Schwab said is
// kept, marked as its own words. With the allowance spent it cannot ask the
// newest window again, and with the run stopped it says so.
func TestWalkFailed(t *testing.T) {
	answer := func(status int, body string) error {
		return newError(status, http.Header{"Content-Type": {"application/json"}}, []byte(body))
	}
	page := newError(http.StatusForbidden, http.Header{}, readTestdata(t, "errors/akamai_403.html"))
	whitelabel := newError(http.StatusInternalServerError, http.Header{}, readTestdata(t, "errors/whitelabel_500.html"))
	refusal := answer(http.StatusBadRequest, `{"message":"the window is too old"}`)
	stopped, stop := context.WithCancel(t.Context())
	stop()
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		err       error
		newest    bool
		sent      int
		again     error // what the newest window, asked again, gets
		how, said string
	}{
		{"a 429", nil, answer(http.StatusTooManyRequests, ""), false, 3, nil, walkInterrupted, "a 429"},
		{"a 401", nil, answer(http.StatusUnauthorized, `{"message":"Unauthorized"}`), false, 3, nil, walkInterrupted, "a 401"},
		{"Invalid account number", nil, answer(http.StatusBadRequest, `{"message":"Invalid account number"}`), false, 3, nil, walkInterrupted, "Invalid account number"},
		{"the edge's page", nil, page, false, 3, nil, walkInterrupted, "the edge's Access Denied page"},
		{"a 503", nil, answer(http.StatusServiceUnavailable, `{"message":"down"}`), false, 3, nil, walkInterrupted, "a 503 after the Client's tries"},
		{"a 500", nil, whitelabel, false, 3, nil, walkInterrupted, "a 500 after the Client's tries"},
		{"a 302", nil, answer(http.StatusFound, ""), false, 3, nil, walkInterrupted, "a 302"},
		{
			"a failure of the network", nil, errors.New("connection reset"), false, 3, nil, walkInterrupted,
			"a failure of the network, or an answer that is not a list",
		},
		{"the run's stop", stopped, refusal, false, 3, nil, walkInterrupted, "the run's stop"},
		{"a refusal of the newest window", nil, refusal, true, 1, nil, walkInterrupted, "a refusal of the newest window"},
		{"a refusal with the allowance spent", nil, refusal, false, walkAllowance, nil, walkSpent, ""},
		{
			"a refusal, then the newest window refused", nil, refusal, false, 3, refusal, walkInterrupted,
			"a refusal, after which the newest window, asked again at once, did not answer 200",
		},
		{"a refusal, then the newest window answers", nil, refusal, false, 3, nil, walkRefused, "HTTP 400, " + said("the window is too old")},
		{
			"a JSON 403, then the newest window answers", nil, answer(http.StatusForbidden, `{"message":"not served"}`), false, 3, nil,
			walkRefused, "HTTP 403, " + said("not served"),
		},
	} {
		ctx := tc.ctx
		if ctx == nil {
			ctx = t.Context()
		}
		res := walkResult{sent: tc.sent}
		asked := 0
		(&liveRun{}).walkFailed(ctx, tc.err, &res, tc.newest, func() error { asked++; return tc.again })
		said := res.why
		if res.how == walkRefused {
			said = res.refusal
		}
		if res.how != tc.how || said != tc.said {
			t.Errorf("%s: %s, %q; want %s, %q", tc.name, res.how, said, tc.how, tc.said)
		}
		if want := strings.HasPrefix(tc.name, "a refusal, then") || strings.HasPrefix(tc.name, "a JSON 403"); (asked == 1) != want || asked > 1 {
			t.Errorf("%s: asked the newest window again %d times", tc.name, asked)
		}
	}
}

// Each way a walk ends has its line: with the oldest row's New York date
// where it saw one, the windows narrowed and the window of 2000.
func TestWalkLine(t *testing.T) {
	a := liveAccount{n: 2}
	oldest := time.Date(2020, 1, 2, 3, 0, 0, 0, time.UTC) // the evening of 2020-01-01 in New York
	for _, tc := range []struct {
		res  walkResult
		want string
	}{
		{walkResult{how: walkInterrupted, sent: 1, why: "a 429"}, "{account 2}: the walk was interrupted after 1 request by a 429, so it settles nothing"},
		{walkResult{how: walkSpent, sent: walkAllowance}, "{account 2}: the walk spent its allowance of 30 requests before it ended"},
		{
			walkResult{how: walkSpent, sent: walkAllowance, oldest: oldest},
			"{account 2}: the walk spent its allowance of 30 requests before it ended; the oldest row it saw is of 2020-01-01, New York time, and older ones may be left",
		},
		{
			walkResult{how: walkEnded, oldest: oldest, empty: 0, control: "200 with []"},
			"{account 2}: rows reach back to 2000-01-01, where the walk stops; the oldest is of 2020-01-01, New York time; the window of 2000: 200 with []",
		},
		{
			walkResult{how: walkEnded, oldest: oldest, empty: 1, narrowed: 2},
			"{account 2}: the oldest row is of 2020-01-01, New York time; the 1 window before it answered 200 with []; " +
				"the oldest window that held rows may have been cut at the cap, so it was narrowed and asked again 2 times",
		},
	} {
		if got := tc.res.line(a); got != tc.want {
			t.Errorf("line(%+v):\n got %s\nwant %s", tc.res, got, tc.want)
		}
	}
	if (walkResult{how: walkEnded, empty: 0, oldest: oldest}).passedHistory() || (walkResult{how: walkEnded}).passedHistory() ||
		!(walkResult{how: walkEnded, empty: 2, oldest: oldest}).passedHistory() || (walkResult{how: walkRefused, empty: 3, oldest: oldest}).passedHistory() {
		t.Error("passedHistory")
	}
}

// A fill's tradeDate is its instant, the next midnight in New York, the
// same day's, or another time; the fills of a span say one of them, or
// that they differ, or that there was none.
func TestFillTally(t *testing.T) {
	ny := newYork()
	fill := time.Date(2025, 1, 13, 19, 30, 0, 0, ny)
	var f fillTally
	if got := f.says(); got != "no fill seen" {
		t.Errorf("no fills: %q", got)
	}
	for _, tc := range []struct {
		tradeDate time.Time
		says      string
	}{
		{fill, "tradeDate is the fill's instant"},
		{time.Date(2025, 1, 14, 0, 0, 0, 0, ny), "tradeDate is the next midnight"},
		{time.Date(2025, 1, 13, 0, 0, 0, 0, ny), "tradeDate is the same day's midnight"},
		{time.Date(2025, 1, 14, 0, 0, 0, 0, time.UTC), "tradeDate is another time"},
	} {
		one := fillTally{}
		one.add(fill, tc.tradeDate)
		one.add(fill, tc.tradeDate)
		if got := one.says(); got != tc.says || one.total != 2 {
			t.Errorf("tradeDate %v: %q, of %d", tc.tradeDate, got, one.total)
		}
		f.add(fill, tc.tradeDate)
	}
	if got := f.says(); got != "tradeDate differs from fill to fill" ||
		f.counts("in all") != "fills in all, New York time: 4; tradeDate the fill's instant: 1, the next midnight: 1, the same day's midnight: 1, another time: 1" {
		t.Errorf("mixed fills: %q, %q", got, f.counts("in all"))
	}
}

// probeRow returns a row for Q4's plan: of type typ, at time at, with the
// description desc, whose one leg is symbol's, or underlying's option.
func probeRow(id int64, typ TransactionType, at time.Time, desc, symbol, underlying string) Transaction {
	return Transaction{
		ActivityID: id, Type: typ, Time: at, Description: desc,
		TransferItems: []TransferItem{{Instrument: Instrument{Symbol: symbol, UnderlyingSymbol: underlying}}},
	}
}

// Q4's plan chooses a symbol with a TRADE and a row that names it after a
// "~": one with both a dividend and a W-8 journal before one with only
// one, a TRADE of its own before its options', and never one that the
// scrubber does not hide; one window for rows within a year of each other,
// else one each; and it widens the window nearest a row of another symbol
// to take it in, or has no plan without one near.
func TestPlanFilter(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2025, 1, 1, 15, 0, 0, 0, time.UTC).AddDate(0, 0, d) }
	all := func(string) bool { return true }
	cash := func(id int64, typ TransactionType, d int, desc string) Transaction {
		return probeRow(id, typ, day(d), desc, "CURRENCY_USD", "")
	}
	if p, why := planFilter([]Transaction{probeRow(1, TransactionTrade, day(0), "", "AAA", ""), cash(2, TransactionWireIn, 1, "Wire Incoming")}, all); p != nil ||
		why != "no symbol of the history rows has both a TRADE and a dividend or a W-8 journal" {
		t.Errorf("no symbol: %+v, %q", p, why)
	}
	// AAA has a dividend a day from its TRADE, and BBB a dividend and a
	// W-8 journal; BBB's option is nearer them than its own TRADE. AAA's
	// rows, the only ones of another symbol, are more than a year away.
	rows := []Transaction{
		probeRow(1, TransactionTrade, day(-300), "", "AAA", ""),
		cash(2, TransactionDividendOrInterest, -299, "Ordinary Dividend~AAA"),
		probeRow(3, TransactionTrade, day(158), "", "BBB   250117C00010000", "BBB"),
		probeRow(4, TransactionTrade, day(100), "", "BBB", ""),
		cash(5, TransactionDividendOrInterest, 160, "Ordinary Dividend~BBB"),
		cash(6, TransactionJournal, 160, "W-8 Withholding~BBB"),
	}
	if p, why := planFilter(rows, all); p != nil || why != "no row of the history that names another symbol lies near enough to the chosen symbol's rows" {
		t.Errorf("no row of another symbol: %+v, %q", p, why)
	}
	rows = append(rows, cash(7, TransactionWireIn, 300, "Wire Incoming"))
	p, why := planFilter(rows, all)
	if p == nil || p.symbol != "BBB" || p.trade.ActivityID != 4 || p.dividend.ActivityID != 5 || p.w8.ActivityID != 6 || !p.widened ||
		len(p.windows) != 1 || !p.windows[0].start.Equal(day(99)) || !p.windows[0].end.Equal(day(301)) {
		t.Fatalf("plan: %+v, %q", p, why)
	}
	// A symbol that the scrubber does not hide is passed over: without
	// BBB, AAA is chosen, with a row of another symbol near it; without
	// either, none is.
	near := append(slices.Clone(rows), cash(8, TransactionWireIn, -290, "Wire Incoming"))
	if p, why := planFilter(near, func(s string) bool { return s != "BBB" }); p == nil || p.symbol != "AAA" || p.trade.ActivityID != 1 {
		t.Errorf("BBB not hidden: %+v, %q", p, why)
	}
	if p, why := planFilter(near, func(string) bool { return false }); p != nil ||
		why != "no symbol of the history rows that the report's scrubber hides has both a TRADE and a dividend or a W-8 journal" {
		t.Errorf("no symbol hidden: %+v, %q", p, why)
	}
	// Rows more than a year apart go in a window each, and a row of
	// another symbol already inside one needs no widening.
	far := []Transaction{
		probeRow(1, TransactionTrade, day(0), "", "AAA", ""),
		cash(2, TransactionDividendOrInterest, 400, "Ordinary Dividend~AAA"),
		cash(3, TransactionWireIn, 400, "Wire Incoming"),
	}
	if p, _ := planFilter(far, all); p == nil || p.widened || len(p.windows) != 2 || p.w8 != nil ||
		!p.windows[1].start.Equal(day(399)) || !p.windows[1].end.Equal(day(401)) {
		t.Errorf("far apart: %+v", p)
	}
}

// An order is read for its id, when it was entered, its status and when it
// was filled: by its last execution, or, for one filled without them, its
// closeTime.
func TestReadOrder(t *testing.T) {
	legs := `"orderActivityCollection":[{"executionLegs":[{"time":"2026-03-13T13:55:09+0000"},{"time":"2026-03-13T13:56:10+0000"}]}]`
	for _, tc := range []struct {
		body string
		want orderSeen
	}{
		{
			`{"orderId":1004517688020,"status":"FILLED","enteredTime":"2026-03-13T13:50:09+0000","closeTime":"2026-03-13T13:59:00+0000",` + legs + `}`,
			orderSeen{id: "1004517688020", status: "FILLED", entered: time.Date(2026, 3, 13, 13, 50, 9, 0, time.UTC), filled: time.Date(2026, 3, 13, 13, 56, 10, 0, time.UTC)},
		},
		{
			`{"orderId":7,"status":"FILLED","enteredTime":"2026-03-13T13:50:09+0000","closeTime":"2026-03-13T13:59:00+0000"}`,
			orderSeen{id: "7", status: "FILLED", entered: time.Date(2026, 3, 13, 13, 50, 9, 0, time.UTC), filled: time.Date(2026, 3, 13, 13, 59, 0, 0, time.UTC)},
		},
		{
			`{"orderId":8,"status":"WORKING","enteredTime":"2026-03-13T13:50:09+0000"}`,
			orderSeen{id: "8", status: "WORKING", entered: time.Date(2026, 3, 13, 13, 50, 9, 0, time.UTC)},
		},
		{`{"orderId":"x","enteredTime":7}`, orderSeen{}},
		{`[1]`, orderSeen{}},
	} {
		got := readOrder(json.RawMessage(tc.body))
		if got.id != tc.want.id || got.status != tc.want.status || !got.entered.Equal(tc.want.entered) || !got.filled.Equal(tc.want.filled) {
			t.Errorf("readOrder(%s) = %+v, want %+v", tc.body, got, tc.want)
		}
	}
	if _, err := readOrders([]byte(`{}`)); err == nil {
		t.Error("an answer that is not a list read as orders")
	}
}

// A row's shape gives its description only when it is on Schwab's
// wording, marking a W-8 one, its legs' enums as they are when they look
// like one, and whether the legs' costs sum to netAmount.
func TestRowShape(t *testing.T) {
	for _, tc := range []struct {
		row   Transaction
		shape string
		w8    bool
	}{
		{
			Transaction{
				ActivityID: 1, Description: "W-8 Withholding~BND", NetAmount: -1.16,
				TransferItems: []TransferItem{{Instrument: Instrument{AssetType: AssetCurrency}, Amount: -1.16}},
			},
			`"W-8 Withholding~SYM"; legs (CURRENCY, amount -, cost 0); ids activityId; the legs' costs sum to netAmount: no`, true,
		},
		{
			Transaction{
				ActivityID: 1, OrderID: 2, Description: "Wire Incoming from ACME BANK", NetAmount: 5,
				TransferItems: []TransferItem{{Instrument: Instrument{AssetType: "odd type"}, Cost: 5, FeeType: "COMMISSION", PositionEffect: "OPENING"}},
			},
			`other text; legs (an unusual value, amount 0, cost not 0, fee COMMISSION, OPENING); ids activityId, orderId; the legs' costs sum to netAmount: yes`, false,
		},
		{Transaction{}, `no description; legs none; ids none; the legs' costs sum to netAmount: yes`, false},
	} {
		if shape, w8 := rowShape(tc.row); shape != tc.shape || w8 != tc.w8 {
			t.Errorf("rowShape(%+v) = %q, %v; want %q, %v", tc.row, shape, w8, tc.shape, tc.w8)
		}
	}
}

// Q2 takes the executions of the history rows from 18:00 to midnight and
// from midnight to 04:00 in New York, and no others: not a row without an
// orderId, nor one of another type.
func TestProbeEveningFills(t *testing.T) {
	ny := newYork()
	fill := func(id int64, h, m int) Transaction {
		at := time.Date(2025, 1, 13, h, m, 0, 0, ny)
		return Transaction{ActivityID: id, OrderID: id, Type: TransactionTrade, Time: at, TradeDate: at}
	}
	rows := []Transaction{fill(1, 17, 59), fill(2, 18, 0), fill(3, 23, 59), fill(4, 0, 0), fill(5, 3, 59), fill(6, 4, 0)}
	noOrder, dividend := fill(7, 19, 0), fill(8, 19, 0)
	noOrder.OrderID, dividend.Type = 0, TransactionDividendOrInterest
	r := &liveRun{accounts: []liveAccount{{n: 1}}, history: map[int][]Transaction{1: append(rows, noOrder, dividend)}}
	v := probeEveningFills(t.Context(), r)
	if v.answer != "from 18:00 to midnight, tradeDate is the fill's instant; from midnight to 04:00, tradeDate is the fill's instant" ||
		len(v.evidence) != 2 || !strings.HasPrefix(v.evidence[0].text, "fills from 18:00 to midnight, New York time: 2;") ||
		!strings.HasPrefix(v.evidence[1].text, "fills from midnight to 04:00, New York time: 2;") {
		t.Errorf("probeEveningFills: %+v", v)
	}
	r.history[1] = rows[:1]
	if v := probeEveningFills(t.Context(), r); v.answer != "inconclusive: no execution of the history rows lies between 18:00 and 04:00 New York time" {
		t.Errorf("without evening fills: %q", v.answer)
	}
}
