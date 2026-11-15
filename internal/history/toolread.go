package history

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// This file reads what a tool export maps (2 of the design): its queries,
// Q0 to Q10, Q12 and QL, run in one read transaction into a toolInput.

// A toolQuery is one of the reads of a tool export (2 of the design): its
// name there, Q0 to Q10, Q12 or QL; its SQL and its arguments; and scan,
// which reads the current row of rows, a row of the query, into in.
type toolQuery struct {
	name  string
	query string
	args  []any
	scan  func(rows *sql.Rows, in *toolInput) error
}

// toolQueries returns the reads of a tool export that selects the account
// of id account, 0 for every account, and the rows up to the day to, ""
// for no bound (2 of the design): Q0 to Q10, Q12 and QL, in that order. The
// account bounds the ledger rows, their legs, the snapshots, the rows that
// wait for you, the coverage and the uncounted entries (Q2, Q3 and Q7 to
// Q10); to bounds the ledger rows, their legs, the positions and the rows
// that wait for you (Q2, Q3, Q6 and Q8). The accounts, the securities,
// their names, the days of the rows under each of their symbols (Q12) and
// the links of rows of two of them (QL) are the store's, and the positions
// every account's, since a transfer names the other account, a price is
// the market's, and every export takes a security for the same as every
// other, whatever it selects; the snapshots and the uncounted entries go
// past to, since Ghostfolio takes an account's balances only in the import
// that makes it and a row that a tool holds may have stopped counting on
// any day. Every query reads views, but Q0, which reads when the store was
// made: no account number, raw JSON or file's content.
func toolQueries(account int64, to nytime.Date) []toolQuery {
	// bounds returns the WHERE clause of conds and of the selection's
	// bounds: on the account column acct and by the condition day, which
	// holds one placeholder for to; each "" when it does not bound the
	// query. It returns the bounds' arguments too.
	bounds := func(acct, day string, conds ...string) (string, []any) {
		var args []any
		if acct != "" && account != 0 {
			conds = append(conds, acct+" = ?")
			args = append(args, account)
		}
		if day != "" && to != "" {
			conds = append(conds, day)
			args = append(args, string(to))
		}
		if len(conds) == 0 {
			return "", nil
		}
		return " WHERE " + strings.Join(conds, " AND "), args
	}
	// A row without a day is read whatever to says, to be left out with a
	// reason; its legs are not.
	ledger, ledgerArgs := bounds("v.account_id", "(v.day <= ? OR v.day IS NULL)", "v.counted = 1")
	legs, legsArgs := bounds("v.account_id", "v.day <= ?", "l.source = 'api'", "l.asset_type IS NOT 'CURRENCY'",
		"v.quantity_micros IS NULL", "v.category IN ('buy', 'sell', 'transfer_in', 'transfer_out', 'journal')")
	positions, positionsArgs := bounds("", "market_date <= ?",
		"in_session_date IS NULL", "market_date IS NOT NULL", "security_key IS NOT NULL")
	snapshots, snapshotsArgs := bounds("account_id", "", "in_session_date IS NULL", "market_date IS NOT NULL")
	waiting, waitingArgs := bounds("account_id", "day <= ?", "source = 'file'", "counted_why IN ('held_out', 'uncertain')")
	uncounted, uncountedArgs := bounds("account_id", "", "counted = 0")
	coverage := sql.NullInt64{Int64: account, Valid: account != 0}
	return []toolQuery{
		{name: "Q0", query: "SELECT created_at FROM store WHERE id = 1", scan: scanToolStore},
		{name: "Q1", query: "SELECT account_id, account, nickname FROM accounts ORDER BY account_id", scan: scanToolAccount},
		{
			name: "Q2",
			// The ledger view is the entries that count, but for a file
			// row's posted date, which entries holds. The partner may be of
			// any account and need not count.
			query: `SELECT v.ref, v.account_id, v.source, v.day, v.posted_date, v.time, v.kind, v.description,
				v.amount_cents, v.fees_cents, v.gross_cents, v.quantity_micros, v.quantity_as_of, v.price, v.symbol,
				v.security_key, v.category, v.pair_kind, v.pair_ref, v.pair_account_id, v.flags,
				p.day, p.time, p.amount_cents, p.price, p.counted, p.category, p.kind, p.description
				FROM entries v LEFT JOIN entries p ON p.account_id = v.pair_account_id AND p.ref = v.pair_ref` + ledger +
				` ORDER BY v.day, v.time, v.account_id, v.ref`,
			args: ledgerArgs, scan: scanToolRow,
		},
		{
			name: "Q3",
			query: `SELECT l.account_id, l.ref, l.leg, l.symbol, l.security_key, l.quantity_micros, l.cost_cents, l.price
				FROM legs l JOIN ledger v ON v.account_id = l.account_id AND v.ref = l.ref` + legs +
				` ORDER BY l.account_id, l.ref, l.leg`,
			args: legsArgs, scan: scanToolLeg,
		},
		{
			name: "Q4",
			query: `SELECT security_key, asset_type, type, symbol, cusip, description, put_call, strike, expiration_day,
				multiplier, underlying_symbol FROM securities ORDER BY security_key`,
			scan: scanToolSecurity,
		},
		{
			name: "Q5",
			// A name that two keys of a security had is a row of each, with
			// the key that had it: a History file's, which the store dates by
			// the file's export, or another.
			query: `SELECT security_key, key, kind, value, first_seen_at, last_seen_at FROM security_names
				WHERE kind IN ('symbol', 'description') ORDER BY security_key, kind, last_seen_at DESC, value, first_seen_at, key`,
			scan: scanToolName,
		},
		{
			name: "Q6",
			query: `SELECT security_key, account_id, market_date, fetched_at, long_micros, short_micros, market_value
				FROM positions` + positions + ` ORDER BY security_key, market_date, fetched_at DESC, account_id, snapshot_id, position`,
			args: positionsArgs, scan: scanToolPosition,
		},
		{
			name: "Q7",
			// A margin account's cash balance can read 0 with a debit or short
			// positions, its cash in the margin and short balances, as sent.
			query: `SELECT account_id, market_date, fetched_at, cash_cents, account_type,
				(SELECT b.value_as_sent FROM balances b WHERE b.snapshot_id = s.snapshot_id AND b.balance_set = 'current' AND b.figure = 'marginBalance'),
				(SELECT b.value_as_sent FROM balances b WHERE b.snapshot_id = s.snapshot_id AND b.balance_set = 'current' AND b.figure = 'shortBalance')
				FROM snapshots s` + snapshots + ` ORDER BY account_id, market_date, fetched_at DESC, snapshot_id`,
			args: snapshotsArgs, scan: scanToolSnapshot,
		},
		{
			name: "Q8",
			// The rows that Export.Waiting counts: held out and uncertain
			// file rows, which do not count.
			query: `SELECT ref, account_id, day, kind, amount_cents, counted_why FROM entries` + waiting +
				` ORDER BY day, account_id, ref`,
			args: waitingArgs, scan: scanToolWaiting,
		},
		{name: "Q9", query: coverageQuery, args: []any{coverage}, scan: scanToolCoverage},
		{
			name: "Q10",
			query: `SELECT ref, account_id, counted_why, matched_with FROM entries` + uncounted +
				` ORDER BY account_id, ref`,
			args: uncountedArgs, scan: scanToolUncounted,
		},
		{
			name: "Q12",
			// The days of every entry, counted or not, and of every API leg of
			// a security, under each symbol of each security, "" for a row of
			// none, of each source: the store dates a name by the read or the
			// file that brought it, which may hold years of rows; the API's
			// rows' days say when a security went by a symbol, and which of two
			// symbols of one read came last, while a History file names its
			// rows by the ticker of its newest export, whatever key the store
			// gives them.
			query: `SELECT coalesce(security_key, ''), symbol, source, min(day), max(day) FROM (
				SELECT security_key, symbol, source, day FROM entries
				UNION ALL
				SELECT security_key, symbol, source, day FROM legs WHERE source = 'api' AND asset_type IS NOT 'CURRENCY')
				WHERE symbol IS NOT NULL AND symbol <> '' AND day IS NOT NULL
				GROUP BY 1, 2, 3 ORDER BY 1, 2, 3`,
			scan: scanToolSymbolDays,
		},
		{
			name: "QL",
			// The keys of the two rows of each link that the store made, a
			// History file's row and the API's row that it counts as one
			// transaction, when they are of two of its securities.
			query: `SELECT DISTINCT f.security_key, a.security_key FROM matches m
				JOIN entries f ON f.account_id = m.account_id AND f.ref = m.file_ref
				JOIN entries a ON a.account_id = m.account_id AND a.ref = m.api_ref
				WHERE f.security_key IS NOT NULL AND a.security_key IS NOT NULL AND f.security_key <> a.security_key
				ORDER BY 1, 2`,
			scan: scanToolLink,
		},
	}
}

