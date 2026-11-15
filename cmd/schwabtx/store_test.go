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
// the command that settles it; an export of the ledger says that it waits,
// and one of the entries, which holds it, does not; edit -link none counts
// it from the file, and the ledger then holds it.
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
		"  linked to Schwab's API rows: 4; uncertain: 0; held out (inside the API's dates, no API row): 1;\n    outside the API's dates (counted from the file): 1\n",
		"; 1 item waits for you: schwabtx status -review\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("importing the export printed no %q:\n%s", want, out)
		}
	}

	// status counts the file transactions linked, not their rows.
	if state, _ := mustRun(t, "status"); !strings.Contains(state, "; 4 file transactions linked to API rows\n") {
		t.Errorf("status printed\n%s", state)
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

	// The ledger leaves the held-out row out, and says so, after what each
	// source covers; the entries hold it, and say nothing of it.
	if _, stderr := mustRun(t, "export"); stderr != "wrote 5 rows ledger\n  A1 ...398: api_file 2026-09-02 to 2026-09-21; file_unknown 2026-09-01 to 2026-09-22\n"+
		"1 file transaction waits for you to resolve it (schwabtx status -review) and is not in this export\n" {
		t.Errorf("export of the ledger said %q", stderr)
	}
	if entries, stderr := mustRun(t, "export", "-what", "entries"); !strings.Contains(entries, held+",") || strings.Contains(stderr, "wait") {
		t.Errorf("export -what entries wrote\n%s\nand said %q", entries, stderr)
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
		"the backup holds 1 account, 0 API transactions, 1 snapshot, 0 imported files, 0 decisions\nnothing was changed (-dry-run)\n"; out != want {
		t.Errorf("restore -dry-run printed %q, want %q", out, want)
	}
	out, stderr := mustRun(t, "store", "restore", backup)
	if !strings.Contains(out, "it now holds 1 account, 0 API transactions, 1 snapshot,") ||
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

// A command printed for you to copy names the store with -store when it
// is not the store that a command opens without -store: the restore that
// store backup prints, run as printed from another folder, restores that
// store, which holds again what it held at the backup. Printed for the
// default store, the command names none.
func TestPrintedCommandsNameTheStore(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	t.Setenv("SCHWAB_STORE_FILE", "")
	dir := t.TempDir()
	first := writeFile(t, dir, "first.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123"))
	second := writeFile(t, dir, "second.jsonl", snapshotFile(t, []time.Time{testNow.Add(24 * time.Hour)}, "22220456"))
	mustRun(t, "import", "-store", store, first)
	t.Chdir(dir)
	out, _ := mustRun(t, "store", "-store", store, "backup", "backup.db")
	printed, ok := strings.CutSuffix(out, " puts it back\n")
	_, command, _ := strings.Cut(printed, "; ")
	words := shellWords(t, command)
	if !ok || len(words) != 6 || !slices.Equal(words[:5], []string{"schwabtx", "store", "-store", store, "restore"}) ||
		!filepath.IsAbs(words[5]) || filepath.Base(words[5]) != "backup.db" {
		t.Fatalf("store backup printed %q", out)
	}
	mustRun(t, "import", "-store", store, second)
	t.Chdir(t.TempDir())
	mustRun(t, words[1:]...)
	if out, _ := mustRun(t, "status", "-store", store); strings.Contains(out, "...456") || !strings.Contains(out, "A1 ...123") {
		t.Errorf("status of the store after %s:\n%s", command, out)
	}

	// The commands that the store composes name it too: the import's
	// completeness hint, review's commands, and the refusal of a file set
	// aside, whose command brings it back.
	export := writeFile(t, dir, "export.csv", historyCSV(historyRow{"09/01/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"}))
	out, _ = mustRun(t, "import", "-store", store, "-account", "A1", export)
	f := refIn(t, out, `F[0-9a-f]{8,}`)
	named := "schwabtx edit -store " + quoteArg(store) + " " + f + " -completeness complete"
	if !strings.Contains(out, named) {
		t.Errorf("the import printed %q, want %q in it", out, named)
	}
	if out, _ := mustRun(t, "status", "-store", store, "-review", "-all"); !strings.Contains(out, named) ||
		strings.Contains(strings.ReplaceAll(out, "schwabtx edit -store "+quoteArg(store), ""), "schwabtx edit") {
		t.Errorf("status -review printed\n%s\nwant every command naming the store", out)
	}
	if out, _ := mustRun(t, "import", "-store", store, "-forget", f); !strings.HasPrefix(out,
		"set aside "+f+" (export.csv): 1 transaction that no other file holds was set aside, counted in the ledger\n") {
		t.Errorf("import -forget printed %q", out)
	}
	_, _, err := runTx(t, "import", "-store", store, "-account", "A1", export)
	_, command, ok = strings.Cut(fmt.Sprint(err), "bring it back with: ")
	command, _, _ = strings.Cut(command, ";")
	if words := shellWords(t, command); !ok || !slices.Equal(words, []string{"schwabtx", "import", "-store", store, "-forget", f, "-undo"}) {
		t.Fatalf("importing a file set aside: %v", err)
	}
	mustRun(t, shellWords(t, command)[1:]...)
	if out, _ := mustRun(t, "status", "-store", store, f); !regexp.MustCompile(`\n  forgotten +0\n`).MatchString(out) {
		t.Errorf("after %s, %s reads\n%s", command, f, out)
	}

	t.Setenv("SCHWAB_STORE_FILE", store)
	out, _ = mustRun(t, "store", "backup", filepath.Join(dir, "default.db"))
	if want := "; schwabtx store restore " + quoteArg(filepath.Join(dir, "default.db")) + " puts it back\n"; !strings.HasSuffix(out, want) {
		t.Errorf("store backup of the default store printed %q, want it to end %q", out, want)
	}
}

// edit -category sets a transaction's category, which the export then shows
// as yours, flags after the ref included; -category auto gives it back to
// the rule. An edit with an option that is refused records none of its
// options.
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
		// An edit refused for one of its options records none of them.
		{"T73508261949 -category fee -count maybe", `a count decision is yes, no or auto, not "maybe"`},
		{"T73508261949 -category fee -pair T1", "no transaction T1 in the store"},
	} {
		if _, _, err := runTx(t, append([]string{"edit"}, strings.Fields(tc.args)...)...); err == nil || !strings.HasPrefix(err.Error(), tc.err) {
			t.Errorf("edit %s: %v, want %s", tc.args, err, tc.err)
		}
	}
	if got := entry(); got != before {
		t.Errorf("after edits that were refused, the entry is %s, want %s", got, before)
	}
}

