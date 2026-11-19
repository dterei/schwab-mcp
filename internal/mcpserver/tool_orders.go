package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// get_orders and get_order read Schwab's orders, which the local history
// does not keep: one request for each account's orders entered in the
// asked days, or for one order by its ID. An order holds the orders that
// it places once it fills, as a bracket's TRIGGER holds an OCO of two
// orders, and the orders that replaced it; an answer lists those under the
// order, flat, each with the ID of the order that holds it, so that no
// output type holds itself. A tag, which may name an app's developer,
// shows only where the order came from (mask.TagOrigin).

// registerOrders registers get_orders and get_order, and the reference
// pages of order types and order statuses.
func (s *Server) registerOrders() {
	addTool(s, getOrdersDef, getOrders)
	addTool(s, getOrderDef, getOrder)
	s.addReferencePage(orderTypesPage())
	s.addReferencePage(orderStatusesPage())
}

const (
	// orderDefaultDays is how many days before to the asked days start when
	// from is left out: 60 days in all, the span that Schwab is reported to
	// serve at least.
	orderDefaultDays = 59
	// orderMaxDays is the most days that one call asks for, 52 weeks: as
	// New York days, they span at most an hour more, past a change of the
	// clocks, which stays within the year that Schwab's spec gives one
	// request (schwab.MaxOrderWindow), as 365 days would not.
	orderMaxDays = 364
)

// getOrdersDef is get_orders' definition.
var getOrdersDef = toolDef{
	name:  "get_orders",
	title: "Schwab orders",
	description: "Orders entered between two New York dates (default the last 60 days), newest first: status and whether " +
		"it is open (may still fill or change), type, prices, quantities ordered, filled and remaining, and each leg's " +
		"instruction, symbol, quantity and average fill price. held lists the orders an order holds, such as a " +
		"bracket's, with the ID of the order that holds each. Filters: account, statuses, open, symbol. detail full adds " +
		"fills, venue and origin. Paged: pass next_cursor back as cursor.",
	slot: true,
	args: map[string]argRule{
		"from": {desc: "The first New York date of entry, YYYY-MM-DD; 59 days before to when left out. At most 364 days " +
			"(52 weeks) from to; Schwab may serve fewer."},
		"to": {desc: "The last New York date of entry, YYYY-MM-DD, included; today when left out, or when later than today."},
		"statuses": {
			desc: "Only orders in these of Schwab's statuses, such as WORKING or FILLED (schwab://reference/order-statuses " +
				"lists them); every status when left out.",
			maxItems: new(21), itemPattern: codePattern,
		},
		"open": {desc: "Only open orders, which may still fill or change: every status but FILLED, CANCELED, REJECTED, " +
			"EXPIRED and REPLACED."},
		"symbol": {desc: "One symbol, in any case: the orders with a leg of it, or of an option on it."},
		"detail": {desc: "compact (the default) or full, which adds each order's fills, venue and origin, and each leg's " +
			"instrument."},
		"limit": {
			desc: "The most orders of a page, 1 to 200; 50 when left out. A page also stops at about 20,000 bytes.",
			min:  new(1.0), max: new(200.0),
		},
	},
}

// getOrderDef is get_order's definition.
var getOrderDef = toolDef{
	name:  "get_order",
	title: "One Schwab order",
	description: "One order in full, by Schwab's order ID (order_id from get_orders, or a transaction's order_id): " +
		"its status, prices and quantities, each leg, each fill, the orders it holds, and with include_raw Schwab's JSON " +
		"with account numbers masked. account is needed only when the login shares several accounts.",
	slot: true,
	args: map[string]argRule{
		"id": {desc: "Schwab's order ID, such as 1004517688020.", pattern: idPattern},
		"account": {desc: "The account, as get_orders takes it, such as an order's account_hash; needed only when the login " +
			"shares several accounts."},
		"include_raw": {desc: "Also return Schwab's JSON of the order, with account numbers masked, the tag shown only as " +
			"its origin, and a value of a key the server does not know withheld unless it is a time, a code, a number " +
			"that cannot be an account number, or a number already masked."},
	},
}

