package history

import (
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// loadSecurities reads what the counting of files' rows reads of the
// store's securities (securities): each key's security and whether it is
// definite, when each security's names were seen, and the pairs of
// securities that your differ decisions keep apart.
func loadSecurities(w *wtx) (*securities, error) {
	s := &securities{keys: map[string]secInfo{}, periods: map[int64][][2]time.Time{}, differ: map[[2]int64]bool{}}
	err := queryEach(w, func(rows *sql.Rows) error {
		var key string
		var i secInfo
		err := rows.Scan(&key, &i.id, &i.definite)
		s.keys[key] = i
		return err
	}, "SELECT k.key, k.security_id, s.definite FROM instrument_key k JOIN security s ON s.security_id = k.security_id")
	if err != nil {
		return nil, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var first, last string
		if err := rows.Scan(&id, &first, &last); err != nil {
			return err
		}
		f, err := parseInstant(first)
		if err != nil {
			return err
		}
		l, err := parseInstant(last)
		s.periods[id] = append(s.periods[id], [2]time.Time{f, l})
		return err
	}, `SELECT k.security_id, n.first_seen_at, n.last_seen_at FROM key_name n JOIN instrument_key k ON k.key = n.key
		ORDER BY k.security_id, n.first_seen_at, n.last_seen_at`)
	if err != nil {
		return nil, err
	}
	links, err := loadKeyLinks(w)
	for _, l := range links {
		a, b := s.keys[l.a], s.keys[l.b]
		if !l.same && a.id != 0 && b.id != 0 && a.id != b.id {
			s.differ[[2]int64{min(a.id, b.id), max(a.id, b.id)}] = true
		}
	}
	return s, err
}

