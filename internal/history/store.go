package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	sqlite3 "modernc.org/sqlite/lib"
)

// applicationID marks a SQLite file as a schwabtx store: "SWTX" in ASCII.
const applicationID = 0x53575458

// Options says which store to open and how.
type Options struct {
	// Path is the store's file. Empty means DefaultPath. It is made
	// absolute.
	Path string
	// MustExist makes Open refuse a missing store, with ErrNoStore, instead
	// of creating one, as the commands that only read do.
	MustExist bool
	// Now tells the time that the store records. Nil means time.Now.
	Now func() time.Time
	// Warn is told what Open noticed or did that its caller should know of:
	// permissions it fixed, a synced folder, a backup it made, derived data
	// it brought up to date. Nil drops the warnings.
	Warn func(string)
	// Waiting is called once when a write has waited for another process
	// for 2 seconds, with what the write was doing. Nil means nothing is.
	Waiting func(op string)
	// BusyTimeout is how long SQLite itself waits for a lock before it
	// reports the store busy. Zero means 5 seconds.
	BusyTimeout time.Duration
	// LockWait is how long a write keeps trying while another process holds
	// the lock, before it gives up with ErrBusy. Zero means 2 minutes.
	LockWait time.Duration

	// disk, if set, stands in for localDisk in tests.
	disk func(path string) (fsInfo, error)
	// maxRows, if set, stands in for schwab.MaxTransactions in tests.
	maxRows int
	// margin, if set, stands in for the second by which a time must lie
	// inside a read's window for the read to count it in (deriveAccount),
	// so that a test can derive under a narrower rule.
	margin time.Duration
}

// The errors that Open, and the store's reads and writes, return, as
// errors.Is reports them. Each comes with a message that says what to do.
var (
	// ErrNoStore means that there is no store yet where Options.MustExist
	// asked for one.
	ErrNoStore = errors.New("no store yet")
	// ErrNotAStore means that the file is another program's.
	ErrNotAStore = errors.New("not a schwabtx store")
	// ErrNewer means that a newer schwabtx has written the store, with a
	// schema, rules or a calendar that this one does not know.
	ErrNewer = errors.New("the store was written by a newer schwabtx")
	// ErrNetworkFS means that the store is on a network file system, where
	// SQLite's WAL mode can damage it.
	ErrNetworkFS = errors.New("the store is on a network file system")
	// ErrNotWAL means that the store's file system refused WAL mode.
	ErrNotWAL = errors.New("the store's file system refused WAL mode")
	// ErrDamaged means that the store is damaged, or not an SQLite file.
	ErrDamaged = errors.New("the store is damaged")
	// ErrBusy means that another process kept the store locked for the
	// whole of Options.LockWait.
	ErrBusy = errors.New("the store stayed locked by another process")
	// ErrOrphanJournal means that the store is missing, or empty, but its
	// -wal or -shm is there: the journal of an earlier store whose database
	// was moved or emptied.
	ErrOrphanJournal = errors.New("the store's journal is there without the store")
)

// A storeError is one of the errors above, with a message of its own.
type storeError struct {
	kind error  // which of the errors above
	msg  string // what happened and what to do
	err  error  // the cause, if any
}

func (e *storeError) Error() string        { return e.msg }
func (e *storeError) Is(target error) bool { return target == e.kind }
func (e *storeError) Unwrap() error        { return e.err }

// A Store is an open store. It is safe for concurrent use: writes queue for
// the one writer connection, and reads share a pool of their own.
type Store struct {
	path string
	o    Options
	w    *sql.DB // the writer: one connection, BEGIN IMMEDIATE
	r    *sql.DB // the readers: query_only
	// opened is what the store held when Open read it, after any migration.
	opened versions
	// retries counts the attempts that SQLITE_BUSY made the store repeat,
	// for tests.
	retries atomic.Int64
}

// DefaultPath returns the store's file when no -store flag names one:
// SCHWAB_STORE_FILE, else schwabtx/history.db in the user config directory,
// or on Windows in %LocalAppData%, which does not roam: a roaming profile
// copies its files at sign-in and sign-out, and a copy of an SQLite file
// taken while it is in use can be damaged.
func DefaultPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("SCHWAB_STORE_FILE")); p != "" {
		return p, nil
	}
	dir, err := storeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a directory for the store; use -store or set SCHWAB_STORE_FILE: %w", err)
	}
	return filepath.Join(dir, "schwabtx", "history.db"), nil
}

