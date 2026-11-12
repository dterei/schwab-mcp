package history

// derivedTables are the tables that hold what the rules derive from the
// evidence, which a rebuild empties and fills again.
var derivedTables = []string{
	"flag", "flag_def", "match_candidate", "match_member", "match", "entry", "coverage",
	"lot_member", "realized_lot", "realized_row", "csv_member", "csv_txn", "history_row",
	"file_fields", "snapshot_position", "snapshot_fields", "market_session", "api_leg",
	"revision_cut", "txn", "key_name", "instrument_key", "security",
}

// rebuild derives the store again under this build's rules and calendar.
// So far no rule derives anything, so it empties every derived table,
// writing no page of evidence, and records the versions it derived with.
// Derived tables refer to one another, so foreign keys are checked at the
// commit, where a dangling reference still fails it.
func rebuild(w *wtx) error {
	if _, err := w.exec("PRAGMA defer_foreign_keys = ON"); err != nil {
		return err
	}
	for _, t := range derivedTables {
		if _, err := w.exec("DELETE FROM " + t); err != nil {
			return err
		}
	}
	if _, err := w.exec("UPDATE account SET account_type = NULL, kind = NULL"); err != nil {
		return err
	}
	b := buildVersions()
	_, err := w.exec("UPDATE store SET derived_with = ?, calendar_version = ? WHERE id = 1", b.derive, b.calendar)
	return err
}

// recalendar recomputes what depends on the NYSE calendar under this
// build's. So far nothing stored does, so it records the calendar's
// version.
func recalendar(w *wtx) error {
	_, err := w.exec("UPDATE store SET calendar_version = ? WHERE id = 1", buildVersions().calendar)
	return err
}
