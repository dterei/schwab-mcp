package history

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
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
// moved or whose entries' securities a symbol or your decision gives;
// when it added or derived a snapshot, it brings the market sessions to
// the snapshots' span (layer 3). Then it derives again, from scratch, each
// account that the write has touched (layer 2): it loads the account's
// evidence, derives it with deriveAccount, pairs journals across accounts
// (pairAccounts; an account whose pairs that changes is derived too), and
// writes what changed. Derived tables refer to one another, so their
// foreign keys are checked at the commit, where a dangling reference still
// fails it. What is derived is no longer marked, so a write that needs the
// derivation's results can derive early, and the end of the write does
// nothing more.
func (w *wtx) derive() error {
	if len(w.touched) == 0 && !w.regroup && !w.sessions && !w.files {
		return nil
	}
	if err := w.deferForeignKeys(); err != nil {
		return err
	}
	if w.files {
		if err := deriveFileFields(w); err != nil {
			return err
		}
	}
	var g *globals
	if w.regroup {
		if err := groupSecurities(w); err != nil {
			return err
		}
		w.regroup = false
		var err error
		if g, err = loadGlobals(w); err != nil {
			return err
		}
		if err := touchMovedSecurities(w, g); err != nil {
			return err
		}
		// What counts each file's rows once reads the securities, so every
		// account with files is derived again.
		ids, err := fileAccounts(w)
		if err != nil {
			return err
		}
		for _, id := range ids {
			w.touch(id)
		}
	}
	if w.sessions {
		if err := deriveSessions(w); err != nil {
			return err
		}
		w.sessions = false
	}
	if len(w.touched) == 0 {
		return nil
	}
	if g == nil {
		var err error
		if g, err = loadGlobals(w); err != nil {
			return err
		}
	}
	outs := map[int64]*accountOutputs{}
	var pairs map[entryKey]pairResult
	for {
		for _, id := range slices.Sorted(maps.Keys(w.touched)) {
			if outs[id] != nil {
				continue
			}
			in, err := loadAccount(w, id, g)
			if err != nil {
				return err
			}
			out := deriveAccount(in)
			outs[id] = &out
		}
		cands, stored, err := loadPairCandidates(w, g, outs)
		if err != nil {
			return err
		}
		for _, out := range outs {
			for _, e := range out.entries {
				if e.pairDecided || e.family == "journal" || e.family == "shares" {
					cands = append(cands, candidateOf(e, g.numbers[e.account]))
				}
			}
		}
		pairs = pairAccounts(cands, g.pairDecisions)
		// An account not derived whose pairs this changes is derived too.
		more := false
		for k, was := range stored {
			if r := pairs[k]; r.kind != was.kind || r.with != was.with || r.flag != was.flag {
				w.touch(k.account)
				more = true
			}
		}
		if !more {
			break
		}
	}
	wr := written{ids: map[entryKey]int64{}, pairs: map[entryKey]storedPair{}}
	for _, id := range slices.Sorted(maps.Keys(outs)) {
		if err := pruneFiles(w, id, *outs[id]); err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(maps.Keys(outs)) {
		finishPairs(outs[id].entries, pairs)
		if err := writeAccount(w, id, *outs[id], wr); err != nil {
			return err
		}
		delete(w.touched, id)
	}
	for _, id := range slices.Sorted(maps.Keys(outs)) {
		if err := writePairs(w, outs[id].entries, wr); err != nil {
			return err
		}
	}
	return writeFlagDefs(w)
}

// loadAccount reads what deriveAccount needs of account id; g holds what
// every account's derivation reads.
func loadAccount(w *wtx, id int64, g *globals) (accountInputs, error) {
	in := accountInputs{
		account: id, margin: w.s.o.margin, number: g.numbers[id],
		pairDecided: g.pairDecided, keySecurities: g.keySecurities, names: g.names,
	}
	if err := loadFiles(w, &in, g); err != nil {
		return in, err
	}
	byID := map[int64]int{} // txn_id -> index in in.txns
	err := queryEach(w, func(rows *sql.Rows) error {
		var t txnInput
		var latest, activity sql.NullInt64
		var at, trade, day, typ, status, text, problems, sent sql.NullString
		if err := rows.Scan(&t.id, &t.firstRead, &latest, &at, &trade, &day, &activity, &t.idSource, &typ, &status,
			&t.description, &text, &t.net, &problems, &sent); err != nil {
			return err
		}
		t.latestListRead, t.tradeDay, t.activityID = latest.Int64, nytime.Date(day.String), activity.Int64
		t.typ, t.status, t.text, t.numberSent = typ.String, status.String, unmasked(t.description, text), sent.String
		if err := json.Unmarshal([]byte(cmp.Or(problems.String, "[]")), &t.problems); err != nil {
			return err
		}
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
	}, `SELECT t.txn_id, t.first_read_id, t.latest_list_read_id, x.time, x.trade_date, x.trade_day, t.activity_id,
		t.id_source, x.type, x.status, x.description, `+rawIfMasked+`,
		x.net_cents, x.problems, x.number_sent_masked
		FROM api_txn t JOIN txn x ON x.txn_id = t.txn_id WHERE t.account_id = ?`, id)
	if err != nil {
		return in, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var txn int64
		var digest []byte
		var changed sql.NullString
		if err := rows.Scan(&txn, &digest, &changed); err != nil {
			return err
		}
		var paths []string
		if changed.Valid {
			if err := json.Unmarshal([]byte(changed.String), &paths); err != nil {
				return err
			}
		}
		t := &in.txns[byID[txn]]
		t.digests = append(t.digests, [32]byte(digest))
		t.changed = append(t.changed, paths)
		return nil
	}, `SELECT v.txn_id, c.digest, c.changed FROM api_revision v JOIN api_txn t ON t.txn_id = v.txn_id
		JOIN revision_cut c ON c.revision_id = v.revision_id WHERE t.account_id = ? ORDER BY v.txn_id, v.seq`, id)
	if err != nil {
		return in, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var txn int64
		var l legInput
		var asset, symbol, fee, effect sql.NullString
		if err := rows.Scan(&txn, &asset, &symbol, &l.amountMicros, &l.amountCents, &l.costCents, &l.price, &fee,
			&effect, &l.security); err != nil {
			return err
		}
		l.assetType, l.symbol, l.feeType, l.positionEffect = asset.String, symbol.String, fee.String, effect.String
		t := &in.txns[byID[txn]]
		t.legs = append(t.legs, l)
		return nil
	}, `SELECT l.txn_id, l.asset_type, l.symbol, l.amount_micros, l.amount_cents, l.cost_cents, l.price, l.fee_type,
		l.position_effect, l.security_id FROM api_leg l JOIN api_txn t ON t.txn_id = l.txn_id
		WHERE t.account_id = ? ORDER BY l.txn_id, l.leg`, id)
	if err != nil {
		return in, err
	}
	dups := map[int64]bool{}
	err = queryEach(w, func(rows *sql.Rows) error {
		var a int64
		err := rows.Scan(&a)
		dups[a] = true
		return err
	}, `SELECT DISTINCT j.value FROM api_read r, json_each(r.duplicates) j
		WHERE r.account_id = ? AND r.duplicates <> '[]' AND j.type = 'integer'`, id)
	if err != nil {
		return in, err
	}
	for i := range in.txns {
		in.txns[i].duplicate = in.txns[i].activityID != 0 && dups[in.txns[i].activityID]
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
	if err != nil {
		return in, err
	}
	acct := strconv.FormatInt(id, 10)
	if in.decisions, err = loadDecisions(w, `(kind IN ('category', 'count', 'pair', 'entry_security')
		AND subject >= ? AND subject < ?) OR (kind = 'account' AND subject = ?)`, "a:"+acct+":", "a:"+acct+";", "acct:"+acct); err != nil {
		return in, err
	}
	return in, loadBases(w, &in)
}

