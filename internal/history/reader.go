package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// ErrNeedsUpdate means that a schwabtx of this version must open the store
// once, which brings it up to date, before a Reader can read it: an older
// schwabtx last wrote it, or it is not in WAL mode.
var ErrNeedsUpdate = errors.New("the store needs updating by schwabtx")

// ReaderOptions says which store a Reader reads and how long it waits.
type ReaderOptions struct {
	// Path is the store's file. Empty means DefaultPath. It is made
	// absolute.
	Path string
	// Warn is told what OpenReader noticed that its caller should know of:
	// a store on a FUSE file system or in a synced folder, or one that
	// others can read. Nil drops the warnings.
	Warn func(string)
	// LockWait is how long a read keeps trying while the store is busy,
	// before it gives up with ErrBusy. Only another process that holds the
	// store in exclusive locking mode, or a journal that a crash left to
	// recover, makes a reader wait: in WAL mode a write never does. Zero
	// means 5 seconds.
	LockWait time.Duration
	// BusyTimeout is how long SQLite itself waits for a lock before it
	// reports the store busy. Zero means 2 seconds.
	BusyTimeout time.Duration
}

// A Reader reads a store and never writes it: it holds only a pool of
// readers' connections, which refuse a write and never create a file, and
// none of its methods writes. Each read runs in one read transaction, so
// that what it returns is the store as of one commit. It is safe for
// concurrent use.
type Reader struct {
	s *Store
	// newer says that the last read found the store derived by a newer
	// schwabtx's rules or calendar.
	newer atomic.Bool
}

// OpenReader opens the store at o.Path for reading only. It never creates
// a store or its folder, never changes its schema, derived data, journal
// mode or files' permissions, and holds no writer: a Reader has no method
// that writes. A missing or empty store is ErrNoStore; a store with a
// schema newer than this build's is ErrNewer; one that an older schwabtx
// last derived (an older schema, rules or calendar), or that is not in WAL
// mode, is ErrNeedsUpdate. A store that a newer schwabtx derived with newer
// rules or a newer calendar, but whose schema is this build's, is read, as
// the store's own rule allows, and NewerRules says so. Network file
// systems, damage, files that are not a store and orphan journals are
// refused as Open refuses them. A store that others can read is reported
// through o.Warn, never made private.
func OpenReader(ctx context.Context, o ReaderOptions) (*Reader, error) {
	so := Options{
		Path: o.Path, MustExist: true, Now: time.Now, Warn: o.Warn, Waiting: func(string) {},
		BusyTimeout: o.BusyTimeout, LockWait: o.LockWait, disk: localDisk,
	}
	if so.Warn == nil {
		so.Warn = func(string) {}
	}
	if so.BusyTimeout <= 0 {
		so.BusyTimeout = 2 * time.Second
	}
	if so.LockWait <= 0 {
		so.LockWait = 5 * time.Second
	}
	path, err := storePath(so)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, o: so}
	if err := s.checkPath(); err != nil {
		return nil, err
	}
	const op = "opening the store"
	if err := s.retry(ctx, op, func() error { return s.identify(ctx) }); err != nil {
		return nil, s.mapError(op, err)
	}
	r := &Reader{s: s}
	if err := r.open(ctx); err != nil {
		s.Close()
		return nil, err
	}
	s.warnLoose()
	return r, nil
}

// open opens the reader's pool and reads the store's journal mode and
// versions, refusing a store that is not in WAL mode, or that is newer or
// older than this build.
func (r *Reader) open(ctx context.Context) error {
	s := r.s
	const op = "opening the store"
	db, err := sql.Open("sqlite", s.readOnlyDSN())
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(4)
	s.r = db
	var mode string
	if err := s.retry(ctx, op, func() error { return db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode) }); err != nil {
		return s.mapError(op, err)
	}
	if !strings.EqualFold(mode, "wal") {
		return &storeError{kind: ErrNeedsUpdate, msg: fmt.Sprintf("the store at %s is not in WAL mode (journal mode %s), which a reader needs; "+
			"any schwabtx command that opens it, such as schwabtx status, sets it", s.path, mode)}
	}
	var v versions
	err = s.retry(ctx, op, func() error {
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // It only read.
		v, err = readVersions(ctx, tx)
		return err
	})
	if err != nil {
		return s.mapError(op, err)
	}
	s.opened = v
	switch {
	case v.schema > buildVersions().schema:
		return s.newerError(v, true)
	case v.older():
		return s.needsUpdate(v)
	}
	r.newer.Store(v.newer())
	return nil
}

