package mcpserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// fixtureAccounts are the fixtures' accounts as accounts.json holds them,
// by number, read once.
var fixtureAccounts = sync.OnceValues(func() (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(filepath.Join("..", "schwab", "schwabtest", "testdata", "accounts.json"))
	if err != nil {
		return nil, err
	}
	var as []json.RawMessage
	if err := json.Unmarshal(b, &as); err != nil {
		return nil, err
	}
	out := map[string]json.RawMessage{}
	for _, a := range as {
		var number struct {
			SecuritiesAccount struct {
				AccountNumber string `json:"accountNumber"`
			} `json:"securitiesAccount"`
		}
		if err := json.Unmarshal(a, &number); err != nil {
			return nil, err
		}
		out[number.SecuritiesAccount.AccountNumber] = a
	}
	return out, nil
})

// changeAccount gives the fake the fixtures' account numbered number,
// changed by change, which takes it decoded: its positions, its balances
// and its other keys, as maps.
func changeAccount(f *schwabtest.Server, number string, change func(a map[string]any)) {
	as, err := fixtureAccounts()
	if err != nil {
		panic(err)
	}
	var a map[string]any
	if err := json.Unmarshal(as[number], &a); err != nil {
		panic(err)
	}
	change(a)
	b, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	f.SetDetails(schwabtest.HashFor(number), string(b))
}

// balancesOf returns the current balances of a, an account decoded by
// changeAccount.
func balancesOf(a map[string]any) map[string]any {
	return a["securitiesAccount"].(map[string]any)["currentBalances"].(map[string]any)
}

// withOddFigures sets the fixtures' accounts up with the figures that
// schwabtx's tables show as -, or find elsewhere: ...135's cash is not its
// value less its holdings, since 500 of what it holds is in savings;
// ...398, a margin account, keeps its cash in marginBalance beside a
// cashBalance of 0; and ...783, as the fixtures have it, sends no value
// and no cash.
func withOddFigures() envOption {
	return withFake(func(f *schwabtest.Server) {
		changeAccount(f, individual, func(a map[string]any) {
			cur := balancesOf(a)
			cur["cashBalance"], cur["savings"] = 1032.17, 500
		})
		changeAccount(f, options, func(a map[string]any) {
			cur := balancesOf(a)
			cur["cashBalance"], cur["marginBalance"], cur["shortBalance"] = 0, 5210.44, 0
		})
	})
}

// withOneBareAccount leaves the fake with one account, ...783, which sends
// no figure, and the server with no store.
func withOneBareAccount() []envOption {
	return []envOption{withoutFixtures(), withoutStore(), withFake(func(f *schwabtest.Server) {
		f.AddAccount(schwabtest.Account{Number: empty, Details: bareAccount(empty)})
	})}
}

// bareAccount returns the margin account numbered number as GET /accounts
// sends one, with the keys that Schwab sends on every account and no figure
// in its balances.
func bareAccount(number string) string {
	return `{"securitiesAccount":{"type":"MARGIN","accountNumber":"` + number + `","roundTrips":0,"isDayTrader":false,` +
		`"isClosingOnlyRestricted":false,"pfcbFlag":false,"initialBalances":{},"currentBalances":{},"projectedBalances":{}}}`
}

// accountsOf returns the accounts of r, list_accounts' or get_balances'
// answer, by masked number.
func accountsOf(t *testing.T, r result) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	as, _ := r.value["accounts"].([]any)
	for _, a := range as {
		m, _ := a.(map[string]any)
		out[fmt.Sprint(m["account"])] = m
	}
	return out
}

// apiRoutes returns the routes of the requests that reached the fake's API
// since it had served before of them.
func apiRoutes(f *schwabtest.Server, before int) []string {
	var routes []string
	for _, r := range f.Requests()[before:] {
		if r.Route != "" {
			routes = append(routes, string(r.Route))
		}
	}
	return routes
}

// positionsOf returns the positions of r, get_positions' answer.
func positionsOf(r result) []map[string]any {
	var out []map[string]any
	ps, _ := r.value["positions"].([]any)
	for _, p := range ps {
		m, _ := p.(map[string]any)
		out = append(out, m)
	}
	return out
}

// symbolsOf returns the masked account and the symbol of each position of
// r, get_positions' answer.
func symbolsOf(r result) []string {
	var out []string
	for _, p := range positionsOf(r) {
		out = append(out, fmt.Sprint(p["account"], " ", p["symbol"]))
	}
	return out
}

// fixturePositions are the fixtures' positions as get_positions lists them:
// by account, then by symbol, as schwabtx's table does.
var fixturePositions = []string{
	"...135 00507V109", "...135 AAPL", "...135 SWVXX", "...135 VTI", "...398 META", "...398 XYZ   260619P00045000",
}

// heldBy returns the positions of a, an account decoded by changeAccount.
func heldBy(a map[string]any) []any {
	ps, _ := a["securitiesAccount"].(map[string]any)["positions"].([]any)
	return ps
}

// withUnreadablePosition sets ...135 up with positions that cannot all be
// read: VTI sends its market value as a string, and AAPL lacks the open
// profit or loss that Schwab sends on every long position.
func withUnreadablePosition() envOption {
	return withFake(func(f *schwabtest.Server) {
		changeAccount(f, individual, func(a map[string]any) {
			for _, p := range heldBy(a) {
				p := p.(map[string]any)
				switch p["instrument"].(map[string]any)["symbol"] {
				case "VTI":
					p["marketValue"] = "3748.92"
				case "AAPL":
					delete(p, "longOpenProfitLoss")
				}
			}
		})
	})
}

