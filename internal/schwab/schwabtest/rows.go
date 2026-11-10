package schwabtest

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// MaxTransactions is the most transactions Schwab sends in one answer, and
// the fake's cap unless TransactionRules.Cap sets another.
const MaxTransactions = 3000

// MaxWindowEnd returns the latest end that the fake takes for a window of
// transactions that starts at start: one calendar year later. Schwab's
// spec says a window may cover a year; what it answers past that is not
// recorded, so this is a guess.
func MaxWindowEnd(start time.Time) time.Time { return start.AddDate(1, 0, 0) }

// TransactionTypes returns the transaction types that Schwab's spec lists,
// in its order, which is the order of a ByType answer.
func TransactionTypes() []string {
	return []string{
		"TRADE", "RECEIVE_AND_DELIVER", "DIVIDEND_OR_INTEREST", "ACH_RECEIPT", "ACH_DISBURSEMENT",
		"CASH_RECEIPT", "CASH_DISBURSEMENT", "ELECTRONIC_FUND", "WIRE_OUT", "WIRE_IN", "JOURNAL",
		"MEMORANDUM", "MARGIN_CALL", "MONEY_MARKET", "SMA_ADJUSTMENT",
	}
}

// TransactionRules say how the fake answers a request for transactions,
// where what Schwab does is not known or is a fault. The zero value is the
// fake's default, each part of it a guess unless it says otherwise.
type TransactionRules struct {
	// Cap is the most rows an answer holds: MaxTransactions when 0.
	Cap int
	// CapMode is how the cap meets a request for several types.
	CapMode CapMode
	// KeepOldest keeps the oldest rows at the cap rather than the newest,
	// which Schwab keeps, as seen live.
	KeepOldest bool
	// FilterOn is the time a window takes a row by.
	FilterOn FilterOn
	// RefuseBefore, if set, refuses with 400 a window that starts before
	// it, which has never been seen: Schwab answers [] for a window past an
	// account's history.
	RefuseBefore time.Time
	// ExclusiveStart and ExclusiveEnd leave out the rows at the window's
	// start or end. Whether Schwab's bounds are inclusive is not known;
	// inclusive is the worst case for rows seen twice.
	ExclusiveStart, ExclusiveEnd bool
	// Order is the order of an answer's rows.
	Order Order
	// RequireTypes refuses with 400 a request without types, as Schwab
	// was reported to once, in 2025-12; live in 2026 it sent every type.
	RequireTypes bool
	// SymbolKeepsDividends makes symbol= keep the rows that name the
	// symbol only in their description, as ~SYM, such as dividends.
	// Whether Schwab keeps them is not known.
	SymbolKeepsDividends bool
	// IgnoreSymbol makes symbol= filter nothing, which has never been
	// seen.
	IgnoreSymbol bool
	// MaxWindow returns the latest end taken for a window that starts at
	// start; MaxWindowEnd when nil. It runs under the fake's lock, so it
	// must not call the Server.
	MaxWindow func(start time.Time) time.Time
}

// CapMode is how the cap applies to a request for several types, which
// has not been seen: the one sighting of the cap was most likely of TRADE
// rows alone.
type CapMode int

// The ways the cap can apply.
const (
	// NewestAcrossTypes keeps the newest rows across every type asked
	// for, the oldest with KeepOldest. The default, and a guess.
	NewestAcrossTypes CapMode = iota
	// InTypeOrder cuts the rows after Cap of them in ByType order, newest
	// first within a type (oldest first with KeepOldest), so that the
	// later types lose rows rather than the oldest days, as answers built
	// one type at a time would; Order then orders what is left.
	InTypeOrder
	// PerType caps each type's rows on its own, so that an answer can hold
	// more than Cap rows.
	PerType
)

// FilterOn is the time that a window takes a row by.
type FilterOn int

// The times a window can take a row by. Which Schwab uses is not known.
const (
	// FilterOnTime takes a row by its time. The default.
	FilterOnTime FilterOn = iota
	// FilterOnTradeDate takes a row by its tradeDate, which is midnight in
	// New York on most rows other than executions; a row without one that
	// the fake can read is taken by its time.
	FilterOnTradeDate
)

// Order is the order of the rows in an answer.
type Order int

