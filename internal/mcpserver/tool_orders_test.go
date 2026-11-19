package mcpserver

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The order tools' tests join the shared ones.
var _ = addToolCases(orderCases...) && addLeaks(orderPlantings, orderLeakCalls...) && addSizeCases(orderSizeCases...)

// The fixtures' orders, by their IDs.
const (
	vtiWorking   = "1004518210415" // ...135's, entered 2026-09-28, GOOD_TILL_CANCEL
	spyRejected  = "1004517650222" // ...135's, 2025-06-02
	aaplFilled   = "1004517639411" // ...135's, 2025-06-02
	bracket      = "1004518301101" // ...398's TRIGGER, entered 2026-09-29, holding an OCO of two orders
	xyzCallSold  = "1004517638295" // ...398's, 2026-08-04
	metaSold     = "1004517688020" // ...398's, 2026-03-13, order.json
	verticalGone = "1004517610333" // ...398's canceled NET_CREDIT, 2025-09-30
	xyzPutBought = "1004517601177" // ...398's, 2025-01-21
)

// bareOrder is an order with only the keys that every order has, of the
// account number given, entered at entered.
func bareOrder(id, number, entered string) string {
	return `{"orderStrategyType":"SINGLE","orderId":` + id + `,"status":"WORKING","enteredTime":"` + entered +
		`","accountNumber":` + number + `}`
}

// ordersOf returns the orders of r, get_orders' answer.
func ordersOf(r result) []map[string]any {
	var out []map[string]any
	os, _ := r.value["orders"].([]any)
	for _, o := range os {
		m, _ := o.(map[string]any)
		out = append(out, m)
	}
	return out
}

// orderIDs returns the order_id of each of os.
func orderIDs(os []map[string]any) []string {
	ids := make([]string, len(os))
	for i, o := range os {
		ids[i] = fmt.Sprint(o["order_id"])
	}
	return ids
}