// readToolInput runs the reads of a tool export (toolQueries) of the
// account of id account, 0 for every account, up to the day to, "" for no
// bound, in the read transaction r, so that they agree with one another,
// and returns what they read as a toolInput of that account and to. The
// tool and the symbols that -manual names, which no read depends on, are
// the caller's to set.
func readToolInput(r *rtx, account int64, to nytime.Date) (*toolInput, error) {
	in := &toolInput{account: account, to: to}
	for _, q := range toolQueries(account, to) {
		if err := q.read(r, in); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// read runs the query in r and reads its rows into in.
func (q toolQuery) read(r *rtx, in *toolInput) error {
	rows, err := r.query(q.query, q.args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := q.scan(rows, in); err != nil {
			return err
		}
	}
	return rows.Err()
}

// toolInstant returns the instant s that the store wrote, the zero time for
// NULL; what says what holds it, for the error of one that is not an
// instant.
func toolInstant(s sql.NullString, what string) (time.Time, error) {
	if !s.Valid {
		return time.Time{}, nil
	}
	t, err := parseInstant(s.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s holds %q, not an instant (please report it)", what, s.String)
	}
	return t, nil
}

// scanToolStore reads Q0's row: when the store was made.
func scanToolStore(rows *sql.Rows, in *toolInput) error {
	return rows.Scan(&in.store)
}

// scanToolAccount reads a row of Q1: an account.
func scanToolAccount(rows *sql.Rows, in *toolInput) error {
	var a toolAccount
	var nickname sql.NullString
	if err := rows.Scan(&a.id, &a.masked, &nickname); err != nil {
		return err
	}
	a.nickname = nickname.String
	in.accounts = append(in.accounts, a)
	return nil
}

// scanToolRow reads a row of Q2: a ledger row and its partner's columns.
func scanToolRow(rows *sql.Rows, in *toolInput) error {
	var r toolRow
	var day, posted, at, kind, description, asOf, symbol, key, pairKind, pairRef, flags sql.NullString
	var pairAccount sql.NullInt64
	var pDay, pAt, pCategory, pKind, pDescription sql.NullString
	var pCounted sql.NullBool
	if err := rows.Scan(&r.ref, &r.account, &r.source, &day, &posted, &at, &kind, &description,
		&r.amount, &r.fees, &r.gross, &r.quantity, &asOf, &r.price, &symbol,
		&key, &r.category, &pairKind, &pairRef, &pairAccount, &flags,
		&pDay, &pAt, &r.partner.amount, &r.partner.price, &pCounted, &pCategory, &pKind, &pDescription); err != nil {
		return err
	}
	var err error
	if r.time, err = toolInstant(at, "the time of "+r.ref); err != nil {
		return err
	}
	if r.partner.time, err = toolInstant(pAt, "the time of "+pairRef.String); err != nil {
		return err
	}
	r.day, r.posted, r.quantityAsOf = nytime.Date(day.String), nytime.Date(posted.String), nytime.Date(asOf.String)
	r.kind, r.description, r.symbol, r.securityKey = kind.String, description.String, symbol.String, key.String
	r.pairKind, r.pairRef, r.pairAccount = pairKind.String, pairRef.String, pairAccount.Int64
	if flags.String != "" {
		r.flags = strings.Split(flags.String, ",")
	}
	r.partner.day, r.partner.counted, r.partner.category = nytime.Date(pDay.String), pCounted.Bool, pCategory.String
	r.partner.kind, r.partner.description = pKind.String, pDescription.String
	in.rows = append(in.rows, r)
	return nil
}

// scanToolLeg reads a row of Q3: a security leg of a ledger row.
func scanToolLeg(rows *sql.Rows, in *toolInput) error {
	var l toolLeg
	var symbol, key sql.NullString
	if err := rows.Scan(&l.account, &l.ref, &l.leg, &symbol, &key, &l.quantity, &l.cost, &l.price); err != nil {
		return err
	}
	l.symbol, l.securityKey = symbol.String, key.String
	in.legs = append(in.legs, l)
	return nil
}

// scanToolSecurity reads a row of Q4: a security.
func scanToolSecurity(rows *sql.Rows, in *toolInput) error {
	var s toolSecurityRow
	var asset, typ, symbol, cusip, description, putCall, expiration, underlying sql.NullString
	if err := rows.Scan(&s.key, &asset, &typ, &symbol, &cusip, &description, &putCall, &s.strike, &expiration,
		&s.multiplier, &underlying); err != nil {
		return err
	}
	s.assetType, s.typ, s.symbol, s.cusip, s.description = asset.String, typ.String, symbol.String, cusip.String, description.String
	s.putCall, s.expiration, s.underlying = putCall.String, nytime.Date(expiration.String), underlying.String
	in.securities = append(in.securities, s)
	return nil
}

// scanToolName reads a row of Q5: a symbol or a description of a security,
// and the key that had it, when it is not the security's own.
func scanToolName(rows *sql.Rows, in *toolInput) error {
	var n toolName
	var first, last sql.NullString
	if err := rows.Scan(&n.key, &n.from, &n.kind, &n.value, &first, &last); err != nil {
		return err
	}
	if n.from == n.key {
		n.from = ""
	}
	var err error
	if n.firstSeen, err = toolInstant(first, "when "+n.key+" was first seen"); err != nil {
		return err
	}
	if n.lastSeen, err = toolInstant(last, "when "+n.key+" was last seen"); err != nil {
		return err
	}
	in.names = append(in.names, n)
	return nil
}

// scanToolPosition reads a row of Q6: a position of a closed-phase
// snapshot.
func scanToolPosition(rows *sql.Rows, in *toolInput) error {
	var p toolPosition
	var date string
	var fetched sql.NullString
	if err := rows.Scan(&p.key, &p.account, &date, &fetched, &p.long, &p.short, &p.marketValue); err != nil {
		return err
	}
	var err error
	if p.fetchedAt, err = toolInstant(fetched, "when a snapshot of "+date+" was taken"); err != nil {
		return err
	}
	p.marketDate = nytime.Date(date)
	in.positions = append(in.positions, p)
	return nil
}

// scanToolSnapshot reads a row of Q7: a closed-phase snapshot of a
// selected account.
func scanToolSnapshot(rows *sql.Rows, in *toolInput) error {
	var s toolSnapshot
	var date string
	var fetched, accountType sql.NullString
	if err := rows.Scan(&s.account, &date, &fetched, &s.cash, &accountType, &s.margin, &s.short); err != nil {
		return err
	}
	s.accountType = accountType.String
	var err error
	if s.fetchedAt, err = toolInstant(fetched, "when a snapshot of "+date+" was taken"); err != nil {
		return err
	}
	s.marketDate = nytime.Date(date)
	in.snapshots = append(in.snapshots, s)
	return nil
}

// scanToolWaiting reads a row of Q8: a file row that waits for you.
func scanToolWaiting(rows *sql.Rows, in *toolInput) error {
	var w toolWaiting
	var day, kind sql.NullString
	if err := rows.Scan(&w.ref, &w.account, &day, &kind, &w.amount, &w.why); err != nil {
		return err
	}
	w.day, w.kind = nytime.Date(day.String), kind.String
	in.waiting = append(in.waiting, w)
	return nil
}

// scanToolCoverage reads a row of Q9: a run of days that a source covers.
func scanToolCoverage(rows *sql.Rows, in *toolInput) error {
	c, err := scanCoverage(rows)
	if err != nil {
		return err
	}
	in.coverage = append(in.coverage, c)
	return nil
}

// scanToolUncounted reads a row of Q10: an entry that the store does not
// count.
func scanToolUncounted(rows *sql.Rows, in *toolInput) error {
	var u toolUncounted
	var matched sql.NullString
	if err := rows.Scan(&u.ref, &u.account, &u.why, &matched); err != nil {
		return err
	}
	u.matchedWith = matched.String
	in.uncounted = append(in.uncounted, u)
	return nil
}

// scanToolSymbolDays reads a row of Q12: the first and the last day of the
// store's rows and legs of one source under a symbol of a security, of
// every account.
func scanToolSymbolDays(rows *sql.Rows, in *toolInput) error {
	var d toolSymbolDays
	var first, last string
	if err := rows.Scan(&d.key, &d.symbol, &d.source, &first, &last); err != nil {
		return err
	}
	d.first, d.last = nytime.Date(first), nytime.Date(last)
	in.days = append(in.days, d)
	return nil
}

// scanToolLink reads a row of QL: the keys of a History file's row and of
// the API's row that the store linked to it, of two securities.
func scanToolLink(rows *sql.Rows, in *toolInput) error {
	var l toolLink
	if err := rows.Scan(&l.file, &l.api); err != nil {
		return err
	}
	in.links = append(in.links, l)
	return nil
}
