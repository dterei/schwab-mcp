package history

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// An APIEntry is what the ledger derives from one API transaction of a
// read: before the store's pairing, matching and counting, and before your
// decisions. Money is in cents and quantities in millionths, nil where the
// store would hold NULL.
type APIEntry struct {
	// Ref is the store's ref of the transaction: T and its activity ID;
	// without one, T~, its time and twelve hex digits of its cut digest,
	// then .2, .3 and so on for a second and third row of that time and
	// cut form.
	Ref string
	// Day is the day it was booked on, its trade day.
	Day nytime.Date
	// Category is what the rules book it as, and Rule the rule's name.
	Category, Rule string
	// Symbol is that of its one security leg, else the one after the tilde
	// of its description, or "".
	Symbol                             string
	AmountCents, FeesCents, GrossCents *int64
	QuantityMicros                     *int64
	Price                              *float64
	// Flags are the codes of the rules' flags, in order, as the entries
	// view lists them.
	Flags []string
}

// DeriveAPIEntries derives the entries of rows, one read's answer, in
// Schwab's order, for the account whose full number is accountNumber, as
// the store derives the transactions it holds once it has recorded the
// read into an account of no other evidence: each row's activity ID and
// cut form (prepareRows), the IDs that the answer holds twice
// (duplicates), its columns and legs with the account's number (txnFields,
// legsOf), its ref (txnRefs), then the fields and the rules of
// apiEntryFields, which apiEntries shares. A row without an activity ID is
// a transaction of its own; rows of one activity ID are one transaction,
// whose fields are those of the last, whose raw JSON the store keeps. What
// needs the store is left out: the security a decision names, your
// decisions, pairs, matches, counting, and the revised, gone and absence
// flags.
//
// It returns one entry for each row, in rows' order, so that rows of one
// activity ID have one entry each, the same: their transaction's, flagged
// duplicate_in_answer. A row without its raw JSON is an error, as the store
// refuses it.
func DeriveAPIEntries(rows []schwab.Transaction, accountNumber string) ([]APIEntry, error) {
	if accountNumber == "" || accountNumber != strings.TrimSpace(accountNumber) {
		return nil, fmt.Errorf("deriving a read's entries: %q is not an account number", mask.Text(accountNumber))
	}
	prepared, err := prepareRows(rows)
	if err != nil {
		return nil, fmt.Errorf("deriving a read's entries: %w", err)
	}
	dups := duplicates(prepared)
	// The transactions the rows are, by id, an index into txns; a row of an
	// activity ID that came before is that ID's transaction again.
	var txns []txnInput
	of := make([]int, len(prepared))
	byActivity := map[int64]int{}
	for i, p := range prepared {
		t, err := apiTxnInput(p, accountNumber, slices.Contains(dups, p.id))
		if err != nil {
			return nil, err
		}
		if j, ok := byActivity[p.id]; ok && p.id != 0 {
			t.id = txns[j].id
			txns[j], of[i] = t, j
			continue
		}
		t.id = int64(len(txns) + 1)
		byActivity[p.id] = len(txns)
		of[i] = len(txns)
		txns = append(txns, t)
	}
	refs := txnRefs(0, txns)
	entries := make([]APIEntry, len(txns))
	for i, t := range txns {
		f := apiEntryFields(t)
		var flags []string
		for _, fl := range f.flags.sorted() {
			flags = append(flags, fl.code)
		}
		entries[i] = APIEntry{
			Ref: refs[t.id].ref, Day: t.tradeDay, Category: f.category, Rule: f.rule, Symbol: f.symbol.String,
			AmountCents: nullInt(f.amount), FeesCents: nullInt(f.fees), GrossCents: nullInt(f.gross),
			QuantityMicros: nullInt(f.quantity), Price: nullFloat(f.price), Flags: flags,
		}
	}
	out := make([]APIEntry, len(prepared))
	for i, j := range of {
		out[i] = entries[j]
		out[i].Flags = slices.Clone(entries[j].Flags)
	}
	return out, nil
}

// apiTxnInput returns p, a row of a read of the account whose number is
// number, as the derivation reads the transaction that the store records
// for it, as loadAccount reads one: its columns and legs as txnFields gives
// them, its times as the store keeps them, to the millisecond, and its
// description as Schwab sent it. duplicate says that the answer holds its
// activity ID twice.
func apiTxnInput(p preparedRow, number string, duplicate bool) (txnInput, error) {
	r, legs := txnFields(p.row, number)
	t := txnInput{
		firstRead: 1, latestListRead: 1, tradeDay: nytime.Date(r.tradeDay.String),
		digests: [][32]byte{p.digest}, changed: [][]string{nil},
		activityID: p.id, idSource: p.idSource,
		typ: r.typ.String, status: r.status.String, description: r.description,
		net: r.net, legs: legsOf(legs), numberSent: r.numberSent.String, duplicate: duplicate,
	}
	t.text = r.description.String
	if strings.Contains(t.text, "...") {
		t.text = rawDescription(string(p.row.Raw))
	}
	if err := json.Unmarshal([]byte(r.problems), &t.problems); err != nil {
		return txnInput{}, err
	}
	var err error
	if t.time, err = parseNullInstant(r.time); err != nil {
		return txnInput{}, err
	}
	if t.tradeDate, err = parseNullInstant(r.tradeDate); err != nil {
		return txnInput{}, err
	}
	return t, nil
}

// nullInt returns v as a pointer, nil for NULL.
func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// nullFloat returns v as a pointer, nil for NULL.
func nullFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}