// getOrdersIn is get_orders' arguments.
type getOrdersIn struct {
	Account  string   `json:"account,omitempty"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to,omitempty"`
	Statuses []string `json:"statuses,omitempty"`
	Open     bool     `json:"open,omitempty"`
	Symbol   string   `json:"symbol,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Limit    int      `json:"limit,omitempty"`
	Cursor   string   `json:"cursor,omitempty"`
}

// getOrdersOut is get_orders' answer.
type getOrdersOut struct {
	envelope
	page
	cuts                    // detail full: an order's fills, and the orders it holds, cut to fit the cap
	Orders   []orderRow     `json:"orders"`
	Accounts []orderAccount `json:"accounts"` // every selected account, on every page
}

// dropRaw holds no raw JSON: get_orders sends none.
func (o *getOrdersOut) dropRaw() bool { return false }

// lists returns each order's fills, which detail full adds, and the orders
// it holds.
func (o *getOrdersOut) lists() []cuttable {
	var ls []cuttable
	for i := range o.Orders {
		ls = append(ls, o.Orders[i].cuttables(fmt.Sprintf("orders.%d.", i))...)
	}
	return ls
}

// cutHint says how to see an order's lists that were cut.
func (o *getOrdersOut) cutHint() string {
	return "get_order with an order's order_id shows its fills and the orders it holds, as many as fit."
}

// An orderAccount is one selected account of get_orders, on every page.
type orderAccount struct {
	accountRef
	Orders *int   `json:"orders"` // its orders that the query matches, on every page; null when it could not be read
	Error  *Ident `json:"error"`  // the code, when the account could not be read
}

// orderFields are what an order of an answer shows, whether Schwab answered
// with it or another order holds it.
type orderFields struct {
	OrderID           Ident     `json:"order_id"`
	Status            *Ident    `json:"status"`
	Open              bool      `json:"open"` // whether it may still fill or change: its status is none of the final ones
	StatusDescription *FreeText `json:"status_description"`
	Entered           *Instant  `json:"entered"`
	Closed            *Instant  `json:"closed"`           // when it filled, was canceled, rejected, replaced or expired
	Strategy          *Ident    `json:"strategy"`         // Schwab's orderStrategyType: SINGLE, TRIGGER, OCO
	OrderType         *Ident    `json:"order_type"`       // MARKET, LIMIT, STOP, NET_DEBIT, ...
	ComplexStrategy   *Ident    `json:"complex_strategy"` // VERTICAL, IRON_CONDOR, ...; null for NONE
	Session           *Ident    `json:"session"`
	Duration          *Ident    `json:"duration"`
	// Price is the limit price, per share, the net of the legs for a
	// NET_DEBIT or NET_CREDIT order; StopPrice the stop.
	Price     lookup.Price `json:"price"`
	StopPrice lookup.Price `json:"stop_price"`
	Quantity  lookup.Qty   `json:"quantity"`
	Filled    lookup.Qty   `json:"filled"`
	Remaining lookup.Qty   `json:"remaining"`
	Legs      []orderLeg   `json:"legs"`
}

// An orderRow is one order that Schwab answered with.
type orderRow struct {
	Account     MaskedNumber `json:"account"`
	AccountHash *Ident       `json:"account_hash"`
	orderFields
	// Held are the orders that it holds, at any depth, and those that
	// replaced it, depth first: [] for none.
	Held   []heldOrder  `json:"held"`
	Detail *orderDetail `json:"detail,omitempty"` // detail full, and always in get_order
}

// cuttables returns the lists of r that an answer may cut, at prefix.
func (r *orderRow) cuttables(prefix string) []cuttable {
	ls := []cuttable{{
		path: prefix + "held", value: r.Held,
		count: func() int { return len(r.Held) }, keep: func(n int) { r.Held = r.Held[:n] },
	}}
	if d := r.Detail; d != nil {
		ls = append(ls, cuttable{
			path: prefix + "detail.fills", value: d.Fills,
			count: func() int { return len(d.Fills) }, keep: func(n int) { d.Fills = d.Fills[:n] },
		})
	}
	return ls
}

// A heldOrder is an order that another holds, or one that replaced it.
type heldOrder struct {
	ParentID Ident `json:"parent_id"` // the order that holds it, or that it replaced
	Relation Ident `json:"relation"`  // "child" for an order held, "replacement" for one that replaced it
	orderFields
}

// An orderLeg is one leg of an order.
type orderLeg struct {
	Instruction    *Ident     `json:"instruction"` // BUY, SELL, BUY_TO_OPEN, SELL_TO_CLOSE, ...
	Symbol         *Ident     `json:"symbol"`
	AssetType      *Ident     `json:"asset_type"`
	Quantity       lookup.Qty `json:"quantity"`
	PositionEffect *Ident     `json:"position_effect"` // OPENING or CLOSING
	// FillPrice is the average price of the leg's fills, per share; null
	// before any.
	FillPrice lookup.Price    `json:"fill_price"`
	Detail    *orderLegDetail `json:"detail,omitempty"` // detail full
}

// orderLegDetail is what detail full adds to a leg: its instrument.
type orderLegDetail struct {
	Description *FreeText    `json:"description"`
	CUSIP       *Ident       `json:"cusip"`
	Underlying  *Ident       `json:"underlying"`
	PutCall     *Ident       `json:"put_call"`
	Strike      lookup.Price `json:"strike"`     // an option's, from its OCC symbol
	Expiration  *Date        `json:"expiration"` // an option's, from its OCC symbol
}

// orderDetail is what detail full adds to an order.
type orderDetail struct {
	// Origin is where the order came from, as its tag shows it: Schwab's own
	// platform's (API_TOS:TRADE_ALL), an app's (TA_...), or [withheld].
	Origin     *Ident      `json:"origin"`
	Venue      *Ident      `json:"venue"` // where it was routed, such as CDRG, or AutoRoute before then
	Cancelable *bool       `json:"cancelable"`
	Editable   *bool       `json:"editable"`
	Fills      []orderFill `json:"fills"`
	Problems   []problem   `json:"problems"`
}

// An orderFill is one fill of one leg.
type orderFill struct {
	LegID    int64        `json:"leg_id"`
	Symbol   *Ident       `json:"symbol"` // the leg's
	Time     *Instant     `json:"time"`
	Quantity lookup.Qty   `json:"quantity"`
	Price    lookup.Price `json:"price"` // per share
}

// An orderQuery is get_orders' arguments, checked, with the defaults
// applied.
type orderQuery struct {
	from, to nytime.Date
	statuses []string // upper case, each once, in order
	open     bool
	symbol   string // upper case, "" for any
	full     bool
	limit    int
}

// orderQueryOf checks in, get_orders' arguments beyond the account and the
// cursor, and applies their defaults. Its errors name the argument and
// never quote its value.
func (c *call) orderQueryOf(in getOrdersIn) (orderQuery, error) {
	today := nytime.DateOf(c.now)
	q := orderQuery{
		to: today, open: in.Open, symbol: asciiUpper(strings.TrimSpace(in.Symbol)), full: in.Detail == "full",
		limit: cmp.Or(in.Limit, 50),
	}
	if in.To != "" {
		if _, ok := dayOf(nytime.Date(in.To)); !ok {
			return q, c.failf(codeBadArgument, "to takes a New York date, YYYY-MM-DD.")
		}
		q.to = min(nytime.Date(in.To), today)
	}
	q.from = addDays(q.to, -orderDefaultDays)
	if in.From != "" {
		if _, ok := dayOf(nytime.Date(in.From)); !ok {
			return q, c.failf(codeBadArgument, "from takes a New York date, YYYY-MM-DD.")
		}
		q.from = nytime.Date(in.From)
	}
	switch {
	case q.from > q.to:
		return q, c.failf(codeBadArgument, "from comes after to, or after today when to is later or left out; "+
			"from takes the first day asked, to the last.")
	case daysFrom(q.from, q.to)+1 > orderMaxDays:
		return q, c.failf(codeBadArgument, "from: one call asks for at most %d days of orders; ask for a shorter range.", orderMaxDays)
	}
	for _, s := range in.Statuses {
		q.statuses = append(q.statuses, asciiUpper(s))
	}
	q.statuses = slices.Compact(slices.Sorted(slices.Values(q.statuses)))
	return q, nil
}

// orderArgs are get_orders' canonical arguments, whose digest a cursor
// holds (cursor.go): the defaults applied, the statuses sorted, the symbol
// in upper case, and the account as its key, never its number.
type orderArgs struct {
	Account  string   `json:"account,omitempty"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Statuses []string `json:"statuses,omitempty"`
	Open     bool     `json:"open,omitempty"`
	Symbol   string   `json:"symbol,omitempty"`
	Detail   string   `json:"detail"`
}

