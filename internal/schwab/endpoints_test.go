package schwab

import (
	"bytes"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// endpointClient returns a client logged in to a fake that answers its API
// requests with answers, in order.
func endpointClient(t *testing.T, answers ...http.HandlerFunc) (*Client, *fakeOAuth) {
	t.Helper()
	f := newFakeOAuth(t)
	f.Script(answers...)
	return newTestClient(t, f, &clock{now: testNow}, &fakeTime{now: testNow}), f
}

// ok answers 200 with body as JSON.
func ok(body string) http.HandlerFunc {
	return answer(http.StatusOK, body, "Content-Type", "application/json")
}

// requested returns the path and query of the fake's API requests.
func requested(t *testing.T, f *fakeOAuth) []*url.URL {
	t.Helper()
	var us []*url.URL
	for _, r := range f.Requests() {
		u, err := url.Parse(r.URL)
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
		t.Fatalf("a window without transactions gave %v, %v; want an empty list", got, err)
	}
	u := requested(t, f)
	if len(u) != 2 || u[0].Path != "/trader/v1/accounts/HASH/transactions" {
		t.Fatalf("requested %v", u)
	}
	q := u[0].Query()
	if q.Get("startDate") != "2026-01-02T08:04:05.678Z" || q.Get("endDate") != "2026-02-01T08:04:05.678Z" {
		t.Errorf("dates sent as %q and %q", q.Get("startDate"), q.Get("endDate"))
	}
	// The default is every type the spec lists, spelled as the spec does.
	if q.Get("types") != "TRADE,RECEIVE_AND_DELIVER,DIVIDEND_OR_INTEREST,ACH_RECEIPT,ACH_DISBURSEMENT,"+
		"CASH_RECEIPT,CASH_DISBURSEMENT,ELECTRONIC_FUND,WIRE_OUT,WIRE_IN,JOURNAL,MEMORANDUM,MARGIN_CALL,"+
		"MONEY_MARKET,SMA_ADJUSTMENT" || q.Has("symbol") {
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
		"no start":              {End: start},
		"no end":                {Start: start},
		"an end before":         {Start: start, End: start.Add(-time.Second)},
		"a window of no length": {Start: start, End: start},
		"a window too long":     {Start: start, End: start.Add(MaxTransactionWindow + time.Second)},
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
	if _, err := cl.Transaction(t.Context(), "", 1); err == nil {
		t.Error("a transaction of no account did not fail")
	}
	if u := requested(t, f); len(u) != 3 || u[0].Path != "/trader/v1/accounts/HASH/transactions/73508261946" {
		t.Errorf("requested %v", u)
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
		answer(http.StatusBadRequest, `[{"message":"Invalid account number"}]`),
		ok(`{"oops":true}`),
	)
	for range 2 {
		_, err := cl.Transactions(t.Context(), "29470135", TransactionQuery{Start: testNow.Add(-time.Hour), End: testNow})
		if !errors.Is(err, ErrInvalidAccount) {
			t.Errorf("Schwab's refusal of the account: %v, want ErrInvalidAccount", err)
		}
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

// The tests below read the fake's world, which LoadFixtures fills with the
// fixtures: three accounts, their transactions and the user's preferences,
// served as the files hold them. Each checks one Client method's records
// against the fixtures, and the requests that it sent.

// fixturesClient returns a client logged in to a fake that holds the
// fixtures' world.
func fixturesClient(t *testing.T) (*Client, *fakeOAuth) {
	t.Helper()
	f := newFakeOAuth(t)
	f.LoadFixtures()
	return newTestClient(t, f, &clock{now: testNow}, &fakeTime{now: testNow}), f
}

// fixtureHashes returns the hash of each of the fixtures' accounts, by
// account number.
func fixtureHashes() map[string]AccountHash {
	hashes := map[string]AccountHash{}
	for _, a := range schwabtest.FixtureAccounts {
		hashes[a.Number] = AccountHash(a.Hash)
	}
	return hashes
}

// withoutPositions returns a without its positions and its bytes, to compare
// it with the same account as an answer without positions holds it.
func withoutPositions(a Account) Account {
	a.SecuritiesAccount.Positions, a.Raw = nil, nil
	return a
}

// The account numbers are the fixture's, each record holding its element's
// bytes.
func TestAccountNumbersFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	got, err := cl.AccountNumbers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := elements(t, readTestdata(t, "account_numbers.json"))
	if len(got) != len(want) || len(got) != len(schwabtest.FixtureAccounts) {
		t.Fatalf("%d account numbers, want %d", len(got), len(want))
	}
	for i, a := range got {
		fa := schwabtest.FixtureAccounts[i]
		if !bytes.Equal(a.Raw, want[i]) || a.AccountNumber != fa.Number || string(a.HashValue) != fa.Hash || len(a.Problems) > 0 {
			t.Errorf("account %d: %s %s, problems %q, from %s; want %s", i, a.AccountNumber, a.HashValue, a.Problems, a.Raw, want[i])
		}
	}
	if u := requested(t, f); len(u) != 1 || u[0].Path != "/trader/v1/accounts/accountNumbers" || u[0].RawQuery != "" {
		t.Errorf("requested %v", u)
	}
}

// The accounts asked for with their positions are the fixture's, byte for
// byte; asked for without, they are the same accounts with no positions.
func TestAccountsFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	held, err := cl.Accounts(t.Context(), AccountFieldPositions)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := cl.Accounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := elements(t, readTestdata(t, "accounts.json"))
	if len(held) != len(want) || len(bare) != len(want) {
		t.Fatalf("%d accounts with positions and %d without, want %d", len(held), len(bare), len(want))
	}
	for i := range want {
		if !bytes.Equal(held[i].Raw, want[i]) || len(held[i].Problems) > 0 {
			t.Errorf("account %d with positions: problems %q, from %s; want %s", i, held[i].Problems, held[i].Raw, want[i])
		}
		if len(bare[i].SecuritiesAccount.Positions) > 0 || len(bare[i].Problems) > 0 ||
			!reflect.DeepEqual(withoutPositions(bare[i]), withoutPositions(held[i])) {
			t.Errorf("account %d without positions: %+v, problems %q; want %+v without its positions",
				i, bare[i].SecuritiesAccount, bare[i].Problems, held[i].SecuritiesAccount)
		}
	}
	if len(held[0].SecuritiesAccount.Positions) == 0 {
		t.Error("the first account holds no positions, so the test checks none")
	}
	u := requested(t, f)
	if len(u) != 2 || u[0].Path != "/trader/v1/accounts" || u[0].RawQuery != "fields=positions" ||
		u[1].Path != "/trader/v1/accounts" || u[1].RawQuery != "" {
		t.Errorf("requested %v", u)
	}
}

// Each account asked for by its hash is the fixture's, as Accounts gets it,
// and the request names the hash and the fields asked for.
func TestAccountFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	fixtures, err := decodeList[Account](readTestdata(t, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{}
	for _, a := range fixtures {
		want[a.SecuritiesAccount.AccountNumber] = a.Raw
	}
	for _, fa := range schwabtest.FixtureAccounts {
		held, err := cl.Account(t.Context(), AccountHash(fa.Hash), AccountFieldPositions)
		if err != nil {
			t.Fatal(err)
		}
		bare, err := cl.Account(t.Context(), AccountHash(fa.Hash))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(held.Raw, want[fa.Number]) || len(held.Problems) > 0 {
			t.Errorf("account %s with positions: problems %q, from %s; want %s", fa.Number, held.Problems, held.Raw, want[fa.Number])
		}
		if len(bare.SecuritiesAccount.Positions) > 0 || len(bare.Problems) > 0 || !reflect.DeepEqual(withoutPositions(bare), withoutPositions(held)) {
			t.Errorf("account %s without positions: %+v, problems %q; want %+v without its positions",
				fa.Number, bare.SecuritiesAccount, bare.Problems, held.SecuritiesAccount)
		}
	}
	u := requested(t, f)
	if len(u) != 2*len(schwabtest.FixtureAccounts) {
		t.Fatalf("requested %v", u)
	}
	for i, fa := range schwabtest.FixtureAccounts {
		path := "/trader/v1/accounts/" + fa.Hash
		if held, bare := u[2*i], u[2*i+1]; held.Path != path || held.RawQuery != "fields=positions" || bare.Path != path || bare.RawQuery != "" {
			t.Errorf("account %s: requested %v and %v", fa.Number, held, bare)
		}
	}
}

// Windows of 365 days less a millisecond, back to back, which with times
// sent in milliseconds neither overlap nor leave a gap, walk back through
// each account's history and get each of its fixture transactions exactly
// once, in the window that holds its time.
func TestTransactionsFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	rows, err := decodeList[Transaction](readTestdata(t, "transactions.json"))
	if err != nil {
		t.Fatal(err)
	}
	oldest := testNow
	for _, row := range rows {
		if !row.Time.Before(testNow) {
			t.Fatalf("transaction %d is at %v, not before %v, where the walk begins", row.ActivityID, row.Time, testNow)
		}
		if row.Time.Before(oldest) {
			oldest = row.Time
		}
	}
	const window = MaxTransactionWindow - time.Millisecond
	types := make([]string, len(AllTransactionTypes()))
	for i, typ := range AllTransactionTypes() {
		types[i] = string(typ)
	}
	hashes := fixtureHashes()
	sent := 0
	for _, fa := range schwabtest.FixtureAccounts {
		want := map[string]int{}
		for _, row := range rows {
			if row.AccountNumber == fa.Number {
				want[string(row.Raw)]++
			}
		}
		got := map[string]int{}
		// In a zone of its own, so that a client that sends local times
		// shows.
		for end := testNow.In(time.FixedZone("EST", -5*3600)); ; {
			start := end.Add(-window)
			txs, err := cl.Transactions(t.Context(), hashes[fa.Number], TransactionQuery{Start: start, End: end})
			if err != nil {
				t.Fatal(err)
			}
			for _, tx := range txs {
				got[string(tx.Raw)]++
				if tx.Time.Before(start) || tx.Time.After(end) || len(tx.Problems) > 0 {
					t.Errorf("account %s, %v to %v: transaction %d at %v, problems %q", fa.Number, start, end, tx.ActivityID, tx.Time, tx.Problems)
				}
			}
			u := requested(t, f)
			sent++
			if len(u) != sent {
				t.Fatalf("%d requests, want %d", len(u), sent)
			}
			q := u[sent-1].Query()
			if u[sent-1].Path != "/trader/v1/accounts/"+string(hashes[fa.Number])+"/transactions" ||
				q.Get("startDate") != start.UTC().Format("2006-01-02T15:04:05.000Z") ||
				q.Get("endDate") != end.UTC().Format("2006-01-02T15:04:05.000Z") ||
				q.Get("types") != strings.Join(types, ",") || len(q) != 3 {
				t.Errorf("account %s, %v to %v: requested %v", fa.Number, start, end, u[sent-1])
			}
			if start.Before(oldest) {
				break
			}
			end = start.Add(-time.Millisecond)
		}
		if !maps.Equal(got, want) {
			t.Errorf("account %s: the walk got %d transactions, %v; want each of the %d in the fixture once", fa.Number, len(got), got, len(want))
		}
	}
	if sent < 2*len(schwabtest.FixtureAccounts) {
		t.Errorf("%d windows for %d accounts: the walk never went past the first", sent, len(schwabtest.FixtureAccounts))
	}
}

// Each transaction asked for by its id is the fixture's: transaction.json's
// for the one it holds, and the list's for each other.
func TestTransactionFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	rows, err := decodeList[Transaction](readTestdata(t, "transactions.json"))
	if err != nil {
		t.Fatal(err)
	}
	one := bytes.TrimSpace(readTestdata(t, "transaction.json"))
	byID, err := decodeOne[Transaction](one)
	if err != nil {
		t.Fatal(err)
	}
	hashes := fixtureHashes()
	found := false
	for _, row := range rows {
		want := row.Raw
		if row.ActivityID == byID.ActivityID {
			want, found = one, true
		}
		got, err := cl.Transaction(t.Context(), hashes[row.AccountNumber], row.ActivityID)
		if err != nil || !bytes.Equal(got.Raw, want) || len(got.Problems) > 0 {
			t.Errorf("transaction %d: %v, problems %q, from %s; want %s", row.ActivityID, err, got.Problems, got.Raw, want)
		}
	}
	if !found {
		t.Errorf("no transaction of the list has transaction.json's id, %d", byID.ActivityID)
	}
	u := requested(t, f)
	if len(u) != len(rows) {
		t.Fatalf("%d requests, want %d", len(u), len(rows))
	}
	for i, row := range rows {
		path := "/trader/v1/accounts/" + string(hashes[row.AccountNumber]) + "/transactions/" + strconv.FormatInt(row.ActivityID, 10)
		if u[i].Path != path || u[i].RawQuery != "" {
			t.Errorf("transaction %d: requested %v, want %s", row.ActivityID, u[i], path)
		}
	}
}

// The user's preferences are the fixture's, with each of the fixtures'
// accounts under its nickname and masked number.
func TestUserPreferenceFromTheFixtures(t *testing.T) {
	cl, f := fixturesClient(t)
	got, err := cl.UserPreference(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.TrimSpace(readTestdata(t, "user_preference.json")); !bytes.Equal(got.Raw, want) || len(got.Problems) > 0 {
		t.Errorf("problems %q, from %s; want %s", got.Problems, got.Raw, want)
	}
	if len(got.Accounts) != len(schwabtest.FixtureAccounts) {
		t.Fatalf("%d accounts, want %d", len(got.Accounts), len(schwabtest.FixtureAccounts))
	}
	for i, fa := range schwabtest.FixtureAccounts {
		if a := got.Accounts[i]; a.AccountNumber != fa.Number || a.NickName != fa.Nickname || a.DisplayAcctID != fa.DisplayID {
			t.Errorf("account %d: %+v, want %+v", i, a, fa)
		}
	}
	if u := requested(t, f); len(u) != 1 || u[0].Path != "/trader/v1/userPreference" || u[0].RawQuery != "" {
		t.Errorf("requested %v", u)
	}
}
