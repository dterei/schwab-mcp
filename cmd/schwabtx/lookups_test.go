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
	"strconv"
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
	balancesOfThreeShown = `ACCOUNT   NAME              TYPE      VALUE     CASH  BUYING POWER
29470135  Individual        CASH    6997.68  1532.17       1532.17
61803398  Options & Margin  MARGIN  5503.84  5210.44       8258.84
40221783  Contributory      MARGIN        -        -       8710.00
TOTAL                                     -        -
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
		{[]string{"balances", "-show-accounts"}, balancesOfThreeShown, "warning: Schwab sent no current value or cash for ...1783 (Contributory)\n"},
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
	f := setupFixtures(t)
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
		{"positions"},
		{"positions", "-account", "61803399"},
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

	// An error shows -account's value with more than 4 digits in a row as
	// ... and the last 4, as the tables mask a number, whatever separates
	// them and whatever words surround them.
	for _, tc := range []struct{ account, shown string }{
		{"03399", "...3399"},
		{"1803399", "...3399"},
		{"...61803399", "...3399"},
		{"6180.3399", "...3399"},
		{"6180_3399", "...3399"},
		{"6180−3399", "...3399"},
		{"#61803399", "#...3399"},
		{"Account 2947-0136", "Account ...0136"},
		{"2947 - 0136 Joint", "...0136 Joint"},
		{"Joint ...29470136", "Joint ...0136"},
		{"Roth 2019", "Roth 2019"},
		{"Joint 12-34", "Joint 12-34"},
		{"401k 2019", "401k 2019"},
	} {
		_, _, err := runTx(t, "accounts", "-account", tc.account)
		if want := fmt.Sprintf("no account matches -account %q: ", tc.shown); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("-account %q: %v, want %q", tc.account, err, want)
		}
	}
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.Unavailable)
	_, _, err := runTx(t, "positions", "-account", "Account 2947-0135")
	if want := `fetching the user preferences, to find the account named "Account ...0135": `; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("-account %q without the preferences: %v, want %q", "Account 2947-0135", err, want)
	}
}

// -account picks one account from the answer for all of them: by the last
// digits of its number, written as the tables, Schwab or schwab.com write
// them, with dashes or spaces of any kind among them, or by its whole
// nickname, whatever its case and the spaces around it. A nickname costs
// the preferences' request in -json mode too, and digits cost no request.
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
		{[]string{"accounts", "-account", "4022\u20131783"}, contributory, both},
		{[]string{"accounts", "-account", "4022\u00a01783"}, contributory, both},
		{[]string{"accounts", "-account", "4022\t1783"}, contributory, both},
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
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", " ", "33332222", "Brokerage"),
			`-account "2222" matches 2 accounts, ...2222, ...2222 (Brokerage); ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		// Nicknames that differ only in their white space are the same name,
		// as the tables show them.
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", "Joint Brokerage", "33332222", "joint  brokerage "),
			`-account "2222" matches 2 accounts, ...2222 (Joint Brokerage), ...2222 (joint brokerage); ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		{
			[]string{"accounts", "-account", "JOINT BROKERAGE"},
			preferenceBody("11112222", "Joint Brokerage", "33332222", "joint  brokerage "),
			`-account "JOINT BROKERAGE" matches 2 accounts, ...2222 (Joint Brokerage), ...2222 (joint brokerage); ` +
				"give the last digits of its number instead",
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

// A nickname picks its account by the name that the tables and the errors
// show: whatever its case, with no white space at its ends, and with a run
// of white space within it, such as two spaces, a tab or a no-break space,
// as one space.
func TestLookupsPickByTheNameShown(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.addAccount("33334444", 2000, 0, testNow, time.Hour)
	f.SetUserPreference(preferenceBody("11112222", " Roth  IRA ", "33334444", "Joint Brokerage\tAccount"))
	doLogin(t)
	stdout, _, err := runTx(t, "accounts")
	if want := `ACCOUNT  NAME                     TYPE       VALUE
