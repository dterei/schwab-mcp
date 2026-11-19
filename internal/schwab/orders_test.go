package schwab

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
)

// An account's orders are asked for by their entry times, in UTC with the
// milliseconds Schwab takes, with status= and maxResults= only when the
// query sets them; every account's are asked for the same way, on their
// own path.
func TestOrdersRequest(t *testing.T) {
	list := string(readTestdata(t, "orders.json"))
	cl, f := endpointClient(t, ok(list), ok(`[]`), ok(list))
	start := time.Date(2026, 1, 2, 3, 4, 5, 678e6, time.FixedZone("EST", -5*3600))
	end := start.Add(30 * 24 * time.Hour)
	got, err := cl.Orders(t.Context(), "AB/C", OrderQuery{Start: start, End: end})
	if err != nil || len(got) != 8 {
		t.Fatalf("Orders() = %d, %v", len(got), err)
	}
	got, err = cl.Orders(t.Context(), "AB/C", OrderQuery{Start: start, End: end, Status: OrderWorking, MaxResults: 25})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("a window without orders gave %v, %v; want an empty list", got, err)
	}
	if got, err = cl.AllOrders(t.Context(), OrderQuery{Start: start, End: end, MaxResults: MaxOrders}); err != nil || len(got) != 8 {
		t.Fatalf("AllOrders() = %d, %v", len(got), err)
	}
	u := requested(t, f)
	if len(u) != 3 || u[0].EscapedPath() != "/trader/v1/accounts/AB%2FC/orders" || u[2].Path != "/trader/v1/orders" {
		t.Fatalf("requested %v", u)
	}
	q := u[0].Query()
	if q.Get("fromEnteredTime") != "2026-01-02T08:04:05.678Z" || q.Get("toEnteredTime") != "2026-02-01T08:04:05.678Z" ||
		q.Has("status") || q.Has("maxResults") || len(q) != 2 {
		t.Errorf("default query %v", q)
	}
	if q := u[1].Query(); q.Get("status") != "WORKING" || q.Get("maxResults") != "25" {
		t.Errorf("query %v", q)
	}
	if q := u[2].Query(); q.Get("fromEnteredTime") != "2026-01-02T08:04:05.678Z" || q.Get("maxResults") != "3000" {
		t.Errorf("every account's query %v", q)
	}
}

// A query Schwab would refuse fails before any request.
func TestOrdersWindowRules(t *testing.T) {
	cl, f := endpointClient(t)
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for name, q := range map[string]OrderQuery{
		"no start":              {End: start},
		"no end":                {Start: start},
		"an end before":         {Start: start, End: start.Add(-time.Second)},
		"a window of no length": {Start: start, End: start},
		"a window too long":     {Start: start, End: start.Add(MaxOrderWindow + time.Second)},
		"too many":              {Start: start, End: start.Add(time.Hour), MaxResults: MaxOrders + 1},
		"fewer than none":       {Start: start, End: start.Add(time.Hour), MaxResults: -1},
	} {
		if _, err := cl.Orders(t.Context(), "HASH", q); err == nil {
			t.Errorf("%s: no error", name)
		}
		if _, err := cl.AllOrders(t.Context(), q); err == nil {
			t.Errorf("%s, of every account: no error", name)
		}
	}
	if _, err := cl.Orders(t.Context(), "", OrderQuery{Start: start, End: start.Add(time.Hour)}); err == nil {
		t.Error("no hash: no error")
	}
	if _, err := cl.Order(t.Context(), "", 1); err == nil {
		t.Error("an order of no account did not fail")
	}
	if u := requested(t, f); len(u) != 0 {
		t.Errorf("sent %v", u)
	}
	cl, f = endpointClient(t, ok(`[]`))
	if _, err := cl.Orders(t.Context(), "HASH", OrderQuery{Start: start, End: start.Add(MaxOrderWindow)}); err != nil {
		t.Errorf("a window of exactly %v: %v", MaxOrderWindow, err)
	}
	if u := requested(t, f); len(u) != 1 {
		t.Errorf("sent %d requests, want 1", len(u))
	}
}

