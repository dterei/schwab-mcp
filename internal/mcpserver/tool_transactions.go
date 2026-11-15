package mcpserver

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// get_transactions (design 10.5) and get_transaction (10.6). Each account
// is read from the local history when its whole history there holds every
// asked day, else from Schwab, and one account's days never come from
// both: the store's matcher alone can tell a History row from the API row
// of one trade a day apart, so a split at a seam could count a trade
// twice. The store chooses, pages and totals in one read transaction
// (history.Reader.Ledger); Schwab's rows get the store's category, symbol,
// figures, ref and flags from history.DeriveAPIEntries, so that filters and
// totals mean the same for both sources. One page merges the two sources
// newest first by day, time, ref and account, a total order.

// registerTransactions registers get_transactions and get_transaction, and
// the reference page of transaction types.
func (s *Server) registerTransactions() {
	windows := newWindowCache()
	addTool(s, getTransactionsDef, func(ctx context.Context, c *call, in getTransactionsIn) (getTransactionsOut, error) {
		return getTransactions(ctx, c, windows, in)
	})
	addTool(s, getTransactionDef, getTransaction)
	s.addReferencePage(transactionTypesPage())
}

// getTransactionsDef is get_transactions' definition.
var getTransactionsDef = toolDef{
	name:  "get_transactions",
	title: "Schwab transactions",
	description: "Transactions between two New York dates (default the last 90 days), newest first, with totals per " +
		"category over every page. Each account is read from the local history when it holds every asked day in full, " +
		"else from Schwab; coverage says which, per account, and what waits for the user's decision. amount is the " +
		"cash effect, positive in; quantity is positive when bought. Filters: account, categories (buy, sell, " +
		"dividend, interest, fee, tax, ...), types (Schwab's, such as TRADE), symbol. source: auto, store or schwab. " +
		"Paged: pass next_cursor back as cursor.",
	slot: true,
	args: map[string]argRule{
		"from": {desc: "The first New York date, YYYY-MM-DD; 90 days before to when left out."},
		"to":   {desc: "The last New York date, YYYY-MM-DD, included; today when left out, or when later than today."},
		"types": {desc: "Only these of Schwab's transaction types, such as TRADE or DIVIDEND_OR_INTEREST " +
			"(schwab://reference/transaction-types lists them); every type when left out. The local history's rows of " +
			"History files, which have no type, are then left out."},
		"symbol": {desc: "Only the rows of this symbol, in any case: a security leg's, or a dividend's after the ~ of " +
			"its description."},
		"include_uncounted": {desc: "Also list the local history's rows that it does not count (held out, uncertain, " +
			"linked to an API row), with counted false; totals stay of the counted rows."},
		"detail": {desc: "compact (the default) or full, which adds each row's legs, settlement date, order ID and the " +
			"local history's pairing."},
		"limit": {
			desc: "The most rows of a page, 1 to 200; 50 when left out. A page also stops at about 20,000 bytes.",
			min:  new(1.0), max: new(200.0),
		},
	},
}

// getTransactionDef is get_transaction's definition.
var getTransactionDef = toolDef{
	name:  "get_transaction",
	title: "One Schwab transaction",
	description: "One transaction in full: every leg (security, fee or cash), settlement date, order ID, what the " +
		"local history says of it (counted or not and why, its pair, its matches, its flags), and with include_raw " +
		"Schwab's JSON with account numbers masked. id is a ref from get_transactions (T... or C...) or Schwab's " +
		"activity ID. account is needed only when neither the local history nor the id tells the account.",
	slot: true,
	args: map[string]argRule{
		"id": {desc: "A ref from get_transactions (T73508261946, T~... or C0869ae0a.2), or Schwab's activity ID " +
			"(73508261946).", minLen: new(1), maxLen: new(64)},
		"account": {desc: "The account, as get_transactions takes it; needed only when neither the local history nor " +
			"id tells it."},
		"include_raw": {desc: "Also return Schwab's JSON of the transaction, with account numbers masked and any value " +
			"of a key the server does not know withheld."},
	},
}

// getTransactionsIn is get_transactions' arguments.
type getTransactionsIn struct {
	Account          string   `json:"account,omitempty"`
	From             string   `json:"from,omitempty"`
	To               string   `json:"to,omitempty"`
	Categories       []string `json:"categories,omitempty"`
	Types            []string `json:"types,omitempty"`
	Symbol           string   `json:"symbol,omitempty"`
	Source           string   `json:"source,omitempty"`
	IncludeUncounted bool     `json:"include_uncounted,omitempty"`
	Detail           string   `json:"detail,omitempty"`
	Limit            int      `json:"limit,omitempty"`
	Cursor           string   `json:"cursor,omitempty"`
}

// getTransactionsOut is get_transactions' answer.
type getTransactionsOut struct {
	envelope
	page
	cuts                  // detail full: a row's legs cut to fit the cap (design 8.3)
	Transactions []txnRow `json:"transactions"`
	// Totals are, per category, every counted row that the query matches,
	// on every page; null when the walk was cut short.
	Totals   []categoryTotal `json:"totals"`
	Coverage []coverageRow   `json:"coverage"` // one per selected account
}

// dropRaw holds no raw JSON: get_transactions sends none.
func (o *getTransactionsOut) dropRaw() bool { return false }

// lists returns each row's legs, which detail full adds.
func (o *getTransactionsOut) lists() []cuttable {
	var ls []cuttable
	for i := range o.Transactions {
		d := o.Transactions[i].Detail
		if d == nil {
			continue
		}
		ls = append(ls, cuttable{
			path: fmt.Sprintf("transactions.%d.detail.legs", i), value: d.Legs,
			count: func() int { return len(d.Legs) }, keep: func(n int) { d.Legs = d.Legs[:n] },
		})
	}
	return ls
}

// cutHint says how to see a row's legs that were cut.
func (o *getTransactionsOut) cutHint() string {
	return "get_transaction with a row's ref shows its legs, as many as fit."
}

// A txnRow is one transaction of an answer: an API row, read from Schwab
// or from the local history, or a row of History files, from the local
// history.
type txnRow struct {
	Ref         Ident        `json:"ref"`       // "T73508261946", or a History row's "C..." ref
	ReadFrom    Ident        `json:"read_from"` // "store" or "schwab"
	RowKind     Ident        `json:"row_kind"`  // "api" (Schwab's row) or "file" (a schwab.com export's)
	Account     MaskedNumber `json:"account"`
	AccountHash *Ident       `json:"account_hash"`
	Date        Date         `json:"date"`   // the trade day, or the file's date
	Time        *Instant     `json:"time"`   // null for a History row, and for Schwab's midnight
	Type        *Ident       `json:"type"`   // Schwab's type; null for a History row
	Action      *FreeText    `json:"action"` // a History row's action
	Status      *Ident       `json:"status"`
	Category    *Ident       `json:"category"`
	Symbol      *Ident       `json:"symbol"`
	Quantity    lookup.Qty   `json:"quantity"`
	Price       lookup.Price `json:"price"`
	Amount      lookup.Money `json:"amount"`
	Fees        lookup.Money `json:"fees"`
	Description *FreeText    `json:"description"`
	Counted     *bool        `json:"counted"`     // the local history's rows; null for Schwab's
	CountedWhy  *Ident       `json:"counted_why"` // the local history's rows
	Flags       []Ident      `json:"flags"`
	Detail      *txnDetail   `json:"detail,omitempty"` // detail full, and always in get_transaction
}

