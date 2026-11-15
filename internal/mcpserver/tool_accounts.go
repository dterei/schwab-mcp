package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/nyse"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The account tools (design 10.2 to 10.4) answer from Schwab's accounts
// endpoint with the figures that internal/lookup works out for schwabtx's
// accounts and balances, so that the two never disagree, and name each
// account as the directory does. Their values are as of the request, which
// outside a regular session follow extended-hours prices: every answer
// with values says so.

// registerAccounts registers the account tools, list_accounts,
// get_balances and get_positions; the account resources, which answer as
// list_accounts and get_positions do; and portfolio_review, the prompt
// that asks for them.
func (s *Server) registerAccounts() {
	addTool(s, toolDef{
		name:  "list_accounts",
		title: "Schwab accounts",
		description: "The brokerage accounts this Schwab login shares: masked number (... and the last three digits), " +
			"account hash, nickname, CASH or MARGIN, whether it is the primary account, the kind the user set in the local " +
			"history (ira, taxable, ...), and the current value and cash, with totals. Other tools take an account by its " +
			"hash, its last digits or its nickname. Values are as of now, at extended-hours prices outside the session. " +
			"A figure Schwab did not send is null, and so is a total that needs it.",
		slot: true,
	}, s.listAccounts)
	addTool(s, toolDef{
		name:  "get_balances",
		title: "Schwab balances",
		description: "Each account's value (liquidation value), cash and buying power, and the totals of value and cash, " +
			"the same figures as `schwabtx balances`. Buying power is a margin account's buying power for stock, or a cash " +
			"account's cash available for trading; it has no total. A figure Schwab did not send is null, and so is a cash " +
			"that is not the value less the holdings. detail full adds each current balance figure that Schwab sent " +
			"and schwabtx reads, by Schwab's name.",
		slot: true,
	}, s.getBalances)
	addTool(s, toolDef{
		name:  "get_positions",
		title: "Schwab positions",
		description: "What each account holds, the same figures as `schwabtx positions`: quantity (negative when short), " +
			"average price, market value, cost (value less open profit or loss) and open profit or loss (positive for a gain), " +
			"each position's share of its account's value and of every shared account's value (fractions), and the totals. " +
			"symbol keeps one symbol and its options. Cash is not a position (see get_balances). A figure Schwab did not send " +
			"is null. Paged: pass next_cursor back as cursor.",
		slot: true,
		args: map[string]argRule{
			"symbol": {desc: "One symbol, in any case: the positions whose symbol or underlying is it, such as a stock and its options."},
			"limit":  {desc: "The most positions a page holds, 1 to 200; 100 when left out.", min: new(1.0), max: new(200.0)},
		},
	}, s.getPositions)
	s.registerAccountResources()
	s.registerPrompts()
}

// listAccountsOut is list_accounts' answer.
type listAccountsOut struct {
	envelope
	Accounts   []accountRow `json:"accounts"`
	TotalValue lookup.Money `json:"total_value"`
	TotalCash  lookup.Money `json:"total_cash"`
}

// accountRow is one account of list_accounts.
type accountRow struct {
	accountRef
	StoreRef *Ident `json:"store_ref"` // "A1"
	Type     *Ident `json:"type"`      // CASH or MARGIN, as Schwab sends it
	// Kind is the kind the user's account decision gives the account in the
	// local history, such as ira or taxable: text of the user's, which the
	// store masked as free text when it stored it.
	Kind           *FreeText    `json:"kind"`
	Primary        *bool        `json:"primary"`
	Value          lookup.Money `json:"value"`
	Cash           lookup.Money `json:"cash"`
	NotSharedSince *Date        `json:"not_shared_since"`
}

// maxAccounts is the most accounts that list_accounts lists.
const maxAccounts = 50

