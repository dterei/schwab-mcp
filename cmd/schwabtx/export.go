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
			format: *format, store: *store, account: *account, to: *to, manual: *manual, since: *since, dir: *dir,
			what: o.What, given: given,
		}, stderr)
	}
	switch {
	case given["manual"]:
		return errors.New("-manual is for -format ghostfolio")
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
	format, store, account, to, manual, since, dir string
	what                                           []string
	given                                          map[string]bool
}

// runToolExport writes the ledger of the store as the files that the tool
// of f.format imports, with a manifest, to a new folder (8 and 9 of the
// tool exports' design): Portfolio Performance's CSV files and their
// configurations, or Ghostfolio's JSON. With -since it goes on from the
// export that you imported last: it writes only what the tool lacks,
// whatever its day, and lists what changed or went, to delete in the tool
// first.
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
	}
	o := history.ToolOptions{Tool: f.format, Account: f.account, SinceDir: f.since}
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
// design): what it wrote (toolWrote) and what it assumes that the tool
// holds: for a full export, none of its accounts, as on a first import or a
// redo of every account, and going on, the exports of the chain; each
// account's coverage; what it counted that needs you (toolCountLines); for
// PP, the accounts to make (ppAccountsToMake); what to delete in the tool
// before importing (toolDeletions), going on with what you booked for a row
// that the export before it left out and that this one writes
// (leftOutWrittenLines); the differences from the snapshots, which what you
// add by hand is to equal, going on those new, changed or gone
// (differenceLines), and the accounts that the checks do not check
// (notCheckedLines); for PP, the order of the files and the rows to tick;
// for Ghostfolio, the cash that its imports leave as it is, and the
// accounts that its file gives no balance (noBalanceLine); going on, when
// the chain selects an account or, for Ghostfolio, names -manual symbols,
// the full export that a redo imports; and the command that goes on from
// it.
func toolSummary(w io.Writer, path string, e *history.ToolExport, dir, since string) {
	tool, gf := toolFormats[e.Tool].name, e.Tool == "ghostfolio"
	manifest := filepath.Join(dir, "manifest.json")
	fmt.Fprintln(w, toolWrote(e, dir, since))
	if since == "" {
		// A full export reads no chain: imported over what the tool holds,
		// it is not a redo.
		held := "none of these accounts' rows: a first import into new, empty accounts"
		if gf {
			held = "none of these accounts: a first import"
		}
		fmt.Fprintf(w, "  this assumes %s holds %s, or a redo of every account (docs/tools.md)\n", tool, held)
	} else {
		done, of := "deleted and ticked", "of them"
		if gf {
			done, of = "deleted", "of their rows"
		}
		fmt.Fprintf(w, "  this assumes %s holds %s and every export before it, each imported once, with what it listed %s\n", tool, since, done)
		if n := e.Late; n > 0 {
			fmt.Fprintf(w, "  %d %s %s dated on or before %s, the end of %s: %s late, %s decided or took a new ref since\n",
				n, of, pluralWord(n, "is", "are"), e.SinceTo, since, pluralWord(n, "it came", "they came"), pluralWord(n, "was", "were"))
		}
	}
	coverageLines(w, e.Coverage)
	toolCountLines(w, path, e, manifest)
	if !gf {
		ppAccountsToMake(w, e)
	}
	toolDeletions(w, e, manifest)
	leftOutWrittenLines(w, e, manifest, since)
	differenceLines(w, e, manifest, since)
	notCheckedLines(w, e, manifest)
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
		noBalanceLine(w, e, manifest)
		if toolActivities(e) == 0 {
			fmt.Fprintf(w, "this file holds no activity: %s's import dialog cannot import it; its accounts and balances come with the next export that has one, or by POST /api/v1/import; the next export lists its differences again, and those gone\n", tool)
		}
	} else {
		var files []string
		for _, f := range e.Files {
			files = append(files, f.Name)
		}
		fmt.Fprintf(w, "import in this order: %s\n", strings.Join(files, ", "))
		if len(e.Tick) > 0 {
			fmt.Fprintf(w, "tick these rows, which %s will show as existing although they are new (it marks no other row of this export but one equal to an entry that you booked by hand for a difference, which you tick too, docs/tools.md step 3):\n", tool)
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
		fmt.Fprintf(w, "a redo (docs/tools.md) imports a full export of this chain's selection: %s\n", copyCommand(path, args...))
	}
	fmt.Fprintf(w, "next time: %s\n", copyCommand(path, "export", "-format", e.Tool, "-since", quoteArg(dir), "-dir", "<a new folder>"))
}

