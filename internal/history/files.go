package history

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// fileFlagCodes are the flags that a file's content gives by itself, which
// the store writes when it reads the file (layer 1) and the derivation of
// its account keeps.
var fileFlagCodes = []string{
	schwabfile.WarnEncodingFallback, schwabfile.WarnLazyQuotes, schwabfile.WarnUndatedRows, schwabfile.WarnRowsAfterTotal,
	schwabfile.WarnEmpty, schwabfile.WarnAtCap, schwabfile.WarnTwoYears, schwabfile.WarnLateStart, schwabfile.WarnTotalMismatch,
}

// fileHex returns the hex of a file's SHA-256, as subjects and refs write
// it.
func fileHex(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// fileKey returns the key of a symbol of a file exported on day:
// s:FILE:<symbol>@<day>, its white space one space. Files write every
// symbol as of their export, so the key is dated by it (10.1 of the
// design).
func fileKey(symbol string, day nytime.Date) string {
	s := fineText(symbol)
	if s == "" {
		return ""
	}
	return "s:FILE:" + s + "@" + string(day)
}

// itemKey returns the key of a History JSON row's ItemIssueId, or "" for a
// row without one ("0" is a row without a symbol).
func itemKey(id string) string {
	if id == "" || id == "0" {
		return ""
	}
	return "x:" + id
}

// deriveFile derives what a file of schwab.com gives by itself (layer 1):
// its fields, its rows, the flags of its warnings, and the keys and names
// of its symbols, dated by its export, else by its import at imported.
// The file's row in import_file is id.
func deriveFile(w *wtx, id int64, name string, f *schwabfile.File, imported time.Time) error {
	exportedAt := sql.NullString{}
	seen := imported
	if !f.ExportedAt.IsZero() {
		exportedAt = sql.NullString{String: instant(f.ExportedAt), Valid: true}
		seen = f.ExportedAt
	}
	span := f.Span()
	var statedTotal, rowsTotal sql.NullInt64
	if f.Kind == schwabfile.KindHistoryCSV || f.Kind == schwabfile.KindHistoryJSON {
		if f.StatedTotal != nil {
			c, ok := f.StatedTotal.Cents()
			statedTotal = sql.NullInt64{Int64: c, Valid: ok}
		}
		rowsTotal = sql.NullInt64{Valid: true}
		for _, r := range f.History {
			c, ok := r.Amount.Cents()
			rowsTotal = addCents(rowsTotal, sql.NullInt64{Int64: c, Valid: ok || !r.Amount.Present})
		}
	}
	rows := len(f.History) + len(f.Realized) + len(f.APIRows)
	if _, err := w.exec(`INSERT INTO file_fields (file_id, ref, account_id, forgotten, label, layout, encoding, exported_at,
		stated_from, stated_to, rows_from, rows_to, row_count, stated_total_cents, rows_total_cents)
		SELECT file_id, 'F' || lower(hex(sha256)), account_id, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? FROM import_file WHERE file_id = ?`,
		mask.FileName(filepath.Base(name)), f.Layout, nullString(f.Encoding), exportedAt,
		nullString(string(span.StatedFrom)), nullString(string(span.StatedTo)), nullString(string(span.Oldest)),
		nullString(string(span.Newest)), rows, statedTotal, rowsTotal, id); err != nil {
		return err
	}
	if err := writeFlagDefs(w); err != nil {
		return err
	}
	for _, wn := range f.Warnings {
		if _, err := w.exec("INSERT INTO flag (file_id, code, detail) VALUES (?, ?, ?)", id, wn.Code, nullString(wn.Detail)); err != nil {
			return err
		}
	}
	day := nytime.DateOf(seen)
	for _, r := range f.History {
		if err := readHistoryRow(w, id, r, day, seen); err != nil {
			return err
		}
	}
	for _, r := range f.Realized {
		if err := readLot(w, id, r, day, seen); err != nil {
			return err
		}
	}
	w.files = true
	return nil
}

// cellNotes returns the notes of a row's number cells that the store
// cannot keep: a placeholder in place of a number, a number off the grid
// (inexact), and a cell that is not a number (unreadable), each with its
// column and its text masked. A cell without a note is blank or kept.
func cellNotes(cells []namedCell) []rowProblem {
	var ps []rowProblem
	for _, c := range cells {
		d := c.value
		switch {
		case !d.Present:
		case d.Placeholder != "":
			ps = append(ps, rowProblem{Path: c.name, Flag: "placeholder", Value: d.Placeholder})
		case !d.Number():
			ps = append(ps, rowProblem{Path: c.name, Flag: "unreadable", Value: mask.Text(d.Text)})
		default:
			if _, ok := c.convert(d); !ok {
				ps = append(ps, rowProblem{Path: c.name, Flag: "inexact", Value: d.Text})
			}
		}
	}
	return ps
}

// A namedCell is a number cell of a row, with how the store keeps it.
type namedCell struct {
	name    string
	value   schwabfile.Decimal
	convert func(schwabfile.Decimal) (int64, bool)
}

func centsOfCell(d schwabfile.Decimal) (int64, bool)  { return d.Cents() }
func microsOfCell(d schwabfile.Decimal) (int64, bool) { return d.Micros() }
func floatOfCell(d schwabfile.Decimal) (int64, bool) {
	_, ok := d.Float()
	return 0, ok
}

// keep returns a number cell as a column, NULL when it is blank or the
// store cannot keep it.
func keep(d schwabfile.Decimal, convert func(schwabfile.Decimal) (int64, bool)) sql.NullInt64 {
	v, ok := convert(d)
	return sql.NullInt64{Int64: v, Valid: ok}
}

// readHistoryRow derives the history_row of a row of History file id,
// exported on day, and observes its keys and names as of seen. Its Action
// is kept through mask.Text, as its Description is: an action that the
// rules know holds no digits, so only one they do not, which the views
// show, could hold an account number. The coarse identity is of the
// Action as written.
func readHistoryRow(w *wtx, id int64, r schwabfile.HistoryRow, day nytime.Date, seen time.Time) error {
	notes := make([]rowProblem, 0, len(r.Problems))
	for _, p := range r.Problems {
		notes = append(notes, rowProblem{What: p})
	}
	notes = append(notes, cellNotes([]namedCell{
		{"Quantity", r.Quantity, microsOfCell},
		{"Price", r.Price, floatOfCell},
		{"Fees & Comm", r.Fees, centsOfCell},
		{"Amount", r.Amount, centsOfCell},
	})...)
	problems, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	quantity := keep(r.Quantity, microsOfCell)
	if quantity.Valid {
		quantity.Int64 = signedQuantity(r.Action, quantity.Int64)
	}
	var price sql.NullFloat64
	if f, ok := r.Price.Float(); ok {
		price = sql.NullFloat64{Float64: f, Valid: true}
	}
	coarse := coarseHistory(r.Posted, r.AsOf, r.Action, r.Fees, r.Amount)
	if _, err := w.exec(`INSERT INTO history_row (file_id, ordinal, line, raw, posted_date, asof_date, action, symbol,
		description, quantity_micros, quantity_text, price, price_text, fees_cents, amount_cents, item_issue_id,
		acctg_rule_cd, coarse, problems) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.Ordinal, r.Line, r.Raw, nullString(string(r.Posted)), nullString(string(r.AsOf)), mask.Text(r.Action), r.Symbol,
		mask.Text(r.Description), quantity, r.Quantity.Canon(), price, r.Price.Canon(), keep(r.Fees, centsOfCell),
		keep(r.Amount, centsOfCell), nullString(r.ItemIssueID), nullString(r.AcctgRuleCd), coarse[:], string(problems)); err != nil {
		return err
	}
	if k := itemKey(r.ItemIssueID); k != "" {
		if err := observe(w, seen, symbolObservation(k, r.Symbol),
			nameObservation{kind: "symbol", value: r.Symbol, seenIn: "history_file"},
			nameObservation{kind: "description", value: r.Description, seenIn: "history_file"}); err != nil {
			return err
		}
	}
	if k := fileKey(r.Symbol, day); k != "" {
		return observe(w, seen, symbolObservation(k, r.Symbol), nameObservation{kind: "symbol", value: fineText(r.Symbol), seenIn: "history_file"})
	}
	return nil
}

// symbolObservation returns what a file says of the key of a symbol: an
// option's contract, when the symbol names one.
func symbolObservation(key, symbol string) keyObservation {
	k := keyObservation{key: key}
	if o, ok := contract(symbol); ok {
		k.assetType, k.underlyingSymbol, k.expiration = "OPTION", o.Root, o.Expiration
		k.putCall, k.strike = o.PutCall, float64(o.StrikeThousandths)/1000
	}
	return k
}

// readLot derives the realized_row of a lot of Realized file id, exported
// on day, and observes its symbol's key as of seen.
func readLot(w *wtx, id int64, r schwabfile.RealizedRow, day nytime.Date, seen time.Time) error {
	notes := make([]rowProblem, 0, len(r.Problems))
	for _, p := range r.Problems {
		notes = append(notes, rowProblem{What: p})
	}
	money := []namedCell{
		{"Quantity", r.Quantity, microsOfCell},
		{"Proceeds", r.Proceeds, centsOfCell},
		{"Cost Basis", r.CostBasis, centsOfCell},
		{"Gain/Loss ($)", r.Gain, centsOfCell},
		{"Long Term Gain/Loss", r.LTGain, centsOfCell},
		{"Short Term Gain/Loss", r.STGain, centsOfCell},
		{"Unadjusted Cost Basis", r.UnadjustedCostBasis, centsOfCell},
		{"Disallowed Loss", r.DisallowedLoss, centsOfCell},
		{"Transaction Cost Basis", r.TxnCostBasis, centsOfCell},
		{"Total Transaction Gain/Loss ($)", r.TxnGain, centsOfCell},
		{"LT Transaction Gain/Loss ($)", r.LTTxnGain, centsOfCell},
		{"ST Transaction Gain/Loss ($)", r.STTxnGain, centsOfCell},
		{"Proceeds Per Share", r.ProceedsPerShare, floatOfCell},
		{"Cost Per Share", r.CostPerShare, floatOfCell},
	}
	notes = append(notes, cellNotes(money)...)
	if r.ColumnsDisagree {
		notes = append(notes, rowProblem{Flag: "columns_disagree"})
	}
	problems, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	float := func(d schwabfile.Decimal) sql.NullFloat64 {
		f, ok := d.Float()
		return sql.NullFloat64{Float64: f, Valid: ok}
	}
	var wash sql.NullBool
	if r.WashSale != nil {
		wash = sql.NullBool{Bool: *r.WashSale, Valid: true}
	}
	coarse := coarseLot(r.Closed, r.OpenedText, r.Quantity, r.Proceeds)
	washText := ""
	if r.WashSale != nil {
		washText = strconv.FormatBool(*r.WashSale)
	}
	fine := digest(append([]string{
		"l1", string(r.Closed), r.Symbol, r.Name, r.OpenedText, r.GainPct, r.Term,
		washText, string(r.TxnClosed), r.TxnGainPct, r.LTTxnGainPct, r.STTxnGainPct,
	}, cellTexts(money)...)...)
	if _, err := w.exec(`INSERT INTO realized_row (file_id, ordinal, line, raw, symbol, name, closed_date, opened_date,
		opened_text, quantity_micros, quantity_text, proceeds_per_share, cost_per_share, proceeds_cents, cost_basis_cents,
		gain_cents, gain_pct, lt_gain_cents, st_gain_cents, term, unadjusted_cost_cents, wash_sale, disallowed_loss_cents,
		txn_closed_date, txn_cost_basis_cents, txn_gain_cents, txn_gain_pct, lt_txn_gain_cents, lt_txn_gain_pct,
		st_txn_gain_cents, st_txn_gain_pct, coarse, fine, problems)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, r.Ordinal, r.Line, r.Raw, r.Symbol, free(r.Name), nullString(string(r.Closed)), nullString(string(r.Opened)),
		r.OpenedText, keep(r.Quantity, microsOfCell), r.Quantity.Canon(), float(r.ProceedsPerShare), float(r.CostPerShare),
		keep(r.Proceeds, centsOfCell), keep(r.CostBasis, centsOfCell), keep(r.Gain, centsOfCell), nullString(r.GainPct),
		keep(r.LTGain, centsOfCell), keep(r.STGain, centsOfCell), nullString(r.Term), keep(r.UnadjustedCostBasis, centsOfCell),
		wash, keep(r.DisallowedLoss, centsOfCell), nullString(string(r.TxnClosed)), keep(r.TxnCostBasis, centsOfCell),
		keep(r.TxnGain, centsOfCell), nullString(r.TxnGainPct), keep(r.LTTxnGain, centsOfCell), nullString(r.LTTxnGainPct),
		keep(r.STTxnGain, centsOfCell), nullString(r.STTxnGainPct), coarse[:], fine[:], string(problems)); err != nil {
		return err
	}
	if k := fileKey(r.Symbol, day); k != "" {
		return observe(w, seen, symbolObservation(k, r.Symbol), nameObservation{kind: "symbol", value: fineText(r.Symbol), seenIn: "realized_file"})
	}
	return nil
}

// cellTexts returns the cells' texts as written.
func cellTexts(cells []namedCell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = c.value.Text
	}
	return out
}

