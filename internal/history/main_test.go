package history

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// template is a new store that TestMain made once through Open, which each
// store test copies rather than creating the schema again: that costs about
// a quarter of a second under -race.
var template string

// TestMain makes the template store, or, when a test runs the test binary
// with HISTORY_TEST_CHILD set, acts as another process that uses a store
// (child). `go test` never sets the variable, and a child makes no store.
func TestMain(m *testing.M) {
	if mode := os.Getenv("HISTORY_TEST_CHILD"); mode != "" {
		os.Exit(child(mode))
	}
	dir, err := os.MkdirTemp("", "history-template-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	template = filepath.Join(dir, "history.db")
	s, err := Open(context.Background(), Options{Path: template, Now: fixedNow})
	if err == nil {
		// The last connection to close takes the -wal and -shm with it, so
		// the database is the whole store.
		err = s.Close()
	}
	code := 1
	if err != nil {
		fmt.Fprintln(os.Stderr, "making the template store:", err)
	} else {
		readerStore = filepath.Join(dir, "reader.db")
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// readerStore is the store that the Reader's tests read copies of
// (readerCopy), beside the template: made once, from a copy of the
// template, by the first of them to run (needReaderStore), of under 100
// rows, so that each of those tests costs no recording under -race, and so
// that its making, about a second under -race, runs beside the other tests
// rather than before them all. It holds the fake's three accounts, with
// their hashes and nicknames, and:
//
//   - ...135's and ...398's transactions of the fake's fixtures, each
//     account's as one complete read, which gives the account api
//     coverage, beside the two sides of a journal between the two accounts
//     that share an activity ID, day and time;
//   - a History export of ...398, established complete, of the half year
//     before its oldest API row and the week after, which gives it file
//     coverage that meets its api coverage, and in whose week inside the
//     API's coverage a row that no API row is is held out;
//   - for ...783, one complete read of the rows that make DeriveAPIEntries'
//     cases: two rows without an activity ID of one time and cut form, an
//     activity ID sent twice, the second time with another amount, a row
//     naming ...135, an interest of an amount off the cent grid, a trade
//     with a fee, and a transfer between the account's types whose
//     description, which the store keeps masked, holds 8 digits that the
//     rules read;
//   - a snapshot file of ...135 and of an account whose number could not
//     be read, whose flags wait for you and are no account's.
var readerStore string

// readerReads are the reads of transactions that made readerStore, in the
// order they were recorded.
var readerReads []TransactionsRead

// readerMade makes readerStore the first time it is called, and returns
// what came of it every time.
var readerMade = sync.OnceValue(func() error { return makeReaderStore(readerStore) })

// needReaderStore makes readerStore and readerReads if no test has yet,
// failing the test if they could not be made.
func needReaderStore(t *testing.T) {
	t.Helper()
	if err := readerMade(); err != nil {
		t.Fatal("making the reader's store:", err)
	}
}

// readerJournal is the activity ID of the journal between ...135 and
// ...398 that readerStore holds in both.
const readerJournal = 5550001

// makeReaderStore makes readerStore at path.
func makeReaderStore(path string) error {
	ctx := context.Background()
	b, err := os.ReadFile(template)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	s, err := Open(ctx, Options{Path: path, Now: fixedNow})
	if err != nil {
		return err
	}
	if err := fillReaderStore(ctx, s); err != nil {
		s.Close()
		return err
	}
	return s.Close()
}

// fillReaderStore records what readerStore holds into s.
func fillReaderStore(ctx context.Context, s *Store) error {
	var numbers []schwab.AccountNumber
	var prefs schwab.UserPreference
	var fixtures []schwab.Transaction
	var accounts []json.RawMessage
	for name, v := range map[string]any{
		"account_numbers.json": &numbers, "user_preference.json": &prefs, "transactions.json": &fixtures, "accounts.json": &accounts,
	} {
		if err := readFixture(name, v); err != nil {
			return err
		}
	}
	if err := s.RecordAccountNumbers(ctx, readAt(fixturesEnd), numbers); err != nil {
		return err
	}
	if err := s.RecordPreferences(ctx, readAt(fixturesEnd), prefs); err != nil {
		return err
	}
	of := func(number string, more ...string) ([]schwab.Transaction, error) {
		var rows []schwab.Transaction
		for _, r := range fixtures {
			if r.AccountNumber == number {
				rows = append(rows, r)
			}
		}
		for _, raw := range more {
			var r schwab.Transaction
			if err := json.Unmarshal([]byte(raw), &r); err != nil {
				return nil, err
			}
			rows = append(rows, r)
		}
		return rows, nil
	}
	ny := nytime.NewYork()
	journal := time.Date(2025, 3, 10, 15, 30, 0, 0, ny)
	individualRows, err := of(individual, readerRaw(individual, readerJournal, journal, "JOURNAL", -100, "JOURNAL TO "+options, ""))
	if err != nil {
		return err
	}
	optionsRows, err := of(options, readerRaw(options, readerJournal, journal, "JOURNAL", 100, "JOURNAL FRM "+individual, ""))
	if err != nil {
		return err
	}
	noID := readerRaw(empty, 0, time.Date(2025, 4, 30, 21, 18, 44, 0, ny), "DIVIDEND_OR_INTEREST", 0.11, "Free Balance Interest Adjustment", "")
	twice := readerRaw(empty, 7770001, time.Date(2025, 5, 15, 10, 40, 3, 0, ny), "DIVIDEND_OR_INTEREST", 12.34, "Ordinary Dividend~XYZ", "")
	again := strings.ReplaceAll(twice, "12.34", "12.43")
	emptyRows, err := of(empty, noID, noID, twice, again,
		readerRaw(individual, 7770002, time.Date(2025, 2, 3, 9, 0, 0, 0, ny), "ELECTRONIC_FUND", 500, "ACH Credit Received", ""),
		readerRaw(empty, 7770003, time.Date(2025, 6, 30, 21, 0, 0, 0, ny), "DIVIDEND_OR_INTEREST", 0.123, "Free Balance Interest Adjustment", ""),
		readerRaw(empty, 7770004, time.Date(2025, 3, 3, 10, 0, 0, 0, ny), "TRADE", -50.65, "", "XYZ"),
		readerRaw(empty, 7770005, time.Date(2025, 7, 7, 10, 0, 0, 0, ny), "JOURNAL", -5, "TRF FDS TO TYPE 12345678", ""))
	if err != nil {
		return err
	}
	readerReads = []TransactionsRead{
		listRead(individual, fixturesStart, fixturesEnd, day(2025, 6, 3), individualRows...),
		listRead(options, fixturesStart, fixturesEnd, day(2026, 8, 5), optionsRows...),
		listRead(empty, time.Date(2025, 1, 1, 0, 0, 0, 0, ny), time.Date(2026, 1, 1, 0, 0, 0, 0, ny), day(2026, 1, 2), emptyRows...),
	}
	for _, r := range readerReads {
		if _, err := s.RecordTransactions(ctx, r); err != nil {
			return err
		}
	}
	history := historyJSON("07/01/2024", "01/20/2025", nil,
		csvRow{"01/16/2025", "Qualified Dividend", "XYZ", "XYZ CORP", "", "", "", "$4.00"},
		csvRow{"12/16/2024", "Qualified Dividend", "XYZ", "XYZ CORP", "", "", "", "$4.00"},
		csvRow{"11/04/2024", "Buy", "XYZ", "XYZ CORP", "10", "$20.00", "$0.65", "-$200.65"},
		csvRow{"09/03/2024", "Credit Interest", "", "SCHWAB1 INT 08/01-08/30", "", "", "", "$0.07"},
		csvRow{"07/02/2024", "MoneyLink Deposit", "", "TFR FROM BANK", "", "", "", "$1000.00"})
	var mine, unread json.RawMessage
	for _, a := range accounts {
		if strings.Contains(string(a), `"accountNumber": "`+individual+`"`) {
			mine, unread = a, json.RawMessage(strings.Replace(string(a), `"`+individual+`"`, individual, 1))
		}
	}
	line, err := schwabfile.SnapshotLine(time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC), []json.RawMessage{mine, unread})
	if err != nil {
		return err
	}
	_, err = s.Import(ctx, []FileInput{
		{Name: exportName("20250121-090000", "json"), Content: []byte(history)},
		{Name: "snapshots.jsonl", Content: line},
	}, ImportOptions{Account: options, Completeness: "complete"})
	return err
}

// readerRaw returns the raw JSON of a transaction of the account whose
// number is number, with activity ID id (none when 0), at at, booked that
// New York day, of the type typ, net amount net and description: a TRADE
// buys two shares of symbol at a fee of 0.65, any other type has one cash
// leg of net.
func readerRaw(number string, id int64, at time.Time, typ string, net float64, description, symbol string) string {
	legs := fmt.Sprintf(`{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
		"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": %v, "cost": 0.0}`, net)
	if typ == "TRADE" {
		legs = fmt.Sprintf(`{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
			"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": 0.65, "cost": -0.65, "feeType": "COMMISSION"},
			{"instrument": {"assetType": "EQUITY", "status": "ACTIVE", "symbol": %q, "instrumentId": 7003, "closingPrice": 25.0,
			"type": "COMMON_STOCK"}, "amount": 2.0, "cost": %v, "price": 25.0, "positionEffect": "OPENING"}`, symbol, net+0.65)
	}
	activity := ""
	if id != 0 {
		activity = fmt.Sprintf(`"activityId": %d, `, id)
	}
	text := ""
	if description != "" {
		text = fmt.Sprintf(`"description": %q, `, description)
	}
	trade := midnight(nytime.DateOf(at))
	return fmt.Sprintf(`{%s"time": %q, %s"accountNumber": %q, "type": %q, "status": "VALID", "subAccount": "CASH",
		"tradeDate": %q, "netAmount": %v, "transferItems": [%s]}`,
		activity, schwabTime(at), text, number, typ, schwabTime(trade), net, legs)
}

// readFixture decodes the fake's fixture name, as its testdata folder holds
// it, into v: TestMain has no testing.TB to give schwabtest.Fixture.
func readFixture(name string, v any) error {
	b, err := os.ReadFile(filepath.Join("..", "schwab", "schwabtest", "testdata", name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// testNow is the time that the tests' stores take for now.
var testNow = time.Date(2026, time.November, 12, 15, 4, 5, 678_000_000, time.UTC)

func fixedNow() time.Time { return testNow }

// tickingNow returns a clock that starts at testNow and moves a second on
// each time it is read, for a test that needs what the store records in
// one write to come after what it recorded in another.
func tickingNow() func() time.Time {
	var ticks atomic.Int64
	return func() time.Time { return testNow.Add(time.Duration(ticks.Add(1)) * time.Second) }
}

// newStore opens a copy of the template store in a directory of its own,
// with a fixed clock unless o says otherwise, and closes it when the test
// ends.
func newStore(t *testing.T, o Options) *Store {
	t.Helper()
	o.Path = copyTemplate(t)
	if o.Now == nil {
		o.Now = fixedNow
	}
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// copyTemplate copies the template store to history.db in a new directory,
// readable only by you, and returns its path.
func copyTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rawDB opens path as another program would, with a connection that writes
// without the store's checks, and closes it when the test ends.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// insertAccount adds an account with number to the store, as a write
// would.
func insertAccount(w *wtx, number string) error {
	_, err := w.exec(`INSERT INTO account (number, created_from, first_seen_at, last_seen_at)
		VALUES (?, 'user', ?, ?)`, number, instant(w.now), instant(w.now))
	return err
}

// countAccounts returns how many accounts the store at path holds.
func countAccounts(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM account").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// child is the test binary acting as another process, as HISTORY_TEST_CHILD
// says:
//
//   - "open" opens and closes each store of HISTORY_TEST_FILES in turn and
//     says "ok" or "fail" of each, and at the end how many attempts it
//     retried;
//   - "write" opens HISTORY_TEST_STORE and makes 20 writes, saying "busy"
//     at its first retry and how many retries its first write needed;
//   - "migrate" opens HISTORY_TEST_STORE as a build with one more schema and
//     newer rules would, saying each warning;
//   - "update" opens HISTORY_TEST_STORE as a build with newer rules or a
//     newer calendar would, as HISTORY_TEST_RAISE says ("rules" or
//     "calendar"), saying "read" once it has read the store's versions and
//     then waiting for the barrier before it brings its derived data up to
//     date, and saying each warning;
//   - "hold" opens HISTORY_TEST_STORE, adds an account inside a write, says
//     "inside", and waits there to be killed;
//   - "commit" opens HISTORY_TEST_STORE, adds an account in a write that
//     commits, says "committed", and waits there to be killed, its changes
//     in the -wal alone;
//   - "restore" restores HISTORY_TEST_STORE from the backup
//     HISTORY_TEST_BACKUP as a build with one more schema and newer rules
//     would, saying each warning and then what Restore reports;
//   - "journal" makes HISTORY_TEST_STORE another program's SQLite file in
//     rollback-journal mode, changes it inside a transaction until the file
//     holds changes that the journal can undo, says "inside", and waits
//     there to be killed;
//   - "read" opens HISTORY_TEST_STORE with OpenReader as a build with one
//     more schema, newer rules or a newer calendar would, as
//     HISTORY_TEST_RAISE says ("schema", "rules" or "calendar"), and says
//     "read" and what came of it: "ok", "needs_update", "newer" or the
//     error.
//
// Each says "ready" once it has done what comes before the barrier, the file
// HISTORY_TEST_BARRIER, and waits for that file before going on. It
// uses a 1 ms busy timeout, so that a busy store makes it retry at once.
func child(mode string) int {
	ctx := context.Background()
	store := os.Getenv("HISTORY_TEST_STORE")
	o := Options{Path: store, Now: time.Now, BusyTimeout: time.Millisecond, LockWait: time.Minute}
	testNoRetry = os.Getenv("HISTORY_TEST_NORETRY") != ""
	fail := func(err error) int {
		fmt.Println("fail", err)
		return 1
	}
	switch mode {
	case "open":
		fmt.Println("ready")
		waitBarrier()
		for f := range strings.SplitSeq(os.Getenv("HISTORY_TEST_FILES"), string(os.PathListSeparator)) {
			o.Path = f
			var retries int64
			s, err := Open(ctx, o)
			if err == nil {
				retries = s.retries.Load()
				err = s.Close()
			}
			if err != nil {
				fmt.Println("fail", f, err)
				continue
			}
			fmt.Println("ok", f, retries)
		}
		return 0
	case "write":
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		defer s.Close()
		var once sync.Once
		testHookRetry = func() { once.Do(func() { fmt.Println("busy") }) }
		fmt.Println("ready")
		waitBarrier()
		id := os.Getenv("HISTORY_TEST_ID")
		for i := range 20 {
			before := s.retries.Load()
			if err := s.withWrite(ctx, "adding an account", func(w *wtx) error {
				return insertAccount(w, id+"-"+strconv.Itoa(i))
			}); err != nil {
				return fail(err)
			}
			if i == 0 {
				fmt.Println("first", s.retries.Load()-before)
			}
		}
		fmt.Println("done")
		return 0
	case "migrate":
		migrations = append(migrations, migration{sql: "CREATE TABLE later (x INTEGER) STRICT;"})
		deriveVersion++
		o.Warn = func(m string) { fmt.Println("warn:", m) }
		fmt.Println("ready")
		waitBarrier()
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		fmt.Println("opened")
		if err := s.Close(); err != nil {
			return fail(err)
		}
		return 0
	case "update":
		if os.Getenv("HISTORY_TEST_RAISE") == "rules" {
			deriveVersion++
		} else {
			calendarVersion++
		}
		o.Warn = func(m string) { fmt.Println("warn:", m) }
		testHookBeforeUpdate = func() {
			fmt.Println("read")
			waitBarrier()
		}
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		fmt.Println("opened")
		if err := s.Close(); err != nil {
			return fail(err)
		}
		return 0
	case "hold":
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		err = s.withWrite(ctx, "adding an account", func(w *wtx) error {
			if err := insertAccount(w, "killed"); err != nil {
				return err
			}
			fmt.Println("inside")
			_, _ = io.Copy(io.Discard, os.Stdin)
			return nil
		})
		return fail(fmt.Errorf("the write ended (%w) instead of waiting to be killed", err))
	case "commit":
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		if err := s.withWrite(ctx, "adding an account", func(w *wtx) error { return insertAccount(w, "committed") }); err != nil {
			return fail(err)
		}
		fmt.Println("committed")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return fail(errors.New("the input ended instead of the child being killed"))
	case "restore":
		migrations = append(migrations, migration{sql: "CREATE TABLE later (x INTEGER) STRICT;"})
		deriveVersion++
		o.Warn = func(m string) { fmt.Println("warn:", m) }
		r, err := Restore(ctx, o, os.Getenv("HISTORY_TEST_BACKUP"), false)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("restored %d accounts, then %d; updated %v\n", r.Before.Accounts, r.After.Accounts, r.Updated)
		return 0
	case "read":
		switch os.Getenv("HISTORY_TEST_RAISE") {
		case "schema":
			migrations = append(migrations, migration{sql: "CREATE TABLE later (x INTEGER) STRICT;"})
		case "rules":
			deriveVersion++
		default:
			calendarVersion++
		}
		r, err := OpenReader(ctx, ReaderOptions{Path: store, BusyTimeout: time.Millisecond, LockWait: time.Minute})
		switch {
		case err == nil:
			fmt.Println("read ok")
			r.Close()
		case errors.Is(err, ErrNeedsUpdate):
			fmt.Println("read needs_update")
		case errors.Is(err, ErrNewer):
			fmt.Println("read newer")
		default:
			fmt.Println("read", err)
		}
		return 0
	case "journal":
		db, err := sql.Open("sqlite", dsn(store, "_pragma=busy_timeout(5000)"))
		if err != nil {
			return fail(err)
		}
		db.SetMaxOpenConns(1)
		// A cache of two pages makes the transaction write the pages it
		// changed to the file before it commits, their old contents in the
		// journal. In a process with other connections open, SQLite can
		// lend the cache their pages instead.
		if _, err := db.ExecContext(ctx, `CREATE TABLE t (x);
			WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200) INSERT INTO t SELECT zeroblob(1000) FROM n;
			PRAGMA cache_size = 2; BEGIN; UPDATE t SET x = zeroblob(1001)`); err != nil {
			return fail(err)
		}
		fmt.Println("inside")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return fail(errors.New("the transaction ended instead of waiting to be killed"))
	}
	return fail(fmt.Errorf("unknown mode %q", mode))
}

// waitBarrier waits for the file HISTORY_TEST_BARRIER, if one is named.
func waitBarrier() {
	b := os.Getenv("HISTORY_TEST_BARRIER")
	if b == "" {
		return
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(b); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// A childProcess is the test binary running as child.
type childProcess struct {
	cmd   *exec.Cmd
	lines chan string // what it says, line by line; closed when it ends
	stdin io.WriteCloser
}

// startChild runs the test binary as a child in mode, with env added to its
// environment.
func startChild(t *testing.T, mode string, env ...string) *childProcess {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	// The race detector waits a second before a process that it watches
	// exits, which a child need not.
	race := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	cmd.Env = append(os.Environ(), append([]string{"HISTORY_TEST_CHILD=" + mode, "GORACE=" + race}, env...)...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &childProcess{cmd: cmd, lines: make(chan string, 100), stdin: stdin}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return c
}

// expect waits for the child to say a line that starts with prefix, and
// returns it; what it says before is logged.
func (c *childProcess) expect(t *testing.T, prefix string) string {
	t.Helper()
	timeout := time.After(time.Minute)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				t.Fatalf("the child ended without saying %q", prefix)
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
			t.Logf("the child said %q", line)
		case <-timeout:
			t.Fatalf("the child did not say %q within a minute", prefix)
		}
	}
}

// rest returns everything the child says until it ends, and fails the test
// if it does not end well.
func (c *childProcess) rest(t *testing.T) []string {
	t.Helper()
	var lines []string
	for line := range c.lines {
		lines = append(lines, line)
	}
	if err := c.cmd.Wait(); err != nil {
		t.Errorf("the child: %v; it said %q", err, lines)
	}
	return lines
}