// accountCases are the account tools' cases of TestToolsEmptyAndFull:
// empty is one account that sends no figure, beside no store, which is no
// failure; full is the fixtures' three accounts, joined with the shared
// store's, one with a cash that is not the value less the holdings, one a
// margin account with its cash in marginBalance, and one that sends no
// value; list_accounts' store answers from the store without a login;
// get_balances' detail is one account's every figure. get_positions' empty
// is an account that holds nothing; full is the fixtures' positions, a
// short put among them, beside a position whose value cannot be read and
// one that lacks its open profit or loss; symbol keeps a symbol's options,
// matched by their underlying in any case; pages is the fixtures' two at a
// time, followed to the end; and detail is one account's with detail full,
// in a world where every account sends its value, so that each position
// has both its weights.
var accountCases = []toolCase{
	{tool: "list_accounts", name: "empty", args: `{}`, opts: withOneBareAccount(), check: func(t *testing.T, _ *env, r result) {
		a := accountsOf(t, r)["...783"]
		if a["value"] != nil || a["cash"] != nil || r.value["total_value"] != nil || r.value["total_cash"] != nil || a["store_ref"] != nil {
			t.Errorf("an account that sends no figure: %s", r.text)
		}
		if !slices.Contains(r.warnings(), "figures_missing") || !strings.Contains(r.text, "Schwab sent no current value or cash for ...783 (Account 1).") {
			t.Errorf("no figures_missing: %s", r.text)
		}
		if slices.Contains(r.warnings(), "store_unavailable") {
			t.Errorf("no store is no failure: %s", r.text)
		}
	}},
	{tool: "list_accounts", name: "full", args: `{}`, opts: []envOption{withOddFigures()}, check: func(t *testing.T, e *env, r result) {
		as := accountsOf(t, r)
		if len(as) != 3 || r.value["source"] != "schwab" {
			t.Fatalf("the fixtures' accounts: %s", r.text)
		}
		for masked, ref := range map[string]string{"...135": "A1", "...398": "A2", "...783": "A3"} {
			if as[masked]["store_ref"] != ref {
				t.Errorf("%s's store_ref is %v, want %s", masked, as[masked]["store_ref"], ref)
			}
		}
		if as["...135"]["value"] != 6997.68 || as["...135"]["cash"] != nil || as["...398"]["cash"] != 5210.44 || as["...135"]["primary"] != true {
			t.Errorf("a cash that differs, and a margin account's cash in marginBalance: %s", r.text)
		}
		if r.value["total_value"] != nil || !slices.Contains(r.warnings(), "extended_hours") || !slices.Contains(r.warnings(), "cash_differs") {
			t.Errorf("a total that needs ...783's value, at 16:40 on a Friday: %s", r.text)
		}
		want := []string{string(schwabtest.AccountNumbersRoute), string(schwabtest.UserPreferenceRoute), string(schwabtest.AccountsRoute)}
		if routes := apiRoutes(e.fake, 0); !slices.Equal(routes, want) {
			t.Errorf("list_accounts sent %v, want %v", routes, want)
		}
	}},
	{tool: "list_accounts", name: "store", args: `{}`, opts: []envOption{withoutLogin()}, check: func(t *testing.T, e *env, r result) {
		as := accountsOf(t, r)
		if r.value["source"] != "store" || len(as) != 3 || as["...135"]["account_hash"] != nil || as["...135"]["value"] != nil {
			t.Errorf("the store's accounts, without values: %s", r.text)
		}
		if !slices.Contains(r.warnings(), "api_unavailable") || !strings.Contains(r.text, "[not_logged_in] There is no Schwab login") {
			t.Errorf("no api_unavailable with the login's error: %s", r.text)
		}
		if n := e.fake.APICalls(); n != 0 {
			t.Errorf("%d requests reached the API without a login", n)
		}
	}},
	{tool: "get_balances", name: "empty", args: `{}`, opts: withOneBareAccount(), check: func(t *testing.T, _ *env, r result) {
		a := accountsOf(t, r)["...783"]
		if a["value"] != nil || a["cash"] != nil || a["buying_power"] != nil || a["buying_power_is"] != "margin_buying_power" {
			t.Errorf("an account that sends no figure: %s", r.text)
		}
		if !strings.Contains(r.text, "Schwab sent no current value, cash or buying power for ...783 (Account 1).") {
			t.Errorf("no figures_missing that names every figure: %s", r.text)
		}
	}},
	{tool: "get_balances", name: "full", args: `{}`, opts: []envOption{withOddFigures()}, check: func(t *testing.T, _ *env, r result) {
		as := accountsOf(t, r)
		for masked, want := range map[string][2]any{
			"...135": {1532.17, "cash_available_for_trading"}, "...398": {8258.84, "margin_buying_power"}, "...783": {8710.0, "margin_buying_power"},
		} {
			if as[masked]["buying_power"] != want[0] || as[masked]["buying_power_is"] != want[1] {
				t.Errorf("%s's buying power: %v", masked, as[masked])
			}
			if _, ok := as[masked]["figures"]; ok {
				t.Errorf("%s has figures in a compact answer", masked)
			}
		}
		if as["...135"]["cash"] != nil || as["...398"]["cash"] != 5210.44 || r.value["total_cash"] != nil {
			t.Errorf("a cash that differs, and a margin account's cash in marginBalance: %s", r.text)
		}
		for _, want := range []string{
			"Schwab's cash for ...135 (Individual) is not its value less its holdings, so it is null.",
			"Schwab sent no current value or cash for ...783 (Contributory).",
		} {
			if !strings.Contains(r.text, want) {
				t.Errorf("no warning %q: %s", want, r.text)
			}
		}
	}},
	{tool: "get_balances", name: "detail", args: `{"account":"398","detail":"full"}`, check: func(t *testing.T, e *env, r result) {
		as := accountsOf(t, r)
		figures, _ := as["...398"]["figures"].([]any)
		if len(as) != 1 || len(figures) != 29 {
			t.Fatalf("...398's figures: %s", r.text)
		}
		if first, _ := figures[0].(map[string]any); first["name"] != "accruedInterest" {
			t.Errorf("the figures are not in Schwab's order: %v", figures[0])
		}
		if routes := apiRoutes(e.fake, 0); routes[len(routes)-1] != string(schwabtest.AccountRoute) {
			t.Errorf("one account's balances came from %v", routes)
		}
	}},
	{tool: "get_positions", name: "empty", args: `{"account":"783"}`, check: func(t *testing.T, _ *env, r result) {
		as, _ := r.value["accounts"].([]any)
		if len(positionsOf(r)) != 0 || len(as) != 1 || r.value["next_cursor"] != nil {
			t.Fatalf("an account that holds nothing: %s", r.text)
		}
		if a, _ := as[0].(map[string]any); a["positions"] != 0.0 || a["positions_value"] != 0.0 || a["open_pl"] != 0.0 {
			t.Errorf("an account that holds nothing: %v", a)
		}
	}},
	{tool: "get_positions", name: "full", args: `{}`, opts: []envOption{withUnreadablePosition()}, check: func(t *testing.T, e *env, r result) {
		if got := symbolsOf(r); !slices.Equal(got, fixturePositions) {
			t.Errorf("positions %q, want %q", got, fixturePositions)
		}
		ps := positionsOf(r)
		if put := ps[5]; put["quantity"] != -2.0 || put["value"] != -310.0 || put["cost"] != -320.0 || put["open_pl"] != 10.0 {
			t.Errorf("the short put: %v", put)
		}
		if vti, aapl := ps[3], ps[1]; vti["value"] != nil || vti["cost"] != nil || vti["open_pl"] != 733.23 || aapl["open_pl"] != nil {
			t.Errorf("figures that cannot be read: %v and %v", vti, aapl)
		}
		total, _ := r.value["total"].(map[string]any)
		if total["positions_value"] != nil || total["open_pl"] != nil || total["value"] != nil {
			t.Errorf("totals that need what cannot be read: %v", total)
		}
		for _, p := range ps {
			if p["portfolio_weight"] != nil {
				t.Errorf("%v has a portfolio weight, though ...783 sends no value", p["symbol"])
			}
		}
		if !slices.Contains(r.warnings(), "problems") || !slices.Contains(r.warnings(), "figures_missing") {
			t.Errorf("no warning of what cannot be read: %v", r.warnings())
		}
		if routes := apiRoutes(e.fake, 0); routes[len(routes)-1] != string(schwabtest.AccountsRoute) {
			t.Errorf("the positions came from %v", routes)
		}
	}},
	{tool: "get_positions", name: "symbol", args: `{"symbol":"xyz"}`, check: func(t *testing.T, _ *env, r result) {
		if got := symbolsOf(r); !slices.Equal(got, []string{"...398 XYZ   260619P00045000"}) {
			t.Errorf("xyz keeps %q, want its put", got)
		}
		if total, _ := r.value["total"].(map[string]any); total["positions_value"] != -310.0 || total["open_pl"] != 10.0 {
			t.Errorf("the totals of what symbol keeps: %v", total)
		}
	}},
	{tool: "get_positions", name: "pages", args: `{"limit":2}`, check: func(t *testing.T, e *env, r result) {
		got, total := symbolsOf(r), fmt.Sprint(r.value["total"])
		for cursor := r.value["next_cursor"]; cursor != nil; {
			next := e.call("get_positions", map[string]any{"limit": 2, "cursor": cursor})
			if next.isError || len(positionsOf(next)) > 2 || fmt.Sprint(next.value["total"]) != total {
				t.Fatalf("a later page: %s", next.text)
			}
			got = append(got, symbolsOf(next)...)
			cursor = next.value["next_cursor"]
		}
		if !slices.Equal(got, fixturePositions) {
			t.Errorf("the pages hold %q, want %q", got, fixturePositions)
		}
		// The totals schwabtx positions prints for the fixtures.
		if tot, _ := r.value["total"].(map[string]any); tot["positions_value"] != 5758.91 || tot["positions_cost"] != 5657.99 || tot["open_pl"] != 100.92 {
			t.Errorf("the totals: %v", tot)
		}
	}},
	{
		tool: "get_positions", name: "detail", args: `{"account":"398","detail":"full"}`,
		opts: []envOption{withFake(func(f *schwabtest.Server) { f.RemoveAccount(schwabtest.HashFor(empty)) })},
		check: func(t *testing.T, _ *env, r result) {
			ps := positionsOf(r)
			if len(ps) != 2 {
				t.Fatalf("...398's positions: %s", r.text)
			}
			meta, put := ps[0], ps[1]
			// META's 603.40 of ...398's 5503.84, and of every account's 12501.52.
			if meta["account_weight"] != 0.1096 || meta["portfolio_weight"] != 0.0483 || put["account_weight"] != -0.0563 {
				t.Errorf("the weights: %v and %v", meta, put)
			}
			detail, _ := put["detail"].(map[string]any)
			if detail["put_call"] != "PUT" || detail["underlying"] != "XYZ" || detail["short_quantity"] != 2.0 || detail["long_quantity"] != 0.0 ||
				detail["maintenance_requirement"] != 900.0 || detail["cusip"] != "0XYZ..RI60045000" {
				t.Errorf("the put's detail: %v", detail)
			}
			if tot, _ := r.value["total"].(map[string]any); tot["value"] != 5503.84 || tot["positions_value"] != 293.4 || tot["open_pl"] != 101.4 {
				t.Errorf("...398's totals: %v", tot)
			}
		},
	},
}

