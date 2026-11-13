package history

import (
	"strings"
	"testing"
)

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

// A command in a flag's detail, quoted for a POSIX shell, is quoted again
// word by word for another shell: a key with spaces and a quote in it, as
// cmd.exe on Windows reads it in double quotes.
func TestRequoted(t *testing.T) {
	t.Parallel()
	a, b := "K:s:FILE:XYZ 09/18/2026 50.00 C@2026-10-01", "K:s:FILE:O'NEIL@2026-10-02"
	command := "schwabtx edit " + shellQuote(a) + " -differ " + shellQuote(b)
	double := func(s string) string {
		if strings.ContainsAny(s, " '") {
			return `"` + s + `"`
		}
		return s
	}
	if got, want := requoted(command, double), `schwabtx edit "`+a+`" -differ "`+b+`"`; got != want {
		t.Errorf("requoted(%q) = %s, want %s", command, got, want)
	}
	if got := requoted(command, shellQuote); got != command {
		t.Errorf("requoted(%q) for a POSIX shell = %s", command, got)
	}
}

// Status counts the runs of an account's coverage by its days: coverages
// that overlap or meet are one run, whichever their sources, and only a
// day that none of them holds parts two runs.
func TestStatusRuns(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	addAccounts(t, s, "61803398")
	if err := s.withWrite(t.Context(), "writing coverage", func(w *wtx) error {
		for _, c := range [][3]string{
			{"file", "2026-01-01", "2026-03-15"},
			{"file_filtered", "2026-02-06", "2026-03-15"},
			{"file_unknown", "2026-03-16", "2026-03-31"},
			{"file", "2026-05-01", "2026-05-31"},
			{"api", "2026-02-01", "2026-02-10"},
			{"api_file", "2026-02-11", "2026-02-20"},
		} {
			if _, err := w.exec("INSERT INTO coverage (account_id, source, from_date, to_date) VALUES (1, ?, ?, ?)", c[0], c[1], c[2]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(t.Context(), StatusOptions{Account: "A1"})
	if err != nil || len(st.Accounts) != 1 {
		t.Fatalf("Status of A1: %+v, %v", st, err)
	}
	if a := st.Accounts[0]; a.Files != (Span{"2026-01-01", "2026-05-31", 2}) || a.API != (Span{"2026-02-01", "2026-02-20", 1}) ||
		a.Snapshots != (Span{}) {
		t.Errorf("the account's spans are files %+v, API %+v, snapshots %+v", a.Files, a.API, a.Snapshots)
	}
}
