package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

// Backup copies the store, as of its last commit, to file, a new file
// readable only by you, while other processes go on using the store. It
// refuses a file that exists, and one that is the store or one of its
// files, by name or as the same file under another name (IsCompanion).
// The copy is one database file, without a journal, which Restore takes.
func (s *Store) Backup(ctx context.Context, file string) error {
	file = cleanPath(file)
	if IsCompanion(file, s.path) {
		return fmt.Errorf("%s is the store itself (or one of its files); choose another name", file)
	}
	return s.vacuumInto(ctx, "backing the store up", file)
}

// vacuumInto copies the store to name, a new file, on the writer connection
// outside any transaction (a reader, being query_only, may not), retried on
// SQLITE_BUSY as writes are. VACUUM INTO writes into an empty file, but a
// file that it creates itself is readable by others, so the file is made
// first, readable only by you; a file that is already there is refused,
// and the file is removed again if the copy fails.
func (s *Store) vacuumInto(ctx context.Context, op, name string) error {
	//nolint:forbidigo // A new file, never one of the store's own: O_EXCL refuses any that is there.
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s: %s exists; choose a new name", op, name)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if err := s.retry(ctx, op, func() error {
		_, err := s.w.ExecContext(ctx, "VACUUM INTO ?", name)
		return err
	}); err != nil {
		_ = os.Remove(name)
		return s.mapError(op, err)
	}
	return nil
}

// Counts are how much evidence a store holds: what a restore replaces.
type Counts struct {
	Accounts, Transactions, Snapshots, Files, Decisions int
}

