package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver that holds the store's lock in TestBusyStore

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The template store: a new store's bytes, made once for the test binary
// through the real history.Open and copied by each test that needs a
// store, which saves the schema's cost under -race (23 of the design).
var (
	templateOnce  sync.Once
	templateBytes []byte
	templateErr   error
)

// newStoreFile copies the template store to history.db in a folder of the
// test's own, readable only by you, makes it the store that the commands
// use (SCHWAB_STORE_FILE), and returns its path.
func newStoreFile(t *testing.T) string {
	t.Helper()
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "schwabtx-template-") //nolint:usetesting // The template outlives the test that makes it; it is removed at once.
		if err != nil {
			templateErr = err
			return
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "history.db")
		s, err := history.Open(context.Background(), history.Options{Path: path})
		if err != nil {
			templateErr = err
			return
		}
		if templateErr = s.Close(); templateErr == nil {
			templateBytes, templateErr = os.ReadFile(path)
		}
	})
	if templateErr != nil {
		t.Fatalf("making the template store: %v", templateErr)
	}
	dir := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "history.db")
	if err := os.WriteFile(path, templateBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCHWAB_STORE_FILE", path)
	return path
}

// offline sets up a run of schwabtx with no login: no app key, no token
// file, and the user's folders in the test's temp space, with the clock at
// testNow.
func offline(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	for _, k := range []string{"SCHWAB_APP_KEY", "SCHWAB_APP_SECRET", "SCHWAB_BASE_URL", "SCHWAB_CALLBACK_URL"} {
		t.Setenv(k, "")
	}
	t.Setenv("SCHWAB_TOKEN_FILE", filepath.Join(home, "token.json"))
	t.Setenv("SCHWAB_SNAPSHOT_FILE", filepath.Join(home, "snapshots.jsonl"))
	t.Setenv("SCHWAB_STORE_FILE", "")
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "XDG_CACHE_HOME", "LocalAppData"} {
		t.Setenv(k, home)
	}
	timeNow = func() time.Time { return testNow }
	t.Cleanup(func() { timeNow = time.Now })
}

