package schwab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The app and the login that the fake takes.
const (
	testKey    = schwabtest.AppKey
	testSecret = schwabtest.AppSecret
	testCode   = schwabtest.Code
)

// fakeOAuth is the fake Schwab of package schwabtest: its token endpoint,
// and the API behind it, which answers only a working access token. As
// Schwab does, a login retires the refresh tokens of earlier ones, while a
// refresh keeps the refresh token. It can also rotate refresh tokens and
// retire older access tokens, which Schwab is not known to do, to check that
// the coordination would cope.
type fakeOAuth = schwabtest.Server

// newFakeOAuth starts a fake over HTTP, which the end of the test closes.
func newFakeOAuth(t *testing.T) *fakeOAuth { return schwabtest.New(t) }

// newFakeOAuthH2 is newFakeOAuth over HTTPS with HTTP/2, which a client
// gets from f.Client().
func newFakeOAuthH2(t *testing.T) *fakeOAuth { return schwabtest.NewTLS(t) }

// answer returns a script entry that answers with status, headers in pairs,
// and body.
var answer = schwabtest.Answer

// tooMany is a script entry that answers as Schwab does when it rate
// limits: 429, a body of white space, and no Retry-After.
var tooMany = schwabtest.TooMany

// cut closes the connection without an answer, as a network failure would,
// even after part of an answer is written.
func cut(w http.ResponseWriter) { schwabtest.Cut(w, nil) }

// lastToken returns the access token of the last API request that carried a
// working one, or "" before the first.
func lastToken(f *fakeOAuth) string {
	rs := f.Requests()
	if len(rs) == 0 {
		return ""
	}
	return rs[len(rs)-1].Token
}

// testdataKinds are the records that the account endpoints answer with, in
// the order of their routes.
var testdataKinds = []struct {
	route schwabtest.Route
	typ   reflect.Type // the record's type
	list  bool         // a list of records, rather than one
}{
	{schwabtest.AccountNumbersRoute, reflect.TypeFor[AccountNumber](), true},
	{schwabtest.AccountsRoute, reflect.TypeFor[Account](), true},
	{schwabtest.AccountRoute, reflect.TypeFor[Account](), false},
	{schwabtest.TransactionsRoute, reflect.TypeFor[Transaction](), true},
	{schwabtest.TransactionRoute, reflect.TypeFor[Transaction](), false},
	{schwabtest.UserPreferenceRoute, reflect.TypeFor[UserPreference](), false},
}

// A testdataAnswer is a fixture that answers an account endpoint with 200,
// and so holds that endpoint's records.
type testdataAnswer struct {
	file    string            // as readTestdata takes it
	typ     reflect.Type      // the records' type
	records []json.RawMessage // its records, as they are in the file
}

// testdataAnswers returns every fixture that the fake's index says answers
// an account endpoint with 200, a recording as well as the hand-made ones:
// by route, in testdataKinds' order, and within a route in the index's.
func testdataAnswers(t *testing.T) []testdataAnswer {
	t.Helper()
	index := schwabtest.Index()
	var answers []testdataAnswer
	for _, kind := range testdataKinds {
		for _, e := range index {
			if route, ok := e.Route(); !ok || route != kind.route || e.Response.Status != http.StatusOK {
				continue
			}
			b := readTestdata(t, e.File)
			records := []json.RawMessage{b}
			if kind.list {
				records = elements(t, b)
			}
			answers = append(answers, testdataAnswer{file: e.File, typ: kind.typ, records: records})
		}
	}
	return answers
}

// The hand-made fixtures that answer the account endpoints are read as
// their endpoints' records, lists split, in the Routes' order, which keeps
// a list's records ahead of the one-record answers of its kind; an answer
// that is not a 200, such as an error body on an account path, is not read.
func TestTestdataAnswers(t *testing.T) {
	var got []string
	for _, a := range testdataAnswers(t) {
		if !strings.HasPrefix(a.file, "recorded/") {
			got = append(got, fmt.Sprintf("%s: %d %s", a.file, len(a.records), a.typ.Name()))
		}
	}
	want := []string{
		"account_numbers.json: 3 AccountNumber",
		"accounts.json: 3 Account",
		"transactions.json: 14 Transaction",
		"transaction.json: 1 Transaction",
		"user_preference.json: 1 UserPreference",
	}
	if !slices.Equal(got, want) {
		t.Errorf("read %q, want %q", got, want)
	}
}
