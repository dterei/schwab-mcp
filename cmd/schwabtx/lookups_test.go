package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The requests a lookup makes, as runLookupOn gives them.
var (
	accountsRequest    = string(schwabtest.AccountsRoute) + "?"
	preferencesRequest = string(schwabtest.UserPreferenceRoute) + "?"
)

// The tables of the fixtures' three accounts.
const (
	accountsOfThree = `ACCOUNT  NAME              TYPE      VALUE
...0135  Individual        CASH    6997.68
...3398  Options & Margin  MARGIN  5503.84
...1783  Contributory      MARGIN        -
TOTAL                                    -
`
	accountsOfThreeShown = `ACCOUNT   NAME              TYPE      VALUE
29470135  Individual        CASH    6997.68
61803398  Options & Margin  MARGIN  5503.84
40221783  Contributory      MARGIN        -
TOTAL                                     -
`
	balancesOfThree = `ACCOUNT  NAME              TYPE      VALUE     CASH  BUYING POWER
...0135  Individual        CASH    6997.68  1532.17       1532.17
...3398  Options & Margin  MARGIN  5503.84  5210.44       8258.84
...1783  Contributory      MARGIN        -        -       8710.00
TOTAL                                    -        -
`
)

// setupFixtures starts the fake with the fixtures' three accounts, and logs
// in to it.
func setupFixtures(t *testing.T) *fakeSchwab {
	t.Helper()
	f, _ := setup(t)
	f.LoadFixtures()
	doLogin(t)
	return f
}

// runLookupOn runs schwabtx with args, as runTx does, and returns besides
// what it printed the API requests that it made, each as its route and its
// query.
func runLookupOn(t *testing.T, f *fakeSchwab, args ...string) (stdout, stderr string, requests []string, err error) {
	t.Helper()
	before := len(f.Requests())
	stdout, stderr, err = runTx(t, args...)
	for _, r := range f.Requests()[before:] {
		requests = append(requests, string(r.Route)+"?"+r.Query)
	}
	return stdout, stderr, requests, err
}

// preferenceBody returns the user preferences that list the accounts with
// the given numbers and nicknames, in pairs, with every key that Schwab
// sends on every record of its kind.
func preferenceBody(numbersAndNicknames ...string) string {
	var accounts []string
	for i := 0; i+1 < len(numbersAndNicknames); i += 2 {
		number, nickname := numbersAndNicknames[i], numbersAndNicknames[i+1]
		quoted, _ := json.Marshal(nickname)
		accounts = append(accounts, fmt.Sprintf(`{"accountNumber":%q,"primaryAccount":false,"type":"BROKERAGE","nickName":%s,`+
			`"displayAcctId":"...%s","autoPositionEffect":false,"accountColor":"Green"}`, number, quoted, number[len(number)-3:]))
	}
	return `{"accounts":[` + strings.Join(accounts, ",") + `],"streamerInfo":[{"streamerSocketUrl":"wss://streamer-api.schwab.com/ws",` +
		`"schwabClientCustomerId":"c","schwabClientCorrelId":"r","schwabClientChannel":"N9","schwabClientFunctionId":"APIAPP"}],` +
		`"offers":[{"level2Permissions":true,"mktDataPermission":"NP"}]}`
}

// The lookups print the fixtures' accounts as Schwab reports them, with the
// accounts' nicknames, from two requests: the accounts without their
// positions, and the user preferences. A figure that Schwab did not send is
// -, and so is its column's total, and a warning names the account.
func TestLookupsOfTheFixtures(t *testing.T) {
	for _, tc := range []struct {
		args           []string
		stdout, stderr string
	}{
		{[]string{"accounts"}, accountsOfThree, "warning: Schwab sent no current value for ...1783 (Contributory)\n"},
		{[]string{"accounts", "-show-accounts"}, accountsOfThreeShown, "warning: Schwab sent no current value for ...1783 (Contributory)\n"},
		{[]string{"balances"}, balancesOfThree, "warning: Schwab sent no current value or cash for ...1783 (Contributory)\n"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			f := setupFixtures(t)
			stdout, stderr, requests, err := runLookupOn(t, f, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			if stdout != tc.stdout {
				t.Errorf("printed\n%s\nwant\n%s", stdout, tc.stdout)
			}
			if stderr != tc.stderr {
				t.Errorf("warned %q, want %q", stderr, tc.stderr)
			}
			if want := []string{accountsRequest, preferencesRequest}; !equal(requests, want) {
				t.Errorf("requests %q, want %q", requests, want)
			}
		})
	}
}

// With every figure sent, the totals add up the accounts' figures, and
// nothing is warned of. Buying power does not pass between accounts, so it
// has no total.
func TestLookupsTotal(t *testing.T) {
	f := setupFixtures(t)
	f.RemoveAccount(schwabtest.HashFor("40221783"))
	for _, tc := range []struct {
		command, want string
	}{
		{"accounts", `ACCOUNT  NAME              TYPE       VALUE
...0135  Individual        CASH     6997.68
...3398  Options & Margin  MARGIN   5503.84
TOTAL                              12501.52
`},
		{"balances", `ACCOUNT  NAME              TYPE       VALUE     CASH  BUYING POWER
...0135  Individual        CASH     6997.68  1532.17       1532.17
...3398  Options & Margin  MARGIN   5503.84  5210.44       8258.84
TOTAL                              12501.52  6742.61
`},
	} {
		stdout, stderr, err := runTx(t, tc.command)
		if err != nil || stdout != tc.want || stderr != "" {
			t.Errorf("%s printed\n%s\nwarned %q, %v; want\n%s", tc.command, stdout, stderr, err, tc.want)
		}
	}
}

