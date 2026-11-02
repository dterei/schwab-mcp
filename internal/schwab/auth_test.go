package schwab

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dterei/schwab-mcp/internal/filelock"
)

// TestMain lets the test binary stand in for another process that uses the
// login, when a test runs it with SCHWAB_TEST_HELPER set. `go test` never
// sets it.
func TestMain(m *testing.M) {
	if role := os.Getenv("SCHWAB_TEST_HELPER"); role != "" {
		os.Exit(helper(role))
	}
	os.Exit(m.Run())
}

// helper is the other process. "access" prints an access token; "hold" takes
// the lock on the login, says so and keeps it until its stdin closes; "rate"
// is rateHelper.
func helper(role string) int {
	ctx := context.Background()
	tokenFile := os.Getenv("SCHWAB_TEST_TOKEN_FILE")
	switch role {
	case "rate":
		return rateHelper()
	case "access":
		a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: os.Getenv("SCHWAB_TEST_URL"), TokenFile: tokenFile})
		if err == nil {
			var tok string
			if tok, err = a.AccessToken(ctx); err == nil {
				fmt.Println(tok)
				return 0
			}
		}
		fmt.Fprintln(os.Stderr, err)
	case "hold":
		l, err := filelock.Acquire(ctx, tokenFile+".lock", time.Minute)
		if err == nil {
			fmt.Println("locked")
			_, _ = io.Copy(io.Discard, os.Stdin)
			l.Release()
			return 0
		}
		fmt.Fprintln(os.Stderr, err)
	default:
		fmt.Fprintln(os.Stderr, "unknown helper role", role)
	}
	return 1
}

// startHelper runs the test binary as another process in role.
func startHelper(t *testing.T, role, url, tokenFile string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "SCHWAB_TEST_HELPER="+role, "SCHWAB_TEST_URL="+url, "SCHWAB_TEST_TOKEN_FILE="+tokenFile)
	return cmd
}

var testNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// clock is a time that tests can move.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestAuth returns an Auth on tokenFile that talks to f and tells the time
// by c.
func newTestAuth(t *testing.T, f *fakeOAuth, tokenFile string, c *clock) *Auth {
	t.Helper()
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, BaseURL: f.srv.URL, TokenFile: tokenFile, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// writeTestToken saves t at path the way an earlier login would have.
func writeTestToken(t *testing.T, path string, tok token) {
	t.Helper()
	if err := writeToken(path, &tok); err != nil {
		t.Fatal(err)
	}
}

func readTestToken(t *testing.T, path string) token {
	t.Helper()
	tok, err := readToken(path)
	if err != nil {
		t.Fatal(err)
	}
	return *tok
}

// call makes a request with a's access token as the CLI does: on a 401 it
// gets a new token once and tries again.
func call(t *testing.T, f *fakeOAuth, a *Auth) string {
	t.Helper()
	ctx := t.Context()
	tok, err := a.AccessToken(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+"/whoami", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return tok
		}
		if tok, err = a.Refresh(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("Schwab refused the access token twice")
	return ""
}

func TestLoginThenAccessToken(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "cfg", "token.json")
	a := newTestAuth(t, f, tokenFile, c)

	if st, err := a.Status(); err != nil || st.LoggedIn {
		t.Fatalf("Status before any login: %+v, %v; want not logged in", st, err)
	}
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("AccessToken before any login: %v, want ErrNotLoggedIn", err)
	}
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	st, err := a.Status()
	if err != nil || !st.LoggedIn || !st.LoginAt.Equal(testNow) || !st.ExpiresAt.Equal(testNow.Add(LoginLifetime)) || st.Scope != "api" || st.Rejected != nil {
		t.Errorf("Status after the login: %+v, %v; want logged in at %v, expiring %v, scope api", st, err, testNow, testNow.Add(LoginLifetime))
	}
	if resolved, err := filepath.EvalSymlinks(tokenFile); err != nil || st.TokenFile != resolved {
		t.Errorf("Status names %s, want %s (%v)", st.TokenFile, resolved, err)
	}
	if tok := call(t, f, a); tok != "access-1" {
		t.Errorf("access token %q, want access-1", tok)
	}
	if code, refresh := f.grants(); code != 1 || refresh != 0 {
		t.Errorf("%d code and %d refresh grants, want 1 and 0", code, refresh)
	}

	// Another process reads the same login from the file.
	b := newTestAuth(t, f, tokenFile, c)
	if tok := call(t, f, b); tok != "access-1" {
		t.Errorf("second process got %q, want access-1", tok)
	}

	if runtime.GOOS == "windows" {
		return // no Unix permission bits; the files take their folder's ACLs
	}
	for path, want := range map[string]os.FileMode{
		filepath.Dir(tokenFile): 0o700,
		tokenFile:               0o600,
		tokenFile + ".lock":     0o600,
	} {
		if info, err := os.Stat(path); err != nil {
			t.Error(err)
		} else if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %v, want %v", path, got, want)
		}
	}
}

