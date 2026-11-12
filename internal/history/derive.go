package history

import (
	"database/sql"
	"maps"
	"slices"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// derivedTables are the tables that hold what the rules derive from the
// evidence, which a rebuild empties and fills again.
var derivedTables = []string{
	"flag", "flag_def", "match_candidate", "match_member", "match", "entry", "coverage",
	"lot_member", "realized_lot", "realized_row", "csv_member", "csv_txn", "history_row",
	"file_fields", "snapshot_position", "snapshot_fields", "market_session", "api_leg",
	"revision_cut", "txn", "key_name", "instrument_key", "security",
}

// derive brings what the write changed up to date. When it added or
// changed a key, a name or a transaction's legs, it groups the keys into
// securities again (layer 3), which may touch the accounts whose legs
// moved; when it added or derived a snapshot, it brings the market
// sessions to the snapshots' span (layer 3). Then it derives again, from
// scratch, each account that the write has touched (layer 2): it loads the
// account's evidence, derives it with deriveAccount, and writes what
// changed. Derived tables refer to one another, so their foreign keys are
// checked at the commit, where a dangling reference still fails it. What
// is derived is no longer marked, so a write that needs the derivation's
// results can derive early, and the end of the write does nothing more.
func (w *wtx) derive() error {
	if len(w.touched) == 0 && !w.regroup && !w.sessions {
		return nil
	}
	if err := w.deferForeignKeys(); err != nil {
		return err
	}
	if w.regroup {
		if err := groupSecurities(w); err != nil {
			return err
		}
		w.regroup = false
	}
	if w.sessions {
		if err := deriveSessions(w); err != nil {
			return err
		}
		w.sessions = false
	}
	for _, id := range slices.Sorted(maps.Keys(w.touched)) {
		in, err := loadAccount(w, id)
		if err != nil {
			return err
		}
		if err := writeAccount(w, id, deriveAccount(in)); err != nil {
			return err
		}
		delete(w.touched, id)
	}
	return nil
}

// loadAccount reads what deriveAccount needs of account id.
func loadAccount(w *wtx, id int64) (accountInputs, error) {
	in := accountInputs{account: id, margin: w.s.o.margin}
	byID := map[int64]int{} // txn_id -> index in in.txns
	err := queryEach(w, func(rows *sql.Rows) error {
		var t txnInput
		var latest sql.NullInt64
		var at, trade, day sql.NullString
		if err := rows.Scan(&t.id, &t.firstRead, &latest, &at, &trade, &day); err != nil {
			return err
		}
		t.latestListRead, t.tradeDay = latest.Int64, nytime.Date(day.String)
		var err error
		if t.time, err = parseNullInstant(at); err != nil {
			return err
		}
		if t.tradeDate, err = parseNullInstant(trade); err != nil {
			return err
		}
		byID[t.id] = len(in.txns)
		in.txns = append(in.txns, t)
		return nil
	}, `SELECT t.txn_id, t.first_read_id, t.latest_list_read_id, x.time, x.trade_date, x.trade_day
		FROM api_txn t JOIN txn x ON x.txn_id = t.txn_id WHERE t.account_id = ?`, id)
	if err != nil {
		return in, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var txn int64
		var digest []byte
		if err := rows.Scan(&txn, &digest); err != nil {
			return err
		}
		t := &in.txns[byID[txn]]
		t.digests = append(t.digests, [32]byte(digest))
		return nil
	}, `SELECT v.txn_id, c.digest FROM api_revision v JOIN api_txn t ON t.txn_id = v.txn_id
		JOIN revision_cut c ON c.revision_id = v.revision_id WHERE t.account_id = ? ORDER BY v.txn_id, v.seq`, id)
	if err != nil {
		return in, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var r readInput
		var start, end, sent string
		if err := rows.Scan(&r.id, &start, &end, &sent); err != nil {
			return err
		}
		var err error
		if r.start, err = parseInstant(start); err != nil {
			return err
		}
		if r.end, err = parseInstant(end); err != nil {
			return err
		}
		r.sent, err = parseInstant(sent)
		in.reads = append(in.reads, r)
		return err
	}, "SELECT read_id, window_start, window_end, sent_at FROM api_read WHERE account_id = ? AND complete = 1", id)
	if err != nil {
		return in, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var a absence
		if err := rows.Scan(&a.read, &a.txn); err != nil {
			return err
		}
		in.absences = append(in.absences, a)
		return nil
	}, `SELECT a.read_id, a.txn_id FROM api_absent a JOIN api_txn t ON t.txn_id = a.txn_id WHERE t.account_id = ?`, id)
	if err != nil {
		return in, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var sn snapshotInput
		var at string
		var typ, market, session sql.NullString
		if err := rows.Scan(&sn.id, &at, &typ, &market, &session); err != nil {
			return err
		}
		sn.accountType, sn.market, sn.inSession = typ.String, nytime.Date(market.String), nytime.Date(session.String)
		var err error
		sn.fetchedAt, err = parseInstant(at)
		in.snapshots = append(in.snapshots, sn)
		return err
	}, "SELECT snapshot_id, fetched_at, account_type, market_date, in_session_date FROM snapshot_fields WHERE account_id = ?", id)
	return in, err
}

