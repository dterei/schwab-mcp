package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The reference pages (design 11.1) are text compiled into the server,
// served as resources under schwab://reference/, which a day's time to
// live marks as fixed (setCacheable). They make no request, and say that
// they list Schwab's values as its spec lists them, which Schwab may add
// to. Each tool file registers the pages of its tools.

// A referencePage is one reference page, of Markdown.
type referencePage struct {
	uri, name, title, description, text string
}

// addReferencePage registers p, a resource that answers its text.
func (s *Server) addReferencePage(p referencePage) {
	s.addResource(&mcp.Resource{URI: p.uri, Name: p.name, Title: p.title, Description: p.description, MIMEType: "text/markdown"},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: p.uri, MIMEType: "text/markdown", Text: p.text}}}, nil
		})
}

// transactionTypesPage returns schwab://reference/transaction-types:
// Schwab's transaction types, what each row holds (docs/schwab-api.md,
// Transactions), and the local history's categories (docs/store.md,
// Categories). TestTransactionTypesPage holds it to
// schwab.AllTransactionTypes and history.Categories.
func transactionTypesPage() referencePage {
	return referencePage{
		uri:   referencePrefix + "transaction-types",
		name:  "transaction-types",
		title: "Schwab transaction types",
		description: "Schwab's transaction types, what each transaction row holds, and the local history's categories, " +
			"for reading get_transactions and get_transaction.",
		text: transactionTypesText,
	}
}

// transactionTypesText is the page of transaction types.
const transactionTypesText = "# Schwab's transaction types\n" + `
The transaction types as Schwab's spec lists them; Schwab may send others.
A row of get_transactions that comes from Schwab, or that the local history
keeps of Schwab's (row_kind api), carries one in type. A row of schwab.com's
History exports (row_kind file) has none, and its action instead.

| Type | What its rows have been seen to be |
| --- | --- |
| TRADE | An order's execution, which has an order ID; a reinvested dividend ("Dividend Reinvest"); the sale of a fraction of a share left over ("Sell Trade"). |
| RECEIVE_AND_DELIVER | Not seen yet; probably expirations, assignments, transfers in kind and corporate actions. |
| DIVIDEND_OR_INTEREST | A dividend ("Ordinary Dividend~XYZ", its security after the ~), or interest on cash ("Free Balance Interest Adjustment"). |
| ACH_RECEIPT | Not seen: an ACH deposit has come as ELECTRONIC_FUND. |
| ACH_DISBURSEMENT | Not seen. |
| CASH_RECEIPT | Money received ("Funds Received"). |
| CASH_DISBURSEMENT | Not seen. |
| ELECTRONIC_FUND | An ACH deposit ("ACH Credit Received"). |
| WIRE_OUT | Not seen. |
| WIRE_IN | An incoming wire ("Wire Incoming"). |
| JOURNAL | A move between two of the user's accounts; a sweep between the account's cash and the bank ("BANK SWEEP TO BROKERAGE", "TRF FDS TO TYPE 1"), which comes in pairs of opposite signs; tax withheld from a dividend of a nonresident's account ("W-8 Withholding~XYZ"). |
| MEMORANDUM | Not seen. |
| MARGIN_CALL | Not seen. |
| MONEY_MARKET | Not seen. |
| SMA_ADJUSTMENT | Not seen. |

## What a row holds

- amount is the cash that the transaction moved, positive into the account;
  fees are a trade's charges, positive; quantity is the shares or contracts,
  positive when bought; price is per share, and an option's is its premium
  per share, which its multiplier, usually 100, scales.
- date is the trade day, the New York date that Schwab booked the row on;
  time is when it happened, null when Schwab gives no time of day.
- detail full adds the legs. A security leg moves shares or contracts, with
  its price and its cost. A cash leg (CURRENCY_USD) carries a fee on a
  trade, which fee_type names when Schwab sends one, and the cash of any
  other row. A trade's fees are all its cash legs, named or not.
- status has been seen as VALID alone. An INVALID row counts in no total.
- Schwab may send a field in a form it has not used before; the warning
  problems then names it, and the figure is null.

## The local history's categories

Each row has a category, which the local history's rules give it by its
type, description and legs, or the user's own decision there
(category_rule user). categories filters by them, and totals add up by
them.

| Category | What it holds |
| --- | --- |
| deposit | Money in from outside the user's accounts: an ACH deposit, a wire, a check; a journal in that has no other side among the user's accounts. |
| withdrawal | Money out to outside the user's accounts; a journal out that has no other side. |
| transfer_in | Securities moved into the account. |
| transfer_out | Securities moved out of the account. |
| journal | Cash moved between two of the user's own accounts, paired with its other side: outside money to each account, inside the whole. |
| buy | A purchase, a reinvested dividend included. |
| sell | A sale, cash in lieu of a fraction and a cash merger included. |
| dividend | A dividend, a capital gain distribution or a return of capital. |
| interest | Interest on cash or bonds; margin interest, paid out, too. |
| fee | A fee charged on its own, such as an ADR fee; a trade's charges stay in its fees. |
| tax | Tax withheld or reclaimed, such as W-8 withholding or foreign tax. |
| other | Anything else: a sweep, a split, an expiration, and a kind of row that the rules do not settle yet, which the user can book with schwabtx edit. |

The local history counts each transaction once, whichever of its sources
hold it: counted and counted_why say whether a row counts and why (api,
file, tda, or policy: the user's policy for its action says count), or why
not (gone, invalid, linked, uncertain, held_out); user is the user's own
decision, either way.
A row read from Schwab carries the rules' category, before the user's
decisions and the local history's pairing.
`

