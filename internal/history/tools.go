package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// This file is a tool export as a whole (8 to 10 of the design): ExportTool
// reads the store once, maps what it read to the records that Portfolio
// Performance (PP) or Ghostfolio gets, compares them with what the export
// that it goes on from gave the tool, and renders the tool's files with
// their manifest, which WriteDir writes. The manifest says what the export
// wrote, left out and asks of you, and holds the chain's part, from which
// the next export goes on.

// ToolOptions says what ExportTool writes.
type ToolOptions struct {
	// Tool is the tool that it writes for, as -format names it: "pp" for
	// Portfolio Performance, or "ghostfolio".
	Tool string
	// Account selects one account, as ExportOptions.Account does; "" means
	// every account. An export that goes on from another (Since) selects
	// that one's account, by its ref, and takes none.
	Account string
	// To is the last day whose rows it writes; "" means the day before
	// today in New York, so that a day still in progress is not split
	// between two exports. Every row up to To is written, from each
	// account's first, since a tool that missed the rows that opened a
	// position would take its later sale for a short.
	To nytime.Date
	// Manual names symbols, as Schwab writes them or as Yahoo does, whose
	// securities Ghostfolio gets as MANUAL assets, priced from the
	// snapshots: those that Yahoo does not serve. It is for Ghostfolio
	// only; an export that goes on from another adds them to that one's.
	Manual []string
	// Since is the manifest.json of the export that this one goes on from,
	// the one imported last, or nil for a full export; SinceDir is its
	// folder, as messages name it. The export then writes only what the
	// earlier ones did not give the tool, and lists what changed or went.
	Since    []byte
	SinceDir string
	// Redo selects, for Ghostfolio going on from another export (Since), an
	// account as Account does: one that you deleted in Ghostfolio, with its
	// activities, to redo it. The export writes every row of it, as a full
	// export would, and lists its rows left out and its differences from
	// the snapshots anew, all that you did by hand for it having gone with
	// it, while it goes on from Since for the other accounts, whose
	// activities Ghostfolio keeps as it holds them. "" redoes none.
	Redo string
}

// A ToolExport is an export for a tool, held in memory until WriteDir
// writes it: what it writes, and what stderr says of it.
type ToolExport struct {
	// At is when the store was read, Tool the tool that the export is for,
	// as -format names it, and To its last day. Account is the account
	// that it selects, as "A2 ...398"; "" for every account.
	At      time.Time
	Tool    string
	To      nytime.Date
	Account string
	// Since is the exported_at of the export that it goes on from, and
	// SinceTo that export's last day; each "" for a full export. Redo is,
	// for Ghostfolio, the account that it writes whole for a redo (-redo),
	// as "A2 ...398"; "" for none.
	Since   string
	SinceTo nytime.Date
	Redo    string
	// Files are the files to import, in the order to import them: for PP,
	// securities.csv and then, for each account, its account file and its
	// portfolio file; for Ghostfolio, ghostfolio.json. WriteDir writes PP's
	// three configurations and manifest.json beside them.
	Files []ToolFile
	// Accounts are the accounts that the export writes or names, and those
	// that the earlier exports of its chain wrote or named, in the order of
	// their ids, those that the store does not hold last: a PP transfer
	// names the account of its other side, which PP must hold.
	Accounts []ToolAccount
	// Coverage is the days that each source covers of the accounts that it
	// selects.
	Coverage []ExportCoverage
	// Written counts the ledger rows that the export writes; Given those
	// that the export it goes on from gave the tool already, unchanged,
	// which it does not write again; Combined the shares rows of two-row
	// cash events that a sale written carries; and InBalances, for
	// Ghostfolio, the cash rows that the accounts' balances carry. Late
	// counts the rows written that the earlier export did not give and
	// that are dated on or before its last day, but those of the account
	// redone (Redo): they came late, were decided, paired or took a new ref
	// since.
	Written, Given, Combined, InBalances, Late int
	// Changed are the rows that the tool holds in another form than the
	// export writes, and Gone those that it holds and that the export
	// neither writes nor gives: delete each in the tool before importing.
	Changed, Gone []ToolChange
	// SameAs counts, for PP, the rows gone under one ref that the export
	// writes under another, alike in everything PP keeps: PP keeps the one
	// it holds, and marks the new one as existing, so neither is to delete
	// or to tick. Late counts none of them.
	SameAs int
	// Tick are, for PP, the rows that PP will mark as existing although
	// they are new, which you tick on its review page.
	Tick []ToolTick
	// LeftOut counts the rows left out, by reason, and Caveats the rows
	// written with a caveat, by caveat, each in the order of the docs
	// (toolReasons, toolCaveats), with no count of 0; CaveatRows counts the
	// rows written with any. LeftOut counts no row that the export it goes
	// on from left out for the same reason, which was asked of you then.
	LeftOut, Caveats []ToolCount
	CaveatRows       int
	// Flagged counts the rows written, given, combined or in the balances
	// that carry a flag that needs you, which the next export lists as
	// gone once you uncount them; Waiting the file rows that wait for you
	// to resolve them, which no export writes until you have; Holdings the
	// securities of an account whose shares differ from its latest
	// snapshot's, but those that the export it goes on from listed, alike
	// or otherwise (DifferencesChanged); SymbolsChanged the securities, as
	// the tool holds them, that a tool holding them under an earlier symbol
	// needs telling of; and, for PP, Cash the accounts whose cash differs
	// from their latest snapshot's, but those that the export it goes on
	// from listed likewise.
	Flagged, Waiting, Holdings, SymbolsChanged, Cash int
	// DifferencesChanged are, going on from another export, the
	// differences of the holdings check and, for PP, of the cash check that
	// that export listed and that this one lists with another, or not at
	// all: what you added or booked by hand in the tool for one is to
	// change by its change, or to delete.
	DifferencesChanged []ToolDifference
	// CashNotChecked are, for PP, the accounts whose cash the cash check
	// does not check, each with why (the manifest's
	// holdings.cash_not_checked).
	CashNotChecked []ToolCashUnchecked
	// NoSnapshot are the labels of the accounts with rows and no snapshot
	// to check against, which neither check checks; and NotInTool, for PP,
	// those with snapshots that PP holds nothing of, as no export of the
	// chain writes or names them: together, the manifest's
	// holdings.not_checked.
	NoSnapshot, NotInTool []string
	// Unsure are the keys of the store that know no CUSIP and had a ticker
	// that other securities have had too, of other CUSIPs or at another
	// time, which the export writes as one of them, or on their own, and
	// cannot be sure of.
	Unsure []ToolUnsure
	// Unused are the symbols that the chain's -manual names and that
	// nothing in the export uses, as given.
	Unused []string
	// RedoAccount and RedoManual are, going on from another export, what a
	// full export of a redo in the tool must repeat of the chain's
	// selection (docs/tools.md): the account that the chain selects, as
	// "A1", "" for every account, and, for Ghostfolio, every symbol that its
	// -manual has named, as given. A full export without them would write
	// the rows of accounts that the tool never had, or the activities of
	// the MANUAL assets that Ghostfolio holds for Yahoo, which it would take
	// for new beside them.
	RedoAccount string
	RedoManual  []string
	// NotInBalances are, for Ghostfolio, the accounts with cash rows that
	// no balance of the file carries yet, in the order of their ids.
	NotInBalances []ToolCashRows
	// Securities and Prices count, for PP, the securities and the prices
	// that securities.csv holds.
	Securities, Prices int
	// For Ghostfolio: Profiles counts the asset profiles that an activity
	// of the file is of, and PricesOnly those written for their prices
	// alone, which Ghostfolio applies only to an asset that it holds
	// already; Yahoo counts the symbols that its import asks Yahoo about,
	// and YahooIdle those of them with no row since IdleSince, a year
	// before To, of securities that no account holds: Yahoo may no longer
	// know them.
	Profiles, PricesOnly, Yahoo, YahooIdle int
	IdleSince                              nytime.Date

	// in is what the export read, with its tool, account, last day and
	// -manual list; events and outcomes are the mapping's (toolEvents),
	// as a full export writes every row; res is what compareChain made of
	// them.
	in       *toolInput
	events   []toolEvent
	outcomes []toolOutcome
	res      *chainResult
	// pp are a PP export's files, the configurations last, and marks what
	// PP will mark of them (ppTicks); gf is an export for Ghostfolio.
	pp    []ppFile
	marks []ppMark
	gf    *gfExport
	// manifest is manifest.json's contents, and store the path of the store
	// that the export read, "" for none: WriteDir writes over none of its
	// files.
	manifest []byte
	store    string
}

