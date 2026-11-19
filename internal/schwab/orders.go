package schwab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// OrderStatus is the state of an order. Schwab may send states beyond these
// constants, so treat it as an open set.
type OrderStatus string

// The order statuses that Schwab's spec lists. FILLED, CANCELED, REJECTED,
// REPLACED and PENDING_ACTIVATION have been seen, on orders read back from
// thinkorswim and the API.
const (
	OrderAwaitingParentOrder    OrderStatus = "AWAITING_PARENT_ORDER"
	OrderAwaitingCondition      OrderStatus = "AWAITING_CONDITION"
	OrderAwaitingStopCondition  OrderStatus = "AWAITING_STOP_CONDITION"
	OrderAwaitingManualReview   OrderStatus = "AWAITING_MANUAL_REVIEW"
	OrderAccepted               OrderStatus = "ACCEPTED"
	OrderAwaitingUROut          OrderStatus = "AWAITING_UR_OUT"
	OrderPendingActivation      OrderStatus = "PENDING_ACTIVATION"
	OrderQueued                 OrderStatus = "QUEUED"
	OrderWorking                OrderStatus = "WORKING"
	OrderRejected               OrderStatus = "REJECTED"
	OrderPendingCancel          OrderStatus = "PENDING_CANCEL"
	OrderCanceled               OrderStatus = "CANCELED"
	OrderPendingReplace         OrderStatus = "PENDING_REPLACE"
	OrderReplaced               OrderStatus = "REPLACED"
	OrderFilled                 OrderStatus = "FILLED"
	OrderExpired                OrderStatus = "EXPIRED"
	OrderNew                    OrderStatus = "NEW"
	OrderAwaitingReleaseTime    OrderStatus = "AWAITING_RELEASE_TIME"
	OrderPendingAcknowledgement OrderStatus = "PENDING_ACKNOWLEDGEMENT"
	OrderPendingRecall          OrderStatus = "PENDING_RECALL"
	OrderStatusUnknown          OrderStatus = "UNKNOWN"
)

// AllOrderStatuses returns every order status that Schwab's spec lists, in
// its order.
func AllOrderStatuses() []OrderStatus {
	return []OrderStatus{
		OrderAwaitingParentOrder, OrderAwaitingCondition, OrderAwaitingStopCondition, OrderAwaitingManualReview,
		OrderAccepted, OrderAwaitingUROut, OrderPendingActivation, OrderQueued, OrderWorking, OrderRejected,
		OrderPendingCancel, OrderCanceled, OrderPendingReplace, OrderReplaced, OrderFilled, OrderExpired, OrderNew,
		OrderAwaitingReleaseTime, OrderPendingAcknowledgement, OrderPendingRecall, OrderStatusUnknown,
	}
}

// Final reports whether an order in status s is done: filled, canceled,
// rejected, expired, or replaced by another order. Any other status, an
// unknown one included, may still fill or change.
func (s OrderStatus) Final() bool {
	switch s {
	case OrderFilled, OrderCanceled, OrderRejected, OrderExpired, OrderReplaced:
		return true
	}
	return false
}

