package history

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A Read is one request to Schwab and what came of it. The sync makes the
// request first and records the answer after, so that no transaction of the
// store stays open across a request.
type Read struct {
	// SentAt is when the request was sent, and AnsweredAt when its answer,
	// or its failure, came.
	SentAt, AnsweredAt time.Time
	// Err is nil when Schwab answered. A *schwab.Error with a 4xx status
	// means Schwab refused the request; any other error that it failed.
	Err error
}

// A TransactionsRead is a read of an account's transactions, from GET
// /accounts/{hash}/transactions.
type TransactionsRead struct {
	Read
	// Account is the number of the account whose hash was in the path.
	Account string
	// Query is the query as it was sent: its window, its types, which when
	// empty mean every type, and its symbol.
	Query schwab.TransactionQuery
	// Rows are the answer's transactions, in Schwab's order, each with its
	// Raw JSON.
	Rows []schwab.Transaction
}

// A TransactionRead is a read of one of an account's transactions by its
// activity ID, from GET /accounts/{hash}/transactions/{id}.
type TransactionRead struct {
	Read
	// Account is the number of the account whose hash was in the path.
	Account string
	// ActivityID is the ID that was asked for.
	ActivityID int64
	// Row is the answer, with its Raw JSON.
	Row schwab.Transaction
}

// Recorded says what the store made of a read of transactions.
type Recorded struct {
	// ReadID is the read's id in the store.
	ReadID int64
	// New counts the rows that the store did not hold, Same those it held
	// as they were, and Revised those that had changed since it last read
	// them, as their cut forms tell (CutForm).
	New, Same, Revised int
	// Back counts the rows that a complete read had shown gone and that
	// this read returned; Gone the transactions that this read, being
	// complete, shows gone.
	Back, Gone int
	// Flagged counts the account's entries that this read left with a flag
	// that needs you (of severity attention) that they did not have.
	Flagged int
}

// RecordTransactions records a read of an account's transactions: the read
// itself, whatever its outcome, and, when Schwab answered, each row as a
// new transaction, a repeat of one the store holds or a revision of it.
// When the read is complete (every type, no symbol, fewer rows than
// schwab.MaxTransactions), it also records which of the account's
// transactions near its window it did not return, which is what can show
// them gone. The account's derived data is brought up to date in the same
// transaction.
func (s *Store) RecordTransactions(ctx context.Context, r TransactionsRead) (Recorded, error) {
	if err := checkRead(r.Read); err != nil {
		return Recorded{}, err
	}
	if err := checkNumber(r.Account); err != nil {
		return Recorded{}, err
	}
	start, end := instant(r.Query.Start), instant(r.Query.End)
	if r.Query.Start.IsZero() || r.Query.End.IsZero() || end <= start {
		return Recorded{}, fmt.Errorf("recording a read of transactions: its window, %s to %s, does not end after it starts", start, end)
	}
	rr := readRow{
		endpoint: "transactions",
		start:    sql.NullString{String: start, Valid: true},
		end:      sql.NullString{String: end, Valid: true},
		symbol:   nullString(r.Query.Symbol),
	}
	rr.types, rr.allTypes = queryTypes(r.Query.Types)
	var rows []schwab.Transaction
	answered := r.Err == nil
	if answered {
		rows = r.Rows
		rr.rows = len(rows)
		rr.capped = len(rows) >= s.maxRows()
		rr.complete = rr.allTypes && r.Query.Symbol == "" && !rr.capped
	}
	in, err := prepareRows(rows)
	if err != nil {
		return Recorded{}, fmt.Errorf("recording a read of transactions: %w", err)
	}
	rr.duplicates = duplicates(in)
	op := fmt.Sprintf("recording a read of %d transactions", len(rows))
	var rec Recorded
	err = s.withWrite(ctx, op, func(w *wtx) error {
		rec = Recorded{}
		acct, err := ensureAccount(w, r.Account, "api", r.AnsweredAt)
		if err != nil {
			return err
		}
		rr.account = sql.NullInt64{Int64: acct, Valid: true}
		if rec.ReadID, err = insertRead(w, r.Read, rr); err != nil || !answered {
			return err // a refused or failed read is recorded, and that is all
		}
		w.touch(acct)
		before, err := attentionFlags(w, acct)
		if err != nil {
			return err
		}
		taken := map[int64]bool{} // transactions that a row of this read is
		for _, row := range in {
			res, err := recordRow(w, acct, r.Account, rec.ReadID, true, r.AnsweredAt, row, taken)
			if err != nil {
				return err
			}
			rec.count(res)
		}
		if rr.complete {
			if err := recordAbsences(w, acct, rec.ReadID, r.Query.Start, minTime(r.Query.End, r.SentAt), taken); err != nil {
				return err
			}
		}
		if err := w.derive(); err != nil {
			return err
		}
		if rec.Flagged, err = newlyFlagged(w, acct, before); err != nil {
			return err
		}
		return w.tx.QueryRowContext(w.ctx, "SELECT count(*) FROM txn WHERE account_id = ? AND gone_read_id = ?",
			acct, rec.ReadID).Scan(&rec.Gone)
	})
	return rec, err
}

