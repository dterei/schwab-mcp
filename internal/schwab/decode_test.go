package schwab

import (
	"bytes"
	"encoding"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// scanned returns what a scanner finds in b: the keys and values of an
// object's members, or the elements of an array, each as "key=value" or
// "value".
func scanned(b []byte) []string {
	var out []string
	if s := scan(b, '{'); s.i < len(b) {
		for key, value, ok := s.member(); ok; key, value, ok = s.member() {
			out = append(out, string(key)+"="+string(value))
		}
		return out
	}
	s := scan(b, '[')
	for value, ok := s.element(); ok; value, ok = s.element() {
		out = append(out, string(value))
	}
	return out
}

// decoded returns what encoding/json finds in b, valid JSON, in the form
// that scanned does.
func decoded(t *testing.T, b []byte) []string {
	var out []string
	switch b = bytes.TrimSpace(b); b[0] {
	case '{':
		dec := json.NewDecoder(bytes.NewReader(b))
		if _, err := dec.Token(); err != nil {
			t.Fatal(err)
		}
		for dec.More() {
			key, err := dec.Token()
			if err != nil {
				t.Fatal(err)
			}
			var value json.RawMessage
			if err := dec.Decode(&value); err != nil {
				t.Fatal(err)
			}
			out = append(out, key.(string)+"="+string(value))
		}
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(b, &elems); err != nil {
			t.Fatal(err)
		}
		for _, e := range elems {
			out = append(out, string(e))
		}
	}
	return out
}

// The scanner that the walk reads objects and arrays with finds what
// encoding/json does in valid JSON, and stops without failing on anything
// else.
func FuzzScanner(f *testing.F) {
	for _, seed := range []string{
		`{}`, `[]`, ` { } `, "[\n]",
		` { "a" : 1 , "b":[1,{"c":"}]"}], "c":{"d":[[],{}]} } `,
		`{"\u0074ype":"x","q\"k":"\\","k\\":"\"","e":-1.5e+3,"t":true,"f":false,"n":null}`,
		`{"a":1,"a":"twice","A":[2]}`,
		`[1, "a,b", {"b": [2, "]"]}, [], {}, null, -0.0, 1E9]`,
		`{"a":"AT&T <x>","b":"\u2028"}`,
		`"x"`, `5`, `null`, `true`,
		`{"a":`, `{"a" 1}`, `{"a":1,}`, `[1,`, `{"a":"\"}`, `{"a":"x`, `[{]`, `{"a":1 "b":2}`, `{,}`, `[,1]`, `{"a":[}`, `{"\`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := scanned([]byte(s))
		if !json.Valid([]byte(s)) || !utf8.ValidString(s) {
			return // only not failing
		}
		if want := decoded(t, []byte(s)); !slices.Equal(got, want) {
			t.Errorf("scanning %s:\n got %q\nwant %q", s, got, want)
		}
	})
}

// The walk relies on what holds of the record types: no field is embedded,
// no two keys of one struct differ only in case, so that each key Schwab
// sends goes to one field, and no value but a time decodes itself from text.
// And a pointer, nil when Schwab did not send its key, is left out of the
// encoding when nil, as the package doc says.
func TestRecordTypesSuitTheWalk(t *testing.T) {
	types := map[reflect.Type]bool{}
	for _, typ := range recordTypes {
		reachable(typ, types)
	}
	for typ := range types {
		var keys []string
		for f := range typ.Fields() {
			if f.Anonymous {
				t.Errorf("%s embeds %s, which the walk does not follow", typ.Name(), f.Name)
			}
			if f.Type.Kind() == reflect.Pointer && !optional(f) {
				t.Errorf("%s.%s is a pointer whose tag does not leave it out when nil", typ.Name(), f.Name)
			}
			ft := f.Type
			for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice {
				ft = ft.Elem()
			}
			if ft != timeType && reflect.PointerTo(ft).Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
				t.Errorf("%s.%s decodes itself from text, which the walk does not know", typ.Name(), f.Name)
			}
			name := jsonName(f)
			if name == "" {
				continue
			}
			for _, k := range keys {
				if strings.EqualFold(k, name) {
					t.Errorf("%s has the keys %q and %q, which differ only in case", typ.Name(), k, name)
				}
			}
			keys = append(keys, name)
		}
	}
}