// mustRun runs schwabtx with args and fails the test if it fails.
func mustRun(t *testing.T, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := runTx(t, args...)
	if err != nil {
		t.Fatalf("schwabtx %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout, stderr
}

// writeFile writes content to name in dir and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// snapshotFile returns a snapshot file of one line for each time in at,
// each with a cash account of each number.
func snapshotFile(t *testing.T, at []time.Time, numbers ...string) string {
	t.Helper()
	var b strings.Builder
	for _, when := range at {
		var raws []json.RawMessage
		for _, n := range numbers {
			raws = append(raws, json.RawMessage(fmt.Sprintf(`{"securitiesAccount":{"type":"CASH","accountNumber":%q,"positions":[],`+
				`"currentBalances":{"cashBalance":10.5,"liquidationValue":10.5}},"aggregatedBalance":{"liquidationValue":10.5}}`, n)))
		}
		line, err := schwabfile.SnapshotLine(when, raws)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
	}
	return b.String()
}

// historyRow is a row of a History export: date, action, symbol,
// description, quantity, price, fees and amount, as the file writes them.
type historyRow [8]string

// historyJSON returns a History export in JSON that states the range
// from..to (MM/DD/YYYY), with rows.
func historyJSON(from, to string, rows ...historyRow) string {
	var els []string
	for _, r := range rows {
		els = append(els, fmt.Sprintf(`{"Date": %q, "Action": %q, "Symbol": %q, "Description": %q, "Quantity": %q, "Price": %q, "Fees & Comm": %q, "Amount": %q}`,
			r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7]))
	}
	return fmt.Sprintf(`{"FromDate": %q, "ToDate": %q, "BrokerageTransactions": [%s]}`, from, to, strings.Join(els, ",\n"))
}

// historyCSV returns a History export in today's CSV layout with rows.
func historyCSV(rows ...historyRow) string {
	var b strings.Builder
	b.WriteString(`"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n")
	for _, r := range rows {
		b.WriteString(`"` + strings.Join(r[:], `","`) + `"` + "\n")
	}
	return b.String()
}

// dividendRaw returns Schwab's JSON of a dividend of amount on symbol, paid
// into the account with the given number at 15:30 New York time on day.
func dividendRaw(id int64, number, day, symbol string, amount float64) (string, time.Time) {
	d, err := time.ParseInLocation(time.DateOnly, day, nytime.NewYork())
	if err != nil {
		panic(err)
	}
	at := d.Add(15*time.Hour + 30*time.Minute)
	return fmt.Sprintf(`{"activityId":%d,"time":%q,"accountNumber":%q,"type":"DIVIDEND_OR_INTEREST","status":"VALID","subAccount":"CASH",`+
		`"tradeDate":%q,"netAmount":%v,"description":"Ordinary Dividend~%s","transferItems":[{"instrument":{"assetType":"CURRENCY",`+
		`"status":"ACTIVE","symbol":"CURRENCY_USD","description":"USD currency","instrumentId":1,"closingPrice":0.0},"amount":%v,"cost":0.0}]}`,
		id, at.UTC().Format("2006-01-02T15:04:05-0700"), number, d.UTC().Format("2006-01-02T15:04:05-0700"), amount, symbol, amount), at
}

// cliJSON returns raws as `schwabtx transactions -json` prints them.
func cliJSON(t *testing.T, raws ...string) string {
	t.Helper()
	ms := make([]json.RawMessage, len(raws))
	for i, r := range raws {
		ms[i] = json.RawMessage(r)
	}
	var out strings.Builder
	if err := printRaw(&out, ms); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// refIn returns the first ref in s that re matches, failing the test if
// there is none.
func refIn(t *testing.T, s, re string) string {
	t.Helper()
	m := regexp.MustCompile(re).FindString(s)
	if m == "" {
		t.Fatalf("no ref like %s in:\n%s", re, s)
	}
	return m
}

// csvRows returns the lines of an export in CSV after its header.
func csvRows(t *testing.T, out string) []string {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("an export without a header: %q", out)
	}
	return lines[1:]
}

// What `schwabtx snapshot` writes, `schwabtx import` reads: two snapshots
// of the fixtures' three accounts, a day apart, are six snapshots of three
// accounts, standing for the two market dates before them; a second import
// adds nothing; status shows the accounts, their snapshots' dates and the
// sessions closed since, and export -what snapshots holds the six.
func TestImportWhatSnapshotWrites(t *testing.T) {
	setupFixtures(t)
	newStoreFile(t)
	file := filepath.Join(t.TempDir(), "snapshots.jsonl")
	mustRun(t, "snapshot", "-file", file)
	timeNow = func() time.Time { return testNow.Add(24 * time.Hour) }
	mustRun(t, "snapshot", "-file", file)

	out, _ := mustRun(t, "import", file)
	if want := "snapshots.jsonl: snapshot file, 2 lines: 6 new snapshots, 0 already held\n"; !strings.HasPrefix(out, want) {
		t.Errorf("import printed %q, want it to start %q", out, want)
	}
	out, _ = mustRun(t, "import", file)
	if want := "snapshots.jsonl: snapshot file, 2 lines: 0 new snapshots, 6 already held\n"; !strings.HasPrefix(out, want) {
		t.Errorf("a second import printed %q, want it to start %q", out, want)
	}
	// Two sessions, a Friday's and a Monday's, have closed since the second
	// snapshot by the Tuesday morning after.
	timeNow = func() time.Time { return testNow.Add(5 * 24 * time.Hour) }
	out, _ = mustRun(t, "status")
	for _, want := range []string{"...135", "...398", "...783", "2026-10-07 to 2026-10-08  0, and 2 sessions since\n", "schema 1, rules "} {
		if !strings.Contains(out, want) {
			t.Errorf("status has no %q:\n%s", want, out)
		}
	}
	out, stderr := mustRun(t, "export", "-what", "snapshots")
	if rows := csvRows(t, out); len(rows) != 6 {
		t.Errorf("export -what snapshots wrote %d rows, want 6:\n%s", len(rows), out)
	}
	if !strings.HasPrefix(stderr, "wrote 6 rows snapshots\n") {
		t.Errorf("export said %q", stderr)
	}
}

// A schwabtx transactions -json file of rows on four days covers the days
// strictly inside them (api_file); a History export of those days then
// links the rows the file holds, holds out a row it lacks on an inner day
// and counts one on an end day. status -review names the held-out row and
// the command that settles it; edit -link none counts it from the file, and
// the ledger then holds it.
func TestImportHistoryAndResolve(t *testing.T) {
	f, _ := setup(t)
	const number = "61803398"
	f.addAccount(number, 0, 0, testNow, time.Hour)
	var txs []fakeTx
	for i, d := range []struct {
		day, symbol string
		amount      float64
	}{{"2026-09-01", "ABCD", 3.21}, {"2026-09-08", "EFGH", 2}, {"2026-09-15", "ABCD", 1.5}, {"2026-09-22", "EFGH", 4}} {
		js, at := dividendRaw(int64(1001+i), number, d.day, d.symbol, d.amount)
		txs = append(txs, fakeTx{ID: int64(1001 + i), When: at, JSON: js})
	}
	f.addRows(number, txs...)
	doLogin(t)
	newStoreFile(t)
	dir := t.TempDir()
	cli, _ := mustRun(t, "transactions", "-json")
	cliFile := writeFile(t, dir, "transactions.json", cli)
	out, _ := mustRun(t, "import", cliFile)
	if want := "  A1 ...398: 4 rows, 4 new to the store, 0 already held\n"; !strings.Contains(out, want) {
		t.Errorf("importing the -json file printed %q, want %q", out, want)
	}

	export := historyJSON("09/01/2026", "09/22/2026",
		historyRow{"09/22/2026", "Bank Interest", "", "BANK INT 092226 SCHWAB BANK", "", "", "", "$0.11"},
		historyRow{"09/22/2026", "Qualified Dividend", "EFGH", "EFGH INC", "", "", "", "$4.00"},
		historyRow{"09/15/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$1.50"},
		historyRow{"09/10/2026", "Bank Interest", "", "BANK INT 091026 SCHWAB BANK", "", "", "", "$0.42"},
		historyRow{"09/08/2026", "Qualified Dividend", "EFGH", "EFGH INC", "", "", "", "$2.00"},
		historyRow{"09/01/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"},
	)
	name := writeFile(t, dir, "Individual_XXX398_Transactions_20261001-120000.json", export)
	out, _ = mustRun(t, "import", name)
	for _, want := range []string{
		"Individual_XXX398_Transactions_20261001-120000.json: History export (JSON) of A1 ...398,\n    2026-09-01 to 2026-09-22 (as the file states), 6 rows\n",
		"  6 transactions new to the store, 0 already held (another file had them)\n",
		"  linked to Schwab's API rows: 4; uncertain: 0; held out (inside the API's dates, no API row): 1;\n    before the API's history: 1\n",
		"; 1 item waits for you: schwabtx status -review\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("importing the export printed no %q:\n%s", want, out)
		}
	}

	review, _ := mustRun(t, "status", "-review")
	held := refIn(t, review, `C[0-9a-f]{8,}\.[0-9]+`)
	for _, want := range []string{
		"Held out: Bank Interest (1): ",
		"  " + held + "  A1 ...398  2026-09-10  Bank Interest  0.42\n",
		"      api_file: ",
		"      if no API row is it: schwabtx edit " + held + " -link none\n",
		"  if the API never shows such rows: schwabtx edit -action " + quoteArg("Bank Interest") + " -policy count\n",
	} {
		if !strings.Contains(review, want) {
			t.Errorf("status -review has no %q:\n%s", want, review)
		}
	}
	detail, _ := mustRun(t, "status", strings.ToLower(held))
	for _, want := range []string{`^` + held + `: file transaction of A1 \.\.\.398\n`, `\n  counted_why +held_out\n`, `\nFlags:\n  held_out: api_file\n`} {
		if !regexp.MustCompile(want).MatchString(detail) {
			t.Errorf("status %s has no %s:\n%s", held, want, detail)
		}
	}

	out, _ = mustRun(t, "edit", held, "-link", "none", "-note", "no API row")
	if want := held + ": not counted (held out: the API covers its dates) -> counted (your decision)\n"; !strings.Contains(out, want) {
		t.Errorf("edit -link none printed %q, want %q", out, want)
	}
	ledger, _ := mustRun(t, "export")
	if !strings.Contains(ledger, held+",1,...398,file,2026-09-10,") || len(csvRows(t, ledger)) != 6 {
		t.Errorf("the ledger after -link none:\n%s", ledger)
	}
	if out, _ := mustRun(t, "status", "-review"); out != "nothing waits for you\n" {
		t.Errorf("status -review after the edit: %q", out)
	}
}

// TestMain's child that writes to the store, for TestImportGlobOfStoreDir:
// it imports a snapshot line at a time, each in a write of its own; says
// ready after the first, then waits for a line on stdin before the rest.
const writerEnv = "SCHWABTX_TEST_WRITER"

// childWriter is the child's work: the store and how many writes are in its
// environment.
func childWriter() int {
	var path string
	var n int
	if _, err := fmt.Sscanf(os.Getenv(writerEnv), "%d %s", &n, &path); err != nil {
		fmt.Println("bad", writerEnv, err)
		return 2
	}
	ctx := context.Background()
	s, err := history.Open(ctx, history.Options{Path: path})
	if err != nil {
		fmt.Println("open:", err)
		return 1
	}
	defer s.Close()
	in := bufio.NewReader(os.Stdin)
	for i := range n {
		at := time.Date(2026, 9, 1+i, 22, 0, 0, 0, time.UTC)
		line, err := schwabfile.SnapshotLine(at, []json.RawMessage{json.RawMessage(`{"securitiesAccount":{"accountNumber":"33330789"}}`)})
		if err == nil {
			_, err = s.Import(ctx, []history.FileInput{{Name: "child.jsonl", Content: line}}, history.ImportOptions{})
		}
		if err != nil {
			fmt.Println("import:", err)
			return 1
		}
		if i == 0 {
			fmt.Println("ready")
			if _, err := in.ReadString('\n'); err != nil {
				fmt.Println("stdin:", err)
				return 1
			}
		}
	}
	fmt.Println("done")
	return 0
}

// Importing every file in the store's folder, while another process writes
// to the store, is refused for the store's own files, the store, its -wal
// and -shm and the lock beside it, before anything is read or the store is
// opened, and imports nothing; the same folder without them imports the
// snapshot file in it; and every write of the other process lands.
func TestImportGlobOfStoreDir(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	dir := filepath.Dir(store)
	writeFile(t, dir, "history.db.sync.lock", "")
	writeFile(t, dir, "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123"))

	const writes = 3
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), fmt.Sprintf("%s=%d %s", writerEnv, writes, store))
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(stdout)
	if !lines.Scan() || lines.Text() != "ready" {
		t.Fatalf("the child said %q (%v)", lines.Text(), lines.Err())
	}

	glob, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"history.db", "history.db-wal", "history.db-shm", "history.db.sync.lock", "snapshots.jsonl"} {
		if !slices.Contains(glob, filepath.Join(dir, want)) {
			t.Fatalf("the store's folder holds %q, without %s", glob, want)
		}
	}
	_, stderr, err := runTx(t, append([]string{"import"}, glob...)...)
	if err == nil || !strings.Contains(err.Error(), "is the store itself (or one of its files: history.db") ||
		!strings.HasSuffix(err.Error(), "; nothing was imported") || stderr != "" {
		t.Errorf("importing the store's folder: %v, warned %q", err, stderr)
	}

	if _, err := io.WriteString(stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	var rest []string
	for _, p := range glob {
		if _, ok := companionOf(p, store); !ok {
			rest = append(rest, p)
		}
	}
	out, _ := mustRun(t, append([]string{"import"}, rest...)...)
	if !strings.HasPrefix(out, "snapshots.jsonl: snapshot file, 1 line: 1 new snapshot, 0 already held\n") {
		t.Errorf("importing the folder without the store's files printed %q", out)
	}
	if !lines.Scan() || lines.Text() != "done" {
		t.Errorf("the child said %q (%v)", lines.Text(), lines.Err())
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
	if out, _ := mustRun(t, "export", "-what", "snapshots"); len(csvRows(t, out)) != writes+1 {
		t.Errorf("after the child's %d writes and the import, the store holds:\n%s", writes, out)
	}
}

// A backup taken before more imports, restored, gives the store back as it
// was: what was imported since is gone, and status reads it. A dry run
// says what each holds and changes nothing; a file that is not a store is
// refused.
func TestRestoreCommand(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	dir := t.TempDir()
	first := writeFile(t, dir, "first.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123"))
	second := writeFile(t, dir, "second.jsonl", snapshotFile(t, []time.Time{testNow.Add(24 * time.Hour)}, "11110123", "22220456"))
	mustRun(t, "import", first)
	backup := filepath.Join(dir, "backup.db")
	out, _ := mustRun(t, "store", "backup", backup)
	if !strings.HasPrefix(out, "backed the store up to "+backup) {
		t.Errorf("store backup printed %q", out)
	}
	if info, err := os.Stat(backup); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the backup: %v, %v", info, err)
	}
	if _, _, err := runTx(t, "store", "backup", backup); err == nil || !strings.Contains(err.Error(), "exists; choose a new name") {
		t.Errorf("a backup over a file: %v", err)
	}
	wal := store + "-wal"
	if _, _, err := runTx(t, "store", "backup", wal); err == nil || err.Error() != wal+" would replace the store itself (or one of its files); choose another name" {
		t.Errorf("a backup over the store's -wal: %v", err)
	}
	mustRun(t, "import", second)

	out, _ = mustRun(t, "store", "restore", "-dry-run", backup)
	if want := "the store held 2 accounts, 0 API transactions, 3 snapshots, 0 imported files, 0 decisions\n" +
		"the backup holds 1 accounts, 0 API transactions, 1 snapshots, 0 imported files, 0 decisions\nnothing was changed (-dry-run)\n"; out != want {
		t.Errorf("restore -dry-run printed %q, want %q", out, want)
	}
	out, stderr := mustRun(t, "store", "restore", backup)
	if !strings.Contains(out, "it now holds 1 accounts, 0 API transactions, 1 snapshots") ||
		!strings.Contains(stderr, "warning: restored the store from "+backup) {
		t.Errorf("restore printed %q, warned %q", out, stderr)
	}
	out, _ = mustRun(t, "status")
	if strings.Contains(out, "...456") || !strings.Contains(out, "A1 ...123") {
		t.Errorf("status after the restore:\n%s", out)
	}

	foreign := writeFile(t, dir, "notes.db", "not a store")
	if _, _, err := runTx(t, "store", "restore", foreign); err == nil {
		t.Error("a file that is not a store was restored")
	}
}

// edit -category sets a transaction's category, which the export then shows
// as yours, flags after the ref included; -category auto gives it back to
// the rule.
func TestClassifyOverride(t *testing.T) {
	offline(t)
	newStoreFile(t)
	raw, _ := dividendRaw(73508261949, "61803398", "2026-09-01", "ABCD", 3.21)
	mustRun(t, "import", writeFile(t, t.TempDir(), "transactions.json", cliJSON(t, raw)))
	entry := func() string {
		t.Helper()
		out, _ := mustRun(t, "export", "-what", "entries")
		rows := csvRows(t, out)
		if len(rows) != 1 {
			t.Fatalf("the entries:\n%s", out)
		}
		return rows[0]
	}
	before := entry()
	if !strings.Contains(before, ",dividend,") {
		t.Fatalf("the entry before: %s", before)
	}
	out, _ := mustRun(t, "edit", "t73508261949", "-category", "fee", "-note", "a fee, really")
	if want := "  T73508261949: dividend -> fee (yours; the rule said "; !strings.Contains(out, want) {
		t.Errorf("edit -category fee printed %q, want %q", out, want)
	}
	if got := entry(); !strings.Contains(got, ",fee,user,") {
		t.Errorf("the entry after -category fee: %s", got)
	}
	detail, _ := mustRun(t, "status", "T73508261949")
	if !strings.Contains(detail, "Your decisions:\n") || !strings.Contains(detail, "category  fee  a fee, really") {
		t.Errorf("status T73508261949 does not show the decision:\n%s", detail)
	}
	out, _ = mustRun(t, "edit", "-category", "auto", "T73508261949")
	if !strings.Contains(out, "  T73508261949: fee -> dividend (the rule ") {
		t.Errorf("edit -category auto printed %q", out)
	}
	for _, tc := range []struct{ args, err string }{
		{"T73508261949 -link none", "T73508261949 is an API transaction; -link takes a file transaction (C…)"},
		{"T1 -count no", "no transaction T1 in the store"},
	} {
		if _, _, err := runTx(t, append([]string{"edit"}, strings.Fields(tc.args)...)...); err == nil || !strings.HasPrefix(err.Error(), tc.err) {
			t.Errorf("edit %s: %v, want %s", tc.args, err, tc.err)
		}
	}
}

// No output of the store's commands holds a full account number, unless
// -show-accounts or -raw asks for one: the import's report, status, its
// review and its details, edit, the exports and the store's own commands.
// An export with -show-accounts to a file is readable only by you, and says
// that it holds full numbers.
func TestStoreCommandsMaskNumbers(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	dir := t.TempDir()
	const number, other = "61803398", "29470135"
	raw, _ := dividendRaw(1001, number, "2026-09-01", "ABCD", 3.21)
	raw = strings.Replace(raw, "Ordinary Dividend~ABCD", "JOURNAL FROM "+other+" ABCD", 1)
	var outputs []string
	run := func(args ...string) {
		t.Helper()
		stdout, stderr := mustRun(t, args...)
		outputs = append(outputs, args[0]+"\n"+stdout+stderr)
	}
	run("import", writeFile(t, dir, "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, number, other)),
		writeFile(t, dir, "transactions.json", cliJSON(t, raw)),
		writeFile(t, dir, "Joint "+other+"_XXX398_Transactions_20261001-120000.csv", historyCSV(
			historyRow{"09/01/2026", "Qualified Dividend", "ABCD", "ABCD INC " + number, "", "", "", "$3.21"},
			historyRow{"09/02/2026", "Journal", "", "JOURNAL TO " + other, "", "", "", "-$5.00"},
		)))
	for _, args := range [][]string{
		{"status"},
		{"status", "-review", "-all"},
		{"status", "T1001"},
		{"edit", "A1", "-kind", "taxable " + number},
		{"store", "backup", filepath.Join(dir, "backup.db")},
		{"export", "-what", "all", "-all", "-dir", filepath.Join(dir, "export")},
	} {
		run(args...)
	}
	f := refIn(t, strings.Join(outputs, "\n"), `F[0-9a-f]{8,}`)
	run("status", f)
	entries, err := os.ReadDir(filepath.Join(dir, "export"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, "export", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, e.Name()+"\n"+string(b))
	}
	for _, o := range outputs {
		for _, n := range []string{number, other} {
			if strings.Contains(o, n) {
				t.Errorf("an output holds %s:\n%s", n, o)
			}
		}
	}

	shown := filepath.Join(dir, "shown.csv")
	_, stderr := mustRun(t, "export", "-show-accounts", "-o", shown)
	if !strings.HasPrefix(stderr, "warning: this export holds full account numbers\n") {
		t.Errorf("export -show-accounts warned %q", stderr)
	}
	b, err := os.ReadFile(shown)
	if err != nil || !strings.Contains(string(b), number) {
		t.Errorf("the export with -show-accounts: %v\n%s", err, b)
	}
	if info, err := os.Stat(shown); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("the export with -show-accounts: %v, %v", info, err)
	}
	if out, _ := mustRun(t, "status", "-show-accounts"); !strings.Contains(out, "A1 "+number) {
		t.Errorf("status -show-accounts:\n%s", out)
	}
	if _, _, err := runTx(t, "status", "-account", "A2", "T1001"); err == nil || err.Error() != "no transaction T1001 in A2 ...135" {
		t.Errorf("status -account A2 T1001, a transaction of A1: %v", err)
	}
	for _, o := range []string{store, store + "-wal", filepath.Join(filepath.Dir(store), ".", "history.db-shm")} {
		if _, _, err := runTx(t, "export", "-o", o); err == nil || err.Error() != "-o "+o+" would replace the store itself (or one of its files); choose another name" {
			t.Errorf("export -o %s: %v", o, err)
		}
	}
	for _, what := range []string{"ledger,flags", "all"} {
		if _, _, err := runTx(t, "export", "-what", what); err == nil || err.Error() != "-what "+what+" needs -dir" {
			t.Errorf("export -what %s to stdout: %v", what, err)
		}
	}
}

// Every store command works without a login: no app key, no token file;
// and none makes one. (The other tests of the store's commands, but
// TestImportWhatSnapshotWrites and TestImportHistoryAndResolve, run without
// a login too: backup and restore in TestRestoreCommand, for one.)
func TestStoreCommandsNeedNoLogin(t *testing.T) {
	offline(t)
	newStoreFile(t)
	dir := t.TempDir()
	snap := writeFile(t, dir, "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123"))
	mustRun(t, "import", snap)
	mustRun(t, "edit", "A1", "-kind", "taxable")
	if out, _ := mustRun(t, "status", "a1"); !strings.HasPrefix(out, "A1 ...123: account\n") || !regexp.MustCompile(`\n  kind +taxable\n`).MatchString(out) {
		t.Errorf("status A1 after edit A1 -kind taxable:\n%s", out)
	}
	if out, _ := mustRun(t, "store", "check"); !strings.HasPrefix(out, "the store is sound: ") {
		t.Errorf("store check printed %q", out)
	}
	mustRun(t, "store", "rebuild")
	if _, err := os.Stat(os.Getenv("SCHWAB_TOKEN_FILE")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a store command made the token file: %v", err)
	}
}

// The commands that only read the store, and edit and the store's own
// (but restore), refuse a store that is not there and make none; an import
// makes one.
func TestReadCommandsNeedAStore(t *testing.T) {
	offline(t)
	dir := filepath.Join(t.TempDir(), "none")
	path := filepath.Join(dir, "history.db")
	t.Setenv("SCHWAB_STORE_FILE", path)
	for _, args := range [][]string{
		{"status"},
		{"status", "-review"},
		{"status", "A1"},
		{"export"},
		{"edit", "A1", "-kind", "ira"},
		{"edit", "-action", "Bank Interest", "-policy", "count"},
		{"import", "-forget", "F00000000"},
		{"store", "check"},
		{"store", "rebuild"},
		{"store", "backup", filepath.Join(t.TempDir(), "b.db")},
		{"export", "-store", filepath.Join(dir, "other.db")},
	} {
		_, _, err := runTx(t, args...)
		if err == nil || !strings.HasPrefix(err.Error(), "no store at ") || !strings.HasSuffix(err.Error(), " yet; schwabtx import makes one") {
			t.Errorf("%q: %v", args, err)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store's folder was made: %v", err)
	}
	// An import makes the store, and says so.
	snap := writeFile(t, t.TempDir(), "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123"))
	if _, stderr := mustRun(t, "import", snap); stderr != "new store at "+path+"\n" {
		t.Errorf("an import into a missing store said %q", stderr)
	}
}

// An import refuses a file of schwab.com that is not one it reads, by what
// it is, and then imports nothing; refuses digits that end two accounts'
// numbers, listing them; takes an account by its ref or its nickname; takes
// -force over the digits in a file's name; and says of a file imported
// again for another account how to move it, which edit -account then does.
func TestImportRefusesAndSelects(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	dir := t.TempDir()
	snap := writeFile(t, dir, "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123", "22220123"))
	bank, err := os.ReadFile(filepath.Join("..", "..", "internal", "history", "schwabfile", "testdata", "refuse", "bank-checking-2022.csv"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = runTx(t, "import", snap, writeFile(t, dir, "Checking_Transactions.csv", string(bank)))
	if err == nil || !strings.Contains(err.Error(), "Schwab Bank") || !strings.HasSuffix(err.Error(), "; nothing was imported") {
		t.Errorf("importing a bank export: %v", err)
	}
	if out, _ := mustRun(t, "import", snap); !strings.HasPrefix(out, "snapshots.jsonl: snapshot file, 1 line: 2 new snapshots") {
		t.Errorf("the snapshot file, after an import of it that was refused: %s", out)
	}

	s, err := history.Open(t.Context(), history.Options{Path: store})
	if err != nil {
		t.Fatal(err)
	}
	read := history.Read{SentAt: testNow, AnsweredAt: testNow}
	err = s.RecordPreferences(t.Context(), read, schwab.UserPreference{Accounts: []schwab.PreferenceAccount{
		{AccountNumber: "11110123", NickName: "Individual"}, {AccountNumber: "22220123", NickName: "Joint Account"},
	}})
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		t.Fatal(err)
	}

	export := historyCSV(historyRow{"09/01/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"})
	file := writeFile(t, dir, "Individual_XXX123_Transactions_20261001-120000.csv", export)
	if _, _, err := runTx(t, "import", "-account", "123", file); err == nil ||
		!strings.Contains(err.Error(), "A1 ...123 Individual and A2 ...123 Joint Account") {
		t.Errorf("-account 123: %v", err)
	}
	renamed := writeFile(t, dir, "Individual_XXX999_Transactions_20261001-120000.csv", export)
	if _, _, err := runTx(t, "import", "-account", "a1", renamed); err == nil || !strings.Contains(err.Error(), "-force") {
		t.Errorf("a file whose name's digits are another account's: %v", err)
	}
	out, _ := mustRun(t, "import", "-account", "joint account", file)
	if !strings.Contains(out, "of A2 ...123 Joint Account,") {
		t.Errorf("-account by nickname printed %q", out)
	}
	out, _ = mustRun(t, "import", "-account", "A1", "-force", renamed)
	f := refIn(t, out, `F[0-9a-f]{8,}`)
	if !strings.Contains(out, "warning: already imported as "+f+" for A2 ...123 Joint Account; if it is for another account: schwabtx edit "+f+" -account ACCOUNT\n") {
		t.Errorf("the same bytes again for A1 printed %q", out)
	}
	out, _ = mustRun(t, "edit", f, "-account", "A1")
	if out != f+" is now for A1 ...123 Individual (was A2 ...123 Joint Account); both accounts were derived again: recorded\n" {
		t.Errorf("edit %s -account A1 printed %q", f, out)
	}
	if out, _ := mustRun(t, "export", "-account", "A1"); len(csvRows(t, out)) != 1 {
		t.Errorf("A1's ledger after the move:\n%s", out)
	}
}

// shellWords splits a command as a POSIX shell does words in single and
// double quotes, which is what the commands printed for you to copy use.
func shellWords(t *testing.T, s string) []string {
	t.Helper()
	var words []string
	var w strings.Builder
	in, quote := false, rune(0)
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			w.WriteRune(r)
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ':
			if in {
				words, in = append(words, w.String()), false
				w.Reset()
			}
		default:
			w.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		t.Fatalf("an unended quote in %q", s)
	}
	if in {
		words = append(words, w.String())
	}
	return words
}

// The command that a symbol_unsure flag prints for you to copy names keys
// of option symbols, which hold spaces, quoted, so that the shell gives the
// keys back whole: run as split, it records your decision.
func TestPrintedCommandsQuoted(t *testing.T) {
	offline(t)
	newStoreFile(t)
	dir := t.TempDir()
	args := []string{"import", "-account", "61803398"}
	for i, symbol := range []string{"XYZ 09/18/2026 50.00 C", "XYZ1 09/18/2026 50.00 C"} {
		name := fmt.Sprintf("Individual_XXX398_Transactions_2026100%d-120000.csv", i+1)
		args = append(args, writeFile(t, dir, name, historyCSV(
			historyRow{"09/18/2026", "Expired", symbol, "CALL XYZ CORP $50 EXP 09/18/26", "-1", "", "", ""})))
	}
	mustRun(t, args...)
	review, _ := mustRun(t, "status", "-review")
	i := strings.Index(review, "schwabtx edit 'K:")
	if i < 0 {
		t.Fatalf("status -review has no command with a quoted key:\n%s", review)
	}
	command, _, _ := strings.Cut(review[i:], "\n")
	words := shellWords(t, command)
	if len(words) != 5 || words[3] != "-differ" || !strings.HasPrefix(words[2], "K:s:FILE:XYZ 09/18/2026 50.00 C@") {
		t.Fatalf("the command %q splits into %q", command, words)
	}
	out, _ := mustRun(t, words[1:]...)
	if !strings.HasPrefix(out, quoteArg(words[2])+" -differ "+quoteArg(words[4])+": recorded") {
		t.Errorf("%s printed %q", command, out)
	}
	if got := quoteArg(words[2]); runtime.GOOS != "windows" && got != "'"+words[2]+"'" || runtime.GOOS == "windows" && got != `"`+words[2]+`"` {
		t.Errorf("quoteArg(%q) = %s", words[2], got)
	}
}

// A write to a store that another process keeps locked gives up, once
// its wait is over, with a message that says so and what may hold it.
func TestBusyStore(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	db, err := sql.Open("sqlite", store)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK") //nolint:errcheck // The test is over.
	storeLockWait, storeBusyTimeout = 200*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { storeLockWait, storeBusyTimeout = 0, 0 })
	_, _, err = runTx(t, "edit", "-action", "Bank Interest", "-policy", "count")
	if err == nil || !strings.HasPrefix(err.Error(), "the store at "+store+" stayed locked by another process for 200ms while recording your decision; "+
		"nothing was saved; is a sync or an import running?") {
		t.Errorf("a write to a locked store: %v", err)
	}
}

// Flags come before or after the other arguments, and -- ends them.
func TestParseInterspersed(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		rest       []string
		category   string
		dryRun     bool
		wantErrStr string
	}{
		{args: []string{"T1", "-category", "fee"}, rest: []string{"T1"}, category: "fee"},
		{args: []string{"-category", "fee", "T1", "-dry-run", "T2"}, rest: []string{"T1", "T2"}, category: "fee", dryRun: true},
		{args: []string{"T1", "--", "-category", "fee"}, rest: []string{"T1", "-category", "fee"}},
		{args: []string{"T1", "-nope"}, wantErrStr: "flag provided but not defined: -nope"},
	} {
		fl := flag.NewFlagSet("schwabtx edit", flag.ContinueOnError)
		fl.SetOutput(io.Discard)
		category := fl.String("category", "", "")
		dryRun := fl.Bool("dry-run", false, "")
		rest, err := parseInterspersed(fl, tc.args)
		switch {
		case tc.wantErrStr != "":
			if err == nil || err.Error() != tc.wantErrStr {
				t.Errorf("%q: %v, want %s", tc.args, err, tc.wantErrStr)
			}
		case err != nil || !slices.Equal(rest, tc.rest) || *category != tc.category || *dryRun != tc.dryRun:
			t.Errorf("%q: %q, %q, %v, %v", tc.args, rest, *category, *dryRun, err)
		}
	}
}