// loadBases reads, for each transaction of in that your decisions name
// with the revision they were made against, what the rules read of it now
// and of those revisions, from their raw JSON.
func loadBases(w *wtx, in *accountInputs) error {
	refs := txnRefs(in.account, in.txns)
	bySubject := map[string]int{}
	for i, t := range in.txns {
		bySubject[refs[t.id].subject] = i
	}
	for _, d := range latestDecisions(in.decisions) {
		i, ok := bySubject[d.subject]
		if !ok || !d.basisValid {
			continue
		}
		t := &in.txns[i]
		if t.bases == nil {
			var raw string
			if err := w.tx.QueryRowContext(w.ctx, "SELECT latest_raw FROM api_txn WHERE txn_id = ?", t.id).Scan(&raw); err != nil {
				return err
			}
			t.current, t.bases = recordClass(decodeTxn(raw)), map[int]apiClass{}
		}
		if _, ok := t.bases[d.basisSeq]; ok {
			continue
		}
		var raw string
		switch err := w.tx.QueryRowContext(w.ctx, "SELECT raw FROM api_revision WHERE txn_id = ? AND seq = ?", t.id, d.basisSeq).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
			continue // a revision that does not exist is no basis: the decision is stale
		case err != nil:
			return err
		}
		t.bases[d.basisSeq] = recordClass(decodeTxn(raw))
	}
	return nil
}

// writeAccount writes what deriveAccount gave account id, changing only
// the rows that differ from what the store holds, so that deriving an
// account whose evidence did not change writes nothing. Its entries' pairs
// are left for writePairs, with what wr gains.
func writeAccount(w *wtx, id int64, out accountOutputs, wr written) error {
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
	if err := writeConflicts(w, id, out.conflicts); err != nil {
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
	if err != nil {
		return err
	}
	if !slices.Equal(have, out.coverage) {
		if _, err := w.exec("DELETE FROM coverage WHERE account_id = ?", id); err != nil {
			return err
		}
		for _, c := range out.coverage {
			if _, err := w.exec("INSERT INTO coverage (account_id, source, from_date, to_date) VALUES (?, ?, ?, ?)",
				id, c.source, string(c.from), string(c.to)); err != nil {
				return err
			}
		}
	}
	if _, err := w.exec("UPDATE account SET kind = ?1 WHERE account_id = ?2 AND kind IS NOT ?1", out.kind, id); err != nil {
		return err
	}
	if err := writeFiles(w, id, &out); err != nil {
		return err
	}
	return writeEntries(w, id, out.entries, wr)
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
	w.flagDefs = false
	if _, err := w.exec("UPDATE account SET account_type = NULL, kind = NULL"); err != nil {
		return err
	}
	if err := rebuildTxns(w); err != nil {
		return err
	}
	if err := rebuildSnapshots(w); err != nil {
		return err
	}
	if err := rebuildFiles(w); err != nil {
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
