package history

import (
	"cmp"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// whysOf returns s's whys of PP's symbols_changed (symbolWhys) in one
// string, with why it has no ticker after a space: "no_ticker delisted".
func whysOf(s *toolSecurity) string {
	why, because := s.symbolWhys()
	return strings.TrimSpace(strings.Join(why, ",") + " " + because)
}

// unsureText returns what us says of each key that it lists, a line each,
// for a test to compare: its key, or the symbol of rows without one, then
// each security that it may be, by its key or symbol, with its command; with
// facts, the facts of each (factsText) in brackets after it, and a CUSIP.
func unsureText(us []ToolUnsure, facts ...bool) []string {
	with := len(facts) > 0 && facts[0]
	var out []string
	for _, u := range us {
		line := cmp.Or(u.Key, "symbol "+u.Symbol)
		if with {
			line += " [" + factsText(u.Facts) + "]"
		}
		if u.Before {
			line += " before"
		}
		var of []string
		for _, s := range u.Securities {
			o := cmp.Or(s.Key, "symbol "+s.Symbol)
			if with {
				o = strings.TrimSpace(o+" "+s.CUSIP) + " [" + factsText(s.Facts) + "]"
			}
			of = append(of, strings.TrimSpace(o+" "+s.Command))
		}
		out = append(out, line+": "+strings.Join(of, "; "))
	}
	return out
}

// factsText returns f in one string: its descriptions, then its tickers,
// each with its source and the day that it saw it last, in New York, then
// the days of its rows, and held.
func factsText(f ToolFacts) string {
	parts := []string{strings.Join(f.Descriptions, "|")}
	for _, k := range f.Tickers {
		parts = append(parts, k.Ticker+" "+k.Source+" "+string(nytime.DateOf(k.LastSeen)))
	}
	if f.First != "" {
		parts = append(parts, "rows "+string(f.First)+".."+string(f.Last))
	}
	if f.Held {
		parts = append(parts, "held")
	}
	return strings.Join(parts, ", ")
}

// checkTwoKeysApart checks case c, two keys of a ticker that nothing joins,
// as variant v (with v.other, a third key of the ticker of another CUSIP)
// sets them, named name: the History file's key, which knows no CUSIP, is
// written on its own in both tools (safeName), with neither ticker nor WKN
// in PP and MANUAL, why unsure, in Ghostfolio, and listed with the API's key
// and the third key as securities that it may be, each with its command;
// so is the API's key, with the file's, unless apiKnows says that it knows a
// CUSIP. Each tool holds them apart.
func checkTwoKeysApart(t *testing.T, name string, c toolCase, v twoKeys, apiKnows bool) {
	t.Helper()
	same := func(key, other string) string { return other + " schwabtx edit K:" + key + " -same K:" + other }
	var want []string
	if !apiKnows {
		want = append(want, keyTwoAPI+": "+same(keyTwoAPI, keyTwoFile))
	}
	file := keyTwoFile + ": " + same(keyTwoFile, keyTwoAPI)
	if v.other != "" {
		file += "; " + same(keyTwoFile, keyTwoOther)
	}
	want = append(want, file)
	for _, tool := range []string{toolPP, toolGF} {
		e := caseExport(t, c, tool)
		if got := unsureText(e.Unsure); !slices.Equal(got, want) {
			t.Errorf("%s %s: unsure %q, want %q", name, tool, got, want)
		}
		for _, ev := range e.events {
			s := ev.security
			ticker, wkn, ppName := ppIdentifiers(s)
			if alone := s.key == keyTwoFile || !apiKnows; s.unsure != alone || alone &&
				(ticker != "" || wkn != "" || ppName != "Security ("+s.key+")" || s.manual != "unsure" || s.gfName() != ppName || s.identity() != s.key) {
				t.Errorf("%s %s: %s of %s: unsure %v, PP %q %q %q, Ghostfolio %s %q, identity %s", name, tool, ev.ref, s.key, s.unsure, ticker, wkn, ppName, s.manual, s.gfName(), s.identity())
			}
		}
		s := chainStepFrom(t, e, t.TempDir())
		if tool == toolPP {
			m := newPPModel()
			ppApply(t, m, s)
			ppReconcile(t, m, e.in, e.events, e.outcomes)
			if held := m.shares("Schwab A1 ...135"); len(held) != 2 {
				t.Errorf("%s: PP holds %v", name, held)
			}
			continue
		}
		m := newGFModel()
		if _, err := gfApply(t, m, s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		gfReconcile(t, s.in, s.events, s.outcomes, m)
		if held := m.holdings(); len(held) != 2 {
			t.Errorf("%s: Ghostfolio holds %v", name, held)
		}
	}
}

// mapCase maps case c for tool.
func mapCase(c toolCase, tool string) (*toolInput, []toolEvent, []toolOutcome) {
	in := c.in
	in.tool = tool
	events, outcomes := toolEvents(&in)
	return &in, events, outcomes
}

// eventsOf returns the records of row i.
func eventsOf(events []toolEvent, i int) []toolEvent {
	var out []toolEvent
	for _, e := range events {
		if e.row == i {
			out = append(out, e)
		}
	}
	return out
}

// describeRow writes what toolEvents made of a row, as the cases' wants
// do: left_out and its reason, combined and its cash row, or in_balances;
// or each record, by its PP Type or Ghostfolio type, with its value, its
// fees, its shares, its leg and its cash profile, and the row's caveats.
func describeRow(tool string, o toolOutcome, events []toolEvent) string {
	switch o.outcome {
	case outcomeLeftOut:
		s := outcomeLeftOut + " " + o.reason
		if o.with != "" {
			s += " with " + o.with
		}
		return s
	case outcomeCombined:
		s := outcomeCombined + " with "
		if o.withAccount.id != 0 {
			s += o.withAccount.label() + " "
		}
		return s + o.with
	case outcomeInBalances:
		return outcomeInBalances
	}
	var records []string
	for _, e := range events {
		typ := e.kind.gfType()
		if tool == toolPP {
			_, typ = e.kind.ppType()
		}
		s := typ + " " + formatCents(e.value)
		if e.fees != 0 {
			s += " fees " + formatCents(e.fees)
		}
		if e.shares != 0 {
			s += " shares " + formatMicros(e.shares)
		}
		if e.leg.Valid {
			s += " leg " + strconv.FormatInt(e.leg.Int64, 10)
		}
		if e.profile != "" {
			s += " @" + e.profile
		}
		records = append(records, s)
	}
	s := strings.Join(records, "; ")
	if len(o.caveats) > 0 {
		s += " [" + strings.Join(o.caveats, " ") + "]"
	}
	return s
}

// TestToolMapping maps every case of toolCases for each tool and requires
// what the case says of each row, one outcome per row, and of the parts of
// the mapping: the lines of toolMapping, each reason and caveat, the
// advice, transfers, deliveries, securities, flags, Ghostfolio's dates and
// cash, the holdings check, -manual, the idle Yahoo symbols and the
// accounts' labels (11.1 of the design).
func TestToolMapping(t *testing.T) {
	t.Parallel()
	t.Run("cases", func(t *testing.T) {
		t.Parallel()
		seen := map[string]bool{}
		for _, c := range toolCases() {
			if seen[c.name] {
				t.Errorf("two cases are named %s", c.name)
			}
			seen[c.name] = true
			checkCase(t, c)
		}
	})

	t.Run("edges", func(t *testing.T) {
		t.Parallel()
		// The bounds of rules that the cases meet only inside: a two-row
		// cash row beside shares that came in, not out; a row that says
		// "Removed due to Expiration" but is no RECEIVE_AND_DELIVER; a buy
		// of 0; a cash merger with fees; an option's multiplier past
		// 10 000; shares past what an int64 holds; a journal pair of 0.00.
		in := tcInput(toolInput{rows: []toolRow{
			{
				ref: "T61003001", account: 1, source: "api", time: tcNY("2025-02-03 10:00:00"), kind: "TRADE", category: "sell",
				amount: centsOf(10_000), securityKey: keyABCD, symbol: "ABCD", flags: []string{"two_row_event"},
			},
			{
				ref: "T61003002", account: 1, source: "api", time: tcNY("2025-02-03 10:00:00"), kind: "RECEIVE_AND_DELIVER", category: "other",
				amount: centsOf(0), securityKey: keyABCD, symbol: "ABCD", quantity: centsOf(5_000_000), flags: []string{"two_row_event"},
			},
			{
				ref: "T61003003", account: 2, source: "api", time: tcNY("2025-02-21 16:00:00"), kind: "TRADE", category: "other",
				description: "Removed due to Expiration", amount: centsOf(0), securityKey: keyPut, symbol: symPut, quantity: centsOf(-1_000_000),
			},
			rcTrade("T61003004", 1, "2025-02-24 10:00:00", "buy", keyEFGH, "EFGH", 1_000_000, 0, 0, 0),
			{
				ref: "T61003005", account: 1, source: "api", time: tcNY("2025-03-03 10:00:00"), kind: "TRADE", category: "sell",
				amount: centsOf(99_000), fees: centsOf(1000), securityKey: keyABCD, symbol: "ABCD", flags: []string{"two_row_event"},
			},
			{
				ref: "T61003006", account: 1, source: "api", time: tcNY("2025-03-03 10:00:00"), kind: "RECEIVE_AND_DELIVER", category: "other",
				amount: centsOf(0), securityKey: keyABCD, symbol: "ABCD", quantity: centsOf(-10_000_000), flags: []string{"two_row_event"},
			},
			rcTrade("T61003007", 2, "2025-03-04 10:00:00", "buy", "i:1000013", "XYZ   250321C00047000", 1_000_000, 1, -10_000, 0),
			rcTrade("T61003008", 2, "2025-03-05 10:00:00", "buy", keyPut, symPut, 100_000_000_000_000_000, 1, -10_000, 0),
			rcJournal("T61003009", 1, "2025-03-06 10:00:00", "JOURNAL TO ...398", 0, "T61003010", 2),
			rcJournal("T61003010", 2, "2025-03-06 10:00:05", "JOURNAL FRM ...135", 0, "T61003009", 1),
		}})
		in.securities = append(slices.Clone(in.securities), toolSecurityRow{
			key: "i:1000013", assetType: "OPTION", putCall: "CALL", strike: tcPrice(47), expiration: "2025-03-21", multiplier: tcPrice(20_000), underlying: "XYZ",
		})
		in.names = append(slices.Clone(in.names), toolName{key: "i:1000013", kind: "symbol", value: "XYZ   250321C00047000"})
		checkCase(t, toolCase{name: "edges", in: in, want: []toolWant{
			{"T61003001", "left_out no_quantity", "left_out no_quantity"},
			{"T61003002", "left_out other", "left_out other"},
			{"T61003003", "left_out other", "left_out other"},
			{"T61003004", "Buy 0.00 shares 1", "BUY 0.00 shares 1"},
			{"T61003005", "Sell 990.00 fees 10.00 shares 10", "SELL 990.00 fees 10.00 shares 10"},
			{"T61003006", "combined with T61003005", "combined with T61003005"},
			{"T61003007", "left_out multiplier", "left_out multiplier"},
			{"T61003008", "left_out no_quantity", "left_out no_quantity"},
			{"T61003009", "left_out zero_amount", "left_out zero_amount"},
			{"T61003010", "left_out zero_amount", "left_out zero_amount"},
		}})
	})

	t.Run("lines", func(t *testing.T) {
		t.Parallel()
		type sample struct{ caseName, ref string }
		// A row of the cases for each line of toolMapping, in its order.
		samples := [][]sample{
			{{"cash", "T61001101"}},
			{{"cash", "T61001102"}},
			{{"cash", "Ce1a2b3c4.3"}},
			{{"cash", "Ce1a2b3c4.4"}},
			{{"transfers", "T61001201"}},
			{{"cash", "T61001105"}},
			{{"cash", "T61001106"}},
			{{"deliveries", "T61001502"}},
			{{"deliveries", "T61001501"}},
			{{"deliveries", "Cb0c1d2e3.1"}},
			{{"deliveries", "Cb0c1d2e3.6"}},
			{{"deliveries", "Cb0c1d2e3.7"}},
			{{"deliveries", "Cb0c1d2e3.2"}},
			{{"trades", "T61001301"}},
			{{"trades", "Cf0e1d2c3.3"}},
			{{"trades", "T61001302"}},
			{{"trades", "Cf0e1d2c3.5"}},
			{{"two_row", "C0a1b2c3d.2"}},
			{{"income", "T61001602"}},
			{{"income", "Cc0d1e2f3.2"}},
			{{"income", "T61001603"}},
			{{"income", "Cc0d1e2f3.5"}, {"bonds", "Ca1b2c3d4.6"}},
			{{"income", "Cc0d1e2f3.6"}},
			{{"income", "Cc0d1e2f3.7"}},
			{{"income", "Cc0d1e2f3.8"}},
			{{"income", "T61001604"}},
			{{"income", "Cc0d1e2f3.9"}},
			{{"options", "Cd0e1f2a3.3"}},
			{{"options", "Cd0e1f2a3.2"}},
			{{"two_row", "C0a1b2c3d.3"}},
			{{"left_out", "Cf1a2b3c4.3"}, {"cash", "T61001109"}},
		}
		if len(samples) != len(toolMapping) {
			t.Fatalf("%d samples for %d lines", len(samples), len(toolMapping))
		}
		for n, line := range toolMapping {
			if !slices.Contains(categories, line.category) {
				t.Errorf("line %d: %q is not a category", n, line.category)
			}
			for _, s := range samples[n] {
				c := toolCaseNamed(s.caseName)
				for _, tool := range []string{toolPP, toolGF} {
					in, events, outcomes := mapCase(c, tool)
					i := slices.IndexFunc(in.rows, func(r toolRow) bool { return r.ref == s.ref })
					if i < 0 {
						t.Fatalf("line %d: case %s holds no %s", n, s.caseName, s.ref)
					}
					what := fmt.Sprintf("line %d (%s, %s) %s %s", n, line.category, line.when, tool, s.ref)
					if in.rows[i].category != line.category {
						t.Errorf("%s is booked %s", what, in.rows[i].category)
					}
					checkLine(t, what, line, tool, outcomes, events, i)
				}
			}
		}
	})

	t.Run("reasons and caveats", func(t *testing.T) {
		t.Parallel()
		reasons, caveats := map[string]bool{}, map[string]string{}
		for _, c := range toolCases() {
			for _, tool := range []string{toolPP, toolGF} {
				_, _, outcomes := mapCase(c, tool)
				for _, o := range outcomes {
					if o.reason != "" {
						reasons[o.reason] = true
					}
					for _, cv := range o.caveats {
						caveats[cv] += tool + " "
					}
				}
			}
		}
		codes := map[string]bool{}
		for _, r := range toolReasons {
			if codes[r.code] {
				t.Errorf("reason %s is listed twice", r.code)
			}
			codes[r.code] = true
			if !reasons[r.code] {
				t.Errorf("no case leaves a row out for %s", r.code)
			}
		}
		for r := range reasons {
			if !codes[r] {
				t.Errorf("rows are left out for %s, which toolReasons does not list", r)
			}
		}
		codes = map[string]bool{}
		for _, cv := range toolCaveats {
			if codes[cv.code] {
				t.Errorf("caveat %s is listed twice", cv.code)
			}
			codes[cv.code] = true
			tools, ok := caveats[cv.code]
			switch {
			case !ok:
				t.Errorf("no case writes a row with the caveat %s", cv.code)
			case cv.tool != "" && strings.TrimSpace(strings.ReplaceAll(tools, cv.tool+" ", "")) != "":
				t.Errorf("the caveat %s is for %s, but %s rows carry it", cv.code, cv.tool, tools)
			case cv.tool != "" && cv.tool != toolPP && cv.tool != toolGF:
				t.Errorf("the caveat %s is for %q, no tool", cv.code, cv.tool)
			}
		}
		for cv := range caveats {
			if !codes[cv] {
				t.Errorf("rows carry the caveat %s, which toolCaveats does not list", cv)
			}
		}
	})

	t.Run("advice", func(t *testing.T) {
		t.Parallel()
		// Each reason's advice is what its line in the docs says, a row's
		// command naming the row.
		for _, c := range toolCases() {
			for _, tool := range []string{toolPP, toolGF} {
				in, _, outcomes := mapCase(c, tool)
				for i, o := range outcomes {
					if o.outcome != outcomeLeftOut {
						if o.advice != "" || o.command != "" || o.reason != "" {
							t.Errorf("%s %s is %s with advice %q %q", c.name, o.ref, o.outcome, o.advice, o.command)
						}
						continue
					}
					j := slices.IndexFunc(toolReasons, func(r toolReason) bool { return r.code == o.reason })
					doc := toolReasons[j].advice
					advice := "`" + o.advice + "`"
					switch {
					case o.advice == "" && doc != "none":
						t.Errorf("%s %s: no advice, but the docs say %q", c.name, o.ref, doc)
					case o.advice != "" && !strings.Contains(doc, advice):
						t.Errorf("%s %s: advice %s, which the docs do not give for %s: %q", c.name, o.ref, o.advice, o.reason, doc)
					case o.command != "" && !strings.Contains(doc, advice+" with `"+strings.Replace(o.command, in.rows[i].ref, "REF", 1)+"`") &&
						!strings.Contains(doc, "(`"+o.command+"`)"):
						t.Errorf("%s %s: command %q, which the docs do not give for %s: %q", c.name, o.ref, o.command, o.reason, doc)
					case o.command == "" && strings.Contains(doc, advice+" with `"):
						t.Errorf("%s %s: no command, but the docs give one for %s: %q", c.name, o.ref, o.reason, doc)
					}
				}
			}
		}
		_, _, outcomes := mapCase(toolCaseNamed("left_out"), toolPP)
		_, _, more := mapCase(toolCaseNamed("trades"), toolPP)
		_, _, gf := mapCase(toolCaseNamed("gf_cash"), toolGF)
		outcomes = append(append(outcomes, more...), gf...)
		for _, want := range []struct{ ref, advice, command string }{
			{"T61002101", "check", "schwabtx status T61002101"},
			{"Cf1a2b3c4.1", "split", ""},
			{"Cf1a2b3c4.2", "corporate_action", ""},
			{"Cf1a2b3c4.3", "set_category", "schwabtx edit Cf1a2b3c4.3 -category C"},
			{"Cf1a2b3c4.4", "corporate_action", ""},
			{"Cf1a2b3c4.5", "corporate_action", ""},
			{"Cf1a2b3c4.6", "set_category", "schwabtx edit Cf1a2b3c4.6 -category C"},
			{"Cf1a2b3c4.7", "split", ""},
			{"Cf1a2b3c4.8", "split", ""},
			{"T61001310", "set_security", "schwabtx edit T61001310 -security K:…"},
			{"Cf0e1d2c3.9", "check", "schwabtx status Cf0e1d2c3.9"},
			{"Cf0e1d2c3.11", "by_hand", ""},
			{"T61002304", "set_cash", "schwabtx snapshot"},
		} {
			i := slices.IndexFunc(outcomes, func(o toolOutcome) bool { return o.ref == want.ref })
			if i < 0 {
				t.Fatalf("no outcome of %s", want.ref)
			}
			if o := outcomes[i]; o.advice != want.advice || o.command != want.command {
				t.Errorf("%s (%s): advice %q, command %q; want %q, %q", want.ref, o.reason, o.advice, o.command, want.advice, want.command)
			}
		}
	})

	t.Run("transfers", func(t *testing.T) {
		t.Parallel()
		in, events, outcomes := mapCase(toolCaseNamed("transfers"), toolPP)
		byRef := map[string]toolEvent{}
		for _, e := range events {
			byRef[e.ref] = e
		}
		// A transfer is one record, of its paying side, on its day, at its
		// time, with its text, which names the receiving side; the
		// receiving side, dated the next day, is combined with it.
		e := byRef["T61001201"]
		if e.kind != evTransferOut || e.day != "2026-03-02" || !e.time.Equal(tcNY("2026-03-02 09:30:00")) || e.text != "JOURNAL TO ...135" || e.value != 100_000 ||
			e.account.label() != "A2 ...398" || e.pair.label() != "A1 ...135" || e.pairRef != "T61001202" || e.security != nil {
			t.Errorf("%s: %s on %s at %s, %q, %s, %s with %s %s", e.ref, e.kind, e.day, e.time, e.text, formatCents(e.value), e.account.label(), e.pair.label(), e.pairRef)
		}
		if i := slices.IndexFunc(in.rows, func(r toolRow) bool { return r.ref == "T61001202" }); outcomes[i].outcome != outcomeCombined ||
			outcomes[i].with != "T61001201" || outcomes[i].withLabel() != "A2 ...398" || len(eventsOf(events, i)) > 0 {
			t.Errorf("T61001202 is %s with %s %s", outcomes[i].outcome, outcomes[i].withLabel(), outcomes[i].with)
		}
		// A pair written apart names its partner; a journal whose partner
		// is not counted is unpaired in effect.
		if e := byRef["T61001203"]; e.pair.label() != "A2 ...398" || e.pairRef != "T61001204" {
			t.Errorf("T61001203 names %q %s", e.pair.label(), e.pairRef)
		}
		if e := byRef["T61001205"]; e.pairRef != "" || len(e.caveats) != 0 {
			t.Errorf("T61001205, whose partner is not counted, names %s with %v", e.pairRef, e.caveats)
		}
		// One account selected: its side is written by its sign, and names
		// the other.
		_, events, _ = mapCase(toolCaseNamed("transfer_outside"), toolPP)
		if e := events[0]; e.kind != evDeposit || e.day != "2026-03-03" || e.text != "JOURNAL FRM ...398" || e.pairRef != "T61001201" || e.pair.label() != "A2 ...398" {
			t.Errorf("one side alone: %s on %s, %q, with %s %s", e.kind, e.day, e.text, e.pair.label(), e.pairRef)
		}

		// Both accounts selected, but the export does not write the paying
		// side: it is dated after -to, or left out for a reason of its own
		// (no day). No transfer: the receiving side is written by its sign,
		// on its own day, and names the paying side, so that no leg is
		// lost; the paying side is left out with its reason.
		pays := rcJournal("T61001291", 2, "2026-03-04 09:30:00", "JOURNAL TO ...135", -100_000, "T61001292", 1)
		receives := rcJournal("T61001292", 1, "2026-03-03 08:00:00", "JOURNAL FRM ...398", 100_000, pays.ref, 2)
		after := tcInput(toolInput{to: "2026-03-03", rows: []toolRow{receives}})
		after.rows[0].partner = toolPartner{day: "2026-03-04", time: pays.time, amount: pays.amount, counted: true, category: "journal", kind: pays.kind, description: pays.description}
		noDay := pays
		noDay.time = time.Time{}
		for _, c := range []struct {
			what string
			in   toolInput
			want []string
		}{
			{"after -to", after, []string{"T61001292 Deposit 1000.00 [pair_outside]"}},
			{"left out", tcInput(toolInput{to: "2026-03-06", rows: []toolRow{noDay, receives}}), []string{"T61001291 left_out no_day", "T61001292 Deposit 1000.00 [pair_outside]"}},
		} {
			in, events, outcomes := mapCase(toolCase{name: c.what, in: c.in}, toolPP)
			var got []string
			for i, o := range outcomes {
				got = append(got, in.rows[i].ref+" "+describeRow(toolPP, o, eventsOf(events, i)))
			}
			if e := events[0]; !slices.Equal(got, c.want) || e.day != "2026-03-03" || e.pair.label() != "A2 ...398" || e.pairRef != pays.ref {
				t.Errorf("%s: %q, want %q; the receiving side on %s names %s %s", c.what, got, c.want, e.day, e.pair.label(), e.pairRef)
			}
		}
	})

	t.Run("deliveries", func(t *testing.T) {
		t.Parallel()
		for _, tool := range []string{toolPP, toolGF} {
			_, events, _ := mapCase(toolCaseNamed("deliveries"), tool)
			prices := map[string]string{}
			for _, e := range events {
				key := e.ref
				if e.leg.Valid {
					key += " leg " + strconv.FormatInt(e.leg.Int64, 10)
				}
				prices[key] = e.price.RatString()
				// A delivery's value is its shares at its price, in cents.
				value := new(big.Rat).Mul(new(big.Rat).SetFrac64(e.shares, 1_000_000), e.price)
				if got := roundUnits(value, 2).Int64(); got != e.value {
					t.Errorf("%s %s: value %s for %s shares at %s", tool, key, formatCents(e.value), formatMicros(e.shares), e.price.RatString())
				}
			}
			for key, want := range map[string]string{
				"T61001501": "52", "T61001502": "52", // the delivering side's, on both sides
				"Cb0c1d2e3.3":     "199/200", // a bond's 99.50 per 100 of face
				"Cb0c1d2e3.4":     "105/2",   // the snapshot's 262.50 for 5 shares, of the day
				"Cb0c1d2e3.5":     "0",
				"T61001504":       "31/20", // per share: 1.55 a contract's share
				"T61001505 leg 1": "52",
				"T61001505 leg 2": "31",
				"T61001506":       "61/2", // the receiving side's, the delivering side having none
				"T61001507":       "61/2",
				"Cb0c1d2e3.1":     "125/4",
				"Cb0c1d2e3.2":     "30",
				"T61001503":       "50",
				"Cb0c1d2e3.6":     "30",
				"Cb0c1d2e3.7":     "30",
			} {
				if prices[key] != want {
					t.Errorf("%s %s: price %s, want %s", tool, key, prices[key], want)
				}
			}
			for _, e := range events {
				if e.ref == "T61001503" && e.amount != -2500 {
					t.Errorf("%s: the cash not written is %s", e.ref, formatCents(e.amount))
				}
			}
		}
	})

	t.Run("securities", func(t *testing.T) {
		t.Parallel()
		in := toolCaseNamed("securities").in
		type want struct {
			ppTicker, gfSymbol, manual, name, gfName string
			ppManual                                 bool
			// why are its whys of PP's symbols_changed (whysOf).
			why string
		}
		for _, c := range []struct {
			key, symbol string
			want        want
		}{
			{keyXYZ, "", want{"XYZ", "XYZ", "", "XYZ CORP COMMON STOCK", "Security (XYZ)", false, ""}},
			{keyClass, "", want{"ABC-B", "ABC-B", "", "ABC HOLDINGS CL B", "Security (ABC-B)", false, ""}},
			{keyDot, "", want{"", uuid8(toolNamespace, "security ticker:QRS.PRK"), "not_plain", "QRS TRUST PFD SER K", "Security (QRS.PRK)", true, ""}},
			{keyGone, "", want{"", uuid8(toolNamespace, "security cusip:ZZQ00K102"), "delisted", "OLDCO INC", "Security (ZZQ00K102)", true, "no_ticker delisted"}},
			{keyRename, "", want{"PQRS", "PQRS", "", "PQR GROUP INC", "Security (PQRS)", false, "renamed"}},
			{"", "QQQX", want{"QQQX", "QQQX", "", "", "Security (QQQX)", false, ""}},
			{keyNoTick, "", want{"", uuid8(toolNamespace, "security cusip:ZZC99X101"), "no_ticker", "EXAMPLE PRIVATE PLACEMENT UNITS", "Security (ZZC99X101)", true, ""}},
			{keyPut, "", want{"XYZ250221P00040000", "fc94e404-4c85-87f0-962e-bb88e7134119", "option", "XYZ 02/21/2025 40.00 P", "XYZ 2025-02-21 40 put (XYZ250221P00040000)", true, ""}},
			{keyFileOp, "", want{"XYZ250321C00045000", uuid8(toolNamespace, "security option:XYZ250321C00045000"), "option", "CALL XYZ CORP $45 EXP 03/21/25", "XYZ 2025-03-21 45 call (XYZ250321C00045000)", true, ""}},
			{keyFutOpt, "", want{symFutOpt, uuid8(toolNamespace, "security key:"+keyFutOpt), "option", "EXAMPLE INDEX FUTURE PUT 5050 MAR 25", "Security (i:205187341)", true, ""}},
			// The first of its symbols in order, not the latest, gives a
			// renamed contract's OCC form, which stays.
			{keyOptRen, "", want{"PQR250620C00030000", uuid8(toolNamespace, "security option:PQR250620C00030000"), "option", "PQR 06/20/2025 30.00 C", "PQR 2025-06-20 30 call (PQR250620C00030000)", true, ""}},
			{keyBondA, "", want{"", uuid8(toolNamespace, "security cusip:9128ZZQ15"), "bond", "EXAMPLE TREASURY NOTE 4.25% 08/15/2034", "Bond (9128ZZQ15)", true, ""}},
			{keyBondB, "", want{"", uuid8(toolNamespace, "security cusip:ZZT10A109"), "bond", "EXAMPLE CITY REVENUE BOND 5% 06/01/2040", "Bond (ZZT10A109)", true, ""}},
			// A bond with a ticker is a bond all the same; an option whose
			// CUSIP the store knows is known by it; a class share spelled
			// two ways has not changed its symbol.
			{keyBondT, "", want{"ZZTB", uuid8(toolNamespace, "security ticker:ZZTB"), "bond", "EXAMPLE BABY BOND 6.5% 2031", "Bond (ZZTB)", true, ""}},
			{keyOptCU, "", want{"XYZ250620C00055000", uuid8(toolNamespace, "security cusip:ZZO55C106"), "option", "XYZ 06/20/2025 55.00 C", "XYZ 2025-06-20 55 call (ZZO55C106)", true, ""}},
			{keyClass2, "", want{"ABD-B", "ABD-B", "", "ABD CORP CL B", "Security (ABD-B)", false, ""}},
			// Ghostfolio asks Yahoo about a ticker of up to six letters.
			{keySix, "", want{"ABCDEF", "ABCDEF", "", "SIX LETTER TICKER INC", "Security (ABCDEF)", false, ""}},
			{keySeven, "", want{"ABCDEFG", uuid8(toolNamespace, "security ticker:ABCDEFG"), "not_plain", "SEVEN LETTER TICKER INC", "Security (ABCDEFG)", false, ""}},
		} {
			s := toolSecurityOf(&in, c.key, c.symbol)
			got := want{s.ppTicker(), s.gfSymbol(), s.manual, s.name, s.gfName(), s.ppManual(), whysOf(s)}
			if got != c.want {
				t.Errorf("%s%s: %+v,\nwant %+v", c.key, c.symbol, got, c.want)
			}
			if wantSource := map[bool]string{true: "MANUAL", false: "YAHOO"}[c.want.manual != ""]; s.gfDataSource() != wantSource {
				t.Errorf("%s%s: data source %s", c.key, c.symbol, s.gfDataSource())
			}
		}
		// The delisted security keeps its ticker of old, which a tool may
		// hold it under; the renamed one has had two.
		if s := toolSecurityOf(&in, keyGone, ""); !s.delisted || s.cusip != "ZZQ00K102" || !slices.Equal(s.tickers, []string{"OLDCO"}) || s.symbol != "ZZQ00K102" {
			t.Errorf("the delisted security: %+v", s)
		}
		if s := toolSecurityOf(&in, keyRename, ""); s.ticker() != "PQRS" || !slices.Equal(s.tickers, []string{"PQRS", "PQR"}) {
			t.Errorf("the renamed security has tickers %v", s.tickers)
		}
		// Of two tickers, PP is told of a rename only when it gets the
		// latest: not of one that keeps a dot in Yahoo's form, which PP finds
		// by its WKN or name, nor of one delisted, or whose ticker another
		// keeps or none does, which is no_ticker.
		for _, c := range []struct {
			s    toolSecurity
			want string
		}{
			{toolSecurity{assetType: "EQUITY", tickers: []string{"NEWN", "OLDN"}, writes: true}, "renamed"},
			{toolSecurity{assetType: "EQUITY", tickers: []string{"QRS.PRL", "QRS.PRK"}, writes: true}, ""},
			{toolSecurity{assetType: "EQUITY", tickers: []string{"NEWN", "OLDN"}, delisted: true}, "no_ticker delisted"},
			{toolSecurity{assetType: "EQUITY", tickers: []string{"NEWN", "OLDN"}, reusedBy: "i:1"}, "no_ticker reused"},
			{toolSecurity{assetType: "EQUITY", tickers: []string{"NEWN", "OLDN"}}, "no_ticker undecided"},
			{toolSecurity{assetType: "EQUITY", tickers: []string{"NEWN", "OLDN"}, unsure: true}, ""},
		} {
			if got := whysOf(&c.s); got != c.want {
				t.Errorf("tickers %q, delisted %v, writes %v, reused by %q: symbols changed %q, want %q", c.s.tickers, c.s.delisted, c.s.writes, c.s.reusedBy, got, c.want)
			}
		}
		// Multipliers: an option's, assumed for a file's option and for a
		// row's contract without a key, refused when not whole.
		for _, c := range []struct {
			key, symbol             string
			m                       int64
			assumed, bad, bond      bool
			assetType, cusip, ident string
		}{
			{keyPut, "", 100, false, false, false, "OPTION", "", "XYZ250221P00040000"},
			{keyFutOpt, "", 50, false, false, false, "OPTION", "", keyFutOpt},
			{keyFileOp, "", 100, true, false, false, "OPTION", "", "XYZ250321C00045000"},
			{"", symPut, 100, true, false, false, "OPTION", "", "XYZ250221P00040000"},
			{keyBadM, "", 1, false, true, false, "OPTION", "", "XYZ1250321C00045000"},
			{keyFuture, "", 1, false, false, false, "FUTURE", "", keyFuture},
			{keyBondB, "", 1, false, false, true, "", "ZZT10A109", "ZZT10A109"},
			{"", "ZZT10A109", 1, false, false, true, "", "ZZT10A109", "ZZT10A109"},
			{keyABCD, "", 1, false, false, false, "EQUITY", "ZZB123458", "ABCD"},
		} {
			s := toolSecurityOf(&in, c.key, c.symbol)
			if s.multiplier != c.m || s.assumed != c.assumed || s.badMultiplier != c.bad || s.bond != c.bond || s.assetType != c.assetType || s.cusip != c.cusip || s.identity() != c.ident {
				t.Errorf("%s%s: multiplier %d (assumed %v, bad %v), bond %v, %q, CUSIP %q, identity %q", c.key, c.symbol, s.multiplier, s.assumed, s.badMultiplier, s.bond, s.assetType, s.cusip, s.identity())
			}
		}
		if s := toolSecurityOf(&in, "", ""); s != nil {
			t.Errorf("no key and no symbol give %+v", s)
		}
		// The names whose UUIDs are MANUAL assets' symbols in Ghostfolio:
		// by the ticker in Yahoo's form, whatever CUSIP the store knows,
		// but for an option, a future, a delisted security and a bond with
		// a CUSIP; else CUSIP, else OCC form, else key, else a row's
		// symbol.
		for _, c := range []struct{ key, symbol, want string }{
			{keyXYZ, "", "security ticker:XYZ"},
			{keyRename, "", "security ticker:PQRS"},
			{keyGone, "", "security cusip:ZZQ00K102"},
			{keyBondA, "", "security cusip:9128ZZQ15"},
			{keyBondT, "", "security ticker:ZZTB"},
			{keyNoTick, "", "security cusip:ZZC99X101"},
			{keyOptCU, "", "security cusip:ZZO55C106"},
			{keyPut, "", "security option:XYZ250221P00040000"},
			{"", symPut, "security option:XYZ250221P00040000"},
			{keyFutOpt, "", "security key:" + keyFutOpt},
			{keyFuture, "", "security key:" + keyFuture},
			{keyClass, "", "security ticker:ABC-B"},
			{"", "QQQX", "security ticker:QQQX"},
			{"", "ZZT10A109", "security cusip:ZZT10A109"},
		} {
			if got := toolSecurityOf(&in, c.key, c.symbol).uuidName(); got != c.want {
				t.Errorf("%s%s: %q, want %q", c.key, c.symbol, got, c.want)
			}
		}
		// A bond with a ticker whose CUSIP the store knows is known by the
		// CUSIP.
		bondCUSIP := in
		bondCUSIP.securities = slices.Clone(in.securities)
		for i := range bondCUSIP.securities {
			if bondCUSIP.securities[i].key == keyBondT {
				bondCUSIP.securities[i].cusip = "ZZT65B101"
			}
		}
		if s := toolSecurityOf(&bondCUSIP, keyBondT, ""); s.identity() != "ZZT65B101" || s.uuidName() != "security cusip:ZZT65B101" || s.gfName() != "Bond (ZZT65B101)" {
			t.Errorf("a bond with a ticker and a CUSIP: %q, %q, %q", s.identity(), s.uuidName(), s.gfName())
		}
		// A security's Ghostfolio name and symbol come from its identity
		// alone, the same whichever description the store saw first or
		// last; the description it saw first is the name the comment and
		// PP take.
		flipped := in
		flipped.names = slices.Clone(in.names)
		for i, n := range flipped.names {
			if n.key == keyXYZ && n.kind == "description" {
				flipped.names[i].firstSeen = n.firstSeen.AddDate(0, 0, map[string]int{"XYZ CORP": -10, "XYZ CORP COMMON STOCK": 10}[n.value])
			}
		}
		a, b := toolSecurityOf(&in, keyXYZ, ""), toolSecurityOf(&flipped, keyXYZ, "")
		if a.gfName() != b.gfName() || a.gfSymbol() != b.gfSymbol() || a.uuidName() != b.uuidName() || b.name != "XYZ CORP" {
			t.Errorf("descriptions flipped: %q %q %q, name %q; was %q %q %q", b.gfName(), b.gfSymbol(), b.uuidName(), b.name, a.gfName(), a.gfSymbol(), a.uuidName())
		}
	})

	t.Run("bond unit", func(t *testing.T) {
		t.Parallel()
		// A bond follows face value and prices per 100 when each trade
		// costs 0.8 to 1.25 times its face at its price, and each snapshot
		// price is 0.2 to 5 a dollar of face; a leg of a trade of several
		// counts too, but not a leg of a journal, which costs nothing. A
		// bond that a row names by its symbol alone, its CUSIP, is one with
		// the key of that CUSIP: the rows and legs of either count for both.
		trade := func(gross int64) toolRow {
			return toolRow{
				ref: "Cf9e8d7c6.1", account: 1, source: "file", day: "2025-03-03", posted: "2025-03-03", kind: "Buy", category: "buy",
				securityKey: keyBondB, symbol: "ZZT10A109", quantity: centsOf(1_000_000_000), price: tcPrice(100),
				amount: centsOf(gross), fees: centsOf(0), gross: centsOf(gross),
			}
		}
		several := func(ref, kind, category string, amount int64) []toolRow {
			return []toolRow{{
				ref: ref, account: 1, source: "api", time: tcNY("2025-03-04 10:00:00"), kind: kind, category: category,
				amount: centsOf(amount), flags: []string{"mixed_legs"},
			}}
		}
		leg := func(ref string, cost int64) []toolLeg {
			return []toolLeg{{account: 1, ref: ref, leg: 2, symbol: "ZZT10A109", securityKey: keyBondB, quantity: centsOf(1_000_000_000), cost: centsOf(cost), price: tcPrice(100)}}
		}
		// A trade and a leg without a security key, of the bond's symbol or
		// another.
		keylessTrade := func(symbol string, gross int64) []toolRow {
			r := trade(gross)
			r.securityKey, r.symbol = "", symbol
			return []toolRow{r}
		}
		keylessLeg := func(symbol string, cost int64) []toolLeg {
			l := leg("T61002901", cost)
			l[0].securityKey, l[0].symbol = "", symbol
			return l
		}
		price := func(value float64) toolPosition {
			return toolPosition{key: keyBondB, account: 1, marketDate: "2025-06-30", fetchedAt: tcNY("2025-06-30 18:00:00"), long: centsOf(1_000_000_000), marketValue: tcPrice(value)}
		}
		for _, c := range []struct {
			name      string
			rows      []toolRow
			legs      []toolLeg
			positions []toolPosition
			fails     bool
			keyless   bool // the bond is a row's symbol without a key
		}{
			{"a trade at 0.8 of face", []toolRow{trade(-80_000)}, nil, nil, false, false},
			{"a trade below 0.8", []toolRow{trade(-79_999)}, nil, nil, true, false},
			{"a trade at 1.25 of face", []toolRow{trade(-125_000)}, nil, nil, false, false},
			{"a trade above 1.25", []toolRow{trade(-125_001)}, nil, nil, true, false},
			{"a leg above 1.25", several("T61002901", "TRADE", "buy", -125_001), leg("T61002901", -125_001), nil, true, false},
			{"a leg at 1.25", several("T61002901", "TRADE", "buy", -125_000), leg("T61002901", -125_000), nil, false, false},
			{"a journal's leg", several("T61002902", "JOURNAL", "journal", 0), leg("T61002902", 0), nil, false, false},
			{"a keyless trade above 1.25", keylessTrade("ZZT10A109", -125_001), nil, nil, true, true},
			{"a keyless trade of another symbol", keylessTrade("ZZT10B107", -125_001), nil, nil, false, true},
			{"a keyless leg above 1.25", several("T61002901", "TRADE", "buy", -125_001), keylessLeg("ZZT10A109", -125_001), nil, true, true},
			{"a keyless leg of another symbol", several("T61002901", "TRADE", "buy", -125_001), keylessLeg("ZZT10B107", -125_001), nil, false, true},
			{"a leg of the key above 1.25", append(keylessTrade("ZZT10A109", -100_000), several("T61002901", "TRADE", "buy", -125_001)...), leg("T61002901", -125_001), nil, true, true},
			{"a keyless leg of the key's CUSIP above 1.25", several("T61002901", "TRADE", "buy", -125_001), keylessLeg("ZZT10A109", -125_001), nil, true, false},
			{"a price of 0.2", nil, nil, []toolPosition{price(200)}, false, false},
			{"a price below 0.2", nil, nil, []toolPosition{price(199.99)}, true, false},
			{"a price of 5", nil, nil, []toolPosition{price(5000)}, false, false},
			{"a price above 5", nil, nil, []toolPosition{price(5000.01)}, true, false},
		} {
			in := tcInput(toolInput{rows: c.rows, legs: c.legs, positions: c.positions})
			key, symbol := keyBondB, ""
			if c.keyless {
				key, symbol = "", "ZZT10A109"
			}
			if s := toolSecurityOf(&in, key, symbol); !s.bond || s.bondUnit != c.fails {
				t.Errorf("%s: a bond %v whose unit is off: %v", c.name, s.bond, s.bondUnit)
			}
		}
		// A bond bought in a History file's rows, under its key, and sold in
		// the API's, under the API's key, which knows its CUSIP: when the
		// API counts it in bonds of $1000, not in dollars of face, its sale
		// and its price are off the unit, so both rows are left out, with
		// every price, as of one security, and neither tool keeps a holding
		// that no row closes; the holdings check, whose snapshot holds none,
		// finds no difference. In dollars of face, both are written.
		for _, off := range []bool{true, false} {
			face := int64(10_000_000_000) // $10,000
			if off {
				face = 10_000_000 // 10 bonds
			}
			c := toolCaseNamed("bonds")
			c.in = tcInput(toolInput{
				securities: append(slices.Clone(c.in.securities), toolSecurityRow{key: keyBondAPI, assetType: "FIXED_INCOME", cusip: "ZZT10A109", symbol: "ZZT10A109"}),
				names: append(slices.Clone(c.in.names),
					toolName{key: keyBondAPI, kind: "symbol", value: "ZZT10A109", firstSeen: tcNY("2026-06-01 10:00:00"), lastSeen: tcNY("2026-06-01 10:00:00")}),
				rows: []toolRow{
					rcCash("T61009900", 1, "2025-05-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 2_000_000),
					rcFile("Cf7a8b9d0.1", 1, "2025-06-01", "Buy", "buy", keyBondB, "ZZT10A109", centsOf(10_000_000_000), 99.5, -995_000),
					rcTrade("T61009901", 1, "2026-06-01 10:00:00", "sell", keyBondAPI, "ZZT10A109", -face, 99.6, 996_000, 0),
				},
				positions: []toolPosition{{key: keyBondAPI, account: 1, marketDate: "2026-05-29", fetchedAt: tcNY("2026-05-29 18:05:00"), long: centsOf(face), marketValue: tcPrice(9_955)}},
				snapshots: []toolSnapshot{rcSnapshot(1, "2026-11-10", 2_001_000)},
			})
			for _, tool := range []string{toolPP, toolGF} {
				in, events, outcomes := mapCase(c, tool)
				for _, o := range outcomes[1:] {
					if (o.outcome == outcomeLeftOut && o.reason == "bond_unit") != off {
						t.Errorf("two keys, off %v, %s: %s %s %s", off, tool, o.ref, o.outcome, o.reason)
					}
				}
				if diffs, _ := holdingsCheck(in, events); len(diffs) != 0 {
					t.Errorf("two keys, off %v, %s: differences %+v", off, tool, diffs)
				}
				for _, l := range listSecurities(in, events) {
					if l.security.bond && len(l.prices) > 0 == off {
						t.Errorf("two keys, off %v, %s: %s has prices %v", off, tool, l.security.key, l.prices)
					}
				}
			}
		}
	})

	t.Run("records", func(t *testing.T) {
		t.Parallel()
		// PP takes a security on a dividend, a fee or a tax, and none on
		// interest; Ghostfolio's fees, taxes, interest and refunds are of
		// a cash profile, never of the security, and no dividend is.
		for _, tool := range []string{toolPP, toolGF} {
			in, events, _ := mapCase(toolCaseNamed("income"), tool)
			for _, e := range events {
				hasSecurity := e.security != nil
				var want bool
				switch r := in.rows[e.row]; {
				case e.kind == evBuy || e.kind == evDividend:
					want = true
				case tool == toolPP && (e.kind == evFees || e.kind == evFeesRefund || e.kind == evTaxes || e.kind == evTaxRefund):
					want = r.securityKey != "" || r.symbol != ""
				}
				if hasSecurity != want {
					t.Errorf("%s %s (%s) has a security: %v", tool, e.ref, e.kind, hasSecurity)
				}
				if tool == toolGF && e.kind.gfType() == "DIVIDEND" && e.profile != "" {
					t.Errorf("%s: a dividend of the %s profile", e.ref, e.profile)
				}
				if tool == toolPP && e.profile != "" {
					t.Errorf("%s: PP's record names the %s profile", e.ref, e.profile)
				}
			}
		}
		// A sale whose fees exceed its proceeds: PP's Sell of the gross,
		// without fees, and its Fees row of the fees, of the security;
		// Ghostfolio's one SELL with every fee. A dividend's gross is its
		// cash and fees.
		for _, tool := range []string{toolPP, toolGF} {
			_, events, _ := mapCase(toolCaseNamed("trades"), tool)
			var got []string
			for _, e := range events {
				if e.ref == "T61001306" {
					got = append(got, fmt.Sprintf("%s %s fees %s gross %s amount %s %s %v", e.kind, formatCents(e.value), formatCents(e.fees),
						formatCents(e.gross), formatCents(e.amount), e.security.symbol, e.caveats))
				}
			}
			want := []string{"sell 0.05 fees 0.10 gross 0.05 amount -0.05 EFGH []"}
			if tool == toolPP {
				want = []string{"sell 0.05 fees 0.00 gross 0.05 amount -0.05 EFGH [fees_apart]", "fees 0.10 fees 0.00 gross 0.00 amount -0.05 EFGH [fees_apart]"}
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: the sale whose fees exceed it: %q, want %q", tool, got, want)
			}
		}
		_, events, _ := mapCase(toolCaseNamed("income"), toolGF)
		for _, e := range events {
			if e.ref == "Cc0d1e2f3.1" && (e.gross != 1000 || e.fees != 50 || e.value != 950) {
				t.Errorf("a dividend with fees: value %s, fees %s, gross %s", formatCents(e.value), formatCents(e.fees), formatCents(e.gross))
			}
		}
		// A buy's gross, which Ghostfolio's unit price is of, is its cash
		// less its fees; a rebate and fees off the grid ride in it.
		_, events, _ = mapCase(toolCaseNamed("trades"), toolGF)
		for _, want := range []struct {
			ref         string
			gross, fees int64
		}{{"T61001301", -50_000, 65}, {"T61001307", -5950, 0}, {"Cf0e1d2c3.8", -30_150, 0}, {"T61001302", 22_000, 3}} {
			i := slices.IndexFunc(events, func(e toolEvent) bool { return e.ref == want.ref })
			if e := events[i]; e.gross != want.gross || e.fees != want.fees {
				t.Errorf("%s: gross %s, fees %s", want.ref, formatCents(e.gross), formatCents(e.fees))
			}
		}
		// The two-row sale carries both rows' refs.
		_, events, _ = mapCase(toolCaseNamed("two_row"), toolPP)
		if i := slices.IndexFunc(events, func(e toolEvent) bool { return e.ref == "C0a1b2c3d.2" }); events[i].with != "C0a1b2c3d.3" || events[i].security.key != keyABCD {
			t.Errorf("the two-row sale: with %q, of %v", events[i].with, events[i].security)
		}
	})

	t.Run("flags", func(t *testing.T) {
		t.Parallel()
		for _, tool := range []string{toolPP, toolGF} {
			_, _, outcomes := mapCase(toolCaseNamed("flags"), tool)
			got := map[string]string{}
			for _, o := range outcomes {
				got[o.ref] = strings.Join(o.flags, ",")
			}
			for ref, want := range map[string]string{
				"T61002201": "", "Cb1c2d3e4.1": "near_duplicate", "Cb1c2d3e4.2": "counts_differ", "T61002202": "late_api_row",
				"T61002203": "unexpected_sign", "Cb1c2d3e4.3": "restated_quantity", "Cb1c2d3e4.4": "", "Cb1c2d3e4.5": "",
			} {
				if got[ref] != want {
					t.Errorf("%s %s is listed with the flags %q, want %q", tool, ref, got[ref], want)
				}
			}
		}
	})

	t.Run("Ghostfolio dates", func(t *testing.T) {
		t.Parallel()
		_, events, _ := mapCase(toolCaseNamed("gf_dates"), toolGF)
		dates := map[string]string{}
		for _, e := range events {
			key := e.ref
			if e.leg.Valid {
				key += " leg " + strconv.FormatInt(e.leg.Int64, 10)
			}
			dates[key] = e.gfDate()
		}
		// In this order, each after the one before: a day's opening rows
		// before its closes, a timeless purchase before a timeless sale, an
		// evening fill booked on the next day before that day's timed sale.
		order := []string{
			"T61002501", "Cc3d4e5f6.1", "T61002502", "T61002503", "Cc3d4e5f6.2", "Cc3d4e5f6.3", "T61002504", "T61002505",
			"T61002506 leg 4", "Cc3d4e5f6.4",
		}
		for i := 1; i < len(order); i++ {
			if dates[order[i-1]] >= dates[order[i]] {
				t.Errorf("%s at %s is not before %s at %s", order[i-1], dates[order[i-1]], order[i], dates[order[i]])
			}
		}
		// Two purchases without a time share one instant, and a trade's legs
		// their row's, but a SELL's, a millisecond later.
		if a, b := dates["Cc3d4e5f6.4"], dates["Cc3d4e5f6.5"]; a != b || a == "" {
			t.Errorf("two timeless purchases at %s and %s", a, b)
		}
		for key, want := range map[string]string{
			"T61002501":       "2025-01-21T12:01:19.824Z",
			"Cc3d4e5f6.1":     "2025-01-21T12:02:52.801Z",
			"Cc3d4e5f6.2":     "2025-01-23T12:00:00.000Z",
			"Cc3d4e5f6.3":     "2025-01-23T12:00:00.001Z",
			"T61002504":       "2025-01-24T12:00:00.000Z",
			"T61002505":       "2025-01-24T12:01:15.601Z",
			"T61002506 leg 4": "2025-01-27T12:01:26.400Z",
			"T61002506 leg 5": "2025-01-27T12:01:26.401Z",
		} {
			if dates[key] != want {
				t.Errorf("%s at %s, want %s", key, dates[key], want)
			}
		}
		// A journal of shares between two accounts whose sides are of one
		// second: Ghostfolio, which holds the security as one across every
		// account, gets the BUY first.
		_, deliveries, _ := mapCase(toolCaseNamed("deliveries"), toolGF)
		journal := map[string]string{}
		for _, e := range deliveries {
			journal[e.ref] = e.gfDate()
		}
		if out, in := journal["T61001506"], journal["T61001507"]; out != "2026-09-01T12:01:12.001Z" || in != "2026-09-01T12:01:12.000Z" {
			t.Errorf("the journal of one second: its SELL at %s, its BUY at %s", out, in)
		}
		// PP's Time: an evening fill booked on the next day has none.
		for _, e := range events {
			seconds, ok := e.clock()
			switch e.ref {
			case "T61002504", "Cc3d4e5f6.2":
				if ok {
					t.Errorf("%s has a time on its day: %d", e.ref, seconds)
				}
			case "T61002501":
				if !ok || seconds != 39_912 {
					t.Errorf("%s: %d, %v", e.ref, seconds, ok)
				}
			}
		}
	})

	t.Run("Ghostfolio cash", func(t *testing.T) {
		t.Parallel()
		in := toolCaseNamed("gf_cash").in
		got := map[int64][]string{}
		for account, bs := range toolBalances(&in) {
			for _, b := range bs {
				got[account] = append(got[account], string(b.date)+" "+formatCents(b.cash))
			}
		}
		want := map[int64][]string{1: {"2026-11-09 1000.00", "2026-11-10 1200.00"}, 2: {"2026-11-10 -12.34"}}
		if len(got) != len(want) || !slices.Equal(got[1], want[1]) || !slices.Equal(got[2], want[2]) {
			t.Errorf("balances %v, want %v", got, want)
		}
		// One account selected: its balances alone.
		in.account = 2
		if bs := toolBalances(&in); len(bs) != 1 || len(bs[2]) != 1 {
			t.Errorf("account 2's balances: %v", bs)
		}
		// A date's balance is its latest snapshot's, whatever their order.
		in.snapshots = []toolSnapshot{rcSnapshot(2, "2026-11-11", 100), {account: 2, marketDate: "2026-11-11", fetchedAt: tcNY("2026-11-11 20:00:00"), cash: centsOf(200)}}
		if bs := toolBalances(&in); len(bs[2]) != 1 || bs[2][0].cash != 200 {
			t.Errorf("two snapshots of a date, the later last: %v", bs)
		}
	})

	t.Run("holdings", func(t *testing.T) {
		t.Parallel()
		for _, tool := range []string{toolPP, toolGF} {
			in, events, _ := mapCase(toolCaseNamed("holdings"), tool)
			diffs, unchecked := holdingsCheck(in, events)
			var got []string
			for _, d := range diffs {
				got = append(got, fmt.Sprintf("%s %s %s %s: %s held, %s written", d.account.label(), d.key, d.symbol, d.snapshot, formatMicros(d.snapshotShares), formatMicros(d.writtenShares)))
			}
			want := []string{
				"A1 ...135 i:1000002 EFGH 2026-11-10: 20 held, 10 written",
				"A1 ...135 i:1854092 XYZ 2026-11-10: 5 held, -10 written",
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s: differences\n%s\nwant\n%s", tool, strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
			if len(unchecked) != 1 || unchecked[0].label() != "A3 ...783" {
				t.Errorf("%s: unchecked %v", tool, unchecked)
			}
			// stderr names A3 as an account with rows and no snapshot.
			if e := caseExport(t, toolCaseNamed("holdings"), tool); !slices.Equal(e.NoSnapshot, []string{"A3 ...783"}) || len(e.NotInTool) != 0 {
				t.Errorf("%s: no snapshot %q, nothing in the tool %q", tool, e.NoSnapshot, e.NotInTool)
			}
		}
		// A security that the snapshot does not hold, that the rows do,
		// is a difference too; an export of one account checks it alone.
		in, events, _ := mapCase(toolCaseNamed("trades"), toolPP)
		in.snapshots = []toolSnapshot{{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00")}}
		diffs, _ := holdingsCheck(in, events)
		var got []string
		for _, d := range diffs {
			got = append(got, fmt.Sprintf("%s %s %s", d.key, formatMicros(d.snapshotShares), formatMicros(d.writtenShares)))
		}
		if want := []string{keyABCD + " 0 3", keyEFGH + " 0 1.99", keyXYZ + " 0 6"}; !slices.Equal(got, want) {
			t.Errorf("held nothing: %q, want %q", got, want)
		}
		in, events, _ = mapCase(toolCaseNamed("holdings"), toolPP)
		in.account = 3
		if diffs, unchecked := holdingsCheck(in, events); len(diffs) != 0 || len(unchecked) != 1 {
			t.Errorf("account 3 alone: %v, %v", diffs, unchecked)
		}
		// An option whose multiplier is not whole is held, but no record of
		// it is ever written: it is not checked.
		held := tcInput(toolInput{
			snapshots: []toolSnapshot{rcSnapshot(1, "2026-11-10", 0)},
			positions: []toolPosition{rcPosition("2026-11-10", keyBadM, 1_000_000, 250)},
		})
		if diffs, _ := holdingsCheck(&held, nil); len(diffs) != 0 {
			t.Errorf("an option of multiplier 2.5 held: %+v", diffs)
		}

		// A3 has snapshots, of 7 EFGH and 2500.00, and no row: PP's export
		// writes and names nothing of it, and you make no PP account for it,
		// so neither of PP's checks checks it, nor lists anything to add by
		// hand; the manifest lists it as not checked. Once a row of it is
		// written, PP holds it, and both checks check it. Ghostfolio's file
		// makes the account, with its balances, and its holdings check
		// checks it from the first export.
		snapshotOnly := func(rows ...toolRow) toolInput {
			p := rcPosition("2026-02-26", keyEFGH, 7_000_000, 210)
			p.account = 3
			return tcInput(toolInput{
				to:        "2026-02-27",
				rows:      append([]toolRow{rcTrade("T7c620101", 1, "2026-02-02 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5_000, 0)}, rows...),
				snapshots: []toolSnapshot{rcSnapshot(1, "2026-02-26", -5_000), rcSnapshot(3, "2026-02-26", 250_000)},
				positions: []toolPosition{p, rcPosition("2026-02-26", keyXYZ, 1_000_000, 50)},
			})
		}
		checked := func(s *chainStep) []string {
			var out []string
			for _, h := range s.res.holdings {
				out = append(out, fmt.Sprintf("%s %s %s %s", h.Account, h.Symbol, h.SnapshotShares, h.WrittenShares))
			}
			for _, c := range s.res.cash {
				out = append(out, fmt.Sprintf("%s cash %s %s", c.Account, c.SnapshotCash, c.WrittenCash))
			}
			for _, a := range s.res.unchecked {
				out = append(out, a.label()+" not checked")
			}
			for _, a := range s.e.NoSnapshot {
				out = append(out, a+" no snapshot")
			}
			for _, a := range s.e.NotInTool {
				out = append(out, a+" nothing in the tool")
			}
			for _, a := range s.e.Accounts {
				out = append(out, fmt.Sprintf("%s exported %v new %v", a.Label, a.Exported, a.New))
			}
			holdings, cash := counted(s.e)
			return append(out, fmt.Sprintf("counted %d %d", holdings, cash))
		}
		deposit := rcCash("T7c620301", 3, "2026-02-20 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 250_000)
		e1 := chainStepOf(t, snapshotOnly(), toolPP, nil, rcAt1)
		e2 := chainStepOf(t, snapshotOnly(deposit), toolPP, e1, rcAt2)
		g1 := chainStepOf(t, snapshotOnly(), toolGF, nil, rcAt1)
		for _, c := range []struct {
			name string
			s    *chainStep
			want []string
		}{
			{"PP, A3 with snapshots alone", e1, []string{"A3 ...783 not checked", "A3 ...783 nothing in the tool", "A1 ...135 exported true new true", "counted 0 0"}},
			{"PP, A3 with a row since", e2, []string{
				"A3 ...783 EFGH 7 0", "A1 ...135 exported false new false", "A3 ...783 exported true new true", "counted 1 0",
			}},
			{"Ghostfolio, A3 with snapshots alone", g1, []string{
				"A3 ...783 EFGH 7 0", "A1 ...135 exported true new true", "A3 ...783 exported true new true", "counted 1 0",
			}},
		} {
			if got := checked(c.s); !slices.Equal(got, c.want) {
				t.Errorf("%s: %q, want %q", c.name, got, c.want)
			}
		}
	})

	t.Run("prices", func(t *testing.T) {
		t.Parallel()
		// A security's price of a market date is its latest snapshot's, of
		// any account, per share: per a contract's share for an option, per
		// dollar of face for a bond.
		got := map[string]string{}
		for _, name := range []string{"deliveries", "bonds", "holdings"} {
			in := toolCaseNamed(name).in
			ix := newToolIndex(&in)
			for _, p := range in.positions {
				for _, price := range ix.pricesOf(p.key, ix.security(p.key, "").multiplier) {
					got[name+" "+p.key+" "+string(price.date)] = roundRat(price.price, 8)
				}
			}
		}
		want := map[string]string{
			"deliveries i:1854092 2026-07-24":   "52.5", // the latest of the day, of any account
			"bonds c:9128ZZQ15 2025-06-30":      "0.995",
			"bonds " + keyBondB + " 2026-11-10": "1.005",
			"holdings i:1854092 2026-11-10":     "52.5", // the later of two snapshots of the day
			"holdings i:1854092 2026-06-01":     "0.00100301",
			"holdings i:1000002 2026-11-10":     "30",
			"holdings i:219900001 2026-11-10":   "1.55", // a short put of 2 contracts worth -310.00
			"holdings i:1000001 2026-11-10":     "40",
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("prices\n%v\nwant\n%v", got, want)
		}
		// A position without a market value or a quantity gives no price,
		// and the next of its date is taken; a price that rounds to 0 is
		// none, whatever the next of its date says; and none comes after
		// the export's last day.
		in := toolInput{to: "2026-11-12", positions: []toolPosition{
			{key: keyEFGH, account: 1, marketDate: "2026-01-02", fetchedAt: tcNY("2026-01-02 18:00:00"), long: centsOf(1_000_000_000_000), marketValue: tcPrice(0.000004)},
			{key: keyEFGH, account: 2, marketDate: "2026-01-02", fetchedAt: tcNY("2026-01-02 17:00:00"), long: centsOf(2_000_000), marketValue: tcPrice(60)},
			{key: keyEFGH, account: 1, marketDate: "2026-01-03", fetchedAt: tcNY("2026-01-03 18:00:00"), long: centsOf(1_000_000)},
			{key: keyEFGH, account: 2, marketDate: "2026-01-03", fetchedAt: tcNY("2026-01-03 17:00:00"), long: centsOf(2_000_000), marketValue: tcPrice(61)},
			{key: keyEFGH, account: 1, marketDate: "2026-01-05", fetchedAt: tcNY("2026-01-05 18:00:00"), long: centsOf(0), short: centsOf(0), marketValue: tcPrice(5)},
			{key: keyEFGH, account: 2, marketDate: "2026-01-05", fetchedAt: tcNY("2026-01-05 17:00:00"), long: centsOf(0), short: centsOf(1_000_000), marketValue: tcPrice(-33)},
			{key: keyEFGH, account: 2, marketDate: "2026-11-13", fetchedAt: tcNY("2026-11-13 17:00:00"), long: centsOf(2_000_000), marketValue: tcPrice(62)},
		}}
		in.securities, in.names = tcSecurities()
		var prices []string
		for _, p := range newToolIndex(&in).pricesOf(keyEFGH, 1) {
			prices = append(prices, string(p.date)+" "+roundRat(p.price, 8))
		}
		if want := []string{"2026-01-03 30.5", "2026-01-05 33"}; !slices.Equal(prices, want) {
			t.Errorf("prices %q, want %q", prices, want)
		}
		// The securities held, whose prices a file writes with no row.
		in = toolCaseNamed("holdings").in
		var held []string
		for _, s := range toolHeld(&in) {
			held = append(held, s.key)
		}
		if want := []string{keyABCD, keyEFGH, keyXYZ, keyPut27}; !slices.Equal(held, want) {
			t.Errorf("held %v, want %v", held, want)
		}
		// One account selected: what it held, not what another did; and
		// nothing after the export's last day.
		in = toolCaseNamed("idle").in
		in.positions = append(slices.Clone(in.positions), toolPosition{key: keyXYZ, account: 1, marketDate: "2026-11-13", fetchedAt: tcNY("2026-11-13 18:00:00"), long: centsOf(1)})
		held = nil
		for _, s := range toolHeld(&in) {
			held = append(held, s.key)
		}
		if want := []string{keyEFGH}; !slices.Equal(held, want) {
			t.Errorf("account 1 held %v, want %v", held, want)
		}
	})

	t.Run("securities written", func(t *testing.T) {
		t.Parallel()
		// Each file writes the securities that its records name, and those
		// a selected account held that it has prices of: prices of the
		// ones for which the tool has no feed, and none of a bond whose
		// unit is off; in the order of their keys, a row's symbol without a
		// key first.
		for _, c := range []struct {
			name, tool string
			want       []string
		}{
			{"holdings", toolPP, []string{keyABCD + " used", keyEFGH + " used", keyXYZ + " used", keyPut + " used", keyCall + " used", keyPut27 + " used 2026-11-10 1.55"}},
			{"holdings", toolGF, []string{keyABCD + " used", keyEFGH + " used", keyXYZ + " used", keyPut + " used", keyCall + " used", keyPut27 + " used 2026-11-10 1.55"}},
			{"bonds", toolPP, []string{keyBondA + " used 2025-06-30 0.995", keyBondB + " used"}},
			{"bonds", toolGF, []string{keyBondA + " used 2025-06-30 0.995"}},
			{"manual", toolPP, []string{"QQQX used", keyClass + " used", keyXYZ + " used"}},
			{"manual", toolGF, []string{"QQQX used", keyABCD + " 2026-11-10 41", keyClass + " used", keyXYZ + " used"}},
		} {
			in, events, _ := mapCase(toolCaseNamed(c.name), c.tool)
			var got []string
			for _, l := range listSecurities(in, events) {
				words := []string{cmp.Or(l.security.key, l.security.symbol)}
				if l.used {
					words = append(words, "used")
				}
				for _, p := range l.prices {
					words = append(words, string(p.date), roundRat(p.price, 8))
				}
				got = append(got, strings.Join(words, " "))
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("%s %s: %q,\nwant %q", c.name, c.tool, got, c.want)
			}
		}
		// A future and an option whose multiplier is not whole, held, are
		// not written: no record of either is.
		in, events, _ := mapCase(toolCaseNamed("manual"), toolGF)
		in.positions = append(slices.Clone(in.positions),
			toolPosition{key: keyFuture, account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), long: centsOf(1_000_000), marketValue: tcPrice(5000)},
			toolPosition{key: keyBadM, account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), long: centsOf(1_000_000), marketValue: tcPrice(250)},
		)
		if l := listSecurities(in, events); len(l) != 4 {
			t.Errorf("with a future and a bad multiplier held: %d securities", len(l))
		}
	})

	t.Run("manual", func(t *testing.T) {
		t.Parallel()
		in, events, _ := mapCase(toolCaseNamed("manual"), toolGF)
		unused, err := checkManual(in, events)
		if err != nil || !slices.Equal(unused, []string{"EFGH"}) {
			t.Errorf("unused %q, %v", unused, err)
		}
		for _, c := range []struct{ key, symbol, manual string }{
			{keyXYZ, "", "asked"}, {keyClass, "", "asked"}, {"", "QQQX", "asked"}, {keyEFGH, "", "asked"}, {keyABCD, "", "asked"}, {keyGone, "", "delisted"},
		} {
			if s := toolSecurityOf(in, c.key, c.symbol); s.manual != c.manual || s.gfSymbol() != uuid8(toolNamespace, s.uuidName()) && c.manual != "" {
				t.Errorf("%s%s: manual %q, symbol %s", c.key, c.symbol, s.manual, s.gfSymbol())
			}
		}
		// A class share, whatever its spelling, Schwab's, Yahoo's or yours,
		// is the one symbol of ABC.B, and is no typo.
		for _, spelling := range []string{"ABC.B", "ABC-B", "abc/b", "ABC B"} {
			c := toolCaseNamed("manual")
			c.in.manual = []string{spelling}
			in, events, _ := mapCase(c, toolGF)
			if _, err := checkManual(in, events); err != nil || toolSecurityOf(in, keyClass, "").manual != "asked" {
				t.Errorf("-manual %s: %q, %v", spelling, toolSecurityOf(in, keyClass, "").manual, err)
			}
		}
		// A symbol that no security has had is refused, said back masked.
		in.manual = []string{"XYZ", "6180 3398"}
		if _, err := checkManual(in, events); err == nil || err.Error() != "-manual names ...398, which no security in the store has" {
			t.Errorf("a typo: %v", err)
		}
		in.manual = []string{"", " "}
		if unused, err := checkManual(in, events); err != nil || unused != nil {
			t.Errorf("empty values: %q, %v", unused, err)
		}
	})

	t.Run("idle", func(t *testing.T) {
		t.Parallel()
		in, events, _ := mapCase(toolCaseNamed("idle"), toolGF)
		if got := yahooIdle(in, events); fmt.Sprint(got) != fmt.Sprint([]toolIdle{{symbol: "EFGH", key: keyEFGH, last: "2025-11-11"}}) {
			t.Errorf("idle %+v", got)
		}
		// Held in the account's latest snapshot, it is not idle.
		in.positions = append(in.positions, toolPosition{key: keyEFGH, account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), long: centsOf(1_000_000), marketValue: tcPrice(31)})
		if got := yahooIdle(in, events); len(got) != 0 {
			t.Errorf("idle though held: %+v", got)
		}
	})

	t.Run("two keys", func(t *testing.T) {
		t.Parallel()
		// One security that the store holds under two keys, a History
		// file's and the API's, whose rows the store linked, is one security
		// of each tool: one MANUAL asset that both its rows are of, -manual
		// naming it; one row of PP's securities; one holding that the
		// snapshot's shares match.
		in, events, _ := mapCase(toolCaseNamed("two_keys"), toolGF)
		profiles, assets := gfAssets(in, events)
		if len(profiles) != 1 || len(assets) != 1 || len(assets[0].securities) != 2 || profiles[0].Name != "Security (JKLM)" {
			t.Fatalf("assets: %+v", profiles)
		}
		for _, e := range events {
			if e.security.gfSymbol() != profiles[0].Symbol {
				t.Errorf("%s is of %s, not of the asset %s", e.ref, e.security.gfSymbol(), profiles[0].Symbol)
			}
		}
		// So it is once you say so in the store (-same), which makes the
		// file's key the API's: its rows, its names and its security. Without
		// the link or -same, the two keys, which know no CUSIP, are two, each
		// written on its own and listed with the other: their ticker alone
		// joins nothing.
		said := toolCaseNamed("two_keys")
		said.in = sameKey(said.in, keyTwoFile, keyTwoAPI)
		for _, tool := range []string{toolPP, toolGF} {
			e := caseExport(t, said, tool)
			diffs, _ := holdingsCheck(e.in, e.events)
			if len(e.Unsure) > 0 || len(diffs) > 0 || e.events[0].security != e.events[1].security && e.events[0].security.gfSymbol() != e.events[1].security.gfSymbol() {
				t.Errorf("-same, %s: unsure %q, differences %+v", tool, unsureText(e.Unsure), diffs)
			}
		}
		apart := toolCaseNamed("two_keys")
		apart.in.links = nil
		checkTwoKeysApart(t, "not linked", apart, twoKeys{}, false)
		// The asset is used when a row of either key is, and has the
		// prices of either, those of a date of the first key that has one.
		priced := toolCaseNamed("two_keys")
		priced.in.rows = priced.in.rows[:1]
		priced.in.positions = append(priced.in.positions,
			rcPosition("2025-06-02", keyTwoFile, 10_000_000, 1400), rcPosition("2026-11-10", keyTwoFile, 1_000_000, 181))
		in2, events2, _ := mapCase(priced, toolGF)
		profiles2, assets2 := gfAssets(in2, events2)
		var prices []string
		for _, p := range profiles2[0].MarketData {
			prices = append(prices, p.Date+" "+string(p.MarketPrice))
		}
		if !assets2[0].used || !slices.Equal(prices, []string{"2025-06-02T00:00:00.000Z 140", "2026-11-10T00:00:00.000Z 180"}) {
			t.Errorf("used %v, prices %q", assets2[0].used, prices)
		}
		inPP, eventsPP, _ := mapCase(toolCaseNamed("two_keys"), toolPP)
		if f := ppFileNamed(t, ppFiles(inPP, eventsPP), ppSecuritiesFile); f.rows != 1 {
			t.Errorf("PP's securities: %d rows", f.rows)
		}
		// A delivery of the file's key, with no price, on the day that the
		// API's key's position prices the security, is valued at that price,
		// which the asset and securities.csv take too.
		delivered := toolCaseNamed("two_keys")
		delivered.in.rows = append(slices.Clone(delivered.in.rows), rcFile("Cf7a8b9c0.3", 1, "2026-11-10", "Journaled Shares", "transfer_in", keyTwoFile, "JKLM", centsOf(2_000_000), 0, 0))
		delivered.in = tcInput(delivered.in)
		for _, tool := range []string{toolPP, toolGF} {
			_, events, _ := mapCase(delivered, tool)
			if e := events[len(events)-1]; e.ref != "Cf7a8b9c0.3" || e.value != 36_000 || e.price.Cmp(big.NewRat(180, 1)) != 0 || !slices.Equal(e.caveats, []string{"value_from_snapshot"}) {
				t.Errorf("%s: the delivery %s of %s: %s at %s, %v", tool, e.ref, e.security.key, formatCents(e.value), e.price.RatString(), e.caveats)
			}
		}
		// The keys are one security by a link of their rows, or by a CUSIP
		// that both know, and take every identifier from both: the API's key
		// renamed since, or delisted, which only it learns, the file's
		// knowing neither, its file exported before; the file's key renamed,
		// in a file exported since; a ticker that keeps a dot in Yahoo's
		// form, of which PP takes none; a class share that Schwab and the
		// file spell otherwise; a CUSIP under one key alone; and the
		// descriptions of either source, alike or not. The latest symbol is
		// the one that either key saw last. Each tool holds one security
		// still: PP finds every record's by one WKN, ticker or name, so that
		// securities.csv lists it once and PP holds the six shares in it;
		// Ghostfolio's activities are of one asset, which holds them; and the
		// holdings check finds no difference. Without the link, a ticker
		// joins nothing (I-1): a key that knows no CUSIP is written on its
		// own and listed, with each key of its ticker as a security that it
		// may be, whatever the ticker's history.
		for _, v := range []twoKeys{
			{
				name: "renamed", api: []string{"JKLM", "JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106", filed: tcFiledEarly,
				identity: "JKLN", symbols: "renamed",
			},
			{
				name: "renamed, a CUSIP under the API's key", api: []string{"JKLM", "JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", filed: tcFiledEarly,
				identity: "JKLN", symbols: "renamed",
			},
			{
				name: "renamed, no ticker alike, a CUSIP under both", api: []string{"JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106",
				filed: tcFiledEarly, identity: "JKLN", symbols: "renamed",
			},
			// -manual names the ticker that the file's key alone had.
			{
				name: "renamed, -manual naming the old ticker", api: []string{"JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106",
				filed: tcFiledEarly, manual: "JKLM", identity: "JKLN", why: "asked", symbols: "renamed",
			},
			// The newer ticker is the file's, which an export after the API's
			// sale names its rows by: the API's key, whose sale was its last
			// row, knows only the old one. The buy is of an earlier file, or
			// of the later one too, which restates it under the new ticker, as
			// the store's entry of it then is: either way, the file's key took
			// up the new ticker at the export that named it so, after the
			// API's sale under the old one, whatever the buy's day, and with a
			// CUSIP under the API's key or none, the two are one security.
			{
				name: "renamed under the file's key alone", api: []string{"JKLM"}, file: []string{"JKLM", "JKLN"}, apiCUSIP: "ZZJ12K106",
				identity: "JKLN", symbols: "renamed",
			},
			{
				name: "renamed under the file's key alone, its buy restated", api: []string{"JKLM"}, file: []string{"JKLM", "JKLN"}, apiCUSIP: "ZZJ12K106",
				buy: "JKLN", identity: "JKLN", symbols: "renamed",
			},
			{
				name: "renamed under the file's key alone, its buy restated, no CUSIP", api: []string{"JKLM"}, file: []string{"JKLM", "JKLN"},
				buy: "JKLN", identity: "JKLN", symbols: "renamed",
			},
			{
				name: "delisted, a CUSIP under the API's key", api: []string{"JKLM", "ZZJ12K106"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106",
				filed: tcFiledEarly, identity: "ZZJ12K106", why: "delisted", symbols: "no_ticker delisted",
			},
			{
				name: "delisted, described alike", api: []string{"JKLM", "ZZJ12K106"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106",
				apiName: "JKLM SMALL CAP VALUE ETF", fileName: "JKLM SMALL CAP VALUE ETF", filed: tcFiledEarly, identity: "ZZJ12K106", why: "delisted", symbols: "no_ticker delisted",
			},
			// A file that writes the CUSIP of a security that has gone, of the
			// buy too when it restates it.
			{
				name: "delisted under the file's key", api: []string{"JKLM"}, file: []string{"JKLM", "ZZJ12K106"}, apiCUSIP: "ZZJ12K106",
				identity: "ZZJ12K106", why: "delisted", symbols: "no_ticker delisted",
			},
			{
				name: "delisted under the file's key, its buy restated", api: []string{"JKLM"}, file: []string{"JKLM", "ZZJ12K106"}, apiCUSIP: "ZZJ12K106",
				buy: "ZZJ12K106", identity: "ZZJ12K106", why: "delisted", symbols: "no_ticker delisted",
			},
			{
				name: "dotted", api: []string{"JKL.PRA"}, file: []string{"JKL.PRA"}, apiName: "JKL TRUST PFD SER A", fileName: "JKL TRUST PFD SER A",
				identity: "JKL.PRA", why: "not_plain",
			},
			{
				name: "dotted, a CUSIP under the API's key", api: []string{"JKL.PRA"}, file: []string{"JKL.PRA"}, apiCUSIP: "ZZJ12K106",
				apiName: "JKL TRUST PFD SER A", fileName: "JKL TRUST 5.25% PFD A", identity: "JKL.PRA", why: "not_plain",
			},
			{name: "a class share spelled two ways, named by -manual", api: []string{"JKL/B"}, file: []string{"JKL.B"}, manual: "jkl/b", identity: "JKL-B", why: "asked"},
			// A third key had the ticker later, of another CUSIP: the file's
			// key, which the link makes one with the API's, is not another,
			// and the ticker is the security's, which an account holds, not
			// the third key's, which the API named by it later (I-5).
			{
				name: "a third key of the ticker, of another CUSIP", api: []string{"JKLM"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", other: "ZZJ12K205",
				identity: "JKLM",
			},
			{
				name: "a third key of the ticker, -manual naming it", api: []string{"JKLM"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", other: "ZZJ12K205",
				manual: "JKLM", identity: "JKLM", why: "asked",
			},
			{
				name: "a third key of the ticker, the API's delisted", api: []string{"JKLM", "ZZJ12K106"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106",
				other: "ZZJ12K205", filed: tcFiledEarly, identity: "ZZJ12K106", why: "delisted", symbols: "no_ticker delisted",
			},
		} {
			// knows reports whether a key of cusip and symbols knows a CUSIP.
			knows := func(cusip string, symbols []string) bool {
				return cusip != "" || slices.ContainsFunc(symbols, schwab.IsCUSIP)
			}
			for _, linked := range []bool{true, false} {
				c := v.toolCase()
				if !linked {
					c.in.links = nil
				}
				name := v.name
				if !linked {
					name += ", not linked"
				}
				if !linked && !knows(v.fileCUSIP, v.file) {
					checkTwoKeysApart(t, name, c, v, knows(v.apiCUSIP, v.api))
					continue
				}
				for _, tool := range []string{toolPP, toolGF} {
					in, events, _ := mapCase(c, tool)
					if diffs, _ := holdingsCheck(in, events); len(diffs) != 0 {
						t.Errorf("%s %s: differences %+v", name, tool, diffs)
					}
					for _, e := range events {
						if s := e.security; s.identity() != v.identity || s.manual != v.why || whysOf(s) != v.symbols {
							t.Errorf("%s %s: %s is of %s, MANUAL %q, symbols changed %q; want %s, %q, %q", name, tool, e.ref, s.identity(), s.manual, whysOf(s), v.identity, v.why, v.symbols)
						}
					}
					e := caseExport(t, c, tool)
					if len(e.Unsure) > 0 {
						t.Errorf("%s %s: unsure %q", name, tool, unsureText(e.Unsure))
					}
					// PP's manifest names a security with a why once, as PP holds
					// it, under the least of its keys with both keys, with every
					// name of each key as its source named it. stderr counts one.
					// Ghostfolio's has no symbols_changed.
					var manifest struct {
						SymbolsChanged *[]toolSymbolsEntry `json:"symbols_changed"`
					}
					if err := json.Unmarshal(e.manifest, &manifest); err != nil {
						t.Fatal(err)
					}
					names := map[string]bool{}
					for _, s := range v.api {
						names[symbolForm(s)+" api"] = true
					}
					for _, s := range v.file {
						names[symbolForm(s)+" history"] = true
					}
					changed := deref(manifest.SymbolsChanged)
					if (manifest.SymbolsChanged != nil) != (tool == toolPP) || (v.symbols != "" && tool == toolPP) != (len(changed) == 1) || e.SymbolsChanged != len(changed) || slices.ContainsFunc(changed, func(s toolSymbolsEntry) bool {
						got := map[string]bool{}
						for _, n := range s.Names {
							got[n.Symbol+" "+n.Source] = true
						}
						return strings.TrimSpace(strings.Join(s.Why, ",")+" "+s.Because) != v.symbols || s.SecurityKey != keyTwoAPI || !slices.Equal(s.SecurityKeys, []string{keyTwoAPI, keyTwoFile}) || !maps.Equal(got, names)
					}) {
						t.Errorf("%s %s: symbols changed %+v, counted %d", name, tool, changed, e.SymbolsChanged)
					}
					s := chainStepFrom(t, e, t.TempDir())
					if tool == toolPP {
						if f := ppFileNamed(t, s.pp, ppSecuritiesFile); f.rows != 1 {
							t.Errorf("%s: PP's securities: %d rows", name, f.rows)
						}
						m := newPPModel()
						ppApply(t, m, s)
						ppReconcile(t, m, e.in, e.events, e.outcomes)
						if held := m.shares("Schwab A1 ...135"); len(held) != 1 || slices.Collect(maps.Values(held))[0] != 600_000_000 {
							t.Errorf("%s: PP holds %v", name, held)
						}
						continue
					}
					symbols := map[string]bool{}
					for _, a := range e.gf.file.Activities {
						symbols[a.DataSource+" "+a.Symbol] = true
					}
					m := newGFModel()
					if _, err := gfImport(m, s.file); err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if held := m.holdings(); len(symbols) != 1 || len(e.gf.file.AssetProfiles) > 1 || len(held) != 1 || slices.Collect(maps.Values(held))[0].Cmp(big.NewRat(6, 1)) != 0 {
						t.Errorf("%s: Ghostfolio's activities are of %v, %d assets, and it holds %v", name, symbols, len(e.gf.file.AssetProfiles), held)
					}
				}
			}
		}
		// -manual naming the old ticker, which the file's key alone had,
		// makes the API key's rows MANUAL too: an export that writes those
		// alone uses it.
		named := twoKeys{
			api: []string{"JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106", filed: tcFiledEarly, manual: "JKLM",
		}.toolCase()
		inNamed, eventsNamed, _ := mapCase(named, toolGF)
		apiOnly := slices.DeleteFunc(slices.Clone(eventsNamed), func(e toolEvent) bool { return e.security.key != keyTwoAPI })
		if unused, err := checkManual(inNamed, apiOnly); err != nil || len(unused) != 0 || len(apiOnly) != 1 || apiOnly[0].security.manual != "asked" {
			t.Errorf("-manual JKLM of the API's rows alone: unused %q (%v), %d records", unused, err, len(apiOnly))
		}
		// Two companies that had one ticker at different times, of two
		// CUSIPs, are two securities: the one that an account holds keeps
		// the ticker, the other, which only a History file names by it, is
		// known by its CUSIP (I-5). -manual naming the ticker makes two
		// assets of them, not one.
		reused := toolCaseNamed("two_keys")
		reused.in.manual = []string{"JKLM"}
		for i := range reused.in.securities {
			if r := &reused.in.securities[i]; r.key == keyTwoAPI || r.key == keyTwoFile {
				r.cusip = map[string]string{keyTwoAPI: "ZZJ12K106", keyTwoFile: "ZZJ99Z109"}[r.key]
			}
		}
		inReused, eventsReused, _ := mapCase(reused, toolGF)
		reusedProfiles, _ := gfAssets(inReused, eventsReused)
		var names []string
		for _, p := range reusedProfiles {
			names = append(names, p.Name)
		}
		if !slices.Equal(names, []string{"Security (JKLM)", "Security (ZZJ99Z109)"}) {
			t.Errorf("a ticker of two companies: assets %q", names)
		}
		for _, c := range []struct {
			tool   string
			in     *toolInput
			events []toolEvent
		}{{toolGF, in, events}, {toolPP, inPP, eventsPP}} {
			if diffs, _ := holdingsCheck(c.in, c.events); len(diffs) != 0 {
				t.Errorf("%s: differences %+v", c.tool, diffs)
			}
		}
		// Five shares held of the seven written is one difference, of the
		// least key, which names both keys. A row of the symbol without a
		// key is another security, which knows no CUSIP; so does the linked
		// one, whose ticker the row's symbol is: each is written on its own,
		// with a difference of its own, marked unsure.
		short := toolCaseNamed("two_keys")
		short.in.positions[0].long = centsOf(5_000_000)
		short.in.rows = append(short.in.rows, rcFile("Cf7a8b9c0.2", 1, "2026-04-01", "Buy", "buy", keyTwoFile, "JKLM", centsOf(1_000_000), 160, -16_000))
		short.in = tcInput(short.in)
		keyless := toolCaseNamed("two_keys")
		keyless.in.positions = slices.Clone(keyless.in.positions)
		keyless.in.positions[0].long = centsOf(5_000_000)
		keyless.in.rows = append(slices.Clone(keyless.in.rows), rcFile("Cf7a8b9c0.2", 1, "2026-04-01", "Buy", "buy", "", "JKLM", centsOf(1_000_000), 160, -16_000))
		keyless.in = tcInput(keyless.in)
		for _, tool := range []string{toolPP, toolGF} {
			in, events, _ := mapCase(short, tool)
			diffs, _ := holdingsCheck(in, events)
			if len(diffs) != 1 || diffs[0].key != keyTwoAPI || !slices.Equal(diffs[0].keys, []string{keyTwoAPI, keyTwoFile}) ||
				diffs[0].symbol != "JKLM" || diffs[0].snapshotShares != 5_000_000 || diffs[0].writtenShares != 7_000_000 {
				t.Errorf("%s: differences %+v", tool, diffs)
			}
			e := caseExport(t, short, tool)
			if h := e.res.holdings; len(h) != 1 || h[0].SecurityKey != keyTwoAPI || !slices.Equal(h[0].SecurityKeys, []string{keyTwoAPI, keyTwoFile}) || h[0].Unsure {
				t.Errorf("%s: the manifest's differences %+v", tool, h)
			}
			e = caseExport(t, keyless, tool)
			var got []string
			for _, h := range e.res.holdings {
				got = append(got, fmt.Sprintf("%s %s %s %s %v", h.SecurityKey, h.SecurityKeys, h.Symbol, h.Difference, h.Unsure))
			}
			if !slices.Equal(got, []string{" [] JKLM -1 true", "i:1000013 [i:1000013 x:66000017] JKLM -1 true"}) || !slices.Equal(unsureText(e.Unsure), []string{
				"i:1000013: symbol JKLM", "symbol JKLM: i:1000013 schwabtx edit REF -security K:i:1000013",
			}) {
				t.Errorf("%s, a row without a key: differences %q, unsure %q", tool, got, unsureText(e.Unsure))
			}
			chainStepFrom(t, e, t.TempDir())
		}
		// The symbol for Yahoo is one too: a row of either key in the year
		// before the export's last day, or a position of either key, says
		// that it is not idle; else the least key is named, with the day
		// of the last row of either.
		yahoo := toolCaseNamed("two_keys")
		yahoo.in.manual = nil
		in, events, _ = mapCase(yahoo, toolGF)
		if got := yahooIdle(in, events); len(got) != 0 {
			t.Errorf("idle though sold in the year: %+v", got)
		}
		in.rows = in.rows[:1]
		if got := yahooIdle(in, events[:1]); len(got) != 0 {
			t.Errorf("idle though held under the other key: %+v", got)
		}
		// A leg of a trade of several is a row of the symbol too.
		legged := toolCaseNamed("two_keys")
		legged.in.manual, legged.in.positions = nil, nil
		legged.in.rows = append(legged.in.rows[:1], toolRow{
			ref: "T61002802", account: 1, source: "api", time: tcNY("2026-09-01 10:00:00"), day: "2026-09-01", kind: "TRADE", category: "buy",
			amount: centsOf(-16_700), fees: centsOf(0), gross: centsOf(-16_700), flags: []string{"mixed_legs"},
		})
		legged.in.legs = []toolLeg{
			{account: 1, ref: "T61002802", leg: 0, symbol: "JKLM", securityKey: keyTwoAPI, quantity: centsOf(1_000_000), cost: centsOf(-17_000), price: tcPrice(170)},
			{account: 1, ref: "T61002802", leg: 1, symbol: symCall, securityKey: keyCall, quantity: centsOf(-1_000_000), cost: centsOf(300), price: tcPrice(3)},
		}
		in, events, _ = mapCase(legged, toolGF)
		if got := yahooIdle(in, events); len(got) != 0 {
			t.Errorf("idle though bought in a trade of two legs in the year: %+v", got)
		}
		// Idle, the symbol names the least key of its rows, whichever row
		// comes first or last, with the day of the last row of any.
		late := toolCaseNamed("two_keys")
		late.in.manual, late.in.positions, late.in.to = nil, nil, "2027-06-01"
		late.in.rows = append([]toolRow{{
			ref: "Cf7a8b9c0.3", account: 1, source: "file", posted: "2023-06-01", day: "2023-06-01", kind: "Buy", description: "JKLM SMALL CAP VALUE ETF",
			category: "buy", securityKey: keyTwoFile, symbol: "JKLM", quantity: centsOf(1_000_000), price: tcPrice(120), amount: centsOf(-12_000), fees: centsOf(0), gross: centsOf(-12_000),
		}}, late.in.rows...)
		late.in.rows = append(late.in.rows, toolRow{
			ref: "Cf7a8b9c0.4", account: 1, source: "file", posted: "2026-04-01", day: "2026-04-01", kind: "Buy", description: "JKLM SMALL CAP VALUE ETF",
			category: "buy", securityKey: keyTwoFile, symbol: "JKLM", quantity: centsOf(1_000_000), price: tcPrice(160), amount: centsOf(-16_000), fees: centsOf(0), gross: centsOf(-16_000),
		})
		in, events, _ = mapCase(late, toolGF)
		if got := yahooIdle(in, events); fmt.Sprint(got) != fmt.Sprint([]toolIdle{{symbol: "JKLM", key: keyTwoAPI, last: "2026-04-01"}}) {
			t.Errorf("idle %+v", got)
		}
	})

	t.Run("joins", func(t *testing.T) {
		t.Parallel()
		// The keys that the store's data make one security (certainJoins):
		// of one CUSIP, of one option contract or one futures symbol, and
		// those whose rows the store linked as one transaction, when the
		// groups that the links reach know one CUSIP at most; whatever order
		// the links come in, and never by a ticker alone.
		var in toolInput
		add := func(key, assetType, cusip string, symbols ...string) {
			in.securities = append(in.securities, toolSecurityRow{key: key, assetType: assetType, cusip: cusip, symbol: symbols[len(symbols)-1]})
			for i, s := range symbols {
				seen := tcNY("2024-01-02 10:00:00").AddDate(0, i, 0)
				in.names = append(in.names, toolName{key: key, kind: "symbol", value: s, firstSeen: seen, lastSeen: seen})
			}
		}
		link := func(file, api string) toolLink { return toolLink{file: file, api: api} }
		add("i:1000501", "EQUITY", "ZZJ50A101", "JNA")
		add("x:66000501", "", "", "ZZJ50A101")
		add("i:1000502", "OPTION", "", "XYZ   250620C00055000")
		add("s:FILE:XYZ 06/20/2025 55.00 C@2025-07-01", "OPTION", "", "XYZ 06/20/2025 55.00 C")
		add("i:1000503", "OPTION", "", "/ZZM25_C5100:XCME")
		add("s:FILE:/ZZM25_C5100:XCME@2025-07-01", "OPTION", "", "/ZZM25_C5100:XCME")
		add("i:1000504", "FUTURE", "", "/ZZM25:XCME")
		add("s:FILE:/ZZM25:XCME@2025-07-01", "", "", "/ZZM25:XCME")
		add("x:66000505", "", "", "JNE")
		add("i:1000505", "EQUITY", "ZZJ50E103", "JNE")
		add("x:66000506", "", "", "JNF")
		add("i:1000506", "COLLECTIVE_INVESTMENT", "", "JNF")
		// A file's key linked to securities of two CUSIPs, and one named by
		// a CUSIP linked to another's.
		add("x:66000507", "", "", "JNG")
		add("i:1000507", "EQUITY", "ZZJ50G108", "JNG")
		add("i:1000508", "EQUITY", "ZZJ50H106", "JNG")
		add("x:66000509", "", "", "ZZJ50J102")
		add("i:1000509", "EQUITY", "ZZJ50K109", "JNJ")
		// Two files' keys, linked to two keys of one CUSIP.
		add("x:66000510", "", "", "JNL")
		add("s:FILE:JNL@2025-07-01", "", "", "JNL")
		add("i:1000510", "EQUITY", "ZZJ50L107", "JNL")
		add("c:ZZJ50L107", "EQUITY", "ZZJ50L107", "JNL")
		// A ticker that a file's key and the API's have had alike.
		add("x:66000511", "", "", "JNM")
		add("i:1000511", "EQUITY", "ZZJ50M105", "JNM")
		in.links = []toolLink{
			link("x:66000505", "i:1000505"), link("x:66000506", "i:1000506"),
			link("x:66000507", "i:1000507"), link("x:66000507", "i:1000508"), link("x:66000509", "i:1000509"),
			link("x:66000510", "i:1000510"), link("s:FILE:JNL@2025-07-01", "c:ZZJ50L107"),
		}
		want := [][]string{
			{"i:1000501", "x:66000501"},
			{"i:1000502", "s:FILE:XYZ 06/20/2025 55.00 C@2025-07-01"},
			{"i:1000503", "s:FILE:/ZZM25_C5100:XCME@2025-07-01"},
			{"i:1000504", "s:FILE:/ZZM25:XCME@2025-07-01"},
			{"i:1000505", "x:66000505"},
			{"i:1000506", "x:66000506"},
			{"i:1000507"},
			{"i:1000508"},
			{"x:66000507"},
			{"i:1000509"},
			{"x:66000509"},
			{"i:1000510", "c:ZZJ50L107", "x:66000510", "s:FILE:JNL@2025-07-01"},
			{"i:1000511"},
			{"x:66000511"},
		}
		for _, order := range []string{"in order", "reversed"} {
			c := in
			if order == "reversed" {
				c.links = slices.Clone(in.links)
				slices.Reverse(c.links)
			}
			ix := newToolIndex(&c)
			var all []*toolSecurity
			for _, r := range c.securities {
				all = append(all, ix.base(r.key, ""))
			}
			g, _ := certainJoins(all, c.links)
			groups := map[int][]string{}
			for i, s := range all {
				groups[g.find(i)] = append(groups[g.find(i)], s.key)
			}
			got := slices.Collect(maps.Values(groups))
			for _, k := range got {
				slices.Sort(k)
			}
			for _, w := range want {
				if !slices.ContainsFunc(got, func(k []string) bool { return slices.Equal(k, slices.Sorted(slices.Values(w))) }) {
					t.Errorf("links %s: no group %q, groups %q", order, w, got)
				}
			}
			if len(got) != len(want) {
				t.Errorf("links %s: %d groups %q, want %d", order, len(got), got, len(want))
			}
		}

		// Through groupAll, which joins by nothing else: a file's key whose
		// ticker securities of two CUSIPs have had is the one that a link
		// names, and is not listed; one linked to both is joined to neither
		// and listed with both, whichever order the links come in, and so is
		// one linked to two of other tickers than its own, as when you link
		// rows by hand; a link keeps a file's key whose other ticker is a
		// second CUSIP's; and a key that only shares a ticker with another,
		// of no link, is listed with it (I-4).
		in = toolInput{}
		add("i:1000521", "EQUITY", "ZZJ52A109", "JNP")
		add("i:1000522", "EQUITY", "ZZJ52B107", "JNP")
		add("x:66000521", "", "", "JNP")
		add("i:1000531", "EQUITY", "ZZJ53A108", "JNR")
		add("i:1000532", "EQUITY", "ZZJ53B106", "JNS")
		add("x:66000531", "", "", "JNR", "JNS")
		add("i:1000541", "EQUITY", "ZZJ54A107", "JNT")
		add("i:1000542", "EQUITY", "ZZJ54B105", "JNU")
		add("x:66000541", "", "", "JNV")
		tickers := in
		grouped := func(links ...toolLink) *toolIndex {
			c := tickers
			c.links = links
			ix := newToolIndex(&c)
			ix.groupAll()
			return ix
		}
		one := func(ix *toolIndex, a, b string) bool { return ix.security(a, "").rep == ix.security(b, "").rep }
		// unsure returns the keys that the group of key may be, nil when it
		// is not unsure.
		unsure := func(ix *toolIndex, key string) []string {
			u := ix.unsure[secRef{key: key}]
			if u == nil {
				return nil
			}
			of := []string{}
			for _, o := range u.of {
				of = append(of, o[0].key)
			}
			return of
		}
		ix := grouped(link("x:66000521", "i:1000522"), link("x:66000531", "i:1000531"))
		if !one(ix, "x:66000521", "i:1000522") || one(ix, "x:66000521", "i:1000521") || unsure(ix, "x:66000521") != nil {
			t.Errorf("a link to the second of two CUSIPs of its ticker: one with it %v, unsure %q", one(ix, "x:66000521", "i:1000522"), unsure(ix, "x:66000521"))
		}
		if !one(ix, "x:66000531", "i:1000531") || one(ix, "x:66000531", "i:1000532") || unsure(ix, "x:66000531") != nil {
			t.Errorf("a link beside a ticker of another CUSIP: one with it %v, unsure %q", one(ix, "x:66000531", "i:1000531"), unsure(ix, "x:66000531"))
		}
		if unsure(ix, "x:66000541") != nil {
			t.Errorf("a key of a ticker of its own: unsure %q", unsure(ix, "x:66000541"))
		}
		for _, links := range [][]toolLink{
			{link("x:66000521", "i:1000521"), link("x:66000521", "i:1000522"), link("x:66000541", "i:1000541"), link("x:66000541", "i:1000542")},
			{link("x:66000541", "i:1000542"), link("x:66000541", "i:1000541"), link("x:66000521", "i:1000522"), link("x:66000521", "i:1000521")},
		} {
			ix := grouped(links...)
			if one(ix, "x:66000521", "i:1000522") || one(ix, "x:66000521", "i:1000521") || !slices.Equal(unsure(ix, "x:66000521"), []string{"i:1000521", "i:1000522"}) ||
				one(ix, "x:66000541", "i:1000541") || !slices.Equal(unsure(ix, "x:66000541"), []string{"i:1000541", "i:1000542"}) {
				t.Errorf("links %+v to two CUSIPs: unsure %q and %q", links, unsure(ix, "x:66000521"), unsure(ix, "x:66000541"))
			}
		}
		ix = grouped()
		if !slices.Equal(unsure(ix, "x:66000521"), []string{"i:1000521", "i:1000522"}) || !slices.Equal(unsure(ix, "x:66000531"), []string{"i:1000531", "i:1000532"}) ||
			unsure(ix, "x:66000541") != nil || unsure(ix, "i:1000521") != nil {
			t.Errorf("no link: unsure %q, %q, %q", unsure(ix, "x:66000521"), unsure(ix, "x:66000531"), unsure(ix, "x:66000541"))
		}
	})

	t.Run("groups", func(t *testing.T) {
		t.Parallel()
		// The store's securities that the tools take for one: by a CUSIP,
		// and options by their OCC form or their futures symbol; never by a
		// ticker alone (I-1). A key that knows no CUSIP and has had a ticker
		// that another has had too is written on its own, its identity its
		// key, and listed with each that had it (I-4); of securities of two
		// CUSIPs whose current symbol is one ticker, the one that Schwab's
		// API saw under it last keeps it, and neither when the API saw both
		// under it at one instant (I-5); and one that a key on its own had
		// too is still the other's.
		in := tcInput(toolInput{})
		add := func(key, assetType, cusip string, symbols ...string) {
			in.securities = append(in.securities, toolSecurityRow{key: key, assetType: assetType, cusip: cusip, symbol: symbols[len(symbols)-1]})
			for i, s := range symbols {
				seen := tcNY("2024-01-02 10:00:00").AddDate(0, i, 0)
				in.names = append(in.names, toolName{key: key, kind: "symbol", value: s, firstSeen: seen, lastSeen: seen})
			}
		}
		add("x:66000101", "", "", "GRPA")
		add("i:1000101", "EQUITY", "ZZG10A103", "GRPA")
		add("i:1000102", "EQUITY", "ZZG10B101", "GRPB")
		add("x:66000102", "", "", "GRPB", "GRPC")
		add("i:1000103", "EQUITY", "ZZG10C109", "GRPC")
		add("i:1000104", "FIXED_INCOME", "", "ZZBB")
		add("x:66000104", "", "", "ZZBB")
		add("x:66000105", "", "", "GRPD")
		add("i:1000105", "EQUITY", "", "GRPD", "GRPE")
		add("i:1000106", "EQUITY", "ZZG10E105", "GRPE")
		add("i:1000107", "EQUITY", "ZZG10F102", "GRPE")
		add("i:1000108", "EQUITY", "ZZG10H108", "GRPG", "GRPH")
		add("i:1000109", "EQUITY", "ZZG10J104", "GRPH")
		add("x:66000106", "", "", "GRPH")
		add("i:1000110", "EQUITY", "ZZG10K101", "GRPK")
		add("i:1000111", "EQUITY", "ZZG10L109", "GRPL")
		add("x:66000107", "", "", "GRPK", "GRPL")
		add("x:66000108", "", "", "GRPK")
		add("s:FILE:XYZ 06/20/2025 55.00 C@2025-07-01", "OPTION", "", "XYZ 06/20/2025 55.00 C")
		add("s:FILE:"+symFutOpt+"@2025-04-01", "OPTION", "", symFutOpt)
		for _, c := range []struct {
			name       string
			keys       []string
			one        bool
			identities []string
			bonds      []bool
		}{
			// The file's key is on its own beside the API's, and GRPA is the
			// API's identity.
			{"a ticker, a CUSIP under one key", []string{"x:66000101", "i:1000101"}, false, []string{"x:66000101", "GRPA"}, []bool{false, false}},
			{"a file's key between two CUSIPs", []string{"i:1000102", "x:66000102", "i:1000103"}, false, []string{"GRPB", "x:66000102", "GRPC"}, []bool{false, false, false}},
			// A bond with a ticker under the API's key, beside the file's
			// key of that ticker, of no asset type: two, neither of a CUSIP.
			{"a bond with a ticker", []string{"i:1000104", "x:66000104"}, false, []string{"i:1000104", "x:66000104"}, []bool{true, false}},
			// GRPE, which the API named two CUSIPs by at one instant, names
			// neither of them; GRPD's two keys, which know no CUSIP, are each
			// on their own.
			{"a key whose ticker two CUSIPs had", []string{"i:1000105", "x:66000105", "i:1000106", "i:1000107"}, false, []string{"i:1000105", "x:66000105", "ZZG10E105", "ZZG10F102"}, []bool{false, false, false, false}},
			// GRPH, which the API named i:1000108 by a month after it named
			// i:1000109 so, is i:1000108's.
			{"a file's key of a ticker that two CUSIPs had", []string{"x:66000106", "i:1000109", "i:1000108"}, false, []string{"x:66000106", "ZZG10J104", "GRPH"}, []bool{false, false, false}},
			{"a file's key beside one between two CUSIPs", []string{"x:66000108", "i:1000110", "x:66000107", "i:1000111"}, false, []string{"x:66000108", "GRPK", "x:66000107", "GRPL"}, []bool{false, false, false, false}},
			// A file's option and the API's of one contract; and a futures
			// option, which has no OCC form, by its symbol, named by the
			// API's key.
			{"an option of two keys", []string{keyOptCU, "s:FILE:XYZ 06/20/2025 55.00 C@2025-07-01"}, true, []string{"ZZO55C106", "ZZO55C106"}, []bool{false, false}},
			{"a futures option of two keys", []string{keyFutOpt, "s:FILE:" + symFutOpt + "@2025-04-01"}, true, []string{keyFutOpt, keyFutOpt}, []bool{false, false}},
		} {
			ix := newToolIndex(&in)
			var secs []*toolSecurity
			var identities []string
			for i, k := range c.keys {
				s := ix.security(k, "")
				secs = append(secs, s)
				identities = append(identities, s.identity())
				if s.bond != c.bonds[i] {
					t.Errorf("%s: %s is a bond: %v", c.name, k, s.bond)
				}
			}
			same := true
			for _, s := range secs[1:] {
				t1, w1, n1 := ppIdentifiers(secs[0])
				t2, w2, n2 := ppIdentifiers(s)
				same = same && s.rep == secs[0].rep && s.gfSymbol() == secs[0].gfSymbol() && t1 == t2 && w1 == w2 && n1 == n2
			}
			if same != c.one || !slices.Equal(identities, c.identities) {
				t.Errorf("%s: one %v, identities %q, want %v, %q", c.name, same, identities, c.one, c.identities)
			}
		}
		// The keys written on their own, each with those that it may be, in
		// the order of their keys; the others are not.
		ix := newToolIndex(&in)
		ix.groupAll()
		for _, k := range []string{
			"x:66000101", "x:66000102", "i:1000104", "x:66000104", "i:1000105", "x:66000105", "x:66000106", "x:66000107", "x:66000108",
			"i:1000101", "i:1000102", "i:1000103", "i:1000106", "i:1000107", "i:1000108", "i:1000109", "i:1000110", "i:1000111", keyOptCU, keyFutOpt,
		} {
			want := map[string]string{
				"x:66000101": "i:1000101", "x:66000102": "i:1000102 i:1000103", "i:1000104": "x:66000104", "x:66000104": "i:1000104",
				"i:1000105": "i:1000106 i:1000107 x:66000105", "x:66000105": "i:1000105", "x:66000106": "i:1000108 i:1000109",
				"x:66000107": "i:1000110 i:1000111 x:66000108", "x:66000108": "i:1000110 x:66000107",
			}[k]
			var got []string
			if u := ix.unsure[secRef{key: k}]; u != nil {
				for _, o := range u.of {
					got = append(got, o[0].key)
				}
			}
			if strings.Join(got, " ") != want {
				t.Errorf("%s: unsure of %q, want %q", k, got, want)
			}
		}
		// One contract's keys give it one multiplier: a row's contract
		// without a key takes the API key's 100, not one assumed; two keys
		// of one contract that give two leave its rows out.
		twins := tcInput(toolInput{rows: []toolRow{rcTrade("T7f1a2b302", 1, "2025-01-06 10:00:00", "buy", "", symPut, 1_000_000, 2, -200, 0)}})
		if s := toolSecurityOf(&twins, "", symPut); s.multiplier != 100 || s.assumed || s.badMultiplier {
			t.Errorf("a row's contract of a key's: multiplier %d, assumed %v, bad %v", s.multiplier, s.assumed, s.badMultiplier)
		}
		twins.securities = append(slices.Clone(twins.securities), toolSecurityRow{key: "i:219771035", assetType: "OPTION", symbol: symPut, multiplier: tcPrice(10)})
		twins.names = append(slices.Clone(twins.names), toolName{key: "i:219771035", kind: "symbol", value: symPut, firstSeen: tcNY("2025-01-06 10:00:00"), lastSeen: tcNY("2025-01-06 10:00:00")})
		for _, k := range []secRef{{key: keyPut}, {key: "i:219771035"}, {symbol: symPut}} {
			if s := toolSecurityOf(&twins, k.key, k.symbol); !s.badMultiplier {
				t.Errorf("%v of a contract with two multipliers: %d, assumed %v", k, s.multiplier, s.assumed)
			}
		}
		// The keys of one contract count it in one unit: a futures option
		// bought under a History file's key, which a file types as an
		// option only when its symbol parses as a contract, as a futures
		// option's does not, and sold under the API's key, which knows its
		// multiplier, 50: one contract in and one out, of 50 shares each,
		// with no multiplier assumed, nets 0 in both tools.
		for _, typed := range []string{"OPTION", ""} {
			fileKey := "s:FILE:" + symFutOpt + "@2025-01-20"
			securities, names := tcSecurities()
			futOpt := toolCase{name: "a futures option of two keys", in: tcInput(toolInput{
				to:         "2025-03-01",
				securities: append(securities, toolSecurityRow{key: fileKey, assetType: typed, symbol: symFutOpt}),
				names:      append(names, toolName{key: fileKey, kind: "symbol", value: symFutOpt, firstSeen: tcNY("2025-01-20 09:00:00"), lastSeen: tcNY("2025-01-20 09:00:00")}),
				rows: []toolRow{
					rcFile("C7f1a2b3c.1", 1, "2025-01-17", "Buy to Open", "buy", fileKey, symFutOpt, centsOf(1_000_000), 21, -105_000),
					rcTrade("T7f1a2b301", 1, "2025-02-10 10:00:00", "sell", keyFutOpt, symFutOpt, -1_000_000, 25, 125_000, 0),
				},
			})}
			for _, tool := range []string{toolPP, toolGF} {
				_, events, _ := mapCase(futOpt, tool)
				for _, e := range events {
					if e.shares != 50_000_000 || len(e.caveats) > 0 {
						t.Errorf("typed %q, %s: %s %s shares %s %v", typed, tool, e.ref, e.kind, formatMicros(e.shares), e.caveats)
					}
				}
				s := chainStepFrom(t, caseExport(t, futOpt, tool), t.TempDir())
				if tool == toolPP {
					m := newPPModel()
					ppApply(t, m, s)
					if held := m.shares("Schwab A1 ...135"); len(held) != 1 || slices.Collect(maps.Values(held))[0] != 0 {
						t.Errorf("typed %q: PP holds %v", typed, held)
					}
					continue
				}
				m := newGFModel()
				if _, err := gfImport(m, s.file); err != nil {
					t.Fatal(err)
				}
				if held := m.holdings(); len(held) != 0 || len(events) != 2 {
					t.Errorf("typed %q: Ghostfolio holds %v of %d activities", typed, held, len(events))
				}
			}
		}
	})

	t.Run("the safe form", func(t *testing.T) {
		t.Parallel()
		// Companies of one ticker: one gave it up, delisted (B1, B8, B4 with
		// a History file's key of each) or renamed (B3, B9), merged away
		// (B7, J2), or took a new one (J1, J3), before another had it; one
		// holds it still while another had it before (B6, F3); one of no
		// CUSIP had it beside another of none (B5); and a security whose
		// History file names its last rows by its CUSIP (B2, S1, S2). Each
		// key that knows no CUSIP and shares a ticker with another security
		// is written on its own (I-4, I-6), whatever the ticker's history:
		// in PP with no ticker and no WKN, named by its key, its
		// descriptions as its note; in Ghostfolio MANUAL, why unsure, by that
		// name. Each that the export writes is listed with each that it may
		// be and the command that says so; its differences are marked unsure,
		// and no other's; and the tools hold the securities apart.
		apartIn := func(securities []toolSecurityRow, names []toolName, rows []toolRow, positions []toolPosition) toolInput {
			base, baseNames := tcSecurities()
			in := tcInput(toolInput{securities: append(base, securities...), names: append(baseNames, names...), rows: rows, positions: positions})
			if len(positions) > 0 {
				in.snapshots = []toolSnapshot{rcSnapshot(1, string(positions[0].marketDate), 100_000)}
			}
			return in
		}
		seen := func(key, kind, value, clock string) toolName {
			return toolName{key: key, kind: kind, value: value, firstSeen: tcNY(clock), lastSeen: tcNY(clock)}
		}
		b1 := apartIn([]toolSecurityRow{
			{key: "i:1000301", assetType: "EQUITY", symbol: "ZZM40A100"}, {key: "x:66000302", symbol: "TKRZ"},
		}, []toolName{
			seen("i:1000301", "symbol", "TKRZ", "2021-01-04 18:30:00"), seen("i:1000301", "symbol", "ZZM40A100", "2022-06-15 18:30:00"),
			seen("i:1000301", "description", "TKRZ OLD CORP", "2021-01-04 18:30:00"),
			seen("x:66000302", "symbol", "TKRZ", "2026-02-10 09:00:00"), seen("x:66000302", "description", "TKRZ NEW HOLDINGS", "2026-02-10 09:00:00"),
		}, []toolRow{
			rcTrade("T7f4a000101", 1, "2021-01-04 10:00:00", "buy", "i:1000301", "TKRZ", 10_000_000, 20, -20_000, 0),
			rcTrade("T7f4a000102", 1, "2022-06-15 10:00:00", "sell", "i:1000301", "ZZM40A100", -10_000_000, 1, 1_000, 0),
			rcFile("C7f4a5b6c.1", 3, "2025-04-01", "Buy", "buy", "x:66000302", "TKRZ", centsOf(5_000_000), 30, -15_000),
		}, nil)
		b2 := apartIn([]toolSecurityRow{
			{key: "x:66000303", symbol: "ZZN40B107"}, {key: "i:1000303", assetType: "EQUITY", symbol: "TKRY"},
		}, []toolName{
			seen("x:66000303", "symbol", "TKRY", "2023-01-15 09:00:00"), seen("x:66000303", "symbol", "ZZN40B107", "2026-02-10 09:00:00"),
			seen("x:66000303", "description", "TKRY OLD CORP", "2023-01-15 09:00:00"),
			seen("i:1000303", "symbol", "TKRY", "2025-09-02 18:30:00"), seen("i:1000303", "description", "TKRY NEW INC", "2025-09-02 18:30:00"),
		}, []toolRow{
			rcFile("C7f4a5b6d.1", 1, "2019-05-01", "Buy", "buy", "x:66000303", "ZZN40B107", centsOf(5_000_000), 10, -5_000),
			rcFile("C7f4a5b6d.2", 1, "2022-05-02", "Sell", "sell", "x:66000303", "ZZN40B107", centsOf(-5_000_000), 12, 6_000),
			rcTrade("T7f4a000202", 1, "2025-03-03 10:00:00", "buy", "i:1000303", "TKRY", 3_000_000, 40, -12_000, 0),
		}, []toolPosition{rcPosition("2026-11-10", "i:1000303", 3_000_000, 150)})
		// S1: a History file's key follows a spin-off from its first row,
		// under its CUSIP (2015), through a purchase under its ticker (2016)
		// to its cash merger under the CUSIP again (2025), and names them at
		// its export (2026); the API's key, which knows no CUSIP, bought the
		// ticker in 2024.
		mergedCash := rcFile("C7f4a5b74.3", 3, "2025-12-01", "Cash Merger", "sell", "x:66000510", "69389C109", sql.NullInt64{}, 0, 75_000)
		mergedShares := rcFile("C7f4a5b74.4", 3, "2025-12-01", "Cash Merger Adj", "other", "x:66000510", "69389C109", centsOf(-15_000_000), 0, 0)
		mergedCash.flags, mergedShares.flags = []string{"two_row_event"}, []string{"two_row_event"}
		s1 := apartIn([]toolSecurityRow{
			{key: "x:66000510", symbol: "69389C109"}, {key: "i:1000510", assetType: "EQUITY", symbol: "TKSP"},
		}, []toolName{
			seen("x:66000510", "symbol", "TKSP", "2026-01-15 09:00:00"), seen("x:66000510", "symbol", "69389C109", "2026-01-15 09:00:00"),
			seen("x:66000510", "description", "TKSP SPINCO INC", "2026-01-15 09:00:00"),
			seen("i:1000510", "symbol", "TKSP", "2024-06-03 18:30:00"), seen("i:1000510", "description", "TKSP SPINCO INC", "2024-06-03 18:30:00"),
		}, []toolRow{
			rcFile("C7f4a5b74.1", 3, "2015-03-02", "Spin-off", "transfer_in", "x:66000510", "69389C109", centsOf(10_000_000), 0, 0),
			rcFile("C7f4a5b74.2", 3, "2016-05-02", "Buy", "buy", "x:66000510", "TKSP", centsOf(5_000_000), 20, -10_000),
			rcTrade("T7f4a000510", 1, "2024-06-03 10:00:00", "buy", "i:1000510", "TKSP", 2_000_000, 30, -6_000, 0),
			mergedCash, mergedShares,
		}, nil)
		// S2: the same of an API key, and a History file's key that bought
		// the ticker in 2024, before the merger.
		s2 := apartIn([]toolSecurityRow{
			{key: "i:1000520", assetType: "EQUITY", symbol: "69389C109"}, {key: "x:66000520", symbol: "TKSP"},
		}, []toolName{
			{key: "i:1000520", kind: "symbol", value: "69389C109", firstSeen: tcNY("2015-03-02 18:30:00"), lastSeen: tcNY("2025-12-01 18:30:00")},
			{key: "i:1000520", kind: "symbol", value: "TKSP", firstSeen: tcNY("2016-05-02 18:30:00"), lastSeen: tcNY("2016-05-02 18:30:00")},
			seen("x:66000520", "symbol", "TKSP", "2024-07-01 09:00:00"),
		}, []toolRow{
			rcTrade("T7f4a000521", 1, "2015-03-02 10:00:00", "buy", "i:1000520", "69389C109", 10_000_000, 1, -1_000, 0),
			rcTrade("T7f4a000522", 1, "2016-05-02 10:00:00", "buy", "i:1000520", "TKSP", 5_000_000, 20, -10_000, 0),
			rcTrade("T7f4a000523", 1, "2025-12-01 10:00:00", "sell", "i:1000520", "69389C109", -15_000_000, 50, 75_000, 0),
			rcFile("C7f4a5b75.1", 3, "2024-06-03", "Buy", "buy", "x:66000520", "TKSP", centsOf(2_000_000), 30, -6_000),
		}, nil)
		// B7: a History file whose one row of the security is its cash
		// merger, under its CUSIP, after the positions' key last saw the
		// ticker; and an API key that bought the ticker after the merger,
		// before the file's export.
		mergerCash := rcFile("C7f4a5b76.1", 1, "2025-12-01", "Cash Merger", "sell", "x:66000309", "69389C109", sql.NullInt64{}, 0, 50_000)
		mergerShares := rcFile("C7f4a5b76.2", 1, "2025-12-01", "Cash Merger Adj", "other", "x:66000309", "69389C109", centsOf(-10_000_000), 0, 0)
		mergerCash.flags, mergerShares.flags = []string{"two_row_event"}, []string{"two_row_event"}
		b7 := apartIn([]toolSecurityRow{
			{key: "x:66000309", symbol: "69389C109"},
			{key: "c:69389C109", assetType: "EQUITY", symbol: "MRGR", cusip: "69389C109"},
			{key: "i:1000309", assetType: "EQUITY", symbol: "MRGR"},
		}, []toolName{
			seen("x:66000309", "symbol", "69389C109", "2026-01-15 09:00:00"),
			{key: "c:69389C109", kind: "symbol", value: "MRGR", firstSeen: tcNY("2025-06-30 18:05:00"), lastSeen: tcNY("2025-11-28 18:05:00")},
			seen("i:1000309", "symbol", "MRGR", "2026-01-05 18:30:00"),
		}, []toolRow{
			mergerCash, mergerShares,
			rcTrade("T7f4a000701", 1, "2026-01-05 10:00:00", "buy", "i:1000309", "MRGR", 2_000_000, 25, -5_000, 0),
		}, nil)
		// B8: B1 with the old company's rows read by one -json import long
		// after, which dates both of its symbols at once.
		b8 := b1
		b8.names = slices.Clone(b1.names)
		for i := range b8.names {
			if n := &b8.names[i]; n.key == "i:1000301" {
				n.firstSeen, n.lastSeen = tcNY("2026-11-12 10:04:05"), tcNY("2026-11-12 10:04:05")
			}
		}
		b2Seen := b2
		b2Seen.names = slices.Clone(b2.names)
		for i := range b2Seen.names {
			if n := &b2Seen.names[i]; n.key == "i:1000303" {
				n.lastSeen = tcNY("2026-11-10 18:05:00")
			}
		}
		b3 := apartIn([]toolSecurityRow{
			{key: "i:1000304", assetType: "EQUITY", symbol: "TKRV", cusip: "ZZP40C103"}, {key: "x:66000304", symbol: "TKRW"},
		}, []toolName{
			seen("i:1000304", "symbol", "TKRW", "2021-01-04 18:30:00"), seen("i:1000304", "symbol", "TKRV", "2024-02-01 18:30:00"),
			seen("i:1000304", "description", "TKRW OLD CORP", "2021-01-04 18:30:00"),
			seen("x:66000304", "symbol", "TKRW", "2026-02-10 09:00:00"), seen("x:66000304", "description", "TKRW NEW INC", "2026-02-10 09:00:00"),
		}, []toolRow{
			rcTrade("T7f4a000301", 1, "2021-01-04 10:00:00", "buy", "i:1000304", "TKRW", 10_000_000, 20, -20_000, 0),
			rcTrade("T7f4a000302", 1, "2024-02-01 10:00:00", "sell", "i:1000304", "TKRV", -10_000_000, 25, 25_000, 0),
			rcFile("C7f4a5b6e.1", 3, "2025-06-02", "Buy", "buy", "x:66000304", "TKRW", centsOf(5_000_000), 30, -15_000),
		}, nil)
		// B9: B3 with the History file exported on the morning of the day
		// of the old company's rename, its buy the day before.
		b9 := b3
		b9.names = slices.Clone(b3.names)
		for i := range b9.names {
			if n := &b9.names[i]; n.key == "x:66000304" {
				n.firstSeen, n.lastSeen = tcNY("2024-02-01 09:00:00"), tcNY("2024-02-01 09:00:00")
			}
		}
		b9.rows = slices.Clone(b3.rows)
		b9.rows[2] = rcFile("C7f4a5b6e.1", 3, "2024-01-31", "Buy", "buy", "x:66000304", "TKRW", centsOf(5_000_000), 30, -15_000)
		b9 = tcInput(b9)
		b5 := apartIn([]toolSecurityRow{{key: "x:66000306", symbol: "TKRT"}, {key: "x:66000307", symbol: "TKRU"}}, []toolName{
			seen("x:66000306", "symbol", "TKRU", "2022-01-15 09:00:00"), seen("x:66000306", "symbol", "TKRT", "2026-02-10 09:00:00"),
			seen("x:66000307", "symbol", "TKRU", "2026-02-10 09:00:00"),
		}, []toolRow{
			rcFile("C7f4a5b71.1", 1, "2021-06-01", "Buy", "buy", "x:66000306", "TKRU", centsOf(4_000_000), 10, -4_000),
			rcFile("C7f4a5b71.2", 1, "2023-03-01", "Sell", "sell", "x:66000306", "TKRT", centsOf(-4_000_000), 11, 4_400),
			rcFile("C7f4a5b72.1", 3, "2025-04-01", "Buy", "buy", "x:66000307", "TKRU", centsOf(2_000_000), 30, -6_000),
		}, nil)
		// B6: a file key of a ticker that an old company gave up and that a
		// company of another CUSIP holds now.
		b6 := apartIn([]toolSecurityRow{
			{key: "i:1000306", assetType: "EQUITY", symbol: "ZZT40D107"},
			{key: "c:ZZU40E103", assetType: "EQUITY", symbol: "TKRX", cusip: "ZZU40E103"},
			{key: "x:66000308", symbol: "TKRX"},
		}, []toolName{
			seen("i:1000306", "symbol", "TKRX", "2021-01-04 18:30:00"), seen("i:1000306", "symbol", "ZZT40D107", "2022-06-15 18:30:00"),
			{key: "c:ZZU40E103", kind: "symbol", value: "TKRX", firstSeen: tcNY("2025-06-02 18:05:00"), lastSeen: tcNY("2026-11-10 18:05:00")},
			seen("x:66000308", "symbol", "TKRX", "2026-02-10 09:00:00"),
		}, []toolRow{
			rcTrade("T7f4a000601", 1, "2021-01-04 10:00:00", "buy", "i:1000306", "TKRX", 1_000_000, 20, -2_000, 0),
			rcTrade("T7f4a000602", 1, "2022-06-15 10:00:00", "sell", "i:1000306", "ZZT40D107", -1_000_000, 1, 100, 0),
			rcFile("C7f4a5b73.1", 1, "2025-07-01", "Buy", "buy", "x:66000308", "TKRX", centsOf(2_000_000), 40, -8_000),
		}, []toolPosition{rcPosition("2026-11-10", "c:ZZU40E103", 2_000_000, 82)})
		b4 := b1
		b4.securities = append(slices.Clone(b1.securities),
			toolSecurityRow{key: "x:66000300", symbol: "TKRZ"}, toolSecurityRow{key: "i:1000302", assetType: "EQUITY", symbol: "TKRZ"})
		b4.names = append(slices.Clone(b1.names),
			seen("x:66000300", "symbol", "TKRZ", "2021-02-01 09:00:00"), seen("i:1000302", "symbol", "TKRZ", "2026-02-15 18:30:00"))
		b4.rows = append(slices.Clone(b1.rows),
			rcFile("C7f4a5b6f.1", 1, "2020-06-01", "Buy", "buy", "x:66000300", "TKRZ", centsOf(2_000_000), 15, -3_000),
			rcTrade("T7f4a000401", 2, "2026-02-15 10:00:00", "buy", "i:1000302", "TKRZ", 1_000_000, 31, -3_100, 0))
		b4 = tcInput(b4)
		// J1: S's History file's key and its API key the store joined itself,
		// their names meeting within days, so that S's canonical key is the
		// API's and its entries keep it whatever their source: the file,
		// exported in January 2026, names S by TKNB, the ticker that it took
		// at its rename in June 2025, its buy of 2015 too; the API's rows
		// name it TKNA up to May 2025 and TKNB from June. Q, which knows no
		// CUSIP, took TKNA in October 2025, and no snapshot held it.
		j1 := apartIn([]toolSecurityRow{
			{key: "i:1000800", assetType: "EQUITY", symbol: "TKNB", cusip: "ZZQ80A107"}, {key: "i:1000801", assetType: "EQUITY", symbol: "TKNA"},
		}, []toolName{
			{key: "i:1000800", kind: "symbol", value: "TKNB", from: "x:66000800", firstSeen: tcNY("2026-01-04 09:00:00"), lastSeen: tcNY("2026-01-04 09:00:00")},
			seen("i:1000800", "symbol", "TKNA", "2026-01-05 18:30:00"),
			{key: "i:1000800", kind: "symbol", value: "TKNB", firstSeen: tcNY("2026-01-05 18:30:00"), lastSeen: tcNY("2026-03-02 18:05:00")},
			seen("i:1000800", "description", "TKNB HOLDINGS INC", "2026-01-04 09:00:00"),
			seen("i:1000801", "symbol", "TKNA", "2026-01-05 18:30:00"), seen("i:1000801", "description", "TKNA NEW CORP", "2026-01-05 18:30:00"),
		}, []toolRow{
			rcFile("C7f8a0001.1", 1, "2015-03-02", "Buy", "buy", "i:1000800", "TKNB", centsOf(10_000_000), 10, -10_000),
			rcTrade("T7f8a000001", 1, "2025-10-01 10:00:00", "buy", "i:1000801", "TKNA", 2_000_000, 40, -8_000, 0),
			rcTrade("T7f8a000002", 1, "2025-11-03 10:00:00", "sell", "i:1000801", "TKNA", -2_000_000, 41, 8_200, 0),
		}, []toolPosition{rcPosition("2026-03-02", "i:1000800", 10_000_000, 300)})
		// S's rows of the API are of another account's.
		j1.days = append(j1.days,
			toolSymbolDays{key: "i:1000800", symbol: "TKNA", source: "api", first: "2024-03-01", last: "2025-05-15"},
			toolSymbolDays{key: "i:1000800", symbol: "TKNB", source: "api", first: "2025-06-16", last: "2026-02-17"})
		// J3: J1 with R, which had TKNB from 2010 until it was renamed TKNR in
		// 2020, before S took TKNB, and has no row.
		j3 := j1
		j3.securities = append(slices.Clone(j1.securities), toolSecurityRow{key: "i:1000802", assetType: "EQUITY", symbol: "TKNR"})
		j3.names = append(slices.Clone(j1.names), seen("i:1000802", "symbol", "TKNB", "2020-01-15 18:30:00"), seen("i:1000802", "symbol", "TKNR", "2020-03-02 18:30:00"))
		j3.days = append(slices.Clone(j1.days),
			toolSymbolDays{key: "i:1000802", symbol: "TKNB", source: "api", first: "2010-06-01", last: "2020-01-15"},
			toolSymbolDays{key: "i:1000802", symbol: "TKNR", source: "api", first: "2020-03-02", last: "2020-03-02"})
		// J2: B7 with the History file's key joined by the store to the
		// positions' key, the file's rows and its name under the CUSIP now of
		// c:69389C109.
		joinedCash, joinedShares := mergerCash, mergerShares
		joinedCash.securityKey, joinedShares.securityKey = "c:69389C109", "c:69389C109"
		j2 := apartIn([]toolSecurityRow{
			{key: "c:69389C109", assetType: "EQUITY", symbol: "69389C109", cusip: "69389C109"},
			{key: "i:1000309", assetType: "EQUITY", symbol: "MRGR"},
		}, []toolName{
			{key: "c:69389C109", kind: "symbol", value: "69389C109", from: "x:66000309", firstSeen: tcNY("2026-01-15 09:00:00"), lastSeen: tcNY("2026-01-15 09:00:00")},
			{key: "c:69389C109", kind: "symbol", value: "MRGR", firstSeen: tcNY("2025-06-30 18:05:00"), lastSeen: tcNY("2025-11-28 18:05:00")},
			seen("i:1000309", "symbol", "MRGR", "2026-01-05 18:30:00"),
		}, []toolRow{
			joinedCash, joinedShares,
			rcTrade("T7f4a000701", 1, "2026-01-05 10:00:00", "buy", "i:1000309", "MRGR", 2_000_000, 25, -5_000, 0),
		}, nil)
		f3 := apartIn([]toolSecurityRow{
			{key: "i:1000501", assetType: "EQUITY", symbol: "GRPZ", cusip: "ZZR50A102"},
			{key: "c:ZZS50B109", assetType: "EQUITY", symbol: "GRPZ", cusip: "ZZS50B109"},
			{key: "x:66000501", symbol: "GRPZ"},
		}, []toolName{
			seen("i:1000501", "symbol", "GRPZ", "2026-01-20 10:00:00"),
			{key: "c:ZZS50B109", kind: "symbol", value: "GRPZ", firstSeen: tcNY("2025-06-02 18:05:00"), lastSeen: tcNY("2026-11-10 18:05:00")},
			seen("x:66000501", "symbol", "GRPZ", "2026-02-10 09:00:00"),
		}, []toolRow{
			rcFile("C7f4a5b70.1", 1, "2019-05-01", "Buy", "buy", "x:66000501", "GRPZ", centsOf(5_000_000), 10, -5_000),
			rcTrade("T7f4a000501", 1, "2021-03-01 10:00:00", "sell", "i:1000501", "GRPZ", -5_000_000, 12, 6_000, 0),
		}, []toolPosition{rcPosition("2026-11-10", "c:ZZS50B109", 2_000_000, 80)})
		for _, c := range []struct {
			name string
			in   toolInput
			// unsure are the keys written on their own, each to those that it
			// may be, in their order.
			unsure map[string]string
		}{
			{"B1, delisted", b1, map[string]string{"x:66000302": "i:1000301"}},
			{"B2, delisted by a later file", b2, map[string]string{"i:1000303": "x:66000303"}},
			{"B2, the API's key seen since", b2Seen, map[string]string{"i:1000303": "x:66000303"}},
			{"B3, renamed", b3, map[string]string{"x:66000304": "i:1000304"}},
			{"B4, a file key of each", b4, map[string]string{
				"i:1000302": "i:1000301 x:66000300 x:66000302", "x:66000300": "i:1000301 i:1000302 x:66000302", "x:66000302": "i:1000301 i:1000302 x:66000300",
			}},
			{"B5, neither of a CUSIP", b5, map[string]string{"x:66000306": "x:66000307", "x:66000307": "x:66000306"}},
			{"B6, a ticker that a company of another CUSIP holds now", b6, map[string]string{"x:66000308": "i:1000306 c:ZZU40E103"}},
			{"B7, a later company after a cash merger alone under the CUSIP", b7, map[string]string{"i:1000309": "c:69389C109"}},
			{"B8, B1 of a -json import", b8, map[string]string{"x:66000302": "i:1000301"}},
			{"B9, a file of the rename's day", b9, map[string]string{"x:66000304": "i:1000304"}},
			{"S1, a spin-off merged away under its CUSIP", s1, map[string]string{"i:1000510": "x:66000510"}},
			{"S2, a spin-off merged away under its CUSIP, of an API key", s2, map[string]string{"x:66000520": "i:1000520"}},
			{"J1, renamed, its History file's key joined to its API key", j1, map[string]string{"i:1000801": "i:1000800"}},
			{"J2, B7 with the History file's key joined to the positions' key", j2, map[string]string{"i:1000309": "c:69389C109"}},
			{"J3, J1 beside an older company of S's new ticker", j3, map[string]string{"i:1000801": "i:1000800", "i:1000802": "i:1000800"}},
			{"F3, a ticker of two companies of CUSIPs", f3, map[string]string{"x:66000501": "i:1000501 c:ZZS50B109"}},
		} {
			ix := newToolIndex(&c.in)
			ix.groupAll()
			// Each listed, as unsureText says it: those that the export
			// writes, which have rows.
			var keys, listed []string
			for _, r := range c.in.securities {
				var of []string
				if u := ix.unsure[secRef{key: r.key}]; u != nil {
					for _, o := range u.of {
						of = append(of, o[0].key)
					}
				}
				if strings.Join(of, " ") != c.unsure[r.key] {
					t.Errorf("%s: %s may be %q, want %q", c.name, r.key, of, c.unsure[r.key])
				}
				if c.unsure[r.key] != "" && slices.ContainsFunc(c.in.rows, func(row toolRow) bool { return row.securityKey == r.key }) {
					keys = append(keys, r.key)
				}
			}
			slices.SortFunc(keys, keyRank)
			for _, k := range keys {
				var of []string
				for o := range strings.FieldsSeq(c.unsure[k]) {
					of = append(of, o+" schwabtx edit K:"+k+" -same K:"+o)
				}
				listed = append(listed, k+": "+strings.Join(of, "; "))
			}
			for _, tool := range []string{toolPP, toolGF} {
				e := caseExport(t, toolCase{name: c.name, in: c.in}, tool)
				if got := unsureText(e.Unsure); !slices.Equal(got, listed) {
					t.Errorf("%s %s: listed %q, want %q", c.name, tool, got, listed)
				}
				for _, ev := range e.events {
					s := ev.security
					ticker, wkn, name := ppIdentifiers(s)
					if alone := c.unsure[s.key] != ""; s.unsure != alone || alone &&
						(ticker != "" || wkn != "" || name != "Security ("+s.key+")" || s.manual != "unsure" || s.gfName() != name) {
						t.Errorf("%s %s: %s of %s: unsure %v, PP %q %q %q, Ghostfolio %s %q", c.name, tool, ev.ref, s.key, s.unsure, ticker, wkn, name, s.manual, s.gfName())
					}
				}
				s := chainStepFrom(t, e, t.TempDir())
				if tool == toolPP {
					m := newPPModel()
					ppApply(t, m, s)
					ppReconcile(t, m, s.in, s.events, s.outcomes)
					continue
				}
				m := newGFModel()
				if _, err := gfApply(t, m, s); err != nil {
					t.Fatal(err)
				}
				gfReconcile(t, s.in, s.events, s.outcomes, m)
			}
		}
		// What the store knows of each, which the entry and its securities
		// carry: F3's file key, of no description, named GRPZ by a History
		// file, and the two that it may be, of their CUSIPs, named so by
		// Schwab's API, the second held.
		e := caseExport(t, toolCase{name: "F3", in: f3}, toolPP)
		if got := unsureText(e.Unsure, true); !slices.Equal(got, []string{
			"x:66000501 [, GRPZ history 2026-02-10, rows 2019-05-01..2019-05-01]: " +
				"i:1000501 ZZR50A102 [, GRPZ api 2026-01-20, rows 2021-03-01..2021-03-01] schwabtx edit K:x:66000501 -same K:i:1000501; " +
				"c:ZZS50B109 ZZS50B109 [, GRPZ api 2026-11-10, held] schwabtx edit K:x:66000501 -same K:c:ZZS50B109",
		}) {
			t.Errorf("F3: listed %q", got)
		}
		// B6's file key's difference is marked, and that of the company of
		// its ticker now is not.
		var marked []string
		for _, d := range caseExport(t, toolCase{name: "B6", in: b6}, toolGF).Differences {
			marked = append(marked, fmt.Sprintf("%s %s %v", d.Key, d.Value, d.Unsure))
		}
		if !slices.Equal(marked, []string{"c:ZZU40E103 2 false", "x:66000308 -2 true"}) {
			t.Errorf("B6: differences %q", marked)
		}
		// B2's API key, written on its own, is held: PP, which has no feed
		// for it, gets its snapshot's price in securities.csv, as for any
		// security that it has no ticker of.
		if f := ppFileNamed(t, chainStepFrom(t, caseExport(t, toolCase{name: "B2", in: b2}, toolPP), t.TempDir()).pp, ppSecuritiesFile); !strings.Contains(string(f.data),
			"\n,,Security (i:1000303),TKRY NEW INC,USD,2026-11-10,50\n") {
			t.Errorf("B2: PP's securities:\n%s", f.data)
		}
		// A key's rows are its legs too: Q's TKNA, which J1's key of no
		// CUSIP sold in November 2025, in a journal of two securities of
		// January 2026.
		legged := j1
		legged.rows = append(slices.Clone(j1.rows), toolRow{
			ref: "T7f8a000003", account: 1, source: "api", time: tcNY("2026-01-02 10:00:00"), day: "2026-01-02", kind: "JOURNAL",
			description: "JOURNALED SHARES", amount: centsOf(0), fees: centsOf(0), gross: centsOf(0), category: "journal",
		})
		legged.legs = []toolLeg{
			{account: 1, ref: "T7f8a000003", leg: 1, symbol: "TKNA", securityKey: "i:1000801", quantity: centsOf(1_000_000), price: tcPrice(42)},
			{account: 1, ref: "T7f8a000003", leg: 2, symbol: "EFGH", securityKey: keyEFGH, quantity: centsOf(-1_000_000), price: tcPrice(30)},
		}
		if got := unsureText(caseExport(t, toolCase{name: "J1 with a leg", in: legged}, toolPP).Unsure, true); len(got) != 1 ||
			!strings.HasPrefix(got[0], "i:1000801 [TKNA NEW CORP, TKNA api 2026-01-05, rows 2025-10-01..2026-01-02]: ") {
			t.Errorf("J1 with a leg: listed %q", got)
		}
		// A description that an older History file brings later is the
		// first of the note, which PP updates on the security that it holds
		// when an export lists it again, and of a MANUAL asset's comment; the
		// name, and with it every row's fp, stays as it was.
		older := b1
		older.names = append(slices.Clone(b1.names), seen("x:66000302", "description", "TKRZ HOLDINGS CORP", "2025-01-02 09:00:00"))
		for _, tool := range []string{toolPP, toolGF} {
			e1 := chainStepOf(t, b1, tool, nil, rcAt1)
			e2 := chainStepOf(t, older, tool, e1, rcAt2)
			changed, gone := rcChanged(e2)
			if len(e2.res.written)+len(changed)+len(gone)+len(e2.res.symbolsChanged) > 0 {
				t.Errorf("%s: an older description writes %d records, changes %q, %q and %d securities", tool, len(e2.res.written), changed, gone, len(e2.res.symbolsChanged))
			}
			var notes []string
			for _, step := range []*chainStep{e1, e2} {
				for _, x := range step.events {
					if x.security.key == "x:66000302" {
						notes = append(notes, x.security.note)
					}
				}
			}
			if !slices.Equal(notes, []string{"TKRZ NEW HOLDINGS", "TKRZ HOLDINGS CORP; TKRZ NEW HOLDINGS"}) {
				t.Errorf("%s: notes %q", tool, notes)
			}
			full := caseExport(t, toolCase{name: "older", in: older}, tool)
			if tool == toolPP {
				if f := ppFileNamed(t, full.pp, ppSecuritiesFile); !strings.Contains(string(f.data), "\n,,Security (x:66000302),TKRZ HOLDINGS CORP; TKRZ NEW HOLDINGS,USD,,\n") {
					t.Errorf("PP's securities:\n%s", f.data)
				}
				continue
			}
			if p := full.gf.file.AssetProfiles; !slices.ContainsFunc(p, func(p gfProfile) bool {
				return p.Name == "Security (x:66000302)" && p.Comment == "TKRZ HOLDINGS CORP; TKRZ NEW HOLDINGS"
			}) {
				t.Errorf("Ghostfolio's assets %+v", p)
			}
		}
		// The rows of a symbol without a key, of a ticker that keys of the
		// store have had, are written on their own too, named so that no
		// ticker names them, and listed with their refs and the command that
		// gives each the security that it may be; the keys list them as one
		// that they may be, which no command names.
		keyless := b1
		keyless.rows = append(slices.Clone(b1.rows), rcTrade("T7f4a000109", 3, "2025-05-01 10:00:00", "buy", "", "TKRZ", 1_000_000, 31, -3_100, 0))
		keyless = tcInput(keyless)
		for _, tool := range []string{toolPP, toolGF} {
			e := caseExport(t, toolCase{name: "keyless", in: keyless}, tool)
			if got := unsureText(e.Unsure); !slices.Equal(got, []string{
				"x:66000302: i:1000301 schwabtx edit K:x:66000302 -same K:i:1000301; symbol TKRZ",
				"symbol TKRZ: i:1000301 schwabtx edit REF -security K:i:1000301; x:66000302 schwabtx edit REF -security K:x:66000302",
			}) || !slices.Equal(e.Unsure[1].Refs, []string{"T7f4a000109"}) {
				t.Errorf("%s: listed %q, refs %q", tool, got, e.Unsure[len(e.Unsure)-1].Refs)
			}
			i := slices.IndexFunc(e.events, func(ev toolEvent) bool { return ev.ref == "T7f4a000109" })
			if s := e.events[i].security; !s.unsure || s.gfName() != "Security (no key TKRZ)" || s.uuidName() != "security symbol:TKRZ" {
				t.Errorf("%s: the row without a key is of %+v", tool, s)
			}
			chainStepFrom(t, e, t.TempDir())
		}
	})

	t.Run("one writer per ticker", func(t *testing.T) {
		t.Parallel()
		// Securities of two CUSIPs whose current symbol is one ticker, as
		// when a company takes a ticker that another gave up, or a security
		// keeps its ticker under a new CUSIP: one at most writes it, as its
		// PP ticker, its symbol for Yahoo and its identity (I-5). Of
		// several, one that an account holds keeps it, else the one that
		// Schwab's API named by it last; one that only History files name
		// by it keeps it from none, and of the best rank two that the API
		// named by it last at one instant keep it from both. Each that does
		// not keep it is written as a delisted one is: no PP ticker, its
		// CUSIP as its WKN and identity, MANUAL in Ghostfolio (reused), and
		// named in PP's symbols_changed, no_ticker, because reused by the
		// key of the one that keeps it, or undecided. The tools hold them
		// apart.
		type sec struct {
			// key is an API key (i:) or a History file's (x:), whose names
			// are its source's; cusip is "" for none; seen is when its source
			// named it by the ticker, at New York's clock; held says that
			// A1's snapshot holds it.
			key, cusip, seen string
			held             bool
		}
		build := func(ticker string, secs []sec) toolInput {
			securities, names := tcSecurities()
			in := toolInput{securities: securities, names: names, snapshots: []toolSnapshot{rcSnapshot(1, "2026-11-10", 100_000)}}
			for i, s := range secs {
				seen := tcNY(s.seen)
				in.securities = append(in.securities, toolSecurityRow{key: s.key, assetType: "EQUITY", symbol: ticker, cusip: s.cusip})
				in.names = append(in.names,
					toolName{key: s.key, kind: "symbol", value: ticker, firstSeen: seen, lastSeen: seen},
					toolName{key: s.key, kind: "description", value: fmt.Sprintf("%s COMPANY %d", ticker, i+1), firstSeen: seen, lastSeen: seen})
				if historyFileKey(s.key) {
					in.rows = append(in.rows, rcFile(fmt.Sprintf("C7f9a%04d.1", i+1), 1, "2024-03-01", "Buy", "buy", s.key, ticker, centsOf(1_000_000), 10, -1_000))
				} else {
					in.rows = append(in.rows, rcTrade(fmt.Sprintf("T7f9a%06d", i+1), 1, "2024-03-01 10:00:00", "buy", s.key, ticker, 1_000_000, 10, -1_000, 0))
				}
				if s.held {
					in.positions = append(in.positions, rcPosition("2026-11-10", s.key, 1_000_000, 12))
				}
			}
			return tcInput(in)
		}
		for _, c := range []struct {
			name, ticker string
			secs         []sec
			// keeper is the key that keeps the ticker, "" for none.
			keeper string
		}{
			// An old -json imported after the snapshot dates the other's name
			// last: the one held keeps it all the same.
			{"held against named later", "TKWA", []sec{
				{"i:1000601", "ZZW60A104", "2025-03-03 18:30:00", true}, {"i:1000602", "ZZW60B102", "2026-11-12 10:04:05", false},
			}, "i:1000601"},
			// B2's: a History file names the old company by the ticker after
			// the API last named the new one so, which no instant of the
			// History file's outranks.
			{"named against History only", "TKWB", []sec{
				{"i:1000603", "ZZW60C100", "2025-09-02 18:30:00", false}, {"x:66000603", "ZZW60D108", "2026-02-10 09:00:00", false},
			}, "i:1000603"},
			{"held against History only", "TKWC", []sec{
				{"i:1000604", "ZZW60F103", "2025-09-02 18:30:00", true}, {"x:66000604", "ZZW60E106", "2026-02-10 09:00:00", false},
			}, "i:1000604"},
			{"two named, the later keeps it", "TKWD", []sec{
				{"i:1000605", "ZZW60G101", "2025-06-02 18:30:00", false}, {"i:1000606", "ZZW60H109", "2024-01-02 18:30:00", false},
			}, "i:1000605"},
			{"two held, the one named later keeps it", "TKWE", []sec{
				{"i:1000607", "ZZW60J105", "2026-03-02 18:30:00", true}, {"i:1000608", "ZZW60K102", "2026-11-10 18:05:00", true},
			}, "i:1000608"},
			{"two held at one instant", "TKWF", []sec{
				{"i:1000609", "ZZW60L100", "2026-11-10 18:05:00", true}, {"i:1000610", "ZZW60M108", "2026-11-10 18:05:00", true},
			}, ""},
			{"two named at one instant", "TKWG", []sec{
				{"i:1000611", "ZZW61A103", "2026-03-02 18:30:00", false}, {"i:1000612", "ZZW61B101", "2026-03-02 18:30:00", false},
			}, ""},
			// A tie keeps it from those of the tie alone, in whatever order
			// the keys come.
			{"two named at one instant, a third later", "TKWH", []sec{
				{"i:1000613", "ZZW61C109", "2026-05-01 18:30:00", false},
				{"i:1000614", "ZZW61D107", "2026-03-02 18:30:00", false},
				{"i:1000615", "ZZW61E105", "2026-03-02 18:30:00", false},
			}, "i:1000613"},
			{"two named at one instant, a third before", "TKWJ", []sec{
				{"i:1000616", "ZZW61F102", "2026-01-02 18:30:00", false},
				{"i:1000617", "ZZW61G100", "2026-03-02 18:30:00", false},
				{"i:1000618", "ZZW61H108", "2026-03-02 18:30:00", false},
			}, ""},
			{"one held beside two named at one instant", "TKWK", []sec{
				{"i:1000619", "ZZW61J104", "2026-03-02 18:30:00", false},
				{"i:1000620", "ZZW61K101", "2026-03-02 18:30:00", false},
				{"i:1000621", "ZZW61L109", "2024-01-02 18:30:00", true},
			}, "i:1000621"},
			// The History files' instants order no names but their own.
			{"History only on both sides", "TKWL", []sec{
				{"x:66000622", "ZZW62A102", "2025-01-15 09:00:00", false}, {"x:66000623", "ZZW62B100", "2026-01-15 09:00:00", false},
			}, ""},
			// One of no CUSIP is written on its own (I-4), which writes no
			// ticker: the other is the only one that may keep it.
			{"one of no CUSIP beside one of a CUSIP", "TKWM", []sec{
				{"i:1000624", "ZZW62C108", "2024-01-02 18:30:00", false}, {"x:66000624", "", "2026-01-15 09:00:00", false},
			}, "i:1000624"},
			{"one of no CUSIP, held and named later, beside one of a CUSIP", "TKWQ", []sec{
				{"i:1000627", "ZZW62F101", "2024-01-02 18:30:00", false}, {"i:1000628", "", "2026-11-10 18:05:00", true},
			}, "i:1000627"},
		} {
			in := build(c.ticker, c.secs)
			ix := newToolIndex(&in)
			var changed, manual []string
			keys := map[string]bool{}
			for _, s := range c.secs {
				keys[s.key] = true
				got := ix.security(s.key, "")
				keeps := s.key == c.keeper
				ticker, identity, why, symbols, by, name := c.ticker, c.ticker, "", "", "", "Security ("+c.ticker+")"
				switch {
				case s.cusip == "":
					ticker, identity, why, name = "", s.key, "unsure", "Security ("+s.key+")"
				case !keeps:
					ticker, identity, why, symbols, by, name = "", s.cusip, "reused", "no_ticker undecided", c.keeper, "Security ("+s.cusip+")"
					if c.keeper != "" {
						symbols = "no_ticker reused"
					}
					changed = append(changed, fmt.Sprintf("%s MANUAL no_ticker %s %s", s.key, strings.TrimPrefix(symbols, "no_ticker "), by))
				}
				if why != "" {
					manual = append(manual, s.key+" "+why)
				}
				if got.writes != keeps || got.ppTicker() != ticker || got.identity() != identity || got.manual != why || whysOf(got) != symbols ||
					got.reusedBy != by || got.gfName() != name {
					t.Errorf("%s: %s writes %v, PP ticker %q, identity %q, MANUAL %q as %q, symbols changed %q, reused by %q",
						c.name, s.key, got.writes, got.ppTicker(), got.identity(), got.manual, got.gfName(), whysOf(got), got.reusedBy)
				}
			}
			// The ticker finds one security at most in either tool, and every
			// other is held apart, as each tool holds it.
			for _, tool := range []string{toolPP, toolGF} {
				e := caseExport(t, toolCase{name: c.name, in: in}, tool)
				var m struct {
					SymbolsChanged *[]toolSymbolsEntry  `json:"symbols_changed"`
					Ghostfolio     *toolGhostfolioEntry `json:"ghostfolio"`
				}
				if err := json.Unmarshal(e.manifest, &m); err != nil {
					t.Fatal(err)
				}
				s := chainStepFrom(t, e, t.TempDir())
				if tool == toolPP {
					var got []string
					for _, x := range deref(m.SymbolsChanged) {
						got = append(got, fmt.Sprintf("%s %s %s %s %s", x.SecurityKey, x.WrittenAs, strings.Join(x.Why, ","), x.Because, x.ReusedBy))
					}
					f := ppFileNamed(t, s.pp, ppSecuritiesFile)
					if !slices.Equal(got, changed) || e.SymbolsChanged != len(changed) || strings.Count(string(f.data), "\n"+c.ticker+",") != map[bool]int{true: 1}[c.keeper != ""] {
						t.Errorf("%s: symbols changed %q (%d), want %q; PP's securities:\n%s", c.name, got, e.SymbolsChanged, changed, f.data)
					}
					pm := newPPModel()
					ppApply(t, pm, s)
					ppReconcile(t, pm, s.in, s.events, s.outcomes)
					if held := pm.shares("Schwab A1 ...135"); len(held) != len(c.secs) {
						t.Errorf("%s: PP holds %d securities of %d", c.name, len(held), len(c.secs))
					}
					continue
				}
				var got []string
				for _, x := range m.Ghostfolio.Manual {
					if keys[x.SecurityKey] {
						got = append(got, x.SecurityKey+" "+x.Why)
					}
				}
				if !slices.Equal(got, manual) || slices.Contains(m.Ghostfolio.Yahoo, c.ticker) != (c.keeper != "") || e.SymbolsChanged != 0 {
					t.Errorf("%s: Ghostfolio's manual %q, want %q; Yahoo %q; symbols changed %d", c.name, got, manual, m.Ghostfolio.Yahoo, e.SymbolsChanged)
				}
				gm := newGFModel()
				if _, err := gfApply(t, gm, s); err != nil {
					t.Fatal(err)
				}
				gfReconcile(t, s.in, s.events, s.outcomes, gm)
				if held := gm.holdings(); len(held) != len(c.secs) {
					t.Errorf("%s: Ghostfolio holds %v", c.name, held)
				}
			}
		}
		// A class share's ticker spelled two ways is one ticker, which the
		// one that the API named by it last keeps.
		spelled := build("TKWR.A", []sec{{"i:1000629", "ZZW62H107", "2025-01-02 18:30:00", false}, {"i:1000630", "ZZW62J103", "2026-01-02 18:30:00", false}})
		for i := range spelled.names {
			if n := &spelled.names[i]; n.key == "i:1000630" && n.kind == "symbol" {
				n.value = "TKWR/A"
			}
		}
		six := newToolIndex(&spelled)
		if a, b := six.security("i:1000629", ""), six.security("i:1000630", ""); a.writes || a.ppTicker() != "" || a.reusedBy != "i:1000630" || !b.writes || b.ppTicker() != "TKWR-A" {
			t.Errorf("one ticker spelled two ways: i:1000629 writes %v, PP ticker %q, reused by %q; i:1000630 writes %v, PP ticker %q", a.writes, a.ppTicker(), a.reusedBy, b.writes, b.ppTicker())
		}
		// One that an account held at an earlier snapshot, and not at its
		// latest, is not held: the one that the API named by the ticker
		// last keeps it.
		sold := build("TKWS", []sec{{"i:1000633", "ZZW62K100", "2026-06-01 18:05:00", false}, {"i:1000634", "ZZW62L108", "2026-09-01 18:30:00", false}})
		sold.positions = append(sold.positions, rcPosition("2026-06-01", "i:1000633", 1_000_000, 11))
		sold.snapshots = append(sold.snapshots, rcSnapshot(1, "2026-06-01", 100_000))
		six = newToolIndex(&sold)
		if a, b := six.security("i:1000633", ""), six.security("i:1000634", ""); a.writes || a.reusedBy != "i:1000634" || !b.writes {
			t.Errorf("held before the latest snapshot: i:1000633 writes %v, reused by %q; i:1000634 writes %v", a.writes, a.reusedBy, b.writes)
		}
		// One that the API named by the ticker before it took another is no
		// longer one of it: the other, alone of the ticker now, keeps it,
		// and each writes its own (I-5).
		renamed := build("TKWN", []sec{{"i:1000625", "ZZW62D106", "2024-01-02 18:30:00", false}, {"i:1000626", "ZZW62E104", "2025-06-02 18:30:00", false}})
		renamed.names = append(renamed.names, toolName{key: "i:1000625", kind: "symbol", value: "TKWO", firstSeen: tcNY("2025-01-02 18:30:00"), lastSeen: tcNY("2025-01-02 18:30:00")})
		ix := newToolIndex(&renamed)
		for key, ticker := range map[string]string{"i:1000625": "TKWO", "i:1000626": "TKWN"} {
			if s := ix.security(key, ""); !s.writes || s.ppTicker() != ticker || s.identity() != ticker || s.manual != "" {
				t.Errorf("renamed away: %s writes %v, PP ticker %q, identity %q, MANUAL %q", key, s.writes, s.ppTicker(), s.identity(), s.manual)
			}
		}
	})

	t.Run("a ticker and a CUSIP of one instant", func(t *testing.T) {
		t.Parallel()
		// A History JSON's key follows a spin-off from its first row, under
		// its CUSIP, to its purchases under its ticker, and the file dates
		// both names by its export: they tie, and the ticker is the latest,
		// since the security trades under it. It is not delisted, alone or
		// with the key of the positions, which saw the ticker since: one
		// security that Ghostfolio asks Yahoo about, whose symbol has not
		// changed, and that both tools hold as the snapshot does.
		const fileKey, cusip, posKey = "x:66000098", "69389B101", "c:69389B101"
		securities, names := tcSecurities()
		exported, first, last := tcNY("2026-01-15 09:00:00"), tcNY("2026-02-02 18:05:00"), tcNY("2026-11-10 18:05:00")
		alone := tcInput(toolInput{
			securities: append(securities, toolSecurityRow{key: fileKey, symbol: cusip, description: "SPINCO INC"}),
			names: append(names,
				toolName{key: fileKey, kind: "symbol", value: "SPNC", firstSeen: exported, lastSeen: exported},
				toolName{key: fileKey, kind: "symbol", value: cusip, firstSeen: exported, lastSeen: exported},
				toolName{key: fileKey, kind: "description", value: "SPINCO INC", firstSeen: exported, lastSeen: exported},
			),
			rows: []toolRow{
				rcFile("C7f2a2b3c.1", 1, "2026-01-05", "Journaled Shares", "transfer_in", fileKey, cusip, centsOf(10_000_000), 12, 0),
				rcFile("C7f2a2b3c.2", 1, "2026-01-08", "Buy", "buy", fileKey, "SPNC", centsOf(5_000_000), 12, -6_000),
			},
			snapshots: []toolSnapshot{rcSnapshot(1, "2026-11-10", 100_000)},
		})
		held := alone
		held.securities = append(slices.Clone(alone.securities), toolSecurityRow{key: posKey, assetType: "EQUITY", cusip: cusip, symbol: "SPNC", description: "SPINCO INC"})
		held.names = append(slices.Clone(alone.names),
			toolName{key: posKey, kind: "symbol", value: "SPNC", firstSeen: first, lastSeen: last},
			toolName{key: posKey, kind: "description", value: "SPINCO INC", firstSeen: first, lastSeen: last},
		)
		held.positions = []toolPosition{rcPosition("2026-11-10", posKey, 15_000_000, 195)}
		// A file whose one row of the security is the spin-off's, under its
		// CUSIP, exported the morning after the snapshot that held it under
		// its ticker: the file's key has no ticker, and saw the CUSIP on the
		// row's day, not at the export, which says nothing of a delisting.
		late := held
		late.securities = append(slices.Clone(securities), held.securities[len(held.securities)-2:]...)
		exportedLate := tcNY("2026-11-11 09:00:00")
		late.names = append(slices.Clone(names),
			toolName{key: fileKey, kind: "symbol", value: cusip, firstSeen: exportedLate, lastSeen: exportedLate},
			toolName{key: fileKey, kind: "description", value: "SPINCO INC", firstSeen: exportedLate, lastSeen: exportedLate},
			toolName{key: posKey, kind: "symbol", value: "SPNC", firstSeen: first, lastSeen: last},
			toolName{key: posKey, kind: "description", value: "SPINCO INC", firstSeen: first, lastSeen: last},
		)
		late.rows = slices.Clone(held.rows[:1])
		late.positions = []toolPosition{rcPosition("2026-11-10", posKey, 10_000_000, 195)}
		late = tcInput(late)
		// The same, with the spin-off received on the day of the one
		// snapshot that held it: the row's day is as of its start, before
		// that evening's snapshot.
		sameDay := late
		sameDay.names = slices.Clone(late.names)
		for i := range sameDay.names {
			if n := &sameDay.names[i]; n.key == posKey {
				n.firstSeen, n.lastSeen = tcNY("2026-01-05 18:05:00"), tcNY("2026-01-05 18:05:00")
			}
		}
		sameDay.positions = []toolPosition{rcPosition("2026-01-05", posKey, 10_000_000, 120)}
		sameDay.snapshots = []toolSnapshot{rcSnapshot(1, "2026-01-05", 100_000)}
		for _, c := range []toolCase{{name: "alone", in: alone}, {name: "held", in: held}, {name: "held, filed after the snapshot", in: late}, {name: "held, received on the snapshot's day", in: sameDay}} {
			for _, tool := range []string{toolPP, toolGF} {
				in, events, _ := mapCase(c, tool)
				for _, e := range events {
					if s := e.security; s.identity() != "SPNC" || s.delisted || s.manual != "" || whysOf(s) != "" || s.gfSymbol() != "SPNC" || s.ppTicker() != "SPNC" {
						t.Errorf("%s %s: %s is of %s, delisted %v, MANUAL %q, symbols changed %q", c.name, tool, e.ref, s.identity(), s.delisted, s.manual, whysOf(s))
					}
				}
				if diffs, _ := holdingsCheck(in, events); c.name != "alone" && len(diffs) != 0 {
					t.Errorf("%s %s: differences %+v", c.name, tool, diffs)
				}
				if e := caseExport(t, c, tool); e.SymbolsChanged != 0 {
					t.Errorf("%s %s: %d securities changed symbol", c.name, tool, e.SymbolsChanged)
				}
			}
		}
		// A security that the rows of one source name by its ticker and,
		// from its cash merger or its delisting on, by its CUSIP: a History
		// file, one API read of its years, or one transactions -json
		// import, which the store dates every name of by its export, the
		// read's answer or the import. The CUSIP's rows come last, so it is
		// delisted, as the securities view's symbol says: MANUAL in
		// Ghostfolio, with no ticker in PP, whose feed would take another
		// company's prices once the ticker is reused, and its symbol changed.
		// The CUSIP's rows decide by their last day, not their first: a
		// corporate action's old side under the CUSIP, between rows of the
		// ticker, leaves the security trading. Of rows of both on one day,
		// the ticker stands. A key of the positions that the CUSIP joins,
		// which saw the ticker at a snapshot before the file's export, adds
		// no row, and the file key's rows decide for both.
		gone := func(key, assetType, ticker, cusip string, seen time.Time, rows ...toolRow) toolInput {
			securities, names := tcSecurities()
			return tcInput(toolInput{
				securities: append(securities, toolSecurityRow{key: key, assetType: assetType, symbol: cusip}),
				names: append(names,
					toolName{key: key, kind: "symbol", value: ticker, firstSeen: seen, lastSeen: seen},
					toolName{key: key, kind: "symbol", value: cusip, firstSeen: seen, lastSeen: seen},
					toolName{key: key, kind: "description", value: ticker + " CORP", firstSeen: seen, lastSeen: seen},
				),
				rows: rows,
			})
		}
		merger := func(ref, key, cusip string) []toolRow {
			cash := rcFile(ref+".2", 1, "2025-12-01", "Cash Merger", "sell", key, cusip, sql.NullInt64{}, 0, 50_000)
			shares := rcFile(ref+".3", 1, "2025-12-01", "Cash Merger Adj", "other", key, cusip, centsOf(-10_000_000), 0, 0)
			cash.flags, shares.flags = []string{"two_row_event"}, []string{"two_row_event"}
			return []toolRow{cash, shares}
		}
		for _, c := range []struct {
			name, ticker string
			in           toolInput
			delisted     bool
		}{
			{"a cash merger in a History file", "MRGR", gone("x:66000301", "", "MRGR", "69389C109", tcNY("2026-01-15 09:00:00"), append([]toolRow{
				rcFile("C7f3a4b5c.1", 1, "2025-03-03", "Buy", "buy", "x:66000301", "MRGR", centsOf(10_000_000), 40, -40_000),
			}, merger("C7f3a4b5c", "x:66000301", "69389C109")...)...), true},
			{"a delisting in one API read", "TKRQ", gone("i:900000001", "EQUITY", "TKRQ", "69389D107", tcNY("2022-12-31 00:00:01"),
				rcTrade("T7f3a000101", 1, "2021-03-01 10:00:00", "buy", "i:900000001", "TKRQ", 10_000_000, 10, -10_000, 0),
				rcTrade("T7f3a000102", 1, "2022-06-15 10:00:00", "sell", "i:900000001", "69389D107", -10_000_000, 0.5, 500, 0)), true},
			{"a delisting in one -json import", "TKRP", gone("i:900000002", "EQUITY", "TKRP", "69389E105", tcNY("2026-11-12 10:04:05"),
				rcTrade("T7f3a000201", 1, "2021-03-02 10:00:00", "buy", "i:900000002", "TKRP", 10_000_000, 10, -10_000, 0),
				rcTrade("T7f3a000202", 1, "2022-06-16 10:00:00", "sell", "i:900000002", "69389E105", -10_000_000, 0.5, 500, 0)), true},
			{"a corporate action under the CUSIP between rows of the ticker", "TKRS", gone("x:66000305", "", "TKRS", "69389G100", tcNY("2026-01-15 09:00:00"),
				rcFile("C7f3a4b5d.1", 1, "2025-01-06", "Buy", "buy", "x:66000305", "TKRS", centsOf(10_000_000), 40, -40_000),
				rcFile("C7f3a4b5d.2", 1, "2025-06-02", "Exchange", "other", "x:66000305", "69389G100", centsOf(0), 0, 0),
				rcFile("C7f3a4b5d.3", 1, "2025-09-02", "Sell", "sell", "x:66000305", "TKRS", centsOf(-10_000_000), 45, 45_000)), false},
			{"a cash merger of a security held at a snapshot before it", "MRGR", func() toolInput {
				in := gone("x:66000301", "", "MRGR", "69389C109", tcNY("2026-01-15 09:00:00"), append([]toolRow{
					rcFile("C7f3a4b5c.1", 1, "2025-03-03", "Buy", "buy", "x:66000301", "MRGR", centsOf(10_000_000), 40, -40_000),
				}, merger("C7f3a4b5c", "x:66000301", "69389C109")...)...)
				in.securities = append(in.securities, toolSecurityRow{key: "c:69389C109", assetType: "EQUITY", symbol: "MRGR", cusip: "69389C109"})
				in.names = append(in.names, toolName{key: "c:69389C109", kind: "symbol", value: "MRGR", firstSeen: tcNY("2025-06-30 18:05:00"), lastSeen: tcNY("2025-06-30 18:05:00")})
				return in
			}(), true},
			// A file whose one row of the security is its cash merger, under
			// the CUSIP, which the positions' key saw under its ticker until a
			// snapshot before the merger: the file's key has no ticker, and saw
			// the CUSIP on the merger's day, after that snapshot.
			{"a cash merger alone under the CUSIP, after a snapshot that held it", "MRGR", func() toolInput {
				securities, names := tcSecurities()
				in := tcInput(toolInput{
					securities: append(securities, toolSecurityRow{key: "x:66000309", symbol: "69389C109"},
						toolSecurityRow{key: "c:69389C109", assetType: "EQUITY", symbol: "MRGR", cusip: "69389C109"}),
					names: append(names,
						toolName{key: "x:66000309", kind: "symbol", value: "69389C109", firstSeen: tcNY("2026-01-15 09:00:00"), lastSeen: tcNY("2026-01-15 09:00:00")},
						toolName{key: "c:69389C109", kind: "symbol", value: "MRGR", firstSeen: tcNY("2025-06-30 18:05:00"), lastSeen: tcNY("2025-11-28 18:05:00")},
					),
					rows: merger("C7f3a4b5e", "x:66000309", "69389C109"),
				})
				return in
			}(), true},
			// An API read after the delisting names the security's old buy by
			// its CUSIP, which the key alone has: the read dates it, after the
			// positions' key last saw the ticker.
			{"an API read of a key with no ticker, after the positions' last sighting", "MRGR", func() toolInput {
				securities, names := tcSecurities()
				return tcInput(toolInput{
					securities: append(securities, toolSecurityRow{key: "i:900000004", assetType: "EQUITY", symbol: "69389C109"},
						toolSecurityRow{key: "c:69389C109", assetType: "EQUITY", symbol: "MRGR", cusip: "69389C109"}),
					names: append(names,
						toolName{key: "i:900000004", kind: "symbol", value: "69389C109", firstSeen: tcNY("2023-01-15 10:00:00"), lastSeen: tcNY("2023-01-15 10:00:00")},
						toolName{key: "c:69389C109", kind: "symbol", value: "MRGR", firstSeen: tcNY("2021-03-01 18:05:00"), lastSeen: tcNY("2022-06-30 18:05:00")},
					),
					rows: []toolRow{rcTrade("T7f3a000401", 1, "2021-03-01 10:00:00", "buy", "i:900000004", "69389C109", 10_000_000, 10, -10_000, 0)},
				})
			}(), true},
			{"rows of both on one day", "TKRN", gone("i:900000003", "EQUITY", "TKRN", "69389F102", tcNY("2026-11-12 10:04:05"),
				rcTrade("T7f3a000301", 1, "2022-06-16 10:00:00", "buy", "i:900000003", "TKRN", 10_000_000, 10, -10_000, 0),
				rcTrade("T7f3a000302", 1, "2022-06-16 15:00:00", "sell", "i:900000003", "69389F102", -10_000_000, 11, 11_000, 0)), false},
		} {
			for _, tool := range []string{toolPP, toolGF} {
				_, events, _ := mapCase(toolCase{name: c.name, in: c.in}, tool)
				if len(events) == 0 {
					t.Errorf("%s %s: no records", c.name, tool)
				}
				for _, e := range events {
					s := e.security
					why, ticker, identity := "delisted", "", s.cusip
					if !c.delisted {
						why, ticker, identity = "", c.ticker, c.ticker
					}
					if s.delisted != c.delisted || s.manual != why || s.ppTicker() != ticker || s.identity() != identity || whysOf(s) != map[bool]string{true: "no_ticker delisted"}[c.delisted] {
						t.Errorf("%s %s: %s is of %s, delisted %v, MANUAL %q, PP ticker %q, symbols changed %q", c.name, tool, e.ref, s.identity(), s.delisted, s.manual, s.ppTicker(), whysOf(s))
					}
				}
				if e := caseExport(t, toolCase{name: c.name, in: c.in}, tool); e.SymbolsChanged != map[bool]int{true: 1}[c.delisted && tool == toolPP] {
					t.Errorf("%s %s: %d securities changed symbol", c.name, tool, e.SymbolsChanged)
				}
			}
		}
	})

	t.Run("cash", func(t *testing.T) {
		t.Parallel()
		// Every kind of record that PP gets, in two accounts, against
		// snapshots after the last row: the check's cash of each account is
		// what PP's model holds once the export is imported, and a snapshot
		// of other cash is a difference.
		in := ppCheckerInput()
		in.snapshots = []toolSnapshot{rcSnapshot(1, "2025-06-30", 0), rcSnapshot(2, "2025-06-30", 123)}
		c := toolCase{name: "every record", in: in}
		_, events, _ := mapCase(c, toolPP)
		diffs, _ := cashCheck(&in, events)
		s := chainStepFrom(t, caseExport(t, c, toolPP), t.TempDir())
		m := newPPModel()
		ppApply(t, m, s)
		if len(diffs) != 2 {
			t.Fatalf("differences %+v", diffs)
		}
		for i, d := range diffs {
			if d.account.id != int64(i+1) || d.snapshot != "2025-06-30" || d.snapshotCash != in.snapshots[i].cash.Int64 || d.writtenCash != m.cash(d.account.name()) {
				t.Errorf("%+v: PP holds %s", d, formatCents(m.cash(d.account.name())))
			}
		}
		in.snapshots[0].cash = centsOf(m.cash("Schwab A1 ...135"))
		if diffs, _ := cashCheck(&in, events); len(diffs) != 1 || diffs[0].account.id != 2 {
			t.Errorf("A1's cash as PP holds it: differences %+v", diffs)
		}

		// An account that held cash before the store's first row. Its
		// snapshot is the latest taken by the end of -to, the one that the
		// holdings check compares with too, not one of -to's session taken
		// the next morning, which may hold rows dated after -to; and its
		// rows are those that the snapshot holds: an API row up to the
		// instant that it was taken, not after; a file row posted on or
		// before its market date, the evening's snapshot following the
		// day's postings. A transfer's other side, which PP makes from the
		// paying side although its own row is dated after -to, is not the
		// snapshot's yet. An account whose snapshot matches, and one
		// without, say nothing; neither does Ghostfolio, whose cash is the
		// balances'.
		opening := tcInput(toolInput{
			to: "2026-06-30",
			rows: []toolRow{
				rcTrade("T7c000001", 1, "2026-03-02 10:00:00", "buy", keyEFGH, "EFGH", 100_000_000, 30, -300_000, 0),
				rcCash("T7c000002", 1, "2026-06-29 18:05:00", "ACH_RECEIPT", "ACH Credit", "deposit", 1_000),
				rcCash("T7c000003", 1, "2026-06-29 18:05:01", "ACH_RECEIPT", "ACH Credit", "deposit", 2_000),
				rcFile("C7c0a0000.1", 1, "2026-06-29", "Bank Interest", "interest", "", "", sql.NullInt64{}, 0, 3_000),
				rcFile("C7c0a0000.2", 1, "2026-06-28", "Bank Interest", "interest", "", "", sql.NullInt64{}, 0, 4_000),
				rcJournal("T7c000004", 1, "2026-06-29 10:00:00", "JOURNAL TO ...398", -50_000, "T7c000101", 2),
				rcCash("T7c000102", 2, "2026-06-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 10_000),
				rcCash("T7c000201", 3, "2026-06-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 10_000),
			},
			snapshots: []toolSnapshot{
				rcSnapshot(1, "2026-06-29", 700_000), rcSnapshot(1, "2026-07-01", 999_999),
				{account: 1, marketDate: "2026-06-30", fetchedAt: tcNY("2026-07-01 07:00:00"), cash: centsOf(888_888)},
				rcSnapshot(2, "2026-06-29", 10_000),
			},
			positions: []toolPosition{rcPosition("2026-06-29", keyEFGH, 100_000_000, 3100)},
		})
		// The journal's other side, of A2, is dated after -to.
		opening.rows[5].partner = toolPartner{day: "2026-07-01", time: tcNY("2026-07-01 09:30:00"), amount: centsOf(50_000), counted: true, category: "journal", kind: "JOURNAL", description: "JOURNAL FRM ...135"}
		for _, tool := range []string{toolPP, toolGF} {
			o := toolCase{name: "opening cash", in: opening}
			in, events, _ := mapCase(o, tool)
			e := caseExport(t, o, tool)
			if tool == toolGF {
				if _, cash := counted(e); cash != 0 {
					t.Errorf("Ghostfolio counts %d accounts' cash", cash)
				}
				continue
			}
			want := []toolCash{{account: in.accountOf(1), snapshot: "2026-06-29", snapshotCash: 700_000, writtenCash: -300_000 + 1_000 + 3_000 + 4_000 - 50_000}}
			if got, _ := cashCheck(in, events); !reflect.DeepEqual(got, want) {
				t.Errorf("the opening cash: differences %+v, want %+v", got, want)
			}
			if _, cash := counted(e); cash != 1 {
				t.Errorf("stderr counts %d accounts' cash", cash)
			}
			// An export of A1 checks A1's cash alone, whatever other
			// account's snapshot it is given.
			only := onlyAccount(*in, 1)
			only.snapshots = append(only.snapshots, rcSnapshot(2, "2026-06-29", 1))
			events, _ = toolEvents(&only)
			if got, _ := cashCheck(&only, events); !reflect.DeepEqual(got, want) {
				t.Errorf("the opening cash of A1 alone: differences %+v, want %+v", got, want)
			}
			// A4's snapshot holds cash, and A5's is of a margin account that
			// reads its cash as 0 beside a debit, but neither has a row: PP
			// holds nothing of them, the export names no PP account for
			// them, and their cash is not checked but listed as such, not as
			// a margin account's to compare by hand; A3's row has no snapshot
			// to check against.
			snapshotOnly := *in
			margin := rcSnapshot(5, "2026-06-29", 0)
			margin.accountType, margin.margin, margin.short = "MARGIN", sql.NullFloat64{Float64: -2000, Valid: true}, sql.NullFloat64{Valid: true}
			snapshotOnly.snapshots = append(slices.Clone(in.snapshots), rcSnapshot(4, "2026-06-29", 250_000), margin)
			s := chainStepFrom(t, caseExport(t, toolCase{name: "a snapshot alone", in: snapshotOnly}, toolPP), t.TempDir())
			var got []string
			for _, c := range s.res.cash {
				got = append(got, c.Account)
			}
			for _, u := range s.res.cashUnchecked {
				got = append(got, u.Account+" margin")
			}
			for _, a := range s.res.unchecked {
				got = append(got, a.label()+" not checked")
			}
			if _, cash := counted(s.e); !slices.Equal(got, []string{"A1 ...135", "A3 ...783 not checked", "A4 not checked", "A5 not checked"}) || cash != 1 || len(s.e.CashNotChecked) != 0 ||
				!slices.Equal(s.e.NoSnapshot, []string{"A3 ...783"}) || !slices.Equal(s.e.NotInTool, []string{"A4", "A5"}) {
				t.Errorf("snapshots alone of A4 and A5: %q, counted %d, cash not checked %q, no snapshot %q, nothing in PP %q", got, cash, s.e.CashNotChecked, s.e.NoSnapshot, s.e.NotInTool)
			}
			// A later snapshot of A1 by -to holds no cash figure: one
			// snapshot serves both checks, so A1's cash is not checked,
			// against no earlier snapshot, and is listed with why.
			noCash := *in
			noCash.snapshots = append(slices.Clone(in.snapshots), toolSnapshot{account: 1, marketDate: "2026-06-30", fetchedAt: tcNY("2026-06-30 18:05:00")})
			events, _ = toolEvents(&noCash)
			if diffs, unchecked := cashCheck(&noCash, events); len(diffs) != 0 ||
				!reflect.DeepEqual(unchecked, []toolCashUnchecked{{account: in.accountOf(1), snapshot: "2026-06-30", why: "no_cash"}}) {
				t.Errorf("a later snapshot without cash: differences %+v, unchecked %+v", diffs, unchecked)
			}
			s = chainStepFrom(t, caseExport(t, toolCase{name: "a later snapshot without cash", in: noCash}, toolPP), t.TempDir())
			if _, cash := counted(s.e); cash != 0 || !slices.Equal(s.e.CashNotChecked, []ToolCashUnchecked{{Account: "A1 ...135", Why: "no_cash"}}) {
				t.Errorf("a later snapshot without cash: counted %d, cash not checked %q", cash, s.e.CashNotChecked)
			}
		}

		// A transfer is one record, of its paying side, which PP books into
		// its Offset Account too: the check credits the receiving account
		// by the receiving row's own day and time, as Q2 reads them of the
		// paying row's partner. A snapshot taken once that row has posted
		// holds it; one taken before does not, of an earlier day or of the
		// row's own day before its time, nor one by -to when the row is
		// dated after it; each matches the records either way. PP's model
		// holds what the check counts.
		pays := rcJournal("T7c010201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", -100_000, "T7c010101", 1)
		receives := rcJournal("T7c010101", 1, "2026-03-03 08:00:00", "JOURNAL FRM ...398", 100_000, pays.ref, 2)
		receivesLate := rcJournal("T7c010101", 1, "2026-03-03 19:40:00", "JOURNAL FRM ...398", 100_000, pays.ref, 2)
		funds := rcCash("T7c010202", 2, "2026-03-01 10:00:00", "WIRE_IN", "Wire Incoming", "deposit", 500_000)
		alone := pays
		alone.partner = toolPartner{day: "2026-03-03", time: receives.time, amount: receives.amount, counted: true, category: "journal", kind: receives.kind, description: receives.description}
		for _, c := range []struct {
			what string
			in   toolInput
			// a1 is the receiving account's snapshot cash, in cents.
			a1 int64
		}{
			{"held", tcInput(toolInput{to: "2026-03-06", rows: []toolRow{funds, pays, receives}, snapshots: []toolSnapshot{
				rcSnapshot(1, "2026-03-05", 100_000), rcSnapshot(2, "2026-03-05", 400_000),
			}}), 100_000},
			{"not yet posted", tcInput(toolInput{to: "2026-03-06", rows: []toolRow{funds, pays, receives}, snapshots: []toolSnapshot{
				rcSnapshot(1, "2026-03-02", 0), rcSnapshot(2, "2026-03-05", 400_000),
			}}), 0},
			{"posted that evening, after the snapshot", tcInput(toolInput{to: "2026-03-06", rows: []toolRow{funds, pays, receivesLate}, snapshots: []toolSnapshot{
				rcSnapshot(1, "2026-03-03", 0), rcSnapshot(2, "2026-03-05", 400_000),
			}}), 0},
			{"dated after -to", tcInput(toolInput{to: "2026-03-02", rows: []toolRow{funds, alone}, snapshots: []toolSnapshot{
				rcSnapshot(1, "2026-03-02", 0), rcSnapshot(2, "2026-03-02", 400_000),
			}}), 0},
		} {
			in, events, _ := mapCase(toolCase{name: c.what, in: c.in}, toolPP)
			if diffs, unchecked := cashCheck(in, events); len(diffs)+len(unchecked) > 0 {
				t.Errorf("a transfer, %s: differences %+v, unchecked %+v", c.what, diffs, unchecked)
			}
			s := chainStepFrom(t, caseExport(t, toolCase{name: c.what, in: c.in}, toolPP), t.TempDir())
			m := newPPModel()
			ppApply(t, m, s)
			if got := m.cash("Schwab A1 ...135"); c.a1 == 100_000 && got != c.a1 || len(s.res.cash) > 0 {
				t.Errorf("a transfer, %s: PP's A1 holds %s; cash entries %+v", c.what, formatCents(got), s.res.cash)
			}
		}

		// A margin account with a margin balance, as of a debit, or a short
		// balance, of short proceeds, is not checked, whatever its cash
		// balance reads, and is listed apart, why margin; so is an account
		// whose snapshot holds no cash figure, why no_cash, but margin for
		// a margin account in use. A margin account whose margin and short
		// balances read 0, and a cash account, are checked as any other.
		margin := func(cash int64, typ string, marginBalance, short float64) toolInput {
			snap := rcSnapshot(2, "2026-01-30", cash)
			snap.accountType = typ
			snap.margin, snap.short = sql.NullFloat64{Float64: marginBalance, Valid: true}, sql.NullFloat64{Float64: short, Valid: true}
			return tcInput(toolInput{
				to: "2026-01-30",
				rows: []toolRow{
					rcCash("T7c600201", 2, "2026-01-02 10:00:00", "ACH_RECEIPT", "ACH Credit", "deposit", 100_000),
					rcTrade("T7c600202", 2, "2026-01-05 10:00:00", "buy", keyEFGH, "EFGH", 100_000_000, 30, -300_000, 0),
				},
				snapshots: []toolSnapshot{snap},
			})
		}
		noFigure := func(in toolInput) toolInput {
			in.snapshots[0].cash = sql.NullInt64{}
			return in
		}
		for _, c := range []struct {
			name  string
			in    toolInput
			diffs int
			// why is why the cash is not checked, "" when it is.
			why string
		}{
			{"a debit read as 0", margin(0, "MARGIN", -2000, 0), 0, "margin"},
			{"short proceeds read as 0", margin(0, "MARGIN", 0, 5000), 0, "margin"},
			{"a debit read as negative cash", margin(-200_000, "MARGIN", -2000, 0), 0, "margin"},
			{"cash beside a short balance", margin(50_000, "MARGIN", 0, 5000), 0, "margin"},
			{"a margin account of no cash nor balances", margin(0, "MARGIN", 0, 0), 1, ""},
			{"a cash account", margin(0, "CASH", -2000, 0), 1, ""},
			{"no cash figure", noFigure(margin(0, "CASH", 0, 0)), 0, "no_cash"},
			{"no cash figure beside a debit", noFigure(margin(0, "MARGIN", -2000, 0)), 0, "margin"},
		} {
			mc := toolCase{name: c.name, in: c.in}
			in, events, _ := mapCase(mc, toolPP)
			diffs, unchecked := cashCheck(in, events)
			why := ""
			if len(unchecked) == 1 {
				why = unchecked[0].why
			}
			if len(diffs) != c.diffs || why != c.why || len(unchecked) > 1 {
				t.Errorf("%s: differences %+v, unchecked %+v", c.name, diffs, unchecked)
			}
			// The manifest and stderr say so too (checkToolSummary).
			e := chainStepFrom(t, caseExport(t, mc, toolPP), t.TempDir()).e
			var m struct {
				Holdings chainHoldings `json:"holdings"`
			}
			if err := json.Unmarshal(e.manifest, &m); err != nil {
				t.Fatal(err)
			}
			var want []chainCashUnchecked
			if c.why != "" {
				want = []chainCashUnchecked{{Account: "A2 ...398", Snapshot: "2026-01-30", Why: c.why}}
			}
			if _, cash := counted(e); !slices.Equal(*m.Holdings.CashNotChecked, want) && len(*m.Holdings.CashNotChecked)+len(want) > 0 || cash != c.diffs {
				t.Errorf("%s: stderr counts %d, the manifest lists %+v as not checked", c.name, cash, *m.Holdings.CashNotChecked)
			}
		}
	})

	t.Run("in the snapshot", func(t *testing.T) {
		t.Parallel()
		// What the holdings and the cash checks take a snapshot to hold: the
		// rows of History files posted on or before its market date, as an
		// evening snapshot holds the day's postings, which have no time; an
		// API row filled after it was taken, in the evening of its market
		// date, not; and of a snapshot taken after midnight, which stands for
		// the session before, not the new day's postings. A snapshot of
		// -to's session taken the morning after -to is not the one checked:
		// it may hold rows dated after -to, as an overnight fill.
		file := func(ref, posted, action, category, key, symbol string, quantity sql.NullInt64, price float64, amount int64) toolRow {
			return rcFile(ref, 3, nytime.Date(posted), action, category, key, symbol, quantity, price, amount)
		}
		snapshot := func(account int64, day, clock string, cash int64) toolSnapshot {
			return toolSnapshot{account: account, marketDate: nytime.Date(day), fetchedAt: tcNY(clock), cash: centsOf(cash)}
		}
		position := func(account int64, day, clock, key string, long int64, value float64) toolPosition {
			p := rcPosition(day, key, long, value)
			p.account, p.fetchedAt = account, tcNY(clock)
			return p
		}
		for _, c := range []struct {
			name     string
			in       toolInput
			holdings []string
			cash     []string
		}{
			{"a file's buy posted on the evening snapshot's day", toolInput{
				to: "2026-03-02",
				rows: []toolRow{
					file("C7c5a0001.1", "2026-03-01", "MoneyLink Deposit", "deposit", "", "", sql.NullInt64{}, 0, 100_000),
					file("C7c5a0001.2", "2026-03-02", "Buy", "buy", keyXYZ, "XYZ", centsOf(10_000_000), 50, -50_000),
				},
				snapshots: []toolSnapshot{snapshot(3, "2026-03-02", "2026-03-02 18:05:00", 50_000)},
				positions: []toolPosition{position(3, "2026-03-02", "2026-03-02 18:05:00", keyXYZ, 10_000_000, 505)},
			}, nil, nil},
			{"file rows posted on the days of the evening's snapshots", toolInput{
				to: "2026-03-03",
				rows: []toolRow{
					file("C7c5a0002.1", "2026-03-02", "MoneyLink Deposit", "deposit", "", "", sql.NullInt64{}, 0, 100_000),
					file("C7c5a0002.2", "2026-03-03", "Qualified Dividend", "dividend", keyXYZ, "XYZ", sql.NullInt64{}, 0, 1_234),
				},
				snapshots: []toolSnapshot{snapshot(3, "2026-03-02", "2026-03-02 18:05:00", 100_000), snapshot(3, "2026-03-03", "2026-03-03 18:05:00", 101_234)},
			}, nil, nil},
			{"a file row of the new day, after a snapshot taken after midnight", toolInput{
				to: "2026-03-03",
				rows: []toolRow{
					file("C7c5a0003.1", "2026-03-02", "MoneyLink Deposit", "deposit", "", "", sql.NullInt64{}, 0, 100_000),
					file("C7c5a0003.2", "2026-03-03", "Buy", "buy", keyXYZ, "XYZ", centsOf(10_000_000), 50, -50_000),
				},
				snapshots: []toolSnapshot{snapshot(3, "2026-03-02", "2026-03-03 00:05:00", 100_000)},
			}, nil, nil},
			{"a sale filled after the evening snapshot of its day", toolInput{
				to: "2026-03-02",
				rows: []toolRow{
					rcCash("T7c500300", 1, "2026-03-02 09:00:00", "ACH_RECEIPT", "ACH Credit", "deposit", 50_000),
					rcTrade("T7c500301", 1, "2026-03-02 10:00:00", "buy", keyXYZ, "XYZ", 10_000_000, 50, -50_000, 0),
					rcTrade("T7c500302", 1, "2026-03-02 18:30:00", "sell", keyXYZ, "XYZ", -10_000_000, 51, 51_000, 0),
				},
				snapshots: []toolSnapshot{snapshot(1, "2026-03-02", "2026-03-02 18:05:00", 0)},
				positions: []toolPosition{position(1, "2026-03-02", "2026-03-02 18:05:00", keyXYZ, 10_000_000, 505)},
			}, nil, nil},
			{"a snapshot of -to's session taken the morning after, past an overnight sale", toolInput{
				to: "2026-03-02",
				rows: []toolRow{
					rcCash("T7c500400", 1, "2026-03-02 09:00:00", "ACH_RECEIPT", "ACH Credit", "deposit", 50_000),
					rcTrade("T7c500401", 1, "2026-03-02 10:00:00", "buy", keyXYZ, "XYZ", 10_000_000, 50, -50_000, 0),
				},
				snapshots: []toolSnapshot{snapshot(1, "2026-03-02", "2026-03-02 18:05:00", 0), snapshot(1, "2026-03-02", "2026-03-03 00:05:00", 51_000)},
				positions: []toolPosition{position(1, "2026-03-02", "2026-03-02 18:05:00", keyXYZ, 10_000_000, 505)},
			}, nil, nil},
			// The checks still see what the rows leave out: here an evening
			// snapshot of a day before the buy's.
			{"a file's buy posted the day after the snapshot", toolInput{
				to: "2026-03-03",
				rows: []toolRow{
					file("C7c5a0005.1", "2026-03-01", "MoneyLink Deposit", "deposit", "", "", sql.NullInt64{}, 0, 100_000),
					file("C7c5a0005.2", "2026-03-03", "Buy", "buy", keyXYZ, "XYZ", centsOf(10_000_000), 50, -50_000),
				},
				snapshots: []toolSnapshot{snapshot(3, "2026-03-02", "2026-03-02 18:05:00", 50_000)},
				positions: []toolPosition{position(3, "2026-03-02", "2026-03-02 18:05:00", keyXYZ, 10_000_000, 505)},
			}, []string{"A3 ...783 XYZ 10 0"}, []string{"A3 ...783 500.00 1000.00"}},
		} {
			for _, tool := range []string{toolPP, toolGF} {
				in, events, _ := mapCase(toolCase{name: c.name, in: tcInput(c.in)}, tool)
				diffs, _ := holdingsCheck(in, events)
				var holdings, cash []string
				for _, d := range diffs {
					holdings = append(holdings, fmt.Sprintf("%s %s %s %s", d.account.label(), d.symbol, formatMicros(d.snapshotShares), formatMicros(d.writtenShares)))
				}
				cashDiffs, _ := cashCheck(in, events)
				for _, d := range cashDiffs {
					cash = append(cash, fmt.Sprintf("%s %s %s", d.account.label(), formatCents(d.snapshotCash), formatCents(d.writtenCash)))
				}
				if !slices.Equal(holdings, c.holdings) || tool == toolPP && !slices.Equal(cash, c.cash) {
					t.Errorf("%s %s: holdings %q, cash %q", c.name, tool, holdings, cash)
				}
			}
		}
	})

	t.Run("PP's securities", func(t *testing.T) {
		t.Parallel()
		// The securities that PP takes for one, met in their order from a PP
		// that holds none, as its import finds a row's security: by its
		// ticker, then its WKN; else by a name that no security made before
		// has but one, whose ticker and WKN do not differ from the row's.
		sec := func(key, ticker, cusip, name string) *toolSecurity {
			s := &toolSecurity{key: key, cusip: cusip, name: name}
			if ticker != "" {
				s.tickers, s.writes = []string{ticker}, true
			}
			return s
		}
		secs := []*toolSecurity{
			sec("i:1", "", "ZZG10A103", "SAME NAME A"),
			sec("i:2", "", "ZZG10B101", "SAME NAME A"),
			sec("i:3", "TKRA", "", "SAME NAME B"),
			sec("i:4", "TKRB", "", "SAME NAME B"),
			sec("i:5", "", "", "SAME NAME C"),
			sec("i:6", "", "ZZG10C109", "SAME NAME C"),
			sec("i:7", "TKRC", "", "SAME NAME D"),
			sec("i:8", "", "ZZG10E105", "SAME NAME D"),
			sec("i:9", "", "", "SAME NAME A"),
			sec("i:10", "TKRA", "ZZG10F102", "OTHER"),
		}
		same := sameSecurity(toolPP, secs)
		// i:2 and i:4 differ by WKN and ticker from the security of their
		// name; i:6 and i:8 have one ticker or WKN where it has none of
		// that; i:9's name is two securities'; i:10's ticker finds i:3.
		want := map[string]string{"i:1": "i:1", "i:2": "i:2", "i:3": "i:3", "i:4": "i:4", "i:5": "i:5", "i:6": "i:5", "i:7": "i:7", "i:8": "i:7", "i:9": "i:9", "i:10": "i:3"}
		for _, s := range secs {
			if got := same[s.ref()].key; got != want[s.key] {
				t.Errorf("%s is %s's security, want %s's", s.key, got, want[s.key])
			}
		}
	})

	t.Run("labels", func(t *testing.T) {
		t.Parallel()
		c := toolCaseNamed("transfers")
		_, events, outcomes := mapCase(c, toolPP)
		// Another account with the same last digits changes no label.
		c.in.accounts = append(slices.Clone(c.in.accounts), toolAccount{id: 4, masked: "...398", nickname: "Trust"})
		_, again, againOutcomes := mapCase(c, toolPP)
		if fmt.Sprint(events) != fmt.Sprint(again) || fmt.Sprint(outcomes) != fmt.Sprint(againOutcomes) {
			t.Errorf("another account ending in 398 changed the mapping")
		}
		for _, a := range []struct {
			account                   toolAccount
			label, name, suffix, gfID string
		}{
			{toolAccount{id: 2, masked: "...398"}, "A2 ...398", "Schwab A2 ...398", "A2-398", "2a1620c1-a41c-8592-88fc-7092e7c386d4"},
			{toolAccount{id: 1, masked: "...135", nickname: "Individual"}, "A1 ...135", "Schwab A1 ...135", "A1-135", "e6461b57-da15-845e-b90b-7d04787642e5"},
			{toolAccount{id: 4, masked: "...398", nickname: "Trust"}, "A4 ...398", "Schwab A4 ...398", "A4-398", uuid8(toolNamespace, "account A4 ...398")},
			{toolAccount{id: 5, masked: "..."}, "A5 ...", "Schwab A5 ...", "A5", uuid8(toolNamespace, "account A5 ...")},
			{toolAccount{id: 6}, "A6", "Schwab A6", "A6", uuid8(toolNamespace, "account A6")},
		} {
			if a.account.label() != a.label || a.account.name() != a.name || a.account.suffix() != a.suffix || a.account.gfID() != a.gfID {
				t.Errorf("%+v: %q %q %q %s", a.account, a.account.label(), a.account.name(), a.account.suffix(), a.account.gfID())
			}
		}
		if in := c.in; in.accountOf(4).nickname != "Trust" || in.accountOf(9) != (toolAccount{id: 9}) {
			t.Errorf("accountOf: %+v, %+v", in.accountOf(4), in.accountOf(9))
		}
	})

	t.Run("kinds", func(t *testing.T) {
		t.Parallel()
		for k := evDeposit; k <= evCloseIn; k++ {
			file, typ := k.ppType()
			if k.String() == "" || typ == "" || file != "account" && file != "portfolio" {
				t.Errorf("kind %d: %q, %s %q", k, k, file, typ)
			}
			if (file == "portfolio") != (k.shareSign() != 0) {
				t.Errorf("kind %s moves shares %d, in PP's %s file", k, k.shareSign(), file)
			}
			if (k.gfType() == "") != (k == evDeposit || k == evWithdrawal || k == evTransferOut) {
				t.Errorf("kind %s is Ghostfolio's %q", k, k.gfType())
			}
		}
	})
}

// checkCase maps case c for each tool and requires what the case says of
// each row, one outcome per row (checkOutcomes).
func checkCase(t *testing.T, c toolCase) {
	t.Helper()
	for _, tool := range []string{toolPP, toolGF} {
		in, events, outcomes := mapCase(c, tool)
		checkOutcomes(t, c.name+" "+tool, in, events, outcomes)
		want := map[string]toolWant{}
		for _, w := range c.want {
			want[w.ref] = w
		}
		for i, r := range in.rows {
			w, ok := want[r.ref]
			if !ok {
				t.Errorf("%s: the case says nothing of %s", c.name, r.ref)
				continue
			}
			delete(want, r.ref)
			expect := w.pp
			if tool == toolGF {
				expect = w.gf
			}
			if got := describeRow(tool, outcomes[i], eventsOf(events, i)); got != expect {
				t.Errorf("%s %s %s: %s, want %s", c.name, tool, r.ref, got, expect)
			}
		}
		for ref := range want {
			t.Errorf("%s: the case wants %s, which it does not hold", c.name, ref)
		}
	}
}

// checkOutcomes requires that the mapping gives every row of in one
// outcome, in the rows' order; records only of the rows it writes, at
// least one each, of the row's ref and account, with a price when they are
// deliveries (0 for an option closed) and none else; a written row's
// caveats, those of its records; and a combined row the cash row it names,
// written, whose records carry both refs, or for a transfer's receiving
// side the paying side that it names, written as one Transfer (Outbound)
// that names it.
func checkOutcomes(t *testing.T, what string, in *toolInput, events []toolEvent, outcomes []toolOutcome) {
	t.Helper()
	if len(outcomes) != len(in.rows) {
		t.Fatalf("%s: %d outcomes for %d rows", what, len(outcomes), len(in.rows))
	}
	written := map[int]int{}
	for j, e := range events {
		if e.row < 0 || e.row >= len(in.rows) || e.ref != in.rows[e.row].ref || e.account.id != in.rows[e.row].account {
			t.Errorf("%s: record %d (%s) is of row %d", what, j, e.ref, e.row)
			continue
		}
		if j > 0 && e.row < events[j-1].row {
			t.Errorf("%s: record %d (%s) is out of the rows' order", what, j, e.ref)
		}
		switch e.kind {
		case evDeliveryIn, evDeliveryOut:
			if e.price == nil || e.price.Sign() < 0 {
				t.Errorf("%s: %s, a %s, has the price %v", what, e.ref, e.kind, e.price)
			}
		case evCloseIn, evCloseOut:
			if e.price == nil || e.price.Sign() != 0 || e.value != 0 {
				t.Errorf("%s: %s, a %s, has the price %v and the value %s", what, e.ref, e.kind, e.price, formatCents(e.value))
			}
		default:
			if e.price != nil {
				t.Errorf("%s: %s, a %s, has the price %s", what, e.ref, e.kind, e.price.RatString())
			}
		}
		written[e.row]++
	}
	byRef := map[rowKey]int{}
	for i, r := range in.rows {
		byRef[rowKey{r.account, r.ref}] = i
	}
	for i, o := range outcomes {
		r := in.rows[i]
		if o.ref != r.ref || o.account.id != r.account {
			t.Errorf("%s: outcome %d is of %s, row %s", what, i, o.ref, r.ref)
		}
		switch o.outcome {
		case outcomeWritten:
			var caveats []string
			for _, e := range eventsOf(events, i) {
				caveats = append(caveats, e.caveats...)
			}
			if written[i] == 0 || !slices.Equal(caveatOrder(caveats), o.caveats) {
				t.Errorf("%s: %s is written as %d records, caveats %v of %v", what, r.ref, written[i], o.caveats, caveats)
			}
		case outcomeCombined:
			c, ok := byRef[rowKey{cmp.Or(o.withAccount.id, r.account), o.with}]
			if o.withAccount.id != 0 {
				pays := eventsOf(events, c)
				if !ok || outcomes[c].outcome != outcomeWritten || len(pays) != 1 || pays[0].kind != evTransferOut ||
					pays[0].pair.id != r.account || pays[0].pairRef != r.ref || written[i] != 0 {
					t.Errorf("%s: %s receives the transfer of %s %s, written as %v", what, r.ref, o.withAccount.label(), o.with, pays)
				}
				continue
			}
			if !ok || outcomes[c].outcome != outcomeWritten || outcomes[c].with != r.ref || written[i] != 0 {
				t.Errorf("%s: %s is combined with %q", what, r.ref, o.with)
				continue
			}
			for _, e := range eventsOf(events, c) {
				if e.ref != o.with || e.with != r.ref {
					t.Errorf("%s: %s's sale carries %s with %s", what, o.with, e.ref, e.with)
				}
			}
		case outcomeInBalances, outcomeLeftOut:
			if written[i] != 0 || len(o.caveats) != 0 {
				t.Errorf("%s: %s is %s with %d records and caveats %v", what, r.ref, o.outcome, written[i], o.caveats)
			}
		default:
			t.Errorf("%s: %s has no outcome: %q", what, r.ref, o.outcome)
		}
	}
}

// checkLine requires that row i, of a line's sample, becomes what the line
// says for tool: the event, PP's file and Type, Ghostfolio's type and cash
// profile, a value of 0 where it says so, the account's balances or the
// cash row that carries it, or nothing.
func checkLine(t *testing.T, what string, line toolMap, tool string, outcomes []toolOutcome, events []toolEvent, i int) {
	t.Helper()
	o := outcomes[i]
	columns := line.pp
	if tool == toolGF {
		columns = line.gf
	}
	switch {
	case line.event == "left out":
		if o.outcome != outcomeLeftOut || columns != "left out" {
			t.Errorf("%s is %s", what, o.outcome)
		}
		return
	case line.event == "(with its cash row)":
		c := slices.IndexFunc(outcomes, func(c toolOutcome) bool { return c.ref == o.with })
		if o.outcome != outcomeCombined || c < 0 {
			t.Errorf("%s is %s with %q", what, o.outcome, o.with)
			return
		}
		i = c
	case tool == toolGF && strings.HasPrefix(columns, "account balance"):
		if o.outcome != outcomeInBalances {
			t.Errorf("%s is %s, not in the balances", what, o.outcome)
		}
		return
	}
	records := eventsOf(events, i)
	if len(records) == 0 {
		t.Errorf("%s writes nothing", what)
	}
	for _, e := range records {
		file, typ := e.kind.ppType()
		if tool == toolGF {
			typ = e.kind.gfType()
		}
		switch {
		case line.event == "(with its cash row)":
			// One sale of the cash row, which the line names in parentheses.
			if !strings.Contains(columns, "(one "+typ+")") {
				t.Errorf("%s is combined with a %s", what, typ)
			}
		case tool == toolPP && e.kind.String() != line.event:
			t.Errorf("%s is a %s", what, e.kind)
		case tool == toolPP && (!strings.Contains(columns, typ) || !strings.HasPrefix(columns, file+": ")):
			t.Errorf("%s is PP's %s %s", what, file, typ)
		case tool == toolGF && e.profile == "" && !strings.HasPrefix(columns, typ):
			t.Errorf("%s is Ghostfolio's %s", what, typ)
		case tool == toolGF && e.profile != "" && !strings.HasPrefix(columns, typ) && !strings.Contains(columns, typ+" of the "+e.profile+" profile"):
			t.Errorf("%s is Ghostfolio's %s of the %s profile", what, typ, e.profile)
		case (strings.HasSuffix(columns, "Value 0") || strings.HasSuffix(columns, " at 0")) && e.value != 0:
			t.Errorf("%s is worth %s", what, formatCents(e.value))
		}
	}
}

// The cases' instants are in New York, whatever the machine's zone.
func TestToolCaseInstants(t *testing.T) {
	t.Parallel()
	if got := tcNY("2025-01-21 11:05:12"); !got.Equal(time.Date(2025, 1, 21, 16, 5, 12, 0, time.UTC)) {
		t.Errorf("tcNY = %s", got)
	}
	for _, c := range toolCases() {
		for _, r := range c.in.rows {
			if r.source == "api" && !r.time.IsZero() && r.day != nytime.DateOf(r.time) && r.ref != "T61002504" {
				t.Errorf("%s %s: day %s, time %s", c.name, r.ref, r.day, r.time)
			}
		}
	}
}
