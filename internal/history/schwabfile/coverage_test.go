package schwabfile

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// historyJSON writes a History export in JSON of n rows, one a day
// backwards from newest, newest first, with the range from and to (none
// when ""), each row of $1.00 and the total stated as total ("" for the
// rows' sum).
func historyJSON(t *testing.T, n int, newest, from, to, total string) []byte {
	t.Helper()
	day, err := time.Parse(time.DateOnly, newest)
	if err != nil {
		t.Fatal(err)
	}
	type row map[string]string
	rows := make([]row, n)
	for i := range rows {
		rows[i] = row{
			"Date": day.AddDate(0, 0, -i).Format("01/02/2006"), "Action": "Credit Interest", "Symbol": "",
			"Description": "SCHWAB1 INT", "Quantity": "", "Price": "", "Fees & Comm": "", "Amount": "$1.00",
		}
	}
	if total == "" {
		total = fmt.Sprintf("$%d.00", n)
	}
	doc := map[string]any{"TotalTransactionsAmount": total, "BrokerageTransactions": rows}
	for k, v := range map[string]string{"FromDate": from, "ToDate": to} {
		if v != "" {
			d, _ := time.Parse(time.DateOnly, v)
			doc[k] = d.Format("01/02/2006")
		}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// built returns the File that Read makes of historyJSON's file of n rows
// with the stated total, built in memory: writing and reading 10,000 rows
// takes seconds under -race.
func built(t *testing.T, n int, newest, from, to string) *File {
	t.Helper()
	day, err := time.Parse(time.DateOnly, newest)
	if err != nil {
		t.Fatal(err)
	}
	amount, _ := ParseDecimal("$1.00")
	total, _ := ParseDecimal(fmt.Sprintf("$%d.00", n))
	f := &File{
		Kind: KindHistoryJSON, Layout: LayoutHistoryJSON, StatedFrom: nytime.Date(from), StatedTo: nytime.Date(to),
		StatedTotal: &total, History: make([]HistoryRow, n),
	}
	for i := range f.History {
		f.History[i] = HistoryRow{Ordinal: i + 1, Posted: nytime.Date(day.AddDate(0, 0, -i).Format(time.DateOnly)), Amount: amount}
	}
	f.Warnings = f.limits()
	return f
}

// read returns what Read makes of content.
func read(t *testing.T, content []byte) *File {
	t.Helper()
	f, err := Read("x.json", content)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// codes returns the codes of warnings.
func codes(ws []Warning) []string {
	var cs []string
	for _, w := range ws {
		cs = append(cs, w.Code)
	}
	return cs
}

// The warnings of a whole export, on generated sets of rows (of 10,000,
// built in memory): exactly
// 1,500 or 10,000 rows is at the cap and 1,499 or 10,001 is not; an empty
// file; a range longer than two years whose oldest row is within a week of
// two years before its end, and one whose oldest row is not; a first row
// more than a week after the range's start; a total that is not the rows'
// sum. Each says what it narrows the coverage to: the cap, the late start
// and the two years start it the day after the oldest row, an empty file
// covers nothing, and a range that ends on the day of the export ends the
// day before.
func TestCaps(t *testing.T) {
	for _, c := range []struct {
		name     string
		f        *File
		exported nytime.Date
		warnings []string
		from, to nytime.Date
		clipped  string
	}{
		{
			"1,500 rows", read(t, historyJSON(t, 1500, "2026-10-30", "2022-09-22", "2026-10-31", "")), "2026-11-02",
			[]string{WarnAtCap},
			"2022-09-23", "2026-10-31", "at_cap: it starts on 2022-09-23, the day after its oldest row",
		},
		{
			"1,499 rows", read(t, historyJSON(t, 1499, "2026-10-30", "2022-09-23", "2026-10-31", "")), "2026-11-02",
			nil, "2022-09-23", "2026-10-31", "",
		},
		{
			"10,000 rows", built(t, 10000, "2026-10-30", "1999-06-15", "2026-10-31"), "",
			[]string{WarnAtCap},
			"1999-06-16", "2026-10-31", "at_cap: it starts on 1999-06-16",
		},
		{
			"10,001 rows", built(t, 10001, "2026-10-30", "1999-06-14", "2026-10-31"), "",
			nil, "1999-06-14", "2026-10-31", "",
		},
		{
			"empty", read(t, historyJSON(t, 0, "2026-10-30", "2026-10-01", "2026-10-31", "$0.00")), "2026-11-02",
			[]string{WarnEmpty},
			"", "", "empty: it holds no rows",
		},
		{
			"empty without a range", read(t, historyJSON(t, 0, "2026-10-30", "", "", "$0.00")), "",
			[]string{WarnEmpty},
			"", "", "",
		},
		{
			"two years", read(t, historyJSON(t, 10, "2024-11-07", "2024-10-29", "2026-10-31", "")), "",
			[]string{WarnTwoYears},
			"2024-10-30", "2026-10-31", "two_years: it starts on 2024-10-30",
		},
		{
			"two years, late", read(t, historyJSON(t, 10, "2024-11-05", "2020-01-01", "2026-10-31", "")), "",
			[]string{WarnTwoYears, WarnLateStart},
			"2024-10-28", "2026-10-31", "two_years: it starts on 2024-10-28",
		},
		{
			"two years and a week", read(t, historyJSON(t, 10, "2024-11-16", "2024-10-30", "2026-10-31", "")), "",
			[]string{WarnTwoYears, WarnLateStart},
			"2024-11-08", "2026-10-31", "two_years",
		},
		{
			"two years and eight days", read(t, historyJSON(t, 10, "2024-11-17", "2024-10-30", "2026-10-31", "")), "",
			[]string{WarnLateStart},
			"2024-11-09", "2026-10-31", "late_start",
		},
		{
			"eight days short of two years", read(t, historyJSON(t, 10, "2024-11-01", "2024-10-23", "2026-10-31", "")), "",
			nil, "2024-10-23", "2026-10-31", "",
		},
		{
			"two years exactly", read(t, historyJSON(t, 10, "2024-11-09", "2024-10-31", "2026-10-31", "")), "",
			nil, "2024-10-31", "2026-10-31", "",
		},
		{
			"a week late", read(t, historyJSON(t, 10, "2026-10-17", "2026-10-01", "2026-10-31", "")), "",
			nil, "2026-10-01", "2026-10-31", "",
		},
		{
			"eight days late", read(t, historyJSON(t, 10, "2026-10-18", "2026-10-01", "2026-10-31", "")), "",
			[]string{WarnLateStart},
			"2026-10-10", "2026-10-31", "late_start: it starts on 2026-10-10",
		},
		{
			"total", read(t, historyJSON(t, 10, "2026-10-30", "2026-10-21", "2026-10-31", "$10.01")), "",
			[]string{WarnTotalMismatch},
			"2026-10-21", "2026-10-31", "",
		},
		{
			"total not a number", read(t, historyJSON(t, 10, "2026-10-30", "2026-10-21", "2026-10-31", "ten")), "",
			[]string{WarnTotalMismatch},
			"2026-10-21", "2026-10-31", "",
		},
		{
			"exported on the last day", read(t, historyJSON(t, 10, "2026-10-30", "2026-10-21", "2026-10-31", "")), "2026-10-31",
			nil, "2026-10-21", "2026-10-30", "exported on 2026-10-31: it ends on 2026-10-30, the day before",
		},
		{
			"exported before the last day", read(t, historyJSON(t, 10, "2026-10-30", "2026-10-21", "2026-10-31", "")), "2026-10-30",
			nil, "2026-10-21", "2026-10-29", "exported on 2026-10-30",
		},
		{
			"exported after the last day", read(t, historyJSON(t, 10, "2026-10-30", "2026-10-21", "2026-10-31", "")), "2026-11-01",
			nil, "2026-10-21", "2026-10-31", "",
		},
		{
			"exported before it starts", read(t, historyJSON(t, 10, "2026-10-30", "2026-10-21", "2026-10-31", "")), "2026-10-21",
			nil, "", "", "exported on 2026-10-21: it ends on 2026-10-20",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := c.f
			if got := codes(f.Warnings); !slices.Equal(got, c.warnings) {
				t.Errorf("warnings %v (%v), want %v", got, f.Warnings, c.warnings)
			}
			cov := f.Coverage("", "", c.exported)
			if cov.From != c.from || cov.To != c.to {
				t.Errorf("covers %q to %q, want %q to %q", cov.From, cov.To, c.from, c.to)
			}
			i := slices.IndexFunc(cov.Warnings, func(w Warning) bool { return w.Code == WarnCoverageClipped })
			switch {
			case c.clipped == "" && i >= 0:
				t.Errorf("clipped: %s", cov.Warnings[i].Detail)
			case c.clipped != "" && (i < 0 || !strings.Contains(cov.Warnings[i].Detail, c.clipped)):
				t.Errorf("coverage warnings %v, want one clipped %q", cov.Warnings, c.clipped)
			}
		})
	}
}

// A file's range is what it states, else what you gave, else its rows',
// each side on its own, and said; rows outside it widen it, and say so;
// the warnings of a Realized Gain/Loss file narrow it as a History file's
// do; a file with neither range nor rows covers nothing.
func TestCoverageRange(t *testing.T) {
	stated := read(t, historyJSON(t, 5, "2026-10-30", "2026-10-25", "2026-10-31", ""))
	unstated := read(t, historyJSON(t, 5, "2026-10-30", "", "", ""))
	half := read(t, historyJSON(t, 5, "2026-10-30", "2026-10-20", "", ""))
	for _, c := range []struct {
		name     string
		f        *File
		from, to nytime.Date
		want     Coverage
		warnings []string
	}{
		{
			"stated", stated, "2026-01-01", "2026-12-31",
			Coverage{RangeFrom: "2026-10-25", RangeTo: "2026-10-31", Source: "stated", From: "2026-10-25", To: "2026-10-31"},
			nil,
		},
		{
			"rows outside", stated, "", "",
			Coverage{RangeFrom: "2026-10-25", RangeTo: "2026-10-31", Source: "stated", From: "2026-10-25", To: "2026-10-31"},
			nil,
		},
		{
			"given", unstated, "2026-10-01", "2026-10-31",
			Coverage{RangeFrom: "2026-10-01", RangeTo: "2026-10-31", Source: "user", From: "2026-10-01", To: "2026-10-31"},
			nil,
		},
		{
			"rows", unstated, "", "",
			Coverage{RangeFrom: "2026-10-26", RangeTo: "2026-10-30", Source: "rows", From: "2026-10-26", To: "2026-10-30"},
			[]string{WarnCoverageFromRows},
		},
		{
			"given start", unstated, "2026-10-01", "",
			Coverage{RangeFrom: "2026-10-01", RangeTo: "2026-10-30", Source: "user", From: "2026-10-01", To: "2026-10-30"},
			[]string{WarnCoverageFromRows},
		},
		{
			"given narrower than its rows", unstated, "2026-10-28", "2026-10-29",
			Coverage{RangeFrom: "2026-10-26", RangeTo: "2026-10-30", Source: "user", From: "2026-10-26", To: "2026-10-30"},
			[]string{WarnRowsOutsideCoverage},
		},
		{
			"stated start, given end", half, "2026-10-01", "2026-11-05",
			Coverage{RangeFrom: "2026-10-20", RangeTo: "2026-11-05", Source: "stated", From: "2026-10-20", To: "2026-11-05"},
			nil,
		},
		{
			"stated start, rows' end", half, "", "",
			Coverage{RangeFrom: "2026-10-20", RangeTo: "2026-10-30", Source: "stated", From: "2026-10-20", To: "2026-10-30"},
			[]string{WarnCoverageFromRows},
		},
	} {
		got := c.f.Coverage(c.from, c.to, "")
		w := got.Warnings
		got.Warnings = nil
		if !reflect.DeepEqual(got, c.want) || !slices.Equal(codes(w), c.warnings) {
			t.Errorf("%s: %+v with %v, want %+v with %v", c.name, got, w, c.want, c.warnings)
		}
	}
	if w := unstated.Coverage("2026-10-28", "2026-10-29", "").Warnings; len(w) != 1 ||
		w[0].Detail != "rows from 2026-10-26, before 2026-10-28; rows to 2026-10-30, after 2026-10-29" {
		t.Errorf("rows outside are said as %v", w)
	}
	if w := half.Coverage("", "", "").Warnings; len(w) != 1 ||
		w[0].Detail != "it states no end of its range, and none was given: its rows give it, 2026-10-20 to 2026-10-30" {
		t.Errorf("a range half from its rows is said as %v", w)
	}

	lots := `"Realized Gain/Loss - Lot Details for XXXX-1234 as of Mon Jul 06 10:47:11 EDT 2026 from 01/01/2024 to 07/06/2026"` +
		"\n" + lotsHeader + `"XYZ","XYZ CORP","07/10/2024","01/05/2024","1","$1","$1","$1","$1","$0","0%","","$0","Short Term","$1","No","",""` + "\n"
	f, err := Read("x.csv", []byte(lots))
	if err != nil {
		t.Fatal(err)
	}
	if got := codes(f.Warnings); !slices.Equal(got, []string{WarnTwoYears}) {
		t.Errorf("a Realized Gain/Loss file two years long warns %v", f.Warnings)
	}
	if c := f.Coverage("", "", nytime.DateOf(f.ExportedAt)); c.From != "2024-07-11" || c.To != "2026-07-05" {
		t.Errorf("it covers %+v", c)
	}
	if c := (&File{Kind: KindHistoryCSV}).Coverage("", "", "2026-10-01"); c.Source != "" || c.From != "" || len(c.Warnings) != 0 {
		t.Errorf("a file without a range or rows covers %+v", c)
	}
}
