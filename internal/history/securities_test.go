package history

import (
	"database/sql"
	"fmt"
	"maps"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/nytime"
)

// observeAt adds keys with their names, seen at at, as a snapshot's
// positions or a file's rows will, and fails the test if it cannot.
func observeAt(t *testing.T, s *Store, at time.Time, k keyObservation, names ...nameObservation) {
	t.Helper()
	if err := s.withWrite(t.Context(), "observing a key", func(w *wtx) error {
		return observe(w, at, k, names...)
	}); err != nil {
		t.Fatal(err)
	}
}

// decideSecurity records your decision of kind security on subject k:a,
// with value.
func decideSecurity(t *testing.T, s *Store, a, value string) {
	t.Helper()
	if _, err := s.Decide(t.Context(), Decision{Kind: "security", Subject: "k:" + a, Value: value}); err != nil {
		t.Fatal(err)
	}
}

// securityOf returns the canonical key of the security that holds key,
// through the security_keys view.
func securityOf(t *testing.T, s *Store, key string) string {
	t.Helper()
	var got string
	if err := peek(s).QueryRowContext(t.Context(), "SELECT security_key FROM security_keys WHERE key = ?", key).Scan(&got); err != nil {
		t.Fatalf("the security of %s: %v", key, err)
	}
	return got
}

// symbolKey returns the key and symbol name of a position's or a file's
// observation of symbol.
func symbolKey(key, symbol string) (keyObservation, nameObservation) {
	return keyObservation{key: key, assetType: "EQUITY"}, nameObservation{kind: "symbol", value: symbol, seenIn: "position"}
}

