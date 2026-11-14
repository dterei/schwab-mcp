package history

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The checker and the model of Ghostfolio's import, for the tests of the
// export for Ghostfolio. gfCheck reads ghostfolio.json as the import of
// Ghostfolio 3.99.0 validates it, with the export's own conventions;
// gfModel imports files one after another into a user, as that import
// does, and says what the user then holds and what Ghostfolio's calculator
// makes of its fees and dividends (11.4 of the design). Both follow
// Ghostfolio's code as read, never run: the DTOs of libs/common/src/lib/dtos
// (create-order, create-account, create-account-with-balances,
// account-balance, create-asset-profile and its with-market-data form),
// apps/api/src/services/import/import.service.ts, the data provider's
// validateActivities, and apps/api/src/app/portfolio/calculator/
// portfolio-calculator.ts with the current-rate service. They read the
// written file, never the records that it was written from, and share no
// code with the writer: only which activities are deliveries, which the
// file cannot show, comes from the records (gfDeliveries).

// A gfObject is a JSON object of the file, as Ghostfolio's import gets it:
// numbers as their text, json.Number.
type gfObject = map[string]any

// The keys that Ghostfolio's DTOs declare: its validation refuses any
// other ("property X should not exist").
var (
	gfFileKeys     = []string{"accounts", "activities", "assetProfiles", "platforms", "tags"}
	gfAccountKeys  = []string{"balance", "balances", "comment", "currency", "id", "isExcluded", "name", "platformId", "tags"}
	gfBalanceKeys  = []string{"comment", "date", "value"}
	gfProfileKeys  = []string{"assetClass", "assetSubClass", "comment", "countries", "currency", "cusip", "dataSource", "figi", "figiComposite", "figiShareClass", "holdings", "isActive", "isin", "marketData", "name", "sectors", "symbol", "url"}
	gfActivityKeys = []string{"accountId", "assetClass", "assetSubClass", "comment", "currency", "customCurrency", "dataSource", "date", "fee", "quantity", "symbol", "tags", "type", "unitPrice", "updateAccountBalance"}
	// gfPriceKeys are the keys of a profile's price. Ghostfolio does not
	// validate prices; the export writes these two.
	gfPriceKeys = []string{"date", "marketPrice"}
)

// Ghostfolio's enums (prisma/schema.prisma), and the data sources that an
// instance enables unless its DATA_SOURCES says otherwise.
var (
	gfTypes          = []string{"BUY", "DIVIDEND", "FEE", "INTEREST", "LIABILITY", "SELL"}
	gfAssetClasses   = []string{"ALTERNATIVE_INVESTMENT", "COMMODITY", "EQUITY", "FIXED_INCOME", "LIQUIDITY", "REAL_ESTATE"}
	gfAssetSubClass  = []string{"BOND", "CASH", "COLLECTIBLE", "COMMODITY", "CRYPTOCURRENCY", "ETF", "LOAN", "MUTUALFUND", "PRECIOUS_METAL", "PRIVATE_EQUITY", "STOCK"}
	gfDataSources    = []string{"ALPHA_VANTAGE", "COINGECKO", "EOD_HISTORICAL_DATA", "FINANCIAL_MODELING_PREP", "GHOSTFOLIO", "GOOGLE_SHEETS", "MANUAL", "RAPID_API", "YAHOO"}
	gfEnabledSources = []string{"COINGECKO", "MANUAL", "YAHOO"}
	// gfNonInvestment are the types that Ghostfolio checks with no data
	// provider and makes MANUAL without a data source.
	gfNonInvestment = []string{"FEE", "INTEREST", "LIABILITY"}
	// gfCashNames are the names of the export's cash profiles.
	gfCashNames = []string{"Schwab fees", "Schwab interest", "Schwab taxes"}
)

// Ghostfolio's lengths (libs/common/src/lib/config.ts) and its test of a
// custom asset's symbol: a UUID of any version from 1 to 8, nil or max, by
// validator's isUUID, or Ghostfolio's GF_ prefix (helper.ts).
const (
	gfCommentMax = 2000
	gfSymbolMax  = 255
)

var gfUUID = regexp.MustCompile(`(?i)^(?:[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}|00000000-0000-0000-0000-000000000000|ffffffff-ffff-ffff-ffff-ffffffffffff)$`)

// gfCustomSymbol reports whether Ghostfolio takes symbol for a custom
// (MANUAL) asset.
func gfCustomSymbol(symbol string) bool {
	return gfUUID.MatchString(symbol) || strings.HasPrefix(symbol, "GF_")
}

