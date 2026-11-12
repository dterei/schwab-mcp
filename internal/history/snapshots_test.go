package history

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/history/schwabfile"
	"github.com/dterei/schwab-mcp/internal/nyse"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
	"github.com/dterei/schwab-mcp/internal/schwab/schwabtest"
)

// The fixtures' account that holds nothing.
const empty = "40221783"

// fixtureAccounts returns the elements of accounts.json, each as Schwab
// sent it.
func fixtureAccounts(t *testing.T) []json.RawMessage {
	t.Helper()
	var raws []json.RawMessage
	if err := json.Unmarshal(schwabtest.Fixture(t, "accounts.json"), &raws); err != nil {
		t.Fatal(err)
	}
	return raws
}

// fixtureAccount returns the element of accounts.json of the account
// whose number is number, changed by each replacement of old by new.
func fixtureAccount(t *testing.T, number string, oldNew ...string) json.RawMessage {
	t.Helper()
	for _, raw := range fixtureAccounts(t) {
		if strings.Contains(string(raw), `"accountNumber": "`+number+`"`) {
			s := string(raw)
			for i := 0; i < len(oldNew); i += 2 {
				if !strings.Contains(s, oldNew[i]) {
					t.Fatalf("account %s has no %s", number, oldNew[i])
				}
				s = strings.Replace(s, oldNew[i], oldNew[i+1], 1)
			}
			return json.RawMessage(s)
		}
	}
	t.Fatalf("accounts.json has no account %s", number)
	return nil
}

