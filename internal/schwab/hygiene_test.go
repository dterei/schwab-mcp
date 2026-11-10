package schwab

import (
	"fmt"
	"html"
	"maps"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// An indexedFile is a file of the fake's testdata as the value hygiene
// tests read it: its entry in the index, and its bytes.
type indexedFile struct {
	entry schwabtest.Entry
	body  []byte
}

// indexedFiles returns each file that the fake's index names, a recording
// as well as the hand-made ones, in the index's order, with its bytes.
func indexedFiles(t testing.TB) []indexedFile {
	t.Helper()
	var files []indexedFile
	for _, e := range schwabtest.Index() {
		files = append(files, indexedFile{entry: e, body: readTestdata(t, e.File)})
	}
	return files
}

// eachKey calls f with each key of each object within n, with the path
// that it ends.
func eachKey(n *node, path []string, f func(path []string)) {
	for i, v := range n.vals {
		at := path
		if !n.array {
			at = append(slices.Clip(path), n.keys[i])
			f(at)
		}
		eachKey(v, at, f)
	}
}

// pathName returns path as a report names a key path: its keys joined with
// ".", each as keyName shows it, or "the answer" for none.
func pathName(path []string) string {
	if len(path) == 0 {
		return "the answer"
	}
	names := make([]string, len(path))
	for i, k := range path {
		names[i] = keyName(k)
	}
	return strings.Join(names, ".")
}

// The forms of the fakes, and of what the format scans look for.
var (
	// counterUUID is a UUID of the fake's counter, which gives correlation
	// IDs and errors' IDs: 00000000-0000-4000-8000-000000000001 and so on.
	counterUUID = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)
	// accountN is the nickname that the scrubber gives an account.
	accountN = regexp.MustCompile(`^Account [1-9][0-9]*$`)
	// wholeNumber is a whole number from 1, as an id is written.
	wholeNumber = regexp.MustCompile(`^[1-9][0-9]*$`)
	// trailingDigits are the digits that end a masked account number.
	trailingDigits = regexp.MustCompile(`[0-9]+$`)

	hexRun      = regexp.MustCompile(`[0-9A-Fa-f]+`)
	upperHexRun = regexp.MustCompile(`[0-9A-F]+`)
	uuidForm    = regexp.MustCompile(`[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}`)
	// occInText finds an OCC option symbol within text, with its strike in
	// the group.
	occInText = regexp.MustCompile(occRoot + `[0-9]{6}[CP]([0-9]{8})`)
	// maxAge finds the seconds of a header's max-age, such as
	// Strict-Transport-Security's max-age=31536000, a year.
	maxAge = regexp.MustCompile(`(?i)max-age=([0-9]+)`)
	// byIDPath finds, in the path of a transaction or an order asked for by
	// its id, which of the two it is, and the id.
	byIDPath = regexp.MustCompile(`^/trader/v1/accounts/[^/]+/(transactions|orders)/([^/]+)$`)
	// longDigits finds a run of six digits or more, as an id is written.
	longDigits = regexp.MustCompile(`[0-9]{6,}`)
	// akamaiReferences find an Akamai Reference: after "Reference #", as
	// errors.go reads it, in the address of Akamai's page of errors, and
	// alone, by its form, a number, eight hex digits, ten digits and more hex
	// digits, joined by dots.
	akamaiReferences = []*regexp.Regexp{
		regexp.MustCompile(`Reference #([0-9A-Za-z.]+)`),
		regexp.MustCompile(`edgesuite\.net/([0-9A-Za-z.]+)`),
		regexp.MustCompile(`(?:^|[^0-9A-Za-z.])([0-9]+\.[0-9a-f]{8}\.[0-9]{10}\.[0-9a-f]+)(?:$|[^0-9A-Za-z])`),
	}
)

// A valueChecker holds what the value hygiene tests check the fixtures'
// values against, and what they find wrong.
type valueChecker struct {
	pages    []*errorPage
	numbers  map[string]bool // the fake account numbers: FixtureAccounts' and the spares
	hashes   map[string]bool // the hashes of the fake account numbers
	nickname map[string]bool // FixtureAccounts' nicknames
	rows     map[string]bool // the activity IDs of the rows that the files hold, in decimal
	orders   map[string]bool // the order IDs of the rows that the files hold, in decimal
	problems []string
}

