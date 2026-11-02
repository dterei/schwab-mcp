package schwab

import (
	"context"
	"errors"
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

// waits records the waits that a client or a login asks for, without
// waiting.
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

// newTestClient logs in to f and returns a client whose waits are recorded
// in w, with no jitter.
func newTestClient(t *testing.T, f *fakeOAuth, c *clock, w *waits) *Client {
	t.Helper()
	a := newTestAuth(t, f, filepath.Join(t.TempDir(), "token.json"), c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	cl := NewClient(a)
	cl.sleep = w.sleep
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
	var w waits
	cl := newTestClient(t, f, &clock{now: testNow}, &w)
	body, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	if err != nil || string(body) != `{"ok":true}` {
		t.Fatalf("Get: %q, %v", body, err)
	}
	if f.calls() != 5 {
		t.Errorf("%d requests, want 5", f.calls())
	}
	if want := []time.Duration{time.Second, 7 * time.Second, 4 * time.Second}; !slices.Equal(w.got(), want) {
		t.Errorf("waited %v, want %v: backoff, Retry-After, backoff", w.got(), want)
	}
}

func TestGetGivesUp(t *testing.T) {
	f := newFakeOAuth(t)
	for range 4 {
		f.script = append(f.script, answer(http.StatusServiceUnavailable, `{"message":"down","errors":["try later"]}`,
			"Schwab-Client-CorrelId", "corr-123"))
	}
	var w waits
	cl := newTestClient(t, f, &clock{now: testNow}, &w)
	_, err := cl.Get(t.Context(), "/trader/v1/test", nil)
	se, ok := errors.AsType[*Error](err)
	if !ok || se.Status != http.StatusServiceUnavailable || se.CorrelID != "corr-123" ||
		err.Error() != "HTTP 503 from Schwab: down; try later (correlation ID corr-123)" {
		t.Errorf("after 4 failures: %v", err)
	}
	if f.calls() != 4 || len(w.got()) != 3 {
		t.Errorf("%d requests and %d waits, want 4 and 3", f.calls(), len(w.got()))
	}
}

func TestGetDoesNotTryAgainAfterClientErrors(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented} {
		f := newFakeOAuth(t)
		f.script = append(f.script, answer(status, `{"message":"no"}`))
		var w waits
		cl := newTestClient(t, f, &clock{now: testNow}, &w)
		if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err == nil {
			t.Errorf("%d: no error", status)
		}
		if f.calls() != 1 || len(w.got()) != 0 {
			t.Errorf("%d: %d requests and waits %v, want 1 and none", status, f.calls(), w.got())
		}
	}
}

func TestGetRetryAfter(t *testing.T) {
	c := &clock{now: testNow}
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{"9", 9 * time.Second},
		{testNow.Add(12 * time.Second).Format(http.TimeFormat), 12 * time.Second},
		{"3600", time.Minute}, // at most maxWait
		{"0", 0},
		{"soon", time.Second}, // not a time, so the backoff
	} {
		f := newFakeOAuth(t)
		f.script = append(f.script, answer(http.StatusTooManyRequests, "", "Retry-After", tc.header))
		var w waits
		cl := newTestClient(t, f, c, &w)
		if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err != nil {
			t.Fatal(err)
		}
		if got := w.got(); len(got) != 1 || got[0] != tc.want {
			t.Errorf("Retry-After %q: waited %v, want %v", tc.header, got, tc.want)
		}
	}
}

// A wait that the caller's deadline would cut short is not begun.
func TestGetDoesNotWaitPastTheDeadline(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusTooManyRequests, "", "Retry-After", "30"))
	var w waits
	cl := newTestClient(t, f, &clock{now: testNow}, &w)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := cl.Get(ctx, "/trader/v1/test", nil)
	if se, ok := errors.AsType[*Error](err); !ok || se.Status != http.StatusTooManyRequests {
		t.Errorf("with 5 seconds left and 30 to wait: %v, want Schwab's 429", err)
	}
	if len(w.got()) != 0 {
		t.Errorf("waited %v", w.got())
	}
}

func TestGetStopsWaitingWhenCancelled(t *testing.T) {
	f := newFakeOAuth(t)
	f.script = append(f.script, answer(http.StatusServiceUnavailable, ""))
	var w waits
	cl := newTestClient(t, f, &clock{now: testNow}, &w)
	cl.sleep = sleep
	cl.firstWait = time.Hour
	cl.maxWait = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	if _, err := cl.Get(ctx, "/trader/v1/test", nil); !errors.Is(err, context.Canceled) {
		t.Errorf("Get: %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Get took %v after the cancel", d)
	}
}

// A failure that the next try would repeat, such as a certificate that is
// not trusted, is not tried again.
func TestGetDoesNotTryAgainAfterACertificateError(t *testing.T) {
	calls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	t.Cleanup(srv.Close)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "a", RefreshToken: "r", AccessExpiresAt: testNow.Add(time.Hour), RefreshObtainedAt: testNow})
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: srv.URL, TokenFile: tokenFile, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	var w waits
	cl := NewClient(a)
	cl.sleep = w.sleep
	if _, err := cl.Get(t.Context(), "/trader/v1/test", nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("Get from a server with an untrusted certificate: %v", err)
	}
	if len(w.got()) != 0 {
		t.Errorf("waited %v to try again", w.got())
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
		_, err := other.renewOnce(ctx, "")
		return err
	}
	if tok, err := a.AccessToken(t.Context()); err != nil || tok != "access-2" {
		t.Fatalf("AccessToken: %q, %v; want the other process's access-2", tok, err)
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want only the other process's", refresh)
	}
}

// A refusal is final: it is not tried again.
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
	if f.tokenRequests() != 1 || len(w.got()) != 0 {
		t.Errorf("%d token requests and waits %v, want 1 and none", f.tokenRequests(), w.got())
	}
}
