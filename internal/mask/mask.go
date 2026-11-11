// Package mask hides account numbers in what schwabtx shows: a number
// itself, as Schwab masks it in displayAcctId, and the digits of free text
// that may hold one, such as a transaction's description, a nickname, a
// note or Schwab's error text.
//
// Free text is masked by pattern, without a list of known numbers, so
// masking a piece of text depends on that text alone. Schwab's account
// numbers have 8 digits, and a run of 8 or more holds as many digits as a
// whole number. Text that the program composes, such as a label or an
// error, is composed from pieces already masked and from identifiers that
// cannot be account numbers, and is never masked as a whole, which would
// also mask the identifiers: an activity ID, a date and time, or an amount.
package mask

import (
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Number returns n as Schwab masks an account number: "..." and its last
// three characters, or "..." alone when it has three or fewer.
func Number(n string) string {
	r := []rune(n)
	if len(r) <= 3 {
		return "..."
	}
	return "..." + string(r[len(r)-3:])
}

// Text masks free text, so that an account number in it shows as Number
// shows it. Groups of ASCII digits joined by a separator, one to three
// spaces of any kind or a dash of any kind with at most one space on each
// side, make a run. A run ends at the group that brings it to 8 or more
// digits, and becomes "..." and its last three digits; but while the group
// it ends at has fewer than 8 digits, and 8 or more with the next, the next
// joins it, since the two may be one number written in two parts. A group
// after a masked run starts a new run, in which the three digits that the
// mask shows count. So "JOURNAL TO 12345678 100 SHS" reads
// "JOURNAL TO ...678 100 SHS", "1234-5678" and "1234 – 5678" read "...678",
// and a check number of six digits stays. A dot, a slash, a comma or an
// underscore joins nothing, so that amounts and dates written with slashes
// stay. What Text returns holds no run of 8 digits, and masking it again
// changes nothing.
func Text(s string) string {
	var b strings.Builder
	done := 0  // s[:done] is in b, or needs no masking
	carry := 0 // digits that a mask just before s[i] shows, joined to it
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		start, digits := i, carry
		carry = 0
		end := i // the run is s[start:end]
		for {
			j := end
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			group := j - end // the digits of the run's last group
			digits, end = digits+group, j
			k := joiner(s[end:])
			if k == 0 {
				break
			}
			// At 8 digits the run ends, unless its last group is short and
			// may be one number with the next, written in two parts.
			if digits >= 8 && (group >= 8 || group+groupLen(s[end+k:]) < 8) {
				break
			}
			end += k
		}
		if digits >= 8 {
			b.WriteString(s[done:start])
			b.WriteString("...")
			b.WriteString(lastDigits(s[start:end], 3))
			done = end
			if k := joiner(s[end:]); k > 0 {
				i, carry = end+k, 3
				continue
			}
		}
		i = end
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// joiner returns the length of the separator at the start of s that joins
// two groups of digits, or 0 when s does not start with one followed by a
// digit: one to three spaces, or a dash with at most one space on each
// side, spaces and dashes of any kind. s starts after a group's last digit.
func joiner(s string) int {
	i, spaces := 0, 0
	for spaces < 3 {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i, spaces = i+n, spaces+1
	}
	if spaces <= 1 {
		if r, n := utf8.DecodeRuneInString(s[i:]); unicode.Is(unicode.Pd, r) {
			i += n
			if r, n := utf8.DecodeRuneInString(s[i:]); unicode.IsSpace(r) {
				i += n
			}
		}
	}
	if i < len(s) && isDigit(s[i]) {
		return i
	}
	return 0
}

// groupLen returns the number of digits at the start of s.
func groupLen(s string) int {
	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}
	return n
}

// stamp is the end of the name of a file that schwab.com exports: an
// underscore, the time of the export, and an extension of letters.
var stamp = regexp.MustCompile(`_([0-9]{8}-[0-9]{6})(\.[A-Za-z]+)?$`)

// FileName masks a file name: its free text through Text, but Schwab's
// export stamp (_YYYYMMDD-HHMMSS before the extension, a valid date and
// time) is kept, and so are its masked digits (XXX398, XXXX1234), since
// Text keeps a run of fewer than 8 digits. So
// "Individual_XXX398_Transactions_20261111-120000.json" stays as it is and
// "Joint 12345678_XXX398_Transactions_20261111-120000.csv" reads
// "Joint ...678_XXX398_Transactions_20261111-120000.csv". A stamp that is
// not a valid date and time is text like the rest.
func FileName(name string) string {
	m := stamp.FindStringSubmatchIndex(name)
	if m == nil {
		return Text(name)
	}
	if _, err := time.Parse("20060102-150405", name[m[2]:m[3]]); err != nil {
		return Text(name)
	}
	return Text(name[:m[0]]) + name[m[0]:]
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// lastDigits returns the last n digits of s, in order, leaving out
// anything else.
func lastDigits(s string, n int) string {
	var d []byte
	for i := len(s) - 1; i >= 0 && len(d) < n; i-- {
		if isDigit(s[i]) {
			d = append(d, s[i])
		}
	}
	for i, j := 0, len(d)-1; i < j; i, j = i+1, j-1 {
		d[i], d[j] = d[j], d[i]
	}
	return string(d)
}
