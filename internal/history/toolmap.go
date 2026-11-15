package history

import (
	"cmp"
	"database/sql"
	"fmt"
	"maps"
	"math"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// This file maps the ledger to the records that a tool export writes, for
// Portfolio Performance (PP) or Ghostfolio (GF), without a store: everything
// it reads is in a toolInput, and every function here is a function of its
// arguments alone, so that the mapping is tested row by row. It reads
// nothing of what an earlier export gave the tool: a full export and one
// made with -since map every row alike. Its comments cite the tool
// exports' design by section, as "(3.2 of the design)", and the store's
// design as the store's.

// The tools that a tool export writes for, as -format names them.
const (
	toolPP = "pp"
	toolGF = "ghostfolio"
)

// What becomes of a ledger row in a tool export (3.1 of the design). Each
// row has one outcome.
const (
	// outcomeWritten is a row written as one record or more.
	outcomeWritten = "written"
	// outcomeCombined is the shares row of a two-row cash event, which its
	// cash row's sale writes.
	outcomeCombined = "combined"
	// outcomeInBalances is, for Ghostfolio, a cash row that the account's
	// balances carry.
	outcomeInBalances = "in_balances"
	// outcomeLeftOut is a row that the export leaves out, for a reason.
	outcomeLeftOut = "left_out"
)

// A toolMap is a line of the mapping of the ledger to the tools (3.3 of the
// design), as docs/store.md lists it: the rows of a category that a
// condition picks, the event they become, and what each tool gets of it.
type toolMap struct {
	category, when, event, pp, gf string
}

// toolMapping is the mapping, line by line: the contract that toolEvents
// keeps, which TestToolMapping runs a row of each line through and
// docs/store.md lists.
var toolMapping = []toolMap{
	{"deposit", "amount > 0", "deposit", "account: Deposit", "account balance"},
	{"deposit", "amount < 0", "withdrawal", "account: Withdrawal", "account balance"},
	{"withdrawal", "amount > 0", "deposit", "account: Deposit", "account balance"},
	{"withdrawal", "amount < 0", "withdrawal", "account: Withdrawal", "account balance"},
	{
		"journal", "cash, paired with a counted journal of another account of the opposite amount, both booked `journal`, both accounts exported", "transfer",
		"account: Transfer (Outbound) in the paying account's file, Transfer (Inbound) in the receiving one's", "account balances",
	},
	{"journal", "cash, any other, amount > 0", "deposit", "account: Deposit", "account balance"},
	{"journal", "cash, any other, amount < 0", "withdrawal", "account: Withdrawal", "account balance"},
	{"journal", "shares, quantity > 0", "delivery in", "portfolio: Delivery (Inbound)", "BUY"},
	{"journal", "shares, quantity < 0", "delivery out", "portfolio: Delivery (Outbound)", "SELL"},
	{"transfer_in", "quantity > 0", "delivery in", "portfolio: Delivery (Inbound)", "BUY"},
	{"transfer_in", "quantity < 0", "delivery out", "portfolio: Delivery (Outbound)", "SELL"},
	{"transfer_out", "quantity > 0", "delivery in", "portfolio: Delivery (Inbound)", "BUY"},
	{"transfer_out", "quantity < 0", "delivery out", "portfolio: Delivery (Outbound)", "SELL"},
	{"buy", "quantity > 0", "buy", "portfolio: Buy", "BUY"},
	{"buy", "quantity < 0 (a Cancel Buy)", "sell", "portfolio: Sell", "SELL"},
	{"sell", "quantity < 0", "sell", "portfolio: Sell", "SELL"},
	{"sell", "quantity > 0 (a Cancel Sell)", "buy", "portfolio: Buy", "BUY"},
	{"sell", "no quantity, with the shares row of its two-row event", "sell", "portfolio: Sell", "SELL"},
	{"dividend", "amount > 0", "dividend", "account: Dividend", "DIVIDEND"},
	{"dividend", "amount < 0", "fees", "account: Fees", "FEE"},
	{"interest", "amount > 0, no security", "interest", "account: Interest", "INTEREST"},
	{"interest", "amount > 0, a security (a bond's coupon)", "dividend", "account: Dividend", "DIVIDEND (INTEREST of the interest profile when the bond is left out)"},
	{"interest", "amount < 0", "interest charge", "account: Interest Charge", "FEE"},
	{"fee", "amount < 0", "fees", "account: Fees", "FEE"},
	{"fee", "amount > 0", "fees refund", "account: Fees Refund", "INTEREST of the fees profile"},
	{"tax", "amount < 0", "taxes", "account: Taxes", "FEE"},
	{"tax", "amount > 0", "tax refund", "account: Tax Refund", "INTEREST of the taxes profile"},
	{"other", "an option expired, assigned or exercised, no cash, quantity < 0", "close out", "portfolio: Delivery (Outbound), Value 0", "SELL at 0"},
	{"other", "the same, quantity > 0", "close in", "portfolio: Delivery (Inbound), Value 0", "BUY at 0"},
	{"other", "the shares of a two-row cash event", "(with its cash row)", "(one Sell)", "(one SELL)"},
	{"other", "anything else, and sweep pairs", "left out", "left out", "left out"},
}

// A toolReason is why a tool export leaves a ledger row out (3.7 of the
// design), as docs/store.md lists it: its code, what it means, and what
// the manifest advises (leftOutAdvice), in words.
type toolReason struct {
	code, means, advice string
}

// toolReasons are the reasons, in the order of the docs and of the counts
// that stderr prints.
var toolReasons = []toolReason{
	{"no_day", "a row with neither a trade date nor a time", "`check` with `schwabtx status REF`"},
	{"sweep_pair", "a bank sweep and its return, which move cash within the account", "none"},
	{"other", "booked `other`", "by kind: `split`, `corporate_action`, or `set_category` with `schwabtx edit REF -category C`"},
	{"no_amount", "the amount, or a leg's quantity or cost, is off the cent or millionth grid", "`check` with `schwabtx status REF`"},
	{"zero_amount", "a cash row of 0.00", "none"},
	{"no_quantity", "a trade or move without a quantity (Cash In Lieu, the cash row of an event whose shares row is not found)", "`by_hand`"},
	{"no_security", "needs a security, has none", "`set_security` with `schwabtx edit REF -security K:…`"},
	{"signs_disagree", "a trade's cash has its quantity's sign", "`check` with `schwabtx status REF`"},
	{"legs_mismatch", "a trade of several legs whose legs do not add up to its cash", "`check`"},
	{"future", "a futures contract", "`by_hand`"},
	{"multiplier", "an option whose multiplier is not a whole number from 1 to 10000, or whose keys in the store give two", "`by_hand`"},
	{"bond_unit", "a bond whose rows or prices do not follow face value and prices per 100", "`check`: the probe run settles the API's unit"},
	{
		"gf_cash", "Ghostfolio: a cash row posted after the account's latest snapshot was taken (one with only a date: on or after the day it was taken), or of an account with no snapshot, which no balance carries yet",
		"`set_cash`: a later snapshot carries it; set the account's cash in Ghostfolio to that snapshot's, or take snapshots (`schwabtx snapshot`) of an account that has none",
	},
}

// leftOutAdvice returns what the manifest advises for row r, left out for
// reason: the advice's code, "" when there is nothing to do, and the
// command that does it or shows the row, "" for none. A row booked other
// is advised by its kind (normalAction): a split, another corporate action,
// or else a category to set.
func leftOutAdvice(reason string, r *toolRow) (advice, command string) {
	switch reason {
	case "no_day", "no_amount", "signs_disagree":
		return "check", "schwabtx status " + r.ref
	case "legs_mismatch", "bond_unit":
		return "check", ""
	case "no_quantity", "future", "multiplier":
		return "by_hand", ""
	case "no_security":
		return "set_security", "schwabtx edit " + r.ref + " -security K:…"
	case "gf_cash":
		return "set_cash", "schwabtx snapshot"
	case "other":
		switch a := normalAction(r.kind); {
		case slices.Contains(splitActions, a):
			return "split", ""
		case slices.Contains(corporateActions, a):
			return "corporate_action", ""
		}
		return "set_category", "schwabtx edit " + r.ref + " -category C"
	}
	return "", ""
}

// splitActions and corporateActions are the kinds, normalized, of the rows
// booked other that the manifest advises as a split or as another corporate
// action; closeActions those of a file's rows that close an option (4.3 of
// the design), which an option close takes before a corporate action.
var (
	splitActions     = normalActions("Stock Split", "Reverse Split", "Options Frwd Split", "Options Frwd Split Adj")
	corporateActions = normalActions("Stock Merger", "Name Change", "Conversion", "Spin-off", "Stock Div Dist",
		"Cash Merger Adj", "Full Redemption", "Exchange or Exercise")
	closeActions = normalActions("Expired", "Assigned", "Exercised", "Exchange or Exercise")
)

// normalActions returns actions as normalAction gives them.
func normalActions(actions ...string) []string {
	out := make([]string, len(actions))
	for i, a := range actions {
		out[i] = normalAction(a)
	}
	return out
}

// A toolCaveat is what a tool export says of a row that it writes, but not
// quite as the store holds it (3.7 of the design), as docs/store.md lists
// it: its code, the tool it is for ("" for both), and what it means.
type toolCaveat struct {
	code, tool, means string
}

// toolCaveats are the caveats, in the order of the docs, of each row's list
// in the manifest and of the counts that stderr prints.
var toolCaveats = []toolCaveat{
	{"value_unknown", "", "a delivery with no price on either side and no snapshot price of its day, written at 0: set its value in the tool"},
	{"value_from_snapshot", "", "a delivery with no price, valued at the security's snapshot price of the day it was delivered"},
	{"assumed_multiplier", "", "an option without a multiplier, taken as 100"},
	{"cash_not_written", "", "a delivery that moved cash too, which the delivery does not carry"},
	{"fees_unknown", "", "fees off the cent grid, written as 0: the cash is right and the fees ride in the price"},
	{"pair_amounts_differ", "", "a journal whose partner's amount is not the opposite of its own, written by its sign"},
	{"pair_not_journal", "", "a journal whose partner you booked as another category, each written by its own"},
	{"pair_outside", "", "a journal with an account that the export leaves out, written by its sign"},
	{"return_of_capital", "", "a return of capital, written as a dividend: it should lower the cost basis, which neither tool can do"},
	{"option_closed_at_zero", "", "an option expired, assigned or exercised, closed by a delivery of value 0"},
	{"fees_apart", toolPP, "a sale whose fees exceed its proceeds, written as a Sell of the proceeds and a Fees row of the fees"},
	{"coupon_as_interest", toolGF, "a coupon of a bond whose trades are left out (bond_unit), written as interest of the interest profile"},
}

// An eventKind is what a record of a tool export is (3.3 of the design).
type eventKind int

// The kinds of record. A transfer is a cash journal between two accounts
// of the export, written once in each account's file.
const (
	evDeposit eventKind = iota + 1
	evWithdrawal
	evTransferOut // the paying account's side of a transfer
	evTransferIn  // the receiving account's side of a transfer
	evDeliveryIn
	evDeliveryOut
	evBuy
	evSell
	evDividend
	evFees
	evInterest
	evInterestCharge
	evFeesRefund
	evTaxes
	evTaxRefund
	evCloseOut // an option removed at 0: expired, assigned or exercised
	evCloseIn  // a short option removed at 0
)

// eventKinds says, for each kind, its name in toolMapping's event column;
// the file that PP takes it in (account or portfolio) and its Type there,
// as the PP import configurations label it; Ghostfolio's activity type, ""
// when Ghostfolio's cash is the accounts' balances instead; and whether it
// adds shares (1), takes them away (-1), or moves none (0).
var eventKinds = [...]struct {
	name, ppFile, ppType, gfType string
	shares                       int64
}{
	evDeposit:        {"deposit", "account", "Deposit", "", 0},
	evWithdrawal:     {"withdrawal", "account", "Withdrawal", "", 0},
	evTransferOut:    {"transfer", "account", "Transfer (Outbound)", "", 0},
	evTransferIn:     {"transfer", "account", "Transfer (Inbound)", "", 0},
	evDeliveryIn:     {"delivery in", "portfolio", "Delivery (Inbound)", "BUY", 1},
	evDeliveryOut:    {"delivery out", "portfolio", "Delivery (Outbound)", "SELL", -1},
	evBuy:            {"buy", "portfolio", "Buy", "BUY", 1},
	evSell:           {"sell", "portfolio", "Sell", "SELL", -1},
	evDividend:       {"dividend", "account", "Dividend", "DIVIDEND", 0},
	evFees:           {"fees", "account", "Fees", "FEE", 0},
	evInterest:       {"interest", "account", "Interest", "INTEREST", 0},
	evInterestCharge: {"interest charge", "account", "Interest Charge", "FEE", 0},
	evFeesRefund:     {"fees refund", "account", "Fees Refund", "INTEREST", 0},
	evTaxes:          {"taxes", "account", "Taxes", "FEE", 0},
	evTaxRefund:      {"tax refund", "account", "Tax Refund", "INTEREST", 0},
	evCloseOut:       {"close out", "portfolio", "Delivery (Outbound)", "SELL", -1},
	evCloseIn:        {"close in", "portfolio", "Delivery (Inbound)", "BUY", 1},
}

// String returns the kind's name in toolMapping's event column; both sides
// of a transfer are a transfer there.
func (k eventKind) String() string { return eventKinds[k].name }

// ppType returns the PP file that takes a record of the kind, account or
// portfolio, and its Type there.
func (k eventKind) ppType() (file, typ string) { return eventKinds[k].ppFile, eventKinds[k].ppType }

// gfType returns Ghostfolio's activity type for a record of the kind: BUY,
// SELL, DIVIDEND, FEE or INTEREST; "" for a deposit, a withdrawal or a
// transfer, which Ghostfolio's account balances carry instead.
func (k eventKind) gfType() string { return eventKinds[k].gfType }

// shareSign returns 1 for a kind that adds shares to a holding, -1 for one
// that takes them away, and 0 for one that moves none.
func (k eventKind) shareSign() int64 { return eventKinds[k].shares }

// A toolInput is what a tool export reads of the store (2 of the design),
// Q0 to Q12, as plain values, with the export's options: everything that
// toolEvents and the functions beside it read. Days are New York dates as
// the store writes them, "" for none; instants are the zero time for none;
// money is in whole cents and quantities in whole millionths, as the store
// holds them, NULL off the grid.
type toolInput struct {
	// tool is the tool that the export is for: toolPP or toolGF.
	tool string
	// account is the account that the export selects, by its id (A<id>);
	// 0 selects every account. Q2, Q3, Q7, Q8, Q10 and Q11 hold only its
	// rows.
	account int64
	// to is the last day that the export selects (-to): Q2, Q3, Q6 and Q8
	// end there, and Q11 begins after it.
	to nytime.Date
	// manual are the symbols that -manual names for Ghostfolio, with those
	// that the chain's earlier exports named, each as given.
	manual []string
	// store is when the store was made (Q0), as an instant in the store's
	// form: a chain of exports stays in one store.
	store string
	// accounts are every account of the store (Q1), whatever the export
	// selects: a transfer names the other account.
	accounts []toolAccount
	// rows are the ledger rows that the export selects (Q2), in the order
	// of their day, time, account and ref.
	rows []toolRow
	// legs are the security legs of the selected API rows that have no one
	// quantity (Q3), in the order of their account, ref and leg.
	legs []toolLeg
	// securities are every security of the store (Q4), and names their
	// symbols and descriptions (Q5).
	securities []toolSecurityRow
	names      []toolName
	// positions are every account's positions in its closed-phase
	// snapshots up to to (Q6), for prices and the holdings check.
	positions []toolPosition
	// snapshots are the selected accounts' closed-phase snapshots of every
	// day (Q7), whatever to says: Ghostfolio takes balances only in the
	// import that makes an account.
	snapshots []toolSnapshot
	// waiting are the file rows that wait for you to resolve them (Q8).
	waiting []toolWaiting
	// coverage is the days each source covers of the selected accounts
	// (Q9).
	coverage []ExportCoverage
	// uncounted are the selected accounts' entries that the store does not
	// count (Q10), and later the rows that it counts but dates after to
	// (Q11), none for no to: they say why a row that a tool holds went.
	uncounted []toolUncounted
	later     []toolLaterRow
	// days are when the store's rows and legs, of every account and day,
	// counted or not, had each symbol of each security (Q12).
	days []toolSymbolDays
}

// A toolSymbolDays is when the store's rows and legs had a symbol of a
// security (Q12): the first and the last day of those that name the
// security of key by it, key "" for the rows of a symbol without one. A
// name's instant is the read's or the file's that brought it (Q5), which
// may hold years of rows; their days say when the security went by it.
type toolSymbolDays struct {
	key, symbol string
	first, last nytime.Date
}

// selected reports whether the export selects the account of id.
func (in *toolInput) selected(id int64) bool { return in.account == 0 || in.account == id }

// accountOf returns the account of id, as Q1 read it; an id that Q1 did
// not read has no number.
func (in *toolInput) accountOf(id int64) toolAccount {
	for _, a := range in.accounts {
		if a.id == id {
			return a
		}
	}
	return toolAccount{id: id}
}

// A toolAccount is an account as a tool export names it (Q1, 3.6 of the
// design): by its ref and the last digits of its number, which never
// change, and not by its nickname, which you can change at Schwab.
type toolAccount struct {
	// id is its id (A<id>); masked its number masked, as "...135".
	id     int64
	masked string
	// nickname is its nickname at Schwab, masked free text, "" for none:
	// stderr and the manifest show it beside the name.
	nickname string
}

// label returns how the export names the account in notes, comments and
// names: its ref and its number masked, as "A1 ...135"; its ref alone when
// it has no masked number.
func (a toolAccount) label() string {
	ref := "A" + strconv.FormatInt(a.id, 10)
	if a.masked == "" {
		return ref
	}
	return ref + " " + a.masked
}

// name returns the name of the account's PP deposit and securities
// accounts, and of its Ghostfolio account: "Schwab " and its label.
func (a toolAccount) name() string { return "Schwab " + a.label() }

// suffix returns what names the account's PP files: its ref and the digits
// of its masked number, as "A1-135"; its ref alone when there are none.
func (a toolAccount) suffix() string {
	ref := "A" + strconv.FormatInt(a.id, 10)
	digits := strings.Map(func(r rune) rune {
		if '0' <= r && r <= '9' {
			return r
		}
		return -1
	}, a.masked)
	if digits == "" {
		return ref
	}
	return ref + "-" + digits
}

// gfID returns the account's id in Ghostfolio: the name-based UUID of
// "account " and its label (6.7 of the design).
func (a toolAccount) gfID() string { return uuid8(toolNamespace, "account "+a.label()) }

// A toolRow is a ledger row that a tool export selects (Q2): an entry that
// the store counts, with what the export reads of its journal partner.
type toolRow struct {
	// ref and account name it: its ref, and its account's id.
	ref     string
	account int64
	// source is "api" or "file".
	source string
	// day is the day it happened: an API row's trade date, a file row's "as
	// of" date when it has one, else its posted date; "" for an API row
	// with neither a trade date nor a time. posted is a file row's posted
	// date.
	day, posted nytime.Date
	// time is an API row's instant, the zero time for none.
	time time.Time
	// kind is Schwab's type of an API row or a file row's Action, and
	// description its description, masked free text.
	kind, description string
	// amount is its cash, in cents, positive in; fees its fees, in cents,
	// positive; gross amount plus fees. Each is NULL off the cent grid,
	// and fees and gross when a fee is.
	amount, fees, gross sql.NullInt64
	// quantity is what it moved of its one security, in millionths of a
	// share or contract, positive in; NULL when it moved none, several (a
	// trade of several legs) or a quantity off the grid.
	quantity sql.NullInt64
	// quantityAsOf is the day of the export whose quantity a file row
	// shows: a split between it and the row's day restates the quantity.
	quantityAsOf nytime.Date
	// price is its price in dollars per share or contract (per 100 of
	// face for a bond), as the store holds it; NULL for none.
	price sql.NullFloat64
	// symbol is the symbol that it names, "" for none; securityKey the key
	// of its security, "" for none.
	symbol, securityKey string
	// category is what it is booked as (categories).
	category string
	// pairKind is journal, sweep or reversal when it is paired, else "";
	// pairRef and pairAccount name its partner.
	pairKind, pairRef string
	pairAccount       int64
	// flags are the codes of its flags.
	flags []string
	// partner is what the export reads of its partner, when pairRef is set.
	partner toolPartner
}

// A toolPartner is what a tool export reads of a ledger row's partner,
// whether the export selects it or not: a journal's other side.
type toolPartner struct {
	// day and time are its own.
	day  nytime.Date
	time time.Time
	// amount is its cash, in cents, positive in.
	amount sql.NullInt64
	// price is its price in dollars per share or contract (per 100 of
	// face for a bond), NULL for none.
	price sql.NullFloat64
	// counted says that the store counts it.
	counted bool
	// category is what it is booked as.
	category string
	// kind and description are its own, the description masked: a
	// transfer's note names the paying side's.
	kind, description string
}

// A toolLeg is a security leg of an API row that has no one quantity (Q3):
// each option of a spread, each security of a journal of several.
type toolLeg struct {
	// account and ref name the row; leg is the leg's index among the
	// transaction's legs, from 0.
	account int64
	ref     string
	leg     int
	// symbol and securityKey are the leg's security's: its symbol, and its
	// key (Q4 gives its asset type).
	symbol, securityKey string
	// quantity is what the leg moved, in millionths, positive in; cost its
	// cash, in cents, positive in (13.1 of the store's design).
	quantity, cost sql.NullInt64
	// price is its price in dollars per share or contract, NULL for none.
	price sql.NullFloat64
}

// A toolSecurityRow is a security of the store (Q4, the securities view).
type toolSecurityRow struct {
	// key is its security key; assetType and typ Schwab's asset type and
	// type of it, "" for none.
	key, assetType, typ string
	// symbol is its latest symbol, cusip its latest CUSIP, description its
	// latest description (masked free text); each "" for none.
	symbol, cusip, description string
	// putCall, strike, expiration and underlying are an option's: PUT or
	// CALL, the strike in dollars, the expiration day and the underlying's
	// symbol.
	putCall    string
	strike     sql.NullFloat64
	expiration nytime.Date
	underlying string
	// multiplier is an option's or a future's multiplier, NULL for none.
	multiplier sql.NullFloat64
}

// A toolName is a symbol or a description that a security has had (Q5):
// kind is "symbol" or "description" (masked free text), and firstSeen and
// lastSeen when the store first and last saw it.
type toolName struct {
	key, kind, value    string
	firstSeen, lastSeen time.Time
}

// A toolPosition is a position of a closed-phase snapshot (Q6).
type toolPosition struct {
	// key is its security's key, and account its account's id.
	key     string
	account int64
	// marketDate is the session that the snapshot stands for, and
	// fetchedAt when it was taken.
	marketDate nytime.Date
	fetchedAt  time.Time
	// long and short are its long and short quantities, in millionths.
	long, short sql.NullInt64
	// marketValue is its market value in dollars, negative for a short.
	marketValue sql.NullFloat64
}

// A toolSnapshot is a closed-phase snapshot of a selected account (Q7).
type toolSnapshot struct {
	// account is its account's id; marketDate the session that it stands
	// for; fetchedAt when it was taken.
	account    int64
	marketDate nytime.Date
	fetchedAt  time.Time
	// cash is the account's cash balance (currentBalances.cashBalance), in
	// cents, signed; NULL when Schwab sent none or one off the grid.
	cash sql.NullInt64
}

// A toolWaiting is a file row that waits for you to resolve it (Q8): held
// out, or uncertain which API row it is; neither counts until you decide.
type toolWaiting struct {
	ref     string
	account int64
	day     nytime.Date
	kind    string
	// amount is its cash, in cents.
	amount sql.NullInt64
	// why is held_out or uncertain.
	why string
}

// A toolUncounted is an entry of a selected account that the store does
// not count (Q10): why is its counted_why, and matchedWith the refs of the
// entries it is linked to, comma-separated, "" for none.
type toolUncounted struct {
	ref              string
	account          int64
	why, matchedWith string
}

// A toolLaterRow is a row of a selected account that the store counts and
// dates after the export's to (Q11), on day: a revision may have moved a
// row that an earlier export took there.
type toolLaterRow struct {
	ref     string
	account int64
	day     nytime.Date
}

// A toolEvent is one record that a tool export writes: a row of a PP file
// or a Ghostfolio activity. A ledger row that the export writes becomes
// one record or more (a leg each of a trade of several legs; a PP Fees row
// beside a sale whose fees exceed its proceeds), and every record is of
// one row. Money is in whole cents and shares in whole millionths of a
// share, as the store holds them.
type toolEvent struct {
	// row is the index in toolInput.rows of the ledger row that it writes.
	row int
	// ref is that row's ref, and account its account. leg is the leg's
	// index among its transaction's legs (Q3) when the record is one leg
	// of a trade or journal of several; NULL otherwise.
	ref     string
	account toolAccount
	leg     sql.NullInt64
	// with is the ref of the shares row of a two-row cash event, whose
	// shares this record of its cash row's sale carries; "" otherwise.
	with string
	// kind is what the record is: its PP file and Type, its Ghostfolio
	// type, and whether it moves shares in or out.
	kind eventKind
	// day is the record's New York date, and time its instant, the zero
	// time for none (a file row's): the row's own, but for a transfer, whose
	// two sides take the paying side's.
	day  nytime.Date
	time time.Time
	// text is what the record's note says of it: the row's description,
	// else its kind (masked free text); a transfer's is the paying side's.
	text string
	// value is the record's value in cents, unsigned, as PP's Value writes
	// it: what a buy cost with its fees, what a sale brought in (its gross
	// when its fees, written apart, exceed it), a delivery's value, 0 for
	// an option closed, the cash of any other.
	value int64
	// fees are the fees in cents written with a buy, a sale or a dividend,
	// 0 or more (0 for a sale whose fees PP gets in a Fees row of their
	// own); gross is a trade's or a dividend's cash before its fees, in
	// cents, signed (a buy's below 0): Ghostfolio's unit price is |gross|
	// over shares, and a dividend's unit price gross.
	fees, gross int64
	// amount is the cash in cents of the row, or of the leg, signed, as
	// the store holds it, or as a trade of several legs splits it: what a
	// delivery's caveat says it did not write.
	amount int64
	// shares is what a buy, a sale, a delivery or a close moved, in
	// millionths of a share, unsigned: the quantity times the security's
	// multiplier (a bond's face value, in millionths of a dollar); 0 for a
	// record of cash.
	shares int64
	// price is a delivery's price per share, in dollars, exact: the
	// delivering side's (÷ 100 for a bond), else a snapshot price, else 0;
	// 0 for an option closed; nil for any other record.
	price *big.Rat
	// security is the security that the record names, nil for none: not
	// for a deposit, withdrawal, transfer, interest or interest charge,
	// which PP refuses with a security, nor for a Ghostfolio activity of
	// a cash profile.
	security *toolSecurity
	// profile is the Ghostfolio cash profile that a FEE or an INTEREST
	// activity is of: fees, interest or taxes; "" for none, as for every PP
	// record.
	profile string
	// pair and pairRef are the account and the ref of the other side of a
	// transfer, or of the partner of a journal written apart from it (a
	// pair_ caveat); the zero account and "" for none.
	pair    toolAccount
	pairRef string
	// caveats are the record's caveats (toolCaveats), in their order.
	caveats []string
}

// clock returns the record's time on New York's clock, in seconds since
// midnight, when it has a time whose New York date is its day: PP's Time
// and Ghostfolio's place in the day come from it. An evening fill booked on
// the next day has none.
func (e *toolEvent) clock() (int, bool) {
	if e.time.IsZero() {
		return 0, false
	}
	day, seconds := nyClock(e.time)
	return seconds, day == e.day
}

// gfOffset returns the milliseconds that Ghostfolio's date of the record
// adds to noon UTC of its day (6.5 of the design), so that a day's
// activities keep their order: two for each second of the record's place
// in the day, and one more for a SELL. Its place is an option close's
// 86 400, after every other; a record with a time on its day, its seconds
// since New York's midnight; any other's 0, so that a purchase of the day
// comes first. Ghostfolio holds a security as one across every account and
// orders the activities of one instant at random, so a BUY and a SELL of
// one second, as the two sides of a journal of shares can be, never share
// one: the BUY comes first.
func (e *toolEvent) gfOffset() int64 {
	var place int64
	switch seconds, ok := e.clock(); {
	case e.kind == evCloseOut || e.kind == evCloseIn:
		place = 86_400
	case ok:
		place = int64(seconds)
	}
	if e.kind.gfType() == "SELL" {
		return 2*place + 1
	}
	return 2 * place
}

// gfDate returns Ghostfolio's date of the record (gfInstant).
func (e *toolEvent) gfDate() string { return gfInstant(e.day, e.gfOffset()) }

// A toolOutcome is what a tool export does with a ledger row (3.1 of the
// design). toolEvents gives one for each row, in the rows' order.
type toolOutcome struct {
	// ref is the row's ref, and account its account.
	ref     string
	account toolAccount
	// outcome is outcomeWritten, outcomeCombined, outcomeInBalances or
	// outcomeLeftOut.
	outcome string
	// reason is why a row is left out (toolReasons), and advice and
	// command what the manifest advises (leftOutAdvice).
	reason, advice, command string
	// with is the other row of a two-row cash event: the shares row of a
	// cash row written, the cash row that a shares row is combined with or
	// left out with.
	with string
	// caveats are a written row's caveats (toolCaveats), in their order.
	caveats []string
	// flags are the codes of the row's flags of severity attention, for a
	// row written, combined or in the balances, which the manifest lists.
	flags []string
}

// A secRef is how the export knows a security: by its key, or, for a row
// without one, by its symbol.
type secRef struct{ key, symbol string }

// A toolSecurity is a security as the tool exports write it (3.5 of the
// design): its identifiers, the multiplier of its quantities, and what
// makes it a security that a tool has no prices for.
type toolSecurity struct {
	// key is its security key, "" for the security that a row without one
	// names by its symbol alone.
	key string
	// symbol is its latest symbol (the securities view's), or that row's.
	symbol string
	// assetType is Schwab's asset type of it, "" for none; OPTION for a row
	// without a key whose symbol names a contract.
	assetType string
	// symbols are every symbol it has had, the latest first (symbolOrder),
	// and tickers those of them that are not CUSIPs.
	symbols, tickers []string
	// cusip is its CUSIP, "" when the store knows none.
	cusip string
	// delisted says that its latest symbol is its CUSIP although it had a
	// ticker: Schwab sends a delisted security's CUSIP in place of its
	// symbol.
	delisted bool
	// bond says that it is a bond, a CD or a Treasury, whose quantity is
	// face value and whose price is per 100 of face.
	bond bool
	// contract is the option contract that its symbols name, and occ its
	// OCC compact form, "" for none (a futures option has none).
	contract schwab.OCC
	occ      string
	// multiplier is what a quantity of 1 stands for in shares: an option's
	// multiplier, one for all of the store's securities that the tools
	// take for one (multiplier), else 1. assumed says that an option's
	// multiplier of 100 is assumed, for want of one; badMultiplier that an
	// option's multiplier is not a whole number from 1 to 10 000, or not
	// one, which leaves its rows out.
	multiplier             int64
	assumed, badMultiplier bool
	// name is the description that the store saw first (masked free text);
	// with none, and neither ticker nor CUSIP, its latest symbol; else "".
	// described says that it is a description, and nameSeen when the store
	// first saw it.
	name      string
	described bool
	nameSeen  time.Time
	// rep is the first, in the order of their keys (keyRank), of the
	// store's securities that the tools take for one with it (groupAll),
	// whose key or symbol names them all when nothing else does; reused
	// says that its ticker is one that another security has had too, of
	// another CUSIP or at another time, as when a company takes a ticker
	// that another gave up, so that it is not its identity.
	rep    *toolSecurity
	reused bool
	// bondUnit says that it is a bond whose rows or snapshot prices, of any
	// of the store's securities that the tools take for one with it, do not
	// follow face value and prices per 100 (4.4 of the design): its trades,
	// deliveries and prices are left out.
	bondUnit bool
	// manual is why Ghostfolio gets it as a MANUAL asset: option, bond,
	// no_ticker, delisted, not_plain or asked (-manual); "" when Ghostfolio
	// asks Yahoo for it.
	manual string
}

// ref returns how the export knows the security: two toolSecurity values
// of one security, from two functions of this file, have one ref.
func (s *toolSecurity) ref() secRef {
	if s.key != "" {
		return secRef{key: s.key}
	}
	return secRef{symbol: s.symbol}
}

// is reports whether s is the security of a row or a leg whose security
// key is key and whose symbol is symbol: by its key, or, when neither has
// one, by its symbol.
func (s *toolSecurity) is(key, symbol string) bool {
	if s.key != "" {
		return key == s.key
	}
	return key == "" && symbol == s.symbol
}

// ticker returns its latest symbol that is not a CUSIP, "" for none.
func (s *toolSecurity) ticker() string {
	if len(s.tickers) == 0 {
		return ""
	}
	return s.tickers[0]
}

// ppTicker returns the Ticker Symbol that PP gets for it: an option's OCC
// compact form, so that a live contract can take Yahoo's quotes; none for
// a delisted security, whose ticker another company may have taken by now,
// nor for one that keeps a "." in Yahoo's form, which PP would take for the
// ticker before the dot (PSA.PRK for PSA); else its ticker in Yahoo's form.
func (s *toolSecurity) ppTicker() string {
	if s.occ != "" {
		return s.occ
	}
	if s.delisted {
		return ""
	}
	t := yahooForm(s.ticker())
	if strings.Contains(t, ".") {
		return ""
	}
	return t
}

// ppManual reports whether PP gets snapshot prices for it, having no feed
// that serves it: an option, a bond, or one without a PP ticker (none, one
// delisted, or one with a dot).
func (s *toolSecurity) ppManual() bool {
	return s.assetType == "OPTION" || s.bond || s.ppTicker() == ""
}

// gfDataSource returns the data source that Ghostfolio gets for it.
func (s *toolSecurity) gfDataSource() string {
	if s.manual != "" {
		return "MANUAL"
	}
	return "YAHOO"
}

// gfSymbol returns its symbol in Ghostfolio: for a MANUAL asset, the
// name-based UUID of its identity (uuidName); else its ticker in Yahoo's
// form, which Ghostfolio asks Yahoo about.
func (s *toolSecurity) gfSymbol() string {
	if s.manual != "" {
		return uuid8(toolNamespace, s.uuidName())
	}
	return yahooForm(s.ticker())
}

// identity returns what identifies the security whatever store keys it
// has: its ticker in Yahoo's form, which Ghostfolio knows a security by
// that it asks Yahoo about, and which the store's securities that the tools
// take for one (a History file's key and the API's, with a CUSIP under one
// of them only) have had alike; but for an option, a future, a delisted
// security, a bond whose CUSIP the store knows, and a ticker that another
// security has had too, of another CUSIP or at another time, which have
// none (identityTicker); else its CUSIP, else its OCC compact form, else
// the key, else the symbol, of the first of them.
func (s *toolSecurity) identity() string {
	r := s.first()
	return cmp.Or(s.identityTicker(), s.cusip, s.occ, r.key, r.symbol)
}

// identityTicker returns the ticker in Yahoo's form that is the security's
// identity, "" when it has none: it is an option or a future, which its
// OCC form or its key names; it is delisted, its ticker another company's
// now perhaps; it is a bond whose CUSIP the store knows; its ticker is one
// that another security has had too, of another CUSIP or at another time,
// which two companies cannot share as one identity; or it has no ticker.
func (s *toolSecurity) identityTicker() string {
	if s.occ != "" || s.assetType == "OPTION" || s.assetType == "FUTURE" || s.delisted || s.bond && s.cusip != "" || s.reused {
		return ""
	}
	return yahooForm(s.ticker())
}

// first returns the first of the store's securities that the tools take for
// one with s, whose key or symbol names them when nothing else does: s
// itself when it is alone.
func (s *toolSecurity) first() *toolSecurity {
	if s.rep != nil {
		return s.rep
	}
	return s
}

// uuidName returns the name whose UUID is its Ghostfolio symbol as a
// MANUAL asset (6.7 of the design), made from its identity.
func (s *toolSecurity) uuidName() string {
	r := s.first()
	switch {
	case s.identityTicker() != "":
		return "security ticker:" + s.identityTicker()
	case s.cusip != "":
		return "security cusip:" + s.cusip
	case s.occ != "":
		return "security option:" + s.occ
	case r.key != "":
		return "security key:" + r.key
	}
	return "security symbol:" + r.symbol
}

// gfName returns the name of its MANUAL asset in Ghostfolio (6.4 of the
// design), made from its identity alone, so that every export finds the
// asset again by its name: an option's root, expiration, strike and side,
// read from its OCC form; Bond; or Security; and the identity in
// parentheses: "XYZ 2025-02-21 40 put (XYZ250221P00040000)". The
// description, which changes with the source that saw the security last,
// goes in the asset's comment.
func (s *toolSecurity) gfName() string {
	what := "Security"
	switch {
	case s.occ != "":
		strike := new(big.Rat).SetFrac64(s.contract.StrikeThousandths, 1000)
		what = fmt.Sprintf("%s %s %s %s", s.contract.Root, s.contract.Expiration, roundRat(strike, 3), strings.ToLower(s.contract.PutCall))
	case s.bond:
		what = "Bond"
	}
	return what + " (" + s.identity() + ")"
}

// symbolsChanged reports whether a tool that holds the security under an
// earlier symbol needs telling of a new one: it is not an option, and it
// has had two tickers that differ in Yahoo's form, or it is delisted.
func (s *toolSecurity) symbolsChanged() bool {
	if s.assetType == "OPTION" {
		return false
	}
	forms := map[string]bool{}
	for _, t := range s.tickers {
		forms[yahooForm(t)] = true
	}
	return len(forms) > 1 || s.delisted
}

// plainYahoo is a ticker, in Yahoo's form, that Ghostfolio asks Yahoo
// about: up to six letters and digits, and a class after a dash.
var plainYahoo = regexp.MustCompile(`^[A-Z0-9]{1,6}(-[A-Z]{1,2})?$`)

// A toolPrice is a security's price on a market date, in dollars per share
// (per dollar of face for a bond), from the snapshots (5.5 of the design):
// its market value over its shares, rounded to 8 places.
type toolPrice struct {
	date  nytime.Date
	price *big.Rat
}

// A toolBalance is an account's cash on a market date, in cents, signed:
// the latest snapshot of the day's (6.3 of the design), taken at
// fetchedAt.
type toolBalance struct {
	date      nytime.Date
	cash      int64
	fetchedAt time.Time
}

// A toolIndex is a toolInput indexed once for the functions of this file,
// with the securities and prices that they work out, each worked out once.
type toolIndex struct {
	in       *toolInput
	accounts map[int64]toolAccount
	secRows  map[string]*toolSecurityRow
	names    map[string][]toolName
	legs     map[rowKey][]toolLeg
	manual   map[string]bool
	secs     map[secRef]*toolSecurity
	prices   map[string][]toolPrice
	// bases are the securities as their own symbols and names make them
	// (base); members, once groupAll has run, the bases of each security's
	// group, by its ref; and reused the tickers (symbolForm) that securities
	// of two groups have had.
	bases   map[secRef]*toolSecurity
	members map[secRef][]*toolSecurity
	reused  map[string]bool
	// unsure are, by the ref of each of their securities, the groups that
	// groupAll cannot be sure of (placeAlone).
	unsure map[secRef]*toolUnsure
	// groups are the securities that each group is written as (group), and
	// groupPrices its prices (pricesOfGroup), by the ref of its first.
	groups      map[secRef]*toolSecurity
	groupPrices map[secRef][]toolPrice
	// days are when the store's rows had each symbol of each key (Q12), by
	// key, "" for the rows of a symbol without one, and symbol; keyDays
	// each key's, by key.
	days    map[[2]string]toolSymbolDays
	keyDays map[string][]toolSymbolDays
}

// newToolIndex indexes in.
func newToolIndex(in *toolInput) *toolIndex {
	ix := &toolIndex{
		in: in, accounts: map[int64]toolAccount{}, secRows: map[string]*toolSecurityRow{}, names: map[string][]toolName{},
		legs: map[rowKey][]toolLeg{}, manual: map[string]bool{}, secs: map[secRef]*toolSecurity{}, prices: map[string][]toolPrice{},
		bases: map[secRef]*toolSecurity{}, groups: map[secRef]*toolSecurity{}, groupPrices: map[secRef][]toolPrice{},
		days: map[[2]string]toolSymbolDays{}, keyDays: map[string][]toolSymbolDays{},
	}
	for _, d := range in.days {
		ix.days[[2]string{d.key, d.symbol}] = d
		ix.keyDays[d.key] = append(ix.keyDays[d.key], d)
	}
	for _, a := range in.accounts {
		ix.accounts[a.id] = a
	}
	for i := range in.securities {
		ix.secRows[in.securities[i].key] = &in.securities[i]
	}
	for _, n := range in.names {
		ix.names[n.key] = append(ix.names[n.key], n)
	}
	for _, l := range in.legs {
		k := rowKey{l.account, l.ref}
		ix.legs[k] = append(ix.legs[k], l)
	}
	for _, ls := range ix.legs {
		slices.SortFunc(ls, func(a, b toolLeg) int { return cmp.Compare(a.leg, b.leg) })
	}
	for _, m := range in.manual {
		if m = symbolForm(m); m != "" {
			ix.manual[m] = true
		}
	}
	return ix
}

// symbolForm returns symbol as the export compares symbols, -manual's with
// a security's (6.6 of the design) and the tickers of two securities
// (groupAll): without spaces at its ends, in upper case, in Yahoo's form,
// so that BRK.B, BRK/B and brk-b name one class share, however Schwab, a
// file or you spell it.
func symbolForm(symbol string) string { return yahooForm(strings.ToUpper(strings.TrimSpace(symbol))) }

// A symbolSeen is a symbol of a security as symbolOrder takes it: when the
// store saw it last (Q5), the zero time for one that it holds no name of,
// and the last day of the rows and legs that named the security by it
// (Q12), "" for none.
type symbolSeen struct {
	value    string
	lastSeen time.Time
	lastRow  nytime.Date
}

// symbolOrder orders a security's symbols as the export takes them, the
// latest first (3.5 of the design; Q5's order): by when each was seen last;
// of one instant, by the last day of the rows under each, since the store
// dates every name of one API read, one transactions -json import or one
// History file by that read, import or file's export, which may hold years
// of rows: a cash merger's or a delisting's last row, under the CUSIP,
// makes the security delisted, and a spin-off's purchases, under its
// ticker after its first row under its CUSIP, keep it trading; of one day
// too, or with no row, a ticker before a CUSIP, the symbol that the
// security trades under; then by value.
func symbolOrder(a, b symbolSeen) int {
	cusip := func(n symbolSeen) int {
		if schwab.IsCUSIP(n.value) {
			return 1
		}
		return 0
	}
	return cmp.Or(b.lastSeen.Compare(a.lastSeen), cmp.Compare(b.lastRow, a.lastRow), cmp.Compare(cusip(a), cusip(b)), cmp.Compare(a.value, b.value))
}

// A rowKey names a ledger row: its account's id and its ref.
type rowKey struct {
	account int64
	ref     string
}

// legsOf returns the security legs of row r, in the order of their index.
func (ix *toolIndex) legsOf(r *toolRow) []toolLeg { return ix.legs[rowKey{r.account, r.ref}] }

// account returns the account of id.
func (ix *toolIndex) account(id int64) toolAccount {
	if a, ok := ix.accounts[id]; ok {
		return a
	}
	return toolAccount{id: id}
}

// toolSecurityOf returns the security of key, or, when key is "", the one
// that symbol names alone; nil when both are "", which is no security.
func toolSecurityOf(in *toolInput, key, symbol string) *toolSecurity {
	return newToolIndex(in).security(key, symbol)
}

// security returns the security of key, or of symbol alone when key is ""
// (3.5 of the design); nil for neither: the security that the store's
// securities that the tools take for one with it are written as (group),
// under its own key or symbol, so that the records of each of them name the
// tools' one security alike, in one unit.
func (ix *toolIndex) security(key, symbol string) *toolSecurity {
	if key == "" && symbol == "" {
		return nil
	}
	id := secRef{key: key}
	if key == "" {
		id = secRef{symbol: symbol}
	}
	if s, ok := ix.secs[id]; ok {
		return s
	}
	b := ix.base(key, symbol)
	s := new(toolSecurity)
	*s = *ix.group(b)
	s.key, s.symbol = b.key, b.symbol
	ix.secs[id] = s
	return s
}

// group returns, once for all of them, the security that b and the store's
// securities that the tools take for one with it (membersOf) are written
// as: what they have had together (join), why Ghostfolio gets it as a MANUAL
// asset, and, for a bond, whether its unit is off, by the rows and prices
// of every one of them (4.4 of the design), so that no tool keeps a holding
// that the rows of one key opened and those of another, left out, would
// close. It is under the key or symbol of the first of them.
func (ix *toolIndex) group(b *toolSecurity) *toolSecurity {
	members := ix.membersOf(b)
	if g, ok := ix.groups[members[0].ref()]; ok {
		return g
	}
	g := ix.join(members)
	g.manual = ix.gfManual(g)
	g.bondUnit = g.bond && !ix.followsFace(g)
	ix.groups[members[0].ref()] = g
	return g
}

// membersOf returns the store's securities that the tools take for one with
// s (groupAll), as their own symbols and names make them (base), in the
// order of their keys: s's alone when no row, leg, position or name of the
// store names it.
func (ix *toolIndex) membersOf(s *toolSecurity) []*toolSecurity {
	ix.groupAll()
	if members := ix.members[s.ref()]; len(members) > 0 {
		return members
	}
	return []*toolSecurity{ix.base(s.key, s.symbol)}
}

// base returns the security of key, or of symbol alone when key is "", as
// its own symbols and names (Q4, Q5) make it, once: symbol stands for its
// symbols when the store has none of a key's.
func (ix *toolIndex) base(key, symbol string) *toolSecurity {
	id := secRef{key: key}
	if key == "" {
		id = secRef{symbol: symbol}
	}
	if s, ok := ix.bases[id]; ok {
		return s
	}
	s := &toolSecurity{key: key, multiplier: 1}
	var row toolSecurityRow
	if key != "" {
		if r := ix.secRows[key]; r != nil {
			row = *r
		}
		names := ix.names[key]
		var symbols []symbolSeen
		var first *toolName
		for i, n := range names {
			switch n.kind {
			case "symbol":
				symbols = append(symbols, symbolSeen{value: n.value, lastSeen: n.lastSeen, lastRow: ix.days[[2]string{key, n.value}].last})
			case "description":
				if first == nil || n.firstSeen.Before(first.firstSeen) || n.firstSeen.Equal(first.firstSeen) && n.value < first.value {
					first = &names[i]
				}
			}
		}
		slices.SortFunc(symbols, symbolOrder)
		for _, n := range symbols {
			if !slices.Contains(s.symbols, n.value) {
				s.symbols = append(s.symbols, n.value)
			}
		}
		if first != nil {
			s.name, s.described, s.nameSeen = first.value, true, first.firstSeen
		}
		s.assetType = row.assetType
		s.symbol = row.symbol
	} else {
		s.symbol = symbol
	}
	if len(s.symbols) == 0 && symbol != "" {
		s.symbols = []string{symbol}
	}
	if s.symbol == "" && len(s.symbols) > 0 {
		s.symbol = s.symbols[0]
	}
	if _, ok := contract(symbol); key == "" && ok {
		s.assetType = "OPTION"
	}
	allCUSIPs := len(s.symbols) > 0
	for _, sym := range s.symbols {
		if schwab.IsCUSIP(sym) {
			if s.cusip == "" {
				s.cusip = sym
			}
		} else {
			allCUSIPs = false
			s.tickers = append(s.tickers, sym)
		}
	}
	if schwab.IsCUSIP(row.cusip) {
		s.cusip = row.cusip
	}
	s.delisted = len(s.symbols) > 0 && schwab.IsCUSIP(s.symbols[0]) && len(s.tickers) > 0
	s.bond = s.assetType == "FIXED_INCOME" || s.assetType == "" && allCUSIPs
	if s.name == "" && len(s.tickers) == 0 && s.cusip == "" {
		s.name = s.symbol
	}
	if s.assetType == "OPTION" {
		s.contract, s.occ = optionContract(s.symbols)
	}
	ix.bases[id] = s
	return s
}

// optionContract returns the option contract that symbols name, and its OCC
// compact form (3.5 of the design): the first of them, sorted, that parses
// as a contract with one; none for a futures option.
func optionContract(symbols []string) (schwab.OCC, string) {
	for _, sym := range slices.Sorted(slices.Values(symbols)) {
		if c, ok := contract(sym); ok {
			if occ := occCompact(c); occ != "" {
				return c, occ
			}
		}
	}
	return schwab.OCC{}, ""
}

// join returns the security that members, the store's securities that the
// tools take for one (membersOf), are written as, under the first one's key
// or symbol: what they have had together, so that each of them is written
// alike: their symbols and tickers, in the order of one security's, each by
// when any of them saw it last and by the days of their rows under it
// (symbolsOf), so that the latest ticker is the one that any of them saw
// last; their CUSIP and asset type; delisted when the latest of those
// symbols is a CUSIP, as of one security, never because one of them alone
// reads so; a bond by those; the description that the store saw first of
// any of them; for an option, one contract and one multiplier, which a
// futures option that a History file names has none of (multiplier); and
// whether its ticker is one that a security of another group has had too.
func (ix *toolIndex) join(members []*toolSecurity) *toolSecurity {
	s := new(toolSecurity)
	*s = *members[0]
	s.rep = members[0]
	if len(members) > 1 {
		s.symbols, s.tickers, s.cusip, s.assetType = nil, nil, "", ""
		var named *toolSecurity
		for _, m := range members {
			s.cusip, s.assetType = cmp.Or(s.cusip, m.cusip), cmp.Or(s.assetType, m.assetType)
			if m.described && (named == nil || m.nameSeen.Before(named.nameSeen) || m.nameSeen.Equal(named.nameSeen) && m.name < named.name) {
				named = m
			}
		}
		for _, n := range ix.symbolsOf(members) {
			s.symbols = append(s.symbols, n.value)
			if !schwab.IsCUSIP(n.value) {
				s.tickers = append(s.tickers, n.value)
			}
		}
		s.delisted = len(s.symbols) > 0 && schwab.IsCUSIP(s.symbols[0]) && len(s.tickers) > 0
		s.bond = s.assetType == "FIXED_INCOME" || s.assetType == "" && len(s.tickers) == 0
		// Every security of a group has a ticker or a CUSIP, which it joins
		// by, so none is named by its symbol for want of both.
		s.name, s.described = "", false
		if named != nil {
			s.name, s.described, s.nameSeen = named.name, true, named.nameSeen
		}
	}
	s.contract, s.occ = schwab.OCC{}, ""
	s.multiplier, s.assumed, s.badMultiplier = 1, false, false
	if s.assetType == "OPTION" {
		s.contract, s.occ = optionContract(s.symbols)
		s.multiplier, s.assumed, s.badMultiplier = ix.multiplier(members)
	}
	if t := s.ticker(); t != "" {
		s.reused = ix.reused[symbolForm(t)]
	}
	return s
}

// symbolsOf returns the symbols that members, the store's securities that
// the tools take for one, have had, in the order of one security's
// (symbolOrder): each by when any of them saw it last, and by the last day
// of the rows and legs of any of them under it; a symbol that none has a
// name of stands, unseen, after those seen.
func (ix *toolIndex) symbolsOf(members []*toolSecurity) []symbolSeen {
	last := map[string]time.Time{}
	for _, m := range members {
		seen := false
		if m.key != "" {
			for _, n := range ix.names[m.key] {
				if n.kind == "symbol" {
					seen = true
					if t, ok := last[n.value]; !ok || n.lastSeen.After(t) {
						last[n.value] = n.lastSeen
					}
				}
			}
		}
		for _, sym := range m.symbols {
			if _, ok := last[sym]; !ok && !seen {
				last[sym] = time.Time{}
			}
		}
	}
	symbols := make([]symbolSeen, 0, len(last))
	for v, t := range last {
		n := symbolSeen{value: v, lastSeen: t}
		for _, m := range members {
			n.lastRow = max(n.lastRow, ix.rowDays(m, v).last)
		}
		symbols = append(symbols, n)
	}
	slices.SortFunc(symbols, symbolOrder)
	return symbols
}

// rowDays returns when the store's rows and legs named the security m by
// symbol (Q12), the zero value for never: a row's security without a key,
// by the rows of its symbol without one.
func (ix *toolIndex) rowDays(m *toolSecurity, symbol string) toolSymbolDays {
	if m.key == "" && symbol != m.symbol {
		return toolSymbolDays{}
	}
	return ix.days[[2]string{m.key, symbol}]
}

// since returns when members, the store's securities that the tools take
// for one, first had a symbol that is: the first day of their rows and legs
// under one (Q12), or the New York date when the store first saw one (Q5),
// whichever is earlier, "" for never; and that first instant that the store
// saw one, the zero time for none. A name's instant is its source's, which
// a late History file or -json import makes late, and the rows' days say
// when the security went by it.
func (ix *toolIndex) since(members []*toolSecurity, is func(symbol string) bool) (nytime.Date, time.Time) {
	var day nytime.Date
	var at time.Time
	earlier := func(d nytime.Date) {
		if d != "" && (day == "" || d < day) {
			day = d
		}
	}
	for _, m := range members {
		if m.key == "" {
			if is(m.symbol) {
				earlier(ix.rowDays(m, m.symbol).first)
			}
			continue
		}
		for _, d := range ix.keyDays[m.key] {
			if is(d.symbol) {
				earlier(d.first)
			}
		}
		for _, n := range ix.names[m.key] {
			if n.kind == "symbol" && is(n.value) {
				earlier(nytime.DateOf(n.firstSeen))
				if at.IsZero() || n.firstSeen.Before(at) {
					at = n.firstSeen
				}
			}
		}
	}
	return day, at
}

// formOf returns a test of a symbol: whether it is the ticker f, as the
// export compares tickers (symbolForm).
func formOf(f string) func(string) bool {
	return func(symbol string) bool { return symbolForm(symbol) == f }
}

// gaveUp reports whether members, a group of the store's securities that a
// CUSIP or an OCC form joins, gave the ticker f (symbolForm) up before had,
// the day that another security first had it: their latest symbol
// (symbolsOf) is no longer f, as of a security renamed or delisted, and
// they had that symbol (since) before had.
func (ix *toolIndex) gaveUp(members []*toolSecurity, f string, had nytime.Date) bool {
	symbols := ix.symbolsOf(members)
	if had == "" || len(symbols) == 0 || symbolForm(symbols[0].value) == f {
		return false
	}
	latest := symbols[0].value
	day, _ := ix.since(members, func(symbol string) bool { return symbol == latest })
	return day != "" && day < had
}

// apart reports whether a and b, groups of the store's securities that a
// CUSIP or an OCC form joins, had the ticker f (symbolForm) at times that
// do not meet: one of them gave it up (gaveUp) before the other first had
// it, as when a company takes a ticker that another gave up when it was
// renamed, delisted or merged away. The days of their rows decide, which
// do not depend on when the store read them.
func (ix *toolIndex) apart(a, b []*toolSecurity, f string) bool {
	da, _ := ix.since(a, formOf(f))
	db, _ := ix.since(b, formOf(f))
	return ix.gaveUp(a, f, db) || ix.gaveUp(b, f, da)
}

// earlierSince reports whether a security that first had a ticker on day,
// first seen with it at at (since), had it before one that did on other, at
// otherAt: by the day, a day before none; of one day, by the instant, an
// instant before none.
func earlierSince(day nytime.Date, at time.Time, other nytime.Date, otherAt time.Time) bool {
	switch {
	case day != other:
		return day != "" && (other == "" || day < other)
	case !at.IsZero() && !otherAt.IsZero():
		return at.Before(otherAt)
	}
	return !at.IsZero() && otherAt.IsZero()
}

// multiplier returns the multiplier of an option that the store holds as
// members (4.1 of the design): the one that those of them that the store
// knows one of know, when it is a whole number from 1 to 10 000, else bad;
// 100, assumed, when none knows one, as of a History file's option alone,
// or of a row's contract without a key. Of a futures option that a History
// file's key and the API's name, the API's is the one, which the file's,
// whose symbol no contract parses, would take for 1, and an option of a
// file 100: so each of its rows, of either key, counts its contracts alike.
// Two that know different ones are bad, which leaves the option's rows out
// (multiplier): no one unit is known for them.
func (ix *toolIndex) multiplier(members []*toolSecurity) (m int64, assumed, bad bool) {
	var known []float64
	for _, s := range members {
		if r := ix.secRows[s.key]; s.key != "" && r != nil && r.multiplier.Valid && !slices.Contains(known, r.multiplier.Float64) {
			known = append(known, r.multiplier.Float64)
		}
	}
	switch {
	case len(known) == 0:
		return 100, true, false
	case len(known) == 1 && known[0] == math.Trunc(known[0]) && known[0] >= 1 && known[0] <= 10_000:
		return int64(known[0]), false, false
	}
	return 1, false, true
}

// groupAll works out, once, which of the store's securities the tools take
// for one, where the store holds them apart (3.5 of the design): the store
// joins two keys only when they had one symbol at times that overlap, so a
// security that a History file's rows name, under an ItemIssueId or a
// symbol, and the API's rows, under an instrumentId, often stays two, a
// CUSIP under one of them only; and when it is delisted or renamed, only
// the API's key learns it. The groups are those that share a CUSIP, the
// options of one OCC form, and those that have had a ticker alike
// (symbolForm), or an option's or a future's symbol, when those that have
// had it know at most one CUSIP between them, and had it at times that
// meet: a ticker that two securities of different CUSIPs have had, as when
// a company takes a ticker that another gave up, joins none of them, and is
// reused; and of those that know one CUSIP or none, one that had it after
// another gave it up (apart), as such a company known by a History file's
// key or by an API key that no snapshot held, joins those that had it with
// it, apart from the other, and the ticker is reused too. A group that its
// tickers would give two CUSIPs, through a security that had a ticker of
// each, is joined by its CUSIPs and OCC forms alone. Then a group with a key
// that knows no CUSIP, as a History file's key often does, and that those
// joins leave tied by its tickers to securities of other CUSIPs, or apart
// from it, is placed among them (placeAlone): it is taken for the one that
// had its ticker first, or stays on its own when its tickers are each of
// another, or each had it at another time. Every security of the store is
// grouped, whatever the export selects, and those of rows and legs that
// have a symbol but no key, so that every export groups alike; the first of
// a group, in the order of the keys (keyRank), then of the symbols of those
// without, names it.
func (ix *toolIndex) groupAll() {
	if ix.members != nil {
		return
	}
	ix.members, ix.reused, ix.unsure = map[secRef][]*toolSecurity{}, map[string]bool{}, map[secRef]*toolUnsure{}
	var all []*toolSecurity
	seen := map[secRef]bool{}
	add := func(key, symbol string) {
		id := secRef{key: key}
		if key == "" {
			id = secRef{symbol: symbol}
		}
		if id == (secRef{}) || seen[id] {
			return
		}
		seen[id] = true
		all = append(all, ix.base(key, symbol))
	}
	// A row's or a leg's symbol stands for those of a key that the store
	// has none of, as when security is first asked for it.
	for i := range ix.in.rows {
		add(ix.in.rows[i].securityKey, ix.in.rows[i].symbol)
	}
	for _, l := range ix.in.legs {
		add(l.securityKey, l.symbol)
	}
	for _, r := range ix.in.securities {
		add(r.key, "")
	}
	for _, n := range ix.in.names {
		add(n.key, "")
	}
	for _, p := range ix.in.positions {
		add(p.key, "")
	}
	slices.SortFunc(all, func(a, b *toolSecurity) int {
		switch {
		case a.key != "" && b.key != "":
			return keyRank(a.key, b.key)
		case a.key != "" || b.key != "":
			// A key before a symbol alone.
			return strings.Compare(b.key, a.key)
		}
		return strings.Compare(a.symbol, b.symbol)
	})
	parent := make([]int, len(all))
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i], i = parent[parent[i]], parent[i]
		}
		return i
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		parent[max(ra, rb)] = min(ra, rb)
	}
	cusips := func(is []int) int {
		set := map[string]bool{}
		for _, i := range is {
			if c := all[i].cusip; c != "" {
				set[c] = true
			}
		}
		return len(set)
	}
	// By CUSIP and by OCC form, which name one security each.
	first := map[string]int{}
	for i, s := range all {
		for _, id := range []string{"cusip " + s.cusip, "option " + s.occ} {
			if strings.HasSuffix(id, " ") {
				continue
			}
			if j, ok := first[id]; ok {
				union(i, j)
			} else {
				first[id] = i
			}
		}
	}
	alone := slices.Clone(parent)
	// The groups that those joins make, which the joins by ticker compare.
	staged := map[int][]*toolSecurity{}
	stage := make([]int, len(all))
	for i, s := range all {
		stage[i] = find(i)
		staged[stage[i]] = append(staged[stage[i]], s)
	}
	// By ticker: an option's or a future's symbol, which no stock has, joins
	// the keys of one contract that has no OCC form, as a futures option.
	holders := map[string][]int{}
	for i, s := range all {
		for _, t := range s.tickers {
			if f := symbolForm(t); !slices.Contains(holders[f], i) {
				holders[f] = append(holders[f], i)
			}
		}
	}
	for _, f := range slices.Sorted(maps.Keys(holders)) {
		hs := holders[f]
		if cusips(hs) > 1 {
			continue
		}
		// The groups that had it, in the order that they first had it, each
		// joined to the last of those before it that it is apart from none
		// of, else on to a group of its own.
		var roots []int
		for _, i := range hs {
			if !slices.Contains(roots, stage[i]) {
				roots = append(roots, stage[i])
			}
		}
		type had struct {
			day nytime.Date
			at  time.Time
		}
		first := map[int]had{}
		for _, r := range roots {
			day, at := ix.since(staged[r], formOf(f))
			first[r] = had{day, at}
		}
		slices.SortStableFunc(roots, func(a, b int) int {
			switch {
			case earlierSince(first[a].day, first[a].at, first[b].day, first[b].at):
				return -1
			case earlierSince(first[b].day, first[b].at, first[a].day, first[a].at):
				return 1
			}
			return 0
		})
		var joined [][]int
		for _, r := range roots {
			k := len(joined) - 1
			for ; k >= 0; k-- {
				if !slices.ContainsFunc(joined[k], func(c int) bool { return ix.apart(staged[c], staged[r], f) }) {
					break
				}
			}
			if k < 0 {
				joined = append(joined, []int{r})
				continue
			}
			joined[k] = append(joined[k], r)
			union(joined[k][0], r)
		}
	}
	groups := map[int][]int{}
	for i := range all {
		groups[find(i)] = append(groups[find(i)], i)
	}
	for _, is := range groups {
		if cusips(is) > 1 {
			for _, i := range is {
				parent[i] = alone[i]
			}
		}
	}
	for _, j := range ix.placeAlone(all, find, holders, cusips) {
		union(j[0], j[1])
	}
	members := map[int][]*toolSecurity{}
	for i, s := range all {
		members[find(i)] = append(members[find(i)], s)
	}
	for i, s := range all {
		ix.members[s.ref()] = members[find(i)]
	}
	for f, hs := range holders {
		roots := map[int]bool{}
		for _, i := range hs {
			roots[find(i)] = true
		}
		ix.reused[f] = len(roots) > 1
	}
}

