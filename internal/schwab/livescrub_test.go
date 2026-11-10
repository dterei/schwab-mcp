package schwab

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
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

// A liveScrubber keeps identifiers, secrets, local paths and the owner's
// nicknames and symbols out of what a live run prints and writes. scrub
// replaces, in a text, each identifier, secret and path that the run has
// seen, and each nickname and symbol in Schwab's own words, with a label,
// such as {account 2} for an account number, {access token 1} for a token
// or ~ for the home folder, and holds, the self-check, says whether a text
// still holds one. It is built from the run's paths, the app's key and
// secret, the login's tokens and the run's first answers, and learns more
// as requests go and answers arrive. Identifiers and secrets are matched
// escaped too, as a path or a form spells them.
//
// It matches as a strings.Replacer cannot:
//   - an identifier, a nickname or a symbol of 6 characters or more
//     anywhere, and a shorter one only as a whole token, so that a short
//     value, such as a nickname "Ed", does not match ordinary words;
//   - identifiers, which are digits and letters, in any case, and
//     nicknames and symbols as they are, since a symbol in capitals, such
//     as "ALL", is a word in lower case;
//   - nicknames and symbols only in Schwab's own words, which said marks,
//     such as an error's message, which can quote them. The rest of a
//     text is the run's own words, or Schwab's names, its keys, headers
//     and wording, which hold none of the owner's but can spell one, as
//     "no read of the account API was refused" spells the symbol API: a
//     label there, in words the reader knows, would show what it hides;
//   - paths anywhere, and in any case where the file system takes a path
//     in any case, as on macOS and Windows;
//   - the user name, the home folder's last element, only as the folder of
//     a home, after /home/, /Users/ or \Users\: nothing but a local path
//     brings it into a line, while the run's own words and the API's paths
//     can spell it, as accounts/{account 1}/orders spells a user orders,
//     and a short one, such as "ed", is inside many words.
//
// At each place the longest value wins. Schwab's own name is not hidden as
// a nickname: the report and Schwab's words name the broker throughout, so
// showing it shows nothing, while a label in its place would.
type liveScrubber struct {
	mu sync.Mutex
	// foldPaths says whether paths and the user name match in any case.
	foldPaths bool
	// idents and names hold the identifiers, lower-cased in ASCII, and the
	// nicknames and symbols, as they are, by their length in bytes, with
	// their labels; lens are the lengths they hold, longest first.
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

// The marks that said puts around Schwab's own words in a text: control
// characters, which no value holds, and which unmark takes out before the
// text is printed or written.
const (
	saidOpen  = '\x02'
	saidClose = '\x03'
)

// said returns text, Schwab's own words, such as a message that is not on
// its wording, marked for the report's scrubber, which hides nicknames and
// symbols only in such words. Any mark that text holds is taken out first,
// so that Schwab's words cannot end the marking early.
func said(text string) string {
	return string(saidOpen) + unmark(text) + string(saidClose)
}

// unmark returns text without said's marks, as it is printed or written.
func unmark(text string) string {
	return strings.Map(func(r rune) rune {
		if r == saidOpen || r == saidClose {
			return -1
		}
		return r
	}, text)
}

// saidSpans returns the spans of text that said marks, without the marks:
// each from after an opening mark to the closing mark after it, or to the
// end of text if none does.
func saidSpans(text string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(text); {
		open := strings.IndexRune(text[i:], saidOpen)
		if open < 0 {
			break
		}
		start, end := i+open+1, len(text)
		if n := strings.IndexRune(text[start:], saidClose); n >= 0 {
			end = start + n
		}
		spans = append(spans, [2]int{start, end})
		i = end + 1
	}
	return spans
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
// label: a nickname or a symbol only in what said marks, and every other
// value anywhere. The marks stay, for unmark to take out. A replacement can
// make a short value next to it a whole token, so text is scrubbed again
// until nothing changes.
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
// the labels, where scrub would match it: the self-check, which a line must
// pass before it is printed or written.
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
// its end, and "". A value is never matched across or inside a label, and a
// nickname or a symbol only inside what said marks.
func (s *liveScrubber) each(text string, f func(i, end int, label string) bool) {
	t := s.newText(text)
	var spans [][]int
	for _, span := range labelPattern.FindAllStringIndex(text, -1) {
		if s.labels[text[span[0]:span[1]]] {
			spans = append(spans, span)
		}
	}
	words := saidSpans(text)
	for i, k, w := 0, 0, 0; i < len(text); {
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
		for w < len(words) && words[w][1] <= i {
			w++
		}
		names := -1 // where a name at i must end by: the end of Schwab's words that i lies in
		if w < len(words) && words[w][0] <= i {
			names = min(limit, words[w][1])
		}
		if end, label := s.match(t, i, limit, names); end > i {
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
// limit, ends, and its label; or -1 for none. A nickname or a symbol must
// end by names, which is -1 where none may be matched.
func (s *liveScrubber) match(t *scrubText, i, limit, names int) (end int, label string) {
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
		if l, ok := s.names[n][t.s[i:i+n]]; ok && i+n <= names && whole(t.s, i, i+n) {
			take(i+n, l)
		}
	}
	for _, p := range s.paths {
		if e := t.pathAt(p, i, s.foldPaths); e > i {
			take(e, p.label)
		}
	}
	if s.user != nil {
		if e := t.pathAt(*s.user, i, s.foldPaths); e > i && homeOf(t.s, i, e, s.foldPaths) {
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

// homeOf reports whether s[i:end], the user name found in s, is the folder
// of a home: after one of homeFolders, in any case where fold is set, and
// before a separator, the end, or anything that cannot go on a name, such
// as a space, a colon or a dot.
func homeOf(s string, i, end int, fold bool) bool {
	if after, _ := utf8.DecodeRuneInString(s[end:]); end < len(s) && (wordRune(after) || after == '-') {
		return false
	}
	return slices.ContainsFunc(homeFolders, func(f string) bool {
		return i >= len(f) && (s[i-len(f):i] == f || fold && strings.EqualFold(s[i-len(f):i], f))
	})
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
// next label of its kind, unless it has one; spelled escaped for a path or
// for a query, as a form sends a refresh token, it has the same label.
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
	for _, v := range spellings(value) {
		s.put(s.idents, asciiLower(v), label)
	}
}

// spellings returns the ways a text may spell value, an identifier: as it
// is, and escaped for a path and for a query.
func spellings(value string) []string {
	return slices.Compact([]string{value, url.PathEscape(value), url.QueryEscape(value)})
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
			for _, v := range spellings(a.value) {
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
	s.addNameLocked("nickname", value)
}

// addNameLocked gives value, a name of kind, such as "nickname" or
// "symbol", which is matched as it is, the next label of its kind, unless
// it has one, or it is Schwab's own name, in any case, which is not hidden.
// It needs s.mu.
func (s *liveScrubber) addNameLocked(kind, value string) {
	if short(value) || strings.EqualFold(value, "Schwab") {
		return
	}
	if _, ok := s.names[len(value)][value]; ok {
		return
	}
	s.counts[kind]++
	s.put(s.names, value, fmt.Sprintf("{%s %d}", kind, s.counts[kind]))
}

// hides reports whether s hides value wherever it is a whole token: an
// identifier or a name that s has learned. A probe sends a value of the
// owner's, such as a symbol, only when s hides it, since Schwab's refusal
// may quote it.
func (s *liveScrubber) hides(value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ident := s.idents[len(value)][asciiLower(value)]
	_, name := s.names[len(value)][value]
	return ident || name
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
// learns, with the kind of each, and whether it is a name, matched as it
// is, rather than an identifier, matched in any case: the API's, and the
// tokens of the token endpoint's.
var identKeys = []struct {
	key, kind string
	name      bool
}{
	{"accountNumber", "account", false},
	{"hashValue", "hash", false},
	{"nickName", "nickname", true},
	{"displayAcctId", "display id", false},
	{"schwabClientCustomerId", "streamer customer id", false},
	{"schwabClientCorrelId", "streamer correl id", false},
	{"activityId", "activity id", false},
	{"orderId", "order id", false},
	{"positionId", "position id", false},
	{"symbol", "symbol", true},
	{"underlyingSymbol", "symbol", true},
	{"rootSymbol", "symbol", true},
	{"uniformSymbol", "symbol", true},
	{"access_token", "access token", false},
	{"refresh_token", "refresh token", false},
	{"id_token", "id token", false},
}

// learnAnswer learns the identifiers in body, an answer of Schwab's: the
// value of every key of identKeys, in any case and at any depth, a string
// or a number as written. A symbol of liveBenchmarks is not learned: it is
// the probes' own, which the report names.
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
// The keys of an object are taken in order, and its values in the order of
// identKeys, so that the labels are the same each run.
func (s *liveScrubber) learnJSON(v any) {
	switch v := v.(type) {
	case []any:
		for _, x := range v {
			s.learnJSON(x)
		}
	case map[string]any:
		values := make([]string, len(identKeys))
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
			for i, ik := range identKeys {
				if strings.EqualFold(k, ik.key) && values[i] == "" {
					values[i] = text
				}
			}
		}
		for i, ik := range identKeys {
			switch text := values[i]; {
			case text == "", ik.kind == "symbol" && slices.Contains(liveBenchmarks, text):
			case ik.name:
				s.addNameLocked(ik.kind, text)
			default:
				s.addIdentLocked(ik.kind, text)
			}
		}
	}
}

// learnRequest learns the secrets that req, a request of the run's,
// carries, before an answer can echo them: the credentials of its
// Authorization header, a bearer token, or the app's key and secret as
// Basic sends them, and the refresh token of a token request's form.
func (s *liveScrubber) learnRequest(req *http.Request) {
	if scheme, credentials, ok := strings.Cut(req.Header.Get("Authorization"), " "); ok {
		kind := "credentials"
		if strings.EqualFold(scheme, "Bearer") {
			kind = "access token"
		}
		s.addIdent(kind, strings.TrimSpace(credentials))
	}
	if req.Method == http.MethodPost && req.URL.EscapedPath() == tokenPath {
		if form, why := grantForm(req); why == "" {
			for _, v := range form["refresh_token"] {
				s.addIdent("refresh token", v)
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

// addressText matches an address in an error's text, with or without its
// port, as net writes one: an IPv4 address, or an IPv6 address in
// brackets, whose zone takes in no mark of said's.
var addressText = regexp.MustCompile(`(?:\b\d{1,3}(?:\.\d{1,3}){3}\b|\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*(?:%[^\]\s\x02\x03]*)?\])(?::\d+)?`)

// failureText returns the text of err, a read's failure, for the report,
// with Schwab's words marked, as errorText marks them, and without the
// addresses that a failure of the network names, or the query that a
// request's address holds. A connection's error names both its ends, this
// machine's address and port among them: on the owner's computer, its
// address on the local network, or a global IPv6 address, which can hold
// its hardware address. A failed lookup names the resolver's address. The
// scrubber learns none of them. So each of net's errors in err, under the
// Client's tries or in a refresh's failure that it wraps, is told by its
// operation, its network and its cause alone, and any address left, as in
// an error that quotes one of net's without wrapping it, becomes
// {address}. A request's address, which url's errors quote whole, is cut
// to its path: its query can hold a symbol of the owner's, outside
// Schwab's words, where the scrubber does not look for one.
func failureText(err error) string {
	text := unmark(err.Error())
	var walk func(e error)
	walk = func(e error) {
		if e == nil {
			return
		}
		// Each error before those it wraps, whose text it holds.
		switch ne := e.(type) { //nolint:errorlint // each error of the tree is looked at in turn, below
		case *net.OpError:
			if ne.Err != nil {
				told := ne.Op
				if ne.Net != "" {
					told += " " + ne.Net
				}
				text = strings.ReplaceAll(text, unmark(ne.Error()), told+": "+unmark(ne.Err.Error()))
			}
		case *net.DNSError:
			text = strings.ReplaceAll(text, unmark(ne.Error()), "lookup "+ne.Name+": "+ne.Err)
		case *url.Error:
			if u, perr := url.Parse(ne.URL); perr == nil && u.RawQuery != "" {
				u.RawQuery = ""
				text = strings.ReplaceAll(text, strconv.Quote(unmark(ne.URL)), strconv.Quote(u.String()))
			}
		case *Error:
			text = strings.ReplaceAll(text, unmark(ne.Error()), errorText(ne))
		}
		switch u := e.(type) { //nolint:errorlint // walking the tree by hand
		case interface{ Unwrap() error }:
			walk(u.Unwrap())
		case interface{ Unwrap() []error }:
			for _, inner := range u.Unwrap() {
				walk(inner)
			}
		}
	}
	walk(err)
	return addressText.ReplaceAllString(text, "{address}")
}

// errorText returns e's text, as Error gives it, with Schwab's words in it
// as errorSays marks them: its status and the run's own words stay as
// they are, so that "HTTP 400 from Schwab" never shows a label in place of
// a nickname spelled Schwab. Its correlation ID and its Reference are
// identifiers, which the scrubber hides anywhere.
func errorText(e *Error) string {
	text := unmark(e.Error())
	if says := unmark(e.says()); says != "" {
		// Error puts what Schwab says first, after its status and tries.
		text = strings.Replace(text, ": "+says, ": "+errorSays(e), 1)
	}
	return text
}

// errorSays returns what e says, as says gives it, for the report: a
// message of Schwab's wording as it is, since it names no one, and any
// other text, its message, its errors or what its page says, marked by said,
// as Schwab's own words that can quote a nickname or a symbol.
func errorSays(e *Error) string {
	says := e.says()
	var parts []string
	if e.Message != "" {
		parts = append(parts, e.Message)
	}
	for _, s := range e.Errors {
		if s != "" && s != e.Message {
			parts = append(parts, s)
		}
	}
	switch {
	case says == "":
		return ""
	case strings.Join(parts, "; ") != says:
		return said(says) // a page's, or the start of a body
	}
	for i, p := range parts {
		if _, ok := onWordingOf(schwabMessages, p); !ok {
			parts[i] = said(p)
		}
	}
	return strings.Join(parts, "; ")
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
// are, the display IDs, the streamer's customer and correlation IDs, each
// activity, order and position ID, and each symbol, as it is, but for the
// benchmarks that the report names. Each is found in Schwab's words, as
// said marks them.
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
		{"'AAPL', META and BND; XYZ   260619P00045000 on XYZ", "'{symbol 1}', {symbol 6} and {symbol 13}; {symbol 4} on {symbol 5}"},
		{"/ESH25_P5050:XCME of /ESH25:XCME, and CURRENCY_USD", "{symbol 11} of {symbol 10}, and {symbol 7}"},
		{"SPY and VTI, aapl and Meta, XYZW and BNDS", "SPY and VTI, aapl and Meta, XYZW and BNDS"},
	} {
		if got := s.scrub(said(tc.in)); got != said(tc.want) {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if s.holds(s.scrub(said(tc.in))) {
			t.Errorf("%q still holds a value once scrubbed", tc.in)
		}
	}
	if !s.holds("29470135") || !s.holds(said("Options & Margin")) || !s.holds(strings.ToLower(fixtureHash(1))) || !s.holds(said("'BND'")) {
		t.Error("the self-check does not find the fixtures' values")
	}
	for value, want := range map[string]bool{"BND": true, "XYZ   260918C00050000": true, "61803398": true, "SPY": false, "VTI": false, "bnd": false, "BN": false} {
		if s.hides(value) != want {
			t.Errorf("hides(%q) = %v", value, !want)
		}
	}
}

// fixtureHash returns the hash of the fixtures' nth account.
func fixtureHash(n int) string {
	return string(fixtureHashes()[[]string{"29470135", "61803398", "40221783"}[n]])
}

// A value of six characters or more is found anywhere, inside a word too;
// a shorter one only as a whole token, so that a nickname such as "Ed" or
// "IRA" leaves the words it is inside alone. A value of one character is
// not hidden at all. An identifier is found escaped for a path or a query
// too.
func TestScrubberShortValuesAreWholeTokens(t *testing.T) {
	s := newLiveScrubber()
	for _, v := range []string{"Ed", "IRA", "Trading", "x", "Margin"} {
		s.addName(v)
	}
	s.addIdent("position id", "12345")
	s.addIdent("order id", "1234567")
	s.addIdent("hash", "zq9 hash/+1")
	s.addIdent("position id", "654321")
	for _, tc := range []struct{ in, want string }{
		{"Ed's IRA", "{nickname 1}'s {nickname 2}"},
		{"Edward received an IRAs file", "Edward received an IRAs file"},
		{"Ed_x and xIRA and IRA9", "Ed_x and xIRA and IRA9"},
		{"(IRA)", "({nickname 2})"},
		{"dayTradingBuyingPower", "day{nickname 3}BuyingPower"},
		{"a x b", "a x b"},
		{"position 12345, not 123456 or a12345", "position {position id 1}, not 123456 or a12345"},
		{"order 1234567 and x1234567y", "order {order id 1} and x{order id 1}y"},
		{"MarginX and a654321b, of six", "{nickname 4}X and a{position id 2}b, of six"},
		{"/accounts/zq9%20hash%2F+1/x and zq9 HASH/+1", "/accounts/{hash 1}/x and {hash 1}"},
		{"hash=zq9+hash%2F%2B1&x=1", "hash={hash 1}&x=1"},
	} {
		if got := s.scrub(said(tc.in)); got != said(tc.want) {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if s.holds(said("Edward")) || s.holds("a12345") || !s.holds(said("Ed!")) || !s.holds("x1234567y") || !s.holds(said("MarginX")) {
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
	in := said("Ed29470135 and 29470135Ed")
	got := s.scrub(in)
	if want := said("{nickname 1}{account 1} and {account 1}{nickname 1}"); got != want {
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
	in := said("accounts/{account 1}/transactions/{id} for account 29470135")
	got := s.scrub(in)
	if want := said("{nickname 1}s/{account 1}/transactions/{id} for {nickname 1} {account 1}"); got != want {
		t.Errorf("scrub(%q) = %q, want %q", in, got, want)
	}
	if again := s.scrub(got); again != got || s.holds(got) {
		t.Errorf("scrubbed again: %q, holding a value: %v", again, s.holds(got))
	}
	for _, text := range []string{"{account 29470135}", "{29470135}", "{Account 1} id", "{account 9} id"} {
		if !s.holds(said(text)) {
			t.Errorf("the self-check passes %q", text)
		}
	}
	odd := filepath.Join(t.TempDir(), "{account 1}") // a folder named as a label is
	s.addPath("{rate file}", odd)
	if got := s.scrub(odd); got != odd || s.holds(got) {
		t.Errorf("scrub(%q) = %q, matched across a label", odd, got)
	}
}

// A nickname or a symbol is hidden only in Schwab's own words, which said
// marks, while an identifier is hidden anywhere: the rest of a text is the
// run's own words and Schwab's names and wording, which can spell a
// nickname or a symbol, as "no read of the account API was refused" spells
// the symbol API, where a label would show what it hides. The marks stay,
// for unmark to take out; said takes out any mark in Schwab's words first,
// and a mark left open runs to the end. Schwab's own name, in any case, is
// not hidden as a nickname.
func TestScrubberHidesNamesInSchwabsWords(t *testing.T) {
	s := newLiveScrubber()
	s.learnAnswer([]byte(`{"accounts":[{"accountNumber":"29470135","nickName":"Dividend"},{"nickName":"Schwab"},{"nickName":"schwab"}],` +
		`"legs":[{"instrument":{"symbol":"API"}},{"instrument":{"symbol":"SYM"}}]}`))
	for _, tc := range []struct{ in, want string }{
		{"no read of the account API was refused", "no read of the account API was refused"},
		{`"Ordinary Dividend~SYM" of 29470135`, `"Ordinary Dividend~SYM" of {account 1}`},
		{
			"HTTP 400 from Schwab: " + said("no API of 29470135, Dividend's, at Schwab"),
			"HTTP 400 from Schwab: " + said("no {symbol 1} of {account 1}, {nickname 1}'s, at Schwab"),
		},
		{said("schwab or SCHWAB"), said("schwab or SCHWAB")},
		{said("API") + " API " + said("SYM"), said("{symbol 1}") + " API " + said("{symbol 2}")},
		{"open \x02API, Dividend", "open \x02{symbol 1}, {nickname 1}"},
		{"API\x03 Dividend", "API\x03 Dividend"},
	} {
		got := s.scrub(tc.in)
		if got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if s.holds(got) {
			t.Errorf("%q still holds a value once scrubbed", tc.in)
		}
	}
	if s.holds("the account API") || !s.holds(said("the account API")) || !s.holds("29470135") {
		t.Error("the self-check takes names otherwise than scrub does")
	}
	if got := said("zq\x02 and \x03zq"); got != "\x02zq and zq\x03" || unmark(got) != "zq and zq" {
		t.Errorf("said = %q, unmarked %q", got, unmark(got))
	}
	if !s.hides("API") || s.hides("Schwab") {
		t.Error("the scrubber hides a symbol API, or a nickname Schwab, otherwise")
	}
}

// The user name is hidden only as the folder of a home, so the API's own
// paths and the run's templates, which spell a user named as one of their
// words, are left as they are, wherever the word stands in them.
func TestScrubberLeavesTheAPIsPathsAlone(t *testing.T) {
	const api = `GET /trader/v1/accounts/{hash 1}/orders and /marketdata/v1/quotes, after Post "https://h/v1/oauth/token": no; ` +
		"accounts/{account 2}/transactions/{id}, accounts/accountNumbers, orders, marketdata/pricehistory"
	for _, user := range []string{"trader", "v1", "marketdata", "oauth", "token", "accounts", "orders", "transactions", "accountNumbers", "pricehistory"} {
		s := newLiveScrubber()
		s.setHome(filepath.Join(t.TempDir(), user))
		if got := s.scrub(api); got != api || s.holds(api) {
			t.Errorf("user %s: scrub(%q) = %q", user, api, got)
		}
		in := "/home/" + user + "/x"
		if got, want := s.scrub(in), "/home/{user}/x"; got != want {
			t.Errorf("user %s: scrub(%q) = %q, want %q", user, in, got, want)
		}
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
// case is matched too. The user name is matched only as the folder of a
// home, after /home/, /Users/ or \Users\, so that a short one is not found
// in words, nor in another folder's path.
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
				{"/home/ed/x, C:\\Users\\ed\\y, /Users/ed: gone, /var/home/ed.", "/home/{user}/x, C:\\Users\\{user}\\y, /Users/{user}: gone, /var/home/{user}."},
				{"Ed received edits at /home/eddie, /home/ed-old, /home/x_ed and /srv/ed", "Ed received edits at /home/eddie, /home/ed-old, /home/x_ed and /srv/ed"},
			} {
				if got := s.scrub(tc.in); got != tc.want {
					t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
				}
			}
			if got := s.scrub(upper); (got == homeLabel) != fold {
				t.Errorf("scrub(%q) = %q", upper, got)
			}
			if s.holds(upper) != fold || s.holds("/HOME/ED/x") != fold || s.holds("/HOME/ed/x") != fold || !s.holds(home) ||
				s.holds("Ed received edits") || s.holds("/srv/ed/x") {
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
// in any case, as strings or numbers; each symbol of an instrument, as it
// is, but for a benchmark's; correlation IDs from an answer's header; and,
// from each *Error in an error's tree, its correlation ID, the edge's
// Reference, and the identifiers in its header and body.
func TestScrubberLearns(t *testing.T) {
	s := newLiveScrubber()
	s.learnAnswer([]byte(`[{"ActivityID": 51234567890, "x": {"orderId": "71234567890"}, "positionId": 9.5, ` +
		`"accountNumber": 81234567, "hashValue": "HASH9", "flag": true, "list": [{"NICKNAME": "Zq Fund"}], ` +
		`"legs": [{"instrument": {"symbol": "ZQ   260918C00050000", "underlyingSymbol": "ALL", "rootSymbol": "ZQR", "uniformSymbol": "ZQU"}}, ` +
		`{"instrument": {"symbol": "SPY"}}]}]`))
	if got, want := s.scrub(said("ZQ   260918C00050000 of ALL, all, ZQR, ZQU and SPY")), said("{symbol 1} of {symbol 2}, all, {symbol 3}, {symbol 4} and SPY"); got != want {
		t.Errorf("the symbols scrubbed: %q, want %q", got, want)
	}
	s.learnHeader(http.Header{
		"Schwab-Client-Correlid": {"corr-zq1"}, "X-Request-Id": {"req-zq2"}, "Content-Type": {"application/json"},
	})
	inner := &Error{
		Status: http.StatusForbidden, CorrelID: "corr-zq3", Reference: "18.zq4.5", Header: http.Header{"X-Request-Id": {"req-zq5"}},
		Body: `{"message":"no","errors":[],"accountNumber":"91234567"}`,
	}
	err := fmt.Errorf("one: %w; two: %w", errors.New("x"), fmt.Errorf("wrapped: %w", inner))
	s.learnError(err)
	in := said("51234567890 71234567890 9.5 81234567 HASH9 Zq Fund corr-zq1 req-zq2 corr-zq3 18.zq4.5 req-zq5 91234567 application/json true")
	want := said("{activity id 1} {order id 1} {position id 1} {account 1} {hash 1} {nickname 1} {correlation id 1} " +
		"{correlation id 2} {correlation id 3} {reference 1} {correlation id 4} {account 2} application/json true")
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

// The scrubber learns the secrets that a request carries, its bearer token,
// or the Basic credentials and the refresh token of a token request's
// form, and the tokens of a grant, each as it is and escaped for a path
// and for a query, as a form sends it; a request without them teaches it
// nothing.
func TestScrubberLearnsSecrets(t *testing.T) {
	s := newLiveScrubber()
	get, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://api.schwabapi.com/trader/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.learnRequest(get)
	if len(s.lens) != 0 {
		t.Errorf("learned from a request without secrets: %v", s.idents)
	}
	get.Header.Set("Authorization", "Bearer zq.access-9-_=@")
	s.learnRequest(get)
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"zq-refresh/9-_=@"}}
	post, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.schwabapi.com"+tokenPath, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	post.SetBasicAuth("zqkey", "zqsecret")
	s.learnRequest(post)
	s.learnAnswer([]byte(`{"access_token":"zq.access-10-_=@","refresh_token":"zq-refresh-10-_=@","id_token":"zq.id.10","expires_in":1800}`))
	basic := base64.StdEncoding.EncodeToString([]byte("zqkey:zqsecret"))
	for _, tc := range []struct{ in, want string }{
		{"token zq.access-9-_=@, zq.access-9-_%3D%40 or ZQ.ACCESS-9-_=@", "token {access token 1}, {access token 1} or {access token 1}"},
		{"refresh_token=zq-refresh%2F9-_%3D%40, zq-refresh/9-_=@", "refresh_token={refresh token 1}, {refresh token 1}"},
		{"/x/zq-refresh%2F9-_=@/y", "/x/{refresh token 1}/y"},
		{"Basic " + basic, "Basic {credentials 1}"},
		{"zq.access-10-_=@ zq-refresh-10-_=@ zq.id.10", "{access token 2} {refresh token 2} {id token 1}"},
	} {
		if got := s.scrub(tc.in); got != tc.want {
			t.Errorf("scrub(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A failure of the network reaches the report without the addresses it
// names, wherever it lies: a connection's two ends, IPv4, or IPv6 with a
// zone, under the Client's tries, the try before's included; a lookup's
// resolver; a refresh's failure under the url.Error of the Auth's request,
// beside Schwab's answer, which stays as it is; errors joined; and an
// address quoted by an error that does not wrap net's. A request's address
// comes without its query. Schwab's answer keeps its status and its
// wording as they are, with its other words marked as Schwab's, in which
// no mark of Schwab's own is kept.
func TestFailureText(t *testing.T) {
	local := &net.TCPAddr{IP: net.ParseIP("192.168.1.23"), Port: 51234}
	global := &net.TCPAddr{IP: net.ParseIP("2001:db8:85a3::8a2e:370:7334"), Port: 51235}
	linkLocal := &net.TCPAddr{IP: net.ParseIP("fe80::1c2b:3cff:fe4d:5e6f"), Port: 51236, Zone: "en0"}
	schwab := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 443}
	reset := func(op string, source net.Addr) *net.OpError {
		return &net.OpError{Op: op, Net: "tcp", Source: source, Addr: schwab, Err: os.NewSyscallError(op, errors.New("connection reset by peer"))}
	}
	lookup := &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.schwabapi.com", Server: "[fe80::1%en0]:53", IsNotFound: true}}
	refused := newError(http.StatusUnauthorized, http.Header{"Content-Type": {"application/json"}}, []byte(`{"message":"Client not authorized"}`))
	refresh := fmt.Errorf("refreshing the access token after 2 tries: %w",
		&url.Error{Op: "Post", URL: "https://api.schwabapi.com/v1/oauth/token", Err: reset("read", global)})
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{
			"tries", fmt.Errorf("%w; the try before got: %w", fmt.Errorf("GET /x after 2 tries: %w", reset("write", local)), fmt.Errorf("GET /x: %w", reset("read", linkLocal))),
			"GET /x after 2 tries: write tcp: write: connection reset by peer; the try before got: GET /x: read tcp: read: connection reset by peer",
		},
		{"a lookup", fmt.Errorf("GET /x: %w", lookup), "GET /x: dial tcp: lookup api.schwabapi.com: no such host"},
		{
			"a refresh's", fmt.Errorf("%w, after the request got: %w", refresh, refused),
			`refreshing the access token after 2 tries: Post "https://api.schwabapi.com/v1/oauth/token": read tcp: read: connection reset by peer, ` +
				"after the request got: HTTP 401 from Schwab: Client not authorized",
		},
		{"joined", errors.Join(reset("write", local), lookup), "write tcp: write: connection reset by peer\ndial tcp: lookup api.schwabapi.com: no such host"},
		{
			"quoted, not wrapped", errors.New("cannot retry err [" + reset("write", global).Error() + "] after the body was written"),
			"cannot retry err [write tcp {address}->{address}: write: connection reset by peer] after the body was written",
		},
		{"Schwab's", refused, "HTTP 401 from Schwab: Client not authorized"},
		{
			"a request's query", fmt.Errorf("GET /trader/v1/x: %w", &url.Error{Op: "Get", URL: "https://api.schwabapi.com/trader/v1/x?symbol=ZQ&types=TRADE", Err: reset("read", local)}),
			`GET /trader/v1/x: Get "https://api.schwabapi.com/trader/v1/x": read tcp: read: connection reset by peer`,
		},
		{
			"Schwab's words", newError(http.StatusBadRequest, http.Header{"Schwab-Client-Correlid": {"corr-zq"}}, []byte(`{"message":"Invalid account number","errors":["zq is not one"]}`)),
			"HTTP 400 from Schwab: Invalid account number; " + said("zq is not one") + " (correlation ID corr-zq)",
		},
		{
			"Schwab's words after its tries", &Error{Status: http.StatusServiceUnavailable, Tries: 3, Message: "zq\u0002down\u0003 now", Code: "zq.fault"},
			"HTTP 503 from Schwab after 3 tries: " + said("zqdown now") + " (zq.fault)",
		},
		{"Schwab's page", newError(http.StatusBadGateway, http.Header{}, []byte("upstream zq")), "HTTP 502 from Schwab: " + said("upstream zq")},
		{"Schwab's code", newError(http.StatusBadRequest, http.Header{}, []byte(`{"error":"invalid_grant"}`)), "HTTP 400 from Schwab: invalid_grant"},
	} {
		got := failureText(tc.err)
		if got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
		for _, addr := range []string{"192.168.1.23", "2001:db8", "fe80:", "203.0.113.7", "51234", "51235", "51236"} {
			if strings.Contains(got, addr) {
				t.Errorf("%s: %q holds %q", tc.name, got, addr)
			}
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
