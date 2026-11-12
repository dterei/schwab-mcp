package history

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// flagsOf returns an entry's flags as "code detail", in the order of
// their codes.
func flagsOf(e ledgerEntry) []string {
	var out []string
	for _, f := range e.flags.sorted() {
		out = append(out, strings.TrimSpace(f.code+" "+f.detail))
	}
	return out
}

// entryOf returns the entry of ref among entries.
func entryOf(t *testing.T, entries []ledgerEntry, ref string) ledgerEntry {
	t.Helper()
	for _, e := range entries {
		if e.ref == ref {
			return e
		}
	}
	t.Fatalf("no entry %s among %d", ref, len(entries))
	return ledgerEntry{}
}

// The entries of an account's API transactions, on values: the flags that
// every row gets (13.3 of the design), from the legs' cash, the numbers
// the store rounded or could not keep, the record's problems, the status,
// the activity ID, the account named, an answer that held the row twice,
// revisions and gone; counting, by your decision, then gone, then INVALID;
// your category, which answers the flags that say how the rule booked it
// for want of better; your entry_security; refs of their own for two rows
// without an activity ID; and decisions stale when what the rules read of
// the revision they were made against is not what they read now. The
// entries do not depend on the order of the inputs.
func TestEntries(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 5, 15, 0, 0, 0, time.UTC)
	row := func(id int64, edit func(string) string) schwab.Transaction {
		return decodeRow(t, edit(cashRaw(id, at)))
	}
	same := func(s string) string { return s }
	swap := func(old, with string) func(string) string {
		return func(s string) string {
			if !strings.Contains(s, old) {
				t.Fatalf("no %q in %s", old, s)
			}
			return strings.Replace(s, old, with, 1)
		}
	}
	noID := row(0, same)
	in := accountInputs{account: 3, txns: []txnInput{
		recordInput(t, 1, row(1, same)),
		recordInput(t, 2, row(2, swap(`"netAmount": 1.0`, `"netAmount": 2.0`))),
		recordInput(t, 3, row(3, swap(`"transferItems": [{`, `"transferItems": [], "x": [{`))),
		recordInput(t, 4, row(4, swap(`"netAmount": 1.0`, `"netAmount": 81.29223999999999`))),
		recordInput(t, 5, row(5, func(s string) string {
			return swap(`"netAmount": 1.0`, `"netAmount": 0.3`)(swap(`"amount": 1.0`, `"amount": 0.30000000000000004`)(s))
		})),
		recordInput(t, 6, row(6, swap(`"status": "VALID"`, `"status": "PENDING"`))),
		recordInput(t, 7, row(7, swap(`"status": "VALID"`, `"status": "INVALID"`))),
		recordInput(t, 8, noID),
		recordInput(t, 9, noID),
		recordInput(t, 10, row(10, swap(individual, "12345678"))),
		recordInput(t, 11, row(11, swap(`"subAccount": "CASH"`, `"subAccount": 5`))),
		recordInput(t, 12, row(12, same)), // revised, gone, counted by you
		recordInput(t, 13, row(13, swap(`"DIVIDEND_OR_INTEREST"`, `"MEMORANDUM"`))),
		recordInput(t, 14, row(14, same)), // its ~XYZ resolved by name
		recordInput(t, 15, row(15, same)), // your entry_security
		recordInput(t, 16, row(16, same)), // decided against another revision
		recordInput(t, 17, row(17, same)), // twice in an answer
	}}
	in.txns[16].duplicate = true
	// The row names another account than the one read.
	in.txns[9].numberSent = "...678"
	in.txns[10].problems = []txnProblem{{Path: "subAccount", What: "a number"}}
	in.txns[11].digests = [][32]byte{{1}, {2}, {2}, {3}}
	in.txns[11].changed = [][]string{nil, {"status"}, {"x"}, {"qualifiedDividend", "status"}}
	in.txns[12].text = "Removed due to Expiration XYZ"
	in.txns[15].current = recordClass(row(16, same))
	in.txns[15].bases = map[int]apiClass{1: recordClass(row(16, swap(`"status": "VALID"`, `"status": "PENDING"`))), 2: in.txns[15].current}
	in.txns[14].current = recordClass(row(15, same))
	in.txns[14].bases = map[int]apiClass{1: in.txns[14].current}
	in.reads = []readInput{{id: 4, start: at.Add(-48 * time.Hour), end: at.Add(48 * time.Hour), sent: at.Add(72 * time.Hour)}}
	in.absences = []absence{{4, 12}}
	// XYZ was last held more than a year before.
	in.names = []symbolName{{security: 70, value: "XYZ", first: at.AddDate(-3, 0, 0), last: at.AddDate(-1, 0, -1)}}
	in.keySecurities = map[string]int64{"i:1": 71}
	decide := func(id int64, kind, subject, value string, seq int) decisionInput {
		return decisionInput{id: id, kind: kind, subject: subject, value: value, basisSeq: seq, basisValid: seq > 0}
	}
	in.decisions = []decisionInput{
		decide(1, "count", "a:3:12", "yes", 0),
		decide(2, "category", "a:3:13", "fee", 0),
		decide(3, "category", "a:3:1", "tax", 0),
		decide(4, "category", "a:3:1", "auto", 0), // withdrawn
		decide(5, "count", "a:3:6", "no", 0),
		decide(6, "entry_security", "a:3:15", "k:i:1", 1),
		decide(7, "category", "a:3:16", "interest", 1),
		decide(8, "category", "a:3:16", "dividend", 2), // the latest holds
		decide(9, "count", "a:3:16", "yes", 1),
		decide(10, "account", "acct:3", "kind=ira 12345678", 0),
	}
	out := deriveAccount(in)
	type want struct {
		category, rule string
		counted        string
		flags          []string
	}
	unresolved := "unresolved_security XYZ"
	for ref, w := range map[string]want{
		"T1":  {"dividend", "doi.dividend", "1 api", []string{unresolved}},
		"T2":  {"dividend", "doi.dividend", "1 api", []string{"legs_mismatch the legs' cash is 1.00, the net amount 2.00", unresolved}},
		"T3":  {"dividend", "doi.dividend", "1 api", []string{"no_legs", unresolved}},
		"T4":  {"dividend", "doi.dividend", "1 api", []string{"inexact netAmount 81.29223999999999", unresolved}},
		"T5":  {"dividend", "doi.dividend", "1 api", []string{"rounded transferItems[0].amount 0.30000000000000004", unresolved}},
		"T6":  {"dividend", "doi.dividend", "0 user", []string{"status PENDING", unresolved}},
		"T7":  {"dividend", "doi.dividend", "0 invalid", []string{"status INVALID", unresolved}},
		"T10": {"dividend", "doi.dividend", "1 api", []string{"account_mismatch the row names ...678", unresolved}},
		"T11": {"dividend", "doi.dividend", "1 api", []string{"problems subAccount", unresolved}},
		"T12": {"dividend", "doi.dividend", "1 user", []string{
			"gone left out by the complete read sent 2026-10-08T15:00:00.000Z",
			"revised qualifiedDividend, status", unresolved,
		}},
		"T13": {"fee", "user", "1 api", nil},
		"T14": {"dividend", "doi.dividend", "1 api", []string{unresolved}},
		"T15": {"dividend", "doi.dividend", "1 api", nil},
		"T16": {"dividend", "user", "1 user", []string{"stale_decision count decided against revision 1", unresolved}},
		"T17": {"dividend", "doi.dividend", "1 api", []string{"duplicate_in_answer", unresolved}},
	} {
		e := entryOf(t, out.entries, ref)
		got := want{e.category, e.rule, fmt.Sprintf("%d %s", map[bool]int{true: 1}[e.counted], e.countedWhy), flagsOf(e)}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("%s: %+v, want %+v", ref, got, w)
		}
	}
	if e := entryOf(t, out.entries, "T4"); e.amount.Valid || e.gross.Valid {
		t.Errorf("T4, off the grid, has amount %v and gross %v", e.amount, e.gross)
	}
	if e := entryOf(t, out.entries, "T15"); e.security != centsOf(71) {
		t.Errorf("T15's security is %v, want your decision's 71", e.security)
	}
	if e := entryOf(t, out.entries, "T13"); e.family != "other" {
		t.Errorf("T13, a memorandum booked by you, is of family %s", e.family)
	}
	if out.kind != (sql.NullString{String: "ira ...678", Valid: true}) {
		t.Errorf("the account's kind is %v", out.kind)
	}
	// The two rows without an activity ID take refs of their own, and
	// are flagged.
	refs := txnRefs(3, in.txns)
	if a, b := refs[8], refs[9]; !strings.HasPrefix(a.ref, "T~20261005T150000.") || b.ref != a.ref+".2" || b.subject != "a:3:"+b.ref[1:] {
		t.Errorf("the rows without an activity ID have refs %+v and %+v", a, b)
	}
	if e := entryOf(t, out.entries, refs[9].ref); !slices.Contains(flagsOf(e), "no_activity_id") {
		t.Errorf("%s is flagged %q", e.ref, flagsOf(e))
	}
	// Held 30 days before, XYZ names its security by name; 3 days
	// before, plainly.
	in.names[0].last = at.AddDate(0, 0, -30)
	if e := entryOf(t, deriveAccount(in).entries, "T14"); e.security != centsOf(70) || !slices.Equal(flagsOf(e), []string{"security_by_name XYZ last seen 2026-09-05"}) {
		t.Errorf("T14, by name: security %v, flags %q", e.security, flagsOf(e))
	}
	in.names[0].last = at.AddDate(0, 0, -3)
	if e := entryOf(t, deriveAccount(in).entries, "T14"); e.security != centsOf(70) || len(e.flags) != 0 {
		t.Errorf("T14, near: security %v, flags %q", e.security, flagsOf(e))
	}

	// In any order, the same entries.
	want0 := deriveAccount(in).entries
	r := rand.New(rand.NewPCG(3, 4))
	for range 10 {
		sh := in
		sh.txns, sh.decisions = slices.Clone(in.txns), slices.Clone(in.decisions)
		r.Shuffle(len(sh.txns), func(i, j int) { sh.txns[i], sh.txns[j] = sh.txns[j], sh.txns[i] })
		r.Shuffle(len(sh.decisions), func(i, j int) { sh.decisions[i], sh.decisions[j] = sh.decisions[j], sh.decisions[i] })
		if got := deriveAccount(sh).entries; !reflect.DeepEqual(got, want0) {
			t.Fatalf("with its inputs in another order, the entries differ")
		}
	}
}