// storeDir is the directory that holds schwabtx's directory of the store.
// Never the cache directory elsewhere: the store holds history that Schwab
// no longer serves, and decisions.
func storeDir() (string, error) {
	if runtime.GOOS == "windows" {
		return os.UserCacheDir() // %LocalAppData%
	}
	return os.UserConfigDir()
}

// Open opens the store at o.Path, creating it unless o.MustExist says not
// to, and brings its schema and derived data up to this schwabtx's.
//
// Nothing that can change a file runs before Open has shown that the file
// is a schwabtx store or a new one: it reads the file's identity through a
// connection that cannot write. Its first statements are retried on
// SQLITE_BUSY as writes are, since two processes that open a new store at
// once can be refused where SQLite's own wait does not apply.
func Open(ctx context.Context, o Options) (*Store, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Warn == nil {
		o.Warn = func(string) {}
	}
	if o.Waiting == nil {
		o.Waiting = func(string) {}
	}
	if o.BusyTimeout <= 0 {
		o.BusyTimeout = 5 * time.Second
	}
	if o.LockWait <= 0 {
		o.LockWait = 2 * time.Minute
	}
	if o.disk == nil {
		o.disk = localDisk
	}
	path := o.Path
	if path == "" {
		p, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	// A file: URI of a relative path is refused, so the path is made
	// absolute.
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("finding the store's path: %w", err)
	}
	s := &Store{path: path, o: o}
	if err := s.checkPath(); err != nil {
		return nil, err
	}
	if err := s.retry(ctx, "opening the store", func() error { return s.connect(ctx) }); err != nil {
		s.Close()
		return nil, s.mapError("opening the store", err)
	}
	if s.opened.schema < buildVersions().schema {
		if err := s.migrate(ctx); err != nil {
			s.Close()
			return nil, err
		}
	}
	if err := s.update(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the store's connections.
func (s *Store) Close() error {
	var errs []error
	for _, db := range []*sql.DB{s.w, s.r} {
		if db != nil {
			errs = append(errs, db.Close())
		}
	}
	return errors.Join(errs...)
}

// Path returns the store's file, made absolute.
func (s *Store) Path() string { return s.path }

// checkPath makes the checks that come before SQLite, by path only, never
// with a descriptor of the store or its journals, and makes a missing
// store's file, private, unless o.MustExist.
func (s *Store) checkPath() error {
	path := s.path
	info, err := s.o.disk(path)
	if err == nil {
		switch {
		case info.network:
			return &storeError{kind: ErrNetworkFS, msg: fmt.Sprintf("the store must be on a local disk: %s is on a network file system (%s), "+
				"where SQLite's WAL mode can damage it; put it on a local disk with -store PATH or SCHWAB_STORE_FILE", path, info.name)}
		case info.fuse:
			s.o.Warn(fmt.Sprintf("%s is on a FUSE file system (%s); if that is a network or synced folder, SQLite's WAL mode, "+
				"which needs shared memory and working locks, can damage the store; a local disk is safest (-store PATH or SCHWAB_STORE_FILE)", path, info.name))
		}
	}
	if name := syncedFolder(path); name != "" {
		s.o.Warn(fmt.Sprintf("%s looks like a synced folder (%s); a sync service that copies the store while it is open can leave a damaged copy", path, name))
	}
	st, err := os.Stat(path)
	switch {
	case err == nil && st.IsDir():
		return fmt.Errorf("%s is a directory, not a store; name the store's file with -store PATH or SCHWAB_STORE_FILE", path)
	case err == nil && st.Size() > 0:
		return nil
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("looking for the store: %w", err)
	}
	// The store is missing, or empty. A journal beside it belongs to an
	// earlier store whose database was moved, or emptied: adopting it would
	// lose or damage that store, and SQLite deletes the -wal of an empty
	// database the first time it reads it. A store that another process is
	// making has a page in its database before it has a -wal or -shm.
	for _, j := range []string{path + "-wal", path + "-shm"} {
		if _, jerr := os.Lstat(j); jerr == nil {
			base := filepath.Base(path)
			wal, shm := base+"-wal", base+"-shm"
			if err == nil {
				return &storeError{kind: ErrOrphanJournal, msg: fmt.Sprintf("%s is empty but %s is here: the journal of an earlier store whose database "+
					"was emptied or replaced by an empty file; put that store's database back as %s, or remove %s and %s if you mean to discard that store",
					path, j, base, wal, shm)}
			}
			return &storeError{kind: ErrOrphanJournal, msg: fmt.Sprintf("%s is missing but %s is here: the journal of an earlier store whose database "+
				"was moved or deleted; move %s and %s to where %s went, or remove them if you mean to discard that store",
				path, j, wal, shm, base)}
		}
	}
	if err == nil {
		return nil // an empty file: a new store, unless o.MustExist (identify)
	}
	if s.o.MustExist {
		return &storeError{kind: ErrNoStore, msg: fmt.Sprintf("no store at %s yet; schwabtx import makes one", path)}
	}
	return createPrivate(path)
}

// createPrivate makes the store's directory, readable only by you, and its
// file, empty and readable only by you, before any SQLite connection to it
// exists, so that SQLite makes its -wal and -shm with the same permissions.
// It never changes a file that is there: another process may have just made
// it, and a file that is not a store must stay as it was.
func createPrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating the store's directory: %w", err)
	}
	//nolint:forbidigo // The store is missing, and O_EXCL never opens one that another process made meanwhile; no connection to it exists yet.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating the store: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("creating the store: %w", err)
	}
	return nil
}

