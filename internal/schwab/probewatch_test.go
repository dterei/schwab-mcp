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
// the total of its initial balances and of its current ones, each nil when
// Schwab sent none. They stay in memory: the report gives only times and
// yes or no.
type balanceSample struct {
	at               time.Time
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
	before, at     time.Time // the last sample before the move, and the first after
	from, until    time.Time // the samples' span, when they did not move
	close, evening string    // whether they then equal the current total of 16:00-16:15 and of 20:00: yes, no, or why neither
	why            string    // why the samples say nothing, if they do not
}

// moveOf returns what samples, one account's in time order, say: the first
// sample whose initial total differs from the first's to the cent is the
// move, and its total is held against the current total as sampled from
// 16:00 to 16:15 on the watch's first day, the close, and as sampled at
// 20:00, by the first sample in the 10 minutes from then. start is the
// watch's start, in New York.
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
			if !s.at.Before(from) && s.at.Before(until) {
				return s.current
			}
		}
		return nil
	}
	closing, evening := sampled(day(16, 0), day(16, 15)), sampled(day(20, 0), day(20, 10))
	last := first.at
	for _, s := range samples {
		if s.initial == nil {
			continue
		}
		if !sameCents(s.initial, first.initial) {
			m.moved, m.before, m.at = true, last, s.at
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
		last = s.at
	}
	m.from, m.until = first.at, last
	return m
}