// writeAccount writes what deriveAccount gave account id, changing only
// the rows that differ from what the store holds, so that deriving an
// account whose evidence did not change writes nothing.
func writeAccount(w *wtx, id int64, out accountOutputs) error {
	for _, t := range out.txns {
		gone := sql.NullInt64{Int64: t.goneRead, Valid: t.goneRead != 0}
		if _, err := w.exec(`UPDATE txn SET revisions = ?, gone_read_id = ?
			WHERE txn_id = ? AND (revisions IS NOT ? OR gone_read_id IS NOT ?)`,
			t.revisions, gone, t.id, t.revisions, gone); err != nil {
			return err
		}
	}
	if _, err := w.exec("UPDATE account SET account_type = ?1 WHERE account_id = ?2 AND account_type IS NOT ?1",
		nullString(out.accountType), id); err != nil {
		return err
	}
	var have []coverageRun
	err := queryEach(w, func(rows *sql.Rows) error {
		var c coverageRun
		var from, to string
		if err := rows.Scan(&c.source, &from, &to); err != nil {
			return err
		}
		c.from, c.to = nytime.Date(from), nytime.Date(to)
		have = append(have, c)
		return nil
	}, "SELECT source, from_date, to_date FROM coverage WHERE account_id = ? ORDER BY source, from_date", id)
	if err != nil || slices.Equal(have, out.coverage) {
		return err
	}
	if _, err := w.exec("DELETE FROM coverage WHERE account_id = ?", id); err != nil {
		return err
	}
	for _, c := range out.coverage {
		if _, err := w.exec("INSERT INTO coverage (account_id, source, from_date, to_date) VALUES (?, ?, ?, ?)",
			id, c.source, string(c.from), string(c.to)); err != nil {
			return err
		}
	}
	return nil
}

// queryEach runs a query in the write and calls each for every row.
func queryEach(w *wtx, each func(*sql.Rows) error, query string, args ...any) error {
	rows, err := w.tx.QueryContext(w.ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := each(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// parseNullInstant reads an instant column, the zero time for NULL.
func parseNullInstant(s sql.NullString) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	return parseInstant(s.String)
}

// rebuild derives the store again under this build's rules and calendar.
// It empties every derived table, writing no page of evidence, and fills
// them again: first what each piece of evidence gives by itself (layer 1),
// then the securities and the market sessions (layer 3) and each account
// (layer 2), and records the versions it derived with.
// Derived tables refer to one another, so foreign keys are checked at the
// commit, where a dangling reference still fails it.
func rebuild(w *wtx) error {
	if err := w.deferForeignKeys(); err != nil {
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
	if err := rebuildTxns(w); err != nil {
		return err
	}
	if err := rebuildSnapshots(w); err != nil {
		return err
	}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		err := rows.Scan(&id)
		w.touch(id)
		return err
	}, "SELECT account_id FROM account"); err != nil {
		return err
	}
	if err := w.derive(); err != nil {
		return err
	}
	b := buildVersions()
	_, err := w.exec("UPDATE store SET derived_with = ?, calendar_version = ? WHERE id = 1", b.derive, b.calendar)
	return err
}

// rebuildBatch is how many transactions rebuildTxns reads at a time, so
// that it holds no more of their raw JSON in memory than that.
const rebuildBatch = 100

// rebuildTxns derives each API transaction's columns and legs from its
// latest raw JSON, and each of its revisions' cut digests and changed key
// paths from their raw JSON, in the order of their ids; and the keys and
// names of their instruments from the raw JSON the store keeps, each
// revision's as of when it was first seen and the latest as of when it
// was seen.
func rebuildTxns(w *wtx) error {
	type stored struct {
		id, account       int64
		number, raw, seen string
	}
	for after := int64(0); ; {
		var batch []stored
		err := queryEach(w, func(rows *sql.Rows) error {
			var t stored
			err := rows.Scan(&t.id, &t.account, &t.number, &t.raw, &t.seen)
			batch = append(batch, t)
			return err
		}, `SELECT t.txn_id, t.account_id, a.number, t.latest_raw, t.latest_seen_at FROM api_txn t
			JOIN account a ON a.account_id = t.account_id WHERE t.txn_id > ? ORDER BY t.txn_id LIMIT ?`, after, rebuildBatch)
		if err != nil || len(batch) == 0 {
			return err
		}
		for _, t := range batch {
			latest := decodeTxn(t.raw)
			if err := deriveTxn(w, t.id, t.account, t.number, latest); err != nil {
				return err
			}
			if err := rebuildCuts(w, t.id); err != nil {
				return err
			}
			seen, err := parseInstant(t.seen)
			if err != nil {
				return err
			}
			if err := observeTxn(w, latest, seen); err != nil {
				return err
			}
		}
		after = batch[len(batch)-1].id
	}
}

// rebuildCuts derives the cut digest of each of transaction txn's
// revisions, the key paths that changed from the one before, and the keys
// and names of its instruments as of when it was first seen.
func rebuildCuts(w *wtx, txn int64) error {
	type revision struct {
		id        int64
		raw, seen string
	}
	var revs []revision
	if err := queryEach(w, func(rows *sql.Rows) error {
		var r revision
		err := rows.Scan(&r.id, &r.raw, &r.seen)
		revs = append(revs, r)
		return err
	}, "SELECT revision_id, raw, first_seen_at FROM api_revision WHERE txn_id = ? ORDER BY seq", txn); err != nil {
		return err
	}
	for i, r := range revs {
		d, err := cutDigest([]byte(r.raw))
		if err != nil {
			return err
		}
		seen, err := parseInstant(r.seen)
		if err != nil {
			return err
		}
		if err := observeTxn(w, decodeTxn(r.raw), seen); err != nil {
			return err
		}
		var changed []string
		if i > 0 {
			if changed, err = changedPaths([]byte(revs[i-1].raw), []byte(r.raw)); err != nil {
				return err
			}
		}
		if err := insertCut(w, r.id, d, changed); err != nil {
			return err
		}
	}
	return nil
}
