package schwab

import (
	"bytes"
	"html"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

// A jsonScalar is a scalar of a JSON text, as walkJSON meets it: a string, a
// number, true, false or null.
type jsonScalar struct {
	path       []string    // the keys that lead to it, as sent, unescaped; array elements are implicit
	start, end int         // where its bytes are in the text
	in         *jsonObject // the object that holds it, or nil for an element of an array or the text itself
}

// A jsonObject is an object of a JSON text, as walkJSON meets it: its keys,
// unescaped, and the spans of their values in the text, in order, every copy
// of a key sent twice among them.
type jsonObject struct {
	keys  []string
	spans [][2]int
}

// last returns the span of the value of o's last member with key, in any
// case, since encoding/json takes the last of several, and whether o has
// one.
func (o *jsonObject) last(key string) ([2]int, bool) {
	if o == nil {
		return [2]int{}, false
	}
	for i, k := range slices.Backward(o.keys) {
		if strings.EqualFold(k, key) {
			return o.spans[i], true
		}
	}
	return [2]int{}, false
}

// walkJSON calls scalar with each scalar of b, valid JSON, and object with
// each object, before what it holds, in the order of their bytes. Either may
// be nil. It walks with the package's scanner, so that it meets every copy
// of a key sent twice, and gives each scalar's place in b, so that a scrubber
// can put another value there and leave every other byte as it is.
func walkJSON(b []byte, scalar func(jsonScalar), object func(path []string, o *jsonObject)) {
	start := skipSpace(b, 0)
	if end := valueEnd(b, start); end > start {
		walkJSONValue(b, start, end, nil, nil, scalar, object)
	}
}

// walkJSONValue is walkJSON for the value b[start:end], at path, in the
// object in.
func walkJSONValue(b []byte, start, end int, path []string, in *jsonObject, scalar func(jsonScalar), object func([]string, *jsonObject)) {
	v := b[start:end]
	switch v[0] {
	case '{':
		o := &jsonObject{}
		s := scan(v, '{')
		for key, value, ok := s.member(); ok; key, value, ok = s.member() {
			at := start + s.i - len(value)
			o.keys = append(o.keys, string(key))
			o.spans = append(o.spans, [2]int{at, at + len(value)})
		}
		if object != nil {
			object(path, o)
		}
		for i, k := range o.keys {
			walkJSONValue(b, o.spans[i][0], o.spans[i][1], append(slices.Clip(path), k), o, scalar, object)
		}
	case '[':
		s := scan(v, '[')
		for value, ok := s.element(); ok; value, ok = s.element() {
			at := start + s.i - len(value)
			walkJSONValue(b, at, at+len(value), path, nil, scalar, object)
		}
	default:
		if scalar != nil {
			scalar(jsonScalar{path: path, start: start, end: end, in: in})
		}
	}
}

// A splice puts with in place of the bytes from start to end.
type splice struct {
	start, end int
	with       []byte
}

// applySplices returns b with each of splices made, in order of their
// starts, none overlapping another, and every other byte as it is.
func applySplices(b []byte, splices []splice) []byte {
	if len(splices) == 0 {
		return b
	}
	slices.SortFunc(splices, func(x, y splice) int { return x.start - y.start })
	var out bytes.Buffer
	at := 0
	for _, s := range splices {
		out.Write(b[at:s.start])
		out.Write(s.with)
		at = s.end
	}
	out.Write(b[at:])
	return out.Bytes()
}

// unescapeJSON returns the text that raw, a JSON string with its quotes,
// holds, and for each byte of the text the offset in raw of the byte that it
// is, or -1 where it comes from an escape, such as \u0032. A surrogate
// escape that is not half of a pair is U+FFFD, as encoding/json reads it.
func unescapeJSON(raw []byte) (text []byte, at []int) {
	for i := 1; i < len(raw)-1; {
		if raw[i] != '\\' {
			text = append(text, raw[i])
			at = append(at, i)
			i++
			continue
		}
		r, n := rune(raw[i+1]), 2
		switch raw[i+1] {
		case 'b':
			r = '\b'
		case 'f':
			r = '\f'
		case 'n':
			r = '\n'
		case 'r':
			r = '\r'
		case 't':
			r = '\t'
		case 'u':
			r, n = hexRune(raw[i+2:i+6]), 6
			if utf16.IsSurrogate(r) {
				pair := utf8.RuneError
				if i+12 < len(raw) && raw[i+6] == '\\' && raw[i+7] == 'u' {
					pair = utf16.DecodeRune(r, hexRune(raw[i+8:i+12]))
				}
				if r = pair; r != utf8.RuneError {
					n = 12
				}
			}
		}
		text = utf8.AppendRune(text, r)
		for len(at) < len(text) {
			at = append(at, -1)
		}
		i += n
	}
	return text, at
}

// hexRune returns the rune of four hex digits, as a \u escape writes it.
func hexRune(digits []byte) rune {
	n, err := strconv.ParseUint(string(digits), 16, 32)
	if err != nil {
		return utf8.RuneError
	}
	return rune(n)
}

// htmlReference is a character reference of HTML at the start of a text:
// &#46;, &#x2E; or &amp;, with its semicolon.
var htmlReference = regexp.MustCompile(`^&(?:#[0-9]{1,7}|#[xX][0-9A-Fa-f]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});`)

// unescapeHTML returns the text of raw, a part of an HTML page, with its
// character references unescaped, and for each byte of the text the offset
// in raw of the byte that it is, or -1 where it comes from a reference; and
// whether that text is the one html.UnescapeString reads. A reference ends
// at its semicolon here, so one that html.UnescapeString takes without, as
// it takes &amp, makes the page one the scrubber does not read.
func unescapeHTML(raw []byte) (text []byte, at []int, ok bool) {
	for i := 0; i < len(raw); {
		if ref := htmlReference.Find(raw[i:]); ref != nil && html.UnescapeString(string(ref)) != string(ref) {
			text = append(text, html.UnescapeString(string(ref))...)
			for len(at) < len(text) {
				at = append(at, -1)
			}
			i += len(ref)
			continue
		}
		text = append(text, raw[i])
		at = append(at, i)
		i++
	}
	return text, at, string(text) == html.UnescapeString(string(raw))
}

// rawSpan returns where the bytes text[i:j] are in the raw form that text
// was unescaped from, at giving each byte's offset there as unescapeJSON and
// unescapeHTML do, and whether each of them is a byte of its own there,
// none part of an escape, and next to the one before: so that a value found
// in the text can be replaced in place, its neighbors' spelling kept.
func rawSpan(at []int, i, j int) (start, end int, ok bool) {
	if i >= j {
		return 0, 0, false
	}
	for k := i; k < j; k++ {
		if at[k] < 0 || k > i && at[k] != at[k-1]+1 {
			return 0, 0, false
		}
	}
	return at[i], at[j-1] + 1, true
}

// spellAs returns text spelled as raw, a part of a page, spells its own
// characters: each character that raw writes as a character reference is
// written with the same reference, the first raw uses for it, and the
// others as they are, but for a character raw does not hold, which is
// written as a decimal reference when raw writes every character as one.
// So Akamai's Reference, 18&#46;9efa3f17 and so on, keeps its &#46; dots.
func spellAs(text string, raw []byte) string {
	spelled := map[rune]string{}
	escaped := len(raw) > 0
	for i := 0; i < len(raw); {
		if ref := htmlReference.Find(raw[i:]); ref != nil && html.UnescapeString(string(ref)) != string(ref) {
			r, _ := utf8.DecodeRuneInString(html.UnescapeString(string(ref)))
			if _, ok := spelled[r]; !ok {
				spelled[r] = string(ref)
			}
			i += len(ref)
			continue
		}
		r, n := utf8.DecodeRune(raw[i:])
		if _, ok := spelled[r]; !ok {
			spelled[r] = string(raw[i : i+n])
		}
		escaped = false
		i += n
	}
	var b strings.Builder
	for _, r := range text {
		switch s, ok := spelled[r]; {
		case ok:
			b.WriteString(s)
		case escaped:
			b.WriteString("&#" + strconv.Itoa(int(r)) + ";")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isZeroLiteral reports whether lit, a JSON number, is a zero, such as 0,
// -0 or -0.0, which scaling keeps byte for byte.
func isZeroLiteral(lit []byte) bool {
	r, ok := new(big.Rat).SetString(string(lit))
	return ok && r.Sign() == 0
}

// hasExponent reports whether lit, a JSON number, has an exponent, as Java
// writes a double below 10^-3 or from 10^7 up, such as 1.0E-4.
func hasExponent(lit []byte) bool {
	return bytes.ContainsAny(lit, "eE")
}

// decimalsOf returns how many digits lit, a JSON number without an
// exponent, has after its point.
func decimalsOf(lit []byte) int {
	i := bytes.IndexByte(lit, '.')
	if i < 0 {
		return 0
	}
	return len(lit) - i - 1
}

// scaleLiteral returns lit, a JSON number without an exponent, multiplied by
// k in decimal, exactly, written with as many decimals as lit and its sign:
// 0.4513 by 20 is 9.0260, and -2.70 by 20 is -54.00.
func scaleLiteral(lit []byte, k int64) []byte {
	r, ok := new(big.Rat).SetString(string(lit))
	if !ok {
		return lit
	}
	r.Mul(r, new(big.Rat).SetInt64(k))
	return []byte(r.FloatString(decimalsOf(lit)))
}

// ratOf returns lit, a JSON number, as an exact fraction, and whether it is
// one.
func ratOf(lit []byte) (*big.Rat, bool) {
	if !isJSONNumber(lit) {
		return nil, false
	}
	return new(big.Rat).SetString(string(lit))
}

// The walk meets every scalar of a JSON text with its bytes' place, the keys
// that lead to it, as sent, unescaped, and every copy of a key sent twice,
// in the order of their bytes; and every object, with its members' keys and
// spans, so that the object that holds a scalar can be read.
func TestWalkJSON(t *testing.T) {
	b := []byte(` {"a" : 1, "b":[true, {"c":"x\"y"}, [null]], "aA":-2.5e3 ,"d":{}} `)
	var got []string
	var objects []string
	walkJSON(b, func(s jsonScalar) {
		holder := "-"
		if s.in != nil {
			holder = strings.Join(s.in.keys, "|")
		}
		got = append(got, strings.Join(s.path, ".")+"="+string(b[s.start:s.end])+" in "+holder)
	}, func(path []string, o *jsonObject) {
		objects = append(objects, strings.Join(path, ".")+":"+strings.Join(o.keys, ","))
	})
	want := []string{"a=1 in a|b|aA|d", "b=true in -", `b.c="x\"y" in c`, "b=null in -", "aA=-2.5e3 in a|b|aA|d"}
	if !slices.Equal(got, want) {
		t.Errorf("scalars %q, want %q", got, want)
	}
	if wantObjects := []string{":a,b,aA,d", "b:c", "d:"}; !slices.Equal(objects, wantObjects) {
		t.Errorf("objects %q, want %q", objects, wantObjects)
	}
	var o *jsonObject
	walkJSON([]byte(`{"x":1,"X":2,"y":3}`), nil, func(_ []string, obj *jsonObject) { o = obj })
	if span, ok := o.last("x"); !ok || span != [2]int{11, 12} {
		t.Errorf("the last x is at %v, %v", span, ok)
	}
	if _, ok := o.last("z"); ok {
		t.Error("found a key the object does not have")
	}
	walkJSON([]byte(`"alone"`), func(s jsonScalar) {
		if s.path != nil || s.in != nil || s.start != 0 || s.end != 7 {
			t.Errorf("the text itself: %+v", s)
		}
	}, nil)
}

// A JSON string's text is unescaped as encoding/json reads it, and each of
// its bytes knows whether it is a byte of its own in the string or comes
// from an escape.
func TestUnescapeJSON(t *testing.T) {
	for _, tc := range []struct {
		raw, text string
		at        []int
	}{
		{`"ab"`, "ab", []int{1, 2}},
		{`""`, "", nil},
		{`"a\"b"`, `a"b`, []int{1, -1, 4}},
		{`"\u0032\u00399"`, "299", []int{-1, -1, 13}},
		{`"\n\t\\\/"`, "\n\t\\/", []int{-1, -1, -1, -1}},
		{`"é!"`, "é!", []int{1, 2, 3}},
		{`"\u00e9\u00e9"`, "\u00e9\u00e9", []int{-1, -1, -1, -1}},
		{`"\ud83d\ude00!"`, "\U0001F600!", []int{-1, -1, -1, -1, 13}},
		{`"\ud83dx"`, "\uFFFDx", []int{-1, -1, -1, 7}},
		{`"\udc00\ud83d"`, "\uFFFD\uFFFD", []int{-1, -1, -1, -1, -1, -1}},
	} {
		text, at := unescapeJSON([]byte(tc.raw))
		if want, _ := jsonString([]byte(tc.raw)); string(text) != tc.text || string(text) != want || !slices.Equal(at, tc.at) {
			t.Errorf("unescapeJSON(%s) = %q, %v; want %q (encoding/json: %q), %v", tc.raw, text, at, tc.text, want, tc.at)
		}
	}
}

// A page's text is unescaped as html.UnescapeString reads it, each byte
// knowing whether it is one of the page's own or comes from a reference;
// and a value found in the text is replaced in the page's spelling.
func TestUnescapeHTML(t *testing.T) {
	raw := []byte("18&#46;9e&#x41;&amp;x")
	text, at, ok := unescapeHTML(raw)
	if string(text) != "18.9eA&x" || !ok || !slices.Equal(at, []int{0, 1, -1, 7, 8, -1, -1, 20}) {
		t.Errorf("unescapeHTML(%s) = %q, %v, %v", raw, text, at, ok)
	}
	if _, _, ok := unescapeHTML([]byte("a &amp b")); ok {
		t.Error("a reference without its semicolon is read as html.UnescapeString does not read it")
	}
	if text, _, ok := unescapeHTML([]byte("a & b &unknown; c")); !ok || string(text) != "a & b &unknown; c" {
		t.Errorf("text that holds no reference: %q, %v", text, ok)
	}
	if text, at, ok := unescapeHTML([]byte("& &#46;")); !ok || string(text) != "& ." || !slices.Equal(at, []int{0, 1, -1}) {
		t.Errorf("a lone & before a reference: %q, %v, %v", text, at, ok)
	}
	if s, e, ok := rawSpan(at, 0, 2); !ok || s != 0 || e != 2 {
		t.Errorf("the span of 18: %d, %d, %v", s, e, ok)
	}
	for _, span := range [][2]int{{1, 3}, {2, 3}, {3, 3}} {
		if _, _, ok := rawSpan(at, span[0], span[1]); ok {
			t.Errorf("the span %v, which a reference is in, can be replaced in place", span)
		}
	}
	for _, tc := range []struct{ raw, text, want string }{
		{"18&#46;9efa3f17", "77.0a1b2c3d", "77&#46;0a1b2c3d"},
		{"x & &#46;y", "z.", "z&#46;"},
		{"18.9efa&#x2E;3f17", "77.0a1b", "77.0a1b"},
		{"&#49;&#56;", "72", "&#55;&#50;"},
		{"&#49;&#56;", "81", "&#56;&#49;"},
		{"https&#58;&#47;&#47;api", "https://x", "https&#58;&#47;&#47;x"},
	} {
		if got := spellAs(tc.text, []byte(tc.raw)); got != tc.want {
			t.Errorf("spellAs(%q) as %q spells it = %q, want %q", tc.text, tc.raw, got, tc.want)
		}
	}
}

// Scaling multiplies a number by K exactly, in decimal, keeping its
// decimals and its sign; and the forms that scaling must leave alone, or
// cannot write as Java would, are known for what they are.
func TestScaleLiteral(t *testing.T) {
	for _, tc := range []struct {
		lit  string
		k    int64
		want string
	}{
		{"0.4513", 20, "9.0260"},
		{"-2.70", 20, "-54.00"},
		{"25", 7, "175"},
		{"81.29223999999999", 20, "1625.84479999999980"},
		{"0.01", 2, "0.02"},
		{"-0.0001", 3, "-0.0003"},
	} {
		if got := string(scaleLiteral([]byte(tc.lit), tc.k)); got != tc.want {
			t.Errorf("scaleLiteral(%s, %d) = %s, want %s", tc.lit, tc.k, got, tc.want)
		}
	}
	for _, lit := range []string{"0", "0.0", "-0", "-0.0", "0.000", "0e5", "-0E-3"} {
		if !isZeroLiteral([]byte(lit)) {
			t.Errorf("%s is not a zero", lit)
		}
	}
	for _, lit := range []string{"1", "-0.0001", "1e-9"} {
		if isZeroLiteral([]byte(lit)) {
			t.Errorf("%s is a zero", lit)
		}
	}
	for lit, want := range map[string]bool{"1.0E-4": true, "1.23456789E7": true, "1e2": true, "100": false, "-1.5": false} {
		if hasExponent([]byte(lit)) != want {
			t.Errorf("hasExponent(%s) = %v", lit, !want)
		}
	}
	if r, ok := ratOf([]byte("-1.25E2")); !ok || r.RatString() != "-125" {
		t.Errorf("ratOf(-1.25E2) = %v, %v", r, ok)
	}
	if _, ok := ratOf([]byte(`"5"`)); ok {
		t.Error("a string is a number")
	}
}
