package history

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/nyse"
	"github.com/dterei/schwab-mcp/internal/nytime"
)

// The kinds of thing that a ref names (5.4 of the design), as Target.Kind
// says them.
const (
	TargetAccount         = "account"
	TargetTransaction     = "API transaction"
	TargetFileTransaction = "file transaction"
	TargetFile            = "file"
	TargetLot             = "lot"
	TargetSnapshot        = "snapshot"
	TargetKey             = "instrument key"
)

// A Target is what a ref names in the store.
type Target struct {
	// Kind is what it is: one of the Target constants.
	Kind string
	// Ref is its ref as the views show it: A3, T73508261941, C3a9e21b0.17,
	// F3a9e21b0, L5c1d2e3f.4, S812 or K:i:1973757747.
	Ref string
	// Subject is how your decisions name it (16 of the design): a:…, c:…,
	// l:…, f:…, k:… or acct:…; "" for a snapshot, which takes none.
	Subject string
	// AccountID is its account's id, and Account how messages name the
	// account, "A2 ...456 Joint"; 0 and "" for a key, and for a file or a
	// snapshot of no account.
	AccountID int64
	Account   string
}

// reader returns a wtx over the read transaction r, for the helpers that
// take one and only read through it (selectAccount with add false,
// accountLabel, findFile, queryEach): the readers' connections refuse a
// write.
func reader(r *rtx) *wtx { return &wtx{tx: r.tx, ctx: r.ctx} }

// Find returns what ref names (5.4 of the design), its letter in either
// case: an account (A3), an API transaction by its activity ID
// (T73508261941) or, without one, its time and digest (T~…), a file
// transaction (C3a9e21b0.17) or a lot (L5c1d2e3f.4) by the start of its
// file's SHA-256 in hex and its row, a file by the start of its SHA-256
// (F3a9e21b0), a snapshot (S812) or an instrument key (K:i:1973757747).
// Any start of a digest that names one thing will do. account, when it is
// not "", selects an account (ImportOptions.Account) that the ref must be
// of, which tells apart transactions of two accounts that share an
// activity ID. A ref that names nothing, or several things, is an error
// that says so, listing the candidates.
func (s *Store) Find(ctx context.Context, ref, account string) (Target, error) {
	var t Target
	err := s.withRead(ctx, func(r *rtx) error {
		var err error
		t, err = find(reader(r), ref, account)
		return err
	})
	return t, err
}

// refShape is a ref of a file transaction or a lot: its letter, the start
// of its file's digest in hex, and its row's ordinal.
var refShape = regexp.MustCompile(`^([cl])([0-9a-f]+)\.([1-9][0-9]*)$`)

// find returns what ref names, as Find does, through w, which it only
// reads.
func find(w *wtx, ref, account string) (Target, error) {
	given := strings.TrimSpace(ref)
	var only sql.NullInt64
	if account != "" {
		id, err := selectAccount(w, account, false)
		if err != nil {
			return Target{}, err
		}
		only = sql.NullInt64{Int64: id, Valid: true}
	}
	lower := strings.ToLower(given)
	shown := mask.Text(given)
	var targets []Target
	// add appends the targets that query returns, each its ref, subject and
	// account, of the account asked for.
	add := func(kind, query string, args ...any) error {
		return queryEach(w, func(rows *sql.Rows) error {
			t := Target{Kind: kind}
			var acct sql.NullInt64
			if err := rows.Scan(&t.Ref, &t.Subject, &acct); err != nil {
				return err
			}
			if only.Valid && acct.Int64 != only.Int64 {
				return nil
			}
			t.AccountID = acct.Int64
			targets = append(targets, t)
			return nil
		}, query, args...)
	}
	var err error
	var what string
	switch m := refShape.FindStringSubmatch(lower); {
	case strings.HasPrefix(lower, "k:"):
		what = "instrument key"
		key := given[2:]
		err = add(TargetKey, `SELECT 'K:' || key, 'k:' || key, NULL FROM instrument_key WHERE key = ?1
			UNION SELECT 'K:' || key, 'k:' || key, NULL FROM instrument_key WHERE lower(key) = lower(?1)
				AND NOT EXISTS (SELECT 1 FROM instrument_key WHERE key = ?1) ORDER BY 1`, key)
	case m != nil && m[1] == "c":
		what = "file transaction"
		err = add(TargetFileTransaction, `SELECT e.ref, c.subject, c.account_id FROM csv_txn c JOIN entry e ON e.csv_txn_id = c.csv_txn_id
			WHERE substr(c.subject, 3, ?1) = ?2 AND c.subject LIKE '%:' || ?3 ORDER BY c.account_id, e.ref`,
			len(m[2]), m[2], m[3])
	case m != nil && m[1] == "l":
		what = "lot"
		err = add(TargetLot, `SELECT ref, subject, account_id FROM realized_lot
			WHERE substr(subject, 3, ?1) = ?2 AND subject LIKE '%:' || ?3 ORDER BY account_id, ref`, len(m[2]), m[2], m[3])
	case strings.HasPrefix(lower, "t~"):
		what = "transaction"
		err = add(TargetTransaction, `SELECT ref, subject, account_id FROM entry
			WHERE txn_id IS NOT NULL AND lower(ref) = ?1 ORDER BY account_id`, lower)
	case isDigits(strings.TrimPrefix(lower, "t")) && strings.HasPrefix(lower, "t"):
		what = "transaction"
		err = add(TargetTransaction, `SELECT e.ref, e.subject, e.account_id FROM entry e JOIN api_txn t ON t.txn_id = e.txn_id
			WHERE t.activity_id = ? ORDER BY e.account_id`, lower[1:])
	case isDigits(strings.TrimPrefix(lower, "a")) && strings.HasPrefix(lower, "a"):
		what = "account"
		err = add(TargetAccount, "SELECT 'A' || account_id, 'acct:' || account_id, account_id FROM account WHERE account_id = ?", lower[1:])
	case isDigits(strings.TrimPrefix(lower, "s")) && strings.HasPrefix(lower, "s"):
		what = "snapshot"
		err = add(TargetSnapshot, `SELECT 'S' || s.snapshot_id, '', f.account_id FROM snapshot s
			JOIN snapshot_fields f ON f.snapshot_id = s.snapshot_id WHERE s.snapshot_id = ?`, lower[1:])
	case strings.HasPrefix(lower, "f") && isHex(lower[1:]):
		what = "file"
		id, sha, _, fref, ferr := findFile(w, given)
		if ferr != nil {
			return Target{}, ferr
		}
		err = add(TargetFile, "SELECT ?, ?, account_id FROM file_fields WHERE file_id = ?", fref, "f:"+sha, id)
	default:
		return Target{}, fmt.Errorf("%q is not a ref: a ref is A3, T73508261941, C3a9e21b0.17, F3a9e21b0, L5c1d2e3f.4, S812 or K:i:1973757747", shown)
	}
	if err != nil {
		return Target{}, err
	}
	for i := range targets {
		if targets[i].AccountID != 0 {
			if targets[i].Account, err = accountLabel(w, targets[i].AccountID); err != nil {
				return Target{}, err
			}
		}
	}
	switch {
	case len(targets) == 1:
		return targets[0], nil
	case len(targets) == 0 && only.Valid:
		label, err := accountLabel(w, only.Int64)
		return Target{}, cmp.Or(err, fmt.Errorf("no %s %s in %s", what, shown, label))
	case len(targets) == 0:
		return Target{}, fmt.Errorf("no %s %s in the store", what, shown)
	}
	names := make([]string, len(targets))
	sameRef := true
	for i, t := range targets {
		names[i] = t.Ref
		if t.Account != "" {
			names[i] += " of " + t.Account
		}
		sameRef = sameRef && t.Ref == targets[0].Ref
	}
	if sameRef {
		return Target{}, fmt.Errorf("%s fits several %ss, %s; say which account with -account", shown, what, joinAnd(names))
	}
	return Target{}, fmt.Errorf("%s fits several %ss, %s; give more of it", shown, what, joinAnd(names))
}

