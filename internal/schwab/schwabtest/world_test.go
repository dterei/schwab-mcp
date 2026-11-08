package schwabtest

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// Fatalf keeps the message, as Errorf does, and ends the goroutine, as
// Fatalf does.
func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

// fatal runs f on a goroutine of its own, which a Fatalf may end, and
// returns what r took.
func fatal(r *recorder, f func()) []string {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	<-done
	return r.took()
}

// loggedIn returns a fake with a login, and a function that GETs path
// from its API in process.
func loggedIn(t *testing.T, tb testing.TB) (*Server, func(path string) *httptest.ResponseRecorder) {
	t.Helper()
	s := New(tb)
	auth := "Bearer " + login(t, s)
	return s, func(path string) *httptest.ResponseRecorder { return serve(t, s, path, auth) }
}

// wantAnswer checks that rec is a 200 with body, as the fake built it.
func wantAnswer(t *testing.T, rec *httptest.ResponseRecorder, body string) {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Body.String() != body {
		t.Errorf("%d %s, want 200 %s", rec.Code, rec.Body, body)
	}
	wantFakeHeaders(t, rec.Header(), http.StatusOK)
}

// wantInvalid checks that rec is Schwab's answer to an account it does not
// know.
func wantInvalid(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"message":"Invalid account number"}` {
		t.Errorf("%d %s, want Invalid account number", rec.Code, rec.Body)
	}
	wantFakeHeaders(t, rec.Header(), http.StatusBadRequest)
}

// Two accounts, one with positions, laid out as Schwab does, and served
// byte for byte, & and U+2028 and 250.0 as they are.
var (
	details1 = "{\n  \"securitiesAccount\": {\n    \"type\": \"MARGIN\",\n    \"accountNumber\": \"11111111\",\n" +
		"    \"positions\": [\n      {\"instrument\": {\"symbol\": \"T\", \"description\": \"AT&T" + string(rune(0x2028)) + "INC\"}, \"longQuantity\": 250.0}\n    ],\n" +
		"    \"initialBalances\": {\"cashBalance\": 0.0}\n  },\n  \"aggregatedBalance\": {\"liquidationValue\": 250.0}\n}"
	details1Bare = "{\n  \"securitiesAccount\": {\n    \"type\": \"MARGIN\",\n    \"accountNumber\": \"11111111\",\n" +
		"    \"initialBalances\": {\"cashBalance\": 0.0}\n  },\n  \"aggregatedBalance\": {\"liquidationValue\": 250.0}\n}"
	details2  = `{"securitiesAccount":{"type":"CASH","accountNumber":"22222222"}}`
	numbers2  = `{ "accountNumber" : "22222222", "hashValue" : "H2", "x": [1] }`
	numbers1  = `{"accountNumber":"11111111","hashValue":"` + HashFor("11111111") + `"}`
	positions = "/trader/v1/accounts?fields=positions"
)

// The account endpoints answer from the accounts as given, in the order
// they were added, positions cut out unless asked for.
func TestAccounts(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.AddAccount(Account{Number: "11111111", Details: details1})
	s.AddAccount(Account{Number: "22222222", Hash: "H2", Details: details2, NumberJSON: numbers2})
	wantAnswer(t, get("/trader/v1/accounts/accountNumbers"), "["+numbers1+","+numbers2+"]")
	wantAnswer(t, get("/trader/v1/accounts"), "["+details1Bare+","+details2+"]")
	wantAnswer(t, get(positions), "["+details1+","+details2+"]")
	wantAnswer(t, get("/trader/v1/accounts?fields=positions,orders"), "["+details1+","+details2+"]")
	wantAnswer(t, get("/trader/v1/accounts?fields=orders"), "["+details1Bare+","+details2+"]")
	wantAnswer(t, get("/trader/v1/accounts/"+HashFor("11111111")+"?fields=positions"), details1)
	wantAnswer(t, get("/trader/v1/accounts/"+HashFor("11111111")), details1Bare)
	wantAnswer(t, get("/trader/v1/accounts/H2"), details2)
	wantInvalid(t, get("/trader/v1/accounts/11111111"))
	wantInvalid(t, get("/trader/v1/accounts/"+UnknownHash))

	for _, tc := range []struct {
		a    Account
		want string
	}{
		{Account{Details: details2}, "needs a Number"},
		{Account{Number: "33333333", Details: `[]`}, "not a JSON object"},
		{Account{Number: "33333333", Details: `{"a":}`}, "not a JSON object"},
		{Account{Number: "33333333", Details: `{}`, NumberJSON: `{`}, "NumberJSON is not JSON"},
		{Account{Number: "11111111", Details: `{}`}, "already in the world"},
		{Account{Number: "33333333", Hash: "H2", Details: `{}`}, "has the hash H2"},
	} {
		wantReported(t, r, tc.want, func() { s.AddAccount(tc.a) })
	}
	wantAnswer(t, get("/trader/v1/accounts/accountNumbers"), "["+numbers1+","+numbers2+"]")

	s.SetDetails("H2", `{"securitiesAccount":{"type":"MARGIN"}}`)
	wantAnswer(t, get("/trader/v1/accounts"), "["+details1Bare+`,{"securitiesAccount":{"type":"MARGIN"}}]`)
	wantReported(t, r, "SetDetails(H3)", func() { s.SetDetails("H3", `{}`) })
	wantReported(t, r, "not a JSON object", func() { s.SetDetails("H2", `[]`) })

	_, getEmpty := loggedIn(t, t)
	wantAnswer(t, getEmpty("/trader/v1/accounts/accountNumbers"), "[]")
	wantAnswer(t, getEmpty("/trader/v1/accounts"), "[]")
}

// An account removed is no longer listed, its paths get Schwab's 400, and
// its rows and their handles go with it.
func TestRemoveAccount(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.AddAccount(Account{Number: "11111111", Details: details1})
	s.AddAccount(Account{Number: "22222222", Hash: "H2", Details: details2})
	ref := s.AddRow("H2", Row{JSON: testRows["r1"]})
	s.RemoveAccount("H2")
	wantAnswer(t, get("/trader/v1/accounts/accountNumbers"), "["+numbers1+"]")
	wantInvalid(t, get("/trader/v1/accounts/H2"))
	wantInvalid(t, get("/trader/v1/accounts/H2/transactions?"+window(january, month)))
	wantReported(t, r, "its account was removed", func() { ref.Remove() })
	wantReported(t, r, "its account was removed", func() { ref.Replace(Row{JSON: testRows["r2"]}) })
	wantReported(t, r, "RemoveAccount(H2)", func() { s.RemoveAccount("H2") })
	s.AddAccount(Account{Number: "22222222", Hash: "H2", Details: details2})
	wantAnswer(t, get("/trader/v1/accounts/H2/transactions?"+window(january, month)), "[]")
}

// Rehash gives an account a new hash, keeping its place, its rows and
// their handles, and splices the hash into the NumberJSON it was given.
func TestRehash(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.AddAccount(Account{Number: "11111111", Details: details1})
	s.AddAccount(Account{Number: "22222222", Hash: "H2", Details: details2, NumberJSON: numbers2})
	s.AddAccount(Account{Number: "33333333", Hash: "H3", Details: `{}`, NumberJSON: `{"accountNumber":"33333333","hashValue":3}`})
	ref := s.AddRow("H2", Row{JSON: testRows["r1"]})
	s.Rehash("H2", "NEW")
	s.Rehash(HashFor("11111111"), "ONE")
	wantAnswer(t, get("/trader/v1/accounts/accountNumbers"),
		`[{"accountNumber":"11111111","hashValue":"ONE"},{ "accountNumber" : "22222222", "hashValue" : "NEW", "x": [1] },{"accountNumber":"33333333","hashValue":3}]`)
	wantInvalid(t, get("/trader/v1/accounts/H2"))
	wantInvalid(t, get("/trader/v1/accounts/H2/transactions?"+window(january, month)))
	wantAnswer(t, get("/trader/v1/accounts/NEW"), details2)
	wantAnswer(t, get("/trader/v1/accounts/NEW/transactions?"+window(january, month)), rowList("r1"))
	ref.Replace(Row{JSON: testRows["r2"]})
	wantAnswer(t, get("/trader/v1/accounts/NEW/transactions?"+window(january, month)), rowList("r2"))

	wantReported(t, r, "no hashValue string", func() { s.Rehash("H3", "H4") })
	wantAnswer(t, get("/trader/v1/accounts/H3"), `{}`)
	wantReported(t, r, "no account has the hash H2", func() { s.Rehash("H2", "H5") })
	wantReported(t, r, "empty or another account's", func() { s.Rehash("NEW", "ONE") })
	wantReported(t, r, "empty or another account's", func() { s.Rehash("NEW", "") })
}

// Rows are added, replaced and removed through their handles, or by id,
// each keeping its place, and every bad call is refused with nothing
// changed.
func TestRows(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.AddAccount(Account{Number: "11111111", Details: `{}`})
	hash := HashFor("11111111")
	list := func() string {
		t.Helper()
		rec := get("/trader/v1/accounts/" + hash + "/transactions?" + window(january, month))
		return rec.Body.String()
	}
	byID := func(id string) string {
		t.Helper()
		return get("/trader/v1/accounts/" + hash + "/transactions/" + id).Body.String()
	}
	a := `{"time":"2026-01-10T10:00:00Z","n":"a"}`
	b := `{"time":"2026-01-10T10:00:00Z","n":"b"}`
	refs := s.AddRows(hash, a, b, testRows["r1"])
	if len(refs) != 3 || list() != "["+testRows["r1"]+","+a+","+b+"]" {
		t.Fatalf("added %d: %s", len(refs), list())
	}
	a2 := `{"time":"2026-01-10T10:00:00Z","n":"a2"}`
	refs[0].Replace(Row{JSON: a2})
	if got := list(); got != "["+testRows["r1"]+","+a2+","+b+"]" {
		t.Errorf("after Replace, the row moved: %s", got)
	}
	refs[1].Remove()
	wantReported(t, r, "it was removed", func() { refs[1].Remove() })
	wantReported(t, r, "it was removed", func() { refs[1].Replace(Row{JSON: b}) })
	if got := list(); got != "["+testRows["r1"]+","+a2+"]" {
		t.Errorf("after Remove: %s", got)
	}

	s.SetRowByID(hash, 1, `{"by":"id"}`)
	if got := byID("1"); got != `{"by":"id"}` {
		t.Errorf("by id: %s", got)
	}
	refs[2].Replace(Row{JSON: `{"activityId":1,"time":"2026-01-11T00:00:00Z","new":true}`})
	if got := byID("1"); got != `{"activityId":1,"time":"2026-01-11T00:00:00Z","new":true}` {
		t.Errorf("after Replace, by id: %s", got)
	}
	s.ReplaceRow(hash, 1, `{"activityId":2,"time":"2026-01-12T00:00:00Z"}`)
	if got := byID("1"); got != `{"activityId":2,"time":"2026-01-12T00:00:00Z"}` {
		t.Errorf("ReplaceRow did not keep the id given: %s", got)
	}
	if rec := get("/trader/v1/accounts/" + hash + "/transactions/2"); rec.Code != http.StatusNotFound {
		t.Errorf("the replaced row is found by the id its bytes hold: %d", rec.Code)
	}

	wantReported(t, r, "already has a row with the id 1", func() { s.AddRow(hash, Row{JSON: testRows["r1"]}) })
	wantReported(t, r, "row 1: the account already has a row with the id 3", func() {
		s.AddRows(hash, testRows["r3"], testRows["r3"])
	})
	wantReported(t, r, "row 1: a row needs a time", func() { s.AddRows(hash, testRows["r4"], `{"activityId":5}`) })
	if rec := get("/trader/v1/accounts/" + hash + "/transactions/4"); rec.Code != http.StatusNotFound {
		t.Errorf("a refused AddRows added a row: %d %s", rec.Code, rec.Body)
	}
	wantReported(t, r, "no account has the hash x", func() {
		if ref := s.AddRow("x", Row{JSON: a}); ref != (RowRef{}) {
			t.Errorf("a refused row has a handle: %+v", ref)
		}
	})
	other := s.AddRow(hash, Row{JSON: testRows["r4"]})
	wantReported(t, r, "another row of the account has the id 1", func() {
		other.Replace(Row{JSON: `{"activityId":1,"time":"2026-01-01T00:00:00Z"}`})
	})
	wantReported(t, r, "a row needs a time", func() { other.Replace(Row{JSON: `{}`}) })
	wantReported(t, r, "no row with the id 9", func() { s.ReplaceRow(hash, 9, a) })
	wantReported(t, r, "a row must be a JSON object", func() { s.ReplaceRow(hash, 1, `[]`) })
	wantReported(t, r, "no row with the id 9", func() { s.RemoveRow(hash, 9) })
	wantReported(t, r, "no row with the id 9", func() { s.SetRowByID(hash, 9, `{}`) })
	wantReported(t, r, "the body is not JSON", func() { s.SetRowByID(hash, 1, `{`) })
	s.RemoveRow(hash, 4)
	if rec := get("/trader/v1/accounts/" + hash + "/transactions/4"); rec.Code != http.StatusNotFound {
		t.Errorf("after RemoveRow: %d", rec.Code)
	}
	wantReported(t, r, "it was removed", func() { other.Remove() })

	defer func() {
		if recover() == nil {
			t.Error("a RowRef that AddRow did not return was used")
		}
	}()
	RowRef{}.Remove()
}

// userPreference sends what SetUserPreference set, or a body built from
// the accounts with every key Schwab sends on every record of its kind.
func TestUserPreference(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.AddAccount(Account{Number: "11111111", Details: `{}`})
	s.AddAccount(Account{Number: "12", Details: `{}`})
	rec := get("/trader/v1/userPreference")
	wantFakeHeaders(t, rec.Header(), http.StatusOK)
	var got struct {
		Accounts     []map[string]any `json:"accounts"`
		StreamerInfo []map[string]any `json:"streamerInfo"`
		Offers       []map[string]any `json:"offers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Accounts) != 2 || len(got.StreamerInfo) != 1 || len(got.Offers) != 1 {
		t.Fatalf("%s (%v)", rec.Body, err)
	}
	for i, want := range []map[string]any{
		{"accountNumber": "11111111", "primaryAccount": true, "type": "BROKERAGE", "nickName": "Account 1", "displayAcctId": "...111", "autoPositionEffect": false, "accountColor": "Green"},
		{"accountNumber": "12", "primaryAccount": false, "type": "BROKERAGE", "nickName": "Account 2", "displayAcctId": "...12", "autoPositionEffect": false, "accountColor": "Green"},
	} {
		if !maps.Equal(got.Accounts[i], want) {
			t.Errorf("account %d: %v", i, got.Accounts[i])
		}
	}
	if want := (map[string]any{
		"streamerSocketUrl": "wss://streamer-api.schwab.com/ws", "schwabClientCustomerId": StreamerCustomerID,
		"schwabClientCorrelId": StreamerCorrelID, "schwabClientChannel": "N9", "schwabClientFunctionId": "APIAPP",
	}); !maps.Equal(got.StreamerInfo[0], want) {
		t.Errorf("streamer info: %v", got.StreamerInfo[0])
	}
	if want := (map[string]any{"level2Permissions": true, "mktDataPermission": "NP"}); !maps.Equal(got.Offers[0], want) {
		t.Errorf("offers: %v", got.Offers[0])
	}

	s.SetUserPreference(`{"accounts":[] }`)
	wantAnswer(t, get("/trader/v1/userPreference"), `{"accounts":[] }`)
	wantReported(t, r, "not JSON", func() { s.SetUserPreference(`{`) })
	wantAnswer(t, get("/trader/v1/userPreference"), `{"accounts":[] }`)
	s.SetUserPreference("")
	if got := get("/trader/v1/userPreference").Body.String(); !strings.Contains(got, `"nickName":"Account 1"`) {
		t.Errorf("after SetUserPreference(\"\"): %s", got)
	}
}

