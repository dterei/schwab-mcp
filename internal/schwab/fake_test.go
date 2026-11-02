package schwab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

const (
	testKey      = "test-app-key"
	testSecret   = "test-app-secret"
	testCode     = "C0.abc123@"
	testCallback = "https://127.0.0.1"
)

// fakeOAuth imitates Schwab's token endpoint and an API endpoint that checks
// the access token. As Schwab does, a login retires the refresh tokens of
// earlier ones, while a refresh keeps the refresh token. It can also rotate
// refresh tokens and retire older access tokens, which Schwab is not known
// to do, to check that the coordination would cope.
type fakeOAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	refresh       map[string]bool // refresh tokens that still work
	access        map[string]bool // access tokens that still work
	rotate        bool            // a refresh issues a new refresh token and retires the old one
	revokeOlder   bool            // issuing an access token retires the earlier ones
	delay         time.Duration   // how long each grant takes, to widen races
	onGrant       func()          // called as each grant is made, before the answer
	requests      int             // token requests, granted or not
	issued        int
	codeGrants    int
	refreshGrants int

	// tokenFailures answers the next token requests with these statuses, in
	// order, where 0 cuts the connection instead.
	tokenFailures []int
	// script answers the next requests to the API, each entry once, in
	// order; after it, the API answers 200.
	script   []func(w http.ResponseWriter, r *http.Request)
	apiCalls int
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	f := &fakeOAuth{t: t, refresh: map[string]bool{}, access: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", f.handleToken)
	mux.HandleFunc("GET /whoami", f.handleWhoami)
	mux.HandleFunc("GET /trader/v1/test", f.handleAPI)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOAuth) handleToken(w http.ResponseWriter, r *http.Request) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if len(f.tokenFailures) > 0 {
		status := f.tokenFailures[0]
		f.tokenFailures = f.tokenFailures[1:]
		if status == 0 {
			cut(w)
		} else {
			http.Error(w, `{"error":"server_error"}`, status)
		}
		return
	}
	user, pass, ok := r.BasicAuth()
	if !ok || user != testKey || pass != testSecret {
		http.Error(w, `{"error":"invalid_client","error_description":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		http.Error(w, `{"error":"bad content type"}`, http.StatusBadRequest)
		return
	}
	var refresh string
	switch r.FormValue("grant_type") {
	case "authorization_code":
		if r.FormValue("code") != testCode || r.FormValue("redirect_uri") != testCallback {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.codeGrants++
		f.issued++
		refresh = fmt.Sprintf("refresh-%d", f.issued)
		clear(f.refresh)
		f.refresh[refresh] = true
	case "refresh_token":
		refresh = r.FormValue("refresh_token")
		if !f.refresh[refresh] {
			http.Error(w, `{"error":"unsupported_token_type","error_description":"400 Bad Request: {\"error_description\":\"Refresh token is invalid, expired or revoked\",\"error\":\"invalid_grant\"}"}`,
				http.StatusBadRequest)
			return
		}
		f.refreshGrants++
		f.issued++
		if f.rotate {
			delete(f.refresh, refresh)
			refresh = fmt.Sprintf("refresh-%d", f.issued)
			f.refresh[refresh] = true
		}
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
	}
	if f.onGrant != nil {
		f.onGrant()
	}
	access := fmt.Sprintf("access-%d", f.issued)
	if f.revokeOlder {
		clear(f.access)
	}
	f.access[access] = true
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"expires_in": 1800, "token_type": "Bearer", "scope": "api",
		"refresh_token": refresh, "access_token": access, "id_token": "id",
	}); err != nil {
		f.t.Errorf("fake Schwab: writing response: %v", err)
	}
}

// handleWhoami answers 200 for a working access token and 401 otherwise.
func (f *fakeOAuth) handleWhoami(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(prefix) || !f.access[auth[len(prefix):]] {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleAPI answers an API request with a working access token by the next
// entry of the script, or else 200.
func (f *fakeOAuth) handleAPI(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.apiCalls++
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) <= len(prefix) || !f.access[auth[len(prefix):]] {
		f.mu.Unlock()
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var answer func(w http.ResponseWriter, r *http.Request)
	if len(f.script) > 0 {
		answer = f.script[0]
		f.script = f.script[1:]
	}
	f.mu.Unlock()
	if answer == nil {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
		return
	}
	answer(w, r)
}

// answer returns a script entry that answers with status, headers in pairs,
// and body.
func answer(status int, body string, headers ...string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(headers); i += 2 {
			w.Header().Set(headers[i], headers[i+1])
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

// cut closes the connection without an answer, as a network failure would.
func cut(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if conn, _, err := hj.Hijack(); err == nil {
			conn.Close()
		}
	}
}

// calls returns how many API requests the fake has had.
func (f *fakeOAuth) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.apiCalls
}

// allow makes refresh a refresh token that the fake takes.
func (f *fakeOAuth) allow(refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh[refresh] = true
}

// tokenRequests returns how many token requests the fake has had.
func (f *fakeOAuth) tokenRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// grants returns how many code and refresh grants the fake has made.
func (f *fakeOAuth) grants() (code, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codeGrants, f.refreshGrants
}