func TestExpiredAccessTokenIsRefreshed(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(fmt.Sprintf("rotate=%v", rotate), func(t *testing.T) {
			f := newFakeOAuth(t)
			f.rotate = rotate
			c := &clock{now: testNow}
			tokenFile := filepath.Join(t.TempDir(), "token.json")
			a := newTestAuth(t, f, tokenFile, c)
			if err := a.Login(t.Context(), testCode); err != nil {
				t.Fatal(err)
			}
			before := readTestToken(t, tokenFile)

			c.advance(29*time.Minute + 30*time.Second) // within the minute before the access token expires
			tok := call(t, f, a)
			if _, refresh := f.grants(); refresh != 1 || tok != "access-2" {
				t.Fatalf("%d refresh grants, token %q; want 1, access-2", refresh, tok)
			}
			after := readTestToken(t, tokenFile)
			if after.AccessToken != "access-2" || !after.AccessExpiresAt.Equal(c.Now().Add(30*time.Minute)) {
				t.Errorf("saved %+v, want access-2 expiring 30 minutes from now", after)
			}
			if !after.RefreshObtainedAt.Equal(before.RefreshObtainedAt) {
				t.Errorf("a refresh moved the login time from %v to %v", before.RefreshObtainedAt, after.RefreshObtainedAt)
			}
			if rotate == (after.RefreshToken == before.RefreshToken) {
				t.Errorf("refresh token %q after %q, with rotation %v", after.RefreshToken, before.RefreshToken, rotate)
			}
		})
	}
}

// A process that logged in earlier picks up a later login by another
// process, rather than refresh with a refresh token that no longer works.
func TestRefreshUsesTheLatestLogin(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	server := newTestAuth(t, f, tokenFile, c)
	if err := server.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(24 * time.Hour)
	cli := newTestAuth(t, f, tokenFile, c)
	if err := cli.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}

	if tok := call(t, f, server); tok != "access-2" {
		t.Errorf("the server got %q, want the new login's access-2", tok)
	}
	if _, refresh := f.grants(); refresh != 0 {
		t.Errorf("%d refresh grants, want 0: the new login's access token works", refresh)
	}
	if st, err := server.Status(); err != nil || !st.ExpiresAt.Equal(testNow.Add(24*time.Hour+LoginLifetime)) {
		t.Errorf("the server says the login expires at %v (%v), want %v", st.ExpiresAt, err, testNow.Add(24*time.Hour+LoginLifetime))
	}
	c.advance(31 * time.Minute)
	if tok := call(t, f, server); tok != "access-3" {
		t.Errorf("after expiry the server got %q, want access-3 from refresh-2", tok)
	}
}

// A process whose access token Schwab refuses, because another process's
// refresh replaced it, takes the new one from the file without a refresh of
// its own.
func TestRefreshAfter401TakesAnotherProcesssToken(t *testing.T) {
	f := newFakeOAuth(t)
	f.revokeOlder = true
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuth(t, f, tokenFile, c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	if tok := call(t, f, a); tok != "access-1" {
		t.Fatalf("got %q, want access-1", tok)
	}
	c.advance(2 * time.Minute) // a 401 for a token issued moments ago leads to no refresh
	b := newTestAuth(t, f, tokenFile, c)
	if _, err := b.Refresh(t.Context(), "access-1"); err != nil {
		t.Fatal(err)
	}
	if tok := call(t, f, a); tok != "access-2" {
		t.Errorf("got %q, want b's access-2", tok)
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want only b's", refresh)
	}
}

// Many users of one login, in one process, refresh once between them.
func TestConcurrentRefreshInOneProcess(t *testing.T) {
	f := newFakeOAuth(t)
	f.rotate = true // a second refresh with the same refresh token would fail
	f.delay = 50 * time.Millisecond
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)

	shared := newTestAuth(t, f, tokenFile, c)
	var wg sync.WaitGroup
	tokens := make([]string, 8)
	errs := make([]error, len(tokens))
	for i := range tokens {
		a := shared
		if i%2 == 1 {
			a = newTestAuth(t, f, tokenFile, c) // its own view, as in another package
		}
		wg.Go(func() { tokens[i], errs[i] = a.AccessToken(t.Context()) })
	}
	wg.Wait()
	for i := range tokens {
		if errs[i] != nil || tokens[i] != "access-2" {
			t.Errorf("caller %d got %q, %v; want access-2", i, tokens[i], errs[i])
		}
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want 1", refresh)
	}
}