// isDigits reports whether s is one or more ASCII digits.
func isDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// isHex reports whether s is one or more lower-case hex digits.
func isHex(s string) bool { return s != "" && strings.Trim(s, "0123456789abcdef") == "" }

// Label returns how messages name the account that sel selects
// (ImportOptions.Account), "A2 ...456 Joint", and its id.
func (s *Store) Label(ctx context.Context, sel string) (id int64, label string, err error) {
	err = s.withRead(ctx, func(r *rtx) error {
		w := reader(r)
		var err error
		if id, err = selectAccount(w, sel, false); err != nil {
			return err
		}
		label, err = accountLabel(w, id)
		return err
	})
	return id, label, err
}

// A Status is the state of the store, as `schwabtx status` prints it.
type Status struct {
	// Schema, Rules and Calendar are the store's versions, and
	// VerifiedThrough the last year that the calendar was checked against
	// NYSE's published dates.
	Schema, Rules, Calendar, VerifiedThrough int
	// Accounts are the accounts, or the one asked for.
	Accounts []AccountStatus
	// FromAPI counts the entries that count from the API, FromFiles those
	// that count from files only, and Linked the file transactions linked
	// to API transactions, whose records count in their place.
	FromAPI, FromFiles, Linked int
	// ItemIssueAgree counts the securities whose History rows'
	// ItemIssueIds are each one of the security's instrumentIds, of the
	// ItemIssueBoth securities that have both.
	ItemIssueAgree, ItemIssueBoth int
	// What waits for you (status -review): Uncertain file transactions,
	// which several API transactions could be; HeldOut ones, which the
	// API's dates hold and no API transaction is; Attention, the other
	// flags that need you; Filtered, the files that may be filtered.
	Uncertain, HeldOut, Attention, Filtered int
	// Warnings are what Check warns of too: account numbers that masking
	// by pattern would miss.
	Warnings []string
}

// Waiting counts what waits for you.
func (s Status) Waiting() int { return s.Uncertain + s.HeldOut + s.Attention + s.Filtered }

// An AccountStatus is an account's part of a Status.
type AccountStatus struct {
	// ID is the account's id, and Label how messages name it, "A1 ...398
	// Individual".
	ID    int64
	Label string
	// Number is its full number, only when StatusOptions.ShowAccounts
	// asks for it.
	Number string
	// API is the days that Schwab's API (or `-json` files) covers, Files
	// those that History files cover, and Snapshots those from its first
	// to its last snapshot.
	API, Files, Snapshots Span
	// Gaps counts the sessions between its first and last snapshot taken
	// after a close that no snapshot stands for, and Since those that have
	// closed since its last such snapshot.
	Gaps, Since int
}

// A Span is the days from From to To, in Runs runs of coverage; Runs is 0
// when nothing covers any.
type Span struct {
	From, To nytime.Date
	Runs     int
}

// StatusOptions says what Status reads.
type StatusOptions struct {
	// Account selects one account (ImportOptions.Account); "" means every
	// account.
	Account string
	// ShowAccounts reads each account's full number into the status.
	ShowAccounts bool
}

// attentionOther are the attention flags that Status counts apart from
// Attention.
var attentionOther = []string{"uncertain_match", "held_out", "looks_filtered", "lacks_rows"}