// A toolUnsure is a group of the store's securities that knows no CUSIP and
// whose tickers other securities have had too, of other CUSIPs, or apart
// from it, so that the export cannot be sure which of them it is
// (placeAlone): it takes it for the one that had its ticker first, or
// leaves it on its own, and lists it for you to say which it is in the
// store (keysUnsure).
type toolUnsure struct {
	// keys are the keys of the group, in the order of keys (keyRank), and
	// tickers those of its tickers (symbolForm) that the others have had.
	keys, tickers []string
	// of are the securities that it may be, each by the first of its keys
	// and its CUSIP, in that order; taken is the one of them that it is
	// written as, -1 for none.
	of    []toolUnsureOf
	taken int
}

// A toolUnsureOf is a security that a toolUnsure may be: the first of its
// keys, "" for none, its CUSIP, "" for none, and whether the two had their
// ticker at times that do not meet (apart), which the export takes for two
// securities.
type toolUnsureOf struct {
	key, cusip string
	apart      bool
}

// placeAlone returns the joins that groupAll makes, after those by CUSIP,
// OCC form and ticker, of the groups of all (find) that have a key, know no
// CUSIP and had a ticker that other securities have had too (3.5 of the
// design): each as the roots of the two groups that it joins. Of a CUSIP,
// another counts when the two had the ticker at times that meet (apart);
// one that gave it up before the group had it is apart, and so is one of no
// CUSIP that did, so that two companies of one ticker are not one.
//   - One that a ticker of it ties to one security of a CUSIP alone joins
//     it: the joins by ticker made that join, and undid it for another key
//     of their group, which had tickers of two CUSIPs.
//   - One whose tickers securities of two or more CUSIPs have had alike,
//     which the joins by ticker leave on its own, as when the store holds a
//     reorganized security's new CUSIP or another company under the ticker
//     of a History file's key, is taken for the one of them that had one of
//     its tickers first, by the days of their rows under it (a late import
//     of old rows dates its names late), else by when the store first saw
//     each; when none of them says, the first of them.
//   - One whose tickers are each of another stays on its own: its rows may
//     be of each.
//   - One that the others had the ticker apart from, alone or beside those
//     above, is taken for the one of the others that had it while it did,
//     as above, or stays on its own when none did: its rows may still be of
//     one of them.
//
// The export cannot be sure of the last three, which a tool would otherwise
// split, or take for whichever its ticker finds: each of their securities
// is recorded in ix.unsure.
func (ix *toolIndex) placeAlone(all []*toolSecurity, find func(int) int, holders map[string][]int, cusips func([]int) int) [][2]int {
	groups := map[int][]int{}
	for i := range all {
		groups[find(i)] = append(groups[find(i)], i)
	}
	members := func(c int) []*toolSecurity {
		var out []*toolSecurity
		for _, i := range groups[c] {
			out = append(out, all[i])
		}
		return out
	}
	var joins [][2]int
	for _, r := range slices.Sorted(maps.Keys(groups)) {
		is := groups[r]
		if cusips(is) > 0 || !slices.ContainsFunc(is, func(i int) bool { return all[i].key != "" }) {
			continue
		}
		// The groups that had each of its tickers: of a CUSIP, apart from it
		// or not (live), and of none, apart when they gave it up before it
		// had it; and those live that alone of such groups had one.
		mine := members(r)
		ties, live, apart, only := map[int][]string{}, map[int]bool{}, map[int]bool{}, map[int]bool{}
		var forms []string
		for _, i := range is {
			for _, t := range all[i].tickers {
				f := symbolForm(t)
				if slices.Contains(forms, f) {
					continue
				}
				forms = append(forms, f)
				had, _ := ix.since(mine, formOf(f))
				var held []int
				for _, h := range holders[f] {
					c := find(h)
					if c == r || slices.Contains(ties[c], f) {
						continue
					}
					switch theirs := members(c); {
					case cusips(groups[c]) > 0 && !ix.apart(mine, theirs, f):
						live[c] = true
						held = append(held, c)
					case cusips(groups[c]) > 0, ix.gaveUp(theirs, f, had):
						apart[c] = true
					default:
						continue
					}
					ties[c] = append(ties[c], f)
				}
				if len(held) == 1 {
					only[held[0]] = true
				}
			}
		}
		if len(ties) == 0 {
			continue
		}
		if len(apart) == 0 && len(only) == 1 {
			for c := range only {
				joins = append(joins, [2]int{r, c})
			}
			continue
		}
		u := &toolUnsure{taken: -1}
		for _, i := range is {
			if all[i].key != "" {
				u.keys = append(u.keys, all[i].key)
			}
		}
		of := slices.Sorted(maps.Keys(ties))
		var day nytime.Date
		var at time.Time
		for k, c := range of {
			o := toolUnsureOf{key: all[c].key, apart: apart[c]}
			for _, i := range groups[c] {
				o.cusip = cmp.Or(o.cusip, all[i].cusip)
			}
			u.of = append(u.of, o)
			for _, f := range ties[c] {
				if !slices.Contains(u.tickers, f) {
					u.tickers = append(u.tickers, f)
				}
				switch {
				case !live[c] || len(only) > 1:
				case len(only) == 1:
					if only[c] {
						u.taken = k
					}
				default:
					if d, a := ix.since(members(c), formOf(f)); u.taken < 0 || earlierSince(d, a, day, at) {
						u.taken, day, at = k, d, a
					}
				}
			}
		}
		if u.taken >= 0 {
			joins = append(joins, [2]int{r, of[u.taken]})
		}
		slices.Sort(u.tickers)
		for _, i := range is {
			ix.unsure[all[i].ref()] = u
		}
	}
	return joins
}