// noBalanceLine says on w which accounts of the export e for Ghostfolio,
// whose manifest is manifest, its file gives no balance, as none of their
// snapshots holds a cash figure, or they have none: Ghostfolio's cash of
// such an account is what you set by hand, which no export sets until a
// snapshot holds its cash.
func noBalanceLine(w io.Writer, e *history.ToolExport, manifest string) {
	var none []string
	for _, a := range e.Accounts {
		switch {
		case !a.Exported || a.Balances > 0 || a.NoBalances == "":
		case a.NoBalances == "no snapshot":
			none = append(none, a.Name+", which has no snapshot")
		default:
			none = append(none, a.Name+", none of whose snapshots holds a cash figure")
		}
	}
	if n := len(none); n > 0 {
		fmt.Fprintf(w, "%s no cash balance in this file (%s says why under accounts): %s: set %s in %s by hand to what Schwab shows (docs/tools.md step 2)\n",
			pluralWord(n, "1 account has", fmt.Sprintf("%d accounts have", n)), manifest, strings.Join(none, ", and "), pluralWord(n, "its cash", "the cash of each"), toolFormats[e.Tool].name)
	}
}

// ppAccountsToMake says on w which accounts to make in PP for the export e
// for PP, by the names that its files give them: those that no export of
// its chain before it wrote or named (ToolAccount.New). It comes before
// what to delete and what to book by hand, as docs/tools.md's steps 2 and 6
// have you make them first: an empty account touches nothing that PP
// holds, and what you book for a difference of a new account goes into it.
func ppAccountsToMake(w io.Writer, e *history.ToolExport) {
	var accounts []history.ToolAccount
	for _, a := range e.Accounts {
		if a.New {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		return
	}
	fmt.Fprintf(w, "in %s, make a USD deposit account and a securities account for each account, named:\n", toolFormats[e.Tool].name)
	for _, a := range accounts {
		label := a.Label
		if a.Nickname != "" {
			label += " (" + plainCell(a.Nickname) + ")"
		}
		fmt.Fprintf(w, "  %s: %s\n", label, a.Name)
	}
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
// rows left out and those written with a caveat, by code; for PP, the
// securities that it may hold otherwise than written, or that may no longer
// trade, in one wording for a full export, which may be a first import or
// a redo, and one for -since; the keys of the store that the export cannot
// be sure of (unsureLines); the rows flagged; the file transactions that
// wait for you; and for Ghostfolio, the -manual symbols that nothing uses,
// masked, and the symbols that Yahoo may no longer know. Going on from
// another export, it counts no row that that one listed alike.
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
	// PP's alone: Ghostfolio holds a security written under another symbol
	// as another asset, of which the export lists the rows given as changed.
	switch n := e.SymbolsChanged; {
	case n == 1 && e.Since == "":
		fmt.Fprintf(w, "1 security may be held by %s otherwise than written, or may no longer trade (%s lists it under symbols_changed, with why): on a first import into empty accounts it asks something only if named_by_cusip, after importing; on a redo, %[1]s holds it from before: set it as docs/tools.md step 8 says before you import\n", tool, manifest)
	case n > 1 && e.Since == "":
		fmt.Fprintf(w, "%d securities may be held by %s otherwise than written, or may no longer trade (%s lists each under symbols_changed, with why): on a first import into empty accounts only those named_by_cusip ask anything, after importing; on a redo, %[2]s holds them from before: set each as docs/tools.md step 8 says before you import\n", n, tool, manifest)
	case n == 1:
		fmt.Fprintf(w, "1 security has changed symbol or may no longer trade (%s lists it under symbols_changed, with why): before you import, set it as docs/tools.md step 8 says if %s holds it; if it is new there, it asks something only if named_by_cusip, after importing\n", manifest, tool)
	case n > 1:
		fmt.Fprintf(w, "%d securities have changed symbol or may no longer trade (%s lists each under symbols_changed, with why): before you import, set each that %s holds as docs/tools.md step 8 says; one that is new to it asks only, if named_by_cusip, after importing\n", n, manifest, tool)
	}
	unsureLines(w, path, e.Unsure, manifest)
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
		fmt.Fprintf(w, "1 symbol for Yahoo may no longer be the security written (%s lists it under ghostfolio.yahoo_idle): no row since %s and not held, or named by a CUSIP; look it up on Yahoo, and if it lacks it or shows it as another company, name it in -manual (docs/tools.md step 1)\n",
			manifest, e.IdleSince)
	case n > 1:
		fmt.Fprintf(w, "%d symbols for Yahoo may no longer be the securities written (%s lists them under ghostfolio.yahoo_idle): no row since %s and not held, or named by a CUSIP; look each up on Yahoo, and name in one -manual those that it lacks or shows as another company (docs/tools.md step 1)\n",
			n, manifest, e.IdleSince)
	}
}

