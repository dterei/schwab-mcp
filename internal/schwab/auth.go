// Package schwab is a client for the Charles Schwab Trader API: the browser
// login, the refresh of the access token, the token file that every process
// using the login shares, and a Client that reads from the API, trying again
// after a failure where that is safe, under a rate limit that every process
// using the app key shares.
//
// Processes share the login through the token file. A lock file next to it
// lets one process at a time decide whether to refresh, refresh, or replace
// the login, and a process re-reads the file under the lock first, because
// another process may have refreshed the token, or logged in again, since it
// last looked. The lock is held for a file read, at most one token request
// and a file write, and nothing else is locked while it is held.
//
// They share the rate limit through a small file of its own, which each
// request locks, reads and rewrites; see ClientConfig.
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

	// refreshTries is how many times a refresh that fails for a while is
	// sent before its failure is returned, and refreshWait is the wait after
	// the first. A look at the token file after a refusal can send it once
	// more.
	refreshTries = 4
	refreshWait  = time.Second

	// recheckWait is how long a refresh that Schwab refused waits before it
	// looks at the token file once more: twice the longest that a process
	// waiting for the lock sleeps between looks, so that a login waiting to
	// save gets in first.
	recheckWait = 500 * time.Millisecond

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
	// HTTPClient makes the token requests, and a Client's API requests. Nil
	// means a client with a 30-second timeout whose HTTP/2 connections are
	// checked with a ping when they go quiet, so that one that has stopped
	// answering, as after a change of network, is not used again.
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

	// flights are the renewals in progress through this Auth, by the access
	// token that they replace after Schwab refused it, or "". Goroutines that
	// need the same renewal wait for it rather than make their own.
	flights map[string]*flight

	// sleep waits between tries of a refresh, and jitter picks a wait up to
	// the one given; tests replace them.
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(d time.Duration) time.Duration
}

// flight is a renewal in progress.
type flight struct {
	done chan struct{}
	tok  string
	err  error
	// quit means that it ended without an answer, because the caller who
	// made it gave up or had no time left for the next try, or a panic.
	// Then one of those waiting carries on its tries from next.
	quit bool
	next series
	// followers is how many others have waited for it, for tests.
	followers int
}

// series is how far a renewal's tries have got, so that a goroutine that
// takes it over carries on rather than starts over.
type series struct {
	try, requests, timeouts int
	rechecked               bool
	// due is when the next try may go, by the system clock, or zero.
	due time.Time
	// last is the last try's failure.
	last error
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
	if u, err := url.Parse(cfg.BaseURL); err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Host == "" ||
		u.User != nil || strings.ContainsAny(cfg.BaseURL, "?#") {
		return nil, fmt.Errorf("the base URL %q is not an https or http address without a user, query or fragment", cfg.BaseURL)
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: newTransport()}
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
		sleep: sleep, jitter: equalJitter,
	}, nil
}

// newTransport returns Go's default transport with a health check for HTTP/2
// connections: Go keeps using one that has stopped answering until the
// system gives up on it, which can take many minutes.
func newTransport() http.RoundTripper {
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return http.DefaultTransport
	}
	t := def.Clone()
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 10 * time.Second}
	return t
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

// resolve returns the file that path names, as the system takes it when it
// opens or creates the file: an absolute path with every link followed, so
// that every process naming the file, however it spells it, uses the same
// lock file, and a rewrite replaces the file rather than a link to it. What
// does not exist yet, as before the first login, is taken by name, since it
// will be made with that name.
func resolve(path string) (string, error) {
	if runtime.GOOS == "windows" {
		return resolveWindows(path)
	}
	// Walk the path as the system does: ".." leads to the parent of the
	// folder reached so far, and a link's target takes its place, so that a
	// ".." after a link leads out of the link's target, and a link to a file
	// not made yet leads to where it will be.
	cur := "/"
	if !filepath.IsAbs(path) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		if cur, err = filepath.EvalSymlinks(wd); err != nil {
			cur = wd
		}
	}
	todo := strings.Split(path, "/")
	for links := 0; len(todo) > 0; {
		elem := todo[0]
		todo = todo[1:]
		switch elem {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, elem)
		if info, err := os.Lstat(next); err != nil || info.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}
		if links++; links > 255 {
			return "", fmt.Errorf("%s: too many links", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			cur = "/"
		}
		todo = append(strings.Split(target, "/"), todo...)
	}
	return cur, nil
}