// orderCases are the order tools' cases of TestToolsEmptyAndFull.
var orderCases = []toolCase{
	{tool: "get_orders", name: "empty", args: `{"account":"783"}`, check: func(t *testing.T, e *env, r result) {
		as := accountsOf(t, r)
		if len(ordersOf(r)) != 0 || len(as) != 1 || as["...783"]["orders"] != 0.0 || r.value["next_cursor"] != nil ||
			r.value["complete"] != true {
			t.Errorf("an account without orders: %s", r.text)
		}
		want := []string{string(schwabtest.AccountNumbersRoute), string(schwabtest.UserPreferenceRoute), string(schwabtest.OrdersRoute)}
		if routes := apiRoutes(e.fake, 0); !slices.Equal(routes, want) {
			t.Errorf("get_orders sent %v, want %v", routes, want)
		}
		q := e.fake.Requests()[len(e.fake.Requests())-1].Query
		if !strings.Contains(q, "fromEnteredTime=2026-09-15T04%3A00%3A00.000Z") || !strings.Contains(q, "toEnteredTime=2026-11-13T21%3A40%3A00.000Z") {
			t.Errorf("the last 60 days, to now, asked as %s", q)
		}
	}},
	{tool: "get_orders", name: "full", args: `{"from":"2026-01-01","detail":"full"}`, check: func(t *testing.T, _ *env, r result) {
		os := ordersOf(r)
		if got, want := orderIDs(os), []string{bracket, vtiWorking, xyzCallSold, metaSold}; !slices.Equal(got, want) {
			t.Fatalf("orders %v, want %v, newest first", got, want)
		}
		held, _ := os[0]["held"].([]any)
		if len(held) != 3 {
			t.Fatalf("the bracket holds %v", held)
		}
		var parents, relations, types []string
		for _, h := range held {
			m := h.(map[string]any)
			parents = append(parents, fmt.Sprint(m["parent_id"]))
			relations = append(relations, fmt.Sprint(m["relation"]))
			types = append(types, fmt.Sprint(m["strategy"], " ", m["order_type"], " ", m["stop_price"]))
		}
		if !slices.Equal(parents, []string{bracket, "1004518301102", "1004518301102"}) ||
			!slices.Equal(relations, []string{"child", "child", "child"}) ||
			!slices.Equal(types, []string{"OCO <nil> <nil>", "SINGLE LIMIT <nil>", "SINGLE STOP 45"}) {
			t.Errorf("held %v %v %v", parents, relations, types)
		}
		if os[0]["open"] != true || os[3]["open"] != false || os[3]["closed"] != "2026-03-13T09:55:09-04:00" {
			t.Errorf("open and closed: %v %v %v", os[0]["open"], os[3]["open"], os[3]["closed"])
		}
		leg := os[2]["legs"].([]any)[0].(map[string]any)
		detail := leg["detail"].(map[string]any)
		if leg["fill_price"] != 3.12 || detail["strike"] != 50.0 || detail["expiration"] != "2026-09-18" || detail["put_call"] != "CALL" {
			t.Errorf("an option's leg: %v", leg)
		}
		d := os[1]["detail"].(map[string]any)
		if d["origin"] != "[withheld]" || d["venue"] != "CDRG" || d["cancelable"] != true || len(d["fills"].([]any)) != 0 {
			t.Errorf("a working order of an app's tag that names no TA_ origin: %v", d)
		}
		if d := os[3]["detail"].(map[string]any); d["origin"] != "API_TOS:TRADE_ALL" || d["cancelable"] != false {
			t.Errorf("an order of Schwab's own platform: %v", d)
		}
		fills := os[3]["detail"].(map[string]any)["fills"].([]any)
		if len(fills) != 1 || fills[0].(map[string]any)["price"] != 603.4 || fills[0].(map[string]any)["symbol"] != "META" {
			t.Errorf("the META sale's fills: %v", fills)
		}
		as := accountsOf(t, r)
		if as["...135"]["orders"] != 1.0 || as["...398"]["orders"] != 3.0 || as["...783"]["orders"] != 0.0 || len(r.warnings()) != 0 {
			t.Errorf("the accounts: %v, warnings %v", as, r.warnings())
		}
	}},
	{tool: "get_orders", name: "open", args: `{"open":true,"from":"2025-01-01","to":"2025-12-31"}`, check: func(t *testing.T, _ *env, r result) {
		if len(ordersOf(r)) != 0 {
			t.Errorf("open orders of 2025: %s", r.text)
		}
	}},
	{tool: "get_orders", name: "open_now", args: `{"open":true}`, check: func(t *testing.T, _ *env, r result) {
		if got := orderIDs(ordersOf(r)); !slices.Equal(got, []string{bracket, vtiWorking}) {
			t.Errorf("the open orders %v", got)
		}
	}},
	{
		tool: "get_orders", name: "filtered", args: `{"from":"2025-01-01","to":"2025-12-31","statuses":["filled","Canceled"],"symbol":"xyz"}`,
		check: func(t *testing.T, _ *env, r result) {
			if got := orderIDs(ordersOf(r)); !slices.Equal(got, []string{verticalGone, xyzPutBought}) {
				t.Errorf("XYZ's filled and canceled orders of 2025: %v", got)
			}
			if ordersOf(r)[0]["complex_strategy"] != "VERTICAL" || ordersOf(r)[1]["complex_strategy"] != nil {
				t.Errorf("complex strategies: %s", r.text)
			}
		},
	},
	{tool: "get_orders", name: "rejected", args: `{"account":"135","from":"2025-06-02","to":"2025-06-02"}`, check: func(t *testing.T, _ *env, r result) {
		os := ordersOf(r)
		if got := orderIDs(os); !slices.Equal(got, []string{aaplFilled, spyRejected}) {
			t.Fatalf("...135's orders of 2025-06-02: %v", got)
		}
		if os[1]["status_description"] != "Your limit price is significantly away from the current market price. Please adjust your order." ||
			os[0]["status_description"] != nil || os[0]["legs"].([]any)[0].(map[string]any)["fill_price"] != 201.82 {
			t.Errorf("a rejection and a fill: %s", r.text)
		}
	}},
	{tool: "get_orders", name: "too_long", args: `{"from":"2025-01-01"}`, check: func(t *testing.T, e *env, r result) {
		if r.code() != "bad_argument" || !strings.Contains(r.text, "at most 365 days") || e.fake.APICalls() != 0 {
			t.Errorf("a range over 365 days: %s, %d requests", r.text, e.fake.APICalls())
		}
	}},
	{tool: "get_orders", name: "one_fails", opts: []envOption{withFake(func(f *schwabtest.Server) {
		f.Override(schwabtest.OrdersRoute, func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("hash") == schwabtest.HashFor(options) {
				schwabtest.Answer(http.StatusServiceUnavailable, `{"message":"down"}`)(w, r)
				return
			}
			body := `[]`
			if r.PathValue("hash") == schwabtest.HashFor(individual) {
				body = `[` + bareOrder("1004519000001", individual, "2026-11-02T15:00:00+0000") + `]`
			}
			schwabtest.Answer(http.StatusOK, body)(w, r)
		})
	})}, args: `{}`, check: func(t *testing.T, _ *env, r result) {
		as := accountsOf(t, r)
		if as["...398"]["error"] != "schwab_unavailable" || as["...398"]["orders"] != nil || as["...135"]["orders"] != 1.0 ||
			!slices.Contains(r.warnings(), "account_failed") {
			t.Errorf("one account failing: %s", r.text)
		}
		if got := orderIDs(ordersOf(r)); !slices.Equal(got, []string{"1004519000001"}) {
			t.Errorf("the other accounts' orders: %v", got)
		}
	}},
	{tool: "get_order", name: "empty", opts: []envOption{withFake(func(f *schwabtest.Server) {
		f.AddOrders(schwabtest.HashFor(empty), bareOrder("1004519000002", empty, "2026-11-02T15:00:00+0000"))
	})}, args: `{"id":"1004519000002","account":"783"}`, check: func(t *testing.T, _ *env, r result) {
		o := r.value["order"].(map[string]any)
		if o["order_type"] != nil || o["price"] != nil || o["quantity"] != nil || len(o["legs"].([]any)) != 0 || r.value["raw"] != nil {
			t.Errorf("an order with only the keys every order has: %s", r.text)
		}
		if d := o["detail"].(map[string]any); d["origin"] != nil || len(d["problems"].([]any)) != 0 {
			t.Errorf("its detail: %v", d)
		}
	}},
	{tool: "get_order", name: "full", args: `{"id":"` + bracket + `","account":"398","include_raw":true}`, check: func(t *testing.T, _ *env, r result) {
		o := r.value["order"].(map[string]any)
		if len(o["held"].([]any)) != 3 || o["detail"] == nil {
			t.Errorf("the bracket: %s", r.text)
		}
		raw := string(r.raw)
		if !strings.Contains(raw, `"accountNumber":"...398"`) || strings.Contains(raw, options) || !strings.Contains(raw, `"stopPrice":45.0`) {
			t.Errorf("Schwab's JSON, masked: %s", raw)
		}
	}},
	{tool: "get_order", name: "by_id", args: `{"id":"` + metaSold + `","account":"Options & Margin"}`, check: func(t *testing.T, e *env, r result) {
		o := r.value["order"].(map[string]any)
		if o["filled"] != 1.0 || o["remaining"] != 0.0 || o["legs"].([]any)[0].(map[string]any)["fill_price"] != 603.4 {
			t.Errorf("order.json: %s", r.text)
		}
		rs := e.fake.Requests()
		if last := rs[len(rs)-1]; last.Route != schwabtest.OrderRoute || !strings.HasSuffix(last.Path, "/orders/"+metaSold) {
			t.Errorf("asked %s", last.URL)
		}
	}},
	{tool: "get_order", name: "needs_account", args: `{"id":"` + metaSold + `"}`, check: func(t *testing.T, _ *env, r result) {
		if r.code() != "bad_argument" || !strings.Contains(r.text, "account is needed: the login shares 3 accounts") {
			t.Errorf("an order of one of several accounts: %s", r.text)
		}
	}},
	{tool: "get_order", name: "not_found", args: `{"id":"9999999999999","account":"398"}`, check: func(t *testing.T, _ *env, r result) {
		if r.code() != "not_found" || !strings.Contains(r.text, "Schwab has no order 9999999999999 in account ...398.") {
			t.Errorf("an order Schwab does not have: %s", r.text)
		}
	}},
	{tool: "get_order", name: "bad_id", args: `{"id":"99999999999999999999","account":"398"}`, check: func(t *testing.T, e *env, r result) {
		if r.code() != "bad_argument" || e.fake.APICalls() != 0 {
			t.Errorf("an id past int64: %s", r.text)
		}
	}},
}