// Status returns the state of the store: its versions, what each account's
// sources cover, how its snapshots stand, what counts, and what waits for
// you.
func (s *Store) Status(ctx context.Context, o StatusOptions) (Status, error) {
	var st Status
	now := s.o.Now()
	err := s.withRead(ctx, func(r *rtx) error {
		st = Status{VerifiedThrough: nyse.VerifiedThrough}
		w := reader(r)
		v, err := readVersions(r.ctx, r.tx)
		if err != nil {
			return err
		}
		st.Schema, st.Rules, st.Calendar = v.schema, v.derive, v.calendar
		var only sql.NullInt64
		if o.Account != "" {
			id, err := selectAccount(w, o.Account, false)
			if err != nil {
				return err
			}
			only = sql.NullInt64{Int64: id, Valid: true}
		}
		var ids []int64
		if err := queryEach(w, func(rows *sql.Rows) error {
			var id int64
			err := rows.Scan(&id)
			ids = append(ids, id)
			return err
		}, "SELECT account_id FROM accounts WHERE (?1 IS NULL OR account_id = ?1) AND (used OR ?1 IS NOT NULL) ORDER BY account_id", only); err != nil {
			return err
		}
		for _, id := range ids {
			a, err := accountStatus(w, id, o.ShowAccounts, now)
			if err != nil {
				return err
			}
			st.Accounts = append(st.Accounts, a)
		}
		if err := r.queryRow(`SELECT coalesce(sum(source = 'api' AND counted), 0), coalesce(sum(source = 'file' AND counted), 0),
			coalesce(sum(counted_why = 'linked'), 0) FROM entries WHERE ?1 IS NULL OR account_id = ?1`, only).Scan(&st.FromAPI, &st.FromFiles, &st.Linked); err != nil {
			return err
		}
		if err := countWaiting(w, only, &st); err != nil {
			return err
		}
		if st.ItemIssueBoth, st.ItemIssueAgree, err = itemIssueAgreement(w, only); err != nil {
			return err
		}
		st.Warnings, err = numberWarnings(w, only)
		return err
	})
	return st, err
}

// countWaiting counts into st what waits for you, of the account, or of
// every account when it is not valid.
func countWaiting(w *wtx, only sql.NullInt64, st *Status) error {
	if err := w.tx.QueryRowContext(w.ctx, `SELECT coalesce(sum(counted_why = 'uncertain'), 0), coalesce(sum(counted_why = 'held_out'), 0)
		FROM entries WHERE ?1 IS NULL OR account_id = ?1`, only).Scan(&st.Uncertain, &st.HeldOut); err != nil {
		return err
	}
	if err := w.tx.QueryRowContext(w.ctx, `SELECT count(*) FROM flags WHERE severity = 'attention' AND code NOT IN (?2, ?3, ?4, ?5)
		AND (?1 IS NULL OR account_id = ?1)`, only, attentionOther[0], attentionOther[1], attentionOther[2], attentionOther[3]).Scan(&st.Attention); err != nil {
		return err
	}
	return w.tx.QueryRowContext(w.ctx, `SELECT count(DISTINCT subject_ref) FROM flags WHERE code IN ('looks_filtered', 'lacks_rows')
		AND (?1 IS NULL OR account_id = ?1)`, only).Scan(&st.Filtered)
}

// accountStatus returns the status of account id, its full number too when
// show, as of now.
func accountStatus(w *wtx, id int64, show bool, now time.Time) (AccountStatus, error) {
	a := AccountStatus{ID: id}
	var err error
	if a.Label, err = accountLabel(w, id); err != nil {
		return a, err
	}
	if show {
		if err := w.tx.QueryRowContext(w.ctx, "SELECT number FROM account WHERE account_id = ?", id).Scan(&a.Number); err != nil {
			return a, err
		}
	}
	span := func(sources ...string) (Span, error) {
		var sp Span
		var from, to sql.NullString
		err := w.tx.QueryRowContext(w.ctx, `SELECT min(from_date), max(to_date), count(*) FROM coverages
			WHERE account_id = ? AND source IN (SELECT value FROM json_each(?))`, id, jsonList(sources)).Scan(&from, &to, &sp.Runs)
		sp.From, sp.To = nytime.Date(from.String), nytime.Date(to.String)
		return sp, err
	}
	if a.API, err = span("api", "api_file"); err != nil {
		return a, err
	}
	if a.Files, err = span("file", "file_unknown", "file_filtered"); err != nil {
		return a, err
	}
	if a.Snapshots, err = span("snapshot"); err != nil {
		return a, err
	}
	var last sql.NullString
	if err := w.tx.QueryRowContext(w.ctx, `SELECT (SELECT count(*) FROM snapshot_gaps WHERE account_id = ?1),
		(SELECT max(market_date) FROM snapshot_fields WHERE account_id = ?1 AND in_session_date IS NULL)`, id).Scan(&a.Gaps, &last); err != nil {
		return a, err
	}
	a.Since, err = sessionsSince(nytime.Date(last.String), now)
	return a, err
}

// sessionsSince counts the sessions after the date last that have closed by
// now: none when last is "".
func sessionsSince(last nytime.Date, now time.Time) (int, error) {
	if last == "" {
		return 0, nil
	}
	closed, err := nyse.LastClosed(now)
	if errors.Is(err, nyse.ErrBeforeFirst) || err == nil && closed.Date <= last {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	day, err := time.Parse(time.DateOnly, string(last))
	if err != nil {
		return 0, err
	}
	sessions, err := nyse.Between(nytime.Date(day.AddDate(0, 0, 1).Format(time.DateOnly)), closed.Date)
	return len(sessions), err
}

// itemIssueAgreement counts the securities with an instrumentId whose
// History rows carry an ItemIssueId, and those of them whose rows'
// ItemIssueIds are each one of the security's instrumentIds, of the
// account, or of every account when it is not valid.
func itemIssueAgreement(w *wtx, account sql.NullInt64) (both, agree int, err error) {
	err = w.tx.QueryRowContext(w.ctx, `WITH pairs AS (
			SELECT DISTINCT e.security_id, r.item_issue_id FROM entry e JOIN csv_member m ON m.csv_txn_id = e.csv_txn_id
			JOIN history_row r ON r.row_id = m.row_id
			WHERE e.security_id IS NOT NULL AND r.item_issue_id IS NOT NULL AND r.item_issue_id NOT IN ('', '0')
				AND (?1 IS NULL OR e.account_id = ?1)
				AND EXISTS (SELECT 1 FROM instrument_key k WHERE k.security_id = e.security_id AND k.key LIKE 'i:%')),
		judged AS (SELECT security_id, min(EXISTS (SELECT 1 FROM instrument_key k
				WHERE k.security_id = pairs.security_id AND k.key = 'i:' || pairs.item_issue_id)) AS agrees
			FROM pairs GROUP BY security_id)
		SELECT count(*), coalesce(sum(agrees), 0) FROM judged`, account).Scan(&both, &agree)
	return both, agree, err
}

// numberWarnings warns of each stored account number, of the account or
// of every account when it is not valid, that masking by pattern would not
// find, as Check does.
func numberWarnings(w *wtx, account sql.NullInt64) ([]string, error) {
	var out []string
	err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		var number string
		if err := rows.Scan(&id, &number); err != nil {
			return err
		}
		if msg := numberWarning(id, number); msg != "" {
			out = append(out, msg)
		}
		return nil
	}, "SELECT account_id, number FROM account WHERE ?1 IS NULL OR account_id = ?1 ORDER BY account_id", account)
	return out, err
}

