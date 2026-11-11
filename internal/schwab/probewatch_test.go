package schwab

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

// balancesEvery is how often the watch of balances, Q3, samples them.
const balancesEvery = 10 * time.Minute

// balancesWatch returns the span of the watch of balances for a run that
// starts at now: from 15:45 in New York today, or tomorrow once today's
// close at 16:00 has passed, to 10:00 the day after.
func balancesWatch(now time.Time) (start, end time.Time) {
	ny := now.In(newYork())
	day := ny
	if ny.Hour() >= 16 {
		day = ny.AddDate(0, 0, 1)
	}
	start = time.Date(day.Year(), day.Month(), day.Day(), 15, 45, 0, 0, ny.Location())
	end = time.Date(day.Year(), day.Month(), day.Day()+1, 10, 0, 0, 0, ny.Location())
	return start, end
}

// A balanceSample is one account's balances as one sample of Q3 read them:
// when its read was sent and when its answer came, which bound when Schwab
// read the balances, and the total of its initial balances and of its
// current ones, each nil when Schwab sent none. They stay in memory: the
// report gives only times and yes or no.
type balanceSample struct {
	sent, answered   time.Time
	initial, current *float64
}

// initialTotal returns the total of a's initial balances: their
// liquidationValue, or else their accountValue.
func initialTotal(a Account) *float64 {
	b := a.SecuritiesAccount.InitialBalances
	return cmp.Or(b.LiquidationValue, b.AccountValue)
}

// currentTotal returns the total of a's current balances: their
// liquidationValue, or else the aggregated balance's, which a margin
// account's current balances can lack.
func currentTotal(a Account) *float64 {
	return cmp.Or(a.SecuritiesAccount.CurrentBalances.LiquidationValue, a.AggregatedBalance.CurrentLiquidationValue, a.AggregatedBalance.LiquidationValue)
}

// sameCents reports whether a and b, both set, are the same to the cent.
func sameCents(a, b *float64) bool {
	return a != nil && b != nil && math.Abs(*a-*b) < 0.005
}

// A balanceMove is what one account's samples say of when its initial
// balances moved to the new session's.
type balanceMove struct {
	moved          bool
	before, at     time.Time // when the last sample before the move was sent, and when the first after it was answered
	from, until    time.Time // when the first and the last samples were sent, when they did not move
	close, evening string    // whether they then equal the current total of 16:00-16:15 and of 20:00: yes, no, or why neither
	why            string    // why the samples say nothing, if they do not
}

// moveOf returns what samples, one account's in time order, say: the first
// sample whose initial total differs from the first's to the cent is the
// move, which came after the sample before it was sent and before its own
// answer came, and its total is held against the current total as sampled
// from 16:00 to 16:15 on the watch's first day, the close, and as sampled
// at 20:00, by the first sample in the 10 minutes from then, each sample
// sent and answered in its span. start is the watch's start, in New York.
func moveOf(samples []balanceSample, start time.Time) balanceMove {
	var m balanceMove
	var first *balanceSample
	for i := range samples {
		if samples[i].initial != nil {
			first = &samples[i]
			break
		}
	}
	if first == nil {
		m.why = "no sample held an initial total"
		return m
	}
	ny := start.Location()
	day := func(h, m int) time.Time { return time.Date(start.Year(), start.Month(), start.Day(), h, m, 0, 0, ny) }
	sampled := func(from, until time.Time) *float64 {
		for _, s := range samples {
			if !s.sent.Before(from) && s.answered.Before(until) {
				return s.current
			}
		}
		return nil
	}
	closing, evening := sampled(day(16, 0), day(16, 15)), sampled(day(20, 0), day(20, 10))
	last := first.sent
	for _, s := range samples {
		if s.initial == nil {
			continue
		}
		if !sameCents(s.initial, first.initial) {
			m.moved, m.before, m.at = true, last, s.answered
			word := func(total *float64, when string) string {
				switch {
				case total == nil:
					return "not known, with no current total sampled " + when
				case sameCents(s.initial, total):
					return "yes"
				}
				return "no"
			}
			m.close, m.evening = word(closing, "from 16:00 to 16:15"), word(evening, "at 20:00")
			return m
		}
		last = s.sent
	}
	m.from, m.until = first.sent, last
	return m
}

// nyClock returns t's time of day in New York to the minute, as the report
// gives one: rounded up when up is set, as an upper bound is, and down
// otherwise.
func nyClock(t time.Time, up bool) string {
	if c := t.Truncate(time.Minute); up && !c.Equal(t) {
		t = c.Add(time.Minute)
	}
	return t.In(newYork()).Format("15:04")
}

// line returns the report's line for m, account a's.
func (m balanceMove) line(a liveAccount) string {
	switch {
	case m.why != "":
		return a.label() + ": " + m.why
	case !m.moved:
		return fmt.Sprintf("%s: the initial balances did not move from %s to %s New York time", a.label(), nyClock(m.from, false), nyClock(m.until, false))
	}
	return fmt.Sprintf("%s: the initial balances moved between %s and %s New York time; they then equal the current total sampled from 16:00 to 16:15: %s; sampled at 20:00: %s",
		a.label(), nyClock(m.before, false), nyClock(m.at, true), m.close, m.evening)
}

