package history

import (
	"strings"
	"unicode"
)

// SelectorDigits returns the digits that an account selector, such as
// -account's value, names an account by, and whether it names one by
// digits at all: digits, with or without a leading "...", as a number is
// masked, and with dashes or spaces of any kind among them, as schwab.com
// writes a number, name the account whose number ends in them, or whose
// number they are. Anything else names an account by its nickname
// (SameNickname). schwabtx reads -account this way in every command.
func SelectorDigits(sel string) (string, bool) {
	digits := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) {
			return -1
		}
		return r
	}, strings.TrimPrefix(strings.TrimSpace(sel), "..."))
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return "", false
	}
	return digits, true
}

// SameNickname reports whether two nicknames are one as an account
// selector compares them: with no white space at their ends and a run of
// it within as one space, and whatever their case.
func SameNickname(a, b string) bool {
	return strings.EqualFold(strings.Join(strings.Fields(a), " "), strings.Join(strings.Fields(b), " "))
}