// txnDetail is what detail full adds to a row.
type txnDetail struct {
	// SettlementDate, OrderID and SubAccount are Schwab's, null for a row of
	// the local history, whose reader keeps none of them.
	SettlementDate *Date     `json:"settlement_date"`
	OrderID        *Ident    `json:"order_id"`
	SubAccount     *Ident    `json:"sub_account"`
	PairKind       *Ident    `json:"pair_kind"`    // the local history's pairing: journal, sweep or reversal
	PairRef        *Ident    `json:"pair_ref"`     // the row it is paired with
	MatchedWith    []Ident   `json:"matched_with"` // the refs it is linked to
	Problems       []problem `json:"problems"`
	Legs           []txnLeg  `json:"legs"`
}

// A txnLeg is one leg of a transaction: a security, a fee or cash.
type txnLeg struct {
	AssetType   *Ident       `json:"asset_type"`
	Symbol      *Ident       `json:"symbol"`
	Description *FreeText    `json:"description"` // the instrument's; null for the local history's legs
	Quantity    lookup.Qty   `json:"quantity"`    // a security leg's shares or contracts, positive when bought
	Price       lookup.Price `json:"price"`       // per share; an option's premium per share
	Cost        lookup.Money `json:"cost"`        // its cash, on a trade
	Cash        lookup.Money `json:"cash"`        // a cash leg's amount: the fee on a trade's fee leg
	FeeType     *Ident       `json:"fee_type"`
	// PositionEffect is OPENING or CLOSING.
	PositionEffect *Ident       `json:"position_effect"`
	PutCall        *Ident       `json:"put_call"`
	Strike         lookup.Price `json:"strike"`
	Expiration     *Date        `json:"expiration"`
}

// A categoryTotal is a category's counted rows that a query matches, on
// every page.
type categoryTotal struct {
	Category Ident        `json:"category"`
	Count    int          `json:"count"`
	Amount   lookup.Money `json:"amount"` // null when a row's amount is
	Fees     lookup.Money `json:"fees"`   // null when a row's fees are
}

// A coverageRow says where an account was read from, and which of the
// asked days the answer holds in full.
type coverageRow struct {
	accountRef
	StoreRef *Ident `json:"store_ref"`
	ReadFrom Ident  `json:"read_from"` // "store", "schwab" or "none"
	Asked    span   `json:"asked"`
	Whole    []span `json:"whole"` // asked days this answer holds in full
	// Partial are the local history's other runs within the asked days:
	// for an account read from the store, those of sources that are not
	// whole history; for any other, every run, none of which it read.
	Partial     []sourceSpan `json:"partial"`
	Waiting     *waitingRow  `json:"waiting"`      // read from the store: what waits for the user's decision
	LastRead    *Instant     `json:"last_read"`    // read from the store: Schwab's rows last read or imported
	ReachedBack *Date        `json:"reached_back"` // read from Schwab: the oldest day the walk read
	Rows        *int         `json:"rows"`         // rows of the account that the query matches, all pages; null when cut short
	Error       *Ident       `json:"error"`        // the code, when the account could not be read
}

// A sourceSpan is a run of days that one source of the local history
// covers.
type sourceSpan struct {
	Source Ident `json:"source"` // api_file, file_unknown, file_filtered, realized, snapshot; api or file
	From   Date  `json:"from"`
	To     Date  `json:"to"`
}

// A waitingRow is what waits for the user's decision in an account, as
// schwabtx status counts it: held-out and uncertain rows of the asked days,
// and the account's flags and files that wait.
type waitingRow struct {
	HeldOut   int `json:"held_out"`
	Uncertain int `json:"uncertain"`
	Attention int `json:"attention"`
	Filtered  int `json:"filtered"`
}

// A txnQuery is get_transactions' arguments, checked, with the defaults
// applied.
type txnQuery struct {
	from, to   nytime.Date
	categories []string // each once, in order
	types      []string // upper case, each once, in order
	symbol     string   // upper case, "" for any
	source     string   // auto, store or schwab
	all        bool     // include_uncounted
	full       bool     // detail full
	limit      int
}

// defaultDays is how many days before to the asked days start when from
// is left out.
const defaultDays = 90

// txnQueryOf checks in, get_transactions' arguments beyond the account
// and the cursor, and applies their defaults. Its errors name the argument
// and never quote its value.
func (c *call) txnQueryOf(in getTransactionsIn) (txnQuery, error) {
	today := nytime.DateOf(c.now)
	q := txnQuery{
		to: today, source: cmp.Or(in.Source, "auto"), all: in.IncludeUncounted, full: in.Detail == "full",
		limit: cmp.Or(in.Limit, 50), symbol: asciiUpper(strings.TrimSpace(in.Symbol)),
	}
	if in.To != "" {
		if _, ok := dayOf(nytime.Date(in.To)); !ok {
			return q, c.failf(codeBadArgument, "to takes a New York date, YYYY-MM-DD.")
		}
		q.to = min(nytime.Date(in.To), today)
	}
	q.from = addDays(q.to, -defaultDays)
	if in.From != "" {
		if _, ok := dayOf(nytime.Date(in.From)); !ok {
			return q, c.failf(codeBadArgument, "from takes a New York date, YYYY-MM-DD.")
		}
		q.from = nytime.Date(in.From)
	}
	if q.from > q.to {
		return q, c.failf(codeBadArgument, "from comes after to, or after today when to is later or left out; "+
			"from takes the first day asked, to the last.")
	}
	q.categories = slices.Compact(slices.Sorted(slices.Values(in.Categories)))
	for _, t := range in.Types {
		t = asciiUpper(t)
		if !slices.Contains(schwab.AllTransactionTypes(), schwab.TransactionType(t)) {
			names := make([]string, 0, len(schwab.AllTransactionTypes()))
			for _, k := range schwab.AllTransactionTypes() {
				names = append(names, string(k))
			}
			return q, c.failf(codeBadArgument, "types takes Schwab's transaction types: %s.", Ident(strings.Join(names, ", ")))
		}
		q.types = append(q.types, t)
	}
	q.types = slices.Compact(slices.Sorted(slices.Values(q.types)))
	return q, nil
}

// asciiUpper returns s with its ASCII letters in upper case, as SQLite's
// NOCASE compares a symbol.
func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, s)
}

// txnArgs are get_transactions' canonical arguments, whose digest a cursor
// holds (cursor.go): the defaults applied, each list sorted, the symbol in
// upper case, and the account as its key, never its number.
type txnArgs struct {
	Account          string   `json:"account"`
	From             string   `json:"from"`
	To               string   `json:"to"`
	Categories       []string `json:"categories"`
	Types            []string `json:"types"`
	Symbol           string   `json:"symbol"`
	Source           string   `json:"source"`
	IncludeUncounted bool     `json:"include_uncounted"`
	Detail           string   `json:"detail"`
}

// args returns q's canonical arguments, account being the picked account's
// key, or "" for every account.
func (q txnQuery) args(account string) txnArgs {
	detail := "compact"
	if q.full {
		detail = "full"
	}
	return txnArgs{
		Account: account, From: string(q.from), To: string(q.to),
		Categories: append([]string{}, q.categories...), Types: append([]string{}, q.types...),
		Symbol: q.symbol, Source: q.source, IncludeUncounted: q.all, Detail: detail,
	}
}

