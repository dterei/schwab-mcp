package history

import (
	"bytes"
	"database/sql"
	"flag"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The cases of the tool exports: one toolInput for each category of the
// mapping and each hard case of the design (3.3 and 4), each named for what
// it holds, with what toolEvents makes of every row of it for each tool.
// TestToolMapping runs them; TestToolCases writes each as an export of each
// tool. Every value is made up: the accounts, refs, securities, CUSIPs and
// amounts.

// A toolCase is a named toolInput, and what toolEvents makes of each of
// its rows.
type toolCase struct {
	name string
	in   toolInput
	// want holds, for every row, what toolEvents makes of it for PP and
	// for Ghostfolio, as describeRow writes it.
	want []toolWant
}

// A toolWant is what toolEvents makes of the row ref for PP and for
// Ghostfolio (describeRow).
type toolWant struct{ ref, pp, gf string }

// toolCases returns every case, made afresh, so that no test sees what
// another changed.
func toolCases() []toolCase {
	return []toolCase{
		toolCaseCash(), toolCaseTransfers(), toolCaseTransferOutside(), toolCaseTrades(), toolCaseMultiLeg(),
		toolCaseTwoRow(), toolCaseDeliveries(), toolCaseIncome(), toolCaseOptions(), toolCaseBonds(),
		toolCaseSecurities(), toolCaseLeftOut(), toolCaseFlags(), toolCaseGFCash(), toolCaseHoldings(),
		toolCaseGFDates(), toolCaseManual(), toolCaseIdle(), toolCaseTwoKeys(),
	}
}

// toolCaseNamed returns the case named name.
func toolCaseNamed(name string) toolCase {
	i := slices.IndexFunc(toolCases(), func(c toolCase) bool { return c.name == name })
	if i < 0 {
		panic("no tool case " + name)
	}
	return toolCases()[i]
}

// update rewrites the golden files of TestToolCases, TestToolChain and
// TestToolExport from what the exports write: go test ./internal/history
// -run 'TestToolCases|TestToolChain|TestToolExport' -history.update. Read
// the diff before committing it.
var update = flag.Bool("history.update", false, "TestToolCases, TestToolChain and TestToolExport: rewrite their golden files under testdata/tools")

// TestToolCases writes each case of toolCases as an export of each tool and
// requires the golden files of testdata/tools/cases (11.2 of the design): a
// folder for each tool, named as -format names it, holds the files that
// every export of the tool writes alike, and a folder for each case, which
// holds the files that the case's export writes. Each export must also read
// and import into a model of its tool as the case means it to (ppCase,
// gfCase).
func TestToolCases(t *testing.T) {
	t.Parallel()
	for _, tool := range []struct {
		name string
		// alike are the files that every export of the tool writes alike,
		// by name: PP's configurations.
		alike map[string][]byte
		// check writes case c as an export of the tool, requires that the
		// folder golden hold what it writes (toolGolden), and checks what
		// the tool makes of it.
		check func(t *testing.T, c toolCase, golden string)
	}{
		{toolPP, ppConfigurations(), ppCase},
		{toolGF, nil, gfCase},
	} {
		t.Run(tool.name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join("testdata", "tools", "cases", tool.name)
			toolGolden(t, dir, tool.alike)
			for _, c := range toolCases() {
				t.Run(c.name, func(t *testing.T) {
					t.Parallel()
					tool.check(t, c, filepath.Join(dir, c.name))
				})
			}
		})
	}
}

