package history

import (
	"bytes"
	"flag"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// updateGF rewrites the golden files of TestToolCasesGF from what the
// export writes for Ghostfolio of each case: go test ./internal/history
// -run TestToolCasesGF -history.update-gf. Read the diff before committing
// it.
var updateGF = flag.Bool("history.update-gf", false, "rewrite the golden files of TestToolCasesGF")

// TestToolCasesGF writes each case of toolCases for Ghostfolio, as an
// export writes ghostfolio.json, and requires the bytes of
// testdata/tools/gf/<case>/ghostfolio.json (11.2 of the design). Each file
// must pass gfCheck, and import into a user who holds nothing as the case
// means it to: refused only when it holds no activity, and else with every
// activity new; each account's cash the latest balance; the activities'
// cash, per account, the cash of the rows written, a delivery's at its
// value; the shares of each asset in each account those that the records
// move; nothing new when the file is imported again, on its own instance
// or on one where another user holds its ids and symbols; every fee off
// net performance; and no holding with a dividend and no investment, since
// each case buys what it is paid a dividend on before the dividend.
func TestToolCasesGF(t *testing.T) {
	t.Parallel()
	for _, c := range toolCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in, events, outcomes := mapCase(c, toolGF)
			x := gfRender(in, events)
			path := filepath.Join(t.TempDir(), gfFileName)
			if err := writePrivate(path, x.file.write); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "tools", "gf", c.name, gfFileName)
			if *updateGF {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from what the export writes (-history.update-gf rewrites it):\n%s", golden, gfFirstDiff(want, got))
			}
			gfCheck(t, path, gfDeliveries(events))

			m := newGFModel()
			n, err := gfImport(m, got)
			switch {
			case len(x.file.Activities) == 0:
				if err == nil {
					t.Errorf("a file with no activity imported, %d activities", n)
				}
				return
			case err != nil:
				t.Fatalf("Ghostfolio refuses the file: %v", err)
			case n != len(x.file.Activities):
				t.Errorf("imported %d activities of %d", n, len(x.file.Activities))
			}
			for _, a := range x.file.Accounts {
				cash, ok := m.cash(a.Name)
				if len(a.Balances) == 0 {
					if ok {
						t.Errorf("%s has cash %s without a balance", a.Name, cash.FloatString(2))
					}
					continue
				}
				if last := a.Balances[len(a.Balances)-1].Value; !ok || cash.FloatString(2) != gfRat(t, string(last)).FloatString(2) {
					t.Errorf("%s has cash %v, not its latest balance %s", a.Name, cash, last)
				}
			}
			gfReconcile(t, in, events, outcomes, m)
			if n, err := gfImport(m, got); err == nil {
				t.Errorf("imported again, %d activities are new", n)
			}
			// Where another user holds every id and symbol of the file, the
			// import makes fresh ones; imported again, the file finds each
			// account and asset by its name, and every activity is one the
			// user has.
			shared := newGFModel()
			for _, a := range x.file.Accounts {
				shared.otherAccounts[a.ID] = true
			}
			for _, p := range x.file.AssetProfiles {
				shared.otherSymbols[p.Symbol] = true
			}
			if n, err := gfImport(shared, got); err != nil || n != len(x.file.Activities) {
				t.Fatalf("on a shared instance: %d imported, %v", n, err)
			}
			accounts, profiles := len(shared.accounts), len(shared.profiles)
			if n, err := gfImport(shared, got); err == nil || len(shared.accounts) != accounts || len(shared.profiles) != profiles {
				t.Errorf("on a shared instance, imported again: %d new activities, %d accounts and %d profiles, were %d and %d",
					n, len(shared.accounts), len(shared.profiles), accounts, profiles)
			}
			p := m.performance()
			if p.offNet.Cmp(p.fees) != 0 || len(p.unvalued) > 0 {
				t.Errorf("fees %s, of which net performance takes off %s; not valued: %v", p.fees.FloatString(2), p.offNet.FloatString(2), p.unvalued)
			}
			if len(p.noInvestment) > 0 {
				t.Errorf("dividends with no investment, which set the dividend yield to 0: %v", p.noInvestment)
			}
		})
	}
}

