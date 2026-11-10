package schwab

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A class is what the scrub policy does with the values at a key path, as
// policyTables say.
type class int

const (
	// classMap is for identifiers, such as account numbers: the scrubber
	// puts a fake in place of each, and a fixture holds only fakes.
	classMap class = iota + 1
	// classScale is for money and quantities: the scrubber copies them, or
	// multiplies them by K under scale:K, and each is a number.
	classScale
	// classKeep is for values that name no one, such as enums, times and
	// symbols: each has its grammar's form, and the scrubber copies it, or
	// refuses the file for a value of another form.
	classKeep
	// classPlaceholder is for free text: Schwab's own wording is copied,
	// and any other text is replaced by a numbered placeholder.
	classPlaceholder
)

// A rule is the policy for the values at one key path.
type rule struct {
	path  string   // the key path, as a Problem names one
	keys  []string // path's keys
	class class
	id    identifier // what a mapped value identifies
	gram  *grammar   // the form of a kept value
	text  *freeText  // the wording that free text may hold as it is
}

// onClosedList reports whether raw, a value at r's path, is on the closed
// list of r's enum: Schwab's own word, such as CASH, which names no one.
func (r rule) onClosedList(raw []byte) bool {
	return r.class == classKeep && r.gram.values != nil && r.gram.match(raw)
}

// rulesAt returns r at each of paths.
func rulesAt(r rule, paths []string) []rule {
	rs := make([]rule, len(paths))
	for i, p := range paths {
		rs[i] = r
		rs[i].path, rs[i].keys = p, strings.Split(p, ".")
	}
	return rs
}

// mapAt returns the rules for identifiers of id at paths.
func mapAt(id identifier, paths ...string) []rule {
	return rulesAt(rule{class: classMap, id: id}, paths)
}

// scaleAt returns the rules for money and quantities at paths.
func scaleAt(paths ...string) []rule {
	return rulesAt(rule{class: classScale}, paths)
}

// keepAt returns the rules for values of g's form at paths.
func keepAt(g *grammar, paths ...string) []rule {
	return rulesAt(rule{class: classKeep, gram: g}, paths)
}

// placeholderAt returns the rules for free text that may hold text's
// wording at paths.
func placeholderAt(text *freeText, paths ...string) []rule {
	return rulesAt(rule{class: classPlaceholder, text: text}, paths)
}

// under returns each of names under prefix.
func under(prefix string, names ...string) []string {
	paths := make([]string, len(names))
	for i, n := range names {
		paths[i] = prefix + "." + n
	}
	return paths
}

// A table is the policy for one kind of record: a rule per key path, and
// the tables of the records that it holds at some paths, such as the
// instruments of a transaction's legs.
type table struct {
	rules  []rule
	nested []nestedTable
}

// A nestedTable is the table of the records that another table's records
// hold at one key path.
type nestedTable struct {
	keys  []string
	table *table
}

// rule returns the rule for the values at path, the keys that lead to
// them, and whether the table has one. Keys match in any case, as the
// decoders match keys to fields. A nil table, as of an answer that is not
// JSON, has none.
func (t *table) rule(path []string) (rule, bool) {
	if t == nil {
		return rule{}, false
	}
	for _, r := range t.rules {
		if keysMatch(path, r.keys) {
			return r, true
		}
	}
	for _, n := range t.nested {
		if len(path) > len(n.keys) && keysMatch(path[:len(n.keys)], n.keys) {
			return n.table.rule(path[len(n.keys):])
		}
	}
	return rule{}, false
}

// keysMatch reports whether two key paths are one, their keys compared in
// any case, as encoding/json and the decoders compare a key with a field's.
func keysMatch(a, b []string) bool {
	return slices.EqualFunc(a, b, strings.EqualFold)
}

// An identifier is what a mapped value identifies, such as an account, as a
// report names it. The scrubber gives each one it meets a fake, the same
// one wherever it meets it, and the hygiene tests know the fakes of each.
type identifier string

// The identifiers that the tables map.
const (
	idAccountNumber identifier = "an account number"
	idHash          identifier = "an account's hash"
	idDisplay       identifier = "an account number as Schwab masks it"
	idNickname      identifier = "an account's nickname"
	idActivity      identifier = "an activity ID"
	idOrder         identifier = "an order ID"
	idPosition      identifier = "a position ID"
	idCustomer      identifier = "the streamer's customer ID"
	idCorrelation   identifier = "the streamer's correlation ID"
	idError         identifier = "an error's ID"
	idReference     identifier = "an Akamai Reference"
)

// A grammar is a form that a kept value has. The scrubber copies a value of
// its form and refuses a file that holds one of another, and the hygiene
// tests check each kept value of the fixtures against it.
type grammar struct {
	name   string   // what a value of the form is, such as "a time"
	values []string // an enum's closed list, or nil for a grammar of another kind
	// match reports whether raw, a scalar as JSON sends it, has the form.
	match func(raw []byte) bool
}

// enumOf returns the grammar of an enum: a string that is one of values, in
// the same case.
func enumOf[S ~string](values ...S) *grammar {
	list := make([]string, len(values))
	for i, v := range values {
		list[i] = string(v)
	}
	return &grammar{name: "a value of its closed list", values: list, match: func(raw []byte) bool {
		s, ok := jsonString(raw)
		return ok && slices.Contains(list, s)
	}}
}

// stringGrammar returns the grammar of the strings for which form is true.
func stringGrammar(name string, form func(string) bool) *grammar {
	return &grammar{name: name, match: func(raw []byte) bool {
		s, ok := jsonString(raw)
		return ok && form(s)
	}}
}

// The grammars of the values that the tables keep, but enums.
var (
	timeGrammar   = stringGrammar("a time", isTime)
	symbolGrammar = stringGrammar("a symbol", isSymbol)
	cusipGrammar  = stringGrammar("a CUSIP", cusipForm.MatchString)
	nameGrammar   = stringGrammar("a security's name", nameForm.MatchString)
	urlGrammar    = stringGrammar("an address of Schwab's streamer", streamerURLForm.MatchString)
	codeGrammar   = stringGrammar("a code", codeForm.MatchString)
	numberGrammar = &grammar{name: "a number", match: isJSONNumber}
	boolGrammar   = &grammar{name: "a boolean", match: isJSONBool}
	// statusGrammar is an error's HTTP status: three digits, as a number,
	// or as a string, as the Market Data API sends it and errorList reads
	// it.
	statusGrammar = &grammar{name: "an HTTP status", match: isStatus}
	// javaDateGrammar is a time as Java's Date.toString writes it, as on the
	// Whitelabel page, whose times are not Schwab's instants.
	javaDateGrammar = stringGrammar("a time as Java writes one", isJavaDate)
	// apiURLGrammar is an address of Schwab's API, as Akamai's page quotes
	// the one asked for.
	apiURLGrammar = stringGrammar("an address of Schwab's API", apiURLForm.MatchString)
)