// list_accounts answers from the store, without values, when Schwab cannot
// be read for a reason of the login or the app's set-up: a lapsed login, or
// the page Schwab answers until the app is ready, here for the accounts
// after the directory read the account numbers. An account is the primary
// one as the store's last read of the preferences says, and primary is
// null for one that no read of them named. A failure of Schwab's own, no
// accounts, or a store that does not open or knows no account, is the
// call's error. The accounts endpoint's answers take turns on one server,
// whose directory the first call read.
func TestListAccountsFromTheStore(t *testing.T) {
	t.Parallel()
	check := func(name string, r result, code string, fromStore bool) {
		t.Helper()
		switch {
		case fromStore && (r.isError || r.value["source"] != "store" || !strings.Contains(r.text, "without values or hashes: ["+code+"] ")):
			t.Errorf("%s: %s, want the store's accounts", name, r.text)
		case !fromStore && r.code() != code:
			t.Errorf("%s: %s, want [%s]", name, r.text, code)
		}
	}
	e := newEnv(t)
	for _, tc := range []struct {
		name      string
		answer    http.HandlerFunc
		code      string // the error's, or api_unavailable's
		fromStore bool
	}{
		{"app not ready", schwabtest.Whitelabel, "app_not_ready", true},
		{"Schwab down", schwabtest.Unavailable, "schwab_unavailable", false},
		{"no accounts", schwabtest.Answer(200, "[]"), "schwab_refused", false},
	} {
		e.fake.Override(schwabtest.AccountsRoute, tc.answer)
		check(tc.name, e.call("list_accounts", `{}`), tc.code, tc.fromStore)
	}
	check("lapsed", newEnv(t, withLoginAt(testNow.Add(-8*24*time.Hour))).call("list_accounts", `{}`), "login_expired", true)
	unnamed := storeCopy(t)
	exec(t, rawStore(t, unnamed), "UPDATE account SET primary_account = NULL WHERE account_id IN (1, 3)")
	r := newEnv(t, withStore(unnamed), withLoginAt(testNow.Add(-8*24*time.Hour))).call("list_accounts", `{}`)
	check("lapsed, beside a store that lacks the preferences", r, "login_expired", true)
	if as := accountsOf(t, r); as["...135"]["primary"] != nil || as["...783"]["primary"] != nil || as["...398"]["primary"] != false {
		t.Errorf("primary from the store: %v, %v and %v", as["...135"], as["...398"], as["...783"])
	}
	loggedOut := newEnv(t, withoutLogin(), withoutStore())
	check("no store", loggedOut.call("list_accounts", `{}`), "not_logged_in", false)
	// A store made since, by schwabtx, which knows no account yet: the
	// server finds it at the next call.
	s, err := history.Open(t.Context(), history.Options{Path: loggedOut.storePath, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	check("a store that knows no account", loggedOut.call("list_accounts", `{}`), "not_logged_in", false)
}

// An account keeps the kind the user gave it in the store, with the known
// numbers in it masked, as the store's text is, here one of 7 digits that
// only the store knows, which mask.Text leaves; and from the store, once
// the login is gone, the date the login stopped sharing it, in New York,
// and the accounts that only the store knows.
func TestListAccountsShowsTheStoresAccounts(t *testing.T) {
	t.Parallel()
	path := storeCopy(t)
	exec(t, rawStore(t, path),
		"INSERT INTO account (number, created_from, first_seen_at, last_seen_at) "+
			"VALUES ('"+sevenDigits+"', 'user', '2026-01-05T19:11:00.000Z', '2026-01-05T19:11:00.000Z')",
		"UPDATE account SET kind = 'gift from "+sevenDigits+"' WHERE account_id = 1",
		"UPDATE account SET not_shared_since = '2026-11-01T03:30:00.000Z' WHERE account_id = 3")
	e := newEnv(t, withStore(path))
	e.plant(sevenDigits)
	for _, tc := range []struct {
		name      string
		loggedOut bool
		accounts  int
		since     any
	}{
		{"from Schwab", false, 3, nil},
		{"from the store", true, 4, "2026-10-31"},
	} {
		if tc.loggedOut {
			if err := os.Remove(e.tokenFile); err != nil {
				t.Fatal(err)
			}
		}
		as := accountsOf(t, e.call("list_accounts", `{}`))
		if len(as) != tc.accounts || as["...135"]["kind"] != "gift from ...321" || as["...783"]["not_shared_since"] != tc.since {
			t.Errorf("%s: %d accounts, %v and %v", tc.name, len(as), as["...135"], as["...783"])
		}
	}
}

// A store that cannot be read leaves list_accounts' refs and kinds null,
// and says why. No store at all is no failure, and says nothing (the
// cases' empty).
func TestListAccountsWithoutTheStore(t *testing.T) {
	t.Parallel()
	path := storeCopy(t)
	exec(t, rawStore(t, path), "PRAGMA user_version = 2")
	r := newEnv(t, withStore(path)).call("list_accounts", `{}`)
	if r.isError || accountsOf(t, r)["...135"]["store_ref"] != nil || !slices.Contains(r.warnings(), "store_unavailable") ||
		!strings.Contains(r.text, "written by a newer schwabtx") {
		t.Errorf("a store newer than the server: %s", r.text)
	}
}

// list_accounts quotes an error in a warning as get_transactions does:
// whole, its code, its sentence and Schwab's detail. Here, an app that
// Schwab does not serve yet, for which list_accounts answers from the
// store (api_unavailable), and a store that a newer schwabtx wrote, which
// leaves its refs null (store_unavailable).
func TestListAccountsQuotesErrorsWhole(t *testing.T) {
	t.Parallel()
	reason := func(r result, code string) string {
		ws, _ := r.value["warnings"].([]any)
		for _, w := range ws {
			if m, ok := w.(map[string]any); ok && m["code"] == code {
				_, why, _ := strings.Cut(fmt.Sprint(m["message"]), ": ")
				return why
			}
		}
		return ""
	}
	notReady := newEnv(t)
	notReady.fake.Override(schwabtest.AccountNumbersRoute, schwabtest.Whitelabel)
	newer := storeCopy(t)
	exec(t, rawStore(t, newer), "PRAGMA user_version = 2")
	newerStore := newEnv(t, withStore(newer))
	for _, tc := range []struct {
		e            *env
		warning, why string // why the reason starts with
	}{
		{notReady, "api_unavailable", "[app_not_ready] Schwab answers this way until"},
		{newerStore, "store_unavailable", "[store_newer] The store was written by a newer schwabtx."},
	} {
		accounts := reason(tc.e.call("list_accounts", `{}`), tc.warning)
		transactions := reason(tc.e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`), tc.warning)
		if !strings.HasPrefix(transactions, tc.why) || accounts != transactions {
			t.Errorf("%s: list_accounts quotes %q, get_transactions %q", tc.warning, accounts, transactions)
		}
	}
	if why := reason(notReady.call("list_accounts", `{}`), "api_unavailable"); !strings.Contains(why, " Detail: HTTP 500 from Schwab") {
		t.Errorf("api_unavailable quotes no detail of Schwab's: %q", why)
	}
}

// An account that the directory does not know, as one shared at a login
// since it was read, makes the directory read again, once, so that the
// account comes with its hash.
func TestAccountsNewSinceTheDirectory(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore())
	e.plant("12345678")
	if r := e.call("list_accounts", `{}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.AddAccount(schwabtest.Account{Number: "12345678", Details: bareAccount("12345678")})
	before := len(e.fake.Requests())
	for _, tool := range []string{"list_accounts", "get_balances"} {
		r := e.call(tool, `{}`)
		if a := accountsOf(t, r)["...678"]; a == nil || a["account_hash"] != schwabtest.HashFor("12345678") {
			t.Errorf("%s: the new account: %s", tool, r.text)
		}
	}
	if n := strings.Count(strings.Join(apiRoutes(e.fake, before), " "), string(schwabtest.AccountNumbersRoute)); n != 1 {
		t.Errorf("the directory was read %d times again, want once", n)
	}
}

// The directory keeps Schwab's part for 5 minutes, and reads the store's
// accounts at every call (design 12.4): a store that appears after the
// directory was read, as the first schwabtx import while a client runs
// makes it, is read by the next call. source store and auto read the
// accounts it holds, get_transaction finds a ref that only it holds, and
// list_accounts names each account's store ref; a kind that the user
// gives an account since shows at the next call.
func TestStoreFoundAfterTheDirectory(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore())
	if r := e.call("list_accounts", `{}`); r.isError || accountsOf(t, r)["...398"]["store_ref"] != nil {
		t.Fatalf("before the store: %s", r.text)
	}
	copyFile(t, sharedStore(t), e.storePath)
	before := len(e.fake.Requests())
	r := e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31","source":"store"}`)
	if r.isError || txnCoverageOf(r, "...135")["read_from"] != "store" || txnCoverageOf(r, "...398")["read_from"] != "store" ||
		len(r.value["totals"].([]any)) == 0 {
		t.Errorf("source store, after the store appeared: %.600s", r.text)
	}
	r = e.call("get_transactions", `{"from":"2025-01-01","to":"2025-12-31"}`)
	if r.isError || txnCoverageOf(r, "...398")["read_from"] != "store" || txnCoverageOf(r, "...135")["read_from"] != "schwab" {
		t.Errorf("auto, after the store appeared: %.600s", r.text)
	}
	if r := e.call("get_transaction", `{"id":"C2cdf9679.3"}`); r.isError || r.value["source"] != "store" {
		t.Errorf("a ref that only the store holds: %.300s", r.text)
	}
	exec(t, rawStore(t, e.storePath), "UPDATE account SET kind = 'taxable' WHERE account_id = 2")
	as := accountsOf(t, e.call("list_accounts", `{}`))
	if as["...135"]["store_ref"] != "A1" || as["...398"]["kind"] != "taxable" {
		t.Errorf("list_accounts, after the store appeared and a kind was given: %v and %v", as["...135"], as["...398"])
	}
	if n := strings.Count(strings.Join(apiRoutes(e.fake, before), " "), string(schwabtest.AccountNumbersRoute)); n != 0 {
		t.Errorf("Schwab's part of the directory was read %d times again, want none", n)
	}
}

// list_accounts lists at most 50 accounts. The first and the last of 51
// stand for every one in the check of what the server sent, as each goes
// the same way.
func TestListAccountsBound(t *testing.T) {
	t.Parallel()
	var numbers []string
	e := newEnv(t, withoutFixtures(), withoutStore(), withFake(func(f *schwabtest.Server) {
		for i := range maxAccounts + 1 {
			n := fmt.Sprintf("1234%04d", i)
			numbers = append(numbers, n)
			f.AddAccount(schwabtest.Account{Number: n, Details: bareAccount(n)})
		}
	}))
	e.plant(numbers[0], numbers[maxAccounts])
	if r := e.call("list_accounts", `{}`); r.code() != "too_big" || !strings.Contains(r.text, "51 accounts") {
		t.Errorf("51 accounts: %s", r.text)
	}
}

// One account's balances come from its own endpoint, with its hash as the
// directory knows it after Schwab refused the one it had.
func TestBalancesOfARehashedAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore())
	if r := e.call("get_balances", `{"account":"Individual"}`); r.isError {
		t.Fatal(r.text)
	}
	e.fake.Rehash(schwabtest.HashFor(individual), schwabtest.HashFor("rehashed"))
	r := e.call("get_balances", `{"account":"135"}`)
	if a := accountsOf(t, r)["...135"]; a == nil || a["account_hash"] != schwabtest.HashFor("rehashed") || a["value"] != 6997.68 {
		t.Errorf("after a new hash: %s", r.text)
	}
}

// The figures of detail full are every number of the current balances that
// schwab.Balances reads, under its names, in Schwab's order: one that could
// not be read is null, a name sent twice keeps its first place and its last
// value, and a key the record does not read, a null and a value that is no
// number are left out.
func TestBalanceFigures(t *testing.T) {
	t.Parallel()
	var a schwab.Account
	js := `{"securitiesAccount":{"type":"MARGIN","accountNumber":"12345678","currentBalances":{` +
		`"liquidationValue":"5503.84","cashBalance":5210.44,"isInCall":false,"newFigure12345678":12345678,` +
		`"buyingPower":null,"equity":1,"CASHBALANCE":7,"sma":1e400}}}`
	if err := json.Unmarshal([]byte(js), &a); err != nil {
		t.Fatal(err)
	}
	b, err := encode(balanceFigures(a))
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"name":"liquidationValue","value":null},{"name":"cashBalance","value":7.00},{"name":"equity","value":1.00},` +
		`{"name":"sma","value":null}]`; string(b) != want {
		t.Errorf("figures %s\nwant    %s", b, want)
	}
	if len(figureNames) < 40 || !slices.Contains(figureNames, "liquidationValue") || slices.Contains(figureNames, "isInCall") {
		t.Errorf("the figures' names: %v", figureNames)
	}
}

// accountPlantings are the account tools' share of TestNoAccountNumberLeaks'
// world: ...398 sends its value as a string that holds its number, which no
// figure can read, and a count as a number that holds it, which a problem
// quotes; one account's own endpoint answers Schwab's error, which quotes
// numbers; and of ...135's positions, one has a description that holds two
// numbers, one of 7 digits that only the known numbers catch, and another
// a time that holds one, which a problem quotes. No cursor of
// get_positions may hold a digest over a number, and the account
// resources and the prompt, given full numbers, echo none.
var accountPlantings = []func(t *testing.T, w *leakWorld){func(_ *testing.T, w *leakWorld) {
	w.opts = append(w.opts, withFake(func(f *schwabtest.Server) {
		changeAccount(f, options, func(a map[string]any) {
			balancesOf(a)["liquidationValue"] = options
			a["securitiesAccount"].(map[string]any)["roundTrips"] = 61803398.5
		})
		f.Override(schwabtest.AccountRoute, schwabtest.Answer(400,
			`{"message":"Invalid account number 29470135","errors":["account 61803398 is closed"]}`))
	}))
	w.masked = append(w.masked, "Schwab sent the number ...398.5 where a whole number was expected", "Invalid account number ...135")
}, func(_ *testing.T, w *leakWorld) {
	w.opts = append(w.opts, withFake(func(f *schwabtest.Server) {
		changeAccount(f, individual, func(a map[string]any) {
			for _, p := range heldBy(a) {
				in := p.(map[string]any)["instrument"].(map[string]any)
				switch in["symbol"] {
				case "VTI":
					in["description"] = "TRANSFER FROM 61803398 FOR " + sevenDigits
				case "AAPL":
					in["expirationDate"] = individual
				}
			}
		})
	}))
	w.masked = append(w.masked, "TRANSFER FROM ...398 FOR ...321", `...135\", which is not a time`)
	w.checks = append(w.checks, positionCursorsHoldNoNumber, accountReadsHoldNoNumber)
	w.sides = append(w.sides, accountSides...)
}}

