package history

import (
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// globals are what the derivation of every account reads beside the
// account's own evidence.
type globals struct {
	// names are the symbols that securities have held, by which a
	// dividend's symbol finds its security.
	names []symbolName
	// keySecurities are the securities of the keys that your entry_security
	// decisions name, without their k:.
	keySecurities map[string]int64
	// pairDecisions are your pair decisions, and pairDecided the subjects
	// of the entries that the latest of them name.
	pairDecisions []decisionInput
	pairDecided   map[string]bool
	// numbers are the accounts' numbers, by id.
	numbers map[int64]string
}

// loadGlobals reads what every account's derivation reads.
func loadGlobals(w *wtx) (*globals, error) {
	g := &globals{keySecurities: map[string]int64{}, pairDecided: map[string]bool{}, numbers: map[int64]string{}}
	var err error
	if g.names, err = loadSymbolNames(w); err != nil {
		return nil, err
	}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var key string
		var id int64
		err := rows.Scan(&key, &id)
		g.keySecurities[key] = id
		return err
	}, `SELECT DISTINCT k.key, k.security_id FROM decision d JOIN instrument_key k ON 'k:' || k.key = d.value
		WHERE d.kind = 'entry_security'`); err != nil {
		return nil, err
	}
	if g.pairDecisions, err = loadDecisions(w, "kind = 'pair'"); err != nil {
		return nil, err
	}
	for _, d := range latestDecisions(g.pairDecisions) {
		g.pairDecided[d.subject] = true
		if d.value != "none" {
			g.pairDecided[d.value] = true
		}
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var number string
		err := rows.Scan(&id, &number)
		g.numbers[id] = number
		return err
	}, "SELECT account_id, number FROM account")
	return g, err
}

// loadDecisions reads the decisions that where selects, in the order they
// were made.
func loadDecisions(w *wtx, where string, args ...any) ([]decisionInput, error) {
	var ds []decisionInput
	err := queryEach(w, func(rows *sql.Rows) error {
		var d decisionInput
		var basis sql.NullString
		if err := rows.Scan(&d.id, &d.kind, &d.subject, &d.value, &basis); err != nil {
			return err
		}
		if basis.Valid {
			var b struct {
				Seq *int `json:"seq"`
			}
			if err := json.Unmarshal([]byte(basis.String), &b); err != nil {
				return err
			}
			if b.Seq != nil {
				d.basisSeq, d.basisValid = *b.Seq, true
			}
		}
		ds = append(ds, d)
		return nil
	}, "SELECT decision_id, kind, subject, value, basis FROM decision WHERE "+where+" ORDER BY decision_id", args...)
	return ds, err
}

// candidateOf returns e as pairing across accounts reads it; number is its
// account's number.
func candidateOf(e ledgerEntry, number string) pairCandidate {
	return pairCandidate{
		key: e.key(), subject: e.subject, family: e.family, amount: e.amount, quantity: e.quantity,
		security: e.security, day: e.day.String, text: e.text, number: number, userCategory: e.userCategory,
	}
}

// A storedPair is an entry's pair as the store holds it, and the journal
// flag that pairing gave it.
type storedPair struct {
	kind string
	with entryKey
	flag string
}

// loadPairCandidates reads the entries of the accounts not in outs that
// pairing across accounts reads (journals, entries paired as journals, and
// entries that your pair decisions name), with their pairs as the store
// holds them.
func loadPairCandidates(w *wtx, g *globals, outs map[int64]*accountOutputs) ([]pairCandidate, map[entryKey]storedPair, error) {
	decided, err := json.Marshal(slices.Sorted(maps.Keys(g.pairDecided)))
	if err != nil {
		return nil, nil, err
	}
	var cands []pairCandidate
	stored := map[entryKey]storedPair{}
	err = queryEach(w, func(rows *sql.Rows) error {
		var c pairCandidate
		var day, description, text, kind, withRef sql.NullString
		var withAccount sql.NullInt64
		var unpaired, ambiguous bool
		if err := rows.Scan(&c.key.account, &c.key.ref, &c.subject, &c.family, &c.amount, &c.quantity, &c.security, &day,
			&c.userCategory, &description, &text, &kind, &withAccount, &withRef, &unpaired, &ambiguous); err != nil {
			return err
		}
		if outs[c.key.account] != nil {
			return nil
		}
		c.day, c.text, c.number = day.String, unmasked(description, text), g.numbers[c.key.account]
		cands = append(cands, c)
		p := storedPair{kind: kind.String}
		if withRef.Valid {
			p.with = entryKey{withAccount.Int64, withRef.String}
		}
		switch {
		case unpaired:
			p.flag = "unpaired_journal"
		case ambiguous:
			p.flag = "ambiguous_journal"
		}
		stored[c.key] = p
		return nil
	}, `SELECT e.account_id, e.ref, e.subject, e.family, e.amount_cents, e.quantity_micros, e.security_id, e.day,
		e.category_rule = 'user', x.description, `+rawIfMasked+`,
		e.pair_kind, p.account_id, p.ref,
		EXISTS (SELECT 1 FROM flag f WHERE f.entry_id = e.entry_id AND f.code = 'unpaired_journal'),
		EXISTS (SELECT 1 FROM flag f WHERE f.entry_id = e.entry_id AND f.code = 'ambiguous_journal')
		FROM entry e LEFT JOIN entry p ON p.entry_id = e.pair_entry_id LEFT JOIN api_txn t ON t.txn_id = e.txn_id
		LEFT JOIN txn x ON x.txn_id = e.txn_id
		WHERE e.family IN ('journal', 'shares') OR e.pair_kind = 'journal'
		OR e.subject IN (SELECT value FROM json_each(?))`, string(decided))
	return cands, stored, err
}

