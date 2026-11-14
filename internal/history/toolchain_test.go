package history

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The chain of tool exports (3.8 of the design), tested without a store:
// toolInput states, the real mapping, writers, chain and replay, and the
// models of PP and Ghostfolio that toolcheck_pp_test.go and
// toolcheck_gf_test.go hold (11.5). Every value is made up.

// A chainStep is an export of a chain: its input, its mapping, what
// compareChain made of it, the folder that holds its files, and its
// manifest, by whose chain part the next export goes on from it.
type chainStep struct {
	// e is the export, as ExportTool makes it.
	e  *ToolExport
	in *toolInput
	// events and outcomes are the mapping's (toolEvents): every row the
	// tool holds once the chain is imported, as a full export writes it.
	events   []toolEvent
	outcomes []toolOutcome
	res      *chainResult
	unused   []string
	dir      string
	// pp are a PP export's files and marks what PP marks of them (ppTicks);
	// gf is a Ghostfolio export, and file its bytes.
	pp    []ppFile
	marks []ppMark
	gf    *gfExport
	file  []byte
	// manifest is its manifest.json.
	manifest []byte
}

// chainStepOf exports in for tool at exportedAt, as ExportTool does after
// its read (exportTool): going on from prev, nil for a full export, with
// prev's account and its -manual list and those that in names; writes it
// to a folder of its own (WriteDir); and requires the files to pass the
// tool's checker.
func chainStepOf(t *testing.T, in toolInput, tool string, prev *chainStep, exportedAt string) *chainStep {
	t.Helper()
	at, err := time.Parse(time.RFC3339, exportedAt)
	if err != nil {
		t.Fatal(err)
	}
	o := ToolOptions{Tool: tool, Manual: in.manual}
	var since *toolChain
	if prev != nil {
		if since, err = parseChain(prev.manifest); err != nil {
			t.Fatal(err)
		}
		if in.account, err = since.account(); err != nil {
			t.Fatal(err)
		}
		o.SinceDir = prev.dir
	}
	e, err := exportTool(&in, o, since, at, buildVersions(), "")
	if err != nil {
		t.Fatal(err)
	}
	return chainStepFrom(t, e, t.TempDir())
}

// chainStepFrom writes the export e to the folder dir (WriteDir), requires
// its files to pass the tool's checker, and returns it as a step of a chain.
func chainStepFrom(t *testing.T, e *ToolExport, dir string) *chainStep {
	t.Helper()
	s := &chainStep{
		e: e, in: e.in, events: e.events, outcomes: e.outcomes, res: e.res, unused: e.Unused, dir: dir,
		pp: e.pp, marks: e.marks, gf: e.gf, manifest: e.manifest,
	}
	checkChain(t, s)
	checkToolSummary(t, e)
	if err := e.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	switch e.Tool {
	case toolPP:
		ppCheck(t, dir, ppAccountNames(e))
	case toolGF:
		path := filepath.Join(dir, gfFileName)
		var err error
		if s.file, err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
		gfCheck(t, path, gfDeliveries(e.res.written))
	}
	return s
}

// checkChain requires what compareChain made of s to agree with s's
// mapping (3.1 of the design): each row given was written by the mapping,
// and has no record to write, while each row written has all of its
// records; each row combined follows a cash row written or given; any other
// row keeps the mapping's outcome; and the next given holds an entry for
// each row written or given, and none else.
func checkChain(t *testing.T, s *chainStep) {
	t.Helper()
	res := s.res
	if len(res.outcomes) != len(s.in.rows) {
		t.Fatalf("%d outcomes for %d rows", len(res.outcomes), len(s.in.rows))
	}
	count := func(events []toolEvent, i int) int {
		return len(slices.DeleteFunc(slices.Clone(events), func(e toolEvent) bool { return e.row != i }))
	}
	byRef := map[rowKey]int{}
	for i, r := range s.in.rows {
		byRef[rowKey{r.account, r.ref}] = i
	}
	entries := 0
	for i, o := range res.outcomes {
		r, m := s.in.rows[i], s.outcomes[i]
		_, entry := res.given[o.account.label()][r.ref]
		switch o.outcome {
		case outcomeGiven, outcomeWritten:
			n := count(res.written, i)
			if m.outcome != outcomeWritten || o.outcome == outcomeGiven && n != 0 || o.outcome == outcomeWritten && n != count(s.events, i) || !entry {
				t.Errorf("%s is %s, mapped %s, with %d records to write and an entry %v", r.ref, o.outcome, m.outcome, n, entry)
			}
			entries++
		case outcomeCombined:
			c, ok := byRef[rowKey{r.account, o.with}]
			if !ok || res.outcomes[c].outcome != outcomeWritten && res.outcomes[c].outcome != outcomeGiven || entry {
				t.Errorf("%s is combined with %s, which is not written or given", r.ref, o.with)
			}
		default:
			if o.outcome != m.outcome || entry {
				t.Errorf("%s is %s, mapped %s", r.ref, o.outcome, m.outcome)
			}
		}
	}
	all := 0
	for _, refs := range res.given {
		all += len(refs)
	}
	if all != entries {
		t.Errorf("the next given holds %d entries, for %d rows written or given", all, entries)
	}
}

// refs returns the refs of the rows of s whose outcome is outcome, in the
// rows' order.
func (s *chainStep) refs(outcome string) []string {
	var out []string
	for i, o := range s.res.outcomes {
		if o.outcome == outcome {
			out = append(out, s.in.rows[i].ref)
		}
	}
	return out
}