// txnAfter reads cur, a cursor of get_transactions, into the position the
// page starts after: nil from the start. A cursor's position is [day,
// time, ref, account], the last row sent, or null.
func (c *call) txnAfter(cur string, args txnArgs) (*history.LedgerKey, error) {
	if cur == "" {
		return nil, nil
	}
	var k *[4]string
	if err := c.readCursor(cur, args, &k); err != nil {
		return nil, err
	}
	if k == nil {
		return nil, nil
	}
	if _, ok := dayOf(nytime.Date(k[0])); !ok || k[2] == "" || k[3] == "" {
		return nil, c.failf(codeBadCursor, "The cursor does not belong to these arguments; call again without it.")
	}
	return &history.LedgerKey{Day: nytime.Date(k[0]), Time: k[1], Ref: k[2], Account: k[3]}, nil
}

// The sources an account is read from.
const (
	fromStore  = "store"
	fromSchwab = "schwab"
	fromNone   = "none"
)

// A txnWalk is one call of get_transactions as it goes: the accounts,
// where each is read from, and what was read.
type txnWalk struct {
	c     *call
	q     txnQuery
	after *history.LedgerKey
	accts []*txnAccount
	// ledger is the store's part, nil when the store was not read; newer
	// says that a newer schwabtx derived the store.
	ledger *history.LedgerPage
	newer  bool
	// windows are the windows of the asked days, newest first, when an
	// account is read from Schwab; stopped is the first one that the
	// budget left unread for some account, -1 for none.
	windows []window
	stopped int
}

// A txnAccount is one selected account, and how it was read.
type txnAccount struct {
	a    dirAccount
	from string   // fromStore, fromSchwab or fromNone
	err  *toolErr // why it could not be read, or nil
	// reads are the windows read from Schwab, newest first; unread says
	// that the budget left the next unread, and refused that Schwab refused
	// it as too old. empty is the index of an older window that Schwab sent
	// nothing for, though it sent rows of newer days, or -1.
	reads   []*windowRead
	unread  bool
	refused bool
	empty   int
	// items are the rows read from Schwab that the query matches, each
	// once, and problems their records' problems.
	items    []txnItem
	problems [][]schwab.Problem
}

// A txnItem is a row of a page to be: one of the store's, or one read from
// Schwab with what the store derives of it.
type txnItem struct {
	key   history.LedgerKey
	ta    *txnAccount
	store *history.LedgerRow
	t     *schwab.Transaction
	e     *history.APIEntry
}

// getTransactions answers get_transactions: the store's part in one read,
// then Schwab's windows, then one page of both, newest first.
func getTransactions(ctx context.Context, c *call, wc *windowCache, in getTransactionsIn) (getTransactionsOut, error) {
	var out getTransactionsOut
	q, err := c.txnQueryOf(in)
	if err != nil {
		return out, err
	}
	v, err := c.directory(ctx)
	if err != nil {
		return out, err
	}
	w := &txnWalk{c: c, q: q, stopped: -1}
	accts, err := w.pick(ctx, v, in.Account)
	if err != nil {
		return out, err
	}
	key := ""
	if strings.TrimSpace(in.Account) != "" {
		key = accts[0].key()
	}
	if w.after, err = c.txnAfter(in.Cursor, q.args(key)); err != nil {
		return out, err
	}
	for _, a := range accts {
		w.accts = append(w.accts, &txnAccount{a: a, empty: -1})
	}
	if q.source == "auto" && v.schwabErr != nil {
		c.warn("api_unavailable", "Schwab could not be read, so each account was read from the local history as far as "+
			"it holds these days: %s", c.errText(c.s.toolError(c.tool, v.schwabErr)))
	}
	if err := w.readStore(ctx, v); err != nil {
		return out, err
	}
	if err := w.readSchwab(ctx, wc); err != nil {
		return out, err
	}
	return w.answer(ctx, q.args(key))
}

// pick returns the selected accounts: under schwab those that Schwab
// shares, else every account the directory knows, those only the store
// knows included. Under store, a store that cannot be read is the call's
// error.
func (w *txnWalk) pick(ctx context.Context, v *dirView, arg string) ([]dirAccount, error) {
	c := w.c
	switch w.q.source {
	case "schwab":
		if v.schwabErr != nil {
			return nil, v.schwabErr
		}
		return v.pick(c, arg, false)
	case "store":
		err := v.storeErr
		if err == nil {
			err = c.store.read(ctx, func(*history.Reader) error { return nil })
		}
		if err != nil {
			return nil, c.storeError(err)
		}
	}
	if v.schwabErr != nil && len(v.accounts) == 0 {
		return nil, v.schwabErr
	}
	return v.pick(c, arg, true)
}

// storeError returns err, a failure of the store, as a call may answer it:
// a config that names no store that may be used is no_store.
func (c *call) storeError(err error) error {
	if errors.Is(err, errNoStorePath) {
		return c.s.errorFor(codeNoStore)
	}
	return err
}

// readStore reads the store's part (design 10.5, 12.4): one Ledger, given
// every selected account that the store knows, each read only when its
// whole history holds every asked day under auto, and whatever covers under
// store and for an account that Schwab no longer shares. In one read
// transaction it chooses which accounts it reads, and pages and totals
// them. Every other account goes to Schwab, when Schwab shares it. Under
// auto, a store that cannot be read sends every account to Schwab, with
// the warning store_unavailable.
func (w *txnWalk) readStore(ctx context.Context, v *dirView) error {
	c, q := w.c, w.q
	if q.source == "schwab" {
		for _, ta := range w.accts {
			ta.from = fromSchwab
		}
		return nil
	}
	var listed []history.LedgerAccount
	for _, ta := range w.accts {
		if ta.a.storeID != 0 {
			listed = append(listed, history.LedgerAccount{ID: ta.a.storeID, Key: ta.a.key(), IfWhole: q.source == "auto" && ta.a.shared})
		}
	}
	err := v.storeErr
	switch {
	case err != nil:
	case len(listed) == 0:
		err = c.store.read(ctx, func(*history.Reader) error { return nil })
	default:
		err = c.store.read(ctx, func(r *history.Reader) error {
			p, err := r.Ledger(ctx, history.LedgerQuery{
				Accounts: listed, From: q.from, To: q.to, Categories: q.categories, Types: q.types,
				Symbol: q.symbol, All: q.all, After: w.after, Limit: q.limit,
			})
			w.ledger, w.newer = p, r.NewerRules()
			return err
		})
	}
	var storeErr *toolErr
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		err = c.storeError(err)
		if q.source == "store" {
			return err
		}
		w.ledger = nil
		storeErr = c.s.toolError(c.tool, err)
		c.warn("store_unavailable", "The local history could not be read, so each account was read from Schwab: %s", c.errText(storeErr))
	}
	if w.newer {
		c.warn("store_newer_rules", "A newer schwabtx derived the local history; ask the user to update schwab-mcp to the same version.")
	}
	for _, ta := range w.accts {
		switch {
		case w.ledger != nil && slices.Contains(w.ledger.Read, ta.a.storeID):
			ta.from = fromStore
		case q.source == "auto" && ta.a.shared:
			ta.from = fromSchwab
		default:
			ta.from = fromNone
			if q.source == "auto" {
				ta.err = storeErr
			}
		}
	}
	return nil
}