// snapshotLine returns the line of a snapshot file for accounts fetched at
// at.
func snapshotLine(t *testing.T, at time.Time, accounts ...json.RawMessage) string {
	t.Helper()
	line, err := schwabfile.SnapshotLine(at, accounts)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// importFile imports one file named name with content, and returns its
// report.
func importFile(t *testing.T, s *Store, name, content string) FileReport {
	t.Helper()
	r, err := s.Import(t.Context(), []FileInput{{Name: name, Content: []byte(content)}}, ImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Files) != 1 {
		t.Fatalf("the import reports %d files, want 1", len(r.Files))
	}
	return r.Files[0]
}

// storeRows returns the rows of query, each as its columns written as
// text, NULL as "NULL".
func storeRows(t *testing.T, s *Store, query string) [][]string {
	t.Helper()
	rows, err := peek(s).QueryContext(t.Context(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		row := make([]string, len(cols))
		for i, v := range vals {
			switch v := v.(type) {
			case nil:
				row[i] = "NULL"
			case []byte:
				row[i] = string(v)
			case string:
				row[i] = v
			case float64:
				row[i] = strconv.FormatFloat(v, 'f', -1, 64)
			default:
				row[i] = fmt.Sprint(v)
			}
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A snapshot's fields come from its raw JSON as the client reads an
// account: the account's flags and type, every balance figure that Schwab
// sent in balances with its cents, the liquidation values and cash in
// columns of their own, and a position's figures, each NULL when its key
// is absent, so that a long position's short figures are not 0. Each
// position's instrument is keyed by its CUSIP, with its names, an option's
// contract coming from its OCC symbol.
func TestReadSnapshot(t *testing.T) {
	t.Parallel()
	raw := fixtureAccount(t, options)
	r := readSnapshot(raw)
	if r.number != options {
		t.Errorf("the number reads as %q", r.number)
	}
	f := r.fields
	want := snapshotFieldsRow{
		accountType:           sql.NullString{String: "MARGIN", Valid: true},
		roundTrips:            sql.NullInt64{Int64: 1, Valid: true},
		dayTrader:             sql.NullBool{Valid: true},
		closingOnly:           sql.NullBool{Valid: true},
		pfcb:                  sql.NullBool{Valid: true},
		portfolioMargin:       sql.NullBool{Valid: true},
		intradayMargin:        sql.NullBool{Valid: true},
		initialLiquidation:    sql.NullInt64{Int64: 550063, Valid: true},
		currentLiquidation:    sql.NullInt64{Int64: 550384, Valid: true},
		aggregatedLiquidation: sql.NullInt64{Int64: 550384, Valid: true},
		cash:                  sql.NullInt64{Int64: 521044, Valid: true},
		balances:              f.balances,
		problems:              "[]",
	}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("the fields read as\n%+v\nwant\n%+v", f, want)
	}
	// Every figure of every set is in balances, with its cents.
	var sent struct {
		SecuritiesAccount map[string]json.RawMessage `json:"securitiesAccount"`
		Aggregated        map[string]any             `json:"aggregatedBalance"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatal(err)
	}
	var balances map[string]map[string]figure
	if err := json.Unmarshal([]byte(f.balances), &balances); err != nil {
		t.Fatal(err)
	}
	for set, key := range map[string]string{"initial": "initialBalances", "current": "currentBalances", "projected": "projectedBalances"} {
		var figures map[string]any
		if err := json.Unmarshal(sent.SecuritiesAccount[key], &figures); err != nil {
			t.Fatal(err)
		}
		if got, want := slices.Sorted(maps.Keys(balances[set])), slices.Sorted(maps.Keys(figures)); !slices.Equal(got, want) {
			t.Errorf("balances holds %s's figures %v, want %v", set, got, want)
		}
	}
	if got, want := slices.Sorted(maps.Keys(balances["aggregated"])), slices.Sorted(maps.Keys(sent.Aggregated)); !slices.Equal(got, want) {
		t.Errorf("balances holds the aggregated figures %v, want %v", got, want)
	}
	if fig := balances["current"]["liquidationValue"]; fig.V != 5503.84 || fig.C == nil || *fig.C != 550384 {
		t.Errorf("the current liquidation value reads as %+v", fig)
	}
	if fig := balances["initial"]["isInCall"]; fig.V != 0 || fig.C != nil {
		t.Errorf("isInCall reads as %+v, want 0 without cents", fig)
	}
	inCall := readSnapshot(fixtureAccount(t, options, `"isInCall": false`, `"isInCall": true`))
	var inCallSets map[string]map[string]figure
	if err := json.Unmarshal([]byte(inCall.fields.balances), &inCallSets); err != nil {
		t.Fatal(err)
	}
	if fig := inCallSets["initial"]["isInCall"]; fig.V != 1 || fig.C != nil {
		t.Errorf("isInCall true reads as %+v, want 1 without cents", fig)
	}
	if fig, ok := balances["initial"]["equityPercentage"]; !ok || fig.C != nil {
		t.Errorf("equityPercentage reads as %+v, want a value without cents", fig)
	}

	if len(r.positions) != 2 {
		t.Fatalf("read %d positions, want 2", len(r.positions))
	}
	option := r.positions[0]
	null := sql.NullInt64{}
	if option.quantities != [7]any{
		sql.NullInt64{Valid: true},
		sql.NullInt64{Int64: 2_000_000, Valid: true},
		sql.NullInt64{Valid: true},
		sql.NullInt64{Int64: -2_000_000, Valid: true},
		null,
		sql.NullInt64{Int64: 2_000_000, Valid: true},
		null,
	} {
		t.Errorf("the option's quantities read as %v", option.quantities)
	}
	if v := option.figures[1]; v != (sql.NullFloat64{}) {
		t.Errorf("the short option's average long price reads as %v, want NULL", v)
	}
	if v := option.figures[5]; v != (sql.NullFloat64{Float64: -310, Valid: true}) {
		t.Errorf("the option's market value reads as %v", v)
	}
	wantKey := keyObservation{
		key: "c:0XYZ..RI60045000", assetType: "OPTION", typ: "VANILLA", putCall: "PUT", strike: 45,
		expiration: "2026-06-19", underlyingSymbol: "XYZ",
	}
	if option.key != wantKey {
		t.Errorf("the option is keyed as %+v, want %+v", option.key, wantKey)
	}
	if !slices.Contains(option.names, nameObservation{kind: "symbol", value: "XYZ   260619P00045000", seenIn: "position"}) ||
		!slices.Contains(option.names, nameObservation{kind: "cusip", value: "0XYZ..RI60045000", seenIn: "position"}) {
		t.Errorf("the option's names are %+v", option.names)
	}

	// An account that holds nothing, without current liquidation value,
	// cash or aggregated balance.
	r = readSnapshot(fixtureAccount(t, empty))
	if f := r.fields; f.currentLiquidation.Valid || f.cash.Valid || f.aggregatedLiquidation.Valid || !f.initialLiquidation.Valid ||
		f.portfolioMargin.Valid || len(r.positions) != 0 {
		t.Errorf("the empty account reads as %+v with %d positions", f, len(r.positions))
	}
}

// Numbers that are noise are kept rounded, and numbers off the grid are
// left NULL, each noted with its path, its position and the number as
// sent, for the flags. A figure sent as a string or as null, a number sent
// as a number, and a position that is not an object read as absent, never
// as 0; free text is masked.
func TestReadSnapshotOddValues(t *testing.T) {
	t.Parallel()
	r := readSnapshot(fixtureAccount(t, individual,
		`"liquidationValue": 6975.01`, `"liquidationValue": 81.29223999999999`,
		`"liquidationValue": 6997.68`, `"liquidationValue": 6997.69`,
		`"longQuantity": 12.0`, `"longQuantity": -236.999999999999`,
		`"shortQuantity": 0.0`, `"shortQuantity": "0"`,
		`"agedQuantity": 1220.89`, `"agedQuantity": null`,
		`"cashBalance": 1532.17`, `"cashBalance": "1532.17"`,
		`"description": "VANGUARD TOTAL STOCK MARKET ETF"`, `"description": "JOURNAL TO 12345678"`,
	))
	if f := r.fields; f.initialLiquidation.Valid || f.cash != (sql.NullInt64{Int64: 153217, Valid: true}) ||
		f.currentLiquidation.Int64 != 699769 || f.aggregatedLiquidation.Int64 != 699768 {
		t.Errorf("the liquidation values read as %v, %v and %v, and current cash as %v",
			f.initialLiquidation, f.currentLiquidation, f.aggregatedLiquidation, f.cash)
	}
	var ps []snapshotProblem
	if err := json.Unmarshal([]byte(r.fields.problems), &ps); err != nil {
		t.Fatal(err)
	}
	zero := 0
	for _, want := range []snapshotProblem{
		{Path: "securitiesAccount.initialBalances.liquidationValue", Flag: "inexact", Value: "81.29223999999999"},
		{Path: "securitiesAccount.positions.longQuantity", Position: &zero, Flag: "rounded", Value: "-236.999999999999"},
	} {
		if !slices.ContainsFunc(ps, func(p snapshotProblem) bool { return reflect.DeepEqual(p, want) }) {
			t.Errorf("the problems %s do not hold %+v", r.fields.problems, want)
		}
	}
	if !slices.ContainsFunc(ps, func(p snapshotProblem) bool {
		return p.Path == "securitiesAccount.initialBalances.cashBalance" && p.What != "" && p.Flag == ""
	}) {
		t.Errorf("the problems %s do not hold the figure sent as a string", r.fields.problems)
	}
	var balances map[string]map[string]figure
	if err := json.Unmarshal([]byte(r.fields.balances), &balances); err != nil {
		t.Fatal(err)
	}
	if _, ok := balances["initial"]["cashBalance"]; ok {
		t.Error("a figure sent as a string is in balances")
	}
	if fig := balances["initial"]["liquidationValue"]; fig.V != 81.29223999999999 || fig.C != nil {
		t.Errorf("an off-grid figure reads as %+v, want its value without cents", fig)
	}
	vti := r.positions[0]
	if vti.quantities[0] != (sql.NullInt64{Int64: -237_000_000, Valid: true}) || vti.quantities[1] != (sql.NullInt64{}) ||
		vti.description.String != "JOURNAL TO ...678" {
		t.Errorf("the noisy position reads %v and %v, %q", vti.quantities[0], vti.quantities[1], vti.description.String)
	}
	if fund := r.positions[3]; fund.quantities[6] != (sql.NullInt64{}) {
		t.Errorf("an aged quantity sent as null reads as %v", fund.quantities[6])
	}

	r = readSnapshot(json.RawMessage(`{"securitiesAccount": {"accountNumber": 29470135, "type": "CASH", "positions": [5]}}`))
	if r.number != "" || len(r.positions) != 1 || r.positions[0].quantities != [7]any{
		sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{},
	} || r.positions[0].key.key != "" || r.positions[0].problems == "[]" {
		t.Errorf("an account number sent as a number, with a position that is a number, reads as %+v", r)
	}
	if f := r.fields; f.accountType.String != "CASH" || f.roundTrips.Valid || f.dayTrader.Valid || f.closingOnly.Valid || f.pfcb.Valid {
		t.Errorf("an account of only a type reads as %+v", f)
	}
	if r := readSnapshot(json.RawMessage(`{"securitiesAccount": {"accountNumber": " 29470135 "}}`)); r.number != individual || r.fields.accountType.Valid {
		t.Errorf("a number with spaces around reads as %q, of type %v", r.number, r.fields.accountType)
	}
	if r := readSnapshot(json.RawMessage(`null`)); r.number != "" || r.fields.problems == "[]" || r.fields.balances != "{}" {
		t.Errorf("null reads as %+v", r)
	}
}

// A snapshot is flagged for what the record could not read, by path, its
// own or a position's; for a number stored rounded and, when it has a
// column of its own or is a position's quantity, for one off the grid,
// each with its path and the number as sent; and for an account number
// that could not be read. Another balance figure off the grid is not
// flagged.
func TestSnapshotFlags(t *testing.T) {
	t.Parallel()
	r := readSnapshot(fixtureAccount(t, individual,
		`"liquidationValue": 6975.01`, `"liquidationValue": 81.29223999999999`,
		`"longStockValue": 5442.84`, `"longStockValue": 5442.845`,
		`"cashBalance": 1532.17`, `"cashBalance": "1532.17"`,
		`"longQuantity": 12.0`, `"longQuantity": -236.999999999999`,
		`"shortQuantity": 0.0`, `"shortQuantity": "0"`,
		`"agedQuantity": 1220.89`, `"agedQuantity": 1.0000005`,
	))
	want := []entryFlag{
		{"inexact", "securitiesAccount.initialBalances.liquidationValue 81.29223999999999; securitiesAccount.positions[3].agedQuantity 1.0000005"},
		{"problems", "securitiesAccount.initialBalances.cashBalance; securitiesAccount.positions[0].shortQuantity"},
		{"rounded", "securitiesAccount.positions[0].longQuantity -236.999999999999"},
	}
	if got := snapshotFlags(r).sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("the snapshot is flagged\n%q\nwant\n%q", got, want)
	}
	// The current cash and the aggregated liquidation value have columns
	// of their own too.
	r = readSnapshot(fixtureAccount(t, individual,
		`"cashBalance": 1532.17`, `"cashBalance":1532.17`, // the initial one, left as it was
		`"cashBalance": 1532.17`, `"cashBalance": 1532.171`,
		`"liquidationValue": 6997.68`, `"liquidationValue":6997.68`, // the current one, left as it was
		`"liquidationValue": 6997.68`, `"liquidationValue": 6997.6801`,
	))
	want = []entryFlag{{"inexact", "securitiesAccount.currentBalances.cashBalance 1532.171; aggregatedBalance.liquidationValue 6997.6801"}}
	if got := snapshotFlags(r).sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("the snapshot is flagged\n%q\nwant\n%q", got, want)
	}
	want = []entryFlag{{"no_account_number", ""}, {"problems", "securitiesAccount.accountNumber"}}
	if got := snapshotFlags(readSnapshot(fixtureAccount(t, individual, `"29470135"`, `29470135`))).sorted(); !reflect.DeepEqual(got, want) {
		t.Errorf("a snapshot whose number is a number is flagged %q, want %q", got, want)
	}
	if got := snapshotFlags(readSnapshot(fixtureAccount(t, individual))); len(got) != 0 {
		t.Errorf("a snapshot as Schwab sent it is flagged %q", got)
	}

	for _, c := range []struct {
		market, inSession nytime.Date
		want              []entryFlag
	}{
		{"2026-11-06", "", nil},
		{"2026-11-09", "2026-11-10", []entryFlag{{"in_session", "2026-11-10"}}},
		{"", "", []entryFlag{{"before_calendar", ""}}},
		{"", "2000-01-03", []entryFlag{{"before_calendar", ""}, {"in_session", "2000-01-03"}}},
		{"2028-12-29", "", nil},
		{"2029-01-02", "", []entryFlag{{"calendar_unverified", "2029-01-02"}}},
		{"2028-12-29", "2029-01-02", []entryFlag{{"calendar_unverified", "2029-01-02"}, {"in_session", "2029-01-02"}}},
	} {
		if got := calendarFlags(c.market, c.inSession).sorted(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("a snapshot that stands for %q, in session %q, is flagged %q, want %q", c.market, c.inSession, got, c.want)
		}
	}
}

// Every figure of the records' balances is read, whatever its name, so
// that a figure added to them is kept; isInCall is a figure too.
func TestBalanceFigures(t *testing.T) {
	t.Parallel()
	if n, want := len(balanceFigures), reflect.TypeFor[schwab.Balances]().NumField(); n != want {
		t.Errorf("read %d figures of %d", n, want)
	}
	if n, want := len(aggregatedFigures), reflect.TypeFor[schwab.AggregatedBalance]().NumField(); n != want {
		t.Errorf("read %d aggregated figures of %d", n, want)
	}
	for _, f := range balanceFigures {
		if f.money != (f.name != "isInCall" && f.name != "equityPercentage") {
			t.Errorf("%s is money: %v", f.name, f.money)
		}
	}
}

// pst is Los Angeles' zone in winter, which every date in these tests
// that is written in Pacific time is in.
var pst = time.FixedZone("PST", -8*3600)

// timeIn returns the instant of a date and time in zone.
func timeIn(t *testing.T, zone *time.Location, datetime string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04:05", datetime, zone)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// A snapshot stands for the last session that closed at or before it was
// taken, with early closes, not for the New York date of its time; one
// taken during a session also names that session, since it stands for no
// close. Before the calendar's first close it stands for none.
func TestMarketDates(t *testing.T) {
	t.Parallel()
	ny := nytime.NewYork()
	for _, c := range []struct {
		zone              *time.Location
		at                string
		market, inSession nytime.Date
	}{
		{ny, "2026-11-10 15:45:00", "2026-11-09", "2026-11-10"},
		{ny, "2026-11-10 15:59:59", "2026-11-09", "2026-11-10"},
		{ny, "2026-11-10 16:00:00", "2026-11-10", ""},
		{pst, "2026-11-10 21:05:00", "2026-11-10", ""},
		{ny, "2026-11-09 09:00:00", "2026-11-06", ""},
		{ny, "2026-11-09 09:30:00", "2026-11-06", "2026-11-09"},
		{pst, "2026-11-26 21:05:00", "2026-11-25", ""},
		{ny, "2026-11-27 12:59:00", "2026-11-25", "2026-11-27"},
		{ny, "2026-11-27 13:00:00", "2026-11-27", ""},
		{pst, "2026-11-29 21:05:00", "2026-11-27", ""},
		{pst, "2026-12-31 21:05:00", "2026-12-31", ""},
		{ny, "2025-01-09 16:30:00", "2025-01-08", ""},
		{ny, "2000-01-03 10:00:00", "", "2000-01-03"},
		{ny, "1999-12-31 16:30:00", "", ""},
	} {
		market, inSession, err := marketDates(timeIn(t, c.zone, c.at))
		if err != nil || market != c.market || inSession != c.inSession {
			t.Errorf("%s %s stands for %q, in session %q (%v), want %q and %q", c.at, c.zone, market, inSession, err, c.market, c.inSession)
		}
	}
}

// An account's snapshots give it a coverage from the first to the last
// market date that a snapshot taken outside a session stands for, and its
// type from the latest snapshot that gives one, whatever their order.
func TestDeriveAccountSnapshots(t *testing.T) {
	t.Parallel()
	day := func(d int) time.Time { return time.Date(2026, 11, d, 5, 5, 0, 0, time.UTC) }
	in := accountInputs{account: 1, snapshots: []snapshotInput{
		{id: 4, fetchedAt: day(9), accountType: "CASH", market: "2026-11-06", inSession: "2026-11-09"},
		{id: 2, fetchedAt: day(5), accountType: "MARGIN", market: "2026-11-04"},
		{id: 3, fetchedAt: day(7), market: "2026-11-06"},
		{id: 1, fetchedAt: day(3), accountType: "CASH", market: "2026-11-02"},
		{id: 5, fetchedAt: day(10), market: "2026-11-09", inSession: "2026-11-10"},
		{id: 6, fetchedAt: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC), accountType: "CASH"},
	}}
	want := accountOutputs{coverage: []coverageRun{{source: "snapshot", from: "2026-11-02", to: "2026-11-06"}}, accountType: "CASH"}
	for range 3 {
		// Only what snapshots give: TestEntries checks the entries.
		out := deriveAccount(in)
		if got := (accountOutputs{txns: out.txns, coverage: out.coverage, accountType: out.accountType, conflicts: out.conflicts}); !reflect.DeepEqual(got, want) {
			t.Errorf("derived %+v, want %+v", got, want)
		}
		slices.Reverse(in.snapshots)
		in.snapshots[0], in.snapshots[2] = in.snapshots[2], in.snapshots[0]
	}
	tie := []snapshotInput{
		{id: 7, fetchedAt: day(9), accountType: "MARGIN"},
		{id: 3, fetchedAt: day(8)},
		{id: 9, fetchedAt: day(9)},
		{id: 8, fetchedAt: day(9), accountType: "CASH"},
		{id: 2, fetchedAt: day(8).Add(time.Second)},
	}
	conflicts := []snapshotFlag{{7, "differs from S8, S9"}, {8, "differs from S7, S9"}, {9, "differs from S7, S8"}}
	for range 2 {
		got := deriveAccount(accountInputs{account: 1, snapshots: tie})
		if got.accountType != "CASH" {
			t.Errorf("of two snapshots at one time the type is %q, want the later id's", got.accountType)
		}
		if !reflect.DeepEqual(got.conflicts, conflicts) {
			t.Errorf("the conflicts are %+v, want %+v", got.conflicts, conflicts)
		}
		slices.Reverse(tie)
	}
	if got := deriveAccount(accountInputs{account: 1, snapshots: []snapshotInput{{id: 1, market: "2026-11-09", inSession: "2026-11-10"}}}); got.coverage != nil {
		t.Errorf("an intraday snapshot alone covers %v", got.coverage)
	}
}

// Lines of a snapshot file import as snapshots, each account as it was
// at the line's time; importing the file again, or a copy of it with more
// lines, adds only what is new. A partial last line and a line of another
// version are skipped and counted; an account whose number is a number is
// kept but no account's; two different snapshots of one account at one
// time are both kept and reported. Accounts are added as first seen in a
// snapshot, take their type from their latest snapshot, and their
// positions point at their securities.
func TestImportSnapshots(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ctx := t.Context()
	fri := time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC) // Fri 2026-11-06 21:05 PT
	accounts := fixtureAccounts(t)
	day1 := snapshotLine(t, fri, accounts...)
	day2 := snapshotLine(t, fri.Add(24*time.Hour), accounts...)
	r := importFile(t, s, "/home/me/schwabtx/snapshots.jsonl", day1+day2)
	if r.Name != "snapshots.jsonl" || r.Kind != "snapshots" || r.Lines != 2 || len(r.Skipped) != 0 ||
		r.Snapshots.New != 6 || r.Snapshots.Known != 0 || len(r.Warnings) != 0 {
		t.Fatalf("the first import reports %+v", r)
	}

	conflicting := fixtureAccount(t, individual, `"roundTrips": 0`, `"roundTrips": 1`)
	day3 := fri.Add(48 * time.Hour)
	more := day1 + day2 + "\n" +
		snapshotLine(t, day3, append(accounts[1:], conflicting)...) +
		snapshotLine(t, day3, accounts[0]) +
		`{"version":2,"fetched_at":"2026-11-10T05:05:00Z","accounts":[]}` + "\n" +
		snapshotLine(t, day3.Add(24*time.Hour), json.RawMessage(`{"securitiesAccount":{"accountNumber":29470135,"type":"CASH"}}`)) +
		`{"version":1,"fetched_at":"2026-11-11T05:05:00Z","accou`
	r = importFile(t, s, "Joint 12345678 snapshots.jsonl", more)
	if r.Name != "Joint ...678 snapshots.jsonl" || r.Lines != 7 || r.Snapshots.New != 5 || r.Snapshots.Known != 6 ||
		r.Snapshots.Unattributed != 1 || !maps.Equal(r.Skipped, map[string]int{"version 2": 1, schwabfile.SkipPartial: 1}) {
		t.Errorf("the second import reports %+v", r)
	}
	wantWarnings := []string{
		"1 line skipped (1 version 2: written by a newer schwabtx?)",
		"the last line has no newline (a snapshot being written?); it was skipped",
		"1 account's number could not be read; that snapshot is kept but not attributed",
		"2 snapshots of A1 ...135 at 2026-11-09T05:05:00Z differ; both are kept",
	}
	if !slices.Equal(r.Warnings, wantWarnings) {
		t.Errorf("the second import warns\n%q\nwant\n%q", r.Warnings, wantWarnings)
	}
	wantConflicts := []SnapshotConflict{{Account: "A1 ...135", FetchedAt: day3, Snapshots: 2}}
	if !reflect.DeepEqual(r.Snapshots.Conflicts, wantConflicts) {
		t.Errorf("the conflicts are %+v, want %+v", r.Snapshots.Conflicts, wantConflicts)
	}
	// The two snapshots that differ are flagged, each naming the other, and
	// so is the snapshot that is no account's.
	if got := storeRows(t, s, "SELECT ref, flags FROM snapshots WHERE flags IS NOT NULL ORDER BY snapshot_id"); !reflect.DeepEqual(got,
		[][]string{{"S9", "snapshot_conflict"}, {"S10", "snapshot_conflict"}}) {
		t.Errorf("the snapshots view flags %q", got)
	}
	if got := storeStrings(t, s, `SELECT subject_ref || ' ' || code || ' ' || severity || ' ' || coalesce(detail, '') FROM flags
		WHERE code <> 'problems' ORDER BY subject_ref, code`); !slices.Equal(got, []string{
		"S10 snapshot_conflict attention differs from S9",
		"S11 no_account_number attention ",
		"S9 snapshot_conflict attention differs from S10",
	}) {
		t.Errorf("the flags are %q", got)
	}

	if got := storeRows(t, s, "SELECT label, lines, new, known, skipped FROM snapshot_import ORDER BY import_id"); !reflect.DeepEqual(got, [][]string{
		{"snapshots.jsonl", "2", "6", "0", "{}"},
		{"Joint ...678 snapshots.jsonl", "7", "5", "6", `{"partial last line":1,"version 2":1}`},
	}) {
		t.Errorf("snapshot_import holds %q", got)
	}
	if got := storeRows(t, s, `SELECT number, created_from, first_seen_at, last_seen_at, account_type FROM account ORDER BY account_id`); !reflect.DeepEqual(got, [][]string{
		{individual, "snapshot", "2026-11-07T05:05:00.000Z", "2026-11-09T05:05:00.000Z", "CASH"},
		{options, "snapshot", "2026-11-07T05:05:00.000Z", "2026-11-09T05:05:00.000Z", "MARGIN"},
		{empty, "snapshot", "2026-11-07T05:05:00.000Z", "2026-11-09T05:05:00.000Z", "MARGIN"},
	}) {
		t.Errorf("the accounts are %q", got)
	}
	// The line of the account whose number is a number is kept, but in no
	// view.
	if got := storeRows(t, s, `SELECT s.line, s.line_version, s.source, f.account_id FROM snapshot s
		JOIN snapshot_fields f ON f.snapshot_id = s.snapshot_id WHERE f.account_id IS NULL`); !reflect.DeepEqual(got, [][]string{{"7", "1", "file", "NULL"}}) {
		t.Errorf("the unattributed snapshot is %q", got)
	}
	if got := storeRows(t, s, `SELECT account, fetched_at, market_date, in_session_date, current_liquidation_cents,
		initial_liquidation_cents, aggregated_liquidation_cents, cash_cents, positions FROM snapshots ORDER BY fetched_at, account_id, snapshot_id`); len(got) != 10 ||
		!slices.Equal(got[0], []string{"...135", "2026-11-07T05:05:00.000Z", "2026-11-06", "NULL", "699768", "697501", "699768", "153217", "4"}) ||
		!slices.Equal(got[2], []string{"...783", "2026-11-07T05:05:00.000Z", "2026-11-06", "NULL", "NULL", "485550", "NULL", "NULL", "0"}) {
		t.Errorf("the snapshots view shows %q", got)
	}
	// Each balance figure is a row of balances, with its cents.
	if got := storeStrings(t, s, `SELECT balance_set || '.' || figure || '=' || value_as_sent || '/' || coalesce(value_cents, 'NULL') FROM balances
		WHERE snapshot_id = 1 AND figure IN ('liquidationValue', 'isInCall') ORDER BY balance_set, figure`); !slices.Equal(got, []string{
		"aggregated.liquidationValue=6997.68/699768",
		"current.liquidationValue=6997.68/699768",
		"initial.isInCall=0/NULL",
		"initial.liquidationValue=6975.01/697501",
	}) {
		t.Errorf("the balances view shows %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM balances WHERE snapshot_id = 1"); n != 18+20+2+4 {
		t.Errorf("the balances view shows %d figures of the first snapshot, want 44", n)
	}
	// A position's absent figures are NULL, and each position points at
	// the security of its CUSIP.
	if got := storeRows(t, s, `SELECT symbol, security_key, long_micros, short_micros, settled_short_micros, cost_basis FROM positions
		WHERE snapshot_id = 2 ORDER BY position`); !reflect.DeepEqual(got, [][]string{
		{"XYZ   260619P00045000", "c:0XYZ..RI60045000", "0", "2000000", "-2000000", "320"},
		{"META", "c:30303M102", "1000000", "0", "0", "512"},
	}) {
		t.Errorf("the positions of the second account are %q", got)
	}
	if got := storeRows(t, s, `SELECT average_long_price, prev_long_micros, prev_short_micros, aged_micros FROM snapshot_position
		WHERE snapshot_id = 2 ORDER BY position`); !reflect.DeepEqual(got, [][]string{{"NULL", "NULL", "2000000", "NULL"}, {"512", "1000000", "NULL", "NULL"}}) {
		t.Errorf("the figures of the second account's positions are %q", got)
	}
	if got := storeRows(t, s, `SELECT security_key, put_call, strike, expiration_day, underlying_symbol FROM securities WHERE asset_type = 'OPTION'`); !reflect.DeepEqual(got,
		[][]string{{"c:0XYZ..RI60045000", "PUT", "45", "2026-06-19", "XYZ"}}) {
		t.Errorf("the option is %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM snapshot_position WHERE security_id IS NULL"); n != 0 {
		t.Errorf("%d positions point at no security", n)
	}

	// Importing again adds nothing, but records the import.
	r = importFile(t, s, "snapshots.jsonl", more)
	if r.Snapshots.New != 0 || r.Snapshots.Known != 11 || len(r.Snapshots.Conflicts) != 0 {
		t.Errorf("the third import reports %+v", r)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM snapshot"); n != 11 {
		t.Errorf("the store holds %d snapshots, want 11", n)
	}

	// A dry run reports what it would do and saves nothing; a file that the
	// store does not import is refused with nothing imported.
	dry, err := s.Import(ctx, []FileInput{{Name: "a.jsonl", Content: []byte(snapshotLine(t, day3.Add(48*time.Hour), accounts...))}}, ImportOptions{DryRun: true})
	if err != nil || len(dry.Files) != 1 || dry.Files[0].Snapshots.New != 3 {
		t.Errorf("the dry run reports %+v, %v", dry, err)
	}
	_, err = s.Import(ctx, []FileInput{
		{Name: "b.jsonl", Content: []byte(snapshotLine(t, day3.Add(72*time.Hour), accounts...))},
		{Name: "Checking_XXX135_Transactions_20261111-120000.json", Content: []byte(`{"PostedTransactions": []}`)},
	}, ImportOptions{})
	if err == nil || err.Error() != "Checking_XXX135_Transactions_20261111-120000.json is a Schwab Bank export, which schwabtx does not import; nothing was imported" {
		t.Errorf("importing a Schwab Bank export gives %v", err)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM snapshot"); n != 11 {
		t.Errorf("the store holds %d snapshots after a dry run and a refusal, want 11", n)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM snapshot_import"); n != 3 {
		t.Errorf("the store records %d imports after a dry run and a refusal, want 3", n)
	}
}

// snapshotAt returns a line of the fixtures' accounts whose numbers are
// numbers, fetched at at.
func snapshotAt(t *testing.T, at time.Time, numbers ...string) string {
	t.Helper()
	var raws []json.RawMessage
	for _, n := range numbers {
		raws = append(raws, fixtureAccount(t, n))
	}
	return snapshotLine(t, at, raws...)
}

// Each snapshot stands for the last session closed when it was taken, and
// the market sessions run from the earliest to the latest of those. A
// session from an account's first to its last closed-phase snapshot that
// none of its closed-phase snapshots stands for is a gap: a session whose
// only snapshot was taken during it is one, and weekends and holidays are
// not sessions. The coverage of the account's snapshots is that run. None
// of it depends on the clock.
func TestMarketDatesAndGaps(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	pt := func(datetime string) time.Time { return timeIn(t, pst, datetime) }
	et := func(datetime string) time.Time { return timeIn(t, nytime.NewYork(), datetime) }
	file := snapshotAt(t, pt("2026-11-06 21:05:00"), individual, options) + // stands for Fri 11-06
		snapshotAt(t, pt("2026-11-07 21:05:00"), individual) + // a Saturday: Fri 11-06 again
		snapshotAt(t, et("2026-11-09 15:45:00"), individual, options) + // during Mon 11-09
		snapshotAt(t, pt("2026-11-10 21:05:00"), individual) + // Tue 11-10
		snapshotAt(t, pt("2026-11-25 21:05:00"), individual, options) + // Wed 11-25, before Thanksgiving
		snapshotAt(t, pt("2026-11-26 21:05:00"), individual) // Thanksgiving: Wed 11-25 again
	importFile(t, s, "snapshots.jsonl", file)
	if got := storeStrings(t, s, "SELECT DISTINCT market_date || '/' || coalesce(in_session_date, '') FROM snapshots ORDER BY fetched_at"); !slices.Equal(got,
		[]string{"2026-11-06/", "2026-11-06/2026-11-09", "2026-11-10/", "2026-11-25/"}) {
		t.Errorf("the market dates are %q", got)
	}
	if got := storeStrings(t, s, "SELECT DISTINCT market_date || ' ' || coalesce(flags, '') FROM snapshots ORDER BY fetched_at"); !slices.Equal(got,
		[]string{"2026-11-06 ", "2026-11-06 in_session", "2026-11-10 ", "2026-11-25 "}) {
		t.Errorf("the snapshots are flagged %q", got)
	}
	wantSessions, err := nyse.Between("2026-11-06", "2026-11-25")
	if err != nil {
		t.Fatal(err)
	}
	if got := storeStrings(t, s, "SELECT date || ' ' || open_at || ' ' || close_at || ' ' || early || ' ' || verified FROM sessions ORDER BY date"); len(got) != len(wantSessions) ||
		got[0] != "2026-11-06 2026-11-06T14:30:00.000Z 2026-11-06T21:00:00.000Z 0 1" {
		t.Errorf("the sessions are %q, want the %d from 2026-11-06 to 2026-11-25", got, len(wantSessions))
	}
	gaps := func(s *Store) []string {
		return storeStrings(t, s, "SELECT account || ' ' || market_date FROM snapshot_gaps ORDER BY account_id, market_date")
	}
	var want []string
	for _, ses := range wantSessions[1:] {
		if ses.Date != "2026-11-10" && ses.Date != "2026-11-25" {
			want = append(want, "...135 "+string(ses.Date))
		}
	}
	for _, ses := range wantSessions[1 : len(wantSessions)-1] {
		want = append(want, "...398 "+string(ses.Date))
	}
	if got := gaps(s); !slices.Equal(got, want) {
		t.Errorf("the gaps are\n%q\nwant\n%q", got, want)
	}
	coverages := func(s *Store) [][]string {
		return storeRows(t, s, "SELECT account, source, from_date, to_date FROM coverages ORDER BY account_id")
	}
	wantCoverage := [][]string{{"...135", "snapshot", "2026-11-06", "2026-11-25"}, {"...398", "snapshot", "2026-11-06", "2026-11-25"}}
	if got := coverages(s); !reflect.DeepEqual(got, wantCoverage) {
		t.Errorf("the coverages are %q", got)
	}

	// Another handle on the same store, whose clock reads years later,
	// reads the same views.
	later, err := Open(t.Context(), Options{Path: s.Path(), Now: func() time.Time { return testNow.AddDate(3, 0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { later.Close() })
	if !slices.Equal(gaps(later), want) || !reflect.DeepEqual(coverages(later), wantCoverage) {
		t.Error("the views read otherwise with another clock")
	}
}

// A snapshot taken during a session stands for no close: it fills no gap,
// though its market date is the session before, and it does not move an
// account's first closed-phase snapshot back.
func TestIntradaySnapshotFillsNoGap(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	et := func(datetime string) time.Time { return timeIn(t, nytime.NewYork(), datetime) }
	file := snapshotAt(t, et("2026-11-05 18:00:00"), individual) + // stands for Thu 11-05
		snapshotAt(t, et("2026-11-09 10:00:00"), options) + // during Mon 11-09: Fri 11-06, but not a close
		snapshotAt(t, et("2026-11-09 15:45:00"), individual) + // likewise
		snapshotAt(t, et("2026-11-10 18:00:00"), individual, options) + // Tue 11-10
		snapshotAt(t, et("2026-11-11 18:00:00"), options) // Wed 11-11
	importFile(t, s, "snapshots.jsonl", file)
	if got := storeStrings(t, s, "SELECT account || ' ' || market_date FROM snapshot_gaps ORDER BY account_id, market_date"); !slices.Equal(got,
		[]string{"...135 2026-11-06", "...135 2026-11-09"}) {
		t.Errorf("the gaps are %q, want Fri 11-06 and Mon 11-09 of ...135 alone", got)
	}
	if got := storeRows(t, s, "SELECT account, from_date, to_date FROM coverages WHERE source = 'snapshot' ORDER BY account_id"); !reflect.DeepEqual(got,
		[][]string{{"...135", "2026-11-05", "2026-11-10"}, {"...398", "2026-11-10", "2026-11-11"}}) {
		t.Errorf("the snapshots' coverages are %q", got)
	}
}

// A sync's read of the accounts keeps each account as a snapshot taken
// when the answer came, to the second, its raw JSON without white space;
// the snapshot file that the same answer gives (schwabfile.SnapshotLine)
// then imports as known. A refused read is recorded, and that is all.
func TestSyncSnapshotMeetsFile(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	ctx := t.Context()
	var accounts []schwab.Account
	if err := json.Unmarshal(schwabtest.Fixture(t, "accounts.json"), &accounts); err != nil {
		t.Fatal(err)
	}
	read := Read{SentAt: time.Date(2026, 11, 7, 5, 4, 59, 0, time.UTC), AnsweredAt: time.Date(2026, 11, 7, 5, 5, 0, 734e6, time.UTC)}
	rec, err := s.RecordAccounts(ctx, read, accounts)
	if err != nil || rec.ReadID == 0 || rec.New != 3 || rec.Known != 0 {
		t.Fatalf("recording the accounts gave %+v, %v", rec, err)
	}
	if got := storeRows(t, s, "SELECT endpoint, account_id, row_count, outcome FROM api_read"); !reflect.DeepEqual(got, [][]string{{"accounts", "NULL", "3", "answered"}}) {
		t.Errorf("the read is %q", got)
	}
	if got := storeRows(t, s, "SELECT DISTINCT fetched_at, source, read_id, import_id FROM snapshot"); !reflect.DeepEqual(got,
		[][]string{{"2026-11-07T05:05:00.000Z", "sync", "1", "NULL"}}) {
		t.Errorf("the snapshots are %q", got)
	}
	for _, raw := range storeStrings(t, s, "SELECT raw FROM snapshot") {
		var b bytes.Buffer
		if err := json.Compact(&b, []byte(raw)); err != nil || b.String() != raw {
			t.Errorf("a snapshot keeps white space: %.80s", raw)
		}
	}
	raws := make([]json.RawMessage, len(accounts))
	for i, a := range accounts {
		raws[i] = a.Raw
	}
	r := importFile(t, s, "snapshots.jsonl", snapshotLine(t, read.AnsweredAt, raws...))
	if r.Snapshots.New != 0 || r.Snapshots.Known != 3 {
		t.Errorf("the file of the same answer imports as %+v", r.Snapshots)
	}

	refused := Read{SentAt: read.SentAt.Add(time.Hour), AnsweredAt: read.SentAt.Add(time.Hour), Err: &schwab.Error{Status: 401, Message: "Unauthorized"}}
	rec, err = s.RecordAccounts(ctx, refused, accounts)
	if err != nil || rec.ReadID != 2 || rec.New != 0 {
		t.Errorf("recording a refused read gave %+v, %v", rec, err)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM snapshot"); n != 3 {
		t.Errorf("the store holds %d snapshots, want 3", n)
	}
	if _, err := s.RecordAccounts(ctx, read, []schwab.Account{{}}); err == nil {
		t.Error("an account without raw JSON was recorded")
	}
}

// A rebuild derives every snapshot again from its raw JSON, as importing
// it did, without writing to the snapshots or their accounts.
func TestSnapshotsRebuilt(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	fri := time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC)
	importFile(t, s, "snapshots.jsonl", snapshotLine(t, fri, fixtureAccounts(t)...)+
		snapshotAt(t, fri.Add(24*time.Hour), individual)+
		snapshotLine(t, fri.Add(48*time.Hour), json.RawMessage(`{"securitiesAccount":{"accountNumber":1}}`)))
	dump := func() map[string][][]string {
		d := map[string][][]string{}
		for _, q := range []string{
			"SELECT * FROM snapshot_fields ORDER BY snapshot_id",
			"SELECT snapshot_id, position, s.canonical_key, p.asset_type, symbol, cusip, description, long_micros, short_micros, market_value, problems " +
				"FROM snapshot_position p LEFT JOIN security s ON s.security_id = p.security_id ORDER BY snapshot_id, position",
			"SELECT * FROM market_session ORDER BY date",
			"SELECT * FROM coverage ORDER BY account_id, source",
			"SELECT * FROM account ORDER BY account_id",
			"SELECT key, kind, value, seen_in, first_seen_at, last_seen_at FROM key_name ORDER BY key, kind, value",
			"SELECT snapshot_id, code, detail FROM flag ORDER BY snapshot_id, code",
		} {
			d[q] = storeRows(t, s, q)
		}
		return d
	}
	before := dump()
	if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if after := dump(); !reflect.DeepEqual(after, before) {
		for q := range before {
			if !reflect.DeepEqual(after[q], before[q]) {
				t.Errorf("after a rebuild %s gives\n%q\nnot\n%q", q, after[q], before[q])
			}
		}
	}
}

// A newer calendar works every snapshot's market dates out again when the
// store is opened, with the sessions and each account's coverage, and
// records its version. The test stands in a calendar that closes
// 2026-11-10 and closes 2026-11-06 early, so it changes the package's
// calendar, and runs alone.
func TestCalendarChangeRecomputes(t *testing.T) {
	path := copyTemplate(t)
	s, err := Open(t.Context(), Options{Path: path, Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	pt := func(datetime string) time.Time { return timeIn(t, pst, datetime) }
	importFile(t, s, "snapshots.jsonl", snapshotAt(t, pt("2026-11-06 21:05:00"), individual)+
		snapshotAt(t, pt("2026-11-10 21:05:00"), individual)+
		snapshotAt(t, timeIn(t, nytime.NewYork(), "2026-11-10 15:45:00"), individual))
	calendarFlags := func() []string {
		return storeStrings(t, s, "SELECT subject_ref || ' ' || code || ' ' || coalesce(detail, '') FROM flags ORDER BY subject_ref, code")
	}
	if got := calendarFlags(); !slices.Equal(got, []string{"S3 in_session 2026-11-10"}) {
		t.Errorf("before the new calendar the flags are %q", got)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	old, oldVersion := calendar, calendarVersion
	t.Cleanup(func() { calendar, calendarVersion = old, oldVersion })
	closed := func(d nytime.Date) bool { return d == "2026-11-10" }
	calendar.lastClosed = func(when time.Time) (nyse.Session, error) {
		for {
			ses, err := old.lastClosed(when)
			if err != nil || !closed(ses.Date) {
				return ses, err
			}
			when = ses.Open
		}
	}
	calendar.during = func(when time.Time) (nyse.Session, bool) {
		ses, ok := old.during(when)
		return ses, ok && !closed(ses.Date)
	}
	calendar.between = func(from, to nytime.Date) ([]nyse.Session, error) {
		all, err := old.between(from, to)
		for i, s := range all {
			if s.Date == "2026-11-06" {
				all[i].Early, all[i].Close = true, s.Close.Add(-3*time.Hour)
			}
		}
		return slices.DeleteFunc(all, func(s nyse.Session) bool { return closed(s.Date) }), err
	}
	calendarVersion++
	var warned []string
	s, err = Open(t.Context(), Options{Path: path, Now: fixedNow, Warn: func(m string) { warned = append(warned, m) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if !slices.Equal(warned, []string{"recomputed market dates for calendar 2"}) {
		t.Errorf("opening the store warned %q", warned)
	}
	if got := storeStrings(t, s, "SELECT market_date || ' ' || calendar_version FROM snapshot_fields ORDER BY snapshot_id"); !slices.Equal(got,
		[]string{"2026-11-06 2", "2026-11-09 2", "2026-11-09 2"}) {
		t.Errorf("the market dates are %q", got)
	}
	if got := calendarFlags(); len(got) != 0 {
		t.Errorf("a snapshot taken on a day the new calendar closes is flagged %q", got)
	}
	if got := storeStrings(t, s, "SELECT date || ' ' || close_at || ' ' || early FROM market_session ORDER BY date"); !slices.Equal(got,
		[]string{"2026-11-06 2026-11-06T18:00:00.000Z 1", "2026-11-09 2026-11-09T21:00:00.000Z 0"}) {
		t.Errorf("the sessions are %q", got)
	}
	if got := storeRows(t, s, "SELECT from_date, to_date FROM coverage WHERE source = 'snapshot'"); !reflect.DeepEqual(got, [][]string{{"2026-11-06", "2026-11-09"}}) {
		t.Errorf("the snapshot coverage is %q", got)
	}
	if n := storeInt(t, s, "SELECT calendar_version FROM store"); n != 2 {
		t.Errorf("the store records calendar %d, want 2", n)
	}
}

// A snapshot that stands for a session past the years the calendar was
// checked for is reported, and its session is not verified.
func TestUnverifiedCalendar(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	r := importFile(t, s, "snapshots.jsonl", snapshotAt(t, time.Date(2028, 12, 29, 22, 0, 0, 0, time.UTC), individual)+
		snapshotAt(t, time.Date(2029, 1, 2, 22, 0, 0, 0, time.UTC), individual)+
		// During the first session past the checked years: it stands for
		// a checked close, but names an unchecked session.
		snapshotAt(t, timeIn(t, nytime.NewYork(), "2029-01-02 10:00:00"), options))
	want := []string{"snapshots on or after 2029-01-01 use a calendar not yet checked against NYSE's published dates; " +
		"a newer schwabtx will have it"}
	if r.Snapshots.Unverified != 2 || !slices.Equal(r.Warnings, want) {
		t.Errorf("the import reports %d unverified and warns %q", r.Snapshots.Unverified, r.Warnings)
	}
	if got := storeStrings(t, s, "SELECT date || ' ' || verified FROM sessions ORDER BY date"); !slices.Equal(got,
		[]string{"2028-12-29 1", "2029-01-02 0"}) {
		t.Errorf("the sessions are %q", got)
	}
	if got := storeStrings(t, s, "SELECT subject_ref || ' ' || code || ' ' || detail FROM flags ORDER BY subject_ref, code"); !slices.Equal(got,
		[]string{"S2 calendar_unverified 2029-01-02", "S3 calendar_unverified 2029-01-02", "S3 in_session 2029-01-02"}) {
		t.Errorf("the flags are %q", got)
	}
}

// The market sessions span the market dates of every snapshot, one that
// is no account's included, and one taken during a session too.
func TestSessionsOfAnySnapshot(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFile(t, s, "snapshots.jsonl", snapshotAt(t, timeIn(t, nytime.NewYork(), "2026-11-05 11:00:00"), individual)+
		snapshotLine(t, time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC), json.RawMessage(`{"securitiesAccount":{"accountNumber":29470135}}`)))
	if got := storeStrings(t, s, "SELECT date FROM sessions ORDER BY date"); !slices.Equal(got, []string{"2026-11-04", "2026-11-05", "2026-11-06"}) {
		t.Errorf("the sessions are %q", got)
	}
}

// Snapshots of one account at one time that differ are each flagged with
// the others, those of a later import too.
func TestSnapshotConflictsAcrossImports(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	at := time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC)
	importFile(t, s, "a.jsonl", snapshotLine(t, at, fixtureAccount(t, individual))+
		snapshotLine(t, at, fixtureAccount(t, individual, `"roundTrips": 0`, `"roundTrips": 1`)))
	importFile(t, s, "b.jsonl", snapshotLine(t, at, fixtureAccount(t, individual, `"roundTrips": 0`, `"roundTrips": 2`)))
	if got := storeStrings(t, s, "SELECT subject_ref || ' ' || detail FROM flags WHERE code = 'snapshot_conflict' ORDER BY subject_ref"); !slices.Equal(got,
		[]string{"S1 differs from S2, S3", "S2 differs from S1, S3", "S3 differs from S1, S2"}) {
		t.Errorf("the conflicts are %q", got)
	}
}

// Each position points at its security, even when the snapshot that
// holds it adds no key or name of its own: one taken between two others
// that already showed every name.
func TestPositionsPointAtSecurities(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	fri := time.Date(2026, 11, 7, 5, 5, 0, 0, time.UTC)
	importFile(t, s, "a.jsonl", snapshotAt(t, fri, options)+snapshotAt(t, fri.Add(48*time.Hour), options))
	importFile(t, s, "b.jsonl", snapshotAt(t, fri.Add(24*time.Hour), options))
	if got := storeStrings(t, s, "SELECT snapshot_id || ' ' || coalesce(security_key, '') FROM positions ORDER BY snapshot_id, position"); !slices.Equal(got, []string{
		"1 c:0XYZ..RI60045000", "1 c:30303M102", "2 c:0XYZ..RI60045000", "2 c:30303M102", "3 c:0XYZ..RI60045000", "3 c:30303M102",
	}) {
		t.Errorf("the positions point at %q", got)
	}
}

// A position moves to the security that its key joins: snapshots imported
// first point the AAPL position at its CUSIP's security, and a read of
// transactions that later joins the CUSIP to AAPL's instrumentId, whose
// key sorts first, points it at the joined security, in a write that
// leaves no position pointing at the security it removed.
func TestPositionMovesWithItsSecurity(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	importFile(t, s, "snapshots.jsonl", snapshotAt(t, day(2025, 6, 2).Add(22*time.Hour), individual))
	aapl := "SELECT coalesce(security_key, '-') FROM positions WHERE symbol = 'AAPL'"
	if got := storeStrings(t, s, aapl); !slices.Equal(got, []string{"c:037833100"}) {
		t.Fatalf("before the transactions, the AAPL position points at %q, want c:037833100", got)
	}
	if _, err := s.RecordTransactions(t.Context(), listRead(individual, fixturesStart, fixturesEnd, day(2025, 6, 3), fixtureRows(t, individual)...)); err != nil {
		t.Fatalf("recording the transactions: %v", err)
	}
	if got := storeStrings(t, s, aapl); !slices.Equal(got, []string{"i:1973757747"}) {
		t.Errorf("after the transactions, the AAPL position points at %q, want i:1973757747", got)
	}
	if got := storeStrings(t, s, "SELECT security_key FROM security_keys WHERE key = 'c:037833100'"); !slices.Equal(got, []string{"i:1973757747"}) {
		t.Errorf("the CUSIP's key is in %q, want i:1973757747", got)
	}
}

// What an import skipped is said in one line, with a hint that a newer
// schwabtx wrote a line of a newer version; a partial last line is said
// apart.
func TestSkippedWarnings(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		skipped map[string]int
		want    []string
	}{
		{nil, nil},
		{map[string]int{schwabfile.SkipNotJSON: 2, `version "1"`: 1}, []string{`3 lines skipped (2 not JSON, 1 version "1")`}},
		{map[string]int{"version 3": 1, schwabfile.SkipPartial: 1}, []string{
			"1 line skipped (1 version 3: written by a newer schwabtx?)",
			"the last line has no newline (a snapshot being written?); it was skipped",
		}},
	} {
		if got := skippedWarnings(c.skipped); !slices.Equal(got, c.want) {
			t.Errorf("skippedWarnings(%v) = %q, want %q", c.skipped, got, c.want)
		}
	}
}