// checkValues returns what is wrong with the values of files, the fixtures
// with their index entries, as the value hygiene tests find it. They know
// only the fakes, never a real value:
//
//   - every file is an answer of a kind that the scrub policy covers, and
//     every scalar of a JSON file sits at a key path that its kind's table
//     covers, but for a key that no field takes, which the census fails
//     instead; an error page is one of errorPages;
//   - a mapped value is a fake: an account number, as a string or a number,
//     is FixtureAccounts' or a spare; a hash is HashFor of the account
//     number beside it; a masked account number ends with digits of the
//     account number beside it and holds no other; a nickname is
//     FixtureAccounts' or Account N; the streamer's IDs are
//     StreamerCustomerID and StreamerCorrelID; an error's ID has the form of
//     the fake's counter; an activity, order or position ID is a whole
//     number below 2^53, as a number or a string, which holds no fake
//     account number, and no row's activity ID is UnknownActivityID; and an
//     Akamai Reference is AkamaiReference;
//   - a scaled value is a number, a kept value matches its grammar, and free
//     text is on its part of Schwab's wording or is its placeholder, such as
//     DESCRIPTION 3;
//   - no string of 20 or more characters but one that its grammar or its
//     fake admits: an enum, a symbol, a time, a security's name, an address
//     of the streamer, Schwab's wording, a hash, the streamer's IDs, or an
//     error's ID;
//   - the format scans, over each JSON string and key decoded, each page's
//     text with its character references unescaped, and each index entry's
//     file, request path and query, unescaped too, and header values: every
//     run of exactly eight digits is a fake account number, but within a
//     run of 64 hex digits, a UUID, an OCC option's strike, a CUSIP, a
//     header's max-age, or the id in the path of a transaction or an order
//     asked for by its id;
//     every run of 64 or more upper-case hex digits is a fake's hash or
//     UnknownHash; every UUID has the counter's form or is StreamerCorrelID;
//     and every Akamai Reference is AkamaiReference;
//   - a request's path names a fake's hash, UnknownHash or a fake account
//     number where an account goes, a row's activity ID or
//     UnknownActivityID where a transaction's id goes, and a row's order ID
//     where an order's id goes; and after its account it holds no other run
//     of six digits or more but within a hash, since an id that no row holds
//     cannot be told from a real one.
//
// A null holds no value, so it may stand anywhere; the census and the
// records' problems report one where the records take none. Each problem
// names the file, where in it, and what is wrong, never the value.
func checkValues(t testing.TB, files []indexedFile) []string {
	t.Helper()
	c := &valueChecker{
		pages:   errorPages(t),
		numbers: map[string]bool{}, hashes: map[string]bool{}, nickname: map[string]bool{}, rows: map[string]bool{},
		orders: map[string]bool{},
	}
	for _, a := range schwabtest.FixtureAccounts {
		c.numbers[a.Number] = true
		c.nickname[a.Nickname] = true
	}
	for _, n := range schwabtest.SpareAccountNumbers {
		c.numbers[n] = true
	}
	for n := range c.numbers {
		c.hashes[schwabtest.HashFor(n)] = true
	}
	roots := make([]*node, len(files))
	for i, f := range files {
		if schwabtest.ExtensionFor(f.body) != ".json" {
			continue
		}
		roots[i] = parseNode(f.body)
		if kind, ok := policyKindOf(c.pages, f.entry, f.body); ok && policyTables[kind] == transactionTable {
			eachScalar(roots[i], nil, nil, func(path []string, raw []byte, _ *node) {
				switch {
				case keysMatch(path, []string{"activityId"}):
					c.rows[idDigits(raw)] = true
				case keysMatch(path, []string{"orderId"}):
					c.orders[idDigits(raw)] = true
				}
			})
		}
	}
	for i, f := range files {
		c.checkFile(f, roots[i])
	}
	return c.problems
}

// complain adds a problem of file, at where in it.
func (c *valueChecker) complain(file, where, what string) {
	c.problems = append(c.problems, file+": "+where+": "+what)
}

// checkFile checks one file and its index entry, given root, the file's
// JSON, or nil for a file that is not JSON.
func (c *valueChecker) checkFile(f indexedFile, root *node) {
	file := f.entry.File
	ext := schwabtest.ExtensionFor(f.body)
	kind, ok := policyKindOf(c.pages, f.entry, f.body)
	switch {
	case !ok:
		c.complain(file, "the answer", "an answer of no kind that the scrub policy covers")
	case kind != errorsKind && ext != ".json":
		c.complain(file, "the answer", "an answer of "+kind+" that is not JSON")
		ok = false
	}
	switch ext {
	case ".json":
		var tab *table
		if ok {
			tab = policyTables[kind]
		}
		c.checkJSON(file, kind, tab, root)
	case ".html":
		c.checkPage(file, f.body)
	default:
		c.scanText(file, "the answer", string(f.body), nil)
	}
	c.checkEntry(f.entry)
}

// checkJSON checks the keys and scalars of root, an answer of kind, against
// tab, its table, or, when it has none, by the format scans alone.
func (c *valueChecker) checkJSON(file, kind string, tab *table, root *node) {
	var record reflect.Type
	if k, ok := kindByName(kind); ok {
		record = k.typ
	}
	eachKey(root, nil, func(path []string) {
		c.scanText(file, pathName(path)+", its key", path[len(path)-1], nil)
	})
	eachScalar(root, nil, nil, func(path []string, raw []byte, in *node) {
		if string(raw) == "null" {
			return
		}
		where := pathName(path)
		r, ruled, problem := rule{}, false, ""
		if tab != nil {
			r, ruled = tab.rule(path)
			switch {
			case ruled:
				if problem = c.valueProblem(r, raw, in); problem != "" {
					c.complain(file, where, problem)
				}
			case record != nil && !isField(record, path):
				// A key that no field takes, which the census fails.
			default:
				c.complain(file, where, "no rule of the scrub policy for "+kind+" covers it")
			}
		}
		s, isString := jsonString(raw)
		if !isString {
			return
		}
		admitted := ruled && problem == "" && admitsLong(r, s)
		if n := utf8.RuneCountInString(s); tab != nil && n >= 20 && !admitted {
			c.complain(file, where, fmt.Sprintf("a string of %d characters that no grammar or fake admits", n))
		}
		var skip [][]int
		if ruled && r.gram == cusipGrammar {
			skip = [][]int{{0, len(s)}}
		}
		c.scanText(file, where, s, skip)
	})
}

// isField reports whether a field of the records of type t takes the value
// at path, its keys matched as encoding/json matches them.
func isField(t reflect.Type, path []string) bool {
	_, _, ok := fieldAt(t, path)
	return ok
}

