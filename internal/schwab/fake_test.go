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
// the access token. It can rotate refresh tokens and revoke older access
// tokens, the two ways a refresh by one process can lock another out.
type fakeOAuth struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	refresh       map[string]bool // refresh tokens that still work
	access        map[string]bool // access tokens that still work
	rotate        bool            // a refresh issues a new refresh token and retires the old one
	revokeOlder   bool            // issuing an access token retires the earlier ones
	delay         time.Duration   // how long each grant takes, to widen races
	issued        int
	codeGrants    int
	refreshGrants int
}

func newFakeOAuth(t *testing.T) *fakeOAuth {
	f := &fakeOAuth{t: t, refresh: map[string]bool{}, access: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/oauth/token", f.handleToken)
	mux.HandleFunc("GET /whoami", f.handleWhoami)
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
	user, pass, ok := r.BasicAuth()
	if !ok || user != testKey || pass != testSecret {
		http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
		http.Error(w, `{"error":"bad content type"}`, http.StatusBadRequest)
		return
	}
	f.issued++
	var refresh string
	switch r.FormValue("grant_type") {
	case "authorization_code":
		if r.FormValue("code") != testCode || r.FormValue("redirect_uri") != testCallback {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		f.codeGrants++
		refresh = fmt.Sprintf("refresh-%d", f.issued)
		f.refresh[refresh] = true
	case "refresh_token":
		refresh = r.FormValue("refresh_token")
		if !f.refresh[refresh] {
			http.Error(w, `{"error":"unsupported_token_type","error_description":"refresh token invalid"}`, http.StatusBadRequest)
			return
		}
		f.refreshGrants++
		if f.rotate {
			delete(f.refresh, refresh)
			refresh = fmt.Sprintf("refresh-%d", f.issued)
			f.refresh[refresh] = true
		}
	default:
		http.Error(w, `{"error":"unsupported_grant_type"}`, http.StatusBadRequest)
		return
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

// allow makes refresh a refresh token that the fake takes.
func (f *fakeOAuth) allow(refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refresh[refresh] = true
}

// retire makes the fake refuse refresh, as Schwab does once a later login
// replaces it.
func (f *fakeOAuth) retire(refresh string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.refresh, refresh)
}

// grants returns how many code and refresh grants the fake has made.
func (f *fakeOAuth) grants() (code, refresh int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.codeGrants, f.refreshGrants
}