// A ReviewGroup is a kind of thing that waits for you, as `schwabtx status
// -review` lists them.
type ReviewGroup struct {
	// Title says what they are, and Ask what to decide.
	Title, Ask string
	// Items are the things.
	Items []ReviewItem
	// Lines say more of the group as a whole, and Commands settle the
	// whole group at once.
	Lines, Commands []string
}

// A ReviewItem is one thing that waits for you.
type ReviewItem struct {
	// Ref names it, and Account names its account, "A1 ...398".
	Ref, Account string
	// Fields say what it is: its day, its kind, symbol, quantity and
	// price, its amount, as the group has them.
	Fields []string
	// Lines say more: a flag's detail, the API transactions it may be.
	Lines []string
	// Commands settle it.
	Commands []string
}

// ReviewOptions says what Review lists.
type ReviewOptions struct {
	// Account selects one account (ImportOptions.Account); "" means every
	// account.
	Account string
	// All lists the flags of info severity too.
	All bool
	// Quote quotes a word of the commands it gives, as the shell that runs
	// them needs. Nil means as a POSIX shell needs, as the commands in
	// flags' details are.
	Quote func(string) string
}

// Review returns what waits for you, grouped, each with the commands that
// settle it: uncertain matches with their candidates; held-out file
// transactions by action; kinds the probes have not settled; each other
// flag that needs you, by code (legs that do not add up, gone rows, stale
// decisions, symbols that nothing resolves, files that may be filtered or
// capped, and the rest); snapshot gaps and the sessions since the last
// snapshot; and Schwab's revisions by the paths they changed. With All,
// the flags of info severity too.
func (s *Store) Review(ctx context.Context, o ReviewOptions) ([]ReviewGroup, error) {
	var groups []ReviewGroup
	now := s.o.Now()
	err := s.withRead(ctx, func(r *rtx) error {
		groups = nil
		w := reader(r)
		q := o.Quote
		if q == nil {
			q = shellQuote
		}
		var only sql.NullInt64
		if o.Account != "" {
			id, err := selectAccount(w, o.Account, false)
			if err != nil {
				return err
			}
			only = sql.NullInt64{Int64: id, Valid: true}
		}
		add := func(g ReviewGroup, err error) error {
			if err == nil && (len(g.Items) > 0 || len(g.Lines) > 0) {
				groups = append(groups, g)
			}
			return err
		}
		if err := add(reviewUncertain(w, only, q)); err != nil {
			return err
		}
		held, err := reviewHeldOut(w, only, q)
		if err != nil {
			return err
		}
		groups = append(groups, held...)
		for _, d := range flagDefs {
			if d.severity == info && !o.All || slices.Contains([]string{"uncertain_match", "held_out"}, d.code) {
				continue
			}
			if err := add(reviewFlag(w, only, d, q)); err != nil {
				return err
			}
		}
		if err := add(reviewGaps(w, only, now)); err != nil {
			return err
		}
		return add(reviewRevisions(w, only))
	})
	return groups, err
}

// entryItem returns the review item of the entry ref of account acct: its
// day, kind, symbol, quantity and price, and amount.
func entryItem(w *wtx, acct int64, ref string) (ReviewItem, error) {
	it := ReviewItem{Ref: ref}
	var day, kind, symbol sql.NullString
	var amount, quantity sql.NullInt64
	var price sql.NullFloat64
	var masked string
	if err := w.tx.QueryRowContext(w.ctx, `SELECT account, day, kind, symbol, quantity_micros, price, amount_cents FROM entries
		WHERE account_id = ? AND ref = ?`, acct, ref).Scan(&masked, &day, &kind, &symbol, &quantity, &price, &amount); err != nil {
		return it, err
	}
	it.Account = fmt.Sprintf("A%d %s", acct, masked)
	what := kind.String
	if symbol.String != "" {
		what += "  " + symbol.String
	}
	if quantity.Valid {
		what += "  " + formatMicros(quantity.Int64)
		if price.Valid {
			what += " @ " + strconv.FormatFloat(price.Float64, 'f', -1, 64)
		}
	}
	amt := "-"
	if amount.Valid {
		amt = formatCents(amount.Int64)
	}
	it.Fields = []string{day.String, what, amt}
	return it, nil
}