// listAccounts answers list_accounts: Schwab's accounts, with their values,
// joined with the store's for their refs and kinds. When Schwab cannot be
// read for a reason of the login or the app's set-up, it answers from the
// store, without values.
func (s *Server) listAccounts(ctx context.Context, c *call, _ struct{}) (listAccountsOut, error) {
	v, err := c.directory(ctx)
	if err != nil {
		return listAccountsOut{}, err
	}
	if v.schwabErr != nil {
		return c.accountsFromStore(ctx, v.schwabErr)
	}
	accounts, err := ask(ctx, c, func(cl *schwab.Client) ([]schwab.Account, error) { return cl.Accounts(ctx) })
	if err != nil {
		return c.accountsFromStore(ctx, err)
	}
	if err := c.someAccounts(accounts); err != nil {
		return listAccountsOut{}, err
	}
	if len(accounts) > maxAccounts {
		return listAccountsOut{}, c.failf(codeTooBig, "The login shares %d accounts, more than the %d that list_accounts lists; "+
			"get_balances takes one account at a time.", len(accounts), maxAccounts)
	}
	dirs, err := c.joined(ctx, v, accounts)
	if err != nil {
		return listAccountsOut{}, err
	}
	out := listAccountsOut{
		envelope: envelope{Source: sourceSchwab}, Accounts: []accountRow{},
		TotalValue: lookup.FromCents(0), TotalCash: lookup.FromCents(0),
	}
	c.warnExtendedHours()
	problems := make([][]schwab.Problem, len(accounts))
	for i, a := range accounts {
		d := dirs[i]
		b := lookup.BalanceOf(a)
		r := accountRow{
			accountRef: c.ref(d), StoreRef: identPtr(d.storeRef), Type: identPtr(a.SecuritiesAccount.Type),
			Primary: d.primary, Value: b.Value, Cash: b.Cash,
		}
		if d.kind != "" {
			r.Kind = new(c.mask.stored(d.kind))
		}
		out.Accounts = append(out.Accounts, r)
		out.TotalValue, out.TotalCash = out.TotalValue.Plus(b.Value), out.TotalCash.Plus(b.Cash)
		c.warnMissing(r.accountRef, b, false)
		problems[i] = lookup.Problems(a)
	}
	c.warnAccountProblems(problems)
	if v.storeErr != nil {
		c.warn("store_unavailable", "The local history could not be read, so store_ref and kind are null: %s",
			c.errText(s.toolError(c.tool, v.storeErr)))
	}
	return out, nil
}

// accountsFromStore answers list_accounts from the store's accounts, with
// no values, when cause, why Schwab could not be read, is a reason of the
// login or the app's set-up, which the user must act on. It fails with
// cause otherwise, or when the store does not open or knows no account.
func (c *call) accountsFromStore(ctx context.Context, cause error) (listAccountsOut, error) {
	e := c.s.toolError(c.tool, cause)
	if !slices.Contains(loginCodes, e.code) {
		return listAccountsOut{}, e
	}
	numbered, views, err := c.store.accounts(ctx)
	if err != nil || len(numbered) == 0 {
		return listAccountsOut{}, e
	}
	numberOf := map[int64]string{}
	for _, n := range numbered {
		numberOf[n.ID] = n.Number
	}
	out := listAccountsOut{envelope: envelope{Source: sourceStore}, Accounts: []accountRow{}}
	c.warn("api_unavailable", "Schwab could not be read, so the accounts come from the local history, "+
		"without values or hashes: %s", c.errText(e))
	for _, a := range views {
		number, ok := numberOf[a.ID]
		if !ok {
			continue
		}
		r := accountRow{
			accountRef: c.ref(dirAccount{number: number, nickname: a.Nickname, storeNickname: true}),
			StoreRef:   identPtr(a.Ref), Type: identPtr(a.Type), Primary: a.Primary,
		}
		if a.Kind != "" {
			r.Kind = new(c.mask.stored(a.Kind))
		}
		if !a.NotSharedSince.IsZero() {
			r.NotSharedSince = datePtr(nytime.DateOf(a.NotSharedSince))
		}
		out.Accounts = append(out.Accounts, r)
	}
	return out, nil
}

// loginCodes are the codes of a failure to read Schwab that the user must
// act on, the login or the app's set-up, beside which list_accounts answers
// from the store.
var loginCodes = []errorCode{
	codeNotConfigured, codeConfigUnreadable, codeNotLoggedIn, codeLoginExpired, codeAppRejected, codeAppNotReady, codeTokenRefused,
}

// someAccounts fails when Schwab answered no accounts, as when none was
// ticked at the login.
func (c *call) someAccounts(as []schwab.Account) error {
	if len(as) == 0 {
		return c.failf(codeSchwabRefused, "Schwab returned no accounts. Ask the user to run schwabtx login again and tick the accounts to share.")
	}
	return nil
}

