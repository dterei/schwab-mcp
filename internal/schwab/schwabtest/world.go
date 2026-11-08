package schwabtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// An Account is one of the fake's accounts, as the bytes it serves for it.
type Account struct {
	Number string // accountNumber
	Hash   string // hashValue; HashFor(Number) when empty
	// Details is the account's element of GET /accounts, a JSON object,
	// with positions inside securitiesAccount when it holds any. Without
	// fields=positions they are cut out of the answer.
	Details string
	// NumberJSON is the account's element of GET /accounts/accountNumbers;
	// one is built from Number and Hash when it is empty.
	NumberJSON string
	// NoHistory makes every window of the account's transactions answer
	// [], as for an account without thinkorswim, though its rows stay.
	NoHistory bool
}

// account is an Account in the world, with its rows.
type account struct {
	Account
	serial int // which account it is, whatever its hash
	rows   []*row
}

// numberJSON returns the account's element of GET /accounts/accountNumbers.
func (a *account) numberJSON() string {
	if a.NumberJSON != "" {
		return a.NumberJSON
	}
	return `{"accountNumber":` + string(jsonString(a.Number)) + `,"hashValue":` + string(jsonString(a.Hash)) + `}`
}

// details returns the account's element of GET /accounts, with its
// positions or without.
func (a *account) details(positions bool) []byte {
	if positions {
		return []byte(a.Details)
	}
	return withoutPositions([]byte(a.Details))
}

// invalidAccountBody is Schwab's answer to an account it does not know.
var invalidAccountBody = mustFixture("errors/invalid_account_400.json")

// invalidAccount returns Schwab's answer to an account it does not know.
// It needs the lock.
func (s *Server) invalidAccount() answer { return s.built(http.StatusBadRequest, invalidAccountBody) }

// byHash returns the account with the given hash, or nil. It needs the
// lock.
func (s *Server) byHash(hash string) *account {
	for _, a := range s.accounts {
		if a.Hash == hash {
			return a
		}
	}
	return nil
}

// bySerial returns the account with the given serial, or nil. It needs the
// lock.
func (s *Server) bySerial(serial int) *account {
	for _, a := range s.accounts {
		if a.serial == serial {
			return a
		}
	}
	return nil
}

// wantsPositions reports whether a query asks for positions: fields, a
// list split by commas, names them. The spec gives no other field; what
// Schwab does with another is not known, and it is answered as none.
func wantsPositions(q url.Values) bool {
	return slices.Contains(strings.Split(q.Get("fields"), ","), "positions")
}

// list returns the JSON list of es, as they are.
func list(es [][]byte) []byte {
	return slices.Concat([]byte("["), bytes.Join(es, []byte(",")), []byte("]"))
}

// accountNumbers answers GET /accounts/accountNumbers. It needs the lock.
func (s *Server) accountNumbers() answer {
	var es [][]byte
	for _, a := range s.accounts {
		es = append(es, []byte(a.numberJSON()))
	}
	return s.built(http.StatusOK, list(es))
}

// accountList answers GET /accounts. It needs the lock.
func (s *Server) accountList(positions bool) answer {
	var es [][]byte
	for _, a := range s.accounts {
		es = append(es, a.details(positions))
	}
	return s.built(http.StatusOK, list(es))
}

// oneAccount answers GET /accounts/{hash}. It needs the lock.
func (s *Server) oneAccount(hash string, positions bool) answer {
	a := s.byHash(hash)
	if a == nil {
		return s.invalidAccount()
	}
	return s.built(http.StatusOK, a.details(positions))
}