// loadFiles reads what the derivation of account in.account reads of its
// files: each History and Realized export that is the account's now, set
// aside or not, with its rows or lots, the flags its content gave, and
// your decisions on it and on its rows' transactions; and the spans of the
// account's rows in `-json` files.
func loadFiles(w *wtx, in *accountInputs, g *globals) error {
	byID := map[int64]*fileInput{}
	err := queryEach(w, func(rows *sql.Rows) error {
		f := &fileInput{}
		var exported, imported string
		var statedFrom, statedTo, rowsFrom, rowsTo, userFrom, userTo, exportedAt sql.NullString
		var name string
		if err := rows.Scan(&f.id, &f.sha, &f.kind, &f.ref, &f.label, &f.forgotten, &exportedAt, &imported,
			&statedFrom, &statedTo, &rowsFrom, &rowsTo, &userFrom, &userTo, &name); err != nil {
			return err
		}
		exported = imported
		if exportedAt.Valid {
			exported = exportedAt.String
		}
		var err error
		if f.exportedAt, err = parseInstant(exported); err != nil {
			return err
		}
		f.span = schwabfile.Span{
			StatedFrom: nytime.Date(statedFrom.String), StatedTo: nytime.Date(statedTo.String),
			Oldest: nytime.Date(rowsFrom.String), Newest: nytime.Date(rowsTo.String),
		}
		f.from, f.to = nytime.Date(userFrom.String), nytime.Date(userTo.String)
		f.nameDigits = schwabfile.NameDigits(name)
		in.files = append(in.files, f)
		byID[f.id] = f
		return nil
	}, `SELECT f.file_id, lower(hex(f.sha256)), f.kind, d.ref, d.label, d.forgotten, d.exported_at, f.imported_at,
		d.stated_from, d.stated_to, d.rows_from, d.rows_to, f.user_from, f.user_to, f.name
		FROM import_file f JOIN file_fields d ON d.file_id = f.file_id
		WHERE d.account_id = ? AND f.kind <> 'api_json' ORDER BY f.file_id`, in.account)
	if err != nil {
		return err
	}
	if err := loadAPIFiles(w, in); err != nil || len(in.files) == 0 {
		return err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var code string
		var detail sql.NullString
		if err := rows.Scan(&id, &code, &detail); err != nil {
			return err
		}
		f := byID[id]
		f.flags.add(code, detail.String)
		if slices.Contains(coverageCodes, code) {
			f.span.Warnings = append(f.span.Warnings, code)
		}
		return nil
	}, `SELECT f.file_id, f.code, f.detail FROM flag f JOIN file_fields d ON d.file_id = f.file_id
		WHERE d.account_id = ? AND f.code IN (SELECT value FROM json_each(?)) ORDER BY f.file_id, f.code`, in.account, jsonList(fileFlagCodes))
	if err != nil {
		return err
	}
	for _, f := range in.files {
		latest := map[string]string{}
		err := queryEach(w, func(rows *sql.Rows) error {
			var kind, value string
			err := rows.Scan(&kind, &value)
			latest[kind] = value
			return err
		}, "SELECT kind, value FROM decision WHERE kind IN ('completeness', 'file_range') AND subject = ? ORDER BY decision_id", "f:"+f.sha)
		if err != nil {
			return err
		}
		if v := latest["completeness"]; v != "auto" {
			f.completeness = v
		}
		if from, to, ok := strings.Cut(latest["file_range"], ".."); ok {
			f.from, f.to = nytime.Date(from), nytime.Date(to)
		}
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		r := &historyRowInput{}
		var file int64
		var posted, asOf, item, problems sql.NullString
		var coarse []byte
		if err := rows.Scan(&r.id, &file, &r.ordinal, &posted, &asOf, &r.action, &r.symbol, &r.description, &r.quantity,
			&r.quantityText, &r.price, &r.priceText, &r.fees, &r.amount, &item, &coarse, &problems); err != nil {
			return err
		}
		r.file = byID[file]
		r.posted, r.asOf = nytime.Date(posted.String), nytime.Date(asOf.String)
		copy(r.coarse[:], coarse)
		if err := json.Unmarshal([]byte(problems.String), &r.problems); err != nil {
			return err
		}
		r.feesBlank = !r.fees.Valid && !noted(r.problems, "Fees & Comm")
		r.amountBlank = !r.amount.Valid && !noted(r.problems, "Amount")
		r.xKey, r.fileKey = itemKey(item.String), fileKey(r.symbol, r.file.exported())
		r.file.rows = append(r.file.rows, r)
		return nil
	}, `SELECT r.row_id, r.file_id, r.ordinal, r.posted_date, r.asof_date, r.action, r.symbol, r.description,
		r.quantity_micros, r.quantity_text, r.price, r.price_text, r.fees_cents, r.amount_cents, r.item_issue_id,
		r.coarse, r.problems FROM history_row r JOIN file_fields d ON d.file_id = r.file_id
		WHERE d.account_id = ? ORDER BY r.file_id, r.ordinal`, in.account)
	if err != nil {
		return err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		l := &lotRowInput{}
		var file int64
		var closed, name, problems sql.NullString
		var coarse, fine []byte
		if err := rows.Scan(&l.id, &file, &l.ordinal, &closed, &l.symbol, &name, &l.quantity, &l.proceeds, &coarse, &fine,
			&problems); err != nil {
			return err
		}
		l.file, l.closed, l.name = byID[file], nytime.Date(closed.String), name.String
		copy(l.coarse[:], coarse)
		copy(l.fine[:], fine)
		if err := json.Unmarshal([]byte(problems.String), &l.problems); err != nil {
			return err
		}
		l.fileKey = fileKey(l.symbol, l.file.exported())
		l.file.lots = append(l.file.lots, l)
		return nil
	}, `SELECT r.row_id, r.file_id, r.ordinal, r.closed_date, r.symbol, r.name, r.quantity_text, r.proceeds_cents,
		r.coarse, r.fine, r.problems FROM realized_row r JOIN file_fields d ON d.file_id = r.file_id
		WHERE d.account_id = ? ORDER BY r.file_id, r.ordinal`, in.account)
	if err != nil {
		return err
	}
	for _, f := range in.files {
		ds, err := loadDecisions(w, "kind IN ('category', 'count', 'pair', 'entry_security') AND subject >= ? AND subject < ?",
			"c:"+f.sha+":", "c:"+f.sha+";")
		if err != nil {
			return err
		}
		in.fileDecisions = append(in.fileDecisions, ds...)
	}
	in.secs, err = g.fileSecurities(w)
	return err
}