// notCheckedLines says on w, of the export e for a tool, whose manifest is
// manifest, each kind of account that its checks of the snapshots do not
// check, with the step that checks it or what to do instead: for PP, the
// accounts whose cash is not checked, each with why; the accounts with rows
// and no snapshot to check against; and, for PP, those with snapshots that
// PP holds nothing of. The manifest carries on what the export before it
// listed of the first two kinds, and an account whose cash it does not
// check with no figures when nothing of its cash is listed, so the next
// export that checks one lists its differences again, never as before, or
// as gone.
func notCheckedLines(w io.Writer, e *history.ToolExport, manifest string) {
	tool := toolFormats[e.Tool].name
	names := func(labels []string) string {
		var out []string
		for _, l := range labels {
			out = append(out, toolAccountName(e, l))
		}
		return strings.Join(out, " and ")
	}
	if n := len(e.CashNotChecked); n > 0 {
		var why []string
		for _, u := range e.CashNotChecked {
			if u.Why == "margin" {
				why = append(why, toolAccountName(e, u.Account)+", a margin account with a margin or short balance at its latest snapshot")
			} else {
				why = append(why, toolAccountName(e, u.Account)+", whose latest snapshot holds no cash figure (take another with schwabtx snapshot)")
			}
		}
		it := pluralWord(n, "it", "each")
		fmt.Fprintf(w, "%s cash in %s is not checked (%s lists %s under holdings.cash_not_checked): %s: compare the cash that Schwab shows for %s with the balance of its deposit account in %s, and book the difference as step 4 says; the next export that checks %s lists its difference again, or as gone when it finds none (docs/tools.md step 4)\n",
			pluralWord(n, "1 account's", fmt.Sprintf("%d accounts'", n)), tool, manifest, pluralWord(n, "it", "them"), strings.Join(why, ", and "), it, tool, it)
	}
	if n := len(e.NoSnapshot); n > 0 {
		fmt.Fprintf(w, "%s rows and no snapshot to check against (%s lists %s under holdings.not_checked): %s: %s with schwabtx snapshot (an account that Schwab has closed has none); the next export that checks %s lists its differences\n",
			pluralWord(n, "1 account has", fmt.Sprintf("%d accounts have", n)), manifest, pluralWord(n, "it", "them"), names(e.NoSnapshot), pluralWord(n, "take one", "take one of each"),
			pluralWord(n, "it", "each"))
	}
	if n := len(e.NotInTool); n > 0 {
		fmt.Fprintf(w, "%s snapshots and nothing in %s (%s lists %s under holdings.not_checked): %s %s no row that this export or its chain writes or names; import %s History files (docs/store.md) to have %s there\n",
			pluralWord(n, "1 account has", fmt.Sprintf("%d accounts have", n)), tool, manifest, pluralWord(n, "it", "them"), names(e.NotInTool),
			pluralWord(n, "has", "have"), pluralWord(n, "its", "their"), pluralWord(n, "it", "them"))
	}
}