// RecordTransaction records a read of one transaction by its activity ID:
// the read, and when Schwab answered, its row, as RecordTransactions does.
// Such a read moves the transaction's latest raw JSON and may add a
// revision, but it never brings back a transaction that a complete read
// showed gone: whether the endpoint serves rows that the list no longer
// returns is not known. A refusal, such as a 404, is recorded as it is.
func (s *Store) RecordTransaction(ctx context.Context, r TransactionRead) (Recorded, error) {
	if err := checkRead(r.Read); err != nil {
		return Recorded{}, err
	}
	if err := checkNumber(r.Account); err != nil {
		return Recorded{}, err
	}
	if r.ActivityID <= 0 {
		return Recorded{}, fmt.Errorf("recording a read of a transaction: activity ID %d is not one", r.ActivityID)
	}
	rr := readRow{endpoint: "transaction", activityID: sql.NullInt64{Int64: r.ActivityID, Valid: true}}
	var in []preparedRow
	if r.Err == nil {
		rr.rows = 1
		var err error
		if in, err = prepareRows([]schwab.Transaction{r.Row}); err != nil {
			return Recorded{}, fmt.Errorf("recording a read of a transaction: %w", err)
		}
	}
	var rec Recorded
	err := s.withWrite(ctx, "recording a read of a transaction", func(w *wtx) error {
		rec = Recorded{}
		acct, err := ensureAccount(w, r.Account, "api", r.AnsweredAt)
		if err != nil {
			return err
		}
		rr.account = sql.NullInt64{Int64: acct, Valid: true}
		if rec.ReadID, err = insertRead(w, r.Read, rr); err != nil {
			return err
		}
		if len(in) == 0 {
			return nil
		}
		w.touch(acct)
		before, err := attentionFlags(w, acct)
		if err != nil {
			return err
		}
		for _, row := range in {
			res, err := recordRow(w, acct, r.Account, rec.ReadID, false, r.AnsweredAt, row, map[int64]bool{})
			if err != nil {
				return err
			}
			rec.count(res)
		}
		if err := w.derive(); err != nil {
			return err
		}
		rec.Flagged, err = newlyFlagged(w, acct, before)
		return err
	})
	return rec, err
}

// attentionFlags returns the codes of the flags of severity attention on
// account acct's entries, by ref.
func attentionFlags(w *wtx, acct int64) (map[string][]string, error) {
	flags := map[string][]string{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var ref, code string
		err := rows.Scan(&ref, &code)
		flags[ref] = append(flags[ref], code)
		return err
	}, `SELECT e.ref, f.code FROM flag f JOIN entry e ON e.entry_id = f.entry_id JOIN flag_def d ON d.code = f.code
		WHERE e.account_id = ? AND d.severity = 'attention'`, acct)
	return flags, err
}