// Processes that find the access token expired at the same time refresh it
// once between them.
func TestConcurrentRefreshAcrossProcesses(t *testing.T) {
	if !filelock.Supported {
		t.Skip("no file locks on " + runtime.GOOS)
	}
	f := newFakeOAuth(t)
	f.rotate = true
	f.delay = 200 * time.Millisecond
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	f.allow("refresh-0")
	// The helpers tell the time by the real clock.
	writeTestToken(t, tokenFile, token{
		AccessToken: "access-0", RefreshToken: "refresh-0",
		AccessExpiresAt: time.Now().Add(-time.Minute), RefreshObtainedAt: time.Now().Add(-time.Hour),
	})

	var wg sync.WaitGroup
	outs := make([]string, 5)
	errs := make([]error, len(outs))
	for i := range outs {
		cmd := startHelper(t, "access", f.srv.URL, tokenFile)
		wg.Go(func() {
			b, err := cmd.Output()
			outs[i] = strings.TrimSpace(string(b))
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				err = fmt.Errorf("%w: %s", err, ee.Stderr)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil || outs[i] != outs[0] || outs[i] == "" {
			t.Errorf("process %d printed %q, %v; want the same token as process 0 (%q)", i, outs[i], errs[i], outs[0])
		}
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want 1", refresh)
	}
	if saved := readTestToken(t, tokenFile); saved.AccessToken != outs[0] {
		t.Errorf("saved %q, but the processes use %q", saved.AccessToken, outs[0])
	}
}

func TestLockWait(t *testing.T) {
	if !filelock.Supported {
		t.Skip("no file locks on " + runtime.GOOS)
	}
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuth(t, f, tokenFile, c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)

	// Another process holds the lock.
	holder := startHelper(t, "hold", f.srv.URL, tokenFile)
	stdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	holder.Stderr = os.Stderr
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the holder said %q, %v", line, err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := a.AccessToken(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting past the context's deadline: %v, want context.DeadlineExceeded", err)
	}
	a.cfg.LockWait = 100 * time.Millisecond
	if _, err := a.AccessToken(t.Context()); !errors.Is(err, filelock.ErrTimeout) || !strings.Contains(err.Error(), "another process kept the Schwab login locked") {
		t.Errorf("waiting past the limit: %v, want it to say that another process kept the login locked", err)
	}

	// Once the holder lets go, the refresh goes ahead.
	a.cfg.LockWait = DefaultLockWait
	done := make(chan error, 1)
	go func() {
		_, err := a.AccessToken(t.Context())
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	stdin.Close()
	if err := holder.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("after the holder let go: %v", err)
	}
}

