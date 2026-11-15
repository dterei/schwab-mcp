package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// runExport writes views of the store as CSV or JSON Lines (18 of the
// design): to stdout, to a file (-o) or to a folder (-dir); or, with -format
// pp or ghostfolio, the ledger as the files that Portfolio Performance or
// Ghostfolio imports, to a new folder (runToolExport).
//
// The order of its work keeps the store's files safe and the store free
// for others (3.4 and 4 of the design): it parses its flags; checks that no
// output is the store or one of its files; opens the store; reads every
// row it writes in one read transaction, which ends before anything is
// written, so that a slow reader of stdout holds nothing open; and only
// then writes.
func runExport(ctx context.Context, args []string, stdout, stderr io.Writer) (err error) {
	kinds := history.ExportKinds()
	fl := flag.NewFlagSet("schwabtx export", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	what := fl.String("what", "ledger", "what to export: one of the names below, a comma-separated list of them, or all (a list or all needs -dir)")
	format := fl.String("format", "csv", "csv, jsonl for JSON Lines, pp for Portfolio Performance, or ghostfolio (pp and ghostfolio write the ledger to a new -dir)")
	account := fl.String("account", "", "only this account: A and its number in the store, the last digits of its number, or its nickname")
	from := fl.String("from", "", "with csv and jsonl, only rows of this day or later (YYYY-MM-DD or MM/DD/YYYY)")
	to := fl.String("to", "", "only rows of this day or earlier (for pp and ghostfolio, by default the day before today in New York)")
	all := fl.Bool("all", false, "every entry and leg, counted or not (the ledger and legs hold counted ones)")
	raw := fl.Bool("raw", false, "add each row's raw bytes as Schwab or the file had them (includes full account numbers)")
	show := fl.Bool("show-accounts", false, "add each row's full account number")
	manual := fl.String("manual", "", "with -format ghostfolio: these symbols, comma-separated, as manual assets priced from your snapshots (later exports with -since keep them)")
	since := fl.String("since", "", "with -format pp or ghostfolio: the folder of the export you imported last; write only what it did not, and list what changed or went")
	redo := fl.String("redo", "", "with -format ghostfolio and -since: this account, which you deleted in Ghostfolio with its activities to redo it, written whole")
	out := fl.String("o", "", "write to this file, readable only by you, instead of stdout")
	dir := fl.String("dir", "", "write each table to a file of its own in this folder, with manifest.json (for pp and ghostfolio, a new folder)")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), "Usage of schwabtx export:\n\n  schwabtx export [flags]\n\n"+
			"Writes what the store holds as CSV or JSON Lines, or the ledger as files for Portfolio Performance or Ghostfolio. What -what names:\n\n")
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
	o := history.ExportOptions{Format: *format, Account: *account, All: *all, Raw: *raw, ShowAccounts: *show}
	for w := range strings.SplitSeq(*what, ",") {
		if w = strings.TrimSpace(w); w != "" {
			o.What = append(o.What, w)
		}
	}
	// Which flags were given, whatever their values: an export for a tool
	// refuses those that it does not take.
	given := map[string]bool{}
	fl.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if _, ok := toolFormats[*format]; ok {
		return runToolExport(ctx, toolFlags{
			format: *format, store: *store, account: *account, to: *to, manual: *manual, since: *since, redo: *redo, dir: *dir,
			what: o.What, given: given,
		}, stderr)
	}
	switch {
	case given["manual"]:
		return errors.New("-manual is for -format ghostfolio")
	case given["redo"]:
		return errors.New("-redo is for -format ghostfolio")
	case given["since"]:
		return errors.New("-since is for -format pp and ghostfolio")
	case *out != "" && *dir != "":
		return errors.New("-o and -dir name two places to write; give one")
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
	// The commands in what the store says name it.
	defer func() { err = errorWithStore(path, err) }()
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
		// The folder of an export for a tool is that export's: this one's
		// manifest would replace its manifest, from which the next export
		// for the tool goes on.
		if m, err := readToolManifest(*dir); err == nil {
			if _, ok := toolFormats[m.Format]; ok {
				return fmt.Errorf("-dir %s holds a %s export: write this one to another folder", *dir, m.Format)
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
	coverageLines(w, e.Coverage)
	waitingLine(w, path, e.Waiting)
}

// coverageLines says on w, a line for each account, the days that each
// source covers of it, which says which dates the rows are whole for.
func coverageLines(w io.Writer, coverage []history.ExportCoverage) {
	var account string
	var line []string
	flush := func() {
		if account != "" {
			fmt.Fprintf(w, "  %s: %s\n", account, strings.Join(line, "; "))
		}
	}
	for _, c := range coverage {
		label := fmt.Sprintf("A%d %s", c.AccountID, c.Account)
		if label != account {
			flush()
			account, line = label, nil
		}
		line = append(line, fmt.Sprintf("%s %s to %s", c.Source, c.From, c.To))
	}
	flush()
}

// waitingLine says on w that n file transactions of the store at path wait
// for you to resolve them, and so are not in an export of counted rows;
// nothing when none does.
func waitingLine(w io.Writer, path string, n int) {
	if n > 0 {
		fmt.Fprintf(w, "%d file %s for you to resolve %s (%s) and %s not in this export\n",
			n, pluralWord(n, "transaction waits", "transactions wait"), pluralWord(n, "it", "them"),
			copyCommand(path, "status", "-review"), pluralWord(n, "is", "are"))
	}
}

// A toolFormat is a format of an export for a tool (-format): the tool's
// name, and what the export writes in its folder (5.1 and 6.1 of the tool
// exports' design): the names of the files that every export writes, none
// of which may be the store's, and the patterns (path.Match) of the names
// of the others, its accounts' files and PP's configurations of any
// layout.
type toolFormat struct {
	name           string
	always, others []string
}

// toolFormats are the formats of the exports for a tool, by -format.
var toolFormats = map[string]toolFormat{
	"pp": {
		name:   "Portfolio Performance",
		always: []string{"manifest.json", "securities.csv", "pp-securities.json", "pp-account-transactions.json", "pp-portfolio-transactions.json"},
		others: []string{"account-transactions-A*.csv", "portfolio-transactions-A*.csv", "pp-*.json"},
	},
	"ghostfolio": {
		name:   "Ghostfolio",
		always: []string{"manifest.json", "ghostfolio.json"},
	},
}

// writes reports whether an export of the format writes a file of the name
// in its folder, whatever the name's case, which a file system may ignore.
func (t toolFormat) writes(name string) bool {
	name = strings.ToLower(name)
	for _, p := range slices.Concat(t.always, t.others) {
		if ok, _ := path.Match(strings.ToLower(p), name); ok {
			return true
		}
	}
	return false
}

// toolFlags are the flags of an export for a tool, -format pp or
// ghostfolio: their values, -what's names split, and which flags were
// given, whatever their values.
type toolFlags struct {
	format, store, account, to, manual, since, redo, dir string
	what                                                 []string
	given                                                map[string]bool
}

// runToolExport writes the ledger of the store as the files that the tool
// of f.format imports, with a manifest, to a new folder (8 and 9 of the
// tool exports' design): Portfolio Performance's CSV files and their
// configurations, or Ghostfolio's JSON. With -since it goes on from the
// export that you imported last: it writes only what the tool lacks,
// whatever its day, and lists what changed or went, to delete in the tool
// first; and for Ghostfolio, with -redo, it writes whole the account that
// you deleted there to redo it.
//
// Before it opens the store, it refuses the flags that are for csv and
// jsonl; a folder that holds an export already, whose files this one would
// replace before they are imported; and a -since that names no export of
// the format and layout, or one from which an export beside it has gone on
// already. Then it reads the store (ExportTool), writes the folder
// (WriteDir), and says on stderr what it wrote and what it asks of you
// (toolSummary). stdout gets nothing.
func runToolExport(ctx context.Context, f toolFlags, stderr io.Writer) (err error) {
	format := "-format " + f.format
	switch {
	case f.given["o"]:
		return fmt.Errorf("%s writes several files and a manifest: give -dir DIR, not -o", format)
	case f.dir == "":
		return fmt.Errorf("%s writes several files and a manifest: give -dir DIR", format)
	case slices.ContainsFunc(f.what, func(w string) bool { return w != "ledger" }):
		return fmt.Errorf("%s writes the ledger: -what is for csv and jsonl", format)
	case f.given["all"]:
		return fmt.Errorf("%s writes counted rows only: -all is for csv and jsonl", format)
	case f.given["raw"] || f.given["show-accounts"]:
		return fmt.Errorf("%s never writes full account numbers: -raw and -show-accounts are for csv and jsonl", format)
	case f.given["from"]:
		return fmt.Errorf("%s writes every row from the account's first, so that each position has its opening rows: -from is for csv and jsonl", format)
	case f.given["manual"] && f.format != "ghostfolio":
		return errors.New("-manual is for -format ghostfolio")
	case f.given["since"] && f.since == "":
		return errors.New("-since needs a folder: the one of the export that you imported last")
	case f.since != "" && f.given["account"]:
		return fmt.Errorf("-since %s goes on from the export in %s, with its account: drop -account", f.since, f.since)
	case f.given["redo"] && f.format != "ghostfolio":
		return errors.New("-redo is for -format ghostfolio: a redo in Portfolio Performance deletes every account and imports a full export (docs/tools.md)")
	case f.given["redo"] && strings.TrimSpace(f.redo) == "":
		return errors.New("-redo needs an account: the one that you deleted in Ghostfolio, with its activities, to redo it")
	case f.given["redo"] && f.since == "":
		return errors.New("-redo goes on from the export that you imported last: give its folder with -since (a full export redoes every account, docs/tools.md)")
	}
	o := history.ToolOptions{Tool: f.format, Account: f.account, SinceDir: f.since, Redo: strings.TrimSpace(f.redo)}
	if o.To, err = parseDay("-to", f.to); err != nil {
		return err
	}
	// ExportTool trims each symbol, and takes a blank one for none.
	if f.manual != "" {
		o.Manual = strings.Split(f.manual, ",")
	}
	path, err := storePath(f.store)
	if err != nil {
		return err
	}
	// The commands in what the store says name it.
	defer func() { err = errorWithStore(path, err) }()
	// No file of the folder may be the store or one of its files, by its
	// name too, since the folder need not exist yet: here those that every
	// export writes, and in WriteDir each account's too.
	for _, n := range toolFormats[f.format].always {
		if _, ok := companionOf(filepath.Join(f.dir, n), path); ok {
			return fmt.Errorf("-dir %s would put %s over the store itself (or one of its files); choose another folder", f.dir, n)
		}
	}
	if err := toolDirIsNew(f.dir, f.format); err != nil {
		return err
	}
	if f.since != "" {
		if o.Since, err = sinceManifest(f.since, f.format); err != nil {
			return err
		}
	}

	s, err := openStore(ctx, path, stderr)
	if err != nil {
		return err
	}
	e, err := s.ExportTool(ctx, o)
	if cerr := s.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := e.WriteDir(f.dir); err != nil {
		return err
	}
	toolSummary(stderr, path, e, f.dir, f.since)
	return nil
}

// toolDirIsNew refuses a folder dir that holds an export already, for a
// tool or not: a manifest.json, or a file of a name that an export of
// format writes (9 of the tool exports' design). Written there, the export
// would replace the files of one that may not be imported yet, or leave its
// own beside another's, and the manifest of the export that the next one
// goes on from. A missing folder is new: WriteDir makes it.
func toolDirIsNew(dir, format string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("-dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if toolFormats[format].writes(e.Name()) {
			return fmt.Errorf("-dir %s holds an export already: write each export to a new folder, so that none is overwritten before it is imported", dir)
		}
	}
	return nil
}

// A toolManifest is what a command reads of a tool export's manifest.json
// before it opens the store (9 of the tool exports' design): its
// exported_at, which names the export; its format and layout; its chain's
// id, the exported_at of the chain's first export; and the exported_at of
// the export that it went on from, "" for a full export. They are the
// manifest's keys exported_at, format, layout, chain and filters.
type toolManifest struct {
	ExportedAt string
	Format     string
	Layout     int
	Chain      struct {
		ID string `json:"id"`
	}
	Filters struct {
		Since string `json:"since"`
	}
}

// readToolManifest reads what the manifest.json of the folder dir says of
// the export in dir, if it is a tool export's (decodeToolManifest).
func readToolManifest(dir string) (toolManifest, error) {
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return toolManifest{}, err
	}
	defer f.Close()
	return decodeToolManifest(f)
}