// gfReconcile requires the activities of model m, into which the export of
// in for Ghostfolio was imported, to move the cash and the shares that the
// rows of in do (11.4 of the design): per account, the cash of each row
// written, from the store's amount, but for a delivery, which moves its
// value; and, per account and asset, the shares that the records events
// move. The cash rows, which the balances carry, have no activity.
func gfReconcile(t *testing.T, in *toolInput, events []toolEvent, outcomes []toolOutcome, m *gfModel) {
	t.Helper()
	cash := map[string]*big.Rat{}
	add := func(account toolAccount, cents int64) {
		id := account.gfID()
		if cash[id] == nil {
			cash[id] = new(big.Rat)
		}
		cash[id].Add(cash[id], big.NewRat(cents, 100))
	}
	shares := map[gfmHolding]*big.Rat{}
	for i, o := range outcomes {
		if o.outcome != outcomeWritten {
			continue
		}
		delivered := false
		for _, e := range eventsOf(events, i) {
			switch e.kind {
			case evDeliveryIn, evCloseIn:
				add(e.account, -e.value)
				delivered = true
			case evDeliveryOut, evCloseOut:
				add(e.account, e.value)
				delivered = true
			}
			if e.kind.shareSign() != 0 {
				h := gfmHolding{e.account.gfID(), e.security.gfDataSource(), e.security.gfSymbol()}
				if shares[h] == nil {
					shares[h] = new(big.Rat)
				}
				shares[h].Add(shares[h], big.NewRat(e.kind.shareSign()*e.shares, 1_000_000))
			}
		}
		if !delivered {
			add(o.account, in.rows[i].amount.Int64)
		}
	}
	flows := m.flows()
	for id, want := range cash {
		if got := flows[id]; got == nil || got.Cmp(want) != 0 {
			t.Errorf("the activities of account %s move %v, the rows written %s", id, got, want.FloatString(2))
		}
		delete(flows, id)
	}
	for id, got := range flows {
		t.Errorf("the activities of account %s move %s, which no row written does", id, got.FloatString(2))
	}
	for h, q := range shares {
		if q.Sign() == 0 {
			delete(shares, h)
		}
	}
	held := m.holdings()
	for h, want := range shares {
		if got := held[h]; got == nil || got.Cmp(want) != 0 {
			t.Errorf("%v: Ghostfolio holds %v, the records move %s", h, got, want.FloatString(6))
		}
		delete(held, h)
	}
	for h, got := range held {
		t.Errorf("%v: Ghostfolio holds %s, which no record moves", h, got.FloatString(6))
	}
}

// gfRat returns the number s, exact.
func gfRat(t testing.TB, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("%q is not a number", s)
	}
	return r
}

// gfFirstDiff returns the first line where got differs from want, with its
// number.
func gfFirstDiff(want, got []byte) string {
	w, g := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for i := range max(len(w), len(g)) {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + strconv.Itoa(i+1) + ": want " + wl + "\n got " + gl
		}
	}
	return "(no line differs)"
}