// keysUnsure returns, in the order of the first of their keys, the groups
// that the export of in writes or prices (listSecurities) and cannot be
// sure of (placeAlone): it takes each for the security of another CUSIP
// that had its ticker first, or while it did, or leaves it on its own, and
// you say which it is in the store.
func keysUnsure(in *toolInput, events []toolEvent) []*toolUnsure {
	ix := newToolIndex(in)
	ix.groupAll()
	var out []*toolUnsure
	for _, l := range listSecurities(in, events) {
		if u := ix.unsure[l.security.ref()]; u != nil && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b *toolUnsure) int { return keyRank(a.keys[0], b.keys[0]) })
	return out
}

// gfManual returns why Ghostfolio gets s as a MANUAL asset, "" when it
// asks Yahoo (3.5 and 6.6 of the design).
func (ix *toolIndex) gfManual(s *toolSecurity) string {
	switch {
	case s.assetType == "OPTION":
		return "option"
	case s.bond:
		return "bond"
	case s.ticker() == "":
		return "no_ticker"
	case s.delisted:
		return "delisted"
	case !plainYahoo.MatchString(yahooForm(s.ticker())):
		return "not_plain"
	}
	for _, sym := range s.symbols {
		if ix.manual[symbolForm(sym)] {
			return "asked"
		}
	}
	return ""
}

