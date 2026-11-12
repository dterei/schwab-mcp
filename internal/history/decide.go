package history

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
)

// A Decision is one of yours, which the store adds to its log of them and
// never changes: the latest of a kind on a subject holds, and the value
// auto withdraws the ones before it. The derivation applies them after its
// rules (16 of the design).
type Decision struct {
	// Kind is what it decides:
	//
	//	category        an entry's category: one of the categories, or auto
	//	count           whether an entry counts: yes, no or auto
	//	pair            the entry an entry is paired with (its subject), none or auto
	//	entry_security  an entry's security: k:<key>, or auto
	//	security        that two keys name one security or two: same k:<key>,
	//	                differ k:<key>, or auto
	//	account         an account's kind: kind=<text>, or auto
	//	completeness    whether a file holds every type and symbol: complete,
	//	                filtered or auto; on `-json` output, filtered withdraws
	//	                the coverage its rows give
	//	file_account    the account a file is for: acct:<account>, or auto
	//	file_range      the days a file that states none covers:
	//	                <from>..<to> (YYYY-MM-DD), or auto
	//	forget          whether a file is set aside: yes or no
	Kind string
	// Subject is what it is about: an entry's subject (a:<account>:<activity
	// ID>, or a:<account>:~<time>.<digest> for one without an activity ID),
	// a row of a History file (c:<sha256 hex>:<ordinal>), which stands for
	// the file transaction that holds it, a file's (f:<sha256 hex>), a key's
	// (k:<key>) or an account's (acct:<account>).
	Subject string
	Value   string
	// Note is yours to say why. It is kept masked (mask.Text), as all free
	// text is.
	Note string
}

// Decided says what a decision changed.
type Decided struct {
	// DecisionID is the decision's id in the log.
	DecisionID int64
	// Changes are the entries whose booking the decision changed, by
	// account and ref.
	Changes []EntryChange
}

// An EntryChange is an entry that a decision booked otherwise.
type EntryChange struct {
	AccountID     int64
	Ref           string
	Before, After EntryState
}

// An EntryState is how an entry is booked: its category and the rule that
// gave it (user for your decision), whether it counts and why, its pair,
// and its security by its key.
type EntryState struct {
	Category, CategoryRule string
	Counted                bool
	CountedWhy             string
	PairKind, PairRef      string
	SecurityKey            string
}

// decisionKinds are the kinds that Decide takes. Decisions that link a
// file transaction to API transactions come with the matching of the two.
var decisionKinds = []string{
	"category", "count", "pair", "entry_security", "security", "account",
	"completeness", "file_account", "file_range", "forget",
}

