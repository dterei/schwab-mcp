package history

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// An exportTable is one of the tables that Export writes: a view of the
// store, the order of its rows, what the options filter it by, and which
// raw bytes -raw adds to it.
type exportTable struct {
	// name is the table's name in -what, and about what it holds, for the
	// help.
	name, about string
	// view is the view it holds, and allView the one it holds under -all
	// when that is another.
	view, allView string
	// order is the columns that order its rows, so that each order is
	// total: an export of a store that has not changed is the same bytes.
	order []string
	// day is the column that -from and -to bound, if any; span the two
	// columns of a range that must meet theirs, if any.
	day  string
	span [2]string
	// counted says that only counted rows are in it unless -all.
	counted bool
	// noAccount says that its rows are of no account, so that -account
	// does not filter them.
	noAccount bool
	// raw is the raw bytes -raw adds to it, if any.
	raw rawKind
}

// A rawKind says which raw bytes -raw adds to a table's rows.
type rawKind int

const (
	rawNone rawKind = iota
	// rawEntry adds an entry's: an API transaction's latest raw JSON
	// (raw) and its first (first_raw), or a file transaction's row as
	// the newest export holds it (raw).
	rawEntry
	// rawFileRow adds a file transaction's row as the newest export
	// holds it.
	rawFileRow
	// rawSnapshot adds the snapshot's element as the snapshot file held it.
	rawSnapshot
	// rawLot adds a closed lot's row as the newest export holds it.
	rawLot
)

// exportTables are the tables that Export writes, in the order of the help;
// the help and every error that lists them are made from it.
var exportTables = []exportTable{
	{
		name: "ledger", view: "ledger", allView: "entries", about: "counted entries, from the API and from files: what a calculator reads",
		order: []string{"day", "time", "account_id", "ref"}, day: "day", raw: rawEntry,
	},
	{
		name: "entries", view: "entries", about: "every entry, with whether it is counted and why",
		order: []string{"day", "time", "account_id", "ref"}, day: "day", raw: rawEntry,
	},
	{
		name: "transactions", view: "transactions", about: "Schwab's API transactions, one row each",
		order: []string{"time", "activity_id", "account_id", "ref"}, day: "trade_day", raw: rawEntry,
	},
	{
		name: "legs", view: "legs", about: "one row per leg, the API's and those made for file transactions; of counted entries unless -all",
		order: []string{"day", "ref", "account_id", "leg"}, day: "day", counted: true,
	},
	{
		name: "file-transactions", view: "file_transactions", about: "the transactions of History files, each once",
		order: []string{"posted_date", "ref", "account_id"}, day: "event_date", raw: rawFileRow,
	},
	{
		name: "imports", view: "files", about: "the files imported from schwab.com and schwabtx",
		order: []string{"imported_at", "ref"},
	},
	{
		name: "snapshots", view: "snapshots", about: "snapshots of the accounts, with the market date each stands for",
		order: []string{"fetched_at", "account_id", "snapshot_id"}, day: "market_date", raw: rawSnapshot,
	},
	{
		name: "positions", view: "positions", about: "the positions of each snapshot",
		order: []string{"fetched_at", "account_id", "snapshot_id", "position"}, day: "market_date",
	},
	{
		name: "balances", view: "balances", about: "every balance figure of each snapshot",
		order: []string{"fetched_at", "account_id", "snapshot_id", "balance_set", "figure"}, day: "market_date",
	},
	{
		name: "lots", view: "realized_lots", about: "closed lots from Realized Gain/Loss files, each once",
		order: []string{"closed_date", "ref", "account_id"}, day: "closed_date", raw: rawLot,
	},
	{
		name: "gaps", view: "snapshot_gaps", about: "market sessions that no snapshot of an account stands for",
		order: []string{"market_date", "account_id"}, day: "market_date",
	},
	{
		name: "flags", view: "flags", about: "what the store flagged, with why",
		order: []string{"account_id", "subject_ref", "code", "detail"},
	},
	{
		name: "coverage", view: "coverages", about: "the days each source covers, per account",
		order: []string{"account_id", "source", "from_date"}, span: [2]string{"from_date", "to_date"},
	},
	{
		name: "securities", view: "securities", about: "the securities, with their latest symbol, CUSIP and name",
		order: []string{"security_key"}, noAccount: true,
	},
}