// orderKey is where a page of get_orders ended: the last order sent, by
// the answer's order, newest entry first, then the largest ID, then the
// account's key.
type orderKey struct {
	Entered int64  `json:"e"` // Unix milliseconds
	ID      int64  `json:"i"`
	Account string `json:"a"`
}

// compareOrderKeys orders x and y as get_orders' answers do.
func compareOrderKeys(x, y orderKey) int {
	return cmp.Or(cmp.Compare(y.Entered, x.Entered), cmp.Compare(y.ID, x.ID), strings.Compare(x.Account, y.Account))
}

// An orderItem is an order that a query matched, of an account.
type orderItem struct {
	key   orderKey
	a     dirAccount
	order schwab.Order
}

// getOrders answers get_orders: each selected account's orders entered in
// the asked days, one request each, filtered, merged newest first, and a
// page of them.
func getOrders(ctx context.Context, c *call, in getOrdersIn) (getOrdersOut, error) {
	q, err := c.orderQueryOf(in)
	if err != nil {
		return getOrdersOut{}, err
	}
	picked, err := c.pickAccounts(ctx, in.Account)
	if err != nil {
		return getOrdersOut{}, err
	}
	one := strings.TrimSpace(in.Account) != ""
	detail := "compact"
	if q.full {
		detail = "full"
	}
	args := orderArgs{From: string(q.from), To: string(q.to), Statuses: q.statuses, Open: q.open, Symbol: q.symbol, Detail: detail}
	if one {
		args.Account = picked[0].key()
	}
	var after *orderKey
	if in.Cursor != "" {
		after = new(orderKey)
		if err := c.readCursor(in.Cursor, args, after); err != nil {
			return getOrdersOut{}, err
		}
	}

	out := getOrdersOut{
		envelope: envelope{Source: sourceSchwab}, page: page{Complete: true},
		Orders: []orderRow{}, Accounts: []orderAccount{},
	}
	start, end := midnight(q.from), midnight(addDays(q.to, 1))
	if end.After(c.now) {
		end = c.now
	}
	var items []orderItem
	var problems [][]schwab.Problem
	var failed []*toolErr
	for i := range picked {
		a := &picked[i]
		var orders []schwab.Order
		var rerr error
		switch {
		case !end.After(start):
		case !c.roomForMore():
			rerr = errNoTime
		default:
			orders, rerr = askAccount(ctx, c, a, func(cl *schwab.Client, hash schwab.AccountHash) ([]schwab.Order, error) {
				return cl.Orders(ctx, hash, schwab.OrderQuery{Start: start, End: end})
			})
		}
		// The account as the read left it, with the new hash that the
		// directory read when Schwab refused the one it had.
		row := orderAccount{accountRef: c.ref(*a)}
		if rerr != nil {
			if ctx.Err() != nil {
				return getOrdersOut{}, rerr
			}
			// An account left unread for want of time fails the call: a
			// cursor goes on past where the account's newer orders would
			// lie, so a later page could not show them.
			if errors.Is(rerr, errNoTime) {
				return getOrdersOut{}, c.s.errorFor(codeTimedOut)
			}
			e := c.s.toolError(c.tool, rerr)
			if one {
				return getOrdersOut{}, e
			}
			row.Error = new(Ident(e.code))
			c.warn("account_failed", "%s could not be read: %s", c.accountLabel(row.accountRef), c.errText(e))
			failed = append(failed, e)
			out.Accounts = append(out.Accounts, row)
			continue
		}
		if len(orders) >= schwab.MaxOrders {
			out.Complete = false
			c.warn("partial", "Schwab sent its most orders, %d, for %s, so older orders of the asked days may be missing; "+
				"ask for fewer days.", schwab.MaxOrders, c.accountLabel(row.accountRef))
		}
		n := 0
		for _, o := range orders {
			if !q.matches(o) {
				continue
			}
			if len(o.Problems) > 0 {
				problems = append(problems, o.Problems)
			}
			n++
			items = append(items, orderItem{key: orderKey{Entered: o.EnteredTime.UnixMilli(), ID: o.OrderID, Account: a.key()}, a: *a, order: o})
		}
		row.Orders = &n
		out.Accounts = append(out.Accounts, row)
	}
	if len(failed) == len(picked) && len(failed) > 0 {
		return getOrdersOut{}, failed[0]
	}
	c.warnProblems(plural(len(problems), "order"), problems, "a figure that could not be read is null")

	slices.SortFunc(items, func(x, y orderItem) int { return compareOrderKeys(x.key, y.key) })
	from := 0
	if after != nil {
		from = len(items)
		for i, it := range items {
			if compareOrderKeys(it.key, *after) > 0 {
				from = i
				break
			}
		}
	}
	rows := make([]orderRow, len(items)-from)
	sizes := make([]int, len(rows))
	for i, it := range items[from:] {
		rows[i] = c.orderRowOf(it.a, it.order, q.full)
		b, err := encode(rows[i])
		if err != nil {
			return getOrdersOut{}, err
		}
		sizes[i] = len(b)
	}
	out.AsOf, out.Warnings = instantOf(c.now), slices.Clone(c.warnings)
	fixed, err := encode(out)
	if err != nil {
		return getOrdersOut{}, err
	}
	n := c.pageRows(len(fixed), sizes, q.limit)
	out.Orders = append(out.Orders, rows[:n]...)
	if n < len(rows) {
		if out.NextCursor, err = c.newCursor(args, items[from+n-1].key); err != nil {
			return getOrdersOut{}, err
		}
	}
	return out, nil
}