// loadAPIFiles reads the spans of the account's rows in `-json` files, and
// whether your completeness decision said each file was filtered.
func loadAPIFiles(w *wtx, in *accountInputs) error {
	return queryEach(w, func(rows *sql.Rows) error {
		var s apiFileSpan
		var start, end sql.NullString
		if err := rows.Scan(&start, &end, &s.filtered); err != nil {
			return err
		}
		if start.Valid && end.Valid {
			from, err := parseInstant(start.String)
			if err != nil {
				return err
			}
			to, err := parseInstant(end.String)
			if err != nil {
				return err
			}
			s.oldest, s.newest = nytime.DateOf(from), nytime.DateOf(to)
		}
		in.apiFiles = append(in.apiFiles, s)
		return nil
	}, `SELECT r.window_start, r.window_end,
		coalesce((SELECT d.value = 'filtered' FROM decision d WHERE d.kind = 'completeness'
			AND d.subject = 'f:' || lower(hex(f.sha256)) ORDER BY d.decision_id DESC LIMIT 1), 0)
		FROM api_read r JOIN import_file f ON f.file_id = r.file_id WHERE r.account_id = ? AND r.endpoint = 'file'
		ORDER BY r.read_id`, in.account)
}

// coverageCodes are the codes of a file's warnings that narrow what it
// covers (schwabfile.Span.Coverage).
var coverageCodes = []string{schwabfile.WarnEmpty, schwabfile.WarnAtCap, schwabfile.WarnLateStart, schwabfile.WarnTwoYears}

// noted reports whether a row's notes say why its column path is empty.
func noted(ps []rowProblem, path string) bool {
	return slices.ContainsFunc(ps, func(p rowProblem) bool { return p.Path == path && p.Flag != "" })
}

// jsonList returns a list of strings as a JSON array, for json_each.
func jsonList(ss []string) string {
	b, _ := json.Marshal(ss) // strings always marshal
	return string(b)
}