// unsureLines says on w which keys of the store, with no CUSIP, share a
// ticker with another security that an export whose manifest is manifest
// cannot join them to (history.ToolUnsure), but those that the export it
// goes on from listed alike: each is written on its own, and you judge
// which security it is by what the store knows of it and of each that it
// may be, which its line says, with the command, for the store at path,
// that says it is that one.
func unsureLines(w io.Writer, path string, unsure []history.ToolUnsure, manifest string) {
	var listed []history.ToolUnsure
	for _, u := range unsure {
		if !u.Before {
			listed = append(listed, u)
		}
	}
	status := copyCommand(path, "status", "K:…")
	switch n := len(listed); {
	case n == 1:
		fmt.Fprintf(w, "1 key of the store with no CUSIP shares a ticker with another security that the export cannot join it to (%s lists it under keys_unsure, with what the store knows of it): it is written on its own, which neither tool merges with another. Before you import, judge it by its names and the days of its rows (%s shows its names over time): if it is the security named after it, run that command and export again; if it is not, run nothing and book its differences as any other's (docs/tools.md):\n", manifest, status)
	case n > 1:
		fmt.Fprintf(w, "%d keys of the store with no CUSIP share a ticker with another security that the export cannot join them to (%s lists them under keys_unsure, with what the store knows of each): each is written on its own, which neither tool merges with another. Before you import, judge each by its names and the days of its rows (%s shows its names over time): if it is the security named after it, run that command and export again; if it is not, run nothing and book its differences as any other's (docs/tools.md):\n", n, manifest, status)
	}
	for _, u := range listed {
		var commands []string
		for _, s := range u.Securities {
			facts := unsureFacts(s.Facts, s.CUSIP)
			var c string
			switch {
			case s.Key == "":
				c = "the rows of " + plainCell(s.Symbol) + " without a key"
			case u.Key == "" && len(commands) == 0:
				c = copyCommand(path, "edit", "REF", "-security", quoteArg("K:"+s.Key))
			case u.Key == "":
				c = "-security " + quoteArg("K:"+s.Key)
			case len(commands) == 0:
				c = copyCommand(path, "edit", quoteArg("K:"+u.Key), "-same", quoteArg("K:"+s.Key))
			default:
				c = "-same " + quoteArg("K:"+s.Key)
			}
			if facts != "" {
				c += " (" + facts + ")"
			}
			commands = append(commands, c)
		}
		if u.Key == "" {
			fmt.Fprintf(w, "  %s, %s without a key (%s): %s for each; settling the keys of %[1]s first may let the store place them\n",
				plainCell(u.Symbol), plural(len(u.Refs), "row"), strings.Join(u.Refs, ", "), strings.Join(commands, " or "))
			continue
		}
		key := plainCell(u.Key)
		if facts := unsureFacts(u.Facts, ""); facts != "" {
			key += ", " + facts
		}
		fmt.Fprintf(w, "  %s: %s\n", key, strings.Join(commands, " or "))
	}
}

