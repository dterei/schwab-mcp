package history

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// release creates the barrier file that children wait for.
func release(t *testing.T, barrier string) {
	t.Helper()
	if err := os.WriteFile(barrier, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

// queryInt runs a query of one integer on the store at path, through a
// connection of its own.
func queryInt(t *testing.T, path, query string) int {
	t.Helper()
	var n int
	if err := rawDB(t, path).QueryRowContext(t.Context(), query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// Three processes that each open the same new stores in turn, with a 1 ms
// busy timeout, all succeed, and each store has one schema and WAL mode:
// the retries cover the first statements, where SQLite's own wait does not
// apply. Without them the same run fails now and then, which the test
// logs.
func TestOpenNewStoreConcurrently(t *testing.T) {
	t.Parallel()
	files := 20
	if raceOn {
		files = 3
	}
	run := func(t *testing.T, env ...string) (oks int, fails []string, retries int) {
		dir := t.TempDir()
		var paths []string
		for i := range files {
			paths = append(paths, filepath.Join(dir, "s"+strconv.Itoa(i)+".db"))
		}
		barrier := filepath.Join(dir, "go")
		env = append(env, "HISTORY_TEST_FILES="+strings.Join(paths, string(os.PathListSeparator)), "HISTORY_TEST_BARRIER="+barrier)
		var children []*childProcess
		for range 3 {
			children = append(children, startChild(t, "open", env...))
		}
		for _, c := range children {
			c.expect(t, "ready")
		}
		release(t, barrier)
		for _, c := range children {
			for _, line := range c.rest(t) {
				f := strings.Fields(line)
				switch {
				case len(f) == 3 && f[0] == "ok":
					oks++
					n, _ := strconv.Atoi(f[2])
					retries += n
				default:
					fails = append(fails, line)
				}
			}
		}
		for _, p := range paths {
			if len(fails) == 0 {
				if n := queryInt(t, p, "PRAGMA user_version"); n != 1 {
					t.Errorf("%s has schema %d, want 1", filepath.Base(p), n)
				}
				if n := queryInt(t, p, "SELECT count(*) FROM sqlite_schema WHERE type = 'table'"); n != 35 {
					t.Errorf("%s has %d tables, want 35", filepath.Base(p), n)
				}
				if n := queryInt(t, p, "SELECT count(*) FROM store"); n != 1 {
					t.Errorf("%s has %d store rows, want 1", filepath.Base(p), n)
				}
				var mode string
				if err := rawDB(t, p).QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
					t.Errorf("%s is in journal mode %q, %v; want wal", filepath.Base(p), mode, err)
				}
			}
		}
		return oks, fails, retries
	}
	oks, fails, retries := run(t)
	if oks != 3*files || len(fails) > 0 {
		t.Errorf("%d of %d opens succeeded; the others said %q", oks, 3*files, fails)
	}
	t.Logf("with the retries, %d of %d opens succeeded after %d retries", oks, 3*files, retries)
	if !raceOn {
		oks, fails, _ = run(t, "HISTORY_TEST_NORETRY=1")
		t.Logf("without the retries, %d of %d opens succeeded; the others said %q", oks, 3*files, fails)
	}
}

// Two processes that open an older store at once migrate it once, back it
// up once, privately, and derive it again once: the second finds each done
// once it has its turn.
func TestMigrateOnceWithBackup(t *testing.T) {
	t.Parallel()
	path := copyTemplate(t)
	if _, err := rawDB(t, path).ExecContext(t.Context(), `INSERT INTO account (number, created_from, first_seen_at, last_seen_at)
		VALUES ('12345678', 'user', '2026-11-12T15:04:05.678Z', '2026-11-12T15:04:05.678Z')`); err != nil {
		t.Fatal(err)
	}
	barrier := filepath.Join(filepath.Dir(path), "go")
	var children []*childProcess
	for range 2 {
		children = append(children, startChild(t, "migrate", "HISTORY_TEST_STORE="+path, "HISTORY_TEST_BARRIER="+barrier))
	}
	for _, c := range children {
		c.expect(t, "ready")
	}
	release(t, barrier)
	said := map[string]int{}
	for _, c := range children {
		for _, line := range c.rest(t) {
			if b, ok := strings.CutPrefix(line, "warn: backed the store up to "); ok {
				line = "backup " + strings.TrimSuffix(b, " before updating its schema")
			}
			said[line]++
		}
	}
	backups, err := filepath.Glob(path + ".schema1.*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("the backups are %q, %v; want one", backups, err)
	}
	want := map[string]int{
		"opened":               2,
		"backup " + backups[0]: 1,
		"warn: updated the store's derived data for this schwabtx (rules 1 -> 2)": 1,
	}
	if len(said) != len(want) {
		t.Errorf("the children said %v, want %v", said, want)
	}
	for line, n := range want {
		if said[line] != n {
			t.Errorf("the children said %q %d times, want %d; they said %v", line, said[line], n, said)
		}
	}
	if runtime.GOOS != "windows" {
		if m := mode(t, backups[0]); m != 0o600 {
			t.Errorf("the backup has mode %v, want 0600", m)
		}
	}
	for q, want := range map[string]int{
		"PRAGMA user_version":                                     2,
		"SELECT derived_with FROM store":                          2,
		"SELECT count(*) FROM sqlite_schema WHERE name = 'later'": 1,
		"SELECT count(*) FROM account":                            1,
	} {
		if got := queryInt(t, path, q); got != want {
			t.Errorf("the store: %s = %d, want %d", q, got, want)
		}
	}
	for q, want := range map[string]int{
		"PRAGMA user_version":            1,
		"SELECT derived_with FROM store": 1,
		"SELECT count(*) FROM account":   1,
	} {
		if got := queryInt(t, backups[0], q); got != want {
			t.Errorf("the backup: %s = %d, want %d", q, got, want)
		}
	}
}

// Two processes that open a store of older rules, or of an older calendar,
// at once, and have both read its versions before either writes, derive it
// again, or recompute its market dates, once: the second reads the versions
// again inside its write, and finds the work done.
func TestUpdateOnce(t *testing.T) {
	t.Parallel()
	for raise, warning := range map[string]string{
		"rules":    "warn: updated the store's derived data for this schwabtx (rules " + strconv.Itoa(deriveVersion) + " -> " + strconv.Itoa(deriveVersion+1) + ")",
		"calendar": "warn: recomputed market dates for calendar " + strconv.Itoa(calendarVersion+1),
	} {
		t.Run(raise, func(t *testing.T) {
			t.Parallel()
			path := copyTemplate(t)
			var children []*childProcess
			var barriers []string
			for i := range 2 {
				barriers = append(barriers, filepath.Join(filepath.Dir(path), "go"+strconv.Itoa(i)))
				children = append(children, startChild(t, "update", "HISTORY_TEST_STORE="+path, "HISTORY_TEST_BARRIER="+barriers[i],
					"HISTORY_TEST_RAISE="+raise))
			}
			for _, c := range children {
				c.expect(t, "read")
			}
			said := map[string]int{}
			for i, c := range children {
				release(t, barriers[i])
				for _, line := range c.rest(t) {
					said[line]++
				}
			}
			if want := map[string]int{"opened": 2, warning: 1}; !maps.Equal(said, want) {
				t.Errorf("the children said %v, want %v", said, want)
			}
			q, want := "SELECT calendar_version FROM store", calendarVersion+1
			if raise == "rules" {
				q, want = "SELECT derived_with FROM store", deriveVersion+1
			}
			if got := queryInt(t, path, q); got != want {
				t.Errorf("the store: %s = %d, want %d", q, got, want)
			}
		})
	}
}

// Three processes making 20 writes each, with a 1 ms busy timeout, all
// land. The parent holds the write lock until each has been refused once,
// so each first write must retry; no later one is asked to.
func TestWritersInOtherProcesses(t *testing.T) {
	t.Parallel()
	path := copyTemplate(t)
	barrier := filepath.Join(filepath.Dir(path), "go")
	var children []*childProcess
	for i := range 3 {
		children = append(children, startChild(t, "write", "HISTORY_TEST_STORE="+path, "HISTORY_TEST_BARRIER="+barrier,
			"HISTORY_TEST_ID=c"+strconv.Itoa(i)))
	}
	for _, c := range children {
		c.expect(t, "ready")
	}
	conn, err := rawDB(t, path).Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	release(t, barrier)
	for _, c := range children {
		c.expect(t, "busy")
	}
	if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	for i, c := range children {
		first := strings.Fields(c.expect(t, "first "))
		if n, err := strconv.Atoi(first[1]); err != nil || n < 1 {
			t.Errorf("child %d's first write retried %s times, want at least once", i, first[1])
		}
		c.expect(t, "done")
		c.rest(t)
	}
	if n := queryInt(t, path, "SELECT count(*) FROM account"); n != 60 {
		t.Errorf("the store holds %d accounts, want 60", n)
	}
}

// A process killed inside a write transaction leaves nothing of it, and
// the next write goes ahead on a store that SQLite finds whole.
func TestKilledWriter(t *testing.T) {
	t.Parallel()
	path := copyTemplate(t)
	c := startChild(t, "hold", "HISTORY_TEST_STORE="+path)
	c.expect(t, "inside")
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = c.cmd.Wait()
	s, err := Open(t.Context(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.withWrite(t.Context(), "adding an account", func(w *wtx) error { return insertAccount(w, "12345678") }); err != nil {
		t.Fatal(err)
	}
	var numbers []string
	if err := s.withRead(t.Context(), func(r *rtx) error {
		numbers = nil
		rows, err := r.query("SELECT number FROM account ORDER BY number")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			numbers = append(numbers, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(numbers) != 1 || numbers[0] != "12345678" {
		t.Errorf("the store holds the accounts %q, want only 12345678", numbers)
	}
	var check string
	if err := s.r.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Errorf("integrity_check: %q, %v", check, err)
	}
}
