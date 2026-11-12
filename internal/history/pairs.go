package history

import (
	"cmp"
	"database/sql"
	"slices"
	"strings"
)

// A pairCandidate is an entry as pairing across accounts reads it: a
// journal of cash or of shares, or an entry that one of your pair
// decisions names.
type pairCandidate struct {
	key     entryKey
	subject string
	family  string
	amount  sql.NullInt64
	// quantity and security are those of a journal of shares.
	quantity, security sql.NullInt64
	day                string
	// text is its description unmasked, and number its account's number,
	// whose last three digits a journal's description may name.
	text, number string
	// userCategory says that your category decision booked it, and
	// uncounted that it does not count; either keeps it out of automatic
	// pairing.
	userCategory, uncounted bool
}

// A pairResult is what pairAccounts makes of a candidate: the entry it is
// paired with and how, or the flag of a journal left unpaired.
type pairResult struct {
	kind string
	with entryKey
	flag string // unpaired_journal or ambiguous_journal
	// decided says that one of your pair decisions names it, and none that
	// the latest of yours on it says it has no pair.
	decided, none bool
}

// pairAccounts pairs entries across accounts (13.5 of the design), a
// function of the candidates and your decisions whatever their order.
// Your pair decisions come first, the latest of each entry's holding, the
// newer before the older: one pairs its entry with the entry it names,
// when both are there and neither is paired by a newer one; "none" keeps
// its entry unpaired. An entry that a decision names takes no part in
// automatic pairing. Then a journal pairs with a journal of another of
// your accounts, of the same kind (cash or shares), at most a day apart,
// with the opposite amount (cash) or the opposite quantity of the same
// security (shares), when each is the other's only candidate; among
// several, the one whose description names the other's account by its
// last three digits, if one alone does. A journal booked by your category
// decision, or one that does not count, takes no part. A journal left unpaired is flagged
// unpaired_journal, or ambiguous_journal when it had candidates.
func pairAccounts(cands []pairCandidate, decisions []decisionInput) map[entryKey]pairResult {
	slices.SortFunc(cands, func(a, b pairCandidate) int {
		return cmp.Or(cmp.Compare(a.key.account, b.key.account), cmp.Compare(a.key.ref, b.key.ref))
	})
	bySubject := map[string]int{}
	for i, c := range cands {
		bySubject[c.subject] = i
	}
	res := make([]pairResult, len(cands))
	var live []decisionInput
	for _, d := range latestDecisions(decisions) {
		if d.kind == "pair" {
			live = append(live, d)
		}
	}
	slices.SortFunc(live, func(a, b decisionInput) int { return cmp.Compare(b.id, a.id) })
	settled := map[string]bool{}
	for _, d := range live {
		a, okA := bySubject[d.subject]
		if okA {
			res[a].decided = true
		}
		b, okB := bySubject[d.value]
		if okB {
			res[b].decided = true
		}
		if settled[d.subject] {
			continue
		}
		settled[d.subject] = true
		if !okA {
			continue
		}
		if d.value == "none" {
			res[a].none = true
			continue
		}
		if !okB || settled[d.value] || a == b {
			continue
		}
		settled[d.value] = true
		kind := "journal"
		switch {
		case cands[a].family == "sweep" && cands[b].family == "sweep":
			kind = "sweep"
		case cands[a].family == "reversal" || cands[b].family == "reversal":
			kind = "reversal"
		}
		res[a].kind, res[a].with = kind, cands[b].key
		res[b].kind, res[b].with = kind, cands[a].key
	}

	// Automatic journal pairs.
	auto := func(i int) bool {
		c := cands[i]
		return !res[i].decided && !c.userCategory && !c.uncounted && (c.family == "journal" || c.family == "shares")
	}
	matches := func(a, b pairCandidate) bool {
		d, ok := daysApart(a.day, b.day)
		if a.key.account == b.key.account || a.family != b.family || !ok || d < -1 || d > 1 {
			return false
		}
		if a.family == "journal" {
			return a.amount.Valid && b.amount.Valid && a.amount.Int64 != 0 && a.amount.Int64 == -b.amount.Int64
		}
		return a.quantity.Valid && b.quantity.Valid && a.quantity.Int64 != 0 && a.quantity.Int64 == -b.quantity.Int64 &&
			a.security.Valid && a.security == b.security
	}
	options := make([][]int, len(cands))
	for i := range cands {
		if !auto(i) {
			continue
		}
		for j := range cands {
			if j != i && auto(j) && matches(cands[i], cands[j]) {
				options[i] = append(options[i], j)
			}
		}
	}
	// effective narrows several candidates to the one that a description
	// names, when one alone is.
	effective := func(i int) []int {
		if len(options[i]) < 2 {
			return options[i]
		}
		var named []int
		for _, j := range options[i] {
			if names(cands[i].text, cands[j].number) || names(cands[j].text, cands[i].number) {
				named = append(named, j)
			}
		}
		if len(named) == 1 {
			return named
		}
		return options[i]
	}
	for i := range cands {
		if !auto(i) {
			continue
		}
		if e := effective(i); len(e) == 1 {
			if f := effective(e[0]); len(f) == 1 && f[0] == i {
				res[i].kind, res[i].with = "journal", cands[e[0]].key
				continue
			}
		}
		res[i].flag = "unpaired_journal"
		if len(options[i]) > 0 {
			res[i].flag = "ambiguous_journal"
		}
	}
	out := make(map[entryKey]pairResult, len(cands))
	for i, c := range cands {
		r := res[i]
		// A journal that your decision meant to pair, with an entry that is
		// gone or paired by a newer decision, has no counterpart.
		if r.decided && !r.none && r.with == (entryKey{}) && !c.userCategory && (c.family == "journal" || c.family == "shares") {
			r.flag = "unpaired_journal"
		}
		out[c.key] = r
	}
	return out
}