// readSchwab reads the accounts that go to Schwab, in windows: the newest
// window of each account, then the next, so that a walk that the budget
// cuts short leaves only older days unread. A window that Schwab refuses as
// too old, after it served a newer one, ends that account's walk, as the
// CLI's does; any other failure is the account's, and the others are read
// on.
func (w *txnWalk) readSchwab(ctx context.Context, wc *windowCache) error {
	c, q := w.c, w.q
	var sw []*txnAccount
	for _, ta := range w.accts {
		if ta.from == fromSchwab {
			sw = append(sw, ta)
		}
	}
	if len(sw) == 0 {
		return nil
	}
	if daysFrom(q.from, q.to)+1 > schwabDays {
		return c.failf(codeBadArgument, "from: one call reads at most %d days of an account from Schwab. Ask for a "+
			"shorter range; or, for older years, read the local history with source store, after the user imports "+
			"schwab.com's History exports (schwabtx import).", schwabDays)
	}
	w.windows = windowsOf(q.from, q.to)
	for i, win := range w.windows {
		for _, ta := range sw {
			if ta.err != nil || ta.refused || ta.unread {
				continue
			}
			if w.stopped >= 0 {
				ta.unread = true
				continue
			}
			r, err := wc.read(ctx, c, ta.a, win)
			switch {
			case err == nil:
				ta.reads = append(ta.reads, r)
				if i > 0 && r.sent == 0 && ta.empty < 0 && slices.ContainsFunc(ta.reads, func(r *windowRead) bool { return r.sent > 0 }) {
					ta.empty = i
				}
			case errors.Is(err, errNoTime):
				w.stopped, ta.unread = i, true
			case ctx.Err() != nil:
				return err
			case i > 0 && refusedAsOld(err):
				ta.refused = true
			default:
				ta.err, ta.from = c.s.toolError(c.tool, err), fromNone
			}
		}
	}
	for _, ta := range sw {
		if ta.from == fromSchwab {
			w.match(ta)
		}
	}
	return nil
}

// refusedAsOld reports whether err is Schwab refusing a window, as for
// days older than it serves: a 400 that is no refusal of the login, the
// app or the account, which no window would get past.
func refusedAsOld(err error) bool {
	se, ok := errors.AsType[*schwab.Error](err)
	return ok && se.Status == http.StatusBadRequest && !errors.Is(err, schwab.ErrLoginExpired) &&
		!errors.Is(err, schwab.ErrAppRejected) && !errors.Is(err, schwab.ErrInvalidAccount)
}

// match keeps the rows of ta's windows, which hold the asked days alone,
// that the query's filters match, each ref once: the store records one
// transaction of an activity ID that an answer holds twice.
func (w *txnWalk) match(ta *txnAccount) {
	q := w.q
	seen := map[string]bool{}
	for _, r := range ta.reads {
		for i := range r.rows {
			t, e := &r.rows[i], &r.entries[i]
			switch {
			case seen[e.Ref],
				len(q.types) > 0 && !slices.Contains(q.types, string(t.Type)),
				len(q.categories) > 0 && !slices.Contains(q.categories, e.Category),
				q.symbol != "" && asciiUpper(e.Symbol) != q.symbol:
				continue
			}
			seen[e.Ref] = true
			ta.items = append(ta.items, txnItem{
				key: history.LedgerKey{Day: e.Day, Time: storeInstant(t.Time), Ref: e.Ref, Account: ta.a.key()},
				ta:  ta, t: t, e: e,
			})
			ta.problems = append(ta.problems, t.Problems)
		}
	}
}

// cut reports whether the walk was cut short: the budget left a window
// unread, or Schwab may have cut a window that could not be halved again.
func (w *txnWalk) cut() bool {
	return w.stopped >= 0 || slices.ContainsFunc(w.accts, (*txnAccount).capped)
}

// cut reports whether ta's walk was cut short: the budget left one of its
// windows unread, or Schwab may have cut one.
func (ta *txnAccount) cut() bool { return ta.unread || ta.capped() }

// capped reports whether Schwab may have cut one of ta's windows, which
// could not be halved again.
func (ta *txnAccount) capped() bool {
	return slices.ContainsFunc(ta.reads, func(r *windowRead) bool { return r.capped })
}

// horizon returns the last day of the newest window that the budget left
// unread: a page holds only rows of later days, since the unread ones
// could come between the others. "" when none was left unread.
func (w *txnWalk) horizon() nytime.Date {
	if w.stopped < 0 {
		return ""
	}
	return w.windows[w.stopped].last
}

// answer builds the call's answer: the coverage, the totals and the
// warnings, then one page of rows of both sources, newest first, at most
// limit and within the page budget, and the cursor of the next.
func (w *txnWalk) answer(ctx context.Context, args txnArgs) (getTransactionsOut, error) {
	c := w.c
	out := getTransactionsOut{Transactions: []txnRow{}, Coverage: []coverageRow{}}
	read, failed := 0, 0
	var firstErr *toolErr
	sources := map[string]bool{}
	for _, ta := range w.accts {
		out.Coverage = append(out.Coverage, w.coverage(ta))
		switch {
		case ta.from != fromNone:
			read++
			sources[ta.from] = true
		case ta.err != nil:
			failed++
			if firstErr == nil {
				firstErr = ta.err
			}
		}
	}
	if read == 0 && failed > 0 {
		return out, firstErr
	}
	switch {
	case sources[fromStore] && sources[fromSchwab]:
		out.Source = sourceSchwabStore
	case sources[fromStore], read == 0 && w.q.source == "store":
		out.Source = sourceStore
	default:
		out.Source = sourceSchwab
	}
	w.warnings(out.Coverage)
	out.Complete = !w.cut()
	if out.Complete {
		out.Totals = w.totals()
	}
	if w.stopped >= 0 {
		c.warn("partial", "The call's time ran out before Schwab answered for every asked day: this page holds only rows "+
			"after %s, totals are null, and next_cursor continues the walk.", Date(w.horizon()))
	}

	// The candidates: the store's page, and Schwab's rows after the cursor,
	// merged newest first; only rows after the horizon when the budget left
	// a window unread.
	var schwabItems []txnItem
	for _, ta := range w.accts {
		for _, it := range ta.items {
			if w.after == nil || keyCompare(it.key, *w.after) < 0 {
				schwabItems = append(schwabItems, it)
			}
		}
	}
	slices.SortFunc(schwabItems, func(x, y txnItem) int { return keyCompare(y.key, x.key) })
	var storeItems []txnItem
	if w.ledger != nil {
		for i := range w.ledger.Rows {
			x := &w.ledger.Rows[i]
			storeItems = append(storeItems, txnItem{key: x.Key, ta: w.byStoreID(x.AccountID), store: x})
		}
	}
	merged := mergeItems(storeItems, schwabItems)
	if h := w.horizon(); h != "" {
		merged = slices.DeleteFunc(merged, func(it txnItem) bool { return it.key.Day <= h })
	}
	cands := merged[:min(len(merged), w.q.limit)]

	// The page: rows while the answer, its fixed part as it stands and
	// next_cursor at its longest, stays within the page budget.
	out.Warnings = append([]warning{}, c.warnings...)
	fixed, err := encode(out)
	if err != nil {
		return out, err
	}
	var sizes []int
	for _, it := range cands {
		row, err := w.row(ctx, it)
		if err != nil {
			return out, err
		}
		b, err := encode(row)
		if err != nil {
			return out, err
		}
		sizes = append(sizes, len(b))
		if c.pageRows(len(fixed), sizes, len(sizes)) < len(sizes) {
			break
		}
		out.Transactions = append(out.Transactions, row)
	}
	n := len(out.Transactions)
	more := n < len(merged) || w.ledger != nil && w.ledger.Next != nil || w.stopped >= 0
	if more {
		var k *[4]string
		switch {
		case n > 0:
			last := cands[n-1].key
			k = &[4]string{string(last.Day), last.Time, last.Ref, last.Account}
		case w.after != nil:
			k = &[4]string{string(w.after.Day), w.after.Time, w.after.Ref, w.after.Account}
		}
		if out.NextCursor, err = c.newCursor(args, k); err != nil {
			return out, err
		}
	}
	return out, nil
}