// newlyFlagged counts account acct's entries that have a flag of
// severity attention now that before, attentionFlags' answer of earlier,
// does not give them.
func newlyFlagged(w *wtx, acct int64, before map[string][]string) (int, error) {
	after, err := attentionFlags(w, acct)
	n := 0
	for ref, codes := range after {
		if slices.ContainsFunc(codes, func(c string) bool { return !slices.Contains(before[ref], c) }) {
			n++
		}
	}
	return n, err
}

// maxRows returns how many rows make an answer capped:
// schwab.MaxTransactions, unless a test has set fewer, so that it can cap a
// read without sending thousands of rows.
func (s *Store) maxRows() int {
	if s.o.maxRows > 0 {
		return s.o.maxRows
	}
	return schwab.MaxTransactions
}

// checkRead refuses a read without its times, or answered before it was
// sent.
func checkRead(r Read) error {
	switch {
	case r.SentAt.IsZero() || r.AnsweredAt.IsZero():
		return errors.New("recording a read: it needs the times it was sent and answered")
	case instant(r.AnsweredAt) < instant(r.SentAt):
		return fmt.Errorf("recording a read: it was answered at %s, before it was sent at %s", instant(r.AnsweredAt), instant(r.SentAt))
	}
	return nil
}

// checkNumber refuses an account number that the store could not key an
// account by.
func checkNumber(number string) error {
	if number == "" || number != strings.TrimSpace(number) {
		return fmt.Errorf("recording a read: %q is not an account number", mask.Text(number))
	}
	return nil
}

// queryTypes returns the types a query sent, as the client sends them, and
// whether they are every type: an empty list asks for every type, as does
// one that holds each of schwab.AllTransactionTypes.
func queryTypes(types []schwab.TransactionType) (sql.NullString, bool) {
	all := len(types) == 0
	if all {
		types = schwab.AllTransactionTypes()
	} else {
		all = true
		for _, t := range schwab.AllTransactionTypes() {
			all = all && slices.Contains(types, t)
		}
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = string(t)
	}
	return sql.NullString{String: strings.Join(names, ","), Valid: true}, all
}

// A readRow is what api_read records of a read beyond its Read.
type readRow struct {
	endpoint   string
	account    sql.NullInt64
	start, end sql.NullString
	types      sql.NullString
	allTypes   bool
	symbol     sql.NullString
	activityID sql.NullInt64
	rows       int
	duplicates []int64 // activity IDs that came earlier in the answer
	capped     bool
	complete   bool
}

// insertRead records a read and returns its id. Its outcome comes from
// r.Err: answered, refused (a 4xx from Schwab) or failed, with Schwab's
// status and correlation ID, and the error's text masked.
func insertRead(w *wtx, r Read, rr readRow) (int64, error) {
	outcome := "answered"
	var status sql.NullInt64
	var correl, text sql.NullString
	if r.Err != nil {
		outcome = "failed"
		if e, ok := errors.AsType[*schwab.Error](r.Err); ok {
			status = sql.NullInt64{Int64: int64(e.Status), Valid: true}
			correl = nullString(e.CorrelID)
			if e.Status >= 400 && e.Status < 500 {
				outcome = "refused"
			}
		}
		text = free(r.Err.Error())
	}
	dups, err := json.Marshal(append([]int64{}, rr.duplicates...))
	if err != nil {
		return 0, err
	}
	var id int64
	err = w.tx.QueryRowContext(w.ctx, `INSERT INTO api_read (endpoint, account_id, window_start, window_end, types,
		all_types, symbol, activity_id, sent_at, answered_at, outcome, http_status, correlation_id, error,
		row_count, duplicates, capped, complete)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING read_id`,
		rr.endpoint, rr.account, rr.start, rr.end, rr.types, rr.allTypes, rr.symbol, rr.activityID,
		instant(r.SentAt), instant(r.AnsweredAt), outcome, status, correl, text,
		rr.rows, string(dups), rr.capped, rr.complete).Scan(&id)
	return id, err
}