// Decide records your decision d, once it has checked that its subject is
// in the store now and that its value is one its kind takes, and derives
// again what it bears on. A decision on an entry records the revision of
// the transaction it was made against, so that the entry is flagged
// stale_decision when what the rules read of the transaction changes.
func (s *Store) Decide(ctx context.Context, d Decision) (Decided, error) {
	if !slices.Contains(decisionKinds, d.Kind) {
		return Decided{}, fmt.Errorf("schwabtx takes no decision of kind %q; it takes %s", mask.Text(d.Kind), strings.Join(decisionKinds, ", "))
	}
	var out Decided
	err := s.withWrite(ctx, "recording your decision", func(w *wtx) error {
		out = Decided{}
		before, err := entryStates(w)
		if err != nil {
			return err
		}
		basis, err := checkDecision(w, d)
		if err != nil {
			return err
		}
		if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO decision (made_at, kind, subject, value, basis, note)
			VALUES (?, ?, ?, ?, ?, ?) RETURNING decision_id`,
			instant(w.now), d.Kind, d.Subject, d.Value, basis, free(d.Note)).Scan(&out.DecisionID); err != nil {
			return err
		}
		if err := w.derive(); err != nil {
			return err
		}
		after, err := entryStates(w)
		if err != nil {
			return err
		}
		for _, k := range slices.SortedFunc(maps.Keys(after), func(a, b entryKey) int {
			return cmp.Or(cmp.Compare(a.account, b.account), cmp.Compare(a.ref, b.ref))
		}) {
			if b, ok := before[k]; ok && b != after[k] {
				out.Changes = append(out.Changes, EntryChange{AccountID: k.account, Ref: k.ref, Before: b, After: after[k]})
			}
		}
		return nil
	})
	return out, err
}

// entryStates reads how every entry is booked.
func entryStates(w *wtx) (map[entryKey]EntryState, error) {
	states := map[entryKey]EntryState{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var k entryKey
		var st EntryState
		var pairKind, pairRef, key sql.NullString
		if err := rows.Scan(&k.account, &k.ref, &st.Category, &st.CategoryRule, &st.Counted, &st.CountedWhy,
			&pairKind, &pairRef, &key); err != nil {
			return err
		}
		st.PairKind, st.PairRef, st.SecurityKey = pairKind.String, pairRef.String, key.String
		states[k] = st
		return nil
	}, `SELECT e.account_id, e.ref, e.category, e.category_rule, e.counted, e.counted_why, e.pair_kind, p.ref,
		s.canonical_key FROM entry e LEFT JOIN entry p ON p.entry_id = e.pair_entry_id
		LEFT JOIN security s ON s.security_id = e.security_id`)
	return states, err
}

// checkDecision checks d against the store, touches what it bears on, and
// returns its basis: for a decision on an API entry, the revision of the
// transaction it is made against; for one on a file transaction, the rows
// it holds and the one it shows.
func checkDecision(w *wtx, d Decision) (sql.NullString, error) {
	var basis sql.NullString
	refuse := func(format string, args ...any) (sql.NullString, error) {
		return basis, fmt.Errorf(format, args...)
	}
	// refuseOr refuses, or returns err when the store could not say.
	refuseOr := func(err error, format string, args ...any) (sql.NullString, error) {
		if err != nil {
			return basis, err
		}
		return refuse(format, args...)
	}
	switch d.Kind {
	case "security":
		key, _ := strings.CutPrefix(d.Subject, "k:")
		if ok, err := keyExists(w, key); err != nil || !ok || !strings.HasPrefix(d.Subject, "k:") {
			return refuseOr(err, "no instrument key %s in the store", keyRef(d.Subject))
		}
		if d.Value != "auto" {
			how, other, _ := strings.Cut(d.Value, " ")
			okey, isKey := strings.CutPrefix(other, "k:")
			if how != "same" && how != "differ" {
				return refuse("a security decision is same K…, differ K… or auto, not %q", mask.Text(d.Value))
			}
			if ok, err := keyExists(w, okey); err != nil || !ok || !isKey {
				return refuseOr(err, "no instrument key %s in the store", keyRef(other))
			}
			if okey == key {
				return refuse("%s cannot be decided %s itself", keyRef(d.Subject), how)
			}
		}
		w.regroup = true
		return basis, nil
	case "account":
		id, err := strconv.ParseInt(strings.TrimPrefix(d.Subject, "acct:"), 10, 64)
		if err != nil || !strings.HasPrefix(d.Subject, "acct:") {
			return refuse("no account %s in the store", d.Subject)
		}
		if ok, err := rowExists(w, "SELECT 1 FROM account WHERE account_id = ?", id); err != nil || !ok {
			return refuseOr(err, "no account %s in the store", d.Subject)
		}
		if kind, ok := strings.CutPrefix(d.Value, "kind="); d.Value != "auto" && (!ok || strings.TrimSpace(kind) == "") {
			return refuse("an account decision is kind=TEXT or auto, not %q", mask.Text(d.Value))
		}
		w.touch(id)
		return basis, nil
	case "completeness", "file_account", "file_range", "forget":
		return basis, checkFileDecision(w, d)
	}
	if strings.HasPrefix(d.Subject, "c:") {
		return checkRowDecision(w, d)
	}
	// The rest are about an entry.
	var acct int64
	var txn sql.NullInt64
	var ref string
	err := w.tx.QueryRowContext(w.ctx, "SELECT account_id, txn_id, ref FROM entry WHERE subject = ?", d.Subject).Scan(&acct, &txn, &ref)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse("no transaction %s in the store", d.Subject)
	}
	if err != nil {
		return basis, err
	}
	// An entry paired with this one, in another account, is derived again
	// when its pair changes (derive).
	w.touch(acct)
	switch d.Kind {
	case "category":
		if d.Value != "auto" && !slices.Contains(categories, d.Value) {
			return refuse("unknown category %q; one of %s, or auto", mask.Text(d.Value), strings.Join(categories, ", "))
		}
	case "count":
		if !slices.Contains([]string{"yes", "no", "auto"}, d.Value) {
			return refuse("a count decision is yes, no or auto, not %q", mask.Text(d.Value))
		}
	case "pair":
		if d.Value == "none" || d.Value == "auto" {
			break
		}
		var other int64
		err := w.tx.QueryRowContext(w.ctx, "SELECT account_id FROM entry WHERE subject = ?", d.Value).Scan(&other)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return refuse("no transaction %s in the store to pair %s with", d.Value, ref)
		case err != nil:
			return basis, err
		case d.Value == d.Subject:
			return refuse("%s cannot be paired with itself", ref)
		}
		w.touch(other)
	case "entry_security":
		if d.Value == "auto" {
			break
		}
		key, isKey := strings.CutPrefix(d.Value, "k:")
		if ok, err := keyExists(w, key); err != nil || !ok || !isKey {
			return refuseOr(err, "no instrument key %s in the store", keyRef(d.Value))
		}
	}
	if txn.Valid {
		var seq int
		if err := w.tx.QueryRowContext(w.ctx, "SELECT max(seq) FROM api_revision WHERE txn_id = ?", txn.Int64).Scan(&seq); err != nil {
			return basis, err
		}
		basis = sql.NullString{String: `{"seq":` + strconv.Itoa(seq) + `}`, Valid: true}
	}
	return basis, nil
}

// keyExists reports whether the store holds key.
func keyExists(w *wtx, key string) (bool, error) {
	return rowExists(w, "SELECT 1 FROM instrument_key WHERE key = ?", key)
}

// rowExists reports whether query returns a row.
func rowExists(w *wtx, query string, args ...any) (bool, error) {
	var one int
	err := w.tx.QueryRowContext(w.ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// keyRef returns a key's subject, k:<key>, as its ref, K:<key>.
func keyRef(subject string) string {
	if key, ok := strings.CutPrefix(subject, "k:"); ok {
		return "K:" + key
	}
	return subject
}

// checkFileDecision checks a decision on a file, d, whose subject is
// f:<sha256 hex>, and marks the write to derive the files again: their
// accounts, and the account each is for.
func checkFileDecision(w *wtx, d Decision) error {
	var kind, ref string
	var acct sql.NullInt64
	err := w.tx.QueryRowContext(w.ctx, `SELECT f.kind, d.ref, d.account_id FROM import_file f JOIN file_fields d ON d.file_id = f.file_id
		WHERE ? = 'f:' || lower(hex(f.sha256))`, d.Subject).Scan(&kind, &ref, &acct)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no file %s in the store", mask.Text(d.Subject))
	}
	if err != nil {
		return err
	}
	if kind == "api_json" && d.Kind != "completeness" {
		return fmt.Errorf("%s brought transactions from Schwab's API, which the store keeps; it is for no one account and cannot be set aside", ref)
	}
	switch d.Kind {
	case "completeness":
		allowed := []string{"complete", "filtered", "auto"}
		if kind == "api_json" {
			allowed = []string{"filtered", "auto"}
		}
		if !slices.Contains(allowed, d.Value) {
			return fmt.Errorf("a completeness decision on %s is %s, not %q", ref, strings.Join(allowed, ", "), mask.Text(d.Value))
		}
	case "file_account":
		if d.Value != "auto" {
			id, err := strconv.ParseInt(strings.TrimPrefix(d.Value, "acct:"), 10, 64)
			if !strings.HasPrefix(d.Value, "acct:") || err != nil {
				return fmt.Errorf("a file_account decision is acct:<account> or auto, not %q", mask.Text(d.Value))
			}
			if ok, err := rowExists(w, "SELECT 1 FROM account WHERE account_id = ?", id); err != nil || !ok {
				return cmp.Or(err, fmt.Errorf("no account A%d in the store", id))
			}
		}
	case "file_range":
		from, to, ok := strings.Cut(d.Value, "..")
		_, errFrom := time.Parse(time.DateOnly, from)
		_, errTo := time.Parse(time.DateOnly, to)
		if d.Value != "auto" && (!ok || errFrom != nil || errTo != nil || to < from) {
			return fmt.Errorf("a file_range decision is FROM..TO (YYYY-MM-DD, FROM not after TO) or auto, not %q", mask.Text(d.Value))
		}
	case "forget":
		if d.Value != "yes" && d.Value != "no" {
			return fmt.Errorf("a forget decision is yes or no, not %q", mask.Text(d.Value))
		}
	}
	if acct.Valid {
		w.touch(acct.Int64)
	}
	if kind == "api_json" {
		return queryEach(w, func(rows *sql.Rows) error {
			var id int64
			err := rows.Scan(&id)
			w.touch(id)
			return err
		}, `SELECT DISTINCT r.account_id FROM api_read r JOIN import_file f ON f.file_id = r.file_id
			WHERE ? = 'f:' || lower(hex(f.sha256))`, d.Subject)
	}
	w.files = true
	return nil
}

// checkRowDecision checks a decision on a file transaction, d, whose
// subject is a row of a History file, c:<sha256 hex>:<ordinal>, and
// returns its basis: the rows of the transaction that holds the row now
// and the one it shows, each <sha256 hex>:<ordinal>, against which it goes
// stale. A row of a file set aside holds no transaction; a decision on it
// waits for one (13.7 of the design).
func checkRowDecision(w *wtx, d Decision) (sql.NullString, error) {
	var basis sql.NullString
	if !slices.Contains(decidedKinds, d.Kind) {
		return basis, fmt.Errorf("schwabtx takes no decision of kind %s on a file transaction", d.Kind)
	}
	var row int64
	var acct sql.NullInt64
	err := w.tx.QueryRowContext(w.ctx, `SELECT r.row_id, d.account_id FROM history_row r JOIN import_file f ON f.file_id = r.file_id
		JOIN file_fields d ON d.file_id = f.file_id WHERE ? = 'c:' || lower(hex(f.sha256)) || ':' || r.ordinal`, d.Subject).Scan(&row, &acct)
	if errors.Is(err, sql.ErrNoRows) {
		return basis, fmt.Errorf("no transaction %s in the store", mask.Text(d.Subject))
	}
	if err != nil {
		return basis, err
	}
	switch d.Kind {
	case "category":
		if d.Value != "auto" && !slices.Contains(categories, d.Value) {
			return basis, fmt.Errorf("unknown category %q; one of %s, or auto", mask.Text(d.Value), strings.Join(categories, ", "))
		}
	case "count":
		if !slices.Contains([]string{"yes", "no", "auto"}, d.Value) {
			return basis, fmt.Errorf("a count decision is yes, no or auto, not %q", mask.Text(d.Value))
		}
	case "pair":
		if d.Value != "none" && d.Value != "auto" {
			var other int64
			if err := w.tx.QueryRowContext(w.ctx, "SELECT account_id FROM entry WHERE subject = ?", d.Value).Scan(&other); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return basis, fmt.Errorf("no transaction %s in the store to pair it with", mask.Text(d.Value))
				}
				return basis, err
			}
			w.touch(other)
		}
	case "entry_security":
		if d.Value != "auto" {
			key, isKey := strings.CutPrefix(d.Value, "k:")
			if ok, err := keyExists(w, key); err != nil || !ok || !isKey {
				if err != nil {
					return basis, err
				}
				return basis, fmt.Errorf("no instrument key %s in the store", keyRef(d.Value))
			}
		}
	}
	if acct.Valid {
		w.touch(acct.Int64)
	}
	var b struct {
		Shown string   `json:"shown"`
		Rows  []string `json:"rows"`
	}
	err = queryEach(w, func(rows *sql.Rows) error {
		var id string
		var shown bool
		err := rows.Scan(&id, &shown)
		b.Rows = append(b.Rows, id)
		if shown {
			b.Shown = id
		}
		return err
	}, `SELECT lower(hex(f.sha256)) || ':' || r.ordinal, r.row_id = c.shown_row_id FROM csv_member m
		JOIN csv_txn c ON c.csv_txn_id = m.csv_txn_id JOIN csv_member o ON o.csv_txn_id = c.csv_txn_id
		JOIN history_row r ON r.row_id = o.row_id JOIN import_file f ON f.file_id = r.file_id
		WHERE m.row_id = ? ORDER BY f.file_id, r.ordinal`, row)
	if err != nil || b.Shown == "" {
		return basis, err
	}
	j, err := json.Marshal(b)
	return sql.NullString{String: string(j), Valid: true}, err
}
