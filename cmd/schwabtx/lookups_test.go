package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"runtime"
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
...135   Individual        CASH    6997.68
...398   Options & Margin  MARGIN  5503.84
...783   Contributory      MARGIN        -
TOTAL                                    -
`
	accountsOfThreeShown = `ACCOUNT   NAME              TYPE      VALUE
29470135  Individual        CASH    6997.68
61803398  Options & Margin  MARGIN  5503.84
40221783  Contributory      MARGIN        -
TOTAL                                     -
`
	balancesOfThree = `ACCOUNT  NAME              TYPE      VALUE     CASH  BUYING POWER
...135   Individual        CASH    6997.68  1532.17       1532.17
...398   Options & Margin  MARGIN  5503.84  5210.44       8258.84
...783   Contributory      MARGIN        -        -       8710.00
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
		{[]string{"accounts"}, accountsOfThree, "warning: Schwab sent no current value for ...783 (Contributory)\n"},
		{[]string{"accounts", "-show-accounts"}, accountsOfThreeShown, "warning: Schwab sent no current value for ...783 (Contributory)\n"},
		{[]string{"balances"}, balancesOfThree, "warning: Schwab sent no current value or cash for ...783 (Contributory)\n"},
		{[]string{"balances", "-show-accounts"}, balancesOfThreeShown, "warning: Schwab sent no current value or cash for ...783 (Contributory)\n"},
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
...135   Individual        CASH     6997.68
...398   Options & Margin  MARGIN   5503.84
TOTAL                              12501.52
`},
		{"balances", `ACCOUNT  NAME              TYPE       VALUE     CASH  BUYING POWER
