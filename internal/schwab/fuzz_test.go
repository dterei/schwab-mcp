package schwab

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fuzzRecord fuzzes the decoding of a record of type T from any bytes,
// starting from seeds, from the seeds that every record target has, and from
// a record of each shape of type T in the fixtures. It checks these
// properties, where a record's problems are its own and those of each record
// nested in it that decodes itself, an account's positions, at their paths
// within it:
//
//  1. Invalid JSON fails json.Unmarshal, and a direct UnmarshalJSON call
//     returns nil and gives a problem. Only this one is checked on invalid
//     JSON, where a problem can hold encoding/json's text by design.
//  2. Valid JSON never fails json.Unmarshal.
//  3. Raw is the bytes, without the space around them, and a direct
//     UnmarshalJSON call, which gets the space too, gives the same record.
//  4. Two decodes are the same, which checks the walk's cache.
//  5. No record has a problem twice, and every problem's path is one of the
//     record's field paths, or "".
//  6. No problem holds encoding/json's own text, which reads differently on
//     each Go version.
//  7. Each record of a list of two copies is the record decoded alone.
//  8. A field that takes a number, string or boolean holds what
//     encoding/json makes of the last copy sent alone, or, where
//     encoding/json refuses it, zero with a problem at its path; sent once,
//     it has no problem, but that it is missing where it is zero. A time
//     holds what readTime reads, with its problem. An object or array sent
//     once is not missing, and is checked in the same way, at any depth.
//  9. A record without problems survives its own encoding: it decodes again
//     without problems and the same but for every Raw, and the encoding
//     holds only keys that Schwab sent.
func fuzzRecord[T any, PT interface {
	*T
	json.Unmarshaler
}](f *testing.F, seeds ...string,
) {
	for _, raw := range fixtureRecords(f)[reflect.TypeFor[T]()] {
		f.Add([]byte(raw))
	}
	for _, s := range slices.Concat(seeds, everyRecordSeeds) {
		f.Add([]byte(s))
	}
	paths := recordPaths(reflect.TypeFor[T]())
	f.Fuzz(func(t *testing.T, b []byte) {
		checkRecord[T, PT](t, b, paths)
	})
}

// everyRecordSeeds are seeds of every record target: invalid JSON, for
// property 1 and for decode's fallback, "the JSON could not be read", which
// nothing else reaches; and JSON that is not an object.
var everyRecordSeeds = []string{
	`{"activityId":1,`, `{"activityId":1}}`, `{"activityId":1 "time":2}`,
	`null`, `5`, `"x"`, `[]`, `{}`,
}

