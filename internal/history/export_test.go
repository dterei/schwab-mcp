package history

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// exportSnapshot is a small snapshot of fileAccount: its cash, one
// position of XYZ and its liquidation values.
const exportSnapshot = `{"securitiesAccount": {"type": "CASH", "accountNumber": "` + fileAccount + `", "roundTrips": 0,
	"positions": [{"shortQuantity": 0.0, "averagePrice": 10.1, "longQuantity": 15.0, "marketValue": 165.75,
		"instrument": {"assetType": "EQUITY", "cusip": "988888101", "symbol": "XYZ", "instrumentId": 7003}}],
	"initialBalances": {"cashBalance": 1000.0, "liquidationValue": 1100.25},
	"currentBalances": {"cashBalance": 1234.56, "liquidationValue": 1400.31}}}`

// exportStore returns a store of fileAccount with a History export in JSON
// of January, before the API's history; a Realized Gain/Loss export; a
// complete read of the API's transactions of February; a History export
// in CSV of two February days, whose interest the API's rows lack, which
// is held out, and whose dividend either of two API rows could be, which
// is uncertain; and a snapshot of it and of another account.
func exportStore(t *testing.T) *Store {
	t.Helper()
	s := newStore(t, Options{})
	importFiles(t, s, ImportOptions{Account: fileAccount},
		exportName("20260201-090000", "json"), historyJSON("01/01/2026", "01/31/2026", nil,
			csvRow{"01/05/2026", "Buy", "XYZ", "XYZ CORP", "10", "$10.00", "$1.00", "-$101.00"},
			csvRow{"01/09/2026", "Qualified Dividend", "ABCD", "ABCD INC", "", "", "", "$3.21"},
			csvRow{"01/20/2026", "Wire Funds", "", "WIRED FUNDS TO 1234-5678", "", "", "", "-$500.00"}),
		"XXXX0398_GainLoss_Realized_Details_20260301-090000.csv", realizedCSV("Sun Mar 01 09:00:00 EST 2026", "01/01/2026", "03/01/2026",
			[9]string{"XYZ", "02/20/2026", "01/05/2026", "5", "$60.00", "$50.50", "$9.50", "No", ""}))
	record(t, s, listRead(fileAccount, at(time.February, 1), at(time.March, 1), at(time.March, 1),
		decodeRow(t, apiRaw(1002, at(time.February, 10), "TRADE", -55.5, "XYZ", 5, "")),
		decodeRow(t, apiRaw(1001, at(time.February, 12), "DIVIDEND_OR_INTEREST", 1.5, "", 0, "Ordinary Dividend~XYZ")),
		decodeRow(t, apiRaw(1003, at(time.February, 13), "DIVIDEND_OR_INTEREST", 2, "", 0, "Ordinary Dividend~EFGH")),
		decodeRow(t, apiRaw(1004, at(time.February, 15), "DIVIDEND_OR_INTEREST", 2, "", 0, "Ordinary Dividend~EFGH"))))
	importFiles(t, s, ImportOptions{Account: fileAccount}, exportName("20260301-090000", "csv"), historyCSV(
		csvRow{"02/11/2026", "Credit Interest", "", "SCHWAB1 INT 01/30-02/27", "", "", "", "$0.07"},
		csvRow{"02/14/2026", "Qualified Dividend", "EFGH", "EFGH INC", "", "", "", "$2.00"}))
	importFile(t, s, "snapshots.jsonl", snapshotLine(t, time.Date(2026, time.March, 3, 22, 0, 0, 0, time.UTC),
		json.RawMessage(exportSnapshot), json.RawMessage(`{"securitiesAccount": {"type": "MARGIN", "accountNumber": "22220135"}}`)))
	return s
}