// toolGolden requires that the folder golden hold files, each byte for
// byte, and no other file, or with -history.update writes them there and
// removes any other file. It leaves the folders in golden alone: a tool's
// folder holds one for each case.
func toolGolden(t *testing.T, golden string, files map[string][]byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(golden, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(golden)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := files[e.Name()]; ok || e.IsDir() {
			continue
		}
		if !*update {
			t.Errorf("%s holds %s, which the export does not write (-history.update removes it)", golden, e.Name())
		} else if err := os.Remove(filepath.Join(golden, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		path := filepath.Join(golden, name)
		if *update {
			if err := os.WriteFile(path, files[name], 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		switch {
		case err != nil:
			t.Errorf("%v (-history.update writes it)", err)
		case !bytes.Equal(files[name], want):
			t.Errorf("%s differs from what the export writes (-history.update rewrites it):\n%s", path, firstDiff(want, files[name]))
		}
	}
}

// caseExport returns case c exported for tool in full (exportTool), at the
// test stores' clock, by this build's versions: with the -manual symbols
// that the case names for Ghostfolio, which takes them.
func caseExport(t *testing.T, c toolCase, tool string) *ToolExport {
	t.Helper()
	in := c.in
	o := ToolOptions{Tool: tool}
	if tool == toolGF {
		o.Manual = in.manual
	}
	e, err := exportTool(&in, o, nil, testNow, buildVersions(), "")
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// toolFolder returns the files of the folder dir, by name.
func toolFolder(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, e := range entries {
		if files[e.Name()], err = os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// firstDiff says where got, which differs from want, first differs from it:
// the line, with its number, or that only the line ends at their end differ.
func firstDiff(want, got []byte) string {
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
	return "only the line ends at the end differ"
}

// The securities of the cases, by key.
const (
	keyXYZ    = "i:1854092"
	keyABCD   = "i:1000001"
	keyEFGH   = "i:1000002"
	keyPut    = "i:219771034"
	keyCall   = "i:219834765"
	keyPut27  = "i:219900001"
	keyFutOpt = "i:205187341"
	keyFuture = "i:205100001"
	keyBondA  = "c:9128ZZQ15"
	keyBondB  = "s:FILE:ZZT10A109@2025-06-01"
	keyClass  = "i:1000003"
	keyDot    = "i:1000004"
	keyGone   = "i:1000005"
	keyRename = "i:1000006"
	keyBadM   = "i:1000007"
	keyFileOp = "s:FILE:XYZ 03/21/2025 45.00 C@2025-04-01"
	keyNoTick = "i:1000008"
	keyOptRen = "i:1000009"
	keyBondT  = "i:1000010"
	keyOptCU  = "i:1000011"
	keyClass2 = "i:1000012"
	// One security that the store holds under two keys, a History file's
	// and the API's, as it does when nothing joins them (two_keys).
	keyTwoFile = "x:66000017"
	keyTwoAPI  = "i:1000013"
)

// The symbols of the cases' options.
const (
	symPut    = "XYZ   250221P00040000"
	symCall   = "XYZ   260918C00050000"
	symPut27  = "XYZ   270115P00045000"
	symFutOpt = "/ZZH25_P5050:XCME"
	symFuture = "/ZZH25:XCME"
	symBadM   = "XYZ   250321C00045000"
	symFileOp = "XYZ 03/21/2025 45.00 C"
	symOptCU  = "XYZ   250620C00055000"
)

// tcNY returns the instant at which New York's clocks show clock, as in
// "2025-01-21 11:05:12".
func tcNY(clock string) time.Time {
	t, err := time.ParseInLocation(time.DateTime, clock, nytime.NewYork())
	if err != nil {
		panic(err)
	}
	return t
}

// tcPrice returns a price as a column.
func tcPrice(p float64) sql.NullFloat64 { return sql.NullFloat64{Float64: p, Valid: true} }

// tcAccounts returns the cases' accounts: two of the store's fixtures and
// a third.
func tcAccounts() []toolAccount {
	return []toolAccount{
		{id: 1, masked: "...135", nickname: "Individual"},
		{id: 2, masked: "...398", nickname: "Options & Margin"},
		{id: 3, masked: "...783", nickname: "Joint"},
	}
}

// tcSecurities returns the store's securities (Q4) and their names (Q5),
// which every case reads, as a store holds every security whatever an
// export selects. A security's symbols are listed oldest first, and its
// descriptions in the order the store first saw them.
func tcSecurities() ([]toolSecurityRow, []toolName) {
	type security struct {
		row                   toolSecurityRow
		symbols, descriptions []string
	}
	option := func(key, symbol, putCall string, strike float64, expiration nytime.Date, multiplier float64, description string) security {
		s := security{
			row:     toolSecurityRow{key: key, assetType: "OPTION", putCall: putCall, strike: tcPrice(strike), expiration: expiration, underlying: "XYZ"},
			symbols: []string{symbol}, descriptions: []string{description},
		}
		if multiplier != 0 {
			s.row.multiplier = tcPrice(multiplier)
		}
		return s
	}
	// An option whose CUSIP the store knows.
	withCUSIP := option(keyOptCU, symOptCU, "CALL", 55, "2025-06-20", 100, "XYZ 06/20/2025 55.00 C")
	withCUSIP.row.cusip = "ZZO55C106"
	secs := []security{
		{toolSecurityRow{key: keyXYZ, assetType: "EQUITY", typ: "COMMON_STOCK", cusip: "98389B100"}, []string{"XYZ"}, []string{"XYZ CORP COMMON STOCK", "XYZ CORP"}},
		{toolSecurityRow{key: keyABCD, assetType: "EQUITY", cusip: "ZZB123458"}, []string{"ABCD"}, []string{"ABCD INDUSTRIES INC"}},
		{toolSecurityRow{key: keyEFGH, assetType: "COLLECTIVE_INVESTMENT"}, []string{"EFGH"}, []string{"EFGH TOTAL MARKET ETF"}},
		option(keyPut, symPut, "PUT", 40, "2025-02-21", 100, "XYZ 02/21/2025 40.00 P"),
		option(keyCall, symCall, "CALL", 50, "2026-09-18", 100, "XYZ 09/18/2026 50.00 C"),
		option(keyPut27, symPut27, "PUT", 45, "2027-01-15", 100, "XYZ 01/15/2027 45.00 P"),
		option(keyFutOpt, symFutOpt, "PUT", 5050, "2025-03-21", 50, "EXAMPLE INDEX FUTURE PUT 5050 MAR 25"),
		option(keyBadM, symBadM, "CALL", 45, "2025-03-21", 2.5, "XYZ 03/21/2025 45.00 C ADJ"),
		option(keyFileOp, symFileOp, "CALL", 45, "2025-03-21", 0, "CALL XYZ CORP $45 EXP 03/21/25"),
		// A call whose root was renamed with its underlying, PQR to PQRS.
		{
			toolSecurityRow{key: keyOptRen, assetType: "OPTION", putCall: "CALL", strike: tcPrice(30), expiration: "2025-06-20", multiplier: tcPrice(100), underlying: "PQRS"},
			[]string{"PQR   250620C00030000", "PQRS  250620C00030000"},
			[]string{"PQR 06/20/2025 30.00 C"},
		},
		{toolSecurityRow{key: keyFuture, assetType: "FUTURE", multiplier: tcPrice(50)}, []string{symFuture}, []string{"EXAMPLE INDEX FUTURE MAR 25"}},
		{toolSecurityRow{key: keyBondA, assetType: "FIXED_INCOME", cusip: "9128ZZQ15"}, []string{"9128ZZQ15"}, []string{"EXAMPLE TREASURY NOTE 4.25% 08/15/2034"}},
		{toolSecurityRow{key: keyBondB}, []string{"ZZT10A109"}, []string{"EXAMPLE CITY REVENUE BOND 5% 06/01/2040"}},
		{toolSecurityRow{key: keyClass, assetType: "EQUITY"}, []string{"ABC.B"}, []string{"ABC HOLDINGS CL B"}},
		{toolSecurityRow{key: keyDot, assetType: "EQUITY"}, []string{"QRS.PRK"}, []string{"QRS TRUST PFD SER K"}},
		{toolSecurityRow{key: keyGone, assetType: "EQUITY", cusip: "ZZQ00K102"}, []string{"OLDCO", "ZZQ00K102"}, []string{"OLDCO INC"}},
		{toolSecurityRow{key: keyRename, assetType: "EQUITY", cusip: "ZZR00K200"}, []string{"PQR", "PQRS"}, []string{"PQR GROUP INC", "PQRS GROUP INC"}},
		{toolSecurityRow{key: keyNoTick, assetType: "EQUITY", cusip: "ZZC99X101"}, []string{"ZZC99X101"}, []string{"EXAMPLE PRIVATE PLACEMENT UNITS"}},
		// A bond whose symbol is a ticker, and a class share that Schwab
		// has spelled two ways.
		{toolSecurityRow{key: keyBondT, assetType: "FIXED_INCOME"}, []string{"ZZTB"}, []string{"EXAMPLE BABY BOND 6.5% 2031"}},
		withCUSIP,
		{toolSecurityRow{key: keyClass2, assetType: "EQUITY"}, []string{"ABD.B", "ABD/B"}, []string{"ABD CORP CL B"}},
	}
	var rows []toolSecurityRow
	var names []toolName
	for _, s := range secs {
		// Each name is seen for a month, one after another from 2023.
		seen := time.Date(2023, time.March, 1, 15, 0, 0, 0, time.UTC)
		for _, sym := range s.symbols {
			names = append(names, toolName{key: s.row.key, kind: "symbol", value: sym, firstSeen: seen, lastSeen: seen.AddDate(0, 1, 0)})
			s.row.symbol = sym
			seen = seen.AddDate(0, 2, 0)
		}
		seen = time.Date(2023, time.March, 1, 15, 0, 0, 0, time.UTC)
		for _, d := range s.descriptions {
			// A description seen first is often seen last too: the source
			// that saw it first sees the security again later.
			names = append(names, toolName{key: s.row.key, kind: "description", value: d, firstSeen: seen, lastSeen: seen.AddDate(0, 1, len(s.descriptions))})
			s.row.description = d
			seen = seen.AddDate(0, 0, 1)
		}
		rows = append(rows, s.row)
	}
	return rows, names
}

// tcInput completes a case's input as the store's reads would give it: the
// accounts and securities of the cases; to 2026-11-12 unless it says
// another; an API row's day is its time's New York date unless it says
// another; a file row's day is its posted date unless it says another (an
// "as of" date), and its quantity as of a file exported on 2026-11-11;
// fees are 0 unless the row is flagged inexact, and gross is amount plus
// fees; a pair's partner, when the case holds it, is read from it,
// counted.
func tcInput(in toolInput) toolInput {
	in.accounts = cmpOr(in.accounts, tcAccounts())
	if in.securities == nil {
		in.securities, in.names = tcSecurities()
	}
	if in.to == "" {
		in.to = "2026-11-12"
	}
	if in.store == "" {
		in.store = "2023-05-01T14:00:00.000Z"
	}
	for i := range in.rows {
		r := &in.rows[i]
		switch {
		case r.source == "api" && r.day == "" && !r.time.IsZero():
			r.day = nytime.DateOf(r.time)
		case r.source == "file" && r.day == "":
			r.day = r.posted
		}
		if r.source == "file" && r.quantity.Valid {
			// The newest export that holds the row, whose quantities it
			// shows.
			r.quantityAsOf = "2026-11-11"
		}
		if !r.fees.Valid && !slices.Contains(r.flags, "inexact") {
			r.fees = centsOf(0)
		}
		if !r.gross.Valid && r.amount.Valid && r.fees.Valid {
			r.gross = centsOf(r.amount.Int64 + r.fees.Int64)
		}
	}
	for i := range in.rows {
		r := &in.rows[i]
		if r.pairRef == "" || r.partner != (toolPartner{}) {
			continue
		}
		for j := range in.rows {
			if p := &in.rows[j]; p.account == r.pairAccount && p.ref == r.pairRef {
				r.partner = toolPartner{
					day: p.day, time: p.time, amount: p.amount, price: p.price, counted: true,
					category: p.category, kind: p.kind, description: p.description,
				}
			}
		}
	}
	return in
}

// cmpOr returns a, or b when a is empty.
func cmpOr[T any](a, b []T) []T {
	if len(a) == 0 {
		return b
	}
	return a
}

// toolCaseCash holds cash rows: deposits and withdrawals of either sign,
// unpaired journals, a row of 0.00, a sweep pair and an amount off the
// grid.
func toolCaseCash() toolCase {
	return toolCase{
		name: "cash",
		in: tcInput(toolInput{
			rows: []toolRow{
				{ref: "T61001101", account: 1, source: "api", time: tcNY("2025-03-03 10:15:00"), kind: "WIRE_IN", description: "Wire Incoming", amount: centsOf(1_000_000), category: "deposit"},
				{ref: "T61001102", account: 1, source: "api", time: tcNY("2025-03-04 09:00:00"), kind: "ELECTRONIC_FUND", description: "ACH Debit", amount: centsOf(-25_000), category: "deposit", flags: []string{"unexpected_sign"}},
				{ref: "Ce1a2b3c4.3", account: 1, source: "file", posted: "2025-03-10", kind: "MoneyLink Transfer", description: "Tfr EXAMPLE BANK", amount: centsOf(50_000), category: "withdrawal"},
				{ref: "Ce1a2b3c4.4", account: 1, source: "file", posted: "2025-03-11", kind: "Wire Sent", description: "WIRE TO EXAMPLE BANK", amount: centsOf(-150_000), category: "withdrawal"},
				{ref: "T61001105", account: 1, source: "api", time: tcNY("2025-04-01 11:00:00"), kind: "JOURNAL", description: "JOURNAL FRM ...999", amount: centsOf(7500), category: "journal", flags: []string{"unpaired_journal"}},
				{ref: "T61001106", account: 1, source: "api", time: tcNY("2025-04-02 11:00:00"), kind: "JOURNAL", description: "JOURNAL TO ...999", amount: centsOf(-2500), category: "journal", flags: []string{"unpaired_journal"}},
				{ref: "T61001107", account: 1, source: "api", time: tcNY("2025-04-03 12:00:00"), kind: "CASH_RECEIPT", description: "Funds Received", amount: centsOf(0), category: "deposit"},
				{ref: "T61001108", account: 1, source: "api", time: tcNY("2025-05-01 08:00:00"), kind: "ELECTRONIC_FUND", description: "BANK SWEEP TO BROKERAGE", amount: centsOf(30_000), category: "deposit", pairKind: "sweep", pairRef: "T61001109", pairAccount: 1},
				{ref: "T61001109", account: 1, source: "api", time: tcNY("2025-05-01 16:00:00"), kind: "ELECTRONIC_FUND", description: "BANK SWEEP FROM BROKERAGE", amount: centsOf(-30_000), category: "other", pairKind: "sweep", pairRef: "T61001108", pairAccount: 1},
				{ref: "Ce1a2b3c4.9", account: 1, source: "file", posted: "2025-06-02", kind: "Funds Received", description: "FUNDS RECEIVED", category: "deposit", flags: []string{"inexact"}},
			},
			snapshots: []toolSnapshot{{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(1_234_567)}},
			waiting:   []toolWaiting{{ref: "Ce1a2b3c4.12", account: 1, day: "2025-06-03", kind: "Bank Interest", amount: centsOf(37), why: "held_out"}},
			coverage:  []ExportCoverage{{AccountID: 1, Account: "...135", Source: "api_file", From: "2023-06-13", To: "2026-11-10"}},
			uncounted: []toolUncounted{{ref: "Ce1a2b3c4.5", account: 1, why: "linked", matchedWith: "T61001101"}},
		}),
		want: []toolWant{
			{"T61001101", "Deposit 10000.00", "in_balances"},
			{"T61001102", "Withdrawal 250.00", "in_balances"},
			{"Ce1a2b3c4.3", "Deposit 500.00", "in_balances"},
			{"Ce1a2b3c4.4", "Withdrawal 1500.00", "in_balances"},
			{"T61001105", "Deposit 75.00", "in_balances"},
			{"T61001106", "Withdrawal 25.00", "in_balances"},
			{"T61001107", "left_out zero_amount", "left_out zero_amount"},
			{"T61001108", "left_out sweep_pair", "left_out sweep_pair"},
			{"T61001109", "left_out sweep_pair", "left_out sweep_pair"},
			{"Ce1a2b3c4.9", "left_out no_amount", "left_out no_amount"},
		},
	}
}

// toolCaseTransfers holds journals of cash between two accounts that the
// export selects: a transfer, whose receiving side is dated a day after
// the paying one; a pair whose amounts differ; one whose partner is not
// counted; and a pair with a side that you booked, payer and receiver. And
// a pair within one account, which you paired by hand. One account's
// balance is negative.
func toolCaseTransfers() toolCase {
	journal := func(ref string, account int64, clock, description, category string, amount int64, pair string, pairAccount int64) toolRow {
		return toolRow{
			ref: ref, account: account, source: "api", time: tcNY(clock), kind: "JOURNAL", description: description,
			amount: centsOf(amount), category: category, pairKind: "journal", pairRef: pair, pairAccount: pairAccount,
		}
	}
	notCounted := journal("T61001205", 1, "2026-05-04 10:00:00", "JOURNAL TO ...398", "journal", -20_000, "T61001206", 2)
	notCounted.partner = toolPartner{day: "2026-05-04", time: tcNY("2026-05-04 10:00:03"), amount: centsOf(20_000), category: "journal", kind: "JOURNAL", description: "JOURNAL FRM ...135"}
	return toolCase{
		name: "transfers",
		in: tcInput(toolInput{
			rows: []toolRow{
				journal("T61001201", 2, "2026-03-02 09:30:00", "JOURNAL TO ...135", "journal", -100_000, "T61001202", 1),
				journal("T61001202", 1, "2026-03-03 08:00:00", "JOURNAL FRM ...398", "journal", 100_000, "T61001201", 2),
				journal("T61001203", 1, "2026-04-01 10:00:00", "JOURNAL TO ...398", "journal", -30_000, "T61001204", 2),
				journal("T61001204", 2, "2026-04-01 10:00:05", "JOURNAL FRM ...135", "journal", 29_900, "T61001203", 1),
				notCounted,
				journal("T61001207", 1, "2026-06-01 10:00:00", "JOURNAL TO ...398", "withdrawal", -15_000, "T61001208", 2),
				journal("T61001208", 2, "2026-06-01 10:00:07", "JOURNAL FRM ...135", "journal", 15_000, "T61001207", 1),
				journal("T61001209", 2, "2026-07-01 10:00:00", "JOURNAL TO ...135", "journal", -8000, "T61001210", 1),
				journal("T61001210", 1, "2026-07-01 10:00:09", "JOURNAL FRM ...398", "deposit", 8000, "T61001209", 2),
				journal("T61001211", 1, "2026-08-03 10:00:00", "JOURNAL TO ...135", "journal", -10_000, "T61001212", 1),
				journal("T61001212", 1, "2026-08-03 10:00:05", "JOURNAL FRM ...135", "journal", 10_000, "T61001211", 1),
			},
			snapshots: []toolSnapshot{
				{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(500_000)},
				{account: 2, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:01"), cash: centsOf(-1234)},
			},
		}),
		want: []toolWant{
			{"T61001201", "Transfer (Outbound) 1000.00", "in_balances"},
			{"T61001202", "Transfer (Inbound) 1000.00", "in_balances"},
			{"T61001203", "Withdrawal 300.00 [pair_amounts_differ]", "in_balances"},
			{"T61001204", "Deposit 299.00 [pair_amounts_differ]", "in_balances"},
			{"T61001205", "Withdrawal 200.00", "in_balances"},
			{"T61001207", "Withdrawal 150.00", "in_balances"},
			{"T61001208", "Deposit 150.00 [pair_not_journal]", "in_balances"},
			{"T61001209", "Withdrawal 80.00 [pair_not_journal]", "in_balances"},
			{"T61001210", "Deposit 80.00", "in_balances"},
			{"T61001211", "Withdrawal 100.00", "in_balances"},
			{"T61001212", "Deposit 100.00", "in_balances"},
		},
	}
}

// toolCaseTransferOutside is an export of one account (-account A1) that
// holds one side of a transfer: the other account is not in it.
func toolCaseTransferOutside() toolCase {
	return toolCase{
		name: "transfer_outside",
		in: tcInput(toolInput{
			account: 1,
			rows: []toolRow{{
				ref: "T61001202", account: 1, source: "api", time: tcNY("2026-03-03 08:00:00"), kind: "JOURNAL", description: "JOURNAL FRM ...398",
				amount: centsOf(100_000), category: "journal", pairKind: "journal", pairRef: "T61001201", pairAccount: 2,
				partner: toolPartner{
					day: "2026-03-02", time: tcNY("2026-03-02 09:30:00"), amount: centsOf(-100_000), counted: true,
					category: "journal", kind: "JOURNAL", description: "JOURNAL TO ...135",
				},
			}},
			snapshots: []toolSnapshot{{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(500_000)}},
		}),
		want: []toolWant{{"T61001202", "Deposit 1000.00 [pair_outside]", "in_balances"}},
	}
}

// toolCaseTrades holds trades: a buy and a sale with fees, a Buy and its
// Cancel Buy, a Sell and its Cancel Sell, a sale whose fees exceed its
// proceeds, a rebate, fees off the grid, a cancel with the original's sign,
// a trade without a security and a Cash In Lieu without a quantity.
func toolCaseTrades() toolCase {
	trade := func(ref, clock, category string, key, symbol string, quantity int64, price float64, amount, fees int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: "TRADE", category: category, securityKey: key, symbol: symbol,
			quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount), fees: centsOf(fees),
		}
	}
	file := func(ref string, posted nytime.Date, action, category string, quantity int64, price float64, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "XYZ CORP", category: category,
			securityKey: keyXYZ, symbol: "XYZ", quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount),
		}
	}
	buy, cancel := file("Cf0e1d2c3.2", "2025-04-01", "Buy", "buy", 2_000_000, 50, -10_000), file("Cf0e1d2c3.3", "2025-04-01", "Cancel Buy", "buy", -2_000_000, 50, 10_000)
	buy.pairKind, buy.pairRef, buy.pairAccount = "reversal", cancel.ref, 1
	cancel.pairKind, cancel.pairRef, cancel.pairAccount = "reversal", buy.ref, 1
	sell, uncancel := file("Cf0e1d2c3.4", "2025-04-02", "Sell", "sell", -1_000_000, 55, 5500), file("Cf0e1d2c3.5", "2025-04-02", "Cancel Sell", "sell", 1_000_000, 55, -5500)
	sell.pairKind, sell.pairRef, sell.pairAccount = "reversal", uncancel.ref, 1
	uncancel.pairKind, uncancel.pairRef, uncancel.pairAccount = "reversal", sell.ref, 1
	inexact := toolRow{
		ref: "Cf0e1d2c3.8", account: 1, source: "file", posted: "2025-06-02", kind: "Buy", description: "ABCD INDUSTRIES INC", category: "buy",
		securityKey: keyABCD, symbol: "ABCD", quantity: centsOf(3_000_000), price: tcPrice(100.5), amount: centsOf(-30_150), flags: []string{"inexact"},
	}
	wrongSign := file("Cf0e1d2c3.9", "2025-06-03", "Cancel Buy", "buy", -1_000_000, 50, -5000)
	wrongSign.flags = []string{"reversal_signs", "unpaired_reversal"}
	cashInLieu := file("Cf0e1d2c3.11", "2025-07-02", "Cash In Lieu", "sell", 0, 0, 321)
	cashInLieu.quantity, cashInLieu.price = sql.NullInt64{}, sql.NullFloat64{}
	noSecurity := trade("T61001310", "2025-07-01 10:00:00", "buy", "", "", 1_000_000, 10, -1000, 0)
	noSecurity.flags = []string{"no_security_leg"}
	return toolCase{
		name: "trades",
		in: tcInput(toolInput{rows: []toolRow{
			trade("T61001301", "2025-02-03 10:15:00", "buy", keyXYZ, "XYZ", 10_000_000, 50, -50_065, 65),
			trade("T61001302", "2025-03-03 14:00:00", "sell", keyXYZ, "XYZ", -4_000_000, 55, 21_997, 3),
			buy, cancel, sell, uncancel,
			trade("T61001306", "2025-05-05 10:00:00", "sell", keyEFGH, "EFGH", -10_000, 5, -5, 10),
			trade("T61001307", "2025-05-06 10:00:00", "buy", keyEFGH, "EFGH", 2_000_000, 30, -5950, -50),
			inexact, wrongSign, noSecurity, cashInLieu,
		}}),
		want: []toolWant{
			{"T61001301", "Buy 500.65 fees 0.65 shares 10", "BUY 500.65 fees 0.65 shares 10"},
			{"T61001302", "Sell 219.97 fees 0.03 shares 4", "SELL 219.97 fees 0.03 shares 4"},
			{"Cf0e1d2c3.2", "Buy 100.00 shares 2", "BUY 100.00 shares 2"},
			{"Cf0e1d2c3.3", "Sell 100.00 shares 2", "SELL 100.00 shares 2"},
			{"Cf0e1d2c3.4", "Sell 55.00 shares 1", "SELL 55.00 shares 1"},
			{"Cf0e1d2c3.5", "Buy 55.00 shares 1", "BUY 55.00 shares 1"},
			{"T61001306", "Sell 0.05 shares 0.01; Fees 0.10 [fees_apart]", "SELL 0.05 fees 0.10 shares 0.01"},
			{"T61001307", "Buy 59.50 shares 2", "BUY 59.50 shares 2"},
			{"Cf0e1d2c3.8", "Buy 301.50 shares 3 [fees_unknown]", "BUY 301.50 shares 3 [fees_unknown]"},
			{"Cf0e1d2c3.9", "left_out signs_disagree", "left_out signs_disagree"},
			{"T61001310", "left_out no_security", "left_out no_security"},
			{"Cf0e1d2c3.11", "left_out no_quantity", "left_out no_quantity"},
		},
	}
}

// toolLegs returns the security legs of an option spread of row ref of
// account 2: leg 4 buys a put and leg 5 sells a call, a contract each,
// each with its cost in cents; a cost of 1 stands for none (off the grid).
func toolLegs(ref string, putCost, callCost int64, putPrice, callPrice float64) []toolLeg {
	legs := []toolLeg{
		{account: 2, ref: ref, leg: 4, symbol: symPut, securityKey: keyPut, quantity: centsOf(1_000_000), cost: centsOf(putCost), price: tcPrice(putPrice)},
		{account: 2, ref: ref, leg: 5, symbol: symCall, securityKey: keyCall, quantity: centsOf(-1_000_000), cost: centsOf(callCost), price: tcPrice(callPrice)},
	}
	for i := range legs {
		if legs[i].cost.Int64 == 1 {
			legs[i].cost = sql.NullInt64{}
		}
	}
	return legs
}

// toolCaseMultiLeg holds trades of two option legs: one whose fees go to
// its larger leg, one whose legs do not add up, one with a leg's cost off
// the grid, one with fees off the grid and one whose legs cost alike; and a
// trade of one leg whose quantity is off the grid.
func toolCaseMultiLeg() toolCase {
	spread := func(ref, clock string, amount int64, fees sql.NullInt64, flags ...string) toolRow {
		return toolRow{
			ref: ref, account: 2, source: "api", time: tcNY(clock), kind: "TRADE", category: "buy",
			amount: centsOf(amount), fees: fees, flags: append([]string{"mixed_legs"}, flags...),
		}
	}
	var legs []toolLeg
	legs = append(legs, toolLegs("T61001401", -15_000, 8000, 1.5, 0.8)...)
	legs = append(legs, toolLegs("T61001402", -10_000, 4000, 1, 0.4)...)
	legs = append(legs, toolLegs("T61001403", 1, 4000, 1, 0.4)...)
	legs = append(legs, toolLegs("T61001404", -20_000, 12_000, 2, 1.2)...)
	legs = append(legs, toolLegs("T61001405", -10_000, 10_000, 1, 1)...)
	legs = append(legs, toolLeg{account: 2, ref: "T61001406", leg: 4, symbol: symPut, securityKey: keyPut, cost: centsOf(-10_000), price: tcPrice(1)})
	return toolCase{
		name: "multi_leg",
		in: tcInput(toolInput{
			rows: []toolRow{
				spread("T61001401", "2025-01-10 10:00:00", -7132, centsOf(132)),
				spread("T61001402", "2025-01-11 10:00:00", -5000, centsOf(100), "legs_mismatch"),
				spread("T61001403", "2025-01-12 10:00:00", -5100, centsOf(100), "inexact"),
				spread("T61001404", "2025-01-13 10:00:00", -8137, sql.NullInt64{}, "inexact"),
				spread("T61001405", "2025-01-14 10:00:00", -130, centsOf(130)),
				spread("T61001406", "2025-01-15 10:00:00", -10_000, centsOf(0), "inexact"),
			},
			legs: legs,
		}),
		want: []toolWant{
			{"T61001401", "Buy 151.32 fees 1.32 shares 100 leg 4; Sell 80.00 shares 100 leg 5", "BUY 151.32 fees 1.32 shares 100 leg 4; SELL 80.00 shares 100 leg 5"},
			{"T61001402", "left_out legs_mismatch", "left_out legs_mismatch"},
			{"T61001403", "left_out no_amount", "left_out no_amount"},
			{"T61001404", "Buy 201.37 shares 100 leg 4; Sell 120.00 shares 100 leg 5 [fees_unknown]", "BUY 201.37 shares 100 leg 4; SELL 120.00 shares 100 leg 5 [fees_unknown]"},
			{"T61001405", "Buy 101.30 fees 1.30 shares 100 leg 4; Sell 100.00 shares 100 leg 5", "BUY 101.30 fees 1.30 shares 100 leg 4; SELL 100.00 shares 100 leg 5"},
			{"T61001406", "left_out no_quantity", "left_out no_quantity"},
		},
	}
}

// toolCaseTwoRow holds two-row cash events: a cash merger with its shares
// row, a cash row with none and one with two, two events whose sale is
// left out (no security; a future), whose shares rows follow, and a cash
// row whose quantity is 0 rather than none; and rows that are no pair: two
// cash rows for one shares row, two rows without a key of two symbols, two
// rows of the shapes that no flag makes one event, a cash row of 0.00, and
// a shares row that moved cash.
func toolCaseTwoRow() toolCase {
	row := func(ref string, posted nytime.Date, action, category, key, symbol string, quantity sql.NullInt64, amount int64) toolRow {
		r := toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "MERGER", category: category,
			securityKey: key, symbol: symbol, quantity: quantity, amount: centsOf(amount),
		}
		if action != "Buy" {
			r.flags = []string{"two_row_event"}
		}
		return r
	}
	none := sql.NullInt64{}
	adj := row("C0a1b2c3d.3", "2025-08-01", "Cash Merger Adj", "other", keyABCD, "ABCD", centsOf(-25_000_000), 0)
	adj.flags = append(adj.flags, "restated_quantity")
	buy := row("C0a1b2c3d.1", "2025-01-15", "Buy", "buy", keyABCD, "ABCD", centsOf(25_000_000), -100_000)
	buy.price = tcPrice(40)
	// Rows of the two shapes, but not flagged as the two rows of one event.
	unflaggedCash := row("C0a1b2c3d.19", "2025-12-04", "Cash In Lieu", "sell", keyXYZ, "XYZ", none, 300)
	unflaggedShares := row("C0a1b2c3d.20", "2025-12-04", "Reverse Split", "other", keyXYZ, "XYZ", centsOf(-1_000_000), 0)
	unflaggedCash.flags, unflaggedShares.flags = nil, nil
	return toolCase{
		name: "two_row",
		in: tcInput(toolInput{rows: []toolRow{
			buy,
			row("C0a1b2c3d.2", "2025-08-01", "Cash Merger", "sell", keyABCD, "ABCD", none, 123_450),
			adj,
			row("C0a1b2c3d.4", "2025-08-04", "Cash Merger", "sell", keyEFGH, "EFGH", none, 5000),
			row("C0a1b2c3d.5", "2025-09-02", "Cash Merger", "sell", keyXYZ, "XYZ", none, 7700),
			row("C0a1b2c3d.6", "2025-09-02", "Cash Merger Adj", "other", keyXYZ, "XYZ", centsOf(-1_000_000), 0),
			row("C0a1b2c3d.7", "2025-09-02", "Cash Merger Adj", "other", keyXYZ, "XYZ", centsOf(-1_000_000), 0),
			row("C0a1b2c3d.8", "2025-10-01", "Cash Merger", "sell", "", "", none, 900),
			row("C0a1b2c3d.9", "2025-10-01", "Cash Merger Adj", "other", "", "", centsOf(-3_000_000), 0),
			row("C0a1b2c3d.10", "2025-11-03", "Cash Merger", "sell", keyFuture, symFuture, none, 4000),
			row("C0a1b2c3d.11", "2025-11-03", "Cash Merger Adj", "other", keyFuture, symFuture, centsOf(-1_000_000), 0),
			row("C0a1b2c3d.12", "2025-12-01", "Cash Merger", "sell", keyEFGH, "EFGH", centsOf(0), 6200),
			row("C0a1b2c3d.13", "2025-12-01", "Cash Merger Adj", "other", keyEFGH, "EFGH", centsOf(-2_000_000), 0),
			row("C0a1b2c3d.14", "2025-12-02", "Cash Merger", "sell", keyABCD, "ABCD", none, 1000),
			row("C0a1b2c3d.15", "2025-12-02", "Cash Merger", "sell", keyABCD, "ABCD", none, 2000),
			row("C0a1b2c3d.16", "2025-12-02", "Cash Merger Adj", "other", keyABCD, "ABCD", centsOf(-1_000_000), 0),
			row("C0a1b2c3d.17", "2025-12-03", "Cash Merger", "sell", "", "AAAA", none, 1000),
			row("C0a1b2c3d.18", "2025-12-03", "Cash Merger Adj", "other", "", "BBBB", centsOf(-1_000_000), 0),
			unflaggedCash, unflaggedShares,
			row("C0a1b2c3d.21", "2025-12-05", "Cash Merger", "sell", keyXYZ, "XYZ", none, 0),
			row("C0a1b2c3d.22", "2025-12-05", "Cash Merger Adj", "other", keyXYZ, "XYZ", centsOf(-1_000_000), 0),
			row("C0a1b2c3d.23", "2025-12-08", "Cash Merger", "sell", keyXYZ, "XYZ", none, 700),
			row("C0a1b2c3d.24", "2025-12-08", "Cash Merger Adj", "other", keyXYZ, "XYZ", centsOf(-1_000_000), 15),
		}}),
		want: []toolWant{
			{"C0a1b2c3d.1", "Buy 1000.00 shares 25", "BUY 1000.00 shares 25"},
			{"C0a1b2c3d.2", "Sell 1234.50 shares 25", "SELL 1234.50 shares 25"},
			{"C0a1b2c3d.3", "combined with C0a1b2c3d.2", "combined with C0a1b2c3d.2"},
			{"C0a1b2c3d.4", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.5", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.6", "left_out other", "left_out other"},
			{"C0a1b2c3d.7", "left_out other", "left_out other"},
			{"C0a1b2c3d.8", "left_out no_security", "left_out no_security"},
			{"C0a1b2c3d.9", "left_out no_security with C0a1b2c3d.8", "left_out no_security with C0a1b2c3d.8"},
			{"C0a1b2c3d.10", "left_out future", "left_out future"},
			{"C0a1b2c3d.11", "left_out future with C0a1b2c3d.10", "left_out future with C0a1b2c3d.10"},
			{"C0a1b2c3d.12", "Sell 62.00 shares 2", "SELL 62.00 shares 2"},
			{"C0a1b2c3d.13", "combined with C0a1b2c3d.12", "combined with C0a1b2c3d.12"},
			{"C0a1b2c3d.14", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.15", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.16", "left_out other", "left_out other"},
			{"C0a1b2c3d.17", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.18", "left_out other", "left_out other"},
			{"C0a1b2c3d.19", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.20", "left_out other", "left_out other"},
			{"C0a1b2c3d.21", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.22", "left_out other", "left_out other"},
			{"C0a1b2c3d.23", "left_out no_quantity", "left_out no_quantity"},
			{"C0a1b2c3d.24", "left_out other", "left_out other"},
		},
	}
}

// toolCaseDeliveries holds moves of shares in kind: a journal pair whose
// sides have two prices, one whose delivering side has none, and one with
// none, valued on the day it was delivered; deliveries in and out of either
// category and sign; a bond's, priced per 100 of face; one priced at 0.00,
// valued from a snapshot; one with no price at all, one that moved cash
// too, one whose cash is off the grid, an option's, a journal of two
// securities, and three left out. The last journal pair's sides are of one
// second, a SELL in one account and a BUY in the other, which Ghostfolio
// must not get at one instant.
func toolCaseDeliveries() toolCase {
	journal := func(ref string, account int64, clock, key, symbol string, quantity int64, price sql.NullFloat64, pair string, pairAccount int64) toolRow {
		return toolRow{
			ref: ref, account: account, source: "api", time: tcNY(clock), kind: "JOURNAL", description: "JOURNALED SHARES",
			amount: centsOf(0), category: "journal", securityKey: key, symbol: symbol, quantity: centsOf(quantity), price: price,
			pairKind: "journal", pairRef: pair, pairAccount: pairAccount,
		}
	}
	file := func(ref string, posted nytime.Date, action, category, key, symbol string, quantity int64, price float64) toolRow {
		r := toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "SHARES", amount: centsOf(0),
			category: category, securityKey: key, symbol: symbol, quantity: centsOf(quantity),
		}
		if price != 0 {
			r.price = tcPrice(price)
		}
		return r
	}
	vest := file("Cb0c1d2e3.4", "2026-07-27", "Stock Plan Activity", "transfer_in", keyXYZ, "XYZ", 3_000_000, 0)
	vest.day, vest.price = "2026-07-24", tcPrice(0)
	noAmount := file("Cb0c1d2e3.10", "2026-08-16", "Journaled Shares", "transfer_in", keyEFGH, "EFGH", 1_000_000, 30)
	noAmount.amount, noAmount.flags = sql.NullInt64{}, []string{"inexact"}
	noQuantity := file("Cb0c1d2e3.8", "2026-08-14", "Journaled Shares", "transfer_in", keyEFGH, "EFGH", 0, 30)
	noQuantity.quantity, noQuantity.flags = sql.NullInt64{}, []string{"inexact"}
	withCash := toolRow{
		ref: "T61001503", account: 1, source: "api", time: tcNY("2026-08-10 10:00:00"), kind: "JOURNAL", description: "SECURITY TRANSFER",
		amount: centsOf(-2500), category: "transfer_out", securityKey: keyXYZ, symbol: "XYZ", quantity: centsOf(-1_000_000), price: tcPrice(50),
	}
	option := toolRow{
		ref: "T61001504", account: 2, source: "api", time: tcNY("2026-08-11 10:00:00"), kind: "RECEIVE_AND_DELIVER", description: "TRANSFER OF SECURITY",
		amount: centsOf(0), category: "transfer_in", securityKey: keyPut27, symbol: symPut27, quantity: centsOf(2_000_000), price: tcPrice(1.55),
	}
	twoSecurities := toolRow{
		ref: "T61001505", account: 2, source: "api", time: tcNY("2026-08-20 10:00:00"), kind: "JOURNAL", description: "JOURNALED SHARES",
		amount: centsOf(0), category: "journal",
	}
	return toolCase{
		name: "deliveries",
		in: tcInput(toolInput{
			rows: []toolRow{
				journal("T61001501", 2, "2026-05-04 10:00:00", keyXYZ, "XYZ", -5_000_000, tcPrice(52), "T61001502", 1),
				journal("T61001502", 1, "2026-05-05 09:00:00", keyXYZ, "XYZ", 5_000_000, tcPrice(51), "T61001501", 2),
				file("Cb0c1d2e3.1", "2026-06-01", "Journaled Shares", "transfer_in", keyEFGH, "EFGH", 10_000_000, 31.25),
				file("Cb0c1d2e3.2", "2026-06-02", "Security Transfer", "transfer_out", keyEFGH, "EFGH", -4_000_000, 30),
				file("Cb0c1d2e3.3", "2026-06-03", "Journaled Shares", "transfer_in", keyBondA, "9128ZZQ15", 10_000_000_000, 99.5),
				vest,
				journal("T61001508", 2, "2026-07-24 10:00:00", keyXYZ, "XYZ", -2_000_000, sql.NullFloat64{}, "T61001509", 1),
				journal("T61001509", 1, "2026-07-27 10:00:00", keyXYZ, "XYZ", 2_000_000, sql.NullFloat64{}, "T61001508", 2),
				file("Cb0c1d2e3.5", "2026-08-03", "Received", "transfer_in", keyEFGH, "EFGH", 1_000_000, 0),
				withCash, option,
				file("Cb0c1d2e3.6", "2026-08-12", "Journaled Shares", "transfer_in", keyEFGH, "EFGH", -1_000_000, 30),
				file("Cb0c1d2e3.7", "2026-08-13", "Journaled Shares", "transfer_out", keyEFGH, "EFGH", 1_000_000, 30),
				noQuantity,
				file("Cb0c1d2e3.9", "2026-08-15", "Journaled Shares", "transfer_in", "", "", 1_000_000, 30),
				noAmount,
				file("Cb0c1d2e3.11", "2026-08-17", "Journaled Shares", "transfer_in", keyFuture, symFuture, 1_000_000, 5050),
				twoSecurities,
				journal("T61001506", 1, "2026-09-01 10:00:00", keyEFGH, "EFGH", -1_000_000, sql.NullFloat64{}, "T61001507", 2),
				journal("T61001507", 2, "2026-09-01 10:00:00", keyEFGH, "EFGH", 1_000_000, tcPrice(30.5), "T61001506", 1),
			},
			legs: []toolLeg{
				{account: 2, ref: "T61001505", leg: 1, symbol: "XYZ", securityKey: keyXYZ, quantity: centsOf(-2_000_000), price: tcPrice(52)},
				{account: 2, ref: "T61001505", leg: 2, symbol: "EFGH", securityKey: keyEFGH, quantity: centsOf(-3_000_000), price: tcPrice(31)},
			},
			positions: []toolPosition{
				{key: keyXYZ, account: 2, marketDate: "2026-07-24", fetchedAt: tcNY("2026-07-24 18:00:00"), long: centsOf(5_000_000), short: centsOf(0), marketValue: tcPrice(262.5)},
				{key: keyXYZ, account: 1, marketDate: "2026-07-24", fetchedAt: tcNY("2026-07-24 17:00:00"), long: centsOf(1_000_000), short: centsOf(0), marketValue: tcPrice(50)},
			},
		}),
		want: []toolWant{
			{"T61001501", "Delivery (Outbound) 260.00 shares 5", "SELL 260.00 shares 5"},
			{"T61001502", "Delivery (Inbound) 260.00 shares 5", "BUY 260.00 shares 5"},
			{"Cb0c1d2e3.1", "Delivery (Inbound) 312.50 shares 10", "BUY 312.50 shares 10"},
			{"Cb0c1d2e3.2", "Delivery (Outbound) 120.00 shares 4", "SELL 120.00 shares 4"},
			{"Cb0c1d2e3.3", "Delivery (Inbound) 9950.00 shares 10000", "BUY 9950.00 shares 10000"},
			{"T61001508", "Delivery (Outbound) 105.00 shares 2 [value_from_snapshot]", "SELL 105.00 shares 2 [value_from_snapshot]"},
			{"Cb0c1d2e3.4", "Delivery (Inbound) 157.50 shares 3 [value_from_snapshot]", "BUY 157.50 shares 3 [value_from_snapshot]"},
			{"T61001509", "Delivery (Inbound) 105.00 shares 2 [value_from_snapshot]", "BUY 105.00 shares 2 [value_from_snapshot]"},
			{"Cb0c1d2e3.5", "Delivery (Inbound) 0.00 shares 1 [value_unknown]", "BUY 0.00 shares 1 [value_unknown]"},
			{"T61001503", "Delivery (Outbound) 50.00 shares 1 [cash_not_written]", "SELL 50.00 shares 1 [cash_not_written]"},
			{"T61001504", "Delivery (Inbound) 310.00 shares 200", "BUY 310.00 shares 200"},
			{"Cb0c1d2e3.6", "Delivery (Outbound) 30.00 shares 1", "SELL 30.00 shares 1"},
			{"Cb0c1d2e3.7", "Delivery (Inbound) 30.00 shares 1", "BUY 30.00 shares 1"},
			{"Cb0c1d2e3.8", "left_out no_quantity", "left_out no_quantity"},
			{"Cb0c1d2e3.9", "left_out no_security", "left_out no_security"},
			{"Cb0c1d2e3.10", "Delivery (Inbound) 30.00 shares 1", "BUY 30.00 shares 1"},
			{"Cb0c1d2e3.11", "left_out future", "left_out future"},
			{"T61001505", "Delivery (Outbound) 104.00 shares 2 leg 1; Delivery (Outbound) 93.00 shares 3 leg 2", "SELL 104.00 shares 2 leg 1; SELL 93.00 shares 3 leg 2"},
			{"T61001506", "Delivery (Outbound) 30.50 shares 1", "SELL 30.50 shares 1"},
			{"T61001507", "Delivery (Inbound) 30.50 shares 1", "BUY 30.50 shares 1"},
		},
	}
}

// toolCaseIncome holds income, fees and taxes: dividends, one with fees,
// one with fees below 0, one negative, one a return of capital, one with
// fees off the grid and one without a security; interest with and without
// a security, of 0.00, and charged, with and without a security (a bond's
// accrued interest, paid); a fee and its refund; a withholding and a
// reclaim. Each dividend is of a security bought before it.
func toolCaseIncome() toolCase {
	api := func(ref, clock, kind, description, category, key, symbol string, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: kind, description: description, category: category,
			securityKey: key, symbol: symbol, amount: centsOf(amount),
		}
	}
	file := func(ref string, posted nytime.Date, action, category, key, symbol string, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "INCOME", category: category,
			securityKey: key, symbol: symbol, amount: centsOf(amount),
		}
	}
	buy := api("T61001601", "2024-01-10 10:00:00", "TRADE", "", "buy", keyEFGH, "EFGH", -30_000)
	buy.quantity, buy.price = centsOf(10_000_000), tcPrice(30)
	withFees := file("Cc0d1e2f3.1", "2024-05-15", "Qualified Dividend", "dividend", keyEFGH, "EFGH", 950)
	withFees.fees = centsOf(50)
	capital := file("Cc0d1e2f3.3", "2024-06-15", "Return Of Capital", "dividend", keyEFGH, "EFGH", 500)
	capital.flags = []string{"return_of_capital"}
	bond := file("Cc0d1e2f3.4", "2024-07-01", "Buy", "buy", keyBondA, "9128ZZQ15", -99_000)
	bond.quantity, bond.price = centsOf(1_000_000_000), tcPrice(99)
	inexact := file("Cc0d1e2f3.10", "2024-11-01", "Cash Dividend", "dividend", keyEFGH, "EFGH", 400)
	inexact.flags = []string{"inexact"}
	rebate := file("Cc0d1e2f3.13", "2024-12-02", "Cash Dividend", "dividend", keyEFGH, "EFGH", 300)
	rebate.fees = centsOf(-10)
	return toolCase{
		name: "income",
		in: tcInput(toolInput{rows: []toolRow{
			buy,
			api("T61001602", "2024-04-04 11:54:14", "DIVIDEND_OR_INTEREST", "Ordinary Dividend~EFGH", "dividend", keyEFGH, "EFGH", 386),
			api("T61001604", "2024-04-04 11:54:15", "JOURNAL", "W-8 Withholding~EFGH", "tax", keyEFGH, "EFGH", -116),
			api("T61001603", "2024-04-30 21:18:44", "DIVIDEND_OR_INTEREST", "Free Balance Interest Adjustment", "interest", "", "", 42),
			withFees,
			file("Cc0d1e2f3.2", "2024-05-20", "Div Adjustment", "dividend", keyEFGH, "EFGH", -125),
			capital, bond,
			file("Cc0d1e2f3.5", "2024-08-15", "Bond Interest", "interest", keyBondA, "9128ZZQ15", 2125),
			file("Cc0d1e2f3.6", "2024-09-01", "Margin Interest", "interest", "", "", -317),
			file("Cc0d1e2f3.7", "2024-09-05", "Service Fee", "fee", "", "", -2500),
			file("Cc0d1e2f3.8", "2024-09-06", "Service Fee", "fee", "", "", 2500),
			file("Cc0d1e2f3.9", "2024-10-01", "Foreign Tax Reclaim", "tax", keyEFGH, "EFGH", 80),
			api("T61001605", "2024-10-15 10:00:00", "DIVIDEND_OR_INTEREST", "Ordinary Dividend", "dividend", "", "", 200),
			inexact,
			file("Cc0d1e2f3.11", "2024-11-15", "Credit Interest", "interest", "", "", 0),
			file("Cc0d1e2f3.12", "2024-11-20", "Bond Interest", "interest", keyBondA, "9128ZZQ15", -500),
			rebate,
		}}),
		want: []toolWant{
			{"T61001601", "Buy 300.00 shares 10", "BUY 300.00 shares 10"},
			{"T61001602", "Dividend 3.86", "DIVIDEND 3.86"},
			{"T61001604", "Taxes 1.16", "FEE 1.16 @taxes"},
			{"T61001603", "Interest 0.42", "INTEREST 0.42 @interest"},
			{"Cc0d1e2f3.1", "Dividend 9.50 fees 0.50", "DIVIDEND 9.50 fees 0.50"},
			{"Cc0d1e2f3.2", "Fees 1.25", "FEE 1.25 @fees"},
			{"Cc0d1e2f3.3", "Dividend 5.00 [return_of_capital]", "DIVIDEND 5.00 [return_of_capital]"},
			{"Cc0d1e2f3.4", "Buy 990.00 shares 1000", "BUY 990.00 shares 1000"},
			{"Cc0d1e2f3.5", "Dividend 21.25", "DIVIDEND 21.25"},
			{"Cc0d1e2f3.6", "Interest Charge 3.17", "FEE 3.17 @fees"},
			{"Cc0d1e2f3.7", "Fees 25.00", "FEE 25.00 @fees"},
			{"Cc0d1e2f3.8", "Fees Refund 25.00", "INTEREST 25.00 @fees"},
			{"Cc0d1e2f3.9", "Tax Refund 0.80", "INTEREST 0.80 @taxes"},
			{"T61001605", "left_out no_security", "left_out no_security"},
			{"Cc0d1e2f3.10", "Dividend 4.00 [fees_unknown]", "DIVIDEND 4.00 [fees_unknown]"},
			{"Cc0d1e2f3.11", "left_out zero_amount", "left_out zero_amount"},
			{"Cc0d1e2f3.12", "Interest Charge 5.00", "FEE 5.00 @fees"},
			{"Cc0d1e2f3.13", "Dividend 3.00", "DIVIDEND 3.00"},
		},
	}
}

// toolCaseOptions holds options: a futures option, a file's option without
// a multiplier, bought and expired; a short put that expires; an
// assignment, an exercise and an Exchange or Exercise; the API's
// expiration and another receive-and-deliver; an option whose multiplier
// is not whole, traded and expired; a future; an expiration that moved
// cash, and an option's split, which no rule closes.
func toolCaseOptions() toolCase {
	api := func(ref, clock, kind, description, category, key, symbol string, quantity int64, price float64, amount, fees int64) toolRow {
		r := toolRow{
			ref: ref, account: 2, source: "api", time: tcNY(clock), kind: kind, description: description, category: category,
			securityKey: key, symbol: symbol, quantity: centsOf(quantity), amount: centsOf(amount), fees: centsOf(fees),
		}
		if price != 0 {
			r.price = tcPrice(price)
		}
		return r
	}
	file := func(ref string, posted nytime.Date, action, category, key, symbol string, quantity int64) toolRow {
		return toolRow{
			ref: ref, account: 2, source: "file", posted: posted, kind: action, description: "OPTION", category: category,
			securityKey: key, symbol: symbol, quantity: centsOf(quantity), amount: centsOf(0),
		}
	}
	buyToOpen := file("Cd0e1f2a3.1", "2025-02-03", "Buy to Open", "buy", keyFileOp, symFileOp, 1_000_000)
	buyToOpen.price, buyToOpen.amount, buyToOpen.fees = tcPrice(2.1), centsOf(-21_065), centsOf(65)
	expired := api("T61001702", "2025-05-16 23:00:00", "RECEIVE_AND_DELIVER", "Removed due to Expiration", "other", keyPut, symPut, -1_000_000, 0, 0, 0)
	expired.flags = []string{"awaits_probe"}
	exchange := api("T61001703", "2025-05-19 10:00:00", "RECEIVE_AND_DELIVER", "Exchange", "other", keyPut, symPut, -1_000_000, 0, 0, 0)
	exchange.flags = []string{"awaits_probe"}
	withCash := file("Cd0e1f2a3.8", "2025-06-04", "Expired", "other", keyCall, symCall, -1_000_000)
	withCash.amount = centsOf(100)
	split := file("Cd0e1f2a3.9", "2025-06-05", "Options Frwd Split Adj", "other", keyCall, symCall, 1_000_000)
	return toolCase{
		name: "options",
		in: tcInput(toolInput{rows: []toolRow{
			api("T61001705", "2025-01-14 00:15:12", "TRADE", "", "buy", keyFutOpt, symFutOpt, 1_000_000, 21, -105_282, 282),
			api("T61001706", "2025-01-15 10:00:00", "TRADE", "", "buy", keyFuture, symFuture, 1_000_000, 5050, -235, 235),
			buyToOpen,
			api("T61001701", "2025-02-04 10:00:00", "TRADE", "", "sell", keyPut, symPut, -2_000_000, 1.6, 31_870, 130),
			file("Cd0e1f2a3.2", "2025-02-21", "Expired", "other", keyPut, symPut, 2_000_000),
			file("Cd0e1f2a3.3", "2025-03-21", "Expired", "other", keyFileOp, symFileOp, -1_000_000),
			file("Cd0e1f2a3.4", "2025-04-17", "Assigned", "other", keyPut, symPut, 1_000_000),
			file("Cd0e1f2a3.5", "2025-04-18", "Exercised", "other", keyCall, symCall, -1_000_000),
			file("Cd0e1f2a3.6", "2025-04-19", "Exchange or Exercise", "other", keyCall, symCall, -1_000_000),
			expired, exchange,
			api("T61001704", "2025-06-02 10:00:00", "TRADE", "", "buy", keyBadM, symBadM, 1_000_000, 1, -100, 0),
			file("Cd0e1f2a3.7", "2025-06-03", "Expired", "other", keyBadM, symBadM, -1_000_000),
			withCash, split,
		}}),
		want: []toolWant{
			{"T61001705", "Buy 1052.82 fees 2.82 shares 50", "BUY 1052.82 fees 2.82 shares 50"},
			{"T61001706", "left_out future", "left_out future"},
			{"Cd0e1f2a3.1", "Buy 210.65 fees 0.65 shares 100 [assumed_multiplier]", "BUY 210.65 fees 0.65 shares 100 [assumed_multiplier]"},
			{"T61001701", "Sell 318.70 fees 1.30 shares 200", "SELL 318.70 fees 1.30 shares 200"},
			{"Cd0e1f2a3.2", "Delivery (Inbound) 0.00 shares 200 [option_closed_at_zero]", "BUY 0.00 shares 200 [option_closed_at_zero]"},
			{"Cd0e1f2a3.3", "Delivery (Outbound) 0.00 shares 100 [assumed_multiplier option_closed_at_zero]", "SELL 0.00 shares 100 [assumed_multiplier option_closed_at_zero]"},
			{"Cd0e1f2a3.4", "Delivery (Inbound) 0.00 shares 100 [option_closed_at_zero]", "BUY 0.00 shares 100 [option_closed_at_zero]"},
			{"Cd0e1f2a3.5", "Delivery (Outbound) 0.00 shares 100 [option_closed_at_zero]", "SELL 0.00 shares 100 [option_closed_at_zero]"},
			{"Cd0e1f2a3.6", "Delivery (Outbound) 0.00 shares 100 [option_closed_at_zero]", "SELL 0.00 shares 100 [option_closed_at_zero]"},
			{"T61001702", "Delivery (Outbound) 0.00 shares 100 [option_closed_at_zero]", "SELL 0.00 shares 100 [option_closed_at_zero]"},
			{"T61001703", "left_out other", "left_out other"},
			{"T61001704", "left_out multiplier", "left_out multiplier"},
			{"Cd0e1f2a3.7", "left_out multiplier", "left_out multiplier"},
			{"Cd0e1f2a3.8", "left_out other", "left_out other"},
			{"Cd0e1f2a3.9", "left_out other", "left_out other"},
		},
	}
}

// toolCaseBonds holds two bonds: one that follows face value and prices per
// 100, bought, paid a coupon and redeemed in two rows; and one an API row
// of which prices per dollar of face, whose trades and delivery are left
// out and whose coupon is still written.
func toolCaseBonds() toolCase {
	file := func(ref string, posted nytime.Date, action, category, key, symbol string, quantity sql.NullInt64, price float64, amount int64, flags ...string) toolRow {
		r := toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "BOND", category: category,
			securityKey: key, symbol: symbol, quantity: quantity, amount: centsOf(amount), flags: flags,
		}
		if price != 0 {
			r.price = tcPrice(price)
		}
		return r
	}
	none := sql.NullInt64{}
	return toolCase{
		name: "bonds",
		in: tcInput(toolInput{
			rows: []toolRow{
				file("Ca1b2c3d4.1", "2025-03-03", "Buy", "buy", keyBondA, "9128ZZQ15", centsOf(5_000_000_000), 99.25, -498_250),
				file("Ca1b2c3d4.5", "2025-04-01", "Buy", "buy", keyBondB, "ZZT10A109", centsOf(10_000_000_000), 101, -1_010_000),
				{
					ref: "T61001801", account: 1, source: "api", time: tcNY("2025-05-01 10:00:00"), kind: "TRADE", category: "buy",
					securityKey: keyBondB, symbol: "ZZT10A109", quantity: centsOf(10_000_000_000), price: tcPrice(1.01), amount: centsOf(-1_010_000),
				},
				file("Ca1b2c3d4.2", "2025-08-15", "Bond Interest", "interest", keyBondA, "9128ZZQ15", none, 0, 10_625),
				file("Ca1b2c3d4.6", "2025-12-01", "Bond Interest", "interest", keyBondB, "ZZT10A109", none, 0, 25_000),
				file("Ca1b2c3d4.7", "2026-01-05", "Journaled Shares", "transfer_in", keyBondB, "ZZT10A109", centsOf(1_000_000_000), 100.5, 0),
				file("Ca1b2c3d4.3", "2026-08-17", "Full Redemption Adj", "sell", keyBondA, "9128ZZQ15", none, 0, 500_000, "two_row_event"),
				file("Ca1b2c3d4.4", "2026-08-17", "Full Redemption", "other", keyBondA, "9128ZZQ15", centsOf(-5_000_000_000), 0, 0, "two_row_event"),
			},
			positions: []toolPosition{
				{key: keyBondA, account: 1, marketDate: "2025-06-30", fetchedAt: tcNY("2025-06-30 18:00:00"), long: centsOf(5_000_000_000), short: centsOf(0), marketValue: tcPrice(4975)},
				{key: keyBondB, account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), long: centsOf(10_000_000_000), short: centsOf(0), marketValue: tcPrice(10_050)},
			},
			snapshots: []toolSnapshot{{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(100_000)}},
		}),
		want: []toolWant{
			{"Ca1b2c3d4.1", "Buy 4982.50 shares 5000", "BUY 4982.50 shares 5000"},
			{"Ca1b2c3d4.5", "left_out bond_unit", "left_out bond_unit"},
			{"T61001801", "left_out bond_unit", "left_out bond_unit"},
			{"Ca1b2c3d4.2", "Dividend 106.25", "DIVIDEND 106.25"},
			{"Ca1b2c3d4.6", "Dividend 250.00", "INTEREST 250.00 @interest [coupon_as_interest]"},
			{"Ca1b2c3d4.7", "left_out bond_unit", "left_out bond_unit"},
			{"Ca1b2c3d4.3", "Sell 5000.00 shares 5000", "SELL 5000.00 shares 5000"},
			{"Ca1b2c3d4.4", "combined with Ca1b2c3d4.3", "combined with Ca1b2c3d4.3"},
		},
	}
}

// toolCaseSecurities holds trades of securities that the tools know
// otherwise than by a plain ticker: a class share, a ticker with a dot, a
// delisted security under its ticker and then its CUSIP, a renamed one
// under each ticker, one only a CUSIP names, a row's symbol without a
// security key, and XYZ, whose description changed.
func toolCaseSecurities() toolCase {
	trade := func(ref, clock, category, key, symbol string, quantity int64, price float64, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: "TRADE", category: category,
			securityKey: key, symbol: symbol, quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount),
		}
	}
	keyless := toolRow{
		ref: "Ce5f6a7b8.1", account: 1, source: "file", posted: "2025-03-02", kind: "Buy", description: "QQQX FUND", category: "buy",
		symbol: "QQQX", quantity: centsOf(1_000_000), price: tcPrice(40), amount: centsOf(-4000), flags: []string{"unresolved_security"},
	}
	cusipOnly := toolRow{
		ref: "Ce5f6a7b8.2", account: 1, source: "file", posted: "2025-03-03", kind: "Buy", description: "EXAMPLE PRIVATE PLACEMENT UNITS", category: "buy",
		securityKey: keyNoTick, symbol: "ZZC99X101", quantity: centsOf(100_000_000), price: tcPrice(1), amount: centsOf(-10_000),
	}
	return toolCase{
		name: "securities",
		in: tcInput(toolInput{rows: []toolRow{
			trade("T61002001", "2025-02-03 10:00:00", "buy", keyClass, "ABC.B", 2_000_000, 300, -60_000),
			trade("T61002002", "2025-02-04 10:00:00", "buy", keyDot, "QRS.PRK", 10_000_000, 25, -25_000),
			trade("T61002003", "2025-02-05 10:00:00", "buy", keyGone, "OLDCO", 5_000_000, 10, -5000),
			trade("T61002005", "2025-02-06 10:00:00", "buy", keyRename, "PQR", 4_000_000, 20, -8000),
			trade("T61002007", "2025-02-07 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000),
			keyless, cusipOnly,
			trade("T61002006", "2026-02-06 10:00:00", "sell", keyRename, "PQRS", -4_000_000, 25, 10_000),
			trade("T61002004", "2026-06-01 10:00:00", "sell", keyGone, "ZZQ00K102", -5_000_000, 2, 1000),
		}}),
		want: []toolWant{
			{"T61002001", "Buy 600.00 shares 2", "BUY 600.00 shares 2"},
			{"T61002002", "Buy 250.00 shares 10", "BUY 250.00 shares 10"},
			{"T61002003", "Buy 50.00 shares 5", "BUY 50.00 shares 5"},
			{"T61002005", "Buy 80.00 shares 4", "BUY 80.00 shares 4"},
			{"T61002007", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"Ce5f6a7b8.1", "Buy 40.00 shares 1", "BUY 40.00 shares 1"},
			{"Ce5f6a7b8.2", "Buy 100.00 shares 100", "BUY 100.00 shares 100"},
			{"T61002006", "Sell 100.00 shares 4", "SELL 100.00 shares 4"},
			{"T61002004", "Sell 10.00 shares 5", "SELL 10.00 shares 5"},
		},
	}
}