// unsureFacts returns, as a key's line says them, what the store knows of a
// security that keys_unsure names: its descriptions; its CUSIP, "" for
// none; each of its tickers, from Schwab's API, in History files or both;
// the days of the export's rows of it; and held, when an account holds it.
func unsureFacts(f history.ToolFacts, cusip string) string {
	var parts []string
	if len(f.Descriptions) > 0 {
		parts = append(parts, plainCell(strings.Join(f.Descriptions, "; ")))
	}
	if cusip != "" {
		parts = append(parts, "CUSIP "+plainCell(cusip))
	}
	var tickers []string
	sources := map[string][]string{}
	for _, t := range f.Tickers {
		if _, ok := sources[t.Ticker]; !ok {
			tickers = append(tickers, t.Ticker)
		}
		where := "from Schwab's API"
		if t.Source == "history" {
			where = "in History files"
		}
		sources[t.Ticker] = append(sources[t.Ticker], where)
	}
	for _, t := range tickers {
		parts = append(parts, plainCell(t)+" "+strings.Join(sources[t], " and "))
	}
	switch f.First {
	case "":
	case f.Last:
		parts = append(parts, "rows "+string(f.First))
	default:
		parts = append(parts, "rows "+string(f.First)+" to "+string(f.Last))
	}
	if f.Held {
		parts = append(parts, "held")
	}
	return strings.Join(parts, ", ")
}