// plural returns n and noun, with an s when n is not 1.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// matches reports whether o is one that q asks for: of one of its
// statuses, open when it asks for open orders, and with a leg of its
// symbol, or of an option on it, in o or an order that o holds.
func (q orderQuery) matches(o schwab.Order) bool {
	if len(q.statuses) > 0 && !slices.Contains(q.statuses, string(o.Status)) {
		return false
	}
	if q.open && o.Status.Final() {
		return false
	}
	return q.symbol == "" || hasSymbol(o, q.symbol)
}

// hasSymbol reports whether a leg of o, or of an order that o holds, at
// any depth, is of symbol, an upper-case symbol, or of an option on it.
func hasSymbol(o schwab.Order, symbol string) bool {
	for _, l := range o.OrderLegCollection {
		if strings.EqualFold(l.Instrument.Symbol, symbol) || strings.EqualFold(l.Instrument.UnderlyingSymbol, symbol) {
			return true
		}
	}
	for _, held := range slices.Concat(o.ChildOrderStrategies, o.ReplacingOrderCollection) {
		if hasSymbol(held, symbol) {
			return true
		}
	}
	return false
}

// orderRowOf returns o, an order of the account a, as a row of an answer;
// full adds its detail.
func (c *call) orderRowOf(a dirAccount, o schwab.Order, full bool) orderRow {
	r := orderRow{
		Account: c.mask.masked(a.number), AccountHash: identPtr(string(a.hash)),
		orderFields: c.orderFieldsOf(o, full), Held: []heldOrder{},
	}
	var hold func(parent schwab.Order)
	hold = func(parent schwab.Order) {
		id := Ident(strconv.FormatInt(parent.OrderID, 10))
		for _, h := range parent.ChildOrderStrategies {
			r.Held = append(r.Held, heldOrder{ParentID: id, Relation: "child", orderFields: c.orderFieldsOf(h, full)})
			hold(h)
		}
		for _, h := range parent.ReplacingOrderCollection {
			r.Held = append(r.Held, heldOrder{ParentID: id, Relation: "replacement", orderFields: c.orderFieldsOf(h, full)})
			hold(h)
		}
	}
	hold(o)
	if !full {
		return r
	}
	d := &orderDetail{
		Origin: identPtr(mask.TagOrigin(o.Tag)), Venue: identPtr(o.DestinationLinkName),
		Cancelable: o.Cancelable, Editable: o.Editable, Fills: []orderFill{}, Problems: []problem{},
	}
	symbols := map[int64]string{}
	for _, l := range o.OrderLegCollection {
		symbols[l.LegID] = l.Instrument.Symbol
	}
	priceBad := lookup.Unreadable(o.Problems, "orderActivityCollection.executionLegs.price")
	qtyBad := lookup.Unreadable(o.Problems, "orderActivityCollection.executionLegs.quantity")
	for _, f := range o.Fills() {
		fill := orderFill{LegID: f.LegID, Symbol: identPtr(symbols[f.LegID]), Time: instantPtr(f.Time)}
		if !priceBad {
			fill.Price = lookup.PriceOf(f.Price)
		}
		if !qtyBad {
			fill.Quantity = lookup.QtyOf(f.Quantity)
		}
		d.Fills = append(d.Fills, fill)
	}
	for _, p := range o.Problems {
		d.Problems = append(d.Problems, problem{Path: Ident(p.Path), What: c.mask.free(p.What)})
	}
	r.Detail = d
	return r
}

