package history

import (
	"cmp"
	"database/sql"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
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
	// snapshots are the account's snapshots.
	snapshots []snapshotInput
	// margin is how far inside a read's window a time must be for the read
	// to count it in: a second, unless a test asks for another.
	margin time.Duration
	// decisions are your decisions on the account's entries and on the
	// account, but for pairs, which pairAccounts reads.
	decisions []decisionInput
	// pairDecided holds the subjects of entries that your pair decisions
	// name, which pairWithin leaves to pairAccounts.
	pairDecided map[string]bool
	// keySecurities are the securities of the keys that your entry_security
	// decisions name, and names the symbols that securities have held, by
	// which a dividend's symbol finds its security.
	keySecurities map[string]int64
	names         []symbolName

	// number is the account's number, whose last digits a file's name
	// should give.
	number string
	// files are the files of schwab.com imported for the account, those
	// set aside included, in the order of their import; secs is what the
	// counting of their rows reads of the store's securities.
	files []*fileInput
	secs  *securities
	// fileDecisions are your decisions on the entries of file
	// transactions, each naming a row of one of files.
	fileDecisions []decisionInput
	// apiFiles are the spans of the rows that each `schwabtx transactions
	// -json` file holds of the account, which the API's rows of those
	// files cover (9.6 of the design).
	apiFiles []apiFileSpan
}

// An apiFileSpan is the span of the trade days of the rows of a
// `schwabtx transactions -json` file that are an account's, and whether
// your completeness decision says the file was filtered, which withdraws
// what it covers.
type apiFileSpan struct {
	oldest, newest nytime.Date
	filtered       bool
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
	// digests are its revisions' cut digests, in the order of their seq,
	// and changed the key paths that changed in each from the one before.
	digests [][32]byte
	changed [][]string

	// activityID is its activity ID, or 0, and idSource where that came
	// from: number, string or none.
	activityID int64
	idSource   string
	// typ, status, net and legs are its columns in txn and its legs in
	// api_leg; description is its description masked, and text unmasked.
	typ, status string
	description sql.NullString
	text        string
	net         sql.NullInt64
	legs        []legInput
	// problems are what the record could not read of it, and the numbers
	// that the store rounded or could not keep.
	problems []txnProblem
	// numberSent is the account number it named, masked, when that is not
	// the account read.
	numberSent string
	// duplicate says that an answer held it twice.
	duplicate bool
	// current is what the rules read of it now, and bases what they read
	// of the revisions that your decisions on it were made against, by seq;
	// both only when you made one.
	current apiClass
	bases   map[int]apiClass
}

// A readInput is a complete read of the account's transactions.
type readInput struct {
	id int64
	// start and end are its window as sent, and sent when it was sent.
	start, end, sent time.Time
}

// An absence is a transaction that a complete read did not return.
type absence struct{ read, txn int64 }

// A snapshotInput is what the derivation reads of a snapshot of the
// account.
type snapshotInput struct {
	id        int64
	fetchedAt time.Time
	// accountType is the account's type that the snapshot gives, or "".
	accountType string
	// market is the market date that the snapshot stands for, or "" before
	// the calendar; inSession the date of the session under way when it
	// was taken, or "".
	market, inSession nytime.Date
}