// probeBalances is Q3, when the initial balances move to the new session,
// and whether they then hold the close. It reads the accounts, without
// their positions, every 10 minutes from 15:45 in New York to 10:00 the
// next day, waiting for 15:45 on the run's clock, and for each account
// finds the first sample whose initial total moved, and whether it then
// equals the current total as sampled from 16:00 to 16:15 and at 20:00, to
// the cent. The totals stay in memory. An account that holds only cash
// never moves.
func probeBalances(ctx context.Context, r *liveRun) verdict {
	start, end := balancesWatch(r.now())
	if wait := start.Sub(r.now()); wait > 0 {
		if err := r.wait(ctx, wait); err != nil {
			return inconclusive("the run stopped before the watch began at 15:45 New York time")
		}
	}
	samples := map[string][]balanceSample{} // by account number
	missed, read := 0, 0
	for at := r.now(); !at.After(end); at = at.Add(balancesEvery) {
		if wait := at.Sub(r.now()); wait > 0 {
			if err := r.wait(ctx, wait); err != nil {
				break
			}
		}
		sent := r.now()
		rep, err := r.read(ctx, "accounts", "/trader/v1/accounts", nil, "accounts")
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			missed++
			continue
		}
		read++
		answered := r.now()
		accounts, _ := decodeList[Account](rep.body)
		for _, acct := range accounts {
			n := acct.SecuritiesAccount.AccountNumber
			samples[n] = append(samples[n], balanceSample{sent: sent, answered: answered, initial: initialTotal(acct), current: currentTotal(acct)})
		}
	}
	v := verdict{evidence: lines(fmt.Sprintf("%s read, %s failed, every %v from 15:45 New York time", plural(read, "sample"), plural(missed, "sample"), balancesEvery))}
	var moves []balanceMove
	for _, a := range r.accounts {
		m := moveOf(samples[a.number], start.In(newYork()))
		v.evidence = append(v.evidence, reportLine{text: m.line(a)})
		if m.moved {
			moves = append(moves, m)
		}
	}
	if ctx.Err() != nil {
		v.answer = "inconclusive: the run stopped before the watch's end"
		return v
	}
	if len(moves) == 0 {
		v.answer = "inconclusive: no account's initial balances moved during the watch; those of an account that holds only cash never do"
		return v
	}
	same := !slices.ContainsFunc(moves, func(m balanceMove) bool {
		return !m.before.Equal(moves[0].before) || !m.at.Equal(moves[0].at) || m.close != moves[0].close || m.evening != moves[0].evening
	})
	if !same {
		v.answer = "the initial balances moved, at different samples or to different totals for different accounts"
		return v
	}
	m := moves[0]
	v.answer = fmt.Sprintf("the initial balances move between %s and %s New York time, and then equal the current total sampled from 16:00 to 16:15: %s; sampled at 20:00: %s",
		nyClock(m.before, false), nyClock(m.at, true), m.close, m.evening)
	return v
}

// The watch of fills, Q13: a poll a minute for half an hour, of the orders
// entered in the last day and of the TRADE rows that their fills can have.
const (
	fillsEvery      = time.Minute
	fillsPolls      = 30
	fillsOrdersBack = oneDay
)

// nyMidnight returns the midnight in New York that begins the day days
// after t's day there.
func nyMidnight(t time.Time, days int) time.Time {
	ny := t.In(newYork())
	return time.Date(ny.Year(), ny.Month(), ny.Day()+days, 0, 0, 0, 0, ny.Location())
}

// datedAhead is how many weekdays after its day an evening or overnight
// fill's row may be dated: the next trading day is the next weekday, or a
// later one after a holiday, and four weekdays take in up to three
// holidays in a row.
const datedAhead = 4

// lastDated returns the latest time by which a window takes in the row of
// a fill at t, whether windows go by a row's time or by its tradeDate, which
// is not known. A row's time is the fill, and its tradeDate the fill or the
// midnight that begins its day in New York, both by t. But a fill from
// 18:00 to 04:00 there, in the evening and overnight sessions, may be booked
// on the next trading day, as Q2 asks, so its row may be dated the midnight
// that begins it, which a window ending at t leaves out: the next weekday,
// or a later one after a holiday, as a Wednesday evening's fill before
// Thanksgiving is booked on the Friday. So for such a fill it is the
// midnight that begins the datedAhead-th weekday after t's day; a window
// may end after its request.
func lastDated(t time.Time) time.Time {
	if h := t.In(newYork()).Hour(); h >= overnightTil && h < eveningFrom {
		return t
	}
	next := t
	for range datedAhead {
		next = nyMidnight(next, 1)
		for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
			next = nyMidnight(next, 1)
		}
	}
	return next
}

// A fillSeen is a filled order that Q13's polls saw: its account, by its
// place in the account numbers, and its fill, by the order's first
// execution or its closeTime. Each execution of an order has a row of its
// own, with the order's id, and when rows post in the order of their
// executions, the first's comes first.
type fillSeen struct {
	account int
	filled  time.Time
}

// A rowsRead is one of Q13's reads of an account's TRADE rows: when it was
// sent and when its answer came, which bound when Schwab read the rows; the
// end of its window; and the orderIds of the rows it got, each with the
// latest tradeDate of its rows, or nil when it failed.
type rowsRead struct {
	sent, answered time.Time
	end            time.Time
	orders         map[string]time.Time
}

// How the row of a fill stands at the end of Q13's watch.
const (
	rowCame    = iota // in a read of the watch, after one without it
	rowThere          // in the first read that could show it, which bounds its time only from above
	rowMissing        // in no read, the last that could show it after the fill
	rowUnread         // in no read, none that could show it after the fill
)

