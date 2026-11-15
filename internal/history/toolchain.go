package history

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/big"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// This file is the chain of tool exports (3.8 of the design). Neither tool
// updates or deletes what it imported, and the store changes days already
// exported: rows come late, are decided, pair, are revised or take a new ref.
// So each export's manifest records what it gave the tool, row by row, and an
// export made with -since compares the rows it maps with what the export it
// goes on from gave: it writes only what the tool lacks, whatever its day,
// lists what changed or went, for you to delete in the tool first, and, for
// PP, which new records PP will mark as existing, from a replay of PP's
// duplicate rule over its files in the order to import them. Like the
// mapping, everything here is a function of its arguments: the store's reads
// come in a toolInput, and an earlier export as its manifest's bytes.

// ToolLayout is the layout of a tool export's files and manifest: what its
// manifest.json says as its layout, and the v1 that ends the PP
// configurations' labels (5.6 of the design). An export goes on only from
// an export of its own layout.
const ToolLayout = 1

// outcomeGiven is a row, with -since, whose records the tool holds already
// from an earlier export of the chain, unchanged (3.1 of the design): the
// export does not write it again.
const outcomeGiven = "given"

// A chainManifest is the chain's part of a tool export's manifest (3.8 and
// 8.1 of the design), which chainJSON writes and parseChain reads back: the
// chain, the accounts it wrote, what the tool holds of each row once this
// export is imported, the securities as it wrote them, and the rows left out
// and the holdings that differ, which the next export compares with.
type chainManifest struct {
	Chain    chainHead      `json:"chain"`
	Accounts []chainAccount `json:"accounts"`
	// Given holds, by account label and then ref, each row whose records the
	// tool holds once this export and the earlier ones of its chain are
	// imported: written by this export or given by an earlier one. A row
	// combined into its cash row's sale has no entry of its own, and a
	// Ghostfolio cash row, which has no activity, none at all.
	Given      map[string]map[string]chainGiven `json:"given"`
	Securities []chainSecurity                  `json:"securities"`
	LeftOut    []chainLeftOut                   `json:"left_out"`
	Holdings   chainHoldings                    `json:"holdings"`
}

// A chainHead is the manifest's chain: the exported_at of its first export,
// which names it; the account that it selects, A<id>, null for every
// account, resolved once so that a nickname changed at Schwab or another
// account of the same last digits cannot break it; the symbols that -manual
// has named in it, each once; and this export's last day (-to).
type chainHead struct {
	ID      string        `json:"id"`
	Account *string       `json:"account"`
	Manual  []chainManual `json:"manual"`
	To      nytime.Date   `json:"to"`
}

// A chainManual is a symbol that -manual named in the chain, as it was
// given; unused says that no record or price of this export uses it.
type chainManual struct {
	Symbol string `json:"symbol"`
	Unused bool   `json:"unused,omitempty"`
}

// A chainAccount is an account of the manifest (8.1 of the design): one that
// this export writes or names, or that an earlier export of its chain wrote
// or named. Exported says that this export writes it: a PP record of it, or
// Ghostfolio's account in the file; new that no earlier export of the chain
// wrote or named it, so that you make it in PP before this import. A PP
// transfer names the account of its other side as its Offset Account, which
// PP must hold even when this export writes nothing of that account: its
// side is dated after the export's last day. For Ghostfolio, its id there,
// how many balances the file holds and of which days, or why it holds none,
// and its latest snapshot's cash, which you set in Ghostfolio after its first
// import (6.3).
type chainAccount struct {
	Label        string         `json:"label"`
	Account      string         `json:"account"`
	Nickname     string         `json:"nickname"`
	Name         string         `json:"name"`
	Exported     bool           `json:"exported"`
	LastDay      *nytime.Date   `json:"last_day"`
	New          bool           `json:"new"`
	GhostfolioID string         `json:"ghostfolio_id,omitempty"`
	Balances     *chainBalances `json:"balances,omitempty"`
	Cash         *chainCash     `json:"cash,omitempty"`
}

// A chainBalances says how many balances a Ghostfolio account carries and
// the first and last of their dates, or why it carries none.
type chainBalances struct {
	Count int          `json:"count"`
	First *nytime.Date `json:"first,omitempty"`
	Last  *nytime.Date `json:"last,omitempty"`
	Why   string       `json:"why,omitempty"`
}

// A chainCash is an account's cash in dollars, signed, as its latest
// snapshot of a market date holds it.
type chainCash struct {
	Value json.Number `json:"value"`
	Date  nytime.Date `json:"date"`
}

// A chainGiven is what the tool holds of a row (3.8 of the design): its day;
// what the tool shows of its records (chainAs); fp, the hash of the fields
// that the tool keeps of them (fingerprint); the shares row of a two-row cash
// event that its sale carries; and for PP, the keys of the records' sides
// that PP's duplicate rule compares (ppKeys), the ref that the Note PP holds
// names when it is not the row's own, and for a transfer the PP transaction
// that it is of, which both of the transfer's records are of: the head of
// the Note that PP holds of it, its paying side's label and ref, "to", and
// its receiving side's, as "A2 ...398 T61001201 to A1 ...135 T61001202".
type chainGiven struct {
	Day      nytime.Date `json:"day"`
	As       string      `json:"as"`
	FP       string      `json:"fp"`
	With     string      `json:"with,omitempty"`
	Keys     []string    `json:"keys,omitempty"`
	NoteRef  string      `json:"note_ref,omitempty"`
	Transfer string      `json:"transfer,omitempty"`
}

// A chainSecurity is a security that the chain wrote, with the identifiers
// that its latest export wrote it under (8.1 of the design): its key ("" for
// the security that a row without one names by its symbol), its latest
// symbol, and its identity (toolSecurity.identity); for PP the Ticker Symbol,
// WKN and Security Name that PP finds it by, and for Ghostfolio its symbol,
// data source and profile name ("" for a security that Ghostfolio asks Yahoo
// about, which has no profile).
type chainSecurity struct {
	SecurityKey string `json:"security_key"`
	Symbol      string `json:"symbol"`
	Identity    string `json:"identity"`
	Ticker      string `json:"ticker,omitempty"`
	WKN         string `json:"wkn,omitempty"`
	GFSymbol    string `json:"ghostfolio_symbol,omitempty"`
	DataSource  string `json:"data_source,omitempty"`
	Name        string `json:"name,omitempty"`
}

// ref returns how the chain knows the security: by its key, or by its symbol
// when it has none.
func (s chainSecurity) ref() secRef {
	if s.SecurityKey != "" {
		return secRef{key: s.SecurityKey}
	}
	return secRef{symbol: s.Symbol}
}

// written returns the identifiers that the tool got for the security, in
// one string, by which two exports' are compared.
func (s chainSecurity) written() string {
	return strings.Join([]string{s.Ticker, s.WKN, s.GFSymbol, s.DataSource, s.Name}, "\x1f")
}

