package schwab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// A finding is something that checkAnswer found in an answer that the
// records do not expect. It holds key paths, JSON kinds and counts, never a
// value of Schwab's, so that a report can show it as it is.
type finding struct {
	check string // the check that found it: problemCheck, keyCheck, nullCheck or repeatCheck
	path  string // where it is: a key path, as a Problem's, or "" for a record or the answer itself
	what  string // what it is
	fails bool   // whether it fails the answer, rather than being only reported
	n     int    // in how many places the answer holds it
}

// The checks that a finding can come from, as a report names them.
const (
	problemCheck = "problem"
	keyCheck     = "unknown key"
	nullCheck    = "null"
	repeatCheck  = "repeated key"
)

// String returns the finding as a line of a report, such as "repeated key,
// reported only: transferItems.instrument.assetType: sent 2 times, the same
// each time".
func (f finding) String() string {
	s := f.check
	if !f.fails {
		s += ", reported only"
	}
	s += ": "
	if f.path != "" {
		s += f.path + ": "
	}
	s += f.what
	if f.n > 1 {
		s += " (in " + strconv.Itoa(f.n) + " places)"
	}
	return s
}

// findings gathers findings, each once, in the order first found, with how
// many places each was found in.
type findings struct{ list []finding }

// add adds f, or counts one more place for it if it is there already.
func (fs *findings) add(f finding) {
	f.n = 0
	for i, g := range fs.list {
		if g.n = 0; g == f {
			fs.list[i].n++
			return
		}
	}
	f.n = 1
	fs.list = append(fs.list, f)
}

// An answerKind is a kind of answer that checkAnswer reads: that of one of
// the account routes.
type answerKind struct {
	name  string           // as checkAnswer takes it, such as "accounts"
	route schwabtest.Route // the route that answers with it
	typ   reflect.Type     // its records' type
	list  bool             // a list of records, rather than one
	// decode decodes an answer of the kind as the Client does, and returns
	// its records.
	decode func(r *reply) ([]reflect.Value, error)
}

// answerKindOf returns the kind of answer called name, which route answers
// with: records of type T, a list of them or one, decoded with the Client's
// list or one.
func answerKindOf[T any](name string, route schwabtest.Route, isList bool) answerKind {
	decode := func(r *reply) ([]reflect.Value, error) {
		var records []T
		if isList {
			rs, err := list[T](r, name)
			if err != nil {
				return nil, err
			}
			records = rs
		} else {
			rec, err := one[T](r, name)
			if err != nil {
				return nil, err
			}
			records = []T{rec}
		}
		vs := make([]reflect.Value, len(records))
		for i := range records {
			vs[i] = reflect.ValueOf(&records[i]).Elem()
		}
		return vs, nil
	}
	return answerKind{name: name, route: route, typ: reflect.TypeFor[T](), list: isList, decode: decode}
}

// answerKinds are the kinds of answer that checkAnswer reads, each named
// after its route.
var answerKinds = []answerKind{
	answerKindOf[AccountNumber]("accountNumbers", schwabtest.AccountNumbersRoute, true),
	answerKindOf[Account]("accounts", schwabtest.AccountsRoute, true),
	answerKindOf[Account]("account", schwabtest.AccountRoute, false),
	answerKindOf[Transaction]("transactions", schwabtest.TransactionsRoute, true),
	answerKindOf[Transaction]("transaction", schwabtest.TransactionRoute, false),
	answerKindOf[UserPreference]("userPreference", schwabtest.UserPreferenceRoute, false),
}

// kindOfRoute returns the kind of answer that route answers with, if it is
// one that checkAnswer reads.
func kindOfRoute(route schwabtest.Route) (answerKind, bool) {
	i := slices.IndexFunc(answerKinds, func(k answerKind) bool { return k.route == route })
	if i < 0 {
		return answerKind{}, false
	}
	return answerKinds[i], true
}

// checkAnswer checks body, an answer of the kind called kind, such as
// "accounts", against the records, and returns what it finds:
//   - each record's problems, as the Client's decoder gives them, with those
//     of an account's positions; or, for an answer that is not of its kind's
//     shape, that alone;
//   - each key that no field takes, Schwab's own problems key among them,
//     and each that a field takes in another case, named as such;
//   - each null where a field that is not on every record takes an object
//     or an array, which reads as absent, and is only reported;
//   - each key that an object sends more than once. Copies that differ, or
//     an object or array among them, fail; a scalar sent again with the same
//     bytes, as Schwab has sent an instrument's assetType, is only reported.
//
// A finding that the answer holds in several places is returned once, with
// their count. checkAnswer panics for a kind that it does not read, which is
// a mistake in its caller.
func checkAnswer(kind string, body []byte) []finding {
	i := slices.IndexFunc(answerKinds, func(k answerKind) bool { return k.name == kind })
	if i < 0 {
		panic("checkAnswer: no kind of answer is called " + strconv.Quote(kind))
	}
	k := answerKinds[i]
	var fs findings
	records, err := k.decode(&reply{header: http.Header{}, body: body})
	if err != nil {
		// The decoders' own words name only kinds and counts, while
		// encoding/json's, for what is not JSON, can quote it.
		what := "the answer is not JSON"
		var de *decodeError
		if json.Valid(body) && errors.As(err, &de) {
			what = valueFree(de.err.Error())
		}
		fs.add(finding{check: problemCheck, what: what, fails: true})
		return fs.list
	}
	for _, rec := range records {
		recordsIn(rec, "", func(path string, ps []Problem) {
			for _, p := range ps {
				fs.add(finding{check: problemCheck, path: joinPath(path, p.Path), what: valueFree(p.What), fails: true})
			}
		})
	}
	// The body is JSON, since the decoder took it. Numbers are kept as
	// written: Go 1.26 reads one too large for a float64 as null, which the
	// census would take for a null that Schwab sent.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	_ = dec.Decode(&v)
	c := newCensus()
	for _, rec := range recordsOf(v) {
		unknown, nulls := c.add(k.typ, rec)
		for _, u := range unknown {
			what := "no field takes it"
			if u.field != "" {
				what = u.field + " in another case"
			}
			fs.add(finding{check: keyCheck, path: u.path, what: what, fails: true})
		}
		for _, path := range nulls {
			fs.add(finding{check: nullCheck, path: path, what: "Schwab sent null, which reads as absent"})
		}
	}
	for _, r := range repeats(body, "") {
		fs.add(r.finding())
	}
	return fs.list
}

