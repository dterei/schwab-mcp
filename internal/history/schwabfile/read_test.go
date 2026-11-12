package schwabfile

import (
	"bytes"
	"encoding/json"
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// update rewrites the golden files of TestReadFixtures from what Read
// makes of the fixtures: go test ./internal/history/schwabfile -run
// TestReadFixtures -update. Read the diff before committing it.
var update = flag.Bool("update", false, "rewrite the golden files of TestReadFixtures")

// An indexEntry is a fixture as testdata/index.json lists it.
type indexEntry struct {
	// File is its path under testdata, and Name the name Read is given
	// for it, as the export would be named.
	File, Name string
	// Source says what it is modeled on; every fixture is made by hand,
	// with made-up accounts, names and amounts.
	Source string
	// CR counts its carriage returns, and BOM is whether it starts with a
	// UTF-8 byte order mark, so that a checkout that changed its bytes
	// fails.
	CR  int
	BOM bool
	// Refused, for a file that Read refuses, is what its error says the
	// file is.
	Refused string
}

// readIndex reads testdata/index.json.
func readIndex(t testing.TB) []indexEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []indexEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatal(err)
	}
	return entries
}

// fixture reads a fixture's bytes.
func fixture(t testing.TB, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every fixture under testdata has one entry in the index, but the index,
// the README and the golden files, and each entry a source; each fixture has the
// carriage returns and the byte order mark its entry says, so that a
// checkout that changed its line ends fails here rather than in a test
// that reads it.
func TestFixtureIndex(t *testing.T) {
	entries := readIndex(t)
	indexed := map[string]bool{}
	for _, e := range entries {
		if indexed[e.File] {
			t.Errorf("%s is in the index twice", e.File)
		}
		indexed[e.File] = true
		if e.Source == "" || (e.Name == "") == (e.Refused == "") {
			t.Errorf("%s needs a source, and a name or what it is refused as, not both", e.File)
		}
		b := fixture(t, e.File)
		if n := bytes.Count(b, []byte("\r")); n != e.CR {
			t.Errorf("%s holds %d carriage returns, want %d: has its checkout changed its line ends?", e.File, n, e.CR)
		}
		if bom := bytes.HasPrefix(b, []byte("\xef\xbb\xbf")); bom != e.BOM {
			t.Errorf("%s starts with a byte order mark: %v, want %v", e.File, bom, e.BOM)
		}
	}
	err := fs.WalkDir(os.DirFS("testdata"), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == "index.json" || p == "README.md" ||
			strings.HasPrefix(p, "golden/") || strings.HasPrefix(p, "fuzz/") {
			return err
		}
		if !indexed[p] {
			t.Errorf("%s is not in testdata/index.json", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Each fixture that Read reads reads to its golden file: what it is, its
// layout, encoding and bytes, the account's digits, the time of the
// export, the stated range and totals, every row, the warnings, and the
// days it covers with nothing given and exported as its ExportedAt says.
func TestReadFixtures(t *testing.T) {
	for _, e := range readIndex(t) {
		if e.Refused != "" {
			continue
		}
		t.Run(e.File, func(t *testing.T) {
			f, err := Read(e.Name, fixture(t, e.File))
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			enc := json.NewEncoder(&buf)
			enc.SetEscapeHTML(false)
			enc.SetIndent("", "  ")
			if err := enc.Encode(golden(f)); err != nil {
				t.Fatal(err)
			}
			got := buf.Bytes()
			name := filepath.Join("testdata", "golden", path.Base(e.File)+".json")
			if *update {
				if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
				t.Errorf("%s reads to\n%s\nwant (%s)\n%s", e.File, got, name, want)
			}
		})
	}
}

// A goldenFile is what TestReadFixtures records of a File.
type goldenFile struct {
	Kind                    Kind
	Layout, Encoding        string
	BOM, CRLF, LazyQuotes   bool
	AccountHint, NameDigits string
	ExportedAt              string
	StatedFrom, StatedTo    nytime.Date
	StatedTotal, StatedFees string           `json:",omitempty"`
	History                 []goldenHistory  `json:",omitempty"`
	Realized                []goldenRealized `json:",omitempty"`
	Snapshots               []goldenSnapshot `json:",omitempty"`
	SnapshotSkips           map[string]int   `json:",omitempty"`
	APIRows                 []string         `json:",omitempty"`
	Warnings                []Warning        `json:",omitempty"`
	Coverage                *Coverage        `json:",omitempty"`
}

type goldenHistory struct {
	Ordinal, Line                 int
	Raw                           string
	Posted, AsOf                  nytime.Date
	Action, Symbol, Description   string
	Quantity, Price, Fees, Amount string
	ItemIssueID, AcctgRuleCd      string   `json:",omitempty"`
	Problems                      []string `json:",omitempty"`
}

type goldenRealized struct {
	Ordinal, Line                                                 int
	Raw                                                           string
	Symbol, Name                                                  string
	Closed, Opened                                                nytime.Date
	OpenedText                                                    string
	Quantity, ProceedsPerShare, CostPerShare, Proceeds, CostBasis string
	Gain, GainPct, LTGain, STGain, Term, UnadjustedCostBasis      string
	WashSale                                                      *bool
	DisallowedLoss                                                string
	TxnClosed                                                     nytime.Date
	TxnCostBasis, TxnGain, TxnGainPct, LTTxnGain, LTTxnGainPct    string
	STTxnGain, STTxnGainPct                                       string
	Placeholders                                                  []string `json:",omitempty"`
	ColumnsDisagree                                               bool
	Problems                                                      []string `json:",omitempty"`
}

type goldenSnapshot struct {
	Line      int
	FetchedAt string
	Accounts  []string
}

// dec writes a Decimal as the text it was read from and what Read made of
// it.
func dec(d Decimal) string {
	switch {
	case !d.Present:
		return ""
	case d.Placeholder != "":
		return d.Text + " => placeholder"
	case !d.Number():
		return d.Text + " => not a number"
	}
	return d.Text + " => " + d.Canon()
}

// texts returns JSON values as text, which the golden files hold as
// strings: json.Encoder writes a json.RawMessage with U+2028 escaped on
// Go 1.27 and as it is on Go 1.26, and a string escaped on both.
func texts(raws []json.RawMessage) []string {
	var out []string
	for _, r := range raws {
		out = append(out, string(r))
	}
	return out
}

// golden returns what TestReadFixtures records of f.
func golden(f *File) goldenFile {
	g := goldenFile{
		Kind: f.Kind, Layout: f.Layout, Encoding: f.Encoding, BOM: f.BOM, CRLF: f.CRLF, LazyQuotes: f.LazyQuotes,
		AccountHint: f.AccountHint, NameDigits: f.NameDigits, StatedFrom: f.StatedFrom, StatedTo: f.StatedTo,
		SnapshotSkips: f.SnapshotSkips, APIRows: texts(f.APIRows), Warnings: f.Warnings,
	}
	if !f.ExportedAt.IsZero() {
		g.ExportedAt = f.ExportedAt.Format(time.RFC3339)
	}
	if f.StatedTotal != nil {
		g.StatedTotal = dec(*f.StatedTotal)
	}
	if f.StatedFees != nil {
		g.StatedFees = dec(*f.StatedFees)
	}
	for _, r := range f.History {
		g.History = append(g.History, goldenHistory{
			Ordinal: r.Ordinal, Line: r.Line, Raw: r.Raw, Posted: r.Posted, AsOf: r.AsOf,
			Action: r.Action, Symbol: r.Symbol, Description: r.Description,
			Quantity: dec(r.Quantity), Price: dec(r.Price), Fees: dec(r.Fees), Amount: dec(r.Amount),
			ItemIssueID: r.ItemIssueID, AcctgRuleCd: r.AcctgRuleCd, Problems: r.Problems,
		})
	}
	for _, r := range f.Realized {
		g.Realized = append(g.Realized, goldenRealized{
			Ordinal: r.Ordinal, Line: r.Line, Raw: r.Raw, Symbol: r.Symbol, Name: r.Name,
			Closed: r.Closed, Opened: r.Opened, OpenedText: r.OpenedText,
			Quantity: dec(r.Quantity), ProceedsPerShare: dec(r.ProceedsPerShare), CostPerShare: dec(r.CostPerShare),
			Proceeds: dec(r.Proceeds), CostBasis: dec(r.CostBasis), Gain: dec(r.Gain), GainPct: r.GainPct,
			LTGain: dec(r.LTGain), STGain: dec(r.STGain), Term: r.Term, UnadjustedCostBasis: dec(r.UnadjustedCostBasis),
			WashSale: r.WashSale, DisallowedLoss: dec(r.DisallowedLoss), TxnClosed: r.TxnClosed,
			TxnCostBasis: dec(r.TxnCostBasis), TxnGain: dec(r.TxnGain), TxnGainPct: r.TxnGainPct,
			LTTxnGain: dec(r.LTTxnGain), LTTxnGainPct: r.LTTxnGainPct, STTxnGain: dec(r.STTxnGain),
			STTxnGainPct: r.STTxnGainPct, Placeholders: r.Placeholders, ColumnsDisagree: r.ColumnsDisagree,
			Problems: r.Problems,
		})
	}
	for _, s := range f.Snapshots {
		g.Snapshots = append(g.Snapshots, goldenSnapshot{s.Line, s.FetchedAt.Format(time.RFC3339), texts(s.Accounts)})
	}
	switch f.Kind {
	case KindHistoryCSV, KindHistoryJSON, KindRealized:
		c := f.Coverage("", "", nytime.DateOf(f.ExportedAt))
		g.Coverage = &c
	}
	return g
}

// Each file of testdata/refuse is refused, with an error that names the
// file, its free text masked, and says what it is.
func TestRefusals(t *testing.T) {
	for _, e := range readIndex(t) {
		if e.Refused == "" {
			continue
		}
		t.Run(e.File, func(t *testing.T) {
			name := "Joint 12345678 " + path.Base(e.File)
			f, err := Read(name, fixture(t, e.File))
			if err == nil {
				t.Fatalf("read as %s", f.Kind)
			}
			want := "Joint ...678 " + path.Base(e.File) + " "
			if msg := err.Error(); !strings.HasPrefix(msg, want) || !strings.Contains(msg, e.Refused) {
				t.Errorf("refused with %q, want it to start %q and say %q", msg, want, e.Refused)
			}
		})
	}
}

// The files that are not CSV are told by their first character, and the
// JSON ones by their keys, whatever their names; a file whose content
// breaks its kind's syntax is refused, not guessed at.
func TestRefusedByContent(t *testing.T) {
	for content, want := range map[string]string{
		"  \r\n\t":                         "is empty",
		"\xef\xbb\xbf":                     "is empty",
		`{"FromDate": "01/01/2026"`:        "is not a JSON object",
		`{"BrokerageTransactions": 5}`:     "BrokerageTransactions is not a list",
		`{"BrokerageTransactions": []} {}`: "is not a JSON object",
		`{"FromDate": "2026-01-01", "BrokerageTransactions": []}`:        `FromDate "...101" is not MM/DD/YYYY`,
		`{"FromDate": 5, "BrokerageTransactions": []}`:                   `FromDate "5" is not MM/DD/YYYY`,
		`{"ToDate": {}, "BrokerageTransactions": []}`:                    "ToDate is not text",
		`{"Transactions": [{"Date": "01/02/2026"}]}`:                     "a JSON file that is not a History export",
		`[{"activityId": 1}, {"type": "TRADE"}]`:                         "not `schwabtx transactions -json` output",
		`[{"activityId": 1}, 2]`:                                         "not `schwabtx transactions -json` output",
		`[{"activityId": 1}`:                                             "not `schwabtx transactions -json` output",
		"\"Date\",\"Action\"\n\"a\"b\"\"\n":                              "not a file schwabtx knows",
		"\"Transactions  for account XXXX-1234 as of 12/20/2014\"\n":     "has a first line and nothing else",
		"\"Transactions for account ...123 as of x ET\"\n\"A\",\"B\"":    `has no "Date" column; it is not a History export`,
		"\"Realized Gain/Loss for ...123\"\n\"A\",\"B\"":                 `has no "Symbol" column; it is not a Realized Gain/Loss Lot Details`,
		"\"\",\"\"\n,,\n":                                                "not a file schwabtx knows",
		"\"Date\",\"Action\",\"Symbol\",\"Amount\",\"Price\"\n":          `has no "Description" column`,
		"\"Date\",\"Action\",\"Symbol\",\"Amount\"\n":                    `has no "Description" column; it is not a History export`,
		"\"Date\",\"Action\",\"Amount\"\n":                               "not a file schwabtx knows",
		"\"Transactions  for account ...123 as of x ET\"\n" + lotsHeader: `has no "Date" column; it is not a History export`,
		halfLots(12):    `has no "Symbol" column; it is not a Realized Gain/Loss Lot Details`,
		halfLots(11):    "not a file schwabtx knows",
		"<html></html>": "an XML export",
		"Hello":         "not a file schwabtx knows",
	} {
		f, err := Read("x.json", []byte(content))
		if err == nil {
			t.Errorf("%q read as %s", content, f.Kind)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q refused with %q, want %q", content, err, want)
		}
	}
}

// halfLots is a Lot Details header of only its last n columns, with no
// title.
func halfLots(n int) string {
	var names []string
	for _, c := range lotColumns[len(lotColumns)-n:] {
		names = append(names, `"`+c[0]+`"`)
	}
	return strings.Join(names, ",") + "\n"
}

// A History header is bound by name, in any case and order, with padding
// and empty cells around it, so that Price before Quantity reads as
// Quantity before Price, and cells beyond a short record are blank; a
// header without one of the eight is refused by that name.
func TestHeaderByName(t *testing.T) {
	content := " PRICE ,amount,Fees & Comm,quantity,Date,Action,Description,Symbol,,\n" +
		"$1.50,-$15.00,,10,01/02/2026,Buy,XYZ CORP,XYZ,,\n" +
		"$2.00,$4.00,$0.01,2,01/03/2026,Sell\n"
	f, err := Read("x.csv", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.History) != 2 {
		t.Fatalf("read %d rows, want 2", len(f.History))
	}
	r := f.History[0]
	if r.Price.Canon() != "1.5" || r.Quantity.Canon() != "10" || r.Amount.Canon() != "-15" || r.Fees.Present ||
		r.Symbol != "XYZ" || r.Description != "XYZ CORP" || r.Action != "Buy" || r.Posted != "2026-01-02" {
		t.Errorf("row 1 reads as %+v", r)
	}
	r = f.History[1]
	if r.Symbol != "" || r.Description != "" || r.Fees.Canon() != "0.01" || r.Quantity.Canon() != "2" || len(r.Problems) != 0 {
		t.Errorf("row 2, short of the last two cells, reads as %+v", r)
	}
	for _, missing := range []string{"Date", "Action", "Symbol", "Description", "Quantity", "Price", "Fees & Comm", "Amount"} {
		var header []string
		for _, c := range historyColumns {
			if c[0] != missing {
				header = append(header, `"`+c[0]+`"`)
			}
		}
		_, err := Read("x.csv", []byte(strings.Join(header, ",")+"\n"))
		if err == nil || !strings.Contains(err.Error(), `has no "`+missing+`" column`) {
			t.Errorf("a header without %s is refused with %v", missing, err)
		}
	}
}

// The account's digits come from a banner or a title, else from the
// file's name; NameDigits are the name's alone, whatever the content
// says; a name, banner or title without them gives none.
func TestHints(t *testing.T) {
	header := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n"
	lots := lotsHeader
	for _, c := range []struct {
		name, content, hint, nameDigits string
	}{
		{"Individual_XXX123_Transactions_20260101-120000.csv", header, "123", "123"},
		{`C:\Users\me\Downloads\Joint_XXX456_TRANSACTIONS_20260101-120000.CSV`, header, "456", "456"},
		{"Brokerage_Transactions_20201115-180021.CSV", header, "", ""},
		{"XXX123_Transactions.csv", header, "", ""},
		{"Individual_XXX12_Transactions_20260101-120000.csv", header, "", ""},
		{"XXXX1234_GainLoss_Realized_Details_20260706-104711.csv", lots, "1234", "1234"},
		{`C:\dl\XXXX1234_GainLoss_Realized_Details_20260706-104711.csv`, lots, "1234", "1234"},
		{"dl/XXXX1234_GainLoss_Realized_Details_20260706-104711.csv", lots, "1234", "1234"},
		{"dl/Joint_XXXX1234_GainLoss_Realized_Details_20260706-104711.csv", lots, "", ""},
		{
			"Joint_XXX456_Transactions_20260101-120000.csv",
			`"Transactions  for account XXXX-1234 as of 12/20/2014 21:16:49 ET"` + "\n" + header, "1234", "456",
		},
		{"x.csv", `"Transactions  for account Joint ...456 as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "456", ""},
		{"x.csv", `"Transactions  for account Brokerage as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "", ""},
		{"x.csv", `"Transactions  for account Trust 12345678 as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "12345678", ""},
		{"x.csv", `"Transactions  for account Trust 2 as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "", ""},
		{"x.csv", `"Transactions  for account ...456 Joint as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "", ""},
		{"x.csv", `"Transactions  for account 12345678 Trust as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "", ""},
		{"x.csv", `"Transactions  for account Trust 1234567890 as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "1234567890", ""},
		{"x.csv", `"Transactions  for account Trust1234567890 as of 05/03/2022 02:21:05 PM ET"` + "\n" + header, "", ""},
		{"x.csv", `"Realized Gain/Loss - Lot Details for Joint …456 as of Mon Jul 06 10:47:11 EDT 2026 from 01/01/2026 to 07/06/2026"` +
			"\n" + lots, "456", ""},
		{"x.csv", `"Realized Gain/Loss - Lot Details for Joint XXX456 as of Mon Jul 06 10:47:11 EDT 2026 from 01/01/2026 to 07/06/2026"` +
			"\n" + lots, "456", ""},
	} {
		f, err := Read(c.name, []byte(c.content))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if f.AccountHint != c.hint || f.NameDigits != c.nameDigits {
			t.Errorf("%s with %.40q: hint %q and name's digits %q, want %q and %q",
				c.name, c.content, f.AccountHint, f.NameDigits, c.hint, c.nameDigits)
		}
	}
}

// The time of an export comes from a banner (24-hour or 12-hour, New York
// time), a title (Java's form, any run of spaces, its zone taken for New
// York's), else from the name's stamp, as New York time; a stamp that is
// not a time gives none.
func TestExportedAt(t *testing.T) {
	header := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n"
	for _, c := range []struct {
		name, content, want string
	}{
		{"x.csv", `"Transactions  for account XXXX-1234 as of 12/20/2014 21:16:49 ET"` + "\n" + header, "2014-12-21T02:16:49Z"},
		{"x.csv", `"Transactions  for account ...456 as of 7/3/2022 02:21:05 pm ET"` + "\n" + header, "2022-07-03T18:21:05Z"},
		{"x.csv", `"Transactions  for account ...456 as of 05/03/2022 12:21:05 AM ET"` + "\n" + header, "2022-05-03T04:21:05Z"},
		{"x.csv", `"Transactions  for account ...123 as of 05/03/2023 2:21:05 PM ET"` + "\n" + header, "2023-05-03T18:21:05Z"},
		{"x.csv", `"Transactions  for account ...123 as of 05/03/2023 9:21:05 ET"` + "\n" + header, "2023-05-03T13:21:05Z"},
		{"a_20260706-104711.csv", header, "2026-07-06T14:47:11Z"},
		{"a_20260115-104711.json", `{"BrokerageTransactions": []}`, "2026-01-15T15:47:11Z"},
		{"a_20261341-250000.csv", header, ""},
		{"a_20260706-104711.csv.bak", header, ""},
		{"x_20200101-000000.csv", `"Realized Gain/Loss - Lot Details for XXXX-1234 as of Mon  Jul 06   10:47:11 EDT 2026 ` +
			`from 01/01/2026 to 07/06/2026"` + "\n" + lotsHeader, "2026-07-06T14:47:11Z"},
		{"x_20200101-000000.csv", `"Realized Gain/Loss - Lot Details for XXXX-1234 as of July 6 ` +
			`from 01/01/2026 to 07/06/2026"` + "\n" + lotsHeader, "2020-01-01T05:00:00Z"},
	} {
		f, err := Read(c.name, []byte(c.content))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		got := ""
		if !f.ExportedAt.IsZero() {
			got = f.ExportedAt.Format(time.RFC3339)
		}
		if got != c.want {
			t.Errorf("%s with %.50q: exported at %q, want %q", c.name, c.content, got, c.want)
		}
	}
	for s, want := range map[string]string{
		"Mon Jul 06 10:47:11 EDT 2026":    "2026-07-06T14:47:11Z",
		"Mon Jul 06  10:47:11  EDT 2026":  "2026-07-06T14:47:11Z",
		"Thu Jan 15 10:47:11 EST 2026":    "2026-01-15T15:47:11Z",
		"Thu Jan 15 10:47:11 GMT 2026":    "2026-01-15T15:47:11Z",
		"Jan 15 10:47:11 EST 2026":        "",
		"Thu Jan 32 10:47:11 EST 2026":    "",
		"Thu Jan 15 10:47:11 EST 2026 xx": "",
	} {
		got := ""
		if t := javaTime(s); !t.IsZero() {
			got = t.Format(time.RFC3339)
		}
		if got != want {
			t.Errorf("javaTime(%q) = %q, want %q", s, got, want)
		}
	}
}

// What Read gives is the same each time, and every row's Raw is in the
// text it read, line ends and all.
func TestRawIsInTheText(t *testing.T) {
	for _, e := range readIndex(t) {
		if e.Refused != "" {
			continue
		}
		b := fixture(t, e.File)
		f, err := Read(e.Name, b)
		if err != nil {
			t.Fatal(err)
		}
		text, _, _ := decode(b)
		for _, r := range f.History {
			if !strings.Contains(text, r.Raw) || r.Raw == "" {
				t.Errorf("%s: row %d's Raw %q is not in its text", e.File, r.Ordinal, r.Raw)
			}
			if line := strings.Split(text, "\n")[r.Line-1]; !strings.HasPrefix(r.Raw, strings.TrimSpace(line)) {
				t.Errorf("%s: row %d starts on line %d, %q, not with its Raw %q", e.File, r.Ordinal, r.Line, line, r.Raw)
			}
		}
		for _, r := range f.Realized {
			if !strings.Contains(text, r.Raw) || r.Raw == "" {
				t.Errorf("%s: lot %d's Raw %q is not in its text", e.File, r.Ordinal, r.Raw)
			}
		}
	}
}

// A CSV file that breaks the rules of quoting is read again leniently, and
// says so; Windows-1252 is decoded and said; a BOM and CR LF are passed
// over and said.
func TestBytes(t *testing.T) {
	header := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"`
	f, err := Read("x.csv", []byte(header+"\n"+`"01/02/2026","Buy","XYZ","XYZ "THE" CORP","1","$1.00","","-$1.00"`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.LazyQuotes || len(f.Warnings) == 0 || f.Warnings[0].Code != WarnLazyQuotes ||
		f.History[0].Description != `XYZ "THE" CORP` {
		t.Errorf("a bare quote reads as lazy %v, warnings %v, description %q",
			f.LazyQuotes, f.Warnings, f.History[0].Description)
	}
	f, err = Read("x.csv", []byte("\xef\xbb\xbf"+header+"\r\n"+
		"\"01/02/2026\",\"Buy\",\"XYZ\",\"CAF\xc9 \x85\",\"1\",\"$1.00\",\"\",\"-$1.00\"\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.BOM || !f.CRLF || f.Encoding != "windows-1252" || f.History[0].Description != "CAFÉ …" ||
		f.Warnings[0].Code != WarnEncodingFallback {
		t.Errorf("Windows-1252 with a BOM and CR LF reads as %+v", f)
	}
	f, err = Read("x.csv", []byte(header+"\n"+"\"01/02/2026\",\"Buy\",\"XYZ\",\"A\rB\",\"1\",\"$1.00\",\"\",\"-$1.00\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.BOM || f.CRLF || f.Encoding != "utf-8" || f.History[0].Description != "A\rB" || len(f.Warnings) != 0 {
		t.Errorf("a carriage return inside a cell reads as %+v", f)
	}
}

// Decoding Windows-1252 maps each of the 256 bytes to one character:
// ASCII and Latin-1 to themselves, 0x80-0x9F by Windows' table, the five
// bytes it leaves undefined to the control characters of their number.
func TestCP1252(t *testing.T) {
	var all []byte
	for b := range 256 {
		all = append(all, byte(b))
	}
	got := []rune(decodeCP1252(all))
	if len(got) != 256 {
		t.Fatalf("256 bytes decode to %d characters", len(got))
	}
	for b, r := range got {
		switch {
		case b < 0x80 || b >= 0xA0:
			if r != rune(b) {
				t.Errorf("byte %#x decodes to %U, want %U", b, r, rune(b))
			}
		case slices.Contains([]int{0x81, 0x8D, 0x8F, 0x90, 0x9D}, b):
			if r != rune(b) {
				t.Errorf("undefined byte %#x decodes to %U, want %U", b, r, rune(b))
			}
		case r < 0x100:
			t.Errorf("byte %#x decodes to %U, which is not Windows' character", b, r)
		}
	}
	for b, want := range map[byte]rune{0x80: '€', 0x85: '…', 0x92: '’', 0x96: '–', 0x9F: 'Ÿ', 0xC9: 'É', 0xA0: '\u00a0'} {
		if got := []rune(decodeCP1252([]byte{b})); got[0] != want {
			t.Errorf("byte %#x decodes to %U, want %U", b, got[0], want)
		}
	}
	if text, enc, _ := decode([]byte("CAFÉ")); text != "CAFÉ" || enc != "utf-8" {
		t.Errorf("UTF-8 decodes to %q as %s", text, enc)
	}
}

// A History row's Date is a date, or a date "as of" another, with a month
// and a day of one or two digits; anything else leaves the row undated,
// with a problem, and the file warns.
func TestHistoryDates(t *testing.T) {
	for s, want := range map[string][2]nytime.Date{
		"03/16/2026":                   {"2026-03-16", ""},
		"3/6/2026":                     {"2026-03-06", ""},
		"03/16/2026 as of 03/13/2026":  {"2026-03-16", "2026-03-13"},
		"03/16/2026  AS OF  3/13/2026": {"2026-03-16", "2026-03-13"},
		"03/16/2026 as of":             {"", ""},
		"03/16/2026 as of 02/30/2026":  {"", ""},
		"02/30/2026":                   {"", ""},
		"2026-03-16":                   {"", ""},
		"03/16/26":                     {"", ""},
		"":                             {"", ""},
		"Pending":                      {"", ""},
	} {
		posted, asOf := parseHistoryDate(s)
		if posted != want[0] || asOf != want[1] {
			t.Errorf("parseHistoryDate(%q) = %q, %q, want %q, %q", s, posted, asOf, want[0], want[1])
		}
	}
	header := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n"
	f, err := Read("x.csv", []byte(header+`"13/45/2025","Buy","","","","","","$1.00"`+"\n"+
		`"Pending 12345678","Buy","","","","","","abc"`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{`Date "13/45/2025" is not MM/DD/YYYY`},
		{`Date "Pending ...678" is not MM/DD/YYYY`, `Amount "abc" is not a number`},
	}
	for i, r := range f.History {
		if !slices.Equal(r.Problems, want[i]) || r.Posted != "" {
			t.Errorf("row %d's problems are %q, want %q", i+1, r.Problems, want[i])
		}
	}
	if !slices.ContainsFunc(f.Warnings, func(w Warning) bool { return w.Code == WarnUndatedRows }) {
		t.Errorf("warnings %v, want %s", f.Warnings, WarnUndatedRows)
	}
}

// Option symbols as files write them read to the contract that the OCC
// symbol of the same option does; what is not one, or what an OCC symbol
// cannot write, is not one.
func TestOptionSymbols(t *testing.T) {
	for file, occ := range map[string]string{
		"XYZ 09/04/2026 17.50 P":      "XYZ   260904P00017500",
		"  XYZ  9/4/2026  17.5  P ":   "XYZ   260904P00017500",
		"ABCD 11/20/2026 20.00 C":     "ABCD  261120C00020000",
		"SPXW 12/31/2026 5000.00 C":   "SPXW  261231C05000000",
		"XYZ1 01/15/2027 5.00 C":      "XYZ1  270115C00005000",
		"ABCDEF 01/15/2027 0.125 P":   "ABCDEF270115P00000125",
		"XYZ 01/15/2027 99999.999 C":  "XYZ   270115C99999999",
		"XYZ 01/15/2027 17 C":         "XYZ   270115C00017000",
		"XYZ 01/15/2027 17. C":        "XYZ   270115C00017000",
		"XYZ 01/15/2027 17.5000000 C": "XYZ   270115C00017500",
		"XYZ 12/31/2068 1.00 C":       "XYZ   681231C00001000",
		"XYZ 01/01/1969 1.00 C":       "XYZ   690101C00001000",
	} {
		got, ok := ParseOption(file)
		want, wantOK := schwab.ParseOCC(occ)
		if !ok || !wantOK || got != want {
			t.Errorf("ParseOption(%q) = %+v, %v; ParseOCC(%q) = %+v, %v", file, got, ok, occ, want, wantOK)
		}
	}
	for _, s := range []string{
		"XYZ", "", "XYZ 09/04/2026 17.50", "XYZ 09/04/2026 17.50 X", "xyz 09/04/2026 17.50 P",
		"XYZ 02/30/2026 17.50 P", "XYZ 09/04/26 17.50 P", "XYZ 09/04/2026 17.5005 P", "XYZ 09/04/2026 100000 P",
		"ABCDEFG 09/04/2026 17.50 P", "XYZ 09/04/2026 $17.50 P", "XYZ 09/04/2026 -1 P", "XYZ   260904P00017500",
		"000000AA7", "XYZ 09/04/1968 17.50 P", "XYZ 09/04/2069 17.50 P",
	} {
		if got, ok := ParseOption(s); ok {
			t.Errorf("ParseOption(%q) = %+v, want none", s, got)
		}
	}
}

// lotsHeader is the header of Realized Gain/Loss's Lot Details.
const lotsHeader = `"Symbol","Name","Closed Date","Opened Date","Quantity","Proceeds Per Share","Cost Per Share",` +
	`"Proceeds","Cost Basis (CB)","Gain/Loss ($)","Gain/Loss (%)","Long Term Gain/Loss","Short Term Gain/Loss",` +
	`"Term","Unadjusted Cost Basis","Wash Sale?","Disallowed Loss","Transaction Closed Date",` +
	`"Transaction Cost Basis","Total Transaction Gain/Loss ($)","Total Transaction Gain/Loss (%)",` +
	`"LT Transaction Gain/Loss ($)","LT Transaction Gain/Loss (%)","ST Transaction Gain/Loss ($)",` +
	`"ST Transaction Gain/Loss (%)"` + "\n"