// kindByName returns the kind of answer that checkAnswer reads called name,
// and whether there is one.
func kindByName(name string) (answerKind, bool) {
	i := slices.IndexFunc(answerKinds, func(k answerKind) bool { return k.name == name })
	if i < 0 {
		return answerKind{}, false
	}
	return answerKinds[i], true
}

// admitsLong reports whether r admits s, a string of 20 or more characters
// that has passed r's check: an enum's grammar does, as do a symbol's, a
// time's, a security's name's and the streamer's address's, and Schwab's
// wording, and the fakes of a hash, the streamer's IDs and an error's ID;
// a placeholder does not.
func admitsLong(r rule, s string) bool {
	switch r.class {
	case classKeep:
		return r.gram.values != nil || slices.Contains([]*grammar{symbolGrammar, timeGrammar, nameGrammar, urlGrammar}, r.gram)
	case classPlaceholder:
		return r.text.isWording(s)
	case classMap:
		return slices.Contains([]identifier{idHash, idCustomer, idCorrelation, idError}, r.id)
	}
	return false
}

// valueProblem returns what is wrong with raw, a value at r's path in the
// object in, or nil for an element of an array, as a fixture holds it, or
// "" for nothing.
func (c *valueChecker) valueProblem(r rule, raw []byte, in *node) string {
	switch r.class {
	case classMap:
		return c.fakeProblem(r.id, raw, in)
	case classScale:
		if !isJSONNumber(raw) {
			return "money that is not a number"
		}
	case classKeep:
		if !r.gram.match(raw) {
			return "not " + r.gram.name
		}
	case classPlaceholder:
		if s, ok := jsonString(raw); !ok || !r.text.isWording(s) && !r.text.isPlaceholder(s) {
			return r.text.name + " that is neither Schwab's wording nor " + r.text.word + " N"
		}
	}
	return ""
}

// fakeProblem returns what is wrong with raw, a value that id identifies,
// in the object in, as a fixture holds it, or "" if it is a fake.
func (c *valueChecker) fakeProblem(id identifier, raw []byte, in *node) string {
	s, isString := jsonString(raw)
	switch id {
	case idAccountNumber:
		if !isString {
			s = string(raw)
		}
		if !c.numbers[s] {
			return "an account number that is neither FixtureAccounts' nor a spare"
		}
	case idHash:
		number, ok := besideNumber(in)
		switch {
		case !ok:
			return "a hash with no account number beside it"
		case !isString || s != schwabtest.HashFor(number):
			return "a hash that is not HashFor of the account number beside it"
		}
	case idDisplay:
		number, ok := besideNumber(in)
		tail := trailingDigits.FindString(s)
		switch {
		case !ok:
			return "a masked account number with no account number beside it"
		case !isString || tail == "" || !strings.HasSuffix(number, tail):
			return "a masked account number that does not end with digits of the account number beside it"
		case strings.ContainsAny(s[:len(s)-len(tail)], "0123456789"):
			return "a masked account number that holds other digits"
		}
	case idNickname:
		if !isString || !c.nickname[s] && !accountN.MatchString(s) {
			return "a nickname that is neither FixtureAccounts' nor Account N"
		}
	case idActivity, idOrder, idPosition:
		return c.idProblem(id, raw)
	case idCustomer:
		if s != schwabtest.StreamerCustomerID {
			return "a customer ID that is not StreamerCustomerID"
		}
	case idCorrelation:
		if s != schwabtest.StreamerCorrelID {
			return "a correlation ID that is not StreamerCorrelID"
		}
	case idError:
		if !counterUUID.MatchString(s) {
			return "an ID that is not a UUID of the fake's counter"
		}
	case idReference:
		if s != schwabtest.AkamaiReference {
			return "an Akamai Reference that is not AkamaiReference"
		}
	}
	return ""
}

// idProblem returns what is wrong with raw as a fake id of the kind id,
// or "": it is a whole number from 1 and below 2^53, as a number or a
// string, which holds no fake account number; and no activity ID is
// UnknownActivityID, which no row may have.
func (c *valueChecker) idProblem(id identifier, raw []byte) string {
	digits := idDigits(raw)
	v, err := strconv.ParseUint(digits, 10, 64)
	switch {
	case !wholeNumber.MatchString(digits) || err != nil || v >= 1<<53:
		return string(id) + " that is not a whole number below 2^53"
	case id == idActivity && digits == strconv.FormatInt(schwabtest.UnknownActivityID, 10):
		return "an activity ID that is UnknownActivityID, which no row has"
	}
	for n := range c.numbers {
		if strings.Contains(digits, n) {
			return string(id) + " that holds a fake account number"
		}
	}
	return ""
}

// idDigits returns an id as raw holds it: the string that it is, or the
// number as written.
func idDigits(raw []byte) string {
	if s, ok := jsonString(raw); ok {
		return s
	}
	return string(raw)
}

// besideNumber returns the account number of in, the object that holds a
// value: its accountNumber, a string or a number as written, the last if it
// sends more than one, as encoding/json takes the last; and whether it has
// one.
func besideNumber(in *node) (string, bool) {
	if in == nil {
		return "", false
	}
	number, found := "", false
	for i, k := range in.keys {
		if raw := in.vals[i].raw; strings.EqualFold(k, "accountNumber") && raw != nil {
			if s, ok := jsonString(raw); ok {
				number, found = s, true
			} else if isJSONNumber(raw) {
				number, found = string(raw), true
			}
		}
	}
	return number, found
}

