package schwabtest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The app and the login that the fake takes.
const (
	AppKey      = "test-app-key"
	AppSecret   = "test-app-secret"
	CallbackURL = "https://127.0.0.1"
	// Code is a code the token endpoint always takes, in its decoded form
	// (a redirect sends the @ as %40). Unlike Schwab's, it works any number
	// of times and never lapses; the authorize page's codes follow Schwab's
	// rule.
	Code = "C0.abc123@"
)

// How long the fake's codes and logins last, as Schwab's do: a code from
// the authorize page works once within 30 seconds, and a login's refresh
// tokens for 7 days, however often they are used.
const (
	codeLife    = 30 * time.Second
	refreshLife = 7 * 24 * time.Hour
)

// AccessToken returns the nth access token the fake issues. Its tokens
// hold ".", "-", "_", "=" and "@", as Schwab's do, so that a client that
// fails to escape one in a form or a header shows it.
func AccessToken(n int) string { return fmt.Sprintf("I0.access-%d-_=@", n) }

// RefreshToken returns the nth refresh token the fake issues. Access,
// refresh and id tokens share one count, so a login is AccessToken(1) and
// RefreshToken(1), and the refresh after it AccessToken(2).
func RefreshToken(n int) string { return fmt.Sprintf("refresh-%d-_=@", n) }