// toolCaseLeftOut holds rows booked other that no rule writes, each with
// its advice by kind, and two rows without a day, one of them paired by
// your decision as a sweep.
func toolCaseLeftOut() toolCase {
	other := func(ref string, posted nytime.Date, action string, quantity sql.NullInt64, amount int64, flags ...string) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "XYZ CORP", category: "other",
			securityKey: keyXYZ, symbol: "XYZ", quantity: quantity, amount: centsOf(amount), flags: flags,
		}
	}
	none := sql.NullInt64{}
	return toolCase{
		name: "left_out",
		in: tcInput(toolInput{rows: []toolRow{
			{ref: "T61002101", account: 1, source: "api", kind: "CASH_RECEIPT", description: "Funds Received", amount: centsOf(1000), category: "deposit"},
			{
				ref: "T61002102", account: 1, source: "api", kind: "ELECTRONIC_FUND", description: "BANK SWEEP TO BROKERAGE", amount: centsOf(2000),
				category: "other", pairKind: "sweep", pairRef: "T61002103", pairAccount: 1,
			},
			other("Cf1a2b3c4.1", "2025-06-09", "Stock Split", centsOf(10_000_000), 0),
			other("Cf1a2b3c4.2", "2025-06-10", "Stock Merger", centsOf(-10_000_000), 0),
			other("Cf1a2b3c4.3", "2025-06-11", "Misc Cash Entry", none, 100, "other_action"),
			other("Cf1a2b3c4.4", "2025-06-12", "Name Change", centsOf(-1_000_000), 0),
			other("Cf1a2b3c4.5", "2025-06-13", "Exchange or Exercise", centsOf(-1_000_000), 0),
			other("Cf1a2b3c4.6", "2025-06-16", "Futures MM Sweep", none, -500, "unpaired_sweep"),
			other("Cf1a2b3c4.7", "2025-06-17", "Reverse Split", centsOf(-5_000_000), 0),
			other("Cf1a2b3c4.8", "2025-06-18", "Options Frwd Split", centsOf(1_000_000), 0),
		}}),
		want: []toolWant{
			{"T61002101", "left_out no_day", "left_out no_day"},
			{"T61002102", "left_out no_day", "left_out no_day"},
			{"Cf1a2b3c4.1", "left_out other", "left_out other"},
			{"Cf1a2b3c4.2", "left_out other", "left_out other"},
			{"Cf1a2b3c4.3", "left_out other", "left_out other"},
			{"Cf1a2b3c4.4", "left_out other", "left_out other"},
			{"Cf1a2b3c4.5", "left_out other", "left_out other"},
			{"Cf1a2b3c4.6", "left_out other", "left_out other"},
			{"Cf1a2b3c4.7", "left_out other", "left_out other"},
			{"Cf1a2b3c4.8", "left_out other", "left_out other"},
		},
	}
}