// A chainLeftOut is a row that the export left out (8.1 of the design): the
// row as the ledger holds it (descriptions and kinds masked free text,
// quantities and amounts null when the store holds none on the grid), why,
// what to do, the row whose outcome decided it (with), and, with -since,
// whether the export it goes on from left it out for the same reason
// (before), so that what you did by hand is not asked of you again.
type chainLeftOut struct {
	Ref         string       `json:"ref"`
	Account     string       `json:"account"`
	Day         *nytime.Date `json:"day"`
	Category    string       `json:"category"`
	Kind        string       `json:"kind"`
	Description string       `json:"description"`
	Symbol      string       `json:"symbol"`
	Quantity    *json.Number `json:"quantity"`
	Amount      *json.Number `json:"amount"`
	Reason      string       `json:"reason"`
	Advice      string       `json:"advice"`
	Command     string       `json:"command"`
	With        string       `json:"with,omitempty"`
	Before      bool         `json:"before,omitempty"`
}

// A chainHoldings is the holdings check of the manifest (4.15 of the
// design): each difference, for PP each difference of the cash check and
// each account whose cash it could not check, and the labels of the
// accounts whose shares it could not check, for want of a snapshot. Cash
// and CashNotChecked are nil for Ghostfolio, whose cash is the balances'.
type chainHoldings struct {
	Differences    []chainHolding        `json:"differences"`
	Cash           *[]chainCashHolding   `json:"cash,omitempty"`
	CashNotChecked *[]chainCashUnchecked `json:"cash_not_checked,omitempty"`
	NotChecked     []string              `json:"not_checked"`
}

// A chainCashUnchecked is an account whose cash PP's cash check could not
// check (toolCashUnchecked): its label, the market date of its snapshot, and
// why, "margin": the snapshot is of a margin account whose cash balance
// reads 0 beside a margin or a short balance, as Schwab can report a debit
// or short positions (docs/schwab-api.md).
type chainCashUnchecked struct {
	Account  string      `json:"account"`
	Snapshot nytime.Date `json:"snapshot"`
	Why      string      `json:"why"`
}

// A chainCashHolding is a difference of PP's cash check (toolCash): the
// account, the market date of the snapshot, its cash and what the records
// leave of it in PP, and, with -since, whether the export it goes on from
// listed the same account with the same difference (before): an opening
// deposit booked in PP changes nothing in the store, so its difference
// comes back in every export.
type chainCashHolding struct {
	Account      string      `json:"account"`
	Snapshot     nytime.Date `json:"snapshot"`
	SnapshotCash json.Number `json:"snapshot_cash"`
	WrittenCash  json.Number `json:"written_cash"`
	Before       bool        `json:"before,omitempty"`
}

// A chainHolding is a difference of the holdings check (toolHolding), its
// shares signed, and, with -since, whether the export it goes on from listed
// the same account and security with the same difference (before): the
// snapshot's shares less the written ones, which trades the store records
// leave as they are. SecurityKeys are, for a security that the tool holds
// under several of the store's keys, every one of them.
type chainHolding struct {
	Account        string      `json:"account"`
	SecurityKey    string      `json:"security_key"`
	SecurityKeys   []string    `json:"security_keys,omitempty"`
	Symbol         string      `json:"symbol"`
	Snapshot       nytime.Date `json:"snapshot"`
	SnapshotShares json.Number `json:"snapshot_shares"`
	WrittenShares  json.Number `json:"written_shares"`
	Before         bool        `json:"before,omitempty"`
}

// A chainChanged is a row that the tool holds in another form than this
// export writes it (3.8 of the design): delete what it was in the tool
// before importing. Was is what the tool shows of it (chainAs), NoteRef the
// ref that its PP Note names, when it is not the row's own, and Transfer,
// for a side of a PP transfer, the head of the Note that PP holds of the
// transfer (chainGiven.Transfer), which names its paying side first. A row
// that the tool holds under the Note of another row, which this export
// writes, is changed too, its form alike: the tool then holds each row under
// its own Note.
type chainChanged struct {
	Ref      string `json:"ref"`
	Account  string `json:"account"`
	Was      string `json:"was"`
	NoteRef  string `json:"note_ref,omitempty"`
	Transfer string `json:"transfer,omitempty"`
	Now      string `json:"now"`
	// day is the row's day as the earlier export gave it, by which the list
	// is in order.
	day nytime.Date
}

// A chainGone is a row that the tool holds and that this export neither
// writes nor gives (3.8 of the design): delete it in the tool. NoteRef and
// Transfer are as a chainChanged's. Why says what became of it: uncounted
// with the store's counted_why, linked to the row that the store counts in
// its place, dated after -to with its day now, no longer in the store,
// left out with its reason, or, for Ghostfolio, now cash, which its
// balances carry.
type chainGone struct {
	Ref      string `json:"ref"`
	Account  string `json:"account"`
	Was      string `json:"was"`
	NoteRef  string `json:"note_ref,omitempty"`
	Transfer string `json:"transfer,omitempty"`
	Why      string `json:"why"`
}

// A chainSameAs is, for PP, a row gone under its ref that this export writes
// under another, alike in everything PP keeps (a file row now linked to its
// API row): PP marks the new record as existing and keeps the old one, so
// neither is to delete or to tick.
type chainSameAs struct {
	Account string `json:"account"`
	Gone    string `json:"gone"`
	New     string `json:"new"`
}

// A toolChain is the manifest of the export that a -since export goes on
// from, as parseChain reads it: that export's exported_at, the store it was
// made from (when the store was made, Q0), its format and layout, and its
// chain's part.
type toolChain struct {
	ExportedAt string `json:"exported_at"`
	Store      string `json:"store"`
	Format     string `json:"format"`
	Layout     int    `json:"layout"`
	chainManifest
}

// chainHex is an fp or a key: 16 lower-case hex digits.
var chainHex = regexp.MustCompile(`^[0-9a-f]{16}$`)

// parseChain reads manifest, the manifest.json of a tool export, for a -since
// export to go on from it. It refuses what no tool export of a layout this
// code knows writes; whether the export may go on from it is refuse's to say.
func parseChain(manifest []byte) (*toolChain, error) {
	var c toolChain
	if err := json.Unmarshal(manifest, &c); err != nil {
		return nil, fmt.Errorf("not a manifest of a tool export: %w", err)
	}
	switch {
	case c.Format != toolPP && c.Format != toolGF:
		return nil, fmt.Errorf("not the manifest of a tool export: its format is %q", c.Format)
	case c.Layout != ToolLayout:
		return nil, fmt.Errorf("the manifest of an export of layout %d, not %d", c.Layout, ToolLayout)
	case c.ExportedAt == "" || c.Store == "" || c.Chain.ID == "":
		return nil, errors.New("the manifest names no export, store or chain")
	case !isDate(c.Chain.To):
		return nil, fmt.Errorf("the manifest's chain ends on %q, which is not a day", c.Chain.To)
	}
	for _, label := range slices.Sorted(maps.Keys(c.Given)) {
		for _, ref := range slices.Sorted(maps.Keys(c.Given[label])) {
			g := c.Given[label][ref]
			bad := !isDate(g.Day) || !chainHex.MatchString(g.FP) || c.Format == toolPP && len(g.Keys) == 0
			for _, k := range g.Keys {
				bad = bad || !chainHex.MatchString(k)
			}
			if bad {
				return nil, fmt.Errorf("the manifest's entry of %s %s is not one that an export writes", label, ref)
			}
		}
	}
	return &c, nil
}

