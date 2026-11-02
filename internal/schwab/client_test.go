package schwab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

// waits records the waits that a login asks for, without waiting.
type waits struct {
	mu sync.Mutex
	d  []time.Duration
}

func (w *waits) sleep(ctx context.Context, d time.Duration) error {
	w.mu.Lock()
	w.d = append(w.d, d)
	w.mu.Unlock()
	return ctx.Err()
}

func (w *waits) got() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.d)
}

// newTestClient logs in to f and returns a client whose waits run on ft,
// with no jitter, and whose rate limit is the test's own.
func newTestClient(t *testing.T, f *fakeOAuth, c *clock, ft *fakeTime) *Client {
	t.Helper()
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if f.srv.TLS != nil {
		a.cfg.HTTPClient = f.srv.Client()
	}
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	cl := NewClient(a, ClientConfig{RateFile: filepath.Join(t.TempDir(), "rate")})
	cl.now = ft.Now
	cl.sleep = ft.Sleep
	cl.jitter = func(d time.Duration) time.Duration { return d }
	return cl
}

func TestGetTriesAgainAfterAFailure(t *testing.T) {
	f := newFakeOAuth(t)
	// Go's transport sends a read again by itself, once, when a connection
	// it reused is cut, so the client sees a cut only when that happens
	// twice.
	f.script = []func(http.ResponseWriter, *http.Request){
		answer(http.StatusServiceUnavailable, `{"message":"down"}`),
		answer(http.StatusTooManyRequests, `{"message":"slow down"}`, "Retry-After", "7"),
		func(w http.ResponseWriter, _ *http.Request) { cut(w) },
		func(w http.ResponseWriter, _ *http.Request) { cut(w) },
	}
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	body, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("Get: %q, %v", body, err)
	}
	if f.calls() != 5 {
		t.Errorf("%d requests, want 5", f.calls())
	}
	if want := []time.Duration{time.Second, 7 * time.Second, 4 * time.Second}; !slices.Equal(ft.got(), want) {
		t.Errorf("waited %v, want %v: backoff, Retry-After, backoff", ft.got(), want)
	}
}

func TestGetGivesUp(t *testing.T) {
	f := newFakeOAuth(t)
	for range 4 {
		f.script = append(f.script, answer(http.StatusServiceUnavailable, `{"message":"down","errors":["try later"]}`,
			"Schwab-Client-CorrelId", "corr-123"))
	}
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	se, ok := errors.AsType[*Error](err)
	if !ok || se.Status != http.StatusServiceUnavailable || se.CorrelID != "corr-123" || se.Tries != 4 ||
		err.Error() != "HTTP 503 from Schwab after 4 tries: down; try later (correlation ID corr-123)" {
		t.Errorf("after 4 failures: %v", err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; f.calls() != 4 || !slices.Equal(ft.got(), want) {
		t.Errorf("%d requests and waits %v, want 4 and %v", f.calls(), ft.got(), want)
	}
}

// Answers that would come back the same are not tried again: 4xx, 501, and
// the gateway's refusal of an answer too big to pass on.
func TestGetDoesNotTryAgainAfterAnswersThatWouldRepeat(t *testing.T) {
	tooBig := `{"fault":{"faultstring":"Body buffer overflow","detail":{"errorcode":"protocol.http.TooBigBody"}}}`
	for _, tc := range []struct {
		status int
		body   string
	}{
		{http.StatusBadRequest, `{"message":"no"}`},
		{http.StatusForbidden, `{"message":"no"}`},
		{http.StatusNotFound, `{"message":"no"}`},
		{http.StatusNotImplemented, `{"message":"no"}`},
		{http.StatusBadGateway, tooBig},
	} {
		f := newFakeOAuth(t)
		f.script = append(f.script, answer(tc.status, tc.body))
		ft := &fakeTime{now: testNow}
		cl := newTestClient(t, f, &clock{now: testNow}, ft)
		if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err == nil {
			t.Errorf("%d: no error", tc.status)
		} else if tc.body == tooBig && !errors.Is(err, ErrResponseTooBig) {
			t.Errorf("%d: %v, want ErrResponseTooBig", tc.status, err)
		}
		if f.calls() != 1 || len(ft.got()) != 0 {
			t.Errorf("%d: %d requests and waits %v, want 1 and none", tc.status, f.calls(), ft.got())
		}
	}
}

// A 429 holds back the next try for what Retry-After asks, or else for the
// backoff, and for at least the first wait. One that asks for longer than
// the client waits ends the tries at once, but still holds back the next
// request.
func TestGetRetryAfter(t *testing.T) {
	date := testNow.Add(-time.Hour) // Schwab's clock, not this one's
	for _, tc := range []struct {
		headers []string
		want    time.Duration
	}{
		{[]string{"Retry-After", "9"}, 9 * time.Second},
		{[]string{"Retry-After", date.Add(12 * time.Second).Format(http.TimeFormat), "Date", date.Format(http.TimeFormat)}, 12 * time.Second},
		{[]string{"Retry-After", "0"}, time.Second},
		{[]string{"Retry-After", "soon"}, time.Second}, // not a time, so the backoff
		{nil, time.Second},
	} {
		f := newFakeOAuth(t)
		f.script = append(f.script, answer(http.StatusTooManyRequests, "", tc.headers...))
		ft := &fakeTime{now: testNow}
		cl := newTestClient(t, f, &clock{now: testNow}, ft)
		if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
			t.Fatal(err)
		}
		if got := ft.got(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%q: waited %v, want %v", tc.headers, got, tc.want)
		}
	}

	f := newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusTooManyRequests, "", "Retry-After", "3600"))
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusTooManyRequests || se.RetryAfter != time.Hour ||
		!errors.Is(err, ErrRateLimited) || f.calls() != 1 || len(ft.got()) != 0 {
		t.Errorf("Retry-After 3600: %v after %d requests and waits %v, want the 429 at once", err, f.calls(), ft.got())
	}
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Fatal(err)
	}
	if got := ft.got(); len(got) != 1 || got[0] != maxPause {
		t.Errorf("the next request waited %v, want %v", got, maxPause)
	}
}

