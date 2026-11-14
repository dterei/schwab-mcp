package history

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

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
		return outcomeCombined + " with " + o.with
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
			{{"transfers", "T61001201"}, {"transfers", "T61001202"}},
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
		_, events, _ := mapCase(toolCaseNamed("transfers"), toolPP)
		byRef := map[string]toolEvent{}
		for _, e := range events {
			byRef[e.ref] = e
		}
		// Both sides of a transfer take the paying side's day, time and
		// text, and name each other.
		payer, payee := byRef["T61001201"], byRef["T61001202"]
		paid := tcNY("2026-03-02 09:30:00")
		for _, c := range []struct {
			e                      toolEvent
			kind                   eventKind
			account, pair, pairRef string
		}{
			{payer, evTransferOut, "A2 ...398", "A1 ...135", "T61001202"},
			{payee, evTransferIn, "A1 ...135", "A2 ...398", "T61001201"},
		} {
			e := c.e
			if e.kind != c.kind || e.day != "2026-03-02" || !e.time.Equal(paid) || e.text != "JOURNAL TO ...135" || e.value != 100_000 ||
				e.account.label() != c.account || e.pair.label() != c.pair || e.pairRef != c.pairRef || e.security != nil {
				t.Errorf("%s: %s on %s at %s, %q, %s, %s with %s %s", e.ref, e.kind, e.day, e.time, e.text, formatCents(e.value), e.account.label(), e.pair.label(), e.pairRef)
			}
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
			ppManual, changed                        bool
		}
		for _, c := range []struct {
			key, symbol string
			want        want
		}{
			{keyXYZ, "", want{"XYZ", "XYZ", "", "XYZ CORP COMMON STOCK", "Security (XYZ)", false, false}},
			{keyClass, "", want{"ABC-B", "ABC-B", "", "ABC HOLDINGS CL B", "Security (ABC-B)", false, false}},
			{keyDot, "", want{"", uuid8(toolNamespace, "security ticker:QRS.PRK"), "not_plain", "QRS TRUST PFD SER K", "Security (QRS.PRK)", true, false}},
			{keyGone, "", want{"", uuid8(toolNamespace, "security cusip:ZZQ00K102"), "delisted", "OLDCO INC", "Security (ZZQ00K102)", true, true}},
			{keyRename, "", want{"PQRS", "PQRS", "", "PQR GROUP INC", "Security (PQRS)", false, true}},
			{"", "QQQX", want{"QQQX", "QQQX", "", "", "Security (QQQX)", false, false}},
			{keyNoTick, "", want{"", uuid8(toolNamespace, "security cusip:ZZC99X101"), "no_ticker", "EXAMPLE PRIVATE PLACEMENT UNITS", "Security (ZZC99X101)", true, false}},
			{keyPut, "", want{"XYZ250221P00040000", "fc94e404-4c85-87f0-962e-bb88e7134119", "option", "XYZ 02/21/2025 40.00 P", "XYZ 2025-02-21 40 put (XYZ250221P00040000)", true, false}},
			{keyFileOp, "", want{"XYZ250321C00045000", uuid8(toolNamespace, "security option:XYZ250321C00045000"), "option", "CALL XYZ CORP $45 EXP 03/21/25", "XYZ 2025-03-21 45 call (XYZ250321C00045000)", true, false}},
			{keyFutOpt, "", want{symFutOpt, uuid8(toolNamespace, "security key:"+keyFutOpt), "option", "EXAMPLE INDEX FUTURE PUT 5050 MAR 25", "Security (i:205187341)", true, false}},
			// The first of its symbols in order, not the latest, gives a
			// renamed contract's OCC form, which stays.
			{keyOptRen, "", want{"PQR250620C00030000", uuid8(toolNamespace, "security option:PQR250620C00030000"), "option", "PQR 06/20/2025 30.00 C", "PQR 2025-06-20 30 call (PQR250620C00030000)", true, false}},
			{keyBondA, "", want{"", uuid8(toolNamespace, "security cusip:9128ZZQ15"), "bond", "EXAMPLE TREASURY NOTE 4.25% 08/15/2034", "Bond (9128ZZQ15)", true, false}},
			{keyBondB, "", want{"", uuid8(toolNamespace, "security cusip:ZZT10A109"), "bond", "EXAMPLE CITY REVENUE BOND 5% 06/01/2040", "Bond (ZZT10A109)", true, false}},
			// A bond with a ticker is a bond all the same; an option whose
			// CUSIP the store knows is known by it; a class share spelled
			// two ways has not changed its symbol.
			{keyBondT, "", want{"ZZTB", uuid8(toolNamespace, "security ticker:ZZTB"), "bond", "EXAMPLE BABY BOND 6.5% 2031", "Bond (ZZTB)", true, false}},
			{keyOptCU, "", want{"XYZ250620C00055000", uuid8(toolNamespace, "security cusip:ZZO55C106"), "option", "XYZ 06/20/2025 55.00 C", "XYZ 2025-06-20 55 call (ZZO55C106)", true, false}},
			{keyClass2, "", want{"ABD-B", "ABD-B", "", "ABD CORP CL B", "Security (ABD-B)", false, false}},
		} {
			s := toolSecurityOf(&in, c.key, c.symbol)
			got := want{s.ppTicker(), s.gfSymbol(), s.manual, s.name, s.gfName(), s.ppManual(), s.symbolsChanged()}
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
			{keyBadM, "", 1, false, true, false, "OPTION", "", "XYZ250321C00045000"},
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
		// bond that a row names by its symbol alone counts the legs of that
		// symbol without a key.
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
			{"a leg of the key above 1.25", several("T61002901", "TRADE", "buy", -125_001), leg("T61002901", -125_001), nil, false, true},
			{"a keyless leg of the key's symbol", several("T61002901", "TRADE", "buy", -125_001), keylessLeg("ZZT10A109", -125_001), nil, false, false},
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
		// file's and the API's, is one security of each tool: one MANUAL
		// asset that both its rows are of, -manual naming it; one row of
		// PP's securities; one holding that the snapshot's shares match.
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
		// The keys join by a ticker that both have had, or by a CUSIP, and
		// take every identifier from both: the API's key renamed since, or
		// delisted, which only it learns, the file's knowing neither; a
		// ticker that keeps a dot in Yahoo's form, of which PP takes none; a
		// class share that Schwab and the file spell otherwise; a CUSIP under
		// one key alone; and the descriptions of either source, alike or not.
		// Each tool holds one security still: PP finds every record's by one
		// WKN, ticker or name, so that securities.csv lists it once and PP
		// holds the six shares in it; Ghostfolio's activities are of one
		// asset, which holds them; and the holdings check finds no difference.
		for _, v := range []twoKeys{
			{name: "renamed", api: []string{"JKLM", "JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106", identity: "JKLN", changed: true},
			{name: "renamed, a CUSIP under the API's key", api: []string{"JKLM", "JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", identity: "JKLN", changed: true},
			{
				name: "renamed, no ticker alike, a CUSIP under both", api: []string{"JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106",
				identity: "JKLN", changed: true,
			},
			// -manual names the ticker that the file's key alone had.
			{
				name: "renamed, -manual naming the old ticker", api: []string{"JKLN"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106", fileCUSIP: "ZZJ12K106",
				manual: "JKLM", identity: "JKLN", why: "asked", changed: true,
			},
			{
				name: "delisted, a CUSIP under the API's key", api: []string{"JKLM", "ZZJ12K106"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106",
				identity: "ZZJ12K106", why: "delisted", changed: true,
			},
			{
				name: "delisted, described alike", api: []string{"JKLM", "ZZJ12K106"}, file: []string{"JKLM"}, apiCUSIP: "ZZJ12K106",
				apiName: "JKLM SMALL CAP VALUE ETF", fileName: "JKLM SMALL CAP VALUE ETF", identity: "ZZJ12K106", why: "delisted", changed: true,
			},
			// A file that writes the CUSIP of a security that has gone.
			{
				name: "delisted under the file's key", api: []string{"JKLM"}, file: []string{"JKLM", "ZZJ12K106"}, apiCUSIP: "ZZJ12K106",
				identity: "ZZJ12K106", why: "delisted", changed: true,
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
		} {
			c := v.toolCase()
			for _, tool := range []string{toolPP, toolGF} {
				in, events, _ := mapCase(c, tool)
				if diffs, _ := holdingsCheck(in, events); len(diffs) != 0 {
					t.Errorf("%s %s: differences %+v", v.name, tool, diffs)
				}
				for _, e := range events {
					if s := e.security; s.identity() != v.identity || s.manual != v.why || s.symbolsChanged() != v.changed {
						t.Errorf("%s %s: %s is of %s, MANUAL %q, changed %v; want %s, %q, %v", v.name, tool, e.ref, s.identity(), s.manual, s.symbolsChanged(), v.identity, v.why, v.changed)
					}
				}
				e := caseExport(t, c, tool)
				// The manifest names a security whose symbol changed under each
				// of its keys, its symbol now first: a delisted one's CUSIP.
				var manifest struct {
					SymbolsChanged []toolSymbolsEntry `json:"symbols_changed"`
				}
				if err := json.Unmarshal(e.manifest, &manifest); err != nil {
					t.Fatal(err)
				}
				changed := manifest.SymbolsChanged
				if v.changed != (len(changed) == 2) || slices.ContainsFunc(changed, func(s toolSymbolsEntry) bool { return s.Symbols[0] != v.identity }) {
					t.Errorf("%s %s: symbols changed %+v", v.name, tool, changed)
				}
				s := chainStepFrom(t, e, t.TempDir())
				if tool == toolPP {
					if f := ppFileNamed(t, s.pp, ppSecuritiesFile); f.rows != 1 {
						t.Errorf("%s: PP's securities: %d rows", v.name, f.rows)
					}
					m := newPPModel()
					ppApply(t, m, s)
					ppReconcile(t, m, e.in, e.events, e.outcomes)
					if held := m.shares("Schwab A1 ...135"); len(held) != 1 || slices.Collect(maps.Values(held))[0] != 600_000_000 {
						t.Errorf("%s: PP holds %v", v.name, held)
					}
					continue
				}
				symbols := map[string]bool{}
				for _, a := range e.gf.file.Activities {
					symbols[a.DataSource+" "+a.Symbol] = true
				}
				m := newGFModel()
				if _, err := gfImport(m, s.file); err != nil {
					t.Fatalf("%s: %v", v.name, err)
				}
				if held := m.holdings(); len(symbols) != 1 || len(e.gf.file.AssetProfiles) > 1 || len(held) != 1 || slices.Collect(maps.Values(held))[0].Cmp(big.NewRat(6, 1)) != 0 {
					t.Errorf("%s: Ghostfolio's activities are of %v, %d assets, and it holds %v", v.name, symbols, len(e.gf.file.AssetProfiles), held)
				}
			}
		}
		// Two companies that had one ticker at different times, of two
		// CUSIPs, are two securities, each known by its CUSIP: -manual
		// naming the ticker makes two assets of them, not one.
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
		if !slices.Equal(names, []string{"Security (ZZJ12K106)", "Security (ZZJ99Z109)"}) {
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
		// Five shares held of the seven written, with a row of the symbol
		// without a key, is one difference, of the least key, which names
		// both keys.
		short := toolCaseNamed("two_keys")
		short.in.positions[0].long = centsOf(5_000_000)
		short.in.rows = append(short.in.rows, toolRow{
			ref: "Cf7a8b9c0.2", account: 1, source: "file", posted: "2026-04-01", day: "2026-04-01", kind: "Buy", description: "JKLM SMALL CAP VALUE ETF",
			category: "buy", symbol: "JKLM", quantity: centsOf(1_000_000), price: tcPrice(160), amount: centsOf(-16_000), fees: centsOf(0), gross: centsOf(-16_000),
		})
		for _, tool := range []string{toolPP, toolGF} {
			in, events, _ := mapCase(short, tool)
			diffs, _ := holdingsCheck(in, events)
			if len(diffs) != 1 || diffs[0].key != keyTwoAPI || !slices.Equal(diffs[0].keys, []string{keyTwoAPI, keyTwoFile}) ||
				diffs[0].symbol != "JKLM" || diffs[0].snapshotShares != 5_000_000 || diffs[0].writtenShares != 7_000_000 {
				t.Errorf("%s: differences %+v", tool, diffs)
			}
			e := caseExport(t, short, tool)
			if h := e.res.holdings; len(h) != 1 || h[0].SecurityKey != keyTwoAPI || !slices.Equal(h[0].SecurityKeys, []string{keyTwoAPI, keyTwoFile}) {
				t.Errorf("%s: the manifest's differences %+v", tool, h)
			}
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
			category: "buy", symbol: "JKLM", quantity: centsOf(1_000_000), price: tcPrice(120), amount: centsOf(-12_000), fees: centsOf(0), gross: centsOf(-12_000),
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

	t.Run("groups", func(t *testing.T) {
		t.Parallel()
		// The store's securities that the tools take for one: by a ticker
		// that they have had, in Yahoo's form, when those that had it know
		// one CUSIP at most; by a CUSIP; and options by their OCC form. A
		// ticker that securities of two CUSIPs had joins none of them and
		// names neither; a security that had a ticker of each of two others
		// of different CUSIPs, which would join them, joins neither.
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
		add("s:FILE:XYZ 06/20/2025 55.00 C@2025-07-01", "OPTION", "", "XYZ 06/20/2025 55.00 C")
		add("s:FILE:"+symFutOpt+"@2025-04-01", "OPTION", "", symFutOpt)
		for _, c := range []struct {
			name       string
			keys       []string
			one        bool
			identities []string
			bond       bool
		}{
			// The file's key takes the API's CUSIP.
			{"a ticker, a CUSIP under one key", []string{"x:66000101", "i:1000101"}, true, []string{"GRPA", "GRPA"}, false},
			// GRPB's and GRPC's keys know a CUSIP each, and the file's key
			// had both tickers: it joins neither, and neither ticker, which
			// two of them had, names one.
			{"a file's key between two CUSIPs", []string{"i:1000102", "x:66000102", "i:1000103"}, false, []string{"ZZG10B101", "x:66000102", "ZZG10C109"}, false},
			// A bond with a ticker under the API's key, which the file's
			// key, of no asset type, is too.
			{"a bond with a ticker", []string{"i:1000104", "x:66000104"}, true, []string{"ZZBB", "ZZBB"}, true},
			// GRPE, which two CUSIPs had, names neither of them, nor the
			// group of GRPD, which knows no CUSIP: its first key does.
			{"a group whose ticker two CUSIPs had", []string{"i:1000105", "x:66000105", "i:1000106", "i:1000107"}, false, []string{"i:1000105", "i:1000105", "ZZG10E105", "ZZG10F102"}, false},
			// A file's option and the API's of one contract; and a futures
			// option, which has no OCC form, by its symbol, named by the
			// API's key.
			{"an option of two keys", []string{keyOptCU, "s:FILE:XYZ 06/20/2025 55.00 C@2025-07-01"}, true, []string{"ZZO55C106", "ZZO55C106"}, false},
			{"a futures option of two keys", []string{keyFutOpt, "s:FILE:" + symFutOpt + "@2025-04-01"}, true, []string{keyFutOpt, keyFutOpt}, false},
		} {
			ix := newToolIndex(&in)
			var secs []*toolSecurity
			var identities []string
			for _, k := range c.keys {
				s := ix.security(k, "")
				secs = append(secs, s)
				identities = append(identities, s.identity())
				if s.bond != c.bond {
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
				s.tickers = []string{ticker}
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
			if (k.gfType() == "") != (k == evDeposit || k == evWithdrawal || k == evTransferIn || k == evTransferOut) {
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
// written, whose records carry both refs.
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
	byRef := map[string]int{}
	for i, r := range in.rows {
		byRef[r.ref] = i
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
			c, ok := byRef[o.with]
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
