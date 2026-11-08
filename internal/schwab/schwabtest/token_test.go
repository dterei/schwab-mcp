package schwabtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A clock is a fake's clock that a test moves.
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

// newClock gives s a clock of its own and returns it.
func newClock(s *Server) *clock {
	c := &clock{now: time.Date(2026, 11, 8, 15, 4, 5, 0, time.UTC)}
	s.SetNow(c.Now)
	return c
}

// works reports whether access is an access token that the API takes.
func works(t *testing.T, s *Server, access string) bool {
	t.Helper()
	resp, _ := get(t, s, "/trader/v1/test", access)
	return resp.StatusCode == http.StatusOK
}

// refused checks that the token endpoint answers form with 400 and body,
// as JSON without correlation headers.
func refused(t *testing.T, s *Server, form url.Values, body string) {
	t.Helper()
	resp, got := postToken(t, s, form)
	if resp.StatusCode != http.StatusBadRequest || got != body || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("%v: %d %s (%s), want 400 %s", form, resp.StatusCode, got, resp.Header.Get("Content-Type"), body)
	}
	if resp.Header.Get("Schwab-Client-CorrelId") != "" || resp.Header.Get("X-Request-Id") != "" {
		t.Errorf("%v: correlation headers %v", form, resp.Header)
	}
}

// A grant answers as Schwab documents, its keys in Schwab's order and no
// correlation headers; a login is AccessToken(1), RefreshToken(1) and
// IDToken(1); and Code works again.
func TestLogin(t *testing.T) {
	s := New(t)
	resp, body := postToken(t, s, codeForm(Code))
	want := `{"expires_in":1800,"token_type":"Bearer","scope":"api","refresh_token":"refresh-1-_=@",` +
		`"access_token":"I0.access-1-_=@","id_token":"` + IDToken(1) + `"}`
	if resp.StatusCode != http.StatusOK || body != want || resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("login: %d %s (%s), want %s", resp.StatusCode, body, resp.Header.Get("Content-Type"), want)
	}
	for k := range resp.Header {
		if strings.EqualFold(k, "Schwab-Client-CorrelId") || strings.EqualFold(k, "X-Request-Id") {
			t.Errorf("a token answer carries %s", k)
		}
	}
	if !works(t, s, AccessToken(1)) {
		t.Error("the login's access token does not work")
	}
	if g := granted(t, s, codeForm(Code)); g.AccessToken != AccessToken(2) || g.RefreshToken != RefreshToken(2) {
		t.Errorf("a second login with Code: %+v", g)
	}
	if code, refresh := s.Grants(); code != 2 || refresh != 0 {
		t.Errorf("Grants() = %d, %d", code, refresh)
	}
}

// A refresh gives a new access token and the same refresh token, escaped
// as JSON whatever it holds, without ending the older access tokens,
// unless the fake rotates or revokes; a login ends every earlier refresh
// token.
func TestRefresh(t *testing.T) {
	s := New(t)
	g := granted(t, s, codeForm(Code))
	r := granted(t, s, refreshForm(g.RefreshToken))
	if r.AccessToken != AccessToken(2) || r.RefreshToken != g.RefreshToken || r.IDToken != IDToken(2) {
		t.Errorf("refresh: %+v", r)
	}
	if !works(t, s, AccessToken(1)) || !works(t, s, AccessToken(2)) {
		t.Error("a refresh ended an older access token")
	}
	if code, refresh := s.Grants(); code != 1 || refresh != 1 {
		t.Errorf("Grants() = %d, %d", code, refresh)
	}

	login := granted(t, s, codeForm(Code))
	refused(t, s, refreshForm(g.RefreshToken), Refusal2026)
	granted(t, s, refreshForm(login.RefreshToken))

	s.SetRotate(true)
	rotated := granted(t, s, refreshForm(login.RefreshToken))
	if rotated.RefreshToken != RefreshToken(5) {
		t.Errorf("a rotating refresh gave %s, want %s", rotated.RefreshToken, RefreshToken(5))
	}
	refused(t, s, refreshForm(login.RefreshToken), Refusal2026)
	s.SetRotate(false)

	s.SetRevokeOlder(true)
	latest := granted(t, s, refreshForm(rotated.RefreshToken))
	if works(t, s, AccessToken(5)) || !works(t, s, latest.AccessToken) {
		t.Error("SetRevokeOlder(true) kept an older access token, or ended the new one")
	}

	odd := `r"1\2`
	s.AllowRefresh(odd)
	if g := granted(t, s, refreshForm(odd)); g.RefreshToken != odd {
		t.Errorf("a refresh with %s gave back %s", odd, g.RefreshToken)
	}
}