// userPreference answers GET /userPreference: what SetUserPreference set,
// or else a body built from the accounts with every key that Schwab sends
// on every record of its kind. It needs the lock.
func (s *Server) userPreference() answer {
	if s.preference != "" {
		return s.built(http.StatusOK, []byte(s.preference))
	}
	var b bytes.Buffer
	b.WriteString(`{"accounts":[`)
	for i, a := range s.accounts {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"accountNumber":%s,"primaryAccount":%t,"type":"BROKERAGE","nickName":%s,"displayAcctId":%s,`+
			`"autoPositionEffect":false,"accountColor":"Green"}`,
			jsonString(a.Number), i == 0, jsonString("Account "+strconv.Itoa(i+1)), jsonString("..."+a.Number[max(0, len(a.Number)-3):]))
	}
	fmt.Fprintf(&b, `],"streamerInfo":[{"streamerSocketUrl":"wss://streamer-api.schwab.com/ws","schwabClientCustomerId":%s,`+
		`"schwabClientCorrelId":%s,"schwabClientChannel":"N9","schwabClientFunctionId":"APIAPP"}],`+
		`"offers":[{"level2Permissions":true,"mktDataPermission":"NP"}]}`,
		jsonString(StreamerCustomerID), jsonString(StreamerCorrelID))
	return s.built(http.StatusOK, b.Bytes())
}

// addAccount adds a to the world. It needs the lock.
func (s *Server) addAccount(a Account) error {
	if a.Hash == "" {
		a.Hash = HashFor(a.Number)
	}
	switch {
	case a.Number == "":
		return errors.New("an account needs a Number")
	case !isObject(a.Details):
		return fmt.Errorf("account %s: its Details are not a JSON object", a.Number)
	case a.NumberJSON != "" && !json.Valid([]byte(a.NumberJSON)):
		return fmt.Errorf("account %s: its NumberJSON is not JSON", a.Number)
	case slices.ContainsFunc(s.accounts, func(b *account) bool { return b.Number == a.Number }):
		return fmt.Errorf("account %s is already in the world", a.Number)
	case s.byHash(a.Hash) != nil:
		return fmt.Errorf("account %s: another account has the hash %s", a.Number, a.Hash)
	}
	s.serials++
	s.accounts = append(s.accounts, &account{Account: a, serial: s.serials})
	return nil
}

// AddAccount adds an account to the world, after the others.
func (s *Server) AddAccount(a Account) {
	s.t.Helper()
	s.mu.Lock()
	err := s.addAccount(a)
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: AddAccount: %v", err)
	}
}

// SetDetails replaces the Details of the account with the given hash,
// which keeps its place, its hash and its rows.
func (s *Server) SetDetails(hash, details string) {
	s.t.Helper()
	s.mu.Lock()
	a := s.byHash(hash)
	ok := a != nil && isObject(details)
	if ok {
		a.Details = details
	}
	s.mu.Unlock()
	if !ok {
		s.t.Errorf("schwabtest: SetDetails(%s): no such account, or the details are not a JSON object", hash)
	}
}

// RemoveAccount takes the account with the given hash out of the world,
// with its rows, as when it is no longer shared: its paths then get 400
// Invalid account number.
func (s *Server) RemoveAccount(hash string) {
	s.t.Helper()
	s.mu.Lock()
	n := len(s.accounts)
	s.accounts = slices.DeleteFunc(s.accounts, func(a *account) bool { return a.Hash == hash })
	gone := len(s.accounts) < n
	s.mu.Unlock()
	if !gone {
		s.t.Errorf("schwabtest: RemoveAccount(%s): no such account", hash)
	}
}

// Rehash gives the account with oldHash the hash newHash, as Schwab is
// reported to do on a new login. The account keeps its place, its rows and
// its RowRefs, and the hashValue string in a NumberJSON that was given is
// replaced; one without a hashValue string fails the test. The old hash
// then gets 400 Invalid account number.
func (s *Server) Rehash(oldHash, newHash string) {
	s.t.Helper()
	s.mu.Lock()
	err := func() error {
		a := s.byHash(oldHash)
		switch {
		case a == nil:
			return fmt.Errorf("no account has the hash %s", oldHash)
		case newHash == "" || s.byHash(newHash) != nil:
			return fmt.Errorf("the hash %q is empty or another account's", newHash)
		}
		if a.NumberJSON != "" {
			b, ok := withString([]byte(a.NumberJSON), "hashValue", newHash)
			if !ok {
				return errors.New("its NumberJSON has no hashValue string")
			}
			a.NumberJSON = string(b)
		}
		a.Hash = newHash
		return nil
	}()
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: Rehash(%s, %s): %v", oldHash, newHash, err)
	}
}

// SetUserPreference sets the body of GET /userPreference; "" builds one
// from the accounts again, with nicknames Account 1, Account 2, and so on.
func (s *Server) SetUserPreference(body string) {
	s.t.Helper()
	if body != "" && !json.Valid([]byte(body)) {
		s.t.Errorf("schwabtest: SetUserPreference: the body is not JSON")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preference = body
}

// addRows reads rs and adds them to the account with the given hash, all
// or none. It needs the lock.
func (s *Server) addRows(hash string, rs ...Row) ([]RowRef, error) {
	a := s.byHash(hash)
	if a == nil {
		return nil, fmt.Errorf("no account has the hash %s", hash)
	}
	ids := map[int64]bool{}
	for _, r := range a.rows {
		if r.hasID {
			ids[r.id] = true
		}
	}
	var read []*row
	for i, r := range rs {
		x, err := readRow(r)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", i, err)
		}
		if x.hasID && ids[x.id] {
			return nil, fmt.Errorf("row %d: the account already has a row with the id %d", i, x.id)
		}
		if x.hasID {
			ids[x.id] = true
		}
		read = append(read, x)
	}
	refs := make([]RowRef, len(read))
	for i, x := range read {
		s.serials++
		x.serial = s.serials
		a.rows = append(a.rows, x)
		refs[i] = RowRef{s: s, account: a.serial, row: x.serial}
	}
	return refs, nil
}

// AddRow adds a row to the account with the given hash, and returns a
// handle on it. A row whose activityId is an integer, or a string holding
// one, has that id, which no other row of the account may have; any other
// row has none, and is served in lists, never by id.
func (s *Server) AddRow(hash string, r Row) RowRef {
	s.t.Helper()
	s.mu.Lock()
	refs, err := s.addRows(hash, r)
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: AddRow: %v", err)
		return RowRef{}
	}
	return refs[0]
}

// AddRows adds rows, each the JSON of one, to the account with the given
// hash, as AddRow does with their Time and Type read from the JSON, and
// returns handles on them. If one cannot be added, none is.
func (s *Server) AddRows(hash string, rows ...string) []RowRef {
	s.t.Helper()
	rs := make([]Row, len(rows))
	for i, r := range rows {
		rs[i] = Row{JSON: r}
	}
	s.mu.Lock()
	refs, err := s.addRows(hash, rs...)
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: AddRows: %v", err)
	}
	return refs
}

// find returns the row ref names and its account, or why there is none. It
// needs the lock.
func (r RowRef) find() (*account, int, error) {
	a := r.s.bySerial(r.account)
	if a == nil {
		return nil, 0, errors.New("its account was removed")
	}
	i := slices.IndexFunc(a.rows, func(x *row) bool { return x.serial == r.row })
	if i < 0 {
		return nil, 0, errors.New("it was removed")
	}
	return a, i, nil
}

// Replace replaces the row with next, read as AddRow reads a row. The row
// keeps its place among the rows added; a body SetRowByID gave it goes.
func (r RowRef) Replace(next Row) {
	if r.s == nil {
		panic("schwabtest: Replace on a RowRef that AddRow did not return")
	}
	r.s.t.Helper()
	r.s.mu.Lock()
	err := func() error {
		a, i, err := r.find()
		if err != nil {
			return err
		}
		x, err := readRow(next)
		if err != nil {
			return err
		}
		if x.hasID && slices.ContainsFunc(a.rows, func(y *row) bool { return y != a.rows[i] && y.hasID && y.id == x.id }) {
			return fmt.Errorf("another row of the account has the id %d", x.id)
		}
		x.serial = r.row
		a.rows[i] = x
		return nil
	}()
	r.s.mu.Unlock()
	if err != nil {
		r.s.t.Errorf("schwabtest: RowRef.Replace: %v", err)
	}
}

// Remove takes the row out of its account.
func (r RowRef) Remove() {
	if r.s == nil {
		panic("schwabtest: Remove on a RowRef that AddRow did not return")
	}
	r.s.t.Helper()
	r.s.mu.Lock()
	a, i, err := r.find()
	if err == nil {
		a.rows = slices.Delete(a.rows, i, i+1)
	}
	r.s.mu.Unlock()
	if err != nil {
		r.s.t.Errorf("schwabtest: RowRef.Remove: %v", err)
	}
}

// byID returns the row with the given id of the account with the given
// hash, or why there is none. It needs the lock.
func (s *Server) byID(hash string, id int64) (*account, int, error) {
	a := s.byHash(hash)
	if a == nil {
		return nil, 0, fmt.Errorf("no account has the hash %s", hash)
	}
	i := slices.IndexFunc(a.rows, func(r *row) bool { return r.hasID && r.id == id })
	if i < 0 {
		return nil, 0, fmt.Errorf("the account has no row with the id %d", id)
	}
	return a, i, nil
}

// ReplaceRow replaces the row with the given id by the JSON next, keeping
// its place. The row keeps the id given, whatever the new bytes say; the
// rest is read from them as AddRow reads a row. A body SetRowByID gave it
// goes.
func (s *Server) ReplaceRow(hash string, id int64, next string) {
	s.t.Helper()
	s.mu.Lock()
	err := func() error {
		a, i, err := s.byID(hash, id)
		if err != nil {
			return err
		}
		x, err := readRow(Row{JSON: next})
		if err != nil {
			return err
		}
		x.id, x.hasID, x.serial = id, true, a.rows[i].serial
		a.rows[i] = x
		return nil
	}()
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: ReplaceRow: %v", err)
	}
}

// RemoveRow takes the row with the given id out of its account.
func (s *Server) RemoveRow(hash string, id int64) {
	s.t.Helper()
	s.mu.Lock()
	a, i, err := s.byID(hash, id)
	if err == nil {
		a.rows = slices.Delete(a.rows, i, i+1)
	}
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: RemoveRow: %v", err)
	}
}

// setRowByID sets what transactions/{id} sends for a row. It needs the
// lock.
func (s *Server) setRowByID(hash string, id int64, body string) error {
	if !json.Valid([]byte(body)) {
		return errors.New("the body is not JSON")
	}
	a, i, err := s.byID(hash, id)
	if err != nil {
		return err
	}
	a.rows[i].byID = body
	return nil
}

// SetRowByID sets what GET /accounts/{hash}/transactions/{id} sends for the
// row with the given id, which is otherwise the row as the list sends it.
func (s *Server) SetRowByID(hash string, id int64, body string) {
	s.t.Helper()
	s.mu.Lock()
	err := s.setRowByID(hash, id, body)
	s.mu.Unlock()
	if err != nil {
		s.t.Errorf("schwabtest: SetRowByID: %v", err)
	}
}

// LoadFixtures fills the world with the hand-made fixtures: the accounts of
// account_numbers.json with their elements of accounts.json, the rows of
// transactions.json under the account each names, transaction.json as what
// its row sends by id, and user_preference.json.
func (s *Server) LoadFixtures() {
	s.t.Helper()
	s.mu.Lock()
	err := s.loadFixtures()
	s.mu.Unlock()
	if err != nil {
		s.t.Fatalf("schwabtest: LoadFixtures: %v", err)
	}
}

// loadFixtures is LoadFixtures. It needs the lock.
func (s *Server) loadFixtures() error {
	type numbered struct {
		AccountNumber string `json:"accountNumber"`
		HashValue     string `json:"hashValue"`
	}
	read := func(name string) ([][]byte, error) {
		es, err := elements(mustFixture(name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return es, nil
	}
	numbers, err := read("account_numbers.json")
	if err != nil {
		return err
	}
	accounts, err := read("accounts.json")
	if err != nil {
		return err
	}
	details := map[string]string{}
	for _, a := range accounts {
		var probe struct {
			SecuritiesAccount numbered `json:"securitiesAccount"`
		}
		if err := json.Unmarshal(a, &probe); err != nil {
			return fmt.Errorf("accounts.json: %w", err)
		}
		details[probe.SecuritiesAccount.AccountNumber] = string(a)
	}
	hashes := map[string]string{}
	for _, n := range numbers {
		var probe numbered
		if err := json.Unmarshal(n, &probe); err != nil {
			return fmt.Errorf("account_numbers.json: %w", err)
		}
		d, ok := details[probe.AccountNumber]
		if !ok {
			return fmt.Errorf("accounts.json has no account %s", probe.AccountNumber)
		}
		if err := s.addAccount(Account{Number: probe.AccountNumber, Hash: probe.HashValue, Details: d, NumberJSON: string(n)}); err != nil {
			return err
		}
		hashes[probe.AccountNumber] = probe.HashValue
	}
	rows, err := read("transactions.json")
	if err != nil {
		return err
	}
	for _, r := range rows {
		var probe numbered
		if err := json.Unmarshal(r, &probe); err != nil {
			return fmt.Errorf("transactions.json: %w", err)
		}
		if _, err := s.addRows(hashes[probe.AccountNumber], Row{JSON: string(r)}); err != nil {
			return fmt.Errorf("transactions.json: %w", err)
		}
	}
	one := mustFixture("transaction.json")
	var probe struct {
		numbered
		ActivityID int64 `json:"activityId"`
	}
	if err := json.Unmarshal(one, &probe); err != nil {
		return fmt.Errorf("transaction.json: %w", err)
	}
	if err := s.setRowByID(hashes[probe.AccountNumber], probe.ActivityID, string(one)); err != nil {
		return fmt.Errorf("transaction.json: %w", err)
	}
	s.preference = string(mustFixture("user_preference.json"))
	return nil
}

// A replayed is one request's recorded answers, sent in turn, the last
// again and again.
type replayed struct {
	answers []http.HandlerFunc
	next    int
}

// replayKey is what a recorded answer is found by: the method, the
// escaped path and the raw query.
func replayKey(method, path, query string) string { return method + " " + path + "?" + query }

// replayFor returns the recorded answer to r, if there is one. It needs
// the lock.
func (s *Server) replayFor(r *http.Request) http.HandlerFunc {
	rp := s.replays[replayKey(r.Method, r.URL.EscapedPath(), r.URL.RawQuery)]
	if rp == nil {
		return nil
	}
	h := rp.answers[min(rp.next, len(rp.answers)-1)]
	rp.next++
	return h
}

// LoadRecording makes the fake answer as a recording did: each file of
// testdata/recorded/<date>/ answers the request its index entry names, the
// method, the path and the query exactly, with the recorded status and the
// headers its entry keeps, ahead of the world. A request recorded more
// than once gets its answers in the index's order, and the last after
// that. Other requests go to the world as usual, which LoadFixtures may
// fill, or not.
func (s *Server) LoadRecording(date string) {
	s.t.Helper()
	sub, err := fs.Sub(files, "testdata")
	if err == nil {
		err = s.loadRecording(sub, Index(), date)
	}
	if err != nil {
		s.t.Fatalf("schwabtest: LoadRecording(%s): %v", date, err)
	}
}

// loadRecording is LoadRecording, from fsys and index.
func (s *Server) loadRecording(fsys fs.FS, index []Entry, date string) error {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return err
	}
	loaded := map[string][]http.HandlerFunc{}
	var keys []string
	for _, e := range index {
		if e.Source != "recorded" || e.Recorded != date {
			continue
		}
		if !strings.HasPrefix(e.File, "recorded/"+date+"/") {
			return fmt.Errorf("%s is not under recorded/%s/", e.File, date)
		}
		body, err := fs.ReadFile(fsys, e.File)
		if err != nil {
			return err
		}
		k := replayKey(e.Request.Method, e.Request.Path, e.Request.Query)
		if loaded[k] == nil {
			keys = append(keys, k)
		}
		loaded[k] = append(loaded[k], replay(e.Response.Status, e.Response.Headers, body))
	}
	if len(keys) == 0 {
		return errors.New("no file is recorded on that day")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range keys {
		if s.replays[k] == nil {
			s.replays[k] = &replayed{}
		}
		s.replays[k].answers = append(s.replays[k].answers, loaded[k]...)
	}
	return nil
}