...2222  Roth IRA                 MARGIN  12345.60
...4444  Joint Brokerage Account  MARGIN  12345.60
TOTAL                                     24691.20
`; err != nil || stdout != want {
		t.Errorf("%v, printed\n%s\nwant\n%s", err, stdout, want)
	}
	for _, tc := range []struct{ account, want string }{
		{"Roth IRA", "...2222  Roth IRA"},
		{"roth\tira", "...2222  Roth IRA"},
		{"Joint Brokerage Account", "...4444  Joint Brokerage Account"},
		{"JOINT  BROKERAGE ACCOUNT", "...4444  Joint Brokerage Account"},
	} {
		stdout, _, err := runTx(t, "accounts", "-account", tc.account)
		if lines := strings.Split(stdout, "\n"); err != nil || len(lines) != 3 || !strings.HasPrefix(lines[1], tc.want+" ") {
			t.Errorf("-account %q: %v, printed\n%s", tc.account, err, stdout)
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
		{
			"a negative cashBalance beside a margin balance",
			marginAccount(`"cashBalance":-50,"marginBalance":-5,"shortBalance":20,"buyingPower":1`, ""),
			"- -50.00 1.00; unsent value",
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
			"cash a cent above what the value leaves",
			marginAccount(`"liquidationValue":8500.44,"cashBalance":500.45,"buyingPower":1,`+held, ""),
			"8500.44 500.45 1.00",
		},
		{
			"cash two cents below what the value leaves",
			marginAccount(`"liquidationValue":8500.45,"cashBalance":500.43,"buyingPower":1,`+held, ""),
			"8500.45 - 1.00; cash differs",
		},
		{
			"cash two cents above what the value leaves",
			marginAccount(`"liquidationValue":8500.45,"cashBalance":500.47,"buyingPower":1,`+held, ""),
			"8500.45 - 1.00; cash differs",
		},
		{
			"long options and bonds",
			marginAccount(`"liquidationValue":9000.45,"cashBalance":500.45,"buyingPower":1,`+
				strings.NewReplacer(`"longOptionMarketValue":0`, `"longOptionMarketValue":300`, `"bondValue":0`, `"bondValue":200`).Replace(held), ""),
			"9000.45 500.45 1.00",
		},
		{
			// A real margin account of 2025, jwplatta/schwab_rb's fixture,
			// which holds long and short options and nothing else.
			"options long and short",
			marginAccount(`"liquidationValue":50946.39,"cashBalance":51420.89,"longMarketValue":0,"shortMarketValue":0,`+
				`"longOptionMarketValue":436,"shortOptionMarketValue":-910.5,"mutualFundValue":0,"bondValue":0,"buyingPower":88853.04`, ""),
			"50946.39 51420.89 88853.04",
		},
		{
			"no cashBalance beside every market value",
			marginAccount(`"liquidationValue":8500.45,"buyingPower":1,`+held, ""),
			"8500.45 - 1.00; unsent cash",
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
// as ?, and a nickname or a description has its white space collapsed.
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
	if got := textCell("Joint\x1b[31m\t Brokerage\u2028Account", 30); got != "Joint?[31m Brokerage Account" {
		t.Errorf("textCell = %q", got)
	}
	if got := textCell(strings.Repeat("Ab", 20), 30); got != strings.Repeat("Ab", 13)+"A..." {
		t.Errorf("a long nickname: %q", got)
	}
	// Text whose end says the most keeps its end, from a word's start when
	// it can.
	for _, tc := range []struct{ in, want string }{
		{"NVIDIA CORP 02/28/2025 $115 Put", "NVIDIA CORP 02/28/2025 $115 Put"},
		{"XYZ\x1b[2J 06/19/2026 45.00 P", "XYZ?[2J 06/19/2026 45.00 P"},
		{strings.Repeat("a", 36) + " Put", strings.Repeat("a", 36) + " Put"},
		{"MARVELL TECHNOLOGY INC 03/21/2025 $155 Call", "...TECHNOLOGY INC 03/21/2025 $155 Call"},
		{"ISHARES 20+ YEAR TREASURY BOND ETF 01/16/2026 $90 Call", "...TREASURY BOND ETF 01/16/2026 $90 Call"},
		{"MARVELL TECHNOLOGY INC\t03/21/2025  $155\x1b[2J Call", "...INC 03/21/2025 $155?[2J Call"},
		{strings.Repeat("ab", 25), "...b" + strings.Repeat("ab", 18)},
	} {
		if got := tailCell(tc.in, 40); got != tc.want {
			t.Errorf("tailCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
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

// A value that cannot be read is named in a warning, which counts every
// account that it is true of, and the value falls back to the aggregated
// balance's.
func TestLookupsWarnOfUnreadableBalances(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.addAccount("33334444", 2000, 0, testNow, time.Hour)
	f.setBalancesJSON(`{"liquidationValue":"12345.60","cashBalance":100.0}`)
	doLogin(t)
	stdout, stderr, err := runTx(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if want := "warning: 2 accounts differed from what Schwab has sent before (securitiesAccount.currentBalances.liquidationValue: " +
		"Schwab sent a string where a number was expected); the table may not show those fields as Schwab sent them, " +
		"and -json prints Schwab's own JSON\n"; stderr != want {
		t.Errorf("warned %q\nwant %q", stderr, want)
	}
	if want := `ACCOUNT  NAME       TYPE       VALUE
...2222  Account 1  MARGIN  12345.60
...4444  Account 2  MARGIN  12345.60
TOTAL                       24691.20
`; stdout != want {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want)
	}
}

// Each - in balances, and so in its column's total, is explained on
// stderr: a warning names each account whose figure Schwab did not send,
// and each whose cash is not what its value leaves once the market values
// of its holdings are taken out. Long options are a holding like any
// other.
func TestLookupsSayWhyAFigureIsNone(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.addAccount("33334444", 2000, 0, testNow, time.Hour)
	doLogin(t)
	// The market values of holdings worth nothing.
	const nothing = `"longMarketValue":0,"shortMarketValue":0,"longOptionMarketValue":0,"shortOptionMarketValue":0,"mutualFundValue":0,"bondValue":0`
	for _, tc := range []struct {
		name, balances, stdout, stderr string
	}{
		{
			"savings beside the cash",
			`{"liquidationValue":1500,"cashBalance":1000,"buyingPower":2000,"savings":500,` + nothing + `}`,
			`ACCOUNT  NAME       TYPE      VALUE  CASH  BUYING POWER
...2222  Account 1  MARGIN  1500.00     -       2000.00
...4444  Account 2  MARGIN  1500.00     -       2000.00
TOTAL                       3000.00     -
`,
			"warning: Schwab's cash for ...2222 (Account 1) is not its value less its holdings, so CASH shows -\n" +
				"warning: Schwab's cash for ...4444 (Account 2) is not its value less its holdings, so CASH shows -\n",
		},
		{
			"long options beside the cash",
			`{"liquidationValue":1500,"cashBalance":1000,"buyingPower":2000,` +
				strings.Replace(nothing, `"longOptionMarketValue":0`, `"longOptionMarketValue":500`, 1) + `}`,
			`ACCOUNT  NAME       TYPE      VALUE     CASH  BUYING POWER
