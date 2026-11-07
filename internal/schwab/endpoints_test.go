package schwab

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// endpointClient returns a client logged in to a fake that answers its API
// requests with answers, in order.
func endpointClient(t *testing.T, answers ...func(http.ResponseWriter, *http.Request)) (*Client, *fakeOAuth) {
	t.Helper()
	f := newFakeOAuth(t)
	f.script = answers
	return newTestClient(t, f, &clock{now: testNow}, &fakeTime{now: testNow}), f
}

// ok answers 200 with body as JSON.
func ok(body string) func(http.ResponseWriter, *http.Request) {
	return answer(http.StatusOK, body, "Content-Type", "application/json")
}

// requested returns the path and query of the fake's API requests.
func requested(t *testing.T, f *fakeOAuth) []*url.URL {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var us []*url.URL
	for _, s := range f.urls {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		us = append(us, u)
	}
	return us
}

func TestAccountNumbersRequest(t *testing.T) {
	cl, f := endpointClient(t, ok(string(readTestdata(t, "account_numbers.json"))))
	got, err := cl.AccountNumbers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].HashValue == "" {
		t.Fatalf("got %+v", got)
	}
	if u := requested(t, f); len(u) != 1 || u[0].Path != "/trader/v1/accounts/accountNumbers" || u[0].RawQuery != "" {
		t.Errorf("requested %v", u)
	}
}

func TestAccountsRequests(t *testing.T) {
	body := string(readTestdata(t, "accounts.json"))
	cl, f := endpointClient(t, ok(body), ok(body), ok(`{"securitiesAccount":{"type":"CASH","accountNumber":"29470135"}}`))
	if got, err := cl.Accounts(t.Context()); err != nil || len(got) != 3 {
		t.Fatalf("Accounts() = %d accounts, %v", len(got), err)
	}
	if got, err := cl.Accounts(t.Context(), AccountFieldPositions); err != nil || len(got[0].SecuritiesAccount.Positions) != 4 {
		t.Fatalf("Accounts(Positions) = %v", err)
	}
	got, err := cl.Account(t.Context(), "AB/C D", AccountFieldPositions)
	if err != nil || got.SecuritiesAccount.AccountNumber != "29470135" {
		t.Fatalf("Account() = %+v, %v", got, err)
	}
	u := requested(t, f)
	if len(u) != 3 {
		t.Fatalf("%d requests, want 3", len(u))
	}
	if u[0].Path != "/trader/v1/accounts" || u[0].RawQuery != "" {
		t.Errorf("Accounts() requested %v", u[0])
	}
	if u[1].Path != "/trader/v1/accounts" || u[1].Query().Get("fields") != "positions" {
		t.Errorf("Accounts(Positions) requested %v", u[1])
	}
	if u[2].EscapedPath() != "/trader/v1/accounts/AB%2FC%20D" || u[2].Query().Get("fields") != "positions" {
		t.Errorf("Account() requested %v", u[2])
	}
	if _, err := cl.Account(t.Context(), ""); err == nil {
		t.Error("Account() without a hash did not fail")
	}
}

func TestTransactionsRequest(t *testing.T) {
	cl, f := endpointClient(t, ok(string(readTestdata(t, "transactions.json"))), ok(`[]`))
	start := time.Date(2026, 1, 2, 3, 4, 5, 678e6, time.FixedZone("EST", -5*3600))
	end := start.Add(30 * 24 * time.Hour)
	got, err := cl.Transactions(t.Context(), "HASH", TransactionQuery{Start: start, End: end})
	if err != nil || len(got) != 14 {
		t.Fatalf("Transactions() = %d, %v", len(got), err)
	}
	got, err = cl.Transactions(t.Context(), "HASH", TransactionQuery{
		Start: start, End: end, Types: []TransactionType{TransactionTrade, TransactionJournal}, Symbol: "BND",
	})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("an empty window gave %v, %v; want an empty list", got, err)
	}
	u := requested(t, f)
	if len(u) != 2 || u[0].Path != "/trader/v1/accounts/HASH/transactions" {
		t.Fatalf("requested %v", u)
	}
	q := u[0].Query()
	if q.Get("startDate") != "2026-01-02T08:04:05.678Z" || q.Get("endDate") != "2026-02-01T08:04:05.678Z" {
		t.Errorf("dates sent as %q and %q", q.Get("startDate"), q.Get("endDate"))
	}
	all := AllTransactionTypes()
	names := make([]string, len(all))
	for i, ty := range all {
		names[i] = string(ty)
	}
	if q.Get("types") != strings.Join(names, ",") || len(all) != 15 || q.Has("symbol") {
		t.Errorf("default query %v", q)
	}
	if q := u[1].Query(); q.Get("types") != "TRADE,JOURNAL" || q.Get("symbol") != "BND" {
		t.Errorf("query %v", q)
	}
}