// countEvidence counts the evidence that q holds.
func countEvidence(ctx context.Context, q querier) (Counts, error) {
	var c Counts
	err := q.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM account), (SELECT count(*) FROM api_txn),
		(SELECT count(*) FROM snapshot), (SELECT count(*) FROM import_file), (SELECT count(*) FROM decision)`).
		Scan(&c.Accounts, &c.Transactions, &c.Snapshots, &c.Files, &c.Decisions)
	return c, err
}

// String says what c counts, as a restore reports it.
func (c Counts) String() string {
	return fmt.Sprintf("%d %s, %d %s, %d %s, %d %s, %d %s",
		c.Accounts, plural(c.Accounts, "account", "accounts"), c.Transactions, plural(c.Transactions, "API transaction", "API transactions"),
		c.Snapshots, plural(c.Snapshots, "snapshot", "snapshots"), c.Files, plural(c.Files, "imported file", "imported files"),
		c.Decisions, plural(c.Decisions, "decision", "decisions"))
}

// Restored says what Restore did, or, on a dry run, would do.
type Restored struct {
	// Taken is when the backup was written, as its file's modification
	// time says.
	Taken time.Time
	// Updated says that the backup was of an older schema, rules or
	// calendar, and was brought up to this schwabtx's before it was
	// restored (not on a dry run).
	Updated bool
	// Before is what the store held, and After what it holds now, which is
	// the backup's.
	Before, After Counts
}

// Restore replaces the store's content with that of backup, a store that
// Backup wrote (or a backup made before a schema changed), through SQLite's
// backup API. It never copies, moves or removes the store's files: SQLite
// first recovers whatever the store's own -wal holds and then replaces the
// store's pages in one transaction, which other processes' connections see
// as they would any other write. So a store whose writer was killed is
// restored as well as one that was closed, and a process that has it open
// meanwhile sees the backup's content at its next read.
//
// The backup must be a schwabtx store that passes SQLite's integrity check
// and that no newer schwabtx wrote. One of an older schema, rules or
// calendar is first copied beside the store and brought up to this
// schwabtx's there, so that what is restored is current. The store is
// opened as Open opens it, created if it is missing; while it is restored,
// no sync and no change of schema can start. Restore warns, through
// o.Warn, that what was recorded since the backup is gone. dryRun reports
// the counts and changes nothing: it reads the store without opening it as
// Open does, so it neither creates a missing store, nor makes an existing
// one private, nor brings an older one up to date.
func Restore(ctx context.Context, o Options, backup string, dryRun bool) (Restored, error) {
	var r Restored
	path, err := storePath(o)
	if err != nil {
		return r, err
	}
	backup = cleanPath(backup)
	if isCompanion(backup, path, false) {
		return r, fmt.Errorf("%s is the store itself (or one of its files), not a backup of it", backup)
	}
	st, err := os.Stat(backup) //nolint:gosec // G703: the backup is yours to name; only its metadata is read here.
	switch {
	case err != nil:
		return r, fmt.Errorf("reading the backup: %w", err)
	case !st.Mode().IsRegular():
		return r, fmt.Errorf("%s is not a file, so not a backup of the store", backup)
	}
	r.Taken = st.ModTime().UTC()
	v, after, err := inspectBackup(ctx, backup)
	if err != nil {
		return r, err
	}
	r.After = after
	if dryRun {
		r.Before, err = dryCounts(ctx, o, path)
		return r, err
	}
	s, err := Open(ctx, o)
	if err != nil {
		return r, err
	}
	defer s.Close()
	// The sync's lock first, as a sync takes it before it opens the store.
	releaseSync, err := s.lock(ctx, ".sync.lock", "restoring the store")
	if err != nil {
		return r, err
	}
	defer releaseSync()
	releaseMigrate, err := s.lock(ctx, ".migrate.lock", "restoring the store")
	if err != nil {
		return r, err
	}
	defer releaseMigrate()
	if r.Before, err = s.counts(ctx); err != nil {
		return r, err
	}
	src := backup
	if v.older() {
		dir, err := os.MkdirTemp(filepath.Dir(path), ".restore-")
		if err != nil {
			return r, fmt.Errorf("restoring the store: %w", err)
		}
		defer os.RemoveAll(dir)
		if src, err = updateCopy(ctx, o, backup, filepath.Join(dir, "history.db")); err != nil {
			return r, err
		}
		r.Updated = true
	}
	const op = "restoring the store"
	if err := s.retry(ctx, op, func() error { return s.restoreFrom(ctx, src) }); err != nil {
		return r, s.mapError(op, err)
	}
	if r.After, err = s.counts(ctx); err != nil {
		return r, err
	}
	s.o.Warn(fmt.Sprintf("restored the store from %s (taken %s); what was recorded since is gone", backup, instant(r.Taken)))
	return r, nil
}

// dryCounts counts the evidence of the store at path for a dry run of
// Restore, changing nothing: the path is checked as Open checks it, and the
// store identified and read through connections that cannot change it
// (a WAL that a killed writer left is recovered, as Open's probe recovers
// it). A missing store holds nothing; a newer schwabtx's is refused.
func dryCounts(ctx context.Context, o Options, path string) (Counts, error) {
	o.MustExist = true
	if o.Warn == nil {
		o.Warn = func(string) {}
	}
	if o.disk == nil {
		o.disk = localDisk
	}
	s := &Store{path: path, o: o}
	err := s.checkPath()
	if err == nil {
		err = s.identify(ctx)
	}
	switch {
	case errors.Is(err, ErrNoStore):
		return Counts{}, nil
	case err != nil:
		return Counts{}, err
	}
	db, err := sql.Open("sqlite", dsn(path, "mode=ro"))
	if err != nil {
		return Counts{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Counts{}, s.mapError("reading the store", err)
	}
	defer tx.Rollback() //nolint:errcheck // a read: nothing to undo
	v, err := readVersions(ctx, tx)
	switch {
	case err != nil:
		return Counts{}, s.mapError("reading the store", err)
	case v.newer():
		return Counts{}, s.newerError(v, true)
	}
	c, err := countEvidence(ctx, tx)
	if err != nil {
		return Counts{}, s.mapError("reading the store", err)
	}
	return c, nil
}

// lock takes the lock on the file beside the store whose name adds suffix,
// waiting for it as long as a write waits for the store.
func (s *Store) lock(ctx context.Context, suffix, op string) (release func(), err error) {
	l, err := filelock.Acquire(ctx, s.path+suffix, s.o.LockWait)
	switch {
	case errors.Is(err, filelock.ErrTimeout):
		return nil, &storeError{kind: ErrBusy, err: err, msg: fmt.Sprintf("the store at %s stayed locked by another process for %v while %s; "+
			"nothing was changed; is a sync or an import running?", s.path, s.o.LockWait, op)}
	case errors.Is(err, filelock.ErrUnsupported):
		// The store's checks have refused the network file systems that
		// cannot lock; the restore is one transaction all the same.
		return func() {}, nil
	case err != nil:
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return l.Release, nil
}

// counts counts the store's evidence.
func (s *Store) counts(ctx context.Context) (Counts, error) {
	var c Counts
	err := s.withRead(ctx, func(r *rtx) (err error) {
		c, err = countEvidence(r.ctx, r.tx)
		return err
	})
	return c, err
}

// restoreFrom copies the pages of the database at src into the store, on
// the writer connection, in one step of SQLite's backup API.
func (s *Store) restoreFrom(ctx context.Context, src string) error {
	conn, err := s.w.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(dc any) error {
		r, ok := dc.(interface {
			NewRestore(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("the SQLite driver cannot restore a backup")
		}
		b, err := r.NewRestore(dsn(src, "mode=ro"))
		if err != nil {
			return err
		}
		_, err = b.Step(-1)
		return errors.Join(err, b.Finish())
	})
}

// inspectBackup reads the backup at path through connections that cannot
// change it, as Open reads a store's identity: it must be a schwabtx store,
// pass SQLite's integrity check and be of no schema, rules or calendar
// newer than this schwabtx's. It returns the backup's versions and counts.
func inspectBackup(ctx context.Context, path string) (versions, Counts, error) {
	var v versions
	var c Counts
	b := &Store{path: path, o: Options{MustExist: true}}
	if err := b.identify(ctx); err != nil {
		switch {
		case errors.Is(err, ErrDamaged):
			return v, c, fmt.Errorf("%s is damaged or not an SQLite file, so it cannot be restored", path)
		case errors.Is(err, ErrNotAStore), errors.Is(err, ErrNoStore):
			return v, c, fmt.Errorf("%s is not a schwabtx store, so it cannot be restored", path)
		}
		return v, c, fmt.Errorf("reading the backup: %w", err)
	}
	db, err := sql.Open("sqlite", backupDSN(path))
	if err != nil {
		return v, c, fmt.Errorf("reading the backup: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var problems []string
	err = func() error {
		rows, err := db.QueryContext(ctx, "PRAGMA integrity_check(5)")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				return err
			}
			if p != "ok" {
				problems = append(problems, p)
			}
		}
		return rows.Err()
	}()
	switch {
	case code(err)&0xff == sqlite3.SQLITE_CORRUPT, code(err)&0xff == sqlite3.SQLITE_NOTADB:
		return v, c, fmt.Errorf("%s is damaged (integrity_check: %w)", path, err)
	case err != nil:
		return v, c, fmt.Errorf("reading the backup: %w", err)
	case len(problems) > 0:
		return v, c, fmt.Errorf("%s is damaged (integrity_check: %s)", path, strings.Join(problems, "; "))
	}
	if v, err = readVersions(ctx, db); err != nil {
		return v, c, fmt.Errorf("reading the backup: %w", err)
	}
	if v.schema == 0 {
		return v, c, fmt.Errorf("%s is not a schwabtx store, so it cannot be restored", path)
	}
	if v.newer() {
		return v, c, fmt.Errorf("%s was written by a newer schwabtx (schema %d, rules %d, calendar %d); "+
			"update this schwabtx to restore it", path, v.schema, v.derive, v.calendar)
	}
	if c, err = countEvidence(ctx, db); err != nil {
		return v, c, fmt.Errorf("reading the backup: %w", err)
	}
	return v, c, nil
}

// backupDSN returns the URI of a connection to the backup at path that
// cannot change it. A backup has no journal, and is read as Open's probe
// reads a store without one, making none.
func backupDSN(path string) string {
	b := &Store{path: path}
	if b.noJournal() {
		return dsn(path, "mode=ro", "immutable=1")
	}
	return dsn(path, "mode=ro")
}

// updateCopy copies the backup at path to tmp, a new file readable only by
// you, and opens it there as a store, which brings its schema, rules and
// calendar up to this schwabtx's, and returns tmp.
func updateCopy(ctx context.Context, o Options, path, tmp string) (string, error) {
	const op = "bringing the backup up to this schwabtx"
	db, err := sql.Open("sqlite", backupDSN(path))
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	// VACUUM INTO, through a connection to the backup that cannot change
	// it, into a file made first, readable only by you.
	//nolint:forbidigo // A new file in a directory just made for it, never one of the store's own.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	t, err := Open(ctx, Options{Path: tmp, Now: o.Now, BusyTimeout: o.BusyTimeout, LockWait: o.LockWait, disk: o.disk})
	if err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	if err := t.Close(); err != nil {
		return "", fmt.Errorf("%s: %w", op, err)
	}
	return tmp, nil
}
