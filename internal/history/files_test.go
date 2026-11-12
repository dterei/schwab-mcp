package history

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The account of the files of these tests: its number is made up, and the
// files' names give its last three digits.
const fileAccount = "11110398"

// csvRow is a row of a History CSV export: date, action, symbol,
// description, quantity, price, fees and amount, as the file writes them.
type csvRow [8]string

// historyCSV returns a History export in today's CSV layout with rows.
func historyCSV(rows ...csvRow) string {
	var b strings.Builder
	b.WriteString(`"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n")
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = `"` + c + `"`
		}
		b.WriteString(strings.Join(cells, ",") + "\n")
	}
	return b.String()
}

// historyJSON returns a History export in JSON stating the range from..to
// (MM/DD/YYYY) with rows, each with ItemIssueId item when it is not "".
func historyJSON(from, to string, item map[int]string, rows ...csvRow) string {
	var els []string
	for i, r := range rows {
		extra := ""
		if id, ok := item[i]; ok {
			extra = fmt.Sprintf(`, "ItemIssueId": %q, "AcctgRuleCd": "2"`, id)
		}
		els = append(els, fmt.Sprintf(`{"Date": %q, "Action": %q, "Symbol": %q, "Description": %q, "Quantity": %q, "Price": %q, "Fees & Comm": %q, "Amount": %q%s}`,
			r[0], r[1], r[2], r[3], r[4], r[5], r[6], r[7], extra))
	}
	return fmt.Sprintf(`{"FromDate": %q, "ToDate": %q, "BrokerageTransactions": [%s]}`, from, to, strings.Join(els, ",\n"))
}

// exportName returns the name schwab.com gives a History export of the
// account, exported at stamp (YYYYMMDD-HHMMSS), in ext.
func exportName(stamp, ext string) string {
	return "Individual_XXX398_Transactions_" + stamp + "." + ext
}

// importFiles imports files, each a name and its content, with o, and fails
// the test if the import is refused.
func importFiles(t *testing.T, s *Store, o ImportOptions, files ...string) ImportReport {
	t.Helper()
	var in []FileInput
	for i := 0; i+1 < len(files); i += 2 {
		in = append(in, FileInput{Name: files[i], Content: []byte(files[i+1])})
	}
	r, err := s.Import(t.Context(), in, o)
	if err != nil {
		t.Fatalf("importing: %v", err)
	}
	return r
}

// fileTxns returns the file_transactions view's rows, each as its posted
// date, action, symbol, amount and number of files.
func fileTxns(t *testing.T, s *Store) []string {
	t.Helper()
	return storeStrings(t, s, `SELECT posted_date || ' ' || action || ' ' || symbol || ' ' || coalesce(amount_cents, '') || ' x' || files
		FROM file_transactions ORDER BY posted_date, action, symbol, amount_cents, ref`)
}

// Each transaction counts once across files (14.5 of the design): a CSV
// export and the JSON export of the same rows are one set; two identical
// trades of one file stay two; overlapping exports count each row of the
// days they share once, as the most rows of it that one file holds; and
// the same bytes imported again add nothing.
func TestHistoryCountedOnce(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	rows := []csvRow{
		{"02/13/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"},
		{"02/13/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"},
		{"02/10/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"},
		{"02/03/2026", "Sell", "ABCD", "ABCD INC", "1,200", "$12.00", "$0.05", "$14399.95"},
	}
	o := ImportOptions{Account: fileAccount}
	r := importFiles(t, s, o,
		exportName("20260216-090000", "csv"), historyCSV(rows...),
		exportName("20260216-090500", "json"), historyJSON("02/01/2026", "02/15/2026", map[int]string{0: "71234567", 1: "71234567"}, rows...))
	if got := []int{r.Files[0].New, r.Files[0].Known, r.Files[1].New, r.Files[1].Known}; !slices.Equal(got, []int{4, 0, 0, 4}) {
		t.Errorf("the twins report new and known %v, want [4 0 0 4]", got)
	}
	want := []string{
		"2026-02-03 Sell ABCD 1439995 x2",
		"2026-02-10 Qualified Dividend ABCD 321 x2",
		"2026-02-13 Buy XYZ -5000 x2",
		"2026-02-13 Buy XYZ -5000 x2",
	}
	if got := fileTxns(t, s); !reflect.DeepEqual(got, want) {
		t.Errorf("the twins hold %q, want %q", got, want)
	}

	// A later export overlapping the first: the days they share count once,
	// the new days add theirs.
	later := []csvRow{
		{"03/02/2026", "Credit Interest", "", "SCHWAB1 INT 01/30-02/27", "", "", "", "$0.07"},
		rows[0], rows[1], rows[2],
	}
	r = importFiles(t, s, o, exportName("20260303-090000", "csv"), historyCSV(later...))
	if f := r.Files[0]; f.New != 1 || f.Known != 3 || f.Kind != "history_csv" || !strings.HasPrefix(f.Account, "A1 ...398") {
		t.Errorf("the overlapping export reports %+v", f)
	}
	if got := fileTxns(t, s); len(got) != 5 || got[4] != "2026-03-02 Credit Interest  7 x1" {
		t.Errorf("after the overlap the files hold %q", got)
	}

	// The same bytes again, under another name: nothing more.
	r = importFiles(t, s, o, "copy.csv", historyCSV(later...))
	if f := r.Files[0]; !f.Already || f.Ref == "" || !strings.Contains(f.Warnings[0], "already imported as "+f.Ref+" for A1 ...398") {
		t.Errorf("the same bytes again report %+v", f)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM import_file"); n != 3 {
		t.Errorf("the store holds %d files, want 3", n)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM entries WHERE source = 'file' AND counted = 1"); n != 5 {
		t.Errorf("%d file entries count, want 5", n)
	}
}

// apiRaw returns the raw JSON of an API transaction of fileAccount with
// activity ID id, at at (booked that New York day), of the type typ, net
// amount net and description; a TRADE has one EQUITY leg of quantity
// shares of symbol, any other type a cash leg of net.
func apiRaw(id int64, at time.Time, typ string, net float64, symbol string, quantity float64, description string) string {
	trade := midnight(nytime.DateOf(at))
	leg := fmt.Sprintf(`{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
		"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": %v, "cost": 0.0}`, net)
	if typ == "TRADE" {
		leg = fmt.Sprintf(`{"instrument": {"assetType": "EQUITY", "status": "ACTIVE", "symbol": %q, "instrumentId": %d,
			"closingPrice": 1.0, "type": "COMMON_STOCK"}, "amount": %v, "cost": %v, "price": 1.0, "positionEffect": "OPENING"}`,
			symbol, 7000+len(symbol), quantity, net)
	}
	return fmt.Sprintf(`{"activityId": %d, "time": %q, "description": %q, "accountNumber": %q, "type": %q, "status": "VALID",
		"subAccount": "CASH", "tradeDate": %q, "netAmount": %v, "transferItems": [%s]}`,
		id, schwabTime(at), description, fileAccount, typ, schwabTime(trade), net, leg)
}

// cliJSON returns `schwabtx transactions -json` output of raws, indented as
// the command prints it.
func cliJSON(t *testing.T, raws ...string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Indent(&b, []byte("["+strings.Join(raws, ",")+"]"), "", "  "); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// at returns 15:30 in New York on day d of month m of 2026.
func at(m time.Month, d int) time.Time {
	return time.Date(2026, m, d, 15, 30, 0, 0, nytime.NewYork())
}

// `schwabtx transactions -json` output imports as API rows (9.8 of the
// design): a row the store holds stays as it was, and is reported when
// it differs; a new row is first seen by the file, absent from the
// earlier complete read whose window held it, flagged absent_earlier and
// still counted, and gone only once a later complete read leaves it out.
// The file covers the days strictly inside its rows' span (api_file),
// until your completeness decision says it was filtered; a row that names
// no account is skipped and reported.
func TestImportCLIJSON(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	a := apiRaw(1001, at(time.January, 12), "DIVIDEND_OR_INTEREST", 3.21, "", 0, "Ordinary Dividend~ABCD")
	record(t, s, listRead(fileAccount, at(time.January, 1), at(time.February, 1), at(time.February, 1), decodeRow(t, a)))
	changed := strings.Replace(a, `"Ordinary Dividend~ABCD"`, `"Qualified Dividend~ABCD"`, 1)
	b := apiRaw(1002, at(time.January, 20), "TRADE", -50, "XYZ", 5, "")
	c := apiRaw(1003, at(time.January, 26), "TRADE", 60, "XYZ", -5, "")
	d := apiRaw(1004, at(time.February, 5), "DIVIDEND_OR_INTEREST", 1.5, "", 0, "Ordinary Dividend~XYZ")
	nameless := strings.Replace(apiRaw(1005, at(time.February, 6), "DIVIDEND_OR_INTEREST", 2, "", 0, "x"), `"accountNumber": "`+fileAccount+`", `, "", 1)
	r := importFiles(t, s, ImportOptions{}, "transactions.json", cliJSON(t, changed, b, c, d, nameless))
	f := r.Files[0]
	if f.Kind != "api_json" || len(f.Accounts) != 1 || f.Accounts[0] != (APIFileAccount{Account: "A1 ...398", Rows: 4, New: 3, Known: 1, Differ: 1}) {
		t.Errorf("the import reports %+v", f)
	}
	if !slices.Equal(f.Warnings, []string{"1 row names no account; give -account to attribute them", "1 row differs from what the store holds; kept the store's"}) {
		t.Errorf("the import warns %q", f.Warnings)
	}
	if got := entryRowOf(t, s, "T1001", "description"); got != "Ordinary Dividend~ABCD" {
		t.Errorf("the row the store held is now %q", got)
	}
	got := storeStrings(t, s, `SELECT ref || ' ' || first_seen_by || ' ' || counted || ' ' || (instr(coalesce(flags, ''), 'absent_earlier') > 0)
		FROM transactions ORDER BY ref`)
	if want := []string{"T1001 api 1 0", "T1002 file 1 1", "T1003 file 1 1", "T1004 file 1 0"}; !slices.Equal(got, want) {
		t.Errorf("the transactions are %q, want %q", got, want)
	}
	if got := storeStrings(t, s, "SELECT source || ' ' || from_date || ' ' || to_date FROM coverages ORDER BY source"); !slices.Contains(got, "api_file 2026-01-13 2026-02-04") {
		t.Errorf("the coverage is %q, want the -json file's inner days", got)
	}

	// A complete read after the import that leaves T1002 out shows it gone.
	record(t, s, listRead(fileAccount, at(time.January, 15), at(time.February, 10), at(time.February, 10), decodeRow(t, c), decodeRow(t, d)))
	if got := entryRowOf(t, s, "T1002", "counted, counted_why"); got != "0 gone" {
		t.Errorf("T1002 after a later complete read without it: %q", got)
	}
	if got := storeStrings(t, s, "SELECT counted || ' ' || coalesce(flags, '') FROM entries WHERE ref = 'T1003'"); !slices.Equal(got, []string{"1 "}) {
		t.Errorf("T1003, which the later read returned: %q", got)
	}

	// Your word that the file was filtered withdraws its coverage; auto
	// gives it back.
	subject := "f:" + fileHex([]byte(cliJSON(t, changed, b, c, d, nameless)))
	decide(t, s, Decision{Kind: "completeness", Subject: subject, Value: "filtered"})
	if n := storeInt(t, s, "SELECT count(*) FROM coverages WHERE source = 'api_file'"); n != 0 {
		t.Errorf("a filtered -json file still covers %d runs", n)
	}
	decide(t, s, Decision{Kind: "completeness", Subject: subject, Value: "auto"})
	if n := storeInt(t, s, "SELECT count(*) FROM coverages WHERE source = 'api_file'"); n != 1 {
		t.Errorf("your filtered withdrawn, the -json file covers %d runs, want 1", n)
	}
}

// A row that a -json file brings to the store is absent from each earlier
// complete read whose window, a day wider on each side and ending no
// later than when the read was sent, holds its time or its trade date
// (9.8 of the design). With -account, the rows of every number are one
// read of the account.
func TestImportCLIJSONAbsences(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	record(t, s, listRead(fileAccount, at(time.January, 1), at(time.February, 1), at(time.February, 1)))
	record(t, s, listRead(fileAccount, at(time.February, 5), at(time.February, 28), at(time.February, 6)))
	ny := func(m time.Month, d, h int) time.Time { return time.Date(2026, m, d, h, 0, 0, 0, nytime.NewYork()) }
	early := apiRaw(3001, ny(time.December, 31, 20).AddDate(-1, 0, 0), "DIVIDEND_OR_INTEREST", 1, "", 0, "Ordinary Dividend~ABCD")
	late := apiRaw(3002, ny(time.February, 2, 10), "DIVIDEND_OR_INTEREST", 2, "", 0, "Ordinary Dividend~ABCD")
	byTrade := strings.Replace(apiRaw(3003, ny(time.February, 4, 12), "DIVIDEND_OR_INTEREST", 3, "", 0, "Ordinary Dividend~ABCD"),
		`"tradeDate": "`+schwabTime(midnight("2026-02-04"))+`"`, `"tradeDate": "`+schwabTime(midnight("2026-01-30"))+`"`, 1)
	afterSent := apiRaw(3004, ny(time.February, 12, 10), "DIVIDEND_OR_INTEREST", 4, "", 0, "Ordinary Dividend~ABCD")
	elsewhere := strings.Replace(apiRaw(3005, ny(time.March, 2, 10), "DIVIDEND_OR_INTEREST", 5, "", 0, "x"),
		`"accountNumber": "`+fileAccount+`"`, `"accountNumber": "22220456"`, 1)
	importFiles(t, s, ImportOptions{Account: fileAccount}, "transactions.json", cliJSON(t, early, late, byTrade, afterSent, elsewhere))
	got := storeStrings(t, s, `SELECT t.activity_id || ' ' || a.read_id FROM api_absent a JOIN api_txn t ON t.txn_id = a.txn_id
		ORDER BY t.activity_id, a.read_id`)
	if want := []string{"3001 1", "3002 1", "3003 1"}; !slices.Equal(got, want) {
		t.Errorf("the absences are %q, want %q", got, want)
	}
	if got := storeStrings(t, s, "SELECT account_id || ' ' || row_count FROM api_read WHERE endpoint = 'file'"); !slices.Equal(got, []string{"1 5"}) {
		t.Errorf("the -json file's reads are %q, want one of the account's 5 rows", got)
	}
}

// date writes day d of month m of 2026 as files do.
func date(m time.Month, d int) string { return fmt.Sprintf("%02d/%02d/2026", int(m), d) }

// dividends returns n dividends of ABCD, one every fourth day from January
// 2, of $1.00 and up by a cent.
func dividends(n int) []csvRow {
	var rows []csvRow
	for i := range n {
		day := time.Date(2026, time.January, 2+4*i, 0, 0, 0, 0, time.UTC)
		rows = append(rows, csvRow{date(day.Month(), day.Day()), "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", fmt.Sprintf("$1.%02d", i)})
	}
	return rows
}

// fileOf returns columns of the files view for the file named name,
// joined with spaces, NULL as "".
func fileOf(t *testing.T, s *Store, name, columns string) string {
	t.Helper()
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = "coalesce(" + strings.TrimSpace(c) + ", '')"
	}
	got := storeStrings(t, s, "SELECT "+strings.Join(cols, " || ' ' || ")+" FROM files WHERE label = '"+name+"'")
	if len(got) != 1 {
		t.Fatalf("the files view holds %d rows of %s", len(got), name)
	}
	return got[0]
}

// A file's completeness is established, never assumed (14.6 of the
// design). A dividends-only export imported alone looks filtered and is
// unknown, its coverage file_unknown, with the hint to say so; beside API
// rows that hold trades on its days, it lacks them in a type filter's
// shape and is filtered. Imported after an export you said is complete, it
// is filtered the same, and the counts and the complete file's flags stay
// as they were; your word that it is complete makes it so.
func TestFilteredExport(t *testing.T) {
	t.Parallel()
	o := ImportOptions{Account: fileAccount}
	divs := dividends(21) // January 2 to March 22
	name := exportName("20260401-090000", "csv")

	s := newStore(t, Options{})
	r := importFiles(t, s, o, name, historyCSV(divs...))
	if got := fileOf(t, s, name, "completeness, warnings"); got != "unknown completeness_unknown,coverage_from_rows,looks_filtered" {
		t.Errorf("alone, the dividends-only file is %q", got)
	}
	if f := r.Files[0]; f.Completeness != "unknown" || !slices.ContainsFunc(f.Warnings, func(w string) bool {
		return strings.HasPrefix(w, "completeness: unknown; if you exported every type and no symbol, say so: schwabtx edit F")
	}) {
		t.Errorf("the import reports %+v", f)
	}
	if got := storeStrings(t, s, "SELECT source FROM coverages"); !slices.Equal(got, []string{"file_unknown"}) {
		t.Errorf("the coverage is %q", got)
	}
	trades := cliJSON(t,
		apiRaw(2001, at(time.January, 5), "TRADE", -50, "XYZ", 5, ""),
		apiRaw(2002, at(time.January, 30), "TRADE", 60, "XYZ", -5, ""),
		apiRaw(2003, at(time.March, 10), "TRADE", -70, "XYZ", 7, ""),
		apiRaw(2004, at(time.March, 25), "TRADE", 80, "XYZ", -8, ""))
	importFiles(t, s, ImportOptions{}, "trades.json", trades)
	if got := fileOf(t, s, name, "completeness, lacks_rows"); got != "filtered 2" {
		t.Errorf("beside the API's trades, the dividends-only file is %q", got)
	}
	if got := storeStrings(t, s, "SELECT detail FROM flags WHERE code = 'lacks_rows'"); len(got) != 1 ||
		got[0] != "lacks 2 rows that Schwab's API holds between 2026-01-30 and 2026-03-10, every one of a family this file has none of: an export filtered by type?" {
		t.Errorf("lacks_rows says %q", got)
	}
	if got := storeStrings(t, s, "SELECT source FROM coverages ORDER BY source"); !slices.Equal(got, []string{"api_file", "file_filtered"}) {
		t.Errorf("the coverage is %q", got)
	}

	s = newStore(t, Options{})
	full := append(slices.Clone(divs), csvRow{date(time.January, 5), "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"},
		csvRow{date(time.February, 9), "Sell", "XYZ", "XYZ CORP", "5", "$12.00", "", "$60.00"})
	importFiles(t, s, ImportOptions{Account: fileAccount, Completeness: "complete"}, exportName("20260401-080000", "csv"), historyCSV(full...))
	importFiles(t, s, o, name, historyCSV(divs...))
	if got := fileOf(t, s, name, "completeness, lacks_rows"); got != "filtered 2" {
		t.Errorf("after a complete export, the dividends-only file is %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM file_transactions"); n != 23 {
		t.Errorf("the files hold %d transactions, want 23", n)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM flags WHERE code = 'missing_from_later'"); n != 0 {
		t.Errorf("%d transactions are missing from later", n)
	}
	sha := fileHex([]byte(historyCSV(divs...)))
	decide(t, s, Decision{Kind: "completeness", Subject: "f:" + sha, Value: "complete"})
	if got := fileOf(t, s, name, "completeness, complete_by, warnings"); got != "complete you coverage_from_rows" {
		t.Errorf("once you say so, the dividends-only file is %q", got)
	}
}

// A lack with a correction's shape does not demote a file (14.6 of the
// design): a later export that lacks one dividend among others of its
// symbol, and one of two identical trades, agrees with the complete export
// before it otherwise, and is complete; the transactions it lacks are
// flagged missing_from_later and counts_differ, each of which Review gives
// the command that settles it.
func TestCorrectionShaped(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	buy := csvRow{"02/13/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"}
	first := []csvRow{
		{"02/02/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$1.00"},
		{"02/04/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$2.00"},
		{"02/06/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.00"},
		buy, buy,
	}
	later := []csvRow{first[0], first[2], buy}
	importFiles(t, s, ImportOptions{Account: fileAccount, Completeness: "complete"},
		exportName("20260301-090000", "json"), historyJSON("02/01/2026", "02/28/2026", nil, first...))
	importFiles(t, s, ImportOptions{Account: fileAccount},
		exportName("20260302-090000", "json"), historyJSON("02/01/2026", "02/28/2026", nil, later...))
	if got := storeStrings(t, s, "SELECT completeness || ' ' || complete_by FROM files ORDER BY imported_at, ref"); !slices.Equal(got, []string{"complete you", "complete file"}) {
		t.Errorf("the files are %q", got)
	}
	got := storeStrings(t, s, "SELECT posted_date || ' ' || action || ' ' || coalesce(flags, '') FROM file_transactions ORDER BY posted_date, ref")
	want := []string{
		"2026-02-02 Qualified Dividend ",
		"2026-02-04 Qualified Dividend missing_from_later",
		"2026-02-06 Qualified Dividend ",
		"2026-02-13 Buy counts_differ",
		"2026-02-13 Buy counts_differ,missing_from_later",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the transactions are %q, want %q", got, want)
	}
	// Review gives each of them the command that settles it.
	groups, err := s.Review(t.Context(), ReviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string][]string{}
	for _, g := range groups {
		for _, it := range g.Items {
			commands[g.Title] = append(commands[g.Title], it.Commands...)
		}
	}
	for code, want := range map[string]string{
		"missing_from_later": "if Schwab took it back, as the later export says: schwabtx edit C",
		"counts_differ":      "if it is counted once too often: schwabtx edit C",
	} {
		if cs := commands[code]; len(cs) == 0 || !strings.HasPrefix(cs[0], want) || !strings.HasSuffix(cs[0], " -count no") {
			t.Errorf("Review gives %s the commands %q", code, cs)
		}
	}
}

// A file's coverage is narrowed by what its warnings show (14.6 of the
// design), in the files and coverages views alike: an empty export covers
// nothing, until you say its range had no activity; a range that ends on
// the day of the export ends the day before. A late start waits for you,
// with the command that settles it, until you say the file was filtered.
func TestCoverageClipped(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	empty := historyJSON("01/01/2026", "01/31/2026", nil)
	r := importFiles(t, s, ImportOptions{Account: fileAccount},
		exportName("20260205-090000", "json"), empty,
		exportName("20260320-090000", "json"), historyJSON("02/01/2026", "03/20/2026", nil,
			csvRow{"02/03/2026", "Credit Interest", "", "SCHWAB1 INT", "", "", "", "$0.07"}))
	if f := r.Files[0]; f.From != "" || !slices.ContainsFunc(f.Warnings, func(w string) bool { return strings.HasPrefix(w, "empty: it holds no transactions") }) {
		t.Errorf("the empty export reports %+v", f)
	}
	if f := r.Files[1]; f.From != "2026-02-01" || f.To != "2026-03-19" || f.Source != "stated" {
		t.Errorf("the export of the range's last day reports %+v", f)
	}
	if got := storeStrings(t, s, "SELECT detail FROM flags WHERE code = 'coverage_clipped' ORDER BY subject_ref"); len(got) != 2 ||
		!slices.Contains(got, "empty: it holds no rows, as an export over a limit is reported to") ||
		!slices.Contains(got, "exported on 2026-03-20: it ends on 2026-03-19, the day before") {
		t.Errorf("coverage_clipped says %q", got)
	}
	if got := storeStrings(t, s, "SELECT source || ' ' || from_date || ' ' || to_date FROM coverages"); !slices.Equal(got, []string{"file_unknown 2026-02-01 2026-03-19"}) {
		t.Errorf("the coverage is %q", got)
	}
	decide(t, s, Decision{Kind: "completeness", Subject: "f:" + fileHex([]byte(empty)), Value: "complete"})
	if got := storeStrings(t, s, "SELECT source || ' ' || from_date || ' ' || to_date FROM coverages ORDER BY source"); !slices.Equal(got,
		[]string{"file 2026-01-01 2026-01-31", "file_unknown 2026-02-01 2026-03-19"}) {
		t.Errorf("once you say the empty export is complete, the coverage is %q", got)
	}
	// An export whose oldest row is long after its range's start waits for
	// you, until you say it was filtered, as its late start suggests.
	late := historyJSON("01/01/2026", "01/31/2026", nil, csvRow{"01/26/2026", "Credit Interest", "", "SCHWAB1 INT", "", "", "", "$0.05"})
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260210-090000", "json"), late)
	lateRef := storeStrings(t, s, "SELECT subject_ref FROM flags WHERE code = 'late_start'")
	if len(lateRef) != 1 {
		t.Fatalf("late_start is on %q", lateRef)
	}
	waiting := func() (int, []string) {
		t.Helper()
		st, err := s.Status(t.Context(), StatusOptions{})
		if err != nil {
			t.Fatal(err)
		}
		groups, err := s.Review(t.Context(), ReviewOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, g := range groups {
			for _, it := range g.Items {
				if g.Title == "late_start" {
					lines = append(lines, append([]string{it.Ref}, it.Commands...)...)
				}
			}
		}
		return st.Attention, lines
	}
	n, lines := waiting()
	if want := []string{lateRef[0], "if the file was filtered by type or symbol: schwabtx edit " + lateRef[0] + " -completeness filtered"}; !slices.Equal(lines, want) {
		t.Errorf("Review lists late_start as %q, want %q", lines, want)
	}
	decide(t, s, Decision{Kind: "completeness", Subject: "f:" + fileHex([]byte(late)), Value: "filtered"})
	if m, lines := waiting(); m != n-1 || lines != nil {
		t.Errorf("once you say the late export is filtered, %d wait for attention (%d before) and Review lists late_start as %q", m, n, lines)
	}
}

// realizedCSV returns a Realized Gain/Loss export's Lot Details of the
// account, exported at asOf (Java's form) for the range from..to, with
// lots: symbol, closed and opened dates, quantity, proceeds, cost basis,
// gain, wash sale and disallowed loss.
func realizedCSV(asOf, from, to string, lots ...[9]string) string {
	var b strings.Builder
	b.WriteString(`"Realized Gain/Loss - Lot Details for Individual XXXX-0398 as of ` + asOf + ` from ` + from + ` to ` + to + `"` + strings.Repeat(`,""`, 24) + "\n")
	b.WriteString(`"Symbol","Name","Closed Date","Opened Date","Quantity","Proceeds Per Share","Cost Per Share","Proceeds","Cost Basis (CB)",` +
		`"Gain/Loss ($)","Gain/Loss (%)","Long Term Gain/Loss","Short Term Gain/Loss","Term","Unadjusted Cost Basis","Wash Sale?",` +
		`"Disallowed Loss","Transaction Closed Date","Transaction Cost Basis","Total Transaction Gain/Loss ($)",` +
		`"Total Transaction Gain/Loss (%)","LT Transaction Gain/Loss ($)","LT Transaction Gain/Loss (%)",` +
		`"ST Transaction Gain/Loss ($)","ST Transaction Gain/Loss (%)"` + "\n")
	for _, l := range lots {
		fmt.Fprintf(&b, `%q,"%s CORP",%q,%q,%q,"","",%q,%q,%q,"","",%q,"Short Term","",%q,%q,%q,"","","","","","",""`+"\n",
			l[0], l[0], l[1], l[2], l[3], l[4], l[5], l[6], l[6], l[7], l[8], l[1])
	}
	return b.String()
}

// Closed lots count once across overlapping Realized exports (14.5 of the
// design): a lot that a later export re-adjusts for a wash sale is one lot
// in two versions, flagged lot_revised and shown as the newer export has
// it. A lot with a placeholder in place of a number is flagged
// placeholder, and one whose Gain/Loss is neither convention's,
// columns_disagree (14.4).
func TestRealizedLots(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	a := [9]string{"XYZ", "07/01/2026", "01/05/2026", "10", "$120.00", "$100.00", "$20.00", "No", ""}
	b := [9]string{"ABCD", "06/15/2026", "03/02/2026", "5", "$150.00", "$160.00", "-$10.00", "Yes", "$10.00"}
	adjusted := b
	adjusted[5], adjusted[6], adjusted[8] = "$165.00", "-$15.00", "$15.00"
	c := [9]string{"XYZ", "07/20/2026", "02/01/2026", "3", "$40.00", "$30.00", "$10.00", "No", ""}
	placeholder := [9]string{"QRS", "07/21/2026", "Various", "2", "--", "$30.00", "--", "No", ""}
	disagree := [9]string{"QRS", "07/22/2026", "02/03/2026", "4", "$50.00", "$40.00", "$5.00", "No", ""}
	r := importFiles(t, s, ImportOptions{Account: fileAccount},
		"XXXX0398_GainLoss_Realized_Details_20260706-104711.csv", realizedCSV("Mon Jul 06 10:47:11 EDT 2026", "01/01/2026", "07/06/2026", a, b),
		"XXXX0398_GainLoss_Realized_Details_20260801-090000.csv", realizedCSV("Sat Aug 01 09:00:00 EDT 2026", "01/01/2026", "08/01/2026",
			a, adjusted, c, placeholder, disagree))
	if r.Files[0].New != 2 || r.Files[1].New != 3 || r.Files[1].Known != 2 || r.Files[1].Account != "A1 ...398" {
		t.Errorf("the exports report %+v", r.Files)
	}
	got := storeStrings(t, s, "SELECT symbol || ' ' || closed_date || ' ' || cost_basis_cents || ' ' || versions || ' ' || files || ' ' || coalesce(flags, '') FROM realized_lots ORDER BY closed_date")
	want := []string{
		"ABCD 2026-06-15 16500 2 2 lot_revised", "XYZ 2026-07-01 10000 1 2 ", "XYZ 2026-07-20 3000 1 1 ",
		"QRS 2026-07-21 3000 1 1 placeholder", "QRS 2026-07-22 4000 1 1 columns_disagree",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the lots are %q, want %q", got, want)
	}
	if got := storeStrings(t, s, "SELECT source || ' ' || from_date || ' ' || to_date FROM coverages"); !slices.Equal(got, []string{"realized 2026-01-01 2026-07-31"}) {
		t.Errorf("the coverage is %q", got)
	}
}

// subjectOf returns the decision subject of row ordinal of the file whose
// content is content: c:<sha256 hex>:<ordinal>.
func subjectOf(content string, ordinal int) string {
	return fmt.Sprintf("c:%s:%d", fileHex([]byte(content)), ordinal)
}

// A file is named by its ref, F and the start of its SHA-256 in hex, in
// either case, or by that start alone, even when the digest starts with
// f, as one in sixteen does.
func TestForgetByRef(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	var content, hex string
	for i := 0; !strings.HasPrefix(hex, "f"); i++ {
		content = historyCSV(csvRow{"02/02/2026", "Qualified Dividend", "ABCD", "ABCD INC " + strconv.Itoa(i), "", "", "", "$1.00"})
		hex = fileHex([]byte(content))
	}
	ref := importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260210-090000", "csv"), content).Files[0].Ref
	if ref != "F"+hex[:8] {
		t.Fatalf("the file's ref is %s, want F%s", ref, hex[:8])
	}
	for _, given := range []string{ref, strings.ToLower(ref), "F" + hex[:12], hex[:9]} {
		r, err := s.Forget(t.Context(), given, given != ref)
		if err != nil || r.Ref != ref {
			t.Errorf("Forget(%s): %+v, %v", given, r, err)
		}
	}
	if _, err := s.Forget(t.Context(), "F00000000", false); err == nil || !strings.Contains(err.Error(), "no file F00000000 in the store") {
		t.Errorf("a ref of no file: %v", err)
	}
}

// A file set aside takes no part in the derivation (14.7 of the design):
// the transactions only it held leave the ledger, and those another file
// holds stay, your decision on its row following the transaction; your
// decisions on a transaction no other file holds (a category, a link, but
// not one withdrawn) wait, and are applied again when the file comes back.
// The file set aside, imported again, is refused, and nothing is imported.
func TestForget(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	a := csvRow{"02/02/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$1.00"}
	b := csvRow{"02/05/2026", "Wire Funds", "", "WIRED FUNDS", "", "", "", "-$500.00"}
	c := csvRow{"02/09/2026", "Credit Interest", "", "SCHWAB1 INT", "", "", "", "$0.07"}
	d := csvRow{"02/12/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$2.00"}
	first, second := historyCSV(a, b, c), historyCSV(b, c, d)
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260210-090000", "csv"), first)
	decide(t, s, Decision{Kind: "category", Subject: subjectOf(first, 1), Value: "interest"})
	decide(t, s, Decision{Kind: "match", Subject: subjectOf(first, 1), Value: "none"})
	decide(t, s, Decision{Kind: "count", Subject: subjectOf(first, 1), Value: "auto"}) // withdraws nothing, waits for nothing
	decide(t, s, Decision{Kind: "category", Subject: subjectOf(first, 2), Value: "transfer_out"})
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260215-090000", "csv"), second)
	ref := storeStrings(t, s, "SELECT ref FROM files ORDER BY imported_at, ref")
	if len(ref) != 2 {
		t.Fatalf("the store holds files %q", ref)
	}
	firstRef := storeStrings(t, s, "SELECT ref FROM file_fields WHERE file_id = 1")[0]
	r, err := s.Forget(t.Context(), firstRef, false)
	if err != nil {
		t.Fatal(err)
	}
	secondRef := storeStrings(t, s, "SELECT ref FROM file_fields WHERE file_id = 2")[0]
	if r.Ref != firstRef || r.Left != 1 || r.Stayed != 2 || !slices.Equal(r.StayIn, []string{secondRef}) || r.Waiting != 2 {
		t.Errorf("setting the first file aside reports %+v", r)
	}
	got := storeStrings(t, s, "SELECT posted_date || ' ' || category || ' ' || category_rule FROM file_transactions ORDER BY posted_date")
	want := []string{"2026-02-05 transfer_out user", "2026-02-09 interest file.income", "2026-02-12 dividend file.income"}
	if !slices.Equal(got, want) {
		t.Errorf("with the first file set aside, the transactions are %q, want %q", got, want)
	}
	if got := fileOf(t, s, "Individual_XXX398_Transactions_20260210-090000.csv", "forgotten, completeness"); got != "1 " {
		t.Errorf("the file set aside is %q", got)
	}
	// Imported again, with another file, it is refused, with how to bring
	// it back, and nothing is imported.
	_, err = s.Import(t.Context(), []FileInput{
		{Name: exportName("20260216-090000", "csv"), Content: []byte(historyCSV(d))},
		{Name: exportName("20260210-090000", "csv"), Content: []byte(first)},
	}, ImportOptions{Account: fileAccount})
	if want := exportName("20260210-090000", "csv") + " was set aside as " + firstRef + "; bring it back with: schwabtx import -forget " +
		firstRef + " -undo; nothing was imported"; err == nil || err.Error() != want {
		t.Errorf("importing the file set aside again: %v, want %s", err, want)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM import_file"); n != 2 {
		t.Errorf("the store holds %d files after an import that was refused", n)
	}
	r, err = s.Forget(t.Context(), firstRef, true)
	if err != nil || !r.Undo || r.Left != 1 || r.Stayed != 2 {
		t.Errorf("bringing it back reports %+v, %v", r, err)
	}
	if got := storeStrings(t, s, "SELECT category FROM file_transactions WHERE posted_date = '2026-02-02'"); !slices.Equal(got, []string{"interest"}) {
		t.Errorf("brought back, the first file's own transaction is %q", got)
	}
	if _, err := s.Forget(t.Context(), "Fzz", false); err == nil {
		t.Error("a ref that is not one is taken")
	}
}

// Forget counts, of the transactions that the file alone holds, those
// that counted in the ledger: not one linked to an API transaction, which
// counts in its place. It counts as waiting each decision that applied to
// them, whatever row it names: one made on a row of a file set aside
// before, which reached the transaction that this file's row holds now,
// too.
func TestForgetCounts(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFiles(t, s, ImportOptions{}, "transactions.json", cliJSON(t,
		apiRaw(3001, at(time.February, 2), "DIVIDEND_OR_INTEREST", 1, "", 0, "Qualified Dividend~ABCD")))
	a := csvRow{"02/02/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$1.00"}
	b := csvRow{"02/09/2026", "Wire Funds", "", "WIRED FUNDS", "", "", "", "-$500.00"}
	first, second := historyCSV(a, b), historyCSV(b)
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260210-090000", "csv"), first)
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260215-090000", "csv"), second)
	decide(t, s, Decision{Kind: "category", Subject: subjectOf(first, 2), Value: "transfer_out"})
	if got := storeStrings(t, s, "SELECT posted_date || ' ' || counted_why FROM file_transactions ORDER BY posted_date"); !slices.Equal(got,
		[]string{"2026-02-02 linked", "2026-02-09 file"}) {
		t.Fatalf("the file transactions are %q", got)
	}
	firstRef := storeStrings(t, s, "SELECT ref FROM file_fields WHERE file_id = 2")[0]
	secondRef := storeStrings(t, s, "SELECT ref FROM file_fields WHERE file_id = 3")[0]
	r, err := s.Forget(t.Context(), firstRef, false)
	if err != nil || r.Left != 1 || r.Counted != 0 || r.Stayed != 1 || r.Waiting != 0 {
		t.Errorf("setting the first file aside reports %+v, %v; want 1 left, none of it counted, 1 stayed, none waiting", r, err)
	}
	r, err = s.Forget(t.Context(), secondRef, false)
	if err != nil || r.Left != 1 || r.Counted != 1 || r.Stayed != 0 || r.Waiting != 1 {
		t.Errorf("setting the second file aside reports %+v, %v; want 1 left and counted, your decision on the first's row waiting", r, err)
	}
	if r, err = s.Forget(t.Context(), secondRef, true); err != nil || r.Left != 1 || r.Counted != 1 || r.Waiting != 0 {
		t.Errorf("bringing the second back reports %+v, %v", r, err)
	}
	if got := storeStrings(t, s, "SELECT category FROM file_transactions"); !slices.Equal(got, []string{"transfer_out"}) {
		t.Errorf("brought back, the wire is %q", got)
	}
}

// A file can be moved to another account, and given another range, by
// your decisions (14.7 of the design): its transactions leave the one and
// join the other, both derived again, and a name whose digits are not the
// new account's is flagged.
func TestMoveFile(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	content := historyCSV(csvRow{"02/05/2026", "Wire Funds", "", "WIRED FUNDS", "", "", "", "-$500.00"})
	importFiles(t, s, ImportOptions{Account: fileAccount, From: "2026-02-01", To: "2026-02-10"}, exportName("20260215-090000", "csv"), content)
	importFiles(t, s, ImportOptions{Account: "22220456"}, "other.csv", historyCSV())
	if got := storeStrings(t, s, "SELECT account_id || ' ' || coalesce(covers_from, '') || ' ' || coalesce(covers_to, '') || ' ' || coalesce(coverage_source, '') FROM files ORDER BY account_id"); !slices.Equal(got,
		[]string{"1 2026-02-01 2026-02-10 user", "2   "}) {
		t.Errorf("the files are %q", got)
	}
	subject := "f:" + fileHex([]byte(content))
	decide(t, s, Decision{Kind: "file_account", Subject: subject, Value: "acct:2"})
	if got := storeStrings(t, s, "SELECT account_id || ' ' || ref FROM file_transactions"); len(got) != 1 || !strings.HasPrefix(got[0], "2 C") {
		t.Errorf("moved, the file's transaction is %q", got)
	}
	if got := storeStrings(t, s, "SELECT account_id || ' ' || source FROM coverages ORDER BY account_id"); !slices.Equal(got, []string{"2 file_unknown"}) {
		t.Errorf("moved, the coverage is %q", got)
	}
	if got := storeStrings(t, s, "SELECT detail FROM flags WHERE code = 'name_mismatch'"); !slices.Equal(got, []string{"the digits in its name, 398, are not those of A2 ...456"}) {
		t.Errorf("name_mismatch says %q", got)
	}
	decide(t, s, Decision{Kind: "file_range", Subject: subject, Value: "2026-01-20..2026-02-12"})
	if got := storeStrings(t, s, "SELECT from_date || ' ' || to_date FROM coverages"); !slices.Equal(got, []string{"2026-01-20 2026-02-12"}) {
		t.Errorf("with your range, the coverage is %q", got)
	}
	decide(t, s, Decision{Kind: "file_account", Subject: subject, Value: "auto"})
	if n := storeInt(t, s, "SELECT count(*) FROM file_transactions WHERE account_id = 1"); n != 1 {
		t.Errorf("moved back, account 1 holds %d file transactions", n)
	}
	for _, d := range []Decision{
		{Kind: "file_account", Subject: subject, Value: "acct:9"},
		{Kind: "file_range", Subject: subject, Value: "2026-02-12..2026-01-20"},
		{Kind: "forget", Subject: subject, Value: "maybe"},
		{Kind: "completeness", Subject: "f:00", Value: "complete"},
	} {
		if _, err := s.Decide(t.Context(), d); err == nil {
			t.Errorf("%+v is taken", d)
		}
	}

	// A file that states its range keeps it, and completeness is not said
	// of a Realized export: edit would say it recorded what changes
	// nothing.
	stated := historyJSON("02/01/2026", "02/15/2026", nil, csvRow{"02/05/2026", "Wire Funds", "", "WIRED FUNDS", "", "", "", "-$500.00"})
	realized := realizedCSV("Sun Mar 01 09:00:00 EST 2026", "01/01/2026", "03/01/2026",
		[9]string{"XYZ", "02/20/2026", "01/05/2026", "10", "$120.00", "$100.00", "$20.00", "No", ""})
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260216-090000", "json"), stated,
		"XXXX0398_GainLoss_Realized_Details_20260301-090000.csv", realized)
	refs := storeStrings(t, s, "SELECT ref FROM files WHERE kind IN ('history_json', 'realized_csv') ORDER BY kind")
	for _, c := range []struct {
		d    Decision
		want string
	}{
		{
			Decision{Kind: "file_range", Subject: "f:" + fileHex([]byte(stated)), Value: "2026-01-01..2026-02-28"},
			refs[0] + " states the dates it covers, 2026-02-01 to 2026-02-15, which it keeps: -from and -to are for a file that states none",
		},
		{
			Decision{Kind: "file_range", Subject: "f:" + fileHex([]byte(realized)), Value: "2026-01-01..2026-02-28"},
			refs[1] + " states the dates it covers, 2026-01-01 to 2026-03-01, which it keeps: -from and -to are for a file that states none",
		},
		{
			Decision{Kind: "completeness", Subject: "f:" + fileHex([]byte(realized)), Value: "complete"},
			refs[1] + " is a Realized Gain/Loss export, which holds closed lots, not transactions of every type: completeness is not said of it",
		},
	} {
		if _, err := s.Decide(t.Context(), c.d); err == nil || err.Error() != c.want {
			t.Errorf("%+v: %v, want %s", c.d, err, c.want)
		}
	}
	decide(t, s, Decision{Kind: "file_range", Subject: "f:" + fileHex([]byte(stated)), Value: "auto"})
}

// Import imports every file or none (14.7 of the design): a refused file
// among three leaves the store as it was, and a dry run reports exactly
// what the import would do and saves nothing.
func TestImportAllOrNothing(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	good := historyCSV(csvRow{"02/05/2026", "Wire Funds", "", "WIRED FUNDS", "", "", "", "-$500.00"})
	_, err := s.Import(t.Context(), []FileInput{
		{Name: exportName("20260215-090000", "csv"), Content: []byte(good)},
		{Name: "Checking.json", Content: []byte(`{"PostedTransactions": []}`)},
		{Name: "transactions.json", Content: []byte(cliJSON(t, apiRaw(1, at(time.February, 3), "DIVIDEND_OR_INTEREST", 1, "", 0, "x")))},
	}, ImportOptions{Account: fileAccount})
	if err == nil || !strings.Contains(err.Error(), "Checking.json is a Schwab Bank export") {
		t.Errorf("a Schwab Bank export among three gives %v", err)
	}
	dry := importFiles(t, s, ImportOptions{Account: fileAccount, DryRun: true}, exportName("20260215-090000", "csv"), good)
	if f := dry.Files[0]; f.New != 1 || f.Rows != 1 || f.Account != "A1 ...398" || f.From != "2026-02-05" {
		t.Errorf("the dry run reports %+v", f)
	}
	for _, table := range []string{"import_file", "account", "history_row", "api_read", "decision"} {
		if n := storeInt(t, s, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s holds %d rows after a refusal and a dry run", table, n)
		}
	}
}

// An export is for the account that -account names, else for the one
// whose number ends in the digits that the file gives (14.7 of the
// design); none, or several, is an error, as is a name whose digits are not
// the account's, unless you force it.
func TestImportSelectsAccount(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFiles(t, s, ImportOptions{Account: "11110123"}, "a.csv", historyCSV())
	importFiles(t, s, ImportOptions{Account: "22220123"}, "b.csv", historyCSV(csvRow{"02/05/2026", "Wire Funds", "", "W", "", "", "", "-$5.00"}))
	content := historyCSV(csvRow{"02/06/2026", "Wire Funds", "", "W", "", "", "", "-$6.00"})
	name := "Joint_XXX123_Transactions_20260215-090000.csv"
	for _, c := range []struct {
		o    ImportOptions
		name string
		err  string
	}{
		{ImportOptions{}, name, name + " is for an account ending in 123, which fits A1 ...123 and A2 ...123; say which with -account"},
		{ImportOptions{}, "export.csv", "which account is export.csv for? Neither it nor its name gives one; use -account"},
		{ImportOptions{}, "Joint_XXX999_Transactions_20260215-090000.csv", "Joint_XXX999_Transactions_20260215-090000.csv is for an account ending in 999, which the store does not know; import its snapshots first, or use -account"},
		{ImportOptions{Account: "123"}, name, `-account "123" fits A1 ...123 and A2 ...123; say which with its ref, such as A1`},
		{ImportOptions{Account: "Joint"}, name, `no account in the store matches -account "Joint"`},
		{ImportOptions{Account: "33330456"}, name, "the digits in " + name + "'s name, 123, are not those of A3 ...456; use -force if the file was renamed"},
	} {
		if _, err := s.Import(t.Context(), []FileInput{{Name: c.name, Content: []byte(content)}}, c.o); err == nil || err.Error() != c.err {
			t.Errorf("importing %s with %+v gives %v, want %q", c.name, c.o, err, c.err)
		}
	}
	if r := importFiles(t, s, ImportOptions{Account: "A2"}, name, content); r.Files[0].Account != "A2 ...123" {
		t.Errorf("-account A2 imports for %q", r.Files[0].Account)
	}
	other := historyCSV(csvRow{"02/07/2026", "Wire Funds", "", "W", "", "", "", "-$7.00"})
	if r := importFiles(t, s, ImportOptions{Account: "...2220123 ", Force: true}, "Joint_XXX456_Transactions_20260216-090000.csv", other); r.Files[0].Account == "" {
		t.Errorf("a forced import reports %+v", r.Files[0])
	}
	if n := storeInt(t, s, "SELECT count(*) FROM flags WHERE code = 'name_mismatch'"); n != 1 {
		t.Errorf("%d files are flagged name_mismatch, want 1", n)
	}
}

// A later export that restates a row, with a renamed symbol and a
// quantity on another split basis, adds no transaction (14.5 of the
// design): the transaction shows the newer export's fields and the date
// its quantity is as of, flagged restated and restated_quantity.
func TestRestatedExport(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260301-090000", "csv"),
		historyCSV(csvRow{"02/13/2026", "Buy", "OLD", "OLD CORP", "5", "$10.00", "", "-$50.00"}))
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260901-090000", "csv"),
		historyCSV(csvRow{"02/13/2026", "Buy", "NEW", "NEW CORP", "10", "$5.00", "", "-$50.00"}))
	got := storeStrings(t, s, `SELECT symbol || ' ' || quantity_micros || ' ' || quantity_as_of || ' ' || files || ' ' || flags FROM file_transactions`)
	if want := []string{"NEW 10000000 2026-09-01 2 restated,restated_quantity,symbol_unsure"}; !slices.Equal(got, want) {
		t.Errorf("the transactions are %q, want %q", got, want)
	}
	if got := storeStrings(t, s, "SELECT detail FROM flags WHERE code = 'symbol_unsure'"); len(got) != 1 ||
		!strings.HasSuffix(got[0], "if they are two securities: schwabtx edit K:s:FILE:NEW@2026-09-01 -differ K:s:FILE:OLD@2026-03-01") {
		t.Errorf("symbol_unsure says %q", got)
	}
}

// Rows of one day, action and amount that are known to be of different
// securities are two transactions, though no one file holds both (14.5 of
// the design): two exports filtered by different symbols, each with one of
// two expirations of a blank amount, and the equal dividends of two
// securities that Schwab's ids tell apart; each is flagged
// split_by_security; a blank Amount is 0. File legs give the cash rows
// one leg of their Amount, and the trades a leg of their security and one
// of their fees.
func TestSymbolFilteredExports(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFiles(t, s, ImportOptions{Account: fileAccount},
		exportName("20260920-090000", "json"), historyJSON("09/01/2026", "09/19/2026", map[int]string{1: "81000001"},
			csvRow{"09/18/2026", "Expired", "XYZ 09/18/2026 50.00 C", "CALL XYZ", "-1", "", "", ""},
			csvRow{"09/15/2026", "Qualified Dividend", "AAA", "AAA INC", "", "", "", "$5.00"},
			csvRow{"09/10/2026", "Buy", "AAA", "AAA INC", "2", "$10.00", "$1.00", "-$21.00"}),
		exportName("20260920-090500", "json"), historyJSON("09/01/2026", "09/19/2026", map[int]string{1: "81000002"},
			csvRow{"09/18/2026", "Expired", "XYZ 09/18/2026 55.00 C", "CALL XYZ", "-1", "", "", ""},
			csvRow{"09/15/2026", "Qualified Dividend", "BBB", "BBB INC", "", "", "", "$5.00"}))
	got := storeStrings(t, s, "SELECT posted_date || ' ' || symbol || ' ' || files || ' ' || coalesce(flags, '') FROM file_transactions ORDER BY posted_date, symbol")
	want := []string{
		"2026-09-10 AAA 1 ",
		"2026-09-15 AAA 1 ",
		"2026-09-15 BBB 1 split_by_security",
		"2026-09-18 XYZ 09/18/2026 50.00 C 1 ",
		"2026-09-18 XYZ 09/18/2026 55.00 C 1 split_by_security",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the transactions are %q, want %q", got, want)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM entries WHERE kind = 'Expired' AND amount_cents = 0 AND gross_cents = 0"); n != 2 {
		t.Errorf("%d expirations of a blank Amount have an amount of 0, want 2", n)
	}
	legs := storeStrings(t, s, `SELECT day || ' ' || leg || ' ' || coalesce(asset_type, '') || ' ' || coalesce(quantity_micros, '') || ' ' ||
		coalesce(cash_cents, '') || ' ' || cost_cents || ' ' || coalesce(txn_symbol, '') FROM legs WHERE day < '2026-09-16' ORDER BY day, txn_symbol, leg`)
	if want := []string{
		"2026-09-10 0  2000000  -2000 AAA",
		"2026-09-10 1 CURRENCY  100 -100 AAA",
		"2026-09-15 0 CURRENCY  500 0 AAA",
		"2026-09-15 0 CURRENCY  500 0 BBB",
	}; !slices.Equal(legs, want) {
		t.Errorf("the legs are %q, want %q", legs, want)
	}
}

// Your pair decision follows its transaction (13.7 of the design), as your
// other decisions on file transactions do: made on a row whose file is
// then set aside, it holds for the transaction that another file's row
// opens now, and one naming that row as its value pairs that transaction;
// a rebuild derives the same.
func TestPairFollowsTransaction(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	journal := csvRow{"02/05/2026", "Journal", "", "JOURNAL TO ...456", "", "", "", "-$500.00"}
	first := historyCSV(journal)
	second := historyCSV(journal, csvRow{"02/09/2026", "Credit Interest", "", "SCHWAB1 INT", "", "", "", "$0.07"})
	importFiles(t, s, ImportOptions{Account: fileAccount},
		exportName("20260210-090000", "csv"), first, exportName("20260215-090000", "csv"), second)
	importFiles(t, s, ImportOptions{Account: "22220456"}, "other.csv",
		historyCSV(csvRow{"02/05/2026", "Journal", "", "JOURNAL FRM ...398", "", "", "", "$500.00"}))
	other := storeStrings(t, s, "SELECT subject FROM entry WHERE account_id = 2")[0]
	pairs := func() []string {
		t.Helper()
		return storeStrings(t, s, `SELECT e.ref || ' ' || e.category || ' ' || coalesce(e.pair_kind, '-') || ' ' || coalesce(p.ref, '-')
			FROM entry e LEFT JOIN entry p ON p.entry_id = e.pair_entry_id WHERE e.family = 'journal' ORDER BY e.account_id`)
	}
	step := func(name string, want ...string) {
		t.Helper()
		got := pairs()
		var shape []string
		for _, g := range got {
			_, rest, _ := strings.Cut(g, " ")
			shape = append(shape, rest)
		}
		if !slices.Equal(shape, want) {
			t.Errorf("%s: the journals are %q, want %q", name, got, want)
		}
	}
	firstRef := storeStrings(t, s, "SELECT ref FROM file_fields WHERE file_id = 1")[0]
	firstTxn := storeStrings(t, s, "SELECT ref FROM entry WHERE account_id = 1 AND kind = 'Journal'")[0]
	otherTxn := storeStrings(t, s, "SELECT ref FROM entry WHERE account_id = 2")[0]
	step("paired by the rules", "journal journal "+otherTxn, "journal journal "+firstTxn)

	decide(t, s, Decision{Kind: "pair", Subject: subjectOf(first, 1), Value: "none"})
	step("your none", "withdrawal - -", "deposit - -")
	r, err := s.Forget(t.Context(), firstRef, false)
	if err != nil || r.Waiting != 0 {
		t.Fatalf("setting the first file aside: %+v, %v", r, err)
	}
	step("your none, its row's file set aside", "withdrawal - -", "deposit - -")

	decide(t, s, Decision{Kind: "pair", Subject: other, Value: subjectOf(first, 1)})
	secondTxn := storeStrings(t, s, "SELECT ref FROM entry WHERE account_id = 1 AND kind = 'Journal'")[0]
	step("paired with the set-aside row's transaction", "journal journal "+otherTxn, "journal journal "+secondTxn)
	before := pairs()
	if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if after := pairs(); !slices.Equal(after, before) {
		t.Errorf("recorded %q, rebuilt %q", before, after)
	}
}

// A row is kept as the store reads it (13.1 and 14.3 of the design):
// trades' quantities, unsigned in History files, are signed by the action,
// a Cancel Buy's and a Cancel Sell's opposite to the trade they cancel,
// whatever sign the file wrote, and a share movement's as written, in the
// rows of a CSV and of its JSON twin, the transactions and their legs; a
// quantity of more than six decimals is flagged inexact; a blank Amount
// (an expiration's) adds nothing to the file's total. Rows outside the
// range you give for a file that states none are refused.
func TestImportRowDetails(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	rows := []csvRow{
		{"03/02/2026", "Sell", "XYZ", "XYZ CORP", "5", "$10.00", "", "$50.00"},
		{"03/03/2026", "Sell to Open", "XYZ 09/18/2026 50.00 C", "CALL XYZ", "1", "$1.00", "", "$100.00"},
		{"03/04/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"},
		{"03/05/2026", "Cancel Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "$50.00"},
		{"03/06/2026", "Cancel Sell", "XYZ", "XYZ CORP", "-5", "$10.00", "", "-$50.00"},
		{"03/07/2026", "Expired", "XYZ 09/18/2026 50.00 C", "CALL XYZ", "-1", "", "", ""},
		{"03/09/2026", "Reinvest Shares", "VTI", "VANGUARD", "0.1234567", "$300.00", "", "-$37.04"},
	}
	csv := historyCSV(rows...)
	o := ImportOptions{Account: fileAccount, From: "2026-03-02", To: "2026-03-09"}
	for _, c := range []struct {
		o    ImportOptions
		want string
	}{
		{ImportOptions{Account: fileAccount, From: "2026-03-03"}, "has rows from 2026-03-02, before -from 2026-03-03"},
		{ImportOptions{Account: fileAccount, To: "2026-03-08"}, "has rows to 2026-03-09, after -to 2026-03-08"},
	} {
		if _, err := s.Import(t.Context(), []FileInput{{Name: exportName("20260310-090000", "csv"), Content: []byte(csv)}}, c.o); err == nil ||
			!strings.Contains(err.Error(), c.want) {
			t.Errorf("rows outside -from %s -to %s: %v, want %q", c.o.From, c.o.To, err, c.want)
		}
	}
	importFiles(t, s, o, exportName("20260310-090000", "csv"), csv,
		exportName("20260310-090500", "json"), historyJSON("03/02/2026", "03/09/2026", nil, rows...))
	want := []string{"Sell -5000000", "Sell to Open -1000000", "Buy 5000000", "Cancel Buy -5000000", "Cancel Sell 5000000", "Expired -1000000", "Reinvest Shares "}
	for file := 1; file <= 2; file++ {
		got := storeStrings(t, s, fmt.Sprintf("SELECT action || ' ' || coalesce(quantity_micros, '') FROM history_row WHERE file_id = %d ORDER BY ordinal", file))
		if !slices.Equal(got, want) {
			t.Errorf("file %d's rows are %q, want %q", file, got, want)
		}
	}
	if got := storeStrings(t, s, "SELECT action || ' ' || coalesce(quantity_micros, '') FROM file_transactions ORDER BY posted_date"); !slices.Equal(got, want) {
		t.Errorf("the file transactions are %q, want %q", got, want)
	}
	got := storeStrings(t, s, `SELECT e.kind || ' ' || l.quantity_micros FROM legs l JOIN entries e ON e.ref = l.ref AND e.account_id = l.account_id
		WHERE l.leg = 0 AND l.quantity_micros IS NOT NULL ORDER BY e.day`)
	if !slices.Equal(got, want[:6]) {
		t.Errorf("the legs are %q, want %q", got, want[:6])
	}
	if got := storeStrings(t, s, "SELECT coalesce(flags, '') FROM entries WHERE kind = 'Reinvest Shares'"); len(got) != 1 || !strings.Contains(got[0], "inexact") {
		t.Errorf("a quantity of seven decimals is flagged %q", got)
	}
	if got := storeStrings(t, s, "SELECT coalesce(rows_total_cents, 'NULL') FROM file_fields ORDER BY file_id"); !slices.Equal(got, []string{"6296", "6296"}) {
		t.Errorf("the files' totals are %q, want 6296 each", got)
	}
}