// A query Schwab would refuse fails before any request.
func TestTransactionsWindowRules(t *testing.T) {
	cl, f := endpointClient(t)
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for name, q := range map[string]TransactionQuery{
		"no start":          {End: start},
		"no end":            {Start: start},
		"an end before":     {Start: start, End: start.Add(-time.Second)},
		"an empty window":   {Start: start, End: start},
		"a window too long": {Start: start, End: start.Add(MaxTransactionWindow + time.Second)},
	} {
		if _, err := cl.Transactions(t.Context(), "HASH", q); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := cl.Transactions(t.Context(), "", TransactionQuery{Start: start, End: start.Add(time.Hour)}); err == nil {
		t.Error("no hash: no error")
	}
	if u := requested(t, f); len(u) != 0 {
		t.Errorf("sent %v", u)
	}
	// The longest window allowed is sent.
	cl, f = endpointClient(t, ok(`[]`))
	if _, err := cl.Transactions(t.Context(), "HASH", TransactionQuery{Start: start, End: start.Add(MaxTransactionWindow)}); err != nil {
		t.Errorf("a window of exactly %v: %v", MaxTransactionWindow, err)
	}
	if u := requested(t, f); len(u) != 1 {
		t.Errorf("sent %d requests, want 1", len(u))
	}
}

func TestTransactionByID(t *testing.T) {
	one := string(readTestdata(t, "transaction.json"))
	cl, f := endpointClient(t, ok(one), ok("["+one+"]"), ok(`[]`))
	for range 2 {
		got, err := cl.Transaction(t.Context(), "HASH", 73508261946)
		if err != nil || got.ActivityID != 73508261946 || len(got.Problems) > 0 {
			t.Fatalf("Transaction() = %+v, %v", got, err)
		}
	}
	if _, err := cl.Transaction(t.Context(), "HASH", 1); err == nil {
		t.Error("an empty list did not fail")
	}
	if u := requested(t, f); u[0].Path != "/trader/v1/accounts/HASH/transactions/73508261946" {
		t.Errorf("requested %v", u[0])
	}
}

func TestUserPreferenceRequest(t *testing.T) {
	one := string(readTestdata(t, "user_preference.json"))
	cl, f := endpointClient(t, ok(one), ok("["+one+"]"))
	for range 2 {
		got, err := cl.UserPreference(t.Context())
		if err != nil || len(got.Accounts) != 3 {
			t.Fatalf("UserPreference() = %+v, %v", got, err)
		}
	}
	if u := requested(t, f); u[0].Path != "/trader/v1/userPreference" {
		t.Errorf("requested %v", u[0])
	}
}

// Errors from Schwab come back as they are, and a body that is not the
// expected kind of JSON is an error.
func TestEndpointErrors(t *testing.T) {
	cl, _ := endpointClient(t,
		answer(http.StatusBadRequest, `{"message":"Invalid account number"}`),
		ok(`{"oops":true}`),
	)
	_, err := cl.Transactions(t.Context(), "29470135", TransactionQuery{Start: testNow.Add(-time.Hour), End: testNow})
	if !errors.Is(err, ErrInvalidAccount) {
		t.Errorf("Schwab's refusal of the account: %v, want ErrInvalidAccount", err)
	}
	if _, err := cl.AccountNumbers(t.Context()); err == nil || !strings.Contains(err.Error(), "not a list") {
		t.Errorf("an object for a list: %v", err)
	}
}

func TestErrInvalidAccount(t *testing.T) {
	for _, tt := range []struct {
		e    *Error
		want bool
	}{
		{&Error{Status: http.StatusBadRequest, Message: "Invalid account number"}, true},
		{&Error{Status: http.StatusBadRequest, Message: " invalid account number "}, true},
		{&Error{Status: http.StatusBadRequest, Message: "Invalid account"}, false},
		{&Error{Status: http.StatusNotFound, Message: "Invalid account number"}, false},
	} {
		if got := errors.Is(tt.e, ErrInvalidAccount); got != tt.want {
			t.Errorf("errors.Is(%+v, ErrInvalidAccount) = %v, want %v", tt.e, got, tt.want)
		}
	}
}

// An answer that is not the JSON its endpoint sends is an error that keeps
// the answer's correlation ID, for a report to Schwab, and not its body.
func TestUndecodableAnswerKeepsTheCorrelationID(t *testing.T) {
	cl, _ := endpointClient(t, answer(http.StatusOK, `{"accountNumber":"29470135"}`, "Schwab-Client-CorrelId", "corr-7"))
	_, err := cl.AccountNumbers(t.Context())
	if err == nil {
		t.Fatal("no error")
	}
	if got, want := err.Error(), "decoding the account numbers: the answer is an object, not a list (correlation ID corr-7)"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}