// accountSides are the account tools' worlds beside the leak world, whose
// bodies from Schwab hold numbers that no account the server knows has:
// Schwab limiting the app, with a rate file whose name holds a number, and
// alone in its world, since the pause it asks for holds back every later
// request; and Schwab's edge refusing an account, its Whitelabel page for
// another, a gateway's refusal of an answer too big, and two accounts of
// one nickname.
var accountSides = []leakSide{
	{name: "Schwab limits the app", opts: func(home string) []envOption {
		return []envOption{
			withConfig(func(c *config.Config) { c.RateFile.Value = filepath.Join(home, ".cache", "rate-"+unknownShort) }),
			withFake(func(f *schwabtest.Server) {
				f.Override(schwabtest.AccountsRoute, schwabtest.Answer(http.StatusTooManyRequests,
					`{"message":"Too many requests for `+unknownLong+`"}`, "Retry-After", "3600"))
			}),
		}
	}, calls: []leakCall{{tool: "get_balances", args: `{}`}}},
	{name: "Schwab refuses the accounts", opts: func(string) []envOption {
		return []envOption{withFake(func(f *schwabtest.Server) {
			f.Queue(schwabtest.AccountRoute, schwabtest.Answer(http.StatusForbidden, `<html><head><title>Access Denied for `+unknownLong+
				`</title></head><body>Reference #18.6a2f1c17.1700000000.1a2b3c4d</body></html>`), schwabtest.Whitelabel, schwabtest.Whitelabel)
			f.Override(schwabtest.AccountsRoute, schwabtest.TooBigBody)
			f.SetUserPreference(`{"accounts":[{"accountNumber":"` + nineDigits + `","nickName":"Kids ` + sevenDigits + `"},` +
				`{"accountNumber":"` + sevenDigits + `","nickName":"Kids ` + sevenDigits + `"}]}`)
		})}
	}, calls: []leakCall{
		{tool: "get_balances", args: `{"account":"135"}`},
		{tool: "get_balances", args: `{"account":"398"}`},
		{tool: "get_positions", args: `{}`},
		{tool: "get_balances", args: `{"account":"Kids ` + sevenDigits + `"}`},
	}},
}

