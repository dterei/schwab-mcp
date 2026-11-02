package schwab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
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
// the lock, says so and keeps it until its stdin closes.
func helper(role string) int {
	ctx := context.Background()
	tokenFile := os.Getenv("SCHWAB_TEST_TOKEN_FILE")
	switch role {
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
		l, err := lockFile(ctx, tokenFile+".lock", time.Minute)
		if err == nil {
			fmt.Println("locked")
			_, _ = io.Copy(io.Discard, os.Stdin)
			l.release()
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

	if err := a.Load(t.Context()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("Load before any login: %v, want ErrNotLoggedIn", err)
	}
	if _, err := os.Stat(tokenFile + ".lock"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a run without a login left a lock file: %v", err)
	}
	if err := a.Login(t.Context(), testCode); err != nil {
		t.Fatal(err)
	}
	if got := a.LoginExpiresAt(); !got.Equal(testNow.Add(LoginLifetime)) {
		t.Errorf("login expires at %v, want %v", got, testNow.Add(LoginLifetime))
	}
	if tok := call(t, f, a); tok != "access-1" {
		t.Errorf("access token %q, want access-1", tok)
	}
	if code, refresh := f.grants(); code != 1 || refresh != 0 {
		t.Errorf("%d code and %d refresh grants, want 1 and 0", code, refresh)
	}

	// Another process reads the same login from the file.
	b := newTestAuth(t, f, tokenFile, c)
	if err := b.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
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
	f.retire("refresh-1") // the new login replaced the old one

	if tok := call(t, f, server); tok != "access-2" {
		t.Errorf("the server got %q, want the new login's access-2", tok)
	}
	if _, refresh := f.grants(); refresh != 0 {
		t.Errorf("%d refresh grants, want 0: the new login's access token works", refresh)
	}
	if got, want := server.LoginExpiresAt(), testNow.Add(24*time.Hour+LoginLifetime); !got.Equal(want) {
		t.Errorf("the server thinks the login expires at %v, want %v", got, want)
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
	if !lockSupported {
		t.Skip("no file locking on " + runtime.GOOS)
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
	if !lockSupported {
		t.Skip("no file locking on " + runtime.GOOS)
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
	a.lockWait = 100 * time.Millisecond
	if _, err := a.AccessToken(t.Context()); err == nil || !strings.Contains(err.Error(), "waiting for another process") {
		t.Errorf("waiting past the limit: %v, want it to say it waited for another process", err)
	}

	// Once the holder lets go, the refresh goes ahead.
	a.lockWait = defaultLockWait
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
		err := newTestAuth(t, f, path, c).Load(t.Context())
		if err == nil || !strings.Contains(err.Error(), "run `schwabtx login` again") {
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
	var se *Error
	if !errors.Is(err, ErrLoginExpired) || !errors.As(err, &se) || se.Status != http.StatusBadRequest ||
		!strings.HasPrefix(err.Error(), "Schwab rejected the saved login (it expires 7 days after you log in); run `schwabtx login` again: HTTP 400") {
		t.Errorf("stale refresh token: %v, want ErrLoginExpired with the HTTP 400", err)
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
	if _, err := wrong.AccessToken(t.Context()); !errors.Is(err, ErrAppRejected) || !strings.Contains(err.Error(), "SCHWAB_APP_SECRET") {
		t.Errorf("wrong secret: %v, want ErrAppRejected", err)
	}

	for _, cfg := range []Config{{AppKey: testKey, TokenFile: stale}, {AppKey: testKey, AppSecret: testSecret}} {
		if _, err := NewAuth(cfg); err == nil {
			t.Errorf("NewAuth(%+v) took an incomplete config", cfg)
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
	if err := a.Load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 10, 10, 9, 15, 0, 500e6, time.FixedZone("", -7*3600)); !a.LoginExpiresAt().Equal(want) {
		t.Errorf("login expires at %v, want %v", a.LoginExpiresAt(), want)
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
		if _, ok := saved[key]; !ok || len(saved) != 4 {
			t.Errorf("saved %s, want the four fields schwabtx has always written", b)
			break
		}
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
