package mask

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hash is the fake's hash of the account number 29470135
// (schwabtest.HashFor), which holds the run of 8 digits 41224019.
const hash = "742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691"

// Known masks a known number where it stands whole, with no digit beside
// it, whatever its length: its digits in one run, or in groups joined as
// Text joins them, by up to three spaces or a dash with at most one space
// on each side, of any kind; but never within a run of 32 or more hex
// digits, as an account hash is, nor across a date's dashes. So a number
// inside a longer run of digits, one whose groups are joined otherwise, a
// hash that holds one, a date of 1900 to 2099 written YYYY-MM-DD, and a
// number that Known does not hold stay as they are. It holds only numbers
// of 6 digits or more, written with digits alone. What it masks, it masks
// once, the three digits a mask shows counting with the groups after it,
// and Contains finds what it masks.
func TestKnown(t *testing.T) {
	var k Known
	k.Add("29470135", "1234567", "123456789012", "41224019", "654321", "567890", "20261113", "20260230", "21001113", "11131234")
	k.Add("12345", "...135", "2947 0135", "２９４７０１３５", "", "29470135") // ignored, or held already
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"29470135", "...135"},
		{"ACCT29470135 TRF", "ACCT...135 TRF"},
		{"JOURNAL TO 1234567", "JOURNAL TO ...567"},
		{"x123456789012y", "x...012y"},
		{"x654321y", "x...321y"},
		{"29470135-61803398", "...135-61803398"},
		{"29470135 and 1234567", "...135 and ...567"},
		{"1294701350", "1294701350"},
		{"12947013", "12947013"},
		{"12345 and 123456", "12345 and 123456"},
		{"...135", "...135"},
		{"２９４７０１３５", "２９４７０１３５"},
		{"order 1004517688020 of 29470135", "order 1004517688020 of ...135"},

		// In groups that Text joins: spaces or a dash, of any kind.
		{"2947 0135", "...135"},
		{"2947-0135", "...135"},
		{"2947   0135", "...135"},
		{"2947 – 0135", "...135"},
		{"2947—0135", "...135"},
		{"2947 0135", "...135"},
		{"2947\t0135", "...135"},
		{"2 9 4 7 0 1 3 5", "...135"},
		{"Kids 123-4567 gift", "Kids ...567 gift"},
		{"to 1 234 567.", "to ...567."},
		{"ACCT123-4567TRF", "ACCT...567TRF"},
		{"(2947 0135)", "(...135)"},
		{"29 4701 35 77", "...135 77"},
		{"77 29 4701 35", "77 ...135"},
		{"2947 0135 and 123 4567", "...135 and ...567"},
		// Joined otherwise, or with a digit beside the number, they stay.
		{"2947.0135", "2947.0135"},
		{"2947/0135", "2947/0135"},
		{"2947_0135", "2947_0135"},
		{"2947    0135", "2947    0135"},
		{"2947 -  0135", "2947 -  0135"},
		{"2947  -0135", "2947  -0135"},
		{"2947--0135", "2947--0135"},
		{"12947 0135", "12947 0135"},
		{"2947 01350", "2947 01350"},
		{"2947a0135", "2947a0135"},
		{"123 45678", "123 45678"},
		// The three digits that a mask shows count with the groups after it,
		// so that masking again changes nothing: 1234567's mask shows 567,
		// which 890 makes 567890.
		{"1234567 890", "...890"},
		{"123 4567 890", "...890"},
		{"123 4567 89", "...567 89"},

		// A date of the years 1900 to 2099 written YYYY-MM-DD, alone or as
		// a time's date, is a date, whose groups join nothing, though its
		// digits spell a known number; the same digits written otherwise,
		// and what only looks like such a date, are masked.
		{"2026-11-13", "2026-11-13"},
		{"as of 2026-11-13T16:40:00-05:00", "as of 2026-11-13T16:40:00-05:00"},
		{"2026-11-13T21:40:00.000Z", "2026-11-13T21:40:00.000Z"},
		{"from 2026-11-01 to 2026-11-13.", "from 2026-11-01 to 2026-11-13."},
		{"sent 2026-11-13 1234567", "sent 2026-11-13 ...567"},
		{"2026-11-13a", "2026-11-13a"},
		{"20261113", "...113"},
		{"2026 1113", "...113"},
		{"2026-1113", "...113"},
		{"2026 11 13", "...113"},
		{"2026–11–13", "...113"},
		{"2947-01-35", "...135"},
		{"2026-02-30", "...230"},
		{"2100-11-13", "...113"},
		{"2026-11-131234", "2026-...234"},

		// Within a hash, or any run of 32 hex digits or more, a known number
		// stays; in a run of 31, it is masked. A hash's digits join no group.
		{hash, hash},
		{"account " + hash + ".", "account " + hash + "."},
		{strings.Repeat("A", 24) + "29470135", strings.Repeat("A", 24) + "29470135"},
		{"29470135" + strings.Repeat("f", 24), "29470135" + strings.Repeat("f", 24)},
		{strings.Repeat("A", 23) + "29470135", strings.Repeat("A", 23) + "...135"},
		{"G" + strings.Repeat("A", 23) + "29470135", "G" + strings.Repeat("A", 23) + "...135"},
		{strings.Repeat("A", 12) + "-29470135-" + strings.Repeat("A", 20), strings.Repeat("A", 12) + "-...135-" + strings.Repeat("A", 20)},
		{"ab29470135c1234567d", "ab...135c...567d"},
		{strings.Repeat("29470135a", 4), strings.Repeat("29470135a", 4)},
		{hash + " 123 4567", hash + " ...567"},
		{strings.Repeat("A", 31) + "1-234567", strings.Repeat("A", 31) + "1-234567"},
		{strings.Repeat("A", 30) + "1-234567", strings.Repeat("A", 30) + "...567"},
		{"2947-0135" + strings.Repeat("f", 28), "2947-0135" + strings.Repeat("f", 28)},
		{"2947-0135" + strings.Repeat("f", 27), "...135" + strings.Repeat("f", 27)},
	} {
		got := k.Text(tc.in)
		if got != tc.want {
			t.Errorf("Text(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if again := k.Text(got); again != got {
			t.Errorf("Text(%q) = %q, and Text of that = %q", tc.in, got, again)
		}
		if k.Contains(tc.in) != (tc.want != tc.in) {
			t.Errorf("Contains(%q) = %v, want %v", tc.in, k.Contains(tc.in), tc.want != tc.in)
		}
	}
	for _, n := range []string{"29470135", "1234567", "123456789012", "41224019", "654321", "567890", "20261113"} {
		if !k.has(n) {
			t.Errorf("Known does not hold %s, which it was given", n)
		}
	}
	for _, n := range []string{"12345", "...135", "2947 0135", "２９４７０１３５", ""} {
		if k.has(n) {
			t.Errorf("Known holds %q, which is no number of 6 digits or more", n)
		}
	}

	// A Known with no numbers, and a nil one, mask nothing.
	var none Known
	var nilKnown *Known
	for _, k := range []*Known{&none, nilKnown} {
		if got := k.Text("ACCT29470135"); got != "ACCT29470135" || k.Contains("29470135") || k.has("29470135") {
			t.Errorf("an empty Known masks %q, contains %v", got, k.Contains("29470135"))
		}
	}
}