var (
	// timeForm is a time as Schwab writes one: a date, then a time to the
	// second, with a fraction of up to nine digits, and Z or an offset, with
	// a colon or without; or a date alone.
	timeForm = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(?:T[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:?[0-9]{2}))?$`)
	// plainSymbol is the symbol of a stock, a fund or an index, such as
	// AAPL, SWVXX, BRK.B, BRK/B or $SPX.
	plainSymbol = regexp.MustCompile(`^\$?[A-Z][A-Z0-9]{0,5}(?:[./][A-Z]{1,3})?$`)
	// occSymbol is an OCC option symbol: the root, padded with spaces to
	// six characters, the expiration as YYMMDD, C or P, and the strike in
	// thousandths of a dollar, eight digits, as in XYZ   260619P00045000.
	occSymbol = regexp.MustCompile(`^` + occRoot + `([0-9]{6})[CP][0-9]{8}$`)
	// futureSymbol is the symbol of a future, or of an option on one, as
	// Schwab writes it: /ESH25:XCME, /ESH25_P5050:XCME, and .ESH25:XCME for
	// such an option's underlying.
	futureSymbol = regexp.MustCompile(`^[/.][A-Z][A-Z0-9]{0,3}[FGHJKMNQUVXZ][0-9]{2}(?:_[CP][0-9]{1,6}(?:\.[0-9]{1,4})?)?:[A-Z]{4}$`)
	// currencySymbol is cash's symbol, such as CURRENCY_USD.
	currencySymbol = regexp.MustCompile(`^CURRENCY_[A-Z]{3}$`)
	// cusipForm is a CUSIP as Schwab sends one: nine characters, or, for an
	// option, up to sixteen with dots, such as 0XYZ..RI60045000.
	cusipForm = regexp.MustCompile(`^[0-9A-Z.]{9,16}$`)
	// nameForm is a security's name, such as VANGUARD TOTAL STOCK MARKET ETF
	// or E-MINI S&P 500 OPTIONS MAR 25 5050 PUT.
	nameForm = regexp.MustCompile(`^[A-Za-z0-9 &.,/'()%-]{1,64}$`)
	// streamerURLForm is an address of Schwab's streamer: wss or https, on a
	// host under schwab.com, with no port, query or fragment.
	streamerURLForm = regexp.MustCompile(`^(?:wss|https)://(?:[a-z0-9-]+\.)*schwab\.com(?:/[A-Za-z0-9._-]+)*/?$`)
	// apiURLForm is an address of Schwab's API: http or https, on
	// api.schwabapi.com, under the token endpoint's path or one of the APIs'.
	apiURLForm = regexp.MustCompile(`^https?://api\.schwabapi\.com/(?:v1/oauth|trader/v1|marketdata/v1)(?:/[^\s?#]*)?(?:\?[^\s#]*)?$`)
	// codeForm is a code of the streamer's, such as N9 or APIAPP.
	codeForm = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)
	// javaDateForm is a time as Java's Date.toString writes it, EEE MMM dd
	// HH:mm:ss zzz yyyy, such as Mon Nov 02 10:01:02 EST 2026.
	javaDateForm = regexp.MustCompile(`^(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun) (?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) ` +
		`[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} [A-Z]{3,4} [0-9]{4}$`)
	// threeDigits is an HTTP status.
	threeDigits = regexp.MustCompile(`^[0-9]{3}$`)
)

// occRoot is an OCC option symbol's root, padded with spaces to six
// characters.
const occRoot = `(?:[A-Z0-9]{6}|[A-Z0-9]{5} |[A-Z0-9]{4} {2}|[A-Z0-9]{3} {3}|[A-Z0-9]{2} {4}|[A-Z0-9] {5})`

// isTime reports whether s is a time as Schwab writes one, which readTime
// takes without a problem.
func isTime(s string) bool {
	if !timeForm.MatchString(s) {
		return false
	}
	var ps []Problem
	readTime(jsonText(s), "", &ps)
	return len(ps) == 0
}

// isSymbol reports whether s is a symbol: a stock's, a fund's or an
// index's; an OCC option symbol, whose expiration is a date; a future's,
// or an option's on one; cash's; or a CUSIP, which Schwab sends in place of
// the symbol of a delisted security.
func isSymbol(s string) bool {
	if m := occSymbol.FindStringSubmatch(s); m != nil {
		_, err := time.Parse("060102", m[1])
		return err == nil
	}
	return plainSymbol.MatchString(s) || futureSymbol.MatchString(s) || currencySymbol.MatchString(s) || isCUSIP(s)
}

// isCUSIP reports whether s is a CUSIP: eight letters and digits and a
// check digit that agrees with them, as the CUSIP's own algorithm gives it.
func isCUSIP(s string) bool {
	if len(s) != 9 {
		return false
	}
	sum := 0
	for i := range 8 {
		var v int
		switch c := s[i]; {
		case '0' <= c && c <= '9':
			v = int(c - '0')
		case 'A' <= c && c <= 'Z':
			v = int(c-'A') + 10
		default:
			return false
		}
		if i%2 == 1 {
			v *= 2
		}
		sum += v/10 + v%10
	}
	return s[8] == byte('0'+(10-sum%10)%10)
}

// isJavaDate reports whether s is a time as Java's Date.toString writes it.
func isJavaDate(s string) bool {
	_, err := time.Parse("Mon Jan 02 15:04:05 MST 2006", s)
	return err == nil && javaDateForm.MatchString(s)
}

// isJSONNumber reports whether raw is a number as JSON writes one.
func isJSONNumber(raw []byte) bool {
	return len(raw) > 0 && (raw[0] == '-' || '0' <= raw[0] && raw[0] <= '9') && json.Valid(raw)
}

// isJSONBool reports whether raw is true or false.
func isJSONBool(raw []byte) bool {
	return string(raw) == "true" || string(raw) == "false"
}

// isStatus reports whether raw is an HTTP status as Schwab's errors send
// one: three digits, as a number or as a string.
func isStatus(raw []byte) bool {
	if s, ok := jsonString(raw); ok {
		return threeDigits.MatchString(s)
	}
	return threeDigits.Match(raw)
}

// jsonString returns the string that raw, a scalar as JSON sends it, holds,
// and whether it is a string.
func jsonString(raw []byte) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// jsonText returns s as a JSON string.
func jsonText(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}

