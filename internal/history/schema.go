package history

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"runtime/debug"
	"strconv"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

//go:embed schema/*.sql
var schemaFiles embed.FS

// A migration brings a store from one schema to the next: its SQL, then
// its Go step, if any, in the same transaction. A step must be able to run
// again, since the transaction may.
type migration struct {
	sql  string
	step func(*wtx) error
}

// migrations are the store's schemas in order: a store's user_version is
// how many of them it has had. Tests in a child process may add one.
var migrations = []migration{
	{sql: schemaFile("schema/001.sql")},
}

// schemaFile returns the SQL of an embedded schema file, with its macros
// expanded.
func schemaFile(name string) string {
	b, err := fs.ReadFile(schemaFiles, name)
	if err != nil {
		panic(err) // embedded: it is there
	}
	return expandMacros(string(b))
}

// macro matches INSTANT(c) and DAY(c), which the schema files use in their
// checks.
var macro = regexp.MustCompile(`\b(INSTANT|DAY)\(([a-z_]+)\)`)

// The patterns of an instant, UTC text of fixed width so that text order is
// time order, and of a New York calendar date.
const (
	instantGlob = `'[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]T[0-2][0-9]:[0-5][0-9]:[0-6][0-9].[0-9][0-9][0-9]Z'`
	dayGlob     = `'[0-9][0-9][0-9][0-9]-[0-1][0-9]-[0-3][0-9]'`
)

// expandMacros writes INSTANT(c) and DAY(c) out as GLOB checks, so that the
// schema stored in the file holds only what any SQLite from 3.37 reads.
func expandMacros(sql string) string {
	return macro.ReplaceAllStringFunc(sql, func(m string) string {
		p := macro.FindStringSubmatch(m)
		if p[1] == "INSTANT" {
			return p[2] + " GLOB " + instantGlob
		}
		return p[2] + " GLOB " + dayGlob
	})
}

// migrate brings the store's schema up to this build's. Another process
// may be doing the same: the lock file makes them take turns, and each
// reads the schema's version again once it holds the lock, so the store is
// migrated, and backed up, once. A store that holds data is backed up
// first; a crash while migrating rolls the migration back and leaves the
// backup.
func (s *Store) migrate(ctx context.Context) error {
	const op = "updating the store's schema"
	lock, err := filelock.Acquire(ctx, s.path+".migrate.lock", s.o.LockWait)
	switch {
	case errors.Is(err, filelock.ErrTimeout):
		return &storeError{kind: ErrBusy, err: err, msg: fmt.Sprintf("the store at %s stayed locked by another process for %v while %s; "+
			"nothing was saved; is a sync or an import running?", s.path, s.o.LockWait, op)}
	case errors.Is(err, filelock.ErrUnsupported):
		// The store's checks have refused the network file systems that
		// cannot lock; withWrite's second read of the version below still
		// keeps two processes from migrating twice.
	case err != nil:
		return fmt.Errorf("%s: %w", op, err)
	default:
		defer lock.Release()
	}
	var v versions
	if err := s.retry(ctx, op, func() (err error) {
		v, err = readVersions(ctx, s.w)
		return err
	}); err != nil {
		return s.mapError(op, err)
	}
	b := buildVersions()
	switch {
	case v.schema > b.schema:
		return s.newerError(v, true)
	case v.schema == b.schema:
		s.opened = v
		return nil
	case v.schema > 0:
		if err := s.backup(ctx, v.schema); err != nil {
			return err
		}
	}
	err = s.withWrite(ctx, op, func(w *wtx) error {
		v, err := readVersions(w.ctx, w.tx)
		if err != nil || v.schema >= b.schema {
			return err
		}
		for _, m := range migrations[v.schema:] {
			if _, err := w.exec(m.sql); err != nil {
				return err
			}
			if m.step != nil {
				if err := m.step(w); err != nil {
					return err
				}
			}
		}
		// Neither pragma can take a bound parameter. They commit with the
		// schema, or roll back with it.
		if _, err := w.exec("PRAGMA user_version = " + strconv.Itoa(b.schema)); err != nil {
			return err
		}
		if v.schema > 0 {
			return nil
		}
		if _, err := w.exec("PRAGMA application_id = " + strconv.Itoa(applicationID)); err != nil {
			return err
		}
		_, err = w.exec("INSERT INTO store (id, created_at, created_by, derived_with, calendar_version) VALUES (1, ?, ?, ?, ?)",
			instant(w.now), build(), b.derive, b.calendar)
		return err
	})
	if err != nil {
		return err
	}
	return s.retry(ctx, op, func() (err error) {
		s.opened, err = readVersions(ctx, s.w)
		return err
	})
}

// backup copies the store, whose schema is version, to a new file beside
// it, readable only by you, before its schema changes.
func (s *Store) backup(ctx context.Context, version int) error {
	name := fmt.Sprintf("%s.schema%d.%s.bak", s.path, version, s.o.Now().UTC().Format("20060102T150405Z"))
	const op = "backing the store up before updating its schema"
	if err := s.vacuumInto(ctx, op, name); err != nil {
		return err
	}
	s.o.Warn("backed the store up to " + name + " before updating its schema")
	return nil
}

// update brings the store's derived data and market dates up to this
// build's rules and calendar. Each is decided inside the write that does
// it, so that when two processes open the store at once one does the work
// and the other finds it done. A store that a newer schwabtx has derived is
// left as it is: it can be read, and writes refuse it.
func (s *Store) update(ctx context.Context) error {
	const op = "updating the store's derived data"
	b := buildVersions()
	if s.opened.newer() || s.opened.derive >= b.derive && s.opened.calendar >= b.calendar {
		return nil
	}
	var did string
	err := s.withWrite(ctx, op, func(w *wtx) error {
		did = ""
		v, err := readVersions(w.ctx, w.tx)
		switch {
		case err != nil:
			return err
		case v.derive < b.derive:
			did = fmt.Sprintf("updated the store's derived data for this schwabtx (rules %d -> %d)", v.derive, b.derive)
			return rebuild(w)
		case v.calendar < b.calendar:
			did = fmt.Sprintf("recomputed market dates for calendar %d", b.calendar)
			return recalendar(w)
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrNewer) {
		return err
	}
	if did != "" && err == nil {
		s.o.Warn(did)
	}
	if err := s.retry(ctx, op, func() (err error) {
		s.opened, err = readVersions(ctx, s.w)
		return err
	}); err != nil {
		return s.mapError(op, err)
	}
	if s.opened.schema > b.schema {
		return s.newerError(s.opened, true)
	}
	return nil
}

// build names this schwabtx, for the store's record of what made it.
func build() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return "schwabtx " + info.Main.Version
	}
	return "schwabtx"
}
