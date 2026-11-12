package history

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// warnings collects what a store warns of.
type warnings struct {
	mu   sync.Mutex
	said []string
}

func (w *warnings) warn(m string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.said = append(w.said, m)
}

func (w *warnings) all() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.said)
}

// mode returns the permissions of the file at path.
func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

// listing returns the names in dir.
func listing(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// A new store is made readable only by you, its -wal and -shm too, in a
// directory readable only by you; a store that others can read is made
// private once Open has seen that it is a store, and says so.
func TestOpenMakesItPrivate(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows gives files their folder's permissions")
	}
	t.Run("new", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "schwabtx", "history.db")
		s, err := Open(t.Context(), Options{Path: path, Now: fixedNow})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		if err := s.withWrite(t.Context(), "adding an account", func(w *wtx) error { return insertAccount(w, "12345678") }); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if m := mode(t, p); m != 0o600 {
				t.Errorf("%s has mode %v, want 0600", filepath.Base(p), m)
			}
		}
		if m := mode(t, filepath.Dir(path)); m != 0o700 {
			t.Errorf("the directory has mode %v, want 0700", m)
		}
	})
	t.Run("readable by others", func(t *testing.T) {
		t.Parallel()
		path := copyTemplate(t)
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		var w warnings
		s, err := Open(t.Context(), Options{Path: path, Now: fixedNow, Warn: w.warn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if m := mode(t, p); m != 0o600 {
				t.Errorf("%s has mode %v, want 0600", filepath.Base(p), m)
			}
		}
		want := []string{path + " could be read by others; it is now readable only by you"}
		if got := w.all(); !slices.Equal(got, want) {
			t.Errorf("Open warned %q, want %q", got, want)
		}
	})
}

// A store whose path holds what a file: URI would read as its own (#, %, a
// space, and ? where the system allows it) opens that very file, from a
// test directory whose name holds # too.
func TestOpenPathWithOddCharacters(t *testing.T) {
	t.Parallel()
	names := []string{"a#b%c d"}
	if runtime.GOOS != "windows" { // Windows forbids ? in names
		names = append(names, "a#b?c")
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			top := t.TempDir()
			if !strings.Contains(top, "#") {
				t.Fatalf("the test's directory %s holds no #", top)
			}
			path := filepath.Join(top, name, "history.db")
			s, err := Open(t.Context(), Options{Path: path, Now: fixedNow})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.withWrite(t.Context(), "adding an account", func(w *wtx) error { return insertAccount(w, "12345678") }); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if got := listing(t, top); !slices.Equal(got, []string{name}) {
				t.Errorf("the test's directory holds %q, want only %q", got, name)
			}
			if got, want := listing(t, filepath.Dir(path)), []string{"history.db", "history.db.migrate.lock"}; !slices.Equal(got, want) {
				t.Errorf("the store's directory holds %q, want %q", got, want)
			}
			s, err = Open(t.Context(), Options{Path: path, MustExist: true})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if n := countAccounts(t, s.r); n != 1 {
				t.Errorf("the store holds %d accounts, want 1", n)
			}
		})
	}
}

// header returns bytes 18 and 19 of the file at path, the versions that
// SQLite reads and writes it with: 1 for a rollback journal, 2 for WAL.
func header(t *testing.T, path string) [2]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) < 20 {
		t.Fatalf("%s has %d bytes", path, len(b))
	}
	return [2]byte{b[18], b[19]}
}