// latency returns how the row of the order id, filled at filled, stands in
// reads, its account's reads of rows in time order, and what they allow of
// the time from the fill to the row: for a row that came, from the last read
// without it, or the fill, to the first read with it; for a row there at the
// first read that could show it, up to that read; and for a row missing,
// more than the time to the last read. Schwab read the rows of each read
// at some time from its sending to its answer, so a read without the row
// counts from when it was sent, and a read with it until its answer came.
// A read that failed says nothing, nor does one after the fill whose window
// ended before the row's tradeDate can, lastDated(filled): it could not
// have shown the row. Nor, unless ahead, does one sent before the later of
// the fill and the row's tradeDate, as the first read answered after the
// fill that held the row gives it, or before lastDated(filled) for a row
// that none held: whether Schwab serves the part of a window past its
// request is not known, and ahead says that an answer held a row dated
// after it came, which shows that a read can show such a row, as Schwab
// serves a window past the request, or takes rows by their time. A read
// whose answer came before the fill does not hold its row: a row of the
// order that it holds is of an execution that the order does not show,
// which the reads after the fill cannot tell from the fill's.
func latency(reads []rowsRead, id string, filled time.Time, ahead bool) (how int, least, most time.Duration) {
	dated := lastDated(filled) // the later of the fill and the row's tradeDate
	for _, rd := range reads {
		if d, ok := rd.orders[id]; ok && !rd.answered.Before(filled) {
			dated = maxTime(filled, d)
			break
		}
	}
	var before time.Time // when the last read without the row was sent
	for _, rd := range reads {
		_, held := rd.orders[id]
		switch {
		case rd.orders == nil:
		case held && !rd.answered.Before(filled) && before.IsZero():
			return rowThere, 0, rd.answered.Sub(filled)
		case held && !rd.answered.Before(filled):
			return rowCame, max(before.Sub(filled), 0), rd.answered.Sub(filled)
		case rd.sent.Before(filled) || !rd.end.Before(lastDated(filled)) && (ahead || !rd.sent.Before(dated)):
			before = rd.sent
		}
	}
	if before.IsZero() || before.Before(filled) {
		return rowUnread, 0, 0
	}
	return rowMissing, before.Sub(filled), 0
}

