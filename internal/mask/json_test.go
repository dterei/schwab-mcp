package mask

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"math/rand/v2"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// testClasses are the classes of the keys that the tests' table knows.
var testClasses = map[string]Class{
	"accountNumber": AccountNumber,
	"hashValue":     Hash,
	"activityId":    Identifier, "orderId": Identifier, "symbol": Identifier, "cusip": Identifier,
	"time": Time, "enteredTime": Time,
	"netAmount": Amount, "price": Amount,
	"description": FreeText, "statusDescription": FreeText,
	"tag":      Tag,
	"withheld": Withheld,
}

// testClass is the tests' table: a path through a key user is Withheld, as
// a transaction's user object is; any other has its last key's class in
// testClasses, or is Unknown.
func testClass(path []string) Class {
	if slices.ContainsFunc(path, func(k string) bool { return strings.EqualFold(k, "user") }) {
		return Withheld
	}
	if len(path) == 0 {
		return Unknown
	}
	return testClasses[path[len(path)-1]]
}

// testKnown returns the numbers that the tests' masking knows.
func testKnown() *Known {
	k := &Known{}
	k.Add("29470135", "1234567", "123456789", "41224019")
	return k
}

// An order shaped like those that Schwab sends, with unknown keys of text,
// of an 8-digit integer, of a code and of a 13-digit ID, and what masking
// makes of it.
const (
	prototypeIn = `{"orderId":1004517688020,"accountNumber":12345678,"tag":"TA_someone at example dot com 1234",
		"enteredTime":"2024-06-06T13:46:03+0000","statusDescription":"Rejected: account 12345678 is closed | AT&T order",
		"price":1.50,"orderLegCollection":[{"instrument":{"cusip":"0F....CK60014000",
		"symbol":"AAPL  240517C00100000"}}],"newKey":"journal to 87654321","newNumber":87654321,
		"newCode":"NEW_STATE","newId":1004517688777}`
	prototypeOut = `{"orderId":1004517688020,"accountNumber":"...678","tag":"TA_...",` +
		`"enteredTime":"2024-06-06T13:46:03+0000","statusDescription":"Rejected: account ...678 is closed | AT&T order",` +
		`"price":1.50,"orderLegCollection":[{"instrument":{"cusip":"0F....CK60014000",` +
		`"symbol":"AAPL  240517C00100000"}}],"newKey":"[withheld]","newNumber":"...321",` +
		`"newCode":"NEW_STATE","newId":1004517688777}`
)

// The order comes out with its key order, 1.50, the 13-digit order ID, the
// option code, the OCC symbol, the time, an unknown code and an unknown
// 13-digit number as they were; the
// numeric account number, the tag, the free text's number, the unknown text
// and the unknown 8-digit integer masked, each counted at its path. Masking
// it again changes nothing and reports nothing.
func TestMaskJSONPrototype(t *testing.T) {
	got, r, err := JSON([]byte(prototypeIn), testClass, testKnown())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != prototypeOut {
		t.Errorf("JSON of the prototype =\n%s\nwant\n%s", got, prototypeOut)
	}
	want := Report{AccountNumbers: 1, Tags: 1, Withheld: 1, UnknownIntegers: 1, Paths: []string{"accountNumber", "tag", "newKey", "newNumber"}}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("the prototype's report = %+v, want %+v", r, want)
	}
	again, r, err := JSON(got, testClass, testKnown())
	if err != nil || string(again) != prototypeOut || !reflect.DeepEqual(r, Report{}) {
		t.Errorf("JSON of the prototype's mask = %s, %+v, %v; want it as it was, an empty report", again, r, err)
	}
}

