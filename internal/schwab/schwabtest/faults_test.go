package schwabtest

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Each canned answer sends its file with the status and headers of 1.7's
// list, the header names spelled as given.
func TestCannedAnswers(t *testing.T) {
	jsonError := func(status string) http.Header {
		return http.Header{"Content-Type": {"application/json"}, "Schwab-Client-CorrelID": {"corr-" + status}}
	}
	page := http.Header{"Content-Type": {"text/html; charset=utf-8"}}
	for _, tc := range []struct {
		name   string
		h      http.HandlerFunc
		file   string
		status int
		header http.Header
	}{
		{"TooMany", TooMany, "errors/rate_limit_429.txt", 429, http.Header{
			"Content-Type": {"application/json"}, "Schwab-Client-CorrelID": {"corr-429"}, "X-Request-Id": {"req-429"},
		}},
		{"InvalidAccount", InvalidAccount, "errors/invalid_account_400.json", 400, jsonError("400")},
		{"InvalidAccountList", InvalidAccountList, "errors/invalid_account_list_400.json", 400, jsonError("400")},
		{"NotAuthorized", NotAuthorized, "errors/not_authorized_401.json", 401, jsonError("401")},
		{"InvalidAPIKey", InvalidAPIKey, "errors/invalid_api_key_401.json", 401, jsonError("401")},
		{"AccessDenied", AccessDenied, "errors/akamai_403.html", 403, page},
		{"Whitelabel", Whitelabel, "errors/whitelabel_500.html", 500, page},
		{"TooBigBody", TooBigBody, "errors/too_big_body_502.json", 502, jsonError("502")},
		{"Unavailable", Unavailable, "errors/unavailable_503.json", 503, jsonError("503")},
		{"GatewayTimeout", GatewayTimeout, "errors/gateway_timeout_504.json", 504, jsonError("504")},
	} {
		rec := httptest.NewRecorder()
		tc.h(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/trader/v1/accounts", nil))
		if rec.Code != tc.status || rec.Body.String() != string(Fixture(t, tc.file)) || !maps.EqualFunc(rec.Header(), tc.header, equalValues) {
			t.Errorf("%s: %d %v %q, want %d %v and %s", tc.name, rec.Code, rec.Header(), rec.Body, tc.status, tc.header, tc.file)
		}
	}
	if body := string(Fixture(t, "errors/rate_limit_429.txt")); body != " \n" {
		t.Errorf("the rate limit's body is %q, want white space", body)
	}
	if body := string(Fixture(t, "errors/not_authorized_401.json")); strings.Count(body, cannedID) != 1 {
		t.Errorf("NotAuthorized's body does not hold its fixed ID once: %s", body)
	}
	if !strings.Contains(string(Fixture(t, "errors/invalid_api_key_401.json")), "oauth.v2.InvalidApiKey") {
		t.Error("InvalidAPIKey is not the gateway's fault")
	}
}

func equalValues(a, b []string) bool { return strings.Join(a, "\x00") == strings.Join(b, "\x00") }

// The refusals are the token endpoint's bodies as logged: they name the
// codes the client looks for, and only Schwab's own text says "refresh
// token".
func TestRefusals(t *testing.T) {
	for _, tc := range []struct{ name, body, file, code, nested string }{
		{"Refusal2026", Refusal2026, "errors/refusal_2026_400.json", "unsupported_token_type", `\"error\":\"invalid_grant\"`},
		{"Refusal2024", Refusal2024, "errors/refusal_2024_400.json", "unsupported_token_type", `refresh_token_authentication_error`},
		{"RefusalInvalidClient", RefusalInvalidClient, "errors/refusal_invalid_client_400.json", "invalid_client", "refresh token invalid"},
	} {
		if tc.body != string(Fixture(t, tc.file)) || !strings.HasPrefix(tc.body, `{"error":"`+tc.code+`"`) || !strings.Contains(tc.body, tc.nested) {
			t.Errorf("%s = %s", tc.name, tc.body)
		}
	}
	if !strings.Contains(Refusal2024, "tokenDigest=abc]") {
		t.Errorf("Refusal2024's digests are not written abc: %s", Refusal2024)
	}
}

// Answer sends what it is given and nothing more, and takes headers only
// in pairs.
func TestAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	Answer(http.StatusAccepted, "body", "Retry-After", "7", "X-Two", "a", "X-Two", "b")(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if want := (http.Header{"Retry-After": {"7"}, "X-Two": {"a", "b"}}); rec.Code != http.StatusAccepted || rec.Body.String() != "body" ||
		!maps.EqualFunc(rec.Header(), want, equalValues) {
		t.Errorf("%d %v %q", rec.Code, rec.Header(), rec.Body)
	}
	defer func() {
		if recover() == nil {
			t.Error("Answer took an odd number of header strings")
		}
	}()
	odd := strings.Split("Retry-After", ",")
	Answer(http.StatusOK, "", odd...)
}

// Hang answers when the client gives up, even on a request whose body the
// server would otherwise leave unread.
func TestHang(t *testing.T) {
	s := New(t)
	hanging := make(chan struct{})
	s.FailTokens(Failure{Handler: func(w http.ResponseWriter, r *http.Request) {
		close(hanging)
		Hang(w, r)
	}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL()+"/v1/oauth/token", strings.NewReader(codeForm(Code).Encode()))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		<-hanging
		cancel()
	}()
	if resp, err := s.Client().Do(req); !errors.Is(err, context.Canceled) {
		if err == nil {
			resp.Body.Close()
		}
		t.Fatalf("a hanging token request: %v", err)
	}
	done := make(chan struct{})
	go func() {
		s.srv.Close() // waits for the handler, which must see the client go
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Hang did not see the client hang up")
	}
}