// pairEntry returns an entry of account 1 for the tests of pairs.
func pairEntry(ref, family, category, day string, amount int64) ledgerEntry {
	e := ledgerEntry{account: 1}
	e.ref, e.subject = ref, "a:1:"+ref
	e.family, e.category, e.rule = family, category, "test"
	e.day = sql.NullString{String: day, Valid: true}
	e.amount = centsOf(amount)
	return e
}

// pairsOf returns each entry's pair, flags and category, by ref.
func pairsOf(entries []ledgerEntry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		out[fmt.Sprintf("%d:%s", e.account, e.ref)] = strings.TrimSpace(fmt.Sprintf("%s %s %d:%s %s",
			e.category, e.pairKind, e.pair.account, e.pair.ref, strings.Join(flagsOf(e), ",")))
	}
	return out
}

// Pairs within an account (13.5 of the design): sweeps of opposite amounts
// a day apart at most, and a cancel with the trade it cancels, posted up
// to 5 days before with equal amounts and opposite quantities; the cancel
// takes the trade's category, and both are flagged when their amounts
// have one sign. What is left unpaired is flagged, unless your decision
// says it has no pair; an entry your decision names is left to it.
func TestPairWithin(t *testing.T) {
	t.Parallel()
	qty := func(e ledgerEntry, q int64, sym string) ledgerEntry {
		e.quantity, e.symbol = centsOf(q), sql.NullString{String: sym, Valid: true}
		e.posted = e.day
		return e
	}
	entries := []ledgerEntry{
		pairEntry("S1", "sweep", "other", "2026-10-01", -500),
		pairEntry("S2", "sweep", "other", "2026-10-02", 500),
		pairEntry("S3", "sweep", "other", "2026-10-02", 700),
		pairEntry("S4", "sweep", "other", "2026-10-04", -700), // two days after S3
		pairEntry("S5", "sweep", "other", "2026-10-04", 0),
		pairEntry("S6", "sweep", "other", "2026-10-05", -900), // yours: none
		pairEntry("S7", "sweep", "other", "2026-10-05", 900),
		qty(pairEntry("B1", "buy", "dividend", "2026-10-01", -1000), 10, "XYZ"),
		qty(pairEntry("C1", "reversal", "buy", "2026-10-04", 1000), -10, "XYZ"),
		qty(pairEntry("B2", "sell", "sell", "2026-10-01", 2000), -20, "ABC"),
		qty(pairEntry("C2", "reversal", "sell", "2026-10-03", 2000), 20, "ABC"), // its Amount has the sale's sign
		qty(pairEntry("B3", "buy", "buy", "2026-10-01", -1000), 10, "QQQ"),
		qty(pairEntry("C3", "reversal", "buy", "2026-10-07", 1000), -10, "QQQ"), // 6 days after
		qty(pairEntry("C4", "reversal", "buy", "2026-10-07", 1000), -10, "QQQ"), // yours: none
		qty(pairEntry("B5", "buy", "buy", "2026-09-28", -1000), 10, "XYZ"),      // 6 days before C1
	}
	for i := range entries {
		entries[i].pairDecided = entries[i].ref == "C4" || entries[i].ref == "S6"
	}
	pairWithin(entries)
	finishPairs(entries, map[entryKey]pairResult{{1, "C4"}: {decided: true, none: true}, {1, "S6"}: {decided: true, none: true}})
	want := map[string]string{
		"1:S1": "other sweep 1:S2",
		"1:S2": "other sweep 1:S1",
		"1:S3": "other  0: unpaired_sweep",
		"1:S4": "other  0: unpaired_sweep",
		"1:S5": "other  0: unpaired_sweep",
		"1:S6": "other  0:",
		"1:S7": "other  0: unpaired_sweep",
		"1:B1": "dividend reversal 1:C1",
		"1:C1": "dividend reversal 1:B1",
		"1:B2": "sell reversal 1:C2 reversal_signs",
		"1:C2": "sell reversal 1:B2 reversal_signs",
		"1:B3": "buy  0:",
		"1:C3": "buy  0: unpaired_reversal",
		"1:C4": "buy  0:",
		"1:B5": "buy  0:",
	}
	if got := pairsOf(entries); !reflect.DeepEqual(got, want) {
		t.Errorf("pairs:\n%v\nwant\n%v", got, want)
	}
}

