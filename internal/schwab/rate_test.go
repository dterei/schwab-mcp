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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
// says so once.
func TestRateFileUnusable(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	ft := &fakeTime{now: testNow}
	c := newRateClient(t, filepath.Join(notADir, "rate"), ClientConfig{}, ft)
	c.auth.cfg.Warn = func(msg string) { warnings = append(warnings, msg) }
	got := takeTimes(t, c, 7, testNow)
	if want := []time.Duration{0, 0, 0, 0, 0, 600 * time.Millisecond, 1200 * time.Millisecond}; !slices.Equal(got, want) {
		t.Errorf("let go at %v, want %v", got, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "keeps its own rate limit") {
		t.Errorf("warnings %q, want one that says the process keeps its own limit", warnings)
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
	// after, putting the TAT at least at before + pause + tolerance.
	cfg := testRateConfig("")
	interval := time.Minute / time.Duration(cfg.RatePerMinute)
	tolerance := time.Duration(cfg.Burst-1) * interval
	const slack = time.Millisecond // for clocks read in a different order than the lock was taken
	slices.Sort(times)
	var tat time.Time
	paused, early, late := false, 0, 0
	for _, n := range times {
		at := time.Unix(0, n)
		if !paused && !at.Before(after) {
			paused = true
			tat = later(tat, before.Add(pause+tolerance))
		}
		if paused {
			late++
		} else {
			early++
		}
		if at.Add(slack).Before(tat.Add(-tolerance)) {
			t.Fatalf("a request went at %v, %v before the limit let it", at.Format(time.StampMicro), tat.Add(-tolerance).Sub(at))
		}
		tat = later(tat, at).Add(interval)
	}
	if early == 0 || late == 0 {
		t.Errorf("%d requests before the pause and %d after, want some of each", early, late)
	}
}