// toolCaseFlags holds rows that the store may count twice until you decide
// (late_api_row, near_duplicate, counts_differ), which are written; a cash
// row in the balances, a combined row and a row left out, each with a flag
// that needs you; and flags that are only information.
func toolCaseFlags() toolCase {
	trade := func(ref, clock string, price float64, amount int64, flags ...string) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: "TRADE", category: "buy", securityKey: keyXYZ, symbol: "XYZ",
			quantity: centsOf(1_000_000), price: tcPrice(price), amount: centsOf(amount), flags: flags,
		}
	}
	file := func(ref string, posted nytime.Date, action, category string, quantity sql.NullInt64, amount int64, flags ...string) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: action, description: "XYZ CORP", category: category,
			securityKey: keyXYZ, symbol: "XYZ", quantity: quantity, amount: centsOf(amount), flags: flags,
		}
	}
	none := sql.NullInt64{}
	buy := file("Cb1c2d3e4.2", "2025-07-02", "Buy", "buy", centsOf(1_000_000), -5000, "counts_differ", "restated")
	buy.price = tcPrice(50)
	return toolCase{
		name: "flags",
		in: tcInput(toolInput{
			rows: []toolRow{
				trade("T61002201", "2025-06-02 10:00:00", 50, -5000),
				file("Cb1c2d3e4.1", "2025-07-01", "Qualified Dividend", "dividend", none, 2500, "near_duplicate", "restated"),
				buy,
				trade("T61002202", "2025-07-03 10:00:00", 51, -5100, "late_api_row"),
				{
					ref: "T61002203", account: 1, source: "api", time: tcNY("2025-07-04 10:00:00"), kind: "ELECTRONIC_FUND", description: "ACH Debit",
					amount: centsOf(-1000), category: "deposit", flags: []string{"unexpected_sign"},
				},
				file("Cb1c2d3e4.3", "2025-07-07", "Cash Merger Adj", "other", centsOf(-1_000_000), 0, "two_row_event", "restated_quantity"),
				file("Cb1c2d3e4.4", "2025-07-07", "Cash Merger", "sell", none, 5300, "two_row_event"),
				file("Cb1c2d3e4.5", "2025-07-08", "Misc Cash Entry", "other", none, 100, "other_action", "symbol_unsure"),
			},
			snapshots: []toolSnapshot{{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(100_000)}},
		}),
		want: []toolWant{
			{"T61002201", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"Cb1c2d3e4.1", "Dividend 25.00", "DIVIDEND 25.00"},
			{"Cb1c2d3e4.2", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"T61002202", "Buy 51.00 shares 1", "BUY 51.00 shares 1"},
			{"T61002203", "Withdrawal 10.00", "in_balances"},
			{"Cb1c2d3e4.3", "combined with Cb1c2d3e4.4", "combined with Cb1c2d3e4.4"},
			{"Cb1c2d3e4.4", "Sell 53.00 shares 1", "SELL 53.00 shares 1"},
			{"Cb1c2d3e4.5", "left_out other", "left_out other"},
		},
	}
}

