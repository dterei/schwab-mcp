package schwabtest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// decodeAny decodes b, failing the test if it is not JSON.
func decodeAny(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return v
}

// Every fixture account loses its positions and nothing else, laid out
// pretty, compact or with tabs, and keeps its layout.
func TestWithoutPositionsOnTheFixtures(t *testing.T) {
	accounts, err := elements(Fixture(t, "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	cut := 0
	for i, pretty := range accounts {
		var compact, tabs bytes.Buffer
		if err := json.Compact(&compact, pretty); err != nil {
			t.Fatal(err)
		}
		if err := json.Indent(&tabs, compact.Bytes(), "", "\t"); err != nil {
			t.Fatal(err)
		}
		for _, form := range []struct {
			name           string
			b              []byte
			prefix, indent string
		}{
			{"pretty", pretty, "  ", "  "},
			{"compact", compact.Bytes(), "", ""},
			{"with tabs", tabs.Bytes(), "", "\t"},
		} {
			got := withoutPositions(form.b)
			want := decodeAny(t, form.b).(map[string]any)
			if sa, ok := want["securitiesAccount"].(map[string]any); ok {
				if _, ok := sa["positions"]; ok {
					cut++
				}
				delete(sa, "positions")
			}
			if !reflect.DeepEqual(decodeAny(t, got), want) {
				t.Errorf("account %d, %s: %s", i, form.name, got)
			}
			if bytes.Contains(got, []byte(`"positions"`)) {
				t.Errorf("account %d, %s: positions are left", i, form.name)
			}
			// What is left is laid out as the rest was: no comma or blank
			// line where the positions were.
			var again bytes.Buffer
			if form.indent == "" {
				_ = json.Compact(&again, got)
			} else {
				var c bytes.Buffer
				_ = json.Compact(&c, got)
				_ = json.Indent(&again, c.Bytes(), form.prefix, form.indent)
			}
			if !bytes.Equal(again.Bytes(), got) {
				t.Errorf("account %d, %s: the layout changed:\n%s", i, form.name, got)
			}
		}
	}
	if cut != 2*3 {
		t.Errorf("positions cut from %d forms of accounts, want the 2 accounts that hold some in 3 forms", cut)
	}
}

// The positions splice cuts every positions member of every
// securitiesAccount, keys matched as the client matches them, and leaves
// the neighbors' white space and every other byte.
func TestWithoutPositions(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"first", `{"securitiesAccount":{"positions":[1],"a":1}}`, `{"securitiesAccount":{"a":1}}`},
		{"last", `{"securitiesAccount":{"a":1,"positions":[1]}}`, `{"securitiesAccount":{"a":1}}`},
		{"alone", `{"securitiesAccount": { "positions": [] } }`, `{"securitiesAccount": {  } }`},
		{"in another case", `{"securitiesAccount":{"a":1,"Positions":[1]}}`, `{"securitiesAccount":{"a":1}}`},
		{"in an account in another case", `{"SECURITIESACCOUNT":{"positions":[1],"a":1}}`, `{"SECURITIESACCOUNT":{"a":1}}`},
		{"with an escaped key", `{"securitiesAccount":{"a":1,"posi\u0074ions":[1]}}`, `{"securitiesAccount":{"a":1}}`},
		{"with brackets in strings", `{"securitiesAccount":{"positions":[{"s":"]}\"[,"}],"a":"[{"}}`, `{"securitiesAccount":{"a":"[{"}}`},
		{"nested elsewhere", `{"securitiesAccount":{"a":{"positions":[1]}},"positions":[2]}`, `{"securitiesAccount":{"a":{"positions":[1]}},"positions":[2]}`},
		{
			"in an account sent twice", `{"securitiesAccount":{"positions":[1],"a":1},"x":0,"securitiesAccount":{"b":2,"positions":[2]}}`,
			`{"securitiesAccount":{"a":1},"x":0,"securitiesAccount":{"b":2}}`,
		},
		{"sent twice", `{"securitiesAccount":{"positions":[1],"a":1,"positions":[2]}}`, `{"securitiesAccount":{"a":1}}`},
		{"twice, adjacent and last", `{"securitiesAccount":{"a":1,"positions":[1],"positions":[2]}}`, `{"securitiesAccount":{"a":1}}`},
		{"twice and alone", `{"securitiesAccount":{"positions":[1],"positions":[2]}}`, `{"securitiesAccount":{}}`},
		{"in an account that is not an object", `{"securitiesAccount":[{"positions":[1]}]}`, `{"securitiesAccount":[{"positions":[1]}]}`},
		{"in an account that is not a member", `{"x":{"securitiesAccount":{"positions":[1]}}}`, `{"x":{"securitiesAccount":{"positions":[1]}}}`},
		{
			"pretty, followed", "{\n  \"securitiesAccount\": {\n    \"type\": \"CASH\",\n    \"positions\": [\n      1\n    ],\n    \"initialBalances\": {}\n  }\n}",
			"{\n  \"securitiesAccount\": {\n    \"type\": \"CASH\",\n    \"initialBalances\": {}\n  }\n}",
		},
		{
			"pretty, last", "{\n  \"securitiesAccount\": {\n    \"type\": \"CASH\",\n    \"positions\": []\n  }\n}",
			"{\n  \"securitiesAccount\": {\n    \"type\": \"CASH\"\n  }\n}",
		},
		{"none", `{"securitiesAccount":{"a":1},"aggregatedBalance":{}}`, `{"securitiesAccount":{"a":1},"aggregatedBalance":{}}`},
	} {
		if got := string(withoutPositions([]byte(tc.in))); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// withString replaces every string member of a key, as the client matches
// keys, escaping the new value, and says whether there was one.
func TestWithString(t *testing.T) {
	for _, tc := range []struct {
		in, value, want string
		found           bool
	}{
		{`{"accountNumber":"1","hashValue":"A"}`, "B", `{"accountNumber":"1","hashValue":"B"}`, true},
		{"{\n  \"hashValue\" : \"A\",\n  \"x\": 1\n}", "B", "{\n  \"hashValue\" : \"B\",\n  \"x\": 1\n}", true},
		{`{"HashValue":"A","hashValue":"\u0041"}`, "B", `{"HashValue":"B","hashValue":"B"}`, true},
		{`{"hashValue":"A"}`, `B"\`, `{"hashValue":"B\"\\"}`, true},
		{`{"hashValue":5}`, "B", `{"hashValue":5}`, false},
		{`{"x":{"hashValue":"A"}}`, "B", `{"x":{"hashValue":"A"}}`, false},
		{`{}`, "B", `{}`, false},
	} {
		if got, found := withString([]byte(tc.in), "hashValue", tc.value); string(got) != tc.want || found != tc.found {
			t.Errorf("withString(%s, %q) = %s, %v, want %s, %v", tc.in, tc.value, got, found, tc.want, tc.found)
		}
	}
}

// elements splits a list into its elements' bytes, and members and
// elements refuse what is not JSON without panicking.
func TestScanner(t *testing.T) {
	es, err := elements([]byte(" [ {\"a\":[1,\"]\"]} ,\n [2], \"x,]\", -1.5e3 ] "))
	want := []string{`{"a":[1,"]"]}`, `[2]`, `"x,]"`, `-1.5e3`}
	got := make([]string, len(es))
	for i, e := range es {
		got[i] = string(e)
	}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("elements: %q, %v", got, err)
	}
	if es, err := elements([]byte("[]")); err != nil || len(es) != 0 {
		t.Errorf("elements of []: %q, %v", es, err)
	}
	for _, bad := range []string{``, `[`, `[1`, `[1,`, `[1 2]`, `{}`, `["a]`, `[{]`} {
		if es, err := elements([]byte(bad)); err == nil {
			t.Errorf("elements(%s) = %q, want an error", bad, es)
		}
	}
	for _, bad := range []string{``, `{`, `{"a"}`, `{"a":}`, `{"a":1,}`, `{a:1}`, `{"a":1`, `{"a":1 "b":2}`, `{"a`, `{"a":"b}`, `[1]`, `{"\x":1}`} {
		if ms, err := members([]byte(bad), 0); err == nil {
			t.Errorf("members(%s) = %+v, want an error", bad, ms)
		}
	}
	if ms, err := members([]byte(`{"a\"b" : {"c":[]} , "d":null}`), 0); err != nil || len(ms) != 2 || ms[0].key != `a"b` || ms[1].key != "d" {
		t.Errorf("members: %+v, %v", ms, err)
	}
	for _, s := range []string{`{}`, ` {"a":1} `, "{\n}"} {
		if !isObject(s) {
			t.Errorf("isObject(%q) = false", s)
		}
	}
	for _, s := range []string{``, `[]`, `"{}"`, `{"a":}`, `{} {}`, `null`} {
		if isObject(s) {
			t.Errorf("isObject(%q) = true", s)
		}
	}
	if !strings.Contains(string(jsonString("a<b")), `a\u003cb`) {
		t.Errorf("jsonString does not escape as encoding/json does: %s", jsonString("a<b"))
	}
}