// The closed lists of the enums that the tables keep: Schwab's spec's, and
// where the spec has none, the values seen. A recording that shows a new
// value is refused until the value is added here by hand.
var (
	accountTypes = enumOf("CASH", "MARGIN")
	assetTypes   = enumOf(AssetEquity, AssetOption, AssetIndex, AssetMutualFund, AssetCashEquivalent, AssetFixedIncome,
		AssetCurrency, AssetCollectiveInvestment, AssetForex, AssetFuture, AssetProduct)
	// instrumentTypes are the kinds within every asset type, together.
	instrumentTypes = enumOf(
		"COMMON_STOCK", "PREFERRED_STOCK", "DEPOSITORY_RECEIPT", "PREFERRED_DEPOSITORY_RECEIPT", "RESTRICTED_STOCK",
		"COMPONENT_UNIT", "RIGHT", "WARRANT", "CONVERTIBLE_PREFERRED_STOCK", "CONVERTIBLE_STOCK", "LIMITED_PARTNERSHIP",
		"WHEN_ISSUED",
		"VANILLA", "BINARY", "BARRIER",
		"UNIT_INVESTMENT_TRUST", "EXCHANGE_TRADED_FUND", "CLOSED_END_FUND", "INDEX", "UNITS",
		"NOT_APPLICABLE", "OPEN_END_NON_TAXABLE", "OPEN_END_TAXABLE", "NO_LOAD_NON_TAXABLE", "NO_LOAD_TAXABLE",
		"SWEEP_VEHICLE", "SAVINGS", "MONEY_MARKET_FUND",
		"BOND_UNIT", "CERTIFICATE_OF_DEPOSIT", "CONVERTIBLE_BOND", "COLLATERALIZED_MORTGAGE_OBLIGATION", "CORPORATE_BOND",
		"GOVERNMENT_MORTGAGE", "GNMA_BONDS", "MUNICIPAL_ASSESSMENT_DISTRICT", "MUNICIPAL_BOND", "OTHER_GOVERNMENT",
		"SHORT_TERM_PAPER", "US_TREASURY_BOND", "US_TREASURY_BILL", "US_TREASURY_NOTE", "US_TREASURY_ZERO_COUPON",
		"AGENCY_BOND", "WHEN_AS_AND_IF_ISSUED_BOND", "ASSET_BACKED_SECURITY",
		"BROAD_BASED", "NARROW_BASED", "STANDARD", "NBBO", "TBD", "UNKNOWN")
	// instrumentStatuses: the spec has none, and only ACTIVE has been seen.
	instrumentStatuses  = enumOf("ACTIVE")
	putCalls            = enumOf("PUT", "CALL", "UNKNOWN")
	currencyTypes       = enumOf("USD", "CAD", "EUR", "JPY")
	transactionTypes    = enumOf(AllTransactionTypes()...)
	transactionStatuses = enumOf(TransactionStatusValid, TransactionStatusInvalid, TransactionStatusPending, TransactionStatusUnknown)
	subAccounts         = enumOf("CASH", "MARGIN", "SHORT", "DIV", "INCOME", "UNKNOWN")
	activityTypes       = enumOf("ACTIVITY_CORRECTION", "EXECUTION", "ORDER_ACTION", "TRANSFER", "UNKNOWN")
	feeTypes            = enumOf("COMMISSION", "SEC_FEE", "STR_FEE", "R_FEE", "CDSC_FEE", "OPT_REG_FEE", "ADDITIONAL_FEE",
		"MISCELLANEOUS_FEE", "FTT", "FUTURES_CLEARING_FEE", "FUTURES_DESK_OFFICE_FEE", "FUTURES_EXCHANGE_FEE",
		"FUTURES_GLOBEX_FEE", "FUTURES_NFA_FEE", "FUTURES_PIT_BROKERAGE_FEE", "FUTURES_TRANSACTION_FEE",
		"LOW_PROCEEDS_COMMISSION", "BASE_CHARGE", "GENERAL_CHARGE", "GST_FEE", "TAF_FEE", "INDEX_OPTION_FEE", "TEFRA_TAX",
		"STATE_TAX", "UNKNOWN")
	positionEffects = enumOf("OPENING", "CLOSING", "AUTOMATIC", "UNKNOWN")
	// preferenceTypes: the spec has none, and only BROKERAGE has been seen,
	// retirement accounts included.
	preferenceTypes     = enumOf("BROKERAGE")
	accountColors       = enumOf("Green", "Blue")
	lotSelectionMethods = enumOf("FIFO", "LIFO", "HIGH_COST", "LOW_COST", "AVERAGE_COST", "SPECIFIC_LOT", "LOSS_HARVESTER")
	// marketDataPermissions: the spec has none, and only NP has been seen.
	marketDataPermissions = enumOf("NP")
	// faultCodes are the gateway's codes, those of testdata/errors.
	faultCodes = enumOf("messaging.adaptors.http.flow.GatewayTimeout", "messaging.adaptors.http.flow.ServiceUnavailable",
		"oauth.v2.InvalidApiKey", "protocol.http.TooBigBody")
	// oauthErrors are the token endpoint's codes, OAuth's and Schwab's own.
	oauthErrors = enumOf("invalid_client", "invalid_grant", "invalid_request", "unsupported_grant_type",
		"unsupported_token_type", "refresh_token_authentication_error", "server_error")
)

// A freeText is a part of Schwab's wording, which free text may hold as it
// is: the scrubber puts the placeholder's word and a number in place of any
// other text, the same number for the same text.
type freeText struct {
	name  string   // what a value is, such as "a description"
	word  string   // the placeholder's word, such as DESCRIPTION
	words []string // the wording, a part of schwabWording
}

// placeholderNumber is the number of a placeholder: 1, 2 and so on.
var placeholderNumber = regexp.MustCompile(`^[1-9][0-9]*$`)

// isWording reports whether s is on the text's part of Schwab's wording,
// matched as onWording matches text with the whole of it.
func (f *freeText) isWording(s string) bool {
	_, ok := onWordingOf(f.words, s)
	return ok
}

// isPlaceholder reports whether s is a placeholder for the text: its word
// and a number.
func (f *freeText) isPlaceholder(s string) bool {
	n, ok := strings.CutPrefix(s, f.word+" ")
	return ok && placeholderNumber.MatchString(n)
}

// The free text that the tables replace: the descriptions of transactions,
// and the messages of errors.
var (
	descriptionText = &freeText{name: "a description", word: "DESCRIPTION", words: schwabDescriptions}
	messageText     = &freeText{name: "a message", word: "MESSAGE", words: schwabMessages}
)

// balanceFigures are the money figures of a set of balances: every field
// of Balances but equityPercentage, a percentage, and isInCall, a flag.
// optionBuyingPower is among them, though no answer seen has held it.
var balanceFigures = []string{
	"accountValue", "accruedInterest", "availableFunds", "availableFundsNonMarginableTrade", "bondValue", "buyingPower",
	"buyingPowerNonMarginableTrade", "cashAvailableForTrading", "cashAvailableForWithdrawal", "cashBalance", "cashCall",
	"cashDebitCallValue", "cashReceipts", "dayTradingBuyingPower", "dayTradingBuyingPowerCall", "dayTradingEquityCall",
	"equity", "intradayBuyingPowerAmount", "liquidationValue", "longMarginValue", "longMarketValue",
	"longNonMarginableMarketValue", "longOptionMarketValue", "longStockValue", "maintenanceCall", "maintenanceRequirement",
	"margin", "marginBalance", "marginEquity", "moneyMarketFund", "mutualFundValue", "optionBuyingPower", "pendingDeposits",
	"regTCall", "savings", "shortBalance", "shortMarginValue", "shortMarketValue", "shortOptionMarketValue",
	"shortStockValue", "sma", "stockBuyingPower", "totalCash", "unsettledCash",
}

// balanceSets are the paths of an account's three sets of balances.
var balanceSets = []string{
	"securitiesAccount.initialBalances", "securitiesAccount.currentBalances", "securitiesAccount.projectedBalances",
}

// inBalances returns each of names in each set of balances.
func inBalances(names ...string) []string {
	var paths []string
	for _, set := range balanceSets {
		paths = append(paths, under(set, names...)...)
	}
	return paths
}

// accountNumbersTable is the policy for the account numbers.
var accountNumbersTable = &table{rules: slices.Concat(
	mapAt(idAccountNumber, "accountNumber"),
	mapAt(idHash, "hashValue"),
)}

// accountTable is the policy for an account: its balances and its
// positions' quantities and values are money, while its positions' average
// prices and percentages are kept, as their instruments are.
var accountTable = &table{
	rules: slices.Concat(
		mapAt(idAccountNumber, "securitiesAccount.accountNumber"),
		scaleAt(inBalances(balanceFigures...)...),
		scaleAt(under("aggregatedBalance", "currentLiquidationValue", "liquidationValue", "intradayBuyingPowerAmount",
			"currentIntradayBuyingPowerAmount")...),
		scaleAt(under("securitiesAccount.positions", "longQuantity", "shortQuantity", "settledLongQuantity",
			"settledShortQuantity", "previousSessionLongQuantity", "previousSessionShortQuantity", "agedQuantity",
			"marketValue", "longOpenProfitLoss", "shortOpenProfitLoss", "maintenanceRequirement", "currentDayProfitLoss",
			"currentDayCost")...),
		keepAt(accountTypes, "securitiesAccount.type"),
		keepAt(numberGrammar, "securitiesAccount.roundTrips"),
		keepAt(numberGrammar, inBalances("equityPercentage")...),
		keepAt(numberGrammar, under("securitiesAccount.positions", "averagePrice", "averageLongPrice", "averageShortPrice",
			"taxLotAverageLongPrice", "taxLotAverageShortPrice", "currentDayProfitLossPercentage")...),
		keepAt(boolGrammar, under("securitiesAccount", "isDayTrader", "isClosingOnlyRestricted", "pfcbFlag",
			"isPortfolioMargin", "isIntradayMargin")...),
		keepAt(boolGrammar, inBalances("isInCall")...),
	),
	nested: []nestedTable{{keys: []string{"securitiesAccount", "positions", "instrument"}, table: instrumentTable}},
}