// fixtureDigits returns the first run of 5 digits of a fixture account's
// number that s holds, or "": the tables, the warnings and the errors show
// no more of a number than its last 4 digits unless -show-accounts asks.
func fixtureDigits(s string) string {
	for _, a := range schwabtest.FixtureAccounts {
		for i := 0; i+5 <= len(a.Number); i++ {
			if run := a.Number[i : i+5]; strings.Contains(s, run) {
				return run
			}
		}
	}
	return ""
}

// No output shows more than the last 4 digits of an account's number
// without -show-accounts: not the tables, nor the warnings, nor an error
// that echoes -account, though it holds most of a number.
func TestLookupsMaskAccountNumbers(t *testing.T) {
	setupFixtures(t)
	for _, args := range [][]string{
		{"accounts"},
		{"balances"},
		{"balances", "-account", "61803398"},
		{"accounts", "-account", "61803399"},
		{"accounts", "-account", "71803398"},
		{"accounts", "-account", "6180-3399"},
		{"balances", "-account", "...29470136"},
		{"accounts", "-account", "Roth"},
		{"accounts", "-json", "-account", "61803399"},
	} {
		stdout, stderr, err := runTx(t, args...)
		all := stdout + stderr
		if err != nil {
			all += err.Error()
		}
		if run := fixtureDigits(all); run != "" {
			t.Errorf("%q shows %s of an account's number:\n%s", args, run, all)
		}
	}
}

// -account picks one account from the answer for all of them: by the last
// digits of its number, written as the tables, Schwab or schwab.com write
// them, or by its whole nickname, whatever its case and the spaces around
// it. A nickname costs the preferences' request in -json mode too, and
// digits cost no request.
func TestLookupsPickAnAccount(t *testing.T) {
	f := setupFixtures(t)
	contributory := `ACCOUNT  NAME          TYPE    VALUE
...1783  Contributory  MARGIN      -
`
	margin := `ACCOUNT  NAME              TYPE      VALUE     CASH  BUYING POWER
...3398  Options & Margin  MARGIN  5503.84  5210.44       8258.84
`
	both := []string{accountsRequest, preferencesRequest}
	for _, tc := range []struct {
		args     []string
		stdout   string
		requests []string
	}{
		{[]string{"accounts", "-account", "3"}, contributory, both},
		{[]string{"accounts", "-account", "1783"}, contributory, both},
		{[]string{"accounts", "-account", "...783"}, contributory, both},
		{[]string{"accounts", "-account", "4022-1783"}, contributory, both},
		{[]string{"accounts", "-account", " 4022 1783 "}, contributory, both},
		{[]string{"accounts", "-account", "contributory"}, contributory, both},
		{[]string{"balances", "-account", "61803398"}, margin, both},
		{[]string{"balances", "-account", "...3398"}, margin, both},
		{[]string{"balances", "-account", "  options & MARGIN "}, margin, both},
		{[]string{"balances", "-json", "-account", "3398"}, "", []string{accountsRequest}},
		{[]string{"balances", "-json", "-account", "Options & Margin"}, "", both},
	} {
		stdout, _, requests, err := runLookupOn(t, f, tc.args...)
		if err != nil {
			t.Errorf("%q: %v", tc.args, err)
			continue
		}
		if tc.stdout != "" && stdout != tc.stdout {
			t.Errorf("%q printed\n%s\nwant\n%s", tc.args, stdout, tc.stdout)
		}
		if !equal(requests, tc.requests) {
			t.Errorf("%q made the requests %q, want %q", tc.args, requests, tc.requests)
		}
	}
}