// checkPage checks body, an HTML page: each slot of the error page that it
// is, by the slot's rule, and its text, unescaped, by the format scans.
func (c *valueChecker) checkPage(file string, body []byte) {
	if p, texts, ok := matchErrorPage(c.pages, body); ok {
		for i, text := range texts {
			r := p.slots[i].rule
			if problem := c.valueProblem(r, jsonText(html.UnescapeString(text)), nil); problem != "" {
				c.complain(file, r.path, problem)
			}
		}
	}
	c.scanText(file, "the page", html.UnescapeString(string(body)), nil)
}

// checkEntry checks e, a file's index entry: its file's name, its request's
// path and query, as written and unescaped, and its headers' values by the
// format scans, and its path by the rules for the accounts, the
// transactions and the orders that a path names.
func (c *valueChecker) checkEntry(e schwabtest.Entry) {
	c.scanText(e.File, "its name", e.File, nil)
	paths := written(e.Request.Path, url.PathUnescape)
	for _, path := range paths {
		var skip [][]int
		if m := byIDPath.FindStringSubmatchIndex(path); m != nil {
			skip = [][]int{m[4:6]}
		}
		c.scanText(e.File, "the request's path", path, skip)
	}
	for _, query := range written(e.Request.Query, url.QueryUnescape) {
		c.scanText(e.File, "the request's query", query, nil)
	}
	for _, name := range slices.Sorted(maps.Keys(e.Response.Headers)) {
		for _, v := range e.Response.Headers[name] {
			var skip [][]int
			for _, m := range maxAge.FindAllStringSubmatchIndex(v, -1) {
				skip = append(skip, m[2:4])
			}
			c.scanText(e.File, "the header "+name, v, skip)
		}
	}
	rest, ok := strings.CutPrefix(paths[len(paths)-1], "/trader/v1/accounts/")
	if !ok {
		return
	}
	parts := strings.Split(rest, "/")
	if account := parts[0]; account != "accountNumbers" && !c.hashes[account] && account != schwabtest.UnknownHash && !c.numbers[account] {
		c.complain(e.File, "the request's path", "an account that is neither a fake's hash, UnknownHash nor a fake account number")
	}
	after := parts[1:]
	for i, part := range after {
		switch pathIDKind(after, i) {
		case idActivity:
			if !c.rows[part] && part != strconv.FormatInt(schwabtest.UnknownActivityID, 10) {
				c.complain(e.File, "the request's path", "an activity ID that is no row's and not UnknownActivityID")
			}
		case idOrder:
			if !c.orders[part] {
				c.complain(e.File, "the request's path", "an order ID that is no row's")
			}
		default:
			if len(idRuns(part)) > 0 {
				c.complain(e.File, "the request's path", "six digits or more where no id goes")
			}
		}
	}
}

// pathIDKind returns what after[i] identifies, where after is the parts of
// a path under /trader/v1/accounts/ after its account: an activity ID
// after transactions/, and an order ID after orders/, as a transaction or
// an order is asked for by its id; or "" where no id goes.
func pathIDKind(after []string, i int) identifier {
	if i == 1 {
		switch after[0] {
		case "transactions":
			return idActivity
		case "orders":
			return idOrder
		}
	}
	return ""
}

// idRuns returns the runs of six digits or more in part, a part of a path,
// but those within a run of 64 hex digits, as a hash's digits are: an id
// that the path holds, wherever it is.
func idRuns(part string) []string {
	var hashes [][]int
	for _, m := range hexRun.FindAllStringIndex(part, -1) {
		if m[1]-m[0] == 64 {
			hashes = append(hashes, m)
		}
	}
	var runs []string
	for _, m := range longDigits.FindAllStringIndex(part, -1) {
		if !slices.ContainsFunc(hashes, func(h []int) bool { return h[0] <= m[0] && m[1] <= h[1] }) {
			runs = append(runs, part[m[0]:m[1]])
		}
	}
	return runs
}

// written returns s, and s unescaped by unescape again and again until it
// stops changing or fails, each form once: so that what a path or a query
// holds is found however many times it was escaped.
func written(s string, unescape func(string) (string, error)) []string {
	forms := []string{s}
	for {
		next, err := unescape(forms[len(forms)-1])
		if err != nil || next == forms[len(forms)-1] {
			return forms
		}
		forms = append(forms, next)
	}
}

// scanText checks text, decoded, by the format scans, and complains of
// what they find, at where in file: a run of exactly eight digits that is
// no fake account number, outside a run of 64 hex digits, a UUID, an OCC
// option's strike and the spans of skip; a run of 64 or more upper-case hex
// digits that is no fake's hash and not UnknownHash; a UUID that is not of
// the fake's counter and not StreamerCorrelID; and an Akamai Reference that
// is not AkamaiReference. A scan that text is too short to match, or
// lacks the characters for, is skipped, since the race detector makes each
// one slow.
func (c *valueChecker) scanText(file, where, text string, skip [][]int) {
	digits := 0
	for i := range len(text) {
		if '0' <= text[i] && text[i] <= '9' {
			digits++
		}
	}
	if digits >= 8 {
		c.scanDigits(file, where, text, skip)
	}
	if len(text) >= 64 {
		for _, run := range upperHexRun.FindAllString(text, -1) {
			if len(run) >= 64 && !c.hashes[run] && run != schwabtest.UnknownHash {
				c.complain(file, where, "a hash that is no fake account's")
			}
		}
	}
	if len(text) >= 36 && strings.Count(text, "-") >= 4 {
		for _, id := range uuidForm.FindAllString(text, -1) {
			if !counterUUID.MatchString(id) && id != schwabtest.StreamerCorrelID {
				c.complain(file, where, "a UUID that is neither of the fake's counter nor StreamerCorrelID")
			}
		}
	}
	if strings.Contains(text, "Reference #") || strings.Contains(text, "edgesuite.net/") || strings.Count(text, ".") >= 3 {
		for _, re := range akamaiReferences {
			for _, m := range re.FindAllStringSubmatch(text, -1) {
				if strings.TrimRight(m[1], ".") != schwabtest.AkamaiReference {
					c.complain(file, where, "an Akamai Reference that is not AkamaiReference")
				}
			}
		}
	}
}