// readOnlyDSN is a Reader's: the readers' (readerDSN), with mode=rw, so
// that a connection that the pool opens after the store's files have gone
// fails, rather than making an empty store in their place.
func (s *Store) readOnlyDSN() string {
	return dsn(s.path, append(s.pragmas(), "_pragma=query_only(1)", "mode=rw")...)
}

// needsUpdate is ErrNeedsUpdate for a store whose versions are v, older
// than this build's.
func (s *Store) needsUpdate(v versions) error {
	return &storeError{kind: ErrNeedsUpdate, msg: fmt.Sprintf("the store at %s was last brought up to date by an older schwabtx "+
		"(schema %d, rules %d, calendar %d); any schwabtx command of this version that opens it, such as schwabtx status, "+
		"brings it up to date", s.path, v.schema, v.derive, v.calendar)}
}

// warnLoose warns once if others can read the store, its -wal or its
// -shm, by path: a Reader leaves them as they are, and schwabtx makes them
// private when it opens the store. Windows gives files their folder's
// permissions instead.
func (s *Store) warnLoose() {
	if runtime.GOOS == "windows" {
		return
	}
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if st, err := os.Stat(p); err == nil && st.Mode().Perm()&0o077 != 0 {
			s.o.Warn(s.path + " can be read by others; any schwabtx command that opens it, such as schwabtx status, " +
				"makes it readable only by you")
			return
		}
	}
}

// Close closes the reader's connections. Whichever connection of any
// process closes last checkpoints the store's -wal, which copies committed
// pages into the database and changes no row.
func (r *Reader) Close() error { return r.s.Close() }

// Path returns the store's file, made absolute.
func (r *Reader) Path() string { return r.s.path }

// NewerRules reports whether the last read found the store derived with
// newer rules or a newer calendar than this build's.
func (r *Reader) NewerRules() bool { return r.newer.Load() }

// read runs fn in one read transaction, as withRead does, after reading
// the store's versions in it: a store that an older schwabtx has since
// written is ErrNeedsUpdate, and NewerRules says what the read found.
func (r *Reader) read(ctx context.Context, fn func(*rtx) error) error {
	var v versions
	err := r.s.withRead(ctx, func(t *rtx) error {
		var err error
		if v, err = readVersions(t.ctx, t.tx); err != nil {
			return err
		}
		if v.older() {
			return r.s.needsUpdate(v)
		}
		return fn(t)
	})
	if err == nil {
		r.newer.Store(v.newer())
	}
	return err
}

// A NumberedAccount is an account's id and its full number.
type NumberedAccount struct {
	ID     int64
	Number string
}

// AccountNumbers returns each account's id and full number, by id, for a
// masker and for joining Schwab's accounts to the store's inside the
// process only. It is the one read outside the views, and nothing it
// returns may be shown.
func (r *Reader) AccountNumbers(ctx context.Context) ([]NumberedAccount, error) {
	var out []NumberedAccount
	err := r.read(ctx, func(t *rtx) error {
		out = nil
		return queryEach(reader(t), func(rows *sql.Rows) error {
			var a NumberedAccount
			err := rows.Scan(&a.ID, &a.Number)
			out = append(out, a)
			return err
		}, "SELECT account_id, number FROM account ORDER BY account_id")
	})
	return out, err
}

// A ReaderAccount is an account as the accounts view shows it.
type ReaderAccount struct {
	// ID is the account's id, and Ref its ref, A and the id.
	ID  int64
	Ref string
	// Masked is its number as Schwab masks it, "...135".
	Masked string
	// Nickname is its nickname and Kind the kind your account decision
	// gives it, each masked as free text when it was stored; Type its type
	// as its latest snapshot gives it, CASH or MARGIN; and Hash its hash
	// seen last. Each is "" when there is none.
	Nickname, Kind, Type, Hash string
	// Primary says that the preferences call it the primary account.
	Primary bool
	// NotSharedSince is when the login stopped sharing it, or the zero
	// time while it shares it.
	NotSharedSince time.Time
	// Used says that the store holds something of it: transactions,
	// snapshots, files not set aside, or a hash.
	Used bool
}

