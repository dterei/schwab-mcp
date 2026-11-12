package history

import (
	"cmp"
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// An entryKey names an entry across derivations: its account and its ref.
// Entries' row ids may change; their keys do not while their evidence
// stays.
type entryKey struct {
	account int64
	ref     string
}

// An entryRow is an entry's columns in entry but for its pair, which
// names another entry by its row id.
type entryRow struct {
	txn, csvTxn            sql.NullInt64
	ref, subject           string
	kind, description      sql.NullString
	day, posted, time      sql.NullString
	amount, fees, gross    sql.NullInt64
	quantity               sql.NullInt64
	quantityAsOf           sql.NullString
	price                  sql.NullFloat64
	symbol                 sql.NullString
	security               sql.NullInt64
	family, category, rule string
	counted                bool
	countedWhy             string
}

// A ledgerEntry is an entry as the derivation makes it: its columns, its
// pair and its flags, and what pairing it reads.
type ledgerEntry struct {
	entryRow
	account int64
	// pairKind is journal, sweep or reversal, and pair the entry it is
	// paired with, when it is.
	pairKind string
	pair     entryKey
	flags    flagSet
	// text is its description unmasked, in which a journal names the
	// account it went to.
	text string
	// userCategory says that your category decision booked it, and
	// pairDecided that one of your pair decisions names it.
	userCategory, pairDecided bool
	// csvSubject is the subject of a file transaction's entry, by which its
	// row id in csv_txn is found once that is written.
	csvSubject string
}

// key returns the entry's key.
func (e *ledgerEntry) key() entryKey { return entryKey{e.account, e.ref} }

// A decisionInput is one of your decisions as the derivation reads it.
type decisionInput struct {
	id                   int64
	kind, subject, value string
	// basisSeq is the revision of the transaction that the decision was
	// made against, when basisValid.
	basisSeq   int
	basisValid bool
	// basisShown and basisRows are, for a decision on a file transaction,
	// the row it showed and the rows it held, each <sha256 hex>:<ordinal>.
	basisShown string
	basisRows  []string
}

// latestDecisions returns the latest decision of each kind and subject,
// leaving out those that withdraw the ones before them (auto).
func latestDecisions(ds []decisionInput) map[[2]string]decisionInput {
	latest := map[[2]string]decisionInput{}
	for _, d := range ds {
		k := [2]string{d.kind, d.subject}
		if l, ok := latest[k]; !ok || d.id > l.id {
			latest[k] = d
		}
	}
	for k, d := range latest {
		if d.value == "auto" {
			delete(latest, k)
		}
	}
	return latest
}

// decidedKinds are the kinds of your decisions on an entry that record
// the revision they were made against, and go stale when what the rules
// read of the transaction changes.
var decidedKinds = []string{"category", "count", "pair", "entry_security"}

// answeredByCategory are the flags that say how the rules booked an entry
// for want of knowing better, which your category answers.
var answeredByCategory = []string{"awaits_probe", "unexpected_sign", "zero_amount", "unknown_action", "other_action"}

// apiEntries makes the ledger entries of the account's API transactions,
// in the order of their refs: each one's fields (13.1 of the design), its
// classification (13.3), then your category, and its counting and flags.
// Pairs come after (pairWithin, pairAccounts). gone holds the read that
// shows each transaction gone, by id, and earlier the read before its
// first that left it out (absentEarlier).
func apiEntries(in accountInputs, gone map[int64]int64, revisions map[int64]int, earlier map[int64]int64) []ledgerEntry {
	reads := map[int64]readInput{}
	for _, r := range in.reads {
		reads[r.id] = r
	}
	latest := latestDecisions(in.decisions)
	refs := txnRefs(in.account, in.txns)
	traded := tradedSecurities(in.txns)
	entries := make([]ledgerEntry, 0, len(in.txns))
	for _, t := range in.txns {
		e := ledgerEntry{account: in.account, text: t.text}
		e.txn = sql.NullInt64{Int64: t.id, Valid: true}
		r := refs[t.id]
		e.ref, e.subject = r.ref, r.subject
		e.kind, e.description = nullString(t.typ), t.description
		e.day = nullString(string(t.tradeDay))
		if !t.time.IsZero() {
			e.time = sql.NullString{String: instant(t.time), Valid: true}
		}
		e.amount = t.net
		var sec []legInput
		for _, l := range t.legs {
			if l.isSecurity() {
				sec = append(sec, l)
			}
		}
		e.fees = sql.NullInt64{Valid: true}
		if len(sec) > 0 {
			for _, l := range t.legs {
				if !l.isSecurity() {
					e.fees = addCents(e.fees, sql.NullInt64{Int64: -l.costCents.Int64, Valid: l.costCents.Valid})
				}
			}
		}
		e.gross = addCents(e.amount, e.fees)
		sym, hasSym := schwab.SymbolAfterTilde(t.text)
		if len(sec) == 1 {
			e.quantity, e.price = sec[0].amountMicros, sec[0].price
			e.symbol = nullString(sec[0].symbol)
		}
		if !e.symbol.Valid && hasSym {
			e.symbol = nullString(sym)
		}
		var decided sql.NullInt64
		if d, ok := latest[[2]string{"entry_security", e.subject}]; ok {
			if id, ok := in.keySecurities[strings.TrimPrefix(d.value, "k:")]; ok {
				decided = sql.NullInt64{Int64: id, Valid: true}
			}
		}
		var how entryFlag
		e.security, how = entrySecurity(decided, t.legs, t.text, t.tradeDay, in.names, traded)
		if how.code != "" {
			e.flags.add(how.code, how.detail)
		}

		cl := classifyAPI(classOf(t.typ, t.status, t.text, t.net, t.legs))
		e.family, e.category, e.rule = cl.family, cl.category, cl.rule
		for _, f := range cl.flags {
			e.flags.add(f.code, f.detail)
		}
		if d, ok := latest[[2]string{"category", e.subject}]; ok {
			e.category, e.rule, e.userCategory = d.value, "user", true
			e.flags.drop(answeredByCategory...)
		}
		_, e.pairDecided = in.pairDecided[e.subject]

		legsCheck(&e.flags, t)
		for _, p := range t.problems {
			switch p.Flag {
			case "":
				e.flags.add("problems", p.Path)
			default:
				where := p.Path
				if p.Leg != nil {
					where = strings.Replace(p.Path, ".", "["+strconv.Itoa(*p.Leg)+"].", 1)
				}
				e.flags.add(p.Flag, where+" "+p.Value)
			}
		}
		if t.status != string(schwab.TransactionStatusValid) {
			e.flags.add("status", cmp.Or(t.status, "none"))
		}
		switch t.idSource {
		case "none":
			e.flags.add("no_activity_id", "")
		case "string":
			e.flags.add("activity_id_text", "")
		}
		if t.numberSent != "" {
			e.flags.add("account_mismatch", "the row names "+t.numberSent)
		}
		if t.duplicate {
			e.flags.add("duplicate_in_answer", "")
		}
		if revisions[t.id] > 1 {
			e.flags.add("revised", strings.Join(changedSince(t), ", "))
		}
		if r, ok := reads[gone[t.id]]; ok {
			e.flags.add("gone", "left out by the complete read sent "+instant(r.sent))
		}
		if r, ok := reads[earlier[t.id]]; ok {
			e.flags.add("absent_earlier", "left out by the complete read sent "+instant(r.sent)+
				", before you imported it; if it was cancelled: schwabtx edit "+e.ref+" -count no")
		}
		var stale []string
		for _, kind := range decidedKinds {
			d, ok := latest[[2]string{kind, e.subject}]
			if !ok || !d.basisValid {
				continue
			}
			if b, ok := t.bases[d.basisSeq]; !ok || !b.equal(t.current) {
				stale = append(stale, fmt.Sprintf("%s decided against revision %d", kind, d.basisSeq))
			}
		}
		if len(stale) > 0 {
			e.flags.add("stale_decision", strings.Join(stale, "; "))
		}

		switch d, ok := latest[[2]string{"count", e.subject}]; {
		case ok:
			e.counted, e.countedWhy = d.value == "yes", "user"
		case gone[t.id] != 0:
			e.countedWhy = "gone"
		case t.status == string(schwab.TransactionStatusInvalid):
			e.countedWhy = "invalid"
		default:
			e.counted, e.countedWhy = true, "api"
		}
		entries = append(entries, e)
	}
	slices.SortFunc(entries, func(a, b ledgerEntry) int { return cmp.Compare(a.ref, b.ref) })
	return entries
}

// A txnRef is the ref and the subject of an API transaction.
type txnRef struct{ ref, subject string }

// txnRefs returns the refs and subjects of the API transactions txns of
// account acct, by id (apiRef). Transactions without an activity ID at one
// time and with one cut form, which an answer can hold, take refs of their
// own: in the order of their ids, the second's ends .2, and so on.
func txnRefs(acct int64, txns []txnInput) map[int64]txnRef {
	type made struct {
		id int64
		txnRef
	}
	all := make([]made, len(txns))
	for i, t := range txns {
		ref, subject := apiRef(acct, t)
		all[i] = made{t.id, txnRef{ref, subject}}
	}
	slices.SortFunc(all, func(a, b made) int { return cmp.Or(cmp.Compare(a.ref, b.ref), cmp.Compare(a.id, b.id)) })
	out := make(map[int64]txnRef, len(all))
	n := 1
	for i, m := range all {
		if i > 0 && m.ref == all[i-1].ref {
			n++
			m.ref += "." + strconv.Itoa(n)
			m.subject += "." + strconv.Itoa(n)
		} else {
			n = 1
		}
		out[m.id] = m.txnRef
	}
	return out
}

// apiRef returns the ref and the subject of an API transaction of account
// acct: T and its activity ID; without one, T~, its time and twelve hex
// digits of its cut digest, stable while the row is (9.4 of the design).
func apiRef(acct int64, t txnInput) (ref, subject string) {
	prefix := "a:" + strconv.FormatInt(acct, 10) + ":"
	if t.activityID > 0 {
		id := strconv.FormatInt(t.activityID, 10)
		return "T" + id, prefix + id
	}
	at := "none"
	if !t.time.IsZero() {
		at = t.time.UTC().Format("20060102T150405")
	}
	var digest [32]byte
	if n := len(t.digests); n > 0 {
		digest = t.digests[n-1]
	}
	tail := fmt.Sprintf("~%s.%x", at, digest[:6])
	return "T" + tail, prefix + tail
}

// addCents returns a + b, NULL when either is.
func addCents(a, b sql.NullInt64) sql.NullInt64 {
	return sql.NullInt64{Int64: a.Int64 + b.Int64, Valid: a.Valid && b.Valid}
}

// legsCheck flags a transaction whose legs' cash is not its net amount
// (13.2 of the design): the sum of every leg's cost when it has a leg of a
// security, as a trade's net amount is; else the sum of its cash legs'
// amounts, which is where a cash row carries its cash. A value the store
// could not keep skips the check, and a row without legs cannot be checked.
func legsCheck(flags *flagSet, t txnInput) {
	if len(t.legs) == 0 {
		flags.add("no_legs", "")
		return
	}
	trade := slices.ContainsFunc(t.legs, legInput.isSecurity)
	sum := sql.NullInt64{Valid: true}
	for _, l := range t.legs {
		switch {
		case trade:
			sum = addCents(sum, l.costCents)
		case !l.isSecurity():
			sum = addCents(sum, l.amountCents)
		}
	}
	if sum.Valid && t.net.Valid && sum.Int64 != t.net.Int64 {
		flags.add("legs_mismatch", "the legs' cash is "+formatCents(sum.Int64)+", the net amount "+formatCents(t.net.Int64))
	}
}

// formatCents writes an amount in cents as dollars with two places.
func formatCents(c int64) string {
	s := ""
	if c < 0 {
		s, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", s, c/100, c%100)
}

// changedSince returns the key paths that changed in a transaction's
// revisions, each once, in order: those of each run whose cut differs from
// the run's before.
func changedSince(t txnInput) []string {
	var paths []string
	for i := 1; i < len(t.digests) && i < len(t.changed); i++ {
		if t.digests[i] != t.digests[i-1] {
			paths = append(paths, t.changed[i]...)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths)
}

// entrySecurity returns the security of an API entry and, when it is found
// by name or not at all, the flag that says so (13.1 and 10.3 of the
// design): your entry_security decision's, decided; else that of its one
// security leg; else the one that the symbol after the tilde of its
// description, text, named on its day: a name seen within symbolSlack of
// the day, else a security of a leg that the account's transactions
// traded within symbolSlack of the day and that has held the symbol
// (tradedNear), else a name seen within a year (by name); else none.
func entrySecurity(decided sql.NullInt64, legs []legInput, text string, day nytime.Date, names []symbolName, traded []tradedSecurity) (sql.NullInt64, entryFlag) {
	if decided.Valid {
		return decided, entryFlag{}
	}
	var sec []legInput
	for _, l := range legs {
		if l.isSecurity() {
			sec = append(sec, l)
		}
	}
	if len(sec) == 1 && sec[0].security.Valid {
		return sec[0].security, entryFlag{}
	}
	if sym, ok := schwab.SymbolAfterTilde(text); ok {
		if day != "" {
			r, ok := resolveSymbol(names, sym, day)
			if ok && !r.byName {
				return sql.NullInt64{Int64: r.security, Valid: true}, entryFlag{}
			}
			if id, ok := tradedNear(traded, names, sym, day); ok {
				return sql.NullInt64{Int64: id, Valid: true}, entryFlag{}
			}
			if ok {
				return sql.NullInt64{Int64: r.security, Valid: true}, entryFlag{"security_by_name", sym + " last seen " + string(nytime.DateOf(r.seen))}
			}
		}
		return sql.NullInt64{}, entryFlag{"unresolved_security", sym}
	}
	if len(sec) == 1 {
		return sql.NullInt64{}, entryFlag{"unresolved_security", sec[0].symbol}
	}
	return sql.NullInt64{}, entryFlag{}
}

// A tradedSecurity is a security that a leg of one of an account's
// transactions moved, and the day the transaction was booked on.
type tradedSecurity struct {
	day      nytime.Date
	security int64
}

// tradedSecurities returns the securities that the legs of txns, an
// account's transactions, moved, with their days.
func tradedSecurities(txns []txnInput) []tradedSecurity {
	var out []tradedSecurity
	for _, t := range txns {
		for _, l := range t.legs {
			if l.isSecurity() && l.security.Valid && t.tradeDay != "" {
				out = append(out, tradedSecurity{t.tradeDay, l.security.Int64})
			}
		}
	}
	return out
}

// tradedNear returns the one security among traded, what an account's
// transactions moved, that a transaction booked within symbolSlack of day
// moved and that has ever held symbol, or false when none or several did.
// Names are dated when they were served, so a read that comes long after a
// dividend, as a backfill does, dates its security's names long after it;
// but a transaction of the same account that week, such as the dividend's
// reinvestment, shows which security the symbol named then. A reused
// ticker's later holder is not among them unless the account traded it
// that week.
func tradedNear(traded []tradedSecurity, names []symbolName, symbol string, day nytime.Date) (int64, bool) {
	held := map[int64]bool{}
	for _, n := range names {
		if n.value == symbol {
			held[n.security] = true
		}
	}
	found := map[int64]bool{}
	for _, t := range traded {
		if d, ok := daysApart(string(day), string(t.day)); ok && d >= -7 && d <= 7 && held[t.security] {
			found[t.security] = true
		}
	}
	if len(found) != 1 {
		return 0, false
	}
	for id := range found {
		return id, true
	}
	return 0, false
}

// daysApart returns how many days b is after a, or false when either is
// not a date.
func daysApart(a, b string) (int, bool) {
	ta, err := time.Parse(time.DateOnly, a)
	if err != nil {
		return 0, false
	}
	tb, err := time.Parse(time.DateOnly, b)
	if err != nil {
		return 0, false
	}
	return int(tb.Sub(ta).Hours() / 24), true
}

// pairWithin pairs an account's entries with one another, in a fixed
// order whatever the order of the entries, each entry in one pair at most
// and none that one of your pair decisions names (pairAccounts pairs
// those):
//
//   - sweep: two sweeps of opposite amounts at most a day apart;
//   - reversal: a file's Cancel Buy or Cancel Sell and the Buy or Sell of
//     the same symbol that it cancels, posted up to 5 days before it, of
//     equal amounts and quantities with the quantities of opposite signs,
//     the nearest first.
//
// Journals pair across accounts, in pairAccounts.
func pairWithin(entries []ledgerEntry) {
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int {
		a, b := &entries[i], &entries[j]
		return cmp.Or(cmp.Compare(a.day.String, b.day.String), cmp.Compare(a.ref, b.ref))
	})
	free := func(e *ledgerEntry) bool { return e.pairKind == "" && !e.pairDecided }
	for x, i := range order {
		a := &entries[i]
		if a.family != "sweep" || !free(a) || !a.amount.Valid || a.amount.Int64 == 0 {
			continue
		}
		for _, j := range order[x+1:] {
			b := &entries[j]
			d, ok := daysApart(a.day.String, b.day.String)
			if b.family == "sweep" && free(b) && b.amount.Valid && b.amount.Int64 == -a.amount.Int64 && ok && d <= 1 {
				a.pairKind, a.pair, b.pairKind, b.pair = "sweep", b.key(), "sweep", a.key()
				break
			}
		}
	}
	posted := func(e *ledgerEntry) string { return cmp.Or(e.posted.String, e.day.String) }
	abs := func(v sql.NullInt64) int64 { return max(v.Int64, -v.Int64) }
	for _, i := range order {
		c := &entries[i]
		if c.family != "reversal" || !free(c) {
			continue
		}
		var best *ledgerEntry
		bestDays := 0
		for _, j := range order {
			o := &entries[j]
			d, ok := daysApart(posted(o), posted(c))
			if (o.family != "buy" && o.family != "sell") || !free(o) || !ok || d < 0 || d > 5 ||
				o.symbol != c.symbol || !o.amount.Valid || !c.amount.Valid || abs(o.amount) != abs(c.amount) || !o.quantity.Valid || !c.quantity.Valid ||
				o.quantity.Int64 != -c.quantity.Int64 || o.quantity.Int64 == 0 {
				continue
			}
			if best == nil || d < bestDays {
				best, bestDays = o, d
			}
		}
		if best != nil {
			c.pairKind, c.pair, best.pairKind, best.pair = "reversal", best.key(), "reversal", c.key()
		}
	}
}