// toolCaseGFCash holds cash rows around the accounts' latest snapshots: one
// taken the evening of its market date, with an earlier one of that date
// and a later one with no cash, a row timed the instant it was taken; one
// taken after midnight; and an account with none.
func toolCaseGFCash() toolCase {
	api := func(ref string, account int64, clock string, amount int64) toolRow {
		return toolRow{
			ref: ref, account: account, source: "api", time: tcNY(clock), kind: "ELECTRONIC_FUND", description: "ACH Credit Received",
			amount: centsOf(amount), category: "deposit",
		}
	}
	file := func(ref string, account int64, posted nytime.Date, amount int64) toolRow {
		return toolRow{
			ref: ref, account: account, source: "file", posted: posted, kind: "MoneyLink Deposit", description: "Tfr EXAMPLE BANK",
			amount: centsOf(amount), category: "deposit",
		}
	}
	asOf := file("Ca2b3c4d5.3", 1, "2026-11-11", 5000)
	asOf.day = "2026-11-08"
	timeless := toolRow{
		ref: "T61002305", account: 1, source: "api", day: "2026-11-09", kind: "CASH_RECEIPT", description: "Funds Received",
		amount: centsOf(9000), category: "deposit",
	}
	return toolCase{
		name: "gf_cash",
		in: tcInput(toolInput{
			rows: []toolRow{
				api("T61002304", 3, "2026-11-02 10:00:00", 8000),
				asOf,
				file("Ca2b3c4d5.1", 1, "2026-11-09", 3000),
				timeless,
				file("Ca2b3c4d5.2", 1, "2026-11-10", 4000),
				file("Ca2b3c4d5.4", 2, "2026-11-10", 7000),
				api("T61002301", 1, "2026-11-10 17:00:00", 1000),
				api("T61002306", 1, "2026-11-10 18:05:00", 1100),
				api("T61002302", 1, "2026-11-10 19:00:00", 2000),
				api("T61002303", 1, "2026-11-11 10:00:00", 6000),
			},
			snapshots: []toolSnapshot{
				{account: 1, marketDate: "2026-11-09", fetchedAt: tcNY("2026-11-09 17:40:00"), cash: centsOf(100_000)},
				{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(120_000)},
				{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 16:30:00"), cash: centsOf(110_000)},
				{account: 1, marketDate: "2026-11-11", fetchedAt: tcNY("2026-11-11 17:50:00")},
				{account: 2, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-11 00:20:00"), cash: centsOf(-1234)},
			},
		}),
		want: []toolWant{
			{"T61002304", "Deposit 80.00", "left_out gf_cash"},
			{"Ca2b3c4d5.3", "Deposit 50.00", "left_out gf_cash"},
			{"Ca2b3c4d5.1", "Deposit 30.00", "in_balances"},
			{"T61002305", "Deposit 90.00", "in_balances"},
			{"Ca2b3c4d5.2", "Deposit 40.00", "left_out gf_cash"},
			{"Ca2b3c4d5.4", "Deposit 70.00", "in_balances"},
			{"T61002301", "Deposit 10.00", "in_balances"},
			{"T61002306", "Deposit 11.00", "in_balances"},
			{"T61002302", "Deposit 20.00", "left_out gf_cash"},
			{"T61002303", "Deposit 60.00", "left_out gf_cash"},
		},
	}
}

// toolCaseHoldings holds an account's rows against its latest snapshot on
// or before the export's last day (beside an earlier one of its date, one
// of an earlier date and one after that day): a sale of a position opened
// before the rows, a purchase split after it, a short option held, a
// position that agrees with a purchase of the snapshot's day, a long and a
// short option that expired, and a purchase after the snapshot's day; and
// an account with rows and no snapshot, which buys what the first holds.
func toolCaseHoldings() toolCase {
	trade := func(ref, clock, category, key, symbol string, quantity int64, price float64, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: "TRADE", category: category,
			securityKey: key, symbol: symbol, quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount),
		}
	}
	snap := tcNY("2026-11-10 18:05:00")
	position := func(key string, long, short int64, value float64) toolPosition {
		return toolPosition{key: key, account: 1, marketDate: "2026-11-10", fetchedAt: snap, long: centsOf(long), short: centsOf(short), marketValue: tcPrice(value)}
	}
	expired := func(ref string, posted nytime.Date, key, symbol string, quantity int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "file", posted: posted, kind: "Expired", description: "OPTION", category: "other",
			securityKey: key, symbol: symbol, quantity: centsOf(quantity), amount: centsOf(0),
		}
	}
	later := position(keyXYZ, 999_000_000, 0, 1)
	later.marketDate, later.fetchedAt = "2026-11-13", tcNY("2026-11-13 18:05:00")
	// An earlier snapshot of the same market date, and one of an earlier
	// date, which the check does not take.
	sameDay := position(keyXYZ, 998_000_000, 0, 1)
	sameDay.fetchedAt = tcNY("2026-11-10 16:30:00")
	earlier := position(keyXYZ, 997_000_000, 0, 1)
	earlier.marketDate, earlier.fetchedAt = "2026-06-01", tcNY("2026-06-01 18:05:00")
	return toolCase{
		name: "holdings",
		in: tcInput(toolInput{
			rows: []toolRow{
				trade("T61002407", "2025-01-06 10:00:00", "buy", keyPut, symPut, 1_000_000, 2, -20_000),
				expired("Ca3b4c5d6.2", "2025-02-21", keyPut, symPut, -1_000_000),
				trade("T61002401", "2026-02-02 10:00:00", "sell", keyXYZ, "XYZ", -10_000_000, 50, 50_000),
				trade("T61002402", "2026-03-02 10:00:00", "buy", keyEFGH, "EFGH", 10_000_000, 30, -30_000),
				trade("T61002408", "2026-04-01 10:00:00", "sell", keyCall, symCall, -1_000_000, 3, 30_000),
				{
					ref: "T61002406", account: 3, source: "api", time: tcNY("2026-05-05 10:00:00"), kind: "WIRE_IN", description: "Wire Incoming",
					amount: centsOf(1000), category: "deposit",
				},
				{
					ref: "T61002410", account: 3, source: "api", time: tcNY("2026-05-06 10:00:00"), kind: "TRADE", category: "buy",
					securityKey: keyEFGH, symbol: "EFGH", quantity: centsOf(1_000_000), price: tcPrice(30), amount: centsOf(-3000),
				},
				{
					ref: "Ca3b4c5d6.1", account: 1, source: "file", posted: "2026-06-10", kind: "Stock Split", description: "EFGH TOTAL MARKET ETF",
					category: "other", securityKey: keyEFGH, symbol: "EFGH", quantity: centsOf(10_000_000), amount: centsOf(0),
				},
				trade("T61002403", "2026-07-01 10:00:00", "sell", keyPut27, symPut27, -2_000_000, 1.6, 32_000),
				trade("T61002404", "2026-08-03 10:00:00", "buy", keyABCD, "ABCD", 3_000_000, 40, -12_000),
				expired("Ca3b4c5d6.3", "2026-09-18", keyCall, symCall, 1_000_000),
				trade("T61002409", "2026-11-10 10:00:00", "buy", keyABCD, "ABCD", 1_000_000, 40, -4000),
				trade("T61002405", "2026-11-11 10:00:00", "buy", keyABCD, "ABCD", 1_000_000, 41, -4100),
			},
			positions: []toolPosition{
				position(keyXYZ, 5_000_000, 0, 262.5),
				position(keyEFGH, 20_000_000, 0, 600),
				position(keyPut27, 0, 2_000_000, -310),
				position(keyABCD, 4_000_000, 0, 160),
				later, sameDay, earlier,
			},
			snapshots: []toolSnapshot{
				{account: 1, marketDate: "2026-06-01", fetchedAt: tcNY("2026-06-01 18:05:00"), cash: centsOf(100_000)},
				{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 16:30:00"), cash: centsOf(100_000)},
				{account: 1, marketDate: "2026-11-10", fetchedAt: snap, cash: centsOf(100_000)},
				{account: 1, marketDate: "2026-11-13", fetchedAt: tcNY("2026-11-13 18:05:00"), cash: centsOf(100_000)},
			},
		}),
		want: []toolWant{
			{"T61002407", "Buy 200.00 shares 100", "BUY 200.00 shares 100"},
			{"Ca3b4c5d6.2", "Delivery (Outbound) 0.00 shares 100 [option_closed_at_zero]", "SELL 0.00 shares 100 [option_closed_at_zero]"},
			{"T61002401", "Sell 500.00 shares 10", "SELL 500.00 shares 10"},
			{"T61002402", "Buy 300.00 shares 10", "BUY 300.00 shares 10"},
			{"T61002408", "Sell 300.00 shares 100", "SELL 300.00 shares 100"},
			{"T61002406", "Deposit 10.00", "left_out gf_cash"},
			{"T61002410", "Buy 30.00 shares 1", "BUY 30.00 shares 1"},
			{"Ca3b4c5d6.1", "left_out other", "left_out other"},
			{"T61002403", "Sell 320.00 shares 200", "SELL 320.00 shares 200"},
			{"T61002404", "Buy 120.00 shares 3", "BUY 120.00 shares 3"},
			{"Ca3b4c5d6.3", "Delivery (Inbound) 0.00 shares 100 [option_closed_at_zero]", "BUY 0.00 shares 100 [option_closed_at_zero]"},
			{"T61002409", "Buy 40.00 shares 1", "BUY 40.00 shares 1"},
			{"T61002405", "Buy 41.00 shares 1", "BUY 41.00 shares 1"},
		},
	}
}

