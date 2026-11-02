// Package schwab is a client for the Charles Schwab Trader API. So far it
// holds the login: the browser login, the refresh of the access token, and
// the token file that every process using the login shares.
//
// Processes share the login through the token file. A lock file next to it
// lets one process at a time decide whether to refresh, refresh, or replace
// the login, and a process re-reads the file under the lock first, because
// another process may have refreshed the token, or logged in again, since it
// last looked. The lock is held for a file read, at most one token request
// and a file write, and nothing else is locked while it is held.
package schwab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

const (
	// DefaultBaseURL is where Schwab's API is.
	DefaultBaseURL = "https://api.schwabapi.com"
	// DefaultCallbackURL is the callback URL that the README has you
	// register for the app.
	DefaultCallbackURL = "https://127.0.0.1"

	// LoginLifetime is how long a login lasts. Schwab's refresh tokens stop
	// working 7 days after the browser login that issued them, and
	// refreshing the access token does not extend that. A later login can
	// end one sooner.
	LoginLifetime = 7 * 24 * time.Hour

	// DefaultLockWait is how long a process waits for another to finish with
	// the token file before it gives up: several times the longest that
	// another holds the lock, one token request.
	DefaultLockWait = 2 * time.Minute

	// tokenRequestTimeout bounds each token request.
	tokenRequestTimeout = 30 * time.Second

	// refreshMargin is how long before an access token expires it is
	// refreshed.
	refreshMargin = time.Minute

	// freshTokenAge is how old an access token must be before Schwab
	// refusing it leads to a refresh. Schwab refusing a token it has only
	// just issued means that something else is wrong, such as an API product
	// that the app lacks, which another token would not fix.
	freshTokenAge = time.Minute
)

// The ways the login can fail that a caller may want to tell apart. They come
// wrapped with details, so check for them with errors.Is.
var (
	// ErrNotLoggedIn means that there is no token file yet.
	ErrNotLoggedIn = errors.New("not logged in yet")
	// ErrLoginExpired means that Schwab refused the saved refresh token: the
	// 7 days are up, or a later login replaced it.
	ErrLoginExpired = errors.New("Schwab rejected the saved login")
	// ErrAppRejected means that Schwab refused the app key or secret.
	ErrAppRejected = errors.New("Schwab rejected the app key or secret")
	// ErrTokenRefused means that Schwab refused an access token it had
	// issued moments before, so a new one would not help.
	ErrTokenRefused = errors.New("Schwab refused an access token it had just issued")
)

// DefaultTokenFile returns where the login is kept unless Config says
// otherwise: schwabtx/token.json in the user config directory.
func DefaultTokenFile() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the user config directory: %w", err)
	}
	return filepath.Join(dir, "schwabtx", "token.json"), nil
}

// Config says which app to log in with and where the login is kept.
type Config struct {
	AppKey    string
	AppSecret string
	// CallbackURL must match one of the app's callback URLs exactly. Empty
	// means DefaultCallbackURL.
	CallbackURL string
	// BaseURL is where Schwab's API is. Empty means DefaultBaseURL.
	BaseURL string
	// TokenFile holds the login for every process that uses it. Empty means
	// DefaultTokenFile. Next to it are lock files, which are never deleted:
	// TokenFile + ".lock" for the login, and TokenFile + ".stream.lock",
	// kept for the streamer.
	TokenFile string
	// HTTPClient makes the token requests. Nil means a client with a
	// 30-second timeout.
	HTTPClient *http.Client
	// Now tells the time by which tokens expire. Nil means time.Now. Waits
	// use real timers whatever it says.
	Now func() time.Time
	// LockWait is how long to wait for another process to finish with the
	// token file. Zero means DefaultLockWait.
	LockWait time.Duration
	// Warn is told of problems that do not stop the login from working, such
	// as a token file that others could read. Nil means they go untold.
	Warn func(msg string)
}

