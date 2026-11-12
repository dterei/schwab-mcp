package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// CutVersion is the version of the cut form. A new version may only tell
// apart less than the one before it, never more: a revision keeps only the
// first raw JSON of its run, so a run that a new cut would split cannot be.
const CutVersion = 1

// CutForm returns raw, a transaction's JSON as Schwab sent it, in its cut
// form, which two answers share exactly when they say the same thing of
// the transaction:
//
//   - In an object, the last copy of a repeated key wins, as in the
//     records.
//   - Every object under a key named instrument, in any case, keeps only
//     its members named assetType and instrumentId, in any case. The rest,
//     such as closingPrice, symbol and description, is reference data as of
//     the request, which changes without the transaction changing.
//   - Objects are written with their keys sorted bytewise, arrays in order
//     and strings as encoding/json writes them, without white space.
//   - A number is written as its value: Schwab's numbers are doubles, so
//     0.1 and 0.10000000000000001 are one value sent twice, as are 1.5 and
//     1.50, or 100 and 1e2 (numberValue).
//
// A new key, a changed value and a string "123" for a number 123 each make
// another cut form.
func CutForm(raw []byte) ([]byte, error) {
	v, err := cutTree(raw)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	writeCanonical(&b, v)
	return b.Bytes(), nil
}

// cutDigest returns the SHA-256 of raw's cut form, after a line that names
// the cut version, so that two versions' digests never agree.
func cutDigest(raw []byte) ([32]byte, error) {
	cut, err := CutForm(raw)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("cut"+strconv.Itoa(CutVersion)+"\n"), cut...)), nil
}

// cutTree decodes raw, with each number as written, and cuts each
// instrument.
func cutTree(raw []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("reading a transaction's JSON: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("reading a transaction's JSON: there is more after it")
	}
	return cut(v), nil
}

// cut returns v with every object under an instrument key cut to its
// assetType and instrumentId.
func cut(v any) any {
	switch v := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, e := range v {
			if in, ok := e.(map[string]any); ok && strings.EqualFold(k, "instrument") {
				kept := map[string]any{}
				for ik, ie := range in {
					if strings.EqualFold(ik, "assetType") || strings.EqualFold(ik, "instrumentId") {
						kept[ik] = ie
					}
				}
				out[k] = kept
				continue
			}
			out[k] = cut(e)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = cut(e)
		}
		return out
	}
	return v
}

// writeCanonical writes v, as encoding/json decodes it with UseNumber, in
// one way of all those that mean the same.
func writeCanonical(b *bytes.Buffer, v any) {
	switch v := v.(type) {
	case map[string]any:
		b.WriteByte('{')
		for i, k := range slices.Sorted(maps.Keys(v)) {
			if i > 0 {
				b.WriteByte(',')
			}
			writeString(b, k)
			b.WriteByte(':')
			writeCanonical(b, v[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range v {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCanonical(b, e)
		}
		b.WriteByte(']')
	case string:
		writeString(b, v)
	case json.Number:
		b.WriteString(numberValue(string(v)))
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case nil:
		b.WriteString("null")
	}
}

// writeString writes s as encoding/json does.
func writeString(b *bytes.Buffer, s string) {
	j, _ := json.Marshal(s) // a string always marshals
	b.Write(j)
}

// maxExact is 2^53, up to which a double holds every integer.
const maxExact = 1 << 53

// numberValue returns the value of lit, a JSON number, in one spelling of
// all those that mean it. An integer literal within 2^53 is its digits;
// beyond, it is kept exact, as i and its digits, since a double would lose
// its last digits. Any other literal is the double that strconv.ParseFloat
// reads it as: an integer within 2^53 as its digits, else in Go's shortest
// exponent form. A literal too large for a double is kept exact, as x and
// its digits and exponent.
func numberValue(lit string) string {
	if !strings.ContainsAny(lit, ".eE") {
		n, ok := new(big.Int).SetString(lit, 10)
		if ok && n.CmpAbs(big.NewInt(maxExact)) <= 0 {
			return n.String()
		}
		if ok {
			return "i" + n.String()
		}
	}
	f, err := strconv.ParseFloat(lit, 64)
	switch {
	case err != nil:
		return "x" + exactDecimal(lit)
	case f == math.Trunc(f) && math.Abs(f) <= maxExact:
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'e', -1, 64)
}

// exactDecimal returns lit, a JSON number, as its sign, its digits without
// leading or trailing zeros, e and the exponent that puts the point after
// the last of them: 1.50e400 is 15e399.
func exactDecimal(lit string) string {
	sign := ""
	if rest, ok := strings.CutPrefix(lit, "-"); ok {
		sign, lit = "-", rest
	}
	mant, exp, _ := strings.Cut(strings.ToLower(lit), "e")
	whole, frac, _ := strings.Cut(mant, ".")
	e := new(big.Int)
	if exp != "" {
		e.SetString(exp, 10)
	}
	e.Sub(e, big.NewInt(int64(len(frac))))
	digits := strings.TrimLeft(whole+frac, "0")
	trimmed := strings.TrimRight(digits, "0")
	e.Add(e, big.NewInt(int64(len(digits)-len(trimmed))))
	if trimmed == "" {
		return "0"
	}
	return sign + trimmed + "e" + e.String()
}

// changedPaths returns the key paths whose values differ between two of a
// transaction's raw JSONs, cut, in order: the keys from the top joined by
// dots, without array indexes, as the Q12 probe writes them, so that
// "transferItems.instrument.instrumentId" is any leg's.
func changedPaths(before, after []byte) ([]string, error) {
	b, err := cutTree(before)
	if err != nil {
		return nil, err
	}
	a, err := cutTree(after)
	if err != nil {
		return nil, err
	}
	bv, av := map[string][]string{}, map[string][]string{}
	leafValues(b, "", bv)
	leafValues(a, "", av)
	var changed []string
	for p := range bv {
		if !slices.Equal(bv[p], av[p]) {
			changed = append(changed, p)
		}
	}
	for p := range av {
		if _, ok := bv[p]; !ok {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// leafValues adds to out each value of v under its key path, as its
// canonical form: an array's elements under the array's path, and an empty
// object or array as itself.
func leafValues(v any, path string, out map[string][]string) {
	switch v := v.(type) {
	case map[string]any:
		if len(v) == 0 {
			out[path] = append(out[path], "{}")
		}
		for _, k := range slices.Sorted(maps.Keys(v)) {
			p := keyName(k)
			if path != "" {
				p = path + "." + p
			}
			leafValues(v[k], p, out)
		}
	case []any:
		if len(v) == 0 {
			out[path] = append(out[path], "[]")
		}
		for _, e := range v {
			leafValues(e, path, out)
		}
	default:
		var b bytes.Buffer
		writeCanonical(&b, v)
		out[path] = append(out[path], b.String())
	}
}

// keyName returns a key as a key path shows it: a key of letters, digits
// and underscores, in any script, as it is, and any other quoted, so that
// a path's dots are only its own.
func keyName(key string) string {
	plain := key != "" && strings.IndexFunc(key, func(r rune) bool {
		return r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) < 0
	if plain {
		return key
	}
	return strconv.Quote(key)
}