// get_orders pages its orders, newest first, through every account's, each
// once; a cursor of other arguments is bad_cursor.
func TestOrdersPages(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var ids []string
	args := map[string]any{"from": "2025-11-14", "limit": 1}
	for page := 0; ; page++ {
		r := e.call("get_orders", args)
		if r.isError {
			t.Fatal(r.text)
		}
		ids = append(ids, orderIDs(ordersOf(r))...)
		cursor, _ := r.value["next_cursor"].(string)
		if cursor == "" {
			break
		}
		if page > 10 {
			t.Fatal("too many pages")
		}
		args["cursor"] = cursor
	}
	if want := []string{bracket, vtiWorking, xyzCallSold, metaSold}; !slices.Equal(ids, want) {
		t.Errorf("pages hold %v, want %v", ids, want)
	}
	r := e.call("get_orders", map[string]any{"from": "2025-11-14", "limit": 1})
	cursor := r.value["next_cursor"]
	if r := e.call("get_orders", map[string]any{"from": "2025-11-15", "cursor": cursor}); r.code() != "bad_cursor" {
		t.Errorf("a cursor of other arguments: %s", r.text)
	}
	if r := e.call("get_transactions", map[string]any{"cursor": cursor}); r.code() != "bad_cursor" {
		t.Errorf("a cursor of another tool: %s", r.text)
	}
}