// deriveFileFields brings what every file's derived fields hold beyond its
// content up to date (layer 3): its ref, F and the shortest prefix of 8 hex
// digits or more of its SHA-256 that no other file's shares; the account
// it is for, your file_account decision's, else the one named at its
// import; and whether your forget decision set it aside. An account that
// gains or loses a file, or whose file was set aside or brought back, is
// derived again.
func deriveFileFields(w *wtx) error {
	type file struct {
		id                  int64
		sha                 string
		imported, effective sql.NullInt64
		forgotten           bool
		ref                 string
	}
	var files []file
	if err := queryEach(w, func(rows *sql.Rows) error {
		var f file
		err := rows.Scan(&f.id, &f.sha, &f.imported, &f.effective, &f.forgotten, &f.ref)
		files = append(files, f)
		return err
	}, `SELECT f.file_id, lower(hex(f.sha256)), f.account_id, d.account_id, d.forgotten, d.ref
		FROM import_file f JOIN file_fields d ON d.file_id = f.file_id ORDER BY f.file_id`); err != nil {
		return err
	}
	latest := map[[2]string]string{} // (kind, subject) -> value
	if err := queryEach(w, func(rows *sql.Rows) error {
		var kind, subject, value string
		err := rows.Scan(&kind, &subject, &value)
		latest[[2]string{kind, subject}] = value
		return err
	}, "SELECT kind, subject, value FROM decision WHERE kind IN ('file_account', 'forget') ORDER BY decision_id"); err != nil {
		return err
	}
	shas := make([]string, len(files))
	for i, f := range files {
		shas[i] = f.sha
	}
	for _, f := range files {
		account := f.imported
		if v, ok := strings.CutPrefix(latest[[2]string{"file_account", "f:" + f.sha}], "acct:"); ok && f.imported.Valid {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil {
				account = sql.NullInt64{Int64: id, Valid: true}
			}
		}
		forgotten := latest[[2]string{"forget", "f:" + f.sha}] == "yes"
		ref := "F" + hexRef(f.sha, shas)
		if account == f.effective && forgotten == f.forgotten && ref == f.ref {
			continue
		}
		if _, err := w.exec("UPDATE file_fields SET account_id = ?, forgotten = ?, ref = ? WHERE file_id = ?",
			account, forgotten, ref, f.id); err != nil {
			return err
		}
		for _, a := range []sql.NullInt64{f.effective, account} {
			if a.Valid {
				w.touch(a.Int64)
			}
		}
	}
	w.files = false
	return nil
}