// An ExportKind is one of the tables that Export writes.
type ExportKind struct {
	// Name is how -what names it, and About what it holds.
	Name, About string
	// Raw says that -raw adds raw bytes to it.
	Raw bool
}

// ExportKinds returns the tables that Export writes, in the order of the
// help.
func ExportKinds() []ExportKind {
	out := make([]ExportKind, len(exportTables))
	for i, t := range exportTables {
		out[i] = ExportKind{Name: t.name, About: t.about, Raw: t.raw != rawNone}
	}
	return out
}

// ExportOptions says what Export reads.
type ExportOptions struct {
	// What names the tables (ExportKinds): one, several, or "all". None
	// means the ledger.
	What []string
	// Format is "csv", or "jsonl" for JSON Lines. "" means csv.
	Format string
	// Account selects one account, as ImportOptions.Account does: A and its
	// id, its number, the digits that end it, or its nickname. "" means
	// every account.
	Account string
	// From and To bound the rows' days, each "" for no bound: an entry's
	// or a leg's day, a transaction's trade day, a file transaction's
	// event date, a snapshot's market date, a lot's closed date; a
	// coverage that meets them. Imports, flags and securities have no day.
	From, To nytime.Date
	// All exports every entry in the ledger and every leg, counted or not.
	All bool
	// Raw adds the raw bytes of each row: Schwab's JSON, a snapshot's
	// element, or a file's row. They hold full account numbers.
	Raw bool
	// ShowAccounts adds each row's full account number, as
	// account_number.
	ShowAccounts bool
}

// An Export is what Export read from the store, held in memory until it is
// written.
type Export struct {
	// At is when it was read.
	At time.Time
	// Format is how it is written: "csv" or "jsonl".
	Format string
	// Tables are its tables, in the order asked for.
	Tables []ExportedTable
	// Coverage is the days each source covers, of the accounts exported.
	Coverage []ExportCoverage
	// Waiting counts the file transactions inside the rows' days that wait
	// for you to resolve them (held out, or uncertain which API
	// transaction they are), which no export of counted rows holds.
	Waiting int
	// HoldsNumbers says that it holds full account numbers (-raw or
	// -show-accounts).
	HoldsNumbers bool

	opts     ExportOptions
	account  string // the account exported, as "A2 ...456", or ""
	versions versions
	store    string
}

// An ExportedTable is one table of an export.
type ExportedTable struct {
	// Name is how -what names it, View the view it holds, and File its
	// file's name in a directory.
	Name, View, File string
	// Columns are its header: the view's columns in the view's order, with
	// *_cents and *_micros named without the suffix, and account_number,
	// raw and first_raw when asked for.
	Columns []string
	// Rows counts its rows.
	Rows int

	rows [][]cell
}

// An ExportCoverage is a run of days that a source covers for an account.
type ExportCoverage struct {
	// AccountID is the account's id, and Account its number masked.
	AccountID int64  `json:"account_id"`
	Account   string `json:"account"`
	// Source is what covers the days: api, api_file, file, file_unknown,
	// file_filtered, realized or snapshot (the coverages view).
	Source string      `json:"source"`
	From   nytime.Date `json:"from"`
	To     nytime.Date `json:"to"`
}

// A cell is a value of an exported row, as CSV and JSON Lines write it.
type cell struct {
	text string
	kind cellKind
}

// A cellKind says how JSON Lines writes a cell.
type cellKind int

const (
	cellNull   cellKind = iota // null; an empty CSV cell
	cellNumber                 // a number, written as its text
	cellString                 // a string
	cellJSON                   // JSON, written as it is (oneLine)
)