// transactionTable is the policy for a transaction: its ids are mapped, its
// cash and quantities are money, and its description is free text.
var transactionTable = &table{
	rules: slices.Concat(
		mapAt(idActivity, "activityId"),
		mapAt(idOrder, "orderId"),
		mapAt(idPosition, "positionId"),
		mapAt(idAccountNumber, "accountNumber"),
		scaleAt("netAmount", "transferItems.amount", "transferItems.cost"),
		keepAt(timeGrammar, "time", "tradeDate", "settlementDate"),
		keepAt(transactionTypes, "type"),
		keepAt(transactionStatuses, "status"),
		keepAt(subAccounts, "subAccount"),
		keepAt(activityTypes, "activityType"),
		keepAt(feeTypes, "transferItems.feeType"),
		keepAt(positionEffects, "transferItems.positionEffect"),
		keepAt(boolGrammar, "qualifiedDividend"),
		keepAt(numberGrammar, "transferItems.price"),
		placeholderAt(descriptionText, "description"),
	),
	nested: []nestedTable{{keys: []string{"transferItems", "instrument"}, table: instrumentTable}},
}

// instrumentTable is the policy for an instrument, wherever one is held: it
// is public reference data, kept. An option's deliverable is an instrument
// too. Besides the fields that transactions and positions send, an option
// deliverable's symbol, asset type and currency, which Schwab's spec gives
// a position's options, are kept.
var instrumentTable = func() *table {
	t := &table{rules: slices.Concat(
		keepAt(assetTypes, "assetType", "optionDeliverables.assetType"),
		keepAt(instrumentTypes, "type"),
		keepAt(instrumentStatuses, "status"),
		keepAt(putCalls, "putCall"),
		keepAt(currencyTypes, "optionDeliverables.apiCurrencyType"),
		keepAt(symbolGrammar, "symbol", "uniformSymbol", "underlyingSymbol", "optionDeliverables.rootSymbol",
			"optionDeliverables.symbol"),
		keepAt(cusipGrammar, "cusip", "underlyingCusip"),
		keepAt(nameGrammar, "description"),
		keepAt(numberGrammar, "instrumentId", "netChange", "closingPrice", "strikePrice", "optionPremiumMultiplier",
			"optionDeliverables.strikePercent", "optionDeliverables.deliverableNumber", "optionDeliverables.deliverableUnits"),
		keepAt(timeGrammar, "expirationDate"),
	)}
	t.nested = []nestedTable{{keys: []string{"optionDeliverables", "deliverable"}, table: t}}
	return t
}()

// userPreferenceTable is the policy for the user's preferences: the
// accounts' numbers, nicknames and masked numbers, and the streamer's IDs,
// are mapped.
var userPreferenceTable = &table{rules: slices.Concat(
	mapAt(idAccountNumber, "accounts.accountNumber"),
	mapAt(idNickname, "accounts.nickName"),
	mapAt(idDisplay, "accounts.displayAcctId"),
	mapAt(idCustomer, "streamerInfo.schwabClientCustomerId"),
	mapAt(idCorrelation, "streamerInfo.schwabClientCorrelId"),
	keepAt(preferenceTypes, "accounts.type"),
	keepAt(accountColors, "accounts.accountColor"),
	keepAt(lotSelectionMethods, "accounts.lotSelectionMethod"),
	keepAt(marketDataPermissions, "offers.mktDataPermission"),
	keepAt(boolGrammar, "accounts.primaryAccount", "accounts.autoPositionEffect", "offers.level2Permissions"),
	keepAt(urlGrammar, "streamerInfo.streamerSocketUrl"),
	keepAt(codeGrammar, "streamerInfo.schwabClientChannel", "streamerInfo.schwabClientFunctionId"),
)}

// errorsTable is the policy for Schwab's errors in JSON: the account API's
// message, the Market Data API's errors, the gateway's fault and the token
// endpoint's error. An error's ID is mapped; its status and codes are
// kept; and its words are free text.
var errorsTable = &table{rules: slices.Concat(
	mapAt(idError, "errors.id"),
	keepAt(statusGrammar, "errors.status"),
	keepAt(faultCodes, "fault.detail.errorcode"),
	keepAt(oauthErrors, "error"),
	placeholderAt(messageText, "message", "errors.title", "errors.detail", "fault.faultstring", "error_description"),
)}

// errorsKind is the kind of Schwab's errors, of any route or none.
const errorsKind = "errors"

// policyTables are the scrub policy's tables, by the kind of answer that
// each is for: those that checkAnswer reads, each named after its route, and
// errors.
//
// The policy says what each scalar of an answer may hold, by its key path:
// the keys that lead to it, joined with ".", array elements implicit, as a
// Problem names them, matched in any case, as the decoders match keys to
// fields. It is positive. The scrubber, which makes fixtures of a live
// run's recording, refuses a file that holds a scalar at a path that no
// rule covers, or that is an answer of a kind with no table; and the value
// hygiene tests (hygiene_test.go) hold every fixture to the same tables.
var policyTables = map[string]*table{
	"accountNumbers": accountNumbersTable,
	"accounts":       accountTable,
	"account":        accountTable,
	"transactions":   transactionTable,
	"transaction":    transactionTable,
	"userPreference": userPreferenceTable,
	errorsKind:       errorsTable,
}

// policyKindOf returns the kind of the answer that e names, given its body,
// and whether the policy has a table for it. A 200 answer of a route that
// checkAnswer reads is of that route's kind. An answer that is not a 200 is
// of the kind errors when it has the shape of one of Schwab's errors, as
// errorShaped says, on any path: so is each file of testdata/errors, the
// token endpoint's refusals among them. Any other answer, such as one of
// orders or of market data, is of no kind the policy covers. pages are
// errorPages'.
func policyKindOf(pages []*errorPage, e schwabtest.Entry, body []byte) (string, bool) {
	if e.Response.Status == http.StatusOK {
		route, ok := e.Route()
		if !ok {
			return "", false
		}
		k, ok := kindOfRoute(route)
		return k.name, ok
	}
	if errorShaped(pages, body) {
		return errorsKind, true
	}
	return "", false
}

// errorShaped reports whether body has the shape of one of Schwab's errors:
// JSON whose records, an object or each object of a list, have keys that
// all start a path of the errors table; one of pages, errorPages'; or white
// space, as Schwab's 429 sends.
func errorShaped(pages []*errorPage, body []byte) bool {
	switch schwabtest.ExtensionFor(body) {
	case ".json":
		return errorRecords(body)
	case ".html":
		_, _, ok := matchErrorPage(pages, body)
		return ok
	}
	return isWhiteSpace(body)
}