// An -account that names no account, or several, is an error that lists
// the accounts by their masked numbers and nicknames, and says what to
// give instead. Nothing is printed. When the preferences were not read,
// the error reads them, to name the accounts.
func TestLookupsPickNoneOrSeveral(t *testing.T) {
	shares := "the login shares ...0135 (Individual), ...3398 (Options & Margin), ...1783 (Contributory); " +
		"to share another, run `schwabtx login` and tick it"
	f := setupFixtures(t)
	for _, tc := range []struct {
		args     []string
		err      string
		requests int
	}{
		{[]string{"accounts", "-account", "9999"}, `no account matches -account "9999": ` + shares, 2},
		{[]string{"accounts", "-account", "61803399"}, `no account matches -account "...3399": ` + shares, 2},
		{[]string{"balances", "-account", "Roth"}, `no account matches -account "Roth": ` + shares, 2},
		{[]string{"balances", "-json", "-account", "9999"}, `no account matches -account "9999": ` + shares, 2},
		{[]string{"balances", "-json", "-account", "..."}, `no account matches -account "...": ` + shares, 2},
	} {
		stdout, _, requests, err := runLookupOn(t, f, tc.args...)
		if err == nil || err.Error() != tc.err || stdout != "" || len(requests) != tc.requests {
			t.Errorf("%q: %v, printed %q, after the requests %q; want the error %q after %d requests", tc.args, err, stdout, requests, tc.err, tc.requests)
		}
	}

	// Two accounts end in 2222.
	f2, _ := setup(t)
	f2.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f2.addAccount("33332222", 2000, 0, testNow, time.Hour)
	doLogin(t)
	several := `-account "2222" matches 2 accounts, ...2222 (Account 1), ...2222 (Account 2); `
	for _, tc := range []struct {
		args       []string
		preference string // the user preferences, or "" for the fake's own
		err        string
	}{
		{[]string{"accounts", "-account", "2222"}, "", several + "give its nickname instead"},
		{[]string{"accounts", "-json", "-account", "2222"}, "", several + "give its nickname instead"},
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", "Brokerage", "33332222", "brokerage"),
			`-account "2222" matches 2 accounts, ...2222 (Brokerage), ...2222 (brokerage); ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		{
			[]string{"accounts", "-account", "BROKERAGE"},
			preferenceBody("11112222", "Brokerage", "33332222", "brokerage"),
			`-account "BROKERAGE" matches 2 accounts, ...2222 (Brokerage), ...2222 (brokerage); give the last digits of its number instead`,
		},
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", "Brokerage"),
			`-account "2222" matches 2 accounts, ...2222 (Brokerage), ...2222; ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
	} {
		f2.SetUserPreference(tc.preference)
		stdout, _, err := runTx(t, tc.args...)
		if err == nil || err.Error() != tc.err || stdout != "" {
			t.Errorf("%q: %v, printed %q; want %q", tc.args, err, stdout, tc.err)
		}
	}
	// More digits tell them apart.
	f2.SetUserPreference("")
	for _, account := range []string{"12222", "1111-2222", "11112222"} {
		stdout, _, err := runTx(t, "accounts", "-account", account)
		if err != nil || !strings.Contains(stdout, "Account 1") || strings.Contains(stdout, "Account 2") {
			t.Errorf("-account %s: %v, printed\n%s", account, err, stdout)
		}
	}
}

