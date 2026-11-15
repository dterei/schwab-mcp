package mcpserver

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Transactions are read from Schwab in windows of days (design 10.5). A
// window holds at most 362 days of trade dates, and is asked as the
// instants from the start of the day before its first day to the end of
// the day after its last, New York time: 364 days at most, so that a row
// whose time and trade day differ by a day is still read, and the request
// stays within Schwab's 365 days across a change of daylight time. Its rows
// are kept by their trade day. A window whose answer holds
// schwab.MaxTransactions rows, which Schwab may have cut, is split in
// halves, each asked again, three times at most. What a window read is
// kept for 60 seconds (windowCache), so that following next_cursor does not
// read Schwab again for each page.

const (
	// windowDays is the most days of trade dates that one window holds.
	windowDays = 362
	// schwabDays is the most days of one account that a call reads from
	// Schwab: two windows.
	schwabDays = 2 * windowDays
	// splitDepth is how many times a window is halved at most.
	splitDepth = 3
)

// A window is a run of days, both included, of one account's
// transactions, which one request to Schwab reads, or its halves.
type window struct {
	first, last nytime.Date
}

// windowsOf returns the windows that hold the days from from to to, both
// included, newest first: as few as hold at most windowDays days each, of
// days as equal in number as can be, so that the oldest is no sliver of a
// few days, which an empty answer would make a needless warning of.
func windowsOf(from, to nytime.Date) []window {
	days := daysFrom(from, to) + 1
	n := (days + windowDays - 1) / windowDays
	size := (days + n - 1) / n
	var ws []window
	for last := to; ; {
		first := max(addDays(last, 1-size), from)
		ws = append(ws, window{first: first, last: last})
		if first == from {
			return ws
		}
		last = addDays(first, -1)
	}
}

// asked returns the instants that w is asked as: from the start of the day
// before its first day to the end of the day after its last, in New York,
// or to now when that comes first. No row has a later time, and whether
// Schwab takes a window that ends after the request is not known
// (docs/schwab-api.md), so the newest window ends at now, as the CLI's do.
func (w window) asked(now time.Time) (start, end time.Time) {
	start, end = midnight(addDays(w.first, -1)), midnight(addDays(w.last, 2))
	if end.After(now) {
		end = now
	}
	return start, end
}

// dayOf returns d as midnight in UTC, for counting days, and whether d is
// a date.
func dayOf(d nytime.Date) (time.Time, bool) {
	t, err := time.Parse(time.DateOnly, string(d))
	return t, err == nil
}

// addDays returns the date n days after d, or before it for a negative n.
// d must be a date.
func addDays(d nytime.Date, n int) nytime.Date {
	t, _ := dayOf(d)
	return nytime.Date(t.AddDate(0, 0, n).Format(time.DateOnly))
}

// daysFrom returns how many days to comes after from: 0 for the same day.
// Both must be dates.
func daysFrom(from, to nytime.Date) int {
	f, _ := dayOf(from)
	t, _ := dayOf(to)
	return int(t.Sub(f).Hours()) / 24
}

// midnight returns the start of d, a date, in New York.
func midnight(d nytime.Date) time.Time {
	t, _ := dayOf(d)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, nytime.NewYork())
}

// errNoTime says that a walk stopped because the call's budget kept back
// its reserve (design 5.5): the window it was to read is unread.
var errNoTime = errors.New("the call's budget left no time for another request")

// A windowKey names a window of one account's transactions.
type windowKey struct {
	hash        schwab.AccountHash
	first, last nytime.Date
}

// A windowRead is what Schwab sent of one window: its rows whose trade day
// is one of the window's days, in the order Schwab sent them, each with the
// entry that the store derives of it.
type windowRead struct {
	rows    []schwab.Transaction
	entries []history.APIEntry
	// sent is how many rows Schwab sent, whatever their trade day; none is
	// also what Schwab answers for days older than it serves.
	sent int
	// capped says that a part of the window that could not be halved again
	// held schwab.MaxTransactions rows: older rows of that part may be
	// missing.
	capped bool
	// at is when it was read, by the system clock, and size the bytes of
	// Schwab's JSON it holds.
	at   time.Time
	size int
}

const (
	// windowTTL is how long a window read from Schwab serves later calls.
	windowTTL = 60 * time.Second
	// maxWindows and maxWindowBytes bound the cache: windows, and bytes of
	// Schwab's JSON.
	maxWindows     = 64
	maxWindowBytes = 16 << 20
)

// windowCache holds the windows read from Schwab, each for 60 seconds, at
// most 64 of them and 16 MB of Schwab's JSON; when it is full, the oldest
// go first. It is safe for concurrent use.
type windowCache struct {
	// now is the system clock, and full how many rows an answer holds that
	// Schwab may have cut, schwab.MaxTransactions; a test may set either.
	now   func() time.Time
	full  int
	mu    sync.Mutex
	reads map[windowKey]*windowRead
	order []windowKey // oldest first
	size  int
}