// pruneFiles deletes the file transactions and lots of account id, and
// their members and entries, that out no longer holds, before any
// account's are written: a file moved between two accounts takes its transactions'
// subjects and its rows with it, which must leave the one before they
// join the other.
func pruneFiles(w *wtx, id int64, out accountOutputs) error {
	txns, lots := map[string]bool{}, map[string]bool{}
	rows := map[int64]bool{}
	for _, t := range out.fileTxns {
		txns[t.subject] = true
		for _, r := range t.members {
			rows[r.id] = true
		}
	}
	lotRows := map[int64]bool{}
	for _, l := range out.lots {
		lots[l.subject] = true
		for _, r := range l.members {
			lotRows[r.id] = true
		}
	}
	for _, p := range []struct {
		table, key string
		want       map[string]bool
	}{
		{"csv_txn", "csv_txn_id", txns},
		{"realized_lot", "lot_id", lots},
	} {
		var gone []int64
		if err := queryEach(w, func(rows *sql.Rows) error {
			var rid int64
			var subject string
			err := rows.Scan(&rid, &subject)
			if !p.want[subject] {
				gone = append(gone, rid)
			}
			return err
		}, "SELECT "+p.key+", subject FROM "+p.table+" WHERE account_id = ?", id); err != nil {
			return err
		}
		for _, rid := range gone {
			if p.table == "realized_lot" {
				if _, err := w.exec("DELETE FROM flag WHERE lot_id = ?", rid); err != nil {
					return err
				}
			} else {
				// Its entry goes with it, so that a transaction that takes its
				// row id meanwhile finds no entry naming it.
				if _, err := w.exec("DELETE FROM flag WHERE entry_id IN (SELECT entry_id FROM entry WHERE csv_txn_id = ?)", rid); err != nil {
					return err
				}
				if _, err := w.exec("UPDATE entry SET pair_kind = NULL, pair_entry_id = NULL WHERE pair_entry_id IN (SELECT entry_id FROM entry WHERE csv_txn_id = ?)", rid); err != nil {
					return err
				}
				if _, err := w.exec("DELETE FROM entry WHERE csv_txn_id = ?", rid); err != nil {
					return err
				}
			}
			if _, err := w.exec("DELETE FROM "+p.table+" WHERE "+p.key+" = ?", rid); err != nil {
				return err
			}
		}
	}
	for _, p := range []struct {
		query string
		want  map[int64]bool
		table string
	}{
		{"SELECT m.row_id FROM csv_member m JOIN csv_txn c ON c.csv_txn_id = m.csv_txn_id WHERE c.account_id = ?", rows, "csv_member"},
		{"SELECT m.row_id FROM lot_member m JOIN realized_lot l ON l.lot_id = m.lot_id WHERE l.account_id = ?", lotRows, "lot_member"},
	} {
		var gone []int64
		if err := queryEach(w, func(rs *sql.Rows) error {
			var rid int64
			err := rs.Scan(&rid)
			if !p.want[rid] {
				gone = append(gone, rid)
			}
			return err
		}, p.query, id); err != nil {
			return err
		}
		for _, rid := range gone {
			if _, err := w.exec("DELETE FROM "+p.table+" WHERE row_id = ?", rid); err != nil {
				return err
			}
		}
	}
	// Members whose transaction pruning deleted go with it.
	if _, err := w.exec("DELETE FROM csv_member WHERE csv_txn_id NOT IN (SELECT csv_txn_id FROM csv_txn)"); err != nil {
		return err
	}
	_, err := w.exec("DELETE FROM lot_member WHERE lot_id NOT IN (SELECT lot_id FROM realized_lot)")
	return err
}