// byStoreID returns the selected account whose store id is id.
func (w *txnWalk) byStoreID(id int64) *txnAccount {
	for _, ta := range w.accts {
		if ta.a.storeID == id {
			return ta
		}
	}
	return nil
}

// keyCompare orders rows by day, then time in the store's instant form,
// then ref, then account key: the order of history.LedgerKey, a total one.
// A page lists rows from the greatest down.
func keyCompare(x, y history.LedgerKey) int {
	return cmp.Or(
		strings.Compare(string(x.Day), string(y.Day)), strings.Compare(x.Time, y.Time),
		strings.Compare(x.Ref, y.Ref), strings.Compare(x.Account, y.Account))
}

// mergeItems merges xs and ys, each newest first, into one list newest
// first.
func mergeItems(xs, ys []txnItem) []txnItem {
	out := make([]txnItem, 0, len(xs)+len(ys))
	for len(xs) > 0 && len(ys) > 0 {
		if keyCompare(xs[0].key, ys[0].key) >= 0 {
			out, xs = append(out, xs[0]), xs[1:]
		} else {
			out, ys = append(out, ys[0]), ys[1:]
		}
	}
	return append(append(out, xs...), ys...)
}

// row returns it as a row of the answer: detail full reads the legs of a
// row of the store.
func (w *txnWalk) row(ctx context.Context, it txnItem) (txnRow, error) {
	c := w.c
	if it.store == nil {
		return c.schwabRow(it.ta.a, *it.t, *it.e, w.q.full), nil
	}
	var legs []history.LegRow
	if w.q.full {
		err := c.store.read(ctx, func(r *history.Reader) error {
			found, err := r.EntryByRef(ctx, it.store.Ref)
			for _, d := range found {
				if d.Entry.AccountID == it.store.AccountID {
					legs = d.Legs
				}
			}
			return err
		})
		if err != nil {
			return txnRow{}, c.storeError(err)
		}
	}
	return c.storeRow(it.ta.a, *it.store, legs, w.q.full), nil
}

// totals returns the totals per category of every counted row that the
// query matches: the store's, from the same read transaction as its page,
// and the server's sum of Schwab's, whose INVALID rows the store would not
// count. A sum is none when one of its rows' figures is.
func (w *txnWalk) totals() []categoryTotal {
	type sum struct {
		count            int
		amount, fees     int64
		noAmount, noFees bool
	}
	sums := map[string]*sum{}
	add := func(category string, count int, amount, fees *int64) {
		s := sums[category]
		if s == nil {
			s = &sum{}
			sums[category] = s
		}
		s.count += count
		if amount == nil {
			s.noAmount = true
		} else {
			s.amount += *amount
		}
		if fees == nil {
			s.noFees = true
		} else {
			s.fees += *fees
		}
	}
	if w.ledger != nil {
		for _, t := range w.ledger.Totals {
			add(t.Category, t.Count, t.AmountCents, t.FeesCents)
		}
	}
	for _, ta := range w.accts {
		for _, it := range ta.items {
			if it.t.Status != schwab.TransactionStatusInvalid {
				add(it.e.Category, 1, it.e.AmountCents, it.e.FeesCents)
			}
		}
	}
	out := []categoryTotal{}
	for _, name := range slices.Sorted(maps.Keys(sums)) {
		s := sums[name]
		t := categoryTotal{Category: Ident(name), Count: s.count}
		if !s.noAmount {
			t.Amount = lookup.FromCents(s.amount)
		}
		if !s.noFees {
			t.Fees = lookup.FromCents(s.fees)
		}
		out = append(out, t)
	}
	return out
}

// coverage returns ta's coverage row.
func (w *txnWalk) coverage(ta *txnAccount) coverageRow {
	c, q := w.c, w.q
	asked := span{From: Date(q.from), To: Date(q.to)}
	r := coverageRow{
		accountRef: c.ref(ta.a), StoreRef: identPtr(ta.a.storeRef), ReadFrom: Ident(ta.from), Asked: asked,
		Whole: []span{}, Partial: []sourceSpan{},
	}
	if ta.err != nil {
		r.Error = new(Ident(ta.err.code))
	}
	var runs []history.CoverageRun
	if w.ledger != nil && ta.a.storeID != 0 {
		for _, run := range w.ledger.Coverage {
			if run.AccountID == ta.a.storeID {
				runs = append(runs, run)
			}
		}
	}
	switch ta.from {
	case fromStore:
		var whole []history.CoverageRun
		for _, run := range runs {
			if run.Source == "api" || run.Source == "file" {
				whole = append(whole, run)
			} else {
				r.Partial = append(r.Partial, w.clip(run))
			}
		}
		r.Whole = w.wholeOf(whole)
		for _, wt := range w.ledger.Waiting {
			if wt.AccountID == ta.a.storeID {
				r.Waiting = &waitingRow{HeldOut: wt.HeldOut, Uncertain: wt.Uncertain, Attention: wt.Attention, Filtered: wt.Filtered}
			}
		}
		for _, rd := range w.ledger.Reads {
			if rd.AccountID == ta.a.storeID {
				r.LastRead = instantPtr(rd.AnsweredAt)
			}
		}
		n := 0
		for _, m := range w.ledger.Matched {
			if m.AccountID == ta.a.storeID {
				n = m.Rows
			}
		}
		r.Rows = &n
	case fromSchwab:
		for _, run := range runs {
			r.Partial = append(r.Partial, w.clip(run))
		}
		var oldestWhole nytime.Date
		for i, rd := range ta.reads {
			if rd.capped || i == ta.empty {
				break
			}
			oldestWhole = w.windows[i].first
		}
		if oldestWhole != "" {
			r.Whole = []span{{From: Date(oldestWhole), To: asked.To}}
		}
		if len(ta.reads) > 0 {
			r.ReachedBack = new(Date(w.windows[len(ta.reads)-1].first))
		}
		if !ta.cut() {
			r.Rows = new(len(ta.items))
		}
	default:
		for _, run := range runs {
			r.Partial = append(r.Partial, w.clip(run))
		}
	}
	return r
}

// clip returns run within the asked days.
func (w *txnWalk) clip(run history.CoverageRun) sourceSpan {
	return sourceSpan{Source: Ident(run.Source), From: Date(max(run.From, w.q.from)), To: Date(min(run.To, w.q.to))}
}

// wholeOf returns runs, the api and file runs of one account, merged where
// they meet or overlap, within the asked days: the days that the local
// history holds as whole history (docs/store.md, Coverage).
func (w *txnWalk) wholeOf(runs []history.CoverageRun) []span {
	slices.SortFunc(runs, func(x, y history.CoverageRun) int { return strings.Compare(string(x.From), string(y.From)) })
	var merged []history.CoverageRun
	for _, run := range runs {
		if n := len(merged); n > 0 && addDays(merged[n-1].To, 1) >= run.From {
			merged[n-1].To = max(merged[n-1].To, run.To)
			continue
		}
		merged = append(merged, run)
	}
	out := []span{}
	for _, run := range merged {
		from, to := max(run.From, w.q.from), min(run.To, w.q.to)
		if from <= to {
			out = append(out, span{From: Date(from), To: Date(to)})
		}
	}
	return out
}

