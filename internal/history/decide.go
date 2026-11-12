package history

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

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
	Kind string
	// Subject is what it is about: an entry's subject (a:<account>:<activity
	// ID>, or a:<account>:~<time>.<digest> for one without an activity ID),
	// a key's (k:<key>) or an account's (acct:<account>).
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

// decisionKinds are the kinds that Decide takes: those of this part of the
// store. Decisions about files come with their import.
var decisionKinds = []string{"category", "count", "pair", "entry_security", "security", "account"}

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
// transaction it is made against.
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
