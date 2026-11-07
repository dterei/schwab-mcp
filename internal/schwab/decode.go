package schwab

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Problem is a value in a record's JSON that its typed fields could not
// take, a key that every record of its kind has and this one lacks, or an
// object or array that Schwab sent twice. The field keeps its zero value,
// or nil, or what encoding/json made of the copies, and the record's Raw
// has what Schwab sent.
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
// from b: that b is not an object, or what a walk of b against rec finds,
// which see. skip names the keys that the record reads by hand, which report
// their own problems. Should the decode have failed with err and the walk
// found no value that explains it, err itself is a problem.
func recordProblems(b []byte, rec any, isObject bool, err error, skip ...string) []Problem {
	b = bytes.TrimSpace(b)
	if !isObject {
		return []Problem{{What: sent(b, "an object")}}
	}
	var w walker
	w.value(b, reflect.ValueOf(rec).Elem(), "", "", skip, true)
	if err != nil && !w.mismatched {
		w.add("", "the JSON could not be read: "+err.Error())
	}
	return w.ps
}

// timeType and rawType are the types that the walk does not look into:
// times, which the records read by hand, and raw JSON, which takes any
// value.
var (
	timeType = reflect.TypeFor[time.Time]()
	rawType  = reflect.TypeFor[json.RawMessage]()
)

// walker walks a record's JSON against the Go value decoded from it, the way
// encoding/json matches keys to fields, and gathers the record's problems:
//   - a value whose JSON kind its field cannot take, which encoding/json
//     skipped, leaving the field zero; a pointer that such a value left
//     pointing at a zero is set back to nil, since nil means that Schwab did
//     not send it;
//   - a key that every record of its kind has, a field whose tag has no omit
//     option, that is absent or null;
//   - an object or array sent twice under one key, since encoding/json may
//     then combine the copies into a value that Schwab did not send;
//   - null as an element of an array of objects, which encoding/json takes
//     for an object without a single key.
//
// Of a key sent more than once, the last copy counts, as encoding/json has
// it for a number, string or boolean that fits its field; the walk sets
// the field to zero where a last copy of the wrong kind, or null, left it
// as an earlier copy set it. An earlier copy of the wrong kind is a problem
// too, and a copy that is null is no copy, for counting an object or array
// sent twice.
//
// A nested value whose type has its own UnmarshalJSON checks itself, and is
// passed over.
type walker struct {
	ps []Problem
	// mismatched is whether the walk found what explains an error of
	// encoding/json's: a value of the wrong JSON kind, or an object or array
	// sent twice, whose earlier copies are not walked.
	mismatched bool
}

func (w *walker) add(path, what string) {
	p := Problem{Path: path, What: what}
	if !slices.Contains(w.ps, p) {
		w.ps = append(w.ps, p)
	}
}

func (w *walker) mismatch(path, what string) {
	w.mismatched = true
	w.add(path, what)
}

// value walks raw, a JSON value, against v, the Go value decoded from it,
// at key within parent; the path is built only for a problem. top is true
// for the record itself, and skip names keys of the record that it reads by
// hand.
func (w *walker) value(raw []byte, v reflect.Value, parent, key string, skip []string, top bool) {
	t := v.Type()
	if t == timeType || t == rawType || (!top && decodesItself(t)) || isNull(raw) {
		return
	}
	switch t.Kind() {
	case reflect.Pointer:
		if what := mismatch(raw, t.Elem()); what != "" {
			v.SetZero()
			w.mismatch(joinPath(parent, key), what)
		} else if !v.IsNil() {
			w.value(raw, v.Elem(), parent, key, nil, false)
		}
	case reflect.Struct:
		if what := mismatch(raw, t); what != "" {
			// encoding/json left the field zero, or as an earlier copy of
			// the key set it.
			v.SetZero()
			w.mismatch(joinPath(parent, key), what)
			return
		}
		w.object(raw, v, joinPath(parent, key), skip)
	case reflect.Slice:
		if what := mismatch(raw, t); what != "" {
			v.SetZero()
			w.mismatch(joinPath(parent, key), what)
			return
		}
		at := joinPath(parent, key)
		elem := t.Elem()
		s := scan(raw, '[')
		for i := range v.Len() {
			e, ok := s.element()
			if !ok {
				break
			}
			if isNull(e) && elem.Kind() == reflect.Struct && !decodesItself(elem) {
				// encoding/json leaves such an element zero, without the keys
				// that every one of its kind has, and raises no error.
				w.add(at, sent(e, "an object"))
				continue
			}
			w.value(e, v.Index(i), at, "", nil, false)
		}
	default:
		if what := mismatch(raw, t); what != "" {
			// encoding/json skipped the value, leaving the field zero, or as
			// an earlier copy of the key set it.
			v.SetZero()
			w.mismatch(joinPath(parent, key), what)
		}
	}
}