func TestErrors(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	dir := t.TempDir()

	missing := newTestAuth(t, f, filepath.Join(dir, "missing.json"), c)
	if _, err := missing.AccessToken(t.Context()); !errors.Is(err, ErrNotLoggedIn) {
		t.Errorf("no token file: %v, want ErrNotLoggedIn", err)
	}

	for name, content := range map[string]string{
		"corrupt":    "{not json",
		"empty":      "",
		"no refresh": `{"access_token":"a"}`,
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := newTestAuth(t, f, path, c).Status()
		if err == nil || !strings.Contains(err.Error(), "log in again") {
			t.Errorf("%s token file: %v, want it to say to log in again", name, err)
		}
	}

	// A refresh token that Schwab no longer takes.
	stale := filepath.Join(dir, "stale.json")
	writeTestToken(t, stale, token{AccessToken: "old", RefreshToken: "stale", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow.Add(-8 * 24 * time.Hour)})
	before, err := os.ReadFile(stale)
	if err != nil {
		t.Fatal(err)
	}
	_, err = newTestAuth(t, f, stale, c).AccessToken(t.Context())
	se, ok := errors.AsType[*Error](err)
	if !errors.Is(err, ErrLoginExpired) || !ok || se.Status != http.StatusBadRequest || se.Code != "unsupported_token_type" ||
		err.Error() != "Schwab rejected the saved login: HTTP 400 from Schwab: unsupported_token_type (invalid_grant)" {
		t.Errorf("stale refresh token: %v, want ErrLoginExpired with the HTTP 400 and its code", err)
	}
	if after, err := os.ReadFile(stale); err != nil || string(after) != string(before) {
		t.Errorf("a failed refresh changed the token file: %v", err)
	}

	// An app key or secret that Schwab refuses.
	f.allow("refresh-x")
	writeTestToken(t, stale, token{AccessToken: "old", RefreshToken: "refresh-x", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow})
	wrong, err := NewAuth(Config{AppKey: testKey, AppSecret: "wrong", BaseURL: f.srv.URL, TokenFile: stale, Now: c.Now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.AccessToken(t.Context()); !errors.Is(err, ErrAppRejected) || !strings.Contains(err.Error(), "invalid_client") {
		t.Errorf("wrong secret: %v, want ErrAppRejected", err)
	}
	if err := wrong.Login(t.Context(), testCode); !errors.Is(err, ErrAppRejected) {
		t.Errorf("login with the wrong secret: %v, want ErrAppRejected", err)
	}

	for _, cfg := range []Config{{AppKey: testKey, TokenFile: stale}, {AppSecret: testSecret, TokenFile: stale}} {
		if _, err := NewAuth(cfg); err == nil {
			t.Errorf("NewAuth(%+v) took a config without the app key or secret", cfg)
		}
	}
}

// A token file that schwabtx wrote before this package existed still works.
func TestEarlierTokenFile(t *testing.T) {
	f := newFakeOAuth(t)
	f.allow("refresh-token-1")
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	earlier := `{
  "access_token": "access-0",
  "refresh_token": "refresh-token-1",
  "access_expires_at": "2026-10-08T04:30:00.123456789-07:00",
  "refresh_obtained_at": "2026-10-03T09:15:00.5-07:00"
}
`
	if err := os.WriteFile(tokenFile, []byte(earlier), 0o600); err != nil {
		t.Fatal(err)
	}
	a := newTestAuth(t, f, tokenFile, c)
	if st, err := a.Status(); err != nil || !st.ExpiresAt.Equal(time.Date(2026, 10, 10, 9, 15, 0, 500e6, time.FixedZone("", -7*3600))) {
		t.Errorf("Status of the earlier file: %+v, %v", st, err)
	}
	if tok := call(t, f, a); tok != "access-1" {
		t.Errorf("got %q, want a refreshed access-1", tok)
	}
	var saved map[string]any
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_token", "refresh_token", "access_expires_at", "refresh_obtained_at"} {
		if _, ok := saved[key]; !ok {
			t.Errorf("saved %s, without the %s that schwabtx has always written", b, key)
		}
	}
	if saved["version"] != 1.0 || saved["refresh_obtained_at"] != "2026-10-03T09:15:00.5-07:00" {
		t.Errorf("saved %s, want version 1 and the login time as it was", b)
	}
}

func TestCodeFromRedirect(t *testing.T) {
	cases := []struct {
		name, in, state, want string
		wantErr               bool
	}{
		{"full address", "https://127.0.0.1/?code=C0.abc%40&session=x&state=s1\n", "s1", "C0.abc@", false},
		{"no state echoed", "https://127.0.0.1/?code=C0.abc%40", "s1", "C0.abc@", false},
		{"wrong state", "https://127.0.0.1/?code=C0.abc%40&state=other", "s1", "", true},
		{"bare encoded code", "C0.abc%40", "s1", "C0.abc@", false},
		{"bare code", "  C0.abc@  ", "s1", "C0.abc@", false},
		{"address without scheme", "127.0.0.1/?code=C0.abc%40&session=x&state=s1", "s1", "C0.abc@", false},
		{"query string only", "code=C0.abc%40&state=s1", "s1", "C0.abc@", false},
		{"query string, wrong state", "code=C0.abc%40&state=other", "s1", "", true},
		{"denied", "https://127.0.0.1/?error=access_denied&error_description=nope", "s1", "", true},
		{"no code", "https://127.0.0.1/", "s1", "", true},
		{"empty", "\n", "s1", "", true},
	}
	for _, tc := range cases {
		got, err := CodeFromRedirect(tc.in, tc.state)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q, err=%v", tc.name, got, err, tc.want, tc.wantErr)
		}
	}
}

// Status reads the file, so it shows a login that another process made, at
// once and without a request to Schwab.
func TestStatusSeesAnotherProcesssLogin(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	server := newTestAuth(t, f, tokenFile, c)
	if err := server.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(6 * 24 * time.Hour)
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	requests := f.tokenRequests()
	st, err := server.Status()
	if err != nil || !st.LoginAt.Equal(c.Now()) {
		t.Errorf("Status after another process's login: %+v, %v; want the login at %v", st, err, c.Now())
	}
	if f.tokenRequests() != requests {
		t.Error("Status made a request to Schwab")
	}
}

