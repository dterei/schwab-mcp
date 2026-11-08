package schwab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

// labelPattern matches what may be a label that a liveScrubber puts in
// place of what it hides, such as {account 2}: a brace, a lower-case word,
// more words or numbers, and a brace. Only one that the scrubber gave, or
// an endpoint's {id}, is a label, and no value is matched inside one, so
// that a text scrubbed again stays as it is.
var labelPattern = regexp.MustCompile(`\{[a-z][a-z0-9 ]*\}`)

// idLabel is how an endpoint's template names an id in its path, as in
// accounts/{account 1}/transactions/{id}.
const idLabel = "{id}"

// homeLabel is what the home folder is shown as.
const homeLabel = "~"

// longValue is how many characters a value needs to be matched anywhere in
// a text; a shorter one is matched only as a whole token.
const longValue = 6

// A liveScrubber keeps identifiers and local paths out of what a live run
// prints and writes. scrub replaces, in any text, each identifier and path
// that the run has seen with a label, such as {account 2} for an account
// number or ~ for the home folder, and holds, the self-check, says whether
// a text still holds one. It is built from the run's paths and its first
// answers, and learns more as answers arrive.
//
// It matches as a strings.Replacer cannot:
//   - an identifier or a nickname of 6 characters or more anywhere, and a
//     shorter one only as a whole token, so that a short value, such as a
//     nickname "Ed", does not match ordinary words;
//   - identifiers, which are digits and letters, in any case, and
//     nicknames as they are;
//   - paths anywhere, and in any case where the file system takes a path
//     in any case, as on macOS and Windows;
//   - the user name, the home folder's last element, only as a path
//     component, since a short one, such as "ed", is inside many words.
//
// At each place the longest value wins.
type liveScrubber struct {
	mu sync.Mutex
	// foldPaths says whether paths and the user name match in any case.
	foldPaths bool
	// idents and names hold the identifiers, lower-cased in ASCII, and the
	// nicknames, as they are, by their length in bytes, with their labels;
	// lens are the lengths they hold, longest first.
	idents, names map[int]map[string]string
	lens          []int
	paths         []scrubPath
	user          *scrubPath
	// counts holds the labels given so far, by kind, such as "order id",
	// and labels each label given.
	counts map[string]int
	labels map[string]bool
}

// A scrubPath is a path, or the user name, as a liveScrubber matches it:
// as given, and folded, for a match in any case.
type scrubPath struct {
	text  string
	fold  []rune
	label string
}

// newLiveScrubber returns a scrubber that has seen nothing, which matches
// paths in any case on macOS and Windows.
func newLiveScrubber() *liveScrubber {
	return &liveScrubber{
		foldPaths: runtime.GOOS == "darwin" || runtime.GOOS == "windows",
		idents:    map[int]map[string]string{},
		names:     map[int]map[string]string{},
		counts:    map[string]int{},
		labels:    map[string]bool{idLabel: true, homeLabel: true},
	}
}

// scrub returns text with each value the scrubber knows replaced by its
// label. A replacement can make a short value next to it a whole token, so
// text is scrubbed again until nothing changes.
func (s *liveScrubber) scrub(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for range 8 {
		next, changed := s.replace(text)
		if !changed {
			break
		}
		text = next
	}
	return text
}

// holds reports whether text holds a value that the scrubber knows, outside
// the labels: the self-check, which a line must pass before it is printed
// or written.
func (s *liveScrubber) holds(text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	s.each(text, func(i, end int, _ string) bool {
		found = end > i
		return !found
	})
	return found
}

// replace is one pass of scrub, which needs s.mu: it reports whether
// anything was replaced.
func (s *liveScrubber) replace(text string) (string, bool) {
	var b strings.Builder
	changed := false
	s.each(text, func(i, end int, label string) bool {
		if end > i {
			b.WriteString(label)
			changed = true
		} else {
			b.WriteString(text[i:-end])
		}
		return true
	})
	return b.String(), changed
}