// orderFieldsOf returns what an answer shows of o; full adds each leg's
// instrument.
func (c *call) orderFieldsOf(o schwab.Order, full bool) orderFields {
	f := orderFields{
		OrderID: Ident(strconv.FormatInt(o.OrderID, 10)), Status: identPtr(string(o.Status)), Open: !o.Status.Final(),
		StatusDescription: c.freeText(o.StatusDescription), Entered: instantPtr(o.EnteredTime), Closed: instantPtr(o.CloseTime),
		Strategy: identPtr(o.OrderStrategyType), OrderType: identPtr(o.OrderType), Session: identPtr(o.Session),
		Duration: identPtr(o.Duration), Price: priceAt(o.Price), StopPrice: priceAt(o.StopPrice),
		Quantity: qtyAt(o.Quantity), Filled: qtyAt(o.FilledQuantity), Remaining: qtyAt(o.RemainingQuantity),
		Legs: []orderLeg{},
	}
	if o.ComplexOrderStrategyType != "NONE" {
		f.ComplexStrategy = identPtr(o.ComplexOrderStrategyType)
	}
	// A figure that could not be read, in any leg or any fill, is null in
	// every one, since a problem's path names no element of a list; so is
	// every average fill price when a fill's leg, price or quantity could
	// not be read.
	fills := o.Fills()
	qtyBad := lookup.Unreadable(o.Problems, "orderLegCollection.quantity")
	fillsBad := false
	for _, path := range []string{
		"orderLegCollection.legId", "orderActivityCollection.executionType",
		"orderActivityCollection.executionLegs.legId", "orderActivityCollection.executionLegs.price",
		"orderActivityCollection.executionLegs.quantity",
	} {
		fillsBad = fillsBad || lookup.Unreadable(o.Problems, path)
	}
	for _, l := range o.OrderLegCollection {
		in := l.Instrument
		leg := orderLeg{
			Instruction: identPtr(l.Instruction), Symbol: identPtr(in.Symbol), AssetType: identPtr(string(in.AssetType)),
			PositionEffect: identPtr(l.PositionEffect),
		}
		if !qtyBad {
			leg.Quantity = lookup.QtyOf(l.Quantity)
		}
		if !fillsBad {
			leg.FillPrice = averagePrice(fills, l.LegID)
		}
		if full {
			ld := &orderLegDetail{
				Description: c.freeText(in.Description), CUSIP: identPtr(in.CUSIP), Underlying: identPtr(in.UnderlyingSymbol),
				PutCall: identPtr(in.PutCall),
			}
			if occ, ok := schwab.ParseOCC(in.Symbol); ok && in.AssetType == schwab.AssetOption {
				ld.Strike, ld.Expiration = lookup.PriceOf(float64(occ.StrikeThousandths)/1000), datePtr(occ.Expiration)
			}
			leg.Detail = ld
		}
		f.Legs = append(f.Legs, leg)
	}
	return f
}