// Every account's orders read one request each; Schwab's most orders, sent
// for one account, says that older ones may be missing.
func TestOrdersCapped(t *testing.T) {
	t.Parallel()
	var orders []string
	for i := range schwab.MaxOrders {
		orders = append(orders, bareOrder(strconv.Itoa(1004520000000+i), individual, "2026-11-02T15:00:00+0000"))
	}
	body := "[" + strings.Join(orders, ",") + "]"
	e := newEnv(t, withFake(func(f *schwabtest.Server) {
		f.Override(schwabtest.OrdersRoute, schwabtest.Answer(http.StatusOK, body))
	}))
	r := e.call("get_orders", `{"account":"135","limit":3}`)
	if r.isError || r.value["complete"] != false || !slices.Contains(r.warnings(), "partial") || len(ordersOf(r)) != 3 ||
		accountsOf(t, r)["...135"]["orders"] != float64(schwab.MaxOrders) {
		t.Errorf("Schwab's most orders: %.600s", r.text)
	}
}

// Both reference pages of orders are served as Markdown, private, for a
// day, and need no request: the statuses' page names each of Schwab's
// statuses with whether it is final, as OrderStatus.Final says, and the
// types' page each of the strategies the tools show.
func TestOrderPages(t *testing.T) {
	t.Parallel()
	for _, protocol := range caseProtocols {
		e := newEnv(t, withProtocol(protocol))
		list, err := e.cs.ListResources(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		pages := map[string]string{}
		for _, name := range []string{"order-types", "order-statuses"} {
			uri := referencePrefix + name
			if !slices.ContainsFunc(list.Resources, func(r *mcp.Resource) bool { return r.URI == uri && r.MIMEType == "text/markdown" }) {
				t.Errorf("resources/list lacks %s", uri)
			}
			r, err := e.readResource(uri)
			if err != nil {
				t.Fatal(err)
			}
			if r.Contents[0].URI != uri || r.Contents[0].MIMEType != "text/markdown" || r.CacheScope != "private" || r.TTLMs != 86_400_000 ||
				!strings.Contains(r.Contents[0].Text, "as Schwab's spec lists them; Schwab may send others") {
				t.Errorf("%s: %+v", uri, r)
			}
			pages[name] = r.Contents[0].Text
		}
		for _, s := range schwab.AllOrderStatuses() {
			final := "no"
			if s.Final() {
				final = "yes"
			}
			if !strings.Contains(pages["order-statuses"], "\n| "+string(s)+" | "+final+" | ") {
				t.Errorf("the statuses' page does not describe %s as final %s", s, final)
			}
		}
		for _, word := range []string{
			"MARKET", "LIMIT", "STOP", "STOP_LIMIT", "NET_DEBIT", "NET_CREDIT", "TRIGGER", "OCO",
			"VERTICAL", "GOOD_TILL_CANCEL", "SELL_TO_OPEN", "parent_id", "fill_price", "stop_price",
		} {
			if !strings.Contains(pages["order-types"], word) {
				t.Errorf("the types' page does not name %s", word)
			}
		}
		if n := e.fake.APICalls() + e.fake.TokenRequests(); n != 1 {
			t.Errorf("reading the pages sent %d requests to Schwab", n-1)
		}
	}
}

// orderPlantings plant account numbers where an order holds text from
// outside: Schwab's sentence about a rejection, and an app's tag, whose
// text may name its developer; and a number in a key no table knows.
var orderPlantings = []func(t *testing.T, w *leakWorld){func(_ *testing.T, w *leakWorld) {
	w.opts = append(w.opts, withFake(func(f *schwabtest.Server) {
		f.AddOrders(schwabtest.HashFor(options),
			`{"orderStrategyType":"SINGLE","orderId":1004519000003,"status":"REJECTED","enteredTime":"2026-11-02T15:00:00+0000",`+
				`"accountNumber":`+options+`,"statusDescription":"Account 29470135 may not trade this. Call about 61803398.",`+
				`"tag":"TA_dev`+options+`@example.com","newKey":`+individual+`,"orderLegCollection":[{"orderLegType":"EQUITY",`+
				`"legId":1,"instrument":{"assetType":"EQUITY","symbol":"XYZ","description":"XYZ FOR ACCT `+individual+`"},`+
				`"instruction":"BUY","quantity":1.0}]}`)
	}))
	w.masked = append(w.masked, "Account ...135 may not trade this. Call about ...398.", "TA_...", "XYZ FOR ACCT ...135")
	w.forbidden = append(w.forbidden, "dev"+options+"@example.com", "example.com")
}}

// orderLeakCalls are the order tools' calls of TestNoAccountNumberLeaks:
// every account's orders in full, one account's by its full number, and
// the planted order with Schwab's JSON.
var orderLeakCalls = []leakCall{
	{tool: "get_orders", args: `{"detail":"full"}`},
	{tool: "get_orders", args: `{"account":"61803398","statuses":["REJECTED"]}`},
	{tool: "get_order", args: `{"id":"1004519000003","account":"398","include_raw":true}`},
}

// orderSizeCases are the order tools' cases of TestSizeCap: an order filled
// in 600 parts, whose fills pass the cap, fits by cutting them, Schwab's
// JSON first; and get_orders' pages of an account of 400 orders stay
// within the page budget.
var orderSizeCases = []sizeCase{{name: "get_order of many fills", opts: []envOption{withFake(func(f *schwabtest.Server) {
	var fills []string
	for i := range 600 {
		fills = append(fills, fmt.Sprintf(`{"activityType":"EXECUTION","activityId":%d,"executionType":"FILL","quantity":1.0,`+
			`"orderRemainingQuantity":%d.0,"executionLegs":[{"legId":1,"price":10.01,"quantity":1.0,"mismarkedQuantity":0.0,`+
			`"instrumentId":5215623,"time":"2026-11-02T15:%02d:%02d+0000"}]}`, 91437900000+i, 599-i, i/60, i%60))
	}
	f.AddOrders(schwabtest.HashFor(individual), `{"orderStrategyType":"SINGLE","orderId":1004519000004,"status":"FILLED",`+
		`"enteredTime":"2026-11-02T15:00:00+0000","accountNumber":`+individual+`,"orderType":"LIMIT","price":10.01,"quantity":600.0,`+
		`"filledQuantity":600.0,"remainingQuantity":0.0,"orderLegCollection":[{"orderLegType":"EQUITY","legId":1,`+
		`"instrument":{"assetType":"COLLECTIVE_INVESTMENT","symbol":"VTI","instrumentId":5215623},"instruction":"BUY",`+
		`"quantity":600.0}],"orderActivityCollection":[`+strings.Join(fills, ",")+`]}`)
})}, run: func(t *testing.T, e *env) {
	r := e.call("get_order", `{"id":"1004519000004","account":"135","include_raw":true}`)
	if r.isError {
		t.Fatal(r.text)
	}
	cut, _ := r.value["cut"].([]any)
	if r.value["raw"] != nil || !slices.Contains(r.warnings(), "raw_omitted") || !slices.Contains(r.warnings(), "list_cut") ||
		len(cut) != 1 || cut[0].(map[string]any)["path"] != "order.detail.fills" || cut[0].(map[string]any)["total"] != 600.0 {
		t.Errorf("an order of 600 fills: %v, cut %v", r.warnings(), cut)
	}
	o := r.value["order"].(map[string]any)
	if o["legs"].([]any)[0].(map[string]any)["fill_price"] != 10.01 || o["filled"] != 600.0 {
		t.Errorf("the figures stay whole: %v", o)
	}
}}, {name: "get_orders pages", opts: []envOption{withFake(func(f *schwabtest.Server) {
	var orders []string
	for i := range 400 {
		orders = append(orders, fmt.Sprintf(`{"orderStrategyType":"SINGLE","orderId":%d,"status":"FILLED",`+
			`"enteredTime":"2026-11-02T15:%02d:%02d+0000","accountNumber":%s,"orderType":"LIMIT","price":10.01,`+
			`"orderLegCollection":[{"orderLegType":"EQUITY","legId":1,"instrument":{"assetType":"EQUITY","symbol":"S%03d"},`+
			`"instruction":"BUY","quantity":1.0}]}`, 1004521000000+i, i/60, i%60, individual, i))
	}
	f.AddOrders(schwabtest.HashFor(individual), orders...)
})}, run: func(t *testing.T, e *env) {
	args := map[string]any{"account": "135", "from": "2026-11-02", "to": "2026-11-02", "limit": 200}
	seen := 0
	for page := 0; ; page++ {
		r := e.call("get_orders", args)
		if r.isError {
			t.Fatal(r.text)
		}
		n := len(ordersOf(r))
		seen += n
		cursor, _ := r.value["next_cursor"].(string)
		if len(r.text) > e.srv.o.PageBytes || cursor != "" && (n >= 200 || len(r.text) < e.srv.o.PageBytes-1000) {
			t.Errorf("page %d holds %d orders in %d bytes", page, n, len(r.text))
		}
		if cursor == "" || page > 50 {
			break
		}
		args["cursor"] = cursor
	}
	if seen != 400 {
		t.Errorf("the pages hold %d orders, want 400", seen)
	}
}}}