// orderTypesPage returns schwab://reference/order-types: Schwab's order
// types, strategies, complex strategies, sessions, durations, instructions
// and position effects as its spec lists them, and how get_orders shows
// an order's figures (docs/schwab-api.md, Orders). TestOrderPages holds its
// lists to the spec's.
func orderTypesPage() referencePage {
	return referencePage{
		uri:   referencePrefix + "order-types",
		name:  "order-types",
		title: "Schwab order types",
		description: "Schwab's order types, strategies, sessions, durations and leg instructions, and what an order's " +
			"figures mean, for reading get_orders and get_order.",
		text: orderTypesText,
	}
}

// orderTypesText is the page of order types.
const orderTypesText = "# Schwab's order types\n" + `
The values as Schwab's spec lists them; Schwab may send others. Every
order read back so far, from thinkorswim or an app, has been a SINGLE
order of type LIMIT, NET_DEBIT or NET_CREDIT.

## order_type

| Type | What it is |
| --- | --- |
| MARKET | Buy or sell at the market's price; it has no price. |
| LIMIT | Buy at price or lower, or sell at price or higher. |
| STOP | A market order once the market reaches stop_price. |
| STOP_LIMIT | A limit order at price once the market reaches stop_price. |
| TRAILING_STOP | A stop that follows the market by an offset. |
| TRAILING_STOP_LIMIT | A trailing stop that becomes a limit order. |
| MARKET_ON_CLOSE | A market order at the close. |
| LIMIT_ON_CLOSE | A limit order at the close. |
| NET_DEBIT | An order of several legs, an option spread or stock with options (COVERED), bought for at most price, the net of its legs per share. |
| NET_CREDIT | An order of several legs, an option spread or stock with options, sold for at least price, the net of its legs per share. |
| NET_ZERO | An order of several legs at no net price. |
| EXERCISE | An option's exercise. |
| CABINET, NON_MARKETABLE, UNKNOWN | Rare kinds the spec lists. |

## strategy

SINGLE is one order. The others hold orders, which held lists, each with
the order that holds it in parent_id:

- TRIGGER places the orders it holds once it fills, as a bracket does: a
  buy that, once filled, places an OCO of a sale at a profit and a stop.
- OCO holds two orders, one cancels the other: when one fills, the other
  is canceled.
- CANCEL, RECALL, PAIR, FLATTEN, TWO_DAY_SWAP and BLAST_ALL are rare kinds
  the spec lists.

## complex_strategy

How an order's legs make up a spread: null for a single leg (Schwab's
NONE); COVERED (stock and a call), VERTICAL, BACK_RATIO, CALENDAR,
DIAGONAL, STRADDLE, STRANGLE, COLLAR_SYNTHETIC, BUTTERFLY, CONDOR,
IRON_CONDOR, VERTICAL_ROLL, COLLAR_WITH_STOCK, DOUBLE_DIAGONAL,
UNBALANCED_BUTTERFLY, UNBALANCED_CONDOR, UNBALANCED_IRON_CONDOR,
UNBALANCED_VERTICAL_ROLL, MUTUAL_FUND_SWAP or CUSTOM.

## session and duration

session is when the order may fill: NORMAL, the regular session; AM,
before it; PM, after it; SEAMLESS, all of them.

duration is how long it works: DAY, until the session ends;
GOOD_TILL_CANCEL, until it fills or is canceled, which Schwab ends after
some months; FILL_OR_KILL, all at once or not at all; IMMEDIATE_OR_CANCEL,
what fills at once, the rest canceled; END_OF_WEEK, END_OF_MONTH and
NEXT_END_OF_MONTH; UNKNOWN.

## A leg's instruction and position_effect

instruction is BUY or SELL for a stock or a fund; SELL_SHORT and
BUY_TO_COVER to open and close a short position, SELL_SHORT_EXEMPT too;
BUY_TO_OPEN, SELL_TO_OPEN, BUY_TO_CLOSE and SELL_TO_CLOSE for an option;
EXCHANGE for a mutual fund's exchange. position_effect is OPENING or
CLOSING, or AUTOMATIC.

## What an order's figures mean

- Figures are as Schwab sends them, never signed: instruction says which
  way a leg goes.
- price is the limit price per share; an option's is its premium per
  share, which its multiplier, usually 100, scales; a NET_DEBIT or
  NET_CREDIT order's is the net of its legs per share, which a spread's
  multiplier scales, as an option's premium. stop_price is the
  stop. A figure Schwab did not send is null, never 0.
- quantity is the shares or contracts ordered, filled what has filled and
  remaining what is left; a spread's are in spreads, and each leg's
  quantity is that leg's.
- A leg's fill_price is the average price of its fills, per share. detail
  full lists each fill, with its time.
- entered is when the order was placed; closed when it filled, was
  canceled or rejected, was replaced, or expired.
`