// each walks text, which it needs s.mu for, calling f for each part in
// turn, until f returns false: a value it holds, as its start, its end and
// its label; or anything else, a label or a character, as its start, minus
// its end, and "". A value is never matched across or inside a label.
func (s *liveScrubber) each(text string, f func(i, end int, label string) bool) {
	t := s.newText(text)
	var spans [][]int
	for _, span := range labelPattern.FindAllStringIndex(text, -1) {
		if s.labels[text[span[0]:span[1]]] {
			spans = append(spans, span)
		}
	}
	for i, k := 0, 0; i < len(text); {
		if k < len(spans) && i == spans[k][0] {
			if !f(i, -spans[k][1], "") {
				return
			}
			i = spans[k][1]
			k++
			continue
		}
		limit := len(text)
		if k < len(spans) {
			limit = spans[k][0]
		}
		if end, label := s.match(t, i, limit); end > i {
			if !f(i, end, label) {
				return
			}
			i = end
			continue
		}
		_, n := utf8.DecodeRuneInString(text[i:])
		if !f(i, -(i + n), "") {
			return
		}
		i += n
	}
}

// A scrubText is a text as a liveScrubber looks through it.
type scrubText struct {
	s     string
	lower string // s with ASCII lower-cased, which keeps its length
	// fold holds s's runes, folded, and at the place in fold of the rune at
	// each byte of s, or -1 inside one, and ends the byte at which each
	// rune ends; all for paths in any case.
	fold []rune
	at   []int
	ends []int
}

// newText returns text as s looks through it.
func (s *liveScrubber) newText(text string) *scrubText {
	t := &scrubText{s: text, lower: asciiLower(text)}
	if s.foldPaths {
		t.at = slices.Repeat([]int{-1}, len(text)+1)
		for i, r := range text {
			t.at[i] = len(t.fold)
			t.fold = append(t.fold, foldRune(r))
			t.ends = append(t.ends, i+utf8.RuneLen(r))
		}
	}
	return t
}

// match returns where the longest value that t holds at byte i, ending by
// limit, ends, and its label; or -1 for none.
func (s *liveScrubber) match(t *scrubText, i, limit int) (end int, label string) {
	end = -1
	take := func(e int, l string) {
		if e <= limit && e > end {
			end, label = e, l
		}
	}
	for _, n := range s.lens {
		if i+n > limit {
			continue
		}
		if l, ok := s.idents[n][t.lower[i:i+n]]; ok && whole(t.s, i, i+n) {
			take(i+n, l)
		}
		if l, ok := s.names[n][t.s[i:i+n]]; ok && whole(t.s, i, i+n) {
			take(i+n, l)
		}
	}
	for _, p := range s.paths {
		if e := t.pathAt(p, i, s.foldPaths); e > i {
			take(e, p.label)
		}
	}
	if s.user != nil {
		if e := t.pathAt(*s.user, i, s.foldPaths); e > i && isComponent(t.s, i, e) {
			take(e, s.user.label)
		}
	}
	return end, label
}

// pathAt returns where p ends if t holds it at byte i, in any case if fold
// is set, or -1.
func (t *scrubText) pathAt(p scrubPath, i int, fold bool) int {
	if !fold {
		if strings.HasPrefix(t.s[i:], p.text) {
			return i + len(p.text)
		}
		return -1
	}
	j := t.at[i]
	if j < 0 || j+len(p.fold) > len(t.fold) || len(p.fold) == 0 || !slices.Equal(t.fold[j:j+len(p.fold)], p.fold) {
		return -1
	}
	return t.ends[j+len(p.fold)-1]
}

// whole reports whether s[i:end], a value found in s, counts as one: a value
// of longValue characters or more anywhere, and a shorter one only where no
// letter, digit or underscore is next to it.
func whole(s string, i, end int) bool {
	if utf8.RuneCountInString(s[i:end]) >= longValue {
		return true
	}
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[end:])
	return (i == 0 || !wordRune(before)) && (end == len(s) || !wordRune(after))
}

// isComponent reports whether s[i:end], the user name found in s, is a
// component of a path: after a separator, and before another, the end, or
// anything that cannot go on a name, such as a space, a colon or a dot.
func isComponent(s string, i, end int) bool {
	before, _ := utf8.DecodeLastRuneInString(s[:i])
	after, _ := utf8.DecodeRuneInString(s[end:])
	return i > 0 && (before == '/' || before == '\\') && (end == len(s) || !wordRune(after) && after != '-')
}

// wordRune reports whether r is part of a word: a letter, a digit or an
// underscore.
func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// foldRune returns r as a match in any case takes it: the least rune of
// its orbit under Unicode's simple case folding.
func foldRune(r rune) rune {
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}

