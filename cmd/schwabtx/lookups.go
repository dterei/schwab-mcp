package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A lookup is one of the commands that print what Schwab reports of the
// accounts as of the request.
type lookup struct {
	positions bool // it asks for the accounts' positions
	names     bool // its table shows the accounts' nicknames
	// table returns the table of the accounts as, with full account
	// numbers if show, and a warning for each - in it that no problem of an
	// account's explains.
	table func(as []namedAccount, show bool) (cols []column, rows [][]string, warnings []string)
	// summary returns a line for stderr before the table, or is nil.
	summary func(as []namedAccount) string
}

var lookups = map[string]lookup{
	"accounts":  {names: true, table: accountsTable},
	"balances":  {names: true, table: balancesTable},
	"positions": {positions: true, table: positionsTable, summary: positionsSummary},
}

// namedAccount is an account from GET /accounts and its nickname from the
// user's preferences: "" when it has none, or they were not read. The
// account is a named field, as the package doc of internal/schwab asks.
type namedAccount struct {
	account  schwab.Account
	nickname string
}

// label names a in an error or a warning: by its masked number, whatever
// -show-accounts says, and its nickname when it is known.
func (a namedAccount) label() string {
	l := accountCell(a.account.SecuritiesAccount.AccountNumber, false)
	if n := textCell(a.nickname, 30); n != "" {
		l += " (" + n + ")"
	}
	return l
}

// runLookup runs the lookup of the given name: it reads the accounts, with
// their positions if the lookup shows them, and the user's preferences
// when the table shows the accounts' nicknames or -account gives one, and
// prints the accounts that -account picks, as a table or as Schwab's own
// JSON.
func runLookup(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	l := lookups[name]
	fs := flag.NewFlagSet("schwabtx "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	account := fs.String("account", "", "only this account: the last digits of its number, or its nickname")
	asJSON := fs.Bool("json", false, "print Schwab's raw JSON instead of a table (includes full account numbers)")
	showAccounts := fs.Bool("show-accounts", false, "print full account numbers in the table instead of the last 4 digits")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	sel := parseSelector(*account)

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	auth, err := openLogin(cfg, stderr)
	if err != nil {
		return err
	}
	c := newClient(auth)

	var fields []schwab.AccountField
	if l.positions {
		fields = append(fields, schwab.AccountFieldPositions)
	}
	accounts, err := c.accounts(ctx, fields...)
	if err != nil {
		return fmt.Errorf("fetching accounts: %w", err)
	}
	if len(accounts) == 0 {
		return errors.New("Schwab returned no accounts; run `schwabtx login` again and tick the accounts to share")
	}
	warn := func(msg string) { fmt.Fprintln(stderr, "warning:", msg) }
	as := make([]namedAccount, len(accounts))
	problems := make([][]schwab.Problem, len(accounts))
	for i, a := range accounts {
		as[i].account = a
		problems[i] = lookupProblems(a)
	}
	if w := problemsWarning("account", problems); w != "" {
		warn(w + "; the table may not show those fields as Schwab sent them, and -json prints Schwab's own JSON")
	}

	// The nicknames are in the user's preferences, a request of their own,
	// which the table's names need, and so does a nickname given to
	// -account.
	asked := sel.nickname != "" || l.names && !*asJSON
	if asked {
		prefs, err := c.preferences(ctx)
		switch {
		case err == nil:
			nameAccounts(as, prefs)
			if n, places := problemPlaces([][]schwab.Problem{nicknameProblems(prefs)}); n > 0 {
				warn("the user preferences differed from what Schwab has sent before (" + places + "); a nickname may be missing")
			}
		case ctx.Err() != nil:
			return fmt.Errorf("fetching the user preferences: %w", err)
		case sel.nickname != "":
			return fmt.Errorf("fetching the user preferences, to find the account named %q: %w", sel.shown(), err)
		default:
			warn("the table leaves out the accounts' nicknames: fetching the user preferences: " + err.Error())
		}
	}
	picked, err := sel.pick(as)
	if err != nil && !asked {
		// The error lists the accounts, by their nicknames too when they
		// can be read: they tell apart accounts whose masked numbers do
		// not, and -account takes them.
		if prefs, perr := c.preferences(ctx); perr == nil {
			nameAccounts(as, prefs)
			_, err = sel.pick(as)
		} else if ctx.Err() != nil {
			return fmt.Errorf("fetching the user preferences: %w", perr)
		}
	}
	if err != nil {
		return err
	}

	if *asJSON {
		raws := make([]json.RawMessage, len(picked))
		for i, a := range picked {
			raws[i] = a.account.Raw
		}
		return printRaw(stdout, raws)
	}
	cols, rows, warnings := l.table(picked, *showAccounts)
	for _, w := range warnings {
		warn(w)
	}
	if l.summary != nil {
		fmt.Fprintln(stderr, l.summary(picked))
	}
	return writeTable(stdout, cols, rows)
}

// lookupProblems returns the problems of a and of its positions, as
// accountProblems does, with one for each open profit or loss that a
// position lacks of a side that it holds, which Schwab sends on every
// position of that side.
func lookupProblems(a schwab.Account) []schwab.Problem {
	ps := accountProblems(a)
	for _, p := range a.SecuritiesAccount.Positions {
		_, missing := openPL(p)
		for _, m := range missing {
			ps = append(ps, schwab.Problem{Path: "securitiesAccount.positions." + m.Path, What: m.What})
		}
	}
	return ps
}

// selector is the account that -account names: by the digits that its
// number ends in, or by its nickname. All three are empty when it names
// none.
type selector struct{ given, digits, nickname string }

// parseSelector reads -account's value. Digits, with or without a leading
// "...", as the tables and Schwab mask numbers, and with dashes or spaces of
// any kind among them, as schwab.com writes a number, name the account
// whose number ends in them; a whole number names its account too.
// Anything else names the account with that nickname, ignoring case and the
// spaces around it, so a nickname of digits alone cannot be given, though
// its account's number can.
func parseSelector(v string) selector {
	v = strings.TrimSpace(v)
	s := selector{given: v}
	digits := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.Is(unicode.Pd, r) {
			return -1
		}
		return r
	}, strings.TrimPrefix(v, "..."))
	if digits != "" && strings.Trim(digits, "0123456789") == "" {
		s.digits = digits
	} else {
		s.nickname = v
	}
	return s
}