// isDate reports whether d is a day of the calendar, YYYY-MM-DD.
func isDate(d nytime.Date) bool {
	_, err := time.Parse(time.DateOnly, string(d))
	return err == nil
}

// refuse returns why an export of in cannot go on with -since dir from the
// export whose manifest is c, nil when it can (3.8 and 9 of the design): c is
// of another format, or of another store, or ends after in's last day.
func (c *toolChain) refuse(in *toolInput, dir string) error {
	switch {
	case c.Format != in.tool:
		return fmt.Errorf("-since %s: %s is not the manifest of a -format %s export of layout %d", dir, filepath.Join(dir, "manifest.json"), in.tool, ToolLayout)
	case c.Store != in.store:
		return fmt.Errorf("-since %s: that export was made from another store (made %s): export in full and redo in the tool (docs/tools.md)", dir, c.Store)
	case in.to < c.Chain.To:
		return fmt.Errorf("-to %s is before the end of the export in %s, %s", in.to, dir, c.Chain.To)
	}
	return nil
}

// account returns the id of the account that the chain selects, 0 for every
// account.
func (c *toolChain) account() (int64, error) {
	if c.Chain.Account == nil {
		return 0, nil
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(*c.Chain.Account, "A"), 10, 64)
	if err != nil || id <= 0 || !strings.HasPrefix(*c.Chain.Account, "A") {
		return 0, fmt.Errorf("the manifest's chain selects the account %q, which is no A<id>", *c.Chain.Account)
	}
	return id, nil
}

// manual returns the symbols that -manual named in the chain, and then those
// of more that it did not name, each once (compared as -manual compares
// symbols, whatever their case and spaces), as given.
func (c *toolChain) manual(more []string) []string {
	var out []string
	if c != nil {
		for _, m := range c.Chain.Manual {
			out = append(out, m.Symbol)
		}
	}
	return chainManualUnion(out, more)
}