// Schwab refusing an access token that it issued moments ago means that
// another token would not help, so there is no refresh; an older token gets
// one.
func TestJustIssuedTokenRefusedIsNotRefreshed(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuth(t, f, tokenFile, c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(30 * time.Second)
	if _, err := a.Refresh(t.Context(), "access-1"); !errors.Is(err, ErrTokenRefused) {
		t.Errorf("Refresh of a token issued 30 seconds ago: %v, want ErrTokenRefused", err)
	}
	if _, refresh := f.grants(); refresh != 0 {
		t.Errorf("%d refresh grants, want 0", refresh)
	}
	c.advance(time.Minute)
	if tok, err := a.Refresh(t.Context(), "access-1"); err != nil || tok != "access-2" {
		t.Errorf("Refresh of a token issued 90 seconds ago: %q, %v; want access-2", tok, err)
	}

	// A token file from before the issue time was recorded gets a refresh.
	earlier := filepath.Join(t.TempDir(), "earlier.json")
	f.allow("refresh-e")
	writeTestToken(t, earlier, token{AccessToken: "access-e", RefreshToken: "refresh-e", AccessExpiresAt: c.Now().Add(time.Hour), RefreshObtainedAt: c.Now()})
	if _, err := newTestAuth(t, f, earlier, c).Refresh(t.Context(), "access-e"); err != nil {
		t.Errorf("Refresh with no issue time recorded: %v", err)
	}
}

// A refresh token that Schwab refused is not sent again while the file still
// holds it, and Status says why.
func TestRefusedRefreshTokenIsNotSentAgain(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	writeTestToken(t, tokenFile, token{AccessToken: "old", RefreshToken: "lapsed", AccessExpiresAt: testNow.Add(-time.Hour), RefreshObtainedAt: testNow.Add(-8 * 24 * time.Hour)})
	a := newTestAuth(t, f, tokenFile, c)
	for i := range 3 {
		if _, err := a.AccessToken(t.Context()); !errors.Is(err, ErrLoginExpired) {
			t.Fatalf("call %d: %v, want ErrLoginExpired", i, err)
		}
	}
	if n := f.tokenRequests(); n != 1 {
		t.Errorf("%d token requests for 3 calls, want 1", n)
	}
	if st, err := a.Status(); err != nil || !errors.Is(st.Rejected, ErrLoginExpired) {
		t.Errorf("Status: %+v, %v; want Rejected to be ErrLoginExpired", st, err)
	}
	// A new login, by this process or another, is tried.
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	if st, err := a.Status(); err != nil || st.Rejected != nil {
		t.Errorf("Status after a new login: %+v, %v; want nothing rejected", st, err)
	}
	if tok := call(t, f, a); tok != "access-1" {
		t.Errorf("after a new login: %q, want access-1", tok)
	}
}

// A refresh that the caller gives up on still gets its answer from Schwab and
// saves it, since Schwab may already have retired the old refresh token.
func TestCancelledRefreshIsStillSaved(t *testing.T) {
	f := newFakeOAuth(t)
	f.rotate = true
	f.delay = 300 * time.Millisecond
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuth(t, f, tokenFile, c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, _ = a.AccessToken(ctx)
	if saved := readTestToken(t, tokenFile); saved.RefreshToken != "refresh-2" || saved.AccessToken != "access-2" {
		t.Errorf("saved %s and %s, want the refresh's refresh-2 and access-2", saved.RefreshToken, saved.AccessToken)
	}
	if tok := call(t, f, newTestAuth(t, f, tokenFile, c)); tok != "access-2" {
		t.Errorf("another process got %q, want access-2", tok)
	}
}

// A login holds the lock through its exchange when it can, so that no
// refresh with the old login happens meanwhile, and the others take the new
// login once it is saved.
func TestLoginHoldsTheLockThroughTheExchange(t *testing.T) {
	f := newFakeOAuth(t)
	f.delay = 200 * time.Millisecond
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	f.allow("refresh-0")
	writeTestToken(t, tokenFile, token{AccessToken: "access-0", RefreshToken: "refresh-0", AccessExpiresAt: testNow.Add(-time.Minute), RefreshObtainedAt: testNow.Add(-6 * 24 * time.Hour)})

	done := make(chan error, 1)
	go func() { done <- newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode) }()
	time.Sleep(50 * time.Millisecond) // the login is at Schwab
	tok, err := newTestAuth(t, f, tokenFile, c).AccessToken(t.Context())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err != nil || tok != "access-1" {
		t.Errorf("during the login another process got %q, %v; want the new login's access-1", tok, err)
	}
	if _, refresh := f.grants(); refresh != 0 {
		t.Errorf("%d refresh grants during the login, want 0", refresh)
	}
}

// A login that waited for the lock does not replace a login that another
// process saved after it began.
func TestLoginKeepsANewerLogin(t *testing.T) {
	if !filelock.Supported {
		t.Skip("no file locks on " + runtime.GOOS)
	}
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	held, err := filelock.Acquire(t.Context(), tokenFile+".lock", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	older := newTestAuth(t, f, tokenFile, c)
	go func() { done <- older.Login(t.Context(), testCode) }()
	// Wait until the exchange is done and the login waits for the lock.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if code, _ := f.grants(); code == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the login never reached Schwab")
		}
		time.Sleep(10 * time.Millisecond)
	}
	newer := token{AccessToken: "access-newer", RefreshToken: "refresh-newer", AccessExpiresAt: testNow.Add(time.Hour), RefreshObtainedAt: testNow.Add(time.Minute)}
	writeTestToken(t, tokenFile, newer)
	held.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if saved := readTestToken(t, tokenFile); saved.RefreshToken != "refresh-newer" {
		t.Errorf("the token file holds %s, want the newer login's refresh-newer", saved.RefreshToken)
	}
	if tok, err := older.AccessToken(t.Context()); err != nil || tok != "access-newer" {
		t.Errorf("the older login's process got %q, %v; want the newer login's token", tok, err)
	}
}