// probeFills is Q13, how soon after a fill its trade appears in the
// transactions, of a trade that the owner places by hand during the watch,
// whose reads before the fill bound the time from below. Every minute for
// half an hour it reads each account's orders entered in the last day and
// its TRADE rows over every time that their fills' rows can be dated by:
// from a second before the midnight in New York that begins the day a day
// before the watch, whether or not Schwab's start takes in a row dated at
// it, to the read, or later for a fill of the evening or overnight sessions,
// whose row may be dated the next trading day, to a second past the latest
// tradeDate that lastDated allows it, whether or not Schwab's end takes in
// a row dated at it. For each filled order it finds the first read that
// held a row with its orderId, and so the minutes from the fill to the
// row, bounded by when the read before was sent and when that read's
// answer came, as latency says. Every execution of an order has a row of
// its own, with the order's id, so it times an order filled in parts from
// its first execution, whose row comes first when rows post in the order
// of their executions. A row there at the first read that could show it
// bounds the time only from above, and a row that never came only from
// below; a read that failed says nothing. Whether Schwab serves the part of
// a window past its request, where the row of such a fill may lie, is not
// known: a read says nothing of a row dated after it unless an answer of
// the watch held a row dated after it came, which shows that a read can
// show such a row, as Schwab serves a window past the request, or takes
// rows by their time. Of a fill whose row no read could show, the verdict
// says no more than that. A verdict from rows that came starts its span at
// 0 when a row there at the first read that could show it was there within
// as many minutes of its fill as the least of theirs, or fewer.
func probeFills(ctx context.Context, r *liveRun) verdict {
	start := r.now()
	// Every fill lies after the earliest entry that the orders' span takes
	// in, so after this midnight, and the midnight that begins its day, a
	// tradeDate that rows carry, is this midnight or after it. Whether
	// Schwab's start takes in a row dated at it is not known, so the window
	// starts a second before it, not a millisecond, which Schwab may round
	// away.
	from := nyMidnight(start.Add(-fillsOrdersBack), 0).Add(-time.Second)
	fills := map[string]*fillSeen{} // by the order's id
	reads := map[int][]rowsRead{}   // by the account's place
	ahead := false                  // whether an answer held a row dated after it came
	polls, failedOrders, failedRows := 0, 0, 0
	for poll := range fillsPolls {
		at := start.Add(time.Duration(poll) * fillsEvery)
		if wait := at.Sub(r.now()); wait > 0 {
			if err := r.wait(ctx, wait); err != nil {
				break
			}
		}
		if ctx.Err() != nil {
			break
		}
		at = r.now()
		polls++
		for _, a := range r.accounts {
			if a.hash == "" {
				continue
			}
			rep, err := r.read(ctx, a.endpoint("/orders"), a.path()+"/orders", ordersValues(window{at.Add(-fillsOrdersBack), at}), "")
			var orders []orderSeen
			if err == nil {
				orders, err = readOrders(rep.body)
			}
			if err != nil {
				failedOrders++
			}
			for _, o := range orders {
				if o.id != "" && !o.filled.IsZero() && fills[o.id] == nil {
					fills[o.id] = &fillSeen{account: a.n, filled: o.filled}
				}
			}
			rd := rowsRead{sent: r.now()}
			rd.end = rd.sent
			for _, f := range fills {
				if f.account == a.n {
					rd.end = maxTime(rd.end, lastDated(f.filled).Add(time.Second))
				}
			}
			q, err := TransactionQuery{Start: from, End: rd.end, Types: []TransactionType{TransactionTrade}}.values()
			if err != nil {
				panic(err) // a few days is always a window
			}
			var latest time.Time // the latest tradeDate of the rows it got
			if rep, err := r.read(ctx, a.endpoint("/transactions"), a.path()+"/transactions", q, "transactions"); err == nil {
				if rows, err := decodeList[Transaction](rep.body); err == nil {
					rd.orders = map[string]time.Time{}
					for _, row := range rows {
						latest = maxTime(latest, row.TradeDate)
						if row.OrderID != 0 {
							id := strconv.FormatInt(row.OrderID, 10)
							rd.orders[id] = maxTime(rd.orders[id], row.TradeDate)
						}
					}
				}
			}
			rd.answered = r.now()
			ahead = ahead || latest.After(rd.answered)
			if rd.orders == nil {
				failedRows++
			}
			reads[a.n] = append(reads[a.n], rd)
		}
	}
	v := verdict{evidence: lines(fmt.Sprintf("%s, a minute apart; %s filled in the last day", plural(polls, "poll"), plural(len(fills), "order")))}
	if failedOrders+failedRows > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("reads that failed: %d of the orders, %d of the rows", failedOrders, failedRows)})
	}
	if len(fills) == 0 {
		v.answer = "inconclusive: no order of the last day was filled; run the watch, then place a trade by hand"
		return v
	}
	minutes := func(d time.Duration) int { return int(math.Ceil(d.Minutes())) }
	var least, most, upTo, waited []int
	unread := 0
	for id, f := range fills {
		switch how, lo, hi := latency(reads[f.account], id, f.filled, ahead); how {
		case rowCame:
			least, most = append(least, int(lo.Minutes())), append(most, minutes(hi))
		case rowThere:
			upTo = append(upTo, minutes(hi))
		case rowMissing:
			waited = append(waited, int(lo.Minutes()))
		default:
			unread++
		}
	}
	if len(least) > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("rows that came during the watch: %d, %s after the fills",
			len(least), minutesSpan(slices.Min(least), slices.Max(most)))})
	}
	if len(upTo) > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("rows there at the first read that could show them: %d, within %d minutes of the fills",
			len(upTo), slices.Max(upTo))})
	}
	if len(waited) > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("filled orders without a row by the watch's end: %d, %s after the fills",
			len(waited), minutesSpan(slices.Min(waited), slices.Max(waited)))})
	}
	if unread > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("filled orders whose rows no read after the fill could show: %d", unread)})
	}
	switch {
	case ctx.Err() != nil && len(least) == 0:
		v.answer = "inconclusive: the run stopped before a fill's row came"
	case len(least) > 0:
		lo := slices.Min(least)
		if len(upTo) > 0 && slices.Min(upTo) <= lo {
			// A row there at the first read that could show it came within
			// lo minutes of its fill or fewer, rounded up, and the rows that
			// came in lo minutes or more, rounded down: it may have come
			// sooner than any of them, and nothing bounds it from below.
			lo = 0
		}
		v.answer = fmt.Sprintf("a fill's trade appears %s after the fill", minutesSpan(lo, slices.Max(most)))
		if len(waited) > 0 {
			v.answer += fmt.Sprintf(", though %s had none by the watch's end, %d minutes or more after %s", plural(len(waited), "other fill"), slices.Min(waited),
				map[bool]string{true: "it", false: "them"}[len(waited) == 1])
		}
	case unread > 0 && len(waited)+len(upTo) > 0:
		// The verdicts below speak of every fill, but no read could show
		// these fills' rows.
		v.answer = "inconclusive: no read could show the rows of " + plural(unread, "fill") + ", and no other fill's row came during the watch"
	case len(waited) > 0 && len(upTo) == 0:
		v.answer = fmt.Sprintf("no fill's trade appeared by the watch's end, %s after the fills", minutesSpan(slices.Min(waited), slices.Max(waited)))
	case len(waited) > 0:
		v.answer = "inconclusive: no fill's row came during the watch: some were there at the first read that could show them, and the others had not come by its end"
	case len(upTo) > 0:
		v.answer = fmt.Sprintf("inconclusive: every fill's row was there at the first read that could show it, within %d minutes of its fill; "+
			"place a trade during the watch to measure it", slices.Max(upTo))
	default:
		v.answer = "inconclusive: no read after a fill could show its row"
	}
	return v
}

// minutesSpan returns the span from lo to hi minutes as the report gives
// it: "from 2 to 3 minutes", or "3 minutes" when they are one.
func minutesSpan(lo, hi int) string {
	if lo == hi {
		return plural(lo, "minute")
	}
	return fmt.Sprintf("from %d to %d minutes", lo, hi)
}

// rateSteps are Q14's steps, in requests a minute, a minute each: never
// above DefaultRatePerMinute, which leaves Schwab's 120 room.
var rateSteps = []int{20, 40, 60, 80, 100}

// The bounds of Q14: how long each step lasts, and how far behind its pace
// a step's request may go, since answers that come more slowly than the
// pace hold each request up; how long the rate file must be quiet before
// it starts, and the quiet before its burst; the burst, the Client's own,
// and how long after its first request its last may go; and how far it
// keeps from the scheduled snapshot.
const (
	rateStep    = time.Minute
	rateBehind  = 5 * time.Second
	rateQuiet   = 2 * time.Minute
	rateBurst   = DefaultBurst
	burstWithin = 3 * time.Second
	snapshotGap = 30 * time.Minute
)

// rateWrites returns when the rate file of l was last written, by any
// process, and when this process last wrote it, both read under the file's
// lock: a later write in the file than this process's own means that
// another process used the app key. A file that cannot be locked tells
// nothing of the others, so it is an error.
func rateWrites(ctx context.Context, l *limiter) (file, own time.Time, err error) {
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return time.Time{}, time.Time{}, ctx.Err()
	}
	defer func() { <-l.sem }()
	if l.noFile {
		return time.Time{}, time.Time{}, errors.New("the file system cannot lock it")
	}
	lk, err := l.take(ctx)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	defer lk.Release()
	buf := make([]byte, 2*rateRecordLen)
	n, err := lk.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return time.Time{}, time.Time{}, err
	}
	s := parseRate(buf[:n])
	if s.bad {
		return time.Time{}, time.Time{}, errors.New("it holds something other than the rate limit's record")
	}
	return s.last, l.mem.last, nil
}