// importExport imports a History or a Realized Gain/Loss export, read as
// f, whose bytes have the SHA-256 sha: for the account that o names, else
// the one whose number ends in the digits that the file gives; with the
// range you gave, which counts where the file states none; and your word
// on its completeness. It is an error when no one account fits, when the
// digits in its name are not its account's (unless o.Force), or when rows
// lie outside the range you gave for a file that states none.
func importExport(w *wtx, in FileInput, f *schwabfile.File, o ImportOptions) (int64, error) {
	shown := mask.FileName(filepath.Base(in.Name))
	acct, err := exportAccount(w, shown, f, o.Account)
	if err != nil {
		return 0, err
	}
	var number string
	if err := w.tx.QueryRowContext(w.ctx, "SELECT number FROM account WHERE account_id = ?", acct).Scan(&number); err != nil {
		return 0, err
	}
	if f.NameDigits != "" && !strings.HasSuffix(number, f.NameDigits) && !o.Force {
		label, err := accountLabel(w, acct)
		if err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("the digits in %s's name, %s, are not those of %s; use -force if the file was renamed", shown, f.NameDigits, label)
	}
	var from, to nytime.Date
	if f.Kind != schwabfile.KindRealized {
		from, to = o.From, o.To
		span := f.Span()
		if from != "" && span.StatedFrom == "" && span.Oldest != "" && span.Oldest < from {
			return 0, fmt.Errorf("%s has rows from %s, before -from %s", shown, span.Oldest, from)
		}
		if to != "" && span.StatedTo == "" && span.Newest != "" && span.Newest > to {
			return 0, fmt.Errorf("%s has rows to %s, after -to %s", shown, span.Newest, to)
		}
	}
	var id int64
	if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at,
		user_from, user_to) VALUES (?, ?, ?, ?, ?, ?, ?, ?) RETURNING file_id`,
		acct, string(f.Kind), filepath.Base(in.Name), in.Content, sha256Of(in.Content), instant(w.now),
		nullString(string(from)), nullString(string(to))).Scan(&id); err != nil {
		return 0, err
	}
	if err := deriveFile(w, id, in.Name, f, w.now); err != nil {
		return 0, err
	}
	if o.Completeness != "" && f.Kind != schwabfile.KindRealized {
		if _, err := w.exec(`INSERT INTO decision (made_at, kind, subject, value, note) VALUES (?, 'completeness', ?, ?, NULL)`,
			instant(w.now), "f:"+fileHex(in.Content), o.Completeness); err != nil {
			return 0, err
		}
	}
	w.touch(acct)
	return id, nil
}

// sha256Of returns the SHA-256 of b, as import_file keeps it.
func sha256Of(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// exportAccount returns the account that a History or Realized export is
// for: the one that sel names, when you named one; else the one whose
// number is the file's hint, a whole number of eight digits or more, which
// is added when the store does not know it; else the one account whose
// number ends in the hint's digits.
func exportAccount(w *wtx, shown string, f *schwabfile.File, sel string) (int64, error) {
	if sel != "" {
		return selectAccount(w, sel, true)
	}
	hint := f.AccountHint
	if hint == "" {
		return 0, fmt.Errorf("which account is %s for? Neither it nor its name gives one; use -account", shown)
	}
	if len(hint) >= 8 {
		return ensureAccount(w, hint, "file", w.now)
	}
	ids, err := accountsEnding(w, hint)
	if err != nil {
		return 0, err
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return 0, fmt.Errorf("%s is for an account ending in %s, which the store does not know; import its snapshots first, or use -account", shown, hint)
	}
	labels, err := accountLabels(w, ids)
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s is for an account ending in %s, which fits %s; say which with -account", shown, hint, joinAnd(labels))
}

// accountsEnding returns the accounts whose number ends in digits.
func accountsEnding(w *wtx, digits string) ([]int64, error) {
	var ids []int64
	err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var number string
		if err := rows.Scan(&id, &number); err != nil {
			return err
		}
		if strings.HasSuffix(number, digits) {
			ids = append(ids, id)
		}
		return nil
	}, "SELECT account_id, number FROM account ORDER BY account_id")
	return ids, err
}

// selectAccount returns the account that an account selector names (19.2
// of the design): A and its id; its number, whole; digits that end its
// number, dashes, spaces and a leading ... allowed; or its nickname. No
// account, or several, is an error that lists them masked. A whole number
// that the store does not know adds the account when add is set.
func selectAccount(w *wtx, sel string, add bool) (int64, error) {
	sel = strings.TrimSpace(sel)
	if id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimPrefix(sel, "A"), "a"), 10, 64); err == nil && (strings.HasPrefix(sel, "A") || strings.HasPrefix(sel, "a")) {
		if ok, err := rowExists(w, "SELECT 1 FROM account WHERE account_id = ?", id); err != nil || !ok {
			return 0, cmp.Or(err, fmt.Errorf("no account A%d in the store", id))
		}
		return id, nil
	}
	var ids []int64
	if digits, ok := SelectorDigits(sel); ok {
		var err error
		if ids, err = accountsEnding(w, digits); err != nil {
			return 0, err
		}
		if len(ids) == 0 && add && len(digits) >= 8 {
			return ensureAccount(w, digits, "file", w.now)
		}
	} else {
		err := queryEach(w, func(rows *sql.Rows) error {
			var id int64
			var nickname sql.NullString
			if err := rows.Scan(&id, &nickname); err != nil {
				return err
			}
			if SameNickname(nickname.String, sel) {
				ids = append(ids, id)
			}
			return nil
		}, "SELECT account_id, nickname FROM account ORDER BY account_id")
		if err != nil {
			return 0, err
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return 0, fmt.Errorf("no account in the store matches -account %q", mask.Text(sel))
	}
	labels, err := accountLabels(w, ids)
	if err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("-account %q fits %s; say which with its ref, such as A%d", mask.Text(sel), joinAnd(labels), ids[0])
}

// accountLabel returns how messages name an account: its ref, its number
// masked and its nickname, "A2 ...456 Joint".
func accountLabel(w *wtx, id int64) (string, error) {
	var masked string
	var nickname sql.NullString
	if err := w.tx.QueryRowContext(w.ctx, "SELECT masked, nickname FROM account WHERE account_id = ?", id).Scan(&masked, &nickname); err != nil {
		return "", err
	}
	return strings.TrimSpace(fmt.Sprintf("A%d %s %s", id, masked, nickname.String)), nil
}

// accountLabels returns the labels of accounts ids.
func accountLabels(w *wtx, ids []int64) ([]string, error) {
	out := make([]string, len(ids))
	for i, id := range ids {
		var err error
		if out[i], err = accountLabel(w, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// An APIFileAccount says what an import of `schwabtx transactions -json`
// output made of the rows of one account.
type APIFileAccount struct {
	// Account is the account, as "A2 ...456 Joint".
	Account string
	// Rows counts the account's rows in the file: New those the store did
	// not hold, which it now holds as transactions; Known those it held,
	// which it leaves as they were, and Differ those of them whose cut form
	// is none of the transaction's revisions.
	Rows, New, Known, Differ int
}

// An apiFileReport is what importAPIJSON made of `-json` output: the
// file's row in import_file, what it made of each account's rows, and
// what the file's owner should know.
type apiFileReport struct {
	file     int64
	Accounts []APIFileAccount
	Warnings []string
}

// importAPIJSON imports `schwabtx transactions -json` output (9.8 of the
// design): its rows are Schwab's, each attributed to the account its
// accountNumber names, or all to the account that sel names. Each
// account's rows are one read of the endpoint file, never complete. A row
// that the account holds already changes nothing, since when it was read
// is not known; a new row becomes a transaction that no list read has
// returned, absent from every earlier complete read of the account whose
// window holds it.
func importAPIJSON(w *wtx, in FileInput, f *schwabfile.File, sel string) (apiFileReport, error) {
	var r apiFileReport
	var id int64
	if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO import_file (account_id, kind, name, content, sha256, imported_at)
		VALUES (NULL, 'api_json', ?, ?, ?, ?) RETURNING file_id`,
		filepath.Base(in.Name), in.Content, sha256Of(in.Content), instant(w.now)).Scan(&id); err != nil {
		return r, err
	}
	r.file = id
	if err := deriveFile(w, id, in.Name, f, w.now); err != nil {
		return r, err
	}
	var all int64
	if sel != "" {
		var err error
		if all, err = selectAccount(w, sel, true); err != nil {
			return r, err
		}
	}
	type group struct {
		number string
		rows   []schwab.Transaction
	}
	var groups []*group
	byNumber := map[string]*group{}
	nameless := 0
	for _, raw := range f.APIRows {
		var t schwab.Transaction
		_ = json.Unmarshal(raw, &t) // lenient: what it cannot read is in its problems
		number := strings.TrimSpace(t.AccountNumber)
		if all != 0 {
			number = ""
		}
		if number == "" && all == 0 {
			nameless++
			continue
		}
		g := byNumber[number]
		if g == nil {
			g = &group{number: number}
			byNumber[number] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, t)
	}
	differ := 0
	for _, g := range groups {
		acct := all
		number := g.number
		if acct == 0 {
			var err error
			if acct, err = ensureAccount(w, number, "file", w.now); err != nil {
				return r, err
			}
		} else if err := w.tx.QueryRowContext(w.ctx, "SELECT number FROM account WHERE account_id = ?", acct).Scan(&number); err != nil {
			return r, err
		}
		a, err := recordFileRows(w, acct, number, id, g.rows)
		if err != nil {
			return r, err
		}
		if a.Account, err = accountLabel(w, acct); err != nil {
			return r, err
		}
		differ += a.Differ
		r.Accounts = append(r.Accounts, a)
	}
	if nameless > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%d %s no account; give -account to attribute them", nameless, plural(nameless, "row names", "rows name")))
	}
	if differ > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%d %s from what the store holds; kept the store's", differ, plural(differ, "row differs", "rows differ")))
	}
	return r, nil
}

