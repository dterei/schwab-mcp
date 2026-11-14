package mask

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A Class is what the values at a key path of Schwab's JSON hold, as far as
// masking goes. JSON masks each value as its class says, and keeps true,
// false and null under every class. It takes a number for its value,
// however it is written: 12345678, 12345678.0 and 1.2345678E7 are all the
// integer 12345678. A Class that is none of those below is masked as
// Unknown.
type Class int

// The classes, each with what JSON makes of its values.
const (
	// Unknown is a key that no table knows. A string is withheld unless it
	// is wholly a time in one of Schwab's forms, a short code (a capital
	// letter and up to 40 more capitals, digits and underscores, with no run
	// of six digits: the shape of Schwab's codes) or an account number
	// already masked ("..." and three digits); a time that holds a known
	// number is withheld too. An integer of 8 or 9 digits, or one that is a
	// known number, becomes Number of its digits, a string; any other
	// number is kept, so that a 13-digit ID or a time in milliseconds comes
	// through.
	Unknown Class = iota
	// AccountNumber is a full account number, as a string or a number: it
	// becomes Number of it, a string, so that "12345678", 12345678 and
	// 12345678.0 read "...678", and Schwab's own masked "...678" stays as it
	// is.
	AccountNumber
	// Hash is an account's hash, which the next request passes back;
	// Identifier an ID, a symbol, a CUSIP or one of Schwab's codes; Time a
	// time or date in one of Schwab's forms; Amount money, a quantity or a
	// price. Each is kept, but a known number that a string holds is masked
	// as Known.Text masks it, which keeps a hash whole, and an integer that
	// is a known number becomes Number of its digits, a string.
	Hash
	Identifier
	Time
	Amount
	// FreeText is text that Schwab or a person wrote, such as a
	// description: a string is masked by Text, then by Known.Text. A number
	// is kept, but one that is a known number, as Identifier's.
	FreeText
	// Tag is an order's tag, whose text may name an app or its developer: a
	// tag of Schwab's own platform, API_TOS: and capitals, is kept whole, a
	// TA_ tag becomes "TA_...", and any other string or number
	// "[withheld]".
	Tag
	// Withheld is a value never shown, such as anything in a transaction's
	// user object, which names a person: a string becomes "[withheld]" and
	// a number null.
	Withheld
)

// withheld is what a string, or a tag, that JSON withholds becomes.
const withheld = "[withheld]"

// A Report says what JSON replaced, for a log and for tests.
type Report struct {
	// AccountNumbers counts the account numbers masked: the values of
	// AccountNumber, and the known numbers in the values of the other
	// classes and in keys; Tags the tags reduced to their origin or
	// withheld; Withheld the values withheld, of Withheld and of Unknown;
	// and UnknownIntegers the integers of Unknown masked. Free text that
	// Text masked by pattern is not counted.
	AccountNumbers, Tags, Withheld, UnknownIntegers int
	// Paths are the key paths of the values counted, keys joined with ".",
	// each once, in the order that the first of their values came.
	Paths []string
}

// errNotJSON is JSON's error: it never quotes what it was given.
var errNotJSON = errors.New("mask: not one JSON value")

// JSON rewrites raw, one value of Schwab's JSON, so that it holds no
// account number and no text it cannot vouch for: it walks the tokens,
// writes them compactly in the order they came, number literals as sent and
// strings re-encoded without HTML escaping, and replaces each value that
// the class of its key path calls for, as classOf gives it. A path is the
// keys that lead to the value from the root, without array indexes, and
// classOf may keep it. k holds the known numbers, which JSON masks in keys
// too; it may be nil. Every rule maps what it writes to itself, so that
// masking what JSON wrote gives the same bytes and an empty Report. JSON
// fails only on raw that is not one JSON value.
func JSON(raw []byte, classOf func(path []string) Class, k *Known) ([]byte, Report, error) {
	if !json.Valid(raw) {
		return nil, Report{}, errNotJSON
	}
	w := &jsonWriter{classOf: classOf, known: k, counted: map[string]bool{}}
	w.enc = json.NewEncoder(&w.scratch)
	w.enc.SetEscapeHTML(false)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, Report{}, errNotJSON
		}
		if err := w.token(tok); err != nil {
			return nil, Report{}, err
		}
	}
	return w.out.Bytes(), w.report, nil
}