// day returns midnight UTC of a date.
func day(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// A position's CUSIP joins a transaction's instrumentId that held its
// symbol at the time, and the security's canonical key moves from the
// CUSIP to the instrumentId, which security_keys maps the CUSIP to; the
// transaction's leg points at it. An option's deliverable that the
// option names as its underlying takes the underlyingCusip, which joins a
// position of that CUSIP however far apart in time; the option's security
// has its contract and its underlying.
func TestSecurities(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	k, n := symbolKey("c:037833100", "AAPL")
	observeAt(t, s, day(2025, 6, 1), k, n, nameObservation{kind: "cusip", value: "037833100", seenIn: "position"},
		nameObservation{kind: "description", value: "APPLE INC 12345678", seenIn: "position"})
	if got := securityOf(t, s, "c:037833100"); got != "c:037833100" {
		t.Errorf("the position alone is security %s", got)
	}
	start := time.Date(2023, 1, 1, 5, 0, 0, 0, time.UTC)
	end := time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)
	record(t, s, listRead(individual, start, end, day(2025, 6, 3), fixtureRows(t, individual)...))
	record(t, s, listRead(options, start, end, day(2025, 6, 3), fixtureRows(t, options)...))
	if got := securityOf(t, s, "c:037833100"); got != "i:1973757747" {
		t.Errorf("the position's CUSIP is in security %s, want the transaction's i:1973757747", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM security WHERE canonical_key = 'c:037833100'"); n != 0 {
		t.Errorf("the security of the CUSIP alone is still there")
	}
	if got := storeStrings(t, s, `SELECT s.canonical_key || ' ' || s.definite || ' ' || x.symbol || ' ' || x.cusip || ' ' || x.description
		FROM api_leg l JOIN security s USING (security_id) JOIN securities x ON x.security_key = s.canonical_key
		JOIN txn t USING (txn_id) WHERE t.activity_id = 73508261943`); !slices.Equal(got,
		[]string{"i:1973757747 1 AAPL 037833100 APPLE INC ...678"}) {
		t.Errorf("the AAPL leg's security: %q", got)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM api_leg WHERE asset_type <> 'CURRENCY' AND security_id IS NULL"); n != 0 {
		t.Errorf("%d legs of securities have none", n)
	}
	// The dividend's ~SYM, AAPL, names the security on the day.
	var res resolution
	var ok bool
	if err := s.withWrite(t.Context(), "resolving", func(w *wtx) error {
		names, err := loadSymbolNames(w)
		res, ok = resolveSymbol(names, "AAPL", "2025-06-02")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := storeInt(t, s, "SELECT security_id FROM security WHERE canonical_key = 'i:1973757747'"); !ok || res != (resolution{security: want}) {
		t.Errorf("AAPL on 2025-06-02 resolves to %+v, %v; want security %d", res, ok, want)
	}
	// A by-id read answered in the same millisecond as the list read, with
	// another closingPrice, writes the leg again, and names nothing new.
	aapl := fixtureRows(t, individual)[0]
	if aapl.ActivityID != 73508261943 {
		t.Fatalf("the first row is %d", aapl.ActivityID)
	}
	again := decodeRow(t, string(replaceOnce(t, aapl.Raw, `"closingPrice": 227.51`, `"closingPrice": 199.5`)))
	if _, err := s.RecordTransaction(t.Context(), TransactionRead{
		Read: readAt(day(2025, 6, 3)), Account: individual,
		ActivityID: again.ActivityID, Row: again,
	}); err != nil {
		t.Fatal(err)
	}
	if n := storeInt(t, s, "SELECT count(*) FROM api_leg WHERE asset_type <> 'CURRENCY' AND security_id IS NULL"); n != 0 {
		t.Errorf("after a read that wrote a leg again, %d legs of securities have none", n)
	}
	// A name seen in a position after a transaction is still seen in a
	// transaction first.
	observeAt(t, s, day(2025, 7, 1), keyObservation{key: "i:1973757747"}, nameObservation{kind: "symbol", value: "AAPL", seenIn: "position"})
	if got := storeStrings(t, s, "SELECT seen_in FROM key_name WHERE key = 'i:1973757747' AND kind = 'symbol'"); !slices.Equal(got, []string{"transaction"}) {
		t.Errorf("AAPL was seen in %q, want transaction", got)
	}
	// A leg whose symbol is a CUSIP, as Schwab sends a delisted security's,
	// joins a position of that CUSIP years before.
	k, n = symbolKey("c:30303M102", "OLDCO")
	observeAt(t, s, day(2019, 1, 2), k, n)
	delisted := replaceOnce(t, aapl.Raw, `"symbol": "AAPL"`, `"symbol": "30303M102"`)
	delisted = replaceOnce(t, delisted, `1973757747`, `777`)
	delisted = replaceOnce(t, delisted, `73508261943`, `9001`)
	record(t, s, listRead(individual, start, end, day(2025, 8, 1), decodeRow(t, string(delisted))))
	if got := securityOf(t, s, "c:30303M102"); got != "i:777" {
		t.Errorf("the delisted CUSIP is in security %s, want i:777", got)
	}

	// The call's deliverable, i:1854092, takes its underlyingCusip, which a
	// position held long before.
	k, n = symbolKey("c:98389B100", "XYZ")
	observeAt(t, s, day(2024, 1, 2), k, n)
	if got := securityOf(t, s, "c:98389B100"); got != "i:1854092" {
		t.Errorf("the underlying's CUSIP is in security %s, want the deliverable's i:1854092", got)
	}
	got := storeStrings(t, s, `SELECT asset_type || ' ' || put_call || ' ' || strike || ' ' || expiration_day || ' ' || multiplier
		|| ' ' || underlying_symbol || ' ' || coalesce(underlying_key, '-') FROM securities WHERE security_key = 'i:219834765'`)
	if want := "OPTION CALL 50.0 2026-09-18 100.0 XYZ i:1854092"; !slices.Equal(got, []string{want}) {
		t.Errorf("the call: %q, want %q", got, want)
	}
	// The put's contract comes from its OCC symbol and fields alike.
	got = storeStrings(t, s, `SELECT put_call || ' ' || strike || ' ' || expiration_day FROM securities WHERE security_key = 'i:219771034'`)
	if want := "PUT 40.0 2025-02-21"; !slices.Equal(got, []string{want}) {
		t.Errorf("the put: %q, want %q", got, want)
	}
}

// A ticker that two companies used at different times makes two
// securities, and a file's key of that ticker joins the one that held it
// around the export's date, however old the rows; a file's key that no
// security held near its date stays alone, and the file keys of one
// ticker that nothing resolves are one security, unless you say they
// differ. You may also say two keys are the same.
func TestSecuritiesOfFiles(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	old, oldName := symbolKey("i:100", "ZZZ")
	observeAt(t, s, day(2021, 2, 1), old, oldName)
	observeAt(t, s, day(2021, 3, 1), old, oldName)
	cur, curName := symbolKey("i:200", "ZZZ")
	observeAt(t, s, day(2025, 6, 1), cur, curName)
	observeAt(t, s, day(2026, 6, 1), cur, curName)
	file := func(symbol string, d time.Time) string {
		key := "s:FILE:" + symbol + "@" + d.Format(time.DateOnly)
		k, n := symbolKey(key, symbol)
		n.seenIn = "history_file"
		observeAt(t, s, d.Add(15*time.Hour), k, n)
		return key
	}
	for key, want := range map[string]string{
		file("ZZZ", day(2026, 6, 5)):  "i:200",
		file("ZZZ", day(2021, 3, 5)):  "i:100",
		file("ZZZ", day(2023, 7, 1)):  "s:FILE:ZZZ@2023-07-01",
		file("QQQX", day(2026, 1, 2)): "s:FILE:QQQX@2026-01-02",
		file("QQQX", day(2026, 3, 4)): "s:FILE:QQQX@2026-01-02",
	} {
		if got := securityOf(t, s, key); got != want {
			t.Errorf("%s is in security %s, want %s", key, got, want)
		}
	}
	if n := storeInt(t, s, "SELECT definite FROM security WHERE canonical_key = 's:FILE:QQQX@2026-01-02'"); n != 0 {
		t.Errorf("the unresolved ticker's security is definite")
	}
	decideSecurity(t, s, "s:FILE:QQQX@2026-03-04", "differ k:s:FILE:QQQX@2026-01-02")
	if got := securityOf(t, s, "s:FILE:QQQX@2026-03-04"); got != "s:FILE:QQQX@2026-03-04" {
		t.Errorf("after differ, the second QQQX file is in security %s", got)
	}
	decideSecurity(t, s, "s:FILE:QQQX@2026-03-04", "auto")
	if got := securityOf(t, s, "s:FILE:QQQX@2026-03-04"); got != "s:FILE:QQQX@2026-01-02" {
		t.Errorf("after auto, the second QQQX file is in security %s", got)
	}
	decideSecurity(t, s, "s:FILE:ZZZ@2023-07-01", "same k:i:200")
	if got := securityOf(t, s, "s:FILE:ZZZ@2023-07-01"); got != "i:200" {
		t.Errorf("after same, the 2023 ZZZ file is in security %s", got)
	}
}

// The window of the reads of the fixtures' transactions.
var (
	fixturesStart = time.Date(2023, 1, 1, 5, 0, 0, 0, time.UTC)
	fixturesEnd   = time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC)
)

// recordFixtures records the fixtures' transactions, those of the
// individual account read in June 2025 and those of the other in August
// 2026.
func recordFixtures(t *testing.T, s *Store) {
	t.Helper()
	record(t, s, listRead(individual, fixturesStart, fixturesEnd, day(2025, 6, 3), fixtureRows(t, individual)...))
	record(t, s, listRead(options, fixturesStart, fixturesEnd, day(2026, 8, 5), fixtureRows(t, options)...))
}

// The keys and the grouping depend on the evidence alone: files' keys
// first and transactions after, or the other way round, give the same
// securities.
func TestSecuritiesInAnyOrder(t *testing.T) {
	t.Parallel()
	api := func(s *Store) { recordFixtures(t, s) }
	files := func(s *Store) {
		for _, f := range []struct {
			symbol string
			at     time.Time
		}{{"AAPL", day(2025, 6, 6)}, {"BND", day(2025, 6, 1)}, {"META", day(2026, 8, 1)}, {"XYZ", day(2026, 8, 7)}, {"OLD", day(2020, 1, 1)}} {
			k, n := symbolKey("s:FILE:"+f.symbol+"@"+f.at.Format(time.DateOnly), f.symbol)
			n.seenIn = "history_file"
			observeAt(t, s, f.at, k, n)
		}
	}
	dump := func(s *Store) []string {
		return storeStrings(t, s, "SELECT key || ' ' || security_key FROM security_keys ORDER BY key")
	}
	a := newStore(t, Options{})
	files(a)
	api(a)
	b := newStore(t, Options{})
	api(b)
	files(b)
	if da, db := dump(a), dump(b); !slices.Equal(da, db) {
		t.Errorf("files first:\n%s\ntransactions first:\n%s", strings.Join(da, "\n"), strings.Join(db, "\n"))
	}
	for _, want := range []string{
		"s:FILE:AAPL@2025-06-06 i:1973757747", "s:FILE:META@2026-08-01 i:1940083512",
		"s:FILE:XYZ@2026-08-07 i:1854092", "s:FILE:OLD@2020-01-01 s:FILE:OLD@2020-01-01",
	} {
		if !slices.Contains(dump(a), want) {
			t.Errorf("security_keys lacks %q", want)
		}
	}
}

// A rebuild of a store of transactions gives the keys, names, fields and
// securities that recording them gave: from the raw JSON the store keeps,
// each revision's as of when it was first seen and the latest as of when
// it was seen.
func TestSecuritiesRebuilt(t *testing.T) {
	t.Parallel()
	c := newStore(t, Options{})
	recordFixtures(t, c)
	// A later read of the call, another day, with its deliverable's
	// description changed.
	call := fixtureRows(t, options)[0]
	call = decodeRow(t, string(replaceOnce(t, replaceOnce(t, call.Raw, "XYZ CORP COMMON STOCK", "XYZ CORPORATION"),
		`"type": "VANILLA"`, `"type": "STANDARD"`)))
	record(t, c, listRead(options, fixturesStart, fixturesEnd, day(2026, 8, 20), call))
	derived := func() []string {
		return slices.Concat(
			storeStrings(t, c, `SELECT key || ' ' || coalesce(asset_type, '') || ' ' || coalesce(type, '') || ' ' ||
				coalesce(put_call, '') || ' ' || coalesce(strike, '') || ' ' || coalesce(expiration_day, '') || ' ' ||
				coalesce(multiplier, '') || ' ' || coalesce(underlying_symbol, '') || ' ' || first_seen_at || ' ' ||
				last_seen_at FROM instrument_key ORDER BY key`),
			storeStrings(t, c, `SELECT key || ' ' || kind || ' ' || value || ' ' || seen_in || ' ' || first_seen_at || ' ' ||
				last_seen_at FROM key_name ORDER BY key, kind, value`),
			storeStrings(t, c, `SELECT security_key || ' ' || definite || ' ' || coalesce(symbol, '') || ' ' ||
				coalesce(description, '') || ' ' || coalesce(underlying_key, '') || ' ' || first_seen_at || ' ' ||
				last_seen_at FROM securities ORDER BY security_key`),
			storeStrings(t, c, `SELECT t.activity_id || ' ' || l.leg || ' ' || coalesce(s.canonical_key, '') FROM api_leg l
				JOIN txn t USING (txn_id) LEFT JOIN security s USING (security_id) ORDER BY 1`))
	}
	before := derived()
	if err := c.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if after := derived(); !slices.Equal(before, after) {
		t.Errorf("recorded:\n%s\nrebuilt:\n%s", strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	if !slices.ContainsFunc(before, func(s string) bool {
		return strings.HasPrefix(s, "i:1854092 description XYZ CORP COMMON STOCK deliverable")
	}) {
		t.Errorf("the deliverable's first description is not among its names")
	}
	// A key's fields are its latest observation's, whatever the order
	// they came in.
	observeAt(t, c, day(2020, 1, 1), keyObservation{key: "i:219834765", typ: "OLDER"})
	if got := storeStrings(t, c, "SELECT type FROM instrument_key WHERE key = 'i:219834765'"); !slices.Equal(got, []string{"STANDARD"}) {
		t.Errorf("the call's type is %q, want its latest, STANDARD", got)
	}
}

// A key's fields are its latest observation's, whole: when that leaves a
// field out, the key has none, though an earlier observation gave one, so
// that the order in which transactions were read, which a rebuild does
// not see, cannot decide the field. Of observations at one time, the
// greater value of each field holds.
func TestKeyFieldsRebuilt(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	meta := fixtureRows(t, options)[1]
	if meta.ActivityID != 73508261942 {
		t.Fatalf("the fixture's second row of the options account is %d, want 73508261942", meta.ActivityID)
	}
	without := decodeRow(t, string(replaceOnce(t, replaceOnce(t, meta.Raw, `, "type": "COMMON_STOCK"`, ``), `73508261942`, `9001`)))
	with := decodeRow(t, string(replaceOnce(t, meta.Raw, `73508261942`, `9002`)))
	fields := func() []string {
		return slices.Concat(
			storeStrings(t, s, "SELECT key || ' ' || coalesce(type, '-') FROM instrument_key WHERE key = 'i:1940083512'"),
			storeStrings(t, s, "SELECT security_key || ' ' || coalesce(type, '-') FROM securities WHERE security_key = 'i:1940083512'"))
	}
	record(t, s, listRead(options, fixturesStart, fixturesEnd, day(2026, 8, 1), without))
	record(t, s, listRead(options, fixturesStart, fixturesEnd, day(2026, 8, 2), with))
	if got, want := fields(), []string{"i:1940083512 COMMON_STOCK", "i:1940083512 COMMON_STOCK"}; !slices.Equal(got, want) {
		t.Errorf("after the read with a type: %q, want %q", got, want)
	}
	record(t, s, listRead(options, fixturesStart, fixturesEnd, day(2026, 8, 3), without))
	want := []string{"i:1940083512 -", "i:1940083512 -"}
	if got := fields(); !slices.Equal(got, want) {
		t.Errorf("after a later read without it: %q, want %q", got, want)
	}
	if err := s.withWrite(t.Context(), "rebuilding", rebuild); err != nil {
		t.Fatal(err)
	}
	if got := fields(); !slices.Equal(got, want) {
		t.Errorf("rebuilt: %q, want %q as recorded", got, want)
	}
	// Two observations at one time, in either order.
	at := day(2026, 9, 1)
	observeAt(t, s, at, keyObservation{key: "i:1940083512", typ: "B"})
	observeAt(t, s, at, keyObservation{key: "i:1940083512", typ: "A", assetType: "EQUITY"})
	if got := storeStrings(t, s, "SELECT coalesce(asset_type, '-') || ' ' || type FROM instrument_key WHERE key = 'i:1940083512'"); !slices.Equal(got, []string{"EQUITY B"}) {
		t.Errorf("of two observations at one time, the fields are %q, want the greater of each, EQUITY B", got)
	}
}

// A transaction's instruments give keys and names: an instrumentId of 1,
// which is not an instrument's own, keys a leg by its asset type and
// symbol and keys no deliverable; an option's underlyingCusip names the
// one deliverable whose symbol is its underlying symbol, and none when two
// deliverables are.
func TestObserveInstruments(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	row := decodeRow(t, `{"activityId": 1, "time": "2026-10-01T14:00:00+0000", "type": "TRADE", "transferItems": [
		{"instrument": {"assetType": "EQUITY", "symbol": "ONE", "instrumentId": 1}, "amount": 1},
		{"instrument": {"assetType": "OPTION", "symbol": "XYZ   260918C00050000", "instrumentId": 500,
		 "underlyingSymbol": "XYZ", "underlyingCusip": "111111111", "optionDeliverables": [
		 {"deliverable": {"assetType": "EQUITY", "symbol": "OTHER", "instrumentId": 502}},
		 {"deliverable": {"assetType": "EQUITY", "symbol": "XYZ", "instrumentId": 501}},
		 {"deliverable": {"assetType": "EQUITY", "symbol": "XYZ", "instrumentId": 1}}]}, "amount": 1},
		{"instrument": {"assetType": "OPTION", "symbol": "ABC   260918C00050000", "instrumentId": 600,
		 "underlyingSymbol": "ABC", "underlyingCusip": "222222222", "optionDeliverables": [
		 {"deliverable": {"assetType": "EQUITY", "symbol": "ABC", "instrumentId": 601}},
		 {"deliverable": {"assetType": "EQUITY", "symbol": "ABC", "instrumentId": 602}}]}, "amount": 1}]}`)
	if err := s.withWrite(t.Context(), "observing a transaction", func(w *wtx) error {
		return observeTxn(w, row, day(2026, 10, 2))
	}); err != nil {
		t.Fatal(err)
	}
	if got := storeStrings(t, s, "SELECT key FROM instrument_key ORDER BY key"); !slices.Equal(got,
		[]string{"i:500", "i:501", "i:502", "i:600", "i:601", "i:602", "s:EQUITY:ONE"}) {
		t.Errorf("the keys are %q", got)
	}
	if got := storeStrings(t, s, "SELECT key || ' ' || value FROM key_name WHERE kind = 'cusip' ORDER BY key"); !slices.Equal(got,
		[]string{"i:501 111111111"}) {
		t.Errorf("the CUSIP names are %q, want the underlying's alone", got)
	}
}

// The grouping on values: links by a CUSIP, by a symbol that one key of
// each kind held at once (the keys of one symbol in several exports being
// one), and none when two keys of one kind held it then,
// or when the link would put two keys you said differ in one security,
// though neither is the one linked; a security that holds a key of
// Schwab's is definite, whichever its other keys; the groups and their
// canonical keys do not depend on the order of the keys and names.
func TestGroupKeys(t *testing.T) {
	t.Parallel()
	at := func(m time.Month) time.Time { return time.Date(2026, m, 1, 0, 0, 0, 0, time.UTC) }
	key := func(k string) keyInfo { return keyInfo{key: k, first: at(1), last: at(12)} }
	name := func(k, kind, v string, from, to time.Month) nameInfo {
		return nameInfo{key: k, kind: kind, value: v, first: at(from), last: at(to)}
	}
	keys := []keyInfo{
		key("i:1"), key("c:111111111"), key("x:9"), key("i:2"), key("i:3"), key("c:222222222"),
		key("s:EQUITY:LONE"), key("s:FILE:AMB@2026-05-01"), key("i:4"), key("c:333333333"),
		key("i:5"), key("c:555555555"), key("x:7"), key("i:7"), key("s:FILE:EEE@2026-05-01"), key("x:8"),
		key("x:6"), key("s:FILE:FFF@2026-02-01"), key("s:FILE:FFF@2026-03-01"),
	}
	names := []nameInfo{
		name("i:1", "symbol", "AAA", 1, 6),
		name("c:111111111", "symbol", "AAA", 6, 6), // joins i:1 by symbol
		name("x:9", "symbol", "AAA", 3, 3),         // and so does an ItemIssueId
		// Two instrumentIds held BBB at once: neither joins the CUSIP.
		name("i:2", "symbol", "BBB", 1, 12),
		name("i:3", "symbol", "BBB", 1, 12),
		name("c:222222222", "symbol", "BBB", 5, 5),
		// A CUSIP name joins its c: key, however far apart in time.
		name("i:3", "cusip", "222222222", 1, 1),
		name("s:FILE:AMB@2026-05-01", "symbol", "AMB", 5, 5),
		// Held months apart: no link.
		name("i:4", "symbol", "CCC", 1, 2),
		name("c:333333333", "symbol", "CCC", 11, 11),
		// i:5 holds a CUSIP and the symbol DDD, which x:7 holds too; you
		// said x:7 differs from i:5's CUSIP, so the symbol does not link
		// them.
		name("i:5", "cusip", "555555555", 1, 1),
		name("i:5", "symbol", "DDD", 1, 12),
		name("x:7", "symbol", "DDD", 5, 5),
		// A file's symbol joins the instrumentId that held it.
		name("i:7", "symbol", "EEE", 1, 12),
		name("s:FILE:EEE@2026-05-01", "symbol", "EEE", 5, 5),
		// Two exports' keys of FFF each join the ItemIssueId that both
		// exports held: they are one file symbol, not two holders of it.
		name("x:6", "symbol", "FFF", 2, 3),
		name("s:FILE:FFF@2026-02-01", "symbol", "FFF", 2, 2),
		name("s:FILE:FFF@2026-03-01", "symbol", "FFF", 3, 3),
	}
	links := []keyLink{{a: "c:555555555", b: "x:7"}}
	want := [][]string{
		{"i:1", "c:111111111", "x:9"},
		{"i:2"},
		{"i:3", "c:222222222"},
		{"i:4"},
		{"i:5", "c:555555555"},
		{"i:7", "s:FILE:EEE@2026-05-01"},
		{"c:333333333"},
		{"x:6", "s:FILE:FFF@2026-02-01", "s:FILE:FFF@2026-03-01"},
		{"x:7"},
		{"x:8"},
		{"s:EQUITY:LONE"},
		{"s:FILE:AMB@2026-05-01"},
	}
	groups := func(keys []keyInfo, names []nameInfo) [][]string {
		var out [][]string
		for _, g := range groupKeys(keys, names, links) {
			out = append(out, g.keys)
		}
		return out
	}
	definite := map[string]bool{}
	for _, g := range groupKeys(keys, names, links) {
		definite[g.keys[0]] = g.definite
	}
	if want := map[string]bool{
		"i:1": true, "i:2": true, "i:3": true, "i:4": true, "i:5": true, "i:7": true, "c:333333333": true,
		"x:6": true, "x:7": true, "x:8": true, "s:EQUITY:LONE": false, "s:FILE:AMB@2026-05-01": false,
	}; !maps.Equal(definite, want) {
		t.Errorf("the groups are definite %v, want %v", definite, want)
	}
	if got := groups(keys, names); !reflect.DeepEqual(got, want) {
		t.Fatalf("groupKeys: %q, want %q", got, want)
	}
	r := rand.New(rand.NewPCG(3, 4))
	for range 20 {
		k, n := slices.Clone(keys), slices.Clone(names)
		r.Shuffle(len(k), func(i, j int) { k[i], k[j] = k[j], k[i] })
		r.Shuffle(len(n), func(i, j int) { n[i], n[j] = n[j], n[i] })
		if got := groups(k, n); !reflect.DeepEqual(got, want) {
			t.Fatalf("with its inputs in another order, groupKeys gives %q", got)
		}
	}
}

// An option's contract comes from the instrumentId seen last, the
// greater of two seen last at once, else from the least OCC symbol among
// its names; its underlying is the one security that held its underlying
// symbol while the option was seen, and none when two did. A security's
// asset type and type are its canonical key's, and it was seen from the
// first time one of its keys was to the last.
func TestGroupAttributes(t *testing.T) {
	t.Parallel()
	at := func(m time.Month) time.Time { return time.Date(2026, m, 1, 0, 0, 0, 0, time.UTC) }
	option := func(k, putCall, under string, last time.Month) keyInfo {
		return keyInfo{
			key: k, assetType: "OPTION", putCall: putCall, strike: sql.NullFloat64{Float64: 50, Valid: true},
			underlyingSymbol: under, first: at(1), last: at(last),
		}
	}
	keys := []keyInfo{
		// One contract under two ids, joined by a CUSIP: the later says PUT.
		option("i:10", "CALL", "AAA", 3), option("i:11", "PUT", "AAA", 4),
		option("i:20", "CALL", "BBB", 12),
		{key: "s:OPTION:XYZ   260918C00050000", assetType: "OPTION", first: at(1), last: at(1)},
		{key: "i:1", first: at(1), last: at(12)},
		{key: "i:2", first: at(1), last: at(12)},
		{key: "i:3", first: at(1), last: at(12)},
	}
	names := []nameInfo{
		{key: "i:10", kind: "cusip", value: "0XYZ..RI", first: at(1), last: at(1)},
		{key: "i:11", kind: "cusip", value: "0XYZ..RI", first: at(1), last: at(1)},
		{key: "s:OPTION:XYZ   260918C00050000", kind: "symbol", value: "XYZ   260918C00050000", first: at(1), last: at(1)},
		{key: "i:1", kind: "symbol", value: "AAA", first: at(1), last: at(12)},
		{key: "i:2", kind: "symbol", value: "BBB", first: at(1), last: at(12)},
		{key: "i:3", kind: "symbol", value: "BBB", first: at(1), last: at(12)},
	}
	keys = append(keys, keyInfo{key: "c:0XYZ..RI", first: at(1), last: at(1)})
	early, late := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2020, 3, 1, 0, 0, 0, 0, time.UTC)
	keys = append(keys,
		// Two ids of one contract seen last at once: the greater's.
		option("i:50", "CALL", "", 6), option("i:51", "PUT", "", 6),
		// An option on OLD, which one company held in 2020 and another
		// while the option was seen.
		option("i:40", "CALL", "OLD", 6),
		keyInfo{key: "i:41", first: early, last: late},
		keyInfo{key: "i:42", first: at(1), last: at(12)},
		// An option known by two OCC symbols, the greater first.
		keyInfo{key: "s:OPTION:ZZZ", assetType: "OPTION", first: at(1), last: at(1)},
		// A stock whose CUSIP's position says otherwise of it.
		keyInfo{key: "i:30", assetType: "EQUITY", typ: "COMMON_STOCK", first: at(3), last: at(5)},
		keyInfo{key: "c:300000000", assetType: "COLLECTIVE_INVESTMENT", typ: "ETF", first: at(1), last: at(9)},
	)
	names = append(names,
		nameInfo{key: "i:50", kind: "cusip", value: "0ZZZ..RI", first: at(1), last: at(1)},
		nameInfo{key: "i:51", kind: "cusip", value: "0ZZZ..RI", first: at(1), last: at(1)},
		nameInfo{key: "i:41", kind: "symbol", value: "OLD", first: early, last: late},
		nameInfo{key: "i:42", kind: "symbol", value: "OLD", first: at(1), last: at(12)},
		nameInfo{key: "s:OPTION:ZZZ", kind: "symbol", value: "ZZZ   270918C00060000", first: at(1), last: at(1)},
		nameInfo{key: "s:OPTION:ZZZ", kind: "symbol", value: "ZZZ   260918C00050000", first: at(1), last: at(1)},
		nameInfo{key: "i:30", kind: "cusip", value: "300000000", first: at(3), last: at(3)},
	)
	keys = append(keys, keyInfo{key: "c:0ZZZ..RI", first: at(1), last: at(1)})
	got := map[string]string{}
	for _, g := range groupKeys(keys, names, nil) {
		got[g.keys[0]] = fmt.Sprintf("%s %v %s %s %s", g.putCall, g.strike.Float64, g.expiration, g.underlyingSymbol, g.underlying)
		if g.keys[0] == "i:30" {
			got["i:30"] = fmt.Sprintf("%s %s %s %s", g.assetType, g.typ, instant(g.first), instant(g.last))
		}
	}
	for k, want := range map[string]string{
		"i:10":                           "PUT 50  AAA i:1",
		"i:20":                           "CALL 50  BBB ",
		"s:OPTION:XYZ   260918C00050000": "CALL 50 2026-09-18 XYZ ",
		"i:50":                           "PUT 50   ",
		"i:40":                           "CALL 50  OLD i:42",
		"s:OPTION:ZZZ":                   "CALL 50 2026-09-18 ZZZ ",
		"i:30":                           "EQUITY COMMON_STOCK " + instant(at(1)) + " " + instant(at(9)),
	} {
		if got[k] != want {
			t.Errorf("security %s: %q, want %q", k, got[k], want)
		}
	}
}

// A ~SYM names the one security that the account traded within 7 days of
// the day and that has ever held the symbol: none when it traded two such,
// or traded the one only further away.
func TestTradedNear(t *testing.T) {
	t.Parallel()
	names := []symbolName{
		{security: 1, value: "BND", first: day(2026, 1, 1), last: day(2026, 1, 1)},
		{security: 2, value: "TWO", first: day(2026, 1, 1), last: day(2026, 1, 1)},
		{security: 3, value: "TWO", first: day(2026, 1, 1), last: day(2026, 1, 1)},
	}
	traded := []tradedSecurity{{"2024-04-04", 1}, {"2024-04-04", 9}, {"2024-05-01", 2}, {"2024-05-03", 3}, {"2024-06-01", 2}}
	for _, c := range []struct {
		symbol string
		day    nytime.Date
		want   int64
		ok     bool
	}{
		{"BND", "2024-04-04", 1, true},
		{"BND", "2024-03-28", 1, true},
		{"BND", "2024-04-11", 1, true},
		{"BND", "2024-04-12", 0, false},
		{"TWO", "2024-05-02", 0, false},
		{"TWO", "2024-06-03", 2, true},
		{"NONE", "2024-04-04", 0, false},
	} {
		if got, ok := tradedNear(traded, names, c.symbol, c.day); got != c.want || ok != c.ok {
			t.Errorf("tradedNear(%q, %s) = %d, %v; want %d, %v", c.symbol, c.day, got, ok, c.want, c.ok)
		}
	}
}

// The symbols that a dividend's ~SYM is looked up among are each key's
// symbols and uniform symbols, which Schwab writes otherwise at times.
func TestSymbolNames(t *testing.T) {
	t.Parallel()
	s := newStore(t, Options{})
	observeAt(t, s, day(2026, 5, 1), keyObservation{key: "i:900", assetType: "EQUITY"},
		nameObservation{kind: "symbol", value: "BRKB", seenIn: "transaction"},
		nameObservation{kind: "uniform_symbol", value: "BRK.B", seenIn: "transaction"},
		nameObservation{kind: "description", value: "BERKSHIRE", seenIn: "transaction"})
	var got []string
	if err := s.withWrite(t.Context(), "reading the names", func(w *wtx) error {
		names, err := loadSymbolNames(w)
		got = nil
		for _, n := range names {
			got = append(got, n.value)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"BRK.B", "BRKB"}) {
		t.Errorf("the symbol names are %q, want BRK.B and BRKB", got)
	}
}

// A ~SYM names the security that held the symbol within 7 days of the
// transaction's day; failing that, the one security that ever held it, if
// it held it within a year, by its name only; else none, as when two
// companies held the ticker, or one held it years before. An option is
// named by its OCC symbol.
func TestResolveSymbol(t *testing.T) {
	t.Parallel()
	names := []symbolName{
		{security: 1, value: "XYZ", first: day(2021, 1, 1), last: day(2021, 6, 1)},
		{security: 2, value: "XYZ", first: day(2025, 1, 1), last: day(2026, 6, 1)},
		{security: 3, value: "ABC", first: day(2020, 1, 1), last: day(2020, 2, 1)},
		{security: 4, value: "XYZ   260918C00050000", first: day(2026, 8, 1), last: day(2026, 8, 1)},
		{security: 5, value: "DUP", first: day(2026, 1, 1), last: day(2026, 3, 1)},
		{security: 6, value: "DUP", first: day(2026, 2, 1), last: day(2026, 2, 1)},
		// One security held TWO under two keys, the later first.
		{security: 7, value: "TWO", first: day(2026, 2, 1), last: day(2026, 3, 1)},
		{security: 7, value: "TWO", first: day(2025, 11, 1), last: day(2025, 12, 1)},
		// Seen 7 days and 12 hours after the start of 2026-05-01: within 7
		// days of the day's end.
		{security: 8, value: "EDGE", first: midnight("2026-05-08").Add(12 * time.Hour), last: midnight("2026-05-08").Add(12 * time.Hour)},
	}
	for _, c := range []struct {
		symbol string
		day    nytime.Date
		want   resolution
		ok     bool
	}{
		{"XYZ", "2021-03-01", resolution{security: 1}, true},
		{"XYZ", "2026-06-07", resolution{security: 2}, true},
		{"XYZ", "2023-06-01", resolution{}, false},
		{"ABC", "2020-06-01", resolution{security: 3, byName: true, seen: day(2020, 2, 1)}, true},
		{"ABC", "2022-06-01", resolution{}, false},
		{"NONE", "2026-01-01", resolution{}, false},
		{"DUP", "2026-02-01", resolution{}, false},
		{"XYZ   260918C00050000", "2026-08-03", resolution{security: 4}, true},
		{"XYZ   260918P00050000", "2026-08-03", resolution{}, false},
		{"TWO", "2026-06-01", resolution{security: 7, byName: true, seen: day(2026, 3, 1)}, true},
		{"EDGE", "2026-05-01", resolution{security: 8}, true},
	} {
		got, ok := resolveSymbol(names, c.symbol, c.day)
		if got != c.want || ok != c.ok {
			t.Errorf("resolveSymbol(%q, %s) = %+v, %v; want %+v, %v", c.symbol, c.day, got, ok, c.want, c.ok)
		}
	}
}
