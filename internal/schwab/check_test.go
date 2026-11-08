package schwab

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"
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