// accountLeakCalls are the account tools' calls of TestNoAccountNumberLeaks,
// with each detail: every account in full, one by its full number, and one
// that is no account, by a nickname that holds a number; and pages of
// positions, whose cursors hold a digest of their arguments.
var accountLeakCalls = []leakCall{
	{tool: "list_accounts", args: `{}`},
	{tool: "get_balances", args: `{"detail":"full"}`},
	{tool: "get_balances", args: `{"account":"29470135"}`},
	{tool: "get_balances", args: `{"account":"Nobody 61803398"}`},
	{tool: "get_positions", args: `{"detail":"full"}`},
	{tool: "get_positions", args: `{"account":"29470135","limit":1}`},
}

// accountSizeCases are the account tools' cases of TestSizeCap. get_balances
// with detail full for 30 margin accounts of 14 figures each passes the
// cap: it fits by cutting the accounts' figures, each list keeping its
// first ones, and cut names each list cut; the values and the totals stay
// whole, and list_cut says how to see one account's figures in full. And
// get_positions' pages (sizePositions).
var accountSizeCases = []sizeCase{{name: "get_balances full", opts: []envOption{withoutFixtures(), withoutStore(), withFake(func(f *schwabtest.Server) {
	const figures = `"liquidationValue":13846.92,"cashBalance":1501.25,"buyingPower":3002.5,"availableFunds":1501.25,` +
		`"equity":13846.92,"longMarketValue":12345.67,"shortMarketValue":0,"longOptionMarketValue":0,"shortOptionMarketValue":0,` +
		`"mutualFundValue":0,"bondValue":0,"maintenanceRequirement":3703.7,"sma":1501.25,"longMarginValue":12345.67`
	for _, n := range manyAccounts {
		details := strings.Replace(bareAccount(n), `"currentBalances":{}`, `"currentBalances":{`+figures+`}`, 1)
		f.AddAccount(schwabtest.Account{Number: n, Details: details})
	}
})}, run: func(t *testing.T, e *env) {
	e.plant(manyAccounts[0], manyAccounts[len(manyAccounts)-1]) // each goes the same way
	r := e.call("get_balances", `{"detail":"full"}`)
	if r.isError {
		t.Fatal(r.text)
	}
	var got struct {
		Cut      []cutList `json:"cut"`
		Accounts []struct {
			Value   *float64 `json:"value"`
			Figures []struct {
				Name Ident `json:"name"`
			} `json:"figures"`
		} `json:"accounts"`
		TotalValue *float64 `json:"total_value"`
		TotalCash  *float64 `json:"total_cash"`
	}
	if err := json.Unmarshal(r.raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != len(manyAccounts) || got.TotalValue == nil || *got.TotalValue != 415407.6 || got.TotalCash == nil ||
		*got.TotalCash != 45037.5 || len(got.Cut) == 0 {
		t.Fatalf("%d accounts, totals %v and %v, cut %v", len(got.Accounts), got.TotalValue, got.TotalCash, got.Cut)
	}
	for i, a := range got.Accounts {
		if a.Value == nil || *a.Value != 13846.92 {
			t.Errorf("account %d lost its value", i)
		}
		shown := shownOf(got.Cut, Ident(fmt.Sprintf("accounts.%d.figures", i)))
		if shown < 0 {
			shown = 14
		}
		if len(a.Figures) != shown || shown > 0 && a.Figures[0].Name != "liquidationValue" {
			t.Errorf("account %d shows %d figures, and cut says %d", i, len(a.Figures), shown)
		}
	}
	for _, c := range got.Cut {
		if c.Total != 14 {
			t.Errorf("cut %s: total %d, want 14", c.Path, c.Total)
		}
	}
	if !slices.Contains(r.warnings(), "list_cut") || !strings.Contains(r.text, "Pass account for one account's figures in full.") {
		t.Errorf("no list_cut that says how to see them all: %v", r.warnings())
	}
}}, {name: "get_positions pages", opts: []envOption{withoutStore(), withBackstopAllowed(), withManyPositions()}, run: sizePositions}}

// manyPositionsCount is how many positions withManyPositions gives ...135:
// more than a page of the budget holds.
const manyPositionsCount = 100

// withManyPositions gives ...135 100 positions in place of its own, the
// first of which, by symbol, is an option whose strike's digits are
// ...135's number: the backstop, blind to kinds, withholds them after the
// page is cut.
func withManyPositions() envOption {
	return withFake(func(f *schwabtest.Server) {
		changeAccount(f, individual, func(a map[string]any) {
			ps := []any{position("AAAA  261218C"+individual, "OPTION")}
			for i := range manyPositionsCount - 1 {
				ps = append(ps, position(fmt.Sprintf("S%04d", i), "EQUITY"))
			}
			a["securitiesAccount"].(map[string]any)["positions"] = ps
		})
	})
}

// position returns a long position of one share of symbol, decoded as
// changeAccount takes it.
func position(symbol, assetType string) map[string]any {
	return map[string]any{
		"longQuantity": 1, "shortQuantity": 0, "averagePrice": 10, "marketValue": 12.34, "longOpenProfitLoss": 2.34,
		"maintenanceRequirement": 0, "instrument": map[string]any{
			"assetType": assetType, "symbol": symbol, "description": "A COMPANY WITH A LONG NAME, CLASS A COMMON SHARES " + symbol,
		},
	}
}

// sizePositions is TestSizeCap's case of get_positions: the pages of 100
// positions, read through their cursors, fill to the page budget before
// the limit of 100, and hold every position once, in order. And the
// margin between the budget and the cap: the first page, filled to the
// budget, gains the warning value_withheld when the backstop withholds
// the digits of its first symbol, and stays within the cap.
func sizePositions(t *testing.T, e *env) {
	var symbols []string
	var cursor any
	for page := 0; ; page++ {
		args := map[string]any{"account": "135"}
		if cursor != nil {
			args["cursor"] = cursor
		}
		r := e.call("get_positions", args)
		if r.isError {
			t.Fatal(r.text)
		}
		n := len(positionsOf(r))
		symbols = append(symbols, symbolsOf(r)...)
		cursor = r.value["next_cursor"]
		budget := e.srv.o.PageBytes
		if page == 0 {
			budget += 200 // value_withheld, which comes after the page is cut
			if !slices.Contains(r.warnings(), "value_withheld") || positionsOf(r)[0]["symbol"] != "AAAA  261218C...135" {
				t.Errorf("the first page: %v, first symbol %v", r.warnings(), positionsOf(r)[0]["symbol"])
			}
		}
		if len(r.text) > budget || cursor != nil && (n >= 100 || len(r.text) < e.srv.o.PageBytes-1000) {
			t.Errorf("page %d holds %d positions in %d bytes", page, n, len(r.text))
		}
		if cursor == nil {
			break
		}
	}
	want := []string{"...135 AAAA  261218C...135"}
	for i := range manyPositionsCount - 1 {
		want = append(want, fmt.Sprintf("...135 S%04d", i))
	}
	if !slices.Equal(symbols, want) {
		t.Errorf("the pages hold %d positions, want each of %d once, in order", len(symbols), len(want))
	}
}

// The account tools' tests join the shared ones: each registration answers
// true, so that the three are made, in this order, before any test runs.
var _ = addToolCases(accountCases...) && addLeaks(accountPlantings, accountLeakCalls...) && addSizeCases(accountSizeCases...)

// manyAccounts are the 30 made-up accounts of the size case of
// get_balances.
var manyAccounts = func() []string {
	var ns []string
	for i := range 30 {
		ns = append(ns, fmt.Sprintf("5555%04d", i))
	}
	return ns
}()

// cursorField is a cursor in what the server sent: in a structured content,
// or in a text block that holds one.
var cursorField = regexp.MustCompile(`next_cursor\\?":\\?"([A-Za-z0-9_-]+)`)

// positionCursorsHoldNoNumber checks the cursors of get_positions that the
// server sent: none holds a digest of its arguments with a planted number
// in place of the account's key, and one of an account's page holds the
// digest over its hash, so that the check sees what it looks for.
func positionCursorsHoldNoNumber(t *testing.T, e *env) {
	t.Helper()
	overHash := 0
	for _, line := range e.sent() {
		for _, m := range cursorField.FindAllStringSubmatch(line, -1) {
			b, err := base64.RawURLEncoding.DecodeString(m[1])
			var body cursorBody
			if err != nil || json.Unmarshal(b, &body) != nil || body.T != "get_positions" {
				continue
			}
			for _, n := range e.planted {
				if f, _ := argsDigest(positionsArgs{Account: n, Detail: "compact"}); f == body.F {
					t.Errorf("a cursor holds a digest over the account number %s", n)
				}
			}
			if f, _ := argsDigest(positionsArgs{Account: schwabtest.HashFor(individual), Detail: "compact"}); f == body.F {
				overHash++
			}
		}
	}
	if overHash == 0 {
		t.Error("no cursor of one account's positions holds the digest over its hash")
	}
}

// accountReadsHoldNoNumber reads the account resources, two of them by a
// full number in their URI, and gets the prompt with a full number as its
// account; then checks what the server sent with assertClean, and that the
// masked forms are there.
func accountReadsHoldNoNumber(t *testing.T, e *env) {
	t.Helper()
	for i, uri := range []string{accountsURI, positionsOfURI(individual), positionsOfURI(options)} {
		r, err := e.read(uri)
		if err != nil {
			t.Errorf("resource %d: %v", i, err)
			continue
		}
		if c := r.Contents[0]; i > 0 && (strings.Contains(c.URI, individual) || !strings.Contains(c.Text, `"account":"...`)) {
			t.Errorf("resource %d: %s, %.200s", i, c.URI, c.Text)
		}
	}
	if text, err := e.getPrompt(map[string]string{"account": individual}); err != nil || !strings.Contains(text, " for account ...135.") {
		t.Errorf("the prompt for a full number: %v, %.80s", err, text)
	}
	e.assertClean()
}

// An account whose positions are not a list has positions null, and so
// have its sums and the totals; the other accounts' positions are listed.
// An account that the directory lists and Schwab's answer does not is no
// longer shared. A cursor of other arguments, or that does not decode, is
// bad_cursor; another limit is not; and one past the positions that a
// later read holds gives an empty last page.
func TestPositionsOfAnUnreadableAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		changeAccount(f, options, func(a map[string]any) {
			a["securitiesAccount"].(map[string]any)["positions"] = map[string]any{"longQuantity": 1}
		})
	}))
	r := e.call("get_positions", `{}`)
	as, _ := r.value["accounts"].([]any)
	if len(as) != 3 || len(positionsOf(r)) != 4 {
		t.Fatalf("the accounts: %s", r.text)
	}
	unread, _ := as[1].(map[string]any)
	total, _ := r.value["total"].(map[string]any)
	if unread["positions"] != nil || unread["positions_value"] != nil || total["positions_value"] != nil || total["open_pl"] != nil {
		t.Errorf("an account whose positions are not a list: %v, total %v", unread, total)
	}
	e.fake.Override(schwabtest.AccountsRoute, schwabtest.Answer(200, "[]"))
	if r := e.call("get_positions", `{"account":"398"}`); r.code() != "schwab_refused" {
		t.Errorf("no accounts: %s", r.text)
	}
	as2, err := fixtureAccounts()
	if err != nil {
		t.Fatal(err)
	}
	e.fake.Override(schwabtest.AccountsRoute, schwabtest.Answer(200, "["+string(as2[individual])+"]"))
	if r := e.call("get_positions", `{"account":"398"}`); r.code() != "account_not_shared" || !strings.Contains(r.text, "...398") {
		t.Errorf("an account Schwab's answer lacks: %s", r.text)
	}
	e.fake.Override(schwabtest.AccountsRoute, schwabtest.Answer(200, "["+string(as2[individual])+","+string(as2[options])+"]"))
	first := e.call("get_positions", `{"limit":1}`)
	for _, args := range []map[string]any{
		{"limit": 1, "symbol": "VTI", "cursor": first.value["next_cursor"]},
		{"limit": 1, "account": "135", "cursor": first.value["next_cursor"]},
		{"limit": 1, "detail": "full", "cursor": first.value["next_cursor"]},
		{"cursor": "not a cursor"},
	} {
		if r := e.call("get_positions", args); r.code() != "bad_cursor" {
			t.Errorf("%v: %s", args, r.text)
		}
	}
	if r := e.call("get_positions", map[string]any{"limit": 3, "cursor": first.value["next_cursor"]}); r.isError || len(positionsOf(r)) != 3 {
		t.Errorf("the next page, with another limit: %s", r.text)
	}
	past := e.call("get_positions", `{"limit":5}`)
	e.fake.Override(schwabtest.AccountsRoute, schwabtest.Answer(200, "["+string(as2[individual])+"]"))
	if r := e.call("get_positions", map[string]any{"cursor": past.value["next_cursor"]}); r.isError || len(positionsOf(r)) != 0 || r.value["next_cursor"] != nil {
		t.Errorf("a cursor past the 4 positions that remain: %s", r.text)
	}
}