// recordsOf returns the records of v, an answer decoded as any that its
// kind's decoder took: each element of a list, a list of one taken for one
// record among them, or else v.
func recordsOf(v any) []any {
	if elems, ok := v.([]any); ok {
		return elems
	}
	return []any{v}
}

// quotedValue matches what a problem's text quotes of Schwab's values: a
// string, in Go's double quotes, and a number, after the word "number", as
// in "the number 1e999".
var quotedValue = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|\bnumber -?[0-9][0-9.eE+-]*`)

// valueFree returns what, a problem's text, with each value that it quotes
// replaced by an ellipsis: a string by "…", and a number by "number …". A
// problem quotes what Schwab sent where a time was expected, say, which can
// be anything, and a finding holds none of Schwab's values.
func valueFree(what string) string {
	return quotedValue.ReplaceAllStringFunc(what, func(s string) string {
		if strings.HasPrefix(s, `"`) {
			return `"…"`
		}
		return "number …"
	})
}

// The kinds of answer that checkAnswer reads are those whose fixtures the
// tests of the records read, with the same records, so that a route added
// to one list is not missing from the other.
func TestAnswerKinds(t *testing.T) {
	if len(answerKinds) != len(testdataKinds) {
		t.Fatalf("%d kinds of answer, and %d kinds of fixture", len(answerKinds), len(testdataKinds))
	}
	for i, want := range testdataKinds {
		if k := answerKinds[i]; k.route != want.route || k.typ != want.typ || k.list != want.list {
			t.Errorf("kind %q: %s, %s, a list %v; the fixtures': %s, %s, a list %v", k.name, k.route, k.typ, k.list, want.route, want.typ, want.list)
		}
	}
	for _, k := range answerKinds {
		if got, ok := kindOfRoute(k.route); !ok || got.name != k.name {
			t.Errorf("the kind of %s is %q, %v; want %q", k.route, got.name, ok, k.name)
		}
	}
	if k, ok := kindOfRoute(schwabtest.TestRoute); ok {
		t.Errorf("the fake's own route has the kind %q", k.name)
	}
}

// checked returns what checkAnswer finds in body, an answer of kind, as
// text.
func checked(kind, body string) []string {
	var out []string
	for _, f := range checkAnswer(kind, []byte(body)) {
		out = append(out, f.String())
	}
	return out
}