// TestGFRender checks what the golden files cannot show of gfRender: the
// symbols that Ghostfolio's import asks Yahoo about and those that Yahoo
// may no longer know; what each profile is, and whether an activity uses
// it; that the activities' order does not depend on the records'; and
// which accounts the file holds.
func TestGFRender(t *testing.T) {
	t.Parallel()
	render := func(name string) *gfExport {
		in, events, _ := mapCase(toolCaseNamed(name), toolGF)
		return gfRender(in, events)
	}
	t.Run("yahoo", func(t *testing.T) {
		t.Parallel()
		if got := render("securities").yahoo; !slices.Equal(got, []string{"ABC-B", "PQRS", "QQQX", "XYZ"}) {
			t.Errorf("securities: Yahoo is asked about %q", got)
		}
		if got := render("manual").yahoo; len(got) > 0 {
			t.Errorf("manual: Yahoo is asked about %q", got)
		}
		if got := render("idle").idle; len(got) != 1 || got[0] != (toolIdle{symbol: "EFGH", key: keyEFGH, last: "2025-11-11"}) {
			t.Errorf("idle: %v", got)
		}
	})
	t.Run("assets", func(t *testing.T) {
		t.Parallel()
		describe := func(x *gfExport) []string {
			var out []string
			for i, a := range x.assets {
				s := x.file.AssetProfiles[i].Name
				switch {
				case a.profile != "":
					s += " @" + a.profile
				case a.security != nil:
					s += " " + a.security.manual
				}
				if !a.used {
					s += " prices_only"
				}
				out = append(out, s)
			}
			return out
		}
		for _, c := range []struct {
			name string
			want []string
		}{
			{"manual", []string{"Security (QQQX) asked", "Security (ZZB123458) asked prices_only", "Security (i:1000003) asked", "Security (98389B100) asked"}},
			{"income", []string{"Schwab fees @fees", "Schwab interest @interest", "Schwab taxes @taxes", "Bond (9128ZZQ15) bond"}},
			{"securities", []string{"Security (i:1000004) not_plain", "Security (ZZQ00K102) delisted", "Security (ZZC99X101) no_ticker"}},
			{"holdings", []string{
				"XYZ 2025-02-21 40 put (XYZ250221P00040000) option", "XYZ 2026-09-18 50 call (XYZ260918C00050000) option",
				"XYZ 2027-01-15 45 put (XYZ270115P00045000) option",
			}},
		} {
			if got := describe(render(c.name)); !slices.Equal(got, c.want) {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
			}
		}
	})
	t.Run("order", func(t *testing.T) {
		t.Parallel()
		for _, c := range toolCases() {
			in, events, _ := mapCase(c, toolGF)
			var want, got bytes.Buffer
			x := gfRender(in, events)
			if err := x.file.write(&want); err != nil {
				t.Fatal(err)
			}
			reversed := slices.Clone(events)
			slices.Reverse(reversed)
			x = gfRender(in, reversed)
			if err := x.file.write(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Errorf("%s: the records in another order write another file:\n%s", c.name, gfFirstDiff(want.Bytes(), got.Bytes()))
			}
		}
	})
	t.Run("sorted", func(t *testing.T) {
		t.Parallel()
		// Rows whose refs and accounts are in another order than their
		// times, given in neither: a day's timeless row comes first, then
		// the rest by time, and those of one instant by account.
		trade := func(ref string, account int64, clock string) toolRow {
			return toolRow{
				ref: ref, account: account, source: "api", time: tcNY(clock), kind: "TRADE", category: "buy",
				securityKey: keyXYZ, symbol: "XYZ", quantity: centsOf(1_000_000), price: tcPrice(50), amount: centsOf(-5000),
			}
		}
		in := tcInput(toolInput{tool: toolGF, rows: []toolRow{
			trade("T81000101", 1, "2025-03-03 15:00:00"),
			trade("T81000103", 2, "2025-03-03 11:00:00"),
			trade("T81000104", 1, "2025-03-03 11:00:00"),
			{
				ref: "T81000102", account: 2, source: "file", posted: "2025-03-03", kind: "Buy", description: "XYZ CORP", category: "buy",
				securityKey: keyXYZ, symbol: "XYZ", quantity: centsOf(1_000_000), price: tcPrice(50), amount: centsOf(-5000),
			},
			trade("T81000100", 1, "2025-03-04 09:30:00"),
		}})
		events, _ := toolEvents(&in)
		var got []string
		for _, a := range gfRender(&in, events).file.Activities {
			got = append(got, a.Comment[strings.LastIndex(a.Comment, " ")+1:])
		}
		if want := []string{"T81000102", "T81000104", "T81000103", "T81000101", "T81000100"}; !slices.Equal(got, want) {
			t.Errorf("activities in the order %q, want %q", got, want)
		}
	})
	t.Run("text", func(t *testing.T) {
		t.Parallel()
		// Descriptions are written as they are, & and < and > too.
		in := tcInput(toolInput{tool: toolGF, rows: []toolRow{{
			ref: "T81000201", account: 1, source: "api", time: tcNY("2025-03-03 10:00:00"), kind: "TRADE", category: "buy",
			securityKey: keyNoTick, symbol: "ZZC99X101", quantity: centsOf(1_000_000), price: tcPrice(10), amount: centsOf(-1000),
		}}})
		for i := range in.names {
			if in.names[i].key == keyNoTick && in.names[i].kind == "description" {
				in.names[i].value = "P&Q <UNITS> CL A"
			}
		}
		events, _ := toolEvents(&in)
		var b bytes.Buffer
		if err := gfRender(&in, events).file.write(&b); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(b.Bytes(), []byte(`"comment": "P&Q <UNITS> CL A"`)) {
			t.Errorf("the description is not written as it is:\n%s", b.Bytes())
		}
	})
	t.Run("accounts", func(t *testing.T) {
		t.Parallel()
		in := tcInput(toolInput{
			tool: toolGF, account: 1,
			rows: []toolRow{{
				ref: "T81000001", account: 1, source: "api", time: tcNY("2025-03-03 10:00:00"), kind: "WIRE_IN", description: "Wire Incoming",
				amount: centsOf(100_000), category: "deposit",
			}},
			snapshots: []toolSnapshot{
				{account: 1, marketDate: "2025-03-04", fetchedAt: tcNY("2025-03-04 18:00:00")},
				{account: 2, marketDate: "2025-03-04", fetchedAt: tcNY("2025-03-04 18:00:00"), cash: centsOf(500)},
			},
		})
		events, _ := toolEvents(&in)
		accounts := gfRender(&in, events).file.Accounts
		if len(accounts) != 1 || accounts[0].Name != "Schwab A1 ...135" || accounts[0].Balances != nil || accounts[0].PlatformID != nil {
			t.Errorf("one account selected, with a snapshot without cash: %+v", accounts)
		}
		in.snapshots = nil
		if accounts := gfRender(&in, events).file.Accounts; len(accounts) != 0 {
			t.Errorf("an account with no activity and no snapshot: %+v", accounts)
		}
	})
}
