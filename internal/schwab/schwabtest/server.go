package schwabtest

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Route is an endpoint of the API that the fake serves, as the pattern of
// a ServeMux: a method and a path. A request's Pattern is its Route.
type Route string

// The Routes: Schwab's account endpoints, and one of the fake's own for
// tests of the client that need a path that answers.
const (
	AccountNumbersRoute Route = "GET /trader/v1/accounts/accountNumbers"
	AccountsRoute       Route = "GET /trader/v1/accounts"
	AccountRoute        Route = "GET /trader/v1/accounts/{hash}"
	TransactionsRoute   Route = "GET /trader/v1/accounts/{hash}/transactions"
	TransactionRoute    Route = "GET /trader/v1/accounts/{hash}/transactions/{id}"
	UserPreferenceRoute Route = "GET /trader/v1/userPreference"
	// TestRoute answers {"ok":true}. It is not Schwab's.
	TestRoute Route = "GET /trader/v1/test"
)

// allRoutes are the Routes, in the order above.
var allRoutes = []Route{
	AccountNumbersRoute, AccountsRoute, AccountRoute, TransactionsRoute, TransactionRoute,
	UserPreferenceRoute, TestRoute,
}

// apiPrefixes are the paths of the API, which need an access token.
var apiPrefixes = []string{"/trader/v1/", "/marketdata/v1/"}

// A Request is an API request that the fake has had.
type Request struct {
	Method string
	Route  Route       // the Route that served it; "" for a path without one
	URL    string      // r.URL.String(): the escaped path and the raw query
	Path   string      // r.URL.EscapedPath()
	Query  string      // r.URL.RawQuery
	Token  string      // the access token it carried
	Header http.Header // its headers
}

// A Server is the fake: Schwab's token endpoint and authorize page, and
// the API behind them, which answers only a working access token. Each
// answer to an API request is chosen as the package doc says. A Server is
// safe for use by many goroutines, its own handlers' among them.
type Server struct {
	t   testing.TB
	srv *httptest.Server

	// mu guards everything below. The fake holds it to count, check the
	// token, record and choose an answer, and lets go before it calls a
	// handler, so that a handler may call the Server's methods.
	mu    sync.Mutex
	now   func() time.Time
	all   int // requests of any kind
	uuids int // the counter of the UUIDs the fake hands out

	// The token endpoint and the authorize page (token.go).
	tokenRequests int
	issued        int // the counter of access, refresh and id tokens
	codes         int // the counter of the authorize page's codes
	codeGrants    int
	refreshGrants int
	access        map[string]time.Time // access tokens that work, and when each lapses
	refresh       map[string]time.Time // refresh tokens that work, and when the login of each was
	pageCodes     map[string]time.Time // the authorize page's unused codes, and when each was issued
	secrets       []string
	rotate        bool
	revokeOlder   bool
	delay         time.Duration
	onToken       func()
	onGrant       func()
	failures      []Failure
	refusal       string
	expiresIn     int
	scope         string

	// The API.
	apiCalls  int
	requests  []Request
	onAPI     func(r *http.Request)
	script    []http.HandlerFunc
	queues    map[Route][]http.HandlerFunc
	overrides map[Route]http.HandlerFunc
	paths     map[string]http.HandlerFunc // OverridePath's table
	pathMux   *http.ServeMux              // the table's patterns, rebuilt on each change
	replays   map[string]*replayed        // LoadRecording's answers, by replayKey

	// The world (world.go, rows.go).
	accounts   []*account
	serials    int    // the counter of accounts' and rows' serials
	preference string // SetUserPreference's body
	rules      TransactionRules
}

// New starts a fake over HTTP, which the end of the test closes.
func New(t testing.TB) *Server {
	t.Helper()
	s := newServer(t)
	s.srv = httptest.NewServer(s.handler())
	t.Cleanup(s.srv.Close)
	return s
}