// A preparedRow is a row of an answer with what the store keys it by,
// worked out before the write: its activity ID and where that came from,
// and its cut digest.
type preparedRow struct {
	row      schwab.Transaction
	id       int64  // 0: none
	idSource string // number, string or none
	digest   [32]byte
}

// prepareRows works out each row's activity ID and cut digest. A row
// without its Raw JSON is refused: the store keeps what Schwab sent.
func prepareRows(rows []schwab.Transaction) ([]preparedRow, error) {
	out := make([]preparedRow, len(rows))
	for i, row := range rows {
		if len(bytes.TrimSpace(row.Raw)) == 0 {
			return nil, fmt.Errorf("row %d has no raw JSON", i+1)
		}
		d, err := cutDigest(row.Raw)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i+1, err)
		}
		id, source := activityID(row)
		out[i] = preparedRow{row: row, id: id, idSource: source, digest: d}
	}
	return out, nil
}

// idText is an activity ID that Schwab sent as a string of digits.
var idText = regexp.MustCompile(`^[0-9]{1,19}$`)

// activityID returns a row's activity ID and where it came from: the
// record's number; else, when its raw JSON holds activityId as a string of
// 1 to 19 digits that names a positive int64, that; else none.
func activityID(row schwab.Transaction) (int64, string) {
	if row.ActivityID > 0 {
		return row.ActivityID, "number"
	}
	var w struct {
		ActivityID json.RawMessage `json:"activityId"`
	}
	var s string
	if json.Unmarshal(row.Raw, &w) == nil && json.Unmarshal(w.ActivityID, &s) == nil && idText.MatchString(s) {
		if id, err := strconv.ParseInt(s, 10, 64); err == nil && id > 0 {
			return id, "string"
		}
	}
	return 0, "none"
}

// duplicates returns the activity IDs that come again in rows after their
// first, once each, in the order they come again.
func duplicates(rows []preparedRow) []int64 {
	seen := map[int64]bool{}
	var dups []int64
	for _, r := range rows {
		switch {
		case r.id == 0:
		case seen[r.id] && !slices.Contains(dups, r.id):
			dups = append(dups, r.id)
		default:
			seen[r.id] = true
		}
	}
	return dups
}

// A rowResult says what a row was to the store.
type rowResult struct {
	kind int // rowNew, rowSame or rowRevised
	back bool
}

const (
	rowNew = iota
	rowSame
	rowRevised
)

// count adds a row's result to the counts.
func (r *Recorded) count(res rowResult) {
	switch res.kind {
	case rowNew:
		r.New++
	case rowSame:
		r.Same++
	case rowRevised:
		r.Revised++
	}
	if res.back {
		r.Back++
	}
}

