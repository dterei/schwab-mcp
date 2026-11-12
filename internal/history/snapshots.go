package history

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/nyse"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// SnapshotsRecorded says what the store made of the snapshots it was
// given, from a snapshot file or from a sync's read of the accounts.
type SnapshotsRecorded struct {
	// ReadID is the id in the store of a sync's read of the accounts; 0
	// for a file.
	ReadID int64
	// New counts the snapshots that the store did not hold, and Known those
	// it did: the same JSON of an account, but for white space, taken at
	// the same time, whether from a file or a sync.
	New, Known int
	// Unattributed counts the new snapshots whose account number could not
	// be read. They are kept, but are no account's.
	Unattributed int
	// Conflicts are the accounts and times at which a new snapshot and
	// another of the same account and time differ. All of them are kept.
	Conflicts []SnapshotConflict
	// Unverified counts the new snapshots that stand for a session in a
	// year past nyse.VerifiedThrough, worked out from the calendar's rules
	// alone.
	Unverified int
}

// A SnapshotConflict is snapshots of one account, taken at one time, that
// differ: two answers at one time, or a line that an older schwabtx wrote
// beside the same answer as it is written now.
type SnapshotConflict struct {
	// Account names the account by its ref and masked number: A1 ...398.
	Account   string
	FetchedAt time.Time
	// Snapshots counts the account's snapshots at that time.
	Snapshots int
}

// warnings says what the store made of the snapshots that their owner
// should know.
func (r SnapshotsRecorded) warnings() []string {
	var ws []string
	if r.Unattributed > 0 {
		ws = append(ws, fmt.Sprintf("%d %s could not be read; %s kept but not attributed",
			r.Unattributed, plural(r.Unattributed, "account's number", "accounts' numbers"),
			plural(r.Unattributed, "that snapshot is", "those snapshots are")))
	}
	for _, c := range r.Conflicts {
		all := "both"
		if c.Snapshots > 2 {
			all = "all"
		}
		ws = append(ws, fmt.Sprintf("%d snapshots of %s at %s differ; %s are kept",
			c.Snapshots, c.Account, c.FetchedAt.Format(time.RFC3339), all))
	}
	if r.Unverified > 0 {
		ws = append(ws, fmt.Sprintf("snapshots on or after %d-01-01 use a calendar not yet checked against NYSE's published dates; "+
			"a newer schwabtx will have it", calendar.verifiedThrough+1))
	}
	return ws
}

// plural returns one when n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// RecordAccounts records a read of the accounts with their positions,
// from GET /accounts?fields=positions: the read itself, whatever its
// outcome, and when Schwab answered, each account as a snapshot taken when
// the answer came, to the second, as `schwabtx snapshot` takes one. Each
// snapshot keeps the account's raw JSON with its white space taken out, so
// that a snapshot file that a sync writes from the same answer
// (schwabfile.SnapshotLine) holds what the store holds, and importing it
// adds nothing.
func (s *Store) RecordAccounts(ctx context.Context, r Read, accounts []schwab.Account) (SnapshotsRecorded, error) {
	if err := checkRead(r); err != nil {
		return SnapshotsRecorded{}, err
	}
	rr := readRow{endpoint: "accounts"}
	var raws [][]byte
	if r.Err == nil {
		rr.rows = len(accounts)
		for i, a := range accounts {
			var b bytes.Buffer
			if err := json.Compact(&b, a.Raw); err != nil {
				return SnapshotsRecorded{}, fmt.Errorf("recording a read of the accounts: account %d's raw JSON cannot be read: %w", i+1, err)
			}
			raws = append(raws, b.Bytes())
		}
	}
	fetched := r.AnsweredAt.UTC().Truncate(time.Second)
	var rec SnapshotsRecorded
	err := s.withWrite(ctx, fmt.Sprintf("recording a read of %d accounts", len(accounts)), func(w *wtx) error {
		id, err := insertRead(w, r, rr)
		if err != nil || r.Err != nil {
			rec = SnapshotsRecorded{ReadID: id}
			return err // a refused or failed read is recorded, and that is all
		}
		evidence := make([]snapshotEvidence, len(raws))
		for i, raw := range raws {
			evidence[i] = snapshotEvidence{fetchedAt: fetched, raw: raw, source: "sync", readID: id}
		}
		rec, err = recordSnapshots(w, evidence)
		rec.ReadID = id
		return err
	})
	return rec, err
}

