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
// transfer names the receiving account as its Offset Account, which PP must
// hold even when this export writes nothing of that account: the transfer
// carries its side, or its side is dated after the export's last day. For
// Ghostfolio, its id there, how many balances the file holds and of which
// days, or why it holds none, and its latest snapshot's cash, which you set
// in Ghostfolio after its first import (6.3).
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

// A chainGiven is what the tool holds of a row (3.8 of the design, R-7 of
// the consolidation): its day; what the tool shows of its records (chainAs);
// fp, the hash of the fields that the tool keeps of them (fingerprint); the
// shares row of a two-row cash event that its sale carries; and for PP,
// Note, how the Note that PP holds of it begins when that is not its own
// label and ref: of a row that took a new ref since PP got it (same_as), the
// old one's, as "A1 ...135 C7a0b1c2d.1"; of a transfer, whose one entry is
// its paying side's, the head that it was written with, its paying side's
// label and ref, "to", and its receiving side's, as "A2 ...398 T61001201 to
// A1 ...135 T61001202". Note is set when the row is written, and copied as
// it was while the row is given.
type chainGiven struct {
	Day  nytime.Date `json:"day"`
	As   string      `json:"as"`
	FP   string      `json:"fp"`
	With string      `json:"with,omitempty"`
	Note string      `json:"note,omitempty"`
}

// A chainSecurity is a security that the chain wrote, with the identifiers
// that its latest export wrote it under (8.1 of the design): its key ("" for
// the security that a row without one names by its symbol), its latest
// symbol, and its identity (toolSecurity.identity); for PP the Ticker Symbol,
// WKN and Security Name that PP finds it by, and the whys of symbols_changed
// that it had of itself (symbolWhys), which a later export names it again
// for only when it lacks one; for Ghostfolio, kept for the record and
// compared with nothing, its symbol, data source and profile name ("" for a
// security that Ghostfolio asks Yahoo about, which has no profile); and
// unsure when it was written on its own, of a key under keys_unsure.
type chainSecurity struct {
	SecurityKey string   `json:"security_key"`
	Symbol      string   `json:"symbol"`
	Identity    string   `json:"identity"`
	Ticker      string   `json:"ticker,omitempty"`
	WKN         string   `json:"wkn,omitempty"`
	GFSymbol    string   `json:"ghostfolio_symbol,omitempty"`
	DataSource  string   `json:"data_source,omitempty"`
	Name        string   `json:"name,omitempty"`
	Why         []string `json:"why,omitempty"`
	Unsure      bool     `json:"unsure,omitempty"`
}

// ref returns how the chain knows the security: by its key, or by its symbol
// when it has none.
func (s chainSecurity) ref() secRef {
	if s.SecurityKey != "" {
		return secRef{key: s.SecurityKey}
	}
	return secRef{symbol: s.Symbol}
}