func newWindowCache() *windowCache {
	return &windowCache{now: time.Now, full: schwab.MaxTransactions, reads: map[windowKey]*windowRead{}}
}

// get returns the read of the window k, when one younger than 60 seconds
// is kept.
func (wc *windowCache) get(k windowKey) (*windowRead, bool) {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	r, ok := wc.reads[k]
	if !ok || wc.now().Sub(r.at) >= windowTTL {
		return nil, false
	}
	return r, true
}

// put keeps r, the read of the window k, dropping the reads that have
// expired, and then the oldest, until the cache holds at most 64 windows
// and 16 MB. A read bigger than the cache on its own is not kept.
func (wc *windowCache) put(k windowKey, r *windowRead) {
	if r.size > maxWindowBytes {
		return
	}
	wc.mu.Lock()
	defer wc.mu.Unlock()
	now := wc.now()
	wc.drop(func(_ windowKey, old *windowRead) bool { return now.Sub(old.at) >= windowTTL })
	wc.drop(func(old windowKey, _ *windowRead) bool { return old == k })
	for len(wc.order) > 0 && (len(wc.order) >= maxWindows || wc.size+r.size > maxWindowBytes) {
		oldest := wc.order[0]
		wc.drop(func(old windowKey, _ *windowRead) bool { return old == oldest })
	}
	wc.reads[k] = r
	wc.order = append(wc.order, k)
	wc.size += r.size
}

// drop removes the reads that gone says go. It needs the lock.
func (wc *windowCache) drop(gone func(windowKey, *windowRead) bool) {
	wc.order = slices.DeleteFunc(wc.order, func(k windowKey) bool {
		r := wc.reads[k]
		if !gone(k, r) {
			return false
		}
		wc.size -= r.size
		delete(wc.reads, k)
		return true
	})
}

// read returns what Schwab holds of *a's transactions in w: kept from an
// earlier call, or read, halved as need be, and derived as the store
// derives it (history.DeriveAPIEntries), with the account's full number.
// It begins no request without time for it beyond the call's reserve, and
// fails then with errNoTime. When Schwab refuses a's hash and the
// directory reads the account again (askAccount), *a is the account with
// its new hash, under which the read is kept.
func (wc *windowCache) read(ctx context.Context, c *call, a *dirAccount, w window) (*windowRead, error) {
	k := windowKey{hash: a.hash, first: w.first, last: w.last}
	if r, ok := wc.get(k); ok {
		return r, nil
	}
	start, end := w.asked(c.now)
	rows, capped, err := wc.readSpan(ctx, c, a, start, end, 0)
	if err != nil {
		return nil, err
	}
	k.hash = a.hash
	r := &windowRead{sent: len(rows), capped: capped, at: wc.now()}
	for _, t := range rows {
		if d := t.TradeDay(); w.first <= d && d <= w.last {
			r.rows = append(r.rows, t)
			r.size += len(t.Raw)
		}
	}
	if r.entries, err = history.DeriveAPIEntries(r.rows, a.number); err != nil {
		return nil, err
	}
	wc.put(k, r)
	return r, nil
}

// readSpan asks Schwab for *a's transactions from start to end. When the
// answer is full, holding as many rows as Schwab sends at most, which it
// may have cut, it asks for each half again, the newer first, depth being
// how often the span was halved already; capped says that a half that
// could not be halved again was full. A row that both halves hold, at the
// instant between them, is kept once; rows that one answer holds twice are
// kept as Schwab sent them, for the derivation to flag. A hash that Schwab
// refuses, and the directory reads again, is the new one in *a, which the
// halves ask with.
func (wc *windowCache) readSpan(ctx context.Context, c *call, a *dirAccount, start, end time.Time, depth int) ([]schwab.Transaction, bool, error) {
	if !c.roomForMore() {
		return nil, false, errNoTime
	}
	rows, err := askAccount(ctx, c, a, func(cl *schwab.Client, hash schwab.AccountHash) ([]schwab.Transaction, error) {
		return cl.Transactions(ctx, hash, schwab.TransactionQuery{Start: start, End: end})
	})
	if err != nil || len(rows) < wc.full {
		return rows, false, err
	}
	if depth == splitDepth {
		return rows, true, nil
	}
	mid := start.Add(end.Sub(start) / 2)
	newer, newerCapped, err := wc.readSpan(ctx, c, a, mid, end, depth+1)
	if err != nil {
		return nil, false, err
	}
	older, olderCapped, err := wc.readSpan(ctx, c, a, start, mid, depth+1)
	if err != nil {
		return nil, false, err
	}
	seen := map[string]bool{}
	for _, t := range newer {
		seen[lookup.Key(t)] = true
	}
	for _, t := range older {
		if !seen[lookup.Key(t)] {
			newer = append(newer, t)
		}
	}
	return newer, newerCapped || olderCapped, nil
}
