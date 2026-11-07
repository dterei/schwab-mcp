package schwab

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Problem is a value in a record's JSON that its typed fields could not
// take, or a key that every record of its kind has and this one lacks. The
// field keeps its zero value, or nil, and the record's Raw has what Schwab
// sent.
type Problem struct {
	// Path is where the value is: the JSON keys that lead to it, joined by
	// dots and without array indexes, such as
	// "transferItems.instrument.closingPrice", or "" for the record itself.
	Path string `json:"path,omitempty"`
	// What says what is wrong, such as "Schwab sent a string where a number
	// was expected".
	What string `json:"what"`
}

// String returns the problem as "path: what", or what alone.
func (p Problem) String() string {
	if p.Path == "" {
		return p.What
	}
	return p.Path + ": " + p.What
}

// decode decodes b, one record of Schwab's JSON, into w, the record's
// wrapper: a struct that embeds, by value, a method-less alias of the
// record's type, beside the fields that the record reads by hand. It
// reports whether b is an object, and encoding/json's error, which says only
// that something could not be read: a value of the wrong JSON type is
// skipped, and the rest of the record still decodes.
func decode(b []byte, w any) (isObject bool, err error) {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return false, nil
	}
	return true, json.Unmarshal(b, w)
}

// recordProblems returns the problems of rec, a pointer to a record decoded
// from b: that b is not an object, or, when its decode failed with err, each
// value whose JSON kind its field cannot take. A pointer field that such a
// value left pointing at a zero is set back to nil, since nil means that
// Schwab did not send it. skip names the keys that the record reads by hand,
// which report their own problems.
func recordProblems(b []byte, rec any, isObject bool, err error, skip ...string) []Problem {
	b = bytes.TrimSpace(b)
	if !isObject {
		return []Problem{{What: sent(b, "an object")}}
	}
	if err == nil {
		return nil
	}
	ps := mismatches(b, reflect.ValueOf(rec).Elem(), "", skip, true)
	if len(ps) == 0 {
		// The walk finds what encoding/json skipped; should it not, say what
		// encoding/json said.
		ps = []Problem{errorProblem(err)}
	}
	return ps
}

// errorProblem describes a decoding error that the walk did not explain.
func errorProblem(err error) Problem {
	if te, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		return Problem{Path: jsonPath(te.Field), What: "Schwab sent " + te.Value + " where " + expected(te.Type) + " was expected"}
	}
	return Problem{What: "the JSON could not be read: " + err.Error()}
}

// jsonPath turns the field path of a decoding error into the path of JSON
// keys it is at: Go 1.26 starts it with the name of the alias type that the
// records decode into, plain, and Go 1.27 writes array indexes, which a
// Problem's path leaves out.
func jsonPath(field string) string {
	var keys []string
	for i, k := range strings.Split(field, ".") {
		if (i == 0 && k == "plain") || k == "" || strings.Trim(k, "0123456789") == "" {
			continue
		}
		keys = append(keys, k)
	}
	return strings.Join(keys, ".")
}

// timeType and rawType are the types that the walk does not look into:
// times, which the records read by hand, and raw JSON, which takes any
// value.
var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// mismatches walks raw, a JSON value, against v, the Go value decoded from
// it, at path, and returns a problem for each value whose JSON kind v's type
// cannot take, the way encoding/json matches keys to fields. top is true
// for the record itself; a nested value whose type has its own UnmarshalJSON
// reports its own problems, and is passed over. skip names keys of the
// record that it reads by hand.
func mismatches(raw json.RawMessage, v reflect.Value, path string, skip []string, top bool) []Problem {
	t := v.Type()
	if t == timeType || t == rawType {
		return nil
	}
	if !top && v.CanAddr() {
		if _, ok := reflect.TypeAssert[json.Unmarshaler](v.Addr()); ok {
			return nil
		}
	}
	if bytes.Equal(raw, []byte("null")) {
		return nil
	}
	switch t.Kind() {
	case reflect.Pointer:
		if what := mismatch(raw, t.Elem()); what != "" {
			v.SetZero()
			return []Problem{{Path: path, What: what}}
		}
		if v.IsNil() {
			return nil
		}
		return mismatches(raw, v.Elem(), path, nil, false)
	case reflect.Struct:
		if what := mismatch(raw, t); what != "" {
			return []Problem{{Path: path, What: what}}
		}
		members := objectMembers(raw)
		var ps []Problem
		for i := range t.NumField() {
			name := jsonName(t.Field(i))
			if name == "" || slices.Contains(skip, name) {
				continue
			}
			// As encoding/json does, a key takes the field of its name, or else
			// of its name in another case, and the last such key wins.
			var value json.RawMessage
			for _, m := range members {
				if m.key == name || strings.EqualFold(m.key, name) {
					value = m.value
				}
			}
			if value != nil {
				ps = append(ps, mismatches(value, v.Field(i), joinPath(path, name), nil, false)...)
			}
		}
		return ps
	case reflect.Slice:
		if what := mismatch(raw, t); what != "" {
			return []Problem{{Path: path, What: what}}
		}
		var elems []json.RawMessage
		if json.Unmarshal(raw, &elems) != nil {
			return nil
		}
		var ps []Problem
		for i, e := range elems {
			if i < v.Len() {
				ps = append(ps, mismatches(e, v.Index(i), path, nil, false)...)
			}
		}
		return ps
	default:
		if what := mismatch(raw, t); what != "" {
			return []Problem{{Path: path, What: what}}
		}
		return nil
	}
}

