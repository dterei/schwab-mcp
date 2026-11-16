package history

import (
	"bytes"
	"cmp"
	"encoding/csv"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The tool exports made from a store (11.3 of the design): TestToolExport
// exports the tool exports' store (newToolStore) for both tools, in full,
// for one account and in a chain, and reads it as the exports read it
// (checkToolRead and the checks after it), so that the package builds the
// store once. Every value it holds is made up.

// toolExportTo exports the tool exports' store as o asks (ExportTool) and
// writes it to dir, a folder that WriteDir makes, as a step of a chain whose
// files pass the tool's checker (chainStepFrom).
func toolExportTo(t *testing.T, ts *toolStore, o ToolOptions, dir string) *chainStep {
	t.Helper()
	e, err := ts.s.ExportTool(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	return chainStepFrom(t, e, dir)
}

// checkToolSummary requires that what the export e says of itself, which
// stderr prints, agree with its manifest, which the user reads, read back
// from its bytes: the selection and the chain; the files and the accounts;
// the counts of rows written, given, combined and in the balances, and of
// what was left out, with a caveat, flagged, waiting, holding other shares
// than the snapshot, or under another symbol, those listed by the export it
// goes on from aside, and for PP the accounts whose cash is not their
// latest snapshot's; what changed, went and is to tick; the -manual
// symbols that nothing uses; going on, the redo's selection, the chain's
// account and, for Ghostfolio, its -manual symbols; for PP, the securities
// and prices of securities.csv; and for Ghostfolio, the profiles, the
// symbols for Yahoo, and the cash rows that no balance carries yet.
func checkToolSummary(t *testing.T, e *ToolExport) {
	t.Helper()
	var m struct {
		Filters struct {
			Account *string     `json:"account"`
			To      nytime.Date `json:"to"`
			Since   *string     `json:"since"`
		} `json:"filters"`
		Chain struct {
			Account *string `json:"account"`
			Manual  []struct {
				Symbol string `json:"symbol"`
				Unused bool   `json:"unused"`
			} `json:"manual"`
		} `json:"chain"`
		Files []struct {
			File, Kind, Configuration, Account string
			Rows                               int
			Tick                               *int
		} `json:"files"`
		Accounts []struct {
			Label, Account, Nickname, Name string
			Exported, New                  bool
			LastDay                        *nytime.Date `json:"last_day"`
			GhostfolioID                   string       `json:"ghostfolio_id"`
			Balances                       *struct {
				Count       int
				First, Last *nytime.Date
				Why         string
			}
			Cash *struct{ Value json.Number }
		} `json:"accounts"`
		Written struct{ Rows int }               `json:"written"`
		Given   map[string]map[string]chainGiven `json:"given"`
		Changed []chainChanged                   `json:"changed"`
		Gone    []chainGone                      `json:"gone"`
		Tick    *[]chainTick                     `json:"tick"`
		LeftOut []struct {
			Account, Reason string
			Before          bool
		} `json:"left_out"`
		Caveats []struct {
			Ref, Account, Caveat string
		} `json:"caveats"`
		Combined   []struct{ Ref string } `json:"combined"`
		InBalances *[]struct{ Rows int }  `json:"in_balances"`
		Flagged    []struct{ Ref string } `json:"flagged"`
		Waiting    []struct{ Ref string } `json:"waiting"`
		Holdings   struct {
			Differences    []chainHolding
			Gone           *[]chainHoldingGone
			Cash           *[]chainCashHolding
			CashGone       *[]chainCashGone      `json:"cash_gone"`
			CashNotChecked *[]chainCashUnchecked `json:"cash_not_checked"`
			NotChecked     []string              `json:"not_checked"`
		} `json:"holdings"`
		Ghostfolio *struct {
			Yahoo     []string
			YahooIdle []struct{ Symbol string } `json:"yahoo_idle"`
			Profiles  []struct{ Status string }
		} `json:"ghostfolio"`
		SymbolsChanged *[]struct{ SecurityKey string } `json:"symbols_changed"`
		KeysUnsure     []toolUnsureEntry               `json:"keys_unsure"`
		Coverage       []ExportCoverage                `json:"coverage"`
	}
	if err := json.Unmarshal(e.manifest, &m); err != nil {
		t.Fatalf("the manifest does not read back: %v", err)
	}
	check := func(what string, got, want any) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the export's %s is %v, its manifest's %v", what, got, want)
		}
	}
	check("account", e.Account, deref(m.Filters.Account))
	check("last day", e.To, m.Filters.To)
	check("export it goes on from", e.Since, deref(m.Filters.Since))
	var redoAccount string
	var redoManual []string
	if e.Since != "" {
		redoAccount = deref(m.Chain.Account)
		for _, s := range m.Chain.Manual {
			if e.Tool == toolGF {
				redoManual = append(redoManual, s.Symbol)
			}
		}
	}
	check("redo's account", e.RedoAccount, redoAccount)
	check("redo's -manual", e.RedoManual, redoManual)
	check("coverage", orNil(e.Coverage), orNil(m.Coverage))

	var files []ToolFile
	for _, f := range m.Files {
		files = append(files, ToolFile{Name: f.File, Kind: f.Kind, Configuration: f.Configuration, Account: f.Account, Rows: f.Rows, Tick: deref(f.Tick)})
	}
	got := slices.Clone(e.Files)
	for i := range got {
		got[i].data = nil
	}
	check("files", got, files)
	var accounts []ToolAccount
	for _, a := range m.Accounts {
		ta := ToolAccount{
			Label: a.Label, Account: a.Account, Nickname: a.Nickname, Name: a.Name, Exported: a.Exported, New: a.New,
			LastDay: deref(a.LastDay), GhostfolioID: a.GhostfolioID,
		}
		if b := a.Balances; b != nil {
			ta.Balances, ta.FirstBalance, ta.LastBalance, ta.NoBalances = b.Count, deref(b.First), deref(b.Last), b.Why
		}
		if a.Cash != nil {
			ta.Cash = a.Cash.Value.String()
		}
		accounts = append(accounts, ta)
	}
	check("accounts", e.Accounts, accounts)

	entries := 0
	for _, refs := range m.Given {
		entries += len(refs)
	}
	check("rows written", e.Written, m.Written.Rows)
	check("rows given", e.Given, entries-m.Written.Rows)
	check("rows combined", e.Combined, len(m.Combined))
	inBalances := 0
	if m.InBalances != nil {
		for _, a := range *m.InBalances {
			inBalances += a.Rows
		}
	}
	check("rows in the balances", e.InBalances, inBalances)
	var changed, gone []ToolChange
	for _, c := range m.Changed {
		changed = append(changed, ToolChange{Ref: c.Ref, Account: c.Account, Was: c.Was, Note: c.Note, Now: c.Now})
	}
	for _, g := range m.Gone {
		gone = append(gone, ToolChange{Ref: g.Ref, Account: g.Account, Was: g.Was, Note: g.Note, Why: g.Why})
	}
	check("rows changed", e.Changed, changed)
	check("rows gone", e.Gone, gone)
	var ticks int
	var tick []ToolTick
	if m.Tick != nil {
		for _, k := range *m.Tick {
			tick = append(tick, ToolTick(k))
		}
	}
	for _, f := range files {
		ticks += f.Tick
	}
	check("rows to tick", e.Tick, tick)
	check("files' rows to tick", ticks, len(tick))

	byCode := func(codes []string, count map[string]int) []ToolCount {
		var out []ToolCount
		for _, c := range codes {
			if count[c] > 0 {
				out = append(out, ToolCount{c, count[c]})
			}
		}
		return out
	}
	reasons, caveats, gfCash := map[string]int{}, map[string]int{}, map[string]int{}
	for _, l := range m.LeftOut {
		if !l.Before {
			reasons[l.Reason]++
			if l.Reason == "gf_cash" {
				gfCash[l.Account]++
			}
		}
	}
	caveatRows := map[string]bool{}
	for _, c := range m.Caveats {
		caveats[c.Caveat]++
		caveatRows[c.Account+" "+c.Ref] = true
	}
	var reasonCodes, caveatCodes []string
	for _, r := range toolReasons {
		reasonCodes = append(reasonCodes, r.code)
	}
	for _, c := range toolCaveats {
		caveatCodes = append(caveatCodes, c.code)
	}
	check("rows left out", e.LeftOut, byCode(reasonCodes, reasons))
	check("caveats", e.Caveats, byCode(caveatCodes, caveats))
	check("rows with a caveat", e.CaveatRows, len(caveatRows))
	check("rows flagged", e.Flagged, len(m.Flagged))
	check("rows waiting", e.Waiting, len(m.Waiting))
	// The differences from the snapshots that stderr lists (K-7 of the
	// consolidation): those gone, of shares and then of cash, then the
	// others, but those before or carried, each by its value and named as
	// the tool finds its security: by PP's WKN, else its ticker, else its
	// name; by Ghostfolio's asset. Each difference is the snapshot's figure
	// less the records', and a carried one is never before.
	var differences []ToolDifference
	finds := func(what string, pp *chainPPSecurity, asset *chainAsset) string {
		t.Helper()
		if (pp != nil) != (e.Tool == toolPP) || (asset != nil) != (e.Tool == toolGF) {
			t.Errorf("the %s manifest lists %s as of the PP security %+v and the asset %+v", e.Tool, what, pp, asset)
		}
		switch {
		case asset != nil:
			return asset.DataSource
		case pp == nil:
			return ""
		case pp.WKN != "":
			return "WKN " + pp.WKN
		case pp.Ticker != "":
			return "ticker " + pp.Ticker
		}
		return "name " + pp.Name
	}
	same := func(what string, snapshot, written, difference json.Number, before, carried bool) {
		t.Helper()
		if chainRat(snapshot).Sub(chainRat(snapshot), chainRat(written)).Cmp(chainRat(difference)) != 0 || difference == "" || before && carried {
			t.Errorf("the manifest lists %s with %s less %s as %q, before %v, carried %v", what, snapshot, written, difference, before, carried)
		}
	}
	if m.Holdings.Gone == nil {
		t.Errorf("the %s manifest's holdings list no differences gone", e.Tool)
	} else {
		for _, g := range *m.Holdings.Gone {
			differences = append(differences, ToolDifference{
				Account: g.Account, Key: g.SecurityKey, Symbol: g.Symbol, Asset: g.Asset.shown(), Finds: finds(g.SecurityKey, g.PP, g.Asset), Value: string(g.Was), Gone: true,
			})
		}
	}
	// PP's cash check, which Ghostfolio's manifest has none of.
	if (m.Holdings.Cash != nil) != (e.Tool == toolPP) || (m.Holdings.CashGone != nil) != (e.Tool == toolPP) {
		t.Errorf("the %s manifest's holdings hold a cash check: %v, %v", e.Tool, m.Holdings.Cash != nil, m.Holdings.CashGone != nil)
	}
	for _, g := range deref(m.Holdings.CashGone) {
		differences = append(differences, ToolDifference{Account: g.Account, Cash: true, Value: string(g.Was), Gone: true})
	}
	// A Ghostfolio export that writes no activity, which the import dialog
	// cannot import, carries every difference in its manifest, and stderr
	// lists its own all the same.
	unimported := e.Tool == toolGF && len(e.res.written) == 0
	listed := m.Holdings.Differences
	if unimported {
		listed = e.res.holdings
	}
	for _, h := range m.Holdings.Differences {
		same(h.Account+" "+h.SecurityKey, h.SnapshotShares, h.WrittenShares, h.Difference, h.Before, h.Carried)
		if unimported && !h.Carried {
			t.Errorf("the manifest of an export with no activity lists %s %s as not carried", h.Account, h.SecurityKey)
		}
	}
	for _, h := range listed {
		f := finds(h.SecurityKey, h.PP, h.Asset)
		if !h.Before && !h.Carried {
			differences = append(differences, ToolDifference{
				Account: h.Account, Key: h.SecurityKey, Symbol: h.Symbol, Asset: h.Asset.shown(), Finds: f, Value: string(h.Difference), LeftOut: h.LeftOut,
				Unsure: h.Unsure, NoActivity: h.NoActivity,
			})
		}
	}
	for _, c := range deref(m.Holdings.Cash) {
		if c.SnapshotCash == "" && c.WrittenCash == "" && c.Difference == "" && c.Carried && !c.Before {
			// An account whose cash an export could not check, carried with
			// no figures.
			continue
		}
		same(c.Account+"'s cash", c.SnapshotCash, c.WrittenCash, c.Difference, c.Before, c.Carried)
		if !c.Before && !c.Carried {
			differences = append(differences, ToolDifference{Account: c.Account, Cash: true, Value: string(c.Difference), LeftOut: c.LeftOut})
		}
	}
	check("differences listed", e.Differences, differences)
	if (m.Holdings.CashNotChecked != nil) != (e.Tool == toolPP) {
		t.Errorf("the %s manifest's holdings list accounts whose cash is not checked: %v", e.Tool, m.Holdings.CashNotChecked != nil)
	}
	var notChecked []ToolCashUnchecked
	if m.Holdings.CashNotChecked != nil {
		for _, a := range *m.Holdings.CashNotChecked {
			if a.Why != "margin" && a.Why != "no_cash" {
				t.Errorf("the manifest lists %s's cash as not checked, why %q", a.Account, a.Why)
			}
			notChecked = append(notChecked, ToolCashUnchecked{Account: a.Account, Why: a.Why})
		}
	}
	check("accounts whose cash is not checked", e.CashNotChecked, notChecked)
	// The accounts that neither check checks, which stderr names by kind:
	// with rows and no snapshot to check against, and, for PP, with
	// snapshots and nothing in PP, of which no export of the chain writes
	// or names anything, so that none is among the manifest's accounts.
	var noSnapshot, notInTool []string
	for _, l := range m.Holdings.NotChecked {
		if slices.Contains(e.NotInTool, l) {
			notInTool = append(notInTool, l)
		} else {
			noSnapshot = append(noSnapshot, l)
		}
	}
	check("accounts with no snapshot to check against", e.NoSnapshot, noSnapshot)
	check("accounts that the tool holds nothing of", e.NotInTool, notInTool)
	for _, l := range e.NotInTool {
		if e.Tool != toolPP || slices.ContainsFunc(accounts, func(a ToolAccount) bool { return a.Label == l }) {
			t.Errorf("the %s export names %s as an account that it holds nothing of, among its accounts %+v", e.Tool, l, accounts)
		}
	}
	// symbols_changed is PP's alone.
	check("securities that PP may hold otherwise than written", e.SymbolsChanged, len(deref(m.SymbolsChanged)))
	check("symbols_changed", e.Tool == toolPP, m.SymbolsChanged != nil)
	// keys_unsure, with the facts of each key and of each security that it
	// may be; each of its keys or symbols is that of the differences marked
	// unsure, and of no other (I-11).
	facts := func(f toolFactsEntry) ToolFacts {
		out := ToolFacts{Descriptions: f.Descriptions, Held: f.Held}
		if f.Rows != nil {
			out.First, out.Last = f.Rows.First, f.Rows.Last
		}
		for _, k := range f.Tickers {
			at, err := time.Parse(time.RFC3339, k.LastSeenAt)
			if err != nil || k.Source != "api" && k.Source != "history" {
				t.Errorf("the manifest lists the ticker %+v", k)
			}
			out.Tickers = append(out.Tickers, ToolTicker{Ticker: k.Ticker, Source: k.Source, LastSeen: at})
		}
		return out
	}
	shown := func(us []ToolUnsure) []ToolUnsure {
		out := slices.Clone(us)
		for i := range out {
			norm := func(f ToolFacts) ToolFacts {
				f.Descriptions, f.Tickers = orNil(f.Descriptions), slices.Clone(f.Tickers)
				for j := range f.Tickers {
					f.Tickers[j].LastSeen = f.Tickers[j].LastSeen.UTC()
				}
				return f
			}
			out[i].Facts, out[i].Securities = norm(out[i].Facts), slices.Clone(out[i].Securities)
			for j := range out[i].Securities {
				out[i].Securities[j].Facts = norm(out[i].Securities[j].Facts)
			}
		}
		return out
	}
	var unsure []ToolUnsure
	marked := map[string]bool{}
	for _, u := range m.KeysUnsure {
		k := ToolUnsure{Key: u.SecurityKey, Keys: u.SecurityKeys, Symbol: u.Symbol, Refs: u.Refs, Facts: facts(u.toolFactsEntry), Before: u.Before}
		for _, s := range u.Securities {
			k.Securities = append(k.Securities, ToolUnsureSecurity{
				Key: s.SecurityKey, Symbol: s.Symbol, CUSIP: s.CUSIP, Facts: facts(s.toolFactsEntry), Command: s.Command, LeftOut: s.LeftOut,
			})
		}
		unsure = append(unsure, k)
		marked["symbol "+u.Symbol] = u.SecurityKey == ""
		for _, key := range append([]string{u.SecurityKey}, u.SecurityKeys...) {
			marked["key "+key] = key != ""
		}
	}
	check("keys unsure", shown(e.Unsure), shown(unsure))
	for _, h := range m.Holdings.Differences {
		of := "key " + h.SecurityKey
		if h.SecurityKey == "" {
			of = "symbol " + h.Symbol
		}
		if !h.Carried && marked[of] != h.Unsure {
			t.Errorf("the manifest lists the difference of %s %s marked unsure %v, and keys_unsure %v", h.Account, of, h.Unsure, marked)
		}
	}
	var unused []string
	for _, s := range m.Chain.Manual {
		if s.Unused {
			unused = append(unused, s.Symbol)
		}
	}
	check("-manual symbols unused", e.Unused, unused)

	if e.Tool == toolPP {
		rows, err := csv.NewReader(bytes.NewReader(e.Files[0].data)).ReadAll()
		if err != nil || e.Files[0].Name != ppSecuritiesFile {
			t.Fatalf("the first file is %s: %v", e.Files[0].Name, err)
		}
		securities, prices := map[string]bool{}, 0
		date := slices.Index(rows[0], "Date of Quote")
		for _, r := range rows[1:] {
			securities[strings.Join(r[:3], "|")] = true
			if r[date] != "" {
				prices++
			}
		}
		check("securities", e.Securities, len(securities))
		check("prices", e.Prices, prices)
		return
	}
	var profiles, pricesOnly int
	for _, p := range m.Ghostfolio.Profiles {
		if p.Status == "used" {
			profiles++
		} else {
			pricesOnly++
		}
	}
	check("profiles", e.Profiles, profiles)
	check("profiles with prices only", e.PricesOnly, pricesOnly)
	check("symbols for Yahoo", e.Yahoo, len(m.Ghostfolio.Yahoo))
	check("symbols for Yahoo that may be gone", e.YahooIdle, len(m.Ghostfolio.YahooIdle))
	if y, err := time.Parse(time.DateOnly, string(m.Filters.To)); err == nil {
		check("day from which a symbol is idle", e.IdleSince, nytime.Date(y.AddDate(-1, 0, 0).Format(time.DateOnly)))
	}
	// The accounts of cash rows that no balance carries, in the order of
	// their ids, with the last balance of those that the file holds: an
	// account with no snapshot is in no file.
	var notIn []ToolCashRows
	labels := slices.SortedFunc(maps.Keys(gfCash), func(a, b string) int { return cmp.Compare(labelID(t, a), labelID(t, b)) })
	for _, label := range labels {
		c := ToolCashRows{Account: label, Rows: gfCash[label]}
		if i := slices.IndexFunc(accounts, func(a ToolAccount) bool { return a.Label == label }); i >= 0 {
			c.Snapshot = accounts[i].LastBalance
		}
		for _, got := range e.NotInBalances {
			if got.Account == label {
				c.Taken = got.Taken
			}
		}
		if c.Snapshot != "" && c.Taken.IsZero() || c.Snapshot == "" && !c.Taken.IsZero() {
			t.Errorf("%s's cash rows not in the balances follow a snapshot of %q taken at %v", label, c.Snapshot, c.Taken)
		}
		notIn = append(notIn, c)
	}
	check("cash rows not in the balances", e.NotInBalances, notIn)
}