// A storedEntry is an entry as the store holds it.
type storedEntry struct {
	id  int64
	row entryRow
	// pairKind and pair are its pair, and flags its flags' details by code
	// with their row ids.
	pairKind string
	pair     entryKey
	flags    map[string]storedFlag
}

// A storedFlag is a flag's row id and detail.
type storedFlag struct {
	id     int64
	detail sql.NullString
}

// entryColumns are entry's columns in the order of entryRow's fields.
const entryColumns = `txn_id, csv_txn_id, ref, subject, kind, description, day, posted_date, time, amount_cents,
	fees_cents, gross_cents, quantity_micros, quantity_as_of, price, symbol, security_id, family, category,
	category_rule, counted, counted_why`

// fields returns pointers to r's fields in the order of entryColumns.
func (r *entryRow) fields() []any {
	return []any{
		&r.txn, &r.csvTxn, &r.ref, &r.subject, &r.kind, &r.description, &r.day, &r.posted, &r.time, &r.amount,
		&r.fees, &r.gross, &r.quantity, &r.quantityAsOf, &r.price, &r.symbol, &r.security, &r.family, &r.category,
		&r.rule, &r.counted, &r.countedWhy,
	}
}

// values returns r's fields in the order of entryColumns.
func (r entryRow) values() []any {
	v := r.fields()
	for i, p := range v {
		switch p := p.(type) {
		case *sql.NullInt64:
			v[i] = *p
		case *sql.NullString:
			v[i] = *p
		case *sql.NullFloat64:
			v[i] = *p
		case *string:
			v[i] = *p
		case *bool:
			v[i] = *p
		}
	}
	return v
}

// loadEntries reads the entries of account id as the store holds them, by
// ref.
func loadEntries(w *wtx, id int64) (map[string]*storedEntry, error) {
	have := map[string]*storedEntry{}
	byID := map[int64]*storedEntry{}
	err := queryEach(w, func(rows *sql.Rows) error {
		e := &storedEntry{flags: map[string]storedFlag{}}
		var kind, withRef sql.NullString
		var withAccount sql.NullInt64
		if err := rows.Scan(append([]any{&e.id}, append(e.row.fields(), &kind, &withAccount, &withRef)...)...); err != nil {
			return err
		}
		e.pairKind = kind.String
		if withRef.Valid {
			e.pair = entryKey{withAccount.Int64, withRef.String}
		}
		have[e.row.ref], byID[e.id] = e, e
		return nil
	}, `SELECT e.entry_id, `+prefixColumns("e.", entryColumns)+`, e.pair_kind, p.account_id, p.ref
		FROM entry e LEFT JOIN entry p ON p.entry_id = e.pair_entry_id WHERE e.account_id = ?`, id)
	if err != nil {
		return nil, err
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var f storedFlag
		var entry int64
		var code string
		if err := rows.Scan(&f.id, &entry, &code, &f.detail); err != nil {
			return err
		}
		byID[entry].flags[code] = f
		return nil
	}, `SELECT f.flag_id, f.entry_id, f.code, f.detail FROM flag f JOIN entry e ON e.entry_id = f.entry_id
		WHERE e.account_id = ?`, id)
	return have, err
}