// errorRecords reports whether body, JSON, is an object, or a list of one
// or more objects, each of whose keys, of which it has at least one, starts
// a path of the errors table, in any case.
func errorRecords(body []byte) bool {
	var records []json.RawMessage
	if b := trimSpace(body); len(b) > 0 && b[0] == '[' {
		if json.Unmarshal(b, &records) != nil || len(records) == 0 {
			return false
		}
	} else {
		records = []json.RawMessage{body}
	}
	for _, r := range records {
		keys, _, ok := jsonMembers(r)
		if !ok || len(keys) == 0 {
			return false
		}
		for _, k := range keys {
			if !slices.ContainsFunc(errorsTable.rules, func(r rule) bool { return strings.EqualFold(r.keys[0], k) }) {
				return false
			}
		}
	}
	return true
}

// isWhiteSpace reports whether body holds nothing but JSON's white space,
// as Schwab's 429 does, or nothing at all.
func isWhiteSpace(body []byte) bool {
	return len(trimSpace(body)) == 0
}

// An errorPage is an HTML page that Schwab's edge or its servers send, as a
// file of testdata/errors holds it. The scrubber writes a page only when it
// is one of these, which it may differ from only in its slots, and the
// hygiene tests hold each page of the fixtures to the same.
type errorPage struct {
	file  string     // under testdata
	slots []pageSlot // the slot of each group of re, in order
	re    *regexp.Regexp
}

// A pageSlot is a part of an error page that differs from one answer to
// another, such as the Reference on Akamai's page. Its rule's path names it,
// as a report does.
type pageSlot struct {
	sample string // the slot's text in the page's file, as written there
	form   string // what the slot may hold as a page writes it, a regexp
	rule   rule   // the policy for the slot's text, its character references unescaped
}

// characterReferences is the form of text written with HTML's character
// references, as Akamai writes its Reference, 18&#46;9efa3f17 and so on.
const characterReferences = `(?:[0-9A-Za-z.]|&#[0-9]{1,7};|&#[xX][0-9A-Fa-f]{1,6};)+`

// errorPages returns the pages of testdata/errors, each with its slots:
// Akamai's Access Denied, whose Reference is mapped and which quotes the
// address asked for, which may hold a hash; and Spring's Whitelabel page,
// whose time is kept.
func errorPages(t testing.TB) []*errorPage {
	t.Helper()
	return []*errorPage{
		newErrorPage(t, "errors/akamai_403.html",
			pageSlot{
				sample: "http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;v1&#47;oauth&#47;token",
				form:   `[^"<>]*`,
				rule:   rule{path: "the address quoted", class: classKeep, gram: apiURLGrammar},
			},
			pageSlot{
				sample: strings.ReplaceAll(schwabtest.AkamaiReference, ".", "&#46;"),
				form:   characterReferences,
				rule:   rule{path: "the Reference", class: classMap, id: idReference},
			}),
		newErrorPage(t, "errors/whitelabel_500.html",
			pageSlot{
				sample: "Mon Nov 02 10:01:02 EST 2026",
				form:   `[^<]*`,
				rule:   rule{path: "the time created", class: classKeep, gram: javaDateGrammar},
			}),
	}
}

// newErrorPage returns the page of file, under testdata, with slots: the
// file made a regexp, each place that holds a slot's sample a group of the
// slot's form, and each line end taking a carriage return before it, since
// a recording keeps the line ends that Schwab sent. A slot whose sample is
// not in the file fails the test.
func newErrorPage(t testing.TB, file string, slots ...pageSlot) *errorPage {
	t.Helper()
	text := string(readTestdata(t, file))
	p := &errorPage{file: file}
	var pattern strings.Builder
	pattern.WriteString("^")
	found := make([]bool, len(slots))
	for {
		at, which := -1, -1
		for i, s := range slots {
			if j := strings.Index(text, s.sample); j >= 0 && (at < 0 || j < at) {
				at, which = j, i
			}
		}
		if at < 0 {
			break
		}
		pattern.WriteString(strings.ReplaceAll(regexp.QuoteMeta(text[:at]), "\n", `\r?\n`))
		pattern.WriteString("(" + slots[which].form + ")")
		p.slots = append(p.slots, slots[which])
		found[which] = true
		text = text[at+len(slots[which].sample):]
	}
	pattern.WriteString(strings.ReplaceAll(regexp.QuoteMeta(text), "\n", `\r?\n`) + "$")
	for i, ok := range found {
		if !ok {
			t.Fatalf("%s does not hold the sample of its slot %s", file, slots[i].rule.path)
		}
	}
	p.re = regexp.MustCompile(pattern.String())
	return p
}

// match returns the text of each of the page's slots in body, as body
// writes it, and whether body is the page.
func (p *errorPage) match(body []byte) ([]string, bool) {
	m := p.re.FindSubmatch(body)
	if m == nil {
		return nil, false
	}
	texts := make([]string, len(m)-1)
	for i, g := range m[1:] {
		texts[i] = string(g)
	}
	return texts, true
}

// matchErrorPage returns the page of pages that body is, with the text of
// each of its slots, and whether there is one.
func matchErrorPage(pages []*errorPage, body []byte) (*errorPage, []string, bool) {
	for _, p := range pages {
		if texts, ok := p.match(body); ok {
			return p, texts, true
		}
	}
	return nil, nil, false
}

// eachScalar calls f with each scalar within n, a JSON value at path, and
// the object that holds it, or nil for an element of an array or for n
// itself. path is the keys that lead to the scalar, as sent, array
// elements implicit.
func eachScalar(n *node, path []string, in *node, f func(path []string, raw []byte, in *node)) {
	switch {
	case n.raw != nil:
		f(path, n.raw, in)
	case n.array:
		for _, v := range n.vals {
			eachScalar(v, path, nil, f)
		}
	default:
		for i, v := range n.vals {
			eachScalar(v, append(slices.Clip(path), n.keys[i]), n, f)
		}
	}
}

// A recordField is a scalar field of a record: the keys that lead to it and
// its type.
type recordField struct {
	keys []string
	typ  reflect.Type
}

// scalarFields returns the scalar fields of t, a record's type, and of the
// records it holds, at any depth, through slices and pointers: each string,
// number, boolean and time, under the keys that lead to it, after prefix.
// The record's own Problems and Raw are not Schwab's, and are left out.
func scalarFields(t reflect.Type, prefix []string) []recordField {
	t = derefType(t)
	switch {
	case t.Kind() == reflect.Slice:
		return scalarFields(t.Elem(), prefix)
	case t.Kind() != reflect.Struct || t == timeType:
		return []recordField{{keys: prefix, typ: t}}
	}
	var fs []recordField
	for f := range t.Fields() {
		name := jsonName(f)
		if name == "" || name == "problems" {
			continue
		}
		fs = append(fs, scalarFields(f.Type, append(slices.Clip(prefix), name))...)
	}
	return fs
}