// decodeToolManifest reads from r what a manifest.json says of the export
// (toolManifest), and of the JSON no more than it needs: an export writes
// those keys first, before its files and the chain's given, which grow with
// the history (8.1 of the tool exports' design), so that the check of
// every folder beside a -since folder reads little of each. A key that
// comes later is found all the same, at the cost of reading up to it; one
// that is not there is left zero.
func decodeToolManifest(r io.Reader) (toolManifest, error) {
	var m toolManifest
	d := json.NewDecoder(r)
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return m, cmp.Or(err, errors.New("a manifest is a JSON object"))
	}
	want := map[string]any{
		"exported_at": &m.ExportedAt, "format": &m.Format, "layout": &m.Layout, "chain": &m.Chain, "filters": &m.Filters,
	}
	for len(want) > 0 && d.More() {
		t, err := d.Token()
		if err != nil {
			return m, err
		}
		key, _ := t.(string)
		v, ok := want[key]
		if !ok {
			v = new(json.RawMessage)
		}
		delete(want, key)
		if err := d.Decode(v); err != nil {
			return m, err
		}
	}
	return m, nil
}

// sinceManifest returns the bytes of the manifest.json of the export in the
// folder dir, which an export of format goes on from with -since (3.8 and 9
// of the tool exports' design), once it has checked that it is the
// manifest of an export of format and of this layout, and that no export in
// a folder beside dir has gone on from it already: you name the export that
// you imported last, and an earlier one would list its rows to tick again,
// which, ticked again, would come in twice. ExportTool checks the rest of
// it.
func sinceManifest(dir, format string) ([]byte, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	var m toolManifest
	if err == nil {
		m, err = decodeToolManifest(bytes.NewReader(b))
	}
	if err != nil || m.Format != format || m.Layout != history.ToolLayout || m.ExportedAt == "" || m.Chain.ID == "" {
		return nil, fmt.Errorf("-since %s: %s is not the manifest of a -format %s export of layout %d",
			dir, filepath.Join(dir, "manifest.json"), format, history.ToolLayout)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("-since %s: %w", dir, err)
	}
	parent := filepath.Dir(abs)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, fmt.Errorf("-since %s: %w", dir, err)
	}
	// dir itself is among them, and never went on from itself.
	for _, e := range entries {
		next, err := readToolManifest(filepath.Join(parent, e.Name()))
		if err == nil && next.Format == m.Format && next.Chain.ID == m.Chain.ID && next.Filters.Since == m.ExportedAt {
			return nil, fmt.Errorf("-since %s: %s already went on from %s; name it with -since if you imported it, or move it away if you did not",
				dir, filepath.Join(dir, "..", e.Name()), dir)
		}
	}
	return b, nil
}

