package schwab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TransactionType is the kind of a transaction. Schwab may send kinds
// beyond these constants, so treat it as an open set.
type TransactionType string

// The transaction types that Schwab's spec lists. Kinds of cash movement do
// not map one to one onto them: an ACH deposit has come as
// TransactionElectronicFund, monthly interest on cash as
// TransactionDividendOrInterest, and foreign tax withheld from a dividend as
// TransactionJournal.
const (
	TransactionTrade              TransactionType = "TRADE"
	TransactionReceiveAndDeliver  TransactionType = "RECEIVE_AND_DELIVER"
	TransactionDividendOrInterest TransactionType = "DIVIDEND_OR_INTEREST"
	TransactionACHReceipt         TransactionType = "ACH_RECEIPT"
	TransactionACHDisbursement    TransactionType = "ACH_DISBURSEMENT"
	TransactionCashReceipt        TransactionType = "CASH_RECEIPT"
	TransactionCashDisbursement   TransactionType = "CASH_DISBURSEMENT"
	TransactionElectronicFund     TransactionType = "ELECTRONIC_FUND"
	TransactionWireOut            TransactionType = "WIRE_OUT"
	TransactionWireIn             TransactionType = "WIRE_IN"
	TransactionJournal            TransactionType = "JOURNAL"
	TransactionMemorandum         TransactionType = "MEMORANDUM"
	TransactionMarginCall         TransactionType = "MARGIN_CALL"
	TransactionMoneyMarket        TransactionType = "MONEY_MARKET"
	TransactionSMAAdjustment      TransactionType = "SMA_ADJUSTMENT"
)

// AllTransactionTypes returns every transaction type that Schwab's spec
// lists, which a TransactionQuery asks for unless told otherwise.
func AllTransactionTypes() []TransactionType {
	return []TransactionType{
		TransactionTrade, TransactionReceiveAndDeliver, TransactionDividendOrInterest,
		TransactionACHReceipt, TransactionACHDisbursement, TransactionCashReceipt,
		TransactionCashDisbursement, TransactionElectronicFund, TransactionWireOut,
		TransactionWireIn, TransactionJournal, TransactionMemorandum,
		TransactionMarginCall, TransactionMoneyMarket, TransactionSMAAdjustment,
	}
}

// TransactionStatus is the state of a transaction.
type TransactionStatus string

// The statuses that Schwab's spec lists. Only TransactionStatusValid has
// been seen.
const (
	TransactionStatusValid   TransactionStatus = "VALID"
	TransactionStatusInvalid TransactionStatus = "INVALID"
	TransactionStatusPending TransactionStatus = "PENDING"
	TransactionStatusUnknown TransactionStatus = "UNKNOWN"
)