// resolveWindows is resolve on Windows, which takes ".." by name: it makes
// path absolute, then follows links and makes short names long in the
// longest part of it that exists.
func resolveWindows(path string) (string, error) {
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
	before, _ := readToken(a.tokenFile) // to tell whether another login is saved meanwhile
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
	// A login that another process saved while this one waited, and that
	// began after this one, is the newer one, so it stays. A refresh keeps a
	// login's time, so only a login changes it. The times are compared only
	// for a login saved meanwhile, so that a clock that has been set back
	// since an earlier login does not keep that one.
	if saved, err := readToken(a.tokenFile); err == nil && (before == nil || !saved.RefreshObtainedAt.Equal(before.RefreshObtainedAt)) &&
		saved.RefreshObtainedAt.After(t.RefreshObtainedAt) {
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
// saves the result. Goroutines that renew through this Auth at once, for the
// same rejected token, share one renewal.
func (a *Auth) renew(ctx context.Context, rejected string) (string, error) {
	var from series
	for {
		a.mu.Lock()
		f, follow := a.flights[rejected]
		if follow {
			f.followers++
		} else {
			f = &flight{done: make(chan struct{})}
			if a.flights == nil {
				a.flights = map[string]*flight{}
			}
			a.flights[rejected] = f
		}
		a.mu.Unlock()
		if !follow {
			return a.lead(ctx, rejected, f, from)
		}
		select {
		case <-f.done:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if !f.quit {
			return f.tok, f.err
		}
		// The renewal ended without an answer. Carry on its tries, after the
		// wait it was due, if this caller has the time for it.
		from = f.next
		if wait := time.Until(from.due); wait > 0 {
			if !fits(ctx, wait) {
				return "", f.err // no more time than the one that gave up
			}
			if err := a.sleep(ctx, wait); err != nil {
				return "", withLast(err, f.err)
			}
		}
		if err := ctx.Err(); err != nil {
			return "", withLast(err, f.err)
		}
	}
}

// lead makes the renewal f, which others may wait for, from where from left
// off, and ends it whatever happens, a panic included, so that they do not
// wait forever.
func (a *Auth) lead(ctx context.Context, rejected string, f *flight, from series) (string, error) {
	finished := false
	defer func() {
		if !finished {
			f.quit = true // a panic is no answer for the others
		}
		a.mu.Lock()
		delete(a.flights, rejected)
		a.mu.Unlock()
		close(f.done)
	}()
	tok, next, quit, err := a.renewTries(ctx, rejected, from)
	f.tok, f.err, f.next, f.quit = tok, err, next, err != nil && quit
	finished = true
	return tok, err
}

// renewTries is renew for one goroutine, from where s left off. A refresh
// that fails for a while, on the network or at Schwab, is tried again, up to
// refreshTries times in all, but one that times out only once more, as a
// read is. Between tries the lock is let go and the wait comes without it,
// since another process may refresh meanwhile, and the next try re-reads the
// file to find out. A refresh that Schwab refuses looks at the file once
// more, since a login that was waiting for the lock may just have saved.
//
// quit reports that the tries ended without an answer, because ctx ended or
// its deadline left no time for the next try; then next says where they
// stopped, for another caller with more time to carry on from.
func (a *Auth) renewTries(ctx context.Context, rejected string, s series) (tok string, next series, quit bool, err error) {
	if s.try == 0 {
		s.try = 1
	}
	for {
		// A token request runs to its end whatever ctx says, so none begins
		// for a caller that has gone.
		if err := ctx.Err(); err != nil {
			return "", s, true, withLast(err, s.last)
		}
		s.due = time.Time{}
		tok, requested, err := a.renewOnce(ctx, rejected, s.requests+1)
		if requested {
			s.requests++
		}
		switch {
		case err == nil:
			return tok, s, false, nil
		case !requested && ctx.Err() != nil:
			return "", s, true, withLast(err, s.last) // the wait for the lock was cut short
		case requested && !s.rechecked && errors.Is(err, ErrLoginExpired):
			s.rechecked, s.last = true, err
			if serr := a.sleep(ctx, recheckWait); serr != nil {
				return "", s, true, err
			}
			continue
		case !refreshTransient(context.WithoutCancel(ctx), err):
			// Final. A token request does not see ctx, so its failure says
			// the same whatever ctx has done meanwhile.
			return "", s, false, err
		}
		if timedOut(err) {
			s.timeouts++
		}
		wait := a.backoff(s.try)
		if se, ok := errors.AsType[*Error](err); ok {
			wait = max(wait, se.RetryAfter)
		}
		s.last = err
		if s.try >= refreshTries || s.timeouts > 1 || wait > maxPause {
			return "", s, false, err
		}
		s.try++
		s.due = time.Now().Add(wait)
		if ctx.Err() != nil || !fits(ctx, wait) {
			return "", s, true, err
		}
		if serr := a.sleep(ctx, wait); serr != nil {
			return "", s, true, fmt.Errorf("%w while waiting to try again; the last try got: %w", serr, err)
		}
	}
}

// renewOnce is one try of renew. It reports whether it made a token request,
// which would be the renewal's nth.
func (a *Auth) renewOnce(ctx context.Context, rejected string, n int) (tok string, requested bool, err error) {
	release, err := a.hold(ctx)
	if err != nil {
		return "", false, err
	}
	defer release()
	t, err := a.read()
	if err != nil {
		return "", false, err
	}
	a.set(t)
	if fresh, ok := a.fresh(rejected); ok {
		return fresh, false, nil
	}
	a.mu.Lock()
	refused, refusedErr := a.refused, a.refusedErr
	a.mu.Unlock()
	if t.RefreshToken == refused {
		return "", false, refusedErr
	}
	// An issue time still to come, from a clock that has been set back
	// since, says nothing, so that token gets its refresh.
	if age := a.now().Sub(t.AccessObtainedAt); rejected != "" && t.AccessToken == rejected &&
		!t.AccessObtainedAt.IsZero() && age >= 0 && age < freshTokenAge {
		return "", false, ErrTokenRefused
	}
	tok, err = a.refresh(ctx, t, n)
	return tok, true, err
}

// refresh trades t's refresh token for a new access token and saves the
// result. The caller holds the lock and has just read t from the file. This
// is the nth token request of the renewal, which an error from Schwab says.
func (a *Auth) refresh(ctx context.Context, t *token, n int) (string, error) {
	start := a.now()
	res, err := a.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.RefreshToken},
	})
	if err != nil {
		final, wrapped := refreshError(err, 1)
		if final {
			// Remembered without a count of tries, since those who meet it
			// later sent nothing.
			a.mu.Lock()
			a.refused, a.refusedErr = t.RefreshToken, wrapped
			a.mu.Unlock()
			return "", wrapped
		}
		if se, ok := errors.AsType[*Error](err); ok && n > 1 {
			se.Tries = n // before the error is wrapped, which writes its message
		}
		_, wrapped = refreshError(err, n)
		return "", wrapped
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

// refreshTransient reports whether err, from a refresh, is worth trying
// again: the token request failed on the network, or Schwab answered 429 or
// 5xx. A refusal of the login or the app is final, and so is a problem with
// the token file.
func refreshTransient(ctx context.Context, err error) bool {
	if errors.Is(err, ErrLoginExpired) || errors.Is(err, ErrAppRejected) {
		return false
	}
	if se, ok := errors.AsType[*Error](err); ok {
		return se.Status == http.StatusTooManyRequests || se.Status >= 500 && se.Status != http.StatusNotImplemented
	}
	if _, ok := errors.AsType[*url.Error](err); ok || errors.Is(err, errReading) {
		return retryable(ctx, err)
	}
	return false
}

// backoff returns the wait before refresh try+1, longer with each try and
// jittered.
func (a *Auth) backoff(try int) time.Duration {
	return a.jitter(refreshWait << (try - 1))
}

// refreshError reports whether Schwab refused the refresh token for good, so
// that it is not sent again while the file holds it, and says what a failed
// refresh means.
//
// Schwab refuses a lapsed or replaced refresh token with HTTP 400 and
// unsupported_token_type, nesting the real reason, invalid_grant or
// refresh_token_authentication_error; older reports also show
// invalid_client with "refresh token invalid". A wrong app key or secret
// gets invalid_client and "Unauthorized". The codes decide, whatever the
// status. Another 400 is taken as a refused login too, but only for this
// try: it may be a gateway's. n is how many token requests the renewal has
// made, this one included, which a failure other than Schwab's answer says.
func refreshError(err error, n int) (final bool, _ error) {
	se, ok := errors.AsType[*Error](err)
	switch {
	case !ok:
		if n > 1 {
			return false, fmt.Errorf("refreshing the access token after %d tries: %w", n, err)
		}
	case loginCodes[se.Code], loginCodes[se.Detail],
		(se.Status == http.StatusBadRequest || se.Status == http.StatusUnauthorized) && strings.Contains(strings.ToLower(se.Body), "refresh token"):
		return true, fmt.Errorf("%w: %w", ErrLoginExpired, err)
	case se.Code == "invalid_client", se.Status == http.StatusUnauthorized:
		return false, fmt.Errorf("%w: %w", ErrAppRejected, err)
	case se.Status == http.StatusBadRequest:
		return false, fmt.Errorf("%w: %w", ErrLoginExpired, err)
	}
	return false, fmt.Errorf("refreshing the access token: %w", err)
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
	if err := ctx.Err(); err != nil {
		return nil, err // rather than take the semaphore half the time
	}
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
