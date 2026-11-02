package schwab

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxBody bounds the size of an answer that is read.
const maxBody = 64 << 20

var (
	// errTooLong is the error for an answer larger than maxBody.
	errTooLong = fmt.Errorf("the answer was over %d MB", maxBody>>20)
	// errReading marks a failure to read an answer, such as a connection
	// cut part way through it.
	errReading = errors.New("reading the answer")
)

// ClientConfig tunes a Client. The zero value is the default.
type ClientConfig struct {
	// RatePerMinute is how many requests a minute every process on this
	// computer that uses the app key may make together. Zero means
	// DefaultRatePerMinute.
	RatePerMinute int
	// Burst is how many of them may go at once after a quiet spell. Zero
	// means DefaultBurst.
	Burst int
	// RateFile is where the processes keep the rate limit they share. Empty
	// means a file named for the app key in the user cache directory,
	// schwabtx/rate-<hash>.
	RateFile string
	// Tries is how many times a read is sent before its failure is returned.
	// Zero means 4.
	Tries int
	// Backoff is the wait after a read's first failure, doubled after each
	// later one, up to a minute. Zero means a second.
	Backoff time.Duration
}

// Client calls Schwab's API with a login. It is safe for concurrent use.
type Client struct {
	auth  *Auth
	limit *limiter
	// interval and tolerance are the rate limit's: a request each interval,
	// and a burst of 1 + tolerance/interval.
	interval, tolerance time.Duration
	// tries is how many times a read is sent before its failure is returned,
	// and firstWait and maxWait bound the waits between them.
	tries              int
	firstWait, maxWait time.Duration

	// now tells the time for the rate limit and Retry-After, by the wall
	// clock, whatever the Auth's clock says; sleep waits, and jitter picks a
	// wait up to the one given. Tests replace them.
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error
	jitter func(d time.Duration) time.Duration
}

// NewClient returns a client that uses a's login, base URL and HTTP client.
// Its rate limit is shared with every process on this computer that uses
// a's app key and the same rate file, and it tells the time for it by the
// system clock, not a's.
func NewClient(a *Auth, cfg ClientConfig) *Client {
	rate := cfg.RatePerMinute
	if rate <= 0 {
		rate = DefaultRatePerMinute
	}
	burst := cfg.Burst
	if burst <= 0 {
		burst = DefaultBurst
	}
	rateFile := cfg.RateFile
	if rateFile == "" {
		rateFile = defaultRateFile(a.cfg.AppKey, a.tokenFile)
	}
	if resolved, err := resolve(rateFile); err == nil {
		rateFile = resolved
	}
	interval := time.Minute / time.Duration(rate)
	c := &Client{
		auth:      a,
		limit:     limiterFor(rateFile),
		interval:  interval,
		tolerance: time.Duration(burst-1) * interval,
		tries:     4,
		firstWait: time.Second,
		maxWait:   time.Minute,
		now:       time.Now,
		sleep:     sleep,
		jitter:    equalJitter,
	}
	if cfg.Tries > 0 {
		c.tries = cfg.Tries
	}
	if cfg.Backoff > 0 {
		c.firstWait = min(cfg.Backoff, c.maxWait)
	}
	return c
}

// equalJitter returns a wait between half of d and d, at random, so that
// processes that failed together do not try again together.
func equalJitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1) //nolint:gosec // G404: a wait needs no secret randomness.
}

