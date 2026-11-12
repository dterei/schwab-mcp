package history

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/dterei/schwab-mcp/internal/nyse"
)

// DeriveVersion is the version of the rules that derive data from the
// evidence. It changes whenever a rule does, so that a store derived under
// older rules derives everything again when it is opened.
const DeriveVersion = 1

// The versions that this build writes. Tests raise them to stand in for a
// newer schwabtx; nothing else changes them.
var (
	deriveVersion   = DeriveVersion
	calendarVersion = nyse.Version
)

// versions are what a store records of the build that last wrote it: its
// schema (PRAGMA user_version), the rules that derived its data
// (store.derived_with) and the calendar of its market dates
// (store.calendar_version). A store without a schema has none of them.
type versions struct{ schema, derive, calendar int }

// buildVersions returns this build's versions.
func buildVersions() versions {
	return versions{schema: len(migrations), derive: deriveVersion, calendar: calendarVersion}
}

// newer reports whether v holds a version that this build does not know.
func (v versions) newer() bool {
	b := buildVersions()
	return v.schema > b.schema || v.derive > b.derive || v.calendar > b.calendar
}

// A querier is a connection pool or a transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readVersions reads the store's versions.
func readVersions(ctx context.Context, q querier) (versions, error) {
	var v versions
	if err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v.schema); err != nil {
		return v, err
	}
	if v.schema == 0 {
		return v, nil
	}
	err := q.QueryRowContext(ctx, "SELECT derived_with, calendar_version FROM store WHERE id = 1").Scan(&v.derive, &v.calendar)
	return v, err
}

// newerError is ErrNewer for a store whose versions are v: one that was
// newer when Open read it, or one that a newer schwabtx changed after.
func (s *Store) newerError(v versions, atOpen bool) error {
	if atOpen || v == s.opened {
		return &storeError{kind: ErrNewer, msg: fmt.Sprintf("the store at %s was written by a newer schwabtx (schema %d, rules %d, calendar %d); "+
			"update this schwabtx, and the one your scheduled task runs", s.path, v.schema, v.derive, v.calendar)}
	}
	return &storeError{kind: ErrNewer, msg: fmt.Sprintf("the store at %s was updated by a newer schwabtx while this one was running; restart it", s.path)}
}

// A wtx is a write transaction of withWrite.
type wtx struct {
	s   *Store
	tx  *sql.Tx
	ctx context.Context
	// now is the one reading of the clock for the whole transaction: every
	// time it records is this one.
	now time.Time
	// touched are the accounts whose evidence the write changed, which the
	// end of the write derives again.
	touched map[int64]bool
	// regroup says that the write added or changed an instrument's key or
	// name, a transaction's legs or a decision about securities, so that
	// the end of the write groups the keys into securities again.
	regroup bool
	// sessions says that the write added or derived a snapshot, so that
	// the end of the write brings the market sessions to the snapshots'
	// span again.
	sessions bool
	// deferred says that foreign keys are checked at the commit.
	deferred bool
	// flagDefs says that flag_def holds flagDefs (writeFlagDefs).
	flagDefs bool
}

// touch marks account as one whose evidence the write changed.
func (w *wtx) touch(account int64) {
	w.touched[account] = true
}

// deferForeignKeys has foreign keys checked at the commit rather than at
// each statement, so that derived rows that refer to one another can be
// written in any order; a dangling reference still fails the commit.
func (w *wtx) deferForeignKeys() error {
	if w.deferred {
		return nil
	}
	_, err := w.exec("PRAGMA defer_foreign_keys = ON")
	w.deferred = err == nil
	return err
}

// noteKeys marks the write for grouping when a statement that observed a
// key or a name changed a row.
func (w *wtx) noteKeys(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if n > 0 {
		w.regroup = true
	}
	return err
}

// exec runs a statement in the transaction.
func (w *wtx) exec(query string, args ...any) (sql.Result, error) {
	return w.tx.ExecContext(w.ctx, query, args...)
}

// An rtx is a read transaction of withRead.
type rtx struct {
	tx  *sql.Tx
	ctx context.Context
}

// query runs a query in the transaction. Its rows must be closed before fn
// returns.
func (r *rtx) query(query string, args ...any) (*sql.Rows, error) {
	return r.tx.QueryContext(r.ctx, query, args...)
}

// queryRow runs a query that returns at most one row in the transaction.
func (r *rtx) queryRow(query string, args ...any) *sql.Row {
	return r.tx.QueryRowContext(r.ctx, query, args...)
}

// inWrite marks the context that withWrite hands its fn.
type inWrite struct{}