// line returns the report's line for m, account a's.
func (m balanceMove) line(a liveAccount) string {
	clock := func(t time.Time) string { return t.In(newYork()).Format("15:04") }
	switch {
	case m.why != "":
		return a.label() + ": " + m.why
	case !m.moved:
		return fmt.Sprintf("%s: the initial balances did not move from %s to %s New York time", a.label(), clock(m.from), clock(m.until))
	}
	return fmt.Sprintf("%s: the initial balances moved between %s and %s New York time; they then equal the current total sampled from 16:00 to 16:15: %s; sampled at 20:00: %s",
		a.label(), clock(m.before), clock(m.at), m.close, m.evening)
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
		rep, err := r.read(ctx, "accounts", "/trader/v1/accounts", nil, "accounts")
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			missed++
			continue
		}
		read++
		accounts, _ := decodeList[Account](rep.body)
		for _, acct := range accounts {
			n := acct.SecuritiesAccount.AccountNumber
			samples[n] = append(samples[n], balanceSample{at: at, initial: initialTotal(acct), current: currentTotal(acct)})
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
	clock := func(t time.Time) string { return t.In(newYork()).Format("15:04") }
	v.answer = fmt.Sprintf("the initial balances move between %s and %s New York time, and then equal the current total sampled from 16:00 to 16:15: %s; sampled at 20:00: %s",
		clock(m.before), clock(m.at), m.close, m.evening)
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

// lastDated returns the latest time by which a window takes in the row of
// a fill at t, whether windows go by a row's time or by its tradeDate, which
// is not known. A row's time is the fill, and its tradeDate the fill or the
// midnight that begins its day in New York, both by t. But a fill from
// 18:00 to 04:00 there, in the evening and overnight sessions, may be booked
// on the next trading day, as Q2 asks, so its row may be dated the midnight
// that begins the next weekday, holidays aside, which a window ending at t
// leaves out.
func lastDated(t time.Time) time.Time {
	if h := t.In(newYork()).Hour(); h >= overnightTil && h < eveningFrom {
		return t
	}
	next := nyMidnight(t, 1)
	for next.Weekday() == time.Saturday || next.Weekday() == time.Sunday {
		next = nyMidnight(next, 1)
	}
	return next
}

// A fillSeen is a filled order that Q13's polls saw: its account, by its
// place in the account numbers, and its fill, by the order's last execution
// or its closeTime.
type fillSeen struct {
	account int
	filled  time.Time
}

// A rowsRead is one of Q13's reads of an account's TRADE rows: when it
// went, and the orderIds of the rows it got, or nil when it failed.
type rowsRead struct {
	at     time.Time
	orders map[string]bool
}

// How the row of a fill stands at the end of Q13's watch.
const (
	rowCame    = iota // in a read of the watch, after one without it
	rowThere          // in the first read that went, which bounds its time only from above
	rowMissing        // in no read, the last of them after the fill
	rowUnread         // in no read, none of them after the fill
)

// latency returns how the row of the order id, filled at filled, stands in
// reads, its account's reads of rows in time order, and what they allow of
// the time from the fill to the row: for a row that came, from the last read
// without it, or the fill, to the first read with it; for a row there at the
// first read, up to that read; and for a row missing, more than the time to
// the last read. A read that failed says nothing, nor does a read before the
// fill hold its row, though one may hold the row of an earlier execution of
// the same order.
func latency(reads []rowsRead, id string, filled time.Time) (how int, least, most time.Duration) {
	var before time.Time // the last read that went without the row
	for _, rd := range reads {
		switch {
		case rd.orders == nil:
			continue
		case rd.orders[id] && !rd.at.Before(filled) && before.IsZero():
			return rowThere, 0, rd.at.Sub(filled)
		case rd.orders[id] && !rd.at.Before(filled):
			return rowCame, max(before.Sub(filled), 0), rd.at.Sub(filled)
		}
		before = rd.at
	}
	if before.IsZero() || before.Before(filled) {
		return rowUnread, 0, 0
	}
	return rowMissing, before.Sub(filled), 0
}

// probeFills is Q13, how soon after a fill its trade appears in the
// transactions, after the owner places a trade by hand. Every minute for
// half an hour it reads each account's orders entered in the last day and
// its TRADE rows over every time that their fills' rows can be dated by:
// from the midnight in New York that begins the day a day before the watch,
// to the poll, or later for a fill of the evening or overnight sessions,
// whose row may be dated the next trading day. For each filled order it
// finds the first read that held a row with its orderId, and so the
// minutes from the fill to the row, bounded by that read and the one
// before. A row there at the first read bounds the time only from above,
// and a row that never came only from below; a read that failed says
// nothing.
func probeFills(ctx context.Context, r *liveRun) verdict {
	start := r.now()
	// Every fill lies after the earliest entry that the orders' span takes
	// in, so after this midnight, and so does the midnight that begins its
	// day, a tradeDate that rows carry.
	from := nyMidnight(start.Add(-fillsOrdersBack), 0)
	fills := map[string]*fillSeen{} // by the order's id
	reads := map[int][]rowsRead{}   // by the account's place
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
			end := at
			for _, f := range fills {
				if f.account == a.n {
					end = maxTime(end, lastDated(f.filled))
				}
			}
			q, err := TransactionQuery{Start: from, End: end, Types: []TransactionType{TransactionTrade}}.values()
			if err != nil {
				panic(err) // a few days is always a window
			}
			rd := rowsRead{at: at}
			if rep, err := r.read(ctx, a.endpoint("/transactions"), a.path()+"/transactions", q, "transactions"); err == nil {
				if rows, err := decodeList[Transaction](rep.body); err == nil {
					rd.orders = map[string]bool{}
					for _, row := range rows {
						if row.OrderID != 0 {
							rd.orders[strconv.FormatInt(row.OrderID, 10)] = true
						}
					}
				}
			}
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
		v.answer = "inconclusive: no order of the last day was filled; place a trade by hand, then run the watch"
		return v
	}
	minutes := func(d time.Duration) int { return int(math.Ceil(d.Minutes())) }
	var least, most, upTo, waited []int
	unread := 0
	for id, f := range fills {
		switch how, lo, hi := latency(reads[f.account], id, f.filled); how {
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
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("rows there at the first poll: %d, within %d minutes of the fills", len(upTo), slices.Max(upTo))})
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
		v.answer = fmt.Sprintf("a fill's trade appears %s after the fill", minutesSpan(slices.Min(least), slices.Max(most)))
		if len(waited) > 0 {
			v.answer += fmt.Sprintf(", though %s had none by the watch's end, %d minutes or more after it", plural(len(waited), "other fill"), slices.Min(waited))
		}
	case len(waited) > 0 && len(upTo) == 0:
		v.answer = fmt.Sprintf("no fill's trade appeared by the watch's end, %s after the fills", minutesSpan(slices.Min(waited), slices.Max(waited)))
	case len(waited) > 0:
		v.answer = "inconclusive: no fill's row came during the watch: some were there at the first poll, and the others had not come by its end"
	case len(upTo) > 0:
		v.answer = fmt.Sprintf("inconclusive: every fill's row was there at the first poll, within %d minutes of its fill; place a trade just before the watch to measure it", slices.Max(upTo))
	default:
		v.answer = "inconclusive: no read of the rows went after a fill"
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

// The bounds of Q14: how long each step lasts; how long the rate file must
// be quiet before it starts, and the quiet before its burst; the burst, the
// Client's own; and how far it keeps from the scheduled snapshot.
const (
	rateStep    = time.Minute
	rateQuiet   = 2 * time.Minute
	rateBurst   = DefaultBurst
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
// first sets.
func (r *liveRun) rateSend(ctx context.Context, c *Client, i, n int, began *time.Time) *rateEnd {
	if err := r.rateTurn(ctx, c); err != nil {
		return &rateEnd{why: "the run stopped"}
	}
	if why := r.rateClear(ctx, rateStep); why != "" {
		return &rateEnd{why: why}
	}
	if i == 1 {
		*began = r.now()
	}
	_, err := r.fetch(ctx, c, "accounts/accountNumbers", "/trader/v1/accounts/accountNumbers", nil, "accountNumbers", true)
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
// did, or if the scheduled snapshot nears. Its answer is the first step
// with a 429, or that none came up to 100 a minute, and what the burst
// drew.
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
			switch end := r.rateSend(ctx, c, i, rate, &began); {
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
		switch end := r.rateSend(ctx, c, i, rateBurst, &began); {
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
// total differs from the first's to the cent, and they then equal the
// close, or not, by the current total sampled from 16:00 to 16:15 and at
// 20:00 on the watch's first day; samples without an initial total are
// passed over, and without them all nothing is said.
func TestMoveOf(t *testing.T) {
	ny := newYork()
	start := time.Date(2026, 9, 1, 15, 45, 0, 0, ny)
	f := func(v float64) *float64 { return &v }
	sample := func(h, m int, initial, current *float64) balanceSample {
		return balanceSample{at: time.Date(2026, 9, 1, h, m, 0, 0, ny), initial: initial, current: current}
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
	if !m.moved || !m.before.Equal(samples[4].at) || !m.at.Equal(samples[5].at) || m.close != "yes" || m.evening != "no" {
		t.Errorf("moveOf: %+v", m)
	}
	if got := m.line(liveAccount{n: 1}); got != "{account 1}: the initial balances moved between 19:55 and 20:05 New York time; "+
		"they then equal the current total sampled from 16:00 to 16:15: yes; sampled at 20:00: no" {
		t.Errorf("line: %s", got)
	}
	if m := moveOf(samples[:5], start); m.moved || !m.from.Equal(samples[0].at) || !m.until.Equal(samples[4].at) ||
		m.line(liveAccount{n: 1}) != "{account 1}: the initial balances did not move from 15:45 to 19:55 New York time" {
		t.Errorf("moveOf without a move: %+v", m)
	}
	// Without a sample from 16:00 to 16:15, or at 20:00, it cannot say.
	early := []balanceSample{sample(15, 45, f(100), f(101)), sample(16, 20, f(101), f(101))}
	if m := moveOf(early, start); m.close != "not known, with no current total sampled from 16:00 to 16:15" || m.evening != "not known, with no current total sampled at 20:00" {
		t.Errorf("moveOf without the close: %+v", m)
	}
	if m := moveOf([]balanceSample{sample(15, 45, nil, f(1))}, start); m.why != "no sample held an initial total" {
		t.Errorf("moveOf without an initial total: %+v", m)
	}
}

// A fill's row came after the last read that went without it, or the fill,
// and by the first read that held it; a row there at the first read that
// went is bounded only from above, and one that never came only from
// below, by the last read. A read that failed says nothing, and a read
// before the fill does not hold its row, though it may hold the row of an
// earlier execution of the order.
func TestFillLatency(t *testing.T) {
	base := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	const id = "1004517700001"
	// reads returns a read at each minute from base, holding the row from
	// the minute from on, or never with from -1, and failing at the
	// minutes of failed.
	reads := func(n, from int, failed ...int) []rowsRead {
		var rs []rowsRead
		for i := range n {
			rd := rowsRead{at: base.Add(time.Duration(i) * time.Minute)}
			if !slices.Contains(failed, i) {
				rd.orders = map[string]bool{"1": true}
				if from >= 0 && i >= from {
					rd.orders[id] = true
				}
			}
			rs = append(rs, rd)
		}
		return rs
	}
	for _, tc := range []struct {
		name        string
		reads       []rowsRead
		filled      time.Time
		how         int
		least, most time.Duration
	}{
		{"there at the first read", reads(4, 0), base.Add(-10 * time.Minute), rowThere, 0, 10 * time.Minute},
		{"there at the first read that went", reads(4, 1, 0), base.Add(-10 * time.Minute), rowThere, 0, 11 * time.Minute},
		{"came", reads(4, 3), base.Add(30 * time.Second), rowCame, 90 * time.Second, 150 * time.Second},
		{"came after the fill and a read", reads(4, 2), base.Add(90 * time.Second), rowCame, 0, 30 * time.Second},
		{"came after a read that failed", reads(4, 3, 2), base.Add(30 * time.Second), rowCame, 30 * time.Second, 150 * time.Second},
		{"an earlier execution's row before the fill", reads(4, 0), base.Add(90 * time.Second), rowCame, 0, 30 * time.Second},
		{"missing", reads(4, -1), base.Add(-10 * time.Minute), rowMissing, 13 * time.Minute, 0},
		{"missing, the last read failed", reads(4, -1, 3), base.Add(-10 * time.Minute), rowMissing, 12 * time.Minute, 0},
		{"no read went", reads(4, 0, 0, 1, 2, 3), base.Add(-10 * time.Minute), rowUnread, 0, 0},
		{"no read went after the fill", reads(4, -1, 2, 3), base.Add(90 * time.Second), rowUnread, 0, 0},
	} {
		if how, least, most := latency(tc.reads, id, tc.filled); how != tc.how || least != tc.least || most != tc.most {
			t.Errorf("%s: latency = %d, %v, %v; want %d, %v, %v", tc.name, how, least, most, tc.how, tc.least, tc.most)
		}
	}
}

// The row of a fill is dated by the fill at the latest, but for a fill of
// the evening or overnight sessions, from 18:00 to 04:00 in New York, which
// may be dated the midnight that begins the next weekday there.
func TestLastDated(t *testing.T) {
	ny := newYork()
	at := func(day, h, m int) time.Time { return time.Date(2026, 9, day, h, m, 0, 0, ny) }
	for _, tc := range []struct{ fill, want time.Time }{
		{at(1, 4, 0), at(1, 4, 0)},     // a Tuesday's pre-market
		{at(1, 11, 5), at(1, 11, 5)},   // its session
		{at(1, 17, 59), at(1, 17, 59)}, // its after-hours
		{at(1, 18, 0), at(2, 0, 0)},    // its evening, to Wednesday
		{at(1, 3, 59), at(2, 0, 0)},    // its overnight session
		{at(4, 19, 5), at(7, 0, 0)},    // a Friday's evening, to Monday
		{at(5, 1, 0), at(7, 0, 0)},     // a Saturday's small hours
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