// No output of the store's commands holds a full account number, unless
// -show-accounts or -raw asks for one: the import's report, status, its
// review and its details, edit, the exports, those for Portfolio
// Performance and their refusals among them, and the store's own commands.
// An export with -show-accounts to a file is readable only by you, and says
// that it holds full numbers; so are the files of an export for a tool,
// which writes nothing to stdout.
func TestStoreCommandsMaskNumbers(t *testing.T) {
	offline(t)
	store := newStoreFile(t)
	dir := t.TempDir()
	const number, other = "61803398", "29470135"
	raw, _ := dividendRaw(1001, number, "2026-09-01", "ABCD", 3.21)
	raw = strings.Replace(raw, "Ordinary Dividend~ABCD", "JOURNAL FROM "+other+" ABCD", 1)
	var outputs []string
	run := func(args ...string) (stdout, stderr string) {
		t.Helper()
		stdout, stderr = mustRun(t, args...)
		outputs = append(outputs, args[0]+"\n"+stdout+stderr)
		return stdout, stderr
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
		{"status", "A1"},
		{"store", "backup", filepath.Join(dir, "backup.db")},
		{"export", "-what", "all", "-all", "-dir", filepath.Join(dir, "export")},
	} {
		run(args...)
	}
	f := refIn(t, strings.Join(outputs, "\n"), `F[0-9a-f]{8,}`)
	run("status", f)
	// A number given to a flag of dates is said back masked, as every
	// value an error says back is.
	for _, args := range [][]string{
		{"edit", f, "-from", "6180-3398", "-to", "2026-10-01"},
		{"export", "-to", number},
		{"import", "-from", "6180 3398", "-to", "2026-10-01", filepath.Join(dir, "transactions.json")},
	} {
		_, _, err := runTx(t, args...)
		if err == nil || !strings.Contains(err.Error(), `"...398" is not a date`) {
			t.Errorf("%q: %v", args, err)
		}
		outputs = append(outputs, fmt.Sprint(err))
	}
	// The exports for a tool: one of an account that -account names by its
	// whole number, which nothing says back; and one that goes on from it,
	// with its account, and gives the tool what it gave, writing nothing,
	// and names the account by its label in the full export of a redo.
	// Each writes its files to its folder, readable only by you, and to
	// stderr what it wrote, the accounts to make in the tool and the files
	// to import. (TestToolExport writes Ghostfolio's files, and
	// TestToolSummary says what an export for Ghostfolio says.)
	pp, next := filepath.Join(dir, "pp"), filepath.Join(dir, "pp-next")
	for _, c := range []struct {
		folder      string
		args        []string
		head, names string
		files       []string
	}{
		{
			pp,
			[]string{"export", "-format", "pp", "-account", number, "-dir", pp},
			"wrote 2 rows for Portfolio Performance to " + pp + ", up to 2026-10-07: 1 account file\n" +
				"  this assumes Portfolio Performance holds none of these accounts' rows: a first import into new, empty accounts, or a redo of every account (docs/tools.md)\n",
			"\nin Portfolio Performance, make a USD deposit account and a securities account for each account, named:\n  A1 ...398: Schwab A1 ...398\n" +
				"import in this order: securities.csv, account-transactions-A1-398.csv\n",
			[]string{"account-transactions-A1-398.csv", "manifest.json", "pp-account-transactions.json", "pp-portfolio-transactions.json", "pp-securities.json", "securities.csv"},
		},
		{
			next,
			[]string{"export", "-format", "pp", "-since", pp, "-dir", next},
			"wrote 0 rows for Portfolio Performance to " + next + ", since " + pp + ", up to 2026-10-07\n" +
				"  this assumes Portfolio Performance holds " + pp + " and every export before it, each imported once, with what it listed deleted and ticked\n",
			"\nimport in this order: securities.csv\n" +
				"a redo (docs/tools.md) imports a full export of this chain's selection: schwabtx export -format pp -account A1 -dir <a new folder>\n" +
				"next time: schwabtx export -format pp -since " + quoteArg(next) + " -dir <a new folder>\n",
			[]string{"manifest.json", "pp-account-transactions.json", "pp-portfolio-transactions.json", "pp-securities.json", "securities.csv"},
		},
	} {
		stdout, stderr := run(c.args...)
		if stdout != "" || !strings.HasPrefix(stderr, c.head) || !strings.Contains(stderr, c.names) {
			t.Errorf("%q printed %q and said:\n%s", c.args, stdout, stderr)
		}
		entries, err := os.ReadDir(c.folder)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
			if info, err := e.Info(); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v, %v; want a file readable only by you", e.Name(), info, err)
			}
		}
		if !slices.Equal(names, c.files) {
			t.Errorf("%q wrote %q, want %q", c.args, names, c.files)
		}
		if info, err := os.Stat(c.folder); err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want a folder readable only by you", c.folder, info, err)
		}
	}
	// Going on from the export that the second one went on from already is
	// refused, before the store is opened. A -manual symbol that no security
	// has, beside one of the store, is said back masked, as every value an
	// error says back is, once the store is read and before anything is
	// written.
	refused := filepath.Join(dir, "refused")
	if _, _, err := runTx(t, "export", "-format", "pp", "-since", pp, "-dir", refused); err == nil ||
		err.Error() != "-since "+pp+": "+next+" already went on from "+pp+"; name it with -since if you imported it, or move it away if you did not" {
		t.Errorf("export -since an export that another went on from: %v", err)
	}
	_, _, err := runTx(t, "export", "-format", "ghostfolio", "-dir", refused, "-manual", "ABCD,6180 3398")
	if err == nil || err.Error() != "-manual names ...398, which no security in the store has" {
		t.Errorf("export -manual of no security's symbol: %v", err)
	}
	if _, serr := os.Stat(refused); !errors.Is(serr, fs.ErrNotExist) {
		t.Errorf("the refused export made its folder: %v", serr)
	}
	outputs = append(outputs, fmt.Sprint(err))
	for _, folder := range []string{filepath.Join(dir, "export"), pp, next} {
		entries, err := os.ReadDir(folder)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := os.ReadFile(filepath.Join(folder, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			outputs = append(outputs, e.Name()+"\n"+string(b))
		}
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

// An export for a tool refuses, before it opens a store, the flags that are
// for csv and jsonl; a folder that holds an export already, whatever its
// files' case, or one whose files include the store's; and a -since that
// names no manifest of an export of the format and layout, or one from
// which an export in a folder beside it went on already. Refused, it makes
// no folder. csv and jsonl refuse the tools' flags, and the folder of an
// export for a tool. Each export here that is not refused goes on to open
// the store, which is not there.
func TestExportToolRefusals(t *testing.T) {
	offline(t)
	dir := t.TempDir()
	// manifest writes a folder of dir with a manifest.json of content and
	// the other files named, and returns its path.
	manifest := func(name, content string, files ...string) string {
		t.Helper()
		folder := filepath.Join(dir, name)
		if err := os.Mkdir(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			writeFile(t, folder, "manifest.json", content)
		}
		for _, f := range files {
			writeFile(t, folder, f, "")
		}
		return folder
	}
	// head returns what a tool export's manifest says of the export, as
	// the checks read it: when it was made (at), its format, its chain's
	// id and the export that it went on from ("null" for none).
	head := func(at, format, chain, since string) string {
		if since != "null" {
			since = `"` + since + `"`
		}
		return fmt.Sprintf(`{"exported_at": %q, "format": %q, "layout": %d, "filters": {"since": %s}, "chain": {"id": %q}}`,
			at, format, history.ToolLayout, since, chain)
	}
	const t1, t2, t3 = "2026-10-01T12:00:00Z", "2026-10-15T12:00:00Z", "2026-10-20T12:00:00Z"
	// A chain of three exports, the second gone on from the first, the
	// third from the second; beside them, what does not go on from them:
	// an export of the other format, one of another chain, a file.
	first := manifest("pp-1", head(t1, "pp", t1, "null"), "securities.csv", "pp-securities.json")
	manifest("pp-2", head(t2, "pp", t1, t1))
	third := manifest("pp-3", head(t3, "pp", t1, t2))
	manifest("gf-2", head(t2, "ghostfolio", t1, t3))
	manifest("pp-9", head(t2, "pp", "2026-01-01T00:00:00Z", t3))
	writeFile(t, dir, "notes.txt", "")
	gf := manifest("gf-1", head(t1, "ghostfolio", t1, "null"), "ghostfolio.json")
	layout := manifest("pp-layout", strings.Replace(head(t1, "pp", t1, "null"), fmt.Sprintf(`"layout": %d`, history.ToolLayout), `"layout": 99`, 1))
	csv := manifest("csv", `{"exported_at": "2026-10-01T12:00:00Z", "format": "csv", "filters": {"account": null}}`, "ledger.csv")
	notJSON := manifest("broken", "{")
	noManifest := manifest("empty", "")
	upper := manifest("upper", "", "PP-Account-Transactions-V2.JSON")
	upperManifest := manifest("upper-manifest", "", "Manifest.json")
	noExport := manifest("no-export", strings.Replace(head(t1, "pp", t1, "null"), `"exported_at": "`+t1+`", `, "", 1))
	noChain := manifest("no-chain", strings.Replace(head(t1, "pp", t1, "null"), `, "chain": {"id": "`+t1+`"}`, "", 1))
	perAccount := manifest("accounts", "", "notes.txt", "portfolio-transactions-A3-120.csv")
	gfOnly := manifest("gf-only", "", "Ghostfolio.json")
	other := manifest("other", "", "notes.txt", "ledger.csv", "transactions.json")
	out := filepath.Join(dir, "out")

	// The store named as an export's file, in the folder that -dir names.
	storeDir := filepath.Join(dir, "store")
	storeLike := func(name string) string { return filepath.Join(storeDir, name) }

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-format", "pp", "-o", "ledger.csv"}, "-format pp writes several files and a manifest: give -dir DIR, not -o"},
		{[]string{"-format", "ghostfolio", "-o", "x", "-dir", out}, "-format ghostfolio writes several files and a manifest: give -dir DIR, not -o"},
		{[]string{"-format", "pp"}, "-format pp writes several files and a manifest: give -dir DIR"},
		{[]string{"-format", "pp", "-dir", out, "-what", "ledger,entries"}, "-format pp writes the ledger: -what is for csv and jsonl"},
		{[]string{"-format", "ghostfolio", "-dir", out, "-what", "all"}, "-format ghostfolio writes the ledger: -what is for csv and jsonl"},
		{[]string{"-format", "ghostfolio", "-dir", out, "-all"}, "-format ghostfolio writes counted rows only: -all is for csv and jsonl"},
		{[]string{"-format", "pp", "-dir", out, "-raw"}, "-format pp never writes full account numbers: -raw and -show-accounts are for csv and jsonl"},
		// Given, whatever its value.
		{[]string{"-format", "pp", "-dir", out, "-show-accounts=false"}, "-format pp never writes full account numbers: -raw and -show-accounts are for csv and jsonl"},
		{
			[]string{"-format", "pp", "-dir", out, "-from", "2026-01-01"},
			"-format pp writes every row from the account's first, so that each position has its opening rows: -from is for csv and jsonl",
		},
		{[]string{"-format", "pp", "-dir", out, "-manual", "ABCD"}, "-manual is for -format ghostfolio"},
		{[]string{"-manual", "ABCD"}, "-manual is for -format ghostfolio"},
		{[]string{"-format", "jsonl", "-since", third, "-o", "x"}, "-since is for -format pp and ghostfolio"},
		{[]string{"-format", "pp", "-dir", out, "-since", ""}, "-since needs a folder: the one of the export that you imported last"},
		{[]string{"-format", "ghostfolio", "-dir", out, "-to", "2026-02-30"}, `-to "...230" is not a date: give YYYY-MM-DD or MM/DD/YYYY`},
		{[]string{"-format", "pp", "-dir", filepath.Join(dir, "notes.txt")}, "-dir " + filepath.Join(dir, "notes.txt") + ": "},
		{[]string{"-format", "pp", "-dir", out, "-since", third, "-account", "A1"}, "-since " + third + " goes on from the export in " + third + ", with its account: drop -account"},
		// A folder that holds an export, by its manifest or a file of the
		// format's names in any case; -since naming -dir is one.
		{[]string{"-format", "pp", "-dir", first}, "-dir " + first + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		{[]string{"-format", "ghostfolio", "-dir", third, "-since", third}, "-dir " + third + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		{[]string{"-format", "pp", "-dir", upper}, "-dir " + upper + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		{[]string{"-format", "ghostfolio", "-dir", upperManifest}, "-dir " + upperManifest + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		{[]string{"-format", "pp", "-dir", perAccount}, "-dir " + perAccount + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		{[]string{"-format", "ghostfolio", "-dir", gfOnly}, "-dir " + gfOnly + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		{[]string{"-format", "pp", "-dir", csv}, "-dir " + csv + " holds an export already: write each export to a new folder, so that none is overwritten before it is imported"},
		// A -since that is no manifest of an export of the format and
		// layout.
		{[]string{"-format", "pp", "-dir", out, "-since", noManifest}, "-since " + noManifest + ": " + filepath.Join(noManifest, "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		{[]string{"-format", "pp", "-dir", out, "-since", notJSON}, "-since " + notJSON + ": " + filepath.Join(notJSON, "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		{[]string{"-format", "pp", "-dir", out, "-since", csv}, "-since " + csv + ": " + filepath.Join(csv, "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		{[]string{"-format", "ghostfolio", "-dir", out, "-since", third}, "-since " + third + ": " + filepath.Join(third, "manifest.json") + " is not the manifest of a -format ghostfolio export of layout 1"},
		{[]string{"-format", "pp", "-dir", out, "-since", layout}, "-since " + layout + ": " + filepath.Join(layout, "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		{[]string{"-format", "pp", "-dir", out, "-since", noExport}, "-since " + noExport + ": " + filepath.Join(noExport, "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		{[]string{"-format", "pp", "-dir", out, "-since", noChain}, "-since " + noChain + ": " + filepath.Join(noChain, "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		// One that an export beside it went on from already.
		{
			[]string{"-format", "pp", "-dir", out, "-since", first},
			"-since " + first + ": " + filepath.Join(dir, "pp-2") + " already went on from " + first + "; name it with -since if you imported it, or move it away if you did not",
		},
		// An export into the store's folder over one of its files.
		{[]string{"-format", "pp", "-store", storeLike("securities.csv"), "-dir", storeDir}, "-dir " + storeDir + " would put securities.csv over the store itself (or one of its files); choose another folder"},
		{[]string{"-format", "pp", "-store", storeLike("pp-portfolio-transactions.json"), "-dir", storeDir}, "-dir " + storeDir + " would put pp-portfolio-transactions.json over the store itself (or one of its files); choose another folder"},
		{[]string{"-format", "ghostfolio", "-store", storeLike("ghostfolio.json"), "-dir", storeDir}, "-dir " + storeDir + " would put ghostfolio.json over the store itself (or one of its files); choose another folder"},
		{[]string{"-format", "ghostfolio", "-store", storeLike("manifest.json"), "-dir", storeDir}, "-dir " + storeDir + " would put manifest.json over the store itself (or one of its files); choose another folder"},
		// csv and jsonl into the folder of an export for a tool.
		{[]string{"-what", "all", "-dir", first}, "-dir " + first + " holds a pp export: write this one to another folder"},
		{[]string{"-format", "jsonl", "-dir", gf}, "-dir " + gf + " holds a ghostfolio export: write this one to another folder"},
		// What none of these refuses: the store is opened, and it is not
		// there.
		{[]string{"-format", "pp", "-dir", out, "-since", third, "-what", "ledger"}, "no store at "},
		{[]string{"-format", "ghostfolio", "-dir", out, "-since", gf, "-manual", "ABCD", "-to", "2026-10-31"}, "no store at "},
		{[]string{"-format", "pp", "-dir", noManifest, "-account", "A1"}, "no store at "},
		{[]string{"-format", "pp", "-dir", other}, "no store at "},
		{[]string{"-format", "ghostfolio", "-dir", perAccount}, "no store at "},
		{[]string{"-format", "ghostfolio", "-dir", out, "-store", storeLike("securities.csv")}, "no store at "},
		{[]string{"-what", "all", "-dir", csv}, "no store at "},
	} {
		// A message that ends in a space is the start of one, which the
		// rest, said by the system, ends.
		_, _, err := runTx(t, append([]string{"export"}, c.args...)...)
		if err == nil || !strings.HasPrefix(err.Error(), c.want) || !strings.HasSuffix(c.want, " ") && err.Error() != c.want {
			t.Errorf("export %q: %v, want %s", c.args, err, c.want)
		}
	}
	for _, folder := range []string{out, storeDir} {
		if _, err := os.Stat(folder); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("a refused export made %s: %v", folder, err)
		}
	}
}

// The -since check reads of each manifest beside the folder only what it
// needs: the keys that an export writes first, before its files and the
// chain's given, which grow with the history. So a manifest whose given
// holds years of rows reads whole when cut a few kilobytes after them; and
// one whose keys come in another order is read up to the last of them.
func TestDecodeToolManifest(t *testing.T) {
	const head = `{
  "exported_at": "2026-10-20T12:00:00.000Z",
  "schema": 1,
  "rules": 1,
  "calendar": 1,
  "store": "2023-05-01T14:00:00.000Z",
  "format": "pp",
  "layout": 1,
  "filters": {"account": null, "to": "2026-10-19", "manual": [], "since": "2026-10-15T12:00:00.000Z"},
  "chain": {"id": "2026-10-01T12:00:00.000Z", "account": null, "manual": [], "to": "2026-10-19"}`
	var given strings.Builder
	given.WriteString(`"given": {"A1 ...135": {`)
	for i := range 20_000 {
		if i > 0 {
			given.WriteString(", ")
		}
		fmt.Fprintf(&given, `"T7%09d": {"day": "2026-01-02", "as": "portfolio 2026-01-02 Buy 100.00", "fp": "0123456789abcdef"}`, i)
	}
	given.WriteString("}}")
	tail := `"files": [], "accounts": [], ` + given.String()
	want := toolManifest{ExportedAt: "2026-10-20T12:00:00.000Z", Format: "pp", Layout: 1}
	want.Chain.ID, want.Filters.Since = "2026-10-01T12:00:00.000Z", "2026-10-15T12:00:00.000Z"
	for _, c := range []struct {
		name string
		r    io.Reader
	}{
		{"cut after its head", io.LimitReader(strings.NewReader(head+",\n  "+tail+"}"), int64(len(head)+4<<10))},
		{"its keys in another order", strings.NewReader("{" + tail + "," + head[1:] + "}")},
	} {
		if got, err := decodeToolManifest(c.r); err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", c.name, got, err, want)
		}
	}
}

// What an export for a tool says on stderr (8.2 of the tool exports'
// design), from what it wrote, in full and going on from another: what it
// wrote, the coverage, what it left out, wrote with a caveat, found changed
// in symbol, in the holdings or, for PP, in the cash, could not check, by
// kind and why, flagged or did not write for want of a decision, and for
// Ghostfolio the cash rows that the balances carry, the
// -manual symbols that nothing uses, said back masked, and the symbols
// Yahoo may no longer know; what to delete in the tool; for PP, the
// accounts to make, the order of the files and the rows to tick; for
// Ghostfolio, the cash to set; and the command that goes on from it, which
// names the store when it is not the default one. Each line but the first
// is there only when it has something to say, said for one or for several.
func TestToolSummary(t *testing.T) {
	store := filepath.Join(t.TempDir(), "history.db")
	t.Setenv("SCHWAB_STORE_FILE", store)
	a1 := history.ToolAccount{Label: "A1 ...135", Account: "...135", Nickname: "Individual", Name: "Schwab A1 ...135", Exported: true}
	a2 := history.ToolAccount{Label: "A2 ...398", Account: "...398", Nickname: "Options & Margin", Name: "Schwab A2 ...398", Exported: true}
	newly := func(a history.ToolAccount) history.ToolAccount {
		a.New = true
		return a
	}
	withCash := func(a history.ToolAccount, balances int, cash string, last nytime.Date) history.ToolAccount {
		a.Balances, a.Cash, a.LastBalance = balances, cash, last
		return a
	}
	evening := time.Date(2026, time.November, 10, 23, 5, 0, 0, time.UTC) // 18:05 in New York
	for _, c := range []struct {
		name, store, dir, since string
		e                       history.ToolExport
		want                    string
	}{
		{
			name: "pp in full", dir: "pp-2026-11-13",
			e: history.ToolExport{
				Tool: "pp", To: "2026-11-12", Written: 17,
				Files: []history.ToolFile{
					{Name: "securities.csv", Kind: "securities", Rows: 23},
					{Name: "account-transactions-A1-135.csv", Kind: "account", Account: "A1 ...135", Rows: 6},
					{Name: "portfolio-transactions-A1-135.csv", Kind: "portfolio", Account: "A1 ...135", Rows: 4},
					{Name: "account-transactions-A2-398.csv", Kind: "account", Account: "A2 ...398", Rows: 3, Tick: 1},
					{Name: "portfolio-transactions-A2-398.csv", Kind: "portfolio", Account: "A2 ...398", Rows: 5},
				},
				Securities: 9, Prices: 14,
				Coverage: []history.ExportCoverage{
					{AccountID: 1, Account: "...135", Source: "api_file", From: "2023-06-13", To: "2025-06-01"},
					{AccountID: 1, Account: "...135", Source: "file", From: "2023-02-01", To: "2024-05-31"},
					{AccountID: 1, Account: "...135", Source: "realized", From: "2024-01-01", To: "2024-05-31"},
					{AccountID: 1, Account: "...135", Source: "snapshot", From: "2026-11-02", To: "2026-11-10"},
					{AccountID: 2, Account: "...398", Source: "api_file", From: "2025-01-15", To: "2026-08-03"},
					{AccountID: 2, Account: "...398", Source: "snapshot", From: "2026-11-02", To: "2026-11-10"},
				},
				LeftOut:    []history.ToolCount{{Code: "other", Count: 1}},
				CaveatRows: 1, Caveats: []history.ToolCount{{Code: "assumed_multiplier", Count: 1}},
				SymbolsChanged: 2, Holdings: 2, Cash: 2, Flagged: 1, Waiting: 1,
				// An account with snapshots alone, as the walk-through's third.
				NotInTool: []string{"A3 ...783"},
				Accounts:  []history.ToolAccount{newly(a1), newly(a2)},
				Tick:      []history.ToolTick{{File: "account-transactions-A2-398.csv", Ref: "T73508261958", Account: "A2 ...398", As: "account 2026-03-02 Withdrawal 1000.00"}},
			},
			want: `wrote 17 rows for Portfolio Performance to pp-2026-11-13, up to 2026-11-12: 2 account files, 2 portfolio files, 9 securities, 14 prices
  this assumes Portfolio Performance holds none of these accounts' rows: a first import into new, empty accounts, or a redo of every account (docs/tools.md)
  A1 ...135: api_file 2023-06-13 to 2025-06-01; file 2023-02-01 to 2024-05-31; realized 2024-01-01 to 2024-05-31; snapshot 2026-11-02 to 2026-11-10
  A2 ...398: api_file 2025-01-15 to 2026-08-03; snapshot 2026-11-02 to 2026-11-10
left out 1 row (pp-2026-11-13/manifest.json lists each, with what to do): other 1
wrote 1 row with a caveat (pp-2026-11-13/manifest.json): assumed_multiplier 1
2 securities may be held by Portfolio Performance otherwise than written, or may no longer trade (pp-2026-11-13/manifest.json lists each under symbols_changed, with why): on a first import into empty accounts they ask nothing; on a redo, Portfolio Performance holds them from before: set each as docs/tools.md step 8 says before you import
2 holdings differ from the latest snapshot (pp-2026-11-13/manifest.json): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)
2 accounts' cash in Portfolio Performance differs from their latest snapshots' (pp-2026-11-13/manifest.json): cash held before the store's first row of each, or rows left out; book each difference, less the rows left out that you book by hand, as an opening deposit or withdrawal (docs/tools.md)
1 account has snapshots and nothing in Portfolio Performance (pp-2026-11-13/manifest.json lists it under holdings.not_checked): A3 ...783 has no row that this export or its chain writes or names; import its History files (docs/store.md) to have it there
1 row of this export carries a flag that needs you (schwabtx status -review); if you uncount it, the next -since export lists what to delete
1 file transaction waits for you to resolve it (schwabtx status -review) and is not in this export
in Portfolio Performance, make a USD deposit account and a securities account for each account, named:
  A1 ...135 (Individual): Schwab A1 ...135
  A2 ...398 (Options & Margin): Schwab A2 ...398
import in this order: securities.csv, account-transactions-A1-135.csv, portfolio-transactions-A1-135.csv, account-transactions-A2-398.csv, portfolio-transactions-A2-398.csv
tick these rows, which Portfolio Performance will show as existing although they are new; leave every other such row unticked:
  account-transactions-A2-398.csv: Schwab A2 ...398: 2026-03-02 Withdrawal 1000.00 (T73508261958)
next time: schwabtx export -format pp -since pp-2026-11-13 -dir <a new folder>
`,
		},
		{
			// The full export's wording of symbols_changed, singular: a
			// first import, or a redo, of which Portfolio Performance may
			// hold the security from before.
			name: "pp in full, one security named", dir: "pp-2026-11-14",
			e: history.ToolExport{
				Tool: "pp", To: "2026-11-13", Written: 1, SymbolsChanged: 1, Securities: 1,
				Files:    []history.ToolFile{{Name: "securities.csv", Kind: "securities", Rows: 1}, {Name: "portfolio-transactions-A1-135.csv", Kind: "portfolio", Account: "A1 ...135", Rows: 1}},
				Accounts: []history.ToolAccount{newly(a1)},
			},
			want: `wrote 1 row for Portfolio Performance to pp-2026-11-14, up to 2026-11-13: 1 portfolio file, 1 security
  this assumes Portfolio Performance holds none of these accounts' rows: a first import into new, empty accounts, or a redo of every account (docs/tools.md)
1 security may be held by Portfolio Performance otherwise than written, or may no longer trade (pp-2026-11-14/manifest.json lists it under symbols_changed, with why): on a first import into empty accounts it asks nothing; on a redo, Portfolio Performance holds it from before: set it as docs/tools.md step 8 says before you import
in Portfolio Performance, make a USD deposit account and a securities account for each account, named:
  A1 ...135 (Individual): Schwab A1 ...135
import in this order: securities.csv, portfolio-transactions-A1-135.csv
next time: schwabtx export -format pp -since pp-2026-11-14 -dir <a new folder>
`,
		},
		{
			name: "pp going on", dir: "pp-2026-12-01", since: "pp-2026-11-13",
			e: history.ToolExport{
				Tool: "pp", To: "2026-11-30", Since: "2026-11-13T12:00:00Z", SinceTo: "2026-11-12", Written: 3, Late: 2, SameAs: 1, RedoAccount: "A1", SymbolsChanged: 1,
				// Each kind of account not checked, two of each, and both whys
				// of the cash check's.
				CashNotChecked: []history.ToolCashUnchecked{{Account: "A1 ...135", Why: "no_cash"}, {Account: "A2 ...398", Why: "margin"}},
				NoSnapshot:     []string{"A4 ...783", "A5 ...678"},
				NotInTool:      []string{"A6 ...246", "A7 ...579"},
				Files: []history.ToolFile{
					{Name: "securities.csv", Kind: "securities", Rows: 22},
					{Name: "account-transactions-A1-135.csv", Kind: "account", Account: "A1 ...135", Rows: 1},
					{Name: "portfolio-transactions-A1-135.csv", Kind: "portfolio", Account: "A1 ...135", Rows: 2, Tick: 1},
				},
				Securities: 2, Prices: 20,
				Accounts: []history.ToolAccount{a1, {Label: "A2 ...398", Account: "...398", Name: "Schwab A2 ...398"}, {Label: "A3 ...777"}},
				Changed: []history.ToolChange{
					{Ref: "T73508261950", Account: "A1 ...135", Was: "account 2024-04-04 Dividend 3.86", Now: "account 2024-04-04 Dividend 3.68"},
				},
				Gone: []history.ToolChange{
					{
						Ref: "T73508261953", Account: "A1 ...135", Was: "account 2025-02-10 Transfer (Inbound) 500.00",
						Transfer: "A2 ...398 T73508261952 to A1 ...135 T73508261953", Why: "uncounted: user",
					},
					{Ref: "T73508261944", Account: "A2 ...398", Was: "portfolio 2025-01-21 Buy 400.67", Why: "uncounted: user"},
					{Ref: "T73508261948", Account: "A2 ...398", Was: "account 2025-02-03 Deposit 25.00", NoteRef: "Ca3b4c5d6.4", Why: "linked to T73508261949"},
					{Ref: "C9e8d7c6b.2", Account: "A3 ...777", Was: "portfolio 2025-03-04 Sell 10.00; account 2025-03-04 Fees 15.00", Why: "not in this export: no longer in the store, or dated after -to"},
				},
				Tick: []history.ToolTick{{File: "portfolio-transactions-A1-135.csv", Ref: "T73508261960", Account: "A1 ...135", As: "portfolio 2026-11-20 Buy 1001.25"}},
			},
			want: `wrote 3 rows for Portfolio Performance to pp-2026-12-01, since pp-2026-11-13, up to 2026-11-30: 1 account file, 1 portfolio file, 2 securities, 20 prices
  this assumes Portfolio Performance holds pp-2026-11-13 and every export before it, each imported once, with what it listed deleted and ticked
  2 of them are dated on or before 2026-11-12, the end of pp-2026-11-13: they came late or were decided since
  1 of them is a row that Portfolio Performance holds under another ref (pp-2026-12-01/manifest.json lists it under same_as): it shows it as existing; leave it unticked
1 security has changed symbol or may no longer trade (pp-2026-12-01/manifest.json lists it under symbols_changed, with why): before you import, set it as docs/tools.md step 8 says if Portfolio Performance holds it; if it is new there, it asks nothing
2 accounts' cash in Portfolio Performance is not checked (pp-2026-12-01/manifest.json lists them under holdings.cash_not_checked): Schwab A1 ...135, whose latest snapshot holds no cash figure (take another with schwabtx snapshot), and Schwab A2 ...398, a margin account with a margin or short balance at its latest snapshot: compare the cash that Schwab shows for each with the balance of its deposit account in Portfolio Performance, and book the difference as step 4 says (docs/tools.md step 4)
2 accounts have rows and no snapshot to check against (pp-2026-12-01/manifest.json lists them under holdings.not_checked): A4 ...783 and A5 ...678: take one of each with schwabtx snapshot (an account that Schwab has closed has none)
2 accounts have snapshots and nothing in Portfolio Performance (pp-2026-12-01/manifest.json lists them under holdings.not_checked): A6 ...246 and A7 ...579 have no row that this export or its chain writes or names; import their History files (docs/store.md) to have them there
before importing, delete in Portfolio Performance what changed or went (pp-2026-12-01/manifest.json lists each):
  Schwab A1 ...135: 2024-04-04 Dividend 3.86 (note schwabtx A1 ...135 T73508261950): changed; the new form is in this export
  Schwab A1 ...135: 2025-02-10 Transfer (Inbound) 500.00 (note schwabtx A2 ...398 T73508261952 to A1 ...135 T73508261953): gone, uncounted (user)
  Schwab A2 ...398: 2025-01-21 Buy 400.67 (note schwabtx A2 ...398 T73508261944): gone, uncounted (user)
  Schwab A2 ...398: 2025-02-03 Deposit 25.00 (note schwabtx A2 ...398 Ca3b4c5d6.4): gone, linked to T73508261949
  A3 ...777: 2025-03-04 Sell 10.00; 2025-03-04 Fees 15.00 (note schwabtx A3 ...777 C9e8d7c6b.2): gone, not in this export (no longer in the store, or dated after -to)
import in this order: securities.csv, account-transactions-A1-135.csv, portfolio-transactions-A1-135.csv
tick these rows, which Portfolio Performance will show as existing although they are new; leave every other such row unticked:
  portfolio-transactions-A1-135.csv: Schwab A1 ...135: 2026-11-20 Buy 1001.25 (T73508261960)
a redo (docs/tools.md) imports a full export of this chain's selection: schwabtx export -format pp -account A1 -dir <a new folder>
next time: schwabtx export -format pp -since pp-2026-12-01 -dir <a new folder>
`,
		},
		{
			name: "pp going on, one of each", dir: "pp-2026-12-15", since: "pp-2026-12-01",
			e: history.ToolExport{
				Tool: "pp", To: "2026-12-14", Since: "2026-12-01T12:00:00Z", SinceTo: "2026-11-30", Written: 1, Late: 1,
				Files:      []history.ToolFile{{Name: "securities.csv", Kind: "securities", Rows: 1}, {Name: "portfolio-transactions-A3-777.csv", Kind: "portfolio", Account: "A3 ...777", Rows: 1}},
				Securities: 1, Prices: 1, CaveatRows: 2, Caveats: []history.ToolCount{{Code: "value_unknown", Count: 2}, {Code: "cash_not_written", Count: 1}},
				LeftOut:        []history.ToolCount{{Code: "split", Count: 1}, {Code: "no_quantity", Count: 2}},
				SymbolsChanged: 2, Holdings: 1, Cash: 1, CashNotChecked: []history.ToolCashUnchecked{{Account: "A2 ...398", Why: "margin"}}, Flagged: 2, Waiting: 2,
				// A3, new, has rows and no snapshot; A4 snapshots alone.
				NoSnapshot: []string{"A3 ...777"}, NotInTool: []string{"A4 ...783"},
				// A security with no symbol is named by its key.
				DifferencesChanged: []history.ToolDifference{
					{Account: "A1 ...135", Key: "c:30303M102", Was: "2", Now: "1", Change: "-1"},
					{Account: "A1 ...135", Key: "i:1854092", Symbol: "XYZ", Was: "-5", Change: "5"},
					{Account: "A1 ...135", Cash: true, Was: "1295.65", Now: "997.04", Change: "-298.61"},
				},
				Unsure: []history.ToolUnsure{
					{Key: "x:66000017", Tickers: []string{"JKLM"}, TakenFor: "i:1000013", Securities: []history.ToolUnsureSecurity{{Key: "i:1000013", CUSIP: "ZZJ12K106"}, {Key: "i:1000213", CUSIP: "ZZJ12K205"}}},
					{
						Key: "s:FILE:GRPC FD@2025-01-02", Keys: []string{"s:FILE:GRPC FD@2025-01-02", "x:66000102"}, Tickers: []string{"GRPB", "GRPC-FD"},
						Securities: []history.ToolUnsureSecurity{{Key: "i:1000102", CUSIP: "ZZG10B101"}, {CUSIP: "ZZG10D105"}, {Key: "i:1000103", CUSIP: "ZZG10C109"}},
					},
					// Keys whose ticker another security had at another time:
					// on their own, beside one of a CUSIP or none, and taken for
					// the one of two that had it while it did.
					{Key: "x:66000302", Tickers: []string{"TKRZ"}, Securities: []history.ToolUnsureSecurity{{Key: "i:1000301", CUSIP: "ZZM40A100", Apart: true}}},
					{Key: "i:1000303", Tickers: []string{"TKRY"}, Securities: []history.ToolUnsureSecurity{{Key: "x:66000303", Apart: true}}},
					{
						Key: "x:66000501", Tickers: []string{"GRPZ"}, TakenFor: "c:ZZS50B109",
						Securities: []history.ToolUnsureSecurity{{Key: "i:1000501", CUSIP: "ZZR50A102", Apart: true}, {Key: "c:ZZS50B109", CUSIP: "ZZS50B109"}},
					},
				},
				Accounts: []history.ToolAccount{newly(history.ToolAccount{Label: "A3 ...777", Account: "...777", Nickname: "Joint\u202eTrust", Name: "Schwab A3 ...777", Exported: true}), a1},
			},
			want: `wrote 1 row for Portfolio Performance to pp-2026-12-15, since pp-2026-12-01, up to 2026-12-14: 1 portfolio file, 1 security, 1 price
  this assumes Portfolio Performance holds pp-2026-12-01 and every export before it, each imported once, with what it listed deleted and ticked
  1 of them is dated on or before 2026-11-30, the end of pp-2026-12-01: it came late or was decided since
left out 3 rows (pp-2026-12-15/manifest.json lists each, with what to do): split 1, no_quantity 2
wrote 2 rows with caveats (pp-2026-12-15/manifest.json): value_unknown 2, cash_not_written 1
2 securities have changed symbol or may no longer trade (pp-2026-12-15/manifest.json lists each under symbols_changed, with why): before you import, set each that Portfolio Performance holds as docs/tools.md step 8 says; one that is new to it asks nothing
5 keys of the store with no CUSIP had tickers that other securities have had too (pp-2026-12-15/manifest.json lists them): the export cannot tell which each is; say which in the store (docs/tools.md):
  x:66000017 (JKLM), written as i:1000013, which had it first: schwabtx edit K:x:66000017 -same K:i:1000013 (ZZJ12K106) or -same K:i:1000213 (ZZJ12K205)
  s:FILE:GRPC FD@2025-01-02 (GRPB, GRPC-FD), on its own, its tickers each of another: schwabtx edit QUOTED -same K:i:1000102 (ZZG10B101) or -same K:i:1000103 (ZZG10C109)
  x:66000302 (TKRZ), on its own, each of the others having had its ticker at another time: schwabtx edit K:x:66000302 -same K:i:1000301 (ZZM40A100)
  i:1000303 (TKRY), on its own, each of the others having had its ticker at another time: schwabtx edit K:i:1000303 -same K:x:66000303
  x:66000501 (GRPZ), written as c:ZZS50B109, the others having had its ticker at other times: schwabtx edit K:x:66000501 -same K:i:1000501 (ZZR50A102) or -same K:c:ZZS50B109 (ZZS50B109)
1 holding differs from the latest snapshot (pp-2026-12-15/manifest.json): a position opened before the store's rows, or a split left out; add it by hand, less the rows left out that you book by hand (docs/tools.md)
1 account's cash in Portfolio Performance differs from its latest snapshot's (pp-2026-12-15/manifest.json): cash held before the store's first row of the account, or rows left out; book the difference, less the rows left out that you book by hand, as an opening deposit or withdrawal (docs/tools.md)
1 account's cash in Portfolio Performance is not checked (pp-2026-12-15/manifest.json lists it under holdings.cash_not_checked): A2 ...398, a margin account with a margin or short balance at its latest snapshot: compare the cash that Schwab shows for it with the balance of its deposit account in Portfolio Performance, and book the difference as step 4 says (docs/tools.md step 4)
1 account has rows and no snapshot to check against (pp-2026-12-15/manifest.json lists it under holdings.not_checked): Schwab A3 ...777: take one with schwabtx snapshot (an account that Schwab has closed has none)
1 account has snapshots and nothing in Portfolio Performance (pp-2026-12-15/manifest.json lists it under holdings.not_checked): A4 ...783 has no row that this export or its chain writes or names; import its History files (docs/store.md) to have it there
2 rows of this export carry a flag that needs you (schwabtx status -review); if you uncount one, the next -since export lists what to delete
2 file transactions wait for you to resolve them (schwabtx status -review) and are not in this export
3 differences that pp-2026-12-01 listed have changed or gone (pp-2026-12-15/manifest.json lists each, with was): before importing, change what you added in Portfolio Performance by hand for each by its change, less the rows left out that you book by hand, or delete it: Portfolio Performance marks a new row equal to one that it holds as existing (docs/tools.md):
  Schwab A1 ...135: c:30303M102: 1, was 2: change by -1
  Schwab A1 ...135: XYZ: gone, was -5: delete
  Schwab A1 ...135: cash: 997.04, was 1295.65: change by -298.61
in Portfolio Performance, make a USD deposit account and a securities account for each account, named:
  A3 ...777 (Joint?Trust): Schwab A3 ...777
import in this order: securities.csv, portfolio-transactions-A3-777.csv
next time: schwabtx export -format pp -since pp-2026-12-15 -dir <a new folder>
`,
		},
		{
			// Rows under new refs, which PP marks as existing, and a row of the
			// same form under its own Note in place of another's.
			name: "pp going on, new refs", dir: "pp-2026-12-20", since: "pp-2026-12-15",
			e: history.ToolExport{
				Tool: "pp", To: "2026-12-19", Since: "2026-12-15T12:00:00Z", SinceTo: "2026-12-14", Written: 3, SameAs: 2,
				Files:    []history.ToolFile{{Name: "securities.csv", Kind: "securities", Rows: 1}, {Name: "portfolio-transactions-A1-135.csv", Kind: "portfolio", Account: "A1 ...135", Rows: 3}},
				Accounts: []history.ToolAccount{a1},
				Changed: []history.ToolChange{
					{Ref: "T73508261962", Account: "A1 ...135", Was: "portfolio 2026-01-07 Buy 60.00", NoteRef: "C9e8d7c6b.5", Now: "portfolio 2026-01-07 Buy 60.00"},
				},
			},
			want: `wrote 3 rows for Portfolio Performance to pp-2026-12-20, since pp-2026-12-15, up to 2026-12-19: 1 portfolio file
  this assumes Portfolio Performance holds pp-2026-12-15 and every export before it, each imported once, with what it listed deleted and ticked
  2 of them are rows that Portfolio Performance holds under another ref (pp-2026-12-20/manifest.json lists them under same_as): it shows them as existing; leave them unticked
before importing, delete in Portfolio Performance what changed or went (pp-2026-12-20/manifest.json lists each):
  Schwab A1 ...135: 2026-01-07 Buy 60.00 (note schwabtx A1 ...135 C9e8d7c6b.5): changed; the new form is in this export
import in this order: securities.csv, portfolio-transactions-A1-135.csv
next time: schwabtx export -format pp -since pp-2026-12-20 -dir <a new folder>
`,
		},
		{
			// A row's was as a manifest edited by hand may hold it, without
			// its file's kind.
			name: "pp going on, nothing new", dir: "pp-2026-12-31", since: "pp-2026-12-15",
			e: history.ToolExport{
				Tool: "pp", To: "2026-12-30", Since: "2026-12-15T12:00:00Z", SinceTo: "2026-12-14", Given: 21,
				Files:          []history.ToolFile{{Name: "securities.csv", Kind: "securities"}},
				Accounts:       []history.ToolAccount{a1},
				CashNotChecked: []history.ToolCashUnchecked{{Account: "A1 ...135", Why: "no_cash"}},
				Gone:           []history.ToolChange{{Ref: "T73508261961", Account: "A1 ...135", Was: "2026-12-01 Fees 1.00", Why: "uncounted: user"}},
			},
			want: `wrote 0 rows for Portfolio Performance to pp-2026-12-31, since pp-2026-12-15, up to 2026-12-30
  this assumes Portfolio Performance holds pp-2026-12-15 and every export before it, each imported once, with what it listed deleted and ticked
1 account's cash in Portfolio Performance is not checked (pp-2026-12-31/manifest.json lists it under holdings.cash_not_checked): Schwab A1 ...135, whose latest snapshot holds no cash figure (take another with schwabtx snapshot): compare the cash that Schwab shows for it with the balance of its deposit account in Portfolio Performance, and book the difference as step 4 says (docs/tools.md step 4)
before importing, delete in Portfolio Performance what changed or went (pp-2026-12-31/manifest.json lists each):
  Schwab A1 ...135: 2026-12-01 Fees 1.00 (note schwabtx A1 ...135 T73508261961): gone, uncounted (user)
import in this order: securities.csv
next time: schwabtx export -format pp -since pp-2026-12-31 -dir <a new folder>
`,
		},
		{
			name: "ghostfolio in full", dir: "gf-2026-11-13",
			e: history.ToolExport{
				Tool: "ghostfolio", To: "2026-11-12", Written: 10,
				Files:    []history.ToolFile{{Name: "ghostfolio.json", Kind: "ghostfolio", Rows: 12}},
				Accounts: []history.ToolAccount{withCash(newly(a1), 10, "10234.56", "2026-11-10"), withCash(newly(a2), 8, "-12.34", "2026-11-10")},
				Profiles: 5, PricesOnly: 2, Yahoo: 6, InBalances: 6,
				NotInBalances: []history.ToolCashRows{{Account: "A1 ...135", Rows: 2, Snapshot: "2026-11-10", Taken: evening}},
				Unused:        []string{"ABCD", "61803398", "XY\u200bZ"},
				YahooIdle:     3, IdleSince: "2025-11-12",
			},
			want: `wrote 12 activities for Ghostfolio to gf-2026-11-13, up to 2026-11-12: 2 accounts with 18 balances, 5 manual assets (2 more with prices only), 6 symbols for Yahoo
  this assumes Ghostfolio holds none of these accounts: a first import, or a redo of every account (docs/tools.md)
6 deposits, withdrawals and journals are in the accounts' balances; 2 are not yet, posted after the latest snapshot was taken (A1 ...135: of 2026-11-10, taken 2026-11-10 18:05 in New York), which a later one will carry
-manual names ABCD and ...398 and XY?Z, which nothing in this export uses
3 symbols for Yahoo have no row since 2025-11-12 and are not held (gf-2026-11-13/manifest.json lists them): Yahoo may no longer know them; look them up, and name those it lacks in one -manual
Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
next time: schwabtx export -format ghostfolio -since gf-2026-11-13 -dir <a new folder>
`,
		},
		{
			name: "ghostfolio going on", store: filepath.Join(t.TempDir(), "other.db"), dir: "gf-2026-12-01", since: "gf-2026-11-13",
			e: history.ToolExport{
				Tool: "ghostfolio", To: "2026-11-30", Since: "2026-11-13T12:00:00Z", SinceTo: "2026-11-12", Written: 4, Late: 1,
				RedoManual: []string{"BND", "efgh"},
				Files:      []history.ToolFile{{Name: "ghostfolio.json", Kind: "ghostfolio", Rows: 1}},
				Accounts:   []history.ToolAccount{withCash(a1, 12, "10234.56", "2026-11-28"), withCash(a2, 10, "-12.34", "2026-11-28"), {Label: "A3 ...777", Name: "Schwab A3 ...777"}},
				Profiles:   1, Yahoo: 1,
				NotInBalances: []history.ToolCashRows{{Account: "A2 ...398", Rows: 1, Snapshot: "2026-11-28", Taken: evening.AddDate(0, 0, 18)}, {Account: "A3 ...777", Rows: 1}},
				LeftOut:       []history.ToolCount{{Code: "gf_cash", Count: 2}},
				Holdings:      3, Flagged: 1, YahooIdle: 1, IdleSince: "2025-11-30",
				// A3's cash row has no snapshot to be carried by, nor its
				// holdings to be checked against.
				NoSnapshot: []string{"A3 ...777"},
				Unsure: []history.ToolUnsure{
					{Key: "x:66000017", Tickers: []string{"JKLM"}, TakenFor: "i:1000013", Securities: []history.ToolUnsureSecurity{{Key: "i:1000013", CUSIP: "ZZJ12K106"}, {Key: "i:1000213", CUSIP: "ZZJ12K205"}}},
				},
				Changed: []history.ToolChange{{Ref: "T73508261950", Account: "A1 ...135", Was: "2024-04-04 DIVIDEND 3.86", Now: "2024-04-04 DIVIDEND 3.68"}},
				Gone:    []history.ToolChange{{Ref: "T73508261945", Account: "A2 ...398", Was: "2025-01-22 SELL 412.50 fee 0.65", Why: "left out: gf_cash"}},
				// A security is named by its asset, as Ghostfolio shows it: a
				// MANUAL asset by its name. One that Ghostfolio is to hold
				// under another asset now, after -manual or a rename, is to
				// move there, with its change.
				DifferencesChanged: []history.ToolDifference{
					{Account: "A2 ...398", Key: "c:30303M102", Asset: "Security (30303M102)", Was: "2", Now: "1", Change: "-1"},
					{Account: "A1 ...135", Key: "i:1854092", Symbol: "XYZ", Asset: "Security (XYZ)", WasAsset: "XYZ", Was: "10", Now: "10", Change: "0"},
					{Account: "A2 ...398", Key: "i:1000877", Symbol: "ZZTB", Asset: "ZZTB", WasAsset: "ZZTA", Was: "4", Now: "3", Change: "-1"},
				},
			},
			want: `wrote 1 activity for Ghostfolio to gf-2026-12-01, since gf-2026-11-13, up to 2026-11-30: 2 accounts with 22 balances, 1 manual asset, 1 symbol for Yahoo
  this assumes Ghostfolio holds gf-2026-11-13 and every export before it, each imported once, with what it listed deleted
  1 of their rows is dated on or before 2026-11-12, the end of gf-2026-11-13: it came late or was decided since
2 deposits, withdrawals and journals are not in the accounts' balances yet, posted after the latest snapshot was taken (A2 ...398: of 2026-11-28, taken 2026-11-28 18:05 in New York; A3 ...777: none), which a later one will carry
left out 2 rows (gf-2026-12-01/manifest.json lists each, with what to do): gf_cash 2
1 key of the store with no CUSIP had a ticker that other securities have had too (gf-2026-12-01/manifest.json lists it): the export cannot tell which it is; say which in the store (docs/tools.md):
  x:66000017 (JKLM), written as i:1000013, which had it first: schwabtx edit -store STORE K:x:66000017 -same K:i:1000013 (ZZJ12K106) or -same K:i:1000213 (ZZJ12K205)
3 holdings differ from the latest snapshot (gf-2026-12-01/manifest.json): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)
1 account has rows and no snapshot to check against (gf-2026-12-01/manifest.json lists it under holdings.not_checked): Schwab A3 ...777: take one with schwabtx snapshot (an account that Schwab has closed has none)
1 row of this export carries a flag that needs you (schwabtx status -store STORE -review); if you uncount it, the next -since export lists what to delete
1 symbol for Yahoo has no row since 2025-11-30 and is not held (gf-2026-12-01/manifest.json lists it): Yahoo may no longer know it; look it up, and if Yahoo lacks it, name it in -manual
before importing, delete in Ghostfolio what changed or went (gf-2026-12-01/manifest.json lists each):
  Schwab A1 ...135: 2024-04-04 DIVIDEND 3.86 (comment schwabtx A1 ...135 T73508261950): changed; the new form is in this export
  Schwab A2 ...398: 2025-01-22 SELL 412.50 fee 0.65 (comment schwabtx A2 ...398 T73508261945): gone, left out (gf_cash)
3 differences that gf-2026-11-13 listed have changed or gone (gf-2026-12-01/manifest.json lists each, with was): after importing, change what you added in Ghostfolio by hand for each by its change, less the rows left out that you book by hand, or move it to the asset that Ghostfolio holds the security under now, or delete it (docs/tools.md):
  Schwab A2 ...398: Security (30303M102): 1, was 2: change by -1
  Schwab A1 ...135: Security (XYZ): 10, was 10 under XYZ: delete it under XYZ and add it under Security (XYZ)
  Schwab A2 ...398: ZZTB: 3, was 4 under ZZTA: delete it under ZZTA and add it under ZZTB, changed by -1
Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
set each account's cash in Ghostfolio to its latest snapshot, which imports do not change:
  Schwab A1 ...135: 10234.56 (2026-11-28)
  Schwab A2 ...398: -12.34 (2026-11-28)
a redo (docs/tools.md) imports a full export of this chain's selection: schwabtx export -store STORE -format ghostfolio -manual BND,efgh -dir <a new folder>
next time: schwabtx export -store STORE -format ghostfolio -since gf-2026-12-01 -dir <a new folder>
`,
		},
		{
			name: "ghostfolio, nothing to import", dir: "gf-2026-12-31", since: "gf-2026-12-01",
			e: history.ToolExport{
				Tool: "ghostfolio", To: "2026-12-30", Since: "2026-12-01T12:00:00Z", SinceTo: "2026-11-30",
				Files:      []history.ToolFile{{Name: "ghostfolio.json", Kind: "ghostfolio"}},
				Accounts:   []history.ToolAccount{withCash(newly(history.ToolAccount{Label: "A3 ...777", Name: "Schwab A3 ...777", Exported: true}), 2, "40.00", "2026-12-30")},
				PricesOnly: 2, InBalances: 3,
			},
			want: `wrote 0 activities for Ghostfolio to gf-2026-12-31, since gf-2026-12-01, up to 2026-12-30: 1 account with 2 balances, 2 manual assets with prices only
  this assumes Ghostfolio holds gf-2026-12-01 and every export before it, each imported once, with what it listed deleted
3 deposits, withdrawals and journals are in the accounts' balances
Ghostfolio takes an account's balances only in the import that makes the account; later imports leave its cash as it is
set each account's cash in Ghostfolio to its latest snapshot, which imports do not change:
  Schwab A3 ...777: 40.00 (2026-12-30)
this file holds no activity: Ghostfolio's import dialog cannot import it; its accounts and balances come with the next export that has one, or by POST /api/v1/import
next time: schwabtx export -format ghostfolio -since gf-2026-12-31 -dir <a new folder>
`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := store
			if c.store != "" {
				path = c.store
			}
			var b strings.Builder
			toolSummary(&b, path, &c.e, c.dir, c.since)
			want := strings.NewReplacer(
				"/manifest.json", string(filepath.Separator)+"manifest.json", "-store STORE", "-store "+quoteArg(path), "QUOTED", quoteArg("K:s:FILE:GRPC FD@2025-01-02"),
			).Replace(c.want)
			if got := b.String(); got != want {
				t.Errorf("it says:\n%s\nwant:\n%s", got, want)
			}
		})
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
// (but restore), refuse a store that is not there and make none; so do a
// dry run of an import and an import of a file it refuses; an import makes
// one, and says so.
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
	// Nor does a dry run of an import, or an import of a file it refuses.
	snap := writeFile(t, t.TempDir(), "snapshots.jsonl", snapshotFile(t, []time.Time{testNow}, "11110123"))
	if out, stderr := mustRun(t, "import", "-dry-run", snap); !strings.HasPrefix(out, "snapshots.jsonl: snapshot file, 1 line: 1 new snapshot, 0 already held\n") ||
		!strings.HasSuffix(out, "\nnothing was saved (-dry-run)\n") || stderr != "" {
		t.Errorf("a dry run into a missing store printed %q, warned %q", out, stderr)
	}
	bank, err := os.ReadFile(filepath.Join("..", "..", "internal", "history", "schwabfile", "testdata", "refuse", "bank-checking-2022.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if _, stderr, err := runTx(t, "import", writeFile(t, t.TempDir(), "Checking_Transactions.csv", string(bank))); err == nil || stderr != "" {
		t.Errorf("importing a bank export into a missing store: %v, warned %q", err, stderr)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store's folder was made: %v", err)
	}
	// Nor does an import that the store refuses once it is open: an export
	// whose account nothing gives.
	export := writeFile(t, t.TempDir(), "Transactions.csv", historyCSV(historyRow{"09/01/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"}))
	if _, stderr, err := runTx(t, "import", export); err == nil || !strings.Contains(err.Error(), "use -account") || stderr != "" {
		t.Errorf("importing an export of no account into a missing store: %v, warned %q", err, stderr)
	}
	for _, f := range history.Companions(path) {
		if _, err := os.Stat(f); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("an import refused left %s: %v", f, err)
		}
	}
	// An import makes the store, and says so.
	if _, stderr := mustRun(t, "import", snap); stderr != "new store at "+path+"\n" {
		t.Errorf("an import into a missing store said %q", stderr)
	}
}

// An import refuses a file of schwab.com that is not one it reads, by what
// it is, and then imports nothing; refuses digits that end two accounts'
// numbers, listing them; takes an account by its ref or its nickname; takes
// -force over the digits in a file's name; and says of a file imported
// again for another account how to move it, which edit -account then does.
// -forget refuses the import's other flags, and then sets nothing aside; a
// file set aside is refused when it is imported again.
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
	if !strings.Contains(out, "warning: already imported as "+f+" for A2 ...123 Joint Account; if it is for another account: schwabtx edit "+f+" -account ACCOUNT\n") ||
		!strings.Contains(out, "\n0 files imported (1 already held) into "+store+";") {
		t.Errorf("the same bytes again for A1 printed %q", out)
	}
	out, _ = mustRun(t, "edit", f, "-account", "A1")
	if out != f+" is now for A1 ...123 Individual (was A2 ...123 Joint Account); both accounts were derived again: recorded\n" {
		t.Errorf("edit %s -account A1 printed %q", f, out)
	}
	if out, _ := mustRun(t, "export", "-account", "A1"); len(csvRows(t, out)) != 1 {
		t.Errorf("A1's ledger after the move:\n%s", out)
	}
	// -forget takes none of the import's other flags, and then sets
	// nothing aside.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-dry-run"}, "-dry-run does not go with -forget, which takes only -undo"},
		{[]string{"-account", "A1", "-force"}, "-account and -force do not go with -forget, which takes only -undo"},
	} {
		if _, _, err := runTx(t, append([]string{"import", "-forget", f}, tc.args...)...); err == nil || err.Error() != tc.want {
			t.Errorf("import -forget %s %q: %v, want %s", f, tc.args, err, tc.want)
		}
	}
	if out, _ := mustRun(t, "export", "-account", "A1"); len(csvRows(t, out)) != 1 {
		t.Errorf("A1's ledger after -forget was refused:\n%s", out)
	}
	// Set aside, it is refused when it is imported again.
	mustRun(t, "import", "-forget", f)
	if _, _, err := runTx(t, "import", "-account", "A1", "-force", renamed); err == nil || err.Error() != filepath.Base(renamed)+" was set aside as "+f+
		"; bring it back with: schwabtx import -forget "+f+" -undo; nothing was imported" {
		t.Errorf("importing a file set aside: %v", err)
	}
}

// withStore puts -store after the first word of each command of schwabtx
// that acts on a store, in any message, and only when the store is not
// the default one.
func TestWithStore(t *testing.T) {
	dir := t.TempDir()
	def, other := filepath.Join(dir, "default.db"), filepath.Join(dir, "other.db")
	t.Setenv("SCHWAB_STORE_FILE", def)
	flag := "-store " + quoteArg(other)
	for _, c := range [][2]string{
		{"say so: schwabtx edit F1 -completeness complete", "say so: schwabtx edit " + flag + " F1 -completeness complete"},
		{
			"if it is: schwabtx edit 'C1.2' -link T3; if not: schwabtx edit C1.2 -reject T3",
			"if it is: schwabtx edit " + flag + " 'C1.2' -link T3; if not: schwabtx edit " + flag + " C1.2 -reject T3",
		},
		{"bring it back with: schwabtx import -forget F1 -undo", "bring it back with: schwabtx import " + flag + " -forget F1 -undo"},
		{"then schwabtx store restore BACKUP", "then schwabtx store " + flag + " restore BACKUP"},
		{"(schwabtx status -review)", "(schwabtx status " + flag + " -review)"},
		{"schwabtx export -store x.db -what ledger", "schwabtx export -store x.db -what ledger"},
		{"no store at x yet; schwabtx import makes one", "no store at x yet; schwabtx import makes one"},
		{"those of a schwabtx transactions -json file; schwabtx snapshot takes one", "those of a schwabtx transactions -json file; schwabtx snapshot takes one"},
	} {
		if got := withStore(other, c[0]); got != c[1] {
			t.Errorf("withStore(%q) = %q, want %q", c[0], got, c[1])
		}
		if got := withStore(def, c[0]); got != c[0] {
			t.Errorf("withStore of the default store changed %q to %q", c[0], got)
		}
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
// of option symbols, which hold spaces, quoted for the shell of the system
// it runs on, so that the shell gives the keys back whole: run as split, it
// records your decision.
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
	// Quoted for the shell of this system, single quotes on Unix and
	// double on Windows, and no other way.
	open, other := "'", `"`
	if runtime.GOOS == "windows" {
		open, other = other, open
	}
	i := strings.Index(review, "schwabtx edit "+open+"K:")
	if i < 0 || strings.Contains(review, other+"K:") {
		t.Fatalf("status -review has no command with a key quoted with %s alone:\n%s", open, review)
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
