package lookup

import (
	"slices"
	"strings"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Covers reports whether p is at path, or at a key above it: whether what
// Schwab sent at path could not be read, or is uncertain.
func Covers(p schwab.Problem, path string) bool {
	return p.Path == "" || p.Path == path || strings.HasPrefix(path, p.Path+".")
}

// Unreadable reports whether ps, the problems of a record, cover path.
func Unreadable(ps []schwab.Problem, path string) bool {
	return slices.ContainsFunc(ps, func(p schwab.Problem) bool { return Covers(p, path) })
}

// figure returns v, the figure at path in a record with the problems ps:
// none when Schwab did not send it, or it could not be read.
func figure(ps []schwab.Problem, path string, v *float64) Money {
	if Unreadable(ps, path) {
		return Money{}
	}
	return MoneyOf(v)
}

// unsent reports whether Schwab did not send v, the figure at path in a
// record with the problems ps, though nothing that it sent there was
// unreadable.
func unsent(ps []schwab.Problem, path string, v *float64) bool {
	return v == nil && !Unreadable(ps, path)
}

// currentAt is where an account keeps its current balances.
const currentAt = "securitiesAccount.currentBalances."

// Balance is an account's value, cash and buying power, as of the request,
// as schwabtx's accounts and balances tables show them.
type Balance struct {
	Value, Cash, BuyingPower Money
	// Whether Schwab did not send the figure that each of the three would
	// be, though nothing that it sent there was unreadable.
	NoValue, NoCash, NoBuyingPower bool
	// CashDiffers is whether the cash is none because it is not what the
	// account's value leaves once its holdings are taken out.
	CashDiffers bool
}

// BalanceOf returns a's balance.
func BalanceOf(a schwab.Account) Balance {
	ps, cur, agg := a.Problems, a.SecuritiesAccount.CurrentBalances, a.AggregatedBalance
	var b Balance

	// The value is the current liquidation value, or else the aggregated
	// balance's current one, which a margin account's current balances can
	// lack. The initial balances' liquidation value is as of the start of
	// the session, and the aggregated balance's other one as of a time that
	// is not known.
	b.Value = figure(ps, currentAt+"liquidationValue", cur.LiquidationValue)
	if !b.Value.ok {
		b.Value = figure(ps, "aggregatedBalance.currentLiquidationValue", agg.CurrentLiquidationValue)
	}
	b.NoValue = unsent(ps, currentAt+"liquidationValue", cur.LiquidationValue) &&
		unsent(ps, "aggregatedBalance.currentLiquidationValue", agg.CurrentLiquidationValue)

	// A cash that is not what the value leaves once the holdings are taken
	// out, by more than a cent, is not all of the account's cash, or not
	// cash alone.
	b.Cash, b.NoCash = cash(ps, cur), unsent(ps, currentAt+"cashBalance", cur.CashBalance)
	if left := leftOver(ps, cur); b.Cash.ok && left.ok {
		if d := b.Cash.cents - left.cents; d < -1 || d > 1 {
			b.Cash, b.CashDiffers = Money{}, true
		}
	}

	// The buying power is a margin account's buyingPower, for stock that it
	// can buy on margin. A cash account has none, and what it has to trade
	// with is cashAvailableForTrading. Another type has neither.
	var key string
	var v *float64
	switch a.SecuritiesAccount.Type {
	case "MARGIN":
		key, v = "buyingPower", cur.BuyingPower
	case "CASH":
		key, v = "cashAvailableForTrading", cur.CashAvailableForTrading
	}
	if key != "" {
		b.BuyingPower, b.NoBuyingPower = figure(ps, currentAt+key, v), unsent(ps, currentAt+key, v)
	}
	return b
}

// cash returns the cash in cur, the current balances of an account with
// the problems ps: cashBalance or, when that is 0 and marginBalance is
// sent, marginBalance plus shortBalance, where a margin account with short
// positions or a debit can keep its cash (jkoelker/schwab-mcp#222). An
// absent shortBalance counts as 0, but an absent cashBalance is none, not
// 0: a margin account's answer of 2024 sent none, with marginBalance and
// shortBalance both 0, though the account held cash. Money market funds
// are positions, not cash.
func cash(ps []schwab.Problem, cur schwab.Balances) Money {
	c := figure(ps, currentAt+"cashBalance", cur.CashBalance)
	if !c.ok || c.cents != 0 || cur.MarginBalance == nil && !Unreadable(ps, currentAt+"marginBalance") {
		return c
	}
	short := Money{ok: true} // an absent shortBalance counts as 0
	if cur.ShortBalance != nil || Unreadable(ps, currentAt+"shortBalance") {
		short = figure(ps, currentAt+"shortBalance", cur.ShortBalance)
	}
	return figure(ps, currentAt+"marginBalance", cur.MarginBalance).Plus(short)
}

// leftOver returns what the value in cur, the current balances of an
// account with the problems ps, leaves once the market values of its
// holdings are taken out, or none unless Schwab sent each of those
// figures. In every answer seen, the value is cashBalance plus those
// market values, moneyMarketFund and savings, the last two always 0, so
// that what is left is the cash. Where it is not, what the cash leaves out
// is not known: savings or moneyMarketFund, whatever they hold, or short
// proceeds in shortBalance beside a cashBalance that is not 0, which #222
// does not cover.
func leftOver(ps []schwab.Problem, cur schwab.Balances) Money {
	left := figure(ps, currentAt+"liquidationValue", cur.LiquidationValue)
	for _, held := range []struct {
		key string
		v   *float64
	}{
		{"longMarketValue", cur.LongMarketValue},
		{"shortMarketValue", cur.ShortMarketValue},
		{"longOptionMarketValue", cur.LongOptionMarketValue},
		{"shortOptionMarketValue", cur.ShortOptionMarketValue},
		{"mutualFundValue", cur.MutualFundValue},
		{"bondValue", cur.BondValue},
	} {
		left = left.Minus(figure(ps, currentAt+held.key, held.v))
	}
	return left
}