// reviewUncertain returns the file transactions that several API
// transactions could be, each with them and their scores.
func reviewUncertain(w *wtx, only sql.NullInt64, q func(string) string) (ReviewGroup, error) {
	g := ReviewGroup{Title: "Uncertain matches", Ask: "which API transaction is it?"}
	type txn struct {
		acct int64
		ref  string
	}
	var txns []txn
	if err := queryEach(w, func(rows *sql.Rows) error {
		var t txn
		err := rows.Scan(&t.acct, &t.ref)
		txns = append(txns, t)
		return err
	}, `SELECT account_id, ref FROM entries WHERE counted_why = 'uncertain' AND (?1 IS NULL OR account_id = ?1)
		ORDER BY day, account_id, ref`, only); err != nil {
		return g, err
	}
	for _, t := range txns {
		it, err := entryItem(w, t.acct, t.ref)
		if err != nil {
			return g, err
		}
		var best string
		if err := queryEach(w, func(rows *sql.Rows) error {
			var ref, why string
			var day, tm sql.NullString
			var amount sql.NullInt64
			var score float64
			if err := rows.Scan(&ref, &day, &tm, &amount, &score, &why); err != nil {
				return err
			}
			if best == "" {
				best = ref
			}
			when := day.String
			if at, err := parseNullInstant(tm); err == nil && !at.IsZero() {
				when = at.In(nytime.NewYork()).Format("2006-01-02 15:04")
			}
			amt := "-"
			if amount.Valid {
				amt = formatCents(amount.Int64)
			}
			it.Lines = append(it.Lines, fmt.Sprintf("%s  %s  %s  score %s: %s", ref, when, amt, strconv.FormatFloat(score, 'f', -1, 64), why))
			return nil
		}, `SELECT a.ref, a.day, a.time, a.amount_cents, c.score, c.why FROM entry e JOIN match_candidate c ON c.csv_txn_id = e.csv_txn_id
			JOIN entry a ON a.txn_id = c.txn_id WHERE e.account_id = ? AND e.ref = ? ORDER BY c.score DESC, a.ref`, t.acct, t.ref); err != nil {
			return g, err
		}
		if best != "" {
			it.Commands = append(it.Commands, "schwabtx edit "+q(t.ref)+" -link "+q(best))
		}
		it.Commands = append(it.Commands, "schwabtx edit "+q(t.ref)+" -link none")
		g.Items = append(g.Items, it)
	}
	return g, nil
}

// reviewHeldOut returns the file transactions that the API's dates hold
// and no API transaction is, a group for each action.
func reviewHeldOut(w *wtx, only sql.NullInt64, q func(string) string) ([]ReviewGroup, error) {
	type txn struct {
		acct        int64
		ref, action string
		detail      sql.NullString
	}
	var txns []txn
	if err := queryEach(w, func(rows *sql.Rows) error {
		var t txn
		err := rows.Scan(&t.acct, &t.ref, &t.action, &t.detail)
		txns = append(txns, t)
		return err
	}, `SELECT e.account_id, e.ref, coalesce(e.kind, ''), (SELECT f.detail FROM flags f WHERE f.code = 'held_out'
			AND f.subject_ref = e.ref AND f.account_id = e.account_id)
		FROM entries e WHERE e.counted_why = 'held_out' AND (?1 IS NULL OR e.account_id = ?1)
		ORDER BY e.kind, e.day, e.account_id, e.ref`, only); err != nil {
		return nil, err
	}
	var groups []ReviewGroup
	for _, t := range txns {
		if len(groups) == 0 || groups[len(groups)-1].Title != "Held out: "+t.action {
			groups = append(groups, ReviewGroup{
				Title: "Held out: " + t.action,
				Ask:   "no API transaction is these, though the API's dates hold them; not counted until you say",
				Commands: []string{
					"if the API never shows such rows: schwabtx edit -action " + q(t.action) + " -policy count",
				},
			})
		}
		it, err := entryItem(w, t.acct, t.ref)
		if err != nil {
			return nil, err
		}
		if t.detail.String == "api_file" {
			it.Lines = append(it.Lines, "api_file: the API's dates here are those of a schwabtx transactions -json file")
		}
		it.Commands = []string{"if no API row is it: schwabtx edit " + q(t.ref) + " -link none"}
		g := &groups[len(groups)-1]
		g.Items = append(g.Items, it)
	}
	return groups, nil
}

// flagCommands are the commands that settle a flag on an entry, beside any
// its detail gives, with REF for the entry's ref.
var flagCommands = map[string][]string{
	"awaits_probe":   {"schwabtx edit REF -category CATEGORY"},
	"gone":           {"if it still counts: schwabtx edit REF -count yes"},
	"stale_decision": {"decide again: schwabtx edit REF ..."},
	"status":         {"if it counts: schwabtx edit REF -count yes"},
	"unknown_type":   {"schwabtx edit REF -category CATEGORY"},
	"unknown_action": {"schwabtx edit REF -category CATEGORY"},
	"zero_amount":    {"schwabtx edit REF -category CATEGORY"},
}

// reviewFlag returns the things flagged with d's code.
func reviewFlag(w *wtx, only sql.NullInt64, d flagDef, q func(string) string) (ReviewGroup, error) {
	g := ReviewGroup{Title: d.code, Ask: d.meaning}
	type flagged struct {
		ref    string
		acct   sql.NullInt64
		detail sql.NullString
	}
	var fs []flagged
	if err := queryEach(w, func(rows *sql.Rows) error {
		var f flagged
		err := rows.Scan(&f.ref, &f.acct, &f.detail)
		fs = append(fs, f)
		return err
	}, `SELECT subject_ref, account_id, detail FROM flags WHERE code = ?2 AND (?1 IS NULL OR account_id = ?1)
		ORDER BY account_id, subject_ref`, only, d.code); err != nil {
		return g, err
	}
	for _, f := range fs {
		it := ReviewItem{Ref: f.ref}
		if strings.HasPrefix(f.ref, "T") || strings.HasPrefix(f.ref, "C") { // an entry
			var err error
			if it, err = entryItem(w, f.acct.Int64, f.ref); err != nil {
				return g, err
			}
			for _, c := range flagCommands[d.code] {
				it.Commands = append(it.Commands, strings.ReplaceAll(c, "REF", q(f.ref)))
			}
		} else if f.acct.Valid {
			var masked string
			if err := w.tx.QueryRowContext(w.ctx, "SELECT masked FROM account WHERE account_id = ?", f.acct.Int64).Scan(&masked); err != nil {
				return g, err
			}
			it.Account = fmt.Sprintf("A%d %s", f.acct.Int64, masked)
		}
		if strings.HasPrefix(f.ref, "F") {
			var label sql.NullString
			if err := w.tx.QueryRowContext(w.ctx, "SELECT label FROM files WHERE ref = ?", f.ref).Scan(&label); err != nil {
				return g, err
			}
			it.Fields = []string{label.String}
		}
		if f.detail.String != "" {
			it.Lines = append(it.Lines, f.detail.String)
		}
		g.Items = append(g.Items, it)
	}
	return g, nil
}

