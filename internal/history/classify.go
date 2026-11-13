package history

import (
	"database/sql"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// categories are what an entry can be booked as.
var categories = []string{
	"deposit", "withdrawal", "transfer_in", "transfer_out", "journal",
	"buy", "sell", "dividend", "interest", "fee", "tax", "other",
}

// knownWordings are the descriptions that Schwab's answers have been seen
// to hold, with ~SYM for the symbol after a tilde and # for digits. They
// must agree with schwabDescriptions in internal/schwab's wording_test.go,
// which checks the fixtures against them.
var knownWordings = []string{
	"Dividend Reinvest",
	"Sell Trade",
	"Ordinary Dividend~SYM",
	"W-8 Withholding~SYM",
	"Free Balance Interest Adjustment",
	"Wire Incoming",
	"ACH Credit Received",
	"Funds Received",
	"BANK SWEEP TO BROKERAGE",
	"TRF FDS TO TYPE #",
	"TRF FDS FRM TYPE #",
}

// A sign is the sign of an amount, or unknownSign when the amount is NULL:
// off the grid, so that the store keeps none.
type sign int8

const (
	negative    sign = -1
	zero        sign = 0
	positive    sign = 1
	unknownSign sign = 2
)

// signOf returns the sign of v.
func signOf(v sql.NullInt64) sign {
	switch {
	case !v.Valid:
		return unknownSign
	case v.Int64 < 0:
		return negative
	case v.Int64 > 0:
		return positive
	}
	return zero
}

// A legInput is what the derivation reads of a leg of an API transaction:
// its columns in api_leg.
type legInput struct {
	assetType, symbol      string
	amountMicros           sql.NullInt64 // shares or contracts, or a CURRENCY leg's cash
	amountCents, costCents sql.NullInt64
	price                  sql.NullFloat64
	feeType                string
	positionEffect         string
	security               sql.NullInt64
}

// isSecurity reports whether the leg is of a security rather than of
// cash: a fee, or a cash row's cash.
func (l legInput) isSecurity() bool {
	return l.assetType != string(schwab.AssetCurrency)
}

// legsOf returns legs as the derivation reads them.
func legsOf(legs []legRow) []legInput {
	out := make([]legInput, len(legs))
	for i, l := range legs {
		out[i] = legInput{
			assetType: l.assetType.String, symbol: l.symbol.String,
			amountMicros: l.amountMicros, amountCents: l.amountCents, costCents: l.costCents,
			price: l.price, feeType: l.feeType.String, positionEffect: l.positionEffect.String,
		}
	}
	return out
}

// An apiClass is what the rules for API transactions read of one: its
// type, its status, its wording (its description with the symbol after a
// tilde written ~SYM), its net amount in cents, and of each leg its asset
// type, the sign of its amount, its fee type and its position effect. A
// decision goes stale when this changes (13.7 of the design), and only
// then: not when Schwab adds a key, a closing price changes or a ticker is
// restated.
type apiClass struct {
	typ, status, wording string
	net                  sql.NullInt64
	legs                 []legClass
}

// A legClass is what the rules read of a leg.
type legClass struct {
	assetType      string
	sign           sign
	feeType        string
	positionEffect string
}

// equal reports whether c and d are the same to the rules.
func (c apiClass) equal(d apiClass) bool {
	return c.typ == d.typ && c.status == d.status && c.wording == d.wording && c.net == d.net && slices.Equal(c.legs, d.legs)
}

// classOf returns what the rules read of a transaction of type typ and
// status, whose description, unmasked, is description.
func classOf(typ, status, description string, net sql.NullInt64, legs []legInput) apiClass {
	c := apiClass{typ: typ, status: status, wording: wording(description), net: net}
	for _, l := range legs {
		c.legs = append(c.legs, legClass{assetType: l.assetType, sign: signOf(l.amountMicros), feeType: l.feeType, positionEffect: l.positionEffect})
	}
	return c
}

// recordClass returns what the rules read of t, as the store derives its
// columns and legs from its raw JSON.
func recordClass(t schwab.Transaction) apiClass {
	r, legs := txnFields(t, "")
	return classOf(r.typ.String, r.status.String, t.Description, r.net, legsOf(legs))
}

// wording returns description with the symbol after its last tilde, if it
// names one, written SYM: "Ordinary Dividend~SYM" for "Ordinary
// Dividend~BND".
func wording(description string) string {
	if sym, ok := schwab.SymbolAfterTilde(description); ok {
		return strings.TrimSuffix(description, sym) + "SYM"
	}
	return description
}

// A classification is what a rule makes of a transaction: the rule's
// name, the category and the family its matching uses (15.1 of the
// design), and the flags the rule raises.
type classification struct {
	rule, category, family string
	flags                  flagSet
}

var (
	// dividendWording is "<words> Dividend~SYM", such as "Ordinary
	// Dividend~SYM".
	dividendWording = regexp.MustCompile(`^(?:[A-Za-z][A-Za-z'-]* )+Dividend~SYM$`)
	// transferWording is a transfer between an account's own types, such
	// as "TRF FDS TO TYPE 1".
	transferWording = regexp.MustCompile(`^TRF FDS (?:TO|FRM) TYPE [0-9]+$`)
)

// classifyAPI applies the rules for API transactions (13.3 of the design)
// to c: the first that matches decides. A journal's category is the one it
// takes unpaired, booked by its sign; pairing it with a journal of another
// of your accounts makes it a journal (13.5).
func classifyAPI(c apiClass) classification {
	hasSym := strings.HasSuffix(c.wording, "~SYM")
	var sec []sign // the security legs' signs
	for _, l := range c.legs {
		if l.assetType != string(schwab.AssetCurrency) {
			sec = append(sec, l.sign)
		}
	}
	all := func(s sign) bool {
		return len(sec) > 0 && !slices.ContainsFunc(sec, func(x sign) bool { return x != s })
	}
	netSign := signOf(c.net)
	is := func(rule, category, family string, flags ...string) classification {
		cl := classification{rule: rule, category: category, family: family}
		for _, f := range flags {
			cl.flags.add(f, "")
		}
		return cl
	}
	if !slices.Contains(schwab.AllTransactionTypes(), schwab.TransactionType(c.typ)) {
		cl := is("unknown.type", "other", "other")
		cl.flags.add("unknown_type", c.typ)
		return cl
	}
	switch schwab.TransactionType(c.typ) {
	case schwab.TransactionTrade:
		switch {
		case all(positive) && c.wording == "Dividend Reinvest":
			return is("trade.reinvest", "buy", "buy")
		case all(positive):
			return is("trade.buy", "buy", "buy")
		case all(negative) && c.wording == "Sell Trade":
			return is("trade.sell_fraction", "sell", "sell")
		case all(negative):
			return is("trade.sell", "sell", "sell")
		case len(sec) == 0:
			return is("trade.none", "other", "other", "no_security_leg")
		case !slices.Contains(sec, zero) && !slices.Contains(sec, unknownSign):
			// Legs of both signs: a spread, bought or sold by its net amount.
			switch netSign {
			case negative:
				return is("trade.mixed", "buy", "buy", "mixed_legs")
			case unknownSign:
				return is("trade.mixed", "other", "other", "mixed_legs")
			}
			return is("trade.mixed", "sell", "sell", "mixed_legs")
		case slices.Contains(sec, zero):
			return is("trade.other", "other", "other", "zero_quantity")
		}
		// A leg off the grid, which its inexact flag says.
		return is("trade.other", "other", "other")
	case schwab.TransactionDividendOrInterest:
		unknown := func(rule, category, family string) classification {
			cl := is(rule, category, family)
			cl.flags.add("unknown_wording", mask.Text(c.wording))
			return cl
		}
		switch {
		case dividendWording.MatchString(c.wording):
			return is("doi.dividend", "dividend", "income")
		case hasSym && strings.HasPrefix(c.wording, "Foreign Tax"):
			return unknown("doi.foreign_tax", "tax", "tax")
		case hasSym && strings.HasPrefix(c.wording, "ADR Fee"):
			return unknown("doi.adr_fee", "fee", "fee")
		case c.wording == "Free Balance Interest Adjustment" || strings.HasPrefix(c.wording, "SCHWAB1 INT"):
			return is("doi.interest", "interest", "income")
		case hasSym:
			return unknown("doi.other_sym", "dividend", "income")
		}
		return unknown("doi.other", "interest", "income")
	case schwab.TransactionJournal:
		switch {
		case strings.HasPrefix(c.wording, "W-8 Withholding"):
			return is("journal.w8", "tax", "tax")
		case strings.HasPrefix(c.wording, "BANK SWEEP") || transferWording.MatchString(c.wording):
			return is("journal.sweep", "other", "sweep")
		case len(sec) > 0:
			switch {
			case all(positive):
				return is("journal.shares", "transfer_in", "shares")
			case all(negative):
				return is("journal.shares", "transfer_out", "shares")
			}
			return is("journal.shares", "other", "shares")
		}
		switch netSign {
		case positive:
			return is("journal.cash", "deposit", "journal")
		case negative:
			return is("journal.cash", "withdrawal", "journal")
		case zero:
			return is("journal.cash", "other", "journal", "zero_amount")
		}
		return is("journal.cash", "other", "journal")
	case schwab.TransactionACHReceipt, schwab.TransactionCashReceipt, schwab.TransactionWireIn:
		if netSign == negative {
			return is("cash.in", "withdrawal", "cash", "unexpected_sign")
		}
		return is("cash.in", "deposit", "cash")
	case schwab.TransactionACHDisbursement, schwab.TransactionCashDisbursement, schwab.TransactionWireOut:
		if netSign == positive {
			return is("cash.out", "deposit", "cash", "unexpected_sign")
		}
		return is("cash.out", "withdrawal", "cash")
	case schwab.TransactionElectronicFund:
		if netSign == negative {
			return is("cash.electronic", "withdrawal", "cash")
		}
		return is("cash.electronic", "deposit", "cash")
	}
	// RECEIVE_AND_DELIVER, MEMORANDUM, MARGIN_CALL, MONEY_MARKET and
	// SMA_ADJUSTMENT: kinds that the probes have not settled.
	family := "other"
	if c.typ == string(schwab.TransactionReceiveAndDeliver) {
		family = "holding"
	}
	cl := is("awaits_probe", "other", family)
	var detail string
	switch {
	case strings.HasPrefix(c.wording, "Removed due to Expiration"):
		detail = "expiration?"
	case all(positive):
		detail = "in"
	case all(negative):
		detail = "out"
	}
	cl.flags.add("awaits_probe", detail)
	return cl
}

// A fileClass is what the rules for file transactions read of one: its
// action, normalized (normalAction), whether it has a quantity and that
// quantity's sign once signed by the action (signedQuantity), and its
// Amount in cents, a blank one 0.
type fileClass struct {
	action      string
	hasQuantity bool
	quantity    sign
	amount      sql.NullInt64
}

// normalAction returns an action as the rules compare it: its letters and
// digits only, in lower case, so that "Pr Yr Non-Qual Div" and "Pr Yr Non
// Qual Div" are one.
func normalAction(action string) string {
	var b strings.Builder
	for _, r := range action {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// A fileRule is how the rules for file transactions book an action: its
// family, its category when the action alone gives it, and a note flag.
type fileRule struct {
	family, category, flag string
}

// fileFamilies are the actions of each family (13.4 of the design), as
// History exports write them; every action seen in one is among them.
// The family's category, and the flag that some actions carry, are in
// fileActions.
var fileFamilies = []struct {
	family, category string
	actions          []string
	// flagged are the actions that carry flag.
	flag    string
	flagged []string
}{
	{"buy", "buy", []string{"Buy", "Buy to Open", "Buy to Close", "Buy to Cover", "Reinvest Shares"}, "", nil},
	{"sell", "sell", []string{"Sell", "Sell Short", "Sell to Open", "Sell to Close"}, "", nil},
	{
		"proceeds", "sell",
		[]string{"Cash In Lieu", "Cash Merger", "Full Redemption Adj"},
		"two_row_event",
		[]string{"Cash Merger", "Full Redemption Adj"},
	},
	{"income", "dividend", []string{
		"Qualified Dividend", "Cash Dividend", "Non-Qualified Div", "Special Dividend", "Special Qual Div",
		"Special Non Qual Div", "Pr Yr Cash Div", "Pr Yr Special Div", "Pr Yr Non-Qual Div", "Reinvest Dividend",
		"Qual Div Reinvest", "Pr Yr Div Reinvest", "Long Term Cap Gain", "Short Term Cap Gain",
		"Long Term Cap Gain Reinvest", "Short Term Cap Gain Reinvest", "Div Adjustment", "Qual Div Reinvest Adj",
		"Reinvestment Adj", "Return Of Capital",
	}, "return_of_capital", []string{"Return Of Capital"}},
	{"income", "interest", []string{"Credit Interest", "Bank Interest", "Bond Interest", "Margin Interest", "Interest Adj"}, "", nil},
	{"tax", "tax", []string{
		"NRA Tax Adj", "NRA Withholding", "NRA Withhold", "Pr Yr NRA Tax", "Foreign Tax Paid", "Foreign Tax Reclaim",
		"Foreign Tax Reclaim Adj", "IRS Withhold Adj",
	}, "", nil},
	{"fee", "fee", []string{"ADR Mgmt Fee", "Service Fee", "Advisor Fee", "Wire Fee", "Auto S1 Fee"}, "", nil},
	{"cash", "", []string{
		"MoneyLink Transfer", "MoneyLink Deposit", "MoneyLink Adj", "Wire Sent", "Wire Funds", "Wire Received",
		"Wire Funds Received", "Wire Funds Adj", "Funds Received", "Funds Paid", "Returned Check", "Auto S1 Debit",
		"Auto S1 Credit", "Visa Purchase", "Bank Transfer",
	}, "", nil},
	{"journal", "", []string{"Journal", "Internal Transfer"}, "", nil},
	{"sweep", "other", []string{"Futures MM Sweep"}, "", nil},
	{"shares", "", []string{"Journaled Shares", "Security Transfer", "Stock Plan Activity", "Received"}, "", nil},
	{"holding", "other", []string{
		"Stock Split", "Reverse Split", "Stock Merger", "Name Change", "Conversion", "Spin-off", "Stock Div Dist",
		"Expired", "Assigned", "Exchange or Exercise", "Exercised", "Options Frwd Split", "Options Frwd Split Adj",
		"Cash Merger Adj", "Full Redemption",
	}, "two_row_event", []string{"Cash Merger Adj", "Full Redemption"}},
	{"reversal", "", []string{"Cancel Buy", "Cancel Sell"}, "", nil},
	{"other", "other", []string{"Misc Cash Entry", "Adjustment", "Misc Credits", "Promotional Award", "Schwab Sub Inc Cr"}, "other_action", nil},
}

// fileActions are fileFamilies by normalized action.
var fileActions = func() map[string]fileRule {
	m := map[string]fileRule{}
	for _, f := range fileFamilies {
		for _, a := range f.actions {
			r := fileRule{family: f.family, category: f.category}
			if f.flag != "" && (f.flagged == nil || slices.Contains(f.flagged, a)) {
				r.flag = f.flag
			}
			if _, ok := m[normalAction(a)]; ok {
				panic("history: action " + a + " is in two families")
			}
			m[normalAction(a)] = r
		}
	}
	return m
}()

// signedQuantity returns the quantity q of a file row of action as the
// store keeps it, + in and - out: History writes trades' quantities
// unsigned, so a sale's is made negative and a purchase's positive; a
// Cancel Buy's is negative and a Cancel Sell's positive, opposite to the
// trade they cancel, whatever sign the file wrote; share movements keep the
// sign written.
func signedQuantity(action string, q int64) int64 {
	abs := max(q, -q)
	switch a := normalAction(action); {
	case a == normalAction("Cancel Buy"):
		return -abs
	case a == normalAction("Cancel Sell"):
		return abs
	case fileActions[a].family == "buy":
		return abs
	case fileActions[a].family == "sell":
		return -abs
	}
	return q
}

// classifyFile applies the rules for file transactions (13.4 of the
// design) to c, by its action. Rules are named file.<family>. Like an API
// journal's, a file journal's category is the one it takes unpaired.
func classifyFile(c fileClass) classification {
	r, ok := fileActions[c.action]
	if !ok {
		cl := classification{rule: "file.unknown", category: "other", family: "unknown"}
		cl.flags.add("unknown_action", "")
		return cl
	}
	// Journal, Internal Transfer and Security Transfer move shares when
	// they have a quantity, and cash when they do not.
	switch {
	case c.hasQuantity && (r.family == "journal" || c.action == normalAction("Security Transfer")):
		r = fileRule{family: "shares"}
	case !c.hasQuantity && c.action == normalAction("Security Transfer"):
		r = fileRule{family: "cash"}
	}
	cl := classification{rule: "file." + r.family, category: r.category, family: r.family}
	if r.flag != "" {
		cl.flags.add(r.flag, "")
	}
	byAmount := func() {
		switch signOf(c.amount) {
		case positive:
			cl.category = "deposit"
		case negative:
			cl.category = "withdrawal"
		case zero:
			cl.category = "other"
			cl.flags.add("zero_amount", "")
		default:
			cl.category = "other"
		}
	}
	switch r.family {
	case "cash", "journal":
		byAmount()
	case "shares":
		switch {
		case c.hasQuantity && c.quantity == positive:
			cl.category = "transfer_in"
		case c.hasQuantity && c.quantity == negative:
			cl.category = "transfer_out"
		default:
			cl.category = "other"
		}
	case "reversal":
		cl.category = "buy"
		if c.action == normalAction("Cancel Sell") {
			cl.category = "sell"
		}
	}
	return cl
}
