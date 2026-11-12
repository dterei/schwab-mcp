package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
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
	// Account selects the account that the History and Realized exports
	// and the `-json` output are for: A and its id, its number, the digits
	// that end it, or its nickname. "" takes each export's from the
	// digits it and its name give, and each `-json` row's from its
	// accountNumber.
	Account string
	// From and To are the days that History exports cover, as you say,
	// for those that state no range; "" when you say none.
	From, To nytime.Date
	// Completeness says whether the History exports hold every type and
	// every symbol: "complete", "filtered", or "" when you do not say.
	Completeness string
	// DryRun works out the whole import and reports it, and then saves
	// nothing.
	DryRun bool
	// Force imports an export whose name gives digits that are not those
	// of its account, as a file you renamed would.
	Force bool
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
	// `schwabtx snapshot`; "history_csv" or "history_json", a History
	// export; "realized_csv", a Realized Gain/Loss export's Lot Details;
	// or "api_json", `schwabtx transactions -json` output.
	Kind string
	// Ref is the file's ref in the store, and Already says that the store
	// held the file before, under that ref; then nothing more was done,
	// and Forgotten says whether it is set aside.
	Ref                string
	Already, Forgotten bool
	// Account is the account an export is for, as "A2 ...456 Joint".
	Account string
	// Layout is the export's layout, and From and To the days it covers,
	// "" when none; Source says where its range came from: stated, user
	// (yours) or rows.
	Layout, Source string
	From, To       nytime.Date
	// Completeness is a History export's: complete, filtered or unknown,
	// and CompleteBy what established complete: you, api or file.
	Completeness, CompleteBy string
	// Rows counts the export's rows (or lots); New those that are
	// transactions (or lots) new to the store, and Known those that another
	// file holds too.
	Rows, New, Known int
	// Accounts says what the store made of `-json` output's rows, by
	// account.
	Accounts []APIFileAccount
	// Lines counts the lines of a snapshot file that the import read,
	// those it skipped included, and Skipped those it skipped, by why
	// (the schwabfile.Skip constants).
	Lines   int
	Skipped map[string]int
	// Snapshots says what the store made of a snapshot file's snapshots.
	Snapshots SnapshotsRecorded
	// Warnings say what the file's owner should know: of an export, each
	// of its flags with its detail; of a snapshot file, lines skipped and
	// snapshots that could not be attributed or that differ; of `-json`
	// output, rows that name no account or that differ from the store's.
	Warnings []string
}