// isNumeric reports whether t, or what it points to, is a number.
func isNumeric(t reflect.Type) bool {
	switch derefType(t).Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// unclassifiedNumbers returns the paths of the numeric fields of records of
// type t that tab has no rule for, or a rule that does not take a number:
// one that is free text, or that keeps a form other than a number's.
func unclassifiedNumbers(t reflect.Type, tab *table) []string {
	var paths []string
	for _, f := range scalarFields(t, nil) {
		if !isNumeric(f.typ) {
			continue
		}
		r, ok := tab.rule(f.keys)
		if !ok || r.class == classPlaceholder || r.class == classKeep && r.gram != numberGrammar {
			paths = append(paths, strings.Join(f.keys, "."))
		}
	}
	return paths
}

// Every number of every record has a rule that is map, scale or keep as a
// number: so the scrubber, under scale:K, refuses no number for want of a
// rule and writes no money figure unscaled. The three ids are mapped, and
// a set of balances' optionBuyingPower, which no answer seen has held, is
// money. A table without one of its rules, or with one of the wrong class,
// leaves that number out.
func TestScrubPolicyCoversEveryNumber(t *testing.T) {
	numbers := 0
	for _, k := range answerKinds {
		for _, f := range scalarFields(k.typ, nil) {
			if isNumeric(f.typ) {
				numbers++
			}
		}
		if paths := unclassifiedNumbers(k.typ, policyTables[k.name]); len(paths) > 0 {
			t.Errorf("%s: numbers with no rule of map, scale or keep as a number: %q", k.name, paths)
		}
	}
	if numbers < 100 {
		t.Errorf("only %d numbers in the records", numbers)
	}
	for kind, paths := range map[string]map[string]class{
		"accounts": {
			"securitiesAccount.currentBalances.optionBuyingPower":                        classScale,
			"securitiesAccount.positions.agedQuantity":                                   classScale,
			"securitiesAccount.positions.averagePrice":                                   classKeep,
			"securitiesAccount.positions.instrument.optionDeliverables.deliverableUnits": classKeep,
		},
		"transaction": {
			"activityId": classMap, "orderId": classMap, "positionId": classMap, "netAmount": classScale,
			"transferItems.price": classKeep, "transferItems.instrument.optionDeliverables.deliverable.instrumentId": classKeep,
		},
	} {
		for path, want := range paths {
			if r, ok := policyTables[kind].rule(strings.Split(path, ".")); !ok || r.class != want {
				t.Errorf("%s: %s has a rule %v of class %d, want class %d", kind, path, ok, r.class, want)
			}
		}
	}

	// A table that has lost a rule, or has one that does not take a
	// number, leaves the number out.
	kept := slices.DeleteFunc(slices.Clone(accountTable.rules), func(r rule) bool {
		return r.path == "securitiesAccount.projectedBalances.optionBuyingPower"
	})
	broken := &table{rules: slices.Concat(keepAt(symbolGrammar, "securitiesAccount.roundTrips"),
		placeholderAt(descriptionText, "securitiesAccount.positions.marketValue"), kept), nested: accountTable.nested}
	if got, want := unclassifiedNumbers(reflect.TypeFor[Account](), broken), []string{
		"securitiesAccount.roundTrips", "securitiesAccount.positions.marketValue",
		"securitiesAccount.projectedBalances.optionBuyingPower",
	}; !slices.Equal(got, want) {
		t.Errorf("a broken table leaves out %q, want %q", got, want)
	}
}

// rulesOf returns the rules of t and of the tables nested in it, each at
// its path within t's records, after prefix. Each table is followed once,
// so that an instrument's deliverable, an instrument too, is not.
func rulesOf(t *table, prefix []string, seen map[*table]bool) []rule {
	seen[t] = true
	var rs []rule
	for _, r := range t.rules {
		r.keys = slices.Concat(prefix, r.keys)
		r.path = strings.Join(r.keys, ".")
		rs = append(rs, r)
	}
	for _, n := range t.nested {
		if !seen[n.table] {
			rs = append(rs, rulesOf(n.table, slices.Concat(prefix, n.keys), seen)...)
		}
	}
	return rs
}

// misfit says what is wrong with r as the rule for a field of type t, or ""
// if it fits: a mapped field holds a string or a whole number, a scaled one
// a number with a fraction, a kept one what its grammar takes, and free
// text a string.
func misfit(r rule, t reflect.Type) string {
	t = derefType(t)
	isString := t.Kind() == reflect.String
	isFloat := t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64
	isWhole := isNumeric(t) && !isFloat
	switch r.class {
	case classMap:
		if !isString && !isWhole {
			return "mapped, though neither a string nor a whole number"
		}
	case classScale:
		if !isFloat {
			return "scaled, though not a number with a fraction"
		}
	case classKeep:
		fits := isString
		switch r.gram {
		case numberGrammar:
			fits = isWhole || isFloat
		case boolGrammar:
			fits = t.Kind() == reflect.Bool
		case timeGrammar:
			fits = t == timeType
		}
		if !fits {
			return "kept as " + r.gram.name + ", which it does not hold"
		}
	case classPlaceholder:
		if !isString {
			return "free text, though not a string"
		}
	}
	return ""
}

// Each kind's table fits its records: every scalar field of the record has
// a rule that takes what the field holds, so that no value that a record
// holds is refused for want of one; every rule names a field; no path has
// two rules; and no enum lists a value twice. The errors table names only
// paths that the errors of testdata/errors hold.
func TestScrubPolicyFitsTheRecords(t *testing.T) {
	for _, k := range answerKinds {
		tab := policyTables[k.name]
		for _, f := range scalarFields(k.typ, nil) {
			path := strings.Join(f.keys, ".")
			if r, ok := tab.rule(f.keys); !ok {
				t.Errorf("%s: %s has no rule", k.name, path)
			} else if what := misfit(r, f.typ); what != "" {
				t.Errorf("%s: %s, of type %s, is %s", k.name, path, f.typ, what)
			}
		}
		for _, r := range rulesOf(tab, nil, map[*table]bool{}) {
			if _, _, ok := fieldAt(k.typ, r.keys); !ok {
				t.Errorf("%s: no field takes %s, which has a rule", k.name, r.path)
			}
		}
	}
	for name, tab := range map[string]*table{
		"accountNumbers": accountNumbersTable, "accounts": accountTable, "transactions": transactionTable,
		"instruments": instrumentTable, "userPreference": userPreferenceTable, "errors": errorsTable,
	} {
		for i, r := range tab.rules {
			for _, other := range tab.rules[i+1:] {
				if keysMatch(r.keys, other.keys) {
					t.Errorf("%s: %s has two rules", name, r.path)
				}
			}
			if r.gram != nil && len(slices.Compact(slices.Sorted(slices.Values(r.gram.values)))) != len(r.gram.values) {
				t.Errorf("%s: the enum of %s lists a value twice", name, r.path)
			}
		}
	}
	held := map[string]bool{}
	for _, e := range schwabtest.Index() {
		if b := readTestdata(t, e.File); strings.HasPrefix(e.File, "errors/") && schwabtest.ExtensionFor(b) == ".json" {
			eachScalar(parseNode(b), nil, nil, func(path []string, _ []byte, _ *node) { held[strings.Join(path, ".")] = true })
		}
	}
	for _, r := range errorsTable.rules {
		if !held[r.path] {
			t.Errorf("errors: no error of testdata/errors holds %s, which has a rule", r.path)
		}
	}

	// A rule that takes what its field does not hold is found.
	for _, tc := range []struct {
		r   rule
		typ reflect.Type
	}{
		{rule{class: classMap}, reflect.TypeFor[float64]()},
		{rule{class: classScale}, reflect.TypeFor[int64]()},
		{rule{class: classKeep, gram: numberGrammar}, reflect.TypeFor[string]()},
		{rule{class: classKeep, gram: boolGrammar}, reflect.TypeFor[*float64]()},
		{rule{class: classKeep, gram: timeGrammar}, reflect.TypeFor[string]()},
		{rule{class: classKeep, gram: symbolGrammar}, reflect.TypeFor[time.Time]()},
		{rule{class: classPlaceholder}, reflect.TypeFor[bool]()},
	} {
		if misfit(tc.r, tc.typ) == "" {
			t.Errorf("a rule of class %d, %v, fits a field of type %s", tc.r.class, tc.r.gram, tc.typ)
		}
	}
}

// Each grammar takes the values of its form, as JSON sends them, and no
// others.
func TestGrammars(t *testing.T) {
	for _, tc := range []struct {
		g              *grammar
		takes, refuses []string
	}{
		{timeGrammar, []string{
			`"2024-04-04T15:56:07+0000"`, `"2024-04-04T15:56:07.000+0000"`, `"2024-04-04T15:56:07Z"`, `"2024-04-04T15:56:07.000Z"`,
			`"2024-04-04T11:56:07.000-04:00"`, `"2024-04-04T11:56:07-0400"`, `"2024-04-04"`, `"2024-04-04T15:56:07.123456789Z"`,
		}, []string{
			`"2024-04-04T15:56:07"`, `"2024-04-04T5:56:07Z"`, `"2024-13-04T15:56:07Z"`, `"2024-02-30"`, `"2024-04-04T24:00:00Z"`,
			`"2024-04-04T15:56:07+2400"`, `"2024-04-04T15:56:07.1234567890Z"`, `"2024-04-04 15:56:07Z"`, `"2024-04-04T15:56:07,5Z"`,
			`"Mon Nov 02 10:01:02 EST 2026"`, `"29470135"`, `1712246167`, `null`, `""`,
		}},
		{symbolGrammar, []string{
			`"AAPL"`, `"SWVXX"`, `"BRK.B"`, `"BRK/B"`, `"$SPX"`, `"XYZ   260619P00045000"`, `"SPXW  260619C05000000"`,
			`"AAPL1 260619C00180000"`, `"/ESH25:XCME"`, `"/ESH25_P5050:XCME"`, `".ESH25:XCME"`, `"/ZBZ25_C112.5:XCBT"`,
			`"CURRENCY_USD"`, `"00507V109"`,
		}, []string{
			`"aapl"`, `"ACME BANK"`, `"SEVENCH"`, `"XYZ  260619P00045000"`, `"XYZ   261319P00045000"`, `"XYZ   260619Q00045000"`,
			`"/ESH25"`, `"/ES25:XCME"`, `"CURRENCY_"`, `"00507V108"`, `"29470135"`, `"294701351"`, `""`, `5`, `null`,
		}},
		{
			cusipGrammar,
			[]string{`"922908769"`, `"30303M102"`, `"0XYZ..RI60045000"`},
			[]string{`"92290876"`, `"0XYZ..RI600450001"`, `"92290876a"`, `"9229 0876"`, `""`, `922908769`},
		},
		{nameGrammar, []string{
			`"VANGUARD TOTAL STOCK MARKET ETF"`, `"E-MINI S&P 500 OPTIONS MAR 25 5050 PUT"`, `"USD currency"`,
			`"XYZ 06/19/2026 45.00 P"`, `"O'REILLY (ADR) 5% PFD, CL. A"`,
		}, []string{`""`, `"` + strings.Repeat("A", 65) + `"`, `"Wire; ref 1"`, `"Café"`, `"a\nb"`, `5`}},
		{
			urlGrammar,
			[]string{`"wss://streamer-api.schwab.com/ws"`, `"https://schwab.com"`, `"wss://a.b.schwab.com/x/y.z/"`},
			[]string{
				`"ws://streamer-api.schwab.com/ws"`, `"wss://streamer-api.schwab.com.example.com/ws"`, `"wss://evilschwab.com/ws"`,
				`"wss://streamer-api.schwab.com:443/ws"`, `"wss://streamer-api.schwab.com/ws?id=1"`, `"wss://u@schwab.com/ws"`,
				`"wss://streamer-api.schwab.com/ws#a"`, `""`,
			},
		},
		{codeGrammar, []string{`"N9"`, `"IO"`, `"APIAPP"`}, []string{`"A"`, `"TOOLONGCODE12"`, `"apiapp"`, `"N-9"`, `9`}},
		{
			numberGrammar,
			[]string{`0`, `-0.0`, `1.5`, `1.0E-4`, `81.29223999999999`, `-310.0`},
			[]string{`"5"`, `true`, `null`, `-`, `01`, `1.`, `.5`, `+1`},
		},
		{boolGrammar, []string{`true`, `false`}, []string{`"true"`, `1`, `null`, `True`}},
		{statusGrammar, []string{`401`, `"400"`}, []string{`"4000"`, `40`, `"x"`, `4.01e2`, `401.0`, `"40a"`}},
		{
			javaDateGrammar,
			[]string{`"Mon Nov 02 10:01:02 EST 2026"`, `"Tue Jan 05 00:00:00 UTC 2027"`},
			[]string{
				`"Mon Nov 32 10:01:02 EST 2026"`, `"Mon Nov 2 10:01:02 EST 2026"`, `"2026-11-02T10:01:02Z"`, `"Mon Nov 02 10:01:02 2026"`,
				`"Mon Nov 02 10:01:02 ChST 2026"`, `"Mon Nov 02 10:01:02 GMT+05 2026"`,
			},
		},
		{apiURLGrammar, []string{
			`"http://api.schwabapi.com/v1/oauth/token"`, `"https://api.schwabapi.com/trader/v1/accounts/ABC/transactions?types=TRADE"`,
			`"https://api.schwabapi.com/marketdata/v1/quotes"`,
		}, []string{
			`"https://api.schwabapi.com.example.com/v1/oauth/token"`, `"https://example.com/trader/v1/"`,
			`"https://api.schwabapi.com/other"`, `"ftp://api.schwabapi.com/v1/oauth/token"`, `"https://api.schwabapi.com/trader/v1/a b"`,
		}},
		{enumOf("CASH", "MARGIN"), []string{`"CASH"`, `"MARGIN"`}, []string{`"cash"`, `"IRA"`, `"CASH "`, `5`, `null`}},
	} {
		for _, raw := range tc.takes {
			if !tc.g.match([]byte(raw)) {
				t.Errorf("%s does not take %s", tc.g.name, raw)
			}
		}
		for _, raw := range tc.refuses {
			if tc.g.match([]byte(raw)) {
				t.Errorf("%s takes %s", tc.g.name, raw)
			}
		}
	}
}

// policyEntry returns an index entry of an answer to method and path with
// status.
func policyEntry(method, path string, status int) schwabtest.Entry {
	var e schwabtest.Entry
	e.Request.Method, e.Request.Path, e.Response.Status = method, path, status
	return e
}

// Each fixture is of its route's kind, or of errors: the account routes'
// 200 answers by route, a recording's too, and every file of
// testdata/errors and every other answer of a recording, the pages and the
// 429's white space among them, on any path. A 200 of another route,
// or an answer that is not a 200 and has no shape of Schwab's errors, is of
// no kind.
func TestPolicyKinds(t *testing.T) {
	pages := errorPages(t)
	routes := map[string]string{
		"account_numbers.json": "accountNumbers", "accounts.json": "accounts", "transactions.json": "transactions",
		"transaction.json": "transaction", "user_preference.json": "userPreference",
	}
	for _, e := range schwabtest.Index() {
		want, ok := routes[e.File]
		switch {
		case e.Source == "recorded":
			// A recording is of the kind the scrubber took it for: of its
			// route's, for a 200, and else of errors.
			want, ok = errorsKind, true
			if route, _ := e.Route(); e.Response.Status == http.StatusOK {
				k, _ := kindOfRoute(route)
				want = k.name
			}
		case !ok && strings.HasPrefix(e.File, "errors/"):
			want, ok = errorsKind, true
		}
		if got, has := policyKindOf(pages, e, readTestdata(t, e.File)); got != want || has != ok {
			t.Errorf("%s is of kind %q, %v; want %q, %v", e.File, got, has, want, ok)
		}
	}
	hash := schwabtest.FixtureAccounts[0].Hash
	akamai := string(readTestdata(t, "errors/akamai_403.html"))
	for _, tc := range []struct {
		e    schwabtest.Entry
		body string
		want string // "" for no kind
	}{
		{policyEntry("GET", "/trader/v1/accounts/"+hash, 200), `{}`, "account"},
		{policyEntry("GET", "/trader/v1/accounts/"+hash+"/orders", 200), `[]`, ""},
		{policyEntry("GET", "/marketdata/v1/quotes", 200), `{}`, ""},
		{policyEntry("GET", "/trader/v1/test", 200), `{"ok":true}`, ""},
		{policyEntry("GET", "/marketdata/v1/quotes", 401), string(readTestdata(t, "errors/not_authorized_401.json")), errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 400), `[{"message":"x"}]`, errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 400), `{"Message":"x"}`, errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 400), `{"message":"x","errors":[{"title":"y"}]}`, errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 400), `{"message":"x","accountNumber":"1"}`, ""},
		{policyEntry("GET", "/trader/v1/accounts", 400), `[]`, ""},
		{policyEntry("GET", "/trader/v1/accounts", 400), `{}`, ""},
		{policyEntry("GET", "/trader/v1/accounts", 400), `[{"message":"x"},5]`, ""},
		{policyEntry("GET", "/trader/v1/accounts", 400), `"Invalid account number"`, ""},
		{policyEntry("GET", "/trader/v1/accounts", 429), " \n", errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 401), "", errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 403), strings.ReplaceAll(akamai, "\n", "\r\n"), errorsKind},
		{policyEntry("GET", "/trader/v1/accounts", 502), "<html><body>Bad Gateway</body></html>", ""},
		{policyEntry("GET", "/trader/v1/accounts", 500), "Internal Server Error", ""},
		{policyEntry("POST", "/v1/oauth/token", 400), `{"error":"invalid_grant","error_description":"x"}`, errorsKind},
	} {
		got, ok := policyKindOf(pages, tc.e, []byte(tc.body))
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s %s, %d %q: of kind %q, %v; want %q", tc.e.Request.Method, tc.e.Request.Path, tc.e.Response.Status, tc.body, got, ok, tc.want)
		}
	}
}