// warnings adds the warnings of the walk, whose coverage rows are
// coverage: per account, what waits in the store, days that it does not
// hold as whole history, a failure, and the end of a walk of Schwab's; then
// what applies to every row.
func (w *txnWalk) warnings(coverage []coverageRow) {
	c, q := w.c, w.q
	var problems [][]schwab.Problem
	var beforeDecisions []FreeText
	storeRead := false
	for i, ta := range w.accts {
		name, cov := c.mask.masked(ta.a.number), coverage[i]
		switch ta.from {
		case fromStore:
			storeRead = true
			if wt := cov.Waiting; wt != nil {
				if n := wt.HeldOut + wt.Uncertain; n == 1 {
					c.warn("store_waiting", "1 row of %s in these days waits for the user's decision and is not counted; "+
						"`schwabtx status -review` lists it.", name)
				} else if n > 1 {
					c.warn("store_waiting", "%d rows of %s in these days wait for the user's decision and are not counted; "+
						"`schwabtx status -review` lists them.", n, name)
				}
			}
			if len(cov.Whole) != 1 || cov.Whole[0] != cov.Asked {
				c.warn("store_coverage_partial", "The local history does not hold every asked day of %s as whole history: "+
					"coverage says which days it does, and rows of the others may be missing.", name)
			}
		case fromSchwab:
			if len(cov.Partial) > 0 {
				c.warn("store_covers_some", "%s was read from Schwab: the local history holds some of these days, but not "+
					"every one as whole history.", name)
			}
			if len(ta.items) > 0 {
				beforeDecisions = append(beforeDecisions, c.mask.compose("%s", name))
			}
			problems = append(problems, ta.problems...)
			n := len(ta.reads)
			switch {
			case ta.refused:
				c.warn("empty_not_proof", "Schwab refused the transactions of %s from %s to %s as too old; older ones may "+
					"exist, and this answer does not hold them.", name, Date(w.windows[n].first), Date(w.windows[n].last))
			case ta.empty >= 0:
				c.warn("empty_not_proof", "Schwab sent no transactions of %s from %s to %s, which does not show that there "+
					"were none: Schwab may not serve that far back.", name, Date(w.windows[ta.empty].first), Date(w.windows[ta.empty].last))
			}
			for j, rd := range ta.reads {
				if rd.capped {
					c.warn("partial", "Schwab sent its most rows, %d, for part of the transactions of %s from %s to %s, "+
						"so some may be missing; totals are null.", schwab.MaxTransactions, name,
						Date(w.windows[j].first), Date(w.windows[j].last))
				}
			}
		}
		if ta.err != nil && !isStoreCode(ta.err.code) {
			c.warn("account_failed", "%s could not be read: %s", name, c.errText(ta.err))
		}
	}
	if len(q.types) > 0 && storeRead {
		c.warn("types_filter", "types leaves out the local history's rows of History files, which have no Schwab type.")
	}
	if len(beforeDecisions) > 0 {
		c.warn("before_decisions", "Rows of %s come from Schwab, with the rules' category, before the user's decisions and "+
			"the store's pairing.", c.andList(beforeDecisions))
	}
	if records, _, _ := lookup.ProblemPlaces(problems); records > 0 {
		what := "1 transaction"
		if records > 1 {
			what = strconv.Itoa(records) + " transactions"
		}
		c.warnProblems(what, problems, "a figure that Schwab sent otherwise is null")
	}
}

// andList joins names, each already safe, as a list: "a", "a and b", "a,
// b and c".
func (c *call) andList(names []FreeText) FreeText {
	if n := len(names); n > 1 {
		return c.mask.compose("%s and %s", c.mask.join(names[:n-1], ", "), names[n-1])
	}
	return c.mask.join(names, "")
}

// isStoreCode reports whether code is one of the store's, which the
// warning store_unavailable already gave.
func isStoreCode(code errorCode) bool {
	return slices.Contains([]errorCode{codeNoStore, codeStoreNewer, codeStoreNeedsUpdate, codeStoreBusy, codeStoreUnusable}, code)
}

// storeInstantLayout is how the store writes an instant: UTC with three
// digits of fraction, which sorts as text.
const storeInstantLayout = "2006-01-02T15:04:05.000Z"

// storeInstant returns t as the store writes an instant, the form of a
// row's time in the ledger's order (history.LedgerKey), or "" for none, as
// the store keeps no time.
func storeInstant(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(storeInstantLayout)
}

// rowTime returns t as a row's time: none for the zero time, and for
// midnight in New York, which Schwab sends for a row it gives no time of
// day, as the CLI shows none.
func rowTime(t time.Time) *Instant {
	if t.IsZero() || lookup.NewYorkMidnight(t) {
		return nil
	}
	return instantPtr(t)
}

// moneyCents returns cents as an amount, or none for nil.
func moneyCents(cents *int64) lookup.Money {
	if cents == nil {
		return lookup.Money{}
	}
	return lookup.FromCents(*cents)
}

// qtyMicros returns micros as a quantity, or none for nil.
func qtyMicros(micros *int64) lookup.Qty {
	if micros == nil {
		return lookup.Qty{}
	}
	return lookup.QtyFromMicros(*micros)
}

// priceAt returns p as a price, or none for nil.
func priceAt(p *float64) lookup.Price {
	if p == nil {
		return lookup.Price{}
	}
	return lookup.PriceOf(*p)
}

// identList returns ss as identifiers, [] for none.
func identList(ss []string) []Ident {
	out := make([]Ident, len(ss))
	for i, s := range ss {
		out[i] = Ident(s)
	}
	return out
}

// freeText returns s, text that Schwab wrote, masked as free text, or nil
// for "".
func (c *call) freeText(s string) *FreeText {
	if s == "" {
		return nil
	}
	return new(c.mask.free(s))
}

// storedText returns s, text that the store keeps, masked as the store's
// text, or nil for "".
func (c *call) storedText(s string) *FreeText {
	if s == "" {
		return nil
	}
	return new(c.mask.stored(s))
}

// schwabRow returns t, a row read from Schwab of the account a, with e, what
// the store derives of it, as a row of an answer; full adds its detail.
func (c *call) schwabRow(a dirAccount, t schwab.Transaction, e history.APIEntry, full bool) txnRow {
	r := txnRow{
		Ref: Ident(e.Ref), ReadFrom: fromSchwab, RowKind: "api", Account: c.mask.masked(a.number),
		AccountHash: identPtr(string(a.hash)), Date: Date(e.Day), Time: rowTime(t.Time),
		Type: identPtr(string(t.Type)), Status: identPtr(string(t.Status)), Category: identPtr(e.Category),
		Symbol: identPtr(e.Symbol), Quantity: qtyMicros(e.QuantityMicros), Price: priceAt(e.Price),
		Amount: moneyCents(e.AmountCents), Fees: moneyCents(e.FeesCents), Description: c.freeText(t.Description),
		Flags: identList(e.Flags),
	}
	if !full {
		return r
	}
	d := &txnDetail{
		SettlementDate: datePtr(t.SettlementDay()), SubAccount: identPtr(t.SubAccount),
		MatchedWith: []Ident{}, Problems: []problem{}, Legs: []txnLeg{},
	}
	if t.OrderID != 0 {
		d.OrderID = new(Ident(strconv.FormatInt(t.OrderID, 10)))
	}
	for _, p := range t.Problems {
		d.Problems = append(d.Problems, problem{Path: Ident(p.Path), What: c.mask.free(p.What)})
	}
	priced := pricedLegs(t)
	for i, it := range t.TransferItems {
		in := it.Instrument
		l := txnLeg{
			AssetType: identPtr(string(in.AssetType)), Symbol: identPtr(in.Symbol), Description: c.freeText(in.Description),
			Cost: lookup.Amount(it.Cost), FeeType: identPtr(it.FeeType), PositionEffect: identPtr(it.PositionEffect),
			PutCall: identPtr(in.PutCall), Expiration: datePtr(schwab.DateInNewYork(in.ExpirationDate)),
		}
		if in.AssetType == schwab.AssetCurrency {
			l.Cash = lookup.Amount(it.Amount)
		} else {
			l.Quantity = lookup.QtyOf(it.Amount)
		}
		if priced[i] {
			l.Price = lookup.PriceOf(it.Price)
		}
		if in.StrikePrice != 0 {
			l.Strike = lookup.PriceOf(in.StrikePrice)
		}
		d.Legs = append(d.Legs, l)
	}
	r.Detail = d
	return r
}