// A snapshotEvidence is a snapshot as it comes to the store: one element
// of a snapshot file's line, or one account of a sync's read.
type snapshotEvidence struct {
	fetchedAt time.Time
	// raw is the element, its white space taken out.
	raw []byte
	// source is file or sync.
	source string
	// importID, line and lineVersion are a file's import, the element's
	// line and that line's version.
	importID, line, lineVersion int64
	// readID is a sync's read.
	readID int64
}

// recordSnapshots stores each snapshot that the store does not hold, as
// the instant it was taken and the digest of its JSON tell, whatever its
// source, and derives it; one that the store holds adds nothing. It says
// what it did, and which accounts now have different snapshots at one
// time.
func recordSnapshots(w *wtx, evidence []snapshotEvidence) (SnapshotsRecorded, error) {
	var rec SnapshotsRecorded
	var added []int64
	for _, e := range evidence {
		id, d, err := insertSnapshot(w, e)
		switch {
		case err != nil:
			return rec, err
		case id == 0:
			rec.Known++
			continue
		}
		rec.New++
		added = append(added, id)
		if !d.attributed {
			rec.Unattributed++
		}
		if d.unverified {
			rec.Unverified++
		}
	}
	var err error
	rec.Conflicts, err = snapshotConflicts(w, added)
	return rec, err
}

// insertSnapshot stores a snapshot and derives it, unless the store holds
// one taken at the same instant whose JSON has the same digest, and
// returns its id, or 0 when the store held it.
func insertSnapshot(w *wtx, e snapshotEvidence) (int64, derivedSnapshot, error) {
	digest := sha256.Sum256(e.raw)
	var importID, line, lineVersion, readID sql.NullInt64
	if e.source == "file" {
		importID = sql.NullInt64{Int64: e.importID, Valid: true}
		line = sql.NullInt64{Int64: e.line, Valid: true}
		lineVersion = sql.NullInt64{Int64: e.lineVersion, Valid: true}
	} else {
		readID = sql.NullInt64{Int64: e.readID, Valid: true}
	}
	var id int64
	err := w.tx.QueryRowContext(w.ctx, `INSERT INTO snapshot (fetched_at, raw, digest, source, import_id, line,
		line_version, read_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (fetched_at, digest) DO NOTHING RETURNING snapshot_id`,
		instant(e.fetchedAt), string(e.raw), digest[:], e.source, importID, line, lineVersion, readID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, derivedSnapshot{}, nil
	}
	if err != nil {
		return 0, derivedSnapshot{}, err
	}
	d, err := deriveSnapshot(w, id, e.fetchedAt, e.raw, true)
	return id, d, err
}

// snapshotConflicts returns the accounts and times at which a snapshot
// whose id is among ids and another of the same account and time differ:
// they cannot be the same JSON, which the store keeps once.
func snapshotConflicts(w *wtx, ids []int64) ([]SnapshotConflict, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	list, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	var out []SnapshotConflict
	err = queryEach(w, func(rows *sql.Rows) error {
		var c SnapshotConflict
		var id int64
		var masked, at string
		if err := rows.Scan(&id, &masked, &at, &c.Snapshots); err != nil {
			return err
		}
		c.Account = "A" + strconv.FormatInt(id, 10) + " " + masked
		var err error
		c.FetchedAt, err = parseInstant(at)
		out = append(out, c)
		return err
	}, `SELECT f.account_id, a.masked, f.fetched_at, count(*) FROM snapshot_fields f JOIN account a ON a.account_id = f.account_id
		WHERE (f.account_id, f.fetched_at) IN (SELECT account_id, fetched_at FROM snapshot_fields
			WHERE snapshot_id IN (SELECT value FROM json_each(?)))
		GROUP BY f.account_id, f.fetched_at HAVING count(*) > 1 ORDER BY f.fetched_at, f.account_id`, string(list))
	return out, err
}

