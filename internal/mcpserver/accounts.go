package mcpserver

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The account directory (design 6.4) joins, inside the process only,
// Schwab's account numbers and hashes, the user's preferences (nicknames
// and the primary account) and the store's accounts, matched by full
// number. Every tool that reads accounts loads it first, which also fills
// the masker's known numbers before any output is built. An account
// argument picks exactly one account, or none with an error that names the
// accounts it could have meant: an account is never guessed.

// dirAccount is one account that the directory knows: Schwab's, the
// store's, or both.
type dirAccount struct {
	// number is the full number: never shown, nor put in a cursor or a
	// digest of one.
	number string
	// hash is Schwab's hash of it, "" for an account only the store knows.
	hash schwab.AccountHash
	// nickname is the nickname the preferences give it, or the store's for
	// an account only the store knows, which the store masked when it
	// stored it (storeNickname); "" for none.
	nickname      string
	storeNickname bool
	// primary is whether the preferences call it the primary account; nil
	// when they were not read.
	primary *bool
	// storeID, storeRef, kind and storeType are the store's: its id, its
	// ref (A and the id), the kind your account decision gives it, and its
	// type as its latest snapshot gives it; zero when the store does not
	// know it.
	storeID                   int64
	storeRef, kind, storeType string
	// shared says that Schwab's account numbers list it; notSharedSince is
	// when the store saw the login stop sharing it, or zero.
	shared         bool
	notSharedSince time.Time
}

// key returns the account's key in cursors and in the ledger's order
// (history.LedgerAccount): its hash, or its store ref for an account only
// the store knows. It never holds the account's number.
func (a dirAccount) key() string {
	if a.hash != "" {
		return string(a.hash)
	}
	return a.storeRef
}

// ref returns how an answer names a: its number masked, its hash, and its
// nickname, masked as the text of whoever wrote it.
func (c *call) ref(a dirAccount) accountRef {
	r := accountRef{Account: c.mask.masked(a.number)}
	if a.hash != "" {
		r.AccountHash = new(Ident(a.hash))
	}
	if a.nickname != "" {
		nickname := c.mask.free(a.nickname)
		if a.storeNickname {
			nickname = c.mask.stored(a.nickname)
		}
		r.Nickname = &nickname
	}
	return r
}

// dirView is the directory as one load read it.
type dirView struct {
	// accounts are Schwab's, in the order its account numbers list them,
	// then those only the store knows, by store id.
	accounts []dirAccount
	// schwabErr is why Schwab's account numbers could not be read, or nil;
	// prefsErr why the preferences could not be read, or nil, when the
	// nicknames are none; prefsProblems are the preferences' problems that
	// can cost an account its nickname.
	schwabErr, prefsErr error
	prefsProblems       []schwab.Problem
	// storeErr is why the store's accounts could not be read, or nil: nil
	// too when there is no store, which is no failure here.
	storeErr error
}

// shared returns the accounts that Schwab lists.
func (v *dirView) shared() []dirAccount {
	var out []dirAccount
	for _, a := range v.accounts {
		if a.shared {
			out = append(out, a)
		}
	}
	return out
}

// dirTTL is how long a load of the directory serves.
const dirTTL = 5 * time.Minute

// directory is the server's account directory, read again every 5 minutes,
// and at once when Schwab refuses a hash. It is safe for concurrent use.
type directory struct {
	// lock is held, one load at a time, by whoever loads the directory; a
	// call that waits for it gives up when its context ends.
	lock chan struct{}
	// view is the last whole load, and readAt when it was read, by the
	// system clock; nil when there is none to serve.
	view   *dirView
	readAt time.Time
}

func newDirectory() *directory { return &directory{lock: make(chan struct{}, 1)} }

// load returns the directory: a cached load younger than 5 minutes, or a
// new one. A load is cached only when each of its reads worked. It fails
// only when ctx ends; what could not be read is in the view.
func (d *directory) load(ctx context.Context, c *call) (*dirView, error) {
	select {
	case d.lock <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-d.lock }()
	if d.view != nil && time.Since(d.readAt) < dirTTL {
		return d.view, nil
	}
	v := d.read(ctx, c)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.view = nil
	if v.schwabErr == nil && v.prefsErr == nil && v.storeErr == nil {
		d.view, d.readAt = v, time.Now()
	}
	return v, nil
}

