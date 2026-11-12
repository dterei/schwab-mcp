package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The fixtures' accounts that hold transactions.
const (
	individual = "29470135"
	options    = "61803398"
)

// fixtureRows returns the rows of transactions.json of the account whose
// number is number, as the client decodes them, each with its Raw JSON.
func fixtureRows(t *testing.T, number string) []schwab.Transaction {
	t.Helper()
	var all []schwab.Transaction
	if err := json.Unmarshal(schwabtest.Fixture(t, "transactions.json"), &all); err != nil {
		t.Fatal(err)
	}
	var rows []schwab.Transaction
	for _, r := range all {
		if r.AccountNumber == number {
			rows = append(rows, r)
		}
	}
	return rows
}

// decodeRow returns raw as the client decodes a row.
func decodeRow(t *testing.T, raw string) schwab.Transaction {
	t.Helper()
	var r schwab.Transaction
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Raw) == 0 {
		t.Fatalf("no raw JSON from %s", raw)
	}
	return r
}

// schwabTime writes t as Schwab writes a time.
func schwabTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05-0700")
}

// cashRaw returns the raw JSON of a dividend of $1 in the fixtures'
// individual account, with activity ID id (none when 0), at time at and
// booked on that day.
func cashRaw(id int64, at time.Time) string {
	trade := midnight(nytime.DateOf(at))
	activity := ""
	if id != 0 {
		activity = fmt.Sprintf(`"activityId": %d, `, id)
	}
	return fmt.Sprintf(`{%s"time": %q, "description": "Ordinary Dividend~XYZ", "accountNumber": %q,
		"type": "DIVIDEND_OR_INTEREST", "status": "VALID", "subAccount": "CASH", "tradeDate": %q,
		"settlementDate": %q, "netAmount": 1.0, "transferItems": [{"instrument": {"assetType": "CURRENCY",
		"status": "ACTIVE", "symbol": "CURRENCY_USD", "description": "USD currency", "instrumentId": 1,
		"closingPrice": 0.0}, "amount": 1.0, "cost": 0.0}]}`,
		activity, schwabTime(at), individual, schwabTime(trade), schwabTime(trade))
}

// cashRow returns cashRaw's row, decoded.
func cashRow(t *testing.T, id int64, at time.Time) schwab.Transaction {
	t.Helper()
	return decodeRow(t, cashRaw(id, at))
}

// listRead returns a read of number's transactions of every type over the
// window from start to end, sent at sent and answered a second later.
func listRead(number string, start, end, sent time.Time, rows ...schwab.Transaction) TransactionsRead {
	return TransactionsRead{
		Read:    Read{SentAt: sent, AnsweredAt: sent.Add(time.Second)},
		Account: number,
		Query:   schwab.TransactionQuery{Start: start, End: end},
		Rows:    rows,
	}
}