// checkAnswer finds each record's problems, an account's positions' among
// them, or else that the answer is not of its kind's shape; each key that no
// field takes, or that a field takes in another case; each null that reads
// as absent, which it only reports; and each key that an object sends more
// than once, failing copies that differ or an object or array among them,
// and reporting a scalar sent again the same. A finding in several places is
// given once, with their count.
func TestCheckAnswer(t *testing.T) {
	account := `{"securitiesAccount":` + baseSecuritiesAccount + `}`
	pref := `{"accounts":[],"streamerInfo":[],"offers":[]}`
	noTime := withKeys(baseTransaction, "time", "")
	for _, tc := range []struct {
		name, kind, body string
		want             []string
	}{
		{"account numbers", "accountNumbers", `[{"accountNumber":"29470135","hashValue":"H"}]`, nil},
		{"accounts", "accounts", "[" + account + "," + account + "]", nil},
		{"an account", "account", account, nil},
		{"an account in a list of one", "account", "[" + account + "]", nil},
		{"transactions", "transactions", "[" + baseTransaction + "]", nil},
		{"no transactions", "transactions", " [] ", nil},
		{"a transaction", "transaction", baseTransaction, nil},
		{"preferences", "userPreference", pref, nil},

		{"an object for a list", "accounts", `{"x":1,"x":1}`, []string{"problem: the answer is an object, not a list"}},
		{"null for a list", "transactions", "null", []string{"problem: the answer is null, not a list"}},
		{"null for one", "account", "null", []string{"problem: the answer is null, not an object"}},
		{"a list of two for one", "transaction", "[" + baseTransaction + "," + baseTransaction + "]", []string{
			"problem: the answer is a list of 2, where one was expected",
		}},
		{"a list of a number for one", "userPreference", "[1]", []string{"problem: the answer is a list holding a number, not an object"}},
		{"not JSON", "accounts", "[" + account, []string{"problem: the answer is not JSON"}},
		{"a page", "account", "<html>zq9</html>", []string{"problem: the answer is not JSON"}},
		{"nothing", "userPreference", "", []string{"problem: the answer is not JSON"}},

		{"records that are not objects", "accounts", "[null," + account + ",5]", []string{
			"problem: Schwab sent null where an object was expected",
			"problem: Schwab sent a number where an object was expected",
		}},
		{"a problem in each record", "transactions", "[" + noTime + "," + noTime + "," + noTime + "]", []string{
			"problem: time: missing, though Schwab sends it on every record of its kind (in 3 places)",
		}},
		{"a position's problem", "account", `{"securitiesAccount":` + withKeys(baseSecuritiesAccount,
			"positions", "["+basePosition+","+withKeys(basePosition, "marketValue", `"x"`)+"]") + "}", []string{
			"problem: securitiesAccount.positions.marketValue: Schwab sent a string where a number was expected",
		}},
		{"a value quoted", "transaction", withKeys(baseTransaction, "time", `"zq9"`), []string{
			`problem: time: Schwab sent "…", which is not a time`,
		}},

		{"unknown keys", "transaction", withKeys(baseTransaction, "fee", "1", "problems", "[]"), []string{
			"unknown key: fee: no field takes it", "unknown key: problems: no field takes it",
		}},
		{"a key in another case", "transaction", withKeys(baseTransaction, "status", "", "Status", `"VALID"`), []string{
			"unknown key: Status: status in another case",
		}},
		{"a key in each record", "accountNumbers", `[{"accountNumber":"1","hashValue":"H","x":1},{"accountNumber":"2","hashValue":"H","x":2}]`, []string{
			"unknown key: x: no field takes it (in 2 places)",
		}},

		{"null for positions", "accounts", `[{"securitiesAccount":` + withKeys(baseSecuritiesAccount, "positions", "null") + "}]", []string{
			"null, reported only: securitiesAccount.positions: Schwab sent null, which reads as absent",
		}},
		{"a number too large for positions", "accounts", `[{"securitiesAccount":` + withKeys(baseSecuritiesAccount, "positions", "1e999") + "}]", []string{
			"problem: securitiesAccount.positions: Schwab sent a number where an array was expected",
		}},

		{"a scalar sent again", "transaction", withKeys(baseTransaction, "transferItems",
			`[{"instrument":{"assetType":"EQUITY","symbol":"X","assetType":"EQUITY"},"amount":1,"cost":1}]`), []string{
			"repeated key, reported only: transferItems.instrument.assetType: sent 2 times, the same each time",
		}},
		{"a scalar sent again with another value", "transactions", `[{"subAccount":"MARGIN",` + baseTransaction[1:] + "]", []string{
			"repeated key: subAccount: sent 2 times, with different values",
		}},
		{"an object sent twice", "account", `{"securitiesAccount":` + baseSecuritiesAccount + "," + account[1:], []string{
			"problem: securitiesAccount: Schwab sent it more than once, so what it holds is uncertain",
			"repeated key: securitiesAccount: sent 2 times, an object or array among the copies",
		}},
		{"a key in another case sent again", "transaction", withKeys(baseTransaction, "\u017ftatus", `"VALID"`), []string{
			"unknown key: \u017ftatus: status in another case",
			"repeated key, reported only: status: sent 2 times, the same each time, spelled status and \u017ftatus",
		}},
		{"three spellings", "transaction", withKeys(baseTransaction, "Status", `"VALID"`, "STATUS", `"VALID"`), []string{
			"unknown key: STATUS: status in another case",
			"unknown key: Status: status in another case",
			"repeated key, reported only: status: sent 3 times, the same each time, spelled status, Status and STATUS",
		}},
	} {
		if got := checked(tc.kind, tc.body); !slices.Equal(got, tc.want) {
			t.Errorf("%s: checkAnswer(%q) found\n%q\nwant\n%q", tc.name, tc.kind, got, tc.want)
		}
	}
	defer func() {
		if r := recover(); fmt.Sprint(r) != `checkAnswer: no kind of answer is called "orders"` {
			t.Errorf("checkAnswer of a kind it does not read: %v", r)
		}
	}()
	checkAnswer("orders", []byte("[]"))
}