// Schwab's own 429 carries no Retry-After, so the backoff decides; a 429
// that keeps coming is reported as one.
func TestGetSchwabsTooManyRequests(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, tooMany, tooMany, tooMany, tooMany)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	cl.jitter = func(d time.Duration) time.Duration { return d / 2 } // which a 429's wait does not take
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if !errors.Is(err, ErrRateLimited) || err.Error() != "HTTP 429 from Schwab after 4 tries (correlation ID corr-429)" {
		t.Errorf("after four 429s: %v", err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !slices.Equal(ft.got(), want) {
		t.Errorf("waited %v, want %v", ft.got(), want)
	}
}

// A 429 to one client holds back another on the same rate file: after the
// pause they go one each interval, without a burst.
func TestGetA429HoldsBackOthers(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, tooMany)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	other := NewClient(cl.auth, ClientConfig{RateFile: cl.limit.path})
	other.now, other.sleep = ft.Now, ft.Sleep
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{time.Second, 600 * time.Millisecond}; !slices.Equal(ft.got(), want) {
		t.Errorf("waited %v, want %v: the pause, then one interval for the other client", ft.got(), want)
	}
}

// A wait that the caller's deadline would cut short is not begun: not for a
// Retry-After, and not for the rate limit.
func TestGetDoesNotWaitPastTheDeadline(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusTooManyRequests, "", "Retry-After", "30"))
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := cl.Get(ctx, "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusTooManyRequests {
		t.Errorf("with 5 seconds left and 30 to wait: %v, want Schwab's 429", err)
	}
	_, err = cl.Get(ctx, "/trader/v1/test", nil)
	if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "after the deadline") {
		t.Errorf("the next request, with 30 seconds of the pause left: %v, want ErrRateLimited", err)
	}
	if len(ft.got()) != 0 || f.calls() != 1 {
		t.Errorf("%d requests and waits %v, want 1 and none", f.calls(), ft.got())
	}
}

func TestGetStopsWaitingWhenCancelled(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusServiceUnavailable, ""))
	cl := newTestClient(t, f, &clock{now: testNow}, &fakeTime{now: testNow})
	ctx, cancel := context.WithCancel(t.Context())
	cl.sleep = func(ctx context.Context, d time.Duration) error {
		time.AfterFunc(50*time.Millisecond, cancel) // once the wait has begun
		return sleep(ctx, d)
	}
	cl.firstWait = time.Hour
	cl.maxWait = time.Hour
	start := time.Now()
	_, err := cl.Get(ctx, "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !errors.Is(err, context.Canceled) || !ok || se.Status != http.StatusServiceUnavailable {
		t.Errorf("Get: %v, want context.Canceled and the 503", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Get took %v after the cancel", d)
	}
}

// A failure that the next try would repeat, such as a certificate that is
// not trusted, is not tried again.
func TestGetDoesNotTryAgainAfterACertificateError(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0) // the handshake that fails, as it should
	srv.StartTLS()
	t.Cleanup(srv.Close)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "a", RefreshToken: "r", AccessExpiresAt: testNow.Add(time.Hour), RefreshObtainedAt: testNow})
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: srv.URL, TokenFile: tokenFile, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTime{now: testNow}
	cl := NewClient(a, ClientConfig{RateFile: filepath.Join(t.TempDir(), "rate")})
	cl.now, cl.sleep = ft.Now, ft.Sleep
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("Get from a server with an untrusted certificate: %v", err)
	}
	if len(ft.got()) != 0 {
		t.Errorf("waited %v to try again", ft.got())
	}
}

// counting is a transport that counts the requests sent through it, as the
// client sees them, whether or not the server ever gets them.
type counting struct {
	n     atomic.Int32
	inner http.RoundTripper
}

func (c *counting) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.inner.RoundTrip(req)
}

// A try that runs out the HTTP client's time is tried once more, not three
// times; one that runs out the caller's deadline is not tried again.
func TestGetTimeouts(t *testing.T) {
	hang := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	f := newFakeOAuth(t)
	f.script = append(f.script, hang, hang, hang)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	sent := &counting{inner: http.DefaultTransport}
	cl.auth.cfg.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond, Transport: sent}
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err == nil || !strings.Contains(err.Error(), "GET /trader/v1/test after 2 tries") || !timedOut(err) {
		t.Errorf("after two timeouts: %v", err)
	}
	if n := sent.n.Load(); n != 2 || len(ft.got()) != 1 {
		t.Errorf("%d requests and waits %v, want 2 and one", n, ft.got())
	}

	f = newFakeOAuth(t)
	f.script = append(f.script, hang, hang)
	cl = newTestClient(t, f, &clock{now: testNow}, ft)
	sent = &counting{inner: http.DefaultTransport}
	cl.auth.cfg.HTTPClient = &http.Client{Transport: sent}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := cl.Get(ctx, "/trader/v1/test", nil); !errors.Is(err, context.DeadlineExceeded) || sent.n.Load() != 1 {
		t.Errorf("past the caller's deadline: %v after %d requests, want context.DeadlineExceeded after 1", err, sent.n.Load())
	}
}

// Over HTTP/2, a stream that the server resets is tried again: Go does not
// call that a network error.
func TestGetHTTP2StreamReset(t *testing.T) {
	f := newFakeOAuthH2(t)
	f.script = append(f.script, func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	body, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("Get: %q, %v", body, err)
	}
	if f.calls() != 2 || len(ft.got()) != 1 {
		t.Errorf("%d requests and waits %v, want 2 and one", f.calls(), ft.got())
	}
}