// NewTLS starts a fake over HTTPS, which speaks HTTP/2 to a client that
// does, such as Client's. The end of the test closes it.
func NewTLS(t testing.TB) *Server {
	t.Helper()
	s := newServer(t)
	s.srv = httptest.NewUnstartedServer(s.handler())
	s.srv.EnableHTTP2 = true
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

func newServer(t testing.TB) *Server {
	return &Server{
		t:         t,
		now:       time.Now,
		access:    map[string]time.Time{},
		refresh:   map[string]time.Time{},
		pageCodes: map[string]time.Time{},
		secrets:   []string{AppSecret, Code},
		refusal:   Refusal2026,
		expiresIn: 1800,
		scope:     "api",
		queues:    map[Route][]http.HandlerFunc{},
		overrides: map[Route]http.HandlerFunc{},
		paths:     map[string]http.HandlerFunc{},
		replays:   map[string]*replayed{},
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", s.token)
	mux.HandleFunc("GET /v1/oauth/authorize", s.authorize)
	for _, r := range allRoutes {
		mux.HandleFunc(string(r), s.api)
	}
	for _, p := range apiPrefixes {
		mux.HandleFunc(p, s.api)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.all++
		s.mu.Unlock()
		mux.ServeHTTP(w, r)
	})
}

// URL returns the fake's address, such as "http://127.0.0.1:41234": the
// base URL of Schwab's API.
func (s *Server) URL() string { return s.srv.URL }

// Client returns a client for the fake, which trusts the certificate of
// NewTLS's.
func (s *Server) Client() *http.Client { return s.srv.Client() }

// Addr returns the address the fake listens on, such as "127.0.0.1:41234".
func (s *Server) Addr() string { return s.srv.Listener.Addr().String() }

// SetNow sets the fake's clock, time.Now unless set, by which tokens and
// codes lapse. The fake calls now under its lock, so now must not call the
// Server.
func (s *Server) SetNow(now func() time.Time) {
	s.t.Helper()
	if now == nil {
		s.t.Errorf("schwabtest: SetNow(nil)")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = now
}

// uuid returns the next UUID of the fake's counter:
// 00000000-0000-4000-8000-000000000001, then ...02, and so on. It needs the
// lock.
func (s *Server) uuid() string {
	s.uuids++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", s.uuids)
}

// An answer is one the fake builds under its lock, to write after.
type answer struct {
	status int
	header http.Header // each name as it is to be sent
	body   []byte
}

// write sends a, ignoring a failure to write: a client may hang up.
func (a answer) write(w http.ResponseWriter) {
	maps.Copy(w.Header(), a.header)
	w.WriteHeader(a.status)
	_, _ = w.Write(a.body)
}

// built returns an answer of JSON to an API request, carrying a
// correlation ID and a request ID from the counter, the first spelled as
// Schwab's spec spells it on an answer of that status. It needs the lock.
func (s *Server) built(status int, body []byte) answer {
	correl := "Schwab-Client-CorrelId"
	if status >= http.StatusBadRequest {
		correl = "Schwab-Client-CorrelID"
	}
	h := http.Header{"Content-Type": {"application/json"}}
	h[correl] = []string{s.uuid()}
	h["X-Request-Id"] = []string{s.uuid()}
	return answer{status: status, header: h, body: body}
}

// api answers a request to the API, in the order the package doc gives.
func (s *Server) api(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	hook := s.onAPI
	s.mu.Unlock()
	if hook != nil {
		hook(r)
	}
	route := Route(r.Pattern)
	if !slices.Contains(allRoutes, route) {
		route = ""
	}

	s.mu.Lock()
	s.apiCalls++
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if lapse, issued := s.access[tok]; !ok || !issued || !s.now().Before(lapse) {
		a := s.unauthorized()
		s.mu.Unlock()
		a.write(w)
		return
	}
	s.requests = append(s.requests, Request{
		Method: r.Method, Route: route, URL: r.URL.String(), Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Token: tok, Header: r.Header.Clone(),
	})
	h := s.handlerFor(route, r)
	if h == nil {
		h = s.replayFor(r)
	}
	var a answer
	if h == nil {
		a = s.own(route, r)
	}
	s.mu.Unlock()
	if h != nil {
		h(w, r)
		return
	}
	a.write(w)
}

// handlerFor returns the handler that answers r instead of the fake: the
// next Script entry, the route's next Queue entry, the route's Override,
// or OverridePath's match; or nil for none. It needs the lock.
func (s *Server) handlerFor(route Route, r *http.Request) http.HandlerFunc {
	if len(s.script) > 0 {
		h := s.script[0]
		s.script = s.script[1:]
		if h != nil {
			return h
		}
	}
	if q := s.queues[route]; route != "" && len(q) > 0 {
		s.queues[route] = q[1:]
		if q[0] != nil {
			return q[0]
		}
	}
	if route != "" {
		return s.overrides[route]
	}
	if m := s.pathMux; m != nil {
		if h, _ := m.Handler(r); isPathHandler(h) {
			return m.ServeHTTP
		}
	}
	return nil
}

// A pathHandler is a handler of OverridePath's table, as its ServeMux holds
// it. The ServeMux's own handlers, which redirect a request or refuse it,
// are not, so a request that only one of them would answer is no match.
type pathHandler struct{ http.HandlerFunc }

// isPathHandler reports whether h is one of OverridePath's handlers.
func isPathHandler(h http.Handler) bool {
	_, ok := h.(pathHandler)
	return ok
}

// own returns the fake's own answer to r, a request with a working token
// that no handler or recording took: the world's, built now from what it
// holds. It needs the lock.
func (s *Server) own(route Route, r *http.Request) answer {
	q := r.URL.Query()
	switch route {
	case AccountNumbersRoute:
		return s.accountNumbers()
	case AccountsRoute:
		return s.accountList(wantsPositions(q))
	case AccountRoute:
		return s.oneAccount(r.PathValue("hash"), wantsPositions(q))
	case TransactionsRoute:
		return s.transactions(r.PathValue("hash"), q)
	case TransactionRoute:
		return s.transaction(r.PathValue("hash"), r.PathValue("id"))
	case UserPreferenceRoute:
		return s.userPreference()
	case TestRoute:
		return s.built(http.StatusOK, []byte(`{"ok":true}`))
	default:
		return s.built(http.StatusNotFound, []byte(`{"message":"Not Found (the fake has no such route)"}`))
	}
}

// unauthorized returns the answer to an API request without a working
// access token: Schwab's 401 to a token it does not take, with an ID of
// its own. Of Schwab's answer to a request without any token, only the
// status was kept. It needs the lock.
func (s *Server) unauthorized() answer {
	a := s.built(http.StatusUnauthorized, nil)
	a.body = []byte(strings.Replace(notAuthorizedBody, cannedID, s.uuid(), 1))
	return a
}

// Requests returns the API requests that carried a working access token,
// in the order they came.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := slices.Clone(s.requests)
	for i := range rs {
		rs[i].Header = rs[i].Header.Clone()
	}
	return rs
}

// APICalls returns how many requests the API has had, with a working
// token or not.
func (s *Server) APICalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiCalls
}