// asciiLower returns s with its ASCII letters lower-cased, its length kept.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// put adds a value to m, the identifiers or the names, under key with
// label, unless it is there already.
func (s *liveScrubber) put(m map[int]map[string]string, key, label string) {
	if _, ok := m[len(key)][key]; ok {
		return
	}
	if m[len(key)] == nil {
		m[len(key)] = map[string]string{}
		if !slices.Contains(s.lens, len(key)) {
			s.lens = append(s.lens, len(key))
			slices.SortFunc(s.lens, func(a, b int) int { return b - a })
		}
	}
	m[len(key)][key] = label
	s.labels[label] = true
}

// short reports whether value is too short to hide: a value of one
// character tells nothing, and matching one would garble the report.
func short(value string) bool {
	return utf8.RuneCountInString(value) < 2
}

// addIdent gives value, an identifier of kind, such as "order id", the
// next label of its kind, unless it has one; spelled escaped for a path,
// it has the same label.
func (s *liveScrubber) addIdent(kind, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addIdentLocked(kind, value)
}

// addIdentLocked is addIdent, which needs s.mu.
func (s *liveScrubber) addIdentLocked(kind, value string) {
	if short(value) {
		return
	}
	if _, ok := s.idents[len(value)][asciiLower(value)]; ok {
		return
	}
	s.counts[kind]++
	label := fmt.Sprintf("{%s %d}", kind, s.counts[kind])
	for _, v := range []string{value, url.PathEscape(value)} {
		s.put(s.idents, asciiLower(v), label)
	}
}

// addAccount labels the account number and the hash of the nth account
// in the account numbers' answer {account n} and {hash n}, so that the
// labels follow its order.
func (s *liveScrubber) addAccount(n int, number, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addAccountLocked(n, number, hash)
}

// addAccountLocked is addAccount, which needs s.mu. A value that has a
// label keeps it, and one whose label another value has, as when the list
// read again holds an account new to the run, gets the next of its kind.
func (s *liveScrubber) addAccountLocked(n int, number, hash string) {
	for _, a := range []struct{ kind, value string }{{"account", number}, {"hash", hash}} {
		label := fmt.Sprintf("{%s %d}", a.kind, n)
		switch _, known := s.idents[len(a.value)][asciiLower(a.value)]; {
		case short(a.value) || known:
		case s.labels[label]:
			s.addIdentLocked(a.kind, a.value)
		default:
			for _, v := range []string{a.value, url.PathEscape(a.value)} {
				s.put(s.idents, asciiLower(v), label)
			}
			s.counts[a.kind] = max(s.counts[a.kind], n)
		}
	}
}

// addName gives value, a nickname, the next nickname label, unless it has
// one. Nicknames are matched as they are, in their case.
func (s *liveScrubber) addName(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addNameLocked(value)
}

// addNameLocked is addName, which needs s.mu.
func (s *liveScrubber) addNameLocked(value string) {
	if short(value) {
		return
	}
	if _, ok := s.names[len(value)][value]; ok {
		return
	}
	s.counts["nickname"]++
	s.put(s.names, value, fmt.Sprintf("{nickname %d}", s.counts["nickname"]))
}

// addPath labels each spelling of path, a local file or folder.
func (s *liveScrubber) addPath(label, path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range pathSpellings(path) {
		s.paths = append(s.paths, scrubPath{text: p, fold: foldText(p), label: label})
	}
	s.labels[label] = true
}