// checkRecord checks the properties that fuzzRecord lists on a record of
// type T decoded from b, where paths are the record's field paths.
func checkRecord[T any, PT interface {
	*T
	json.Unmarshaler
}](t *testing.T, b []byte, paths map[string]bool,
) {
	t.Helper()
	var rec T
	v := reflect.ValueOf(&rec).Elem()
	if !json.Valid(b) {
		if err := json.Unmarshal(b, PT(&rec)); err == nil {
			t.Fatalf("json.Unmarshal took invalid JSON %q", b)
		}
		if err := PT(&rec).UnmarshalJSON(b); err != nil || len(gatheredProblems(v)) == 0 {
			t.Fatalf("UnmarshalJSON(%q) = %v, with problems %q; want nil and a problem", b, err, gatheredProblems(v))
		}
		return
	}
	if err := json.Unmarshal(b, PT(&rec)); err != nil {
		t.Fatalf("json.Unmarshal(%q) failed: %v", b, err)
	}
	if raw := v.FieldByName("Raw").Bytes(); !bytes.Equal(raw, bytes.TrimSpace(b)) {
		t.Fatalf("Raw is %q, want %q", raw, bytes.TrimSpace(b))
	}
	var direct T
	if err := PT(&direct).UnmarshalJSON(b); err != nil || !reflect.DeepEqual(direct, rec) {
		t.Fatalf("UnmarshalJSON(%q) = %v, and\n got %+v\nwant %+v", b, err, direct, rec)
	}
	var again T
	if err := json.Unmarshal(b, PT(&again)); err != nil || !reflect.DeepEqual(again, rec) {
		t.Fatalf("a second decode of %q differs: %v\n got %+v\nwant %+v", b, err, again, rec)
	}

	recordsIn(v, "", func(path string, own []Problem) {
		for i, p := range own {
			if slices.Contains(own[:i], p) {
				t.Fatalf("the record at %q has the problem %q twice: %q", path, p, own)
			}
		}
	})
	ps := gatheredProblems(v)
	for _, p := range ps {
		if !paths[p.Path] {
			t.Fatalf("%q has a problem at %q, which is no field's path: %q", b, p.Path, p)
		}
		if strings.HasPrefix(p.What, "the JSON could not be read") || strings.HasPrefix(p.What, "Schwab's value could not be read") {
			t.Fatalf("%q has a problem with encoding/json's text, which differs between Go versions: %q", b, p)
		}
	}

	list, err := decodeList[T]([]byte("[" + string(b) + " , " + string(b) + "]"))
	if err != nil || len(list) != 2 || !reflect.DeepEqual(list[0], rec) || !reflect.DeepEqual(list[1], rec) {
		t.Fatalf("in a list of two, %q decodes as %+v, %v; want %+v twice", b, list, err, rec)
	}

	if tb := bytes.TrimSpace(b); tb[0] == '{' {
		agreesWithJSON(t, tb, v, "", ps, false)
	}

	if len(ps) > 0 {
		return
	}
	enc, err := json.Marshal(PT(&rec))
	if err != nil {
		t.Fatalf("%q, without problems, does not encode: %v", b, err)
	}
	var back T
	if err := json.Unmarshal(enc, PT(&back)); err != nil {
		t.Fatal(err)
	}
	bv := reflect.ValueOf(&back).Elem()
	if bps := gatheredProblems(bv); len(bps) > 0 {
		t.Fatalf("%q encodes as %s, which decodes with problems %q", b, enc, bps)
	}
	clearFields(v, "Raw")
	clearFields(bv, "Raw")
	if !reflect.DeepEqual(back, rec) {
		t.Fatalf("%q changed on the way through its encoding, %s:\n got %+v\nwant %+v", b, enc, back, rec)
	}
	var encoded, sent any
	if json.Unmarshal(enc, &encoded) != nil || json.Unmarshal(b, &sent) != nil {
		t.Fatalf("%q or its encoding %s does not decode as JSON", b, enc)
	}
	if key := keyNotSent(encoded, sent, ""); key != "" {
		t.Fatalf("the encoding of %q, %s, has %s, which Schwab did not send", b, enc, key)
	}
}

// recordPaths returns the path of every field that a record of type t holds,
// at any depth, and "", the record's own: every path that one of its
// problems may have. problems is the package's field, not Schwab's.
func recordPaths(t reflect.Type) map[string]bool {
	paths := map[string]bool{"": true}
	var add func(t reflect.Type, path string)
	add = func(t reflect.Type, path string) {
		t = derefType(t)
		if t.Kind() == reflect.Slice {
			add(t.Elem(), path)
			return
		}
		if t.Kind() != reflect.Struct || t == timeType {
			return
		}
		for f := range t.Fields() {
			if name := jsonName(f); name != "" && name != "problems" {
				paths[joinPath(path, name)] = true
				add(f.Type, joinPath(path, name))
			}
		}
	}
	add(t, "")
	return paths
}

// recordsIn calls fn with the problems of v, a record, and then of each
// record nested in it that decodes itself and has problems of its own, such
// as an account's positions, with the path of each within v.
func recordsIn(v reflect.Value, path string, fn func(path string, ps []Problem)) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			recordsIn(v.Elem(), path, fn)
		}
	case reflect.Slice:
		if v.Type() != rawType {
			for i := range v.Len() {
				recordsIn(v.Index(i), path, fn)
			}
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}
		if ps := v.FieldByName("Problems"); ps.IsValid() && ps.Type() == reflect.TypeFor[[]Problem]() {
			fn(path, ps.Interface().([]Problem))
		}
		for f := range v.Type().Fields() {
			if name := jsonName(f); name != "" && name != "problems" {
				recordsIn(v.FieldByIndex(f.Index), joinPath(path, name), fn)
			}
		}
	}
}

