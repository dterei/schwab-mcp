package schwab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveAPIPrefixes are the paths of Schwab's APIs, which a live run may read.
var liveAPIPrefixes = []string{"/trader/v1/", "/marketdata/v1/"}

// tokenPath is the path of Schwab's token endpoint.
const tokenPath = "/v1/oauth/token"

// maxGrantBody bounds the body of a token request that the guard reads.
const maxGrantBody = 64 << 10

// failuresToStop is how many failures of the network in a row end a live
// run: a network that is down would spend the run's time on tries that
// each take up to a minute.
const failuresToStop = 3

// A guard is the transport of a live run's HTTP client, so that the run is
// read-only by construction. To its origin alone, by scheme https, host and
// port, it lets through a GET under the APIs' paths and a POST to the token
// endpoint that refreshes the access token, which it reads without
// consuming its body, and refuses anything else, a login among them. So no
// probe can reach an order or previewOrder, even through Client.Get, and no
// run can log in.
//
// It counts what it sends and what comes back. On a refusal, a spent budget
// or three failures of the network in a row it stops: it records why,
// refuses everything after, and cancels the run, so that the Client gives up
// at once rather than take the refusal for a failure of the network and try
// again.
type guard struct {
	origin *url.URL
	next   http.RoundTripper
	run    context.Context         // the run's, to tell its end from a failure of the network
	cancel context.CancelCauseFunc // ends the run
	now    func() time.Time        // tells the time each request is sent
	// answered, if set, is told of each answer as it arrives, with the time
	// its request was sent, before the client reads it. It may replace the
	// answer's body with one that reads the same.
	answered func(req *http.Request, sent time.Time, resp *http.Response)

	mu       sync.Mutex
	budget   int         // how many requests may go, token requests included
	sent     int         // how many went
	statuses map[int]int // the answers, by status
	tokens   int         // the token requests that went
	failures int         // the failures of the network
	inRow    int         // how many of them came in a row, since the last answer
	refusals []string    // what was refused
	stopped  error       // why the run was stopped, or nil
}

// newGuard returns a guard of the run of ctx, which cancel ends, for origin,
// an https address alone, such as https://api.schwabapi.com. It sends what
// it lets through with next, at most budget requests.
func newGuard(ctx context.Context, cancel context.CancelCauseFunc, origin string, next http.RoundTripper, budget int) (*guard, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		strings.TrimRight(u.Path, "/") != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("the live run's origin %q is not an https address alone", origin)
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &guard{origin: u, next: next, run: ctx, cancel: cancel, now: time.Now, budget: budget, statuses: map[int]int{}}, nil
}