// followsFace reports whether bond s follows face value and prices per 100
// in every row and price that the export selects (4.4 of the design), of
// any of the store's securities that the tools take for one with it
// (membersOf), whose rows the tools hold in one holding: each trade with a
// price and a gross, or leg of a trade of several legs with a price and a
// cost, costs 0.8 to 1.25 times its face at that price, and each snapshot
// price (pricesOfGroup) is 0.2 to 5 per dollar of face. Accrued interest
// and fees ride in a trade's cash; a unit 100 times off cannot pass. A
// journal moves no cash, so its legs say nothing of the unit.
func (ix *toolIndex) followsFace(s *toolSecurity) bool {
	members := ix.membersOf(s)
	is := func(key, symbol string) bool {
		return slices.ContainsFunc(members, func(m *toolSecurity) bool { return m.is(key, symbol) })
	}
	low, high := big.NewRat(4, 5), big.NewRat(5, 4)
	// costs reports whether a trade of quantity q, in millionths, at price
	// per 100 of face, for gross cents, passes.
	costs := func(q, gross sql.NullInt64, price sql.NullFloat64) bool {
		if !q.Valid || q.Int64 == 0 || !gross.Valid || !price.Valid || price.Float64 <= 0 {
			return true
		}
		p := decimalRat(price.Float64)
		if p == nil {
			return true
		}
		// |gross| / 100 dollars, over |q| / 10^6 of face at p per 100.
		face := new(big.Rat).Mul(new(big.Rat).SetInt64(max(q.Int64, -q.Int64)), p)
		ratio := new(big.Rat).Quo(new(big.Rat).SetInt64(max(gross.Int64, -gross.Int64)*1_000_000), face)
		return ratio.Cmp(low) >= 0 && ratio.Cmp(high) <= 0
	}
	trades := map[rowKey]bool{}
	for i := range ix.in.rows {
		r := &ix.in.rows[i]
		if r.category != "buy" && r.category != "sell" {
			continue
		}
		trades[rowKey{r.account, r.ref}] = true
		if is(r.securityKey, r.symbol) && !costs(r.quantity, r.gross, r.price) {
			return false
		}
	}
	for _, l := range ix.in.legs {
		if trades[rowKey{l.account, l.ref}] && is(l.securityKey, l.symbol) && !costs(l.quantity, l.cost, l.price) {
			return false
		}
	}
	for _, p := range ix.pricesOfGroup(s) {
		if p.price.Cmp(big.NewRat(1, 5)) < 0 || p.price.Cmp(big.NewRat(5, 1)) > 0 {
			return false
		}
	}
	return true
}

