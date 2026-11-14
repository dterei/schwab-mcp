package schwab

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// maskerTables are the masker's key classes of the records whose raw JSON
// a tool may show, by the scrub policy's table for the same records.
var maskerTables = map[*table]struct {
	classOf func([]string) mask.Class
	classes *keyClasses
}{
	transactionTable: {TransactionKeyClass, transactionKeyClasses},
}

// allRules returns the rules of t and of the tables nested in it, each at
// its path within t's records, after prefix: as rulesOf does, but a table
// nested in itself is followed once more, so that an instrument's
// deliverable's rules are among them.
func allRules(t *table, prefix []string, depth map[*table]int) []rule {
	depth[t]++
	defer func() { depth[t]-- }()
	var rs []rule
	for _, r := range t.rules {
		r.keys = slices.Concat(prefix, r.keys)
		r.path = strings.Join(r.keys, ".")
		rs = append(rs, r)
	}
	for _, n := range t.nested {
		if depth[n.table] < 2 {
			rs = append(rs, allRules(n.table, slices.Concat(prefix, n.keys), depth)...)
		}
	}
	return rs
}

// underUser reports whether path is at a record's user object or under it.
func underUser(path []string) bool {
	return len(path) > 0 && strings.EqualFold(path[0], "user")
}

// meant returns the classes that the masker may give the values at r's
// path, by what the scrub policy's rule says they mean, and what that is.
// The cases are tried in order, the first that applies deciding; a user
// object's values, withheld, come before all of them, in keyClassProblems.
func meant(r rule) ([]mask.Class, string) {
	switch {
	case r.class == classMap && r.id == idAccountNumber:
		return []mask.Class{mask.AccountNumber}, "an account number"
	case keysMatch(r.keys, []string{"tag"}):
		return []mask.Class{mask.Tag}, "an order's tag"
	case r.class == classKeep && r.gram == nameGrammar:
		return []mask.Class{mask.FreeText}, "a security's name, free text"
	case r.class == classPlaceholder:
		return []mask.Class{mask.FreeText, mask.Tag}, "free text"
	case r.class == classMap:
		return []mask.Class{mask.Identifier, mask.Hash}, "an identifier"
	case r.class == classScale:
		return []mask.Class{mask.Amount}, "money or a quantity"
	case r.class == classKeep:
		return []mask.Class{mask.Identifier, mask.Time, mask.Amount}, r.gram.name
	}
	return nil, "a rule of no class"
}

// keyClassProblems returns where classOf, the masker's key classes of a
// kind of record, disagrees with tab, the scrub policy's table for it, by
// what each value means: a value at a user object or under it, whatever its
// rule, or one that the masker withholds, is withheld, before anything
// else, so that no rule can demand that a withheld value be shown; any
// other has a class that meant allows for its rule.
func keyClassProblems(tab *table, classOf func([]string) mask.Class) []string {
	var problems []string
	for _, r := range allRules(tab, nil, map[*table]int{}) {
		got := classOf(r.keys)
		if underUser(r.keys) || got == mask.Withheld {
			if got != mask.Withheld {
				problems = append(problems, r.path+": a value of a user object, which must be withheld, has the masker's class "+className(got))
			}
			continue
		}
		if want, what := meant(r); !slices.Contains(want, got) {
			problems = append(problems, r.path+": "+what+", which the masker gives the class "+className(got))
		}
	}
	return problems
}

// className returns c's name, as the tests say it.
func className(c mask.Class) string {
	names := []string{"Unknown", "AccountNumber", "Hash", "Identifier", "Time", "Amount", "FreeText", "Tag", "Withheld"}
	if int(c) < len(names) {
		return names[c]
	}
	return "a class out of range"
}

// unruled returns the paths that classes, the masker's key classes of a
// kind of record, names and that tab, the scrub policy's table for it, has
// no rule for, each joined with ".". Each table nested in classes is
// followed once, since a table nested in itself names the same keys there.
func unruled(classes *keyClasses, tab *table) []string {
	var paths []string
	seen := map[*keyClasses]bool{}
	var walk func(t *keyClasses, prefix []string)
	walk = func(t *keyClasses, prefix []string) {
		seen[t] = true
		for _, c := range t.classes {
			if keys := slices.Concat(prefix, c.keys); !hasRule(tab, keys) {
				paths = append(paths, strings.Join(keys, "."))
			}
		}
		for _, n := range t.nested {
			if !seen[n.table] {
				walk(n.table, slices.Concat(prefix, n.keys))
			}
		}
	}
	walk(classes, nil)
	return paths
}

// hasRule reports whether tab has a rule for the values at path.
func hasRule(tab *table, path []string) bool {
	_, ok := tab.rule(path)
	return ok
}

