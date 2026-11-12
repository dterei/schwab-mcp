package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/mask"
)

// editOption is an option of schwabtx edit that makes a decision: the kinds
// of thing it is for, and what it takes.
type editOption struct {
	flag  string
	kinds []string
	takes string
}

// entryKinds are the kinds of thing an entry's options are for.
var entryKinds = []string{history.TargetTransaction, history.TargetFileTransaction}

// editOptions are the options that decide, in the order edit records them.
var editOptions = []editOption{
	{"category", entryKinds, "a transaction (T… or C…)"},
	{"count", entryKinds, "a transaction (T… or C…)"},
	{"pair", entryKinds, "a transaction (T… or C…)"},
	{"security", entryKinds, "a transaction (T… or C…)"},
	{"link", []string{history.TargetFileTransaction}, "a file transaction (C…)"},
	{"reject", []string{history.TargetFileTransaction}, "a file transaction (C…)"},
	{"completeness", []string{history.TargetFile}, "a file (F…)"},
	{"from", []string{history.TargetFile}, "a file (F…)"},
	{"to", []string{history.TargetFile}, "a file (F…)"},
	{"range", []string{history.TargetFile}, "a file (F…)"},
	{"same", []string{history.TargetKey}, "an instrument key (K:…)"},
	{"differ", []string{history.TargetKey}, "an instrument key (K:…)"},
	{"auto", []string{history.TargetKey}, "an instrument key (K:…)"},
	{"kind", []string{history.TargetAccount}, "an account (A…)"},
}