// liveHTTPClient returns the HTTP client of a live run, which sends through
// g and follows no redirect. Go copies Authorization to a redirect on the
// same host name whatever its scheme, and a 307 or 308 sends a POST's body
// again, so that a redirect to http would send the bearer token, the app
// secret and the refresh token in plaintext. A 3xx comes back instead, as
// Schwab's answer: an *Error.
func liveHTTPClient(g *guard) *http.Client {
	return &http.Client{
		Timeout:       30 * time.Second,
		Transport:     g,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// RoundTrip sends req if the guard lets it through, and counts it and its
// answer.
func (g *guard) RoundTrip(req *http.Request) (*http.Response, error) {
	why, token := g.check(req)
	g.mu.Lock()
	switch {
	case g.stopped != nil:
		err := fmt.Errorf("the live run's guard sends nothing more, since %w", g.stopped)
		g.mu.Unlock()
		closeBody(req)
		return nil, err
	case why == "" && g.sent >= g.budget:
		why = fmt.Sprintf("a request beyond the budget of %d, token requests included", g.budget)
		g.stopLocked(fmt.Errorf("the budget of %d requests, token requests included, is spent", g.budget))
	case why != "":
		g.refusals = append(g.refusals, why)
		g.stopLocked(errors.New("the live run's guard refused " + why))
	}
	if why != "" {
		g.mu.Unlock()
		closeBody(req)
		return nil, errors.New("the live run's guard refused " + why)
	}
	g.sent++
	if token {
		g.tokens++
	}
	g.mu.Unlock()

	sent := g.now()
	resp, err := g.next.RoundTrip(req)
	g.mu.Lock()
	if err != nil {
		if g.run.Err() == nil { // not the run's own end
			g.failures++
			g.inRow++
			if g.inRow >= failuresToStop {
				g.stopLocked(errors.New("three failures of the network in a row"))
			}
		}
		g.mu.Unlock()
		return nil, err
	}
	g.inRow = 0
	g.statuses[resp.StatusCode]++
	g.mu.Unlock()
	if g.answered != nil {
		g.answered(req, sent, resp)
	}
	return resp, nil
}

// closeBody closes the body of a request that is not sent, as a
// RoundTripper must.
func closeBody(req *http.Request) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
}

// check returns why the guard refuses req, or "" if it lets it through,
// and whether req is a token request. Its words hold nothing of req's
// address or body.
func (g *guard) check(req *http.Request) (why string, token bool) {
	u := req.URL
	if u == nil || u.Scheme != "https" || u.User != nil || u.Opaque != "" || !g.isOrigin(u.Host) ||
		req.Host != "" && !g.isOrigin(req.Host) {
		return "a request to another origin", false
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet // as net/http takes it
	}
	p := u.EscapedPath()
	switch {
	case method == http.MethodGet && underAPI(p):
		return "", false
	case method == http.MethodGet:
		return "a GET outside " + strings.Join(liveAPIPrefixes, " and "), false
	case method == http.MethodPost && p == tokenPath:
		return checkGrant(req), true
	}
	return "a " + methodName(method) + " request", false
}

// isOrigin reports whether hostport, a URL's host or a Host header, names
// the guard's origin: its host, in any case, and its port, 443 when none
// is given.
func (g *guard) isOrigin(hostport string) bool {
	h := &url.URL{Host: hostport}
	return h.Hostname() != "" && strings.EqualFold(h.Hostname(), g.origin.Hostname()) && portOf(h) == portOf(g.origin)
}

// portOf returns u's port, or https's when it gives none.
func portOf(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	return "443"
}

// underAPI reports whether p, an escaped path, lies under one of the APIs'
// paths, by the Client's own rule for a path that leads nowhere else: no
// "." or ".." segment, even escaped or behind a backslash.
func underAPI(p string) bool {
	return slices.ContainsFunc(liveAPIPrefixes, func(prefix string) bool { return strings.HasPrefix(p, prefix) }) &&
		checkPath(p) == nil
}

// methodName returns a request's method as a refusal names it: as it is
// when it is a word of capitals, as methods are, and else "odd".
func methodName(m string) string {
	if m == "" || len(m) > 16 || strings.Trim(m, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
		return "odd"
	}
	return m
}

// checkGrant returns why the guard refuses req, a POST to the token
// endpoint, or "" for a refresh of the access token: a form of exactly a
// grant_type of refresh_token and one refresh token, with no query. The
// body is read through GetBody, so that what is sent is untouched.
func checkGrant(req *http.Request) string {
	if req.URL.RawQuery != "" {
		return "a token request with a query"
	}
	if mt, _, err := mime.ParseMediaType(req.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		return "a token request that is not a form"
	}
	if req.GetBody == nil {
		return "a token request whose body cannot be read without being used up"
	}
	body, err := req.GetBody()
	if err != nil {
		return "a token request whose body could not be read"
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, maxGrantBody+1))
	if err != nil || len(b) > maxGrantBody {
		return "a token request whose body could not be read"
	}
	form, err := url.ParseQuery(string(b))
	if err != nil {
		return "a token request whose form could not be read"
	}
	if form.Get("grant_type") != "refresh_token" {
		return "a token request that is not a refresh of the access token"
	}
	for k, v := range form {
		if k != "grant_type" && k != "refresh_token" || len(v) != 1 {
			return "a token request that sends more than a refresh token"
		}
	}
	if len(form["refresh_token"]) != 1 {
		return "a token request without a refresh token"
	}
	return ""
}

