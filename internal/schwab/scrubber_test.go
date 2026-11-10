package schwab

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A recordedAnswer is one answer of a live run's recording, as the recorder
// keeps it under raw/: NNN.json and NNN.body.
type recordedAnswer struct {
	name string    // the number its files are named by, such as 007
	meta rawAnswer // NNN.json: the request, the status, the headers kept
	body []byte    // NNN.body: the bytes as Schwab sent them
}

// An amountsPolicy is what the scrubber does with money, as -scrub.amounts
// says: refuse writes no answer that holds money, keep keeps every number,
// and scale:K multiplies every amount by K.
type amountsPolicy struct {
	mode string // "refuse", "keep" or "scale"
	k    int64  // scale's factor, 2 or more
}

// String returns the policy as -scrub.amounts takes it.
func (p amountsPolicy) String() string {
	if p.mode == "scale" {
		return "scale:" + strconv.FormatInt(p.k, 10)
	}
	return p.mode
}

// parseAmounts reads -scrub.amounts: refuse, keep, or scale:K, K a whole
// number of 2 or more.
func parseAmounts(s string) (amountsPolicy, error) {
	switch s {
	case "refuse", "keep":
		return amountsPolicy{mode: s}, nil
	}
	if k, ok := strings.CutPrefix(s, "scale:"); ok {
		if n, err := strconv.ParseInt(k, 10, 64); err == nil && n >= 2 && k == strconv.FormatInt(n, 10) {
			return amountsPolicy{mode: "scale", k: n}, nil
		}
	}
	return amountsPolicy{}, errors.New("-scrub.amounts takes refuse, keep or scale:K, with K a whole number of 2 or more")
}

// A scrubConfig is what the scrubber takes besides the recording.
type scrubConfig struct {
	amounts amountsPolicy
	version string   // the scrubber's: the commit and whether the tree is modified, or "unknown"
	home    string   // the home folder, which, with the user name, its last element, no output may hold
	paths   []string // other local paths that no output may hold, such as the recording's folder
	// hook, for tests, changes the output once the scrubber has made it and
	// before the gates check it, to show what each gate refuses.
	hook func(o *scrubOutput)
}

// A scrubOutput is what the scrubber makes of a recording: the candidate
// fixtures with their index entries, review.txt, and the shape reports of
// the answers that held money under -scrub.amounts=refuse.
type scrubOutput struct {
	date   string // the recording's day, in UTC, as YYYY-MM-DD
	files  []*scrubbedFile
	review string
	shapes map[string]string // shapes/<name>.txt, by the answer's name
	ids    *identSet         // the recording's identifiers and their fakes, which are never written
}

// A scrubbedFile is a candidate fixture: an answer of the recording,
// scrubbed, with its index entry.
type scrubbedFile struct {
	from  *scrubAnswer // the answer it is made from
	entry schwabtest.Entry
	body  []byte
}

