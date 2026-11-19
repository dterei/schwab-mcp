package schwabtest

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"
)

// MaxOrders is the most orders that the fake sends in one answer when the
// query sets no maxResults: Schwab's spec gives 3000 as the default.
const MaxOrders = 3000

// UnknownOrderID is an order ID that no fixture's order has, for a test or
// a probe to learn how Schwab answers an order it does not know.
const UnknownOrderID int64 = 9999999999999

// An order is one of an account's orders, as the fake serves it.
type order struct {
	json    string // its element of a list
	byID    string // what orders/{id} sends, if not json
	id      int64
	entered time.Time
	status  string
}

// readOrder reads body, a JSON object, as the fake serves an order: its
// orderId, an integer, and its enteredTime, in Schwab's form or RFC 3339's,
// are needed, since the lists and orders/{id} go by them; its status, if a
// string, is what status= matches.
func readOrder(body string) (*order, error) {
	if !isObject(body) {
		return nil, errors.New("an order is not a JSON object")
	}
	var probe struct {
		OrderID     json.RawMessage `json:"orderId"`
		EnteredTime json.RawMessage `json:"enteredTime"`
		Status      json.RawMessage `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return nil, fmt.Errorf("an order does not decode: %w", err)
	}
	o := &order{json: body}
	if !integer.Match(probe.OrderID) {
		return nil, errors.New("an order has no integer orderId")
	}
	id, err := strconv.ParseInt(string(probe.OrderID), 10, 64)
	if err != nil {
		return nil, errors.New("an order has no integer orderId")
	}
	o.id = id
	entered, _ := jsonText(probe.EnteredTime)
	var ok bool
	if o.entered, ok = parseTime(rowTime, entered); !ok {
		return nil, fmt.Errorf("order %d has no enteredTime that the fake reads", id)
	}
	o.status, _ = jsonText(probe.Status)
	return o, nil
}

// addOrders adds orders to the account with the given hash, after its
// others. It needs the lock.
func (s *Server) addOrders(hash string, bodies ...string) error {
	a := s.byHash(hash)
	if a == nil {
		return fmt.Errorf("no account has the hash %s", hash)
	}
	var added []*order
	for _, b := range bodies {
		o, err := readOrder(b)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(a.orders, func(x *order) bool { return x.id == o.id }) ||
			slices.ContainsFunc(added, func(x *order) bool { return x.id == o.id }) {
			return fmt.Errorf("order %d is already in the account", o.id)
		}
		added = append(added, o)
	}
	a.orders = append(a.orders, added...)
	return nil
}

// AddOrders adds orders to the account with the given hash, each a JSON
// object with an integer orderId, unique in the account, and an
// enteredTime. The fake serves each as it is given.
func (s *Server) AddOrders(hash string, orders ...string) {
	s.t.Helper()
	s.mu.Lock()
	err := s.addOrders(hash, orders...)
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: AddOrders: %v", err)
	}
}

// setOrderByID makes orders/{id} of the account send body for the order
// with the given id, which its lists still send as it was added. It needs
// the lock.
func (s *Server) setOrderByID(hash string, id int64, body string) error {
	a := s.byHash(hash)
	if a == nil {
		return fmt.Errorf("no account has the hash %s", hash)
	}
	if !json.Valid([]byte(body)) {
		return fmt.Errorf("order %d: the body is not JSON", id)
	}
	for _, o := range a.orders {
		if o.id == id {
			o.byID = body
			return nil
		}
	}
	return fmt.Errorf("the account has no order %d", id)
}

// orderWindow reads the bounds of a request for orders entered between
// fromEnteredTime and toEnteredTime, its status= and its maxResults=, or
// returns the 400 that refuses it, every one a guess: what Schwab answers
// to a query without either time, with either in another form, with a
// window over a year, or with a maxResults that is not a whole number
// from 1 up, is not recorded.
func (s *Server) orderWindow(q url.Values) (from, to time.Time, status string, most int, refused *answer) {
	refuse := func(message string) (time.Time, time.Time, string, int, *answer) {
		a := s.guess(message)
		return time.Time{}, time.Time{}, "", 0, &a
	}
	for _, name := range []string{"fromEnteredTime", "toEnteredTime"} {
		v, ok := q[name]
		if !ok || len(v) == 0 {
			return refuse("Schwab's answer to a request for orders without " + name + " is not recorded")
		}
		t, ok := parseTime(queryTime, v[0])
		if !ok {
			return refuse("Schwab's answer to this " + name + " is not recorded")
		}
		if name == "fromEnteredTime" {
			from = t
		} else {
			to = t
		}
	}
	switch {
	case to.Before(from):
		return refuse("Schwab's answer to a toEnteredTime before fromEnteredTime is not recorded")
	case to.After(from.AddDate(1, 0, 0)):
		return refuse("Schwab's answer to a window of orders over a year is not recorded")
	}
	most = MaxOrders
	if v, ok := q["maxResults"]; ok && len(v) > 0 {
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 1 || !integer.MatchString(v[0]) {
			return refuse("Schwab's answer to this maxResults is not recorded")
		}
		most = n
	}
	return from, to, q.Get("status"), most, nil
}

// newestOrders sorts os newest first by entry, then by id, largest first,
// and keeps the first most of those entered from from to to, both
// included, in status, if not "".
func newestOrders(os []*order, from, to time.Time, status string, most int) []*order {
	var in []*order
	for _, o := range os {
		if !o.entered.Before(from) && !o.entered.After(to) && (status == "" || o.status == status) {
			in = append(in, o)
		}
	}
	slices.SortStableFunc(in, func(a, b *order) int {
		return cmp.Or(b.entered.Compare(a.entered), cmp.Compare(b.id, a.id))
	})
	return in[:min(len(in), most)]
}

// orderList returns the JSON list of os.
func orderList(os []*order) []byte {
	es := make([][]byte, len(os))
	for i, o := range os {
		es[i] = []byte(o.json)
	}
	return list(es)
}

// orders answers GET /accounts/{hash}/orders, as the package doc says. It
// needs the lock.
func (s *Server) orders(hash string, q url.Values) answer {
	a := s.byHash(hash)
	if a == nil {
		return s.invalidAccount()
	}
	from, to, status, most, refused := s.orderWindow(q)
	if refused != nil {
		return *refused
	}
	return s.built(http.StatusOK, orderList(newestOrders(a.orders, from, to, status, most)))
}

// allOrders answers GET /orders, every account's orders, as the package
// doc says. It needs the lock.
func (s *Server) allOrders(q url.Values) answer {
	from, to, status, most, refused := s.orderWindow(q)
	if refused != nil {
		return *refused
	}
	var all []*order
	for _, a := range s.accounts {
		all = append(all, a.orders...)
	}
	return s.built(http.StatusOK, orderList(newestOrders(all, from, to, status, most)))
}

// order answers GET /accounts/{hash}/orders/{id}. It needs the lock.
func (s *Server) order(hash, id string) answer {
	a := s.byHash(hash)
	if a == nil {
		return s.invalidAccount()
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return s.guess("Schwab's answer to an order id that is not a number is not recorded")
	}
	for _, o := range a.orders {
		if o.id == n {
			if o.byID != "" {
				return s.built(http.StatusOK, []byte(o.byID))
			}
			return s.built(http.StatusOK, []byte(o.json))
		}
	}
	return s.built(http.StatusNotFound, []byte(`{"message":"Not Found (a guess: Schwab's answer is not recorded)"}`))
}