// ppTypes returns PP's types by the label that the export writes them
// under, as the configurations' maps say.
func ppTypes(t *testing.T) map[string]string {
	t.Helper()
	types := map[string]string{}
	for _, data := range []string{ppAccountConfiguration, ppPortfolioConfiguration} {
		c, err := ppApplyConfig([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		maps.Copy(types, c.typeLabels())
	}
	return types
}

// ppHeld returns the sides of the transactions that m holds, as ppKeys
// names them, each once for each transaction that holds it, sorted: what
// the replay of PP's duplicate rule must start from.
func ppHeld(t *testing.T, m *ppModel) []string {
	t.Helper()
	labels := map[string]string{}
	for label, typ := range ppTypes(t) {
		labels[typ] = label
	}
	var out []string
	for kind, owners := range map[string][]*ppOwner{ppAccount: m.accounts, ppPortfolio: m.portfolios} {
		for _, o := range owners {
			for _, tx := range o.txs {
				shares, security := "", ""
				if kind == ppPortfolio {
					shares = formatMicros(tx.shares / 100)
				}
				if s := tx.security; s != nil {
					security = ppFinds(s.ticker, s.wkn, s.name)
				}
				out = append(out, chainHash([]string{kind, o.name, ppClass(kind, labels[tx.typ]), tx.day, tx.currency, formatCents(tx.amount), shares, security}))
			}
		}
	}
	slices.Sort(out)
	return out
}

// ppStart returns the sides that the replay of s starts from, sorted.
func ppStart(s *chainStep) []string {
	var out []string
	for _, keys := range s.res.start {
		out = append(out, keys...)
	}
	slices.Sort(out)
	return out
}

// ppDeleteRow deletes in m, as you do in PP, the transactions of the row of
// the account labelled label that stderr names by how their Note begins
// (docs/tools.md): "schwabtx", the label and ref, or, for a side of a
// transfer, its transfer's head, transfer; each with the other half of its
// pair. It requires them to be what the list says the tool shows of the row
// (was, chainAs): of each record's file, day, type and value. A transfer
// whose other half was deleted already is gone with it.
func ppDeleteRow(t *testing.T, m *ppModel, label, ref, transfer, was string) {
	t.Helper()
	name := "Schwab " + label
	begins := cmp.Or(transfer, label+" "+ref)
	gone := map[*ppTx]bool{}
	for _, o := range []*ppOwner{ppOwnerNamed(m.accounts, name), ppOwnerNamed(m.portfolios, name)} {
		for _, tx := range o.txs {
			// The head of a leg's Note or a two-row cash event's goes on
			// after its row's ref.
			head, _, _ := strings.Cut(strings.TrimPrefix(tx.note, "schwabtx "), ": ")
			if head == begins || strings.HasPrefix(head, begins+" ") {
				gone[tx] = true
				if tx.other != nil {
					gone[tx.other] = true
				}
			}
		}
	}
	types := ppTypes(t)
	for rec := range strings.SplitSeq(was, "; ") {
		f := strings.Fields(rec)
		if len(f) < 4 {
			t.Fatalf("%s %s: %q is not what a PP export shows of a row", label, ref, was)
		}
		owners := m.accounts
		if f[0] == ppPortfolio {
			owners = m.portfolios
		}
		typ, value := types[strings.Join(f[2:len(f)-1], " ")], f[len(f)-1]
		found := false
		for tx := range gone {
			found = found || slices.Contains(owners, tx.owner) && tx.owner.name == name && tx.day == f[1] && tx.typ == typ && formatCents(tx.amount) == value
		}
		if !found && !strings.HasPrefix(typ, "TRANSFER_") {
			t.Errorf("%s holds no %s of %s %s to delete, noted %s", name, rec, label, ref, ref)
		}
	}
	for _, o := range append(slices.Clone(m.accounts), m.portfolios...) {
		o.txs = slices.DeleteFunc(o.txs, func(tx *ppTx) bool { return gone[tx] })
	}
}

// ppApply imports s into m as the how-to has you: it makes in PP the
// accounts that s lists as new (stderr's names line), but those that m
// holds already, as after a redo; deletes in PP what s lists as changed or
// gone; then imports its files in their order, each with the review page set
// to its account, ticking the rows that s lists to tick. It requires PP to
// hold, before the files, what s's replay starts from; to refuse nothing;
// and to mark exactly the rows that the replay says it marks.
func ppApply(t *testing.T, m *ppModel, s *chainStep) {
	t.Helper()
	for _, a := range s.e.Accounts {
		if a.New && ppOwnerNamed(m.accounts, a.Name) == nil {
			m.make(a.Name)
		}
	}
	for _, ch := range s.res.changed {
		ppDeleteRow(t, m, ch.Account, cmp.Or(ch.NoteRef, ch.Ref), ch.Transfer, ch.Was)
	}
	for _, g := range s.res.gone {
		ppDeleteRow(t, m, g.Account, cmp.Or(g.NoteRef, g.Ref), g.Transfer, g.Was)
	}
	if held, start := ppHeld(t, m), ppStart(s); !slices.Equal(held, start) {
		t.Errorf("PP holds %d sides before the files, the replay starts from %d:\n%q\n%q", len(held), len(start), held, start)
	}
	ticks, want := map[string]map[int]bool{}, map[string][]int{}
	for _, mk := range s.marks {
		want[mk.file] = append(want[mk.file], mk.line)
		if mk.tick {
			if ticks[mk.file] == nil {
				ticks[mk.file] = map[int]bool{}
			}
			ticks[mk.file][mk.line] = true
		}
	}
	for _, f := range s.pp {
		if f.kind == ppConfiguration {
			continue
		}
		marked, refused := m.importFile(s.dir, f.name, f.account.name(), ticks[f.name])
		for _, r := range refused {
			t.Error(r)
		}
		if !slices.Equal(marked, want[f.name]) {
			t.Errorf("PP marks the rows %v of %s, the replay %v", marked, f.name, want[f.name])
		}
	}
}

// gfDeleteRow deletes in m, as you do in Ghostfolio, the activities whose
// comment names the row ref of the account labelled label, and requires
// them to be what the list says the tool shows of the row (was, chainAs):
// of each activity's day, type and value.
func gfDeleteRow(t *testing.T, m *gfModel, label, ref, was string) {
	t.Helper()
	comment := "schwabtx " + label + " " + ref
	var gone []gfmActivity
	m.activities = slices.DeleteFunc(m.activities, func(a gfmActivity) bool {
		if a.comment == comment || strings.HasPrefix(a.comment, comment+" leg ") {
			gone = append(gone, a)
			return true
		}
		return false
	})
	var shown []string
	for _, a := range gone {
		value, fee := new(big.Rat).Mul(a.quantity, a.unitPrice), a.fee
		if a.typ == "FEE" {
			value, fee = fee, new(big.Rat)
		}
		w := a.date[:10] + " " + a.typ + " " + formatCents(roundUnits(value, 2).Int64())
		if fee.Sign() != 0 {
			w += " fee " + formatCents(roundUnits(fee, 2).Int64())
		}
		shown = append(shown, w)
	}
	if got := strings.Join(shown, "; "); got != was {
		t.Errorf("Ghostfolio deletes %q of %s %s, which the list shows as %q", got, label, ref, was)
	}
}

// gfApply imports s into m as the how-to has you: it deletes in Ghostfolio
// what s lists as changed or gone, then imports its file through the
// dialog, and returns how many activities m gained, or why it refused the
// file.
func gfApply(t *testing.T, m *gfModel, s *chainStep) (int, error) {
	t.Helper()
	for _, ch := range s.res.changed {
		gfDeleteRow(t, m, ch.Account, ch.Ref, ch.Was)
	}
	for _, g := range s.res.gone {
		gfDeleteRow(t, m, g.Account, g.Ref, g.Was)
	}
	return gfImport(m, s.file)
}

// gfImportDirect imports the file b into the user of m as a direct POST
// /api/v1/import does (import.service.ts:204-1034): as the dialog does
// (gfImport), but for a file with no activity new to the user too, whose
// accounts it makes all the same, with their balances, and whose profiles
// that the user owns take its prices. It returns how many activities the
// user gained, or why Ghostfolio refuses the file.
func gfImportDirect(m *gfModel, b []byte) (int, error) {
	f, err := gfmRead(b)
	if err != nil {
		return 0, err
	}
	if err := gfmSymbols(&f); err != nil {
		return 0, err
	}
	made, err := m.importAccounts(&f)
	if err != nil {
		return 0, err
	}
	owned, toMake := m.importProfiles(&f)
	if err := m.resolve(f, owned, toMake); err != nil {
		return 0, err
	}
	m.accounts = append(m.accounts, made...)
	for _, p := range owned {
		maps.Copy(m.profile(p.symbol).prices, p.prices)
	}
	n := 0
	for _, a := range f.activities {
		if slices.ContainsFunc(m.activities, func(h gfmActivity) bool { return gfmSame(h, a) }) {
			continue
		}
		if a.source == "MANUAL" && m.profile(a.symbol) == nil {
			if i := slices.IndexFunc(toMake, func(p *gfmProfile) bool { return p.symbol == a.symbol }); i >= 0 {
				m.profiles = append(m.profiles, toMake[i])
			}
		}
		m.activities = append(m.activities, a)
		n++
	}
	return n, nil
}

// gfDeleteAccount deletes the account of m named name as Ghostfolio does
// (accounts.controller.ts:62-85): its activities first, by the activities
// page's filter of the account, and then the account, with its balances,
// which Ghostfolio allows only once it has no activity.
func gfDeleteAccount(t *testing.T, m *gfModel, name string) {
	t.Helper()
	i := slices.IndexFunc(m.accounts, func(a *gfmAccount) bool { return a.name == name })
	if i < 0 {
		t.Fatalf("Ghostfolio holds no account %s", name)
	}
	id := m.accounts[i].id
	m.activities = slices.DeleteFunc(m.activities, func(a gfmActivity) bool { return a.account == id })
	m.accounts = slices.Delete(m.accounts, i, i+1)
}

// gfUnshare returns the user of m with the ids and symbols that the
// instance gave in place of file's, which other users held, as file has
// them: each account and profile found by its name. What the user holds is
// then what a user alone on an instance would hold.
func gfUnshare(m *gfModel, file *gfFile) *gfModel {
	ids, symbols := map[string]string{}, map[string]string{}
	for _, a := range m.accounts {
		for _, fa := range file.Accounts {
			if fa.Name == a.name {
				ids[a.id] = fa.ID
			}
		}
	}
	for _, p := range m.profiles {
		for _, fp := range file.AssetProfiles {
			if fp.Name == p.name {
				symbols[p.symbol] = fp.Symbol
			}
		}
	}
	out := newGFModel()
	for _, a := range m.activities {
		a.account, a.symbol = cmp.Or(ids[a.account], a.account), cmp.Or(symbols[a.symbol], a.symbol)
		out.activities = append(out.activities, a)
	}
	return out
}

// rcTrade returns an API trade of account at New York's clock: a buy or a
// sale of quantity millionths of the security of key at price, for amount
// cents, fees fees.
func rcTrade(ref string, account int64, clock, category, key, symbol string, quantity int64, price float64, amount, fees int64) toolRow {
	return toolRow{
		ref: ref, account: account, source: "api", time: tcNY(clock), kind: "TRADE", category: category,
		securityKey: key, symbol: symbol, quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount), fees: centsOf(fees),
	}
}

// rcCash returns an API row of account at New York's clock that moves
// amount cents, booked category.
func rcCash(ref string, account int64, clock, kind, description, category string, amount int64) toolRow {
	return toolRow{ref: ref, account: account, source: "api", time: tcNY(clock), kind: kind, description: description, category: category, amount: centsOf(amount)}
}

// rcFile returns a file row of account posted on posted, of the security of
// key, booked category.
func rcFile(ref string, account int64, posted nytime.Date, action, category, key, symbol string, quantity sql.NullInt64, price float64, amount int64) toolRow {
	r := toolRow{
		ref: ref, account: account, source: "file", posted: posted, kind: action, description: strings.ToUpper(action),
		category: category, securityKey: key, symbol: symbol, quantity: quantity, amount: centsOf(amount),
	}
	if price != 0 {
		r.price = tcPrice(price)
	}
	return r
}

// rcJournal returns an API journal of cash of account, paired with pair of
// pairAccount when pair is not "".
func rcJournal(ref string, account int64, clock, description string, amount int64, pair string, pairAccount int64) toolRow {
	r := rcCash(ref, account, clock, "JOURNAL", description, "journal", amount)
	if pair != "" {
		r.pairKind, r.pairRef, r.pairAccount = "journal", pair, pairAccount
	} else {
		r.flags = []string{"unpaired_journal"}
	}
	return r
}

// rcSnapshot returns a closed-phase snapshot of account of the market date
// day, taken at 18:05 that evening, with cash cents.
func rcSnapshot(account int64, day string, cash int64) toolSnapshot {
	return toolSnapshot{account: account, marketDate: nytime.Date(day), fetchedAt: tcNY(day + " 18:05:00"), cash: centsOf(cash)}
}

// rcPosition returns a position of A1's snapshot of the market date day,
// long quantity millionths worth value dollars.
func rcPosition(day, key string, long int64, value float64) toolPosition {
	return toolPosition{key: key, account: 1, marketDate: nytime.Date(day), fetchedAt: tcNY(day + " 18:05:00"), long: centsOf(long), short: centsOf(0), marketValue: tcPrice(value)}
}

// onlyAccount returns in as an export of the account of id alone reads it:
// its rows, legs, snapshots, waiting and uncounted rows; every account and
// position, which Q1 and Q6 read whatever the export selects.
func onlyAccount(in toolInput, id int64) toolInput {
	in.account = id
	in.rows = slices.DeleteFunc(slices.Clone(in.rows), func(r toolRow) bool { return r.account != id })
	in.legs = slices.DeleteFunc(slices.Clone(in.legs), func(l toolLeg) bool { return l.account != id })
	in.snapshots = slices.DeleteFunc(slices.Clone(in.snapshots), func(s toolSnapshot) bool { return s.account != id })
	in.waiting = slices.DeleteFunc(slices.Clone(in.waiting), func(w toolWaiting) bool { return w.account != id })
	in.uncounted = slices.DeleteFunc(slices.Clone(in.uncounted), func(u toolUncounted) bool { return u.account != id })
	return in
}

// The instants at which the tests' exports are made.
const (
	rcAt1 = "2026-02-01T15:00:00Z"
	rcAt2 = "2026-03-01T15:00:00Z"
	rcAt3 = "2026-04-01T15:00:00Z"
	rcAt4 = "2026-05-01T15:00:00Z"
)

// rcPtrs returns pointers to each of events.
func rcPtrs(events []toolEvent) []*toolEvent {
	out := make([]*toolEvent, len(events))
	for i := range events {
		out[i] = &events[i]
	}
	return out
}

// rcEvents returns the records of the rows of in for tool.
func rcEvents(in toolInput, tool string) []toolEvent {
	in.tool = tool
	events, _ := toolEvents(&in)
	return events
}

// TestToolChain checks the parts of the chain that TestToolReimport's
// sequences rest on (3.8 of the design): the hash; what a row's fp and PP's
// keys hold and leave out; a manifest's chain part read back as it was
// written, and refused when no export writes it; the refusals of an export
// that cannot go on from one; and why a row that an earlier export gave is
// gone.
func TestToolChain(t *testing.T) {
	t.Parallel()

	t.Run("hash", func(t *testing.T) {
		t.Parallel()
		// SHA-256 of "a", U+001F, "b", and of nothing (sha256sum).
		if got := chainHash([]string{"a", "b"}); got != "f04cdced9736a69d" {
			t.Errorf("a, b: %s", got)
		}
		if got := chainHash(nil); got != "e3b0c44298fc1c14" {
			t.Errorf("nothing: %s", got)
		}
	})

	t.Run("fingerprint", func(t *testing.T) {
		t.Parallel()
		// A buy of PQR, whose CUSIP the store knows, and of ABC.B, whose it
		// does not, before and after each was renamed; and a fee on XYZ.
		fee := rcCash("T74100103", 1, "2025-03-03 11:00:00", "JOURNAL", "ADR Fee~XYZ", "fee", -150)
		fee.securityKey, fee.symbol = keyXYZ, "XYZ"
		fp := func(tool string, change func(in *toolInput)) []string {
			in := tcInput(toolInput{rows: []toolRow{
				rcTrade("T74100101", 1, "2025-03-03 10:00:00", "buy", keyRename, "PQR", 1_000_000, 20, -2000, 0),
				rcTrade("T74100102", 1, "2025-03-03 10:00:00", "buy", keyClass, "ABC.B", 1_000_000, 300, -30_000, 0),
				fee,
			}})
			change(&in)
			events := rcEvents(in, tool)
			var out []string
			for _, e := range rcPtrs(events) {
				out = append(out, fingerprint(tool, []*toolEvent{e}))
			}
			return out
		}
		same := func(*toolInput) {}
		for _, c := range []struct {
			what   string
			change func(in *toolInput)
			// pp and gf say which of the rows' fps change, for PP and for
			// Ghostfolio.
			pp, gf [3]bool
		}{
			{"nothing", same, [3]bool{}, [3]bool{}},
			{"another time of the day", func(in *toolInput) { in.rows[0].time = tcNY("2025-03-03 15:30:00") }, [3]bool{}, [3]bool{true, false}},
			{"another description", func(in *toolInput) { in.rows[0].description, in.rows[1].kind = "BOUGHT", "TRADE2" }, [3]bool{}, [3]bool{}},
			{"another name seen first", func(in *toolInput) {
				for i := range in.names {
					if in.names[i].key == keyRename && in.names[i].kind == "description" {
						in.names[i].value += " NEW"
					}
				}
			}, [3]bool{}, [3]bool{}},
			{"the tickers before the renames", func(in *toolInput) {
				in.names = slices.DeleteFunc(in.names, func(n toolName) bool {
					return n.kind == "symbol" && (n.value == "PQRS" || n.key == keyClass)
				})
				in.names = append(in.names, toolName{key: keyClass, kind: "symbol", value: "ABC.A"})
			}, [3]bool{false, true}, [3]bool{true, true}},
			{"another amount", func(in *toolInput) { in.rows[0].amount, in.rows[0].gross = centsOf(-2001), centsOf(-2001) }, [3]bool{true}, [3]bool{true}},
			{"fees", func(in *toolInput) { in.rows[1].fees, in.rows[1].gross = centsOf(5), centsOf(-29_995) }, [3]bool{false, true}, [3]bool{false, true}},
			{"another day", func(in *toolInput) { in.rows[1].time, in.rows[1].day = tcNY("2025-03-04 10:00:00"), "2025-03-04" }, [3]bool{false, true}, [3]bool{false, true}},
			{"another ref", func(in *toolInput) { in.rows[1].ref = "T74100109" }, [3]bool{}, [3]bool{false, true}},
			{"another category", func(in *toolInput) { in.rows[2].category = "tax" }, [3]bool{false, false, true}, [3]bool{false, false, true}},
		} {
			for _, tool := range []string{toolPP, toolGF} {
				want := c.pp
				if tool == toolGF {
					want = c.gf
				}
				base, got := fp(tool, same), fp(tool, c.change)
				for i := range 3 {
					if (base[i] != got[i]) != want[i] {
						t.Errorf("%s, %s: row %d's fp changes %v, want %v", c.what, tool, i, base[i] != got[i], want[i])
					}
				}
			}
		}
	})

	t.Run("keys", func(t *testing.T) {
		t.Parallel()
		// Both records of a transfer have its two legs' keys and are checked
		// by the paying one, which a withdrawal of the paying account of its
		// day and amount shares; a deposit of the receiving account shares
		// the receiving leg's.
		events := rcEvents(toolCaseNamed("transfers").in, toolPP)
		keysOf := func(ref string) ([]string, int) {
			for _, e := range rcPtrs(events) {
				if e.ref == ref {
					return ppKeys(e)
				}
			}
			t.Fatalf("no record of %s", ref)
			return nil, 0
		}
		out, nOut := keysOf("T61001201")
		into, nIn := keysOf("T61001202")
		if len(out) != 2 || !slices.Equal(out, into) || nOut != 1 || nIn != 1 {
			t.Errorf("a transfer's records: %q checked %d, and %q checked %d", out, nOut, into, nIn)
		}
		cash := tcInput(toolInput{rows: []toolRow{
			rcCash("T74200201", 2, "2026-03-02 16:00:00", "ELECTRONIC_FUND", "ACH Debit", "withdrawal", -100_000),
			rcCash("T74200101", 1, "2026-03-02 16:00:00", "ELECTRONIC_FUND", "ACH Credit", "deposit", 100_000),
			rcCash("T74200102", 1, "2026-03-02 16:00:00", "ELECTRONIC_FUND", "ACH Debit", "deposit", -100_000),
		}})
		cashEvents := rcEvents(cash, toolPP)
		w, _ := ppKeys(&cashEvents[0])
		d, _ := ppKeys(&cashEvents[1])
		other, _ := ppKeys(&cashEvents[2])
		if !slices.Equal(w, out[:1]) || !slices.Equal(d, out[1:]) || slices.Contains(out, other[0]) {
			t.Errorf("the withdrawal %q, the deposit %q and the other account's withdrawal %q against the transfer's %q", w, d, other, out)
		}

		// A buy's account side holds its type, value and security, not its
		// shares, and its portfolio side the class of a delivery in.
		buys := tcInput(toolInput{rows: []toolRow{
			rcTrade("T74300101", 1, "2025-05-01 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
			rcTrade("T74300102", 1, "2025-05-01 11:00:00", "buy", keyXYZ, "XYZ", 2_000_000, 25, -5000, 0),
			{
				ref: "T74300103", account: 1, source: "api", time: tcNY("2025-05-01 12:00:00"), kind: "RECEIVE_AND_DELIVER", description: "TRANSFER OF SECURITY",
				amount: centsOf(0), category: "transfer_in", securityKey: keyXYZ, symbol: "XYZ", quantity: centsOf(1_000_000), price: tcPrice(50),
			},
			rcTrade("T74300104", 1, "2025-05-01 13:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 50, -5000, 0),
		}})
		buyEvents := rcEvents(buys, toolPP)
		one, n1 := ppKeys(&buyEvents[0])
		two, _ := ppKeys(&buyEvents[1])
		delivery, nd := ppKeys(&buyEvents[2])
		efgh, _ := ppKeys(&buyEvents[3])
		switch {
		case len(one) != 2 || n1 != 2 || len(delivery) != 1 || nd != 1:
			t.Errorf("a buy's keys %q checked %d, a delivery's %q checked %d", one, n1, delivery, nd)
		case one[0] != two[0] || one[1] == two[1]:
			t.Errorf("two buys of one value and other shares: %q and %q", one, two)
		case delivery[0] != one[1]:
			t.Errorf("a delivery in %q, a buy's portfolio side %q", delivery, one[1])
		case efgh[0] == one[0] || efgh[1] == one[1]:
			t.Errorf("a buy of another security %q, of XYZ %q", efgh, one)
		}
	})

	t.Run("manifest", func(t *testing.T) {
		t.Parallel()
		// The chain part reads back as it was written, for either tool.
		for _, tool := range []string{toolPP, toolGF} {
			s := chainStepOf(t, toolCaseNamed("holdings").in, tool, nil, rcAt1)
			c, err := parseChain(s.manifest)
			if err != nil {
				t.Fatal(err)
			}
			again, err := json.Marshal(c.chainManifest)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(chainJSON(s.res, rcAt1, s.unused))
			if err != nil {
				t.Fatal(err)
			}
			if string(again) != string(want) {
				t.Errorf("%s: read back as\n%s\nwritten as\n%s", tool, again, want)
			}
			if c.ExportedAt != rcAt1 || c.Store != s.in.store || c.Format != tool || c.Layout != ToolLayout || c.Chain.ID != rcAt1 || c.Chain.To != s.in.to {
				t.Errorf("%s: %s %s %s %d, chain %s to %s", tool, c.ExportedAt, c.Store, c.Format, c.Layout, c.Chain.ID, c.Chain.To)
			}
		}

		// The cash row of a two-row event names the shares row that its
		// sale carries, which has no entry of its own.
		two := chainStepOf(t, toolCaseNamed("two_row").in, toolPP, nil, rcAt1)
		if g, ok := two.res.given["A1 ...135"]; g["C0a1b2c3d.2"].With != "C0a1b2c3d.3" || !ok {
			t.Errorf("the cash row's entry %+v", g["C0a1b2c3d.2"])
		} else if _, ok := g["C0a1b2c3d.3"]; ok {
			t.Errorf("the shares row has an entry of its own")
		}

		// What no export writes is refused.
		s := chainStepOf(t, toolCaseNamed("trades").in, toolPP, nil, rcAt1)
		edit := func(change func(m map[string]any)) []byte {
			var m map[string]any
			if err := json.Unmarshal(s.manifest, &m); err != nil {
				t.Fatal(err)
			}
			change(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		entry := func(m map[string]any) map[string]any {
			return m["given"].(map[string]any)["A1 ...135"].(map[string]any)["T61001301"].(map[string]any)
		}
		for _, c := range []struct {
			what     string
			manifest []byte
			want     string
		}{
			{"not JSON", []byte("{"), "not a manifest of a tool export"},
			{"a csv export's", edit(func(m map[string]any) { m["format"] = "csv" }), `its format is "csv"`},
			{"another layout", edit(func(m map[string]any) { m["layout"] = 2 }), "layout 2, not 1"},
			{"a layout as text", edit(func(m map[string]any) { m["layout"] = "1" }), "not a manifest of a tool export"},
			{"no store", edit(func(m map[string]any) { delete(m, "store") }), "names no export, store or chain"},
			{"no chain", edit(func(m map[string]any) { delete(m, "chain") }), "names no export, store or chain"},
			{"no last day", edit(func(m map[string]any) { m["chain"].(map[string]any)["to"] = "2026-11" }), `ends on "2026-11"`},
			{"a short fp", edit(func(m map[string]any) { entry(m)["fp"] = "0123456789abcde" }), "entry of A1 ...135 T61001301"},
			{"an fp in capitals", edit(func(m map[string]any) { entry(m)["fp"] = "0123456789ABCDEF" }), "entry of A1 ...135 T61001301"},
			{"no keys", edit(func(m map[string]any) { delete(entry(m), "keys") }), "entry of A1 ...135 T61001301"},
			{"a bad key", edit(func(m map[string]any) { entry(m)["keys"] = []any{"x"} }), "entry of A1 ...135 T61001301"},
			{"no day", edit(func(m map[string]any) { entry(m)["day"] = "" }), "entry of A1 ...135 T61001301"},
		} {
			if _, err := parseChain(c.manifest); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: %v, want %q", c.what, err, c.want)
			}
		}
		if _, err := parseChain(edit(func(m map[string]any) { delete(entry(m), "keys"); m["format"] = toolGF })); err != nil {
			t.Errorf("a Ghostfolio entry has no keys: %v", err)
		}
	})

	t.Run("shape", func(t *testing.T) {
		t.Parallel()
		// The chain part of a small export of each tool, as chainJSON
		// writes it, in testdata/tools/chain: a buy; a transfer; a
		// deposit of an account with no snapshot, which the holdings
		// check cannot check; a row without a day and a split, left out;
		// a holding that the snapshot holds more of; for Ghostfolio, an
		// account whose snapshot has no cash, and a -manual symbol that
		// nothing uses. -history.update rewrites the files.
		split := rcFile("C7470000.1", 1, "2026-01-07", "Stock Split", "other", keyXYZ, "XYZ", centsOf(1_000_000), 0, 0)
		noDay := toolRow{ref: "T74700103", account: 1, source: "api", kind: "CASH_RECEIPT", description: "Funds Received", amount: centsOf(1000), category: "deposit"}
		in := tcInput(toolInput{
			to: "2026-01-31",
			rows: []toolRow{
				noDay,
				rcTrade("T74700101", 1, "2026-01-05 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
				rcJournal("T74700201", 2, "2026-01-06 09:30:00", "JOURNAL TO ...135", -1000, "T74700102", 1),
				rcJournal("T74700102", 1, "2026-01-06 10:00:00", "JOURNAL FRM ...398", 1000, "T74700201", 2),
				rcCash("T74700301", 3, "2026-01-06 11:00:00", "WIRE_IN", "Wire Incoming", "deposit", 50_000),
				split,
			},
			positions: []toolPosition{rcPosition("2026-01-08", keyXYZ, 3_000_000, 153)},
			snapshots: []toolSnapshot{rcSnapshot(1, "2026-01-08", 100_000), {account: 2, marketDate: "2026-01-08", fetchedAt: tcNY("2026-01-08 18:05:00")}},
		})
		files := map[string][]byte{}
		for _, tool := range []string{toolPP, toolGF} {
			if tool == toolGF {
				in.manual = []string{"ABCD"}
			}
			s := chainStepOf(t, in, tool, nil, rcAt1)
			b, err := json.MarshalIndent(chainJSON(s.res, rcAt1, s.unused), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			files[tool+".json"] = append(b, '\n')
		}
		toolGolden(t, filepath.Join("testdata", "tools", "chain"), files)
	})

	t.Run("refuse", func(t *testing.T) {
		t.Parallel()
		s := chainStepOf(t, toolCaseNamed("cash").in, toolPP, nil, rcAt1)
		c, err := parseChain(s.manifest)
		if err != nil {
			t.Fatal(err)
		}
		in := tcInput(toolInput{tool: toolPP})
		if err := c.refuse(&in, "pp-1"); err != nil {
			t.Errorf("the same store and day: %v", err)
		}
		in.to = "2026-11-13"
		if err := c.refuse(&in, "pp-1"); err != nil {
			t.Errorf("a later day: %v", err)
		}
		in.to = "2026-11-11"
		if err := c.refuse(&in, "pp-1"); err == nil || err.Error() != "-to 2026-11-11 is before the end of the export in pp-1, 2026-11-12" {
			t.Errorf("an earlier day: %v", err)
		}
		in.store, in.to = "2024-02-03T10:00:00.000Z", "2026-11-12"
		if err := c.refuse(&in, "pp-1"); err == nil || err.Error() != "-since pp-1: that export was made from another store (made 2023-05-01T14:00:00.000Z): export in full and redo in the tool (docs/tools.md)" {
			t.Errorf("another store: %v", err)
		}
		in.tool = toolGF
		if err := c.refuse(&in, "pp-1"); err == nil || err.Error() != "-since pp-1: "+filepath.Join("pp-1", "manifest.json")+" is not the manifest of a -format ghostfolio export of layout 1" {
			t.Errorf("another format: %v", err)
		}
	})

	t.Run("account and manual", func(t *testing.T) {
		t.Parallel()
		c := &toolChain{}
		for _, a := range []struct {
			account *string
			id      int64
			ok      bool
		}{{nil, 0, true}, {new("A2"), 2, true}, {new("A0"), 0, false}, {new("2"), 0, false}, {new("B2"), 0, false}, {new("A2x"), 0, false}} {
			c.Chain.Account = a.account
			if id, err := c.account(); id != a.id || (err == nil) != a.ok {
				t.Errorf("%v: %d, %v", a.account, id, err)
			}
		}
		c.Chain.Manual = []chainManual{{Symbol: "ABCD", Unused: true}, {Symbol: "xyz"}}
		if got := c.manual([]string{" XYZ", "abcd ", "EFGH", "", "efgh"}); !slices.Equal(got, []string{"ABCD", "xyz", "EFGH"}) {
			t.Errorf("the chain's -manual and more: %q", got)
		}
		if got := (*toolChain)(nil).manual([]string{"ABC-B", " "}); !slices.Equal(got, []string{"ABC-B"}) {
			t.Errorf("-manual alone: %q", got)
		}
	})

	t.Run("before", func(t *testing.T) {
		t.Parallel()
		// A row left out again for the same reason was listed before; one
		// left out for another, not.
		other := func(ref string) toolRow {
			r := rcFile(ref, 1, "2025-07-01", "Misc Cash Entry", "other", "", "", sql.NullInt64{}, 0, 100)
			r.flags = []string{"other_action"}
			return r
		}
		first := tcInput(toolInput{rows: []toolRow{other("C7450000.1"), other("C7450000.2")}})
		later := first
		later.rows = slices.Clone(first.rows)
		later.rows[0].category = "buy"
		e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
		e2 := chainStepOf(t, later, toolPP, e1, rcAt2)
		var got []string
		for _, l := range chainJSON(e2.res, rcAt2, nil).LeftOut {
			got = append(got, fmt.Sprintf("%s %s %v", l.Ref, l.Reason, l.Before))
		}
		if want := []string{"C7450000.1 no_quantity false", "C7450000.2 other true"}; !slices.Equal(got, want) {
			t.Errorf("left out %q, want %q", got, want)
		}
	})

	t.Run("symbols changed", func(t *testing.T) {
		t.Parallel()
		// PQR is renamed PQRS between two exports: with -since, it alone
		// of what the export writes changed its identifiers; a full export
		// names it for its two tickers.
		before := func(in toolInput) toolInput {
			in.names = slices.DeleteFunc(slices.Clone(in.names), func(n toolName) bool { return n.kind == "symbol" && n.value == "PQRS" })
			return in
		}
		rows := []toolRow{
			rcTrade("T74600101", 1, "2025-08-01 10:00:00", "buy", keyRename, "PQR", 1_000_000, 20, -2000, 0),
			rcTrade("T74600102", 1, "2025-08-01 11:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
		}
		first := before(tcInput(toolInput{rows: slices.Clone(rows)}))
		later := tcInput(toolInput{rows: append(rows,
			rcTrade("T74600103", 1, "2025-09-01 10:00:00", "sell", keyRename, "PQRS", -1_000_000, 25, 2500, 0),
			rcTrade("T74600104", 1, "2025-09-01 11:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 51, -5100, 0))})
		keys := func(ss []*toolSecurity) []string {
			var out []string
			for _, s := range ss {
				out = append(out, s.key)
			}
			return out
		}
		for _, tool := range []string{toolPP, toolGF} {
			e1 := chainStepOf(t, first, tool, nil, rcAt1)
			e2 := chainStepOf(t, later, tool, e1, rcAt2)
			full := chainStepOf(t, later, tool, nil, rcAt2)
			if got := keys(e1.res.symbolsChanged); len(got) > 0 {
				t.Errorf("%s, the first export: %q", tool, got)
			}
			if got := keys(e2.res.symbolsChanged); !slices.Equal(got, []string{keyRename}) {
				t.Errorf("%s, with -since: %q", tool, got)
			}
			if got := keys(full.res.symbolsChanged); !slices.Equal(got, []string{keyRename}) {
				t.Errorf("%s, a full export: %q", tool, got)
			}
			var secs []string
			for _, s := range chainJSON(e2.res, rcAt2, nil).Securities {
				secs = append(secs, s.SecurityKey+" "+cmp.Or(s.Ticker, s.GFSymbol))
			}
			if !slices.Equal(secs, []string{keyRename + " PQRS", keyXYZ + " XYZ"}) {
				t.Errorf("%s: the chain's securities %q", tool, secs)
			}
		}
	})

	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		// Each row that the earlier export gave goes for its own reason.
		// A shares row booked as a sale by your category, which the cash row
		// of its event, come later, carries once the category is undone.
		shares := rcFile("C7440000.3", 1, "2025-06-04", "Cash Merger Adj", "sell", keyXYZ, "XYZ", centsOf(-1_000_000), 0, 0)
		shares.flags = []string{"two_row_event"}
		cash := rcFile("C7440000.4", 1, "2025-06-04", "Cash Merger", "sell", keyXYZ, "XYZ", sql.NullInt64{}, 0, 5000)
		cash.flags = []string{"two_row_event"}
		first := tcInput(toolInput{
			rows: []toolRow{
				rcTrade("T74400101", 1, "2025-06-05 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
				rcTrade("T74400102", 1, "2025-06-02 11:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5001, 0),
				rcTrade("T74400103", 1, "2025-06-02 12:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5002, 0),
				rcTrade("T74400104", 1, "2025-06-02 13:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5003, 0),
				rcCash("T74400105", 1, "2025-06-02 14:00:00", "DIVIDEND_OR_INTEREST", "Ordinary Dividend~XYZ", "dividend", 300),
				rcCash("T74400106", 1, "2025-06-02 15:00:00", "DIVIDEND_OR_INTEREST", "Ordinary Dividend~XYZ", "dividend", 400),
				rcTrade("T74400107", 1, "2025-06-02 16:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5004, 0),
				rcCash("T74400108", 1, "2025-06-03 10:00:00", "JOURNAL", "SERVICE FEE", "fee", -250),
				shares,
			},
			snapshots: []toolSnapshot{rcSnapshot(1, "2025-06-06", 100_000)},
		})
		for i := 4; i < 6; i++ {
			first.rows[i].securityKey, first.rows[i].symbol = keyXYZ, "XYZ"
		}
		later := first
		later.rows = slices.Clone(first.rows)
		later.rows[2].quantity = sql.NullInt64{} // no_quantity
		later.rows[4].category = "deposit"       // now cash
		later.rows[5].day = ""                   // no_day
		later.rows = slices.DeleteFunc(later.rows, func(r toolRow) bool {
			return r.ref == "T74400101" || r.ref == "T74400102" || r.ref == "T74400104" || r.ref == "T74400108"
		})
		later.rows[len(later.rows)-1].category = "other"
		later.rows = append(later.rows, tcInput(toolInput{rows: []toolRow{cash}}).rows[0])
		later.uncounted = []toolUncounted{
			{ref: "T74400101", account: 1, why: "user"},
			{ref: "T74400102", account: 1, why: "linked", matchedWith: "C7440000.1,C7440000.2"},
			{ref: "T74400108", account: 1, why: "user"},
		}
		for _, c := range []struct {
			tool string
			want []string
		}{
			{toolPP, []string{
				"T74400102 linked to C7440000.1, C7440000.2: portfolio 2025-06-02 Buy 50.01",
				"T74400103 left out: no_quantity: portfolio 2025-06-02 Buy 50.02",
				"T74400104 no longer in the store: portfolio 2025-06-02 Buy 50.03",
				"T74400106 left out: no_day: account 2025-06-02 Dividend 4.00",
				"T74400108 uncounted: user: account 2025-06-03 Fees 2.50",
				"C7440000.3 combined with C7440000.4: portfolio 2025-06-04 Sell 0.00",
				"T74400101 uncounted: user: portfolio 2025-06-05 Buy 50.00",
			}},
			{toolGF, []string{
				"T74400102 linked to C7440000.1, C7440000.2: 2025-06-02 BUY 50.01",
				"T74400103 left out: no_quantity: 2025-06-02 BUY 50.02",
				"T74400104 no longer in the store: 2025-06-02 BUY 50.03",
				"T74400105 now cash, in the balances: 2025-06-02 DIVIDEND 3.00",
				"T74400106 left out: no_day: 2025-06-02 DIVIDEND 4.00",
				"T74400108 uncounted: user: 2025-06-03 FEE 2.50",
				"C7440000.3 combined with C7440000.4: 2025-06-04 SELL 0.00",
				"T74400101 uncounted: user: 2025-06-05 BUY 50.00",
			}},
		} {
			e1 := chainStepOf(t, first, c.tool, nil, rcAt1)
			e2 := chainStepOf(t, later, c.tool, e1, rcAt2)
			var got []string
			for _, g := range e2.res.gone {
				got = append(got, g.Ref+" "+g.Why+": "+g.Was)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("%s: gone %q, want %q", c.tool, got, c.want)
			}
			if given := e2.refs(outcomeGiven); !slices.Equal(given, []string{"T74400107"}) {
				t.Errorf("%s: given %q", c.tool, given)
			}
		}
	})
}

// rcSequence1 returns the store of sequence 1 when it is first exported, up
// to 2026-01-31, and a month later: a trade and a deposit of the other
// account revised, a file buy replaced by its API row, alike in all PP
// keeps, and a file sale replaced by its API row whose fees differ; a
// journal paired a day after its first side went in by its sign; a
// corrected dividend uncounted, and a dividend booked as a deposit; a
// held-out row decided to count, a row "as of" a day already exported, and
// an API row of the first export's last day that came late.
func rcSequence1() (first, later toolInput) {
	sell := rcFile("C7a0b1c2d.2", 1, "2026-01-07", "Sell", "sell", keyXYZ, "XYZ", centsOf(-4_000_000), 55, 21_997)
	sell.fees = centsOf(3)
	dividend := func(ref string, amount int64) toolRow {
		r := rcFile(ref, 1, "2026-01-12", "Qualified Dividend", "dividend", keyEFGH, "EFGH", sql.NullInt64{}, 0, amount)
		r.flags = []string{"near_duplicate"}
		return r
	}
	paid := rcCash("T71000104", 1, "2026-01-15 11:00:00", "DIVIDEND_OR_INTEREST", "Ordinary Dividend~XYZ", "dividend", 4000)
	paid.securityKey, paid.symbol = keyXYZ, "XYZ"
	first = tcInput(toolInput{
		to: "2026-01-31",
		rows: []toolRow{
			rcCash("T71000101", 1, "2026-01-05 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000),
			rcCash("T71000201", 2, "2026-01-05 10:30:00", "WIRE_IN", "Wire Incoming", "deposit", 500_000),
			rcTrade("T71000102", 1, "2026-01-06 10:30:00", "buy", keyXYZ, "XYZ", 10_000_000, 50, -50_065, 65),
			rcFile("C7a0b1c2d.1", 1, "2026-01-07", "Buy", "buy", keyEFGH, "EFGH", centsOf(2_000_000), 30, -6000),
			sell,
			rcJournal("T71000103", 1, "2026-01-08 11:00:00", "JOURNAL TO ...398", -30_000, "", 0),
			dividend("C7a0b1c2d.3", 2500), dividend("C7a0b1c2d.4", 2450),
			paid,
		},
		positions: []toolPosition{
			rcPosition("2026-01-30", keyXYZ, 6_000_000, 306), rcPosition("2026-01-30", keyEFGH, 2_000_000, 62),
		},
		snapshots: []toolSnapshot{rcSnapshot(1, "2026-01-30", 1_040_000), rcSnapshot(2, "2026-01-30", 500_000)},
		waiting:   []toolWaiting{{ref: "C7a0b1c2d.5", account: 1, day: "2026-01-20", kind: "Bank Interest", amount: centsOf(37), why: "held_out"}},
	})

	revised := rcTrade("T71000102", 1, "2026-01-06 10:30:00", "buy", keyXYZ, "XYZ", 10_000_000, 50, -50_095, 95)
	lastDay := rcCash("T71000108", 1, "2026-01-31 10:00:00", "DIVIDEND_OR_INTEREST", "Credit Interest", "interest", 55)
	booked := paid
	booked.category = "deposit"
	asOf := rcFile("C7a0b1c2d.6", 1, "2026-02-03", "Qualified Dividend", "dividend", keyXYZ, "XYZ", sql.NullInt64{}, 0, 1200)
	asOf.day = "2026-01-20"
	later = tcInput(toolInput{
		to: "2026-02-27",
		rows: []toolRow{
			rcCash("T71000101", 1, "2026-01-05 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000),
			rcCash("T71000201", 2, "2026-01-05 10:30:00", "WIRE_IN", "Wire Incoming", "deposit", 500_100),
			revised,
			rcTrade("T71000105", 1, "2026-01-07 10:00:00", "buy", keyEFGH, "EFGH", 2_000_000, 30, -6000, 0),
			rcTrade("T71000106", 1, "2026-01-07 14:00:00", "sell", keyXYZ, "XYZ", -4_000_000, 55, 21_997, 5),
			rcJournal("T71000103", 1, "2026-01-08 11:00:00", "JOURNAL TO ...398", -30_000, "T71000202", 2),
			rcJournal("T71000202", 2, "2026-01-09 09:00:00", "JOURNAL FRM ...135", 30_000, "T71000103", 1),
			dividend("C7a0b1c2d.3", 2500),
			booked,
			rcFile("C7a0b1c2d.5", 1, "2026-01-20", "Bank Interest", "interest", "", "", sql.NullInt64{}, 0, 37),
			asOf,
			lastDay,
			rcTrade("T71000107", 1, "2026-02-10 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 51, -5100, 0),
		},
		positions: append(slices.Clone(first.positions),
			rcPosition("2026-02-26", keyXYZ, 7_000_000, 364), rcPosition("2026-02-26", keyEFGH, 2_000_000, 64)),
		snapshots: append(slices.Clone(first.snapshots), rcSnapshot(1, "2026-02-26", 1_028_934), rcSnapshot(2, "2026-02-26", 530_000)),
		uncounted: []toolUncounted{
			{ref: "C7a0b1c2d.1", account: 1, why: "linked", matchedWith: "T71000105"},
			{ref: "C7a0b1c2d.2", account: 1, why: "linked", matchedWith: "T71000106"},
			{ref: "C7a0b1c2d.4", account: 1, why: "user"},
		},
	})
	return first, later
}

// rcSequence4 returns the store of sequence 4, two accounts up to
// 2026-03-06: a transfer from A2 to A1, an ACH withdrawal of its day and
// amount from A2 and a deposit of them into A1, and a withdrawal of A2
// three days later; and the same store once a journal pair of A2 to A1,
// equal to that withdrawal, has come late.
func rcSequence4() (first, later toolInput) {
	rows := []toolRow{
		rcJournal("T74000201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", -100_000, "T74000101", 1),
		rcJournal("T74000101", 1, "2026-03-02 10:00:00", "JOURNAL FRM ...398", 100_000, "T74000201", 2),
		rcCash("T74000202", 2, "2026-03-02 11:00:00", "ELECTRONIC_FUND", "ACH Debit", "withdrawal", -100_000),
		rcCash("T74000102", 1, "2026-03-02 12:00:00", "ELECTRONIC_FUND", "ACH Credit", "deposit", 100_000),
		rcCash("T74000203", 2, "2026-03-05 09:00:00", "ELECTRONIC_FUND", "ACH Debit", "withdrawal", -50_000),
	}
	snapshots := []toolSnapshot{rcSnapshot(1, "2026-03-06", 200_000), rcSnapshot(2, "2026-03-06", -150_000)}
	first = tcInput(toolInput{to: "2026-03-06", rows: slices.Clone(rows), snapshots: snapshots})
	rows = append(rows,
		rcJournal("T74000204", 2, "2026-03-05 10:00:00", "JOURNAL TO ...135", -50_000, "T74000103", 1),
		rcJournal("T74000103", 1, "2026-03-05 10:05:00", "JOURNAL FRM ...398", 50_000, "T74000204", 2),
	)
	later = tcInput(toolInput{to: "2026-03-06", rows: rows, snapshots: snapshots})
	return first, later
}

// rcMarks returns the marks of s as file:line, and those to tick with a
// "tick" after them.
func rcMarks(s *chainStep) []string {
	var out []string
	for _, m := range s.marks {
		w := fmt.Sprintf("%s:%d %s", m.file, m.line, m.record.ref)
		if m.tick {
			w += " tick"
		}
		out = append(out, w)
	}
	return out
}

// rcChanged returns the changed rows of s as ref and was, and its gone rows
// as ref, why and was.
func rcChanged(s *chainStep) (changed, gone []string) {
	for _, c := range s.res.changed {
		changed = append(changed, c.Ref+": "+c.Was+" -> "+c.Now)
	}
	for _, g := range s.res.gone {
		gone = append(gone, g.Ref+" "+g.Why+": "+g.Was)
	}
	return changed, gone
}

// TestToolReimport chains exports and imports through the models of PP
// and Ghostfolio (11.5 of the design): each export goes on from the one
// before by its manifest's chain part, is imported as the how-to says, with
// what it lists deleted first and what it lists ticked, and each sequence
// ends with the models' cash and shares per account equal to the store's.
// Beside the design's eleven sequences, two split a transfer by -to.
func TestToolReimport(t *testing.T) {
	t.Parallel()

	t.Run("1 a later state", func(t *testing.T) {
		t.Parallel()
		first, later := rcSequence1()
		for _, c := range []struct {
			tool                                string
			written, given, changed, gone, same []string
			late                                int
		}{
			{
				toolPP,
				[]string{"T71000201", "T71000102", "T71000105", "T71000106", "T71000103", "T71000202", "T71000104", "C7a0b1c2d.5", "C7a0b1c2d.6", "T71000108", "T71000107"},
				[]string{"T71000101", "C7a0b1c2d.3"},
				[]string{
					"T71000102: portfolio 2026-01-06 Buy 500.65 -> portfolio 2026-01-06 Buy 500.95",
					"T71000103: account 2026-01-08 Withdrawal 300.00 -> account 2026-01-08 Transfer (Outbound) 300.00",
					"T71000104: account 2026-01-15 Dividend 40.00 -> account 2026-01-15 Deposit 40.00",
					"T71000201: account 2026-01-05 Deposit 5000.00 -> account 2026-01-05 Deposit 5001.00",
				},
				[]string{
					"C7a0b1c2d.2 linked to T71000106: portfolio 2026-01-07 Sell 219.97",
					"C7a0b1c2d.4 uncounted: user: account 2026-01-12 Dividend 24.50",
				},
				// T71000105, the same as the file row gone, did not come late.
				[]string{"A1 ...135 C7a0b1c2d.1 T71000105"},
				5,
			},
			{
				toolGF,
				[]string{"T71000102", "T71000105", "T71000106", "C7a0b1c2d.5", "C7a0b1c2d.6", "T71000108", "T71000107"},
				[]string{"C7a0b1c2d.3"},
				[]string{"T71000102: 2026-01-06 BUY 500.00 fee 0.65 -> 2026-01-06 BUY 500.00 fee 0.95"},
				[]string{
					"C7a0b1c2d.1 linked to T71000105: 2026-01-07 BUY 60.00",
					"C7a0b1c2d.2 linked to T71000106: 2026-01-07 SELL 220.00 fee 0.03",
					"C7a0b1c2d.4 uncounted: user: 2026-01-12 DIVIDEND 24.50",
					"T71000104 now cash, in the balances: 2026-01-15 DIVIDEND 40.00",
				},
				nil,
				5,
			},
		} {
			e1 := chainStepOf(t, first, c.tool, nil, rcAt1)
			e2 := chainStepOf(t, later, c.tool, e1, rcAt2)
			changed, gone := rcChanged(e2)
			var same []string
			for _, p := range e2.res.sameAs {
				same = append(same, p.Account+" "+p.Gone+" "+p.New)
			}
			for _, d := range []struct {
				what      string
				got, want []string
			}{
				{"written", e2.refs(outcomeWritten), c.written},
				{"given", e2.refs(outcomeGiven), c.given},
				{"changed", changed, c.changed},
				{"gone", gone, c.gone},
				{"same_as", same, c.same},
			} {
				if !slices.Equal(d.got, d.want) {
					t.Errorf("%s: %s %q, want %q", c.tool, d.what, d.got, d.want)
				}
			}
			if e2.res.late != c.late {
				t.Errorf("%s: %d rows came late, want %d", c.tool, e2.res.late, c.late)
			}
			if part := chainJSON(e2.res, rcAt2, nil); part.Chain.ID != rcAt1 || part.Chain.To != "2026-02-27" {
				t.Errorf("%s: the chain %s up to %s", c.tool, part.Chain.ID, part.Chain.To)
			}

			if c.tool == toolPP {
				m := newPPModel()
				ppApply(t, m, e1)
				ppApply(t, m, e2)
				// The new row's entry keeps the ref of the Note that PP
				// holds, the transfer's two records name one transaction,
				// and the next export, which writes nothing, starts from
				// what PP holds.
				if g := e2.res.given["A1 ...135"]["T71000105"]; g.NoteRef != "C7a0b1c2d.1" {
					t.Errorf("T71000105's entry %+v", g)
				}
				if a, b := e2.res.given["A1 ...135"]["T71000103"].Transfer, e2.res.given["A2 ...398"]["T71000202"].Transfer; a != "A1 ...135 T71000103 to A2 ...398 T71000202" || a != b {
					t.Errorf("the transfer's records are of %q and %q", a, b)
				}
				if got := rcMarks(e2); !slices.Equal(got, []string{"portfolio-transactions-A1-135.csv:2 T71000105", "account-transactions-A2-398.csv:2 T71000202"}) {
					t.Errorf("PP marks %q", got)
				}
				ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
				e3 := chainStepOf(t, later, c.tool, e2, rcAt3)
				if w := e3.refs(outcomeWritten); len(w) > 0 || len(e3.res.changed)+len(e3.res.gone) > 0 {
					t.Errorf("the next export writes %q, lists %v and %v", w, e3.res.changed, e3.res.gone)
				}
				ppApply(t, m, e3)
				ppReconcile(t, m, e3.in, e3.events, e3.outcomes)

				// The buy that PP holds under the file row's Note is revised:
				// it is deleted by that Note, and its new form is its own.
				fourth := later
				fourth.rows = slices.Clone(later.rows)
				for i := range fourth.rows {
					if fourth.rows[i].ref == "T71000105" {
						fourth.rows[i].amount, fourth.rows[i].gross = centsOf(-6010), centsOf(-6010)
					}
				}
				e4 := chainStepOf(t, fourth, c.tool, e3, rcAt4)
				if len(e4.res.changed) != 1 || e4.res.changed[0].NoteRef != "C7a0b1c2d.1" || e4.res.given["A1 ...135"]["T71000105"].NoteRef != "" {
					t.Errorf("the revised buy: changed %+v, its entry %+v", e4.res.changed, e4.res.given["A1 ...135"]["T71000105"])
				}
				ppApply(t, m, e4)
				ppReconcile(t, m, e4.in, e4.events, e4.outcomes)
				continue
			}
			m := newGFModel()
			if _, err := gfApply(t, m, e1); err != nil {
				t.Fatal(err)
			}
			if n, err := gfApply(t, m, e2); err != nil || n != len(c.written) {
				t.Errorf("Ghostfolio imports %d activities, %v", n, err)
			}
			gfReconcile(t, e2.in, e2.events, e2.outcomes, m)
		}
	})

	t.Run("2 the same export twice", func(t *testing.T) {
		t.Parallel()
		first, _ := rcSequence1()
		pp := chainStepOf(t, first, toolPP, nil, rcAt1)
		m := newPPModel()
		ppApply(t, m, pp)
		cash := m.cash("Schwab A1 ...135")
		for _, f := range pp.pp {
			if f.kind == ppConfiguration || f.kind == ppSecurities {
				continue
			}
			marked, refused := m.importFile(pp.dir, f.name, f.account.name(), nil)
			if len(refused) > 0 || len(marked) != f.rows {
				t.Errorf("%s again: PP marks %v of %d rows, refuses %v", f.name, marked, f.rows, refused)
			}
		}
		if got := m.cash("Schwab A1 ...135"); got != cash {
			t.Errorf("A1 holds %s, held %s", formatCents(got), formatCents(cash))
		}
		ppReconcile(t, m, pp.in, pp.events, pp.outcomes)

		gf := chainStepOf(t, first, toolGF, nil, rcAt1)
		g := newGFModel()
		if _, err := gfApply(t, g, gf); err != nil {
			t.Fatal(err)
		}
		activities := len(g.activities)
		if n, err := gfImport(g, gf.file); err == nil || len(g.activities) != activities {
			t.Errorf("imported again, %d activities new, %d in all, were %d", n, len(g.activities), activities)
		}
		gfReconcile(t, gf.in, gf.events, gf.outcomes, g)
	})

	t.Run("3 two identical trades", func(t *testing.T) {
		t.Parallel()
		one := rcTrade("T73000101", 1, "2026-03-02 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0)
		two := rcTrade("T73000102", 1, "2026-03-02 14:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0)
		two.flags = []string{"late_api_row"}
		deposit := rcCash("T73000100", 1, "2026-03-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 100_000)
		first := tcInput(toolInput{to: "2026-03-06", rows: []toolRow{deposit, one}})
		later := tcInput(toolInput{to: "2026-03-06", rows: []toolRow{deposit, one, two}})
		e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
		e2 := chainStepOf(t, later, toolPP, e1, rcAt2)
		if got := rcMarks(e2); !slices.Equal(got, []string{"portfolio-transactions-A1-135.csv:1 T73000102 tick"}) {
			t.Errorf("PP marks %q", got)
		}
		if got := ppTickList(e2.marks); len(got) != 1 || got[0] != (chainTick{
			File: "portfolio-transactions-A1-135.csv", Ref: "T73000102", Account: "A1 ...135", As: "portfolio 2026-03-02 Buy 50.00",
		}) {
			t.Errorf("the tick list %+v", got)
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)

		// Left unticked, the second trade is lost.
		lost := newPPModel()
		ppApply(t, lost, e1)
		for _, f := range e2.pp {
			if _, refused := lost.importFile(e2.dir, f.name, f.account.name(), nil); len(refused) > 0 && f.kind != ppConfiguration {
				t.Error(refused)
			}
		}
		if got, want := lost.cash("Schwab A1 ...135"), m.cash("Schwab A1 ...135")+5000; got != want {
			t.Errorf("unticked, A1 holds %s, not %s", formatCents(got), formatCents(want))
		}
	})

	t.Run("4 a transfer beside an equal withdrawal", func(t *testing.T) {
		t.Parallel()
		first, later := rcSequence4()
		e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
		if got := rcMarks(e1); !slices.Equal(got, []string{"account-transactions-A2-398.csv:1 T74000201", "account-transactions-A2-398.csv:2 T74000202 tick"}) {
			t.Errorf("the full export: PP marks %q", got)
		}
		e2 := chainStepOf(t, later, toolPP, e1, rcAt2)
		if got := rcMarks(e2); !slices.Equal(got, []string{"account-transactions-A1-135.csv:1 T74000103 tick", "account-transactions-A2-398.csv:1 T74000204"}) {
			t.Errorf("the late pair: PP marks %q", got)
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppReconcile(t, m, e1.in, e1.events, e1.outcomes)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)

		// Then the late pair's paying side is uncounted: its other side is
		// a deposit by its sign, changed, and the paying side gone; the
		// transfer, deleted once, goes with both.
		third := later
		third.rows = slices.DeleteFunc(slices.Clone(later.rows), func(r toolRow) bool { return r.ref == "T74000204" })
		for i := range third.rows {
			if third.rows[i].ref == "T74000103" {
				third.rows[i].partner.counted = false
			}
		}
		third.uncounted = []toolUncounted{{ref: "T74000204", account: 2, why: "user"}}
		e3 := chainStepOf(t, third, toolPP, e2, rcAt3)
		changed, gone := rcChanged(e3)
		if !slices.Equal(changed, []string{"T74000103: account 2026-03-05 Transfer (Inbound) 500.00 -> account 2026-03-05 Deposit 500.00"}) ||
			!slices.Equal(gone, []string{"T74000204 uncounted: user: account 2026-03-05 Transfer (Outbound) 500.00"}) || len(e3.marks) > 0 {
			t.Errorf("a side of the pair uncounted: changed %q, gone %q, marks %q", changed, gone, rcMarks(e3))
		}
		ppApply(t, m, e3)
		ppReconcile(t, m, e3.in, e3.events, e3.outcomes)
	})

	t.Run("a transfer split by -to", func(t *testing.T) {
		t.Parallel()
		// The first export ends on the paying side's day: it writes the
		// transfer from that side alone, beside a withdrawal of its day and
		// amount in the same file, which PP does not check it against; the
		// receiving side, written the next time, is of the transaction PP
		// holds, which the withdrawal does not make it new.
		pays := rcJournal("T74100201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", -100_000, "T74100101", 1)
		receives := rcJournal("T74100101", 1, "2026-03-03 08:00:00", "JOURNAL FRM ...398", 100_000, "T74100201", 2)
		withdrawal := rcCash("T74100202", 2, "2026-03-02 11:00:00", "ELECTRONIC_FUND", "ACH Debit", "withdrawal", -100_000)
		first := tcInput(toolInput{to: "2026-03-02", rows: []toolRow{pays, withdrawal}})
		first.rows[0].partner = toolPartner{
			day: "2026-03-03", time: receives.time, amount: receives.amount, counted: true, category: "journal",
			kind: receives.kind, description: receives.description,
		}
		later := tcInput(toolInput{to: "2026-03-06", rows: []toolRow{pays, receives, withdrawal}})
		e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
		if got := rcMarks(e1); len(got) > 0 {
			t.Errorf("the first export: PP marks %q", got)
		}
		e2 := chainStepOf(t, later, toolPP, e1, rcAt2)
		if got := rcMarks(e2); !slices.Equal(got, []string{"account-transactions-A1-135.csv:1 T74100101"}) || !slices.Equal(e2.refs(outcomeWritten), []string{"T74100101"}) {
			t.Errorf("the next: written %q, PP marks %q", e2.refs(outcomeWritten), got)
		}
		if a, b := e2.res.given["A2 ...398"]["T74100201"].Transfer, e2.res.given["A1 ...135"]["T74100101"].Transfer; a != "A2 ...398 T74100201 to A1 ...135 T74100101" || a != b {
			t.Errorf("the transfer's records are of %q and %q", a, b)
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
		// And the export after, which writes nothing, starts from the one
		// transaction that both records name.
		e3 := chainStepOf(t, later, toolPP, e2, rcAt3)
		ppApply(t, m, e3)
	})

	t.Run("a transfer split by -to, its paying side under a new ref", func(t *testing.T) {
		t.Parallel()
		// The paying side, a file row, is written alone; then it is linked
		// to its API row, the same in all that PP keeps; and then the
		// receiving side comes. PP holds one transfer, which the receiving
		// side's record is of.
		receives := rcJournal("T74200101", 1, "2026-03-03 08:00:00", "JOURNAL FRM ...398", 100_000, "T74200201", 2)
		partner := toolPartner{
			day: "2026-03-03", time: receives.time, amount: receives.amount, counted: true, category: "journal",
			kind: receives.kind, description: receives.description,
		}
		file := rcFile("C74200001.1", 2, "2026-03-02", "Journal", "journal", "", "", sql.NullInt64{}, 0, -100_000)
		file.pairKind, file.pairRef, file.pairAccount, file.partner = "journal", "T74200101", 1, partner
		api := rcJournal("T74200201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", -100_000, "T74200101", 1)
		api.partner = partner
		s1 := tcInput(toolInput{to: "2026-03-02", rows: []toolRow{file}})
		s2 := tcInput(toolInput{to: "2026-03-02", rows: []toolRow{api}, uncounted: []toolUncounted{{ref: "C74200001.1", account: 2, why: "linked", matchedWith: "T74200201"}}})
		api.partner = toolPartner{}
		s3 := tcInput(toolInput{to: "2026-03-06", rows: []toolRow{api, receives}, uncounted: s2.uncounted})
		e1 := chainStepOf(t, s1, toolPP, nil, rcAt1)
		e2 := chainStepOf(t, s2, toolPP, e1, rcAt2)
		e3 := chainStepOf(t, s3, toolPP, e2, rcAt3)
		for i, c := range []struct {
			step          *chainStep
			written       []string
			marks, sameAs []string
		}{
			{e2, []string{"T74200201"}, []string{"account-transactions-A2-398.csv:1 T74200201"}, []string{"C74200001.1 T74200201"}},
			{e3, []string{"T74200101"}, []string{"account-transactions-A1-135.csv:1 T74200101"}, nil},
		} {
			var same []string
			for _, p := range c.step.res.sameAs {
				same = append(same, p.Gone+" "+p.New)
			}
			if w, m := c.step.refs(outcomeWritten), rcMarks(c.step); !slices.Equal(w, c.written) || !slices.Equal(m, c.marks) || !slices.Equal(same, c.sameAs) {
				t.Errorf("export %d: written %q, PP marks %q, the same %q", i+2, w, m, same)
			}
		}
		if a, b := e3.res.given["A2 ...398"]["T74200201"], e3.res.given["A1 ...135"]["T74200101"]; a.NoteRef != "C74200001.1" || a.Transfer != "A2 ...398 C74200001.1 to A1 ...135 T74200101" || b.Transfer != a.Transfer {
			t.Errorf("the transfer's entries %+v and %+v", a, b)
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3} {
			ppApply(t, m, s)
		}
		ppReconcile(t, m, e3.in, e3.events, e3.outcomes)
	})

	t.Run("a transfer split by -to, of an account with no other row", func(t *testing.T) {
		t.Parallel()
		// The side of the transfer that the first export holds is written
		// as the transfer, which names the other account although that
		// account has no row up to -to: the export names it to make, and
		// PP books both sides at the paying side's day. The other side,
		// written the next time, is of the transaction that PP holds.
		deposit := rcCash("T74300100", 1, "2026-11-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 500_000)
		for _, c := range []struct {
			what          string
			first, second toolRow
			next          string
		}{
			{
				"the paying side first",
				rcJournal("T74300101", 1, "2026-11-12 15:00:00", "JOURNAL TO ...783", -100_000, "T74300301", 3),
				rcJournal("T74300301", 3, "2026-11-13 09:00:00", "JOURNAL FRM ...135", 100_000, "T74300101", 1),
				"account-transactions-A3-783.csv:1 T74300301",
			},
			{
				"the receiving side first",
				rcJournal("T74300102", 1, "2026-11-12 15:00:00", "JOURNAL FRM ...783", 100_000, "T74300302", 3),
				rcJournal("T74300302", 3, "2026-11-13 09:00:00", "JOURNAL TO ...135", -100_000, "T74300102", 1),
				"account-transactions-A3-783.csv:1 T74300302",
			},
		} {
			alone := c.first
			alone.partner = toolPartner{
				day: "2026-11-13", time: c.second.time, amount: c.second.amount, counted: true, category: "journal",
				kind: c.second.kind, description: c.second.description,
			}
			first := tcInput(toolInput{to: "2026-11-12", rows: []toolRow{deposit, alone}})
			later := tcInput(toolInput{to: "2026-11-13", rows: []toolRow{deposit, c.first, c.second}})
			e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
			var accounts []string
			for _, a := range e1.e.Accounts {
				accounts = append(accounts, fmt.Sprintf("%s exported %v new %v", a.Name, a.Exported, a.New))
			}
			if want := []string{"Schwab A1 ...135 exported true new true", "Schwab A3 ...783 exported false new true"}; !slices.Equal(accounts, want) {
				t.Errorf("%s: the first export's accounts %q, want %q", c.what, accounts, want)
			}
			e2 := chainStepOf(t, later, toolPP, e1, rcAt2)
			if w, m := e2.refs(outcomeWritten), rcMarks(e2); !slices.Equal(w, []string{c.second.ref}) || !slices.Equal(m, []string{c.next}) {
				t.Errorf("%s: the next export writes %q, PP marks %q", c.what, w, m)
			}
			if a := e2.e.Accounts; len(a) != 2 || a[0].New || a[1].New {
				t.Errorf("%s: the next export's accounts %+v", c.what, a)
			}
			m := newPPModel()
			ppApply(t, m, e1)
			ppReconcile(t, m, e1.in, e1.events, e1.outcomes)
			ppApply(t, m, e2)
			ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
		}
	})

	t.Run("a row back beside the one that took its place", func(t *testing.T) {
		t.Parallel()
		// A file buy, given; linked to its API row, which takes its place
		// (same_as) and PP's transaction with it; then counted again beside
		// it. PP holds one transaction, under the file row's Note, which the
		// API row's entry names: the API row is changed, its form alike, so
		// that you delete that one and import both rows, each under its own
		// Note. Then the file row is uncounted, and deleted by its Note
		// alone.
		deposit := rcCash("T79000100", 1, "2026-01-05 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000)
		file := rcFile("C79a0b1c2.1", 1, "2026-01-07", "Buy", "buy", keyEFGH, "EFGH", centsOf(2_000_000), 30, -6000)
		api := rcTrade("T79000105", 1, "2026-01-07 10:00:00", "buy", keyEFGH, "EFGH", 2_000_000, 30, -6000, 0)
		states := []toolInput{
			tcInput(toolInput{to: "2026-01-31", rows: []toolRow{deposit, file}}),
			tcInput(toolInput{
				to: "2026-02-27", rows: []toolRow{deposit, api},
				uncounted: []toolUncounted{{ref: file.ref, account: 1, why: "linked", matchedWith: api.ref}},
			}),
			tcInput(toolInput{to: "2026-03-27", rows: []toolRow{deposit, file, api}}),
			tcInput(toolInput{to: "2026-04-24", rows: []toolRow{deposit, api}, uncounted: []toolUncounted{{ref: file.ref, account: 1, why: "user"}}}),
		}
		for _, tool := range []string{toolPP, toolGF} {
			var steps []*chainStep
			var prev *chainStep
			for i, in := range states {
				prev = chainStepOf(t, in, tool, prev, []string{rcAt1, rcAt2, rcAt3, rcAt4}[i])
				steps = append(steps, prev)
			}
			back, gone := steps[2], steps[3]
			changed, _ := rcChanged(back)
			if w := back.refs(outcomeWritten); tool == toolPP && (!slices.Equal(w, []string{file.ref, api.ref}) ||
				!slices.Equal(changed, []string{api.ref + ": portfolio 2026-01-07 Buy 60.00 -> portfolio 2026-01-07 Buy 60.00"}) ||
				back.res.changed[0].NoteRef != file.ref || len(back.marks) > 0 || back.res.given["A1 ...135"][api.ref].NoteRef != "") {
				t.Errorf("pp: back, it writes %q, lists %q (%+v), PP marks %q", w, changed, back.res.changed, rcMarks(back))
			}
			if _, g := rcChanged(gone); len(g) != 1 || gone.res.gone[0].Ref != file.ref || gone.res.gone[0].NoteRef != "" {
				t.Errorf("%s: uncounted, it lists %q", tool, g)
			}
			if tool == toolPP {
				m := newPPModel()
				for _, s := range steps {
					ppApply(t, m, s)
					ppReconcile(t, m, s.in, s.events, s.outcomes)
				}
				continue
			}
			m := newGFModel()
			for _, s := range steps {
				if _, err := gfApply(t, m, s); err != nil && len(s.gf.file.Activities) > 0 {
					t.Fatal(err)
				}
				gfReconcile(t, s.in, s.events, s.outcomes, m)
			}
		}
	})

	t.Run("a row back in place of the one that took its place", func(t *testing.T) {
		t.Parallel()
		// A file buy, given; linked to its API row, which takes its place;
		// then the API row goes as the file row counts again. The file row
		// is the same as the API row gone, whose entry names the file row's
		// Note: PP holds it under the file row's own Note, which its entry
		// names as its own.
		deposit := rcCash("T79100100", 1, "2026-01-05 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000)
		file := rcFile("C79b1c2d3.1", 1, "2026-01-07", "Buy", "buy", keyEFGH, "EFGH", centsOf(2_000_000), 30, -6000)
		api := rcTrade("T79100105", 1, "2026-01-07 10:00:00", "buy", keyEFGH, "EFGH", 2_000_000, 30, -6000, 0)
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-01-31", rows: []toolRow{deposit, file}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{
			to: "2026-02-27", rows: []toolRow{deposit, api},
			uncounted: []toolUncounted{{ref: file.ref, account: 1, why: "linked", matchedWith: api.ref}},
		}), toolPP, e1, rcAt2)
		e3 := chainStepOf(t, tcInput(toolInput{
			to: "2026-03-27", rows: []toolRow{deposit, file}, uncounted: []toolUncounted{{ref: api.ref, account: 1, why: "user"}},
		}), toolPP, e2, rcAt3)
		if g := e3.res.given["A1 ...135"][file.ref]; len(e3.res.sameAs) != 1 || g.NoteRef != "" || len(e3.marks) != 1 || e3.marks[0].tick {
			t.Errorf("back: the same %+v, its entry %+v, PP marks %q", e3.res.sameAs, g, rcMarks(e3))
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a row back as the same as another row gone", func(t *testing.T) {
		t.Parallel()
		// Two file buys alike, given; one linked to its API row, which takes
		// its place; then the other goes as the first comes back: the first
		// is the same as the other, whose transaction PP keeps for it, so
		// that no Note names it twice and nothing is to delete. Then the API
		// row goes as another API row alike comes, while the first file row
		// is revised: the API row gone is not the same as the new one, since
		// its Note names the revised row, which PP holds under its own; and
		// once that row is uncounted, its Note names one transaction alone.
		deposit := rcCash("T79200100", 1, "2026-01-05 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000)
		file := rcFile("C79c1d2e3.1", 1, "2026-01-07", "Buy", "buy", keyEFGH, "EFGH", centsOf(1_000_000), 30, -3000)
		alike := file
		alike.ref = "C79c1d2e3.2"
		api := rcTrade("T79200105", 1, "2026-01-07 10:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0)
		another := api
		another.ref = "T79200106"
		revised := file
		revised.amount, revised.gross = centsOf(-3001), centsOf(-3001)
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-01-31", rows: []toolRow{deposit, file, alike}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{
			to: "2026-02-27", rows: []toolRow{deposit, alike, api},
			uncounted: []toolUncounted{{ref: file.ref, account: 1, why: "linked", matchedWith: api.ref}},
		}), toolPP, e1, rcAt2)
		e3 := chainStepOf(t, tcInput(toolInput{
			to: "2026-03-27", rows: []toolRow{deposit, file, api}, uncounted: []toolUncounted{{ref: alike.ref, account: 1, why: "user"}},
		}), toolPP, e2, rcAt3)
		changed, gone := rcChanged(e3)
		if len(changed)+len(gone) > 0 || len(e3.res.sameAs) != 1 || e3.res.sameAs[0] != (chainSameAs{Account: "A1 ...135", Gone: alike.ref, New: file.ref}) {
			t.Errorf("back: changed %q, gone %q, the same %+v", changed, gone, e3.res.sameAs)
		}
		e4 := chainStepOf(t, tcInput(toolInput{
			to: "2026-04-24", rows: []toolRow{deposit, revised, another}, uncounted: []toolUncounted{{ref: api.ref, account: 1, why: "user"}},
		}), toolPP, e3, rcAt4)
		if changed, gone := rcChanged(e4); len(e4.res.sameAs) > 0 || len(changed) != 1 || len(gone) != 1 || e4.res.gone[0].Ref != api.ref ||
			e4.res.given["A1 ...135"][another.ref].NoteRef != "" {
			t.Errorf("revised: the same %+v, changed %q, gone %q", e4.res.sameAs, changed, gone)
		}
		e5 := chainStepOf(t, tcInput(toolInput{
			to: "2026-05-29", rows: []toolRow{deposit, another}, uncounted: []toolUncounted{{ref: file.ref, account: 1, why: "user"}},
		}), toolPP, e4, "2026-06-01T15:00:00Z")
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3, e4, e5} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a transfer's paying side given with another receiving side", func(t *testing.T) {
		t.Parallel()
		// A transfer; then its receiving side is unpaired, a Deposit,
		// changed, whose deletion takes the transfer with it, while a new
		// journal of A1 pairs with the paying side, which, alike in all
		// that PP keeps, is given. The new journal's record makes the
		// transfer again, under its Note, which both entries then name.
		pays := rcJournal("T79900201", 2, "2026-05-04 09:30:00", "JOURNAL TO ...135", -100_000, "T79900101", 1)
		receives := rcJournal("T79900101", 1, "2026-05-04 09:30:05", "JOURNAL FRM ...398", 100_000, pays.ref, 2)
		unpaired := receives
		unpaired.pairKind, unpaired.pairRef, unpaired.pairAccount, unpaired.flags = "", "", 0, []string{"unpaired_journal"}
		paysAgain := pays
		paysAgain.pairRef = "T79900102"
		other := rcJournal("T79900102", 1, "2026-05-04 09:31:00", "JOURNAL FRM ...398", 100_000, pays.ref, 2)
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-05-08", rows: []toolRow{pays, receives}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{to: "2026-05-15", rows: []toolRow{paysAgain, unpaired, other}}), toolPP, e1, rcAt2)
		head := "A2 ...398 T79900201 to A1 ...135 T79900102"
		if a, b := e2.res.given["A2 ...398"][pays.ref], e2.res.given["A1 ...135"][other.ref]; !slices.Equal(e2.refs(outcomeGiven), []string{pays.ref}) ||
			a.Transfer != head || b.Transfer != head {
			t.Errorf("given %q, the transfer's entries %+v and %+v", e2.refs(outcomeGiven), a, b)
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a transfer's paying side back beside the one that took its place", func(t *testing.T) {
		t.Parallel()
		// A file journal pays a transfer; its API row takes its place
		// (same_as); then the file row counts again beside it, unpaired, a
		// Withdrawal of the transfer's day and amount. The API row's entry
		// names the file row's Note: it is changed, the transfer deleted
		// once and written again under its own, and the receiving side,
		// given, is of the transfer that the API row makes.
		dep := rcCash("T79300200", 2, "2026-03-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 500_000)
		filePays := rcFile("C79b0c1d2.1", 2, "2026-03-02", "Journal", "journal", "", "", sql.NullInt64{}, 0, -100_000)
		filePays.pairKind, filePays.pairRef, filePays.pairAccount = "journal", "T79300101", 1
		receives := rcJournal("T79300101", 1, "2026-03-02 15:00:00", "JOURNAL FRM ...398", 100_000, filePays.ref, 2)
		apiPays := rcJournal("T79300201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", -100_000, receives.ref, 1)
		receivesAPI := receives
		receivesAPI.pairRef = apiPays.ref
		back := filePays
		back.pairKind, back.pairRef, back.pairAccount = "", "", 0
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-03-06", rows: []toolRow{dep, filePays, receives}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{
			to: "2026-03-13", rows: []toolRow{dep, apiPays, receivesAPI},
			uncounted: []toolUncounted{{ref: filePays.ref, account: 2, why: "linked", matchedWith: apiPays.ref}},
		}), toolPP, e1, rcAt2)
		e3 := chainStepOf(t, tcInput(toolInput{to: "2026-03-20", rows: []toolRow{dep, back, apiPays, receivesAPI}}), toolPP, e2, rcAt3)
		changed, gone := rcChanged(e3)
		if w := e3.refs(outcomeWritten); !slices.Equal(w, []string{back.ref, apiPays.ref}) || len(changed) != 1 || len(gone) != 0 || len(e3.marks) > 0 ||
			e3.res.changed[0].Ref != apiPays.ref || e3.res.changed[0].Transfer != "A2 ...398 C79b0c1d2.1 to A1 ...135 T79300101" {
			t.Errorf("back: it writes %q, lists %+v and %q, PP marks %q", w, e3.res.changed, gone, rcMarks(e3))
		}
		if a, b := e3.res.given["A2 ...398"][apiPays.ref], e3.res.given["A1 ...135"][receives.ref]; a.NoteRef != "" ||
			a.Transfer != "A2 ...398 T79300201 to A1 ...135 T79300101" || b.Transfer != a.Transfer {
			t.Errorf("the transfer's entries %+v and %+v", a, b)
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a transfer's receiving side given with another paying side", func(t *testing.T) {
		t.Parallel()
		// A file journal pays a transfer; then an API journal pays its
		// receiving side, which, alike in all that PP keeps, is given, while
		// the file journal stays counted, unpaired: a Withdrawal, changed,
		// whose deletion takes the transfer with it, which the API journal
		// makes again. The receiving side is then of that transfer, not of
		// the file journal's, whose Withdrawal PP holds; and when it is
		// uncounted, and the API journal is unpaired, a Withdrawal equal to
		// the file journal's, the start holds that Withdrawal, and the API
		// journal's is ticked.
		dep := rcCash("T79400200", 2, "2026-01-02 10:30:00", "WIRE_IN", "Wire Incoming", "deposit", 500_000)
		file := rcFile("C79c0d1e2.1", 2, "2026-01-12", "Journal", "journal", "", "", sql.NullInt64{}, 0, -30_000)
		file.description = "JOURNAL TO ...135"
		api := rcJournal("T79400201", 2, "2026-01-12 11:00:00", "JOURNAL TO ...135", -30_000, "", 0)
		receives := rcJournal("T79400101", 1, "2026-01-13 09:00:00", "JOURNAL FRM ...398", 30_000, "", 0)
		pair := func(a, b toolRow) (toolRow, toolRow) {
			a.pairKind, a.pairRef, a.pairAccount, a.flags = "journal", b.ref, b.account, nil
			b.pairKind, b.pairRef, b.pairAccount, b.flags = "journal", a.ref, a.account, nil
			return a, b
		}
		file1, receives1 := pair(file, receives)
		api2, receives2 := pair(api, receives)
		file.flags = []string{"unpaired_journal"}
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-01-31", rows: []toolRow{dep, file1, receives1}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{to: "2026-02-27", rows: []toolRow{dep, file, api2, receives2}}), toolPP, e1, rcAt2)
		e3 := chainStepOf(t, tcInput(toolInput{
			to: "2026-03-27", rows: []toolRow{dep, file, api},
			uncounted: []toolUncounted{{ref: receives.ref, account: 1, why: "user"}},
		}), toolPP, e2, rcAt3)
		if g := e2.res.given["A1 ...135"][receives.ref]; !slices.Equal(e2.refs(outcomeGiven), []string{dep.ref, receives.ref}) ||
			g.Transfer != "A2 ...398 T79400201 to A1 ...135 T79400101" || e2.res.given["A2 ...398"][api.ref].Transfer != g.Transfer {
			t.Errorf("the receiving side given %q: %+v", e2.refs(outcomeGiven), g)
		}
		if got := rcMarks(e3); !slices.Equal(got, []string{"account-transactions-A2-398.csv:1 T79400201 tick"}) {
			t.Errorf("uncounted: PP marks %q", got)
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a row alike a transfer's side gone with the transfer", func(t *testing.T) {
		t.Parallel()
		// A transfer; then its paying side is unpaired, a Withdrawal, whose
		// deletion takes the transfer, while its receiving side, a file row,
		// is linked to an API row alike, which another journal pays. PP will
		// not hold the file row's transaction, so the API row is not the
		// same as it: the file row is gone, and the API row is written under
		// its own Note.
		dep := rcCash("T79800200", 2, "2026-04-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 500_000)
		pays := rcJournal("T79800201", 2, "2026-04-06 09:30:00", "JOURNAL TO ...135", -100_000, "C79a1b2c3.1", 1)
		file := rcFile("C79a1b2c3.1", 1, "2026-04-06", "Journal", "journal", "", "", sql.NullInt64{}, 0, 100_000)
		file.pairKind, file.pairRef, file.pairAccount = "journal", pays.ref, 2
		unpaired := pays
		unpaired.pairKind, unpaired.pairRef, unpaired.pairAccount, unpaired.flags = "", "", 0, []string{"unpaired_journal"}
		paysAPI := rcJournal("T79800202", 2, "2026-04-06 09:45:00", "JOURNAL TO ...135", -100_000, "T79800101", 1)
		api := rcJournal("T79800101", 1, "2026-04-06 10:00:00", "JOURNAL FRM ...398", 100_000, paysAPI.ref, 2)
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-04-10", rows: []toolRow{dep, pays, file}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{
			to: "2026-04-17", rows: []toolRow{dep, unpaired, paysAPI, api},
			uncounted: []toolUncounted{{ref: file.ref, account: 1, why: "linked", matchedWith: api.ref}},
		}), toolPP, e1, rcAt2)
		changed, gone := rcChanged(e2)
		if len(e2.res.sameAs) > 0 || len(changed) != 1 || !slices.Equal(gone, []string{file.ref + " linked to T79800101: account 2026-04-06 Transfer (Inbound) 1000.00"}) ||
			e2.res.given["A1 ...135"][api.ref].NoteRef != "" {
			t.Errorf("the same %+v, changed %q, gone %q, the API row's entry %+v", e2.res.sameAs, changed, gone, e2.res.given["A1 ...135"][api.ref])
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
	})

	t.Run("two gone rows alike and one new row", func(t *testing.T) {
		t.Parallel()
		// Two file buys alike, given; both linked, one to an API row alike:
		// one is the same as it, the other is gone, and PP keeps one of the
		// two that it holds.
		deposit := rcCash("T79500100", 1, "2026-02-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000)
		one := rcFile("C79d0e1f2.1", 1, "2026-02-04", "Buy", "buy", keyEFGH, "EFGH", centsOf(1_000_000), 30, -3000)
		two := one
		two.ref = "C79d0e1f2.2"
		api := rcTrade("T79500105", 1, "2026-02-04 10:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0)
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-02-27", rows: []toolRow{deposit, one, two}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{
			to: "2026-03-27", rows: []toolRow{deposit, api},
			uncounted: []toolUncounted{{ref: one.ref, account: 1, why: "linked", matchedWith: api.ref}, {ref: two.ref, account: 1, why: "user"}},
		}), toolPP, e1, rcAt2)
		if _, gone := rcChanged(e2); len(e2.res.sameAs) != 1 || e2.res.sameAs[0].Gone != one.ref || !slices.Equal(gone, []string{two.ref + " uncounted: user: portfolio 2026-02-04 Buy 30.00"}) {
			t.Errorf("the same %+v, gone %q", e2.res.sameAs, gone)
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
	})

	t.Run("a row the same as one that was the same as another", func(t *testing.T) {
		t.Parallel()
		// A row given, then replaced twice by rows alike: PP holds its
		// transaction under the first row's Note all along, which the last
		// row's entry names, and which deletes it once that row is revised.
		deposit := rcCash("T79600100", 1, "2026-02-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000)
		first := rcFile("C79e0f1a2.1", 1, "2026-02-04", "Buy", "buy", keyEFGH, "EFGH", centsOf(1_000_000), 30, -3000)
		second := first
		second.ref = "C79f0a1b2.1"
		third := rcTrade("T79600105", 1, "2026-02-04 10:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0)
		revised := third
		revised.amount, revised.gross = centsOf(-3001), centsOf(-3001)
		var steps []*chainStep
		var prev *chainStep
		for i, c := range []struct {
			row  toolRow
			gone string
		}{{first, ""}, {second, first.ref}, {third, second.ref}, {revised, ""}} {
			in := tcInput(toolInput{to: "2026-02-27", rows: []toolRow{deposit, c.row}})
			if c.gone != "" {
				in.uncounted = []toolUncounted{{ref: c.gone, account: 1, why: "linked", matchedWith: c.row.ref}}
			}
			prev = chainStepOf(t, in, toolPP, prev, []string{rcAt1, rcAt2, rcAt3, rcAt4}[i])
			steps = append(steps, prev)
		}
		if g := steps[2].res.given["A1 ...135"][third.ref]; g.NoteRef != first.ref {
			t.Errorf("the third row's entry %+v", g)
		}
		if c := steps[3].res.changed; len(c) != 1 || c[0].NoteRef != first.ref {
			t.Errorf("revised: changed %+v", c)
		}
		m := newPPModel()
		for _, s := range steps {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a transfer beside a deposit of its receiving leg", func(t *testing.T) {
		t.Parallel()
		// A deposit to A1, given, of the day and amount of a transfer that
		// comes later: PP checks the transfer by its paying leg alone, in
		// A2, and marks neither of its records.
		deposit := rcCash("T79700101", 1, "2026-03-02 16:00:00", "ELECTRONIC_FUND", "ACH Credit", "deposit", 100_000)
		pays := rcJournal("T79700201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", -100_000, "T79700102", 1)
		receives := rcJournal("T79700102", 1, "2026-03-02 09:30:02", "JOURNAL FRM ...398", 100_000, "T79700201", 2)
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-03-06", rows: []toolRow{deposit}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{to: "2026-03-06", rows: []toolRow{pays, receives, deposit}}), toolPP, e1, rcAt2)
		if got := rcMarks(e2); !slices.Equal(got, []string{"account-transactions-A2-398.csv:1 T79700201"}) {
			t.Errorf("PP marks %q", got)
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
	})

	// Two transfers from A2 to A1 of one day and amount, which the store
	// cannot tell apart, so that you pair them by hand: their paying sides
	// and receiving sides, and the deposits that fund the accounts.
	sameDay := func(prefix string) (deposits, pays, receives []toolRow) {
		deposits = []toolRow{
			rcCash(prefix+"100", 1, "2026-01-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000),
			rcCash(prefix+"200", 2, "2026-01-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000),
		}
		pays = []toolRow{
			rcJournal(prefix+"021", 2, "2026-02-03 08:00:00", "JOURNAL TO ...135", -5000, "", 0),
			rcJournal(prefix+"022", 2, "2026-02-03 08:30:00", "JOURNAL TO ...135", -5000, "", 0),
		}
		receives = []toolRow{
			rcJournal(prefix+"031", 1, "2026-02-03 08:00:10", "JOURNAL FRM ...398", 5000, "", 0),
			rcJournal(prefix+"032", 1, "2026-02-03 08:30:10", "JOURNAL FRM ...398", 5000, "", 0),
		}
		return deposits, pays, receives
	}
	pairOf := func(a, b toolRow) (toolRow, toolRow) {
		a.pairKind, a.pairRef, a.pairAccount, a.flags = "journal", b.ref, b.account, nil
		b.pairKind, b.pairRef, b.pairAccount, b.flags = "journal", a.ref, a.account, nil
		return a, b
	}
	unpaired := func(a toolRow) toolRow {
		a.pairKind, a.pairRef, a.pairAccount, a.flags = "", "", 0, []string{"unpaired_journal"}
		return a
	}

	t.Run("two transfers of one day paired again by hand", func(t *testing.T) {
		t.Parallel()
		// Each pair as the store made it; then you pair the first paying
		// side with the second receiving side, which, alike in all that PP
		// keeps, are both given, while the other two are unpaired, or
		// uncounted. Both transfers are deleted with those two, so the
		// first paying side is written again: its record makes the new
		// transfer, which both entries name.
		deposits, pays, receives := sameDay("T7d000")
		p1, r1 := pairOf(pays[0], receives[0])
		p2, r2 := pairOf(pays[1], receives[1])
		q1, q2 := pairOf(pays[0], receives[1])
		for _, c := range []struct {
			name      string
			rows      []toolRow
			uncounted []toolUncounted
		}{
			{"unpaired", []toolRow{q1, unpaired(receives[0]), unpaired(pays[1]), q2}, nil},
			{"uncounted", []toolRow{q1, q2}, []toolUncounted{{ref: receives[0].ref, account: 1, why: "user"}, {ref: pays[1].ref, account: 2, why: "user"}}},
		} {
			e1 := chainStepOf(t, tcInput(toolInput{to: "2026-02-27", rows: slices.Concat(deposits, []toolRow{p1, r1, p2, r2})}), toolPP, nil, rcAt1)
			e2 := chainStepOf(t, tcInput(toolInput{to: "2026-03-27", rows: slices.Concat(deposits, c.rows), uncounted: c.uncounted}), toolPP, e1, rcAt2)
			head := "A2 ...398 T7d000021 to A1 ...135 T7d000032"
			a, b := e2.res.given["A2 ...398"][pays[0].ref], e2.res.given["A1 ...135"][receives[1].ref]
			if w := e2.refs(outcomeWritten); !slices.Contains(w, pays[0].ref) || slices.Contains(w, receives[1].ref) || a.Transfer != head || b.Transfer != head {
				t.Errorf("%s: written %q, the transfer's entries %+v and %+v", c.name, w, a, b)
			}
			m := newPPModel()
			for _, s := range []*chainStep{e1, e2} {
				ppApply(t, m, s)
				ppReconcile(t, m, s.in, s.events, s.outcomes)
			}
		}
	})

	t.Run("two transfers of one day swapping partners", func(t *testing.T) {
		t.Parallel()
		// The transfers swap their receiving sides, all four sides given;
		// then one of them goes. Each side given is of the transfer of its
		// paying side now, not of the one it was first written with, so
		// that what goes deletes its own transfer and no other.
		deposits, pays, receives := sameDay("T7e000")
		p1, r1 := pairOf(pays[0], receives[0])
		p2, r2 := pairOf(pays[1], receives[1])
		q1, s2 := pairOf(pays[0], receives[1])
		q2, s1 := pairOf(pays[1], receives[0])
		for _, c := range []struct {
			name        string
			first, then []toolRow
		}{
			// Swapped: given as they were, each side is of its new paying
			// side's transfer, which PP holds still.
			{"swapped", []toolRow{p1, r1, p2, r2}, []toolRow{q1, s2, q2, s1}},
			// From one pair and a deposit and a withdrawal alike, to two:
			// the new paying side's record makes a second transfer, of the
			// receiving side given beside it.
			{"paired", []toolRow{q1, s2, unpaired(receives[0]), unpaired(pays[1])}, []toolRow{p1, r1, p2, r2}},
		} {
			e1 := chainStepOf(t, tcInput(toolInput{to: "2026-02-27", rows: slices.Concat(deposits, c.first)}), toolPP, nil, rcAt1)
			e2 := chainStepOf(t, tcInput(toolInput{to: "2026-03-27", rows: slices.Concat(deposits, c.then)}), toolPP, e1, rcAt2)
			// Each side is of the transfer of its paying side now.
			for _, r := range c.then {
				payer := r.ref
				if r.amount.Int64 > 0 {
					payer = r.pairRef
				}
				g, pg := e2.res.given[e2.in.accountOf(r.account).label()][r.ref], e2.res.given["A2 ...398"][payer]
				if g.Transfer == "" || g.Transfer != pg.Transfer {
					t.Errorf("%s: %s is of %q, its paying side %s of %q", c.name, r.ref, g.Transfer, payer, pg.Transfer)
				}
			}
			// The second receiving side goes, and its paying side is unpaired.
			var left []toolRow
			for _, r := range c.then {
				switch {
				case r.ref == receives[1].ref:
				case r.pairRef == receives[1].ref:
					left = append(left, unpaired(r))
				default:
					left = append(left, r)
				}
			}
			e3 := chainStepOf(t, tcInput(toolInput{
				to: "2026-04-27", rows: slices.Concat(deposits, left), uncounted: []toolUncounted{{ref: receives[1].ref, account: 1, why: "user"}},
			}), toolPP, e2, rcAt3)
			m := newPPModel()
			for _, s := range []*chainStep{e1, e2, e3} {
				ppApply(t, m, s)
				ppReconcile(t, m, s.in, s.events, s.outcomes)
			}
		}
	})

	t.Run("a receiving side the same as a gone one, of another paying side then", func(t *testing.T) {
		t.Parallel()
		// A file journal receives a transfer, and its API row takes its
		// place (same_as), under its Note; then the API row is paid by the
		// other journal, a transfer of another Note, and its entry names no
		// file row's Note: when the file row counts again, the API row is
		// given still, not changed.
		deposits, pays, receives := sameDay("T7g000")
		file := rcFile("C7g000000.1", 1, "2026-02-03", "Journal", "journal", "", "", sql.NullInt64{}, 0, 5000)
		file.description = "JOURNAL FRM ...398"
		p1, f1 := pairOf(pays[0], file)
		q1, a1 := pairOf(pays[0], receives[0])
		q2, a2 := pairOf(pays[1], receives[0])
		linked := []toolUncounted{{ref: file.ref, account: 1, why: "linked", matchedWith: receives[0].ref}}
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-02-27", rows: slices.Concat(deposits, []toolRow{p1, f1, unpaired(pays[1])})}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{to: "2026-03-27", rows: slices.Concat(deposits, []toolRow{q1, a1, unpaired(pays[1])}), uncounted: linked}), toolPP, e1, rcAt2)
		e3 := chainStepOf(t, tcInput(toolInput{to: "2026-04-27", rows: slices.Concat(deposits, []toolRow{unpaired(pays[0]), q2, a2}), uncounted: linked}), toolPP, e2, rcAt3)
		e4 := chainStepOf(t, tcInput(toolInput{to: "2026-05-27", rows: slices.Concat(deposits, []toolRow{unpaired(pays[0]), q2, a2, unpaired(file)})}), toolPP, e3, rcAt4)
		g2, g3 := e2.res.given["A1 ...135"][receives[0].ref], e3.res.given["A1 ...135"][receives[0].ref]
		if g2.NoteRef != file.ref || g3.NoteRef != "" || g3.Transfer != "A2 ...398 T7g000022 to A1 ...135 T7g000031" {
			t.Errorf("the API row's entries %+v and %+v", g2, g3)
		}
		if changed, gone := rcChanged(e4); len(changed)+len(gone) > 0 || !slices.Equal(e4.refs(outcomeWritten), []string{file.ref}) {
			t.Errorf("the file row back: written %q, changed %q, gone %q", e4.refs(outcomeWritten), changed, gone)
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3, e4} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("a row the same as one that a row back frees", func(t *testing.T) {
		t.Parallel()
		// Four buys alike. Two are written; then both are uncounted and the
		// other two take their places (same_as). Then the first two count
		// again beside the fourth, and the third is uncounted: the first two
		// are written under their own Notes, so the third, gone, is not the
		// same as the fourth, which PP holds under a Note of the first two's;
		// then the fourth, whose Note names a row back, is changed, which
		// only a second look finds.
		deposit := rcCash("T7a000100", 1, "2026-01-02 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 1_000_000)
		buy := func(ref, clock string) toolRow {
			return rcTrade(ref, 1, "2026-01-07 "+clock, "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0)
		}
		g, n, mm, e := buy("T7a000003", "09:00:00"), buy("T7a000002", "10:00:00"), buy("T7a000001", "11:00:00"), buy("T7a000004", "12:00:00")
		e1 := chainStepOf(t, tcInput(toolInput{to: "2026-01-31", rows: []toolRow{deposit, n, mm}}), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, tcInput(toolInput{
			to: "2026-02-27", rows: []toolRow{deposit, g, e}, uncounted: []toolUncounted{{ref: n.ref, account: 1, why: "user"}, {ref: mm.ref, account: 1, why: "user"}},
		}), toolPP, e1, rcAt2)
		e3 := chainStepOf(t, tcInput(toolInput{
			to: "2026-03-27", rows: []toolRow{deposit, n, mm, e}, uncounted: []toolUncounted{{ref: g.ref, account: 1, why: "user"}},
		}), toolPP, e2, rcAt3)
		changed, gone := rcChanged(e3)
		if len(e2.res.sameAs) != 2 || !slices.Equal(changed, []string{"T7a000004: portfolio 2026-01-07 Buy 30.00 -> portfolio 2026-01-07 Buy 30.00"}) ||
			!slices.Equal(gone, []string{"T7a000003 uncounted: user: portfolio 2026-01-07 Buy 30.00"}) {
			t.Errorf("the same %+v, then changed %q, gone %q", e2.res.sameAs, changed, gone)
		}
		m := newPPModel()
		for _, s := range []*chainStep{e1, e2, e3} {
			ppApply(t, m, s)
			ppReconcile(t, m, s.in, s.events, s.outcomes)
		}
	})

	t.Run("5 a redo", func(t *testing.T) {
		t.Parallel()
		// PP: every account of the export deleted, and the full export of
		// the later store imported as a first import.
		first, later := rcSequence4()
		e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
		e2 := chainStepOf(t, later, toolPP, e1, rcAt2)
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		redo := chainStepOf(t, later, toolPP, nil, rcAt3)
		for _, name := range ppAccountNames(redo.e) {
			m.deleteAccount(name)
		}
		ppApply(t, m, redo)
		if got := rcMarks(redo); !slices.Equal(got, []string{
			"account-transactions-A2-398.csv:1 T74000201", "account-transactions-A2-398.csv:2 T74000202 tick",
			"account-transactions-A2-398.csv:3 T74000203 tick", "account-transactions-A2-398.csv:4 T74000204",
		}) {
			t.Errorf("the redo: PP marks %q", got)
		}
		ppReconcile(t, m, redo.in, redo.events, redo.outcomes)

		// Ghostfolio: an account brought up to date, deleted with its
		// activities, and made again by a full export made at once, with
		// every balance, while the other accounts' activities are skipped.
		f1, l1 := rcSequence1()
		g1 := chainStepOf(t, f1, toolGF, nil, rcAt1)
		g2 := chainStepOf(t, l1, toolGF, g1, rcAt2)
		g := newGFModel()
		for _, s := range []*chainStep{g1, g2} {
			if _, err := gfApply(t, g, s); err != nil {
				t.Fatal(err)
			}
		}
		full := chainStepOf(t, l1, toolGF, nil, rcAt3)
		gfDeleteAccount(t, g, "Schwab A1 ...135")
		if n, err := gfImport(g, full.file); err != nil || n != len(full.gf.file.Activities) {
			t.Errorf("the redo of A1 imports %d of %d activities, %v", n, len(full.gf.file.Activities), err)
		}
		if cash, ok := g.cash("Schwab A1 ...135"); !ok || cash.FloatString(2) != "10289.34" {
			t.Errorf("A1's cash in Ghostfolio is %v", cash)
		}
		gfReconcile(t, full.in, full.events, full.outcomes, g)
		// A2 has no activity: deleted, the full export has nothing new for
		// the dialog, and a direct import makes it again.
		gfDeleteAccount(t, g, "Schwab A2 ...398")
		if n, err := gfImport(g, full.file); err == nil {
			t.Errorf("the dialog imports %d activities of a file with none new", n)
		}
		if n, err := gfImportDirect(g, full.file); err != nil || n != 0 {
			t.Errorf("the direct import: %d, %v", n, err)
		}
		i := slices.IndexFunc(g.accounts, func(a *gfmAccount) bool { return a.name == "Schwab A2 ...398" })
		if cash, ok := g.cash("Schwab A2 ...398"); i < 0 || len(g.accounts[i].balances) != 2 || !ok || cash.FloatString(2) != "5300.00" {
			t.Errorf("A2 in Ghostfolio: %v, cash %v", g.accounts, cash)
		}
		gfReconcile(t, full.in, full.events, full.outcomes, g)
	})

	t.Run("6 an account of the same masked number joins", func(t *testing.T) {
		t.Parallel()
		rows := []toolRow{
			rcTrade("T76000101", 1, "2026-04-01 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
			rcTrade("T76000201", 2, "2026-04-01 11:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0),
		}
		first := tcInput(toolInput{to: "2026-04-30", rows: slices.Clone(rows)})
		accounts := append(tcAccounts(), toolAccount{id: 4, masked: "...398", nickname: "Second Margin"})
		later := tcInput(toolInput{to: "2026-04-30", accounts: accounts, rows: append(rows,
			rcTrade("T76000401", 4, "2026-04-02 10:00:00", "buy", keyABCD, "ABCD", 1_000_000, 40, -4000, 0))})
		for _, tool := range []string{toolPP, toolGF} {
			e1 := chainStepOf(t, first, tool, nil, rcAt1)
			e2 := chainStepOf(t, later, tool, e1, rcAt2)
			if w, g := e2.refs(outcomeWritten), e2.refs(outcomeGiven); !slices.Equal(w, []string{"T76000401"}) || !slices.Equal(g, []string{"T76000101", "T76000201"}) {
				t.Errorf("%s: written %q, given %q", tool, w, g)
			}
			var labels []string
			for _, a := range chainJSON(e2.res, rcAt2, nil).Accounts {
				labels = append(labels, fmt.Sprintf("%s %s %v", a.Label, a.Name, a.New))
			}
			if !slices.Equal(labels, []string{"A1 ...135 Schwab A1 ...135 false", "A2 ...398 Schwab A2 ...398 false", "A4 ...398 Schwab A4 ...398 true"}) {
				t.Errorf("%s: accounts %q", tool, labels)
			}
			if tool == toolPP {
				m := newPPModel()
				ppApply(t, m, e1)
				ppApply(t, m, e2)
				ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
				continue
			}
			// Imported into Ghostfolio, the next export and a full one of
			// the later store add the new account and its activity alone.
			full := chainStepOf(t, later, tool, nil, rcAt3)
			for _, file := range [][]byte{e2.file, full.file} {
				g := newGFModel()
				if _, err := gfApply(t, g, e1); err != nil {
					t.Fatal(err)
				}
				if n, err := gfImport(g, file); err != nil || n != 1 || len(g.accounts) != 3 {
					t.Errorf("Ghostfolio imports %d activities, holds %d accounts: %v", n, len(g.accounts), err)
				}
				gfReconcile(t, e2.in, e2.events, e2.outcomes, g)
			}
		}
	})

	t.Run("7 a shared Ghostfolio", func(t *testing.T) {
		t.Parallel()
		tax := rcCash("T77000102", 1, "2026-04-04 10:00:00", "JOURNAL", "W-8 Withholding~XYZ", "tax", -116)
		tax.securityKey, tax.symbol = keyXYZ, "XYZ"
		rows := []toolRow{
			rcTrade("T77000101", 1, "2026-04-01 10:00:00", "buy", keyPut, symPut, 1_000_000, 2, -20_065, 65),
			rcFile("C7700000.1", 1, "2026-04-02", "Service Fee", "fee", "", "", sql.NullInt64{}, 0, -2500),
			rcFile("C7700000.2", 1, "2026-04-03", "Credit Interest", "interest", "", "", sql.NullInt64{}, 0, 42),
			tax,
		}
		snapshots := []toolSnapshot{rcSnapshot(1, "2026-04-06", 1000)}
		state := func(more ...toolRow) toolInput {
			rows = append(rows, more...)
			return tcInput(toolInput{to: "2026-04-30", rows: slices.Clone(rows), snapshots: snapshots})
		}
		s1 := state()
		tax2 := tax
		tax2.ref, tax2.time, tax2.amount = "T77000104", tcNY("2026-04-13 10:00:00"), centsOf(-50)
		s2 := state(
			rcTrade("T77000103", 1, "2026-04-10 10:00:00", "sell", keyPut, symPut, -1_000_000, 3, 29_935, 65),
			rcFile("C7700000.3", 1, "2026-04-11", "Service Fee", "fee", "", "", sql.NullInt64{}, 0, -1000),
			rcFile("C7700000.4", 1, "2026-04-12", "Credit Interest", "interest", "", "", sql.NullInt64{}, 0, 10),
			tax2,
		)
		// The source that saw the option last changes: the view's
		// description, which the export does not take, flips, and back.
		flip := func(in toolInput, description string) toolInput {
			for i := range in.securities {
				if in.securities[i].key == keyPut {
					in.securities[i].description = description
				}
			}
			in.names = append(slices.Clone(in.names), toolName{
				key: keyPut, kind: "description", value: description,
				firstSeen: tcNY("2026-04-15 10:00:00"), lastSeen: tcNY("2026-04-25 10:00:00"),
			})
			return in
		}
		s3 := flip(state(rcTrade("T77000105", 1, "2026-04-20 10:00:00", "buy", keyPut, symPut, 1_000_000, 1, -10_065, 65)), "PUT XYZ $40 EXP 02/21/25")
		s4 := flip(state(rcTrade("T77000106", 1, "2026-04-21 10:00:00", "buy", keyPut, symPut, 1_000_000, 1.5, -15_065, 65)), "XYZ 02/21/2025 40.00 P")

		g := newGFModel()
		for _, p := range gfCashProfiles {
			g.otherSymbols[gfProfileSymbol(p.profile)] = true
		}
		g.otherSymbols[toolSecurityOf(&s1, keyPut, symPut).gfSymbol()] = true
		g.otherAccounts[tcAccounts()[0].gfID()] = true

		var prev *chainStep
		for i, in := range []toolInput{s1, s2, s3, s4} {
			s := chainStepOf(t, in, toolGF, prev, []string{rcAt1, rcAt2, rcAt3, rcAt4}[i])
			before := len(g.activities)
			if n, err := gfApply(t, g, s); err != nil || n != len(s.gf.file.Activities) {
				t.Fatalf("export %d: Ghostfolio imports %d of %d activities: %v", i+1, n, len(s.gf.file.Activities), err)
			}
			if len(g.accounts) != 1 || len(g.profiles) != 4 || len(g.activities) != before+len(s.gf.file.Activities) {
				t.Errorf("export %d: Ghostfolio holds %d accounts, %d profiles, %d activities", i+1, len(g.accounts), len(g.profiles), len(g.activities))
			}
			prev = s
		}
		if g.accounts[0].id == tcAccounts()[0].gfID() || slices.ContainsFunc(g.profiles, func(p *gfmProfile) bool { return g.otherSymbols[p.symbol] }) {
			t.Errorf("Ghostfolio gave the user ids and symbols that another user holds")
		}
		// A redo of the account from a full export made at once.
		full := chainStepOf(t, s4, toolGF, nil, "2026-05-02T15:00:00Z")
		held := len(g.activities)
		gfDeleteAccount(t, g, "Schwab A1 ...135")
		if n, err := gfImport(g, full.file); err != nil || n != held || len(g.profiles) != 4 || len(g.accounts) != 1 {
			t.Errorf("the redo imports %d activities of %d, %d profiles, %d accounts: %v", n, held, len(g.profiles), len(g.accounts), err)
		}
		keys := map[string]bool{}
		for _, a := range g.activities {
			k := gfDuplicateKey(a.comment, a.currency, a.source, a.date, a.fee, a.quantity, a.symbol, a.typ, a.unitPrice)
			if keys[k] {
				t.Errorf("Ghostfolio holds %s twice", a.comment)
			}
			keys[k] = true
		}
		gfReconcile(t, full.in, full.events, full.outcomes, gfUnshare(g, &full.gf.file))
	})

	t.Run("8 -manual kept after its rows", func(t *testing.T) {
		t.Parallel()
		rows := []toolRow{
			rcTrade("T78000101", 1, "2026-05-04 10:00:00", "buy", keyABCD, "ABCD", 3_000_000, 40, -12_000, 0),
			rcTrade("T78000102", 1, "2026-05-04 11:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0),
			rcTrade("T78000103", 1, "2026-05-04 15:00:00", "sell", keyEFGH, "EFGH", -1_000_000, 31, 3100, 0),
		}
		positions := []toolPosition{rcPosition("2026-05-05", keyABCD, 3_000_000, 123)}
		first := tcInput(toolInput{to: "2026-05-31", manual: []string{"ABCD", "efgh"}, rows: slices.Clone(rows), positions: positions})
		later := tcInput(toolInput{to: "2026-06-30", positions: positions, rows: append(rows,
			rcFile("C7800000.1", 1, "2026-06-01", "Qualified Dividend", "dividend", keyABCD, "ABCD", sql.NullInt64{}, 0, 120))})
		e1 := chainStepOf(t, first, toolGF, nil, rcAt1)
		e2 := chainStepOf(t, later, toolGF, e1, rcAt2)
		abcd := toolSecurityOf(e2.in, keyABCD, "ABCD")
		if w := e2.refs(outcomeWritten); !slices.Equal(w, []string{"C7800000.1"}) || abcd.manual != "asked" {
			t.Errorf("written %q, ABCD manual %q", w, abcd.manual)
		}
		if a := e2.gf.file.Activities; len(a) != 1 || a[0].Type != "DIVIDEND" || a[0].DataSource != "MANUAL" || a[0].Symbol != abcd.gfSymbol() {
			t.Errorf("the later dividend: %+v", a)
		}
		if got := chainJSON(e2.res, rcAt2, e2.unused).Chain.Manual; !slices.Equal(got, []chainManual{{Symbol: "ABCD"}, {Symbol: "efgh", Unused: true}}) {
			t.Errorf("the chain's -manual: %+v", got)
		}
		g := newGFModel()
		if _, err := gfApply(t, g, e1); err != nil {
			t.Fatal(err)
		}
		if n, err := gfApply(t, g, e2); err != nil || n != 1 {
			t.Errorf("Ghostfolio imports %d activities: %v", n, err)
		}
		gfReconcile(t, e2.in, e2.events, e2.outcomes, g)
	})

	t.Run("9 an account first exported without an activity", func(t *testing.T) {
		t.Parallel()
		snapshots := []toolSnapshot{rcSnapshot(3, "2026-06-01", 50_000), rcSnapshot(3, "2026-06-02", 51_000)}
		first := tcInput(toolInput{to: "2026-06-05", snapshots: slices.Clone(snapshots)})
		later := tcInput(toolInput{
			to:        "2026-06-12",
			rows:      []toolRow{rcTrade("T79000301", 3, "2026-06-10 10:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 30, -3000, 0)},
			snapshots: append(snapshots, rcSnapshot(3, "2026-06-11", 48_000)),
		})
		e1 := chainStepOf(t, first, toolGF, nil, rcAt1)
		if a := chainJSON(e1.res, rcAt1, nil).Accounts; len(a) != 1 || !a[0].Exported || !a[0].New || a[0].Balances == nil || a[0].Balances.Count != 2 ||
			a[0].Cash == nil || a[0].Cash.Value != "510.00" || a[0].LastDay != nil {
			t.Errorf("the first export's accounts %+v", a)
		}
		g := newGFModel()
		if n, err := gfApply(t, g, e1); err == nil || len(g.accounts) > 0 {
			t.Errorf("the dialog imports %d activities of a file with none: %v", n, err)
		}
		e2 := chainStepOf(t, later, toolGF, e1, rcAt2)
		if n, err := gfApply(t, g, e2); err != nil || n != 1 {
			t.Fatalf("Ghostfolio imports %d activities: %v", n, err)
		}
		if cash, ok := g.cash("Schwab A3 ...783"); !ok || len(g.accounts[0].balances) != 3 || cash.FloatString(2) != "480.00" {
			t.Errorf("A3 in Ghostfolio: %d balances, cash %v", len(g.accounts[0].balances), cash)
		}
		gfReconcile(t, e2.in, e2.events, e2.outcomes, g)
	})

	t.Run("10 the chain's account", func(t *testing.T) {
		t.Parallel()
		rows := []toolRow{
			rcCash("T7a000101", 1, "2026-07-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 100_000),
			rcTrade("T7a000201", 2, "2026-07-01 11:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
		}
		first := onlyAccount(tcInput(toolInput{to: "2026-07-31", rows: slices.Clone(rows)}), 2)
		accounts := tcAccounts()
		accounts[1].nickname = "Trading"
		accounts = append(accounts, toolAccount{id: 4, masked: "...398", nickname: "Options & Margin"})
		later := tcInput(toolInput{to: "2026-08-31", accounts: accounts, rows: append(rows,
			rcTrade("T7a000202", 2, "2026-08-03 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 52, -5200, 0),
			rcTrade("T7a000401", 4, "2026-08-03 11:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 52, -5200, 0))})
		e1 := chainStepOf(t, first, toolPP, nil, rcAt1)
		// The next export reads the chain's account, A2, as the store would
		// select it.
		c, err := parseChain(e1.manifest)
		if err != nil {
			t.Fatal(err)
		}
		if id, err := c.account(); err != nil || id != 2 {
			t.Fatalf("the chain's account: %d, %v", id, err)
		}
		e2 := chainStepOf(t, onlyAccount(later, 2), toolPP, e1, rcAt2)
		if w, g := e2.refs(outcomeWritten), e2.refs(outcomeGiven); !slices.Equal(w, []string{"T7a000202"}) || !slices.Equal(g, []string{"T7a000201"}) {
			t.Errorf("written %q, given %q", w, g)
		}
		part := chainJSON(e2.res, rcAt2, nil)
		if part.Chain.Account == nil || *part.Chain.Account != "A2" || len(part.Accounts) != 1 || part.Accounts[0].Nickname != "Trading" || part.Accounts[0].New {
			t.Errorf("the chain %+v, accounts %+v", part.Chain, part.Accounts)
		}
		m := newPPModel()
		ppApply(t, m, e1)
		ppApply(t, m, e2)
		ppReconcile(t, m, e2.in, e2.events, e2.outcomes)
	})

	t.Run("11 a position opened before the rows", func(t *testing.T) {
		t.Parallel()
		rows := []toolRow{rcTrade("T7b000101", 1, "2026-02-02 10:00:00", "sell", keyXYZ, "XYZ", -10_000_000, 50, 50_000, 0)}
		positions := []toolPosition{rcPosition("2026-02-26", keyXYZ, 5_000_000, 250)}
		snapshots := []toolSnapshot{rcSnapshot(1, "2026-02-26", 50_000)}
		s1 := tcInput(toolInput{to: "2026-02-27", rows: slices.Clone(rows), positions: slices.Clone(positions), snapshots: slices.Clone(snapshots)})
		rows = append(rows, rcTrade("T7b000102", 1, "2026-03-02 10:00:00", "buy", keyXYZ, "XYZ", 3_000_000, 52, -15_600, 0))
		positions = append(positions, rcPosition("2026-03-30", keyXYZ, 8_000_000, 420))
		snapshots = append(snapshots, rcSnapshot(1, "2026-03-30", 34_400))
		s2 := tcInput(toolInput{to: "2026-03-31", rows: slices.Clone(rows), positions: slices.Clone(positions), snapshots: slices.Clone(snapshots)})
		rows = append(rows, rcFile("C7b00000.1", 1, "2026-04-01", "Stock Split", "other", keyXYZ, "XYZ", centsOf(8_000_000), 0, 0))
		positions = append(positions, rcPosition("2026-04-29", keyXYZ, 16_000_000, 440))
		snapshots = append(snapshots, rcSnapshot(1, "2026-04-29", 34_400))
		s3 := tcInput(toolInput{to: "2026-04-30", rows: rows, positions: positions, snapshots: snapshots})

		describe := func(s *chainStep) []string {
			var out []string
			for _, h := range s.res.holdings {
				out = append(out, fmt.Sprintf("%s %s %s %s %v", h.Account, h.Symbol, h.SnapshotShares, h.WrittenShares, h.Before))
			}
			for _, l := range s.res.leftOut {
				out = append(out, fmt.Sprintf("%s %s %v", l.Ref, l.Reason, l.Before))
			}
			return out
		}
		for _, tool := range []string{toolPP, toolGF} {
			var steps []*chainStep
			var prev *chainStep
			for i, in := range []toolInput{s1, s2, s3, s3} {
				prev = chainStepOf(t, in, tool, prev, []string{rcAt1, rcAt2, rcAt3, rcAt4}[i])
				steps = append(steps, prev)
			}
			for i, want := range [][]string{
				{"A1 ...135 XYZ 5 -10 false"},
				{"A1 ...135 XYZ 8 -7 true"},
				{"A1 ...135 XYZ 16 -7 false", "C7b00000.1 other false"},
				{"A1 ...135 XYZ 16 -7 true", "C7b00000.1 other true"},
			} {
				if got := describe(steps[i]); !slices.Equal(got, want) {
					t.Errorf("%s, export %d: %q, want %q", tool, i+1, got, want)
				}
			}

			// The opening position, added by hand in the tool after the
			// first import, and the later exports imported.
			if tool == toolPP {
				m := newPPModel()
				ppApply(t, m, steps[0])
				f := ppFolderOf(toolInput{tool: toolPP})
				f["portfolio-transactions-A1-135.csv"] = ppMade(ppPortfolio, map[string]string{
					"Date": "2026-01-02", "Type": "Delivery (Inbound)", "Value": "750.00", "Shares": "15",
					"Ticker Symbol": "XYZ", "WKN": "98389B100", "Security Name": "XYZ CORP COMMON STOCK", "Securities Account": "Schwab A1 ...135",
				})
				if _, refused := m.importFile(f.write(t), "portfolio-transactions-A1-135.csv", "Schwab A1 ...135", nil); len(refused) > 0 {
					t.Fatal(refused)
				}
				for _, s := range steps[1:] {
					for _, f := range s.pp {
						if f.kind == ppConfiguration {
							continue
						}
						if _, refused := m.importFile(s.dir, f.name, f.account.name(), nil); len(refused) > 0 {
							t.Error(refused)
						}
					}
				}
				var shares int64
				for _, q := range m.shares("Schwab A1 ...135") {
					shares += q
				}
				if shares != 8_00_000_000 || m.cash("Schwab A1 ...135") != 34_400 {
					t.Errorf("PP holds %d shares (in 10^-8) and %s", shares, formatCents(m.cash("Schwab A1 ...135")))
				}
				continue
			}
			g := newGFModel()
			if _, err := gfApply(t, g, steps[0]); err != nil {
				t.Fatal(err)
			}
			g.activities = append(g.activities, gfmActivity{
				account: g.accounts[0].id, comment: "by hand", currency: "USD", source: "YAHOO", symbol: "XYZ", typ: "BUY",
				date: "2026-01-02T12:00:00.000Z", fee: new(big.Rat), quantity: big.NewRat(15, 1), unitPrice: big.NewRat(50, 1),
			})
			for _, s := range steps[1:] {
				// The dialog cannot import a file with no activity: the
				// split's export has none.
				if _, err := gfApply(t, g, s); (err != nil) != (len(s.gf.file.Activities) == 0) {
					t.Fatalf("%d activities: %v", len(s.gf.file.Activities), err)
				}
			}
			if held := g.holdings(); len(held) != 1 || held[gfmHolding{g.accounts[0].id, "YAHOO", "XYZ"}].Cmp(big.NewRat(8, 1)) != 0 {
				t.Errorf("Ghostfolio holds %v", held)
			}
		}
	})
}