// A ToolFile is a file of a tool export that you import.
type ToolFile struct {
	// Name is its name in the export's folder, and Kind what it holds: for
	// PP, securities, or an account's records that move only cash
	// (account) or shares (portfolio); for Ghostfolio, ghostfolio.
	Name, Kind string
	// Configuration is, for PP, the configuration that reads it; Account
	// the label of the account whose records it holds, as "A1 ...135", ""
	// for a file of no one account.
	Configuration, Account string
	// Rows counts its rows below the header (PP) or its activities
	// (Ghostfolio), and Tick, for PP, those of them to tick (Tick).
	Rows, Tick int

	data []byte
}

// A ToolAccount is an account of a tool export (8.1 of the design): one
// that the export writes or names, or that an earlier export of its chain
// wrote or named.
type ToolAccount struct {
	// Label is how the export names it, as "A1 ...135"; Account its number
	// masked; Nickname its nickname at Schwab, masked free text, "" for
	// none; and Name the name of its accounts in the tool, which you give
	// them, as "Schwab A1 ...135". Only Label is set for an account of the
	// chain that the store does not hold.
	Label, Account, Nickname, Name string
	// Exported says that the export writes it; New that no earlier export
	// of its chain wrote or named it, so that you make it in the tool
	// before this import (for PP, by the name that stderr lists); and
	// LastDay is the last day of its records in the export, "" for none.
	Exported, New bool
	LastDay       nytime.Date
	// For Ghostfolio: GhostfolioID is its id there. Balances counts the
	// balances of it that the file holds, from FirstBalance to LastBalance,
	// or NoBalances says why it holds none; Cash is the last of them, in
	// dollars and cents, signed, "" for none: Ghostfolio takes an account's
	// balances only in the import that makes it, so this is what to set its
	// cash to in Ghostfolio after a later import.
	GhostfolioID              string
	Balances                  int
	FirstBalance, LastBalance nytime.Date
	NoBalances, Cash          string
}

// A ToolCashRows is, for Ghostfolio, an account's cash rows that no balance
// of the file carries yet (gf_cash), which a later snapshot will: how many,
// but those that the export it goes on from left out for that reason too;
// and the snapshot whose cash is its last balance, of the market date
// Snapshot, taken at Taken, after which they were posted, "" and the zero
// time for an account with no such snapshot.
type ToolCashRows struct {
	Account  string
	Rows     int
	Snapshot nytime.Date
	Taken    time.Time
}

// A ToolDifference is a difference of the holdings check, or of PP's cash
// check, that the export that a tool export goes on from listed and that
// this one lists with another, or not at all: its account's label; for
// shares, the security's key (the least, when the tool holds it under
// several) and symbol, both "" for cash (Cash), and, for Ghostfolio, the
// asset that holds it, as Ghostfolio shows it (a MANUAL asset's name, else
// its symbol), and, when Ghostfolio is to hold it under another asset now,
// the one that the export it goes on from listed it under (WasAsset); and
// the difference, the snapshot's shares or cash less what the records
// leave, as listed then (Was) and now (Now, "" when it is gone), with the
// change from one to the other (Now less Was, or less Was alone when gone),
// each as the manifest writes it, signed: shares, or dollars with two
// places.
type ToolDifference struct {
	Account, Key, Symbol string
	Cash                 bool
	Asset, WasAsset      string
	Was, Now, Change     string
}