// Auth is one process's view of the login in the token file. It is safe for
// concurrent use.
type Auth struct {
	cfg Config
	// tokenFile is cfg.TokenFile made absolute, with links followed, so that
	// processes that name one file differently share its lock, and a rewrite
	// replaces the file rather than a link to it.
	tokenFile string

	// sem lets one goroutine of this process at a time take the lock file.
	// Every Auth on the same file shares it, since on some network volumes
	// the lock file does not keep out a second open in the same process.
	sem chan struct{}
	// lock and tryLock take the lock file; tests replace them.
	lock    func(ctx context.Context, path string, wait time.Duration) (*filelock.Lock, error)
	tryLock func(path string) (*filelock.Lock, bool, error)
	// unlocked says, once, that the file system cannot lock files.
	unlocked sync.Once

	mu  sync.Mutex
	tok *token // as last read or written; nil before that
	// refused is a refresh token that Schwab refused, and refusedErr why, so
	// that it is not sent again while the file holds it.
	refused    string
	refusedErr error
}

// NewAuth returns an Auth for cfg. It does not read the token file yet.
func NewAuth(cfg Config) (*Auth, error) {
	if cfg.AppKey == "" || cfg.AppSecret == "" {
		return nil, errors.New("the app key and secret are required")
	}
	if cfg.TokenFile == "" {
		var err error
		if cfg.TokenFile, err = DefaultTokenFile(); err != nil {
			return nil, err
		}
	}
	tokenFile, err := resolve(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("finding the token file: %w", err)
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
	if cfg.LockWait <= 0 {
		cfg.LockWait = DefaultLockWait
	}
	return &Auth{
		cfg: cfg, tokenFile: tokenFile, sem: semFor(tokenFile),
		lock: filelock.Acquire, tryLock: filelock.TryAcquire,
	}, nil
}

var (
	semsMu sync.Mutex
	sems   = map[string]chan struct{}{} // by token file
)

// semFor returns the semaphore for the token file at path, which every Auth
// on that file in this process shares.
func semFor(path string) chan struct{} {
	semsMu.Lock()
	defer semsMu.Unlock()
	sem, ok := sems[path]
	if !ok {
		sem = make(chan struct{}, 1)
		sems[path] = sem
	}
	return sem
}

// resolve returns path made absolute, with links followed, and on Windows
// short names made long, in the longest part of it that exists: before the
// first login neither the file nor its directory may exist.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for p := abs; ; {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(p), rest)
		p = parent
	}
}

// Status describes the login in the token file, without its tokens.
type Status struct {
	// TokenFile is the file the login is kept in, made absolute, with links
	// followed.
	TokenFile string
	// LoggedIn is whether TokenFile holds a login.
	LoggedIn bool
	// LoginAt is when the browser login was, and ExpiresAt when it lapses,
	// LoginLifetime later. Both are zero when they are not known.
	LoginAt, ExpiresAt time.Time
	// Scope is what Schwab said it granted, if it said.
	Scope string
	// Rejected, when not nil, is why Schwab refused this login when this
	// process last tried to refresh it. It wraps ErrLoginExpired.
	Rejected error
}