// prefixColumns puts prefix before each of a list of columns.
func prefixColumns(prefix, columns string) string {
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = prefix + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}

// written is what writeEntries leaves for writePairs: the row id of each
// entry it wrote, and the pair that the store held for it.
type written struct {
	ids   map[entryKey]int64
	pairs map[entryKey]storedPair
}

// writeEntries brings account id's entries and their flags to entries,
// writing only the rows that differ: an entry keeps its row id while its
// ref stays, so that a pair that names it stays good. Pairs are written
// after every account's entries, by writePairs, from what wr gains.
func writeEntries(w *wtx, id int64, entries []ledgerEntry, wr written) error {
	have, err := loadEntries(w, id)
	if err != nil {
		return err
	}
	want := map[string]*ledgerEntry{}
	for i := range entries {
		want[entries[i].ref] = &entries[i]
	}
	for _, ref := range slices.Sorted(maps.Keys(have)) {
		if want[ref] != nil {
			continue
		}
		if _, err := w.exec("DELETE FROM flag WHERE entry_id = ?", have[ref].id); err != nil {
			return err
		}
		if _, err := w.exec("DELETE FROM entry WHERE entry_id = ?", have[ref].id); err != nil {
			return err
		}
	}
	// An entry whose transaction is now another's (two transactions without
	// an activity ID that changed places) gives it up first, so that no
	// transaction has two entries at any statement; foreign keys wait for
	// the commit.
	for _, e := range entries {
		if h := have[e.ref]; h != nil && (h.row.txn != e.txn || h.row.csvTxn != e.csvTxn) {
			if _, err := w.exec("UPDATE entry SET txn_id = -entry_id, csv_txn_id = NULL WHERE entry_id = ?", h.id); err != nil {
				return err
			}
		}
	}
	set := strings.ReplaceAll(entryColumns, ",", " = ?,") + " = ?"
	for i := range entries {
		e := &entries[i]
		h := have[e.ref]
		switch {
		case h == nil:
			var eid int64
			if err := w.tx.QueryRowContext(w.ctx, "INSERT INTO entry (account_id, "+entryColumns+") VALUES (?"+
				strings.Repeat(", ?", len(e.values()))+") RETURNING entry_id", append([]any{id}, e.values()...)...).Scan(&eid); err != nil {
				return err
			}
			h = &storedEntry{id: eid, flags: map[string]storedFlag{}}
			have[e.ref] = h
		case h.row != e.entryRow:
			if _, err := w.exec("UPDATE entry SET "+set+" WHERE entry_id = ?", append(e.values(), h.id)...); err != nil {
				return err
			}
		}
		wr.ids[e.key()] = h.id
		wr.pairs[e.key()] = storedPair{kind: h.pairKind, with: h.pair}
		if err := writeFlags(w, "entry_id", h.id, h.flags, e.flags); err != nil {
			return err
		}
	}
	return nil
}