// candidate returns a journal of cash as pairAccounts reads it.
func candidate(account int64, ref, day string, amount int64, text string) pairCandidate {
	return pairCandidate{
		key: entryKey{account, ref}, subject: fmt.Sprintf("a:%d:%s", account, ref), family: "journal",
		amount: centsOf(amount), day: day, text: text, number: fmt.Sprintf("1000%04d", account*111),
	}
}

// Journals pair across accounts (13.5 of the design) when each is the
// other's only candidate: another account's journal of the same kind, a
// day apart at most, with the opposite amount, or the opposite quantity of
// the same security; among several, the one a description names by its
// account's last three digits. Your category keeps a journal out; your pair
// decisions come first, the newer before the older, and "none" keeps an
// entry unpaired. The result does not depend on the order of the
// candidates.
func TestPairAccounts(t *testing.T) {
	t.Parallel()
	shares := func(c pairCandidate, q, sec int64) pairCandidate {
		c.family, c.amount, c.quantity, c.security = "shares", sql.NullInt64{}, centsOf(q), centsOf(sec)
		return c
	}
	user := func(c pairCandidate) pairCandidate {
		c.userCategory = true
		return c
	}
	cands := []pairCandidate{
		// One to one, a day apart.
		candidate(1, "J1", "2026-10-01", -500, "JOURNAL TO ...222"),
		candidate(2, "J2", "2026-10-02", 500, ""),
		// Three of 700: 1's names account 3 (number 10000333), so 1 and 3
		// pair; 4 had a candidate, which is taken.
		candidate(1, "K1", "2026-10-05", -700, "JOURNAL TO 10000333"),
		candidate(3, "K3", "2026-10-05", 700, ""),
		candidate(4, "K4", "2026-10-06", 700, ""),
		// Two of 900 into one, neither named: all three ambiguous.
		candidate(1, "L1", "2026-10-07", -900, ""),
		candidate(2, "L2", "2026-10-07", 900, ""),
		candidate(3, "L3", "2026-10-07", 900, ""),
		// Same account, two days apart, zero: none pair.
		candidate(1, "M1", "2026-10-10", -300, ""),
		candidate(1, "M2", "2026-10-10", 300, ""),
		candidate(2, "N1", "2026-10-12", -400, ""),
		candidate(3, "N3", "2026-10-14", 400, ""),
		candidate(2, "Z2", "2026-10-15", 0, ""),
		candidate(3, "Z3", "2026-10-15", 0, ""),
		// Shares of one security pair; of another, not.
		shares(candidate(1, "H1", "2026-10-16", 0, ""), -5_000_000, 9),
		shares(candidate(2, "H2", "2026-10-16", 0, ""), 5_000_000, 9),
		shares(candidate(3, "H3", "2026-10-16", 0, ""), 5_000_000, 8),
		// Your category keeps U1 out; U2 is left without a counterpart.
		user(candidate(1, "U1", "2026-10-18", -600, "")),
		candidate(2, "U2", "2026-10-18", 600, ""),
		// Your decisions: P1 with P3, though P2 matches it; Q1 none; R1 with
		// R2, but the newer decision on R3 pairs R2 with R3; V1 with an
		// entry that is gone.
		user(candidate(1, "P1", "2026-10-20", -800, "")),
		candidate(2, "P2", "2026-10-20", 800, ""),
		candidate(3, "P3", "2026-11-20", 1, ""),
		candidate(1, "Q1", "2026-10-22", -100, ""),
		candidate(2, "Q2", "2026-10-22", 100, ""),
		candidate(1, "R1", "2026-10-24", -1, ""),
		candidate(2, "R2", "2026-10-24", -2, ""),
		candidate(3, "R3", "2026-10-24", -3, ""),
		candidate(1, "V1", "2026-10-26", -5, ""),
	}
	cands[21].family = "sweep" // P3: an explicit pair of another kind is a journal all the same
	decisions := []decisionInput{
		{id: 1, kind: "pair", subject: "a:1:P1", value: "a:3:P3"},
		{id: 2, kind: "pair", subject: "a:1:Q1", value: "none"},
		{id: 3, kind: "pair", subject: "a:1:R1", value: "a:2:R2"},
		{id: 4, kind: "pair", subject: "a:3:R3", value: "a:2:R2"},
		{id: 5, kind: "pair", subject: "a:1:V1", value: "a:9:V9"},
		{id: 6, kind: "pair", subject: "a:2:U2", value: "a:1:U1"},
		{id: 7, kind: "pair", subject: "a:2:U2", value: "auto"}, // withdrawn
		{id: 8, kind: "category", subject: "a:2:Q2", value: "fee"},
	}
	type got struct {
		kind, with, flag string
		decided, none    bool
	}
	summary := func(res map[entryKey]pairResult) map[string]got {
		out := map[string]got{}
		for k, r := range res {
			with := ""
			if r.with != (entryKey{}) {
				with = fmt.Sprintf("%d:%s", r.with.account, r.with.ref)
			}
			out[fmt.Sprintf("%d:%s", k.account, k.ref)] = got{r.kind, with, r.flag, r.decided, r.none}
		}
		return out
	}
	want := map[string]got{
		"1:J1": {"journal", "2:J2", "", false, false}, "2:J2": {"journal", "1:J1", "", false, false},
		"1:K1": {"journal", "3:K3", "", false, false}, "3:K3": {"journal", "1:K1", "", false, false},
		"4:K4": {"", "", "ambiguous_journal", false, false},
		"1:L1": {"", "", "ambiguous_journal", false, false}, "2:L2": {"", "", "ambiguous_journal", false, false},
		"3:L3": {"", "", "ambiguous_journal", false, false},
		"1:M1": {"", "", "unpaired_journal", false, false}, "1:M2": {"", "", "unpaired_journal", false, false},
		"2:N1": {"", "", "unpaired_journal", false, false}, "3:N3": {"", "", "unpaired_journal", false, false},
		"2:Z2": {"", "", "unpaired_journal", false, false}, "3:Z3": {"", "", "unpaired_journal", false, false},
		"1:H1": {"journal", "2:H2", "", false, false}, "2:H2": {"journal", "1:H1", "", false, false},
		"3:H3": {"", "", "unpaired_journal", false, false},
		"1:U1": {"", "", "", false, false}, "2:U2": {"", "", "unpaired_journal", false, false},
		"1:P1": {"journal", "3:P3", "", true, false}, "3:P3": {"journal", "1:P1", "", true, false},
		"2:P2": {"", "", "unpaired_journal", false, false},
		"1:Q1": {"", "", "", true, true}, "2:Q2": {"", "", "unpaired_journal", false, false},
		"1:R1": {"", "", "unpaired_journal", true, false}, "2:R2": {"journal", "3:R3", "", true, false},
		"3:R3": {"journal", "2:R2", "", true, false},
		"1:V1": {"", "", "unpaired_journal", true, false},
	}
	if res := summary(pairAccounts(slices.Clone(cands), decisions)); !reflect.DeepEqual(res, want) {
		for k, w := range want {
			if res[k] != w {
				t.Errorf("%s: %+v, want %+v", k, res[k], w)
			}
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	for range 10 {
		sh, ds := slices.Clone(cands), slices.Clone(decisions)
		r.Shuffle(len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
		r.Shuffle(len(ds), func(i, j int) { ds[i], ds[j] = ds[j], ds[i] })
		if res := summary(pairAccounts(sh, ds)); !reflect.DeepEqual(res, want) {
			t.Fatalf("with the candidates in another order, the pairs differ")
		}
	}

	// finishPairs books a paired journal as a journal, unless your category
	// says otherwise, and flags what is left.
	entries := []ledgerEntry{
		pairEntry("J1", "journal", "withdrawal", "2026-10-01", -500),
		pairEntry("U1", "journal", "deposit", "2026-10-18", -600),
		pairEntry("P1", "journal", "deposit", "2026-10-20", -800),
		pairEntry("M1", "journal", "withdrawal", "2026-10-10", -300),
	}
	entries[1].userCategory, entries[2].userCategory = true, true
	finishPairs(entries, pairAccounts(slices.Clone(cands), decisions))
	if got, want := pairsOf(entries), map[string]string{
		"1:J1": "journal journal 2:J2",
		"1:U1": "deposit  0:",
		"1:P1": "deposit journal 3:P3",
		"1:M1": "withdrawal  0: unpaired_journal",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("finished:\n%v\nwant\n%v", got, want)
	}

	for text, number := range map[string]string{"JOURNAL TO ...123": "45678123", "JOURNAL TO 45678123": "45678123", "TRANSFER 1123 SHS": "45678123"} {
		if !names(text, number) {
			t.Errorf("%q does not name %s", text, number)
		}
	}
	for text, number := range map[string]string{"JOURNAL TO ...124": "45678123", "JOURNAL TO 12 3": "45678123", "": "45678123", "123": "12"} {
		if names(text, number) {
			t.Errorf("%q names %s", text, number)
		}
	}
}