// readFile returns the bytes of the file at path.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Open refuses a file that is not a store, or not one it can use, and
// changes nothing: another program's SQLite file keeps its journal mode,
// its permissions and its directory as they were.
func TestOpenRefuses(t *testing.T) {
	t.Parallel()
	t.Run("another program's", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "other.db")
		db := rawDB(t, path)
		if _, err := db.ExecContext(t.Context(), "CREATE TABLE t (x); INSERT INTO t VALUES (1)"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		before, perm, names := header(t, path), mode(t, path), listing(t, dir)
		if before != [2]byte{1, 1} {
			t.Fatalf("the file's header holds %v, want a rollback journal's 1, 1", before)
		}
		_, err := Open(t.Context(), Options{Path: path})
		if !errors.Is(err, ErrNotAStore) {
			t.Fatalf("Open: %v, want ErrNotAStore", err)
		}
		if want := path + " is not a schwabtx store (it is another program's SQLite file); nothing in it was changed"; err.Error() != want {
			t.Errorf("Open: %q, want %q", err, want)
		}
		if got := header(t, path); got != before {
			t.Errorf("the header holds %v, want %v as before", got, before)
		}
		if got := mode(t, path); got != perm {
			t.Errorf("the file has mode %v, want %v as before", got, perm)
		}
		if got := listing(t, dir); !slices.Equal(got, names) {
			t.Errorf("the directory holds %q, want %q as before", got, names)
		}
	})
	t.Run("another program's, in WAL mode", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "other.db")
		db := rawDB(t, path)
		if _, err := db.ExecContext(t.Context(), "PRAGMA journal_mode = WAL; CREATE TABLE t (x); INSERT INTO t VALUES (1)"); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		before, perm, names := header(t, path), mode(t, path), listing(t, dir)
		if before != [2]byte{2, 2} || !slices.Equal(names, []string{"other.db"}) {
			t.Fatalf("the file's header holds %v and the directory %q, want WAL's 2, 2 and the file alone", before, names)
		}
		_, err := Open(t.Context(), Options{Path: path})
		if !errors.Is(err, ErrNotAStore) {
			t.Fatalf("Open: %v, want ErrNotAStore", err)
		}
		if got := header(t, path); got != before {
			t.Errorf("the header holds %v, want %v as before", got, before)
		}
		if got := mode(t, path); got != perm {
			t.Errorf("the file has mode %v, want %v as before", got, perm)
		}
		if got := listing(t, dir); !slices.Equal(got, names) {
			t.Errorf("the directory holds %q, want %q as before: no -wal or -shm", got, names)
		}
	})
	t.Run("another program's, with a journal to recover", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "other.db")
		journal := path + "-journal"
		c := startChild(t, "journal", "HISTORY_TEST_STORE="+path)
		c.expect(t, "inside")
		if err := c.cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = c.cmd.Wait()
		db, j := readFile(t, path), readFile(t, journal)
		if magic := []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}; len(j) < 8 || !slices.Equal(j[:8], magic) {
			t.Fatalf("the journal starts %x, want SQLite's magic %x: the child left no journal to recover", j[:min(8, len(j))], magic)
		}
		_, err := Open(t.Context(), Options{Path: path})
		if !errors.Is(err, ErrNotAStore) {
			t.Fatalf("Open: %v, want ErrNotAStore", err)
		}
		want := path + " is not a schwabtx store: it has a rollback journal to recover (other.db-journal), which a schwabtx store never has; " +
			"nothing in it was changed"
		if err.Error() != want {
			t.Errorf("Open: %q, want %q", err, want)
		}
		if got := readFile(t, path); !slices.Equal(got, db) {
			t.Error("the file changed: its journal was rolled back")
		}
		if got := readFile(t, journal); !slices.Equal(got, j) {
			t.Error("the journal changed")
		}
	})
	t.Run("WAL refused", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "history.db")
		_, err := Open(t.Context(), Options{Path: path, journalMode: "TRUNCATE"})
		if !errors.Is(err, ErrNotWAL) {
			t.Fatalf("Open: %v, want ErrNotWAL", err)
		}
		want := "the store at " + path + " needs WAL mode, which its file system refused (journal mode truncate); keep it on a local disk"
		if err.Error() != want {
			t.Errorf("Open: %q, want %q", err, want)
		}
	})
	t.Run("damaged", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "history.db")
		if err := os.WriteFile(path, []byte(strings.Repeat("not a database\n", 300)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(t.Context(), Options{Path: path})
		if !errors.Is(err, ErrDamaged) {
			t.Fatalf("Open: %v, want ErrDamaged", err)
		}
		if !strings.HasPrefix(err.Error(), path+" is damaged or not an SQLite file: move history.db, history.db-wal and history.db-shm aside together") {
			t.Errorf("Open: %q", err)
		}
	})
	t.Run("newer schema", func(t *testing.T) {
		t.Parallel()
		path := copyTemplate(t)
		if _, err := rawDB(t, path).ExecContext(t.Context(), "PRAGMA user_version = 99"); err != nil {
			t.Fatal(err)
		}
		_, err := Open(t.Context(), Options{Path: path})
		if !errors.Is(err, ErrNewer) {
			t.Fatalf("Open: %v, want ErrNewer", err)
		}
		want := "the store at " + path + " was written by a newer schwabtx (schema 99, rules 1, calendar 1); update this schwabtx, and the one your scheduled task runs"
		if err.Error() != want {
			t.Errorf("Open: %q, want %q", err, want)
		}
	})
	for _, column := range []string{"derived_with", "calendar_version"} {
		t.Run("newer "+column, func(t *testing.T) {
			t.Parallel()
			path := copyTemplate(t)
			if _, err := rawDB(t, path).ExecContext(t.Context(), "UPDATE store SET "+column+" = "+column+" + 1"); err != nil {
				t.Fatal(err)
			}
			s, err := Open(t.Context(), Options{Path: path})
			if err != nil {
				t.Fatalf("Open: %v; a store with newer rules or calendar can still be read", err)
			}
			defer s.Close()
			err = s.withWrite(t.Context(), "adding an account", func(w *wtx) error { return insertAccount(w, "12345678") })
			if !errors.Is(err, ErrNewer) || !strings.Contains(err.Error(), "was written by a newer schwabtx") {
				t.Errorf("a write: %v, want ErrNewer, written by a newer schwabtx", err)
			}
			if err := s.withRead(t.Context(), func(*rtx) error { return nil }); err != nil {
				t.Errorf("a read: %v", err)
			}
			if n := countAccounts(t, s.r); n != 0 {
				t.Errorf("the store holds %d accounts, want none", n)
			}
		})
	}
	t.Run("missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "sub", "history.db")
		_, err := Open(t.Context(), Options{Path: path, MustExist: true})
		if !errors.Is(err, ErrNoStore) || err.Error() != "no store at "+path+" yet; schwabtx import makes one" {
			t.Fatalf("Open: %v, want ErrNoStore", err)
		}
		if got := listing(t, dir); len(got) != 0 {
			t.Errorf("Open made %q", got)
		}
	})
	t.Run("empty with MustExist", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "history.db")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Open(t.Context(), Options{Path: path, MustExist: true})
		if !errors.Is(err, ErrNoStore) {
			t.Fatalf("Open: %v, want ErrNoStore", err)
		}
		if st, err := os.Stat(path); err != nil || st.Size() != 0 {
			t.Errorf("the file: %v, %v; want it empty as it was", st, err)
		}
		if got := listing(t, dir); !slices.Equal(got, []string{"history.db"}) {
			t.Errorf("the directory holds %q", got)
		}
	})
	for _, journal := range []string{"-wal", "-shm"} {
		for _, mustExist := range []bool{false, true} {
			t.Run("orphan "+journal, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				path := filepath.Join(dir, "history.db")
				if err := os.WriteFile(path+journal, []byte("journal"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := Open(t.Context(), Options{Path: path, MustExist: mustExist})
				if !errors.Is(err, ErrOrphanJournal) {
					t.Fatalf("Open: %v, want ErrOrphanJournal", err)
				}
				want := path + " is missing but " + path + journal + " is here: the journal of an earlier store whose database was moved or deleted; " +
					"move history.db-wal and history.db-shm to where history.db went, or remove them if you mean to discard that store"
				if err.Error() != want {
					t.Errorf("Open: %q, want %q", err, want)
				}
				if got := listing(t, dir); !slices.Equal(got, []string{"history.db" + journal}) {
					t.Errorf("the directory holds %q", got)
				}
			})
		}
	}
	for _, journal := range []string{"-wal", "-shm"} {
		for _, mustExist := range []bool{false, true} {
			t.Run("empty beside "+journal, func(t *testing.T) {
				t.Parallel()
				dir := t.TempDir()
				path := filepath.Join(dir, "history.db")
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path+journal, []byte("journal"), 0o600); err != nil {
					t.Fatal(err)
				}
				_, err := Open(t.Context(), Options{Path: path, MustExist: mustExist})
				if !errors.Is(err, ErrOrphanJournal) {
					t.Fatalf("Open: %v, want ErrOrphanJournal", err)
				}
				want := path + " is empty but " + path + journal + " is here: the journal of an earlier store whose database was emptied or replaced " +
					"by an empty file; put that store's database back as history.db, or remove history.db-wal and history.db-shm if you mean to discard that store"
				if err.Error() != want {
					t.Errorf("Open: %q, want %q", err, want)
				}
				if got := listing(t, dir); !slices.Equal(got, []string{"history.db", "history.db" + journal}) {
					t.Errorf("the directory holds %q", got)
				}
				if got := readFile(t, path+journal); string(got) != "journal" {
					t.Errorf("the journal holds %q, want it as it was", got)
				}
				if got := readFile(t, path); len(got) != 0 {
					t.Errorf("the database holds %d bytes, want it empty as it was", len(got))
				}
			})
		}
	}
	t.Run("a directory", func(t *testing.T) {
		t.Parallel()
		path := t.TempDir()
		_, err := Open(t.Context(), Options{Path: path})
		if err == nil || !strings.Contains(err.Error(), "is a directory") {
			t.Fatalf("Open: %v, want that it is a directory", err)
		}
		if got := listing(t, path); len(got) != 0 {
			t.Errorf("Open made %q", got)
		}
	})
	t.Run("network", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "history.db")
		disk := func(string) (fsInfo, error) { return fsInfo{network: true, name: "nfs"}, nil }
		_, err := Open(t.Context(), Options{Path: path, disk: disk})
		if !errors.Is(err, ErrNetworkFS) {
			t.Fatalf("Open: %v, want ErrNetworkFS", err)
		}
		want := "the store must be on a local disk: " + path + " is on a network file system (nfs), where SQLite's WAL mode can damage it; " +
			"put it on a local disk with -store PATH or SCHWAB_STORE_FILE"
		if err.Error() != want {
			t.Errorf("Open: %q, want %q", err, want)
		}
		if got := listing(t, dir); len(got) != 0 {
			t.Errorf("Open made %q", got)
		}
	})
}

