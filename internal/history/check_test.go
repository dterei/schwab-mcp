package history

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// Check finds nothing wrong with a store that the derivation wrote: here,
// a dividend from the API that a History export's row is linked to. Each
// invariant that derived rows broken by hand break is reported, naming
// transactions and accounts by their refs: a linked file transaction that
// counts, a match without its API side, without its file side, or empty,
// and of another account than its transactions', a pair one way only (of
// one kind), a pair both ways of two kinds, and a row whose foreign key dangles. A
// stored account number that masking by pattern would miss is warned
// about, by the account's ref. A damaged file is reported by the
// integrity check alone.
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
			// A match of no members, and one of an API transaction alone.
			"INSERT INTO match (account_id, method) VALUES (1, 'user')",
			"INSERT INTO match (match_id, account_id, method) VALUES (99, 1, 'user')",
			"INSERT INTO match_member (match_id, txn_id) SELECT 99, txn_id FROM entry WHERE ref = 'T1002'",
			// T1001 paired one way with T1002, as the one kind that T1002
			// is paired as, but with the file transaction; and T1002 and
			// the file transaction paired both ways, but as two kinds.
			`UPDATE entry SET pair_kind = 'sweep', pair_entry_id = (SELECT entry_id FROM entry WHERE ref = 'T1002')
				WHERE ref = 'T1001'`,
			`UPDATE entry SET pair_kind = 'journal', pair_entry_id = (SELECT entry_id FROM entry WHERE ref = 'T1002')
				WHERE csv_txn_id IS NOT NULL`,
			`UPDATE entry SET pair_kind = 'sweep', pair_entry_id = (SELECT entry_id FROM entry WHERE csv_txn_id IS NOT NULL)
				WHERE ref = 'T1002'`,
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
		"a match of A2 holds no transactions",
		"a match of A2 (" + ref + ") holds no API transaction",
		"a match of A2 (" + ref + ") holds transactions of another account",
		"a match of A2 (T1002) holds no file transaction",
		"a match of A2 (T1002) holds transactions of another account",
		ref + " of A1 is paired with T1002 of A1, which is not paired with it as one kind",
		"T1001 of A1 is paired with T1002 of A1, which is not paired with it as one kind",
		"T1002 of A1 is paired with " + ref + " of A1, which is not paired with it as one kind",
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

	// An index pointed at a table's pages, in a store of accounts alone:
	// the integrity check finds the damage, and Check reports it alone,
	// with no error.
	small := newStore(t, Options{})
	addAccounts(t, small, "3000001") // which the other checks would warn of
	if _, err := rawDB(t, small.Path()).ExecContext(ctx, `PRAGMA writable_schema = ON;
		UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name = 'account') WHERE name = 'txn_time'`); err != nil {
		t.Fatal(err)
	}
	damaged, err := Open(ctx, Options{Path: small.Path(), MustExist: true, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { damaged.Close() })
	c, err = damaged.Check(ctx)
	if err != nil || len(c.Problems) == 0 || len(c.Warnings) != 0 {
		t.Fatalf("Check of a damaged store: %+v, %v", c, err)
	}
	for _, p := range c.Problems {
		if !strings.HasPrefix(p, "integrity_check: ") {
			t.Errorf("Check of a damaged store reports %q, which is not the integrity check's", p)
		}
	}
}
