package history

import "testing"

// A ref that names nothing is said back as it was given, since refs are
// identifiers that hold no account number (5.3 of the design): an API
// transaction's 11-digit activity ID, a file transaction's digest and row.
// Only what may be free text is masked: the symbol of a key of a symbol,
// whose export date stays, and what is not a ref at all.
func TestFindSaysRefsBack(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	for ref, want := range map[string]string{
		"T73508261999":                     "no transaction T73508261999 in the store",
		"t73508261999":                     "no transaction t73508261999 in the store",
		"C12345678.1":                      "no file transaction C12345678.1 in the store",
		"L12345678.2":                      "no lot L12345678.2 in the store",
		"S12345678":                        "no snapshot S12345678 in the store",
		"A12345678":                        "no account A12345678 in the store",
		"K:i:12345678901":                  "no instrument key K:i:12345678901 in the store",
		"K:s:FILE:XYZ 12345678@2026-10-01": "no instrument key K:s:FILE:XYZ ...678@2026-10-01 in the store",
		"K:s:EQUITY:12345678":              "no instrument key K:s:EQUITY:...678 in the store",
		"12345678":                         `"...678" is not a ref: a ref is A3, T73508261941, C3a9e21b0.17, F3a9e21b0, L5c1d2e3f.4, S812 or K:i:1973757747`,
	} {
		if _, err := s.Find(t.Context(), ref, ""); err == nil || err.Error() != want {
			t.Errorf("Find(%q): %v, want %s", ref, err, want)
		}
	}
}