// The ledger of exportStore, as an export writes it in CSV and in JSON
// Lines: counted entries only, the held-out and the uncertain left out, in
// order of day; money with two places, quantities and prices as briefly as they
// read, times in RFC 3339 without a fraction, the description masked.
const (
	ledgerCSV = `ref,account_id,account,source,day,time,kind,description,amount,fees,gross,quantity,quantity_as_of,price,symbol,security_key,category,pair_kind,pair_ref,pair_account_id,flags
C52b4daba.1,1,...398,file,2026-01-05,,Buy,XYZ CORP,-101.00,1.00,-100.00,10,2026-02-01,10,XYZ,s:FILE:XYZ@2026-02-01,buy,,,,
C52b4daba.2,1,...398,file,2026-01-09,,Qualified Dividend,ABCD INC,3.21,0.00,3.21,,2026-02-01,,ABCD,s:FILE:ABCD@2026-02-01,dividend,,,,
C52b4daba.3,1,...398,file,2026-01-20,,Wire Funds,WIRED FUNDS TO ...678,-500.00,0.00,-500.00,,2026-02-01,,,,withdrawal,,,,
T1002,1,...398,api,2026-02-10,2026-02-10T20:30:00Z,TRADE,,-55.50,0.00,-55.50,5,,1,XYZ,i:7003,buy,,,,
T1001,1,...398,api,2026-02-12,2026-02-12T20:30:00Z,DIVIDEND_OR_INTEREST,Ordinary Dividend~XYZ,1.50,0.00,1.50,,,,XYZ,i:7003,dividend,,,,
T1003,1,...398,api,2026-02-13,2026-02-13T20:30:00Z,DIVIDEND_OR_INTEREST,Ordinary Dividend~EFGH,2.00,0.00,2.00,,,,EFGH,s:FILE:EFGH@2026-03-01,dividend,,,,security_by_name
T1004,1,...398,api,2026-02-15,2026-02-15T20:30:00Z,DIVIDEND_OR_INTEREST,Ordinary Dividend~EFGH,2.00,0.00,2.00,,,,EFGH,s:FILE:EFGH@2026-03-01,dividend,,,,security_by_name
`
	ledgerJSONL = `{"ref":"C52b4daba.1","account_id":1,"account":"...398","source":"file","day":"2026-01-05","time":null,"kind":"Buy","description":"XYZ CORP","amount":-101.00,"fees":1.00,"gross":-100.00,"quantity":10,"quantity_as_of":"2026-02-01","price":10,"symbol":"XYZ","security_key":"s:FILE:XYZ@2026-02-01","category":"buy","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"C52b4daba.2","account_id":1,"account":"...398","source":"file","day":"2026-01-09","time":null,"kind":"Qualified Dividend","description":"ABCD INC","amount":3.21,"fees":0.00,"gross":3.21,"quantity":null,"quantity_as_of":"2026-02-01","price":null,"symbol":"ABCD","security_key":"s:FILE:ABCD@2026-02-01","category":"dividend","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"C52b4daba.3","account_id":1,"account":"...398","source":"file","day":"2026-01-20","time":null,"kind":"Wire Funds","description":"WIRED FUNDS TO ...678","amount":-500.00,"fees":0.00,"gross":-500.00,"quantity":null,"quantity_as_of":"2026-02-01","price":null,"symbol":"","security_key":null,"category":"withdrawal","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"T1002","account_id":1,"account":"...398","source":"api","day":"2026-02-10","time":"2026-02-10T20:30:00Z","kind":"TRADE","description":null,"amount":-55.50,"fees":0.00,"gross":-55.50,"quantity":5,"quantity_as_of":null,"price":1,"symbol":"XYZ","security_key":"i:7003","category":"buy","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"T1001","account_id":1,"account":"...398","source":"api","day":"2026-02-12","time":"2026-02-12T20:30:00Z","kind":"DIVIDEND_OR_INTEREST","description":"Ordinary Dividend~XYZ","amount":1.50,"fees":0.00,"gross":1.50,"quantity":null,"quantity_as_of":null,"price":null,"symbol":"XYZ","security_key":"i:7003","category":"dividend","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":null}
{"ref":"T1003","account_id":1,"account":"...398","source":"api","day":"2026-02-13","time":"2026-02-13T20:30:00Z","kind":"DIVIDEND_OR_INTEREST","description":"Ordinary Dividend~EFGH","amount":2.00,"fees":0.00,"gross":2.00,"quantity":null,"quantity_as_of":null,"price":null,"symbol":"EFGH","security_key":"s:FILE:EFGH@2026-03-01","category":"dividend","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":"security_by_name"}
{"ref":"T1004","account_id":1,"account":"...398","source":"api","day":"2026-02-15","time":"2026-02-15T20:30:00Z","kind":"DIVIDEND_OR_INTEREST","description":"Ordinary Dividend~EFGH","amount":2.00,"fees":0.00,"gross":2.00,"quantity":null,"quantity_as_of":null,"price":null,"symbol":"EFGH","security_key":"s:FILE:EFGH@2026-03-01","category":"dividend","pair_kind":null,"pair_ref":null,"pair_account_id":null,"flags":"security_by_name"}
`
)

// exportOf exports o from s, and fails the test if the export is refused.
func exportOf(t *testing.T, s *Store, o ExportOptions) *Export {
	t.Helper()
	e, err := s.Export(t.Context(), o)
	if err != nil {
		t.Fatalf("exporting %+v: %v", o, err)
	}
	return e
}