// setHome labels each spelling of home, the home folder, ~, and its last
// element, the user name, {user}.
func (s *liveScrubber) setHome(home string) {
	spellings := pathSpellings(home)
	if len(spellings) == 0 {
		return
	}
	s.addPath(homeLabel, home)
	user := filepath.Base(spellings[0])
	s.mu.Lock()
	defer s.mu.Unlock()
	if user != "" && user != "." && !strings.ContainsAny(user, `/\`) {
		s.user = &scrubPath{text: user, fold: foldText(user), label: "{user}"}
		s.labels[s.user.label] = true
	}
}

// foldText returns the runes of s, each folded.
func foldText(s string) []rune {
	rs := []rune(s)
	for i, r := range rs {
		rs[i] = foldRune(r)
	}
	return rs
}

// pathSpellings returns the ways an error or a warning may spell path: as
// given, made absolute and clean, and as the system takes it, with its
// links followed, which the Auth and the Client use; on Windows, also with
// slashes for backslashes. A root names no one, and has none.
func pathSpellings(path string) []string {
	if path == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil || filepath.Dir(abs) == abs {
		return nil
	}
	ps := []string{abs}
	if resolved, err := resolve(abs); err == nil && resolved != abs {
		ps = append(ps, resolved)
	}
	if runtime.GOOS == "windows" {
		for _, p := range ps {
			ps = append(ps, strings.ReplaceAll(p, `\`, "/"))
		}
	}
	return ps
}

// identKeys are the keys of Schwab's answers whose values the scrubber
// learns, with the kind of each.
var identKeys = []struct{ key, kind string }{
	{"accountNumber", "account"},
	{"hashValue", "hash"},
	{"nickName", "nickname"},
	{"displayAcctId", "display id"},
	{"schwabClientCustomerId", "streamer customer id"},
	{"schwabClientCorrelId", "streamer correl id"},
	{"activityId", "activity id"},
	{"orderId", "order id"},
	{"positionId", "position id"},
}

// learnAnswer learns the identifiers in body, an answer of Schwab's: the
// value of every key of identKeys, in any case and at any depth, a string
// or a number as written.
func (s *liveScrubber) learnAnswer(body []byte) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.learnJSON(v)
}

// learnJSON is learnAnswer for v, JSON decoded as any, which needs s.mu.
// The keys of an object are taken in order, so that the labels are the
// same each run.
func (s *liveScrubber) learnJSON(v any) {
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			s.learnJSON(x)
		}
	case map[string]any:
		values := map[string]string{}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			var text string
			switch x := v[k].(type) {
			case string:
				text = x
			case json.Number:
				text = x.String()
			default:
				s.learnJSON(x)
				continue
			}
			for _, ik := range identKeys {
				if strings.EqualFold(k, ik.key) && values[ik.kind] == "" {
					values[ik.kind] = text
				}
			}
		}
		for _, ik := range identKeys {
			switch text := values[ik.kind]; {
			case text == "":
			case ik.kind == "nickname":
				s.addNameLocked(text)
			default:
				s.addIdentLocked(ik.kind, text)
			}
		}
	}
}

// isCorrelationHeader reports whether name is a header that carries an ID by
// which a request can be traced, such as Schwab-Client-CorrelId or
// X-Request-Id.
func isCorrelationHeader(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "correl") || strings.Contains(n, "request-id") || strings.Contains(n, "requestid") ||
		strings.Contains(n, "trace-id") || strings.Contains(n, "traceid")
}

// learnHeader learns the correlation IDs in h, an answer's header, its
// names taken in order, so that the labels are the same each run.
func (s *liveScrubber) learnHeader(h http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range slices.Sorted(maps.Keys(h)) {
		if isCorrelationHeader(name) {
			for _, v := range h[name] {
				s.addIdentLocked("correlation id", v)
			}
		}
	}
}

// learnError learns the correlation ID, the edge's Reference, the
// header and the identifiers of the body of each *Error in err's tree.
func (s *liveScrubber) learnError(err error) {
	if err == nil {
		return
	}
	if se, ok := err.(*Error); ok { //nolint:errorlint // each error of the tree is looked at in turn, below
		s.addIdent("correlation id", se.CorrelID)
		s.addIdent("reference", se.Reference)
		s.learnHeader(se.Header)
		s.learnAnswer([]byte(se.Body))
	}
	switch u := err.(type) { //nolint:errorlint // walking the tree by hand
	case interface{ Unwrap() error }:
		s.learnError(u.Unwrap())
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			s.learnError(e)
		}
	}
}

// fixtureScrubber returns a scrubber that has learned the fixtures' own
// answers, with home as the home folder.
func fixtureScrubber(t *testing.T, home string) *liveScrubber {
	t.Helper()
	s := newLiveScrubber()
	s.setHome(home)
	for _, name := range []string{"account_numbers.json", "user_preference.json", "accounts.json", "transactions.json", "transaction.json"} {
		s.learnAnswer(readTestdata(t, name))
	}
	return s
}

// The scrubber labels each account number and hash in the account numbers'
// order, raw and escaped, and in any case; and every other identifier of
// the fixtures' answers by its kind, in the order met: nicknames, as they
// are, the display IDs, the streamer's customer and correlation IDs, and
// each activity, order and position ID.
func TestScrubberLabelsTheFixtures(t *testing.T) {
	home := filepath.Join(t.TempDir(), "zqplanted")
	s := fixtureScrubber(t, home)
	for _, tc := range []struct{ in, want string }{
		{"accounts 29470135, 61803398 and 40221783", "accounts {account 1}, {account 2} and {account 3}"},
		{"/trader/v1/accounts/" + fixtureHash(0) + "/transactions", "/trader/v1/accounts/{hash 1}/transactions"},
		{"hash " + strings.ToLower(fixtureHash(2)) + ".", "hash {hash 3}."},
		{"Individual, Options & Margin, Contributory", "{nickname 1}, {nickname 2}, {nickname 3}"},
		{"individual", "individual"},
		{"...135 and ...398", "{display id 1} and {display id 2}"},
		{"customer " + "5f0c2e9a8b7d4e31a6c9f2b0d8e7a4c15f0c2e9a8b7d4e31a6c9f2b0d8e7a4c1", "customer {streamer customer id 1}"},
		{"correl C3A9E2F4-7B1D-4E8A-9F6C-2D5B8A1E0F7C", "correl {streamer correl id 1}"},
		{"rows 73508261941 and 73508261954", "rows {activity id 1} and {activity id 14}"},
		{"order 1004517638295 at position 3926184750", "order {order id 1} at position {position id 1}"},
		{"transactions/73508261946?x=1004517601177", "transactions/{activity id 6}?x={order id 4}"},
		{"a count of 3 and the year 2026", "a count of 3 and the year 2026"},
	} {
		if got := s.scrub(tc.in); got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if s.holds(s.scrub(tc.in)) {
			t.Errorf("%q still holds a value once scrubbed", tc.in)
		}
	}
	if !s.holds("29470135") || !s.holds("Options & Margin") || !s.holds(strings.ToLower(fixtureHash(1))) {
		t.Error("the self-check does not find the fixtures' values")
	}
}

// fixtureHash returns the hash of the fixtures' nth account.
func fixtureHash(n int) string {
	return string(fixtureHashes()[[]string{"29470135", "61803398", "40221783"}[n]])
}

// A value of six characters or more is found anywhere, inside a word too;
// a shorter one only as a whole token, so that a nickname such as "Ed" or
// "IRA" leaves the words it is inside alone. A value of one character is
// not hidden at all. An identifier is found escaped for a path too.
func TestScrubberShortValuesAreWholeTokens(t *testing.T) {
	s := newLiveScrubber()
	for _, v := range []string{"Ed", "IRA", "Trading", "x"} {
		s.addName(v)
	}
	s.addIdent("position id", "12345")
	s.addIdent("order id", "1234567")
	s.addIdent("hash", "zq9 hash/+1")
	for _, tc := range []struct{ in, want string }{
		{"Ed's IRA", "{nickname 1}'s {nickname 2}"},
		{"Edward received an IRAs file", "Edward received an IRAs file"},
		{"Ed_x and xIRA and IRA9", "Ed_x and xIRA and IRA9"},
		{"(IRA)", "({nickname 2})"},
		{"dayTradingBuyingPower", "day{nickname 3}BuyingPower"},
		{"a x b", "a x b"},
		{"position 12345, not 123456 or a12345", "position {position id 1}, not 123456 or a12345"},
		{"order 1234567 and x1234567y", "order {order id 1} and x{order id 1}y"},
		{"/accounts/zq9%20hash%2F+1/x and zq9 HASH/+1", "/accounts/{hash 1}/x and {hash 1}"},
	} {
		if got := s.scrub(tc.in); got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if s.holds("Edward") || s.holds("a12345") || !s.holds("Ed!") || !s.holds("x1234567y") {
		t.Error("the self-check takes short values otherwise than scrub does")
	}
}

// The accounts of the account numbers' answer are labeled by their place
// in it, number and hash alike; one met elsewhere, or new to a list read
// again, gets the next label of its kind, and no label stands for two
// values.
func TestScrubberLabelsAccountsByTheirPlace(t *testing.T) {
	s := newLiveScrubber()
	s.learnAnswer([]byte(`{"hashValue":"HASHALONE"}`))
	s.addAccount(1, "11111111", "HASHONE1")
	s.addAccount(2, "22222222", "HASHTWO2")
	s.addAccount(1, "33333333", "HASHTHREE")
	s.addAccount(2, "11111111", "HASHONE1")
	s.learnAnswer([]byte(`{"accountNumber":"44444444"}`))
	in := "HASHALONE 11111111 HASHONE1 22222222 HASHTWO2 33333333 HASHTHREE 44444444"
	want := "{hash 1} {account 1} {hash 2} {account 2} {hash 3} {account 3} {hash 4} {account 4}"
	if got := s.scrub(in); got != want {
		t.Errorf("scrub(%q)\n = %q\nwant %q", in, got, want)
	}
}

// A short value that a replacement leaves next to a label is a whole token
// then, and is replaced too, so that the self-check passes what scrub
// gives.
func TestScrubberScrubsUntilNothingChanges(t *testing.T) {
	s := newLiveScrubber()
	s.addName("Ed")
	s.addIdent("account", "29470135")
	in := "Ed29470135 and 29470135Ed"
	got := s.scrub(in)
	if want := "{nickname 1}{account 1} and {account 1}{nickname 1}"; got != want {
		t.Errorf("scrub(%q) = %q, want %q", in, got, want)
	}
	if s.holds(got) {
		t.Errorf("%q holds a value", got)
	}
}

// A label is never matched, even by a value that it holds, so that a text
// scrubbed again stays as it is; only a whole label is one.
func TestScrubberLeavesLabelsAlone(t *testing.T) {
	s := newLiveScrubber()
	s.addName("account")
	s.addName("id")
	s.addAccount(1, "29470135", "HASH01")
	in := "accounts/{account 1}/transactions/{id} for account 29470135"
	got := s.scrub(in)
	if want := "{nickname 1}s/{account 1}/transactions/{id} for {nickname 1} {account 1}"; got != want {
		t.Errorf("scrub(%q) = %q, want %q", in, got, want)
	}
	if again := s.scrub(got); again != got || s.holds(got) {
		t.Errorf("scrubbed again: %q, holding a value: %v", again, s.holds(got))
	}
	for _, text := range []string{"{account 29470135}", "{29470135}", "{Account 1} id", "{account 9} id"} {
		if !s.holds(text) {
			t.Errorf("the self-check passes %q", text)
		}
	}
	odd := filepath.Join(t.TempDir(), "{account 1}") // a folder named as a label is
	s.addPath("{rate file}", odd)
	if got := s.scrub(odd); got != odd || s.holds(got) {
		t.Errorf("scrub(%q) = %q, matched across a label", odd, got)
	}
}

// A root folder names no one, so neither it nor its last element is
// hidden, as the home folder or not.
func TestScrubberLeavesRootsAlone(t *testing.T) {
	root := t.TempDir()
	for filepath.Dir(root) != root {
		root = filepath.Dir(root)
	}
	s := newLiveScrubber()
	s.setHome(root)
	s.addPath("{cache folder}", root)
	in := filepath.Join(root, "zq9", "x")
	if got := s.scrub(in); got != in || s.user != nil {
		t.Errorf("scrub(%q) = %q; the user %v", in, got, s.user)
	}
}

// The home folder is shown as ~, and a file or folder of the run's by its
// label, the longest path winning; on macOS and Windows a path in another
// case is matched too. The user name is matched only as a path component,
// after a slash or a backslash, so that a short one is not found in words.
func TestScrubberPaths(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "ed")
	for _, fold := range []bool{false, true} {
		t.Run(fmt.Sprintf("fold=%v", fold), func(t *testing.T) {
			s := newLiveScrubber()
			s.foldPaths = fold
			s.setHome(home)
			s.addPath("{token file}", filepath.Join(home, "config", "token.json"))
			s.addPath("{rate file}", filepath.Join(dir, "cache", "rate-1"))
			upper := strings.ToUpper(home)
			for _, tc := range []struct{ in, want string }{
				{"open " + filepath.Join(home, "x") + ": denied", "open " + filepath.Join(homeLabel, "x") + ": denied"},
				{filepath.Join(home, "config", "token.json") + " could be read by others", "{token file} could be read by others"},
				{filepath.Join(home, "config", "token.json.lock"), "{token file}.lock"},
				{filepath.Join(dir, "cache", "rate-1") + " cannot be used", "{rate file} cannot be used"},
				{"/srv/ed/x, C:\\Users\\ed\\y, /srv/ed: gone, /srv/ed.", "/srv/{user}/x, C:\\Users\\{user}\\y, /srv/{user}: gone, /srv/{user}."},
				{"Ed received edits at /srv/eddie, /srv/ed-old and /srv/x_ed", "Ed received edits at /srv/eddie, /srv/ed-old and /srv/x_ed"},
			} {
				if got := s.scrub(tc.in); got != tc.want {
					t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
			if got := s.scrub(upper); (got == homeLabel) != fold {
				t.Errorf("scrub(%q) = %q", upper, got)
			}
			if s.holds(upper) != fold || s.holds("/srv/ED/x") != fold || !s.holds(home) || s.holds("Ed received edits") {
				t.Error("the self-check takes paths otherwise than scrub does")
			}
			if runtime.GOOS == "windows" { // where a path may come with slashes too
				token := filepath.Join(home, "config", "token.json")
				if got := s.scrub(filepath.ToSlash(token)); got != "{token file}" {
					t.Errorf("scrub(%q) = %q", filepath.ToSlash(token), got)
				}
			}
		})
	}
}

// A path under a link is matched as given and as the system takes it, with
// the link followed, which is how the Auth and the Client name their files.
func TestScrubberPathsUnderALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need extra rights on Windows")
	}
	dir := t.TempDir()
	if err := osSymlink(filepath.Join(dir, "real"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	s := newLiveScrubber()
	s.addPath("{token file}", filepath.Join(dir, "link", "token.json"))
	resolved, err := resolve(filepath.Join(dir, "real", "token.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "link", "token.json"), resolved} {
		if got := s.scrub(p); got != "{token file}" {
			t.Errorf("scrub(%q) = %q", p, got)
		}
	}
}

// The scrubber learns the identifiers of any answer, at any depth, by key
// in any case, as strings or numbers; correlation IDs from an answer's
// header; and, from each *Error in an error's tree, its correlation ID, the
// edge's Reference, and the identifiers in its header and body.
func TestScrubberLearns(t *testing.T) {
	s := newLiveScrubber()
	s.learnAnswer([]byte(`[{"ActivityID": 51234567890, "x": {"orderId": "71234567890"}, "positionId": 9.5, ` +
		`"accountNumber": 81234567, "hashValue": "HASH9", "flag": true, "list": [{"NICKNAME": "Zq Fund"}]}]`))
	s.learnHeader(http.Header{
		"Schwab-Client-Correlid": {"corr-zq1"}, "X-Request-Id": {"req-zq2"}, "Content-Type": {"application/json"},
	})
	inner := &Error{
		Status: http.StatusForbidden, CorrelID: "corr-zq3", Reference: "18.zq4.5", Header: http.Header{"X-Request-Id": {"req-zq5"}},
		Body: `{"message":"no","errors":[],"accountNumber":"91234567"}`,
	}
	err := fmt.Errorf("one: %w; two: %w", errors.New("x"), fmt.Errorf("wrapped: %w", inner))
	s.learnError(err)
	in := "51234567890 71234567890 9.5 81234567 HASH9 Zq Fund corr-zq1 req-zq2 corr-zq3 18.zq4.5 req-zq5 91234567 application/json true"
	want := "{activity id 1} {order id 1} {position id 1} {account 1} {hash 1} {nickname 1} {correlation id 1} " +
		"{correlation id 2} {correlation id 3} {reference 1} {correlation id 4} {account 2} application/json true"
	if got := s.scrub(in); got != want {
		t.Errorf("scrub(%q)\n = %q\nwant %q", in, got, want)
	}

	// The names of a header are taken in order, so the labels do not
	// change from run to run.
	h := http.Header{"Schwab-Client-Correlid": {"zq-a"}, "X-Request-Id": {"zq-b"}, "X-Correlation-Id": {"zq-c"}, "Trace-Id": {"zq-d"}}
	for range 20 {
		s := newLiveScrubber()
		s.learnHeader(h)
		if got := s.scrub("zq-a zq-b zq-c zq-d"); got != "{correlation id 1} {correlation id 4} {correlation id 3} {correlation id 2}" {
			t.Fatalf("labeled %q", got)
		}
	}
}

// osSymlink makes a link at link to target, a folder it makes.
func osSymlink(target, link string) error {
	if err := os.Mkdir(target, 0o700); err != nil {
		return err
	}
	return os.Symlink(target, link)
}