// Accounts returns the accounts view, by id.
func (r *Reader) Accounts(ctx context.Context) ([]ReaderAccount, error) {
	var out []ReaderAccount
	err := r.read(ctx, func(t *rtx) error {
		out = nil
		return queryEach(reader(t), func(rows *sql.Rows) error {
			var a ReaderAccount
			var nickname, kind, typ, hash, since sql.NullString
			var primary sql.NullBool
			if err := rows.Scan(&a.ID, &a.Ref, &a.Masked, &nickname, &kind, &typ, &hash, &primary, &since, &a.Used); err != nil {
				return err
			}
			a.Nickname, a.Kind, a.Type, a.Hash, a.Primary = nickname.String, kind.String, typ.String, hash.String, primary.Bool
			var err error
			a.NotSharedSince, err = parseNullInstant(since)
			out = append(out, a)
			return err
		}, `SELECT account_id, ref, account, nickname, kind, account_type, hash, primary_account, not_shared_since, used
			FROM accounts ORDER BY account_id`)
	})
	return out, err
}

// A LedgerQuery asks Ledger for a page of the ledger.
type LedgerQuery struct {
	// Accounts are the accounts to look at, at least one.
	Accounts []LedgerAccount
	// From and To are the days asked for, both included.
	From, To nytime.Date
	// Categories, when not empty, are the categories asked for.
	Categories []string
	// Types, when not empty, are Schwab's types asked for: API rows of
	// these types only, and no rows of files, which have none.
	Types []string
	// Symbol, when not "", is the symbol asked for, in any case.
	Symbol string
	// All asks for every entry, as the entries view holds them, not only
	// those that count, as the ledger view does.
	All bool
	// After, when not nil, is where the page starts: the rows strictly
	// after it, in the ledger's order (LedgerKey).
	After *LedgerKey
	// Limit is how many rows a page holds at most, 1 to 500.
	Limit int
}

// A LedgerAccount is an account that Ledger looks at.
type LedgerAccount struct {
	ID int64
	// Key orders this account's rows after rows of other accounts that tie
	// on day, time and ref: its hash, or A and its id. The server merges
	// Schwab's rows by the same keys.
	Key string
	// IfWhole reads the account's rows only when its api and file
	// coverage, merged where runs meet or overlap, holds every day from
	// From to To; else only its coverage, waiting counts and last read come
	// back.
	IfWhole bool
}

// A LedgerKey is a row's place in the ledger's order: newest first, by
// day, then time, then ref, then account, each in descending order, the
// time compared as the store writes an instant, which sorts as text. A
// ref is unique within an account only, so the account's key is needed to
// make the order total.
type LedgerKey struct {
	Day nytime.Date
	// Time is the store's instant text, "" for a row of a file.
	Time string
	Ref  string
	// Account is the key of the row's LedgerAccount.
	Account string
}

// A LedgerPage is one page of the ledger and what Ledger read with it, as
// of one commit.
type LedgerPage struct {
	// Read are the ids of the accounts whose rows were read: every listed
	// account but those of IfWhole whose coverage does not hold the days.
	Read []int64
	// Rows are the page's rows, in the ledger's order.
	Rows []LedgerRow
	// Next is where the next page starts, or nil on the last page.
	Next *LedgerKey
	// Totals are, per category, the counted rows of the read accounts that
	// the query matches, on every page (After is not applied), by
	// category's name; empty when none.
	Totals []CategoryTotal
	// Matched counts, per read account, the rows that the query matches on
	// every page: those that count, or every entry under All.
	Matched []AccountRows
	// Coverage are the coverages view's runs that meet the days asked, of
	// every listed account, by account, source and start.
	Coverage []CoverageRun
	// Waiting are, for every listed account, its uncertain and held-out
	// entries of the days asked, and its flags and files that wait, as
	// Store.Status counts them.
	Waiting []AccountWaiting
	// Reads are, for every listed account with one, when the latest read
	// of its transactions, or import of a -json file, was answered.
	Reads []AccountRead
}