// valueFree takes out each value that a problem quotes, a string or a
// number, whatever its escapes, and leaves the rest of the words alone.
func TestValueFree(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`Schwab sent "2026-13-01", which is not a time`, `Schwab sent "…", which is not a time`},
		{`Schwab sent "a\"b\\", which is not a time`, `Schwab sent "…", which is not a time`},
		{`Schwab sent "\u2028\xff", whose offset from UTC is out of range`, `Schwab sent "…", whose offset from UTC is out of range`},
		{"Schwab sent the number 1e999, which is too large", "Schwab sent the number …, which is too large"},
		{"Schwab sent the number -12.5E+3 where a whole number was expected", "Schwab sent the number … where a whole number was expected"},
		{
			"the JSON could not be read: json: cannot unmarshal number 1e999 into Go struct field .netAmount of type float64",
			"the JSON could not be read: json: cannot unmarshal number … into Go struct field .netAmount of type float64",
		},
		{`Schwab's value could not be read: strconv.ParseInt: parsing "99": value out of range`, `Schwab's value could not be read: strconv.ParseInt: parsing "…": value out of range`},
		{"Schwab sent a string where a number was expected", "Schwab sent a string where a number was expected"},
		{"json: cannot unmarshal number into Go value of type []schwab.Problem", "json: cannot unmarshal number into Go value of type []schwab.Problem"},
		{"missing, though Schwab sends it on every record of its kind", "missing, though Schwab sends it on every record of its kind"},
	} {
		if got := valueFree(tc.in); got != tc.want {
			t.Errorf("valueFree(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// No finding holds a value of Schwab's, whatever its problem quoted: here a
// string that is no time, inside escapes too, a time whose offset or year is
// out of range, and numbers too large or not whole, each a value found
// nowhere else, which the records' problems quote.
func TestFindingsHoldNoValues(t *testing.T) {
	secrets := []string{`zq9time`, `zq9esc`, `2031-07-08T09:10:11+2400`, `9999-12-31T23:00:00-2300`, `7e777`, `98765.4321`, `123456789012345678901234567890`}
	tx := withKeys(baseTransaction,
		"time", `"zq9time"`,
		"tradeDate", `"2031-07-08T09:10:11+2400"`,
		"settlementDate", `"9999-12-31T23:00:00-2300"`,
		"netAmount", "7e777",
		"activityId", "98765.4321",
		"transferItems", `[{"instrument":{"assetType":"OPTION","symbol":"X","expirationDate":"a\"zq9esc\\","instrumentId":123456789012345678901234567890},"amount":1,"cost":1}]`)
	var decoded Transaction
	if err := json.Unmarshal([]byte(tx), &decoded); err != nil {
		t.Fatal(err)
	}
	problems := strings.Join(problemStrings(decoded.Problems), "\n")
	fs := checkAnswer("transaction", []byte(tx))
	if len(fs) != len(secrets) {
		t.Errorf("%d findings, want one for each of the %d values: %v", len(fs), len(secrets), fs)
	}
	for _, form := range []string{
		"which is not a time", "whose offset from UTC is out of range", "which is outside the years 0 to 9999 in UTC",
		"which is too large", "where a whole number was expected",
	} {
		if !strings.Contains(problems, form) {
			t.Errorf("no problem says %q: %s", form, problems)
		}
	}
	for _, s := range secrets {
		if !strings.Contains(problems, s) {
			t.Errorf("no problem quotes %s, so the test shows nothing of it: %s", s, problems)
		}
		for _, f := range fs {
			if strings.Contains(f.String(), s) {
				t.Errorf("a finding holds %s: %s", s, f)
			}
		}
	}
}

// census gathers the keys of records, for the tests of the fixtures and for
// a report of the keys that Schwab sends. Each record, decoded as any, is
// walked against its type, its keys matched to fields as encoding/json and
// the decoders' walk match them, in another case too. It takes any JSON: a
// value of a kind that its field does not take, null included, is not looked
// into, and the records' problems name it, unless it is a null that reads as
// absent, which add returns.
type census struct {
	// records holds, for each struct type, the keys of each of its records
	// that a walk met, each named as its field names it.
	records map[reflect.Type][]map[string]bool
}

// newCensus returns a census that has walked no record.
func newCensus() *census {
	return &census{records: map[reflect.Type][]map[string]bool{}}
}

// An unknownKey is a key that no field takes, or that a field takes only in
// another case.
type unknownKey struct {
	path  string // the key's path, the key as Schwab sent it
	field string // the key of the field that takes it in another case, or ""
}

// String returns the key's path, and the field's key if it is in another
// case, as in "securitiesAccount.RoundTrips (roundTrips in another case)".
func (k unknownKey) String() string {
	if k.field != "" {
		return k.path + " (" + k.field + " in another case)"
	}
	return k.path
}

// problemsType is the type of a record's own Problems, which no key of
// Schwab's goes to: a problems key from Schwab is one that no field takes.
var problemsType = reflect.TypeFor[[]Problem]()

// add walks v, a record of type t decoded as any, and adds the keys of each
// record within it to c.records. It returns the keys that no field takes,
// and, by their key paths, the nulls where a field that is not on every
// record takes an object or an array: such a null reads as absent, so the
// records' problems say nothing of it, while every other null and every
// value of another kind is one of their problems.
func (c *census) add(t reflect.Type, v any) (unknown []unknownKey, nulls []string) {
	w := censusWalk{c: c}
	w.value(t, v, "")
	return w.unknown, w.nulls
}

// censusWalk is one walk of a census, with what it found.
type censusWalk struct {
	c       *census
	unknown []unknownKey
	nulls   []string
}

// value walks v, JSON decoded as any, against t, at path. It looks into an
// object only for a struct and an array only for a slice, and never into a
// time, which holds no keys of Schwab's.
func (w *censusWalk) value(t reflect.Type, v any, path string) {
	t = derefType(t)
	if t == timeType {
		return
	}
	switch t.Kind() {
	case reflect.Slice:
		elems, _ := v.([]any)
		for _, e := range elems {
			w.value(t.Elem(), e, path)
		}
	case reflect.Struct:
		members, ok := v.(map[string]any)
		if !ok {
			return
		}
		si := infoOf(t)
		keys := map[string]bool{}
		for _, key := range slices.Sorted(maps.Keys(members)) {
			j := si.field([]byte(key))
			if j < 0 || t.Field(si.fields[j].index).Type == problemsType {
				w.unknown = append(w.unknown, unknownKey{path: joinPath(path, keyName(key))})
				continue
			}
			f := si.fields[j]
			if key != f.name {
				w.unknown = append(w.unknown, unknownKey{path: joinPath(path, keyName(key)), field: f.name})
			}
			keys[f.name] = true
			x := members[key]
			if x == nil && f.optional && f.composite {
				w.nulls = append(w.nulls, joinPath(path, f.name))
			}
			w.value(t.Field(f.index).Type, x, joinPath(path, f.name))
		}
		w.c.records[t] = append(w.c.records[t], keys)
	}
}

// keyName returns key, one of Schwab's, as a key path shows it: as it is, if
// it is a word of letters, digits and underscores, and else quoted, so that a
// dot or a space in it does not read as part of the path, and a character
// that a report cannot show as itself is escaped.
func keyName(key string) string {
	plain := key != "" && strings.IndexFunc(key, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) < 0
	if plain {
		return key
	}
	return strconv.Quote(key)
}

// A key path shows a key of letters, digits and underscores as it is, in
// any script, and quotes any other, so that its dots are only the path's.
func TestKeyName(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"accountNumber", "accountNumber"},
		{"_x9", "_x9"},
		{"\u017ftatus", "\u017ftatus"},
		{"\u0130d", "\u0130d"},
		{"\u212a", "\u212a"},
		{"", `""`},
		{"a.b", `"a.b"`},
		{"a b", `"a b"`},
		{"a-b", `"a-b"`},
		{`a"b`, `"a\"b"`},
		{"a\nb", `"a\nb"`},
		{"\u2028", `"\u2028"`},
		{"a\xffb", `"a\xffb"`},
	} {
		if got := keyName(tc.key); got != tc.want {
			t.Errorf("keyName(%q) = %s, want %s", tc.key, got, tc.want)
		}
	}
}

// censusOf walks each record of body, a JSON list of records of type typ,
// with a census of its own, and returns the census with the walks' unknown
// keys and nulls, as text.
func censusOf(t *testing.T, typ reflect.Type, body string) (c *census, unknown, nulls []string) {
	t.Helper()
	var records []any
	if err := json.Unmarshal([]byte(body), &records); err != nil {
		t.Fatal(err)
	}
	c = newCensus()
	for _, r := range records {
		keys, ns := c.add(typ, r)
		for _, k := range keys {
			unknown = append(unknown, k.String())
		}
		nulls = append(nulls, ns...)
	}
	return c, unknown, nulls
}

// The census names each key that no field takes by its path: Schwab's own
// problems key, in any case, among them, and a key that a field takes in
// another case, which it follows into, as the decoders do, and names as
// such. It counts the keys of each record by the names of their fields.
func TestCensusUnknownKeys(t *testing.T) {
	sa := withKeys(baseSecuritiesAccount, "roundTrips", "", "RoundTrips", "0", "bonus", "1",
		"positions", "["+withKeys(basePosition, "Problems", "[]", "instrument", `{"assetType":"EQUITY","symbol":"X","a.b":1}`)+"]",
		"CurrentBalances", `{"liquidationValue":5,"rate":0}`, "currentBalances", "")
	body := "[" + withKeys(`{"securitiesAccount":`+sa+"}", "problems", "[]") + "]"
	c, unknown, nulls := censusOf(t, reflect.TypeFor[Account](), body)
	want := []string{
		"problems",
		"securitiesAccount.CurrentBalances (currentBalances in another case)",
		"securitiesAccount.currentBalances.rate",
		"securitiesAccount.RoundTrips (roundTrips in another case)",
		"securitiesAccount.bonus",
		"securitiesAccount.positions.Problems",
		`securitiesAccount.positions.instrument."a.b"`,
	}
	if !slices.Equal(unknown, want) || nulls != nil {
		t.Errorf("unknown keys %q, nulls %q; want %q and none", unknown, nulls, want)
	}
	sas := c.records[reflect.TypeFor[SecuritiesAccount]()]
	if len(sas) != 1 || !sas[0]["roundTrips"] || !sas[0]["currentBalances"] || sas[0]["RoundTrips"] || sas[0]["bonus"] {
		t.Errorf("the securities account's keys: %v", sas)
	}
	if n := len(c.records[reflect.TypeFor[Balances]()]); n != 3 {
		t.Errorf("%d balances, want the three of the account", n)
	}
	if ps := c.records[reflect.TypeFor[Position]()]; len(ps) != 1 || ps[0]["problems"] || !ps[0]["instrument"] {
		t.Errorf("the position's keys: %v", ps)
	}
}

// A null where a record takes an object or an array, or a value of another
// kind, stops neither the census nor the walk of what follows: the census
// does not look into it, and counts no record for it. A null that reads as
// absent, where the field is not on every record, is returned; every other
// one, and every value of another kind, is left to the records' problems,
// which name it.
func TestCensusTakesNullsAndOtherKinds(t *testing.T) {
	body := "[" + strings.Join([]string{
		`{"securitiesAccount":` + withKeys(baseSecuritiesAccount, "positions", "null") + `,"aggregatedBalance":null}`,
		`{"securitiesAccount":null}`,
		`{"securitiesAccount":"x"}`,
		`{"securitiesAccount":` + withKeys(baseSecuritiesAccount, "positions", `[null,1,"x",[],`+basePosition+`]`, "initialBalances", "[]") + `}`,
		`null`, `5`, `[]`,
	}, ",") + "]"
	c, unknown, nulls := censusOf(t, reflect.TypeFor[Account](), body)
	if want := []string{"aggregatedBalance", "securitiesAccount.positions"}; unknown != nil || !slices.Equal(nulls, want) {
		t.Errorf("unknown keys %q, nulls %q; want none and %q", unknown, nulls, want)
	}
	for typ, n := range map[reflect.Type]int{
		reflect.TypeFor[Account]():           4,
		reflect.TypeFor[SecuritiesAccount](): 2,
		reflect.TypeFor[Balances]():          5,
		reflect.TypeFor[Position]():          1,
		reflect.TypeFor[Instrument]():        1,
	} {
		if got := len(c.records[typ]); got != n {
			t.Errorf("%d records of %s, want %d", got, typ.Name(), n)
		}
	}
	for _, r := range c.records[reflect.TypeFor[Account]()] {
		if !r["securitiesAccount"] {
			t.Errorf("an account without securitiesAccount: %v", r)
		}
	}

	// A null deeper in, in a field that is not on every record; a null where
	// a field takes a scalar, which the census does not look into, whether
	// on every record or not; and a time sent as an object, which holds no
	// keys of Schwab's.
	tx := withKeys(baseTransaction, "transferItems", `[{"instrument":{"assetType":"OPTION","symbol":"X","optionDeliverables":null}},`+
		`{"instrument":{"assetType":"OPTION","symbol":"X","optionDeliverables":[{"deliverable":null,"deliverableUnits":1}]}}]`,
		"description", "null", "status", "null", "time", `{"seconds":1}`)
	if _, unknown, nulls := censusOf(t, reflect.TypeFor[Transaction](), "["+tx+"]"); unknown != nil || !slices.Equal(nulls, []string{
		"transferItems.instrument.optionDeliverables", "transferItems.instrument.optionDeliverables.deliverable",
	}) {
		t.Errorf("unknown keys %q, nulls %q", unknown, nulls)
	}
}

// A repeat is a key that one object of an answer sends more than once.
type repeat struct {
	path      string   // the object's path and the key, as first sent
	n         int      // how many times the object sent it
	spellings []string // each way the key was spelled, in order, when there were several
	same      bool     // whether every copy has the same bytes
	composite bool     // whether a copy is an object or an array
}

// repeats returns each key that an object in b, JSON at path, sends more
// than once, at any depth, read with the package's scanner: of each object,
// those within its members' values, and then its own, in the order of their
// first copies. A key is compared with the object's earlier keys by
// bytes.EqualFold, as encoding/json and the decoders' walk match keys to
// fields. strings.ToLower would be wrong both ways: it keeps ſtatus, with a
// long s, apart from status, which encoding/json takes for one key, and
// makes İd, with a dotted capital I, equal id, which it does not. Where b is
// not JSON, the scan stops where the scanner does.
func repeats(b []byte, path string) []repeat {
	b = trimSpace(b)
	if len(b) == 0 {
		return nil
	}
	var out []repeat
	switch b[0] {
	case '[':
		s := scan(b, '[')
		for e, ok := s.element(); ok; e, ok = s.element() {
			out = append(out, repeats(e, path)...)
		}
	case '{':
		type sent struct {
			key   []byte // as first sent
			first []byte // the first copy
			r     repeat
		}
		var keys []*sent
		s := scan(b, '{')
		for key, value, ok := s.member(); ok; key, value, ok = s.member() {
			at := joinPath(path, keyName(string(key)))
			composite := value[0] == '{' || value[0] == '['
			if i := slices.IndexFunc(keys, func(k *sent) bool { return bytes.EqualFold(k.key, key) }); i >= 0 {
				r := &keys[i].r
				r.n++
				r.same = r.same && bytes.Equal(value, keys[i].first)
				r.composite = r.composite || composite
				if !slices.Contains(r.spellings, string(key)) {
					r.spellings = append(r.spellings, string(key))
				}
			} else {
				keys = append(keys, &sent{key: key, first: value, r: repeat{
					path: at, n: 1, spellings: []string{string(key)}, same: true, composite: composite,
				}})
			}
			out = append(out, repeats(value, at)...)
		}
		for _, k := range keys {
			if k.r.n > 1 {
				if len(k.r.spellings) == 1 {
					k.r.spellings = nil
				}
				out = append(out, k.r)
			}
		}
	}
	return out
}

// finding returns the repeat as a finding. Copies that differ, or an object
// or array among them, fail, since encoding/json keeps a scalar's last copy
// and may join the copies of an object, so that a record need not hold what
// each copy says. A scalar sent again with the same bytes is only reported.
func (r repeat) finding() finding {
	what := "sent " + strconv.Itoa(r.n) + " times"
	switch {
	case r.composite:
		what += ", an object or array among the copies"
	case r.same:
		what += ", the same each time"
	default:
		what += ", with different values"
	}
	if len(r.spellings) > 1 {
		names := make([]string, len(r.spellings))
		for i, s := range r.spellings {
			names[i] = keyName(s)
		}
		what += ", spelled " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
	return finding{check: repeatCheck, path: r.path, what: what, fails: r.composite || !r.same}
}

// The scan finds each key that an object sends more than once, at any depth
// and in every element of an array, and tells copies that are the same, by
// their bytes, from copies that differ and from an object or array among
// them; a key sent in another case is the same key, with each spelling
// named.
func TestRepeats(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []repeat
	}{
		{`{"a":1,"b":{"a":2},"c":[{"a":3}]}`, nil},
		{`{"a":1,"a":1}`, []repeat{{path: "a", n: 2, same: true}}},
		{` { "a" : 1 , "a" :1 } `, []repeat{{path: "a", n: 2, same: true}}},
		{`{"a":1,"a":2,"a":1}`, []repeat{{path: "a", n: 3}}},
		{`{"a":1,"a":1.0}`, []repeat{{path: "a", n: 2}}},
		{`{"a":"x","a":"\u0078"}`, []repeat{{path: "a", n: 2}}},
		{`{"a":null,"a":5}`, []repeat{{path: "a", n: 2}}},
		{`{"a":"x","b":0,"A":"x"}`, []repeat{{path: "a", n: 2, spellings: []string{"a", "A"}, same: true}}},
		{`{"\u0061":1,"a":1}`, []repeat{{path: "a", n: 2, same: true}}},
		{`{"a.b":1,"A.B":2}`, []repeat{{path: `"a.b"`, n: 2, spellings: []string{"a.b", "A.B"}}}},
		{`{"a":{},"a":{}}`, []repeat{{path: "a", n: 2, same: true, composite: true}}},
		{`{"a":[1],"a":null}`, []repeat{{path: "a", n: 2, composite: true}}},
		{`{"a":1,"a":[1]}`, []repeat{{path: "a", n: 2, composite: true}}},
		{`[{"x":{"y":1,"y":1}},{"x":[{"z":0,"Z":0}]}]`, []repeat{
			{path: "x.y", n: 2, same: true},
			{path: "x.z", n: 2, spellings: []string{"z", "Z"}, same: true},
		}},
		{`{"o":{"k":1,"k":2},"o":{"k":3,"K":3}}`, []repeat{
			{path: "o.k", n: 2},
			{path: "o.k", n: 2, spellings: []string{"k", "K"}, same: true},
			{path: "o", n: 2, composite: true},
		}},
		{"{\"status\":\"first\",\"\u017ftatus\":\"second\"}", []repeat{{path: "status", n: 2, spellings: []string{"status", "\u017ftatus"}}}},
		{`{"status":"first","\u017ftatus":"second"}`, []repeat{{path: "status", n: 2, spellings: []string{"status", "\u017ftatus"}}}},
		{"{\"id\":1,\"\u0130d\":1}", nil},
		{"{\"k\":1,\"\u212a\":1}", []repeat{{path: "k", n: 2, spellings: []string{"k", "\u212a"}, same: true}}},
		{`[]`, nil},
		{`null`, nil},
		{`"a"`, nil},
		{``, nil},
	} {
		if got := repeats([]byte(tc.body), ""); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("repeats(%s):\n got %+v\nwant %+v", tc.body, got, tc.want)
		}
	}
}

