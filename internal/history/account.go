package history

import (
	"cmp"
	"slices"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// accountInputs are what the derivation of one account reads: the
// evidence of the account, loaded by derive.go, and the rules' settings.
// deriveAccount reads nothing else, so its cases are tested on values.
type accountInputs struct {
	account int64
	// txns are the account's API transactions.
	txns []txnInput
	// reads are the account's complete reads of transactions, the only ones
	// that can show a transaction gone or cover a day.
	reads []readInput
	// absences are which transactions each complete read did not return.
	absences []absence
	// margin is how far inside a read's window a time must be for the read
	// to count it in: a second, unless a test asks for another.
	margin time.Duration
}

// A txnInput is what the derivation reads of an API transaction.
type txnInput struct {
	id int64
	// firstRead is the read that first returned it, and latestListRead the
	// last read of the account's list that did, or 0.
	firstRead, latestListRead int64
	// time and tradeDate are as Schwab sent them, zero when it sent none,
	// and tradeDay the day it was booked on.
	time, tradeDate time.Time
	tradeDay        nytime.Date
	// digests are its revisions' cut digests, in the order of their seq.
	digests [][32]byte
}

// A readInput is a complete read of the account's transactions.
type readInput struct {
	id int64
	// start and end are its window as sent, and sent when it was sent.
	start, end, sent time.Time
}

// An absence is a transaction that a complete read did not return.
type absence struct{ read, txn int64 }

// accountOutputs are what the derivation of one account gives, in a fixed
// order whatever the order of its inputs.
type accountOutputs struct {
	// txns are the API transactions' derived counts, by id.
	txns []txnOutput
	// coverage is the account's runs of covered days, by source and start.
	coverage []coverageRun
}

// A txnOutput is what the derivation gives an API transaction.
type txnOutput struct {
	id int64
	// revisions counts the runs of its cut form whose digest differs from
	// the run's before, the first included.
	revisions int
	// goneRead is the complete read that shows it gone, or 0.
	goneRead int64
}

// A coverageRun is a run of days on which a source holds every one of an
// account's transactions.
type coverageRun struct {
	source   string
	from, to nytime.Date
}

// deriveAccount derives what one account's evidence gives: each API
// transaction's count of revisions and the read that shows it gone, and
// the account's coverage by the API. It is a function of its inputs alone,
// whatever their order.
func deriveAccount(in accountInputs) accountOutputs {
	if in.margin == 0 {
		in.margin = time.Second
	}
	reads := map[int64]readInput{}
	for _, r := range in.reads {
		reads[r.id] = r
	}
	absent := map[int64][]int64{} // transaction -> the complete reads that did not return it
	for _, a := range in.absences {
		if _, ok := reads[a.read]; ok {
			absent[a.txn] = append(absent[a.txn], a.read)
		}
	}
	var out accountOutputs
	for _, t := range in.txns {
		out.txns = append(out.txns, txnOutput{id: t.id, revisions: revisions(t.digests), goneRead: goneRead(t, absent[t.id], reads, in.margin)})
	}
	slices.SortFunc(out.txns, func(a, b txnOutput) int { return cmp.Compare(a.id, b.id) })
	out.coverage = apiCoverage(in.txns, in.reads, in.margin)
	return out
}

// revisions counts the runs of a transaction's cut form that differ from
// the run before them: a later cut rule may make two runs one.
func revisions(digests [][32]byte) int {
	n := 1
	for i := 1; i < len(digests); i++ {
		if digests[i] != digests[i-1] {
			n++
		}
	}
	return n
}

// goneRead returns the first complete read that shows t gone, or 0: one
// that did not return it, made after the last list read that did (or, for
// a transaction no list read has returned, after the read that first
// showed it), whose window holds both its time and its trade date with the
// margin to spare. Whether a window takes rows by their time or their trade
// date is not known, so it must hold both; its end is taken as when it was
// sent, since whether Schwab serves a window past the request is not known
// either. A transaction without a time is never gone.
func goneRead(t txnInput, absentFrom []int64, reads map[int64]readInput, margin time.Duration) int64 {
	if t.time.IsZero() {
		return 0
	}
	after := t.latestListRead
	if after == 0 {
		after = t.firstRead
	}
	var gone int64
	for _, id := range absentFrom {
		r := reads[id]
		if id <= after || gone != 0 && id >= gone {
			continue
		}
		if inside(r, t.time, margin) && (t.tradeDate.IsZero() || inside(r, t.tradeDate, margin)) {
			gone = id
		}
	}
	return gone
}

// inside reports whether x lies inside read r's window, from its start to
// its end or when it was sent, whichever is earlier, with margin to spare
// at each end: a second covers bounds that may be inclusive or not, and
// milliseconds that may be kept, dropped or rounded.
func inside(r readInput, x time.Time, margin time.Duration) bool {
	return !x.Before(r.start.Add(margin)) && !x.After(minTime(r.end, r.sent).Add(-margin))
}

// apiCoverage returns the runs of New York days on which the account's
// complete reads hold every transaction that the API serves: the days that
// lie, with the margin on each side, inside one of the reads' windows merged
// where they overlap or touch, from the day of the account's oldest API
// transaction on. An empty window does not show that the history before it
// has ended, so nothing before the oldest transaction is covered, and an
// account without one has no coverage.
func apiCoverage(txns []txnInput, reads []readInput, margin time.Duration) []coverageRun {
	var floor nytime.Date
	for _, t := range txns {
		if t.tradeDay != "" && (floor == "" || t.tradeDay < floor) {
			floor = t.tradeDay
		}
	}
	if floor == "" || len(reads) == 0 {
		return nil
	}
	type window struct{ start, end time.Time }
	ws := make([]window, 0, len(reads))
	for _, r := range reads {
		ws = append(ws, window{r.start, minTime(r.end, r.sent)})
	}
	slices.SortFunc(ws, func(a, b window) int {
		return cmp.Or(a.start.Compare(b.start), a.end.Compare(b.end))
	})
	merged := []window{ws[0]}
	for _, w := range ws[1:] {
		last := &merged[len(merged)-1]
		if w.start.After(last.end) {
			merged = append(merged, w)
		} else if w.end.After(last.end) {
			last.end = w.end
		}
	}
	var runs []coverageRun
	for _, w := range merged {
		for d := nytime.DateOf(w.start); !midnight(d).Add(-margin).After(w.end); d = nextDay(d) {
			if d < floor || midnight(d).Add(-margin).Before(w.start) || midnight(nextDay(d)).Add(margin).After(w.end) {
				continue
			}
			if n := len(runs); n > 0 && nextDay(runs[n-1].to) == d {
				runs[n-1].to = d
			} else {
				runs = append(runs, coverageRun{source: "api", from: d, to: d})
			}
		}
	}
	return runs
}

// midnight returns the start of day d in New York.
func midnight(d nytime.Date) time.Time {
	t, err := time.ParseInLocation(time.DateOnly, string(d), nytime.NewYork())
	if err != nil {
		panic("history: not a date: " + string(d))
	}
	return t
}

// nextDay returns the day after d.
func nextDay(d nytime.Date) nytime.Date {
	t, err := time.Parse(time.DateOnly, string(d))
	if err != nil {
		panic("history: not a date: " + string(d))
	}
	return nytime.Date(t.AddDate(0, 0, 1).Format(time.DateOnly))
}
