package history

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each transaction counts once (15.3 of the design, I10), in a store: API
// rows and an export of the same days link, and the API's record counts in
// the file's place; a row the API lacks inside its coverage is held out,
// by a -json file's coverage too; rows before the oldest API row, past the
// read's empty days, count from the file; a deliberate miss (another
// amount) inside the coverage is held out, so nothing counts twice; a
// TD Ameritrade row inside the coverage counts; a tie is uncertain, with
// its candidates. Each decision changes the count as the design says: a
// count on a linked row is refused, and so are a link to an API row that
// the store's own match gives another row, to one of another account or
// to none, and a link of a row whose count you decided; your link to the
// API row the store's match gives it is taken; your link settles the tie; a
// policy for an action counts its rows; a reject unlinks; and when the API
// row that a file row is linked to goes, neither counts until you say the
// file row is none of the API's. A row you said is none of the API's, whose
// API row then comes late, is flagged while both count, until you link
// them.
func TestCounting(t *testing.T) {
	t.Parallel()
	// A clock that moves, so that the late API row comes after your
	// decision.
	s := newStore(t, Options{Now: tickingNow()})
	api := []string{
		apiRaw(1001, at(time.January, 12), "DIVIDEND_OR_INTEREST", 3.21, "", 0, "Ordinary Dividend~ABCD"),
		apiRaw(1002, at(time.January, 20), "TRADE", -50, "XYZ", 5, ""),
		apiRaw(1003, at(time.January, 26), "TRADE", 60, "XYZ", -5, ""),
		apiRaw(1004, at(time.January, 13), "DIVIDEND_OR_INTEREST", 2, "", 0, "Ordinary Dividend~EFGH"),
		apiRaw(1005, at(time.January, 15), "DIVIDEND_OR_INTEREST", 2, "", 0, "Ordinary Dividend~EFGH"),
		apiRaw(1006, at(time.January, 28), "WIRE_OUT", -1000.01, "", 0, "WIRED FUNDS DISBURSED"),
	}
	read := listRead(fileAccount, at(time.January, 1), at(time.February, 1), at(time.February, 1))
	for _, r := range api {
		read.Rows = append(read.Rows, decodeRow(t, r))
	}
	record(t, s, read)
	importFiles(t, s, ImportOptions{}, "transactions.json", cliJSON(t,
		apiRaw(2001, at(time.March, 2), "DIVIDEND_OR_INTEREST", 1, "", 0, "Ordinary Dividend~ABCD"),
		apiRaw(2002, at(time.March, 10), "TRADE", -70, "XYZ", 7, ""),
		apiRaw(2003, at(time.March, 20), "DIVIDEND_OR_INTEREST", 1.5, "", 0, "Ordinary Dividend~ABCD"),
		strings.Replace(apiRaw(2099, at(time.March, 2), "DIVIDEND_OR_INTEREST", 1, "", 0, "Ordinary Dividend~ABCD"),
			`"accountNumber": "`+fileAccount+`"`, `"accountNumber": "22220456"`, 1)))
	history := historyCSV(
		csvRow{"01/12/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"},
		csvRow{"01/20/2026", "Buy", "XYZ", "XYZ CORP", "5", "$10.00", "", "-$50.00"},
		csvRow{"01/26/2026", "Sell", "XYZ", "XYZ CORP", "5", "$12.00", "", "$60.00"},
		csvRow{"01/14/2026", "Qualified Dividend", "EFGH", "EFGH INC", "", "", "", "$2.00"},
		csvRow{"01/16/2026 as of 01/15/2026", "Bank Interest", "", "BANK INT 011526 SCHWAB BANK", "", "", "", "$0.42"},
		csvRow{"01/22/2026", "Credit Interest", "", "TDA TRAN - CREDIT INTEREST", "", "", "", "$0.07"},
		csvRow{"01/05/2026", "Buy", "XYZ", "XYZ CORP", "1", "$10.00", "", "-$10.00"},
		csvRow{"01/28/2026", "Wire Funds", "", "WIRED FUNDS DISBURSED", "", "", "", "-$1000.00"},
		csvRow{"12/15/2025", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.00"},
		csvRow{"03/10/2026", "Buy", "XYZ", "XYZ CORP", "7", "$10.00", "", "-$70.00"},
		csvRow{"03/12/2026", "Sell", "XYZ", "XYZ CORP", "1", "$11.00", "", "$11.00"},
	)
	r := importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260401-090000", "csv"), history)
	if f := r.Files[0]; f.Linked != 4 || f.Uncertain != 1 || f.HeldOut != 3 || f.BeforeAPI != 2 {
		t.Errorf("the import reports %d linked, %d uncertain, %d held out and %d before the API, want 4, 1, 3 and 2",
			f.Linked, f.Uncertain, f.HeldOut, f.BeforeAPI)
	}
	counts := func() []string {
		t.Helper()
		return storeStrings(t, s, `SELECT posted_date || ' ' || action || ' ' || counted_why ||
			coalesce(' ' || (SELECT group_concat(code || coalesce(':' || detail, ''), ',') FROM flags f
				WHERE f.subject_ref = file_transactions.ref AND code IN ('held_out', 'uncertain_match', 'linked_api_uncounted')), '')
			FROM file_transactions ORDER BY posted_date, action, amount_cents`)
	}
	want := []string{
		"2025-12-15 Qualified Dividend file",
		"2026-01-05 Buy file",
		"2026-01-12 Qualified Dividend linked",
		"2026-01-14 Qualified Dividend uncertain uncertain_match:it could be T1004 or T1005; say which: schwabtx edit " +
			subjectRef(t, s, history, 4) + " -link T1004, or -link none if none is",
		"2026-01-16 Bank Interest held_out held_out",
		"2026-01-20 Buy linked",
		"2026-01-22 Credit Interest tda",
		"2026-01-26 Sell linked",
		"2026-01-28 Wire Funds held_out held_out",
		"2026-03-10 Buy linked",
		"2026-03-12 Sell held_out held_out:api_file",
	}
	if got := counts(); !slices.Equal(got, want) {
		t.Errorf("the file transactions count\n%q, want\n%q", got, want)
	}
	// Nothing counts twice: the account's ledger holds the 9 API
	// transactions and the 3 file transactions that count, and no counted
	// file transaction is in a match.
	if n := storeInt(t, s, "SELECT count(*) FROM ledger WHERE account_id = 1"); n != 12 {
		t.Errorf("the ledger holds %d entries, want 12", n)
	}
	if n := storeInt(t, s, `SELECT count(*) FROM entry e JOIN match_member m ON m.csv_txn_id = e.csv_txn_id WHERE e.counted = 1`); n != 0 {
		t.Errorf("%d counted file transactions are in a match", n)
	}
	if got := storeStrings(t, s, "SELECT api_ref || ' ' || method FROM matches ORDER BY api_ref"); !slices.Equal(got,
		[]string{"T1001 exact", "T1002 exact", "T1003 exact", "T2002 exact"}) {
		t.Errorf("the matches are %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM match_candidate"); n != 2 {
		t.Errorf("the uncertain row has %d candidates, want 2", n)
	}
	if got := entryRowOf(t, s, "T1006", "counted, flags"); got != "1 not_in_files" {
		t.Errorf("the API's wire, which the complete export lacks: %q", got)
	}
	// A later export that writes the uncertain row's description as the
	// API rows do: still a tie, but each candidate's score and why are
	// written again.
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260402-090000", "csv"),
		historyCSV(csvRow{"01/14/2026", "Qualified Dividend", "EFGH", "Ordinary Dividend~EFGH", "", "", "", "$2.00"}))
	if got, want := storeStrings(t, s, "SELECT CAST(score AS INTEGER) || ' ' || why FROM match_candidate ORDER BY txn_id"), []string{
		"130 the same amount, a day apart, the same symbol, the same fees and the same description",
		"130 the same amount, a day apart, the same symbol, the same fees and the same description",
	}; !slices.Equal(got, want) {
		t.Errorf("the restated row's candidates are %q, want %q", got, want)
	}

	refuses := func(d Decision, want string) {
		t.Helper()
		if _, err := s.Decide(t.Context(), d); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", d, err, want)
		}
	}
	c1, c2, c3, c4 := subjectOf(history, 1), subjectOf(history, 2), subjectOf(history, 3), subjectOf(history, 4)
	ref1 := subjectRef(t, s, history, 1)
	refuses(Decision{Kind: "count", Subject: c1, Value: "yes"},
		ref1+" is linked to T1001, whose record counts in its place; unlink it first: schwabtx edit "+ref1+" -link none")
	refuses(Decision{Kind: "match", Subject: subjectOf(history, 8), Value: "a:1:1002"},
		"T1002 is linked to "+subjectRef(t, s, history, 2)+" by the store's own match (exact)")
	refuses(Decision{Kind: "match", Subject: c4, Value: "a:2:2099"}, "T2099 is in another account (A2 ...456) than "+subjectRef(t, s, history, 4))
	refuses(Decision{Kind: "reject", Subject: c4, Value: "a:2:2099"}, "T2099 is in another account (A2 ...456) than "+subjectRef(t, s, history, 4))
	refuses(Decision{Kind: "reject", Subject: c4, Value: "a:1:9999"}, "no API transaction a:1:9999 in the store")
	// Your link to the API row that the store's own match already gives it
	// is taken.
	decide(t, s, Decision{Kind: "match", Subject: c1, Value: "a:1:1001 "})
	// A row whose count you decided takes no part in matching.
	ref7 := subjectRef(t, s, history, 7)
	decide(t, s, Decision{Kind: "count", Subject: subjectOf(history, 7), Value: "yes"})
	refuses(Decision{Kind: "match", Subject: subjectOf(history, 7), Value: "a:1:1002"},
		"you decided whether "+ref7+" counts, which keeps it out of matching; withdraw that first: schwabtx edit "+ref7+" -count auto")
	decide(t, s, Decision{Kind: "match", Subject: c4, Value: "a:1:1005"})
	decide(t, s, Decision{Kind: "match", Subject: subjectOf(history, 8), Value: "a:1:1006"})
	decide(t, s, Decision{Kind: "action", Subject: "action:BANK INTEREST", Value: "count"})
	decide(t, s, Decision{Kind: "reject", Subject: c2, Value: "a:1:1002"})
	want[5] = "2026-01-20 Buy held_out held_out"
	want[3] = "2026-01-14 Qualified Dividend linked"
	want[4] = "2026-01-16 Bank Interest policy"
	want[8] = "2026-01-28 Wire Funds linked"
	want[1] = "2026-01-05 Buy user"
	if got := counts(); !slices.Equal(got, want) {
		t.Errorf("after your decisions the file transactions count\n%q, want\n%q", got, want)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM match_candidate"); n != 0 {
		t.Errorf("after your link, %d candidates are left", n)
	}

	// The API's sale goes: a later complete read leaves it out. Linked to
	// it, the file's sale does not count either, until you say it is none
	// of the API's.
	record(t, s, listRead(fileAccount, at(time.January, 16), at(time.February, 3), at(time.February, 3),
		decodeRow(t, api[1]), decodeRow(t, api[5])))
	want[7] = "2026-01-26 Sell linked linked_api_uncounted:T1003 gone; to count it from the file: schwabtx edit " +
		subjectRef(t, s, history, 3) + " -link none"
	if got := counts(); !slices.Equal(got, want) {
		t.Errorf("after the API's sale went the file transactions count\n%q, want\n%q", got, want)
	}
	decide(t, s, Decision{Kind: "match", Subject: c3, Value: "none"})
	want[7] = "2026-01-26 Sell user"
	if got := counts(); !slices.Equal(got, want) {
		t.Errorf("after you said the sale is none of the API's the file transactions count\n%q, want\n%q", got, want)
	}
	if got := entryRowOf(t, s, subjectRef(t, s, history, 3), "counted"); got != "1" {
		t.Errorf("the file's sale, none of the API's, counts %q", got)
	}
	if got := entryRowOf(t, s, "T1003", "counted, counted_why"); got != "0 gone" {
		t.Errorf("the API's sale: %q", got)
	}

	// The held-out sale, which you said no API row is, and then the API row
	// of it, which Schwab served late: both count, and the file's is
	// flagged until you say which it is; the API's is not flagged as
	// missing from the files.
	sale := subjectRef(t, s, history, 11)
	decide(t, s, Decision{Kind: "match", Subject: subjectOf(history, 11), Value: "none"})
	importFiles(t, s, ImportOptions{}, "late.json", cliJSON(t, apiRaw(2004, at(time.March, 12), "TRADE", 11, "XYZ", -1, "")))
	if got, want := entryRowOf(t, s, sale, "counted, counted_why, flags"), "1 user late_api_row"; got != want {
		t.Errorf("the file's sale, after its API row came: %q, want %q", got, want)
	}
	if got := storeStrings(t, s, "SELECT detail FROM flags WHERE code = 'late_api_row'"); !slices.Equal(got, []string{
		"it may be T2004, which counts beside it; if it is: schwabtx edit " + sale + " -link T2004; if not: schwabtx edit " + sale + " -reject T2004",
	}) {
		t.Errorf("the late_api_row flag says %q", got)
	}
	if got := entryRowOf(t, s, "T2004", "counted, flags"); got != "1 " {
		t.Errorf("the API's late sale: %q", got)
	}
	decide(t, s, Decision{Kind: "match", Subject: subjectOf(history, 11), Value: "a:1:2004"})
	if got := entryRowOf(t, s, sale, "counted, counted_why"); got != "0 linked" {
		t.Errorf("the file's sale, linked to its late API row: %q", got)
	}
}