// Open warns of a store on a FUSE file system or in a synced folder, and
// goes on.
func TestOpenWarns(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "Dropbox (Personal)", "history.db")
	var w warnings
	disk := func(string) (fsInfo, error) { return fsInfo{fuse: true, name: "fuse.encfs"}, nil }
	s, err := Open(t.Context(), Options{Path: path, Warn: w.warn, disk: disk})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := []string{
		path + " is on a FUSE file system (fuse.encfs); if that is a network or synced folder, SQLite's WAL mode, which needs shared memory " +
			"and working locks, can damage the store; a local disk is safest (-store PATH or SCHWAB_STORE_FILE)",
		path + " looks like a synced folder (Dropbox); a sync service that copies the store while it is open can leave a damaged copy",
	}
	if got := w.all(); !slices.Equal(got, want) {
		t.Errorf("Open warned\n%q\nwant\n%q", got, want)
	}
}

// The store's default file is SCHWAB_STORE_FILE, trimmed, else
// schwabtx/history.db in the user config directory, or on Windows in
// %LocalAppData%, which does not roam.
func TestDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	t.Setenv("LocalAppData", filepath.Join(home, "LocalAppData"))
	t.Setenv("SCHWAB_STORE_FILE", " /elsewhere/my.db\n")
	if got, err := DefaultPath(); err != nil || got != "/elsewhere/my.db" {
		t.Errorf("DefaultPath() = %q, %v, want SCHWAB_STORE_FILE trimmed", got, err)
	}
	t.Setenv("SCHWAB_STORE_FILE", " ")
	want := map[string]string{
		"windows": filepath.Join(home, "LocalAppData", "schwabtx", "history.db"),
		"darwin":  filepath.Join(home, "Library", "Application Support", "schwabtx", "history.db"),
	}[runtime.GOOS]
	if want == "" {
		want = filepath.Join(home, "config", "schwabtx", "history.db")
	}
	if got, err := DefaultPath(); err != nil || got != want {
		t.Errorf("DefaultPath() = %q, %v, want %q", got, err, want)
	}
}