// withWrite runs fn in one write transaction. BEGIN IMMEDIATE takes the
// write lock first, so no transaction reads and then fails to write. It
// first reads the store's versions: any above this build's is ErrNewer, and
// nothing is written. After fn it derives again the accounts that fn
// touched (wtx.derive), and then commits. On SQLITE_BUSY from any statement or the COMMIT (a
// primary code of 5: 5, 261, 517 or 773) it rolls back and runs fn again
// after a pause, until Options.LockWait has passed (ErrBusy) or ctx ends
// (ctx's error). op says what the write does, for messages: "importing 2
// files".
//
// fn may run more than once, so it must have no effect outside the
// transaction, and what it builds is read only once withWrite returns nil.
// Code inside fn writes through the wtx and never calls the Store's own
// methods: a write inside a write would wait for ever for the one writer
// connection, and panics instead.
func (s *Store) withWrite(ctx context.Context, op string, fn func(*wtx) error) error {
	if ctx.Value(inWrite{}) != nil {
		panic("history: a write inside a write")
	}
	err := s.retry(ctx, op, func() error {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		w := &wtx{s: s, tx: tx, ctx: context.WithValue(ctx, inWrite{}, op), now: s.o.Now().UTC(), touched: map[int64]bool{}}
		v, err := readVersions(w.ctx, tx)
		if err == nil && v.newer() {
			err = s.newerError(v, false)
		}
		if err == nil {
			err = fn(w)
		}
		if err == nil {
			err = w.derive()
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	})
	return s.mapError(op, err)
}

// withRead runs fn in one read transaction on the readers' pool, retried as
// withWrite's are (only a crash's recovery can make a reader busy). It reads
// user_version first: a schema newer than this build's is ErrNewer. fn may
// run more than once, so it only reads into values that the caller reads
// once withRead returns nil: it writes nothing anywhere, stdout included.
func (s *Store) withRead(ctx context.Context, fn func(*rtx) error) error {
	const op = "reading the store"
	err := s.retry(ctx, op, func() error {
		tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // Nothing was written.
		r := &rtx{tx: tx, ctx: ctx}
		var schema int
		if err := r.queryRow("PRAGMA user_version").Scan(&schema); err != nil {
			return err
		}
		if schema > buildVersions().schema {
			return s.newerError(versions{schema: schema}, false)
		}
		return fn(r)
	})
	return s.mapError(op, err)
}

// testNoRetry, set in a test's child process, makes retry give up at the
// first SQLITE_BUSY, to show what the retries are for.
var testNoRetry bool

// testHookRetry, if set, runs at each retry, before the pause.
var testHookRetry func()

// retry runs attempt until it ends with anything but SQLITE_BUSY. Between
// attempts it pauses 25 ms, doubling each time to at most 1 s, each pause
// a random time between half and all of that, so that waiting processes
// spread out. SQLite's own wait for a lock polls rather than queues, so a
// waiter can lose to other writers for longer than any one transaction
// lasts; trying again is what makes every write land. It gives up when
// Options.LockWait has passed, with ErrBusy, or when ctx ends, with ctx's
// error, and calls Options.Waiting once when it has waited 2 s.
func (s *Store) retry(ctx context.Context, op string, attempt func() error) error {
	start := time.Now()
	pause := 25 * time.Millisecond
	told := false
	for {
		err := attempt()
		if !isBusy(err) || testNoRetry {
			return err
		}
		s.retries.Add(1)
		if testHookRetry != nil {
			testHookRetry()
		}
		waited := time.Since(start)
		if waited >= s.o.LockWait {
			return &storeError{kind: ErrBusy, err: err, msg: fmt.Sprintf("the store at %s stayed locked by another process for %v while %s; "+
				"nothing was saved; is a sync or an import running?", s.path, s.o.LockWait, op)}
		}
		if !told && waited >= 2*time.Second {
			told = true
			s.o.Waiting(op)
		}
		d := min(pause/2+rand.N(pause/2+1), s.o.LockWait-waited) //nolint:gosec // G404: the pauses need to differ, not to be secret.
		if !told {
			d = min(d, max(2*time.Second-waited, 0)) // wake up to say so
		}
		pause = min(2*pause, time.Second)
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// code returns the extended result code of an error from SQLite, or 0.
func code(err error) int {
	if e, ok := errors.AsType[*sqlite.Error](err); ok {
		return e.Code()
	}
	return 0
}

// isBusy reports whether err is SQLITE_BUSY in any of its forms. Not
// SQLITE_LOCKED: without a shared cache that is a conflict within one
// connection, a bug that a retry would hide.
func isBusy(err error) bool {
	return code(err)&0xff == sqlite3.SQLITE_BUSY
}

// mapError turns an error from SQLite into one that says what failed and
// what it means, by its primary code; any other error is returned as it is.
func (s *Store) mapError(op string, err error) error {
	c := code(err)
	if _, ok := errors.AsType[*storeError](err); c == 0 || ok {
		return err
	}
	var what string
	switch c & 0xff {
	case sqlite3.SQLITE_FULL:
		what = "the disk is full"
	case sqlite3.SQLITE_READONLY:
		what = s.path + " cannot be written"
	case sqlite3.SQLITE_CANTOPEN:
		what = s.path + " cannot be opened"
	case sqlite3.SQLITE_CONSTRAINT:
		what = "a store check refused a value (please report it): " + err.Error()
	case sqlite3.SQLITE_NOTADB, sqlite3.SQLITE_CORRUPT:
		return s.damaged(err)
	default:
		what = err.Error()
	}
	return &opError{msg: op + " failed: " + what, err: err}
}

// An opError is an error from SQLite with a message that says what failed.
type opError struct {
	msg string
	err error
}

func (e *opError) Error() string { return e.msg }
func (e *opError) Unwrap() error { return e.err }
