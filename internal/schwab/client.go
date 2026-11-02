package schwab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// maxBody bounds the size of a response that is read.
const maxBody = 64 << 20

// Client calls Schwab's API with a login. It is safe for concurrent use.
type Client struct {
	auth *Auth

	// tries is how many times a read is sent before its failure is returned,
	// and firstWait and maxWait bound the waits between them. sleep waits,
	// and jitter picks a wait up to the one given; tests replace them.
	tries              int
	firstWait, maxWait time.Duration
	sleep              func(ctx context.Context, d time.Duration) error
	jitter             func(d time.Duration) time.Duration
}

// NewClient returns a client that uses a's login, base URL, HTTP client and
// clock.
func NewClient(a *Auth) *Client {
	return &Client{
		auth:      a,
		tries:     4,
		firstWait: time.Second,
		maxWait:   time.Minute,
		sleep:     sleep,
		jitter:    equalJitter,
	}
}

// equalJitter returns a wait between half of d and d, at random, so that
// processes that failed together do not try again together.
func equalJitter(d time.Duration) time.Duration {
	return d/2 + rand.N(d/2+1) //nolint:gosec // G404: a wait needs no secret randomness.
}

// Get fetches path, such as "/trader/v1/accounts", with query, and returns
// the body of the answer.
//
// A read is safe to send again, so Get tries again, up to four times in
// all, after a 429 or 5xx answer or a failure of the network, waiting what
// Schwab asks in Retry-After or else longer each time. After a 401 it gets a
// new access token once. Other answers come back as an *Error.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := c.auth.cfg.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	refreshed := false
	for try := 1; ; try++ {
		tok, err := c.auth.AccessToken(ctx)
		if err != nil {
			return nil, err
		}
		r, err := c.send(ctx, u, tok)
		if err != nil {
			if try >= c.tries || !transient(ctx, err) {
				return nil, fmt.Errorf("GET %s: %w", path, err)
			}
			if err := c.sleep(ctx, c.backoff(try)); err != nil {
				return nil, err
			}
			continue
		}
		switch status := r.status; {
		case status == http.StatusOK:
			return r.body, nil
		case status == http.StatusUnauthorized && !refreshed:
			refreshed = true
			_, err := c.auth.Refresh(ctx, tok)
			if errors.Is(err, ErrTokenRefused) {
				break // a new token would not help, so report Schwab's answer
			}
			if err != nil {
				return nil, err
			}
			try-- // the first answer was the token's fault, not Schwab's
			continue
		case (status == http.StatusTooManyRequests || status >= 500 && status != http.StatusNotImplemented) && try < c.tries:
			wait := c.backoff(try)
			if after, ok := retryAfter(r.header.Get("Retry-After"), c.auth.now()); ok {
				wait = min(after, c.maxWait)
			}
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < wait {
				break // there is no time left to wait, so report Schwab's answer
			}
			if err := c.sleep(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}
		return nil, newError(r.status, r.header, r.body)
	}
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
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// backoff returns the wait before try+1: firstWait, doubled with each try,
// at most maxWait, and jittered so that processes that failed together do not
// try again together.
func (c *Client) backoff(try int) time.Duration {
	wait := c.firstWait << (try - 1)
	if wait <= 0 || wait > c.maxWait {
		wait = c.maxWait
	}
	return c.jitter(wait)
}

// retryAfter reads a Retry-After header: whole seconds, or an HTTP date.
func retryAfter(v string, now time.Time) (time.Duration, bool) {
	if v == "" {
		return 0, false
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(max(s, 0)) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(t.Sub(now), 0), true
	}
	return 0, false
}

// transient reports whether err, from sending a read, was the network's
// failure, worth trying again: not the caller's cancel or deadline, and not
// a certificate or other error that the next try would repeat.
func transient(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	// Every error from http.Client.Do is a *url.Error, which itself has the
	// methods of a net.Error, so look inside it.
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true // the connection closed before the whole answer came
	}
	_, ok := errors.AsType[net.Error](err)
	return ok
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