// stopLocked stops the run for cause, unless it has stopped already. It
// needs g.mu.
func (g *guard) stopLocked(cause error) {
	if g.stopped == nil {
		g.stopped = cause
		g.cancel(cause)
	}
}

// stop stops the run for cause, unless it has stopped already, so that
// nothing more is sent: for a reason of the run's own, such as its
// deadline.
func (g *guard) stop(cause error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopLocked(cause)
}

// setBudget sets how many requests may go in all, token requests included.
func (g *guard) setBudget(n int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.budget = n
}

// A guardCount is what a guard has counted, for the report.
type guardCount struct {
	budget, sent, tokens, failures int
	statuses                       map[int]int
	refusals                       []string
	stopped                        error
}

// count returns what g has counted so far.
func (g *guard) count() guardCount {
	g.mu.Lock()
	defer g.mu.Unlock()
	return guardCount{
		budget: g.budget, sent: g.sent, tokens: g.tokens, failures: g.failures,
		statuses: maps.Clone(g.statuses), refusals: slices.Clone(g.refusals), stopped: g.stopped,
	}
}

// stubTransport answers each request it is sent with answer's answer, or
// with 200 and {} without one, and keeps each request with its body.
type stubTransport struct {
	mu     sync.Mutex
	got    []stubbed
	answer func(req *http.Request) (*http.Response, error)
}

// stubbed is a request that a stubTransport was sent.
type stubbed struct{ method, url, body string }

func (s *stubTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
	}
	s.mu.Lock()
	s.got = append(s.got, stubbed{method: req.Method, url: req.URL.String(), body: string(body)})
	answer := s.answer
	s.mu.Unlock()
	if answer != nil {
		return answer(req)
	}
	return stubAnswer(req, http.StatusOK, "{}"), nil
}

// sent returns what s was sent.
func (s *stubTransport) sent() []stubbed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// stubAnswer returns an answer to req with status and body.
func stubAnswer(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), StatusCode: status,
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)), Request: req,
	}
}

// guardedClient returns an HTTP client of a live run that sends to s
// through a guard of Schwab's real origin, with budget, and a function that
// says why the guard stopped the run, if it did.
func guardedClient(t *testing.T, s *stubTransport, budget int) (*http.Client, *guard, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(t.Context())
	t.Cleanup(func() { cancel(nil) })
	g, err := newGuard(ctx, cancel, DefaultBaseURL, s, budget)
	if err != nil {
		t.Fatal(err)
	}
	return liveHTTPClient(g), g, func() error { return context.Cause(ctx) }
}

// A guardCase is a request that TestGuard sends, and whether the guard lets
// it through.
type guardCase struct {
	name, method, url string
	body, typ         string            // the body, and its Content-Type
	host              string            // the Host header, if not the URL's
	opaque            bool              // send the body so that GetBody is nil
	header            map[string]string // more headers
	pass              bool
}

// newGuardRequest returns the request of tc.
func newGuardRequest(t *testing.T, tc guardCase) *http.Request {
	t.Helper()
	var body io.Reader
	switch {
	case tc.opaque:
		body = io.MultiReader(strings.NewReader(tc.body)) // a reader that NewRequest cannot read twice
	case tc.body != "":
		body = strings.NewReader(tc.body)
	}
	req, err := http.NewRequestWithContext(t.Context(), tc.method, tc.url, body)
	if err != nil {
		t.Fatal(err)
	}
	if tc.typ != "" {
		req.Header.Set("Content-Type", tc.typ)
	}
	for k, v := range tc.header {
		req.Header.Set(k, v)
	}
	if tc.host != "" {
		req.Host = tc.host
	}
	return req
}