func TestOrderByID(t *testing.T) {
	one := string(readTestdata(t, "order.json"))
	cl, f := endpointClient(t, ok(one), ok(`[]`))
	got, err := cl.Order(t.Context(), "HASH", 1004517688020)
	if err != nil || got.OrderID != 1004517688020 || got.AccountNumber != 61803398 || len(got.Problems) > 0 {
		t.Fatalf("Order() = %+v, %v", got, err)
	}
	if _, err := cl.Order(t.Context(), "HASH", 1); err == nil {
		t.Error("a list did not fail")
	}
	if u := requested(t, f); len(u) != 2 || u[0].Path != "/trader/v1/accounts/HASH/orders/1004517688020" || u[0].RawQuery != "" {
		t.Errorf("requested %v", u)
	}
}

// The fixtures' orders come back from the fake as they are, each account's
// over the years that hold them and every account's together, each by its
// id too, order.json's by its own bytes, all without problems; the bracket's
// orders hold its OCO and the OCO its two orders.
func TestOrdersFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	all, err := decodeList[Order](readTestdata(t, "orders.json"))
	if err != nil {
		t.Fatal(err)
	}
	one := bytes.TrimSpace(readTestdata(t, "order.json"))
	years := []OrderQuery{
		{Start: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC)},
		{Start: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)},
	}
	hashes := fixtureHashes()
	var listed, everyone []Order
	for _, fa := range []string{"29470135", "61803398", "40221783"} {
		for _, q := range years {
			got, err := cl.Orders(t.Context(), hashes[fa], q)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i < len(got); i++ {
				if got[i].EnteredTime.After(got[i-1].EnteredTime) {
					t.Errorf("account %s: order %d entered after the order before it", fa, got[i].OrderID)
				}
			}
			listed = append(listed, got...)
		}
	}
	for _, q := range years {
		got, err := cl.AllOrders(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		everyone = append(everyone, got...)
	}
	ids := func(os []Order) []int64 {
		out := make([]int64, len(os))
		for i, o := range os {
			out[i] = o.OrderID
			if len(o.Problems) > 0 {
				t.Errorf("order %d: problems %q", o.OrderID, o.Problems)
			}
		}
		slices.Sort(out)
		return out
	}
	if want := ids(all); !slices.Equal(ids(listed), want) || !slices.Equal(ids(everyone), want) {
		t.Errorf("listed %v and %v, want %v", ids(listed), ids(everyone), want)
	}
	byID, err := decodeOne[Order](one)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, o := range all {
		want := o.Raw
		if o.OrderID == byID.OrderID {
			want, found = one, true
		}
		got, err := cl.Order(t.Context(), hashes[strconv.FormatInt(o.AccountNumber, 10)], o.OrderID)
		if err != nil || !bytes.Equal(got.Raw, want) || len(got.Problems) > 0 {
			t.Errorf("order %d: %v, problems %q", o.OrderID, err, got.Problems)
		}
		if o.OrderStrategyType == "TRIGGER" {
			if len(o.ChildOrderStrategies) != 1 || len(o.ChildOrderStrategies[0].ChildOrderStrategies) != 2 ||
				o.ChildOrderStrategies[0].ChildOrderStrategies[1].StopPrice == nil {
				t.Errorf("the bracket holds %+v", o.ChildOrderStrategies)
			}
		}
	}
	if !found {
		t.Error("no order of the list has order.json's id")
	}
	if n := len(requested(t, f)); n != 6+2+len(all) {
		t.Errorf("%d requests", n)
	}
}

// An order reads each of its parts, at any depth, and its problems hold
// those of its instruments, its executions' legs and the orders it holds,
// each under its key; a null among the orders it holds is a zero order
// with a problem, and a null among an execution's legs is a problem too.
func TestOrderProblems(t *testing.T) {
	body := withKeys(baseOrder,
		"orderLegCollection", `[{"orderLegType":"EQUITY","legId":1,"instrument":{"assetType":"EQUITY","symbol":"X","strikePrice":"9"},"instruction":"BUY","quantity":1}]`,
		"orderActivityCollection", `[{"activityType":"EXECUTION","executionType":"FILL","executionLegs":[null,`+
			`{"legId":1,"price":1,"quantity":1,"mismarkedQuantity":0,"instrumentId":1,"time":"soon"}]}]`,
		"childOrderStrategies", "["+withKeys(baseOrder, "orderStrategyType", `"OCO"`, "childOrderStrategies",
			"["+withKeys(baseOrder, "status", "null")+"]")+",null]",
	)
	var o Order
	if err := json.Unmarshal([]byte(body), &o); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"orderLegCollection.instrument.strikePrice: Schwab sent a string where a number was expected",
		"orderActivityCollection.executionLegs: Schwab sent null where an object was expected",
		`orderActivityCollection.executionLegs.time: Schwab sent "soon", which is not a time`,
		"childOrderStrategies.childOrderStrategies.status: missing, though Schwab sends it on every record of its kind",
		"childOrderStrategies: Schwab sent null where an object was expected",
	}
	if got := problemStrings(o.Problems); !slices.Equal(got, want) {
		t.Errorf("problems\n got %q\nwant %q", got, want)
	}
	if n := len(o.ChildOrderStrategies); n != 2 || o.ChildOrderStrategies[1].OrderID != 0 {
		t.Errorf("held %d orders: %+v", n, o.ChildOrderStrategies)
	}
	if got, want := problemStrings(o.ChildOrderStrategies[0].Problems),
		[]string{"childOrderStrategies.status: missing, though Schwab sends it on every record of its kind"}; !slices.Equal(got, want) {
		t.Errorf("the OCO's own problems %q, want %q", got, want)
	}
	if o.OrderLegCollection[0].Instrument.problems != nil || o.OrderActivityCollection[0].ExecutionLegs[1].problems != nil {
		t.Error("a nested part kept problems that the order reports")
	}
}