// The orders of an answer.
const (
	// ByType groups the rows by type, in TransactionTypes' order, then
	// other types in byte order, then rows without a type, newest first
	// within each group, as seen live. The default.
	ByType Order = iota
	// NewestFirst is the rows newest first, whatever their type.
	NewestFirst
	// Shuffled is an order that follows neither time nor type, as one
	// capture's rows are in, the same for the same rows each time.
	Shuffled
)

// A Row is a transaction as the fake serves it. JSON is sent as given. A
// zero Time or an empty Type is read from JSON; one that is set wins, so a
// row can carry a time finer than its JSON shows.
type Row struct {
	JSON string
	Time time.Time // for windows and order
	Type string    // for the types filter and the order
}

// A RowRef names one row of one account, with an id or without. It
// survives Rehash; once the row or its account is removed, using it fails
// the test.
type RowRef struct {
	s       *Server
	account int // the account's serial
	row     int // the row's serial
}

// A row is a Row as the fake read it.
type row struct {
	serial    int // the order it was added in, which Replace keeps
	json      string
	time      time.Time
	typ       string // "" for none
	id        int64
	hasID     bool
	tradeDate time.Time // zero when it cannot be read
	symbols   []string  // its legs' instrument.symbol and underlyingSymbol
	described string    // the symbol after a "~" that ends its description
	byID      string    // what transactions/{id} sends, if not json
}

// The forms of an instant that the fake reads: in a row, Schwab's +0000
// and RFC 3339's Z and +00:00; in a query, the last two.
var (
	rowTime   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:?[0-9]{2})$`)
	queryTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})$`)
	dateAlone = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	integer   = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)
)

// parseTime reads s, which form matched, as an instant.
func parseTime(form *regexp.Regexp, s string) (time.Time, bool) {
	if !form.MatchString(s) {
		return time.Time{}, false
	}
	layout := "2006-01-02T15:04:05Z0700"
	if s[len(s)-1] != 'Z' && s[len(s)-3] == ':' {
		layout = "2006-01-02T15:04:05Z07:00"
	}
	t, err := time.Parse(layout, s)
	return t, err == nil
}