// recordFileRows records the rows of a `-json` file, id, that are account
// acct's, whose number is number, as one read of the endpoint file.
func recordFileRows(w *wtx, acct int64, number string, file int64, rows []schwab.Transaction) (APIFileAccount, error) {
	a := APIFileAccount{Rows: len(rows)}
	in, err := prepareRows(rows)
	if err != nil {
		return a, fmt.Errorf("importing a -json file: %w", err)
	}
	rr := readRow{endpoint: "file", account: sql.NullInt64{Int64: acct, Valid: true}, file: sql.NullInt64{Int64: file, Valid: true}, rows: len(rows)}
	rr.duplicates = duplicates(in)
	var oldest, newest nytime.Date
	for _, t := range rows {
		if d := t.TradeDay(); d != "" {
			if oldest == "" || d < oldest {
				oldest = d
			}
			newest = max(newest, d)
		}
	}
	if oldest != "" {
		rr.start = sql.NullString{String: instant(midnight(oldest)), Valid: true}
		rr.end = sql.NullString{String: instant(midnight(newest)), Valid: true}
	}
	readID, err := insertRead(w, Read{SentAt: w.now, AnsweredAt: w.now}, rr)
	if err != nil {
		return a, err
	}
	w.touch(acct)
	taken := map[int64]bool{}
	for _, p := range in {
		cur, err := findTxn(w, acct, p, taken)
		if err != nil {
			return a, err
		}
		if cur != nil {
			taken[cur.id] = true
			a.Known++
			same, err := rowExists(w, `SELECT 1 FROM api_revision v JOIN revision_cut c ON c.revision_id = v.revision_id
				WHERE v.txn_id = ? AND c.digest = ?`, cur.id, p.digest[:])
			if err != nil {
				return a, err
			}
			if !same {
				a.Differ++
			}
			continue
		}
		res, err := recordRow(w, acct, number, readID, false, w.now, p, taken)
		if err != nil {
			return a, err
		}
		a.New++
		if err := absentBefore(w, acct, readID, res.txn, p.row); err != nil {
			return a, err
		}
	}
	return a, nil
}