// What the token endpoint refuses, and how, the app before the type of the
// body. A guess says so in its own text, and none holds "refresh token",
// which the client takes for a lapsed login.
func TestTokenRefusals(t *testing.T) {
	s := New(t)
	g := granted(t, s, codeForm(Code))
	badClient := `{"error":"invalid_client","error_description":"Unauthorized"}`
	for _, tc := range []struct {
		name, key, secret, contentType string
	}{
		{"a wrong secret", AppKey, "wrong", "application/x-www-form-urlencoded"},
		{"a wrong key", "wrong", AppSecret, "application/x-www-form-urlencoded"},
		{"no Basic auth", "", "", "application/x-www-form-urlencoded"},
		{"a wrong secret and a body of another type", AppKey, "wrong", "application/json"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(refreshForm(g.RefreshToken).Encode()))
		if err != nil {
			t.Fatal(err)
		}
		if tc.key != "" {
			req.SetBasicAuth(tc.key, tc.secret)
		}
		req.Header.Set("Content-Type", tc.contentType)
		if resp, body := send(t, s, req); resp.StatusCode != http.StatusUnauthorized || body != badClient {
			t.Errorf("%s: %d %s", tc.name, resp.StatusCode, body)
		}
	}
	for _, ct := range []string{"application/json", "", "application/x-www-form-urlencoded; charset=UTF-8"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(codeForm(Code).Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(AppKey, AppSecret)
		req.Header.Set("Content-Type", ct)
		resp, body := send(t, s, req)
		if ct == "application/x-www-form-urlencoded; charset=UTF-8" {
			if resp.StatusCode != http.StatusOK {
				t.Errorf("Content-Type %q: %d %s", ct, resp.StatusCode, body)
			}
			continue
		}
		if resp.StatusCode != http.StatusBadRequest ||
			body != `{"error":"invalid_request","error_description":"a guess: Schwab's answer to this content type is not recorded"}` {
			t.Errorf("Content-Type %q: %d %s", ct, resp.StatusCode, body)
		}
	}
	badCode := `{"error":"invalid_grant","error_description":"a guess: Schwab's answer to a bad code is not recorded"}`
	refused(t, s, codeForm("C0.abc123%40"), badCode)
	refused(t, s, codeForm(""), badCode)
	wrongCallback := codeForm(Code)
	wrongCallback.Set("redirect_uri", CallbackURL+"/")
	refused(t, s, wrongCallback, badCode)
	refused(t, s, url.Values{"grant_type": {"client_credentials"}}, `{"error":"unsupported_grant_type","error_description":"a guess: Schwab's answer is not recorded"}`)
	refused(t, s, refreshForm("never issued"), Refusal2026)
	s.SetRefusal(Refusal2024)
	refused(t, s, refreshForm("never issued"), Refusal2024)
	s.SetRefusal("")
	refused(t, s, refreshForm("never issued"), Refusal2026)

	for _, body := range []string{
		badClient, badCode, unknownClientPage, invalidURIPage, responseTypePage,
		`{"error":"invalid_request","error_description":"a guess: Schwab's answer to this content type is not recorded"}`,
		`{"error":"unsupported_grant_type","error_description":"a guess: Schwab's answer is not recorded"}`,
	} {
		if strings.Contains(strings.ToLower(body), "refresh token") {
			t.Errorf("%s says refresh token", body)
		}
	}
	if code, refresh := s.Grants(); code != 2 || refresh != 0 {
		t.Errorf("Grants() = %d, %d, want only the two logins", code, refresh)
	}
}

// Tokens and codes lapse on the fake's clock: an access token after
// expires_in, a login's refresh tokens 7 days after it however often they
// are used, an authorize page's code 30 seconds after it was issued.
func TestLapses(t *testing.T) {
	s := New(t)
	c := newClock(s)
	g := granted(t, s, codeForm(Code))
	c.advance(1800*time.Second - time.Nanosecond)
	if !works(t, s, g.AccessToken) {
		t.Error("an access token lapsed before its 1800 seconds")
	}
	c.advance(time.Nanosecond)
	if works(t, s, g.AccessToken) {
		t.Error("an access token works after its 1800 seconds")
	}

	// A rotated refresh token keeps its login's 7 days.
	s.SetRotate(true)
	r := granted(t, s, refreshForm(g.RefreshToken))
	c.advance(7*24*time.Hour - 1800*time.Second - time.Nanosecond)
	r = granted(t, s, refreshForm(r.RefreshToken))
	c.advance(time.Nanosecond)
	refused(t, s, refreshForm(r.RefreshToken), Refusal2026)

	s.AllowRefresh("allowed")
	c.advance(7*24*time.Hour - time.Nanosecond)
	granted(t, s, refreshForm("allowed"))
	c.advance(time.Nanosecond)
	refused(t, s, refreshForm("allowed"), Refusal2026)

	code := authorizeCode(t, s, url.Values{})
	c.advance(30*time.Second - time.Nanosecond)
	granted(t, s, codeForm(code))
	code = authorizeCode(t, s, url.Values{})
	c.advance(30 * time.Second)
	refused(t, s, codeForm(code), `{"error":"invalid_grant","error_description":"a guess: Schwab's answer to a bad code is not recorded"}`)
}

// SetExpiresIn sets what grants advertise and when their tokens lapse,
// 1800 seconds for a value of 0 or less, as the client takes it; SetScope
// sets the scope advertised.
func TestExpiresInAndScope(t *testing.T) {
	s := New(t)
	c := newClock(s)
	s.SetExpiresIn(60)
	s.SetScope(`api "readonly"`)
	resp, body := postToken(t, s, codeForm(Code))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, `{"expires_in":60,"token_type":"Bearer","scope":"api \"readonly\"",`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	c.advance(time.Minute)
	if works(t, s, AccessToken(1)) {
		t.Error("an access token works after the expires_in set")
	}
	for _, n := range []int{0, -5} {
		s.SetExpiresIn(n)
		g := granted(t, s, codeForm(Code))
		if g.ExpiresIn != n {
			t.Errorf("SetExpiresIn(%d): advertised %d", n, g.ExpiresIn)
		}
		c.advance(1800*time.Second - time.Nanosecond)
		if !works(t, s, g.AccessToken) {
			t.Errorf("SetExpiresIn(%d): the token lapsed before 1800 seconds", n)
		}
		c.advance(time.Nanosecond)
		if works(t, s, g.AccessToken) {
			t.Errorf("SetExpiresIn(%d): the token works after 1800 seconds", n)
		}
	}
}

// RevokeAccess ends every access token and no refresh token, and EndLogin
// every refresh token and no access token, which works on until it lapses.
// AllowRefresh makes a refresh token work.
func TestEndLoginAndRevokeAccess(t *testing.T) {
	r := &recorder{TB: t}
	s := New(r)
	g := granted(t, s, codeForm(Code))
	s.AllowRefresh("one", "two")
	granted(t, s, refreshForm("two"))
	s.RevokeAccess()
	if works(t, s, g.AccessToken) || works(t, s, AccessToken(2)) {
		t.Error("an access token works after RevokeAccess")
	}
	later := granted(t, s, refreshForm("one"))
	s.EndLogin()
	if !works(t, s, later.AccessToken) {
		t.Error("an access token stopped working at EndLogin")
	}
	for _, tok := range []string{g.RefreshToken, "one", "two"} {
		refused(t, s, refreshForm(tok), Refusal2026)
	}
	wantReported(t, r, "AllowRefresh of an empty token", func() { s.AllowRefresh("three", "") })
	refused(t, s, refreshForm("three"), Refusal2026)
}

// FailTokens answers the next token requests, in order, before anything
// else, each as it says; the requests count; a Handler runs without the
// lock; a cut over HTTP/2 lets go of it too.
func TestFailTokens(t *testing.T) {
	r := &recorder{TB: t}
	s := New(r)
	hung := make(chan struct{})
	s.FailTokens(
		Failure{Status: 503, RetryAfter: "3", Header: http.Header{"Schwab-Client-CorrelID": {"c-1"}}},
		Failure{Status: 401, Body: `{"fault":{}}`},
		Failure{Status: 401, EmptyBody: true},
		Failure{Status: 502, Body: "<html></html>", Header: http.Header{"Content-Type": {"text/html"}}},
		Failure{Handler: func(w http.ResponseWriter, r *http.Request) {
			s.TokenRequests() // the lock is not held
			close(hung)
			Hang(w, r)
		}},
	)
	for i, want := range []struct {
		status        int
		body, ct, hdr string
	}{
		{503, `{"error":"server_error"}`, "application/json", "c-1"},
		{401, `{"fault":{}}`, "application/json", ""},
		{401, "", "", ""},
		{502, "<html></html>", "text/html", ""},
	} {
		resp, body := postToken(t, s, codeForm(Code))
		if resp.StatusCode != want.status || body != want.body || resp.Header.Get("Content-Type") != want.ct ||
			resp.Header.Get("Schwab-Client-CorrelId") != want.hdr {
			t.Errorf("failure %d: %d %q %v", i, resp.StatusCode, body, resp.Header)
		}
		if ra := resp.Header.Get("Retry-After"); (i == 0) != (ra == "3") {
			t.Errorf("failure %d: Retry-After %q", i, ra)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(codeForm(Code).Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(AppKey, AppSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	done := make(chan error, 1)
	go func() {
		resp, err := s.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-hung:
	case err := <-done:
		t.Fatalf("the hanging failure answered: %v", err)
	}
	granted(t, s, codeForm(Code)) // while the handler hangs, without the lock
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("the hanging failure: %v, want it canceled", err)
	}
	if n := s.TokenRequests(); n != 6 {
		t.Errorf("TokenRequests() = %d, want 6", n)
	}
	if code, _ := s.Grants(); code != 1 {
		t.Errorf("%d code grants, want only the last", code)
	}
	wantReported(t, r, "status 42", func() { s.FailTokens(Failure{Status: 503}, Failure{Status: 42}) })
	wantReported(t, r, "a Body and EmptyBody", func() { s.FailTokens(Failure{Status: 401, Body: "x", EmptyBody: true}) })
	granted(t, s, codeForm(Code))

	for _, tc := range []struct {
		name string
		new  func(testing.TB) *Server
	}{{"HTTP/1", New}, {"HTTP/2", NewTLS}} {
		t.Run("a cut over "+tc.name, func(t *testing.T) {
			s := tc.new(t)
			s.FailTokens(Failure{})
			tr := s.Client().Transport.(*http.Transport).Clone()
			tr.DisableKeepAlives = true
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(codeForm(Code).Encode()))
			if err != nil {
				t.Fatal(err)
			}
			req.SetBasicAuth(AppKey, AppSecret)
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			resp, err := (&http.Client{Transport: tr}).Do(req)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if err == nil {
				t.Errorf("a cut answered %d", resp.StatusCode)
			}
			granted(t, s, codeForm(Code))
		})
	}
}

// grantErr posts form to the token endpoint as postToken does, and returns
// why the answer is not a grant. Unlike granted it does not end the test,
// so a goroutine of the test's may call it.
func grantErr(ctx context.Context, s *Server, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.SetBasicAuth(AppKey, AppSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.Client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %d", form.Get("grant_type"), resp.StatusCode)
	}
	return nil
}

// OnToken runs as a token request arrives, without the lock; OnGrant as a
// grant is made, under it, once per grant; SetTokenDelay holds each token
// request up, without the lock.
func TestTokenHooks(t *testing.T) {
	r := &recorder{TB: t}
	s := New(r)
	var mu sync.Mutex
	var tokens []int
	grants := 0
	seen := func() ([]int, int) {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(tokens), grants
	}
	s.OnToken(func() {
		n := s.TokenRequests() // the lock is not held
		mu.Lock()
		defer mu.Unlock()
		tokens = append(tokens, n)
	})
	s.OnGrant(func() {
		mu.Lock()
		defer mu.Unlock()
		grants++
		if s.mu.TryLock() {
			s.mu.Unlock()
			t.Error("OnGrant ran without the lock")
		}
	})
	g := granted(t, s, codeForm(Code))
	refused(t, s, refreshForm("never issued"), Refusal2026)
	granted(t, s, refreshForm(g.RefreshToken))
	if tokens, grants := seen(); !slices.Equal(tokens, []int{0, 1, 2}) || grants != 2 {
		t.Errorf("OnToken saw %v and OnGrant ran %d times, want [0 1 2] and 2", tokens, grants)
	}
	s.OnToken(nil)
	s.OnGrant(nil)

	s.SetTokenDelay(50 * time.Millisecond)
	start := time.Now()
	granted(t, s, codeForm(Code))
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("a token request took %v with a delay of 50ms", d)
	}
	// The delay is slept without the lock, so two requests wait it out
	// together; one after the other they would take twice as long.
	const delay = 300 * time.Millisecond
	s.SetTokenDelay(delay)
	start = time.Now()
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- grantErr(t.Context(), s, codeForm(Code)) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
	if d := time.Since(start); d >= 2*delay {
		t.Errorf("two token requests with a delay of %v took %v, as if one waited for the other", delay, d)
	}
	wantReported(t, r, "SetTokenDelay(-1ns)", func() { s.SetTokenDelay(-1) })
	if tokens, grants := seen(); len(tokens) != 3 || grants != 2 {
		t.Error("a hook set to nil still runs")
	}
}

// The tokens hold the characters Schwab's do, and an id token is shaped as
// a JWT.
func TestTokenForms(t *testing.T) {
	for _, tok := range []string{AccessToken(3), RefreshToken(3), AuthCode(3), Code} {
		if !strings.ContainsAny(tok, ".-_=@") {
			t.Errorf("%s holds none of Schwab's characters", tok)
		}
	}
	if AccessToken(3) != "I0.access-3-_=@" || RefreshToken(3) != "refresh-3-_=@" || AuthCode(3) != "C0.code-3-_=@" {
		t.Errorf("%s %s %s", AccessToken(3), RefreshToken(3), AuthCode(3))
	}
	parts := strings.Split(IDToken(3), ".")
	if len(parts) != 3 {
		t.Fatalf("IDToken(3) = %s", IDToken(3))
	}
	var header struct{ Alg, Typ string }
	var claims struct{ Sub string }
	for i, v := range []any{&header, &claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil || json.Unmarshal(b, v) != nil {
			t.Errorf("part %d of IDToken(3): %q, %v", i, b, err)
		}
	}
	if sig, err := base64.RawURLEncoding.DecodeString(parts[2]); header.Typ != "JWT" || claims.Sub != "3" || err != nil || string(sig) != "schwabtest-3" {
		t.Errorf("IDToken(3): %+v %+v %q", header, claims, sig)
	}
}

// Secrets lists every token and code the fake issued or took, with the
// app's secret and Code, and not the app key.
func TestSecrets(t *testing.T) {
	s := New(t)
	s.SetRotate(true)
	g := granted(t, s, codeForm(authorizeCode(t, s, url.Values{})))
	r := granted(t, s, refreshForm(g.RefreshToken))
	s.AllowRefresh("allowed")
	want := []string{
		AppSecret, Code, AuthCode(1), g.RefreshToken, g.AccessToken, g.IDToken,
		r.RefreshToken, r.AccessToken, r.IDToken, "allowed",
	}
	got := s.Secrets()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Secrets() = %q, want %q", got, want)
	}
	if slices.Contains(got, AppKey) {
		t.Error("Secrets() holds the app key")
	}
}

// authorizeCode follows the authorize page with the fake's app and query
// added, and returns the code its redirect carries.
func authorizeCode(t *testing.T, s *Server, q url.Values) string {
	t.Helper()
	q.Set("client_id", AppKey)
	q.Set("redirect_uri", CallbackURL)
	q.Set("response_type", "code")
	resp, _ := get(t, s, "/v1/oauth/authorize?"+q.Encode(), "")
	loc, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil {
		t.Fatalf("authorize: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	return loc.Query().Get("code")
}

// The authorize page sends the browser to the callback with a code of its
// own count, which works once, a session ID from the counter, and the
// state sent, escaped; it refuses an unknown app, callback or response
// type with a page that says it is a guess.
func TestAuthorize(t *testing.T) {
	s := New(t)
	for i, state := range []string{"a&b", "a#b", "a+b", "a%b", "a b", "", "x=y&code=no"} {
		q := url.Values{"client_id": {AppKey}, "redirect_uri": {CallbackURL}, "response_type": {"code"}, "state": {state}}
		resp, body := get(t, s, "/v1/oauth/authorize?"+q.Encode(), "")
		loc := resp.Header.Get("Location")
		u, err := url.Parse(loc)
		if resp.StatusCode != http.StatusFound || err != nil || body != "" {
			t.Fatalf("state %q: %d %s %s", state, resp.StatusCode, loc, body)
		}
		wantSession := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		if got := u.Query(); u.Scheme+"://"+u.Host != CallbackURL || u.Path != "/" || got.Get("state") != state ||
			got.Get("code") != AuthCode(i+1) || got.Get("session") != wantSession || len(got) != 3 {
			t.Errorf("state %q: Location %s", state, loc)
		}
		if !strings.HasPrefix(u.RawQuery, "code=C0.code-") || !strings.Contains(u.RawQuery, "%40&session=") {
			t.Errorf("state %q: the query %s is not in Schwab's order, with the @ as %%40", state, u.RawQuery)
		}
		for k := range resp.Header {
			if strings.EqualFold(k, "Schwab-Client-CorrelId") || strings.EqualFold(k, "X-Request-Id") {
				t.Errorf("the authorize page carries %s", k)
			}
		}
	}
	q := url.Values{"client_id": {AppKey}, "redirect_uri": {CallbackURL}, "response_type": {"code"}}
	resp, _ := get(t, s, "/v1/oauth/authorize?"+q.Encode(), "")
	if u, err := url.Parse(resp.Header.Get("Location")); err != nil || u.Query().Has("state") {
		t.Errorf("without a state: %s", resp.Header.Get("Location"))
	}

	code := authorizeCode(t, s, url.Values{})
	if g := granted(t, s, codeForm(code)); g.AccessToken != AccessToken(1) {
		t.Errorf("a login through the page got %s, want %s", g.AccessToken, AccessToken(1))
	}
	refused(t, s, codeForm(code), `{"error":"invalid_grant","error_description":"a guess: Schwab's answer to a bad code is not recorded"}`)

	for _, tc := range []struct {
		name, key, value string
		status           int
		page             string
	}{
		{"an unknown app", "client_id", "other", http.StatusUnauthorized, unknownClientPage},
		{"no app", "client_id", "", http.StatusUnauthorized, unknownClientPage},
		{"a callback with a slash", "redirect_uri", CallbackURL + "/", http.StatusBadRequest, invalidURIPage},
		{"another response type", "response_type", "token", http.StatusBadRequest, responseTypePage},
	} {
		q := url.Values{"client_id": {AppKey}, "redirect_uri": {CallbackURL}, "response_type": {"code"}}
		q.Set(tc.key, tc.value)
		resp, body := get(t, s, "/v1/oauth/authorize?"+q.Encode(), "")
		if resp.StatusCode != tc.status || body != tc.page || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" ||
			resp.Header.Get("Location") != "" || !strings.Contains(body, "the fake's guess") {
			t.Errorf("%s: %d %s", tc.name, resp.StatusCode, body)
		}
	}
	if !strings.Contains(invalidURIPage, "Error - invalid URI specified") {
		t.Errorf("the page for a bad callback is not Schwab's text: %s", invalidURIPage)
	}
}
