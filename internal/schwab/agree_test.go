package schwab

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// fakeAuth returns an Auth for s, the shared fake, as a program makes one,
// whose time is c's.
func fakeAuth(t *testing.T, s *schwabtest.Server, c *clock) *Auth {
	t.Helper()
	a, err := NewAuth(Config{
		AppKey: schwabtest.AppKey, AppSecret: schwabtest.AppSecret, CallbackURL: schwabtest.CallbackURL,
		BaseURL: s.URL(), TokenFile: filepath.Join(t.TempDir(), "token.json"), HTTPClient: s.Client(), Now: c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fakeClient logs in to s at c's time and returns a client whose rate
// limit lets the test's requests go at once.
func fakeClient(t *testing.T, s *schwabtest.Server, c *clock) *Client {
	t.Helper()
	a := fakeAuth(t, s, c)
	if err := a.Login(t.Context(), schwabtest.Code); err != nil {
		t.Fatal(err)
	}
	return NewClient(a, ClientConfig{RateFile: filepath.Join(t.TempDir(), "rate"), RatePerMinute: 60_000, Burst: 100})
}

// The fake keeps its own copies of what the client knows of Schwab, so
// that a mistake in one is not mirrored in the other; they agree.
func TestFakeAgreesWithTheClient(t *testing.T) {
	if schwabtest.MaxTransactions != MaxTransactions {
		t.Errorf("the fake caps answers at %d transactions, the client at %d", schwabtest.MaxTransactions, MaxTransactions)
	}
	var types []string
	for _, typ := range AllTransactionTypes() {
		types = append(types, string(typ))
	}
	if !slices.Equal(schwabtest.TransactionTypes(), types) {
		t.Errorf("the fake's transaction types %v, the client's %v", schwabtest.TransactionTypes(), types)
	}
}

// The fake's refusals of a refresh token read as the client reads
// Schwab's: a lapsed login, which a refresh will not fix.
func TestFakeRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"Refusal2026", schwabtest.Refusal2026, "HTTP 400 from Schwab: unsupported_token_type (invalid_grant)"},
		{"Refusal2024", schwabtest.Refusal2024, "HTTP 400 from Schwab: unsupported_token_type (refresh_token_authentication_error)"},
		{"RefusalInvalidClient", schwabtest.RefusalInvalidClient, "HTTP 400 from Schwab: invalid_client"},
	} {
		final, err := refreshError(newError(http.StatusBadRequest, http.Header{}, []byte(tc.body)), 1)
		if !final || !errors.Is(err, ErrLoginExpired) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (final %v), want a lapsed login that says %q", tc.name, err, final, tc.want)
		}
	}
}

// The fake's canned answers read as the client reads Schwab's.
func TestFakeCannedAnswers(t *testing.T) {
	read := func(h http.HandlerFunc) *Error {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/trader/v1/accounts", nil))
		header := http.Header{} // as the client's transport canonicalizes it
		for k, vs := range rec.Header() {
			for _, v := range vs {
				header.Add(k, v)
			}
		}
		return newError(rec.Code, header, rec.Body.Bytes())
	}
	for _, tc := range []struct {
		name string
		h    http.HandlerFunc
		is   error
		want string
	}{
		{"TooMany", schwabtest.TooMany, ErrRateLimited, "HTTP 429 from Schwab (correlation ID corr-429)"},
		{"InvalidAccount", schwabtest.InvalidAccount, ErrInvalidAccount, "HTTP 400 from Schwab: Invalid account number (correlation ID corr-400)"},
		{"InvalidAccountList", schwabtest.InvalidAccountList, ErrInvalidAccount, "HTTP 400 from Schwab: Invalid account number (correlation ID corr-400)"},
		{"NotAuthorized", schwabtest.NotAuthorized, nil, "HTTP 401 from Schwab: Client not authorized (correlation ID corr-401)"},
		{"InvalidAPIKey", schwabtest.InvalidAPIKey, nil, "HTTP 401 from Schwab: Invalid ApiKey (oauth.v2.InvalidApiKey) (correlation ID corr-401)"},
		{"AccessDenied", schwabtest.AccessDenied, nil, "HTTP 403 from Schwab: an HTML page, Access Denied (reference #" + schwabtest.AkamaiReference + ")"},
		{"Whitelabel", schwabtest.Whitelabel, nil, "HTTP 500 from Schwab: an HTML page, Whitelabel Error Page"},
		{"TooBigBody", schwabtest.TooBigBody, ErrResponseTooBig, "HTTP 502 from Schwab: Body buffer overflow (protocol.http.TooBigBody) (correlation ID corr-502)"},
		{"Unavailable", schwabtest.Unavailable, nil, "HTTP 503 from Schwab: The Service is temporarily unavailable (messaging.adaptors.http.flow.ServiceUnavailable) (correlation ID corr-503)"},
		{"GatewayTimeout", schwabtest.GatewayTimeout, nil, "HTTP 504 from Schwab: Gateway Timeout (messaging.adaptors.http.flow.GatewayTimeout) (correlation ID corr-504)"},
	} {
		e := read(tc.h)
		if e.Error() != tc.want || tc.is != nil && !errors.Is(e, tc.is) {
			t.Errorf("%s: %v, want %q", tc.name, e, tc.want)
		}
	}
}