// The masker's key classes agree with the scrub policy, by what each value
// means: wherever the policy has a rule, the masker's class is one that the
// rule's meaning allows, a user object's values withheld first; and the
// masker names no path that the policy has no rule for, so that it shows no
// value that the scrub policy does not cover. Pinned rows, one of each
// meaning, keep a change of the policy that moves free text to an
// identifier from passing. A rule added at user.login is withheld, and with
// the masker as it is the binding holds; with a copy of it that shows the
// login, the binding fails, naming user.login.
func TestKeyClassesAgreeWithScrubPolicy(t *testing.T) {
	for tab, m := range maskerTables {
		var kinds []string
		for kind, k := range policyTables {
			if k == tab {
				kinds = append(kinds, kind)
			}
		}
		if len(kinds) == 0 {
			t.Errorf("the masker has key classes of records that no kind of the scrub policy is")
		}
		for _, p := range keyClassProblems(tab, m.classOf) {
			t.Errorf("%q: %s", kinds, p)
		}
		for _, path := range unruled(m.classes, tab) {
			t.Errorf("%q: the masker names %s, which no rule of the scrub policy covers", kinds, path)
		}
	}
	if _, ok := maskerTables[transactionTable]; !ok || policyTables["transactions"] != transactionTable || policyTables["transaction"] != transactionTable {
		t.Fatal("the transactions' tables are not both bound")
	}

	// Pinned rows: what the policy keeps at a path, as its rule's class and
	// what it maps or its grammar, and the masker's class there.
	for _, tc := range []struct {
		path, rule string
		want       mask.Class
	}{
		{"accountNumber", "map an account number", mask.AccountNumber},
		{"activityId", "map an activity ID", mask.Identifier},
		{"orderId", "map an order ID", mask.Identifier},
		{"netAmount", "scale", mask.Amount},
		{"transferItems.cost", "scale", mask.Amount},
		{"transferItems.price", "keep a number", mask.Amount},
		{"time", "keep a time", mask.Time},
		{"type", "keep a value of its closed list", mask.Identifier},
		{"description", "placeholder DESCRIPTION", mask.FreeText},
		{"transferItems.instrument.description", "keep a security's name", mask.FreeText},
		{"transferItems.instrument.optionDeliverables.deliverable.description", "keep a security's name", mask.FreeText},
		{"transferItems.instrument.symbol", "keep a symbol", mask.Identifier},
		{"transferItems.instrument.cusip", "keep a CUSIP", mask.Identifier},
		{"transferItems.instrument.expirationDate", "keep a time", mask.Time},
		{"Transferitems.Instrument.Description", "keep a security's name", mask.FreeText},
		{"user.login", "", mask.Withheld},
		{"User.Login", "", mask.Withheld},
		{"USER.firstName", "", mask.Withheld},
	} {
		keys := strings.Split(tc.path, ".")
		r, ok := transactionTable.rule(keys)
		if got := ruleName(r, ok); got != tc.rule {
			t.Errorf("the scrub policy's rule at %s is %q, want %q", tc.path, got, tc.rule)
		}
		if got := TransactionKeyClass(keys); got != tc.want {
			t.Errorf("the masker's class at %s is %s, want %s", tc.path, className(got), className(tc.want))
		}
	}

	// A rule at user.login, as a recording that holds a user object would
	// bring, leaves the login withheld: the binding holds with the masker as
	// it is, and fails, naming user.login, with a masker that shows it.
	login := &table{
		rules:  slices.Concat(slices.Clone(transactionTable.rules), mapAt(identifier("a user's login"), "user.login")),
		nested: transactionTable.nested,
	}
	if p := keyClassProblems(login, TransactionKeyClass); len(p) > 0 {
		t.Errorf("with a rule at user.login, the binding fails: %q", p)
	}
	shown := func(path []string) mask.Class {
		if keysMatch(path, []string{"user", "login"}) {
			return mask.Identifier
		}
		return TransactionKeyClass(path)
	}
	if p := keyClassProblems(login, shown); len(p) != 1 || !strings.HasPrefix(p[0], "user.login: ") {
		t.Errorf("with a masker that shows user.login, the binding finds %q, want user.login alone", p)
	}

	// The binding finds a masker that shows free text, a name, a
	// deliverable's name or an account number as an identifier, or money as
	// a time; and a policy that keeps a description as a code.
	for path, class := range map[string]mask.Class{
		"description":                          mask.Identifier,
		"transferItems.instrument.description": mask.Identifier,
		"transferItems.instrument.optionDeliverables.deliverable.description": mask.Identifier,
		"accountNumber": mask.Identifier,
		"netAmount":     mask.Time,
		"activityId":    mask.FreeText,
	} {
		broken := func(p []string) mask.Class {
			if strings.Join(p, ".") == path {
				return class
			}
			return TransactionKeyClass(p)
		}
		if p := keyClassProblems(transactionTable, broken); len(p) != 1 || !strings.HasPrefix(p[0], path+": ") {
			t.Errorf("with %s of the class %s, the binding finds %q", path, className(class), p)
		}
	}
	coded := &table{rules: slices.Concat(keepAt(codeGrammar, "description"), transactionTable.rules), nested: transactionTable.nested}
	if p := keyClassProblems(coded, TransactionKeyClass); len(p) != 1 || !strings.HasPrefix(p[0], "description: ") {
		t.Errorf("with a description kept as a code, the binding finds %q", p)
	}

	// A path that the masker shows and the policy has no rule for is found,
	// at a transaction or within an instrument.
	more := &keyClasses{
		classes: slices.Concat(transactionKeyClasses.classes, classesAt(mask.Identifier, "zzCode")),
		nested: []nestedKeyClasses{{keys: []string{"transferItems", "instrument"}, table: &keyClasses{
			classes: slices.Concat(instrumentKeyClasses.classes, classesAt(mask.Time, "zzDate")),
			nested:  instrumentKeyClasses.nested,
		}}},
	}
	if p := unruled(more, transactionTable); !slices.Equal(p, []string{"zzCode", "transferItems.instrument.zzDate"}) {
		t.Errorf("with two paths that no rule covers, the binding finds %q", p)
	}
}