// writeFiles writes what deriveAccount gave account id's files, changing
// only the rows that differ: each file's coverage, completeness and
// flags; its file transactions and their members, whose row ids the
// entries of out then take; and its lots, their members and flags.
func writeFiles(w *wtx, id int64, out *accountOutputs) error {
	for _, f := range out.files {
		var have struct {
			from, to, source, completeness, by sql.NullString
			lacks                              sql.NullInt64
		}
		if err := w.tx.QueryRowContext(w.ctx, `SELECT covers_from, covers_to, coverage_source, completeness, complete_by,
			lacks_rows FROM file_fields WHERE file_id = ?`, f.id).Scan(&have.from, &have.to, &have.source, &have.completeness,
			&have.by, &have.lacks); err != nil {
			return err
		}
		want := []any{
			nullString(string(f.from)), nullString(string(f.to)), nullString(f.source), nullString(f.completeness),
			nullString(f.completeBy), f.lacks,
		}
		if have.from != want[0] || have.to != want[1] || have.source != want[2] || have.completeness != want[3] ||
			have.by != want[4] || have.lacks != f.lacks {
			if _, err := w.exec(`UPDATE file_fields SET covers_from = ?, covers_to = ?, coverage_source = ?, completeness = ?,
				complete_by = ?, lacks_rows = ? WHERE file_id = ?`, append(want, f.id)...); err != nil {
				return err
			}
		}
		flags, err := storedFlags(w, "file_id", f.id)
		if err != nil {
			return err
		}
		if err := writeFlags(w, "file_id", f.id, flags, f.flags); err != nil {
			return err
		}
	}
	type stored struct {
		id                        int64
		ref                       string
		coarse                    []byte
		shown                     int64
		asOf, posted, asof, event sql.NullString
		files                     int
	}
	have := map[string]stored{}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var s stored
		var subject string
		err := rows.Scan(&s.id, &subject, &s.ref, &s.coarse, &s.shown, &s.asOf, &s.posted, &s.asof, &s.event, &s.files)
		have[subject] = s
		return err
	}, `SELECT csv_txn_id, subject, ref, coarse, shown_row_id, quantity_as_of, posted_date, asof_date, event_date, files
		FROM csv_txn WHERE account_id = ?`, id); err != nil {
		return err
	}
	ids := map[string]int64{}
	for _, t := range out.fileTxns {
		vals := []any{
			t.ref, t.coarse[:], t.shown.id, nullString(string(t.quantityAsOf)), string(t.posted),
			nullString(string(t.asOf)), string(t.event), t.files,
		}
		h, ok := have[t.subject]
		switch {
		case !ok:
			if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO csv_txn (account_id, subject, ref, coarse, shown_row_id,
				quantity_as_of, posted_date, asof_date, event_date, files) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				RETURNING csv_txn_id`, append([]any{id, t.subject}, vals...)...).Scan(&h.id); err != nil {
				return err
			}
		case h.ref != t.ref || string(h.coarse) != string(t.coarse[:]) || h.shown != t.shown.id ||
			h.asOf != vals[3] || h.posted.String != string(t.posted) || h.asof != vals[5] || h.event.String != string(t.event) ||
			h.files != t.files:
			if _, err := w.exec(`UPDATE csv_txn SET ref = ?, coarse = ?, shown_row_id = ?, quantity_as_of = ?, posted_date = ?,
				asof_date = ?, event_date = ?, files = ? WHERE csv_txn_id = ?`, append(vals, h.id)...); err != nil {
				return err
			}
		}
		ids[t.subject] = h.id
	}
	members := map[int64]int64{}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var row, txn int64
		err := rows.Scan(&row, &txn)
		members[row] = txn
		return err
	}, "SELECT m.row_id, m.csv_txn_id FROM csv_member m JOIN csv_txn c ON c.csv_txn_id = m.csv_txn_id WHERE c.account_id = ?", id); err != nil {
		return err
	}
	for _, t := range out.fileTxns {
		for _, r := range t.members {
			if members[r.id] == ids[t.subject] {
				continue
			}
			if _, err := w.exec(`INSERT INTO csv_member (row_id, csv_txn_id) VALUES (?, ?)
				ON CONFLICT (row_id) DO UPDATE SET csv_txn_id = excluded.csv_txn_id`, r.id, ids[t.subject]); err != nil {
				return err
			}
		}
	}
	for i := range out.entries {
		if e := &out.entries[i]; e.csvSubject != "" {
			e.csvTxn = sql.NullInt64{Int64: ids[e.csvSubject], Valid: true}
		}
	}
	return writeLots(w, id, out.lots)
}

// writeLots writes account id's lots, their members and their flags,
// changing only the rows that differ.
func writeLots(w *wtx, id int64, lots []lotOutput) error {
	type stored struct {
		id              int64
		ref             string
		shown           int64
		versions, files int
		security        sql.NullInt64
	}
	have := map[string]stored{}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var s stored
		var subject string
		err := rows.Scan(&s.id, &subject, &s.ref, &s.shown, &s.versions, &s.files, &s.security)
		have[subject] = s
		return err
	}, "SELECT lot_id, subject, ref, shown_row_id, versions, files, security_id FROM realized_lot WHERE account_id = ?", id); err != nil {
		return err
	}
	members := map[int64]int64{}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var row, lot int64
		err := rows.Scan(&row, &lot)
		members[row] = lot
		return err
	}, "SELECT m.row_id, m.lot_id FROM lot_member m JOIN realized_lot l ON l.lot_id = m.lot_id WHERE l.account_id = ?", id); err != nil {
		return err
	}
	for _, l := range lots {
		h, ok := have[l.subject]
		vals := []any{l.ref, l.shown.id, l.versions, l.files, l.security}
		switch {
		case !ok:
			if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO realized_lot (account_id, subject, ref, shown_row_id, versions,
				files, security_id) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING lot_id`, append([]any{id, l.subject}, vals...)...).Scan(&h.id); err != nil {
				return err
			}
		case h.ref != l.ref || h.shown != l.shown.id || h.versions != l.versions || h.files != l.files || h.security != l.security:
			if _, err := w.exec(`UPDATE realized_lot SET ref = ?, shown_row_id = ?, versions = ?, files = ?, security_id = ?
				WHERE lot_id = ?`, append(vals, h.id)...); err != nil {
				return err
			}
		}
		for _, r := range l.members {
			if members[r.id] == h.id {
				continue
			}
			if _, err := w.exec(`INSERT INTO lot_member (row_id, lot_id) VALUES (?, ?)
				ON CONFLICT (row_id) DO UPDATE SET lot_id = excluded.lot_id`, r.id, h.id); err != nil {
				return err
			}
		}
		flags, err := storedFlags(w, "lot_id", h.id)
		if err != nil {
			return err
		}
		if err := writeFlags(w, "lot_id", h.id, flags, l.flags); err != nil {
			return err
		}
	}
	return nil
}