// Status reads the token file and describes the login in it. It makes no
// request to Schwab, and does not wait for a refresh in progress.
func (a *Auth) Status() (Status, error) {
	st := Status{TokenFile: a.tokenFile}
	t, err := a.read()
	if errors.Is(err, ErrNotLoggedIn) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.LoggedIn = true
	st.Scope = t.Scope
	if !t.RefreshObtainedAt.IsZero() {
		st.LoginAt = t.RefreshObtainedAt
		st.ExpiresAt = t.RefreshObtainedAt.Add(LoginLifetime)
	}
	a.mu.Lock()
	if t.RefreshToken == a.refused {
		st.Rejected = a.refusedErr
	}
	a.mu.Unlock()
	return st, nil
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

// Refresh returns a new access token after Schwab refused rejected, one that
// AccessToken returned. If another process has saved a new token meanwhile,
// that is used; otherwise the token is refreshed. If Schwab issued rejected
// moments ago, Refresh returns ErrTokenRefused instead: the caller should
// report Schwab's response.
func (a *Auth) Refresh(ctx context.Context, rejected string) (string, error) {
	return a.renew(ctx, rejected)
}

// Login exchanges the authorization code from a browser login (see
// AuthorizeURL and CodeFromRedirect) for a new login, and saves it in the
// token file in place of the old one.
func (a *Auth) Login(ctx context.Context, code string) error {
	// If no other process holds the lock, hold it through the exchange, so
	// that no refresh with the old login is at Schwab while it is replaced.
	// Otherwise exchange first anyway: the code lapses about 30 seconds
	// after the browser login.
	release, held, err := a.tryHold()
	if err != nil {
		return err
	}
	if held {
		defer release()
	}
	start := a.now()
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {code},
		"redirect_uri": {a.cfg.CallbackURL},
	})
	if err != nil {
		if se, ok := errors.AsType[*Error](err); ok && (se.Code == "invalid_client" || se.Code == "" && se.Status == http.StatusUnauthorized) {
			return fmt.Errorf("%w: %w", ErrAppRejected, err)
		}
		return fmt.Errorf("exchanging the authorization code: %w", err)
	}
	if res.RefreshToken == "" {
		return errors.New("Schwab's token response had no refresh token")
	}
	// The tokens' lifetimes are counted from before the request, so that a
	// slow answer does not stretch them.
	t := &token{
		AccessToken:       res.AccessToken,
		RefreshToken:      res.RefreshToken,
		AccessObtainedAt:  start,
		AccessExpiresAt:   start.Add(time.Duration(res.ExpiresIn) * time.Second),
		RefreshObtainedAt: start,
		Scope:             res.Scope,
	}
	if !held {
		// The code is spent, so save the login whatever ctx says now.
		if release, err = a.hold(context.WithoutCancel(ctx)); err != nil {
			return fmt.Errorf("saving the login: %w", err)
		}
		defer release()
	}
	// A login that another process saved after this one began is the newer
	// one, so it stays.
	if saved, err := readToken(a.tokenFile); err == nil && saved.RefreshObtainedAt.After(t.RefreshObtainedAt) {
		a.set(saved)
		return nil
	}
	if err := writeToken(a.tokenFile, t); err != nil {
		return err
	}
	a.set(t)
	return nil
}

// renew re-reads the token file under the lock, and returns its access token
// if that is fresh and not rejected; otherwise it refreshes the token and
// saves the result.
func (a *Auth) renew(ctx context.Context, rejected string) (string, error) {
	release, err := a.hold(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	t, err := a.read()
	if err != nil {
		return "", err
	}
	a.set(t)
	if fresh, ok := a.fresh(rejected); ok {
		return fresh, nil
	}
	a.mu.Lock()
	refused, refusedErr := a.refused, a.refusedErr
	a.mu.Unlock()
	if t.RefreshToken == refused {
		return "", refusedErr
	}
	if rejected != "" && t.AccessToken == rejected && !t.AccessObtainedAt.IsZero() &&
		a.now().Sub(t.AccessObtainedAt) < freshTokenAge {
		return "", ErrTokenRefused
	}
	return a.refresh(ctx, t)
}

// refresh trades t's refresh token for a new access token and saves the
// result. The caller holds the lock and has just read t from the file.
func (a *Auth) refresh(ctx context.Context, t *token) (string, error) {
	start := a.now()
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
	})
	if err != nil {
		err = refreshError(err)
		if errors.Is(err, ErrLoginExpired) {
			a.mu.Lock()
			a.refused, a.refusedErr = t.RefreshToken, err
			a.mu.Unlock()
		}
		return "", err
	}
	next := *t
	next.AccessToken = res.AccessToken
	next.AccessObtainedAt = start
	next.AccessExpiresAt = start.Add(time.Duration(res.ExpiresIn) * time.Second)
	if res.RefreshToken != "" {
		next.RefreshToken = res.RefreshToken
	}
	if res.Scope != "" {
		next.Scope = res.Scope
	}
	a.set(&next)
	if err := writeToken(a.tokenFile, &next); err != nil {
		return "", err
	}
	return next.AccessToken, nil
}