// Two keys are one key to the scan exactly when encoding/json takes them for
// one, filling a field tagged with either from the other: under Unicode's
// simple case folding, by which the long s and the Kelvin sign fold with s
// and k, while the dotted capital I and the dotless i fold with no other
// letter, and ss is not ß.
func TestRepeatsFoldAsEncodingJSON(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		one  bool
	}{
		{"status", "\u017ftatus", true},
		{"k", "\u212a", true},
		{"id", "\u0130d", false},
		{"a", "A", true},
		{"a_b", "A_B", true},
		{"\u00df", "\u1e9e", true},
		{"\u03c3", "\u03c2", true},
		{"\u03a3", "\u03c2", true},
		{"\u00e9", "\u00c9", true},
		{"i", "\u0131", false},
		{"ss", "\u00df", false},
		{"a_b", "ab", false},
	} {
		typ := reflect.StructOf([]reflect.StructField{{Name: "F", Type: reflect.TypeFor[int](), Tag: reflect.StructTag(`json:"` + tc.a + `"`)}})
		v := reflect.New(typ)
		b, err := json.Marshal(tc.b)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte("{"+string(b)+":1}"), v.Interface()); err != nil {
			t.Fatal(err)
		}
		a, err := json.Marshal(tc.a)
		if err != nil {
			t.Fatal(err)
		}
		found := len(repeats([]byte("{"+string(a)+":1,"+string(b)+":2}"), "")) == 1
		if one := v.Elem().Field(0).Int() == 1; one != tc.one || found != tc.one {
			t.Errorf("%q and %q: encoding/json takes them for one key: %v; the scan: %v; want %v", tc.a, tc.b, one, found, tc.one)
		}
	}
}