// gatheredProblems returns the problems of v, a record, and of each record
// nested in it, at their paths within v, each once, as a transaction gives
// the problems of its legs' instruments.
func gatheredProblems(v reflect.Value) []Problem {
	var all []Problem
	recordsIn(v, "", func(path string, ps []Problem) { all = within(all, path, ps) })
	return all
}

// clearFields sets every field of v, a record, with one of the names given,
// such as Raw, to its zero value, at any depth.
func clearFields(v reflect.Value, names ...string) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			clearFields(v.Elem(), names...)
		}
	case reflect.Slice:
		if v.Type() != rawType {
			for i := range v.Len() {
				clearFields(v.Index(i), names...)
			}
		}
	case reflect.Struct:
		if v.Type() == timeType {
			return
		}
		for f := range v.Type().Fields() {
			switch {
			case !f.IsExported():
			case slices.Contains(names, f.Name):
				v.FieldByIndex(f.Index).SetZero()
			default:
				clearFields(v.FieldByIndex(f.Index), names...)
			}
		}
	}
}

// jsonMembers returns the keys and values of the members of raw, a JSON
// object, in order, as encoding/json reads them.
func jsonMembers(raw []byte) (keys []string, values []json.RawMessage, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil, false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, nil, false
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, nil, false
		}
		keys, values = append(keys, key.(string)), append(values, value)
	}
	return keys, values, true
}

// agreesWithJSON checks property 8 of fuzzRecord on raw, a JSON object, and
// v, the struct decoded from it, at path: each field, and the members that
// go to it, keys matched as encoding/json matches them. ps are the record's
// problems, gathered. In a list, where another element can have a problem
// at the same path, only the problems that a member must have are looked
// for. A key missing can be a problem of a field that encoding/json reads
// as zero, such as an activityId of 0.
func agreesWithJSON(t *testing.T, raw []byte, v reflect.Value, path string, ps []Problem, inList bool) {
	t.Helper()
	keys, values, ok := jsonMembers(raw)
	if !ok {
		t.Fatalf("%s is not an object", raw)
	}
	for f := range v.Type().Fields() {
		name := jsonName(f)
		if name == "" || name == "problems" {
			continue
		}
		var sent []int
		for i, k := range keys {
			if strings.EqualFold(k, name) {
				sent = append(sent, i)
			}
		}
		if len(sent) == 0 {
			continue
		}
		// The last copy counts. Of an object or array sent twice, what the
		// field holds is uncertain, and of a number, string or boolean, an
		// earlier copy of the wrong kind is a problem too.
		value := bytes.TrimSpace(values[sent[len(sent)-1]])
		once := len(sent) == 1
		at := joinPath(path, name)
		fv := v.FieldByIndex(f.Index)
		missing := Problem{Path: at, What: "missing, though Schwab sends it on every record of its kind"}
		switch ft := derefType(f.Type); {
		case f.Type == timeType:
			var tps []Problem
			want, _ := readTime(value, at, &tps)
			got := fv.Interface().(time.Time)
			if !got.Equal(want) || len(tps) > 0 && !slices.Contains(ps, tps[0]) ||
				len(tps) == 0 && once && !inList && string(value) != "null" && hasProblemAt(ps, at) {
				t.Fatalf("%s = %v, with problems %q; readTime reads %s as %v, %q", at, got, ps, value, want, tps)
			}
		case ft.Kind() == reflect.Struct:
			if !once || value[0] != '{' {
				continue
			}
			if !inList && slices.Contains(ps, missing) {
				t.Fatalf("%s is missing, with problems %q, though Schwab sent %s", at, ps, value)
			}
			if fv.Kind() == reflect.Pointer {
				if fv.IsNil() {
					t.Fatalf("%s is nil, though Schwab sent %s", at, value)
				}
				fv = fv.Elem()
			}
			agreesWithJSON(t, value, fv, at, ps, inList)
		case ft.Kind() == reflect.Slice:
			if !once || value[0] != '[' {
				continue
			}
			if !inList && slices.Contains(ps, missing) {
				t.Fatalf("%s is missing, with problems %q, though Schwab sent %s", at, ps, value)
			}
			var elems []json.RawMessage
			if err := json.Unmarshal(value, &elems); err != nil || fv.Len() != len(elems) {
				t.Fatalf("%s holds %d elements, where Schwab sent %s", at, fv.Len(), value)
			}
			for i, e := range elems {
				if e = bytes.TrimSpace(e); e[0] == '{' && derefType(ft.Elem()).Kind() == reflect.Struct {
					agreesWithJSON(t, e, fv.Index(i), at, ps, true)
				}
			}
		default:
			want := reflect.New(f.Type)
			err := json.Unmarshal(value, want.Interface())
			switch {
			case string(value) == "null":
				if !fv.IsZero() {
					t.Fatalf("%s = %v, though Schwab sent null", at, fv)
				}
			case err != nil:
				if !fv.IsZero() || !hasProblemAt(ps, at) {
					t.Fatalf("%s = %v, with problems %q, where encoding/json refuses %s: %v", at, fv, ps, value, err)
				}
			case !reflect.DeepEqual(fv.Interface(), want.Elem().Interface()):
				t.Fatalf("%s = %#v, where encoding/json reads %s as %#v", at, fv.Interface(), value, want.Elem().Interface())
			case once && !inList && slices.ContainsFunc(ps, func(p Problem) bool { return p.Path == at && (p != missing || !fv.IsZero()) }):
				t.Fatalf("%s = %#v, with problems %q, though encoding/json reads %s", at, fv.Interface(), ps, value)
			}
		}
	}
}