// A LedgerRow is a row of the entries view, and the status of its API
// transaction.
type LedgerRow struct {
	// Key is the row's place in the ledger's order. EntryByRef leaves its
	// Account "".
	Key LedgerKey
	// Ref is the entry's ref, Key's, and AccountID and Account its
	// account's id and masked number.
	Ref       string
	AccountID int64
	Account   string
	// Source is api for an API transaction, file for a file transaction.
	Source string
	// Day is the day it was booked on; PostedDate a file transaction's
	// posted date and QuantityAsOf the date of the export that gives its
	// quantity, each "" for an API transaction.
	Day, PostedDate, QuantityAsOf nytime.Date
	// Time is its time, or the zero time for none.
	Time time.Time
	// Kind is Schwab's type, or the file's action, free text; Description
	// its description, masked as free text when stored.
	Kind, Description                                  string
	AmountCents, FeesCents, GrossCents, QuantityMicros *int64
	Price                                              *float64
	// Symbol is the symbol it names and SecurityKey its security's key,
	// each "" for none.
	Symbol, SecurityKey string
	// Category and Rule are its category and the rule that booked it, or
	// user when you did.
	Category, Rule string
	// PairKind, PairRef and PairAccountID name the entry it is paired with,
	// "" and 0 for none.
	PairKind, PairRef string
	PairAccountID     int64
	// Counted says whether it counts, and CountedWhy why.
	Counted    bool
	CountedWhy string
	// Flags are its flags' codes, and MatchedWith the refs of the entries
	// that it is linked to, in order.
	Flags, MatchedWith []string
	// Status is the status of an API transaction, "" for a file's.
	Status string
}

// A CategoryTotal is the count of a category's rows, and the sums of
// their amounts and fees in cents, each nil when one row's is NULL.
type CategoryTotal struct {
	Category               string
	Count                  int
	AmountCents, FeesCents *int64
}

// An AccountRows counts an account's rows.
type AccountRows struct {
	AccountID int64
	Rows      int
}

// A CoverageRun is a run of days on which a source holds every one of an
// account's transactions, as the coverages view shows it: api, api_file,
// file, file_unknown, file_filtered, realized or snapshot.
type CoverageRun struct {
	AccountID int64
	Source    string
	From, To  nytime.Date
}

// An AccountWaiting is what waits for your decision in an account, as
// Store.Status counts it: Uncertain file transactions, which several API
// transactions could be; HeldOut ones, which the API's dates hold and no
// API transaction is; Attention, the other flags that need you; Filtered,
// the files that may be filtered.
type AccountWaiting struct {
	AccountID                               int64
	Uncertain, HeldOut, Attention, Filtered int
}

// An AccountRead is when the latest read of an account's transactions, or
// import of a -json file of them, was answered.
type AccountRead struct {
	AccountID  int64
	AnsweredAt time.Time
}

