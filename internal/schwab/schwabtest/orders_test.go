package schwabtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// orderJSON returns an order with the given id, entry time and status, as
// the fake reads one.
func orderJSON(id, entered, status string) string {
	return `{"orderStrategyType":"SINGLE","orderId":` + id + `,"status":"` + status + `","enteredTime":"` + entered + `"}`
}

// orderQuery returns the query of a request for the orders entered in a
// window, and more parameters, in pairs.
func orderQuery(from, to string, more ...string) string {
	q := url.Values{"fromEnteredTime": {from}, "toEnteredTime": {to}}
	for i := 0; i+1 < len(more); i += 2 {
		q.Set(more[i], more[i+1])
	}
	return q.Encode()
}

// ordersWorld returns a function that GETs a path of the API of a fake with
// two accounts and their orders, and the accounts' hashes.
func ordersWorld(t *testing.T) (get func(path string) *httptest.ResponseRecorder, h1, h2 string) {
	t.Helper()
	s, get := loggedIn(t, t)
	s.AddAccount(Account{Number: "11111111", Details: `{"securitiesAccount":{"accountNumber":"11111111"}}`})
	s.AddAccount(Account{Number: "22222222", Details: `{"securitiesAccount":{"accountNumber":"22222222"}}`})
	h1, h2 = HashFor("11111111"), HashFor("22222222")
	s.AddOrders(h1, testOrders["o1"], testOrders["o2"], testOrders["o3"])
	s.AddOrders(h2, testOrders["o4"])
	return get, h1, h2
}

// testOrders are orders for ordersWorld: o2 and o3 entered at one time,
// o3 with the larger id; o4 in another account.
var testOrders = map[string]string{
	"o1": orderJSON("1001", "2026-01-05T15:00:00+0000", "FILLED"),
	"o2": orderJSON("1002", "2026-01-20T15:00:00+0000", "WORKING"),
	"o3": orderJSON("1003", "2026-01-20T15:00:00+0000", "FILLED"),
	"o4": `{ "orderId" : 2001, "status": "CANCELED", "enteredTime": "2026-01-10T15:00:00.000Z", "accountNumber": 22222222 }`,
}

// testOrderList returns the answer that holds the named orders, in order.
func testOrderList(names ...string) string {
	os := make([]string, len(names))
	for i, n := range names {
		os[i] = testOrders[n]
	}
	return "[" + strings.Join(os, ",") + "]"
}

// An account's orders are those entered in the window, both ends
// included, newest first by entry, a larger id first at one time, at most
// maxResults of them, and of status= only; every account's are merged in
// the same order. An order is served by its id, as it was given.
func TestOrders(t *testing.T) {
	get, h1, h2 := ordersWorld(t)
	orders := func(hash, query string) *httptest.ResponseRecorder {
		return get("/trader/v1/accounts/" + hash + "/orders?" + query)
	}
	wantAnswer(t, orders(h1, orderQuery(january, month)), testOrderList("o3", "o2", "o1"))
	wantAnswer(t, orders(h1, orderQuery("2026-01-05T15:00:00Z", "2026-01-20T15:00:00Z")), testOrderList("o3", "o2", "o1"))
	wantAnswer(t, orders(h1, orderQuery("2026-01-05T15:00:00.001Z", "2026-01-20T14:59:59.999Z")), "[]")
	wantAnswer(t, orders(h1, orderQuery(january, month, "status", "FILLED")), testOrderList("o3", "o1"))
	wantAnswer(t, orders(h1, orderQuery(january, month, "status", "EXPIRED")), "[]")
	wantAnswer(t, orders(h1, orderQuery(january, month, "maxResults", "2")), testOrderList("o3", "o2"))
	wantAnswer(t, orders(h2, orderQuery(january, month)), testOrderList("o4"))
	wantAnswer(t, get("/trader/v1/orders?"+orderQuery(january, month)), testOrderList("o3", "o2", "o4", "o1"))
	wantAnswer(t, get("/trader/v1/orders?"+orderQuery(january, month, "status", "CANCELED", "maxResults", "1")), testOrderList("o4"))
	wantInvalid(t, orders(UnknownHash, orderQuery(january, month)))

	wantAnswer(t, get("/trader/v1/accounts/"+h1+"/orders/1002"), testOrders["o2"])
	wantAnswer(t, get("/trader/v1/accounts/"+h2+"/orders/2001"), testOrders["o4"])
	wantInvalid(t, get("/trader/v1/accounts/"+UnknownHash+"/orders/1002"))
	for _, path := range []string{
		"/trader/v1/accounts/" + h2 + "/orders/1002",
		"/trader/v1/accounts/" + h1 + "/orders/" + strconv.FormatInt(UnknownOrderID, 10),
	} {
		if rec := get(path); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "a guess") {
			t.Errorf("%s: %d %s, want the 404 that says it is a guess", path, rec.Code, rec.Body)
		}
	}
	if rec := get("/trader/v1/accounts/" + h1 + "/orders/x"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "a guess") {
		t.Errorf("an id that is not a number: %d %s", rec.Code, rec.Body)
	}
}

