package schwab

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	cl.sleep = sleep
	cl.firstWait = time.Hour
	cl.maxWait = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
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

// A try that runs out the HTTP client's time is tried once more, not three
// times; one that runs out the caller's deadline is not tried again.
func TestGetTimeouts(t *testing.T) {
	hang := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	f := newFakeOAuth(t)
	f.script = append(f.script, hang, hang, hang)
	ft := &fakeTime{now: testNow}
	cl := newTestClient(t, f, &clock{now: testNow}, ft)
	cl.auth.cfg.HTTPClient = &http.Client{Timeout: 100 * time.Millisecond}
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err == nil || !strings.Contains(err.Error(), "GET /trader/v1/test after 2 tries") || !strings.Contains(err.Error(), "Client.Timeout") {
		t.Errorf("after two timeouts: %v", err)
	}
	if f.calls() != 2 || len(ft.got()) != 1 {
		t.Errorf("%d requests and waits %v, want 2 and one", f.calls(), ft.got())
	}

	f = newFakeOAuth(t)
	f.script = append(f.script, hang, hang)
	cl = newTestClient(t, f, &clock{now: testNow}, ft)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := cl.Get(ctx, "/trader/v1/test", nil); !errors.Is(err, context.DeadlineExceeded) || f.calls() != 1 {
		t.Errorf("past the caller's deadline: %v after %d requests, want context.DeadlineExceeded after 1", err, f.calls())
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
// refused before any request.
func TestGetRefusesPathsThatLeadElsewhere(t *testing.T) {
	f := newFakeOAuth(t)
	cl := newTestClient(t, f, &clock{now: testNow}, &fakeTime{now: testNow})
	for _, path := range []string{
		"/trader/v1/accounts/..", "/trader/v1/accounts/%2e%2E/x", "/trader/v1/./test",
		"/trader/v1/test?x=1", "/trader/v1/test#x", "trader/v1/test", "/trader/v1/%zz",
	} {
		if _, err := cl.Get(t.Context(), path, nil); err == nil {
			t.Errorf("%q: no error", path)
		}
	}
	if f.calls() != 0 {
		t.Errorf("%d requests, want none", f.calls())
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
		_, _, err := other.renewOnce(ctx, "")
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
	// The next call knows the refusal, and makes no request.
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, ErrLoginExpired) || f.tokenRequests() != 1 {
		t.Errorf("AccessToken again: %v after %d token requests, want ErrLoginExpired after 1", err, f.tokenRequests())
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