// AllRequests returns how many requests the fake has had, of any kind and
// to any path, one that it had no answer for included.
func (s *Server) AllRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all
}

// Script answers the next API requests with hs, one each, in order,
// whatever their path. Only a request with a working token takes an
// entry, and a nil entry is no answer: the request is answered as if the
// script were empty.
func (s *Server) Script(hs ...http.HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script = append(s.script, hs...)
}

// Queue answers the next requests to route with hs, one each, in order,
// after the Script. A nil entry is no answer: that request goes to the
// route's Override, if one is set, or to the fake.
func (s *Server) Queue(route Route, hs ...http.HandlerFunc) {
	s.t.Helper()
	if !slices.Contains(allRoutes, route) {
		s.t.Errorf("schwabtest: Queue(%q): no such Route", route)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queues[route] = append(s.queues[route], hs...)
}

// Override answers every request to route with h, after the Script and
// the route's Queue, until it is set to nil.
func (s *Server) Override(route Route, h http.HandlerFunc) {
	s.t.Helper()
	if !slices.Contains(allRoutes, route) {
		s.t.Errorf("schwabtest: Override(%q): no such Route", route)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h == nil {
		delete(s.overrides, route)
		return
	}
	s.overrides[route] = h
}

// OverridePath answers the API requests that pattern, a ServeMux pattern,
// matches, after the Script, until it is set to nil: for paths that no
// Route serves, such as "GET /trader/v1/accounts/{hash}/orders". The
// handler sees the request's Pattern and PathValue as pattern gives them.
// Setting a pattern again replaces its handler. A pattern outside the API,
// one that matches a request some Route serves (Override is for those), or
// one that conflicts with another pattern set fails the test, and the
// patterns stay as they were.
func (s *Server) OverridePath(pattern string, h http.HandlerFunc) {
	s.t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, set := s.paths[pattern]
	switch {
	case h == nil && !set:
		return
	case h == nil:
		delete(s.paths, pattern)
	case set:
		s.paths[pattern] = h
	default:
		if err := checkPattern(pattern, slices.Collect(maps.Keys(s.paths))); err != nil {
			s.t.Errorf("schwabtest: OverridePath(%q): %v", pattern, err)
			return
		}
		s.paths[pattern] = h
	}
	s.pathMux = nil
	if len(s.paths) > 0 {
		s.pathMux = http.NewServeMux()
		for p, h := range s.paths {
			s.pathMux.Handle(p, pathHandler{h})
		}
	}
}

// checkPattern returns why pattern cannot join others in OverridePath's
// table, or nil if it can.
func checkPattern(pattern string, others []string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	alone := http.NewServeMux()
	alone.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {}) // panics on a pattern that is not one
	sample, err := sampleRequest(pattern)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(apiPrefixes, func(p string) bool { return strings.HasPrefix(sample.URL.Path, p) }) {
		return fmt.Errorf("it is not under %s, so the fake would never ask it", strings.Join(apiPrefixes, " or "))
	}
	if route, ok := routeOf(sample.Method, sample.URL.EscapedPath()); ok {
		return fmt.Errorf("%s serves the requests it matches; use Override", route)
	}
	for _, r := range allRoutes {
		rs, err := sampleRequest(string(r))
		if err != nil {
			return err
		}
		if _, p := alone.Handler(rs); p == pattern {
			return fmt.Errorf("it matches requests that %s serves; use Override", r)
		}
	}
	// A pattern that overlaps a Route or another one in the table without
	// either being the more specific panics.
	all := http.NewServeMux()
	for _, p := range append(slices.Concat(others, routeStrings()), pattern) {
		all.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return nil
}

// routeStrings returns the Routes as patterns.
func routeStrings() []string {
	ps := make([]string, len(allRoutes))
	for i, r := range allRoutes {
		ps[i] = string(r)
	}
	return ps
}

// sampleRequest returns a request that pattern matches: each wildcard
// filled with x, one that takes the rest of the path with several
// segments, and the method GET when pattern names none.
func sampleRequest(pattern string) (*http.Request, error) {
	method, rest, ok := strings.Cut(pattern, " ")
	if !ok {
		method, rest = http.MethodGet, pattern
	}
	host, p, _ := strings.Cut(strings.TrimLeft(rest, " \t"), "/")
	p = "/" + p
	var segs []string
	for seg := range strings.SplitSeq(p, "/") {
		switch {
		case seg == "{$}":
			seg = ""
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}"):
			seg = "x/x/x/x/x/x/x/x"
		case strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}"):
			seg = "x"
		}
		segs = append(segs, seg)
	}
	u := &url.URL{Path: strings.Join(segs, "/")}
	if host == "" {
		host = "schwabtest"
	}
	if method == "" {
		return nil, fmt.Errorf("no method or path in %q", pattern)
	}
	return &http.Request{Method: method, URL: u, Host: host}, nil
}

// OnAPI calls f as each API request arrives, once the request is routed,
// so that its Pattern is its Route, or /trader/v1/ or /marketdata/v1/ for
// a path without one, and before anything else. f runs without the fake's
// lock, so it may call the Server, and it may run on several requests at
// once. A nil f calls nothing.
func (s *Server) OnAPI(f func(r *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onAPI = f
}