// refreshError says what a failed refresh means. Schwab refuses a lapsed or
// replaced refresh token with HTTP 400 and unsupported_token_type, nesting
// the real reason, invalid_grant or refresh_token_authentication_error;
// older reports also show invalid_client with "refresh token invalid". A
// wrong app key or secret gets invalid_client and "Unauthorized".
func refreshError(err error) error {
	se, ok := errors.AsType[*Error](err)
	if !ok || se.Status != http.StatusBadRequest && se.Status != http.StatusUnauthorized {
		return fmt.Errorf("refreshing the access token: %w", err)
	}
	switch {
	case loginCodes[se.Code], loginCodes[se.Detail], strings.Contains(strings.ToLower(se.Body), "refresh token"):
		return fmt.Errorf("%w: %w", ErrLoginExpired, err)
	case se.Code == "invalid_client", se.Status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", ErrAppRejected, err)
	}
	return fmt.Errorf("%w: %w", ErrLoginExpired, err)
}

// loginCodes are the OAuth error codes with which Schwab refuses a refresh
// token that no longer works.
var loginCodes = map[string]bool{
	"invalid_grant": true, "unsupported_token_type": true, "refresh_token_authentication_error": true,
}

// fresh returns the access token last seen if it is not rejected and does
// not expire within refreshMargin.
func (a *Auth) fresh(rejected string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == nil || a.tok.AccessToken == "" || a.tok.AccessToken == rejected {
		return "", false
	}
	if !a.now().Before(a.tok.AccessExpiresAt.Add(-refreshMargin)) {
		return "", false
	}
	return a.tok.AccessToken, true
}

// now returns the time without its monotonic clock reading, which may stop
// while the computer sleeps, so that times computed from it compare by the
// wall clock, as those read from the token file do.
func (a *Auth) now() time.Time {
	return a.cfg.Now().Round(0)
}

func (a *Auth) set(t *token) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.tok = t
}

// hold takes the in-process semaphore and then the lock file, waiting while
// another holds either, and returns what lets go of both.
func (a *Auth) hold(ctx context.Context) (release func(), err error) {
	select {
	case a.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	l, err := a.lock(ctx, a.tokenFile+".lock", a.cfg.LockWait)
	return a.held(l, err)
}

// tryHold is hold without the wait: it reports whether it took both.
func (a *Auth) tryHold() (release func(), ok bool, err error) {
	select {
	case a.sem <- struct{}{}:
	default:
		return nil, false, nil
	}
	l, ok, err := a.tryLock(a.tokenFile + ".lock")
	if err == nil && !ok {
		<-a.sem
		return nil, false, nil
	}
	release, err = a.held(l, err)
	return release, err == nil, err
}

// held finishes hold and tryHold once the semaphore is taken: l is the lock
// file, or err why it is not held.
func (a *Auth) held(l *filelock.Lock, err error) (func(), error) {
	switch {
	case err == nil:
		return func() { l.Release(); <-a.sem }, nil
	case errors.Is(err, filelock.ErrUnsupported):
		// A refresh race cannot end the login, and a login and a refresh
		// racing is rare, so carry on as before there were locks.
		a.unlocked.Do(func() {
			a.warn(fmt.Sprintf("%s cannot be locked (%v), so processes that use the login may refresh it at once; keep it on a local disk", a.tokenFile, err))
		})
		return func() { <-a.sem }, nil
	case errors.Is(err, filelock.ErrTimeout):
		err = fmt.Errorf("another process kept the Schwab login locked for %v: %w", a.cfg.LockWait, err)
	}
	<-a.sem
	return nil, err
}

// read reads the token file. On Linux and macOS a file that others can read,
// such as one copied in by hand, is made private, with a warning.
func (a *Auth) read() (*token, error) {
	t, err := readToken(a.tokenFile)
	if err != nil || runtime.GOOS == "windows" {
		return t, err
	}
	if info, err := os.Stat(a.tokenFile); err == nil && info.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(a.tokenFile, 0o600); err != nil {
			a.warn(fmt.Sprintf("%s can be read by others and could not be made private: %v", a.tokenFile, err))
		} else {
			a.warn(a.tokenFile + " could be read by others; it is now readable only by you")
		}
	}
	return t, nil
}

func (a *Auth) warn(msg string) {
	if a.cfg.Warn != nil {
		a.cfg.Warn(msg)
	}
}