// A derivedSnapshot is what deriveSnapshot found of a snapshot that
// recordSnapshots reports.
type derivedSnapshot struct {
	// attributed says that the snapshot's account number could be read.
	attributed bool
	// unverified says that it stands for a session in a year past the
	// calendar's checked years.
	unverified bool
}

// deriveSnapshot derives snapshot id, taken at fetched, from its raw JSON
// (layer 1 of the derivation): its fields, its positions, its market
// dates, and the keys and names of the positions' instruments. Its account
// is the one whose number it holds, added when the store does not hold it;
// when seen, the account's first and last seen times take in fetched,
// which is so when the snapshot is new and not when it is derived again. A
// snapshot whose number cannot be read is no account's: it is kept, never
// attributed by guess.
func deriveSnapshot(w *wtx, id int64, fetched time.Time, raw []byte, seen bool) (derivedSnapshot, error) {
	var d derivedSnapshot
	r := readSnapshot(raw)
	var acct sql.NullInt64
	if r.number != "" {
		a, err := snapshotAccount(w, r.number, fetched, seen)
		if err != nil {
			return d, err
		}
		acct = sql.NullInt64{Int64: a, Valid: true}
		w.touch(a)
		d.attributed = true
	}
	market, session, err := marketDates(fetched)
	if err != nil {
		return d, err
	}
	d.unverified = unverified(market) || unverified(session)
	f := r.fields
	if _, err := w.exec(`INSERT INTO snapshot_fields (snapshot_id, fetched_at, account_id, market_date, in_session_date,
		calendar_version, account_type, round_trips, is_day_trader, closing_only, pfcb, portfolio_margin,
		intraday_margin, initial_liquidation_cents, current_liquidation_cents, aggregated_liquidation_cents,
		cash_cents, balances, problems) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, instant(fetched), acct, nullString(string(market)), nullString(string(session)), buildVersions().calendar,
		f.accountType, f.roundTrips, f.dayTrader, f.closingOnly, f.pfcb, f.portfolioMargin, f.intradayMargin,
		f.initialLiquidation, f.currentLiquidation, f.aggregatedLiquidation, f.cash, f.balances, f.problems); err != nil {
		return d, err
	}
	for i, p := range r.positions {
		args := []any{id, i, p.assetType, p.symbol, p.cusip, p.description}
		args = append(args, p.quantities[:]...)
		args = append(args, p.figures[:]...)
		if _, err := w.exec(`INSERT INTO snapshot_position (snapshot_id, position, asset_type, symbol, cusip, description,
			long_micros, short_micros, settled_long_micros, settled_short_micros, prev_long_micros, prev_short_micros,
			aged_micros, average_price, average_long_price, average_short_price, taxlot_long_price, taxlot_short_price,
			market_value, maintenance, long_open_pl, short_open_pl, day_pl, day_pl_pct, day_cost, problems)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, append(args, p.problems)...); err != nil {
			return d, err
		}
		if p.key.key != "" {
			if err := observe(w, fetched, p.key, p.names...); err != nil {
				return d, err
			}
		}
	}
	// The positions are written without their securities, which the
	// grouping at the end of the write gives them.
	if len(r.positions) > 0 {
		w.regroup = true
	}
	if market != "" {
		w.sessions = true
	}
	return d, nil
}

// snapshotAccount returns the account whose number is number. When seen,
// the account is added if the store does not hold it, as first seen in a
// snapshot, and its first and last seen times take in at; else it is only
// looked up, and added only if the store does not hold it, so that
// deriving a snapshot again writes no evidence.
func snapshotAccount(w *wtx, number string, at time.Time, seen bool) (int64, error) {
	if !seen {
		var id int64
		err := w.tx.QueryRowContext(w.ctx, "SELECT account_id FROM account WHERE number = ?", number).Scan(&id)
		if !errors.Is(err, sql.ErrNoRows) {
			return id, err
		}
	}
	return ensureAccount(w, number, "snapshot", at)
}

