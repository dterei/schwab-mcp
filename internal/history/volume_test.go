package history

import (
	"flag"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// volume runs TestVolume, which takes minutes: by hand, and in make volume.
var volume = flag.Bool("history.volume", false, "TestVolume: run it (a year of History exports and five years of snapshots)")

// The store holds a real history's volume (23 of the design): a year of
// History exports of 3,000 rows a quarter (one export of 1,000 rows a
// month, below the cap of an export) and a snapshot file of five years, a
// line each evening, in one import; derived as it is written, it is what a
// rebuild derives; Check finds it sound; and every table exports. It runs
// only with -history.volume, and logs how long each step took.
func TestVolume(t *testing.T) {
	if !*volume {
		t.Skip("a test of volume: run it with -history.volume, or make volume")
	}
	s := newStore(t, Options{})
	ctx := t.Context()
	var files []string
	for m := time.January; m <= time.December; m++ {
		first := time.Date(2025, m, 1, 0, 0, 0, 0, time.UTC)
		last := first.AddDate(0, 1, -1)
		var days []time.Time
		for d := first; !d.After(last); d = d.AddDate(0, 0, 1) {
			if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
				days = append(days, d)
			}
		}
		rows := make([]csvRow, 1000)
		for i := range rows {
			d := days[i*len(days)/len(rows)].Format("01/02/2006")
			symbol := fmt.Sprintf("S%03d", i%250)
			cents := 100 + (int(m)*1000+i)*7%99_900
			amount := fmt.Sprintf("$%d.%02d", cents/100, cents%100)
			switch i % 5 {
			case 0:
				rows[i] = csvRow{d, "Buy", symbol, symbol + " CORP", "5", "$10.00", "$0.65", "-" + amount}
			case 1:
				rows[i] = csvRow{d, "Sell", symbol, symbol + " CORP", "5", "$10.00", "$0.65", amount}
			case 2:
				rows[i] = csvRow{d, "Qualified Dividend", symbol, symbol + " CORP", "", "", "", amount}
			case 3:
				rows[i] = csvRow{d, "Credit Interest", "", "SCHWAB1 INT", "", "", "", amount}
			default:
				rows[i] = csvRow{d, "Wire Sent", "", "WIRED FUNDS DISBURSED", "", "", "", "-" + amount}
			}
		}
		slices.Reverse(rows) // newest first, as an export writes them
		stamp := last.AddDate(0, 0, 3).Format("20060102") + "-090000"
		files = append(files, exportName(stamp, "json"), historyJSON(first.Format("01/02/2006"), last.Format("01/02/2006"), nil, rows...))
	}
	accounts := fixtureAccounts(t)
	var snapshots strings.Builder
	for d := time.Date(2021, time.January, 1, 22, 0, 0, 0, time.UTC); d.Year() < 2026; d = d.AddDate(0, 0, 1) {
		snapshots.WriteString(snapshotLine(t, d, accounts...))
	}
	files = append(files, "snapshots.jsonl", snapshots.String())

	step := time.Now()
	took := func(what string) {
		t.Logf("%s: %v", what, time.Since(step).Round(time.Millisecond))
		step = time.Now()
	}
	r := importFiles(t, s, ImportOptions{Account: fileAccount, Completeness: "complete"}, files...)
	took("importing 12 exports of 1,000 rows and 1,826 snapshot lines")
	for _, f := range r.Files[:12] {
		if f.New != 1000 || f.Completeness != "complete" {
			t.Errorf("%s: %d transactions new to the store, completeness %q", f.Name, f.New, f.Completeness)
		}
	}
	if n := storeInt(t, s, "SELECT count(*) FROM ledger WHERE source = 'file'"); n != 12_000 {
		t.Errorf("the ledger holds %d file transactions, want 12,000", n)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM snapshot"); n != int64(len(accounts))*1826 {
		t.Errorf("the store holds %d snapshots, want %d", n, len(accounts)*1826)
	}

	before := dumpDerived(t, s)
	took("dumping the derived tables")
	if err := s.Rebuild(ctx); err != nil {
		t.Fatal(err)
	}
	took("rebuilding")
	after := dumpDerived(t, s)
	for _, table := range slices.Sorted(maps.Keys(before)) {
		if !slices.Equal(before[table], after[table]) {
			t.Errorf("%s differs after the rebuild: %d rows before, %d after", table, len(before[table]), len(after[table]))
		}
	}
	c, err := s.Check(ctx)
	if err != nil || len(c.Problems) > 0 {
		t.Errorf("Check: %v, %q", err, c.Problems)
	}
	took("checking")
	e, err := s.Export(ctx, ExportOptions{What: []string{"all"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range e.Tables {
		if table.Name == "ledger" && table.Rows != 12_000 {
			t.Errorf("the ledger exports %d rows, want 12,000", table.Rows)
		}
	}
	took("exporting every table")
}
