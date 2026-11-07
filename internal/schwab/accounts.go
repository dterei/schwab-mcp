package schwab

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// AccountHash stands for one account in the path of the endpoints that read
// one account, such as Account and Transactions. AccountNumbers gives each
// account's hash; Schwab refuses an account number in its place.
type AccountHash string

// AccountNumber is an account shared at login, from GET
// /accounts/accountNumbers.
type AccountNumber struct {
	// AccountNumber is the account's number, as a string of digits.
	AccountNumber string `json:"accountNumber"`
	// HashValue is the account's hash. Whether it stays the same across
	// logins is not known, so keep data by account number and look the hash
	// up again for each run.
	HashValue AccountHash `json:"hashValue"`

	// Problems and Raw are as the package doc describes.
	Problems []Problem       `json:"problems,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes one account number leniently, as the package doc
// describes. It never fails.
func (a *AccountNumber) UnmarshalJSON(b []byte) error {
	type plain AccountNumber
	var w struct{ plain }
	isObject, err := decode(b, &w)
	*a = AccountNumber(w.plain)
	ps := recordProblems(b, a, isObject, err)
	if isObject {
		need(&ps, a.AccountNumber != "", "accountNumber")
		need(&ps, a.HashValue != "", "hashValue")
	}
	a.Problems, a.Raw = ps, copyRaw(b)
	return nil
}

// Account is an account with its balances and, when asked for, its
// positions, from GET /accounts or /accounts/{hash}.
type Account struct {
	SecuritiesAccount SecuritiesAccount `json:"securitiesAccount"`
	// AggregatedBalance is not in Schwab's spec, and some answers from 2024
	// lack it.
	AggregatedBalance AggregatedBalance `json:"aggregatedBalance,omitzero"`

	// Problems and Raw are as the package doc describes. Each position has
	// its own.
	Problems []Problem       `json:"problems,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes one account leniently, as the package doc
// describes. It never fails.
func (a *Account) UnmarshalJSON(b []byte) error {
	type plain Account
	var w struct{ plain }
	isObject, err := decode(b, &w)
	*a = Account(w.plain)
	ps := recordProblems(b, a, isObject, err)
	if isObject {
		need(&ps, a.SecuritiesAccount.AccountNumber != "", "securitiesAccount.accountNumber")
	}
	a.Problems, a.Raw = ps, copyRaw(b)
	return nil
}

// SecuritiesAccount is an account's details.
type SecuritiesAccount struct {
	// Type is CASH or MARGIN. An IRA is one of the two; nothing in the API
	// says which accounts are retirement accounts.
	Type          string `json:"type"`
	AccountNumber string `json:"accountNumber"`

	RoundTrips              int  `json:"roundTrips"`
	IsDayTrader             bool `json:"isDayTrader"`
	IsClosingOnlyRestricted bool `json:"isClosingOnlyRestricted"`
	PfcbFlag                bool `json:"pfcbFlag"`
	// IsPortfolioMargin and IsIntradayMargin are not in the spec, and of
	// the answers seen, only those of August 2026 have them.
	IsPortfolioMargin *bool `json:"isPortfolioMargin,omitempty"`
	IsIntradayMargin  *bool `json:"isIntradayMargin,omitempty"`

	// Positions are what the account holds, when they were asked for. An
	// account that holds nothing has none, even then.
	Positions []Position `json:"positions,omitempty"`

	// InitialBalances are as of the start of the session, and stay so
	// through the evening and the weekend. CurrentBalances are as of the
	// request: outside regular hours they follow extended-hours and
	// overnight prices. ProjectedBalances hold buying power and cash
	// available, not totals.
	InitialBalances   Balances `json:"initialBalances"`
	CurrentBalances   Balances `json:"currentBalances"`
	ProjectedBalances Balances `json:"projectedBalances"`
}

// AggregatedBalance is the total value of one account, beside its details.
// Every field is nil when Schwab did not send it.
type AggregatedBalance struct {
	CurrentLiquidationValue *float64 `json:"currentLiquidationValue,omitempty"`
	LiquidationValue        *float64 `json:"liquidationValue,omitempty"`
	// The intraday buying power has been seen only in answers of August
	// 2026.
	IntradayBuyingPowerAmount        *float64 `json:"intradayBuyingPowerAmount,omitempty"`
	CurrentIntradayBuyingPowerAmount *float64 `json:"currentIntradayBuyingPowerAmount,omitempty"`
}

// Balances is one set of an account's balances: initial, current or
// projected. Which figures Schwab sends depends on the set and on whether
// the account is CASH or MARGIN, and zero is a common real value, so every
// figure is nil when Schwab did not send it.
//
// The account's total value is LiquidationValue. In CurrentBalances it is
// the sum of CashBalance, LongMarketValue, MutualFundValue (where money
// market funds count), LongOptionMarketValue, ShortOptionMarketValue,
// ShortMarketValue, BondValue, MoneyMarketFund and Savings. Equity is not
// the total, since a margin account's leaves options out. Some figures are
// not what their names say: in InitialBalances, MutualFundValue repeats a
// cash figure and a margin account's BondValue is not bonds; and a margin
// account with short positions or a debit can show a CashBalance of 0, with
// its cash in MarginBalance and ShortBalance.
type Balances struct {
	AccountValue                     *float64 `json:"accountValue,omitempty"`
	AccruedInterest                  *float64 `json:"accruedInterest,omitempty"`
	AvailableFunds                   *float64 `json:"availableFunds,omitempty"`
	AvailableFundsNonMarginableTrade *float64 `json:"availableFundsNonMarginableTrade,omitempty"`
	BondValue                        *float64 `json:"bondValue,omitempty"`
	BuyingPower                      *float64 `json:"buyingPower,omitempty"`
	BuyingPowerNonMarginableTrade    *float64 `json:"buyingPowerNonMarginableTrade,omitempty"`
	CashAvailableForTrading          *float64 `json:"cashAvailableForTrading,omitempty"`
	CashAvailableForWithdrawal       *float64 `json:"cashAvailableForWithdrawal,omitempty"`
	CashBalance                      *float64 `json:"cashBalance,omitempty"`
	CashCall                         *float64 `json:"cashCall,omitempty"`
	CashDebitCallValue               *float64 `json:"cashDebitCallValue,omitempty"`
	CashReceipts                     *float64 `json:"cashReceipts,omitempty"`
	DayTradingBuyingPower            *float64 `json:"dayTradingBuyingPower,omitempty"`
	DayTradingBuyingPowerCall        *float64 `json:"dayTradingBuyingPowerCall,omitempty"`
	DayTradingEquityCall             *float64 `json:"dayTradingEquityCall,omitempty"`
	Equity                           *float64 `json:"equity,omitempty"`
	EquityPercentage                 *float64 `json:"equityPercentage,omitempty"`
	IntradayBuyingPowerAmount        *float64 `json:"intradayBuyingPowerAmount,omitempty"`
	// IsInCall is a boolean, though the spec says a number.
	IsInCall                     *bool    `json:"isInCall,omitempty"`
	LiquidationValue             *float64 `json:"liquidationValue,omitempty"`
	LongMarginValue              *float64 `json:"longMarginValue,omitempty"`
	LongMarketValue              *float64 `json:"longMarketValue,omitempty"`
	LongNonMarginableMarketValue *float64 `json:"longNonMarginableMarketValue,omitempty"`
	LongOptionMarketValue        *float64 `json:"longOptionMarketValue,omitempty"`
	LongStockValue               *float64 `json:"longStockValue,omitempty"`
	MaintenanceCall              *float64 `json:"maintenanceCall,omitempty"`
	MaintenanceRequirement       *float64 `json:"maintenanceRequirement,omitempty"`
	Margin                       *float64 `json:"margin,omitempty"`
	MarginBalance                *float64 `json:"marginBalance,omitempty"`
	MarginEquity                 *float64 `json:"marginEquity,omitempty"`
	MoneyMarketFund              *float64 `json:"moneyMarketFund,omitempty"`
	MutualFundValue              *float64 `json:"mutualFundValue,omitempty"`
	OptionBuyingPower            *float64 `json:"optionBuyingPower,omitempty"`
	PendingDeposits              *float64 `json:"pendingDeposits,omitempty"`
	RegTCall                     *float64 `json:"regTCall,omitempty"`
	Savings                      *float64 `json:"savings,omitempty"`
	ShortBalance                 *float64 `json:"shortBalance,omitempty"`
	ShortMarginValue             *float64 `json:"shortMarginValue,omitempty"`
	ShortMarketValue             *float64 `json:"shortMarketValue,omitempty"`
	ShortOptionMarketValue       *float64 `json:"shortOptionMarketValue,omitempty"`
	ShortStockValue              *float64 `json:"shortStockValue,omitempty"`
	SMA                          *float64 `json:"sma,omitempty"`
	StockBuyingPower             *float64 `json:"stockBuyingPower,omitempty"`
	TotalCash                    *float64 `json:"totalCash,omitempty"`
	UnsettledCash                *float64 `json:"unsettledCash,omitempty"`
}

// Position is one holding of an account, as of the request.
//
// A position carries the figures of its own side: a long position's short
// figures are absent and read as 0, as they are. ShortQuantity is positive
// for a short position, while SettledShortQuantity is negative.
// MarketValue is signed and includes an option's multiplier. AveragePrice
// is the tax-lot average, fees and adjustments included; the cost basis is
// MarketValue - LongOpenProfitLoss for a long position and
// ShortOpenProfitLoss - MarketValue for a short one. AverageLongPrice and
// AverageShortPrice are trade prices, not cost basis. The currentDay
// figures are not reliable: a position sold to open that day can show its
// whole value as the day's profit or loss.
type Position struct {
	Instrument Instrument `json:"instrument"`

	LongQuantity                 float64 `json:"longQuantity"`
	ShortQuantity                float64 `json:"shortQuantity"`
	SettledLongQuantity          float64 `json:"settledLongQuantity"`
	SettledShortQuantity         float64 `json:"settledShortQuantity"`
	PreviousSessionLongQuantity  float64 `json:"previousSessionLongQuantity,omitzero"`
	PreviousSessionShortQuantity float64 `json:"previousSessionShortQuantity,omitzero"`
	// AgedQuantity has been seen only on money market funds.
	AgedQuantity float64 `json:"agedQuantity,omitzero"`

	AveragePrice            float64 `json:"averagePrice"`
	AverageLongPrice        float64 `json:"averageLongPrice,omitzero"`
	AverageShortPrice       float64 `json:"averageShortPrice,omitzero"`
	TaxLotAverageLongPrice  float64 `json:"taxLotAverageLongPrice,omitzero"`
	TaxLotAverageShortPrice float64 `json:"taxLotAverageShortPrice,omitzero"`

	MarketValue            float64 `json:"marketValue"`
	LongOpenProfitLoss     float64 `json:"longOpenProfitLoss,omitzero"`
	ShortOpenProfitLoss    float64 `json:"shortOpenProfitLoss,omitzero"`
	MaintenanceRequirement float64 `json:"maintenanceRequirement"`

	CurrentDayProfitLoss           float64 `json:"currentDayProfitLoss"`
	CurrentDayProfitLossPercentage float64 `json:"currentDayProfitLossPercentage"`
	CurrentDayCost                 float64 `json:"currentDayCost"`

	// Problems and Raw are as the package doc describes.
	Problems []Problem       `json:"problems,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes one position leniently, as the package doc
// describes. It never fails.
func (p *Position) UnmarshalJSON(b []byte) error {
	type plain Position
	// The keys every position has are read as raw JSON too, to tell an
	// absent key from 0.
	var w struct {
		plain
		LongQuantity  json.RawMessage `json:"longQuantity"`
		ShortQuantity json.RawMessage `json:"shortQuantity"`
		MarketValue   json.RawMessage `json:"marketValue"`
	}
	isObject, err := decode(b, &w)
	*p = Position(w.plain)
	ps := recordProblems(b, p, isObject, err, "longQuantity", "shortQuantity", "marketValue")
	hasLong := readField(w.LongQuantity, "longQuantity", &p.LongQuantity, &ps)
	hasShort := readField(w.ShortQuantity, "shortQuantity", &p.ShortQuantity, &ps)
	hasValue := readField(w.MarketValue, "marketValue", &p.MarketValue, &ps)
	if isObject {
		ps = within(ps, "instrument", p.Instrument.problems)
		need(&ps, p.Instrument.AssetType != "", "instrument.assetType")
		need(&ps, hasLong, "longQuantity")
		need(&ps, hasShort, "shortQuantity")
		need(&ps, hasValue, "marketValue")
	}
	p.Instrument.problems = nil
	p.Problems, p.Raw = ps, copyRaw(b)
	return nil
}

// AccountField names a set of fields that GET /accounts leaves out unless
// asked.
type AccountField string

// AccountFieldPositions asks for each account's positions.
const AccountFieldPositions AccountField = "positions"

// AccountNumbers lists the accounts shared at login, each with the hash
// that the endpoints for one account, such as Account and Transactions,
// take in place of its number.
func (c *Client) AccountNumbers(ctx context.Context) ([]AccountNumber, error) {
	r, err := c.get(ctx, "/trader/v1/accounts/accountNumbers", nil)
	if err != nil {
		return nil, err
	}
	return list[AccountNumber](r, "the account numbers")
}

// Accounts returns every account shared at login, with its balances and
// the fields asked for.
func (c *Client) Accounts(ctx context.Context, fields ...AccountField) ([]Account, error) {
	r, err := c.get(ctx, "/trader/v1/accounts", accountFields(fields))
	if err != nil {
		return nil, err
	}
	return list[Account](r, "the accounts")
}

// Account returns the account with the given hash, with its balances and
// the fields asked for.
func (c *Client) Account(ctx context.Context, hash AccountHash, fields ...AccountField) (Account, error) {
	if hash == "" {
		return Account{}, errors.New("no account hash")
	}
	r, err := c.get(ctx, "/trader/v1/accounts/"+url.PathEscape(string(hash)), accountFields(fields))
	if err != nil {
		return Account{}, err
	}
	return one[Account](r, "the account")
}

// accountFields returns the query that asks for fields.
func accountFields(fields []AccountField) url.Values {
	if len(fields) == 0 {
		return nil
	}
	names := make([]string, len(fields))
	for i, f := range fields {
		names[i] = string(f)
	}
	return url.Values{"fields": {strings.Join(names, ",")}}
}
