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

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A lookup is one of the commands that print what Schwab reports of the
// accounts as of the request.
type lookup struct {
	names bool // its table shows the accounts' nicknames
	// table returns the table of the accounts as, with full account
	// numbers if show, and a warning for each - in it that no problem of an
	// account's explains.
	table func(as []namedAccount, show bool) (cols []column, rows [][]string, warnings []string)
}

var lookups = map[string]lookup{
	"accounts": {names: true, table: accountsTable},
	"balances": {names: true, table: balancesTable},
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
	if n := nameCell(a.nickname); n != "" {
		l += " (" + n + ")"
	}
	return l
}

// runLookup runs the lookup of the given name: it reads the accounts, and
// the user's preferences when the table shows the accounts' nicknames or
// -account gives one, and prints the accounts that -account picks, as a
// table or as Schwab's own JSON.
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

	accounts, err := c.accounts(ctx)
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
		problems[i] = accountProblems(a)
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
			return fmt.Errorf("fetching the user preferences, to find the account named %q: %w", sel.nickname, err)
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
	return writeTable(stdout, cols, rows)
}

// selector is the account that -account names: by the digits that its
// number ends in, or by its nickname. All three are empty when it names
// none.
type selector struct{ given, digits, nickname string }

// parseSelector reads -account's value. Digits, with or without a leading
// "...", as the tables and Schwab mask numbers, and with dashes or spaces
// among them, as schwab.com writes a number, name the account whose number
// ends in them; a whole number names its account too. Anything else names
// the account with that nickname, ignoring case and the spaces around it,
// so a nickname of digits alone cannot be given, though its account's
// number can.
func parseSelector(v string) selector {
	v = strings.TrimSpace(v)
	s := selector{given: v}
	digits := strings.NewReplacer("-", "", " ", "").Replace(strings.TrimPrefix(v, "..."))
	if digits != "" && strings.Trim(digits, "0123456789") == "" {
		s.digits = digits
	} else {
		s.nickname = v
	}
	return s
}

// shown returns -account's value as an error shows it: as given, but for
// more than 4 digits, which come close to a whole account number, and are
// masked as the tables mask one.
func (s selector) shown() string {
	if len(s.digits) > 4 {
		return "..." + s.digits[len(s.digits)-4:]
	}
	return s.given
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
			accountCell(a.account.SecuritiesAccount.AccountNumber, show), nameCell(a.nickname),
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
			accountCell(a.account.SecuritiesAccount.AccountNumber, show), nameCell(a.nickname),
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