// unverified reports whether day is in a year past the calendar's checked
// years.
func unverified(day nytime.Date) bool {
	if len(day) < 4 {
		return false
	}
	year, err := strconv.Atoi(string(day[:4]))
	return err == nil && year > calendar.verifiedThrough
}

// A marketCalendar is the NYSE calendar that the store works market dates
// and sessions out with.
type marketCalendar struct {
	lastClosed      func(time.Time) (nyse.Session, error)
	during          func(time.Time) (nyse.Session, bool)
	between         func(from, to nytime.Date) ([]nyse.Session, error)
	verifiedThrough int
}

// calendar is nyse's calendar. A test stands in another, with a newer
// calendarVersion, to stand in for a newer schwabtx; nothing else changes
// it.
var calendar = marketCalendar{
	lastClosed:      nyse.LastClosed,
	during:          nyse.During,
	between:         nyse.Between,
	verifiedThrough: nyse.VerifiedThrough,
}

// marketDates returns the market date that a snapshot taken at t stands
// for, the date of the last session that closed at or before t (16:00 New
// York time, or 13:00 on an early close), and the date of the session under
// way at t, if any. A snapshot taken during a session stands for no close,
// though it has a market date. Before the calendar's first close there is
// no market date.
func marketDates(t time.Time) (market, inSession nytime.Date, err error) {
	if s, ok := calendar.during(t); ok {
		inSession = s.Date
	}
	s, err := calendar.lastClosed(t)
	if errors.Is(err, nyse.ErrBeforeFirst) {
		return "", inSession, nil
	}
	return s.Date, inSession, err
}

// deriveSessions brings market_session to the sessions from the earliest
// to the latest market date of any snapshot (layer 3 of the derivation), a
// function of the snapshots, never of the clock, each marked verified when
// its year is one the calendar was checked for. Only rows that change are
// written.
func deriveSessions(w *wtx) error {
	var from, to sql.NullString
	if err := w.tx.QueryRowContext(w.ctx, "SELECT min(market_date), max(market_date) FROM snapshot_fields").Scan(&from, &to); err != nil {
		return err
	}
	var want []nyse.Session
	if from.Valid {
		var err error
		if want, err = calendar.between(nytime.Date(from.String), nytime.Date(to.String)); err != nil {
			return err
		}
	}
	type session struct {
		open, close     string
		early, verified bool
	}
	have := map[string]session{}
	err := queryEach(w, func(rows *sql.Rows) error {
		var date string
		var s session
		err := rows.Scan(&date, &s.open, &s.close, &s.early, &s.verified)
		have[date] = s
		return err
	}, "SELECT date, open_at, close_at, early, verified FROM market_session")
	if err != nil {
		return err
	}
	for _, s := range want {
		date := string(s.Date)
		row := session{open: instant(s.Open), close: instant(s.Close), early: s.Early, verified: !unverified(s.Date)}
		if old, ok := have[date]; !ok || old != row {
			if _, err := w.exec(`INSERT INTO market_session (date, open_at, close_at, early, verified) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (date) DO UPDATE SET open_at = excluded.open_at, close_at = excluded.close_at,
				early = excluded.early, verified = excluded.verified`, date, row.open, row.close, row.early, row.verified); err != nil {
				return err
			}
		}
		delete(have, date)
	}
	for date := range have {
		if _, err := w.exec("DELETE FROM market_session WHERE date = ?", date); err != nil {
			return err
		}
	}
	return nil
}