// Fills are the legs of the executions whose type is FILL, in order; a
// cancel's legs are not fills. An order is done once filled, canceled,
// rejected, expired or replaced.
func TestFillsAndFinal(t *testing.T) {
	leg := func(price float64) ExecutionLeg { return ExecutionLeg{LegID: 1, Price: price, Quantity: 1} }
	o := Order{OrderActivityCollection: []OrderActivity{
		{ExecutionType: "FILL", ExecutionLegs: []ExecutionLeg{leg(1), leg(2)}},
		{ExecutionType: "CANCELED", ExecutionLegs: []ExecutionLeg{leg(0)}},
		{ExecutionType: "FILL", ExecutionLegs: []ExecutionLeg{leg(3)}},
	}}
	var prices []float64
	for _, l := range o.Fills() {
		prices = append(prices, l.Price)
	}
	if !slices.Equal(prices, []float64{1, 2, 3}) {
		t.Errorf("fills at %v", prices)
	}
	var final []OrderStatus
	for _, s := range AllOrderStatuses() {
		if s.Final() {
			final = append(final, s)
		}
	}
	if want := []OrderStatus{OrderRejected, OrderCanceled, OrderReplaced, OrderFilled, OrderExpired}; !slices.Equal(final, want) {
		t.Errorf("final statuses %v, want %v", final, want)
	}
	if len(AllOrderStatuses()) != 21 || OrderStatus("SOMETHING_NEW").Final() {
		t.Error("the spec lists 21 statuses, and a status it does not list is not final")
	}
}

// An order's raw JSON is masked by key: its account number, a number,
// becomes the masked string; a tag of another app than Schwab's own
// platform is cut to its origin; Schwab's sentence about a rejection is free
// text; and the orders it holds are classed as orders, at any depth.
func TestOrderKeyClass(t *testing.T) {
	for path, want := range map[string]mask.Class{
		"accountNumber":                        mask.AccountNumber,
		"tag":                                  mask.Tag,
		"statusDescription":                    mask.FreeText,
		"orderId":                              mask.Identifier,
		"enteredTime":                          mask.Time,
		"price":                                mask.Amount,
		"orderLegCollection.instrument.symbol": mask.Identifier,
		"orderLegCollection.instrument.description":               mask.FreeText,
		"orderActivityCollection.executionLegs.time":              mask.Time,
		"childOrderStrategies.childOrderStrategies.accountNumber": mask.AccountNumber,
		"replacingOrderCollection.tag":                            mask.Tag,
		"somethingNew":                                            mask.Unknown,
	} {
		if got := OrderKeyClass(strings.Split(path, ".")); got != want {
			t.Errorf("%s: %s, want %s", path, className(got), className(want))
		}
	}
	raw := []byte(withKeys(baseOrder, "tag", `"TA_someone@example.com"`, "statusDescription", `"Call 12345678"`))
	out, _, err := mask.JSON(raw, OrderKeyClass, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"61803398", "someone", "12345678"} {
		if bytes.Contains(out, []byte(leak)) {
			t.Errorf("%s holds %s", out, leak)
		}
	}
	if !bytes.Contains(out, []byte(`"accountNumber":"...398"`)) || !bytes.Contains(out, []byte(`"tag":"TA_..."`)) {
		t.Errorf("masked %s", out)
	}
}
