package history

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The derivation of one account, on values: a transaction's revisions
// count the runs whose cut digest differs from the run before; it is gone
// by the first complete read after the last list read that returned it
// whose window holds its time and trade date with the margin to spare,
// never by a read that is not complete or by one that returned it later;
// the API covers the days inside the merged windows from the oldest
// transaction's day on. The outputs do not depend on the order of the
// inputs.
func TestDeriveAccount(t *testing.T) {
	t.Parallel()
	at := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	a, b := [32]byte{1}, [32]byte{2}
	in := accountInputs{
		account: 1,
		txns: []txnInput{
			// Gone by read 3, the first complete read after read 1 that left
			// it out; read 2 is older than read 1 for it.
			{id: 10, firstRead: 1, latestListRead: 1, time: at(3, 15), tradeDate: at(3, 4), tradeDay: "2026-10-03", digests: [][32]byte{a, b, a}},
			// Returned by read 4, after the reads that left it out.
			{id: 11, firstRead: 1, latestListRead: 4, time: at(3, 16), tradeDate: at(3, 4), tradeDay: "2026-10-03", digests: [][32]byte{a, a}},
			// Its trade date is outside every window.
			{id: 12, firstRead: 1, latestListRead: 1, time: at(2, 15), tradeDate: at(1, 4), tradeDay: "2026-10-01", digests: [][32]byte{a}},
			// No list read returned it: its first read counts, and read 3 is
			// after it.
			{id: 13, firstRead: 2, time: at(4, 15), tradeDay: "2026-10-04", digests: [][32]byte{b}},
			// No time: never gone.
			{id: 14, firstRead: 1, latestListRead: 1, tradeDate: at(4, 4), tradeDay: "2026-10-04", digests: [][32]byte{b}},
			// After read 7 was sent, though inside its window: not gone.
			{id: 15, firstRead: 1, latestListRead: 1, time: at(8, 20), tradeDate: at(8, 4), tradeDay: "2026-10-08", digests: [][32]byte{b}},
			// Before read 7 was sent: gone by it.
			{id: 16, firstRead: 1, latestListRead: 1, time: at(8, 6), tradeDate: at(8, 4), tradeDay: "2026-10-08", digests: [][32]byte{b}},
		},
		reads: []readInput{
			{id: 1, start: at(2, 4), end: at(6, 4), sent: at(7, 12)},
			{id: 3, start: at(2, 4), end: at(9, 4), sent: at(9, 12)},
			{id: 5, start: at(2, 4), end: at(9, 4), sent: at(9, 13)},
			{id: 7, start: at(2, 4), end: at(20, 4), sent: at(8, 12)},
		},
		absences: []absence{
			{3, 10},
			{5, 10},
			{3, 11},
			{3, 12},
			{3, 13},
			{3, 14},
			{7, 15},
			{7, 16},
			// Read 2 is not complete, so it is not among the reads.
			{2, 10},
		},
	}
	want := accountOutputs{
		txns: []txnOutput{
			{id: 10, revisions: 3, goneRead: 3},
			{id: 11, revisions: 1},
			{id: 12, revisions: 1},
			{id: 13, revisions: 1, goneRead: 3},
			{id: 14, revisions: 1},
			{id: 15, revisions: 1},
			{id: 16, revisions: 1, goneRead: 7},
		},
		// The windows start at midnight on 2026-10-02 and end at midnight
		// on 2026-10-09, so the second before the first and the second
		// after the last are outside them.
		coverage: []coverageRun{{source: "api", from: "2026-10-03", to: "2026-10-07"}},
	}
	if got := deriveAccount(in); !reflect.DeepEqual(got, want) {
		t.Errorf("deriveAccount:\n%+v\nwant\n%+v", got, want)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 20 {
		sh := in
		sh.txns, sh.reads, sh.absences = slices.Clone(in.txns), slices.Clone(in.reads), slices.Clone(in.absences)
		r.Shuffle(len(sh.txns), func(i, j int) { sh.txns[i], sh.txns[j] = sh.txns[j], sh.txns[i] })
		r.Shuffle(len(sh.reads), func(i, j int) { sh.reads[i], sh.reads[j] = sh.reads[j], sh.reads[i] })
		r.Shuffle(len(sh.absences), func(i, j int) { sh.absences[i], sh.absences[j] = sh.absences[j], sh.absences[i] })
		if got := deriveAccount(sh); !reflect.DeepEqual(got, want) {
			t.Fatalf("with its inputs in another order, deriveAccount gives\n%+v", got)
		}
	}
	// A wider margin narrows what a read shows gone and covers.
	in.margin = 25 * time.Hour
	got := deriveAccount(in)
	if got.txns[0].goneRead != 0 || got.txns[3].goneRead != 3 {
		t.Errorf("with a margin of 25 hours, the reads that show gone are %+v", got.txns)
	}
	if want := []coverageRun{{source: "api", from: "2026-10-04", to: "2026-10-06"}}; !reflect.DeepEqual(got.coverage, want) {
		t.Errorf("with a margin of 25 hours, the API covers %+v, want %+v", got.coverage, want)
	}
	// Nothing before the oldest transaction's day is covered.
	in.margin = 0
	in.txns = slices.Clone(in.txns)
	for i := range in.txns {
		in.txns[i].tradeDay = "2026-10-05"
	}
	if got, want := deriveAccount(in).coverage, []coverageRun{{source: "api", from: "2026-10-05", to: "2026-10-07"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("from the oldest transaction's day on, the API covers %+v, want %+v", got, want)
	}
	if got := deriveAccount(accountInputs{reads: in.reads}); got.coverage != nil {
		t.Errorf("an account without transactions is covered: %+v", got.coverage)
	}
}

// A day is covered when its New York midnights, a second outside each,
// lie inside a window, so a window from midnight to midnight across a
// change of daylight saving time covers the days strictly inside it,
// however long they are.
func TestCoverageAcrossDST(t *testing.T) {
	t.Parallel()
	ny := nytime.NewYork()
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, ny) // the day clocks go back
	end := time.Date(2026, 11, 4, 0, 0, 0, 0, ny)
	txns := []txnInput{{id: 1, time: start, tradeDay: "2026-10-30"}}
	got := apiCoverage(txns, []readInput{{id: 1, start: start.Add(-time.Second), end: end.Add(time.Second), sent: end.Add(time.Hour)}}, time.Second)
	if want := []coverageRun{{source: "api", from: "2026-11-01", to: "2026-11-03"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("across the change of time: %+v, want %+v", got, want)
	}
	got = apiCoverage(txns, []readInput{{id: 1, start: start, end: end, sent: end}}, time.Second)
	if want := []coverageRun{{source: "api", from: "2026-11-02", to: "2026-11-02"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("from midnight to midnight: %+v, want %+v", got, want)
	}
}
