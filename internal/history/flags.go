package history

import (
	"cmp"
	"database/sql"
	"maps"
	"slices"
	"strings"
)

// A flagDef is a flag that the derivation can raise: its code, what it can
// be on (entries, snapshots, files or lots), how much it needs you and what
// it means. Every derivation writes the table to flag_def, so that the views
// carry each flag's severity and meaning, and a flag of a code not here fails
// its foreign key.
type flagDef struct {
	code string
	// subjects are what it can be on. flag_def holds one, the first.
	subjects []string
	// severity is attention, for what needs you, or info.
	severity string
	meaning  string
}

const (
	attention = "attention"
	info      = "info"
)

// flagDefs are the flags, in the order of 13.6 of the design, which
// docs/store.md lists.
var flagDefs = []flagDef{
	{"problems", on("entry", "snapshot"), attention, "Schwab sent something the records could not read"},
	{"inexact", on("entry", "lot", "snapshot"), attention, "a money or quantity value off the cent or millionth grid; its column is empty"},
	{"rounded", on("entry", "snapshot", "lot"), info, "a value within a ten-thousandth of a step of the grid (binary noise), stored rounded"},
	{"no_activity_id", on("entry"), attention, "Schwab sent no activity ID: the row is known by its time and content"},
	{"activity_id_text", on("entry"), info, "Schwab sent the activity ID as text"},
	{"account_mismatch", on("entry"), attention, "the row names another account than the one read"},
	{"duplicate_in_answer", on("entry"), info, "one answer held the row twice"},
	{"unknown_type", on("entry"), attention, "a transaction type outside Schwab's 15"},
	{"unknown_wording", on("entry"), info, "a description not seen in Schwab's answers before"},
	{"awaits_probe", on("entry"), attention, "a kind the probes have not settled; other until you set a category"},
	{"unexpected_sign", on("entry"), attention, "a deposit or withdrawal type with the other sign"},
	{"mixed_legs", on("entry"), info, "a trade with legs bought and sold"},
	{"no_security_leg", on("entry"), attention, "a trade without a security"},
	{"zero_quantity", on("entry"), attention, "a trade whose security leg moved nothing"},
	{"zero_amount", on("entry"), attention, "a cash movement or journal of 0.00: booked as other"},
	{"legs_mismatch", on("entry"), attention, "the legs' cash does not add up to the net amount"},
	{"no_legs", on("entry"), attention, "the row has no legs, so its cash cannot be checked"},
	{"status", on("entry"), attention, "a status other than VALID; INVALID is not counted"},
	{"revised", on("entry"), info, "Schwab's record changed since first seen"},
	{"gone", on("entry"), attention, "a complete re-read left it out; not counted unless you say"},
	{"absent_earlier", on("entry"), attention, "first met in a -json file, and a complete read made before you imported it did not return it: it came late, or it was cancelled"},
	{"unpaired_sweep", on("entry"), info, "a sweep without its opposite"},
	{"unpaired_journal", on("entry"), attention, "a journal without a counterpart in your other accounts; booked by sign"},
	{"ambiguous_journal", on("entry"), attention, "a journal with several possible counterparts in your other accounts; booked by sign"},
	{"unresolved_security", on("entry", "lot"), info, "the symbol names no one security in the store near the row's date"},
	{"security_by_name", on("entry"), info, "the symbol was attached to the one security that has had the name, seen within a year but not within a week of the row"},
	{"stale_decision", on("entry", "file"), attention, "your decision was made when what the rules read of the row was different, or the transaction it named now holds other rows"},
	{"category_disagrees", on("entry"), info, "the linked file row's action suggests another category"},
	{"not_in_files", on("entry"), info, "inside a complete History file's dates, with no file row"},
	{"unknown_action", on("entry"), attention, "an Action not known"},
	{"other_action", on("entry"), info, "an Action booked as other"},
	{"return_of_capital", on("entry"), info, "a return of capital, booked as a dividend"},
	{"two_row_event", on("entry"), info, "one of the two rows that a file writes for one event"},
	{"tda_history", on("entry"), info, "a row from TD Ameritrade's history, which the API probably does not serve"},
	{"uncertain_match", on("entry"), attention, "several API rows could be it; not counted until you say"},
	{"held_out", on("entry"), attention, "no API row although the API covers its dates; not counted until you say"},
	{"linked_api_uncounted", on("entry"), attention, "the API row it is linked to is gone, INVALID or not counted by you, so neither counts"},
	{"merged_fills", on("entry"), info, "linked to several fills of one order"},
	{"merged_rows", on("entry"), info, "one of several file rows linked to one API transaction"},
	{"differs_from_api", on("entry"), info, "linked, with another quantity, price, fees or day"},
	{"restated", on("entry", "lot"), info, "another export shows it with another symbol, description or price"},
	{"restated_quantity", on("entry"), attention, "another export shows a row that moves holdings with another quantity (a split restatement?)"},
	{"symbol_unsure", on("entry", "lot"), attention, "rows in different exports of one day, action and amount name different symbols that nothing in the store resolves"},
	{"split_by_security", on("entry", "lot"), attention, "counted apart from a row of another file because their securities differ"},
	{"near_duplicate", on("entry"), attention, "another transaction of the same day, action and symbol, never in the same file, differs in amount"},
	{"missing_from_later", on("entry"), attention, "a later complete export of its day does not have it, though it holds the rows around it"},
	{"counts_differ", on("entry"), attention, "complete exports of its day hold different numbers of it"},
	{"unpaired_reversal", on("entry"), attention, "a Cancel Buy or Cancel Sell without its original"},
	{"reversal_signs", on("entry"), attention, "a Cancel Buy or Cancel Sell whose Amount has the original's sign, so its cash counts twice"},
	{"no_account_number", on("snapshot"), attention, "no readable account number"},
	{"snapshot_conflict", on("snapshot"), attention, "two different snapshots of one account at one time"},
	{"in_session", on("snapshot"), info, "taken during a session"},
	{"before_calendar", on("snapshot"), info, "taken before 2000, where the calendar starts"},
	{"calendar_unverified", on("snapshot"), info, "past the years the calendar was checked against NYSE's published dates"},
	{"empty", on("file"), attention, "the file holds no transactions"},
	{"at_cap", on("file"), attention, "the file holds as many rows as an export can, so its oldest days may be missing"},
	{"two_years", on("file"), attention, "a range of more than two years whose oldest row is near its two-year limit"},
	{"late_start", on("file"), attention, "the oldest row is more than a week after the range's start"},
	{"total_mismatch", on("file"), attention, "the stated total is not the rows' sum"},
	{"lacks_rows", on("file"), attention, "it lacks, in a filter's shape, rows that other sources of the account hold in its dates"},
	{"looks_filtered", on("file"), attention, "every row is of one family or one symbol over a long span: an export filtered by type or symbol?"},
	{"completeness_unknown", on("file"), info, "nothing yet shows that the file holds every type and symbol"},
	{"coverage_clipped", on("file"), info, "its coverage is narrower than its stated range"},
	{"encoding_fallback", on("file"), info, "the file is not UTF-8 and was read as Windows-1252"},
	{"lazy_quotes", on("file"), info, "the file's quotes were read leniently"},
	{"coverage_from_rows", on("file"), info, "the file states no range, so its rows' dates give it"},
	{"rows_outside_coverage", on("file"), info, "rows lie outside the stated range, which they widen"},
	{"undated_rows", on("file"), attention, "rows without a date, which make no transaction"},
	{"unknown_actions", on("file"), info, "rows with actions schwabtx does not know"},
	{"name_mismatch", on("file"), attention, "the digits in the file's name are not those of its account"},
	{"columns_disagree", on("lot"), info, "the gain or loss is not proceeds less cost basis, with or without the disallowed loss"},
	{"placeholder", on("lot"), info, "a money column holds a placeholder such as --"},
	{"lot_revised", on("lot"), info, "exports show the lot in more than one version"},
}

