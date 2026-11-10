package schwabtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// recorder is a testing.TB that keeps what the fake reports with Errorf,
// so that a test can check that bad input is reported.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

// took returns what was reported since the last call.
func (r *recorder) took() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	errs := r.errs
	r.errs = nil
	return errs
}

// wantReported checks that f reports one error that says want, and none
// otherwise.
func wantReported(t *testing.T, r *recorder, want string, f func()) {
	t.Helper()
	f()
	if errs := r.took(); len(errs) != 1 || !strings.Contains(errs[0], want) {
		t.Errorf("reported %q, want one error that says %q", errs, want)
	}
}

// A reply is an answer of the fake, whose body send has read.
type reply struct {
	StatusCode int
	Header     http.Header
	Proto      string
	ProtoMajor int
}

// send sends req to the fake, following no redirect, and returns the
// answer with its body.
func send(t *testing.T, s *Server, req *http.Request) (reply, string) {
	t.Helper()
	c := *s.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading: %v", req.Method, req.URL, err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header, Proto: resp.Proto, ProtoMajor: resp.ProtoMajor}, string(b)
}

// get sends GET path, with token as its access token unless it is empty.
func get(t *testing.T, s *Server, path, token string) (reply, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return send(t, s, req)
}

// A grant is the token endpoint's answer to a grant.
type grant struct {
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
	Scope        string `json:"scope"`
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
}

// postToken posts form to the token endpoint with the fake's app key and
// secret.
func postToken(t *testing.T, s *Server, form url.Values) (reply, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(AppKey, AppSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return send(t, s, req)
}

// granted posts form and returns the grant, failing the test unless it is
// one.
func granted(t *testing.T, s *Server, form url.Values) grant {
	t.Helper()
	resp, body := postToken(t, s, form)
	var g grant
	if err := json.Unmarshal([]byte(body), &g); resp.StatusCode != http.StatusOK || err != nil {
		t.Fatalf("%s: %d %s (%v)", form.Get("grant_type"), resp.StatusCode, body, err)
	}
	return g
}

func codeForm(code string) url.Values {
	return url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {CallbackURL}}
}

func refreshForm(refresh string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
}

// login logs in to s with Code and returns the access token.
func login(t *testing.T, s *Server) string {
	t.Helper()
	return granted(t, s, codeForm(Code)).AccessToken
}

// An API request with a working token is recorded as it was sent: the
// escaped path, the raw query in its order and escapes, the token and the
// headers, and Requests returns copies.
func TestRequests(t *testing.T) {
	s := New(t)
	tok := login(t, s)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+"/trader/v1/accounts/AB%2FC%20D?x=%26&fields=positions&y=%2f", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Test", "1")
	send(t, s, req)
	get(t, s, "/trader/v1/test", tok)
	get(t, s, "/trader/v1/test", "")
	get(t, s, "/trader/v1/no/such/path", tok)

	rs := s.Requests()
	if len(rs) != 3 {
		t.Fatalf("%d requests recorded, want 3: %+v", len(rs), rs)
	}
	r := rs[0]
	if r.Method != http.MethodGet || r.Route != AccountRoute || r.Path != "/trader/v1/accounts/AB%2FC%20D" ||
		r.Query != "x=%26&fields=positions&y=%2f" || r.URL != "/trader/v1/accounts/AB%2FC%20D?x=%26&fields=positions&y=%2f" ||
		r.Token != tok || r.Header.Get("X-Test") != "1" {
		t.Errorf("recorded %+v", r)
	}
	if rs[1].Route != TestRoute || rs[2].Route != "" || rs[2].Path != "/trader/v1/no/such/path" {
		t.Errorf("recorded the routes %q and %q", rs[1].Route, rs[2].Route)
	}
	rs[0].Header.Set("X-Test", "changed")
	if again := s.Requests(); again[0].Header.Get("X-Test") != "1" {
		t.Error("a change to what Requests returned reached the fake")
	}
}