// exported returns what e's one table writes.
func exported(t *testing.T, e *Export) string {
	t.Helper()
	var b bytes.Buffer
	n, err := e.WriteTo(&b)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(b.Len()) {
		t.Errorf("WriteTo says it wrote %d bytes, not %d", n, b.Len())
	}
	return b.String()
}

// csvCell returns s as a CSV cell, quoted.
func csvCell(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// tableOf returns e's table named name.
func tableOf(t *testing.T, e *Export, name string) ExportedTable {
	t.Helper()
	for _, tb := range e.Tables {
		if tb.Name == name {
			return tb
		}
	}
	t.Fatalf("the export has no table %s", name)
	return ExportedTable{}
}

// readOutput returns the contents of an export's file at path.
func readOutput(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// exportTemps returns the temporary files of exports left in dir.
func exportTemps(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, ".schwabtx-export-*"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// An export (18 of the design) writes a view's rows as golden CSV and JSON
// Lines, the same bytes each time, and each row once though its read ran
// twice; -raw adds Schwab's bytes, a file's row and a snapshot's element as
// the store holds them, and -show-accounts the number after the masked
// one, and without them no cell holds a full number. -account, -from, -to
// and -all filter as they say. A file is written readable only by you
// through a temporary file renamed over it, which a write that fails
// leaves as it was; a directory gets each table and a manifest; and an
// output that is the store, one of its files or a link to it is refused.
func TestExport(t *testing.T) {
	t.Parallel()
	s := exportStore(t)
	ledger := exportOf(t, s, ExportOptions{})
	if got := exported(t, ledger); got != ledgerCSV {
		t.Errorf("the ledger in CSV is\n%s\nwant\n%s", got, ledgerCSV)
	}
	if got := exported(t, exportOf(t, s, ExportOptions{Format: "jsonl"})); got != ledgerJSONL {
		t.Errorf("the ledger in JSON Lines is\n%s\nwant\n%s", got, ledgerJSONL)
	}
	if ledger.Waiting != 2 || ledger.HoldsNumbers || ledger.Format != "csv" || ledger.At != testNow {
		t.Errorf("the ledger's export says Waiting %d, HoldsNumbers %v, Format %q, At %v", ledger.Waiting, ledger.HoldsNumbers, ledger.Format, ledger.At)
	}
	retries := s.retries.Load()
	s.testBusyReads.Store(1)
	if got := exported(t, exportOf(t, s, ExportOptions{})); got != ledgerCSV {
		t.Errorf("an export whose read ran twice writes\n%s", got)
	}
	if n := s.retries.Load() - retries; n != 1 {
		t.Errorf("the read ran again %d times, want 1", n)
	}

	t.Run("raw and numbers", func(t *testing.T) {
		e := exportOf(t, s, ExportOptions{What: []string{"transactions", "file-transactions", "lots", "snapshots"}, Format: "jsonl", Raw: true, ShowAccounts: true})
		if !e.HoldsNumbers || !exportOf(t, s, ExportOptions{ShowAccounts: true}).HoldsNumbers || !exportOf(t, s, ExportOptions{Raw: true}).HoldsNumbers {
			t.Error("an export with -raw or -show-accounts does not say it holds full numbers")
		}
		var b bytes.Buffer
		if err := tableOf(t, e, "transactions").write(&b, "jsonl"); err != nil {
			t.Fatal(err)
		}
		// JSON that spans lines loses the white space between its tokens,
		// which a line of JSON Lines cannot hold; in CSV it is as it is.
		var csvOut bytes.Buffer
		if err := tableOf(t, e, "transactions").write(&csvOut, "csv"); err != nil {
			t.Fatal(err)
		}
		for _, raws := range storeRows(t, s, "SELECT latest_raw, first_raw FROM api_txn ORDER BY activity_id DESC") {
			if !strings.Contains(raws[0], "\n") {
				t.Fatalf("the raw JSON is on one line: %s", raws[0])
			}
			var latest, first bytes.Buffer
			if json.Compact(&latest, []byte(raws[0])) != nil || json.Compact(&first, []byte(raws[1])) != nil {
				t.Fatal("the raw JSON is not JSON")
			}
			if want := `"raw":` + latest.String() + `,"first_raw":` + first.String() + "}\n"; !strings.Contains(b.String(), want) {
				t.Errorf("the transactions in JSON Lines lack %s:\n%s", want, b.String())
			}
			if want := csvCell(raws[0]) + "," + csvCell(raws[1]) + "\n"; !strings.Contains(csvOut.String(), want) {
				t.Errorf("the transactions in CSV lack %s:\n%s", want, csvOut.String())
			}
		}
		if !strings.Contains(b.String(), `"account":"...398","account_number":"`+fileAccount+`",`) {
			t.Errorf("the transactions do not show the number after the masked one:\n%s", b.String())
		}
		b.Reset()
		if err := tableOf(t, e, "file-transactions").write(&b, "jsonl"); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
		rows := storeStrings(t, s, `SELECT h.raw FROM history_row h JOIN import_file f ON f.file_id = h.file_id ORDER BY h.posted_date`)
		if len(lines) != 5 || len(rows) != 5 {
			t.Fatalf("the file transactions are %q, the rows %q", lines, rows)
		}
		for i, raw := range rows {
			want := `"raw":` + raw + "}" // a History export in JSON: its element as it is
			if i >= 3 {
				want = `"raw":` + jsonString(raw) + "}" // in CSV: the row's text
			}
			if !strings.HasSuffix(lines[i], want) {
				t.Errorf("file transaction %d ends %q, want %q", i, lines[i][max(0, len(lines[i])-len(want)):], want)
			}
		}
		snapshot, lot := tableOf(t, e, "snapshots"), tableOf(t, e, "lots")
		if got, want := snapshot.rows[0][len(snapshot.Columns)-1], storeStrings(t, s, "SELECT raw FROM snapshot")[0]; got != (cell{text: want, kind: cellJSON}) {
			t.Errorf("the snapshot's raw is %+v, want %s", got, want)
		}
		if got, want := lot.rows[0][len(lot.Columns)-1], storeStrings(t, s, "SELECT raw FROM realized_row")[0]; got != (cell{text: want, kind: cellString}) {
			t.Errorf("the lot's raw is %+v, want %s", got, want)
		}

		// Without -raw and -show-accounts, no table holds a full number.
		all := exportOf(t, s, ExportOptions{What: []string{"all"}, All: true})
		for _, tb := range all.Tables {
			for _, row := range tb.rows {
				for i, c := range row {
					if strings.Contains(c.text, fileAccount) || strings.Contains(c.text, "22220135") || strings.Contains(c.text, "12345678") || strings.Contains(c.text, "1234-5678") {
						t.Errorf("%s's %s holds a full number: %q", tb.Name, tb.Columns[i], c.text)
					}
					if strings.HasSuffix(c.text, ".000Z") {
						t.Errorf("%s's %s holds a time as the store writes it: %q", tb.Name, tb.Columns[i], c.text)
					}
				}
			}
		}
	})

	t.Run("filters", func(t *testing.T) {
		refs := func(e *Export) []string {
			var out []string
			for _, row := range e.Tables[0].rows {
				out = append(out, row[0].text)
			}
			return out
		}
		e := exportOf(t, s, ExportOptions{What: []string{"entries"}, Account: "...398", From: "2026-01-09", To: "2026-02-11"})
		if got := refs(e); !slices.Equal(got, []string{"C52b4daba.2", "C52b4daba.3", "T1002", "Cfeb86276.1"}) {
			t.Errorf("the entries from 2026-01-09 to 2026-02-11 are %q", got)
		}
		if e.account != "A1 ...398" || e.Waiting != 1 || len(e.Coverage) != 5 || e.Coverage[4].Source != "snapshot" {
			t.Errorf("the export is of %q with %d waiting and the coverage %+v", e.account, e.Waiting, e.Coverage)
		}
		if e := exportOf(t, s, ExportOptions{From: "2026-02-12"}); e.Waiting != 1 || e.Tables[0].Rows != 3 {
			t.Errorf("from 2026-02-12 the ledger holds %d rows and %d wait", e.Tables[0].Rows, e.Waiting)
		}
		if got := refs(exportOf(t, s, ExportOptions{What: []string{"ledger"}, All: true, From: "2026-02-11", To: "2026-02-11"})); !slices.Equal(got, []string{"Cfeb86276.1"}) {
			t.Errorf("the ledger with -all of 2026-02-11 is %q", got)
		}
		if got := exportOf(t, s, ExportOptions{What: []string{"legs"}}).Tables[0].Rows; got != 8 {
			t.Errorf("the counted legs are %d, want 8", got)
		}
		if got := exportOf(t, s, ExportOptions{What: []string{"legs"}, All: true}).Tables[0].Rows; got != 10 {
			t.Errorf("every leg is %d, want 10", got)
		}
		e = exportOf(t, s, ExportOptions{What: []string{"coverage"}, From: "2026-02-28", To: "2026-03-01"})
		var sources []string
		for _, row := range e.Tables[0].rows {
			sources = append(sources, row[2].text)
		}
		if !slices.Equal(sources, []string{"api", "realized"}) {
			t.Errorf("the coverage that meets 2026-02-28 to 2026-03-01 is %q", sources)
		}
		if len(e.Coverage) != 6 || e.Coverage[5].AccountID != 2 {
			t.Errorf("the export's coverage is %+v, want both accounts' whole", e.Coverage)
		}
		if _, err := s.Export(t.Context(), ExportOptions{Account: "A9"}); err == nil || err.Error() != "no account A9 in the store" {
			t.Errorf("an unknown account: %v", err)
		}
	})

	t.Run("files", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "ledger.csv")
		if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ledger.WriteFile(path); err != nil {
			t.Fatal(err)
		}
		if got := readOutput(t, path); got != ledgerCSV {
			t.Errorf("the file holds\n%s", got)
		}
		if m := mode(t, path); runtime.GOOS != "windows" && m != 0o600 {
			t.Errorf("the file's mode is %v, want 0600", m)
		}
		if temps := exportTemps(t, dir); len(temps) != 0 {
			t.Errorf("the export left %q", temps)
		}

		// A write that fails midway leaves the file as it was.
		failed := errors.New("the disk went away")
		err := writePrivate(path, func(w io.Writer) error {
			_, _ = io.WriteString(w, "half a ledger")
			return failed
		})
		if !errors.Is(err, failed) || !strings.HasPrefix(err.Error(), "writing "+path+": ") {
			t.Errorf("a failing write returns %v", err)
		}
		if got := readOutput(t, path); got != ledgerCSV {
			t.Errorf("after a failing write the file holds\n%s", got)
		}
		if temps := exportTemps(t, dir); len(temps) != 0 {
			t.Errorf("the failing write left %q", temps)
		}

		all := exportOf(t, s, ExportOptions{What: []string{"all"}, Account: "A1", From: "2026-01-01"})
		if _, err := all.WriteTo(io.Discard); err == nil || err.Error() != "-what all needs -dir" {
			t.Errorf("writing every table to one stream: %v", err)
		}
		if err := all.WriteFile(filepath.Join(dir, "all.csv")); err == nil {
			t.Error("every table written to one file")
		}
		out := filepath.Join(dir, "export", "2026")
		if err := all.WriteDir(out); err != nil {
			t.Fatal(err)
		}
		if m := mode(t, out); runtime.GOOS != "windows" && m != 0o700 {
			t.Errorf("the directory's mode is %v, want 0700", m)
		}
		var manifest struct {
			ExportedAt              string `json:"exported_at"`
			Schema, Rules, Calendar int
			Format                  string
			Filters                 struct {
				Account, From, To *string
				All               bool
			}
			Tables []struct {
				What, View, File string
				Rows             int
			}
			Units    map[string]string
			Coverage []map[string]any
		}
		if err := json.Unmarshal([]byte(readOutput(t, filepath.Join(out, "manifest.json"))), &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.ExportedAt != "2026-11-12T15:04:05.678Z" || manifest.Schema != 1 || manifest.Rules != DeriveVersion || manifest.Calendar < 1 ||
			manifest.Format != "csv" || *manifest.Filters.Account != "A1 ...398" || *manifest.Filters.From != "2026-01-01" ||
			manifest.Filters.To != nil || len(manifest.Units) == 0 || len(manifest.Coverage) != 5 || manifest.Coverage[0]["source"] != "api" {
			t.Errorf("the manifest is %+v", manifest)
		}
		if len(manifest.Tables) != len(exportTables) {
			t.Errorf("the manifest lists %d tables, want %d", len(manifest.Tables), len(exportTables))
		}
		for _, tb := range manifest.Tables {
			path := filepath.Join(out, tb.File)
			lines := strings.Count(readOutput(t, path), "\n")
			if tb.File != tb.What+".csv" || lines != tb.Rows+1 {
				t.Errorf("%s: %s holds %d lines, and the manifest says %d rows", tb.What, tb.File, lines, tb.Rows)
			}
			if m := mode(t, path); runtime.GOOS != "windows" && m != 0o600 {
				t.Errorf("%s's mode is %v, want 0600", tb.File, m)
			}
		}
		if got := readOutput(t, filepath.Join(out, "ledger.csv")); got != ledgerCSV {
			t.Errorf("the directory's ledger is\n%s", got)
		}
		jsonl := filepath.Join(dir, "jsonl")
		if err := exportOf(t, s, ExportOptions{What: []string{"ledger", "gaps"}, Format: "jsonl"}).WriteDir(jsonl); err != nil {
			t.Fatal(err)
		}
		if got := listing(t, jsonl); !slices.Equal(got, []string{"gaps.jsonl", "ledger.jsonl", "manifest.json"}) {
			t.Errorf("the directory in JSON Lines holds %q", got)
		}
		if got := readOutput(t, filepath.Join(jsonl, "ledger.jsonl")); got != ledgerJSONL {
			t.Errorf("the directory's ledger in JSON Lines is\n%s", got)
		}
	})

	t.Run("the store's own files", func(t *testing.T) {
		outputs := []string{s.path, s.path + "-wal", s.path + ".sync.lock"} // the last does not exist
		link := filepath.Join(t.TempDir(), "link.db")
		if err := os.Link(s.path, link); err != nil {
			t.Logf("no hard link here: %v", err)
		} else {
			outputs = append(outputs, link)
			// A directory whose table file would be the store, under
			// another name, is refused before anything is written.
			linked := t.TempDir()
			if err := os.Link(s.path, filepath.Join(linked, "ledger.csv")); err != nil {
				t.Fatal(err)
			}
			if err := ledger.WriteDir(linked); err == nil || !strings.Contains(err.Error(), "would replace the store itself") {
				t.Errorf("writing a directory whose ledger.csv is the store: %v", err)
			}
			if got := listing(t, linked); !slices.Equal(got, []string{"ledger.csv"}) {
				t.Errorf("the refused directory holds %q", got)
			}
		}
		before := listing(t, filepath.Dir(s.path))
		for _, path := range outputs {
			err := ledger.WriteFile(path)
			if want := path + " would replace the store itself (or one of its files); choose another name"; err == nil || err.Error() != want {
				t.Errorf("writing to %s: %v", path, err)
			}
		}
		if err := ledger.WriteDir(filepath.Dir(s.path) + string(filepath.Separator)); err != nil {
			t.Errorf("writing beside the store: %v", err)
		}
		dir := filepath.Dir(s.path)
		if got := listing(t, dir); !slices.Equal(got, slices.Sorted(slices.Values(append(before, "ledger.csv", "manifest.json")))) {
			t.Errorf("beside the store are %q", got)
		}
		if got := exported(t, exportOf(t, s, ExportOptions{})); got != ledgerCSV {
			t.Errorf("after the refusals the ledger is\n%s", got)
		}
	})

	t.Run("newer data", func(t *testing.T) {
		// A later export re-adjusts the lot: -raw gives the newer row. A
		// row posted after -from but as of a day before it is outside.
		later := realizedCSV("Wed Apr 01 09:00:00 EDT 2026", "01/01/2026", "04/01/2026",
			[9]string{"XYZ", "02/20/2026", "01/05/2026", "5", "$60.00", "$51.50", "$8.50", "Yes", "$1.00"})
		importFiles(t, s, ImportOptions{Account: fileAccount},
			"XXXX0398_GainLoss_Realized_Details_20260401-090000.csv", later,
			exportName("20260402-090000", "csv"), historyCSV(csvRow{"03/02/2026 as of 02/27/2026", "Bank Interest", "", "BANK INT", "", "", "", "$0.04"}))
		lots := tableOf(t, exportOf(t, s, ExportOptions{What: []string{"lots"}, Raw: true}), "lots")
		if want := strings.TrimSuffix(strings.SplitN(later, "\n", 3)[2], "\n"); len(lots.rows) != 1 || lots.rows[0][len(lots.Columns)-1].text != want {
			t.Errorf("the re-adjusted lot's raw is %+v, want the later export's %q", lots.rows, want)
		}
		for _, c := range []struct {
			from nytime.Date
			rows int
		}{{"2026-02-28", 0}, {"2026-02-27", 1}} {
			e := exportOf(t, s, ExportOptions{What: []string{"file-transactions"}, From: c.from, To: "2026-03-05"})
			if n := e.Tables[0].Rows; n != c.rows {
				t.Errorf("file transactions from %s: %d, want %d, by the day each is as of", c.from, n, c.rows)
			}
		}

		// The manifest names the store's versions, each its own.
		if _, err := rawDB(t, s.path).ExecContext(t.Context(), "UPDATE store SET derived_with = 7, calendar_version = 9"); err != nil {
			t.Fatal(err)
		}
		b, err := exportOf(t, s, ExportOptions{}).manifest()
		if err != nil {
			t.Fatal(err)
		}
		var m struct{ Schema, Rules, Calendar int }
		if err := json.Unmarshal(b, &m); err != nil || m.Schema != 1 || m.Rules != 7 || m.Calendar != 9 {
			t.Errorf("the manifest gives the versions %+v, %v; want schema 1, rules 7, calendar 9", m, err)
		}
	})
}

// Every view's columns, as an export names them, are unique, with
// account_number, raw and first_raw beside them (18 of the design); and
// each table that Export writes orders and filters its view by the view's
// own columns.
func TestExportNamesUnique(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	columns := map[string][]string{}
	for _, view := range storeStrings(t, s, "SELECT name FROM sqlite_schema WHERE type = 'view' ORDER BY name") {
		rows, err := s.r.QueryContext(t.Context(), "SELECT * FROM "+view+" LIMIT 0")
		if err != nil {
			t.Fatal(err)
		}
		cols, err := rows.Columns()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		columns[view] = cols
		seen := map[string]string{}
		for _, c := range append(cols, "account_number", "raw", "first_raw") {
			if other, ok := seen[exportedColumn(c)]; ok {
				t.Errorf("%s exports %s and %s as %s", view, other, c, exportedColumn(c))
			}
			seen[exportedColumn(c)] = c
		}
	}
	if len(columns) != 21 {
		t.Errorf("the store has %d views, want 21", len(columns))
	}
	for _, tb := range exportTables {
		for _, view := range []string{tb.view, tb.allView} {
			if view == "" {
				continue
			}
			cols, ok := columns[view]
			if !ok {
				t.Errorf("%s: no view %s", tb.name, view)
				continue
			}
			for _, c := range append(slices.Clone(tb.order), tb.day, tb.span[0], tb.span[1]) {
				if c != "" && !slices.Contains(cols, c) {
					t.Errorf("%s: %s has no column %s", tb.name, view, c)
				}
			}
			if slices.Contains(cols, "account_id") == tb.noAccount {
				t.Errorf("%s: %s has account_id %v, but noAccount is %v", tb.name, view, !tb.noAccount, tb.noAccount)
			}
			if tb.counted && !slices.Contains(cols, "counted") {
				t.Errorf("%s: %s has no column counted", tb.name, view)
			}
		}
	}
}

// Options that Export refuses, before it reads anything, and the tables
// those it takes ask for.
func TestExportOptions(t *testing.T) {
	t.Parallel()
	names := func(o ExportOptions) string {
		ts, err := o.tables()
		if err != nil {
			return "error: " + err.Error()
		}
		var out []string
		for _, tb := range ts {
			out = append(out, tb.name)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		o    ExportOptions
		want string
	}{
		{ExportOptions{}, "ledger"},
		{ExportOptions{What: []string{"lots", " legs", "lots"}}, "lots,legs"},
		{ExportOptions{What: []string{"gaps", "all"}}, "gaps,ledger,entries,transactions,legs,file-transactions,imports,snapshots,positions,balances,lots,flags,coverage,securities"},
		{ExportOptions{What: []string{"all"}, Raw: true}, "ledger,entries,transactions,legs,file-transactions,imports,snapshots,positions,balances,lots,gaps,flags,coverage,securities"},
		{ExportOptions{What: []string{"trades"}}, `error: unknown -what "trades"; one of ledger, entries, transactions, legs, file-transactions, imports, snapshots, positions, balances, lots, gaps, flags, coverage, securities, or all`},
		{ExportOptions{What: []string{"ledger", "legs"}, Raw: true}, "error: -raw is kept for ledger, entries, transactions, file-transactions, snapshots and lots"},
		{ExportOptions{Format: "xlsx"}, `error: -format is csv or jsonl, not "xlsx"`},
		{ExportOptions{Format: "jsonl", From: "2026-02-30"}, `error: "...230" is not a date (YYYY-MM-DD)`}, // masked, as free text is
		{ExportOptions{To: "2/30/26"}, `error: "2/30/26" is not a date (YYYY-MM-DD)`},
		{ExportOptions{From: "2026-03-01", To: "2026-02-01"}, "error: -from 2026-03-01 is after -to 2026-02-01"},
	} {
		if got := names(c.o); got != c.want {
			t.Errorf("%+v: %s, want %s", c.o, got, c.want)
		}
	}
	if kinds := ExportKinds(); len(kinds) != len(exportTables) || kinds[0] != (ExportKind{Name: "ledger", About: exportTables[0].about, Raw: true}) || kinds[3].Raw {
		t.Errorf("the kinds are %+v", kinds)
	}
	coverage := exportTables[slices.IndexFunc(exportTables, func(t exportTable) bool { return t.name == "coverage" })]
	where, args := exportFilter(coverage, ExportOptions{From: "2026-01-01", To: "2026-01-31"}, sql.NullInt64{Int64: 3, Valid: true})
	if where != " WHERE v.account_id = ? AND v.to_date >= ? AND v.from_date <= ?" || !slices.Equal(args, []any{int64(3), "2026-01-01", "2026-01-31"}) {
		t.Errorf("the coverage's filter is %q %v", where, args)
	}
}

// A cell is written as the export's units say: cents with two places and
// millionths trimmed, both numbers in JSON; a float as briefly as it reads
// back; a time in RFC 3339 with milliseconds only when it has some; raw
// JSON as it is, on one line; anything else as a string, and NULL as an
// empty CSV cell and null.
func TestExportCells(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		column string
		v      any
		isJSON bool
		want   cell
	}{
		{"amount_cents", int64(-60546), false, cell{"-605.46", cellNumber}},
		{"amount_cents", int64(5), false, cell{"0.05", cellNumber}},
		{"quantity_micros", int64(37800), false, cell{"0.0378", cellNumber}},
		{"quantity_micros", int64(-3_000_000), false, cell{"-3", cellNumber}},
		{"account_id", int64(12), false, cell{"12", cellNumber}},
		{"price", 201.82, false, cell{"201.82", cellNumber}},
		{"strike", 1e21, false, cell{"1000000000000000000000", cellNumber}},
		{"price", math.Inf(1), false, cell{"+Inf", cellString}},
		{"time", "2025-06-02T14:30:27.000Z", false, cell{"2025-06-02T14:30:27Z", cellString}},
		{"imported_at", "2025-06-02T14:30:27.120Z", false, cell{"2025-06-02T14:30:27.120Z", cellString}},
		{"gone_since", "2025-06-02T14:30:27.000Z", false, cell{"2025-06-02T14:30:27Z", cellString}},
		{"description", "2025-06-02T14:30:27.000Z", false, cell{"2025-06-02T14:30:27.000Z", cellString}},
		{"time", "yesterday", false, cell{"yesterday", cellString}},
		{"raw", `{"a": 1.10, "b": [ 2 ]}`, true, cell{`{"a": 1.10, "b": [ 2 ]}`, cellJSON}},
		{"first_raw", "{\"a\": 1.10,\n \"b\": \"x y\"}", true, cell{"{\"a\": 1.10,\n \"b\": \"x y\"}", cellJSON}},
		{"raw", `{"a": 1`, true, cell{`{"a": 1`, cellString}},
		{"raw", `"XYZ","1.00"`, false, cell{`"XYZ","1.00"`, cellString}},
		{"raw", []byte(`{}`), true, cell{`{}`, cellJSON}},
		{"flags", nil, false, cell{"", cellNull}},
	} {
		got, err := exportCell(c.column, c.v, c.isJSON)
		if err != nil || got != c.want {
			t.Errorf("%s %#v: %+v, %v; want %+v", c.column, c.v, got, err, c.want)
		}
	}
	for _, v := range []any{1.5, true} {
		if _, err := exportCell("amount_cents", v, false); err == nil {
			t.Errorf("amount_cents %#v is taken", v)
		}
	}

	tb := ExportedTable{Columns: []string{"a", "b", "c"}, rows: [][]cell{
		{{"x, \"y\"\nz", cellString}, {"", cellNull}, {"-1.50", cellNumber}},
		{{"<a&b>", cellString}, {`{"k":[1]}`, cellJSON}, {"é", cellString}},
		{{"", cellString}, {"{\"k\": [1.0,\r\n 2]}", cellJSON}, {"", cellNull}},
	}}
	var b bytes.Buffer
	if err := tb.write(&b, "csv"); err != nil {
		t.Fatal(err)
	}
	if want := "a,b,c\n\"x, \"\"y\"\"\nz\",,-1.50\n<a&b>,\"{\"\"k\"\":[1]}\",é\n,\"{\"\"k\"\": [1.0,\r\n 2]}\",\n"; b.String() != want {
		t.Errorf("the CSV is %q, want %q", b.String(), want)
	}
	b.Reset()
	if err := tb.write(&b, "jsonl"); err != nil {
		t.Fatal(err)
	}
	if want := `{"a":"x, \"y\"\nz","b":null,"c":-1.50}` + "\n" + `{"a":"<a&b>","b":{"k":[1]},"c":"é"}` + "\n" +
		`{"a":"","b":{"k":[1.0,2]},"c":null}` + "\n"; b.String() != want {
		t.Errorf("the JSON Lines are %q, want %q", b.String(), want)
	}
	for line := range strings.Lines(b.String()) {
		if !json.Valid([]byte(line)) {
			t.Errorf("%q is not JSON", line)
		}
	}
}
