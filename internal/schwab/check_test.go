package schwab

import (
	"bytes"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

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