// labelID returns the id of the account labelled label, as "A3 ...783".
func labelID(t *testing.T, label string) int64 {
	t.Helper()
	ref, _, _ := strings.Cut(label, " ")
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, "A"), 10, 64)
	if err != nil {
		t.Fatalf("%q is not an account's label", label)
	}
	return id
}

// deref returns what p points to, or the zero value for nil.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// counted returns how many differences of shares and of cash the export e
// lists on stderr with their value, those gone left aside.
func counted(e *ToolExport) (holdings, cash int) {
	for _, d := range e.Differences {
		switch {
		case d.Gone:
		case d.Cash:
			cash++
		default:
			holdings++
		}
	}
	return holdings, cash
}

// orNil returns s, or nil for an empty s, as the export holds an empty list.
func orNil[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return s
}

// toolSince returns the options of an export for tool up to the day to that
// goes on from the export of s, by its folder's manifest, as the CLI reads
// it (-since).
func toolSince(t *testing.T, tool string, to nytime.Date, s *chainStep) ToolOptions {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	return ToolOptions{Tool: tool, To: to, Since: b, SinceDir: s.dir}
}

// toolChainOf returns the options of a PP export up to 2026-11-11 that goes
// on from the export of s, whose manifest's chain is made to select the
// account account.
func toolChainOf(t *testing.T, s *chainStep, account string) ToolOptions {
	t.Helper()
	o := toolSince(t, toolPP, "2026-11-11", s)
	var m map[string]any
	if err := json.Unmarshal(o.Since, &m); err != nil {
		t.Fatal(err)
	}
	m["chain"].(map[string]any)["account"] = account
	var err error
	if o.Since, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	return o
}

