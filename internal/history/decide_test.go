package history

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// decide records d and fails the test if it is refused.
func decide(t *testing.T, s *Store, d Decision) Decided {
	t.Helper()
	got, err := s.Decide(t.Context(), d)
	if err != nil {
		t.Fatalf("deciding %+v: %v", d, err)
	}
	return got
}

// entryRowOf returns columns of the entries view for ref, joined with
// spaces, NULL as "".
func entryRowOf(t *testing.T, s *Store, ref, columns string) string {
	t.Helper()
	cols := strings.Split(columns, ",")
	for i, c := range cols {
		cols[i] = "coalesce(" + strings.TrimSpace(c) + ", '')"
	}
	got := storeStrings(t, s, "SELECT "+strings.Join(cols, " || ' ' || ")+" FROM entries WHERE ref = '"+ref+"'")
	if len(got) != 1 {
		t.Fatalf("the entries view holds %d rows of %s", len(got), ref)
	}
	return got[0]
}

// The fixtures, recorded, make one counted entry each, booked as the
// rules say (TestClassifyFixtures); the entries, transactions and ledger
// views show them, and flag_def holds every flag. What the rules read of
// each transaction from the store's columns is what they read of its raw
// JSON. Read again, nothing is newly flagged.
func TestLedgerOfFixtures(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	recordFixtures(t, s)
	for query, want := range map[string]int64{
		"SELECT count(*) FROM entries":                                                14,
		"SELECT count(*) FROM transactions":                                           14,
		"SELECT count(*) FROM ledger":                                                 14,
		"SELECT count(*) FROM flag_def":                                               int64(len(flagDefs)),
		"SELECT sum(fees_cents) FROM entries":                                         69 + 4 + 67 + 282,
		"SELECT count(*) FROM entries WHERE amount_cents + fees_cents <> gross_cents": 0,
		"SELECT count(*) FROM entries WHERE category_rule = 'doi.dividend'":           2,
	} {
		if got := storeInt(t, s, query); got != want {
			t.Errorf("%s: %d, want %d", query, got, want)
		}
	}
	for ref, want := range map[string]string{
		"T73508261941": "sell trade.sell 31200 1 api",
		"T73508261946": "buy trade.reinvest -270 1 api",
		"T73508261949": "interest doi.interest 42 1 api",
		"T73508261950": "dividend doi.dividend 386 1 api",
		"T73508261954": "tax journal.w8 -116 1 api",
	} {
		if got := entryRowOf(t, s, ref, "category, category_rule, gross_cents, counted, counted_why"); got != want {
			t.Errorf("%s: %q, want %q", ref, got, want)
		}
	}
	// Names are dated when they were served: BND's, by a read 14 months
	// after its dividend, name no security then, but the account
	// reinvested the dividend in BND that day, so its ~BND is that
	// security, and so is its withholding's; XYZ's, the call's
	// deliverable's symbol, served 3 weeks after its dividend, names that
	// security by name.
	if got := entryRowOf(t, s, "T73508261950", "security_key, flags"); got != "i:4417302 " {
		t.Errorf("the dividend of BND: %q", got)
	}
	if got := entryRowOf(t, s, "T73508261954", "security_key, flags"); got != "i:4417302 " {
		t.Errorf("the withholding on BND: %q", got)
	}
	if got := entryRowOf(t, s, "T73508261948", "security_key, flags"); got != "i:1854092 security_by_name" {
		t.Errorf("the dividend of XYZ: %q", got)
	}
	if err := s.withWrite(t.Context(), "loading", func(w *wtx) error {
		g, err := loadGlobals(w)
		if err != nil {
			return err
		}
		for _, acct := range []int64{1, 2} {
			in, err := loadAccount(w, acct, g)
			if err != nil {
				return err
			}
			for _, x := range in.txns {
				var raw string
				if err := w.tx.QueryRowContext(w.ctx, "SELECT latest_raw FROM api_txn WHERE txn_id = ?", x.id).Scan(&raw); err != nil {
					return err
				}
				if got, want := classOf(x.typ, x.status, x.text, x.net, x.legs), recordClass(decodeTxn(raw)); !got.equal(want) {
					t.Errorf("transaction %d: the store's columns give %+v, its raw JSON %+v", x.id, got, want)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, number := range []string{individual, options} {
		rec := record(t, s, listRead(number, fixturesStart, fixturesEnd, day(2026, 11, 1), fixtureRows(t, number)...))
		if rec.Flagged != 0 {
			t.Errorf("reading %s again flagged %d entries", number, rec.Flagged)
		}
	}
}

// A category decision made against one revision of a transaction still
// applies after a revision that changes its status, and flags the entry
// stale_decision; deciding again clears it. A revision that only adds a
// key, and cut digests of another cut version, leave the decision fresh:
// what the rules read did not change.
func TestStaleDecision(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	start, end := at.Add(-72*time.Hour), at.Add(72*time.Hour)
	valid := cashRaw(1, at)
	pending := strings.Replace(valid, `"status": "VALID"`, `"status": "PENDING"`, 1)
	keyed := strings.Replace(pending, `"subAccount": "CASH",`, `"subAccount": "CASH", "qualifiedDividend": true,`, 1)
	read := func(i int, raw string) Recorded {
		return record(t, s, listRead(individual, start, end, end.Add(time.Duration(i)*time.Hour), decodeRow(t, raw)))
	}
	state := func() string { return entryRowOf(t, s, "T1", "category, category_rule, flags") }
	read(1, valid)
	d := decide(t, s, Decision{Kind: "category", Subject: "a:1:1", Value: "fee", Note: "a fee to 12345678"})
	if want := []EntryChange{{
		AccountID: 1, Ref: "T1",
		Before: EntryState{Category: "dividend", CategoryRule: "doi.dividend", Counted: true, CountedWhy: "api"},
		After:  EntryState{Category: "fee", CategoryRule: "user", Counted: true, CountedWhy: "api"},
	}}; !reflect.DeepEqual(d.Changes, want) {
		t.Errorf("the decision changed %+v, want %+v", d.Changes, want)
	}
	if got := storeStrings(t, s, "SELECT basis || ' ' || note FROM decision"); !slices.Equal(got, []string{`{"seq":1} a fee to ...678`}) {
		t.Errorf("the decision's basis and note: %q", got)
	}
	if got := state(); got != "fee user unresolved_security" {
		t.Errorf("decided: %q", got)
	}
	if rec := read(2, pending); rec.Flagged != 1 {
		t.Errorf("the revision of its status flagged %d entries, want 1", rec.Flagged)
	}
	if got := state(); got != "fee user revised,stale_decision,status,unresolved_security" {
		t.Errorf("after a revision of its status: %q", got)
	}
	decide(t, s, Decision{Kind: "category", Subject: "a:1:1", Value: "fee"})
	if got := state(); got != "fee user revised,status,unresolved_security" {
		t.Errorf("decided again: %q", got)
	}
	if rec := read(3, keyed); rec.Flagged != 0 {
		t.Errorf("a revision that adds a key flagged %d entries, already flagged", rec.Flagged)
	}
	if got := storeInt(t, s, "SELECT revisions FROM txn"); got != 3 {
		t.Fatalf("%d revisions, want 3", got)
	}
	if got := state(); got != "fee user revised,status,unresolved_security" {
		t.Errorf("after a revision that adds a key: %q", got)
	}
	// Cut digests as another cut version gives them.
	if err := s.withWrite(t.Context(), "cutting again", func(w *wtx) error {
		if _, err := w.exec("UPDATE revision_cut SET cut_version = 2, digest = randomblob(32)"); err != nil {
			return err
		}
		w.touch(1)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != "fee user revised,status,unresolved_security" {
		t.Errorf("under another cut version: %q", got)
	}
	decide(t, s, Decision{Kind: "category", Subject: "a:1:1", Value: "auto"})
	if got := state(); got != "dividend doi.dividend revised,status,unresolved_security" {
		t.Errorf("withdrawn: %q", got)
	}
}

// journalRaw returns the raw JSON of a JOURNAL of cash of amount dollars
// in the account whose number is number, with activity ID id, at at.
func journalRaw(id int64, number string, at time.Time, amount float64, description string) string {
	trade := midnight(schwab.DateInNewYork(at))
	return fmt.Sprintf(`{"activityId": %d, "time": %q, "description": %q, "accountNumber": %q, "type": "JOURNAL",
		"status": "VALID", "subAccount": "CASH", "tradeDate": %q, "settlementDate": %q, "netAmount": %v,
		"transferItems": [{"instrument": {"assetType": "CURRENCY", "status": "ACTIVE", "symbol": "CURRENCY_USD",
		"description": "USD currency", "instrumentId": 1, "closingPrice": 0.0}, "amount": %v, "cost": 0.0}]}`,
		id, schwabTime(at), description, number, schwabTime(trade), schwabTime(trade), amount, amount)
}

// Journals of opposite amounts in two of your accounts pair and are booked
// as journals. A journal you book as a deposit (from a spouse's account,
// say) stays a deposit and takes no part in automatic pairing, and the
// journal it would have paired with is booked by its sign and flagged; your
// pair decision pairs them and keeps your category. Withdrawing it, and
// your category, pairs them again by the rules.
func TestJournalPairsAndOverrides(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	start, end := at.Add(-72*time.Hour), at.Add(72*time.Hour)
	record(t, s, listRead(individual, start, end, end, decodeRow(t, journalRaw(1, individual, at, -500, "JOURNAL TO ...398"))))
	record(t, s, listRead(options, start, end, end, decodeRow(t, journalRaw(2, options, at.Add(time.Hour), 500, "JOURNAL FRM ...135"))))
	pairs := func() string {
		return entryRowOf(t, s, "T1", "category, pair_kind, pair_ref, flags") + " | " +
			entryRowOf(t, s, "T2", "category, category_rule, pair_kind, pair_ref, flags")
	}
	if got, want := pairs(), "journal journal T2  | journal journal.cash journal T1 "; got != want {
		t.Errorf("recorded: %q, want %q", got, want)
	}
	d := decide(t, s, Decision{Kind: "category", Subject: "a:2:2", Value: "deposit"})
	if got, want := pairs(), "withdrawal   unpaired_journal | deposit user   "; got != want {
		t.Errorf("T2 booked as a deposit: %q, want %q", got, want)
	}
	if len(d.Changes) != 2 || d.Changes[0].Ref != "T1" || d.Changes[0].After.PairRef != "" || d.Changes[1].Before.PairRef != "T1" {
		t.Errorf("the decision changed %+v", d.Changes)
	}
	decide(t, s, Decision{Kind: "pair", Subject: "a:2:2", Value: "a:1:1"})
	if got, want := pairs(), "journal journal T2  | deposit user journal T1 "; got != want {
		t.Errorf("paired by you: %q, want %q", got, want)
	}
	decide(t, s, Decision{Kind: "pair", Subject: "a:1:1", Value: "none"})
	if got, want := pairs(), "withdrawal    | deposit user   "; got != want {
		t.Errorf("unpaired by your newer decision: %q, want %q", got, want)
	}
	decide(t, s, Decision{Kind: "pair", Subject: "a:1:1", Value: "auto"})
	decide(t, s, Decision{Kind: "pair", Subject: "a:2:2", Value: "auto"})
	decide(t, s, Decision{Kind: "category", Subject: "a:2:2", Value: "auto"})
	if got, want := pairs(), "journal journal T2  | journal journal.cash journal T1 "; got != want {
		t.Errorf("every decision withdrawn: %q, want %q", got, want)
	}
	// What a rebuild derives is what the decisions left.
	before := pairs()
	if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if after := pairs(); after != before {
		t.Errorf("rebuilt: %q, want %q", after, before)
	}
}

// A pair that your decision made across accounts, of entries that pairing
// would not read by themselves (sweeps, a deposit and a withdrawal), is
// undone on both sides when the decision is withdrawn or names another
// entry, as a rebuild undoes it.
func TestPairDecisionsAcrossAccounts(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	start, end := at.Add(-72*time.Hour), at.Add(72*time.Hour)
	const third = "40221783"
	record(t, s, listRead(individual, start, end, end, decodeRow(t, journalRaw(1, individual, at, -500, "BANK SWEEP TO BROKERAGE"))))
	record(t, s, listRead(options, start, end, end, decodeRow(t, journalRaw(2, options, at.Add(time.Hour), 500, "BANK SWEEP TO BROKERAGE"))))
	record(t, s, listRead(third, start, end, end, decodeRow(t, journalRaw(3, third, at.Add(2*time.Hour), 500, "BANK SWEEP TO BROKERAGE"))))
	pairs := func() string {
		var out []string
		for _, ref := range []string{"T1", "T2", "T3"} {
			out = append(out, entryRowOf(t, s, ref, "pair_kind, pair_ref, flags"))
		}
		return strings.Join(out, " | ")
	}
	rebuilt := func(step string, pairs func() string) {
		t.Helper()
		before := pairs()
		if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
			t.Fatal(err)
		}
		if after := pairs(); after != before {
			t.Errorf("%s: recorded %q, rebuilt %q", step, before, after)
		}
	}
	for _, step := range []struct {
		value, want string
		changes     []string // the refs whose booking changed
	}{
		{"a:2:2", "sweep T2  | sweep T1  |   unpaired_sweep", []string{"T1", "T2"}},
		{"a:3:3", "sweep T3  |   unpaired_sweep | sweep T1 ", []string{"T1", "T2", "T3"}},
		{"auto", "  unpaired_sweep |   unpaired_sweep |   unpaired_sweep", []string{"T1", "T3"}},
	} {
		d := decide(t, s, Decision{Kind: "pair", Subject: "a:1:1", Value: step.value})
		if got := pairs(); got != step.want {
			t.Errorf("paired with %s: %q, want %q", step.value, got, step.want)
		}
		var changed []string
		for _, c := range d.Changes {
			changed = append(changed, c.Ref)
		}
		if !slices.Equal(changed, step.changes) {
			t.Errorf("paired with %s: the decision changed %q, want %q", step.value, changed, step.changes)
		}
		rebuilt("paired with "+step.value, pairs)
	}
	cash := func(id int64, number, typ string, amount float64) string {
		return strings.Replace(journalRaw(id, number, at.Add(3*time.Hour), amount, "ACH"), `"type": "JOURNAL"`, `"type": "`+typ+`"`, 1)
	}
	record(t, s, listRead(individual, start, end, end.Add(time.Hour), decodeRow(t, cash(4, individual, "ACH_RECEIPT", 300))))
	record(t, s, listRead(options, start, end, end.Add(time.Hour), decodeRow(t, cash(5, options, "ACH_DISBURSEMENT", -300))))
	booked := func() string {
		return entryRowOf(t, s, "T4", "category, pair_kind, pair_ref") + " | " + entryRowOf(t, s, "T5", "category, pair_kind, pair_ref")
	}
	for _, step := range []struct{ value, want string }{
		{"a:2:5", "journal journal T5 | journal journal T4"},
		{"auto", "deposit   | withdrawal  "},
	} {
		decide(t, s, Decision{Kind: "pair", Subject: "a:1:4", Value: step.value})
		if got := booked(); got != step.want {
			t.Errorf("the deposit paired with %s: %q, want %q", step.value, got, step.want)
		}
		rebuilt("the deposit paired with "+step.value, booked)
	}
}

// Decide refuses, with a sentence, a kind it does not take, a subject the
// store does not hold and a value the kind does not take, and records
// nothing; it records the rest, with what each changed: whether an entry
// counts, its security, an account's kind (masked), and that two keys are
// one security.
func TestDecide(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	recordFixtures(t, s)
	for _, c := range []struct {
		d    Decision
		want string
	}{
		{Decision{Kind: "match", Subject: "a:1:73508261946", Value: "none"}, `schwabtx takes no decision of kind "match"`},
		{Decision{Kind: "category", Subject: "a:1:1", Value: "fee"}, "no transaction a:1:1 in the store"},
		{Decision{Kind: "category", Subject: "a:1:73508261946", Value: "trade"}, `unknown category "trade"; one of deposit, withdrawal,`},
		{Decision{Kind: "count", Subject: "a:1:73508261946", Value: "maybe"}, `a count decision is yes, no or auto, not "maybe"`},
		{Decision{Kind: "pair", Subject: "a:1:73508261946", Value: "a:1:73508261946"}, "T73508261946 cannot be paired with itself"},
		{Decision{Kind: "pair", Subject: "a:1:73508261946", Value: "a:2:5"}, "no transaction a:2:5 in the store to pair T73508261946 with"},
		{Decision{Kind: "entry_security", Subject: "a:1:73508261946", Value: "k:i:5"}, "no instrument key K:i:5 in the store"},
		{Decision{Kind: "entry_security", Subject: "a:1:73508261946", Value: "i:4417302"}, "no instrument key i:4417302 in the store"},
		{Decision{Kind: "security", Subject: "k:i:5", Value: "auto"}, "no instrument key K:i:5 in the store"},
		{Decision{Kind: "security", Subject: "k:i:4417302", Value: "join k:i:4417355"}, `a security decision is same K…, differ K… or auto, not "join k:i:4417355"`},
		{Decision{Kind: "security", Subject: "k:i:4417302", Value: "same k:i:4417302"}, "K:i:4417302 cannot be decided same itself"},
		{Decision{Kind: "account", Subject: "acct:9", Value: "auto"}, "no account acct:9 in the store"},
		{Decision{Kind: "account", Subject: "acct:1", Value: "kind="}, `an account decision is kind=TEXT or auto, not "kind="`},
	} {
		if _, err := s.Decide(t.Context(), c.d); err == nil || !strings.HasPrefix(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.d, err, c.want)
		}
	}
	if n := storeInt(t, s, "SELECT count(*) FROM decision"); n != 0 {
		t.Errorf("refused decisions left %d in the log", n)
	}

	d := decide(t, s, Decision{Kind: "count", Subject: "a:1:73508261949", Value: "no"})
	if len(d.Changes) != 1 || d.Changes[0].Ref != "T73508261949" || d.Changes[0].After.Counted || d.Changes[0].After.CountedWhy != "user" {
		t.Errorf("count no changed %+v", d.Changes)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM ledger"); n != 13 {
		t.Errorf("the ledger holds %d entries after one is not counted, want 13", n)
	}
	d = decide(t, s, Decision{Kind: "entry_security", Subject: "a:2:73508261948", Value: "k:i:219834765"})
	if len(d.Changes) != 1 || d.Changes[0].After.SecurityKey != "i:219834765" {
		t.Errorf("entry_security changed %+v", d.Changes)
	}
	if got := entryRowOf(t, s, "T73508261948", "security_key, flags"); got != "i:219834765 " {
		t.Errorf("the dividend of XYZ, given a security: %q", got)
	}
	decide(t, s, Decision{Kind: "account", Subject: "acct:1", Value: "kind=ira 12345678"})
	if got := storeStrings(t, s, "SELECT kind FROM accounts WHERE account_id = 1"); !slices.Equal(got, []string{"ira ...678"}) {
		t.Errorf("the account's kind is %q", got)
	}
	decide(t, s, Decision{Kind: "security", Subject: "k:i:4417302", Value: "same k:i:4417355"})
	if a, b := securityOf(t, s, "i:4417302"), securityOf(t, s, "i:4417355"); a != b {
		t.Errorf("keys decided the same are in %s and %s", a, b)
	}
	decide(t, s, Decision{Kind: "account", Subject: "acct:1", Value: "auto"})
	if got := storeStrings(t, s, "SELECT coalesce(kind, 'none') FROM accounts WHERE account_id = 1"); !slices.Equal(got, []string{"none"}) {
		t.Errorf("the account's kind, withdrawn, is %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM decision"); n != 5 {
		t.Errorf("the log holds %d decisions, want 5", n)
	}
}

// A dividend's ~XYZ that no security holds is unresolved, until a read of
// another account brings a security that holds XYZ then: the dividend's
// account, which that read did not touch, is derived again, and its entry
// is what a rebuild gives it.
func TestSecurityFoundLater(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	start, end := at.Add(-72*time.Hour), at.Add(72*time.Hour)
	record(t, s, listRead(individual, start, end, end, cashRow(t, 1, at)))
	if got := entryRowOf(t, s, "T1", "security_key, flags"); got != " unresolved_security" {
		t.Fatalf("the dividend of XYZ: %q", got)
	}
	trade := fmt.Sprintf(`{"activityId": 2, "time": %q, "accountNumber": %q, "type": "TRADE", "status": "VALID",
		"subAccount": "CASH", "tradeDate": %q, "netAmount": -100.0, "orderId": 5, "transferItems": [
		{"instrument": {"assetType": "EQUITY", "status": "ACTIVE", "symbol": "XYZ", "instrumentId": 777,
		"closingPrice": 10.0}, "amount": 10, "cost": -100.0, "price": 10.0, "positionEffect": "OPENING"}]}`,
		schwabTime(at), options, schwabTime(at))
	record(t, s, listRead(options, start, end, end.Add(time.Hour), decodeRow(t, trade)))
	before := entryRowOf(t, s, "T1", "security_key, flags")
	if before != "i:777 " {
		t.Errorf("the dividend of XYZ, once a security holds XYZ: %q", before)
	}
	if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if after := entryRowOf(t, s, "T1", "security_key, flags"); after != before {
		t.Errorf("rebuilt: %q, want %q", after, before)
	}
}

// The rules read a description unmasked: a dividend of a delisted
// security, named by its CUSIP after the tilde, is a dividend of that
// symbol, though the description that the views show is masked.
func TestRulesReadUnmasked(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	raw := strings.Replace(cashRaw(1, at), "Ordinary Dividend~XYZ", "Ordinary Dividend~037833100", 1)
	record(t, s, listRead(individual, at.Add(-time.Hour), at.Add(time.Hour), at.Add(time.Hour), decodeRow(t, raw)))
	if got, want := entryRowOf(t, s, "T1", "category_rule, symbol, description"), "doi.dividend 037833100 Ordinary Dividend~...100"; got != want {
		t.Errorf("the dividend of a CUSIP: %q, want %q", got, want)
	}
}