// recordRow records one row of a read of account acct, whose number is
// number: a new transaction, or a repeat or revision of the one the store
// holds, whose latest raw JSON it becomes. A list read (list) also moves
// the transaction's latest list read, which is what ends a "gone". taken
// holds the transactions that earlier rows of the read are, so that two
// identical rows without an activity ID are two transactions; recordRow
// adds this row's.
func recordRow(w *wtx, acct int64, number string, readID int64, list bool, seen time.Time, p preparedRow, taken map[int64]bool) (rowResult, error) {
	cur, err := findTxn(w, acct, p, taken)
	if err != nil {
		return rowResult{}, err
	}
	raw, at := string(p.row.Raw), instant(seen)
	listRead := sql.NullInt64{Int64: readID, Valid: list}
	if cur == nil {
		var id int64
		err := w.tx.QueryRowContext(w.ctx, `INSERT INTO api_txn (account_id, activity_id, id_source, first_raw,
			first_read_id, first_seen_at, latest_raw, latest_read_id, latest_seen_at, latest_list_read_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING txn_id`,
			acct, sql.NullInt64{Int64: p.id, Valid: p.id != 0}, p.idSource, raw, readID, at, raw, readID, at, listRead).Scan(&id)
		if err != nil {
			return rowResult{}, err
		}
		taken[id] = true
		if err := insertRevision(w, id, 1, raw, readID, at, p.digest, nil); err != nil {
			return rowResult{}, err
		}
		if err := deriveTxn(w, id, acct, number, p.row); err != nil {
			return rowResult{}, err
		}
		return rowResult{kind: rowNew}, observeTxn(w, p.row, seen)
	}
	taken[cur.id] = true
	res := rowResult{kind: rowSame, back: list && cur.gone}
	if cur.digest == p.digest {
		if _, err := w.exec(`UPDATE api_revision SET last_read_id = ?, last_seen_at = max(?, first_seen_at)
			WHERE revision_id = ?`, readID, at, cur.revision); err != nil {
			return rowResult{}, err
		}
	} else {
		res.kind = rowRevised
		changed, err := changedPaths([]byte(cur.revisionRaw), p.row.Raw)
		if err != nil {
			return rowResult{}, err
		}
		if err := insertRevision(w, cur.id, cur.seq+1, raw, readID, at, p.digest, changed); err != nil {
			return rowResult{}, err
		}
	}
	if _, err := w.exec(`UPDATE api_txn SET latest_raw = ?, latest_read_id = ?, latest_seen_at = max(?, first_seen_at),
		latest_list_read_id = coalesce(?, latest_list_read_id) WHERE txn_id = ?`, raw, readID, at, listRead, cur.id); err != nil {
		return rowResult{}, err
	}
	if cur.latestRaw != raw {
		if err := deriveTxn(w, cur.id, acct, number, p.row); err != nil {
			return rowResult{}, err
		}
	}
	return res, observeTxn(w, p.row, seen)
}

// A storedTxn is what recordRow needs of a transaction the store holds.
type storedTxn struct {
	id          int64
	latestRaw   string
	gone        bool // a complete read shows it gone
	revision    int64
	seq         int
	revisionRaw string // the first raw JSON of its current run
	digest      [32]byte
}

// findTxn returns the transaction that a row of a read of account acct
// is, or nil for a new one. A row with an activity ID is the account's
// transaction of that ID. A row without one is the oldest of the
// account's transactions without an ID, at the same time and with the
// same cut form now, that no earlier row of the read is (taken): such
// rows are never keyed by their bytes, which change from day to day with
// their instruments' closing prices.
func findTxn(w *wtx, acct int64, p preparedRow, taken map[int64]bool) (*storedTxn, error) {
	const cols = `SELECT t.txn_id, t.latest_raw, x.gone_read_id IS NOT NULL, v.revision_id, v.seq, v.raw, c.digest
		FROM api_txn t JOIN txn x ON x.txn_id = t.txn_id
		JOIN api_revision v ON v.txn_id = t.txn_id AND v.seq = (SELECT max(seq) FROM api_revision WHERE txn_id = t.txn_id)
		JOIN revision_cut c ON c.revision_id = v.revision_id `
	var rows *sql.Rows
	var err error
	if p.id != 0 {
		rows, err = w.tx.QueryContext(w.ctx, cols+"WHERE t.account_id = ? AND t.activity_id = ?", acct, p.id)
	} else {
		if p.row.Time.IsZero() {
			return nil, nil // a row without a time is new each time it comes
		}
		rows, err = w.tx.QueryContext(w.ctx, cols+"WHERE t.account_id = ? AND t.activity_id IS NULL AND x.time = ? AND c.digest = ? ORDER BY t.txn_id",
			acct, instant(p.row.Time), p.digest[:])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t storedTxn
		var digest []byte
		if err := rows.Scan(&t.id, &t.latestRaw, &t.gone, &t.revision, &t.seq, &t.revisionRaw, &digest); err != nil {
			return nil, err
		}
		if taken[t.id] && p.id == 0 {
			continue
		}
		copy(t.digest[:], digest)
		return &t, nil
	}
	return nil, rows.Err()
}