// Import imports files in one write: each file and all of them, or
// nothing. Each is read before the write (schwabfile.Read), by its
// content, never its name: a snapshot file of `schwabtx snapshot`, a
// History export of schwab.com in CSV or JSON, a Realized Gain/Loss
// export's Lot Details, or `schwabtx transactions -json` output. A file
// that is none of them is the import's error, naming the file, and nothing
// is imported. A file that the store holds already, by its bytes, is
// reported and changes nothing. A snapshot that the store holds adds
// nothing, so importing a snapshot file again, or a longer copy of it,
// adds only what is new. The report is worked out inside the write, so
// that a dry run reports exactly what the import would do.
func (s *Store) Import(ctx context.Context, files []FileInput, o ImportOptions) (ImportReport, error) {
	if !slices.Contains([]string{"", "complete", "filtered"}, o.Completeness) {
		return ImportReport{}, fmt.Errorf("importing: completeness is complete, filtered or nothing, not %q", mask.Text(o.Completeness))
	}
	for _, d := range []nytime.Date{o.From, o.To} {
		if _, err := time.Parse(time.DateOnly, string(d)); d != "" && err != nil {
			return ImportReport{}, fmt.Errorf("importing: %q is not a date (YYYY-MM-DD)", mask.Text(string(d)))
		}
	}
	if o.From != "" && o.To != "" && o.To < o.From {
		return ImportReport{}, fmt.Errorf("importing: -from %s is after -to %s", o.From, o.To)
	}
	read := make([]*schwabfile.File, len(files))
	for i, f := range files {
		var err error
		if read[i], err = schwabfile.Read(filepath.Base(f.Name), f.Content); err != nil {
			return ImportReport{}, fmt.Errorf("%w; nothing was imported", err)
		}
	}
	op := fmt.Sprintf("importing %d %s", len(files), plural(len(files), "file", "files"))
	var report ImportReport
	err := s.withWrite(ctx, op, func(w *wtx) error {
		report = ImportReport{}
		imported := map[int]int64{} // the file of each report, by its index
		for i, in := range files {
			f := read[i]
			name := mask.FileName(filepath.Base(in.Name))
			if f.Kind == schwabfile.KindSnapshots {
				r, err := importSnapshots(w, name, f.Snapshots, f.SnapshotSkips)
				if err != nil {
					return err
				}
				report.Files = append(report.Files, r)
				continue
			}
			r := FileReport{Name: name, Kind: string(f.Kind), Layout: f.Layout}
			var id int64
			switch err := w.tx.QueryRowContext(w.ctx, "SELECT file_id FROM import_file WHERE sha256 = ?", sha256Of(in.Content)).Scan(&id); {
			case err == nil:
				r.Already = true
			case !errors.Is(err, sql.ErrNoRows):
				return err
			case f.Kind == schwabfile.KindAPIJSON:
				ar, err := importAPIJSON(w, in, f, o.Account)
				if err != nil {
					return err
				}
				r.Accounts, r.Warnings, r.Rows, id = ar.Accounts, ar.Warnings, len(f.APIRows), ar.file
			default:
				if id, err = importExport(w, in, f, o); err != nil {
					return err
				}
			}
			imported[len(report.Files)] = id
			report.Files = append(report.Files, r)
		}
		if err := w.derive(); err != nil {
			return err
		}
		for i, id := range imported {
			if err := fileReport(w, id, &report.Files[i]); err != nil {
				return err
			}
		}
		if o.DryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	return report, err
}

// fileReport fills r, the report of file id, from what the store holds of
// it once the write has derived it.
func fileReport(w *wtx, id int64, r *FileReport) error {
	var acct sql.NullInt64
	var from, to, source, completeness, by sql.NullString
	var forgotten bool
	var rows sql.NullInt64
	if err := w.tx.QueryRowContext(w.ctx, `SELECT ref, account_id, forgotten, covers_from, covers_to, coverage_source,
		completeness, complete_by, row_count FROM file_fields WHERE file_id = ?`, id).Scan(&r.Ref, &acct, &forgotten,
		&from, &to, &source, &completeness, &by, &rows); err != nil {
		return err
	}
	if acct.Valid {
		var err error
		if r.Account, err = accountLabel(w, acct.Int64); err != nil {
			return err
		}
	}
	if r.Already {
		r.Forgotten = forgotten
		msg := "already imported as " + r.Ref
		if r.Account != "" {
			msg += " for " + r.Account + "; if it is for another account: schwabtx edit " + r.Ref + " -account ACCOUNT"
		}
		if forgotten {
			msg = "it was set aside as " + r.Ref + "; bring it back with: schwabtx import -forget " + r.Ref + " -undo"
		}
		r.Warnings = append(r.Warnings, msg)
		return nil
	}
	if r.Kind == string(schwabfile.KindAPIJSON) {
		return nil
	}
	r.From, r.To, r.Source = nytime.Date(from.String), nytime.Date(to.String), source.String
	r.Completeness, r.CompleteBy, r.Rows = completeness.String, by.String, int(rows.Int64)
	query := `SELECT coalesce(sum(c.subject = 'c:' || lower(hex(f.sha256)) || ':' || r.ordinal), 0), count(c.subject)
		FROM history_row r JOIN import_file f ON f.file_id = r.file_id JOIN csv_member m ON m.row_id = r.row_id
		JOIN csv_txn c ON c.csv_txn_id = m.csv_txn_id WHERE r.file_id = ?`
	if r.Kind == string(schwabfile.KindRealized) {
		query = `SELECT coalesce(sum(c.subject = 'l:' || lower(hex(f.sha256)) || ':' || r.ordinal), 0), count(c.subject)
			FROM realized_row r JOIN import_file f ON f.file_id = r.file_id JOIN lot_member m ON m.row_id = r.row_id
			JOIN realized_lot c ON c.lot_id = m.lot_id WHERE r.file_id = ?`
	}
	if err := w.tx.QueryRowContext(w.ctx, query, id).Scan(&r.New, &r.Known); err != nil {
		return err
	}
	r.Known -= r.New
	return queryEach(w, func(rows *sql.Rows) error {
		var code string
		var detail sql.NullString
		if err := rows.Scan(&code, &detail); err != nil {
			return err
		}
		r.Warnings = append(r.Warnings, fileWarning(code, detail.String, r.Ref))
		return nil
	}, "SELECT code, detail FROM flag WHERE file_id = ? ORDER BY code", id)
}

// fileWarning says what a file's flag means for you, as an import reports
// it (20 of the design).
func fileWarning(code, detail, ref string) string {
	switch code {
	case "empty":
		return "empty: it holds no transactions; if the range had activity, the export may have exceeded a limit, reported to " +
			"give an empty file over 1,500 rows; export a shorter range; if the range really had none: schwabtx edit " + ref +
			" -completeness complete"
	case "at_cap":
		return "at_cap: " + detail + ": its oldest days may be missing, since rows come newest first; export the earlier dates separately"
	case "completeness_unknown":
		return "completeness: unknown; " + detail
	}
	if detail == "" {
		return code
	}
	return code + ": " + detail
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