// LoadFixtures serves the hand-made fixtures as they are: every element
// of each list, every row in a window of its account, transaction.json by
// its id, and the preferences.
func TestLoadFixtures(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	s.LoadFixtures()
	elementsOf := func(b []byte) []string {
		t.Helper()
		es, err := elements(b)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(es))
		for i, e := range es {
			out[i] = string(e)
		}
		return out
	}
	numbers := elementsOf(Fixture(t, "account_numbers.json"))
	wantAnswer(t, get("/trader/v1/accounts/accountNumbers"), "["+strings.Join(numbers, ",")+"]")
	accounts := elementsOf(Fixture(t, "accounts.json"))
	wantAnswer(t, get(positions), "["+strings.Join(accounts, ",")+"]")
	bare := get("/trader/v1/accounts").Body.String()
	if strings.Contains(bare, `"positions"`) || !json.Valid([]byte(bare)) || !strings.Contains(bare, accounts[2]) {
		t.Errorf("the accounts without positions: %.300s", bare)
	}
	wantAnswer(t, get("/trader/v1/userPreference"), string(Fixture(t, "user_preference.json")))

	var rows []struct {
		ActivityID    int64  `json:"activityId"`
		AccountNumber string `json:"accountNumber"`
	}
	all := elementsOf(Fixture(t, "transactions.json"))
	if err := json.Unmarshal(Fixture(t, "transactions.json"), &rows); err != nil {
		t.Fatal(err)
	}
	for i, a := range FixtureAccounts {
		var want []string
		for i, row := range rows {
			if row.AccountNumber == a.Number {
				want = append(want, all[i])
			}
		}
		var got []string
		for start := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC); start.Year() < 2027; start = start.AddDate(1, 0, 0) {
			end := start.AddDate(1, 0, 0).Add(-time.Millisecond)
			rec := get("/trader/v1/accounts/" + a.Hash + "/transactions?" + window(start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano)))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %s", a.Number, rec.Code, rec.Body)
			}
			got = append(got, elementsOf(rec.Body.Bytes())...)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("account %s: served %d rows, want its %d", a.Number, len(got), len(want))
		}
		wantAnswer(t, get("/trader/v1/accounts/"+a.Hash+"?fields=positions"), accounts[i])
	}
	for i, row := range rows {
		hash := HashFor(row.AccountNumber)
		want := all[i]
		if row.ActivityID == 73508261946 {
			want = string(Fixture(t, "transaction.json"))
		}
		wantAnswer(t, get("/trader/v1/accounts/"+hash+"/transactions/"+strconv.FormatInt(row.ActivityID, 10)), want)
	}
	if errs := fatal(r, s.LoadFixtures); len(errs) != 1 || !strings.Contains(errs[0], "already in the world") {
		t.Errorf("LoadFixtures twice: %q", errs)
	}
}