// mismatch returns what is wrong with raw, a JSON value other than null, as
// a value of type t, or "" if nothing is.
func mismatch(raw json.RawMessage, t reflect.Type) string {
	isNumber := len(raw) > 0 && (raw[0] == '-' || raw[0] >= '0' && raw[0] <= '9')
	switch t.Kind() {
	case reflect.Float32, reflect.Float64:
		if !isNumber {
			return sent(raw, "a number")
		}
		if _, err := strconv.ParseFloat(string(raw), t.Bits()); err != nil {
			return fmt.Sprintf("Schwab sent the number %s, which is too large", raw)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if !isNumber {
			return sent(raw, "a whole number")
		}
		if _, err := strconv.ParseInt(string(raw), 10, t.Bits()); err != nil {
			return fmt.Sprintf("Schwab sent the number %s where a whole number was expected", raw)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if !isNumber {
			return sent(raw, "a whole number")
		}
		if _, err := strconv.ParseUint(string(raw), 10, t.Bits()); err != nil {
			return fmt.Sprintf("Schwab sent the number %s where a whole number was expected", raw)
		}
	case reflect.String:
		if raw[0] != '"' {
			return sent(raw, "a string")
		}
	case reflect.Bool:
		if !bytes.Equal(raw, []byte("true")) && !bytes.Equal(raw, []byte("false")) {
			return sent(raw, "a boolean")
		}
	case reflect.Struct:
		if raw[0] != '{' {
			return sent(raw, "an object")
		}
	case reflect.Slice:
		if raw[0] != '[' {
			return sent(raw, "an array")
		}
	}
	return ""
}

// sent says that Schwab sent raw where something else was expected.
func sent(raw json.RawMessage, want string) string {
	return "Schwab sent " + describeJSON(raw) + " where " + want + " was expected"
}

// expected names the JSON that a Go type takes.
func expected(t reflect.Type) string {
	if t == nil {
		return "something else"
	}
	switch t.Kind() {
	case reflect.Pointer:
		return expected(t.Elem())
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "a boolean"
	case reflect.Slice, reflect.Array:
		return "an array"
	default:
		return "an object"
	}
}

// member is one key and value of a JSON object.
type member struct {
	key   string
	value json.RawMessage
}

// objectMembers returns the members of raw, a JSON object, in order.
func objectMembers(raw json.RawMessage) []member {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var ms []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ms
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return ms
		}
		ms = append(ms, member{key: key, value: value})
	}
	return ms
}

// jsonName returns the JSON key of a struct field, or "" for a field that
// JSON leaves out.
func jsonName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	switch name {
	case "-":
		return ""
	case "":
		return f.Name
	}
	return name
}

// joinPath returns the path of key within path. Either may be "".
func joinPath(path, key string) string {
	switch {
	case path == "":
		return key
	case key == "":
		return path
	}
	return path + "." + key
}

// timeLayouts are the forms that Schwab writes instants in. The first is
// what the Accounts and Trading API sends, 2024-04-04T15:56:07+0000, whose
// offset has no colon, so that RFC 3339, and time.Time's own decoding,
// refuse it; it also reads Z and fractions of a second. Market Data writes
// RFC 3339.
var timeLayouts = []string{"2006-01-02T15:04:05Z0700", time.RFC3339Nano}

// readTime reads raw, the value at path, a timestamp as Schwab writes one,
// as an instant in UTC, and reports whether the key was there and not null.
// A date alone stands for midnight in New York, as Schwab's dates do.
// Anything that is not a timestamp is the zero time and a problem.
func readTime(raw json.RawMessage, path string, ps *[]Problem) (time.Time, bool) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return time.Time{}, false
	}
	var s string
	if raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		*ps = append(*ps, Problem{Path: path, What: sent(raw, "a time")})
		return time.Time{}, true
	}
	if s == "" {
		*ps = append(*ps, Problem{Path: path, What: "Schwab sent an empty string, which is not a time"})
		return time.Time{}, true
	}
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			if badOffset(s) {
				*ps = append(*ps, Problem{Path: path, What: fmt.Sprintf("Schwab sent %q, whose offset from UTC is out of range", s)})
				return time.Time{}, true
			}
			return t.UTC(), true
		}
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, newYork()); err == nil {
		return t.UTC(), true
	}
	*ps = append(*ps, Problem{Path: path, What: fmt.Sprintf("Schwab sent %q, which is not a time", s)})
	return time.Time{}, true
}

