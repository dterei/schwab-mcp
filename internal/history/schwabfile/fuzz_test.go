package schwabfile

import (
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Read never panics, gives the same each time, keeps each row's Raw as a
// piece of the text it read, and gives a coverage whose days are in order.
// The seeds are the fixtures, and the shapes the readers branch on.
func FuzzRead(f *testing.F) {
	for _, e := range readIndex(f) {
		f.Add(e.Name, fixture(f, e.File))
	}
	for _, s := range []string{
		"\"Transactions  for account XXXX-1234 as of 12/20/2014 21:16:49 ET\"\n\"Date\",\"Action\",\"Symbol\",\"Description\"," +
			"\"Quantity\",\"Price\",\"Fees & Comm\",\"Amount\"\n\"01/02/2026 as of 01/01/2026\",\"Buy\",\"X\",\"X\",\"1\",\"$1\",\"\",\"($1)\"\n" +
			"Transactions Total,,,,,,,$1\n",
		`{"FromDate":"01/01/2026","BrokerageTransactions":[{"Date":"01/02/2026","Amount":"$1"},5,null]}`,
		`[{"activityId":1},{"transferItems":[]}]`,
		"\"Realized Gain/Loss - Lot Details for ...123 as of Mon Jul 06 10:47:11 EDT 2026 from 01/01/2026 to 07/06/2026\"\n" +
			lotsHeader + "\"X\",\"X\",\"07/01/2026\",\"Various\",\"1\",\"--\"\n\"Total\",\"\",\"\",\"\",\"1\"\n",
		"{\"version\":1,\"fetched_at\":\"2026-11-05T21:05:00Z\",\"accounts\":[{}]}\n",
		"\"a\"b\"\n",
	} {
		f.Add("Individual_XXX123_Transactions_20260101-120000.csv", []byte(s))
	}
	f.Fuzz(func(t *testing.T, name string, content []byte) {
		got, err := Read(name, content)
		again, err2 := Read(name, content)
		if (err == nil) != (err2 == nil) || !reflect.DeepEqual(got, again) || err != nil && err.Error() != err2.Error() {
			t.Fatalf("Read gave %v, %v and then %v, %v", got, err, again, err2)
		}
		if err != nil {
			return
		}
		text, _, _ := decode(content)
		for _, r := range got.History {
			if !strings.Contains(text, r.Raw) {
				t.Errorf("row %d's Raw %q is not in the text", r.Ordinal, r.Raw)
			}
		}
		for _, r := range got.Realized {
			if !strings.Contains(text, r.Raw) {
				t.Errorf("lot %d's Raw %q is not in the text", r.Ordinal, r.Raw)
			}
		}
		for _, exported := range []string{"", "2026-01-01"} {
			c := got.Coverage("", "", nytime.Date(exported))
			if c.From > c.To || (c.From == "") != (c.To == "") {
				t.Errorf("covers %q to %q", c.From, c.To)
			}
		}
	})
}

// ParseDecimal never panics and refuses only what is neither blank, a
// placeholder nor a number; a number's canonical spelling reads back to
// itself, its cents and millionths agree, and its float is the float64
// nearest its exact value, as math/big rounds it, unless that is too large.
func FuzzDecimal(f *testing.F) {
	for _, s := range []string{
		"$1,234.56", "-$1,234.56", "(10.65)", "($10.65)", "1,000", "0.0123", "0.1234567", "--", "N/A", "Missing", "",
		"-0.00", ".5", "5.", "abc", "1e5", "92233720368547758.08", "+(5)",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDecimal(s)
		if refused := d.Present && d.Placeholder == "" && !d.Number(); d.Text != strings.TrimSpace(s) || (err != nil) != refused {
			t.Fatalf("ParseDecimal(%q) = %+v, %v", s, d, err)
		}
		if !d.Number() {
			if d.Canon() != "" {
				t.Fatalf("%q is not a number, but its canonical spelling is %q", s, d.Canon())
			}
			return
		}
		back, err := ParseDecimal(d.Canon())
		if err != nil || back.Canon() != d.Canon() {
			t.Fatalf("%q reads as %q, which reads as %q, %v", s, d.Canon(), back.Canon(), err)
		}
		cents, centsOK := d.Cents()
		micros, microsOK := d.Micros()
		if centsOK && cents > -1<<40 && cents < 1<<40 && (!microsOK || micros != cents*10000) {
			t.Fatalf("%q is %d cents but %d millionths (%v)", s, cents, micros, microsOK)
		}
		exact, _ := new(big.Rat).SetString(d.Canon())
		want, _ := exact.Float64() // the nearest float64, or an infinity
		if f, ok := d.Float(); ok != !math.IsInf(want, 0) || ok && f != want {
			t.Fatalf("%q as a float is %v, %v; its value's nearest is %v", s, f, ok, want)
		}
	})
}

// What ParseOption reads, written as an OCC symbol, is the same contract
// to schwab.ParseOCC.
func FuzzParseOption(f *testing.F) {
	for _, s := range []string{"XYZ 09/04/2026 17.50 P", "ABCD 1/2/2027 0.125 C", "SPXW 12/31/2026 5000 C", "XYZ"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		o, ok := ParseOption(s)
		if !ok {
			return
		}
		exp, err := time.Parse(time.DateOnly, string(o.Expiration))
		if err != nil {
			t.Fatalf("%q expires %q", s, o.Expiration)
		}
		occ := fmt.Sprintf("%-6s%s%c%08d", o.Root, exp.Format("060102"), o.PutCall[0], o.StrikeThousandths)
		if back, ok := schwab.ParseOCC(occ); !ok || back != o {
			t.Fatalf("%q reads as %+v, whose OCC symbol %q reads as %+v, %v", s, o, occ, back, ok)
		}
	})
}