// Ledger chooses, reads and pages in one read transaction, so that the
// choice of accounts, the page, the totals, the coverage and the counts
// agree: for each of q.Accounts, its coverage, waiting counts and last
// read; and for those it reads (every one, but those of IfWhole whose
// coverage does not hold the days), one page of entries after q.After, the
// totals of the query and the rows each account matches.
func (r *Reader) Ledger(ctx context.Context, q LedgerQuery) (*LedgerPage, error) {
	if err := q.check(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(q.Accounts))
	for i, a := range q.Accounts {
		ids[i] = a.ID
	}
	listed := mustJSON(ids)
	var page *LedgerPage
	err := r.read(ctx, func(t *rtx) error {
		p := &LedgerPage{Totals: []CategoryTotal{}}
		w := reader(t)
		if err := queryEach(w, func(rows *sql.Rows) error {
			var c CoverageRun
			var from, to string
			err := rows.Scan(&c.AccountID, &c.Source, &from, &to)
			c.From, c.To = nytime.Date(from), nytime.Date(to)
			p.Coverage = append(p.Coverage, c)
			return err
		}, `SELECT c.account_id, c.source, c.from_date, c.to_date FROM json_each(?1) a JOIN coverages c ON c.account_id = a.value
			WHERE c.to_date >= ?2 AND c.from_date <= ?3 ORDER BY c.account_id, c.source, c.from_date`, listed, string(q.From), string(q.To)); err != nil {
			return err
		}
		waits, err := countWaitingOf(w, listed, q.From, q.To)
		if err != nil {
			return err
		}
		for _, a := range q.Accounts {
			st := waits[a.ID]
			p.Waiting = append(p.Waiting, AccountWaiting{a.ID, st.Uncertain, st.HeldOut, st.Attention, st.Filtered})
		}
		if err := queryEach(w, func(rows *sql.Rows) error {
			var a AccountRead
			var at string
			if err := rows.Scan(&a.AccountID, &at); err != nil {
				return err
			}
			var err error
			a.AnsweredAt, err = parseInstant(at)
			p.Reads = append(p.Reads, a)
			return err
		}, `SELECT r.account_id, max(r.answered_at) FROM json_each(?1) a JOIN reads r ON r.account_id = a.value
			WHERE r.endpoint IN ('transactions', 'transaction', 'file') AND r.outcome = 'answered'
			GROUP BY r.account_id ORDER BY r.account_id`, listed); err != nil {
			return err
		}
		var read [][2]any
		for _, a := range q.Accounts {
			if !a.IfWhole || whole(p.Coverage, a.ID, q.From, q.To) {
				p.Read = append(p.Read, a.ID)
				read = append(read, [2]any{a.ID, a.Key})
			}
		}
		if len(read) > 0 {
			if err := ledgerPage(t, q, mustJSON(read), p); err != nil {
				return err
			}
		}
		page = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return page, nil
}

// check refuses a query that Ledger cannot answer.
func (q LedgerQuery) check() error {
	if len(q.Accounts) == 0 {
		return errors.New("reading the ledger: no account to read")
	}
	ids, keys := map[int64]bool{}, map[string]bool{}
	for _, a := range q.Accounts {
		if a.Key == "" || ids[a.ID] || keys[a.Key] {
			return fmt.Errorf("reading the ledger: account %d is listed twice, or has no key or another's", a.ID)
		}
		ids[a.ID], keys[a.Key] = true, true
	}
	for _, d := range []nytime.Date{q.From, q.To} {
		if _, err := time.Parse(time.DateOnly, string(d)); err != nil {
			return fmt.Errorf("reading the ledger: %q is not a date", d)
		}
	}
	switch {
	case q.To < q.From:
		return fmt.Errorf("reading the ledger: the days end on %s, before they start on %s", q.To, q.From)
	case q.Limit < 1 || q.Limit > 500:
		return fmt.Errorf("reading the ledger: a page of %d rows; a page holds 1 to 500", q.Limit)
	case q.After != nil && (q.After.Ref == "" || q.After.Account == ""):
		return errors.New("reading the ledger: a page that starts after a row without its ref or account")
	}
	return nil
}

// whole reports whether runs, coverage of accounts, hold for account every
// day from from to to in their api and file runs, merged where they meet
// or overlap: the store's own definition of whole history.
func whole(runs []CoverageRun, account int64, from, to nytime.Date) bool {
	var spans []coverageRun
	for _, r := range runs {
		if r.AccountID == account && (r.Source == "api" || r.Source == "file") {
			spans = append(spans, coverageRun{source: "whole", from: r.From, to: r.To})
		}
	}
	return slices.ContainsFunc(mergeRuns(spans), func(r coverageRun) bool { return r.from <= from && to <= r.to })
}

// The queries of a page of the ledger, in three steps, so that the entries
// view's correlated subqueries (flags, matched_with, pair_ref) run for the
// page's rows alone. ledgerKeysQuery and ledgerMatchQuery are driven from
// json_each of the accounts read, ?1 as [[id, key], ...], so that SQLite
// searches the entry_day index of each, and the view, flattened, runs no
// subquery of a column they do not read.
const (
	// ledgerFilter is the filters of the query: ?2 and ?3 the days, ?4
	// All, ?5 the categories and ?6 the types, each a JSON list or NULL,
	// and ?7 the symbol or NULL.
	ledgerFilter = `e.day BETWEEN ?2 AND ?3
		AND (?4 OR e.counted = 1)
		AND (?5 IS NULL OR e.category IN (SELECT value FROM json_each(?5)))
		AND (?6 IS NULL OR (e.source = 'api' AND e.kind IN (SELECT value FROM json_each(?6))))
		AND (?7 IS NULL OR e.symbol = ?7 COLLATE NOCASE)`
	// ledgerKeysQuery is step 1, the keys of the page's rows: strictly after
	// ?8 to ?11, the cursor's day, time, ref and account key, or from the
	// start when ?8 is NULL; ?12 rows at most.
	ledgerKeysQuery = `SELECT e.day, coalesce(e.time, ''), e.ref, a.value ->> 1 AS akey, e.account_id
		FROM json_each(?1) a JOIN entries e ON e.account_id = a.value ->> 0
		WHERE ` + ledgerFilter + `
		AND (?8 IS NULL OR (e.day, coalesce(e.time, ''), e.ref, a.value ->> 1) < (?8, ?9, ?10, ?11))
		ORDER BY e.day DESC, coalesce(e.time, '') DESC, e.ref DESC, akey DESC
		LIMIT ?12`
	// ledgerRowsQuery is step 2, the rows of the keys ?1, [[account_id,
	// ref], ...], each with its API transaction's status, read from txn by
	// the entry's txn_id.
	ledgerRowsQuery = entrySelect + ` WHERE (e.account_id, e.ref) IN (SELECT value ->> 0, value ->> 1 FROM json_each(?1))`
	// ledgerMatchQuery is step 3: what the query matches on every page, by
	// account, category and whether it counts, for the totals and the
	// counts of rows.
	ledgerMatchQuery = `SELECT e.account_id, e.category, e.counted, count(*), count(e.amount_cents), sum(e.amount_cents),
		count(e.fees_cents), sum(e.fees_cents)
		FROM json_each(?1) a JOIN entries e ON e.account_id = a.value ->> 0
		WHERE ` + ledgerFilter + `
		GROUP BY e.account_id, e.category, e.counted`
	// entrySelect selects the entries view's columns and the status of the
	// entry's API transaction.
	entrySelect = `SELECT e.ref, e.account_id, e.account, e.source, e.day, e.posted_date, e.time, e.kind, e.description,
		e.amount_cents, e.fees_cents, e.gross_cents, e.quantity_micros, e.quantity_as_of, e.price, e.symbol,
		e.security_key, e.category, e.category_rule, e.pair_kind, e.pair_ref, e.pair_account_id, e.counted,
		e.counted_why, e.flags, e.matched_with,
		(SELECT x.status FROM entry n JOIN txn x ON x.txn_id = n.txn_id WHERE n.account_id = e.account_id AND n.ref = e.ref) AS status
		FROM entries e`
)

// ledgerArgs returns the arguments of ledgerFilter, ?2 to ?7, of q.
func ledgerArgs(q LedgerQuery) []any {
	list := func(ss []string) any {
		if len(ss) == 0 {
			return nil
		}
		return mustJSON(ss)
	}
	return []any{string(q.From), string(q.To), q.All, list(q.Categories), list(q.Types), nullString(q.Symbol)}
}

// ledgerPage reads into p the page of q of the accounts read, given as
// [[id, key], ...]: the rows, the next page's start, the totals and the
// rows each account matches.
func ledgerPage(t *rtx, q LedgerQuery, read string, p *LedgerPage) error {
	w := reader(t)
	args := append([]any{read}, ledgerArgs(q)...)
	var after [4]any
	if q.After != nil {
		after = [4]any{string(q.After.Day), q.After.Time, q.After.Ref, q.After.Account}
	}
	type key struct {
		LedgerKey
		id int64
	}
	var page []key
	if err := queryEach(w, func(rows *sql.Rows) error {
		var k key
		var day string
		err := rows.Scan(&day, &k.Time, &k.Ref, &k.Account, &k.id)
		k.Day = nytime.Date(day)
		page = append(page, k)
		return err
	}, ledgerKeysQuery, append(args, after[0], after[1], after[2], after[3], q.Limit+1)...); err != nil {
		return err
	}
	if len(page) > q.Limit {
		next := page[q.Limit-1].LedgerKey
		p.Next, page = &next, page[:q.Limit]
	}
	if len(page) > 0 {
		wanted := make([][2]any, len(page))
		for i, k := range page {
			wanted[i] = [2]any{k.id, k.Ref}
		}
		rows := map[[2]any]LedgerRow{}
		if err := queryEach(w, func(r *sql.Rows) error {
			x, err := scanLedgerRow(r)
			rows[[2]any{x.AccountID, x.Ref}] = x
			return err
		}, ledgerRowsQuery, mustJSON(wanted)); err != nil {
			return err
		}
		for _, k := range page {
			x, ok := rows[[2]any{k.id, k.Ref}]
			if !ok {
				return fmt.Errorf("reading the ledger: entry %s of account %d is gone from the read", k.Ref, k.id)
			}
			x.Key = k.LedgerKey
			p.Rows = append(p.Rows, x)
		}
	}
	totals := map[string]*CategoryTotal{}
	nulls := map[string][2]bool{}
	matched := map[int64]int{}
	if err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var category string
		var counted bool
		var n, amounts, fees int
		var amount, fee sql.NullInt64
		if err := rows.Scan(&id, &category, &counted, &n, &amounts, &amount, &fees, &fee); err != nil {
			return err
		}
		matched[id] += n
		if !counted {
			return nil
		}
		c := totals[category]
		if c == nil {
			c = &CategoryTotal{Category: category, AmountCents: new(int64), FeesCents: new(int64)}
			totals[category] = c
		}
		c.Count += n
		*c.AmountCents += amount.Int64
		*c.FeesCents += fee.Int64
		was := nulls[category]
		nulls[category] = [2]bool{was[0] || amounts < n, was[1] || fees < n}
		return nil
	}, ledgerMatchQuery, args...); err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(totals)) {
		c := totals[name]
		if nulls[name][0] {
			c.AmountCents = nil
		}
		if nulls[name][1] {
			c.FeesCents = nil
		}
		p.Totals = append(p.Totals, *c)
	}
	for _, id := range p.Read {
		if n, ok := matched[id]; ok {
			p.Matched = append(p.Matched, AccountRows{AccountID: id, Rows: n})
		}
	}
	return nil
}