// absentBefore records that the account's complete reads before the read
// readID, of a `-json` file, did not return transaction txn, t, which the
// file brought to the store: each whose window, a day wider on each side
// as for any absence, held its time or its trade date. That is true,
// since no API read returned it; whether it came late or was cancelled,
// the store cannot tell.
func absentBefore(w *wtx, acct, readID, txn int64, t schwab.Transaction) error {
	var reads []int64
	if err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var start, end, sent string
		if err := rows.Scan(&id, &start, &end, &sent); err != nil {
			return err
		}
		from, err := parseInstant(start)
		if err != nil {
			return err
		}
		e, err := parseInstant(end)
		if err != nil {
			return err
		}
		at, err := parseInstant(sent)
		if err != nil {
			return err
		}
		lo, hi := from.Add(-24*time.Hour), minTime(e, at).Add(24*time.Hour)
		near := func(x time.Time) bool { return !x.IsZero() && !x.Before(lo) && !x.After(hi) }
		if near(t.Time) || near(t.TradeDate) {
			reads = append(reads, id)
		}
		return nil
	}, "SELECT read_id, window_start, window_end, sent_at FROM api_read WHERE account_id = ? AND complete = 1 AND read_id < ? ORDER BY read_id",
		acct, readID); err != nil {
		return err
	}
	for _, id := range reads {
		if _, err := w.exec("INSERT INTO api_absent (read_id, txn_id) VALUES (?, ?)", id, txn); err != nil {
			return err
		}
	}
	return nil
}