// toolSummary says on w what an export for a tool, of the store at path,
// wrote to the folder dir, going on from the export in the folder since (""
// for a full export), and what it asks of you (8.2 of the tool exports'
// design): what it wrote (toolWrote) and, going on, what it assumes that the
// tool holds, but the account that a redo writes whole; each account's
// coverage; what it counted that needs you (toolCountLines); what to delete
// in the tool before importing (toolDeletions); going on, the differences
// from the snapshots that the export it goes on from listed and that
// changed or went, with what to do with what you added by hand for them
// (differenceLines); for PP, the accounts to make, the order of the files
// and the rows to tick; for Ghostfolio, the cash that its imports leave as
// it is; going on, when the chain selects an account or, for Ghostfolio,
// names -manual symbols, the full export that a redo of every account
// imports; and the command that goes on from it.
func toolSummary(w io.Writer, path string, e *history.ToolExport, dir, since string) {
	tool, gf := toolFormats[e.Tool].name, e.Tool == "ghostfolio"
	manifest := filepath.Join(dir, "manifest.json")
	fmt.Fprintln(w, toolWrote(e, dir, since))
	if since != "" {
		done, of := "deleted and ticked", "of them"
		if gf {
			done, of = "deleted", "of their rows"
		}
		fmt.Fprintf(w, "  this assumes %s holds %s and every export before it, each imported once, with what it listed %s\n", tool, since, done)
		if e.Redo != "" {
			fmt.Fprintf(w, "  but not %s, which you deleted with its activities to redo it: this export writes it whole, with every balance, and lists anew what to do for it by hand\n", e.Redo)
		}
		if n := e.Late; n > 0 {
			fmt.Fprintf(w, "  %d %s %s dated on or before %s, the end of %s: %s late or %s decided since\n",
				n, of, pluralWord(n, "is", "are"), e.SinceTo, since, pluralWord(n, "it came", "they came"), pluralWord(n, "was", "were"))
		}
		if n := e.SameAs; n > 0 {
			fmt.Fprintf(w, "  %d %s %s that %s holds under another ref (%s lists %s under same_as): it shows %s as existing; leave %s unticked\n",
				n, of, pluralWord(n, "is a row", "are rows"), tool, manifest, pluralWord(n, "it", "them"), pluralWord(n, "it", "them"), pluralWord(n, "it", "them"))
		}
	}
	coverageLines(w, e.Coverage)
	toolCountLines(w, path, e, manifest)
	toolDeletions(w, e, manifest)
	differenceLines(w, e, manifest, since)
	if gf {
		fmt.Fprintf(w, "%s takes an account's balances only in the import that makes the account; later imports leave its cash as it is\n", tool)
		if since != "" {
			var cash []string
			for _, a := range e.Accounts {
				if a.Cash != "" {
					cash = append(cash, fmt.Sprintf("  %s: %s (%s)\n", a.Name, a.Cash, a.LastBalance))
				}
			}
			if len(cash) > 0 {
				fmt.Fprintf(w, "set each account's cash in %s to its latest snapshot, which imports do not change:\n%s", tool, strings.Join(cash, ""))
			}
		}
		if toolActivities(e) == 0 {
			fmt.Fprintf(w, "this file holds no activity: %s's import dialog cannot import it; its accounts and balances come with the next export that has one, or by POST /api/v1/import\n", tool)
		}
	} else {
		var accounts []history.ToolAccount
		for _, a := range e.Accounts {
			if a.New {
				accounts = append(accounts, a)
			}
		}
		if len(accounts) > 0 {
			fmt.Fprintf(w, "in %s, make a USD deposit account and a securities account for each account, named:\n", tool)
			for _, a := range accounts {
				label := a.Label
				if a.Nickname != "" {
					label += " (" + plainCell(a.Nickname) + ")"
				}
				fmt.Fprintf(w, "  %s: %s\n", label, a.Name)
			}
		}
		var files []string
		for _, f := range e.Files {
			files = append(files, f.Name)
		}
		fmt.Fprintf(w, "import in this order: %s\n", strings.Join(files, ", "))
		if len(e.Tick) > 0 {
			fmt.Fprintf(w, "tick these rows, which %s will show as existing although they are new; leave every other such row unticked:\n", tool)
			for _, t := range e.Tick {
				fmt.Fprintf(w, "  %s: %s: %s (%s)\n", t.File, toolAccountName(e, t.Account), ppShown(t.As), t.Ref)
			}
		}
	}
	if since != "" && (e.RedoAccount != "" || len(e.RedoManual) > 0) {
		args := []string{"export", "-format", e.Tool}
		if e.RedoAccount != "" {
			args = append(args, "-account", quoteArg(e.RedoAccount))
		}
		if len(e.RedoManual) > 0 {
			var manual []string
			for _, m := range e.RedoManual {
				manual = append(manual, mask.Text(m))
			}
			args = append(args, "-manual", quoteArg(strings.Join(manual, ",")))
		}
		args = append(args, "-dir", "<a new folder>")
		redo := "a redo"
		if gf {
			// One account is redone with -redo, which goes on from the
			// chain.
			redo = "a redo of every account"
		}
		fmt.Fprintf(w, "%s (docs/tools.md) imports a full export of this chain's selection: %s\n", redo, copyCommand(path, args...))
	}
	fmt.Fprintf(w, "next time: %s\n", copyCommand(path, "export", "-format", e.Tool, "-since", quoteArg(dir), "-dir", "<a new folder>"))
}