// The weights of one account's positions in the portfolio divide by every
// shared account's value, so an account that is not picked and sent no
// value, as the fixtures' ...783 does, leaves each null, and the warning
// figures_missing says why, in get_positions and in the positions resource
// alike; and so does one whose value could not be read, here ...135's two
// values sent as strings beside ...783's value, which the warning problems
// names.
func TestPortfolioWeightsOfOneAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore())
	const why = "Schwab sent no current value for ...783 (Contributory), so every portfolio_weight is null."
	r := e.call("get_positions", `{"account":"398","detail":"full"}`)
	if r.isError || !slices.Contains(r.warnings(), "figures_missing") || !strings.Contains(r.text, why) {
		t.Errorf("one account's positions beside an account of no value: %.600s", r.text)
	}
	for _, p := range positionsOf(r) {
		if p["portfolio_weight"] != nil {
			t.Errorf("%v has a portfolio weight", p["symbol"])
		}
	}
	res, err := e.readResource("schwab://accounts/398/positions")
	if err != nil || !strings.Contains(res.Contents[0].Text, why) {
		t.Errorf("the positions resource: %v, %v", res, err)
	}
	if r := e.call("get_positions", `{"account":"783"}`); strings.Contains(r.text, "every portfolio_weight is null") {
		t.Errorf("the account of no value picked: %.600s", r.text)
	}

	e = newEnv(t, withoutStore(), withFake(func(f *schwabtest.Server) {
		changeAccount(f, individual, func(a map[string]any) {
			balancesOf(a)["liquidationValue"] = "6997.68"
			a["aggregatedBalance"].(map[string]any)["currentLiquidationValue"] = "6997.68"
		})
		changeAccount(f, empty, func(a map[string]any) { balancesOf(a)["liquidationValue"] = 100.0 })
	}))
	const unread = "Schwab's current value for ...135 (Individual) could not be read, so every portfolio_weight is null."
	r = e.call("get_positions", `{"account":"398"}`)
	if r.isError || !slices.Contains(r.warnings(), "problems") || !strings.Contains(r.text, unread) {
		t.Errorf("one account's positions beside an account whose value cannot be read: %.600s", r.text)
	}
	for _, p := range positionsOf(r) {
		if p["portfolio_weight"] != nil || p["account_weight"] == nil {
			t.Errorf("%v's weights: %v and %v", p["symbol"], p["account_weight"], p["portfolio_weight"])
		}
	}
	if r := e.call("get_positions", `{"account":"135"}`); strings.Contains(r.text, "every portfolio_weight is null") {
		t.Errorf("the account whose value cannot be read picked: %.600s", r.text)
	}
}