// on lists what a flag can be on.
func on(subjects ...string) []string { return subjects }

// flagSeverity returns the severity of the flag code.
func flagSeverity(code string) string {
	i := slices.IndexFunc(flagDefs, func(d flagDef) bool { return d.code == code })
	if i < 0 {
		panic("history: no flag " + code)
	}
	return flagDefs[i].severity
}

// writeFlagDefs brings flag_def to flagDefs, writing only the rows that
// differ, once in a write, before the write's first flag: flagDefs does not
// change while it runs. A code that flagDefs no longer holds is deleted,
// so foreign keys are checked at the commit, by when no flag holds it.
func writeFlagDefs(w *wtx) error {
	if w.flagDefs {
		return nil
	}
	if err := w.deferForeignKeys(); err != nil {
		return err
	}
	have := map[string]flagDef{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var d flagDef
		var subject string
		err := rows.Scan(&d.code, &subject, &d.severity, &d.meaning)
		d.subjects = on(subject)
		have[d.code] = d
		return err
	}, "SELECT code, subject, severity, meaning FROM flag_def")
	if err != nil {
		return err
	}
	for _, d := range flagDefs {
		h, ok := have[d.code]
		delete(have, d.code)
		if ok && h.subjects[0] == d.subjects[0] && h.severity == d.severity && h.meaning == d.meaning {
			continue
		}
		if _, err := w.exec(`INSERT INTO flag_def (code, subject, severity, meaning) VALUES (?, ?, ?, ?)
			ON CONFLICT (code) DO UPDATE SET subject = excluded.subject, severity = excluded.severity, meaning = excluded.meaning`,
			d.code, d.subjects[0], d.severity, d.meaning); err != nil {
			return err
		}
	}
	for _, code := range slices.Sorted(maps.Keys(have)) {
		if _, err := w.exec("DELETE FROM flag_def WHERE code = ?", code); err != nil {
			return err
		}
	}
	w.flagDefs = true
	return nil
}

// An entryFlag is a flag on an entry and its detail, which says what:
// composed of free text masked before (mask.Text) and of identifiers,
// amounts and literals, which are never masked.
type entryFlag struct {
	code   string
	detail string
}

// flagSet gathers an entry's flags, each code once: a code raised twice
// keeps both details.
type flagSet []entryFlag

// add raises code with detail.
func (s *flagSet) add(code, detail string) {
	flagSeverity(code) // a code not in flagDefs is a bug
	for i, f := range *s {
		if f.code == code {
			if detail != "" && !slices.Contains(strings.Split(f.detail, "; "), detail) {
				(*s)[i].detail = strings.TrimPrefix(f.detail+"; "+detail, "; ")
			}
			return
		}
	}
	*s = append(*s, entryFlag{code, detail})
}

// drop removes the flags of codes.
func (s *flagSet) drop(codes ...string) {
	*s = slices.DeleteFunc(*s, func(f entryFlag) bool { return slices.Contains(codes, f.code) })
}

// has reports whether code is raised.
func (s flagSet) has(code string) bool {
	return slices.ContainsFunc(s, func(f entryFlag) bool { return f.code == code })
}

// sorted returns the flags in the order of their codes.
func (s flagSet) sorted() []entryFlag {
	out := slices.Clone([]entryFlag(s))
	slices.SortFunc(out, func(a, b entryFlag) int { return cmp.Compare(a.code, b.code) })
	return out
}