// read reads the directory: Schwab's account numbers, then its
// preferences, then the store's accounts, which it joins by number.
func (d *directory) read(ctx context.Context, c *call) *dirView {
	v := &dirView{}
	byNumber := map[string]int{}
	numbers, err := ask(ctx, c, func(cl *schwab.Client) ([]schwab.AccountNumber, error) { return cl.AccountNumbers(ctx) })
	if err != nil {
		v.schwabErr = err
	}
	for _, n := range numbers {
		if n.AccountNumber == "" || n.HashValue == "" {
			continue
		}
		c.mask.known.Add(n.AccountNumber)
		if _, seen := byNumber[n.AccountNumber]; seen {
			continue
		}
		byNumber[n.AccountNumber] = len(v.accounts)
		v.accounts = append(v.accounts, dirAccount{number: n.AccountNumber, hash: n.HashValue, shared: true})
	}
	if v.schwabErr == nil {
		prefs, err := ask(ctx, c, func(cl *schwab.Client) (schwab.UserPreference, error) { return cl.UserPreference(ctx) })
		if err != nil {
			v.prefsErr = err
		} else {
			v.prefsProblems = lookup.NicknameProblems(prefs)
			seen := map[string]bool{}
			for _, p := range prefs.Accounts {
				i, ok := byNumber[p.AccountNumber]
				if !ok || seen[p.AccountNumber] {
					continue
				}
				seen[p.AccountNumber] = true
				v.accounts[i].nickname = p.NickName
				v.accounts[i].primary = new(p.PrimaryAccount)
			}
			for i := range v.accounts {
				if v.accounts[i].primary == nil {
					v.accounts[i].primary = new(false)
				}
			}
		}
	}
	numbered, views, err := c.store.accounts(ctx)
	if err != nil && !errors.Is(err, history.ErrNoStore) && !errors.Is(err, errNoStorePath) {
		v.storeErr = err
		return v
	}
	viewOf := map[int64]history.ReaderAccount{}
	for _, a := range views {
		viewOf[a.ID] = a
	}
	for _, n := range numbered {
		c.mask.known.Add(n.Number)
		sv := viewOf[n.ID]
		i, ok := byNumber[n.Number]
		if !ok {
			i = len(v.accounts)
			byNumber[n.Number] = i
			v.accounts = append(v.accounts, dirAccount{number: n.Number, nickname: sv.Nickname, storeNickname: true})
		}
		a := &v.accounts[i]
		a.storeID, a.storeRef, a.kind, a.storeType, a.notSharedSince = n.ID, sv.Ref, sv.Kind, sv.Type, sv.NotSharedSince
	}
	return v
}

// refreshed reads the directory again, as after Schwab refused a hash it
// gave before, and returns the account numbered number, and whether Schwab
// still lists it.
func (d *directory) refreshed(ctx context.Context, c *call, number string) (dirAccount, bool, error) {
	select {
	case d.lock <- struct{}{}:
	case <-ctx.Done():
		return dirAccount{}, false, ctx.Err()
	}
	d.view = nil
	<-d.lock
	v, err := d.load(ctx, c)
	if err != nil {
		return dirAccount{}, false, err
	}
	if v.schwabErr != nil {
		return dirAccount{}, false, v.schwabErr
	}
	for _, a := range v.accounts {
		if a.number == number && a.shared {
			return a, true, nil
		}
	}
	return dirAccount{}, false, nil
}

// directory loads the account directory for the call, and adds the
// warnings of what it could not read of the nicknames.
func (c *call) directory(ctx context.Context) (*dirView, error) {
	v, err := c.accounts.load(ctx, c)
	if err != nil {
		return nil, err
	}
	if v.schwabErr == nil && v.prefsErr != nil {
		c.s.log.Warn("the preferences could not be read", "tool", c.tool, "error", v.prefsErr)
		c.warn("nicknames_missing", "Schwab's preferences, which hold the nicknames, could not be read, so every nickname is null.")
	}
	if len(v.prefsProblems) > 0 {
		c.warnProblems("the preferences", [][]schwab.Problem{v.prefsProblems}, "a nickname may be missing")
	}
	return v, nil
}

// warnProblems adds the warning problems for records that differed from
// what Schwab has sent before: what names them, and after, what it costs.
// Each place is an identifier and what Schwab sent there is masked as free
// text; the warning is never the CLI's sentence, which quotes Schwab's
// values unmasked.
func (c *call) warnProblems(what string, problems [][]schwab.Problem, after string) {
	records, first, more := lookup.ProblemPlaces(problems)
	if records == 0 {
		return
	}
	var places []FreeText
	for _, p := range first {
		places = append(places, c.mask.compose("%s: %s", Ident(p.Path), c.mask.free(p.What)))
	}
	if more > 0 {
		places = append(places, c.mask.compose("and %d more places", more))
	}
	c.warn("problems", "%s differed from what Schwab has sent before (%s); %s.",
		Ident(what), c.mask.join(places, "; "), Ident(after))
}

// pickAccounts returns the accounts that arg, an account argument, names,
// of those that Schwab shares: all of them when arg is "". It fails when
// Schwab's accounts could not be read.
func (c *call) pickAccounts(ctx context.Context, arg string) ([]dirAccount, error) {
	v, err := c.directory(ctx)
	if err != nil {
		return nil, err
	}
	if v.schwabErr != nil {
		return nil, v.schwabErr
	}
	return v.pick(c, arg, false)
}