// A recording answers the requests it recorded, method, path and query
// exactly, with only its recorded headers, ahead of the world and after
// the handlers, each request's answers in turn and the last again, and a
// later day's after an earlier day's.
func TestLoadRecording(t *testing.T) {
	r := &recorder{TB: t}
	s, get := loggedIn(t, r)
	fsys := fstest.MapFS{
		"recorded/2026-11-08/001.txt":  {Data: []byte(" \n")},
		"recorded/2026-11-08/002.json": {Data: []byte(`{"ok":"recorded"}`)},
		"recorded/2026-11-08/003.json": {Data: []byte(`{"quotes":{}}`)},
		"recorded/2026-11-09/001.json": {Data: []byte(`[]`)},
		"recorded/2026-11-09/002.json": {Data: []byte(`{"day":2}`)},
	}
	entry := func(file, path, query string, status int, headers map[string][]string, date string) Entry {
		var e Entry
		e.File, e.Source, e.Recorded, e.Scrub = file, "recorded", date, "abc keep"
		e.Request.Method, e.Request.Path, e.Request.Query = http.MethodGet, path, query
		e.Response.Status, e.Response.Headers = status, headers
		return e
	}
	index := []Entry{
		entry("recorded/2026-11-08/001.txt", "/trader/v1/test", "a=1", 429,
			map[string][]string{"Schwab-Client-Correlid": {"00000000-0000-4000-8000-000000000042"}}, "2026-11-08"),
		entry("recorded/2026-11-08/002.json", "/trader/v1/test", "a=1", 200,
			map[string][]string{"Content-Type": {"application/json;charset=UTF-8"}}, "2026-11-08"),
		entry("recorded/2026-11-08/003.json", "/marketdata/v1/quotes", "symbols=X", 200, nil, "2026-11-08"),
		entry("recorded/2026-11-09/001.json", "/trader/v1/test", "a=1", 200, nil, "2026-11-09"),
		entry("recorded/2026-11-09/002.json", "/trader/v1/test", "a=1", 200, nil, "2026-11-09"),
	}
	if err := s.loadRecording(fsys, index, "2026-11-08"); err != nil {
		t.Fatal(err)
	}
	rec := get("/trader/v1/test?a=1")
	if want := (http.Header{"Schwab-Client-Correlid": {"00000000-0000-4000-8000-000000000042"}}); rec.Code != 429 ||
		rec.Body.String() != " \n" || !maps.EqualFunc(rec.Header(), want, equalValues) {
		t.Errorf("the first answer: %d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	for range 2 {
		if rec := get("/trader/v1/test?a=1"); rec.Code != http.StatusOK || rec.Body.String() != `{"ok":"recorded"}` ||
			rec.Header().Get("Content-Type") != "application/json;charset=UTF-8" || rec.Header().Get("X-Request-Id") != "" {
			t.Errorf("the second answer, and after it: %d %v %s", rec.Code, rec.Header(), rec.Body)
		}
	}
	if rec := get("/marketdata/v1/quotes?symbols=X"); rec.Code != http.StatusOK || rec.Body.String() != `{"quotes":{}}` {
		t.Errorf("a path without a Route: %d %s", rec.Code, rec.Body)
	}
	wantAnswer(t, get("/trader/v1/test?a=2"), `{"ok":true}`)
	wantAnswer(t, get("/trader/v1/test?a=1&b=2"), `{"ok":true}`)
	s.Script(Answer(http.StatusTeapot, ""))
	if rec := get("/trader/v1/test?a=1"); rec.Code != http.StatusTeapot {
		t.Errorf("the script came after the recording: %d", rec.Code)
	}
	// A second day's answers to the same request follow the first day's,
	// whether those ran out before it was loaded or not.
	answers := func(from func(string) *httptest.ResponseRecorder, n int) []string {
		var bodies []string
		for range n {
			bodies = append(bodies, from("/trader/v1/test?a=1").Body.String())
		}
		return bodies
	}
	if err := s.loadRecording(fsys, index, "2026-11-09"); err != nil {
		t.Fatal(err)
	}
	if got, want := answers(get, 3), []string{`[]`, `{"day":2}`, `{"day":2}`}; !slices.Equal(got, want) {
		t.Errorf("a second day loaded after the first day's answers ran out: %q, want %q", got, want)
	}
	both, getBoth := loggedIn(t, r)
	for _, date := range []string{"2026-11-08", "2026-11-09"} {
		if err := both.loadRecording(fsys, index, date); err != nil {
			t.Fatal(err)
		}
	}
	if got, want := answers(getBoth, 5), []string{" \n", `{"ok":"recorded"}`, `[]`, `{"day":2}`, `{"day":2}`}; !slices.Equal(got, want) {
		t.Errorf("two days loaded before a request: %q, want %q", got, want)
	}

	for _, tc := range []struct {
		date  string
		index []Entry
		want  string
	}{
		{"2026-11-10", index, "no file is recorded on that day"},
		{"2026-11", index, "cannot parse"},
		{"2026-11-08", []Entry{entry("recorded/2026-11-09/001.json", "/trader/v1/test", "", 200, nil, "2026-11-08")}, "is not under recorded/2026-11-08/"},
		{"2026-11-08", []Entry{entry("recorded/2026-11-08/009.json", "/trader/v1/test", "", 200, nil, "2026-11-08")}, "009.json"},
	} {
		if err := s.loadRecording(fsys, tc.index, tc.date); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error that says %q", tc.date, err, tc.want)
		}
	}
	if errs := fatal(r, func() { s.LoadRecording("2026-11-08") }); len(errs) != 1 || !strings.Contains(errs[0], "no file is recorded on that day") {
		t.Errorf("LoadRecording with no recordings: %q", errs)
	}
}