// errDryRun ends a dry run's write, so that it saves nothing.
var errDryRun = errors.New("a dry run saves nothing")

// A ForgetReport says what setting a file aside, or bringing it back, did.
type ForgetReport struct {
	// Ref is the file's ref, and Name its name with its free text masked.
	Ref, Name string
	// Undo says that the file was brought back.
	Undo bool
	// Lots says that the file is a Realized export, whose rows are lots.
	Lots bool
	// Left counts the transactions or lots that the file alone holds,
	// which were set aside (or, with Undo, came back); Counted those of
	// its transactions that counted in the ledger (or, with Undo, count
	// again), not those linked to an API transaction, which counts in their
	// place; Stayed those that other files hold too, which stay, and
	// StayIn those files' refs.
	Left, Counted, Stayed int
	StayIn                []string
	// Waiting counts your decisions that applied to the transactions that
	// the file alone holds, whatever row of them each names: they wait,
	// applied to nothing, until a file that holds the transaction comes.
	Waiting int
}

// Forget sets a History or Realized export aside, or with undo brings it
// back: a forget decision, which leaves the file's bytes in the store. A
// file set aside takes no part in the derivation, so the transactions and
// lots that it alone held leave the ledger; those that other files hold
// stay, and your decisions on them follow them (13.7 of the design).
// fileRef is the file's ref, or the start of its SHA-256 in hex, which
// must name one file. `-json` output cannot be set aside: the API's rows
// it brought are transactions, which the store keeps.
func (s *Store) Forget(ctx context.Context, fileRef string, undo bool) (ForgetReport, error) {
	var r ForgetReport
	err := s.withWrite(ctx, "setting a file aside", func(w *wtx) error {
		r = ForgetReport{Undo: undo}
		id, sha, kind, ref, err := findFile(w, fileRef)
		if err != nil {
			return err
		}
		if kind == "api_json" {
			return fmt.Errorf("%s brought transactions from Schwab's API, which the store keeps; there is nothing to set aside", ref)
		}
		r.Lots = kind == "realized_csv"
		if !undo {
			if err := forgetCounts(w, id, &r); err != nil {
				return err
			}
			if r.Waiting, err = forgetWaiting(w, id); err != nil {
				return err
			}
		}
		value := "yes"
		if undo {
			value = "no"
		}
		if _, err := w.exec("INSERT INTO decision (made_at, kind, subject, value) VALUES (?, 'forget', ?, ?)",
			instant(w.now), "f:"+sha, value); err != nil {
			return err
		}
		w.files = true
		if err := w.derive(); err != nil {
			return err
		}
		if undo {
			if err := forgetCounts(w, id, &r); err != nil {
				return err
			}
		}
		return w.tx.QueryRowContext(w.ctx, `SELECT d.ref, d.label FROM file_fields d WHERE d.file_id = ?`, id).Scan(&r.Ref, &r.Name)
	})
	return r, err
}