// nearSnapshot reports whether the span from to until comes within
// snapshotGap of a scheduled snapshot, which runs each day at r.snapshot
// after midnight in r.loc, and the snapshot's time if it does.
func (r *liveRun) nearSnapshot(from, until time.Time) (bool, time.Time) {
	local := from.In(r.loc)
	h, m := int(r.snapshot/time.Hour), int(r.snapshot%time.Hour/time.Minute)
	for day := -1; day <= 2; day++ {
		at := time.Date(local.Year(), local.Month(), local.Day()+day, h, m, 0, 0, r.loc)
		if from.Before(at.Add(snapshotGap)) && until.After(at.Add(-snapshotGap)) {
			return true, at
		}
	}
	return false, time.Time{}
}

// rateReady says whether Q14 can start, before any request of the run: no
// process may have used the app key in the last two minutes, by the rate
// file, and the run's plan may not come within 30 minutes of the scheduled
// snapshot.
func rateReady(ctx context.Context, r *liveRun) error {
	now := r.now()
	_, takes := livePlan(modeRate, 0, now)
	if near, at := r.nearSnapshot(now, now.Add(takes)); near {
		return fmt.Errorf("its %v would come within %v of the scheduled snapshot at %s; run it at another time, or give the snapshot's time with -live.snapshot",
			takes, snapshotGap, at.Format("15:04"))
	}
	file, _, err := rateWrites(ctx, r.client.limit)
	if err != nil {
		return fmt.Errorf("the rate file could not be read (%w), so another process's use of the app key could not be seen", err)
	}
	if !file.IsZero() && now.Sub(file) < rateQuiet {
		return fmt.Errorf("a process used the app key less than %v ago; run it when nothing has used the key for %v", rateQuiet, rateQuiet)
	}
	return nil
}

// rateClient returns a Client of r's Auth for Q14: perMinute requests a
// minute, burst of them at once, one try each, sharing r's rate file and
// its limiter, on r's clock.
func (r *liveRun) rateClient(perMinute, burst int) *Client {
	c := NewClient(r.auth, ClientConfig{RatePerMinute: perMinute, Burst: burst, Tries: 1, RateFile: r.client.limit.path})
	c.now, c.sleep, c.jitter = r.client.now, r.client.sleep, r.client.jitter
	return c
}

// rateTurn waits until c's next request may go at once, so that this
// process writes the rate file no more between the check that follows and
// the request: a wait in the limiter writes the file, and a write of this
// process's would hide another's.
func (r *liveRun) rateTurn(ctx context.Context, c *Client) error {
	l := c.limit
	select {
	case l.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	due := l.mem.tat.Add(-c.tolerance)
	<-l.sem
	if wait := due.Sub(r.now()); wait > 0 {
		return r.wait(ctx, wait)
	}
	return nil
}

// rateClear returns why Q14 must not send its next request, which starts a
// span of d, or "": another process wrote the rate file after this one
// last did, or the span comes within 30 minutes of the scheduled snapshot.
func (r *liveRun) rateClear(ctx context.Context, d time.Duration) string {
	now := r.now()
	if near, at := r.nearSnapshot(now, now.Add(d)); near {
		return "the scheduled snapshot at " + at.Format("15:04") + " is near"
	}
	file, own, err := rateWrites(ctx, r.client.limit)
	switch {
	case err != nil:
		return "the rate file could not be read: " + err.Error()
	case file.After(own):
		return "another process used the app key"
	}
	return ""
}

// A rateEnd is why a request of Q14 ended it: a 429, at which request of
// its step and how long after the step's first, or why it stopped.
type rateEnd struct {
	limited bool
	at      string
	why     string
}

// rateSend sends the ith of n requests of a step of Q14 through c, once
// its turn comes and nothing stands in its way, and returns what ended
// Q14, if it did. began is when the step's first request went, which the
// first sets. A step's pace is a request every every, and a later request
// that would go more than behind after its place in that pace ends Q14:
// its answers came too slowly for the step to keep its pace, so a verdict
// could claim a rate that was not sent.
func (r *liveRun) rateSend(ctx context.Context, c *Client, i, n int, every, behind time.Duration, began *time.Time) *rateEnd {
	if err := r.rateTurn(ctx, c); err != nil {
		return &rateEnd{why: "the run stopped"}
	}
	if why := r.rateClear(ctx, rateStep); why != "" {
		return &rateEnd{why: why}
	}
	if i == 1 {
		*began = r.now()
	} else if after := r.now().Sub(*began); after-time.Duration(i-1)*every > behind {
		why := fmt.Sprintf("the answers came too slowly to keep the pace: request %d of %d would go %v after the first", i, n, after.Round(time.Second))
		if every > 0 {
			why += fmt.Sprintf(", %v behind its place", (after - time.Duration(i-1)*every).Round(time.Second))
		}
		return &rateEnd{why: why}
	}
	_, err := r.fetch(ctx, c, "accounts/accountNumbers", "/trader/v1/accounts/accountNumbers", nil, "accountNumbers", true, false)
	if err == nil {
		return nil
	}
	at := fmt.Sprintf("request %d of %d, %v after the first", i, n, r.now().Sub(*began).Round(time.Second))
	se, ok := errors.AsType[*Error](err)
	switch {
	case ok && se.Status == http.StatusTooManyRequests:
		return &rateEnd{limited: true, at: at}
	case ok && (se.Status == http.StatusForbidden || se.Status >= http.StatusInternalServerError):
		return &rateEnd{why: fmt.Sprintf("a %d came, at %s", se.Status, at)}
	}
	return &rateEnd{why: "a request failed, at " + at + ": " + schwabSays(err)}
}

// probeRate is Q14, how far under 120 a minute Schwab refuses requests, and
// whether a burst draws a refusal. It runs alone, once no process has used
// the app key for two minutes: the account numbers at 20, 40, 60, 80 and
// 100 a minute, a minute each, each request let go one at a time, then,
// after two quiet minutes, a burst of 5, all through the shared rate limit
// with one try each. It stops at the first 429, 403 or 5xx, and before any
// request if another process has written the rate file since this one last
// did, if the scheduled snapshot nears, or if the answers come so slowly
// that a request would go more than rateBehind after its place in its
// step's pace, or the burst's more than burstWithin after its first. Its
// answer is the first step with a 429, or that none came up to 100 a
// minute, and what the burst drew.
func probeRate(ctx context.Context, r *liveRun) verdict {
	var v verdict
	upTo := 0 // the fastest step that drew no 429
	stopped := func(why, step string) verdict {
		v.answer = "inconclusive: " + why + ", at " + step
		if upTo > 0 {
			v.answer += fmt.Sprintf("; no 429 came up to %d a minute", upTo)
		}
		return v
	}
	for _, rate := range rateSteps {
		c := r.rateClient(rate, 1)
		step := strconv.Itoa(rate) + " a minute"
		var began time.Time
		for i := 1; i <= rate; i++ {
			switch end := r.rateSend(ctx, c, i, rate, rateStep/time.Duration(rate), rateBehind, &began); {
			case end == nil:
			case end.limited:
				v.answer = "the first 429 came at " + step + ", at " + end.at
				return v
			default:
				return stopped(end.why, step)
			}
		}
		upTo = rate
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("%s: %s in %v, no 429", step, plural(rate, "request"), r.now().Sub(began).Round(time.Second))})
	}
	step := fmt.Sprintf("the burst of %d after %v of quiet", rateBurst, rateQuiet)
	if err := r.wait(ctx, rateQuiet); err != nil {
		return stopped("the run stopped", step)
	}
	c := r.rateClient(DefaultRatePerMinute, rateBurst)
	var began time.Time
	for i := 1; i <= rateBurst; i++ {
		switch end := r.rateSend(ctx, c, i, rateBurst, 0, burstWithin, &began); {
		case end == nil:
		case end.limited:
			v.answer = fmt.Sprintf("no 429 came up to %d a minute, but %s drew one, at %s", upTo, step, end.at)
			return v
		default:
			return stopped(end.why, step)
		}
	}
	v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("%s: %s in %v, no 429", step, plural(rateBurst, "request"), r.now().Sub(began).Round(time.Second))})
	v.answer = fmt.Sprintf("no 429 came up to %d a minute, nor at a burst of %d", upTo, rateBurst)
	return v
}

