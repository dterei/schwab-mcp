package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/mask"
)

// A FileInput is a file to import: its name and its contents, which the
// command read before it opened the store. The store never opens a file
// by its path.
type FileInput struct {
	// Name is the file's name. Only its last element is kept, and only its
	// free text masked is shown.
	Name    string
	Content []byte
}

// ImportOptions says how to import files.
type ImportOptions struct {
	// DryRun works out the whole import and reports it, and then saves
	// nothing.
	DryRun bool
}

// An ImportReport says what an import made of each file, in the order the
// files were given.
type ImportReport struct {
	Files []FileReport
}

// A FileReport says what an import made of one file.
type FileReport struct {
	// Name is the file's name, its free text masked (mask.FileName).
	Name string
	// Kind is what the file is: "snapshots", a snapshot file of
	// `schwabtx snapshot`.
	Kind string
	// Lines counts the lines of a snapshot file that the import read,
	// those it skipped included, and Skipped those it skipped, by why
	// (the schwabfile.Skip constants).
	Lines   int
	Skipped map[string]int
	// Snapshots says what the store made of a snapshot file's snapshots.
	Snapshots SnapshotsRecorded
	// Warnings say what the file's owner should know: lines skipped,
	// snapshots that could not be attributed or that differ.
	Warnings []string
}

// errDryRun ends a dry run's write, so that it saves nothing.
var errDryRun = errors.New("a dry run saves nothing")

// Import imports files in one write: each file and all of them, or
// nothing. Each file is read before the write; a file that is not one the
// store imports is the import's error, naming the file, and nothing is
// imported. So far the store imports the snapshot files that `schwabtx
// snapshot` writes: each line's accounts are snapshots taken at its time,
// and a snapshot that the store holds adds nothing, so importing a file
// again, or a longer copy of it, adds only what is new. The report is
// worked out inside the write, so that a dry run reports exactly what the
// import would do.
func (s *Store) Import(ctx context.Context, files []FileInput, o ImportOptions) (ImportReport, error) {
	type snapshotFile struct {
		name    string
		lines   []schwabfile.Snapshot
		skipped map[string]int
	}
	read := make([]snapshotFile, len(files))
	for i, f := range files {
		name := mask.FileName(filepath.Base(f.Name))
		if !schwabfile.IsSnapshotFile(f.Content) {
			return ImportReport{}, fmt.Errorf("%s is not a snapshot file of `schwabtx snapshot`, the only kind of file "+
				"this schwabtx imports; nothing was imported", name)
		}
		lines, skipped := schwabfile.ReadSnapshotLines(f.Content)
		read[i] = snapshotFile{name: name, lines: lines, skipped: skipped}
	}
	op := fmt.Sprintf("importing %d %s", len(files), plural(len(files), "file", "files"))
	var report ImportReport
	err := s.withWrite(ctx, op, func(w *wtx) error {
		report = ImportReport{}
		for _, f := range read {
			r, err := importSnapshots(w, f.name, f.lines, f.skipped)
			if err != nil {
				return err
			}
			report.Files = append(report.Files, r)
		}
		if !o.DryRun {
			return nil
		}
		if err := w.derive(); err != nil {
			return err
		}
		return errDryRun
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	return report, err
}

// importSnapshots imports the lines of a snapshot file named name, which
// skipped lines by why: it records the import and each line's accounts as
// snapshots, and reports it.
func importSnapshots(w *wtx, name string, lines []schwabfile.Snapshot, skipped map[string]int) (FileReport, error) {
	r := FileReport{Name: name, Kind: "snapshots", Lines: len(lines), Skipped: maps.Clone(skipped)}
	for _, n := range skipped {
		r.Lines += n
	}
	b, err := json.Marshal(skipped)
	if err != nil {
		return r, err
	}
	var id int64
	if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO snapshot_import (label, imported_at, lines, new, known, skipped)
		VALUES (?, ?, ?, 0, 0, ?) RETURNING import_id`, name, instant(w.now), r.Lines, string(b)).Scan(&id); err != nil {
		return r, err
	}
	var evidence []snapshotEvidence
	for _, l := range lines {
		for _, a := range l.Accounts {
			evidence = append(evidence, snapshotEvidence{
				fetchedAt: l.FetchedAt, raw: a, source: "file",
				importID: id, line: int64(l.Line), lineVersion: schwabfile.SnapshotVersion,
			})
		}
	}
	if r.Snapshots, err = recordSnapshots(w, evidence); err != nil {
		return r, err
	}
	if _, err := w.exec("UPDATE snapshot_import SET new = ?, known = ? WHERE import_id = ?",
		r.Snapshots.New, r.Snapshots.Known, id); err != nil {
		return r, err
	}
	r.Warnings = append(skippedWarnings(skipped), r.Snapshots.warnings()...)
	return r, nil
}

// skippedWarnings says which lines of a snapshot file were skipped, and
// why: "3 lines skipped (2 not JSON, 1 version 2: written by a newer
// schwabtx?)", and of a last line without its newline that it may be a
// write still in progress.
func skippedWarnings(skipped map[string]int) []string {
	var ws, why []string
	n, newer := 0, ""
	for _, k := range slices.Sorted(maps.Keys(skipped)) {
		if k == schwabfile.SkipPartial {
			continue
		}
		n += skipped[k]
		why = append(why, fmt.Sprintf("%d %s", skipped[k], k))
		if v, ok := strings.CutPrefix(k, "version "); ok {
			if i, err := strconv.Atoi(v); err == nil && i > schwabfile.SnapshotVersion {
				newer = ": written by a newer schwabtx?"
			}
		}
	}
	if n > 0 {
		ws = append(ws, fmt.Sprintf("%d %s skipped (%s%s)", n, plural(n, "line", "lines"), strings.Join(why, ", "), newer))
	}
	if skipped[schwabfile.SkipPartial] > 0 {
		ws = append(ws, "the last line has no newline (a snapshot being written?); it was skipped")
	}
	return ws
}
