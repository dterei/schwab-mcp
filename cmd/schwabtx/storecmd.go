package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history"
)

// runStoreCommand looks after the store itself: it checks it, derives
// everything again from the evidence, backs it up, or restores a backup.
func runStoreCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("schwabtx store", flag.ContinueOnError)
	fl.SetOutput(stderr)
	store := storeFlag(fl)
	dryRun := fl.Bool("dry-run", false, "with restore, say what the store and the backup hold, and change nothing")
	fl.Usage = func() {
		fmt.Fprint(fl.Output(), `Usage of schwabtx store:

  schwabtx store [-store PATH] check            check the store: SQLite's integrity and foreign keys, and
                                                what the derived data must hold; fails on any problem
  schwabtx store [-store PATH] rebuild          derive everything again from the evidence
  schwabtx store [-store PATH] backup FILE      copy the store, as of now, to a new file readable only by you
  schwabtx store [-store PATH] restore BACKUP   replace what the store holds with what BACKUP holds

Copy the store only with backup, and put a backup back only with restore:
the store's file, its -wal and its -shm are one thing, which a copy of the
file alone can damage.

`)
		fl.PrintDefaults()
	}
	rest, err := parseInterspersed(fl, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		fl.Usage()
		return errors.New("store needs a command: check, rebuild, backup FILE or restore BACKUP")
	}
	sub, rest := rest[0], rest[1:]
	want := map[string]int{"check": 0, "rebuild": 0, "backup": 1, "restore": 1}
	n, ok := want[sub]
	switch {
	case !ok:
		return fmt.Errorf("unknown store command %q; it is check, rebuild, backup FILE or restore BACKUP", plainCell(sub))
	case len(rest) < n:
		return fmt.Errorf("store %s needs a file: schwabtx store %s %s", sub, sub, strings.ToUpper(map[string]string{"backup": "file", "restore": "backup"}[sub]))
	case len(rest) > n:
		return fmt.Errorf("unexpected argument %q", plainCell(rest[n]))
	case *dryRun && sub != "restore":
		return errors.New("-dry-run goes with store restore")
	}
	path, err := storePath(*store)
	if err != nil {
		return err
	}
	if sub == "restore" {
		return restoreStore(ctx, path, rest[0], *dryRun, stdout, stderr)
	}
	if sub == "backup" {
		if _, ok := companionOf(rest[0], path); ok {
			return fmt.Errorf("%s would replace the store itself (or one of its files); choose another name", rest[0])
		}
	}
	s, err := openStore(ctx, path, true, stderr)
	if err != nil {
		return err
	}
	defer s.Close()
	switch sub {
	case "check":
		r, err := s.Check(ctx)
		if err != nil {
			return err
		}
		for _, w := range r.Warnings {
			fmt.Fprintln(stderr, "warning:", w)
		}
		if len(r.Problems) == 0 {
			fmt.Fprintln(stdout, "the store is sound:", path)
			return nil
		}
		for _, p := range r.Problems {
			fmt.Fprintln(stdout, plainCell(p))
		}
		return fmt.Errorf("the store at %s has %s; schwabtx store rebuild derives its data again, and a damaged file needs schwabtx store restore",
			path, plural(len(r.Problems), "problem"))
	case "rebuild":
		if err := s.Rebuild(ctx); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "derived everything in", path, "again from the evidence")
		return nil
	}
	if err := s.Backup(ctx, rest[0]); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "backed the store up to %s; schwabtx store restore %s puts it back\n", rest[0], quoteArg(rest[0]))
	return nil
}

// restoreStore replaces what the store at path holds with what the backup
// holds, through SQLite, or with dryRun says what each holds, and makes a
// store that is missing.
func restoreStore(ctx context.Context, path, backup string, dryRun bool, stdout, stderr io.Writer) error {
	_, statErr := os.Stat(path)
	r, err := history.Restore(ctx, storeOptions(path, false, stderr), backup, dryRun)
	if err != nil {
		return err
	}
	if !dryRun && errors.Is(statErr, fs.ErrNotExist) {
		fmt.Fprintln(stderr, "new store at", path)
	}
	fmt.Fprintf(stdout, "the store held %s\n", r.Before)
	if dryRun {
		fmt.Fprintf(stdout, "the backup holds %s\nnothing was changed (-dry-run)\n", r.After)
		return nil
	}
	fmt.Fprintf(stdout, "it now holds %s\n", r.After)
	return nil
}