// The guard of Schwab's real origin lets through only a GET under the APIs'
// paths and a refresh of the access token, its body untouched, to https and
// the API's host, in any case, with no port or 443. It refuses http, another
// host, another port, a user in the address, a Host header naming another
// host, a path outside the two prefixes or leading out of them, every other
// method, an order among them, and every other token request: a login, a
// grant sent twice, a refresh with more in it, a query, a body that is not a
// form or that cannot be read twice. A refusal reaches nothing, names
// nothing of the request, and stops the run with why.
func TestGuard(t *testing.T) {
	const (
		api     = "https://api.schwabapi.com"
		form    = "application/x-www-form-urlencoded"
		refresh = "grant_type=refresh_token&refresh_token=zq9-refresh-_%3D%40"
		hash    = "ZQ9HASH0123456789ABCDEF"
	)
	for _, tc := range []guardCase{
		{name: "an account read", method: http.MethodGet, url: api + "/trader/v1/accounts/accountNumbers", pass: true},
		{name: "a market data read on port 443", method: http.MethodGet, url: "https://api.schwabapi.com:443/marketdata/v1/quotes?symbols=SPY", pass: true},
		{name: "the host in another case", method: http.MethodGet, url: "https://API.SchwabAPI.com/trader/v1/userPreference", pass: true},
		{name: "an escaped hash", method: http.MethodGet, url: api + "/trader/v1/accounts/" + hash + "%2Bx/transactions", pass: true},
		{name: "no method, which is GET", method: "", url: api + "/trader/v1/accounts", pass: true},
		{name: "a refresh", method: http.MethodPost, url: api + tokenPath, body: refresh, typ: form, pass: true},
		{name: "a refresh with a charset", method: http.MethodPost, url: api + tokenPath, body: refresh, typ: form + "; charset=utf-8", pass: true},

		{name: "http", method: http.MethodGet, url: "http://api.schwabapi.com/trader/v1/accounts"},
		{name: "a refresh over http", method: http.MethodPost, url: "http://api.schwabapi.com" + tokenPath, body: refresh, typ: form},
		{name: "another host", method: http.MethodGet, url: "https://api.schwabapi.com.zq9.example/trader/v1/accounts"},
		{name: "another port", method: http.MethodGet, url: "https://api.schwabapi.com:8443/trader/v1/accounts"},
		{name: "a user in the address", method: http.MethodGet, url: "https://zq9user@api.schwabapi.com/trader/v1/accounts"},
		{name: "a Host header naming another host", method: http.MethodGet, url: api + "/trader/v1/accounts", host: "zq9.example"},
		{name: "a Host header naming another port", method: http.MethodGet, url: api + "/trader/v1/accounts", host: "api.schwabapi.com:8443"},
		{name: "the authorize page", method: http.MethodGet, url: api + "/v1/oauth/authorize?client_id=zq9key&redirect_uri=x"},
		{name: "the token endpoint by GET", method: http.MethodGet, url: api + tokenPath + "?grant_type=refresh_token"},
		{name: "another version", method: http.MethodGet, url: api + "/trader/v2/accounts"},
		{name: "a prefix without its slash", method: http.MethodGet, url: api + "/trader/v1"},
		{name: "a path leading out", method: http.MethodGet, url: api + "/trader/v1/accounts/../../v1/oauth/authorize"},
		{name: "a path leading out, escaped", method: http.MethodGet, url: api + "/trader/v1/%2e%2e/%2E%2E/v1/oauth/authorize"},
		{name: "a path leading out behind a backslash", method: http.MethodGet, url: api + "/trader/v1/x%5C..%5C..%5Cv1/oauth/token"},
		{name: "an order", method: http.MethodPost, url: api + "/trader/v1/accounts/" + hash + "/orders", body: `{"orderType":"MARKET"}`, typ: "application/json"},
		{name: "a preview of an order", method: http.MethodPost, url: api + "/trader/v1/accounts/" + hash + "/previewOrder", body: "{}", typ: "application/json"},
		{name: "a replaced order", method: http.MethodPut, url: api + "/trader/v1/accounts/" + hash + "/orders/1", body: "{}", typ: "application/json"},
		{name: "a cancelled order", method: http.MethodDelete, url: api + "/trader/v1/accounts/" + hash + "/orders/1"},
		{name: "a patch", method: http.MethodPatch, url: api + "/trader/v1/accounts", body: "{}", typ: "application/json"},
		{name: "a head", method: http.MethodHead, url: api + "/trader/v1/accounts"},
		{name: "an options", method: http.MethodOptions, url: api + "/trader/v1/accounts"},
		{
			name: "a login", method: http.MethodPost, url: api + tokenPath, typ: form,
			body: "grant_type=authorization_code&code=C0.zq9code%40&redirect_uri=https%3A%2F%2F127.0.0.1",
		},
		{
			name: "a grant sent twice", method: http.MethodPost, url: api + tokenPath, typ: form,
			body: refresh + "&grant_type=authorization_code&code=C0.zq9code%40",
		},
		{name: "a refresh with a code", method: http.MethodPost, url: api + tokenPath, typ: form, body: refresh + "&code=C0.zq9code%40"},
		{name: "a refresh token sent twice", method: http.MethodPost, url: api + tokenPath, typ: form, body: refresh + "&refresh_token=zq9other"},
		{name: "a refresh without a token", method: http.MethodPost, url: api + tokenPath, typ: form, body: "grant_type=refresh_token"},
		{name: "a token request with a query", method: http.MethodPost, url: api + tokenPath + "?grant_type=authorization_code&code=zq9", body: refresh, typ: form},
		{name: "a token request of JSON", method: http.MethodPost, url: api + tokenPath, body: `{"grant_type":"refresh_token"}`, typ: "application/json"},
		{name: "a token request without a type", method: http.MethodPost, url: api + tokenPath, body: refresh},
		{name: "a token request that cannot be read twice", method: http.MethodPost, url: api + tokenPath, body: refresh, typ: form, opaque: true},
		{name: "a token request too large", method: http.MethodPost, url: api + tokenPath, typ: form, body: refresh + strings.Repeat("a", maxGrantBody)},
		{name: "a login with a refresh token", method: http.MethodPost, url: api + tokenPath, typ: form, body: "grant_type=authorization_code&refresh_token=zq9"},
		{name: "a grant of another kind", method: http.MethodPost, url: api + tokenPath, typ: form, body: "refresh_token=zq9&grant_type=client_credentials"},
		{name: "a refresh, then a login", method: http.MethodPost, url: api + tokenPath, typ: form, body: refresh + "&grant_type=authorization_code"},
		{name: "a token request of no form", method: http.MethodPost, url: api + tokenPath, typ: form, body: "grant_type=refresh_token&refresh_token=%zz"},
		{name: "a token request elsewhere", method: http.MethodPost, url: api + "/v1/oauth/token/", body: refresh, typ: form},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &stubTransport{}
			client, g, cause := guardedClient(t, s, 10)
			req := newGuardRequest(t, tc)
			resp, err := client.Do(req)
			if resp != nil {
				resp.Body.Close()
			}
			got := s.sent()
			if tc.pass {
				if err != nil || len(got) != 1 || cause() != nil {
					t.Fatalf("not let through: %v; sent %d; stopped by %v", err, len(got), cause())
				}
				if tc.body != "" && got[0].body != tc.body {
					t.Errorf("the body arrived as %q, want %q", got[0].body, tc.body)
				}
				if c := g.count(); c.sent != 1 || c.statuses[http.StatusOK] != 1 || (c.tokens == 1) != (tc.method == http.MethodPost) {
					t.Errorf("counted %+v", c)
				}
				return
			}
			if err == nil || len(got) != 0 {
				t.Fatalf("let through: %v; sent %q", err, got)
			}
			ue, ok := errors.AsType[*url.Error](err)
			if !ok || !strings.HasPrefix(ue.Err.Error(), "the live run's guard refused a") {
				t.Fatalf("the error: %v", err)
			}
			if cause() == nil || !strings.HasPrefix(cause().Error(), "the live run's guard refused a") {
				t.Errorf("the run was stopped by %v, want the refusal", cause())
			}
			for _, s := range []string{"zq9", hash, "schwabapi", "8443", "127.0.0.1", "orders", "oauth"} {
				if strings.Contains(ue.Err.Error(), s) || strings.Contains(cause().Error(), s) {
					t.Errorf("the refusal names %s: %v; %v", s, ue.Err, cause())
				}
			}
			if c := g.count(); c.sent != 0 || len(c.refusals) != 1 || c.stopped == nil {
				t.Errorf("counted %+v", c)
			}
		})
	}
}