// badOffset reports whether s, a timestamp that time.Parse took, has an
// offset from UTC of 24 hours or more, or with 60 minutes or more, which
// time.Parse lets through.
func badOffset(s string) bool {
	i := strings.LastIndexAny(s, "+-")
	if i < strings.IndexByte(s, 'T') {
		return false // the offset is Z; the sign found is the date's
	}
	off := strings.ReplaceAll(s[i+1:], ":", "")
	return len(off) == 4 && (off[:2] > "23" || off[2:] > "59")
}

// readField decodes raw, the value at path, into v, and reports whether the
// key was there and not null. A value of the wrong JSON type leaves v as it
// was and is a problem.
func readField(raw json.RawMessage, path string, v any, ps *[]Problem) bool {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return false
	}
	if what := mismatch(raw, reflect.TypeOf(v).Elem()); what != "" {
		*ps = append(*ps, Problem{Path: path, What: what})
		return true
	}
	if err := json.Unmarshal(raw, v); err != nil {
		*ps = append(*ps, Problem{Path: path, What: "Schwab's value could not be read: " + err.Error()})
	}
	return true
}

// need adds a problem to ps when a key that every record of its kind has is
// missing: absent, null or empty. A key that already has a problem, such as
// a value of the wrong type, or whose parent has one, gets no second one.
func need(ps *[]Problem, present bool, key string) {
	if present {
		return
	}
	for _, p := range *ps {
		if p.Path == key || strings.HasPrefix(key, p.Path+".") || p.Path == "" {
			return
		}
	}
	*ps = append(*ps, Problem{Path: key, What: "missing, though Schwab sends it on every record of its kind"})
}

// within returns a nested object's problems with their paths inside prefix,
// the path of the object.
func within(prefix string, nested []Problem) []Problem {
	out := make([]Problem, len(nested))
	for i, p := range nested {
		out[i] = Problem{Path: joinPath(prefix, p.Path), What: p.What}
	}
	return out
}

// copyRaw returns a copy of b without the space around it; the decoder may
// reuse b.
func copyRaw(b []byte) json.RawMessage {
	return append(json.RawMessage(nil), bytes.TrimSpace(b)...)
}

// decodeList decodes body as a JSON array of records, which Schwab sends for
// the list endpoints. A record that cannot be read is still in the list,
// with its problems.
func decodeList[T any](body []byte) ([]T, error) {
	b := bytes.TrimSpace(body)
	if len(b) == 0 || b[0] != '[' {
		return nil, fmt.Errorf("the answer is %s, not a list", describeJSON(b))
	}
	var list []T
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// decodeOne decodes body as one record. Schwab's spec has some endpoints
// answer with a list of one, though Schwab sends the object, so a list of
// one object is taken too.
func decodeOne[T any](body []byte) (T, error) {
	var one T
	b := bytes.TrimSpace(body)
	switch {
	case len(b) > 0 && b[0] == '{':
		err := json.Unmarshal(b, &one)
		return one, err
	case len(b) > 0 && b[0] == '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(b, &elems); err != nil {
			return one, err
		}
		if len(elems) != 1 {
			return one, fmt.Errorf("the answer is a list of %d, where one was expected", len(elems))
		}
		if e := bytes.TrimSpace(elems[0]); len(e) == 0 || e[0] != '{' {
			return one, fmt.Errorf("the answer is a list holding %s, not an object", describeJSON(e))
		}
		err := json.Unmarshal(elems[0], &one)
		return one, err
	}
	return one, fmt.Errorf("the answer is %s, not an object", describeJSON(b))
}

// decodeError is an answer that came with 200 but is not the JSON its
// endpoint sends. It keeps the answer's correlation ID, for a report to
// Schwab, but never the body, which can hold account numbers.
type decodeError struct {
	what     string // what was being read, such as "the accounts"
	correlID string
	err      error
}

func (e *decodeError) Error() string {
	msg := "decoding " + e.what + ": " + e.err.Error()
	if e.correlID != "" {
		msg += " (correlation ID " + e.correlID + ")"
	}
	return msg
}

func (e *decodeError) Unwrap() error { return e.err }

// list decodes r, Schwab's answer, as a list of records; what names them,
// for an error.
func list[T any](r *reply, what string) ([]T, error) {
	v, err := decodeList[T](r.body)
	if err != nil {
		return nil, &decodeError{what: what, correlID: r.header.Get(correlationHeader), err: err}
	}
	return v, nil
}

// one decodes r, Schwab's answer, as one record; what names it, for an
// error.
func one[T any](r *reply, what string) (T, error) {
	v, err := decodeOne[T](r.body)
	if err != nil {
		var zero T
		return zero, &decodeError{what: what, correlID: r.header.Get(correlationHeader), err: err}
	}
	return v, nil
}

// describeJSON names the kind of JSON value that b starts, for a problem or
// an error.
func describeJSON(b []byte) string {
	if len(b) == 0 {
		return "nothing"
	}
	switch b[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	case '"':
		return "a string"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "a number"
	}
	return "something else"
}