// ruleName returns what r, the rule at a path if ok, does there, as
// TestKeyClassesAgreeWithScrubPolicy's pinned rows say it, or "" for none.
func ruleName(r rule, ok bool) string {
	switch {
	case !ok:
		return ""
	case r.class == classMap:
		return "map " + string(r.id)
	case r.class == classScale:
		return "scale"
	case r.class == classKeep:
		return "keep " + r.gram.name
	case r.class == classPlaceholder:
		return "placeholder " + r.text.word
	}
	return "a rule of no class"
}

// The fixtures' transactions come through mask.JSON by their key classes
// with nothing withheld and no key unknown: every value as it was, but each
// account number, masked, and the descriptions, masked by pattern; and a
// user object, as Schwab's spec has it, withheld whole, its key in any
// case.
func TestTransactionKeyClassShowsTheFixtures(t *testing.T) {
	raw := readTestdata(t, "transactions.json")
	got, r, err := mask.JSON(raw, TransactionKeyClass, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if r.Withheld != 0 || r.UnknownIntegers != 0 || r.Tags != 0 || r.AccountNumbers != len(rows) || !slices.Equal(r.Paths, []string{"accountNumber"}) {
		t.Errorf("the fixtures' transactions report %+v, want only their %d account numbers masked", r, len(rows))
	}
	var unknown []string
	eachScalar(parseNode(raw), nil, nil, func(path []string, _ []byte, _ *node) {
		if TransactionKeyClass(path) == mask.Unknown && !slices.Contains(unknown, strings.Join(path, ".")) {
			unknown = append(unknown, strings.Join(path, "."))
		}
	})
	if len(unknown) > 0 {
		t.Errorf("the fixtures' transactions hold keys of no class: %q", unknown)
	}
	for _, a := range schwabtest.FixtureAccounts {
		if strings.Contains(string(got), a.Number) {
			t.Errorf("the fixtures' transactions, masked, hold %s", mask.Number(a.Number))
		}
	}
	if !strings.Contains(string(got), `"accountNumber":"...135"`) || !strings.Contains(string(got), `"description":"Dividend Reinvest"`) ||
		!strings.Contains(string(got), `"activityId":73508261946`) {
		t.Errorf("the fixtures' transactions, masked, lack what they held: %s", got)
	}

	user := `{"activityId":73508261946,"user":{"cdDomainId":"A000000012345678","login":"jdoe1234","type":"ADVISOR_USER",` +
		`"userId":12345678,"systemUserName":"jdoe","firstName":"Jane","lastName":"Doe","brokerRepCode":"X1"}}`
	got, r, err = mask.JSON([]byte(user), TransactionKeyClass, nil)
	want := `{"activityId":73508261946,"user":{"cdDomainId":"[withheld]","login":"[withheld]","type":"[withheld]",` +
		`"userId":null,"systemUserName":"[withheld]","firstName":"[withheld]","lastName":"[withheld]","brokerRepCode":"[withheld]"}}`
	if err != nil || string(got) != want || r.Withheld != 8 {
		t.Errorf("a user object comes through as %s, %+v, %v; want %s", got, r, err, want)
	}
	for _, key := range []string{"User", "USER"} {
		got, r, err = mask.JSON([]byte(strings.Replace(user, `"user"`, `"`+key+`"`, 1)), TransactionKeyClass, nil)
		if want := strings.Replace(want, `"user"`, `"`+key+`"`, 1); err != nil || string(got) != want || r.Withheld != 8 || r.UnknownIntegers != 0 {
			t.Errorf("a user object under %q comes through as %s, %+v, %v; want %s", key, got, r, err, want)
		}
	}
}