// toolWrote returns the first line that an export for a tool says, which
// wrote to dir, going on from the export in since, "" for none: what it
// wrote, and in which files. For PP, the rows of the ledger; its account
// and portfolio files; and the securities and prices of securities.csv.
// For Ghostfolio, the activities; the accounts of the file with their
// balances; the manual assets that an activity is of, and those written for
// their prices alone; and the symbols that its import asks Yahoo about.
func toolWrote(e *history.ToolExport, dir, since string) string {
	var written string
	var parts []string
	if e.Tool == "ghostfolio" {
		n := toolActivities(e)
		written = fmt.Sprintf("%d %s", n, pluralWord(n, "activity", "activities"))
		accounts, balances := 0, 0
		for _, a := range e.Accounts {
			if a.Exported {
				accounts++
				balances += a.Balances
			}
		}
		if accounts > 0 {
			s := plural(accounts, "account")
			if balances > 0 {
				s += " with " + plural(balances, "balance")
			}
			parts = append(parts, s)
		}
		switch {
		case e.Profiles > 0 && e.PricesOnly > 0:
			parts = append(parts, fmt.Sprintf("%s (%d more with prices only)", plural(e.Profiles, "manual asset"), e.PricesOnly))
		case e.Profiles > 0:
			parts = append(parts, plural(e.Profiles, "manual asset"))
		case e.PricesOnly > 0:
			parts = append(parts, plural(e.PricesOnly, "manual asset")+" with prices only")
		}
		if e.Yahoo > 0 {
			parts = append(parts, plural(e.Yahoo, "symbol")+" for Yahoo")
		}
	} else {
		written = plural(e.Written, "row")
		kinds := map[string]int{}
		for _, f := range e.Files {
			kinds[f.Kind]++
		}
		for _, k := range []string{"account", "portfolio"} {
			if n := kinds[k]; n > 0 {
				parts = append(parts, plural(n, k+" file"))
			}
		}
		if e.Securities > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", e.Securities, pluralWord(e.Securities, "security", "securities")))
		}
		if e.Prices > 0 {
			parts = append(parts, plural(e.Prices, "price"))
		}
	}
	line := fmt.Sprintf("wrote %s for %s to %s", written, toolFormats[e.Tool].name, dir)
	if since != "" {
		line += ", since " + since
	}
	line += ", up to " + string(e.To)
	if len(parts) > 0 {
		line += ": " + strings.Join(parts, ", ")
	}
	return line
}