// qtyAt returns *v as a quantity, or none for nil.
func qtyAt(v *float64) lookup.Qty {
	if v == nil {
		return lookup.Qty{}
	}
	return lookup.QtyOf(*v)
}

// averagePrice returns the average price of the fills of the leg legID,
// weighted by their quantities, or none when it has none.
func averagePrice(fills []schwab.ExecutionLeg, legID int64) lookup.Price {
	var qty, cost float64
	for _, f := range fills {
		if f.LegID == legID && f.Quantity > 0 {
			qty += f.Quantity
			cost += f.Quantity * f.Price
		}
	}
	if qty == 0 {
		return lookup.Price{}
	}
	return lookup.PriceOf(cost / qty)
}

// orderIDShown returns given, an order ID as the model gave it, of the
// value id, as an answer may echo it, as txnID.shown echoes an activity
// ID: whole when it is written plainly, with 10 digits or more, as
// Schwab's order IDs are, and holds no account number the server knows;
// else only its last digits, as an account selector's, since a number of
// 8 or 9 digits may be an account's.
func (c *call) orderIDShown(given string, id int64) FreeText {
	if id >= 1_000_000_000 && given == strconv.FormatInt(id, 10) && !c.mask.known.Contains(given) {
		return c.mask.compose("%s", Ident(given))
	}
	return c.mask.free(lookup.Selector{Given: given}.Shown())
}

