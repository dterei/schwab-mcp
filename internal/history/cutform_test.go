package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// listRow returns the fixture list's row of activity 73508261946, as
// Schwab sent it, which transaction.json holds as the by-id endpoint sent
// it.
func listRow(t *testing.T) []byte {
	t.Helper()
	var rows []json.RawMessage
	if err := json.Unmarshal(schwabtest.Fixture(t, "transactions.json"), &rows); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if bytes.Contains(r, []byte(`"activityId": 73508261946`)) {
			return r
		}
	}
	t.Fatal("transactions.json holds no activity 73508261946")
	return nil
}

// mustCut returns raw's cut form and digest.
func mustCut(t *testing.T, raw []byte) (string, [32]byte) {
	t.Helper()
	c, err := CutForm(raw)
	if err != nil {
		t.Fatalf("CutForm(%s): %v", raw, err)
	}
	d, err := cutDigest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(c), d
}

// replaceOnce replaces old in s, which must hold it once, with with.
func replaceOnce(t *testing.T, s []byte, old, with string) []byte {
	t.Helper()
	if n := bytes.Count(s, []byte(old)); n != 1 {
		t.Fatalf("the row holds %q %d times, want once", old, n)
	}
	return bytes.Replace(s, []byte(old), []byte(with), 1)
}

// The fixture's list row and by-id row of one activity, whose bytes differ
// (another closingPrice, assetType sent twice, other white space), have one
// cut form; so do the row with another closingPrice, a CUSIP for its
// symbol, other white space or another order of keys. A changed status or
// a new key makes another, and the changed key paths say which.
func TestCutForm(t *testing.T) {
	t.Parallel()
	list := listRow(t)
	byID := schwabtest.Fixture(t, "transaction.json")
	if bytes.Equal(list, byID) {
		t.Fatal("the list row and the by-id row are the same bytes")
	}
	want, digest := mustCut(t, list)
	if got, d := mustCut(t, byID); got != want || d != digest {
		t.Errorf("the by-id row cuts to\n%s\nwant the list row's\n%s", got, want)
	}
	var tree map[string]any
	if err := json.Unmarshal(list, &tree); err != nil {
		t.Fatal(err)
	}
	sorted, err := json.Marshal(tree) // keys in another order, no white space
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, list); err != nil {
		t.Fatal(err)
	}
	same := map[string][]byte{
		"another closingPrice": replaceOnce(t, list, `"closingPrice": 71.59`, `"closingPrice": 73.5`),
		"a CUSIP for a symbol": replaceOnce(t, list, `"symbol": "BND"`, `"symbol": "921937835"`),
		"compact":              compact.Bytes(),
		"keys sorted":          sorted,
	}
	for name, raw := range same {
		if got, d := mustCut(t, raw); got != want || d != digest {
			t.Errorf("%s: the cut form is\n%s\nwant\n%s", name, got, want)
		}
		if changed, err := changedPaths(list, raw); err != nil || len(changed) != 0 {
			t.Errorf("%s: changed paths %q, %v; want none", name, changed, err)
		}
	}
	differ := map[string]struct {
		raw     []byte
		changed []string
	}{
		"status":        {replaceOnce(t, list, `"status": "VALID"`, `"status": "PENDING"`), []string{"status"}},
		"a new key":     {replaceOnce(t, list, `"subAccount": "CASH",`, `"subAccount": "CASH", "qualifiedDividend": false,`), []string{"qualifiedDividend"}},
		"a cost":        {replaceOnce(t, list, `"cost": -2.7`, `"cost": -2.71`), []string{"transferItems.cost"}},
		"an instrument": {replaceOnce(t, list, `"instrumentId": 4417302`, `"instrumentId": 4417303`), []string{"transferItems.instrument.instrumentId"}},
		"a key gone":    {replaceOnce(t, list, `"positionId": 3926184811,`, ``), []string{"positionId"}},
	}
	for name, c := range differ {
		if got, d := mustCut(t, c.raw); got == want || d == digest {
			t.Errorf("%s: the cut form did not change", name)
		}
		if changed, err := changedPaths(list, c.raw); err != nil || !slices.Equal(changed, c.changed) {
			t.Errorf("%s: changed paths %q, %v; want %q", name, changed, err, c.changed)
		}
	}
}