// A store that a newer schwabtx has changed since Open read it, in its
// schema, its rules or its calendar, takes no more writes from this one,
// and the write that finds it out writes nothing.
func TestNewerBinaryStops(t *testing.T) {
	t.Parallel()
	for _, raise := range []string{
		"PRAGMA user_version = 2",
		"UPDATE store SET derived_with = derived_with + 1",
		"UPDATE store SET calendar_version = calendar_version + 1",
	} {
		t.Run(raise, func(t *testing.T) {
			t.Parallel()
			s := newStore(t, Options{})
			if _, err := rawDB(t, s.Path()).ExecContext(t.Context(), raise); err != nil {
				t.Fatal(err)
			}
			wrote := false
			err := s.withWrite(t.Context(), "adding an account", func(w *wtx) error {
				wrote = true
				return insertAccount(w, "12345678")
			})
			want := "the store at " + s.Path() + " was updated by a newer schwabtx while this one was running; restart it"
			if !errors.Is(err, ErrNewer) || err.Error() != want {
				t.Errorf("a write: %v, want %q", err, want)
			}
			if wrote {
				t.Error("the write ran")
			}
			if n := countAccounts(t, rawDB(t, s.Path())); n != 0 {
				t.Errorf("the store holds %d accounts, want none", n)
			}
		})
	}
	t.Run("reads", func(t *testing.T) {
		t.Parallel()
		s := newStore(t, Options{})
		if _, err := rawDB(t, s.Path()).ExecContext(t.Context(), "PRAGMA user_version = 2"); err != nil {
			t.Fatal(err)
		}
		if err := s.withRead(t.Context(), func(*rtx) error { return nil }); !errors.Is(err, ErrNewer) {
			t.Errorf("a read of a newer schema: %v, want ErrNewer", err)
		}
	})
}