// insertRevision records a run of a transaction's cut form: its first raw
// JSON, when it was first and last seen, its digest and the key paths that
// changed from the run before.
func insertRevision(w *wtx, txn int64, seq int, raw string, readID int64, at string, digest [32]byte, changed []string) error {
	var id int64
	if err := w.tx.QueryRowContext(w.ctx, `INSERT INTO api_revision (txn_id, seq, raw, first_read_id, first_seen_at,
		last_read_id, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING revision_id`,
		txn, seq, raw, readID, at, readID, at).Scan(&id); err != nil {
		return err
	}
	return insertCut(w, id, digest, changed)
}

// insertCut records a revision's cut digest and changed key paths.
func insertCut(w *wtx, revision int64, digest [32]byte, changed []string) error {
	var c sql.NullString
	if changed != nil {
		b, err := json.Marshal(changed)
		if err != nil {
			return err
		}
		c = sql.NullString{String: string(b), Valid: true}
	}
	_, err := w.exec("INSERT INTO revision_cut (revision_id, cut_version, digest, changed) VALUES (?, ?, ?, ?)",
		revision, CutVersion, digest[:], c)
	return err
}

// recordAbsences records that the complete read readID of account acct,
// whose window ran from start to end (its end capped at when it was sent),
// did not return the account's transactions whose time or trade date lies
// within a day of the window: every one of them that is not in returned.
// The day is wider than the margins of the gone rule (9.5 of the design),
// so that a later rule with other margins can be worked out from the same
// evidence.
func recordAbsences(w *wtx, acct, readID int64, start, end time.Time, returned map[int64]bool) error {
	lo, hi := instant(start.Add(-24*time.Hour)), instant(end.Add(24*time.Hour))
	rows, err := w.tx.QueryContext(w.ctx, `SELECT txn_id FROM txn WHERE account_id = ?
		AND (time BETWEEN ? AND ? OR trade_date BETWEEN ? AND ?) ORDER BY txn_id`, acct, lo, hi, lo, hi)
	if err != nil {
		return err
	}
	var absent []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !returned[id] {
			absent = append(absent, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range absent {
		if _, err := w.exec("INSERT INTO api_absent (read_id, txn_id) VALUES (?, ?)", readID, id); err != nil {
			return err
		}
	}
	return nil
}

// minTime returns the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// nullString returns s as a column, NULL when it is empty.
func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// A txnRow is a transaction's columns in txn that its latest raw JSON
// gives.
type txnRow struct {
	time, tradeDate, tradeDay     sql.NullString
	settlementDate, settlementDay sql.NullString
	typ, status, subAccount       sql.NullString
	description                   sql.NullString
	net                           sql.NullInt64
	orderID, positionID           sql.NullInt64
	activityType                  sql.NullString
	qualifiedDividend             sql.NullBool
	numberSent                    sql.NullString
	problems                      string
}

// A legRow is a transaction's leg in api_leg, but for its security, which
// the grouping of instruments gives.
type legRow struct {
	assetType      sql.NullString
	instrumentID   sql.NullInt64
	symbol         sql.NullString
	amountMicros   sql.NullInt64
	amountCents    sql.NullInt64
	costCents      sql.NullInt64
	price          sql.NullFloat64
	feeType        sql.NullString
	positionEffect sql.NullString
}

// A txnProblem is an entry of txn.problems: one of the record's problems,
// with its path and what is wrong, or a number that the store rounded from
// noise or could not keep, with its path, its leg, the flag it raises
// (rounded or inexact) and the number as Schwab sent it.
type txnProblem struct {
	Path  string `json:"path,omitempty"`
	What  string `json:"what,omitempty"`
	Leg   *int   `json:"leg,omitempty"`
	Flag  string `json:"flag,omitempty"`
	Value string `json:"value,omitempty"`
}

// txnFields returns the columns of t, a transaction of the account whose
// number is number, and its legs, as the record reads its raw JSON. The
// record's decoding never fails; what it could not read is in its
// problems.
func txnFields(t schwab.Transaction, number string) (txnRow, []legRow) {
	var ps []txnProblem
	for _, p := range t.Problems {
		ps = append(ps, txnProblem{Path: p.Path, What: p.What})
	}
	// convert keeps f in the unit that how gives, noting a value rounded
	// from noise or kept as NULL.
	convert := func(path string, leg *int, f float64, how func(float64) (int64, Conversion)) sql.NullInt64 {
		v, c := how(f)
		switch c {
		case Rounded:
			ps = append(ps, txnProblem{Path: path, Leg: leg, Flag: "rounded", Value: strconv.FormatFloat(f, 'g', -1, 64)})
		case Off:
			ps = append(ps, txnProblem{Path: path, Leg: leg, Flag: "inexact", Value: strconv.FormatFloat(f, 'g', -1, 64)})
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: v, Valid: true}
	}
	r := txnRow{
		time:           nullInstant(t.Time),
		tradeDate:      nullInstant(t.TradeDate),
		tradeDay:       nullString(string(t.TradeDay())),
		settlementDate: nullInstant(t.SettlementDate),
		settlementDay:  nullString(string(t.SettlementDay())),
		typ:            nullString(string(t.Type)),
		status:         nullString(string(t.Status)),
		subAccount:     nullString(t.SubAccount),
		description:    free(t.Description),
		orderID:        sql.NullInt64{Int64: t.OrderID, Valid: t.OrderID != 0},
		positionID:     sql.NullInt64{Int64: t.PositionID, Valid: t.PositionID != 0},
		activityType:   nullString(t.ActivityType),
	}
	if !slices.ContainsFunc(t.Problems, func(p schwab.Problem) bool { return p.Path == "netAmount" }) {
		r.net = convert("netAmount", nil, t.NetAmount, cents)
	}
	if t.QualifiedDividend != nil {
		r.qualifiedDividend = sql.NullBool{Bool: *t.QualifiedDividend, Valid: true}
	}
	if t.AccountNumber != "" && t.AccountNumber != number {
		r.numberSent = sql.NullString{String: mask.Number(t.AccountNumber), Valid: true}
	}
	// The record reads a leg without a price as 0; the raw JSON tells them
	// apart.
	var priced struct {
		TransferItems []struct {
			Price json.RawMessage `json:"price"`
		} `json:"transferItems"`
	}
	_ = json.Unmarshal(t.Raw, &priced) // what does not read has no price
	legs := make([]legRow, len(t.TransferItems))
	for i, it := range t.TransferItems {
		in := it.Instrument
		l := legRow{
			assetType:      nullString(string(in.AssetType)),
			instrumentID:   sql.NullInt64{Int64: in.InstrumentID, Valid: in.InstrumentID != 0},
			symbol:         nullString(in.Symbol),
			feeType:        nullString(it.FeeType),
			positionEffect: nullString(it.PositionEffect),
		}
		if in.AssetType == schwab.AssetCurrency {
			l.amountCents = convert("transferItems.amount", &i, it.Amount, cents)
			l.amountMicros, _ = exact(it.Amount, micros)
		} else {
			l.amountMicros = convert("transferItems.amount", &i, it.Amount, micros)
		}
		l.costCents = convert("transferItems.cost", &i, it.Cost, cents)
		if i < len(priced.TransferItems) && len(priced.TransferItems[i].Price) > 0 && string(priced.TransferItems[i].Price) != "null" {
			l.price = sql.NullFloat64{Float64: it.Price, Valid: true}
		}
		legs[i] = l
	}
	b, _ := json.Marshal(append([]txnProblem{}, ps...)) // a slice of plain values always marshals
	r.problems = string(b)
	return r, legs
}

// exact returns f in the unit that how gives, NULL when it is Off.
func exact(f float64, how func(float64) (int64, Conversion)) (sql.NullInt64, Conversion) {
	v, c := how(f)
	return sql.NullInt64{Int64: v, Valid: c != Off}, c
}

// decodeTxn reads raw JSON that the store keeps as the client reads a
// transaction.
func decodeTxn(raw string) schwab.Transaction {
	var t schwab.Transaction
	_ = json.Unmarshal([]byte(raw), &t) // lenient: it never fails
	return t
}

// rawDescription returns the description in a transaction's raw JSON as
// the record reads it (decodeTxn), without reading the rest: the last
// copy of the key decides, and one that is not a string, or null, is "". A
// description is free text, which the store keeps masked; the rules read
// it unmasked, from the raw JSON.
func rawDescription(raw string) string {
	var w struct {
		Description json.RawMessage `json:"description"`
	}
	var s string
	if json.Unmarshal([]byte(raw), &w) != nil || json.Unmarshal(w.Description, &s) != nil {
		return ""
	}
	return s
}

// rawIfMasked selects a transaction's latest raw JSON when its masked
// description, x.description, shows that masking changed it, and NULL
// otherwise: unmasked needs no more.
const rawIfMasked = "CASE WHEN instr(x.description, '...') > 0 THEN t.latest_raw END"

// unmasked returns a transaction's description as Schwab sent it, from
// description, the column that holds it masked, and raw, its latest raw
// JSON as rawIfMasked selects it. Masking writes "..." in place of the
// digits it hides, so a masked description without "..." is the
// description itself, and only one with it needs its raw JSON read.
func unmasked(description, raw sql.NullString) string {
	if raw.Valid {
		return rawDescription(raw.String)
	}
	return description.String
}

// nullInstant returns t as an instant column, NULL for the zero time.
func nullInstant(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: instant(t), Valid: true}
}

// deriveTxn writes the columns of transaction id, of account acct whose
// number is number, that its latest raw JSON, as the record t reads it,
// gives (layer 1 of the derivation): its txn row, which keeps its
// revisions and gone read, and its legs.
func deriveTxn(w *wtx, id, acct int64, number string, t schwab.Transaction) error {
	r, legs := txnFields(t, number)
	var activity sql.NullInt64
	if err := w.tx.QueryRowContext(w.ctx, "SELECT activity_id FROM api_txn WHERE txn_id = ?", id).Scan(&activity); err != nil {
		return err
	}
	if _, err := w.exec(`INSERT INTO txn (txn_id, account_id, activity_id, time, trade_date, trade_day, settlement_date,
		settlement_day, type, status, sub_account, description, net_cents, order_id, position_id, activity_type,
		qualified_dividend, number_sent_masked, problems)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (txn_id) DO UPDATE SET time = excluded.time, trade_date = excluded.trade_date,
		trade_day = excluded.trade_day, settlement_date = excluded.settlement_date,
		settlement_day = excluded.settlement_day, type = excluded.type, status = excluded.status,
		sub_account = excluded.sub_account, description = excluded.description, net_cents = excluded.net_cents,
		order_id = excluded.order_id, position_id = excluded.position_id, activity_type = excluded.activity_type,
		qualified_dividend = excluded.qualified_dividend, number_sent_masked = excluded.number_sent_masked,
		problems = excluded.problems`,
		id, acct, activity, r.time, r.tradeDate, r.tradeDay, r.settlementDate, r.settlementDay, r.typ, r.status,
		r.subAccount, r.description, r.net, r.orderID, r.positionID, r.activityType, r.qualifiedDividend,
		r.numberSent, r.problems); err != nil {
		return err
	}
	// The legs are written again without their securities, which the
	// grouping at the end of the write gives them.
	w.regroup = true
	if _, err := w.exec("DELETE FROM api_leg WHERE txn_id = ?", id); err != nil {
		return err
	}
	for i, l := range legs {
		if _, err := w.exec(`INSERT INTO api_leg (txn_id, leg, asset_type, instrument_id, symbol, amount_micros,
			amount_cents, cost_cents, price, fee_type, position_effect) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, i, l.assetType, l.instrumentID, l.symbol, l.amountMicros, l.amountCents, l.costCents, l.price,
			l.feeType, l.positionEffect); err != nil {
			return err
		}
	}
	return nil
}