// toolActivities returns how many activities a Ghostfolio export's file
// holds.
func toolActivities(e *history.ToolExport) int {
	n := 0
	for _, f := range e.Files {
		n += f.Rows
	}
	return n
}

// toolAccountName returns the name of the tool's account of the account of
// the export e labelled label: its label, for an account of the chain that
// the store does not hold.
func toolAccountName(e *history.ToolExport, label string) string {
	for _, a := range e.Accounts {
		if a.Label == label && a.Name != "" {
			return a.Name
		}
	}
	return label
}

// toolCountLines says on w, of the export e for a tool from the store at
// path, whose manifest is manifest, a line for each kind of row or
// security that needs you, with how many there are, when there are some:
// for Ghostfolio, the cash rows that the balances carry or not yet; the
// rows left out and those written with a caveat, by code; the securities
// whose symbol changed; the holdings that differ from the latest snapshot;
// the rows flagged; the file transactions that wait for you; and for
// Ghostfolio, the -manual symbols that nothing uses, masked, and the
// symbols that Yahoo may no longer know. Going on from another export, it
// counts no row or holding that that one listed alike.
func toolCountLines(w io.Writer, path string, e *history.ToolExport, manifest string) {
	tool, gf := toolFormats[e.Tool].name, e.Tool == "ghostfolio"
	if gf {
		notIn := 0
		var taken []string
		for _, c := range e.NotInBalances {
			notIn += c.Rows
			if c.Snapshot == "" {
				taken = append(taken, c.Account+": none")
				continue
			}
			taken = append(taken, fmt.Sprintf("%s: of %s, taken %s in New York", c.Account, c.Snapshot, c.Taken.In(nytime.NewYork()).Format("2006-01-02 15:04")))
		}
		cash := func(n int) string {
			return fmt.Sprintf("%d %s", n, pluralWord(n, "deposit, withdrawal or journal is", "deposits, withdrawals and journals are"))
		}
		later := fmt.Sprintf("posted after the latest snapshot was taken (%s), which a later one will carry", strings.Join(taken, "; "))
		switch {
		case e.InBalances > 0 && notIn > 0:
			fmt.Fprintf(w, "%s in the accounts' balances; %d %s not yet, %s\n", cash(e.InBalances), notIn, pluralWord(notIn, "is", "are"), later)
		case e.InBalances > 0:
			fmt.Fprintf(w, "%s in the accounts' balances\n", cash(e.InBalances))
		case notIn > 0:
			fmt.Fprintf(w, "%s not in the accounts' balances yet, %s\n", cash(notIn), later)
		}
	}
	if n := toolRows(e.LeftOut); n > 0 {
		fmt.Fprintf(w, "left out %s (%s lists each, with what to do): %s\n", plural(n, "row"), manifest, toolCounts(e.LeftOut))
	}
	if n := e.CaveatRows; n > 0 {
		fmt.Fprintf(w, "wrote %s with %s (%s): %s\n", plural(n, "row"), pluralWord(n, "a caveat", "caveats"), manifest, toolCounts(e.Caveats))
	}
	if n := e.SymbolsChanged; n > 0 {
		it := pluralWord(n, "it", "them")
		advice := fmt.Sprintf("if %s holds %s under an earlier ticker, give %s the new one; once delisted, no price feed", tool, it, it)
		if gf {
			advice = fmt.Sprintf("if %s holds activities of %s under an earlier symbol, delete those that an export wrote, and move to the new one those that you added by hand: it does not follow the change", tool, it)
		}
		fmt.Fprintf(w, "%d %s changed symbol (%s): %s (docs/tools.md)\n", n, pluralWord(n, "security has", "securities have"), manifest, advice)
	}
	unsureLines(w, path, e.Unsure, manifest)
	switch n := e.Holdings; {
	case n == 1:
		fmt.Fprintf(w, "1 holding differs from the latest snapshot (%s): a position opened before the store's rows, or a split left out; add it by hand, less the rows left out that you book by hand (docs/tools.md)\n", manifest)
	case n > 1:
		fmt.Fprintf(w, "%d holdings differ from the latest snapshot (%s): positions opened before the store's rows, or splits left out; add them by hand, less the rows left out that you book by hand (docs/tools.md)\n", n, manifest)
	}
	switch n := e.Cash; {
	case n == 1:
		fmt.Fprintf(w, "1 account's cash in %s differs from its latest snapshot's (%s): cash held before the store's first row of the account, or rows left out; book the difference, less the rows left out that you book by hand, as an opening deposit or withdrawal (docs/tools.md)\n", tool, manifest)
	case n > 1:
		fmt.Fprintf(w, "%d accounts' cash in %s differs from their latest snapshots' (%s): cash held before the store's first row of each, or rows left out; book each difference, less the rows left out that you book by hand, as an opening deposit or withdrawal (docs/tools.md)\n", n, tool, manifest)
	}
	switch n := len(e.CashNotChecked); {
	case n == 1:
		fmt.Fprintf(w, "1 account's cash in %s is not checked (%s): the latest snapshot of %s, a margin account, reads its cash as 0 beside a margin or short balance, as Schwab can report a debit or short positions; compare its cash at Schwab with %s's by hand (docs/tools.md)\n",
			tool, manifest, toolAccountName(e, e.CashNotChecked[0]), tool)
	case n > 1:
		var names []string
		for _, a := range e.CashNotChecked {
			names = append(names, toolAccountName(e, a))
		}
		fmt.Fprintf(w, "%d accounts' cash in %s is not checked (%s): the latest snapshots of %s, margin accounts, read their cash as 0 beside a margin or short balance, as Schwab can report a debit or short positions; compare each one's cash at Schwab with %s's by hand (docs/tools.md)\n",
			n, tool, manifest, strings.Join(names, " and "), tool)
	}
	// The rows flagged are written, given, combined or, for Ghostfolio, in
	// the balances, which no export lists to delete: a balance is Schwab's.
	if n := e.Flagged; n > 0 {
		fmt.Fprintf(w, "%d %s of this export %s a flag that needs you (%s); if you uncount %s, the next -since export lists what to delete\n",
			n, pluralWord(n, "row"), pluralWord(n, "carries", "carry"), copyCommand(path, "status", "-review"), pluralWord(n, "it", "one"))
	}
	waitingLine(w, path, e.Waiting)
	if !gf {
		return
	}
	if len(e.Unused) > 0 {
		var unused []string
		for _, u := range e.Unused {
			unused = append(unused, plainCell(mask.Text(u)))
		}
		fmt.Fprintf(w, "-manual names %s, which nothing in this export uses\n", strings.Join(unused, " and "))
	}
	switch n := e.YahooIdle; {
	case n == 1:
		fmt.Fprintf(w, "1 symbol for Yahoo has no row since %s and is not held (%s lists it): Yahoo may no longer know it; look it up, and if Yahoo lacks it, name it in -manual\n",
			e.IdleSince, manifest)
	case n > 1:
		fmt.Fprintf(w, "%d symbols for Yahoo have no row since %s and are not held (%s lists them): Yahoo may no longer know them; look them up, and name those it lacks in one -manual\n",
			n, e.IdleSince, manifest)
	}
}