// writeFlags brings the flags of what id names in column (entry_id or
// snapshot_id) to flags, from have, the flags the store holds of it by code;
// only what differs is written.
func writeFlags(w *wtx, column string, id int64, have map[string]storedFlag, flags flagSet) error {
	want := map[string]sql.NullString{}
	for _, f := range flags {
		want[f.code] = sql.NullString{String: f.detail, Valid: f.detail != ""}
	}
	for _, code := range slices.Sorted(maps.Keys(have)) {
		if _, ok := want[code]; !ok {
			if _, err := w.exec("DELETE FROM flag WHERE flag_id = ?", have[code].id); err != nil {
				return err
			}
		}
	}
	for _, f := range flags.sorted() {
		detail := want[f.code]
		switch had, ok := have[f.code]; {
		case !ok:
			if _, err := w.exec("INSERT INTO flag ("+column+", code, detail) VALUES (?, ?, ?)", id, f.code, detail); err != nil {
				return err
			}
		case had.detail != detail:
			if _, err := w.exec("UPDATE flag SET detail = ? WHERE flag_id = ?", detail, had.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// writePairs writes the pairs of entries, an account's, where they differ
// from what the store held, from what writeEntries left in wr.
func writePairs(w *wtx, entries []ledgerEntry, wr written) error {
	for _, e := range entries {
		if had := wr.pairs[e.key()]; had.kind == e.pairKind && had.with == e.pair {
			continue
		}
		var pairKind sql.NullString
		var pairID sql.NullInt64
		if e.pairKind != "" {
			pairKind = sql.NullString{String: e.pairKind, Valid: true}
			id, ok := wr.ids[e.pair]
			if !ok {
				if err := w.tx.QueryRowContext(w.ctx, "SELECT entry_id FROM entry WHERE account_id = ? AND ref = ?",
					e.pair.account, e.pair.ref).Scan(&id); err != nil {
					return err
				}
			}
			pairID = sql.NullInt64{Int64: id, Valid: true}
		}
		if _, err := w.exec("UPDATE entry SET pair_kind = ?, pair_entry_id = ? WHERE entry_id = ?", pairKind, pairID, wr.ids[e.key()]); err != nil {
			return err
		}
	}
	return nil
}

// touchMovedSecurities marks for derivation the accounts not yet marked
// that hold an API entry whose security, found by your entry_security
// decision or by the symbol after its description's tilde, is now
// another, or found in another way: the grouping of keys into securities,
// or the names it read, changed. An entry whose security is its one
// security leg's moves with the leg, which pointLegs sees.
func touchMovedSecurities(w *wtx, g *globals) error {
	type row struct {
		account               int64
		subject, day, sym     sql.NullString
		description, text     sql.NullString
		security, legSecurity sql.NullInt64
		legs                  int
		byName, unresolved    sql.NullString
	}
	var rows []row
	err := queryEach(w, func(r *sql.Rows) error {
		var x row
		err := r.Scan(&x.account, &x.subject, &x.day, &x.security, &x.legs, &x.legSecurity, &x.sym, &x.description, &x.text,
			&x.byName, &x.unresolved)
		rows = append(rows, x)
		return err
	}, `WITH sec AS (SELECT txn_id, count(*) AS n, max(security_id) AS s, max(symbol) AS sym FROM api_leg
		WHERE asset_type IS NOT 'CURRENCY' GROUP BY txn_id)
		SELECT e.account_id, e.subject, e.day, e.security_id, coalesce(sec.n, 0), sec.s, sec.sym, x.description,
		`+rawIfMasked+`,
		(SELECT coalesce(f.detail, '') FROM flag f WHERE f.entry_id = e.entry_id AND f.code = 'security_by_name'),
		(SELECT coalesce(f.detail, '') FROM flag f WHERE f.entry_id = e.entry_id AND f.code = 'unresolved_security')
		FROM entry e JOIN api_txn t ON t.txn_id = e.txn_id JOIN txn x ON x.txn_id = e.txn_id LEFT JOIN sec ON sec.txn_id = e.txn_id
		WHERE NOT (coalesce(sec.n, 0) = 1 AND sec.s IS NOT NULL)
		OR e.subject IN (SELECT subject FROM decision WHERE kind = 'entry_security')`)
	if err != nil {
		return err
	}
	decided := map[string]sql.NullInt64{}
	ds, err := loadDecisions(w, "kind = 'entry_security'")
	if err != nil {
		return err
	}
	for _, d := range latestDecisions(ds) {
		if id, ok := g.keySecurities[strings.TrimPrefix(d.value, "k:")]; ok {
			decided[d.subject] = sql.NullInt64{Int64: id, Valid: true}
		}
	}
	traded := map[int64][]tradedSecurity{} // by account
	if err := queryEach(w, func(r *sql.Rows) error {
		var acct int64
		var t tradedSecurity
		err := r.Scan(&acct, &t.day, &t.security)
		traded[acct] = append(traded[acct], t)
		return err
	}, `SELECT x.account_id, x.trade_day, l.security_id FROM api_leg l JOIN txn x ON x.txn_id = l.txn_id
		WHERE l.asset_type IS NOT 'CURRENCY' AND l.security_id IS NOT NULL AND x.trade_day IS NOT NULL`); err != nil {
		return err
	}
	for _, x := range rows {
		if w.touched[x.account] {
			continue
		}
		var legs []legInput
		if x.legs == 1 {
			legs = []legInput{{assetType: "EQUITY", symbol: x.sym.String, security: x.legSecurity}}
		}
		sec, how := entrySecurity(decided[x.subject.String], legs, unmasked(x.description, x.text), nytime.Date(x.day.String), g.names, traded[x.account])
		var byName, unresolved sql.NullString
		switch how.code {
		case "security_by_name":
			byName = sql.NullString{String: how.detail, Valid: true}
		case "unresolved_security":
			unresolved = sql.NullString{String: how.detail, Valid: true}
		}
		if sec != x.security || byName != x.byName || unresolved != x.unresolved {
			w.touch(x.account)
		}
	}
	return nil
}