// A path that could reach another endpoint than the one its caller built is
// refused before any request, and so is one that is not a URL path.
func TestGetRefusesPathsThatLeadElsewhere(t *testing.T) {
	f := newFakeOAuth(t)
	cl := newTestClient(t, f, &clock{now: testNow}, &fakeTime{now: testNow})
	before := f.requestsOfAnyKind()
	for _, path := range []string{
		"/trader/v1/accounts/..", "/trader/v1/accounts/%2e%2E/x", "/trader/v1/./test",
		"/marketdata/v1/..%2F..%2Ftrader%2Fv1%2Faccounts/quotes", "/marketdata/v1/..%5C..%5Cx/quotes", `/marketdata/v1/..\x`,
		"/trader/v1/test?x=1", "/trader/v1/test#x", "trader/v1/test", "@127.0.0.1:1/x",
		"/trader/v1/%zz", "/trader/v1/test\n", "/trader/v1/..;/accounts", "/trader/v1/%2e%2e;x/accounts",
	} {
		if _, err := cl.Get(t.Context(), path, nil); err == nil || !strings.HasPrefix(err.Error(), "API path ") {
			t.Errorf("%q: %v, want it refused", path, err)
		}
	}
	if n := f.requestsOfAnyKind() - before; n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// Paths that Schwab's endpoints take are let through.
func TestCheckPathTakesSchwabsPaths(t *testing.T) {
	for _, path := range []string{
		"/trader/v1/accounts/0123456789ABCDEF0123456789ABCDEF/transactions",
		"/trader/v1/accounts/0123456789ABCDEF/transactions/98765432101",
		"/marketdata/v1/%24SPX/quotes", "/marketdata/v1/$SPX/quotes", "/marketdata/v1/BRK.B/quotes",
		"/marketdata/v1/BRK%2FB/quotes", "/marketdata/v1/%2FESZ24/quotes", "/marketdata/v1/instruments/037833100",
		"/marketdata/v1/AAPL%20%20261218C00200000/quotes", "/marketdata/v1/.hidden",
	} {
		if err := checkPath(path); err != nil {
			t.Errorf("%q: %v", path, err)
		}
	}
}

// A base URL that would send the token elsewhere, or is not one, is refused.
func TestNewAuthChecksTheBaseURL(t *testing.T) {
	for _, base := range []string{
		"api.schwabapi.com", "ftp://api.schwabapi.com", "https://", "https://user@api.schwabapi.com",
		"https://api.schwabapi.com/?x=1", "https://api.schwabapi.com/#", "https://api.schwabapi.com/%zz",
	} {
		if _, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: base, TokenFile: filepath.Join(t.TempDir(), "t.json")}); err == nil {
			t.Errorf("%q: no error", base)
		}
	}
	for _, base := range []string{"https://api.schwabapi.com", "http://127.0.0.1:8080", "https://example.com/prefix/"} {
		if _, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: base, TokenFile: filepath.Join(t.TempDir(), "t.json")}); err != nil {
			t.Errorf("%q: %v", base, err)
		}
	}
}

// A wait that outlives the access token, as when the computer sleeps through
// it, gets a new token before the request goes.
func TestGetTokenOutlivesTheWait(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, c, ft)
	if err := cl.pause(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	ft.onSleep = func(time.Duration) { c.advance(40 * time.Minute) }
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	sent := f.lastToken
	f.mu.Unlock()
	if _, refresh := f.grants(); sent != "access-2" || refresh != 1 {
		t.Errorf("sent %s after %d refreshes, want access-2 after 1", sent, refresh)
	}
}

// A refresh that fails for a while is tried again.
func TestRefreshTriesAgain(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	var w waits
	a.sleep = w.sleep
	a.jitter = func(d time.Duration) time.Duration { return d }
	c.advance(time.Hour)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable, 0, http.StatusTooManyRequests}
	f.mu.Unlock()
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Fatalf("AccessToken: %q, %v; want access-2 after three failures", tok, err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; !slices.Equal(w.got(), want) {
		t.Errorf("waited %v, want %v", w.got(), want)
	}
}

// A refresh waits what the token endpoint's Retry-After asks, when that is
// longer than its own wait.
func TestRefreshRetryAfter(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	var w waits
	a.sleep = w.sleep
	a.jitter = func(d time.Duration) time.Duration { return d }
	c.advance(time.Hour)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.tokenRetryAfter = "3"
	f.mu.Unlock()
	if _, err := a.AccessToken(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{3 * time.Second}; !slices.Equal(w.got(), want) {
		t.Errorf("waited %v, want %v", w.got(), want)
	}

	// One that asks for longer than a 429 may hold everyone back ends the
	// tries.
	c.advance(time.Hour)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.tokenRetryAfter = "3600"
	f.mu.Unlock()
	before := f.tokenRequests()
	if _, err := a.AccessToken(t.Context()); err == nil || f.tokenRequests()-before != 1 || len(w.got()) != 1 {
		t.Errorf("with Retry-After 3600: %v after %d token requests and waits %v, want the 503 after 1 and no wait",
			err, f.tokenRequests()-before, w.got())
	}
}

// While a refresh waits to try again it does not hold the lock, so another
// process can refresh meanwhile, and the next try takes that one's token.
func TestRefreshWaitsWithoutTheLock(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuth(t, f, tokenFile, c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusBadGateway}
	f.mu.Unlock()
	other := newTestAuth(t, f, tokenFile, c)
	// The other process does not wait for the lock: it fails if the first
	// one still holds the lock file, or, within a few seconds, the semaphore
	// that they share.
	other.lock = func(_ context.Context, path string, _ time.Duration) (*filelock.Lock, error) {
		l, ok, err := filelock.TryAcquire(path)
		if err == nil && !ok {
			return nil, errors.New("the first process still holds the lock")
		}
		return l, err
	}
	a.sleep = func(ctx context.Context, _ time.Duration) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, _, err := other.renewOnce(ctx, "", 1)
		return err
	}
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Fatalf("AccessToken: %q, %v; want the other process's access-2", tok, err)
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want only the other process's", refresh)
	}
}

// A refusal is final: it is not tried again. Once the lock is free the
// token file is looked at once more, in case a login was waiting to save.
func TestRefreshRefusalIsNotTriedAgain(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "old", RefreshToken: "lapsed", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow.Add(-8 * 24 * time.Hour)})
	a := newTestAuth(t, f, tokenFile, c)
	var w waits
	a.sleep = w.sleep
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("AccessToken: %v, want ErrLoginExpired", err)
	}
	if want := []time.Duration{recheckWait}; f.tokenRequests() != 1 || !slices.Equal(w.got(), want) {
		t.Errorf("%d token requests and waits %v, want 1 and %v", f.tokenRequests(), w.got(), want)
	}
	// The next call knows the refusal, makes no request, and does not wait.
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, ErrLoginExpired) || f.tokenRequests() != 1 || len(w.got()) != 1 {
		t.Errorf("AccessToken again: %v after %d token requests and waits %v, want ErrLoginExpired after 1 and no more waits",
			err, f.tokenRequests(), w.got())
	}
}

// A login that saves while a refresh with the old refresh token is refused,
// as when the login waited for the lock, is used: the caller gets its token,
// not a refusal.
func TestRefreshRefusalFindsANewLogin(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "old", RefreshToken: "ended", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow.Add(-time.Hour)})
	a := newTestAuth(t, f, tokenFile, c)
	a.sleep = func(ctx context.Context, _ time.Duration) error {
		writeTestToken(t, tokenFile, token{AccessToken: "new", RefreshToken: "fresh", AccessObtainedAt: testNow, AccessExpiresAt: testNow.Add(30 * time.Minute), RefreshObtainedAt: testNow})
		return ctx.Err()
	}
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "new" {
		t.Errorf("AccessToken: %q, %v; want the new login's token", tok, err)
	}
}

