package schwabfile

import (
	"slices"
	"strings"
	"testing"
)

// A History CSV's footer gives its stated total and ends its rows: a
// record after it is not read, and warns; a footer whose total is not a
// number warns that it is not; empty records are passed over.
func TestHistoryFooter(t *testing.T) {
	header := `"Date","Action","Symbol","Description","Quantity","Price","Fees & Comm","Amount"` + "\n"
	row := `"01/02/2026","Credit Interest","","SCHWAB1 INT","","","","$1.25"` + "\n"
	f, err := Read("x.csv", []byte(header+row+`,,,,,,,`+"\n"+row+
		`"TRANSACTIONS TOTAL","","","","","","$2.50",""`+"\n"+"\n"+row+row))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.History) != 2 || f.StatedTotal == nil || f.StatedTotal.Canon() != "2.5" {
		t.Fatalf("read %d rows and the total %+v", len(f.History), f.StatedTotal)
	}
	if !slices.Equal(f.Warnings, []Warning{{
		WarnRowsAfterTotal,
		"2 records after the Transactions Total line, the first on line 7, not read",
	}}) {
		t.Errorf("warnings %v", f.Warnings)
	}
	f, err = Read("x.csv", []byte(header+row+`"Transactions Total","","","","","","$0.10","$1.25",""`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.StatedTotal == nil || f.StatedTotal.Canon() != "1.25" || len(f.Warnings) != 0 {
		t.Errorf("the total is the footer's last cell, not %+v (%v)", f.StatedTotal, f.Warnings)
	}
	f, err = Read("x.csv", []byte(header+row+`Transactions Total,"","","","","","",N/A`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Warnings, []Warning{{WarnTotalMismatch, `the stated total, "N/A", is not a number`}}) {
		t.Errorf("a total that is not a number warns %v", f.Warnings)
	}
	f, err = Read("x.csv", []byte(header+row+`Transactions Total,"","","","","","",-$1.25`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.Warnings, []Warning{{WarnTotalMismatch, "the stated total is -$1.25 but the rows sum to 1.25"}}) {
		t.Errorf("a wrong total warns %v", f.Warnings)
	}
	f, err = Read("x.csv", []byte(header+row+strings.Replace(row, "$1.25", "--", 1)+`Transactions Total,"","","","","","",$9`+"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 0 {
		t.Errorf("a total beside a placeholder, whose sum is unknown, warns %v", f.Warnings)
	}
}

// History JSON's rows are read by their keys: a number is read as
// written, null and a missing key are blank (a missing column is a
// problem), any other value is a problem, an element that is not an object
// is a row of problems, keys beyond the ten are kept in Raw only, and the
// stated fees are checked like the total.
func TestHistoryJSONRows(t *testing.T) {
	content := `{"FromDate":"01/01/2026","ToDate":"01/31/2026","TotalTransactionsAmount":"$3.01",` +
		`"TotalFeesAndCommAmount":"$0.02","BrokerageTransactions":[` + "\n" +
		`{"Date":"01/02/2026","Action":"Buy","Symbol":"XYZ","Description":"XYZ CORP","Quantity":2,"Price":"$1.00",` +
		`"Fees & Comm":"$0.01","Amount":"-$2.01","ItemIssueId":71234567,"AcctgRuleCd":"2","NewKey":{"a":1}},` + "\n" +
		`{"Date":"01/03/2026","Action":"Credit Interest","Symbol":null,"Description":["x"],"Price":"","Fees & Comm":"",` +
		`"Amount":"$5.01"},` + "\n" +
		`"a string"]}`
	f, err := Read("x.json", []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.History) != 3 {
		t.Fatalf("read %d rows, want 3", len(f.History))
	}
	r := f.History[0]
	if r.Quantity.Canon() != "2" || r.ItemIssueID != "71234567" || r.AcctgRuleCd != "2" || len(r.Problems) != 0 ||
		!strings.Contains(r.Raw, `"NewKey":{"a":1}`) || r.Line != 2 || !strings.HasPrefix(r.Raw, `{"Date"`) || !strings.HasSuffix(r.Raw, "}") {
		t.Errorf("row 1 reads as %+v", r)
	}
	r = f.History[1]
	if r.Symbol != "" || r.Description != "" || r.Line != 3 ||
		!slices.Equal(r.Problems, []string{"Description is not text", "no Quantity"}) {
		t.Errorf("row 2 reads as %+v", r)
	}
	r = f.History[2]
	if r.Raw != `"a string"` || r.Posted != "" || !slices.Equal(r.Problems, []string{"not an object"}) {
		t.Errorf("row 3 reads as %+v", r)
	}
	if !slices.Equal(codes(f.Warnings), []string{WarnUndatedRows, WarnTotalMismatch}) ||
		f.Warnings[1].Detail != "the stated total is $3.01 but the rows sum to 3; the stated total of fees is $0.02 but the rows sum to 0.01" {
		t.Errorf("warnings %v", f.Warnings)
	}
	f, err = Read("x.json", []byte(`{"BrokerageTransactions":[{"Date":"01/02/2026"}],"BrokerageTransactions":[]}`))
	if err != nil || len(f.History) != 0 {
		t.Errorf("a key twice reads as the last: %v, %v", f, err)
	}
}

// `schwabtx transactions -json` output is kept row by row, its white space
// taken out and nothing else changed; a BOM before it is passed over.
func TestAPIJSON(t *testing.T) {
	f, err := Read("t.json", []byte("\xef\xbb\xbf[\n  {\n    \"activityId\": 1,\n    \"description\": \"A & B \\u0026 <c>\"\n  },"+
		"\n  {\"transferItems\": [], \"netAmount\": 1.50}\n]\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`{"activityId":1,"description":"A & B \u0026 <c>"}`, `{"transferItems":[],"netAmount":1.50}`}
	if f.Kind != KindAPIJSON || !f.BOM || len(f.APIRows) != 2 || string(f.APIRows[0]) != want[0] || string(f.APIRows[1]) != want[1] {
		t.Errorf("read as %s %v: %s", f.Kind, f.BOM, f.APIRows)
	}
}

// Realized Gain/Loss's lots: Gain/Loss is checked against Proceeds less
// Cost Basis, and on a wash sale against that plus the Disallowed Loss,
// the two conventions seen; a Wash Sale? other than Yes or No, a Closed
// Date or a Transaction Closed Date that is not a date, and a number that
// is not one are problems; an Opened Date that is not a date is not; a
// Total line ends the lots and is checked against their sums.
func TestRealizedRows(t *testing.T) {
	lot := func(gain, wash, disallowed string) string {
		return `"XYZ","XYZ CORP","07/01/2026","01/05/2026","10","$9","$10","$90.00","$100.00","` + gain +
			`","","","","Short Term","$100.00","` + wash + `","` + disallowed + `","","","","","","","",""` + "\n"
	}
	title := `"Realized Gain/Loss - Lot Details for XXXX-1234 as of Mon Jul 06 10:47:11 EDT 2026 from 01/01/2026 to 07/06/2026"` + "\n"
	for _, c := range []struct {
		lot      string
		disagree bool
		problems []string
	}{
		{lot("-$10.00", "No", ""), false, nil},
		{lot("-$10.00", "", ""), false, nil},
		{lot("-$10.00", "Yes", "$10.00"), false, nil},
		{lot("$0.00", "yes", "$10.00"), false, nil},
		{lot("$0.00", "No", "$10.00"), true, nil},
		{lot("$0.00", "Yes", ""), true, nil},
		{lot("$5.00", "Yes", "$10.00"), true, nil},
		{lot("--", "Yes", "$10.00"), false, nil},
		{lot("-$10.00", "Maybe", ""), false, []string{`Wash Sale? "Maybe" is not Yes or No`}},
		{lot("ten", "No", "x"), false, []string{`Gain/Loss ($) "ten" is not a number`, `Disallowed Loss "x" is not a number`}},
		{
			strings.Replace(strings.Replace(lot("-$10.00", "No", ""), "07/01/2026", "Pending 12345678", 1), "01/05/2026", "Various", 1),
			false,
			[]string{`Closed Date "Pending ...678" is not MM/DD/YYYY`},
		},
		{
			strings.Replace(lot("-$10.00", "No", ""), `"Short Term","$100.00","No","","",`, `"Short Term","$100.00","No","","13/01/2026",`, 1),
			false,
			[]string{`Transaction Closed Date "13/01/2026" is not MM/DD/YYYY`},
		},
	} {
		f, err := Read("x.csv", []byte(title+lotsHeader+c.lot))
		if err != nil {
			t.Fatal(err)
		}
		r := f.Realized[0]
		if r.ColumnsDisagree != c.disagree || !slices.Equal(r.Problems, c.problems) {
			t.Errorf("%s reads as disagreeing %v with problems %q, want %v and %q", c.lot, r.ColumnsDisagree, r.Problems,
				c.disagree, c.problems)
		}
		if r.Closed == "" && !slices.Contains(codes(f.Warnings), WarnUndatedRows) {
			t.Errorf("%s: a lot without a Closed Date does not warn: %v", c.lot, f.Warnings)
		}
		if strings.Contains(c.lot, "Various") && (r.Opened != "" || r.OpenedText != "Various") {
			t.Errorf("an Opened Date of Various reads as %q, %q", r.Opened, r.OpenedText)
		}
	}
	total := `"Total","","","","20","","","$180.00","$200.01","-$20.00","","","","","--","","","","","","","","","",""` + "\n"
	f, err := Read("x.csv", []byte(title+lotsHeader+lot("-$10.00", "No", "")+lot("-$10.00", "No", "")+total+
		lot("-$10.00", "No", "")))
	if err != nil {
		t.Fatal(err)
	}
	want := []Warning{
		{WarnRowsAfterTotal, "1 record after the Total line, the first on line 6, not read"},
		{WarnTotalMismatch, "the Total line's Cost Basis (CB) is $200.01 but the lots sum to 200"},
	}
	if len(f.Realized) != 2 || !slices.Equal(f.Warnings, want) {
		t.Errorf("read %d lots with warnings %v, want 2 and %v", len(f.Realized), f.Warnings, want)
	}
}
