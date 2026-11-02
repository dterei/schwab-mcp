package schwab

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

// fakeTime is a wall clock that moves only when something sleeps on it: each
// sleep moves it on by the wait, which it records.
type fakeTime struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	// onSleep, if set, runs at each sleep, as the computer sleeping through
	// the wait would.
	onSleep func(d time.Duration)
}

func (f *fakeTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeTime) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.waits = append(f.waits, d)
	hook := f.onSleep
	f.mu.Unlock()
	if hook != nil {
		hook(d)
	}
	if err := ctx.Err(); err != nil {
		return err // cut short
	}
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
	return nil
}

func (f *fakeTime) set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = t
}

func (f *fakeTime) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.waits)
}

// newRateClient returns a client, without a login, whose rate limit is kept
// in rateFile and runs on ft.
func newRateClient(t *testing.T, rateFile string, cfg ClientConfig, ft *fakeTime) *Client {
	t.Helper()
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, TokenFile: filepath.Join(t.TempDir(), "token.json")})
	if err != nil {
		t.Fatal(err)
	}
	cfg.RateFile = rateFile
	c := NewClient(a, cfg)
	if ft != nil {
		c.now = ft.Now
		c.sleep = ft.Sleep
	}
	return c
}

// takeTimes takes n slots from c and returns when each was let go, after
// start.
func takeTimes(t *testing.T, c *Client, n int, start time.Time) []time.Duration {
	t.Helper()
	var times []time.Duration
	for range n {
		at, err := c.takeAt(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		times = append(times, at.Sub(start))
	}
	return times
}

// After a quiet spell a burst goes at once, then one request each interval.
func TestRateBurstThenSteady(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	got := takeTimes(t, c, 8, testNow)
	want := []time.Duration{0, 0, 0, 0, 0, 600 * time.Millisecond, 1200 * time.Millisecond, 1800 * time.Millisecond}
	if !slices.Equal(got, want) {
		t.Errorf("let go at %v, want %v: a burst of %d, then one each 600ms", got, want, DefaultBurst)
	}
}

// Clients on one rate file share its limit, in one process as in several.
func TestRateSharedBetweenClients(t *testing.T) {
	ft := &fakeTime{now: testNow}
	path := filepath.Join(t.TempDir(), "rate")
	c1 := newRateClient(t, path, ClientConfig{Burst: 2}, ft)
	c2 := newRateClient(t, path, ClientConfig{Burst: 2}, ft)
	if c1.limit != c2.limit {
		t.Error("two clients on one rate file have their own limiters")
	}
	var got []time.Duration
	for range 2 {
		got = append(got, takeTimes(t, c1, 1, testNow)...)
		got = append(got, takeTimes(t, c2, 1, testNow)...)
	}
	if want := []time.Duration{0, 0, 600 * time.Millisecond, 1200 * time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("let go at %v, want %v", got, want)
	}
}

// A pause holds back every request until it ends, then lets them go one
// each interval, without a burst. A shorter pause does not cut short a
// longer one.
func TestRatePauseHoldsBackEveryone(t *testing.T) {
	ft := &fakeTime{now: testNow}
	path := filepath.Join(t.TempDir(), "rate")
	c1 := newRateClient(t, path, ClientConfig{}, ft)
	c2 := newRateClient(t, path, ClientConfig{}, ft)
	if err := c1.pause(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := c1.pause(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	got := takeTimes(t, c2, 3, testNow)
	if want := []time.Duration{10 * time.Second, 10*time.Second + 600*time.Millisecond, 10*time.Second + 1200*time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("after a pause of 10s and then one of 1s, let go at %v, want %v", got, want)
	}
}

// A clock set back does not hold requests for as long as it moved.
func TestRateClockSetBack(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	takeTimes(t, c, DefaultBurst, testNow)
	back := testNow.Add(-30 * time.Second)
	ft.set(back)
	got := takeTimes(t, c, 3, back)
	if want := []time.Duration{600 * time.Millisecond, 1200 * time.Millisecond, 1800 * time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("after the clock went back 30s, let go at %v, want %v", got, want)
	}
}

// A rate file that holds something else, or a time far ahead, as a clock set
// forward and back again might leave, lets a request go at once, then one
// each interval; an empty file has had no requests yet.
func TestRateFileContents(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     []time.Duration
	}{
		{"empty", "", []time.Duration{0, 0, 0}},
		{"not a record", "garbage", []time.Duration{0, 600 * time.Millisecond, 1200 * time.Millisecond}},
		{"cut short", "1 0000000", []time.Duration{0, 600 * time.Millisecond, 1200 * time.Millisecond}},
		{"an hour ahead", string(rateState{tat: testNow.Add(time.Hour), last: testNow}.record()), []time.Duration{0, 600 * time.Millisecond, 1200 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rate")
			if err := os.WriteFile(path, []byte(tc.contents), 0o600); err != nil {
				t.Fatal(err)
			}
			ft := &fakeTime{now: testNow}
			c := newRateClient(t, path, ClientConfig{}, ft)
			if got := takeTimes(t, c, 3, testNow); !slices.Equal(got, tc.want) {
				t.Errorf("let go at %v, want %v", got, tc.want)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if s := parseRate(b); s.bad || len(b) != rateRecordLen {
				t.Errorf("the file holds %q afterwards, want a record", b)
			}
		})
	}
}

// A request that the rate limit would let go only after the caller's
// deadline is refused at once, and takes no slot; nor does one whose caller
// gives up while it waits.
func TestRateTakesNoSlotForAWaitCutShort(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	if err := c.pause(t.Context(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 20 {
		_, err := c.takeAt(ctx)
		if !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "in 30s, after the deadline") {
			t.Fatalf("with 5s left and 30s to wait: %v, want ErrRateLimited", err)
		}
	}
	cancelled, cancelNow := context.WithCancel(t.Context())
	ft.onSleep = func(time.Duration) { cancelNow() }
	for range 20 {
		if _, err := c.takeAt(cancelled); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled during the wait: %v", err)
		}
	}
	ft.onSleep = nil
	ft.set(testNow)
	if got := takeTimes(t, c, 1, testNow); got[0] != 30*time.Second {
		t.Errorf("after 40 requests that gave up, the next waited %v, want 30s", got[0])
	}
}

// Where the rate file cannot be used, the process keeps its own limit, and
// says so once. Once the file can be used again, it is, with what the
// process did meanwhile.
func TestRateFileUnusable(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	ft := &fakeTime{now: testNow}
	path := filepath.Join(notADir, "rate")
	c := newRateClient(t, path, ClientConfig{}, ft)
	c.auth.cfg.Warn = func(msg string) { warnings = append(warnings, msg) }
	got := takeTimes(t, c, 7, testNow)
	if want := []time.Duration{0, 0, 0, 0, 0, 600 * time.Millisecond, 1200 * time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("let go at %v, want %v", got, want)
	}
	if err := c.pause(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(notADir); err != nil {
		t.Fatal(err)
	}
	got = takeTimes(t, c, 1, testNow)
	if want := 1200*time.Millisecond + 10*time.Second; got[0] != want {
		t.Errorf("with the file back, let go at %v, want %v: after the pause made without it", got[0], want)
	}
	b, err := os.ReadFile(c.limit.path)
	if err != nil {
		t.Fatal(err)
	}
	if s := parseRate(b); s.bad || len(b) != rateRecordLen || !s.tat.After(testNow.Add(10*time.Second)) {
		t.Errorf("the file holds %q, want the pause in its record", b)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "keeps its own rate limit") {
		t.Errorf("warnings %q, want one that says the process keeps its own limit", warnings)
	}
}

// A lock on the rate file that another process keeps, as a process stopped
// while it held it would, costs one wait, not one for each request; the
// requests meanwhile keep this process's own limit, and the file takes in
// what they did once its lock is free.
func TestRateLockKeptByAnother(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	var warnings []string
	c.auth.cfg.Warn = func(msg string) { warnings = append(warnings, msg) }
	waits, looks := 0, 0
	held := true
	c.limit.lock = func(ctx context.Context, path string, wait time.Duration) (*filelock.Lock, error) {
		waits++
		if held {
			return nil, fmt.Errorf("waited %v: %w", wait, filelock.ErrTimeout)
		}
		return filelock.Acquire(ctx, path, wait)
	}
	c.limit.tryLock = func(path string) (*filelock.Lock, bool, error) {
		looks++
		if held {
			return nil, false, nil
		}
		return filelock.TryAcquire(path)
	}
	got := takeTimes(t, c, 6, testNow)
	if err := c.pause(t.Context(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{0, 0, 0, 0, 0, 600 * time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("while the lock was kept, let go at %v, want %v", got, want)
	}
	if waits != 1 || looks != 7 {
		t.Errorf("%d waits for the lock and %d looks, want 1 and 7", waits, looks)
	}
	held = false
	if got := takeTimes(t, c, 1, testNow); got[0] != 600*time.Millisecond+10*time.Second {
		t.Errorf("once the lock was free, let go at %v, want after the pause made meanwhile", got[0])
	}
	// And later checks wait for the lock again, as before, rather than look.
	before := waits
	takeTimes(t, c, 1, testNow)
	if waits == before || looks != 8 {
		t.Errorf("after the lock was free: %d more waits for it and %d looks, want some and still 8", waits-before, looks)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings %q, want one", warnings)
	}
	b, err := os.ReadFile(c.limit.path)
	if err != nil || parseRate(b).bad || len(b) != rateRecordLen {
		t.Errorf("the file holds %q, %v; want a record", b, err)
	}
}

// Callers whose deadlines are too short for the whole wait for a kept lock
// end it together, and then keep the process's own limit.
func TestRateShortDeadlinesReachBusyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rate")
	c := newRateClient(t, path, ClientConfig{}, nil)
	c.limit.wait = 300 * time.Millisecond
	var warnings []string
	c.auth.cfg.Warn = func(msg string) { warnings = append(warnings, msg) }
	if _, err := c.takeAt(t.Context()); err != nil { // makes the file
		t.Fatal(err)
	}
	l, ok, err := filelock.TryAcquire(c.limit.path)
	if err != nil || !ok {
		t.Fatalf("taking the lock: %v, %v", ok, err)
	}
	defer l.Release()
	for i := 1; ; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		_, err := c.takeAt(ctx)
		cancel()
		if err == nil {
			break
		}
		if !errors.Is(err, context.DeadlineExceeded) || i == 10 {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	// The warning names the wait in all, not the last check's part of it.
	var waited time.Duration
	if len(warnings) != 1 {
		t.Fatalf("warnings %q, want one", warnings)
	}
	if _, after, ok := strings.Cut(warnings[0], "waited "); ok {
		waited, _ = time.ParseDuration(strings.Fields(after)[0])
	}
	if waited < c.limit.wait {
		t.Errorf("the warning %q names a wait of %v, want at least %v", warnings[0], waited, c.limit.wait)
	}
}

// A clock set back while the process kept its own limit is taken into
// account once, not twice, when the file can be used again.
func TestRateClockSetBackWhileKeptApart(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	takeTimes(t, c, 1, testNow) // the file's last write, before the clock goes back
	held := true
	c.limit.lock = func(ctx context.Context, path string, wait time.Duration) (*filelock.Lock, error) {
		if held {
			return nil, filelock.ErrTimeout
		}
		return filelock.Acquire(ctx, path, wait)
	}
	c.limit.tryLock = func(path string) (*filelock.Lock, bool, error) {
		if held {
			return nil, false, nil
		}
		return filelock.TryAcquire(path)
	}
	if err := c.pause(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	back := testNow.Add(-30 * time.Second)
	ft.set(back)
	// A check with no time to wait moves the process's own limit to the
	// new clock.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := c.takeAt(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("a check during the pause: %v, want ErrRateLimited", err)
	}
	held = false
	if got := takeTimes(t, c, 1, back); got[0] != time.Minute {
		t.Errorf("let go %v after the clock went back, want the minute that was left of the pause", got[0])
	}
}

// A pause waits for its turn at the limit longer than the lock does, so that
// when another process keeps the lock, the lock's wait runs out first and the
// pause is kept in this process's own limit, rather than dropped.
func TestRatePauseOutlastsTheLockWait(t *testing.T) {
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, &fakeTime{now: testNow})
	c.limit.lock = func(ctx context.Context, _ string, wait time.Duration) (*filelock.Lock, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) < wait+time.Second {
			t.Errorf("the pause gives its turn %v, the lock waits %v", time.Until(deadline), wait)
		}
		return nil, filelock.ErrTimeout
	}
	if err := c.pause(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
}

// A file system that cannot lock files is not tried again.
func TestRateFileSystemWithoutLocks(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	calls := 0
	c.limit.lock = func(context.Context, string, time.Duration) (*filelock.Lock, error) {
		calls++
		return nil, fmt.Errorf("lock: %w", filelock.ErrUnsupported)
	}
	if got := takeTimes(t, c, 7, testNow); got[6] != 1200*time.Millisecond {
		t.Errorf("let go at %v, want the process's own limit", got)
	}
	if calls != 1 {
		t.Errorf("%d tries of the lock, want 1", calls)
	}
}

// Goroutines that share a client keep its limit together.
func TestRateGoroutinesShareOneLimiter(t *testing.T) {
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), testRateConfig(""), nil)
	var mu sync.Mutex
	var times []int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 10 {
				at, err := c.takeAt(t.Context())
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				times = append(times, at.UnixNano())
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	slices.Sort(times)
	var tat time.Time
	for _, n := range times {
		at := time.Unix(0, n)
		if at.Before(tat.Add(-c.tolerance)) {
			t.Fatalf("a request went at %v, %v before the limit let it", at.Format(time.StampMicro), tat.Add(-c.tolerance).Sub(at))
		}
		tat = later(tat, at).Add(c.interval)
	}
}

// A TAT that a process with a slower rate put far ahead is not taken for a
// corrupt one by a process with the defaults, and a burst is at most a
// minute's worth.
func TestRateBoundIsTheSameForEveryProcess(t *testing.T) {
	ft := &fakeTime{now: testNow}
	path := filepath.Join(t.TempDir(), "rate")
	slow := newRateClient(t, path, ClientConfig{RatePerMinute: 1, Burst: 2}, ft)
	if got := takeTimes(t, slow, 2, testNow); got[1] != 0 {
		t.Fatalf("a burst of 2 went at %v", got)
	}
	// The TAT is now two minutes ahead. A default process's check, which its
	// deadline cuts short, must leave it there.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := newRateClient(t, path, ClientConfig{}, ft).takeAt(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("the default process's check: %v, want ErrRateLimited", err)
	}
	if got := takeTimes(t, slow, 1, testNow); got[0] != time.Minute {
		t.Errorf("after the default process's check, the slow one went again at %v, want 1m0s", got[0])
	}
	for _, cfg := range []ClientConfig{{RatePerMinute: 10, Burst: 30}, {Burst: 1 << 40}} {
		c := newRateClient(t, path, cfg, ft)
		if c.tolerance > time.Minute || c.tolerance < 0 {
			t.Errorf("%+v has a tolerance of %v, want at most a minute", cfg, c.tolerance)
		}
	}
}

// A pause right after the clock was set back counts from the new time.
func TestRatePauseAfterTheClockWentBack(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	takeTimes(t, c, 1, testNow)
	back := testNow.Add(-30 * time.Second)
	ft.set(back)
	if err := c.pause(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	if got := takeTimes(t, c, 1, back); got[0] != time.Second {
		t.Errorf("let go %v after the pause began, want 1s", got[0])
	}
}

// A caller that has gone takes no slot, and a pause is made all the same.
func TestRateCallerGone(t *testing.T) {
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(t.TempDir(), "rate"), ClientConfig{}, ft)
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	for range 20 {
		if _, err := c.takeAt(gone); !errors.Is(err, context.Canceled) {
			t.Fatalf("takeAt after the cancel: %v", err)
		}
	}
	if got := takeTimes(t, c, DefaultBurst, testNow); got[DefaultBurst-1] != 0 {
		t.Errorf("after 20 takes that had gone, a burst went at %v, want all at once", got)
	}
	if err := c.pause(gone, 10*time.Second); err != nil {
		t.Fatalf("a pause after the cancel: %v", err)
	}
	if got := takeTimes(t, c, 1, testNow); got[0] != 10*time.Second {
		t.Errorf("after a pause by a caller that had gone, let go at %v, want 10s", got[0])
	}
}

// The limit is kept by app key, in the user cache directory, or next to the
// token file without one: logins on one app key share it.
func TestDefaultRateFile(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache) // Linux and other Unix systems
	t.Setenv("HOME", cache)           // macOS: Library/Caches in it
	t.Setenv("LocalAppData", cache)   // Windows
	newClientFor := func(key, tokenFile string) *Client {
		a, err := NewAuth(Config{AppKey: key, AppSecret: testSecret, TokenFile: tokenFile})
		if err != nil {
			t.Fatal(err)
		}
		return NewClient(a, ClientConfig{})
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir, err = resolve(dir); err != nil {
		t.Fatal(err)
	}
	one := newClientFor(testKey, filepath.Join(t.TempDir(), "a.json"))
	two := newClientFor(testKey, filepath.Join(t.TempDir(), "b.json"))
	other := newClientFor("another-key", filepath.Join(t.TempDir(), "a.json"))
	if one.limit != two.limit || one.limit == other.limit {
		t.Errorf("rate files %s, %s and %s; want the first two the same, for one app key", one.limit.path, two.limit.path, other.limit.path)
	}
	if got := filepath.Dir(one.limit.path); got != filepath.Join(dir, "schwabtx") {
		t.Errorf("rate file %s, want it in %s", one.limit.path, filepath.Join(dir, "schwabtx"))
	}
	if name := filepath.Base(one.limit.path); len(name) != len("rate-")+16 || !strings.HasPrefix(name, "rate-") || strings.Contains(name, testKey) {
		t.Errorf("rate file named %s, want rate- and 16 hex digits of a hash of the key", name)
	}

	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	t.Setenv("LocalAppData", "")
	if _, err := os.UserCacheDir(); err == nil {
		t.Skip("this system has a cache directory without HOME")
	}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	c := newClientFor(testKey, tokenFile)
	resolvedToken, err := resolve(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Dir(c.limit.path); got != filepath.Dir(resolvedToken) {
		t.Errorf("without a cache directory, the rate file is %s, want it next to %s", c.limit.path, resolvedToken)
	}
}

// rateHelper is the helper process for TestRateManyProcesses: once its stdin
// says "go", it takes n slots of the rate file and prints when each was let
// go, in nanoseconds since 1970.
func rateHelper() int {
	n, _ := strconv.Atoi(os.Getenv("SCHWAB_TEST_RATE_N"))
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, TokenFile: os.Getenv("SCHWAB_TEST_TOKEN_FILE")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c := NewClient(a, testRateConfig(os.Getenv("SCHWAB_TEST_RATE_FILE")))
	fmt.Println("ready")
	if line, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil || line != "go\n" {
		fmt.Fprintln(os.Stderr, "no go:", line, err)
		return 1
	}
	out := bufio.NewWriter(os.Stdout)
	for range n {
		at, err := c.takeAt(context.Background())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Fprintln(out, at.UnixNano())
	}
	if err := out.Flush(); err != nil {
		return 1
	}
	return 0
}

// testRateConfig is the limit that TestRateManyProcesses uses, a hundred
// times as fast as the default.
func testRateConfig(rateFile string) ClientConfig {
	return ClientConfig{RatePerMinute: 6000, Burst: 3, RateFile: rateFile}
}

// Processes that share a rate file keep its limit together, and a pause that
// one of them makes holds back the others.
func TestRateManyProcesses(t *testing.T) {
	if !filelock.Supported {
		t.Skip("no file locks on " + runtime.GOOS)
	}
	const procs, each = 3, 30
	path := filepath.Join(t.TempDir(), "rate")
	type helper struct {
		cmd   *exec.Cmd
		stdin io.WriteCloser
		out   *bufio.Reader
	}
	var helpers []helper
	for range procs {
		cmd := startHelper(t, "rate", "", filepath.Join(t.TempDir(), "token.json"))
		cmd.Env = append(cmd.Env, "SCHWAB_TEST_RATE_FILE="+path, fmt.Sprintf("SCHWAB_TEST_RATE_N=%d", each))
		cmd.Stderr = os.Stderr
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		h := helper{cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
		if line, err := h.out.ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("a helper said %q, %v", line, err)
		}
		helpers = append(helpers, h)
	}
	for _, h := range helpers {
		if _, err := io.WriteString(h.stdin, "go\n"); err != nil {
			t.Fatal(err)
		}
	}

	// Part way through, this process pauses them all, as a 429 would.
	c := newRateClient(t, path, testRateConfig(""), nil)
	const pause = 150 * time.Millisecond
	time.Sleep(200 * time.Millisecond)
	before := time.Now()
	if err := c.pause(t.Context(), pause); err != nil {
		t.Fatal(err)
	}
	after := time.Now()

	var times []int64
	for _, h := range helpers {
		for {
			line, err := h.out.ReadString('\n')
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			n, err := strconv.ParseInt(strings.TrimSpace(line), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			times = append(times, n)
		}
		if err := h.cmd.Wait(); err != nil {
			t.Fatalf("a helper: %v", err)
		}
	}
	if len(times) != procs*each {
		t.Fatalf("%d requests let go, want %d", len(times), procs*each)
	}

	// Replayed in order, the times must keep the limit: none before TAT -
	// tolerance, with the pause, which this process made between before and
	// after, putting the TAT at least at before + pause + tolerance. Each
	// time was read under the lock, so a request let go before the pause has
	// a time of at most after, even on a clock as coarse as Windows', and one
	// let go after it a time at least pause later.
	cfg := testRateConfig("")
	interval := time.Minute / time.Duration(cfg.RatePerMinute)
	tolerance := time.Duration(cfg.Burst-1) * interval
	slices.Sort(times)
	var tat time.Time
	paused, early, late := false, 0, 0
	for _, n := range times {
		at := time.Unix(0, n)
		if !paused && at.After(after) {
			paused = true
			tat = later(tat, before.Add(pause+tolerance))
		}
		if paused {
			late++
		} else {
			early++
		}
		if at.Before(tat.Add(-tolerance)) {
			t.Fatalf("a request went at %v, %v before the limit let it", at.Format(time.StampMicro), tat.Add(-tolerance).Sub(at))
		}
		tat = later(tat, at).Add(interval)
	}
	if early == 0 || late == 0 {
		t.Errorf("%d requests before the pause and %d after, want some of each", early, late)
	}
}
