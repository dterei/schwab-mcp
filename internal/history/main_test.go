package history

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// template is a new store that TestMain made once through Open, which each
// store test copies rather than creating the schema again: that costs about
// a quarter of a second under -race.
var template string

// TestMain makes the template store, or, when a test runs the test binary
// with HISTORY_TEST_CHILD set, acts as another process that uses a store
// (child). `go test` never sets the variable, and a child makes no template.
func TestMain(m *testing.M) {
	if mode := os.Getenv("HISTORY_TEST_CHILD"); mode != "" {
		os.Exit(child(mode))
	}
	dir, err := os.MkdirTemp("", "history-template-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	template = filepath.Join(dir, "history.db")
	s, err := Open(context.Background(), Options{Path: template, Now: fixedNow})
	if err == nil {
		// The last connection to close takes the -wal and -shm with it, so
		// the database is the whole store.
		err = s.Close()
	}
	code := 1
	if err != nil {
		fmt.Fprintln(os.Stderr, "making the template store:", err)
	} else {
		code = m.Run()
	}
	os.RemoveAll(dir)
	os.Exit(code)
}

// testNow is the time that the tests' stores take for now.
var testNow = time.Date(2026, time.November, 12, 15, 4, 5, 678_000_000, time.UTC)

func fixedNow() time.Time { return testNow }

// newStore opens a copy of the template store in a directory of its own,
// with a fixed clock unless o says otherwise, and closes it when the test
// ends.
func newStore(t *testing.T, o Options) *Store {
	t.Helper()
	o.Path = copyTemplate(t)
	if o.Now == nil {
		o.Now = fixedNow
	}
	s, err := Open(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// copyTemplate copies the template store to history.db in a new directory,
// readable only by you, and returns its path.
func copyTemplate(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "history.db")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// rawDB opens path as another program would, with a connection that writes
// without the store's checks, and closes it when the test ends.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(path, "_pragma=busy_timeout(5000)"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

// insertAccount adds an account with number to the store, as a write
// would.
func insertAccount(w *wtx, number string) error {
	_, err := w.exec(`INSERT INTO account (number, created_from, first_seen_at, last_seen_at)
		VALUES (?, 'user', ?, ?)`, number, instant(w.now), instant(w.now))
	return err
}

// countAccounts returns how many accounts the store at path holds.
func countAccounts(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM account").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// child is the test binary acting as another process, as HISTORY_TEST_CHILD
// says:
//
//   - "open" opens and closes each store of HISTORY_TEST_FILES in turn and
//     says "ok" or "fail" of each, and at the end how many attempts it
//     retried;
//   - "write" opens HISTORY_TEST_STORE and makes 20 writes, saying "busy"
//     at its first retry and how many retries its first write needed;
//   - "migrate" opens HISTORY_TEST_STORE as a build with one more schema and
//     newer rules would, saying each warning;
//   - "update" opens HISTORY_TEST_STORE as a build with newer rules or a
//     newer calendar would, as HISTORY_TEST_RAISE says ("rules" or
//     "calendar"), saying "read" once it has read the store's versions and
//     then waiting for the barrier before it brings its derived data up to
//     date, and saying each warning;
//   - "hold" opens HISTORY_TEST_STORE, adds an account inside a write, says
//     "inside", and waits there to be killed;
//   - "journal" makes HISTORY_TEST_STORE another program's SQLite file in
//     rollback-journal mode, changes it inside a transaction until the file
//     holds changes that the journal can undo, says "inside", and waits
//     there to be killed.
//
// Each says "ready" once it has done what comes before the barrier, the file
// HISTORY_TEST_BARRIER, and waits for that file before going on. It
// uses a 1 ms busy timeout, so that a busy store makes it retry at once.
func child(mode string) int {
	ctx := context.Background()
	store := os.Getenv("HISTORY_TEST_STORE")
	o := Options{Path: store, Now: time.Now, BusyTimeout: time.Millisecond, LockWait: time.Minute}
	testNoRetry = os.Getenv("HISTORY_TEST_NORETRY") != ""
	fail := func(err error) int {
		fmt.Println("fail", err)
		return 1
	}
	switch mode {
	case "open":
		fmt.Println("ready")
		waitBarrier()
		for f := range strings.SplitSeq(os.Getenv("HISTORY_TEST_FILES"), string(os.PathListSeparator)) {
			o.Path = f
			var retries int64
			s, err := Open(ctx, o)
			if err == nil {
				retries = s.retries.Load()
				err = s.Close()
			}
			if err != nil {
				fmt.Println("fail", f, err)
				continue
			}
			fmt.Println("ok", f, retries)
		}
		return 0
	case "write":
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		defer s.Close()
		var once sync.Once
		testHookRetry = func() { once.Do(func() { fmt.Println("busy") }) }
		fmt.Println("ready")
		waitBarrier()
		id := os.Getenv("HISTORY_TEST_ID")
		for i := range 20 {
			before := s.retries.Load()
			if err := s.withWrite(ctx, "adding an account", func(w *wtx) error {
				return insertAccount(w, id+"-"+strconv.Itoa(i))
			}); err != nil {
				return fail(err)
			}
			if i == 0 {
				fmt.Println("first", s.retries.Load()-before)
			}
		}
		fmt.Println("done")
		return 0
	case "migrate":
		migrations = append(migrations, migration{sql: "CREATE TABLE later (x INTEGER) STRICT;"})
		deriveVersion++
		o.Warn = func(m string) { fmt.Println("warn:", m) }
		fmt.Println("ready")
		waitBarrier()
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		fmt.Println("opened")
		if err := s.Close(); err != nil {
			return fail(err)
		}
		return 0
	case "update":
		if os.Getenv("HISTORY_TEST_RAISE") == "rules" {
			deriveVersion++
		} else {
			calendarVersion++
		}
		o.Warn = func(m string) { fmt.Println("warn:", m) }
		testHookBeforeUpdate = func() {
			fmt.Println("read")
			waitBarrier()
		}
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		fmt.Println("opened")
		if err := s.Close(); err != nil {
			return fail(err)
		}
		return 0
	case "hold":
		s, err := Open(ctx, o)
		if err != nil {
			return fail(err)
		}
		err = s.withWrite(ctx, "adding an account", func(w *wtx) error {
			if err := insertAccount(w, "killed"); err != nil {
				return err
			}
			fmt.Println("inside")
			_, _ = io.Copy(io.Discard, os.Stdin)
			return nil
		})
		return fail(fmt.Errorf("the write ended (%w) instead of waiting to be killed", err))
	case "journal":
		db, err := sql.Open("sqlite", dsn(store, "_pragma=busy_timeout(5000)"))
		if err != nil {
			return fail(err)
		}
		db.SetMaxOpenConns(1)
		// A cache of two pages makes the transaction write the pages it
		// changed to the file before it commits, their old contents in the
		// journal. In a process with other connections open, SQLite can
		// lend the cache their pages instead.
		if _, err := db.ExecContext(ctx, `CREATE TABLE t (x);
			WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 200) INSERT INTO t SELECT zeroblob(1000) FROM n;
			PRAGMA cache_size = 2; BEGIN; UPDATE t SET x = zeroblob(1001)`); err != nil {
			return fail(err)
		}
		fmt.Println("inside")
		_, _ = io.Copy(io.Discard, os.Stdin)
		return fail(errors.New("the transaction ended instead of waiting to be killed"))
	}
	return fail(fmt.Errorf("unknown mode %q", mode))
}

// waitBarrier waits for the file HISTORY_TEST_BARRIER, if one is named.
func waitBarrier() {
	b := os.Getenv("HISTORY_TEST_BARRIER")
	if b == "" {
		return
	}
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(b); err == nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// A childProcess is the test binary running as child.
type childProcess struct {
	cmd   *exec.Cmd
	lines chan string // what it says, line by line; closed when it ends
	stdin io.WriteCloser
}

// startChild runs the test binary as a child in mode, with env added to its
// environment.
func startChild(t *testing.T, mode string, env ...string) *childProcess {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{"HISTORY_TEST_CHILD=" + mode}, env...)...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &childProcess{cmd: cmd, lines: make(chan string, 100), stdin: stdin}
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return c
}

// expect waits for the child to say a line that starts with prefix, and
// returns it; what it says before is logged.
func (c *childProcess) expect(t *testing.T, prefix string) string {
	t.Helper()
	timeout := time.After(time.Minute)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				t.Fatalf("the child ended without saying %q", prefix)
			}
			if strings.HasPrefix(line, prefix) {
				return line
			}
			t.Logf("the child said %q", line)
		case <-timeout:
			t.Fatalf("the child did not say %q within a minute", prefix)
		}
	}
}

// rest returns everything the child says until it ends, and fails the test
// if it does not end well.
func (c *childProcess) rest(t *testing.T) []string {
	t.Helper()
	var lines []string
	for line := range c.lines {
		lines = append(lines, line)
	}
	if err := c.cmd.Wait(); err != nil {
		t.Errorf("the child: %v; it said %q", err, lines)
	}
	return lines
}