// dsn returns the file: URI of path with the query params. net/url escapes
// what SQLite would read as the URI's own, such as # and ?, and a Windows
// path gets the slash before its volume that SQLite needs (file:///C:/...).
func dsn(path string, params ...string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	u := url.URL{Scheme: "file", Path: p, RawQuery: strings.Join(params, "&")}
	return u.String()
}

// pragmas are the parameters that every connection to the store sets.
// fullfsync and checkpoint_fullfsync make macOS flush the drive's cache at
// each commit and checkpoint, as synchronous(FULL) does elsewhere; readers
// set them too, since the last connection to close, which checkpoints, may
// be a reader. Neither sets journal_mode, which connect sets once it has
// seen that the file is a store.
func (s *Store) pragmas() []string {
	return []string{
		"_pragma=busy_timeout(" + strconv.FormatInt(s.o.BusyTimeout.Milliseconds(), 10) + ")",
		"_pragma=foreign_keys(1)",
		"_pragma=fullfsync(1)",
		"_pragma=checkpoint_fullfsync(1)",
		"_pragma=trusted_schema(0)",
	}
}

// writerDSN is the writer's: one connection, whose transactions take the
// write lock at BEGIN.
func (s *Store) writerDSN() string {
	return dsn(s.path, append(s.pragmas(),
		"_pragma=synchronous(FULL)",
		"_pragma=journal_size_limit(67108864)",
		"_txlock=immediate")...)
}

// readerDSN is the readers': a write on a query_only connection fails.
func (s *Store) readerDSN() string {
	return dsn(s.path, append(s.pragmas(), "_pragma=query_only(1)")...)
}

// connect runs Open's steps that SQLITE_BUSY can interrupt, so it may run
// again: it reads the file's identity, makes the store's files private, sets
// WAL mode and reads the versions.
func (s *Store) connect(ctx context.Context) error {
	if err := s.identify(ctx); err != nil {
		return err
	}
	s.makePrivate()
	if s.w == nil {
		w, err := sql.Open("sqlite", s.writerDSN())
		if err != nil {
			return err
		}
		w.SetMaxOpenConns(1)
		s.w = w
		r, err := sql.Open("sqlite", s.readerDSN())
		if err != nil {
			return err
		}
		r.SetMaxOpenConns(4)
		s.r = r
	}
	var mode string
	if err := s.w.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		// The pragma returns the mode in force rather than failing when the
		// file system cannot give WAL what it needs. The mode persists in
		// the file, so this happens once, to a new store.
		if err := s.w.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
			return err
		}
		if !strings.EqualFold(mode, "wal") {
			return &storeError{kind: ErrNotWAL, msg: fmt.Sprintf("the store at %s needs WAL mode, which its file system refused (journal mode %s); keep it on a local disk", s.path, mode)}
		}
	}
	v, err := readVersions(ctx, s.w)
	if err != nil {
		return err
	}
	s.opened = v
	if v.schema > buildVersions().schema {
		return s.newerError(v, true)
	}
	return nil
}