// A login through the fake's authorize page, as a browser would make it,
// gives the client a code it takes, the state sent coming back as it was.
func TestFakeAuthorizePage(t *testing.T) {
	s := schwabtest.New(t)
	a := fakeAuth(t, s, &clock{now: testNow})
	state := "a&b#c+d%e f"
	c := *s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, a.AuthorizeURL(state), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	code, err := CodeFromRedirect(resp.Header.Get("Location"), state)
	if err != nil || code != schwabtest.AuthCode(1) {
		t.Fatalf("CodeFromRedirect(%s) = %q, %v", resp.Header.Get("Location"), code, err)
	}
	if _, err := CodeFromRedirect(resp.Header.Get("Location"), "another"); err == nil {
		t.Error("the redirect's state matched another login's")
	}
	if err := a.Login(t.Context(), code); err != nil {
		t.Fatal(err)
	}
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != schwabtest.AccessToken(1) {
		t.Errorf("AccessToken() = %q, %v", tok, err)
	}
	if err := a.Login(t.Context(), code); err == nil {
		t.Error("the page's code worked twice")
	}
}

// The fake's accounts, as LoadFixtures and as it builds them, read through
// the client with no problems; the user preferences it builds carry every
// key that Schwab sends and no other.
func TestFakeAnswersDecode(t *testing.T) {
	s := schwabtest.New(t)
	s.LoadFixtures()
	cl := fakeClient(t, s, &clock{now: testNow})
	ctx := t.Context()
	numbers, err := cl.AccountNumbers(ctx)
	if err != nil || len(numbers) != 3 {
		t.Fatalf("AccountNumbers: %d, %v", len(numbers), err)
	}
	noProblems(t, numbers, func(a AccountNumber) []Problem { return a.Problems })
	for _, fields := range [][]AccountField{nil, {AccountFieldPositions}} {
		accounts, err := cl.Accounts(ctx, fields...)
		if err != nil || len(accounts) != 3 {
			t.Fatalf("Accounts(%v): %d, %v", fields, len(accounts), err)
		}
		noProblems(t, accounts, func(a Account) []Problem { return a.Problems })
		if held := len(accounts[0].SecuritiesAccount.Positions) > 0; held != (fields != nil) {
			t.Errorf("Accounts(%v): positions %v", fields, accounts[0].SecuritiesAccount.Positions)
		}
	}
	if a, err := cl.Account(ctx, numbers[1].HashValue, AccountFieldPositions); err != nil || len(a.Problems) > 0 ||
		a.SecuritiesAccount.AccountNumber != numbers[1].AccountNumber {
		t.Errorf("Account: %+v, %v", a.SecuritiesAccount, err)
	}
	start := testNow.AddDate(-3, 0, 0)
	txs, err := cl.Transactions(ctx, numbers[0].HashValue, TransactionQuery{Start: start, End: start.Add(MaxTransactionWindow)})
	if err != nil || len(txs) == 0 {
		t.Fatalf("Transactions: %d, %v", len(txs), err)
	}
	noProblems(t, txs, func(tx Transaction) []Problem { return tx.Problems })
	if tx, err := cl.Transaction(ctx, numbers[0].HashValue, 73508261946); err != nil || len(tx.Problems) > 0 || tx.ActivityID != 73508261946 {
		t.Errorf("Transaction: %+v, %v", tx, err)
	}
	if _, err := cl.Transaction(ctx, AccountHash(schwabtest.UnknownHash), 73508261946); !errors.Is(err, ErrInvalidAccount) {
		t.Errorf("a transaction of an unknown hash: %v, want ErrInvalidAccount", err)
	}

	s.SetUserPreference("")
	pref, err := cl.UserPreference(ctx)
	if err != nil || len(pref.Problems) > 0 || len(pref.Accounts) != 3 || pref.Accounts[0].NickName != "Account 1" {
		t.Fatalf("UserPreference: %+v, %v", pref, err)
	}
	c := &census{records: map[reflect.Type][]map[string]bool{}}
	c.walk(reflect.TypeFor[UserPreference](), decodeAny(t, pref.Raw))
	if len(c.unknown) > 0 {
		t.Errorf("keys that no field takes: %v", c.unknown)
	}
	for typ, records := range c.records {
		for k, f := range jsonFields(typ) {
			for i, r := range records {
				if !f.optional && !r[k] {
					t.Errorf("%s %d lacks %q", typ.Name(), i, k)
				}
			}
		}
	}
}

// After the fake ends the access tokens, the client gets a new one and
// tries again, as it does with Schwab, once its token is old enough that
// Schwab's 401 does not mean something else.
func TestFakeRefreshAfter401(t *testing.T) {
	s := schwabtest.New(t)
	c := &clock{now: testNow}
	cl := fakeClient(t, s, c)
	c.advance(2 * freshTokenAge)
	s.RevokeAccess()
	if body, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("Get: %q, %v", body, err)
	}
	rs := s.Requests()
	if _, refresh := s.Grants(); refresh != 1 || len(rs) != 1 || rs[0].Token != schwabtest.AccessToken(2) || s.APICalls() != 2 {
		t.Errorf("%d refreshes, %d API calls, requests %+v", refresh, s.APICalls(), rs)
	}
}