// scanLedgerRow reads a row of entrySelect.
func scanLedgerRow(rows *sql.Rows) (LedgerRow, error) {
	var x LedgerRow
	var day, posted, at, kind, description, asOf, symbol, security, pairKind, pairRef, flags, matched, status sql.NullString
	var amount, fees, gross, quantity, pairAccount sql.NullInt64
	var price sql.NullFloat64
	if err := rows.Scan(&x.Ref, &x.AccountID, &x.Account, &x.Source, &day, &posted, &at, &kind, &description,
		&amount, &fees, &gross, &quantity, &asOf, &price, &symbol, &security, &x.Category, &x.Rule, &pairKind, &pairRef,
		&pairAccount, &x.Counted, &x.CountedWhy, &flags, &matched, &status); err != nil {
		return x, err
	}
	x.Day, x.PostedDate, x.QuantityAsOf = nytime.Date(day.String), nytime.Date(posted.String), nytime.Date(asOf.String)
	x.Kind, x.Description, x.Symbol, x.SecurityKey = kind.String, description.String, symbol.String, security.String
	x.AmountCents, x.FeesCents, x.GrossCents, x.QuantityMicros = nullInt(amount), nullInt(fees), nullInt(gross), nullInt(quantity)
	x.Price = nullFloat(price)
	x.PairKind, x.PairRef, x.PairAccountID, x.Status = pairKind.String, pairRef.String, pairAccount.Int64, status.String
	x.Flags, x.MatchedWith = splitList(flags), splitList(matched)
	x.Key = LedgerKey{Day: x.Day, Time: at.String, Ref: x.Ref}
	var err error
	x.Time, err = parseNullInstant(at)
	return x, err
}

