package mask

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// hash is the fake's hash of the account number 29470135
// (schwabtest.HashFor), which holds the run of 8 digits 41224019.
const hash = "742C51B544655B85F41224019A82CD7AB3C6BABDB67BF80531E82B306CB5F691"

// Known masks a known number where it stands whole, a run of digits with no
// digit beside it, whatever its length, but never within a run of 32 or
// more hex digits, as an account hash is: so a number inside a longer run
// of digits, a hash that holds one, and a number that Known does not hold
// stay as they are. It holds only numbers of 6 digits or more, written
// with digits alone. What it masks, it masks once, and Contains finds what
// it masks.
func TestKnown(t *testing.T) {
	var k Known
	k.Add("29470135", "1234567", "123456789012", "41224019")
	k.Add("12345", "...135", "2947 0135", "２９４７０１３５", "", "29470135") // ignored, or held already
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"29470135", "...135"},
		{"ACCT29470135 TRF", "ACCT...135 TRF"},
		{"JOURNAL TO 1234567", "JOURNAL TO ...567"},
		{"x123456789012y", "x...012y"},
		{"29470135-61803398", "...135-61803398"},
		{"29470135 and 1234567", "...135 and ...567"},
		{"1294701350", "1294701350"},
		{"12947013", "12947013"},
		{"12345 and 123456", "12345 and 123456"},
		{"2947 0135", "2947 0135"},
		{"...135", "...135"},
		{"２９４７０１３５", "２９４７０１３５"},
		{"order 1004517688020 of 29470135", "order 1004517688020 of ...135"},

		// Within a hash, or any run of 32 hex digits or more, a known number
		// stays; in a run of 31, it is masked.
		{hash, hash},
		{"account " + hash + ".", "account " + hash + "."},
		{strings.Repeat("A", 24) + "29470135", strings.Repeat("A", 24) + "29470135"},
		{"29470135" + strings.Repeat("f", 24), "29470135" + strings.Repeat("f", 24)},
		{strings.Repeat("A", 23) + "29470135", strings.Repeat("A", 23) + "...135"},
		{"G" + strings.Repeat("A", 23) + "29470135", "G" + strings.Repeat("A", 23) + "...135"},
		{strings.Repeat("A", 12) + "-29470135-" + strings.Repeat("A", 20), strings.Repeat("A", 12) + "-...135-" + strings.Repeat("A", 20)},
		{"ab29470135c1234567d", "ab...135c...567d"},
		{strings.Repeat("29470135a", 4), strings.Repeat("29470135a", 4)},
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
	for _, n := range []string{"29470135", "1234567", "123456789012", "41224019"} {
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
// run of its own are all masked.
func TestKnownTextTakesLinearTime(t *testing.T) {
	var k Known
	k.Add("29470135")
	glued := strings.Repeat("29470135a", 1<<17)
	apart := strings.Repeat("29470135-", 1<<17)
	done := make(chan [2]string, 1)
	go func() { done <- [2]string{k.Text(glued), k.Text(apart)} }()
	select {
	case got := <-done:
		if got[0] != glued {
			t.Errorf("Text of the glued numbers, a hash-like run, changed them")
		}
		if got[1] != strings.Repeat("...135-", 1<<17) {
			t.Errorf("Text of the numbers apart masked some of them only")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Text took more than 10 seconds over a megabyte")
	}
}
