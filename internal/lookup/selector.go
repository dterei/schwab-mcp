package lookup

import (
	"strings"
	"unicode"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// Named is an account from GET /accounts and its nickname from the user's
// preferences: "" when it has none, or they were not read. The account is
// a named field, as the package doc of internal/schwab asks.
type Named struct {
	Account  schwab.Account
	Nickname string
}

// NameAccounts gives each of as the nickName of the first account of prefs
// with its number. The preferences may list accounts that were not shared
// at login, which is not known, so an account that GET /accounts did not
// list gets no row of its own; an account that the preferences leave out
// gets no nickname.
func NameAccounts(as []Named, prefs schwab.UserPreference) {
	nicknames := map[string]string{}
	for _, p := range prefs.Accounts {
		if _, seen := nicknames[p.AccountNumber]; p.AccountNumber != "" && !seen {
			nicknames[p.AccountNumber] = p.NickName
		}
	}
	for i := range as {
		if n := as[i].Account.SecuritiesAccount.AccountNumber; n != "" {
			as[i].Nickname = nicknames[n]
		}
	}
}

// NicknameProblems returns the problems of prefs that can cost an account
// its nickname: those at an account's number or nickname, or above them. A
// change to the streamer's details or to the offers costs none.
func NicknameProblems(prefs schwab.UserPreference) []schwab.Problem {
	var ps []schwab.Problem
	for _, p := range prefs.Problems {
		if Covers(p, "accounts.nickName") || Covers(p, "accounts.accountNumber") {
			ps = append(ps, p)
		}
	}
	return ps
}

// A Selector is the account that a value such as -account's names: by the
// digits that its number ends in, or by its nickname. All three are empty
// when it names none.
type Selector struct{ Given, Digits, Nickname string }

// ParseSelector reads a value that names an account as every command of
// schwabtx reads -account's (history.SelectorDigits). Digits, with or
// without a leading "...", as its tables and Schwab mask numbers, and with
// dashes or spaces of any kind among them, as schwab.com writes a number,
// name the account whose number ends in them; a whole number names its
// account too. Anything else names the account with that nickname, as
// sameName compares them, so a nickname of digits alone cannot be given,
// though its account's number can.
func ParseSelector(v string) Selector {
	v = strings.TrimSpace(v)
	s := Selector{Given: v}
	if digits, ok := history.SelectorDigits(v); ok {
		s.Digits = digits
	} else {
		s.Nickname = v
	}
	return s
}

// Shown returns the value as an error shows it: as given, but with no more
// of an account number than schwabtx's tables show. Each run of more than
// 3 digits, which may be part of a number, shows as ... and its last 3, as
// the tables mask a number, whether the value names digits or a nickname.
// A run goes on across anything but a letter, so that the digits of a
// number written with dots, dashes or spaces of any kind among them make
// one run.
func (s Selector) Shown() string {
	rs := []rune(s.Given)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if !unicode.IsDigit(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		var digits []rune
		end := i // just after the run's last digit
		for j := i; j < len(rs) && !unicode.IsLetter(rs[j]); j++ {
			if unicode.IsDigit(rs[j]) {
				digits, end = append(digits, rs[j]), j+1
			}
		}
		switch {
		case len(digits) <= 3:
			b.WriteString(string(rs[i:end]))
		case strings.HasSuffix(b.String(), "..."): // masked already, as the tables mask
			b.WriteString(string(digits[len(digits)-3:]))
		default:
			b.WriteString(mask.Number(string(digits)))
		}
		i = end
	}
	return b.String()
}

// PickKind says why Pick picked no account.
type PickKind int

const (
	// NoMatch is a selector that no account matches.
	NoMatch PickKind = iota + 1
	// AmbiguousNickname is a nickname that several accounts have, as Pick
	// compares nicknames, which their numbers tell apart.
	AmbiguousNickname
	// AmbiguousNicknamedDigits is digits that the numbers of several
	// accounts end in, each with a nickname that no other of them has,
	// which tells them apart.
	AmbiguousNicknamedDigits
	// AmbiguousDigits is digits that the numbers of several accounts end
	// in, which their nicknames do not tell apart: more digits do.
	AmbiguousDigits
)

// A PickError is why Pick picked no account: its kind, and the accounts
// that the selector matches, in the order given, of which there are none
// for NoMatch. It is data for the caller to word, and not an error.
type PickError struct {
	Kind    PickKind
	Matches []Named
}

// Pick returns the accounts of as that s names: all of them when it names
// none, and else the one whose number ends in its digits, or whose
// nickname is the same name as its own, as Schwab sent it or as schwabtx's
// tables show it (mask.Text). It never guesses: no account, or several, is
// a PickError, which says which.
func (s Selector) Pick(as []Named) ([]Named, *PickError) {
	if s.Given == "" {
		return as, nil
	}
	var got []Named
	for _, a := range as {
		n := a.Account.SecuritiesAccount.AccountNumber
		// A nickname is the one Schwab sent, or as the tables show it.
		if s.Digits != "" && n != "" && strings.HasSuffix(n, s.Digits) ||
			s.Nickname != "" && (sameName(a.Nickname, s.Nickname) || sameName(mask.Text(a.Nickname), mask.Text(s.Nickname))) {
			got = append(got, a)
		}
	}
	switch {
	case len(got) == 1:
		return got, nil
	case len(got) == 0:
		return nil, &PickError{Kind: NoMatch}
	case s.Nickname != "":
		return nil, &PickError{Kind: AmbiguousNickname, Matches: got}
	case nicknamed(got):
		return nil, &PickError{Kind: AmbiguousNicknamedDigits, Matches: got}
	}
	return nil, &PickError{Kind: AmbiguousDigits, Matches: got}
}

// nicknamed reports whether each of as has a nickname, and no two the same
// as a selector compares them, so that a nickname picks any one of them.
func nicknamed(as []Named) bool {
	for i, a := range as {
		if strings.TrimSpace(a.Nickname) == "" {
			return false
		}
		for _, b := range as[:i] {
			if sameName(a.Nickname, b.Nickname) {
				return false
			}
		}
	}
	return true
}

// sameName reports whether two nicknames are the same as a selector
// compares them in every command (history.SameNickname): as the tables and
// the errors show a nickname, with no white space at its ends and a run of
// it within as one space, and whatever their case.
func sameName(a, b string) bool { return history.SameNickname(a, b) }
