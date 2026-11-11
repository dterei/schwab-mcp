package schwab

import (
	"cmp"
	"errors"
	"fmt"
	"html"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// idTrace is what a correlation header identifies: the request, which
// Schwab can trace by it. The scrubber gives it a UUID of the fake's
// counter, from the same counter as an error's ID.
const idTrace identifier = "a correlation ID"

// idPathAccount is an account that a request's path names but that no
// answer of the recording does, so that the scrubber has no fake for it,
// and refuses any file that would hold it.
const idPathAccount identifier = "an account that no answer names"

// idPathID is a run of six digits or more that a request's path holds after
// its account where no id goes, as an id of a route that the scrubber does
// not know would be. It has no fake, so a file that would hold it is
// refused, unless the same digits are an identifier that has one.
const idPathID identifier = "an ID where no id goes"

// An identValue is an identifier that a recording holds, and the fake that
// the scrubber puts in its place.
type identValue struct {
	id   identifier
	orig string // as the recording holds it: a JSON string's text, or a number as written
	// fake is what the scrubber writes in its place; "" for one that it has
	// no fake for, so that a file that would hold it is refused.
	fake string
	fold bool // matched in any ASCII case, as identSet.folds says
}

// An identSet is the identifiers of a recording, as pass one collects them,
// each once, with its fake: what the scrubber puts in place of each, and
// what its checks for leaks look for. It finds a value of 6 characters or
// more wherever it is, a shorter one only as a whole token, as the live
// report's scrubber does, so that a short nickname, such as "Ed", does not
// match ordinary words, and a value of one character not at all, since it
// tells nothing; in any case or as it is, as folds says.
type identSet struct {
	values []*identValue
	byKey  map[identKey]*identValue
	// exact and folded hold the values that find finds, by their length in
	// bytes: those matched as they are, and those matched in any case,
	// lower-cased; lens are the lengths, longest first.
	exact, folded map[int]map[string]*identValue
	lens          []int
	// words and subs are what inReal looks in, made as needed: the runs of
	// letters, digits and underscores of the values, and by length the
	// values' substrings of that length, all lower-cased.
	words map[string]bool
	subs  map[int]map[string]bool
	// schwab is Schwab's own words, as schwabWords gives them, lower-cased:
	// a nickname that they hold is matched only as it is.
	schwab string
}

// An identKey is how an identSet holds a value: its kind, and its text,
// lower-cased for one matched in any case.
type identKey struct {
	id   identifier
	orig string
}

// newIdentSet returns a set that holds no identifier.
func newIdentSet() *identSet {
	return &identSet{
		byKey: map[identKey]*identValue{}, exact: map[int]map[string]*identValue{}, folded: map[int]map[string]*identValue{},
	}
}

// key returns the key of the identifier id with the text orig.
func (s *identSet) key(id identifier, orig string) identKey {
	if id == idNickname {
		return identKey{id, orig}
	}
	return identKey{id, asciiLower(orig)}
}

// add returns the identifier id with the text orig, which it adds, without
// a fake, if the set does not hold it.
func (s *identSet) add(id identifier, orig string) *identValue {
	k := s.key(id, orig)
	if v := s.byKey[k]; v != nil {
		return v
	}
	v := &identValue{id: id, orig: orig, fold: s.folds(id, orig)}
	s.byKey[k] = v
	s.values = append(s.values, v)
	s.words, s.subs = nil, nil
	if short(orig) {
		return v
	}
	m, text := s.exact, orig
	if v.fold {
		m, text = s.folded, asciiLower(orig)
	}
	if m[len(text)] == nil {
		m[len(text)] = map[string]*identValue{}
		if !slices.Contains(s.lens, len(text)) {
			s.lens = append(s.lens, len(text))
			slices.SortFunc(s.lens, func(a, b int) int { return b - a })
		}
	}
	if m[len(text)][text] == nil {
		m[len(text)][text] = v
	}
	return v
}

// get returns the identifier id with the text orig, or nil if the set does
// not hold it.
func (s *identSet) get(id identifier, orig string) *identValue {
	return s.byKey[s.key(id, orig)]
}

// folds reports whether the identifier id with the text orig is matched in
// any ASCII case. Every identifier but a nickname is, as hexadecimal digits
// are written in either case. A nickname of 6 characters or more is too,
// so that a kept value that holds Rainy Day Fund as RAINY DAY FUND is
// found, but not one that Schwab's own words hold, such as Brokerage, an
// account's type, which in another case is far likelier Schwab's word than
// the nickname. A shorter nickname, such as Ed, is matched only as it is.
func (s *identSet) folds(id identifier, orig string) bool {
	if id != idNickname {
		return true
	}
	return utf8.RuneCountInString(orig) >= longValue && !strings.Contains(s.schwab, asciiLower(orig))
}

// schwabWords returns Schwab's own words as the scrub policy knows them,
// lower-cased, one to a line: the keys of its tables' rules, its enums'
// values, its wording, and the text of its error pages, pages being
// errorPages'.
func schwabWords(t testing.TB, pages []*errorPage) string {
	t.Helper()
	var words []string
	for _, tab := range policyTables {
		for _, r := range rulesOf(tab, nil, map[*table]bool{}) {
			words = append(words, r.keys...)
			if r.gram != nil {
				words = append(words, r.gram.values...)
			}
		}
	}
	words = append(words, schwabWording...)
	for _, p := range pages {
		words = append(words, html.UnescapeString(string(readTestdata(t, p.file))))
	}
	return asciiLower(strings.Join(words, "\n"))
}

// of returns the identifiers of the set of kind id, in the order they were
// added.
func (s *identSet) of(id identifier) []*identValue {
	var vs []*identValue
	for _, v := range s.values {
		if v.id == id {
			vs = append(vs, v)
		}
	}
	return vs
}

// An identMatch is an identifier found in a text: where it is, and which.
type identMatch struct {
	start, end int
	v          *identValue
}

// find returns the identifiers that text holds, in order, the longest at
// each place, none overlapping. In a number, as JSON writes one, a short
// identifier counts only as the whole number, its sign aside, since a
// number's digits and its point are of one token.
func (s *identSet) find(text string, number bool) []identMatch {
	if len(s.lens) == 0 {
		return nil
	}
	lower := asciiLower(text)
	var out []identMatch
	for i := 0; i < len(text); {
		found := false
		for _, n := range s.lens {
			if i+n > len(text) {
				continue
			}
			v := s.exact[n][text[i:i+n]]
			if v == nil {
				v = s.folded[n][lower[i:i+n]]
			}
			if v != nil && wholeIn(text, i, i+n, number) {
				out = append(out, identMatch{start: i, end: i + n, v: v})
				i += n
				found = true
				break
			}
		}
		if !found {
			_, size := utf8.DecodeRuneInString(text[i:])
			i += size
		}
	}
	return out
}

// wholeIn reports whether text[i:end], a value found in text, counts as
// one: as whole says, or in a number, a short one only as all of it, but a
// minus sign.
func wholeIn(text string, i, end int, number bool) bool {
	if !number || utf8.RuneCountInString(text[i:end]) >= longValue {
		return whole(text, i, end)
	}
	return (i == 0 || i == 1 && text[0] == '-') && end == len(text)
}

// holds returns the first identifier that text holds, as find finds it, or
// nil.
func (s *identSet) holds(text string, number bool) *identValue {
	if ms := s.find(text, number); len(ms) > 0 {
		return ms[0].v
	}
	return nil
}

// numberForms returns the texts in which a JSON number, lit, could show an
// identifier: as written, and, for one whose value is a whole number,
// that number in decimal, so that 2.9470135E7 shows 29470135.
func numberForms(lit []byte) []string {
	forms := []string{string(lit)}
	if r, ok := ratOf(lit); ok && r.IsInt() {
		if whole := r.Num().String(); whole != string(lit) {
			forms = append(forms, whole)
		}
	}
	return forms
}

// collides reports whether fake, a candidate for an identifier's fake,
// could be taken for an identifier of the recording: it holds one, as find
// finds it, or one holds it, as find would find it there. A fake that
// collides would make the checks for leaks refuse good output, or hide a
// value that is there.
func (s *identSet) collides(fake string) bool {
	return len(s.find(fake, false)) > 0 || s.inReal(fake)
}

// inReal reports whether an identifier of the set holds text, in any ASCII
// case, as find would find text there: anywhere, if it has 6 characters or
// more, and else only as a whole token.
func (s *identSet) inReal(text string) bool {
	lower := asciiLower(text)
	if utf8.RuneCountInString(text) >= longValue {
		if s.subs == nil {
			s.subs = map[int]map[string]bool{}
		}
		if s.subs[len(lower)] == nil {
			subs := map[string]bool{}
			for _, v := range s.values {
				r := asciiLower(v.orig)
				for i := 0; i+len(lower) <= len(r); i++ {
					subs[r[i:i+len(lower)]] = true
				}
			}
			s.subs[len(lower)] = subs
		}
		return s.subs[len(lower)][lower]
	}
	if strings.IndexFunc(text, func(r rune) bool { return !wordRune(r) }) >= 0 {
		// A short text that is not one word: look for it in each value.
		for _, v := range s.values {
			r := asciiLower(v.orig)
			for i := 0; ; {
				j := strings.Index(r[i:], lower)
				if j < 0 {
					break
				}
				if whole(r, i+j, i+j+len(lower)) {
					return true
				}
				i += j + 1
			}
		}
		return false
	}
	if s.words == nil {
		s.words = map[string]bool{}
		for _, v := range s.values {
			for _, w := range strings.FieldsFunc(asciiLower(v.orig), func(r rune) bool { return !wordRune(r) }) {
				s.words[w] = true
			}
		}
	}
	return s.words[lower]
}

// accountFakes are the fakes that a recording's accounts take, in turn:
// FixtureAccounts' numbers, then the spares.
func accountFakes() []string {
	var fakes []string
	for _, a := range schwabtest.FixtureAccounts {
		fakes = append(fakes, a.Number)
	}
	return append(fakes, schwabtest.SpareAccountNumbers...)
}

// holdsFakeAccount reports whether text holds a fake account number,
// FixtureAccounts' or a spare, which no other value of a committed fixture
// may hold.
func holdsFakeAccount(text string) bool {
	return slices.ContainsFunc(accountFakes(), func(n string) bool { return strings.Contains(text, n) })
}

// giveAccountFakes gives each of numbers, the recording's account numbers
// in the order they take their fakes, the next of accountFakes. More
// accounts than fakes, or a fake that collides with an identifier of the
// recording, refuses the recording, since an account's fake is not chosen
// but fixed by its place.
func (s *identSet) giveAccountFakes(numbers []*identValue) error {
	fakes := accountFakes()
	if len(numbers) > len(fakes) {
		return fmt.Errorf("the recording holds %d accounts, more than the %d fake account numbers", len(numbers), len(fakes))
	}
	for i, v := range numbers {
		if s.collides(fakes[i]) {
			return fmt.Errorf("the fake of account %d could be taken for an identifier of the recording", i+1)
		}
		v.fake = fakes[i]
	}
	return nil
}

// giveFixedFake gives each of vs the one fake, such as StreamerCustomerID.
// A fake that collides with an identifier of the recording refuses it.
func (s *identSet) giveFixedFake(vs []*identValue, fake string) error {
	if len(vs) > 0 && s.collides(fake) {
		return fmt.Errorf("the fake of %s could be taken for an identifier of the recording", vs[0].id)
	}
	for _, v := range vs {
		v.fake = fake
	}
	return nil
}

// giveIDFakes gives each of vs, the ids of one kind, such as the activity
// IDs, a fake: a whole number of as many digits, below 2^53, in the ids'
// order, so that an id's rank among the others is kept and equal ids have
// equal fakes. A candidate is passed over when it is UnknownActivityID,
// holds a fake account number, could be taken for an identifier of the
// recording, or is in used, the fakes that other ids have; the fakes given
// are added to used.
func (s *identSet) giveIDFakes(vs []*identValue, used map[string]bool) error {
	vs = slices.Clone(vs)
	slices.SortFunc(vs, func(a, b *identValue) int {
		return cmp.Or(cmp.Compare(len(a.orig), len(b.orig)), strings.Compare(a.orig, b.orig))
	})
	var next uint64
	for _, v := range vs {
		digits := len(v.orig)
		if digits > 16 {
			return fmt.Errorf("the recording holds %s of %d digits, more than a fake below 2^53 has", v.id, digits)
		}
		low, high := pow10(digits-1), min(pow10(digits), 1<<53)
		if digits == 1 {
			low = 1
		}
		next = max(next, low)
		for next < high && !s.idCandidate(strconv.FormatUint(next, 10), used) {
			next++
		}
		if next >= high {
			return fmt.Errorf("the recording holds more ids of %d digits, %s among them, than there are fakes for", digits, v.id)
		}
		v.fake = strconv.FormatUint(next, 10)
		used[v.fake] = true
		next++
	}
	return nil
}

// idCandidate reports whether c may be an id's fake: it is not
// UnknownActivityID, holds no fake account number, which no other value of
// a committed fixture may hold, could not be taken for an identifier of the
// recording, and is not in used, the fakes that other ids have.
func (s *identSet) idCandidate(c string, used map[string]bool) bool {
	return !used[c] && c != strconv.FormatInt(schwabtest.UnknownActivityID, 10) && !holdsFakeAccount(c) && !s.collides(c)
}

// pow10 returns 10 to the power n, for n from 0 to 19.
func pow10(n int) uint64 {
	p := uint64(1)
	for range n {
		p *= 10
	}
	return p
}

// giveNicknameFakes gives each of vs, the nicknames in the order the
// recording first holds them, Account N: Account 1, Account 2 and so on,
// passing over an N whose name could be taken for an identifier of the
// recording, such as a nickname that is Account 2 already.
func (s *identSet) giveNicknameFakes(vs []*identValue) error {
	n := 1
	for _, v := range vs {
		for limit := n + 1000; s.collides("Account " + strconv.Itoa(n)); n++ {
			if n == limit {
				return errors.New("every name of the form Account N could be taken for an identifier of the recording")
			}
		}
		v.fake = "Account " + strconv.Itoa(n)
		n++
	}
	return nil
}

// giveUUIDFakes gives each of vs, the errors' IDs and the correlation IDs
// in the order the recording first holds them, a UUID of the fake's
// counter: 00000000-0000-4000-8000-000000000001 and so on, the same for
// the same ID wherever it is, passing over a UUID that could be taken for
// an identifier of the recording.
func (s *identSet) giveUUIDFakes(vs []*identValue) error {
	given := map[string]string{}
	n := 1
	for _, v := range vs {
		if fake, ok := given[asciiLower(v.orig)]; ok {
			v.fake = fake
			continue
		}
		for limit := n + 1000; s.collides(counterID(n)); n++ {
			if n == limit {
				return errors.New("every UUID of the fake's counter could be taken for an identifier of the recording")
			}
		}
		v.fake = counterID(n)
		given[asciiLower(v.orig)] = v.fake
		n++
	}
	return nil
}

// counterID returns the nth UUID of the fake's counter.
func counterID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

// fakeSpelled returns v's fake as the text found, at, spells v: in lower or
// in upper case where a value matched in any case is written all in that
// case there, as a path may spell a hash in lower case, and a security's
// name a nickname in upper case.
func fakeSpelled(v *identValue, at string) string {
	switch upper := strings.ToUpper(at); {
	case !v.fold:
	case at == asciiLower(at) && at != upper:
		return asciiLower(v.fake)
	case at == upper && at != asciiLower(at):
		return strings.ToUpper(v.fake)
	}
	return v.fake
}

// isWholeNumber reports whether lit, a JSON number, is written as a whole
// number from 0, with no sign, point or exponent, as an id or an account
// number is.
func isWholeNumber(lit []byte) bool {
	return len(lit) > 0 && strings.Trim(string(lit), "0123456789") == "" && isJSONNumber(lit)
}

// The set finds each identifier in a text: one of 6 characters or more
// anywhere and in any case, a nickname of 6 too, but a nickname that
// Schwab's own words hold, which only as it is; a shorter one only as a
// whole token, a nickname in its own case alone, and in a number only as
// the whole number; one of one character not at all; and the longest at
// each place.
func TestIdentSetFind(t *testing.T) {
	s := newIdentSet()
	s.schwab = "type\nbrokerage\nbank sweep to brokerage"
	s.add(idAccountNumber, "29470135")
	s.add(idHash, "ABCDEF0123456789")
	s.add(idNickname, "Joint")
	s.add(idNickname, "Options & Margin")
	s.add(idPosition, "766")
	s.add(idNickname, "A")
	s.add(idActivity, "2947013")
	s.add(idNickname, "Brokerage")
	s.add(idNickname, "Savers")
	found := func(text string, number bool) []string {
		var out []string
		for _, m := range s.find(text, number) {
			out = append(out, text[m.start:m.end]+"="+string(m.v.id))
		}
		return out
	}
	for _, tc := range []struct {
		text   string
		number bool
		want   []string
	}{
		{"to 29470135.", false, []string{"29470135=" + string(idAccountNumber)}},
		{"x129470135x", false, []string{"29470135=" + string(idAccountNumber)}},
		{"h=abcdef0123456789&", false, []string{"abcdef0123456789=" + string(idHash)}},
		{"Joint account", false, []string{"Joint=" + string(idNickname)}},
		{"JOINT account, Jointly", false, nil},
		{"Options & Margin", false, []string{"Options & Margin=" + string(idNickname)}},
		{"OPTIONS & MARGIN fund", false, []string{"OPTIONS & MARGIN=" + string(idNickname)}},
		{"SAVERS fund", false, []string{"SAVERS=" + string(idNickname)}},
		{"Brokerage account", false, []string{"Brokerage=" + string(idNickname)}},
		{"BANK SWEEP TO BROKERAGE", false, nil},
		{"Position 766 closed", false, []string{"766=" + string(idPosition)}},
		{"7660 766a A", false, nil},
		{"766", true, []string{"766=" + string(idPosition)}},
		{"-766", true, []string{"766=" + string(idPosition)}},
		{"766.5", true, nil},
		{"766.5", false, []string{"766=" + string(idPosition)}},
		{"1294701350.5", true, []string{"29470135=" + string(idAccountNumber)}},
	} {
		if got := found(tc.text, tc.number); !slices.Equal(got, tc.want) {
			t.Errorf("find(%q, %v) = %q, want %q", tc.text, tc.number, got, tc.want)
		}
	}
	if v := s.holds("2947013", false); v == nil || v.id != idActivity {
		t.Errorf("the shorter of two values in a text: %v", v)
	}
	if got := numberForms([]byte("2.9470135E7")); !slices.Equal(got, []string{"2.9470135E7", "29470135"}) {
		t.Errorf("numberForms = %q", got)
	}
	if s.add(idHash, "abcdef0123456789") != s.get(idHash, "ABCDEF0123456789") || s.get(idNickname, "joint") != nil ||
		s.get(idNickname, "options & margin") != nil {
		t.Error("a hash is not one value in any case, or a nickname is")
	}
	// Schwab's own words are the policy's keys, its enums' values, its
	// wording and its error pages' text.
	words := schwabWords(t, errorPages(t))
	for _, w := range []string{"cashavailablefortrading", "brokerage", "collective_investment", "bank sweep to brokerage", "access denied"} {
		if !strings.Contains(words, w) {
			t.Errorf("Schwab's words lack %q", w)
		}
	}
}

// A fake collides with the recording's identifiers when it holds one or one
// holds it, as find would find it: a long one anywhere, a short one only as
// a whole token.
func TestIdentSetCollides(t *testing.T) {
	s := newIdentSet()
	s.add(idAccountNumber, "29470135")
	s.add(idHash, "AB766CDEF0123456")
	s.add(idPosition, "811")
	s.add(idNickname, "Account 12")
	for fake, want := range map[string]bool{
		"29470135":   true,
		"1294701350": true,
		"9470135":    true,
		"766":        false,
		"811":        true,
		"8110":       false,
		"Account 1":  true,
		"Account 2":  false,
		"cdef012345": true,
		"12345678":   false,
	} {
		if got := s.collides(fake); got != want {
			t.Errorf("collides(%q) = %v, want %v", fake, got, want)
		}
	}
}

// Ids get fakes of as many digits, in their order, equal ids one fake,
// passing over a candidate that could be taken for a value of the
// recording, holds a fake account number or is UnknownActivityID; and
// nicknames and UUIDs get the next of their forms that collides with
// nothing.
func TestIdentSetFakes(t *testing.T) {
	s := newIdentSet()
	for _, orig := range []string{"73508261946", "73508261941", "5", "811", "100", "0123"} {
		s.add(idActivity, orig)
	}
	s.add(idPosition, "101")
	used := map[string]bool{}
	if err := s.giveIDFakes(s.of(idActivity), used); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, v := range s.of(idActivity) {
		got[v.orig] = v.fake
	}
	// 100 is a real id, as is 101, so the 3-digit fakes start at 102.
	want := map[string]string{"5": "1", "100": "102", "811": "103", "0123": "1000", "73508261941": "10000000000", "73508261946": "10000000001"}
	if !maps.Equal(got, want) {
		t.Errorf("fakes %v, want %v", got, want)
	}
	if err := s.giveIDFakes([]*identValue{{id: idOrder, orig: "100"}}, used); err != nil || !used["104"] {
		t.Errorf("the fakes of another kind of id are not passed over: %v, %v", err, used)
	}
	if err := s.giveIDFakes([]*identValue{{id: idOrder, orig: "12345678901234567"}}, used); err == nil {
		t.Error("an id of 17 digits got a fake below 2^53")
	}
	for c, want := range map[string]bool{
		"10000000002": true, "99999999999": false, "12947013500": false, "3" + schwabtest.SpareAccountNumbers[19]: false,
		"104": false, "811": false,
	} {
		if got := s.idCandidate(c, used); got != want {
			t.Errorf("idCandidate(%s) = %v, want %v", c, got, want)
		}
	}
	full := newIdentSet()
	var nine []*identValue
	for i := range 10 {
		nine = append(nine, full.add(idActivity, strconv.Itoa(i)))
	}
	if err := full.giveIDFakes(nine, map[string]bool{}); err == nil {
		t.Error("ten ids of one digit got fakes")
	}

	n := newIdentSet()
	n.add(idNickname, "Account 1")
	names := []*identValue{n.add(idNickname, "Joint"), n.add(idNickname, "IRA")}
	if err := n.giveNicknameFakes(names); err != nil || names[0].fake != "Account 2" || names[1].fake != "Account 3" {
		t.Errorf("nicknames %q, %q, %v", names[0].fake, names[1].fake, err)
	}
	u := newIdentSet()
	u.add(idError, counterID(1))
	ids := []*identValue{u.add(idError, "8F14E45F-CEEA-467A-9B5C-1D2A3B4C5D6E"), u.add(idTrace, "8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e"), u.add(idTrace, "x-1")}
	if err := u.giveUUIDFakes(ids); err != nil || ids[0].fake != counterID(2) || ids[1].fake != counterID(2) || ids[2].fake != counterID(3) {
		t.Errorf("UUIDs %q, %q, %q, %v", ids[0].fake, ids[1].fake, ids[2].fake, err)
	}

	a := newIdentSet()
	var accounts []*identValue
	for i := range 23 {
		accounts = append(accounts, a.add(idAccountNumber, fmt.Sprintf("5%07d", i)))
	}
	if err := a.giveAccountFakes(accounts); err != nil || accounts[0].fake != schwabtest.FixtureAccounts[0].Number || accounts[3].fake != schwabtest.SpareAccountNumbers[0] {
		t.Errorf("accounts: %v, %q, %q", err, accounts[0].fake, accounts[3].fake)
	}
	if err := a.giveAccountFakes(append(accounts, a.add(idAccountNumber, "59999999"))); err == nil {
		t.Error("24 accounts got fakes")
	}
	c := newIdentSet()
	clash := c.add(idAccountNumber, schwabtest.FixtureAccounts[1].Number)
	if err := c.giveAccountFakes([]*identValue{c.add(idAccountNumber, "12345678"), clash}); err == nil {
		t.Error("an account's fake that is another account's number was given")
	}
	for _, tc := range []struct {
		fake string
		fold bool
		at   string
		want string
	}{
		{"ABC1", true, "abc9", "abc1"},
		{"ABC1", true, "ABC9", "ABC1"},
		{"abc1", true, "ABC9", "ABC1"},
		{"Account 1", true, "RAINY DAY FUND", "ACCOUNT 1"},
		{"Account 1", true, "rainy day fund", "account 1"},
		{"Account 1", true, "Rainy day fund", "Account 1"},
		{"Account 1", true, "1234", "Account 1"},
		{"Account 1", false, "joint", "Account 1"},
	} {
		if got := fakeSpelled(&identValue{fake: tc.fake, fold: tc.fold}, tc.at); got != tc.want {
			t.Errorf("the fake %q of %q, folded %v, is spelled %q, want %q", tc.fake, tc.at, tc.fold, got, tc.want)
		}
	}
	for lit, want := range map[string]bool{"29470135": true, "0": true, "-5": false, "5.0": false, "5e1": false, `"5"`: false, "": false} {
		if isWholeNumber([]byte(lit)) != want {
			t.Errorf("isWholeNumber(%s) = %v", lit, !want)
		}
	}
}