// Processes that reach the token file through a link and by its own name
// share its lock, and a rewrite leaves the link in place.
func TestSymlinkedTokenFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need extra rights on Windows")
	}
	if !filelock.Supported {
		t.Skip("no file locks on " + runtime.GOOS)
	}
	f := newFakeOAuth(t)
	f.rotate = true // a second refresh with the same refresh token would fail
	f.delay = 300 * time.Millisecond
	dir := t.TempDir()
	target := filepath.Join(dir, "data", "token.json")
	f.allow("refresh-0")
	// The helpers tell the time by the real clock.
	writeTestToken(t, target, token{
		AccessToken: "access-0", RefreshToken: "refresh-0",
		AccessExpiresAt: time.Now().Add(-time.Minute), RefreshObtainedAt: time.Now().Add(-time.Hour),
	})
	link := filepath.Join(dir, "cfg", "token.json")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	paths := []string{target, link}
	outs := make([]string, len(paths))
	errs := make([]error, len(paths))
	for i, path := range paths {
		cmd := startHelper(t, "access", f.srv.URL, path)
		wg.Go(func() {
			b, err := cmd.Output()
			outs[i] = strings.TrimSpace(string(b))
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				err = fmt.Errorf("%w: %s", err, ee.Stderr)
			}
			errs[i] = err
		})
	}
	wg.Wait()
	for i := range outs {
		if errs[i] != nil || outs[i] != outs[0] || outs[i] == "" {
			t.Errorf("through %s: %q, %v; want the same token as through %s (%q)", paths[i], outs[i], errs[i], paths[0], outs[0])
		}
	}
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want 1", refresh)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is gone after the refresh: %v, %v", info, err)
	}
	if saved := readTestToken(t, link); saved.AccessToken != outs[0] {
		t.Errorf("through the link the file holds %s, want %s", saved.AccessToken, outs[0])
	}
	// A login through the link locks next to the file, too.
	if err := newTestAuth(t, f, link, &clock{now: time.Now()}).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(link + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("there is a lock file next to the link: %v", err)
	}
}

// Fields that this version does not know survive a refresh, and a file
// from a newer format is not rewritten.
func TestTokenFileFromANewerVersion(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	f.allow("refresh-1")
	later := `{"version":1,"access_token":"old","refresh_token":"refresh-1","access_expires_at":"2026-10-08T11:00:00Z",` +
		`"refresh_obtained_at":"2026-10-07T12:00:00Z","granted":{"readonly":true},"note":"kept"}`
	if err := os.WriteFile(tokenFile, []byte(later), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok := call(t, f, newTestAuth(t, f, tokenFile, c)); tok != "access-1" {
		t.Fatalf("got %q, want access-1", tok)
	}
	var saved map[string]json.RawMessage
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	var granted bytes.Buffer
	if err := json.Compact(&granted, saved["granted"]); err != nil || granted.String() != `{"readonly":true}` ||
		string(saved["note"]) != `"kept"` || string(saved["access_token"]) != `"access-1"` {
		t.Errorf("after a refresh the file is %s, want the unknown fields as they were", b)
	}

	newer := strings.Replace(later, `"version":1`, `"version":2`, 1)
	if err := os.WriteFile(tokenFile, []byte(newer), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newTestAuth(t, f, tokenFile, c).Status(); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("a file of format 2: %v, want it refused as from a newer version", err)
	}
}

// tokenFields must name every field of token, so that a rewrite does not
// keep a stale copy of one among the unknown fields.
func TestTokenFields(t *testing.T) {
	var names []string
	for field := range reflect.TypeFor[token]().Fields() {
		if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" {
			names = append(names, name)
		}
	}
	if !slices.Equal(names, tokenFields) {
		t.Errorf("token's fields are %q, but tokenFields is %q", names, tokenFields)
	}
}

func TestDefaultTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)            // macOS
	t.Setenv("XDG_CONFIG_HOME", home) // Linux and other Unix systems
	t.Setenv("AppData", home)         // Windows
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.Status()
	if err != nil {
		t.Fatal(err)
	}
	want, err := DefaultTokenFile()
	if err != nil {
		t.Fatal(err)
	}
	if want, _ = resolve(want); st.TokenFile != want || !strings.HasSuffix(want, filepath.Join("schwabtx", "token.json")) {
		t.Errorf("token file %s, want %s, ending in schwabtx/token.json", st.TokenFile, want)
	}
}

// A token file under a link to a folder is found by its real path even
// before the folders it goes in exist, so that a process that starts before
// the first login uses the same path as one that starts after.
func TestTokenFileUnderALinkedFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links need extra rights on Windows")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	a, err := NewAuth(Config{AppKey: testKey, AppSecret: testSecret, TokenFile: filepath.Join(dir, "link", "cfg", "token.json")})
	if err != nil {
		t.Fatal(err)
	}
	st, err := a.Status()
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if want = filepath.Join(want, "cfg", "token.json"); st.TokenFile != want {
		t.Errorf("token file %s, want %s", st.TokenFile, want)
	}
}