// Each class masks a string, a number, true, false and null as its rule
// says, and counts what it replaced in the report; what a rule writes, it
// writes again.
func TestMaskJSON(t *testing.T) {
	for _, tc := range []struct {
		key, in, want string
		counted       string // the count of the report that the replacement adds to, or ""
	}{
		// An account number, as a string or a number, as Schwab masks one,
		// a number that is no integer by its literal; Schwab's own masked
		// form stays.
		{"accountNumber", `"12345678"`, `"...678"`, "AccountNumbers"},
		{"accountNumber", `12345678`, `"...678"`, "AccountNumbers"},
		{"accountNumber", `12345678.0`, `"...678"`, "AccountNumbers"},
		{"accountNumber", `1.2345678E7`, `"...678"`, "AccountNumbers"},
		{"accountNumber", `12345678.5`, `"...8.5"`, "AccountNumbers"},
		{"accountNumber", `"61803398"`, `"...398"`, "AccountNumbers"},
		{"accountNumber", `"...678"`, `"...678"`, ""},
		{"accountNumber", `"12"`, `"..."`, "AccountNumbers"},
		{"accountNumber", `"..."`, `"..."`, ""},
		{"accountNumber", `null`, `null`, ""},
		{"accountNumber", `true`, `true`, ""},

		// A hash, kept, though it holds a known number's digits; but a known
		// number that is no hash is masked.
		{"hashValue", `"` + hash + `"`, `"` + hash + `"`, ""},
		{"hashValue", `"ACCT29470135"`, `"ACCT...135"`, "AccountNumbers"},
		{"hashValue", `29470135`, `"...135"`, "AccountNumbers"},
		{"hashValue", `2.9470135e7`, `"...135"`, "AccountNumbers"},

		// Identifiers, times and amounts, kept but for known numbers.
		{"activityId", `73508261946`, `73508261946`, ""},
		{"activityId", `"73508261946"`, `"73508261946"`, ""},
		{"orderId", `1004517688020`, `1004517688020`, ""},
		{"orderId", `87654321`, `87654321`, ""},
		{"orderId", `29470135`, `"...135"`, "AccountNumbers"},
		{"orderId", `-29470135`, `"...135"`, "AccountNumbers"},
		{"orderId", `29470135.0`, `"...135"`, "AccountNumbers"},
		{"orderId", `2.9470135E7`, `"...135"`, "AccountNumbers"},
		{"orderId", `29470135e0`, `"...135"`, "AccountNumbers"},
		{"orderId", `29470135.5`, `29470135.5`, ""},
		{"orderId", `2.94701355E7`, `2.94701355E7`, ""},
		{"symbol", `"ACCT29470135 TRF"`, `"ACCT...135 TRF"`, "AccountNumbers"},
		{"symbol", `"0AMZN.TK12300000"`, `"0AMZN.TK12300000"`, ""},
		{"symbol", `"AAPL  240517C00100000"`, `"AAPL  240517C00100000"`, ""},
		{"symbol", `"/ESH25_P5050:XCME"`, `"/ESH25_P5050:XCME"`, ""},
		{"symbol", `"` + hash + `"`, `"` + hash + `"`, ""},
		{"cusip", `"922908769"`, `"922908769"`, ""},
		{"time", `"2024-06-06T13:46:03+0000"`, `"2024-06-06T13:46:03+0000"`, ""},
		{"time", `1712246167000`, `1712246167000`, ""},
		{"time", `294701350e-1`, `"...135"`, "AccountNumbers"},
		{"time", `"2024-06-06T13:46:03.123456789Z"`, `"2024-06-06T13:46:03....789Z"`, "AccountNumbers"},
		{"netAmount", `1.50`, `1.50`, ""},
		{"netAmount", `-0.0`, `-0.0`, ""},
		{"netAmount", `1.0E-4`, `1.0E-4`, ""},
		{"price", `29470135`, `"...135"`, "AccountNumbers"},
		{"price", `29470135.00`, `"...135"`, "AccountNumbers"},
		{"price", `false`, `false`, ""},

		// Free text, masked by pattern, then by known number; HTML is not
		// escaped; a number is kept, but one that is a known number.
		{"description", `"JOURNAL TO 12345678"`, `"JOURNAL TO ...678"`, ""},
		{"description", `"Rejected: account 12345678 is closed | AT&T order"`, `"Rejected: account ...678 is closed | AT&T order"`, ""},
		{"description", `"JOURNAL TO 1234567"`, `"JOURNAL TO ...567"`, "AccountNumbers"},
		{"description", `"S\u0026P 500 INDEX \u003c\u003e"`, `"S&P 500 INDEX <>"`, ""},
		{"description", `"Dividend 2026-11-11"`, `"Dividend ...111"`, ""},
		{"description", `"VANGUARD TOTAL BOND MARKET ETF"`, `"VANGUARD TOTAL BOND MARKET ETF"`, ""},
		{"description", `87654321`, `87654321`, ""},
		{"description", `29470135`, `"...135"`, "AccountNumbers"},
		{"description", `0.29470135e8`, `"...135"`, "AccountNumbers"},

		// A tag: Schwab's platform's kept, a TA_ tag reduced to its origin,
		// any other withheld.
		{"tag", `"API_TOS:TRADE_ALL"`, `"API_TOS:TRADE_ALL"`, ""},
		{"tag", `"API_TOS:trade"`, `"[withheld]"`, "Tags"},
		{"tag", `"API_TOS:"`, `"[withheld]"`, "Tags"},
		{"tag", `"TA_someone at example dot com 1234"`, `"TA_..."`, "Tags"},
		{"tag", `"TA_FAKEAPP"`, `"TA_..."`, "Tags"},
		{"tag", `"TA_..."`, `"TA_..."`, ""},
		{"tag", `"TAX_X"`, `"[withheld]"`, "Tags"},
		{"tag", `"jdoe@example.com"`, `"[withheld]"`, "Tags"},
		{"tag", `"[withheld]"`, `"[withheld]"`, ""},
		{"tag", `1234`, `"[withheld]"`, "Tags"},
		{"tag", `null`, `null`, ""},

		// Withheld: text and numbers, never shown.
		{"withheld", `"Jane"`, `"[withheld]"`, "Withheld"},
		{"withheld", `12345678`, `null`, "Withheld"},
		{"withheld", `1.5`, `null`, "Withheld"},
		{"withheld", `2.9470135E7`, `null`, "Withheld"},
		{"withheld", `"[withheld]"`, `"[withheld]"`, ""},
		{"withheld", `true`, `true`, ""},
		{"withheld", `null`, `null`, ""},

		// A key no table knows: a time, a short code or a masked number
		// kept, any other text withheld; an integer of 8 or 9 digits, or a
		// known one, masked, any other number kept.
		{"newKey", `"journal to 87654321"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"Jane Doe"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"jdoe@example.com"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"NEW_STATE"`, `"NEW_STATE"`, ""},
		{"newKey", `"N9"`, `"N9"`, ""},
		{"newKey", `"ACCT12345"`, `"ACCT12345"`, ""},
		{"newKey", `"ACCT123456"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"A` + strings.Repeat("B", 40) + `"`, `"A` + strings.Repeat("B", 40) + `"`, ""},
		{"newKey", `"A` + strings.Repeat("B", 41) + `"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"new_state"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"1ABC"`, `"[withheld]"`, "Withheld"},
		{"newKey", `""`, `"[withheld]"`, "Withheld"},
		{"newKey", `"2024-06-06T13:46:03+0000"`, `"2024-06-06T13:46:03+0000"`, ""},
		{"newKey", `"2024-06-06T13:46:03.000Z"`, `"2024-06-06T13:46:03.000Z"`, ""},
		{"newKey", `"2024-06-06T09:46:03-04:00"`, `"2024-06-06T09:46:03-04:00"`, ""},
		{"newKey", `"2024-06-06"`, `"2024-06-06"`, ""},
		{"newKey", `"2024-06-28 00:00:00.0"`, `"2024-06-28 00:00:00.0"`, ""},
		{"newKey", `"2024-06-28 00:00:00"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"2024-06-06T13:46:03"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"2024-02-30"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"2024-06-06T13:46:03.1234567890Z"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"2024-06-06T13:46:03.123456789Z"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"...321"`, `"...321"`, ""},
		{"newKey", `"....321"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"...4321"`, `"[withheld]"`, "Withheld"},
		{"newKey", `"[withheld]"`, `"[withheld]"`, ""},
		{"newKey", `87654321`, `"...321"`, "UnknownIntegers"},
		{"newKey", `987654321`, `"...321"`, "UnknownIntegers"},
		{"newKey", `123456789`, `"...789"`, "UnknownIntegers"},
		{"newKey", `-87654321`, `"...321"`, "UnknownIntegers"},
		{"newKey", `1234567`, `"...567"`, "UnknownIntegers"},
		{"newKey", `7654321`, `7654321`, ""},
		{"newKey", `1004517688777`, `1004517688777`, ""},
		{"newKey", `1712246167000`, `1712246167000`, ""},
		{"newKey", `87654321.0`, `"...321"`, "UnknownIntegers"},
		{"newKey", `8.7654321E7`, `"...321"`, "UnknownIntegers"},
		{"newKey", `2.9470135e7`, `"...135"`, "UnknownIntegers"},
		{"newKey", `1.234567E6`, `"...567"`, "UnknownIntegers"},
		{"newKey", `87654321.5`, `87654321.5`, ""},
		{"newKey", `1.0E13`, `1.0E13`, ""},
		{"newKey", `1e999999`, `1e999999`, ""},
		{"newKey", `0`, `0`, ""},
		{"newKey", `false`, `false`, ""},
		{"newKey", `null`, `null`, ""},

		// Anything under a user object is withheld, whatever its key.
		{
			"user", `{"firstName":"Jane","lastName":"Doe","login":"jdoe1234","userId":12345678,"symbol":"AAPL","active":true}`,
			`{"firstName":"[withheld]","lastName":"[withheld]","login":"[withheld]","userId":null,"symbol":"[withheld]","active":true}`, "Withheld",
		},
		{"user", `"jdoe1234"`, `"[withheld]"`, "Withheld"},
	} {
		in := `{"` + tc.key + `":` + tc.in + `}`
		got, r, err := JSON([]byte(in), testClass, testKnown())
		if err != nil {
			t.Errorf("JSON(%s): %v", in, err)
			continue
		}
		if want := `{"` + tc.key + `":` + tc.want + `}`; string(got) != want {
			t.Errorf("JSON(%s) = %s, want %s", in, got, want)
		}
		counts := map[string]int{"AccountNumbers": r.AccountNumbers, "Tags": r.Tags, "Withheld": r.Withheld, "UnknownIntegers": r.UnknownIntegers}
		for name, n := range counts {
			if want := name == tc.counted; (n > 0) != want {
				t.Errorf("JSON(%s) counts %d %s", in, n, name)
			}
		}
		if (len(r.Paths) > 0) != (tc.counted != "") {
			t.Errorf("JSON(%s) reports the paths %q", in, r.Paths)
		}
		again, r, err := JSON(got, testClass, testKnown())
		if err != nil || !bytes.Equal(again, got) || !reflect.DeepEqual(r, Report{}) {
			t.Errorf("JSON(%s) = %s, and JSON of that = %s, %+v, %v", in, got, again, r, err)
		}
	}
}