// scanDigits complains of each run of exactly eight digits in text that is
// no fake account number, outside a run of 64 hex digits, a UUID, an OCC
// option's strike and the spans of skip.
func (c *valueChecker) scanDigits(file, where, text string, skip [][]int) {
	var runs [][]int
	for i := 0; i < len(text); {
		j := i
		for j < len(text) && '0' <= text[j] && text[j] <= '9' {
			j++
		}
		if j-i == 8 && !c.numbers[text[i:j]] {
			runs = append(runs, []int{i, j})
		}
		i = max(j, i+1)
	}
	if len(runs) == 0 {
		return
	}
	exempt := slices.Clone(skip)
	for _, m := range hexRun.FindAllStringIndex(text, -1) {
		if m[1]-m[0] == 64 {
			exempt = append(exempt, m)
		}
	}
	exempt = append(exempt, uuidForm.FindAllStringIndex(text, -1)...)
	for _, m := range occInText.FindAllStringSubmatchIndex(text, -1) {
		exempt = append(exempt, m[2:4])
	}
	for _, m := range runs {
		if !slices.ContainsFunc(exempt, func(e []int) bool { return e[0] <= m[0] && m[1] <= e[1] }) {
			c.complain(file, where, "eight digits that are no fake account number")
		}
	}
}

// Every fixture that the fake's index names, a recording as well as the
// hand-made ones, holds only values that the scrub policy and the fakes
// admit, as checkValues says.
func TestValueHygiene(t *testing.T) {
	files := indexedFiles(t)
	if len(files) == 0 {
		t.Fatal("no file is indexed")
	}
	for _, p := range checkValues(t, files) {
		t.Error(p)
	}
}

// An edit changes a fixture, for a case of TestValueHygieneCatches.
type edit func(t *testing.T, f *indexedFile)

// nodeAt returns the node at keys within n, through the first element of
// each array on the way.
func nodeAt(t *testing.T, n *node, keys ...string) *node {
	t.Helper()
	for _, k := range keys {
		for n.array {
			n = n.vals[0]
		}
		i := slices.Index(n.keys, k)
		if i < 0 {
			t.Fatalf("no key %s in %s", k, n.text())
		}
		n = n.vals[i]
	}
	return n
}

// editJSON returns an edit that changes a fixture's JSON.
func editJSON(change func(t *testing.T, root *node)) edit {
	return func(t *testing.T, f *indexedFile) {
		t.Helper()
		root := parseNode(f.body)
		change(t, root)
		f.body = []byte(root.text())
	}
}

// setAt returns an edit that makes the value at keys raw.
func setAt(raw string, keys ...string) edit {
	return editJSON(func(t *testing.T, root *node) {
		t.Helper()
		*nodeAt(t, root, keys...) = *parseNode([]byte(raw))
	})
}

// addAt returns an edit that adds a member, key and raw, to the object at
// keys.
func addAt(key, raw string, keys ...string) edit {
	return editJSON(func(t *testing.T, root *node) {
		t.Helper()
		n := nodeAt(t, root, keys...)
		for n.array {
			n = n.vals[0]
		}
		n.keys, n.vals = append(n.keys, key), append(n.vals, parseNode([]byte(raw)))
	})
}

// renameAt returns an edit that renames the last key of keys to key.
func renameAt(key string, keys ...string) edit {
	return editJSON(func(t *testing.T, root *node) {
		t.Helper()
		n := nodeAt(t, root, keys[:len(keys)-1]...)
		for n.array {
			n = n.vals[0]
		}
		n.keys[slices.Index(n.keys, keys[len(keys)-1])] = key
	})
}

// replaceText returns an edit that puts with in place of old, which the
// fixture must hold, everywhere.
func replaceText(old, with string) edit {
	return func(t *testing.T, f *indexedFile) {
		t.Helper()
		if !strings.Contains(string(f.body), old) {
			t.Fatalf("%s does not hold %q", f.entry.File, old)
		}
		f.body = []byte(strings.ReplaceAll(string(f.body), old, with))
	}
}

// editEntry returns an edit that changes a fixture's index entry.
func editEntry(change func(e *schwabtest.Entry)) edit {
	return func(_ *testing.T, f *indexedFile) {
		if f.entry.Response.Headers == nil {
			f.entry.Response.Headers = map[string][]string{}
		}
		change(&f.entry)
	}
}

// edits returns an edit that makes each of es in turn.
func edits(es ...edit) edit {
	return func(t *testing.T, f *indexedFile) {
		t.Helper()
		for _, e := range es {
			e(t, f)
		}
	}
}

// newAnswer returns an edit that makes a fixture an answer to method and
// path, with status and body.
func newAnswer(method, path string, status int, body string) edit {
	return func(_ *testing.T, f *indexedFile) {
		f.entry.Request.Method, f.entry.Request.Path, f.entry.Response.Status = method, path, status
		f.body = []byte(body)
	}
}

