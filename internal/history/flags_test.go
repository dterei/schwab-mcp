package history

import (
	"slices"
	"strings"
	"testing"
)

// writeFlagDefs brings flag_def to flagDefs: a new store's first write adds
// every row, and a write after flag_def changed rewrites what differs, a
// single row's meaning as well as a severity, a subject and a missing row at
// once, and deletes a code that flagDefs does not hold.
func TestFlagDefsWritten(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	var want []string
	for _, d := range flagDefs {
		want = append(want, strings.Join([]string{d.code, d.subjects[0], d.severity, d.meaning}, " "))
	}
	slices.Sort(want)
	for _, code := range []string{"gone", "inexact", "rounded", "no_activity_id"} {
		if !slices.ContainsFunc(flagDefs, func(d flagDef) bool { return d.code == code }) {
			t.Fatalf("flagDefs has no %s, which the test changes", code)
		}
	}
	// change changes flag_def in one write, as what, and then has
	// writeFlagDefs bring it back in another.
	change := func(what string, f func(w *wtx) error) {
		t.Helper()
		if err := s.withWrite(t.Context(), "changing flag_def", f); err != nil {
			t.Fatal(err)
		}
		if err := s.withWrite(t.Context(), "writing flag_def", writeFlagDefs); err != nil {
			t.Fatal(err)
		}
		if got := storeStrings(t, s, "SELECT code || ' ' || subject || ' ' || severity || ' ' || meaning FROM flag_def ORDER BY code"); !slices.Equal(got, want) {
			t.Errorf("after %s, flag_def holds %q, want %q", what, got, want)
		}
	}
	change("a new store's first write", func(*wtx) error { return nil })
	change("a meaning changed", func(w *wtx) error {
		_, err := w.exec("UPDATE flag_def SET meaning = 'what it meant once' WHERE code = 'gone'")
		return err
	})
	change("a severity and a subject changed, a row deleted and one added", func(w *wtx) error {
		_, err := w.exec(`UPDATE flag_def SET severity = 'info' WHERE code = 'inexact';
			UPDATE flag_def SET subject = 'file' WHERE code = 'rounded';
			DELETE FROM flag_def WHERE code = 'no_activity_id';
			INSERT INTO flag_def VALUES ('retired', 'entry', 'info', 'a flag that is raised no more')`)
		return err
	})
}