// A refused request's body is closed, as a RoundTripper must, and the guard
// sends nothing once it has stopped the run, not even what it would
// otherwise let through.
func TestGuardSendsNothingOnceStopped(t *testing.T) {
	s := &stubTransport{}
	client, g, cause := guardedClient(t, s, 10)
	body := &closeCheck{Reader: strings.NewReader("{}")}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, DefaultBaseURL+"/trader/v1/accounts/x/orders", body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := g.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || !body.closed {
		t.Fatalf("an order: %v; its body closed: %v", err, body.closed)
	}
	for _, path := range []string{"/trader/v1/accounts", "/trader/v1/userPreference"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil || !strings.Contains(err.Error(), "the live run's guard sends nothing more, since the live run's guard refused a POST request") {
			t.Errorf("GET %s after the stop: %v", path, err)
		}
	}
	if got := s.sent(); len(got) != 0 || !errors.Is(g.count().stopped, cause()) {
		t.Errorf("sent %q; stopped by %v and %v", got, g.count().stopped, cause())
	}
}

// closeCheck is a body that says whether it was closed.
type closeCheck struct {
	io.Reader
	closed bool
}

func (c *closeCheck) Close() error {
	c.closed = true
	return nil
}

// A redirect comes back as the answer, unfollowed, even one to the same
// host over http, which would send the token in plaintext; a refresh's
// body is not sent again.
func TestGuardFollowsNoRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, body string
	}{
		{"a read", http.MethodGet, "/trader/v1/accounts", ""},
		{"a refresh", http.MethodPost, tokenPath, "grant_type=refresh_token&refresh_token=R"},
	} {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(fmt.Sprintf("%s, %d", tc.name, status), func(t *testing.T) {
				s := &stubTransport{answer: func(req *http.Request) (*http.Response, error) {
					resp := stubAnswer(req, status, "")
					resp.Header.Set("Location", "http://api.schwabapi.com"+tc.path)
					return resp, nil
				}}
				client, g, cause := guardedClient(t, s, 10)
				var body io.Reader
				if tc.body != "" {
					body = strings.NewReader(tc.body)
				}
				req, err := http.NewRequestWithContext(t.Context(), tc.method, DefaultBaseURL+tc.path, body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				if resp.StatusCode != status || len(s.sent()) != 1 || cause() != nil || g.count().statuses[status] != 1 {
					t.Errorf("got %d after sending %d; stopped by %v", resp.StatusCode, len(s.sent()), cause())
				}
			})
		}
	}
}