// toolStoreGolden requires that the folder of testdata/tools/store/<tool>
// named name hold the files of the export s, byte for byte: for PP, but the
// configurations, which must be those that every export writes.
func toolStoreGolden(t *testing.T, s *chainStep, tool, name string) {
	t.Helper()
	files := toolFolder(t, s.dir)
	if tool == toolPP {
		for n, data := range ppConfigurations() {
			if !bytes.Equal(files[n], data) {
				t.Errorf("%s: the export writes %s otherwise than every export does", name, n)
			}
			delete(files, n)
		}
	}
	toolGolden(t, filepath.Join("testdata", "tools", "store", tool, name), files)
}

// toolModels are a model of each tool, into which a chain of exports is
// imported, as you import them.
type toolModels struct {
	pp *ppModel
	gf *gfModel
}

// apply imports the export s into the model of its tool as the how-to has
// you (ppApply, gfApply), and requires that the model then hold what the
// store's ledger holds, as s's mapping writes it (ppReconcile,
// gfReconcile): wantNew activities new to Ghostfolio, where -1 means none,
// which the import dialog refuses.
func (m *toolModels) apply(t *testing.T, s *chainStep, wantNew int) {
	t.Helper()
	if s.in.tool == toolPP {
		ppApply(t, m.pp, s)
		ppReconcile(t, m.pp, s.in, s.events, s.outcomes)
		return
	}
	n, err := gfApply(t, m.gf, s)
	switch {
	case wantNew < 0 && err == nil:
		t.Errorf("Ghostfolio imports %d activities of an export that holds no new one", n)
	case wantNew >= 0 && (err != nil || n != wantNew):
		t.Errorf("Ghostfolio imports %d activities, %v; want %d", n, err, wantNew)
	}
	gfReconcile(t, s.in, s.events, s.outcomes, m.gf)
}

