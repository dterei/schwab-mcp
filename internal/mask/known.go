package mask

import (
	"iter"
	"strings"
	"sync"
)

// minKnown is the fewest digits that a number Known holds has: a shorter
// one would mask too much that is no account number.
const minKnown = 6

// hashRun is the fewest hex digits in a run that Known takes for an account
// hash, whose digits it never masks: Schwab's hashes have 64.
const hashRun = 32

// Known is the set of full account numbers that a process has seen, which
// it masks wherever they stand: a second net beside Text, which masks by
// pattern alone and so misses a number of fewer than 8 digits, and which
// is never run over identifiers. It is safe for concurrent use. Its zero
// value is an empty set, and so is a nil *Known, which only reads.
type Known struct {
	mu      sync.RWMutex
	numbers map[string]bool
}

// Add adds numbers to the set, each a string of 6 or more ASCII digits; it
// ignores any other string, such as a shorter one or a masked number.
func (k *Known) Add(numbers ...string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, n := range numbers {
		if len(n) < minKnown || strings.Trim(n, "0123456789") != "" {
			continue
		}
		if k.numbers == nil {
			k.numbers = map[string]bool{}
		}
		k.numbers[n] = true
	}
}

// Text returns s with each known number in it shown as Number shows it: each
// run of digits, not preceded or followed by a digit, that is a known number,
// but within a run of 32 or more hex digits, which is an account hash, so
// that a hash stays whole. With 29470135 known, "ACCT29470135 TRF" reads
// "ACCT...135 TRF", while "1294701350" and a hash that holds 29470135 stay
// as they are. What Text returns holds no known number, and masking it again
// changes nothing.
func (k *Known) Text(s string) string {
	if k == nil {
		return s
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if len(k.numbers) == 0 {
		return s
	}
	var b strings.Builder
	done := 0 // s[:done] is in b, or needs no masking
	for i, j := range k.runs(s) {
		b.WriteString(s[done:i])
		b.WriteString(Number(s[i:j]))
		done = j
	}
	if done == 0 {
		return s
	}
	b.WriteString(s[done:])
	return b.String()
}

// Contains reports whether s holds a known number that Text would mask.
func (k *Known) Contains(s string) bool {
	if k == nil {
		return false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	for range k.runs(s) {
		return true
	}
	return false
}

// has reports whether n, a string of digits, is a known number.
func (k *Known) has(n string) bool {
	if k == nil {
		return false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.numbers[n]
}

// runs yields the start and end of each run of digits in s that Text masks:
// a known number, in a run of hex digits shorter than a hash's. Digits are
// hex digits, so each run of digits lies in one run of hex digits, which is
// measured once, however many numbers it holds. The caller holds k's lock.
func (k *Known) runs(s string) iter.Seq2[int, int] {
	return func(yield func(int, int) bool) {
		for i := 0; i < len(s); {
			if !isHex(s[i]) {
				i++
				continue
			}
			end := i // the run of hex digits is s[i:end]
			for end < len(s) && isHex(s[end]) {
				end++
			}
			for j := i; end-i < hashRun && j < end; {
				if !isDigit(s[j]) {
					j++
					continue
				}
				m := j // the run of digits is s[j:m]
				for m < end && isDigit(s[m]) {
					m++
				}
				if m-j >= minKnown && k.numbers[s[j:m]] && !yield(j, m) {
					return
				}
				j = m
			}
			i = end
		}
	}
}

// isHex reports whether c is a hex digit, in either case.
func isHex(c byte) bool {
	return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