// pricesOf returns the prices of the security of key, whose multiplier is
// m, worked out once (5.5 of the design): for each market date, from the
// first of its positions (the latest taken, then the least account) that
// gives a price, its market value over its shares, rounded to 8 places;
// none on a date whose price rounds to 0. A position without a quantity or
// a market value gives none, and the next of its date is taken. Any
// account's positions price it, since a price is the market's.
func (ix *toolIndex) pricesOf(key string, m int64) []toolPrice {
	if key == "" {
		return nil
	}
	if p, ok := ix.prices[key]; ok {
		return p
	}
	var ps []toolPosition
	for _, p := range ix.in.positions {
		if p.key == key && p.marketDate != "" && (ix.in.to == "" || p.marketDate <= ix.in.to) {
			ps = append(ps, p)
		}
	}
	slices.SortStableFunc(ps, func(a, b toolPosition) int {
		return cmp.Or(cmp.Compare(a.marketDate, b.marketDate), b.fetchedAt.Compare(a.fetchedAt), cmp.Compare(a.account, b.account))
	})
	var out []toolPrice
	for i := 0; i < len(ps); {
		j := i
		for j < len(ps) && ps[j].marketDate == ps[i].marketDate {
			j++
		}
		for _, p := range ps[i:j] {
			price, ok := positionPrice(p, m)
			if !ok {
				continue
			}
			// A price that rounds to 0 is not written: whether the tools
			// take one is not known.
			if price.Sign() > 0 {
				out = append(out, toolPrice{date: p.marketDate, price: price})
			}
			break
		}
		i = j
	}
	ix.prices[key] = out
	return out
}