// Get fetches path, such as "/trader/v1/accounts", with query, and returns
// the body of the answer.
//
// Each try waits its turn under the rate limit. A read is safe to send
// again, so Get tries again, up to four times in all, after a 429 or 5xx
// answer or a failure of the network, waiting longer each time: 1, 2, then 4
// seconds, jittered, or what Schwab asks in Retry-After. A 429 holds back
// every process that shares the rate limit for that long, not only this
// one. A try that times out is tried once more, not three times. After a
// 401 Get gets a new access token once. A wait that the caller's deadline
// would cut short is not begun. Other answers come back as an *Error.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	if err := checkPath(path); err != nil {
		return nil, err
	}
	u := c.auth.cfg.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	refreshed := false
	timeouts := 0
	for try := 1; ; try++ {
		tok, err := c.admit(ctx)
		if err != nil {
			return nil, err
		}
		r, err := c.send(ctx, u, tok)
		if err != nil {
			if timedOut(err) {
				timeouts++
			}
			final := try >= c.tries || timeouts > 1 || !retryable(ctx, err)
			if try > 1 {
				err = fmt.Errorf("GET %s after %d tries: %w", path, try, err)
			} else {
				err = fmt.Errorf("GET %s: %w", path, err)
			}
			if final {
				return nil, err
			}
			// A connection that stopped answering stays in the pool, so drop
			// the idle ones before the next try.
			c.auth.cfg.HTTPClient.CloseIdleConnections()
			if err := c.wait(ctx, c.backoff(try), err); err != nil {
				return nil, err
			}
			continue
		}
		if r.status == http.StatusOK {
			return r.body, nil
		}
		if r.status == http.StatusUnauthorized && !refreshed {
			refreshed = true
			_, err := c.auth.Refresh(ctx, tok)
			if err == nil {
				try-- // the first answer was the token's fault, not Schwab's
				continue
			}
			if !errors.Is(err, ErrTokenRefused) {
				return nil, err
			}
			// A new token would not help, so report Schwab's answer.
		}
		e := newError(r.status, r.header, r.body)
		e.Tries = try
		switch {
		case r.status == http.StatusTooManyRequests:
			asked, ok := retryAfter(r.header, c.now())
			if !ok {
				asked = c.step(try)
			}
			// Hold back every process that uses the app key, then let this
			// request's next try wait its turn in the limit like any other.
			// Trying again sooner than Schwab asks would only bring another
			// 429, so a longer wait than maxWait ends the tries.
			d := min(max(asked, c.firstWait), maxPause)
			retry := try < c.tries && asked <= c.maxWait && fits(ctx, d)
			if err := c.pause(ctx, d); err != nil || !retry {
				return nil, e
			}
		case retryableStatus(e) && try < c.tries:
			if err := c.wait(ctx, c.backoff(try), e); err != nil {
				return nil, err
			}
		default:
			return nil, e
		}
	}
}

// admit returns an access token for a request once the rate limit lets the
// request go, so that it can go at once: nothing that can block comes
// between the two.
func (c *Client) admit(ctx context.Context) (string, error) {
	if _, err := c.auth.AccessToken(ctx); err != nil {
		return "", err
	}
	if err := c.take(ctx); err != nil {
		return "", err
	}
	if fresh, ok := c.auth.fresh(""); ok {
		return fresh, nil
	}
	// The wait outlived the token, as when the computer slept through it.
	return c.auth.AccessToken(ctx)
}

// take waits until the rate limit lets a request go, and takes its slot.
// It does not begin a wait that ctx's deadline would cut short.
func (c *Client) take(ctx context.Context) error {
	_, err := c.takeAt(ctx)
	return err
}

// takeAt is take, and returns the time by which the request was let go.
func (c *Client) takeAt(ctx context.Context) (time.Time, error) {
	for {
		var wait time.Duration
		var at time.Time
		// The state is saved even when the request must wait, since settle
		// may have moved the TAT for a clock that was set back.
		err := c.limit.change(ctx, c.now, c.auth.warn, func(s rateState, now time.Time) (rateState, bool) {
			s, wait = s.settle(now, c.interval, c.tolerance).admit(now, c.interval, c.tolerance)
			at = now
			return s, true
		})
		if err != nil {
			return time.Time{}, err
		}
		if wait == 0 {
			return at, nil
		}
		if !fits(ctx, wait) {
			return time.Time{}, &limitWait{wait: wait}
		}
		if err := c.sleep(ctx, wait); err != nil {
			return time.Time{}, err
		}
	}
}

// pause holds back every request that shares the rate limit for d.
func (c *Client) pause(ctx context.Context, d time.Duration) error {
	return c.limit.change(ctx, c.now, c.auth.warn, func(s rateState, now time.Time) (rateState, bool) {
		return s.settle(now, c.interval, c.tolerance).pause(now, d, c.tolerance), true
	})
}