// pointPositions points each position of a snapshot at the security of
// its key, as readSnapshot keys it.
func pointPositions(w *wtx) error {
	const key = `(SELECT k.security_id FROM instrument_key k WHERE k.key = CASE WHEN snapshot_position.cusip IS NOT NULL
		THEN 'c:' || snapshot_position.cusip ELSE 's:' || coalesce(snapshot_position.asset_type, '') || ':' || snapshot_position.symbol END)`
	_, err := w.exec("UPDATE snapshot_position SET security_id = " + key + " WHERE security_id IS NOT " + key)
	return err
}

// recalendar works out again, under this build's calendar, every
// snapshot's market dates, the market sessions and what each account
// derives from its snapshots' market dates, and records the calendar's
// version.
func recalendar(w *wtx) error {
	type taken struct {
		id int64
		at string
	}
	var all []taken
	if err := queryEach(w, func(rows *sql.Rows) error {
		var d taken
		err := rows.Scan(&d.id, &d.at)
		all = append(all, d)
		return err
	}, "SELECT snapshot_id, fetched_at FROM snapshot_fields ORDER BY snapshot_id"); err != nil {
		return err
	}
	version := buildVersions().calendar
	for _, d := range all {
		at, err := parseInstant(d.at)
		if err != nil {
			return err
		}
		market, session, err := marketDates(at)
		if err != nil {
			return err
		}
		if _, err := w.exec(`UPDATE snapshot_fields SET market_date = ?, in_session_date = ?, calendar_version = ?
			WHERE snapshot_id = ?`, nullString(string(market)), nullString(string(session)), version, d.id); err != nil {
			return err
		}
	}
	w.sessions = true
	if err := queryEach(w, func(rows *sql.Rows) error {
		var id int64
		err := rows.Scan(&id)
		w.touch(id)
		return err
	}, "SELECT account_id FROM account"); err != nil {
		return err
	}
	if err := w.derive(); err != nil {
		return err
	}
	_, err := w.exec("UPDATE store SET calendar_version = ? WHERE id = 1", version)
	return err
}

// rebuildSnapshots derives each snapshot again from its raw JSON, in the
// order of their ids, without writing to the snapshots or their accounts.
func rebuildSnapshots(w *wtx) error {
	type stored struct {
		id      int64
		at, raw string
	}
	for after := int64(0); ; {
		var batch []stored
		err := queryEach(w, func(rows *sql.Rows) error {
			var s stored
			err := rows.Scan(&s.id, &s.at, &s.raw)
			batch = append(batch, s)
			return err
		}, "SELECT snapshot_id, fetched_at, raw FROM snapshot WHERE snapshot_id > ? ORDER BY snapshot_id LIMIT ?", after, rebuildBatch)
		if err != nil || len(batch) == 0 {
			return err
		}
		for _, s := range batch {
			at, err := parseInstant(s.at)
			if err != nil {
				return err
			}
			if _, err := deriveSnapshot(w, s.id, at, []byte(s.raw), false); err != nil {
				return err
			}
		}
		after = batch[len(batch)-1].id
	}
}

// A snapshotRow is what a snapshot's raw JSON gives, as the record reads
// it.
type snapshotRow struct {
	// number is the account's number, trimmed, or "" when it cannot be
	// read: absent, or not a string.
	number    string
	fields    snapshotFieldsRow
	positions []positionRow
}

// A snapshotFieldsRow is a snapshot's columns in snapshot_fields that its
// raw JSON gives. A figure that Schwab did not send is NULL.
type snapshotFieldsRow struct {
	accountType                                                   sql.NullString
	roundTrips                                                    sql.NullInt64
	dayTrader, closingOnly, pfcb                                  sql.NullBool
	portfolioMargin, intradayMargin                               sql.NullBool
	initialLiquidation, currentLiquidation, aggregatedLiquidation sql.NullInt64
	cash                                                          sql.NullInt64
	// balances is every balance figure sent, as JSON: {"initial"|"current"|
	// "projected"|"aggregated": {"<figure>": {"v": the float, "c": its
	// cents, or null}}}.
	balances string
	// problems are the record's problems and the numbers rounded from
	// noise or left NULL, as JSON (snapshotProblem).
	problems string
}

