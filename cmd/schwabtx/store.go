package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// storeLockWait and storeBusyTimeout, if set, stand in for how long a write
// to the store waits for another process that holds it, and how long
// SQLite itself waits each time, in tests.
var storeLockWait, storeBusyTimeout time.Duration

// storeFlag adds -store to fs.
func storeFlag(fs *flag.FlagSet) *string {
	return fs.String("store", "", "the store's file (default: $SCHWAB_STORE_FILE, else history.db in schwabtx's folder of your config directory)")
}

// storePath returns the store's file: the -store flag, else
// SCHWAB_STORE_FILE, else history.db in schwabtx's folder of the user
// config directory (%LocalAppData% on Windows), made absolute.
func storePath(flagValue string) (string, error) {
	p := strings.TrimSpace(flagValue)
	if p == "" {
		var err error
		if p, err = history.DefaultPath(); err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("finding the store's path: %w", err)
	}
	return abs, nil
}

// storeOptions returns the options that open the store at path: warnings
// go to stderr, and so does, once, that a write waits for another process.
// A read command (mustExist) refuses a missing store rather than make one.
func storeOptions(path string, mustExist bool, stderr io.Writer) history.Options {
	var once sync.Once
	return history.Options{
		Path:      path,
		MustExist: mustExist,
		Now:       func() time.Time { return timeNow() },
		Warn:      func(msg string) { fmt.Fprintln(stderr, "warning:", msg) },
		Waiting: func(string) {
			once.Do(func() { fmt.Fprintln(stderr, "waiting for another schwabtx process that is writing to the store...") })
		},
		LockWait:    storeLockWait,
		BusyTimeout: storeBusyTimeout,
	}
}

// openStore opens the store at path, as storeOptions says, and says so on
// stderr when it made a new one.
func openStore(ctx context.Context, path string, mustExist bool, stderr io.Writer) (*history.Store, error) {
	_, statErr := os.Stat(path)
	s, err := history.Open(ctx, storeOptions(path, mustExist, stderr))
	if err != nil {
		return nil, err
	}
	if errors.Is(statErr, fs.ErrNotExist) {
		fmt.Fprintln(stderr, "new store at", path)
	}
	return s, nil
}

// parseInterspersed parses args with fs, flags before or after the other
// arguments, as in `schwabtx edit T73508261949 -category fee`, and returns
// the other arguments in order. After "--" every argument is another.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		left := fs.Args()
		if len(left) == 0 {
			return rest, nil
		}
		if used := len(args) - len(left); used > 0 && args[used-1] == "--" {
			return append(rest, left...), nil
		}
		rest, args = append(rest, left[0]), left[1:]
	}
}

// parseDay reads a date given to a flag: YYYY-MM-DD, or MM/DD/YYYY as
// schwab.com writes one. "" is no date.
func parseDay(name, v string) (nytime.Date, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	for _, layout := range []string{time.DateOnly, "01/02/2006"} {
		if d, err := time.Parse(layout, v); err == nil {
			return nytime.Date(d.Format(time.DateOnly)), nil
		}
	}
	return "", fmt.Errorf("%s %q is not a date: give YYYY-MM-DD or MM/DD/YYYY", name, plainCell(v))
}

// quoteArg quotes a word of a command printed for you to copy, when the
// shell that this system runs would split it or read something in it: in
// single quotes on Linux and macOS, and in double quotes on Windows.
func quoteArg(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789:@._/+-,=") == "" {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// companionOf reports whether path, an input or output of a command, is
// the store at store or one of its files (history.IsCompanion), by name or
// as the same file under another, and names which: a process that has the
// store open must not open its files by path, so a command checks this
// before it opens the store.
func companionOf(path, store string) (string, bool) {
	if !history.IsCompanion(path, store) {
		return "", false
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(strings.ToLower(name), strings.ToLower(filepath.Base(store))) {
		name = filepath.Base(store) // the same file under another name
	}
	return name, true
}