// toolCaseGFDates holds what Ghostfolio's dates must keep in order: a buy
// and its option's expiration on one day, a day trade, a file's Buy and
// Cancel Buy of one day, an evening fill booked on the next day beside a
// timed sale of that day, the legs of one trade, and two timeless buys of
// one day.
func toolCaseGFDates() toolCase {
	trade := func(ref, clock, category, key, symbol string, quantity int64, price float64, amount, fees int64) toolRow {
		return toolRow{
			ref: ref, account: 2, source: "api", time: tcNY(clock), kind: "TRADE", category: category, securityKey: key, symbol: symbol,
			quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount), fees: centsOf(fees),
		}
	}
	file := func(ref string, posted nytime.Date, action, category, key, symbol string, quantity int64, price float64, amount int64) toolRow {
		r := toolRow{
			ref: ref, account: 2, source: "file", posted: posted, kind: action, description: "XYZ", category: category,
			securityKey: key, symbol: symbol, quantity: centsOf(quantity), amount: centsOf(amount),
		}
		if price != 0 {
			r.price = tcPrice(price)
		}
		return r
	}
	evening := trade("T61002504", "2025-01-23 19:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 52, -5200, 0)
	evening.day = "2025-01-24"
	spread := toolRow{
		ref: "T61002506", account: 2, source: "api", time: tcNY("2025-01-27 12:00:00"), kind: "TRADE", category: "buy",
		amount: centsOf(-7000), fees: centsOf(0), flags: []string{"mixed_legs"},
	}
	return toolCase{
		name: "gf_dates",
		in: tcInput(toolInput{
			rows: []toolRow{
				file("Cc3d4e5f6.1", "2025-01-21", "Expired", "other", keyPut, symPut, -1_000_000, 0, 0),
				trade("T61002501", "2025-01-21 11:05:12", "buy", keyPut, symPut, 1_000_000, 2, -20_065, 65),
				trade("T61002502", "2025-01-22 10:00:00", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000, 0),
				trade("T61002503", "2025-01-22 14:30:00", "sell", keyXYZ, "XYZ", -1_000_000, 51, 5100, 0),
				file("Cc3d4e5f6.2", "2025-01-23", "Buy", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000),
				file("Cc3d4e5f6.3", "2025-01-23", "Cancel Buy", "buy", keyXYZ, "XYZ", -1_000_000, 50, 5000),
				evening,
				trade("T61002505", "2025-01-24 10:30:00", "sell", keyXYZ, "XYZ", -1_000_000, 52, 5200, 0),
				spread,
				file("Cc3d4e5f6.4", "2025-01-28", "Buy", "buy", keyXYZ, "XYZ", 1_000_000, 50, -5000),
				file("Cc3d4e5f6.5", "2025-01-28", "Buy", "buy", keyABCD, "ABCD", 1_000_000, 40, -4000),
			},
			legs: toolLegs("T61002506", -15_000, 8000, 1.5, 0.8),
		}),
		want: []toolWant{
			{"Cc3d4e5f6.1", "Delivery (Outbound) 0.00 shares 100 [option_closed_at_zero]", "SELL 0.00 shares 100 [option_closed_at_zero]"},
			{"T61002501", "Buy 200.65 fees 0.65 shares 100", "BUY 200.65 fees 0.65 shares 100"},
			{"T61002502", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"T61002503", "Sell 51.00 shares 1", "SELL 51.00 shares 1"},
			{"Cc3d4e5f6.2", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"Cc3d4e5f6.3", "Sell 50.00 shares 1", "SELL 50.00 shares 1"},
			{"T61002504", "Buy 52.00 shares 1", "BUY 52.00 shares 1"},
			{"T61002505", "Sell 52.00 shares 1", "SELL 52.00 shares 1"},
			{"T61002506", "Buy 150.00 shares 100 leg 4; Sell 80.00 shares 100 leg 5", "BUY 150.00 shares 100 leg 4; SELL 80.00 shares 100 leg 5"},
			{"Cc3d4e5f6.4", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"Cc3d4e5f6.5", "Buy 40.00 shares 1", "BUY 40.00 shares 1"},
		},
	}
}