// A jsonWriter writes what JSON makes of raw's tokens, one at a time.
type jsonWriter struct {
	classOf func([]string) Class
	known   *Known
	out     bytes.Buffer
	// scratch is where enc encodes a string, before it goes to out.
	scratch bytes.Buffer
	enc     *json.Encoder
	// frames are the open objects and arrays, the innermost last, and path
	// the keys of the open objects, each its key now, masked.
	frames []frame
	path   []string
	report Report
	// counted are the paths in report.Paths.
	counted map[string]bool
}

// A frame is an open object or array.
type frame struct {
	object bool
	// key says, of an object, that its next token is a key.
	key bool
	// written counts the members of an object, or the elements of an array,
	// begun.
	written int
}

// token writes tok, with the comma or colon that goes before it.
func (w *jsonWriter) token(tok json.Token) error {
	if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
		if w.frames[len(w.frames)-1].object {
			w.path = w.path[:len(w.path)-1]
		}
		w.frames = w.frames[:len(w.frames)-1]
		w.out.WriteRune(rune(d))
		w.ended()
		return nil
	}
	if n := len(w.frames); n > 0 {
		f := &w.frames[n-1]
		if f.written > 0 && (!f.object || f.key) {
			w.out.WriteByte(',')
		}
		if f.object && f.key {
			f.written, f.key = f.written+1, false
			key, _ := tok.(string) // a valid object's keys are strings
			masked := w.known.Text(key)
			w.path[len(w.path)-1] = masked
			if masked != key {
				w.count(&w.report.AccountNumbers)
			}
			if err := w.write(masked); err != nil {
				return err
			}
			w.out.WriteByte(':')
			return nil
		}
		if !f.object {
			f.written++
		}
	}
	if d, ok := tok.(json.Delim); ok {
		w.out.WriteRune(rune(d))
		w.frames = append(w.frames, frame{object: d == '{', key: d == '{'})
		if d == '{' {
			w.path = append(w.path, "")
		}
		return nil
	}
	out, counter := w.rule(w.classOf(slices.Clone(w.path)), tok)
	if out != tok && counter != nil {
		w.count(counter)
	}
	w.ended()
	return w.write(out)
}

// ended notes that a value has ended: in an object, a key comes next.
func (w *jsonWriter) ended() {
	if n := len(w.frames); n > 0 && w.frames[n-1].object {
		w.frames[n-1].key = true
	}
}

// count adds one to counter, of report, and the value's path to its paths.
func (w *jsonWriter) count(counter *int) {
	*counter++
	if p := strings.Join(w.path, "."); !w.counted[p] {
		w.counted[p] = true
		w.report.Paths = append(w.report.Paths, p)
	}
}

// rule returns what tok, a value of class c, becomes, and the count of
// report that its replacement adds to, if any.
func (w *jsonWriter) rule(c Class, tok json.Token) (json.Token, *int) {
	r := &w.report
	switch v := tok.(type) {
	case string:
		switch c {
		case AccountNumber:
			return Number(v), &r.AccountNumbers
		case Hash, Identifier, Time, Amount:
			return w.known.Text(v), &r.AccountNumbers
		case FreeText:
			text := Text(v)
			if known := w.known.Text(text); known != text {
				return known, &r.AccountNumbers
			}
			return text, nil
		case Tag:
			return tagOrigin(v), &r.Tags
		case Withheld:
			return withheld, &r.Withheld
		}
		if (isTimeForm(v) || isShortCode(v) || maskedNumber.MatchString(v)) && !w.known.Contains(v) {
			return v, nil
		}
		return withheld, &r.Withheld
	case json.Number:
		digits, integer := integerDigits(v)
		switch c {
		case AccountNumber:
			if integer {
				return Number(digits), &r.AccountNumbers
			}
			return Number(string(v)), &r.AccountNumbers
		case Hash, Identifier, Time, Amount, FreeText:
			if integer && w.known.has(digits) {
				return Number(digits), &r.AccountNumbers
			}
			return v, nil
		case Tag:
			return withheld, &r.Tags
		case Withheld:
			return nil, &r.Withheld
		}
		if integer && (len(digits) == 8 || len(digits) == 9 || w.known.has(digits)) {
			return Number(digits), &r.UnknownIntegers
		}
		return v, nil
	}
	return tok, nil // true, false and null hold nothing
}