// Order is an order, from GET /accounts/{hash}/orders, /accounts/{hash}/
// orders/{id} or /orders, or one of the orders that another holds.
//
// Every order seen, read back from thinkorswim or from the API, has been a
// SINGLE order whose keys are those of the first block below, each present
// as its comment says: CloseTime once the order is done, StatusDescription
// on a rejected order, Price on a limit order, Tag on most; the keys that
// Schwab's spec adds are kept, though none has been seen. An order that
// holds others, such as a TRIGGER whose child is an OCO of two orders (a
// bracket), has not been seen either, so the orders it holds are taken to
// carry what any order does: an order without OrderID, AccountNumber,
// Status, EnteredTime or OrderStrategyType, at any depth, has a problem.
//
// Figures are as Schwab sends them, unsigned: Instruction says the
// direction. Price is per share, and an option's is its premium per share;
// a NET_DEBIT or NET_CREDIT order's is the net of its legs.
type Order struct {
	// OrderID identifies the order. The executions of an order, the
	// transactions whose TransactionTrade rows carry it, share it.
	OrderID int64 `json:"orderId"`
	// AccountNumber is the account's number, which Schwab sends in orders
	// as a number, though as a string everywhere else.
	AccountNumber int64       `json:"accountNumber"`
	Status        OrderStatus `json:"status"`
	// StatusDescription is Schwab's sentence about a rejected order, such
	// as "You do not have enough available cash/buying power for this
	// order.", absent otherwise.
	StatusDescription string `json:"statusDescription,omitzero"`
	// EnteredTime is when the order was placed, and CloseTime when it was
	// filled, canceled, rejected, replaced or expired; an open order has no
	// CloseTime.
	EnteredTime time.Time `json:"enteredTime"`
	CloseTime   time.Time `json:"closeTime,omitzero"`
	// OrderStrategyType is SINGLE, or TRIGGER, OCO and the others that hold
	// orders in ChildOrderStrategies.
	OrderStrategyType string `json:"orderStrategyType"`
	// OrderType is MARKET, LIMIT, STOP, STOP_LIMIT, TRAILING_STOP,
	// NET_DEBIT, NET_CREDIT, NET_ZERO and the others the spec lists.
	OrderType string `json:"orderType,omitzero"`
	// ComplexOrderStrategyType is NONE, or how the legs make up a spread:
	// COVERED, VERTICAL, IRON_CONDOR and the others the spec lists.
	ComplexOrderStrategyType string `json:"complexOrderStrategyType,omitzero"`
	// Session is NORMAL, AM, PM or SEAMLESS, and Duration DAY,
	// GOOD_TILL_CANCEL, FILL_OR_KILL and the others the spec lists.
	Session  string `json:"session,omitzero"`
	Duration string `json:"duration,omitzero"`
	// Quantity is the order's shares or contracts, of which FilledQuantity
	// have filled and RemainingQuantity are left; nil when absent.
	Quantity          *float64 `json:"quantity,omitempty"`
	FilledQuantity    *float64 `json:"filledQuantity,omitempty"`
	RemainingQuantity *float64 `json:"remainingQuantity,omitempty"`
	// Price is the limit price, absent on a MARKET order.
	Price *float64 `json:"price,omitempty"`
	// RequestedDestination has been seen only as AUTO, and
	// DestinationLinkName as the venue's code, such as CDRG, or AutoRoute
	// before the order is routed.
	RequestedDestination string `json:"requestedDestination,omitzero"`
	DestinationLinkName  string `json:"destinationLinkName,omitzero"`
	// Cancelable and Editable say whether the order may still be canceled
	// or replaced; nil when absent.
	Cancelable *bool `json:"cancelable,omitempty"`
	Editable   *bool `json:"editable,omitempty"`
	// Tag names the platform or app that placed the order, such as
	// API_TOS:TRADE_ALL, or TA_ and an app's own text, which may name the
	// app's developer.
	Tag string `json:"tag,omitzero"`

	// The keys below are in Schwab's spec and have not been seen.
	StopPrice          *float64  `json:"stopPrice,omitempty"`
	StopPriceLinkBasis string    `json:"stopPriceLinkBasis,omitzero"`
	StopPriceLinkType  string    `json:"stopPriceLinkType,omitzero"`
	StopPriceOffset    *float64  `json:"stopPriceOffset,omitempty"`
	StopType           string    `json:"stopType,omitzero"`
	PriceLinkBasis     string    `json:"priceLinkBasis,omitzero"`
	PriceLinkType      string    `json:"priceLinkType,omitzero"`
	ActivationPrice    *float64  `json:"activationPrice,omitempty"`
	TaxLotMethod       string    `json:"taxLotMethod,omitzero"`
	SpecialInstruction string    `json:"specialInstruction,omitzero"`
	CancelTime         time.Time `json:"cancelTime,omitzero"`
	ReleaseTime        time.Time `json:"releaseTime,omitzero"`

	OrderLegCollection []OrderLeg `json:"orderLegCollection,omitzero"`
	// OrderActivityCollection holds the order's executions, once one has
	// happened: each fill, and a cancel.
	OrderActivityCollection []OrderActivity `json:"orderActivityCollection,omitzero"`
	// ChildOrderStrategies are the orders this one holds, such as the OCO
	// that a TRIGGER order places once it fills, and the two orders of the
	// OCO; ReplacingOrderCollection the orders that replaced this one. Each
	// is an Order with its own Problems and Raw, whose problems this
	// order's Problems hold too, under the key that holds it. A null among
	// them is a zero Order with a problem.
	ChildOrderStrategies     []Order `json:"childOrderStrategies,omitzero"`
	ReplacingOrderCollection []Order `json:"replacingOrderCollection,omitzero"`

	// Problems and Raw are as the package doc describes. Problems include
	// those of the legs' instruments, the executions' legs and the orders
	// this one holds, under their keys.
	Problems []Problem       `json:"problems,omitempty"`
	Raw      json.RawMessage `json:"-"`
}