...2222  Account 1  MARGIN  1500.00  1000.00       2000.00
...4444  Account 2  MARGIN  1500.00  1000.00       2000.00
TOTAL                       3000.00  2000.00
`,
			"",
		},
		{
			"no buying power",
			`{"liquidationValue":1500.01,"cashBalance":1500}`,
			`ACCOUNT  NAME       TYPE      VALUE     CASH  BUYING POWER
...2222  Account 1  MARGIN  1500.01  1500.00             -
...4444  Account 2  MARGIN  1500.01  1500.00             -
TOTAL                       3000.02  3000.00
`,
			"warning: Schwab sent no current buying power for ...2222 (Account 1)\n" +
				"warning: Schwab sent no current buying power for ...4444 (Account 2)\n",
		},
		{
			"no cash beside every market value",
			`{"liquidationValue":1500,"buyingPower":2000,` + nothing + `}`,
			`ACCOUNT  NAME       TYPE      VALUE  CASH  BUYING POWER
...2222  Account 1  MARGIN  1500.00     -       2000.00
...4444  Account 2  MARGIN  1500.00     -       2000.00
TOTAL                       3000.00     -
`,
			"warning: Schwab sent no current cash for ...2222 (Account 1)\n" +
				"warning: Schwab sent no current cash for ...4444 (Account 2)\n",
		},
		{
			"no current balances",
			`{}`,
			`ACCOUNT  NAME       TYPE       VALUE  CASH  BUYING POWER
...2222  Account 1  MARGIN  12345.60     -             -
...4444  Account 2  MARGIN  12345.60     -             -
TOTAL                       24691.20     -
`,
			"warning: Schwab sent no current cash or buying power for ...2222 (Account 1)\n" +
				"warning: Schwab sent no current cash or buying power for ...4444 (Account 2)\n",
		},
		{
			"no balances at all",
			"",
			`ACCOUNT  NAME       TYPE    VALUE  CASH  BUYING POWER
