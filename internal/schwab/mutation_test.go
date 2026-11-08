package schwab

import (
	"bytes"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// node is a JSON value whose members keep their order, so that a key can be
// sent twice: an object's keys and values, an array's elements, or the bytes
// of anything else, as they were sent.
type node struct {
	keys  []string
	vals  []*node
	array bool
	raw   []byte // a number, string, boolean or null
}

// parseNode returns raw, valid JSON, as a node.
func parseNode(raw []byte) *node {
	raw = bytes.TrimSpace(raw)
	switch raw[0] {
	case '{':
		keys, values, _ := jsonMembers(raw)
		n := &node{keys: keys}
		for _, v := range values {
			n.vals = append(n.vals, parseNode(v))
		}
		return n
	case '[':
		var elems []json.RawMessage
		_ = json.Unmarshal(raw, &elems)
		n := &node{array: true}
		for _, e := range elems {
			n.vals = append(n.vals, parseNode(e))
		}
		return n
	}
	return &node{raw: raw}
}

// text returns n as JSON.
func (n *node) text() string {
	parts := make([]string, len(n.vals))
	for i, v := range n.vals {
		parts[i] = v.text()
	}
	switch {
	case n.raw != nil:
		return string(n.raw)
	case n.array:
		return "[" + strings.Join(parts, ",") + "]"
	}
	for i, k := range n.keys {
		key, _ := json.Marshal(k)
		parts[i] = string(key) + ":" + parts[i]
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// member is a key's place in a record: the object that holds it, its index
// there, and the keys that lead to it.
type member struct {
	obj  *node
	i    int
	path []string
}

// membersOf adds the members of n, at path, and of every object within it,
// to out, each before those of its value.
func membersOf(n *node, path []string, out *[]member) {
	for i, v := range n.vals {
		at := path
		if !n.array {
			at = append(slices.Clip(path), n.keys[i])
			*out = append(*out, member{n, i, at})
		}
		membersOf(v, at, out)
	}
}

// fieldAt returns the field of a record of type t that the keys of path go
// to, matched as encoding/json matches them, and the path of its JSON keys,
// as a problem names it; or false if a key goes to no field.
func fieldAt(t reflect.Type, path []string) (reflect.StructField, string, bool) {
	var f reflect.StructField
	var names []string
	for _, key := range path {
		t = derefType(t)
		for t.Kind() == reflect.Slice {
			t = derefType(t.Elem())
		}
		if t.Kind() != reflect.Struct || t == timeType {
			return f, "", false
		}
		i := slices.IndexFunc(slices.Collect(t.Fields()), func(f reflect.StructField) bool {
			name := jsonName(f)
			return name != "" && name != "problems" && strings.EqualFold(name, key)
		})
		if i < 0 {
			return f, "", false
		}
		f = t.Field(i)
		names = append(names, jsonName(f))
		t = f.Type
	}
	return f, strings.Join(names, "."), true
}

// kindOf returns the JSON kind that a field of type t takes, as a problem
// names it, such as "a whole number" or "a time".
func kindOf(t reflect.Type) string {
	t = derefType(t)
	if t == timeType {
		return "a time"
	}
	switch t.Kind() {
	case reflect.String:
		return "a string"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "a whole number"
	case reflect.Bool:
		return "a boolean"
	case reflect.Slice:
		return "an array"
	}
	return "an object"
}

// takes reports whether a field that takes kind, as kindOf names it, takes
// the JSON value raw, whatever its value.
func takes(kind string, raw []byte) bool {
	switch got := describeJSON(raw); kind {
	case "a time":
		return got == "a string"
	case "a whole number":
		return got == "a number"
	default:
		return got == kind
	}
}

// Each member of each fixture record, at any depth, is changed in each way
// that Schwab could change it, and the record's problems say exactly what
// changed:
//
//   - a value of another JSON kind is named as such, and leaves the record as
//     if the member were taken out;
//   - taken out, or null, a key that Schwab sends on every record of its
//     kind is missing, and any other is no problem;
//   - a number, string or boolean sent twice, the same, is no problem, and
//     an object or array sent twice is one;
//   - the key in another case, or a key that no field takes beside it,
//     changes nothing, and Raw is the bytes sent.
//
// An account's problems include its positions'. Each member is changed in
// the first record that has its path, so that the cost follows the shapes
// of the records rather than their number, and a member that its object
// already repeats is left as it is, since its last copy counts.
func TestEveryMemberMutated(t *testing.T) {
	var mu sync.Mutex
	mutated := map[string]bool{}
	t.Run("types", func(t *testing.T) {
		for typ, records := range testdataRecords(t) {
			t.Run(typ.Name(), func(t *testing.T) {
				t.Parallel()
				done := map[string]bool{}
				for i, raw := range records {
					for _, path := range mutateMembers(t, typ, i, raw, done) {
						mu.Lock()
						mutated[path] = true
						mu.Unlock()
					}
				}
			})
		}
	})
	// The changes reach through arrays, pointers and nested records.
	for _, path := range []string{
		"securitiesAccount.currentBalances", "securitiesAccount.positions.instrument.assetType",
		"aggregatedBalance.liquidationValue", "instrument.assetType", "accounts.nickName",
		"transferItems.instrument.optionDeliverables.deliverable.symbol",
	} {
		if !mutated[path] {
			t.Errorf("no member at %s was changed", path)
		}
	}
}

// mutateMembers changes each member of raw, record i of type typ, whose
// path, in lower case, done does not yet hold, as TestEveryMemberMutated
// says, adds the path to done and returns the paths, as a problem names
// them.
func mutateMembers(t *testing.T, typ reflect.Type, i int, raw []byte, done map[string]bool) []string {
	t.Helper()
	// decoded returns what text decodes to: its problems, the record
	// without any Problems or Raw, and its Raw.
	decoded := func(text string) (ps []string, rec any, raw string) {
		v := reflect.New(typ).Elem()
		if err := json.Unmarshal([]byte(text), v.Addr().Interface()); err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		ps, raw = problemStrings(gatheredProblems(v)), string(v.FieldByName("Raw").Bytes())
		clearFields(v, "Problems", "Raw")
		return ps, v.Interface(), raw
	}
	root := parseNode(raw)
	ps, original, _ := decoded(root.text())
	if len(ps) > 0 {
		t.Fatalf("%s %d has problems: %q", typ.Name(), i, ps)
	}
	var members []member
	membersOf(root, nil, &members)
	var paths []string
	for _, m := range members {
		f, at, ok := fieldAt(typ, m.path)
		key := strings.ToLower(at)
		copies := 0
		for _, k := range m.obj.keys {
			if strings.EqualFold(k, m.obj.keys[m.i]) {
				copies++
			}
		}
		if !ok || done[key] || copies > 1 {
			continue
		}
		done[key] = true
		paths = append(paths, at)
		keys, vals := slices.Clone(m.obj.keys), slices.Clone(m.obj.vals)
		restore := func() { m.obj.keys, m.obj.vals = slices.Clone(keys), slices.Clone(vals) }
		check := func(how string, want []string, ref any) {
			t.Helper()
			text := root.text()
			got, rec, raw := decoded(text)
			switch {
			case !slices.Equal(got, want):
				t.Errorf("%s %d, %s %s: problems\n got %q\nwant %q", typ.Name(), i, at, how, got, want)
			case ref != nil && !reflect.DeepEqual(rec, ref):
				t.Errorf("%s %d, %s %s: decoded as\n%+v\nwant\n%+v", typ.Name(), i, at, how, rec, ref)
			case raw != text:
				t.Errorf("%s %d, %s %s: Raw is %s, want the bytes sent, %s", typ.Name(), i, at, how, raw, text)
			}
			restore()
		}

		var wantGone []string
		if !optional(f) {
			wantGone = missing(at)
		}
		m.obj.keys, m.obj.vals = slices.Delete(m.obj.keys, m.i, m.i+1), slices.Delete(m.obj.vals, m.i, m.i+1)
		_, gone, _ := decoded(root.text())
		check("taken out", wantGone, gone)
		m.obj.vals[m.i] = &node{raw: []byte("null")}
		check("null", wantGone, gone)

		kind := kindOf(f.Type)
		for _, other := range []string{`"x"`, `5`, `true`, `{}`, `[]`} {
			if takes(kind, []byte(other)) {
				continue
			}
			m.obj.vals[m.i] = parseNode([]byte(other))
			check("sent as "+other, []string{at + ": " + sent([]byte(other), kind)}, gone)
		}

		m.obj.keys, m.obj.vals = slices.Insert(m.obj.keys, m.i, m.obj.keys[m.i]), slices.Insert(m.obj.vals, m.i, m.obj.vals[m.i])
		if vals[m.i].raw == nil {
			check("sent twice", []string{at + ": Schwab sent it more than once, so what it holds is uncertain"}, nil)
		} else {
			check("sent twice", nil, original)
		}

		m.obj.keys[m.i] = strings.ToUpper(m.obj.keys[m.i])
		check("in upper case", nil, original)
		m.obj.keys, m.obj.vals = slices.Insert(m.obj.keys, m.i, "zzUnknown"), slices.Insert(m.obj.vals, m.i, &node{raw: []byte(`"a"`)})
		check("beside an unknown key", nil, original)
	}
	return paths
}