// Known is safe for concurrent use: numbers added while others read are
// masked once added.
func TestKnownConcurrently(t *testing.T) {
	t.Parallel()
	var k Known
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 50 {
				n := strconv.Itoa(10_000_000 + g*1000 + i)
				k.Add(n)
				if got := k.Text("to " + n); got != "to ..."+n[len(n)-3:] {
					t.Errorf("Text of a number just added = %q", got)
					return
				}
				_ = k.Contains("to 12345678")
			}
		})
	}
	wg.Wait()
	if got := k.Text("10007049"); got != "...049" {
		t.Errorf("after the adds, Text(10007049) = %q, want ...049", got)
	}
}

// Text walks a run of hex digits once, however many known numbers it holds:
// over a megabyte of them glued together with a hex letter, one run that
// holds 131,072 numbers, it takes milliseconds, where measuring the whole
// run again for each number took minutes; and as many numbers each in a
// group of its own, which dashes join into one run, are all masked, and so
// are 65,536 numbers each written a digit at a time, a run of a million
// groups that spaces join.
func TestKnownTextTakesLinearTime(t *testing.T) {
	var k Known
	k.Add("29470135")
	glued := strings.Repeat("29470135a", 1<<17)
	apart := strings.Repeat("29470135-", 1<<17)
	spaced := strings.Repeat("2 9 4 7 0 1 3 5 ", 1<<16)
	done := make(chan [3]string, 1)
	go func() { done <- [3]string{k.Text(glued), k.Text(apart), k.Text(spaced)} }()
	select {
	case got := <-done:
		if got[0] != glued {
			t.Errorf("Text of the glued numbers, a hash-like run, changed them")
		}
		if got[1] != strings.Repeat("...135-", 1<<17) {
			t.Errorf("Text of the numbers apart masked some of them only")
		}
		if got[2] != strings.Repeat("...135 ", 1<<16) {
			t.Errorf("Text of the numbers a digit at a time masked some of them only")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Text took more than 10 seconds over a megabyte")
	}
}

// HasNumber judges a JSON number by its value, as JSON does, so that a
// known number written with a fraction or an exponent is still found, and
// finds only the known numbers' values: no other integer, no number with a
// fraction, nothing in a nil Known.
func TestKnownHasNumber(t *testing.T) {
	var k Known
	k.Add("29470135", "1234567")
	for _, tc := range []struct {
		n    json.Number
		want bool
	}{
		{"29470135", true},
		{"-29470135", true},
		{"29470135.0", true},
		{"29470135.000", true},
		{"2.9470135E7", true},
		{"2.9470135e+7", true},
		{"294701350e-1", true},
		{"1234567", true},
		{"1.234567e6", true},
		{"2947013.5", false},
		{"129470135", false},
		{"294701350", false},
		{"29470135.5", false},
		{"2.9470135", false},
		{"0", false},
		{"1e999999", false},
		{"123456", false},
	} {
		if got := k.HasNumber(tc.n); got != tc.want {
			t.Errorf("HasNumber(%s) = %v, want %v", tc.n, got, tc.want)
		}
	}
	var none *Known
	if none.HasNumber("29470135") {
		t.Error("a nil Known has 29470135")
	}
}

// What Text returns holds no known number, by a pattern that finds each of
// them as the leak tests do, outside the runs of 32 hex digits or more and
// the dates of 1900 to 2099 written YYYY-MM-DD, with no digit beside it and
// any of Text's joiners between any two of its digits; masking it again
// changes nothing; and Contains says whether Text masks anything. The
// numbers overlap, so that a mask's three digits and the groups after it
// can spell another, and one of them reads as a date.
func FuzzKnownText(f *testing.F) {
	numbers := []string{"29470135", "1234567", "654321", "567890", "123456789012", "20261113"}
	var k Known
	k.Add(numbers...)
	var patterns []*regexp.Regexp
	for _, n := range numbers {
		patterns = append(patterns, regexp.MustCompile(`(?:^|[^0-9])`+strings.Join(strings.Split(n, ""), sep+`?`)+`(?:[^0-9]|$)`))
	}
	hexRun := regexp.MustCompile(`[0-9A-Fa-f]{32,}`)
	for _, s := range []string{
		"", "2947 0135", "2947-0135", "2 9 4 7 0 1 3 5", "29 4701 35 77", "1234567 890", "123 4567 890", "12 3456 7890",
		"2947\u00a00135", "2947 \u2013 0135", "Kids 123-4567 gift", hash + " 123 4567", strings.Repeat("A", 31) + "1-234567",
		"2947-0135" + strings.Repeat("f", 27), "6 54321 567890 1234567 890", "1-2-3-4-5-6-7-8-9-0-1-2",
		"2026-11-13", "2026-11-13 2026-11-13T16:40:00", "2026 11 13", "2026-11-131", "12026-11-13 567 890",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := k.Text(s)
		outside := undated(hexRun.ReplaceAllString(got, "#"))
		for i, p := range patterns {
			if p.MatchString(outside) {
				t.Fatalf("Text(%q) = %q, which holds %s", s, got, numbers[i])
			}
		}
		if again := k.Text(got); again != got {
			t.Fatalf("Text(%q) = %q, and Text of that = %q", s, got, again)
		}
		if k.Contains(s) != (got != s) {
			t.Fatalf("Contains(%q) = %v, but Text gives %q", s, k.Contains(s), got)
		}
	})
}

// undated returns s with each date that Known keeps whole replaced by #:
// a valid date of 1900 to 2099 written YYYY-MM-DD, with no digit before or
// after it.
func undated(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		d := s[i:]
		if len(d) >= 10 && (i == 0 || !isDigit(s[i-1])) && (len(d) == 10 || !isDigit(d[10])) &&
			(d[:2] == "19" || d[:2] == "20") {
			if _, err := time.Parse(time.DateOnly, d[:10]); err == nil {
				b.WriteByte('#')
				i += 9
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
