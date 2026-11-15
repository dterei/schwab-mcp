package history

import (
	"bytes"
	"encoding/csv"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// ppConfigurations returns the configurations, by name, that every PP
// export writes alike.
func ppConfigurations() map[string][]byte {
	files := map[string][]byte{}
	for _, f := range ppFiles(&toolInput{tool: toolPP}, nil) {
		if f.kind == ppConfiguration {
			files[f.name] = f.data
		}
	}
	return files
}

// ppAccountNames returns the names of the accounts of the PP export e, as
// its manifest lists them (8.1 of the design): each that it writes or names
// as a transfer's Offset Account, and each of its chain. Those are the
// accounts that PP holds once you have made those that stderr lists, and
// the only ones that a transfer may name.
func ppAccountNames(e *ToolExport) []string {
	var names []string
	for _, a := range e.Accounts {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return names
}

// ppCase writes case c as a PP export (TestToolCases), through WriteDir,
// and requires that the folder golden hold its securities, its records and
// its manifest, byte for byte, beside the configurations that every export
// writes alike; that PP 0.90.0 read the export as it means (ppCheck); and
// that a model of PP into which it is imported, in the order of its files,
// mark exactly the rows that the export says PP marks, which are none, and
// then hold the ledger's cash and shares of each account (ppReconcile): no
// row of the cases is one that PP marks as existing, as a transfer is one
// row of its paying account's file.
func ppCase(t *testing.T, c toolCase, golden string) {
	e := caseExport(t, c, toolPP)
	s := chainStepFrom(t, e, t.TempDir())
	files := toolFolder(t, s.dir)
	for name, data := range ppConfigurations() {
		if !bytes.Equal(files[name], data) {
			t.Errorf("the export writes %s otherwise than every export does", name)
		}
		delete(files, name)
	}
	toolGolden(t, golden, files)

	m := newPPModel()
	ppApply(t, m, s)
	for _, mk := range s.marks {
		t.Errorf("PP marks row %d of %s, of %s, as existing", mk.line, mk.file, mk.record.ref)
	}
	ppReconcile(t, m, e.in, e.events, e.outcomes)
}

// ppReconcile requires that model m, into which the PP export of in was
// imported, hold what the ledger holds of each account of the export (11.4
// of the design): its cash, the cash of each row written, but a
// delivery's, which PP books without its cash (cash_not_written), and of
// the receiving side of each transfer written, whose own row is not
// written, combined with it or dated after the export's last day, which PP
// makes from the transfer; and of each security, the shares that the rows
// written and combined move, their quantities, or their legs', times the
// security's multiplier. The model finds each security by the identifiers
// the export writes for it, as PP does; the shares of every security it
// holds must add up to the ledger's, so that none is held twice.
func ppReconcile(t *testing.T, m *ppModel, in *toolInput, events []toolEvent, outcomes []toolOutcome) {
	t.Helper()
	ix := newToolIndex(in)
	written := map[rowKey]bool{}
	for i, o := range outcomes {
		if o.outcome == outcomeWritten {
			written[rowKey{in.rows[i].account, in.rows[i].ref}] = true
		}
	}
	cash := map[int64]int64{}
	type holding struct {
		account int64
		ref     secRef
	}
	shares := map[holding]int64{}
	securities := map[secRef]*toolSecurity{}
	move := func(account int64, s *toolSecurity, q int64) {
		h := holding{account, s.ref()}
		shares[h] += q * s.multiplier
		securities[s.ref()] = s
	}
	for i, o := range outcomes {
		r := &in.rows[i]
		if o.outcome != outcomeWritten && o.outcome != outcomeCombined {
			continue
		}
		delivery := slices.ContainsFunc(eventsOf(events, i), func(e toolEvent) bool {
			return e.kind == evDeliveryIn || e.kind == evDeliveryOut
		})
		if o.outcome == outcomeWritten && !delivery {
			cash[r.account] += r.amount.Int64
		}
		for _, e := range eventsOf(events, i) {
			if e.kind == evTransferOut && !written[rowKey{e.pair.id, e.pairRef}] {
				cash[e.pair.id] -= r.amount.Int64
			}
		}
		if legs := ix.legsOf(r); !r.quantity.Valid && len(legs) >= 2 {
			for _, l := range legs {
				move(r.account, ix.security(l.securityKey, l.symbol), l.quantity.Int64)
			}
		} else if r.quantity.Valid && r.quantity.Int64 != 0 {
			move(r.account, ix.security(r.securityKey, r.symbol), r.quantity.Int64)
		}
	}
	for _, a := range in.accounts {
		if !in.selected(a.id) {
			continue
		}
		if got := m.cash(a.name()); got != cash[a.id] {
			t.Errorf("%s holds %s of cash, the ledger %s", a.name(), formatCents(got), formatCents(cash[a.id]))
		}
		held := m.shares(a.name())
		var total, want int64
		for _, q := range held {
			total += q
		}
		// The ledger's shares of each security that PP holds, which the
		// store may hold under several keys.
		cache := newPPCache(m.securities)
		of := map[*ppSecurity]int64{}
		var order []*ppSecurity
		for _, ref := range slices.SortedFunc(maps.Keys(securities), func(a, b secRef) int {
			return strings.Compare(a.key+" "+a.symbol, b.key+" "+b.symbol)
		}) {
			q := shares[holding{a.id, ref}] * 100 // millionths to PP's 10^-8
			want += q
			s, made, err := cache.lookup(ppIdentifiers(securities[ref]))
			if err != nil || made && q != 0 {
				t.Errorf("%s: PP holds no security %v: %v", a.name(), ref, err)
				continue
			}
			if _, ok := of[s]; !ok {
				order = append(order, s)
			}
			of[s] += q
		}
		for _, s := range order {
			if held[s] != of[s] {
				t.Errorf("%s holds %d of %s, the ledger %d (in 10^-8)", a.name(), held[s], s.name, of[s])
			}
		}
		if total != want {
			t.Errorf("%s holds %d shares in all, the ledger %d (in 10^-8)", a.name(), total, want)
		}
	}
}

// ppFilesOf returns the files of the PP export of case name.
func ppFilesOf(name string) []ppFile {
	in, events, _ := mapCase(toolCaseNamed(name), toolPP)
	return ppFiles(in, events)
}

// ppFileNamed returns the file of files named name.
func ppFileNamed(t *testing.T, files []ppFile, name string) ppFile {
	t.Helper()
	i := slices.IndexFunc(files, func(f ppFile) bool { return f.name == name })
	if i < 0 {
		t.Fatalf("no file %s", name)
	}
	return files[i]
}

// ppRows returns the rows of file f, each a map from its header's labels
// to its cells.
func ppRows(t *testing.T, f ppFile) []map[string]string {
	t.Helper()
	records, err := csv.NewReader(bytes.NewReader(f.data)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]string
	for _, rec := range records[1:] {
		row := map[string]string{}
		for i, label := range records[0] {
			row[label] = rec[i]
		}
		out = append(out, row)
	}
	return out
}

// ppRowOf returns the row of file f whose Note names ref first.
func ppRowOf(t *testing.T, f ppFile, ref string) map[string]string {
	t.Helper()
	for _, r := range ppRows(t, f) {
		if strings.Contains(r["Note"], " "+ref+":") || strings.Contains(r["Note"], " "+ref+" ") {
			return r
		}
	}
	t.Fatalf("%s has no row of %s", f.name, ref)
	return nil
}

// TestPPFiles checks what ppFiles writes, beside the golden files that pin
// its bytes (5 of the design): the files and their order, the rows of each
// kind of record, the notes, times and identifiers.
func TestPPFiles(t *testing.T) {
	t.Parallel()

	t.Run("files", func(t *testing.T) {
		t.Parallel()
		// Each account's account file and then its portfolio file, by id,
		// after securities.csv, then the configurations; a file only when
		// it has a row, but securities.csv always.
		type want struct {
			name, kind, configuration, account string
			rows                               int
		}
		files := ppFilesOf("gf_cash")
		more := ppFilesOf("deliveries")
		none := ppFilesOf("left_out")
		both := ppFilesOf("holdings")
		var got []want
		for _, f := range slices.Concat(files, more, none, both) {
			got = append(got, want{f.name, f.kind, f.configuration, f.account.label(), f.rows})
		}
		config := func(name string) want { return want{name, ppConfiguration, "", "A0", 0} }
		wants := []want{
			{"securities.csv", ppSecurities, ppSecuritiesConfig, "A0", 0},
			{"account-transactions-A1-135.csv", ppAccount, ppAccountConfig, "A1 ...135", 8},
			{"account-transactions-A2-398.csv", ppAccount, ppAccountConfig, "A2 ...398", 1},
			{"account-transactions-A3-783.csv", ppAccount, ppAccountConfig, "A3 ...783", 1},
			config(ppSecuritiesConfig), config(ppAccountConfig), config(ppPortfolioConfig),
			{"securities.csv", ppSecurities, ppSecuritiesConfig, "A0", 5},
			{"portfolio-transactions-A1-135.csv", ppPortfolio, ppPortfolioConfig, "A1 ...135", 13},
			{"portfolio-transactions-A2-398.csv", ppPortfolio, ppPortfolioConfig, "A2 ...398", 6},
			config(ppSecuritiesConfig), config(ppAccountConfig), config(ppPortfolioConfig),
			{"securities.csv", ppSecurities, ppSecuritiesConfig, "A0", 0},
			config(ppSecuritiesConfig), config(ppAccountConfig), config(ppPortfolioConfig),
			// A1's portfolio file, then A3's account file and its portfolio
			// file.
			{"securities.csv", ppSecurities, ppSecuritiesConfig, "A0", 6},
			{"portfolio-transactions-A1-135.csv", ppPortfolio, ppPortfolioConfig, "A1 ...135", 10},
			{"account-transactions-A3-783.csv", ppAccount, ppAccountConfig, "A3 ...783", 1},
			{"portfolio-transactions-A3-783.csv", ppPortfolio, ppPortfolioConfig, "A3 ...783", 1},
			config(ppSecuritiesConfig), config(ppAccountConfig), config(ppPortfolioConfig),
		}
		if !slices.Equal(got, wants) {
			t.Errorf("files\n%v\nwant\n%v", got, wants)
		}
		if f := ppFileNamed(t, none, "securities.csv"); string(f.data) != "Ticker Symbol,WKN,Security Name,Currency,Date of Quote,Quote\n" {
			t.Errorf("securities.csv of no security is %q", f.data)
		}
	})

	t.Run("configurations", func(t *testing.T) {
		t.Parallel()
		// Each configuration is one that PP applies, of its target, whose
		// labels are its files' header.
		headers := map[string][]string{
			ppSecuritiesConfig: ppSecuritiesHeader, ppAccountConfig: ppAccountHeader, ppPortfolioConfig: ppPortfolioHeader,
		}
		for _, f := range ppFiles(&toolInput{tool: toolPP}, nil) {
			if f.kind != ppConfiguration {
				continue
			}
			c, probs := ppParseConfig(f.name, f.data, ppKinds[f.name].target)
			for _, p := range probs {
				t.Error(p)
			}
			if c == nil || !slices.Equal(c.labels(), headers[f.name]) {
				t.Errorf("%s does not label the columns of its files' header %q", f.name, headers[f.name])
			}
		}
	})

	t.Run("records", func(t *testing.T) {
		t.Parallel()
		// Rows of the design's example (5.3), with the cases' made-up
		// values: a dividend and its withholding with the security's ticker
		// and name, and interest without a security.
		files := ppFilesOf("income")
		acct := ppFileNamed(t, files, "account-transactions-A1-135.csv")
		lines := strings.Split(string(acct.data), "\n")
		for _, want := range []string{
			"Date,Time,Type,Value,Transaction Currency,Fees,Ticker Symbol,WKN,Security Name,Note,Cash Account,Offset Account",
			"2024-04-04,11:54,Dividend,3.86,USD,,EFGH,,EFGH TOTAL MARKET ETF,schwabtx A1 ...135 T61001602: Ordinary Dividend~EFGH,Schwab A1 ...135,",
			"2024-04-04,11:54,Taxes,1.16,USD,,EFGH,,EFGH TOTAL MARKET ETF,schwabtx A1 ...135 T61001604: W-8 Withholding~EFGH,Schwab A1 ...135,",
			"2024-04-30,21:18,Interest,0.42,USD,,,,,schwabtx A1 ...135 T61001603: Free Balance Interest Adjustment,Schwab A1 ...135,",
			"2024-05-15,,Dividend,9.50,USD,0.50,EFGH,,EFGH TOTAL MARKET ETF,schwabtx A1 ...135 Cc0d1e2f3.1: INCOME,Schwab A1 ...135,",
			// A coupon is a dividend of its bond, which has no ticker and
			// whose WKN is its CUSIP.
			"2024-08-15,,Dividend,21.25,USD,,,9128ZZQ15,EXAMPLE TREASURY NOTE 4.25% 08/15/2034,schwabtx A1 ...135 Cc0d1e2f3.5: INCOME,Schwab A1 ...135,",
			"2024-09-01,,Interest Charge,3.17,USD,,,,,schwabtx A1 ...135 Cc0d1e2f3.6: INCOME,Schwab A1 ...135,",
			"2024-09-06,,Fees Refund,25.00,USD,,,,,schwabtx A1 ...135 Cc0d1e2f3.8: INCOME,Schwab A1 ...135,",
			// A note with a comma is quoted.
			`2024-11-01,,Dividend,4.00,USD,,EFGH,,EFGH TOTAL MARKET ETF,"schwabtx A1 ...135 Cc0d1e2f3.10: INCOME; fees unknown, in the price",Schwab A1 ...135,`,
		} {
			if !slices.Contains(lines, want) {
				t.Errorf("%s lacks the line\n%s\nit holds\n%s", acct.name, want, acct.data)
			}
		}
		port := ppFileNamed(t, files, "portfolio-transactions-A1-135.csv")
		if !bytes.HasPrefix(port.data, []byte("Date,Time,Type,Value,Transaction Currency,Shares,Fees,Ticker Symbol,WKN,Security Name,Note,Cash Account,Securities Account\n"+
			"2024-01-10,10:00,Buy,300.00,USD,10,,EFGH,,EFGH TOTAL MARKET ETF,schwabtx A1 ...135 T61001601: TRADE,Schwab A1 ...135,Schwab A1 ...135\n")) {
			t.Errorf("%s is\n%s", port.name, port.data)
		}

		// A buy with fees, a sale whose fees PP gets in a Fees row of their
		// own, and a rebate in the price.
		files = ppFilesOf("trades")
		port = ppFileNamed(t, files, "portfolio-transactions-A1-135.csv")
		acct = ppFileNamed(t, files, "account-transactions-A1-135.csv")
		for _, c := range []struct {
			f    ppFile
			ref  string
			want map[string]string
		}{
			{port, "T61001301", map[string]string{"Type": "Buy", "Value": "500.65", "Fees": "0.65", "Shares": "10", "Cash Account": "Schwab A1 ...135"}},
			{port, "T61001306", map[string]string{"Type": "Sell", "Value": "0.05", "Fees": "", "Shares": "0.01", "Note": "schwabtx A1 ...135 T61001306: TRADE; fees in a Fees row"}},
			{acct, "T61001306", map[string]string{"Type": "Fees", "Value": "0.10", "Fees": "", "Ticker Symbol": "EFGH", "Note": "schwabtx A1 ...135 T61001306: TRADE; fees of the sale"}},
			{port, "T61001307", map[string]string{"Type": "Buy", "Value": "59.50", "Fees": "", "Shares": "2"}},
			{port, "Cf0e1d2c3.3", map[string]string{"Type": "Sell", "Value": "100.00", "Time": "", "Note": "schwabtx A1 ...135 Cf0e1d2c3.3: XYZ CORP"}},
		} {
			row := ppRowOf(t, c.f, c.ref)
			for k, v := range c.want {
				if row[k] != v {
					t.Errorf("%s %s: %s %q, want %q", c.f.name, c.ref, k, row[k], v)
				}
			}
		}
	})

	t.Run("transfers", func(t *testing.T) {
		t.Parallel()
		// A transfer is one row of the paying account's file, noted with
		// both sides, its Offset Account the receiving account, whose file
		// holds nothing of it; a pair written apart names its partner.
		files := ppFilesOf("transfers")
		a1 := ppFileNamed(t, files, "account-transactions-A1-135.csv")
		a2 := ppFileNamed(t, files, "account-transactions-A2-398.csv")
		row := ppRowOf(t, a2, "T61001201")
		if row["Type"] != "Transfer (Outbound)" || row["Date"] != "2026-03-02" || row["Time"] != "09:30" || row["Value"] != "1000.00" ||
			row["Note"] != "schwabtx A2 ...398 T61001201 to A1 ...135 T61001202: JOURNAL TO ...135" ||
			row["Cash Account"] != "Schwab A2 ...398" || row["Offset Account"] != "Schwab A1 ...135" || row["Ticker Symbol"]+row["WKN"]+row["Security Name"] != "" {
			t.Errorf("the transfer is %v", row)
		}
		for _, r := range ppRows(t, a1) {
			if strings.Contains(r["Note"], "T61001201") || strings.Contains(r["Note"], "T61001202") {
				t.Errorf("A1's file holds a row of the transfer: %v", r)
			}
		}
		for _, c := range []struct {
			f         ppFile
			ref, want string
		}{
			{a1, "T61001203", "schwabtx A1 ...135 T61001203: JOURNAL TO ...398; paired with A2 ...398 T61001204, written apart"},
			{a2, "T61001208", "schwabtx A2 ...398 T61001208: JOURNAL FRM ...135; paired with A1 ...135 T61001207, written apart"},
			{a1, "T61001205", "schwabtx A1 ...135 T61001205: JOURNAL TO ...398"},
		} {
			if row := ppRowOf(t, c.f, c.ref); row["Note"] != c.want || row["Offset Account"] != "" {
				t.Errorf("%s: %v", c.ref, row)
			}
		}
		files = ppFilesOf("transfer_outside")
		row = ppRowOf(t, ppFileNamed(t, files, "account-transactions-A1-135.csv"), "T61001202")
		if row["Type"] != "Deposit" || row["Offset Account"] != "" || row["Note"] != "schwabtx A1 ...135 T61001202: JOURNAL FRM ...398; paired with A2 ...398 T61001201, written apart" {
			t.Errorf("a transfer with an account outside the export is %v", row)
		}
	})

	t.Run("notes", func(t *testing.T) {
		t.Parallel()
		// Legs, a two-row cash event, deliveries and closes with their
		// caveats' words, and an evening fill, which has no Time.
		for _, c := range []struct{ caseName, file, ref, column, want string }{
			{"multi_leg", "portfolio-transactions-A2-398.csv", "T61001401", "Note", "schwabtx A2 ...398 T61001401 leg 4: TRADE"},
			{"two_row", "portfolio-transactions-A1-135.csv", "C0a1b2c3d.2", "Note", "schwabtx A1 ...135 C0a1b2c3d.2 with C0a1b2c3d.3: MERGER"},
			{"deliveries", "portfolio-transactions-A2-398.csv", "T61001508", "Note", "schwabtx A2 ...398 T61001508: JOURNALED SHARES; value from the snapshot"},
			{"deliveries", "portfolio-transactions-A1-135.csv", "Cb0c1d2e3.5", "Note", "schwabtx A1 ...135 Cb0c1d2e3.5: SHARES; value unknown: set it"},
			{"deliveries", "portfolio-transactions-A1-135.csv", "T61001503", "Note", "schwabtx A1 ...135 T61001503: SECURITY TRANSFER; its cash of -25.00 is not in this file"},
			{"deliveries", "portfolio-transactions-A1-135.csv", "T61001503", "Cash Account", ""},
			{"deliveries", "portfolio-transactions-A1-135.csv", "Cb0c1d2e3.3", "Value", "9950.00"},
			{"options", "portfolio-transactions-A2-398.csv", "Cd0e1f2a3.3", "Note", "schwabtx A2 ...398 Cd0e1f2a3.3: OPTION; multiplier 100 assumed; closed at 0"},
			{"options", "portfolio-transactions-A2-398.csv", "Cd0e1f2a3.3", "Ticker Symbol", "XYZ250321C00045000"},
			{"income", "account-transactions-A1-135.csv", "Cc0d1e2f3.3", "Note", "schwabtx A1 ...135 Cc0d1e2f3.3: INCOME; return of capital"},
			{"gf_dates", "portfolio-transactions-A2-398.csv", "T61002504", "Time", ""},
			{"gf_dates", "portfolio-transactions-A2-398.csv", "T61002505", "Time", "10:30"},
		} {
			files := ppFilesOf(c.caseName)
			if got := ppRowOf(t, ppFileNamed(t, files, c.file), c.ref)[c.column]; got != c.want {
				t.Errorf("%s %s: %s %q, want %q", c.caseName, c.ref, c.column, got, c.want)
			}
		}
	})

	t.Run("identifiers", func(t *testing.T) {
		t.Parallel()
		// A class share in Yahoo's form; no ticker for one with a dot or
		// delisted; a renamed security's latest ticker with its CUSIP; a
		// row's symbol without a key; a security only a CUSIP names.
		files := ppFilesOf("securities")
		port := ppFileNamed(t, files, "portfolio-transactions-A1-135.csv")
		for _, c := range []struct{ ref, ticker, wkn, name string }{
			{"T61002001", "ABC-B", "", "ABC HOLDINGS CL B"},
			{"T61002002", "", "", "QRS TRUST PFD SER K"},
			{"T61002003", "", "ZZQ00K102", "OLDCO INC"},
			{"T61002004", "", "ZZQ00K102", "OLDCO INC"},
			{"T61002005", "PQRS", "ZZR00K200", "PQR GROUP INC"},
			{"T61002006", "PQRS", "ZZR00K200", "PQR GROUP INC"},
			{"T61002007", "XYZ", "98389B100", "XYZ CORP COMMON STOCK"},
			{"Ce5f6a7b8.1", "QQQX", "", ""},
			{"Ce5f6a7b8.2", "", "ZZC99X101", "EXAMPLE PRIVATE PLACEMENT UNITS"},
		} {
			row := ppRowOf(t, port, c.ref)
			if row["Ticker Symbol"] != c.ticker || row["WKN"] != c.wkn || row["Security Name"] != c.name {
				t.Errorf("%s names %q %q %q, want %q %q %q", c.ref, row["Ticker Symbol"], row["WKN"], row["Security Name"], c.ticker, c.wkn, c.name)
			}
		}
		// A security that would have no identifier is named by its symbol,
		// else its key; a name is trimmed as PP trims it.
		for _, c := range []struct {
			s                 toolSecurity
			ticker, wkn, name string
		}{
			{toolSecurity{key: "i:1", symbol: "QRS.PRK", tickers: []string{"QRS.PRK"}}, "", "", "QRS.PRK"},
			{toolSecurity{key: "i:2"}, "", "", "i:2"},
			{toolSecurity{key: "i:3", symbol: "XYZ", tickers: []string{"XYZ"}, name: " XYZ CORP\u00a0"}, "XYZ", "", "XYZ CORP"},
			{toolSecurity{key: "i:4", symbol: "XYZ", tickers: []string{"XYZ"}, name: "\u2003\x1cXYZ\u2028 CORP\t\ufeff"}, "XYZ", "", "XYZ\u2028 CORP"},
		} {
			ticker, wkn, name := ppIdentifiers(&c.s)
			if ticker != c.ticker || wkn != c.wkn || name != c.name {
				t.Errorf("%s: %q %q %q, want %q %q %q", c.s.key, ticker, wkn, name, c.ticker, c.wkn, c.name)
			}
		}
	})

	t.Run("prices", func(t *testing.T) {
		t.Parallel()
		// The prices of a security without a feed: a short option held,
		// priced per share; a security with a ticker gets none; a security
		// that a record names, without prices, a row of its own.
		files := ppFilesOf("holdings")
		want := "Ticker Symbol,WKN,Security Name,Currency,Date of Quote,Quote\n" +
			"ABCD,ZZB123458,ABCD INDUSTRIES INC,USD,,\n" +
			"EFGH,,EFGH TOTAL MARKET ETF,USD,,\n" +
			"XYZ,98389B100,XYZ CORP COMMON STOCK,USD,,\n" +
			"XYZ250221P00040000,,XYZ 02/21/2025 40.00 P,USD,,\n" +
			"XYZ260918C00050000,,XYZ 09/18/2026 50.00 C,USD,,\n" +
			"XYZ270115P00045000,,XYZ 01/15/2027 45.00 P,USD,2026-11-10,1.55\n"
		if f := ppFileNamed(t, files, "securities.csv"); string(f.data) != want {
			t.Errorf("securities.csv is\n%s\nwant\n%s", f.data, want)
		}
	})

	t.Run("notes and caveats", func(t *testing.T) {
		t.Parallel()
		// Every caveat that a PP record can carry has its words; each of
		// a pair's caveats names the partner, said once; the text is
		// trimmed as PP trims a cell.
		a1 := toolAccount{id: 1, masked: "...135"}
		e := toolEvent{
			ref: "T1", account: a1, kind: evDeposit, text: "  ACH\tCredit ", amount: -2500,
			pair: toolAccount{id: 2, masked: "...398"}, pairRef: "T2",
			caveats: []string{"pair_amounts_differ", "pair_not_journal", "pair_outside"},
		}
		if got := ppNote(&e); got != "schwabtx A1 ...135 T1: ACH\tCredit; paired with A2 ...398 T2, written apart" {
			t.Errorf("note %q", got)
		}
		for _, cv := range toolCaveats {
			if cv.tool == toolGF {
				continue
			}
			if w := ppCaveatWords(&e, cv.code); w == cv.code || w == "" {
				t.Errorf("the caveat %s has no words", cv.code)
			}
		}
		e.kind, e.caveats = evFees, []string{"fees_apart"}
		f := e
		f.kind = evSell
		if a, b := ppNote(&e), ppNote(&f); a == b {
			t.Errorf("a sale and its Fees row both say %q", a)
		}
	})

	t.Run("csv", func(t *testing.T) {
		t.Parallel()
		// RFC 4180 quotes for a comma, a quote or a line end in a cell, LF
		// line ends, no byte order mark.
		got := string(ppCSV([]string{"A", "B"}, [][]string{{`x, "y"`, "z"}, {"", "a\nb"}}))
		if want := "A,B\n\"x, \"\"y\"\"\",z\n,\"a\nb\"\n"; got != want {
			t.Errorf("csv %q, want %q", got, want)
		}
	})

	t.Run("time", func(t *testing.T) {
		t.Parallel()
		// The time on New York's clocks, to the minute, on a day the clocks
		// change; none for a time whose date is another day.
		at := time.Date(2025, time.March, 9, 7, 30, 59, 0, time.UTC) // 03:30:59 in New York, just after 02:00 became 03:00
		e := toolEvent{ref: "T1", account: toolAccount{id: 1}, kind: evDeposit, day: "2025-03-09", time: at, text: "X"}
		if r := ppRecordOf(&e); r.time != "03:30" {
			t.Errorf("time %q", r.time)
		}
		e.day = "2025-03-10"
		if r := ppRecordOf(&e); r.time != "" {
			t.Errorf("a time of another day is %q", r.time)
		}
	})
}