// weight is a value's share of a base, to 4 places, null for either null
// and for a base not above 0.
func TestWeight(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		value, base lookup.Money
		want        string
	}{
		{lookup.Amount(3748.92), lookup.Amount(6997.68), "0.5357"},
		{lookup.Amount(-310), lookup.Amount(5503.84), "-0.0563"},
		{lookup.Amount(0), lookup.Amount(100), "0"},
		{lookup.Amount(1), lookup.Amount(0), "null"},
		{lookup.Amount(1), lookup.Amount(-5), "null"},
		{lookup.Money{}, lookup.Amount(100), "null"},
		{lookup.Amount(1), lookup.Money{}, "null"},
	} {
		if b, err := encode(weight(tc.value, tc.base)); err != nil || string(b) != tc.want {
			t.Errorf("weight(%v, %v) = %s, want %s", tc.value, tc.base, b, tc.want)
		}
	}
}

// detail full gives each position its identifiers, both its quantities and
// its maintenance requirement, each null when Schwab did not send it or it
// could not be read, and its problems, with each open profit or loss that
// it lacks of a side it holds.
func TestPositionDetail(t *testing.T) {
	t.Parallel()
	e := newEnv(t, withoutStore(), withUnreadablePosition(), withFake(func(f *schwabtest.Server) {
		changeAccount(f, options, func(a map[string]any) {
			for _, p := range heldBy(a) {
				p := p.(map[string]any)
				delete(p, "maintenanceRequirement")
				switch p["instrument"].(map[string]any)["symbol"] {
				case "META":
					p["longQuantity"] = "1"
				case "XYZ   260619P00045000":
					p["shortQuantity"] = "2"
				}
			}
		})
	}))
	details := map[string]map[string]any{}
	for _, p := range positionsOf(e.call("get_positions", `{"detail":"full"}`)) {
		details[fmt.Sprint(p["symbol"])], _ = p["detail"].(map[string]any)
	}
	problems := func(symbol string) string { return fmt.Sprint(details[symbol]["problems"]) }
	if !strings.Contains(problems("AAPL"), "path:longOpenProfitLoss what:missing, though Schwab sends it on every long position") ||
		!strings.Contains(problems("VTI"), "path:marketValue what:Schwab sent a string where a number was expected") ||
		!strings.Contains(problems("META"), "path:maintenanceRequirement what:missing") || problems("SWVXX") != "[]" {
		t.Errorf("the problems: AAPL %s, VTI %s, META %s, SWVXX %s", problems("AAPL"), problems("VTI"), problems("META"), problems("SWVXX"))
	}
	if details["META"]["maintenance_requirement"] != nil || details["META"]["long_quantity"] != nil || details["META"]["short_quantity"] != 0.0 ||
		details["VTI"]["maintenance_requirement"] != 0.0 || details["VTI"]["cusip"] != "922908769" {
		t.Errorf("META %v, VTI %v", details["META"], details["VTI"])
	}
	if put := details["XYZ   260619P00045000"]; put["short_quantity"] != nil || put["long_quantity"] != 0.0 {
		t.Errorf("a short quantity that cannot be read: %v", put)
	}
}