// toolCaseManual is an export for which -manual names a ticker in another
// case, a class share in Yahoo's form, a security with no row and no
// position, a row's symbol without a security key, and a security with no
// row that a position holds.
func toolCaseManual() toolCase {
	trade := func(ref, clock, key, symbol string, quantity int64, price float64, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: "TRADE", category: "buy", securityKey: key, symbol: symbol,
			quantity: centsOf(quantity), price: tcPrice(price), amount: centsOf(amount),
		}
	}
	keyless := trade("Ce6f7a8b9.1", "2025-03-03 10:00:00", "", "QQQX", 1_000_000, 40, -4000)
	keyless.source, keyless.time, keyless.posted = "file", time.Time{}, "2025-03-03"
	return toolCase{
		name: "manual",
		in: tcInput(toolInput{
			manual: []string{"xyz", "ABC-B", " EFGH ", "QQQX", "ABCD"},
			rows: []toolRow{
				trade("T61002601", "2025-02-03 10:00:00", keyXYZ, "XYZ", 1_000_000, 50, -5000),
				trade("T61002602", "2025-02-04 10:00:00", keyClass, "ABC.B", 1_000_000, 300, -30_000),
				keyless,
			},
			// ABCD has no row, but a position, whose prices the export
			// writes.
			positions: []toolPosition{{key: keyABCD, account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), long: centsOf(1_000_000), marketValue: tcPrice(41)}},
		}),
		want: []toolWant{
			{"T61002601", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
			{"T61002602", "Buy 300.00 shares 1", "BUY 300.00 shares 1"},
			{"Ce6f7a8b9.1", "Buy 40.00 shares 1", "BUY 40.00 shares 1"},
		},
	}
}