// The cut form of a small row is written out exactly, and its digest is
// the SHA-256 of the cut version's line and the form.
func TestCutFormWritten(t *testing.T) {
	t.Parallel()
	raw := `{ "b": [1, "x", true, null, {}], "a": {"instrument": {"symbol": "X", "AssetType": "EQUITY", "instrumentId": 5,
		"closingPrice": 1.5, "optionDeliverables": [{"closingPrice": 2}]}, "s": "é<\u00e9"}, "a": 1, "a": {"n": 1.50, "z": 1e2}}`
	want := `{"a":{"n":1.5e+00,"z":100},"b":[1,"x",true,null,{}]}`
	got, digest := mustCut(t, []byte(raw))
	if got != want {
		t.Errorf("CutForm = %s\nwant      %s", got, want)
	}
	if digest != sha256.Sum256([]byte("cut1\n"+want)) {
		t.Error("the digest is not the SHA-256 of cut1 and the form")
	}
	got, _ = mustCut(t, []byte(`{"instrument": {"symbol": "X", "AssetType": "EQUITY", "instrumentId": 5}, "s": "é<\u00e9", "list": [{"Instrument": {"description": "D", "assetType": "OPTION"}}]}`))
	want = `{"instrument":{"AssetType":"EQUITY","instrumentId":5},"list":[{"Instrument":{"assetType":"OPTION"}}],"s":"é\u003cé"}`
	if got != want {
		t.Errorf("CutForm = %s\nwant      %s", got, want)
	}
	for _, bad := range []string{``, `{`, `{"a": 1} {"b": 2}`, `{"a": 1}x`, `[1,]`} {
		if _, err := CutForm([]byte(bad)); err == nil {
			t.Errorf("CutForm(%q) did not fail", bad)
		}
	}
}

// The spellings of one number cut to one value: the double that a literal
// denotes, integers beyond 2^53 kept exact, and literals too large for a
// double kept as decimals; other numbers, and a string of digits, stay
// apart.
func TestNumberValue(t *testing.T) {
	t.Parallel()
	same := map[string][]string{
		"0":                     {"0", "-0", "0.0", "-0.0", "0e5", "0E-5", "1e-400"},
		"1.5e+00":               {"1.5", "1.50", "15e-1", "0.15E1"},
		"100":                   {"100", "1e2", "100.0", "1E+2", "10000e-2"},
		"1e-01":                 {"0.1", "0.10000000000000001", "1e-1"},
		"-2.7e+00":              {"-2.7", "-2.70"},
		"9007199254740992":      {"9007199254740992", "9007199254740992.0", "9007199254740993.0"},
		"-9007199254740992":     {"-9007199254740992", "-9.007199254740992e15"},
		"i9007199254740993":     {"9007199254740993"},
		"i12345678901234567890": {"12345678901234567890"},
		"9.007199254740994e+15": {"9007199254740994.0", "9.007199254740994e15"},
		"x1e400":                {"1e400", "10e399", "1.0E400", "0.001e403"},
		"x-15e399":              {"-1.5e400", "-1.50e400"},
	}
	for want, lits := range same {
		for _, lit := range lits {
			if got := numberValue(lit); got != want {
				t.Errorf("numberValue(%s) = %s, want %s", lit, got, want)
			}
		}
	}
	apart := [][2]string{
		{"9007199254740993", "9007199254740993.0"}, // an integer beyond 2^53, and the double a decimal denotes
		{"9007199254740994", "9007199254740994.0"},
		{"0.1", "0.2"},
		{"1e400", "2e400"},
		{"1e400", "1e401"},
		{"1", "-1"},
	}
	for _, p := range apart {
		if numberValue(p[0]) == numberValue(p[1]) {
			t.Errorf("%s and %s cut to one value, %s", p[0], p[1], numberValue(p[0]))
		}
	}
	a, _ := mustCut(t, []byte(`{"amount": "123"}`))
	b, _ := mustCut(t, []byte(`{"amount": 123}`))
	if a == b || !strings.Contains(a, `"123"`) {
		t.Errorf("a string and a number of the same digits cut to %s and %s", a, b)
	}
}