// hasProblemAt reports whether ps holds a problem at path.
func hasProblemAt(ps []Problem, path string) bool {
	return slices.ContainsFunc(ps, func(p Problem) bool { return p.Path == path })
}

// keyNotSent returns the path of a key in enc that sent lacks, where enc and
// sent are JSON decoded as any, keys matched as encoding/json matches them
// and arrays element by element, or "" for none.
func keyNotSent(enc, sent any, path string) string {
	switch e := enc.(type) {
	case map[string]any:
		s, _ := sent.(map[string]any)
		for k, x := range e {
			var match any
			found := false
			for sk, sx := range s {
				if strings.EqualFold(sk, k) {
					match, found = sx, true
				}
			}
			if !found {
				return joinPath(path, k)
			}
			if key := keyNotSent(x, match, joinPath(path, k)); key != "" {
				return key
			}
		}
	case []any:
		s, _ := sent.([]any)
		for i, x := range e {
			if i < len(s) {
				if key := keyNotSent(x, s[i], path); key != "" {
					return key
				}
			}
		}
	}
	return ""
}

// fixtureRecords returns the fixtures' records by type, the positions of the
// accounts among them, one of each shape, for the fuzz targets' seeds. It
// reads the files that testdataRecords reads, which needs a *testing.T where
// a fuzz target has a *testing.F; TestFuzzSeedsHoldEveryFixtureShape keeps
// the two in step.
func fixtureRecords(tb testing.TB) map[reflect.Type][]json.RawMessage {
	tb.Helper()
	list := func(name string) []json.RawMessage {
		var raws []json.RawMessage
		if err := json.Unmarshal(readTestdata(tb, name), &raws); err != nil {
			tb.Fatalf("%s: %v", name, err)
		}
		return raws
	}
	accounts := list("accounts.json")
	var positions []json.RawMessage
	for _, account := range accounts {
		var a struct {
			SecuritiesAccount struct {
				Positions []json.RawMessage `json:"positions"`
			} `json:"securitiesAccount"`
		}
		if err := json.Unmarshal(account, &a); err != nil {
			tb.Fatal(err)
		}
		positions = append(positions, a.SecuritiesAccount.Positions...)
	}
	records := map[reflect.Type][]json.RawMessage{
		reflect.TypeFor[AccountNumber]():  list("account_numbers.json"),
		reflect.TypeFor[Account]():        accounts,
		reflect.TypeFor[Position]():       positions,
		reflect.TypeFor[Transaction]():    append(list("transactions.json"), readTestdata(tb, "transaction.json")),
		reflect.TypeFor[UserPreference](): {readTestdata(tb, "user_preference.json")},
	}
	for typ, raws := range records {
		seen := map[string]bool{}
		var kept []json.RawMessage
		for _, raw := range raws {
			if shape := recordShape(raw); !seen[shape] {
				seen[shape] = true
				kept = append(kept, raw)
			}
		}
		records[typ] = kept
	}
	return records
}