// chainManualUnion returns the symbols of lists, trimmed, each once, as
// -manual compares them (symbolForm), in their order.
func chainManualUnion(lists ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, s := range l {
			s = strings.TrimSpace(s)
			if k := symbolForm(s); s != "" && !seen[k] {
				seen[k] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// chainHash returns the first 16 hex digits of the SHA-256 of fields joined
// by U+001F (7 of the design).
func chainHash(fields []string) string {
	sum := sha256.Sum256([]byte(strings.Join(fields, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

// fingerprint returns a row's fp (3.8 of the design): the hash of the fields
// that tool keeps of the row's records, in their order (chainFields), so that
// two exports of a row that the tool takes alike have one fp, whatever else
// differs.
func fingerprint(tool string, records []*toolEvent) string {
	var fields []string
	for _, e := range records {
		fields = append(fields, chainFields(tool, e)...)
	}
	return chainHash(fields)
}

// chainFields returns the fields that tool keeps of record e (3.8 of the
// design). PP: the file (account or portfolio), Type, Date, Value, Shares,
// Fees, the security as PP finds it (ppFinds), and the Cash, Securities and
// Offset Account; not Time, nor Note, nor a Ticker Symbol or Security Name
// beside the identifier that finds the security, which PP's duplicate rule
// does not compare and which may change between exports. Ghostfolio: every
// key of the activity; its comment holds the row's ref and its symbol the
// security's identity.
func chainFields(tool string, e *toolEvent) []string {
	if tool == toolGF {
		a := gfActivityOf(e)
		return []string{
			a.AccountID, a.Comment, a.Currency, a.DataSource, a.Date, a.Fee.String(), a.Quantity.String(),
			a.Symbol, a.Type, a.UnitPrice.String(),
		}
	}
	r := ppRecordOf(e)
	return []string{r.file, r.typ, r.date, r.value, r.shares, r.fees, ppFinds(r.ticker, r.wkn, r.name), r.cash, r.securities, r.offset}
}

// ppFinds returns the security of a PP record as PP finds it again in every
// export (3.8 and 4.14 of the design): by its WKN, the CUSIP, which a ticker
// that changes leaves as it is; else by its ticker; else by its name; "" for
// a record of no security.
func ppFinds(ticker, wkn, name string) string {
	switch {
	case wkn != "":
		return "wkn " + wkn
	case ticker != "":
		return "ticker " + ticker
	case name != "":
		return "name " + name
	}
	return ""
}

// ppClass returns what PP's duplicate rule takes a type of the file kind
// (ppAccount or ppPortfolio) for (5.9 of the design): in an account, a
// deposit and a transfer in are one, and a withdrawal and a transfer out;
// in a securities account, a buy and a delivery in, and a sale and a
// delivery out; any other type is itself alone.
func ppClass(kind, typ string) string {
	switch {
	case kind == ppAccount && (typ == "Deposit" || typ == "Transfer (Inbound)"),
		kind == ppPortfolio && (typ == "Buy" || typ == "Delivery (Inbound)"):
		return "in"
	case kind == ppAccount && (typ == "Withdrawal" || typ == "Transfer (Outbound)"),
		kind == ppPortfolio && (typ == "Sell" || typ == "Delivery (Outbound)"):
		return "out"
	}
	return typ
}

// ppKeys returns the keys of record e for PP's duplicate rule (3.8 of the
// design), one for each side of the record that the rule compares: the hash
// of the side's account or securities account, its class (ppClass), day,
// currency, amount, shares and security as PP finds it; and how many of
// them, from the first, PP checks the record by when it is new. A record of
// cash has one side, in its account; a buy or a sale two, both checked: its
// account side, of its type, amount and security but no shares, which a
// buy-sell entry puts on its portfolio side alone, and its portfolio side; a
// delivery one, in its securities account; a transfer two, its paying leg in
// the paying account, which PP checks whichever account's file holds the
// record, and its receiving leg, which an import adds without a check.
func ppKeys(e *toolEvent) (keys []string, checked int) {
	r := ppRecordOf(e)
	security := ppFinds(r.ticker, r.wkn, r.name)
	side := func(kind, owner, typ, shares, security string) string {
		return chainHash([]string{kind, owner, ppClass(kind, typ), r.date, ppCurrency, r.value, shares, security})
	}
	switch e.kind {
	case evTransferOut, evTransferIn:
		payer, payee := r.cash, r.offset
		if e.kind == evTransferIn {
			payer, payee = r.offset, r.cash
		}
		return []string{side(ppAccount, payer, "Transfer (Outbound)", "", ""), side(ppAccount, payee, "Transfer (Inbound)", "", "")}, 1
	case evBuy, evSell:
		return []string{side(ppAccount, r.cash, r.typ, "", security), side(ppPortfolio, r.securities, r.typ, r.shares, security)}, 2
	}
	if r.file == ppPortfolio {
		return []string{side(ppPortfolio, r.securities, r.typ, r.shares, security)}, 1
	}
	return []string{side(ppAccount, r.cash, r.typ, "", security)}, 1
}

// chainAs returns what tool shows of a row's records, by which the lists of
// what changed or went name it: for PP each record's file, day, Type and
// Value, as "portfolio 2025-01-21 Buy 400.67"; for Ghostfolio each activity's
// day, type and value, its quantity times its unit price in dollars and
// cents, or a FEE's fee, and the fee of any other with one, which its value
// leaves out, as "2025-01-21 BUY 400.00 fee 0.67"; the records joined by
// "; ".
func chainAs(tool string, records []*toolEvent) string {
	var out []string
	for _, e := range records {
		if tool == toolGF {
			a := gfActivityOf(e)
			value, fee := new(big.Rat).Mul(chainRat(a.Quantity), chainRat(a.UnitPrice)), chainRat(a.Fee)
			if a.Type == "FEE" {
				value, fee = fee, new(big.Rat)
			}
			s := string(e.day) + " " + a.Type + " " + formatCents(roundUnits(value, 2).Int64())
			if fee.Sign() != 0 {
				s += " fee " + formatCents(roundUnits(fee, 2).Int64())
			}
			out = append(out, s)
			continue
		}
		r := ppRecordOf(e)
		out = append(out, r.file+" "+r.date+" "+r.typ+" "+r.value)
	}
	return strings.Join(out, "; ")
}

// chainRat returns n, a number that the export wrote from exact text, as
// that rational.
func chainRat(n json.Number) *big.Rat {
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		return new(big.Rat)
	}
	return r
}

// A chainResult is what compareChain makes of an export: each row's outcome
// with -since, the records to write, what changed, went and is the same as
// before, the next export's given, the rows left out and the holdings that
// differ with what the earlier export listed of them, the securities as the
// chain wrote them, and, for PP, what its replay of the duplicate rule starts
// from.
type chainResult struct {
	in    *toolInput
	ix    *toolIndex
	since *toolChain
	// outcomes are the rows' outcomes, in their order: the mapping's, but
	// given for a row whose records the tool holds already.
	outcomes []toolOutcome
	// written are the records to write: those of the rows written, in the
	// mapping's order.
	written []toolEvent
	// changed, gone and sameAs are the rows that changed or went, and for PP
	// those under a new ref, in the order of their accounts, days and refs.
	changed []chainChanged
	gone    []chainGone
	sameAs  []chainSameAs
	// late counts the rows written that the earlier export did not give and
	// that are dated on or before its last day, but those the same as a row
	// gone: they came late, were decided or paired since.
	late int
	// given is the next given: what the tool holds once this export is
	// imported.
	given map[string]map[string]chainGiven
	// leftOut, holdings and unchecked are the rows left out, the differences
	// of the holdings check and the accounts it could not check, with what
	// the earlier export listed of them.
	leftOut   []chainLeftOut
	holdings  []chainHolding
	unchecked []toolAccount
	// cash are, for PP, the differences of the cash check, and
	// cashUnchecked the accounts whose cash it could not check.
	cash          []chainCashHolding
	cashUnchecked []toolCashUnchecked
	// securities are every security that the chain has written, with its
	// identifiers as written last; symbolsChanged those of this export whose
	// symbol a tool that holds them under an earlier one needs telling of,
	// one list for each of the tool's securities (sameSecurity), of the
	// store's securities of it whose symbol changed, in their order.
	securities     []chainSecurity
	symbolsChanged [][]*toolSecurity
	// kept holds the earlier export's entries of the rows given, and oldOf,
	// for PP, the row that each row written under a new ref is the same as.
	kept  map[rowKey]chainGiven
	oldOf map[rowKey]chainOld
	// deleted are, for PP, the names of the transactions that you delete
	// before importing (chainOld.tx): those of the rows changed or gone,
	// with the other half of a transfer.
	deleted map[string]bool
	// start is, for PP, what PP holds before this export's files: the keys
	// of each transaction, by its name.
	start map[string][]string
}

// A chainOld is an entry of the earlier export's given: the row it is of,
// by its account's label and its ref, and what the tool holds of it.
type chainOld struct {
	label, ref string
	given      chainGiven
}

// tx returns the name of the PP transaction that the entry is of, in the
// replay of PP's duplicate rule, which is the head of the Note that PP holds
// of it, so that no two transactions that PP holds share one: a transfer's,
// "<paying side> to <receiving side>", which both its records name; else
// the row's, its label and the ref that the Note names, which no other row
// of the account has (compareChain lists an entry whose Note names a row
// that this export writes as changed). The records of one row, legs or a
// sale with its Fees row, share the row's name: they are deleted together.
func (o chainOld) tx() string {
	return cmp.Or(o.given.Transfer, o.label+" "+cmp.Or(o.given.NoteRef, o.ref))
}

// compareChain compares the export of in, whose mapping gave events and
// outcomes (toolEvents), with the export since, whose manifest it goes on
// from, nil for a full export (3.8 of the design). A row that since gave
// with the fp it has now is given, not written; one it gave with another is
// written and changed; one it gave that this export neither writes nor
// gives is gone, with why; a row that it did not give is written, whatever
// its day. For PP a gone row whose fp equals that of a row written now in its
// account under another ref is the same as it, and neither is to delete or to
// tick, unless PP will not hold the gone row's transaction: you delete it
// with the other half of a transfer, or its Note names a row that PP will
// hold under its own. A row given whose Note names such a row is changed,
// so that each transaction that PP holds has a Note of its own row. The rows
// left out and the holdings that differ are marked before when since listed
// them alike.
func compareChain(in *toolInput, events []toolEvent, outcomes []toolOutcome, since *toolChain) *chainResult {
	c := &chainResult{
		in: in, ix: newToolIndex(in), since: since, outcomes: slices.Clone(outcomes),
		given: map[string]map[string]chainGiven{}, kept: map[rowKey]chainGiven{}, oldOf: map[rowKey]chainOld{}, start: map[string][]string{},
	}
	records := map[int][]*toolEvent{}
	for i := range events {
		records[events[i].row] = append(records[events[i].row], &events[i])
	}

	// The earlier export's given, by account and ref; an entry whose label
	// names no account of the store is gone with its rows.
	labels := map[string]toolAccount{}
	for _, a := range in.accounts {
		labels[a.label()] = a
	}
	old := map[rowKey]chainOld{}
	var unknown []chainOld
	if since != nil {
		for label, refs := range since.Given {
			for ref, g := range refs {
				if a, ok := labels[label]; ok {
					old[rowKey{a.id, ref}] = chainOld{label, ref, g}
				} else {
					unknown = append(unknown, chainOld{label, ref, g})
				}
			}
		}
	}

	type fresh struct {
		i  int
		fp string
	}
	var news []fresh
	var gone []chainOld
	why := map[[2]string]string{} // by label and ref
	seen := map[rowKey]bool{}
	for i, o := range outcomes {
		r := &in.rows[i]
		k := rowKey{r.account, r.ref}
		prev, had := old[k]
		seen[k] = had
		switch {
		case o.outcome == outcomeWritten:
			fp := fingerprint(in.tool, records[i])
			switch {
			case !had:
				news = append(news, fresh{i, fp})
			case prev.given.FP == fp:
				c.outcomes[i].outcome = outcomeGiven
				c.kept[k] = prev.given
			default:
				c.changed = append(c.changed, chainChanged{
					Ref: r.ref, Account: prev.label, Was: prev.given.As, NoteRef: prev.given.NoteRef, Transfer: prev.given.Transfer,
					Now: chainAs(in.tool, records[i]), day: prev.given.Day,
				})
			}
		case had:
			gone = append(gone, prev)
			w := "left out: " + o.reason
			switch o.outcome {
			case outcomeInBalances:
				w = "now cash, in the balances"
			case outcomeCombined:
				w = "combined with " + o.with
			}
			why[[2]string{prev.label, prev.ref}] = w
		}
	}
	for k, o := range old {
		if !seen[k] {
			gone = append(gone, o)
			why[[2]string{o.label, o.ref}] = c.whyGone(k)
		}
	}
	for _, o := range unknown {
		gone = append(gone, o)
		why[[2]string{o.label, o.ref}] = "no longer in the store"
	}
	slices.SortFunc(gone, func(a, b chainOld) int {
		return cmp.Or(cmp.Compare(c.labelOrder(a.label), c.labelOrder(b.label)), cmp.Compare(a.given.Day, b.given.Day), cmp.Compare(a.ref, b.ref))
	})

	// For PP, a gone row is the same as a row written under a new ref when
	// both are of one account and PP keeps them alike: each pair once, in
	// order. PP then holds the gone row's transaction for the new row, unless
	// you delete it as the other half of a transfer that changed or went.
	pairs := map[int]int{} // a gone row's index in gone to its new row's in news
	if in.tool == toolPP {
		matched := map[int]bool{}
		for g, o := range gone {
			a, ok := labels[o.label]
			j := slices.IndexFunc(news, func(f fresh) bool {
				return ok && !matched[f.i] && in.rows[f.i].account == a.id && f.fp == o.given.FP
			})
			if j >= 0 {
				pairs[g] = j
				matched[news[j].i] = true
			}
		}
	}
	rows := map[rowKey]int{}
	for i := range in.rows {
		rows[rowKey{in.rows[i].account, in.rows[i].ref}] = i
	}
	noted := map[rowKey]rowKey{} // the ref that an entry's Note names, of its account, to the entry
	for k, o := range old {
		if o.given.NoteRef != "" {
			noted[rowKey{k.account, o.given.NoteRef}] = k
		}
	}
	// The sides of the transfers that this export writes, for PP, the paying
	// sides first.
	var sides []int
	if in.tool == toolPP {
		for _, kind := range []eventKind{evTransferOut, evTransferIn} {
			for i := range in.rows {
				if rs := records[i]; len(rs) > 0 && rs[0].kind == kind {
					sides = append(sides, i)
				}
			}
		}
	}
	for {
		// The rows that PP will hold under their own Note: those written now
		// that no gone row is the same as, and those changed. (A row given
		// that is written again below is not one whose Note an entry names:
		// the entry that named it was changed when it came back.)
		paired := map[int]bool{}
		for _, j := range pairs {
			paired[j] = true
		}
		own := map[rowKey]bool{}
		for j, f := range news {
			if !paired[j] {
				own[rowKey{in.rows[f.i].account, in.rows[f.i].ref}] = true
			}
		}
		for _, ch := range c.changed {
			if a, ok := labels[ch.Account]; ok {
				own[rowKey{a.id, ch.Ref}] = true
			}
		}
		// An entry whose Note names such a row, which PP would hold besides
		// the row's own: given, it is changed, so that you delete the one
		// that PP holds and this export writes its row again under its own
		// Note; gone, it is not the same as a row written now, and you delete
		// it. No two transactions that PP holds then have Notes of one row.
		again := false
		for _, k := range noted {
			prev := old[k]
			i, ok := rows[k]
			if !ok || c.outcomes[i].outcome != outcomeGiven || !own[rowKey{k.account, prev.given.NoteRef}] {
				continue
			}
			c.outcomes[i].outcome = outcomeWritten
			delete(c.kept, k)
			c.changed = append(c.changed, chainChanged{
				Ref: prev.ref, Account: prev.label, Was: prev.given.As, NoteRef: prev.given.NoteRef, Transfer: prev.given.Transfer,
				Now: chainAs(in.tool, records[i]), day: prev.given.Day,
			})
			again = true
		}
		for g := range pairs {
			if a, ok := labels[gone[g].label]; ok && own[rowKey{a.id, gone[g].given.NoteRef}] {
				delete(pairs, g)
				again = true
			}
		}
		// What you delete: the transactions of the rows changed, and of the
		// rows gone that no row is the same as.
		c.deleted = map[string]bool{}
		for _, ch := range c.changed {
			if a, ok := labels[ch.Account]; ok {
				c.deleted[old[rowKey{a.id, ch.Ref}].tx()] = true
			}
		}
		for g, o := range gone {
			if _, ok := pairs[g]; !ok {
				c.deleted[o.tx()] = true
			}
		}
		// A gone row whose transaction you delete with another row's is not
		// the same as the new row: PP will not hold it.
		for g := range pairs {
			if c.deleted[gone[g].tx()] {
				delete(pairs, g)
				again = true
			}
		}
		// A transfer's side given whose transfer you delete with another
		// row's, its other side of then having changed or gone, of which no
		// record of this export makes the transfer that it is of now: its
		// other side now is given too, or is not in this export. It is
		// written again, and listed nowhere, since PP holds nothing of it
		// once you delete: its record makes the transfer.
		for _, i := range sides {
			e := records[i][0]
			k := rowKey{e.account.id, e.ref}
			if c.outcomes[i].outcome != outcomeGiven || !c.deleted[c.kept[k].Transfer] {
				continue
			}
			if j, ok := rows[rowKey{e.pair.id, e.pairRef}]; ok && c.outcomes[j].outcome == outcomeWritten {
				continue
			}
			c.outcomes[i].outcome = outcomeWritten
			delete(c.kept, k)
			again = true
		}
		if !again {
			break
		}
	}
	paired := map[int]bool{}
	for g, o := range gone {
		j, ok := pairs[g]
		if !ok {
			c.gone = append(c.gone, chainGone{
				Ref: o.ref, Account: o.label, Was: o.given.As, NoteRef: o.given.NoteRef, Transfer: o.given.Transfer,
				Why: why[[2]string{o.label, o.ref}],
			})
			continue
		}
		r := &in.rows[news[j].i]
		paired[j] = true
		c.oldOf[rowKey{r.account, r.ref}] = o
		c.sameAs = append(c.sameAs, chainSameAs{Account: o.label, Gone: o.ref, New: r.ref})
	}
	for j, f := range news {
		if since != nil && !paired[j] && in.rows[f.i].day <= since.Chain.To {
			c.late++
		}
	}
	slices.SortStableFunc(c.changed, func(a, b chainChanged) int {
		return cmp.Or(cmp.Compare(c.labelOrder(a.Account), c.labelOrder(b.Account)), cmp.Compare(a.day, b.day), cmp.Compare(a.Ref, b.Ref))
	})

	for i := range events {
		if c.outcomes[events[i].row].outcome == outcomeWritten {
			c.written = append(c.written, events[i])
		}
	}
	c.replayStart(old, unknown)
	c.nextGiven(records)
	c.compareLeftOut()
	c.compareHoldings(events)
	c.compareCash(events)
	c.compareSecurities(events)
	return c
}

// labelOrder returns where the account of label comes in the lists: by its
// id, an account that the store does not know last.
func (c *chainResult) labelOrder(label string) int64 {
	for _, a := range c.in.accounts {
		if a.label() == label {
			return a.id
		}
	}
	return math.MaxInt64
}

// whyGone returns why the row k, which the earlier export gave and which
// this export does not select, went: uncounted, with the store's
// counted_why, or linked to the rows that the store counts in its place
// (Q10); dated after -to, with its day now, which an export up to that day
// writes again (Q11); or no longer in the store.
func (c *chainResult) whyGone(k rowKey) string {
	for _, u := range c.in.uncounted {
		if u.account != k.account || u.ref != k.ref {
			continue
		}
		if u.why == "linked" && u.matchedWith != "" {
			return "linked to " + strings.ReplaceAll(u.matchedWith, ",", ", ")
		}
		return "uncounted: " + u.why
	}
	for _, l := range c.in.later {
		if l.account == k.account && l.ref == k.ref {
			return "dated after -to: " + string(l.day)
		}
	}
	return "no longer in the store"
}

// nextGiven makes the next given: the earlier export's entry of each row
// given, and a new entry of each row written, which for PP keeps, of a row
// the same as a gone one, the ref that the Note PP holds names, and of a
// transfer, the PP transaction that it is of (txOf). A transfer's side given
// is of the transfer that it makes now with its other side, whichever that
// is (txOf): its paying side's, which PP holds still, or that a record
// written now makes, under its Note; with another name, its entry names no
// other row's Note.
func (c *chainResult) nextGiven(records map[int][]*toolEvent) {
	for i, o := range c.outcomes {
		r := &c.in.rows[i]
		k := rowKey{r.account, r.ref}
		label := c.ix.account(r.account).label()
		var g chainGiven
		switch o.outcome {
		case outcomeGiven:
			g = c.kept[k]
			if e := records[i][0]; c.in.tool == toolPP && (e.kind == evTransferOut || e.kind == evTransferIn) {
				if tx := c.txOf(e); tx != g.Transfer {
					g.Transfer, g.NoteRef = tx, ""
				}
			}
		case outcomeWritten:
			g = chainGiven{Day: r.day, As: chainAs(c.in.tool, records[i]), FP: fingerprint(c.in.tool, records[i]), With: o.with}
			if c.in.tool == toolPP {
				for _, e := range records[i] {
					keys, _ := ppKeys(e)
					g.Keys = append(g.Keys, keys...)
				}
				if prev, ok := c.oldOf[k]; ok && cmp.Or(prev.given.NoteRef, prev.ref) != r.ref {
					g.NoteRef = cmp.Or(prev.given.NoteRef, prev.ref)
				}
				if e := records[i][0]; e.kind == evTransferOut || e.kind == evTransferIn {
					g.Transfer = c.txOf(e)
				}
			}
		default:
			continue
		}
		if c.given[label] == nil {
			c.given[label] = map[string]chainGiven{}
		}
		c.given[label][r.ref] = g
	}
}

// replayStart sets, for PP, what PP holds before this export's files: every
// transaction that the earlier export's given holds, a transfer once for its
// two records, less those that you delete before importing (deleted), which
// are each row's changed or gone, with the other half of a transfer (3.8 of
// the design). A row gone that a row written now is the same as stays: PP
// keeps it.
func (c *chainResult) replayStart(old map[rowKey]chainOld, unknown []chainOld) {
	if c.in.tool != toolPP {
		return
	}
	all := slices.Concat(slices.Collect(maps.Values(old)), unknown)
	slices.SortFunc(all, func(a, b chainOld) int { return cmp.Or(cmp.Compare(a.label, b.label), cmp.Compare(a.ref, b.ref)) })
	for _, o := range all {
		if tx := o.tx(); !c.deleted[tx] && c.start[tx] == nil {
			c.start[tx] = o.given.Keys
		}
	}
}

// txOf returns, for PP, the name of the transaction that record e, of a row
// written now or given, makes or is of (chainOld.tx): a transfer's is that of
// its paying side, both of whose records make one transaction; any other
// its row's. A row that the earlier export gave keeps the name it gave it,
// and a row the same as a gone one takes the gone one's, since PP holds that
// transaction; but a transaction that you delete is made again, by the
// record of this export that PP imports, and named by that record's Note.
func (c *chainResult) txOf(e *toolEvent) string {
	k := rowKey{e.account.id, e.ref}
	if e.kind == evTransferIn {
		k = rowKey{e.pair.id, e.pairRef}
	}
	if g, ok := c.kept[k]; ok {
		if tx := (chainOld{c.ix.account(k.account).label(), k.ref, g}).tx(); !c.deleted[tx] {
			return tx
		}
	} else if o, ok := c.oldOf[k]; ok {
		return o.tx()
	}
	if e.kind == evTransferOut || e.kind == evTransferIn {
		return ppTransferHead(e)
	}
	return c.ix.account(k.account).label() + " " + k.ref
}

// compareLeftOut lists the rows left out, each marked before when the
// earlier export left it out for the same reason.
func (c *chainResult) compareLeftOut() {
	before := map[[3]string]bool{}
	if c.since != nil {
		for _, l := range c.since.LeftOut {
			before[[3]string{l.Account, l.Ref, l.Reason}] = true
		}
	}
	for i, o := range c.outcomes {
		if o.outcome != outcomeLeftOut {
			continue
		}
		r := &c.in.rows[i]
		l := chainLeftOut{
			Ref: r.ref, Account: o.account.label(), Category: r.category, Kind: r.kind, Description: r.description, Symbol: r.symbol,
			Reason: o.reason, Advice: o.advice, Command: o.command, With: o.with,
		}
		if r.day != "" {
			l.Day = &r.day
		}
		if r.quantity.Valid {
			q := json.Number(formatMicros(r.quantity.Int64))
			l.Quantity = &q
		}
		if r.amount.Valid {
			a := json.Number(formatCents(r.amount.Int64))
			l.Amount = &a
		}
		l.Before = before[[3]string{l.Account, l.Ref, l.Reason}]
		c.leftOut = append(c.leftOut, l)
	}
}

// compareHoldings runs the holdings check over the records of every row
// written or given, events (4.15 of the design), and marks before each
// difference that the earlier export listed for the same account and
// security with the same difference: an opening position added by hand in
// the tool changes nothing in the store, so its difference comes back in
// every export.
func (c *chainResult) compareHoldings(events []toolEvent) {
	diffs, unchecked := holdingsCheck(c.in, events)
	c.unchecked = unchecked
	before := map[string]bool{}
	if c.since != nil {
		for _, h := range c.since.Holdings.Differences {
			d := new(big.Rat).Sub(chainRat(h.SnapshotShares), chainRat(h.WrittenShares))
			before[chainHoldingKey(h.Account, h.SecurityKey, h.Symbol, d)] = true
		}
	}
	for _, d := range diffs {
		h := chainHolding{
			Account: d.account.label(), SecurityKey: d.key, SecurityKeys: d.keys, Symbol: d.symbol, Snapshot: d.snapshot,
			SnapshotShares: json.Number(formatMicros(d.snapshotShares)), WrittenShares: json.Number(formatMicros(d.writtenShares)),
		}
		h.Before = before[chainHoldingKey(h.Account, h.SecurityKey, h.Symbol, big.NewRat(d.snapshotShares-d.writtenShares, 1_000_000))]
		c.holdings = append(c.holdings, h)
	}
}

// compareCash runs PP's cash check over the records of every row written
// or given, events, and marks before each difference that the earlier
// export listed for the same account with the same difference, the
// snapshot's cash less the records'.
func (c *chainResult) compareCash(events []toolEvent) {
	if c.in.tool != toolPP {
		return
	}
	before := map[string]bool{}
	if c.since != nil && c.since.Holdings.Cash != nil {
		for _, h := range *c.since.Holdings.Cash {
			d := new(big.Rat).Sub(chainRat(h.SnapshotCash), chainRat(h.WrittenCash))
			before[h.Account+"\x1f"+d.RatString()] = true
		}
	}
	diffs, unchecked := cashCheck(c.in, events)
	c.cashUnchecked = unchecked
	for _, d := range diffs {
		h := chainCashHolding{
			Account: d.account.label(), Snapshot: d.snapshot,
			SnapshotCash: json.Number(formatCents(d.snapshotCash)), WrittenCash: json.Number(formatCents(d.writtenCash)),
		}
		h.Before = before[h.Account+"\x1f"+big.NewRat(d.snapshotCash-d.writtenCash, 100).RatString()]
		c.cash = append(c.cash, h)
	}
}

// chainHoldingKey names a difference of the holdings check: its account,
// its security (by its key, or by its symbol for one without), and the
// snapshot's shares less the written ones.
func chainHoldingKey(account, key, symbol string, diff *big.Rat) string {
	if key != "" {
		symbol = ""
	}
	return strings.Join([]string{account, key, symbol, diff.RatString()}, "\x1f")
}

// compareSecurities makes the chain's securities, the earlier export's with
// those that this export writes (listSecurities) as it writes them, and the
// securities whose symbol changed: with -since, those that this export
// writes under other identifiers than the earlier one, which a tool that
// holds them does not follow by itself (3.5 of the design), and those that
// the chain wrote and that a row given of events names, or that an account
// that the export selects holds, whose identifiers would be written
// otherwise now: PP finds a security whose CUSIP the store knows by its WKN,
// so its rows stay given when it is renamed, and PP keeps its old ticker,
// and that ticker's price feed, until you give it the new one (4.14);
// without -since, those that have had another ticker or are delisted. The
// store's securities that the tool takes for one (mergeListed) are one
// security whose symbol changed, as the tool holds them.
func (c *chainResult) compareSecurities(events []toolEvent) {
	byRef := map[secRef]chainSecurity{}
	if c.since != nil {
		for _, s := range c.since.Securities {
			byRef[s.ref()] = s
		}
	}
	listed := listSecurities(c.in, c.written)
	if c.since != nil {
		known := map[secRef]bool{}
		for _, l := range listed {
			known[l.security.ref()] = true
		}
		var held []*toolSecurity
		for i := range events {
			if s := events[i].security; s != nil && c.outcomes[events[i].row].outcome == outcomeGiven {
				held = append(held, s)
			}
		}
		for _, s := range append(held, toolHeld(c.in)...) {
			if _, had := byRef[s.ref()]; had && !known[s.ref()] {
				known[s.ref()] = true
				listed = append(listed, toolListed{security: s})
			}
		}
		slices.SortFunc(listed, func(a, b toolListed) int {
			return cmp.Or(cmp.Compare(a.security.key, b.security.key), cmp.Compare(a.security.symbol, b.security.symbol))
		})
	}
	changed := map[secRef]bool{}
	for _, l := range listed {
		s := l.security
		w := chainSecurity{SecurityKey: s.key, Symbol: s.symbol, Identity: s.identity()}
		if c.in.tool == toolGF {
			w.GFSymbol, w.DataSource = s.gfSymbol(), s.gfDataSource()
			if s.manual != "" {
				w.Name = s.gfName()
			}
		} else {
			w.Ticker, w.WKN, w.Name = ppIdentifiers(s)
		}
		if prev, had := byRef[w.ref()]; c.since == nil && s.symbolsChanged() || c.since != nil && had && prev.written() != w.written() {
			changed[s.ref()] = true
		}
		byRef[w.ref()] = w
	}
	for _, l := range mergeListed(c.in.tool, listed) {
		var of []*toolSecurity
		for _, s := range l.members {
			if changed[s.ref()] {
				of = append(of, s)
			}
		}
		if len(of) > 0 {
			c.symbolsChanged = append(c.symbolsChanged, of)
		}
	}
	c.securities = slices.SortedFunc(maps.Values(byRef), func(a, b chainSecurity) int {
		return cmp.Or(cmp.Compare(a.SecurityKey, b.SecurityKey), cmp.Compare(a.Symbol, b.Symbol))
	})
}

// A ppMark is a record of a PP export that PP's review page marks as
// existing (ppTicks), which PP imports only when you tick it: the file and
// its row (from 1, below the header), the record, and whether to tick it,
// since the record is new to PP all the same.
type ppMark struct {
	file   string
	line   int
	record *toolEvent
	tick   bool
}

// A chainTick is a record of the manifest's tick (8.1 of the design): a row
// of a file that PP will mark as existing although it is new, which you tick.
type chainTick struct {
	File    string `json:"file"`
	Ref     string `json:"ref"`
	Account string `json:"account"`
	As      string `json:"as"`
}

// ppTickList returns the marks to tick, as the manifest's tick lists them.
func ppTickList(marks []ppMark) []chainTick {
	out := []chainTick{}
	for _, m := range marks {
		if m.tick {
			out = append(out, chainTick{File: m.file, Ref: m.record.ref, Account: m.record.account.label(), As: chainAs(toolPP, []*toolEvent{m.record})})
		}
	}
	return out
}

// ppTicks replays PP's duplicate rule over the records of files, a PP
// export's files in the order to import them (ppFiles), from what PP holds
// before them (3.8 and 5.9 of the design), and returns each record that PP's
// review page will mark as existing. PP checks a file's records against what
// its accounts hold before the file, never against each other, by the keys
// that it checks a new record by (ppKeys), and then imports each that it does
// not mark and each that is ticked, every side of it. A record that a
// transaction marks is to tick, unless the transaction is its own: a
// transfer's other half, imported earlier in this order, or the record that
// PP holds of a row the same as it.
func ppTicks(files []ppFile, c *chainResult) []ppMark {
	held := map[string][]string{}
	for _, tx := range slices.Sorted(maps.Keys(c.start)) {
		for _, k := range c.start[tx] {
			held[k] = append(held[k], tx)
		}
	}
	var marks []ppMark
	for _, f := range files {
		type added struct {
			tx   string
			keys []string
		}
		var adds []added
		for j, e := range f.records {
			keys, checked := ppKeys(e)
			tx := c.txOf(e)
			var holders []string
			for _, k := range keys[:checked] {
				holders = append(holders, held[k]...)
			}
			own := slices.Contains(holders, tx)
			if len(holders) > 0 {
				marks = append(marks, ppMark{file: f.name, line: j + 1, record: e, tick: !own})
			}
			if len(holders) == 0 || !own {
				adds = append(adds, added{tx, keys})
			}
		}
		for _, a := range adds {
			for _, k := range a.keys {
				held[k] = append(held[k], a.tx)
			}
		}
	}
	return marks
}

// chainJSON returns the chain's part of the manifest of the export that c
// compared (3.8 and 8.1 of the design), exported at exportedAt, which names
// the chain when the export goes on from none; unused are the symbols that
// -manual names and nothing of the export uses (checkManual).
func chainJSON(c *chainResult, exportedAt string, unused []string) chainManifest {
	m := chainManifest{
		Chain:      chainHead{ID: exportedAt, Manual: []chainManual{}, To: c.in.to},
		Accounts:   c.accounts(),
		Given:      c.given,
		Securities: slices.Clone(c.securities),
		LeftOut:    slices.Clone(c.leftOut),
		Holdings:   chainHoldings{Differences: slices.Clone(c.holdings), NotChecked: []string{}},
	}
	if c.since != nil {
		m.Chain.ID = c.since.Chain.ID
	}
	if c.in.account != 0 {
		a := "A" + strconv.FormatInt(c.in.account, 10)
		m.Chain.Account = &a
	}
	for _, s := range chainManualUnion(c.in.manual) {
		m.Chain.Manual = append(m.Chain.Manual, chainManual{Symbol: s, Unused: slices.Contains(unused, s)})
	}
	for _, a := range c.unchecked {
		m.Holdings.NotChecked = append(m.Holdings.NotChecked, a.label())
	}
	if m.Securities == nil {
		m.Securities = []chainSecurity{}
	}
	if m.LeftOut == nil {
		m.LeftOut = []chainLeftOut{}
	}
	if m.Holdings.Differences == nil {
		m.Holdings.Differences = []chainHolding{}
	}
	if c.in.tool == toolPP {
		cash := append([]chainCashHolding{}, c.cash...)
		notChecked := []chainCashUnchecked{}
		for _, u := range c.cashUnchecked {
			notChecked = append(notChecked, chainCashUnchecked{Account: u.account.label(), Snapshot: u.snapshot, Why: "margin"})
		}
		m.Holdings.Cash, m.Holdings.CashNotChecked = &cash, &notChecked
	}
	return m
}

// accounts returns the accounts of the manifest, by id: each that this
// export writes or names as a transfer's Offset Account, and each that an
// earlier export of the chain wrote or named.
func (c *chainResult) accounts() []chainAccount {
	exported, named := map[int64]bool{}, map[int64]bool{}
	last := map[int64]nytime.Date{}
	for _, e := range c.written {
		exported[e.account.id] = true
		last[e.account.id] = max(last[e.account.id], e.day)
		if e.kind == evTransferOut || e.kind == evTransferIn {
			named[e.pair.id] = true
		}
	}
	var balances map[int64][]toolBalance
	if c.in.tool == toolGF {
		for _, a := range gfAccounts(c.in, c.written) {
			for _, ta := range c.in.accounts {
				if ta.gfID() == a.ID {
					exported[ta.id] = true
				}
			}
		}
		balances = toolBalances(c.in)
	}
	before := map[string]bool{}
	if c.since != nil {
		for _, a := range c.since.Accounts {
			before[a.Label] = true
		}
	}
	out := []chainAccount{}
	for _, a := range c.in.accounts {
		listed := exported[a.id] || named[a.id]
		if !listed && !before[a.label()] {
			continue
		}
		ca := chainAccount{
			Label: a.label(), Account: a.masked, Nickname: a.nickname, Name: a.name(),
			Exported: exported[a.id], New: listed && !before[a.label()],
		}
		if d, ok := last[a.id]; ok {
			ca.LastDay = &d
		}
		if c.in.tool == toolGF {
			ca.GhostfolioID = a.gfID()
			ca.Balances = &chainBalances{Count: len(balances[a.id])}
			if bs := balances[a.id]; len(bs) > 0 {
				lo, hi := bs[0], bs[len(bs)-1]
				ca.Balances.First, ca.Balances.Last = &lo.date, &hi.date
				ca.Cash = &chainCash{Value: json.Number(formatCents(hi.cash)), Date: hi.date}
			} else {
				ca.Balances.Why = "no snapshot"
				if slices.ContainsFunc(c.in.snapshots, func(s toolSnapshot) bool { return s.account == a.id }) {
					ca.Balances.Why = "no snapshot holds its cash"
				}
			}
		}
		out = append(out, ca)
		delete(before, a.label())
	}
	for _, label := range slices.Sorted(maps.Keys(before)) {
		out = append(out, chainAccount{Label: label})
	}
	return out
}
