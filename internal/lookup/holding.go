package lookup

import (
	"encoding/json"
	"slices"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Holding is what schwabtx's positions table shows of a position.
type Holding struct {
	Symbol, Description string
	Quantity            Qty
	Price               Price
	Value, Cost, Gain   Money
}

// HoldingOf returns p's holding: its quantity and market value, negative
// for a short position, the value with an option's multiplier in it; its
// average price, the tax-lot average per share with fees and adjustments
// in it, what a share cost or, short, brought in; its open profit or loss,
// positive for a gain whether long or short; and its cost, the value less
// the open profit or loss, which for a short position is its proceeds,
// negative. Each is none when a figure that it needs could not be read, or
// was not sent. The cost needs no multiplier, which positions do not
// carry, and value - cost = open profit or loss to the cent, since the
// value and the open profit or loss are rounded first.
func HoldingOf(p schwab.Position) Holding {
	ps := p.Problems
	h := Holding{Symbol: p.Instrument.Symbol, Description: p.Instrument.Description}
	if !Unreadable(ps, "longQuantity") && !Unreadable(ps, "shortQuantity") {
		h.Quantity = QtyOf(p.LongQuantity - p.ShortQuantity)
	}
	if !Unreadable(ps, "averagePrice") {
		h.Price = PriceOf(p.AveragePrice)
	}
	if !Unreadable(ps, "marketValue") {
		h.Value = Amount(p.MarketValue)
	}
	h.Gain, _ = OpenPL(p)
	h.Cost = h.Value.Minus(h.Gain)
	return h
}

// OpenPL returns p's open profit or loss: longOpenProfitLoss plus
// shortOpenProfitLoss, since a position carries only its own side's, and
// the other's is absent, which reads as 0. It is none when either could
// not be read, or a quantity, which says which sides p holds; or when p
// lacks the figure of a side that it holds, which Schwab sends on every
// position of that side. missing has a problem for each such figure, since
// no problem of p's names it, with its path in p.
func OpenPL(p schwab.Position) (gain Money, missing []schwab.Problem) {
	ps := p.Problems
	for _, key := range []string{"longQuantity", "shortQuantity", "longOpenProfitLoss", "shortOpenProfitLoss"} {
		if Unreadable(ps, key) {
			return Money{}, nil
		}
	}
	// Whether each figure was sent, which the record's 0 does not say;
	// what could not be read of them is a problem of p's, above.
	var sent struct {
		Long  *float64 `json:"longOpenProfitLoss"`
		Short *float64 `json:"shortOpenProfitLoss"`
	}
	_ = json.Unmarshal(p.Raw, &sent)
	if p.LongQuantity != 0 && sent.Long == nil {
		missing = append(missing, schwab.Problem{Path: "longOpenProfitLoss", What: "missing, though Schwab sends it on every long position"})
	}
	if p.ShortQuantity != 0 && sent.Short == nil {
		missing = append(missing, schwab.Problem{Path: "shortOpenProfitLoss", What: "missing, though Schwab sends it on every short position"})
	}
	if missing != nil {
		return Money{}, missing
	}
	return Amount(p.LongOpenProfitLoss).Plus(Amount(p.ShortOpenProfitLoss)), nil
}

// Problems returns the problems of a and of its positions, as
// AccountProblems does, with one for each open profit or loss that a
// position lacks of a side that it holds, which Schwab sends on every
// position of that side.
func Problems(a schwab.Account) []schwab.Problem {
	ps := AccountProblems(a)
	for _, p := range a.SecuritiesAccount.Positions {
		_, missing := OpenPL(p)
		for _, m := range missing {
			ps = append(ps, schwab.Problem{Path: "securitiesAccount.positions." + m.Path, What: m.What})
		}
	}
	return ps
}

// AccountProblems returns the problems of a and of its positions.
func AccountProblems(a schwab.Account) []schwab.Problem {
	ps := slices.Clone(a.Problems)
	for _, pos := range a.SecuritiesAccount.Positions {
		for _, p := range pos.Problems {
			path := "securitiesAccount.positions"
			if p.Path != "" {
				path += "." + p.Path
			}
			ps = append(ps, schwab.Problem{Path: path, What: p.What})
		}
	}
	return ps
}