// finds returns, in one string, the identifiers that PP finds the security
// by again, of those that it got for it, by which two exports' are
// compared: its Ticker Symbol and WKN, before which PP takes no name
// (SecurityCache.java:66-159), or its Security Name when it has neither, as
// a security that PP finds by its name alone. A name that changes beside a
// ticker or a WKN, as the description seen first does when an older History
// file brings an earlier one, finds the same security in PP.
func (s chainSecurity) finds() string {
	if s.Ticker == "" && s.WKN == "" {
		return "name\x1f" + s.Name
	}
	return strings.Join([]string{s.Ticker, s.WKN}, "\x1f")
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
// design, K-3 to K-6 of the consolidation): each difference, and, with
// -since, each that the export it goes on from listed of an account that
// this one does not check, carried on as it was; each that the export it
// goes on from listed, of an account that this one checks, and that this one
// does not find (Gone); for PP each difference of the cash check, carried
// and gone likewise, and each account whose cash it could not check; and
// the labels of the accounts that neither check checked: of one with rows,
// for want of a snapshot, and, for PP, of one with a snapshot, for want of a
// PP account to check (ppHolds), as no export of the chain writes or names
// it. Cash, CashGone and CashNotChecked are nil for Ghostfolio, whose cash is
// the balances'.
type chainHoldings struct {
	Differences    []chainHolding        `json:"differences"`
	Gone           []chainHoldingGone    `json:"gone"`
	Cash           *[]chainCashHolding   `json:"cash,omitempty"`
	CashGone       *[]chainCashGone      `json:"cash_gone,omitempty"`
	CashNotChecked *[]chainCashUnchecked `json:"cash_not_checked,omitempty"`
	NotChecked     []string              `json:"not_checked"`
}

// A chainCashUnchecked is an account whose cash PP's cash check does not
// check (toolCashUnchecked): its label, the market date of its snapshot, and
// why: "margin", the snapshot is of a margin account with a margin or a
// short balance, whose cash Schwab's figures do not settle
// (docs/schwab-api.md); "no_cash", the snapshot holds no cash figure. The
// cash difference that the export it goes on from listed for the account
// is carried on (chainCashHolding.Carried).
type chainCashUnchecked struct {
	Account  string      `json:"account"`
	Snapshot nytime.Date `json:"snapshot"`
	Why      string      `json:"why"`
}

// A chainCashHolding is a difference of PP's cash check (toolCash): the
// account, the market date of the snapshot, its cash and what the records
// leave of it in PP, and the difference, the first less the second, which
// what you book by hand for the account's opening cash is to equal.
// LeftOut, Before and Carried are as a chainHolding's, of a row left out
// that moves cash. An account whose cash an export does not check and of
// which no difference is listed or carried has a carried entry with no
// figures, of the snapshot that it could not check (compareCash).
type chainCashHolding struct {
	Account      string      `json:"account"`
	Snapshot     nytime.Date `json:"snapshot"`
	SnapshotCash json.Number `json:"snapshot_cash,omitempty"`
	WrittenCash  json.Number `json:"written_cash,omitempty"`
	Difference   json.Number `json:"difference,omitempty"`
	LeftOut      bool        `json:"left_out,omitempty"`
	Before       bool        `json:"before,omitempty"`
	Carried      bool        `json:"carried,omitempty"`
}

// A chainHolding is a difference of the holdings check (toolHolding): the
// account and the security as the tool holds it, its shares signed, and the
// difference, the snapshot's shares less the written ones, which what you
// add by hand for the security is to equal (K-3 of the consolidation).
// SecurityKeys are, for a security that the tool holds under several of the
// store's keys, every one of them. PP is, for PP, the security's Ticker
// Symbol, WKN and Security Name as written, by which PP finds it, and
// Asset, for Ghostfolio, the asset that it holds the security under: what
// holds what you add by hand for it in the tool (key). LeftOut says that
// the export leaves out a row of the account that moves the security, which
// the snapshot holds: the difference holds what it moved. With -since,
// Before says that the export it goes on from listed the same difference
// under the same key as its own; Carried that this export could not check
// the account and copied the difference, as it was, from the export it goes
// on from, for the next export that checks the account (K-6). Unsure says
// that the security is written on its own, of a key under keys_unsure: a
// fact, which changes neither what to book for it nor whether it is before
// or gone (I-11).
type chainHolding struct {
	Account        string           `json:"account"`
	SecurityKey    string           `json:"security_key"`
	SecurityKeys   []string         `json:"security_keys,omitempty"`
	Symbol         string           `json:"symbol"`
	PP             *chainPPSecurity `json:"pp_security,omitempty"`
	Asset          *chainAsset      `json:"ghostfolio_asset,omitempty"`
	Snapshot       nytime.Date      `json:"snapshot"`
	SnapshotShares json.Number      `json:"snapshot_shares"`
	WrittenShares  json.Number      `json:"written_shares"`
	Difference     json.Number      `json:"difference"`
	LeftOut        bool             `json:"left_out,omitempty"`
	Unsure         bool             `json:"unsure,omitempty"`
	Before         bool             `json:"before,omitempty"`
	Carried        bool             `json:"carried,omitempty"`
}

// key returns what h is a difference of, by which two exports' differences
// are compared (K-6 of the consolidation): its account, and the security as
// the tool finds it, which holds what you add by hand for it there: for PP
// by its WKN, else its ticker, else its name (ppFinds), for Ghostfolio by
// its asset's data source and symbol. A security that the tool finds
// otherwise now, as after a rename without a CUSIP, a CUSIP learned or
// -manual, is another key: its difference is gone under the old one and
// listed under the new.
func (h chainHolding) key() string {
	return holdingKey(h.Account, h.PP, h.Asset)
}

// holdingKey returns the key (chainHolding.key) of a difference of the
// account labelled account of a security that PP finds by pp, or that
// Ghostfolio holds under asset.
func holdingKey(account string, pp *chainPPSecurity, asset *chainAsset) string {
	k := []string{account}
	if pp != nil {
		k = append(k, ppFinds(pp.Ticker, pp.WKN, pp.Name))
	}
	if asset != nil {
		k = append(k, asset.DataSource, asset.Symbol)
	}
	return strings.Join(k, "\x1f")
}

// A chainPPSecurity is a security as a PP export writes it: its Ticker
// Symbol, WKN and Security Name (ppIdentifiers).
type chainPPSecurity struct {
	Ticker string `json:"ticker,omitempty"`
	WKN    string `json:"wkn,omitempty"`
	Name   string `json:"name,omitempty"`
}

// ppSecurityOf returns security s as PP's records of it name it.
func ppSecurityOf(s *toolSecurity) *chainPPSecurity {
	ticker, wkn, name := ppIdentifiers(s)
	return &chainPPSecurity{Ticker: ticker, WKN: wkn, Name: name}
}

// shown returns how PP finds the security, as stderr names it: by its WKN,
// else its ticker, else its name (ppFinds), as "WKN 98389B100".
func (p *chainPPSecurity) shown() string {
	switch {
	case p == nil:
		return ""
	case p.WKN != "":
		return "WKN " + p.WKN
	case p.Ticker != "":
		return "ticker " + p.Ticker
	}
	return "name " + p.Name
}

// A chainAsset is a Ghostfolio asset: its data source and symbol, by which
// Ghostfolio ties an activity to it, and, for a MANUAL asset, its name, by
// which Ghostfolio shows it.
type chainAsset struct {
	DataSource string `json:"data_source"`
	Symbol     string `json:"symbol"`
	Name       string `json:"name,omitempty"`
}

// gfAssetOf returns the asset that Ghostfolio holds security s under.
func gfAssetOf(s *toolSecurity) *chainAsset {
	a := &chainAsset{DataSource: s.gfDataSource(), Symbol: s.gfSymbol()}
	if s.manual != "" {
		a.Name = s.gfName()
	}
	return a
}

// shown returns how Ghostfolio shows asset a: by its name when it is MANUAL,
// else by its symbol; "" for none.
func (a *chainAsset) shown() string {
	if a == nil {
		return ""
	}
	return cmp.Or(a.Name, a.Symbol)
}

// A chainHoldingGone is a difference of the holdings check that the export
// it goes on from listed, of an account that this one checks, and whose key
// this one does not list (K-6 of the consolidation): the records now leave
// of the security what the account's latest snapshot holds, or the tool
// finds the security otherwise now. It is as that export listed it, with
// its difference as Was: what you added by hand for it is to delete.
type chainHoldingGone struct {
	Account      string           `json:"account"`
	SecurityKey  string           `json:"security_key"`
	SecurityKeys []string         `json:"security_keys,omitempty"`
	Symbol       string           `json:"symbol"`
	PP           *chainPPSecurity `json:"pp_security,omitempty"`
	Asset        *chainAsset      `json:"ghostfolio_asset,omitempty"`
	Was          json.Number      `json:"was"`
}

// A chainCashGone is, for PP, an account whose cash the export it goes on
// from listed as differing from its latest snapshot's, by Was, the
// snapshot's cash less the records', or carried with no figures, as one
// that it could not check (Was ""), and which this one checks and finds
// alike: an opening deposit or withdrawal booked by hand for it is to
// delete.
type chainCashGone struct {
	Account string      `json:"account"`
	Was     json.Number `json:"was,omitempty"`
}

// A chainChanged is a row that the tool holds in another form than this
// export writes it (3.8 of the design): delete what it was in the tool
// before importing. Was is what the tool shows of it (chainAs), and Note,
// for PP, how the Note that PP holds of it begins, when that is not its own
// label and ref (chainGiven.Note).
type chainChanged struct {
	Ref     string `json:"ref"`
	Account string `json:"account"`
	Was     string `json:"was"`
	Note    string `json:"note,omitempty"`
	Now     string `json:"now"`
	// day is the row's day as the earlier export gave it, by which the list
	// is in order.
	day nytime.Date
}

// A chainGone is a row that the tool holds and that this export neither
// writes nor gives (3.8 of the design): delete it in the tool. Note is as a
// chainChanged's. Why says what became of it: uncounted with the store's
// counted_why, linked to the row that the store counts in its place, left
// out with its reason, combined with another row (its label and ref: a sale
// under its cash row's, or for PP the transfer that a paying side writes),
// or, for Ghostfolio, now cash, which its balances carry; else not in this
// export (notInExport).
type chainGone struct {
	Ref     string `json:"ref"`
	Account string `json:"account"`
	Was     string `json:"was"`
	Note    string `json:"note,omitempty"`
	Why     string `json:"why"`
}

// A chainSameAs is, for PP, a row gone under its ref and a row of its
// account under another, alike in everything PP keeps (a file row now
// linked to its API row), which this export gives (R-2 of the
// consolidation): PP holds the gone row's records for the new one, so
// nothing of the two is to delete, import or tick.
type chainSameAs struct {
	Account string `json:"account"`
	Gone    string `json:"gone"`
	New     string `json:"new"`
}

// A toolChain is the manifest of the export that a -since export goes on
// from, as parseChain reads it: that export's exported_at, the store it was
// made from (when the store was made, Q0), its format and layout, its
// chain's part, and the keys that it listed under keys_unsure.
type toolChain struct {
	ExportedAt string            `json:"exported_at"`
	Store      string            `json:"store"`
	Format     string            `json:"format"`
	Layout     int               `json:"layout"`
	KeysUnsure []toolUnsureEntry `json:"keys_unsure"`
	chainManifest
}

// chainHex is an fp: 16 lower-case hex digits.
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
			if g := c.Given[label][ref]; !isDate(g.Day) || !chainHex.MatchString(g.FP) {
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

// listed reports whether the export whose manifest is c listed u under
// keys_unsure alike: the same key, or the same symbol of rows without one,
// with the same securities that it may be, in their order; false for no
// export. What the store knows of each changes nothing: what you judge them
// by is listed again only when their securities change.
func (c *toolChain) listed(u ToolUnsure) bool {
	if c == nil {
		return false
	}
	return slices.ContainsFunc(c.KeysUnsure, func(e toolUnsureEntry) bool {
		return e.SecurityKey == u.Key && e.Symbol == u.Symbol && slices.EqualFunc(e.Securities, u.Securities, func(a toolUnsureSecurity, b ToolUnsureSecurity) bool {
			return a.SecurityKey == b.Key && a.Symbol == b.Symbol
		})
	})
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
// the paying account, which PP checks it by, and its receiving leg in its
// Offset Account, which an import adds without a check.
func ppKeys(e *toolEvent) (keys []string, checked int) {
	r := ppRecordOf(e)
	security := ppFinds(r.ticker, r.wkn, r.name)
	side := func(kind, owner, typ, shares, security string) string {
		return chainHash([]string{kind, owner, ppClass(kind, typ), r.date, ppCurrency, r.value, shares, security})
	}
	switch e.kind {
	case evTransferOut:
		return []string{side(ppAccount, r.cash, "Transfer (Outbound)", "", ""), side(ppAccount, r.offset, "Transfer (Inbound)", "", "")}, 1
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
	// the rows gone that a row given under a new ref is the same as, in the
	// order of their accounts, days and refs.
	changed []chainChanged
	gone    []chainGone
	sameAs  []chainSameAs
	// late counts the rows written that the earlier export did not give and
	// that are dated on or before its last day (R-4 of the consolidation):
	// they came late, were decided, paired or took a new ref since.
	late int
	// given is the next given: what the tool holds once this export is
	// imported.
	given map[string]map[string]chainGiven
	// leftOut, holdings and unchecked are the rows left out, the differences
	// of the holdings check, with those that the earlier export listed of
	// the accounts that it did not check, carried on, and those accounts;
	// notHeld are, for PP, those of unchecked that have a snapshot and that
	// PP holds nothing of (ppHolds), the others having rows and no snapshot
	// to check against.
	leftOut   []chainLeftOut
	holdings  []chainHolding
	unchecked []toolAccount
	notHeld   []toolAccount
	// holdingsGone are the differences that the earlier export listed, of
	// the accounts that the holdings check checks, and that it no longer
	// finds; cash, cashGone and cashUnchecked are, for PP, the cash check's
	// differences, carried ones among them, those gone, and the accounts
	// whose cash it could not check, each as the manifest lists it.
	holdingsGone  []chainHoldingGone
	cash          []chainCashHolding
	cashGone      []chainCashGone
	cashUnchecked []chainCashUnchecked
	// unimported says, for Ghostfolio, that this export writes no
	// activity, so that the import dialog cannot import it (K-6 of the
	// consolidation); goneCarried are then the differences that the
	// earlier export listed and that this one finds gone, as listed. Its
	// manifest carries both, and its own differences, for the next export
	// to list again, never as before, or as gone.
	unimported  bool
	goneCarried []chainHolding
	// securities are every security that the chain has written, with its
	// identifiers as written last; symbolsChanged, for PP, those of this
	// export that PP may hold otherwise than written or that may no longer
	// trade (symbols_changed), one for each of PP's securities.
	securities     []chainSecurity
	symbolsChanged []symbolsChange
	// kept holds the entries of the rows given: the earlier export's, or for
	// a row the same as a gone one, the gone one's.
	kept map[rowKey]chainGiven
	// start is, for PP, what PP holds before this export's files (R-6 of the
	// consolidation): the keys (ppKeys) of each record of the rows given,
	// each once for each record.
	start []string
}

// A chainOld is an entry of the earlier export's given: the row it is of,
// by its account's label and its ref, and what the tool holds of it.
type chainOld struct {
	label, ref string
	given      chainGiven
}

// compareChain compares the export of in, whose mapping gave events and
// outcomes (toolEvents), with the export since, whose manifest it goes on
// from, nil for a full export (3.8 of the design, R-1 to R-4 of the
// consolidation). A row that since gave with the fp it has now is given, not
// written; one it gave with another is written and changed; one it gave that
// this export neither writes nor gives is gone, with why; a row that it did
// not give is written, whatever its day. For PP a gone row whose fp equals
// that of a row written now in its account under another ref is the same as
// it: PP holds the gone row's records for the new one, which is given, so
// that nothing of the two is to delete, import or tick. The rows left out
// and the differences from the snapshots are marked before when since
// listed them alike, and since's differences of an account that this export
// cannot check are carried on.
func compareChain(in *toolInput, events []toolEvent, outcomes []toolOutcome, since *toolChain) *chainResult {
	c := &chainResult{
		in: in, ix: newToolIndex(in), since: since, outcomes: slices.Clone(outcomes),
		given: map[string]map[string]chainGiven{}, kept: map[rowKey]chainGiven{},
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
					Ref: r.ref, Account: prev.label, Was: prev.given.As, Note: prev.given.Note, Now: chainAs(in.tool, records[i]), day: prev.given.Day,
				})
			}
		case had:
			gone = append(gone, prev)
			w := "left out: " + o.reason
			switch o.outcome {
			case outcomeInBalances:
				w = "now cash, in the balances"
			case outcomeCombined:
				w = "combined with " + o.withLabel() + " " + o.with
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
		why[[2]string{o.label, o.ref}] = notInExport
	}
	slices.SortFunc(gone, func(a, b chainOld) int {
		return cmp.Or(cmp.Compare(c.labelOrder(a.label), c.labelOrder(b.label)), cmp.Compare(a.given.Day, b.given.Day), cmp.Compare(a.ref, b.ref))
	})

	// A gone row is the same as a row written under a new ref when the two
	// have one fp, each pair once, in order (R-2 of the consolidation): for
	// PP they are then of one account, whose names the fp holds, and alike in
	// all that PP keeps, so PP holds the gone row's records for the new row,
	// which is given. Its entry is the gone row's, whose note says how the
	// Note that PP holds of it begins: the gone row's own label and ref, or
	// what that row's entry had in their place. A Ghostfolio fp holds the
	// row's ref, in the comment, so no row there is the same as another.
	for _, o := range gone {
		j := slices.IndexFunc(news, func(f fresh) bool { return f.fp == o.given.FP && c.outcomes[f.i].outcome == outcomeWritten })
		if j < 0 {
			c.gone = append(c.gone, chainGone{Ref: o.ref, Account: o.label, Was: o.given.As, Note: o.given.Note, Why: why[[2]string{o.label, o.ref}]})
			continue
		}
		r := &in.rows[news[j].i]
		c.outcomes[news[j].i].outcome = outcomeGiven
		g := o.given
		if g.Note = cmp.Or(g.Note, o.label+" "+o.ref); g.Note == o.label+" "+r.ref {
			// PP holds the records under the new row's own Note, as when a
			// row comes back in place of the one that took its place.
			g.Note = ""
		}
		c.kept[rowKey{r.account, r.ref}] = g
		c.sameAs = append(c.sameAs, chainSameAs{Account: o.label, Gone: o.ref, New: r.ref})
	}
	for _, f := range news {
		if since != nil && c.outcomes[f.i].outcome == outcomeWritten && in.rows[f.i].day <= since.Chain.To {
			c.late++
		}
	}
	slices.SortStableFunc(c.changed, func(a, b chainChanged) int {
		return cmp.Or(cmp.Compare(c.labelOrder(a.Account), c.labelOrder(b.Account)), cmp.Compare(a.day, b.day), cmp.Compare(a.Ref, b.Ref))
	})

	for i := range events {
		switch c.outcomes[events[i].row].outcome {
		case outcomeWritten:
			c.written = append(c.written, events[i])
		case outcomeGiven:
			if in.tool == toolPP {
				keys, _ := ppKeys(&events[i])
				c.start = append(c.start, keys...)
			}
		}
	}
	c.nextGiven(records)
	c.compareLeftOut()
	shares, cash := c.leftOutMoves()
	c.compareHoldings(events, shares)
	c.compareCash(events, cash)
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

// notInExport is why a row went that the earlier export gave and that
// this export neither selects nor reads as uncounted, or whose account the
// store does not hold: the store no longer holds it, or a revision dates
// it after -to, and an export up to its new day writes it again. The
// export reads no row after -to to tell which: the tool's row is to delete
// either way.
const notInExport = "not in this export: no longer in the store, or dated after -to"

// whyGone returns why the row k, which the earlier export gave and which
// this export does not select, went: uncounted, with the store's
// counted_why, or linked to the rows that the store counts in its place
// (Q10); else notInExport.
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
	return notInExport
}

// nextGiven makes the next given (R-7 of the consolidation): the entry of
// each row given as it was kept (kept), and a new entry of each row written,
// whose note is, for a PP transfer, the head of the Note that it is written
// with (ppTransferHead). A transfer has one entry, under its paying side,
// whatever receiving side it pairs with since: its fp holds no receiving
// side's ref.
func (c *chainResult) nextGiven(records map[int][]*toolEvent) {
	for i, o := range c.outcomes {
		r := &c.in.rows[i]
		label := c.ix.account(r.account).label()
		var g chainGiven
		switch o.outcome {
		case outcomeGiven:
			g = c.kept[rowKey{r.account, r.ref}]
		case outcomeWritten:
			g = chainGiven{Day: r.day, As: chainAs(c.in.tool, records[i]), FP: fingerprint(c.in.tool, records[i]), With: o.with}
			if c.in.tool == toolPP && records[i][0].kind == evTransferOut {
				g.Note = ppTransferHead(records[i][0])
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

// leftOutMoves returns what the rows that this export leaves out move of
// what the snapshots hold (K-3 of the consolidation): by key
// (chainHolding.key), the securities, as the tool finds them, that a row
// left out moves by its quantity or a leg's; and, by label, the accounts
// whose cash a row left out moves by its amount; each of a row that its
// account's snapshot holds (checkedSnapshot, inSnapshot). A difference of
// one holds what the row moved, which you book by hand or not, as its
// left_out entry advises.
func (c *chainResult) leftOutMoves() (shares, cash map[string]bool) {
	shares, cash = map[string]bool{}, map[string]bool{}
	legs := map[rowKey][]toolLeg{}
	for _, l := range c.in.legs {
		legs[rowKey{l.account, l.ref}] = append(legs[rowKey{l.account, l.ref}], l)
	}
	for i, o := range c.outcomes {
		r := &c.in.rows[i]
		snap, ok := checkedSnapshot(c.in, r.account)
		if o.outcome != outcomeLeftOut || !ok || !inSnapshot(r, snap) {
			continue
		}
		label := o.account.label()
		if r.amount.Valid && r.amount.Int64 != 0 {
			cash[label] = true
		}
		for _, m := range append([]toolLeg{{symbol: r.symbol, securityKey: r.securityKey, quantity: r.quantity}}, legs[rowKey{r.account, r.ref}]...) {
			if s := c.ix.security(m.securityKey, m.symbol); s != nil && m.quantity.Valid && m.quantity.Int64 != 0 {
				shares[holdingKey(label, c.ppSecurity(s), c.gfAsset(s))] = true
			}
		}
	}
	return shares, cash
}

// ppSecurity returns, for PP, how PP's records name security s
// (ppSecurityOf); nil for Ghostfolio.
func (c *chainResult) ppSecurity(s *toolSecurity) *chainPPSecurity {
	if c.in.tool != toolPP {
		return nil
	}
	return ppSecurityOf(s)
}

// gfAsset returns, for Ghostfolio, the asset that it holds security s under
// (gfAssetOf); nil for PP.
func (c *chainResult) gfAsset(s *toolSecurity) *chainAsset {
	if c.in.tool != toolGF {
		return nil
	}
	return gfAssetOf(s)
}

// A chainEntry is a difference from the snapshots as compareEntries compares
// it: its account's label, its key (chainHolding.key, or the label for
// cash), its value, and whether the export that listed it carried it.
type chainEntry struct {
	account, key string
	value        *big.Rat
	carried      bool
}

// compareEntries compares the differences of this export, now, with those
// that the export that it goes on from listed, then (K-6 of the
// consolidation), for the holdings check or the cash check, whose accounts
// that this export checks are checked. It returns, for each of now, whether
// it is before: one of then that is not carried has its key and value, as
// what you add by hand for a difference is to equal it, whatever you did
// before; and, of then, those to carry on, of an account that this export
// does not check, and those gone, of an account that it checks, whose key it
// does not list. Any other of then is listed again in now, with its value.
func compareEntries(now, then []chainEntry, checked map[string]bool) (before []bool, carry, gone []int) {
	own, found := map[string]*big.Rat{}, map[string]bool{}
	for _, e := range then {
		if !e.carried {
			own[e.key] = e.value
		}
	}
	for _, e := range now {
		found[e.key] = true
		v, ok := own[e.key]
		before = append(before, ok && v.Cmp(e.value) == 0)
	}
	for i, e := range then {
		switch {
		case !checked[e.account]:
			carry = append(carry, i)
		case !found[e.key]:
			gone = append(gone, i)
		}
	}
	return before, carry, gone
}

// compareHoldings runs the holdings check over the records of every row
// written or given, events (4.15 of the design), and compares its
// differences with those that the earlier export listed (compareEntries):
// what you add by hand for a security, as an opening position, changes
// nothing in the store, so its difference comes back in every export, and
// the rows that a later export writes for it take it away. Each difference
// is of the security as the tool finds it, which holds what you added: for
// Ghostfolio its asset, which does not follow a rename, a delisting or
// -manual, for PP its identifiers. A difference of a security that a row
// left out moves, by shares (moves, leftOutMoves), is marked left_out. For
// PP, an account that PP holds nothing of (ppHolds), as one with snapshots
// and no row, is not checked: no account of PP's would hold what you added
// for it by hand. The earlier export's differences of an account that this
// one does not check, as one whose snapshot a store restored from an older
// backup lacks, are carried on as they were, for the next export that
// checks it. A Ghostfolio export that writes no activity, which the import
// dialog cannot import, carries its own differences and those that it finds
// gone (unimported).
func (c *chainResult) compareHoldings(events []toolEvent, moves map[string]bool) {
	diffs, unchecked := holdingsCheck(c.in, events)
	holds := map[string]bool{}
	if c.in.tool == toolPP {
		// An account that PP holds nothing of has nothing in PP to check:
		// it is not checked.
		holds = c.ppHolds()
		diffs = slices.DeleteFunc(diffs, func(d toolHolding) bool { return !holds[d.account.label()] })
	}
	checked := map[string]bool{}
	for _, id := range snapshotAccounts(c.in) {
		a := c.ix.account(id)
		_, ok := checkedSnapshot(c.in, id)
		switch {
		case ok && c.in.tool == toolPP && !holds[a.label()]:
			c.notHeld = append(c.notHeld, a)
		case ok:
			checked[a.label()] = true
		}
	}
	unchecked = append(unchecked, c.notHeld...)
	slices.SortFunc(unchecked, func(a, b toolAccount) int { return cmp.Compare(a.id, b.id) })
	c.unchecked = unchecked
	var now, then []chainEntry
	for _, d := range diffs {
		h := chainHolding{
			Account: d.account.label(), SecurityKey: d.key, SecurityKeys: d.keys, Symbol: d.symbol,
			PP: c.ppSecurity(d.security), Asset: c.gfAsset(d.security), Snapshot: d.snapshot,
			SnapshotShares: json.Number(formatMicros(d.snapshotShares)), WrittenShares: json.Number(formatMicros(d.writtenShares)),
			Difference: json.Number(formatMicros(d.snapshotShares - d.writtenShares)),
		}
		h.LeftOut, h.Unsure = moves[h.key()], d.security.unsure
		c.holdings = append(c.holdings, h)
		now = append(now, chainEntry{h.Account, h.key(), chainRat(h.Difference), false})
	}
	var listed []chainHolding
	if c.since != nil {
		listed = c.since.Holdings.Differences
	}
	for _, l := range listed {
		then = append(then, chainEntry{l.Account, l.key(), chainRat(l.Difference), l.Carried})
	}
	before, carry, gone := compareEntries(now, then, checked)
	for i, b := range before {
		c.holdings[i].Before = b
	}
	for _, i := range carry {
		l := listed[i]
		l.Before, l.Carried = false, true
		c.holdings = append(c.holdings, l)
	}
	slices.SortStableFunc(c.holdings, func(a, b chainHolding) int { return cmp.Compare(c.labelOrder(a.Account), c.labelOrder(b.Account)) })
	for _, i := range gone {
		l := listed[i]
		c.holdingsGone = append(c.holdingsGone, chainHoldingGone{
			Account: l.Account, SecurityKey: l.SecurityKey, SecurityKeys: l.SecurityKeys, Symbol: l.Symbol, PP: l.PP, Asset: l.Asset, Was: l.Difference,
		})
	}
	// A Ghostfolio export that writes no activity is one that the import
	// dialog cannot import: stderr lists its differences all the same, for
	// a POST of the file, and its manifest carries them, with those that it
	// finds gone, as the next export goes on from an import of the one
	// before it (chainJSON).
	if c.in.tool == toolGF && len(c.written) == 0 {
		c.unimported = true
		for _, i := range gone {
			l := listed[i]
			l.Before, l.Carried = false, true
			c.goneCarried = append(c.goneCarried, l)
		}
	}
}

// compareCash runs PP's cash check over the records of every row written
// or given, events, and compares each account's difference, the snapshot's
// cash less the records', with those that the earlier export listed
// (compareEntries), as compareHoldings does, by account: what you book by
// hand for an account's opening cash is to equal its difference, marked
// left_out when a row left out moves the account's cash (moves,
// leftOutMoves). An account whose cash it does not check, a margin account
// in use or one whose snapshot holds no cash figure, is listed apart with
// why, and so is not checked, nor is one that the holdings check does not
// check: the earlier export's difference of either is carried on. An account
// that PP holds nothing of (ppHolds) is not checked.
func (c *chainResult) compareCash(events []toolEvent, moves map[string]bool) {
	if c.in.tool != toolPP {
		return
	}
	// An account that PP holds nothing of has no cash in PP to check:
	// compareHoldings lists it as not checked.
	holds := c.ppHolds()
	diffs, unchecked := cashCheck(c.in, events)
	diffs = slices.DeleteFunc(diffs, func(d toolCash) bool { return !holds[d.account.label()] })
	unchecked = slices.DeleteFunc(unchecked, func(u toolCashUnchecked) bool { return !holds[u.account.label()] })
	checked := map[string]bool{}
	for _, id := range snapshotAccounts(c.in) {
		if _, ok := checkedSnapshot(c.in, id); ok && holds[c.ix.account(id).label()] {
			checked[c.ix.account(id).label()] = true
		}
	}
	for _, u := range unchecked {
		c.cashUnchecked = append(c.cashUnchecked, chainCashUnchecked{Account: u.account.label(), Snapshot: u.snapshot, Why: u.why})
		delete(checked, u.account.label())
	}
	var now, then []chainEntry
	for _, d := range diffs {
		h := chainCashHolding{
			Account: d.account.label(), Snapshot: d.snapshot,
			SnapshotCash: json.Number(formatCents(d.snapshotCash)), WrittenCash: json.Number(formatCents(d.writtenCash)),
			Difference: json.Number(formatCents(d.snapshotCash - d.writtenCash)), LeftOut: moves[d.account.label()],
		}
		c.cash = append(c.cash, h)
		now = append(now, chainEntry{h.Account, h.Account, chainRat(h.Difference), false})
	}
	var listed []chainCashHolding
	if c.since != nil && c.since.Holdings.Cash != nil {
		listed = *c.since.Holdings.Cash
	}
	for _, l := range listed {
		then = append(then, chainEntry{l.Account, l.Account, chainRat(l.Difference), l.Carried})
	}
	before, carry, gone := compareEntries(now, then, checked)
	for i, b := range before {
		c.cash[i].Before = b
	}
	for _, i := range carry {
		l := listed[i]
		l.Before, l.Carried = false, true
		c.cash = append(c.cash, l)
	}
	// An account whose cash this export does not check, and of which no
	// difference is listed or carried, is carried with no figures (1b): what
	// you booked after comparing Schwab's cash with PP's by hand, as its
	// line says, meets the next export that checks the account, which lists
	// its difference whatever it is, and as gone at none.
	for _, u := range c.cashUnchecked {
		if !slices.ContainsFunc(c.cash, func(h chainCashHolding) bool { return h.Account == u.Account }) {
			c.cash = append(c.cash, chainCashHolding{Account: u.Account, Snapshot: u.Snapshot, Carried: true})
		}
	}
	slices.SortStableFunc(c.cash, func(a, b chainCashHolding) int { return cmp.Compare(c.labelOrder(a.Account), c.labelOrder(b.Account)) })
	for _, i := range gone {
		c.cashGone = append(c.cashGone, chainCashGone{Account: listed[i].Account, Was: listed[i].Difference})
	}
}

// ppHolds returns, by label, the accounts that PP holds once this export is
// imported: those of the manifest (accounts), which this export writes or
// names as a transfer's Offset Account, or an earlier export of its chain
// wrote or named, and which you make in PP. PP holds nothing of any other
// account that the export selects, as one with snapshots and no row, which
// the export makes no PP account for: its positions and its cash are no
// PP account's to check, and stderr names no account to add them to.
func (c *chainResult) ppHolds() map[string]bool {
	holds := map[string]bool{}
	for _, a := range c.accounts() {
		holds[a.Label] = true
	}
	return holds
}

// A symbolsChange is a security of PP's symbols_changed (R-10 of the
// consolidation), one of PP's securities: the store's securities of it that
// are named, in their order; why (renamed, no_ticker, named_by_cusip,
// changed) and because
// (delisted, reused or undecided, for no_ticker); for reused, reusedBy, the
// key of the security that keeps the ticker (toolSecurity.reusedBy); new,
// with -since, when no export of the chain wrote any of the store's
// securities of it, so that PP does not hold it; and every symbol that the
// named ones have had, as each source named it (namesOf).
type symbolsChange struct {
	members  []*toolSecurity
	why      []string
	because  string
	reusedBy string
	new      bool
	names    []sourcedSymbol
}

// compareSecurities makes the chain's securities, the earlier export's with
// those that this export writes (listSecurities) as it writes them, and, for
// PP, which keeps the ticker and the feed of a security that it holds, the
// securities that PP may hold otherwise than written, or that may no longer
// trade (symbols_changed): without -since, those with a why of their own
// (symbolWhys); with -since, those that this export writes, and those that
// the chain wrote and that a row given of events names or that an account
// that the export selects holds, whose identifiers that PP finds them by
// (finds) differ from the chain's (changed), as PP finds a security whose
// CUSIP the store knows by its WKN, so that its rows stay given when it is
// renamed, and keeps its old ticker, and that ticker's feed, until you give
// it the new one (4.14 of the design); and those with a why that the
// chain's entry lacks, which is every why of one that the chain never wrote
// (new). A PP Security Name that changes beside a ticker or a WKN changes
// nothing that PP finds the security by, and the chain's entry takes it all
// the same. One that the chain wrote on its own (unsure) and that is no
// longer unsure is never named: you delete PP's security of it once empty.
// The store's securities that PP takes for one (mergeListed) are one
// security, as PP holds them. Ghostfolio's chain securities are kept for
// the record and compared with nothing: it holds a security that is written
// under another symbol, data source or name as another asset, whose rows the
// export lists as changed by their fp.
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
	changes, had := map[secRef]symbolsChange{}, map[secRef]bool{}
	for _, l := range listed {
		s := l.security
		w := chainSecurity{SecurityKey: s.key, Symbol: s.symbol, Identity: s.identity(), Unsure: s.unsure}
		if c.in.tool == toolGF {
			w.GFSymbol, w.DataSource = s.gfSymbol(), s.gfDataSource()
			if s.manual != "" {
				w.Name = s.gfName()
			}
			byRef[w.ref()] = w
			continue
		}
		w.Ticker, w.WKN, w.Name = ppIdentifiers(s)
		why, because := s.symbolWhys()
		w.Why = why
		prev, ok := byRef[w.ref()]
		had[s.ref()] = ok
		byRef[w.ref()] = w
		if ok && prev.Unsure && !s.unsure {
			// Written on its own before, and joined since (R-10): PP's
			// security of it ends empty, its rows listed as changed and
			// written under the security that it joined, whose identifiers
			// PP would hold twice if you gave them to this one.
			continue
		}
		named := slices.ContainsFunc(why, func(y string) bool { return !slices.Contains(prev.Why, y) })
		if ok && prev.finds() != w.finds() {
			why, named = append(slices.Clone(why), whyChanged), true
		}
		if named {
			changes[s.ref()] = symbolsChange{why: why, because: because, reusedBy: s.reusedBy}
		}
	}
	if c.in.tool == toolPP {
		for _, l := range mergeListed(toolPP, listed) {
			var ch symbolsChange
			for _, s := range l.members {
				if m, ok := changes[s.ref()]; ok {
					ch.members, ch.why = append(ch.members, s), append(ch.why, m.why...)
					ch.because, ch.reusedBy = cmp.Or(ch.because, m.because), cmp.Or(ch.reusedBy, m.reusedBy)
				}
			}
			if len(ch.members) == 0 {
				continue
			}
			// The whys of the store's securities of one of PP's, each once,
			// in their order; new when PP holds none of them.
			ch.why = slices.DeleteFunc([]string{whyRenamed, whyNoTicker, whyNamedByCUSIP, whyChanged}, func(y string) bool { return !slices.Contains(ch.why, y) })
			ch.new = c.since != nil && !slices.ContainsFunc(l.members, func(s *toolSecurity) bool { return had[s.ref()] })
			ch.names = c.ix.namesOf(ch.members)
			c.symbolsChanged = append(c.symbolsChanged, ch)
		}
	}
	c.securities = slices.SortedFunc(maps.Values(byRef), func(a, b chainSecurity) int {
		return cmp.Or(cmp.Compare(a.SecurityKey, b.SecurityKey), cmp.Compare(a.Symbol, b.Symbol))
	})
}

// A ppMark is a record of a PP export that PP's review page marks as
// existing (ppTicks), which PP imports only when you tick it: the file and
// its row (from 1, below the header), and the record. Each is to tick, as
// the record is new to PP all the same.
type ppMark struct {
	file   string
	line   int
	record *toolEvent
}

// A chainTick is a record of the manifest's tick (8.1 of the design): a row
// of a file that PP will mark as existing although it is new, which you tick.
type chainTick struct {
	File    string `json:"file"`
	Ref     string `json:"ref"`
	Account string `json:"account"`
	As      string `json:"as"`
}

// ppTickList returns the marks, each to tick, as the manifest's tick lists
// them.
func ppTickList(marks []ppMark) []chainTick {
	out := []chainTick{}
	for _, m := range marks {
		out = append(out, chainTick{File: m.file, Ref: m.record.ref, Account: m.record.account.label(), As: chainAs(toolPP, []*toolEvent{m.record})})
	}
	return out
}

// ppTicks replays PP's duplicate rule over the records of files, a PP
// export's files in the order to import them (ppFiles), from what PP holds
// before them (3.8 and 5.9 of the design, R-6 of the consolidation): the
// records of the rows that the export gives (start), whose keys are those
// of the records that the chain wrote of them, since a given row's fp is
// the one written and holds every field that the keys hash. PP checks a
// file's records against what its accounts hold before the file, never
// against each other, by the keys that it checks a new record by (ppKeys),
// and then imports each that it does not mark and each that is ticked,
// every side of it. It returns each record that PP's review page will mark
// as existing, each to tick: no record of the export is one that PP holds,
// as the export gives, not writes, a row that PP holds under another ref
// (same_as), and writes a transfer once.
func ppTicks(files []ppFile, c *chainResult) []ppMark {
	held := map[string]bool{}
	for _, k := range c.start {
		held[k] = true
	}
	var marks []ppMark
	for _, f := range files {
		var adds []string
		for j, e := range f.records {
			keys, checked := ppKeys(e)
			if slices.ContainsFunc(keys[:checked], func(k string) bool { return held[k] }) {
				marks = append(marks, ppMark{file: f.name, line: j + 1, record: e})
			}
			adds = append(adds, keys...)
		}
		for _, k := range adds {
			held[k] = true
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
	if c.unimported {
		// An export that the import dialog cannot import carries every
		// difference that it lists or finds gone: the next export lists
		// each again, never as before, or as gone.
		for i := range m.Holdings.Differences {
			m.Holdings.Differences[i].Before, m.Holdings.Differences[i].Carried = false, true
		}
		m.Holdings.Differences = append(m.Holdings.Differences, c.goneCarried...)
		slices.SortStableFunc(m.Holdings.Differences, func(a, b chainHolding) int { return cmp.Compare(c.labelOrder(a.Account), c.labelOrder(b.Account)) })
	}
	if m.Holdings.Differences == nil {
		m.Holdings.Differences = []chainHolding{}
	}
	m.Holdings.Gone = append([]chainHoldingGone{}, c.holdingsGone...)
	if c.in.tool == toolPP {
		cash, gone, notChecked := append([]chainCashHolding{}, c.cash...), append([]chainCashGone{}, c.cashGone...), append([]chainCashUnchecked{}, c.cashUnchecked...)
		m.Holdings.Cash, m.Holdings.CashGone, m.Holdings.CashNotChecked = &cash, &gone, &notChecked
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
		if e.kind == evTransferOut {
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
