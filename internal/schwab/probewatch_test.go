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
// of the last day and the TRADE rows of the last two hours.
const (
	fillsEvery      = time.Minute
	fillsPolls      = 30
	fillsOrdersBack = oneDay
	fillsRowsBack   = 2 * time.Hour
)

// A fillSeen is a filled order that Q13's polls saw, and its row.
type fillSeen struct {
	filled  time.Time // the fill, by the order's last execution or its closeTime
	rowPoll int       // the poll at which its row was first seen, or -1
}

// latency returns what the polls at polls say of f's row: the least and the
// most it can have taken to come, and whether it was there at the first
// poll, which bounds it only from above.
func (f fillSeen) latency(polls []time.Time) (least, most time.Duration, first bool) {
	most = polls[f.rowPoll].Sub(f.filled)
	if f.rowPoll == 0 {
		return 0, most, true
	}
	return max(polls[f.rowPoll-1].Sub(f.filled), 0), most, false
}

// probeFills is Q13, how soon after a fill its trade appears in the
// transactions, after the owner places a trade by hand. Every minute for
// half an hour it reads each account's orders of the last day and its
// TRADE rows of the last two hours; for each filled order it finds the poll
// at which a row with its orderId first came, and so the minutes from the
// fill to the row, bounded by that poll and the one before. A row there at
// the first poll bounds the time only from above.
func probeFills(ctx context.Context, r *liveRun) verdict {
	start := r.now()
	fills := map[string]*fillSeen{} // by the order's id
	var polls []time.Time
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
		polls = append(polls, at)
		for _, a := range r.accounts {
			if a.hash == "" {
				continue
			}
			if rep, err := r.read(ctx, a.endpoint("/orders"), a.path()+"/orders", ordersValues(window{at.Add(-fillsOrdersBack), at}), ""); err == nil {
				orders, _ := readOrders(rep.body)
				for _, o := range orders {
					if o.id != "" && !o.filled.IsZero() && fills[o.id] == nil {
						fills[o.id] = &fillSeen{filled: o.filled, rowPoll: -1}
					}
				}
			}
			q, err := TransactionQuery{Start: at.Add(-fillsRowsBack), End: at, Types: []TransactionType{TransactionTrade}}.values()
			if err != nil {
				panic(err) // two hours is always a window
			}
			if rep, err := r.read(ctx, a.endpoint("/transactions"), a.path()+"/transactions", q, "transactions"); err == nil {
				rows, _ := decodeList[Transaction](rep.body)
				for _, row := range rows {
					if f := fills[strconv.FormatInt(row.OrderID, 10)]; row.OrderID != 0 && f != nil && f.rowPoll < 0 {
						f.rowPoll = len(polls) - 1
					}
				}
			}
		}
	}
	v := verdict{evidence: lines(fmt.Sprintf("%s, a minute apart; %s filled in the last day", plural(len(polls), "poll"), plural(len(fills), "order")))}
	if len(fills) == 0 {
		v.answer = "inconclusive: no order of the last day was filled; place a trade by hand, then run the watch"
		return v
	}
	minutes := func(d time.Duration) int { return int(math.Ceil(d.Minutes())) }
	var least, most, upTo []int
	missing := 0
	for _, f := range fills {
		if f.rowPoll < 0 {
			missing++
			continue
		}
		lo, hi, first := f.latency(polls)
		if first {
			upTo = append(upTo, minutes(hi))
			continue
		}
		least, most = append(least, int(lo.Minutes())), append(most, minutes(hi))
	}
	if len(least) > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("rows that came during the watch: %d, from %d to %d minutes after the fills",
			len(least), slices.Min(least), slices.Max(most))})
	}
	if len(upTo) > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("rows there at the first poll: %d, within %d minutes of the fills", len(upTo), slices.Max(upTo))})
	}
	if missing > 0 {
		v.evidence = append(v.evidence, reportLine{text: fmt.Sprintf("filled orders without a row by the watch's end: %d", missing)})
	}
	switch {
	case ctx.Err() != nil && len(least) == 0:
		v.answer = "inconclusive: the run stopped before a fill's row came"
	case len(least) > 0:
		v.answer = fmt.Sprintf("a fill's trade appears from %d to %d minutes after the fill", slices.Min(least), slices.Max(most))
	case len(upTo) > 0:
		v.answer = fmt.Sprintf("inconclusive: every fill's row was there at the first poll, within %d minutes of its fill; place a trade just before the watch to measure it", slices.Max(upTo))
	default:
		v.answer = fmt.Sprintf("no fill's trade appeared within the watch's %v", time.Duration(fillsPolls)*fillsEvery)
	}
	return v
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

// A fill's row came after the poll before the one that saw it, and by
// that one; a row there at the first poll is bounded only from above.
func TestFillLatency(t *testing.T) {
	base := time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
	polls := []time.Time{base, base.Add(time.Minute), base.Add(2 * time.Minute), base.Add(3 * time.Minute)}
	for _, tc := range []struct {
		f           fillSeen
		least, most time.Duration
		first       bool
	}{
		{fillSeen{filled: base.Add(-10 * time.Minute), rowPoll: 0}, 0, 10 * time.Minute, true},
		{fillSeen{filled: base.Add(30 * time.Second), rowPoll: 3}, 90 * time.Second, 150 * time.Second, false},
		{fillSeen{filled: base.Add(90 * time.Second), rowPoll: 2}, 0, 30 * time.Second, false},
	} {
		if least, most, first := tc.f.latency(polls); least != tc.least || most != tc.most || first != tc.first {
			t.Errorf("latency(%+v) = %v, %v, %v; want %v, %v, %v", tc.f, least, most, first, tc.least, tc.most, tc.first)
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
