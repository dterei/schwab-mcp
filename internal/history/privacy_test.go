package history

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"modernc.org/sqlite"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The numbers that TestViewsHoldNoAccountNumber plants beside the stored
// accounts' own: 8-digit numbers of accounts that the store does not
// know.
const (
	plantedA = "48213907"
	plantedB = "57302618"
)

// fillEverySource fills s from every source the store reads, with account
// numbers, stored and planted, whole, hyphenated and spaced, in every
// piece of free text that a source can hold: transactions' descriptions
// from API reads and from a -json file, a read's error, a nickname, a
// position's description, a History CSV file's name and rows, a History
// JSON row with an ItemIssueId whose description names its security, a
// Realized lot's name, an account's kind and decisions' notes.
func fillEverySource(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	var numbers []schwab.AccountNumber
	if err := json.Unmarshal(schwabtest.Fixture(t, "account_numbers.json"), &numbers); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAccountNumbers(ctx, readAt(fixturesEnd), numbers); err != nil {
		t.Fatal(err)
	}
	prefs := schwab.UserPreference{Accounts: []schwab.PreferenceAccount{
		{AccountNumber: individual, NickName: "Joint 4821-3907"},
		{AccountNumber: options, NickName: "Trading " + options},
	}}
	if err := s.RecordPreferences(ctx, readAt(fixturesEnd), prefs); err != nil {
		t.Fatal(err)
	}
	recordFixtures(t, s)

	// API transactions of the files' account, whose descriptions hold
	// numbers, one of them in a dividend's wording that no rule knows.
	jan := func(d int) time.Time { return at(time.January, d) }
	rows := []string{
		apiRaw(2001, jan(5), "JOURNAL", 100, "", 0, "JOURNAL FRM "+plantedA),
		apiRaw(2002, jan(6), "JOURNAL", -100, "", 0, "TRF TO "+individual),
		apiRaw(2003, jan(7), "WIRE_OUT", -250, "", 0, "WIRE TO 5730 2618 REF 12"),
		apiRaw(2004, jan(8), "DIVIDEND_OR_INTEREST", 2, "", 0, "Payout 4821 – 3907~XYZ"),
		apiRaw(2005, jan(9), "TRADE", -50, "XYZ", 5, "Buy for "+options),
	}
	var decoded []schwab.Transaction
	for _, r := range rows {
		decoded = append(decoded, decodeRow(t, r))
	}
	record(t, s, listRead(fileAccount, jan(1), jan(20), jan(20), decoded...))
	refused := listRead(fileAccount, jan(1), jan(20), jan(21))
	refused.Err = &schwab.Error{Status: 400, Message: "account " + plantedB + " (" + fileAccount + ") is not valid"}
	if _, err := s.RecordTransactions(ctx, refused); err != nil {
		t.Fatal(err)
	}
	failed := listRead(fileAccount, jan(1), jan(20), jan(22))
	failed.Err = errors.New("reading account 4821 3907: connection reset")
	if _, err := s.RecordTransactions(ctx, failed); err != nil {
		t.Fatal(err)
	}
	// One import of every kind of file: a -json file; a snapshot whose
	// position's description holds a number; History exports of the files'
	// account, a CSV file whose name holds a number and a JSON export whose
	// row with an ItemIssueId names its security by a description that
	// holds one; and a Realized export.
	cli := cliJSON(t,
		apiRaw(2006, jan(26), "JOURNAL", 7, "", 0, "JOURNAL FRM 5730-2618"),
		apiRaw(2007, jan(27), "CASH_RECEIPT", 9, "", 0, "FROM "+fileAccount),
		apiRaw(2008, jan(28), "CASH_RECEIPT", 9, "", 0, "FROM 4821 3907"))
	line := snapshotLine(t, time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC),
		fixtureAccount(t, individual, "VANGUARD TOTAL STOCK MARKET ETF", "FUND OF "+plantedA),
		fixtureAccount(t, options))
	csv := []csvRow{
		{"02/02/2026", "Journal", "", "JOURNAL TO " + plantedB, "", "", "", "-$10.00"},
		{"02/03/2026", "Wire Sent", "", "WIRE TO 4821-3907", "", "", "", "-$20.00"},
		{"02/04/2026", "Buy", "XYZ", "XYZ CORP " + individual, "5", "$10.00", "", "-$50.00"},
		{"02/04/2026", "Journal " + plantedA, "", "MISC", "", "", "", "$1.00"}, // an action no rule knows
	}
	js := []csvRow{
		{"02/05/2026", "Qualified Dividend", "QRS", "QRS FUND 5730 2618", "", "", "", "$3.00"},
		{"02/06/2026", "Credit Interest", "", "SCHWAB1 INT " + fileAccount, "", "", "", "$0.07"},
	}
	realized := strings.Replace(realizedCSV("Mon Jul 06 10:47:11 EDT 2026", "01/01/2026", "07/06/2026",
		[9]string{"XYZ", "07/01/2026", "01/05/2026", "10", "$120.00", "$100.00", "$20.00", "No", ""}),
		`"XYZ CORP"`, `"XYZ CORP `+plantedB+`"`, 1)
	importFiles(t, s, ImportOptions{Account: fileAccount},
		"transactions.json", cli,
		"snapshots.jsonl", line,
		"Joint "+plantedA+"_XXX398_Transactions_20260210-090000.csv", historyCSV(csv...),
		exportName("20260210-090500", "json"), historyJSON("02/05/2026", "02/09/2026", map[int]string{0: "71234567"}, js...),
		"XXXX0398_GainLoss_Realized_Details_20260706-104711.csv", realized)

	// Your decisions, with notes that hold numbers, and an account's kind.
	files := storeInt(t, s, "SELECT account_id FROM account WHERE number = ?", fileAccount)
	mine := storeInt(t, s, "SELECT account_id FROM account WHERE number = ?", individual)
	decide(t, s, Decision{Kind: "category", Subject: fmt.Sprintf("a:%d:2001", files), Value: "deposit", Note: "from " + plantedA})
	decide(t, s, Decision{Kind: "account", Subject: fmt.Sprintf("acct:%d", mine), Value: "kind=ira " + plantedB, Note: "4821-3907"})
}

