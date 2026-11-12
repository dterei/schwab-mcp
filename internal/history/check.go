package history

import (
	"context"
	"fmt"
	"strings"
)

// A CheckReport is what Check found.
type CheckReport struct {
	// Problems are what is wrong: what SQLite's integrity and foreign key
	// checks report, and derived data that breaks an invariant of the
	// store. A rebuild (Rebuild) derives the data again; a damaged file
	// needs a restore.
	Problems []string
	// Warnings are what is not wrong but needs you: an account number that
	// the masking of free text would not find.
	Warnings []string
}

// Check checks the store (schwabtx store check): SQLite's integrity check
// and foreign key check, and the invariants that the derived data must
// hold: no file transaction that counts is in a match, since a linked
// one's API transaction counts in its place (I10); every match holds API
// and file transactions both, all of its own account; and every pair is
// paired both ways, as one kind. It warns of each stored account number
// that has fewer than 8 digits, or characters other than digits, which
// free text masked by pattern would show whole. What it reports names
// accounts and transactions by their refs, never by their numbers.
func (s *Store) Check(ctx context.Context) (CheckReport, error) {
	var c CheckReport
	err := s.withRead(ctx, func(r *rtx) error {
		c = CheckReport{}
		problem := func(format string, args ...any) { c.Problems = append(c.Problems, fmt.Sprintf(format, args...)) }
		each := func(do func(scan func(...any) error) error, query string) error {
			rows, err := r.query(query)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				if err := do(rows.Scan); err != nil {
					return err
				}
			}
			return rows.Err()
		}
		checks := []struct {
			query string
			do    func(scan func(...any) error) error
		}{
			{"PRAGMA integrity_check", func(scan func(...any) error) error {
				var line string
				if err := scan(&line); err != nil || line == "ok" {
					return err
				}
				problem("integrity_check: %s", line)
				return nil
			}},
			{"PRAGMA foreign_key_check", func(scan func(...any) error) error {
				var table, parent string
				var rowid, fk *int64
				if err := scan(&table, &rowid, &parent, &fk); err != nil {
					return err
				}
				row := "a row"
				if rowid != nil {
					row = fmt.Sprintf("row %d", *rowid)
				}
				problem("foreign_key_check: %s of %s refers to a row of %s that is not there", row, table, parent)
				return nil
			}},
			{`SELECT e.ref, e.account_id FROM entry e JOIN match_member m ON m.csv_txn_id = e.csv_txn_id
				WHERE e.counted = 1 ORDER BY e.account_id, e.ref`, func(scan func(...any) error) error {
				var ref string
				var account int64
				if err := scan(&ref, &account); err != nil {
					return err
				}
				problem("%s of A%d counts, though it is linked to an API transaction, which counts in its place", ref, account)
				return nil
			}},
			{`SELECT m.account_id,
				(SELECT group_concat(ref, ', ') FROM (SELECT e.ref FROM match_member x
				   JOIN entry e ON e.txn_id = x.txn_id OR e.csv_txn_id = x.csv_txn_id
				   WHERE x.match_id = m.match_id ORDER BY e.ref)),
				(SELECT count(*) FROM match_member x WHERE x.match_id = m.match_id AND x.txn_id IS NOT NULL),
				(SELECT count(*) FROM match_member x WHERE x.match_id = m.match_id AND x.csv_txn_id IS NOT NULL),
				(SELECT count(*) FROM match_member x
				   LEFT JOIN api_txn t ON t.txn_id = x.txn_id LEFT JOIN csv_txn c ON c.csv_txn_id = x.csv_txn_id
				   WHERE x.match_id = m.match_id AND coalesce(t.account_id, c.account_id) IS NOT m.account_id)
				FROM match m ORDER BY m.account_id, 2`, func(scan func(...any) error) error {
				var account int64
				var refs *string
				var api, files, others int
				if err := scan(&account, &refs, &api, &files, &others); err != nil {
					return err
				}
				what := fmt.Sprintf("a match of A%d", account)
				if refs != nil {
					what += " (" + *refs + ")"
				}
				switch {
				case api == 0 && files == 0:
					problem("%s holds no transactions", what)
				case api == 0:
					problem("%s holds no API transaction", what)
				case files == 0:
					problem("%s holds no file transaction", what)
				}
				if others > 0 {
					problem("%s holds transactions of another account", what)
				}
				return nil
			}},
			{`SELECT e.ref, e.account_id, p.ref, p.account_id FROM entry e JOIN entry p ON p.entry_id = e.pair_entry_id
				WHERE p.pair_entry_id IS NOT e.entry_id OR p.pair_kind IS NOT e.pair_kind
				ORDER BY e.account_id, e.ref`, func(scan func(...any) error) error {
				var ref, with string
				var account, withAccount int64
				if err := scan(&ref, &account, &with, &withAccount); err != nil {
					return err
				}
				problem("%s of A%d is paired with %s of A%d, which is not paired with it as one kind", ref, account, with, withAccount)
				return nil
			}},
			{"SELECT account_id, number FROM account ORDER BY account_id", func(scan func(...any) error) error {
				var account int64
				var number string
				if err := scan(&account, &number); err != nil {
					return err
				}
				if w := numberWarning(account, number); w != "" {
					c.Warnings = append(c.Warnings, w)
				}
				return nil
			}},
		}
		for _, ch := range checks {
			if err := each(ch.do, ch.query); err != nil {
				return err
			}
		}
		return nil
	})
	return c, err
}

// numberWarning returns the warning of an account number that masking by
// pattern would miss in free text, which finds runs of 8 digits or more,
// or "".
func numberWarning(account int64, number string) string {
	if strings.Trim(number, "0123456789") != "" {
		return fmt.Sprintf("A%d's number has characters other than digits: text that holds it may not be masked; please report it", account)
	}
	if len(number) < 8 {
		return fmt.Sprintf("A%d's number has %d digits: text that holds it is not masked; please report it", account, len(number))
	}
	return ""
}