// An account's initial balances move at the first sample whose initial
// total differs from the first's to the cent, after the sample before it
// was sent and before its own answer came, the one rounded down to the
// minute and the other up, the first sample's sending when the move is at
// the second; and they then equal the close, or not, by the current total
// sampled from 16:00 to 16:15 and at 20:00 on the watch's first day, by a
// sample sent and answered in those spans. Samples without an initial
// total are passed over, and without them all nothing is said.
func TestMoveOf(t *testing.T) {
	ny := newYork()
	start := time.Date(2026, 9, 1, 15, 45, 0, 0, ny)
	f := func(v float64) *float64 { return &v }
	// sample returns a sample sent at h:m and answered 20 seconds later.
	sample := func(h, m int, initial, current *float64) balanceSample {
		sent := time.Date(2026, 9, 1, h, m, 0, 0, ny)
		return balanceSample{sent: sent, answered: sent.Add(20 * time.Second), initial: initial, current: current}
	}
	samples := []balanceSample{
		sample(15, 45, f(100), f(101)),
		sample(16, 5, f(100), f(102)),
		sample(17, 55, nil, f(102)),
		sample(18, 5, f(100.001), f(102)),
		sample(19, 55, f(100), f(102)),
		sample(20, 5, f(102), f(103)),
	}
	m := moveOf(samples, start)
	if !m.moved || !m.before.Equal(samples[4].sent) || !m.at.Equal(samples[5].answered) || m.close != "yes" || m.evening != "no" {
		t.Errorf("moveOf: %+v", m)
	}
	if got := m.line(liveAccount{n: 1}); got != "{account 1}: the initial balances moved between 19:55 and 20:06 New York time; "+
		"they then equal the current total sampled from 16:00 to 16:15: yes; sampled at 20:00: no" {
		t.Errorf("line: %s", got)
	}
	if m := moveOf(samples[:5], start); m.moved || !m.from.Equal(samples[0].sent) || !m.until.Equal(samples[4].sent) ||
		m.line(liveAccount{n: 1}) != "{account 1}: the initial balances did not move from 15:45 to 19:55 New York time" {
		t.Errorf("moveOf without a move: %+v", m)
	}
	// Without a sample from 16:00 to 16:15, or at 20:00, it cannot say; nor
	// with one sent before 16:00 and answered after it, which may have read
	// the balances before the close.
	early := []balanceSample{sample(15, 45, f(100), f(101)), sample(15, 59, f(100), f(101)), sample(16, 20, f(101), f(101))}
	early[1].sent = early[1].sent.Add(50 * time.Second) // 15:59:50, answered at 16:00:10
	early[1].answered = early[1].sent.Add(20 * time.Second)
	if m := moveOf(early, start); m.close != "not known, with no current total sampled from 16:00 to 16:15" || m.evening != "not known, with no current total sampled at 20:00" {
		t.Errorf("moveOf without the close: %+v", m)
	}
	// Nor with one sent before 16:15 and answered after it, which may have
	// read the balances after the span.
	late := []balanceSample{sample(15, 45, f(100), f(101)), sample(16, 14, f(100), f(101)), sample(16, 25, f(101), f(101))}
	late[1].sent = late[1].sent.Add(50 * time.Second) // 16:14:50, answered at 16:15:10
	late[1].answered = late[1].sent.Add(20 * time.Second)
	if m := moveOf(late, start); m.close != "not known, with no current total sampled from 16:00 to 16:15" {
		t.Errorf("moveOf with a sample answered after the close's span: %+v", m)
	}
	// A move at the second sample came after the first was sent.
	second := []balanceSample{sample(15, 45, f(100), f(101)), sample(15, 55, f(101), f(101))}
	if m := moveOf(second, start); !m.moved || !m.before.Equal(second[0].sent) || !m.at.Equal(second[1].answered) {
		t.Errorf("moveOf with a move at the second sample: %+v", m)
	}
	if m := moveOf([]balanceSample{sample(15, 45, nil, f(1))}, start); m.why != "no sample held an initial total" {
		t.Errorf("moveOf without an initial total: %+v", m)
	}
	// The clock of a bound: to the minute, an upper bound rounded up.
	at := time.Date(2026, 9, 1, 20, 5, 0, 0, ny)
	for _, tc := range []struct {
		t    time.Time
		up   bool
		want string
	}{{at, true, "20:05"}, {at.Add(time.Nanosecond), true, "20:06"}, {at.Add(59 * time.Second), false, "20:05"}} {
		if got := nyClock(tc.t, tc.up); got != tc.want {
			t.Errorf("nyClock(%v, %v) = %s, want %s", tc.t, tc.up, got, tc.want)
		}
	}
}

