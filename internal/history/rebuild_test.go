package history

import (
	"flag"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The size of TestIncrementalEqualsRebuild: under the race detector, where
// a derived write costs a tenth of a second or more, 4 seeds of 8
// operations; else 4 of 30. make volume runs many more.
var (
	seeds = flag.Int("history.seeds", 4, "TestIncrementalEqualsRebuild: how many seeds to run")
	ops   = flag.Int("history.ops", 0, "TestIncrementalEqualsRebuild: operations per seed (0: 8 under -race, else 30)")
)

// naturalKeys are the derived tables whose rows other tables refer to by
// derived row ids, which change at every derivation, with the column of
// that id and an expression over the row (as y) that names it by what
// does not change: refs, subjects, keys, evidence ids. A match is named by
// its members.
var naturalKeys = map[string]struct{ id, expr string }{
	"security":     {"security_id", "y.canonical_key"},
	"entry":        {"entry_id", "y.account_id || ':' || y.ref"},
	"csv_txn":      {"csv_txn_id", "y.subject"},
	"realized_lot": {"lot_id", "y.subject"},
	"history_row":  {"row_id", "y.file_id || ':' || y.ordinal"},
	"realized_row": {"row_id", "y.file_id || ':' || y.ordinal"},
	"match": {"match_id", `(SELECT group_concat(k, ',') FROM (SELECT coalesce('t' || m.txn_id,
		(SELECT c.subject FROM csv_txn c WHERE c.csv_txn_id = m.csv_txn_id)) AS k
		FROM match_member m WHERE m.match_id = y.match_id ORDER BY k))`},
	"flag": {"flag_id", "''"},
}

// dumpQueries are the queries of dumpDerived, by table, which the schema
// alone decides: made once, from the first store dumped.
var dumpQueries = struct {
	once    sync.Once
	queries map[string]string
}{}

// dumpDerived returns every derived table's rows, and the account's
// derived columns, each row as its values quoted and joined, sorted, with
// every derived row id replaced by its row's natural key, so that two
// derivations of the same evidence and decisions give equal dumps.
func dumpDerived(t *testing.T, s *Store) map[string][]string {
	t.Helper()
	dumpQueries.once.Do(func() {
		q := map[string]string{
			"account": "SELECT account_id || '|' || quote(account_type) || '|' || quote(kind) FROM account",
		}
		for _, table := range derivedTables {
			refers := map[string]string{}
			for _, fk := range storeRows(t, s, "SELECT \"from\", \"table\" FROM pragma_foreign_key_list('"+table+"')") {
				refers[fk[0]] = fk[1]
			}
			var exprs []string
			for _, col := range storeStrings(t, s, "SELECT name FROM pragma_table_info('"+table+"') ORDER BY cid") {
				if k, ok := naturalKeys[refers[col]]; ok {
					exprs = append(exprs, fmt.Sprintf("quote((SELECT %s FROM %s y WHERE y.%s = x.%s))", k.expr, refers[col], k.id, col))
					continue
				}
				if k, ok := naturalKeys[table]; ok && k.id == col {
					continue // the row's own derived id
				}
				exprs = append(exprs, "quote(x."+col+")")
			}
			q[table] = "SELECT " + strings.Join(exprs, " || '|' || ") + " FROM " + table + " x"
		}
		dumpQueries.queries = q
	})
	if len(dumpQueries.queries) != len(derivedTables)+1 {
		t.Fatal("the dump's queries were not made")
	}
	d := map[string][]string{}
	for table, q := range dumpQueries.queries {
		rows := storeStrings(t, s, q)
		slices.Sort(rows)
		d[table] = rows
	}
	return d
}

// A world is what TestIncrementalEqualsRebuild draws its operations from:
// two accounts' API transactions and History rows, some of them one and
// the same, lots, snapshots and decisions.
type world struct {
	t   *testing.T
	s   *Store
	rnd *rand.Rand
	// op counts the operations so far; each happens an hour after the one
	// before, from now, by the store's clock too.
	op  int
	now time.Time
}

// worldAccounts are the world's accounts: the fixtures', so that the
// snapshots' elements are theirs, with the digits their files' names give.
var worldAccounts = []struct{ number, digits, name string }{{individual, "135", "Individual"}, {options, "398", "Options"}}

// worldRaws are each account's API transactions: activity ID, day of
// January 2026, type, net amount, symbol, quantity and description.
var worldRaws = map[string][]struct {
	id          int64
	day         int
	typ         string
	net         float64
	symbol      string
	quantity    float64
	description string
}{
	individual: {
		{101, 12, "DIVIDEND_OR_INTEREST", 3.21, "", 0, "Ordinary Dividend~ABCD"},
		{102, 14, "TRADE", -50, "XYZ", 5, ""},
		{103, 20, "TRADE", 60, "XYZ", -5, ""},
		{104, 22, "JOURNAL", -500, "", 0, "JOURNAL TO ...398"},
		{105, 5, "CASH_RECEIPT", 1000, "", 0, "CASH RECEIPT"},
		{106, 29, "DIVIDEND_OR_INTEREST", 1.5, "", 0, "Ordinary Dividend~XYZ"},
	},
	options: {
		{201, 13, "DIVIDEND_OR_INTEREST", 2, "", 0, "Ordinary Dividend~QRS"},
		{202, 15, "TRADE", -30, "QRS", 3, ""},
		{204, 22, "JOURNAL", 500, "", 0, "JOURNAL FRM ...135"},
		{205, 27, "WIRE_OUT", -100, "", 0, "WIRE TO BANK"},
	},
}

// worldRows are each account's History rows: most are its API
// transactions as an export writes them, and some are history the API
// does not hold.
var worldRows = map[string][]csvRow{
	individual: {
		{"01/12/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"},
		{"01/14/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"},
		{"01/20/2026", "Sell", "XYZ", "XYZ CORP", "5", "$12.00", "", "$60.00"},
		{"01/22/2026", "Journal", "", "JOURNAL TO ...398", "", "", "", "-$500.00"},
		{"01/08/2026", "Bank Interest", "", "BANK INT", "", "", "", "$0.12"},
		{"12/30/2025", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.00"},
	},
	options: {
		{"01/13/2026", "Qualified Dividend", "QRS", "QRS FUND", "", "", "", "$2.00"},
		{"01/15/2026", "Buy", "QRS", "QRS FUND", "3", "$10.00", "", "-$30.00"},
		{"01/22/2026", "Journal", "", "JOURNAL FRM ...135", "", "", "", "$500.00"},
		{"01/28/2026", "Wire Sent", "", "WIRE", "", "", "", "-$100.00"},
	},
}

// worldLots are the lots that Realized exports may hold.
var worldLots = [][9]string{
	{"XYZ", "01/20/2026", "01/14/2025", "5", "$60.00", "$50.00", "$10.00", "No", ""},
	{"ABCD", "01/21/2026", "03/02/2025", "2", "$30.00", "$40.00", "-$10.00", "Yes", "$10.00"},
	{"QRS", "01/28/2026", "06/01/2025", "1", "$12.00", "$10.00", "$2.00", "No", ""},
}

// sent returns when the current operation's read was sent: an hour after
// the one before.
func (w *world) sent() time.Time {
	return w.now.Add(time.Duration(w.op) * time.Hour)
}

// account returns one of the world's accounts at random.
func (w *world) account() (number, digits, name string) {
	a := worldAccounts[w.rnd.IntN(len(worldAccounts))]
	return a.number, a.digits, a.name
}

// raw returns the raw JSON of the ith API transaction of account number,
// pending instead of valid when pending says so (another revision).
func (w *world) raw(number string, i int, pending bool) string {
	r := worldRaws[number][i]
	at := time.Date(2026, time.January, r.day, 15, 30, int(r.id%60), 0, time.UTC)
	raw := strings.Replace(apiRaw(r.id, at, r.typ, r.net, r.symbol, r.quantity, r.description),
		`"accountNumber": "`+fileAccount+`"`, `"accountNumber": "`+number+`"`, 1)
	if pending {
		raw = strings.Replace(raw, `"status": "VALID"`, `"status": "PENDING"`, 1)
	}
	return raw
}

// some returns a random subset of n indexes, at least one.
func (w *world) some(n int) []int {
	var picked []int
	for i := range n {
		if w.rnd.IntN(10) < 7 {
			picked = append(picked, i)
		}
	}
	if len(picked) == 0 {
		picked = append(picked, w.rnd.IntN(n))
	}
	return picked
}

// stamp returns the export stamp of the current operation.
func (w *world) stamp() string {
	return fmt.Sprintf("202602%02d-%02d0000", 10+w.op/20, w.op%20)
}

// pick returns the first column of one of query's rows on the store, at
// random, and whether there was one.
func (w *world) pick(query string) (string, bool) {
	got := storeStrings(w.t, w.s, query)
	if len(got) == 0 {
		return "", false
	}
	return got[w.rnd.IntN(len(got))], true
}

// step makes one operation at random, and returns what it did, for the
// log.
func (w *world) step() string {
	w.t.Helper()
	t, s, ctx := w.t, w.s, w.t.Context()
	w.op++
	sent := w.sent()
	number, digits, name := w.account()
	switch k := w.rnd.IntN(13); k {
	case 0, 1: // a list read over some days of January
		from := 1 + w.rnd.IntN(25)
		start := time.Date(2026, time.January, from, 5, 0, 0, 0, time.UTC)
		end := start.Add(time.Duration(1+w.rnd.IntN(10)) * 24 * time.Hour)
		var rows []schwab.Transaction
		for i, r := range worldRaws[number] {
			at := time.Date(2026, time.January, r.day, 15, 30, int(r.id%60), 0, time.UTC)
			if !at.Before(start) && at.Before(end) && w.rnd.IntN(10) < 8 {
				rows = append(rows, decodeRow(t, w.raw(number, i, w.rnd.IntN(5) == 0)))
			}
		}
		read := listRead(number, start, end, sent, rows...)
		if w.rnd.IntN(4) == 0 {
			read.Query.Types = []schwab.TransactionType{schwab.TransactionTrade}
		}
		record(t, s, read)
		return fmt.Sprintf("a read of %s from %s, %d rows, types %v", digits, start.Format(time.DateOnly), len(rows), read.Query.Types)
	case 2: // a read by ID
		i := w.rnd.IntN(len(worldRaws[number]))
		row := decodeRow(t, w.raw(number, i, w.rnd.IntN(3) == 0))
		if _, err := s.RecordTransaction(ctx, TransactionRead{Read: readAt(sent), Account: number, ActivityID: row.ActivityID, Row: row}); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("a read of %s's %d by ID", digits, row.ActivityID)
	case 3: // a -json file
		var raws []string
		for _, i := range w.some(len(worldRaws[number])) {
			raws = append(raws, w.raw(number, i, w.rnd.IntN(5) == 0))
		}
		importFiles(t, s, ImportOptions{}, "transactions.json", cliJSON(t, raws...))
		return fmt.Sprintf("a -json file of %d of %s's rows", len(raws), digits)
	case 4: // a snapshot line
		// With the account that holds nothing, now and then: an account
		// that only snapshots name.
		at := time.Date(2026, time.January, 5+w.rnd.IntN(30), 22, 5, 0, 0, time.UTC)
		numbers := []string{number}
		if w.rnd.IntN(2) == 0 {
			numbers = append(numbers, empty)
		}
		importFile(t, s, "snapshots.jsonl", snapshotAt(t, at, numbers...))
		return fmt.Sprintf("a snapshot of %s at %s (%d accounts)", digits, at.Format(time.DateOnly), len(numbers))
	case 5, 6: // a History export, in CSV or JSON
		var rows []csvRow
		for _, i := range w.some(len(worldRows[number])) {
			rows = append(rows, worldRows[number][i])
		}
		file := name + "_XXX" + digits + "_Transactions_" + w.stamp()
		o := ImportOptions{Account: number}
		if k == 5 {
			importFiles(t, s, o, file+".csv", historyCSV(rows...))
		} else {
			importFiles(t, s, o, file+".json", historyJSON("12/29/2025", "02/09/2026", nil, rows...))
		}
		return fmt.Sprintf("a History export of %d of %s's rows", len(rows), digits)
	case 7: // a Realized export
		var lots [][9]string
		for _, i := range w.some(len(worldLots)) {
			lots = append(lots, worldLots[i])
		}
		importFiles(t, s, ImportOptions{Account: number, Force: true}, "XXXX0"+digits+"_GainLoss_Realized_Details_"+w.stamp()+".csv",
			realizedCSV("Mon Feb 09 10:00:00 EST 2026", "12/01/2025", "02/09/2026", lots...))
		return fmt.Sprintf("a Realized export of %d lots of %s", len(lots), digits)
	case 8: // the accounts shared at login, with their hashes
		var numbers []schwab.AccountNumber
		for _, n := range []string{individual, options, empty} {
			numbers = append(numbers, schwab.AccountNumber{AccountNumber: n, HashValue: schwab.AccountHash(fmt.Sprintf("HASH%d%s", w.op, n[5:]))})
		}
		if err := s.RecordAccountNumbers(ctx, readAt(sent), numbers); err != nil {
			t.Fatal(err)
		}
		return "the accounts shared at login"
	case 9: // a file set aside or brought back
		ref, ok := w.pick("SELECT ref FROM files WHERE kind <> 'api_json'")
		if !ok {
			return "nothing to set aside"
		}
		undo := w.rnd.IntN(3) == 0
		if _, err := s.Forget(ctx, ref, undo); err != nil {
			t.Fatal(err)
		}
		return fmt.Sprintf("%s set aside (undo %v)", ref, undo)
	}
	return w.decide()
}

// decide makes one of your decisions at random. Decide may refuse it, as
// it refuses you, but never fails.
func (w *world) decide() string {
	t, s := w.t, w.s
	var d Decision
	random := func(values ...string) string { return values[w.rnd.IntN(len(values))] }
	entry, ok := w.pick("SELECT subject FROM entry")
	if !ok {
		return "nothing to decide"
	}
	switch w.rnd.IntN(9) {
	case 0:
		d = Decision{Kind: "category", Subject: entry, Value: random("deposit", "fee", "journal", "other", "auto")}
	case 1:
		d = Decision{Kind: "count", Subject: entry, Value: random("yes", "no", "auto")}
	case 2:
		file, _ := w.pick("SELECT subject FROM entry WHERE csv_txn_id IS NOT NULL")
		api, _ := w.pick("SELECT subject FROM entry WHERE txn_id IS NOT NULL")
		d = Decision{Kind: "match", Subject: file, Value: random(api, "none", "auto")}
	case 3:
		other, _ := w.pick("SELECT subject FROM entry")
		d = Decision{Kind: "pair", Subject: entry, Value: random(other, "none", "auto")}
	case 4:
		file, _ := w.pick("SELECT 'f:' || lower(hex(sha256)) FROM import_file")
		d = Decision{Kind: "completeness", Subject: file, Value: random("complete", "filtered", "auto")}
	case 5:
		file, _ := w.pick("SELECT 'f:' || lower(hex(sha256)) FROM import_file WHERE kind <> 'api_json'")
		account, _ := w.pick("SELECT 'acct:' || account_id FROM account")
		d = Decision{Kind: "file_account", Subject: file, Value: random(account, "auto")}
	case 6:
		d = Decision{Kind: "action", Subject: random("action:BANK INTEREST", "action:WIRE SENT"), Value: random("count", "hold", "auto")}
	case 7:
		account, _ := w.pick("SELECT 'acct:' || account_id FROM account")
		d = Decision{Kind: "account", Subject: account, Value: random("kind=ira", "kind=taxable", "auto")}
	default:
		key, _ := w.pick("SELECT 'k:' || key FROM instrument_key")
		other, _ := w.pick("SELECT key FROM instrument_key")
		d = Decision{Kind: "security", Subject: key, Value: random("same k:"+other, "differ k:"+other, "auto")}
	}
	if _, err := s.Decide(t.Context(), d); err != nil {
		if strings.Contains(err.Error(), " failed: ") {
			t.Fatalf("deciding %+v: %v", d, err)
		}
		return fmt.Sprintf("%s %s %s, refused: %v", d.Kind, d.Subject, d.Value, err)
	}
	return fmt.Sprintf("%s %s %s", d.Kind, d.Subject, d.Value)
}

// Derived data is a function of the evidence and your decisions (I12): a
// store built by seeded random sequences of operations (list reads, of
// every type or not, reads by ID, revisions, -json files, snapshot lines,
// History and Realized exports, the accounts shared at login, files set
// aside and brought back, and decisions of each kind, files moved to
// another account among them)
// holds, in every derived table dumped by natural keys, what a rebuild of
// it derives from the evidence alone. Each seed is a parallel subtest; a
// failure names the seed, which -history.seeds and -history.ops reach.
func TestIncrementalEqualsRebuild(t *testing.T) {
	t.Parallel()
	n := *ops
	if n == 0 {
		n = 30
		if raceOn {
			n = 8
		}
	}
	for seed := range uint64(*seeds) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			w := &world{t: t, rnd: rand.New(rand.NewPCG(seed, 1)), now: time.Date(2026, time.February, 10, 0, 0, 0, 0, time.UTC)}
			// The store's clock is the world's: each write comes two
			// seconds after the read it records was sent.
			w.s = newStore(t, Options{Now: func() time.Time { return w.sent().Add(2 * time.Second) }})
			s := w.s
			var did []string
			for range n {
				did = append(did, w.step())
			}
			before := dumpDerived(t, s)
			if err := s.Rebuild(t.Context()); err != nil {
				t.Fatal(err)
			}
			after := dumpDerived(t, s)
			failed := false
			for _, table := range slices.Sorted(maps.Keys(before)) {
				b, a := before[table], after[table]
				if slices.Equal(b, a) {
					continue
				}
				failed = true
				only := func(x, y []string) []string {
					return slices.DeleteFunc(slices.Clone(x), func(r string) bool { return slices.Contains(y, r) })
				}
				t.Errorf("%s differs after the rebuild:\nonly before: %q\nonly after:  %q", table, only(b, a), only(a, b))
			}
			if failed {
				t.Logf("the operations:\n%s", strings.Join(did, "\n"))
			}
		})
	}
}
