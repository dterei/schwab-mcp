package lookup

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// position decodes js, a position as GET /accounts sends one.
func position(t *testing.T, js string) schwab.Position {
	t.Helper()
	var p schwab.Position
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A position's row: its quantity and value signed as the side it holds, its
// average cost, its open profit or loss, positive for a gain, and its cost,
// its value less its open profit or loss. A figure that could not be read,
// or that its side lacks, is none, and so is what needs it.
func TestHoldingOf(t *testing.T) {
	const long = `"longQuantity":12,"shortQuantity":0,"averagePrice":251.3075,"marketValue":3748.92,"longOpenProfitLoss":733.23,` +
		`"instrument":{"assetType":"COLLECTIVE_INVESTMENT","symbol":"VTI"}`
	const short = `"longQuantity":0,"shortQuantity":100,"averagePrice":52.5,"marketValue":-5000,"shortOpenProfitLoss":250,` +
		`"instrument":{"assetType":"EQUITY","symbol":"XYZ"}`
	for _, tc := range []struct {
		name, position string
		want           string // quantity, average cost, value, cost and open profit or loss
		missing        string // the problem of the side's figure that it lacks
	}{
		{"long", "{" + long + "}", "12 251.3075 3748.92 3015.69 733.23", ""},
		{"short", "{" + short + "}", "-100 52.50 -5000.00 -5250.00 250.00", ""},
		{
			"a short put",
			`{"longQuantity":0,"shortQuantity":2,"averagePrice":1.6,"marketValue":-310,"shortOpenProfitLoss":10,` +
				`"instrument":{"assetType":"OPTION","symbol":"XYZ   260619P00045000"}}`,
			"-2 1.60 -310.00 -320.00 10.00", "",
		},
		{
			"a long call",
			`{"longQuantity":3,"shortQuantity":0,"averagePrice":2.15,"marketValue":750,"longOpenProfitLoss":105,` +
				`"instrument":{"assetType":"OPTION","symbol":"XYZ   260619C00050000"}}`,
			"3 2.15 750.00 645.00 105.00", "",
		},
		{
			"binary noise",
			`{"longQuantity":0.30000000000000004,"shortQuantity":0,"averagePrice":1.23456789,"marketValue":-236.999999999999,` +
				`"longOpenProfitLoss":81.29223999999999,"instrument":{"assetType":"EQUITY","symbol":"X"}}`,
			"0.3 1.234568 -237.00 -318.29 81.29", "",
		},
		{
			"a long position without its open profit or loss",
			"{" + strings.Replace(long, `"longOpenProfitLoss":733.23,`, ``, 1) + "}",
			"12 251.3075 3748.92 - -", "longOpenProfitLoss: missing, though Schwab sends it on every long position",
		},
		{
			"a long position with the short side's",
			"{" + strings.Replace(long, `"longOpenProfitLoss":733.23`, `"shortOpenProfitLoss":733.23`, 1) + "}",
			"12 251.3075 3748.92 - -", "longOpenProfitLoss: missing, though Schwab sends it on every long position",
		},
		{
			"a short position without its open profit or loss",
			"{" + strings.Replace(short, `"shortOpenProfitLoss":250,`, ``, 1) + "}",
			"-100 52.50 -5000.00 - -", "shortOpenProfitLoss: missing, though Schwab sends it on every short position",
		},
		{
			"a short position's open profit or loss sent as null",
			"{" + strings.Replace(short, `"shortOpenProfitLoss":250`, `"shortOpenProfitLoss":null`, 1) + "}",
			"-100 52.50 -5000.00 - -", "shortOpenProfitLoss: missing, though Schwab sends it on every short position",
		},
		{
			"a position of neither side",
			`{"longQuantity":0,"shortQuantity":0,"averagePrice":0,"marketValue":0,"instrument":{"assetType":"EQUITY","symbol":"X"}}`,
			"0 0.00 0.00 0.00 0.00", "",
		},
		{"an unreadable value", "{" + strings.Replace(long, `3748.92`, `"3748.92"`, 1) + "}", "12 251.3075 - - 733.23", ""},
		{"no value", "{" + strings.Replace(long, `"marketValue":3748.92,`, ``, 1) + "}", "12 251.3075 - - 733.23", ""},
		{"an unreadable average cost", "{" + strings.Replace(long, `251.3075`, `true`, 1) + "}", "12 - 3748.92 3015.69 733.23", ""},
		{"no average cost", "{" + strings.Replace(long, `"averagePrice":251.3075,`, ``, 1) + "}", "12 - 3748.92 3015.69 733.23", ""},
		{"an unreadable quantity", "{" + strings.Replace(long, `"longQuantity":12`, `"longQuantity":"12"`, 1) + "}", "- 251.3075 3748.92 - -", ""},
		{
			"an unreadable short quantity",
			"{" + strings.Replace(short, `"shortQuantity":100`, `"shortQuantity":"100"`, 1) + "}",
			"- 52.50 -5000.00 - -", "",
		},
		{
			"an unreadable open profit or loss",
			"{" + strings.Replace(long, `733.23`, `"733.23"`, 1) + "}",
			"12 251.3075 3748.92 - -", "",
		},
		{"not an object", `null`, "- - - - -", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := position(t, tc.position)
			h := HoldingOf(p)
			if got := fmt.Sprintf("%s %s %s %s %s", h.Quantity, h.Price, h.Value, h.Cost, h.Gain); got != tc.want {
				t.Errorf("got %q, want %q (problems %v)", got, tc.want, p.Problems)
			}
			_, missing := OpenPL(p)
			got := ""
			if len(missing) > 0 {
				got = missing[0].String()
			}
			if got != tc.missing || len(missing) > 1 {
				t.Errorf("missing %v, want %q", missing, tc.missing)
			}
		})
	}
}