// Transaction is one activity in an account, from GET
// /accounts/{hash}/transactions or /accounts/{hash}/transactions/{id}.
//
// Which fields Schwab sends depends on the kind of row. An order execution
// (a TransactionTrade with an OrderID) has an OrderID and a PositionID but
// no Description and no SettlementDate. Every other row has a Description
// and a SettlementDate, and the cash rows (dividends, interest, journals,
// deposits) have no ids.
//
// NetAmount is the transaction's effect on the account's cash, positive in
// and negative out. On every trade seen it equals the sum of the legs'
// Cost, and a trade's fees are minus the Cost of its AssetCurrency legs,
// whether or not they name a FeeType. On other rows, the AssetCurrency leg
// carries the cash in its Amount, with a Cost of 0.
type Transaction struct {
	// ActivityID identifies the record; the fills of one order share its
	// OrderID but not their ActivityIDs.
	ActivityID int64 `json:"activityId"`
	// Time is the moment Schwab gives the activity: an execution's fill,
	// or when a dividend or deposit was posted.
	Time time.Time `json:"time"`
	// TradeDate is an order execution's fill, as Time is, or midnight in
	// New York on the day of any other row; some executions carry the
	// midnight too. TradeDay gives its date.
	TradeDate time.Time `json:"tradeDate"`
	// SettlementDate is midnight in New York on the settlement day, and
	// absent on order executions.
	SettlementDate time.Time         `json:"settlementDate,omitzero"`
	AccountNumber  string            `json:"accountNumber"`
	Type           TransactionType   `json:"type"`
	Status         TransactionStatus `json:"status"`
	// SubAccount is CASH or MARGIN, as seen.
	SubAccount string `json:"subAccount"`
	// Description says what the row is when Type does not, such as
	// "Dividend Reinvest", "Free Balance Interest Adjustment" or "W-8
	// Withholding~BND". A dividend's or withholding's security is named only
	// after the "~", since its only leg is cash.
	Description string  `json:"description,omitzero"`
	NetAmount   float64 `json:"netAmount"`
	OrderID     int64   `json:"orderId,omitzero"`
	PositionID  int64   `json:"positionId,omitzero"`
	// ActivityType is in Schwab's spec but has not been seen.
	ActivityType string `json:"activityType,omitzero"`
	// QualifiedDividend is on dividends since 2026; nil when absent.
	QualifiedDividend *bool          `json:"qualifiedDividend,omitempty"`
	TransferItems     []TransferItem `json:"transferItems"`

	// Problems and Raw are as the package doc describes. Problems include
	// those of the legs' instruments, under transferItems.instrument.
	Problems []Problem       `json:"problems,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes one transaction leniently, as the package doc
// describes. It never fails.
func (t *Transaction) UnmarshalJSON(b []byte) error {
	type plain Transaction
	// The times are read by hand, and the keys every transaction has are
	// read as raw JSON too, to tell an absent key from a zero value.
	var w struct {
		plain
		ActivityID     json.RawMessage `json:"activityId"`
		Time           json.RawMessage `json:"time"`
		TradeDate      json.RawMessage `json:"tradeDate"`
		SettlementDate json.RawMessage `json:"settlementDate"`
		NetAmount      json.RawMessage `json:"netAmount"`
	}
	isObject, err := decode(b, &w)
	*t = Transaction(w.plain)
	ps := recordProblems(b, t, isObject, err, "activityId", "time", "tradeDate", "settlementDate", "netAmount")
	hasID := readField(w.ActivityID, "activityId", &t.ActivityID, &ps)
	var hasTime bool
	t.Time, hasTime = readTime(w.Time, "time", &ps)
	t.TradeDate, _ = readTime(w.TradeDate, "tradeDate", &ps)
	t.SettlementDate, _ = readTime(w.SettlementDate, "settlementDate", &ps)
	hasNet := readField(w.NetAmount, "netAmount", &t.NetAmount, &ps)
	if isObject {
		need(&ps, hasID && t.ActivityID != 0, "activityId")
		need(&ps, hasTime, "time")
		need(&ps, t.Type != "", "type")
		need(&ps, t.Status != "", "status")
		need(&ps, hasNet, "netAmount")
		for i := range t.TransferItems {
			in := &t.TransferItems[i].Instrument
			ps = append(ps, within("transferItems.instrument", in.problems)...)
			in.problems = nil
		}
	}
	t.Problems, t.Raw = ps, copyRaw(b)
	return nil
}

// TradeDay returns the day the transaction was booked on, the New York date
// of its TradeDate, or of its Time if it has none. Use it to group
// transactions by day or month. To place a transaction between two
// snapshots, compare its Time with theirs instead: a row booked on a day can
// be posted that evening, after the day's snapshot.
func (t Transaction) TradeDay() Date {
	if !t.TradeDate.IsZero() {
		return DateInNewYork(t.TradeDate)
	}
	return DateInNewYork(t.Time)
}

// SettlementDay returns the New York date of the SettlementDate, or "" for
// an order execution, which has none: its settlement has to be worked out
// from the trade day on a calendar of business days.
func (t Transaction) SettlementDay() Date {
	return DateInNewYork(t.SettlementDate)
}

// TransferItem is one leg of a transaction: a security, a fee, or cash.
//
// On a security leg, Amount is the signed quantity in shares or contracts
// (positive for a buy), Cost the signed cash, and Price the price per share,
// which for an option is the premium, to be multiplied by
// Instrument.OptionPremiumMultiplier. On an AssetCurrency leg of a trade,
// Amount is the fee and Cost minus the fee; such a leg has no Price, and its
// FeeType names it when Schwab sends one. On an AssetCurrency leg of any
// other row, Amount is the cash, and Cost and Price are 0.
type TransferItem struct {
	Instrument Instrument `json:"instrument"`
	Amount     float64    `json:"amount"`
	Cost       float64    `json:"cost"`
	Price      float64    `json:"price,omitzero"`
	// FeeType has been seen as COMMISSION, SEC_FEE, OPT_REG_FEE, TAF_FEE and
	// FUTURES_EXCHANGE_FEE.
	FeeType string `json:"feeType,omitzero"`
	// PositionEffect is OPENING or CLOSING on a security leg.
	PositionEffect string `json:"positionEffect,omitzero"`
}

// MaxTransactions is the most transactions Schwab sends in one answer. It
// cuts a longer answer without saying so, keeping the newest, so an answer
// of this length may be missing older ones.
const MaxTransactions = 3000

// MaxTransactionWindow is the longest span that one request for
// transactions can cover.
const MaxTransactionWindow = 365 * 24 * time.Hour

// TransactionQuery asks for an account's transactions.
type TransactionQuery struct {
	// Start and End bound the transactions' times. Both are needed, and End
	// must come after Start, by at most MaxTransactionWindow. A window whose
	// start equals its end returns nothing.
	Start, End time.Time
	// Types are the kinds asked for, or every kind when empty.
	Types []TransactionType
	// Symbol asks for one security's transactions only. Dividends and
	// withholdings name their security only in their Description, so they
	// may be left out.
	Symbol string
}

// schwabTimeFormat is how Schwab takes instants in a query.
const schwabTimeFormat = "2006-01-02T15:04:05.000Z"

// values returns the query parameters of q, or an error for a window Schwab
// would refuse.
func (q TransactionQuery) values() (url.Values, error) {
	switch {
	case q.Start.IsZero() || q.End.IsZero():
		return nil, errors.New("a transaction query needs a start and an end")
	case !q.End.After(q.Start):
		return nil, fmt.Errorf("a transaction query ends at %s, which is not after its start, %s",
			q.End.UTC().Format(time.RFC3339), q.Start.UTC().Format(time.RFC3339))
	case q.End.Sub(q.Start) > MaxTransactionWindow:
		return nil, fmt.Errorf("a transaction query covers more than %d days, the most that Schwab serves in one request",
			MaxTransactionWindow/(24*time.Hour))
	}
	types := q.Types
	if len(types) == 0 {
		types = AllTransactionTypes()
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	v := url.Values{
		"startDate": {q.Start.UTC().Format(schwabTimeFormat)},
		"endDate":   {q.End.UTC().Format(schwabTimeFormat)},
		"types":     {strings.Join(names, ",")},
	}
	if q.Symbol != "" {
		v.Set("symbol", q.Symbol)
	}
	return v, nil
}

// Transactions returns the account's transactions that q asks for, in
// Schwab's order, which is not by time: sort by Time, then ActivityID.
// Schwab cuts an answer at MaxTransactions, so a window that returns that
// many should be split and asked for again. An empty window is not an
// error.
func (c *Client) Transactions(ctx context.Context, hash AccountHash, q TransactionQuery) ([]Transaction, error) {
	if hash == "" {
		return nil, errors.New("no account hash")
	}
	query, err := q.values()
	if err != nil {
		return nil, err
	}
	r, err := c.get(ctx, "/trader/v1/accounts/"+url.PathEscape(string(hash))+"/transactions", query)
	if err != nil {
		return nil, err
	}
	return list[Transaction](r, "the transactions")
}

// Transaction returns one of the account's transactions by its ActivityID.
func (c *Client) Transaction(ctx context.Context, hash AccountHash, activityID int64) (Transaction, error) {
	if hash == "" {
		return Transaction{}, errors.New("no account hash")
	}
	path := "/trader/v1/accounts/" + url.PathEscape(string(hash)) + "/transactions/" + strconv.FormatInt(activityID, 10)
	r, err := c.get(ctx, path, nil)
	if err != nil {
		return Transaction{}, err
	}
	return one[Transaction](r, "the transaction")
}