// object walks the members of raw, a JSON object, against v, a struct, at
// path.
func (w *walker) object(raw []byte, v reflect.Value, path string, skip []string) {
	si := infoOf(v.Type())
	// How many times each field's key came, how many of those were not
	// null, and its last value.
	type sent struct {
		n, set int
		last   []byte
	}
	var buf [64]sent
	var keys []sent
	if n := len(si.fields); n <= len(buf) {
		keys = buf[:n]
	} else {
		keys = make([]sent, n)
	}
	s := scan(raw, '{')
	for key, value, ok := s.member(); ok; key, value, ok = s.member() {
		if j := si.field(key); j >= 0 {
			keys[j].n++
			if !isNull(value) {
				keys[j].set++
			}
			keys[j].last = value
		}
	}
	for j, f := range si.fields {
		if slices.Contains(skip, f.name) {
			continue
		}
		n, set, last := keys[j].n, keys[j].set, keys[j].last
		if n > 1 && !f.composite {
			// encoding/json keeps the last copy of a number, string or
			// boolean that fits; an earlier one that does not, whatever the
			// last copy is, is still a problem.
			t := derefType(v.Field(f.index).Type())
			s, earlier := scan(raw, '{'), n-1
			for key, value, ok := s.member(); ok && earlier > 0; key, value, ok = s.member() {
				if si.field(key) != j {
					continue
				}
				earlier--
				if what := mismatch(value, t); what != "" && !isNull(value) {
					w.mismatch(joinPath(path, f.name), what)
				}
			}
		}
		if n == 0 || isNull(last) {
			if n > 1 {
				// encoding/json lets a null leave a field as an earlier copy
				// set it. Zeroing the field discards that copy, and with it
				// whatever encoding/json could not read of an object or
				// array in it.
				v.Field(f.index).SetZero()
				w.mismatched = w.mismatched || (f.composite && set > 0)
			}
			if !f.optional {
				w.add(joinPath(path, f.name), "missing, though Schwab sends it on every record of its kind")
			}
			continue
		}
		if f.composite && set > 1 {
			// encoding/json may have combined the copies. Their walk is
			// skipped, and this problem stands for whatever it could not
			// read of the earlier ones.
			w.mismatch(joinPath(path, f.name), "Schwab sent it more than once, so what it holds is uncertain")
		}
		w.value(last, v.Field(f.index), path, f.name, nil, false)
	}
}

// fieldInfo is what the walk needs to know of a struct field.
type fieldInfo struct {
	index     int    // in its struct
	name      string // its JSON key
	optional  bool   // its tag marks it omitempty or omitzero
	composite bool   // it holds an object or an array
}

// structInfo is what the walk needs to know of a struct type.
type structInfo struct {
	fields        []fieldInfo    // the fields that JSON takes
	byKey         map[string]int // the place in fields of each one's key
	decodesItself bool           // it has an UnmarshalJSON of its own
}

// structInfos holds the structInfo of each struct type walked.
var structInfos sync.Map

// infoOf returns what the walk needs to know of t, a struct type.
func infoOf(t reflect.Type) *structInfo {
	if si, ok := structInfos.Load(t); ok {
		return si.(*structInfo)
	}
	si := &structInfo{
		byKey:         map[string]int{},
		decodesItself: reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()),
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if name := jsonName(f); name != "" {
			si.byKey[name] = len(si.fields)
			si.fields = append(si.fields, fieldInfo{index: i, name: name, optional: optional(f), composite: composite(f.Type)})
		}
	}
	got, _ := structInfos.LoadOrStore(t, si)
	return got.(*structInfo)
}

// field returns the place in si.fields of the field that key goes to, as
// encoding/json matches them: the field of that name, or else of that name
// in another case; or -1 for none.
func (si *structInfo) field(key []byte) int {
	if j, ok := si.byKey[string(key)]; ok {
		return j
	}
	for j, f := range si.fields {
		if bytes.EqualFold(key, []byte(f.name)) {
			return j
		}
	}
	return -1
}