// TestToolExport exports the tool exports' store (11.3 of the design), after
// reading it as the exports do (checkToolRead):
//
//   - in full, for each tool, up to the day before the store's today (no
//     -to): what it writes is its golden files, with a transfer in both
//     accounts' files, a combined row and a row for each leg of the spread;
//     it passes the checkers, imports into the models with exactly the marks
//     that it predicts, and leaves them holding the ledger's cash and
//     shares; the folder and its files are readable only by you; and an
//     export made again writes the same bytes;
//   - for one account, which writes that account's records of the full
//     export, but the journal with the other account, by its sign; an
//     export that goes on from it selects that account, by its ref;
//   - refused once it has read the store: a -to before the export that it
//     goes on from, an export of another store or of the other tool, a
//     chain of an account that the store does not hold or of no account's
//     ref, a -manual typo;
//   - then a chain, imported into the same models: once the later files are
//     in (checkToolReadLater), a -since export writes the row "as of" a day
//     that the first one exported and both dividends of one day, nothing
//     else; once you uncount the dividend that Schwab corrected, the next
//     lists it as gone, keeps its correction given and flagged, and writes
//     nothing; and once you pair a journal with its other side and uncount
//     that side (checkToolReadDecided), the next lists that side as gone
//     and, for PP, the journal as changed, now a Deposit.
func TestToolExport(t *testing.T) {
	t.Parallel()
	ts := newToolStore(t)
	want := checkToolRead(t, ts)
	base := t.TempDir()
	tools := []string{toolPP, toolGF}
	models := map[string]*toolModels{}
	full := map[string]*chainStep{}

	for _, tool := range tools {
		s := toolExportTo(t, ts, ToolOptions{Tool: tool}, filepath.Join(base, tool, "full"))
		full[tool] = s
		if s.in.to != "2026-11-11" {
			t.Errorf("%s: an export with no -to ends on %s, not the day before the store's today", tool, s.in.to)
		}
		toolStoreGolden(t, s, tool, "full")
		if runtime.GOOS != "windows" {
			files, err := os.ReadDir(s.dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if info, err := f.Info(); err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("%s: %s is %v, %v; want readable only by you", tool, f.Name(), info.Mode(), err)
				}
			}
			if info, err := os.Stat(s.dir); err != nil || info.Mode().Perm() != 0o700 {
				t.Errorf("%s: the folder is %v, %v; want readable only by you", tool, info.Mode(), err)
			}
		}
		again := toolExportTo(t, ts, ToolOptions{Tool: tool, To: "2026-11-11"}, filepath.Join(base, tool, "again"))
		if a, b := toolFolder(t, s.dir), toolFolder(t, again.dir); !maps.EqualFunc(a, b, bytes.Equal) {
			t.Errorf("%s: an export made again writes other bytes", tool)
		}

		// The cash journal is a transfer, one record of its paying side, in
		// its account's file, with which its receiving side is combined; the
		// cash merger's shares row rides with its cash row's sale; the
		// spread is a record for each leg.
		records := map[string][]string{}
		for i := range s.res.written {
			e := &s.res.written[i]
			what := ppRecordOf(e).typ
			if tool == toolGF {
				what = gfActivityOf(e).Type
			}
			if e.leg.Valid {
				what += " leg " + strconv.FormatInt(e.leg.Int64, 10)
			}
			records[e.ref] = append(records[e.ref], e.account.label()+" "+what)
		}
		if tool == toolPP {
			i := slices.IndexFunc(s.res.outcomes, func(o toolOutcome) bool { return o.ref == "T73600000102" })
			if got := records["T73600000101"]; !slices.Equal(got, []string{"A2 ...398 Transfer (Outbound)"}) || len(records["T73600000102"]) > 0 ||
				i < 0 || s.res.outcomes[i].outcome != outcomeCombined || s.res.outcomes[i].withLabel()+" "+s.res.outcomes[i].with != "A2 ...398 T73600000101" {
				t.Errorf("%s: the transfer is %q, its receiving side %q", tool, got, records["T73600000102"])
			}
		}
		h1, h2 := fileRefs(ts.history)+"1", fileRefs(ts.history)+"2"
		if i := slices.IndexFunc(s.res.outcomes, func(o toolOutcome) bool { return o.ref == h2 }); i < 0 ||
			s.res.outcomes[i].outcome != outcomeCombined || s.res.outcomes[i].with != h1 {
			t.Errorf("%s: the cash merger's shares row is not combined with its sale", tool)
		}
		spread := []string{"A2 ...398 Buy leg 2", "A2 ...398 Sell leg 3"}
		if tool == toolGF {
			spread = []string{"A2 ...398 BUY leg 2", "A2 ...398 SELL leg 3"}
		}
		if got := records["T73600000105"]; !slices.Equal(got, spread) {
			t.Errorf("%s: the spread is %q, want %q", tool, got, spread)
		}

		m := &toolModels{pp: newPPModel(), gf: newGFModel()}
		models[tool] = m
		m.apply(t, s, len(s.res.written))
		if tool == toolGF {
			// XYZ alone has a dividend and no investment: the options
			// account trades XYZ as options only, and held its shares
			// before the store's rows.
			if p := m.gf.performance(); !slices.Equal(p.noInvestment, []string{"YAHOO XYZ"}) {
				t.Errorf("the dividends with no investment are %q", p.noInvestment)
			}
		}
	}

	// One account: its records of the full export, but the journal with the
	// other account, which is written by its sign.
	for _, tool := range tools {
		s := toolExportTo(t, ts, ToolOptions{Tool: tool, Account: "A2", To: "2026-11-11"}, filepath.Join(base, tool, "account"))
		if s.in.account != ts.options {
			t.Fatalf("%s: -account A2 selects A%d", tool, s.in.account)
		}
		of := func(s *chainStep) map[string]string {
			out := map[string]string{}
			for i := range s.res.written {
				e := &s.res.written[i]
				if e.account.id != ts.options {
					continue
				}
				k := e.ref
				if e.leg.Valid {
					k += " leg " + strconv.FormatInt(e.leg.Int64, 10)
				}
				if tool == toolPP {
					out[k] = strings.Join(ppRecordOf(e).cells(), "|")
					continue
				}
				a := gfActivityOf(e)
				out[k] = strings.Join([]string{a.AccountID, a.Comment, a.Date, a.Type, a.Symbol, a.Quantity.String(), a.UnitPrice.String(), a.Fee.String()}, "|")
			}
			return out
		}
		got, wantOf := of(s), of(full[tool])
		if tool == toolPP {
			journal := got["T73600000101"]
			if !strings.Contains(journal, "|Withdrawal|500.00|") || !strings.Contains(journal, "paired with A1 ...135 T73600000102, written apart") {
				t.Errorf("pp: the journal of one account is %q", journal)
			}
			delete(got, "T73600000101")
			delete(wantOf, "T73600000101")
		}
		if !maps.Equal(got, wantOf) {
			t.Errorf("%s: one account's records differ from the full export's:\n%q\n%q", tool, got, wantOf)
		}
		if i := slices.IndexFunc(s.res.outcomes, func(o toolOutcome) bool { return o.ref == "T73600000101" }); tool == toolPP &&
			(i < 0 || s.res.outcomes[i].outcome != outcomeWritten || !slices.Equal(s.res.outcomes[i].caveats, []string{"pair_outside"})) {
			t.Errorf("pp: one account's journal is not written by its sign, caveat pair_outside")
		}
		pm := &toolModels{pp: newPPModel(), gf: newGFModel()}
		pm.apply(t, s, len(s.res.written))

		// An export that goes on from it selects its account, by its ref:
		// it gives every row, and writes none.
		next := toolExportTo(t, ts, toolSince(t, tool, "2026-11-11", s), filepath.Join(base, tool, "account-since"))
		if next.in.account != ts.options || !slices.Equal(next.refs(outcomeGiven), s.refs(outcomeWritten)) || len(next.res.written) > 0 {
			t.Errorf("%s: the export after one account's selects A%d, gives %q, writes %d records", tool, next.in.account, next.refs(outcomeGiven), len(next.res.written))
		}
		pm.apply(t, next, -1)
	}

	// What the export refuses once it has read the store.
	for _, c := range []struct {
		what string
		o    ToolOptions
		want string
	}{
		{"a -to before the earlier export's", toolSince(t, toolPP, "2026-11-10", full[toolPP]), "-to 2026-11-10 is before the end of the export in " + full[toolPP].dir + ", 2026-11-11"},
		{"the other tool's export", toolSince(t, toolGF, "2026-11-11", full[toolPP]), "is not the manifest of a -format ghostfolio export of layout 1"},
		{"another store's export", func() ToolOptions {
			o := toolSince(t, toolPP, "2026-11-11", full[toolPP])
			o.Since = bytes.Replace(o.Since, []byte(`"store": "`+full[toolPP].in.store+`"`), []byte(`"store": "2020-01-02T03:04:05.000Z"`), 1)
			return o
		}(), "that export was made from another store (made 2020-01-02T03:04:05.000Z): export in full and redo in the tool"},
		{"a -manual typo", ToolOptions{Tool: toolGF, Manual: []string{"ABCD", "AAPLE"}}, "-manual names AAPLE, which no security in the store has"},
		{"a chain of an account that the store does not hold", toolChainOf(t, full[toolPP], "A99"), "-since " + full[toolPP].dir + ": its chain selects A99, which the store does not hold"},
		{"a chain of no account's ref", toolChainOf(t, full[toolPP], "B2"), "-since " + full[toolPP].dir + `: the manifest's chain selects the account "B2", which is no A<id>`},
	} {
		if _, err := ts.s.ExportTool(t.Context(), c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.what, err, c.want)
		}
	}

	// The chain. The later files bring a row "as of" a day that the full
	// export exported, posted after it, and a dividend with its correction.
	ts.importLater(t)
	want = checkToolReadLater(t, ts, want)
	later, correction := fileRefs(ts.later), fileRefs(ts.correction)
	since := map[string]*chainStep{}
	for _, tool := range tools {
		s := toolExportTo(t, ts, toolSince(t, tool, "2026-11-13", full[tool]), filepath.Join(base, tool, "since1"))
		since[tool] = s
		toolStoreGolden(t, s, tool, "since1")
		written := []string{later + "1", later + "2", correction + "1"}
		if got := s.refs(outcomeWritten); !slices.Equal(got, written) {
			t.Errorf("%s: the export after the later files writes %q, want %q", tool, got, written)
		}
		if got, want := s.refs(outcomeGiven), full[tool].refs(outcomeWritten); !slices.Equal(got, want) {
			t.Errorf("%s: it gives %q, want %q", tool, got, want)
		}
		if changed, gone := rcChanged(s); len(changed)+len(gone) > 0 || s.e.Late != 1 || s.e.Since != exportInstant(full[tool].e.At) || s.e.SinceTo != "2026-11-11" {
			t.Errorf("%s: it lists %q and %q, %d late, going on from %s up to %s", tool, changed, gone, s.e.Late, s.e.Since, s.e.SinceTo)
		}
		models[tool].apply(t, s, 3)
	}

	// You uncount the dividend that Schwab corrected: the next export lists
	// it as gone, keeps its correction given, with its flag, and writes
	// nothing.
	if _, err := ts.s.DecideAll(t.Context(), []Decision{toolLaterDecision(ts)}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		s := toolExportTo(t, ts, toolSince(t, tool, "2026-11-13", since[tool]), filepath.Join(base, tool, "since2"))
		toolStoreGolden(t, s, tool, "since2")
		if got := s.refs(outcomeWritten); len(got) > 0 {
			t.Errorf("%s: the export after your decision writes %q", tool, got)
		}
		was := "account 2026-11-13 Dividend 3.60"
		if tool == toolGF {
			was = "2026-11-13 DIVIDEND 3.60"
		}
		if changed, gone := rcChanged(s); len(changed) > 0 || !slices.Equal(gone, []string{later + "2 uncounted: user: " + was}) {
			t.Errorf("%s: it lists %q and %q", tool, changed, gone)
		}
		i := slices.IndexFunc(s.res.outcomes, func(o toolOutcome) bool { return o.ref == correction+"1" })
		if o := s.res.outcomes[i]; o.outcome != outcomeGiven || !slices.Equal(o.flags, []string{"near_duplicate"}) {
			t.Errorf("%s: the correction is %s, flagged %q", tool, o.outcome, o.flags)
		}
		models[tool].apply(t, s, -1)
		since[tool] = s
	}

	// You pair the journal with its other side and uncount that side: for
	// PP, the next export lists that side, the transfer's paying side, as
	// gone, and its transfer goes with both legs; the journal, now a
	// deposit, which had no entry of its own, is written. Ghostfolio's
	// balances carry both.
	if _, err := ts.s.DecideAll(t.Context(), toolDecisions(ts)); err != nil {
		t.Fatal(err)
	}
	checkToolReadDecided(t, ts, want)
	for _, tool := range tools {
		s := toolExportTo(t, ts, toolSince(t, tool, "2026-11-13", since[tool]), filepath.Join(base, tool, "since3"))
		toolStoreGolden(t, s, tool, "since3")
		var changed, gone, written []string
		if tool == toolPP {
			gone = []string{"T73600000101 uncounted: user: account 2026-09-08 Transfer (Outbound) 500.00"}
			written = []string{"T73600000102"}
		}
		if c, g := rcChanged(s); !slices.Equal(c, changed) || !slices.Equal(g, gone) || !slices.Equal(s.refs(outcomeWritten), written) {
			t.Errorf("%s: the export after your pairing lists %q and %q, writes %q", tool, c, g, s.refs(outcomeWritten))
		}
		models[tool].apply(t, s, -1)
		since[tool] = s
	}

	// The full export wrote META's sale, of the API's key, on its own: the
	// store holds that key apart from the position's, of META's CUSIP, and
	// the two share META alone (I-4 of the consolidation). You say that they
	// are one security: the next export lists the sale as changed, writes it
	// under META, lists nothing unsure, names no security as changed in
	// symbols, and lists the difference of the sale's security gone and
	// META's with its value.
	if _, err := ts.s.DecideAll(t.Context(), []Decision{{Kind: "security", Subject: "k:i:1940083512", Value: "same k:c:30303M102"}}); err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if u := unsureText(full[tool].e.Unsure); !slices.Equal(u, []string{"i:1940083512: c:30303M102 schwabtx edit K:i:1940083512 -same K:c:30303M102; symbol META"}) {
			t.Errorf("%s: the full export lists %q", tool, u)
		}
		s := toolExportTo(t, ts, toolSince(t, tool, "2026-11-13", since[tool]), filepath.Join(base, tool, "since4"))
		toolStoreGolden(t, s, tool, "since4")
		was := "portfolio 2026-03-13 Sell 603.36"
		if tool == toolGF {
			was = "2026-03-13 SELL 603.40 fee 0.04"
		}
		var differences []string
		for _, d := range s.e.Differences {
			line := cmp.Or(d.Asset, d.Finds) + " " + d.Value
			if d.Gone {
				line += " gone"
			}
			if d.Unsure {
				line += " unsure"
			}
			differences = append(differences, line)
		}
		// Ghostfolio's two exports before held no activity, which its
		// import dialog cannot import: this one lists every difference
		// again, never as before (K-6).
		meta := map[string][]string{
			toolPP: {"name Security (i:1940083512) 1 gone", "WKN 30303M102 2"},
			toolGF: {
				"Security (i:1940083512) 1 gone", "Security (00507V109) 10", "SWVXX 1220.89", "VTI 12", "AAPL 1.1788", "BND -0.0378", "VBR 0.4513",
				"XYZ 2026-06-19 45 put (XYZ260619P00045000) -200", "META 2", "AAPL -2", "Security (i:205187341) -50",
				"XYZ 2025-02-21 40 put (XYZ250221P00040000) -200", "XYZ 2026-09-18 50 call (XYZ260918C00050000) 100",
				"XYZ 2026-10-16 45 put (XYZ261016P00045000) -100", "XYZ 2026-10-16 40 put (XYZ261016P00040000) 100",
			},
		}[tool]
		if c, g := rcChanged(s); !slices.Equal(c, []string{"T73508261942: " + was + " -> " + was}) || len(g) > 0 || !slices.Equal(s.refs(outcomeWritten), []string{"T73508261942"}) ||
			len(s.e.Unsure) > 0 || s.e.SymbolsChanged > 0 || !slices.Equal(differences, meta) {
			t.Errorf("%s: the export after your -same lists %q and %q, writes %q, unsure %q, %d securities changed, differences %q",
				tool, c, g, s.refs(outcomeWritten), unsureText(s.e.Unsure), s.e.SymbolsChanged, differences)
		}
		models[tool].apply(t, s, 1)
	}
}