// Each rule of checkValues finds a value planted against it, one at a time
// in today's fixtures, and names the file it is in, while a fake planted
// where a rule admits one is found by none.
func TestValueHygieneCatches(t *testing.T) {
	quote := func(s string) string { return string(jsonText(s)) }
	asked := "http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;v1&#47;oauth&#47;token"
	reference := strings.ReplaceAll(schwabtest.AkamaiReference, ".", "&#46;")
	account := "/trader/v1/accounts/" + schwabtest.FixtureAccounts[0].Hash
	byID, byOrder := account+"/transactions/", account+"/orders/"
	header := func(name, value string) edit {
		return editEntry(func(e *schwabtest.Entry) { e.Response.Headers[name] = []string{value} })
	}
	for _, tc := range []struct {
		name, file string
		edit       edit
		want       string // what a problem of the file says, or "" for no problem at all
	}{
		// What the policy covers.
		{"a scalar where an array goes", "transactions.json", setAt(`5`, "transferItems"), "no rule of the scrub policy for transactions covers it"},
		{
			"an error's path that no rule covers", "errors/not_authorized_401.json",
			addAt("source", `{"parameter":"symbol"}`, "errors"), "errors.source.parameter: no rule of the scrub policy for errors covers it",
		},
		{
			"a 200 of market data", "recorded/2026-11-08/001.json",
			newAnswer("GET", "/marketdata/v1/quotes", 200, `{"SPY":{"quote":{"lastPrice":1.5}}}`), "an answer of no kind",
		},
		{"an error with a key of no error's", "errors/invalid_account_400.json", addAt("accountNumber", `"29470135"`), "an answer of no kind"},
		{
			"an account's answer that is a page", "accounts.json", replaceText(string(readTestdata(t, "accounts.json")), "<html><body>Hi</body></html>"),
			"an answer of accounts that is not JSON",
		},
		{"a page of no template", "errors/akamai_403.html", replaceText("<H1>Access Denied</H1>", "<H1>Denied</H1>"), "an answer of no kind"},
		{"a 429 that is not white space", "errors/rate_limit_429.txt", replaceText(" \n", "slow down\n"), "an answer of no kind"},

		// Mapped values are fakes.
		{"an account number", "account_numbers.json", setAt(`"12345678"`, "accountNumber"), "accountNumber: an account number that is neither"},
		{"an account number as a number", "transactions.json", setAt(`12345678`, "accountNumber"), "an account number that is neither"},
		{"a fake account number as a number", "transactions.json", setAt(`61803398`, "accountNumber"), ""},
		{
			"an account number at a key in another case", "transactions.json", addAt("AccountNumber", `"12345678"`),
			"AccountNumber: an account number that is neither",
		},
		{
			"a hash of another account", "account_numbers.json", setAt(quote(schwabtest.FixtureAccounts[1].Hash), "hashValue"),
			"a hash that is not HashFor of the account number beside it",
		},
		{"a hash alone", "account_numbers.json", renameAt("number", "accountNumber"), "a hash with no account number beside it"},
		{
			"a hash beside an account number sent twice", "account_numbers.json", addAt("accountNumber", quote(schwabtest.FixtureAccounts[1].Number)),
			"a hash that is not HashFor of the account number beside it",
		},
		{
			"a masked number of another", "user_preference.json", setAt(`"...999"`, "accounts", "displayAcctId"),
			"a masked account number that does not end with digits of the account number beside it",
		},
		{
			"a masked number with more digits", "user_preference.json", setAt(`"1...135"`, "accounts", "displayAcctId"),
			"a masked account number that holds other digits",
		},
		{"a masked number of four digits", "user_preference.json", setAt(`"*0135"`, "accounts", "displayAcctId"), ""},
		{"a nickname", "user_preference.json", setAt(`"Joint"`, "accounts", "nickName"), "a nickname that is neither"},
		{"a scrubbed nickname", "user_preference.json", setAt(`"Account 4"`, "accounts", "nickName"), ""},
		{
			"a customer ID", "user_preference.json", setAt(quote(strings.Repeat("ab", 32)), "streamerInfo", "schwabClientCustomerId"),
			"a customer ID that is not StreamerCustomerID",
		},
		{
			"a correlation ID", "user_preference.json", setAt(`"c3a9e2f4-7b1d-4e8a-9f6c-2d5b8a1e0f7d"`, "streamerInfo", "schwabClientCorrelId"),
			"a correlation ID that is not StreamerCorrelID",
		},
		{
			"an error's ID", "errors/not_authorized_401.json", setAt(`"8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e"`, "errors", "id"),
			"errors.id: an ID that is not a UUID of the fake's counter",
		},
		{"an activity ID with a fraction", "transactions.json", setAt(`1.5`, "activityId"), "an activity ID that is not a whole number below 2^53"},
		{"an order ID past 2^53", "transactions.json", setAt(`9007199254740993`, "orderId"), "an order ID that is not a whole number below 2^53"},
		{"a position ID as a string", "transactions.json", setAt(`"3926184750"`, "positionId"), ""},
		{
			"a position ID that holds an account number", "transactions.json", setAt(`3929470135`, "positionId"),
			"a position ID that holds a fake account number",
		},
		{"a row of UnknownActivityID", "transactions.json", setAt(`99999999999`, "activityId"), "UnknownActivityID, which no row has"},
		{
			"another Reference", "errors/akamai_403.html", replaceText("Reference&#32;&#35;"+reference, "Reference&#32;&#35;18&#46;0a1b2c3d&#46;1792539138&#46;32dec4a"),
			"the Reference: an Akamai Reference that is not AkamaiReference",
		},

		// A null holds no value.
		{"a null where an object goes", "accounts.json", setAt(`null`, "aggregatedBalance"), ""},
		{"a null for money", "transactions.json", setAt(`null`, "netAmount"), ""},

		// Scaled values are numbers.
		{"money as a string", "transactions.json", setAt(`"311.31"`, "netAmount"), "netAmount: money that is not a number"},

		// Kept values have their grammar's form.
		{"a type off the list", "transactions.json", setAt(`"IRA"`, "type"), "type: not a value of its closed list"},
		{"a date alone", "transactions.json", setAt(`"2026-08-04"`, "time"), ""},
		{"a time", "transactions.json", setAt(`"yesterday"`, "time"), "time: not a time"},
		{"a symbol", "transactions.json", setAt(`"ACME BANK"`, "transferItems", "instrument", "symbol"), "not a symbol"},
		{"a CUSIP", "accounts.json", setAt(`"abc"`, "securitiesAccount", "positions", "instrument", "cusip"), "not a CUSIP"},
		{"a name", "transactions.json", setAt(`"USD; wire 1"`, "transferItems", "instrument", "description"), "not a security's name"},
		{"a price as a string", "transactions.json", setAt(`"0.0"`, "transferItems", "instrument", "closingPrice"), "not a number"},
		{"a flag as a string", "transactions.json", addAt("qualifiedDividend", `"false"`), "qualifiedDividend: not a boolean"},
		{
			"a streamer elsewhere", "user_preference.json", setAt(`"wss://example.com/ws"`, "streamerInfo", "streamerSocketUrl"),
			"not an address of Schwab's streamer",
		},
		{"a code in lower case", "user_preference.json", setAt(`"n9"`, "streamerInfo", "schwabClientChannel"), "not a code"},
		{"a color off the list", "user_preference.json", setAt(`"Red"`, "accounts", "accountColor"), "not a value of its closed list"},
		{"a status of four digits", "errors/not_authorized_401.json", setAt(`"4010"`, "errors", "status"), "errors.status: not an HTTP status"},
		{"a status as a string", "errors/not_authorized_401.json", setAt(`"401"`, "errors", "status"), ""},
		{"an OAuth error off the list", "errors/refusal_2026_400.json", setAt(`"access_denied"`, "error"), "error: not a value of its closed list"},
		{
			"a fault code off the list", "errors/too_big_body_502.json", setAt(`"protocol.http.Other"`, "fault", "detail", "errorcode"),
			"not a value of its closed list",
		},
		{
			"a Whitelabel time", "errors/whitelabel_500.html", replaceText("Mon Nov 02 10:01:02 EST 2026", "2026-11-02T10:01:02Z"),
			"the time created: not a time as Java writes one",
		},
		{
			"an address elsewhere", "errors/akamai_403.html", replaceText(asked, "http&#58;&#47;&#47;example&#46;com&#47;"),
			"the address quoted: not an address of Schwab's API",
		},

		// Free text is Schwab's wording or a placeholder.
		{
			"a description", "transactions.json", addAt("description", `"Wire from ACME BANK"`),
			"description: a description that is neither Schwab's wording nor DESCRIPTION N",
		},
		{"a description's placeholder", "transactions.json", addAt("description", `"DESCRIPTION 3"`), ""},
		{"a placeholder without a number", "transactions.json", addAt("description", `"DESCRIPTION one"`), "a description that is neither"},
		{"a message's placeholder for a description", "transactions.json", addAt("description", `"MESSAGE 3"`), "a description that is neither"},
		{"a message for a description", "transactions.json", addAt("description", `"Invalid account number"`), "a description that is neither"},
		{
			"a message", "errors/invalid_account_400.json", setAt(`"Account 29470135 is closed"`, "message"),
			"message: a message that is neither Schwab's wording nor MESSAGE N",
		},
		{"a message's placeholder", "errors/invalid_account_400.json", setAt(`"MESSAGE 1"`, "message"), ""},

		// Long strings are what a grammar or a fake admits.
		{
			"a long string of a key no field takes", "accounts.json", addAt("zzNote", `"a note of twenty or more characters"`, "securitiesAccount"),
			"securitiesAccount.zzNote: a string of 35 characters that no grammar or fake admits",
		},
		{
			"a long placeholder", "transactions.json", addAt("description", `"DESCRIPTION 123456789"`),
			"description: a string of 21 characters that no grammar or fake admits",
		},

		// The format scans.
		{
			"eight digits in a string", "accounts.json", addAt("zzNote", `"ref 12345678"`, "securitiesAccount"),
			"securitiesAccount.zzNote: eight digits that are no fake account number",
		},
		{"eight digits in a key", "accounts.json", addAt("12345678", `1`, "securitiesAccount"), "its key: eight digits that are no fake account number"},
		{
			"eight digits in a page", "errors/akamai_403.html",
			replaceText(asked, "http&#58;&#47;&#47;api&#46;schwabapi&#46;com&#47;trader&#47;v1&#47;accounts&#47;12345678"),
			"the page: eight digits that are no fake account number",
		},
		{
			"eight digits in a query", "accounts.json", editEntry(func(e *schwabtest.Entry) { e.Request.Query += "&n=12345678" }),
			"the request's query: eight digits that are no fake account number",
		},
		{
			"eight digits escaped in a query", "accounts.json", editEntry(func(e *schwabtest.Entry) { e.Request.Query += "&n=1234%2535678" }),
			"the request's query: eight digits that are no fake account number",
		},
		{
			"eight digits in a header", "accounts.json", header("Retry-After", "12345678"),
			"the header Retry-After: eight digits that are no fake account number",
		},
		{"a year's max-age", "accounts.json", header("Strict-Transport-Security", "max-age=31536000"), ""},
		{"eight digits of a spare", "accounts.json", header("Retry-After", schwabtest.SpareAccountNumbers[19]), ""},
		{"a hash", "accounts.json", header("Vary", schwabtest.HashFor("12345678")), "the header Vary: a hash that is no fake account's"},
		{
			"a UUID", "accounts.json", header("X-Request-Id", "8f14e45f-ceea-467a-9b5c-1d2a3b4c5d6e"),
			"the header X-Request-Id: a UUID that is neither of the fake's counter nor StreamerCorrelID",
		},
		{
			"eight digits in a file's name", "recorded/2026-11-08/12345678.json",
			newAnswer("GET", "/trader/v1/accounts", 400, `{"message":"Invalid account number"}`),
			"its name: eight digits that are no fake account number",
		},
		{"a path to a transaction of eight digits", "transaction.json", edits(setAt(`12345678`, "activityId"),
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byID + "12345678" })), ""},
		{
			"a Reference in a message", "errors/invalid_account_400.json", setAt(`"Reference #abc"`, "message"),
			"message: an Akamai Reference that is not AkamaiReference",
		},
		{
			"an edge's address in a message", "errors/invalid_account_400.json", setAt(`"see errors.edgesuite.net/abc"`, "message"),
			"message: an Akamai Reference that is not AkamaiReference",
		},
		{
			"a Reference by its form", "errors/invalid_account_400.json", setAt(`"ref 18.0a1b2c3d.1792539138.32dec4a"`, "message"),
			"message: an Akamai Reference that is not AkamaiReference",
		},

		// The accounts and transactions that a path names.
		{
			"a path to a transaction of no row", "transaction.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byID + "73508261999" }),
			"the request's path: an activity ID that is no row's and not UnknownActivityID",
		},
		{
			"a path to UnknownActivityID", "transaction.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byID + "99999999999" }), "",
		},
		{
			"a path to a transaction by a row's order ID", "transaction.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byID + "1004517638295" }),
			"the request's path: an activity ID that is no row's",
		},
		{
			"a path to an order of a row", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byOrder + "1004517638295" }), "",
		},
		{
			"a path to an order of no row", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byOrder + "1004517638290" }),
			"the request's path: an order ID that is no row's",
		},
		{
			"a path to an order by a row's activity ID", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = byOrder + "73508261946" }), "an order ID that is no row's",
		},
		{
			"an id where no id goes", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = account + "/positions/123456" }),
			"the request's path: six digits or more where no id goes",
		},
		{
			"an id within a part where no id goes", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = account + "/orders/1004517638295/fill1234567" }),
			"the request's path: six digits or more where no id goes",
		},
		{
			"a hash after the account, its digits not an id", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = account + "/x/" + schwabtest.FixtureAccounts[0].Hash }), "",
		},
		{
			"a path to an account of no fake", "transactions.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = "/trader/v1/accounts/ABC/transactions" }),
			"the request's path: an account that is neither a fake's hash, UnknownHash nor a fake account number",
		},
		{
			"a path to UnknownHash", "transactions.json",
			editEntry(func(e *schwabtest.Entry) {
				e.Request.Path = "/trader/v1/accounts/" + schwabtest.UnknownHash + "/transactions"
			}), "",
		},
		{
			"a path to an account number of no fake", "errors/invalid_account_400.json",
			editEntry(func(e *schwabtest.Entry) { e.Request.Path = "/trader/v1/accounts/12345678" }), "an account that is neither",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			files := indexedFiles(t)
			i := slices.IndexFunc(files, func(f indexedFile) bool { return f.entry.File == tc.file })
			if i < 0 {
				var e schwabtest.Entry
				e.File = tc.file
				files, i = append(files, indexedFile{entry: e}), len(files)
			}
			tc.edit(t, &files[i])
			got := checkValues(t, files)
			switch {
			case tc.want == "" && len(got) > 0:
				t.Errorf("found %q, want nothing", got)
			case tc.want != "" && !slices.ContainsFunc(got, func(p string) bool {
				return strings.HasPrefix(p, tc.file+": ") && strings.Contains(p, tc.want)
			}):
				t.Errorf("found %q, want a problem of %s that says %q", got, tc.file, tc.want)
			}
		})
	}
}

// A key that no field takes is the census's to fail: checkValues finds
// nothing in a short value at one, while checkAnswer finds the key.
func TestValueHygieneLeavesUnknownKeysToTheCensus(t *testing.T) {
	files := indexedFiles(t)
	i := slices.IndexFunc(files, func(f indexedFile) bool { return f.entry.File == "accounts.json" })
	addAt("zzNote", `"short"`, "securitiesAccount")(t, &files[i])
	if got := checkValues(t, files); len(got) > 0 {
		t.Errorf("checkValues found %q, want nothing", got)
	}
	if found := checked("accounts", string(files[i].body)); !slices.Contains(found, "unknown key: securitiesAccount.zzNote: no field takes it") {
		t.Errorf("checkAnswer found %q, want the unknown key", found)
	}
}