// A ToolCashUnchecked is, for PP, an account whose cash the cash check does
// not check: its label, and why: "margin", its latest snapshot is of a
// margin account with a margin or a short balance, whose cash Schwab's
// figures do not settle; "no_cash", its latest snapshot holds no cash
// figure.
type ToolCashUnchecked struct {
	Account, Why string
}

// A ToolChange is a row that the tool holds and that a tool export changes
// or that went (3.8 of the design): delete it in the tool before
// importing.
type ToolChange struct {
	// Ref is its ref, and Account its account's label.
	Ref, Account string
	// Was is what the tool shows of it, as the earlier export wrote it:
	// for PP, each record's file, day, Type and Value, as "portfolio
	// 2025-01-21 Buy 400.67"; for Ghostfolio, each activity's day, type and
	// value, with its fee. NoteRef is the ref that its PP Note names, when
	// it is not Ref; Transfer, for a side of a PP transfer, the head of the
	// Note that PP holds of the transfer, which names its paying side first,
	// as "A2 ...398 T61001201 to A1 ...135 T61001202", "" for any other row.
	Was, NoteRef, Transfer string
	// Now is, for a row changed, what the export writes of it, in Was's
	// form; Why is, for a row gone, what became of it: uncounted with the
	// store's reason, linked to the row that counts in its place, left out
	// with its reason, combined with another row, or now cash, which
	// Ghostfolio's balances carry; else "not in this export: no longer in
	// the store, or dated after -to".
	Now, Why string
}

// A ToolTick is a row of a PP export that PP will mark as existing although
// it is new, which you tick on PP's review page: the second such row of two
// equal ones, or a row equal to a transfer's leg in its account.
type ToolTick struct {
	// File is the file whose row it is, Ref the row's ref, Account its
	// account's label, and As what PP shows of the record, as Was is.
	File, Ref, Account, As string
}

// A ToolUnsure is a key of the store that knows no CUSIP, as a History
// file's key often does, and had a ticker that other securities have had
// too: of other CUSIPs, as when the store holds a reorganized security's new
// CUSIP or another company under that ticker, or at another time, as when
// it had the ticker after another company gave it up (3.5 of the design). A
// tool export cannot be sure which of them it is, and writes its rows as
// the one that had its ticker first, or on their own when its tickers are
// each of another or each had it at another time. You say which it is in
// the store, with one of the commands.
type ToolUnsure struct {
	// Key is its key, and Keys every key of the store that the export takes
	// for one security with it, when there are several; Tickers those of its
	// tickers, in Yahoo's form, that the others have had.
	Key     string
	Keys    []string
	Tickers []string
	// TakenFor is the key of the security that the export writes it as, ""
	// when it stays on its own.
	TakenFor string
	// Securities are those that it may be, in the order of their keys.
	Securities []ToolUnsureSecurity
}

// A ToolUnsureSecurity is a security that a ToolUnsure may be: the first of
// its keys, its CUSIP ("" for none), and the command, naming no -store,
// that tells the store that the ToolUnsure's key is of it; Key and Command
// are "" for a security that the store holds under no key. Apart says that
// the two had the ticker at times that do not meet, by the days of the API's
// rows and the exports that named the History files' keys: one gave it up,
// renamed, delisted or merged away, before the other had it, so that the
// export does not take them for one.
type ToolUnsureSecurity struct {
	Key, CUSIP, Command string
	Apart               bool
}

// toolUnsures returns the keys that the groups unsure stand for, as an
// export says them.
func toolUnsures(unsure []*toolUnsure) []ToolUnsure {
	var out []ToolUnsure
	for _, u := range unsure {
		t := ToolUnsure{Key: u.keys[0], Tickers: slices.Clone(u.tickers)}
		if len(u.keys) > 1 {
			t.Keys = slices.Clone(u.keys)
		}
		if u.taken >= 0 {
			t.TakenFor = u.of[u.taken].key
		}
		for _, o := range u.of {
			s := ToolUnsureSecurity{Key: o.key, CUSIP: o.cusip, Apart: o.apart}
			if o.key != "" {
				s.Command = "schwabtx edit " + shellQuote("K:"+t.Key) + " -same " + shellQuote("K:"+o.key)
			}
			t.Securities = append(t.Securities, s)
		}
		out = append(out, t)
	}
	return out
}

// A ToolCount is how many rows of a tool export have a code: a reason for
// which they were left out, or a caveat.
type ToolCount struct {
	Code  string
	Count int
}