// TestToolOptions checks what ExportTool refuses before it reads the store
// (9 of the design), and the last day it takes when none is given: the day
// before today in New York, whose midnight is the store's.
func TestToolOptions(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.November, 13, 4, 30, 0, 0, time.UTC) // 23:30 in New York on the 12th
	if o, since, err := (ToolOptions{Tool: toolPP}).check(now); err != nil || o.To != "2026-11-11" || since != nil {
		t.Errorf("no -to: %q, %v, %v", o.To, since, err)
	}
	if o, _, err := (ToolOptions{Tool: toolGF, To: "2026-10-01", Manual: []string{"ABCD"}}).check(now); err != nil || o.To != "2026-10-01" {
		t.Errorf("-to 2026-10-01: %q, %v", o.To, err)
	}
	manifest := []byte(`{"exported_at": "2026-11-01T12:00:00Z", "store": "2026-01-01T00:00:00.000Z", "format": "pp", "layout": 1,
		"chain": {"id": "2026-11-01T12:00:00Z", "account": "A2", "manual": [], "to": "2026-10-31"}}`)
	if _, since, err := (ToolOptions{Tool: toolPP, Since: manifest, SinceDir: "pp-1"}).check(now); err != nil || since == nil || since.Chain.To != "2026-10-31" {
		t.Errorf("-since: %+v, %v", since, err)
	}
	for _, c := range []struct {
		o    ToolOptions
		want string
	}{
		{ToolOptions{Tool: "csv"}, `a tool export is for pp or ghostfolio, not "csv"`},
		{ToolOptions{Tool: toolPP, To: "11/12/2026"}, `"11/12/2026" is not a date (YYYY-MM-DD)`},
		// Said back masked, as every value an error says back: its digits
		// could be an account number's.
		{ToolOptions{Tool: toolPP, To: "2026-02-30"}, `"...230" is not a date (YYYY-MM-DD)`},
		{ToolOptions{Tool: toolPP, Manual: []string{"ABCD"}}, "-manual is for -format ghostfolio"},
		{ToolOptions{Tool: toolPP, Account: "A1", Since: manifest, SinceDir: "pp-1"}, "-since pp-1 goes on from the export in pp-1, with its account: drop -account"},
		{ToolOptions{Tool: toolPP, Since: []byte("{"), SinceDir: "pp-1"}, "-since pp-1: " + filepath.Join("pp-1", "manifest.json") + " is not the manifest of a -format pp export of layout 1"},
		{ToolOptions{Tool: toolGF, Since: bytes.Replace(manifest, []byte(`"layout": 1`), []byte(`"layout": 2`), 1), SinceDir: "gf-1"}, "-since gf-1: " + filepath.Join("gf-1", "manifest.json") + " is not the manifest of a -format ghostfolio export of layout 1"},
	} {
		if _, _, err := c.o.check(now); err == nil || err.Error() != c.want {
			t.Errorf("%+v: %v, want %q", c.o, err, c.want)
		}
	}
	// A manual symbol of spaces alone names nothing, as -manual takes it.
	if _, _, err := (ToolOptions{Tool: toolPP, Manual: []string{" "}}).check(now); err != nil {
		t.Errorf("-manual of a space for pp: %v", err)
	}
}