// IDToken returns the nth id token the fake issues: shaped as a JWT, as
// Schwab's are, though not signed.
func IDToken(n int) string {
	enc := base64.RawURLEncoding
	return enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`)) + "." +
		enc.EncodeToString([]byte(`{"sub":"`+strconv.Itoa(n)+`"}`)) + "." +
		enc.EncodeToString([]byte("schwabtest-"+strconv.Itoa(n)))
}

// AuthCode returns the nth code the authorize page issues. Its codes have
// a count of their own, so that a login through the page still gets
// AccessToken(1).
func AuthCode(n int) string { return fmt.Sprintf("C0.code-%d-_=@", n) }

// A Failure is an answer of the token endpoint that FailTokens queues.
type Failure struct {
	Status     int    // 0 cuts the connection
	RetryAfter string // its Retry-After, if set
	// Body is its body; "" means {"error":"server_error"}, unless
	// EmptyBody is set.
	Body string
	// EmptyBody sends no body at all, as for a 401 with an empty body.
	EmptyBody bool
	// Header holds more headers, such as a correlation ID, each name as it
	// is to be sent.
	Header http.Header
	// Handler, if set, answers in place of all the above, with the fake's
	// lock released, so that it may call the Server: Hang, say, hangs one
	// token request.
	Handler http.HandlerFunc
}

// token answers POST /v1/oauth/token, in the order the package doc gives.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	onToken, delay := s.onToken, s.delay
	s.mu.Unlock()
	if onToken != nil {
		onToken()
	}
	if delay > 0 {
		time.Sleep(delay)
	}

	// The unlock is deferred so that a cut over HTTP/2, which panics,
	// releases the lock too; a Handler is called after an early one.
	s.mu.Lock()
	unlock := sync.OnceFunc(s.mu.Unlock)
	defer unlock()
	s.tokenRequests++
	if len(s.failures) > 0 {
		f := s.failures[0]
		s.failures = s.failures[1:]
		switch {
		case f.Handler != nil:
			unlock()
			f.Handler(w, r)
		case f.Status == 0:
			Cut(w, r)
		default:
			f.write(w)
		}
		return
	}

	if user, pass, ok := r.BasicAuth(); !ok || user != AppKey || pass != AppSecret {
		tokenJSON(w, http.StatusUnauthorized, `{"error":"invalid_client","error_description":"Unauthorized"}`)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		tokenJSON(w, http.StatusBadRequest,
			`{"error":"invalid_request","error_description":"a guess: Schwab's answer to this content type is not recorded"}`)
		return
	}
	now := s.now()
	var refresh string
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		code := r.PostFormValue("code")
		issued, fromPage := s.pageCodes[code]
		works := code == Code || fromPage && now.Sub(issued) < codeLife
		if !works || r.PostFormValue("redirect_uri") != CallbackURL {
			tokenJSON(w, http.StatusBadRequest,
				`{"error":"invalid_grant","error_description":"a guess: Schwab's answer to a bad code is not recorded"}`)
			return
		}
		delete(s.pageCodes, code)
		s.codeGrants++
		s.issued++
		refresh = RefreshToken(s.issued)
		clear(s.refresh) // one login at a time
		s.refresh[refresh] = now
		s.addSecrets(refresh)
	case "refresh_token":
		refresh = r.PostFormValue("refresh_token")
		login, ok := s.refresh[refresh]
		if !ok || !now.Before(login.Add(refreshLife)) {
			tokenJSON(w, http.StatusBadRequest, s.refusal)
			return
		}
		s.refreshGrants++
		s.issued++
		if s.rotate {
			delete(s.refresh, refresh)
			refresh = RefreshToken(s.issued)
			s.refresh[refresh] = login
			s.addSecrets(refresh)
		}
	default:
		tokenJSON(w, http.StatusBadRequest,
			`{"error":"unsupported_grant_type","error_description":"a guess: Schwab's answer is not recorded"}`)
		return
	}
	if s.onGrant != nil {
		s.onGrant()
	}
	access, id := AccessToken(s.issued), IDToken(s.issued)
	if s.revokeOlder {
		clear(s.access)
	}
	lapse := s.expiresIn
	if lapse <= 0 {
		lapse = 1800 // as the client takes it
	}
	s.access[access] = now.Add(time.Duration(lapse) * time.Second)
	s.addSecrets(access, id)

	// Schwab's documented order, each value escaped as JSON: the refresh
	// token that a refresh does not rotate is the request's own.
	var b bytes.Buffer
	b.WriteString(`{"expires_in":` + strconv.Itoa(s.expiresIn) + `,"token_type":"Bearer","scope":`)
	b.Write(jsonString(s.scope))
	b.WriteString(`,"refresh_token":`)
	b.Write(jsonString(refresh))
	b.WriteString(`,"access_token":`)
	b.Write(jsonString(access))
	b.WriteString(`,"id_token":`)
	b.Write(jsonString(id))
	b.WriteString(`}`)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
}

// jsonString returns s as a JSON string.
func jsonString(s string) []byte {
	b, err := json.Marshal(s)
	if err != nil {
		panic("schwabtest: encoding a string: " + err.Error())
	}
	return b
}

// tokenJSON writes an answer of the token endpoint: JSON without
// correlation headers, since whether Schwab sends them there is not known.
func tokenJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// write sends f, which has neither a Handler nor Status 0.
func (f Failure) write(w http.ResponseWriter) {
	h := w.Header()
	for k, v := range f.Header {
		h[k] = slices.Clone(v)
	}
	if f.RetryAfter != "" {
		h.Set("Retry-After", f.RetryAfter)
	}
	typed := false
	for k := range f.Header {
		typed = typed || strings.EqualFold(k, "Content-Type")
	}
	if !typed && !f.EmptyBody {
		h.Set("Content-Type", "application/json")
	}
	w.WriteHeader(f.Status)
	if f.EmptyBody {
		return
	}
	body := f.Body
	if body == "" {
		body = `{"error":"server_error"}`
	}
	_, _ = io.WriteString(w, body)
}

// addSecrets adds to what Secrets returns. It needs the lock.
func (s *Server) addSecrets(ss ...string) {
	for _, x := range ss {
		if !slices.Contains(s.secrets, x) {
			s.secrets = append(s.secrets, x)
		}
	}
}

// The authorize page's errors, which are the fake's guesses, labeled as
// such in their own text.
const (
	unknownClientPage = "<html><body><h1>Unknown client (the fake's guess)</h1></body></html>"
	invalidURIPage    = "<html><body><h1>Error - invalid URI specified (the status is the fake's guess)</h1></body></html>"
	responseTypePage  = "<html><body><h1>Error - unsupported response type (the fake's guess)</h1></body></html>"
)

// authorize answers GET /v1/oauth/authorize, the page where the browser
// login starts, as if the user had signed in and approved: it sends the
// browser to the callback with a new code.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status, page := 0, ""
	switch {
	case q.Get("client_id") != AppKey:
		status, page = http.StatusUnauthorized, unknownClientPage
	case q.Get("redirect_uri") != CallbackURL:
		status, page = http.StatusBadRequest, invalidURIPage
	case q.Get("response_type") != "code":
		status, page = http.StatusBadRequest, responseTypePage
	}
	if page != "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, page)
		return
	}
	s.mu.Lock()
	s.codes++
	code := AuthCode(s.codes)
	s.pageCodes[code] = s.now()
	s.addSecrets(code)
	// Encode escapes every value and sorts the keys, which is Schwab's
	// order: code, session, state.
	v := url.Values{"code": {code}, "session": {s.uuid()}}
	s.mu.Unlock()
	if q.Has("state") {
		v.Set("state", q.Get("state"))
	}
	w.Header().Set("Location", CallbackURL+"/?"+v.Encode())
	w.WriteHeader(http.StatusFound)
}

// FailTokens answers the next token requests with fs, one each, in order,
// before anything else is checked.
func (s *Server) FailTokens(fs ...Failure) {
	s.t.Helper()
	for _, f := range fs {
		if f.Handler == nil && f.Status != 0 && (f.Status < 100 || f.Status > 999) {
			s.t.Errorf("schwabtest: FailTokens: status %d", f.Status)
			return
		}
		if f.EmptyBody && f.Body != "" {
			s.t.Errorf("schwabtest: FailTokens: a Failure with a Body and EmptyBody")
			return
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, fs...)
}

// SetTokenDelay makes each token request take d before it is answered,
// without the fake's lock, so that requests overlap and can outlast a
// client's time limit.
func (s *Server) SetTokenDelay(d time.Duration) {
	s.t.Helper()
	if d < 0 {
		s.t.Errorf("schwabtest: SetTokenDelay(%v)", d)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delay = d
}

// OnToken calls f as each token request arrives, before anything else.
// It runs without the fake's lock, so it may call the Server. A nil f
// calls nothing.
func (s *Server) OnToken(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onToken = f
}

// OnGrant calls f as each grant is made, before the answer is written. It
// runs under the fake's lock, so that the grant and what f does are one
// step for the requests around it; f must not call the Server. A nil f
// calls nothing.
func (s *Server) OnGrant(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onGrant = f
}

// SetRotate makes each refresh issue a new refresh token and end the one
// it was given, which Schwab has not been seen to do.
func (s *Server) SetRotate(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rotate = on
}

// SetRevokeOlder makes each grant end every access token issued before
// it. Whether Schwab does is not known.
func (s *Server) SetRevokeOlder(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokeOlder = on
}

// SetRefusal sets the body with which the token endpoint refuses a refresh
// token that does not work; "" sets Refusal2026 again.
func (s *Server) SetRefusal(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if body == "" {
		body = Refusal2026
	}
	s.refusal = body
}

// SetExpiresIn sets the expires_in that grants advertise, and when the
// access tokens they issue lapse, seconds later on the fake's clock. For a
// value of 0 or less the tokens lapse after 1800 seconds, as the client
// takes such a value, while the value itself is still sent.
func (s *Server) SetExpiresIn(seconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expiresIn = seconds
}

// SetScope sets the scope that grants advertise, "api" unless set.
func (s *Server) SetScope(scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scope = scope
}

// AllowRefresh makes each of tokens a refresh token that the fake takes,
// as if it came from a login now, for a token file written without a
// login.
func (s *Server) AllowRefresh(tokens ...string) {
	s.t.Helper()
	if slices.Contains(tokens, "") {
		s.t.Errorf("schwabtest: AllowRefresh of an empty token")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for _, tok := range tokens {
		s.refresh[tok] = now
	}
	s.addSecrets(tokens...)
}

// EndLogin ends every refresh token, as when a login's 7 days are up. The
// access tokens work on until they lapse, unless RevokeAccess ends them.
func (s *Server) EndLogin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.refresh)
}

// RevokeAccess ends every access token issued so far, and leaves the
// refresh tokens working.
func (s *Server) RevokeAccess() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.access)
}

// TokenRequests returns how many token requests the fake has had, granted
// or not.
func (s *Server) TokenRequests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tokenRequests
}

// Grants returns how many code and refresh grants the fake has made.
func (s *Server) Grants() (code, refresh int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.codeGrants, s.refreshGrants
}

// Secrets returns what must never appear in a program's output: AppSecret,
// Code, every access, refresh and id token the fake issued, every code its
// authorize page issued, and every refresh token AllowRefresh took. Not
// AppKey, which the login link carries by design.
func (s *Server) Secrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.secrets)
}