// ExportTool makes an export for a tool (8 to 10 of the design). It reads
// what the export maps (2), in one read transaction, which has ended when
// it returns; maps each ledger row to its records, or says why it left the
// row out (3 and 4); compares them with what the export that it goes on
// from gave the tool (3.8); and renders the tool's files and their
// manifest (5, 6 and 8.1). Nothing is written until WriteDir. It refuses
// options that do not go together; a Since that is not the manifest of an
// export of its tool, made from this store, whose last day is no later than
// To; a Manual symbol that no security in the store has; and a Redo that
// names no one account of the store, or another than the chain selects.
func (s *Store) ExportTool(ctx context.Context, o ToolOptions) (*ToolExport, error) {
	o, since, err := o.check(s.o.Now())
	if err != nil {
		return nil, err
	}
	var in *toolInput
	var at time.Time
	var v versions
	err = s.withRead(ctx, func(r *rtx) error {
		// A read may run again: everything it builds is built afresh.
		at = s.o.Now().UTC()
		var err error
		if v, err = readVersions(r.ctx, r.tx); err != nil {
			return err
		}
		var account, redo int64
		if o.Redo != "" {
			if redo, err = selectAccountOf(reader(r), o.Redo, false, "-redo"); err != nil {
				return err
			}
		}
		switch {
		case since != nil:
			// The chain's account, by its ref, whatever its nickname is now
			// and whichever accounts share its last digits.
			if account, err = since.account(); err != nil {
				return fmt.Errorf("-since %s: %w", o.SinceDir, err)
			}
		case o.Account != "":
			if account, err = selectAccount(reader(r), o.Account, false); err != nil {
				return err
			}
		}
		if in, err = readToolInput(r, account, o.To); err == nil {
			in.redo = redo
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return exportTool(in, o, since, at, v, s.path)
}

// check checks the options before the store is read, and returns them with
// To set, now being the store's clock, and the export that they go on
// from, parsed, nil for none.
func (o ToolOptions) check(now time.Time) (ToolOptions, *toolChain, error) {
	// A Redo of spaces alone names no account, as -redo refuses it.
	o.Redo = strings.TrimSpace(o.Redo)
	switch {
	case o.Tool != toolPP && o.Tool != toolGF:
		return o, nil, fmt.Errorf("a tool export is for pp or ghostfolio, not %q", mask.Text(o.Tool))
	case o.To != "" && !isDate(o.To):
		return o, nil, fmt.Errorf("%q is not a date (YYYY-MM-DD)", mask.Text(string(o.To)))
	case o.Tool != toolGF && len(chainManualUnion(o.Manual)) > 0:
		return o, nil, errors.New("-manual is for -format ghostfolio")
	case o.Since != nil && o.Account != "":
		return o, nil, fmt.Errorf("-since %s goes on from the export in %s, with its account: drop -account", o.SinceDir, o.SinceDir)
	case o.Redo != "" && o.Tool != toolGF:
		return o, nil, errors.New("-redo is for -format ghostfolio: a redo in Portfolio Performance deletes every account and imports a full export (docs/tools.md)")
	case o.Redo != "" && o.Since == nil:
		return o, nil, errors.New("-redo goes on from the export that you imported last: give its folder with -since (a full export redoes every account, docs/tools.md)")
	}
	if o.To == "" {
		o.To = addDays(nytime.DateOf(now), -1)
	}
	if o.Since == nil {
		return o, nil, nil
	}
	since, err := parseChain(o.Since)
	if err != nil {
		return o, nil, fmt.Errorf("-since %s: %s is not the manifest of a -format %s export of layout %d",
			o.SinceDir, filepath.Join(o.SinceDir, "manifest.json"), o.Tool, ToolLayout)
	}
	return o, since, nil
}

// exportTool makes the export for o.Tool of in, which the store's reads
// gave (readToolInput) for the account and up to the day that in holds,
// going on from the export since, nil for a full export: read at at, from a
// store of versions v at the path store, "" for none. Of o it takes the
// tool, the -manual symbols and the folder of since. Like the mapping, it is
// a function of its arguments, so that tests make exports of inputs that no
// store holds.
func exportTool(in *toolInput, o ToolOptions, since *toolChain, at time.Time, v versions, store string) (*ToolExport, error) {
	in.tool = o.Tool
	if since != nil {
		if err := since.refuse(in, o.SinceDir); err != nil {
			return nil, err
		}
		if in.account != 0 && !slices.ContainsFunc(in.accounts, func(a toolAccount) bool { return a.id == in.account }) {
			return nil, fmt.Errorf("-since %s: its chain selects A%d, which the store does not hold", o.SinceDir, in.account)
		}
		if in.redo != 0 && in.account != 0 && in.redo != in.account {
			return nil, fmt.Errorf("-redo %s: the chain of -since %s selects %s alone", in.accountOf(in.redo).label(), o.SinceDir, in.accountOf(in.account).label())
		}
	}
	in.manual = since.manual(o.Manual)
	events, outcomes := toolEvents(in)
	res := compareChain(in, events, outcomes, since)
	unused, err := checkManual(in, res.written)
	if err != nil {
		return nil, err
	}
	e := &ToolExport{
		At: at, Tool: in.tool, To: in.to, Coverage: in.coverage, Waiting: len(in.waiting), Late: res.late,
		SameAs: len(res.sameAs), SymbolsChanged: len(res.symbolsChanged), Unsure: toolUnsures(keysUnsure(in, res.written)), Unused: unused,
		in: in, events: events, outcomes: outcomes, res: res, store: store,
	}
	if in.account != 0 {
		e.Account = in.accountOf(in.account).label()
	}
	if in.redo != 0 {
		e.Redo = in.accountOf(in.redo).label()
	}
	if since != nil {
		e.Since, e.SinceTo = since.ExportedAt, since.Chain.To
		if a := since.Chain.Account; a != nil {
			e.RedoAccount = *a
		}
		if in.tool == toolGF {
			e.RedoManual = chainManualUnion(in.manual)
		}
	}
	switch in.tool {
	case toolPP:
		e.renderPP()
	case toolGF:
		if err := e.renderGF(); err != nil {
			return nil, err
		}
	}
	e.count()
	part := chainJSON(res, exportInstant(at), unused)
	e.accounts(part.Accounts)
	if e.manifest, err = e.manifestOf(o, part, v); err != nil {
		return nil, err
	}
	return e, nil
}

// renderPP renders a PP export's files (ppFiles), and what PP will mark of
// them (ppTicks).
func (e *ToolExport) renderPP() {
	e.pp = ppFiles(e.in, e.res.written)
	e.marks = ppTicks(e.pp, e.res)
	ticks := map[string]int{}
	for _, m := range e.marks {
		if m.tick {
			ticks[m.file]++
		}
	}
	for _, f := range e.pp {
		if f.kind == ppConfiguration {
			continue
		}
		tf := ToolFile{Name: f.name, Kind: f.kind, Configuration: f.configuration, Rows: f.rows, Tick: ticks[f.name], data: f.data}
		if f.kind != ppSecurities {
			tf.Account = f.account.label()
		}
		e.Files = append(e.Files, tf)
	}
	for _, l := range mergeListed(toolPP, listSecurities(e.in, e.res.written)) {
		e.Securities++
		e.Prices += len(l.prices)
	}
}

// renderGF renders an export's file for Ghostfolio (gfRender).
func (e *ToolExport) renderGF() error {
	e.gf = gfRender(e.in, e.res.written)
	var b bytes.Buffer
	if err := e.gf.file.write(&b); err != nil {
		return err
	}
	e.Files = []ToolFile{{Name: gfFileName, Kind: toolGF, Rows: len(e.gf.file.Activities), data: b.Bytes()}}
	for _, a := range e.gf.assets {
		if a.used {
			e.Profiles++
		} else {
			e.PricesOnly++
		}
	}
	e.Yahoo, e.YahooIdle, e.IdleSince = len(e.gf.yahoo), len(e.gf.idle), yearBefore(e.in.to)
	return nil
}

// yearBefore returns the day a year before d, as yahooIdle counts the
// year: "" for no day.
func yearBefore(d nytime.Date) nytime.Date {
	t, err := time.Parse(time.DateOnly, string(d))
	if err != nil {
		return ""
	}
	return nytime.Date(t.AddDate(-1, 0, 0).Format(time.DateOnly))
}

// count counts the export's rows by their outcome, and what stderr counts
// of them: the rows left out, by reason, but those that the export it goes
// on from left out alike; the rows written with each caveat; the flagged
// ones; and the holdings that differ, but those listed before.
func (e *ToolExport) count() {
	caveats, reasons := map[string]int{}, map[string]int{}
	for i, o := range e.res.outcomes {
		switch o.outcome {
		case outcomeWritten:
			e.Written++
			if len(o.caveats) > 0 {
				e.CaveatRows++
			}
			for _, c := range o.caveats {
				caveats[c]++
			}
		case outcomeGiven:
			e.Given++
		case outcomeCombined:
			if e.carried(i) {
				e.Combined++
			}
		case outcomeInBalances:
			e.InBalances++
		}
		// The mapping flags no row that it leaves out (toolOutcome.flags).
		if len(o.flags) > 0 {
			e.Flagged++
		}
	}
	for _, l := range e.res.leftOut {
		if !l.Before {
			reasons[l.Reason]++
		}
	}
	for _, r := range toolReasons {
		if n := reasons[r.code]; n > 0 {
			e.LeftOut = append(e.LeftOut, ToolCount{Code: r.code, Count: n})
		}
	}
	for _, c := range toolCaveats {
		if n := caveats[c.code]; n > 0 {
			e.Caveats = append(e.Caveats, ToolCount{Code: c.code, Count: n})
		}
	}
	for _, h := range e.res.holdings {
		if !h.Before && h.Was == "" {
			e.Holdings++
		}
	}
	for _, c := range e.res.cash {
		if !c.Before && c.Was == "" {
			e.Cash++
		}
	}
	for _, u := range e.res.cashUnchecked {
		e.CashNotChecked = append(e.CashNotChecked, ToolCashUnchecked{Account: u.Account, Why: u.Why})
	}
	for _, a := range e.res.unchecked {
		if slices.ContainsFunc(e.res.notHeld, func(n toolAccount) bool { return n.id == a.id }) {
			e.NotInTool = append(e.NotInTool, a.label())
		} else {
			e.NoSnapshot = append(e.NoSnapshot, a.label())
		}
	}
	e.DifferencesChanged = e.res.differencesChanged()
	for _, c := range e.res.changed {
		e.Changed = append(e.Changed, ToolChange{Ref: c.Ref, Account: c.Account, Was: c.Was, NoteRef: c.NoteRef, Transfer: c.Transfer, Now: c.Now})
	}
	for _, g := range e.res.gone {
		e.Gone = append(e.Gone, ToolChange{Ref: g.Ref, Account: g.Account, Was: g.Was, NoteRef: g.NoteRef, Transfer: g.Transfer, Why: g.Why})
	}
	for _, t := range ppTickList(e.marks) {
		e.Tick = append(e.Tick, ToolTick(t))
	}
}

// carried reports whether row i, combined with its cash row's sale, is
// carried by a sale that the export writes, rather than by one that the
// tool holds already.
func (e *ToolExport) carried(i int) bool {
	r := &e.in.rows[i]
	for j, o := range e.res.outcomes {
		if c := &e.in.rows[j]; c.account == r.account && c.ref == e.res.outcomes[i].with {
			return o.outcome == outcomeWritten
		}
	}
	return false
}

// accounts sets the export's accounts from the chain's part of its
// manifest, and, for Ghostfolio, the accounts with cash rows that no balance
// carries yet, with the snapshot that each one's balances end with.
func (e *ToolExport) accounts(accounts []chainAccount) {
	rows := map[string]int{}
	for _, l := range e.res.leftOut {
		if l.Reason == "gf_cash" && !l.Before {
			rows[l.Account]++
		}
	}
	balances := toolBalances(e.in)
	for _, a := range e.in.accounts {
		if n := rows[a.label()]; n > 0 {
			c := ToolCashRows{Account: a.label(), Rows: n}
			if bs := balances[a.id]; len(bs) > 0 {
				c.Snapshot, c.Taken = bs[len(bs)-1].date, bs[len(bs)-1].fetchedAt
			}
			e.NotInBalances = append(e.NotInBalances, c)
		}
	}
	for _, a := range accounts {
		ta := ToolAccount{
			Label: a.Label, Account: a.Account, Nickname: a.Nickname, Name: a.Name, Exported: a.Exported, New: a.New,
			GhostfolioID: a.GhostfolioID,
		}
		if a.LastDay != nil {
			ta.LastDay = *a.LastDay
		}
		if b := a.Balances; b != nil {
			ta.Balances, ta.NoBalances = b.Count, b.Why
			if b.First != nil && b.Last != nil {
				ta.FirstBalance, ta.LastBalance = *b.First, *b.Last
			}
		}
		if a.Cash != nil {
			ta.Cash = a.Cash.Value.String()
		}
		e.Accounts = append(e.Accounts, ta)
	}
}

// WriteDir writes the export to the folder dir (8.1 and 9 of the design):
// its Files, then, for PP, the three configurations, and last
// manifest.json, which says what the export wrote, left out and asks of
// you, and holds what the next export goes on from. dir is made, readable
// only by you, when it is missing, and each file is written as
// Export.WriteFile writes one, readable only by you; a file of a name that
// one of the store's files has, or that would be taken for one, is refused
// before anything is written. dir should hold no export already, whose
// files this one would replace or leave beside its own.
func (e *ToolExport) WriteDir(dir string) error {
	type file struct {
		name string
		data []byte
	}
	var files []file
	for _, f := range e.Files {
		files = append(files, file{f.Name, f.data})
	}
	for _, f := range e.pp {
		if f.kind == ppConfiguration {
			files = append(files, file{f.name, f.data})
		}
	}
	files = append(files, file{"manifest.json", e.manifest})
	for _, f := range files {
		if err := notStoreFile(filepath.Join(dir, f.name), e.store); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	for _, f := range files {
		if err := writePrivate(filepath.Join(dir, f.name), func(w io.Writer) error {
			_, err := w.Write(f.data)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// The manifest's own parts (8.1 of the design), beside the chain's
// (chainManifest), each in the order of its keys there.
type (
	// toolFilters are what the export selects: its account, by its label,
	// null for every account; its last day; the symbols that its -manual
	// names, masked, the chain's aside; the exported_at of the export that
	// it goes on from, null for none; and the account that it writes whole
	// for a redo (-redo), by its label, null for none.
	toolFilters struct {
		Account *string     `json:"account"`
		To      nytime.Date `json:"to"`
		Manual  []string    `json:"manual"`
		Since   *string     `json:"since"`
		Redo    *string     `json:"redo"`
	}
	// A toolFileEntry is a file of the manifest's files (ToolFile); tick is
	// for PP only.
	toolFileEntry struct {
		File          string `json:"file"`
		Kind          string `json:"kind"`
		Configuration string `json:"configuration,omitempty"`
		Account       string `json:"account,omitempty"`
		Rows          int    `json:"rows"`
		Tick          *int   `json:"tick,omitempty"`
	}
	// toolWritten counts the ledger rows that the export writes, and its
	// records by PP's Type or Ghostfolio's type.
	toolWritten struct {
		Rows  int            `json:"rows"`
		Types map[string]int `json:"types"`
	}
	// A toolCaveatEntry is a caveat of a row that the export writes.
	toolCaveatEntry struct {
		Ref     string      `json:"ref"`
		Account string      `json:"account"`
		Day     nytime.Date `json:"day"`
		Caveat  string      `json:"caveat"`
	}
	// A toolCombinedEntry is the shares row of a two-row cash event that a
	// sale that the export writes carries, and that sale's row (with).
	toolCombinedEntry struct {
		Ref     string `json:"ref"`
		Account string `json:"account"`
		With    string `json:"with"`
	}
	// A toolInBalancesEntry counts, for Ghostfolio, an account's cash rows
	// that its balances carry.
	toolInBalancesEntry struct {
		Account string `json:"account"`
		Rows    int    `json:"rows"`
	}
	// A toolFlaggedEntry is a row written, given, combined or in the
	// balances that carries flags that need you, and its outcome.
	toolFlaggedEntry struct {
		Ref     string      `json:"ref"`
		Account string      `json:"account"`
		Day     nytime.Date `json:"day"`
		Outcome string      `json:"outcome"`
		Flags   []string    `json:"flags"`
	}
	// A toolWaitingEntry is a file row that waits for you (Q8): its amount
	// in dollars, signed, null off the cent grid; why it waits; and the
	// command that shows it, with what settles it.
	toolWaitingEntry struct {
		Ref     string       `json:"ref"`
		Account string       `json:"account"`
		Day     nytime.Date  `json:"day"`
		Kind    string       `json:"kind"`
		Amount  *json.Number `json:"amount"`
		Why     string       `json:"why"`
		Command string       `json:"command"`
	}
	// toolGhostfolioEntry is what the manifest says of a Ghostfolio
	// export's file (6.4 and 6.6 of the design).
	toolGhostfolioEntry struct {
		Yahoo     []string           `json:"yahoo"`
		YahooIdle []toolIdleEntry    `json:"yahoo_idle"`
		Manual    []toolManualEntry  `json:"manual"`
		Profiles  []toolProfileEntry `json:"profiles"`
	}
	// A toolIdleEntry is a symbol that Ghostfolio's import asks Yahoo about
	// and that Yahoo may no longer know (toolIdle), with the day of its
	// security's last row.
	toolIdleEntry struct {
		Symbol      string      `json:"symbol"`
		SecurityKey string      `json:"security_key"`
		LastDay     nytime.Date `json:"last_day"`
	}
	// A toolManualEntry is a security that Ghostfolio gets as a MANUAL
	// asset, and why (toolSecurity.manual).
	toolManualEntry struct {
		SecurityKey string `json:"security_key"`
		Symbol      string `json:"symbol"`
		Why         string `json:"why"`
	}
	// A toolProfileEntry is an asset profile of the file: used by an
	// activity of the file, or written for its prices alone (prices_only).
	toolProfileEntry struct {
		Symbol string `json:"symbol"`
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	// A toolSymbolsEntry is a security whose symbol changed (3.5 of the
	// design), as the tool holds it: its key, the first of the store's
	// securities that the tool takes for it, and every one's key when there
	// are several, as the holdings check names them; how the export writes
	// it (its ticker, or MANUAL for one that the tool gets no price feed
	// for); and every ticker that it has had, the latest first, its CUSIP
	// first when it is delisted.
	toolSymbolsEntry struct {
		SecurityKey  string   `json:"security_key"`
		SecurityKeys []string `json:"security_keys,omitempty"`
		WrittenAs    string   `json:"written_as"`
		Symbols      []string `json:"symbols"`
	}
	// A toolUnsureEntry is a key that the export cannot be sure of
	// (ToolUnsure): taken_for is null when it stays on its own.
	toolUnsureEntry struct {
		SecurityKey  string               `json:"security_key"`
		SecurityKeys []string             `json:"security_keys,omitempty"`
		Tickers      []string             `json:"tickers"`
		TakenFor     *string              `json:"taken_for"`
		Securities   []toolUnsureSecurity `json:"securities"`
	}
	// A toolUnsureSecurity is a security that a toolUnsureEntry may be
	// (ToolUnsureSecurity): apart is set when the two had the ticker at
	// times that do not meet.
	toolUnsureSecurity struct {
		SecurityKey string `json:"security_key"`
		CUSIP       string `json:"cusip"`
		Apart       bool   `json:"apart,omitempty"`
		Command     string `json:"command"`
	}
)

// manifestOf returns the export's manifest.json (8.1 of the design), with
// its keys in the design's order: the existing manifest's first keys and
// the store; the tool's format and the layout; what it selects, of o; the
// chain's part (part) at its keys; its files, in the order to import them;
// the units; what it wrote, changed, ticked, left out or flagged, and what
// waits for you; the holdings check; for Ghostfolio, what its import asks
// Yahoo about and what is MANUAL; the securities whose symbol changed; the
// keys that it cannot be sure of; and the coverage. A key of the other tool's only is left out. Descriptions
// and kinds are the store's masked free text, and the commands name no
// -store.
func (e *ToolExport) manifestOf(o ToolOptions, part chainManifest, v versions) ([]byte, error) {
	in, res := e.in, e.res
	m := struct {
		ExportedAt     string                           `json:"exported_at"`
		Schema         int                              `json:"schema"`
		Rules          int                              `json:"rules"`
		Calendar       int                              `json:"calendar"`
		Store          string                           `json:"store"`
		Format         string                           `json:"format"`
		Layout         int                              `json:"layout"`
		Filters        toolFilters                      `json:"filters"`
		Chain          chainHead                        `json:"chain"`
		Files          []toolFileEntry                  `json:"files"`
		Accounts       []chainAccount                   `json:"accounts"`
		Units          map[string]string                `json:"units"`
		Written        toolWritten                      `json:"written"`
		Given          map[string]map[string]chainGiven `json:"given"`
		Securities     []chainSecurity                  `json:"securities"`
		Changed        []chainChanged                   `json:"changed"`
		Gone           []chainGone                      `json:"gone"`
		SameAs         *[]chainSameAs                   `json:"same_as,omitempty"`
		Tick           *[]chainTick                     `json:"tick,omitempty"`
		LeftOut        []chainLeftOut                   `json:"left_out"`
		Caveats        []toolCaveatEntry                `json:"caveats"`
		Combined       []toolCombinedEntry              `json:"combined"`
		InBalances     *[]toolInBalancesEntry           `json:"in_balances,omitempty"`
		Flagged        []toolFlaggedEntry               `json:"flagged"`
		Waiting        []toolWaitingEntry               `json:"waiting"`
		Holdings       chainHoldings                    `json:"holdings"`
		Ghostfolio     *toolGhostfolioEntry             `json:"ghostfolio,omitempty"`
		SymbolsChanged []toolSymbolsEntry               `json:"symbols_changed"`
		KeysUnsure     []toolUnsureEntry                `json:"keys_unsure"`
		Coverage       []ExportCoverage                 `json:"coverage"`
	}{
		ExportedAt: exportInstant(e.At), Schema: v.schema, Rules: v.derive, Calendar: v.calendar,
		Store: in.store, Format: in.tool, Layout: ToolLayout,
		Filters: toolFilters{To: in.to, Manual: []string{}},
		Chain:   part.Chain, Files: []toolFileEntry{}, Accounts: part.Accounts, Units: toolUnits[in.tool],
		Written: toolWritten{Types: map[string]int{}}, Given: part.Given, Securities: part.Securities,
		Changed: orEmpty(res.changed), Gone: orEmpty(res.gone), LeftOut: part.LeftOut,
		Caveats: []toolCaveatEntry{}, Combined: []toolCombinedEntry{}, Flagged: []toolFlaggedEntry{},
		Waiting: []toolWaitingEntry{}, Holdings: part.Holdings, SymbolsChanged: []toolSymbolsEntry{},
		KeysUnsure: []toolUnsureEntry{}, Coverage: orEmpty(in.coverage),
	}
	if e.Account != "" {
		m.Filters.Account = &e.Account
	}
	for _, s := range chainManualUnion(o.Manual) {
		m.Filters.Manual = append(m.Filters.Manual, mask.Text(s))
	}
	if e.Since != "" {
		m.Filters.Since = &e.Since
	}
	if e.Redo != "" {
		m.Filters.Redo = &e.Redo
	}
	for _, f := range e.Files {
		entry := toolFileEntry{File: f.Name, Kind: f.Kind, Configuration: f.Configuration, Account: f.Account, Rows: f.Rows}
		if in.tool == toolPP {
			entry.Tick = &f.Tick
		}
		m.Files = append(m.Files, entry)
	}
	m.Written.Rows = e.Written
	for i := range res.written {
		r := &res.written[i]
		typ := r.kind.gfType()
		if in.tool == toolPP {
			_, typ = r.kind.ppType()
		}
		m.Written.Types[typ]++
	}
	if in.tool == toolPP {
		sameAs, tick := orEmpty(res.sameAs), ppTickList(e.marks)
		m.SameAs, m.Tick = &sameAs, &tick
	}
	inBalances := map[int64]int{}
	for i, o := range res.outcomes {
		r := &in.rows[i]
		label := o.account.label()
		switch o.outcome {
		case outcomeWritten:
			for _, c := range o.caveats {
				m.Caveats = append(m.Caveats, toolCaveatEntry{Ref: r.ref, Account: label, Day: r.day, Caveat: c})
			}
		case outcomeCombined:
			if e.carried(i) {
				m.Combined = append(m.Combined, toolCombinedEntry{Ref: r.ref, Account: label, With: o.with})
			}
		case outcomeInBalances:
			inBalances[r.account]++
		}
		if len(o.flags) > 0 {
			m.Flagged = append(m.Flagged, toolFlaggedEntry{Ref: r.ref, Account: label, Day: r.day, Outcome: o.outcome, Flags: o.flags})
		}
	}
	if in.tool == toolGF {
		list := []toolInBalancesEntry{}
		for _, a := range in.accounts {
			if n := inBalances[a.id]; n > 0 {
				list = append(list, toolInBalancesEntry{Account: a.label(), Rows: n})
			}
		}
		m.InBalances = &list
		m.Ghostfolio = e.ghostfolioEntry()
	}
	for _, w := range in.waiting {
		entry := toolWaitingEntry{
			Ref: w.ref, Account: in.accountOf(w.account).label(), Day: w.day, Kind: w.kind, Why: w.why,
			Command: "schwabtx status " + w.ref,
		}
		if w.amount.Valid {
			a := json.Number(formatCents(w.amount.Int64))
			entry.Amount = &a
		}
		m.Waiting = append(m.Waiting, entry)
	}
	for _, of := range res.symbolsChanged {
		s := of[0]
		entry := toolSymbolsEntry{SecurityKey: s.key, WrittenAs: "MANUAL", Symbols: slices.Clone(s.tickers)}
		if len(of) > 1 {
			for _, k := range of {
				entry.SecurityKeys = append(entry.SecurityKeys, k.key)
			}
		}
		switch {
		case in.tool == toolPP && s.ppTicker() != "":
			entry.WrittenAs = s.ppTicker()
		case in.tool == toolGF && s.manual == "":
			entry.WrittenAs = s.gfSymbol()
		}
		if s.delisted {
			// Its CUSIP, which a delisted security, or another of the store's
			// securities that the tools take for one with it, has for its
			// symbol now.
			entry.Symbols = slices.Insert(entry.Symbols, 0, s.cusip)
		}
		m.SymbolsChanged = append(m.SymbolsChanged, entry)
	}
	for _, u := range e.Unsure {
		entry := toolUnsureEntry{SecurityKey: u.Key, SecurityKeys: u.Keys, Tickers: u.Tickers, Securities: []toolUnsureSecurity{}}
		if u.TakenFor != "" {
			entry.TakenFor = &u.TakenFor
		}
		for _, s := range u.Securities {
			entry.Securities = append(entry.Securities, toolUnsureSecurity{SecurityKey: s.Key, CUSIP: s.CUSIP, Apart: s.Apart, Command: s.Command})
		}
		m.KeysUnsure = append(m.KeysUnsure, entry)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	// Descriptions and nicknames hold &, as in "Options & Margin".
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// ghostfolioEntry returns what the manifest says of a Ghostfolio export's
// file: the symbols that its import asks Yahoo about, those that Yahoo may
// no longer know, the securities that it gets as MANUAL assets and why, and
// each asset profile, used by an activity or written for its prices alone.
func (e *ToolExport) ghostfolioEntry() *toolGhostfolioEntry {
	x := e.gf
	g := &toolGhostfolioEntry{
		Yahoo: orEmpty(x.yahoo), YahooIdle: []toolIdleEntry{}, Manual: []toolManualEntry{}, Profiles: []toolProfileEntry{},
	}
	for _, i := range x.idle {
		g.YahooIdle = append(g.YahooIdle, toolIdleEntry{Symbol: i.symbol, SecurityKey: i.key, LastDay: i.last})
	}
	for i, a := range x.assets {
		p := x.file.AssetProfiles[i]
		status := "used"
		if !a.used {
			status = "prices_only"
		}
		g.Profiles = append(g.Profiles, toolProfileEntry{Symbol: p.Symbol, Name: p.Name, Status: status})
		for _, s := range a.securities {
			g.Manual = append(g.Manual, toolManualEntry{SecurityKey: s.key, Symbol: s.symbol, Why: s.manual})
		}
	}
	return g
}

// orEmpty returns s, or an empty slice for nil, which JSON writes as [].
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// toolUnits say, in the manifest, how each tool's files and the manifest
// write values (7 of the design), by the tool.
var toolUnits = map[string]map[string]string{
	toolPP: {
		"money":    "Value and Fees: dollars with two places, unsigned, as the Type says which way the cash goes, from the store's whole cents",
		"shares":   "Shares: shares, an option's contracts times its multiplier, or a bond's face value in dollars, from the store's whole millionths",
		"prices":   "Quote: dollars per share, or per dollar of a bond's face, a snapshot's market value over its shares, rounded to 8 places",
		"dates":    "New York calendar dates, YYYY-MM-DD",
		"times":    "Time: HH:mm on New York's clocks, empty for a row with no time on its day",
		"manifest": "amounts, quantities and shares signed, as the store's: into the account is positive",
		"fp":       "the first 16 hex digits of the SHA-256 of the fields that PP keeps of a row's records, joined by U+001F; keys likewise of each side that PP's duplicate check compares",
	},
	toolGF: {
		"money":    "fee, a DIVIDEND's and an INTEREST's unitPrice, and balances: dollars with two places, from the store's whole cents; balances signed",
		"shares":   "quantity: shares, an option's contracts times its multiplier, or a bond's face value in dollars, from the store's whole millionths",
		"prices":   "a BUY's or a SELL's unitPrice: its cash before fees over its shares, to 10 places; a profile's marketPrice: a snapshot's market value over its shares, to 8 places",
		"dates":    "an activity's day at noon UTC, and milliseconds that keep the day's order; balances and prices at midnight UTC of their market date",
		"manifest": "amounts, quantities and shares signed, as the store's: into the account is positive; days are New York calendar dates, YYYY-MM-DD",
		"fp":       "the first 16 hex digits of the SHA-256 of every key of a row's activities, joined by U+001F",
	},
}

// notStoreFile refuses an output at path that is one of the files of the
// store at store, or would be taken for one (IsCompanion); "" is no store,
// whose files nothing is.
func notStoreFile(path, store string) error {
	if store != "" && IsCompanion(path, store) {
		return fmt.Errorf("%s would replace the store itself (or one of its files); choose another name", path)
	}
	return nil
}