// Each error page is its file made a template: the file is the page, with
// its slots' samples as their texts; a page that differs from the file only
// in a slot, or in line ends of CR and LF, is the page, with the slot's
// text as written; and one that differs anywhere else is no page.
func TestErrorPages(t *testing.T) {
	pages := errorPages(t)
	for _, p := range pages {
		texts, ok := p.match(readTestdata(t, p.file))
		samples := make([]string, len(p.slots))
		for i, s := range p.slots {
			samples[i] = s.sample
		}
		if !ok || !slices.Equal(texts, samples) {
			t.Errorf("%s is the page %v, with %q; want %q", p.file, ok, texts, samples)
		}
	}
	if len(pages) != 2 || len(pages[0].slots) != 3 || len(pages[1].slots) != 1 {
		t.Fatalf("%d pages, want Akamai's, with the address and the Reference twice, and the Whitelabel page, with its time", len(pages))
	}
	akamai := string(readTestdata(t, "errors/akamai_403.html"))
	whitelabel := string(readTestdata(t, "errors/whitelabel_500.html"))
	asked, reference := pages[0].slots[0].sample, pages[0].slots[1].sample
	other := "77&#46;0a1b2c3d&#46;1792539138&#46;5e6f"
	account := "https&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;trader&#47;v1&#47;accounts&#47;" + schwabtest.FixtureAccounts[0].Hash
	for _, tc := range []struct {
		name, page string
		file       string   // the file of the page it is, or "" for none
		want       []string // the slots' texts
	}{
		{"another Reference", strings.ReplaceAll(akamai, reference, other), pages[0].file, []string{asked, other, other}},
		{"an account's address", strings.Replace(akamai, asked, account, 1), pages[0].file, []string{account, reference, reference}},
		{"line ends of CR and LF", strings.ReplaceAll(akamai, "\n", "\r\n"), pages[0].file, []string{asked, reference, reference}},
		{
			"another time", strings.Replace(whitelabel, "Mon Nov 02 10:01:02 EST 2026", "Fri Jan 08 23:59:59 EDT 2027", 1),
			pages[1].file,
			[]string{"Fri Jan 08 23:59:59 EDT 2027"},
		},
		{"another title", strings.Replace(akamai, "<TITLE>Access Denied</TITLE>", "<TITLE>Denied</TITLE>", 1), "", nil},
		{"a quote in the address", strings.Replace(akamai, asked, asked+`"x`, 1), "", nil},
		{"a space in the Reference", strings.ReplaceAll(akamai, reference, "18 9efa3f17"), "", nil},
		{"more after the page", akamai + "<P>more</P>", "", nil},
		{"another status", strings.Replace(whitelabel, "status=500", "status=502", 1), "", nil},
	} {
		p, texts, ok := matchErrorPage(pages, []byte(tc.page))
		file := ""
		if ok {
			file = p.file
		}
		if ok != (tc.file != "") || file != tc.file || !slices.Equal(texts, tc.want) {
			t.Errorf("%s: the page of %q, %v, with %q; want that of %q, with %q", tc.name, file, ok, texts, tc.file, tc.want)
		}
	}
}

