package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dterei/schwab-mcp/internal/config"
	"github.com/dterei/schwab-mcp/internal/history"
	"github.com/dterei/schwab-mcp/internal/lookup"
	"github.com/dterei/schwab-mcp/internal/nytime"
	"github.com/dterei/schwab-mcp/internal/schwab"
)

// auth_status (design 10.1) says whether the server can read Schwab, and
// which files it uses, from the token file, the config and the store, and
// sends nothing to Schwab. It takes no slot, and never fails: every
// problem is a field of its answer.

// authStatusOut is auth_status's answer.
type authStatusOut struct {
	envelope
	LoggedIn       bool       `json:"logged_in"`
	LoginAt        *Instant   `json:"login_at"`
	ExpiresAt      *Instant   `json:"expires_at"`
	HoursLeft      *int       `json:"hours_left"`
	ExpiresSoon    bool       `json:"expires_soon"`    // within 48 hours
	Refused        bool       `json:"refused"`         // Schwab refused this login while this server ran
	AppConfigured  bool       `json:"app_configured"`  // the app key and secret are set
	AppFrom        *Ident     `json:"app_from"`        // "environment" or "config file"
	APIHost        Ident      `json:"api_host"`        // "api.schwabapi.com", or the host config.json names
	ConfigFile     *Path      `json:"config_file"`     // the config file read, or null
	ConfigProblems []FreeText `json:"config_problems"` // each problem's setting, code in words and path
	TokenFile      Path       `json:"token_file"`
	TokenFileFrom  Ident      `json:"token_file_from"` // environment, config file, default
	RateFile       *Path      `json:"rate_file"`
	RateFileFrom   *Ident     `json:"rate_file_from"`
	Store          storeState `json:"store"`
	Action         *FreeText  `json:"action"` // what to ask the user to do, or null
}

// storeState is what auth_status says of the store.
type storeState struct {
	File    Path   `json:"file"`
	From    Ident  `json:"from"`
	Exists  bool   `json:"exists"`
	Usable  bool   `json:"usable"`
	Problem *Ident `json:"problem"` // no_store, store_newer, store_needs_update, store_busy, store_unusable
	Waiting *int   `json:"waiting"` // what waits for the user's decision, every account, as schwabtx status counts it
}

// registerAuth registers auth_status.
func (s *Server) registerAuth() {
	addTool(s, toolDef{
		name:  "auth_status",
		title: "Schwab login status",
		description: "Whether this server can read Schwab: if a login is saved, when it was made and when it lapses " +
			"(Schwab ends a login after 7 days), and which config, token, rate-limit and local history files the server uses, " +
			"with how many items in the history wait for the user. Sends nothing to Schwab. If the login is missing or has " +
			"lapsed, ask the user to run `schwabtx login` in their own terminal; this server cannot log in.",
	}, s.authStatus)
}

// authStatus answers auth_status.
func (s *Server) authStatus(ctx context.Context, c *call, _ struct{}) (authStatusOut, error) {
	cfg := s.o.Config
	out := authStatusOut{
		envelope:       envelope{Source: sourceLocal},
		AppConfigured:  cfg.AppKey.Value != "" && cfg.AppSecret.Value != "",
		APIHost:        Ident(s.apiHost()),
		ConfigProblems: []FreeText{},
		TokenFile:      c.mask.path(cfg.TokenFile.Value),
		TokenFileFrom:  Ident(cfg.TokenFile.From),
	}
	if out.AppConfigured {
		from := Ident(cfg.AppKey.From)
		if cfg.AppSecret.From != cfg.AppKey.From {
			from = "environment and config file"
		}
		out.AppFrom = &from
	}
	if cfg.File != "" {
		out.ConfigFile = new(c.mask.path(cfg.File))
	}
	for _, p := range cfg.Problems {
		out.ConfigProblems = append(out.ConfigProblems, s.problemText(p))
	}
	rate, rateFrom := cfg.RateFile.Value, cfg.RateFile.From
	if rate == "" && s.auth != nil {
		rate, rateFrom = schwab.DefaultRateFile(cfg.AppKey.Value, s.auth.TokenFile()), config.FromDefault
	}
	if rate != "" {
		out.RateFile, out.RateFileFrom = new(c.mask.path(rate)), new(Ident(rateFrom))
	}

	// The login, as the token file holds it; the Auth knows whether Schwab
	// refused it.
	var action error
	if err := s.configError(); err != nil {
		action = err
	} else if s.auth == nil {
		action = s.notConfigured()
	}
	if st, err := s.loginStatus(); err != nil {
		c.s.log.Warn("the token file could not be read", "error", err)
		if action == nil {
			action = c.failf(codeNotLoggedIn, "The Schwab login at %s could not be read. "+loginAdvice, out.TokenFile)
		}
	} else {
		out.LoggedIn, out.Refused = st.LoggedIn, st.Rejected != nil
		out.LoginAt, out.ExpiresAt = instantPtr(st.LoginAt), instantPtr(st.ExpiresAt)
		if !st.ExpiresAt.IsZero() {
			left, soon := lookup.ExpiresIn(st.ExpiresAt, c.now)
			out.HoursLeft, out.ExpiresSoon = new(max(0, int(left/time.Hour))), soon
		}
		switch {
		case action != nil:
		case !st.LoggedIn:
			action = s.notLoggedIn()
		case out.Refused || !st.ExpiresAt.IsZero() && !c.now.Before(st.ExpiresAt):
			action = s.errorFor(codeLoginExpired)
		case out.ExpiresSoon:
			action = c.failf(codeLoginExpired, "Ask the user to run `schwabtx login` in their own terminal before %s ET; "+
				"do not run it yourself, it needs their browser.", Ident(st.ExpiresAt.In(nytime.NewYork()).Format("2006-01-02 15:04")))
		}
	}

	// The store: its file's metadata, and whether the lazy reader opens it.
	out.Store = storeState{File: c.mask.path(cfg.StoreFile.Value), From: Ident(cfg.StoreFile.From)}
	if cfg.StoreFile.Value != "" {
		if _, err := os.Stat(cfg.StoreFile.Value); err == nil {
			out.Store.Exists = true
		}
		var waiting []history.AccountWaiting
		newer := false
		err := c.store.read(ctx, func(r *history.Reader) error {
			var err error
			waiting, err = r.Waiting(ctx)
			newer = r.NewerRules()
			return err
		})
		if code, ok := storeCode(err); ok {
			out.Store.Problem = new(Ident(code))
			if action == nil && code != codeNoStore {
				action = s.errorFor(code)
			}
		} else if err != nil {
			return out, err // the call's context ended
		} else {
			out.Store.Usable = true
			n := 0
			for _, w := range waiting {
				n += w.Uncertain + w.HeldOut + w.Attention + w.Filtered
			}
			out.Store.Waiting = &n
			if newer {
				c.warn("store_newer_rules", "A newer schwabtx derived the local history; ask the user to update schwab-mcp to the same version.")
			}
		}
	}
	if e, ok := errors.AsType[*toolErr](action); ok {
		out.Action = &e.msg
	}
	return out, nil
}

