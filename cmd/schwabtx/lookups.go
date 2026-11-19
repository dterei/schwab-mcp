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

	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/mask"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// A lookupCommand is one of the commands that print what Schwab reports of
// the accounts as of the request.
type lookupCommand struct {
	positions bool // it asks for the accounts' positions
	names     bool // its table shows the accounts' nicknames
	// table returns the table of the accounts as, with full account
	// numbers if show, and a warning for each - in it that no problem of an
	// account's explains.
	table func(as []namedAccount, show bool) (cols []column, rows [][]string, warnings []string)
	// summary returns a line for stderr before the table, or is nil.
	summary func(as []namedAccount) string
}

var lookups = map[string]lookupCommand{
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
// -show-accounts says, and its nickname when it is known, masked.
func (a namedAccount) label() string {
	l := accountCell(a.account.SecuritiesAccount.AccountNumber, false)
	if n := a.name(false); n != "" {
		l += " (" + n + ")"
	}
	return l
}

// name returns a's nickname for a table: free text, in which a run of 8
// digits, which may be an account number, is masked as the store masks
// it (mask.Text), unless show (-show-accounts).
func (a namedAccount) name(show bool) string { return textCell(freeText(a.nickname, show), 30) }

// freeText returns text that Schwab or you wrote, masked (mask.Text)
// unless show.
func freeText(s string, show bool) string {
	if show {
		return s
	}
	return mask.Text(s)
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
	showAccounts := fs.Bool("show-accounts", false, "print full account numbers in the table instead of the last 3 digits, and nicknames and descriptions as Schwab sent them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	sel := lookup.ParseSelector(*account)

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
		problems[i] = lookup.Problems(a)
	}
	if w := problemsWarning("account", problems); w != "" {
		warn(w + "; the table may not show those fields as Schwab sent them, and -json prints Schwab's own JSON")
	}

	// The nicknames are in the user's preferences, a request of their own,
	// which the table's names need, and so does a nickname given to
	// -account.
	asked := sel.Nickname != "" || l.names && !*asJSON
	if asked {
		prefs, err := c.preferences(ctx)
		switch {
		case err == nil:
			nameAccounts(as, prefs)
			if n, places := problemPlaces([][]schwab.Problem{lookup.NicknameProblems(prefs)}); n > 0 {
				warn("the user preferences differed from what Schwab has sent before (" + places + "); a nickname may be missing")
			}
		case ctx.Err() != nil:
			return fmt.Errorf("fetching the user preferences: %w", err)
		case sel.Nickname != "":
			return fmt.Errorf("fetching the user preferences, to find the account named %q: %w", sel.Shown(), err)
		default:
			warn("the table leaves out the accounts' nicknames: fetching the user preferences: " + err.Error())
		}
	}
	picked, err := pick(sel, as)
	if err != nil && !asked {
		// The error lists the accounts, by their nicknames too when they
		// can be read: they tell apart accounts whose masked numbers do
		// not, and -account takes them.
		if prefs, perr := c.preferences(ctx); perr == nil {
			nameAccounts(as, prefs)
			_, err = pick(sel, as)
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

// pick returns the accounts of as that s names, as lookup.Selector.Pick
// picks them. It never guesses: no account, or several, is an error that
// lists the accounts.
func pick(s lookup.Selector, as []namedAccount) ([]namedAccount, error) {
	got, perr := s.Pick(named(as))
	if perr == nil {
		return unnamed(got), nil
	}
	matches := unnamed(perr.Matches)
	switch perr.Kind {
	case lookup.NoMatch:
		return nil, fmt.Errorf("no account matches -account %q: the login shares %s; to share another, run `schwabtx login` and tick it",
			s.Shown(), labels(as))
	case lookup.AmbiguousNickname:
		return nil, fmt.Errorf("-account %q matches %d accounts, %s; give the last digits of its number instead", s.Shown(), len(matches), labels(matches))
	case lookup.AmbiguousNicknamedDigits:
		return nil, fmt.Errorf("-account %q matches %d accounts, %s; give its nickname instead", s.Shown(), len(matches), labels(matches))
	}
	return nil, fmt.Errorf("-account %q matches %d accounts, %s; give more digits of its number, which `schwabtx accounts -show-accounts` prints",
		s.Shown(), len(matches), labels(matches))
}

// labels lists as by their labels.
func labels(as []namedAccount) string {
	ls := make([]string, len(as))
	for i, a := range as {
		ls[i] = a.label()
	}
	return strings.Join(ls, ", ")
}

// named returns as as internal/lookup takes them, in the same order.
func named(as []namedAccount) []lookup.Named {
	ns := make([]lookup.Named, len(as))
	for i, a := range as {
		ns[i] = lookup.Named{Account: a.account, Nickname: a.nickname}
	}
	return ns
}

// unnamed returns ns as the tables take them, in the same order.
func unnamed(ns []lookup.Named) []namedAccount {
	as := make([]namedAccount, len(ns))
	for i, n := range ns {
		as[i] = namedAccount{account: n.Account, nickname: n.Nickname}
	}
	return as
}

// nameAccounts gives each of as the nickname that prefs gives its
// account, as lookup.NameAccounts does.
func nameAccounts(as []namedAccount, prefs schwab.UserPreference) {
	ns := named(as)
	lookup.NameAccounts(ns, prefs)
	for i := range as {
		as[i].nickname = ns[i].Nickname
	}
}

// accountsTable returns the table of accounts: each account's number,
// nickname, type and value, and their total.
func accountsTable(as []namedAccount, show bool) ([]column, [][]string, []string) {
	cols := []column{{head: "ACCOUNT"}, {head: "NAME"}, {head: "TYPE"}, {head: "VALUE", right: true}}
	var rows [][]string
	var warnings []string
	total := lookup.FromCents(0)
	for _, a := range as {
		b := lookup.BalanceOf(a.account)
		rows = append(rows, []string{
			accountCell(a.account.SecuritiesAccount.AccountNumber, show), a.name(show),
			plainCell(a.account.SecuritiesAccount.Type), b.Value.String(),
		})
		total = total.Plus(b.Value)
		if b.NoValue {
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
	totalValue, totalCash := lookup.FromCents(0), lookup.FromCents(0)
	for _, a := range as {
		b := lookup.BalanceOf(a.account)
		rows = append(rows, []string{
			accountCell(a.account.SecuritiesAccount.AccountNumber, show), a.name(show),
			plainCell(a.account.SecuritiesAccount.Type), b.Value.String(), b.Cash.String(), b.BuyingPower.String(),
		})
		totalValue, totalCash = totalValue.Plus(b.Value), totalCash.Plus(b.Cash)
		var missing []string
		for _, f := range []struct {
			name string
			no   bool
		}{{"value", b.NoValue}, {"cash", b.NoCash}, {"buying power", b.NoBuyingPower}} {
			if f.no {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, "Schwab sent no current "+orList(missing)+" for "+a.label())
		}
		if b.CashDiffers {
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
	value, cost, gain := lookup.FromCents(0), lookup.FromCents(0), lookup.FromCents(0)
	for _, a := range as {
		number := accountCell(a.account.SecuritiesAccount.AccountNumber, show)
		if lookup.Unreadable(a.account.Problems, "securitiesAccount.positions") {
			rows = append(rows, []string{number, "?", "-", "-", "-", "-", "-", ""})
			value, cost, gain = lookup.Money{}, lookup.Money{}, lookup.Money{}
			continue
		}
		// Byte order, which puts an OCC symbol, its root padded to six
		// characters, after its root's own; equal symbols keep their order.
		ps := slices.Clone(a.account.SecuritiesAccount.Positions)
		slices.SortStableFunc(ps, func(x, y schwab.Position) int { return strings.Compare(x.Instrument.Symbol, y.Instrument.Symbol) })
		for _, p := range ps {
			h := lookup.HoldingOf(p)
			symbol := plainCell(h.Symbol)
			if symbol == "" {
				symbol = "?"
			}
			description := textCell(freeText(h.Description, show), 40)
			if p.Instrument.AssetType == schwab.AssetOption {
				// An option's description ends with its expiration, strike
				// and type, which say more than the start of its
				// underlying's name: the option's symbol starts with the
				// underlying's own.
				description = tailCell(freeText(h.Description, show), 40)
			}
			rows = append(rows, []string{
				number, symbol, h.Quantity.String(), h.Price.String(), h.Value.String(), h.Cost.String(), h.Gain.String(), description,
			})
			value, cost, gain = value.Plus(h.Value), cost.Plus(h.Cost), gain.Plus(h.Gain)
		}
	}
	if len(rows) > 1 {
		rows = append(rows, []string{"TOTAL", "", "", "", value.String(), cost.String(), gain.String(), ""})
	}
	return cols, rows, nil
}

// positionsSummary returns the line before the positions table: how many
// positions in how many accounts, and that cash, which is not a position,
// is left out. An account whose positions could not be read is counted
// apart, since how many it holds is not known.
func positionsSummary(as []namedAccount) string {
	n, unread := 0, 0
	for _, a := range as {
		if lookup.Unreadable(a.account.Problems, "securitiesAccount.positions") {
			unread++
			continue
		}
		n += len(a.account.SecuritiesAccount.Positions)
	}
	s := plural(n, "position") + " in " + plural(len(as)-unread, "account") + ", not counting cash (see schwabtx balances)"
	if unread > 0 {
		s += "; the positions of " + plural(unread, "account") + " could not be read"
	}
	return s
}