// findFile returns the file that given names, F and the start of its
// SHA-256 in hex, or that start alone, and the file's ref. A start of a
// digest that begins with f is also F and the rest: both are looked for.
func findFile(w *wtx, given string) (id int64, sha, kind, ref string, err error) {
	lower := strings.ToLower(given)
	isHex := func(p string) bool { return p != "" && strings.Trim(p, "0123456789abcdef") == "" }
	var prefixes []string
	if p, ok := strings.CutPrefix(lower, "f"); ok && isHex(p) {
		prefixes = append(prefixes, p)
	}
	if isHex(lower) {
		prefixes = append(prefixes, lower)
	}
	if len(prefixes) == 0 {
		return 0, "", "", "", fmt.Errorf("%q is not a file's ref, such as F3a9e21b0", mask.Text(given))
	}
	prefixes = append(prefixes, prefixes[0]) // the query takes two
	var refs []string
	err = queryEach(w, func(rows *sql.Rows) error {
		err := rows.Scan(&id, &sha, &kind, &ref)
		refs = append(refs, ref)
		return err
	}, `SELECT f.file_id, lower(hex(f.sha256)), f.kind, d.ref FROM import_file f JOIN file_fields d ON d.file_id = f.file_id
		WHERE substr(lower(hex(f.sha256)), 1, ?) = ? OR substr(lower(hex(f.sha256)), 1, ?) = ? ORDER BY d.ref`,
		len(prefixes[0]), prefixes[0], len(prefixes[1]), prefixes[1])
	switch {
	case err != nil:
		return 0, "", "", "", err
	case len(refs) == 0:
		return 0, "", "", "", fmt.Errorf("no file F%s in the store", prefixes[0])
	case len(refs) > 1:
		return 0, "", "", "", fmt.Errorf("the ref F%s fits several files, %s; give more of it", prefixes[0], joinAnd(refs))
	}
	return id, sha, kind, ref, nil
}