// The refusals below are as community clients logged them: Schwab nests the
// real reason in the description, in more than one way. Only a refusal that
// says why is kept, so that the refresh token is not sent again.
func TestRefreshErrors(t *testing.T) {
	expired2026 := `{"error":"unsupported_token_type","error_description":"400 Bad Request: {\"error_description\":\"Refresh token is invalid, expired or revoked\",\"error\":\"invalid_grant\"}"}`
	replaced2024 := `{"error":"unsupported_token_type","error_description":"400 Bad Request: \"{\\\"error_description\\\":\\\"Exception while authenticating refresh token [tokenDigest=abc], Exception=Failed refresh token authentication [tokenDigest=abc]]\\\",\\\"error\\\":\\\"refresh_token_authentication_error\\\"}\""}`
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		want       error
		final      bool
		wantDetail string
		wantText   string
	}{
		{"lapsed, 2026", 400, expired2026, ErrLoginExpired, true, "invalid_grant", "HTTP 400 from Schwab: unsupported_token_type (invalid_grant)"},
		{"replaced by a later login, 2024", 400, replaced2024, ErrLoginExpired, true, "refresh_token_authentication_error", "HTTP 400 from Schwab: unsupported_token_type (refresh_token_authentication_error)"},
		{"lapsed, as an older report has it", 400, `{"error":"invalid_client","error_description":"refresh token invalid"}`, ErrLoginExpired, true, "", "invalid_client"},
		{"lapsed, older report, with a 401", 401, `{"error":"invalid_client","error_description":"refresh token invalid"}`, ErrLoginExpired, true, "", "invalid_client"},
		{"a refusal code with a 5xx", 500, `{"error":"invalid_grant"}`, ErrLoginExpired, true, "", "HTTP 500 from Schwab: invalid_grant"},
		{"wrong app key or secret", 401, `{"error":"invalid_client","error_description":"Unauthorized"}`, ErrAppRejected, false, "", "HTTP 401 from Schwab: invalid_client"},
		{"a bare 401", 401, ``, ErrAppRejected, false, "", "HTTP 401"},
		{"a bare 400", 400, `{"message":"bad request"}`, ErrLoginExpired, false, "", "bad request"},
		{"a gateway's page with a 400", 400, `<html><title>Bad Request</title></html>`, ErrLoginExpired, false, "", "an HTML page, Bad Request"},
		{"a server error", 503, `{"error":"server_error"}`, nil, false, "", "refreshing the access token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newError(tc.status, http.Header{}, []byte(tc.body))
			final, err := refreshError(e)
			if tc.want != nil && !errors.Is(err, tc.want) || tc.want == nil && (errors.Is(err, ErrLoginExpired) || errors.Is(err, ErrAppRejected)) {
				t.Errorf("got %v, want %v", err, tc.want)
			}
			if final != tc.final {
				t.Errorf("final = %v, want %v", final, tc.final)
			}
			if e.Detail != tc.wantDetail || !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("got %q with detail %q, want %q in it and detail %q", err, e.Detail, tc.wantText, tc.wantDetail)
			}
			if strings.Contains(err.Error(), "tokenDigest") {
				t.Errorf("the message shows the token digest: %v", err)
			}
		})
	}
}