// pricesOfGroup returns the prices of s and of the store's securities that
// the tools take for one with it (membersOf), whose multiplier is s's,
// worked out once: for each market date, the price of the first of them, in
// the order of their keys, that has one (pricesOf), as securities.csv and a
// Ghostfolio asset take them (mergeListed); in the order of their dates.
func (ix *toolIndex) pricesOfGroup(s *toolSecurity) []toolPrice {
	members := ix.membersOf(s)
	if p, ok := ix.groupPrices[members[0].ref()]; ok {
		return p
	}
	byKey := slices.SortedFunc(slices.Values(members), func(a, b *toolSecurity) int {
		return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.symbol, b.symbol))
	})
	var out []toolPrice
	for _, m := range byKey {
		for _, p := range ix.pricesOf(m.key, s.multiplier) {
			if !slices.ContainsFunc(out, func(q toolPrice) bool { return q.date == p.date }) {
				out = append(out, p)
			}
		}
	}
	slices.SortStableFunc(out, func(a, b toolPrice) int { return cmp.Compare(a.date, b.date) })
	ix.groupPrices[members[0].ref()] = out
	return out
}

// positionPrice returns the price per share of position p of a security
// whose multiplier is m, rounded to 8 places, and false when it gives
// none: its quantity is its long quantity when above 0, else its short
// one, its shares that times m, and its price its market value, unsigned,
// over its shares.
func positionPrice(p toolPosition, m int64) (*big.Rat, bool) {
	q := p.long
	if !q.Valid || q.Int64 <= 0 {
		q = p.short
	}
	if !q.Valid || q.Int64 <= 0 || !p.marketValue.Valid {
		return nil, false
	}
	shares, ok := sharesOf(q.Int64, m)
	value := decimalRat(p.marketValue.Float64)
	if !ok || value == nil {
		return nil, false
	}
	// |market value| over shares / 10^6, to 8 places.
	price := new(big.Rat).Quo(new(big.Rat).Mul(value.Abs(value), big.NewRat(1_000_000, 1)), new(big.Rat).SetInt64(shares))
	return new(big.Rat).SetFrac(roundUnits(price, 8), big.NewInt(100_000_000)), true
}

// priceOn returns the snapshot price of security s on day, nil for none:
// of any of the store's securities that the tools take for one with it,
// as securities.csv and a Ghostfolio asset price it (pricesOfGroup).
func (ix *toolIndex) priceOn(s *toolSecurity, day nytime.Date) *big.Rat {
	if s.badMultiplier {
		return nil
	}
	for _, p := range ix.pricesOfGroup(s) {
		if p.date == day {
			return p.price
		}
	}
	return nil
}

// sharesOf returns |q| × m, a quantity in millionths times a multiplier,
// and false when the product, or |q|, overflows.
func sharesOf(q, m int64) (int64, bool) {
	if q == math.MinInt64 || m <= 0 {
		return 0, false
	}
	q = max(q, -q)
	if q > math.MaxInt64/m {
		return 0, false
	}
	return q * m, true
}

// A toolListed is a security that a tool export writes (5.5 and 6.4 of the
// design): in PP's securities.csv, and as a Ghostfolio asset when it is a
// MANUAL one.
type toolListed struct {
	security *toolSecurity
	// members are, once the securities that the tool takes for one are
	// merged (mergeListed), each of them, security first; nil before.
	members []*toolSecurity
	// used says that a record of the export names it; one that no record
	// names is written for its prices alone.
	used bool
	// prices are its snapshot prices (pricesOfGroup), in the order of their
	// market dates, when the tool has no feed that serves it (ppManual for
	// PP, a MANUAL asset for Ghostfolio); none for a bond whose unit is off,
	// whose prices are left out with its trades, nor for a future or an
	// option whose multiplier is not whole, no record of which is ever
	// written.
	prices []toolPrice
}

// listSecurities returns the securities that the export of in writes for
// in.tool, in the order of their keys, then the symbols of those without
// one (5.5 and 6.4 of the design): every security that a record of events
// names, and every one that a position of a selected account held up to
// the export's last day, when it has prices; each with its prices when the
// tool has no feed for it (toolListed).
func listSecurities(in *toolInput, events []toolEvent) []toolListed {
	ix := newToolIndex(in)
	listed := map[secRef]*toolListed{}
	for _, e := range events {
		if s := e.security; s != nil && listed[s.ref()] == nil {
			listed[s.ref()] = &toolListed{security: s, used: true}
		}
	}
	for _, s := range toolHeld(in) {
		if listed[s.ref()] == nil {
			listed[s.ref()] = &toolListed{security: s}
		}
	}
	var out []toolListed
	for _, l := range listed {
		s := l.security
		feedless := in.tool == toolPP && s.ppManual() || in.tool == toolGF && s.manual != ""
		if feedless && !s.bondUnit && !s.badMultiplier && s.assetType != "FUTURE" {
			l.prices = ix.pricesOfGroup(s)
		}
		if l.used || len(l.prices) > 0 {
			out = append(out, *l)
		}
	}
	slices.SortFunc(out, func(a, b toolListed) int {
		return cmp.Or(cmp.Compare(a.security.key, b.security.key), cmp.Compare(a.security.symbol, b.security.symbol))
	})
	return out
}

// sameSecurity returns, for each of secs by its ref, the ref of the first
// of secs that tool takes for the same security, whatever store keys they
// have, so that what the export writes and checks of a security is of the
// tool's one security. The store's securities that the tools take for one
// (groupAll) have one data source and symbol, and one Ticker Symbol, WKN
// and Security Name. For Ghostfolio, those of one data source and symbol
// are one; for PP, those that PP's import finds one of when it meets them
// in the order of secs, as securities.csv lists them, from a PP that holds
// none (SecurityCache.java:66-159): by the Ticker Symbol up to its first
// dot, then the WKN, the first that finds one; else by the exact name, when
// the name is no other's of them and no Ticker Symbol or WKN that both
// have differs; else it is a new one.
func sameSecurity(tool string, secs []*toolSecurity) map[secRef]secRef {
	of := make([]int, len(secs))
	if tool == toolGF {
		first := map[string]int{}
		for i, s := range secs {
			id := s.gfDataSource() + " " + s.gfSymbol()
			if _, ok := first[id]; !ok {
				first[id] = i
			}
			of[i] = first[id]
		}
	} else {
		type made struct {
			i                 int
			ticker, wkn, name string
		}
		var pp []made
		differ := func(a, b string) bool { return a != "" && b != "" && !strings.EqualFold(a, b) }
		finds := func(ticker, wkn, name string) int {
			prefix, _, _ := strings.Cut(ticker, ".")
			for _, m := range pp {
				if p, _, _ := strings.Cut(m.ticker, "."); prefix != "" && p == prefix {
					return m.i
				}
			}
			for _, m := range pp {
				if wkn != "" && m.wkn == wkn {
					return m.i
				}
			}
			named := slices.DeleteFunc(slices.Clone(pp), func(m made) bool { return m.name != name })
			if len(named) == 1 && !differ(ticker, named[0].ticker) && !differ(wkn, named[0].wkn) {
				return named[0].i
			}
			return -1
		}
		for i, s := range secs {
			ticker, wkn, name := ppIdentifiers(s)
			if name == "" {
				// BaseCSVExtractor.java:138-151.
				name = "Imported security: " + cmp.Or(ticker, wkn)
			}
			if of[i] = finds(ticker, wkn, name); of[i] < 0 {
				of[i] = i
				pp = append(pp, made{i, ticker, wkn, name})
			}
		}
	}
	out := map[secRef]secRef{}
	for i, s := range secs {
		out[s.ref()] = secs[of[i]].ref()
	}
	return out
}

// mergeListed returns the securities of listed, in their order, as tool
// writes them (sameSecurity): one for each of the tool's securities, the
// first of listed of it with every one of them as its members, used when
// any of them is, and with the prices of each date from the first of them
// that has one.
func mergeListed(tool string, listed []toolListed) []toolListed {
	secs := make([]*toolSecurity, len(listed))
	for i := range listed {
		secs[i] = listed[i].security
	}
	same := sameSecurity(tool, secs)
	var out []toolListed
	at := map[secRef]int{}
	for _, l := range listed {
		i, ok := at[same[l.security.ref()]]
		if !ok {
			i = len(out)
			at[same[l.security.ref()]] = i
			out = append(out, toolListed{security: l.security})
		}
		o := &out[i]
		o.used = o.used || l.used
		o.members = append(o.members, l.security)
		for _, p := range l.prices {
			if !slices.ContainsFunc(o.prices, func(q toolPrice) bool { return q.date == p.date }) {
				o.prices = append(o.prices, p)
			}
		}
	}
	for i := range out {
		slices.SortStableFunc(out[i].prices, func(a, b toolPrice) int { return cmp.Compare(a.date, b.date) })
	}
	return out
}

// toolHeld returns the securities that a position of a selected account
// held on some day up to the export's last (Q6), in the order of their
// keys: those whose prices a tool export writes even when no record names
// them.
func toolHeld(in *toolInput) []*toolSecurity {
	ix := newToolIndex(in)
	keys := map[string]bool{}
	for _, p := range in.positions {
		if p.key != "" && in.selected(p.account) && (in.to == "" || p.marketDate <= in.to) {
			keys[p.key] = true
		}
	}
	var out []*toolSecurity
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		out = append(out, ix.security(k, ""))
	}
	return out
}

// toolBalances returns each selected account's balances for Ghostfolio
// (6.3 of the design), by its id: for every market date of its
// closed-phase snapshots, whatever the export's last day, the cash of the
// latest snapshot of that date, when it has one, in the order of the
// dates.
func toolBalances(in *toolInput) map[int64][]toolBalance {
	type accountDay struct {
		account int64
		day     nytime.Date
	}
	latest := map[accountDay]toolSnapshot{}
	for _, s := range in.snapshots {
		if !in.selected(s.account) || s.marketDate == "" {
			continue
		}
		k := accountDay{s.account, s.marketDate}
		if l, ok := latest[k]; !ok || s.fetchedAt.After(l.fetchedAt) {
			latest[k] = s
		}
	}
	out := map[int64][]toolBalance{}
	for _, s := range latest {
		if s.cash.Valid {
			out[s.account] = append(out[s.account], toolBalance{date: s.marketDate, cash: s.cash.Int64, fetchedAt: s.fetchedAt})
		}
	}
	for _, bs := range out {
		slices.SortFunc(bs, func(a, b toolBalance) int { return cmp.Compare(a.date, b.date) })
	}
	return out
}

// toolEvents maps the ledger rows of in to the records that in.tool gets,
// and gives each row its outcome (3.2 to 3.4 and 4 of the design): the
// records in the order of the rows, a row's in the order of its legs; the
// outcomes one for each row, in the rows' order. It reads nothing of an
// earlier export: -since compares what it gives with what that export gave.
func toolEvents(in *toolInput) ([]toolEvent, []toolOutcome) {
	m := &toolMapper{ix: newToolIndex(in), in: in, outcomes: make([]toolOutcome, len(in.rows))}
	m.run()
	return m.events, m.outcomes
}

// A toolMapper maps the ledger rows of a toolInput (toolEvents).
type toolMapper struct {
	ix       *toolIndex
	in       *toolInput
	events   []toolEvent
	outcomes []toolOutcome
	// sharesRow and cashRow are the two rows of each two-row cash event
	// (pairTwoRows), by their index in the rows: the shares row of each
	// cash row, and the cash row of each shares row.
	sharesRow, cashRow map[int]int
	// balances are the accounts' balances (toolBalances), once read.
	balances map[int64][]toolBalance
}

// run maps every row: the shares row of a two-row cash event after its
// cash row, whose sale decides it; then gives each row written, combined
// or in the balances its attention flags.
func (m *toolMapper) run() {
	m.pairTwoRows()
	for i := range m.in.rows {
		if _, ok := m.cashRow[i]; !ok {
			m.decide(i)
		}
	}
	for _, o := range slices.Sorted(maps.Keys(m.cashRow)) {
		m.follow(o, m.cashRow[o])
	}
	for i := range m.outcomes {
		o := &m.outcomes[i]
		if o.outcome == outcomeLeftOut {
			continue
		}
		for _, f := range m.in.rows[i].flags {
			if flagSeverity(f) == attention && !slices.Contains(o.flags, f) {
				o.flags = append(o.flags, f)
			}
		}
		slices.Sort(o.flags)
	}
}

// decide maps row i by the first rule that applies (3.2 of the design).
func (m *toolMapper) decide(i int) {
	r := &m.in.rows[i]
	switch {
	case r.day == "":
		m.leftOut(i, "no_day")
	case r.pairKind == "sweep":
		m.leftOut(i, "sweep_pair")
	case r.category == "other":
		if s, ok := m.isClose(r); ok {
			m.optionClose(i, s)
			return
		}
		m.leftOut(i, "other")
	case !r.amount.Valid && !m.inKind(r):
		m.leftOut(i, "no_amount")
	case r.category == "buy" || r.category == "sell":
		m.trade(i)
	case m.inKind(r):
		m.delivery(i)
	case r.category == "journal":
		m.cashJournal(i)
	case slices.Contains([]string{"deposit", "withdrawal", "dividend", "interest", "fee", "tax"}, r.category):
		m.bySign(i)
	default:
		m.leftOut(i, "other")
	}
}

// inKind reports whether row r moves shares in kind (3.4.2 of the design):
// a transfer in or out, or a journal of shares, which has a quantity, or
// legs of a security when it has several.
func (m *toolMapper) inKind(r *toolRow) bool {
	switch r.category {
	case "transfer_in", "transfer_out":
		return true
	case "journal":
		return r.quantity.Valid || len(m.ix.legsOf(r)) > 0
	}
	return false
}

// event returns a record of row i of kind, with the row's own day, time,
// text and cash.
func (m *toolMapper) event(i int, kind eventKind) toolEvent {
	r := &m.in.rows[i]
	return toolEvent{
		row: i, ref: r.ref, account: m.ix.account(r.account), kind: kind,
		day: r.day, time: r.time, text: cmp.Or(r.description, r.kind), amount: r.amount.Int64,
	}
}

// written records row i as written, as the records evs.
func (m *toolMapper) written(i int, evs []toolEvent) {
	r := &m.in.rows[i]
	o := toolOutcome{ref: r.ref, account: m.ix.account(r.account), outcome: outcomeWritten}
	for j := range evs {
		evs[j].caveats = caveatOrder(evs[j].caveats)
		o.caveats = append(o.caveats, evs[j].caveats...)
	}
	o.caveats = caveatOrder(o.caveats)
	m.outcomes[i] = o
	m.events = append(m.events, evs...)
}

// caveatOrder returns caveats each once, in the order of toolCaveats.
func caveatOrder(caveats []string) []string {
	var out []string
	for _, c := range toolCaveats {
		if slices.Contains(caveats, c.code) {
			out = append(out, c.code)
		}
	}
	return out
}

// leftOut records row i as left out for reason, with the manifest's advice.
func (m *toolMapper) leftOut(i int, reason string) {
	r := &m.in.rows[i]
	advice, command := leftOutAdvice(reason, r)
	m.outcomes[i] = toolOutcome{
		ref: r.ref, account: m.ix.account(r.account), outcome: outcomeLeftOut,
		reason: reason, advice: advice, command: command,
	}
}