// identify reads the file's application ID, schema version and whether it
// holds any schema, through a connection that cannot write and sets no
// pragma, and refuses a file that is neither a schwabtx store nor new.
//
// A read-only connection to a file in WAL mode still makes a -wal and a
// -shm beside it, and leaves them, so a file without any journal is read as
// immutable, which makes none: with no journal there is nothing for it to
// miss. That read takes no lock, so if a journal has appeared meanwhile,
// or the read failed, another process may have been changing the file, and
// it is read again with locks.
func (s *Store) identify(ctx context.Context) error {
	var id, version int64
	var empty, read bool
	var err error
	if s.noJournal() {
		id, version, empty, err = s.probe(ctx, "mode=ro", "immutable=1")
		read = err == nil && s.noJournal()
	}
	if !read {
		id, version, empty, err = s.probe(ctx, "mode=ro")
	}
	if err != nil && journalNeedsRecovery(err) && exists(s.path+"-wal") {
		// A read-only connection cannot recover a WAL that a killed writer
		// left. Without mode=ro, and still with no pragma, SQLite recovers
		// the journal that it wrote itself, and writes nothing else.
		id, version, empty, err = s.probe(ctx)
	}
	if code(err) == sqlite3.SQLITE_READONLY_ROLLBACK {
		// A rollback journal to recover, which a store, always in WAL mode,
		// never has: recovering it would change another program's file.
		return &storeError{kind: ErrNotAStore, err: err, msg: fmt.Sprintf("%s is not a schwabtx store: it has a rollback journal to recover (%s-journal), "+
			"which a schwabtx store never has; nothing in it was changed", s.path, filepath.Base(s.path))}
	}
	if err != nil {
		switch code(err) & 0xff {
		case sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_CORRUPT:
			return s.damaged(err)
		}
		return err
	}
	switch {
	case id == applicationID:
		return nil
	case id == 0 && empty && version == 0 && s.o.MustExist:
		// An empty file, such as one that a process made before it was
		// killed: no store yet, and a reader does not make one.
		return &storeError{kind: ErrNoStore, msg: fmt.Sprintf("no store at %s yet; schwabtx import makes one", s.path)}
	case id == 0 && empty && version == 0:
		return nil // a new store
	}
	return &storeError{kind: ErrNotAStore, msg: s.path + " is not a schwabtx store (it is another program's SQLite file); nothing in it was changed"}
}

// noJournal reports whether the file has no -wal, -shm or rollback journal
// beside it.
func (s *Store) noJournal() bool {
	return !exists(s.path+"-wal") && !exists(s.path+"-shm") && !exists(s.path+"-journal")
}

// probe reads the file's application ID and user_version and whether its
// schema is empty, through a connection with the URI's params.
func (s *Store) probe(ctx context.Context, params ...string) (id, version int64, empty bool, err error) {
	db, err := sql.Open("sqlite", dsn(s.path, params...))
	if err != nil {
		return 0, 0, false, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// One transaction, so that the three reads see the file at one moment
	// even while another process creates the store's schema.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck // It only read.
	if err := tx.QueryRowContext(ctx, "PRAGMA application_id").Scan(&id); err != nil {
		return 0, 0, false, err
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, 0, false, err
	}
	var some bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM sqlite_schema)").Scan(&some); err != nil {
		return 0, 0, false, err
	}
	return id, version, !some, nil
}

// journalNeedsRecovery reports whether err is a read-only connection's
// refusal to open a WAL that needs recovering.
func journalNeedsRecovery(err error) bool {
	c := code(err)
	return c&0xff == sqlite3.SQLITE_CANTOPEN || c == sqlite3.SQLITE_READONLY_RECOVERY || c == sqlite3.SQLITE_READONLY_CANTINIT
}

// makePrivate makes the store, its -wal and its -shm readable only by you,
// by path, now that the file is known to be a store, and warns once if any
// was not. Windows gives files their folder's permissions instead.
func (s *Store) makePrivate() {
	if runtime.GOOS == "windows" {
		return
	}
	var loose bool
	var errs []error
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm()&0o077 == 0 {
			continue
		}
		loose = true
		if err := os.Chmod(p, 0o600); err != nil {
			errs = append(errs, err)
		}
	}
	switch {
	case len(errs) > 0:
		s.o.Warn(fmt.Sprintf("%s could be read by others and could not be made private: %v", s.path, errors.Join(errs...)))
	case loose:
		s.o.Warn(s.path + " could be read by others; it is now readable only by you")
	}
}

// damaged is the error for a store that SQLite found damaged or not a
// database.
func (s *Store) damaged(err error) error {
	b := filepath.Base(s.path)
	return &storeError{kind: ErrDamaged, err: err, msg: fmt.Sprintf("%s is damaged or not an SQLite file: move %s, %s-wal and %s-shm aside together "+
		"(never the first alone: the others may hold its last changes), then schwabtx store restore BACKUP (%s.schema*.bak, "+
		"or one from schwabtx store backup), or start a new store", s.path, b, b, b, s.path)}
}

// exists reports whether there is a file at path, by path.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