// The free text of each kind is its part of Schwab's wording, and its
// placeholder is its word and a number from 1: a description is not a
// message, nor DESCRIPTION 3 a message's placeholder.
func TestFreeText(t *testing.T) {
	for _, tc := range []struct {
		text                     *freeText
		wording, placeholder, no []string
	}{
		{
			descriptionText,
			[]string{"Dividend Reinvest", "W-8 Withholding~BND"},
			[]string{"DESCRIPTION 1", "DESCRIPTION 30"},
			[]string{"Invalid account number", "MESSAGE 1", "DESCRIPTION 0", "DESCRIPTION 03", "DESCRIPTION x", "DESCRIPTION", "description 1", "DESCRIPTION 1 "},
		},
		{
			messageText,
			[]string{"Invalid account number", "Gateway Timeout"},
			[]string{"MESSAGE 1", "MESSAGE 12"},
			[]string{"Dividend Reinvest", "DESCRIPTION 1", "MESSAGE -1", "MESSAGE", "MESSAGE 1.5"},
		},
	} {
		for _, s := range tc.wording {
			if !tc.text.isWording(s) || tc.text.isPlaceholder(s) {
				t.Errorf("%s: %q is wording %v and a placeholder %v; want wording alone", tc.text.name, s, tc.text.isWording(s), tc.text.isPlaceholder(s))
			}
		}
		for _, s := range tc.placeholder {
			if tc.text.isWording(s) || !tc.text.isPlaceholder(s) {
				t.Errorf("%s: %q is wording %v and a placeholder %v; want a placeholder alone", tc.text.name, s, tc.text.isWording(s), tc.text.isPlaceholder(s))
			}
		}
		for _, s := range tc.no {
			if tc.text.isWording(s) || tc.text.isPlaceholder(s) {
				t.Errorf("%s: %q is wording %v and a placeholder %v; want neither", tc.text.name, s, tc.text.isWording(s), tc.text.isPlaceholder(s))
			}
		}
	}
}