// unsureLines says on w which keys of the store, with no CUSIP, had a
// ticker that other securities have had too, of other CUSIPs or at another
// time, which an export whose manifest is manifest cannot be sure of
// (history.ToolUnsure): how it writes each, and the command that says which
// security each is, for each that it may be, in the store at path, with its
// CUSIP when the store knows one.
func unsureLines(w io.Writer, path string, unsure []history.ToolUnsure, manifest string) {
	switch n := len(unsure); {
	case n == 1:
		fmt.Fprintf(w, "1 key of the store with no CUSIP had a ticker that other securities have had too (%s lists it): the export cannot tell which it is; say which in the store (docs/tools.md):\n", manifest)
	case n > 1:
		fmt.Fprintf(w, "%d keys of the store with no CUSIP had tickers that other securities have had too (%s lists them): the export cannot tell which each is; say which in the store (docs/tools.md):\n", n, manifest)
	}
	for _, u := range unsure {
		// Whether each security that it is not written as, of one at least,
		// had its ticker at another time.
		others, apart := 0, true
		for _, s := range u.Securities {
			if s.Key == "" || s.Key != u.TakenFor {
				others++
				apart = apart && s.Apart
			}
		}
		apart = apart && others > 0
		var as string
		switch {
		case u.TakenFor != "" && apart:
			as = "written as " + u.TakenFor + ", the others having had its ticker at other times"
		case u.TakenFor != "":
			as = "written as " + u.TakenFor + ", which had it first"
		case apart:
			as = "on its own, each of the others having had its ticker at another time"
		default:
			as = "on its own, its tickers each of another"
		}
		var commands []string
		for _, s := range u.Securities {
			if s.Key == "" {
				continue
			}
			c := "-same " + quoteArg("K:"+s.Key)
			if len(commands) == 0 {
				c = copyCommand(path, "edit", quoteArg("K:"+u.Key), "-same", quoteArg("K:"+s.Key))
			}
			if s.CUSIP != "" {
				c += " (" + s.CUSIP + ")"
			}
			commands = append(commands, c)
		}
		fmt.Fprintf(w, "  %s (%s), %s: %s\n", plainCell(u.Key), plainCell(strings.Join(u.Tickers, ", ")), as, strings.Join(commands, " or "))
	}
}