// pick returns the accounts that arg names, as design 6.4 says: all of
// them when arg is "", else exactly one, or an error. withStoreOnly lets
// it name an account that only the store knows.
func (v *dirView) pick(c *call, arg string, withStoreOnly bool) ([]dirAccount, error) {
	from := v.accounts
	if !withStoreOnly {
		from = v.shared()
	}
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return from, nil
	}
	var got []dirAccount
	switch {
	case hashLike(arg):
		got = byHashPrefix(from, arg)
	case storeRef.MatchString(arg):
		for _, a := range from {
			if strings.EqualFold(a.storeRef, arg) {
				got = append(got, a)
			}
		}
	default:
		named := make([]lookup.Named, len(from))
		for i, a := range from {
			named[i].Account.SecuritiesAccount.AccountNumber = a.number
			named[i].Nickname = a.nickname
		}
		picked, perr := lookup.ParseSelector(arg).Pick(named)
		if perr == nil {
			for _, p := range picked {
				for _, a := range from {
					if a.number == p.Account.SecuritiesAccount.AccountNumber {
						got = append(got, a)
					}
				}
			}
			break
		}
		for _, m := range perr.Matches {
			for _, a := range from {
				if a.number == m.Account.SecuritiesAccount.AccountNumber {
					got = append(got, a)
				}
			}
		}
		if len(got) == 0 && allDigits.MatchString(arg) && len(arg) >= 12 {
			got = byHashPrefix(from, arg) // a hash's prefix that happens to be digits alone
		}
		if len(got) > 1 && perr.Kind != lookup.AmbiguousNickname {
			return nil, c.ambiguous(arg, got, "pass more of the last digits, or the account_hash, instead")
		}
	}
	switch len(got) {
	case 1:
		return got, nil
	case 0:
		return nil, c.notFound(arg, v)
	}
	return nil, c.ambiguous(arg, got, "pass the account_hash or the last digits instead")
}

var (
	// hexRun is 12 to 64 hex digits, the shape of a hash or of a prefix of
	// one that a selector may give.
	hexRun = regexp.MustCompile(`^[0-9A-Fa-f]{12,64}$`)
	// storeRef is a store's ref of an account: A and its id.
	storeRef = regexp.MustCompile(`^[Aa][0-9]{1,18}$`)
	// allDigits is digits alone.
	allDigits = regexp.MustCompile(`^[0-9]+$`)
)

// hashLike reports whether arg is a hash, or a prefix of one: 12 to 64 hex
// digits, a letter among them, which no account number has.
func hashLike(arg string) bool {
	return hexRun.MatchString(arg) && !allDigits.MatchString(arg)
}

// byHashPrefix returns the accounts whose hash starts with prefix, in any
// case.
func byHashPrefix(as []dirAccount, prefix string) []dirAccount {
	var got []dirAccount
	for _, a := range as {
		if len(a.hash) >= len(prefix) && strings.EqualFold(string(a.hash[:len(prefix)]), prefix) {
			got = append(got, a)
		}
	}
	return got
}

// shown returns arg, an account argument, as an error may echo it: a hash
// or a prefix of one whole, as an identifier, since it cannot be an account
// number and a model may pass it back; anything else as
// lookup.Selector.Shown shows it, every run of more than 3 digits as ...
// and its last 3.
func (c *call) shown(arg string) FreeText {
	arg = strings.TrimSpace(arg)
	if hashLike(arg) {
		return c.mask.compose("%s", Ident(arg))
	}
	return c.mask.free(lookup.Selector{Given: arg}.Shown())
}

// notFound is the error for an account argument that no account matches,
// which lists the accounts the login shares, each with its hash whole, so
// that the model can pass one back.
func (c *call) notFound(arg string, v *dirView) error {
	var listed []FreeText
	for _, a := range v.accounts {
		r := c.ref(a)
		name := c.mask.compose("")
		if r.Nickname != nil {
			name = c.mask.compose(` "%s"`, *r.Nickname)
		}
		switch {
		case a.hash != "":
			listed = append(listed, c.mask.compose("%s%s (account_hash %s)", r.Account, name, Ident(a.hash)))
		case a.storeRef != "":
			listed = append(listed, c.mask.compose("%s%s (store_ref %s, only in the local history)", r.Account, name, Ident(a.storeRef)))
		}
	}
	if len(listed) == 0 {
		return c.failf(codeAccountNotFound, `No account matches "%s", and the login shares none.`, c.shown(arg))
	}
	return c.failf(codeAccountNotFound, `No account matches "%s". The login shares %s. Pass one of those.`,
		c.shown(arg), c.mask.join(listed, ", "))
}

// ambiguous is the error for an account argument that several accounts
// match, naming them by their masked numbers.
func (c *call) ambiguous(arg string, got []dirAccount, what string) error {
	names := make([]FreeText, len(got))
	for i, a := range got {
		names[i] = c.mask.compose("%s", c.mask.masked(a.number))
	}
	list := c.mask.compose("%s and %s", c.mask.join(names[:len(names)-1], ", "), names[len(names)-1])
	return c.failf(codeAccountAmbiguous, `"%s" matches %d accounts, %s; %s.`, c.shown(arg), len(got), list, Ident(what))
}
