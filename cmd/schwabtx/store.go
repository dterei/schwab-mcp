package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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

// copyCommand returns a command of schwabtx, its words given, as a command
// prints it for you to copy: with -store and path after its first word
// when path is not the store that the command opens without -store (the
// default, or SCHWAB_STORE_FILE), so that it acts on the same store. The
// words are quoted, when they need it, by the caller.
func copyCommand(path string, words ...string) string {
	cmd := []string{"schwabtx", words[0]}
	if def, err := storePath(""); err != nil || !sameStore(def, path) {
		cmd = append(cmd, "-store", quoteArg(path))
	}
	return strings.Join(append(cmd, words[1:]...), " ")
}

// sameStore reports whether a and b, absolute paths, name one store file:
// one path, or, when both exist, one file.
func sameStore(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
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

// openStore opens the store at path, as storeOptions says, for a command
// that needs one there and makes none.
func openStore(ctx context.Context, path string, stderr io.Writer) (*history.Store, error) {
	return history.Open(ctx, storeOptions(path, true, stderr))
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
