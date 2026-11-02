// Package schwab is a client for the Charles Schwab Trader API. So far it
// holds the login: the browser login, the refresh of the access token, and
// the token file that every process using the login shares.
//
// Processes share the login through the token file. A lock file next to it
// lets one process at a time read or change it, and a process re-reads the
// file before it refreshes, because another process may have refreshed the
// token, or logged in again, since it last looked.
package schwab

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultBaseURL is where Schwab's API is.
	DefaultBaseURL = "https://api.schwabapi.com"
	// DefaultCallbackURL is the callback URL that the README has you
	// register for the app.
	DefaultCallbackURL = "https://127.0.0.1"

	// LoginLifetime is how long a login lasts. Schwab's refresh tokens stop
	// working 7 days after the browser login that issued them, and
	// refreshing the access token does not extend that.
	LoginLifetime = 7 * 24 * time.Hour

	// refreshMargin is how long before an access token expires it is
	// refreshed.
	refreshMargin = time.Minute
)

// The ways the login can fail that a caller may want to tell apart. They come
// wrapped with details, so check for them with errors.Is.
var (
	// ErrNotLoggedIn means that there is no token file yet.
	ErrNotLoggedIn = errors.New("not logged in yet; run `schwabtx login` first")
	// ErrLoginExpired means that Schwab refused the saved refresh token: the
	// 7 days are up, or a login elsewhere replaced it.
	ErrLoginExpired = errors.New("Schwab rejected the saved login (it expires 7 days after you log in); run `schwabtx login` again")
	// ErrAppRejected means that Schwab refused the app key or secret.
	ErrAppRejected = errors.New("Schwab rejected the app key or secret; check SCHWAB_APP_KEY and SCHWAB_APP_SECRET and that the app is \"Ready For Use\"")
)

// Config says which app to log in with and where the login is kept.
type Config struct {
	AppKey    string
	AppSecret string
	// CallbackURL must match one of the app's callback URLs exactly. Empty
	// means DefaultCallbackURL.
	CallbackURL string
	// BaseURL is where Schwab's API is. Empty means DefaultBaseURL.
	BaseURL string
	// TokenFile holds the login for every process that uses it. Next to it,
	// TokenFile + ".lock" is the lock file.
	TokenFile string
	// HTTPClient makes the token requests. Nil means a client with a
	// 30-second timeout.
	HTTPClient *http.Client
	// Now tells the time. Nil means time.Now.
	Now func() time.Time
}

// Auth is one process's view of the login in the token file. It is safe for
// concurrent use.
type Auth struct {
	cfg      Config
	lockWait time.Duration

	// sem lets one goroutine at a time read the token file or refresh, so
	// that the others wait for its result rather than refresh again.
	sem chan struct{}

	mu  sync.Mutex
	tok *token // as last read or written; nil before that
}

// NewAuth returns an Auth for cfg. It does not read the token file yet.
func NewAuth(cfg Config) (*Auth, error) {
	if cfg.AppKey == "" || cfg.AppSecret == "" {
		return nil, errors.New("the app key and secret are required")
	}
	if cfg.TokenFile == "" {
		return nil, errors.New("the token file is required")
	}
	if cfg.CallbackURL == "" {
		cfg.CallbackURL = DefaultCallbackURL
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Auth{cfg: cfg, lockWait: defaultLockWait, sem: make(chan struct{}, 1)}, nil
}

// Load reads the login from the token file. Calling it first is optional:
// AccessToken reads the file when it needs to. It returns ErrNotLoggedIn if
// there is no token file.
func (a *Auth) Load(ctx context.Context) error {
	// Check before taking the lock, so that a failed first run leaves no
	// lock file behind.
	if _, err := os.Stat(a.cfg.TokenFile); errors.Is(err, fs.ErrNotExist) {
		return ErrNotLoggedIn
	}
	return a.locked(ctx, func() error {
		t, err := readToken(a.cfg.TokenFile)
		if err != nil {
			return err
		}
		a.set(t)
		return nil
	})
}

// LoginExpiresAt returns when the login lapses and `schwabtx login` is
// needed again, as of the last time the token file was read or written. It
// returns the zero time if that is unknown.
func (a *Auth) LoginExpiresAt() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil || a.tok.RefreshObtainedAt.IsZero() {
		return time.Time{}
	}
	return a.tok.RefreshObtainedAt.Add(LoginLifetime)
}