// holdWriteLock takes the store's write lock from another connection, as
// another process would, until the test ends.
func holdWriteLock(t *testing.T, path string) {
	t.Helper()
	conn, err := rawDB(t, path).Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		conn.Close()
	})
}

// A write behind a lock that another connection holds ends with its
// context, at the deadline: neither SQLite's wait nor the pauses between
// retries outlast it. SQLite waits 1 ms, so after a second of retries the
// deadline falls in a pause, which lasts half a second or more by then.
func TestWaitEndsWithContext(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{BusyTimeout: time.Millisecond})
	holdWriteLock(t, s.Path())
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	deadline, _ := ctx.Deadline()
	err := s.withWrite(ctx, "adding an account", func(w *wtx) error { return insertAccount(w, "12345678") })
	late := time.Since(deadline)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the write: %v, want context.DeadlineExceeded", err)
	}
	if late > 50*time.Millisecond {
		t.Errorf("the write ended %v after the deadline, want within 50ms", late)
	}
}

// A write that another process keeps out says once that it is waiting,
// after 2 seconds, and gives up after LockWait with ErrBusy, saying what it
// was doing; it saved nothing.
func TestWriteGivesUp(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var waiting []string
	s := newStore(t, Options{BusyTimeout: time.Millisecond, LockWait: 2200 * time.Millisecond, Waiting: func(op string) {
		mu.Lock()
		defer mu.Unlock()
		waiting = append(waiting, op)
	}})
	holdWriteLock(t, s.Path())
	err := s.withWrite(t.Context(), "importing 2 files", func(w *wtx) error { return insertAccount(w, "12345678") })
	want := "the store at " + s.Path() + " stayed locked by another process for 2.2s while importing 2 files; nothing was saved; is a sync or an import running?"
	if !errors.Is(err, ErrBusy) || err.Error() != want {
		t.Errorf("the write: %v, want %q", err, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(waiting, []string{"importing 2 files"}) {
		t.Errorf("Waiting was told %q, want once of the import", waiting)
	}
}

// Errors from SQLite say what failed and what it means: a value that a
// check refuses is a bug to report, and a reader cannot write.
func TestErrorsSayWhat(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	add := func(w *wtx) error { return insertAccount(w, "12345678") }
	if err := s.withWrite(t.Context(), "adding an account", add); err != nil {
		t.Fatal(err)
	}
	err := s.withWrite(t.Context(), "adding an account", add)
	if want := "adding an account failed: a store check refused a value (please report it): "; err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Errorf("a second account of one number: %v, want %q...", err, want)
	}
	err = s.withRead(t.Context(), func(r *rtx) error {
		_, err := r.tx.ExecContext(r.ctx, "DELETE FROM sync_state")
		return err
	})
	if want := "reading the store failed: " + s.Path() + " cannot be written"; err == nil || err.Error() != want {
		t.Errorf("a write in a read: %v, want %q", err, want)
	}
}

// A write inside a write would wait for ever for the one writer
// connection; it panics instead.
func TestNestedWritePanics(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	defer func() {
		if recover() == nil {
			t.Error("a write inside a write did not panic")
		}
	}()
	_ = s.withWrite(t.Context(), "outer", func(w *wtx) error {
		return s.withWrite(w.ctx, "inner", func(*wtx) error { return nil })
	})
}

// Each connection has the settings that keep the store whole: commits and
// checkpoints flushed to the drive (fullfsync matters on macOS), foreign
// keys checked, no function of the schema trusted, a bounded -wal on the
// writer, and readers that cannot write.
func TestConnectionSettings(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	pragmas := func(db *sql.DB, names ...string) map[string]int {
		t.Helper()
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		got := map[string]int{}
		for _, name := range names {
			var v int
			if err := conn.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&v); err != nil {
				t.Fatalf("PRAGMA %s: %v", name, err)
			}
			got[name] = v
		}
		return got
	}
	common := map[string]int{"busy_timeout": 5000, "fullfsync": 1, "checkpoint_fullfsync": 1, "synchronous": 2, "foreign_keys": 1, "trusted_schema": 0}
	writer := maps.Clone(common)
	writer["journal_size_limit"] = 64 << 20
	writer["query_only"] = 0
	reader := maps.Clone(common)
	reader["query_only"] = 1
	for name, c := range map[string]struct {
		db   *sql.DB
		want map[string]int
	}{"writer": {s.w, writer}, "reader": {s.r, reader}} {
		if got := pragmas(c.db, slices.Collect(maps.Keys(c.want))...); !maps.Equal(got, c.want) {
			t.Errorf("the %s's settings are %v, want %v", name, got, c.want)
		}
	}
}