// entriesJSON returns the index entries of the output's files, as
// entries.json holds them, in index.json's form, for the owner to merge.
func (o *scrubOutput) entriesJSON() []byte {
	entries := make([]schwabtest.Entry, len(o.files))
	for i, f := range o.files {
		entries[i] = f.entry
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(entries)
	return b.Bytes()
}

// A scrubAnswer is an answer of the recording as the scrubber reads it, and
// what it makes of it.
type scrubAnswer struct {
	raw   recordedAnswer
	entry schwabtest.Entry // the request and the status, as an index entry gives them
	kind  string           // the kind of answer it is, of policyTables, or "" for none
	table *table           // the kind's table, for JSON
	ext   string           // the extension that ExtensionFor gives its body
	route string           // its request, as review.txt names it, with no value in it

	// What pass two makes of it.
	body    []byte           // the scrubbed body
	out     schwabtest.Entry // its index entry
	refused string           // why it is not written, or ""
	money   bool             // it holds money: a number at a path that the policy scales
	shape   bool             // it is refused for its money alone, so its shape is reported
	review  answerReview
}

// refuse leaves the answer out, for why, which holds no value, unless it is
// left out already.
func (a *scrubAnswer) refuse(why string) {
	if a.refused == "" {
		a.refused = why
	}
}

// answerReview is what review.txt says of an answer besides why it is left
// out, if it is.
type answerReview struct {
	mapped       map[identifier]int // how many values of each kind it maps, in its body and its path
	inside       []insideValue      // each identifier replaced inside a value that is otherwise kept
	placeholders []string           // each free text replaced, as its path and its placeholder
	otherCase    []string           // the paths, as sent, of mapped values at keys in another case
	w8           []string           // the rows of W-8 withholding, by their activity IDs' fakes
}

// An insideValue is an identifier that the scrubber replaced inside a
// value that it otherwise kept, such as a nickname inside a security's
// name: where the value is, as review.txt names it, and what the
// identifier is. A public name can hold a word spelled as a nickname, so
// review.txt lists each for the owner to check.
type insideValue struct {
	where string
	id    identifier
}

// replacedInside records that the identifiers ids were replaced inside the
// value at where.
func (r *answerReview) replacedInside(where string, ids []identifier) {
	for _, id := range ids {
		r.inside = append(r.inside, insideValue{where: where, id: id})
	}
}

// A scrubber makes fixtures of a live run's recording: it reads each
// answer, its path, its query and the values of its headers, gives every
// identifier a fake in pass one, writes each answer with its identifiers
// replaced and its amounts as the policy says in pass two, and checks what
// it wrote by the gates before any of it goes out.
type scrubber struct {
	t       testing.TB
	cfg     scrubConfig
	pages   []*errorPage
	answers []*scrubAnswer
	date    string
	ids     *identSet
	// accounts are the account numbers in the order they take their fakes:
	// those of the recording's first answer of account numbers, in its
	// order, then the rest as they come.
	accounts []*identValue
	// hashNumber and numberHash pair the account numbers and the hashes, as
	// the answers of account numbers do; hashes in upper case.
	hashNumber, numberHash map[string]string
	conflict               string // why the pairs cannot be taken, or ""
	// displays holds the account numbers that each masked number is beside.
	displays map[*identValue][]string
	// placeholders holds, by word, the number given to each free text, by
	// its bytes as sent.
	placeholders map[string]map[string]int
}

// scrubRecording makes fixtures of answers, a live run's recording, in two
// passes, and checks them by the gates, which refuse the whole output on
// any failure. It writes nothing; TestScrub writes what it returns. An
// answer that the policy does not take is left out, and review.txt says
// why.
func scrubRecording(t testing.TB, answers []recordedAnswer, cfg scrubConfig) (*scrubOutput, error) {
	t.Helper()
	s, err := newScrubber(t, answers, cfg)
	if err != nil {
		return nil, err
	}
	for _, a := range s.answers {
		s.scrubAnswer(a)
	}
	s.checkHygiene()
	o := s.output()
	if cfg.hook != nil {
		cfg.hook(o)
	}
	if err := s.checkGates(o); err != nil {
		return nil, err
	}
	return o, nil
}

// newScrubber returns a scrubber of answers that has made pass one: it has
// read every answer and given each identifier its fake. A recording that
// cannot be kept apart from its fakes is refused.
func newScrubber(t testing.TB, answers []recordedAnswer, cfg scrubConfig) (*scrubber, error) {
	t.Helper()
	if len(answers) == 0 {
		return nil, errors.New("the recording holds no answer")
	}
	s := &scrubber{
		t: t, cfg: cfg, pages: errorPages(t), ids: newIdentSet(),
		hashNumber: map[string]string{}, numberHash: map[string]string{}, displays: map[*identValue][]string{},
		placeholders: map[string]map[string]int{},
	}
	s.ids.schwab = schwabWords(t, s.pages)
	date, err := recordingDate(answers)
	if err != nil {
		return nil, err
	}
	s.date = date
	for _, a := range answers {
		s.answers = append(s.answers, s.newAnswer(a))
	}
	if err := s.collect(); err != nil {
		return nil, err
	}
	if err := s.giveFakes(); err != nil {
		return nil, err
	}
	return s, nil
}

// recordingDate returns the day of answers, a recording, in UTC, as
// YYYY-MM-DD: the day its first answer was sent. An answer whose time sent
// is not known leaves the day unknown, which refuses the recording.
func recordingDate(answers []recordedAnswer) (string, error) {
	var first time.Time
	for _, a := range answers {
		if a.meta.Sent.IsZero() {
			return "", fmt.Errorf("answer %s has no time sent", a.name)
		}
		if first.IsZero() || a.meta.Sent.Before(first) {
			first = a.meta.Sent
		}
	}
	return first.UTC().Format(time.DateOnly), nil
}

// newAnswer returns the answer a as the scrubber reads it: its request and
// status as an index entry gives them, and its kind.
func (s *scrubber) newAnswer(a recordedAnswer) *scrubAnswer {
	sa := &scrubAnswer{raw: a, ext: schwabtest.ExtensionFor(a.body), review: answerReview{mapped: map[identifier]int{}}}
	sa.entry.Request.Method, sa.entry.Request.Path, sa.entry.Request.Query = a.meta.Request.Method, a.meta.Request.Path, a.meta.Request.Query
	sa.entry.Response.Status = a.meta.Response.Status
	if kind, ok := policyKindOf(s.pages, sa.entry, a.body); ok {
		sa.kind = kind
		if sa.ext == ".json" {
			sa.table = policyTables[kind]
		}
	}
	sa.route = routeName(sa.entry)
	return sa
}

// pathWord is a part of a path that review.txt shows as it is: a word of
// letters, which may end in a digit, such as transactions or v1. Any other
// part, such as a hash, an id or a symbol, it shows as {…}.
var pathWord = regexp.MustCompile(`^[a-z][A-Za-z]{0,19}[0-9]?$`)

// routeName returns how review.txt names e's request, with no value in it:
// its method and its route, such as GET /trader/v1/accounts/{hash}, or for
// a path that no Route serves, its path with each part that is not a word
// as {…}, and its status.
func routeName(e schwabtest.Entry) string {
	var name string
	if r, ok := e.Route(); ok {
		name = string(r)
	} else {
		parts := strings.Split(e.Request.Path, "/")
		for i, p := range parts {
			if p != "" && !pathWord.MatchString(p) {
				parts[i] = "{…}"
			}
		}
		name = e.Request.Method + " " + strings.Join(parts, "/")
	}
	if e.Request.Query != "" {
		name += "?" + queryKeys(e.Request.Query)
	}
	return name + ", " + strconv.Itoa(e.Response.Status)
}

// queryKeys returns a query's keys without their values, such as
// startDate&endDate&types, each that is not a word as {…}.
func queryKeys(query string) string {
	var keys []string
	for part := range strings.SplitSeq(query, "&") {
		k, _, _ := strings.Cut(part, "=")
		if !pathWord.MatchString(k) {
			k = "{…}"
		}
		keys = append(keys, k)
	}
	return strings.Join(keys, "&")
}

// collect is pass one: it reads every answer, path, query and kept header
// of the recording and adds each identifier to s.ids. The accounts of the
// first answer of account numbers come first, in its order.
func (s *scrubber) collect() error {
	for _, a := range s.answers {
		if a.kind == "accountNumbers" && a.table != nil {
			s.collectJSON(a)
			break
		}
	}
	for _, a := range s.answers {
		switch {
		case a.table != nil:
			s.collectJSON(a)
		case a.ext == ".html" && a.kind == errorsKind:
			s.collectPage(a)
		}
		for _, name := range slices.Sorted(maps.Keys(a.raw.meta.Response.Headers)) {
			if isCorrelationHeader(name) {
				for _, v := range a.raw.meta.Response.Headers[name] {
					if v != "" {
						s.ids.add(idTrace, v)
					}
				}
			}
		}
	}
	// The paths, once every answer has named its accounts.
	for _, a := range s.answers {
		s.collectPath(a)
	}
	if s.conflict != "" {
		return errors.New(s.conflict)
	}
	return nil
}

// collectJSON adds the identifiers of a, a JSON answer of a kind with a
// table: each value at a path that the table maps, of the form its
// identifier takes, and the pairs of accounts and hashes.
func (s *scrubber) collectJSON(a *scrubAnswer) {
	body := a.raw.body
	walkJSON(body, func(sc jsonScalar) {
		r, ok := a.table.rule(sc.path)
		if !ok || r.class != classMap {
			return
		}
		raw := body[sc.start:sc.end]
		switch r.id {
		case idAccountNumber:
			if n, ok := accountText(raw); ok {
				s.addAccount(n)
			}
		case idHash:
			if h, ok := jsonString(raw); ok && h != "" {
				s.ids.add(idHash, h)
				if n, ok := besideAccount(body, sc.in); ok {
					s.pair(n, h)
				}
			}
		case idDisplay:
			if d, ok := jsonString(raw); ok && d != "" {
				v := s.ids.add(idDisplay, d)
				if n, ok := besideAccount(body, sc.in); ok && !slices.Contains(s.displays[v], n) {
					s.displays[v] = append(s.displays[v], n)
				}
			}
		case idActivity, idOrder, idPosition:
			if id, ok := idText(raw); ok {
				s.ids.add(r.id, id)
			}
		default:
			if str, ok := jsonString(raw); ok && str != "" {
				s.ids.add(r.id, str)
			}
		}
	}, nil)
}

// collectPage adds the identifiers of a, an error page: the text of each
// slot that its rule maps, such as Akamai's Reference.
func (s *scrubber) collectPage(a *scrubAnswer) {
	p, m := matchPageSpans(s.pages, a.raw.body)
	if p == nil {
		return
	}
	for i, slot := range p.slots {
		if slot.rule.class != classMap {
			continue
		}
		if text, _, ok := unescapeHTML(a.raw.body[m[2*i+2]:m[2*i+3]]); ok && strings.TrimRight(string(text), ".") != "" {
			s.ids.add(slot.rule.id, strings.TrimRight(string(text), "."))
		}
	}
}

// collectPath adds the identifiers of a's path, which may appear in no
// answer: an account that no answer names, which has no fake; the id of a
// transaction or an order asked for by its id, as with an id that Schwab
// does not know, which takes a fake as the same id in a body would; and
// any other run of six digits or more after the account, an ID where no id
// goes, which has none. UnknownActivityID, which the Extras probe asks for,
// is no transaction's, and stays.
func (s *scrubber) collectPath(a *scrubAnswer) {
	rest, ok := strings.CutPrefix(a.raw.meta.Request.Path, "/trader/v1/accounts/")
	if !ok {
		return
	}
	parts := strings.Split(rest, "/")
	for i, p := range parts {
		parts[i] = unescapedPart(p)
	}
	switch account := parts[0]; {
	case account == "" || account == "accountNumbers" || account == schwabtest.UnknownHash:
	case s.ids.get(idHash, account) != nil, s.ids.get(idAccountNumber, account) != nil:
	default:
		s.ids.add(idPathAccount, account)
	}
	after := parts[1:]
	for i, part := range after {
		switch kind := pathIDKind(after, i); {
		case kind == idActivity && part == strconv.FormatInt(schwabtest.UnknownActivityID, 10):
		case kind != "" && isDigits(part):
			s.ids.add(kind, part)
		default:
			for _, run := range idRuns(part) {
				s.ids.add(idPathID, run)
			}
		}
	}
}

// unescapedPart returns a part of an escaped path, unescaped, or as it is
// if it cannot be.
func unescapedPart(p string) string {
	if u, err := url.PathUnescape(p); err == nil {
		return u
	}
	return p
}

// isDigits reports whether s is one or more decimal digits.
func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// accountText returns an account number as raw holds it: a string's text,
// or a whole number as written; and whether it is one of those.
func accountText(raw []byte) (string, bool) {
	if s, ok := jsonString(raw); ok {
		return s, s != ""
	}
	return string(raw), isWholeNumber(raw)
}

// idText returns an id as raw holds it, a whole number or a string of
// digits, and whether it is either.
func idText(raw []byte) (string, bool) {
	if s, ok := jsonString(raw); ok {
		return s, isDigits(s)
	}
	return string(raw), isWholeNumber(raw)
}

// besideAccount returns the account number of in, an object of body that
// holds a value: its last accountNumber, as encoding/json takes the last,
// as accountText reads it; and whether it has one.
func besideAccount(body []byte, in *jsonObject) (string, bool) {
	span, ok := in.last("accountNumber")
	if !ok {
		return "", false
	}
	return accountText(body[span[0]:span[1]])
}

// addAccount adds an account number, which takes the next fake if it is new.
func (s *scrubber) addAccount(number string) {
	if s.ids.get(idAccountNumber, number) == nil {
		s.accounts = append(s.accounts, s.ids.add(idAccountNumber, number))
	}
}

// pair records that the answers of account numbers give number the hash h.
// A hash of two accounts, or an account of two hashes, as after a change of
// hashes between two answers, refuses the recording: a hash's fake is
// HashFor of its account's fake, and the two could not both be kept apart.
func (s *scrubber) pair(number, h string) {
	h = strings.ToUpper(h)
	if n, ok := s.hashNumber[h]; ok && n != number {
		s.conflict = "the recording gives one hash to two accounts; scrub a recording of one login"
	}
	if g, ok := s.numberHash[number]; ok && g != h {
		s.conflict = "the recording gives an account two hashes; scrub a recording of one login"
	}
	s.hashNumber[h], s.numberHash[number] = number, h
}

// fakeAccount returns the fake of an account number, or "".
func (s *scrubber) fakeAccount(number string) string {
	if v := s.ids.get(idAccountNumber, number); v != nil {
		return v.fake
	}
	return ""
}

// giveFakes gives each identifier of pass one its fake, by its place, never
// by its value: the accounts FixtureAccounts' numbers and then the spares',
// in order, and their hashes and masked numbers from those; ids fakes of as
// many digits in the same order; nicknames Account N; the streamer's IDs
// their fixed fakes; errors' and correlation IDs UUIDs of the counter; and
// Akamai's References AkamaiReference. No fake could be taken for an
// identifier of the recording.
func (s *scrubber) giveFakes() error {
	if err := s.ids.giveAccountFakes(s.accounts); err != nil {
		return err
	}
	for _, v := range s.ids.of(idHash) {
		if n, ok := s.hashNumber[strings.ToUpper(v.orig)]; ok {
			if v.fake = schwabtest.HashFor(s.fakeAccount(n)); s.ids.collides(v.fake) {
				return errors.New("the fake of an account's hash could be taken for an identifier of the recording")
			}
		}
	}
	for _, v := range s.ids.of(idDisplay) {
		var fakes []string
		for _, n := range s.displays[v] {
			if fake, why := s.displayFake(v.orig, n); why == "" && !slices.Contains(fakes, fake) {
				fakes = append(fakes, fake)
			}
		}
		if len(fakes) == 1 {
			if v.fake = fakes[0]; s.ids.collides(v.fake) {
				return errors.New("the fake of a masked account number could be taken for an identifier of the recording")
			}
		}
	}
	used := map[string]bool{}
	for _, id := range []identifier{idActivity, idOrder, idPosition} {
		if err := s.ids.giveIDFakes(s.ids.of(id), used); err != nil {
			return err
		}
	}
	if err := s.ids.giveNicknameFakes(s.ids.of(idNickname)); err != nil {
		return err
	}
	var uuids []*identValue
	for _, v := range s.ids.values {
		if v.id == idError || v.id == idTrace {
			uuids = append(uuids, v)
		}
	}
	if err := s.ids.giveUUIDFakes(uuids); err != nil {
		return err
	}
	for _, f := range []struct {
		id   identifier
		fake string
	}{
		{idCustomer, schwabtest.StreamerCustomerID},
		{idCorrelation, schwabtest.StreamerCorrelID},
		{idReference, schwabtest.AkamaiReference},
	} {
		if err := s.ids.giveFixedFake(s.ids.of(f.id), f.fake); err != nil {
			return err
		}
	}
	return nil
}

// displayFake returns the fake of orig, a masked account number beside the
// account number number: orig with the digits it ends in, which must end
// number, in place of as many digits that end number's fake, so that
// ...135 beside 29470135 is ...398 beside its fake 61803398. Schwab's form
// is kept, not imposed. It returns why there is none: orig does not end
// with digits of number, or holds other digits, or ends with more digits
// than the fake has.
func (s *scrubber) displayFake(orig, number string) (fake, why string) {
	tail := trailingDigits.FindString(orig)
	switch {
	case tail == "" || !strings.HasSuffix(number, tail):
		return "", "a masked account number that does not end with digits of the account number beside it"
	case strings.ContainsAny(orig[:len(orig)-len(tail)], "0123456789"):
		return "", "a masked account number that holds other digits"
	}
	f := s.fakeAccount(number)
	if len(tail) > len(f) {
		return "", "a masked account number that ends with more digits than its fake has"
	}
	return orig[:len(orig)-len(tail)] + f[len(f)-len(tail):], ""
}

// scrubAnswer is pass two for one answer: it writes the answer's body with
// its identifiers replaced and its amounts as the policy says, and its
// index entry, or leaves it out, with why.
func (s *scrubber) scrubAnswer(a *scrubAnswer) {
	switch {
	case a.raw.meta.Response.CutShort:
		a.refuse("its body was cut short")
	case a.kind == "":
		a.refuse("an answer of no kind that the scrub policy covers")
	case a.kind != errorsKind && a.ext != ".json":
		a.refuse("an answer of " + a.kind + " that is not JSON")
	}
	if a.refused != "" {
		return
	}
	switch a.ext {
	case ".json":
		s.scrubJSON(a)
	case ".html":
		s.scrubPage(a)
	default:
		a.body = bytes.Clone(a.raw.body) // white space, as Schwab's 429 sends
	}
	if a.refused == "" && a.money && s.cfg.amounts.mode == "refuse" {
		a.refuse("it holds money, which -scrub.amounts=refuse does not write; its shape is in shapes/" + a.raw.name + ".txt")
		a.shape = true
	}
	if a.refused == "" {
		s.scrubEntry(a)
	}
	if a.refused == "" {
		if where, v := s.leakIn(a.body, a.table, a.out); v != nil {
			a.refuse(where + ": " + string(v.id) + " that the scrubber could not replace")
		}
	}
	if a.refused == "" && a.ext == ".json" {
		if where := accountInValue(a.body); where != "" {
			a.refuse(where + ": holds an account number, which TestTestdataAccountNumbersStandAlone refuses in any value but an account number")
		}
	}
	if k, ok := kindByName(a.kind); a.refused == "" && ok {
		var failing []string
		for _, f := range checkAnswer(k.name, a.body) {
			if f.fails {
				failing = append(failing, f.String())
			}
		}
		if len(failing) > 0 {
			a.refuse("checkAnswer finds what TestCheckFixtures fails on: " + strings.Join(failing, "; "))
		}
	}
}

// accountInValue returns where body, a JSON answer as written, holds a fake
// account number in a value other than an account number, at an
// accountNumber key as a string or a whole number: as Schwab's wording can,
// once its digits are replaced, and which no committed fixture may, so that
// a test can search for account numbers as text. It returns "" for none.
func accountInValue(body []byte) string {
	where := ""
	walkJSON(body, func(sc jsonScalar) {
		raw := body[sc.start:sc.end]
		text, isString := jsonString(raw)
		if !isString {
			text = string(raw)
		}
		atNumber := len(sc.path) > 0 && strings.EqualFold(sc.path[len(sc.path)-1], "accountNumber") && (isString || isWholeNumber(raw))
		if where == "" && !atNumber && holdsFakeAccount(text) {
			where = pathName(sc.path)
		}
	}, nil)
	return where
}

// scrubJSON writes a, a JSON answer, value by value as its kind's table
// says, splicing each new value into Schwab's bytes so that key order,
// repeated keys, white space, number literals and escapes stay as they
// were. A scalar at a path that no rule covers, or one that its rule does
// not take, leaves the answer out. A null holds no value, and stays.
func (s *scrubber) scrubJSON(a *scrubAnswer) {
	body := a.raw.body
	var splices []splice
	walkJSON(body, func(sc jsonScalar) {
		raw := body[sc.start:sc.end]
		if a.refused != "" || string(raw) == "null" {
			return
		}
		where := pathName(sc.path)
		r, ok := a.table.rule(sc.path)
		if !ok {
			a.refuse(where + ": no rule of the scrub policy for " + a.kind + " covers it")
			return
		}
		var out []byte
		var why string
		switch r.class {
		case classMap:
			out, why = s.mapValue(a, r.id, sc, raw)
			if why == "" && !slices.Equal(sc.path, r.keys) {
				a.review.otherCase = append(a.review.otherCase, where)
			}
		case classScale:
			out, why = s.scaleValue(a, raw)
		case classKeep:
			out, why = s.keepValue(a, r, where, raw)
		case classPlaceholder:
			out, why = s.placeholderValue(a, r, sc, raw)
		}
		if why != "" {
			a.refuse(where + ": " + why)
			return
		}
		if !bytes.Equal(out, raw) {
			splices = append(splices, splice{start: sc.start, end: sc.end, with: out})
		}
	}, nil)
	if a.refused == "" {
		a.body = bytes.Clone(applySplices(body, splices))
	}
}

// mapValue returns the fake of raw, a value at a path that maps it to id,
// in sc's object of a's body, of the same JSON kind: a string for a string
// and a number for a number. It returns why there is none instead.
func (s *scrubber) mapValue(a *scrubAnswer, id identifier, sc jsonScalar, raw []byte) ([]byte, string) {
	str, isString := jsonString(raw)
	var v *identValue
	switch id {
	case idAccountNumber:
		n, ok := accountText(raw)
		if !ok {
			return nil, "an account number that is neither a string nor a whole number"
		}
		v = s.ids.get(id, n)
	case idHash:
		number, ok := besideAccount(a.raw.body, sc.in)
		switch {
		case !isString:
			return nil, "a hash that is not a string"
		case !ok:
			return nil, "a hash with no account number beside it"
		case s.hashNumber[strings.ToUpper(str)] != number:
			return nil, "a hash beside an account number other than its own"
		}
		v = s.ids.get(id, str)
	case idDisplay:
		number, ok := besideAccount(a.raw.body, sc.in)
		if !isString || !ok {
			return nil, "a masked account number with no account number beside it"
		}
		fake, why := s.displayFake(str, number)
		if why != "" {
			return nil, why
		}
		a.review.mapped[id]++
		return jsonText(fake), ""
	case idActivity, idOrder, idPosition:
		n, ok := idText(raw)
		if !ok {
			return nil, string(id) + " that is neither a whole number nor a string of digits"
		}
		v = s.ids.get(id, n)
	default:
		if !isString {
			return nil, string(id) + " that is not a string"
		}
		v = s.ids.get(id, str)
	}
	if v == nil || v.fake == "" {
		return nil, string(id) + " that has no fake"
	}
	a.review.mapped[id]++
	if isString {
		return jsonText(v.fake), ""
	}
	return []byte(v.fake), ""
}

// scaleValue returns raw, money, as the amounts policy has it: as it is
// under refuse, which writes no answer that holds money, and keep; and
// under scale:K multiplied by K, a zero as it is. Money that is not a
// number, or a number with an exponent under scale:K, which scaling could
// make into a form Java never writes, leaves the answer out.
func (s *scrubber) scaleValue(a *scrubAnswer, raw []byte) ([]byte, string) {
	if !isJSONNumber(raw) {
		return nil, "money that is not a number"
	}
	a.money = true
	if s.cfg.amounts.mode != "scale" || isZeroLiteral(raw) {
		return raw, ""
	}
	if hasExponent(raw) {
		return nil, "money written with an exponent, which scaling could make into a form Java never writes"
	}
	return scaleLiteral(raw, s.cfg.amounts.k), ""
}

// keepValue returns raw, a value that r keeps, at where in a's body, as it
// is, a string with any identifier in it replaced, which a's review
// records; or why it cannot be kept: it is not of r's grammar, or holds an
// identifier that cannot be replaced. A value on its enum's closed list,
// such as CASH, is Schwab's own word, which names no one, even where a
// nickname is spelled the same, so nothing in it is replaced.
func (s *scrubber) keepValue(a *scrubAnswer, r rule, where string, raw []byte) ([]byte, string) {
	if r.onClosedList(raw) {
		return raw, ""
	}
	out := raw
	var ids []identifier
	if raw[0] == '"' {
		var why string
		if out, ids, why = s.replaceInJSONString(raw); why != "" {
			return nil, why
		}
	}
	if !r.gram.match(out) {
		return nil, "not " + r.gram.name
	}
	a.review.replacedInside(where, ids)
	return out, ""
}

// placeholderValue returns raw, free text, as it is when it is on its part
// of Schwab's wording, any identifier in it replaced, and else its
// placeholder, such as DESCRIPTION 3, the same for the same text. A row's
// description that tells of W-8 withholding is kept for review.txt.
func (s *scrubber) placeholderValue(a *scrubAnswer, r rule, sc jsonScalar, raw []byte) ([]byte, string) {
	text, ok := jsonString(raw)
	if !ok {
		return nil, r.text.name + " that is not a string"
	}
	if r.text == descriptionText && strings.Contains(strings.ToUpper(text), "W-8") {
		row := "a row with no activity ID"
		if span, ok := sc.in.last("activityId"); ok {
			if id, ok := idText(a.raw.body[span[0]:span[1]]); ok && s.ids.get(idActivity, id) != nil {
				row = "the row of activity ID " + s.ids.get(idActivity, id).fake
			}
		}
		a.review.w8 = append(a.review.w8, row)
	}
	// Wording that held an account number, as the fraction of a time that
	// a message quotes can, would hold a fake one once it is replaced,
	// which no value but an account number may.
	if out, ids, why := s.replaceInJSONString(raw); why == "" {
		if kept, _ := jsonString(out); r.text.isWording(kept) && !holdsFakeAccount(kept) {
			a.review.replacedInside(pathName(sc.path), ids)
			return out, ""
		}
	}
	numbers := s.placeholders[r.text.word]
	if numbers == nil {
		numbers = map[string]int{}
		s.placeholders[r.text.word] = numbers
	}
	n, ok := numbers[string(raw)]
	if !ok {
		n = len(numbers) + 1
		numbers[string(raw)] = n
	}
	placeholder := r.text.word + " " + strconv.Itoa(n)
	a.review.placeholders = append(a.review.placeholders, pathName(sc.path)+": "+placeholder)
	return jsonText(placeholder), ""
}

// replaceInJSONString returns raw, a JSON string, with each identifier in
// its text replaced by its fake, spliced into its bytes where the
// identifier is written as it is, so that every other byte, escapes
// included, stays, and what each identifier replaced is. It never encodes
// the string again, which would change bytes outside the identifier, such
// as & into \u0026. An identifier that is written only with escapes, such
// as a digit as \u0032, or that has no fake, gives why instead.
func (s *scrubber) replaceInJSONString(raw []byte) ([]byte, []identifier, string) {
	text, at := unescapeJSON(raw)
	var splices []splice
	var ids []identifier
	for _, m := range s.ids.find(string(text), false) {
		if m.v.fake == "" {
			return nil, nil, "holds " + string(m.v.id) + ", which has no fake"
		}
		start, end, ok := rawSpan(at, m.start, m.end)
		if !ok {
			return nil, nil, "holds " + string(m.v.id) + " written with escapes"
		}
		splices = append(splices, splice{start: start, end: end, with: []byte(fakeSpelled(m.v, string(text[m.start:m.end])))})
		ids = append(ids, m.v.id)
	}
	return applySplices(raw, splices), ids, ""
}

// replaceInText returns text, a path, a query or a header's value, with
// each identifier in it replaced by its fake, and what each identifier
// replaced is; or why it cannot be: it holds one that has no fake. One
// written with escapes is found by the leak check, which unescapes.
func (s *scrubber) replaceInText(text string) (string, []identifier, string) {
	var splices []splice
	var ids []identifier
	for _, m := range s.ids.find(text, false) {
		if m.v.fake == "" {
			return "", nil, "holds " + string(m.v.id) + ", which has no fake"
		}
		splices = append(splices, splice{start: m.start, end: m.end, with: []byte(fakeSpelled(m.v, text[m.start:m.end]))})
		ids = append(ids, m.v.id)
	}
	return string(applySplices([]byte(text), splices)), ids, ""
}

// matchPageSpans returns the page of pages that body is, and the spans of
// its slots in body, as regexp.FindSubmatchIndex gives them; or nil.
func matchPageSpans(pages []*errorPage, body []byte) (*errorPage, []int) {
	for _, p := range pages {
		if m := p.re.FindSubmatchIndex(body); m != nil {
			return p, m
		}
	}
	return nil, nil
}

// scrubPage writes a, an error page, slot by slot, each in the page's own
// spelling: Akamai's Reference is AkamaiReference, its dots written &#46;
// as the page writes them; the address it quotes keeps its form with each
// identifier in it replaced, such as an account's hash; and the Whitelabel
// page's time is kept. Every other byte of the page is the template's.
func (s *scrubber) scrubPage(a *scrubAnswer) {
	body := a.raw.body
	p, m := matchPageSpans(s.pages, body)
	if p == nil {
		a.refuse("a page that is none of testdata/errors")
		return
	}
	var splices []splice
	for i, slot := range p.slots {
		start, end := m[2*i+2], m[2*i+3]
		raw := body[start:end]
		text, at, ok := unescapeHTML(raw)
		if !ok {
			a.refuse(slot.rule.path + ": text written in a way html.UnescapeString reads otherwise")
			return
		}
		var out string
		switch slot.rule.class {
		case classMap:
			// A dot after it ends a sentence, as errors.go reads it.
			value := strings.TrimRight(string(text), ".")
			v := s.ids.get(slot.rule.id, value)
			if v == nil || v.fake == "" {
				a.refuse(slot.rule.path + ": " + string(slot.rule.id) + " that has no fake")
				return
			}
			out = spellAs(v.fake+string(text[len(value):]), raw)
			a.review.mapped[slot.rule.id]++
		default:
			var page []splice
			var ids []identifier
			for _, found := range s.ids.find(string(text), false) {
				rs, re, ok := rawSpan(at, found.start, found.end)
				switch {
				case found.v.fake == "":
					a.refuse(slot.rule.path + ": holds " + string(found.v.id) + ", which has no fake")
				case !ok:
					a.refuse(slot.rule.path + ": holds " + string(found.v.id) + " written with character references")
				}
				if a.refused != "" {
					return
				}
				page = append(page, splice{start: rs, end: re, with: []byte(fakeSpelled(found.v, string(text[found.start:found.end])))})
				ids = append(ids, found.v.id)
			}
			out = string(applySplices(raw, page))
			if !slot.rule.gram.match(jsonText(html.UnescapeString(out))) {
				a.refuse(slot.rule.path + ": not " + slot.rule.gram.name)
				return
			}
			a.review.replacedInside(slot.rule.path, ids)
		}
		splices = append(splices, splice{start: start, end: end, with: []byte(out)})
	}
	a.body = bytes.Clone(applySplices(body, splices))
}

// scrubEntry writes a's index entry: its request's path and query with
// each identifier replaced, its status, the values of the headers that
// KeptHeader keeps, a correlation ID's fake in place of its own, and the
// names alone of the rest, Content-Length among them, whose value would be
// stale once a value changes length. a's review counts the path's
// identifiers with those its body maps, since a path names its account and
// its id by design, and lists those of the query and the headers' values.
func (s *scrubber) scrubEntry(a *scrubAnswer) {
	m := a.raw.meta
	var e schwabtest.Entry
	e.File = "recorded/" + s.date + "/" + a.raw.name + schwabtest.ExtensionFor(a.body)
	e.Request.Method = m.Request.Method
	var ids []identifier
	var why string
	if e.Request.Path, ids, why = s.replaceInText(m.Request.Path); why != "" {
		a.refuse("the request's path " + why)
		return
	}
	for _, id := range ids {
		a.review.mapped[id]++
	}
	if e.Request.Query, ids, why = s.replaceInText(m.Request.Query); why != "" {
		a.refuse("the request's query " + why)
		return
	}
	a.review.replacedInside("the request's query", ids)
	e.Response.Status, e.Response.Proto, e.Response.Uncompressed = m.Response.Status, m.Response.Proto, m.Response.Uncompressed
	e.Response.Headers = map[string][]string{}
	others := slices.Clone(m.Response.OtherHeaders)
	for _, name := range slices.Sorted(maps.Keys(m.Response.Headers)) {
		if !schwabtest.KeptHeader(name) || strings.EqualFold(name, "Content-Length") {
			others = append(others, name)
			continue
		}
		values := []string{}
		for _, value := range m.Response.Headers[name] {
			out := value
			switch {
			case value == "":
			case isCorrelationHeader(name):
				v := s.ids.get(idTrace, value)
				if v == nil || v.fake == "" {
					a.refuse("the header " + name + ": " + string(idTrace) + " that has no fake")
					return
				}
				out = v.fake
				a.review.mapped[idTrace]++
			default:
				if out, ids, why = s.replaceInText(value); why != "" {
					a.refuse("the header " + name + " " + why)
					return
				}
				a.review.replacedInside("the header "+name, ids)
			}
			values = append(values, out)
		}
		e.Response.Headers[name] = values
	}
	e.Response.OtherHeaders = []string{}
	for _, name := range others {
		if !slices.ContainsFunc(e.Response.OtherHeaders, func(n string) bool { return strings.EqualFold(n, name) }) {
			e.Response.OtherHeaders = append(e.Response.OtherHeaders, name)
		}
	}
	e.Source, e.Recorded = "recorded", s.date
	e.Scrub = s.cfg.version + "; amounts " + s.cfg.amounts.String()
	e.CR = bytes.Count(a.body, []byte("\r"))
	a.out = e
}

// A leakText is a text of an output that the checks for leaks search, in
// the form they search it: decoded, or unescaped.
type leakText struct {
	where  string // where it is, as a problem names it, with no value
	text   string
	number bool // a JSON number, in which a short identifier counts only as the whole number
	long   bool // only identifiers of 6 characters or more count: the raw text of a body
	key    bool // a JSON key, one of Schwab's names rather than a value
}

// textsOf returns the texts of body, an answer as written, that the checks
// for leaks search, tab being its kind's table, or nil: the raw bytes, for
// identifiers of 6 characters or more; for JSON, each key and string
// decoded, each unescaped as a path and as a query too, and each number as
// written and as a whole number; for a page, its text unescaped, and that
// unescaped as a path and as a query. A value on its enum's closed list,
// such as CASH, is Schwab's own word, which names no one, though a
// nickname may be spelled the same: it is searched in no form, its bytes
// left out of the raw bytes too.
func textsOf(body []byte, tab *table) []leakText {
	var texts []leakText
	addForms := func(where, text string, key bool) {
		for _, f := range unescapedForms(text) {
			texts = append(texts, leakText{where: where, text: f, key: key})
		}
	}
	raw := body
	switch schwabtest.ExtensionFor(body) {
	case ".json":
		var schwab []splice
		walkJSON(body, func(sc jsonScalar) {
			lit := body[sc.start:sc.end]
			if r, ok := tab.rule(sc.path); ok && r.onClosedList(lit) {
				schwab = append(schwab, splice{start: sc.start + 1, end: sc.end - 1})
				return
			}
			if str, ok := jsonString(lit); ok {
				addForms(pathName(sc.path), str, false)
			} else if isJSONNumber(lit) {
				for _, f := range numberForms(lit) {
					texts = append(texts, leakText{where: pathName(sc.path), text: f, number: true})
				}
			}
		}, func(path []string, o *jsonObject) {
			for _, k := range o.keys {
				addForms(pathName(append(slices.Clip(path), k))+", its key", k, true)
			}
		})
		raw = applySplices(body, schwab)
	case ".html":
		addForms("the page", html.UnescapeString(string(body)), false)
	}
	// The bytes as written, last, so that a value found is named by where
	// it is first.
	return append(texts, leakText{where: "the answer", text: string(raw), long: true})
}

// unescapedForms returns text, and text unescaped as a path and as a query
// again and again until it stops changing, each form once: so that what a
// text holds is found however many times it was escaped.
func unescapedForms(text string) []string {
	forms := written(text, url.PathUnescape)
	for _, f := range written(text, url.QueryUnescape) {
		if !slices.Contains(forms, f) {
			forms = append(forms, f)
		}
	}
	return forms
}

// entryTexts returns the texts of e, an index entry as written, that the
// checks for leaks search: its file's name, its request's path and query,
// each unescaped too, and its headers' names and values.
func entryTexts(e schwabtest.Entry) []leakText {
	var texts []leakText
	add := func(where, text string) {
		for _, f := range unescapedForms(text) {
			texts = append(texts, leakText{where: where, text: f})
		}
	}
	add("its name", e.File)
	add("the request's path", e.Request.Path)
	add("the request's query", e.Request.Query)
	for _, name := range slices.Sorted(maps.Keys(e.Response.Headers)) {
		add("a header's name", name)
		for _, v := range e.Response.Headers[name] {
			add("the header "+name, v)
		}
	}
	for _, name := range e.Response.OtherHeaders {
		add("a header's name", name)
	}
	add("its source", e.Source+" "+e.Recorded+" "+e.Scrub)
	return texts
}

// leakIn returns where body, an answer as written of a kind whose table is
// tab, or e, its index entry, holds an identifier of the recording, and
// which; or "", nil.
func (s *scrubber) leakIn(body []byte, tab *table, e schwabtest.Entry) (string, *identValue) {
	for _, t := range slices.Concat(textsOf(body, tab), entryTexts(e)) {
		if v := s.leakInText(t); v != nil {
			return t.where, v
		}
	}
	return "", nil
}

// leakInText returns the first identifier of the recording that t holds, or
// nil.
func (s *scrubber) leakInText(t leakText) *identValue {
	for _, m := range s.ids.find(t.text, t.number) {
		if !t.long || utf8.RuneCountInString(m.v.orig) >= longValue {
			return m.v
		}
	}
	return nil
}

// checkHygiene holds what pass two wrote to the value hygiene tests, which
// CI runs over every committed fixture, and leaves out each file that they
// find a problem in, with the problem, which names no value: so that what
// the owner commits passes them. Leaving one out can leave another with a
// problem, such as a path to a transaction of a row that is no longer
// written, so it checks again until nothing is found.
func (s *scrubber) checkHygiene() {
	for {
		var files []indexedFile
		byFile := map[string]*scrubAnswer{}
		for _, a := range s.answers {
			if a.refused == "" {
				files = append(files, indexedFile{entry: a.out, body: a.body})
				byFile[a.out.File] = a
			}
		}
		left := false
		for _, p := range checkValues(s.t, files) {
			file, what, _ := strings.Cut(p, ": ")
			if a := byFile[file]; a != nil {
				a.refuse("the value hygiene tests would fail on it: " + what)
				left = true
			}
		}
		if !left {
			return
		}
	}
}

// output returns what pass two made: the files written, review.txt and the
// shape reports.
func (s *scrubber) output() *scrubOutput {
	o := &scrubOutput{date: s.date, shapes: map[string]string{}, ids: s.ids}
	for _, a := range s.answers {
		switch {
		case a.refused == "":
			o.files = append(o.files, &scrubbedFile{from: a, entry: a.out, body: a.body})
		case a.shape:
			o.shapes[a.raw.name] = s.mask(shapeReport(a))
		}
	}
	o.review = s.mask(s.reviewText(o))
	return o
}

// mask returns text, a line of review.txt or of a shape, with each
// identifier of the recording in it shown as {…}. Neither holds a value by
// design, but each names key paths as Schwab sent them, and a key, such as
// one Schwab sent in place of a value, could be an identifier; gate (v)
// checks what is left.
func (s *scrubber) mask(text string) string {
	var splices []splice
	for _, m := range s.ids.find(text, false) {
		splices = append(splices, splice{start: m.start, end: m.end, with: []byte("{…}")})
	}
	return string(applySplices([]byte(text), splices))
}

// reviewText returns review.txt: what was written and what was mapped in
// each file, which answers were left out and why, the identifiers replaced
// inside values kept otherwise, the placeholders, the mapped values at
// keys in another case, the rows of W-8 withholding to decide on, and what
// scaling hides. It holds key paths, routes, counts and placeholders, never
// a value of the recording.
func (s *scrubber) reviewText(o *scrubOutput) string {
	var b strings.Builder
	written := len(o.files)
	fmt.Fprintf(&b, "# The scrubbed recording of %s\n\n", s.date)
	fmt.Fprintf(&b, "Scrubbed by %s, with -scrub.amounts=%s. %s: %d written, %d left out.\n\n",
		s.cfg.version, s.cfg.amounts, plural(len(s.answers), "answer"), written, len(s.answers)-written)
	b.WriteString("This file names answers by their numbers in the recording and requests by their\n" +
		"routes; it holds no value of the recording. Before you commit the files:\n\n" +
		"1. Read this file whole, and the files themselves.\n" +
		"2. Decide on each row of W-8 withholding below: keep it, or leave its file out.\n" +
		"3. Copy recorded/" + s.date + "/ to internal/schwab/schwabtest/testdata/recorded/" + s.date + "/,\n" +
		"   which must not be there yet: the module keeps one recording a day, scrubbed whole so that\n" +
		"   its fakes agree. If one is there, this one may only replace it: first remove that folder\n" +
		"   and its entries in index.json. Then add the entries of entries.json to the list in\n" +
		"   internal/schwab/schwabtest/testdata/index.json, the one index of every fixture.\n" +
		"4. Run go test ./... and commit nothing that fails.\n")
	section := func(title string, lines []string) {
		if len(lines) == 0 {
			return
		}
		b.WriteString("\n## " + title + "\n\n")
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
	}
	var wrote, left, inside, placeholders, otherCase, w8 []string
	for _, a := range s.answers {
		for _, p := range a.review.otherCase {
			otherCase = append(otherCase, a.raw.name+": "+p)
		}
		if a.refused != "" {
			left = append(left, a.raw.name+": "+a.route+": "+a.refused)
			continue
		}
		file := path.Base(a.out.File)
		line := file + ": " + a.route
		if a.kind != "" {
			line += " (" + a.kind + ")"
		}
		if mapped := mappedCounts(a.review.mapped); mapped != "" {
			line += "; mapped " + mapped
		}
		wrote = append(wrote, line)
		for _, l := range insideCounts(a.review.inside) {
			inside = append(inside, file+": "+l)
		}
		for _, p := range a.review.placeholders {
			placeholders = append(placeholders, file+": "+p)
		}
		for _, r := range a.review.w8 {
			w8 = append(w8, file+": "+r)
		}
	}
	section("Written to recorded/"+s.date+"/", wrote)
	section("Left out", left)
	if len(inside) > 0 {
		section("Identifiers replaced inside values kept otherwise", append([]string{
			"Each value below kept every character but an identifier found in it, which its fake replaced. A word",
			"of a public name, such as a fund's, spelled as a nickname is replaced too, and in any case where the",
			"nickname has 6 characters or more: check each.",
			"",
		}, inside...))
	}
	section("Free text replaced by placeholders, the same text by the same placeholder", placeholders)
	section("Values mapped at keys in another case", otherCase)
	section("Rows of W-8 withholding, which show that an account is a nonresident's: decide on each", w8)
	if s.cfg.amounts.mode == "scale" {
		section("Scaling", []string{
			"Every amount is multiplied by " + strconv.FormatInt(s.cfg.amounts.k, 10) + ". Scaling hides nothing: whole quantities give K away",
			"by their greatest common divisor, and prices, which are kept, are public on their dates.",
		})
	}
	if len(o.shapes) > 0 {
		section("Shapes", []string{"shapes/ holds the shape of each answer left out for its money, with no values."})
	}
	return b.String()
}

// mappedCounts returns how many values of each kind were mapped, such as
// "3 of an account number, 3 of an account's hash", in a fixed order.
func mappedCounts(counts map[identifier]int) string {
	var parts []string
	for _, id := range []identifier{
		idAccountNumber, idHash, idDisplay, idNickname, idActivity, idOrder, idPosition, idCustomer, idCorrelation,
		idError, idTrace, idReference,
	} {
		if n := counts[id]; n > 0 {
			parts = append(parts, strconv.Itoa(n)+" of "+string(id))
		}
	}
	return strings.Join(parts, ", ")
}

// insideCounts returns, for review.txt, each place in vs, the identifiers
// replaced inside the values of an answer, with how many of each kind were
// replaced there, such as "transferItems.instrument.description: 2 of an
// account's nickname", in the order first met.
func insideCounts(vs []insideValue) []string {
	counts := map[insideValue]int{}
	var order []insideValue
	for _, v := range vs {
		if counts[v] == 0 {
			order = append(order, v)
		}
		counts[v]++
	}
	lines := make([]string, len(order))
	for i, v := range order {
		lines[i] = v.where + ": " + strconv.Itoa(counts[v]) + " of " + string(v.id)
	}
	return lines
}

// shapeReport returns the shape of a, an answer left out for its money,
// with no values: its route and kind, how many records it holds, each key
// path with the JSON kinds and number forms it holds and in how many
// places, and what checkAnswer finds in it, so that a hand-made fixture can
// be modeled on it.
func shapeReport(a *scrubAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s: %s (%s)\n\n", a.raw.name, a.route, a.kind)
	b.WriteString("Left out: it holds money, which -scrub.amounts=refuse does not write. Its shape, with no values:\n\n")
	if k, ok := kindByName(a.kind); ok {
		n := 1
		if records, err := answerRecords(a.raw.body, k.list); err == nil {
			n = len(records)
		}
		fmt.Fprintf(&b, "%s\n\n", plural(n, "record"))
	}
	type shape struct {
		path  string
		forms map[string]int
		order []string
	}
	var shapes []*shape
	byPath := map[string]*shape{}
	walkJSON(a.raw.body, func(sc jsonScalar) {
		p := pathName(sc.path)
		sh := byPath[p]
		if sh == nil {
			sh = &shape{path: p, forms: map[string]int{}}
			byPath[p] = sh
			shapes = append(shapes, sh)
		}
		f := literalForm(a.raw.body[sc.start:sc.end])
		if sh.forms[f] == 0 {
			sh.order = append(sh.order, f)
		}
		sh.forms[f]++
	}, nil)
	for _, sh := range shapes {
		var forms []string
		for _, f := range sh.order {
			forms = append(forms, f+" in "+plural(sh.forms[f], "place"))
		}
		b.WriteString(sh.path + ": " + strings.Join(forms, "; ") + "\n")
	}
	if fs := checkAnswer(a.kind, a.raw.body); len(fs) > 0 {
		b.WriteString("\nWhat checkAnswer finds:\n\n")
		for _, f := range fs {
			b.WriteString(f.String() + "\n")
		}
	}
	return b.String()
}

// literalForm returns the form of raw, a JSON scalar, with no value: a
// string, true or false, null, or a number with its form, such as "a
// number with 2 decimals".
func literalForm(raw []byte) string {
	switch {
	case raw[0] == '"':
		return "a string"
	case isJSONBool(raw):
		return "true or false"
	case string(raw) == "null":
		return "null"
	case hasExponent(raw):
		return "a number with an exponent"
	case decimalsOf(raw) > 0:
		return "a number with " + plural(decimalsOf(raw), "decimal")
	}
	return "a whole number"
}

// readRecording reads the recording in dir, a live run's raw folder: each
// pair of NNN.json and NNN.body, in the order of their numbers. Anything
// else in the folder is left alone. A file of a pair without the other, or
// an NNN.json that the scrubber cannot read whole, refuses the recording:
// a key it does not know could hold what it would not scrub.
func readRecording(dir string) ([]recordedAnswer, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	numbered := regexp.MustCompile(`^([0-9]{3,})\.(json|body)$`)
	pairs := map[string]int{}
	for _, e := range entries {
		if m := numbered.FindStringSubmatch(e.Name()); m != nil && e.Type().IsRegular() {
			pairs[m[1]]++
		}
	}
	names := slices.SortedFunc(maps.Keys(pairs), func(a, b string) int {
		return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
	})
	if len(names) == 0 {
		return nil, errors.New("it holds no answer of a recording, no NNN.json and NNN.body; -scrub takes a recording's raw folder")
	}
	var answers []recordedAnswer
	for _, n := range names {
		if pairs[n] != 2 {
			return nil, fmt.Errorf("answer %s has its .json or its .body but not both", n)
		}
		meta, err := os.ReadFile(filepath.Join(dir, n+".json"))
		if err != nil {
			return nil, err
		}
		body, err := os.ReadFile(filepath.Join(dir, n+".body"))
		if err != nil {
			return nil, err
		}
		a := recordedAnswer{name: n, body: body}
		dec := json.NewDecoder(bytes.NewReader(meta))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a.meta); err != nil {
			return nil, fmt.Errorf("answer %s: its .json cannot be read whole", n)
		}
		if a.meta.Request.Method == "" || !strings.HasPrefix(a.meta.Request.Path, "/") || a.meta.Response.Status < 100 || a.meta.Response.Status > 599 {
			return nil, fmt.Errorf("answer %s: its .json names no request and status", n)
		}
		answers = append(answers, a)
	}
	return answers, nil
}