// storedFlags returns the flags that the store holds of what id names in
// column (file_id or lot_id), by code.
func storedFlags(w *wtx, column string, id int64) (map[string]storedFlag, error) {
	have := map[string]storedFlag{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var f storedFlag
		var code string
		err := rows.Scan(&f.id, &code, &f.detail)
		have[code] = f
		return err
	}, "SELECT flag_id, code, detail FROM flag WHERE "+column+" = ?", id)
	return have, err
}

// rebuildFiles derives again what each imported file gives by itself, from
// its bytes, in the order of their ids (layer 1).
func rebuildFiles(w *wtx) error {
	type stored struct {
		id       int64
		name     string
		imported string
	}
	var files []stored
	if err := queryEach(w, func(rows *sql.Rows) error {
		var f stored
		err := rows.Scan(&f.id, &f.name, &f.imported)
		files = append(files, f)
		return err
	}, "SELECT file_id, name, imported_at FROM import_file ORDER BY file_id"); err != nil {
		return err
	}
	for _, f := range files {
		var content []byte
		if err := w.tx.QueryRowContext(w.ctx, "SELECT content FROM import_file WHERE file_id = ?", f.id).Scan(&content); err != nil {
			return err
		}
		read, err := schwabfile.Read(f.name, content)
		if err != nil {
			return err
		}
		at, err := parseInstant(f.imported)
		if err != nil {
			return err
		}
		if err := deriveFile(w, f.id, f.name, read, at); err != nil {
			return err
		}
	}
	return nil
}

// fileAccounts returns the accounts that hold a History or Realized export
// not set aside, whose transactions and lots the grouping of securities
// bears on.
func fileAccounts(w *wtx) ([]int64, error) {
	var ids []int64
	err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		err := rows.Scan(&id)
		ids = append(ids, id)
		return err
	}, `SELECT DISTINCT d.account_id FROM file_fields d JOIN import_file f ON f.file_id = d.file_id
		WHERE f.kind <> 'api_json' AND d.forgotten = 0 AND d.account_id IS NOT NULL ORDER BY d.account_id`)
	return ids, err
}

// fileSecurities returns what the counting reads of the store's securities,
// read once in a derivation.
func (g *globals) fileSecurities(w *wtx) (*securities, error) {
	if g.secs != nil {
		return g.secs, nil
	}
	var err error
	g.secs, err = loadSecurities(w)
	return g.secs, err
}