// APICalls counts API requests whether their token works or not, and
// AllRequests every request, the token endpoint's and those of paths that
// get no answer included.
func TestCounts(t *testing.T) {
	s := New(t)
	tok := login(t, s)
	get(t, s, "/trader/v1/test", tok)
	get(t, s, "/trader/v1/test", "nope")
	get(t, s, "/marketdata/v1/quotes", "")
	if resp, _ := get(t, s, "/elsewhere", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a path outside the API: %d", resp.StatusCode)
	}
	if n := s.APICalls(); n != 3 {
		t.Errorf("APICalls() = %d, want 3", n)
	}
	if n := s.AllRequests(); n != 5 {
		t.Errorf("AllRequests() = %d, want 5", n)
	}
	if n := len(s.Requests()); n != 1 {
		t.Errorf("%d requests recorded, want the 1 with a working token", n)
	}
}

// serve answers a GET of path in the test's process, so that the headers'
// names come as the fake spelled them, which a client's reading would
// canonicalize.
func serve(t *testing.T, s *Server, path, auth string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	s.srv.Config.Handler.ServeHTTP(rec, req)
	return rec
}

// spelled returns the values of the header that h holds under name, spelled
// as it is.
func spelled(h http.Header, name string) []string { return h[name] }

// wantFakeHeaders checks that h, as the fake spelled it, carries the
// counter's UUIDs, the correlation ID in the spelling Schwab's spec gives an
// answer of status, and JSON's Content-Type.
func wantFakeHeaders(t *testing.T, h http.Header, status int) {
	t.Helper()
	want, other := "Schwab-Client-CorrelId", "Schwab-Client-CorrelID"
	if status >= 400 {
		want, other = other, want
	}
	if v := h[want]; len(v) != 1 || !counterID.MatchString(v[0]) {
		t.Errorf("%s %q, want a UUID of the counter", want, v)
	}
	if v, ok := h[other]; ok {
		t.Errorf("%s %q as well, the spelling of another status", other, v)
	}
	if v := h["X-Request-Id"]; len(v) != 1 || !counterID.MatchString(v[0]) || v[0] == h[want][0] {
		t.Errorf("X-Request-Id %q, want another UUID of the counter", v)
	}
	if ct := h["Content-Type"]; len(ct) != 1 || ct[0] != "application/json" {
		t.Errorf("Content-Type %q", ct)
	}
}

// A request without a working token gets Schwab's 401, with an ID and
// correlation headers from the counter, is not recorded, and takes no
// Script entry.
func TestUnauthorized(t *testing.T) {
	s := New(t)
	tok := login(t, s)
	s.Script(Answer(http.StatusTeapot, "scripted"))
	ids := map[string]bool{}
	for _, tc := range []struct{ name, auth string }{
		{"no token", ""},
		{"a token never issued", "Bearer " + AccessToken(9)},
		{"a token in another scheme", "Basic " + tok},
		{"an empty token", "Bearer "},
	} {
		rec := serve(t, s, "/trader/v1/test", tc.auth)
		var got struct {
			Errors []struct {
				ID, Title, Detail string
				Status            int
			}
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || rec.Code != http.StatusUnauthorized ||
			len(got.Errors) != 1 || got.Errors[0].Status != 401 || got.Errors[0].Title != "Unauthorized" ||
			got.Errors[0].Detail != "Client not authorized" || !counterID.MatchString(got.Errors[0].ID) || ids[got.Errors[0].ID] {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body)
		}
		if len(got.Errors) == 1 {
			ids[got.Errors[0].ID] = true
		}
		wantFakeHeaders(t, rec.Header(), http.StatusUnauthorized)
	}
	if ids[cannedID] {
		t.Errorf("the 401 carries NotAuthorized's fixed ID")
	}
	if n := len(s.Requests()); n != 0 {
		t.Errorf("%d requests recorded", n)
	}
	if resp, body := get(t, s, "/trader/v1/test", tok); resp.StatusCode != http.StatusTeapot || body != "scripted" {
		t.Errorf("the scripted answer: %d %s", resp.StatusCode, body)
	}
}