// reviewGaps returns, for each account, the sessions between its first and
// last snapshot taken after a close that no snapshot stands for, and those
// that have closed, by now, since its last.
func reviewGaps(w *wtx, only sql.NullInt64, now time.Time) (ReviewGroup, error) {
	g := ReviewGroup{Title: "Snapshot gaps", Ask: "market sessions that no snapshot taken after their close stands for"}
	type acct struct {
		id     int64
		masked string
		last   sql.NullString
	}
	var accts []acct
	if err := queryEach(w, func(rows *sql.Rows) error {
		var a acct
		err := rows.Scan(&a.id, &a.masked, &a.last)
		accts = append(accts, a)
		return err
	}, `SELECT a.account_id, a.masked, (SELECT max(market_date) FROM snapshot_fields f WHERE f.account_id = a.account_id
			AND f.in_session_date IS NULL)
		FROM account a WHERE ?1 IS NULL OR a.account_id = ?1 ORDER BY a.account_id`, only); err != nil {
		return g, err
	}
	for _, a := range accts {
		var gaps []string
		if err := queryEach(w, func(rows *sql.Rows) error {
			var d string
			err := rows.Scan(&d)
			gaps = append(gaps, d)
			return err
		}, "SELECT market_date FROM snapshot_gaps WHERE account_id = ? ORDER BY market_date", a.id); err != nil {
			return g, err
		}
		since, err := sessionsSince(nytime.Date(a.last.String), now)
		if err != nil {
			return g, err
		}
		if len(gaps) == 0 && since == 0 {
			continue
		}
		it := ReviewItem{Account: fmt.Sprintf("A%d %s", a.id, a.masked)}
		if len(gaps) > 0 {
			it.Lines = append(it.Lines, fmt.Sprintf("%d %s without a snapshot: %s", len(gaps), plural(len(gaps), "session", "sessions"), strings.Join(gaps, ", ")))
		}
		if since > 0 {
			it.Lines = append(it.Lines, fmt.Sprintf("%d %s closed since the last snapshot, of %s; schwabtx snapshot takes one",
				since, plural(since, "session has", "sessions have"), a.last.String))
		}
		g.Items = append(g.Items, it)
	}
	return g, nil
}

// reviewRevisions returns Schwab's revisions of its records, grouped by
// the key paths they changed.
func reviewRevisions(w *wtx, only sql.NullInt64) (ReviewGroup, error) {
	g := ReviewGroup{Title: "Revisions", Ask: "records Schwab changed after it first sent them, by what changed"}
	err := queryEach(w, func(rows *sql.Rows) error {
		var changed, refs string
		var n int
		if err := rows.Scan(&changed, &n, &refs); err != nil {
			return err
		}
		g.Lines = append(g.Lines, fmt.Sprintf("%s: %d (%s)", changed, n, refs))
		return nil
	}, `SELECT coalesce((SELECT group_concat(value, ', ') FROM json_each(changed)), '?'), count(*),
			(SELECT group_concat(ref, ', ') FROM (SELECT DISTINCT x.ref FROM revisions x
				WHERE coalesce(x.changed, '') = coalesce(r.changed, '') AND x.seq > 1 AND (?1 IS NULL OR x.account_id = ?1)
				ORDER BY x.ref LIMIT 5))
		FROM revisions r WHERE seq > 1 AND (?1 IS NULL OR account_id = ?1) GROUP BY r.changed ORDER BY 1`, only)
	return g, err
}

// A Detail is one thing in full, as `schwabtx status REF` prints it.
type Detail struct {
	Target Target
	// Fields are its fields, each a name and a value: the columns of its
	// view that are not empty, money as dollars and quantities as shares.
	Fields [][2]string
	// Sections are what else the store holds of it.
	Sections []DetailSection
}

// A DetailSection is a part of a Detail: its title and its lines.
type DetailSection struct {
	Title string
	Lines []string
}

// Lookup returns the thing that ref names (Find) in full: an entry's
// fields, flags with their details, revisions with the paths they changed,
// the complete reads it was absent from, its match or candidates, its rows
// and the decisions on it; a file's fields, flags and the transactions it
// holds; a lot's versions; a key's security, keys and names over time; an
// account's coverage and hashes over time; a snapshot's fields and flags.
func (s *Store) Lookup(ctx context.Context, ref, account string) (Detail, error) {
	var d Detail
	err := s.withRead(ctx, func(r *rtx) error {
		w := reader(r)
		t, err := find(w, ref, account)
		if err != nil {
			return err
		}
		d = Detail{Target: t}
		switch t.Kind {
		case TargetTransaction, TargetFileTransaction:
			return lookupEntry(w, &d)
		case TargetFile:
			return lookupFile(w, &d)
		case TargetLot:
			return lookupLot(w, &d)
		case TargetKey:
			return lookupKey(w, &d)
		case TargetAccount:
			return lookupAccount(w, &d)
		}
		return lookupSnapshot(w, &d)
	})
	return d, err
}

// viewFields returns the columns of the one row of view that where picks
// with args, each its name as an export writes it and its value as an
// export writes it, but those that are NULL or empty or that skip names.
func viewFields(w *wtx, view, where string, skip []string, args ...any) ([][2]string, error) {
	rows, err := w.tx.QueryContext(w.ctx, "SELECT * FROM "+view+" WHERE "+where, args...) //nolint:gosec // G202: view and where are this file's constants; values are bound.
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][2]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, c := range cols {
			if vals[i] == nil || slices.Contains(skip, c) {
				continue
			}
			cl, err := exportCell(c, vals[i], false)
			if err != nil {
				return nil, err
			}
			if cl.text == "" {
				continue
			}
			out = append(out, [2]string{exportedColumn(c), cl.text})
		}
	}
	return out, rows.Err()
}

