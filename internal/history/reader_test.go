package history

import (
	"cmp"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// readerCopy returns the path of a copy of readerStore in a folder of the
// test's own, readable only by you.
func readerCopy(t *testing.T) string {
	t.Helper()
	needReaderStore(t)
	b, err := os.ReadFile(readerStore)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// openReader opens a Reader of the store at path, and closes it when the
// test ends.
func openReader(t *testing.T, path string) *Reader {
	t.Helper()
	r, err := OpenReader(t.Context(), ReaderOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

// openStoreAt opens the store at path, as schwabtx does, with a fixed clock,
// and closes it when the test ends.
func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(t.Context(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// readerAccounts returns readerStore's three accounts as Ledger takes them,
// each keyed by its hash and IfWhole as ifWhole says, and the ids of the
// accounts by number.
func readerAccounts(t *testing.T, r *Reader, ifWhole bool) ([]LedgerAccount, map[string]int64) {
	t.Helper()
	numbers, err := r.AccountNumbers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	var accounts []LedgerAccount
	for _, n := range numbers {
		ids[n.Number] = n.ID
		accounts = append(accounts, LedgerAccount{ID: n.ID, Key: schwabtest.HashFor(n.Number), IfWhole: ifWhole})
	}
	if len(accounts) != 3 {
		t.Fatalf("readerStore holds %d accounts, want 3", len(accounts))
	}
	return accounts, ids
}

// rowRef returns a row of the ledger as the tests name it: its ref and its
// account's id.
func rowRef(ref string, account int64) string { return ref + "@A" + strconv.FormatInt(account, 10) }

// pageAll reads the ledger of q page by page, the first page of first rows
// and each after of q.Limit, and returns each row by its rowRef and the
// page it came on. Each page holds at most its limit, and starts where the
// one before said that the next starts, the key of its last row.
func pageAll(t *testing.T, r *Reader, q LedgerQuery, first int) (rows []string, pages []int) {
	t.Helper()
	limit := q.Limit
	q.Limit = first
	for n := 0; ; n++ {
		p, err := r.Ledger(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Rows) > q.Limit || p.Next != nil && len(p.Rows) != q.Limit {
			t.Fatalf("page %d holds %d rows of %d, its next %+v", n, len(p.Rows), q.Limit, p.Next)
		}
		for _, x := range p.Rows {
			rows, pages = append(rows, rowRef(x.Ref, x.AccountID)), append(pages, n)
		}
		if p.Next == nil {
			return rows, pages
		}
		if last := p.Rows[len(p.Rows)-1].Key; *p.Next != last {
			t.Fatalf("page %d's next is %+v, not its last row's key %+v", n, *p.Next, last)
		}
		if n > 100 {
			t.Fatal("more than 100 pages")
		}
		q.After, q.Limit = p.Next, limit
	}
}

// An exportedEntry is a row of the ledger or entries table of an export.
type exportedEntry struct {
	ref              string
	account          int64
	day, time        string // time as the store writes an instant, "" for none
	source, kind     string
	category, symbol string
	amount, fees     *int64
	counted          bool
	key              string // the account's key, as readerAccounts gives it
}

// exportedEntries returns the rows of the table that Export writes of the
// ledger, or of every entry with o.All, in readerStore's copy s; keys are
// the accounts' keys by id.
func exportedEntries(t *testing.T, s *Store, o ExportOptions, keys map[int64]string) []exportedEntry {
	t.Helper()
	o.What = []string{"ledger"}
	e, err := s.Export(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	tb := e.Tables[0]
	col := func(name string) int {
		i := slices.Index(tb.Columns, name)
		if i < 0 && name != "counted" {
			t.Fatalf("the export's %s has no column %s: %q", tb.Name, name, tb.Columns)
		}
		return i
	}
	cents := func(c cell) *int64 {
		if c.kind == cellNull {
			return nil
		}
		v, err := strconv.ParseInt(strings.Replace(c.text, ".", "", 1), 10, 64)
		if err != nil {
			t.Fatalf("money %q: %v", c.text, err)
		}
		return &v
	}
	var out []exportedEntry
	for _, row := range tb.rows {
		account, err := strconv.ParseInt(row[col("account_id")].text, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		x := exportedEntry{
			ref: row[col("ref")].text, account: account, day: row[col("day")].text, source: row[col("source")].text,
			kind: row[col("kind")].text, category: row[col("category")].text, symbol: row[col("symbol")].text,
			amount: cents(row[col("amount")]), fees: cents(row[col("fees")]), counted: true, key: keys[account],
		}
		if c := row[col("time")]; c.kind != cellNull {
			at, err := time.Parse(time.RFC3339, c.text)
			if err != nil {
				t.Fatal(err)
			}
			x.time = instant(at)
		}
		if i := col("counted"); i >= 0 {
			x.counted = row[i].text == "1"
		}
		out = append(out, x)
	}
	// The ledger's order: newest first, by day, time, ref and account.
	slices.SortFunc(out, func(a, b exportedEntry) int {
		return -cmp.Or(cmp.Compare(a.day, b.day), cmp.Compare(a.time, b.time), cmp.Compare(a.ref, b.ref), cmp.Compare(a.key, b.key))
	})
	return out
}

// Pages of 3 rebuild exactly the ledger that Export reads, every row once
// and in the ledger's order, whatever the filters: every entry, the
// categories, Schwab's types, a symbol in any case and the days. The two
// rows that share an activity ID, a day and a time in two accounts, which
// differ only in their account, are each read once though the first page
// ends between them.
func TestLedgerPagesCoverEachRowOnce(t *testing.T) {
	t.Parallel()
	path := readerCopy(t)
	r := openReader(t, path)
	s := openStoreAt(t, path)
	accounts, _ := readerAccounts(t, r, false)
	keys := map[int64]string{}
	for _, a := range accounts {
		keys[a.ID] = a.Key
	}
	journal := "T" + strconv.Itoa(readerJournal) + "@"
	all := ExportOptions{From: "2023-01-01", To: "2026-12-31"}
	exports := map[string][]exportedEntry{} // by the options, which cases share
	for _, tc := range []struct {
		name   string
		q      LedgerQuery
		export ExportOptions
		keep   func(exportedEntry) bool
	}{
		{"every counted row", LedgerQuery{From: "2023-01-01", To: "2026-12-31"}, all, nil},
		{"every entry", LedgerQuery{From: "2023-01-01", To: "2026-12-31", All: true}, ExportOptions{From: "2023-01-01", To: "2026-12-31", All: true}, nil},
		{"the days of 2025", LedgerQuery{From: "2025-01-01", To: "2025-12-31"}, ExportOptions{From: "2025-01-01", To: "2025-12-31"}, nil},
		{
			"dividends and buys",
			LedgerQuery{From: "2023-01-01", To: "2026-12-31", Categories: []string{"dividend", "buy"}},
			all,
			func(x exportedEntry) bool { return x.category == "dividend" || x.category == "buy" },
		},
		{
			"Schwab's trades and journals",
			LedgerQuery{From: "2023-01-01", To: "2026-12-31", Types: []string{"TRADE", "JOURNAL"}},
			all,
			func(x exportedEntry) bool { return x.source == "api" && (x.kind == "TRADE" || x.kind == "JOURNAL") },
		},
		{
			"a symbol in lower case",
			LedgerQuery{From: "2023-01-01", To: "2026-12-31", Symbol: "xyz"},
			all,
			func(x exportedEntry) bool { return x.symbol == "XYZ" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := fmt.Sprint(tc.export)
			if exports[o] == nil {
				exports[o] = exportedEntries(t, s, tc.export, keys)
			}
			var want []string
			for _, x := range exports[o] {
				if tc.keep == nil || tc.keep(x) {
					want = append(want, rowRef(x.ref, x.account))
				}
			}
			if len(want) < 4 {
				t.Fatalf("only %d rows, too few for the test", len(want))
			}
			// The first page ends at the journal's first side, where it has
			// two side by side, and else at its first 3 rows.
			first := 3
			if i := slices.IndexFunc(want, func(ref string) bool { return strings.HasPrefix(ref, journal) }); i >= 0 && i+1 < len(want) &&
				strings.HasPrefix(want[i+1], journal) {
				first = i + 1
			}
			q := tc.q
			q.Accounts, q.Limit = accounts, 3
			got, pages := pageAll(t, r, q, first)
			if !slices.Equal(got, want) {
				t.Errorf("the pages hold\n%q\nwant\n%q", got, want)
			}
			if tc.name == "every counted row" && (first == 3 || pages[first-1] == pages[first]) {
				t.Errorf("no page ends between the journal's sides: the first holds %d rows of %q", first, got)
			}
		})
	}
}

// The totals are of every page: per category, the count of the counted rows
// that the query matches and the sums of their amounts and fees, as
// Export's rows add up, with no sum of the amounts of a category that holds
// a row whose amount the store could not keep. They do not depend on where
// the page starts, and Matched counts each account's rows that the query
// matches, of every entry under All.
func TestLedgerTotals(t *testing.T) {
	t.Parallel()
	path := readerCopy(t)
	r := openReader(t, path)
	s := openStoreAt(t, path)
	accounts, _ := readerAccounts(t, r, false)
	keys := map[int64]string{}
	for _, a := range accounts {
		keys[a.ID] = a.Key
	}
	add := func(sum **int64, v *int64) {
		if *sum != nil && v != nil {
			**sum += *v
		} else {
			*sum = nil
		}
	}
	for _, q := range []LedgerQuery{
		{From: "2023-01-01", To: "2026-12-31"},
		{From: "2025-01-01", To: "2025-12-31", All: true},
		{From: "2023-01-01", To: "2026-12-31", Categories: []string{"interest", "buy"}},
	} {
		q.Accounts, q.Limit = accounts, 2
		want := map[string]*CategoryTotal{}
		matched := map[int64]int{}
		for _, x := range exportedEntries(t, s, ExportOptions{From: q.From, To: q.To, All: q.All}, keys) {
			if len(q.Categories) > 0 && !slices.Contains(q.Categories, x.category) {
				continue
			}
			matched[x.account]++
			if !x.counted {
				continue
			}
			c := want[x.category]
			if c == nil {
				c = &CategoryTotal{Category: x.category, AmountCents: new(int64), FeesCents: new(int64)}
				want[x.category] = c
			}
			c.Count++
			add(&c.AmountCents, x.amount)
			add(&c.FeesCents, x.fees)
		}
		var wantTotals []CategoryTotal
		for _, name := range slices.Sorted(maps.Keys(want)) {
			wantTotals = append(wantTotals, *want[name])
		}
		var wantMatched []AccountRows
		for _, a := range accounts {
			if n := matched[a.ID]; n > 0 {
				wantMatched = append(wantMatched, AccountRows{AccountID: a.ID, Rows: n})
			}
		}
		first, err := r.Ledger(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		q.After = first.Next
		later, err := r.Ledger(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []*LedgerPage{first, later} {
			if !reflect.DeepEqual(p.Totals, wantTotals) || !reflect.DeepEqual(p.Matched, wantMatched) {
				t.Errorf("%+v: the totals are %s and the rows matched %v; want %s and %v", q, totalsText(p.Totals), p.Matched,
					totalsText(wantTotals), wantMatched)
			}
		}
	}
	// The interest that the store could not keep in cents leaves its
	// category without a sum of amounts; its fees, 0, still add up.
	p, err := r.Ledger(t.Context(), LedgerQuery{Accounts: accounts, From: "2025-06-30", To: "2025-06-30", Categories: []string{"interest"}, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Totals) != 1 || p.Totals[0].Count != 1 || p.Totals[0].AmountCents != nil || p.Totals[0].FeesCents == nil || *p.Totals[0].FeesCents != 0 {
		t.Errorf("the interest off the cent grid totals %s", totalsText(p.Totals))
	}
	p, err = r.Ledger(t.Context(), LedgerQuery{Accounts: accounts, From: "2020-01-01", To: "2020-12-31", Limit: 1})
	if err != nil || p.Totals == nil || len(p.Totals) != 0 || len(p.Rows) != 0 || p.Matched != nil {
		t.Errorf("days without rows total %v, rows %d, matched %v, %v", p.Totals, len(p.Rows), p.Matched, err)
	}
}

// totalsText returns totals as a test says them.
func totalsText(totals []CategoryTotal) string {
	sum := func(v *int64) string {
		if v == nil {
			return "null"
		}
		return strconv.FormatInt(*v, 10)
	}
	var parts []string
	for _, c := range totals {
		parts = append(parts, fmt.Sprintf("%s %d %s %s", c.Category, c.Count, sum(c.AmountCents), sum(c.FeesCents)))
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// An account marked IfWhole is read only when its api and file runs,
// merged where they meet or overlap, hold every day asked: ...135's API
// covers 2023-06-12 to 2025-06-01, ...398's History file 2024-07-01 to
// 2025-01-20 and its API from 2025-01-14 to 2026-08-03, and ...783's API
// 2025-02-03 to 2025-12-30. The coverage, waiting counts and last reads of
// every listed account come back either way, and an account not marked is
// read whatever its coverage. An account's last read is that of its
// transactions that Schwab answered, which neither a later read of another
// endpoint nor a later refusal moves.
func TestLedgerChoosesWhole(t *testing.T) {
	t.Parallel()
	path := readerCopy(t)
	db := rawDB(t, path)
	for _, q := range []string{
		`INSERT INTO api_read (endpoint, account_id, sent_at, answered_at, outcome)
			VALUES ('accounts', 1, '2026-11-01T00:00:00.000Z', '2026-11-01T00:00:01.000Z', 'answered')`,
		`INSERT INTO api_read (endpoint, account_id, window_start, window_end, sent_at, answered_at, outcome, http_status)
			VALUES ('transactions', 2, '2026-01-01T05:00:00.000Z', '2026-02-01T05:00:00.000Z', '2026-11-02T00:00:00.000Z',
			'2026-11-02T00:00:01.000Z', 'refused', 400)`,
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	r := openReader(t, path)
	accounts, ids := readerAccounts(t, r, true)
	for _, tc := range []struct {
		from, to nytime.Date
		read     []string
	}{
		{"2024-08-01", "2025-05-31", []string{individual, options}},
		{"2024-06-30", "2024-12-31", []string{individual}},
		{"2024-07-01", "2025-01-20", []string{individual, options}},
		{"2025-02-03", "2025-05-31", []string{individual, options, empty}},
		{"2025-06-01", "2025-06-02", []string{options, empty}},
		{"2025-12-30", "2025-12-31", []string{options}},
		{"2026-08-03", "2026-08-04", nil},
	} {
		p, err := r.Ledger(t.Context(), LedgerQuery{Accounts: accounts, From: tc.from, To: tc.to, All: true, Limit: 500})
		if err != nil {
			t.Fatal(err)
		}
		var want []int64
		for _, n := range tc.read {
			want = append(want, ids[n])
		}
		if !slices.Equal(p.Read, want) {
			t.Errorf("%s to %s: read %v, want %v", tc.from, tc.to, p.Read, want)
		}
		for _, x := range p.Rows {
			if !slices.Contains(want, x.AccountID) {
				t.Errorf("%s to %s: a row of A%d, which was not read", tc.from, tc.to, x.AccountID)
			}
		}
		if len(p.Waiting) != 3 || len(p.Reads) != 3 {
			t.Errorf("%s to %s: the waiting counts of %d accounts and the reads of %d, want 3 of each", tc.from, tc.to, len(p.Waiting), len(p.Reads))
		}
		for _, c := range p.Coverage {
			if c.To < tc.from || c.From > tc.to {
				t.Errorf("%s to %s: a run that does not meet the days, %+v", tc.from, tc.to, c)
			}
		}
	}

	// Each listed account's coverage that meets the days, whether read or
	// not; an account not marked IfWhole is read with none.
	accounts[2].IfWhole = false
	p, err := r.Ledger(t.Context(), LedgerQuery{Accounts: accounts, From: "2024-06-30", To: "2025-01-31", Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	want := []CoverageRun{
		{ids[individual], "api", "2023-06-12", "2025-06-01"},
		{ids[options], "api", "2025-01-14", "2026-08-03"},
		{ids[options], "file", "2024-07-01", "2025-01-20"},
	}
	if !reflect.DeepEqual(p.Coverage, want) || !slices.Equal(p.Read, []int64{ids[individual], ids[empty]}) {
		t.Errorf("the coverage is %+v and read %v; want %+v and the first and last accounts", p.Coverage, p.Read, want)
	}
	wantReads := []AccountRead{
		{ids[individual], day(2025, 6, 3).Add(time.Second)},
		{ids[options], day(2026, 8, 5).Add(time.Second)},
		{ids[empty], day(2026, 1, 2).Add(time.Second)},
	}
	if !slices.EqualFunc(p.Reads, wantReads, func(a, b AccountRead) bool { return a.AccountID == b.AccountID && a.AnsweredAt.Equal(b.AnsweredAt) }) {
		t.Errorf("the last reads are %+v, want %+v", p.Reads, wantReads)
	}
}

// The store's whole history is its api and file runs, merged where they
// meet or overlap: other sources, and other accounts' runs, hold none of it.
func TestWhole(t *testing.T) {
	t.Parallel()
	runs := []CoverageRun{
		{1, "file", "2024-01-01", "2024-03-31"},
		{1, "api", "2024-04-01", "2024-06-30"},
		{1, "api", "2024-06-15", "2024-08-31"},
		{1, "api", "2024-09-02", "2024-12-31"},
		{1, "api_file", "2024-09-01", "2024-09-01"},
		{1, "file_unknown", "2023-01-01", "2023-12-31"},
		{2, "api", "2023-01-01", "2023-12-31"},
	}
	for _, tc := range []struct {
		from, to nytime.Date
		want     bool
	}{
		{"2024-01-01", "2024-08-31", true},
		{"2024-03-31", "2024-04-01", true},
		{"2024-02-01", "2024-02-01", true},
		{"2024-08-31", "2024-09-02", false},
		{"2024-09-02", "2024-12-31", true},
		{"2023-12-31", "2024-01-01", false},
		{"2023-06-01", "2023-06-30", false},
	} {
		if got := whole(runs, 1, tc.from, tc.to); got != tc.want {
			t.Errorf("%s to %s: whole %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// Reader.Waiting counts what Store.Status counts, account by account, and,
// with what waits of no account, of every account: here the snapshot whose
// number could not be read. Ledger's waiting counts are Status's of the
// account too, but for its held-out and uncertain rows, of the days asked.
// ...398's History export is flagged twice as maybe filtered, which counts
// once.
func TestWaitingAgreesWithStatus(t *testing.T) {
	t.Parallel()
	path := readerCopy(t)
	db := rawDB(t, path)
	if _, err := db.ExecContext(t.Context(), `INSERT INTO flag (file_id, code)
		SELECT i.file_id, c.value FROM file_fields i JOIN account a ON a.account_id = i.account_id, json_each('["lacks_rows", "looks_filtered"]') c
		WHERE a.number = ? AND i.layout <> 'snapshots'`, options); err != nil {
		t.Fatal(err)
	}
	db.Close()
	r := openReader(t, path)
	s := openStoreAt(t, path)
	ctx := t.Context()
	waits, err := r.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	counts := func(st Status) [4]int { return [4]int{st.Uncertain, st.HeldOut, st.Attention, st.Filtered} }
	statusOf := map[int64]Status{}
	var sum [4]int
	for _, w := range waits {
		got := [4]int{w.Uncertain, w.HeldOut, w.Attention, w.Filtered}
		for i := range sum {
			sum[i] += got[i]
		}
		if w.AccountID == 0 {
			if got != [4]int{0, 0, 2, 0} {
				t.Errorf("what waits of no account is %v, want the snapshot's two flags", got)
			}
			continue
		}
		st, err := s.Status(ctx, StatusOptions{Account: "A" + strconv.FormatInt(w.AccountID, 10)})
		if err != nil {
			t.Fatal(err)
		}
		statusOf[w.AccountID] = st
		if counts(st) != got {
			t.Errorf("A%d waits %v, as Status counts it %v", w.AccountID, got, counts(st))
		}
	}
	st, err := s.Status(ctx, StatusOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if counts(st) != sum || len(waits) != 4 || waits[3].AccountID != 0 {
		t.Errorf("the accounts wait %v in all, %+v; Status counts %v of every account", sum, waits, counts(st))
	}

	accounts, ids := readerAccounts(t, r, false)
	if i := slices.IndexFunc(waits, func(w AccountWaiting) bool { return w.AccountID == ids[options] }); i < 0 || waits[i].Filtered != 1 {
		t.Errorf("...398 waits %+v, want its one file that may be filtered", waits)
	}
	for _, tc := range []struct {
		from, to nytime.Date
		heldOut  int
	}{{"2025-01-01", "2025-01-31", 1}, {"2025-01-17", "2025-12-31", 0}, {"2025-01-16", "2025-01-16", 1}} {
		p, err := r.Ledger(ctx, LedgerQuery{Accounts: accounts, From: tc.from, To: tc.to, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for i, w := range p.Waiting {
			st, ok := statusOf[w.AccountID]
			if !ok {
				t.Fatalf("%s to %s: the waiting counts of A%d, which Waiting has none of", tc.from, tc.to, w.AccountID)
			}
			held := 0
			if w.AccountID == ids[options] {
				held = tc.heldOut
			}
			if w.AccountID != accounts[i].ID || w.HeldOut != held || w.Uncertain != 0 || w.Attention != st.Attention || w.Filtered != st.Filtered {
				t.Errorf("%s to %s: A%d waits %+v; Status counts %v of every day", tc.from, tc.to, w.AccountID, w, counts(st))
			}
		}
	}
}

// planOf returns what SQLite plans for query, each step's detail.
func planOf(t *testing.T, r *Reader, query string, args ...any) []string {
	t.Helper()
	rows, err := r.s.r.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatalf("planning %s: %v", query, err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}

// A page searches each read account's entries by the entry_day index,
// rather than scanning every entry, and step 1 runs none of the entries
// view's correlated subqueries; no step materializes a view. Were a later
// SQLite to stop searching the index through json_each, the queries would
// need the accounts as placeholders instead.
func TestLedgerPlan(t *testing.T) {
	t.Parallel()
	r := openReader(t, readerCopy(t))
	accounts := `[[1,"A"],[2,"B"]]`
	filters := func(q LedgerQuery) []any { return append([]any{accounts}, ledgerArgs(q)...) }
	plain := LedgerQuery{From: "2023-01-01", To: "2026-12-31"}
	filtered := LedgerQuery{From: "2023-01-01", To: "2026-12-31", Categories: []string{"dividend"}, Types: []string{"TRADE"}, Symbol: "XYZ", All: true}
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		step1 bool
	}{
		{"step 1", ledgerKeysQuery, append(filters(plain), nil, nil, nil, nil, 4), true},
		{"step 1 with a cursor and filters", ledgerKeysQuery, append(filters(filtered), "2025-03-10", "2025-03-10T19:30:00.000Z", "T5550001", "B", 4), true},
		{"step 2", ledgerRowsQuery, []any{`[[1,"T5550001"],[2,"T5550001"]]`}, false},
		{"step 3", ledgerMatchQuery, filters(filtered), false},
	} {
		plan := planOf(t, r, tc.query, tc.args...)
		joined := strings.Join(plan, "\n")
		if strings.Contains(joined, "MATERIALIZE") {
			t.Errorf("%s materializes a view:\n%s", tc.name, joined)
		}
		if tc.name == "step 2" {
			if !strings.Contains(joined, "SEARCH e USING INDEX sqlite_autoindex_entry_3 (account_id=? AND ref=?)") {
				t.Errorf("%s does not search the entries by account and ref:\n%s", tc.name, joined)
			}
			continue
		}
		if !strings.Contains(joined, "SEARCH e USING INDEX entry_day (account_id=? AND day>? AND day<?)") ||
			slices.ContainsFunc(plan, func(p string) bool { return strings.HasPrefix(p, "SCAN e") }) {
			t.Errorf("%s does not search entry_day by account and day:\n%s", tc.name, joined)
		}
		if tc.step1 && strings.Contains(joined, "CORRELATED") {
			t.Errorf("%s runs a correlated subquery:\n%s", tc.name, joined)
		}
	}
}

// A ledger query that Ledger cannot answer is refused before it reads.
func TestLedgerQueryChecks(t *testing.T) {
	t.Parallel()
	r := openReader(t, copyTemplate(t))
	one := []LedgerAccount{{ID: 1, Key: "A1"}}
	ok := LedgerQuery{Accounts: one, From: "2025-01-01", To: "2025-12-31", Limit: 1}
	for _, tc := range []struct {
		name string
		edit func(q *LedgerQuery)
		want string
	}{
		{"no account", func(q *LedgerQuery) { q.Accounts = nil }, "no account to read"},
		{"an account twice", func(q *LedgerQuery) { q.Accounts = []LedgerAccount{{1, "A1", false}, {1, "A2", false}} }, "account 1 is listed twice"},
		{"a key twice", func(q *LedgerQuery) { q.Accounts = []LedgerAccount{{1, "A1", false}, {2, "A1", false}} }, "account 2 is listed twice"},
		{"no key", func(q *LedgerQuery) { q.Accounts = []LedgerAccount{{ID: 1}} }, "has no key"},
		{"no date", func(q *LedgerQuery) { q.From = "" }, `"" is not a date`},
		{"a bad date", func(q *LedgerQuery) { q.To = "2025-02-30" }, `"2025-02-30" is not a date`},
		{"days that end first", func(q *LedgerQuery) { q.From, q.To = "2025-02-01", "2025-01-31" }, "end on 2025-01-31, before they start on 2025-02-01"},
		{"no row", func(q *LedgerQuery) { q.Limit = 0 }, "a page of 0 rows"},
		{"too many rows", func(q *LedgerQuery) { q.Limit = 501 }, "a page of 501 rows"},
		{"a start without its ref", func(q *LedgerQuery) { q.After = &LedgerKey{Day: "2025-03-01", Account: "A1"} }, "without its ref or account"},
	} {
		q := ok
		tc.edit(&q)
		if _, err := r.Ledger(t.Context(), q); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := r.Ledger(t.Context(), ok); err != nil {
		t.Errorf("a query that may be answered: %v", err)
	}
}

// Categories are the store's, a copy each time.
func TestCategories(t *testing.T) {
	t.Parallel()
	got := Categories()
	if !slices.Equal(got, categories) || len(got) != 12 {
		t.Errorf("Categories() = %q", got)
	}
	got[0] = "x"
	if Categories()[0] != "deposit" {
		t.Error("changing what Categories returned changed the categories")
	}
}

// fileDigest returns the SHA-256 and mode of the store's main file at path,
// and which of its -wal and -shm are beside it: a store whose -wal is gone
// is unchanged while its main file's bytes are.
func fileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	d := []string{fmt.Sprintf("%x %v", sha256.Sum256(b), st.Mode().Perm())}
	for _, j := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(path + j); err == nil {
			d = append(d, j)
		}
	}
	return strings.Join(d, " ")
}

// storeDigest returns what TestReaderNeverWrites compares of the store at
// path: its main file's SHA-256 and mode, its user_version and journal mode,
// and every table's rows in their order, read through a connection that
// takes no lock and makes no journal.
func storeDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file's format numbers say WAL (2) or rollback (1).
	var d strings.Builder
	fmt.Fprintf(&d, "%x %v format %d %d\n", sha256.Sum256(b), st.Mode().Perm(), b[18], b[19])
	db, err := sql.Open("sqlite", dsn(path, "mode=ro", "immutable=1"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&d, "user_version %d\n", version)
	rows, err := db.QueryContext(t.Context(), "SELECT name FROM sqlite_schema WHERE type = 'table' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	for _, tb := range tables {
		h := sha256.New()
		rows, err := db.QueryContext(t.Context(), `SELECT * FROM "`+tb+`"`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		n := 0
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(h, "%q\n", vals)
			n++
		}
		rows.Close()
		fmt.Fprintf(&d, "%s %d %x\n", tb, n, h.Sum(nil))
	}
	return d.String()
}

// runEveryRead runs each read of r, failing the test on an error.
func runEveryRead(t *testing.T, r *Reader) {
	t.Helper()
	ctx := t.Context()
	if _, err := r.AccountNumbers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Accounts(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Waiting(ctx); err != nil {
		t.Fatal(err)
	}
	accounts, _ := readerAccounts(t, r, true)
	q := LedgerQuery{Accounts: accounts, From: "2023-01-01", To: "2026-12-31", Categories: []string{"buy"}, Limit: 2}
	p, err := r.Ledger(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	q.After = p.Next
	if _, err := r.Ledger(ctx, q); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"T73508261946", "t5550001", "Ca0223cbd.1"} {
		if _, err := r.EntryByRef(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	_, _ = r.NewerRules(), r.Path()
}

// readerHoldsNoNumber is TestViewsHoldNoAccountNumber's case of a Reader:
// of the store s, AccountNumbers returns the numbers that s holds, and no
// other read returns a value that holds one of numbers, nor its digits with
// spaces or dashes between them.
func readerHoldsNoNumber(t *testing.T, s *Store, numbers []string) {
	t.Helper()
	r, err := OpenReader(t.Context(), ReaderOptions{Path: s.Path()})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	read := readEverything(t, r)
	var stored []string
	for _, a := range read["AccountNumbers"].([]NumberedAccount) {
		stored = append(stored, a.Number)
	}
	if want := storeStrings(t, s, "SELECT number FROM account ORDER BY account_id"); !slices.Equal(stored, want) {
		t.Errorf("AccountNumbers returns %d numbers, want the %d the store holds", len(stored), len(want))
	}
	values := 0
	for _, name := range slices.Sorted(maps.Keys(read)) {
		if name == "AccountNumbers" {
			continue
		}
		for _, v := range valuesOf(reflect.ValueOf(read[name])) {
			values++
			joined := strings.Map(func(r rune) rune {
				if separator(r) {
					return -1
				}
				return r
			}, v)
			for _, n := range numbers {
				if strings.Contains(joined, n) {
					t.Errorf("%s returns the number %s: %q", name, mask.Number(n), v)
				}
			}
		}
	}
	t.Logf("%d values of %d reads checked", values, len(read)-1)
}

// readEverything returns what each read of r returns of the whole store,
// by the read's name: every account's ledger, every entry with each of its
// refs, and the rest.
func readEverything(t *testing.T, r *Reader) map[string]any {
	t.Helper()
	ctx := t.Context()
	numbers, err := r.AccountNumbers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := r.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := r.Waiting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var listed []LedgerAccount
	for _, a := range accounts {
		listed = append(listed, LedgerAccount{ID: a.ID, Key: a.Ref})
	}
	ledger, err := r.Ledger(ctx, LedgerQuery{Accounts: listed, From: "2000-01-01", To: "2030-12-31", All: true, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(numbers) < 4 || len(accounts) != len(numbers) || len(ledger.Rows) < 20 || ledger.Next != nil {
		t.Fatalf("the reads return %d numbers, %d accounts and %d rows of the ledger, %v", len(numbers), len(accounts), len(ledger.Rows), ledger.Next)
	}
	// EntryByRef finds a ref's entries in every account. The refs are read
	// all at once, as a server's calls may be, on the Reader's four
	// connections.
	var refs []string
	for _, x := range ledger.Rows {
		if !slices.Contains(refs, x.Ref) {
			refs = append(refs, x.Ref)
		}
	}
	found := make([][]EntryDetail, len(refs))
	errs := make([]error, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Go(func() { found[i], errs[i] = r.EntryByRef(ctx, ref) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	entries := slices.Concat(found...)
	if len(entries) != len(ledger.Rows) {
		t.Errorf("EntryByRef finds %d entries of the %d rows of the ledger", len(entries), len(ledger.Rows))
	}
	return map[string]any{
		"AccountNumbers": numbers, "Accounts": accounts, "Waiting": waiting, "Ledger": ledger, "EntryByRef": entries,
	}
}

// valuesOf returns each string and number that v holds, at any depth,
// written as text.
func valuesOf(v reflect.Value) []string {
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return []string{strconv.FormatInt(v.Int(), 10)}
	case reflect.Float32, reflect.Float64:
		return []string{strconv.FormatFloat(v.Float(), 'f', -1, 64)}
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return nil
		}
		return valuesOf(v.Elem())
	case reflect.Slice, reflect.Array:
		var out []string
		for i := range v.Len() {
			out = append(out, valuesOf(v.Index(i))...)
		}
		return out
	case reflect.Struct:
		if t, ok := reflect.TypeAssert[time.Time](v); ok {
			return []string{t.Format(time.RFC3339Nano)}
		}
		var out []string
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out = append(out, valuesOf(v.Field(i))...)
			}
		}
		return out
	}
	return nil
}

// A Reader writes nothing: on a copy of readerStore closed cleanly, which
// others can read, the main file's bytes and mode, its user_version and
// journal mode and every table's rows are the same after a Reader has run
// every read and closed, and no journal is left beside it. The Reader
// warned that others can read the store and left it so.
func TestReaderNeverWrites(t *testing.T) {
	t.Parallel()
	path := readerCopy(t)
	loose := runtime.GOOS != "windows"
	if loose {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	before := storeDigest(t, path)
	var warned []string
	r, err := OpenReader(t.Context(), ReaderOptions{Path: path, Warn: func(m string) { warned = append(warned, m) }})
	if err != nil {
		t.Fatal(err)
	}
	runEveryRead(t, r)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if after := storeDigest(t, path); after != before {
		t.Errorf("the store changed:\n%s\nwas\n%s", after, before)
	}
	for _, j := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(j); err == nil {
			t.Errorf("%s is left", filepath.Base(j))
		}
	}
	if loose && (len(warned) != 1 || !strings.HasSuffix(warned[0], "history.db can be read by others; any schwabtx command that opens it, "+
		"such as schwabtx status, makes it readable only by you")) {
		t.Errorf("the reader warned %q", warned)
	}
}

// A Reader makes no store: a missing store, in a missing folder or not, or
// an empty file, is ErrNoStore, and the folder and the file stay as they
// were; and when the store's files go after OpenReader, a read through a
// fresh connection of the pool fails, and makes no file at the path.
func TestReaderRefusesMissing(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	dir := t.TempDir()
	missing := []string{filepath.Join(dir, "none", "history.db"), filepath.Join(dir, "history.db")}
	for _, path := range missing {
		if _, err := OpenReader(ctx, ReaderOptions{Path: path}); !errors.Is(err, ErrNoStore) {
			t.Errorf("a missing store: %v, want ErrNoStore", err)
		}
	}
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReader(ctx, ReaderOptions{Path: empty}); !errors.Is(err, ErrNoStore) {
		t.Errorf("an empty file: %v, want ErrNoStore", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "empty.db" {
		t.Errorf("the folder holds %v, want the empty file alone", entries)
	}
	if st, err := os.Stat(empty); err != nil || st.Size() != 0 {
		t.Errorf("the empty file is now %v, %v", st, err)
	}

	path := copyTemplate(t)
	r := openReader(t, path)
	if _, err := r.Accounts(ctx); err != nil {
		t.Fatal(err)
	}
	// The pool's idle connections close, the last taking the -wal and -shm
	// with it, and the store's database goes too.
	r.s.r.SetMaxIdleConns(0)
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if _, err := r.Accounts(ctx); err == nil {
		t.Error("a read of a store whose files are gone worked")
	}
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(f); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the read made %s: %v", filepath.Base(f), err)
		}
	}
}

// A store moved away while it is being opened, after checkPath saw it and
// before identify reads it, leaves no store at the path. Its -wal, which a
// killed writer left and which a read-only connection cannot recover, makes
// identify open the path again to recover it, and that open makes no file
// in the store's place, of which SQLite would take the -wal: the moved
// store keeps what the writer committed. So it is when OpenReader opens,
// and when Open does. Left in place, the store is recovered, and a Reader
// reads what the writer committed.
func TestIdentifyMakesNoStoreInAMovedOnesPlace(t *testing.T) {
	t.Parallel()
	path := copyTemplate(t)
	c := startChild(t, "commit", "HISTORY_TEST_STORE="+path)
	c.expect(t, "committed")
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.cmd.Wait()
	// More stores as the writer left them.
	copyStore := func() string {
		to := filepath.Join(t.TempDir(), "history.db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			b, err := os.ReadFile(path + suffix)
			if err != nil {
				t.Fatalf("the killed writer left no %s: %v", filepath.Base(path+suffix), err)
			}
			if err := os.WriteFile(to+suffix, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return to
	}
	inPlace, second := copyStore(), copyStore()

	r := openReader(t, inPlace)
	numbers, err := r.AccountNumbers(t.Context())
	if err != nil || !slices.ContainsFunc(numbers, func(a NumberedAccount) bool { return a.Number == "committed" }) {
		t.Errorf("in place, a Reader reads the accounts %+v, %v; want the one that the killed writer committed", numbers, err)
	}

	for _, tc := range []struct {
		name      string
		path      string
		mustExist bool
	}{
		{"OpenReader", path, true},
		{"Open", second, false},
	} {
		s := &Store{path: tc.path, o: Options{
			Path: tc.path, MustExist: tc.mustExist, Now: fixedNow, Warn: func(string) {}, Waiting: func(string) {},
			BusyTimeout: time.Second, LockWait: time.Second, disk: localDisk,
		}}
		if err := s.checkPath(); err != nil {
			t.Fatalf("%s: checkPath: %v", tc.name, err)
		}
		away := filepath.Join(t.TempDir(), "history.db")
		if err := os.Rename(tc.path, away); err != nil {
			t.Fatal(err)
		}
		if err := s.identify(t.Context()); err == nil {
			t.Errorf("%s: identify found a store at the path that it was moved from", tc.name)
		}
		if _, err := os.Lstat(tc.path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: identify made %s: %v", tc.name, filepath.Base(tc.path), err)
		}
		// The -wal and -shm follow the database, as the store's advice for
		// a damaged store moves them, and the store holds the writer's
		// account.
		for _, suffix := range []string{"-wal", "-shm"} {
			if err := os.Rename(tc.path+suffix, away+suffix); err != nil {
				t.Fatalf("%s: moving the %s: %v", tc.name, suffix, err)
			}
		}
		if n := queryInt(t, rawDB(t, away), "SELECT count(*) FROM account WHERE number = 'committed'"); n != 1 {
			t.Errorf("%s: the moved store holds %d accounts that the killed writer committed, want 1", tc.name, n)
		}
	}
}

// A store that a newer schwabtx derived with newer rules is read, and
// NewerRules says so, also of a Reader opened before the rules were raised,
// from its next read on; one that a newer schwabtx migrated to a newer
// schema is ErrNewer, at OpenReader and at the next read of a Reader opened
// before. The stores are copies of the template, which hold no rows, so
// that what the newer schwabtx derives again costs it nothing: a Reader
// reads the versions alike of any store.
func TestReaderNewer(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := copyTemplate(t)
	r := openReader(t, path)
	if _, err := r.Accounts(ctx); err != nil || r.NewerRules() {
		t.Errorf("a store of this build's rules: %v, newer rules %v", err, r.NewerRules())
	}
	c := startChild(t, "update", "HISTORY_TEST_STORE="+path, "HISTORY_TEST_RAISE=rules")
	c.expect(t, "opened")
	c.rest(t)
	if _, err := r.Waiting(ctx); err != nil || !r.NewerRules() {
		t.Errorf("after a newer schwabtx derived the store: %v, newer rules %v", err, r.NewerRules())
	}
	later := openReader(t, path)
	if !later.NewerRules() {
		t.Error("a Reader opened on a store of newer rules does not say so")
	}
	if accounts, err := later.Accounts(ctx); err != nil || accounts != nil || !later.NewerRules() {
		t.Errorf("a store of newer rules reads %v, %v, newer rules %v", accounts, err, later.NewerRules())
	}

	path = copyTemplate(t)
	r = openReader(t, path)
	c = startChild(t, "migrate", "HISTORY_TEST_STORE="+path)
	c.expect(t, "opened")
	c.rest(t)
	if _, err := r.Accounts(ctx); !errors.Is(err, ErrNewer) {
		t.Errorf("a read of a store migrated meanwhile: %v, want ErrNewer", err)
	}
	if _, err := OpenReader(ctx, ReaderOptions{Path: path}); !errors.Is(err, ErrNewer) || !strings.Contains(err.Error(), "written by a newer schwabtx") {
		t.Errorf("a store of a newer schema: %v, want ErrNewer", err)
	}
}

// A store that an older schwabtx last wrote, of an older schema, rules or
// calendar, is ErrNeedsUpdate to a Reader of a build with newer ones, and a
// store not in WAL mode is ErrNeedsUpdate to any; the Reader changes
// neither: the main file's bytes and mode stay, and no journal is left.
// The stores are copies of the template: a Reader reads the versions and
// the journal mode alike of any store.
func TestReaderNeedsUpdate(t *testing.T) {
	t.Parallel()
	for _, raise := range []string{"schema", "rules", "calendar"} {
		t.Run(raise, func(t *testing.T) {
			t.Parallel()
			path := copyTemplate(t)
			before := fileDigest(t, path)
			c := startChild(t, "read", "HISTORY_TEST_STORE="+path, "HISTORY_TEST_RAISE="+raise)
			if got := c.expect(t, "read"); got != "read needs_update" {
				t.Errorf("a build of newer %s reads the store: %q", raise, got)
			}
			c.rest(t)
			if after := fileDigest(t, path); after != before {
				t.Errorf("the reader changed the store:\n%s\nwas\n%s", after, before)
			}
		})
	}
	t.Run("older after OpenReader", func(t *testing.T) {
		t.Parallel()
		path := copyTemplate(t)
		r := openReader(t, path)
		db := rawDB(t, path)
		if _, err := db.ExecContext(t.Context(), "PRAGMA user_version = 0"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Accounts(t.Context()); !errors.Is(err, ErrNeedsUpdate) || !strings.Contains(err.Error(), "schema 0, rules 0, calendar 0") {
			t.Errorf("a read of a store made older: %v, want ErrNeedsUpdate", err)
		}
	})
	t.Run("not WAL", func(t *testing.T) {
		t.Parallel()
		path := copyTemplate(t)
		db := rawDB(t, path)
		var mode string
		if err := db.QueryRowContext(t.Context(), "PRAGMA journal_mode = DELETE").Scan(&mode); err != nil || mode != "delete" {
			t.Fatalf("journal mode %q, %v", mode, err)
		}
		db.Close()
		before := fileDigest(t, path)
		_, err := OpenReader(t.Context(), ReaderOptions{Path: path})
		if !errors.Is(err, ErrNeedsUpdate) || !strings.Contains(err.Error(), "is not in WAL mode (journal mode delete)") {
			t.Errorf("a store not in WAL mode: %v, want ErrNeedsUpdate", err)
		}
		if after := fileDigest(t, path); after != before {
			t.Errorf("the reader changed the store:\n%s\nwas\n%s", after, before)
		}
	})
}

// EntryByRef finds each entry of a ref, in any case, by account, with its
// legs, as the legs view gives them, an option's details included, and its
// flags with their severity and detail; an API row has its status, a file
// row none. A ref that names no entry finds none.
func TestEntryByRef(t *testing.T) {
	t.Parallel()
	r := openReader(t, readerCopy(t))
	_, ids := readerAccounts(t, r, false)
	ctx := t.Context()
	both, err := r.EntryByRef(ctx, " t5550001 ")
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 || both[0].Entry.AccountID != ids[individual] || both[1].Entry.AccountID != ids[options] ||
		both[0].Entry.PairRef != "T5550001" || both[0].Entry.PairAccountID != ids[options] || both[0].Entry.Status != "VALID" ||
		len(both[0].Legs) != 1 || both[0].Legs[0].AssetType != "CURRENCY" || *both[0].Legs[0].CashCents != -10000 {
		t.Errorf("the journal's two sides are %+v", both)
	}
	held, err := r.EntryByRef(ctx, "Ca0223cbd.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].Entry.Status != "" || held[0].Entry.Source != "file" || held[0].Entry.Counted ||
		!reflect.DeepEqual(held[0].Flags, []FlagRow{{"held_out", "attention", ""}}) || len(held[0].Legs) != 1 || *held[0].Legs[0].CashCents != 400 {
		t.Errorf("the held-out file row is %+v", held)
	}
	option, err := r.EntryByRef(ctx, "T73508261944")
	if err != nil {
		t.Fatal(err)
	}
	if len(option) != 1 || len(option[0].Legs) != 5 {
		t.Fatalf("the put's entry is %+v", option)
	}
	put := option[0].Legs[4]
	if put.Leg != 4 || put.AssetType != "OPTION" || put.PutCall != "PUT" || put.Strike == nil || *put.Strike != 40 ||
		put.ExpirationDay != "2025-02-21" || *put.QuantityMicros != 2_000_000 || put.UnderlyingSymbol != "XYZ" {
		t.Errorf("the put's leg is %+v", put)
	}
	if none, err := r.EntryByRef(ctx, "T73508261999"); err != nil || len(none) != 0 {
		t.Errorf("a ref of no entry finds %+v, %v", none, err)
	}
}

// The rules' flags: those that apiEntryFields raises, which DeriveAPIEntries
// gives too.
var rulesFlags = []string{
	"problems", "inexact", "rounded", "no_activity_id", "activity_id_text", "account_mismatch", "duplicate_in_answer",
	"unknown_type", "unknown_wording", "awaits_probe", "unexpected_sign", "mixed_legs", "no_security_leg", "zero_quantity",
	"zero_amount", "legs_mismatch", "no_legs", "status",
}

// entryText returns what DeriveAPIEntries gives of an entry, as a test
// compares it: e, or the store's x of it, with its category unless
// paired says the store paired it.
func entryText(ref string, day nytime.Date, category, rule, symbol string, amount, fees, gross, quantity *int64, price *float64, flags []string) string {
	n := func(v *int64) string {
		if v == nil {
			return "null"
		}
		return strconv.FormatInt(*v, 10)
	}
	p := "null"
	if price != nil {
		p = strconv.FormatFloat(*price, 'g', -1, 64)
	}
	var kept []string
	for _, f := range flags {
		if slices.Contains(rulesFlags, f) {
			kept = append(kept, f)
		}
	}
	return fmt.Sprintf("%s %s %s %s %q amount %s fees %s gross %s quantity %s price %s flags %q",
		ref, day, category, rule, symbol, n(amount), n(fees), n(gross), n(quantity), p, kept)
}

// DeriveAPIEntries gives each row of a read what the store derived for it,
// wherever no pair applies: its ref, day, category, rule, symbol, amount,
// fees, gross amount, quantity, price and the rules' flags. readerStore's
// reads hold the fake's fixture transactions, two rows without an activity
// ID of one time and cut form, refs of their own, an activity ID sent
// twice, one entry for both, and a row that names another account's
// number, flagged so. A read without its raw JSON, or without the
// account's number, is refused.
func TestDeriveAPIEntriesMatchesLedger(t *testing.T) {
	t.Parallel()
	r := openReader(t, readerCopy(t))
	accounts, ids := readerAccounts(t, r, false)
	p, err := r.Ledger(t.Context(), LedgerQuery{Accounts: accounts, From: "2023-01-01", To: "2026-12-31", All: true, Limit: 500})
	if err != nil || p.Next != nil {
		t.Fatalf("the entries: %v, a page after %+v", err, p)
	}
	stored := map[string]LedgerRow{}
	for _, x := range p.Rows {
		stored[rowRef(x.Ref, x.AccountID)] = x
	}
	compared, total := 0, 0
	for _, rd := range readerReads {
		got, err := DeriveAPIEntries(rd.Rows, rd.Account)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(rd.Rows) {
			t.Fatalf("%d entries of %d rows", len(got), len(rd.Rows))
		}
		total += len(rd.Rows)
		for row, e := range got {
			x, ok := stored[rowRef(e.Ref, ids[rd.Account])]
			if !ok {
				t.Errorf("row %d of %s: no entry %s in the store", row, mask.Number(rd.Account), e.Ref)
				continue
			}
			category := x.Category
			if x.PairKind != "" {
				category = e.Category // pairing books a journal as a journal
			}
			want := entryText(x.Ref, x.Day, category, x.Rule, x.Symbol, x.AmountCents, x.FeesCents, x.GrossCents, x.QuantityMicros, x.Price, x.Flags)
			if g := entryText(e.Ref, e.Day, e.Category, e.Rule, e.Symbol, e.AmountCents, e.FeesCents, e.GrossCents, e.QuantityMicros, e.Price, e.Flags); g != want {
				t.Errorf("DeriveAPIEntries gives\n%s\nthe store holds\n%s", g, want)
			}
			compared++
		}
	}
	if compared != total || total < 20 {
		t.Errorf("%d rows compared of %d, want every row of the reads", compared, total)
	}

	// The rows made for the test come out as the store names and flags them.
	empty := readerReads[2]
	got, err := DeriveAPIEntries(empty.Rows, empty.Account)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got[0].Ref, "T~20250501T011844.") || got[1].Ref != got[0].Ref+".2" ||
		got[2].Ref != "T7770001" || !reflect.DeepEqual(got[2], got[3]) || !slices.Contains(got[2].Flags, "duplicate_in_answer") ||
		got[2].AmountCents == nil || *got[2].AmountCents != 1243 ||
		!slices.Contains(got[4].Flags, "account_mismatch") || got[5].AmountCents != nil {
		t.Errorf("the made rows derive %+v", got)
	}
	if _, err := DeriveAPIEntries(empty.Rows, ""); err == nil {
		t.Error("a read of no account's number derived")
	}
	rows := slices.Clone(empty.Rows)
	rows[1].Raw = nil
	if _, err := DeriveAPIEntries(rows, empty.Account); err == nil || !strings.Contains(err.Error(), "row 2 has no raw JSON") {
		t.Errorf("a row without its raw JSON: %v", err)
	}
}

// DeriveAPIEntries and apiEntries make the same fields of the same
// transactions, the rules' flags and the ref included, without SQLite: so
// a drift between them shows in a pure test too. Rows of one activity ID
// are one transaction, as the store keeps them.
func TestDeriveAPIEntriesMatchesAPIEntries(t *testing.T) {
	t.Parallel()
	needReaderStore(t)
	compared := 0
	for _, rd := range readerReads {
		got, err := DeriveAPIEntries(rd.Rows, rd.Account)
		if err != nil {
			t.Fatal(err)
		}
		var in accountInputs
		in.account = 1
		seen := map[int64]int{}
		for i, row := range rd.Rows {
			txn := recordInput(t, int64(i+1), row)
			if row.AccountNumber != rd.Account {
				txn.numberSent = mask.Number(row.AccountNumber)
			}
			if j, ok := seen[row.ActivityID]; ok && row.ActivityID != 0 {
				txn.id, txn.duplicate = in.txns[j].id, true
				in.txns[j] = txn
				continue
			}
			seen[row.ActivityID] = len(in.txns)
			in.txns = append(in.txns, txn)
		}
		entries := apiEntries(in, nil, nil, nil)
		byRef := map[string]ledgerEntry{}
		for _, e := range entries {
			byRef[e.ref] = e
		}
		for _, e := range got {
			x, ok := byRef[e.Ref]
			if !ok {
				t.Errorf("apiEntries makes no entry %s", e.Ref)
				continue
			}
			compared++
			var codes []string
			for _, f := range x.flags.sorted() {
				codes = append(codes, f.code)
			}
			want := entryText(x.ref, nytime.Date(x.day.String), x.category, x.rule, x.symbol.String, nullInt(x.amount), nullInt(x.fees),
				nullInt(x.gross), nullInt(x.quantity), nullFloat(x.price), codes)
			if g := entryText(e.Ref, e.Day, e.Category, e.Rule, e.Symbol, e.AmountCents, e.FeesCents, e.GrossCents, e.QuantityMicros, e.Price, e.Flags); g != want {
				t.Errorf("DeriveAPIEntries gives\n%s\napiEntries makes\n%s", g, want)
			}
		}
	}
	if compared < 20 {
		t.Errorf("%d entries compared, want every row of the reads", compared)
	}
}