// Where the file system cannot lock files, the login works without the
// lock, as it did before there was one, and says so once.
func TestUnsupportedFileSystem(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	var warnings []string
	a, err := NewAuth(Config{
		AppKey: testKey, AppSecret: testSecret, BaseURL: f.srv.URL, TokenFile: tokenFile, Now: c.Now,
		Warn: func(msg string) { warnings = append(warnings, msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	unsupported := fmt.Errorf("%w: flock: no locks available", filelock.ErrUnsupported)
	a.lock = func(context.Context, string, time.Duration) (*filelock.Lock, error) { return nil, unsupported }
	a.tryLock = func(string) (*filelock.Lock, bool, error) { return nil, false, unsupported }

	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	for range 2 {
		c.advance(time.Hour)
		if _, err := a.AccessToken(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "cannot be locked") || !strings.Contains(warnings[0], "local disk") {
		t.Errorf("warnings %q, want one saying that the file cannot be locked", warnings)
	}
}

// Every Auth on one file in a process waits for the others, even where the
// lock file would not keep out a second open in the same process, as on
// some network volumes.
func TestOneRefreshPerProcessWithoutFileLocks(t *testing.T) {
	f := newFakeOAuth(t)
	f.delay = 50 * time.Millisecond
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	var wg sync.WaitGroup
	for range 6 {
		a := newTestAuth(t, f, tokenFile, c)
		a.lock = func(context.Context, string, time.Duration) (*filelock.Lock, error) {
			return nil, filelock.ErrUnsupported // the OS lets every open in
		}
		wg.Go(func() {
			if _, err := a.AccessToken(t.Context()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, refresh := f.grants(); refresh != 1 {
		t.Errorf("%d refresh grants, want 1", refresh)
	}
}

// A token file that others can read, such as one copied in by hand, is
// made private, with a warning.
func TestTokenFileOthersCanReadIsMadePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no Unix permission bits on Windows")
	}
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	a, err := NewAuth(Config{
		AppKey: testKey, AppSecret: testSecret, BaseURL: f.srv.URL, TokenFile: tokenFile, Now: c.Now,
		Warn: func(msg string) { warnings = append(warnings, msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Status(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(tokenFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the token file's mode is %v (%v), want 0600", info.Mode().Perm(), err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "readable only by you") {
		t.Errorf("warnings %q, want one saying that the file is now private", warnings)
	}
}

// A token's lifetime counts from before the request for it, so that a slow
// answer does not stretch it.
func TestLifetimeCountsFromTheRequest(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	f.onGrant = func() { c.advance(20 * time.Second) }
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	a := newTestAuth(t, f, tokenFile, c)
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	saved := readTestToken(t, tokenFile)
	if !saved.AccessExpiresAt.Equal(testNow.Add(30*time.Minute)) || !saved.RefreshObtainedAt.Equal(testNow) {
		t.Errorf("login saved with the access token expiring at %v and the login at %v; want %v and %v",
			saved.AccessExpiresAt, saved.RefreshObtainedAt, testNow.Add(30*time.Minute), testNow)
	}
	c.advance(time.Hour)
	asked := c.Now()
	if _, err := a.AccessToken(t.Context()); err != nil {
		t.Fatal(err)
	}
	if saved := readTestToken(t, tokenFile); !saved.AccessExpiresAt.Equal(asked.Add(30 * time.Minute)) {
		t.Errorf("refresh saved the access token expiring at %v, want %v", saved.AccessExpiresAt, asked.Add(30*time.Minute))
	}
}

// A clock set back since the last login, as on a PC that starts with its
// clock hours ahead until it syncs, neither keeps the old login over a new
// one nor stops a 401 from leading to a refresh.
func TestClockSetBack(t *testing.T) {
	f := newFakeOAuth(t)
	c := &clock{now: testNow.Add(5 * time.Hour)} // ahead
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	if err := newTestAuth(t, f, tokenFile, c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	c.now = testNow // set right
	a := newTestAuth(t, f, tokenFile, c)

	// The access token looks fresh by the file, but Schwab has expired it.
	f.mu.Lock()
	clear(f.access)
	f.mu.Unlock()
	if tok := call(t, f, a); tok != "access-2" {
		t.Errorf("after a 401 for a token stamped in the future: %q, want a refreshed access-2", tok)
	}

	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	if saved := readTestToken(t, tokenFile); saved.RefreshToken != "refresh-3" || !saved.RefreshObtainedAt.Equal(testNow) {
		t.Errorf("after a new login the file holds %s from %v, want the new login's refresh-3 from %v", saved.RefreshToken, saved.RefreshObtainedAt, testNow)
	}
}

// A relative token file is the file that the system opens for it: with ".."
// after a link, that is next to the link's target, whatever the shell calls
// the working directory, as the scheduler scripts' cd -P and a shell's cd
// differ.
func TestRelativeTokenFileThroughALink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows takes .. by name")
	}
	f := newFakeOAuth(t)
	c := &clock{now: testNow}
	dir := t.TempDir()
	proj := filepath.Join(dir, "real", "proj")
	if err := os.MkdirAll(proj, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(proj, link); err != nil {
		t.Fatal(err)
	}
	if err := newTestAuth(t, f, filepath.Join(dir, "real", "token.json"), c).Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	t.Chdir(link) // sets PWD to the link, as a shell's cd does
	for _, name := range []string{"../token.json", "../cfg/../token.json"} {
		st, err := newTestAuth(t, f, name, c).Status()
		want, _ := filepath.EvalSymlinks(filepath.Join(dir, "real"))
		if err != nil || !st.LoggedIn || st.TokenFile != filepath.Join(want, "token.json") {
			t.Errorf("%s from the link: %+v, %v; want the login in %s", name, st, err, filepath.Join(want, "token.json"))
		}
	}
	// A file not made yet, below a folder not made yet, under the link.
	st, err := newTestAuth(t, f, "new/../../new.json", c).Status()
	want, _ := filepath.EvalSymlinks(filepath.Join(dir, "real"))
	if err != nil || st.TokenFile != filepath.Join(want, "new.json") {
		t.Errorf("a path that does not exist yet: %s, %v; want %s", st.TokenFile, err, filepath.Join(want, "new.json"))
	}
}
