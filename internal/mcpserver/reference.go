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
file, tda, user), or why not (gone, invalid, linked, uncertain, held_out).
A row read from Schwab carries the rules' category, before the user's
decisions and the local history's pairing.
`