// A positionRow is a position's row in snapshot_position, but for its
// security, which the grouping gives, with the key and names of its
// instrument.
type positionRow struct {
	assetType, symbol, cusip, description sql.NullString
	// quantities are, in micros: long, short, settled long, settled short,
	// the previous session's long and short, and aged.
	quantities [7]any
	// figures are the average price, the average long and short prices,
	// the tax-lot average long and short prices, the market value, the
	// maintenance requirement, the long and short open profit or loss, and
	// the day's profit or loss, its percentage and its cost.
	figures [12]any
	// problems are the position's own, as JSON.
	problems string
	key      keyObservation
	names    []nameObservation
}

// positionQuantities and positionFigures are the keys of a position whose
// values are the columns of positionRow's quantities and figures, in
// order.
var (
	positionQuantities = [7]string{
		"longQuantity", "shortQuantity", "settledLongQuantity", "settledShortQuantity",
		"previousSessionLongQuantity", "previousSessionShortQuantity", "agedQuantity",
	}
	positionFigures = [12]string{
		"averagePrice", "averageLongPrice", "averageShortPrice", "taxLotAverageLongPrice", "taxLotAverageShortPrice",
		"marketValue", "maintenanceRequirement", "longOpenProfitLoss", "shortOpenProfitLoss",
		"currentDayProfitLoss", "currentDayProfitLossPercentage", "currentDayCost",
	}
)

// A snapshotProblem is an entry of snapshot_fields.problems: one of the
// record's problems, with its path and what is wrong; or a number that the
// store rounded from noise or could not keep, with its path, the position
// it is of, the flag it raises (rounded or inexact) and the number as
// Schwab sent it.
type snapshotProblem struct {
	Path     string `json:"path,omitempty"`
	What     string `json:"what,omitempty"`
	Position *int   `json:"position,omitempty"`
	Flag     string `json:"flag,omitempty"`
	Value    string `json:"value,omitempty"`
}

// A balanceFigure is a figure of schwab.Balances or
// schwab.AggregatedBalance: its JSON name, its field, and whether it is an
// amount of money.
type balanceFigure struct {
	name  string
	field int
	money bool
}

// balanceFigures and aggregatedFigures are every figure of the records'
// balances, so that a figure added to them is kept with no change here.
var (
	balanceFigures    = figuresOf(reflect.TypeFor[schwab.Balances]())
	aggregatedFigures = figuresOf(reflect.TypeFor[schwab.AggregatedBalance]())
)

// figuresOf returns the figures of a balances record: each field that is
// a *float64 or a *bool, by its JSON name. Every *float64 is money but
// equityPercentage.
func figuresOf(t reflect.Type) []balanceFigure {
	var out []balanceFigure
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Type.Kind() != reflect.Pointer || name == "" || name == "-" {
			continue
		}
		switch f.Type.Elem().Kind() {
		case reflect.Float64:
			out = append(out, balanceFigure{name: name, field: i, money: name != "equityPercentage"})
		case reflect.Bool:
			out = append(out, balanceFigure{name: name, field: i})
		default:
		}
	}
	return out
}

// A figure is a balance figure as snapshot_fields.balances holds it: the
// float that Schwab sent, and its cents when they are exact or rounded
// from noise.
type figure struct {
	V float64 `json:"v"`
	C *int64  `json:"c"`
}