// jsonText returns raw as a string if it is a JSON string.
func jsonText(raw json.RawMessage) (string, bool) {
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// readRow reads r as the fake serves it, as the package doc's table says.
func readRow(r Row) (*row, error) {
	if !isObject(r.JSON) {
		return nil, errors.New("a row must be a JSON object")
	}
	var probe struct {
		ActivityID    json.RawMessage `json:"activityId"`
		Time          json.RawMessage `json:"time"`
		Type          json.RawMessage `json:"type"`
		TradeDate     json.RawMessage `json:"tradeDate"`
		Description   json.RawMessage `json:"description"`
		TransferItems json.RawMessage `json:"transferItems"`
	}
	if err := json.Unmarshal([]byte(r.JSON), &probe); err != nil {
		return nil, err
	}
	out := &row{json: r.JSON, time: r.Time, typ: r.Type}
	id := string(bytes.TrimSpace(probe.ActivityID))
	if s, ok := jsonText(probe.ActivityID); ok {
		id = s
	}
	if integer.MatchString(id) {
		if n, err := strconv.ParseInt(id, 10, 64); err == nil {
			out.id, out.hasID = n, true
		}
	}
	if out.time.IsZero() {
		s, _ := jsonText(probe.Time)
		t, ok := parseTime(rowTime, s)
		if !ok {
			return nil, fmt.Errorf("a row needs a time, and its time is %s", probe.Time)
		}
		out.time = t
	}
	if out.typ == "" {
		out.typ, _ = jsonText(probe.Type)
	}
	if s, ok := jsonText(probe.TradeDate); ok {
		out.tradeDate, _ = parseTime(rowTime, s)
	}
	if s, ok := jsonText(probe.Description); ok {
		if i := strings.LastIndex(s, "~"); i >= 0 && i+1 < len(s) {
			out.described = s[i+1:]
		}
	}
	var legs []json.RawMessage
	if json.Unmarshal(probe.TransferItems, &legs) == nil {
		for _, leg := range legs {
			var l struct {
				Instrument struct {
					Symbol           json.RawMessage `json:"symbol"`
					UnderlyingSymbol json.RawMessage `json:"underlyingSymbol"`
				} `json:"instrument"`
			}
			if json.Unmarshal(leg, &l) != nil {
				continue
			}
			for _, raw := range []json.RawMessage{l.Instrument.Symbol, l.Instrument.UnderlyingSymbol} {
				if s, ok := jsonText(raw); ok {
					out.symbols = append(out.symbols, s)
				}
			}
		}
	}
	return out, nil
}

// names reports whether r names symbol: a leg's instrument does, or, with
// dividends, its description's ~SYM.
func (r *row) names(symbol string, dividends bool) bool {
	return slices.Contains(r.symbols, symbol) || dividends && r.described == symbol
}

// newer orders rows newest first by time; at equal times, rows with ids
// before rows without, larger ids first, and rows without ids in the
// order they were added.
func newer(a, b *row) int {
	if c := b.time.Compare(a.time); c != 0 {
		return c
	}
	switch {
	case a.hasID && b.hasID:
		return cmp.Compare(b.id, a.id)
	case a.hasID:
		return -1
	case b.hasID:
		return 1
	}
	return cmp.Compare(a.serial, b.serial)
}

// typeRank orders types as ByType does: the spec's in its order, then the
// others in byte order, then none.
func typeRank(a, b string) int {
	rank := func(t string) int {
		if t == "" {
			return 2
		}
		if slices.Contains(TransactionTypes(), t) {
			return 0
		}
		return 1
	}
	if c := cmp.Compare(rank(a), rank(b)); c != 0 {
		return c
	}
	if rank(a) == 0 {
		return cmp.Compare(slices.Index(TransactionTypes(), a), slices.Index(TransactionTypes(), b))
	}
	return strings.Compare(a, b)
}

// shuffleKey is what Shuffled orders a row by: the 64-bit FNV-1a hash of
// its id's decimal digits, or of its bytes when it has none.
func shuffleKey(r *row) uint64 {
	h := fnv.New64a()
	if r.hasID {
		_, _ = h.Write([]byte(strconv.FormatInt(r.id, 10)))
	} else {
		_, _ = h.Write([]byte(r.json))
	}
	return h.Sum64()
}

// guess returns a 400 whose message, the fake's guess, says so.
func (s *Server) guess(message string) answer {
	return s.built(http.StatusBadRequest, []byte(`{"message":`+string(jsonString("a guess: "+message))+`}`))
}

// queryDate reads the window's bound name from q, or returns the 400 that
// refuses it.
func (s *Server) queryDate(q url.Values, name string) (time.Time, *answer) {
	v, ok := q[name]
	if !ok || len(v) == 0 {
		a := s.guess("Schwab's answer to a request without " + name + " is not recorded")
		return time.Time{}, &a
	}
	if t, ok := parseTime(queryTime, v[0]); ok {
		return t, nil
	}
	if dateAlone.MatchString(v[0]) {
		// Schwab's reported wording, with the value cut short, whatever it
		// is, so that the body stays small. Only the wording was reported:
		// the status and the rest of the body are guesses.
		value := v[0][:min(len(v[0]), 64)]
		a := s.built(http.StatusBadRequest, []byte(`{"message":`+string(jsonString("'"+value+"' is not a valid value for "+name))+`}`))
		return time.Time{}, &a
	}
	a := s.guess("Schwab's answer to this " + name + " is not recorded")
	return time.Time{}, &a
}

// transactions answers GET /accounts/{hash}/transactions, as the package
// doc says. It needs the lock.
func (s *Server) transactions(hash string, q url.Values) answer {
	acct := s.byHash(hash)
	if acct == nil {
		return s.invalidAccount()
	}
	rules := s.rules
	start, refused := s.queryDate(q, "startDate")
	if refused != nil {
		return *refused
	}
	end, refused := s.queryDate(q, "endDate")
	if refused != nil {
		return *refused
	}
	maxWindow := rules.MaxWindow
	if maxWindow == nil {
		maxWindow = MaxWindowEnd
	}
	types := slices.DeleteFunc(strings.Split(q.Get("types"), ","), func(t string) bool { return t == "" })
	switch {
	case end.Before(start):
		return s.guess("Schwab's answer to a window that ends before it starts is not recorded")
	case end.After(maxWindow(start)):
		return s.guess("Schwab's answer to a window longer than it serves is not recorded")
	case rules.RequireTypes && len(types) == 0:
		return s.guess("a request without types was reported refused once, in 2025-12")
	case !rules.RefuseBefore.IsZero() && start.Before(rules.RefuseBefore):
		return s.guess("never seen: Schwab answers [] for a window before an account's history")
	}
	if acct.NoHistory {
		return s.built(http.StatusOK, []byte("[]"))
	}

	symbol := q.Get("symbol")
	var rows []*row
	for _, r := range acct.rows {
		at := r.time
		if rules.FilterOn == FilterOnTradeDate && !r.tradeDate.IsZero() {
			at = r.tradeDate
		}
		switch {
		case at.Before(start), at.After(end),
			rules.ExclusiveStart && at.Equal(start), rules.ExclusiveEnd && at.Equal(end),
			len(types) > 0 && (r.typ == "" || !slices.Contains(types, r.typ)),
			symbol != "" && !rules.IgnoreSymbol && !r.names(symbol, rules.SymbolKeepsDividends):
			continue
		}
		rows = append(rows, r)
	}

	limit := rules.Cap
	if limit == 0 {
		limit = MaxTransactions
	}
	keep := func(rs []*row) []*row {
		if len(rs) <= limit {
			return rs
		}
		if rules.KeepOldest {
			return rs[len(rs)-limit:]
		}
		return rs[:limit]
	}
	byType := func(a, b *row) int {
		if c := typeRank(a.typ, b.typ); c != 0 {
			return c
		}
		return newer(a, b)
	}
	switch rules.CapMode {
	case NewestAcrossTypes:
		slices.SortFunc(rows, newer)
		rows = keep(rows)
	case InTypeOrder:
		within := newer
		if rules.KeepOldest {
			within = func(a, b *row) int { return newer(b, a) }
		}
		slices.SortFunc(rows, func(a, b *row) int { return cmp.Or(typeRank(a.typ, b.typ), within(a, b)) })
		rows = rows[:min(len(rows), limit)]
	case PerType:
		slices.SortFunc(rows, byType)
		var capped []*row
		for group := range chunkBy(rows, func(r *row) string { return r.typ }) {
			capped = append(capped, keep(group)...)
		}
		rows = capped
	}

	switch rules.Order {
	case ByType:
		slices.SortFunc(rows, byType)
	case NewestFirst:
		slices.SortFunc(rows, newer)
	case Shuffled:
		slices.SortFunc(rows, func(a, b *row) int { return cmp.Or(cmp.Compare(shuffleKey(a), shuffleKey(b)), newer(a, b)) })
	}
	var b bytes.Buffer
	b.WriteByte('[')
	for i, r := range rows {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(r.json)
	}
	b.WriteByte(']')
	return s.built(http.StatusOK, b.Bytes())
}

// chunkBy yields the runs of rs that share key.
func chunkBy(rs []*row, key func(*row) string) func(yield func([]*row) bool) {
	return func(yield func([]*row) bool) {
		for i := 0; i < len(rs); {
			j := i + 1
			for j < len(rs) && key(rs[j]) == key(rs[i]) {
				j++
			}
			if !yield(rs[i:j]) {
				return
			}
			i = j
		}
	}
}

// transaction answers GET /accounts/{hash}/transactions/{id}. It needs the
// lock.
func (s *Server) transaction(hash, id string) answer {
	acct := s.byHash(hash)
	if acct == nil {
		return s.invalidAccount()
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return s.guess("Schwab's answer to an id that is not a number is not recorded")
	}
	for _, r := range acct.rows {
		if r.hasID && r.id == n {
			if r.byID != "" {
				return s.built(http.StatusOK, []byte(r.byID))
			}
			return s.built(http.StatusOK, []byte(r.json))
		}
	}
	return s.built(http.StatusNotFound, []byte(`{"message":"Not Found (a guess: Schwab's answer is not recorded)"}`))
}

// SetTransactionRules sets how the fake answers requests for transactions.
func (s *Server) SetTransactionRules(r TransactionRules) {
	s.t.Helper()
	switch {
	case r.Cap < 0:
		s.t.Errorf("schwabtest: SetTransactionRules: Cap %d", r.Cap)
		return
	case r.CapMode < NewestAcrossTypes || r.CapMode > PerType:
		s.t.Errorf("schwabtest: SetTransactionRules: CapMode %d", r.CapMode)
		return
	case r.FilterOn < FilterOnTime || r.FilterOn > FilterOnTradeDate:
		s.t.Errorf("schwabtest: SetTransactionRules: FilterOn %d", r.FilterOn)
		return
	case r.Order < ByType || r.Order > Shuffled:
		s.t.Errorf("schwabtest: SetTransactionRules: Order %d", r.Order)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rules = r
}

// TransactionRules returns the rules SetTransactionRules last set.
func (s *Server) TransactionRules() TransactionRules {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rules
}