// recordShape returns the shape of raw, a record: the paths of its keys at
// every depth, in lower case, each once, and which of them an object sends
// twice, as transaction.json sends an instrument's assetType.
func recordShape(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var paths []string
	var walk func(path string) error
	walk = func(path string) error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		switch tok {
		case json.Delim('{'):
			keys := map[string]bool{}
			for dec.More() {
				k, err := dec.Token()
				if err != nil {
					return err
				}
				key := strings.ToLower(k.(string))
				at := joinPath(path, key)
				paths = append(paths, at)
				if keys[key] {
					paths = append(paths, at+" twice")
				}
				keys[key] = true
				if err := walk(at); err != nil {
					return err
				}
			}
		case json.Delim('['):
			for dec.More() {
				if err := walk(path); err != nil {
					return err
				}
			}
		default:
			return nil
		}
		_, err = dec.Token() // the end of the object or array
		return err
	}
	if walk("") != nil {
		return "not JSON"
	}
	slices.Sort(paths)
	return strings.Join(slices.Compact(paths), " ")
}

// Every shape of record that testdataRecords gives is a seed of its type's
// fuzz target, so that CI, which runs only the seeds, checks the fuzz
// properties on each. A record of a new shape in a file that only
// testdataRecords reads, such as a recording, fails here until
// fixtureRecords reads that file too.
func TestFuzzSeedsHoldEveryFixtureShape(t *testing.T) {
	seeds := fixtureRecords(t)
	for typ, records := range testdataRecords(t) {
		shapes := map[string]bool{}
		for _, raw := range seeds[typ] {
			shapes[recordShape(raw)] = true
		}
		for i, raw := range records {
			if !shapes[recordShape(raw)] {
				t.Errorf("%s %d of testdataRecords has a shape that no seed of the fuzz target for %s has", typ.Name(), i, typ.Name())
			}
		}
	}
}

