package mcpserver

import (
	"context"
	"errors"
	"time"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// The login's checks before any request to Schwab (design 6.2), and the
// requests themselves, whose failures toolError tells from the server's
// own.

// The problems that cmd/schwab-mcp adds to Options.Config.Problems for its
// own arguments, beside internal/config's (design 5.1).
const (
	// ProblemStrayArgument is an argument that is no flag, such as the part
	// after a space of a path that was not quoted.
	ProblemStrayArgument = "stray_argument"
	// ProblemBadFlag is a flag that the server does not know, or a value it
	// does not take.
	ProblemBadFlag = "bad_flag"
)

// client returns the client for Schwab, after the checks that cost Schwab
// nothing: the config could be read, the app key and secret are set, a
// login is saved and has not lapsed nor been refused. A login that lapses
// within 48 hours adds the warning login_expires_soon. It reads the token
// file once per call.
func (c *call) client(ctx context.Context) (*schwab.Client, error) {
	if c.cl != nil {
		return c.cl, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := c.s
	if err := s.configError(); err != nil {
		return nil, err
	}
	if s.auth == nil {
		return nil, s.notConfigured()
	}
	st, err := s.auth.Status()
	if err != nil {
		s.log.Warn("the token file could not be read", "error", err)
		return nil, c.failf(codeNotLoggedIn, "The Schwab login at %s could not be read. "+loginAdvice, s.mask.path(s.auth.TokenFile()))
	}
	switch {
	case !st.LoggedIn:
		return nil, s.notLoggedIn()
	case st.Rejected != nil || !st.ExpiresAt.IsZero() && !c.now.Before(st.ExpiresAt):
		return nil, s.errorFor(codeLoginExpired)
	}
	if left, soon := lookup.ExpiresIn(st.ExpiresAt, c.now); soon {
		c.warn("login_expires_soon", "The Schwab login lapses in about %d hours (%s ET). "+
			"Ask the user to run `schwabtx login` in their own terminal before then.",
			int(left/time.Hour), Ident(st.ExpiresAt.In(nytime.NewYork()).Format("2006-01-02 15:04")))
	}
	c.cl = s.client
	return c.cl, nil
}

// loginAdvice is what every error about the login asks: the user's own
// terminal, since a command-line agent could run the command itself, and
// it needs the user's browser.
const loginAdvice = "Ask the user to run `schwabtx login` in their own terminal, then call again; " +
	"do not run it yourself, it needs their browser."

// notLoggedIn is the error for a token file that holds no login, with the
// hint for a CLI that keeps its login elsewhere.
func (s *Server) notLoggedIn() *toolErr {
	file := s.o.Config.TokenFile
	return &toolErr{code: codeNotLoggedIn, msg: s.mask.compose("There is no Schwab login at %s (%s). "+loginAdvice+
		" If schwabtx already works in their terminal, it keeps its login elsewhere: ask them to run `schwabtx setup` there, "+
		"which saves the paths it uses, and to restart this server.", s.mask.path(file.Value), Ident(fromWords(file.From, "SCHWAB_TOKEN_FILE")))}
}

// notConfigured is the error for a server without the app key and secret.
func (s *Server) notConfigured() *toolErr {
	return &toolErr{code: codeNotConfigured, msg: s.mask.words("The server has no Schwab app key and secret. Ask the user to run " +
		"`schwabtx setup` in their own terminal and to start this server with `--config` and the path it prints (docs/mcp.md).")}
}

// fromWords says where a setting came from, for an error: variable names
// the environment's variable.
func fromWords(from config.Source, variable string) string {
	switch from {
	case config.FromEnv:
		return "from " + variable
	case config.FromFile:
		return "from config.json"
	}
	return "the default"
}

// configError returns the error that every tool that reads Schwab answers
// while the config cannot be used, or nil: a config file that could not be
// read, an argument or flag that the server does not take, a token file
// that may not be used, or a base URL that schwab.NewAuth refused.
func (s *Server) configError() error {
	for _, p := range s.o.Config.Problems {
		switch {
		case p.Code == ProblemStrayArgument || p.Code == ProblemBadFlag:
			return &toolErr{code: codeConfigUnreadable, msg: s.mask.words("The server was started with an argument it does not take; " +
				"a path that holds a space must be quoted in a shell line, or be one element of a JSON `args` list, " +
				"as `schwabtx setup` prints it.")}
		case p.Setting == "config_file" && p.Code != config.CodeMadePrivate && p.Code != config.CodeNotPrivate:
			return &toolErr{code: codeConfigUnreadable, msg: s.mask.compose("The server's config file could not be read, at %s (%s). "+
				"Ask the user to run `schwabtx setup` again in their own terminal.", s.mask.path(p.Path), Ident(problemWords[p.Code]))}
		}
	}
	if s.o.Config.TokenFile.Value == "" && s.authErr == nil && s.o.Config.AppKey.Value != "" && s.o.Config.AppSecret.Value != "" {
		return &toolErr{code: codeConfigUnreadable, msg: s.mask.words("The server's config names no token file it may use. " +
			"Ask the user to run `schwabtx setup` again in their own terminal.")}
	}
	if s.authErr != nil {
		return &toolErr{code: codeConfigUnreadable, msg: s.mask.words("The API's address in the server's config file is not one it may send the login to: " +
			"https, or http to this computer only. Ask the user to run `schwabtx setup` again in their own terminal.")}
	}
	return nil
}

// problemWords say what each config.Problem's code means.
var problemWords = map[string]string{
	config.CodeMissing:      "it does not exist",
	config.CodeUnreadable:   "it could not be read, or is not a config file",
	config.CodeNewerVersion: "a newer schwabtx wrote it",
	config.CodeRelativePath: "its path is not absolute",
	config.CodeMadePrivate:  "others could read it; it is now readable only by you",
	config.CodeNotPrivate:   "others can read it, and it could not be made private",
	config.CodeCompanion:    "it is one of the store's files, which only SQLite may open",
	ProblemStrayArgument:    "the server was started with an argument it does not take; quote a path that holds a space",
	ProblemBadFlag:          "the server was started with a flag or a value it does not take",
}

// ask runs f, one or more requests to Schwab with the call's client, after
// the login's checks, and marks its failure as a request's, so that
// toolError tells Schwab's failures and the network's from the server's
// own.
func ask[T any](ctx context.Context, c *call, f func(*schwab.Client) (T, error)) (T, error) {
	var zero T
	cl, err := c.client(ctx)
	if err != nil {
		return zero, err
	}
	v, err := f(cl)
	if err != nil {
		return zero, &requestError{err: err}
	}
	return v, nil
}

// askAccount runs f, requests about the account a, with its hash. When
// Schwab refuses the hash as an invalid account, the directory reads the
// account numbers again, and f runs once more with the account's new hash;
// an account that Schwab no longer lists is account_not_shared.
func askAccount[T any](ctx context.Context, c *call, a dirAccount, f func(*schwab.Client, schwab.AccountHash) (T, error)) (T, error) {
	var zero T
	if a.hash == "" {
		return zero, c.failf(codeAccountNotShared, "Schwab no longer shares %s; ask the user to run schwabtx login and tick it.", c.mask.masked(a.number))
	}
	v, err := ask(ctx, c, func(cl *schwab.Client) (T, error) { return f(cl, a.hash) })
	if err == nil || !errors.Is(err, schwab.ErrInvalidAccount) {
		return v, err
	}
	again, ok, rerr := c.accounts.refreshed(ctx, c, a.number)
	if rerr != nil {
		return zero, rerr
	}
	if !ok {
		return zero, c.failf(codeAccountNotShared, "Schwab no longer shares that account; ask the user to run schwabtx login and tick it.")
	}
	return ask(ctx, c, func(cl *schwab.Client) (T, error) { return f(cl, again.hash) })
}