// TestToolWriteDir checks where WriteDir writes an export: into a folder
// that it makes when missing, the data files first and manifest.json last,
// and never over one of the store's files, which it refuses before it
// writes anything.
func TestToolWriteDir(t *testing.T) {
	t.Parallel()
	in := toolCaseNamed("transfers").in
	dir := t.TempDir()
	store := filepath.Join(dir, "securities.csv")
	e, err := exportTool(&in, ToolOptions{Tool: toolPP}, nil, testNow, buildVersions(), store)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.WriteDir(dir); err == nil || !strings.Contains(err.Error(), "would replace the store itself") {
		t.Errorf("an export over the store: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("refused, it wrote %d files", len(entries))
	}
	// The manifest is written last: a folder whose manifest.json cannot be
	// written holds every other file.
	out := filepath.Join(dir, "export")
	if err := os.MkdirAll(filepath.Join(out, "manifest.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	e.store = ""
	if err := e.WriteDir(out); err == nil {
		t.Error("an export over a folder named manifest.json was written")
	}
	names := []string{ppSecuritiesConfig, ppAccountConfig, ppPortfolioConfig, "manifest.json"}
	for _, f := range e.Files {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	if got := slices.Sorted(maps.Keys(toolFolderNames(t, out))); !slices.Equal(got, names) {
		t.Errorf("the folder holds %q, want %q", got, names)
	}
}

// toolFolderNames returns the names of what the folder dir holds.
func toolFolderNames(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	return names
}

// TestToolManifest checks what the golden files of the cases do not show
// of a manifest: the -manual symbols of its filters are said back masked,
// as every value of them is, though the store's symbols that they name may
// hold any digits; and PP's symbols_changed names a renamed security, a
// ticker of more than six letters, as written, with each of its names once
// for each source that named it, with the latest time that that source saw
// it: the API's first, then the History files', each the latest first.
// Ghostfolio's manifest has no symbols_changed.
func TestToolManifest(t *testing.T) {
	t.Parallel()
	const keyDigits, keyLong = "i:2000001", "i:2000002"
	seen := time.Date(2026, time.January, 5, 15, 0, 0, 0, time.UTC)
	in := tcInput(toolInput{
		securities: []toolSecurityRow{
			{key: keyDigits, assetType: "EQUITY", symbol: "31415926"},
			{key: keyLong, assetType: "EQUITY", symbol: "ABCDEFGJ"},
		},
		names: []toolName{
			{key: keyDigits, kind: "symbol", value: "31415926", firstSeen: seen, lastSeen: seen},
			{key: keyLong, kind: "symbol", value: "ABCDEFGJ", firstSeen: seen.AddDate(0, 2, 0), lastSeen: seen.AddDate(0, 2, 0)},
			{key: keyLong, kind: "symbol", value: "ABCDEFGH", firstSeen: seen, lastSeen: seen},
			// Two History files that the store joined to the key, which named
			// it by the latest ticker too.
			{key: keyLong, from: "x:66000901", kind: "symbol", value: "ABCDEFGJ", firstSeen: seen.AddDate(0, 1, 0), lastSeen: seen.AddDate(0, 1, 0)},
			{key: keyLong, from: "x:66000902", kind: "symbol", value: "ABCDEFGJ", firstSeen: seen.AddDate(0, 1, 15), lastSeen: seen.AddDate(0, 1, 15)},
		},
		rows: []toolRow{
			rcTrade("T81100101", 1, "2026-01-05 10:00:00", "buy", keyDigits, "31415926", 1_000_000, 20, -2000, 0),
			rcTrade("T81100102", 1, "2026-03-05 10:00:00", "buy", keyLong, "ABCDEFGJ", 1_000_000, 30, -3000, 0),
		},
	})
	renamed := []toolSymbolsEntry{{
		SecurityKey: keyLong, WrittenAs: "ABCDEFGJ", Why: []string{whyRenamed},
		Names: []toolSymbolName{{"ABCDEFGJ", "api", "2026-03-05T15:00:00Z"}, {"ABCDEFGH", "api", "2026-01-05T15:00:00Z"}, {"ABCDEFGJ", "history", "2026-02-20T15:00:00Z"}},
	}}
	for _, c := range []struct {
		tool   string
		manual []string
		want   *[]toolSymbolsEntry
	}{
		{toolPP, nil, &renamed},
		{toolGF, []string{"31415926"}, nil},
	} {
		in := in
		e, err := exportTool(&in, ToolOptions{Tool: c.tool, Manual: c.manual}, nil, testNow, buildVersions(), "")
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			Filters struct{ Manual []string } `json:"filters"`
			Chain   struct {
				Manual []chainManual `json:"manual"`
			} `json:"chain"`
			SymbolsChanged *[]toolSymbolsEntry `json:"symbols_changed"`
		}
		if err := json.Unmarshal(e.manifest, &m); err != nil {
			t.Fatal(err)
		}
		wantManual := []string{}
		if c.manual != nil {
			wantManual = []string{"...926"}
		}
		if !slices.Equal(m.Filters.Manual, wantManual) {
			t.Errorf("%s: the filters' -manual are %q, want %q", c.tool, m.Filters.Manual, wantManual)
		}
		if !reflect.DeepEqual(m.SymbolsChanged, c.want) {
			t.Errorf("%s: symbols_changed is %+v, want %+v", c.tool, deref(m.SymbolsChanged), deref(c.want))
		}
	}
}