// TestRestatedTickerStaysLinked imports a dividend on the first day of a
// -json file's dates, which the store counts from the API's record alone
// once a History export's row of it links to it, and then a later export
// that names the security by another ticker: the transaction stays linked,
// and the dividend counts once.
func TestRestatedTickerStaysLinked(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFiles(t, s, ImportOptions{}, "transactions.json", cliJSON(t,
		apiRaw(3001, at(time.March, 2), "DIVIDEND_OR_INTEREST", 15, "", 0, "Qualified Dividend~XYZ"),
		apiRaw(3002, at(time.March, 20), "DIVIDEND_OR_INTEREST", 1.5, "", 0, "Ordinary Dividend~ABCD")))
	dividend := csvRow{"03/02/2026", "Qualified Dividend", "XYZ", "XYZ CORP", "", "", "", "$15.00"}
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260401-090000", "csv"), historyCSV(dividend))
	ledger := func() []string {
		t.Helper()
		return storeStrings(t, s, `SELECT ref || ' ' || source || ' ' || counted_why FROM entries
			WHERE day = '2026-03-02' ORDER BY ref`)
	}
	ref := storeStrings(t, s, "SELECT ref FROM file_transactions")
	if len(ref) != 1 {
		t.Fatalf("the file transactions are %q", ref)
	}
	want := []string{ref[0] + " file linked", "T3001 api api"}
	if got := ledger(); !slices.Equal(got, want) {
		t.Fatalf("the dividend's entries are %q, want %q", got, want)
	}
	dividend[2] = "XYZ N"
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260402-090000", "csv"), historyCSV(dividend))
	if got := ledger(); !slices.Equal(got, want) {
		t.Errorf("after an export that names it XYZ N, the dividend's entries are %q, want %q", got, want)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM ledger WHERE day = '2026-03-02'"); n != 1 {
		t.Errorf("the ledger counts the dividend %d times", n)
	}
}

// subjectRef returns the ref of the file transaction that holds row
// ordinal of the History export content.
func subjectRef(t *testing.T, s *Store, content string, ordinal int) string {
	t.Helper()
	got := storeStrings(t, s, `SELECT e.ref FROM history_row r JOIN import_file f ON f.file_id = r.file_id
		JOIN csv_member m ON m.row_id = r.row_id JOIN entry e ON e.csv_txn_id = m.csv_txn_id
		WHERE lower(hex(f.sha256)) = '`+fileHex([]byte(content))+`' AND r.ordinal = `+strconv.Itoa(ordinal))
	if len(got) != 1 {
		t.Fatalf("row %d is in %d transactions", ordinal, len(got))
	}
	return got[0]
}