// A 400 from the token endpoint that does not say the login is over is not
// remembered as a refusal: the look once more tries the refresh again.
func TestRefreshUnknown400IsNotRemembered(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	var w waits
	a.sleep = w.sleep
	c.advance(time.Hour)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusBadRequest}
	f.mu.Unlock()
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Errorf("AccessToken: %q, %v; want access-2", tok, err)
	}
}

// A refresh does not begin for a caller that has gone, since a token request
// runs to its end whatever the caller does.
func TestRefreshDoesNotBeginForAGoneCaller(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	before := f.tokenRequests()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.AccessToken(ctx); !errors.Is(err, context.Canceled) || f.tokenRequests() != before {
		t.Errorf("AccessToken after the cancel: %v after %d token requests, want context.Canceled and none", err, f.tokenRequests()-before)
	}
}

// Goroutines that need a refresh at once share one: while the token
// endpoint fails, there is one series of tries, not one for each, and each
// goroutine gets its outcome.
func TestRefreshesAreShared(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	var w waits
	a.sleep = w.sleep
	c.advance(time.Hour)
	f.mu.Lock()
	for range 40 {
		f.tokenFailures = append(f.tokenFailures, http.StatusServiceUnavailable)
	}
	f.delay = 100 * time.Millisecond
	f.mu.Unlock()
	before := f.tokenRequests()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() { _, errs[i] = a.AccessToken(t.Context()) })
	}
	wg.Wait()
	for i, err := range errs {
		if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusServiceUnavailable {
			t.Errorf("goroutine %d: %v, want the 503", i, err)
		}
	}
	if n := f.tokenRequests() - before; n != refreshTries {
		t.Errorf("%d token requests, want %d: one series of tries for all", n, refreshTries)
	}
}

// When the goroutine making a refresh that others wait for gives up, they do
// not take its giving up for their answer: one of them refreshes.
func TestRefreshOutlivesTheGoroutineThatBeganIt(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.mu.Unlock()
	first, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The first goroutine's wait to try again ends with its cancel, once
	// another goroutine waits for its refresh.
	a.sleep = func(ctx context.Context, d time.Duration) error {
		if ctx != first {
			return sleep(ctx, d)
		}
		for followers := 0; followers == 0; {
			time.Sleep(time.Millisecond)
			a.mu.Lock()
			if fl := a.flights[""]; fl != nil {
				followers = fl.followers
			}
			a.mu.Unlock()
		}
		cancel()
		return ctx.Err()
	}
	a.jitter = func(time.Duration) time.Duration { return time.Millisecond }
	done := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(first)
		done <- err
	}()
	for {
		a.mu.Lock()
		started := a.flights[""] != nil
		a.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Errorf("the goroutine that waited: %q, %v; want access-2", tok, err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the goroutine that gave up: %v, want context.Canceled", err)
	}
}

// A 401 that a new access token answers does not count as a try, and a 401
// after the new token is reported, with no second refresh.
func TestGet401(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, c, ft)
	c.advance(2 * freshTokenAge) // so that Schwab refusing the token calls for a new one
	f.script = append(f.script, answer(http.StatusUnauthorized, `{"message":"expired"}`))
	for range 4 {
		f.script = append(f.script, answer(http.StatusServiceUnavailable, `{"message":"down"}`))
	}
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusServiceUnavailable || se.Tries != 4 || f.calls() != 5 {
		t.Errorf("a 401, then four 503s: %v after %d requests, want the 503 after 4 tries and 5 requests", err, f.calls())
	}

	// The second 401 comes when the new token is old enough to be refreshed
	// again, which Get must not do.
	f = newFakeOAuth(t)
	c = &clock{now: testNow}
	cl = newTestClient(t, f, c, ft)
	c.advance(2 * freshTokenAge)
	f.script = append(f.script,
		answer(http.StatusUnauthorized, `{"message":"expired"}`),
		func(w http.ResponseWriter, r *http.Request) {
			c.advance(2 * freshTokenAge)
			answer(http.StatusUnauthorized, `{"message":"expired"}`)(w, r)
		})
	_, err = cl.Get(t.Context(), "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusUnauthorized || f.calls() != 2 {
		t.Errorf("a 401 after a new token: %v after %d requests, want the 401 after 2", err, f.calls())
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refreshes, want 1", refresh)
	}
}

// An answer too big to read is refused, and not tried again.
func TestGetAnswerTooBig(t *testing.T) {
	defer func(n int) { maxBody = n }(maxBody)
	maxBody = 1 << 20
	f := newFakeOAuth(t)
	f.script = append(f.script, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, strings.Repeat("x", maxBody+1))
	})
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if !errors.Is(err, errTooLong) || f.calls() != 1 || len(ft.got()) != 0 {
		t.Errorf("an answer over %d bytes: %v after %d requests and waits %v, want errTooLong after 1 and none", maxBody, err, f.calls(), ft.got())
	}
}