// forgetCounts counts, into r, the transactions and lots that file id
// holds now: those it alone holds, and of those the transactions that
// count; those that other files hold too, and those files.
func forgetCounts(w *wtx, id int64, r *ForgetReport) error {
	err := queryEach(w, func(rows *sql.Rows) error {
		var others sql.NullString
		var counted bool
		if err := rows.Scan(&others, &counted); err != nil {
			return err
		}
		if !others.Valid {
			r.Left++
			if counted {
				r.Counted++
			}
			return nil
		}
		r.Stayed++
		for ref := range strings.SplitSeq(others.String, ",") {
			if !slices.Contains(r.StayIn, ref) {
				r.StayIn = append(r.StayIn, ref)
			}
		}
		return nil
	}, `WITH mine AS (SELECT m.csv_txn_id AS txn, 'c' AS kind FROM csv_member m JOIN history_row r ON r.row_id = m.row_id
			WHERE r.file_id = ?1
		UNION SELECT m.lot_id, 'l' FROM lot_member m JOIN realized_row r ON r.row_id = m.row_id WHERE r.file_id = ?1)
		SELECT (SELECT group_concat(DISTINCT d.ref) FROM (
			SELECT r.file_id FROM csv_member m JOIN history_row r ON r.row_id = m.row_id WHERE mine.kind = 'c' AND m.csv_txn_id = mine.txn
			UNION SELECT r.file_id FROM lot_member m JOIN realized_row r ON r.row_id = m.row_id WHERE mine.kind = 'l' AND m.lot_id = mine.txn
		) o JOIN file_fields d ON d.file_id = o.file_id WHERE o.file_id <> ?1),
		mine.kind = 'c' AND coalesce((SELECT e.counted FROM entry e WHERE e.csv_txn_id = mine.txn), 0) = 1
		FROM mine`, id)
	if err != nil {
		return err
	}
	slices.Sort(r.StayIn)
	return nil
}

// forgetWaiting counts your decisions that apply to the transactions that
// file id alone holds, before it is set aside: each kind's latest on the
// transaction, and its rejects, whatever row of it each names (one made on
// a row of a file set aside before reaches the transaction that holds the
// row now). Set aside, the file takes them with it: they wait.
func forgetWaiting(w *wtx, id int64) (int, error) {
	var acct sql.NullInt64
	if err := w.tx.QueryRowContext(w.ctx, "SELECT account_id FROM file_fields WHERE file_id = ?", id).Scan(&acct); err != nil || !acct.Valid {
		return 0, err
	}
	g, err := loadGlobals(w)
	if err != nil {
		return 0, err
	}
	in, err := loadAccount(w, acct.Int64, g)
	if err != nil {
		return 0, err
	}
	fl := deriveFiles(in, nil, nil)
	n := 0
	for i, t := range fl.txns {
		if slices.ContainsFunc(t.members, func(r *historyRowInput) bool { return r.file.id != id }) {
			continue
		}
		for kind := range fl.decided[i] {
			if kind != "reject" {
				n++
			}
		}
		n += len(rejectedBy(fl.all[i]))
	}
	return n, nil
}