...135   Individual        CASH     6997.68  1532.17       1532.17
...398   Options & Margin  MARGIN   5503.84  5210.44       8258.84
TOTAL                              12501.52  6742.61
`},
	} {
		stdout, stderr, err := runTx(t, tc.command)
		if err != nil || stdout != tc.want || stderr != "" {
			t.Errorf("%s printed\n%s\nwarned %q, %v; want\n%s", tc.command, stdout, stderr, err, tc.want)
		}
	}
}

// fixtureDigits returns the first run of 4 digits of a fixture account's
// number that s holds, or "": the tables, the warnings and the errors show
// no more of a number than its last 3 digits unless -show-accounts asks.
func fixtureDigits(s string) string {
	for _, a := range schwabtest.FixtureAccounts {
		for i := 0; i+4 <= len(a.Number); i++ {
			if run := a.Number[i : i+4]; strings.Contains(s, run) {
				return run
			}
		}
	}
	return ""
}

// No output shows more than the last 3 digits of an account's number
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

	// An error shows -account's value with more than 3 digits in a row as
	// ... and the last 3, as the tables mask a number, whatever separates
	// them and whatever words surround them.
	for _, tc := range []struct{ account, shown string }{
		{"03399", "...399"},
		{"1803399", "...399"},
		{"...61803399", "...399"},
		{"6180.3399", "...399"},
		{"6180_3399", "...399"},
		{"6180−3399", "...399"},
		{"#61803399", "#...399"},
		{"Account 2947-0136", "Account ...136"},
		{"2947 - 0136 Joint", "...136 Joint"},
		{"Joint ...29470136", "Joint ...136"},
		{"Roth 2019", "Roth ...019"},
		{"Joint 12-34", "Joint ...234"},
		{"401k 2019", "401k ...019"},
		{"Roth 019", "Roth 019"},
		{"Joint 1-23", "Joint 1-23"},
		{"401k", "401k"},
	} {
		_, _, err := runTx(t, "accounts", "-account", tc.account)
		if want := fmt.Sprintf("no account matches -account %q: ", tc.shown); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Errorf("-account %q: %v, want %q", tc.account, err, want)
		}
	}
	f.Override(schwabtest.UserPreferenceRoute, schwabtest.Unavailable)
	_, _, err := runTx(t, "positions", "-account", "Account 2947-0135")
	if want := `fetching the user preferences, to find the account named "Account ...135": `; err == nil || !strings.HasPrefix(err.Error(), want) {
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
...783   Contributory  MARGIN      -
`
	margin := `ACCOUNT  NAME              TYPE      VALUE     CASH  BUYING POWER
...398   Options & Margin  MARGIN  5503.84  5210.44       8258.84
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
	shares := "the login shares ...135 (Individual), ...398 (Options & Margin), ...783 (Contributory); " +
		"to share another, run `schwabtx login` and tick it"
	f := setupFixtures(t)
	for _, tc := range []struct {
		args     []string
		err      string
		requests int
	}{
		{[]string{"accounts", "-account", "9999"}, `no account matches -account "...999": ` + shares, 2},
		{[]string{"accounts", "-account", "61803399"}, `no account matches -account "...399": ` + shares, 2},
		{[]string{"balances", "-account", "Roth"}, `no account matches -account "Roth": ` + shares, 2},
		{[]string{"balances", "-json", "-account", "9999"}, `no account matches -account "...999": ` + shares, 2},
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
	several := `-account "...222" matches 2 accounts, ...222 (Account 1), ...222 (Account 2); `
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
			`-account "...222" matches 2 accounts, ...222 (Brokerage), ...222 (brokerage); ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		{
			[]string{"accounts", "-account", "BROKERAGE"},
			preferenceBody("11112222", "Brokerage", "33332222", "brokerage"),
			`-account "BROKERAGE" matches 2 accounts, ...222 (Brokerage), ...222 (brokerage); give the last digits of its number instead`,
		},
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", "Brokerage"),
			`-account "...222" matches 2 accounts, ...222 (Brokerage), ...222; ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", " ", "33332222", "Brokerage"),
			`-account "...222" matches 2 accounts, ...222, ...222 (Brokerage); ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		// Nicknames that differ only in their white space are the same name,
		// as the tables show them.
		{
			[]string{"accounts", "-account", "2222"},
			preferenceBody("11112222", "Joint Brokerage", "33332222", "joint  brokerage "),
			`-account "...222" matches 2 accounts, ...222 (Joint Brokerage), ...222 (joint brokerage); ` +
				"give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		},
		{
			[]string{"accounts", "-account", "JOINT BROKERAGE"},
			preferenceBody("11112222", "Joint Brokerage", "33332222", "joint  brokerage "),
			`-account "JOINT BROKERAGE" matches 2 accounts, ...222 (Joint Brokerage), ...222 (joint brokerage); ` +
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
...222   Roth IRA                 MARGIN  12345.60
...444   Joint Brokerage Account  MARGIN  12345.60
TOTAL                                     24691.20
`; err != nil || stdout != want {
		t.Errorf("%v, printed\n%s\nwant\n%s", err, stdout, want)
	}
	for _, tc := range []struct{ account, want string }{
		{"Roth IRA", "...222   Roth IRA"},
		{"roth\tira", "...222   Roth IRA"},
		{"Joint Brokerage Account", "...444   Joint Brokerage Account"},
		{"JOINT  BROKERAGE ACCOUNT", "...444   Joint Brokerage Account"},
	} {
		stdout, _, err := runTx(t, "accounts", "-account", tc.account)
		if lines := strings.Split(stdout, "\n"); err != nil || len(lines) != 3 || !strings.HasPrefix(lines[1], tc.want+" ") {
			t.Errorf("-account %q: %v, printed\n%s", tc.account, err, stdout)
		}
	}
}

// A nickname is free text, which the tables, the warnings and the errors
// show masked, as the store keeps it: a run of 8 digits, which may be an
// account number, shows as ... and its last 3, unless -show-accounts. The
// nickname picks its account as Schwab sent it or as the tables show it.
func TestLookupsMaskNicknames(t *testing.T) {
	f, _ := setup(t)
	f.addAccount("11112222", 1000, 0, testNow, time.Hour)
	f.addAccount("33334444", 2000, 0, testNow, time.Hour)
	f.SetUserPreference(preferenceBody("11112222", "Joint 6180 3398", "33334444", "Trf 48213907"))
	doLogin(t)
	stdout, _, err := runTx(t, "accounts")
	if want := `ACCOUNT  NAME          TYPE       VALUE
...222   Joint ...398  MARGIN  12345.60
...444   Trf ...907    MARGIN  12345.60
TOTAL                          24691.20
`; err != nil || stdout != want {
		t.Errorf("%v, printed\n%s\nwant\n%s", err, stdout, want)
	}
	if stdout, _, err := runTx(t, "accounts", "-show-accounts"); err != nil || !strings.Contains(stdout, "11112222  Joint 6180 3398") {
		t.Errorf("-show-accounts: %v, printed\n%s", err, stdout)
	}
	for _, account := range []string{"Joint 6180 3398", "joint ...398"} {
		if stdout, _, err := runTx(t, "accounts", "-account", account); err != nil || !strings.Contains(stdout, "...222   Joint ...398") {
			t.Errorf("-account %q: %v, printed\n%s", account, err, stdout)
		}
	}
	_, _, err = runTx(t, "accounts", "-account", "Nope 6180 3398")
	if want := `no account matches -account "Nope ...398": the login shares ...222 (Joint ...398), ...444 (Trf ...907); `; err == nil ||
		!strings.HasPrefix(err.Error(), want) {
		t.Errorf("-account of no account: %v, want %q", err, want)
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
	if got, want := accountCell("12345678", false), "...678"; got != want {
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
...222   Account 1  MARGIN  12345.60
...444   Account 2  MARGIN  12345.60
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
...222   Account 1  MARGIN  1500.00     -       2000.00
...444   Account 2  MARGIN  1500.00     -       2000.00
TOTAL                       3000.00     -
`,
			"warning: Schwab's cash for ...222 (Account 1) is not its value less its holdings, so CASH shows -\n" +
				"warning: Schwab's cash for ...444 (Account 2) is not its value less its holdings, so CASH shows -\n",
		},
		{
			"long options beside the cash",
			`{"liquidationValue":1500,"cashBalance":1000,"buyingPower":2000,` +
				strings.Replace(nothing, `"longOptionMarketValue":0`, `"longOptionMarketValue":500`, 1) + `}`,
			`ACCOUNT  NAME       TYPE      VALUE     CASH  BUYING POWER
...222   Account 1  MARGIN  1500.00  1000.00       2000.00
...444   Account 2  MARGIN  1500.00  1000.00       2000.00
TOTAL                       3000.00  2000.00
`,
			"",
		},
		{
			"no buying power",
			`{"liquidationValue":1500.01,"cashBalance":1500}`,
			`ACCOUNT  NAME       TYPE      VALUE     CASH  BUYING POWER
...222   Account 1  MARGIN  1500.01  1500.00             -
...444   Account 2  MARGIN  1500.01  1500.00             -
TOTAL                       3000.02  3000.00
`,
			"warning: Schwab sent no current buying power for ...222 (Account 1)\n" +
				"warning: Schwab sent no current buying power for ...444 (Account 2)\n",
		},
		{
			"no cash beside every market value",
			`{"liquidationValue":1500,"buyingPower":2000,` + nothing + `}`,
			`ACCOUNT  NAME       TYPE      VALUE  CASH  BUYING POWER
...222   Account 1  MARGIN  1500.00     -       2000.00
...444   Account 2  MARGIN  1500.00     -       2000.00
TOTAL                       3000.00     -
`,
			"warning: Schwab sent no current cash for ...222 (Account 1)\n" +
				"warning: Schwab sent no current cash for ...444 (Account 2)\n",
		},
		{
			"no current balances",
			`{}`,
			`ACCOUNT  NAME       TYPE       VALUE  CASH  BUYING POWER
...222   Account 1  MARGIN  12345.60     -             -
...444   Account 2  MARGIN  12345.60     -             -
TOTAL                       24691.20     -
`,
			"warning: Schwab sent no current cash or buying power for ...222 (Account 1)\n" +
				"warning: Schwab sent no current cash or buying power for ...444 (Account 2)\n",
		},
		{
			"no balances at all",
			"",
			`ACCOUNT  NAME       TYPE    VALUE  CASH  BUYING POWER
...222   Account 1  MARGIN      -     -             -
...444   Account 2  MARGIN      -     -             -
TOTAL                           -     -
`,
			"warning: Schwab sent no current value, cash or buying power for ...222 (Account 1)\n" +
				"warning: Schwab sent no current value, cash or buying power for ...444 (Account 2)\n",
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
	if want := `no account matches -account "...222": the login shares ?; to share another, run ` + "`schwabtx login` and tick it"; err == nil || err.Error() != want {
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
	named := "ACCOUNT  NAME       TYPE       VALUE\n...222   Brokerage  MARGIN  12345.60\n"
	nameless := "ACCOUNT  NAME  TYPE       VALUE\n...222         MARGIN  12345.60\n"
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
...222   First  MARGIN  12345.60
...444          MARGIN  12345.60
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
	if want := "ACCOUNT  NAME                  TYPE       VALUE\n...222   Joint?[31m Brokerage  MARGIN  12345.60\n"; stdout != want || stderr != "" {
		t.Errorf("printed\n%q\nwarned %q; want\n%q", stdout, stderr, want)
	}
	// The fake sends no buying power, which balances warns of.
	stdout, stderr, err = runTx(t, "balances")
	if err != nil {
		t.Fatal(err)
	}
	if want := "ACCOUNT  NAME                  TYPE       VALUE    CASH  BUYING POWER\n" +
		"...222   Joint?[31m Brokerage  MARGIN  12345.60  100.00             -\n"; stdout != want {
		t.Errorf("printed\n%q\nwant\n%q", stdout, want)
	}
	if want := "warning: Schwab sent no current buying power for ...222 (Joint?[31m Brokerage)\n"; stderr != want {
		t.Errorf("warned %q, want %q", stderr, want)
	}
	// An error lists the accounts with their nicknames, which positions
	// reads for that.
	for _, command := range []string{"accounts", "positions"} {
		_, _, err := runTx(t, command, "-account", "9999")
		if want := `no account matches -account "...999": the login shares ...222 (Joint?[31m Brokerage); ` +
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
		!strings.HasSuffix(stderr, "\nwarning: Schwab sent no current value or cash for ...783\n") {
		t.Errorf("warned %q", stderr)
	}
	if want := `ACCOUNT  NAME  TYPE      VALUE     CASH  BUYING POWER
...135         CASH    6997.68  1532.17       1532.17
...398         MARGIN  5503.84  5210.44       8258.84
...783         MARGIN        -        -       8710.00
TOTAL                        -        -
`; stdout != want {
		t.Errorf("printed\n%s\nwant\n%s", stdout, want)
	}

	_, _, err = runTx(t, "balances", "-account", "Individual")
	if want := `fetching the user preferences, to find the account named "Individual": HTTP 503 from Schwab after 4 tries`; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("-account Individual: %v, want %q", err, want)
	}
	_, _, err = runTx(t, "balances", "-json", "-account", "9999")
	if want := `no account matches -account "...999": the login shares ...135, ...398, ...783; ` +
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
		"schwabtx snapshot ", "schwabtx import ", "schwabtx status ", "schwabtx edit ", "schwabtx export ", "schwabtx store ",
		`"schwabtx <command> -h"`, "SCHWAB_STORE_FILE",
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
	for _, args := range [][]string{
		{"-h"},
		{"login", "-h"},
		{"accounts", "-h"},
		{"positions", "-h"},
		{"balances", "-h"},
		{"snapshot", "-h"},
		{"import", "-h"},
		{"status", "-h"},
		{"edit", "-h"},
		{"export", "-h"},
		{"store", "-h"},
		{"store", "check", "-h"},
	} {
		stdout, stderr, err := runTx(t, args...)
		if want := "Usage of schwabtx"; !errors.Is(err, flag.ErrHelp) || stdout != "" || !strings.HasPrefix(stderr, want) {
			t.Errorf("%q: %v, printed %q, warned %q; want flag.ErrHelp and %q", args, err, stdout, stderr, want)
		}
	}
	// The tables mask an account's number to its last 3 digits, as
	// -show-accounts says.
	for _, args := range [][]string{{"-h"}, {"accounts", "-h"}, {"positions", "-h"}, {"balances", "-h"}} {
		if _, stderr, _ := runTx(t, args...); !strings.Contains(stderr, "instead of the last 3 digits") {
			t.Errorf("%q printed %q, which does not say how the tables mask a number", args, stderr)
		}
	}
	// -store names where the store is by default on this system: on
	// Windows %LocalAppData%, not the config directory, which roams.
	where := "your config directory"
	if runtime.GOOS == "windows" {
		where = "%LocalAppData%"
	}
	for _, cmd := range []string{"import", "status", "edit", "export", "store"} {
		if _, stderr, _ := runTx(t, cmd, "-h"); !strings.Contains(stderr, "history.db in schwabtx's folder of "+where+")") {
			t.Errorf("%s -h does not say the store is in %s:\n%s", cmd, where, stderr)
		}
	}
	// export -h lists what -what takes, from the store's own table.
	if _, stderr, _ := runTx(t, "export", "-h"); !strings.Contains(stderr, "  ledger ") || !strings.Contains(stderr, "  securities ") ||
		!strings.Contains(stderr, "(takes -raw)") {
		t.Errorf("export -h does not list what -what takes:\n%s", stderr)
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
...135   00507V109                   10     81.50     0.00   815.00   -815.00
...135   AAPL                    2.1788    190.20   495.70   414.41     81.29
...135   SWVXX                  1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
...135   VTI                         12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
...398   META                         1    512.00   603.40   512.00     91.40
...398   XYZ   260619P00045000       -2      1.60  -310.00  -320.00     10.00  XYZ 06/19/2026 45.00 P
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
...135   00507V109       10     81.50     0.00   815.00   -815.00
...135   AAPL        2.1788    190.20   495.70   414.41     81.29
...135   SWVXX      1220.89      1.00  1220.89  1220.89      0.00  SCHWAB VALUE ADVANTAGE MONEY FUND
...135   VTI             12  251.3075  3748.92  3015.69    733.23  VANGUARD TOTAL STOCK MARKET ETF
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
...398   META                     1    512.00   603.40   512.00     91.40
...398   XYZ   260619P00045000   -2      1.60  -310.00  -320.00     10.00  XYZ 06/19/2026 45.00 P
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
	if want := `no account matches -account "...999": the login shares ...135 (Individual), ...398 (Options & Margin), ` +
		"...783 (Contributory); to share another, run `schwabtx login` and tick it"; err == nil || err.Error() != want ||
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
		fmt.Fprintf(&manyRows, "...222   XYZ       1      1.00   1.00   1.00      0.00  %d\n", 40-i)
	}
	for range 14 {
		manyRows.WriteString("...222   ZZZ       1      1.00   1.00   1.00      0.00\n")
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
...222   AAPL                     1      1.00   4.00   4.00      0.00
...222   XYZ                      1      1.00   3.00   3.00      0.00  first
...222   XYZ                      1      1.00   5.00   5.00      0.00  second
...222   XYZ   260619P00045000    1      1.00   1.00   1.00      0.00
...222   XYZA                     1      1.00   2.00   2.00      0.00
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
...222   AAPL      1      1.00   4.00  4.00      0.00
...444   ?         -         -      -     -         -
TOTAL                               -     -         -
`,
		},
		{
			"an account that is not an object",
			[]string{holdingAccount(pos("AAPL", 4, "")), `null`},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...222   AAPL      1      1.00   4.00  4.00      0.00
?        ?         -         -      -     -         -
TOTAL                               -     -         -
`,
		},
		{
			"a position that is not an object",
			[]string{holdingAccount(pos("AAPL", 4, "") + `,null`)},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...222   ?         -         -      -     -         -
...222   AAPL      1      1.00   4.00  4.00      0.00
TOTAL                               -     -         -
`,
		},
		{
			"a description with control characters",
			[]string{holdingAccount(pos("AAPL", 4, "APPLE\u001b[2J\u2028INC "+strings.Repeat("x", 40)))},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...222   AAPL      1      1.00   4.00  4.00      0.00  APPLE?[2J INC xxxxxxxxxxxxxxxxxxxxxxx...
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
...222   AAPL                     1      1.00     4.00     4.00      0.00  APPLE INC xxxxxxxxxxxxxxxxxxxxxxxxxxx...
...222   MRVL  250321C00155000   -1    2.0533  -272.50  -205.33    -67.17  ...TECHNOLOGY INC 03/21/2025 $155 Call
TOTAL                                          -268.50  -201.33    -67.17
`,
		},
		{
			// A description is free text, masked as the store masks it.
			"a description that holds an account number",
			[]string{holdingAccount(pos("FUND", 4, "FUND 6180-3398"))},
			`ACCOUNT  SYMBOL  QTY  AVG COST  VALUE  COST  OPEN P/L  DESCRIPTION
...222   FUND      1      1.00   4.00  4.00      0.00  FUND ...398
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
...222   T        10     17.50  250.00     -         -  AT&T INC
...222   VTI       5    246.91       -     -     65.95  VANGUARD TOTAL STOCK MARKET ETF
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