// Export reads what o asks for into memory, in one read transaction, so
// that its tables agree with one another; the transaction has ended when
// it returns, and only then is anything written (WriteTo, WriteFile,
// WriteDir), so that a slow reader of the output holds no transaction
// open. Each table holds a view's columns in its order: money as dollars
// with two places, from the store's whole cents, and quantities from its
// whole millionths, each formatted from the integer and so exact; other
// numbers as briefly as they read back; dates as they are; times in UTC,
// in RFC 3339, with milliseconds only when there are any.
func (s *Store) Export(ctx context.Context, o ExportOptions) (*Export, error) {
	tables, err := o.tables()
	if err != nil {
		return nil, err
	}
	var e *Export
	err = s.withRead(ctx, func(r *rtx) error {
		// A read may run again: everything it builds is built afresh.
		e = &Export{At: s.o.Now().UTC(), Format: o.Format, HoldsNumbers: o.Raw || o.ShowAccounts, opts: o, store: s.path}
		if e.Format == "" {
			e.Format = "csv"
		}
		var err error
		if e.versions, err = readVersions(r.ctx, r.tx); err != nil {
			return err
		}
		var account sql.NullInt64
		if o.Account != "" {
			// selectAccount only reads through its wtx when it may not add
			// an account, and the readers' connections refuse a write.
			id, err := selectAccount(&wtx{tx: r.tx, ctx: r.ctx}, o.Account, false)
			if err != nil {
				return err
			}
			account = sql.NullInt64{Int64: id, Valid: true}
			if err := r.queryRow("SELECT 'A' || account_id || ' ' || masked FROM account WHERE account_id = ?", id).Scan(&e.account); err != nil {
				return err
			}
		}
		for _, t := range tables {
			et, err := exportRows(r, t, o, account)
			if err != nil {
				return err
			}
			e.Tables = append(e.Tables, et)
		}
		if e.Coverage, err = exportCoverage(r, account); err != nil {
			return err
		}
		where, args := exportFilter(exportTable{day: "day"}, o, account)
		return r.queryRow(`SELECT count(*) FROM entries v WHERE v.source = 'file' AND v.counted_why IN ('held_out', 'uncertain')`+
			strings.Replace(where, " WHERE ", " AND ", 1), args...).Scan(&e.Waiting)
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// tables checks the options and returns the tables they ask for, each
// once, in the order asked.
func (o ExportOptions) tables() ([]exportTable, error) {
	if o.Format != "" && o.Format != "csv" && o.Format != "jsonl" {
		return nil, fmt.Errorf("-format is csv or jsonl, not %q", mask.Text(o.Format))
	}
	for _, d := range []nytime.Date{o.From, o.To} {
		if _, err := time.Parse(time.DateOnly, string(d)); d != "" && err != nil {
			return nil, fmt.Errorf("%q is not a date (YYYY-MM-DD)", mask.Text(string(d)))
		}
	}
	if o.From != "" && o.To != "" && o.To < o.From {
		return nil, fmt.Errorf("-from %s is after -to %s", o.From, o.To)
	}
	what := o.What
	if len(what) == 0 {
		what = []string{"ledger"}
	}
	var out []exportTable
	for _, w := range what {
		w = strings.TrimSpace(w)
		if w == "all" {
			for _, t := range exportTables {
				if !slices.ContainsFunc(out, func(u exportTable) bool { return u.name == t.name }) {
					out = append(out, t)
				}
			}
			continue
		}
		i := slices.IndexFunc(exportTables, func(t exportTable) bool { return t.name == w })
		if i < 0 {
			return nil, fmt.Errorf("unknown -what %q; one of %s, or all", mask.Text(w), strings.Join(exportNames(func(exportTable) bool { return true }), ", "))
		}
		t := exportTables[i]
		if o.Raw && t.raw == rawNone {
			return nil, fmt.Errorf("-raw is kept for %s", joinAnd(exportNames(func(t exportTable) bool { return t.raw != rawNone })))
		}
		if !slices.ContainsFunc(out, func(u exportTable) bool { return u.name == t.name }) {
			out = append(out, t)
		}
	}
	return out, nil
}

// exportNames returns the names of the tables that keep says to.
func exportNames(keep func(exportTable) bool) []string {
	var names []string
	for _, t := range exportTables {
		if keep(t) {
			names = append(names, t.name)
		}
	}
	return names
}

// exportFilter returns the WHERE clause that filters table t's view, named
// v, by the options, and its arguments.
func exportFilter(t exportTable, o ExportOptions, account sql.NullInt64) (string, []any) {
	var conds []string
	var args []any
	if account.Valid && !t.noAccount {
		conds = append(conds, "v.account_id = ?")
		args = append(args, account.Int64)
	}
	from, to := t.day, t.day
	if t.span[0] != "" {
		// A range meets [From, To] when it ends on or after From and starts
		// on or before To.
		from, to = t.span[1], t.span[0]
	}
	if o.From != "" && from != "" {
		conds = append(conds, "v."+from+" >= ?")
		args = append(args, string(o.From))
	}
	if o.To != "" && to != "" {
		conds = append(conds, "v."+to+" <= ?")
		args = append(args, string(o.To))
	}
	if t.counted && !o.All {
		conds = append(conds, "v.counted = 1")
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// The joins and columns that -raw and -show-accounts add to a table's
// view, v. Each raw comes with raw_json, which says whether it is JSON;
// a file's row is JSON only in a History export in JSON.
const (
	joinEntryRaw = ` LEFT JOIN entry x ON x.account_id = v.account_id AND x.ref = v.ref
		LEFT JOIN api_txn xt ON xt.txn_id = x.txn_id
		LEFT JOIN csv_txn xc ON xc.csv_txn_id = x.csv_txn_id
		LEFT JOIN history_row xh ON xh.row_id = xc.shown_row_id
		LEFT JOIN import_file xf ON xf.file_id = xh.file_id`
	columnsEntryRaw = `coalesce(xt.latest_raw, xh.raw) AS raw,
		xt.txn_id IS NOT NULL OR xf.kind = 'history_json' AS raw_json`
	joinSnapshotRaw    = ` LEFT JOIN snapshot xs ON xs.snapshot_id = v.snapshot_id`
	columnsSnapshotRaw = `xs.raw AS raw, 1 AS raw_json`
	joinLotRaw         = ` LEFT JOIN realized_lot xl ON xl.account_id = v.account_id AND xl.ref = v.ref
		LEFT JOIN realized_row xr ON xr.row_id = xl.shown_row_id`
	columnsLotRaw = `xr.raw AS raw, 0 AS raw_json`
	joinNumber    = ` LEFT JOIN account xa ON xa.account_id = v.account_id`
)

// exportRows reads table t's rows into memory.
func exportRows(r *rtx, t exportTable, o ExportOptions, account sql.NullInt64) (ExportedTable, error) {
	view := t.view
	if o.All && t.allView != "" {
		view = t.allView
	}
	ext := "." + o.Format
	if o.Format == "" {
		ext = ".csv"
	}
	et := ExportedTable{Name: t.name, View: view, File: t.name + ext}
	columns := []string{"v.*"}
	joins := ""
	if o.Raw {
		switch t.raw {
		case rawEntry:
			joins += joinEntryRaw
			columns = append(columns, columnsEntryRaw, "xt.first_raw AS first_raw")
		case rawFileRow:
			joins += joinEntryRaw
			columns = append(columns, columnsEntryRaw)
		case rawSnapshot:
			joins += joinSnapshotRaw
			columns = append(columns, columnsSnapshotRaw)
		case rawLot:
			joins += joinLotRaw
			columns = append(columns, columnsLotRaw)
		}
	}
	if o.ShowAccounts && !t.noAccount {
		joins += joinNumber
		columns = append(columns, "xa.number AS account_number")
	}
	order := make([]string, len(t.order))
	for i, c := range t.order {
		order[i] = "v." + c
	}
	where, args := exportFilter(t, o, account)
	rows, err := r.query("SELECT "+strings.Join(columns, ", ")+" FROM "+view+" v"+joins+where+" ORDER BY "+strings.Join(order, ", "), args...)
	if err != nil {
		return et, err
	}
	defer rows.Close()
	names, err := rows.Columns()
	if err != nil {
		return et, err
	}
	// The columns as written: the view's, account_number after the
	// account's, and the raw bytes last; raw_json is not written.
	var keep []int
	jsonAt := slices.Index(names, "raw_json")
	numberAt := slices.Index(names, "account_number")
	for i, n := range names {
		if i != jsonAt && i != numberAt && n != "raw" && n != "first_raw" {
			keep = append(keep, i)
		}
	}
	if numberAt >= 0 {
		after := slices.Index(names, "account")
		if after < 0 {
			after = slices.Index(names, "account_id")
		}
		at := slices.Index(keep, after) + 1
		keep = slices.Insert(keep, at, numberAt)
	}
	for i, n := range names {
		if n == "raw" || n == "first_raw" {
			keep = append(keep, i)
		}
	}
	for _, i := range keep {
		et.Columns = append(et.Columns, exportedColumn(names[i]))
	}
	values := make([]any, len(names))
	ptrs := make([]any, len(names))
	for i := range values {
		ptrs[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return et, err
		}
		isJSON := jsonAt >= 0 && values[jsonAt] == int64(1)
		row := make([]cell, len(keep))
		for j, i := range keep {
			c, err := exportCell(names[i], values[i], isJSON)
			if err != nil {
				return et, fmt.Errorf("exporting %s: %w", t.name, err)
			}
			row[j] = c
		}
		et.rows = append(et.rows, row)
	}
	et.Rows = len(et.rows)
	return et, rows.Err()
}

// exportedColumn returns a view's column as an export names it: a *_cents or
// *_micros column without its suffix, since the export writes dollars and
// whole units.
func exportedColumn(column string) string {
	if c, ok := strings.CutSuffix(column, "_cents"); ok {
		return c
	}
	if c, ok := strings.CutSuffix(column, "_micros"); ok {
		return c
	}
	return column
}

// isInstantColumn reports whether a view's column holds instants, which
// the export writes in RFC 3339.
func isInstantColumn(column string) bool {
	return strings.HasSuffix(column, "_at") || column == "time" || column == "gone_since"
}

// exportCell returns the value v of column as the export writes it. A raw
// column is JSON when isJSON says so and it is valid JSON, and text
// otherwise.
func exportCell(column string, v any, isJSON bool) (cell, error) {
	switch v := v.(type) {
	case nil:
		return cell{kind: cellNull}, nil
	case int64:
		switch {
		case strings.HasSuffix(column, "_cents"):
			return cell{text: formatCents(v), kind: cellNumber}, nil
		case strings.HasSuffix(column, "_micros"):
			return cell{text: formatMicros(v), kind: cellNumber}, nil
		}
		return cell{text: strconv.FormatInt(v, 10), kind: cellNumber}, nil
	case float64:
		if strings.HasSuffix(column, "_cents") || strings.HasSuffix(column, "_micros") {
			return cell{}, fmt.Errorf("%s holds %v, not a whole number (please report it)", column, v)
		}
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if math.IsInf(v, 0) || math.IsNaN(v) {
			return cell{text: text, kind: cellString}, nil
		}
		return cell{text: text, kind: cellNumber}, nil
	case []byte:
		return exportCell(column, string(v), isJSON)
	case string:
		switch {
		case column == "raw" || column == "first_raw":
			if isJSON && json.Valid([]byte(v)) {
				return cell{text: v, kind: cellJSON}, nil
			}
		case isInstantColumn(column):
			if t, err := parseInstant(v); err == nil {
				return cell{text: exportInstant(t), kind: cellString}, nil
			}
		}
		return cell{text: v, kind: cellString}, nil
	}
	return cell{}, fmt.Errorf("%s holds a value of type %T (please report it)", column, v)
}

// exportInstant writes an instant in RFC 3339, in UTC, with its
// milliseconds when it has any.
func exportInstant(t time.Time) string {
	if t.Nanosecond() == 0 {
		return t.UTC().Format(time.RFC3339)
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// oneLine returns valid JSON as it is, unless it spans lines, which a line
// of JSON Lines cannot: then with the white space between its tokens
// removed, every token kept as it was.
func oneLine(raw string) string {
	if !strings.ContainsAny(raw, "\r\n") {
		return raw
	}
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(raw)); err != nil {
		return raw
	}
	return b.String()
}

// exportCoverage returns the coverage of the account, or of every account
// when it is not valid.
func exportCoverage(r *rtx, account sql.NullInt64) ([]ExportCoverage, error) {
	rows, err := r.query(`SELECT account_id, account, source, from_date, to_date FROM coverages
		WHERE ?1 IS NULL OR account_id = ?1 ORDER BY account_id, source, from_date`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportCoverage
	for rows.Next() {
		var c ExportCoverage
		if err := rows.Scan(&c.AccountID, &c.Account, &c.Source, &c.From, &c.To); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// WriteTo writes the export's one table to w, as its format says, and
// returns how many bytes it wrote. An export of several tables is written
// with WriteDir.
func (e *Export) WriteTo(w io.Writer) (int64, error) {
	if err := e.one(); err != nil {
		return 0, err
	}
	cw := &countingWriter{w: w}
	err := e.Tables[0].write(cw, e.Format)
	return cw.n, err
}

// WriteFile writes the export's one table to the file at path, readable
// only by you, through a temporary file beside it that is synced and then
// renamed over it: an export that fails midway leaves path as it was. A
// path that is one of the store's files, or would be taken for one, is
// refused before anything is written.
func (e *Export) WriteFile(path string) error {
	if err := e.one(); err != nil {
		return err
	}
	if err := e.notTheStore(path); err != nil {
		return err
	}
	return writePrivate(path, func(w io.Writer) error { return e.Tables[0].write(w, e.Format) })
}

// WriteDir writes each of the export's tables to a file of its own in dir
// (ExportedTable.File), and manifest.json, which says when the export was
// read, by which versions of the store's schema, rules and calendar, with
// which options, how many rows each table holds, the units, and what each
// source covers of each account. dir is made, readable only by you, if it
// is missing; each file is written as WriteFile writes one.
func (e *Export) WriteDir(dir string) error {
	names := []string{"manifest.json"}
	for _, t := range e.Tables {
		names = append(names, t.File)
	}
	for _, n := range names {
		if err := e.notTheStore(filepath.Join(dir, n)); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making %s: %w", dir, err)
	}
	for _, t := range e.Tables {
		if err := writePrivate(filepath.Join(dir, t.File), func(w io.Writer) error { return t.write(w, e.Format) }); err != nil {
			return err
		}
	}
	manifest, err := e.manifest()
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(dir, "manifest.json"), func(w io.Writer) error {
		_, err := w.Write(manifest)
		return err
	})
}

// one checks that the export holds one table, which WriteTo and WriteFile
// write.
func (e *Export) one() error {
	if len(e.Tables) == 1 {
		return nil
	}
	what := e.opts.What
	if len(what) == 0 {
		what = []string{"ledger"}
	}
	return fmt.Errorf("-what %s needs -dir", strings.Join(what, ","))
}

// notTheStore refuses an output at path that is one of the store's files,
// or would be taken for one (IsCompanion).
func (e *Export) notTheStore(path string) error {
	if IsCompanion(path, e.store) {
		return fmt.Errorf("%s would replace the store itself (or one of its files); choose another name", path)
	}
	return nil
}

// manifest returns manifest.json's contents.
func (e *Export) manifest() ([]byte, error) {
	type table struct {
		What string `json:"what"`
		View string `json:"view"`
		File string `json:"file"`
		Rows int    `json:"rows"`
	}
	type filters struct {
		Account      *string      `json:"account"`
		From         *nytime.Date `json:"from"`
		To           *nytime.Date `json:"to"`
		All          bool         `json:"all"`
		Raw          bool         `json:"raw"`
		ShowAccounts bool         `json:"show_accounts"`
	}
	m := struct {
		ExportedAt string            `json:"exported_at"`
		Schema     int               `json:"schema"`
		Rules      int               `json:"rules"`
		Calendar   int               `json:"calendar"`
		Format     string            `json:"format"`
		Filters    filters           `json:"filters"`
		Tables     []table           `json:"tables"`
		Units      map[string]string `json:"units"`
		Coverage   []ExportCoverage  `json:"coverage"`
	}{
		ExportedAt: exportInstant(e.At),
		Schema:     e.versions.schema, Rules: e.versions.derive, Calendar: e.versions.calendar,
		Format:   e.Format,
		Filters:  filters{All: e.opts.All, Raw: e.opts.Raw, ShowAccounts: e.opts.ShowAccounts},
		Units:    exportUnits,
		Tables:   []table{},
		Coverage: e.Coverage,
	}
	if e.account != "" {
		m.Filters.Account = &e.account
	}
	if e.opts.From != "" {
		m.Filters.From = &e.opts.From
	}
	if e.opts.To != "" {
		m.Filters.To = &e.opts.To
	}
	for _, t := range e.Tables {
		m.Tables = append(m.Tables, table{What: t.Name, View: t.View, File: t.File, Rows: t.Rows})
	}
	if m.Coverage == nil {
		m.Coverage = []ExportCoverage{}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	return append(b, '\n'), err
}

// exportUnits says, in manifest.json, how an export writes values.
var exportUnits = map[string]string{
	"money":      "dollars with two places, from the store's whole cents (the views' *_cents columns)",
	"quantities": "shares, contracts or units, from the store's whole millionths (the views' *_micros columns)",
	"times":      "UTC, RFC 3339, with milliseconds only when there are any",
	"dates":      "New York calendar dates, YYYY-MM-DD",
	"signs":      "Schwab's: cash into the account, and quantities into it, are positive",
}

// write writes the table to w in format: CSV (RFC 4180, a header row, LF
// line ends, no byte order mark), raw JSON as it is, or JSON Lines (an
// object per row, its keys the CSV header's, numbers as numbers, raw JSON
// as it is but for line breaks between its tokens).
func (t ExportedTable) write(w io.Writer, format string) error {
	if format == "jsonl" {
		return t.writeJSONLines(w)
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(t.Columns); err != nil {
		return err
	}
	record := make([]string, len(t.Columns))
	for _, row := range t.rows {
		for i, c := range row {
			record[i] = c.text
		}
		if err := cw.Write(record); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// writeJSONLines writes the table to w as JSON Lines.
func (t ExportedTable) writeJSONLines(w io.Writer) error {
	keys := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		keys[i] = jsonString(c) + ":"
	}
	var b bytes.Buffer
	for _, row := range t.rows {
		b.Reset()
		b.WriteByte('{')
		for i, c := range row {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(keys[i])
			switch c.kind {
			case cellNull:
				b.WriteString("null")
			case cellNumber:
				b.WriteString(c.text)
			case cellJSON:
				b.WriteString(oneLine(c.text))
			case cellString:
				b.WriteString(jsonString(c.text))
			}
		}
		b.WriteString("}\n")
		if _, err := w.Write(b.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// jsonString returns s as a JSON string, with <, > and & as they are.
func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	return strings.TrimSuffix(b.String(), "\n")
}

// A countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writePrivate makes the file at path, readable only by you, with what
// write writes: to a temporary file in path's directory, which is synced
// and then renamed over path, so that a write that fails midway leaves
// neither a half-written file nor a change to path. A rename within a
// directory replaces path at once on Linux, macOS and Windows.
func writePrivate(path string, write func(io.Writer) error) (err error) {
	// CreateTemp makes the file readable and writable only by its owner.
	f, err := os.CreateTemp(filepath.Dir(path), ".schwabtx-export-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(f.Name())
			err = fmt.Errorf("writing %s: %w", path, err)
		}
	}()
	bw := bufio.NewWriter(f)
	if err := write(bw); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// errTestBusy, which only tests make a read return (Store.testBusyReads),
// is taken for SQLITE_BUSY, so that a test sees a read run again.
var errTestBusy = errors.New("history: busy, as a test asked")