// An answer cut short part way through is tried again.
func TestGetAnswerCutShort(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"partial":`)
		w.(http.Flusher).Flush()
		cut(w)
	})
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	body, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err != nil || string(body) != `{"ok":true}` || f.calls() != 2 || len(ft.got()) != 1 {
		t.Errorf("Get: %q, %v after %d requests and waits %v; want the second answer", body, err, f.calls(), ft.got())
	}
}

// An https base URL at a server that answers plain HTTP fails at once.
func TestGetSchemeMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "a", RefreshToken: "r", AccessExpiresAt: testNow.Add(time.Hour), RefreshObtainedAt: testNow})
	c := &clock{now: testNow}
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: "https://" + srv.Listener.Addr().String(), TokenFile: tokenFile, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTime{now: testNow}
	cl := NewClient(a, ClientConfig{RateFile: filepath.Join(t.TempDir(), "rate")})
	cl.now, cl.sleep = ft.Now, ft.Sleep
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err == nil || len(ft.got()) != 0 {
		t.Errorf("Get over https from an http server: %v, waits %v; want an error at once", err, ft.got())
	}
}

// A 5xx's Retry-After is waited for when it is longer than the backoff, and
// one longer than a minute ends the tries; a backoff that the deadline would
// cut short is not begun.
func TestGet5xxWaits(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusServiceUnavailable, "", "Retry-After", "9"))
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{9 * time.Second}; !slices.Equal(ft.got(), want) {
		t.Errorf("a 503 with Retry-After 9: waited %v, want %v", ft.got(), want)
	}

	f = newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusServiceUnavailable, "", "Retry-After", "3600"))
	ft = &fakeTime{now: testNow}
	cl = newTestClient(t, f, &clock{now: testNow}, ft)
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err == nil || f.calls() != 1 || len(ft.got()) != 0 {
		t.Errorf("a 503 with Retry-After 3600: %v after %d requests and waits %v, want the 503 at once", err, f.calls(), ft.got())
	}

	f = newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusServiceUnavailable, ""))
	ft = &fakeTime{now: testNow}
	cl = newTestClient(t, f, &clock{now: testNow}, ft)
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	_, err := cl.Get(ctx, "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusServiceUnavailable || f.calls() != 1 || len(ft.got()) != 0 {
		t.Errorf("a 503 with half a second left: %v after %d requests and waits %v, want the 503 at once", err, f.calls(), ft.got())
	}
}

// When the wait for a later try ends without it, Get still says what the
// last try got, with its correlation ID.
func TestGetKeepsTheLastAnswer(t *testing.T) {
	// A cancel during a 429's wait, which is in the rate limit.
	f := newFakeOAuth(t)
	f.script = append(f.script, tooMany)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	ctx, cancel := context.WithCancel(t.Context())
	ft.onSleep = func(time.Duration) { cancel() }
	_, err := cl.Get(ctx, "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !errors.Is(err, context.Canceled) || !ok || se.CorrelID != "corr-429" {
		t.Errorf("a cancel during a 429's wait: %v, want context.Canceled and the 429", err)
	}

	// A 503, and while its backoff runs another process's 429 pauses
	// everyone for longer than the deadline leaves: the 503 is the answer,
	// not the rate limit.
	f = newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusServiceUnavailable, `{"message":"down"}`, "Schwab-Client-CorrelId", "corr-503"))
	ft = &fakeTime{now: testNow}
	cl = newTestClient(t, f, &clock{now: testNow}, ft)
	ft.onSleep = func(time.Duration) {
		ft.onSleep = nil
		if err := cl.pause(t.Context(), 30*time.Second); err != nil {
			t.Error(err)
		}
	}
	ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = cl.Get(ctx, "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusServiceUnavailable || se.CorrelID != "corr-503" || errors.Is(err, ErrRateLimited) {
		t.Errorf("a 503, then a pause past the deadline: %v, want the 503", err)
	}
}

// The default HTTP client checks a quiet HTTP/2 connection with a ping.
func TestDefaultTransportChecksQuietConnections(t *testing.T) {
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, TokenFile: filepath.Join(t.TempDir(), "t.json")})
	if err != nil {
		t.Fatal(err)
	}
	tr, ok := a.cfg.HTTPClient.Transport.(*http.Transport)
	if !ok || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout != 15*time.Second || tr.HTTP2.PingTimeout != 10*time.Second || a.cfg.HTTPClient.Timeout != 30*time.Second {
		t.Errorf("the default client: %+v", a.cfg.HTTPClient)
	}
	// Cloning gives Go's default transport an empty HTTP/2 config of its own
	// since Go 1.27; it must not be the clone's, or have the clone's pings.
	if def := http.DefaultTransport.(*http.Transport).HTTP2; def != nil && (def == tr.HTTP2 || def.SendPingTimeout != 0 || def.PingTimeout != 0) {
		t.Errorf("Go's default transport was changed: %+v", def)
	}
}

// stallProxy passes TCP connections on to a server until told to stall,
// after which the connections it has stop passing anything, as when the
// network changes under them; new ones still pass.
type stallProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	stall  chan struct{} // closed to stall the connections made so far
	dials  int
}

func newStallProxy(t *testing.T, target string) *stallProxy {
	ln, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &stallProxy{ln: ln, target: target, stall: make(chan struct{})}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			out, err := new(net.Dialer).DialContext(t.Context(), "tcp", target)
			if err != nil {
				in.Close()
				continue
			}
			p.mu.Lock()
			p.dials++
			stall := p.stall
			p.mu.Unlock()
			t.Cleanup(func() { in.Close(); out.Close() })
			pass := func(dst, src net.Conn) {
				buf := make([]byte, 32<<10)
				for {
					n, err := src.Read(buf)
					select {
					case <-stall:
						return // what comes now goes nowhere
					default:
					}
					if n > 0 {
						if _, err := dst.Write(buf[:n]); err != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}
			go pass(out, in)
			go pass(in, out)
		}
	}()
	return p
}

func (p *stallProxy) stallNow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.stall)
	p.stall = make(chan struct{})
}

func (p *stallProxy) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dials
}

// An HTTP/2 connection that stops answering is not used for the next try.
func TestGetDropsAConnectionThatStoppedAnswering(t *testing.T) {
	f := newFakeOAuthH2(t)
	proxy := newStallProxy(t, f.srv.Listener.Addr().String())
	c := &clock{now: testNow}
	a, err := NewAuth(Config{
		AppKey: testKey, AppSecret: testSecret, BaseURL: "https://" + proxy.ln.Addr().String(),
		TokenFile: filepath.Join(t.TempDir(), "token.json"), Now: c.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	client := f.srv.Client()
	client.Timeout = 30 * time.Second
	a.cfg.HTTPClient = client
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	cl := NewClient(a, ClientConfig{RateFile: filepath.Join(t.TempDir(), "rate"), Backoff: 20 * time.Millisecond})
	cl.jitter = func(d time.Duration) time.Duration { return d }
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Fatal(err)
	}
	client.Timeout = 2 * time.Second // for the try that the stall cuts off
	proxy.stallNow()
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Errorf("Get after the connection stopped answering: %v", err)
	}
	if n := proxy.connections(); n != 2 {
		t.Errorf("%d connections, want 2: the one that stopped and a new one", n)
	}
}

// followers returns how many goroutines wait for a's renewal of a token
// that has not been refused.
func followers(a *Auth) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if fl := a.flights[""]; fl != nil {
		return fl.followers
	}
	return 0
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// loggedInAuth returns an Auth logged in to f whose access token is stale.
func loggedInAuth(t *testing.T, f *fakeOAuth) *Auth {
	t.Helper()
	c := &clock{now: testNow}
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	return a
}

// A goroutine that waits for another's renewal stops waiting when its own
// caller gives up.
func TestRefreshFollowerGivesUp(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.mu.Unlock()
	release, sleeping := make(chan struct{}), make(chan struct{})
	a.sleep = func(ctx context.Context, _ time.Duration) error {
		close(sleeping)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	leader := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(t.Context())
		leader <- err
	}()
	<-sleeping
	ctx, cancel := context.WithCancel(t.Context())
	follower := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(ctx)
		follower <- err
	}()
	waitFor(t, "the follower", func() bool { return followers(a) == 1 })
	cancel()
	select {
	case err := <-follower:
		if se, ok := errors.AsType[*Error](err); !errors.Is(err, context.Canceled) || !ok || se.Status != http.StatusServiceUnavailable {
			t.Errorf("the follower: %v, want context.Canceled and the last try's 503", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the follower kept waiting after its cancel")
	}
	close(release)
	if err := <-leader; err != nil {
		t.Errorf("the leader: %v", err)
	}
}

// A goroutine that stops its tries because its own deadline leaves no time
// for the next one does not give its failure to one with more time, which
// tries for itself.
func TestRefreshLeaderOutOfTime(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	a.jitter = func(d time.Duration) time.Duration { return d }
	var once sync.Once
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.onToken = func() {
		once.Do(func() { waitFor(t, "the follower", func() bool { return followers(a) == 1 }) })
	}
	f.mu.Unlock()
	short, cancel := context.WithTimeout(t.Context(), 900*time.Millisecond) // shorter than the first wait, a second
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(short)
		leader <- err
	}()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Errorf("the goroutine with more time: %q, %v; want access-2", tok, err)
	}
	if se, ok := errors.AsType[*Error](<-leader); !ok || se.Status != http.StatusServiceUnavailable {
		t.Error("the goroutine out of time did not get the 503")
	}
}

// panicky is a transport whose first request waits for a condition and
// panics.
type panicky struct {
	ready func() bool
	once  sync.Once
}

func (p *panicky) RoundTrip(req *http.Request) (*http.Response, error) {
	first := false
	p.once.Do(func() { first = true })
	if first {
		for !p.ready() {
			time.Sleep(time.Millisecond)
		}
		panic("the transport broke")
	}
	return http.DefaultTransport.RoundTrip(req)
}

// A renewal that panics does not leave those waiting for it waiting forever:
// one of them renews.
func TestRefreshLeaderPanics(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	a.cfg.HTTPClient = &http.Client{Transport: &panicky{ready: func() bool { return followers(a) == 1 }}}
	leader := make(chan any, 1)
	go func() {
		defer func() { leader <- recover() }()
		_, _ = a.AccessToken(t.Context())
	}()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if tok, err := a.AccessToken(ctx); err != nil || tok != "access-2" {
		t.Errorf("the goroutine that waited: %q, %v; want access-2", tok, err)
	}
	if p := <-leader; p == nil {
		t.Error("the transport did not panic")
	}
}

// A token request that times out is tried once more, not three times, since
// each try holds the lock until it times out.
func TestRefreshTimeoutTriedOnce(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	var w waits
	a.sleep = w.sleep
	sent := &counting{inner: http.DefaultTransport}
	a.cfg.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond, Transport: sent}
	f.mu.Lock()
	f.delay = 300 * time.Millisecond
	f.mu.Unlock()
	if _, err := a.AccessToken(t.Context()); !timedOut(err) {
		t.Errorf("AccessToken: %v, want a timeout", err)
	}
	if n := sent.n.Load(); n != 2 {
		t.Errorf("%d token requests, want 2", n)
	}
}

// A refresh that fails each time says how many times it was sent.
func TestRefreshTriesCounted(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	var w waits
	a.sleep = w.sleep
	f.mu.Lock()
	f.tokenFailures = []int{503, 503, 503, 503}
	f.mu.Unlock()
	_, err := a.AccessToken(t.Context())
	if se, ok := errors.AsType[*Error](err); !ok || se.Tries != refreshTries || !strings.Contains(err.Error(), fmt.Sprintf("after %d tries", refreshTries)) {
		t.Errorf("after %d failures: %v", refreshTries, err)
	}
}

// A refresh after Schwab refused a token does not take the outcome of a
// renewal that may hand back that same token.
func TestRefreshAfterA401DoesNotJoinAccessToken(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * freshTokenAge)
	// Another view of the login, with nothing read yet, whose first look at
	// the lock waits until it is let go.
	b := newTestAuth(t, f, tokenFile, c)
	entered, letGo := make(chan struct{}), make(chan struct{})
	firstLock := true
	b.lock = func(ctx context.Context, path string, wait time.Duration) (*filelock.Lock, error) {
		if firstLock {
			firstLock = false
			close(entered)
			<-letGo
		}
		return filelock.Acquire(ctx, path, wait)
	}
	reader := make(chan string, 1)
	go func() {
		tok, _ := b.AccessToken(t.Context())
		reader <- tok
	}()
	<-entered
	refreshed := make(chan string, 1)
	go func() {
		tok, err := b.Refresh(t.Context(), "access-1")
		if err != nil {
			t.Error(err)
		}
		refreshed <- tok
	}()
	time.Sleep(50 * time.Millisecond) // for the Refresh to wait, if it wrongly would, for the renewal
	close(letGo)
	if tok := <-refreshed; tok != "access-2" {
		t.Errorf("Refresh after a 401 for access-1 gave %q, want access-2", tok)
	}
	if tok := <-reader; tok != "access-1" {
		t.Errorf("AccessToken gave %q, want access-1", tok)
	}
}

// After a 401 whose refresh fails, the error has both why and Schwab's 401,
// with its correlation ID.
func TestGet401RefreshFails(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	cl := newTestClient(t, f, c, &fakeTime{now: testNow})
	var w waits
	cl.auth.sleep = w.sleep
	c.advance(2 * freshTokenAge)
	f.script = append(f.script, answer(http.StatusUnauthorized, `{"message":"expired"}`, "Schwab-Client-CorrelId", "corr-401"))
	f.mu.Lock()
	f.tokenFailures = []int{503, 503, 503, 503}
	f.mu.Unlock()
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 503 from Schwab after 4 tries") || !strings.Contains(err.Error(), "corr-401") {
		t.Errorf("a 401 and a refresh that fails: %v, want both", err)
	}
}

// A read that fails on the network each time is sent four times; and when
// the wait for a later try ends without it, the last failure is kept.
func TestGetNetworkFailures(t *testing.T) {
	reset := func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }
	f := newFakeOAuthH2(t)
	f.script = append(f.script, reset, reset, reset, reset, reset)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err == nil || !strings.Contains(err.Error(), "after 4 tries") || f.calls() != 4 {
		t.Errorf("five resets: %v after %d requests, want the fourth's error", err, f.calls())
	}

	f = newFakeOAuthH2(t)
	f.script = append(f.script, reset)
	ft = &fakeTime{now: testNow}
	cl = newTestClient(t, f, &clock{now: testNow}, ft)
	ft.onSleep = func(time.Duration) {
		ft.onSleep = nil
		if err := cl.pause(t.Context(), 30*time.Second); err != nil {
			t.Error(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = cl.Get(ctx, "/trader/v1/test", nil)
	if err == nil || errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "GET /trader/v1/test: ") {
		t.Errorf("a reset, then a pause past the deadline: %v, want the reset", err)
	}
}

// A 429 while another process keeps the rate file's lock still holds this
// process back, in its own limit, and the read is tried again.
func TestGetPauseWhileTheLockIsKept(t *testing.T) {
	f := newFakeOAuth(t)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	cl.limit.wait = 200 * time.Millisecond
	var warnings []string
	cl.auth.cfg.Warn = func(msg string) { warnings = append(warnings, msg) }
	f.script = append(f.script, func(w http.ResponseWriter, r *http.Request) {
		l, ok, err := filelock.TryAcquire(cl.limit.path)
		if err != nil || !ok {
			t.Errorf("taking the rate file's lock: %v, %v", ok, err)
		} else {
			t.Cleanup(l.Release)
		}
		tooMany(w, r)
	})
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
		t.Errorf("Get: %v", err)
	}
	if want := []time.Duration{time.Second}; f.calls() != 2 || !slices.Equal(ft.got(), want) || len(warnings) != 1 {
		t.Errorf("%d requests, waits %v and warnings %q; want 2, %v and one", f.calls(), ft.got(), warnings, want)
	}
}

// Goroutines that share a deadline too short for the next try get the
// failure of the one that made the first, rather than each make their own.
func TestRefreshFollowersShareADeadline(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	a.jitter = func(d time.Duration) time.Duration { return d }
	sent := &counting{inner: http.DefaultTransport}
	a.cfg.HTTPClient = &http.Client{Transport: sent}
	const goroutines = 8
	var once sync.Once
	f.mu.Lock()
	for range 20 {
		f.tokenFailures = append(f.tokenFailures, http.StatusServiceUnavailable)
	}
	f.onToken = func() {
		once.Do(func() { waitFor(t, "the others", func() bool { return followers(a) == goroutines-1 }) })
	}
	f.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 900*time.Millisecond) // shorter than the first wait, a second
	defer cancel()
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := range errs {
		wg.Go(func() { _, errs[i] = a.AccessToken(ctx) })
	}
	wg.Wait()
	for i, err := range errs {
		if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusServiceUnavailable {
			t.Errorf("goroutine %d: %v, want the 503", i, err)
		}
	}
	if n := sent.n.Load(); n != 1 {
		t.Errorf("%d token requests, want 1", n)
	}
}

// One that takes over after another gave up for lack of time waits what
// that one was due to, here a Retry-After, and carries on its tries.
func TestRefreshCarriesOnTheTries(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	var w waits
	a.sleep = w.sleep
	a.jitter = func(d time.Duration) time.Duration { return d }
	sent := &counting{inner: http.DefaultTransport}
	a.cfg.HTTPClient = &http.Client{Transport: sent}
	var once sync.Once
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.tokenRetryAfter = "5"
	f.onToken = func() {
		once.Do(func() { waitFor(t, "the follower", func() bool { return followers(a) == 1 }) })
	}
	f.mu.Unlock()
	short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(short)
		leader <- err
	}()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Errorf("the one with time: %q, %v; want access-2", tok, err)
	}
	if got := w.got(); len(got) != 1 || got[0] < 4*time.Second || got[0] > 5*time.Second {
		t.Errorf("waited %v, want what was left of Retry-After's 5s", got)
	}
	if n := sent.n.Load(); n != 2 {
		t.Errorf("%d token requests, want 2", n)
	}
	if err := <-leader; err == nil {
		t.Error("the one out of time got no error")
	}
}

// A final answer that comes after the deadline of the goroutine that asked
// for it is still the answer for those waiting with it.
func TestRefreshFinalAnswerAfterTheDeadline(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	var w waits
	a.sleep = w.sleep
	a.jitter = func(time.Duration) time.Duration { return time.Millisecond } // a wait that fits the deadline
	sent := &counting{inner: http.DefaultTransport}
	a.cfg.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond, Transport: sent}
	var once sync.Once
	f.mu.Lock()
	f.delay = 300 * time.Millisecond // each token request times out
	f.onToken = func() {
		once.Do(func() { waitFor(t, "the follower", func() bool { return followers(a) == 1 }) })
	}
	f.mu.Unlock()
	short, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond) // ends during the second request
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(short)
		leader <- err
	}()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	if _, err := a.AccessToken(t.Context()); !timedOut(err) {
		t.Errorf("the follower: %v, want the second timeout", err)
	}
	if err := <-leader; !timedOut(err) {
		t.Errorf("the leader: %v, want the second timeout", err)
	}
	if n := sent.n.Load(); n != 2 {
		t.Errorf("%d token requests, want 2", n)
	}
}

// A refresh whose next try cannot get the lock before the deadline keeps
// what the last try got.
func TestRefreshKeepsTheLastFailure(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.mu.Unlock()
	a.jitter = func(time.Duration) time.Duration { return time.Millisecond } // a wait that fits the deadline
	a.sleep = func(ctx context.Context, _ time.Duration) error {
		// Another process takes the lock while this one waits to try again.
		l, ok, err := filelock.TryAcquire(a.tokenFile + ".lock")
		if err != nil || !ok {
			t.Errorf("taking the lock: %v, %v", ok, err)
		} else {
			t.Cleanup(l.Release)
		}
		return ctx.Err()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := a.AccessToken(ctx)
	if se, ok := errors.AsType[*Error](err); !errors.Is(err, context.DeadlineExceeded) || !ok || se.Status != http.StatusServiceUnavailable {
		t.Errorf("AccessToken: %v, want the deadline and the 503", err)
	}
}

// A refusal is remembered without a count of tries, since those who meet it
// later send nothing, and a refresh that fails on the network each time
// says how many times it was sent.
func TestRefreshCounts(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "old", RefreshToken: "lapsed", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow.Add(-8 * 24 * time.Hour)})
	a := newTestAuth(t, f, tokenFile, c)
	var w waits
	a.sleep = w.sleep
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.mu.Unlock()
	for range 2 {
		if _, err := a.AccessToken(t.Context()); !errors.Is(err, ErrLoginExpired) || strings.Contains(err.Error(), "after") {
			t.Errorf("a refusal after a 503: %v, want ErrLoginExpired without a count", err)
		}
	}

	f = newFakeOAuth(t)
	a = loggedInAuth(t, f)
	a.sleep = w.sleep
	f.mu.Lock()
	f.tokenFailures = []int{0, 0, 0, 0}
	f.mu.Unlock()
	if _, err := a.AccessToken(t.Context()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("after %d tries", refreshTries)) {
		t.Errorf("four cut connections: %v, want the count", err)
	}
}

// When the deadline of the goroutine renewing passes during a token request
// that fails, but not for good, those waiting with it carry on.
func TestRefreshLeaderGoneDuringItsRequest(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	var w waits
	a.sleep = w.sleep
	a.cfg.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond}
	var first atomic.Bool
	first.Store(true)
	f.mu.Lock()
	f.onToken = func() {
		if first.CompareAndSwap(true, false) {
			waitFor(t, "the follower", func() bool { return followers(a) == 1 })
			time.Sleep(300 * time.Millisecond) // past the client's timeout
		}
	}
	f.mu.Unlock()
	short, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(short)
		leader <- err
	}()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Errorf("the follower: %q, %v; want access-2", tok, err)
	}
	if err := <-leader; err == nil {
		t.Error("the leader got no error")
	}
}

// When the goroutine renewing has no time left for the look at the token
// file after a refusal, one that waits with it makes the look, after the
// same wait, and so finds a login saved meanwhile.
func TestRefreshRecheckHandedOver(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "old", RefreshToken: "ended", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow.Add(-time.Hour)})
	a := newTestAuth(t, f, tokenFile, c)
	var once sync.Once
	f.mu.Lock()
	f.delay = 200 * time.Millisecond // past the first goroutine's deadline
	f.onToken = func() {
		once.Do(func() {
			waitFor(t, "the follower", func() bool { return followers(a) == 1 })
			// A login waiting for the lock saves soon after the refusal.
			time.AfterFunc(400*time.Millisecond, func() {
				writeTestToken(t, tokenFile, token{AccessToken: "new", RefreshToken: "fresh", AccessObtainedAt: testNow, AccessExpiresAt: testNow.Add(30 * time.Minute), RefreshObtainedAt: testNow})
			})
		})
	}
	f.mu.Unlock()
	short, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	leader := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(short)
		leader <- err
	}()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "new" {
		t.Errorf("the goroutine with time: %q, %v; want the new login's token", tok, err)
	}
	if err := <-leader; !errors.Is(err, ErrLoginExpired) {
		t.Errorf("the goroutine out of time: %v, want the refusal", err)
	}
}

// A goroutine that asks for a renewal while another waits to carry on one
// that ended without an answer waits with it, rather than sends at once.
func TestRefreshNewcomerJoinsTheCarriedWait(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	sent := &counting{inner: http.DefaultTransport}
	a.cfg.HTTPClient = &http.Client{Transport: sent}
	var once sync.Once
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.tokenRetryAfter = "2"
	f.onToken = func() {
		once.Do(func() { waitFor(t, "the follower", func() bool { return followers(a) == 1 }) })
	}
	f.mu.Unlock()
	newcomer := make(chan string, 1)
	a.sleep = func(ctx context.Context, _ time.Duration) error {
		// The carried wait: a newcomer arrives during it, and must join.
		go func() {
			tok, err := a.AccessToken(t.Context())
			if err != nil {
				t.Errorf("the newcomer: %v", err)
			}
			newcomer <- tok
		}()
		for i := 0; followers(a) == 0 && i < 5000; i++ {
			time.Sleep(time.Millisecond)
		}
		return ctx.Err()
	}
	short, cancel := context.WithTimeout(t.Context(), time.Second) // shorter than Retry-After's 2s
	defer cancel()
	go func() { _, _ = a.AccessToken(short) }()
	waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Errorf("the one that carried on: %q, %v; want access-2", tok, err)
	}
	if tok := <-newcomer; tok != "access-2" {
		t.Errorf("the newcomer got %q, want access-2", tok)
	}
	if n := sent.n.Load(); n != 2 {
		t.Errorf("%d token requests, want 2", n)
	}
}

// A goroutine waiting for a renewal whose next wait it has no time for gets
// the last failure at once, and never the renewing goroutine's own cancel,
// whether it joined before that wait began or during it.
func TestRefreshFollowerGetsTheLastFailure(t *testing.T) {
	for _, joinDuring := range []bool{false, true} {
		f := newFakeOAuth(t)
		a := loggedInAuth(t, f)
		leaderCtx, cancelLeader := context.WithCancel(t.Context())
		follower := make(chan error, 1)
		startFollower := func() {
			go func() {
				ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond) // shorter than Retry-After's 5s
				defer cancel()
				_, err := a.AccessToken(ctx)
				follower <- err
			}()
		}
		var once sync.Once
		f.mu.Lock()
		f.tokenFailures = []int{http.StatusServiceUnavailable}
		f.tokenRetryAfter = "5"
		if !joinDuring {
			f.onToken = func() {
				once.Do(func() { waitFor(t, "the follower", func() bool { return followers(a) == 1 }) })
			}
		}
		f.mu.Unlock()
		var err error
		a.sleep = func(ctx context.Context, _ time.Duration) error {
			if joinDuring {
				// The follower joins while this wait goes on, and must not
				// wait for it.
				startFollower()
				select {
				case err = <-follower:
				case <-time.After(10 * time.Second):
				}
			}
			cancelLeader() // its caller gives up during the wait
			return ctx.Err()
		}
		leaderDone := make(chan struct{})
		go func() {
			defer close(leaderDone)
			_, _ = a.AccessToken(leaderCtx)
		}()
		if joinDuring {
			<-leaderDone
		} else {
			waitFor(t, "the renewal", func() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.flights[""] != nil })
			startFollower()
			err = <-follower
		}
		if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusServiceUnavailable ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("joined during the wait %v: the follower got %v, want the 503 at once, without a cancel", joinDuring, err)
		}
	}
}

// A caller that gives up during a token request that fails for a while gets
// its cancel, with the failure, as when it gives up during the wait after.
func TestRefreshCancelDuringAFailingRequest(t *testing.T) {
	f := newFakeOAuth(t)
	a := loggedInAuth(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	f.mu.Lock()
	f.tokenFailures = []int{http.StatusServiceUnavailable}
	f.onToken = cancel
	f.mu.Unlock()
	_, err := a.AccessToken(ctx)
	if se, ok := errors.AsType[*Error](err); !errors.Is(err, context.Canceled) || !ok || se.Status != http.StatusServiceUnavailable {
		t.Errorf("AccessToken: %v, want context.Canceled and the 503", err)
	}
}