// section appends to d a section of the lines that query returns, each of
// its columns, but those that are NULL, joined by two spaces.
func (d *Detail) section(w *wtx, title, query string, args ...any) error {
	sec := DetailSection{Title: title}
	err := queryEach(w, func(rows *sql.Rows) error {
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		vals := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		var parts []string
		for _, v := range vals {
			if v.Valid && v.String != "" {
				parts = append(parts, v.String)
			}
		}
		sec.Lines = append(sec.Lines, strings.Join(parts, "  "))
		return nil
	}, query, args...)
	if err == nil && len(sec.Lines) > 0 {
		d.Sections = append(d.Sections, sec)
	}
	return err
}

// Free text in the sections' queries is stored masked; money is formatted
// by SQL as an export formats it from whole cents.
const sqlCents = `CASE WHEN %[1]s IS NULL THEN NULL ELSE printf('%%s%%d.%%02d', CASE WHEN %[1]s < 0 THEN '-' ELSE '' END, abs(%[1]s) / 100, abs(%[1]s) %% 100) END`

// sqlMoney returns the SQL that formats the cents column c as dollars.
func sqlMoney(c string) string { return fmt.Sprintf(sqlCents, c) }

// lookupEntry fills d, the detail of an entry.
func lookupEntry(w *wtx, d *Detail) error {
	t := d.Target
	var err error
	if d.Fields, err = viewFields(w, "entries", "account_id = ? AND ref = ?", []string{"ref", "account_id", "account"}, t.AccountID, t.Ref); err != nil {
		return err
	}
	var entryID int64
	var txn, csv sql.NullInt64
	if err := w.tx.QueryRowContext(w.ctx, "SELECT entry_id, txn_id, csv_txn_id FROM entry WHERE account_id = ? AND ref = ?",
		t.AccountID, t.Ref).Scan(&entryID, &txn, &csv); err != nil {
		return err
	}
	if txn.Valid {
		more, err := viewFields(w, "transactions", "account_id = ? AND ref = ?", []string{
			"ref", "account_id", "account", "time", "type", "description", "category", "category_rule", "symbol", "security_key",
			"quantity_micros", "price", "gross_cents", "fees_cents", "counted", "counted_why", "flags", "net_cents",
		}, t.AccountID, t.Ref)
		if err != nil {
			return err
		}
		d.Fields = append(d.Fields, more...)
	}
	if err := d.section(w, "Flags", "SELECT f.code || coalesce(': ' || f.detail, '') FROM flag f WHERE f.entry_id = ? ORDER BY f.code", entryID); err != nil {
		return err
	}
	if txn.Valid {
		if err := d.section(w, "Revisions", `SELECT 'revision ' || seq, 'first seen ' || first_seen_at || ' by ' || first_seen_by,
			'last seen ' || last_seen_at, 'changed ' || (SELECT group_concat(value, ', ') FROM json_each(changed))
			FROM revisions WHERE account_id = ? AND ref = ? ORDER BY seq`, t.AccountID, t.Ref); err != nil {
			return err
		}
		if err := d.section(w, "Absent from complete reads", `SELECT 'read ' || r.read_id, 'answered ' || r.answered_at,
			'window ' || r.window_start || ' to ' || r.window_end FROM api_absent a JOIN api_read r ON r.read_id = a.read_id
			WHERE a.txn_id = ? ORDER BY r.read_id`, txn.Int64); err != nil {
			return err
		}
	}
	if err := d.section(w, "Matched with", `SELECT x.ref, m.method, 'score ' || m.score FROM match_member me
		JOIN match m ON m.match_id = me.match_id JOIN match_member mo ON mo.match_id = m.match_id
		JOIN entry x ON (x.txn_id = mo.txn_id OR x.csv_txn_id = mo.csv_txn_id)
		WHERE (me.txn_id = ?1 OR me.csv_txn_id = ?2) AND x.entry_id <> ?3 ORDER BY x.ref`, txn, csv, entryID); err != nil {
		return err
	}
	subjects := []string{t.Subject}
	if csv.Valid {
		if err := d.section(w, "Candidates", `SELECT a.ref, a.day, `+sqlMoney("a.amount_cents")+`, 'score ' || c.score, c.why
			FROM match_candidate c JOIN entry a ON a.txn_id = c.txn_id WHERE c.csv_txn_id = ? ORDER BY c.score DESC, a.ref`, csv); err != nil {
			return err
		}
		if err := d.section(w, "Rows", `SELECT (SELECT ref FROM file_fields WHERE file_id = r.file_id) || ' row ' || r.ordinal,
			'line ' || r.line, r.posted_date, r.action, r.symbol, r.description, r.quantity_text, r.price_text, `+sqlMoney("r.fees_cents")+`,
			`+sqlMoney("r.amount_cents")+`, CASE WHEN r.row_id = c.shown_row_id THEN '(shown)' END
			FROM csv_member m JOIN history_row r ON r.row_id = m.row_id JOIN csv_txn c ON c.csv_txn_id = m.csv_txn_id
			WHERE m.csv_txn_id = ? ORDER BY r.file_id, r.ordinal`, csv); err != nil {
			return err
		}
		if err := queryEach(w, func(rows *sql.Rows) error {
			var s string
			err := rows.Scan(&s)
			if !slices.Contains(subjects, s) {
				subjects = append(subjects, s)
			}
			return err
		}, `SELECT 'c:' || lower(hex(f.sha256)) || ':' || r.ordinal FROM csv_member m JOIN history_row r ON r.row_id = m.row_id
			JOIN import_file f ON f.file_id = r.file_id WHERE m.csv_txn_id = ? ORDER BY f.file_id, r.ordinal`, csv); err != nil {
			return err
		}
	}
	return d.section(w, "Your decisions", `SELECT made_at, kind, value, note FROM decision
		WHERE subject IN (SELECT value FROM json_each(?1)) OR (kind IN ('match', 'reject', 'pair')
			AND EXISTS (SELECT 1 FROM json_each(?1) s WHERE (',' || decision.value || ',') LIKE '%,' || s.value || ',%'))
		ORDER BY decision_id`, jsonList(subjects))
}

