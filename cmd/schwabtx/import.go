package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
)

// runImport imports files into the store: snapshot files, History and
// Realized Gain/Loss exports of schwab.com, and `schwabtx transactions
// -json` output, each known by its content, all in one write; or, with
// -forget, sets an imported export aside, or brings it back.
//
// The order of its work keeps the store's files safe while another process
// has the store open (3.4 of the design): it parses its flags; checks that
// no input is the store or one of its files, before it reads any; reads
// every input whole; and only then opens the store. The store never opens
// an input by its path.
func runImport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("schwabtx import", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	account := fl.String("account", "", "the account the exports are for: A and its number in the store, the last digits of its number, or its nickname\n(default: what each export's name and content say)")
	from := fl.String("from", "", "the first day the History exports cover, for those that state none (YYYY-MM-DD or MM/DD/YYYY)")
	to := fl.String("to", "", "the last day the History exports cover, for those that state none")
	allTypes := fl.Bool("all-types", false, "the History exports hold every type and no symbol filter: they are complete")
	filtered := fl.Bool("filtered", false, "the History exports were filtered by type or symbol")
	dryRun := fl.Bool("dry-run", false, "report what the import would do, and save nothing")
	force := fl.Bool("force", false, "import an export whose name gives digits that are not those of its account, as a renamed file has")
	forget := fl.String("forget", "", "set aside the imported export with this ref (F…): its transactions that no other file holds leave the ledger")
	undo := fl.Bool("undo", false, "with -forget, bring the export back")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage of schwabtx import:

  schwabtx import [flags] [FILE...]
  schwabtx import [-store PATH] -forget FREF [-undo]

Imports each FILE into the store, all of them or none: a snapshot file that
schwabtx snapshot writes, a History export of schwab.com in CSV or JSON, a
Realized Gain/Loss export's Lot Details in CSV, or the output of
schwabtx transactions -json. Each is known by its content; anything else is
refused, and then nothing is imported. With no FILE, it imports the snapshot
file that schwabtx snapshot writes. Importing a file again adds nothing.

`)
		fl.PrintDefaults()
	}
	files, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	path, err := storePath(*store)
	if err != nil {
		return err
	}
	if *forget != "" {
		if len(files) > 0 {
			return errors.New("-forget takes no files: it sets aside a file the store holds")
		}
		// -forget changes the store at once: none of the import's own
		// flags, -dry-run among them, goes with it.
		var other []string
		fl.Visit(func(f *flag.Flag) {
			if f.Name != "store" && f.Name != "forget" && f.Name != "undo" {
				other = append(other, "-"+f.Name)
			}
		})
		if len(other) > 0 {
			return fmt.Errorf("%s %s not go with -forget, which takes only -undo", strings.Join(other, " and "), pluralWord(len(other), "does", "do"))
		}
		return forgetFile(ctx, path, *forget, *undo, stdout, stderr)
	}
	if *undo {
		return errors.New("-undo goes with -forget F…")
	}
	o := history.ImportOptions{Account: *account, DryRun: *dryRun, Force: *force}
	if o.From, err = parseDay("-from", *from); err != nil {
		return err
	}
	if o.To, err = parseDay("-to", *to); err != nil {
		return err
	}
	switch {
	case *allTypes && *filtered:
		return errors.New("-all-types and -filtered say opposite things; give one")
	case *allTypes:
		o.Completeness = "complete"
	case *filtered:
		o.Completeness = "filtered"
	}

	defaulted := len(files) == 0
	if defaulted {
		p, err := snapshotPath("")
		if err != nil {
			return err
		}
		files = []string{p}
	}
	// No input may be the store or one of its files, and none is read
	// before every one is checked.
	for _, f := range files {
		if name, ok := companionOf(f, path); ok {
			return fmt.Errorf("%s is the store itself (or one of its files: %s); nothing was imported", f, name)
		}
	}
	in := make([]history.FileInput, len(files))
	for i, f := range files {
		content, err := os.ReadFile(f) //nolint:gosec // G703: the files are yours to name, and none is the store (checked above).
		switch {
		case defaulted && errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("there is no snapshot file at %s yet; name the files to import", f)
		case err != nil:
			return fmt.Errorf("reading %w; nothing was imported", err)
		}
		// A file that is none of those an import reads is refused before
		// the store is opened, which would make a missing one.
		if _, err := schwabfile.Read(filepath.Base(f), content); err != nil {
			return fmt.Errorf("%w; nothing was imported", err)
		}
		in[i] = history.FileInput{Name: f, Content: content}
	}
	if o.From != "" && o.To != "" && o.To < o.From {
		return fmt.Errorf("-from %s is after -to %s", o.From, o.To)
	}

	s, made, closeStore, err := openImportStore(ctx, path, *dryRun, stderr)
	if err != nil {
		return err
	}
	defer closeStore()
	report, err := s.Import(ctx, in, o)
	if err != nil {
		return err
	}
	for _, r := range report.Files {
		printFileReport(stdout, r)
	}
	if *dryRun {
		fmt.Fprintln(stdout, "nothing was saved (-dry-run)")
		return nil
	}
	if made {
		fmt.Fprintln(stderr, "new store at", path)
	}
	// A file the store held already is reported, and is not imported.
	held := 0
	for _, r := range report.Files {
		if r.Already {
			held++
		}
	}
	line := plural(len(report.Files)-held, "file") + " imported"
	if held > 0 {
		line += fmt.Sprintf(" (%d already held)", held)
	}
	line += " into " + path
	if n := report.Waiting; n > 0 {
		line += fmt.Sprintf("; %d %s for you: %s", n, pluralWord(n, "item waits", "items wait"), copyCommand(path, "status", "-review"))
	}
	fmt.Fprintln(stdout, line)
	return nil
}

// openImportStore opens the store at path for an import, and makes it
// when it is missing, which made says. A dry run makes no store: when the
// store is missing, it opens a new store of its own, the same as an empty
// one, in a folder of its own. closeStore closes the store, and removes
// that folder.
func openImportStore(ctx context.Context, path string, dryRun bool, stderr io.Writer) (s *history.Store, made bool, closeStore func(), err error) {
	_, statErr := os.Stat(path)
	missing := errors.Is(statErr, fs.ErrNotExist)
	if dryRun && missing {
		// What would refuse to make the store, such as a journal left
		// without it, refuses the dry run too.
		s, err = history.Open(ctx, storeOptions(path, true, stderr))
		if err == nil {
			return s, false, func() { s.Close() }, nil // made meanwhile
		}
		if !errors.Is(err, history.ErrNoStore) {
			return nil, false, nil, err
		}
		dir, err := os.MkdirTemp("", "schwabtx-dry-run-")
		if err != nil {
			return nil, false, nil, fmt.Errorf("making a store for the dry run: %w", err)
		}
		if s, err = history.Open(ctx, storeOptions(filepath.Join(dir, "history.db"), false, stderr)); err != nil {
			os.RemoveAll(dir)
			return nil, false, nil, err
		}
		return s, false, func() {
			s.Close()
			os.RemoveAll(dir)
		}, nil
	}
	if s, err = history.Open(ctx, storeOptions(path, false, stderr)); err != nil {
		return nil, false, nil, err
	}
	return s, missing, func() { s.Close() }, nil
}

// kindNames say what each kind of file is, as an import reports it.
var kindNames = map[string]string{
	"snapshots":    "snapshot file",
	"history_csv":  "History export (CSV)",
	"history_json": "History export (JSON)",
	"realized_csv": "Realized Gain/Loss export (Lot Details)",
	"api_json":     "schwabtx transactions -json output",
}

// sourceNames say where an export's dates came from.
var sourceNames = map[string]string{
	"stated":   "as the file states",
	"user":     "as you said",
	"rows":     "from its rows",
	"api_rows": "from its rows",
}

// printFileReport writes what an import made of one file.
func printFileReport(w io.Writer, r history.FileReport) {
	head := plainCell(r.Name) + ": " + kindNames[r.Kind]
	switch {
	case r.Already:
	case r.Kind == "snapshots":
		head += fmt.Sprintf(", %s: %d new %s, %d already held", plural(r.Lines, "line"), r.Snapshots.New,
			pluralWord(r.Snapshots.New, "snapshot"), r.Snapshots.Known)
	case r.Kind == "api_json":
		head += ", " + plural(r.Rows, "row")
	default:
		if r.Account != "" {
			head += " of " + plainCell(r.Account)
		}
		unit := "row"
		if r.Kind == "realized_csv" {
			unit = "lot"
		}
		dates := ""
		if r.From != "" {
			dates = fmt.Sprintf("%s to %s (%s), ", r.From, r.To, sourceNames[r.Source])
		}
		head += ",\n    " + dates + plural(r.Rows, unit)
	}
	fmt.Fprintln(w, head)
	if !r.Already {
		switch r.Kind {
		case "snapshots":
		case "api_json":
			for _, a := range r.Accounts {
				line := fmt.Sprintf("  %s: %s, %d new to the store, %d already held", plainCell(a.Account), plural(a.Rows, "row"), a.New, a.Known)
				if a.Differ > 0 {
					line += fmt.Sprintf(" (%d %s from the store's, which is kept)", a.Differ, pluralWord(a.Differ, "differs", "differ"))
				}
				fmt.Fprintln(w, line)
			}
		case "realized_csv":
			fmt.Fprintf(w, "  %d %s new to the store, %d already held (another file had them)\n", r.New, pluralWord(r.New, "lot"), r.Known)
		default:
			fmt.Fprintf(w, "  %d %s new to the store, %d already held (another file had them)\n", r.New, pluralWord(r.New, "transaction"), r.Known)
			fmt.Fprintf(w, "  linked to Schwab's API rows: %d; uncertain: %d; held out (inside the API's dates, no API row): %d;\n"+
				"    outside the API's dates (counted from the file): %d\n", r.Linked, r.Uncertain, r.HeldOut, r.BeforeAPI)
		}
	}
	for _, msg := range r.Warnings {
		if strings.HasPrefix(msg, "completeness: ") {
			continue // the completeness line says it
		}
		fmt.Fprintln(w, "  warning:", plainCell(msg))
	}
	if r.Completeness != "" && !r.Already {
		fmt.Fprintln(w, "  completeness:", completenessLine(r))
	}
}

// completenessLine says what is known of whether an export holds every
// type and symbol, and what to do when nothing established it.
func completenessLine(r history.FileReport) string {
	switch r.Completeness {
	case "complete":
		switch r.CompleteBy {
		case "api":
			return "complete: it agrees with Schwab's API rows of its dates"
		case "file":
			return "complete: it agrees with a complete export of its dates"
		}
		return "complete, as you said"
	case "filtered":
		return "filtered: it lacks rows of types or symbols that other sources hold in its dates"
	}
	for _, msg := range r.Warnings {
		if detail, ok := strings.CutPrefix(msg, "completeness: unknown; "); ok {
			return "unknown; " + plainCell(detail)
		}
	}
	return "unknown"
}

// pluralWord returns one when n is 1, else many, which is one and an s
// when it is not given.
func pluralWord(n int, one string, many ...string) string {
	if n == 1 {
		return one
	}
	if len(many) > 0 {
		return many[0]
	}
	return one + "s"
}

// forgetFile sets aside the file that ref names in the store at path, or
// with undo brings it back, and says what that did.
func forgetFile(ctx context.Context, path, ref string, undo bool, stdout, stderr io.Writer) error {
	s, err := openStore(ctx, path, stderr)
	if err != nil {
		return err
	}
	defer s.Close()
	r, err := s.Forget(ctx, ref, undo)
	if err != nil {
		return err
	}
	name := plainCell(r.Name)
	also := ""
	if r.Stayed > 0 {
		also = fmt.Sprintf(", %d %s (also in %s)", r.Stayed, pluralWord(r.Stayed, "stays", "stay"), strings.Join(r.StayIn, ", "))
	}
	if undo {
		fmt.Fprintf(stdout, "brought back %s (%s): %d %s that no other file holds %s in the ledger again%s\n",
			r.Ref, name, r.Left, pluralWord(r.Left, "transaction"), pluralWord(r.Left, "is", "are"), also)
		return nil
	}
	line := fmt.Sprintf("set aside %s (%s): %d %s that no other file holds left the ledger", r.Ref, name, r.Left, pluralWord(r.Left, "transaction"))
	if r.Stayed > 0 {
		line += also + ", and your decisions on them follow them"
	}
	if r.Waiting > 0 {
		line += fmt.Sprintf("; %d of your decisions now %s (their transactions are in no other file)", r.Waiting, pluralWord(r.Waiting, "waits", "wait"))
	}
	fmt.Fprintln(stdout, line)
	fmt.Fprintf(stdout, "bring it back with: %s\n", copyCommand(path, "import", "-forget", quoteArg(r.Ref), "-undo"))
	return nil
}