// joined returns the directory's account for each of as, Schwab's accounts
// in its answer, by number. When the directory does not know one, which a
// login since it was read can do, it reads the directory again, once; an
// account that it still does not know is named by its number alone.
func (c *call) joined(ctx context.Context, v *dirView, as []schwab.Account) ([]dirAccount, error) {
	find := func(v *dirView) ([]dirAccount, string) {
		byNumber := map[string]dirAccount{}
		for _, a := range v.shared() {
			byNumber[a.number] = a
		}
		out := make([]dirAccount, len(as))
		missing := ""
		for i, a := range as {
			number := a.SecuritiesAccount.AccountNumber
			d, ok := byNumber[number]
			if !ok {
				d = dirAccount{number: number}
				if number != "" && missing == "" {
					missing = number
				}
			}
			out[i] = d
		}
		return out, missing
	}
	out, missing := find(v)
	if missing == "" {
		return out, nil
	}
	if _, _, err := c.accounts.refreshed(ctx, c, missing); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return out, nil
	}
	again, err := c.directory(ctx)
	if err != nil {
		return nil, err
	}
	out, _ = find(again)
	return out, nil
}

// current returns a as v, the directory as read now, knows it by its
// number: with the hash that a read since a was picked gives it, as after
// Schwab refused its hash.
func current(v *dirView, a dirAccount) dirAccount {
	for _, d := range v.shared() {
		if d.number == a.number {
			return d
		}
	}
	return a
}

// warnExtendedHours says that values are at extended-hours prices, when the
// call is outside a regular session.
func (c *call) warnExtendedHours() {
	if _, open := nyse.During(c.now); !open {
		c.warn("extended_hours", "Values are as of now, outside the regular session, at extended-hours prices.")
	}
}

// accountLabel names an account in a warning: its masked number, and its
// nickname when it has one.
func (c *call) accountLabel(r accountRef) FreeText {
	if r.Nickname != nil {
		return c.mask.compose("%s (%s)", r.Account, *r.Nickname)
	}
	return c.mask.compose("%s", r.Account)
}