// AccessToken returns an access token for Schwab's API. When the last one
// it saw has expired, or is about to, it re-reads the token file and uses
// the token there, refreshing it first if that one has expired too.
func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	if tok, ok := a.fresh(""); ok {
		return tok, nil
	}
	return a.renew(ctx, "")
}

// Refresh returns a new access token after Schwab refused rejected, the one
// AccessToken returned. If another process has refreshed meanwhile, its token
// is used; otherwise the token is refreshed.
func (a *Auth) Refresh(ctx context.Context, rejected string) (string, error) {
	return a.renew(ctx, rejected)
}

// Login exchanges the authorization code from a browser login (see
// AuthorizeURL and CodeFromRedirect) for a new login, and saves it in the
// token file in place of the old one.
func (a *Auth) Login(ctx context.Context, code string) error {
	// The code lapses about 30 seconds after the login, so the exchange
	// comes first. Only the write needs the lock.
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {a.cfg.CallbackURL},
	})
	if err != nil {
		return fmt.Errorf("exchanging the authorization code: %w", err)
	}
	if res.RefreshToken == "" {
		return errors.New("Schwab's token response had no refresh token")
	}
	now := a.cfg.Now()
	t := &token{
		AccessToken:       res.AccessToken,
		RefreshToken:      res.RefreshToken,
		AccessExpiresAt:   now.Add(time.Duration(res.ExpiresIn) * time.Second),
		RefreshObtainedAt: now,
	}
	return a.locked(ctx, func() error {
		if err := writeToken(a.cfg.TokenFile, t); err != nil {
			return err
		}
		a.set(t)
		return nil
	})
}

// renew re-reads the token file under the lock, and returns its access token
// if that is fresh and not rejected; otherwise it refreshes the token and
// saves the result.
func (a *Auth) renew(ctx context.Context, rejected string) (string, error) {
	var tok string
	err := a.locked(ctx, func() error {
		t, err := readToken(a.cfg.TokenFile)
		if err != nil {
			return err
		}
		a.set(t)
		if fresh, ok := a.fresh(rejected); ok {
			tok = fresh
			return nil
		}
		tok, err = a.refresh(ctx, t)
		return err
	})
	return tok, err
}

// refresh trades t's refresh token for a new access token and saves the
// result. The caller holds the lock.
func (a *Auth) refresh(ctx context.Context, t *token) (string, error) {
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
	})
	if err != nil {
		var se *Error
		switch {
		case errors.As(err, &se) && se.Status == http.StatusUnauthorized:
			return "", fmt.Errorf("%w: %w", ErrAppRejected, err)
		case errors.As(err, &se) && se.Status == http.StatusBadRequest:
			return "", fmt.Errorf("%w: %w", ErrLoginExpired, err)
		}
		return "", fmt.Errorf("refreshing access token: %w", err)
	}
	next := *t
	next.AccessToken = res.AccessToken
	next.AccessExpiresAt = a.cfg.Now().Add(time.Duration(res.ExpiresIn) * time.Second)
	if res.RefreshToken != "" {
		next.RefreshToken = res.RefreshToken
	}
	a.set(&next)
	if err := writeToken(a.cfg.TokenFile, &next); err != nil {
		return "", err
	}
	return next.AccessToken, nil
}

// fresh returns the access token last seen if it is not rejected and does
// not expire within refreshMargin.
func (a *Auth) fresh(rejected string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil || a.tok.AccessToken == "" || a.tok.AccessToken == rejected {
		return "", false
	}
	if !a.cfg.Now().Before(a.tok.AccessExpiresAt.Add(-refreshMargin)) {
		return "", false
	}
	return a.tok.AccessToken, true
}

func (a *Auth) set(t *token) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = t
}

// locked runs fn while holding both the in-process semaphore and the lock
// file.
func (a *Auth) locked(ctx context.Context, fn func() error) error {
	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-a.sem }()
	l, err := lockFile(ctx, a.cfg.TokenFile+".lock", a.lockWait)
	if err != nil {
		return err
	}
	defer l.release()
	return fn()
}
