package mask

import (
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"time"
)

// minKnown is the fewest digits that a number Known holds has: a shorter
// one would mask too much that is no account number.
const minKnown = 6

// hashRun is the fewest hex digits in a run that Known takes for an account
// hash, whose digits it never masks: Schwab's hashes have 64.
const hashRun = 32

// Known is the set of full account numbers that a process has seen, which
// it masks wherever they stand whole: a second net beside Text, which masks
// by pattern alone and so misses a number of fewer than 8 digits, and which
// is never run over identifiers. It is safe for concurrent use. Its zero
// value is an empty set, and so is a nil *Known, which only reads.
type Known struct {
	mu      sync.RWMutex
	numbers map[string]bool
	// longest is how many digits the longest number has.
	longest int
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
		k.longest = max(k.longest, len(n))
	}
}

// Text returns s with each known number in it shown as Number shows it: its
// digits written whole, or in groups that Text would join, by one to three
// spaces or a dash with at most one space on each side, of any kind, with
// no digit before its first digit or after its last; but none of them
// within a run of 32 or more hex digits, which is an account hash, so that
// a hash stays whole. With 29470135 known, "ACCT29470135 TRF" reads
// "ACCT...135 TRF", and "2947 0135" and "2947-0135" read "...135", while
// "1294701350", "2947.0135" and a hash that holds 29470135 stay as they
// are. Groups that join a known number and more digits are masked as far
// as the number goes: "29 4701 35 77" reads "...135 77". The groups of a
// date of the years 1900 to 2099 written YYYY-MM-DD join nothing, so that
// a date and a time stay whole though their digits spell a known number:
// with 20261113 known, "2026-11-13" and "2026-11-13T16:40:00-05:00" stay
// as they are, while "20261113" and "2026 1113" read "...113". What Text
// returns holds no known number but in a date, and masking it again
// changes nothing: the three digits a mask shows count with the groups
// joined after it, as Text counts them.
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
		b.WriteString("...")
		b.WriteString(lastDigits(s[i:j], 3))
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

// HasNumber reports whether n, a number as JSON writes it, is a known
// number, judged by its value as JSON judges a number: with 29470135 known,
// 29470135, -29470135, 29470135.0 and 2.9470135E7 all are, while
// 2947013.5 and 129470135 are not.
func (k *Known) HasNumber(n json.Number) bool {
	digits, integer := integerDigits(n)
	return integer && k.has(digits)
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

// A piece is a group of digits of a joined run, or a known number of it
// that Text masks, which reads as the three digits its mask shows: the
// mask's dots join nothing before them, and its digits join the groups
// after it.
type piece struct {
	start, end int    // in s, from the first digit to the last
	digits     string // the group's digits, or the three that the mask shows
	masked     bool
}

// runs yields the start and end of each part of s that Text masks, in
// order: a known number in a run of groups of digits that joiner joins,
// each group in a run of hex digits shorter than a hash's, and none of a
// date (isDate). Digits are hex digits, so each group lies in one run of
// hex digits, which is measured once, however many numbers it holds. A
// number is found where its last group ends, as the longest known number
// that ends there, made of whole groups after the run's last mask, or of
// the three digits that mask shows and groups after it, so that what Text
// writes holds none; the pieces that no later group can join into a known
// number are let go, so that a run of any length costs as little as a
// short one. The caller holds k's lock.
func (k *Known) runs(s string) iter.Seq2[int, int] {
	return func(yield func(int, int) bool) {
		if !atLeastDigits(s, minKnown) {
			return
		}
		var run []piece // the joined run's pieces after its last mask, that mask first
		digits := 0     // the digits of run's pieces
		buf := make([]byte, k.longest)
		// drop lets go of run's first piece, yielding it if it is a mask.
		drop := func() bool {
			p := run[0]
			digits -= len(p.digits)
			run = append(run[:0], run[1:]...)
			return !p.masked || yield(p.start, p.end)
		}
		// push adds the group s[i:j] to run, and masks the longest known
		// number that ends with it.
		push := func(i, j int) bool {
			run = append(run, piece{start: i, end: j, digits: s[i:j]})
			digits += j - i
			from, best := len(buf), -1
			for t := len(run) - 1; t >= 0 && from >= len(run[t].digits); t-- {
				from -= len(run[t].digits)
				copy(buf[from:], run[t].digits)
				if len(buf)-from >= minKnown && k.numbers[string(buf[from:])] {
					best = t
				}
			}
			if best >= 0 {
				// What comes before the number, a mask included, can join no
				// later group: the number's own mask stands between.
				for range best {
					if !drop() {
						return false
					}
				}
				run = append(run[:0], piece{start: run[0].start, end: j, digits: string(buf[len(buf)-3:]), masked: true})
				digits = 3
			}
			for len(run) > 0 && digits >= k.longest {
				if !drop() {
					return false
				}
			}
			return true
		}
		// end yields run's mask, if it has one, and empties it, at the end of
		// a joined run.
		end := func() bool {
			for len(run) > 0 {
				if !drop() {
					return false
				}
			}
			return true
		}
		last := -1 // where the run's last group ends, or -1 for no run
		for i := 0; i < len(s); {
			if !isHex(s[i]) {
				i++
				continue
			}
			stop := i // the run of hex digits is s[i:stop]
			for stop < len(s) && isHex(s[stop]) {
				stop++
			}
			if stop-i >= hashRun {
				// A hash's digits join no group.
				if !end() {
					return
				}
				last, i = -1, stop
				continue
			}
			for j := i; j < stop; {
				if !isDigit(s[j]) {
					j++
					continue
				}
				if isDate(s, j) {
					// A date's groups join nothing: the run ends before it,
					// and the next group after it starts another.
					if !end() {
						return
					}
					last, stop = -1, j+len(time.DateOnly)
					break
				}
				m := j // the group is s[j:m]
				for m < stop && isDigit(s[m]) {
					m++
				}
				joined := false
				if last >= 0 {
					n := joiner(s[last:])
					joined = n > 0 && last+n == j
				}
				if !joined && !end() {
					return
				}
				if !push(j, m) {
					return
				}
				last, j = m, m
			}
			i = stop
		}
		end()
	}
}

// isDate reports whether s[j:], where a group of digits starts, begins with
// a date of the years 1900 to 2099 written as ISO 8601 writes one,
// YYYY-MM-DD, with no digit after it: a date, or the date of a time, such
// as Schwab's 2026-11-13T16:40:00+0000 and the store's
// 2026-11-13T21:40:00.000Z. Those years hold every date that Schwab and
// the store write, and the fewer years a date may have, the less often a
// group of a known number written in groups reads as one.
func isDate(s string, j int) bool {
	n := len(time.DateOnly)
	if len(s)-j < n || len(s) > j+n && isDigit(s[j+n]) {
		return false
	}
	d := s[j : j+n]
	if d[4] != '-' || d[7] != '-' || d[:2] != "19" && d[:2] != "20" {
		return false
	}
	_, err := time.Parse(time.DateOnly, d)
	return err == nil
}

// atLeastDigits reports whether s holds n or more ASCII digits.
func atLeastDigits(s string, n int) bool {
	for i := 0; i < len(s) && n > 0; i++ {
		if isDigit(s[i]) {
			n--
		}
	}
	return n <= 0
}

// isHex reports whether c is a hex digit, in either case.
func isHex(c byte) bool {
	return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}