// separators are what may stand between the digits of a number written
// in parts: spaces and dashes of any kind.
func separator(r rune) bool {
	return unicode.IsSpace(r) || unicode.Is(unicode.Pd, r)
}

// composedIdentifiers are what a flag's detail composes beside free text
// masked before (5.3 of the design): refs, instrument keys, dates and
// times, and numeric literals, which may hold 8 digits or more and cannot
// be account numbers.
var composedIdentifiers = regexp.MustCompile(`\b(?:[ATSFCL]~?[0-9a-fA-F][0-9A-Za-z.~]*|K?:?[icxs]:\S+|[0-9]{4}-[0-9]{2}-[0-9]{2}(?:T[0-9:.]+Z)?|-?[0-9]+\.[0-9]+)`)

// No view shows a full account number, raw JSON or a file's content (I9;
// 5.3 of the design), whatever the free text that Schwab, a file or you
// wrote holds. With the store filled from every source, numbers planted in
// every piece of free text: (a) no cell of any view, listed from
// sqlite_schema so that a new view is covered, holds a stored account's
// number or a planted one, nor its digits with spaces or dashes between
// them; (b) no cell of a column of free text holds a run of 8 digits that
// masking would mask (a file's label is masked as a file name, which keeps
// schwab.com's stamp; a flag's detail may compose identifiers); (c) every
// view column has a kind in viewColumns, viewColumns names no other, and
// no column of identifiers holds what was planted, masked; (d) with
// SQLite's authorizer on a connection of its own, preparing each view reads
// no account number, no raw column and no imported file's name or content;
// and (e) an export of every table, every row included, reads none of
// those either unless -raw or -show-accounts asks, and holds no number in
// CSV or in JSON Lines.
func TestViewsHoldNoAccountNumber(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	fillEverySource(t, s)
	ctx := t.Context()

	numbers := append(storeStrings(t, s, "SELECT number FROM account"), plantedA, plantedB)
	if len(numbers) < 6 {
		t.Fatalf("the store holds the accounts %d numbers, too few", len(numbers)-2)
	}
	views := storeStrings(t, s, "SELECT name FROM sqlite_schema WHERE type = 'view' ORDER BY name")
	kinds := map[[2]string]viewColumn{}
	for _, c := range viewColumns {
		if _, ok := kinds[[2]string{c.view, c.column}]; ok {
			t.Errorf("viewColumns names %s.%s twice", c.view, c.column)
		}
		kinds[[2]string{c.view, c.column}] = c
	}
	seen := map[[2]string]bool{}
	cells := 0
	for _, v := range views {
		cols := storeStrings(t, s, "SELECT name FROM pragma_table_info('"+v+"') ORDER BY cid")
		rows := storeRows(t, s, "SELECT * FROM "+v)
		for i, col := range cols {
			seen[[2]string{v, col}] = true
			c, ok := kinds[[2]string{v, col}]
			if !ok {
				t.Errorf("%s.%s has no kind in viewColumns", v, col)
			}
			free := rows
			if c.when != "" {
				free = storeRows(t, s, "SELECT * FROM "+v+" WHERE "+c.when)
			}
			for _, row := range rows {
				cell := row[i]
				cells++
				joined := strings.Map(func(r rune) rune {
					if separator(r) {
						return -1
					}
					return r
				}, cell)
				for _, n := range numbers {
					if strings.Contains(joined, n) {
						t.Errorf("a cell of %s holds the number %s: %q", v, mask.Number(n), cell)
					}
				}
				// What was planted is free text, which only a column of
				// free text holds, masked.
				if c.kind != freeText && (strings.Contains(cell, mask.Number(plantedA)) || strings.Contains(cell, mask.Number(plantedB))) {
					t.Errorf("%s.%s, an identifier in viewColumns, holds free text: %q", v, col, cell)
				}
			}
			if c.kind != freeText {
				continue
			}
			for _, row := range free {
				cell, text := row[i], row[i]
				switch col {
				case "label":
					if mask.FileName(cell) != cell {
						t.Errorf("%s.%s holds a label with a number: %q", v, col, cell)
					}
					continue
				case "detail":
					text = composedIdentifiers.ReplaceAllString(cell, "")
				}
				if mask.Text(text) != text {
					t.Errorf("%s.%s holds a run of 8 digits: %q", v, col, cell)
				}
			}
		}
	}
	for k := range kinds {
		if !seen[k] {
			t.Errorf("viewColumns names %s.%s, which no view has", k[0], k[1])
		}
	}
	// The store holds what was planted: free text of every column the test
	// checks, masked.
	for _, q := range []string{
		"SELECT count(*) FROM accounts WHERE nickname = 'Joint ...907' AND kind = 'ira ...618'",
		"SELECT count(*) FROM transactions WHERE description = 'JOURNAL FRM ...907'",
		"SELECT count(*) FROM transactions WHERE description = 'WIRE TO ...618 REF 12' AND first_seen_by = 'api'",
		"SELECT count(*) FROM transactions WHERE description = 'FROM ...907' AND first_seen_by = 'file'",
		"SELECT count(*) FROM positions WHERE description = 'FUND OF ...907'",
		"SELECT count(*) FROM file_transactions WHERE description = 'JOURNAL TO ...618'",
		"SELECT count(*) FROM security_names WHERE kind = 'description' AND value = 'QRS FUND ...618'",
		"SELECT count(*) FROM security_keys WHERE key = 'x:71234567'",
		"SELECT count(*) FROM realized_lots WHERE name = 'XYZ CORP ...618'",
		"SELECT count(*) FROM files WHERE label = 'Joint ...907_XXX398_Transactions_20260210-090000.csv'",
		"SELECT count(*) FROM flags WHERE code = 'unknown_wording' AND detail = 'Payout ...907~SYM'",
		"SELECT count(*) FROM decision WHERE note = 'from ...907'",
		"SELECT count(*) FROM api_read WHERE error LIKE '%...618 (...398)%'",
		"SELECT count(*) FROM api_read WHERE error LIKE '%...907: connection reset'",
	} {
		if storeInt(t, s, q) != 1 {
			t.Errorf("%s: not 1; the test did not plant what it checks", q)
		}
	}
	t.Logf("%d cells of %d views checked", cells, len(views))

	// The authorizer sees every column that preparing a statement reads,
	// those that a view reads included.
	conn, err := s.r.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var reads []string
	if err := conn.Raw(func(dc any) error {
		return dc.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(func(action sqlite.AuthorizerActionCode, table, column, _, _ string) sqlite.AuthorizerReturnCode {
			if action == sqlite.AuthRead {
				reads = append(reads, table+"."+column)
			}
			return sqlite.AuthorizerOK
		})
	}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = conn.Raw(func(dc any) error { return dc.(sqlite.AuthorizerRegisterer).RegisterAuthorizer(nil) })
	}()
	private := func(read string) bool {
		table, column, _ := strings.Cut(read, ".")
		return read == "account.number" || strings.Contains(column, "raw") ||
			table == "import_file" && (column == "name" || column == "content")
	}
	prepare := func(query string) []string {
		reads = nil
		stmt, err := conn.PrepareContext(ctx, query)
		if err != nil {
			t.Fatalf("preparing %s: %v", query, err)
		}
		stmt.Close()
		return slices.DeleteFunc(reads, func(r string) bool { return !private(r) })
	}
	if got := prepare("SELECT a.number, f.name, t.latest_raw FROM account a, import_file f, api_txn t"); len(got) != 3 {
		t.Fatalf("the authorizer saw %q of three private columns", got)
	}
	for _, v := range views {
		if got := prepare("SELECT * FROM " + v); len(got) > 0 {
			t.Errorf("the view %s reads %q", v, got)
		}
	}

	// An export reads more than its view only when asked, and what it
	// writes holds no number.
	account := sql.NullInt64{Int64: storeInt(t, s, "SELECT account_id FROM account WHERE number = ?", fileAccount), Valid: true}
	for _, o := range []ExportOptions{
		{What: []string{"all"}},
		{What: []string{"all"}, All: true, From: "2026-01-01", To: "2026-12-31"},
		{What: []string{"all"}, Raw: true},
		{What: []string{"all"}, ShowAccounts: true},
	} {
		tables, err := o.tables()
		if err != nil {
			t.Fatal(err)
		}
		for _, tb := range tables {
			query, _ := exportQuery(tb, o, account)
			got := prepare(query)
			want := o.Raw && tb.raw != rawNone || o.ShowAccounts && !tb.noAccount
			if len(got) > 0 != want {
				t.Errorf("exporting %s with %+v reads %q", tb.name, o, got)
			}
		}
	}
	e, err := s.Export(ctx, ExportOptions{What: []string{"all"}, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.Tables) != len(exportTables) || e.HoldsNumbers {
		t.Fatalf("the export holds %d tables of %d, numbers %v", len(e.Tables), len(exportTables), e.HoldsNumbers)
	}
	rows := 0
	for _, format := range []string{"csv", "jsonl"} {
		for _, tb := range e.Tables {
			rows += tb.Rows
			var b strings.Builder
			if err := tb.write(&b, format); err != nil {
				t.Fatal(err)
			}
			joined := strings.Map(func(r rune) rune {
				if separator(r) {
					return -1
				}
				return r
			}, b.String())
			for _, n := range numbers {
				if strings.Contains(joined, n) {
					t.Errorf("the export's %s in %s holds the number %s", tb.Name, format, mask.Number(n))
				}
			}
		}
	}
	t.Logf("%d rows of %d tables exported", rows/2, len(e.Tables))
}
