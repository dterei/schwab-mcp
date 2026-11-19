package mcpserver

import (
	"errors"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The windows of a range (design 10.5): as few as hold at most 362 days
// each, of days as equal in number as can be, newest first, which cover the
// range once; each asked from the start of the day before its first day to
// the end of the day after its last, in New York, which is never more than
// Schwab's 365 days, across every change of daylight time of a year, and
// no later than now.
func TestWindowsOf(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		from, to nytime.Date
		want     []window
	}{
		{"2025-01-01", "2025-01-01", []window{{"2025-01-01", "2025-01-01"}}},
		{"2025-01-01", "2025-12-28", []window{{"2025-01-01", "2025-12-28"}}},
		{"2025-01-01", "2025-12-29", []window{{"2025-07-01", "2025-12-29"}, {"2025-01-01", "2025-06-30"}}},
		{"2024-01-01", "2025-12-24", []window{{"2024-12-28", "2025-12-24"}, {"2024-01-01", "2024-12-27"}}},
		{"2025-01-01", "2025-12-31", []window{{"2025-07-02", "2025-12-31"}, {"2025-01-01", "2025-07-01"}}},
	} {
		if got := windowsOf(tc.from, tc.to); !slices.Equal(got, tc.want) {
			t.Errorf("windowsOf(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
	for d := nytime.Date("2025-01-01"); d < "2026-01-01"; d = addDays(d, 1) {
		w := window{first: d, last: addDays(d, windowDays-1)}
		start, end := w.asked(ny(2030, time.January, 1, 0, 0))
		if !start.Equal(midnight(addDays(d, -1))) || !end.Equal(midnight(addDays(w.last, 2))) {
			t.Fatalf("%v is asked as %v to %v", w, start, end)
		}
		if end.Sub(start) > schwab.MaxTransactionWindow {
			t.Errorf("%v is asked as %v, over Schwab's %v", w, end.Sub(start), schwab.MaxTransactionWindow)
		}
	}
	w := window{first: "2026-11-01", last: "2026-11-13"}
	if _, end := w.asked(testNow); !end.Equal(testNow) {
		t.Errorf("a window to today ends at %v, want now", end)
	}
}

// The cache of windows (design 5.5) serves a window for 60 seconds, and
// drops it when it keeps the next; keeps at most 64 windows and 16 MB,
// dropping the oldest first; and keeps a read as big as it can hold, but
// none bigger.
func TestWindowCache(t *testing.T) {
	t.Parallel()
	now := testNow
	wc := newWindowCache()
	wc.now = func() time.Time { return now }
	key := func(i int) windowKey {
		return windowKey{hash: "H", first: addDays("2025-01-01", i), last: addDays("2025-01-01", i)}
	}
	wc.put(key(0), &windowRead{at: now, size: 10})
	if _, ok := wc.get(key(0)); !ok {
		t.Fatal("a window just read is not served")
	}
	now = now.Add(windowTTL)
	if _, ok := wc.get(key(0)); ok {
		t.Error("a window 60 seconds old is served")
	}
	wc.put(key(1), &windowRead{at: now, size: 10})
	if len(wc.order) != 1 || wc.size != 10 {
		t.Errorf("a put kept the expired window: %d windows of %d bytes", len(wc.order), wc.size)
	}
	for i := range maxWindows + 1 {
		wc.put(key(i), &windowRead{at: now, size: 10})
	}
	if _, ok := wc.get(key(0)); ok || len(wc.order) != maxWindows || wc.size != maxWindows*10 {
		t.Errorf("65 windows: %d kept, %d bytes, the first kept: %v", len(wc.order), wc.size, ok)
	}
	if _, ok := wc.get(key(maxWindows)); !ok {
		t.Error("the newest window is not served")
	}
	wc.put(key(100), &windowRead{at: now, size: maxWindowBytes - 100})
	if len(wc.order) != 11 || wc.size != maxWindowBytes {
		t.Errorf("a big read kept %d windows of %d bytes, want 11 of %d", len(wc.order), wc.size, maxWindowBytes)
	}
	wc.put(key(101), &windowRead{at: now, size: maxWindowBytes + 1})
	if _, ok := wc.get(key(101)); ok {
		t.Error("a read bigger than the cache is kept")
	}
	wc.put(key(100), &windowRead{at: now, size: 5})
	if _, ok := wc.get(key(100)); !ok || len(wc.order) != 11 || wc.size != 105 {
		t.Errorf("a window read again: %d windows of %d bytes, want 11 of 105", len(wc.order), wc.size)
	}
	wc.put(key(102), &windowRead{at: now, size: maxWindowBytes})
	if _, ok := wc.get(key(102)); !ok || len(wc.order) != 1 {
		t.Errorf("a read as big as the cache: kept %v, beside %d others", ok, len(wc.order)-1)
	}
	if newWindowCache().full != schwab.MaxTransactions || schwabtest.MaxTransactions != schwab.MaxTransactions {
		t.Error("a window is split at another size than Schwab's cap")
	}
}

// A window whose answer is full is halved, and each half asked again, the
// newer first, three times at most; a row of the instant between two
// halves is kept once; a half that is full when it cannot be halved again
// marks the window capped. Rows outside the window's days are left out.
func TestWindowSplits(t *testing.T) {
	t.Parallel()
	hash := schwabtest.HashFor(individual)
	var rows []string
	for i := range 8 {
		rows = append(rows, txnJSON(individual, 80000+int64(i), ny(2025, time.March, 2+3*i, 12, 0), "JOURNAL", "TRF FDS TO TYPE 1", 1))
	}
	rows = append(rows, txnJSON(individual, 80100, ny(2025, time.February, 25, 12, 0), "JOURNAL", "TRF FDS TO TYPE 1", 1))
	e := newEnv(t, withoutFixtures(), withFake(func(f *schwabtest.Server) {
		f.AddAccount(schwabtest.Account{Number: individual, Details: `{"securitiesAccount":{"type":"CASH","accountNumber":"` + individual + `"}}`})
		f.AddRows(hash, rows...)
	}))
	c := &call{
		s: e.srv, tool: "test", mask: e.srv.mask, accounts: e.srv.accounts, store: e.srv.store, now: testNow,
		budget: time.Minute, deadline: time.Now().Add(time.Minute),
	}
	a := dirAccount{number: individual, hash: schwab.AccountHash(hash)}
	w := window{first: "2025-03-01", last: "2025-03-31"}
	// The window's 8 rows are 3 days apart from March 2. Full at 8, the
	// halves hold 5 and 3; at 3, the halves are halved again, and their
	// halves of 3 once more; at 2 or 1, a piece of the last depth holds 2
	// or 1, and is capped. A piece without rows is never halved.
	for _, tc := range []struct {
		full, requests int
		capped         bool
	}{{9, 1, false}, {8, 3, false}, {3, 11, false}, {2, 13, true}, {1, 13, true}} {
		wc := newWindowCache()
		wc.full = tc.full
		before := len(e.fake.Requests())
		r, err := wc.read(t.Context(), c, &a, w)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(e.fake.Requests()) - before; n != tc.requests || r.capped != tc.capped {
			t.Errorf("full at %d: %d requests, capped %v; want %d, %v", tc.full, n, r.capped, tc.requests, tc.capped)
		}
		if tc.full == 8 {
			reqs := e.fake.Requests()[before:]
			if newer, older := txnStartOf(t, reqs[1].Query), txnStartOf(t, reqs[2].Query); newer <= older {
				t.Errorf("the halves were asked from %s, then from %s: the newer goes first", newer, older)
			}
		}
		var ids []int64
		for _, row := range r.rows {
			ids = append(ids, row.ActivityID)
		}
		slices.Sort(ids)
		if want := []int64{80000, 80001, 80002, 80003, 80004, 80005, 80006, 80007}; !tc.capped && !slices.Equal(ids, want) {
			t.Errorf("full at %d: rows %v, want %v", tc.full, ids, want)
		}
		if r.sent < len(r.rows) || len(r.entries) != len(r.rows) {
			t.Errorf("full at %d: %d sent, %d rows, %d entries", tc.full, r.sent, len(r.rows), len(r.entries))
		}
	}
	// A row at the instant between two halves, which the fake's bounds both
	// hold, comes back once; rows of the days before and after the window,
	// which its request holds, are sent but left out.
	start, end := w.asked(testNow)
	mid := start.Add(end.Sub(start) / 2)
	e.fake.AddRows(hash, txnJSON(individual, 80200, mid, "JOURNAL", "TRF FDS TO TYPE 1", 1),
		txnJSON(individual, 80300, ny(2025, time.February, 28, 12, 0), "JOURNAL", "TRF FDS TO TYPE 1", 1),
		txnJSON(individual, 80301, ny(2025, time.April, 1, 12, 0), "JOURNAL", "TRF FDS TO TYPE 1", 1))
	wc := newWindowCache()
	wc.full = 9
	r, err := wc.read(t.Context(), c, &a, w)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]int{}
	size := 0
	for _, row := range r.rows {
		seen[row.ActivityID]++
		size += len(row.Raw)
	}
	if seen[80200] != 1 || len(r.rows) != 9 || r.sent != 11 {
		t.Errorf("the row between the halves, and those of the days around: %v of %d sent", seen, r.sent)
	}
	if r.size != size || size == 0 {
		t.Errorf("the read holds %d bytes of JSON, says %d", size, r.size)
	}
	if _, ok := wc.get(windowKey{hash: a.hash, first: w.first, last: w.last}); !ok {
		t.Error("the window read is not kept")
	}
	if _, err := wc.read(t.Context(), &call{s: e.srv, deadline: time.Now(), budget: time.Second}, &a, window{first: "2025-04-01", last: "2025-04-30"}); !errors.Is(err, errNoTime) {
		t.Errorf("a read past the reserve: %v, want errNoTime", err)
	}
}

// txnStartOf returns the start of the span that a request for transactions
// asked, of its raw query, in Schwab's form, which sorts as text.
func txnStartOf(t *testing.T, query string) string {
	t.Helper()
	v, err := url.ParseQuery(query)
	if err != nil || v.Get("startDate") == "" {
		t.Fatalf("a request for transactions of the query %q", query)
	}
	return v.Get("startDate")
}
