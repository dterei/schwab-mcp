package history

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// A new store has schema 1 whole, its macros written out as GLOB checks:
// 35 tables, 19 triggers and 21 views, each of which reads on an empty
// store with trusted_schema off, as the readers' connections have it; it
// is marked as a schwabtx store and records the build and versions that
// made it.
func TestSchema(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ctx := t.Context()
	counts := map[string]int{}
	var views []string
	rows, err := s.r.QueryContext(ctx, "SELECT type, name, coalesce(sql, '') FROM sqlite_schema")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, text string
		if err := rows.Scan(&typ, &name, &text); err != nil {
			t.Fatal(err)
		}
		counts[typ]++
		if typ == "view" {
			views = append(views, name)
		}
		if strings.Contains(text, "INSTANT(") || strings.Contains(text, "DAY(") {
			t.Errorf("%s %s holds a macro: %s", typ, name, text)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	for typ, want := range map[string]int{"table": 35, "trigger": 19, "view": 21} {
		if counts[typ] != want {
			t.Errorf("the store has %d of type %s, want %d", counts[typ], typ, want)
		}
	}
	for _, v := range views {
		rows, err := s.r.QueryContext(ctx, "SELECT * FROM "+v)
		if err != nil {
			t.Errorf("view %s: %v", v, err)
			continue
		}
		if rows.Next() {
			t.Errorf("view %s has a row on an empty store", v)
		}
		if err := rows.Err(); err != nil {
			t.Errorf("view %s: %v", v, err)
		}
		rows.Close()
	}
	var id, version int
	var mode string
	for q, dst := range map[string]any{"PRAGMA application_id": &id, "PRAGMA user_version": &version, "PRAGMA journal_mode": &mode} {
		if err := s.r.QueryRowContext(ctx, q).Scan(dst); err != nil {
			t.Fatal(err)
		}
	}
	if id != 0x53575458 || version != 1 || mode != "wal" {
		t.Errorf("application_id %#x, user_version %d, journal_mode %s; want 0x53575458, 1, wal", id, version, mode)
	}
	var createdAt, createdBy string
	var derived, calendar int
	if err := s.r.QueryRowContext(ctx, "SELECT created_at, created_by, derived_with, calendar_version FROM store").
		Scan(&createdAt, &createdBy, &derived, &calendar); err != nil {
		t.Fatal(err)
	}
	if createdAt != "2026-11-12T15:04:05.678Z" || !strings.HasPrefix(createdBy, "schwabtx") || derived != DeriveVersion || calendar != 1 {
		t.Errorf("the store row holds %s, %q, rules %d, calendar %d", createdAt, createdBy, derived, calendar)
	}
}

// The macros expand to GLOB checks of the column they name, and nothing
// else is touched.
func TestExpandMacros(t *testing.T) {
	t.Parallel()
	got := expandMacros("CHECK (INSTANT(seen_at) AND DAY(day)) -- INSTANTS(x) DAYS(y) day(z)")
	want := "CHECK (seen_at GLOB " + instantGlob + " AND day GLOB " + dayGlob + ") -- INSTANTS(x) DAYS(y) day(z)"
	if got != want {
		t.Errorf("expandMacros: %s\nwant %s", got, want)
	}
}

// The extended result codes of the refusals.
const (
	codeCheck    = 275  // SQLITE_CONSTRAINT_CHECK
	codeFK       = 787  // SQLITE_CONSTRAINT_FOREIGNKEY
	codeNotNull  = 1299 // SQLITE_CONSTRAINT_NOTNULL
	codeTrigger  = 1811 // SQLITE_CONSTRAINT_TRIGGER
	codeUnique   = 2067 // SQLITE_CONSTRAINT_UNIQUE
	codeDatatype = 3091 // SQLITE_CONSTRAINT_DATATYPE
)

// schemaRows are valid rows of the evidence tables and of a few derived
// ones, which TestSchemaRefuses changes one value at a time.
const schemaRows = `
INSERT INTO account (account_id, number, created_from, first_seen_at, last_seen_at)
  VALUES (1, '12345678', 'api', '2026-11-12T15:00:00.000Z', '2026-11-12T15:00:00.000Z');
INSERT INTO account_hash VALUES (1, 'HASH', '2026-11-12T15:00:00.000Z', '2026-11-12T15:00:00.000Z');
INSERT INTO api_read (read_id, endpoint, account_id, window_start, window_end, all_types, sent_at, answered_at, outcome, complete)
  VALUES (1, 'transactions', 1, '2026-11-01T00:00:00.000Z', '2026-11-12T00:00:00.000Z', 1,
          '2026-11-12T15:00:00.000Z', '2026-11-12T15:00:01.000Z', 'answered', 1),
         (2, 'transactions', 1, '2026-11-01T00:00:00.000Z', '2026-11-12T00:00:00.000Z', 1,
          '2026-11-12T16:00:00.000Z', '2026-11-12T16:00:01.000Z', 'answered', 1),
         (3, 'transactions', 1, '2026-11-01T00:00:00.000Z', '2026-11-12T00:00:00.000Z', 1,
          '2026-11-12T17:00:00.000Z', '2026-11-12T17:00:01.000Z', 'answered', 1);
INSERT INTO api_txn (txn_id, account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at,
                     latest_raw, latest_read_id, latest_seen_at, latest_list_read_id)
  VALUES (1, 1, 7, 'number', '{}', 1, '2026-11-12T15:00:01.000Z', '{}', 2, '2026-11-12T16:00:01.000Z', 2);
INSERT INTO txn (txn_id, account_id, activity_id) VALUES (1, 1, 7);
INSERT INTO api_revision VALUES (1, 1, 1, '{}', 1, '2026-11-12T15:00:01.000Z', 2, '2026-11-12T16:00:01.000Z');
INSERT INTO api_absent VALUES (1, 1);
INSERT INTO snapshot_import (import_id, label, imported_at, lines, new, known)
  VALUES (1, 'snapshots.jsonl', '2026-11-12T15:00:00.000Z', 1, 1, 0);
INSERT INTO snapshot (snapshot_id, fetched_at, raw, digest, source, import_id, line)
  VALUES (1, '2026-11-12T15:00:00.000Z', '{}', zeroblob(32), 'file', 1, 1);
INSERT INTO import_file (file_id, account_id, kind, name, content, sha256, imported_at)
  VALUES (1, 1, 'history_csv', 'x.csv', X'00', zeroblob(32), '2026-11-12T15:00:00.000Z');
INSERT INTO decision (decision_id, made_at, kind, subject, value) VALUES (1, '2026-11-12T15:00:00.000Z', 'category', 'a:1:7', 'fee');
INSERT INTO flag_def VALUES ('gone', 'entry', 'attention', 'Schwab no longer returns it');
INSERT INTO entry (entry_id, account_id, txn_id, ref, subject, family, category, category_rule, counted, counted_why)
  VALUES (1, 1, 1, 'T7', 'a:1:7', 'trade', 'buy', 'trade.buy', 1, 'api');
`

// Each trigger and check of the schema refuses what it is there to refuse,
// with its code, and lets through what only moves forward.
func TestSchemaRefuses(t *testing.T) {
	t.Parallel()
	path := copyTemplate(t)
	db, err := sql.Open("sqlite", dsn(path, "_pragma=foreign_keys(1)", "_pragma=trusted_schema(0)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx := t.Context()
	if _, err := db.ExecContext(ctx, schemaRows); err != nil {
		t.Fatal(err)
	}
	const (
		i = "'2026-11-12T15:00:00.000Z'"
		d = "'2026-11-12'"
	)
	readRow := func(endpoint, more string) string {
		cols, vals := "endpoint, sent_at, answered_at, outcome", "'"+endpoint+"', "+i+", "+i+", 'answered'"
		if more != "" {
			c, v, _ := strings.Cut(more, "|")
			cols, vals = cols+", "+c, vals+", "+v
		}
		return "INSERT INTO api_read (" + cols + ") VALUES (" + vals + ")"
	}
	cases := []struct {
		name string
		stmt string
		code int // 0: it is let through
	}{
		// The triggers.
		{"account_number_fixed", "UPDATE account SET number = '87654321'", codeTrigger},
		{"account_kept", "DELETE FROM account", codeTrigger},
		{"account_hash_kept", "DELETE FROM account_hash", codeTrigger},
		{"api_read_fixed", "UPDATE api_read SET row_count = 1 WHERE read_id = 3", codeTrigger},
		{"api_read_kept", "DELETE FROM api_read WHERE read_id = 3", codeTrigger},
		{"api_txn_first_fixed", "UPDATE api_txn SET first_raw = '[]'", codeTrigger},
		{"api_txn_first_fixed id", "UPDATE api_txn SET activity_id = 8", codeTrigger},
		{"api_txn_latest_forward read", "UPDATE api_txn SET latest_read_id = 1", codeTrigger},
		{"api_txn_latest_forward list read", "UPDATE api_txn SET latest_list_read_id = 1", codeTrigger},
		{"api_txn_latest_forward list read to NULL", "UPDATE api_txn SET latest_list_read_id = NULL", codeTrigger},
		{"api_txn latest moves forward", "UPDATE api_txn SET latest_raw = '[]', latest_read_id = 3, latest_list_read_id = 3", 0},
		{"api_txn_kept", "DELETE FROM api_txn", codeTrigger},
		{"api_revision_fixed", "UPDATE api_revision SET raw = '[]'", codeTrigger},
		{"api_revision last read moves", "UPDATE api_revision SET last_read_id = 3, last_seen_at = '2026-11-12T17:00:01.000Z'", 0},
		{"api_revision_kept", "DELETE FROM api_revision", codeTrigger},
		{"api_absent_fixed", "UPDATE api_absent SET read_id = 2", codeTrigger},
		{"api_absent_kept", "DELETE FROM api_absent", codeTrigger},
		{"snapshot_import_kept", "DELETE FROM snapshot_import", codeTrigger},
		{"snapshot_fixed", "UPDATE snapshot SET line = 2", codeTrigger},
		{"snapshot_kept", "DELETE FROM snapshot", codeTrigger},
		{"import_file_fixed", "UPDATE import_file SET name = 'y.csv'", codeTrigger},
		{"import_file_kept", "DELETE FROM import_file", codeTrigger},
		{"decision_fixed", "UPDATE decision SET note = 'x'", codeTrigger},
		{"decision_kept", "DELETE FROM decision", codeTrigger},
		{"account seen moves", "UPDATE account SET last_seen_at = '2026-11-13T15:00:00.000Z', nickname = 'Joint'", 0},

		// The checks, by table.
		{"store id", "INSERT INTO store VALUES (2, " + i + ", 'x', 1, 1)", codeCheck},
		{"store derived_with", "UPDATE store SET derived_with = 0", codeCheck},
		{"store calendar_version", "UPDATE store SET calendar_version = 0", codeCheck},
		{"store created_at", "UPDATE store SET created_at = '2026-11-12 15:00:00'", codeCheck},
		{"account number blank", "INSERT INTO account (number, created_from, first_seen_at, last_seen_at) VALUES ('', 'api', " + i + ", " + i + ")", codeCheck},
		{"account number spaced", "INSERT INTO account (number, created_from, first_seen_at, last_seen_at) VALUES (' 2', 'api', " + i + ", " + i + ")", codeCheck},
		{"account number unique", "INSERT INTO account (number, created_from, first_seen_at, last_seen_at) VALUES ('12345678', 'api', " + i + ", " + i + ")", codeUnique},
		{"account created_from", "INSERT INTO account (number, created_from, first_seen_at, last_seen_at) VALUES ('2', 'bank', " + i + ", " + i + ")", codeCheck},
		{"account created_from missing", "INSERT INTO account (number, first_seen_at, last_seen_at) VALUES ('2', " + i + ", " + i + ")", codeNotNull},
		{"account seen backwards", "UPDATE account SET last_seen_at = '2026-11-11T15:00:00.000Z'", codeCheck},
		{"account primary", "UPDATE account SET primary_account = 2", codeCheck},
		{"account_hash blank", "INSERT INTO account_hash VALUES (1, '', " + i + ", " + i + ")", codeCheck},
		{"account_hash of no account", "INSERT INTO account_hash VALUES (9, 'H', " + i + ", " + i + ")", codeFK},
		{"api_read endpoint", readRow("orders", ""), codeCheck},
		{"api_read transactions without account", readRow("transactions", "window_start, window_end|"+i+", '2026-11-13T00:00:00.000Z'"), codeCheck},
		{"api_read file without file", readRow("file", "account_id|1"), codeCheck},
		{"api_read file_id without file", readRow("accounts", "file_id|1"), codeCheck},
		{"api_read window", readRow("transactions", "account_id, window_start, window_end|1, "+i+", "+i), codeCheck},
		{"api_read transaction without id", readRow("transaction", "account_id|1"), codeCheck},
		{"api_read complete not said", readRow("transactions", "account_id, window_start, window_end, all_types|1, "+i+", '2026-11-13T00:00:00.000Z', 1"), codeCheck},
		{"api_read complete with a symbol", readRow("transactions", "account_id, window_start, window_end, all_types, symbol, complete|1, "+i+", '2026-11-13T00:00:00.000Z', 1, 'XYZ', 1"), codeCheck},
		{"api_read complete capped", readRow("transactions", "account_id, window_start, window_end, all_types, capped, complete|1, "+i+", '2026-11-13T00:00:00.000Z', 1, 1, 1"), codeCheck},
		{"api_read complete refused", "INSERT INTO api_read (endpoint, account_id, window_start, window_end, all_types, sent_at, answered_at, outcome, complete) VALUES ('transactions', 1, " + i + ", '2026-11-13T00:00:00.000Z', 1, " + i + ", " + i + ", 'refused', 1)", codeCheck},
		{"api_read answered before sent", "INSERT INTO api_read (endpoint, sent_at, answered_at, outcome) VALUES ('accounts', '2026-11-12T15:00:01.000Z', " + i + ", 'answered')", codeCheck},
		{"api_read outcome", "INSERT INTO api_read (endpoint, sent_at, answered_at, outcome) VALUES ('accounts', " + i + ", " + i + ", 'lost')", codeCheck},
		{"api_read duplicates", readRow("accounts", "duplicates|'[1'"), codeCheck},
		{"api_read row_count", readRow("accounts", "row_count|-1"), codeCheck},
		{"api_read row_count type", readRow("accounts", "row_count|1.5"), codeDatatype},
		{"api_read all_types", readRow("accounts", "all_types|2"), codeCheck},
		{"api_txn id and source", "INSERT INTO api_txn (account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (1, NULL, 'number', '{}', 1, " + i + ", '{}', 1, " + i + ")", codeCheck},
		{"api_txn id positive", "INSERT INTO api_txn (account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (1, 0, 'number', '{}', 1, " + i + ", '{}', 1, " + i + ")", codeCheck},
		{"api_txn one per id", "INSERT INTO api_txn (account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (1, 7, 'string', '{}', 1, " + i + ", '{}', 1, " + i + ")", codeUnique},
		{"api_txn two without ids", "INSERT INTO api_txn (account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (1, NULL, 'none', '{}', 1, " + i + ", '{}', 1, " + i + "), (1, NULL, 'none', '{}', 1, " + i + ", '{}', 1, " + i + ")", 0},
		{"api_txn of no account", "INSERT INTO api_txn (account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (9, 8, 'number', '{}', 1, " + i + ", '{}', 1, " + i + ")", codeFK},
		{"api_txn seen backwards", "INSERT INTO api_txn (account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (1, 8, 'number', '{}', 1, " + i + ", '{}', 1, '2026-11-11T15:00:00.000Z')", codeCheck},
		{"txn instant", "UPDATE txn SET time = '2026-11-12T15:00:00Z'", codeCheck},
		{"txn day", "UPDATE txn SET trade_day = '11/12/2026'", codeCheck},
		{"txn problems", "UPDATE txn SET problems = 'none'", codeCheck},
		{"txn revisions", "UPDATE txn SET revisions = 0", codeCheck},
		{"txn net_cents type", "UPDATE txn SET net_cents = 1.5", codeDatatype},
		{"txn qualified_dividend", "UPDATE txn SET qualified_dividend = 2", codeCheck},
		{"api_revision seq", "INSERT INTO api_revision (txn_id, seq, raw, first_read_id, first_seen_at, last_read_id, last_seen_at) VALUES (1, 0, '{}', 1, " + i + ", 1, " + i + ")", codeCheck},
		{"api_revision one per seq", "INSERT INTO api_revision (txn_id, seq, raw, first_read_id, first_seen_at, last_read_id, last_seen_at) VALUES (1, 1, '{}', 1, " + i + ", 1, " + i + ")", codeUnique},
		{"revision_cut digest", "INSERT INTO revision_cut VALUES (1, 1, zeroblob(31), NULL)", codeCheck},
		{"revision_cut digest type", "INSERT INTO revision_cut VALUES (1, 1, 'digest', NULL)", codeDatatype},
		{"revision_cut changed", "INSERT INTO revision_cut VALUES (1, 1, zeroblob(32), '[status')", codeCheck},
		{"revision_cut cut_version", "INSERT INTO revision_cut VALUES (1, 0, zeroblob(32), NULL)", codeCheck},
		{"api_absent of no read", "INSERT INTO api_absent VALUES (9, 1)", codeFK},
		{"api_leg cents on a security", "INSERT INTO api_leg (txn_id, leg, asset_type, amount_cents) VALUES (1, 0, 'EQUITY', 100)", codeCheck},
		{"api_leg index", "INSERT INTO api_leg (txn_id, leg) VALUES (1, -1)", codeCheck},
		{"security put_call", "INSERT INTO security (canonical_key, definite, put_call, first_seen_at, last_seen_at) VALUES ('i:1', 1, 'BOTH', " + i + ", " + i + ")", codeCheck},
		{"security definite", "INSERT INTO security (canonical_key, definite, first_seen_at, last_seen_at) VALUES ('i:1', 2, " + i + ", " + i + ")", codeCheck},
		{"security expiration", "INSERT INTO security (canonical_key, definite, expiration_day, first_seen_at, last_seen_at) VALUES ('i:1', 1, '2026-11', " + i + ", " + i + ")", codeCheck},
		{"instrument_key key", "INSERT INTO security VALUES (1, 'i:1', 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, " + i + ", " + i + "); INSERT INTO instrument_key (key, security_id, first_seen_at, last_seen_at) VALUES ('z:1', 1, " + i + ", " + i + ")", codeCheck},
		{"instrument_key empty", "INSERT INTO security VALUES (1, 'i:1', 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, " + i + ", " + i + "); INSERT INTO instrument_key (key, security_id, first_seen_at, last_seen_at) VALUES ('i:', 1, " + i + ", " + i + ")", codeCheck},
		{"instrument_key of no security", "INSERT INTO instrument_key (key, security_id, first_seen_at, last_seen_at) VALUES ('i:1', 9, " + i + ", " + i + ")", codeFK},
		{"key_name kind", "INSERT INTO security VALUES (1, 'i:1', 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, " + i + ", " + i + "); INSERT INTO instrument_key (key, security_id, first_seen_at, last_seen_at) VALUES ('i:1', 1, " + i + ", " + i + "); INSERT INTO key_name VALUES ('i:1', 'ticker', 'XYZ', 'transaction', " + i + ", " + i + ")", codeCheck},
		{"key_name value", "INSERT INTO security VALUES (1, 'i:1', 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, " + i + ", " + i + "); INSERT INTO instrument_key (key, security_id, first_seen_at, last_seen_at) VALUES ('i:1', 1, " + i + ", " + i + "); INSERT INTO key_name VALUES ('i:1', 'symbol', '', 'transaction', " + i + ", " + i + ")", codeCheck},
		{"key_name seen_in", "INSERT INTO security VALUES (1, 'i:1', 1, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, " + i + ", " + i + "); INSERT INTO instrument_key (key, security_id, first_seen_at, last_seen_at) VALUES ('i:1', 1, " + i + ", " + i + "); INSERT INTO key_name VALUES ('i:1', 'symbol', 'XYZ', 'rumour', " + i + ", " + i + ")", codeCheck},
		{"snapshot_import counts", "INSERT INTO snapshot_import (label, imported_at, lines, new, known) VALUES ('s', " + i + ", -1, 0, 0)", codeCheck},
		{"snapshot_import skipped", "INSERT INTO snapshot_import (label, imported_at, lines, new, known, skipped) VALUES ('s', " + i + ", 0, 0, 0, '{')", codeCheck},
		{"snapshot from a file without a line", "INSERT INTO snapshot (fetched_at, raw, digest, source, import_id) VALUES (" + i + ", '[]', zeroblob(32), 'file', 1)", codeCheck},
		{"snapshot from a sync without a read", "INSERT INTO snapshot (fetched_at, raw, digest, source) VALUES (" + i + ", '[]', zeroblob(32), 'sync')", codeCheck},
		{"snapshot source", "INSERT INTO snapshot (fetched_at, raw, digest, source, read_id) VALUES (" + i + ", '[]', zeroblob(32), 'mail', 1)", codeCheck},
		{"snapshot once", "INSERT INTO snapshot (fetched_at, raw, digest, source, import_id, line) VALUES (" + i + ", '{}', zeroblob(32), 'file', 1, 2)", codeUnique},
		{"snapshot digest", "INSERT INTO snapshot (fetched_at, raw, digest, source, import_id, line) VALUES (" + i + ", '{}', zeroblob(16), 'file', 1, 2)", codeCheck},
		{"snapshot line", "INSERT INTO snapshot (fetched_at, raw, digest, source, import_id, line) VALUES (" + i + ", '{}', randomblob(32), 'file', 1, 0)", codeCheck},
		{"snapshot_fields market_date", "INSERT INTO snapshot_fields (snapshot_id, fetched_at, market_date) VALUES (1, " + i + ", '2026-11-1')", codeCheck},
		{"snapshot_fields balances", "INSERT INTO snapshot_fields (snapshot_id, fetched_at, balances) VALUES (1, " + i + ", '{')", codeCheck},
		{"snapshot_fields flag", "INSERT INTO snapshot_fields (snapshot_id, fetched_at, pfcb) VALUES (1, " + i + ", 3)", codeCheck},
		{"snapshot_position position", "INSERT INTO snapshot_position (snapshot_id, position) VALUES (1, -1)", codeCheck},
		{"snapshot_position of no snapshot", "INSERT INTO snapshot_position (snapshot_id, position) VALUES (9, 0)", codeFK},
		{"market_session closes after it opens", "INSERT INTO market_session VALUES (" + d + ", " + i + ", " + i + ", 0, 1)", codeCheck},
		{"market_session early", "INSERT INTO market_session VALUES (" + d + ", " + i + ", '2026-11-12T21:00:00.000Z', 2, 1)", codeCheck},
		{"import_file account for an API file", "INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at) VALUES (1, 'api_json', 'x.json', X'00', randomblob(32), " + i + ")", codeCheck},
		{"import_file no account", "INSERT INTO import_file (kind, name, content, sha256, imported_at) VALUES ('history_json', 'x.json', X'00', randomblob(32), " + i + ")", codeCheck},
		{"import_file once", "INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at) VALUES (1, 'history_csv', 'y.csv', X'00', zeroblob(32), " + i + ")", codeUnique},
		{"import_file kind", "INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at) VALUES (1, 'pdf', 'y.pdf', X'00', randomblob(32), " + i + ")", codeCheck},
		{"import_file range", "INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at, user_from, user_to) VALUES (1, 'history_csv', 'y.csv', X'00', randomblob(32), " + i + ", '2026-11-12', '2026-11-11')", codeCheck},
		{"import_file content type", "INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at) VALUES (1, 'history_csv', 'y.csv', 'text', randomblob(32), " + i + ")", codeDatatype},
		{"file_fields completeness", "INSERT INTO file_fields (file_id, ref, completeness) VALUES (1, 'F00', 'most')", codeCheck},
		{"file_fields complete_by", "INSERT INTO file_fields (file_id, ref, complete_by) VALUES (1, 'F00', 'luck')", codeCheck},
		{"file_fields coverage", "INSERT INTO file_fields (file_id, ref, covers_from, covers_to) VALUES (1, 'F00', '2026-11-12', '2026-11-11')", codeCheck},
		{"file_fields coverage_source", "INSERT INTO file_fields (file_id, ref, coverage_source) VALUES (1, 'F00', 'guess')", codeCheck},
		{"file_fields encoding", "INSERT INTO file_fields (file_id, ref, encoding) VALUES (1, 'F00', 'latin-1')", codeCheck},
		{"file_fields forgotten", "INSERT INTO file_fields (file_id, ref, forgotten) VALUES (1, 'F00', 2)", codeCheck},
		{"history_row coarse", "INSERT INTO history_row (file_id, ordinal, line, raw, action, symbol, description, quantity_text, price_text, coarse) VALUES (1, 1, 2, 'r', 'Buy', '', '', '', '', zeroblob(8))", codeCheck},
		{"history_row ordinal", "INSERT INTO history_row (file_id, ordinal, line, raw, action, symbol, description, quantity_text, price_text, coarse) VALUES (1, 0, 2, 'r', 'Buy', '', '', '', '', zeroblob(32))", codeCheck},
		{"realized_row fine", "INSERT INTO realized_row (file_id, ordinal, line, raw, symbol, opened_text, quantity_text, coarse, fine) VALUES (1, 1, 2, 'r', 'XYZ', '', '', zeroblob(32), zeroblob(1))", codeCheck},
		{"realized_row wash_sale", "INSERT INTO realized_row (file_id, ordinal, line, raw, symbol, opened_text, quantity_text, coarse, fine, wash_sale) VALUES (1, 1, 2, 'r', 'XYZ', '', '', zeroblob(32), zeroblob(32), 2)", codeCheck},
		{"entry of neither", "INSERT INTO entry (account_id, ref, subject, family, category, category_rule, counted, counted_why) VALUES (1, 'X', 's', 'f', 'buy', 'r', 1, 'api')", codeCheck},
		{"entry half a pair", "UPDATE entry SET pair_kind = 'journal'", codeCheck},
		{"entry category", "UPDATE entry SET category = 'trade'", codeCheck},
		{"entry counted_why", "UPDATE entry SET counted_why = 'because'", codeCheck},
		{"entry ref once per account", "INSERT INTO api_txn (txn_id, account_id, activity_id, id_source, first_raw, first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at) VALUES (2, 1, 8, 'number', '{}', 1, " + i + ", '{}', 1, " + i + "); INSERT INTO entry (account_id, txn_id, ref, subject, family, category, category_rule, counted, counted_why) VALUES (1, 2, 'T7', 's', 'f', 'buy', 'r', 1, 'api')", codeUnique},
		{"entry pair of no entry", "UPDATE entry SET pair_kind = 'sweep', pair_entry_id = 9", codeFK},
		{"match method", "INSERT INTO match (account_id, method) VALUES (1, 'guess')", codeCheck},
		{"match_member of neither", "INSERT INTO match (match_id, account_id, method) VALUES (1, 1, 'exact'); INSERT INTO match_member (match_id) VALUES (1)", codeCheck},
		{"match_member once", "INSERT INTO match (match_id, account_id, method) VALUES (1, 1, 'exact'); INSERT INTO match_member (match_id, txn_id) VALUES (1, 1), (1, 1)", codeUnique},
		{"decision kind", "INSERT INTO decision (made_at, kind, subject, value) VALUES (" + i + ", 'mood', 'a:1:7', 'x')", codeCheck},
		{"decision subject", "INSERT INTO decision (made_at, kind, subject, value) VALUES (" + i + ", 'category', '', 'x')", codeCheck},
		{"decision basis", "INSERT INTO decision (made_at, kind, subject, value, basis) VALUES (" + i + ", 'category', 'a:1:7', 'x', '{')", codeCheck},
		{"flag_def code", "INSERT INTO flag_def VALUES ('Gone', 'entry', 'info', 'm')", codeCheck},
		{"flag_def code characters", "INSERT INTO flag_def VALUES ('gone-2', 'entry', 'info', 'm')", codeCheck},
		{"flag_def subject", "INSERT INTO flag_def VALUES ('odd', 'account', 'info', 'm')", codeCheck},
		{"flag_def severity", "INSERT INTO flag_def VALUES ('odd', 'entry', 'grave', 'm')", codeCheck},
		{"flag of two subjects", "INSERT INTO flag (entry_id, snapshot_id, code) VALUES (1, 1, 'gone')", codeCheck},
		{"flag of no subject", "INSERT INTO flag (code) VALUES ('gone')", codeCheck},
		{"flag of an unknown code", "INSERT INTO flag (entry_id, code) VALUES (1, 'odd')", codeFK},
		{"flag once", "INSERT INTO flag (entry_id, code) VALUES (1, 'gone'), (1, 'gone')", codeUnique},
		{"coverage source", "INSERT INTO coverage VALUES (1, 'rumour', " + d + ", " + d + ")", codeCheck},
		{"coverage backwards", "INSERT INTO coverage VALUES (1, 'api', " + d + ", '2026-11-11')", codeCheck},
		{"sync_state backfill", "INSERT INTO sync_state (account_id, backfill, updated_at) VALUES (1, 'lost', " + i + ")", codeCheck},
		{"sync_state empty_windows", "INSERT INTO sync_state (account_id, empty_windows, updated_at) VALUES (1, -1, " + i + ")", codeCheck},
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, c := range cases {
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			t.Fatal(err)
		}
		_, err := conn.ExecContext(ctx, c.stmt)
		if got := code(err); got != c.code {
			t.Errorf("%s: %s\ngot code %d (%v), want %d", c.name, c.stmt, got, err, c.code)
		}
		if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
			t.Fatal(err)
		}
	}
}