// -json prints the picked accounts as Schwab sent them, re-indented, full
// numbers included: for accounts and balances alike, the accounts without
// their positions, which they did not ask for. It makes no request for the
// preferences, whose nicknames are not in Schwab's records, and prints
// nothing on stderr.
func TestLookupsJSON(t *testing.T) {
	f := setupFixtures(t)
	accounts, stderr, requests, err := runLookupOn(t, f, "accounts", "-json")
	if err != nil || stderr != "" || !equal(requests, []string{accountsRequest}) {
		t.Fatalf("accounts -json: %v, warned %q, after the requests %q", err, stderr, requests)
	}
	balances, _, err := runTx(t, "balances", "-json")
	if err != nil || balances != accounts {
		t.Errorf("balances -json: %v, printed\n%s\nwhere accounts -json printed\n%s", err, balances, accounts)
	}
	var list []struct {
		SecuritiesAccount map[string]json.RawMessage `json:"securitiesAccount"`
	}
	if err := json.Unmarshal([]byte(accounts), &list); err != nil || len(list) != 3 {
		t.Fatalf("accounts -json printed %d accounts, %v:\n%s", len(list), err, accounts)
	}
	for i, a := range list {
		if _, ok := a.SecuritiesAccount["positions"]; ok {
			t.Errorf("account %d has its positions", i)
		}
		if n := string(a.SecuritiesAccount["accountNumber"]); n != `"`+schwabtest.FixtureAccounts[i].Number+`"` {
			t.Errorf("account %d's number is %s", i, n)
		}
	}

	// The fixture's third account, as the fake sends it: 17420.0 keeps its
	// spelling.
	var fixture []json.RawMessage
	if err := json.Unmarshal(schwabtest.Fixture(t, "accounts.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	want.WriteString("[\n  ")
	if err := json.Indent(&want, fixture[2], "  ", "  "); err != nil {
		t.Fatal(err)
	}
	want.WriteString("\n]\n")
	for _, account := range []string{"1783", "Contributory"} {
		stdout, stderr, err := runTx(t, "balances", "-json", "-account", account)
		if err != nil || stdout != want.String() || stderr != "" {
			t.Errorf("-json -account %s: %v, warned %q, printed\n%s\nwant\n%s", account, err, stderr, stdout, want.String())
		}
	}
	if !strings.Contains(want.String(), `"bondValue": 17420.0,`) {
		t.Errorf("the fixture no longer holds 17420.0")
	}
}

// account decodes js, an account as GET /accounts sends one.
func account(t *testing.T, js string) schwab.Account {
	t.Helper()
	var a schwab.Account
	if err := json.Unmarshal([]byte(js), &a); err != nil {
		t.Fatal(err)
	}
	return a
}

// marginAccount returns a margin account with current balances cur, a JSON
// object's members, and an aggregated balance agg, or none for "".
func marginAccount(cur, agg string) string {
	js := `{"securitiesAccount":{"type":"MARGIN","accountNumber":"12345678","roundTrips":0,"isDayTrader":false,` +
		`"isClosingOnlyRestricted":false,"pfcbFlag":false,"initialBalances":{},"currentBalances":{` + cur + `},"projectedBalances":{}}`
	if agg != "" {
		js += `,"aggregatedBalance":{` + agg + `}`
	}
	return js + "}"
}

// The value, cash and buying power of an account, from the current
// balances, and none for a figure that Schwab did not send or that could
// not be read.
func TestBalanceOf(t *testing.T) {
	// The market values of holdings of 20000 long and 12000 short, worth
	// 8000 together.
	const held = `"longMarketValue":20000,"shortMarketValue":-12000,"longOptionMarketValue":0,"shortOptionMarketValue":0,` +
		`"mutualFundValue":0,"bondValue":0`
	for _, tc := range []struct {
		name, account string
		want          string // the value, cash and buying power, and why one is none, if not for a problem
	}{
		{
			"current value",
			marginAccount(`"liquidationValue":5503.84,"cashBalance":5210.44,"buyingPower":8258.84`, `"currentLiquidationValue":5503.85`),
			"5503.84 5210.44 8258.84",
		},
		{
			"aggregated value",
			marginAccount(`"cashBalance":1,"buyingPower":2`, `"currentLiquidationValue":5503.85`),
			"5503.85 1.00 2.00",
		},
		{"no value", marginAccount(`"cashBalance":1,"buyingPower":2`, ""), "- 1.00 2.00; unsent value"},
		{
			"the aggregated liquidationValue alone",
			marginAccount(`"cashBalance":1,"buyingPower":2`, `"liquidationValue":5503.85`),
			"- 1.00 2.00; unsent value",
		},
		{
			"the initial value alone",
			strings.Replace(marginAccount(`"cashBalance":1,"buyingPower":2`, ""),
				`"initialBalances":{}`, `"initialBalances":{"liquidationValue":5500.63,"accountValue":5500.63}`, 1),
			"- 1.00 2.00; unsent value",
		},
		{
			"unreadable current value",
			marginAccount(`"liquidationValue":"5503.84","cashBalance":1,"buyingPower":2`, `"currentLiquidationValue":5503.85`),
			"5503.85 1.00 2.00",
		},
		{
			"unreadable values",
			marginAccount(`"liquidationValue":"5503.84","cashBalance":1,"buyingPower":2`, `"currentLiquidationValue":"5503.85"`),
			"- 1.00 2.00",
		},

		{
			"a cash account",
			`{"securitiesAccount":{"type":"CASH","accountNumber":"1","currentBalances":{"cashBalance":1532.17,"cashAvailableForTrading":1500}}}`,
			"- 1532.17 1500.00; unsent value",
		},
		{"cash 0", marginAccount(`"cashBalance":0,"buyingPower":0`, ""), "- 0.00 0.00; unsent value"},
		{
			"cash in the margin and short balances",
			marginAccount(`"cashBalance":0,"marginBalance":-12345.67,"shortBalance":12846.12,"buyingPower":1`, ""),
			"- 500.45 1.00; unsent value",
		},
		{"a debit", marginAccount(`"cashBalance":0,"marginBalance":-100,"buyingPower":1`, ""), "- -100.00 1.00; unsent value"},
		{"no cashBalance", marginAccount(`"marginBalance":0,"shortBalance":0,"buyingPower":1`, ""), "- - 1.00; unsent value, cash"},
		{
			"cashBalance beside a margin balance",
			marginAccount(`"cashBalance":5210.44,"marginBalance":-5,"buyingPower":1`, ""),
			"- 5210.44 1.00; unsent value",
		},
		{"unreadable cashBalance", marginAccount(`"cashBalance":"1532.17","buyingPower":1`, ""), "- - 1.00; unsent value"},
		{
			"unreadable shortBalance",
			marginAccount(`"cashBalance":0,"marginBalance":-12345.67,"shortBalance":"12846.12","buyingPower":1`, ""),
			"- - 1.00; unsent value",
		},
		{
			"unreadable marginBalance",
			marginAccount(`"cashBalance":0,"marginBalance":"-12345.67","shortBalance":12846.12,"buyingPower":1`, ""),
			"- - 1.00; unsent value",
		},
		{
			"unreadable marginBalance beside cash",
			marginAccount(`"cashBalance":7,"marginBalance":"-12345.67","buyingPower":1`, ""),
			"- 7.00 1.00; unsent value",
		},
		{
			"unreadable shortBalance without a margin balance",
			marginAccount(`"cashBalance":0,"shortBalance":"5","buyingPower":1`, ""),
			"- 0.00 1.00; unsent value",
		},

		{
			"cash that the value leaves",
			marginAccount(`"liquidationValue":8500.45,"cashBalance":0,"marginBalance":-12345.67,"shortBalance":12846.12,"buyingPower":1,`+held, ""),
			"8500.45 500.45 1.00",
		},
		{
			"cash a cent from what the value leaves",
			marginAccount(`"liquidationValue":8500.46,"cashBalance":500.45,"buyingPower":1,`+held, ""),
			"8500.46 500.45 1.00",
		},
		{
			"savings beside the cash",
			marginAccount(`"liquidationValue":11000.45,"cashBalance":500.45,"savings":2500,"moneyMarketFund":0,"buyingPower":1,`+held, ""),
			"11000.45 - 1.00; cash differs",
		},
		{
			"short proceeds beside the cash",
			marginAccount(`"liquidationValue":1200,"cashBalance":1000,"shortBalance":5000,"marginBalance":0,"buyingPower":1,`+
				`"longMarketValue":0,"shortMarketValue":-4800,"longOptionMarketValue":0,"shortOptionMarketValue":0,"mutualFundValue":0,"bondValue":0`, ""),
			"1200.00 - 1.00; cash differs",
		},
		{
			"a market value missing",
			marginAccount(`"liquidationValue":11000.45,"cashBalance":500.45,"savings":2500,"buyingPower":1,`+
				strings.Replace(held, `"bondValue":0`, `"equity":0`, 1), ""),
			"11000.45 500.45 1.00",
		},
		{
			"a market value unreadable",
			marginAccount(`"liquidationValue":11000.45,"cashBalance":500.45,"savings":2500,"buyingPower":1,`+
				strings.Replace(held, `"bondValue":0`, `"bondValue":"0"`, 1), ""),
			"11000.45 500.45 1.00",
		},

		{"no buying power", marginAccount(`"cashBalance":1`, ""), "- 1.00 -; unsent value, buying power"},
		{"unreadable buying power", marginAccount(`"cashBalance":1,"buyingPower":"2"`, ""), "- 1.00 -; unsent value"},
		{
			"a cash account's buyingPower",
			`{"securitiesAccount":{"type":"CASH","accountNumber":"1","currentBalances":{"cashBalance":1,"buyingPower":2}}}`,
			"- 1.00 -; unsent value, buying power",
		},
		{
			"another type",
			strings.Replace(marginAccount(`"cashBalance":1,"buyingPower":2,"cashAvailableForTrading":3`, ""), `"MARGIN"`, `"FUTURES"`, 1),
			"- 1.00 -; unsent value",
		},

		{
			"current balances sent twice",
			strings.Replace(marginAccount(`"liquidationValue":1,"cashBalance":1,"buyingPower":1`, `"currentLiquidationValue":5503.85`),
				`"projectedBalances"`, `"currentBalances":{"liquidationValue":2},"projectedBalances"`, 1),
			"5503.85 - -",
		},
		{"not an object", `null`, "- - -"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := account(t, tc.account)
			b := balanceOf(a)
			got := fmt.Sprintf("%s %s %s", b.value, b.cash, b.buyingPower)
			var unsent []string
			for _, f := range []struct {
				name string
				no   bool
			}{{"value", b.noValue}, {"cash", b.noCash}, {"buying power", b.noBuyingPower}} {
				if f.no {
					unsent = append(unsent, f.name)
				}
			}
			if len(unsent) > 0 {
				got += "; unsent " + strings.Join(unsent, ", ")
			}
			if b.cashDiffers {
				got += "; cash differs"
			}
			if got != tc.want {
				t.Errorf("got %q, want %q (problems %v)", got, tc.want, a.Problems)
			}
		})
	}
}

// Money is rounded to cents as Schwab wrote it, half away from zero, not
// as the double nearest to what it wrote rounds.
func TestScaled(t *testing.T) {
	for _, tc := range []struct {
		x      float64
		places int
		want   int64
		ok     bool
	}{
		{1.005, 2, 101, true},
		{0.285, 2, 29, true},
		{2.675, 2, 268, true},
		{12.345, 2, 1235, true},
		{-0.125, 2, -13, true},
		{-1.005, 2, -101, true},
		{1.004999, 2, 100, true},
		{81.29223999999999, 2, 8129, true},
		{-236.999999999999, 2, -23700, true},
		{0.30000000000000004, 6, 300000, true},
		{2.1788, 6, 2178800, true},
		{0.0000005, 6, 1, true},
		{-0.004, 2, 0, true},
		{0, 2, 0, true},
		{5e-324, 2, 0, true},
		{999999999999.99, 2, 99999999999999, true},
		{999999999999.9999, 6, 999999999999999900, true},
		{1e12, 2, 0, false},
		{-1e12, 2, 0, false},
		{1e300, 2, 0, false},
		{math.Inf(-1), 2, 0, false},
		{math.NaN(), 2, 0, false},
	} {
		if got, ok := scaled(tc.x, tc.places); got != tc.want || ok != tc.ok {
			t.Errorf("scaled(%v, %d) = %d, %t; want %d, %t", tc.x, tc.places, got, ok, tc.want, tc.ok)
		}
	}
}

// Amounts print with two decimals, a leading - when negative and no other
// sign, and none prints as -.
func TestMoneyString(t *testing.T) {
	for _, tc := range []struct {
		m    money
		want string
	}{
		{amount(-310), "-310.00"},
		{amount(-0.004), "0.00"},
		{amount(math.Copysign(0, -1)), "0.00"},
		{amount(0.05), "0.05"},
		{amount(-0.5), "-0.50"},
		{amount(12501.52), "12501.52"},
		{amount(1e13), "-"},
		{moneyOf(nil), "-"},
		{amount(1).plus(moneyOf(nil)), "-"},
		{moneyOf(nil).minus(amount(1)), "-"},
		{amount(5503.84).minus(amount(-310)), "5813.84"},
	} {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("%+v printed %q, want %q", tc.m, got, tc.want)
		}
	}
	for _, tc := range []struct {
		n            int64
		places, keep int
		want         string
	}{
		{0, 6, 0, "0"},
		{-2000000, 6, 0, "-2"},
		{2178800, 6, 0, "2.1788"},
		{81500000, 6, 2, "81.50"},
		{251307500, 6, 2, "251.3075"},
		{1, 6, 2, "0.000001"},
		{-1, 2, 2, "-0.01"},
	} {
		if got := formatScaled(tc.n, tc.places, tc.keep); got != tc.want {
			t.Errorf("formatScaled(%d, %d, %d) = %q, want %q", tc.n, tc.places, tc.keep, got, tc.want)
		}
	}
}

// Text that Schwab or the user wrote cannot break a table or the terminal:
// control and format characters, and line and paragraph separators, print
// as ?, and a nickname has its white space collapsed.
func TestCells(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Joint\x1b[31m\tBrokerage", "Joint?[31m?Brokerage"},
		{"Roth \u202eARI\u202c", "Roth ?ARI?"},
		{"a\u2028b\u2029c\u200bd\u2066e", "a?b?c?d?e"},
		{"Épargne", "Épargne"},
		{"\xff", "\ufffd"},
	} {
		if got := plainCell(tc.in); got != tc.want {
			t.Errorf("plainCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := nameCell("Joint\x1b[31m\t Brokerage\u2028Account"); got != "Joint?[31m Brokerage Account" {
		t.Errorf("nameCell = %q", got)
	}
	if got := nameCell(strings.Repeat("Ab", 20)); got != strings.Repeat("Ab", 13)+"A..." {
		t.Errorf("a long nickname: %q", got)
	}
	if got, want := accountCell("12345678", false), "...5678"; got != want {
		t.Errorf("accountCell = %q, want %q", got, want)
	}
	if got, want := accountCell("12345678", true), "12345678"; got != want {
		t.Errorf("accountCell shown = %q, want %q", got, want)
	}
	if got, want := accountCell("", true), "?"; got != want {
		t.Errorf("accountCell of no number = %q, want %q", got, want)
	}
}

// Each column is as wide as its widest cell in runes, with numbers on the
// right and text on the left, and no line ends in spaces.
func TestWriteTable(t *testing.T) {
	var b bytes.Buffer
	cols := []column{{head: "NAME"}, {head: "N", right: true}, {head: "NOTE"}}
	if err := writeTable(&b, cols, [][]string{{"Épargne", "1.00", ""}, {"a", "-12345.67", "x"}, {"", "", ""}}); err != nil {
		t.Fatal(err)
	}
	want := "NAME             N  NOTE\n" +
		"Épargne       1.00\n" +
		"a        -12345.67  x\n" +
		"\n"
	if b.String() != want {
		t.Errorf("wrote\n%q\nwant\n%q", b.String(), want)
	}
}

// A value that cannot be read is named in a warning, and the value falls
// back to the aggregated balance's.
func TestLookupsWarnOfUnreadableBalances(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.setBalancesJSON(`{"liquidationValue":"12345.60","cashBalance":100.0}`)
	doLogin(t)
	stdout, stderr, err := runTx(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if want := "warning: 1 account differed from what Schwab has sent before (securitiesAccount.currentBalances.liquidationValue: " +
		"Schwab sent a string where a number was expected); the table may not show those fields as Schwab sent them, " +
		"and -json prints Schwab's own JSON\n"; stderr != want {
		t.Errorf("warned %q\nwant %q", stderr, want)
	}
	if want := "ACCOUNT  NAME       TYPE       VALUE\n...2222  Account 1  MARGIN  12345.60\n"; stdout != want {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want)
	}
}

// An account whose number cannot be read is ? in the table, nameless, and
// no digits pick it.
func TestLookupsUnreadableAccountNumber(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.setNumberJSON(`11112222`)
	doLogin(t)
	stdout, stderr, err := runTx(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ACCOUNT  NAME  TYPE       VALUE\n?              MARGIN  12345.60\n"; stdout != want {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want)
	}
	if !strings.Contains(stderr, "(securitiesAccount.accountNumber: Schwab sent a number where a string was expected)") {
		t.Errorf("warned %q", stderr)
	}
	_, _, err = runTx(t, "accounts", "-account", "2222")
	if want := `no account matches -account "2222": the login shares ?; to share another, run ` + "`schwabtx login` and tick it"; err == nil || err.Error() != want {
		t.Errorf("-account 2222: %v, want %q", err, want)
	}
}

// Only a problem that can cost an account its nickname is warned of, and
// a nickname that cannot be read leaves the name empty.
func TestLookupsWarnOfUnreadablePreferences(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	doLogin(t)
	good := preferenceBody("11112222", "Brokerage")
	named := "ACCOUNT  NAME       TYPE       VALUE\n...2222  Brokerage  MARGIN  12345.60\n"
	nameless := "ACCOUNT  NAME  TYPE       VALUE\n...2222        MARGIN  12345.60\n"
	for _, tc := range []struct {
		name, preference, stderr, stdout string
	}{
		{
			"a nickname that is a number", strings.Replace(good, `"Brokerage"`, `7`, 1),
			"warning: the user preferences differed from what Schwab has sent before " +
				"(accounts.nickName: Schwab sent a number where a string was expected); a nickname may be missing\n", nameless,
		},
		{
			"no account number", strings.Replace(good, `"accountNumber":"11112222",`, ``, 1),
			"warning: the user preferences differed from what Schwab has sent before " +
				"(accounts.accountNumber: missing, though Schwab sends it on every record of its kind); a nickname may be missing\n", nameless,
		},
		{
			"accounts that are not a list", `{"accounts":{},"streamerInfo":[],"offers":[]}`,
			"warning: the user preferences differed from what Schwab has sent before " +
				"(accounts: Schwab sent an object where an array was expected); a nickname may be missing\n", nameless,
		},
		{"the streamer's details", strings.Replace(good, `"schwabClientChannel":"N9"`, `"schwabClientChannel":9`, 1), "", named},
		{"an account's flag", strings.Replace(good, `"primaryAccount":false`, `"primaryAccount":"no"`, 1), "", named},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.SetUserPreference(tc.preference)
			stdout, stderr, err := runTx(t, "accounts")
			if err != nil {
				t.Fatal(err)
			}
			if stderr != tc.stderr {
				t.Errorf("warned %q\nwant %q", stderr, tc.stderr)
			}
			if stdout != tc.stdout {
				t.Errorf("printed\n%s\nwant\n%s", stdout, tc.stdout)
			}
		})
	}
}

// Each account takes the nickname of the first entry of the preferences
// with its number. An entry for an account that GET /accounts did not list
// names nothing, and an account without an entry has no nickname.
func TestLookupsNameAccounts(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.addAccount("33334444", 2000, 0, testNow, time.Hour)
	f.SetUserPreference(preferenceBody("55556666", "Unshared", "11112222", "First", "11112222", "Second"))
	doLogin(t)
	stdout, stderr, err := runTx(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	want := `ACCOUNT  NAME   TYPE       VALUE
...2222  First  MARGIN  12345.60
...4444         MARGIN  12345.60
TOTAL                   24691.20
`
	if stdout != want || stderr != "" {
		t.Errorf("printed\n%s\nwarned %q; want\n%s", stdout, stderr, want)
	}
	if _, _, err := runTx(t, "accounts", "-account", "unshared"); err == nil || !strings.HasPrefix(err.Error(), `no account matches -account "unshared"`) {
		t.Errorf("-account unshared: %v", err)
	}
}

// A nickname with control characters in it prints plain: no escape reaches
// the terminal, and a tab is a space.
func TestLookupsPrintNicknamesPlain(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.SetUserPreference(preferenceBody("11112222", "Joint\x1b[31m\tBrokerage"))
	doLogin(t)
	stdout, _, err := runTx(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ACCOUNT  NAME                  TYPE       VALUE\n...2222  Joint?[31m Brokerage  MARGIN  12345.60\n"; stdout != want {
		t.Errorf("printed\n%q\nwant\n%q", stdout, want)
	}
}

// When the preferences cannot be read, the table leaves the nicknames out
// and says why, but a nickname given to -account is an error, and so is a
// cancel while they are read.
func TestLookupsWithoutPreferences(t *testing.T) {
	f := setupFixtures(t)
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.Unavailable)
	stdout, stderr, err := runTx(t, "balances")
	if err != nil {
		t.Fatal(err)
	}
	if want := "warning: the table leaves out the accounts' nicknames: fetching the user preferences: HTTP 503 from Schwab after 4 tries"; !strings.HasPrefix(stderr, want) ||
		!strings.HasSuffix(stderr, "\nwarning: Schwab sent no current value or cash for ...1783\n") {
		t.Errorf("warned %q", stderr)
	}
	if want := `ACCOUNT  NAME  TYPE      VALUE     CASH  BUYING POWER
...0135        CASH    6997.68  1532.17       1532.17
...3398        MARGIN  5503.84  5210.44       8258.84
...1783        MARGIN        -        -       8710.00
TOTAL                        -        -
`; stdout != want {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want)
	}

	_, _, err = runTx(t, "balances", "-account", "Individual")
	if want := `fetching the user preferences, to find the account named "Individual": HTTP 503 from Schwab after 4 tries`; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("-account Individual: %v, want %q", err, want)
	}
	_, _, err = runTx(t, "balances", "-json", "-account", "9999")
	if want := `no account matches -account "9999": the login shares ...0135, ...3398, ...1783; ` +
		"to share another, run `schwabtx login` and tick it"; err == nil || err.Error() != want {
		t.Errorf("-json -account 9999: %v, want %q", err, want)
	}

	// A cancel, as by Ctrl-C, while the preferences are read ends the run.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.Hang)
	f.OnAPI(func(r *http.Request) {
		if schwabtest.Route(r.Pattern) == schwabtest.UserPreferenceRoute {
			cancel()
		}
	})
	var o, e bytes.Buffer
	err = run(ctx, []string{"balances"}, strings.NewReader(""), &o, &e)
	if !errors.Is(err, context.Canceled) || o.Len() != 0 || strings.Contains(e.String(), "leaves out") {
		t.Errorf("canceled: %v, printed %q, warned %q", err, o.String(), e.String())
	}
}

// The lookups fail as the other commands do: without accounts, without a
// login, with a login that Schwab ended, and with Schwab's errors, which
// say what to do. A login about to lapse is warned of.
func TestLookupsFail(t *testing.T) {
	noAccounts := "Schwab returned no accounts; run `schwabtx login` again and tick the accounts to share"
	t.Run("no accounts", func(t *testing.T) {
		setup(t)
		doLogin(t)
		for _, args := range [][]string{{"accounts"}, {"balances"}, {"balances", "-json"}} {
			if stdout, _, err := runTx(t, args...); err == nil || err.Error() != noAccounts || stdout != "" {
				t.Errorf("%q: %v, printed %q; want %q", args, err, stdout, noAccounts)
			}
		}
	})
	t.Run("no login", func(t *testing.T) {
		f, _ := setup(t)
		f.LoadFixtures()
		if _, _, err := runTx(t, "accounts"); err == nil || !strings.Contains(err.Error(), "run `schwabtx login` first") {
			t.Errorf("got %v, want the missing login", err)
		}
		if n := f.APICalls(); n != 0 {
			t.Errorf("made %d API calls without a login", n)
		}
	})
	t.Run("ended login", func(t *testing.T) {
		f := setupFixtures(t)
		f.EndLogin()
		timeNow = func() time.Time { return testNow.Add(8 * 24 * time.Hour) }
		for _, args := range [][]string{{"accounts"}, {"balances", "-json"}} {
			stdout, _, requests, err := runLookupOn(t, f, args...)
			if !errors.Is(err, schwab.ErrLoginExpired) || !strings.HasPrefix(err.Error(), "fetching accounts: ") ||
				!strings.Contains(err.Error(), "run `schwabtx login` again") || stdout != "" || len(requests) != 0 {
				t.Errorf("%q: %v, printed %q, after the requests %q", args, err, stdout, requests)
			}
		}
	})
	t.Run("login about to lapse", func(t *testing.T) {
		setupFixtures(t)
		timeNow = func() time.Time { return testNow.Add(6*24*time.Hour + 12*time.Hour) }
		stdout, stderr, err := runTx(t, "accounts")
		if err != nil || stdout != accountsOfThree || !strings.Contains(stderr, "expires in about 12 hours") {
			t.Errorf("%v, warned %q, printed\n%s", err, stderr, stdout)
		}
	})
	for _, tc := range []struct {
		name   string
		answer http.HandlerFunc
		want   []string
	}{
		{"an app not yet Ready For Use", schwabtest.Whitelabel, []string{"fetching accounts: ", "Whitelabel Error Page", `until the app says "Ready For Use"`}},
		{"rate limited", schwabtest.TooMany, []string{"fetching accounts: ", "HTTP 429 from Schwab after 4 tries", "wait a minute"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupFixtures(t)
			f.Override(schwabtest.AccountsRoute, tc.answer)
			stdout, _, err := runTx(t, "balances")
			for _, want := range tc.want {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("got %v, want %q in it", err, want)
				}
			}
			if stdout != "" {
				t.Errorf("printed %q", stdout)
			}
		})
	}
}

// Bad arguments fail before any request, and -h lists the flags.
func TestLookupsArguments(t *testing.T) {
	f := setupFixtures(t)
	before := f.AllRequests()
	for _, args := range [][]string{
		{"balances", "extra"},
		{"balances", "-n", "5"},
		{"accounts", "-account"},
		{"accounts", "-json=maybe"},
	} {
		if stdout, _, err := runTx(t, args...); err == nil || stdout != "" {
			t.Errorf("%q: %v, printed %q; want an error", args, err, stdout)
		}
	}
	_, stderr, err := runTx(t, "balances", "-h")
	if !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h: %v", err)
	}
	for _, want := range []string{"schwabtx balances", "-account", "-json", "-show-accounts"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("-h printed %q, without %q", stderr, want)
		}
	}
	if n := f.AllRequests() - before; n != 0 {
		t.Errorf("bad arguments made %d requests", n)
	}
}

// The usage names every command, and where to find each one's flags.
func TestUsage(t *testing.T) {
	stdout, _, err := runTx(t, "help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"schwabtx login ", "schwabtx [flags] ", "schwabtx accounts ", "schwabtx balances ",
		"schwabtx snapshot ", `"schwabtx <command> -h"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the usage has no %q:\n%s", want, stdout)
		}
	}
	_, stderr, err := runTx(t, "acounts")
	if err == nil || err.Error() != `unknown command "acounts"` || !strings.Contains(stderr, "schwabtx accounts ") {
		t.Errorf("a command misspelled: %v, printed %q", err, stderr)
	}
}