// shown returns -account's value as an error shows it: as given, but with
// no more of an account number than the tables show. Each run of more than
// 4 digits, which comes close to a whole number, shows as ... and its last
// 4, as the tables mask a number, whether the value names digits or a
// nickname. A run goes on across anything but a letter, so that the digits
// of a number written with dots, dashes or spaces of any kind among them
// make one run.
func (s selector) shown() string {
	rs := []rune(s.given)
	var b strings.Builder
	for i := 0; i < len(rs); {
		if !unicode.IsDigit(rs[i]) {
			b.WriteRune(rs[i])
			i++
			continue
		}
		var digits []rune
		end := i // just after the run's last digit
		for j := i; j < len(rs) && !unicode.IsLetter(rs[j]); j++ {
			if unicode.IsDigit(rs[j]) {
				digits, end = append(digits, rs[j]), j+1
			}
		}
		switch {
		case len(digits) <= 4:
			b.WriteString(string(rs[i:end]))
		case strings.HasSuffix(b.String(), "..."): // masked already, as the tables mask
			b.WriteString(string(digits[len(digits)-4:]))
		default:
			b.WriteString("..." + string(digits[len(digits)-4:]))
		}
		i = end
	}
	return b.String()
}

// pick returns the accounts of as that s names: all of them when it names
// none, and else the one whose number ends in its digits, or whose
// nickname is its own. It never guesses: no account, or several, is an
// error that lists the accounts.
func (s selector) pick(as []namedAccount) ([]namedAccount, error) {
	if s.given == "" {
		return as, nil
	}
	var got []namedAccount
	for _, a := range as {
		n := a.account.SecuritiesAccount.AccountNumber
		if s.digits != "" && n != "" && strings.HasSuffix(n, s.digits) ||
			s.nickname != "" && strings.EqualFold(strings.TrimSpace(a.nickname), s.nickname) {
			got = append(got, a)
		}
	}
	switch {
	case len(got) == 1:
		return got, nil
	case len(got) == 0:
		return nil, fmt.Errorf("no account matches -account %q: the login shares %s; to share another, run `schwabtx login` and tick it",
			s.shown(), labels(as))
	case s.nickname != "":
		return nil, fmt.Errorf("-account %q matches %d accounts, %s; give the last digits of its number instead", s.shown(), len(got), labels(got))
	case nicknamed(got):
		return nil, fmt.Errorf("-account %q matches %d accounts, %s; give its nickname instead", s.shown(), len(got), labels(got))
	}
	return nil, fmt.Errorf("-account %q matches %d accounts, %s; give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		s.shown(), len(got), labels(got))
}

// labels lists as by their labels.
func labels(as []namedAccount) string {
	ls := make([]string, len(as))
	for i, a := range as {
		ls[i] = a.label()
	}
	return strings.Join(ls, ", ")
}

// nicknamed reports whether each of as has a nickname, and no two the same
// as -account compares them, so that a nickname picks any one of them.
func nicknamed(as []namedAccount) bool {
	for i, a := range as {
		n := strings.TrimSpace(a.nickname)
		if n == "" {
			return false
		}
		for _, b := range as[:i] {
			if strings.EqualFold(strings.TrimSpace(b.nickname), n) {
				return false
			}
		}
	}
	return true
}

// nameAccounts gives each of as the nickName of the first account of prefs
// with its number. The preferences may list accounts that were not shared
// at login, which is not known, so an account that GET /accounts did not
// list gets no row of its own; an account that the preferences leave out
// gets no nickname.
func nameAccounts(as []namedAccount, prefs schwab.UserPreference) {
	nicknames := map[string]string{}
	for _, p := range prefs.Accounts {
		if _, seen := nicknames[p.AccountNumber]; p.AccountNumber != "" && !seen {
			nicknames[p.AccountNumber] = p.NickName
		}
	}
	for i := range as {
		if n := as[i].account.SecuritiesAccount.AccountNumber; n != "" {
			as[i].nickname = nicknames[n]
		}
	}
}

// nicknameProblems returns the problems of prefs that can cost an account
// its nickname: those at an account's number or nickname, or above them. A
// change to the streamer's details or to the offers costs none.
func nicknameProblems(prefs schwab.UserPreference) []schwab.Problem {
	var ps []schwab.Problem
	for _, p := range prefs.Problems {
		if covers(p, "accounts.nickName") || covers(p, "accounts.accountNumber") {
			ps = append(ps, p)
		}
	}
	return ps
}

// covers reports whether p is at path, or at a key above it: whether what
// Schwab sent at path could not be read, or is uncertain.
func covers(p schwab.Problem, path string) bool {
	return p.Path == "" || p.Path == path || strings.HasPrefix(path, p.Path+".")
}

// unreadable reports whether ps, the problems of a record, cover path.
func unreadable(ps []schwab.Problem, path string) bool {
	return slices.ContainsFunc(ps, func(p schwab.Problem) bool { return covers(p, path) })
}

// figure returns v, the figure at path in a record with the problems ps:
// none when Schwab did not send it, or it could not be read.
func figure(ps []schwab.Problem, path string, v *float64) money {
	if unreadable(ps, path) {
		return money{}
	}
	return moneyOf(v)
}

// unsent reports whether Schwab did not send v, the figure at path in a
// record with the problems ps, though nothing that it sent there was
// unreadable.
func unsent(ps []schwab.Problem, path string, v *float64) bool {
	return v == nil && !unreadable(ps, path)
}

// currentAt is where an account keeps its current balances.
const currentAt = "securitiesAccount.currentBalances."

// balance is what the accounts and balances tables show of an account, as
// of the request.
type balance struct {
	value, cash, buyingPower money
	// Whether Schwab did not send the figure that each of the three would
	// be, though nothing that it sent there was unreadable.
	noValue, noCash, noBuyingPower bool
	// cashDiffers is whether the cash is none because it is not what the
	// account's value leaves once its holdings are taken out.
	cashDiffers bool
}

// balanceOf returns a's balance.
func balanceOf(a schwab.Account) balance {
	ps, cur, agg := a.Problems, a.SecuritiesAccount.CurrentBalances, a.AggregatedBalance
	var b balance

	// The value is the current liquidation value, or else the aggregated
	// balance's current one, which a margin account's current balances can
	// lack. The initial balances' liquidation value is as of the start of
	// the session, and the aggregated balance's other one as of a time that
	// is not known.
	b.value = figure(ps, currentAt+"liquidationValue", cur.LiquidationValue)
	if !b.value.ok {
		b.value = figure(ps, "aggregatedBalance.currentLiquidationValue", agg.CurrentLiquidationValue)
	}
	b.noValue = unsent(ps, currentAt+"liquidationValue", cur.LiquidationValue) &&
		unsent(ps, "aggregatedBalance.currentLiquidationValue", agg.CurrentLiquidationValue)

	// A cash that is not what the value leaves once the holdings are taken
	// out, by more than a cent, is not all of the account's cash, or not
	// cash alone.
	b.cash, b.noCash = cash(ps, cur), unsent(ps, currentAt+"cashBalance", cur.CashBalance)
	if left := leftOver(ps, cur); b.cash.ok && left.ok {
		if d := b.cash.cents - left.cents; d < -1 || d > 1 {
			b.cash, b.cashDiffers = money{}, true
		}
	}

	// The buying power is a margin account's buyingPower, for stock that it
	// can buy on margin. A cash account has none, and what it has to trade
	// with is cashAvailableForTrading. Another type has neither.
	var key string
	var v *float64
	switch a.SecuritiesAccount.Type {
	case "MARGIN":
		key, v = "buyingPower", cur.BuyingPower
	case "CASH":
		key, v = "cashAvailableForTrading", cur.CashAvailableForTrading
	}
	if key != "" {
		b.buyingPower, b.noBuyingPower = figure(ps, currentAt+key, v), unsent(ps, currentAt+key, v)
	}
	return b
}

// cash returns the cash in cur, the current balances of an account with
// the problems ps: cashBalance or, when that is 0 and marginBalance is
// sent, marginBalance plus shortBalance, where a margin account with short
// positions or a debit can keep its cash (jkoelker/schwab-mcp#222). An
// absent shortBalance counts as 0, but an absent cashBalance is none, not
// 0: a margin account's answer of 2024 sent none, with marginBalance and
// shortBalance both 0, though the account held cash. Money market funds
// are positions, not cash.
func cash(ps []schwab.Problem, cur schwab.Balances) money {
	c := figure(ps, currentAt+"cashBalance", cur.CashBalance)
	if !c.ok || c.cents != 0 || cur.MarginBalance == nil && !unreadable(ps, currentAt+"marginBalance") {
		return c
	}
	short := money{ok: true} // an absent shortBalance counts as 0
	if cur.ShortBalance != nil || unreadable(ps, currentAt+"shortBalance") {
		short = figure(ps, currentAt+"shortBalance", cur.ShortBalance)
	}
	return figure(ps, currentAt+"marginBalance", cur.MarginBalance).plus(short)
}

// leftOver returns what the value in cur, the current balances of an
// account with the problems ps, leaves once the market values of its
// holdings are taken out, or none unless Schwab sent each of those
// figures. In every answer seen, the value is cashBalance plus those
// market values, moneyMarketFund and savings, the last two always 0, so
// that what is left is the cash. Where it is not, what the cash leaves out
// is not known: savings or moneyMarketFund, whatever they hold, or short
// proceeds in shortBalance beside a cashBalance that is not 0, which #222
// does not cover.
func leftOver(ps []schwab.Problem, cur schwab.Balances) money {
	left := figure(ps, currentAt+"liquidationValue", cur.LiquidationValue)
	for _, held := range []struct {
		key string
		v   *float64
	}{
		{"longMarketValue", cur.LongMarketValue},
		{"shortMarketValue", cur.ShortMarketValue},
		{"longOptionMarketValue", cur.LongOptionMarketValue},
		{"shortOptionMarketValue", cur.ShortOptionMarketValue},
		{"mutualFundValue", cur.MutualFundValue},
		{"bondValue", cur.BondValue},
	} {
		left = left.minus(figure(ps, currentAt+held.key, held.v))
	}
	return left
}

// accountsTable returns the table of accounts: each account's number,
// nickname, type and value, and their total.
func accountsTable(as []namedAccount, show bool) ([]column, [][]string, []string) {
	cols := []column{{head: "ACCOUNT"}, {head: "NAME"}, {head: "TYPE"}, {head: "VALUE", right: true}}
	var rows [][]string
	var warnings []string
	total := money{ok: true}
	for _, a := range as {
		b := balanceOf(a.account)
		rows = append(rows, []string{
			accountCell(a.account.SecuritiesAccount.AccountNumber, show), textCell(a.nickname, 30),
			plainCell(a.account.SecuritiesAccount.Type), b.value.String(),
		})
		total = total.plus(b.value)
		if b.noValue {
			warnings = append(warnings, "Schwab sent no current value for "+a.label())
		}
	}
	if len(rows) > 1 {
		rows = append(rows, []string{"TOTAL", "", "", total.String()})
	}
	return cols, rows, warnings
}

// balancesTable returns the table of balances: each account's number,
// nickname, type, value, cash and buying power, and the totals of the
// values and the cash. Buying power does not pass between accounts, so it
// has none.
func balancesTable(as []namedAccount, show bool) ([]column, [][]string, []string) {
	cols := []column{
		{head: "ACCOUNT"},
		{head: "NAME"},
		{head: "TYPE"},
		{head: "VALUE", right: true},
		{head: "CASH", right: true},
		{head: "BUYING POWER", right: true},
	}
	var rows [][]string
	var warnings []string
	totalValue, totalCash := money{ok: true}, money{ok: true}
	for _, a := range as {
		b := balanceOf(a.account)
		rows = append(rows, []string{
			accountCell(a.account.SecuritiesAccount.AccountNumber, show), textCell(a.nickname, 30),
			plainCell(a.account.SecuritiesAccount.Type), b.value.String(), b.cash.String(), b.buyingPower.String(),
		})
		totalValue, totalCash = totalValue.plus(b.value), totalCash.plus(b.cash)
		var missing []string
		for _, f := range []struct {
			name string
			no   bool
		}{{"value", b.noValue}, {"cash", b.noCash}, {"buying power", b.noBuyingPower}} {
			if f.no {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, "Schwab sent no current "+orList(missing)+" for "+a.label())
		}
		if b.cashDiffers {
			warnings = append(warnings, "Schwab's cash for "+a.label()+" is not its value less its holdings, so CASH shows -")
		}
	}
	if len(rows) > 1 {
		rows = append(rows, []string{"TOTAL", "", "", totalValue.String(), totalCash.String(), ""})
	}
	return cols, rows, warnings
}

// orList joins words as "a", "a or b", or "a, b or c".
func orList(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " or " + words[len(words)-1]
}

// holding is what the positions table shows of a position.
type holding struct {
	symbol, description string
	quantity, price     string // or - for none
	value, cost, gain   money
}

// holdingOf returns what the positions table shows of p: its quantity and
// market value, negative for a short position, the value with an option's
// multiplier in it; its average price, the tax-lot average per share with
// fees and adjustments in it, what a share cost or, short, brought in; its
// open profit or loss, positive for a gain whether long or short; and its
// cost, the value less the open profit or loss, which for a short position
// is its proceeds, negative. Each is none when a figure that it needs
// could not be read, or was not sent. The cost needs no multiplier, which
// positions do not carry, and value - cost = open profit or loss to the
// cent, since the value and the open profit or loss are rounded first.
func holdingOf(p schwab.Position) holding {
	ps := p.Problems
	h := holding{symbol: p.Instrument.Symbol, description: p.Instrument.Description, quantity: "-", price: "-"}
	if !unreadable(ps, "longQuantity") && !unreadable(ps, "shortQuantity") {
		h.quantity = formatQuantity(p.LongQuantity - p.ShortQuantity)
	}
	if !unreadable(ps, "averagePrice") {
		h.price = formatPrice(p.AveragePrice)
	}
	if !unreadable(ps, "marketValue") {
		h.value = amount(p.MarketValue)
	}
	h.gain, _ = openPL(p)
	h.cost = h.value.minus(h.gain)
	return h
}

// openPL returns p's open profit or loss: longOpenProfitLoss plus
// shortOpenProfitLoss, since a position carries only its own side's, and
// the other's is absent, which reads as 0. It is none when either could
// not be read, or a quantity, which says which sides p holds; or when p
// lacks the figure of a side that it holds, which Schwab sends on every
// position of that side. missing has a problem for each such figure, since
// no problem of p's names it, with its path in p.
func openPL(p schwab.Position) (gain money, missing []schwab.Problem) {
	ps := p.Problems
	for _, key := range []string{"longQuantity", "shortQuantity", "longOpenProfitLoss", "shortOpenProfitLoss"} {
		if unreadable(ps, key) {
			return money{}, nil
		}
	}
	// Whether each figure was sent, which the record's 0 does not say;
	// what could not be read of them is a problem of p's, above.
	var sent struct {
		Long  *float64 `json:"longOpenProfitLoss"`
		Short *float64 `json:"shortOpenProfitLoss"`
	}
	_ = json.Unmarshal(p.Raw, &sent)
	if p.LongQuantity != 0 && sent.Long == nil {
		missing = append(missing, schwab.Problem{Path: "longOpenProfitLoss", What: "missing, though Schwab sends it on every long position"})
	}
	if p.ShortQuantity != 0 && sent.Short == nil {
		missing = append(missing, schwab.Problem{Path: "shortOpenProfitLoss", What: "missing, though Schwab sends it on every short position"})
	}
	if missing != nil {
		return money{}, missing
	}
	return amount(p.LongOpenProfitLoss).plus(amount(p.ShortOpenProfitLoss)), nil
}

// positionsTable returns the table of positions: each position's account,
// symbol, quantity, average cost, value, cost, open profit or loss and
// description, account by account in the order of Schwab's answer and by
// symbol within an account, so that an option usually comes straight
// after its stock; and the totals of the values, costs and open profit or
// loss. An account whose positions could not be read has one row, with
// the symbol ? and no figures, and so the totals have none.
func positionsTable(as []namedAccount, show bool) ([]column, [][]string, []string) {
	cols := []column{
		{head: "ACCOUNT"},
		{head: "SYMBOL"},
		{head: "QTY", right: true},
		{head: "AVG COST", right: true},
		{head: "VALUE", right: true},
		{head: "COST", right: true},
		{head: "OPEN P/L", right: true},
		{head: "DESCRIPTION"},
	}
	var rows [][]string
	value, cost, gain := money{ok: true}, money{ok: true}, money{ok: true}
	for _, a := range as {
		number := accountCell(a.account.SecuritiesAccount.AccountNumber, show)
		if unreadable(a.account.Problems, "securitiesAccount.positions") {
			rows = append(rows, []string{number, "?", "-", "-", "-", "-", "-", ""})
			value, cost, gain = money{}, money{}, money{}
			continue
		}
		// Byte order, which puts an OCC symbol, its root padded to six
		// characters, after its root's own; equal symbols keep their order.
		ps := slices.Clone(a.account.SecuritiesAccount.Positions)
		slices.SortStableFunc(ps, func(x, y schwab.Position) int { return strings.Compare(x.Instrument.Symbol, y.Instrument.Symbol) })
		for _, p := range ps {
			h := holdingOf(p)
			symbol := plainCell(h.symbol)
			if symbol == "" {
				symbol = "?"
			}
			rows = append(rows, []string{
				number, symbol, h.quantity, h.price, h.value.String(), h.cost.String(), h.gain.String(), textCell(h.description, 40),
			})
			value, cost, gain = value.plus(h.value), cost.plus(h.cost), gain.plus(h.gain)
		}
	}
	if len(rows) > 1 {
		rows = append(rows, []string{"TOTAL", "", "", "", value.String(), cost.String(), gain.String(), ""})
	}
	return cols, rows, nil
}

// positionsSummary returns the line before the positions table: how many
// positions in how many accounts, and that cash, which is not a position,
// is left out.
func positionsSummary(as []namedAccount) string {
	n := 0
	for _, a := range as {
		n += len(a.account.SecuritiesAccount.Positions)
	}
	return plural(n, "position") + " in " + plural(len(as), "account") + ", not counting cash (see schwabtx balances)"
}