// A transaction decoded from any bytes has the properties fuzzRecord lists.
// The seeds hold an option's leg with a deliverable, the bodies of the
// lenient-decoding tests, and fixtures' records such as transaction.json,
// which sends an instrument's assetType twice.
func FuzzTransaction(f *testing.F) {
	option := `{"assetType":"OPTION","symbol":"X","expirationDate":"2025-03-21T04:00:00+0000",` +
		`"optionDeliverables":[{"deliverableUnits":100,"deliverable":{"assetType":"EQUITY","symbol":"A"}}]}`
	fuzzRecord[Transaction](f,
		baseTransaction,
		withKeys(baseTransaction, "transferItems", "["+baseLeg+","+withKeys(baseLeg, "instrument", option)+"]"),
		withKeys(baseTransaction, "transferItems", "["+withKeys(baseLeg, "instrument", `{"assetType":"OPTION","symbol":"X","optionDeliverables":[]}`)+"]"),
		withKeys(baseTransaction, "qualifiedDividend", "false", "orderId", "12345678901234", "settlementDate", `"2024-04-05"`),
		withKeys(baseTransaction, "time", `"0001-01-01T00:00:00Z"`, "settlementDate", `"0001-01-01T00:00:00Z"`),
		// TestDecodeLeniently's.
		withKeys(baseTransaction, "netAmount", `"12.5"`, "description", `"kept"`),
		withKeys(baseTransaction, "activityId", "1.0"),
		`{"description":"all there is"}`,
		withKeys(baseTransaction, "activityId", "null", "time", "null", "type", "null", "netAmount", "null", "transferItems", "null", "settlementDate", "null"),
		withKeys(baseTransaction, "activityId", "0", "type", `""`, "status", `""`),
		withKeys(baseTransaction, "time", `"2024-13-45"`),
		withKeys(baseTransaction, "time", "1712246167"),
		withKeys(baseTransaction, "transferItems", "["+
			withKeys(baseLeg, "instrument", `{"assetType":"OPTION","symbol":"X","expirationDate":"soon"}`, "amount", "2")+","+
			withKeys(baseLeg, "instrument", `{"assetType":"CURRENCY","symbol":"CURRENCY_USD","closingPrice":"x"}`, "amount", "3")+","+
			withKeys(baseLeg, "instrument", `{"assetType":"CURRENCY","symbol":"CURRENCY_USD","closingPrice":"y"}`, "cost", "")+"]"),
		withKeys(baseTransaction, "transferItems", "{}"),
		withKeys(baseTransaction, "netAmount", "1e400"),
		withKeys(baseTransaction, "activityId", "", "time", "", "ActivityId", "7", "Time", `"2024-04-04T15:56:07Z"`, "futureField", `{"a":[1]}`),
		// TestEveryMismatchIsNamed's.
		`{"TYPE":5,`+withKeys(baseTransaction, "subAccount", "", "SUBACCOUNT", "7", "qualifiedDividend", `"true"`,
			"transferItems", `[{"amount":"1","cost":true,"Price":{},"instrument":{"assetType":"EQUITY","symbol":"X","strikePrice":"9"}}]`)[1:],
		withKeys(baseTransaction, "transferItems", "["+baseLeg+",5]"),
		// TestDuplicateKeys's.
		`{"transferItems":[`+baseLeg+","+baseLeg+"],"+
			withKeys(baseTransaction, "transferItems", `[{"instrument":{"assetType":"EQUITY","symbol":"Y"},`+baseLeg[1:]+"]")[1:],
		`{"subAccount":5,"subAccount":true,`+baseTransaction[1:],
		`{"subAccount":null,`+baseTransaction[1:],
		`{"transferItems":5,`+withKeys(baseTransaction, "transferItems", "null", "type", `""`)[1:],
		withKeys(baseTransaction, "transferItems", "["+withKeys(baseLeg, "instrument", `{"assetType":"OPTION","symbol":"X",`+
			`"optionDeliverables":[{"deliverableUnits":100,"deliverable":{"assetType":"EQUITY","symbol":"A"},"deliverable":{"assetType":"EQUITY","symbol":"B"}}]}`)+"]"),
		// TestNullElements's.
		withKeys(baseTransaction, "transferItems", "["+baseLeg+",null,"+
			withKeys(baseLeg, "instrument", `{"assetType":"OPTION","symbol":"X","optionDeliverables":[null]}`, "cost", `"x"`)+"]"),
		// Keys escaped, which the walk unescapes to match them.
		strings.Replace(baseTransaction, `"activityId"`, `"\u0061ctivityId"`, 1),
		strings.Replace(baseTransaction, `"type"`, `"\u0074ype"`, 1),
		// A time that its offset takes past the year 9999, which no record
		// could encode.
		withKeys(baseTransaction, "time", `"9999-12-31T23:59:59-2359"`),
		// White space around, which Raw leaves out.
		" \t"+baseTransaction+"\r\n",
		// Schwab's own problems key, which no field takes.
		withKeys(baseTransaction, "problems", "5"),
		withKeys(baseTransaction, "problems", "[{}]"),
	)
}