// pairTwoRows finds the two rows of each two-row cash event (4.6 of the
// design): a cash merger's or a redemption's cash row, booked sell, with no
// quantity and cash in, and its shares row, booked other, with shares out
// and no cash, both flagged two_row_event, of one account and day and one
// security (by key, or, both without one, by symbol). Each is paired only
// when it is the other's one candidate and the other is its own, so that
// no shares row is used twice.
func (m *toolMapper) pairTwoRows() {
	m.sharesRow, m.cashRow = map[int]int{}, map[int]int{}
	var cash, shares []int
	for i := range m.in.rows {
		r := &m.in.rows[i]
		if r.day == "" || r.pairKind == "sweep" || !slices.Contains(r.flags, "two_row_event") || !r.amount.Valid {
			continue
		}
		switch {
		case r.category == "sell" && (!r.quantity.Valid || r.quantity.Int64 == 0) && r.amount.Int64 > 0:
			cash = append(cash, i)
		case r.category == "other" && r.quantity.Valid && r.quantity.Int64 < 0 && r.amount.Int64 == 0:
			if _, ok := m.isClose(r); !ok {
				shares = append(shares, i)
			}
		}
	}
	match := func(c, o int) bool {
		a, b := &m.in.rows[c], &m.in.rows[o]
		return a.account == b.account && a.day == b.day && a.securityKey == b.securityKey && (a.securityKey != "" || a.symbol == b.symbol)
	}
	only := func(i int, others []int, match func(int, int) bool) (int, bool) {
		found := -1
		for _, j := range others {
			if match(i, j) {
				if found >= 0 {
					return 0, false
				}
				found = j
			}
		}
		return found, found >= 0
	}
	for _, c := range cash {
		o, ok := only(c, shares, match)
		if !ok {
			continue
		}
		if back, ok := only(o, cash, func(o, c int) bool { return match(c, o) }); ok && back == c {
			m.sharesRow[c], m.cashRow[o] = o, c
		}
	}
}

// follow gives shares row o of a two-row cash event its cash row c's
// outcome: combined when c's sale is written, else left out with c's
// reason.
func (m *toolMapper) follow(o, c int) {
	r := &m.in.rows[o]
	if co := m.outcomes[c]; co.outcome == outcomeLeftOut {
		m.leftOut(o, co.reason)
	} else {
		m.outcomes[o] = toolOutcome{ref: r.ref, account: m.ix.account(r.account), outcome: outcomeCombined}
	}
	m.outcomes[o].with = m.in.rows[c].ref
}

// isClose reports whether row r, booked other, removes an option at no
// cash (4.3 of the design): a file row Expired, Assigned, Exercised or
// Exchange or Exercise, or an API row RECEIVE_AND_DELIVER that says
// "Removed due to Expiration", of an option, with a quantity and no cash;
// and returns the option.
func (m *toolMapper) isClose(r *toolRow) (*toolSecurity, bool) {
	if r.category != "other" || !r.quantity.Valid || r.quantity.Int64 == 0 || !r.amount.Valid || r.amount.Int64 != 0 {
		return nil, false
	}
	s := m.ix.security(r.securityKey, r.symbol)
	if s == nil || s.assetType != "OPTION" {
		return nil, false
	}
	switch r.source {
	case "file":
		return s, slices.Contains(closeActions, normalAction(r.kind))
	case "api":
		return s, r.kind == "RECEIVE_AND_DELIVER" && strings.HasPrefix(r.description, "Removed due to Expiration")
	}
	return nil, false
}

// optionClose writes row i, an option close of option s, as a delivery of
// value 0: out for a long option removed, in for a short one.
func (m *toolMapper) optionClose(i int, s *toolSecurity) {
	r := &m.in.rows[i]
	if s.badMultiplier {
		m.leftOut(i, "multiplier")
		return
	}
	shares, ok := sharesOf(r.quantity.Int64, s.multiplier)
	if !ok {
		m.leftOut(i, "no_quantity")
		return
	}
	kind := evCloseIn
	if r.quantity.Int64 < 0 {
		kind = evCloseOut
	}
	e := m.event(i, kind)
	e.security, e.shares, e.price = s, shares, new(big.Rat)
	e.caveats = []string{"option_closed_at_zero"}
	if s.assumed {
		e.caveats = append(e.caveats, "assumed_multiplier")
	}
	m.written(i, []toolEvent{e})
}

// trade maps row i, booked buy or sell, by the trade rules (3.4.1 of the
// design): a row without a quantity is the cash row of a two-row cash
// event, a trade of several legs, or left out.
func (m *toolMapper) trade(i int) {
	r := &m.in.rows[i]
	if !r.quantity.Valid || r.quantity.Int64 == 0 {
		legs := m.ix.legsOf(r)
		switch o, ok := m.sharesRow[i]; {
		case ok:
			m.twoRowSale(i, o)
		case !r.quantity.Valid && len(legs) >= 2:
			m.multiLeg(i, legs)
		default:
			m.leftOut(i, "no_quantity")
		}
		return
	}
	s := m.ix.security(r.securityKey, r.symbol)
	evs, reason := m.tradeEvents(i, sql.NullInt64{}, r.amount, r.fees, r.gross, r.quantity, s)
	if reason != "" {
		m.leftOut(i, reason)
		return
	}
	m.written(i, evs)
}

// tradeEvents returns the records of a trade of row i, or of its leg leg:
// cash a, fees f and gross g in cents, quantity q in millionths, of
// security s (3.4.1 of the design); or the reason it is left out. A buy
// costs its cash with its fees; a sale brings in its cash, and, for PP, a
// sale whose fees exceed its gross is a Sell of the gross and a Fees row
// of the fees (4.11). Fees off the grid are written as 0, so that the
// cash is right and the fees ride in the price; a rebate (fees below 0)
// rides in the price too.
func (m *toolMapper) tradeEvents(i int, leg, a, f, g, q sql.NullInt64, s *toolSecurity) ([]toolEvent, string) {
	switch {
	case !q.Valid || q.Int64 == 0:
		return nil, "no_quantity"
	case s == nil:
		return nil, "no_security"
	case s.assetType == "FUTURE":
		return nil, "future"
	case s.bondUnit:
		return nil, "bond_unit"
	case s.badMultiplier:
		return nil, "multiplier"
	}
	shares, ok := sharesOf(q.Int64, s.multiplier)
	if !ok {
		return nil, "no_quantity"
	}
	var caveats []string
	if s.assumed {
		caveats = append(caveats, "assumed_multiplier")
	}
	fees, gross := f.Int64, g.Int64
	switch {
	case !f.Valid || !g.Valid:
		fees, gross = 0, a.Int64
		caveats = append(caveats, "fees_unknown")
	case f.Int64 < 0:
		fees, gross = 0, a.Int64
	}
	kind := evBuy
	if q.Int64 < 0 {
		kind = evSell
	}
	if kind == evBuy && gross > 0 || kind == evSell && gross < 0 {
		return nil, "signs_disagree"
	}
	e := m.event(i, kind)
	e.leg, e.security, e.shares, e.amount, e.fees, e.gross, e.caveats = leg, s, shares, a.Int64, fees, gross, caveats
	switch {
	case kind == evBuy:
		e.value = -a.Int64
	case a.Int64 >= 0:
		e.value = a.Int64
	default:
		e.value = gross
		if m.in.tool != toolPP {
			break
		}
		e.fees = 0
		e.caveats = append(e.caveats, "fees_apart")
		apart := m.event(i, evFees)
		apart.leg, apart.security, apart.value, apart.amount = leg, s, fees, a.Int64
		apart.caveats = []string{"fees_apart"}
		return []toolEvent{e, apart}, ""
	}
	return []toolEvent{e}, ""
}

// multiLeg maps row i, a trade of several security legs (4.2 of the
// design), as one trade per leg: each leg's cash is its cost, but the
// leg's of the largest cost (the first such), which carries every fee of
// the row; a row whose legs do not add up to its cash is left out.
func (m *toolMapper) multiLeg(i int, legs []toolLeg) {
	r := &m.in.rows[i]
	if slices.Contains(r.flags, "legs_mismatch") {
		m.leftOut(i, "legs_mismatch")
		return
	}
	feeLeg := 0
	for j, l := range legs {
		if !l.quantity.Valid || !l.cost.Valid {
			m.leftOut(i, "no_amount")
			return
		}
		if abs64(l.cost.Int64) > abs64(legs[feeLeg].cost.Int64) {
			feeLeg = j
		}
	}
	others := int64(0)
	for j, l := range legs {
		if j != feeLeg {
			others += l.cost.Int64
		}
	}
	var evs []toolEvent
	for j, l := range legs {
		a, f, g := l.cost, sql.NullInt64{Valid: true}, l.cost
		if j == feeLeg {
			if r.fees.Valid && r.gross.Valid {
				a, f = sql.NullInt64{Int64: l.cost.Int64 - r.fees.Int64, Valid: true}, r.fees
			} else {
				// Fees off the grid: the leg's cash is the row's, less the
				// other legs', so that the row's cash is right.
				a, f, g = sql.NullInt64{Int64: r.amount.Int64 - others, Valid: true}, sql.NullInt64{}, sql.NullInt64{}
			}
		}
		s := m.ix.security(l.securityKey, l.symbol)
		leg, reason := m.tradeEvents(i, sql.NullInt64{Int64: int64(l.leg), Valid: true}, a, f, g, l.quantity, s)
		if reason != "" {
			m.leftOut(i, reason)
			return
		}
		evs = append(evs, leg...)
	}
	m.written(i, evs)
}

// abs64 returns |v|.
func abs64(v int64) int64 { return max(v, -v) }

// twoRowSale maps cash row c of a two-row cash event as one sale of its
// shares row o's shares, of o's security, for c's cash (4.6 of the design).
func (m *toolMapper) twoRowSale(c, o int) {
	cr, or := &m.in.rows[c], &m.in.rows[o]
	s := m.ix.security(or.securityKey, or.symbol)
	evs, reason := m.tradeEvents(c, sql.NullInt64{}, cr.amount, cr.fees, cr.gross, or.quantity, s)
	if reason != "" {
		m.leftOut(c, reason)
		return
	}
	for j := range evs {
		evs[j].with = or.ref
	}
	m.written(c, evs)
	m.outcomes[c].with = or.ref
}

// delivery maps row i, which moves shares in kind, as a delivery in or out
// of each account's own portfolio (3.4.2 and 4.7 of the design): one per
// leg of a journal of several securities.
func (m *toolMapper) delivery(i int) {
	r := &m.in.rows[i]
	var evs []toolEvent
	if legs := m.ix.legsOf(r); !r.quantity.Valid && len(legs) >= 2 {
		for _, l := range legs {
			s := m.ix.security(l.securityKey, l.symbol)
			e, reason := m.deliveryEvent(i, l.quantity, s, l.price, l.price, r.day)
			if reason != "" {
				m.leftOut(i, reason)
				return
			}
			e.leg = sql.NullInt64{Int64: int64(l.leg), Valid: true}
			evs = append(evs, e)
		}
	} else {
		// A journal pair's two sides take the delivering side's price, so
		// that both carry one value.
		deliver, receive, day := r.price, r.price, r.day
		if r.pairKind == "journal" && r.pairRef != "" {
			switch {
			case r.quantity.Valid && r.quantity.Int64 > 0:
				deliver = r.partner.price
				day = cmp.Or(r.partner.day, r.day)
			default:
				receive = r.partner.price
			}
		}
		e, reason := m.deliveryEvent(i, r.quantity, m.ix.security(r.securityKey, r.symbol), deliver, receive, day)
		if reason != "" {
			m.leftOut(i, reason)
			return
		}
		evs = append(evs, e)
	}
	if r.amount.Valid && r.amount.Int64 != 0 {
		for j := range evs {
			evs[j].caveats = append(evs[j].caveats, "cash_not_written")
		}
	}
	m.written(i, evs)
}

// deliveryEvent returns the record of a delivery of row i: quantity q in
// millionths of security s, priced by the delivering side's price deliver,
// else the receiving side's receive, else the security's snapshot price on
// day, the day it was delivered, else 0; or the reason it is left out.
func (m *toolMapper) deliveryEvent(i int, q sql.NullInt64, s *toolSecurity, deliver, receive sql.NullFloat64, day nytime.Date) (toolEvent, string) {
	switch {
	case !q.Valid || q.Int64 == 0:
		return toolEvent{}, "no_quantity"
	case s == nil:
		return toolEvent{}, "no_security"
	case s.assetType == "FUTURE":
		return toolEvent{}, "future"
	case s.bondUnit:
		return toolEvent{}, "bond_unit"
	case s.badMultiplier:
		return toolEvent{}, "multiplier"
	}
	shares, ok := sharesOf(q.Int64, s.multiplier)
	if !ok {
		return toolEvent{}, "no_quantity"
	}
	kind := evDeliveryIn
	if q.Int64 < 0 {
		kind = evDeliveryOut
	}
	e := m.event(i, kind)
	e.security, e.shares = s, shares
	if s.assumed {
		e.caveats = append(e.caveats, "assumed_multiplier")
	}
	for _, p := range []sql.NullFloat64{deliver, receive} {
		if p.Valid && p.Float64 > 0 {
			e.price = decimalRat(p.Float64)
			break
		}
	}
	switch {
	case e.price != nil && s.bond:
		// A bond's price is per 100 of face, and its shares are its face.
		e.price.Quo(e.price, big.NewRat(100, 1))
	case e.price == nil:
		e.price = new(big.Rat)
		if p := m.ix.priceOn(s, day); p != nil {
			e.price.Set(p)
			e.caveats = append(e.caveats, "value_from_snapshot")
		} else {
			e.caveats = append(e.caveats, "value_unknown")
		}
	}
	// shares / 10^6 × price, in cents.
	value := roundUnits(new(big.Rat).Mul(new(big.Rat).SetFrac64(shares, 1_000_000), e.price), 2)
	if !value.IsInt64() {
		return toolEvent{}, "no_quantity"
	}
	e.value = value.Int64()
	return e, ""
}

// cashJournal maps row i, a journal of cash (3.4.3 of the design): as a
// transfer when it is paired with a counted journal of another account of
// the opposite amount, both booked journal, both of accounts that the export
// selects; else by its sign, saying why a pair of two accounts is written
// apart. A pair within one account, which you can pair by hand, is a
// deposit and a withdrawal, which net to nothing: a transfer of an account
// to itself names that account as its own Offset Account, and PP imports it
// once from each side.
func (m *toolMapper) cashJournal(i int) {
	r := &m.in.rows[i]
	if r.pairKind != "journal" || r.pairRef == "" || !r.partner.counted || r.pairAccount == r.account {
		m.cash(i, nil)
		return
	}
	var caveats []string
	if !m.in.selected(r.pairAccount) {
		caveats = append(caveats, "pair_outside")
	}
	if r.partner.category != "journal" {
		caveats = append(caveats, "pair_not_journal")
	}
	if !r.partner.amount.Valid || r.amount.Int64 != -r.partner.amount.Int64 {
		caveats = append(caveats, "pair_amounts_differ")
	}
	if len(caveats) > 0 || r.amount.Int64 == 0 {
		m.cash(i, caveats)
		return
	}
	if m.in.tool == toolGF {
		m.gfCash(i)
		return
	}
	pays := r.amount.Int64 < 0
	kind := evTransferIn
	if pays {
		kind = evTransferOut
	}
	e := m.event(i, kind)
	e.value, e.pair, e.pairRef = abs64(r.amount.Int64), m.ix.account(r.pairAccount), r.pairRef
	if !pays && r.partner.day != "" {
		// The paying side's day and time date both sides, and its text is
		// both notes'.
		e.day, e.time, e.text = r.partner.day, r.partner.time, cmp.Or(r.partner.description, r.partner.kind)
	}
	m.written(i, []toolEvent{e})
}