// The guard sends at most its budget of requests, token requests included,
// and then stops the run, saying so; a budget can be raised before it is
// spent.
func TestGuardBudget(t *testing.T) {
	s := &stubTransport{}
	client, g, cause := guardedClient(t, s, 1)
	do := func(method, path, body string) error {
		t.Helper()
		var r io.Reader
		if body != "" {
			r = strings.NewReader(body)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, DefaultBaseURL+path, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		return err
	}
	if err := do(http.MethodGet, "/trader/v1/accounts", ""); err != nil {
		t.Fatal(err)
	}
	g.setBudget(3)
	if err := do(http.MethodPost, tokenPath, "grant_type=refresh_token&refresh_token=R"); err != nil {
		t.Fatal(err)
	}
	if err := do(http.MethodGet, "/trader/v1/accounts", ""); err != nil {
		t.Fatal(err)
	}
	err := do(http.MethodGet, "/trader/v1/userPreference", "")
	if err == nil || !strings.Contains(err.Error(), "the live run's guard refused a request beyond the budget of 3, token requests included") {
		t.Errorf("the request beyond the budget: %v", err)
	}
	if got := fmt.Sprint(cause()); got != "the budget of 3 requests, token requests included, is spent" {
		t.Errorf("stopped by %q", got)
	}
	if c := g.count(); c.sent != 3 || c.tokens != 1 || c.statuses[http.StatusOK] != 3 || len(s.sent()) != 3 {
		t.Errorf("counted %+v, sent %d", c, len(s.sent()))
	}
}

// Three failures of the network in a row stop the run; an answer between
// them starts the count again, and a failure that comes from the run's own
// end is not one of the network's.
func TestGuardNetworkFailures(t *testing.T) {
	var fail []bool
	var mu sync.Mutex
	s := &stubTransport{answer: func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		f := fail[0]
		fail = fail[1:]
		if f {
			return nil, errors.New("connection reset")
		}
		return stubAnswer(req, http.StatusTooManyRequests, " "), nil
	}}
	client, g, cause := guardedClient(t, s, 100)
	do := func() error {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL+"/trader/v1/accounts", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if resp != nil {
			resp.Body.Close()
		}
		return err
	}
	fail = []bool{true, true, false, true, true}
	for range 5 {
		_ = do()
	}
	if cause() != nil {
		t.Fatalf("stopped by %v after two failures in a row, twice", cause())
	}
	fail = []bool{true}
	if err := do(); err == nil || fmt.Sprint(cause()) != "three failures of the network in a row" {
		t.Errorf("the third failure in a row: %v; stopped by %v", err, cause())
	}
	if c := g.count(); c.failures != 5 || c.sent != 6 || c.statuses[http.StatusTooManyRequests] != 1 {
		t.Errorf("counted %+v", c)
	}

	// The run's end is no failure of the network.
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(errors.New("the run's deadline passed"))
	g2, err := newGuard(ctx, func(error) {}, DefaultBaseURL, s, 100)
	if err != nil {
		t.Fatal(err)
	}
	fail = []bool{true, true, true}
	for range 3 {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL+"/trader/v1/accounts", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := g2.RoundTrip(req)
		if resp != nil {
			resp.Body.Close()
		}
		if err == nil {
			t.Fatal("no failure")
		}
	}
	if c := g2.count(); c.failures != 0 || c.stopped != nil {
		t.Errorf("after the run's end: %+v", c)
	}
}

