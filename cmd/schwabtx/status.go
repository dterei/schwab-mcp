package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history"
)

// runStatus reports on the store: its state, what waits for you (-review),
// or one thing in full (a REF). It is a report, and fails only when it
// cannot read the store.
func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	fl := flag.NewFlagSet("schwabtx status", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	account := fl.String("account", "", "only this account: A and its number in the store, the last digits of its number, or its nickname")
	review := fl.Bool("review", false, "list what waits for you, each with the command that settles it")
	all := fl.Bool("all", false, "with -review, list what is flagged for information too")
	show := fl.Bool("show-accounts", false, "print full account numbers instead of the last 3 digits")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage of schwabtx status:

  schwabtx status [-store PATH] [-account ACCOUNT] [-show-accounts]
  schwabtx status [-store PATH] [-account ACCOUNT] -review [-all]
  schwabtx status [-store PATH] [-account ACCOUNT] REF

Without -review or a REF, the state of the store: what each account's
sources cover, its snapshots, and what waits for you. With -review, what
waits for you, each with the command that settles it. With a REF (A3,
T73508261941, C3a9e21b0.17, F3a9e21b0, L5c1d2e3f.4, S812, K:i:1973757747),
that one thing in full.

`)
		fl.PrintDefaults()
	}
	rest, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	switch {
	case len(rest) > 1:
		return fmt.Errorf("unexpected argument %q", plainCell(rest[1]))
	case len(rest) == 1 && *review:
		return errors.New("give -review or a REF, not both")
	case *all && !*review:
		return errors.New("-all goes with -review")
	case *show && (*review || len(rest) == 1):
		return errors.New("-show-accounts goes with the state of the store, without -review or a REF")
	}
	path, err := storePath(*store)
	if err != nil {
		return err
	}
	// The commands in what the store says name it.
	defer func() { err = errorWithStore(path, err) }()
	s, err := openStore(ctx, path, stderr)
	if err != nil {
		return err
	}
	defer s.Close()
	switch {
	case len(rest) == 1:
		d, err := s.Lookup(ctx, rest[0], *account)
		if err != nil {
			return err
		}
		return printDetail(stdout, path, d)
	case *review:
		groups, err := s.Review(ctx, history.ReviewOptions{Account: *account, All: *all, Quote: quoteArg})
		if err != nil {
			return err
		}
		return printReview(stdout, path, groups)
	}
	st, err := s.Status(ctx, history.StatusOptions{Account: *account, ShowAccounts: *show})
	if err != nil {
		return err
	}
	return printStatus(stdout, stderr, path, st)
}

// printStatus writes the state of the store at path.
func printStatus(stdout, stderr io.Writer, path string, st history.Status) error {
	for _, w := range st.Warnings {
		fmt.Fprintln(stderr, "warning:", withStore(path, w))
	}
	size := ""
	if info, err := os.Stat(path); err == nil {
		size = " (" + byteSize(info.Size()) + ")"
	}
	fmt.Fprintf(stdout, "store     %s%s, schema %d, rules %d, calendar %d (checked to %d)\n",
		path, size, st.Schema, st.Rules, st.Calendar, st.VerifiedThrough)
	cols := []column{{head: "ACCOUNT"}, {head: "API DATES"}, {head: "FILE DATES"}, {head: "SNAPSHOTS"}, {head: "GAPS"}}
	var rows [][]string
	for _, a := range st.Accounts {
		label := a.Label
		if parts := strings.SplitN(label, " ", 3); a.Number != "" && len(parts) >= 2 {
			parts[1] = a.Number // in place of the masked number
			label = strings.Join(parts, " ")
		}
		gaps := strconv.Itoa(a.Gaps)
		if a.Since > 0 {
			gaps += fmt.Sprintf(", and %s since", plural(a.Since, "session"))
		}
		rows = append(rows, []string{textCell(label, 40), spanCell(a.API), spanCell(a.Files), spanCell(a.Snapshots), gaps})
	}
	if err := writeTable(stdout, cols, rows); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ledger: %s from the API, %s from files only; %s linked to API rows\n",
		thousands(st.FromAPI), thousands(st.FromFiles), plural(st.Linked, "file row"))
	if st.ItemIssueBoth > 0 {
		fmt.Fprintf(stdout, "ItemIssueId equals instrumentId for %d of %d %s that have both\n", st.ItemIssueAgree, st.ItemIssueBoth,
			pluralWord(st.ItemIssueBoth, "security", "securities"))
	}
	if st.Waiting() == 0 {
		fmt.Fprintln(stdout, "nothing waits for you")
		return nil
	}
	var parts []string
	for _, p := range []struct {
		n          int
		one, other string
	}{
		{st.Uncertain, "uncertain match", "uncertain matches"},
		{st.HeldOut, "held out", "held out"},
		{st.Attention, "flagged for attention", "flagged for attention"},
		{st.Filtered, "file that may be filtered", "files that may be filtered"},
	} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, pluralWord(p.n, p.one, p.other)))
		}
	}
	fmt.Fprintf(stdout, "to review: %s\n  (%s)\n", strings.Join(parts, ", "), copyCommand(path, "status", "-review"))
	return nil
}

// spanCell returns a span of days for a table: from and to, and how many
// runs when there are gaps between them, or - when nothing covers any.
func spanCell(s history.Span) string {
	switch s.Runs {
	case 0:
		return "-"
	case 1:
		return fmt.Sprintf("%s to %s", s.From, s.To)
	}
	return fmt.Sprintf("%s to %s (%d runs)", s.From, s.To, s.Runs)
}

// byteSize returns n bytes as a person reads a file's size: 812 B, 3.1 MB.
func byteSize(n int64) string {
	units := []string{"B", "kB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1000 && i < len(units)-1 {
		f, i = f/1000, i+1
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// thousands returns n with commas between its thousands: 4,210.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + thousands(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// printReview writes what waits for you in the store at path, group by
// group, its commands naming the store (withStore).
func printReview(w io.Writer, path string, groups []history.ReviewGroup) error {
	if len(groups) == 0 {
		_, err := fmt.Fprintln(w, "nothing waits for you")
		return err
	}
	var b strings.Builder
	for _, g := range groups {
		n := len(g.Items)
		if n == 0 {
			n = len(g.Lines)
		}
		fmt.Fprintf(&b, "%s (%d): %s\n", g.Title, n, g.Ask)
		for _, l := range g.Lines {
			fmt.Fprintf(&b, "  %s\n", plainCell(l))
		}
		for _, it := range g.Items {
			fields := []string{}
			for _, f := range []string{it.Ref, it.Account} {
				if f != "" {
					fields = append(fields, f)
				}
			}
			for _, f := range it.Fields {
				if f != "" {
					fields = append(fields, plainCell(f))
				}
			}
			fmt.Fprintf(&b, "  %s\n", strings.Join(fields, "  "))
			for _, l := range it.Lines {
				fmt.Fprintf(&b, "      %s\n", plainCell(l))
			}
			if len(it.Commands) > 0 {
				fmt.Fprintf(&b, "      %s\n", strings.Join(it.Commands, "      "))
			}
		}
		for _, c := range g.Commands {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	_, err := io.WriteString(w, withStore(path, b.String()))
	return err
}

// printDetail writes one thing of the store at path in full, the commands
// in its flags naming the store (withStore).
func printDetail(w io.Writer, path string, d history.Detail) error {
	var b strings.Builder
	head := d.Target.Ref + ": " + d.Target.Kind
	switch {
	case d.Target.Kind == history.TargetAccount:
		head = d.Target.Account + ": " + d.Target.Kind
	case d.Target.Account != "":
		head += " of " + d.Target.Account
	}
	fmt.Fprintln(&b, plainCell(head))
	width := 0
	for _, f := range d.Fields {
		width = max(width, len(f[0]))
	}
	for _, f := range d.Fields {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, f[0], plainCell(f[1]))
	}
	for _, s := range d.Sections {
		fmt.Fprintf(&b, "%s:\n", s.Title)
		for _, l := range s.Lines {
			fmt.Fprintf(&b, "  %s\n", plainCell(l))
		}
	}
	_, err := io.WriteString(w, withStore(path, b.String()))
	return err
}