// readSnapshot returns what a snapshot's raw JSON gives, as the client
// reads an account. The record's decoding never fails; what it could not
// read is in its problems, and a figure whose key is absent, null or could
// not be read is NULL, not 0.
func readSnapshot(raw []byte) snapshotRow {
	var a schwab.Account
	_ = json.Unmarshal(raw, &a) // lenient: it never fails
	var r snapshotRow
	var ps []snapshotProblem
	for _, p := range a.Problems {
		ps = append(ps, snapshotProblem{Path: p.Path, What: p.What})
	}
	// convert keeps f in the unit that how gives, noting a value rounded
	// from noise or kept as NULL.
	convert := func(path string, position *int, f float64, how func(float64) (int64, Conversion)) sql.NullInt64 {
		v, c := how(f)
		switch c {
		case Rounded:
			ps = append(ps, snapshotProblem{Path: path, Position: position, Flag: "rounded", Value: strconv.FormatFloat(f, 'g', -1, 64)})
		case Off:
			ps = append(ps, snapshotProblem{Path: path, Position: position, Flag: "inexact", Value: strconv.FormatFloat(f, 'g', -1, 64)})
			return sql.NullInt64{}
		}
		return sql.NullInt64{Int64: v, Valid: true}
	}
	sa := a.SecuritiesAccount
	r.number = strings.TrimSpace(sa.AccountNumber)

	var top map[string]json.RawMessage
	_ = json.Unmarshal(raw, &top) // what does not read sends nothing
	var accountProblems []schwab.Problem
	for _, p := range a.Problems {
		if rest, ok := strings.CutPrefix(p.Path, "securitiesAccount."); ok {
			accountProblems = append(accountProblems, schwab.Problem{Path: rest, What: p.What})
		}
	}
	sent := sentKeys(top["securitiesAccount"], accountProblems)
	f := &r.fields
	f.accountType = nullString(sa.Type)
	if sent["roundTrips"] {
		f.roundTrips = sql.NullInt64{Int64: int64(sa.RoundTrips), Valid: true}
	}
	f.dayTrader = sql.NullBool{Bool: sa.IsDayTrader, Valid: sent["isDayTrader"]}
	f.closingOnly = sql.NullBool{Bool: sa.IsClosingOnlyRestricted, Valid: sent["isClosingOnlyRestricted"]}
	f.pfcb = sql.NullBool{Bool: sa.PfcbFlag, Valid: sent["pfcbFlag"]}
	if sa.IsPortfolioMargin != nil {
		f.portfolioMargin = sql.NullBool{Bool: *sa.IsPortfolioMargin, Valid: true}
	}
	if sa.IsIntradayMargin != nil {
		f.intradayMargin = sql.NullBool{Bool: *sa.IsIntradayMargin, Valid: true}
	}

	balances := map[string]map[string]figure{}
	amounts := map[string]sql.NullInt64{} // path -> cents of each figure of money
	add := func(set, prefix string, figures []balanceFigure, v reflect.Value) {
		got := map[string]figure{}
		for _, b := range figures {
			field := v.Field(b.field)
			if field.IsNil() {
				continue
			}
			switch x := field.Elem().Interface().(type) {
			case bool:
				fig := figure{}
				if x {
					fig.V = 1
				}
				got[b.name] = fig
			case float64:
				fig := figure{V: x}
				if b.money {
					path := prefix + "." + b.name
					c := convert(path, nil, x, cents)
					if c.Valid {
						fig.C = &c.Int64
					}
					amounts[path] = c
				}
				got[b.name] = fig
			}
		}
		if len(got) > 0 {
			balances[set] = got
		}
	}
	add("initial", "securitiesAccount.initialBalances", balanceFigures, reflect.ValueOf(sa.InitialBalances))
	add("current", "securitiesAccount.currentBalances", balanceFigures, reflect.ValueOf(sa.CurrentBalances))
	add("projected", "securitiesAccount.projectedBalances", balanceFigures, reflect.ValueOf(sa.ProjectedBalances))
	add("aggregated", "aggregatedBalance", aggregatedFigures, reflect.ValueOf(a.AggregatedBalance))
	f.initialLiquidation = amounts["securitiesAccount.initialBalances.liquidationValue"]
	f.currentLiquidation = amounts["securitiesAccount.currentBalances.liquidationValue"]
	f.aggregatedLiquidation = amounts["aggregatedBalance.liquidationValue"]
	f.cash = amounts["securitiesAccount.currentBalances.cashBalance"]
	b, _ := json.Marshal(balances) // maps of plain values always marshal
	f.balances = string(b)

	for i, p := range sa.Positions {
		r.positions = append(r.positions, readPosition(p, i, convert))
	}
	b, _ = json.Marshal(append([]snapshotProblem{}, ps...)) // a slice of plain values always marshals
	f.problems = string(b)
	return r
}