// toolDeletions says on w what of the export e for a tool changed or went,
// whose manifest is manifest: what the tool holds of each row, its account,
// day, type and value, and how its PP Note or Ghostfolio comment names it,
// for you to delete in the tool before importing; and for a row gone, what
// became of it.
func toolDeletions(w io.Writer, e *history.ToolExport, manifest string) {
	if len(e.Changed)+len(e.Gone) == 0 {
		return
	}
	gf := e.Tool == "ghostfolio"
	fmt.Fprintf(w, "before importing, delete in %s what changed or went (%s lists each):\n", toolFormats[e.Tool].name, manifest)
	// held says what the tool shows of row c and how its PP Note or
	// Ghostfolio comment begins: a transfer's Note names its paying side
	// first, in both accounts.
	held := func(c history.ToolChange) string {
		note, was, where := c.Account+" "+cmp.Or(c.NoteRef, c.Ref), c.Was, "comment"
		if !gf {
			was, where = ppShown(was), "note"
			if c.Transfer != "" {
				note = c.Transfer
			}
		}
		return fmt.Sprintf("%s: %s (%s schwabtx %s)", toolAccountName(e, c.Account), was, where, note)
	}
	for _, c := range e.Changed {
		fmt.Fprintf(w, "  %s: changed; the new form is in this export\n", held(c))
	}
	for _, g := range e.Gone {
		// "uncounted: user" reads "uncounted (user)".
		why := g.Why
		if what, detail, ok := strings.Cut(why, ": "); ok {
			why = what + " (" + detail + ")"
		}
		fmt.Fprintf(w, "  %s: gone, %s\n", held(g), why)
	}
}