// leftOutWrittenLines says on w, of the export e for a tool, going on from
// the export in the folder since, whose manifest is manifest, each row that
// the export before it left out, advising you to book it by hand or to
// check it, and that e writes (e.LeftOutWritten): what you booked by hand
// for it is to delete before importing, as its records come with e, and
// the tool would hold them twice; for PP, whose review page marks a new row
// equal to one that it holds as existing, a booking equal to it would also
// keep the row out.
func leftOutWrittenLines(w io.Writer, e *history.ToolExport, manifest, since string) {
	n := len(e.LeftOutWritten)
	if n == 0 {
		return
	}
	tool, booked, step := toolFormats[e.Tool].name, "booked", "6"
	if e.Tool == "ghostfolio" {
		booked, step = "added", "7"
	}
	fmt.Fprintf(w, "%s that %s left out %s written now (%s lists %s under left_out_written): before importing, delete in %s what you %s by hand for %s, if anything (docs/tools.md step %s):\n",
		plural(n, "row"), since, pluralWord(n, "is", "are"), manifest, pluralWord(n, "it", "each"), tool, booked, pluralWord(n, "it", "each"), step)
	for _, l := range e.LeftOutWritten {
		now := l.Now
		if e.Tool != "ghostfolio" {
			now = ppShown(now)
		}
		fmt.Fprintf(w, "  %s: %s (%s; left out %s, %s)\n", toolAccountName(e, l.Account), now, l.Ref, l.Reason, l.Advice)
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
	// Ghostfolio comment begins: its label and ref, or for PP what its
	// manifest's note says in their place, as a transfer's head, which names
	// its paying side first.
	held := func(c history.ToolChange) string {
		note, was, where := cmp.Or(c.Note, c.Account+" "+c.Ref), c.Was, "comment"
		if !gf {
			was, where = ppShown(was), "note"
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
// export in the folder since, "" for a full export, whose manifest is
// manifest, each difference from the latest snapshots that the export lists
// (e.Differences, K-7 of the consolidation), gone ones first: what you add
// by hand in the tool for a security, or for an account's opening cash, is
// to equal its difference, less what the rows left out that you book by
// hand move, which a line says where they move it, and is to go for one
// gone. Each line names the security as the tool finds it, which holds what
// you added: for PP by its WKN, ticker or name, for Ghostfolio by its asset,
// as Ghostfolio shows it, with its data source. For a full export you add
// it after importing, into accounts that held none of its rows. Going on,
// in PP that comes before importing, since PP marks a new row equal to one
// that it holds as existing, and the rows that a hand entry stood for,
// arriving as it was booked, would stay out, but for a security that PP
// does not hold yet, which the import brings, after; in Ghostfolio after,
// since the import makes a new MANUAL asset to add under, and an activity
// that an export writes, its comment its own, is never equal to one added
// by hand. A line marks a MANUAL asset of Ghostfolio that no activity of the
// export brings in, which you make first (NoActivity).
func differenceLines(w io.Writer, e *history.ToolExport, manifest, since string) {
	gone := 0
	for _, d := range e.Differences {
		if d.Gone {
			gone++
		}
	}
	n := len(e.Differences) - gone
	if n+gone == 0 {
		return
	}
	tool, gf := toolFormats[e.Tool].name, e.Tool == "ghostfolio"
	// Ghostfolio's hand entry is of the asset that a line names.
	under, signs := "", "; a positive one is what Schwab holds and the records lack, a negative one what the records hold and Schwab does not"
	equal := " equal to it, less what the rows left out that you book by hand move (a line says so where they move it)"
	underEqual := equal
	if gf {
		under = ", under the asset named"
		underEqual = under + "," + equal
	}
	switch {
	case since == "" && gf:
		fmt.Fprintf(w, "%s from the latest snapshots (%s lists %s under holdings, with the asset that holds it): after importing, make what you add by hand in %s for each%s%s (docs/tools.md step 6):\n",
			pluralWord(n, "1 holding differs", fmt.Sprintf("%d holdings differ", n)), manifest, pluralWord(n, "it", "each"), tool, underEqual, signs)
	case since == "":
		fmt.Fprintf(w, "%s from the latest snapshots (%s lists %s under holdings, with the snapshot's figure and the records'): after importing, make what you add by hand in %s for each%s%s (docs/tools.md step 4 says what to book for each):\n",
			plural(n, "difference"), manifest, pluralWord(n, "it", "each"), tool, equal, signs)
	default:
		var head string
		switch {
		case n > 0 && gone > 0:
			head = fmt.Sprintf("%s from the latest snapshots %s new or changed since %s, and %d %s gone", plural(n, "difference"), pluralWord(n, "is", "are"), since, gone, pluralWord(gone, "is", "are"))
		case n > 0:
			head = fmt.Sprintf("%s from the latest snapshots %s new or changed since %s", plural(n, "difference"), pluralWord(n, "is", "are"), since)
		default:
			head = fmt.Sprintf("%s from the latest snapshots that %s listed %s gone", plural(gone, "difference"), since, pluralWord(gone, "is", "are"))
		}
		var do string
		switch {
		case n > 0 && gone > 0:
			do = fmt.Sprintf("delete what you added by hand in %s for each that is gone%s, then make what you added for each other%s", tool, under, equal)
		case n > 0:
			do = fmt.Sprintf("make what you added by hand in %s for each%s", tool, underEqual)
		default:
			do = fmt.Sprintf("delete what you added by hand in %s for each%s", tool, under)
		}
		when, why := "before", "; "+tool+" marks a new row equal to one that it holds as existing"
		if n > 0 {
			why += "; book one of a security that it does not hold yet after importing"
		}
		why += " (docs/tools.md step 4)"
		if gf {
			when, why = "after", signs+" (docs/tools.md step 6)"
		}
		fmt.Fprintf(w, "%s (%s lists each): %s importing, %s%s:\n", head, manifest, when, do, why)
	}
	for _, d := range e.Differences {
		what := "cash"
		if !d.Cash {
			what = strings.Join(strings.Fields(plainCell(cmp.Or(d.Asset, d.Symbol, d.Key))), " ") + " (" + plainCell(d.Finds) + ")"
		}
		value := d.Value
		switch {
		case d.Gone && d.Cash:
			value = "gone: delete the opening deposit or withdrawal that you booked for it, if any"
		case d.Gone:
			value = "gone: delete what you added for it, if anything"
		case d.LeftOut:
			value += ", less the rows left out of it that you book by hand"
		}
		if d.Unsure && !d.Gone {
			value += ", of a key under keys_unsure"
		}
		if d.NoActivity && !d.Gone {
			value += ", of an asset that no activity brings in: make it first (docs/tools.md step 6)"
		}
		fmt.Fprintf(w, "  %s: %s: %s\n", toolAccountName(e, d.Account), what, value)
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