// orderStatusesPage returns schwab://reference/order-statuses: Schwab's
// order statuses as its spec lists them, what each means, and which are
// final. TestOrderPages holds it to schwab.AllOrderStatuses and
// OrderStatus.Final.
func orderStatusesPage() referencePage {
	return referencePage{
		uri:   referencePrefix + "order-statuses",
		name:  "order-statuses",
		title: "Schwab order statuses",
		description: "Schwab's order statuses, what each means, and which are final, for reading get_orders' status " +
			"and open, and for its statuses filter.",
		text: orderStatusesText,
	}
}

// orderStatusesText is the page of order statuses.
const orderStatusesText = "# Schwab's order statuses\n" + `
The statuses as Schwab's spec lists them; Schwab may send others. An
order is open, and may still fill or change, in any status but the five
final ones; get_orders' open is true then, and open: true asks for those
orders alone. Of these, FILLED, CANCELED, REJECTED, REPLACED and
PENDING_ACTIVATION have been seen.

| Status | Final | What it means |
| --- | --- | --- |
| AWAITING_PARENT_ORDER | no | Held by another order, as a bracket's orders are, until that one fills. |
| AWAITING_CONDITION | no | Waiting for a condition to be met. |
| AWAITING_STOP_CONDITION | no | A stop order waiting for the market to reach its stop. |
| AWAITING_MANUAL_REVIEW | no | Waiting for Schwab to review it. |
| ACCEPTED | no | Accepted, not yet working. |
| AWAITING_UR_OUT | no | Waiting for an earlier order's cancel or replacement to finish. |
| PENDING_ACTIVATION | no | Entered outside the session it is for, waiting for it to open. |
| QUEUED | no | Queued to be sent to the market. |
| WORKING | no | At the market, waiting to fill. |
| REJECTED | yes | Refused; status_description says why. |
| PENDING_CANCEL | no | A cancel was asked for and has not finished. |
| CANCELED | yes | Canceled before it filled in full; filled says what had filled. |
| PENDING_REPLACE | no | A replacement was asked for and has not finished. |
| REPLACED | yes | Replaced by another order, which has its own order_id. |
| FILLED | yes | Filled in full. |
| EXPIRED | yes | Ended with its duration before it filled in full. |
| NEW | no | Just entered. |
| AWAITING_RELEASE_TIME | no | Waiting for the time it is to be sent. |
| PENDING_ACKNOWLEDGEMENT | no | Sent, waiting for the market to acknowledge it. |
| PENDING_RECALL | no | Being recalled. |
| UNKNOWN | no | Schwab does not say. |

A status that is not on this list is taken as open. A rejected order's
status_description says why, in Schwab's words, such as "You do not have
enough available cash/buying power for this order."
`