// names reports whether text names an account whose number is number: a
// run of digits in it, three or more, ends with the number's last three,
// as "JOURNAL TO ...123" or "JOURNAL TO 12345123" does.
func names(text, number string) bool {
	if len(number) < 3 {
		return false
	}
	last := number[len(number)-3:]
	for run := range strings.FieldsFuncSeq(text, func(r rune) bool { return r < '0' || r > '9' }) {
		if len(run) >= 3 && strings.HasSuffix(run, last) {
			return true
		}
	}
	return false
}

// finishPairs applies to an account's entries what pairAccounts made of
// them, and flags what pairing left: a journal paired by it is booked as a
// journal, unless your category says otherwise; a sweep, or a cancel,
// without its other half is flagged unless your decision says it has none;
// a cancel takes the category of the trade it cancels, and both are
// flagged reversal_signs when their amounts have one sign, since their
// cash then counts twice.
func finishPairs(entries []ledgerEntry, res map[entryKey]pairResult) {
	byKey := map[entryKey]*ledgerEntry{}
	for i := range entries {
		byKey[entries[i].key()] = &entries[i]
	}
	for i := range entries {
		e := &entries[i]
		r, ok := res[e.key()]
		if ok && (r.decided || e.family == "journal" || e.family == "shares") {
			e.pairKind, e.pair = r.kind, r.with
			if r.flag != "" {
				e.flags.add(r.flag, "")
			}
		}
		if e.pairKind == "journal" && !e.userCategory {
			e.category = "journal"
		}
		switch {
		case e.pairKind == "" && e.family == "sweep" && !r.none && e.counted:
			e.flags.add("unpaired_sweep", "")
		case e.pairKind == "" && e.family == "reversal" && !r.none && e.counted:
			e.flags.add("unpaired_reversal", "")
		case e.pairKind == "reversal":
			o := byKey[e.pair]
			if o == nil {
				break
			}
			if e.family == "reversal" && o.family != "reversal" && !e.userCategory {
				e.category = o.category
			}
			if s := signOf(e.amount); s != zero && s != unknownSign && s == signOf(o.amount) {
				e.flags.add("reversal_signs", "")
			}
		}
	}
}
