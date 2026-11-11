package schwabtest

import (
	"cmp"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A row is read leniently for its id, strictly for its time and type, and
// leniently for what the symbol filter needs.
func TestReadRow(t *testing.T) {
	at := time.Date(2026, 8, 4, 15, 22, 41, 0, time.UTC)
	base := func(changes string) string {
		return `{` + changes + `}`
	}
	for _, tc := range []struct {
		name  string
		row   Row
		id    int64 // 0 for none
		at    time.Time
		typ   string
		wrong bool
	}{
		{"an id", Row{JSON: base(`"activityId":1003,"time":"2026-08-04T15:22:41+0000","type":"TRADE"`)}, 1003, at, "TRADE", false},
		{"an id as a string", Row{JSON: base(`"activityId":"1003","time":"2026-08-04T15:22:41+0000"`)}, 1003, at, "", false},
		{"a negative id", Row{JSON: base(`"activityId":-5,"time":"2026-08-04T15:22:41Z"`)}, -5, at, "", false},
		{"an id sent twice", Row{JSON: base(`"activityId":1,"time":"2026-08-04T15:22:41Z","activityId":2`)}, 2, at, "", false},
		{"an id in another case", Row{JSON: base(`"ActivityID":7,"Time":"2026-08-04T15:22:41Z","TYPE":"JOURNAL"`)}, 7, at, "JOURNAL", false},
		{"a null id", Row{JSON: base(`"activityId":null,"time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"a fraction for an id", Row{JSON: base(`"activityId":1.5,"time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"an id in exponent form", Row{JSON: base(`"activityId":1e3,"time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"an id too big", Row{JSON: base(`"activityId":99999999999999999999,"time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"an id with a leading zero", Row{JSON: base(`"activityId":"0012","time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"an id that is a word", Row{JSON: base(`"activityId":"x","time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"no id", Row{JSON: base(`"time":"2026-08-04T15:22:41Z"`)}, 0, at, "", false},
		{"a time with +00:00", Row{JSON: base(`"time":"2026-08-04T15:22:41+00:00"`)}, 0, at, "", false},
		{"a time with -0400", Row{JSON: base(`"time":"2026-08-04T11:22:41-0400"`)}, 0, at, "", false},
		{"a time with a fraction", Row{JSON: base(`"time":"2026-08-04T15:22:41.123456789Z"`)}, 0, at.Add(123456789), "", false},
		{"Row.Time over the JSON's", Row{JSON: base(`"time":"2026-08-04T15:22:41+0000"`), Time: at.Add(time.Millisecond)}, 0, at.Add(time.Millisecond), "", false},
		{"Row.Time with no time", Row{JSON: base(`"type":"TRADE"`), Time: at}, 0, at, "TRADE", false},
		{"Row.Type over the JSON's", Row{JSON: base(`"time":"2026-08-04T15:22:41Z","type":"TRADE"`), Type: "JOURNAL"}, 0, at, "JOURNAL", false},
		{"a type that is not a string", Row{JSON: base(`"time":"2026-08-04T15:22:41Z","type":5`)}, 0, at, "", false},
		{"no time", Row{JSON: base(`"activityId":1`)}, 0, time.Time{}, "", true},
		{"a date alone", Row{JSON: base(`"time":"2026-08-04"`)}, 0, time.Time{}, "", true},
		{"a time without a zone", Row{JSON: base(`"time":"2026-08-04T15:22:41"`)}, 0, time.Time{}, "", true},
		{"a one-digit hour", Row{JSON: base(`"time":"2026-08-04T5:22:41Z"`)}, 0, time.Time{}, "", true},
		{"a comma before the fraction", Row{JSON: base(`"time":"2026-08-04T15:22:41,5Z"`)}, 0, time.Time{}, "", true},
		{"ten fractional digits", Row{JSON: base(`"time":"2026-08-04T15:22:41.1234567890Z"`)}, 0, time.Time{}, "", true},
		{"a month 13", Row{JSON: base(`"time":"2026-13-04T15:22:41Z"`)}, 0, time.Time{}, "", true},
		{"a time as a number", Row{JSON: base(`"time":5`)}, 0, time.Time{}, "", true},
		{"a list", Row{JSON: `[{"time":"2026-08-04T15:22:41Z"}]`}, 0, time.Time{}, "", true},
		{"not JSON", Row{JSON: `{"time":"2026-08-04T15:22:41Z"`}, 0, time.Time{}, "", true},
	} {
		r, err := readRow(tc.row)
		if tc.wrong {
			if err == nil {
				t.Errorf("%s: read %+v, want an error", tc.name, r)
			}
			continue
		}
		if err != nil || r.hasID != (tc.id != 0) || r.id != tc.id || !r.time.Equal(tc.at) || r.typ != tc.typ || r.json != tc.row.JSON {
			t.Errorf("%s: %+v, %v", tc.name, r, err)
		}
	}

	r, err := readRow(Row{JSON: `{"time":"2026-08-04T15:22:41Z","tradeDate":"2026-08-04T04:00:00+0000",` +
		`"description":"Ordinary Dividend~XYZ","transferItems":[{"instrument":{"symbol":"XYZ   260918C00050000",` +
		`"underlyingSymbol":"XYZ"}},5,{"instrument":{"symbol":7}},{"instrument":{"symbol":"META"}}]}`})
	if err != nil || !r.tradeDate.Equal(time.Date(2026, 8, 4, 4, 0, 0, 0, time.UTC)) || r.described != "XYZ" ||
		!slices.Equal(r.symbols, []string{"XYZ   260918C00050000", "XYZ", "META"}) {
		t.Errorf("legs and description: %+v, %v", r, err)
	}
	for _, s := range []string{
		`{"time":"2026-08-04T15:22:41Z","tradeDate":"2026-08-04","description":"Wire Incoming~","transferItems":5}`,
		`{"time":"2026-08-04T15:22:41Z","description":5,"transferItems":[{"instrument":[]}]}`,
	} {
		if r, err := readRow(Row{JSON: s}); err != nil || !r.tradeDate.IsZero() || r.described != "" || len(r.symbols) != 0 {
			t.Errorf("%s: %+v, %v", s, r, err)
		}
	}
}

// rowJSON returns a row of the given id ("" for none), time and type
// ("" for none), with more members, each a key and its JSON.
func rowJSON(id, at, typ string, more ...string) string {
	var b strings.Builder
	b.WriteString(`{"time":"` + at + `"`)
	if id != "" {
		b.WriteString(`,"activityId":` + id)
	}
	if typ != "" {
		b.WriteString(`,"type":"` + typ + `"`)
	}
	for i := 0; i+1 < len(more); i += 2 {
		b.WriteString(`,"` + more[i] + `":` + more[i+1])
	}
	b.WriteString(`}`)
	return b.String()
}

// leg returns transferItems with one leg of the given symbols.
func leg(symbol, underlying string) string {
	in := `"symbol":"` + symbol + `"`
	if underlying != "" {
		in += `,"underlyingSymbol":"` + underlying + `"`
	}
	return `[{"instrument":{` + in + `}}]`
}

// The rows of TestTransactions, by name.
var testRows = map[string]string{
	"r1":  rowJSON("1", "2026-01-10T10:00:00Z", "TRADE", "transferItems", leg("AAPL", "")),
	"r2":  rowJSON("2", "2026-01-10T10:00:00+0000", "TRADE", "transferItems", leg("XYZ   260918C00050000", "XYZ")),
	"r3":  rowJSON("3", "2026-01-05T00:00:00+0000", "DIVIDEND_OR_INTEREST", "tradeDate", `"2026-01-04T05:00:00+0000"`, "description", `"Ordinary Dividend~XYZ"`),
	"r4":  rowJSON("4", "2026-01-20T12:00:00Z", "JOURNAL"),
	"r5":  rowJSON("null", "2026-01-10T10:00:00Z", "TRADE"),
	"r6":  rowJSON("", "2026-01-15T00:00:00Z", "FOO"),
	"r7":  rowJSON(`"7"`, "2026-01-01T00:00:00Z", ""),
	"r8":  rowJSON("8", "2025-12-31T23:59:59.999Z", "ELECTRONIC_FUND"),
	"r9":  rowJSON("", "2026-01-16T00:00:00Z", "AAA"),
	"r10": rowJSON("10", "2026-01-25T00:00:00-05:00", "TRADE"),
}

// transactionsWorld returns a fake whose one account holds testRows, and a
// function that GETs the account's transactions with a query in process.
func transactionsWorld(t *testing.T) (*Server, func(query string) *httptest.ResponseRecorder) {
	t.Helper()
	s := New(t)
	auth := "Bearer " + login(t, s)
	s.AddAccount(Account{Number: "11111111", Details: `{"securitiesAccount":{"accountNumber":"11111111"}}`})
	hash := HashFor("11111111")
	for _, name := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8", "r9", "r10"} {
		s.AddRows(hash, testRows[name])
	}
	return s, func(query string) *httptest.ResponseRecorder {
		return serve(t, s, "/trader/v1/accounts/"+hash+"/transactions?"+query, auth)
	}
}

// window returns the query of a window and more parameters, in pairs.
func window(start, end string, more ...string) string {
	q := url.Values{"startDate": {start}, "endDate": {end}}
	for i := 0; i+1 < len(more); i += 2 {
		q.Set(more[i], more[i+1])
	}
	return q.Encode()
}

// rowList returns the answer that holds the named rows, in order.
func rowList(names ...string) string {
	rows := make([]string, len(names))
	for i, n := range names {
		rows[i] = testRows[n]
	}
	return "[" + strings.Join(rows, ",") + "]"
}

const (
	january = "2026-01-01T00:00:00.000Z"
	month   = "2026-01-31T00:00:00.000Z"
)

// A window's rows: by type in the spec's order, then other types in byte
// order, then rows without a type; newest first within each, a larger id
// before a smaller and a row without an id after both at one time.
func TestTransactions(t *testing.T) {
	s, txs := transactionsWorld(t)
	want := func(rec *httptest.ResponseRecorder, names ...string) {
		t.Helper()
		if body := rec.Body.String(); rec.Code != http.StatusOK || body != rowList(names...) {
			t.Errorf("%d %s, want %v", rec.Code, body, names)
		}
	}
	rec := txs(window(january, month))
	want(rec, "r10", "r2", "r1", "r5", "r3", "r4", "r9", "r6", "r7")
	wantFakeHeaders(t, rec.Header(), http.StatusOK)
	want(txs(window("2026-02-01T00:00:00Z", "2026-03-01T00:00:00Z")))
	want(txs(window(january, month, "types", "TRADE,JOURNAL")), "r10", "r2", "r1", "r5", "r4")
	want(txs(window(january, month, "types", "")), "r10", "r2", "r1", "r5", "r3", "r4", "r9", "r6", "r7")
	want(txs(window(january, month, "symbol", "XYZ")), "r2")
	want(txs(window(january, month, "symbol", "AAPL")), "r1")

	// Both bounds are taken, unless a rule leaves one out.
	want(txs(window("2025-12-31T23:59:59.999Z", "2026-01-01T00:00:00Z")), "r8", "r7")
	s.SetTransactionRules(TransactionRules{ExclusiveStart: true})
	want(txs(window("2025-12-31T23:59:59.999Z", "2026-01-01T00:00:00Z")), "r7")
	s.SetTransactionRules(TransactionRules{ExclusiveEnd: true})
	want(txs(window("2025-12-31T23:59:59.999Z", "2026-01-01T00:00:00Z")), "r8")

	s.SetTransactionRules(TransactionRules{Order: NewestFirst})
	want(txs(window(january, month)), "r10", "r4", "r9", "r6", "r2", "r1", "r5", "r3", "r7")

	s.SetTransactionRules(TransactionRules{Order: Shuffled})
	got := txs(window(january, month)).Body.String()
	if again := txs(window(january, month)).Body.String(); got != again {
		t.Errorf("Shuffled gave two orders:\n%s\n%s", got, again)
	}
	names := []string{"r10", "r2", "r1", "r5", "r3", "r4", "r9", "r6", "r7"}
	key := func(name string) uint64 {
		h := fnv.New64a()
		var r struct{ ActivityID json.RawMessage }
		_ = json.Unmarshal([]byte(testRows[name]), &r)
		id := strings.Trim(string(r.ActivityID), `"`)
		if _, err := strconv.ParseInt(id, 10, 64); err == nil {
			_, _ = h.Write([]byte(id))
		} else {
			_, _ = h.Write([]byte(testRows[name]))
		}
		return h.Sum64()
	}
	slices.SortStableFunc(names, func(a, b string) int { return cmp.Compare(key(a), key(b)) })
	if got != rowList(names...) || got == rowList("r10", "r2", "r1", "r5", "r3", "r4", "r9", "r6", "r7") {
		t.Errorf("Shuffled: %s, want %v", got, names)
	}
}

// symbol= keeps the rows whose legs name it, and with SymbolKeepsDividends
// those whose description does; IgnoreSymbol keeps every row.
func TestTransactionsSymbol(t *testing.T) {
	s, txs := transactionsWorld(t)
	for _, tc := range []struct {
		rules TransactionRules
		want  string
	}{
		{TransactionRules{}, rowList("r2")},
		{TransactionRules{SymbolKeepsDividends: true}, rowList("r2", "r3")},
		{TransactionRules{IgnoreSymbol: true}, rowList("r10", "r2", "r1", "r5", "r3", "r4", "r9", "r6", "r7")},
	} {
		s.SetTransactionRules(tc.rules)
		if got := txs(window(january, month, "symbol", "XYZ")).Body.String(); got != tc.want {
			t.Errorf("%+v: %s, want %s", tc.rules, got, tc.want)
		}
	}
}

// A window takes a row by its time, or with FilterOnTradeDate by its
// tradeDate, when the fake can read one; with UpToNow, by either, only
// up to the fake's clock.
func TestTransactionsFilterOn(t *testing.T) {
	s, txs := transactionsWorld(t)
	day := window("2026-01-04T00:00:00Z", "2026-01-04T23:59:59Z")
	if got := txs(day).Body.String(); got != "[]" {
		t.Errorf("by time: %s", got)
	}
	s.SetTransactionRules(TransactionRules{FilterOn: FilterOnTradeDate})
	if got := txs(day).Body.String(); got != rowList("r3") {
		t.Errorf("by tradeDate: %s", got)
	}
	if got := txs(window("2026-01-10T10:00:00Z", "2026-01-10T10:00:00Z")).Body.String(); got != rowList("r2", "r1", "r5") {
		t.Errorf("rows without a tradeDate, by time: %s", got)
	}
	// r3, at 2026-01-05, is dated 2026-01-04T05:00Z.
	s.SetNow(func() time.Time { return time.Date(2026, 1, 4, 12, 0, 0, 0, time.UTC) })
	s.SetTransactionRules(TransactionRules{UpToNow: true})
	if got := txs(window(january, month)).Body.String(); got != rowList("r7") {
		t.Errorf("up to the clock, by time: %s", got)
	}
	s.SetTransactionRules(TransactionRules{UpToNow: true, FilterOn: FilterOnTradeDate})
	if got := txs(window(january, month)).Body.String(); got != rowList("r3", "r7") {
		t.Errorf("up to the clock, by tradeDate: %s", got)
	}
}

// The cap keeps Cap rows: the newest across types by default, the oldest
// with KeepOldest; with InTypeOrder, the first in ByType order; with
// PerType, Cap of each type.
func TestTransactionsCap(t *testing.T) {
	s, txs := transactionsWorld(t)
	for _, tc := range []struct {
		rules TransactionRules
		want  []string
	}{
		{TransactionRules{Cap: 3}, []string{"r10", "r4", "r9"}},
		{TransactionRules{Cap: 3, KeepOldest: true}, []string{"r5", "r3", "r7"}},
		{TransactionRules{Cap: 3, Order: NewestFirst}, []string{"r10", "r4", "r9"}},
		{TransactionRules{Cap: 3, CapMode: InTypeOrder}, []string{"r10", "r2", "r1"}},
		{TransactionRules{Cap: 3, CapMode: InTypeOrder, KeepOldest: true}, []string{"r2", "r1", "r5"}},
		{TransactionRules{Cap: 6, CapMode: InTypeOrder, Order: NewestFirst}, []string{"r10", "r4", "r2", "r1", "r5", "r3"}},
		{TransactionRules{Cap: 1, CapMode: PerType}, []string{"r10", "r3", "r4", "r9", "r6", "r7"}},
		{TransactionRules{Cap: 1, CapMode: PerType, KeepOldest: true}, []string{"r5", "r3", "r4", "r9", "r6", "r7"}},
		{TransactionRules{Cap: 100}, []string{"r10", "r2", "r1", "r5", "r3", "r4", "r9", "r6", "r7"}},
	} {
		s.SetTransactionRules(tc.rules)
		if got := txs(window(january, month)).Body.String(); got != rowList(tc.want...) {
			t.Errorf("%+v: %s, want %v", tc.rules, got, tc.want)
		}
	}
}

// Without a Cap, an answer holds MaxTransactions rows: the newest, as
// Schwab's do, or the oldest with KeepOldest.
func TestTransactionsDefaultCap(t *testing.T) {
	s := New(t)
	auth := "Bearer " + login(t, s)
	s.AddAccount(Account{Number: "11111111", Details: `{}`})
	hash := HashFor("11111111")
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]string, MaxTransactions+1)
	for i := range rows {
		rows[i] = rowJSON(strconv.Itoa(i+1), start.Add(time.Duration(i)*time.Minute).Format(time.RFC3339), "TRADE")
	}
	s.AddRows(hash, rows...)
	newestFirst := func(rs []string) string {
		rs = slices.Clone(rs)
		slices.Reverse(rs)
		return "[" + strings.Join(rs, ",") + "]"
	}
	for _, tc := range []struct {
		rules TransactionRules
		want  string
	}{
		{TransactionRules{}, newestFirst(rows[1:])},
		{TransactionRules{KeepOldest: true}, newestFirst(rows[:MaxTransactions])},
	} {
		s.SetTransactionRules(tc.rules)
		rec := serve(t, s, "/trader/v1/accounts/"+hash+"/transactions?"+window(january, month), auth)
		if got := rec.Body.String(); got != tc.want {
			es, _ := elements(rec.Body.Bytes())
			t.Errorf("%+v: %d, %d rows, %.60s..., want %d of the %d, %.60s...", tc.rules, rec.Code, len(es), got, MaxTransactions, len(rows), tc.want)
		}
	}
}

// The query is checked as the package doc says, each refusal but the bare
// date's labeled as the fake's guess.
func TestTransactionsQuery(t *testing.T) {
	s, txs := transactionsWorld(t)
	refused := func(query, body string) {
		t.Helper()
		rec := txs(query)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != body {
			t.Errorf("%s: %d %s, want 400 %s", query, rec.Code, rec.Body, body)
		}
		wantFakeHeaders(t, rec.Header(), http.StatusBadRequest)
	}
	ok := func(query string) {
		t.Helper()
		if rec := txs(query); rec.Code != http.StatusOK {
			t.Errorf("%s: %d %s", query, rec.Code, rec.Body)
		}
	}
	refused(url.Values{"endDate": {month}}.Encode(), `{"message":"a guess: Schwab's answer to a request without startDate is not recorded"}`)
	refused(url.Values{"startDate": {january}}.Encode(), `{"message":"a guess: Schwab's answer to a request without endDate is not recorded"}`)
	refused(window("2024-07-26", month), `{"message":"'2024-07-26' is not a valid value for startDate"}`)
	refused(window(january, "2026-01-31"), `{"message":"'2026-01-31' is not a valid value for endDate"}`)
	for _, bad := range []string{"2026-01-01T00:00:00", "2026-01-01T00:00:00+0000", "2026-01-01T0:00:00Z", "2026-01-01 00:00:00Z", "1767225600000", `2026-01-01"`, ""} {
		refused(window(bad, month), `{"message":"a guess: Schwab's answer to this startDate is not recorded"}`)
	}
	for _, good := range []string{"2026-01-01T00:00:00Z", "2026-01-01T00:00:00.000Z", "2026-01-01T00:00:00.999Z", "2026-01-01T00:00:00+00:00", "2025-12-31T19:00:00.123-05:00"} {
		ok(window(good, month))
	}
	refused(window(month, january), `{"message":"a guess: Schwab's answer to a window that ends before it starts is not recorded"}`)
	ok(window(january, january))
	ok(window(january, "2027-01-01T00:00:00.000Z"))
	refused(window(january, "2027-01-01T00:00:00.001Z"), `{"message":"a guess: Schwab's answer to a window longer than it serves is not recorded"}`)
	s.SetTransactionRules(TransactionRules{MaxWindow: func(start time.Time) time.Time { return start.Add(24 * time.Hour) }})
	refused(window(january, "2026-01-03T00:00:00Z"), `{"message":"a guess: Schwab's answer to a window longer than it serves is not recorded"}`)
	ok(window(january, "2026-01-02T00:00:00Z"))

	s.SetTransactionRules(TransactionRules{RequireTypes: true})
	refused(window(january, month), `{"message":"a guess: a request without types was reported refused once, in 2025-12"}`)
	ok(window(january, month, "types", "TRADE"))
	s.SetTransactionRules(TransactionRules{RefuseBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	refused(window("2025-12-31T23:59:59.999Z", month), `{"message":"a guess: never seen: Schwab answers [] for a window before an account's history"}`)
	ok(window(january, month))

	// A query with several faults gets the refusal of the first check:
	// startDate, then endDate, the window, types, and RefuseBefore last.
	refused("startDate=x&endDate=y", `{"message":"a guess: Schwab's answer to this startDate is not recorded"}`)
	s.SetTransactionRules(TransactionRules{RequireTypes: true, RefuseBefore: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)})
	refused(window(month, january), `{"message":"a guess: Schwab's answer to a window that ends before it starts is not recorded"}`)
	refused(window(january, "2027-06-01T00:00:00Z"), `{"message":"a guess: Schwab's answer to a window longer than it serves is not recorded"}`)
	refused(window(january, month), `{"message":"a guess: a request without types was reported refused once, in 2025-12"}`)
}

// An account without history answers [] for every window, once its query
// is checked; an unknown hash gets Schwab's 400.
func TestTransactionsWithoutHistory(t *testing.T) {
	s := New(t)
	auth := "Bearer " + login(t, s)
	s.AddAccount(Account{Number: "11111111", Details: `{}`, NoHistory: true})
	hash := HashFor("11111111")
	s.AddRows(hash, testRows["r1"])
	if rec := serve(t, s, "/trader/v1/accounts/"+hash+"/transactions?"+window(january, month), auth); rec.Code != http.StatusOK || rec.Body.String() != "[]" {
		t.Errorf("without history: %d %s", rec.Code, rec.Body)
	}
	if rec := serve(t, s, "/trader/v1/accounts/"+hash+"/transactions?"+window("2026-01-01", month), auth); rec.Code != http.StatusBadRequest {
		t.Errorf("without history, a bare date: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/trader/v1/accounts/11111111/transactions?" + window(january, month), "/trader/v1/accounts/" + UnknownHash + "/transactions"} {
		rec := serve(t, s, path, auth)
		if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"message":"Invalid account number"}` {
			t.Errorf("%s: %d %s", path, rec.Code, rec.Body)
		}
		wantFakeHeaders(t, rec.Header(), http.StatusBadRequest)
	}
}

// transactions/{id} sends a row's body for it, or its list bytes; a row
// without an id is never found.
func TestTransactionByID(t *testing.T) {
	s, _ := transactionsWorld(t)
	auth := "Bearer " + login(t, s)
	hash := HashFor("11111111")
	byID := func(id string) *httptest.ResponseRecorder {
		return serve(t, s, "/trader/v1/accounts/"+hash+"/transactions/"+id, auth)
	}
	if rec := byID("1"); rec.Code != http.StatusOK || rec.Body.String() != testRows["r1"] {
		t.Errorf("by id: %d %s", rec.Code, rec.Body)
	}
	if rec := byID("7"); rec.Code != http.StatusOK || rec.Body.String() != testRows["r7"] {
		t.Errorf("by an id sent as a string: %d %s", rec.Code, rec.Body)
	}
	s.SetRowByID(hash, 1, `{"activityId":1,"closingPrice":2.5}`)
	if rec := byID("1"); rec.Body.String() != `{"activityId":1,"closingPrice":2.5}` {
		t.Errorf("after SetRowByID: %s", rec.Body)
	}
	for _, id := range []string{"0", "5", "99999999999"} {
		if rec := byID(id); rec.Code != http.StatusNotFound || rec.Body.String() != `{"message":"Not Found (a guess: Schwab's answer is not recorded)"}` {
			t.Errorf("id %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	for _, id := range []string{"x", "1.5", "99999999999999999999"} {
		if rec := byID(id); rec.Code != http.StatusBadRequest || rec.Body.String() != `{"message":"a guess: Schwab's answer to an id that is not a number is not recorded"}` {
			t.Errorf("id %s: %d %s", id, rec.Code, rec.Body)
		}
	}
	if rec := serve(t, s, "/trader/v1/accounts/x/transactions/1", auth); rec.Code != http.StatusBadRequest ||
		rec.Body.String() != `{"message":"Invalid account number"}` {
		t.Errorf("an unknown hash: %d %s", rec.Code, rec.Body)
	}
}

// SetTransactionRules refuses rules that are not ones, and
// TransactionRules returns what was set.
func TestSetTransactionRules(t *testing.T) {
	r := &recorder{TB: t}
	s := New(r)
	set := TransactionRules{Cap: 5, CapMode: PerType, Order: Shuffled, FilterOn: FilterOnTradeDate, KeepOldest: true}
	s.SetTransactionRules(set)
	for _, bad := range []TransactionRules{{Cap: -1}, {CapMode: PerType + 1}, {CapMode: -1}, {FilterOn: FilterOnTradeDate + 1}, {Order: Shuffled + 1}} {
		wantReported(t, r, "SetTransactionRules", func() { s.SetTransactionRules(bad) })
	}
	if got := s.TransactionRules(); got.Cap != 5 || got.CapMode != PerType || got.Order != Shuffled || got.FilterOn != FilterOnTradeDate || !got.KeepOldest {
		t.Errorf("TransactionRules() = %+v", got)
	}
}