// UnmarshalJSON decodes one order leniently, as the package doc describes.
// It never fails.
func (o *Order) UnmarshalJSON(b []byte) error {
	type plain Order
	var w struct {
		plain
		EnteredTime json.RawMessage `json:"enteredTime"`
		CloseTime   json.RawMessage `json:"closeTime"`
		CancelTime  json.RawMessage `json:"cancelTime"`
		ReleaseTime json.RawMessage `json:"releaseTime"`
		Problems    json.RawMessage `json:"problems"` // Schwab's, as decode says
	}
	isObject, err := decode(b, &w)
	*o = Order(w.plain)
	ps := recordProblems(b, o, isObject, err, "enteredTime", "closeTime", "cancelTime", "releaseTime", "problems")
	var hasEntered bool
	o.EnteredTime, hasEntered = readTime(w.EnteredTime, "enteredTime", &ps)
	o.CloseTime, _ = readTime(w.CloseTime, "closeTime", &ps)
	o.CancelTime, _ = readTime(w.CancelTime, "cancelTime", &ps)
	o.ReleaseTime, _ = readTime(w.ReleaseTime, "releaseTime", &ps)
	if isObject {
		need(&ps, o.OrderID != 0, "orderId")
		need(&ps, o.AccountNumber != 0, "accountNumber")
		need(&ps, o.Status != "", "status")
		need(&ps, hasEntered, "enteredTime")
		need(&ps, o.OrderStrategyType != "", "orderStrategyType")
		for i := range o.OrderLegCollection {
			in := &o.OrderLegCollection[i].Instrument
			ps = within(ps, "orderLegCollection.instrument", in.problems)
			in.problems = nil
		}
		for i := range o.OrderActivityCollection {
			for j := range o.OrderActivityCollection[i].ExecutionLegs {
				leg := &o.OrderActivityCollection[i].ExecutionLegs[j]
				ps = within(ps, "orderActivityCollection.executionLegs", leg.problems)
				leg.problems = nil
			}
		}
		for _, h := range o.ChildOrderStrategies {
			ps = within(ps, "childOrderStrategies", h.Problems)
		}
		for _, h := range o.ReplacingOrderCollection {
			ps = within(ps, "replacingOrderCollection", h.Problems)
		}
	}
	o.Problems, o.Raw = ps, copyRaw(b)
	return nil
}