// wait waits d before the next try after a failure, last. If ctx's deadline
// would cut the wait short it returns last at once, and if ctx ends during
// the wait it returns ctx's error with last.
func (c *Client) wait(ctx context.Context, d time.Duration, last error) error {
	if !fits(ctx, d) {
		return last
	}
	if err := c.sleep(ctx, d); err != nil {
		return fmt.Errorf("%w while waiting to try again; the last try got: %w", err, last)
	}
	return nil
}

// limitWait is the error for a request that the rate limit would let go
// only after the caller's deadline.
type limitWait struct{ wait time.Duration }

func (e *limitWait) Error() string {
	return fmt.Sprintf("the rate limit for Schwab's API lets the next request go in %v, after the deadline", e.wait.Round(time.Millisecond))
}

func (e *limitWait) Is(target error) bool { return target == ErrRateLimited }

// fits reports whether a wait of d ends before ctx's deadline.
func fits(ctx context.Context, d time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= d
}

// reply is an answer from Schwab, read in full.
type reply struct {
	status int
	header http.Header
	body   []byte
}

// send makes one GET request with tok, and reads the whole answer.
func (c *Client) send(ctx context.Context, u, tok string) (*reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.auth.cfg.HTTPClient.Do(req)
	if err != nil {
		// Its message would repeat the method and give the whole address.
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errReading, err)
	}
	if len(body) > maxBody {
		return nil, errTooLong
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// step returns the wait after try failed, before jitter: firstWait, doubled
// with each try, at most maxWait.
func (c *Client) step(try int) time.Duration {
	wait := c.firstWait << (try - 1)
	if wait <= 0 || wait > c.maxWait {
		wait = c.maxWait
	}
	return wait
}

// backoff returns the wait after try failed, jittered so that processes that
// failed together do not try again together.
func (c *Client) backoff(try int) time.Duration {
	return c.jitter(c.step(try))
}

// retryAfter reads a response's Retry-After header: whole seconds, or an
// HTTP date, which is measured against the response's Date header when it
// has one, since the two clocks may not agree.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(min(max(s, 0), 1<<20)) * time.Second, true
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0, false
	}
	if date, err := http.ParseTime(h.Get("Date")); err == nil {
		now = date
	}
	return max(t.Sub(now), 0), true
}

// retryableStatus reports whether an answer with e's status may go away if
// the read is sent again: 5xx, except 501 and the gateway's refusal of an
// answer too big to pass on, which come back the same each time.
func retryableStatus(e *Error) bool {
	return e.Status >= 500 && e.Status != http.StatusNotImplemented && !errors.Is(e, ErrResponseTooBig)
}

// retryable reports whether err, from sending a read or reading its answer,
// may go away if the read is sent again. That is every failure of the
// network, whatever the system calls it, but not the caller's cancel or
// deadline, an answer too big to read, or a certificate or protocol error,
// which the next try would repeat.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	_, cert := errors.AsType[*tls.CertificateVerificationError](err)
	_, unknown := errors.AsType[x509.UnknownAuthorityError](err)
	_, host := errors.AsType[x509.HostnameError](err)
	_, invalid := errors.AsType[x509.CertificateInvalidError](err)
	_, record := errors.AsType[tls.RecordHeaderError](err)
	return !cert && !unknown && !host && !invalid && !record &&
		!errors.Is(err, http.ErrSchemeMismatch) && !errors.Is(err, errTooLong)
}

// timedOut reports whether err is a try that ran out of time, by the HTTP
// client's own limit rather than the caller's.
func timedOut(err error) bool {
	ne, ok := errors.AsType[net.Error](err)
	return ok && ne.Timeout()
}

// checkPath refuses an API path that could reach another endpoint than the
// one its caller built: one with a query, a fragment, or a "." or ".."
// segment, which a gateway may resolve, even when escaped.
func checkPath(path string) error {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return fmt.Errorf("API path %q: want an absolute path without a query", path)
	}
	for seg := range strings.SplitSeq(path, "/") {
		if s, err := url.PathUnescape(seg); err != nil || s == "." || s == ".." {
			return fmt.Errorf("API path %q: a segment is not a name", path)
		}
	}
	return nil
}

// sleep waits for d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