// tokenRepeats returns what repeats should for b, valid JSON at path, found
// another way, for FuzzRepeatedKeys to check repeats by: b is read with a
// json.Decoder, and two keys of an object are one when they fold alike, each
// rune folded to the least rune of its orbit under unicode.SimpleFold, as
// encoding/json folds a key to match it to a field.
func tokenRepeats(t *testing.T, b []byte, path string) []repeat {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	value := func() json.RawMessage {
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	tok, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	var out []repeat
	switch tok {
	case json.Delim('['):
		for dec.More() {
			out = append(out, tokenRepeats(t, value(), path)...)
		}
	case json.Delim('{'):
		byFold := map[string]*repeat{}
		firsts := map[string]json.RawMessage{}
		var folds []string
		for dec.More() {
			tok, err := dec.Token()
			if err != nil {
				t.Fatal(err)
			}
			key, v := tok.(string), value()
			at := joinPath(path, keyName(key))
			composite := v[0] == '{' || v[0] == '['
			if r := byFold[folded(key)]; r != nil {
				r.n++
				r.same = r.same && bytes.Equal(v, firsts[folded(key)])
				r.composite = r.composite || composite
				if !slices.Contains(r.spellings, key) {
					r.spellings = append(r.spellings, key)
				}
			} else {
				byFold[folded(key)] = &repeat{path: at, n: 1, spellings: []string{key}, same: true, composite: composite}
				firsts[folded(key)] = v
				folds = append(folds, folded(key))
			}
			out = append(out, tokenRepeats(t, v, at)...)
		}
		for _, f := range folds {
			if r := byFold[f]; r.n > 1 {
				if len(r.spellings) == 1 {
					r.spellings = nil
				}
				out = append(out, *r)
			}
		}
	}
	return out
}

// folded returns key as encoding/json folds it to match it to a field: each
// rune as the least rune of its orbit under Unicode's simple case folding.
func folded(key string) string {
	var b strings.Builder
	for _, r := range key {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}

// The scan finds what a json.Decoder finds in valid JSON, with keys folded
// as encoding/json folds them, and stops without failing on anything else.
// The seeds hold the pairs that strings.ToLower gets wrong, the long s,
// which folds with s, and the dotted capital I, which folds with no other
// letter; the Kelvin sign, which folds with k; and every JSON fixture.
func FuzzRepeatedKeys(f *testing.F) {
	for _, seed := range []string{
		"{\"status\":\"first\",\"\u017ftatus\":\"second\"}",
		"{\"id\":1,\"\u0130d\":2}",
		"{\"k\":1,\"\u212a\":2}",
		`{"status":"first","\u017ftatus":"second"}`,
		`{"Status":"x","status":"x","STATUS":"x"}`,
		`{"a":1,"a":1.0}`, `{"a":"x","a":"\u0078"}`, `{"\u0061":1,"a":1}`, `{"a":"\ud800","\udfff":1,"\ufffd":1}`,
		`{"a":{"b":1,"b":1},"A":{"b":2}}`, `{"a":[1],"a":null}`,
		` [ {"x":[{"y":0,"Y":0}]} , {"x":{"y":1}} ] `, "{\"a\"\t:\r\n1,\n\"a\":1}",
		`{}`, `[]`, `null`, `5`, `"x"`,
		`{"a":1,"a"`, `{"a":1,"a":}`, `[{"a":1,"a":1}`, `{"a":"\"}`, `{"a" 1}`, `{,"a":1}`,
	} {
		f.Add(seed)
	}
	for _, e := range schwabtest.Index() {
		if b := readTestdata(f, e.File); schwabtest.ExtensionFor(b) == ".json" {
			f.Add(string(b))
		}
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := repeats([]byte(s), "")
		if !json.Valid([]byte(s)) || !utf8.ValidString(s) {
			return // only not failing
		}
		if want := tokenRepeats(t, []byte(s), ""); !reflect.DeepEqual(got, want) {
			t.Errorf("repeats in %s:\n got %+v\nwant %+v", s, got, want)
		}
	})
}

// assetTypeSentTwice is what checkAnswer reports of transaction.json, which
// sends an instrument's assetType twice, the same each time, as a capture of
// 2024 does.
const assetTypeSentTwice = "repeated key, reported only: transferItems.instrument.assetType: sent 2 times, the same each time"

// Every fixture that answers an account route with 200 passes checkAnswer
// as its route's kind, a recording as well as the hand-made ones, and what
// is reported of a recording is logged. Of the hand-made, only
// transaction.json is reported.
func TestCheckFixtures(t *testing.T) {
	want := map[string][]string{
		"account_numbers.json": nil,
		"accounts.json":        nil,
		"transactions.json":    nil,
		"transaction.json":     {assetTypeSentTwice},
		"user_preference.json": nil,
	}
	checked := map[string]bool{}
	for _, e := range schwabtest.Index() {
		route, ok := e.Route()
		if !ok || e.Response.Status != http.StatusOK {
			continue
		}
		kind, ok := kindOfRoute(route)
		if !ok {
			continue
		}
		var reported []string
		for _, f := range checkAnswer(kind.name, readTestdata(t, e.File)) {
			if f.fails {
				t.Errorf("%s, an answer of %s: %s", e.File, kind.name, f)
			} else {
				reported = append(reported, f.String())
			}
		}
		if e.Source == "recorded" {
			for _, r := range reported {
				t.Logf("%s, an answer of %s: %s", e.File, kind.name, r)
			}
			continue
		}
		checked[e.File] = true
		if !slices.Equal(reported, want[e.File]) {
			t.Errorf("%s: reported %q, want %q", e.File, reported, want[e.File])
		}
	}
	for _, file := range slices.Sorted(maps.Keys(want)) {
		if !checked[file] {
			t.Errorf("%s was not checked", file)
		}
	}
}

// Every answer of the fake's world, as LoadFixtures fills it, passes
// checkAnswer: the account numbers, the accounts with their positions and
// without, each account alone, each account's transactions over windows
// back past its oldest, each transaction by its id, and the user's
// preferences, the fixture's and those the fake builds. Only the
// transaction that transaction.json answers by id is reported.
func TestCheckTheFakesAnswers(t *testing.T) {
	cl, f := fixturesClient(t)
	rows, err := decodeList[Transaction](readTestdata(t, "transactions.json"))
	if err != nil {
		t.Fatal(err)
	}
	byID, err := decodeOne[Transaction](readTestdata(t, "transaction.json"))
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	check := func(kind, path string, query url.Values, want ...string) []byte {
		t.Helper()
		body, err := cl.Get(t.Context(), path, query)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		kinds[kind]++
		if got := checked(kind, string(body)); !slices.Equal(got, want) {
			t.Errorf("%s?%s, an answer of %s: found %q, want %q", path, query.Encode(), kind, got, want)
		}
		return body
	}
	positions := url.Values{"fields": {"positions"}}
	check("accountNumbers", "/trader/v1/accounts/accountNumbers", nil)
	check("accounts", "/trader/v1/accounts", nil)
	check("accounts", "/trader/v1/accounts", positions)
	oldest := testNow
	for _, row := range rows {
		if row.Time.Before(oldest) {
			oldest = row.Time
		}
	}
	const window = MaxTransactionWindow - time.Millisecond
	listed := 0
	for _, fa := range schwabtest.FixtureAccounts {
		path := "/trader/v1/accounts/" + fa.Hash
		check("account", path, nil)
		check("account", path, positions)
		for end := testNow; ; {
			start := end.Add(-window)
			q, err := TransactionQuery{Start: start, End: end}.values()
			if err != nil {
				t.Fatal(err)
			}
			listed += len(elements(t, check("transactions", path+"/transactions", q)))
			if start.Before(oldest) {
				break
			}
			end = start.Add(-time.Millisecond)
		}
	}
	if listed != len(rows) {
		t.Errorf("the windows listed %d transactions, want the fixture's %d", listed, len(rows))
	}
	hashes := fixtureHashes()
	for _, row := range rows {
		var want []string
		if row.ActivityID == byID.ActivityID {
			want = []string{assetTypeSentTwice}
		}
		path := "/trader/v1/accounts/" + string(hashes[row.AccountNumber]) + "/transactions/" + strconv.FormatInt(row.ActivityID, 10)
		check("transaction", path, nil, want...)
	}
	check("userPreference", "/trader/v1/userPreference", nil)
	f.SetUserPreference("")
	check("userPreference", "/trader/v1/userPreference", nil)
	for _, k := range answerKinds {
		if kinds[k.name] == 0 {
			t.Errorf("no answer of %s was checked", k.name)
		}
	}
}
