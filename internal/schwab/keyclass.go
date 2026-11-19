package schwab

import (
	"slices"
	"strings"

	"github.com/dterei/schwab-mcp/internal/mask"
)

// A keyClass is the class of the values at one key path, for mask.JSON.
type keyClass struct {
	keys  []string
	class mask.Class
}

// classesAt returns class at each of paths, each the keys that lead to a
// value joined with ".".
func classesAt(class mask.Class, paths ...string) []keyClass {
	cs := make([]keyClass, len(paths))
	for i, p := range paths {
		cs[i] = keyClass{keys: strings.Split(p, "."), class: class}
	}
	return cs
}

// A keyClasses is the classes of one kind of record's key paths, and of
// the records that it holds at some paths, such as the instruments of a
// transaction's legs, laid out as the scrub policy's tables are
// (policy_test.go), which TestKeyClassesAgreeWithScrubPolicy holds them to.
type keyClasses struct {
	classes []keyClass
	nested  []nestedKeyClasses
}

// A nestedKeyClasses is the classes of the records that another record
// holds at one key path.
type nestedKeyClasses struct {
	keys  []string
	table *keyClasses
}

// classOf returns the class of the values at path, and whether t names it.
// Keys match in any case, as the decoders and the scrub policy match them.
func (t *keyClasses) classOf(path []string) (mask.Class, bool) {
	for _, c := range t.classes {
		if slices.EqualFunc(path, c.keys, strings.EqualFold) {
			return c.class, true
		}
	}
	for _, n := range t.nested {
		if len(path) > len(n.keys) && slices.EqualFunc(path[:len(n.keys)], n.keys, strings.EqualFold) {
			return n.table.classOf(path[len(n.keys):])
		}
	}
	return mask.Unknown, false
}

// transactionKeyClasses are the classes of a transaction's key paths: its
// account number, its IDs and codes, its times, its money and quantities,
// and its description, which is free text.
var transactionKeyClasses = &keyClasses{
	classes: slices.Concat(
		classesAt(mask.AccountNumber, "accountNumber"),
		classesAt(mask.Identifier, "activityId", "orderId", "positionId", "type", "status", "subAccount", "activityType",
			"qualifiedDividend", "transferItems.feeType", "transferItems.positionEffect"),
		classesAt(mask.Time, "time", "tradeDate", "settlementDate"),
		classesAt(mask.Amount, "netAmount", "transferItems.amount", "transferItems.cost", "transferItems.price"),
		classesAt(mask.FreeText, "description"),
	),
	nested: []nestedKeyClasses{{keys: []string{"transferItems", "instrument"}, table: instrumentKeyClasses}},
}

// instrumentKeyClasses are the classes of an instrument's key paths,
// wherever one is held, an option's deliverable included: its codes,
// symbols, CUSIPs and ID, its figures, its expiration, and its description,
// a security's name, which is free text, so that a number glued into it is
// masked ("ACCT...398 TRF") while a name passes as it is.
var instrumentKeyClasses = func() *keyClasses {
	t := &keyClasses{classes: slices.Concat(
		classesAt(mask.Identifier, "assetType", "type", "status", "putCall", "symbol", "uniformSymbol", "underlyingSymbol",
			"cusip", "underlyingCusip", "instrumentId", "optionDeliverables.assetType", "optionDeliverables.apiCurrencyType",
			"optionDeliverables.rootSymbol", "optionDeliverables.symbol"),
		classesAt(mask.Amount, "netChange", "closingPrice", "strikePrice", "optionPremiumMultiplier",
			"optionDeliverables.strikePercent", "optionDeliverables.deliverableNumber", "optionDeliverables.deliverableUnits"),
		classesAt(mask.Time, "expirationDate"),
		classesAt(mask.FreeText, "description"),
	)}
	t.nested = []nestedKeyClasses{{keys: []string{"optionDeliverables", "deliverable"}, table: t}}
	return t
}()

// TransactionKeyClass returns the class of the values at path in a
// transaction's JSON, for mask.JSON: path is the keys that lead to them
// from the transaction, array elements implicit, as a Problem names a path,
// matched in any case. Schwab's user object, and everything in it, is
// withheld: its spec puts a person's names and login there (UserDetails),
// though no answer seen has held one. A key that the table does not name is
// mask.Unknown, under which mask.JSON shows only times, codes and numbers
// that cannot be account numbers: a key that Schwab adds shows in full once
// it has a class here, beside its rule in the scrub policy.
func TransactionKeyClass(path []string) mask.Class {
	if len(path) > 0 && strings.EqualFold(path[0], "user") {
		return mask.Withheld
	}
	c, _ := transactionKeyClasses.classOf(path)
	return c
}

// orderKeyClasses are the classes of an order's key paths, and of the
// orders it holds: its account number; its tag, which may name an app's
// developer; its IDs and codes; its times; its prices and quantities; and
// Schwab's sentence about a rejection, which is free text.
var orderKeyClasses = func() *keyClasses {
	t := &keyClasses{classes: slices.Concat(
		classesAt(mask.AccountNumber, "accountNumber"),
		classesAt(mask.Tag, "tag"),
		classesAt(mask.Identifier, "orderId", "status", "orderStrategyType", "orderType", "complexOrderStrategyType",
			"session", "duration", "requestedDestination", "destinationLinkName", "cancelable", "editable",
			"stopPriceLinkBasis", "stopPriceLinkType", "stopType", "priceLinkBasis", "priceLinkType", "taxLotMethod",
			"specialInstruction",
			"orderLegCollection.orderLegType", "orderLegCollection.legId", "orderLegCollection.instruction",
			"orderLegCollection.positionEffect", "orderLegCollection.quantityType", "orderLegCollection.divCapGains",
			"orderLegCollection.toSymbol",
			"orderActivityCollection.activityType", "orderActivityCollection.activityId",
			"orderActivityCollection.executionType", "orderActivityCollection.executionLegs.legId",
			"orderActivityCollection.executionLegs.instrumentId"),
		classesAt(mask.Time, "enteredTime", "closeTime", "cancelTime", "releaseTime",
			"orderActivityCollection.executionLegs.time"),
		classesAt(mask.Amount, "quantity", "filledQuantity", "remainingQuantity", "price", "stopPrice",
			"stopPriceOffset", "activationPrice", "orderLegCollection.quantity", "orderActivityCollection.quantity",
			"orderActivityCollection.orderRemainingQuantity", "orderActivityCollection.executionLegs.price",
			"orderActivityCollection.executionLegs.quantity", "orderActivityCollection.executionLegs.mismarkedQuantity"),
		classesAt(mask.FreeText, "statusDescription"),
	)}
	t.nested = []nestedKeyClasses{
		{keys: []string{"orderLegCollection", "instrument"}, table: instrumentKeyClasses},
		{keys: []string{"childOrderStrategies"}, table: t},
		{keys: []string{"replacingOrderCollection"}, table: t},
	}
	return t
}()

// OrderKeyClass returns the class of the values at path in an order's
// JSON, for mask.JSON, as TransactionKeyClass does of a transaction's: the
// account number becomes its masked form, a string, though Schwab sends a
// number; the tag is shown only as its origin (mask.Tag); and the orders
// that an order holds are classed as orders. A key that the table does not
// name is mask.Unknown.
func OrderKeyClass(path []string) mask.Class {
	c, _ := orderKeyClasses.classOf(path)
	return c
}