// integerDigits takes a number for its value, however JSON writes it: the
// digits of an integer, as math/big reads it, written with a fraction of
// zeros or an exponent or neither; and no integer for a fraction, or for a
// number whose exponent adds zeros to it past 64 digits.
func TestIntegerDigits(t *testing.T) {
	for _, tc := range []struct{ n, want string }{ // want "" for no integer
		{"29470135", "29470135"},
		{"-29470135", "29470135"},
		{"29470135.0", "29470135"},
		{"29470135.000", "29470135"},
		{"2.9470135E7", "29470135"},
		{"2.9470135e+7", "29470135"},
		{"29470135e0", "29470135"},
		{"294701350e-1", "29470135"},
		{"2947013500E-2", "29470135"},
		{"0.29470135e8", "29470135"},
		{"0.0000029470135e13", "29470135"},
		{"-0.29470135E+8", "29470135"},
		{"2947013.5", ""},
		{"2.94701355E7", ""},
		{"29470135e-1", ""},
		{"1e-1", ""},
		{"0.5", ""},
		{"5e-9999999999", ""},
		{"0", "0"},
		{"-0", "0"},
		{"0.0", "0"},
		{"0e5", "0"},
		{"0E-5", "0"},
		{"1e63", "1" + strings.Repeat("0", 63)},
		{"1.5e63", "15" + strings.Repeat("0", 62)},
		{"1e64", ""},
		{"1e999999", ""},
		{strings.Repeat("9", 80), strings.Repeat("9", 80)},
		{strings.Repeat("9", 80) + ".0", strings.Repeat("9", 80)},
	} {
		got, ok := integerDigits(json.Number(tc.n))
		if ok != (tc.want != "") || got != tc.want {
			t.Errorf("integerDigits(%s) = %q, %v; want %q", tc.n, got, ok, tc.want)
		}
	}

	// Numbers written every way that JSON writes one, read by math/big.
	r := rand.New(rand.NewPCG(1, 2))
	digits := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(byte('0' + r.IntN(10)))
		}
		return b.String()
	}
	for range 5000 {
		var n strings.Builder
		if r.IntN(2) == 0 {
			n.WriteString("-")
		}
		if r.IntN(4) == 0 {
			n.WriteString("0")
		} else {
			n.WriteString(strconv.Itoa(1+r.IntN(9)) + digits(r.IntN(12)))
		}
		if r.IntN(2) == 0 {
			n.WriteString("." + strings.Repeat("0", r.IntN(3)) + digits(1+r.IntN(6)) + strings.Repeat("0", r.IntN(3)))
		}
		if r.IntN(2) == 0 {
			n.WriteString([]string{"e", "E", "e+", "E-", "e-"}[r.IntN(5)] + strconv.Itoa(r.IntN(30)))
		}
		lit := n.String()
		if !json.Valid([]byte(lit)) {
			t.Fatalf("%s is no JSON number", lit)
		}
		v, ok := new(big.Rat).SetString(lit)
		if !ok {
			t.Fatalf("math/big does not read %s", lit)
		}
		want := ""
		if v.IsInt() {
			want = new(big.Int).Abs(v.Num()).String()
		}
		if got, ok := integerDigits(json.Number(lit)); ok != v.IsInt() || got != want {
			t.Fatalf("integerDigits(%s) = %q, %v; want %q, %v", lit, got, ok, want, v.IsInt())
		}
	}
}