// pricedLegs returns whether each of t's legs has a price in its raw JSON,
// as the store reads them: the record reads a leg without one as 0.
func pricedLegs(t schwab.Transaction) []bool {
	var raw struct {
		TransferItems []struct {
			Price json.RawMessage `json:"price"`
		} `json:"transferItems"`
	}
	_ = json.Unmarshal(t.Raw, &raw) // what does not read has no price
	out := make([]bool, len(t.TransferItems))
	for i := range out {
		out[i] = i < len(raw.TransferItems) && len(raw.TransferItems[i].Price) > 0 && string(raw.TransferItems[i].Price) != "null"
	}
	return out
}

// storeRow returns x, a row of the local history of the account a, with
// legs, its legs as the store keeps them, as a row of an answer; full adds
// its detail.
func (c *call) storeRow(a dirAccount, x history.LedgerRow, legs []history.LegRow, full bool) txnRow {
	r := txnRow{
		Ref: Ident(x.Ref), ReadFrom: fromStore, RowKind: Ident(x.Source), Account: c.mask.masked(a.number),
		AccountHash: identPtr(string(a.hash)), Date: Date(x.Day), Time: rowTime(x.Time), Status: identPtr(x.Status),
		Category: identPtr(x.Category), Symbol: identPtr(x.Symbol), Quantity: qtyMicros(x.QuantityMicros),
		Price: priceAt(x.Price), Amount: moneyCents(x.AmountCents), Fees: moneyCents(x.FeesCents),
		Description: c.storedText(x.Description), Counted: new(x.Counted), CountedWhy: identPtr(x.CountedWhy),
		Flags: identList(x.Flags),
	}
	if x.Source == "api" {
		r.Type = identPtr(x.Kind)
	} else {
		r.Action = c.storedText(x.Kind)
	}
	if !full {
		return r
	}
	d := &txnDetail{
		PairKind: identPtr(x.PairKind), PairRef: identPtr(x.PairRef), MatchedWith: identList(x.MatchedWith),
		Problems: []problem{}, Legs: []txnLeg{},
	}
	for _, l := range legs {
		d.Legs = append(d.Legs, txnLeg{
			AssetType: identPtr(l.AssetType), Symbol: identPtr(l.Symbol), Quantity: qtyMicros(l.QuantityMicros),
			Price: priceAt(l.Price), Cost: moneyCents(l.CostCents), Cash: moneyCents(l.CashCents),
			FeeType: identPtr(l.FeeType), PositionEffect: identPtr(l.PositionEffect), PutCall: identPtr(l.PutCall),
			Strike: priceAt(l.Strike), Expiration: datePtr(l.ExpirationDay),
		})
	}
	r.Detail = d
	return r
}

// getTransactionIn is get_transaction's arguments.
type getTransactionIn struct {
	ID         string `json:"id"`
	Account    string `json:"account,omitempty"`
	IncludeRaw bool   `json:"include_raw,omitempty"`
}

// getTransactionOut is get_transaction's answer.
type getTransactionOut struct {
	envelope
	cuts                        // legs cut to fit the cap, after raw (design 8.3)
	Transaction txnRow          `json:"transaction"`       // its detail always set
	Store       *storeView      `json:"store"`             // null without a store, or when it does not hold the ref
	Raw         json.RawMessage `json:"raw" masked:"json"` // null unless include_raw and read from Schwab, as masker.json rewrote it
}

// dropRaw drops Schwab's JSON, the first thing to go when the answer
// passes the cap.
func (o *getTransactionOut) dropRaw() bool {
	had := o.Raw != nil
	o.Raw = nil
	return had
}

// lists returns the transaction's legs.
func (o *getTransactionOut) lists() []cuttable {
	d := o.Transaction.Detail
	if d == nil {
		return nil
	}
	return []cuttable{{
		path: "transaction.detail.legs", value: d.Legs,
		count: func() int { return len(d.Legs) }, keep: func(n int) { d.Legs = d.Legs[:n] },
	}}
}

// A storeView is what the local history says of a transaction.
type storeView struct {
	Counted     bool      `json:"counted"`
	CountedWhy  *Ident    `json:"counted_why"`
	Category    Ident     `json:"category"`
	Rule        Ident     `json:"category_rule"` // "user" when the user set it
	PairKind    *Ident    `json:"pair_kind"`
	PairRef     *Ident    `json:"pair_ref"`
	MatchedWith []Ident   `json:"matched_with"`
	Flags       []flagRow `json:"flags"`
}

// A flagRow is a flag of the local history on a transaction: its code, its
// severity, attention or info, and its detail, which the store composed of
// masked text and identifiers.
type flagRow struct {
	Code     Ident     `json:"code"`
	Severity Ident     `json:"severity"`
	Detail   *FreeText `json:"detail"`
}

// The shapes of get_transaction's id: Schwab's activity ID, with or
// without its T; a ref of an API row without one, T~ and its time and
// digest; and a ref of History files' rows, C, hex digits, a dot and the
// row.
var (
	activityRef = regexp.MustCompile(`^[Tt]?([0-9]{1,19})$`)
	tildeRef    = regexp.MustCompile(`^[Tt]~[0-9A-Za-z.]{1,62}$`)
	fileRowRef  = regexp.MustCompile(`^[Cc][0-9A-Fa-f]{1,52}\.[0-9]{1,9}$`)
)

// The store's refs exactly, as internal/history makes them: T~, the row's
// time in UTC or none, a dot and 12 hex digits of its digest, and .2, .3
// and so on for a second and third row of that time and digest; and C, at
// least 8 hex digits of its file's digest, a dot and the row.
var (
	tildeShape = regexp.MustCompile(`^T~(?:[0-9]{8}T[0-9]{6}|none)\.[0-9a-f]{12}(?:\.[0-9]{1,9})?$`)
	fileShape  = regexp.MustCompile(`^C[0-9a-f]{8,64}\.[0-9]{1,9}$`)
)

// A txnID is get_transaction's id: the ref that the store gives it, and
// Schwab's activity ID, 0 for a ref that only the store knows.
type txnID struct {
	given, ref string
	activity   int64
}

// txnIDOf reads id, get_transaction's argument.
func (c *call) txnIDOf(id string) (txnID, error) {
	id = strings.TrimSpace(id)
	bad := c.failf(codeBadArgument, "id takes a ref from get_transactions (T and digits, T~..., or C...), or Schwab's activity ID.")
	switch {
	case activityRef.MatchString(id):
		n, err := strconv.ParseInt(activityRef.FindStringSubmatch(id)[1], 10, 64)
		if err != nil || n == 0 {
			return txnID{}, bad
		}
		return txnID{given: id, ref: "T" + strconv.FormatInt(n, 10), activity: n}, nil
	case tildeRef.MatchString(id), fileRowRef.MatchString(id):
		return txnID{given: id, ref: id}, nil
	}
	return txnID{}, bad
}