// loginStatus reads the token file: through the Auth, which knows whether
// Schwab refused the login, when there is one, or else by its path alone.
func (s *Server) loginStatus() (schwab.Status, error) {
	if s.auth != nil {
		return s.auth.Status()
	}
	if s.o.Config.TokenFile.Value == "" {
		return schwab.Status{}, nil
	}
	return schwab.StatusOf(s.o.Config.TokenFile.Value, s.warnOutside("schwab"))
}

// problemText words p for auth_status: the setting, what its code means,
// and its path as masker.path shows it.
func (s *Server) problemText(p config.Problem) FreeText {
	words, ok := problemWords[p.Code]
	if !ok {
		words = p.Code
	}
	if p.Path == "" {
		return s.mask.compose("%s: %s", Ident(p.Setting), Ident(words))
	}
	return s.mask.compose("%s: %s (%s)", Ident(p.Setting), Ident(words), s.mask.path(p.Path))
}

// Check writes what auth_status answers, for a person at a terminal, as
// schwab-mcp --check prints it: each file the server uses and where its
// setting came from, the API's host when it is not Schwab's, the login, the
// store, the config's problems, and what the model is told to ask the user
// to do. It runs auth_status as a client's call does, so it sends nothing
// to Schwab and writes no secret.
func (s *Server) Check(ctx context.Context, w io.Writer) error {
	b, e := s.runners["auth_status"](ctx, json.RawMessage(`{}`))
	if e != nil {
		return e
	}
	var st authStatusOut
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	line := func(name, format string, args ...any) {
		fmt.Fprintf(w, "  %-13s%s\n", name, fmt.Sprintf(format, args...))
	}
	from := func(f *Ident) string {
		if f == nil {
			return ""
		}
		return " (" + string(*f) + ")"
	}
	if st.ConfigFile != nil {
		fmt.Fprintf(w, "Reading %s:\n", *st.ConfigFile)
	} else {
		fmt.Fprintln(w, "Reading no config file:")
	}
	switch {
	case st.AppConfigured:
		line("app key", "set%s", from(st.AppFrom))
	default:
		line("app key", "not set")
	}
	switch host := string(st.APIHost); {
	case host == "":
		line("API", "none")
	case host != schwabHost:
		line("API", "%s, which is not Schwab's", host)
	default:
		line("API", "%s", host)
	}
	line("token file", "%s%s", st.TokenFile, from(&st.TokenFileFrom))
	if st.RateFile != nil {
		line("rate file", "%s%s", *st.RateFile, from(st.RateFileFrom))
	}
	store := fmt.Sprintf("%s%s", st.Store.File, from(&st.Store.From))
	switch {
	case st.Store.Usable && st.Store.Waiting != nil:
		store += fmt.Sprintf(", usable; %d waiting for you in schwabtx status", *st.Store.Waiting)
	case st.Store.Problem != nil:
		store += ", " + string(*st.Store.Problem)
	}
	line("store", "%s", store)
	switch {
	case !st.LoggedIn:
		line("login", "none")
	case st.LoginAt != nil && st.ExpiresAt != nil && st.HoursLeft != nil:
		login := fmt.Sprintf("made %s, lapses %s, in %d hours", personTime(*st.LoginAt), personTime(*st.ExpiresAt), *st.HoursLeft)
		if st.Refused {
			login += "; Schwab refused it"
		}
		line("login", "%s", login)
	default:
		line("login", "saved")
	}
	for _, p := range st.ConfigProblems {
		line("problem", "%s", p)
	}
	for _, wn := range st.Warnings {
		line("warning", "%s", wn.Message)
	}
	if st.Action != nil {
		fmt.Fprintf(w, "The model is told: %s\n", *st.Action)
	} else {
		fmt.Fprintln(w, "Nothing to do: the server can read Schwab.")
	}
	return nil
}

// personTime writes i, an instant in New York, as a person reads it.
func personTime(i Instant) string {
	t, err := time.Parse(time.RFC3339, string(i))
	if err != nil {
		return string(i)
	}
	return t.Format("2006-01-02 15:04") + " ET"
}