// An account decoded from any bytes, with its positions, has the properties
// fuzzRecord lists.
func FuzzAccount(f *testing.F) {
	account := func(securities string) string { return `{"securitiesAccount":` + securities + "}" }
	badPrice := withKeys(basePosition, "averagePrice", `"x"`)
	fuzzRecord[Account](f,
		account(baseSecuritiesAccount),
		account(withKeys(baseSecuritiesAccount, "positions", "[]")),
		account(withKeys(baseSecuritiesAccount, "positions", "["+basePosition+"]")),
		// Two positions with the same problem, which the account's problems
		// hold once.
		account(withKeys(baseSecuritiesAccount, "positions", "["+badPrice+","+badPrice+"]")),
		// TestDecodeAccountLeniently's and TestDecodeOtherRecordsLeniently's.
		account(withKeys(baseSecuritiesAccount, "currentBalances", `{"isInCall":0,"cashBalance":12.5}`,
			"positions", "["+withKeys(basePosition, "marketValue", "")+","+withKeys(basePosition, "instrument", "null")+"]")),
		account(withKeys(baseSecuritiesAccount, "accountNumber", `""`)),
		// TestEveryMismatchIsNamed's.
		`{"securitiesAccount":`+withKeys(baseSecuritiesAccount, "isPortfolioMargin", "1", "currentBalances", "",
			"CurrentBalances", `{"IsInCall":0,"cashBalance":"x","liquidationValue":9}`)+`,"aggregatedBalance":{"liquidationValue":[1]}}`,
		`{"securitiesAccount":`+withKeys(baseSecuritiesAccount, "currentBalances", `"x"`)+`,"aggregatedBalance":5}`,
		// TestDuplicateKeys's.
		account(`{"type":"CASH","currentBalances":{"cashBalance":100,"liquidationValue":500},`+baseSecuritiesAccount[1:]),
		account(withKeys(baseSecuritiesAccount, "currentBalances", `{"cashBalance":1,"liquidationValue":"x","liquidationValue":5}`)),
		`{"securitiesAccount":`+baseSecuritiesAccount+`,"aggregatedBalance":5,"aggregatedBalance":null}`,
		account(`{"currentBalances":{"liquidationValue":7},"currentBalances":{"cashBalance":1},`+
			withKeys(baseSecuritiesAccount, "currentBalances", "null")[1:]),
		// TestNullElements's.
		account(withKeys(baseSecuritiesAccount, "positions", "["+basePosition+",null]")),
		strings.Replace(account(baseSecuritiesAccount), `"type"`, `"\u0074ype"`, 1),
		" "+account(baseSecuritiesAccount)+"\n",
		withKeys(account(baseSecuritiesAccount), "problems", "5"),
		withKeys(account(baseSecuritiesAccount), "problems", "[{}]"),
	)
}

// A position decoded from any bytes has the properties fuzzRecord lists.
func FuzzPosition(f *testing.F) {
	fuzzRecord[Position](f,
		basePosition,
		withKeys(basePosition, "instrument", `{"assetType":"OPTION","symbol":"XYZ   260320P00050000","putCall":"PUT","underlyingSymbol":"XYZ"}`),
		withKeys(basePosition, "marketValue", ""),
		withKeys(basePosition, "instrument", "null"),
		// TestDecodeOtherRecordsLeniently's and TestProblemsNamedOnce's.
		withKeys(basePosition, "instrument", `{"assetType":"","symbol":"X"}`, "longQuantity", `"1"`, "marketValue", `"x"`),
		withKeys(basePosition, "instrument", `"X"`),
		// TestDuplicateKeys's.
		basePosition[:len(basePosition)-1]+`,"averagePrice":"x"}`,
		basePosition[:len(basePosition)-1]+`,"averagePrice":null}`,
		`{"instrument":{"assetType":"OPTION","symbol":"Y","cusip":"1"},`+basePosition[1:],
		`{"instrument":5,"instrument":null,`+withKeys(basePosition, "instrument", "")[1:],
		`{"instrument":5,`+basePosition[1:],
		strings.Replace(basePosition, `"longQuantity"`, `"\u006congQuantity"`, 1),
		strings.Replace(basePosition, `"averagePrice"`, `"\u0061veragePrice"`, 1),
		" "+basePosition+"\n",
		withKeys(basePosition, "problems", "5"),
		withKeys(basePosition, "problems", "[{}]"),
	)
}

