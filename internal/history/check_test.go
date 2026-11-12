package history

import (
	"slices"
	"testing"
	"time"
)

// Check finds nothing wrong with a store that the derivation wrote: here,
// a dividend from the API that a History export's row is linked to. Each
// invariant that derived rows broken by hand break is reported, naming
// transactions and accounts by their refs: a linked file transaction that
// counts, a match without its API side and of another account than its
// transactions', a pair one way only and a row whose foreign key
// dangles. A stored account number that masking by pattern would miss is
// warned about, by the account's ref.
func TestCheck(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ctx := t.Context()
	record(t, s, listRead(fileAccount, at(time.January, 1), at(time.February, 1), at(time.February, 1),
		decodeRow(t, apiRaw(1001, at(time.January, 12), "DIVIDEND_OR_INTEREST", 3.21, "", 0, "Ordinary Dividend~ABCD")),
		decodeRow(t, apiRaw(1002, at(time.January, 14), "DIVIDEND_OR_INTEREST", 1.5, "", 0, "Ordinary Dividend~XYZ"))))
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260201-090000", "csv"),
		historyCSV(csvRow{"01/12/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"}))
	if got := storeStrings(t, s, "SELECT file_ref || ' ' || api_ref FROM matches"); len(got) != 1 {
		t.Fatalf("the matches are %q, want one", got)
	}
	c, err := s.Check(ctx)
	if err != nil || len(c.Problems) > 0 || len(c.Warnings) > 0 {
		t.Fatalf("Check of a store as derived: %+v, %v", c, err)
	}
	ref := storeStrings(t, s, "SELECT ref FROM entries WHERE source = 'file'")[0]

	if err := s.withWrite(ctx, "breaking the store", func(w *wtx) error {
		for _, q := range []string{
			"UPDATE entry SET counted = 1, counted_why = 'file' WHERE csv_txn_id IS NOT NULL",
			"DELETE FROM match_member WHERE txn_id IS NOT NULL",
			`UPDATE entry SET pair_kind = 'journal', pair_entry_id = (SELECT entry_id FROM entry WHERE ref = 'T1002')
				WHERE ref IN ('T1001', 'T1002')`,
		} {
			if _, err := w.exec(q); err != nil {
				return err
			}
		}
		if err := insertAccount(w, "1234567"); err != nil {
			return err
		}
		if err := insertAccount(w, "12-45678"); err != nil {
			return err
		}
		_, err := w.exec("UPDATE match SET account_id = 2")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A connection without foreign keys, as another program's would be.
	if _, err := rawDB(t, s.Path()).ExecContext(ctx, "INSERT INTO flag (flag_id, entry_id, code) VALUES (999, 999999, 'rounded')"); err != nil {
		t.Fatal(err)
	}
	c, err = s.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantProblems := []string{
		"foreign_key_check: row 999 of flag refers to a row of entry that is not there",
		ref + " of A1 counts, though it is linked to an API transaction, which counts in its place",
		"a match of A2 (" + ref + ") holds no API transaction",
		"a match of A2 (" + ref + ") holds transactions of another account",
		"T1001 of A1 is paired with T1002 of A1, which is not paired with it as one kind",
	}
	if !slices.Equal(c.Problems, wantProblems) {
		t.Errorf("Check reports\n%q\nwant\n%q", c.Problems, wantProblems)
	}
	wantWarnings := []string{
		"A2's number has 7 digits: text that holds it is not masked; please report it",
		"A3's number has characters other than digits: text that holds it may not be masked; please report it",
	}
	if !slices.Equal(c.Warnings, wantWarnings) {
		t.Errorf("Check warns %q, want %q", c.Warnings, wantWarnings)
	}
}