// A stop for a reason of the run's own, such as its deadline, stops the
// guard as a refusal does: it cancels the run with that reason, which a
// later stop does not replace, and nothing more is sent.
func TestGuardStopsForTheRun(t *testing.T) {
	s := &stubTransport{}
	client, g, cause := guardedClient(t, s, 10)
	deadline := errors.New("the run's deadline passed")
	g.stop(deadline)
	if got := g.count().stopped; !errors.Is(got, deadline) || !errors.Is(cause(), deadline) {
		t.Fatalf("stopped by %v; the run's cause %v", got, cause())
	}
	g.stop(errors.New("later"))
	if got := g.count().stopped; !errors.Is(got, deadline) || !errors.Is(cause(), deadline) {
		t.Errorf("after a second stop: %v; the run's cause %v", got, cause())
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, DefaultBaseURL+"/trader/v1/accounts", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, deadline) || len(s.sent()) != 0 {
		t.Errorf("a read after the stop: %v; sent %d", err, len(s.sent()))
	}
}

// The origin of a guard is an https address alone, and the run's client
// sends through it, with a time limit of 30 seconds a request.
func TestNewGuardTakesAnHTTPSOrigin(t *testing.T) {
	for _, origin := range []string{
		"http://api.schwabapi.com", "https://", "https://u@api.schwabapi.com", "https://api.schwabapi.com/trader",
		"https://api.schwabapi.com?x=1", "https://api.schwabapi.com#x", "api.schwabapi.com", "https:api.schwabapi.com",
	} {
		if _, err := newGuard(t.Context(), func(error) {}, origin, nil, 1); err == nil {
			t.Errorf("newGuard(%q) took it", origin)
		}
	}
	for _, origin := range []string{DefaultBaseURL, DefaultBaseURL + "/", "https://127.0.0.1:41234"} {
		g, err := newGuard(t.Context(), func(error) {}, origin, nil, 1)
		if err != nil {
			t.Fatalf("newGuard(%q): %v", origin, err)
		}
		if c := liveHTTPClient(g); c.Timeout != 30*time.Second || c.Transport != g {
			t.Errorf("the run's client: %+v", c)
		}
	}
}