// A write holds the store's write lock from its start, before fn reads
// anything: another connection cannot start a write meanwhile.
func TestWriteLocksFirst(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	other, err := sql.Open("sqlite", dsn(s.Path(), "_pragma=busy_timeout(0)"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	var begin error
	if err := s.withWrite(t.Context(), "adding nothing", func(*wtx) error {
		conn, err := other.Conn(t.Context())
		if err != nil {
			return err
		}
		defer conn.Close()
		_, begin = conn.ExecContext(t.Context(), "BEGIN IMMEDIATE")
		_, _ = conn.ExecContext(t.Context(), "ROLLBACK")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if code(begin) != 5 {
		t.Errorf("another connection's BEGIN IMMEDIATE during a write: %v, want SQLITE_BUSY (5)", begin)
	}
}

// A write that leaves a derived row referring to nothing fails at its
// commit, though it deferred the foreign keys, and saves nothing.
func TestDanglingReferenceFails(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	err := s.withWrite(t.Context(), "adding a transaction's columns", func(w *wtx) error {
		if err := w.deferForeignKeys(); err != nil {
			return err
		}
		if err := insertAccount(w, "12345678"); err != nil {
			return err
		}
		_, err := w.exec("INSERT INTO txn (txn_id, account_id) VALUES (99, (SELECT account_id FROM account))")
		return err
	})
	if code(err) != 787 {
		t.Errorf("the write: %v, want a foreign key's refusal (787) at the commit", err)
	}
	if n := countAccounts(t, s.r); n != 0 {
		t.Errorf("the store holds %d accounts, want none", n)
	}
}
