package history

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// addAccounts adds an account for each number to the store, in one write.
func addAccounts(t *testing.T, s *Store, numbers ...string) {
	t.Helper()
	if err := s.withWrite(t.Context(), "adding accounts", func(w *wtx) error {
		for _, n := range numbers {
			if err := insertAccount(w, n); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A backup is a private copy of the store as of a commit, taken while
// another handle writes to the store: one database file that passes the
// integrity check and holds every account the writes had added before it
// and none of those after. A file that is there, or that is one of the
// store's own by name, is refused and left as it was.
func TestBackup(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	addAccounts(t, s, "10000000")
	other, err := Open(t.Context(), Options{Path: s.Path(), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	dir := t.TempDir()
	file := filepath.Join(dir, "backup.db")
	ctx, stop := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 1; ctx.Err() == nil; i++ {
			err := other.withWrite(ctx, "adding an account", func(w *wtx) error { return insertAccount(w, strconv.Itoa(10000000+i)) })
			if err != nil && ctx.Err() == nil {
				t.Error(err)
				return
			}
		}
	})
	before := storeInt(t, s, "SELECT count(*) FROM account")
	err = s.Backup(t.Context(), file)
	after := storeInt(t, s, "SELECT count(*) FROM account")
	stop()
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && mode(t, file) != 0o600 {
		t.Errorf("the backup's mode is %v, want 0600", mode(t, file))
	}
	if got := listing(t, dir); !slices.Equal(got, []string{"backup.db"}) {
		t.Errorf("the backup's directory holds %q, want the backup alone", got)
	}
	if h := header(t, file); h != [2]byte{1, 1} {
		t.Errorf("the backup's header says journal modes %v, want a file without a journal", h)
	}
	if got := queryInt(t, file, "SELECT count(*) FROM account"); int64(got) < before || int64(got) > after {
		t.Errorf("the backup holds %d accounts, not between %d and %d", got, before, after)
	}
	if got := queryInt(t, file, "SELECT count(*) FROM pragma_integrity_check WHERE integrity_check <> 'ok'"); got != 0 {
		t.Errorf("the backup fails the integrity check")
	}

	if err := s.Backup(t.Context(), file); err == nil || !strings.Contains(err.Error(), file+" exists; choose a new name") {
		t.Errorf("a backup over a file that is there: %v", err)
	}
	for _, name := range []string{s.Path(), s.Path() + "-wal", s.Path() + ".schema1.20260101T000000Z.bak"} {
		err := s.Backup(t.Context(), name)
		if err == nil || !strings.Contains(err.Error(), "is the store itself (or one of its files); choose another name") {
			t.Errorf("a backup to %s: %v", filepath.Base(name), err)
		}
	}
	if got := listing(t, filepath.Dir(s.Path())); slices.ContainsFunc(got, func(n string) bool { return strings.HasSuffix(n, ".bak") }) {
		t.Errorf("a refused backup left a file: %q", got)
	}
}

// A store is restored from a backup through SQLite, never by a file copy,
// so what a killed writer committed to the -wal alone is replaced too and
// a connection that another process holds stays valid and sees the
// backup's content: the backup's accounts, an intact store, for the
// connection held across the restore and a new one alike. The backup,
// being of an older schema and rules than the build that restores it, is
// brought up to that build's first, in a directory that is gone after,
// and the store migrated before it is replaced.
func TestRestore(t *testing.T) {
	t.Parallel()
	path := copyTemplate(t)
	s, err := Open(t.Context(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	addAccounts(t, s, "20000001", "20000002")
	backup := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(t.Context(), backup); err != nil {
		t.Fatal(err)
	}
	addAccounts(t, s, "20000003")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	c := startChild(t, "commit", "HISTORY_TEST_STORE="+path)
	c.expect(t, "committed")
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.cmd.Wait()
	if !exists(path + "-wal") {
		t.Fatal("the killed writer left no -wal")
	}
	held := rawDB(t, path)
	if n := countAccounts(t, held); n != 4 {
		t.Fatalf("before the restore the store holds %d accounts, want 4, the killed writer's among them", n)
	}

	r := startChild(t, "restore", "HISTORY_TEST_STORE="+path, "HISTORY_TEST_BACKUP="+backup)
	lines := r.rest(t)
	if !slices.Contains(lines, "restored 4 accounts, then 2; updated true") {
		t.Errorf("the restore said %q", lines)
	}
	if !slices.ContainsFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "warn: restored the store from "+backup+" (taken ") && strings.HasSuffix(l, "); what was recorded since is gone")
	}) {
		t.Errorf("the restore warned %q", lines)
	}
	if n := countAccounts(t, held); n != 2 {
		t.Errorf("the connection held across the restore sees %d accounts, want the backup's 2", n)
	}
	if n := countAccounts(t, rawDB(t, path)); n != 2 {
		t.Errorf("a new connection sees %d accounts, want the backup's 2", n)
	}
	for query, want := range map[string]int{
		"SELECT count(*) FROM pragma_integrity_check WHERE integrity_check <> 'ok'": 0,
		"PRAGMA user_version": 2,
		"SELECT count(*) FROM sqlite_schema WHERE name = 'later'":                1,
		"SELECT derived_with FROM store":                                         DeriveVersion + 1,
		"SELECT count(*) FROM account WHERE number IN ('20000003', 'committed')": 0,
	} {
		if got := queryInt(t, path, query); got != want {
			t.Errorf("after the restore, %s: %d, want %d", query, got, want)
		}
	}
	if got := listing(t, filepath.Dir(path)); slices.ContainsFunc(got, func(n string) bool { return strings.HasPrefix(n, ".restore-") }) {
		t.Errorf("the restore left its copy of the backup: %q", got)
	}
}

// Restore in one process: a dry run reports and changes nothing, nor
// makes a missing store; a restore of a backup of this build's versions,
// one made before a schema changed and kept beside the store, brings the
// store back to it and says what it held before and after. Refused, each
// changing nothing: the store itself or its journal named as the backup; a
// missing database beside its -wal (a store moved aside without its
// journal); another program's file; a file that is not SQLite's; a
// damaged backup, and one whose damage only the integrity check finds; a
// file marked as a store that has no schema; and a backup of a newer
// schema or newer rules.
func TestRestoreRefuses(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	s := newStore(t, Options{})
	addAccounts(t, s, "30000001")
	// The backup is one made before a schema changed, beside the store,
	// as Open makes them and as the store's damaged message suggests.
	made := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, made); err != nil {
		t.Fatal(err)
	}
	backup := s.Path() + ".schema1.20261101T000000Z.bak"
	if err := os.WriteFile(backup, readFile(t, made), 0o600); err != nil {
		t.Fatal(err)
	}
	addAccounts(t, s, "30000002", "30000003")
	var warned []string
	o := Options{Path: s.Path(), Now: fixedNow, Warn: func(m string) { warned = append(warned, m) }}

	missing := Options{Path: filepath.Join(t.TempDir(), "history.db")}
	if r, err := Restore(ctx, missing, backup, true); err != nil || r.After.Accounts != 1 || r.Before != (Counts{}) {
		t.Errorf("a dry run into a missing store: %+v, %v", r, err)
	}
	if exists(missing.Path) {
		t.Error("a dry run made a store")
	}
	r, err := Restore(ctx, o, backup, true)
	if err != nil || r.Before.Accounts != 3 || r.After.Accounts != 1 || r.Updated || len(warned) > 0 {
		t.Errorf("a dry run: %+v, %v, warned %q", r, err, warned)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM account"); n != 3 {
		t.Errorf("after a dry run the store holds %d accounts, want 3", n)
	}
	r, err = Restore(ctx, o, backup, false)
	if err != nil || r.Before.Accounts != 3 || r.After.Accounts != 1 || r.Updated || r.Taken.IsZero() {
		t.Errorf("a restore: %+v, %v", r, err)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "restored the store from "+backup) {
		t.Errorf("the restore warned %q", warned)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM account"); n != 1 {
		t.Errorf("after the restore the open store reads %d accounts, want 1", n)
	}
	addAccounts(t, s, "30000004")

	orphan := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(orphan+"-wal", []byte("a journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(ctx, Options{Path: orphan}, backup, false); !errors.Is(err, ErrOrphanJournal) {
		t.Errorf("a restore into a store moved aside without its -wal: %v", err)
	}

	dir := t.TempDir()
	foreign := filepath.Join(dir, "foreign.db")
	if _, err := rawDB(t, foreign).ExecContext(ctx, "CREATE TABLE t (x)"); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(garbage, []byte(strings.Repeat("not a database ", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	damaged := filepath.Join(dir, "damaged.db")
	b := readFile(t, backup)
	page := 4096
	for i := 2 * page; i < len(b); i++ {
		b[i] = 0xA5
	}
	if err := os.WriteFile(damaged, b, 0o600); err != nil {
		t.Fatal(err)
	}
	changed := func(name, statement string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, readFile(t, backup), 0o600); err != nil {
			t.Fatal(err)
		}
		db := rawDB(t, p)
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
		db.Close()
		return p
	}
	newerSchema := changed("schema.db", "PRAGMA user_version = 99")
	newerRules := changed("rules.db", "UPDATE store SET derived_with = 99")
	// An index of an empty table that points at the accounts' pages: the
	// accounts read, and only the integrity check finds the damage.
	index := changed("index.db", `PRAGMA writable_schema = ON;
		UPDATE sqlite_schema SET rootpage = (SELECT rootpage FROM sqlite_schema WHERE name = 'account') WHERE name = 'txn_time'`)
	unversioned := filepath.Join(dir, "unversioned.db")
	if _, err := rawDB(t, unversioned).ExecContext(ctx, "CREATE TABLE account (x); PRAGMA application_id = "+strconv.Itoa(applicationID)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ backup, want string }{
		{s.Path(), "is the store itself (or one of its files), not a backup of it"},
		{s.Path() + "-wal", "is the store itself (or one of its files), not a backup of it"},
		{foreign, foreign + " is not a schwabtx store, so it cannot be restored"},
		{garbage, garbage + " is damaged or not an SQLite file"},
		{damaged, damaged + " is damaged"},
		{index, index + " is damaged (integrity_check: "},
		{unversioned, unversioned + " is not a schwabtx store, so it cannot be restored"},
		{newerSchema, newerSchema + " was written by a newer schwabtx (schema 99, rules"},
		{newerRules, newerRules + " was written by a newer schwabtx (schema 1, rules 99"},
	} {
		if _, err := Restore(ctx, o, c.backup, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("restoring %s: %v, want %q", filepath.Base(c.backup), err, c.want)
		}
	}
	if n := storeInt(t, s, "SELECT count(*) FROM account"); n != 2 {
		t.Errorf("after the refusals the store holds %d accounts, want 2", n)
	}
	if len(warned) != 1 {
		t.Errorf("the refusals warned %q", warned[1:])
	}
}