// The fake's own answers carry the counter's UUIDs, each one new, and an
// API path without a Route gets a 404 that says it is the fake's.
func TestOwnAnswers(t *testing.T) {
	s := New(t)
	auth := "Bearer " + login(t, s)
	rec := serve(t, s, "/trader/v1/test", auth)
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` {
		t.Errorf("TestRoute: %d %s", rec.Code, rec.Body)
	}
	wantFakeHeaders(t, rec.Header(), http.StatusOK)
	first := spelled(rec.Header(), "Schwab-Client-CorrelId")

	rec = serve(t, s, "/marketdata/v1/quotes?symbols=X", auth)
	if rec.Code != http.StatusNotFound || rec.Body.String() != `{"message":"Not Found (the fake has no such route)"}` {
		t.Errorf("a path without a Route: %d %s", rec.Code, rec.Body)
	}
	wantFakeHeaders(t, rec.Header(), http.StatusNotFound)
	if slices.Equal(spelled(rec.Header(), "Schwab-Client-CorrelID"), first) {
		t.Error("two answers carry the same correlation ID")
	}
}

// Script answers the next API requests in order on any path, Queue the
// next requests to one Route, and Override every request to it, in that
// order; a nil entry of either is no answer.
func TestScriptQueueOverride(t *testing.T) {
	s := New(t)
	tok := login(t, s)
	status := func(path string) int {
		t.Helper()
		resp, _ := get(t, s, path, tok)
		return resp.StatusCode
	}
	s.Script(Answer(201, ""), nil, Answer(202, ""))
	s.Queue(TestRoute, Answer(203, ""), nil, Answer(204, ""))
	s.Override(TestRoute, Answer(205, ""))
	var got []int
	for range 7 {
		got = append(got, status("/trader/v1/test"))
	}
	// 201 from the script, then nil, which falls to the queue's 203; 202;
	// the queue's nil falls to the override; 204; then the override.
	if want := []int{201, 203, 202, 205, 204, 205, 205}; !slices.Equal(got, want) {
		t.Errorf("statuses %v, want %v", got, want)
	}
	s.Override(TestRoute, nil)
	if got := status("/trader/v1/test"); got != http.StatusOK {
		t.Errorf("after Override(nil): %d", got)
	}
	s.Script(Answer(206, ""))
	if got := status("/trader/v1/elsewhere"); got != 206 {
		t.Errorf("a script entry on a path without a Route: %d", got)
	}

	r := &recorder{TB: t}
	s = New(r)
	wantReported(t, r, "no such Route", func() { s.Queue("GET /trader/v1/orders", Answer(200, "")) })
	wantReported(t, r, "no such Route", func() { s.Override("GET /x", Answer(200, "")) })
}

// OverridePath answers the paths without a Route that its patterns match,
// with the pattern's Pattern and PathValue, and refuses patterns that a
// Route or another pattern would shadow.
func TestOverridePath(t *testing.T) {
	r := &recorder{TB: t}
	s := New(r)
	tok := login(t, s)
	orders := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Pattern+" "+r.PathValue("hash"))
	}
	s.OverridePath("GET /trader/v1/accounts/{hash}/orders", orders)
	s.OverridePath("/marketdata/v1/", Answer(http.StatusTeapot, "market data"))
	if resp, body := get(t, s, "/trader/v1/accounts/AB%2FC/orders", tok); resp.StatusCode != http.StatusOK ||
		body != "GET /trader/v1/accounts/{hash}/orders AB/C" {
		t.Errorf("orders: %d %s", resp.StatusCode, body)
	}
	if resp, body := get(t, s, "/marketdata/v1/quotes", tok); resp.StatusCode != http.StatusTeapot || body != "market data" {
		t.Errorf("market data: %d %s", resp.StatusCode, body)
	}
	if rs := s.Requests(); rs[len(rs)-1].Route != "" {
		t.Errorf("recorded the Route %q", rs[len(rs)-1].Route)
	}
	s.Script(Answer(http.StatusCreated, ""))
	if resp, _ := get(t, s, "/marketdata/v1/quotes", tok); resp.StatusCode != http.StatusCreated {
		t.Errorf("the script came after OverridePath: %d", resp.StatusCode)
	}
	s.OverridePath("/marketdata/v1/", Answer(http.StatusAccepted, ""))
	if resp, _ := get(t, s, "/marketdata/v1/quotes", tok); resp.StatusCode != http.StatusAccepted {
		t.Errorf("after a second OverridePath: %d", resp.StatusCode)
	}
	s.OverridePath("/marketdata/v1/", nil)
	s.OverridePath("/marketdata/v1/none", nil)
	if resp, _ := get(t, s, "/marketdata/v1/quotes", tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after OverridePath(nil): %d", resp.StatusCode)
	}
	if resp, _ := get(t, s, "/trader/v1/accounts/x/orders", tok); resp.StatusCode != http.StatusOK {
		t.Errorf("the other pattern after OverridePath(nil): %d", resp.StatusCode)
	}
	if errs := r.took(); len(errs) > 0 {
		t.Fatalf("reported %q", errs)
	}

	for _, tc := range []struct{ pattern, want string }{
		{"GET /trader/v1/accounts/{x}", "serves the requests it matches"},
		{"GET /trader/v1/accounts/abc", "serves the requests it matches"},
		{"/trader/v1/test", "serves the requests it matches"},
		{"GET /trader/v1/{rest...}", "matches requests that"},
		{"/trader/v1/", "matches requests that"},
		{"GET /trader/v1/{a}/accountNumbers", "matches requests that"},
		{"GET /trader/v1/accounts/{hash}/{what}", "matches requests that"},
		{"GET /trader/v1/{a}/abc", "conflicts"},
		{"GET /trader/v1/accounts/abc/{what}", "conflicts"},
		{"GET /v1/oauth/token", "not under"},
		{"/elsewhere/", "not under"},
		{"GET /trader/v1/{bad", "bad wildcard"},
		{"", "invalid pattern"},
	} {
		wantReported(t, r, tc.want, func() { s.OverridePath(tc.pattern, Answer(http.StatusTeapot, "")) })
	}
	if resp, _ := get(t, s, "/trader/v1/accounts/x/what", tok); resp.StatusCode != http.StatusNotFound {
		t.Errorf("a refused pattern answers: %d", resp.StatusCode)
	}
}

// OverridePath's table answers only the requests its patterns match. A
// path that a pattern ending in a slash would redirect to, and one that no
// pattern matches, go on down the list, to a recording or the fake's own
// 404, as they would with no pattern set.
func TestOverridePathMatchesOnlyItsPatterns(t *testing.T) {
	s := New(t)
	auth := "Bearer " + login(t, s)
	fsys := fstest.MapFS{"recorded/2026-11-08/001.json": {Data: []byte(`{"quotes":"recorded"}`)}}
	var e Entry
	e.File, e.Source, e.Recorded, e.Scrub = "recorded/2026-11-08/001.json", "recorded", "2026-11-08", "abc keep"
	e.Request.Method, e.Request.Path, e.Request.Query = http.MethodGet, "/marketdata/v1/quotes", "symbols=X"
	e.Response.Status = http.StatusOK
	if err := s.loadRecording(fsys, []Entry{e}, "2026-11-08"); err != nil {
		t.Fatal(err)
	}
	s.OverridePath("GET /trader/v1/accounts/{hash}/orders/", Answer(http.StatusTeapot, "orders/"))

	if rec := serve(t, s, "/trader/v1/accounts/x/orders/", auth); rec.Code != http.StatusTeapot || rec.Body.String() != "orders/" {
		t.Errorf("the pattern's own path: %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/trader/v1/accounts/x/orders", "/marketdata/v1/movers"} {
		rec := serve(t, s, path, auth)
		if rec.Code != http.StatusNotFound || rec.Body.String() != `{"message":"Not Found (the fake has no such route)"}` {
			t.Errorf("%s beside a pattern: %d %v %s, want the fake's own 404", path, rec.Code, rec.Header(), rec.Body)
			continue
		}
		wantFakeHeaders(t, rec.Header(), http.StatusNotFound)
	}
	if rec := serve(t, s, "/marketdata/v1/quotes?symbols=X", auth); rec.Code != http.StatusOK || rec.Body.String() != `{"quotes":"recorded"}` {
		t.Errorf("the recording beside a pattern: %d %s", rec.Code, rec.Body)
	}
}

// OnAPI sees each API request once routed, before anything else, and may
// change what answers that same request.
func TestOnAPI(t *testing.T) {
	s := New(t)
	tok := login(t, s)
	var patterns []string
	var mu sync.Mutex
	s.OnAPI(func(r *http.Request) {
		mu.Lock()
		patterns = append(patterns, r.Pattern)
		mu.Unlock()
		if r.URL.Path == "/trader/v1/test" && r.URL.Query().Has("refuse") {
			s.Override(TestRoute, InvalidAccount)
		}
	})
	get(t, s, "/trader/v1/test", tok)
	get(t, s, "/trader/v1/test", "")
	get(t, s, "/marketdata/v1/quotes", tok)
	if resp, _ := get(t, s, "/trader/v1/test?refuse", tok); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the Override set by OnAPI did not answer its own request: %d", resp.StatusCode)
	}
	postToken(t, s, refreshForm("x"))
	saw := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(patterns)
	}
	want := []string{string(TestRoute), string(TestRoute), "/marketdata/v1/", string(TestRoute)}
	if !slices.Equal(saw(), want) {
		t.Errorf("OnAPI saw %q, want %q", saw(), want)
	}
	s.OnAPI(nil)
	get(t, s, "/trader/v1/test", tok)
	if len(saw()) != len(want) {
		t.Error("OnAPI(nil) still calls the hook")
	}
}

// A handler runs without the fake's lock: it may call the Server, and one
// that hangs holds up no other request.
func TestHandlersRunWithoutTheLock(t *testing.T) {
	s := New(t)
	tok := login(t, s)
	s.Script(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%d", len(s.Requests()))
	})
	if _, body := get(t, s, "/trader/v1/test", tok); body != "1" {
		t.Errorf("a handler that calls the Server: %q", body)
	}

	hanging := make(chan struct{})
	s.Script(func(w http.ResponseWriter, r *http.Request) {
		close(hanging)
		Hang(w, r)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	hung := make(chan error, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL()+"/trader/v1/test", nil)
		if err == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
			var resp *http.Response
			if resp, err = s.Client().Do(req); err == nil {
				resp.Body.Close()
			}
		}
		hung <- err
	}()
	select {
	case <-hanging:
	case err := <-hung:
		t.Fatalf("the request to hang ended first: %v", err)
	}
	if resp, body := get(t, s, "/trader/v1/test", tok); resp.StatusCode != http.StatusOK || body != `{"ok":true}` {
		t.Errorf("while another request hangs: %d %s", resp.StatusCode, body)
	}
	cancel()
	if err := <-hung; !errors.Is(err, context.Canceled) {
		t.Errorf("the hanging request: %v, want it canceled", err)
	}
}

// NewTLS serves HTTP/2 over HTTPS to its own Client, at Addr.
func TestNewTLS(t *testing.T) {
	s := NewTLS(t)
	if !strings.HasPrefix(s.URL(), "https://") || !strings.HasSuffix(s.URL(), s.Addr()) {
		t.Errorf("URL %s, Addr %s", s.URL(), s.Addr())
	}
	tok := login(t, s)
	resp, body := get(t, s, "/trader/v1/test", tok)
	if resp.ProtoMajor != 2 || body != `{"ok":true}` {
		t.Errorf("%s: %s", resp.Proto, body)
	}
}

// Cut ends the connection without an answer, over HTTP/1 even after part
// of an answer, and over HTTP/2 by resetting the stream; the fake answers
// the next request as usual.
func TestCut(t *testing.T) {
	partial := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":`)
		w.(http.Flusher).Flush()
		Cut(w, nil)
	}
	for _, tc := range []struct {
		name string
		new  func(testing.TB) *Server
		cut  http.HandlerFunc
	}{
		{"HTTP/1", New, Cut},
		{"HTTP/1, after part of an answer", New, partial},
		{"HTTP/2", NewTLS, Cut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.new(t)
			tok := login(t, s)
			s.Script(tc.cut)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, s.URL()+"/trader/v1/test", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			// Go's transport sends a read again, once, when a connection it
			// reused is cut, so this one uses a connection of its own.
			tr := s.Client().Transport.(*http.Transport).Clone()
			tr.DisableKeepAlives = true
			resp, err := (&http.Client{Transport: tr}).Do(req)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("a cut answered %d", resp.StatusCode)
			}
			if resp, body := get(t, s, "/trader/v1/test", tok); resp.StatusCode != http.StatusOK || body != `{"ok":true}` {
				t.Errorf("after the cut: %d %s", resp.StatusCode, body)
			}
		})
	}
}

// A setter reports bad input with Errorf, which a handler on the fake's
// goroutines may call, and changes nothing.
func TestSetNow(t *testing.T) {
	r := &recorder{TB: t}
	s := New(r)
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	s.SetNow(func() time.Time { calls.Add(1); return at })
	wantReported(t, r, "SetNow(nil)", func() { s.SetNow(nil) })
	login(t, s)
	if calls.Load() == 0 {
		t.Error("the fake's clock was not called")
	}
}