// OrderLeg is one leg of an order: what it buys or sells, and how much.
type OrderLeg struct {
	// OrderLegType is the leg's asset type as the order has it, EQUITY for
	// an exchange-traded fund too, though its Instrument says
	// COLLECTIVE_INVESTMENT.
	OrderLegType string     `json:"orderLegType"`
	LegID        int64      `json:"legId"`
	Instrument   Instrument `json:"instrument"`
	// Instruction is BUY, SELL, BUY_TO_COVER, SELL_SHORT, BUY_TO_OPEN,
	// BUY_TO_CLOSE, SELL_TO_OPEN, SELL_TO_CLOSE and the others the spec
	// lists.
	Instruction string `json:"instruction"`
	// PositionEffect is OPENING or CLOSING, or AUTOMATIC in the spec.
	PositionEffect string  `json:"positionEffect,omitzero"`
	Quantity       float64 `json:"quantity"`
	// QuantityType, DivCapGains and ToSymbol are in the spec, for mutual
	// funds, and have not been seen.
	QuantityType string `json:"quantityType,omitzero"`
	DivCapGains  string `json:"divCapGains,omitzero"`
	ToSymbol     string `json:"toSymbol,omitzero"`
}

// OrderActivity is something that happened to an order: an execution,
// which is a fill, or a cancel that Schwab reports as one.
type OrderActivity struct {
	// ActivityType has been seen only as EXECUTION; the spec adds
	// ORDER_ACTION.
	ActivityType string `json:"activityType"`
	// ActivityID is not in the spec. Whether it is the ActivityID of the
	// fill's transaction is not known.
	ActivityID int64 `json:"activityId,omitzero"`
	// ExecutionType is FILL for a fill, or CANCELED for a cancel, whose
	// legs have a time and a Price of 0. Only a FILL's legs are fills.
	ExecutionType string `json:"executionType,omitzero"`
	// Quantity is what the activity filled, or canceled, and
	// OrderRemainingQuantity what was left of the order after it; nil
	// when absent.
	Quantity               float64        `json:"quantity,omitzero"`
	OrderRemainingQuantity *float64       `json:"orderRemainingQuantity,omitempty"`
	ExecutionLegs          []ExecutionLeg `json:"executionLegs,omitzero"`
}

// ExecutionLeg is what an execution did to one leg of its order: the
// shares or contracts at a price, at a time.
type ExecutionLeg struct {
	// LegID is the LegID of the order's leg.
	LegID int64 `json:"legId"`
	// Price is per share, an option's premium per share; 0 on a cancel.
	Price             float64   `json:"price"`
	Quantity          float64   `json:"quantity"`
	MismarkedQuantity float64   `json:"mismarkedQuantity"`
	InstrumentID      int64     `json:"instrumentId"`
	Time              time.Time `json:"time"`

	// problems are what UnmarshalJSON could not read, for the order that
	// holds the leg to report.
	problems []Problem
}

// UnmarshalJSON decodes an execution's leg leniently. It never fails; what
// it cannot read goes to the problems of the order that holds it. Null
// leaves the leg zero, with a problem, since a leg is an element of a list,
// which no key of the order's reports missing.
func (l *ExecutionLeg) UnmarshalJSON(b []byte) error {
	if isNull(b) {
		*l = ExecutionLeg{problems: []Problem{{What: sent(trimSpace(b), "an object")}}}
		return nil
	}
	type plain ExecutionLeg
	var w struct {
		plain
		Time json.RawMessage `json:"time"`
	}
	isObject, err := decode(b, &w)
	*l = ExecutionLeg(w.plain)
	ps := recordProblems(b, l, isObject, err, "time")
	var hasTime bool
	l.Time, hasTime = readTime(w.Time, "time", &ps)
	if isObject {
		need(&ps, hasTime, "time")
	}
	l.problems = ps
	return nil
}

// Fills returns the order's fills: the legs of its activities whose
// ExecutionType is FILL, in the order Schwab sent them.
func (o Order) Fills() []ExecutionLeg {
	var fills []ExecutionLeg
	for _, a := range o.OrderActivityCollection {
		if a.ExecutionType == "FILL" {
			fills = append(fills, a.ExecutionLegs...)
		}
	}
	return fills
}

// MaxOrders is the most orders that Schwab sends in one answer, its
// maxResults when a request sends none.
const MaxOrders = 3000

