package lookup

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// account decodes js, an account as GET /accounts sends one.
func account(t *testing.T, js string) schwab.Account {
	t.Helper()
	var a schwab.Account
	if err := json.Unmarshal([]byte(js), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// marginAccount returns a margin account with current balances cur, a JSON
// object's members, and an aggregated balance agg, or none for "".
func marginAccount(cur, agg string) string {
	js := `{"securitiesAccount":{"type":"MARGIN","accountNumber":"12345678","roundTrips":0,"isDayTrader":false,` +
		`"isClosingOnlyRestricted":false,"pfcbFlag":false,"initialBalances":{},"currentBalances":{` + cur + `},"projectedBalances":{}}`
	if agg != "" {
		js += `,"aggregatedBalance":{` + agg + `}`
	}
	return js + "}"
}

// The value, cash and buying power of an account, from the current
// balances, and none for a figure that Schwab did not send or that could
// not be read.
func TestBalanceOf(t *testing.T) {
	// The market values of holdings of 20000 long and 12000 short, worth
	// 8000 together.
	const held = `"longMarketValue":20000,"shortMarketValue":-12000,"longOptionMarketValue":0,"shortOptionMarketValue":0,` +
		`"mutualFundValue":0,"bondValue":0`
	for _, tc := range []struct {
		name, account string
		want          string // the value, cash and buying power, and why one is none, if not for a problem
	}{
		{
			"current value",
			marginAccount(`"liquidationValue":5503.84,"cashBalance":5210.44,"buyingPower":8258.84`, `"currentLiquidationValue":5503.85`),
			"5503.84 5210.44 8258.84",
		},
		{
			"aggregated value",
			marginAccount(`"cashBalance":1,"buyingPower":2`, `"currentLiquidationValue":5503.85`),
			"5503.85 1.00 2.00",
		},
		{"no value", marginAccount(`"cashBalance":1,"buyingPower":2`, ""), "- 1.00 2.00; unsent value"},
		{
			"the aggregated liquidationValue alone",
			marginAccount(`"cashBalance":1,"buyingPower":2`, `"liquidationValue":5503.85`),
			"- 1.00 2.00; unsent value",
		},
		{
			"the initial value alone",
			strings.Replace(marginAccount(`"cashBalance":1,"buyingPower":2`, ""),
				`"initialBalances":{}`, `"initialBalances":{"liquidationValue":5500.63,"accountValue":5500.63}`, 1),
			"- 1.00 2.00; unsent value",
		},
		{
			"unreadable current value",
			marginAccount(`"liquidationValue":"5503.84","cashBalance":1,"buyingPower":2`, `"currentLiquidationValue":5503.85`),
			"5503.85 1.00 2.00",
		},
		{
			"unreadable values",
			marginAccount(`"liquidationValue":"5503.84","cashBalance":1,"buyingPower":2`, `"currentLiquidationValue":"5503.85"`),
			"- 1.00 2.00",
		},

		{
			"a cash account",
			`{"securitiesAccount":{"type":"CASH","accountNumber":"1","currentBalances":{"cashBalance":1532.17,"cashAvailableForTrading":1500}}}`,
			"- 1532.17 1500.00; unsent value",
		},
		{"cash 0", marginAccount(`"cashBalance":0,"buyingPower":0`, ""), "- 0.00 0.00; unsent value"},
		{
			"cash in the margin and short balances",
			marginAccount(`"cashBalance":0,"marginBalance":-12345.67,"shortBalance":12846.12,"buyingPower":1`, ""),
			"- 500.45 1.00; unsent value",
		},
		{"a debit", marginAccount(`"cashBalance":0,"marginBalance":-100,"buyingPower":1`, ""), "- -100.00 1.00; unsent value"},
		{"no cashBalance", marginAccount(`"marginBalance":0,"shortBalance":0,"buyingPower":1`, ""), "- - 1.00; unsent value, cash"},
		{
			"cashBalance beside a margin balance",
			marginAccount(`"cashBalance":5210.44,"marginBalance":-5,"buyingPower":1`, ""),
			"- 5210.44 1.00; unsent value",
		},
		{
			"a negative cashBalance beside a margin balance",
			marginAccount(`"cashBalance":-50,"marginBalance":-5,"shortBalance":20,"buyingPower":1`, ""),
			"- -50.00 1.00; unsent value",
		},
		{"unreadable cashBalance", marginAccount(`"cashBalance":"1532.17","buyingPower":1`, ""), "- - 1.00; unsent value"},
		{
			"unreadable shortBalance",
			marginAccount(`"cashBalance":0,"marginBalance":-12345.67,"shortBalance":"12846.12","buyingPower":1`, ""),
			"- - 1.00; unsent value",
		},
		{
			"unreadable marginBalance",
			marginAccount(`"cashBalance":0,"marginBalance":"-12345.67","shortBalance":12846.12,"buyingPower":1`, ""),
			"- - 1.00; unsent value",
		},
		{
			"unreadable marginBalance beside cash",
			marginAccount(`"cashBalance":7,"marginBalance":"-12345.67","buyingPower":1`, ""),
			"- 7.00 1.00; unsent value",
		},
		{
			"unreadable shortBalance without a margin balance",
			marginAccount(`"cashBalance":0,"shortBalance":"5","buyingPower":1`, ""),
			"- 0.00 1.00; unsent value",
		},

		{
			"cash that the value leaves",
			marginAccount(`"liquidationValue":8500.45,"cashBalance":0,"marginBalance":-12345.67,"shortBalance":12846.12,"buyingPower":1,`+held, ""),
			"8500.45 500.45 1.00",
		},
		{
			"cash a cent from what the value leaves",
			marginAccount(`"liquidationValue":8500.46,"cashBalance":500.45,"buyingPower":1,`+held, ""),
			"8500.46 500.45 1.00",
		},
		{
			"cash a cent above what the value leaves",
			marginAccount(`"liquidationValue":8500.44,"cashBalance":500.45,"buyingPower":1,`+held, ""),
			"8500.44 500.45 1.00",
		},
		{
			"cash two cents below what the value leaves",
			marginAccount(`"liquidationValue":8500.45,"cashBalance":500.43,"buyingPower":1,`+held, ""),
			"8500.45 - 1.00; cash differs",
		},
		{
			"cash two cents above what the value leaves",
			marginAccount(`"liquidationValue":8500.45,"cashBalance":500.47,"buyingPower":1,`+held, ""),
			"8500.45 - 1.00; cash differs",
		},
		{
			"long options and bonds",
			marginAccount(`"liquidationValue":9000.45,"cashBalance":500.45,"buyingPower":1,`+
				strings.NewReplacer(`"longOptionMarketValue":0`, `"longOptionMarketValue":300`, `"bondValue":0`, `"bondValue":200`).Replace(held), ""),
			"9000.45 500.45 1.00",
		},
		{
			// A real margin account of 2025, jwplatta/schwab_rb's fixture,
			// which holds long and short options and nothing else.
			"options long and short",
			marginAccount(`"liquidationValue":50946.39,"cashBalance":51420.89,"longMarketValue":0,"shortMarketValue":0,`+
				`"longOptionMarketValue":436,"shortOptionMarketValue":-910.5,"mutualFundValue":0,"bondValue":0,"buyingPower":88853.04`, ""),
			"50946.39 51420.89 88853.04",
		},
		{
			"no cashBalance beside every market value",
			marginAccount(`"liquidationValue":8500.45,"buyingPower":1,`+held, ""),
			"8500.45 - 1.00; unsent cash",
		},
		{
			"savings beside the cash",
			marginAccount(`"liquidationValue":11000.45,"cashBalance":500.45,"savings":2500,"moneyMarketFund":0,"buyingPower":1,`+held, ""),
			"11000.45 - 1.00; cash differs",
		},
		{
			"short proceeds beside the cash",
			marginAccount(`"liquidationValue":1200,"cashBalance":1000,"shortBalance":5000,"marginBalance":0,"buyingPower":1,`+
				`"longMarketValue":0,"shortMarketValue":-4800,"longOptionMarketValue":0,"shortOptionMarketValue":0,"mutualFundValue":0,"bondValue":0`, ""),
			"1200.00 - 1.00; cash differs",
		},
		{
			"a market value missing",
			marginAccount(`"liquidationValue":11000.45,"cashBalance":500.45,"savings":2500,"buyingPower":1,`+
				strings.Replace(held, `"bondValue":0`, `"equity":0`, 1), ""),
			"11000.45 500.45 1.00",
		},
		{
			"a market value unreadable",
			marginAccount(`"liquidationValue":11000.45,"cashBalance":500.45,"savings":2500,"buyingPower":1,`+
				strings.Replace(held, `"bondValue":0`, `"bondValue":"0"`, 1), ""),
			"11000.45 500.45 1.00",
		},

		{"no buying power", marginAccount(`"cashBalance":1`, ""), "- 1.00 -; unsent value, buying power"},
		{"unreadable buying power", marginAccount(`"cashBalance":1,"buyingPower":"2"`, ""), "- 1.00 -; unsent value"},
		{
			"a cash account's buyingPower",
			`{"securitiesAccount":{"type":"CASH","accountNumber":"1","currentBalances":{"cashBalance":1,"buyingPower":2}}}`,
			"- 1.00 -; unsent value, buying power",
		},
		{
			"another type",
			strings.Replace(marginAccount(`"cashBalance":1,"buyingPower":2,"cashAvailableForTrading":3`, ""), `"MARGIN"`, `"FUTURES"`, 1),
			"- 1.00 -; unsent value",
		},

		{
			"current balances sent twice",
			strings.Replace(marginAccount(`"liquidationValue":1,"cashBalance":1,"buyingPower":1`, `"currentLiquidationValue":5503.85`),
				`"projectedBalances"`, `"currentBalances":{"liquidationValue":2},"projectedBalances"`, 1),
			"5503.85 - -",
		},
		{"not an object", `null`, "- - -"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := account(t, tc.account)
			b := BalanceOf(a)
			got := fmt.Sprintf("%s %s %s", b.Value, b.Cash, b.BuyingPower)
			var unsent []string
			for _, f := range []struct {
				name string
				no   bool
			}{{"value", b.NoValue}, {"cash", b.NoCash}, {"buying power", b.NoBuyingPower}} {
				if f.no {
					unsent = append(unsent, f.name)
				}
			}
			if len(unsent) > 0 {
				got += "; unsent " + strings.Join(unsent, ", ")
			}
			if b.CashDiffers {
				got += "; cash differs"
			}
			if got != tc.want {
				t.Errorf("got %q, want %q (problems %v)", got, tc.want, a.Problems)
			}
		})
	}
}