// record records r and fails the test if it is refused.
func record(t *testing.T, s *Store, r TransactionsRead) Recorded {
	t.Helper()
	rec, err := s.RecordTransactions(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// storeInt returns the integer that query gives on the store's readers,
// or -1 for NULL.
func storeInt(t *testing.T, s *Store, query string, args ...any) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := s.r.QueryRowContext(t.Context(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if !n.Valid {
		return -1
	}
	return n.Int64
}

// storeStrings returns the first column of each row that query gives, ""
// for NULL.
func storeStrings(t *testing.T, s *Store, query string) []string {
	t.Helper()
	rows, err := s.r.QueryContext(t.Context(), query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The fixtures' 14 rows, read as one complete read of each of their two
// accounts, are 14 new transactions, each with one revision and its
// columns and legs; read again, they are the same 14, with no revision
// added, and the by-id endpoint's answer for one of them, whose bytes
// differ, is the same too.
func TestRecordFixtures(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	start := time.Date(2023, 1, 1, 5, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	total := 0
	for i, number := range []string{individual, options} {
		rows := fixtureRows(t, number)
		total += len(rows)
		rec := record(t, s, listRead(number, start, end, sent.Add(time.Duration(i)*time.Minute), rows...))
		if rec.New != len(rows) || rec.Same+rec.Revised+rec.Back+rec.Gone != 0 {
			t.Errorf("first read of %s: %+v, want %d new", number, rec, len(rows))
		}
	}
	if total != 14 {
		t.Fatalf("the fixtures hold %d rows, want 14", total)
	}
	for i, number := range []string{individual, options} {
		rows := fixtureRows(t, number)
		rec := record(t, s, listRead(number, start, end, sent.Add(time.Hour+time.Duration(i)*time.Minute), rows...))
		if rec.Same != len(rows) || rec.New+rec.Revised+rec.Back+rec.Gone != 0 {
			t.Errorf("second read of %s: %+v, want %d the same", number, rec, len(rows))
		}
	}
	byID := decodeRow(t, string(schwabtest.Fixture(t, "transaction.json")))
	rec, err := s.RecordTransaction(t.Context(), TransactionRead{
		Read:    Read{SentAt: sent.Add(2 * time.Hour), AnsweredAt: sent.Add(2 * time.Hour)},
		Account: individual, ActivityID: byID.ActivityID, Row: byID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Same != 1 || rec.New+rec.Revised != 0 {
		t.Errorf("the by-id read: %+v, want the same", rec)
	}
	for query, want := range map[string]int64{
		"SELECT count(*) FROM api_txn":                                                  14,
		"SELECT count(*) FROM txn":                                                      14,
		"SELECT count(*) FROM api_revision":                                             14,
		"SELECT count(*) FROM revision_cut WHERE changed IS NULL":                       14,
		"SELECT count(*) FROM txn WHERE revisions = 1 AND gone_read_id IS NULL":         14,
		"SELECT count(*) FROM api_leg":                                                  38,
		"SELECT count(*) FROM api_read WHERE complete = 1 AND all_types = 1":            4,
		"SELECT count(*) FROM api_read WHERE endpoint = 'transaction' AND complete = 0": 1,
		"SELECT count(*) FROM api_absent":                                               0,
		"SELECT count(*) FROM account":                                                  2,
		// The by-id answer is the latest raw JSON of its transaction.
		"SELECT count(*) FROM api_txn t JOIN api_read r ON r.read_id = t.latest_read_id WHERE r.endpoint = 'transaction'": 1,
		"SELECT latest_list_read_id < latest_read_id FROM api_txn WHERE activity_id = 73508261946":                        1,
		"SELECT net_cents FROM txn WHERE activity_id = 73508261946":                                                       -270,
		"SELECT net_cents FROM txn WHERE activity_id = 73508261951":                                                       1000000,
		"SELECT position_id FROM txn WHERE activity_id = 73508261946":                                                     3926184811,
		"SELECT qualified_dividend FROM txn WHERE activity_id = 73508261948":                                              0,
		"SELECT qualified_dividend FROM txn WHERE activity_id = 73508261949":                                              -1,
		"SELECT amount_micros FROM api_leg l JOIN txn x USING (txn_id) WHERE x.activity_id = 73508261946":                 37800,
		"SELECT amount_cents FROM api_leg l JOIN txn x USING (txn_id) WHERE x.activity_id = 73508261951":                  1000000,
		// A trade's fee legs carry no price; its security leg and a cash
		// row's one leg do, though it is 0.
		"SELECT group_concat(price IS NULL, '') FROM api_leg l JOIN txn x USING (txn_id) WHERE x.activity_id = 73508261941": 11110,
		"SELECT price FROM api_leg l JOIN txn x USING (txn_id) WHERE x.activity_id = 73508261951":                           0,
	} {
		if got := storeInt(t, s, query); got != want {
			t.Errorf("%s: %d, want %d", query, got, want)
		}
	}
	for query, want := range map[string]string{
		"SELECT trade_day FROM txn WHERE activity_id = 73508261949":                        "2024-04-30", // posted at 01:18 UTC on May 1
		"SELECT time FROM txn WHERE activity_id = 73508261949":                             "2024-05-01T01:18:44.000Z",
		"SELECT description FROM txn WHERE activity_id = 73508261954":                      "W-8 Withholding~BND",
		"SELECT settlement_day FROM txn WHERE activity_id = 73508261946":                   "2024-04-08",
		"SELECT coalesce(settlement_day, 'none') FROM txn WHERE activity_id = 73508261941": "none",
		"SELECT problems FROM txn WHERE activity_id = 73508261946":                         "[]",
		"SELECT types FROM api_read WHERE read_id = 1":                                     strings.Join(typeNames(schwab.AllTransactionTypes()), ","),
	} {
		if got := storeStrings(t, s, query); len(got) != 1 || got[0] != want {
			t.Errorf("%s: %q, want %q", query, got, want)
		}
	}
}

// typeNames returns the names of types.
func typeNames(types []schwab.TransactionType) []string {
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return names
}

// A changed status makes a revision, which keeps its raw JSON and names the
// key path that changed; changing it back makes a third. Another
// closingPrice, or -2.7 written as -2.70, makes none, though the latest raw JSON
// moves to it; a new key makes one.
func TestRevisions(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	start := time.Date(2024, 4, 1, 4, 0, 0, 0, time.UTC)
	end := time.Date(2024, 4, 10, 4, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	valid := listRow(t)
	pending := replaceOnce(t, valid, `"status": "VALID"`, `"status": "PENDING"`)
	priced := replaceOnce(t, valid, `"closingPrice": 71.59`, `"closingPrice": 71.5`)
	zero := replaceOnce(t, priced, `"cost": -2.7`, `"cost": -2.70`)
	keyed := replaceOnce(t, zero, `"subAccount": "CASH",`, `"subAccount": "CASH", "qualifiedDividend": false,`)
	steps := []struct {
		raw     []byte
		want    Recorded
		changed string // the new revision's changed, if there is one
	}{
		{valid, Recorded{New: 1}, ""},
		{pending, Recorded{Revised: 1}, `["status"]`},
		{valid, Recorded{Revised: 1}, `["status"]`},
		{priced, Recorded{Same: 1}, ""},
		{zero, Recorded{Same: 1}, ""},
		{keyed, Recorded{Revised: 1}, `["qualifiedDividend"]`},
	}
	for i, st := range steps {
		rec := record(t, s, listRead(individual, start, end, sent.Add(time.Duration(i)*time.Hour), decodeRow(t, string(st.raw))))
		st.want.ReadID = rec.ReadID
		if rec != st.want {
			t.Errorf("read %d: %+v, want %+v", i+1, rec, st.want)
		}
		if got := storeStrings(t, s, "SELECT latest_raw FROM api_txn"); len(got) != 1 || got[0] != string(st.raw) {
			t.Errorf("read %d: the latest raw JSON is not the read's", i+1)
		}
		if st.changed == "" {
			continue
		}
		got := storeStrings(t, s, `SELECT c.changed FROM revision_cut c JOIN api_revision v USING (revision_id)
			WHERE v.seq = (SELECT max(seq) FROM api_revision)`)
		if len(got) != 1 || got[0] != st.changed {
			t.Errorf("read %d: changed %q, want %q", i+1, got, st.changed)
		}
	}
	if got := storeStrings(t, s, "SELECT raw FROM api_revision ORDER BY seq"); !slices.Equal(got,
		[]string{string(valid), string(pending), string(valid), string(keyed)}) {
		t.Errorf("the revisions keep %d raw JSONs, not those of the runs", len(got))
	}
	if got := storeInt(t, s, "SELECT revisions FROM txn"); got != 4 {
		t.Errorf("txn.revisions is %d, want 4", got)
	}
	// A by-id read is never complete, and its revision counts all the same.
	byID := decodeRow(t, string(pending))
	if _, err := s.RecordTransaction(t.Context(), TransactionRead{
		Read:    readAt(sent.Add(9 * time.Hour)),
		Account: individual, ActivityID: byID.ActivityID, Row: byID,
	}); err != nil {
		t.Fatal(err)
	}
	if got := storeInt(t, s, "SELECT revisions FROM txn"); got != 5 {
		t.Errorf("after a by-id read that changed its status, txn.revisions is %d, want 5", got)
	}
	if got := storeStrings(t, s, "SELECT first_seen_at || ' ' || last_seen_at FROM api_revision WHERE seq = 3"); len(got) != 1 ||
		got[0] != instant(sent.Add(2*time.Hour+time.Second))+" "+instant(sent.Add(4*time.Hour+time.Second)) {
		t.Errorf("revision 3 was seen %q, want from read 3 to read 5", got)
	}
}

// goneOf returns the read that shows the transaction of activity id
// gone, or -1.
func goneOf(t *testing.T, s *Store, id int64) int64 {
	t.Helper()
	return storeInt(t, s, "SELECT gone_read_id FROM txn WHERE activity_id = ?", id)
}

// A complete read that leaves out a transaction whose time and trade date
// lie inside its window records the absence and shows it gone. A capped
// read, a read for one symbol, a window that holds the time but not the
// trade date, a time in the window's last second and a read older than
// the last that returned the row show nothing gone. A by-id read of a gone
// transaction moves its latest raw JSON but leaves it gone; a later list
// read that returns it brings it back.
func TestGoneAndBack(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{maxRows: 4})
	start := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	const a, b, c, d = 1001, 1002, 1003, 1004
	rowA := cashRow(t, a, time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC))
	// B's time is inside the window, its trade date the day before it.
	rowB := decodeRow(t, strings.Replace(cashRaw(b, time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC)),
		`"tradeDate": "2026-10-01T04:00:00+0000"`, `"tradeDate": "2026-09-30T04:00:00+0000"`, 1))
	// C's time is the window's end, which Schwab's times, in whole
	// seconds, put in its last second.
	rowC := cashRow(t, c, end)
	// D is 12 hours before the window: a complete read of it records its
	// absence, for a wider rule, but cannot show it gone.
	rowD := cashRow(t, d, start.Add(-12*time.Hour))
	var fillers []schwab.Transaction
	for i := range 4 {
		fillers = append(fillers, cashRow(t, int64(2001+i), start.Add(time.Duration(i+1)*time.Hour)))
	}
	at := func(i int) time.Time { return sent.Add(time.Duration(i) * time.Hour) }

	record(t, s, listRead(individual, start, end, at(1), rowA, rowB, rowC, rowD))
	capped := record(t, s, listRead(individual, start, end, at(2), fillers...))
	symbol := listRead(individual, start, end, at(3))
	symbol.Query.Symbol = "XYZ"
	record(t, s, symbol)
	if n := storeInt(t, s, "SELECT count(*) FROM api_absent"); n != 0 {
		t.Errorf("a capped read and a read for one symbol recorded %d absences, want none", n)
	}
	if n := storeInt(t, s, "SELECT capped FROM api_read WHERE read_id = ?", capped.ReadID); n != 1 {
		t.Errorf("the read of %d rows is not capped", len(fillers))
	}
	for _, id := range []int64{a, b, c} {
		if g := goneOf(t, s, id); g != -1 {
			t.Fatalf("after reads that are not complete, %d is gone by read %d", id, g)
		}
	}

	r4 := record(t, s, listRead(individual, start, end, at(4)))
	// The fillers' trade dates are the window's start, which it does not
	// hold with a second to spare.
	if r4.Gone != 1 {
		t.Errorf("the empty complete read shows %d gone, want 1", r4.Gone)
	}
	if g := goneOf(t, s, a); g != r4.ReadID {
		t.Errorf("A is gone by read %d, want %d", g, r4.ReadID)
	}
	for _, id := range []int64{b, c, d} {
		if g := goneOf(t, s, id); g != -1 {
			t.Errorf("%d is gone by read %d, though the window does not hold it with a second to spare", id, g)
		}
		if n := storeInt(t, s, "SELECT count(*) FROM api_absent a JOIN txn x USING (txn_id) WHERE x.activity_id = ?", id); n != 1 {
			t.Errorf("%d has %d absences, want the empty read's", id, n)
		}
	}

	byID, err := s.RecordTransaction(t.Context(), TransactionRead{
		Read: Read{SentAt: at(5), AnsweredAt: at(5)}, Account: individual, ActivityID: a, Row: rowA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if byID.Same != 1 || byID.Back != 0 {
		t.Errorf("the by-id read of A: %+v, want the same and not back", byID)
	}
	if g := goneOf(t, s, a); g != r4.ReadID {
		t.Errorf("after the by-id read A is gone by read %d, want %d", g, r4.ReadID)
	}
	if n := storeInt(t, s, "SELECT latest_read_id FROM api_txn WHERE activity_id = ?", a); n != byID.ReadID {
		t.Errorf("A's latest read is %d, want the by-id read %d", n, byID.ReadID)
	}

	r6 := record(t, s, listRead(individual, start, end, at(6), rowA))
	if r6.Back != 1 || r6.Same != 1 {
		t.Errorf("the list read with A: %+v, want it back", r6)
	}
	if g := goneOf(t, s, a); g != -1 {
		t.Errorf("A, read again, is gone by read %d", g)
	}
	// Read 4's absence stays, but it is older than the read that returned
	// A last.
	if n := storeInt(t, s, "SELECT count(*) FROM api_absent a JOIN txn x USING (txn_id) WHERE x.activity_id = ? AND read_id = ?", a, r4.ReadID); n != 1 {
		t.Errorf("A's absence from read 4 is gone")
	}
}

// After a rebuild under a narrower rule, which needs a time to lie an hour
// inside a window rather than a second, a transaction 30 minutes from
// the window's end is no longer gone, and one well inside still is: the
// rule is worked out again from the same absences, with nothing read
// again.
func TestGoneRecomputable(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	start := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	near := cashRow(t, 1, end.Add(-30*time.Minute))
	well := cashRow(t, 2, start.Add(48*time.Hour))
	record(t, s, listRead(individual, start, end, sent, near, well))
	gone := record(t, s, listRead(individual, start, end, sent.Add(time.Hour)))
	if gone.Gone != 2 {
		t.Fatalf("the empty complete read shows %d gone, want 2", gone.Gone)
	}
	path := s.Path()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(t.Context(), Options{Path: path, Now: fixedNow, margin: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if g := goneOf(t, s, 1); g != -1 {
		t.Errorf("under the narrower rule, the row near the end is gone by read %d", g)
	}
	if g := goneOf(t, s, 2); g != gone.ReadID {
		t.Errorf("under the narrower rule, the row well inside is gone by read %d, want %d", g, gone.ReadID)
	}
	for query, want := range map[string]int64{
		"SELECT count(*) FROM txn":          2,
		"SELECT count(*) FROM api_leg":      2,
		"SELECT count(*) FROM revision_cut": 2,
		"SELECT count(*) FROM api_absent":   2,
	} {
		if got := storeInt(t, s, query); got != want {
			t.Errorf("after the rebuild, %s: %d, want %d", query, got, want)
		}
	}
}

// Two identical rows without an activity ID in one answer are two
// transactions, and the next answer's two are the same two; when one of
// them changes it is a new transaction, since such rows are paired by
// their time and cut form. A row whose activityId is a string of digits is
// taken by it.
func TestRowsWithoutID(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	start := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 8, 4, 0, 0, 0, time.UTC)
	sent := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)
	same := cashRow(t, 0, at)
	if rec := record(t, s, listRead(individual, start, end, sent, same, same)); rec.New != 2 {
		t.Errorf("the first answer: %+v, want 2 new", rec)
	}
	if rec := record(t, s, listRead(individual, start, end, sent.Add(time.Hour), same, same)); rec.Same != 2 || rec.New != 0 {
		t.Errorf("the second answer: %+v, want the same 2", rec)
	}
	changed := decodeRow(t, strings.Replace(cashRaw(0, at), `"VALID"`, `"PENDING"`, 1))
	if rec := record(t, s, listRead(individual, start, end, sent.Add(2*time.Hour), same, changed)); rec.Same != 1 || rec.New != 1 {
		t.Errorf("the third answer: %+v, want 1 the same and 1 new", rec)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM api_txn WHERE activity_id IS NULL AND id_source = 'none'"); n != 3 {
		t.Errorf("the store holds %d transactions without an ID, want 3", n)
	}
	text := decodeRow(t, strings.Replace(cashRaw(0, at), `{"time"`, `{"activityId": "73508261999", "time"`, 1))
	if text.ActivityID != 0 {
		t.Fatalf("the client read the string ID as %d", text.ActivityID)
	}
	if rec := record(t, s, listRead(individual, start, end, sent.Add(3*time.Hour), text)); rec.New != 1 {
		t.Errorf("the row with a string ID: %+v, want new", rec)
	}
	if rec := record(t, s, listRead(individual, start, end, sent.Add(4*time.Hour), text)); rec.Same != 1 {
		t.Errorf("the row with a string ID again: %+v, want the same", rec)
	}
	if got := storeStrings(t, s, "SELECT id_source FROM api_txn WHERE activity_id = 73508261999"); !slices.Equal(got, []string{"string"}) {
		t.Errorf("the row with a string ID is kept as %q, want one with id_source string", got)
	}
}

// The API covers the days that lie, with a second to spare, inside one
// window of the complete reads, merged where they overlap, each ending at
// when it was sent if that came first, and only from the day of the
// account's oldest transaction: an empty window before it covers nothing,
// nor does a read for one symbol.
func TestAPICoverage(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	day := func(d, h, m, sec int) time.Time { return time.Date(2026, 10, d, h, m, sec, 0, time.UTC) }
	coverage := func() []string {
		return storeStrings(t, s, "SELECT from_date || '..' || to_date FROM coverages WHERE source = 'api' ORDER BY from_date")
	}
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	steps := []struct {
		read TransactionsRead
		want []string
	}{
		// Empty, and before the oldest transaction: none.
		{listRead(individual, time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC), time.Date(2026, 9, 20, 4, 0, 0, 0, time.UTC), at), nil},
		// From midnight to midnight: the first day lacks its second before
		// midnight, and the last day its second after.
		{listRead(individual, day(1, 4, 0, 0), day(5, 4, 0, 0), at.Add(time.Hour),
			cashRow(t, 1, day(2, 15, 0, 0))), []string{"2026-10-02..2026-10-03"}},
		// Overlapping the one before: merged with it.
		{listRead(individual, day(5, 3, 59, 0), day(9, 4, 0, 0), at.Add(2*time.Hour)), []string{"2026-10-02..2026-10-07"}},
		// Sent before its end: it ends when it was sent.
		{listRead(individual, day(9, 3, 0, 0), day(20, 4, 0, 0), day(12, 15, 0, 0)), []string{"2026-10-02..2026-10-11"}},
		// A second before midnight is enough.
		{
			listRead(individual, day(15, 3, 59, 59), day(18, 4, 0, 0), at.Add(3*time.Hour)),
			[]string{"2026-10-02..2026-10-11", "2026-10-15..2026-10-16"},
		},
	}
	for i, st := range steps {
		record(t, s, st.read)
		if got := coverage(); !slices.Equal(got, st.want) {
			t.Errorf("after read %d, the API covers %q, want %q", i+1, got, st.want)
		}
	}
	wide := listRead(individual, day(1, 0, 0, 0), day(31, 0, 0, 0), at.Add(4*time.Hour))
	wide.Query.Symbol = "XYZ"
	record(t, s, wide)
	if got := coverage(); len(got) != 2 {
		t.Errorf("a read for one symbol changed the coverage to %q", got)
	}
}

// A transaction's columns from its raw JSON: money in cents and quantities
// in millionths, a value rounded from noise kept and noted, one off the
// grid left NULL and noted, a netAmount that is absent left NULL, the
// description masked, and another account's number noted, masked.
func TestTxnFields(t *testing.T) {
	t.Parallel()
	raw := `{"activityId": 7, "time": "2026-10-03T15:00:00+0000", "tradeDate": "2026-10-03T04:00:00+0000",
		"accountNumber": "40221783", "type": "TRADE", "status": "VALID", "subAccount": "CASH",
		"description": "JOURNAL TO 12345678",
		"netAmount": 0.30000000000000004, "transferItems": [
		{"instrument": {"assetType": "EQUITY", "symbol": "XYZ", "instrumentId": 9}, "amount": -236.999999999999,
		 "cost": 81.29223999999999, "price": 0.0},
		{"instrument": {"assetType": "CURRENCY", "symbol": "CURRENCY_USD", "instrumentId": 1}, "amount": 0.0012,
		 "cost": -0.65, "feeType": "COMMISSION"}]}`
	r, legs := txnFields(decodeRow(t, raw), individual)
	if r.net != (sql.NullInt64{Int64: 30, Valid: true}) || r.description.String != "JOURNAL TO ...678" ||
		r.numberSent.String != "...783" || r.tradeDay.String != "2026-10-03" || r.settlementDay.Valid {
		t.Errorf("the columns: %+v", r)
	}
	wantLegs := []legRow{
		{
			assetType: nullString("EQUITY"), instrumentID: sql.NullInt64{Int64: 9, Valid: true}, symbol: nullString("XYZ"),
			amountMicros: sql.NullInt64{Int64: -237_000_000, Valid: true}, price: sql.NullFloat64{Valid: true},
		},
		{
			assetType: nullString("CURRENCY"), instrumentID: sql.NullInt64{Int64: 1, Valid: true}, symbol: nullString("CURRENCY_USD"),
			amountMicros: sql.NullInt64{Int64: 1200, Valid: true}, costCents: sql.NullInt64{Int64: -65, Valid: true}, feeType: nullString("COMMISSION"),
		},
	}
	if !reflect.DeepEqual(legs, wantLegs) {
		t.Errorf("the legs:\n%+v\nwant\n%+v", legs, wantLegs)
	}
	want := `[{"path":"netAmount","flag":"rounded","value":"0.30000000000000004"},` +
		`{"path":"transferItems.amount","leg":0,"flag":"rounded","value":"-236.999999999999"},` +
		`{"path":"transferItems.cost","leg":0,"flag":"inexact","value":"81.29223999999999"},` +
		`{"path":"transferItems.amount","leg":1,"flag":"inexact","value":"0.0012"}]`
	if r.problems != want {
		t.Errorf("the problems:\n%s\nwant\n%s", r.problems, want)
	}
	r, _ = txnFields(decodeRow(t, strings.Replace(raw, `"netAmount": 0.30000000000000004,`, "", 1)), "40221783")
	if r.net.Valid || r.numberSent.Valid || !strings.Contains(r.problems, `"path":"netAmount","what":`) {
		t.Errorf("without a netAmount: net %+v, number %+v, problems %s", r.net, r.numberSent, r.problems)
	}
}