// lookupFile fills d, the detail of a file.
func lookupFile(w *wtx, d *Detail) error {
	var err error
	if d.Fields, err = viewFields(w, "files", "ref = ?", []string{"ref", "account_id", "account"}, d.Target.Ref); err != nil {
		return err
	}
	var id int64
	if err := w.tx.QueryRowContext(w.ctx, "SELECT file_id FROM file_fields WHERE ref = ?", d.Target.Ref).Scan(&id); err != nil {
		return err
	}
	if err := d.section(w, "Flags", "SELECT code || coalesce(': ' || detail, '') FROM flag WHERE file_id = ? ORDER BY code", id); err != nil {
		return err
	}
	if err := d.section(w, "Coverage of its account", `SELECT source, from_date || ' to ' || to_date FROM coverage
		WHERE account_id = (SELECT account_id FROM file_fields WHERE file_id = ?) ORDER BY source, from_date`, id); err != nil {
		return err
	}
	if err := d.section(w, "Its transactions, by why each counts or not", `SELECT e.counted_why || ': ' || count(DISTINCT e.entry_id) FROM history_row r
		JOIN csv_member m ON m.row_id = r.row_id JOIN entry e ON e.csv_txn_id = m.csv_txn_id WHERE r.file_id = ?
		GROUP BY e.counted_why ORDER BY e.counted_why`, id); err != nil {
		return err
	}
	return d.section(w, "Your decisions", "SELECT made_at, kind, value, note FROM decision WHERE subject = ? ORDER BY decision_id", d.Target.Subject)
}

// lookupLot fills d, the detail of a lot.
func lookupLot(w *wtx, d *Detail) error {
	var err error
	if d.Fields, err = viewFields(w, "realized_lots", "account_id = ? AND ref = ?", []string{"ref", "account_id", "account"}, d.Target.AccountID, d.Target.Ref); err != nil {
		return err
	}
	if err := d.section(w, "Flags", `SELECT f.code || coalesce(': ' || f.detail, '') FROM flag f JOIN realized_lot l ON l.lot_id = f.lot_id
		WHERE l.subject = ? ORDER BY f.code`, d.Target.Subject); err != nil {
		return err
	}
	return d.section(w, "Versions in the files", `SELECT (SELECT ref FROM file_fields WHERE file_id = r.file_id) || ' row ' || r.ordinal,
		r.closed_date, r.opened_text, r.quantity_text, 'proceeds ' || `+sqlMoney("r.proceeds_cents")+`, 'cost ' || `+sqlMoney("r.cost_basis_cents")+`,
		'gain ' || `+sqlMoney("r.gain_cents")+`, CASE WHEN r.row_id = l.shown_row_id THEN '(shown)' END
		FROM realized_lot l JOIN lot_member m ON m.lot_id = l.lot_id JOIN realized_row r ON r.row_id = m.row_id
		WHERE l.subject = ? ORDER BY r.file_id, r.ordinal`, d.Target.Subject)
}

// lookupKey fills d, the detail of an instrument key: its security.
func lookupKey(w *wtx, d *Detail) error {
	key := strings.TrimPrefix(d.Target.Subject, "k:")
	var err error
	if d.Fields, err = viewFields(w, "securities", "security_key = (SELECT security_key FROM security_keys WHERE key = ?)", nil, key); err != nil {
		return err
	}
	if err := d.section(w, "Its keys", `SELECT 'K:' || key FROM security_keys
		WHERE security_key = (SELECT security_key FROM security_keys WHERE key = ?) ORDER BY key`, key); err != nil {
		return err
	}
	if err := d.section(w, "Names over time", `SELECT kind, value, 'in ' || seen_in, first_seen_at || ' to ' || last_seen_at FROM security_names
		WHERE security_key = (SELECT security_key FROM security_keys WHERE key = ?) ORDER BY first_seen_at, kind, value`, key); err != nil {
		return err
	}
	return d.section(w, "Your decisions", "SELECT made_at, kind, value, note FROM decision WHERE subject = ?1 OR value LIKE '% ' || ?1 ORDER BY decision_id",
		d.Target.Subject)
}

// lookupAccount fills d, the detail of an account.
func lookupAccount(w *wtx, d *Detail) error {
	var err error
	if d.Fields, err = viewFields(w, "accounts", "account_id = ?", []string{"account_id", "ref"}, d.Target.AccountID); err != nil {
		return err
	}
	if err := d.section(w, "Coverage", "SELECT source, from_date || ' to ' || to_date FROM coverages WHERE account_id = ? ORDER BY source, from_date",
		d.Target.AccountID); err != nil {
		return err
	}
	if err := d.section(w, "Hashes over time", "SELECT hash, first_seen_at || ' to ' || last_seen_at FROM account_hash WHERE account_id = ? ORDER BY first_seen_at, hash",
		d.Target.AccountID); err != nil {
		return err
	}
	return d.section(w, "Your decisions", "SELECT made_at, kind, value, note FROM decision WHERE subject = ? ORDER BY decision_id", d.Target.Subject)
}

// lookupSnapshot fills d, the detail of a snapshot.
func lookupSnapshot(w *wtx, d *Detail) error {
	id := strings.TrimPrefix(d.Target.Ref, "S")
	var err error
	if d.Fields, err = viewFields(w, "snapshots", "snapshot_id = ?", []string{"snapshot_id", "ref", "account_id", "account"}, id); err != nil {
		return err
	}
	return d.section(w, "Flags", "SELECT code || coalesce(': ' || detail, '') FROM flag WHERE snapshot_id = ? ORDER BY code", id)
}
