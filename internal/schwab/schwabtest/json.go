package schwabtest

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"slices"
)

// The fake serves the bytes it was given. To change a member of an
// object, such as an account's positions or its hash, it splices the
// bytes rather than decoding and encoding them again, which would change
// the escapes, the numbers' forms, the order and repeats of keys, and the
// white space.

var errNotJSON = errors.New("not valid JSON")

// A member is one member of a JSON object, by where it is in the bytes.
type member struct {
	key      string // unescaped
	keyStart int
	valStart int
	valEnd   int
}

// space returns the index of the first byte of b at or after i that is not
// JSON's white space.
func space(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// valueEnd returns the end of the JSON value that starts at b[i].
func valueEnd(b []byte, i int) (int, error) {
	if i >= len(b) {
		return 0, errNotJSON
	}
	switch b[i] {
	case '"':
		for j := i + 1; j < len(b); j++ {
			switch b[j] {
			case '\\':
				j++
			case '"':
				return j + 1, nil
			}
		}
		return 0, errNotJSON
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				end, err := valueEnd(b, j)
				if err != nil {
					return 0, err
				}
				j = end - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1, nil
				}
			}
		}
		return 0, errNotJSON
	default:
		j := i
		for j < len(b) && !bytes.ContainsRune([]byte(" \t\r\n,]}"), rune(b[j])) {
			j++
		}
		if j == i {
			return 0, errNotJSON
		}
		return j, nil
	}
}

// members returns the members of the JSON object that starts at b[i].
func members(b []byte, i int) ([]member, error) {
	if i >= len(b) || b[i] != '{' {
		return nil, errNotJSON
	}
	var ms []member
	i = space(b, i+1)
	if i < len(b) && b[i] == '}' {
		return ms, nil
	}
	for {
		if i >= len(b) || b[i] != '"' {
			return nil, errNotJSON
		}
		keyEnd, err := valueEnd(b, i)
		if err != nil {
			return nil, err
		}
		var key string
		if err := json.Unmarshal(b[i:keyEnd], &key); err != nil {
			return nil, errNotJSON
		}
		colon := space(b, keyEnd)
		if colon >= len(b) || b[colon] != ':' {
			return nil, errNotJSON
		}
		start := space(b, colon+1)
		end, err := valueEnd(b, start)
		if err != nil {
			return nil, err
		}
		ms = append(ms, member{key: key, keyStart: i, valStart: start, valEnd: end})
		i = space(b, end)
		switch {
		case i < len(b) && b[i] == ',':
			i = space(b, i+1)
		case i < len(b) && b[i] == '}':
			return ms, nil
		default:
			return nil, errNotJSON
		}
	}
}

// elements returns the bytes of each element of the JSON array b.
func elements(b []byte) ([][]byte, error) {
	i := space(b, 0)
	if i >= len(b) || b[i] != '[' {
		return nil, errNotJSON
	}
	var es [][]byte
	i = space(b, i+1)
	if i < len(b) && b[i] == ']' {
		return es, nil
	}
	for {
		end, err := valueEnd(b, i)
		if err != nil {
			return nil, err
		}
		es = append(es, b[i:end])
		i = space(b, end)
		switch {
		case i < len(b) && b[i] == ',':
			i = space(b, i+1)
		case i < len(b) && b[i] == ']':
			return es, nil
		default:
			return nil, errNotJSON
		}
	}
}

// isObject reports whether s is one JSON object.
func isObject(s string) bool {
	b := []byte(s)
	return json.Valid(b) && b[space(b, 0)] == '{'
}

// keyIs reports whether a member's key is name, as encoding/json and
// package schwab match keys: unescaped, in any case.
func keyIs(m member, name string) bool { return bytes.EqualFold([]byte(m.key), []byte(name)) }

// withoutPositions returns account, an element of GET /accounts, without
// its positions: every member keyed positions, in any case, of every
// member keyed securitiesAccount that is an object, since a client merges
// repeated keys. A positions key elsewhere stays. The white space around
// what is cut stays, so that the answer keeps its layout.
func withoutPositions(account []byte) []byte {
	top, err := members(account, space(account, 0))
	if err != nil {
		return account
	}
	var spans [][2]int
	for _, m := range top {
		if !keyIs(m, "securitiesAccount") || account[m.valStart] != '{' {
			continue
		}
		inner, err := members(account, m.valStart)
		if err != nil {
			continue
		}
		spans = append(spans, removals(inner, func(m member) bool { return keyIs(m, "positions") })...)
	}
	return cut(account, spans)
}

// removals returns the spans of bytes that remove the members of one
// object that drop picks: a member followed by another from its key to the
// next key; the last member from the end of the last one before it that
// stays to its own end, or from its own key if none stays. Spans are
// taken on the object as it is, and may overlap.
func removals(ms []member, drop func(member) bool) [][2]int {
	var spans [][2]int
	for k, m := range ms {
		switch {
		case !drop(m):
		case k+1 < len(ms):
			spans = append(spans, [2]int{m.keyStart, ms[k+1].keyStart})
		default:
			j := k - 1
			for j >= 0 && drop(ms[j]) {
				j--
			}
			if j >= 0 {
				spans = append(spans, [2]int{ms[j].valEnd, m.valEnd})
			} else {
				spans = append(spans, [2]int{m.keyStart, m.valEnd})
			}
		}
	}
	return spans
}

// cut returns a copy of b without the bytes in spans.
func cut(b []byte, spans [][2]int) []byte {
	if len(spans) == 0 {
		return b
	}
	slices.SortFunc(spans, func(x, y [2]int) int { return cmp.Compare(x[0], y[0]) })
	var out []byte
	next := 0
	for _, s := range spans {
		if s[0] > next {
			out = append(out, b[next:s[0]]...)
		}
		next = max(next, s[1])
	}
	return append(out, b[next:]...)
}

// withString returns obj, a JSON object, with the value of every member
// keyed key, in any case, that is a string replaced by value, and whether
// there was one.
func withString(obj []byte, key, value string) ([]byte, bool) {
	ms, err := members(obj, space(obj, 0))
	if err != nil {
		return obj, false
	}
	var out []byte
	next, found := 0, false
	for _, m := range ms {
		if !keyIs(m, key) || obj[m.valStart] != '"' {
			continue
		}
		out = append(append(out, obj[next:m.valStart]...), jsonString(value)...)
		next, found = m.valEnd, true
	}
	return append(out, obj[next:]...), found
}