// shown returns the id as an error may echo it (design 6.3): whole when it
// has the shape of what it names, one of the store's refs exactly or an
// activity ID whose value has 10 or more digits, and holds no account
// number the server knows; anything else as lookup.Selector.Shown shows a
// number, so that a full account number given as an id, alone or after T~
// or C, or with zeros before it, shows only its last three digits.
func (id txnID) shown(c *call) FreeText {
	whole := id.activity >= 1_000_000_000 || id.activity == 0 && (tildeShape.MatchString(id.given) || fileShape.MatchString(id.given))
	if whole && !c.mask.known.Contains(id.given) {
		return c.mask.compose("%s", Ident(id.given))
	}
	return c.mask.free(lookup.Selector{Given: id.given}.Shown())
}

// getTransaction answers get_transaction: Schwab's row of an activity ID,
// with what the local history says of it, or the local history's own row
// of a ref that only it knows, or when Schwab cannot be read.
func getTransaction(ctx context.Context, c *call, in getTransactionIn) (getTransactionOut, error) {
	var out getTransactionOut
	id, err := c.txnIDOf(in.ID)
	if err != nil {
		return out, err
	}
	v, err := c.directory(ctx)
	if err != nil {
		return out, err
	}
	var found []history.EntryDetail
	storeErr := c.store.read(ctx, func(r *history.Reader) error {
		var err error
		found, err = r.EntryByRef(ctx, id.ref)
		return err
	})
	if storeErr != nil && ctx.Err() != nil {
		return out, storeErr
	}
	if id.activity == 0 && len(found) == 0 {
		if storeErr != nil {
			return out, c.storeError(storeErr)
		}
		return out, c.failf(codeNotFound, "The local history has no transaction %s.", id.shown(c))
	}
	if storeErr != nil && !errors.Is(storeErr, history.ErrNoStore) && !errors.Is(storeErr, errNoStorePath) {
		c.warn("store_unavailable", "The local history could not be read, so its view of the transaction is left out: %s",
			c.errText(c.s.toolError(c.tool, storeErr)))
	}

	a, err := pickTxnAccount(c, v, in.Account, found)
	if err != nil {
		return out, err
	}
	var entry *history.EntryDetail
	for i := range found {
		if a.storeID != 0 && found[i].Entry.AccountID == a.storeID {
			entry = &found[i]
		}
	}
	if entry != nil {
		out.Store = c.storeViewOf(*entry)
	}
	if id.activity == 0 {
		if entry == nil {
			return out, c.failf(codeNotFound, "The local history has no transaction %s in account %s.", id.shown(c), c.mask.masked(a.number))
		}
		out.Source, out.Transaction = sourceStore, c.storeRow(a, entry.Entry, entry.Legs, true)
		return out, nil
	}

	// An account without a hash is one that Schwab no longer shares, or
	// that the store alone knows because Schwab could not be read.
	var t schwab.Transaction
	if a.hash == "" && v.schwabErr != nil {
		err = v.schwabErr
	} else {
		t, err = askAccount(ctx, c, a, func(cl *schwab.Client, hash schwab.AccountHash) (schwab.Transaction, error) {
			return cl.Transaction(ctx, hash, id.activity)
		})
	}
	if err != nil && ctx.Err() != nil {
		return out, err
	}
	if err != nil {
		e := c.s.toolError(c.tool, err)
		if se, ok := errors.AsType[*schwab.Error](err); ok && se.Status == http.StatusNotFound {
			e = &toolErr{code: codeNotFound, msg: c.mask.compose("Schwab has no transaction %s in account %s.", id.shown(c), c.mask.masked(a.number))}
		}
		if entry == nil {
			return out, e
		}
		c.warn("api_unavailable", "Schwab did not send the transaction, so this is the local history's row of it: %s", c.errText(e))
		out.Source, out.Transaction = sourceStore, c.storeRow(a, entry.Entry, entry.Legs, true)
		return out, nil
	}
	entries, err := history.DeriveAPIEntries([]schwab.Transaction{t}, a.number)
	if err != nil {
		return out, err
	}
	out.Source, out.Transaction = sourceSchwab, c.schwabRow(a, t, entries[0], true)
	if out.Store != nil {
		out.Source = sourceSchwabStore
	}
	if len(t.Problems) > 0 {
		c.warnProblems("The transaction", [][]schwab.Problem{t.Problems}, "a figure that Schwab sent otherwise is null")
	}
	if in.IncludeRaw {
		raw, report, err := c.mask.json(t.Raw, schwab.TransactionKeyClass)
		if err != nil {
			return out, err
		}
		c.s.log.Debug("masked a transaction's raw JSON", "tool", c.tool, "account_numbers", report.AccountNumbers,
			"withheld", report.Withheld, "unknown_integers", report.UnknownIntegers)
		out.Raw = raw
	}
	return out, nil
}

// pickTxnAccount returns the account of get_transaction's transaction: the
// one arg names; else the one whose ref the local history found; else the
// only account that Schwab shares. Two accounts that hold the ref, or
// several shared accounts and no ref found, need arg.
func pickTxnAccount(c *call, v *dirView, arg string, found []history.EntryDetail) (dirAccount, error) {
	if strings.TrimSpace(arg) != "" {
		if v.schwabErr != nil && len(v.accounts) == 0 {
			return dirAccount{}, v.schwabErr
		}
		picked, err := v.pick(c, arg, true)
		if err != nil {
			return dirAccount{}, err
		}
		return picked[0], nil
	}
	var holders []dirAccount
	for _, d := range found {
		for _, a := range v.accounts {
			if a.storeID != 0 && a.storeID == d.Entry.AccountID {
				holders = append(holders, a)
			}
		}
	}
	switch {
	case len(holders) == 1:
		return holders[0], nil
	case len(holders) > 1:
		names := make([]FreeText, len(holders))
		for i, a := range holders {
			names[i] = c.mask.compose("%s", c.mask.masked(a.number))
		}
		return dirAccount{}, c.failf(codeBadArgument, "account is needed: the local history holds this id in %s; pass account.",
			c.andList(names))
	case v.schwabErr != nil:
		return dirAccount{}, v.schwabErr
	}
	shared := v.shared()
	if len(shared) == 1 {
		return shared[0], nil
	}
	return dirAccount{}, c.failf(codeBadArgument, "account is needed: the local history does not hold this id, "+
		"and the login shares %d accounts; pass account.", len(shared))
}

// storeViewOf returns what the local history says of d, an entry with its
// legs and flags.
func (c *call) storeViewOf(d history.EntryDetail) *storeView {
	x := d.Entry
	sv := &storeView{
		Counted: x.Counted, CountedWhy: identPtr(x.CountedWhy), Category: Ident(x.Category), Rule: Ident(x.Rule),
		PairKind: identPtr(x.PairKind), PairRef: identPtr(x.PairRef), MatchedWith: identList(x.MatchedWith),
		Flags: []flagRow{},
	}
	for _, f := range d.Flags {
		sv.Flags = append(sv.Flags, flagRow{Code: Ident(f.Code), Severity: Ident(f.Severity), Detail: c.storedText(f.Detail)})
	}
	return sv
}