// toolCaseIdle holds securities that Ghostfolio asks Yahoo about, of which
// one has had no row in the year before the export's last day and no
// account holds; one the same but held in another account's latest
// snapshot; one with a row a year before that day to the day; and a manual
// security with an old row, which Yahoo is never asked about.
func toolCaseIdle() toolCase {
	trade := func(ref, clock, key, symbol string, price float64, amount int64) toolRow {
		return toolRow{
			ref: ref, account: 1, source: "api", time: tcNY(clock), kind: "TRADE", category: "buy", securityKey: key, symbol: symbol,
			quantity: centsOf(1_000_000), price: tcPrice(price), amount: centsOf(amount),
		}
	}
	return toolCase{
		name: "idle",
		in: tcInput(toolInput{
			account: 1,
			rows: []toolRow{
				trade("T61002701", "2025-11-11 10:00:00", keyEFGH, "EFGH", 30, -3000),
				trade("T61002702", "2025-11-11 11:00:00", keyABCD, "ABCD", 40, -4000),
				trade("T61002704", "2025-11-11 12:00:00", keyDot, "QRS.PRK", 25, -2500),
				trade("T61002703", "2025-11-12 10:00:00", keyXYZ, "XYZ", 50, -5000),
			},
			positions: []toolPosition{
				// Account 2's latest snapshot holds ABCD; earlier ones of
				// account 1 held EFGH, one of them of its latest date.
				{key: keyABCD, account: 2, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), long: centsOf(1_000_000), short: centsOf(0), marketValue: tcPrice(41)},
				{key: keyEFGH, account: 1, marketDate: "2026-01-05", fetchedAt: tcNY("2026-01-05 18:05:00"), long: centsOf(1_000_000), short: centsOf(0), marketValue: tcPrice(31)},
				{key: keyEFGH, account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 16:30:00"), long: centsOf(1_000_000), short: centsOf(0), marketValue: tcPrice(32)},
			},
			snapshots: []toolSnapshot{
				{account: 1, marketDate: "2026-01-05", fetchedAt: tcNY("2026-01-05 18:05:00"), cash: centsOf(1000)},
				{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 16:30:00"), cash: centsOf(1000)},
				{account: 1, marketDate: "2026-11-10", fetchedAt: tcNY("2026-11-10 18:05:00"), cash: centsOf(1000)},
			},
		}),
		want: []toolWant{
			{"T61002701", "Buy 30.00 shares 1", "BUY 30.00 shares 1"},
			{"T61002702", "Buy 40.00 shares 1", "BUY 40.00 shares 1"},
			{"T61002704", "Buy 25.00 shares 1", "BUY 25.00 shares 1"},
			{"T61002703", "Buy 50.00 shares 1", "BUY 50.00 shares 1"},
		},
	}
}

// toolCaseTwoKeys holds one security that the store holds under two keys,
// a History file's and the API's, neither with a CUSIP: bought in the
// file's rows and sold in the API's, and held under the API's key, which
// -manual names for Ghostfolio. Each tool holds it as one security: one
// row of PP's securities, one MANUAL asset, and one holding, which the
// snapshot's shares match.
func toolCaseTwoKeys() toolCase {
	securities, names := tcSecurities()
	securities = append(securities,
		toolSecurityRow{key: keyTwoFile, symbol: "JKLM", description: "JKLM SMALL CAP VALUE ETF"},
		toolSecurityRow{key: keyTwoAPI, assetType: "COLLECTIVE_INVESTMENT", symbol: "JKLM", description: "JKLM SMALL-CAP VALUE ETF"},
	)
	for _, n := range []struct {
		key, kind, value string
		seen             time.Time
	}{
		{keyTwoFile, "symbol", "JKLM", tcNY("2026-11-11 09:00:00")},
		{keyTwoFile, "description", "JKLM SMALL CAP VALUE ETF", tcNY("2026-11-11 09:00:00")},
		{keyTwoAPI, "symbol", "JKLM", tcNY("2026-03-16 10:00:00")},
		{keyTwoAPI, "description", "JKLM SMALL-CAP VALUE ETF", tcNY("2026-03-16 10:00:00")},
	} {
		names = append(names, toolName{key: n.key, kind: n.kind, value: n.value, firstSeen: n.seen, lastSeen: n.seen})
	}
	return toolCase{
		name: "two_keys",
		in: tcInput(toolInput{
			manual:     []string{"JKLM"},
			securities: securities, names: names,
			rows: []toolRow{
				{
					ref: "Cf7a8b9c0.1", account: 1, source: "file", posted: "2024-03-11", kind: "Buy", description: "JKLM SMALL CAP VALUE ETF",
					category: "buy", securityKey: keyTwoFile, symbol: "JKLM", quantity: centsOf(10_000_000), price: tcPrice(150), amount: centsOf(-150_000),
				},
				{
					ref: "T61002801", account: 1, source: "api", time: tcNY("2026-03-16 10:00:00"), kind: "TRADE", category: "sell",
					securityKey: keyTwoAPI, symbol: "JKLM", quantity: centsOf(-4_000_000), price: tcPrice(170), amount: centsOf(68_000),
				},
			},
			positions: []toolPosition{rcPosition("2026-11-10", keyTwoAPI, 6_000_000, 1080)},
			snapshots: []toolSnapshot{rcSnapshot(1, "2026-11-10", 100_000)},
		}),
		want: []toolWant{
			{"Cf7a8b9c0.1", "Buy 1500.00 shares 10", "BUY 1500.00 shares 10"},
			{"T61002801", "Sell 680.00 shares 4", "SELL 680.00 shares 4"},
		},
	}
}