// getOrderIn is get_order's arguments.
type getOrderIn struct {
	ID         string `json:"id"`
	Account    string `json:"account,omitempty"`
	IncludeRaw bool   `json:"include_raw,omitempty"`
}

// getOrderOut is get_order's answer.
type getOrderOut struct {
	envelope
	cuts                  // the order's fills and the orders it holds, cut to fit the cap, after raw
	Order orderRow        `json:"order"`             // its detail always set
	Raw   json.RawMessage `json:"raw" masked:"json"` // null unless include_raw, as masker.json rewrote it
}

// dropRaw drops Schwab's JSON, the first thing to go when the answer
// passes the cap.
func (o *getOrderOut) dropRaw() bool {
	had := o.Raw != nil
	o.Raw = nil
	return had
}

// lists returns the order's fills and the orders it holds.
func (o *getOrderOut) lists() []cuttable { return o.Order.cuttables("order.") }

// getOrder answers get_order: the order of the account that the argument
// names, or of the one account that the login shares, by its ID.
func getOrder(ctx context.Context, c *call, in getOrderIn) (getOrderOut, error) {
	var out getOrderOut
	id, err := strconv.ParseInt(in.ID, 10, 64)
	if err != nil || id <= 0 {
		return out, c.failf(codeBadArgument, "id takes Schwab's order ID, a whole number such as 1004517688020.")
	}
	v, err := c.directory(ctx)
	if err != nil {
		return out, err
	}
	if v.schwabErr != nil {
		return out, v.schwabErr
	}
	var a dirAccount
	if strings.TrimSpace(in.Account) != "" {
		picked, err := v.pick(c, in.Account, false)
		if err != nil {
			return out, err
		}
		a = picked[0]
	} else {
		shared := v.shared()
		if len(shared) != 1 {
			return out, c.failf(codeBadArgument, "account is needed: the login shares %d accounts; pass account, such as the "+
				"order's account_hash from get_orders.", len(shared))
		}
		a = shared[0]
	}
	o, err := askAccount(ctx, c, &a, func(cl *schwab.Client, hash schwab.AccountHash) (schwab.Order, error) {
		return cl.Order(ctx, hash, id)
	})
	if err != nil && ctx.Err() != nil {
		return out, err
	}
	if err != nil {
		if se, ok := errors.AsType[*schwab.Error](err); ok && se.Status == http.StatusNotFound {
			return out, c.failf(codeNotFound, "Schwab has no order %s in account %s.", c.orderIDShown(in.ID, id), c.mask.masked(a.number))
		}
		return out, err
	}
	out.Source, out.Order = sourceSchwab, c.orderRowOf(a, o, true)
	if len(o.Problems) > 0 {
		c.warnProblems("The order", [][]schwab.Problem{o.Problems}, "a figure that could not be read is null")
	}
	if in.IncludeRaw {
		raw, report, err := c.mask.json(o.Raw, schwab.OrderKeyClass)
		if err != nil {
			return out, err
		}
		c.s.log.Debug("masked an order's raw JSON", "tool", c.tool, "account_numbers", report.AccountNumbers,
			"tags", report.Tags, "withheld", report.Withheld, "unknown_integers", report.UnknownIntegers)
		out.Raw = raw
	}
	return out, nil
}