// A request for orders without both times, with a time in another form,
// with an end before its start or a year after it, or with a maxResults
// that is not a whole number from 1 up, gets a 400 that says it is a
// guess, from both lists.
func TestOrdersQuery(t *testing.T) {
	get, h1, _ := ordersWorld(t)
	for _, query := range []string{
		"",
		url.Values{"fromEnteredTime": {january}}.Encode(),
		url.Values{"toEnteredTime": {month}}.Encode(),
		orderQuery("2026-01-01", month),
		orderQuery(january, "2026-01-31T00:00:00+0000"),
		orderQuery(month, january),
		orderQuery(january, "2027-01-01T00:00:00.001Z"),
		orderQuery(january, month, "maxResults", "0"),
		orderQuery(january, month, "maxResults", "-1"),
		orderQuery(january, month, "maxResults", "1.0"),
		orderQuery(january, month, "maxResults", "01"),
	} {
		for _, path := range []string{"/trader/v1/accounts/" + h1 + "/orders?", "/trader/v1/orders?"} {
			rec := get(path + query)
			if rec.Code != http.StatusBadRequest || !strings.HasPrefix(rec.Body.String(), `{"message":"a guess: `) {
				t.Errorf("%s%s: %d %s, want a 400 that says it is a guess", path, query, rec.Code, rec.Body)
			}
			wantFakeHeaders(t, rec.Header(), http.StatusBadRequest)
		}
	}
	wantAnswer(t, get("/trader/v1/accounts/"+h1+"/orders?"+orderQuery(january, "2027-01-01T00:00:00.000Z")), testOrderList("o3", "o2", "o1"))
}

// AddOrders refuses an order that the lists or orders/{id} could not go
// by, and an id that the account has already, and adds none of a call's
// orders then; an account's orders go with it when it is removed or
// rehashed.
func TestAddOrders(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.AddAccount(Account{Number: "11111111", Details: `{}`})
	h := HashFor("11111111")
	for bad, want := range map[string]string{
		`[]`: "not a JSON object",
		`{"orderId":"1","enteredTime":"2026-01-05T15:00:00+0000"}`: "no integer orderId",
		`{"orderId":1.5,"enteredTime":"2026-01-05T15:00:00+0000"}`: "no integer orderId",
		`{"orderId":1}`: "no enteredTime",
		`{"orderId":1,"enteredTime":"2026-01-05"}`:                 "no enteredTime",
		`{"orderId":1,"enteredTime":"2026-01-05T15:00:00+0000"} x`: "not a JSON object",
	} {
		wantReported(t, r, want, func() { s.AddOrders(h, bad) })
	}
	wantReported(t, r, "no account", func() { s.AddOrders(UnknownHash, testOrders["o1"]) })
	wantReported(t, r, "already", func() { s.AddOrders(h, testOrders["o2"], testOrders["o1"], testOrders["o1"]) })
	wantAnswer(t, get("/trader/v1/accounts/"+h+"/orders?"+orderQuery(january, month)), "[]")
	s.AddOrders(h, testOrders["o1"])
	wantReported(t, r, "already", func() { s.AddOrders(h, testOrders["o1"]) })

	s.Rehash(h, "H2")
	wantAnswer(t, get("/trader/v1/accounts/H2/orders?"+orderQuery(january, month)), testOrderList("o1"))
	wantAnswer(t, get("/trader/v1/orders?"+orderQuery(january, month)), testOrderList("o1"))
	s.RemoveAccount("H2")
	wantAnswer(t, get("/trader/v1/orders?"+orderQuery(january, month)), "[]")
	if errs := r.took(); len(errs) > 0 {
		t.Errorf("reported %q", errs)
	}
}

// LoadFixtures serves orders.json's orders in their accounts, each by its
// id too, order.json in place of its own, and no order of the third
// account.
func TestLoadFixturesOrders(t *testing.T) {
	s, get := loggedIn(t, t)
	s.LoadFixtures()
	elementsOf := func(b []byte) []string {
		t.Helper()
		es, err := elements(b)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = string(e)
		}
		return out
	}
	var orders []struct {
		OrderID       int64       `json:"orderId"`
		AccountNumber json.Number `json:"accountNumber"`
	}
	all := elementsOf(Fixture(t, "orders.json"))
	if err := json.Unmarshal(Fixture(t, "orders.json"), &orders); err != nil {
		t.Fatal(err)
	}
	var one struct {
		OrderID int64 `json:"orderId"`
	}
	if err := json.Unmarshal(Fixture(t, "order.json"), &one); err != nil {
		t.Fatal(err)
	}
	everyone := elementsOf(get("/trader/v1/orders?" + orderQuery("2024-01-01T00:00:00.000Z", "2024-12-31T23:59:59.999Z")).Body.Bytes())
	if len(everyone) != 0 {
		t.Errorf("orders entered in 2024: %d", len(everyone))
	}
	byOne := false
	for _, a := range FixtureAccounts {
		var want []string
		for i, o := range orders {
			if o.AccountNumber.String() == a.Number {
				want = append(want, all[i])
			}
		}
		var got []string
		for _, w := range [][2]string{
			{"2025-01-01T00:00:00.000Z", "2025-12-31T23:59:59.999Z"},
			{"2026-01-01T00:00:00.000Z", "2026-12-31T23:59:59.999Z"},
		} {
			rec := get("/trader/v1/accounts/" + a.Hash + "/orders?" + orderQuery(w[0], w[1]))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", a.Number, rec.Code, rec.Body)
			}
			got = append(got, elementsOf(rec.Body.Bytes())...)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("account %s: served %d orders, want its %d", a.Number, len(got), len(want))
		}
		if a == FixtureAccounts[2] && len(got) != 0 {
			t.Errorf("the third account has %d orders, want none", len(got))
		}
		for i, o := range orders {
			if o.AccountNumber.String() != a.Number {
				continue
			}
			want := all[i]
			if o.OrderID == one.OrderID {
				want, byOne = string(Fixture(t, "order.json")), true
			}
			wantAnswer(t, get("/trader/v1/accounts/"+a.Hash+"/orders/"+strconv.FormatInt(o.OrderID, 10)), want)
		}
	}
	if !byOne {
		t.Errorf("order.json's order %d is in no account of orders.json", one.OrderID)
	}
}