// warnMissing says what of b, the balance of the account r, Schwab did not
// send, as schwabtx's tables do, buying power too when buyingPower asks;
// and when its cash is null because it is not the value less the holdings.
func (c *call) warnMissing(r accountRef, b lookup.Balance, buyingPower bool) {
	var missing []string
	for _, f := range []struct {
		name string
		no   bool
	}{{"value", b.NoValue}, {"cash", b.NoCash}, {"buying power", buyingPower && b.NoBuyingPower}} {
		if f.no {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		c.warn("figures_missing", "Schwab sent no current %s for %s.", Ident(orWords(missing)), c.accountLabel(r))
	}
	if b.CashDiffers {
		c.warn("cash_differs", "Schwab's cash for %s is not its value less its holdings, so it is null.", c.accountLabel(r))
	}
}

// orWords joins words as "a", "a or b", or "a, b or c".
func orWords(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// warnAccountProblems adds the warning problems for the accounts whose
// records, one list of problems each, differed from what Schwab has sent
// before.
func (c *call) warnAccountProblems(problems [][]schwab.Problem) {
	n := 0
	for _, ps := range problems {
		if len(ps) > 0 {
			n++
		}
	}
	what := strconv.Itoa(n) + " accounts"
	if n == 1 {
		what = "1 account"
	}
	c.warnProblems(what, problems, "a figure that could not be read is null")
}

// balancesIn is get_balances' arguments.
type balancesIn struct {
	Account string `json:"account,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// getBalancesOut is get_balances' answer. With detail full, each account's
// figures may be cut to fit the cap; the values and the totals never are.
type getBalancesOut struct {
	envelope
	cuts
	Accounts   []balanceRow `json:"accounts"`
	TotalValue lookup.Money `json:"total_value"`
	TotalCash  lookup.Money `json:"total_cash"`
}

// balanceRow is one account's balance.
type balanceRow struct {
	accountRef
	Type          *Ident       `json:"type"`
	Value         lookup.Money `json:"value"`
	Cash          lookup.Money `json:"cash"`
	BuyingPower   lookup.Money `json:"buying_power"`
	BuyingPowerIs *Ident       `json:"buying_power_is"`   // "margin_buying_power" or "cash_available_for_trading"
	Figures       []figure     `json:"figures,omitempty"` // detail full: the current balances, by name
}

// figure is one of an account's current balances, under Schwab's name for
// it.
type figure struct {
	Name  Ident        `json:"name"`
	Value lookup.Money `json:"value"`
}

// dropRaw drops nothing: get_balances holds none of Schwab's JSON.
func (o *getBalancesOut) dropRaw() bool { return false }

// lists returns each account's figures.
func (o *getBalancesOut) lists() []cuttable {
	var ls []cuttable
	for i := range o.Accounts {
		r := &o.Accounts[i]
		ls = append(ls, cuttable{
			path: fmt.Sprintf("accounts.%d.figures", i), value: r.Figures,
			count: func() int { return len(r.Figures) }, keep: func(n int) { r.Figures = r.Figures[:n] },
		})
	}
	return ls
}

// cutHint says how to see an account's figures in full.
func (o *getBalancesOut) cutHint() string { return "Pass account for one account's figures in full." }

// getBalances answers get_balances: the accounts that the account argument
// picks, from one request, with lookup.BalanceOf's figures.
func (s *Server) getBalances(ctx context.Context, c *call, in balancesIn) (getBalancesOut, error) {
	picked, err := c.pickAccounts(ctx, in.Account)
	if err != nil {
		return getBalancesOut{}, err
	}
	one := strings.TrimSpace(in.Account) != ""
	var accounts []schwab.Account
	if one {
		var a schwab.Account
		a, err = askAccount(ctx, c, picked[0], func(cl *schwab.Client, hash schwab.AccountHash) (schwab.Account, error) {
			return cl.Account(ctx, hash)
		})
		accounts = []schwab.Account{a}
	} else {
		accounts, err = ask(ctx, c, func(cl *schwab.Client) ([]schwab.Account, error) { return cl.Accounts(ctx) })
	}
	if err != nil {
		return getBalancesOut{}, err
	}
	if err := c.someAccounts(accounts); err != nil {
		return getBalancesOut{}, err
	}
	v, err := c.directory(ctx)
	if err != nil {
		return getBalancesOut{}, err
	}
	var dirs []dirAccount
	if one {
		dirs = []dirAccount{current(v, picked[0])}
	} else if dirs, err = c.joined(ctx, v, accounts); err != nil {
		return getBalancesOut{}, err
	}
	out := getBalancesOut{
		envelope: envelope{Source: sourceSchwab}, Accounts: []balanceRow{},
		TotalValue: lookup.FromCents(0), TotalCash: lookup.FromCents(0),
	}
	c.warnExtendedHours()
	problems := make([][]schwab.Problem, len(accounts))
	for i, a := range accounts {
		b := lookup.BalanceOf(a)
		r := balanceRow{
			accountRef: c.ref(dirs[i]), Type: identPtr(a.SecuritiesAccount.Type),
			Value: b.Value, Cash: b.Cash, BuyingPower: b.BuyingPower,
		}
		switch a.SecuritiesAccount.Type {
		case "MARGIN":
			r.BuyingPowerIs = new(Ident("margin_buying_power"))
		case "CASH":
			r.BuyingPowerIs = new(Ident("cash_available_for_trading"))
		}
		if in.Detail == "full" {
			r.Figures = balanceFigures(a)
		}
		out.Accounts = append(out.Accounts, r)
		out.TotalValue, out.TotalCash = out.TotalValue.Plus(b.Value), out.TotalCash.Plus(b.Cash)
		c.warnMissing(r.accountRef, b, true)
		problems[i] = lookup.Problems(a)
	}
	c.warnAccountProblems(problems)
	return out, nil
}

// positionsIn is get_positions' arguments.
type positionsIn struct {
	Account string `json:"account,omitempty"`
	Symbol  string `json:"symbol,omitempty"`
	Detail  string `json:"detail,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	Cursor  string `json:"cursor,omitempty"`
}

// getPositionsOut is get_positions' answer: a page of the positions that
// the call matches, and on every page the accounts it picked and the
// totals of every position it matches.
type getPositionsOut struct {
	envelope
	page
	Positions []positionRow     `json:"positions"`
	Accounts  []positionAccount `json:"accounts"` // every selected account, on every page
	Total     positionTotal     `json:"total"`
}

// positionRow is one position, with lookup.HoldingOf's figures.
type positionRow struct {
	Account         MaskedNumber    `json:"account"`
	AccountHash     Ident           `json:"account_hash"`
	Symbol          *Ident          `json:"symbol"`
	AssetType       *Ident          `json:"asset_type"`
	Quantity        lookup.Qty      `json:"quantity"`
	AveragePrice    lookup.Price    `json:"average_price"`
	Value           lookup.Money    `json:"value"`
	Cost            lookup.Money    `json:"cost"`
	OpenPL          lookup.Money    `json:"open_pl"`
	AccountWeight   Num             `json:"account_weight"`   // value / the account's value, 4 places
	PortfolioWeight Num             `json:"portfolio_weight"` // value / every shared account's value, 4 places
	Description     *FreeText       `json:"description"`
	Detail          *positionDetail `json:"detail,omitempty"` // detail full
}

// positionDetail is what detail full adds to a position.
type positionDetail struct {
	CUSIP          *Ident       `json:"cusip"`
	PutCall        *Ident       `json:"put_call"`
	Underlying     *Ident       `json:"underlying"`
	LongQuantity   lookup.Qty   `json:"long_quantity"`
	ShortQuantity  lookup.Qty   `json:"short_quantity"`
	MaintenanceReq lookup.Money `json:"maintenance_requirement"`
	Problems       []problem    `json:"problems"`
}

// positionAccount is one account that get_positions picked, with the sums
// of the positions of it that the call matches.
type positionAccount struct {
	accountRef
	Value     lookup.Money `json:"value"`     // the account's liquidation value
	Positions *int         `json:"positions"` // null when its positions could not be read
	Market    lookup.Money `json:"positions_value"`
	Cost      lookup.Money `json:"positions_cost"`
	OpenPL    lookup.Money `json:"open_pl"`
}

// positionTotal is the totals of get_positions: of the picked accounts'
// values, and of every position that the call matches.
type positionTotal struct {
	Value  lookup.Money `json:"value"`
	Market lookup.Money `json:"positions_value"`
	Cost   lookup.Money `json:"positions_cost"`
	OpenPL lookup.Money `json:"open_pl"`
}

// positionsArgs are get_positions' canonical arguments, whose digest a
// cursor holds: the defaults applied, the symbol in capitals, and the
// account as its key, never its number (design 8.5).
type positionsArgs struct {
	Account string `json:"account"`
	Symbol  string `json:"symbol"`
	Detail  string `json:"detail"`
}

// getPositions answers get_positions: the positions of the accounts that
// the account argument picks, from one request for every account, so that
// each position's share of every shared account's value is of that read;
// accounts in Schwab's order, and positions by symbol within each, as
// schwabtx's table lists them. A later page reads Schwab again, and its
// cursor is the index of the page's first position.
func (s *Server) getPositions(ctx context.Context, c *call, in positionsIn) (getPositionsOut, error) {
	limit, detail, symbol := in.Limit, in.Detail, strings.TrimSpace(in.Symbol)
	if limit == 0 {
		limit = 100
	}
	if detail == "" {
		detail = "compact"
	}
	picked, err := c.pickAccounts(ctx, in.Account)
	if err != nil {
		return getPositionsOut{}, err
	}
	one := strings.TrimSpace(in.Account) != ""
	args := positionsArgs{Symbol: strings.ToUpper(symbol), Detail: detail}
	if one {
		args.Account = picked[0].key()
	}
	var start uint
	if in.Cursor != "" {
		if err := c.readCursor(in.Cursor, args, &start); err != nil {
			return getPositionsOut{}, err
		}
	}
	accounts, err := ask(ctx, c, func(cl *schwab.Client) ([]schwab.Account, error) {
		return cl.Accounts(ctx, schwab.AccountFieldPositions)
	})
	if err != nil {
		return getPositionsOut{}, err
	}
	if err := c.someAccounts(accounts); err != nil {
		return getPositionsOut{}, err
	}
	v, err := c.directory(ctx)
	if err != nil {
		return getPositionsOut{}, err
	}
	dirs, err := c.joined(ctx, v, accounts)
	if err != nil {
		return getPositionsOut{}, err
	}
	portfolio := lookup.FromCents(0) // every shared account's value
	var picks, unvalued []int        // the accounts picked, and those not picked whose value is null
	for i, a := range accounts {
		b := lookup.BalanceOf(a)
		portfolio = portfolio.Plus(b.Value)
		_, valued := b.Value.Cents()
		switch {
		case !one || a.SecuritiesAccount.AccountNumber == picked[0].number:
			picks = append(picks, i)
		case !valued:
			unvalued = append(unvalued, i)
		}
	}
	if len(picks) == 0 {
		return getPositionsOut{}, c.failf(codeAccountNotShared, "Schwab no longer shares %s; ask the user to run schwabtx login and tick it.",
			c.mask.masked(picked[0].number))
	}

	out := getPositionsOut{
		envelope: envelope{Source: sourceSchwab}, page: page{Complete: true},
		Positions: []positionRow{}, Accounts: []positionAccount{},
		Total: positionTotal{Value: lookup.FromCents(0), Market: lookup.FromCents(0), Cost: lookup.FromCents(0), OpenPL: lookup.FromCents(0)},
	}
	c.warnExtendedHours()
	// The portfolio's weights divide by every shared account's value, the
	// accounts not picked included, whose problems the answer does not
	// list: a value that Schwab did not send, or that could not be read.
	for _, i := range unvalued {
		label := c.accountLabel(c.ref(dirs[i]))
		if lookup.BalanceOf(accounts[i]).NoValue {
			c.warn("figures_missing", "Schwab sent no current value for %s, so every portfolio_weight is null.", label)
		} else {
			c.warn("problems", "Schwab's current value for %s could not be read, so every portfolio_weight is null.", label)
		}
	}
	var rows []positionRow
	problems := make([][]schwab.Problem, len(picks))
	for k, i := range picks {
		a, ref := accounts[i], c.ref(dirs[i])
		b := lookup.BalanceOf(a)
		if b.NoValue {
			c.warn("figures_missing", "Schwab sent no current value for %s, so the weights that need it are null.", c.accountLabel(ref))
		}
		problems[k] = lookup.Problems(a)
		pa := positionAccount{accountRef: ref, Value: b.Value}
		if !lookup.Unreadable(a.Problems, "securitiesAccount.positions") {
			pa.Market, pa.Cost, pa.OpenPL = lookup.FromCents(0), lookup.FromCents(0), lookup.FromCents(0)
			// Byte order, which puts an option's OCC symbol, its root
			// padded to six characters, after its root's own; equal symbols
			// keep their order.
			ps := slices.Clone(a.SecuritiesAccount.Positions)
			slices.SortStableFunc(ps, func(x, y schwab.Position) int { return strings.Compare(x.Instrument.Symbol, y.Instrument.Symbol) })
			n := 0
			for _, p := range ps {
				if symbol != "" && !strings.EqualFold(p.Instrument.Symbol, symbol) && !strings.EqualFold(p.Instrument.UnderlyingSymbol, symbol) {
					continue
				}
				h := lookup.HoldingOf(p)
				row := positionRow{
					Account: ref.Account, AccountHash: Ident(dirs[i].hash),
					Symbol: identPtr(h.Symbol), AssetType: identPtr(string(p.Instrument.AssetType)),
					Quantity: h.Quantity, AveragePrice: h.Price, Value: h.Value, Cost: h.Cost, OpenPL: h.Gain,
					AccountWeight: weight(h.Value, b.Value), PortfolioWeight: weight(h.Value, portfolio),
				}
				if h.Description != "" {
					row.Description = new(c.mask.free(h.Description))
				}
				if detail == "full" {
					row.Detail = c.positionDetail(p)
				}
				rows = append(rows, row)
				n++
				pa.Market, pa.Cost, pa.OpenPL = pa.Market.Plus(h.Value), pa.Cost.Plus(h.Cost), pa.OpenPL.Plus(h.Gain)
			}
			pa.Positions = &n
		}
		out.Accounts = append(out.Accounts, pa)
		t := &out.Total
		t.Value, t.Market, t.Cost, t.OpenPL = t.Value.Plus(b.Value), t.Market.Plus(pa.Market), t.Cost.Plus(pa.Cost), t.OpenPL.Plus(pa.OpenPL)
	}
	c.warnAccountProblems(problems)

	// The page: the positions from the cursor's on, while the answer, with
	// the warnings known so far and next_cursor at its longest, stays
	// within the page budget.
	from := min(int(start), len(rows))
	out.AsOf, out.Warnings = instantOf(c.now), slices.Clone(c.warnings)
	fixed, err := encode(out)
	if err != nil {
		return getPositionsOut{}, err
	}
	sizes := make([]int, len(rows)-from)
	for j, r := range rows[from:] {
		b, err := encode(r)
		if err != nil {
			return getPositionsOut{}, err
		}
		sizes[j] = len(b)
	}
	to := from + c.pageRows(len(fixed), sizes, limit)
	out.Positions = append(out.Positions, rows[from:to]...)
	if to < len(rows) {
		if out.NextCursor, err = c.newCursor(args, to); err != nil {
			return getPositionsOut{}, err
		}
	}
	return out, nil
}

// weight returns value's share of base, to 4 places: null when either is
// null, or when base is not above 0.
func weight(value, base lookup.Money) Num {
	v, ok := value.Cents()
	b, okBase := base.Cents()
	if !ok || !okBase || b <= 0 {
		return Num{}
	}
	return NumValue(math.Round(float64(v)/float64(b)*10_000) / 10_000)
}

// positionDetail returns what detail full adds to p: its identifiers, the
// quantities of its two sides, its maintenance requirement, null when
// Schwab did not send it, and its problems, with each open profit or loss
// that it lacks of a side that it holds.
func (c *call) positionDetail(p schwab.Position) *positionDetail {
	ps := p.Problems
	d := &positionDetail{
		CUSIP: identPtr(p.Instrument.CUSIP), PutCall: identPtr(p.Instrument.PutCall),
		Underlying: identPtr(p.Instrument.UnderlyingSymbol), Problems: []problem{},
	}
	if !lookup.Unreadable(ps, "longQuantity") {
		d.LongQuantity = lookup.QtyOf(p.LongQuantity)
	}
	if !lookup.Unreadable(ps, "shortQuantity") {
		d.ShortQuantity = lookup.QtyOf(p.ShortQuantity)
	}
	// The record names maintenanceRequirement among its problems when Schwab
	// did not send it, as it sends it on every position.
	if !lookup.Unreadable(ps, "maintenanceRequirement") {
		d.MaintenanceReq = lookup.Amount(p.MaintenanceRequirement)
	}
	_, missing := lookup.OpenPL(p)
	for _, pr := range slices.Concat(ps, missing) {
		d.Problems = append(d.Problems, problem{Path: Ident(pr.Path), What: c.mask.free(pr.What)})
	}
	return d
}

// figureNames are the names of the figures of an account's balances that
// schwab.Balances reads: its fields that hold a number, by their JSON names.
var figureNames = func() []string {
	var names []string
	for _, f := range reflect.VisibleFields(reflect.TypeFor[schwab.Balances]()) {
		if name, _, _ := strings.Cut(f.Tag.Get("json"), ","); f.Type == reflect.TypeFor[*float64]() && name != "" {
			names = append(names, name)
		}
	}
	return names
}()

// balanceFigures returns the figures of a's current balances, each under
// its name, in the order Schwab sent them: every one that schwab.Balances
// reads, as money, and null when it is not a number that the record could
// read. A name sent twice keeps its first place and its last value, as the
// record does. What the record does not read, such as a key Schwab adds,
// is left out: its name and its value come from outside.
func balanceFigures(a schwab.Account) []figure {
	var raw struct {
		SecuritiesAccount struct {
			CurrentBalances json.RawMessage `json:"currentBalances"`
		} `json:"securitiesAccount"`
	}
	if json.Unmarshal(a.Raw, &raw) != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw.SecuritiesAccount.CurrentBalances))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	figures := []figure{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := tok.(string)
		var value any
		if err := dec.Decode(&value); err != nil {
			break
		}
		i := slices.IndexFunc(figureNames, func(n string) bool { return strings.EqualFold(n, key) })
		if i < 0 || value == nil {
			continue
		}
		name := Ident(figureNames[i])
		var m lookup.Money
		if n, ok := value.(json.Number); ok {
			if f, err := n.Float64(); err == nil {
				m = lookup.Amount(f)
			}
		}
		if j := slices.IndexFunc(figures, func(f figure) bool { return f.Name == name }); j >= 0 {
			figures[j].Value = m
			continue
		}
		figures = append(figures, figure{Name: name, Value: m})
	}
	return figures
}