// runEdit records your decisions about something in the store (16 of the
// design), and says what each changed.
func runEdit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("schwabtx edit", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	account := fl.String("account", "", "the account REF is of, when two share it; with a file (F…), the account it is for: an account, or auto")
	note := fl.String("note", "", "why, kept with each decision")
	v := map[string]*string{}
	for _, f := range []struct{ name, usage string }{
		{"category", "a transaction's category: deposit, withdrawal, transfer_in, transfer_out, journal, buy, sell, dividend, interest, fee, tax, other; or auto"},
		{"count", "whether a transaction counts: yes, no or auto"},
		{"pair", "the transaction a transaction is paired with (a journal between your accounts, a sweep, a reversal): REF2, none or auto"},
		{"security", "a transaction's security: K…, or auto"},
		{"link", "the API transactions a file transaction is: T…[,T…], none (count it from the file) or auto"},
		{"reject", "an API transaction a file transaction is not: T…, or auto"},
		{"completeness", "whether a file holds every type and symbol: complete, filtered or auto"},
		{"from", "with -to, the first day a file covers (YYYY-MM-DD or MM/DD/YYYY)"},
		{"to", "with -from, the last day a file covers"},
		{"range", "auto: give back a file the range it states, or that its import gave"},
		{"same", "another key (K:…) of the same security"},
		{"differ", "another key (K:…) of a different security"},
		{"kind", "an account's kind (taxable, ira, roth, ...), or auto"},
		{"action", "a file action, such as \"Bank Interest\", for -policy"},
		{"policy", "whether held-out file rows of -action count: count, hold or auto"},
	} {
		v[f.name] = fl.String(f.name, "", f.usage)
	}
	fl.Bool("auto", false, "withdraw your same or differ decisions on a key")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage of schwabtx edit:

  schwabtx edit [-store PATH] [-account ACCOUNT] [-note TEXT] REF OPTION...
  schwabtx edit [-store PATH] [-note TEXT] -action LABEL -policy count|hold|auto

Records your decisions about REF, which the store applies after its own
rules, and says what each changed. A transaction (T… or C…) takes
-category, -count, -pair and -security, and a file transaction (C…) -link
and -reject too; a file (F…) takes -completeness, -account, -from with -to,
and -range auto; an instrument key (K:…) -same, -differ and -auto; an
account (A…) -kind. The value auto withdraws a decision.

`)
		fl.PrintDefaults()
	}
	rest, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { set[f.Name] = true })
	path, err := storePath(*store)
	if err != nil {
		return err
	}

	if set["action"] || set["policy"] {
		if len(rest) > 0 || !set["action"] || !set["policy"] {
			return errors.New("-action LABEL and -policy count|hold|auto go together, without a REF")
		}
		s, err := openStore(ctx, path, true, stderr)
		if err != nil {
			return err
		}
		defer s.Close()
		d, err := s.Decide(ctx, history.Decision{Kind: "action", Subject: "action:" + *v["action"], Value: strings.TrimSpace(*v["policy"]), Note: *note})
		if err != nil {
			return err
		}
		return printDecided(stdout, fmt.Sprintf("held-out rows of %q: %s", textCell(mask.Text(*v["action"]), 60), textCell(mask.Text(*v["policy"]), 20)), d, true)
	}
	switch {
	case len(rest) == 0:
		fl.Usage()
		return errors.New("edit needs a REF and an option, or -action and -policy")
	case len(rest) > 1:
		return fmt.Errorf("unexpected argument %q: edit takes one REF", plainCell(rest[1]))
	}
	ref := rest[0]
	var given []editOption
	for _, o := range editOptions {
		if set[o.flag] {
			given = append(given, o)
		}
	}
	isFile := strings.HasPrefix(strings.ToLower(strings.TrimSpace(ref)), "f")
	if len(given) == 0 && (!isFile || !set["account"]) {
		return fmt.Errorf("edit %s needs an option that says what you decide; schwabtx edit -h lists them", plainCell(ref))
	}

	s, err := openStore(ctx, path, true, stderr)
	if err != nil {
		return err
	}
	defer s.Close()
	selector := *account
	if isFile {
		selector = "" // -account says which account a file is for
	}
	t, err := s.Find(ctx, ref, selector)
	if err != nil {
		return err
	}
	for _, o := range given {
		if !slices.Contains(o.kinds, t.Kind) {
			return fmt.Errorf("%s is %s %s; -%s takes %s", t.Ref, article(t.Kind), t.Kind, o.flag, o.takes)
		}
	}
	if t.Kind == history.TargetFile && set["account"] {
		given = append(given, editOption{flag: "account"})
	}
	if set["from"] != set["to"] {
		return errors.New("-from and -to go together: give both, or -range auto")
	}
	if set["range"] && (set["from"] || strings.TrimSpace(*v["range"]) != "auto") {
		return errors.New("-range takes auto, and goes without -from and -to")
	}

	// val returns the value given to the option named name.
	val := func(name string) string {
		switch {
		case name == "account":
			return strings.TrimSpace(*account)
		case v[name] != nil:
			return strings.TrimSpace(*v[name])
		}
		return ""
	}
	// Every option is read and its refs found before anything is recorded,
	// and the decisions are recorded in one write: all of them, or, when
	// the store refuses one, none.
	type step struct {
		flag, shown string
		d           history.Decision
	}
	var steps []step
	for _, o := range given {
		d := history.Decision{Subject: t.Subject, Value: val(o.flag), Note: *note}
		// shown is the value as edit says it back: free text masked, refs
		// as the store has them.
		shown := textCell(mask.Text(d.Value), 60)
		switch o.flag {
		case "category", "count", "completeness":
			d.Kind, d.Value = o.flag, strings.ToLower(d.Value)
		case "pair":
			d.Kind = "pair"
			if d.Value != "none" && d.Value != "auto" {
				other, err := s.Find(ctx, d.Value, "")
				if err != nil {
					return err
				}
				d.Value, shown = other.Subject, quoteArg(other.Ref)
			}
		case "security":
			d.Kind = "entry_security"
			if d.Value != "auto" {
				k, err := findKey(ctx, s, d.Value)
				if err != nil {
					return err
				}
				d.Value, shown = k.Subject, quoteArg(k.Ref)
			}
		case "link", "reject":
			d.Kind = map[string]string{"link": "match", "reject": "reject"}[o.flag]
			if d.Value != "none" && d.Value != "auto" {
				var subjects, refs []string
				for r := range strings.SplitSeq(d.Value, ",") {
					api, err := apiTransaction(ctx, s, strings.TrimSpace(r), t.AccountID)
					if err != nil {
						return err
					}
					subjects, refs = append(subjects, api.Subject), append(refs, api.Ref)
				}
				shown = quoteArg(strings.Join(refs, ","))
				if o.flag == "reject" && len(subjects) > 1 {
					return errors.New("-reject takes one API transaction (T…); give it once for each")
				}
				d.Value = strings.Join(subjects, ",")
			}
		case "from", "range":
			d.Kind = "file_range"
			if o.flag == "from" {
				from, err := parseDay("-from", d.Value)
				if err != nil {
					return err
				}
				to, err := parseDay("-to", val("to"))
				if err != nil {
					return err
				}
				d.Value = string(from) + ".." + string(to)
			}
		case "to":
			continue // with -from
		case "account":
			d.Kind = "file_account"
			if d.Value != "auto" {
				id, _, err := s.Label(ctx, d.Value)
				if err != nil {
					return err
				}
				d.Value = "acct:" + strconv.FormatInt(id, 10)
			}
		case "same", "differ":
			d.Kind = "security"
			k, err := findKey(ctx, s, d.Value)
			if err != nil {
				return err
			}
			d.Value, shown = o.flag+" "+k.Subject, quoteArg(k.Ref)
		case "auto":
			d.Kind, d.Value = "security", "auto"
		case "kind":
			d.Kind = "account"
			if d.Value != "auto" {
				d.Value = "kind=" + d.Value
			}
		}
		steps = append(steps, step{flag: o.flag, shown: shown, d: d})
	}
	ds := make([]history.Decision, len(steps))
	for i, st := range steps {
		ds[i] = st.d
	}
	decided, err := s.DecideAll(ctx, ds)
	if err != nil {
		return err
	}
	for i, st := range steps {
		what := fmt.Sprintf("%s -%s %s", quoteArg(t.Ref), st.flag, st.shown)
		quiet := true
		switch st.flag {
		case "auto":
			what = t.Ref + " -auto"
		case "from":
			what = fmt.Sprintf("%s covers %s", t.Ref, strings.ReplaceAll(st.d.Value, "..", " to "))
		case "account":
			after, err := s.Find(ctx, t.Ref, "")
			if err != nil {
				return err
			}
			what = fmt.Sprintf("%s is now for %s (was %s); both accounts were derived again", t.Ref, orUnknown(after.Account), orUnknown(t.Account))
			t, quiet = after, false
		}
		if err := printDecided(stdout, what, decided[i], quiet); err != nil {
			return err
		}
	}
	return nil
}

// article returns the article for a kind of thing.
func article(kind string) string {
	if strings.ContainsAny(kind[:1], "aeiouAEIO") {
		return "an"
	}
	return "a"
}

// orUnknown returns s, or says that it is not known.
func orUnknown(s string) string {
	if s == "" {
		return "no account"
	}
	return s
}

// findKey returns the instrument key that ref (K:…) names.
func findKey(ctx context.Context, s *history.Store, ref string) (history.Target, error) {
	k, err := s.Find(ctx, ref, "")
	if err == nil && k.Kind != history.TargetKey {
		err = fmt.Errorf("%s is %s %s, not an instrument key (K:…)", k.Ref, article(k.Kind), k.Kind)
	}
	return k, err
}

// apiTransaction returns the API transaction that ref names, of the
// account with the id account when that tells it apart from another's; one
// of another account is returned too, for the store to refuse with a
// sentence that says so.
func apiTransaction(ctx context.Context, s *history.Store, ref string, account int64) (history.Target, error) {
	t, err := s.Find(ctx, ref, "A"+strconv.FormatInt(account, 10))
	if err != nil {
		var other error
		if t, other = s.Find(ctx, ref, ""); other != nil {
			return t, err
		}
	}
	if t.Kind != history.TargetTransaction {
		return t, fmt.Errorf("%s is %s %s; a file transaction is linked to an API transaction (T…)", t.Ref, article(t.Kind), t.Kind)
	}
	return t, nil
}

// whyCounted says why an entry counts, or does not, as edit reports it.
var whyCounted = map[string]string{
	"api":       "Schwab's row",
	"gone":      "gone from Schwab's answers",
	"invalid":   "INVALID",
	"user":      "your decision",
	"linked":    "linked to Schwab's row, which counts in its place",
	"uncertain": "uncertain which API row it is",
	"held_out":  "held out: the API covers its dates",
	"policy":    "your policy for its action",
	"file":      "from the file",
	"tda":       "from TD Ameritrade's history",
}

// printDecided writes what a decision changed: what was decided, then each
// entry it booked otherwise, or, when none and say, that it booked none.
func printDecided(w io.Writer, what string, d history.Decided, say bool) error {
	var b strings.Builder
	if len(d.Changes) == 0 && say {
		fmt.Fprintf(&b, "%s: recorded; no transaction is booked otherwise\n", what)
	} else {
		fmt.Fprintf(&b, "%s: recorded\n", what)
	}
	for _, c := range d.Changes {
		from, to := c.Before, c.After
		if from.Category != to.Category || from.CategoryRule != to.CategoryRule {
			how := "the rule " + to.CategoryRule
			if to.CategoryRule == "user" {
				how = "yours"
				if from.CategoryRule != "user" {
					how += "; the rule said " + from.CategoryRule
				}
			}
			fmt.Fprintf(&b, "  %s: %s -> %s (%s)\n", c.Ref, from.Category, to.Category, how)
		}
		if from.Counted != to.Counted || from.CountedWhy != to.CountedWhy {
			fmt.Fprintf(&b, "  %s: %s (%s) -> %s (%s)\n", c.Ref, countedWord(from.Counted), whyCounted[from.CountedWhy],
				countedWord(to.Counted), whyCounted[to.CountedWhy])
		}
		if from.PairRef != to.PairRef || from.PairKind != to.PairKind {
			if to.PairRef == "" {
				fmt.Fprintf(&b, "  %s: no longer paired with %s\n", c.Ref, from.PairRef)
			} else {
				fmt.Fprintf(&b, "  %s: paired with %s (%s)\n", c.Ref, to.PairRef, to.PairKind)
			}
		}
		if from.SecurityKey != to.SecurityKey {
			fmt.Fprintf(&b, "  %s: security %s -> %s\n", c.Ref, keyOrNone(from.SecurityKey), keyOrNone(to.SecurityKey))
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// countedWord says whether an entry counts.
func countedWord(counted bool) string {
	if counted {
		return "counted"
	}
	return "not counted"
}

// keyOrNone returns a security's key as a ref, or none.
func keyOrNone(key string) string {
	if key == "" {
		return "none"
	}
	return quoteArg("K:" + key)
}