// splitList returns a list that a view joins with commas, nil for NULL.
func splitList(s sql.NullString) []string {
	if !s.Valid || s.String == "" {
		return nil
	}
	return strings.Split(s.String, ",")
}

// mustJSON returns v as JSON, for json_each: lists of numbers and strings
// always marshal.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("history: " + err.Error())
	}
	return string(b)
}

// Waiting returns what waits for your decision, per account that the store
// uses, by id, as Store.Status counts it of that account, so that each
// account's sum is what `schwabtx status -account` reports of it. What
// waits that is of no account, such as a snapshot whose account number
// could not be read, is in one more, of AccountID 0, so that the counts of
// every one add up to Status.Waiting of every account, what `schwabtx
// status` reports.
func (r *Reader) Waiting(ctx context.Context) ([]AccountWaiting, error) {
	var out []AccountWaiting
	err := r.read(ctx, func(t *rtx) error {
		out = nil
		w := reader(t)
		ids := []int64{}
		if err := queryEach(w, func(rows *sql.Rows) error {
			var id int64
			err := rows.Scan(&id)
			ids = append(ids, id)
			return err
		}, "SELECT account_id FROM accounts WHERE used ORDER BY account_id"); err != nil {
			return err
		}
		waits, err := countWaitingOf(w, mustJSON(ids), "", "")
		if err != nil {
			return err
		}
		var all, sum Status
		for _, id := range ids {
			st := waits[id]
			out = append(out, AccountWaiting{id, st.Uncertain, st.HeldOut, st.Attention, st.Filtered})
			sum.Uncertain, sum.HeldOut, sum.Attention, sum.Filtered = sum.Uncertain+st.Uncertain, sum.HeldOut+st.HeldOut,
				sum.Attention+st.Attention, sum.Filtered+st.Filtered
		}
		if err := countWaiting(w, sql.NullInt64{}, "", "", &all); err != nil {
			return err
		}
		if all.Waiting() > sum.Waiting() {
			out = append(out, AccountWaiting{
				0, all.Uncertain - sum.Uncertain, all.HeldOut - sum.HeldOut,
				all.Attention - sum.Attention, all.Filtered - sum.Filtered,
			})
		}
		return nil
	})
	return out, err
}