// write writes tok, a scalar, as JSON: a string without HTML escaping, a
// number literal as it came.
func (w *jsonWriter) write(tok json.Token) error {
	switch v := tok.(type) {
	case string:
		w.scratch.Reset()
		if err := w.enc.Encode(v); err != nil {
			return err
		}
		w.out.Write(bytes.TrimSuffix(w.scratch.Bytes(), []byte("\n")))
	case json.Number:
		w.out.WriteString(string(v))
	case bool:
		if v {
			w.out.WriteString("true")
		} else {
			w.out.WriteString("false")
		}
	case nil:
		w.out.WriteString("null")
	}
	return nil
}

var (
	// apiTOSTag is a tag of Schwab's own platform, such as
	// API_TOS:TRADE_ALL, which names no one.
	apiTOSTag = regexp.MustCompile(`^API_TOS:[A-Z_]+$`)
	// shortCode is the shape of Schwab's codes, such as NEW_STATE.
	shortCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,40}$`)
	// sixDigits is a run of six digits, which a code does not hold.
	sixDigits = regexp.MustCompile(`[0-9]{6}`)
	// maskedNumber is an account number masked, as Schwab and Number mask
	// one of more than three digits.
	maskedNumber = regexp.MustCompile(`^\.\.\.[0-9]{3}$`)
	// timeShape is the shape of Schwab's times: a date, then a time to the
	// second, after a T or a space, with a fraction of up to nine digits
	// and Z or an offset, with a colon or without, or none; or a date alone.
	// timeForms then read what has it.
	timeShape = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(?:[T ][0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,9})?(?:Z|[+-][0-9]{2}:?[0-9]{2})?)?$`)
)

// timeForms are the forms that Schwab writes times in: those that its
// records read (internal/schwab's timeLayouts), a date alone, and the
// instruments' date and time with a tenth of a second.
var timeForms = []string{"2006-01-02T15:04:05Z0700", time.RFC3339Nano, time.DateOnly, "2006-01-02 15:04:05.0"}

// isTimeForm reports whether s is wholly a time in one of Schwab's forms.
func isTimeForm(s string) bool {
	if !timeShape.MatchString(s) {
		return false
	}
	for _, layout := range timeForms {
		if _, err := time.Parse(layout, s); err == nil {
			return true
		}
	}
	return false
}

// isShortCode reports whether s has the shape of one of Schwab's codes,
// such as NEW_STATE, with no run of six digits.
func isShortCode(s string) bool {
	return shortCode.MatchString(s) && !sixDigits.MatchString(s)
}

// tagOrigin returns what an order's tag becomes: a tag of Schwab's own
// platform as it is, "TA_..." for a TA_ tag, and "[withheld]" for any
// other.
func tagOrigin(tag string) string {
	switch {
	case apiTOSTag.MatchString(tag):
		return tag
	case strings.HasPrefix(tag, "TA_"):
		return "TA_..."
	}
	return withheld
}

// maxIntegerDigits is the most digits that integerDigits writes out for a
// number whose exponent adds zeros to it.
const maxIntegerDigits = 64

// integerDigits returns the digits of n's value, without its sign or
// leading zeros, and whether that value is an integer, however JSON writes
// it: 29470135, -29470135, 29470135.0, 2.9470135E7 and 294701350e-1 all
// give "29470135", while 2947013.5 is no integer. A number whose exponent
// adds zeros to it past 64 digits, such as 1e999999, is taken for no
// integer, so that they are never written out: no account number has so
// many.
func integerDigits(n json.Number) (string, bool) {
	s := strings.TrimPrefix(string(n), "-")
	if s != "" && strings.Trim(s, "0123456789") == "" {
		return s, true // digits alone, as Schwab writes an ID or an account number
	}
	var exp int64
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.ParseInt(s[i+1:], 10, 32)
		if err != nil {
			return "", false
		}
		s, exp = s[:i], e
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" || strings.Trim(whole, "0123456789") != "" || strings.Trim(frac, "0123456789") != "" {
		return "", false // not a number as JSON writes one
	}
	// The value is digits times ten to the power exp.
	digits := strings.TrimLeft(whole+frac, "0")
	exp -= int64(len(frac))
	switch {
	case digits == "":
		return "0", true
	case exp < 0:
		point := int64(len(digits)) + exp // where the decimal point falls in digits
		if point <= 0 || strings.Trim(digits[point:], "0") != "" {
			return "", false
		}
		return digits[:point], true
	case exp > 0 && int64(len(digits))+exp > maxIntegerDigits:
		return "", false
	}
	return digits + strings.Repeat("0", int(exp)), true
}
