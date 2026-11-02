package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dterei/schwab-mcp/internal/schwab"
)

// expiryWarningWindow is how long before the login lapses the tools start to
// warn. Two days, so that a snapshot scheduled once a day warns at least a
// day ahead, whatever the time of day of the login.
const expiryWarningWindow = 48 * time.Hour

// newAuth returns the login for cfg, kept in cfg.TokenFile and shared with
// any other process using that file. Its warnings go to stderr.
func newAuth(cfg config, httpClient *http.Client, stderr io.Writer) (*schwab.Auth, error) {
	return schwab.NewAuth(schwab.Config{
		AppKey:      cfg.AppKey,
		AppSecret:   cfg.AppSecret,
		CallbackURL: cfg.CallbackURL,
		BaseURL:     cfg.BaseURL,
		TokenFile:   cfg.TokenFile,
		HTTPClient:  httpClient,
		Now:         func() time.Time { return timeNow() },
		Warn:        func(msg string) { fmt.Fprintln(stderr, "warning:", msg) },
	})
}

// openLogin checks that there is a saved login, warning on stderr when it
// lapses soon.
func openLogin(cfg config, httpClient *http.Client, stderr io.Writer) (*schwab.Auth, error) {
	auth, err := newAuth(cfg, httpClient, stderr)
	if err != nil {
		return nil, err
	}
	st, err := auth.Status()
	if err != nil {
		return nil, err
	}
	if !st.LoggedIn {
		return nil, hint(schwab.ErrNotLoggedIn)
	}
	if w := expiryWarning(st.ExpiresAt); w != "" {
		fmt.Fprintln(stderr, "warning:", w)
	}
	return auth, nil
}

// hint adds to an error from the login what to do about it.
func hint(err error) error {
	var what string
	switch {
	case errors.Is(err, schwab.ErrNotLoggedIn):
		what = "run `schwabtx login` first"
	case errors.Is(err, schwab.ErrLoginExpired):
		what = "run `schwabtx login` again (Schwab ends a login after 7 days, or sooner when you log in elsewhere)"
	case errors.Is(err, schwab.ErrAppRejected):
		what = `check SCHWAB_APP_KEY and SCHWAB_APP_SECRET, and that the app is "Ready For Use"`
	default:
		return err
	}
	return &hinted{err: err, hint: what}
}

// hinted is an error with what to do about it.
type hinted struct {
	err  error
	hint string
}

func (h *hinted) Error() string { return h.err.Error() + "; " + h.hint }

func (h *hinted) Unwrap() error { return h.err }

// expiryWarning returns a message when the login, which lapses at expiresAt,
// is due for renewal within expiryWarningWindow.
func expiryWarning(expiresAt time.Time) string {
	if expiresAt.IsZero() {
		return ""
	}
	left := expiresAt.Sub(timeNow())
	if left <= 0 || left > expiryWarningWindow {
		return ""
	}
	when := "in less than an hour"
	if hours := int(left.Round(time.Hour) / time.Hour); hours == 1 {
		when = "in about an hour"
	} else if hours > 1 {
		when = fmt.Sprintf("in about %d hours", hours)
	}
	return fmt.Sprintf("your Schwab login expires %s; run `schwabtx login` to renew it", when)
}

// login walks through the browser authorization. Schwab only allows https
// callback URLs, so rather than run a local TLS server we have the user paste
// back the address their browser was redirected to.
func login(ctx context.Context, cfg config, auth *schwab.Auth, stdin io.Reader, out io.Writer) error {
	state, err := schwab.NewState()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, `1. Open this link in your browser and sign in with your Schwab brokerage login:

   %s

2. Tick the accounts you want to share and approve.
3. Your browser then lands on %s/... and shows a "can't connect" error.
   That is expected. Copy the whole address from the address bar.
4. Paste it here and press Enter (be quick, the code in it expires in about 30 seconds):

> `, auth.AuthorizeURL(state), cfg.CallbackURL)

	// Read in the background so Ctrl-C still works while we wait for the paste.
	type pasted struct {
		line string
		err  error
	}
	ch := make(chan pasted, 1)
	go func() {
		line, err := bufio.NewReaderSize(stdin, 64*1024).ReadString('\n')
		ch <- pasted{line, err}
	}()
	var p pasted
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p = <-ch:
	}
	if p.err != nil && (!errors.Is(p.err, io.EOF) || strings.TrimSpace(p.line) == "") {
		return fmt.Errorf("reading the pasted address: %w", p.err)
	}
	code, err := schwab.CodeFromRedirect(p.line, state)
	if err != nil {
		return err
	}
	if err := auth.Login(ctx, code); err != nil {
		return hint(err)
	}
	fmt.Fprintf(out, "\nLogged in. Token saved to %s.\nSchwab expires this login after 7 days, so run `schwabtx login` again when it does.\n", cfg.TokenFile)
	return nil
}