// A fill's row came after the last read that went without it, from when
// that read was sent, or after the fill, and by when the answer of the
// first read that held it came; a row there at the first read that could
// show it is bounded only from above, and one that never came only from
// below, by when the last read was sent. A read in flight at the fill,
// sent before it and answered after it, may hold its row, or go without
// it. A read that failed says nothing, nor does one after the fill whose
// window ends before the row's tradeDate can, nor, unless an answer held a
// row dated after it came, one sent before the row's tradeDate, or before
// the latest it can bear for a row that never came; and a read answered
// before the fill does not hold its row, though it may hold another row of
// the order, of an execution that the order does not show.
func TestFillLatency(t *testing.T) {
	base := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC) // 11:00 in New York
	const id = "1004517700001"
	// reads returns a read sent at each minute from base and answered 5
	// seconds later, its window ending as it was sent, holding the row
	// from the minute from on, or never with from -1, and failing at the
	// minutes of failed.
	reads := func(n, from int, failed ...int) []rowsRead {
		var rs []rowsRead
		for i := range n {
			sent := base.Add(time.Duration(i) * time.Minute)
			rd := rowsRead{sent: sent, answered: sent.Add(5 * time.Second), end: sent}
			if !slices.Contains(failed, i) {
				rd.orders = map[string]time.Time{"1": {}}
				if from >= 0 && i >= from {
					rd.orders[id] = time.Time{}
				}
			}
			rs = append(rs, rd)
		}
		return rs
	}
	// A read whose answer came 50 seconds after it was sent, as one behind
	// a slow read of another account's does.
	slow := reads(4, 3)
	slow[3].answered = slow[3].sent.Add(50 * time.Second)
	// An evening fill, whose row may be dated days later, read by windows
	// that end as the reads go, and by one that reaches its last date once
	// the orders show the fill.
	evening := base.Add(8*time.Hour + 5*time.Minute + 20*time.Second) // 19:05:20 in New York
	late := reads(4, 3)
	for i := range late {
		late[i].sent, late[i].answered = late[i].sent.Add(8*time.Hour+5*time.Minute), late[i].answered.Add(8*time.Hour+5*time.Minute)
		late[i].end = late[i].sent
	}
	late[3].end = lastDated(evening)
	never := slices.Clone(late)
	never[3] = rowsRead{sent: late[3].sent, answered: late[3].answered, end: late[3].end, orders: map[string]time.Time{}}
	// The same reads, each of a window that reaches the row's last date,
	// the last holding the row, dated as given.
	reaching := func(dated time.Time) []rowsRead {
		rs := slices.Clone(late)
		for i := range rs {
			rs[i].end = lastDated(evening)
		}
		rs[3].orders = map[string]time.Time{"1": {}, id: dated}
		return rs
	}
	nextMidnight := nyMidnight(evening, 1)
	for _, tc := range []struct {
		name        string
		reads       []rowsRead
		filled      time.Time
		ahead       bool // whether an answer held a row dated after it came
		how         int
		least, most time.Duration
	}{
		{"there at the first read", reads(4, 0), base.Add(-10 * time.Minute), false, rowThere, 0, 10*time.Minute + 5*time.Second},
		{"there at the first read that went", reads(4, 1, 0), base.Add(-10 * time.Minute), false, rowThere, 0, 11*time.Minute + 5*time.Second},
		{"came", reads(4, 3), base.Add(30 * time.Second), false, rowCame, 90 * time.Second, 155 * time.Second},
		{"came, the read that held it slow", slow, base.Add(30 * time.Second), false, rowCame, 90 * time.Second, 200 * time.Second},
		{"came after the fill and a read", reads(4, 2), base.Add(90 * time.Second), false, rowCame, 0, 35 * time.Second},
		{"there at a read sent before the fill and answered after it", reads(4, 0), base.Add(2 * time.Second), false, rowThere, 0, 3 * time.Second},
		{"came at a read sent before the fill and answered after it", reads(4, 1), base.Add(time.Minute + 2*time.Second), false, rowCame, 0, 3 * time.Second},
		{"came after a read sent before the fill and answered after it", reads(4, 1), base.Add(2 * time.Second), false, rowCame, 0, 63 * time.Second},
		{"came after a read that failed", reads(4, 3, 2), base.Add(30 * time.Second), false, rowCame, 30 * time.Second, 155 * time.Second},
		{"another row of the order before the fill", reads(4, 0), base.Add(90 * time.Second), false, rowCame, 0, 35 * time.Second},
		{"came after reads that could not show it", late, evening, false, rowCame, 0, 2*time.Minute + 45*time.Second},
		{"came after reads that reach its last date, dated by the fill", reaching(time.Time{}), evening, false, rowCame, 100 * time.Second, 165 * time.Second},
		{"came after reads that reach its date", reaching(nextMidnight), evening, true, rowCame, 100 * time.Second, 165 * time.Second},
		{"came after reads that reach its date, not known to serve it", reaching(nextMidnight), evening, false, rowCame, 0, 165 * time.Second},
		{"missing", reads(4, -1), base.Add(-10 * time.Minute), false, rowMissing, 13 * time.Minute, 0},
		{"missing, the last read failed", reads(4, -1, 3), base.Add(-10 * time.Minute), false, rowMissing, 12 * time.Minute, 0},
		{"missing, after reads that could not show it", never, evening, true, rowMissing, 2*time.Minute + 40*time.Second, 0},
		{"no read went", reads(4, 0, 0, 1, 2, 3), base.Add(-10 * time.Minute), false, rowUnread, 0, 0},
		{"no read went after the fill", reads(4, -1, 2, 3), base.Add(90 * time.Second), false, rowUnread, 0, 0},
		{"no read after the fill could show it", never[:3], evening, true, rowUnread, 0, 0},
		{"no read after the fill known to serve its last date", never, evening, false, rowUnread, 0, 0},
	} {
		if how, least, most := latency(tc.reads, id, tc.filled, tc.ahead); how != tc.how || least != tc.least || most != tc.most {
			t.Errorf("%s: latency = %d, %v, %v; want %d, %v, %v", tc.name, how, least, most, tc.how, tc.least, tc.most)
		}
	}
}