// accountOutputs are what the derivation of one account gives, in a fixed
// order whatever the order of its inputs.
type accountOutputs struct {
	// txns are the API transactions' derived counts, by id.
	txns []txnOutput
	// coverage is the account's runs of covered days, by source and start.
	coverage []coverageRun
	// accountType is the account's type as its latest snapshot that gives
	// one gives it, or "".
	accountType string
	// conflicts are the snapshot_conflict flags of its snapshots, by
	// snapshot.
	conflicts []snapshotFlag
	// entries are the account's ledger entries, by ref, paired within the
	// account; pairAccounts and finishPairs pair them across accounts.
	entries []ledgerEntry
	// kind is the account's kind, from your account decision.
	kind sql.NullString
	// files are what the account's files of schwab.com give: their
	// coverage, completeness and flags, their transactions and their
	// closed lots.
	files    []fileOutput
	fileTxns []csvTxnOutput
	lots     []lotOutput
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
// transaction's count of revisions and the read that shows it gone, the
// account's coverage by the API and by its snapshots, its type, the
// conflicts among its snapshots, and its ledger entries, classified,
// counted, flagged and paired within the account. It is a function of its
// inputs alone, whatever their order.
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
	apiRuns := append(apiCoverage(in.txns, in.reads, in.margin), apiFileCoverage(in.apiFiles)...)
	out.accountType = latestType(in.snapshots)
	out.conflicts = conflictFlags(in.snapshots)
	gone, revs, earlier := map[int64]int64{}, map[int64]int{}, map[int64]int64{}
	for _, t := range out.txns {
		gone[t.id], revs[t.id] = t.goneRead, t.revisions
	}
	for _, t := range in.txns {
		earlier[t.id] = absentEarlier(t, absent[t.id], reads, in.margin)
	}
	out.entries = apiEntries(in, gone, revs, earlier)
	fl := deriveFiles(in, out.entries, apiRuns)
	out.files, out.fileTxns, out.lots = fl.files, fl.txns, fl.lots
	out.entries = append(out.entries, fl.entries...)
	slices.SortFunc(out.entries, func(a, b ledgerEntry) int { return cmp.Compare(a.ref, b.ref) })
	out.coverage = append(append(apiRuns, snapshotCoverage(in.snapshots)...), fl.coverage...)
	slices.SortFunc(out.coverage, func(a, b coverageRun) int {
		return cmp.Or(cmp.Compare(a.source, b.source), cmp.Compare(a.from, b.from))
	})
	pairWithin(out.entries)
	if d, ok := latestDecisions(in.decisions)[[2]string{"account", "acct:" + strconv.FormatInt(in.account, 10)}]; ok {
		out.kind = sql.NullString{String: mask.Text(strings.TrimPrefix(d.value, "kind=")), Valid: true}
	}
	return out
}

// snapshotCoverage returns the run of market dates from the first to the
// last that a snapshot of the account taken outside a session stands for.
// A snapshot taken during a session stands for no close, and one before
// the calendar for none at all; the sessions in the run that no snapshot
// stands for are the account's gaps (the snapshot_gaps view).
func snapshotCoverage(snaps []snapshotInput) []coverageRun {
	var c coverageRun
	for _, s := range snaps {
		if s.market == "" || s.inSession != "" {
			continue
		}
		if c.from == "" || s.market < c.from {
			c.from = s.market
		}
		if s.market > c.to {
			c.to = s.market
		}
	}
	if c.from == "" {
		return nil
	}
	c.source = "snapshot"
	return []coverageRun{c}
}

// latestType returns the account's type that its latest snapshot to give
// one gives, by when it was taken and then by id, or "".
func latestType(snaps []snapshotInput) string {
	var latest *snapshotInput
	for i, s := range snaps {
		if s.accountType == "" {
			continue
		}
		if latest == nil || cmp.Or(s.fetchedAt.Compare(latest.fetchedAt), cmp.Compare(s.id, latest.id)) > 0 {
			latest = &snaps[i]
		}
	}
	if latest == nil {
		return ""
	}
	return latest.accountType
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

// absentEarlier returns the first complete read made before t was first
// read, by the import of a `schwabtx transactions -json` file, that did not
// return it though its window held its time and trade date with the margin
// to spare (9.8 of the design), or 0. Such a read may have run before the
// file was made, when a late row did not exist yet, so it does not show t
// gone; a list read that returns t settles it.
func absentEarlier(t txnInput, absentFrom []int64, reads map[int64]readInput, margin time.Duration) int64 {
	if t.time.IsZero() || t.latestListRead != 0 {
		return 0
	}
	var first int64
	for _, id := range absentFrom {
		r := reads[id]
		if id >= t.firstRead || first != 0 && id >= first {
			continue
		}
		if inside(r, t.time, margin) && (t.tradeDate.IsZero() || inside(r, t.tradeDate, margin)) {
			first = id
		}
	}
	return first
}

// apiFileCoverage returns the runs of days that the API rows of
// `schwabtx transactions -json` files cover (9.6 of the design): for each
// file not said to be filtered, the days strictly between its oldest and
// newest rows' trade days, since `-n` may have cut its oldest day and its
// newest may have gained rows after it ran.
func apiFileCoverage(spans []apiFileSpan) []coverageRun {
	var runs []coverageRun
	for _, s := range spans {
		if s.filtered || s.oldest == "" {
			continue
		}
		if from, to := nextDay(s.oldest), addDays(s.newest, -1); from <= to {
			runs = append(runs, coverageRun{source: "api_file", from: from, to: to})
		}
	}
	return mergeRuns(runs)
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
