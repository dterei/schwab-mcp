package schwab

import (
	"encoding/json"
	"time"
)

// AssetType is the kind of an instrument. Schwab may send kinds beyond these
// constants, so treat it as an open set.
type AssetType string

// The asset types in Schwab's spec. Exchange-traded funds are
// AssetCollectiveInvestment, and money market funds held as positions are
// AssetMutualFund. Cash and fees in a transaction are AssetCurrency.
const (
	AssetEquity               AssetType = "EQUITY"
	AssetOption               AssetType = "OPTION"
	AssetIndex                AssetType = "INDEX"
	AssetMutualFund           AssetType = "MUTUAL_FUND"
	AssetCashEquivalent       AssetType = "CASH_EQUIVALENT"
	AssetFixedIncome          AssetType = "FIXED_INCOME"
	AssetCurrency             AssetType = "CURRENCY"
	AssetCollectiveInvestment AssetType = "COLLECTIVE_INVESTMENT"
	AssetForex                AssetType = "FOREX"
	AssetFuture               AssetType = "FUTURE"
	AssetProduct              AssetType = "PRODUCT"
)

// Instrument is what a position holds or a transaction leg moves, in one
// type for every asset type, with the fields seen in Schwab's answers. Other
// fields, such as a bond's maturity, are left in the record's Raw until an
// answer shows how Schwab sends them.
//
// An instrument is Schwab's reference data as of the request, not of the
// trade: ClosingPrice is a price as of the request, so the same transaction
// reads differently from day to day, and a delisted security can come back
// with its CUSIP as its symbol.
type Instrument struct {
	AssetType AssetType `json:"assetType"`
	// Symbol is the ticker, CURRENCY_USD for cash and fees, or an option's
	// symbol: for an option on a stock, fund or index, its OCC symbol, the
	// root padded to six characters (AAPL  260320C00180000), and for an
	// option on a future, Schwab's own form (/ESH25_P5050:XCME), which gives
	// the future's month but not the day of expiration.
	Symbol        string `json:"symbol"`
	UniformSymbol string `json:"uniformSymbol,omitzero"`
	// CUSIP is on positions, not on transactions.
	CUSIP       string `json:"cusip,omitzero"`
	Description string `json:"description,omitzero"`
	// InstrumentID is on transactions, not on positions. Cash is 1.
	InstrumentID int64 `json:"instrumentId,omitzero"`
	// Type is the kind within the asset type, such as EXCHANGE_TRADED_FUND,
	// COMMON_STOCK, NO_LOAD_TAXABLE (a money market fund) or VANILLA (an
	// option).
	Type string `json:"type,omitzero"`
	// Status has been seen only as ACTIVE.
	Status string `json:"status,omitzero"`

	// NetChange is the day's change in price, on positions.
	NetChange float64 `json:"netChange,omitzero"`
	// ClosingPrice is a price as of the request, on transactions; whether it
	// is the last close or a live price is not known.
	ClosingPrice float64 `json:"closingPrice,omitzero"`

	// The rest describe options. A position's option carries only PutCall
	// and UnderlyingSymbol of them; an OCC symbol holds the strike and
	// expiration, and the multiplier is not sent.
	PutCall     string  `json:"putCall,omitzero"`
	StrikePrice float64 `json:"strikePrice,omitzero"`
	// ExpirationDate is midnight in New York on the day of expiration.
	ExpirationDate time.Time `json:"expirationDate,omitzero"`
	// OptionPremiumMultiplier is 100 for most options, and 50 on options on
	// E-mini S&P 500 futures.
	OptionPremiumMultiplier float64             `json:"optionPremiumMultiplier,omitzero"`
	UnderlyingSymbol        string              `json:"underlyingSymbol,omitzero"`
	UnderlyingCUSIP         string              `json:"underlyingCusip,omitzero"`
	OptionDeliverables      []OptionDeliverable `json:"optionDeliverables,omitempty"`

	// problems are what UnmarshalJSON could not read, for the record that
	// holds the instrument to report.
	problems []Problem
}

// UnmarshalJSON decodes an instrument leniently. It never fails; what it
// cannot read goes to the problems of the record that holds it. Null leaves
// the instrument zero, for the record that holds it to report as missing.
func (in *Instrument) UnmarshalJSON(b []byte) error {
	if isNull(b) {
		*in = Instrument{}
		return nil
	}
	type plain Instrument
	var w struct {
		plain
		ExpirationDate json.RawMessage `json:"expirationDate"`
	}
	isObject, err := decode(b, &w)
	*in = Instrument(w.plain)
	ps := recordProblems(b, in, isObject, err, "expirationDate")
	in.ExpirationDate, _ = readTime(w.ExpirationDate, "expirationDate", &ps)
	in.problems = ps
	return nil
}

// OptionDeliverable is what one option contract delivers. Transactions send
// the root symbol, the strike percent, the deliverable number and units,
// and, in a capture of August 2026 though not in fills of January 2025, the
// deliverable itself; order legs send a symbol and units.
type OptionDeliverable struct {
	RootSymbol        string       `json:"rootSymbol,omitzero"`
	StrikePercent     float64      `json:"strikePercent,omitzero"`
	DeliverableNumber int          `json:"deliverableNumber,omitzero"`
	DeliverableUnits  float64      `json:"deliverableUnits"`
	Deliverable       *Deliverable `json:"deliverable,omitempty"`
	Symbol            string       `json:"symbol,omitzero"`
	APICurrencyType   string       `json:"apiCurrencyType,omitzero"`
	AssetType         AssetType    `json:"assetType,omitzero"`
}

// Deliverable is the instrument that an option delivers, such as its
// underlying stock. It is not an Instrument, which can hold deliverables,
// so that no type holds itself.
type Deliverable struct {
	AssetType     AssetType `json:"assetType"`
	Symbol        string    `json:"symbol"`
	UniformSymbol string    `json:"uniformSymbol,omitzero"`
	CUSIP         string    `json:"cusip,omitzero"`
	Description   string    `json:"description,omitzero"`
	InstrumentID  int64     `json:"instrumentId,omitzero"`
	Type          string    `json:"type,omitzero"`
	Status        string    `json:"status,omitzero"`
	ClosingPrice  float64   `json:"closingPrice,omitzero"`
}