// An EntryDetail is an entry with its legs and its flags.
type EntryDetail struct {
	Entry LedgerRow
	// Legs are the legs view's rows of the entry, by leg.
	Legs []LegRow
	// Flags are the flags view's rows of the entry, by code.
	Flags []FlagRow
}

// A LegRow is a row of the legs view: one leg of an API transaction, or one
// that a file transaction is given, its security first and its fees after.
type LegRow struct {
	Leg int
	// AssetType, Symbol and SecurityKey are what the leg moves, and
	// InstrumentID and ItemIssueID its ids, each "" or nil for none.
	AssetType, Symbol, SecurityKey string
	InstrumentID                   *int64
	ItemIssueID                    string
	// QuantityMicros is a security leg's shares or contracts; CashCents a
	// CURRENCY leg's cash, the fee on a trade's fee leg.
	QuantityMicros, CashCents, CostCents *int64
	Price                                *float64
	FeeType, PositionEffect              string
	// PutCall, Strike, ExpirationDay, Multiplier and UnderlyingSymbol are
	// an option's, as its security holds them.
	PutCall          string
	Strike           *float64
	ExpirationDay    nytime.Date
	Multiplier       *float64
	UnderlyingSymbol string
}

// A FlagRow is a flag of an entry: its code, its severity, attention or
// info, and its detail, composed of free text masked when it was stored and
// of identifiers, or "".
type FlagRow struct {
	Code, Severity, Detail string
}

// EntryByRef returns every entry whose ref is ref, its letter in either
// case, by account, with its legs and flags: normally one, since a ref
// names its account's transaction; two when two accounts share an activity
// ID.
func (r *Reader) EntryByRef(ctx context.Context, ref string) ([]EntryDetail, error) {
	var out []EntryDetail
	err := r.read(ctx, func(t *rtx) error {
		out = nil
		w := reader(t)
		if err := queryEach(w, func(rows *sql.Rows) error {
			x, err := scanLedgerRow(rows)
			out = append(out, EntryDetail{Entry: x})
			return err
		}, entrySelect+" WHERE lower(e.ref) = lower(?) ORDER BY e.account_id", strings.TrimSpace(ref)); err != nil {
			return err
		}
		for i := range out {
			d := &out[i]
			if err := queryEach(w, func(rows *sql.Rows) error {
				var l LegRow
				var asset, symbol, security, item, fee, effect, putCall, expiration, underlying sql.NullString
				var instrument, quantity, cash, cost sql.NullInt64
				var price, strike, multiplier sql.NullFloat64
				if err := rows.Scan(&l.Leg, &asset, &symbol, &security, &instrument, &item, &quantity, &cash, &cost, &price,
					&fee, &effect, &putCall, &strike, &expiration, &multiplier, &underlying); err != nil {
					return err
				}
				l.AssetType, l.Symbol, l.SecurityKey, l.ItemIssueID = asset.String, symbol.String, security.String, item.String
				l.InstrumentID, l.QuantityMicros, l.CashCents, l.CostCents = nullInt(instrument), nullInt(quantity), nullInt(cash), nullInt(cost)
				l.Price, l.Strike, l.Multiplier = nullFloat(price), nullFloat(strike), nullFloat(multiplier)
				l.FeeType, l.PositionEffect, l.PutCall = fee.String, effect.String, putCall.String
				l.ExpirationDay, l.UnderlyingSymbol = nytime.Date(expiration.String), underlying.String
				d.Legs = append(d.Legs, l)
				return nil
			}, `SELECT leg, asset_type, symbol, security_key, instrument_id, item_issue_id, quantity_micros, cash_cents, cost_cents,
				price, fee_type, position_effect, put_call, strike, expiration_day, multiplier, underlying_symbol
				FROM legs WHERE account_id = ? AND ref = ? ORDER BY leg`, d.Entry.AccountID, d.Entry.Ref); err != nil {
				return err
			}
			if err := queryEach(w, func(rows *sql.Rows) error {
				var f FlagRow
				var detail sql.NullString
				err := rows.Scan(&f.Code, &f.Severity, &detail)
				f.Detail = detail.String
				d.Flags = append(d.Flags, f)
				return err
			}, `SELECT code, severity, detail FROM flags WHERE account_id = ? AND subject_ref = ? ORDER BY code`,
				d.Entry.AccountID, d.Entry.Ref); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}