// decodesItself reports whether t, or what it points to, is a struct with an
// UnmarshalJSON of its own.
func decodesItself(t reflect.Type) bool {
	t = derefType(t)
	return t.Kind() == reflect.Struct && infoOf(t).decodesItself
}

// optional reports whether Schwab sends f's key only on some records: its tag
// marks it omitempty or omitzero.
func optional(f reflect.StructField) bool {
	_, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
	return strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
}

// composite reports whether t holds an object or an array.
func composite(t reflect.Type) bool {
	t = derefType(t)
	return t != timeType && t != rawType && (t.Kind() == reflect.Struct || t.Kind() == reflect.Slice || t.Kind() == reflect.Map)
}

// derefType returns the type that t points to, or t.
func derefType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// isNull reports whether raw is JSON null.
func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
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

// scanner steps through the members of a JSON object, or the elements of an
// array. It expects valid JSON, as encoding/json gives an UnmarshalJSON, and
// stops at anything else.
type scanner struct {
	b []byte
	i int
}

// scan returns a scanner of b, which starts with open, '{' or '['.
func scan(b []byte, open byte) scanner {
	s := scanner{b: b, i: skipSpace(b, 0)}
	if s.i < len(b) && b[s.i] == open {
		s.i++
	} else {
		s.i = len(b)
	}
	return s
}

// more moves to the next member or element, and reports whether there is
// one.
func (s *scanner) more() bool {
	s.i = skipSpace(s.b, s.i)
	if s.i < len(s.b) && s.b[s.i] == ',' {
		s.i = skipSpace(s.b, s.i+1)
	}
	return s.i < len(s.b) && s.b[s.i] != '}' && s.b[s.i] != ']'
}

// member returns the object's next key, unquoted, and its value.
func (s *scanner) member() (key, value []byte, ok bool) {
	if !s.more() || s.b[s.i] != '"' {
		s.i = len(s.b)
		return nil, nil, false
	}
	end := valueEnd(s.b, s.i)
	if end < 0 {
		s.i = len(s.b)
		return nil, nil, false
	}
	key = s.b[s.i+1 : end-1]
	if bytes.IndexByte(key, '\\') >= 0 {
		var k string
		if json.Unmarshal(s.b[s.i:end], &k) != nil {
			s.i = len(s.b)
			return nil, nil, false
		}
		key = []byte(k)
	}
	i := skipSpace(s.b, end)
	if i >= len(s.b) || s.b[i] != ':' {
		s.i = len(s.b)
		return nil, nil, false
	}
	i = skipSpace(s.b, i+1)
	if end = valueEnd(s.b, i); end < 0 {
		s.i = len(s.b)
		return nil, nil, false
	}
	s.i = end
	return key, s.b[i:end], true
}

// element returns the array's next element.
func (s *scanner) element() (value []byte, ok bool) {
	if !s.more() {
		s.i = len(s.b)
		return nil, false
	}
	end := valueEnd(s.b, s.i)
	if end < 0 {
		s.i = len(s.b)
		return nil, false
	}
	value, s.i = s.b[s.i:end], end
	return value, true
}

// valueEnd returns where the JSON value that starts at b[i] ends, or -1.
func valueEnd(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		for j := i + 1; j < len(b); j++ {
			switch b[j] {
			case '\\':
				j++
			case '"':
				return j + 1
			}
		}
		return -1
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end := valueEnd(b, j)
				if end < 0 {
					return -1
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	// A number, true, false or null ends where white space or a delimiter
	// starts.
	for j := i; j < len(b); j++ {
		switch b[j] {
		case ',', ':', '}', ']', ' ', '\t', '\n', '\r':
			if j == i {
				return -1
			}
			return j
		}
	}
	return len(b)
}

// skipSpace returns where the JSON white space that starts at b[i] ends.
func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
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

// within adds nested, the problems of an object at prefix, to ps, with
// their paths inside prefix. A problem that ps already has, such as the same
// one in another element of an array, is not added again.
func within(ps []Problem, prefix string, nested []Problem) []Problem {
	for _, p := range nested {
		p.Path = joinPath(prefix, p.Path)
		if !slices.Contains(ps, p) {
			ps = append(ps, p)
		}
	}
	return ps
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
