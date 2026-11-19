package lookup

import (
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// named returns an account of each number, a cash account with nothing
// else sent, nicknamed as nicknames gives, in pairs of a number and its
// nickname.
func named(t *testing.T, numbersAndNicknames ...string) []Named {
	t.Helper()
	var as []Named
	for i := 0; i+1 < len(numbersAndNicknames); i += 2 {
		as = append(as, Named{
			Account:  account(t, `{"securitiesAccount":{"type":"CASH","accountNumber":"`+numbersAndNicknames[i]+`"}}`),
			Nickname: numbersAndNicknames[i+1],
		})
	}
	return as
}

// numbers returns the account numbers of as, in order.
func numbers(as []Named) []string {
	var ns []string
	for _, a := range as {
		ns = append(ns, a.Account.SecuritiesAccount.AccountNumber)
	}
	return ns
}

// A selector picks one account, by the digits that its number ends in or
// by its nickname, as sent or as the tables show it; and else says why it
// picked none, with the accounts that it matches. It never guesses.
func TestPick(t *testing.T) {
	four := named(t, "29470135", "Individual", "61803398", "Options & Margin", "40221783", "Contributory", "12345678", "Joint 6180 3398")
	for _, tc := range []struct {
		name, selector string
		as             []Named
		picked         []string // the numbers of the accounts picked
		kind           PickKind // or 0 for none
		matches        []string
	}{
		{"none named", "", four, numbers(four), 0, nil},
		{"none named, given as spaces", "  ", four, numbers(four), 0, nil},
		{"last digits", "135", four, []string{"29470135"}, 0, nil},
		{"one last digit", "3", four, []string{"40221783"}, 0, nil},
		{"masked digits", "...783", four, []string{"40221783"}, 0, nil},
		{"digits with a dash", "4022-1783", four, []string{"40221783"}, 0, nil},
		{"digits with spaces", " 4022 1783 ", four, []string{"40221783"}, 0, nil},
		{"a whole number", "61803398", four, []string{"61803398"}, 0, nil},
		{"a nickname in another case", "individual", four, []string{"29470135"}, 0, nil},
		{"a nickname with other spaces", "  options \t& MARGIN ", four, []string{"61803398"}, 0, nil},
		{"a nickname as sent", "Joint 6180 3398", four, []string{"12345678"}, 0, nil},
		{"a nickname as the tables show it", "joint ...398", four, []string{"12345678"}, 0, nil},

		{"digits of no account", "9999", four, nil, NoMatch, nil},
		{"a nickname of no account", "Roth", four, nil, NoMatch, nil},
		{"dots alone", "...", four, nil, NoMatch, nil},
		{"digits of an account without a number", "8", named(t, "", "Joint"), nil, NoMatch, nil},
		{"no accounts", "135", nil, nil, NoMatch, nil},

		{"digits of two nicknamed accounts", "8", four, nil, AmbiguousNicknamedDigits, []string{"61803398", "12345678"}},
		{
			"digits of two accounts, one without a nickname", "8",
			named(t, "61803398", "Options & Margin", "12345678", " "), nil, AmbiguousDigits,
			[]string{"61803398", "12345678"},
		},
		{
			"digits of two accounts of one nickname", "8",
			named(t, "61803398", "Joint", "12345678", "JOINT "), nil, AmbiguousDigits,
			[]string{"61803398", "12345678"},
		},
		{
			"a nickname of two accounts", "joint",
			named(t, "29470135", "Joint", "61803398", "Options & Margin", "40221783", " joint"), nil,
			AmbiguousNickname,
			[]string{"29470135", "40221783"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ParseSelector(tc.selector)
			got, perr := s.Pick(tc.as)
			if !slices.Equal(numbers(got), tc.picked) {
				t.Errorf("picked %q, want %q", numbers(got), tc.picked)
			}
			switch {
			case tc.kind == 0 && perr != nil:
				t.Errorf("an error of kind %d with %q", perr.Kind, numbers(perr.Matches))
			case tc.kind != 0 && perr == nil:
				t.Errorf("no error, want one of kind %d", tc.kind)
			case tc.kind != 0 && (perr.Kind != tc.kind || !slices.Equal(numbers(perr.Matches), tc.matches)):
				t.Errorf("an error of kind %d with %q, want kind %d with %q", perr.Kind, numbers(perr.Matches), tc.kind, tc.matches)
			}
		})
	}
}

// A selector reads digits, with or without "...", dashes or spaces among
// them, as digits, and anything else as a nickname; an error shows it
// with no more than the last 3 digits of any run of more than 3.
func TestParseSelector(t *testing.T) {
	for _, tc := range []struct {
		given                   string
		digits, nickname, shown string
	}{
		{"", "", "", ""},
		{" 135 ", "135", "", "135"},
		{"9999", "9999", "", "...999"},
		{"61803399", "61803399", "", "...399"},
		{"...3398", "3398", "", "...398"},
		{"4022-1783", "40221783", "", "...783"},
		{"4022–1783", "40221783", "", "...783"},
		{"...", "", "...", "..."},
		{"Roth", "", "Roth", "Roth"},
		{"Nope 6180 3398", "", "Nope 6180 3398", "Nope ...398"},
		{"Trust 12", "", "Trust 12", "Trust 12"},
		{"acct12345678x", "", "acct12345678x", "acct...678x"},
	} {
		s := ParseSelector(tc.given)
		if s.Given != strings.TrimSpace(tc.given) || s.Digits != tc.digits || s.Nickname != tc.nickname || s.Shown() != tc.shown {
			t.Errorf("ParseSelector(%q) = %+v, shown %q; want digits %q, nickname %q, shown %q",
				tc.given, s, s.Shown(), tc.digits, tc.nickname, tc.shown)
		}
	}
}

// Each account gets the nickname of the first account of the preferences
// with its number; an account that they leave out, or one without a
// number, gets none.
func TestNameAccounts(t *testing.T) {
	as := named(t, "29470135", "old", "61803398", "old", "40221783", "old", "", "old")
	prefs := schwab.UserPreference{Accounts: []schwab.PreferenceAccount{
		{AccountNumber: "61803398", NickName: "Options & Margin"},
		{AccountNumber: "61803398", NickName: "a second"},
		{AccountNumber: "29470135", NickName: "Individual"},
		{AccountNumber: "12345678", NickName: "not shared"},
		{AccountNumber: "", NickName: "no number"},
	}}
	NameAccounts(as, prefs)
	var got []string
	for _, a := range as {
		got = append(got, a.Nickname)
	}
	if want := []string{"Individual", "Options & Margin", "", "old"}; !slices.Equal(got, want) {
		t.Errorf("nicknames %q, want %q", got, want)
	}
}