// cash maps row i, which moves cash only, by its sign: a deposit or a
// withdrawal, with the caveats that say why a pair is written apart; for
// Ghostfolio, whose cash is the accounts' balances, it is in the balances
// or left out.
func (m *toolMapper) cash(i int, caveats []string) {
	r := &m.in.rows[i]
	switch {
	case r.amount.Int64 == 0:
		m.leftOut(i, "zero_amount")
		return
	case m.in.tool == toolGF:
		m.gfCash(i)
		return
	}
	kind := evDeposit
	if r.amount.Int64 < 0 {
		kind = evWithdrawal
	}
	e := m.event(i, kind)
	e.value, e.caveats = abs64(r.amount.Int64), caveats
	if len(caveats) > 0 {
		e.pair, e.pairRef = m.ix.account(r.pairAccount), r.pairRef
	}
	m.written(i, []toolEvent{e})
}

// gfCash gives row i, a cash row, its outcome for Ghostfolio (6.3 of the
// design): in the balances when the account's last balance carries it,
// having been posted before the snapshot whose cash it is was taken (an API
// row by its time; a file row by its posted date and an API row without a
// time by its day, before the New York date that the snapshot was taken);
// else left out, gf_cash, until a later snapshot carries it.
func (m *toolMapper) gfCash(i int) {
	r := &m.in.rows[i]
	carried := false
	if snap, ok := m.lastBalance(r.account); ok {
		if !r.time.IsZero() {
			carried = !r.time.After(snap.fetchedAt)
		} else {
			day := r.day
			if r.source == "file" && r.posted != "" {
				day = r.posted
			}
			taken, _ := nyClock(snap.fetchedAt)
			carried = day != "" && day < taken
		}
	}
	if !carried {
		m.leftOut(i, "gf_cash")
		return
	}
	m.outcomes[i] = toolOutcome{ref: r.ref, account: m.ix.account(r.account), outcome: outcomeInBalances}
}

// lastBalance returns the account's last balance in Ghostfolio
// (toolBalances), and false when it has none.
func (m *toolMapper) lastBalance(account int64) (toolBalance, bool) {
	if m.balances == nil {
		m.balances = toolBalances(m.in)
	}
	bs := m.balances[account]
	if len(bs) == 0 {
		return toolBalance{}, false
	}
	return bs[len(bs)-1], true
}

// bySign maps row i, booked deposit, withdrawal, dividend, interest, fee or
// tax, by the sign of its cash (3.3 of the design).
func (m *toolMapper) bySign(i int) {
	r := &m.in.rows[i]
	a := r.amount.Int64
	if a == 0 {
		m.leftOut(i, "zero_amount")
		return
	}
	s := m.ix.security(r.securityKey, r.symbol)
	switch {
	case r.category == "deposit" || r.category == "withdrawal":
		m.cash(i, nil)
	case r.category == "dividend" && a > 0:
		m.dividend(i, s)
	case r.category == "dividend":
		m.charge(i, evFees, s, "fees")
	case r.category == "interest" && a < 0:
		m.charge(i, evInterestCharge, nil, "fees")
	case r.category == "interest" && s == nil:
		m.charge(i, evInterest, nil, "interest")
	case r.category == "interest" && m.in.tool == toolGF && s.bondUnit:
		// A coupon of a bond whose trades are left out: in Ghostfolio, a
		// dividend of a holding with no investment would set the whole
		// portfolio's dividend yield to 0.
		m.charge(i, evInterest, nil, "interest", "coupon_as_interest")
	case r.category == "interest":
		m.dividend(i, s)
	case r.category == "fee" && a < 0:
		m.charge(i, evFees, s, "fees")
	case r.category == "fee":
		m.charge(i, evFeesRefund, s, "fees")
	case r.category == "tax" && a < 0:
		m.charge(i, evTaxes, s, "taxes")
	default:
		m.charge(i, evTaxRefund, s, "taxes")
	}
}

// dividend writes row i as a dividend of security s, with its fees when it
// has some: a dividend needs its security.
func (m *toolMapper) dividend(i int, s *toolSecurity) {
	r := &m.in.rows[i]
	if s == nil {
		m.leftOut(i, "no_security")
		return
	}
	e := m.event(i, evDividend)
	e.security, e.value = s, r.amount.Int64
	switch {
	case !r.fees.Valid || !r.gross.Valid:
		e.caveats = append(e.caveats, "fees_unknown")
	case r.fees.Int64 > 0:
		e.fees = r.fees.Int64
	}
	e.gross = e.value + e.fees
	if slices.Contains(r.flags, "return_of_capital") {
		e.caveats = append(e.caveats, "return_of_capital")
	}
	m.written(i, []toolEvent{e})
}

// charge writes row i as a record of kind that moves its cash alone: a
// fee, a tax, interest or a refund, of security s when PP takes one on
// it, nil for none. For Ghostfolio it is an activity of the cash profile
// profile, never of the security.
func (m *toolMapper) charge(i int, kind eventKind, s *toolSecurity, profile string, caveats ...string) {
	e := m.event(i, kind)
	e.value, e.caveats = abs64(m.in.rows[i].amount.Int64), caveats
	if m.in.tool == toolGF {
		e.profile = profile
	} else {
		e.security = s
	}
	m.written(i, []toolEvent{e})
}

// A toolHolding is a difference of the holdings check (4.15 of the
// design): what an account's latest snapshot holds of a security, against
// what the records written leave up to its day.
type toolHolding struct {
	account toolAccount
	// key is the security's key, "" for a row's security without one; and
	// symbol its latest symbol. When the tool takes several of the store's
	// securities for this one (sameSecurity), key is the least of their
	// keys, and keys are every one of them, key first.
	key, symbol string
	keys        []string
	// snapshot is the market date of the snapshot.
	snapshot nytime.Date
	// snapshotShares and writtenShares are the shares that the snapshot
	// holds (long less short, times the multiplier) and that the records
	// leave, in millionths, signed.
	snapshotShares, writtenShares int64
}

// holdingsCheck compares, for each selected account with a row or a
// snapshot, the shares that the records events leave of each security up
// to the day of its latest closed-phase snapshot on or before the export's
// last day with that snapshot's positions (4.15 of the design): a sale of a
// position opened before the store's rows, a split left out or a trade left
// out shows as a difference. A security is the tool's (sameSecurity): the
// store's securities that it holds as one, under two keys, are one. It
// returns the differences, by account and then security, and the accounts
// with rows that have no snapshot to check against.
func holdingsCheck(in *toolInput, events []toolEvent) ([]toolHolding, []toolAccount) {
	ix := newToolIndex(in)
	accounts := map[int64]bool{}
	for i := range in.rows {
		if id := in.rows[i].account; in.selected(id) {
			accounts[id] = true
		}
	}
	latest := map[int64]toolSnapshot{}
	for _, s := range in.snapshots {
		if !in.selected(s.account) || s.marketDate == "" || in.to != "" && s.marketDate > in.to {
			continue
		}
		accounts[s.account] = true
		if l, ok := latest[s.account]; !ok || s.marketDate > l.marketDate || s.marketDate == l.marketDate && s.fetchedAt.After(l.fetchedAt) {
			latest[s.account] = s
		}
	}
	var diffs []toolHolding
	var unchecked []toolAccount
	for _, id := range slices.Sorted(maps.Keys(accounts)) {
		snap, ok := latest[id]
		if !ok {
			// An account with a snapshot has a latest: this one has rows.
			unchecked = append(unchecked, ix.account(id))
			continue
		}
		held, wrote := map[secRef]int64{}, map[secRef]int64{}
		secs := map[secRef]*toolSecurity{}
		for _, p := range in.positions {
			if p.account != id || p.marketDate != snap.marketDate || !p.fetchedAt.Equal(snap.fetchedAt) || p.key == "" {
				continue
			}
			s := ix.security(p.key, "")
			if s.badMultiplier {
				continue
			}
			long, _ := sharesOf(p.long.Int64, s.multiplier)
			short, _ := sharesOf(p.short.Int64, s.multiplier)
			held[s.ref()] += long - short
			secs[s.ref()] = s
		}
		for _, e := range events {
			if e.account.id != id || e.security == nil || e.kind.shareSign() == 0 || e.day > snap.marketDate {
				continue
			}
			wrote[e.security.ref()] += e.kind.shareSign() * e.shares
			secs[e.security.ref()] = e.security
		}
		// Each of the tool's securities, from the first of the store's of it
		// in the order of their keys, with every one's shares and key.
		list := slices.SortedFunc(maps.Values(secs), func(a, b *toolSecurity) int {
			return cmp.Or(cmp.Compare(a.key, b.key), cmp.Compare(a.symbol, b.symbol))
		})
		same := sameSecurity(in.tool, list)
		var order []secRef
		group := map[secRef]*toolHolding{}
		for _, s := range list {
			g := group[same[s.ref()]]
			if g == nil {
				g = &toolHolding{account: ix.account(id), symbol: s.symbol, snapshot: snap.marketDate}
				group[same[s.ref()]] = g
				order = append(order, same[s.ref()])
			}
			g.snapshotShares += held[s.ref()]
			g.writtenShares += wrote[s.ref()]
			if s.key != "" {
				g.key = cmp.Or(g.key, s.key)
				g.keys = append(g.keys, s.key)
			}
		}
		for _, ref := range order {
			if g := group[ref]; g.snapshotShares != g.writtenShares {
				if len(g.keys) < 2 {
					g.keys = nil
				}
				diffs = append(diffs, *g)
			}
		}
	}
	return diffs, unchecked
}

// A toolIdle is a symbol that Ghostfolio's import asks Yahoo about that
// Yahoo may no longer know (6.6 of the design): the symbol, the least key
// of the store's securities that the records have it for, and the day of
// the last row of any security that Ghostfolio asks Yahoo about by it.
type toolIdle struct {
	symbol, key string
	last        nytime.Date
}

// yahooIdle returns, in their order, the symbols that Ghostfolio asks Yahoo
// about for the records events, that no row that the export selects of a
// security of the symbol has in the year before its last day and that no
// account holds a security of in its latest snapshot: the likeliest to
// have been acquired, renamed or delisted since they were sold, which the
// store learns only from a later row. A symbol is Ghostfolio's: one
// security that the store holds under two keys, a History file's and the
// API's, is one. It is advice, which the manifest lists for you to look up
// before a first import.
func yahooIdle(in *toolInput, events []toolEvent) []toolIdle {
	ix := newToolIndex(in)
	// symbolOf returns Ghostfolio's symbol of the security of key, else of
	// symbol; "" for none. A MANUAL asset's is a UUID, which no symbol for
	// Yahoo is.
	symbolOf := func(key, symbol string) string {
		if s := ix.security(key, symbol); s != nil {
			return s.gfSymbol()
		}
		return ""
	}
	last := map[string]nytime.Date{}
	days := map[rowKey]nytime.Date{}
	for i := range in.rows {
		r := &in.rows[i]
		sym := symbolOf(r.securityKey, r.symbol)
		last[sym] = max(last[sym], r.day)
		days[rowKey{r.account, r.ref}] = r.day
	}
	for _, l := range in.legs {
		sym := symbolOf(l.securityKey, "")
		last[sym] = max(last[sym], days[rowKey{l.account, l.ref}])
	}
	// Each account's latest snapshot: a selected account's from its
	// snapshots, any other's from its positions.
	type snap struct {
		date nytime.Date
		at   time.Time
	}
	latest := map[int64]snap{}
	later := func(id int64, d nytime.Date, at time.Time) {
		if l, ok := latest[id]; d != "" && (in.to == "" || d <= in.to) && (!ok || d > l.date || d == l.date && at.After(l.at)) {
			latest[id] = snap{d, at}
		}
	}
	snapshotted := map[int64]bool{}
	for _, s := range in.snapshots {
		snapshotted[s.account] = true
		later(s.account, s.marketDate, s.fetchedAt)
	}
	for _, p := range in.positions {
		if !snapshotted[p.account] {
			later(p.account, p.marketDate, p.fetchedAt)
		}
	}
	held := map[string]bool{}
	for _, p := range in.positions {
		if l, ok := latest[p.account]; ok && p.marketDate == l.date && p.fetchedAt.Equal(l.at) &&
			(p.long.Valid && p.long.Int64 > 0 || p.short.Valid && p.short.Int64 > 0) {
			held[symbolOf(p.key, "")] = true
		}
	}
	yearBefore := nytime.Date("")
	if t, err := time.Parse(time.DateOnly, string(in.to)); err == nil {
		yearBefore = nytime.Date(t.AddDate(-1, 0, 0).Format(time.DateOnly))
	}
	keys := map[string]string{} // each symbol of the records, to its least key
	for _, e := range events {
		s := e.security
		if s == nil || e.profile != "" || s.manual != "" {
			continue
		}
		if k, ok := keys[s.gfSymbol()]; !ok || k == "" || s.key != "" && s.key < k {
			keys[s.gfSymbol()] = s.key
		}
	}
	var out []toolIdle
	for _, sym := range slices.Sorted(maps.Keys(keys)) {
		if !held[sym] && last[sym] < yearBefore {
			out = append(out, toolIdle{symbol: sym, key: keys[sym], last: last[sym]})
		}
	}
	return out
}

// checkManual checks the symbols that -manual names (in.manual): each must
// name a security of the store, or the symbol of a row without a security
// key, compared in Yahoo's form whatever the case (symbolForm), however
// Schwab, a file or you spell a class share.
// It returns those that name a security that no record of events and no
// price of the export uses (listSecurities), nor any of the store's
// securities that the tools take for one with such a security, whose
// symbols make it MANUAL (gfManual): most are tickers that Yahoo no longer
// serves, whose rows ended, which a chain keeps all the same. A symbol that
// names nothing is a typo, said back masked.
func checkManual(in *toolInput, events []toolEvent) ([]string, error) {
	symbols := map[string][]secRef{} // as -manual compares them (symbolForm), to the securities that have had them
	add := func(sym string, ref secRef) {
		if form := symbolForm(sym); !slices.Contains(symbols[form], ref) {
			symbols[form] = append(symbols[form], ref)
		}
	}
	for _, n := range in.names {
		if n.kind == "symbol" {
			add(n.value, secRef{key: n.key})
		}
	}
	for i := range in.rows {
		if r := &in.rows[i]; r.securityKey == "" && r.symbol != "" {
			add(r.symbol, secRef{symbol: r.symbol})
		}
	}
	used := map[string]bool{} // the symbols (symbolForm) of the securities used and of their groups
	for _, l := range listSecurities(in, events) {
		for _, sym := range l.security.symbols {
			used[symbolForm(sym)] = true
		}
	}
	var unused []string
	for _, v := range in.manual {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if len(symbols[symbolForm(v)]) == 0 {
			return nil, fmt.Errorf("-manual names %s, which no security in the store has", mask.Text(v))
		}
		if !used[symbolForm(v)] {
			unused = append(unused, v)
		}
	}
	return unused, nil
}