// The forms in which the export writes numbers and dates (7 of the
// design): money as dollars and cents, a balance signed; a quantity in
// millionths, a trade's or a delivery's unit price to 10 places and a
// price to 8, without trailing zeros; balances and prices at midnight UTC,
// activities at noon UTC and a little after.
var (
	gfMoneyForm    = regexp.MustCompile(`^[0-9]+\.[0-9]{2}$`)
	gfBalanceForm  = regexp.MustCompile(`^-?[0-9]+\.[0-9]{2}$`)
	gfQuantityForm = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{0,5}[1-9])?$`)
	gfUnitForm     = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{0,9}[1-9])?$`)
	gfQuoteForm    = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{0,7}[1-9])?$`)
	gfMidnightForm = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})T00:00:00\.000Z$`)
	gfNoonForm     = regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})T12:([0-9]{2}):([0-9]{2})\.([0-9]{3})Z$`)
	// gfIdentity is the end of a security's profile name: its identity in
	// parentheses.
	gfIdentity = regexp.MustCompile(` \(([^()]+)\)$`)
	// gfRefForm is what an activity's comment holds after its account's:
	// a ref, and a leg of a trade of several.
	gfRefForm = regexp.MustCompile(`^[^ ]+( leg [0-9]+)?$`)
)

// gfCheck fails t with each problem that gfProblems finds in the file at
// path.
func gfCheck(t testing.TB, path string, deliveries map[string]bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range gfProblems(b, deliveries) {
		t.Errorf("%s: %s", path, p)
	}
}

// gfDeliveries returns the comments of the activities that are deliveries
// or option closes among records events, which gfProblems checks the unit
// prices of: Ghostfolio's file shows them as a BUY or a SELL like any
// other.
func gfDeliveries(events []toolEvent) map[string]bool {
	out := map[string]bool{}
	for i := range events {
		switch e := &events[i]; e.kind {
		case evDeliveryIn, evDeliveryOut, evCloseIn, evCloseOut:
			out[gfComment(e)] = true
		}
	}
	return out
}

// gfProblems returns what Ghostfolio 3.99.0's import refuses in the file b,
// and what breaks the export's own conventions, one line each; none for a
// file that imports as the export means it to (11.4 of the design).
// deliveries are the comments of the activities that are deliveries, whose
// unit prices must be plausible; nil checks none.
func gfProblems(b []byte, deliveries map[string]bool) []string {
	c := &gfChecker{
		deliveries: deliveries, accounts: map[string]string{}, names: map[string]bool{}, profiles: map[string]gfObject{},
		cash: map[string]bool{}, quotes: map[string][]gfDated{},
	}
	c.file(b)
	return c.problems
}

// A gfChecker is what gfProblems has found, and what it has read so far.
type gfChecker struct {
	problems   []string
	deliveries map[string]bool
	// accounts are the comments of the file's accounts by their ids, and
	// names their names; profiles are its profiles by symbol, cash the
	// symbols of its cash profiles, and quotes each profile's prices.
	accounts map[string]string
	names    map[string]bool
	profiles map[string]gfObject
	cash     map[string]bool
	quotes   map[string][]gfDated
}

// A gfDated is a price on a day, in dollars per share.
type gfDated struct {
	day   string
	price *big.Rat
}

// add records a problem.
func (c *gfChecker) add(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// file checks the whole file: one JSON object of the five keys, activities
// an array, then the accounts, the profiles and the activities.
func (c *gfChecker) file(b []byte) {
	if !utf8.Valid(b) {
		c.add("not UTF-8")
		return
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var top gfObject
	if err := dec.Decode(&top); err != nil {
		c.add("not a JSON object: %v", err)
		return
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		c.add("more than one JSON value")
	}
	c.keys("the file", top, gfFileKeys)
	activities, ok := top["activities"].([]any)
	if !ok {
		c.add("activities must be an array")
	}
	accounts := c.array("accounts", top)
	profiles := c.array("assetProfiles", top)
	c.array("platforms", top)
	c.array("tags", top)
	for i, v := range accounts {
		c.account(fmt.Sprintf("accounts.%d", i), v)
	}
	names, symbols := map[string]bool{}, map[string]bool{}
	for i, v := range profiles {
		c.profile(fmt.Sprintf("assetProfiles.%d", i), v, names, symbols)
	}
	c.activities(activities)
}

// array returns the array at key of the file, nil when it is absent; one
// that is not an array is a problem.
func (c *gfChecker) array(key string, top gfObject) []any {
	v, ok := top[key]
	if !ok || v == nil {
		return nil
	}
	a, ok := v.([]any)
	if !ok {
		c.add("%s must be an array", key)
	}
	return a
}

// keys records each key of obj that is not one of allowed.
func (c *gfChecker) keys(where string, obj gfObject, allowed []string) {
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !slices.Contains(allowed, k) {
			c.add("%s: property %s should not exist", where, k)
		}
	}
}

// object returns v as an object, recording a problem when it is not one.
func (c *gfChecker) object(where string, v any) (gfObject, bool) {
	obj, ok := v.(gfObject)
	if !ok {
		c.add("%s must be an object", where)
	}
	return obj, ok
}

// str returns the string at key of obj, and false when it is absent or
// null, which is a problem when required, or not a string.
func (c *gfChecker) str(where string, obj gfObject, key string, required bool) (string, bool) {
	v, ok := obj[key]
	if !ok || v == nil {
		if required {
			c.add("%s.%s must be given", where, key)
		}
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		c.add("%s.%s must be a string", where, key)
	}
	return s, ok
}

// num returns the number at key of obj, exact, and its text; false when it
// is absent or null (a problem when required) or not a JSON number. A
// number below 0 is a problem when nonNegative, as Ghostfolio's Min(0).
func (c *gfChecker) num(where string, obj gfObject, key string, required, nonNegative bool) (*big.Rat, string, bool) {
	v, ok := obj[key]
	if !ok || v == nil {
		if required {
			c.add("%s.%s must be given", where, key)
		}
		return nil, "", false
	}
	n, ok := v.(json.Number)
	if !ok {
		c.add("%s.%s must be a number", where, key)
		return nil, "", false
	}
	r, ok := new(big.Rat).SetString(n.String())
	if !ok {
		c.add("%s.%s must be a number", where, key)
		return nil, "", false
	}
	if nonNegative && r.Sign() < 0 {
		c.add("%s.%s must not be less than 0", where, key)
	}
	return r, n.String(), true
}

// optional checks the optional keys of obj that the export never writes,
// by the types that Ghostfolio's DTO gives them: strings, booleans,
// numbers, and arrays of unique strings or of objects.
func (c *gfChecker) optional(where string, obj gfObject, strs, bools, nums, stringArrays, arrays []string) {
	for _, k := range strs {
		c.str(where, obj, k, false)
	}
	for _, k := range bools {
		if v, ok := obj[k]; ok && v != nil {
			if _, ok := v.(bool); !ok {
				c.add("%s.%s must be a boolean", where, k)
			}
		}
	}
	for _, k := range nums {
		c.num(where, obj, k, false, false)
	}
	for _, k := range stringArrays {
		v, ok := obj[k]
		if !ok || v == nil {
			continue
		}
		a, ok := v.([]any)
		seen := map[string]bool{}
		for _, e := range a {
			s, isStr := e.(string)
			ok = ok && isStr && !seen[s]
			seen[s] = true
		}
		if !ok {
			c.add("%s.%s must be an array of unique strings", where, k)
		}
	}
	for _, k := range arrays {
		if v, ok := obj[k]; ok && v != nil {
			if _, ok := v.([]any); !ok {
				c.add("%s.%s must be an array", where, k)
			}
		}
	}
}

// usd checks that obj's currency is given and is USD, as the export writes
// every amount.
func (c *gfChecker) usd(where string, obj gfObject) {
	if s, ok := c.str(where, obj, "currency", true); ok && s != "USD" {
		c.add("%s.currency is %q, not USD", where, s)
	}
}

// comment checks the comment at key of obj when it has one: a string of at
// most 2000 characters, which the export writes without white space at
// its ends, since Ghostfolio trims a comment before it compares it.
func (c *gfChecker) comment(where string, obj gfObject, required bool) (string, bool) {
	s, ok := c.str(where, obj, "comment", required)
	if !ok {
		return "", false
	}
	if utf8.RuneCountInString(s) > gfCommentMax {
		c.add("%s.comment is longer than %d characters", where, gfCommentMax)
	}
	if s != strings.TrimSpace(s) {
		c.add("%s.comment has white space at an end", where)
	}
	return s, true
}

// symbol checks the symbol of obj, which Ghostfolio requires: not empty, at
// most 255 characters, without white space at its ends.
func (c *gfChecker) symbol(where string, obj gfObject) (string, bool) {
	s, ok := c.str(where, obj, "symbol", true)
	if !ok {
		return "", false
	}
	switch {
	case s == "":
		c.add("%s.symbol must not be empty", where)
	case utf8.RuneCountInString(s) > gfSymbolMax:
		c.add("%s.symbol is longer than %d characters", where, gfSymbolMax)
	case s != strings.TrimSpace(s):
		c.add("%s.symbol has white space at an end", where)
	}
	return s, true
}

// enum checks that obj's key, when given (or when required), is one of
// values.
func (c *gfChecker) enum(where string, obj gfObject, key string, values []string, required bool) (string, bool) {
	s, ok := c.str(where, obj, key, required)
	if ok && !slices.Contains(values, s) {
		c.add("%s.%s must be one of the following values: %s", where, key, strings.Join(values, ", "))
		return s, false
	}
	return s, ok
}

// account checks an account and its balances (6.3 of the design): its id,
// which its activities name, and its name, by which Ghostfolio finds it,
// each its own; its name and comment, "Schwab" and "schwabtx" with its
// label; USD; platformId present and null; balances at midnight UTC, any
// sign, one per date.
func (c *gfChecker) account(where string, v any) {
	a, ok := c.object(where, v)
	if !ok {
		return
	}
	c.keys(where, a, gfAccountKeys)
	c.optional(where, a, nil, []string{"isExcluded"}, []string{"balance"}, []string{"tags"}, nil)
	comment, _ := c.comment(where, a, false)
	if id, ok := c.str(where, a, "id", true); ok {
		if _, ok := c.accounts[id]; ok {
			c.add("%s.id %s names two accounts", where, id)
		}
		c.accounts[id] = comment
	}
	if name, ok := c.str(where, a, "name", true); ok {
		if c.names[name] {
			c.add("%s.name %q is another account's too: Ghostfolio finds an account by its name", where, name)
		}
		c.names[name] = true
		if label, ours := strings.CutPrefix(name, "Schwab "); !ours || comment != "schwabtx "+label {
			c.add("%s: the name %q and the comment %q do not both give the account's label", where, name, comment)
		}
	}
	c.usd(where, a)
	switch p, ok := a["platformId"]; {
	case !ok:
		c.add("%s.platformId must be given, if only as null", where)
	case p != nil:
		if _, ok := p.(string); !ok {
			c.add("%s.platformId must be a string or null", where)
		} else {
			c.add("%s.platformId names a platform, which the export never does", where)
		}
	}
	balances, ok := a["balances"]
	if !ok || balances == nil {
		return
	}
	list, ok := balances.([]any)
	if !ok {
		c.add("%s.balances must be an array", where)
		return
	}
	dates := map[string]bool{}
	for j, v := range list {
		at := fmt.Sprintf("%s.balances.%d", where, j)
		b, ok := c.object(at, v)
		if !ok {
			continue
		}
		c.keys(at, b, gfBalanceKeys)
		c.comment(at, b, false)
		if _, text, ok := c.num(at, b, "value", true, false); ok && !gfBalanceForm.MatchString(text) {
			c.add("%s.value is %s, not dollars and cents", at, text)
		}
		if d, ok := c.str(at, b, "date", true); ok {
			if !gfDayAt(gfMidnightForm, d) {
				c.add("%s.date is %q, not a day at midnight UTC", at, d)
			}
			if dates[d] {
				c.add("%s.date %s holds a balance already: one account has one balance a date", at, d)
			}
			dates[d] = true
		}
	}
}

// gfDayAt reports whether s matches form, whose first group is a day, and
// that day is a date after 1970.
func gfDayAt(form *regexp.Regexp, s string) bool {
	m := form.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	d, err := time.Parse(time.DateOnly, m[1])
	return err == nil && d.After(time.Unix(0, 0))
}

// profile checks an asset profile (6.4 of the design): MANUAL, a UUID as
// its symbol, in USD, a name of its own; a cash profile with one price of
// 1, a security's named for its identity, its CUSIP when it has one; each
// price a number above 0 at midnight UTC, one per date.
func (c *gfChecker) profile(where string, v any, names, symbols map[string]bool) {
	p, ok := c.object(where, v)
	if !ok {
		return
	}
	c.keys(where, p, gfProfileKeys)
	c.optional(where, p, []string{"cusip", "figi", "figiComposite", "figiShareClass", "isin", "url"}, []string{"isActive"}, nil, nil,
		[]string{"countries", "holdings", "sectors"})
	if ds, ok := c.str(where, p, "dataSource", true); ok && ds != "MANUAL" {
		c.add("%s.dataSource must be 'MANUAL'", where)
	}
	symbol, _ := c.symbol(where, p)
	if !gfUUID.MatchString(symbol) {
		// Ghostfolio takes a GF_ symbol too, but keeps it for an
		// administrator's profiles.
		c.add("%s.symbol (%q) must be a UUID", where, symbol)
	}
	if symbols[symbol] {
		c.add("%s.symbol %s is another profile's too", where, symbol)
	}
	symbols[symbol] = true
	c.profiles[symbol] = p
	c.usd(where, p)
	c.enum(where, p, "assetClass", gfAssetClasses, false)
	c.enum(where, p, "assetSubClass", gfAssetSubClass, false)
	c.comment(where, p, false)
	name, ok := c.str(where, p, "name", true)
	if ok && names[name] {
		c.add("%s.name %q is another profile's too: Ghostfolio finds a profile by its name", where, name)
	}
	names[name] = true
	c.prices(where, p, symbol)
	switch {
	case slices.Contains(gfCashNames, name):
		c.cash[symbol] = true
		if q := c.quotes[symbol]; len(q) != 1 || q[0].price.Cmp(big.NewRat(1, 1)) != 0 {
			c.add("%s: the cash profile %s must have one price, of 1", where, name)
		}
	default:
		m := gfIdentity.FindStringSubmatch(name)
		cusip, _ := c.str(where, p, "cusip", false)
		switch {
		case m == nil:
			c.add("%s.name %q does not end with the security's identity in parentheses", where, name)
		case cusip != "" && m[1] != cusip:
			c.add("%s.name %q names %s, not its CUSIP %s", where, name, m[1], cusip)
		}
	}
}

// prices checks a profile's prices and keeps them for the delivery rule.
func (c *gfChecker) prices(where string, p gfObject, symbol string) {
	v, ok := p["marketData"]
	if !ok || v == nil {
		return
	}
	list, ok := v.([]any)
	if !ok {
		c.add("%s.marketData must be an array", where)
		return
	}
	dates := map[string]bool{}
	for j, v := range list {
		at := fmt.Sprintf("%s.marketData.%d", where, j)
		q, ok := c.object(at, v)
		if !ok {
			continue
		}
		c.keys(at, q, gfPriceKeys)
		price, text, ok := c.num(at, q, "marketPrice", true, false)
		switch {
		case !ok:
		case price.Sign() <= 0:
			c.add("%s.marketPrice must be above 0", at)
		case !gfQuoteForm.MatchString(text):
			c.add("%s.marketPrice is %s, not a price to 8 places", at, text)
		}
		d, dok := c.str(at, q, "date", true)
		if dok && !gfDayAt(gfMidnightForm, d) {
			c.add("%s.date is %q, not a day at midnight UTC", at, d)
		}
		if dok && dates[d] {
			c.add("%s.date %s holds a price already", at, d)
		}
		dates[d] = true
		if ok && dok && len(d) >= 10 {
			c.quotes[symbol] = append(c.quotes[symbol], gfDated{d[:10], price})
		}
	}
}

// A gfChecked is an activity of the file as gfProblems reads it.
type gfChecked struct {
	where                                string
	account, comment, source, symbol     string
	typ, date                            string
	fee, quantity, unitPrice             *big.Rat
	feeText, quantityText, unitPriceText string
	ok                                   bool
}

// activities checks the activities (3.4, 6.5 and 6.6 of the design) one by
// one, then together: no two of one comment, which makes them unlike by
// Ghostfolio's duplicate key too, since the key holds the comment
// (gfDuplicateKey); no BUY and SELL of one symbol in one account at one
// instant, whose order Ghostfolio would choose; and deliveries at
// plausible prices.
func (c *gfChecker) activities(list []any) {
	var all []gfChecked
	for i, v := range list {
		if a, ok := c.activity(fmt.Sprintf("activities.%d", i), v); ok {
			all = append(all, a)
		}
	}
	comments, instants := map[string]string{}, map[string]string{}
	for _, a := range all {
		if prev, ok := comments[a.comment]; ok && a.comment != "" {
			c.add("%s.comment %q is %s's too, so Ghostfolio could take one for the other", a.where, a.comment, prev)
		}
		comments[a.comment] = a.where
		if !a.ok || a.typ != "BUY" && a.typ != "SELL" {
			continue
		}
		at := a.account + " " + a.symbol + " " + a.date
		if prev, ok := instants[at]; ok && !strings.HasSuffix(prev, " "+a.typ) {
			c.add("%s is a %s at the instant of a %s of its symbol in its account, so their order is Ghostfolio's to choose", a.where, a.typ, strings.Fields(prev)[1])
		}
		instants[at] = a.where + " " + a.typ
	}
	for _, a := range all {
		if a.ok && c.deliveries[a.comment] {
			c.plausible(a, all)
		}
	}
}

// activity checks one activity and returns what it holds.
func (c *gfChecker) activity(where string, v any) (gfChecked, bool) {
	a := gfChecked{where: where}
	obj, ok := c.object(where, v)
	if !ok {
		return a, false
	}
	c.keys(where, obj, gfActivityKeys)
	c.optional(where, obj, nil, []string{"updateAccountBalance"}, nil, []string{"tags"}, nil)
	c.enum(where, obj, "assetClass", gfAssetClasses, false)
	c.enum(where, obj, "assetSubClass", gfAssetSubClass, false)
	if s, ok := c.str(where, obj, "customCurrency", false); ok {
		c.add("%s.customCurrency is %q: the export writes none", where, s)
	}
	c.usd(where, obj)
	var okAccount, okComment, okSource, okSymbol, okType, okDate bool
	a.account, okAccount = c.str(where, obj, "accountId", true)
	account, known := c.accounts[a.account]
	if okAccount && !known {
		c.add("%s.accountId (%q) names no account of the file", where, a.account)
	}
	a.comment, okComment = c.comment(where, obj, true)
	if okComment && !strings.HasPrefix(a.comment, "schwabtx ") {
		c.add("%s.comment %q does not start with schwabtx", where, a.comment)
	}
	if rest, named := strings.CutPrefix(a.comment, account+" "); okComment && known && (!named || !gfRefForm.MatchString(rest)) {
		c.add("%s.comment %q is not its account's comment, a ref and maybe a leg", where, a.comment)
	}
	a.source, okSource = c.enum(where, obj, "dataSource", gfDataSources, true)
	if okSource && a.source != "YAHOO" && a.source != "MANUAL" {
		c.add("%s.dataSource is %s: the export writes YAHOO or MANUAL", where, a.source)
	}
	a.symbol, okSymbol = c.symbol(where, obj)
	a.typ, okType = c.enum(where, obj, "type", gfTypes, true)
	a.date, okDate = c.str(where, obj, "date", true)
	if okDate && !gfNoon(a.date) {
		c.add("%s.date is %q, not a day at noon UTC plus up to 86400 milliseconds", where, a.date)
		okDate = false
	}
	var okFee, okQuantity, okPrice bool
	a.fee, a.feeText, okFee = c.num(where, obj, "fee", true, true)
	a.quantity, a.quantityText, okQuantity = c.num(where, obj, "quantity", true, true)
	a.unitPrice, a.unitPriceText, okPrice = c.num(where, obj, "unitPrice", true, true)
	a.ok = okAccount && okComment && okSource && okSymbol && okType && okDate && okFee && okQuantity && okPrice
	if okSource && okSymbol {
		c.source(where, a.source, a.symbol)
	}
	if okType && okSymbol && okFee && okQuantity && okPrice {
		c.conventions(where, a)
	}
	return a, true
}

// gfNoon reports whether date is a day after 1970 at noon UTC plus 0 to
// 86 400 milliseconds, as the export dates an activity (6.5 of the design).
func gfNoon(date string) bool {
	m := gfNoonForm.FindStringSubmatch(date)
	if m == nil || !gfDayAt(gfNoonForm, date) {
		return false
	}
	minute, _ := strconv.Atoi(m[2])
	second, _ := strconv.Atoi(m[3])
	milli, _ := strconv.Atoi(m[4])
	offset := (minute*60+second)*1000 + milli
	return second < 60 && offset <= 86_400
}

// source checks an activity's symbol against its data source: a MANUAL one
// is a UUID that a profile of the file has; Ghostfolio refuses a YAHOO one
// that would do for a MANUAL asset.
func (c *gfChecker) source(where, source, symbol string) {
	switch source {
	case "MANUAL":
		if !gfUUID.MatchString(symbol) {
			c.add("%s.symbol (%q) must be a UUID for the data source (\"MANUAL\")", where, symbol)
		} else if c.profiles[symbol] == nil {
			c.add("%s.symbol %s is no profile of the file", where, symbol)
		}
	default:
		if gfCustomSymbol(symbol) {
			c.add("%s.symbol (%q) is not valid for the data source (%q)", where, symbol, source)
		}
	}
}

// conventions checks how the export writes each type (3.4.4 of the design):
// a FEE of quantity 0 at 0 with a fee above 0, and a DIVIDEND or an
// INTEREST of quantity 1 at a price above 0 with its money in dollars and
// cents; a FEE or an INTEREST of a cash profile, a BUY, a SELL or a
// DIVIDEND never; a BUY or a SELL of a quantity above 0 in millionths, at a
// price to 10 places; no LIABILITY.
func (c *gfChecker) conventions(where string, a gfChecked) {
	cash := c.cash[a.symbol]
	if !gfMoneyForm.MatchString(a.feeText) {
		c.add("%s.fee is %s, not dollars and cents", where, a.feeText)
	}
	switch a.typ {
	case "FEE":
		if a.quantity.Sign() != 0 || a.unitPrice.Sign() != 0 || a.fee.Sign() <= 0 {
			c.add("%s: a FEE is of quantity 0 at 0, with a fee above 0", where)
		}
		if !cash {
			c.add("%s: a FEE is of a cash profile", where)
		}
	case "DIVIDEND", "INTEREST":
		if a.quantity.Cmp(big.NewRat(1, 1)) != 0 || a.unitPrice.Sign() <= 0 {
			c.add("%s: a %s is of quantity 1 at its amount, above 0", where, a.typ)
		}
		if !gfMoneyForm.MatchString(a.unitPriceText) {
			c.add("%s.unitPrice is %s, not dollars and cents", where, a.unitPriceText)
		}
		if a.typ == "INTEREST" && (!cash || a.fee.Sign() != 0) {
			c.add("%s: an INTEREST is of a cash profile, with no fee", where)
		}
		if a.typ == "DIVIDEND" && cash {
			c.add("%s: a DIVIDEND is never of a cash profile", where)
		}
	case "BUY", "SELL":
		if a.quantity.Sign() <= 0 {
			c.add("%s: a %s is of a quantity above 0", where, a.typ)
		}
		if !gfQuantityForm.MatchString(a.quantityText) {
			c.add("%s.quantity is %s, not a quantity in millionths", where, a.quantityText)
		}
		if !gfUnitForm.MatchString(a.unitPriceText) {
			c.add("%s.unitPrice is %s, not a price to 10 places", where, a.unitPriceText)
		}
		if cash {
			c.add("%s: a %s is never of a cash profile", where, a.typ)
		}
	default:
		c.add("%s: the export writes no %s", where, a.typ)
	}
}

// plausible checks a delivery's unit price against the nearest price of
// its security that the file holds, a trade's or a profile's, by day: it
// must be within a factor of 3 of it, unless it is 0 (11.4 of the design).
// A bond at 100 times its price fails.
func (c *gfChecker) plausible(d gfChecked, all []gfChecked) {
	if d.unitPrice.Sign() == 0 {
		return
	}
	var near *gfDated
	distance := func(day string) time.Duration {
		a, _ := time.Parse(time.DateOnly, d.date[:10])
		b, _ := time.Parse(time.DateOnly, day)
		return max(a.Sub(b), b.Sub(a))
	}
	consider := func(p gfDated) {
		if p.price.Sign() > 0 && (near == nil || distance(p.day) < distance(near.day)) {
			near = &p
		}
	}
	for _, a := range all {
		if a.ok && a.symbol == d.symbol && (a.typ == "BUY" || a.typ == "SELL") && !c.deliveries[a.comment] {
			consider(gfDated{a.date[:10], a.unitPrice})
		}
	}
	for _, q := range c.quotes[d.symbol] {
		consider(q)
	}
	if near == nil {
		return
	}
	low, high := new(big.Rat).Quo(near.price, big.NewRat(3, 1)), new(big.Rat).Mul(near.price, big.NewRat(3, 1))
	if d.unitPrice.Cmp(low) < 0 || d.unitPrice.Cmp(high) > 0 {
		c.add("%s: a delivery at %s, more than 3 times off the price %s of %s", d.where, d.unitPriceText, near.price.FloatString(8), near.day)
	}
}

// gfDuplicateKey returns what Ghostfolio compares when it looks for an
// activity that the user has already (import.service.ts:1069-1082): the
// comment, the currency, the data source, the date to the second, the
// fee, the quantity, the symbol, the type and the unit price, with the
// numbers as the doubles that it stores and compares.
func gfDuplicateKey(comment, currency, source, date string, fee, quantity *big.Rat, symbol, typ string, unitPrice *big.Rat) string {
	second := date
	if t, err := time.Parse(time.RFC3339Nano, date); err == nil {
		second = t.Truncate(time.Second).Format(time.RFC3339)
	}
	double := func(r *big.Rat) string {
		f, _ := r.Float64()
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strings.Join([]string{comment, currency, source, second, double(fee), double(quantity), symbol, typ, double(unitPrice)}, "\x1f")
}

// A gfModel is one user of a Ghostfolio instance, as far as importing files
// moves it (import.service.ts:204-1034 of 3.99.0): the user's accounts with
// their balances, MANUAL profiles with their prices, and activities; and,
// of the instance, the account ids and MANUAL symbols that other users
// hold, which the user cannot take, and the symbols that Yahoo does not
// resolve. Files are imported as the web dialog imports them: a dry run,
// which may refuse the file, and then the import itself, only for a file
// with a new activity.
type gfModel struct {
	accounts   []*gfmAccount
	profiles   []*gfmProfile
	activities []gfmActivity
	// otherAccounts and otherSymbols are what other users of the instance
	// hold: account ids and MANUAL symbols.
	otherAccounts, otherSymbols map[string]bool
	// yahooLacks are the symbols that Yahoo does not resolve.
	yahooLacks map[string]bool
	// fresh counts the ids and symbols that the instance has made.
	fresh int
}

// A gfmAccount is an account of the user, with its balances by date.
type gfmAccount struct {
	id, name, currency string
	balances           map[string]*big.Rat
}

// A gfmProfile is a MANUAL profile of the user, with its prices by date.
type gfmProfile struct {
	symbol, name string
	prices       map[string]*big.Rat
}

// A gfmActivity is an activity: its account (none for ""), its asset (a
// data source and a symbol), and its values, exact.
type gfmActivity struct {
	account, comment, currency, source, symbol, typ, date string
	fee, quantity, unitPrice                              *big.Rat
}

// newGFModel returns a user who holds nothing, on an instance whose other
// users hold nothing either.
func newGFModel() *gfModel {
	return &gfModel{otherAccounts: map[string]bool{}, otherSymbols: map[string]bool{}, yahooLacks: map[string]bool{}}
}

// freshID returns an id that the instance makes: a UUID that no one holds.
func (m *gfModel) freshID() string {
	m.fresh++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", m.fresh)
}

// account returns the user's account of id, nil for none.
func (m *gfModel) account(id string) *gfmAccount {
	i := slices.IndexFunc(m.accounts, func(a *gfmAccount) bool { return a.id == id })
	if i < 0 {
		return nil
	}
	return m.accounts[i]
}

// profile returns the user's profile of symbol, nil for none.
func (m *gfModel) profile(symbol string) *gfmProfile {
	i := slices.IndexFunc(m.profiles, func(p *gfmProfile) bool { return p.symbol == symbol })
	if i < 0 {
		return nil
	}
	return m.profiles[i]
}

// A gfmFile is what gfImport reads of a file.
type gfmFile struct {
	accounts   []gfmFileAccount
	profiles   []gfmFileProfile
	activities []gfmActivity
}

// A gfmFileAccount is an account of a file, with its balances.
type gfmFileAccount struct {
	id, name, currency string
	balances           []gfmDated
}

// A gfmFileProfile is an asset profile of a file, with its prices.
type gfmFileProfile struct {
	source, symbol, name string
	prices               []gfmDated
}

// A gfmDated is a number on a date, both as a file writes them.
type gfmDated struct{ date, value string }

// rat returns d's number, exact; 0 for one that is not a number.
func (d gfmDated) rat() *big.Rat {
	r, ok := new(big.Rat).SetString(d.value)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// gfmRead reads a file for gfImport. It expects a file that gfProblems
// passes, and takes a key that is missing or of another type as empty; a
// comment and a symbol it trims, as Ghostfolio does.
func gfmRead(b []byte) (gfmFile, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var top gfObject
	if err := dec.Decode(&top); err != nil {
		return gfmFile{}, err
	}
	str := func(o gfObject, k string) string { s, _ := o[k].(string); return s }
	num := func(o gfObject, k string) string { n, _ := o[k].(json.Number); return n.String() }
	objects := func(v any) []gfObject {
		var out []gfObject
		list, _ := v.([]any)
		for _, e := range list {
			if o, ok := e.(gfObject); ok {
				out = append(out, o)
			}
		}
		return out
	}
	dated := func(v any, key string) []gfmDated {
		var out []gfmDated
		for _, o := range objects(v) {
			out = append(out, gfmDated{str(o, "date"), num(o, key)})
		}
		return out
	}
	var f gfmFile
	for _, a := range objects(top["accounts"]) {
		f.accounts = append(f.accounts, gfmFileAccount{str(a, "id"), str(a, "name"), str(a, "currency"), dated(a["balances"], "value")})
	}
	for _, p := range objects(top["assetProfiles"]) {
		f.profiles = append(f.profiles, gfmFileProfile{str(p, "dataSource"), strings.TrimSpace(str(p, "symbol")), str(p, "name"), dated(p["marketData"], "marketPrice")})
	}
	for _, a := range objects(top["activities"]) {
		f.activities = append(f.activities, gfmActivity{
			account: str(a, "accountId"), comment: strings.TrimSpace(str(a, "comment")), currency: str(a, "currency"),
			source: str(a, "dataSource"), symbol: strings.TrimSpace(str(a, "symbol")), typ: str(a, "type"), date: str(a, "date"),
			fee: gfmDated{value: num(a, "fee")}.rat(), quantity: gfmDated{value: num(a, "quantity")}.rat(),
			unitPrice: gfmDated{value: num(a, "unitPrice")}.rat(),
		})
	}
	return f, nil
}

// gfImport imports the file b into the user of m, as Ghostfolio's import
// dialog does, and returns how many activities the user gained; or why
// Ghostfolio refuses the file, which leaves the user as it was. The dialog
// imports only a file with an activity that is new to the user.
//
// An account of the file whose id the user has, or whose name and
// currency are those of one account of the user's and one of the file's,
// is left as it is, balances and all, and its activities go to the user's;
// any other is made with its balances, under a fresh id when another user
// holds its id. A profile whose symbol the user owns, or else the user's
// profile of its name, takes its prices and its activities; any other is
// made, under a fresh symbol when another user holds its symbol, with its
// prices, but only when an activity that the import makes is of it. An
// activity equal to one that the user has, by the duplicate key, in any
// account, is skipped.
func gfImport(m *gfModel, b []byte) (int, error) {
	f, err := gfmRead(b)
	if err != nil {
		return 0, err
	}
	if err := gfmSymbols(&f); err != nil {
		return 0, err
	}
	made, err := m.importAccounts(&f)
	if err != nil {
		return 0, err
	}
	owned, toMake := m.importProfiles(&f)
	if err := m.resolve(f, owned, toMake); err != nil {
		return 0, err
	}
	isNew := make([]bool, len(f.activities))
	count := 0
	for i, a := range f.activities {
		isNew[i] = !slices.ContainsFunc(m.activities, func(h gfmActivity) bool { return gfmSame(h, a) })
		if isNew[i] {
			count++
		}
		if a.account != "" && m.account(a.account) == nil && !slices.ContainsFunc(made, func(n *gfmAccount) bool { return n.id == a.account }) {
			return 0, fmt.Errorf("activities.%d.accountId (%q) is not valid", i, a.account)
		}
	}
	if count == 0 {
		return 0, errors.New("the file holds no activity that is new to the user: the import dialog has none to import")
	}
	// The import itself.
	m.accounts = append(m.accounts, made...)
	for _, p := range owned {
		maps.Copy(m.profile(p.symbol).prices, p.prices)
	}
	for _, p := range toMake {
		for i, a := range f.activities {
			if isNew[i] && a.source == "MANUAL" && a.symbol == p.symbol {
				m.profiles = append(m.profiles, p)
				break
			}
		}
	}
	for i, a := range f.activities {
		if isNew[i] {
			m.activities = append(m.activities, a)
		}
	}
	return count, nil
}

// gfmSymbols checks the symbols of file f before anything is written
// (import.service.ts:231-294): a profile is MANUAL, of a UUID or GF_
// symbol; an activity of no data source is MANUAL for FEE, INTEREST and
// LIABILITY and YAHOO for the others, the instance's default; one that is
// MANUAL has a UUID or GF_ symbol, and one that is not has neither.
func gfmSymbols(f *gfmFile) error {
	for i, p := range f.profiles {
		switch {
		case p.source != "MANUAL":
			return fmt.Errorf("assetProfiles.%d.dataSource must be 'MANUAL'", i)
		case !gfCustomSymbol(p.symbol):
			return fmt.Errorf("assetProfiles.%d.symbol (%q) must be a UUID or start with the prefix \"GF_\"", i, p.symbol)
		}
	}
	for i := range f.activities {
		a := &f.activities[i]
		switch {
		case a.source == "" && slices.Contains(gfNonInvestment, a.typ):
			a.source = "MANUAL"
		case a.source == "":
			a.source = "YAHOO"
		case a.source == "MANUAL" && !gfCustomSymbol(a.symbol):
			return fmt.Errorf("activities.%d.symbol (%q) must be a UUID or start with the prefix \"GF_\"", i, a.symbol)
		}
		if a.source != "MANUAL" && gfCustomSymbol(a.symbol) {
			return fmt.Errorf("activities.%d.symbol (%q) is not valid for the data source (%q)", i, a.symbol, a.source)
		}
	}
	return nil
}

// importAccounts returns the accounts of file f that the import makes, and
// points f's activities at the user's accounts that it takes or at the
// fresh ids that it gives (import.service.ts:441-586).
func (m *gfModel) importAccounts(f *gfmFile) ([]*gfmAccount, error) {
	ids := map[string]string{}
	var made []*gfmAccount
	for _, fa := range f.accounts {
		if fa.id != "" && m.account(fa.id) != nil {
			continue
		}
		if reuse := m.reusable(fa, f); reuse != nil {
			if fa.id != "" && fa.id != reuse.id {
				ids[fa.id] = reuse.id
			}
			continue
		}
		a := &gfmAccount{id: fa.id, name: fa.name, currency: fa.currency, balances: map[string]*big.Rat{}}
		if fa.id == "" || m.otherAccounts[fa.id] {
			a.id = m.freshID()
			if fa.id != "" {
				ids[fa.id] = a.id
			}
		}
		for _, b := range fa.balances {
			if a.balances[b.date] != nil {
				// The database keeps one balance of an account a date.
				return nil, fmt.Errorf("the account %s has two balances of %s, which its creation refuses", fa.name, b.date)
			}
			a.balances[b.date] = b.rat()
		}
		made = append(made, a)
	}
	for i := range f.activities {
		if id, ok := ids[f.activities[i].account]; ok {
			f.activities[i].account = id
		}
	}
	return made, nil
}

// reusable returns the user's account that account fa of file f takes
// (getAccountToReuse): the one account of the user's with fa's name and
// currency, when fa is the file's one account of them; nil for none.
func (m *gfModel) reusable(fa gfmFileAccount, f *gfmFile) *gfmAccount {
	var mine []*gfmAccount
	for _, a := range m.accounts {
		if a.name == fa.name && a.currency == fa.currency {
			mine = append(mine, a)
		}
	}
	theirs := 0
	for _, a := range f.accounts {
		if a.name == fa.name && a.currency == fa.currency {
			theirs++
		}
	}
	if len(mine) != 1 || theirs != 1 {
		return nil
	}
	return mine[0]
}

// importProfiles returns, for the profiles of file f, the prices that go to
// profiles of the user's (by symbol, or by name) and the profiles that the
// import may make, and points f's activities at the symbols that they take
// (import.service.ts:588-760). A file that holds one profile twice makes
// it once, with both's prices.
func (m *gfModel) importProfiles(f *gfmFile) (owned, toMake []*gfmProfile) {
	symbols := map[string]string{}
	for _, fp := range f.profiles {
		prices := map[string]*big.Rat{}
		for _, q := range fp.prices {
			prices[q.date] = q.rat()
		}
		symbol := fp.symbol
		mine := m.profile(symbol)
		if mine == nil {
			i := slices.IndexFunc(m.profiles, func(p *gfmProfile) bool { return p.name == fp.name && gfCustomSymbol(p.symbol) })
			if i >= 0 {
				mine = m.profiles[i]
			}
		}
		switch {
		case mine != nil:
			symbol = mine.symbol
			owned = append(owned, &gfmProfile{symbol: symbol, prices: prices})
		default:
			if prev, ok := symbols[fp.symbol]; ok {
				symbol = prev
			} else if m.otherSymbols[symbol] || strings.HasPrefix(symbol, "GF_") {
				symbol = m.freshID()
			}
			if i := slices.IndexFunc(toMake, func(p *gfmProfile) bool { return p.symbol == symbol }); i >= 0 {
				maps.Copy(toMake[i].prices, prices)
			} else {
				toMake = append(toMake, &gfmProfile{symbol: symbol, name: fp.name, prices: prices})
			}
		}
		if symbol != fp.symbol {
			symbols[fp.symbol] = symbol
		}
	}
	for i := range f.activities {
		if s, ok := symbols[f.activities[i].symbol]; ok {
			f.activities[i].symbol = s
		}
	}
	return owned, toMake
}

// resolve checks that each asset of file f's activities resolves
// (validateActivities): its data source is enabled; a MANUAL BUY, a FEE,
// an INTEREST, a LIABILITY and an asset of a MANUAL profile of the file
// need no data provider; Yahoo resolves the symbols that it knows, and
// Ghostfolio's own database the MANUAL ones that a profile holds.
func (m *gfModel) resolve(f gfmFile, owned, toMake []*gfmProfile) error {
	inFile := map[string]bool{}
	for _, p := range slices.Concat(owned, toMake) {
		inFile[p.symbol] = true
	}
	resolved := map[string]bool{}
	for i, a := range f.activities {
		if !slices.Contains(gfEnabledSources, a.source) {
			return fmt.Errorf("activities.%d.dataSource (%q) is not valid", i, a.source)
		}
		asset := a.source + " " + a.symbol
		switch {
		case resolved[asset]:
		case a.source == "MANUAL" && (a.typ == "BUY" || inFile[a.symbol]), slices.Contains(gfNonInvestment, a.typ):
		case a.source == "MANUAL" && (m.profile(a.symbol) != nil || m.otherSymbols[a.symbol]):
		case a.source == "YAHOO" && !m.yahooLacks[a.symbol]:
		default:
			return fmt.Errorf("activities.%d.symbol (%q) cannot be resolved by the data source (%q)", i, a.symbol, a.source)
		}
		resolved[asset] = true
	}
	return nil
}

// gfmSame reports whether activities a and b are one by Ghostfolio's
// duplicate key, which no account is part of.
func gfmSame(a, b gfmActivity) bool {
	return gfDuplicateKey(a.comment, a.currency, a.source, a.date, a.fee, a.quantity, a.symbol, a.typ, a.unitPrice) ==
		gfDuplicateKey(b.comment, b.currency, b.source, b.date, b.fee, b.quantity, b.symbol, b.typ, b.unitPrice)
}

// cash returns the cash of the user's account named name: its latest
// balance (account.service.ts:151-160); false for no such account, or one
// without a balance.
func (m *gfModel) cash(name string) (*big.Rat, bool) {
	for _, a := range m.accounts {
		if a.name == name && len(a.balances) > 0 {
			return a.balances[slices.Max(slices.Collect(maps.Keys(a.balances)))], true
		}
	}
	return nil, false
}

// A gfmHolding is what an account holds of an asset: the account's id, and
// the asset's data source and symbol.
type gfmHolding struct{ account, source, symbol string }

// holdings returns the quantity of each asset in each account that the
// user's BUY and SELL activities leave (getFactor): the shares that
// Ghostfolio holds. An asset of none is left out.
func (m *gfModel) holdings() map[gfmHolding]*big.Rat {
	out := map[gfmHolding]*big.Rat{}
	for _, a := range m.activities {
		h := gfmHolding{a.account, a.source, a.symbol}
		if out[h] == nil {
			out[h] = new(big.Rat)
		}
		switch a.typ {
		case "BUY":
			out[h].Add(out[h], a.quantity)
		case "SELL":
			out[h].Sub(out[h], a.quantity)
		}
	}
	for h, q := range out {
		if q.Sign() == 0 {
			delete(out, h)
		}
	}
	return out
}

// flows returns the cash of each account that the user's activities say
// moved: a BUY its value and its fee out, a SELL, a DIVIDEND and an
// INTEREST their value in and their fee out, a FEE its fee out; each value
// quantity × unit price, rounded to cents. Ghostfolio moves no cash when
// it imports an activity: the tests reconcile these with the store's
// ledger, whose cash Ghostfolio's balances carry.
func (m *gfModel) flows() map[string]*big.Rat {
	out := map[string]*big.Rat{}
	for _, a := range m.activities {
		if out[a.account] == nil {
			out[a.account] = new(big.Rat)
		}
		value := gfmCents(new(big.Rat).Mul(a.quantity, a.unitPrice))
		switch a.typ {
		case "BUY":
			out[a.account].Sub(out[a.account], value)
		case "SELL", "DIVIDEND", "INTEREST":
			out[a.account].Add(out[a.account], value)
		}
		out[a.account].Sub(out[a.account], a.fee)
	}
	return out
}

// gfmCents returns r rounded to cents, half away from zero.
func gfmCents(r *big.Rat) *big.Rat {
	hundredths := new(big.Rat).Mul(r, big.NewRat(100, 1))
	q, rem := new(big.Int).QuoRem(hundredths.Num(), hundredths.Denom(), new(big.Int))
	if new(big.Int).Mul(new(big.Int).Abs(rem), big.NewInt(2)).Cmp(hundredths.Denom()) >= 0 {
		q.Add(q, big.NewInt(int64(hundredths.Sign())))
	}
	return new(big.Rat).SetFrac(q, big.NewInt(100))
}

// A gfPerformance is what Ghostfolio's calculator makes of the user's
// activities, as far as their fees, dividends and interest go
// (portfolio-calculator.ts of 3.99.0).
type gfPerformance struct {
	// fees are every activity's fee, the Fees total; offNet those of the
	// holdings that the calculator values, which it takes off net
	// performance (:1243, 1650-1652).
	fees, offNet *big.Rat
	// dividends and interest are the DIVIDEND and INTEREST activities'
	// quantities times their unit prices (:2016-2059).
	dividends, interest *big.Rat
	// unvalued are the holdings, as a data source and a symbol, that the
	// calculator cannot value, whose fees stay off net performance;
	// noInvestment those with a dividend and no investment, any one of
	// which sets the portfolio's dividend yield to 0 (:1799-1835).
	unvalued, noInvestment []string
}

// performance returns what Ghostfolio's calculator makes of the user's
// activities. A holding is an asset, in every account at once. The
// calculator values it only with a price at the end (:1552-1581): Yahoo's,
// which the model takes Yahoo to serve for every symbol that it resolves,
// as the import requires; for a MANUAL asset, its latest price above 0,
// else the unit price of its latest BUY or SELL when above 0
// (current-rate.service.ts:125-181), else that of its last activity when
// that is a BUY or a SELL (:2061-2088). Only a valued holding's fees come
// off net performance; one that is not keeps its dividends and interest
// alone. A holding has an investment when a BUY of it cost more than 0.
func (m *gfModel) performance() gfPerformance {
	p := gfPerformance{fees: new(big.Rat), offNet: new(big.Rat), dividends: new(big.Rat), interest: new(big.Rat)}
	by := map[string][]gfmActivity{}
	for _, a := range m.activities {
		asset := a.source + " " + a.symbol
		by[asset] = append(by[asset], a)
	}
	for _, asset := range slices.Sorted(maps.Keys(by)) {
		acts := slices.Clone(by[asset])
		slices.SortStableFunc(acts, func(a, b gfmActivity) int { return cmp.Compare(a.date, b.date) })
		fees, dividends := new(big.Rat), new(big.Rat)
		invested := false
		var lastTrade *gfmActivity
		for i, a := range acts {
			fees.Add(fees, a.fee)
			value := new(big.Rat).Mul(a.quantity, a.unitPrice)
			switch a.typ {
			case "DIVIDEND":
				dividends.Add(dividends, value)
			case "INTEREST":
				p.interest.Add(p.interest, value)
			case "BUY":
				invested = invested || value.Sign() > 0
				lastTrade = &acts[i]
			case "SELL":
				lastTrade = &acts[i]
			}
		}
		valued := !strings.HasPrefix(asset, "MANUAL ")
		if !valued {
			last := acts[len(acts)-1]
			valued = lastTrade != nil && lastTrade.unitPrice.Sign() > 0 || last.typ == "BUY" || last.typ == "SELL"
			if prof := m.profile(acts[0].symbol); prof != nil {
				for _, price := range prof.prices {
					valued = valued || price.Sign() > 0
				}
			}
		}
		p.fees.Add(p.fees, fees)
		p.dividends.Add(p.dividends, dividends)
		if valued {
			p.offNet.Add(p.offNet, fees)
		} else {
			p.unvalued = append(p.unvalued, asset)
		}
		if dividends.Sign() != 0 && (!valued || !invested) {
			p.noInvestment = append(p.noInvestment, asset)
		}
	}
	return p
}

// gfCheckerInput returns an export for Ghostfolio that holds one of each
// thing that gfProblems checks: two accounts, one with two balances and one
// with a negative one; the three cash profiles; a bond's and an option's
// profiles with their prices; a BUY, a SELL, a DIVIDEND, a FEE and an
// INTEREST; an option's close; and a bond's delivery, between a trade and
// a price of it. Every value is made up.
func gfCheckerInput() toolInput {
	api := func(ref string, account int64, clock, category, key, symbol string, quantity int64, price float64, amount, fees int64) toolRow {
		r := toolRow{
			ref: ref, account: account, source: "api", time: tcNY(clock), kind: "TRADE", category: category,
			securityKey: key, symbol: symbol, amount: centsOf(amount), fees: centsOf(fees),
		}
		if quantity != 0 {
			r.quantity, r.price = centsOf(quantity), tcPrice(price)
		}
		return r
	}
	file := func(ref string, account int64, posted nytime.Date, action, category, key, symbol string, quantity int64, price float64, amount int64) toolRow {
		r := toolRow{
			ref: ref, account: account, source: "file", posted: posted, kind: action, description: "EXAMPLE", category: category,
			securityKey: key, symbol: symbol, amount: centsOf(amount),
		}
		if quantity != 0 {
			r.quantity = centsOf(quantity)
		}
		if price != 0 {
			r.price = tcPrice(price)
		}
		return r
	}
	return tcInput(toolInput{
		tool: toolGF,
		rows: []toolRow{
			api("T71000101", 2, "2025-01-21 11:05:12", "buy", keyPut, symPut, 2_000_000, 2, -40_067, 67),
			api("T71000102", 1, "2025-02-03 10:00:00", "buy", keyXYZ, "XYZ", 10_000_000, 50, -50_065, 65),
			api("T71000103", 2, "2025-02-04 10:00:00", "sell", keyPut, symPut, -1_000_000, 1.6, 15_935, 65),
			file("Cg0a1b2c3.1", 2, "2025-02-21", "Expired", "other", keyPut, symPut, -1_000_000, 0, 0),
			api("T71000104", 1, "2025-03-03 11:54:14", "dividend", keyXYZ, "XYZ", 0, 0, 386, 0),
			api("T71000105", 1, "2025-03-03 11:54:15", "tax", keyXYZ, "XYZ", 0, 0, -116, 0),
			file("Cg0a1b2c3.2", 1, "2025-03-10", "Service Fee", "fee", "", "", 0, 0, -2500),
			file("Cg0a1b2c3.3", 1, "2025-03-11", "Service Fee", "fee", "", "", 0, 0, 2500),
			api("T71000106", 1, "2025-03-31 21:18:44", "interest", "", "", 0, 0, 42, 0),
			file("Cg0a1b2c3.4", 1, "2025-04-01", "Buy", "buy", keyBondA, "9128ZZQ15", 5_000_000_000, 99.25, -496_250),
			file("Cg0a1b2c3.5", 1, "2025-05-01", "Journaled Shares", "transfer_in", keyBondA, "9128ZZQ15", 1_000_000_000, 99.5, 0),
		},
		positions: []toolPosition{
			{key: keyBondA, account: 1, marketDate: "2025-06-30", fetchedAt: tcNY("2025-06-30 18:00:00"), long: centsOf(6_000_000_000), short: centsOf(0), marketValue: tcPrice(5970)},
			{key: keyPut, account: 2, marketDate: "2025-01-31", fetchedAt: tcNY("2025-01-31 18:00:00"), long: centsOf(1_000_000), short: centsOf(0), marketValue: tcPrice(150)},
		},
		snapshots: []toolSnapshot{
			{account: 1, marketDate: "2025-06-30", fetchedAt: tcNY("2025-06-30 18:05:00"), cash: centsOf(123_456)},
			{account: 1, marketDate: "2025-07-31", fetchedAt: tcNY("2025-07-31 18:05:00"), cash: centsOf(130_000)},
			{account: 2, marketDate: "2025-06-30", fetchedAt: tcNY("2025-06-30 18:05:00"), cash: centsOf(-1234)},
		},
	})
}

// gfCheckerFile returns gfCheckerInput's ghostfolio.json and the comments
// of its deliveries.
func gfCheckerFile(t *testing.T) ([]byte, map[string]bool) {
	t.Helper()
	in := gfCheckerInput()
	events, _ := toolEvents(&in)
	var b bytes.Buffer
	x := gfRender(&in, events)
	if err := x.file.write(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), gfDeliveries(events)
}

// gfEdit returns file b with edit's change: b is decoded, changed and
// encoded again.
func gfEdit(t *testing.T, b []byte, edit func(top gfObject)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var top gfObject
	if err := dec.Decode(&top); err != nil {
		t.Fatal(err)
	}
	edit(top)
	out, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// gfIn returns the object of top's list whose key is value, or whose
// comment ends with " "+value when key is "comment".
func gfIn(t *testing.T, top gfObject, list, key, value string) gfObject {
	t.Helper()
	items, _ := top[list].([]any)
	for _, v := range items {
		o, _ := v.(gfObject)
		s, _ := o[key].(string)
		if s == value || key == "comment" && strings.HasSuffix(s, " "+value) {
			return o
		}
	}
	t.Fatalf("no %s has %s %s", list, key, value)
	return nil
}

// TestToolCheckersGF shows that gfProblems passes a good file and refuses
// it changed in each way that Ghostfolio's import refuses, or that breaks
// the export's own conventions (11.4 of the design), one at a time.
func TestToolCheckersGF(t *testing.T) {
	t.Parallel()
	good, deliveries := gfCheckerFile(t)
	if p := gfProblems(good, deliveries); len(p) > 0 {
		t.Fatalf("the good file has problems: %q", p)
	}
	act := func(top gfObject, ref string) gfObject { return gfIn(t, top, "activities", "comment", ref) }
	account := func(top gfObject, name string) gfObject { return gfIn(t, top, "accounts", "name", name) }
	profile := func(top gfObject, name string) gfObject { return gfIn(t, top, "assetProfiles", "name", name) }
	symbolOf := func(top gfObject, name string) string { return profile(top, name)["symbol"].(string) }
	const bond, option = "Bond (9128ZZQ15)", "XYZ 2025-02-21 40 put (XYZ250221P00040000)"
	appendTo := func(top gfObject, list string, o gfObject) {
		c := maps.Clone(o)
		top[list] = append(top[list].([]any), c)
	}
	breaks := []struct {
		name string
		edit func(top gfObject)
		want string
	}{
		{"a key Ghostfolio's file has, meta", func(top gfObject) { top["meta"] = gfObject{} }, "property meta should not exist"},
		{"no activities", func(top gfObject) { delete(top, "activities") }, "activities must be an array"},
		{"accounts not an array", func(top gfObject) { top["accounts"] = gfObject{} }, "accounts must be an array"},
		{"an activity not an object", func(top gfObject) { top["activities"] = append(top["activities"].([]any), "x") }, "must be an object"},
		{"an id on an activity", func(top gfObject) { act(top, "T71000102")["id"] = "x" }, "property id should not exist"},
		{"no fee", func(top gfObject) { delete(act(top, "T71000102"), "fee") }, "fee must be given"},
		{"a fee as a string", func(top gfObject) { act(top, "T71000102")["fee"] = "0.65" }, "fee must be a number"},
		{"a fee below 0", func(top gfObject) { act(top, "T71000102")["fee"] = json.Number("-0.65") }, "fee must not be less than 0"},
		{"a quantity below 0", func(top gfObject) { act(top, "T71000102")["quantity"] = json.Number("-10") }, "quantity must not be less than 0"},
		{"a unit price below 0", func(top gfObject) { act(top, "T71000102")["unitPrice"] = json.Number("-50") }, "unitPrice must not be less than 0"},
		{"a date at midnight", func(top gfObject) { act(top, "T71000102")["date"] = "2025-02-03T00:00:00.000Z" }, "not a day at noon UTC"},
		{"a date past the day's window", func(top gfObject) { act(top, "T71000102")["date"] = "2025-02-03T12:01:26.401Z" }, "not a day at noon UTC"},
		{"a date before 1970", func(top gfObject) { act(top, "T71000102")["date"] = "1969-07-21T12:00:00.000Z" }, "not a day at noon UTC"},
		{"a day that is no date", func(top gfObject) { act(top, "T71000102")["date"] = "2025-02-30T12:00:00.000Z" }, "not a day at noon UTC"},
		{"a currency other than USD", func(top gfObject) { act(top, "T71000102")["currency"] = "EUR" }, `currency is "EUR", not USD`},
		{"a type of none", func(top gfObject) { act(top, "T71000102")["type"] = "ITEM" }, "type must be one of the following values"},
		{"a data source not written", func(top gfObject) { act(top, "T71000102")["dataSource"] = "ALPHA_VANTAGE" }, "the export writes YAHOO or MANUAL"},
		{"no data source", func(top gfObject) { delete(act(top, "T71000102"), "dataSource") }, "dataSource must be given"},
		{"a comment too long", func(top gfObject) { act(top, "T71000102")["comment"] = "schwabtx " + strings.Repeat("x", 1992) }, "longer than 2000"},
		{"a comment with a space at its end", func(top gfObject) {
			c := act(top, "T71000102")
			c["comment"] = c["comment"].(string) + " "
		}, "comment has white space at an end"},
		{"no comment", func(top gfObject) { delete(act(top, "T71000102"), "comment") }, "comment must be given"},
		{"a comment not the export's", func(top gfObject) { act(top, "T71000102")["comment"] = "T71000102" }, "does not start with schwabtx"},
		{"a comment of another account", func(top gfObject) { act(top, "T71000102")["comment"] = "schwabtx A2 ...398 T71000102" }, "is not its account's comment"},
		{"a comment of no ref", func(top gfObject) { act(top, "T71000102")["comment"] = "schwabtx A1 ...135" }, "is not its account's comment"},
		{"a comment of more than a ref", func(top gfObject) { act(top, "T71000102")["comment"] = "schwabtx A1 ...135 T71000102 bought" }, "is not its account's comment"},
		{"a symbol too long", func(top gfObject) { act(top, "T71000102")["symbol"] = strings.Repeat("X", 256) }, "longer than 255"},
		{"an empty symbol", func(top gfObject) { act(top, "T71000102")["symbol"] = "" }, "symbol must not be empty"},
		{"a symbol with a space at its end", func(top gfObject) { act(top, "T71000102")["symbol"] = "XYZ " }, "symbol has white space at an end"},
		{"a MANUAL symbol fees", func(top gfObject) { act(top, "Cg0a1b2c3.2")["symbol"] = "fees" }, `must be a UUID for the data source ("MANUAL")`},
		{"a MANUAL symbol of no profile", func(top gfObject) {
			act(top, "Cg0a1b2c3.2")["symbol"] = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		}, "is no profile of the file"},
		{"a YAHOO symbol that is a UUID", func(top gfObject) {
			act(top, "T71000102")["symbol"] = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
		}, "is not valid for the data source"},
		{"a YAHOO symbol of an administrator's", func(top gfObject) { act(top, "T71000102")["symbol"] = "GF_XYZ" }, "is not valid for the data source"},
		{"an account of no account", func(top gfObject) { act(top, "T71000102")["accountId"] = "no-such-account" }, "names no account of the file"},
		{"a FEE of quantity 1", func(top gfObject) { act(top, "Cg0a1b2c3.2")["quantity"] = json.Number("1") }, "a FEE is of quantity 0 at 0"},
		{"a FEE of no fee", func(top gfObject) { act(top, "Cg0a1b2c3.2")["fee"] = json.Number("0.00") }, "a FEE is of quantity 0 at 0"},
		{"a FEE of a security", func(top gfObject) { act(top, "Cg0a1b2c3.2")["symbol"] = symbolOf(top, bond) }, "a FEE is of a cash profile"},
		{"a DIVIDEND of quantity 2", func(top gfObject) { act(top, "T71000104")["quantity"] = json.Number("2") }, "a DIVIDEND is of quantity 1"},
		{"an INTEREST with a fee", func(top gfObject) { act(top, "T71000106")["fee"] = json.Number("0.01") }, "an INTEREST is of a cash profile, with no fee"},
		{"an INTEREST of a security", func(top gfObject) {
			a := act(top, "T71000106")
			a["symbol"], a["dataSource"] = symbolOf(top, bond), "MANUAL"
		}, "an INTEREST is of a cash profile"},
		{"a DIVIDEND of Schwab fees", func(top gfObject) {
			a := act(top, "T71000104")
			a["symbol"], a["dataSource"] = symbolOf(top, "Schwab fees"), "MANUAL"
		}, "a DIVIDEND is never of a cash profile"},
		{"a BUY of quantity 0", func(top gfObject) { act(top, "T71000102")["quantity"] = json.Number("0") }, "a BUY is of a quantity above 0"},
		{"a BUY of a cash profile", func(top gfObject) {
			a := act(top, "T71000102")
			a["symbol"], a["dataSource"] = symbolOf(top, "Schwab taxes"), "MANUAL"
		}, "a BUY is never of a cash profile"},
		{"a LIABILITY", func(top gfObject) { act(top, "Cg0a1b2c3.2")["type"] = "LIABILITY" }, "the export writes no LIABILITY"},
		{"two activities of one comment", func(top gfObject) { appendTo(top, "activities", act(top, "T71000102")) }, "so Ghostfolio could take one for the other"},
		{"a BUY and a SELL of one asset at one instant", func(top gfObject) {
			act(top, "T71000103")["date"] = act(top, "T71000101")["date"]
		}, "is a SELL at the instant of a BUY"},
		{"a bond delivered at 100 times its price", func(top gfObject) { act(top, "Cg0a1b2c3.5")["unitPrice"] = json.Number("99.5") }, "more than 3 times off"},
		{"a delivery a hundredth of the price of its day", func(top gfObject) {
			act(top, "Cg0a1b2c3.5")["date"] = "2025-06-30T12:00:00.000Z"
			profile(top, bond)["marketData"].([]any)[0].(gfObject)["marketPrice"] = json.Number("99.5")
		}, "more than 3 times off the price 99.50000000 of 2025-06-30"},
		{"a symbol as a number", func(top gfObject) { act(top, "T71000102")["symbol"] = json.Number("1") }, "symbol must be a string"},
		{"a name as a number", func(top gfObject) { account(top, "Schwab A2 ...398")["name"] = json.Number("1") }, "name must be a string"},
		{"a fee not in cents", func(top gfObject) { act(top, "T71000102")["fee"] = json.Number("0.6") }, "not dollars and cents"},
		{"a quantity past millionths", func(top gfObject) { act(top, "T71000102")["quantity"] = json.Number("10.0000001") }, "not a quantity in millionths"},
		{"a quantity with a trailing zero", func(top gfObject) { act(top, "T71000102")["quantity"] = json.Number("10.0") }, "not a quantity in millionths"},
		{"a unit price past 10 places", func(top gfObject) { act(top, "T71000102")["unitPrice"] = json.Number("50.00000000001") }, "not a price to 10 places"},
		{"a DIVIDEND not in cents", func(top gfObject) { act(top, "T71000104")["unitPrice"] = json.Number("3.860") }, "not dollars and cents"},
		{"a custom currency", func(top gfObject) { act(top, "T71000102")["customCurrency"] = "EUR" }, "the export writes none"},
		{"tags that repeat", func(top gfObject) { act(top, "T71000102")["tags"] = []any{"a", "a"} }, "must be an array of unique strings"},
		{"an asset class of none", func(top gfObject) { act(top, "T71000102")["assetClass"] = "STOCKS" }, "assetClass must be one of the following values"},
		{"no platformId", func(top gfObject) { delete(account(top, "Schwab A1 ...135"), "platformId") }, "platformId must be given, if only as null"},
		{"a platformId of a number", func(top gfObject) { account(top, "Schwab A1 ...135")["platformId"] = json.Number("1") }, "platformId must be a string or null"},
		{"a platform", func(top gfObject) { account(top, "Schwab A1 ...135")["platformId"] = "x" }, "names a platform"},
		{"a key that no account has", func(top gfObject) { account(top, "Schwab A1 ...135")["cash"] = json.Number("1") }, "property cash should not exist"},
		{"no name", func(top gfObject) { delete(account(top, "Schwab A2 ...398"), "name") }, "name must be given"},
		{"no currency", func(top gfObject) { delete(account(top, "Schwab A2 ...398"), "currency") }, "currency must be given"},
		{"no account id", func(top gfObject) { delete(account(top, "Schwab A2 ...398"), "id") }, "id must be given"},
		{"two accounts of one name", func(top gfObject) {
			a := account(top, "Schwab A2 ...398")
			a["name"], a["comment"] = "Schwab A1 ...135", "schwabtx A1 ...135"
		}, "is another account's too"},
		{"an account's comment of another label", func(top gfObject) { account(top, "Schwab A2 ...398")["comment"] = "schwabtx A2" }, "do not both give the account's label"},
		{"an account named otherwise", func(top gfObject) { account(top, "Schwab A2 ...398")["name"] = "A2 ...398" }, "do not both give the account's label"},
		{"two accounts of one id", func(top gfObject) {
			account(top, "Schwab A2 ...398")["id"] = account(top, "Schwab A1 ...135")["id"]
		}, "names two accounts"},
		{"isExcluded as text", func(top gfObject) { account(top, "Schwab A1 ...135")["isExcluded"] = "yes" }, "isExcluded must be a boolean"},
		{"two balances of a date", func(top gfObject) {
			b := account(top, "Schwab A1 ...135")["balances"].([]any)
			b[1].(gfObject)["date"] = b[0].(gfObject)["date"]
		}, "holds a balance already"},
		{"a balance as a string", func(top gfObject) {
			account(top, "Schwab A2 ...398")["balances"].([]any)[0].(gfObject)["value"] = "-12.34"
		}, "value must be a number"},
		{"a balance not in cents", func(top gfObject) {
			account(top, "Schwab A2 ...398")["balances"].([]any)[0].(gfObject)["value"] = json.Number("-12.3")
		}, "not dollars and cents"},
		{"a balance at noon", func(top gfObject) {
			account(top, "Schwab A2 ...398")["balances"].([]any)[0].(gfObject)["date"] = "2025-06-30T12:00:00.000Z"
		}, "not a day at midnight UTC"},
		{"a key that no balance has", func(top gfObject) {
			account(top, "Schwab A2 ...398")["balances"].([]any)[0].(gfObject)["id"] = "x"
		}, "property id should not exist"},
		{"balances not an array", func(top gfObject) { account(top, "Schwab A2 ...398")["balances"] = gfObject{} }, "balances must be an array"},
		{"a profile of YAHOO", func(top gfObject) { profile(top, bond)["dataSource"] = "YAHOO" }, "dataSource must be 'MANUAL'"},
		{"a profile of an administrator's symbol", func(top gfObject) { profile(top, bond)["symbol"] = "GF_BOND" }, "must be a UUID"},
		{"a profile with no symbol", func(top gfObject) { delete(profile(top, bond), "symbol") }, "symbol must be given"},
		{"a key that no profile has", func(top gfObject) { profile(top, bond)["id"] = "x" }, "property id should not exist"},
		{"a profile with no currency", func(top gfObject) { delete(profile(top, bond), "currency") }, "currency must be given"},
		{"a profile with no name", func(top gfObject) { delete(profile(top, bond), "name") }, "name must be given"},
		{"a cash profile without its price", func(top gfObject) { delete(profile(top, "Schwab fees"), "marketData") }, "must have one price, of 1"},
		{"a cash profile priced at 2", func(top gfObject) {
			profile(top, "Schwab taxes")["marketData"].([]any)[0].(gfObject)["marketPrice"] = json.Number("2")
		}, "must have one price, of 1"},
		{"a security named without its identity", func(top gfObject) { profile(top, bond)["name"] = "Bond" }, "does not end with the security's identity"},
		{"a security named for another identity", func(top gfObject) { profile(top, bond)["name"] = "Bond (9128ZZQ16)" }, "not its CUSIP"},
		{"two profiles of one symbol", func(top gfObject) { profile(top, option)["symbol"] = symbolOf(top, bond) }, "is another profile's too"},
		{"two profiles of one name", func(top gfObject) { profile(top, option)["name"] = bond }, "Ghostfolio finds a profile by its name"},
		{"a price of 0", func(top gfObject) {
			profile(top, bond)["marketData"].([]any)[0].(gfObject)["marketPrice"] = json.Number("0")
		}, "marketPrice must be above 0"},
		{"a price past 8 places", func(top gfObject) {
			profile(top, bond)["marketData"].([]any)[0].(gfObject)["marketPrice"] = json.Number("0.995000001")
		}, "not a price to 8 places"},
		{"a price at noon", func(top gfObject) {
			profile(top, bond)["marketData"].([]any)[0].(gfObject)["date"] = "2025-06-30T12:00:00.000Z"
		}, "not a day at midnight UTC"},
		{"two prices of a date", func(top gfObject) {
			p := profile(top, bond)
			p["marketData"] = append(p["marketData"].([]any), maps.Clone(p["marketData"].([]any)[0].(gfObject)))
		}, "holds a price already"},
		{"a key that no price has", func(top gfObject) {
			profile(top, bond)["marketData"].([]any)[0].(gfObject)["close"] = json.Number("1")
		}, "property close should not exist"},
		{"an asset class of none on a profile", func(top gfObject) { profile(top, bond)["assetSubClass"] = "BONDS" }, "assetSubClass must be one of"},
		{"a profile's comment too long", func(top gfObject) { profile(top, bond)["comment"] = strings.Repeat("x", 2001) }, "longer than 2000"},
		{"countries not an array", func(top gfObject) { profile(top, bond)["countries"] = "US" }, "countries must be an array"},
	}
	for _, b := range breaks {
		problems := gfProblems(gfEdit(t, good, b.edit), deliveries)
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, b.want) }) {
			t.Errorf("%s: got %q, want a problem with %q", b.name, problems, b.want)
		}
	}
	for _, b := range []struct {
		name, file, want string
	}{
		{"not UTF-8", "{\"activities\": [], \"x\": \"\xff\"}", "not UTF-8"},
		{"not JSON", "{", "not a JSON object"},
		{"two values", "{\"activities\": []}\n{}", "more than one JSON value"},
	} {
		if problems := gfProblems([]byte(b.file), nil); !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, b.want) }) {
			t.Errorf("%s: got %q, want a problem with %q", b.name, problems, b.want)
		}
	}
}

// gfmFileOf returns f as ghostfolio.json, for the tests of gfModel.
func gfmFileOf(t *testing.T, f gfFile) []byte {
	t.Helper()
	if f.Accounts == nil {
		f.Accounts = []gfAccount{}
	}
	if f.AssetProfiles == nil {
		f.AssetProfiles = []gfProfile{}
	}
	if f.Activities == nil {
		f.Activities = []gfActivity{}
	}
	var b bytes.Buffer
	if err := f.write(&b); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// gfmAccountOf returns an account of a test's file, in USD, with balances
// written as "date=value".
func gfmAccountOf(id, name string, balances ...string) gfAccount {
	a := gfAccount{Comment: "schwabtx " + name, Currency: "USD", ID: id, Name: name}
	for _, b := range balances {
		date, value, _ := strings.Cut(b, "=")
		a.Balances = append(a.Balances, gfBalance{Date: date + "T00:00:00.000Z", Value: json.Number(value)})
	}
	return a
}

// gfmProfileOf returns a MANUAL profile of a test's file, in USD, with
// prices written as "date=price".
func gfmProfileOf(symbol, name string, prices ...string) gfProfile {
	p := gfProfile{Currency: "USD", DataSource: "MANUAL", Name: name, Symbol: symbol}
	for _, q := range prices {
		date, price, _ := strings.Cut(q, "=")
		p.MarketData = append(p.MarketData, gfPrice{Date: date + "T00:00:00.000Z", MarketPrice: json.Number(price)})
	}
	return p
}

// gfmActivityOf returns an activity of a test's file, in USD, at noon UTC
// of day: quantity at unitPrice, with fee.
func gfmActivityOf(account, comment, source, symbol, typ, day, quantity, unitPrice, fee string) gfActivity {
	return gfActivity{
		AccountID: account, Comment: comment, Currency: "USD", DataSource: source, Date: day + "T12:00:00.000Z",
		Fee: json.Number(fee), Quantity: json.Number(quantity), Symbol: symbol, Type: typ, UnitPrice: json.Number(unitPrice),
	}
}

// The ids and symbols of gfModel's tests, all made up.
const (
	gfmA1    = "11111111-1111-4111-8111-111111111111"
	gfmA2    = "22222222-2222-4222-8222-222222222222"
	gfmFees  = "33333333-3333-4333-8333-333333333333"
	gfmBond  = "44444444-4444-4444-8444-444444444444"
	gfmOther = "55555555-5555-4555-8555-555555555555"
)

// TestGFModel shows that gfModel imports files as Ghostfolio 3.99.0's
// import does (11.4 of the design): what it refuses, leaving the user as
// it was; how it finds accounts and profiles again, by id or symbol, by
// name, or not at all; that it skips an activity the user has, in any
// account, but not one that a file holds twice; and what Ghostfolio's
// calculator then makes of fees and dividends.
func TestGFModel(t *testing.T) {
	t.Parallel()
	a1 := gfmAccountOf(gfmA1, "Schwab A1 ...135", "2025-06-30=100.00")
	fees := gfmProfileOf(gfmFees, "Schwab fees", "2025-01-02=1")
	buy := gfmActivityOf(gfmA1, "schwabtx A1 ...135 T1", "YAHOO", "XYZ", "BUY", "2025-01-02", "10", "50", "0.65")
	fee := gfmActivityOf(gfmA1, "schwabtx A1 ...135 C1.1", "MANUAL", gfmFees, "FEE", "2025-01-03", "0", "0", "25.00")
	base := gfFile{Accounts: []gfAccount{a1}, AssetProfiles: []gfProfile{fees}, Activities: []gfActivity{buy, fee}}

	t.Run("refusals", func(t *testing.T) {
		t.Parallel()
		changed := func(change func(f *gfFile)) []byte {
			f := base
			f.Accounts = slices.Clone(base.Accounts)
			f.AssetProfiles = slices.Clone(base.AssetProfiles)
			f.Activities = slices.Clone(base.Activities)
			change(&f)
			return gfmFileOf(t, f)
		}
		for _, c := range []struct {
			name  string
			file  []byte
			setup func(m *gfModel)
			want  string
		}{
			{"no activity", changed(func(f *gfFile) { f.Activities = nil }), nil, "no activity that is new"},
			{"an account of no account", changed(func(f *gfFile) { f.Activities[0].AccountID = gfmA2 }), nil, "accountId"},
			{"a MANUAL symbol fees", changed(func(f *gfFile) { f.Activities[1].Symbol = "fees" }), nil, "must be a UUID"},
			{"a YAHOO symbol that is a UUID", changed(func(f *gfFile) { f.Activities[0].Symbol = gfmOther }), nil, "is not valid for the data source"},
			{"a profile of YAHOO", changed(func(f *gfFile) { f.AssetProfiles[0].DataSource = "YAHOO" }), nil, "must be 'MANUAL'"},
			{"a profile's symbol of its name", changed(func(f *gfFile) { f.AssetProfiles[0].Symbol = "Schwab fees" }), nil, "must be a UUID"},
			{"a data source that the instance lacks", changed(func(f *gfFile) { f.Activities[0].DataSource = "ALPHA_VANTAGE" }), nil, "is not valid"},
			{"a symbol that Yahoo lacks", changed(func(*gfFile) {}), func(m *gfModel) { m.yahooLacks["XYZ"] = true }, "cannot be resolved"},
			{"a MANUAL BUY of no profile", changed(func(f *gfFile) {
				f.Activities[0].DataSource, f.Activities[0].Symbol = "MANUAL", gfmOther
			}), nil, ""},
			{"a MANUAL SELL of no profile", changed(func(f *gfFile) {
				f.Activities[0].DataSource, f.Activities[0].Symbol, f.Activities[0].Type = "MANUAL", gfmOther, "SELL"
			}), nil, "cannot be resolved"},
			{"a MANUAL DIVIDEND of no profile", changed(func(f *gfFile) {
				f.Activities[0].DataSource, f.Activities[0].Symbol, f.Activities[0].Type = "MANUAL", gfmOther, "DIVIDEND"
			}), nil, "cannot be resolved"},
			{"two balances of a date", changed(func(f *gfFile) {
				f.Accounts[0].Balances = append(f.Accounts[0].Balances, f.Accounts[0].Balances[0])
			}), nil, "two balances"},
		} {
			m := newGFModel()
			if c.setup != nil {
				c.setup(m)
			}
			n, err := gfImport(m, c.file)
			switch {
			case c.want == "":
				// A MANUAL BUY needs no profile: Ghostfolio makes one.
				if err != nil {
					t.Errorf("%s: %v", c.name, err)
				}
				continue
			case err == nil || !strings.Contains(err.Error(), c.want):
				t.Errorf("%s: imported %d, error %v, want %q", c.name, n, err, c.want)
			case len(m.accounts)+len(m.profiles)+len(m.activities) > 0:
				t.Errorf("%s: refused, but the user holds %d accounts, %d profiles, %d activities", c.name, len(m.accounts), len(m.profiles), len(m.activities))
			}
		}
		m := newGFModel()
		if n, err := gfImport(m, gfmFileOf(t, base)); n != 2 || err != nil {
			t.Fatalf("first import: %d, %v", n, err)
		}
		if n, err := gfImport(m, gfmFileOf(t, base)); err == nil || len(m.activities) != 2 {
			t.Errorf("the same file again: %d, %v; the user holds %d activities", n, err, len(m.activities))
		}
	})

	t.Run("accounts", func(t *testing.T) {
		t.Parallel()
		// An account that the user has keeps its balances; its activities
		// come in.
		m := newGFModel()
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		later := base
		later.Accounts = []gfAccount{gfmAccountOf(gfmA1, "Schwab A1 ...135", "2025-06-30=100.00", "2025-07-31=250.00")}
		later.Activities = append(slices.Clone(base.Activities), gfmActivityOf(gfmA1, "schwabtx A1 ...135 T2", "YAHOO", "XYZ", "SELL", "2025-07-01", "5", "55", "0.65"))
		if n, err := gfImport(m, gfmFileOf(t, later)); n != 1 || err != nil {
			t.Fatalf("later import: %d, %v", n, err)
		}
		if cash, ok := m.cash("Schwab A1 ...135"); !ok || cash.FloatString(2) != "100.00" || len(m.accounts) != 1 {
			t.Errorf("cash %v, %d accounts; want 100.00, the balance of the import that made the account", cash, len(m.accounts))
		}
		// An account of the user's name and currency takes the file's
		// activities, without its balances.
		m = newGFModel()
		m.accounts = append(m.accounts, &gfmAccount{id: "user-1", name: "Schwab A1 ...135", currency: "USD", balances: map[string]*big.Rat{}})
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		if _, ok := m.cash("Schwab A1 ...135"); ok || len(m.accounts) != 1 || m.activities[0].account != "user-1" || m.activities[1].account != "user-1" {
			t.Errorf("by name: %d accounts, activities in %s and %s", len(m.accounts), m.activities[0].account, m.activities[1].account)
		}
		// An account whose id another user holds is made under a fresh one.
		m = newGFModel()
		m.otherAccounts[gfmA1] = true
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		if id := m.accounts[0].id; id == gfmA1 || m.activities[0].account != id || m.activities[1].account != id {
			t.Errorf("another's id: account %s, activities in %s and %s", id, m.activities[0].account, m.activities[1].account)
		}
		if cash, ok := m.cash("Schwab A1 ...135"); !ok || cash.FloatString(2) != "100.00" {
			t.Errorf("another's id: cash %v, want its balance", cash)
		}
		// One of the user's in another currency is not the file's.
		m = newGFModel()
		m.accounts = append(m.accounts, &gfmAccount{id: "user-1", name: "Schwab A1 ...135", currency: "EUR", balances: map[string]*big.Rat{}})
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil || len(m.accounts) != 2 || m.activities[0].account != gfmA1 {
			t.Errorf("another currency: %v, %d accounts, activities in %s", err, len(m.accounts), m.activities[0].account)
		}
		// The cash of an account made with several balances is its latest.
		m = newGFModel()
		two := base
		two.Accounts = []gfAccount{gfmAccountOf(gfmA1, "Schwab A1 ...135", "2025-07-31=250.00", "2025-06-30=-12.34")}
		if _, err := gfImport(m, gfmFileOf(t, two)); err != nil {
			t.Fatal(err)
		}
		if cash, ok := m.cash("Schwab A1 ...135"); !ok || cash.FloatString(2) != "250.00" {
			t.Errorf("two balances: cash %v, want the latest, 250.00", cash)
		}
		// An account renamed in Ghostfolio is found by its id.
		m = newGFModel()
		m.accounts = append(m.accounts, &gfmAccount{id: gfmA1, name: "Brokerage", currency: "USD", balances: map[string]*big.Rat{}})
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil || len(m.accounts) != 1 || m.activities[0].account != gfmA1 {
			t.Errorf("renamed: %v, %d accounts", err, len(m.accounts))
		}
		// Two accounts of the file of one name take no account of the
		// user's: each is made.
		m = newGFModel()
		m.accounts = append(m.accounts, &gfmAccount{id: "user-1", name: "Schwab A1 ...135", currency: "USD", balances: map[string]*big.Rat{}})
		two = base
		two.Accounts = []gfAccount{a1, gfmAccountOf(gfmA2, "Schwab A1 ...135")}
		if _, err := gfImport(m, gfmFileOf(t, two)); err != nil || len(m.accounts) != 3 || m.activities[0].account != gfmA1 {
			t.Errorf("two of one name: %v, %d accounts, activities in %s", err, len(m.accounts), m.activities[0].account)
		}
	})

	t.Run("profiles", func(t *testing.T) {
		t.Parallel()
		// A profile that the user owns keeps its symbol and takes the
		// file's prices.
		m := newGFModel()
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		later := base
		later.AssetProfiles = []gfProfile{gfmProfileOf(gfmFees, "Schwab fees", "2025-01-02=1", "2025-03-03=1")}
		later.Activities = append(slices.Clone(base.Activities), gfmActivityOf(gfmA1, "schwabtx A1 ...135 C1.2", "MANUAL", gfmFees, "FEE", "2025-03-03", "0", "0", "5.00"))
		if _, err := gfImport(m, gfmFileOf(t, later)); err != nil {
			t.Fatal(err)
		}
		if len(m.profiles) != 1 || len(m.profile(gfmFees).prices) != 2 {
			t.Errorf("owned: %d profiles, prices %v", len(m.profiles), m.profile(gfmFees).prices)
		}
		// The user's profile of the file's name takes its activities.
		m = newGFModel()
		m.profiles = append(m.profiles, &gfmProfile{symbol: gfmOther, name: "Schwab fees", prices: map[string]*big.Rat{}})
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		if len(m.profiles) != 1 || m.activities[1].symbol != gfmOther || len(m.profile(gfmOther).prices) != 1 {
			t.Errorf("by name: %d profiles, the FEE of %s", len(m.profiles), m.activities[1].symbol)
		}
		// A symbol that another user holds makes the profile under a fresh
		// one; the next import finds it by its name, and its activities as
		// duplicates.
		m = newGFModel()
		m.otherSymbols[gfmFees] = true
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		fresh := m.activities[1].symbol
		if len(m.profiles) != 1 || fresh == gfmFees || m.profiles[0].symbol != fresh {
			t.Fatalf("another's symbol: %d profiles, the FEE of %s", len(m.profiles), fresh)
		}
		if n, err := gfImport(m, gfmFileOf(t, later)); n != 1 || err != nil || len(m.profiles) != 1 || m.activities[2].symbol != fresh {
			t.Errorf("again: %d, %v; %d profiles", n, err, len(m.profiles))
		}
		// A profile that no activity of the import is of is not made, so
		// its prices are lost; one that the user holds takes them.
		bond := gfmProfileOf(gfmBond, "Bond (9128ZZQ15)", "2025-06-30=0.995")
		withBond := base
		withBond.AssetProfiles = append(slices.Clone(base.AssetProfiles), bond)
		m = newGFModel()
		if _, err := gfImport(m, gfmFileOf(t, withBond)); err != nil || m.profile(gfmBond) != nil {
			t.Errorf("prices only: %v, the bond's profile %v", err, m.profile(gfmBond))
		}
		m = newGFModel()
		m.profiles = append(m.profiles, &gfmProfile{symbol: gfmBond, name: "Bond (9128ZZQ15)", prices: map[string]*big.Rat{}})
		if _, err := gfImport(m, gfmFileOf(t, withBond)); err != nil || len(m.profile(gfmBond).prices) != 1 {
			t.Errorf("prices only, owned: %v, prices %v", err, m.profile(gfmBond).prices)
		}
	})

	t.Run("sources", func(t *testing.T) {
		t.Parallel()
		// An activity with no data source is MANUAL when it is a FEE, an
		// INTEREST or a LIABILITY, else of the instance's default, YAHOO.
		unsourced := gfEdit(t, gfmFileOf(t, base), func(top gfObject) {
			for _, a := range top["activities"].([]any) {
				delete(a.(gfObject), "dataSource")
			}
		})
		m := newGFModel()
		if _, err := gfImport(m, unsourced); err != nil || m.activities[0].source != "YAHOO" || m.activities[1].source != "MANUAL" {
			t.Errorf("no data source: %v, %+v", err, m.activities)
		}
	})

	t.Run("flows", func(t *testing.T) {
		t.Parallel()
		// A reinvested dividend's unit price, 2.70 for 0.0378 shares, is
		// 71.4285714286 to 10 places: the shares at it are worth 2.70 to
		// the cent, as the store's cash is.
		in := tcInput(toolInput{tool: toolGF, rows: []toolRow{{
			ref: "T81000301", account: 1, source: "api", time: tcNY("2024-04-04 11:56:00"), kind: "TRADE", description: "Dividend Reinvest",
			category: "buy", securityKey: keyEFGH, symbol: "EFGH", quantity: centsOf(37_800), price: tcPrice(71.43), amount: centsOf(-270),
		}}})
		events, outcomes := toolEvents(&in)
		f := gfRender(&in, events).file
		if f.Activities[0].UnitPrice != "71.4285714286" {
			t.Fatalf("unit price %s", f.Activities[0].UnitPrice)
		}
		m := newGFModel()
		if _, err := gfImport(m, gfmFileOf(t, f)); err != nil {
			t.Fatal(err)
		}
		gfReconcile(t, &in, events, outcomes, m)
		for _, c := range []struct{ in, want string }{
			{"0.005", "0.01"}, {"-0.005", "-0.01"}, {"0.0049", "0.00"}, {"-0.0049", "0.00"}, {"2.70000000000108", "2.70"},
		} {
			if got := gfmCents(gfRat(t, c.in)).FloatString(2); got != c.want {
				t.Errorf("gfmCents(%s) = %s, want %s", c.in, got, c.want)
			}
		}
	})

	t.Run("duplicates", func(t *testing.T) {
		t.Parallel()
		// No account is part of the duplicate key.
		m := newGFModel()
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		moved := base
		moved.Accounts = append(slices.Clone(base.Accounts), gfmAccountOf(gfmA2, "Schwab A2 ...398"))
		moved.Activities = slices.Clone(base.Activities)
		moved.Activities[0].AccountID = gfmA2
		moved.Activities = append(moved.Activities, gfmActivityOf(gfmA2, "schwabtx A2 ...398 T3", "YAHOO", "XYZ", "BUY", "2025-02-03", "1", "51", "0.00"))
		if n, err := gfImport(m, gfmFileOf(t, moved)); n != 1 || err != nil {
			t.Errorf("an activity of another account: %d imported, %v; want only the new one", n, err)
		}
		// A file's activities are not compared with each other.
		twice := base
		twice.Activities = []gfActivity{buy, buy}
		m = newGFModel()
		if n, err := gfImport(m, gfmFileOf(t, twice)); n != 2 || err != nil {
			t.Errorf("one activity twice in a file: %d imported, %v", n, err)
		}
		// The date counts to the second, and the numbers as the doubles
		// that they read as: 50.0000000000000001 is 50.
		m = newGFModel()
		if _, err := gfImport(m, gfmFileOf(t, base)); err != nil {
			t.Fatal(err)
		}
		again := base
		again.Activities = slices.Clone(base.Activities)
		again.Activities[0].Date = "2025-01-02T12:00:00.999Z"
		again.Activities[0].UnitPrice = "50.0000000000000001"
		again.Activities[1].Fee = "25.01"
		if n, err := gfImport(m, gfmFileOf(t, again)); n != 1 || err != nil || m.activities[2].comment != fee.Comment {
			t.Errorf("one changed fee: %d imported, %v", n, err)
		}
	})

	t.Run("performance", func(t *testing.T) {
		t.Parallel()
		perf := func(f gfFile) gfPerformance {
			m := newGFModel()
			if _, err := gfImport(m, gfmFileOf(t, f)); err != nil {
				t.Fatal(err)
			}
			return m.performance()
		}
		// Ghostfolio's own spec: a FEE of 49 of a MANUAL profile without
		// prices counts as a fee, and net performance keeps it.
		opening := gfmProfileOf(gfmOther, "Account Opening Fee")
		spec := gfFile{
			Accounts: []gfAccount{a1}, AssetProfiles: []gfProfile{opening},
			Activities: []gfActivity{gfmActivityOf(gfmA1, "", "MANUAL", gfmOther, "FEE", "2021-09-01", "0", "0", "49")},
		}
		if p := perf(spec); p.fees.Cmp(big.NewRat(49, 1)) != 0 || p.offNet.Sign() != 0 || !slices.Equal(p.unvalued, []string{"MANUAL " + gfmOther}) {
			t.Errorf("Ghostfolio's spec: fees %v, off net %v, not valued %v", p.fees, p.offNet, p.unvalued)
		}
		// The same FEE of a cash profile, as the export writes it, comes
		// off net performance.
		in := tcInput(toolInput{tool: toolGF, rows: []toolRow{{
			ref: "Ch0a1b2c3.1", account: 1, source: "file", posted: "2021-09-01", kind: "Service Fee", description: "ACCOUNT OPENING FEE",
			category: "fee", amount: centsOf(-4900),
		}}})
		events, _ := toolEvents(&in)
		if p := perf(gfRender(&in, events).file); p.fees.Cmp(big.NewRat(49, 1)) != 0 || p.offNet.Cmp(p.fees) != 0 || len(p.unvalued) > 0 {
			t.Errorf("a cash profile: fees %v, off net %v, not valued %v", p.fees, p.offNet, p.unvalued)
		}
		// A MANUAL holding is valued by a price, by its latest trade's unit
		// price, or by its last activity when that is a BUY or a SELL.
		profile := gfmProfileOf(gfmBond, "Bond (9128ZZQ15)")
		priced := gfmProfileOf(gfmBond, "Bond (9128ZZQ15)", "2025-06-30=0.995")
		bondBuy := gfmActivityOf(gfmA1, "schwabtx A1 ...135 C2.1", "MANUAL", gfmBond, "BUY", "2025-04-01", "1000", "0.99", "1.00")
		atZero := gfmActivityOf(gfmA1, "schwabtx A1 ...135 C2.2", "MANUAL", gfmBond, "BUY", "2025-04-01", "1000", "0", "0.00")
		coupon := gfmActivityOf(gfmA1, "schwabtx A1 ...135 C2.3", "MANUAL", gfmBond, "DIVIDEND", "2025-08-15", "1", "21.25", "0.00")
		feeOf := gfmActivityOf(gfmA1, "schwabtx A1 ...135 C2.4", "MANUAL", gfmBond, "BUY", "2025-08-16", "1000", "0", "2.00")
		closed := gfmActivityOf(gfmA1, "schwabtx A1 ...135 C2.5", "MANUAL", gfmBond, "SELL", "2025-05-01", "1000", "0", "0.00")
		for _, c := range []struct {
			name        string
			profile     gfProfile
			activities  []gfActivity
			valued      bool
			noInvesting bool
		}{
			{"a trade's unit price", profile, []gfActivity{bondBuy, coupon}, true, false},
			{"a price", priced, []gfActivity{coupon}, true, true},
			{"none", profile, []gfActivity{coupon}, false, true},
			{"a last BUY at 0", profile, []gfActivity{coupon, feeOf}, true, true},
			{"a BUY at 0 before a DIVIDEND", profile, []gfActivity{atZero, coupon}, false, true},
			{"a SELL at 0 after a BUY, before a DIVIDEND", profile, []gfActivity{bondBuy, closed, coupon}, false, true},
		} {
			p := perf(gfFile{Accounts: []gfAccount{a1}, AssetProfiles: []gfProfile{c.profile}, Activities: c.activities})
			if valued := len(p.unvalued) == 0; valued != c.valued || (len(p.noInvestment) > 0) != c.noInvesting {
				t.Errorf("%s: valued %v, a dividend with no investment %v; want %v, %v", c.name, valued, p.noInvestment, c.valued, c.noInvesting)
			}
		}
		// A dividend of what the user never bought sets the dividend yield
		// to 0; interest counts as interest alone.
		paid := gfFile{Accounts: []gfAccount{a1}, AssetProfiles: []gfProfile{gfmProfileOf(gfmOther, "Schwab interest", "2025-01-02=1")}, Activities: []gfActivity{
			gfmActivityOf(gfmA1, "schwabtx A1 ...135 T4", "YAHOO", "ABCD", "DIVIDEND", "2025-01-02", "1", "3.86", "0.00"),
			gfmActivityOf(gfmA1, "schwabtx A1 ...135 T5", "MANUAL", gfmOther, "INTEREST", "2025-01-02", "1", "0.42", "0.00"),
		}}
		if p := perf(paid); !slices.Equal(p.noInvestment, []string{"YAHOO ABCD"}) || p.dividends.FloatString(2) != "3.86" || p.interest.FloatString(2) != "0.42" {
			t.Errorf("a dividend never bought: no investment %v, dividends %v, interest %v", p.noInvestment, p.dividends, p.interest)
		}
		paid.Activities = append(paid.Activities, gfmActivityOf(gfmA1, "schwabtx A1 ...135 T6", "YAHOO", "ABCD", "BUY", "2025-01-01", "1", "40", "0.00"))
		if p := perf(paid); len(p.noInvestment) > 0 {
			t.Errorf("a dividend bought: no investment %v", p.noInvestment)
		}
		// The export writes neither a refund as a DIVIDEND of its cash
		// profile nor the coupon of a bond whose trades are left out as a
		// DIVIDEND of the bond, either of which the model takes for a
		// dividend with no investment.
		written := func(name string) (*toolInput, []byte) {
			in, events, _ := mapCase(toolCaseNamed(name), toolGF)
			return in, gfmFileOf(t, gfRender(in, events).file)
		}
		imported := func(b []byte) gfPerformance {
			m := newGFModel()
			if _, err := gfImport(m, b); err != nil {
				t.Fatal(err)
			}
			return m.performance()
		}
		_, income := written("income")
		refund := gfEdit(t, income, func(top gfObject) { gfIn(t, top, "activities", "comment", "Cc0d1e2f3.8")["type"] = "DIVIDEND" })
		if p := imported(refund); !slices.Equal(p.noInvestment, []string{"MANUAL " + gfProfileSymbol("fees")}) {
			t.Errorf("a refund as a DIVIDEND of its cash profile: no investment %v", p.noInvestment)
		}
		bondsIn, bonds := written("bonds")
		if p := imported(bonds); len(p.noInvestment) > 0 {
			t.Errorf("the bonds as written: no investment %v", p.noInvestment)
		}
		unit := toolSecurityOf(bondsIn, keyBondB, "")
		asDividend := gfEdit(t, bonds, func(top gfObject) {
			a := gfIn(t, top, "activities", "comment", "Ca1b2c3d4.6")
			a["type"], a["symbol"] = "DIVIDEND", unit.gfSymbol()
			top["assetProfiles"] = append(top["assetProfiles"].([]any), gfObject{
				"currency": "USD", "dataSource": "MANUAL", "name": unit.gfName(), "symbol": unit.gfSymbol(),
			})
		})
		if p := imported(asDividend); !slices.Equal(p.noInvestment, []string{"MANUAL " + unit.gfSymbol()}) {
			t.Errorf("a coupon as a DIVIDEND of a bond left out: no investment %v", p.noInvestment)
		}
	})
}