// The row of a fill is dated by the fill at the latest, but for a fill of
// the evening or overnight sessions, from 18:00 to 04:00 in New York, which
// may be booked on the next trading day, after a holiday or three, and so
// dated by the midnight that begins the fourth weekday after its day.
func TestLastDated(t *testing.T) {
	ny := newYork()
	at := func(month time.Month, day, h, m int) time.Time { return time.Date(2026, month, day, h, m, 0, 0, ny) }
	for _, tc := range []struct{ fill, want time.Time }{
		{at(9, 1, 4, 0), at(9, 1, 4, 0)},      // a Tuesday's pre-market
		{at(9, 1, 11, 5), at(9, 1, 11, 5)},    // its session
		{at(9, 1, 17, 59), at(9, 1, 17, 59)},  // its after-hours
		{at(9, 1, 18, 0), at(9, 7, 0, 0)},     // its evening, to the next Monday
		{at(9, 1, 3, 59), at(9, 7, 0, 0)},     // its overnight session
		{at(9, 4, 19, 5), at(9, 10, 0, 0)},    // a Friday's evening, to Thursday
		{at(9, 5, 1, 0), at(9, 10, 0, 0)},     // a Saturday's small hours
		{at(11, 25, 19, 35), at(12, 1, 0, 0)}, // the evening before Thanksgiving, past the Friday it is booked on
		{at(12, 24, 19, 0), at(12, 30, 0, 0)}, // Christmas Eve's, past Christmas and the weekend
	} {
		if got := lastDated(tc.fill); !got.Equal(tc.want) {
			t.Errorf("lastDated(%v) = %v, want %v", tc.fill, got, tc.want)
		}
	}
}

// A span comes near the scheduled snapshot when any of it lies within 30
// minutes of it, on whichever day, across midnight too.
func TestNearSnapshot(t *testing.T) {
	loc := time.FixedZone("UTC-5", -5*60*60)
	r := &liveRun{snapshot: 21*time.Hour + 5*time.Minute, loc: loc}
	at := func(d, h, m int) time.Time { return time.Date(2026, 9, d, h, m, 0, 0, loc) }
	for _, tc := range []struct {
		from, until time.Time
		near        bool
	}{
		{at(1, 20, 0), at(1, 20, 34), false},
		{at(1, 20, 0), at(1, 20, 36), true},
		{at(1, 21, 30), at(1, 21, 31), true},
		{at(1, 21, 35), at(1, 21, 40), false},
		{at(1, 12, 0), at(1, 12, 10), false},
		{at(1, 23, 59), at(2, 0, 1), false},
	} {
		near, when := r.nearSnapshot(tc.from, tc.until)
		if near != tc.near || near && when.In(loc).Format("15:04") != "21:05" {
			t.Errorf("nearSnapshot(%v, %v) = %v, %v", tc.from, tc.until, near, when)
		}
	}
	r.snapshot = 10 * time.Minute // 00:10, the next day's from late in the evening
	if near, when := r.nearSnapshot(at(1, 23, 50), at(1, 23, 51)); !near || !when.Equal(at(2, 0, 10)) {
		t.Errorf("across midnight: %v, %v", near, when)
	}
}