// JSON keeps the order of keys, duplicates included, and of elements, and
// writes compactly what came indented; it masks a known number in a key;
// it reads a path from the root through arrays without their indexes,
// counting each path once; and it fails on anything but one JSON value,
// saying nothing of it.
func TestMaskJSONShape(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"{\n  \"b\": 1,\n  \"a\": [1, 2, {\"c\": null}],\n  \"b\": 2\n}\n", `{"b":1,"a":[1,2,{"c":null}],"b":2}`},
		{` [ ] `, `[]`},
		{`{}`, `{}`},
		{`[[],{},[{}]]`, `[[],{},[{}]]`},
		{`{"29470135":"x","...135":1}`, `{"...135":"[withheld]","...135":1}`},
		{`"29470135"`, `"[withheld]"`},
		{`12345678`, `"...678"`},
		{`true`, `true`},
		{
			`[{"accountNumber":"12345678"},{"accountNumber":"61803398","transferItems":[{"accountNumber":1}]}]`,
			`[{"accountNumber":"...678"},{"accountNumber":"...398","transferItems":[{"accountNumber":"..."}]}]`,
		},
		{`{"a":{"accountNumber":"12345678"}}`, `{"a":{"accountNumber":"...678"}}`},
		{`{"description":"\u2028 \ud83d\ude00 \u0000"}`, `{"description":"\u2028 😀 \u0000"}`},
		{"{\"description\":\"a\xffb\"}", `{"description":"a` + "\uFFFD" + `b"}`},
	} {
		got, _, err := JSON([]byte(tc.in), testClass, testKnown())
		if err != nil || string(got) != tc.want {
			t.Errorf("JSON(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}

	_, r, err := JSON([]byte(`[{"accountNumber":"12345678","29470135":1},{"accountNumber":"61803398","tag":"TA_X"}]`), testClass, testKnown())
	if want := (Report{AccountNumbers: 3, Tags: 1, Paths: []string{"accountNumber", "...135", "tag"}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("the report = %+v, %v; want %+v", r, err, want)
	}
	// A value or a key that holds two known numbers counts once.
	_, r, err = JSON([]byte(`{"symbol":"29470135 and 1234567","x":{"29470135 1234567":1}}`), testClass, testKnown())
	if want := (Report{AccountNumbers: 2, Paths: []string{"symbol", "x....135 ...567"}}); err != nil || !reflect.DeepEqual(r, want) {
		t.Errorf("the report of two numbers in a value and in a key = %+v, %v; want %+v", r, err, want)
	}

	// The path reaches classOf from the root, through arrays, and is the
	// caller's to keep.
	var paths []string
	var kept [][]string
	classOf := func(path []string) Class {
		paths = append(paths, strings.Join(path, "."))
		kept = append(kept, path)
		return Hash
	}
	if _, _, err := JSON([]byte(`[{"a":[{"b":1,"c":[2]}],"d":3},4]`), classOf, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a.b", "a.c", "d", ""}; !slices.Equal(paths, want) {
		t.Errorf("classOf was given %q, want %q", paths, want)
	}
	if got := strings.Join(kept[0], "."); got != "a.b" {
		t.Errorf("the first path that classOf kept now reads %q", got)
	}

	for _, in := range []string{``, ` `, `{"a":`, `{"a":1,}`, `[1,]`, `{} {}`, `{"a" 1}`, `[12345678`, `{12345678:1}`, `"29470135`} {
		got, r, err := JSON([]byte(in), testClass, testKnown())
		if !errors.Is(err, errNotJSON) || got != nil || !reflect.DeepEqual(r, Report{}) {
			t.Errorf("JSON(%q) = %q, %+v, %v; want errNotJSON", in, got, r, err)
		}
		if strings.ContainsAny(err.Error(), "0123456789") {
			t.Errorf("JSON(%q)'s error says %q", in, err)
		}
	}
}

// fuzzKeys are a key of each class of the tests' table, under which
// planted puts the value it is given.
var fuzzKeys = []string{"newKey", "accountNumber", "hashValue", "activityId", "time", "netAmount", "description", "tag", "withheld"}

// planted returns a document that holds v, a JSON value, under a key of
// every class and in a user object, beside known numbers planted where each
// class holds them.
func planted(v string) string {
	var b strings.Builder
	b.WriteString("{")
	for _, key := range fuzzKeys {
		fmt.Fprintf(&b, "%q:%s,", key, v)
	}
	fmt.Fprintf(&b, `"user":{"firstName":%s,"login":"jdoe1234","userId":12345678},`, v)
	fmt.Fprintf(&b, `"transferItems":[{"instrument":{"description":%s,"symbol":"ACCT29470135"},"user":[%s,1234567]}],`, v, v)
	b.WriteString(`"planted":[29470135,1234567,"JOURNAL TO 1234567",{"description":"to 123456789","orderId":29470135}]}`)
	return b.String()
}

// A pathToken is a token of a JSON document, with the keys that lead to it;
// a key is a token too.
type pathToken struct {
	tok  json.Token
	path []string
	key  bool
}

// tokensOf returns the tokens of doc, valid JSON.
func tokensOf(t *testing.T, doc []byte) []pathToken {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.UseNumber()
	type open struct {
		object bool
		key    bool
	}
	var stack []open
	var path []string
	var toks []pathToken
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return toks
		}
		if err != nil {
			t.Fatalf("reading %s: %v", doc, err)
		}
		top := len(stack) - 1
		switch d, _ := tok.(json.Delim); {
		case d == '}' || d == ']':
			if stack[top].object {
				path = path[:len(path)-1]
			}
			stack = stack[:top]
			toks = append(toks, pathToken{tok: tok, path: slices.Clone(path)})
		case top >= 0 && stack[top].object && stack[top].key:
			stack[top].key = false
			path[len(path)-1] = tok.(string)
			toks = append(toks, pathToken{tok: tok, path: slices.Clone(path), key: true})
			continue
		case d != 0:
			toks = append(toks, pathToken{tok: tok, path: slices.Clone(path)})
			stack = append(stack, open{object: d == '{', key: d == '{'})
			if d == '{' {
				path = append(path, "")
			}
			continue
		default:
			toks = append(toks, pathToken{tok: tok, path: slices.Clone(path)})
		}
		if top := len(stack) - 1; top >= 0 && stack[top].object {
			stack[top].key = true
		}
	}
}

// checkMasked checks what JSON made of doc, a document, as FuzzMaskJSON
// says.
func checkMasked(t *testing.T, doc string) {
	t.Helper()
	k := testKnown()
	out, _, err := JSON([]byte(doc), testClass, k)
	if !json.Valid([]byte(doc)) {
		if err == nil {
			t.Fatalf("JSON(%q) = %s, though it is not JSON", doc, out)
		}
		return
	}
	if err != nil {
		t.Fatalf("JSON(%q): %v", doc, err)
	}
	if !json.Valid(out) {
		t.Fatalf("JSON(%q) = %q, which does not parse", doc, out)
	}
	in, got := tokensOf(t, []byte(doc)), tokensOf(t, out)
	if len(in) != len(got) {
		t.Fatalf("JSON(%q) = %s, of %d tokens, not %d", doc, out, len(got), len(in))
	}
	isKnownInteger := func(tok json.Token) bool {
		n, ok := tok.(json.Number)
		digits, integer := integerDigits(n)
		return ok && integer && k.has(digits)
	}
	for i, o := range got {
		x := in[i]
		class := testClass(o.path)
		s, isString := o.tok.(string)
		n, isNumber := o.tok.(json.Number)
		where := fmt.Sprintf("JSON(%q) = %s, at %q", doc, out, strings.Join(o.path, "."))
		switch {
		case x.key != o.key:
			t.Fatalf("%s: a key where none was, or none where one was", where)
		case x.key && s != k.Text(x.tok.(string)):
			t.Fatalf("%s: the key %q, from %q", where, s, x.tok)
		case isString && k.Contains(s) || isKnownInteger(o.tok):
			t.Fatalf("%s: a known number, %q", where, o.tok)
		case x.key:
		case reflect.TypeOf(x.tok) == reflect.TypeFor[json.Delim]() && x.tok != o.tok:
			t.Fatalf("%s: %v in place of %v", where, o.tok, x.tok)
		}
		if x.key {
			continue
		}
		_, wasString := x.tok.(string)
		switch class {
		case Hash, Identifier, Time, Amount, FreeText:
			// Their numbers, booleans and nulls are kept as they came, but
			// a known number, masked.
			if !wasString && !isKnownInteger(x.tok) && o.tok != x.tok {
				t.Fatalf("%s: %v in place of %v", where, o.tok, x.tok)
			}
		case Withheld:
			if isString && s != withheld || isNumber {
				t.Fatalf("%s: %v under Withheld", where, o.tok)
			}
		case AccountNumber:
			if isNumber || isString && (!strings.HasPrefix(s, "...") || utf8.RuneCountInString(s) > 6) {
				t.Fatalf("%s: %v under AccountNumber", where, o.tok)
			}
		case Tag:
			if isNumber || isString && s != withheld && s != "TA_..." && !apiTOSTag.MatchString(s) {
				t.Fatalf("%s: %v under Tag", where, o.tok)
			}
		default:
			if digits, integer := integerDigits(n); isNumber && integer && (len(digits) == 8 || len(digits) == 9) {
				t.Fatalf("%s: an integer of %d digits under an unknown key", where, len(digits))
			}
			if isString && s != withheld && !isTimeForm(s) && !isShortCode(s) && !maskedNumber.MatchString(s) {
				t.Fatalf("%s: %q under an unknown key", where, s)
			}
		}
	}
	again, r, err := JSON(out, testClass, k)
	if err != nil || !bytes.Equal(again, out) || !reflect.DeepEqual(r, Report{}) {
		t.Fatalf("JSON(%q) = %s, and JSON of that = %s, %+v, %v", doc, out, again, r, err)
	}
}

// What JSON writes parses, has the tokens of what it read, keys masked by
// known number, and holds no known number; it keeps the numbers, booleans
// and nulls of the classes that keep them as they came, but known numbers;
// under Withheld it holds no text and no number, under AccountNumber and
// Tag only their masks, and under an unknown key no text but times, codes
// and masks and no integer of 8 or 9 digits; and masking it again changes
// nothing and reports nothing. A number is an integer by its value, however
// it is written. Each document is checked as it is and planted under a key
// of every class and in a user object, beside known numbers. The seeds hold
// what a second pass of the rules as first written changed: the mask of an
// 8- or 9-digit integer and of a known 7-digit one under an unknown key, and
// that of an account number under accountNumber; and integers written with
// a fraction or an exponent, which those rules kept.
func FuzzMaskJSON(f *testing.F) {
	for _, s := range []string{
		`{"newNumber":87654321}`, `{"newNumber":987654321}`, `{"newNumber":1234567}`, `{"newNumber":123456789}`,
		`{"orderId":29470135.0}`, `{"newKey":2.9470135E7}`, `{"newKey":8.7654321E7}`, `{"accountNumber":1.2345678e7}`,
		`{"accountNumber":"12345678"}`, `{"accountNumber":12345678}`, `{"accountNumber":"...678"}`,
		`{"newKey":"...321"}`, `{"newKey":"[withheld]"}`,
		prototypeIn, prototypeOut,
		`{"user":{"firstName":"Jane","lastName":"Doe","login":"jdoe1234","userId":12345678}}`,
		`[{"activityId":73508261946,"time":"2024-04-04T15:56:07+0000","accountNumber":"29470135","description":"JOURNAL TO 61803398",` +
			`"netAmount":-2.7,"transferItems":[{"instrument":{"symbol":"BND","description":"VANGUARD TOTAL BOND MARKET ETF"},"amount":0.0378}]}]`,
		`{"hashValue":"` + hash + `","tag":"TA_FAKEAPP","x":true,"y":null,"z":[1,"a",{"q":1.5e3}],"29470135":"1234567"}`,
		`"29470135"`, `12345678`, `[]`, `{}`, `null`, `{"a":`, `{"description":"a\u0026b \u2028 \ud800"}`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		checkMasked(t, doc)
		if json.Valid([]byte(doc)) {
			checkMasked(t, planted(doc))
		}
	})
}

// An order's tag shows only where the order came from: Schwab's own
// platform's tag as it is, an app's TA_ tag cut to TA_..., anything else
// withheld, and no tag as none.
func TestTagOrigin(t *testing.T) {
	for tag, want := range map[string]string{
		"API_TOS:TRADE_ALL":   "API_TOS:TRADE_ALL",
		"API_TOS:POS_STMT":    "API_TOS:POS_STMT",
		"TA_gmailcom12345678": "TA_...",
		"TA_":                 "TA_...",
		"API_TOS:trade":       "[withheld]",
		"MyApp 61803398":      "[withheld]",
		"api_tos:TRADE_ALL":   "[withheld]",
		" API_TOS:TRADE_ALL":  "[withheld]",
		"":                    "",
	} {
		if got := TagOrigin(tag); got != want {
			t.Errorf("TagOrigin(%q) = %q, want %q", tag, got, want)
		}
	}
}
