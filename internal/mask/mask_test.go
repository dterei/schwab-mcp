package mask

import (
	"regexp"
	"strings"
	"testing"
)

// A number shows as Schwab's displayAcctId shows it: "..." and its last
// three characters, and no more of a number of three or fewer.
func TestMaskNumber(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"29470135", "...135"},
		{"123456789012", "...012"},
		{"1234", "...234"},
		{"123", "..."},
		{"12", "..."},
		{"", "..."},
		{"ab€d", "...b€d"},
	} {
		if got := Number(tc.in); got != tc.want {
			t.Errorf("Number(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Text masks each run of 8 or more digits, joined by up to three spaces or
// by a dash with at most a space on each side, to "..." and its last three
// digits, and leaves the rest of the text as it was: shorter runs, such as
// a check number, a date written with slashes or an amount, and digits
// that a dot, a slash, an underscore, two dashes or four spaces keep apart.
// A run ends at the group that brings it to 8 digits, so the number shows
// its own last three digits and a number after it stays, unless that
// group is short and, with the next, may be a number in two parts. A date
// written with hyphens holds 8 digits, and is masked.
func TestMaskText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"JOURNAL TO 12345678", "JOURNAL TO ...678"},
		{"1234-5678", "...678"},
		{"1234 5678", "...678"},
		{"12-34-56-78", "...678"},
		{"1 2 3 4 5 6 7 8", "...678"},
		{"123456789012", "...012"},
		{"Check 123456 paid", "Check 123456 paid"},
		{"1234567", "1234567"},
		{"123-4567", "123-4567"},
		{"TRANSFER 2947-0135 TO 6180 3398.", "TRANSFER ...135 TO ...398."},
		{"12345678 and 87654321", "...678 and ...321"},

		// The separators that join two groups.
		{"Acct 1234  5678", "Acct ...678"},
		{"Acct 1234   5678", "Acct ...678"},
		{"Acct 1234    5678", "Acct 1234    5678"},
		{"Acct 1234\t5678", "Acct ...678"},
		{"Acct 1234\u00a05678", "Acct ...678"},
		{"Acct 1234\u20035678", "Acct ...678"},
		{"Acct 1234–5678", "Acct ...678"},
		{"Acct 1234—5678", "Acct ...678"},
		{"Acct 1234 - 5678", "Acct ...678"},
		{"Joint 1234 – 5678", "Joint ...678"},
		{"Acct 1234 -5678", "Acct ...678"},
		{"Acct 1234-\u00a05678", "Acct ...678"},
		{"Acct 1234  - 5678", "Acct 1234  - 5678"},
		{"Acct 1234 -  5678", "Acct 1234 -  5678"},
		{"1234--5678", "1234--5678"},
		{"1234 - - 5678", "1234 - - 5678"},
		{"1234.5678", "1234.5678"},
		{"1234/5678", "1234/5678"},
		{"1234_5678", "1234_5678"},
		{"1234,5678", "1234,5678"},
		{"1234\u22125678", "1234\u22125678"},

		// A number beside another: the run ends at the group that brings
		// it to 8 digits.
		{"JOURNAL TO 12345678 100 SHS", "JOURNAL TO ...678 100 SHS"},
		{"TRANSFER FROM 2947-0135 1 OF 2", "TRANSFER FROM ...135 1 OF 2"},
		{"TRANSFER 2947-0135 100 SHS", "TRANSFER ...135 100 SHS"},
		{"Ref 12345678-1", "Ref ...678-1"},
		{"12345678 1234", "...678 1234"},
		{"12345678 1 and 12345", "...678 1 and 12345"},
		{"12345678 12345", "...678 ...345"},
		{"12345678 1234-5678", "...678 ...678"},
		{"12345678 1 2 3 4 5", "...678 ...345"},
		{"12345678 1 2 3 4", "...678 1 2 3 4"},
		{"100 12345678", "...678"},
		{"1000 2947-0135", "...135"},
		{"2026 1234 5678", "...678"},
		{"1234567 1234-5678", "...678"},
		{"2947-0135 1000", "...000"},
		{"1000 12345-678", "...678"},
		{"1000 1234567 8", "...678"},
		{"BANK INT 031623-041523 SCHWAB BANK", "BANK INT ...523 SCHWAB BANK"},

		{"-12345678-", "-...678-"},
		{" 12345678 ", " ...678 "},
		{"#12345678", "#...678"},
		{"x12345678y", "x...678y"},
		{"...12345678", "......678"},
		{"Dividend 11/11/2026", "Dividend 11/11/2026"},
		{"Dividend 2026-11-11", "Dividend ...111"},
		{"2026-11-11 12345", "...111 ...345"},
		{"2026-11-11 1234567", "...567"},
		{"2026-11-11 1234", "...111 1234"},
		{"$1,234,567.89", "$1,234,567.89"},
		{"１２３４５６７８", "１２３４５６７８"},
		{"Café 12345678 ü", "Café ...678 ü"},
		{"\xff12345678\xfe", "\xff...678\xfe"},
		{"1234 \xff5678", "1234 \xff5678"},
	} {
		if got := Text(tc.in); got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// FileName keeps the export stamp of a name that schwab.com gives a file,
// and the masked digits in it, and masks the rest as Text does: a nickname
// that holds 8 digits, or a stamp that is not a valid date and time.
func TestMaskFileName(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Individual_XXX398_Transactions_20261111-120000.json", "Individual_XXX398_Transactions_20261111-120000.json"},
		{"XXXX1234_GainLoss_Realized_Details_20260706-104711.csv", "XXXX1234_GainLoss_Realized_Details_20260706-104711.csv"},
		{"Brokerage_Transactions_20201115-180021.CSV", "Brokerage_Transactions_20201115-180021.CSV"},
		{"Joint 12345678_XXX398_Transactions_20261111-120000.csv", "Joint ...678_XXX398_Transactions_20261111-120000.csv"},
		{"Joint 1234-5678_XXX398_Transactions_20261111-120000", "Joint ...678_XXX398_Transactions_20261111-120000"},
		{"Individual_XXX398_Transactions_20261341-250000.json", "Individual_XXX398_Transactions_...341-...000.json"},
		{"Individual_XXX398_Transactions_20261111-120060.json", "Individual_XXX398_Transactions_...111-...060.json"},
		{"Individual_XXX398_Transactions_20261111-120000 (1).json", "Individual_XXX398_Transactions_...111-...000 (1).json"},
		{"Individual_XXX398_Transactions_20261111-120000.12345678", "Individual_XXX398_Transactions_...111-...000....678"},
		{"history-12345678.csv", "history-...678.csv"},
		{"XXX398 12345_Transactions_20261111-120000.csv", "XXX...345_Transactions_20261111-120000.csv"},
		{"snapshots.jsonl", "snapshots.jsonl"},
		{"", ""},
	} {
		if got := FileName(tc.in); got != tc.want {
			t.Errorf("FileName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// space is a space of any kind, as unicode.IsSpace has it, and sep a
// separator that joins two groups of digits, as Text reads one.
const (
	space = `[\t\n\v\f\r\x{85}\p{Z}]`
	sep   = `(?:` + space + `{1,3}|` + space + `?\p{Pd}` + space + `?)`
)

// chain is groups of digits joined as Text joins them, as many as there
// are, and group one group.
var (
	chain = regexp.MustCompile(`[0-9]+(?:` + sep + `[0-9]+)*`)
	group = regexp.MustCompile(`[0-9]+`)
)

// longRun returns the first chain in s of 8 or more digits, or "".
func longRun(s string) string {
	for _, c := range chain.FindAllString(s, -1) {
		if len(strings.Join(group.FindAllString(c, -1), "")) >= 8 {
			return c
		}
	}
	return ""
}

// textByRegexp is Text written another way, with regular expressions: it
// cuts each chain of groups into runs, a group at a time.
func textByRegexp(s string) string {
	return chain.ReplaceAllStringFunc(s, func(c string) string {
		gs := group.FindAllStringIndex(c, -1)
		var b strings.Builder
		kept, from, digits := 0, 0, 0 // c[:kept] is in b; the run starts at gs[from]
		for k, g := range gs {
			n := g[1] - g[0]
			digits += n
			if digits < 8 || k+1 < len(gs) && n < 8 && n+gs[k+1][1]-gs[k+1][0] >= 8 {
				continue
			}
			run := strings.Map(func(r rune) rune {
				if r < '0' || r > '9' {
					return -1
				}
				return r
			}, c[gs[from][0]:g[1]])
			b.WriteString(c[kept:gs[from][0]] + "..." + run[len(run)-3:])
			kept, from, digits = g[1], k+1, 3
		}
		return b.String() + c[kept:]
	})
}

// Text of any string is what textByRegexp makes of it, holds no run of 8
// digits and is its own mask; so is FileName, once the stamp it keeps is
// left out. An account number, whole or in two parts, shows no more than
// its last three digits, whatever digits are beside it.
func FuzzMaskText(f *testing.F) {
	for _, s := range []string{
		"", "JOURNAL TO 12345678", "1234-5678", "Check 123456", "1 2 3 4 5 6 7 8 9", "1234  5678", "--12-34-56-78--",
		"Joint 12345678_XXX398_Transactions_20261111-120000.csv", "x_20261341-250000.csv", "\xff1234 5678\x00",
		"JOURNAL TO 12345678 100 SHS", "1000 2947-0135", "2947-0135 1 OF 2", "1234 \u2013 5678", "1234\u00a05678",
		"2026-11-11 12345", "12345678 12345 6789",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := Text(s)
		if want := textByRegexp(s); got != want {
			t.Fatalf("Text(%q) = %q, textByRegexp = %q", s, got, want)
		}
		if run := longRun(got); run != "" {
			t.Fatalf("Text(%q) = %q, which holds %q", s, got, run)
		}
		if again := Text(got); again != got {
			t.Fatalf("Text(%q) = %q, and Text of that = %q", s, got, again)
		}

		if !strings.Contains(s, "9") {
			for cut := range 8 {
				n := "99999999"
				if cut > 0 {
					n = n[:cut] + "-" + n[cut:]
				}
				in := s + " " + n + " " + s
				if got := Text(in); strings.Count(got, "9") > 3 {
					t.Fatalf("Text(%q) = %q, which shows more than 3 digits of %s", in, got, n)
				}
			}
		}

		name := FileName(s)
		if again := FileName(name); again != name {
			t.Fatalf("FileName(%q) = %q, and FileName of that = %q", s, name, again)
		}
		rest := name
		if m := stamp.FindStringIndex(name); m != nil {
			rest = name[:m[0]] + "_" + name[m[1]:]
		}
		if run := longRun(rest); run != "" {
			t.Fatalf("FileName(%q) = %q, which holds %q besides its stamp", s, name, run)
		}
	})
}