// An account number decoded from any bytes has the properties fuzzRecord
// lists.
func FuzzAccountNumber(f *testing.F) {
	fuzzRecord[AccountNumber](f,
		`{"accountNumber":"29470135","hashValue":"H"}`,
		`{"accountNumber":"","hashValue":""}`,
		`{"accountNumber":29470135,"hashValue":"H"}`,
		`{"AccountNumber":"1","accountNumber":"2","hashValue":"H"}`,
		`{"\u0061ccountNumber":"29470135","hashValue":"H"}`,
		" {\"accountNumber\":\"29470135\",\"hashValue\":\"H\"}\n",
		`{"accountNumber":"29470135","hashValue":"H","problems":5}`,
		`{"accountNumber":"29470135","hashValue":"H","problems":[{}]}`,
	)
}

// The user's preferences decoded from any bytes have the properties
// fuzzRecord lists.
func FuzzUserPreference(f *testing.F) {
	fuzzRecord[UserPreference](f,
		`{"accounts":[],"streamerInfo":[],"offers":[]}`,
		`{"accounts":[{"accountNumber":"1","primaryAccount":true,"type":"BROKERAGE","nickName":"A","displayAcctId":"...1",`+
			`"autoPositionEffect":false,"accountColor":"Green"}],"streamerInfo":[],"offers":[{"level2Permissions":false,"mktDataPermission":"NP"}]}`,
		// TestDecodeOtherRecordsLeniently's and TestNullElements's.
		`{"accounts":{},"offers":[{"level2Permissions":"yes"}]}`,
		`{"accounts":[null],"streamerInfo":[null],"offers":[null]}`,
		`{"\u0061ccounts":[],"streamerInfo":[],"offers":[]}`,
		"\t{\"accounts\":[],\"streamerInfo\":[],\"offers\":[]}\r\n",
		`{"accounts":[],"streamerInfo":[],"offers":[],"problems":5}`,
		`{"accounts":[],"streamerInfo":[],"offers":[],"problems":[{}]}`,
	)
}

// decodeList gives the records of any list, each with its own bytes and as
// it decodes alone, and refuses anything else.
func FuzzDecodeList(f *testing.F) {
	for _, s := range []string{
		string(readTestdata(f, "transactions.json")), `[]`, " [ ] ", "[]\n", "[" + baseTransaction + "]",
		"[null,7,\"x\",[],{}]", "\t[\r\n" + baseTransaction + "\r\n]\t",
		"[" + withKeys(baseTransaction, "netAmount", `"x"`) + ",null,7," + withKeys(baseTransaction, "activityId", "2") + "]",
		`{"activityId":1}`, `null`, ``, `[{"a":1}`, `[] []`, `[1,]`, `[`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		list, err := decodeList[Transaction](body)
		trimmed := bytes.TrimSpace(body)
		switch {
		case len(trimmed) == 0 || trimmed[0] != '[':
			if err == nil || list != nil || !strings.HasSuffix(err.Error(), ", not a list") {
				t.Fatalf("decodeList(%q) = %v, %v; want no list and an error", body, list, err)
			}
			return
		case !json.Valid(trimmed):
			if err == nil {
				t.Fatalf("decodeList(%q) took invalid JSON", body)
			}
			return
		case err != nil || list == nil:
			t.Fatalf("decodeList(%q) = %v, %v", body, list, err)
		}
		var elems []json.RawMessage
		if err := json.Unmarshal(trimmed, &elems); err != nil {
			t.Fatal(err)
		}
		if len(list) != len(elems) {
			t.Fatalf("decodeList(%q) gave %d records for %d elements", body, len(list), len(elems))
		}
		for i, e := range elems {
			if !bytes.Equal(list[i].Raw, bytes.TrimSpace(e)) {
				t.Fatalf("record %d of %q: Raw %q, want %q", i, body, list[i].Raw, e)
			}
			var alone Transaction
			if err := json.Unmarshal(e, &alone); err != nil || !reflect.DeepEqual(alone, list[i]) {
				t.Fatalf("record %d of %q decodes differently alone: %v", i, body, err)
			}
		}
	})
}
