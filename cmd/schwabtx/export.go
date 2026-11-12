package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history"
)

// runExport writes views of the store as CSV or JSON Lines (18 of the
// design): to stdout, to a file (-o) or to a folder (-dir).
//
// The order of its work keeps the store's files safe and the store free
// for others (3.4 and 4 of the design): it parses its flags; checks that no
// output is the store or one of its files; opens the store; reads every
// row it writes in one read transaction, which ends before anything is
// written, so that a slow reader of stdout holds nothing open; and only
// then writes.
func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	kinds := history.ExportKinds()
	fl := flag.NewFlagSet("schwabtx export", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	what := fl.String("what", "ledger", "what to export: one of the names below, a comma-separated list of them, or all (a list or all needs -dir)")
	format := fl.String("format", "csv", "csv, or jsonl for JSON Lines")
	account := fl.String("account", "", "only this account: A and its number in the store, the last digits of its number, or its nickname")
	from := fl.String("from", "", "only rows of this day or later (YYYY-MM-DD or MM/DD/YYYY)")
	to := fl.String("to", "", "only rows of this day or earlier")
	all := fl.Bool("all", false, "every entry and leg, counted or not (the ledger and legs hold counted ones)")
	raw := fl.Bool("raw", false, "add each row's raw bytes as Schwab or the file had them (includes full account numbers)")
	show := fl.Bool("show-accounts", false, "add each row's full account number")
	out := fl.String("o", "", "write to this file, readable only by you, instead of stdout")
	dir := fl.String("dir", "", "write each table to a file of its own in this folder, with manifest.json")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), "Usage of schwabtx export:\n\n  schwabtx export [flags]\n\nWrites what the store holds as CSV or JSON Lines. What -what names:\n\n")
		for _, k := range kinds {
			raw := ""
			if k.Raw {
				raw = " (takes -raw)"
			}
			fmt.Fprintf(fl.Output(), "  %-18s %s%s\n", k.Name, k.About, raw)
		}
		fmt.Fprintln(fl.Output())
		fl.PrintDefaults()
	}
	rest, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q", plainCell(rest[0]))
	}
	if *out != "" && *dir != "" {
		return errors.New("-o and -dir name two places to write; give one")
	}
	o := history.ExportOptions{Format: *format, Account: *account, All: *all, Raw: *raw, ShowAccounts: *show}
	for w := range strings.SplitSeq(*what, ",") {
		if w = strings.TrimSpace(w); w != "" {
			o.What = append(o.What, w)
		}
	}
	if *dir == "" && (len(o.What) > 1 || slices.Contains(o.What, "all")) {
		return fmt.Errorf("-what %s needs -dir", strings.Join(o.What, ","))
	}
	if o.From, err = parseDay("-from", *from); err != nil {
		return err
	}
	if o.To, err = parseDay("-to", *to); err != nil {
		return err
	}
	path, err := storePath(*store)
	if err != nil {
		return err
	}
	// No output may be the store or one of its files, by its name too, since
	// an output need not exist yet.
	if *out != "" {
		if _, ok := companionOf(*out, path); ok {
			return fmt.Errorf("-o %s would replace the store itself (or one of its files); choose another name", *out)
		}
	}
	if *dir != "" {
		names := []string{"manifest.json"}
		for _, k := range kinds {
			names = append(names, k.Name+".csv", k.Name+".jsonl")
		}
		for _, n := range names {
			if _, ok := companionOf(filepath.Join(*dir, n), path); ok {
				return fmt.Errorf("-dir %s would put %s over the store itself (or one of its files); choose another folder", *dir, n)
			}
		}
	}

	s, err := openStore(ctx, path, stderr)
	if err != nil {
		return err
	}
	e, err := s.Export(ctx, o)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if e.HoldsNumbers {
		fmt.Fprintln(stderr, "warning: this export holds full account numbers")
	}
	switch {
	case *dir != "":
		err = e.WriteDir(*dir)
	case *out != "":
		err = e.WriteFile(*out)
	default:
		_, err = e.WriteTo(stdout)
	}
	if err != nil {
		return err
	}
	exportSummary(stderr, path, e, *out, *dir)
	return nil
}

// exportSummary says on w what an export of the store at path wrote and
// where, each account's coverage by source, which says which dates the rows
// are whole for, and what it left out: the file transactions that wait for
// you, when it holds a table of counted rows without -all.
func exportSummary(w io.Writer, path string, e *history.Export, out, dir string) {
	var parts []string
	for _, t := range e.Tables {
		parts = append(parts, fmt.Sprintf("%s %s", plural(t.Rows, "row"), t.Name))
	}
	where := ""
	switch {
	case dir != "":
		where = " to " + dir
	case out != "":
		where = " to " + out
	}
	fmt.Fprintf(w, "wrote %s%s\n", strings.Join(parts, ", "), where)
	var account string
	var line []string
	flush := func() {
		if account != "" {
			fmt.Fprintf(w, "  %s: %s\n", account, strings.Join(line, "; "))
		}
	}
	for _, c := range e.Coverage {
		label := fmt.Sprintf("A%d %s", c.AccountID, c.Account)
		if label != account {
			flush()
			account, line = label, nil
		}
		line = append(line, fmt.Sprintf("%s %s to %s", c.Source, c.From, c.To))
	}
	flush()
	if e.Waiting > 0 {
		fmt.Fprintf(w, "%d file %s for you to resolve %s (%s) and %s not in this export\n",
			e.Waiting, pluralWord(e.Waiting, "transaction waits", "transactions wait"), pluralWord(e.Waiting, "it", "them"),
			copyCommand(path, "status", "-review"), pluralWord(e.Waiting, "is", "are"))
	}
}