// readPosition returns position i of a snapshot. convert keeps a quantity
// in micros, noting a value rounded from noise or kept as NULL among the
// snapshot's problems.
func readPosition(p schwab.Position, i int, convert func(string, *int, float64, func(float64) (int64, Conversion)) sql.NullInt64) positionRow {
	in := p.Instrument
	row := positionRow{
		assetType:   nullString(string(in.AssetType)),
		symbol:      nullString(in.Symbol),
		cusip:       nullString(in.CUSIP),
		description: free(in.Description),
	}
	sent := sentKeys(p.Raw, p.Problems)
	quantities := [7]float64{
		p.LongQuantity, p.ShortQuantity, p.SettledLongQuantity, p.SettledShortQuantity,
		p.PreviousSessionLongQuantity, p.PreviousSessionShortQuantity, p.AgedQuantity,
	}
	for j, q := range quantities {
		row.quantities[j] = sql.NullInt64{}
		if sent[positionQuantities[j]] {
			row.quantities[j] = convert("securitiesAccount.positions."+positionQuantities[j], &i, q, micros)
		}
	}
	figures := [12]float64{
		p.AveragePrice, p.AverageLongPrice, p.AverageShortPrice, p.TaxLotAverageLongPrice, p.TaxLotAverageShortPrice,
		p.MarketValue, p.MaintenanceRequirement, p.LongOpenProfitLoss, p.ShortOpenProfitLoss,
		p.CurrentDayProfitLoss, p.CurrentDayProfitLossPercentage, p.CurrentDayCost,
	}
	for j, v := range figures {
		row.figures[j] = sql.NullFloat64{Float64: v, Valid: sent[positionFigures[j]]}
	}
	var ps []snapshotProblem
	for _, pr := range p.Problems {
		ps = append(ps, snapshotProblem{Path: pr.Path, What: pr.What})
	}
	b, _ := json.Marshal(append([]snapshotProblem{}, ps...)) // a slice of plain values always marshals
	row.problems = string(b)

	// The instrument is keyed by its CUSIP, else by its asset type and
	// symbol; an option's contract comes from its OCC symbol.
	k := keyObservation{assetType: string(in.AssetType), typ: in.Type, putCall: in.PutCall, underlyingSymbol: in.UnderlyingSymbol}
	switch {
	case in.CUSIP != "":
		k.key = "c:" + in.CUSIP
	case in.Symbol != "":
		k.key = "s:" + string(in.AssetType) + ":" + in.Symbol
	default:
		return row
	}
	if o, ok := schwab.ParseOCC(in.Symbol); ok {
		k.putCall, k.underlyingSymbol, k.expiration = o.PutCall, o.Root, o.Expiration
		k.strike = float64(o.StrikeThousandths) / 1000
	}
	row.key = k
	row.names = []nameObservation{
		{kind: "symbol", value: in.Symbol, seenIn: "position"},
		{kind: "description", value: in.Description, seenIn: "position"},
		{kind: "cusip", value: in.CUSIP, seenIn: "position"},
	}
	return row
}

// sentKeys returns the keys of the JSON object raw whose values the record
// read: present, not null, and with no problem at their path in problems,
// whose paths are within raw.
func sentKeys(raw json.RawMessage, problems []schwab.Problem) map[string]bool {
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(raw, &keys) // what does not read sends nothing
	sent := map[string]bool{}
	for k, v := range keys {
		if string(v) != "null" && !hasProblem(problems, k) {
			sent[k] = true
		}
	}
	return sent
}

// hasProblem reports whether problems hold one at path, or within it.
func hasProblem(problems []schwab.Problem, path string) bool {
	for _, p := range problems {
		if p.Path == path || strings.HasPrefix(p.Path, path+".") {
			return true
		}
	}
	return false
}