// differenceLines says on w, of the export e for a tool, going on from the
// export in the folder since, whose manifest is manifest, each difference
// of the holdings check and of PP's cash check that that export listed and
// that this one lists with another, or not at all (e.DifferencesChanged),
// with its change: what you added or booked by hand in the tool for it is
// to change by the change, less the rows left out that you book by hand,
// or to delete, as the rows written stand for it now; in Ghostfolio, which
// does not follow a change of symbol, it is to move to the asset that holds
// the security now, when that is another, each named as Ghostfolio shows
// it. In PP that comes before importing, since PP marks a new row equal to
// one that it holds as existing, and the rows that a hand entry stood for,
// arriving as it was booked, would stay out; in Ghostfolio after, since the
// import makes the MANUAL asset to move to, and an activity that an export
// writes, its comment its own, is never equal to one added by hand.
func differenceLines(w io.Writer, e *history.ToolExport, manifest, since string) {
	n := len(e.DifferencesChanged)
	if n == 0 {
		return
	}
	tool := toolFormats[e.Tool].name
	move, when, why := "", "before", ": "+tool+" marks a new row equal to one that it holds as existing"
	if e.Tool == "ghostfolio" {
		when, why = "after", ""
	}
	if slices.ContainsFunc(e.DifferencesChanged, func(d history.ToolDifference) bool { return d.WasAsset != "" }) {
		move = ", or move it to the asset that Ghostfolio holds the security under now"
	}
	fmt.Fprintf(w, "%d %s that %s listed %s changed or gone (%s lists each, with was): %s importing, change what you added in %s by hand for %s by its change, less the rows left out that you book by hand%s, or delete it%s (docs/tools.md):\n",
		n, pluralWord(n, "difference", "differences"), since, pluralWord(n, "has", "have"), manifest, when, tool, pluralWord(n, "it", "each"), move, why)
	for _, d := range e.DifferencesChanged {
		what := "cash"
		if !d.Cash {
			what = plainCell(mask.Text(cmp.Or(d.Asset, d.Symbol, d.Key)))
		}
		switch {
		case d.Now == "":
			fmt.Fprintf(w, "  %s: %s: gone, was %s: delete\n", toolAccountName(e, d.Account), what, d.Was)
		case d.WasAsset != "":
			was := plainCell(mask.Text(d.WasAsset))
			change := ""
			if d.Change != "0" {
				change = ", changed by " + d.Change
			}
			fmt.Fprintf(w, "  %s: %s: %s, was %s under %s: delete it under %s and add it under %s%s\n",
				toolAccountName(e, d.Account), what, d.Now, d.Was, was, was, what, change)
		default:
			fmt.Fprintf(w, "  %s: %s: %s, was %s: change by %s\n", toolAccountName(e, d.Account), what, d.Now, d.Was, d.Change)
		}
	}
}

// toolRows returns how many rows counts counts.
func toolRows(counts []history.ToolCount) int {
	n := 0
	for _, c := range counts {
		n += c.Count
	}
	return n
}

// toolCounts returns counts as stderr says them: each code and its count,
// in their order.
func toolCounts(counts []history.ToolCount) string {
	var parts []string
	for _, c := range counts {
		parts = append(parts, fmt.Sprintf("%s %d", c.Code, c.Count))
	}
	return strings.Join(parts, ", ")
}

// ppShown returns what PP shows of a row's records, as its manifest writes
// them, each with its file's kind first ("account 2024-04-04 Dividend
// 3.86"), without that kind: the day, Type and Value.
func ppShown(as string) string {
	records := strings.Split(as, "; ")
	for i, r := range records {
		if kind, rest, ok := strings.Cut(r, " "); ok && (kind == "account" || kind == "portfolio") {
			records[i] = rest
		}
	}
	return strings.Join(records, "; ")
}