// MaxOrderWindow is the longest span of entry times that one request for
// orders can cover, as Schwab's spec gives it for an account's orders. It
// is reported that Schwab serves an account's orders for 60 days only
// (docs/live.md, probe Q5), and the spec says that the orders of every
// account (AllOrders) are served from 60 days ago at most.
const MaxOrderWindow = 365 * 24 * time.Hour

// OrderQuery asks for orders.
type OrderQuery struct {
	// Start and End bound the orders' entry times. Both are needed, and End
	// must come after Start, by at most MaxOrderWindow; Orders and
	// AllOrders refuse any other window without sending it.
	Start, End time.Time
	// Status asks for the orders in one status only, or every status when
	// "".
	Status OrderStatus
	// MaxResults is the most orders to send, from 1 to MaxOrders, or 0 for
	// MaxOrders.
	MaxResults int
}

// values returns the query parameters of q, or an error for a query that
// Schwab would refuse.
func (q OrderQuery) values() (url.Values, error) {
	switch {
	case q.Start.IsZero() || q.End.IsZero():
		return nil, errors.New("an order query needs a start and an end")
	case !q.End.After(q.Start):
		return nil, fmt.Errorf("an order query ends at %s, which is not after its start, %s",
			q.End.UTC().Format(time.RFC3339), q.Start.UTC().Format(time.RFC3339))
	case q.End.Sub(q.Start) > MaxOrderWindow:
		return nil, fmt.Errorf("an order query covers more than %d days, the most that Schwab serves in one request",
			MaxOrderWindow/(24*time.Hour))
	case q.MaxResults < 0 || q.MaxResults > MaxOrders:
		return nil, fmt.Errorf("an order query asks for %d orders, outside 1 to %d", q.MaxResults, MaxOrders)
	}
	v := url.Values{
		"fromEnteredTime": {q.Start.UTC().Format(schwabTimeFormat)},
		"toEnteredTime":   {q.End.UTC().Format(schwabTimeFormat)},
	}
	if q.Status != "" {
		v.Set("status", string(q.Status))
	}
	if q.MaxResults > 0 {
		v.Set("maxResults", strconv.Itoa(q.MaxResults))
	}
	return v, nil
}

// Orders returns the account's orders that q asks for, in Schwab's order,
// which has been seen to be by entry time, newest first, but not always:
// sort by EnteredTime, then OrderID. Schwab cuts an answer at q.MaxResults,
// or MaxOrders, so a window that returns that many should be split and
// asked for again. A window without orders gives an empty list, not an
// error.
func (c *Client) Orders(ctx context.Context, hash AccountHash, q OrderQuery) ([]Order, error) {
	if hash == "" {
		return nil, errors.New("no account hash")
	}
	query, err := q.values()
	if err != nil {
		return nil, err
	}
	r, err := c.get(ctx, "/trader/v1/accounts/"+url.PathEscape(string(hash))+"/orders", query)
	if err != nil {
		return nil, err
	}
	return list[Order](r, "the orders")
}

// AllOrders returns the orders that q asks for of every account shared at
// login, as Orders returns one account's. Each order's AccountNumber says
// whose it is.
func (c *Client) AllOrders(ctx context.Context, q OrderQuery) ([]Order, error) {
	query, err := q.values()
	if err != nil {
		return nil, err
	}
	r, err := c.get(ctx, "/trader/v1/orders", query)
	if err != nil {
		return nil, err
	}
	return list[Order](r, "the orders")
}

// Order returns one of the account's orders by its OrderID.
func (c *Client) Order(ctx context.Context, hash AccountHash, orderID int64) (Order, error) {
	if hash == "" {
		return Order{}, errors.New("no account hash")
	}
	path := "/trader/v1/accounts/" + url.PathEscape(string(hash)) + "/orders/" + strconv.FormatInt(orderID, 10)
	r, err := c.get(ctx, path, nil)
	if err != nil {
		return Order{}, err
	}
	return one[Order](r, "the order")
}