...2222  Account 1  MARGIN      -     -             -
...4444  Account 2  MARGIN      -     -             -
TOTAL                           -     -
`,
			"warning: Schwab sent no current value, cash or buying power for ...2222 (Account 1)\n" +
				"warning: Schwab sent no current value, cash or buying power for ...4444 (Account 2)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.balances != "" {
				f.setBalancesJSON(tc.balances)
			} else {
				// The current balances empty, and no aggregated balance.
				f.setBalancesJSON(`{}`)
				for _, number := range []string{"11112222", "33334444"} {
					var details map[string]json.RawMessage
					if err := json.Unmarshal([]byte(f.account(number).details), &details); err != nil {
						t.Fatal(err)
					}
					delete(details, "aggregatedBalance")
					js, err := json.Marshal(details)
					if err != nil {
						t.Fatal(err)
					}
					f.SetDetails(f.hash(number), string(js))
				}
			}
			stdout, stderr, err := runTx(t, "balances")
			if err != nil {
				t.Fatal(err)
			}
			if stdout != tc.stdout {
				t.Errorf("printed\n%s\nwant\n%s", stdout, tc.stdout)
			}
			if stderr != tc.stderr {
				t.Errorf("warned %q\nwant %q", stderr, tc.stderr)
			}
		})
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

// A nickname with control characters in it prints plain wherever it
// shows: in the tables, and in the warnings and the errors that name its
// account. No escape reaches the terminal, and a tab is a space.
func TestLookupsPrintNicknamesPlain(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.SetUserPreference(preferenceBody("11112222", "Joint\x1b[31m\tBrokerage"))
	doLogin(t)
	stdout, stderr, err := runTx(t, "accounts")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ACCOUNT  NAME                  TYPE       VALUE\n...2222  Joint?[31m Brokerage  MARGIN  12345.60\n"; stdout != want || stderr != "" {
		t.Errorf("printed\n%q\nwarned %q; want\n%q", stdout, stderr, want)
	}
	// The fake sends no buying power, which balances warns of.
	stdout, stderr, err = runTx(t, "balances")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ACCOUNT  NAME                  TYPE       VALUE    CASH  BUYING POWER\n" +
		"...2222  Joint?[31m Brokerage  MARGIN  12345.60  100.00             -\n"; stdout != want {
		t.Errorf("printed\n%q\nwant\n%q", stdout, want)
	}
	if want := "warning: Schwab sent no current buying power for ...2222 (Joint?[31m Brokerage)\n"; stderr != want {
		t.Errorf("warned %q, want %q", stderr, want)
	}
	// An error lists the accounts with their nicknames, which positions
	// reads for that.
	for _, command := range []string{"accounts", "positions"} {
		_, _, err := runTx(t, command, "-account", "9999")
		if want := `no account matches -account "9999": the login shares ...2222 (Joint?[31m Brokerage); ` +
			"to share another, run `schwabtx login` and tick it"; err == nil || err.Error() != want {
			t.Errorf("%s -account 9999: %v, want %q", command, err, want)
		}
	}
}

// Every cell of text that Schwab or the user wrote prints its control and
// format characters as ?, in each table, whether it shows full numbers or
// masked ones: an account's number, nickname and type, and a position's
// symbol and description; and so does a warning that names an account.
func TestLookupsPrintTextPlain(t *testing.T) {
	const esc = `\u001b[2J` // an escape that clears the screen, as JSON writes it
	a := namedAccount{
		account: account(t, `{"securitiesAccount":{"type":"MARGIN`+esc+`","accountNumber":"1111`+esc+`2222","positions":[`+
			`{"longQuantity":1,"shortQuantity":0,"averagePrice":1,"marketValue":1,"longOpenProfitLoss":0,`+
			`"instrument":{"assetType":"EQUITY","symbol":"AAPL`+esc+`","description":"APPLE`+esc+` INC"}}],"currentBalances":{}}}`),
		nickname: "Joint\x1b[2J",
	}
	for _, tc := range []struct {
		name  string
		table func([]namedAccount, bool) ([]column, [][]string, []string)
	}{
		{"accounts", accountsTable},
		{"balances", balancesTable},
		{"positions", positionsTable},
	} {
		for _, show := range []bool{false, true} {
			_, rows, warnings := tc.table([]namedAccount{a}, show)
			if all := fmt.Sprint(rows, warnings); strings.ContainsRune(all, '\x1b') || !strings.Contains(all, "?[2J") {
				t.Errorf("%s, show %t: printed %q, warned %q", tc.name, show, rows, warnings)
			}
		}
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

	// Schwab's answers keep their hints, as the accounts' do.
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.TooMany)
	_, _, err = runTx(t, "positions", "-account", "Individual")
	if want := `fetching the user preferences, to find the account named "Individual": HTTP 429 from Schwab after 4 tries`; err == nil ||
		!strings.HasPrefix(err.Error(), want) || !strings.Contains(err.Error(), "wait a minute") {
		t.Errorf("-account Individual, rate limited: %v", err)
	}

	// A cancel, as by Ctrl-C, while the preferences are read ends the run:
	// a read for the table's nicknames, and one for the names of the
	// accounts that an error lists.
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.Hang)
	for _, args := range [][]string{{"balances"}, {"positions", "-account", "9999"}, {"accounts", "-json", "-account", "9999"}} {
		ctx, cancel := context.WithCancel(context.Background())
		f.OnAPI(func(r *http.Request) {
			if schwabtest.Route(r.Pattern) == schwabtest.UserPreferenceRoute {
				cancel()
			}
		})
		var o, e bytes.Buffer
		err = run(ctx, args, strings.NewReader(""), &o, &e)
		cancel()
		if !errors.Is(err, context.Canceled) || o.Len() != 0 || strings.Contains(e.String(), "leaves out") {
			t.Errorf("%q canceled: %v, printed %q, warned %q", args, err, o.String(), e.String())
		}
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
		for _, args := range [][]string{{"accounts"}, {"balances"}, {"balances", "-json"}, {"positions"}} {
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
		for _, args := range [][]string{{"accounts"}, {"balances", "-json"}, {"positions"}} {
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
		{"positions", "extra"},
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
		"schwabtx login ", "schwabtx [flags] ", "schwabtx accounts ", "schwabtx positions ", "schwabtx balances ",
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

	// Each command's -h prints its usage, which main takes for no error.
	for _, args := range [][]string{{"-h"}, {"login", "-h"}, {"accounts", "-h"}, {"positions", "-h"}, {"balances", "-h"}, {"snapshot", "-h"}} {
		stdout, stderr, err := runTx(t, args...)
		if want := "Usage of schwabtx"; !errors.Is(err, flag.ErrHelp) || stdout != "" || !strings.HasPrefix(stderr, want) {
			t.Errorf("%q: %v, printed %q, warned %q; want flag.ErrHelp and %q", args, err, stdout, stderr, want)
		}
	}
	if _, stderr, err := runTx(t, "login", "-h"); !strings.Contains(stderr, "no flags") {
		t.Errorf("login -h: %v, printed %q", err, stderr)
	}
	for _, args := range [][]string{{"login", "now"}, {"login", "-n", "5"}} {
		if _, _, err := runTx(t, args...); err == nil || errors.Is(err, flag.ErrHelp) {
			t.Errorf("%q: %v, want an error", args, err)
		}
	}
	if _, _, err := runTx(t, "login", "now"); err == nil || err.Error() != "login takes no arguments" {
		t.Errorf("login now: %v", err)
	}
}

// positionsRequest is the request for the accounts with their positions,
// as runLookupOn gives it.
var positionsRequest = string(schwabtest.AccountsRoute) + "?fields=positions"

// The positions of the fixtures' accounts: all of them, and the first
// account's.
const (
	positionsOfThree = `ACCOUNT  SYMBOL                     QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
...0135  00507V109                   10     81.50     0.00   815.00   -815.00
...0135  AAPL                    2.1788    190.20   495.70   414.41     81.29
...0135  SWVXX                  1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
...0135  VTI                         12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
...3398  META                         1    512.00   603.40   512.00     91.40
...3398  XYZ   260619P00045000       -2      1.60  -310.00  -320.00     10.00  XYZ 06/19/2026 45.00 P
TOTAL                                              5758.91  5657.99    100.92
`
	positionsOfThreeShown = `ACCOUNT   SYMBOL                     QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
29470135  00507V109                   10     81.50     0.00   815.00   -815.00
29470135  AAPL                    2.1788    190.20   495.70   414.41     81.29
29470135  SWVXX                  1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
29470135  VTI                         12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
61803398  META                         1    512.00   603.40   512.00     91.40
61803398  XYZ   260619P00045000       -2      1.60  -310.00  -320.00     10.00  XYZ 06/19/2026 45.00 P
TOTAL                                               5758.91  5657.99    100.92
`
	positionsOf0135 = `ACCOUNT  SYMBOL         QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
...0135  00507V109       10     81.50     0.00   815.00   -815.00
...0135  AAPL        2.1788    190.20   495.70   414.41     81.29
...0135  SWVXX      1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
...0135  VTI             12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
TOTAL                                  5465.51  5465.99     -0.48
`
)

// positions lists the fixtures' accounts' positions, account by account and
// by symbol, from one request, for the accounts with their positions: the
// table names accounts by their numbers alone. Each VALUE less its COST is
// its OPEN P/L, the totals too, and the first account's VALUE and its cash
// make its value, 6997.68.
func TestPositionsOfTheFixtures(t *testing.T) {
	f := setupFixtures(t)
	for _, tc := range []struct {
		args           []string
		stdout, stderr string
	}{
		{[]string{"positions"}, positionsOfThree, "6 positions in 3 accounts, not counting cash (see schwabtx balances)\n"},
		{[]string{"positions", "-show-accounts"}, positionsOfThreeShown, "6 positions in 3 accounts, not counting cash (see schwabtx balances)\n"},
		{[]string{"positions", "-account", "0135"}, positionsOf0135, "4 positions in 1 account, not counting cash (see schwabtx balances)\n"},
	} {
		stdout, stderr, requests, err := runLookupOn(t, f, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		if stdout != tc.stdout {
			t.Errorf("%q printed\n%s\nwant\n%s", tc.args, stdout, tc.stdout)
		}
		if stderr != tc.stderr {
			t.Errorf("%q warned %q, want %q", tc.args, stderr, tc.stderr)
		}
		if want := []string{positionsRequest}; !equal(requests, want) {
			t.Errorf("%q made the requests %q, want %q", tc.args, requests, want)
		}
	}
}

// -account picks the positions of one account, by digits without the
// preferences and by nickname with them. An account that holds nothing has
// the header alone. An error lists the accounts with their nicknames,
// which it reads for that.
func TestPositionsOfAnAccount(t *testing.T) {
	f := setupFixtures(t)
	margin := `ACCOUNT  SYMBOL                 QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
...3398  META                     1    512.00   603.40   512.00     91.40
...3398  XYZ   260619P00045000   -2      1.60  -310.00  -320.00     10.00  XYZ 06/19/2026 45.00 P
TOTAL                                           293.40   192.00    101.40
`
	for _, account := range []string{"3398", "...3398", "61803398", "options & margin"} {
		stdout, stderr, requests, err := runLookupOn(t, f, "positions", "-account", account)
		want := []string{positionsRequest}
		if account == "options & margin" {
			want = append(want, preferencesRequest)
		}
		if err != nil || stdout != margin || stderr != "2 positions in 1 account, not counting cash (see schwabtx balances)\n" ||
			!equal(requests, want) {
			t.Errorf("-account %s: %v, warned %q, after the requests %q, printed\n%s", account, err, stderr, requests, stdout)
		}
	}
	stdout, stderr, requests, err := runLookupOn(t, f, "positions", "-account", "Contributory")
	if want := "ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION\n"; err != nil || stdout != want ||
		stderr != "0 positions in 1 account, not counting cash (see schwabtx balances)\n" ||
		!equal(requests, []string{positionsRequest, preferencesRequest}) {
		t.Errorf("-account Contributory: %v, warned %q, after the requests %q, printed\n%s", err, stderr, requests, stdout)
	}
	_, _, requests, err = runLookupOn(t, f, "positions", "-account", "9999")
	if want := `no account matches -account "9999": the login shares ...0135 (Individual), ...3398 (Options & Margin), ` +
		"...1783 (Contributory); to share another, run `schwabtx login` and tick it"; err == nil || err.Error() != want ||
		!equal(requests, []string{positionsRequest, preferencesRequest}) {
		t.Errorf("-account 9999: %v, after the requests %q", err, requests)
	}
}

// -json prints each account as Schwab sent it, with its positions,
// re-indented: "&", the line separator U+2028 and 250.0 as they came.
func TestPositionsJSON(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	doLogin(t)
	stdout, stderr, err := runTx(t, "positions", "-json")
	if err != nil || stderr != "" {
		t.Fatalf("%v, warned %q", err, stderr)
	}
	var want bytes.Buffer
	want.WriteString("[\n  ")
	if err := json.Indent(&want, []byte(f.account("11112222").details), "  ", "  "); err != nil {
		t.Fatal(err)
	}
	want.WriteString("\n]\n")
	if stdout != want.String() {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want.String())
	}
	for _, kept := range []string{"AT&T\u2028INC", `"marketValue": 250.0`} {
		if !strings.Contains(stdout, kept) {
			t.Errorf("printed no %q", kept)
		}
	}
}

// position decodes js, a position as GET /accounts sends one.
func position(t *testing.T, js string) schwab.Position {
	t.Helper()
	var p schwab.Position
	if err := json.Unmarshal([]byte(js), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A position's row: its quantity and value signed as the side it holds, its
// average cost, its open profit or loss, positive for a gain, and its cost,
// its value less its open profit or loss. A figure that could not be read,
// or that its side lacks, is none, and so is what needs it.
func TestHoldingOf(t *testing.T) {
	const long = `"longQuantity":12,"shortQuantity":0,"averagePrice":251.3075,"marketValue":3748.92,"longOpenProfitLoss":733.23,` +
		`"instrument":{"assetType":"COLLECTIVE_INVESTMENT","symbol":"VTI"}`
	const short = `"longQuantity":0,"shortQuantity":100,"averagePrice":52.5,"marketValue":-5000,"shortOpenProfitLoss":250,` +
		`"instrument":{"assetType":"EQUITY","symbol":"XYZ"}`
	for _, tc := range []struct {
		name, position string
		want           string // quantity, average cost, value, cost and open profit or loss
		missing        string // the problem of the side's figure that it lacks
	}{
		{"long", "{" + long + "}", "12 251.3075 3748.92 3015.69 733.23", ""},
		{"short", "{" + short + "}", "-100 52.50 -5000.00 -5250.00 250.00", ""},
		{
			"a short put",
			`{"longQuantity":0,"shortQuantity":2,"averagePrice":1.6,"marketValue":-310,"shortOpenProfitLoss":10,` +
				`"instrument":{"assetType":"OPTION","symbol":"XYZ   260619P00045000"}}`,
			"-2 1.60 -310.00 -320.00 10.00", "",
		},
		{
			"a long call",
			`{"longQuantity":3,"shortQuantity":0,"averagePrice":2.15,"marketValue":750,"longOpenProfitLoss":105,` +
				`"instrument":{"assetType":"OPTION","symbol":"XYZ   260619C00050000"}}`,
			"3 2.15 750.00 645.00 105.00", "",
		},
		{
			"binary noise",
			`{"longQuantity":0.30000000000000004,"shortQuantity":0,"averagePrice":1.23456789,"marketValue":-236.999999999999,` +
				`"longOpenProfitLoss":81.29223999999999,"instrument":{"assetType":"EQUITY","symbol":"X"}}`,
			"0.3 1.234568 -237.00 -318.29 81.29", "",
		},
		{
			"a long position without its open profit or loss",
			"{" + strings.Replace(long, `"longOpenProfitLoss":733.23,`, ``, 1) + "}",
			"12 251.3075 3748.92 - -", "longOpenProfitLoss: missing, though Schwab sends it on every long position",
		},
		{
			"a long position with the short side's",
			"{" + strings.Replace(long, `"longOpenProfitLoss":733.23`, `"shortOpenProfitLoss":733.23`, 1) + "}",
			"12 251.3075 3748.92 - -", "longOpenProfitLoss: missing, though Schwab sends it on every long position",
		},
		{
			"a short position without its open profit or loss",
			"{" + strings.Replace(short, `"shortOpenProfitLoss":250,`, ``, 1) + "}",
			"-100 52.50 -5000.00 - -", "shortOpenProfitLoss: missing, though Schwab sends it on every short position",
		},
		{
			"a short position's open profit or loss sent as null",
			"{" + strings.Replace(short, `"shortOpenProfitLoss":250`, `"shortOpenProfitLoss":null`, 1) + "}",
			"-100 52.50 -5000.00 - -", "shortOpenProfitLoss: missing, though Schwab sends it on every short position",
		},
		{
			"a position of neither side",
			`{"longQuantity":0,"shortQuantity":0,"averagePrice":0,"marketValue":0,"instrument":{"assetType":"EQUITY","symbol":"X"}}`,
			"0 0.00 0.00 0.00 0.00", "",
		},
		{"an unreadable value", "{" + strings.Replace(long, `3748.92`, `"3748.92"`, 1) + "}", "12 251.3075 - - 733.23", ""},
		{"no value", "{" + strings.Replace(long, `"marketValue":3748.92,`, ``, 1) + "}", "12 251.3075 - - 733.23", ""},
		{"an unreadable average cost", "{" + strings.Replace(long, `251.3075`, `true`, 1) + "}", "12 - 3748.92 3015.69 733.23", ""},
		{"no average cost", "{" + strings.Replace(long, `"averagePrice":251.3075,`, ``, 1) + "}", "12 - 3748.92 3015.69 733.23", ""},
		{"an unreadable quantity", "{" + strings.Replace(long, `"longQuantity":12`, `"longQuantity":"12"`, 1) + "}", "- 251.3075 3748.92 - -", ""},
		{
			"an unreadable short quantity",
			"{" + strings.Replace(short, `"shortQuantity":100`, `"shortQuantity":"100"`, 1) + "}",
			"- 52.50 -5000.00 - -", "",
		},
		{
			"an unreadable open profit or loss",
			"{" + strings.Replace(long, `733.23`, `"733.23"`, 1) + "}",
			"12 251.3075 3748.92 - -", "",
		},
		{"not an object", `null`, "- - - - -", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := position(t, tc.position)
			h := holdingOf(p)
			if got := fmt.Sprintf("%s %s %s %s %s", h.quantity, h.price, h.value, h.cost, h.gain); got != tc.want {
				t.Errorf("got %q, want %q (problems %v)", got, tc.want, p.Problems)
			}
			_, missing := openPL(p)
			got := ""
			if len(missing) > 0 {
				got = missing[0].String()
			}
			if got != tc.missing || len(missing) > 1 {
				t.Errorf("missing %v, want %q", missing, tc.missing)
			}
		})
	}
}

// holdingAccount returns account 11112222 with the given positions, the
// elements of a list.
func holdingAccount(positions string) string {
	return `{"securitiesAccount":{"type":"MARGIN","accountNumber":"11112222","roundTrips":0,"isDayTrader":false,` +
		`"isClosingOnlyRestricted":false,"pfcbFlag":false,"positions":[` + positions + `],` +
		`"initialBalances":{},"currentBalances":{},"projectedBalances":{}}}`
}

// The table sorts each account's positions by symbol, in byte order, so
// that an option comes after its stock and before the next symbol, and
// keeps the order of equal symbols. An account whose positions could not
// be read, or that is not an object, has a row of its own, and the totals
// are none; so are those of a position whose figure could not be read.
func TestPositionsTable(t *testing.T) {
	pos := func(symbol string, value float64, description string) string {
		instrument, _ := json.Marshal(map[string]string{"assetType": "EQUITY", "symbol": symbol, "description": description})
		return fmt.Sprintf(`{"longQuantity":1,"shortQuantity":0,"averagePrice":1,"marketValue":%v,"longOpenProfitLoss":0,"instrument":%s}`,
			value, instrument)
	}
	// More positions of one symbol than a sort that is not stable keeps in
	// order, among others.
	var manyOfOne []string
	var manyRows strings.Builder
	manyRows.WriteString("ACCOUNT  SYMBOL  QTY  AVG COST  VALUE   COST  OPEN P/L  DESCRIPTION\n")
	for i := range 40 {
		if i%3 == 0 {
			manyOfOne = append(manyOfOne, pos("ZZZ", 1, ""))
		}
		manyOfOne = append(manyOfOne, pos("XYZ", 1, strconv.Itoa(40-i)))
		fmt.Fprintf(&manyRows, "...2222  XYZ       1      1.00   1.00   1.00      0.00  %d\n", 40-i)
	}
	for range 14 {
		manyRows.WriteString("...2222  ZZZ       1      1.00   1.00   1.00      0.00\n")
	}
	manyRows.WriteString("TOTAL                           54.00  54.00      0.00\n")
	for _, tc := range []struct {
		name     string
		accounts []string
		want     string
	}{
		{
			"symbols",
			[]string{holdingAccount(strings.Join([]string{
				pos("XYZ   260619P00045000", 1, ""), pos("XYZA", 2, ""), pos("XYZ", 3, "first"), pos("AAPL", 4, ""), pos("XYZ", 5, "second"),
			}, ","))},
			`ACCOUNT  SYMBOL                 QTY  AVG COST  VALUE   COST  OPEN P/L  DESCRIPTION
...2222  AAPL                     1      1.00   4.00   4.00      0.00
...2222  XYZ                      1      1.00   3.00   3.00      0.00  first
...2222  XYZ                      1      1.00   5.00   5.00      0.00  second
...2222  XYZ   260619P00045000    1      1.00   1.00   1.00      0.00
...2222  XYZA                     1      1.00   2.00   2.00      0.00
TOTAL                                          15.00  15.00      0.00
`,
		},
		{
			"positions that are not a list",
			[]string{
				holdingAccount(pos("AAPL", 4, "")),
				`{"securitiesAccount":{"type":"MARGIN","accountNumber":"33334444","positions":{"longQuantity":1}}}`,
			},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...2222  AAPL      1      1.00   4.00  4.00      0.00
...4444  ?         -         -      -     -         -
TOTAL                               -     -         -
`,
		},
		{
			"an account that is not an object",
			[]string{holdingAccount(pos("AAPL", 4, "")), `null`},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...2222  AAPL      1      1.00   4.00  4.00      0.00
?        ?         -         -      -     -         -
TOTAL                               -     -         -
`,
		},
		{
			"a position that is not an object",
			[]string{holdingAccount(pos("AAPL", 4, "") + `,null`)},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...2222  ?         -         -      -     -         -
...2222  AAPL      1      1.00   4.00  4.00      0.00
TOTAL                               -     -         -
`,
		},
		{
			"a description with control characters",
			[]string{holdingAccount(pos("AAPL", 4, "APPLE\u001b[2J\u2028INC "+strings.Repeat("x", 40)))},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...2222  AAPL      1      1.00   4.00  4.00      0.00  APPLE?[2J INC xxxxxxxxxxxxxxxxxxxxxxx...
`,
		},
		{
			// Schwab's description of an option ends with its expiration,
			// strike and type, which a long one keeps, cut at its start.
			// jwplatta/schwab_rb's real account of 2025 holds this call,
			// short.
			"an option's long description",
			[]string{holdingAccount(pos("AAPL", 4, "APPLE INC "+strings.Repeat("x", 40)) + "," +
				`{"longQuantity":0,"shortQuantity":1,"averagePrice":2.0533,"marketValue":-272.5,"shortOpenProfitLoss":-67.17,` +
				`"instrument":{"assetType":"OPTION","symbol":"MRVL  250321C00155000","description":"MARVELL TECHNOLOGY INC 03/21/2025 $155 Call"}}`)},
			`ACCOUNT  SYMBOL                 QTY  AVG COST    VALUE     COST  OPEN P/L  DESCRIPTION
...2222  AAPL                     1      1.00     4.00     4.00      0.00  APPLE INC xxxxxxxxxxxxxxxxxxxxxxxxxxx...
...2222  MRVL  250321C00155000   -1    2.0533  -272.50  -205.33    -67.17  ...TECHNOLOGY INC 03/21/2025 $155 Call
TOTAL                                          -268.50  -201.33    -67.17
`,
		},
		{
			"many of one symbol",
			[]string{holdingAccount(strings.Join(manyOfOne, ","))},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var as []namedAccount
			for _, js := range tc.accounts {
				as = append(as, namedAccount{account: account(t, js)})
			}
			cols, rows, warnings := positionsTable(as, false)
			var b bytes.Buffer
			if err := writeTable(&b, cols, rows); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				tc.want = manyRows.String()
			}
			if b.String() != tc.want || warnings != nil {
				t.Errorf("printed\n%s\nwant\n%s\nwarned %q", b.String(), tc.want, warnings)
			}
		})
	}
}

// The line before the positions table counts the positions, and the
// accounts whose positions could be read. An account whose positions could
// not be read, whose row in the table is ?, is counted apart, not as one
// that holds none.
func TestPositionsSummary(t *testing.T) {
	const position = `{"longQuantity":1,"shortQuantity":0,"averagePrice":1,"marketValue":1,"longOpenProfitLoss":0,` +
		`"instrument":{"assetType":"EQUITY","symbol":"X"}}`
	two := holdingAccount(position + "," + position)
	unread := `{"securitiesAccount":{"type":"MARGIN","accountNumber":"33334444","positions":{"longQuantity":1}}}`
	// Positions sent twice, of which the record keeps one copy, uncertain.
	twice := strings.Replace(holdingAccount(position), `"positions":[`, `"positions":[`+position+`],"positions":[`, 1)
	for _, tc := range []struct {
		accounts []string
		want     string
	}{
		{[]string{two}, "2 positions in 1 account, not counting cash (see schwabtx balances)"},
		{[]string{two, holdingAccount(position), holdingAccount("")}, "3 positions in 3 accounts, not counting cash (see schwabtx balances)"},
		{
			[]string{two, unread},
			"2 positions in 1 account, not counting cash (see schwabtx balances); the positions of 1 account could not be read",
		},
		{
			[]string{unread, two, `null`},
			"2 positions in 1 account, not counting cash (see schwabtx balances); the positions of 2 accounts could not be read",
		},
		{
			[]string{two, twice},
			"2 positions in 1 account, not counting cash (see schwabtx balances); the positions of 1 account could not be read",
		},
	} {
		var as []namedAccount
		for _, js := range tc.accounts {
			as = append(as, namedAccount{account: account(t, js)})
		}
		if got := positionsSummary(as); got != tc.want {
			t.Errorf("%d accounts: %q, want %q", len(as), got, tc.want)
		}
	}
}

// A position whose figure could not be read, or whose side's open profit or
// loss is missing, is named in the warning, and its row has - where the
// figure is needed, as do the totals.
func TestPositionsWarnOfUnreadablePositions(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.setPositionsJSON(strings.Replace(strings.Replace(fakePositions, `"marketValue":1300.5,`, `"marketValue":"1300.5",`, 1),
		`"longOpenProfitLoss":75.0,`, ``, 1))
	doLogin(t)
	stdout, stderr, err := runTx(t, "positions")
	if err != nil {
		t.Fatal(err)
	}
	if want := "warning: 1 account differed from what Schwab has sent before (securitiesAccount.positions.marketValue: " +
		"Schwab sent a string where a number was expected; securitiesAccount.positions.longOpenProfitLoss: " +
		"missing, though Schwab sends it on every long position); the table may not show those fields as Schwab sent them, " +
		"and -json prints Schwab's own JSON\n2 positions in 1 account, not counting cash (see schwabtx balances)\n"; stderr != want {
		t.Errorf("warned %q\nwant %q", stderr, want)
	}
	want := `ACCOUNT  SYMBOL  QTY  AVG COST   VALUE  COST  OPEN P/L  DESCRIPTION
...2222  T        10     17.50  250.00     -         -  AT&T INC
...2222  VTI       5    246.91       -     -     65.95  VANGUARD TOTAL STOCK MARKET ETF
TOTAL                                -     -         -
`
	if stdout != want {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want)
	}
}

// When the preferences cannot be read, positions print as before, since
// they need none, but a nickname given to -account is an error.
func TestPositionsWithoutPreferences(t *testing.T) {
	f := setupFixtures(t)
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.Unavailable)
	stdout, _, err := runTx(t, "positions")
	if err != nil || stdout != positionsOfThree {
		t.Errorf("%v, printed\n%s", err, stdout)
	}
	_, _, err = runTx(t, "positions", "-account", "Individual")
	if want := `fetching the user preferences, to find the account named "Individual": HTTP 503 from Schwab after 4 tries`; err == nil ||
		!strings.HasPrefix(err.Error(), want) {
		t.Errorf("-account Individual: %v, want %q", err, want)
	}
}

// Quantities print as many decimals as they have, up to 6, and prices at
// least 2.
func TestFormatQuantityAndPrice(t *testing.T) {
	for _, tc := range []struct {
		x               float64
		quantity, price string
	}{
		{0, "0", "0.00"},
		{math.Copysign(0, -1), "0", "0.00"},
		{-0.0000004, "0", "0.00"},
		{12, "12", "12.00"},
		{-2, "-2", "-2.00"},
		{2.1788, "2.1788", "2.1788"},
		{81.5, "81.5", "81.50"},
		{1.23456789, "1.234568", "1.234568"},
		{0.30000000000000004, "0.3", "0.30"},
		{-236.999999999999, "-237", "-237.00"},
		{1e12, "-", "-"},
	} {
		if got := formatQuantity(tc.x); got != tc.quantity {
			t.Errorf("formatQuantity(%v) = %q, want %q", tc.x, got, tc.quantity)
		}
		if got := formatPrice(tc.x); got != tc.price {
			t.Errorf("formatPrice(%v) = %q, want %q", tc.x, got, tc.price)
		}
	}
}
